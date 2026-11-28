// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package entadapter

import (
	"context"
	stdsql "database/sql"
	"fmt"
	"strings"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/commandrequest"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/commandrun"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/execrunoutput"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

var _ store.CommandRequestStore = (*CompositeStore)(nil)

// CreateCommandRequestPrompt implements store.CommandRequestStore.
func (c *CompositeStore) CreateCommandRequestPrompt(ctx context.Context, in *store.CommandRequestPromptInput) (*store.CommandRequestPromptResult, error) {
	if err := validateCommandRequestPromptInput(in); err != nil {
		return nil, err
	}
	return insertNextActionVersion(ctx, c, func(tx *CompositeStore) (*store.CommandRequestPromptResult, error) {
		return tx.createCommandRequestPromptTx(ctx, in)
	})
}

func validateCommandRequestPromptInput(in *store.CommandRequestPromptInput) error {
	if in == nil || in.Message == nil || in.Message.ID == "" || in.Message.ProjectID == "" {
		return fmt.Errorf("%w: a command request prompt needs a message with an ID and a project", store.ErrInvalidInput)
	}
	for _, id := range []string{in.Message.ID, in.Message.ProjectID, in.AgentID} {
		if _, err := parseUUID(id); err != nil {
			return err
		}
	}
	if strings.TrimSpace(in.ApproverID) == "" {
		return fmt.Errorf("%w: approverID is required", store.ErrInvalidInput)
	}
	if in.Request == nil {
		return fmt.Errorf("%w: request is required", store.ErrInvalidInput)
	}
	if !messages.ValidCommandRequestKey(in.Request.Key) {
		return fmt.Errorf("%w: invalid command request key", store.ErrInvalidInput)
	}
	if err := messages.ValidateCommandRequestScript(in.Request.Script); err != nil {
		return fmt.Errorf("%w: %v", store.ErrInvalidInput, err)
	}
	if err := messages.ValidateCommandRequestWorkdir(in.Request.Workdir); err != nil {
		return fmt.Errorf("%w: %v", store.ErrInvalidInput, err)
	}
	if in.Request.TimeoutS < messages.MinCommandRequestTimeoutS || in.Request.TimeoutS > messages.MaxCommandRequestTimeoutS {
		return fmt.Errorf("%w: timeout_s out of range", store.ErrInvalidInput)
	}
	if in.Request.CommandSHA256 == "" {
		return fmt.Errorf("%w: command_sha256 is required", store.ErrInvalidInput)
	}
	return nil
}

func (c *CompositeStore) createCommandRequestPromptTx(ctx context.Context, in *store.CommandRequestPromptInput) (*store.CommandRequestPromptResult, error) {
	client := c.client
	agentID, _ := parseUUID(in.AgentID)
	projectID, _ := parseUUID(in.Message.ProjectID)
	msgID, _ := parseUUID(in.Message.ID)

	now := in.Now.UTC().Truncate(time.Microsecond)
	if now.IsZero() {
		now = time.Now().UTC().Truncate(time.Microsecond)
	}
	ttl := in.TTL
	if ttl <= 0 {
		ttl = msgaction.CommandRequestTTL
	}
	expiresAt := now.Add(ttl)

	norm := messages.CommandRequestKeyNorm(in.Request.Key)

	q := client.CommandRequest.Query().
		Where(
			commandrequest.AgentIDEQ(agentID),
			commandrequest.KeyNormEQ(norm),
		).
		Order(ent.Asc(commandrequest.FieldVersion))
	if c.isPostgres() {
		q = q.ForUpdate()
	}
	existingRows, err := q.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	var headEnt *ent.CommandRequest
	for _, row := range existingRows {
		if headEnt == nil || row.Version >= headEnt.Version {
			headEnt = row
		}
	}

	var head *msgaction.Head
	if headEnt != nil {
		h := &msgaction.Head{
			RowID:   headEnt.ID.String(),
			Key:     headEnt.Key,
			Version: headEnt.Version,
			Status:  string(headEnt.Status),
		}
		// Treat an expired-but-unswept pending row as expired for repost planning.
		if headEnt.Status == commandrequest.StatusPending && !headEnt.ExpiresAt.After(now) {
			h.Status = store.CommandRequestStatusExpired
		}
		if headEnt.ReplacedByID != nil {
			h.ReplacedBy = headEnt.ReplacedByID.String()
		}
		if headEnt.ResolvedAt != nil {
			t := headEnt.ResolvedAt.UTC()
			h.ResolvedAt = &t
		}
		head = h
	}

	plan, planErr := msgaction.PlanRepost(msgaction.CommandRequestKindSpec, head, in.Update)
	if planErr != nil {
		if headEnt != nil {
			msgIDStr := ""
			if headEnt.MessageID != nil {
				msgIDStr = headEnt.MessageID.String()
			}
			return nil, &store.CommandRequestKeyPendingError{
				Key:       headEnt.Key,
				Version:   headEnt.Version,
				MessageID: msgIDStr,
				RowID:     headEnt.ID.String(),
				Status:    string(headEnt.Status),
			}
		}
		return nil, planErr
	}

	// Enforce per-agent pending cap (10 pending non-expired command requests,
	// design §4.5). Replacing an existing pending row via --update keeps the
	// net pending count unchanged and is allowed even at the cap.
	// On PostgreSQL (READ COMMITTED), take a transaction-scoped per-agent
	// advisory lock before counting so concurrent sends with distinct keys
	// cannot phantom-insert past the cap. SQLite serializes writes natively.
	if plan.ReplacesRowID == "" || !plan.MarkReplaced {
		if c.isPostgres() {
			var lockRes stdsql.Result
			if lErr := client.Driver().Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)",
				[]any{int32(store.LockCommandRequestQuota), store.StableProjectHash(in.AgentID)}, &lockRes); lErr != nil {
				return nil, fmt.Errorf("lock command request quota: %w", lErr)
			}
		}
		pendingCount, perr := client.CommandRequest.Query().
			Where(
				commandrequest.AgentIDEQ(agentID),
				commandrequest.StatusEQ(commandrequest.StatusPending),
				commandrequest.ExpiresAtGT(now),
			).
			Count(ctx)
		if perr != nil {
			return nil, mapError(perr)
		}
		if pendingCount >= messages.MaxPendingCommandRequestsPerAgent {
			return nil, &store.CommandRequestLimitError{
				AgentID: in.AgentID,
				Limit:   messages.MaxPendingCommandRequestsPerAgent,
				Current: pendingCount,
			}
		}
	}

	intro := strings.TrimSpace(in.Intro)
	introLen := len(intro)

	msg := *in.Message
	msg.Type = messages.TypeCommandRequest
	msg.Msg = messages.RenderCommandRequestFallback(intro, messages.CommandRequestFallbackInput{
		Key:          in.Request.Key,
		Version:      plan.NextVersion,
		Description:  in.Request.Description,
		Script:       in.Request.Script,
		Workdir:      in.Request.Workdir,
		TimeoutS:     in.Request.TimeoutS,
		ApproverHint: in.ApproverHint,
		AgentName:    in.AgentName,
		ProjectName:  in.ProjectName,
	})
	if err := c.CreateMessage(ctx, &msg); err != nil {
		return nil, err
	}

	var convUID *uuid.UUID
	if msg.ConversationID != "" {
		if u, err := parseUUID(msg.ConversationID); err == nil {
			convUID = &u
		}
	}
	chatKey := strings.TrimSpace(msg.ThreadID)
	if chatKey == "" && strings.HasPrefix(msg.Recipient, "user:") && msg.RecipientID != "" {
		if dmKey, dmErr := messages.DMConversationKey("agent", in.AgentID, "user", msg.RecipientID); dmErr == nil {
			chatKey = dmKey
		}
	}

	rowID := uuid.New()
	builder := client.CommandRequest.Create().
		SetID(rowID).
		SetProjectID(projectID).
		SetAgentID(agentID).
		SetAgentName(in.AgentName).
		SetKey(in.Request.Key).
		SetKeyNorm(norm).
		SetVersion(plan.NextVersion).
		SetDescription(in.Request.Description).
		SetScript(in.Request.Script).
		SetWorkdir(in.Request.Workdir).
		SetTimeoutS(in.Request.TimeoutS).
		SetShareOutputRequested(in.Request.ShareOutputRequested).
		SetCommandSha256(in.Request.CommandSHA256).
		SetApproverUserID(in.ApproverID).
		SetMessageID(msgID).
		SetStatus(commandrequest.StatusPending).
		SetCreatedAt(now).
		SetExpiresAt(expiresAt)
	if introLen > 0 {
		builder.SetIntroLength(introLen)
	}
	if convUID != nil {
		builder.SetConversationID(*convUID)
	}
	if chatKey != "" {
		builder.SetChatKey(chatKey)
	}
	inserted, err := builder.Save(ctx)
	if err != nil {
		return nil, markMessageActionRace(err)
	}

	var replacedRow *store.CommandRequest
	if plan.ReplacesRowID != "" {
		prevUID, _ := parseUUID(plan.ReplacesRowID)
		if plan.MarkReplaced {
			ok, _, cerr := casActionRows[*ent.CommandRequest](
				ctx,
				1,
				func(ctx context.Context) (int, error) {
					return client.CommandRequest.Update().
						Where(
							commandrequest.IDEQ(prevUID),
							commandrequest.StatusEQ(commandrequest.StatusPending),
							commandrequest.ReplacedByIDIsNil(),
						).
						SetStatus(commandrequest.StatusReplaced).
						SetReplacedByID(rowID).
						SetReplacedAt(now).
						SetResolvedAt(now).
						Save(ctx)
				},
				nil,
			)
			if cerr != nil {
				return nil, cerr
			}
			if !ok {
				return nil, errMessageActionRace
			}
		} else {
			_, uerr := client.CommandRequest.Update().
				Where(
					commandrequest.IDEQ(prevUID),
					commandrequest.ReplacedByIDIsNil(),
				).
				SetReplacedByID(rowID).
				SetReplacedAt(now).
				Save(ctx)
			if uerr != nil {
				return nil, mapError(uerr)
			}
		}
		if headEnt != nil {
			prev := entCommandRequestToStore(headEnt)
			if plan.MarkReplaced {
				prev.Status = store.CommandRequestStatusReplaced
				prev.ResolvedAt = &now
			}
			prev.ReplacedByID = rowID.String()
			prev.ReplacedAt = &now
			replacedRow = prev
		}
	}

	in.Message.Type = msg.Type
	in.Message.Msg = msg.Msg
	return &store.CommandRequestPromptResult{
		Message:  &msg,
		Request:  entCommandRequestToStore(inserted),
		Replaced: replacedRow,
	}, nil
}

// GetCommandRequest implements store.CommandRequestStore.
func (c *CompositeStore) GetCommandRequest(ctx context.Context, id string) (*store.CommandRequest, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return nil, err
	}
	row, err := c.client.CommandRequest.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entCommandRequestToStore(row), nil
}

// GetCommandRequestByMessageID implements store.CommandRequestStore.
func (c *CompositeStore) GetCommandRequestByMessageID(ctx context.Context, messageID string) (*store.CommandRequest, error) {
	uid, err := parseUUID(messageID)
	if err != nil {
		return nil, err
	}
	row, err := c.client.CommandRequest.Query().
		Where(commandrequest.MessageIDEQ(uid)).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entCommandRequestToStore(row), nil
}

// ListAgentCommandRequests implements store.CommandRequestStore.
func (c *CompositeStore) ListAgentCommandRequests(ctx context.Context, opts *store.ListAgentCommandRequestsOptions) (*store.ListAgentCommandRequestsResult, error) {
	if opts == nil || opts.AgentID == "" {
		return nil, fmt.Errorf("%w: agentID is required", store.ErrInvalidInput)
	}
	agentUID, err := parseUUID(opts.AgentID)
	if err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = store.DefaultCommandRequestListLimit
	}
	if limit > store.MaxCommandRequestListLimit {
		limit = store.MaxCommandRequestListLimit
	}

	q := c.client.CommandRequest.Query().
		Where(commandrequest.AgentIDEQ(agentUID))
	if k := strings.TrimSpace(opts.Key); k != "" {
		q = q.Where(commandrequest.KeyNormEQ(messages.CommandRequestKeyNorm(k)))
	}

	if opts.Cursor != "" {
		tsStr, idStr, ok := strings.Cut(opts.Cursor, "|")
		if !ok {
			return nil, fmt.Errorf("%w: invalid cursor", store.ErrInvalidInput)
		}
		ts, terr := time.Parse(time.RFC3339Nano, tsStr)
		if terr != nil {
			return nil, fmt.Errorf("%w: invalid cursor timestamp", store.ErrInvalidInput)
		}
		curUID, uerr := parseUUID(idStr)
		if uerr != nil {
			return nil, fmt.Errorf("%w: invalid cursor id", store.ErrInvalidInput)
		}
		q = q.Where(
			commandrequest.Or(
				commandrequest.CreatedAtLT(ts),
				commandrequest.And(
					commandrequest.CreatedAtEQ(ts),
					commandrequest.IDLT(curUID),
				),
			),
		)
	}

	rows, err := q.Order(
		ent.Desc(commandrequest.FieldCreatedAt),
		ent.Desc(commandrequest.FieldID),
	).Limit(limit + 1).All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	var nextCursor string
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		nextCursor = last.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + last.ID.String()
	}

	items := make([]*store.CommandRequest, len(rows))
	reqIDs := make([]string, len(rows))
	for i, r := range rows {
		items[i] = entCommandRequestToStore(r)
		reqIDs[i] = r.ID.String()
	}

	runs, err := c.GetCommandRunsByRequestIDs(ctx, reqIDs)
	if err != nil {
		return nil, err
	}

	return &store.ListAgentCommandRequestsResult{
		Items:      items,
		Runs:       runs,
		NextCursor: nextCursor,
	}, nil
}

// commandRequestNotMarkdownBlock excludes self-run markdown_block rows while
// keeping rows whose surface is NULL (e.g. pending, withdrawn, expired).
func commandRequestNotMarkdownBlock() predicate.CommandRequest {
	return commandrequest.Or(
		commandrequest.SurfaceIsNil(),
		commandrequest.SurfaceNEQ(commandrequest.SurfaceMarkdownBlock),
	)
}

// ListOpenCommandRequests implements store.CommandRequestStore.
func (c *CompositeStore) ListOpenCommandRequests(ctx context.Context, viewerID string, now time.Time, limit int) ([]*store.CommandRequest, error) {
	viewerID = strings.TrimSpace(viewerID)
	if viewerID == "" {
		return []*store.CommandRequest{}, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC().Truncate(time.Microsecond)

	q := c.client.CommandRequest.Query().
		Where(
			commandrequest.ApproverUserIDEQ(viewerID),
			commandrequest.StatusEQ(commandrequest.StatusPending),
			commandrequest.ExpiresAtGT(now),
			commandRequestNotMarkdownBlock(),
			commandrequest.MessageIDNotNil(),
		).
		Order(
			ent.Desc(commandrequest.FieldCreatedAt),
			ent.Desc(commandrequest.FieldID),
		)
	if limit > 0 {
		q = q.Limit(limit)
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	items := make([]*store.CommandRequest, len(rows))
	for i, r := range rows {
		items[i] = entCommandRequestToStore(r)
	}
	return items, nil
}

// ListResolvedCommandRequests implements store.CommandRequestStore.
func (c *CompositeStore) ListResolvedCommandRequests(ctx context.Context, opts store.CommandRequestHistoryQuery) ([]*store.CommandRequest, error) {
	viewerID := strings.TrimSpace(opts.ViewerID)
	if viewerID == "" {
		return []*store.CommandRequest{}, nil
	}

	q := c.client.CommandRequest.Query().
		Where(
			commandrequest.ApproverUserIDEQ(viewerID),
			commandrequest.StatusIn(
				commandrequest.StatusApproved,
				commandrequest.StatusDismissed,
				commandrequest.StatusWithdrawn,
				commandrequest.StatusExpired,
			),
			commandrequest.ResolvedAtNotNil(),
			commandRequestNotMarkdownBlock(),
			commandrequest.MessageIDNotNil(),
		)

	if pid := strings.TrimSpace(opts.FilterProjectID); pid != "" {
		pUID, perr := parseUUID(pid)
		if perr != nil {
			return []*store.CommandRequest{}, nil
		}
		q = q.Where(commandrequest.ProjectIDEQ(pUID))
	}
	if aid := strings.TrimSpace(opts.FilterAgentID); aid != "" {
		aUID, aerr := parseUUID(aid)
		if aerr != nil {
			return []*store.CommandRequest{}, nil
		}
		q = q.Where(commandrequest.AgentIDEQ(aUID))
	}
	if opts.CursorTime != nil && opts.CursorID != "" {
		curUID, uerr := parseUUID(opts.CursorID)
		if uerr != nil {
			return nil, fmt.Errorf("%w: invalid cursor id", store.ErrInvalidInput)
		}
		curTime := opts.CursorTime.UTC()
		q = q.Where(
			commandrequest.Or(
				commandrequest.ResolvedAtLT(curTime),
				commandrequest.And(
					commandrequest.ResolvedAtEQ(curTime),
					commandrequest.IDLT(curUID),
				),
			),
		)
	}

	q = q.Order(
		ent.Desc(commandrequest.FieldResolvedAt),
		ent.Desc(commandrequest.FieldID),
	)
	if opts.Limit > 0 {
		q = q.Limit(opts.Limit)
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	items := make([]*store.CommandRequest, len(rows))
	for i, r := range rows {
		items[i] = entCommandRequestToStore(r)
	}
	return items, nil
}

// GetCommandRequestsByIDs implements store.CommandRequestStore.
func (c *CompositeStore) GetCommandRequestsByIDs(ctx context.Context, ids []string) (map[string]*store.CommandRequest, error) {
	out := make(map[string]*store.CommandRequest, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	uids := make([]uuid.UUID, 0, len(ids))
	seen := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		u, err := uuid.Parse(strings.TrimSpace(id))
		if err != nil {
			continue
		}
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		uids = append(uids, u)
	}
	if len(uids) == 0 {
		return out, nil
	}
	rows, err := c.client.CommandRequest.Query().
		Where(commandrequest.IDIn(uids...)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	for _, r := range rows {
		out[r.ID.String()] = entCommandRequestToStore(r)
	}
	return out, nil
}

// ApproveCommandRequest implements store.CommandRequestStore.
func (c *CompositeStore) ApproveCommandRequest(ctx context.Context, in *store.ApproveCommandRequestInput) (*store.ApproveCommandRequestResult, error) {
	if in == nil || in.RequestID == "" || in.UserID == "" || in.CommandSHA256 == "" || in.ExecAgentID == "" || in.ExecProjectID == "" {
		return nil, fmt.Errorf("%w: requestID, userID, commandSHA256, execAgentID, and execProjectID are required", store.ErrInvalidInput)
	}
	reqUID, err := parseUUID(in.RequestID)
	if err != nil {
		return nil, err
	}
	execAgentUID, err := parseUUID(in.ExecAgentID)
	if err != nil {
		return nil, err
	}
	execProjectUID, err := parseUUID(in.ExecProjectID)
	if err != nil {
		return nil, err
	}
	now := in.Now.UTC().Truncate(time.Microsecond)
	if now.IsZero() {
		now = time.Now().UTC().Truncate(time.Microsecond)
	}
	leaseUntil := in.LeaseUntil.UTC().Truncate(time.Microsecond)
	if leaseUntil.IsZero() {
		leaseUntil = now.Add(6 * time.Minute)
	}
	surface := in.Surface
	if surface == "" {
		surface = msgaction.CommandRequestSurfaceNative
	}

	return withMessageActionRetry(ctx, c, func(tx *CompositeStore) (*store.ApproveCommandRequestResult, error) {
		q := tx.client.CommandRequest.Query().
			Where(commandrequest.IDEQ(reqUID))
		if tx.isPostgres() {
			q = q.ForUpdate()
		}
		row, err := q.Only(ctx)
		if err != nil {
			return nil, mapError(err)
		}

		// Idempotent replay (§4.7 step 6): same user already approved with same idempotency_key.
		if in.IdempotencyKey != "" &&
			row.Status == commandrequest.StatusApproved &&
			row.IdempotencyKey != nil && *row.IdempotencyKey == in.IdempotencyKey &&
			row.ResolvedByUserID != nil && *row.ResolvedByUserID == in.UserID {
			runRow, rerr := tx.client.CommandRun.Query().
				Where(commandrun.RequestIDEQ(reqUID)).
				Only(ctx)
			if rerr != nil {
				return nil, mapError(rerr)
			}
			return &store.ApproveCommandRequestResult{
				Request:  entCommandRequestToStore(row),
				Run:      entCommandRunToStore(runRow),
				Replayed: true,
			}, nil
		}

		// Single CAS: pending -> approved WHERE id AND status='pending' AND command_sha256=? AND expires_at > now.
		ok, current, cerr := casActionRows[*ent.CommandRequest](
			ctx,
			1,
			func(ctx context.Context) (int, error) {
				upd := tx.client.CommandRequest.Update().
					Where(
						commandrequest.IDEQ(reqUID),
						commandrequest.StatusEQ(commandrequest.StatusPending),
						commandrequest.CommandSha256EQ(in.CommandSHA256),
						commandrequest.ExpiresAtGT(now),
					).
					SetStatus(commandrequest.StatusApproved).
					SetResolvedByUserID(in.UserID).
					SetResolvedAt(now).
					SetSurface(commandrequest.Surface(surface))
				if in.UserName != "" {
					upd.SetResolvedByName(in.UserName)
				}
				if in.UserEmail != "" {
					upd.SetResolvedByEmail(in.UserEmail)
				}
				if in.IdempotencyKey != "" {
					upd.SetIdempotencyKey(in.IdempotencyKey)
				}
				return upd.Save(ctx)
			},
			func(ctx context.Context) ([]*ent.CommandRequest, error) {
				reloaded, rerr := tx.client.CommandRequest.Get(ctx, reqUID)
				if rerr != nil {
					return nil, rerr
				}
				return []*ent.CommandRequest{reloaded}, nil
			},
		)
		if cerr != nil {
			return nil, cerr
		}
		if !ok {
			if len(current) == 0 {
				return nil, store.ErrNotFound
			}
			cur := current[0]
			code := classifyApproveConflict(cur, in.CommandSHA256, now)
			return &store.ApproveCommandRequestResult{
				Conflict:     entCommandRequestToStore(cur),
				ConflictCode: code,
			}, nil
		}

		// Insert single queued command_runs row in the same tx. UNIQUE(request_id)
		// guarantees at-most-one run per approved version.
		runID := uuid.New()
		runEnt, err := tx.client.CommandRun.Create().
			SetID(runID).
			SetRequestID(reqUID).
			SetCommandSha256(row.CommandSha256).
			SetScriptLen(len(row.Script)).
			SetApproverUserID(in.UserID).
			SetExecAgentID(execAgentUID).
			SetExecProjectID(execProjectUID).
			SetStatus(commandrun.StatusQueued).
			SetWake(commandrun.WakeNone).
			SetShareOutput(in.ShareOutput).
			SetTimeoutS(row.TimeoutS).
			SetCaptureMode("tail").
			SetWorkdir(row.Workdir).
			SetLeaseUntil(leaseUntil).
			SetCreatedAt(now).
			Save(ctx)
		if err != nil {
			return nil, mapError(err)
		}

		updatedReq, err := tx.client.CommandRequest.Get(ctx, reqUID)
		if err != nil {
			return nil, mapError(err)
		}
		return &store.ApproveCommandRequestResult{
			Request: entCommandRequestToStore(updatedReq),
			Run:     entCommandRunToStore(runEnt),
		}, nil
	})
}

func classifyApproveConflict(row *ent.CommandRequest, submittedHash string, now time.Time) string {
	if row.Status == commandrequest.StatusPending {
		if !row.ExpiresAt.After(now) {
			return msgaction.ConflictExpired
		}
		if row.CommandSha256 != submittedHash {
			return msgaction.ConflictCmdHashMismatch
		}
	}
	if code := msgaction.ConflictCode(msgaction.CommandRequestKindSpec, string(row.Status)); code != "" {
		return code
	}
	return msgaction.ConflictAlreadyResolved
}

// CreateAndApproveMarkdownCommandRequest implements store.CommandRequestStore (§4.14 step 5).
func (c *CompositeStore) CreateAndApproveMarkdownCommandRequest(ctx context.Context, in *store.CreateMarkdownCommandRequestInput) (*store.ApproveCommandRequestResult, error) {
	if in == nil || in.ProjectID == "" || in.AgentID == "" || in.MessageID == "" || in.UserID == "" || in.ExecAgentID == "" || in.ExecProjectID == "" {
		return nil, fmt.Errorf("%w: projectID, agentID, messageID, userID, execAgentID, and execProjectID are required", store.ErrInvalidInput)
	}
	projectUID, err := parseUUID(in.ProjectID)
	if err != nil {
		return nil, err
	}
	agentUID, err := parseUUID(in.AgentID)
	if err != nil {
		return nil, err
	}
	msgUID, err := parseUUID(in.MessageID)
	if err != nil {
		return nil, err
	}
	execAgentUID, err := parseUUID(in.ExecAgentID)
	if err != nil {
		return nil, err
	}
	execProjectUID, err := parseUUID(in.ExecProjectID)
	if err != nil {
		return nil, err
	}
	var convUID *uuid.UUID
	if strings.TrimSpace(in.ConversationID) != "" {
		u, uerr := parseUUID(in.ConversationID)
		if uerr != nil {
			return nil, uerr
		}
		convUID = &u
	}
	if err := messages.ValidateCommandRequestScript(in.Script); err != nil {
		return nil, fmt.Errorf("%w: %v", store.ErrInvalidInput, err)
	}
	expectedHash := messages.CommandRequestCanonicalHash(in.Script, "", messages.DefaultCommandRequestTimeoutS)
	hashHex := strings.ToLower(strings.TrimSpace(in.CommandSHA256))
	if hashHex == "" {
		hashHex = expectedHash
	} else if hashHex != expectedHash {
		return nil, fmt.Errorf("%w: command_sha256 does not match canonical hash", store.ErrInvalidInput)
	}

	now := in.Now.UTC().Truncate(time.Microsecond)
	if now.IsZero() {
		now = time.Now().UTC().Truncate(time.Microsecond)
	}
	ttl := in.TTL
	if ttl <= 0 {
		ttl = msgaction.CommandRequestTTL
	}
	expiresAt := now.Add(ttl)
	leaseUntil := in.LeaseUntil.UTC().Truncate(time.Microsecond)
	if leaseUntil.IsZero() {
		leaseUntil = now.Add(6 * time.Minute)
	}

	key := messages.MarkdownCommandRequestKey(in.MessageID, hashHex)
	keyNorm := messages.CommandRequestKeyNorm(key)
	chatKey := strings.TrimSpace(in.ChatKey)

	lookupExisting := func(client *ent.Client) (*store.ApproveCommandRequestResult, error) {
		existing, qerr := client.CommandRequest.Query().
			Where(
				commandrequest.AgentIDEQ(agentUID),
				commandrequest.KeyNormEQ(keyNorm),
				commandrequest.VersionEQ(1),
			).
			Only(ctx)
		if qerr != nil {
			return nil, mapError(qerr)
		}
		var existingRun *store.CommandRun
		if runEnt, rerr := client.CommandRun.Query().
			Where(commandrun.RequestIDEQ(existing.ID)).
			Only(ctx); rerr == nil && runEnt != nil {
			existingRun = entCommandRunToStore(runEnt)
		}
		if in.IdempotencyKey != "" &&
			existing.Status == commandrequest.StatusApproved &&
			existing.IdempotencyKey != nil && *existing.IdempotencyKey == in.IdempotencyKey &&
			existing.ResolvedByUserID != nil && *existing.ResolvedByUserID == in.UserID &&
			existingRun != nil {
			return &store.ApproveCommandRequestResult{
				Request:  entCommandRequestToStore(existing),
				Run:      existingRun,
				Replayed: true,
			}, nil
		}
		return &store.ApproveCommandRequestResult{
			Conflict:     entCommandRequestToStore(existing),
			ConflictRun:  existingRun,
			ConflictCode: msgaction.ConflictAlreadyResolved,
		}, nil
	}

	var res *store.ApproveCommandRequestResult
	err = runInEntTx(ctx, c.client, c.inTx, func(client *ent.Client) error {
		q := client.CommandRequest.Query().
			Where(
				commandrequest.AgentIDEQ(agentUID),
				commandrequest.KeyNormEQ(keyNorm),
				commandrequest.VersionEQ(1),
			)
		if c.isPostgres() {
			q = q.ForUpdate()
		}
		existing, qerr := q.Only(ctx)
		if qerr == nil && existing != nil {
			var existingRun *store.CommandRun
			if runEnt, rerr := client.CommandRun.Query().
				Where(commandrun.RequestIDEQ(existing.ID)).
				Only(ctx); rerr == nil && runEnt != nil {
				existingRun = entCommandRunToStore(runEnt)
			}
			if in.IdempotencyKey != "" &&
				existing.Status == commandrequest.StatusApproved &&
				existing.IdempotencyKey != nil && *existing.IdempotencyKey == in.IdempotencyKey &&
				existing.ResolvedByUserID != nil && *existing.ResolvedByUserID == in.UserID &&
				existingRun != nil {
				res = &store.ApproveCommandRequestResult{
					Request:  entCommandRequestToStore(existing),
					Run:      existingRun,
					Replayed: true,
				}
				return nil
			}
			res = &store.ApproveCommandRequestResult{
				Conflict:     entCommandRequestToStore(existing),
				ConflictRun:  existingRun,
				ConflictCode: msgaction.ConflictAlreadyResolved,
			}
			return nil
		}
		if qerr != nil && !ent.IsNotFound(qerr) {
			return mapError(qerr)
		}

		rowID := uuid.New()
		reqBuilder := client.CommandRequest.Create().
			SetID(rowID).
			SetProjectID(projectUID).
			SetAgentID(agentUID).
			SetAgentName(in.AgentName).
			SetKey(key).
			SetKeyNorm(keyNorm).
			SetVersion(1).
			SetDescription("").
			SetScript(in.Script).
			SetWorkdir("").
			SetTimeoutS(messages.DefaultCommandRequestTimeoutS).
			SetShareOutputRequested(false).
			SetCommandSha256(hashHex).
			SetApproverUserID(in.UserID).
			SetMessageID(msgUID).
			SetStatus(commandrequest.StatusApproved).
			SetResolvedByUserID(in.UserID).
			SetResolvedAt(now).
			SetSurface(commandrequest.Surface(msgaction.CommandRequestSurfaceMarkdownBlock)).
			SetCreatedAt(now).
			SetExpiresAt(expiresAt)
		if in.UserName != "" {
			reqBuilder.SetResolvedByName(in.UserName)
		}
		if in.UserEmail != "" {
			reqBuilder.SetResolvedByEmail(in.UserEmail)
		}
		if in.IdempotencyKey != "" {
			reqBuilder.SetIdempotencyKey(in.IdempotencyKey)
		}
		if convUID != nil {
			reqBuilder.SetConversationID(*convUID)
		}
		if chatKey != "" {
			reqBuilder.SetChatKey(chatKey)
		}
		insertedReq, ierr := reqBuilder.Save(ctx)
		if ierr != nil {
			return ierr
		}

		runID := uuid.New()
		runEnt, rerr := client.CommandRun.Create().
			SetID(runID).
			SetRequestID(rowID).
			SetCommandSha256(hashHex).
			SetScriptLen(len(in.Script)).
			SetApproverUserID(in.UserID).
			SetExecAgentID(execAgentUID).
			SetExecProjectID(execProjectUID).
			SetStatus(commandrun.StatusQueued).
			SetWake(commandrun.WakeNone).
			SetShareOutput(in.ShareOutput).
			SetTimeoutS(messages.DefaultCommandRequestTimeoutS).
			SetCaptureMode("tail").
			SetWorkdir("").
			SetLeaseUntil(leaseUntil).
			SetCreatedAt(now).
			Save(ctx)
		if rerr != nil {
			return mapError(rerr)
		}

		res = &store.ApproveCommandRequestResult{
			Request: entCommandRequestToStore(insertedReq),
			Run:     entCommandRunToStore(runEnt),
		}
		return nil
	})
	if err != nil {
		if ent.IsConstraintError(err) || isSQLiteUniqueConstraint(err) {
			return lookupExisting(c.client)
		}
		return nil, mapError(err)
	}
	return res, nil
}

func isSQLiteUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "duplicate key value")
}

func classifyCommandRequestNonPendingConflict(row *ent.CommandRequest, now time.Time) string {
	if row.Status == commandrequest.StatusPending && !row.ExpiresAt.After(now) {
		return msgaction.ConflictExpired
	}
	if code := msgaction.ConflictCode(msgaction.CommandRequestKindSpec, string(row.Status)); code != "" {
		return code
	}
	return msgaction.ConflictAlreadyResolved
}

// DismissCommandRequest implements store.CommandRequestStore.
func (c *CompositeStore) DismissCommandRequest(ctx context.Context, in *store.DismissCommandRequestInput) (*store.DismissCommandRequestResult, error) {
	if in == nil || in.RequestID == "" || in.UserID == "" {
		return nil, fmt.Errorf("%w: requestID and userID are required", store.ErrInvalidInput)
	}
	reqUID, err := parseUUID(in.RequestID)
	if err != nil {
		return nil, err
	}
	now := in.Now.UTC().Truncate(time.Microsecond)
	if now.IsZero() {
		now = time.Now().UTC().Truncate(time.Microsecond)
	}
	surface := in.Surface
	if surface == "" {
		surface = msgaction.CommandRequestSurfaceNative
	}

	return withMessageActionRetry(ctx, c, func(tx *CompositeStore) (*store.DismissCommandRequestResult, error) {
		q := tx.client.CommandRequest.Query().
			Where(commandrequest.IDEQ(reqUID))
		if tx.isPostgres() {
			q = q.ForUpdate()
		}
		row, err := q.Only(ctx)
		if err != nil {
			return nil, mapError(err)
		}

		// Idempotent repeat on an already-dismissed row (AC-F6).
		if row.Status == commandrequest.StatusDismissed {
			replayed := in.IdempotencyKey != "" &&
				row.IdempotencyKey != nil && *row.IdempotencyKey == in.IdempotencyKey &&
				row.ResolvedByUserID != nil && *row.ResolvedByUserID == in.UserID
			return &store.DismissCommandRequestResult{
				Request:          entCommandRequestToStore(row),
				AlreadyDismissed: true,
				Replayed:         replayed,
			}, nil
		}

		// CAS pending -> dismissed WHERE id AND status='pending' AND expires_at > now.
		ok, current, cerr := casActionRows[*ent.CommandRequest](
			ctx,
			1,
			func(ctx context.Context) (int, error) {
				upd := tx.client.CommandRequest.Update().
					Where(
						commandrequest.IDEQ(reqUID),
						commandrequest.StatusEQ(commandrequest.StatusPending),
						commandrequest.ExpiresAtGT(now),
					).
					SetStatus(commandrequest.StatusDismissed).
					SetResolvedByUserID(in.UserID).
					SetResolvedAt(now).
					SetSurface(commandrequest.Surface(surface))
				if in.UserName != "" {
					upd.SetResolvedByName(in.UserName)
				}
				if in.UserEmail != "" {
					upd.SetResolvedByEmail(in.UserEmail)
				}
				if in.Reason != "" {
					upd.SetDismissReason(in.Reason)
				}
				if in.IdempotencyKey != "" {
					upd.SetIdempotencyKey(in.IdempotencyKey)
				}
				return upd.Save(ctx)
			},
			func(ctx context.Context) ([]*ent.CommandRequest, error) {
				reloaded, rerr := tx.client.CommandRequest.Get(ctx, reqUID)
				if rerr != nil {
					return nil, rerr
				}
				return []*ent.CommandRequest{reloaded}, nil
			},
		)
		if cerr != nil {
			return nil, cerr
		}
		if !ok {
			if len(current) == 0 {
				return nil, store.ErrNotFound
			}
			cur := current[0]
			if cur.Status == commandrequest.StatusDismissed {
				return &store.DismissCommandRequestResult{
					Request:          entCommandRequestToStore(cur),
					AlreadyDismissed: true,
				}, nil
			}
			return &store.DismissCommandRequestResult{
				Conflict:     entCommandRequestToStore(cur),
				ConflictCode: classifyCommandRequestNonPendingConflict(cur, now),
			}, nil
		}

		updated, err := tx.client.CommandRequest.Get(ctx, reqUID)
		if err != nil {
			return nil, mapError(err)
		}
		return &store.DismissCommandRequestResult{
			Request: entCommandRequestToStore(updated),
		}, nil
	})
}

// WithdrawCommandRequest implements store.CommandRequestStore.
func (c *CompositeStore) WithdrawCommandRequest(ctx context.Context, in *store.WithdrawCommandRequestInput) (*store.WithdrawCommandRequestResult, error) {
	if in == nil || in.AgentID == "" || strings.TrimSpace(in.Key) == "" {
		return nil, fmt.Errorf("%w: agentID and key are required", store.ErrInvalidInput)
	}
	agentUID, err := parseUUID(in.AgentID)
	if err != nil {
		return nil, err
	}
	now := in.Now.UTC().Truncate(time.Microsecond)
	if now.IsZero() {
		now = time.Now().UTC().Truncate(time.Microsecond)
	}
	norm := messages.CommandRequestKeyNorm(in.Key)

	return withMessageActionRetry(ctx, c, func(tx *CompositeStore) (*store.WithdrawCommandRequestResult, error) {
		q := tx.client.CommandRequest.Query().
			Where(
				commandrequest.AgentIDEQ(agentUID),
				commandrequest.KeyNormEQ(norm),
			).
			Order(ent.Desc(commandrequest.FieldVersion)).
			Limit(1)
		if tx.isPostgres() {
			q = q.ForUpdate()
		}
		rows, err := q.All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		if len(rows) == 0 {
			return nil, store.ErrNotFound
		}
		head := rows[0]

		// Idempotent repeat on an already-withdrawn head (AC-F6).
		if head.Status == commandrequest.StatusWithdrawn {
			return &store.WithdrawCommandRequestResult{
				Request:          entCommandRequestToStore(head),
				AlreadyWithdrawn: true,
			}, nil
		}

		if head.Status != commandrequest.StatusPending {
			return &store.WithdrawCommandRequestResult{
				Conflict:     entCommandRequestToStore(head),
				ConflictCode: classifyCommandRequestNonPendingConflict(head, now),
			}, nil
		}
		if !head.ExpiresAt.After(now) {
			exp := entCommandRequestToStore(head)
			exp.Status = store.CommandRequestStatusExpired
			return &store.WithdrawCommandRequestResult{
				Conflict:     exp,
				ConflictCode: msgaction.ConflictExpired,
			}, nil
		}

		ok, current, cerr := casActionRows[*ent.CommandRequest](
			ctx,
			1,
			func(ctx context.Context) (int, error) {
				upd := tx.client.CommandRequest.Update().
					Where(
						commandrequest.IDEQ(head.ID),
						commandrequest.StatusEQ(commandrequest.StatusPending),
						commandrequest.ExpiresAtGT(now),
					).
					SetStatus(commandrequest.StatusWithdrawn).
					SetWithdrawnBy(store.CommandRequestWithdrawnByAgent).
					SetResolvedAt(now)
				if in.Reason != "" {
					upd.SetWithdrawnReason(in.Reason)
				}
				return upd.Save(ctx)
			},
			func(ctx context.Context) ([]*ent.CommandRequest, error) {
				reloaded, rerr := tx.client.CommandRequest.Get(ctx, head.ID)
				if rerr != nil {
					return nil, rerr
				}
				return []*ent.CommandRequest{reloaded}, nil
			},
		)
		if cerr != nil {
			return nil, cerr
		}
		if !ok {
			if len(current) == 0 {
				return nil, store.ErrNotFound
			}
			cur := current[0]
			if cur.Status == commandrequest.StatusWithdrawn {
				return &store.WithdrawCommandRequestResult{
					Request:          entCommandRequestToStore(cur),
					AlreadyWithdrawn: true,
				}, nil
			}
			return &store.WithdrawCommandRequestResult{
				Conflict:     entCommandRequestToStore(cur),
				ConflictCode: classifyCommandRequestNonPendingConflict(cur, now),
			}, nil
		}

		updated, err := tx.client.CommandRequest.Get(ctx, head.ID)
		if err != nil {
			return nil, mapError(err)
		}
		return &store.WithdrawCommandRequestResult{
			Request: entCommandRequestToStore(updated),
		}, nil
	})
}

// WithdrawAgentCommandRequestsOnDelete implements store.CommandRequestStore.
// Only pending rows are transitioned to withdrawn (withdrawn_by="system");
// approved rows and command_runs are left untouched (§4.12).
func (c *CompositeStore) WithdrawAgentCommandRequestsOnDelete(ctx context.Context, agentID string, now time.Time) ([]*store.CommandRequest, error) {
	agentUID, err := parseUUID(agentID)
	if err != nil {
		return nil, err
	}
	now = now.UTC().Truncate(time.Microsecond)
	if now.IsZero() {
		now = time.Now().UTC().Truncate(time.Microsecond)
	}
	q := c.client.CommandRequest.Query().
		Where(
			commandrequest.AgentIDEQ(agentUID),
			commandrequest.StatusEQ(commandrequest.StatusPending),
		)
	if c.isPostgres() {
		q = q.ForUpdate()
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	n, err := c.client.CommandRequest.Update().
		Where(
			commandrequest.IDIn(ids...),
			commandrequest.StatusEQ(commandrequest.StatusPending),
		).
		SetStatus(commandrequest.StatusWithdrawn).
		SetWithdrawnReason("agent deleted").
		SetWithdrawnBy(store.CommandRequestWithdrawnBySystem).
		SetResolvedAt(now).
		Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	if n == 0 {
		return nil, nil
	}
	updated, err := c.client.CommandRequest.Query().
		Where(
			commandrequest.IDIn(ids...),
			commandrequest.StatusEQ(commandrequest.StatusWithdrawn),
			commandrequest.WithdrawnByEQ(store.CommandRequestWithdrawnBySystem),
			commandrequest.ResolvedAtEQ(now),
		).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.CommandRequest, len(updated))
	for i, r := range updated {
		out[i] = entCommandRequestToStore(r)
	}
	store.RecordWithdrawnCommandRequests(ctx, out)
	return out, nil
}

// ExpireCommandRequests implements store.CommandRequestStore.
func (c *CompositeStore) ExpireCommandRequests(ctx context.Context, now time.Time, limit int) ([]*store.CommandRequest, error) {
	now = now.UTC().Truncate(time.Microsecond)
	if limit <= 0 {
		limit = 200
	}
	return withMessageActionRetry(ctx, c, func(tx *CompositeStore) ([]*store.CommandRequest, error) {
		q := tx.client.CommandRequest.Query().
			Where(
				commandrequest.StatusEQ(commandrequest.StatusPending),
				commandrequest.ExpiresAtLTE(now),
			).
			Order(ent.Asc(commandrequest.FieldExpiresAt)).
			Limit(limit)
		if tx.isPostgres() {
			q = q.ForUpdate(sql.WithLockAction(sql.SkipLocked))
		}
		candidates, err := q.All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		if len(candidates) == 0 {
			return nil, nil
		}
		ids := make([]uuid.UUID, len(candidates))
		for i, r := range candidates {
			ids[i] = r.ID
		}
		n, err := tx.client.CommandRequest.Update().
			Where(
				commandrequest.IDIn(ids...),
				commandrequest.StatusEQ(commandrequest.StatusPending),
				commandrequest.ExpiresAtLTE(now),
			).
			SetStatus(commandrequest.StatusExpired).
			SetResolvedAt(now).
			Save(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		if n == 0 {
			return nil, nil
		}
		updated, err := tx.client.CommandRequest.Query().
			Where(
				commandrequest.IDIn(ids...),
				commandrequest.StatusEQ(commandrequest.StatusExpired),
				commandrequest.ResolvedAtEQ(now),
			).
			All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		out := make([]*store.CommandRequest, len(updated))
		for i, r := range updated {
			out[i] = entCommandRequestToStore(r)
		}
		return out, nil
	})
}

// GetCommandRun implements store.CommandRequestStore.
func (c *CompositeStore) GetCommandRun(ctx context.Context, runID string) (*store.CommandRun, error) {
	uid, err := parseUUID(runID)
	if err != nil {
		return nil, err
	}
	row, err := c.client.CommandRun.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entCommandRunToStore(row), nil
}

// GetCommandRunByRequestID implements store.CommandRequestStore.
func (c *CompositeStore) GetCommandRunByRequestID(ctx context.Context, requestID string) (*store.CommandRun, error) {
	uid, err := parseUUID(requestID)
	if err != nil {
		return nil, err
	}
	row, err := c.client.CommandRun.Query().
		Where(commandrun.RequestIDEQ(uid)).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entCommandRunToStore(row), nil
}

// GetCommandRunByOutcomeMessageID implements store.CommandRequestStore.
func (c *CompositeStore) GetCommandRunByOutcomeMessageID(ctx context.Context, messageID string) (*store.CommandRun, error) {
	msgUID, err := parseUUID(messageID)
	if err != nil {
		return nil, err
	}
	row, err := c.client.CommandRun.Query().
		Where(commandrun.OutcomeMessageIDEQ(msgUID)).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entCommandRunToStore(row), nil
}

// GetCommandRunsByRequestIDs implements store.CommandRequestStore.
func (c *CompositeStore) GetCommandRunsByRequestIDs(ctx context.Context, requestIDs []string) (map[string]*store.CommandRun, error) {
	out := make(map[string]*store.CommandRun, len(requestIDs))
	if len(requestIDs) == 0 {
		return out, nil
	}
	uids := make([]uuid.UUID, 0, len(requestIDs))
	for _, id := range requestIDs {
		if uid, err := uuid.Parse(id); err == nil {
			uids = append(uids, uid)
		}
	}
	if len(uids) == 0 {
		return out, nil
	}
	rows, err := c.client.CommandRun.Query().
		Where(commandrun.RequestIDIn(uids...)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	for _, r := range rows {
		out[r.RequestID.String()] = entCommandRunToStore(r)
	}
	return out, nil
}

// TransitionCommandRunToWaking implements store.CommandRequestStore.
func (c *CompositeStore) TransitionCommandRunToWaking(ctx context.Context, runID string, wake string) (bool, error) {
	uid, err := parseUUID(runID)
	if err != nil {
		return false, err
	}
	if wake != store.CommandRunWakeNone && wake != store.CommandRunWakeResume {
		return false, fmt.Errorf("%w: invalid wake %q", store.ErrInvalidInput, wake)
	}
	n, err := c.client.CommandRun.Update().
		Where(
			commandrun.IDEQ(uid),
			commandrun.StatusEQ(commandrun.StatusQueued),
			commandrun.FinishedAtIsNil(),
		).
		SetStatus(commandrun.StatusWaking).
		SetWake(commandrun.Wake(wake)).
		Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return n == 1, nil
}

// TransitionCommandRunToDispatching implements store.CommandRequestStore.
func (c *CompositeStore) TransitionCommandRunToDispatching(ctx context.Context, in *store.CommandRunDispatchInput) (bool, error) {
	if in == nil || in.RunID == "" || in.LaunchID == "" {
		return false, fmt.Errorf("%w: runID and launchID are required", store.ErrInvalidInput)
	}
	uid, err := parseUUID(in.RunID)
	if err != nil {
		return false, err
	}
	dispatchedAt := in.DispatchedAt.UTC().Truncate(time.Microsecond)
	if dispatchedAt.IsZero() {
		dispatchedAt = time.Now().UTC().Truncate(time.Microsecond)
	}
	notAfter := in.NotAfter.UTC().Truncate(time.Microsecond)
	leaseUntil := in.LeaseUntil.UTC().Truncate(time.Microsecond)

	upd := c.client.CommandRun.Update().
		Where(
			commandrun.IDEQ(uid),
			commandrun.StatusIn(commandrun.StatusQueued, commandrun.StatusWaking),
			commandrun.FinishedAtIsNil(),
			commandrun.LeaseUntilGT(dispatchedAt),
		).
		SetStatus(commandrun.StatusDispatching).
		SetLaunchID(in.LaunchID).
		SetDispatchedAt(dispatchedAt)
	if !notAfter.IsZero() {
		upd.SetNotAfter(notAfter)
	}
	if !leaseUntil.IsZero() {
		upd.SetLeaseUntil(leaseUntil)
	}
	n, err := upd.Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return n == 1, nil
}

// TransitionCommandRunToRunning implements store.CommandRequestStore.
func (c *CompositeStore) TransitionCommandRunToRunning(ctx context.Context, runID string) (bool, error) {
	uid, err := parseUUID(runID)
	if err != nil {
		return false, err
	}
	n, err := c.client.CommandRun.Update().
		Where(
			commandrun.IDEQ(uid),
			commandrun.StatusEQ(commandrun.StatusDispatching),
			commandrun.FinishedAtIsNil(),
		).
		SetStatus(commandrun.StatusRunning).
		Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return n == 1, nil
}

// FinishCommandRun implements store.CommandRequestStore.
func (c *CompositeStore) FinishCommandRun(ctx context.Context, in *store.FinishCommandRunInput) (bool, error) {
	if in == nil || in.RunID == "" || !store.IsTerminalCommandRunStatus(in.Status) {
		return false, store.ErrInvalidInput
	}
	if len(in.Output) > store.MaxExecRunOutputBytes {
		return false, store.ErrInvalidInput
	}
	uid, err := parseUUID(in.RunID)
	if err != nil {
		return false, err
	}
	finishedAt := in.FinishedAt.UTC().Truncate(time.Microsecond)
	if finishedAt.IsZero() {
		finishedAt = time.Now().UTC().Truncate(time.Microsecond)
	}

	var expectedStatuses []commandrun.Status
	if len(in.ExpectedStatuses) > 0 {
		expectedStatuses = make([]commandrun.Status, len(in.ExpectedStatuses))
		for i, st := range in.ExpectedStatuses {
			expectedStatuses[i] = commandrun.Status(st)
		}
	}

	var won bool
	err = runInEntTx(ctx, c.client, c.inTx, func(client *ent.Client) error {
		queryPreds := []predicate.CommandRun{
			commandrun.IDEQ(uid),
			commandrun.FinishedAtIsNil(),
		}
		if len(expectedStatuses) > 0 {
			queryPreds = append(queryPreds, commandrun.StatusIn(expectedStatuses...))
		}
		runRow, err := client.CommandRun.Query().
			Where(queryPreds...).
			Only(ctx)
		if ent.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return mapError(err)
		}
		n, err := client.CommandRun.Update().
			Where(queryPreds...).
			SetStatus(commandrun.Status(in.Status)).
			SetReason(in.Reason).
			SetNillableExitCode(in.ExitCode).
			SetNillableDurationMs(in.DurationMS).
			SetNillableOutputBytes(in.OutputBytes).
			SetOutputStatus(in.OutputStatus).
			SetFinishedAt(finishedAt).
			Save(ctx)
		if err != nil {
			return mapError(err)
		}
		if n == 0 {
			return nil
		}
		won = true
		if len(in.Output) > 0 {
			if _, err := client.ExecRunOutput.Create().
				SetID(uid).
				SetOwnerKind(execrunoutput.OwnerKindActionRun).
				SetProjectID(runRow.ExecProjectID).
				SetCaptureMode(runRow.CaptureMode).
				SetContent(in.Output).
				SetCreatedAt(finishedAt).
				Save(ctx); err != nil {
				return mapError(err)
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return won, nil
}

// ClaimCommandRunOutcomeDelivery implements store.CommandRequestStore.
func (c *CompositeStore) ClaimCommandRunOutcomeDelivery(ctx context.Context, runID string, deliveredAt, staleClaimBefore time.Time) (bool, error) {
	uid, err := parseUUID(runID)
	if err != nil {
		return false, err
	}
	ts := deliveredAt.UTC().Truncate(time.Microsecond)
	if ts.IsZero() {
		ts = time.Now().UTC().Truncate(time.Microsecond)
	}
	claimPred := commandrun.OutcomeDeliveredAtIsNil()
	if !staleClaimBefore.IsZero() {
		staleTS := staleClaimBefore.UTC().Truncate(time.Microsecond)
		claimPred = commandrun.Or(
			commandrun.OutcomeDeliveredAtIsNil(),
			commandrun.And(
				commandrun.OutcomeDeliveredAtLTE(staleTS),
				commandrun.OutcomeMessageIDIsNil(),
			),
		)
	}
	n, err := c.client.CommandRun.Update().
		Where(
			commandrun.IDEQ(uid),
			commandrun.FinishedAtNotNil(),
			claimPred,
		).
		SetOutcomeDeliveredAt(ts).
		Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return n == 1, nil
}

// ClearCommandRunOutcomeDelivery implements store.CommandRequestStore.
func (c *CompositeStore) ClearCommandRunOutcomeDelivery(ctx context.Context, runID string, claimedAt time.Time) (bool, error) {
	uid, err := parseUUID(runID)
	if err != nil {
		return false, err
	}
	ts := claimedAt.UTC().Truncate(time.Microsecond)
	n, err := c.client.CommandRun.Update().
		Where(
			commandrun.IDEQ(uid),
			commandrun.OutcomeDeliveredAtEQ(ts),
		).
		ClearOutcomeDeliveredAt().
		Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return n == 1, nil
}

// RecordCommandRunOutcomeMessage implements store.CommandRequestStore.
func (c *CompositeStore) RecordCommandRunOutcomeMessage(ctx context.Context, runID, messageID string) error {
	uid, err := parseUUID(runID)
	if err != nil {
		return err
	}
	msgUID, err := parseUUID(messageID)
	if err != nil {
		return err
	}
	_, err = c.client.CommandRun.Update().
		Where(commandrun.IDEQ(uid)).
		SetOutcomeMessageID(msgUID).
		Save(ctx)
	return mapError(err)
}

// ListExpiredCommandRuns implements store.CommandRequestStore.
func (c *CompositeStore) ListExpiredCommandRuns(ctx context.Context, now time.Time, limit int) ([]*store.CommandRun, error) {
	rows, err := c.client.CommandRun.Query().
		Where(
			commandrun.FinishedAtIsNil(),
			commandrun.LeaseUntilLT(now.UTC().Truncate(time.Microsecond)),
		).
		Order(commandrun.ByLeaseUntil(), commandrun.ByID()).
		Limit(clampLimit(limit)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.CommandRun, len(rows))
	for i, r := range rows {
		out[i] = entCommandRunToStore(r)
	}
	return out, nil
}

// ClaimCommandRunsForPoll implements store.CommandRequestStore.
func (c *CompositeStore) ClaimCommandRunsForPoll(ctx context.Context, now time.Time, claimTTL time.Duration, limit int, excludeRunIDs []string) ([]*store.CommandRun, error) {
	nowUTC := now.UTC().Truncate(time.Microsecond)
	if nowUTC.IsZero() {
		nowUTC = time.Now().UTC().Truncate(time.Microsecond)
	}
	cutoff := nowUTC
	if claimTTL > 0 {
		cutoff = nowUTC.Add(-claimTTL)
	}
	excludeSet := make(map[uuid.UUID]struct{}, len(excludeRunIDs))
	var excludeUIDs []uuid.UUID
	for _, id := range excludeRunIDs {
		if uid, err := uuid.Parse(id); err == nil {
			if _, dup := excludeSet[uid]; !dup {
				excludeSet[uid] = struct{}{}
				excludeUIDs = append(excludeUIDs, uid)
			}
		}
	}
	lim := clampLimit(limit)

	return withMessageActionRetry(ctx, c, func(tx *CompositeStore) ([]*store.CommandRun, error) {
		preds := []predicate.CommandRun{
			commandrun.FinishedAtIsNil(),
			commandrun.StatusIn(commandrun.StatusDispatching, commandrun.StatusRunning),
			commandrun.LeaseUntilGTE(nowUTC),
			commandrun.Or(
				commandrun.PollClaimedAtIsNil(),
				commandrun.PollClaimedAtLTE(cutoff),
			),
		}
		if len(excludeUIDs) > 0 {
			preds = append(preds, commandrun.IDNotIn(excludeUIDs...))
		}
		q := tx.client.CommandRun.Query().
			Where(preds...).
			Order(
				commandrun.ByPollClaimedAt(sql.OrderAsc(), sql.OrderNullsFirst()),
				commandrun.ByCreatedAt(sql.OrderAsc()),
				commandrun.ByID(sql.OrderAsc()),
			).
			Limit(lim)
		if tx.isPostgres() {
			q = q.ForUpdate(sql.WithLockAction(sql.SkipLocked))
		}
		candidates, err := q.All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		if len(candidates) == 0 {
			return nil, nil
		}
		ids := make([]uuid.UUID, len(candidates))
		for i, r := range candidates {
			ids[i] = r.ID
		}
		updPreds := append([]predicate.CommandRun{commandrun.IDIn(ids...)}, preds...)
		n, err := tx.client.CommandRun.Update().
			Where(updPreds...).
			SetPollClaimedAt(nowUTC).
			Save(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		if n == 0 {
			return nil, nil
		}
		claimed, err := tx.client.CommandRun.Query().
			Where(
				commandrun.IDIn(ids...),
				commandrun.FinishedAtIsNil(),
				commandrun.PollClaimedAtEQ(nowUTC),
			).
			Order(commandrun.ByCreatedAt(sql.OrderAsc()), commandrun.ByID(sql.OrderAsc())).
			All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		out := make([]*store.CommandRun, len(claimed))
		for i, r := range claimed {
			out[i] = entCommandRunToStore(r)
		}
		return out, nil
	})
}

// ListCommandRunsPendingOutcomeDelivery implements store.CommandRequestStore.
func (c *CompositeStore) ListCommandRunsPendingOutcomeDelivery(ctx context.Context, finishedSince, staleClaimBefore time.Time, limit int) ([]*store.CommandRun, error) {
	claimPred := commandrun.OutcomeDeliveredAtIsNil()
	if !staleClaimBefore.IsZero() {
		staleTS := staleClaimBefore.UTC().Truncate(time.Microsecond)
		claimPred = commandrun.Or(
			commandrun.OutcomeDeliveredAtIsNil(),
			commandrun.And(
				commandrun.OutcomeDeliveredAtLTE(staleTS),
				commandrun.OutcomeMessageIDIsNil(),
			),
		)
	}
	rows, err := c.client.CommandRun.Query().
		Where(
			commandrun.FinishedAtNotNil(),
			commandrun.FinishedAtGTE(finishedSince.UTC().Truncate(time.Microsecond)),
			claimPred,
		).
		Order(commandrun.ByFinishedAt(), commandrun.ByID()).
		Limit(clampLimit(limit)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.CommandRun, len(rows))
	for i, r := range rows {
		out[i] = entCommandRunToStore(r)
	}
	return out, nil
}

// UpdateCommandRunLastPolled implements store.CommandRequestStore.
func (c *CompositeStore) UpdateCommandRunLastPolled(ctx context.Context, runID string, polledAt time.Time) error {
	uid, err := parseUUID(runID)
	if err != nil {
		return err
	}
	ts := polledAt.UTC().Truncate(time.Microsecond)
	if ts.IsZero() {
		ts = time.Now().UTC().Truncate(time.Microsecond)
	}
	_, err = c.client.CommandRun.Update().
		Where(commandrun.IDEQ(uid), commandrun.FinishedAtIsNil()).
		SetLastPolledAt(ts).
		Save(ctx)
	return mapError(err)
}

// GetActionRunOutput implements store.CommandRequestStore.
func (c *CompositeStore) GetActionRunOutput(ctx context.Context, runID string) ([]byte, error) {
	uid, err := parseUUID(runID)
	if err != nil {
		return nil, err
	}
	row, err := c.client.ExecRunOutput.Query().
		Where(
			execrunoutput.IDEQ(uid),
			execrunoutput.OwnerKindEQ(execrunoutput.OwnerKindActionRun),
		).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return row.Content, nil
}

// HasActionRunOutput implements store.CommandRequestStore.
func (c *CompositeStore) HasActionRunOutput(ctx context.Context, runID string) (bool, error) {
	uid, err := parseUUID(runID)
	if err != nil {
		return false, err
	}
	exists, err := c.client.ExecRunOutput.Query().
		Where(
			execrunoutput.IDEQ(uid),
			execrunoutput.OwnerKindEQ(execrunoutput.OwnerKindActionRun),
		).
		Exist(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return exists, nil
}

// HasActionRunOutputs implements store.CommandRequestStore.
func (c *CompositeStore) HasActionRunOutputs(ctx context.Context, runIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(runIDs))
	if len(runIDs) == 0 {
		return out, nil
	}
	uids := make([]uuid.UUID, 0, len(runIDs))
	seen := make(map[uuid.UUID]struct{}, len(runIDs))
	for _, id := range runIDs {
		u, err := uuid.Parse(strings.TrimSpace(id))
		if err != nil {
			continue
		}
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		uids = append(uids, u)
	}
	if len(uids) == 0 {
		return out, nil
	}
	present, err := c.client.ExecRunOutput.Query().
		Where(
			execrunoutput.IDIn(uids...),
			execrunoutput.OwnerKindEQ(execrunoutput.OwnerKindActionRun),
		).
		IDs(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	for _, u := range present {
		out[u.String()] = true
	}
	return out, nil
}

// PurgeOldActionRunOutputs implements store.CommandRequestStore.
func (c *CompositeStore) PurgeOldActionRunOutputs(ctx context.Context, olderThan time.Time) (int, error) {
	cutoff := olderThan.UTC().Truncate(time.Microsecond)
	n, err := c.client.ExecRunOutput.Delete().
		Where(
			execrunoutput.OwnerKindEQ(execrunoutput.OwnerKindActionRun),
			execrunoutput.CreatedAtLT(cutoff),
		).
		Exec(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

func entCommandRequestToStore(e *ent.CommandRequest) *store.CommandRequest {
	r := &store.CommandRequest{
		ID:                   e.ID.String(),
		ProjectID:            e.ProjectID.String(),
		AgentID:              e.AgentID.String(),
		AgentName:            e.AgentName,
		Key:                  e.Key,
		KeyNorm:              e.KeyNorm,
		Version:              e.Version,
		Description:          e.Description,
		Script:               e.Script,
		Workdir:              e.Workdir,
		TimeoutS:             e.TimeoutS,
		ShareOutputRequested: e.ShareOutputRequested,
		CommandSHA256:        e.CommandSha256,
		ApproverUserID:       e.ApproverUserID,
		IntroLength:          e.IntroLength,
		Status:               string(e.Status),
		ReplacedAt:           e.ReplacedAt,
		ResolvedAt:           e.ResolvedAt,
		CreatedAt:            e.CreatedAt,
		ExpiresAt:            e.ExpiresAt,
	}
	if e.MessageID != nil {
		r.MessageID = e.MessageID.String()
	}
	if e.ConversationID != nil {
		r.ConversationID = e.ConversationID.String()
	}
	if e.ChatKey != nil {
		r.ChatKey = *e.ChatKey
	}
	if e.ReplacedByID != nil {
		r.ReplacedByID = e.ReplacedByID.String()
	}
	if e.WithdrawnReason != nil {
		r.WithdrawnReason = *e.WithdrawnReason
	}
	if e.WithdrawnBy != nil {
		r.WithdrawnBy = *e.WithdrawnBy
	}
	if e.DismissReason != nil {
		r.DismissReason = *e.DismissReason
	}
	if e.ResolvedByUserID != nil {
		r.ResolvedByUserID = *e.ResolvedByUserID
	}
	if e.ResolvedByName != nil {
		r.ResolvedByName = *e.ResolvedByName
	}
	if e.ResolvedByEmail != nil {
		r.ResolvedByEmail = *e.ResolvedByEmail
	}
	if e.Surface != nil {
		r.Surface = string(*e.Surface)
	}
	if e.IdempotencyKey != nil {
		r.IdempotencyKey = *e.IdempotencyKey
	}
	return r
}

func entCommandRunToStore(e *ent.CommandRun) *store.CommandRun {
	r := &store.CommandRun{
		ID:                 e.ID.String(),
		RequestID:          e.RequestID.String(),
		CommandSHA256:      e.CommandSha256,
		ScriptLen:          e.ScriptLen,
		ApproverUserID:     e.ApproverUserID,
		ExecAgentID:        e.ExecAgentID.String(),
		ExecProjectID:      e.ExecProjectID.String(),
		LaunchID:           e.LaunchID,
		Status:             string(e.Status),
		Reason:             e.Reason,
		Wake:               string(e.Wake),
		ShareOutput:        e.ShareOutput,
		TimeoutS:           e.TimeoutS,
		CaptureMode:        e.CaptureMode,
		Workdir:            e.Workdir,
		ExitCode:           e.ExitCode,
		DurationMS:         e.DurationMs,
		OutputBytes:        e.OutputBytes,
		OutputStatus:       e.OutputStatus,
		NotAfter:           e.NotAfter,
		LeaseUntil:         e.LeaseUntil,
		LastPolledAt:       e.LastPolledAt,
		PollClaimedAt:      e.PollClaimedAt,
		CreatedAt:          e.CreatedAt,
		DispatchedAt:       e.DispatchedAt,
		FinishedAt:         e.FinishedAt,
		OutcomeDeliveredAt: e.OutcomeDeliveredAt,
	}
	if e.OutcomeMessageID != nil {
		r.OutcomeMessageID = e.OutcomeMessageID.String()
	}
	return r
}
