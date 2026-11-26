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
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/secretrequest"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// CreateSecretRequestPrompt implements store.SecretRequestStore.
func (c *CompositeStore) CreateSecretRequestPrompt(ctx context.Context, in *store.SecretRequestPromptInput) (*store.SecretRequestPromptResult, error) {
	if err := validateSecretRequestPromptInput(in); err != nil {
		return nil, err
	}
	return insertNextActionVersion(ctx, c, func(tx *CompositeStore) (*store.SecretRequestPromptResult, error) {
		return tx.createSecretRequestPromptTx(ctx, in)
	})
}

func validateSecretRequestPromptInput(in *store.SecretRequestPromptInput) error {
	if in == nil || in.Message == nil || in.Message.ID == "" || in.Message.ProjectID == "" {
		return fmt.Errorf("%w: a secret request prompt needs a message with an ID and a project", store.ErrInvalidInput)
	}
	for _, id := range []string{in.Message.ID, in.Message.ProjectID, in.AgentID} {
		if _, err := parseUUID(id); err != nil {
			return err
		}
	}
	if len(in.Requests) == 0 || len(in.Requests) > messages.MaxSecretRequestsPerMessage {
		return fmt.Errorf("%w: a secret request prompt carries 1 to %d requests", store.ErrInvalidInput, messages.MaxSecretRequestsPerMessage)
	}
	seen := make(map[string]bool, len(in.Requests))
	for i, req := range in.Requests {
		if !messages.ValidSecretRequestKey(req.Key) {
			return fmt.Errorf("%w: secret request %d has an invalid key", store.ErrInvalidInput, i)
		}
		norm := messages.SecretRequestKeyNorm(req.Key)
		if seen[norm] {
			return fmt.Errorf("%w: secret request key %q repeats in the message", store.ErrInvalidInput, req.Key)
		}
		seen[norm] = true
	}
	return nil
}

type secretRequestKeyState struct {
	maxVersion int
	head       *ent.SecretRequest
}

func (c *CompositeStore) createSecretRequestPromptTx(ctx context.Context, in *store.SecretRequestPromptInput) (*store.SecretRequestPromptResult, error) {
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
		ttl = msgaction.SecretRequestTTL
	}
	expiresAt := now.Add(ttl)

	// Load latest version per key_norm in deterministic sorted order.
	norms := make([]string, len(in.Requests))
	for i, req := range in.Requests {
		norms[i] = messages.SecretRequestKeyNorm(req.Key)
	}
	sortedNorms := append([]string(nil), norms...)
	sort.Strings(sortedNorms)

	q := client.SecretRequest.Query().
		Where(
			secretrequest.AgentIDEQ(agentID),
			secretrequest.KeyNormIn(sortedNorms...),
		).
		Order(ent.Asc(secretrequest.FieldKeyNorm), ent.Asc(secretrequest.FieldVersion))
	if c.isPostgres() {
		q = q.ForUpdate()
	}
	existingRows, err := q.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	states := make(map[string]*secretRequestKeyState, len(sortedNorms))
	for _, row := range existingRows {
		st, ok := states[row.KeyNorm]
		if !ok {
			st = &secretRequestKeyState{}
			states[row.KeyNorm] = st
		}
		if row.Version >= st.maxVersion {
			st.maxVersion = row.Version
			st.head = row
		}
	}

	plans := make([]msgaction.RepostPlan, len(in.Requests))
	var pendingConflicts []store.PendingSecretRequestKey
	var submittingConflicts []store.PendingSecretRequestKey
	for i := range in.Requests {
		norm := norms[i]
		st := states[norm]
		var head *msgaction.Head
		if st != nil && st.head != nil {
			h := &msgaction.Head{
				RowID:   st.head.ID.String(),
				Key:     st.head.Key,
				Version: st.head.Version,
				Status:  string(st.head.Status),
			}
			// Treat an expired-but-unswept pending row as expired for repost planning.
			if st.head.Status == secretrequest.StatusPending && !st.head.ExpiresAt.After(now) {
				h.Status = store.SecretRequestStatusExpired
			}
			if st.head.ReplacedByID != nil {
				h.ReplacedBy = st.head.ReplacedByID.String()
			}
			if st.head.ResolvedAt != nil {
				t := st.head.ResolvedAt.UTC()
				h.ResolvedAt = &t
			}
			head = h
		}

		// In-flight fulfill always wins over --update (§4.6 writers table).
		if in.Update && head != nil && head.Status == store.SecretRequestStatusFulfilling {
			submittingConflicts = append(submittingConflicts, store.PendingSecretRequestKey{
				Key:       head.Key,
				Version:   head.Version,
				MessageID: st.head.MessageID.String(),
				RowID:     head.RowID,
				Status:    head.Status,
			})
			continue
		}

		plan, planErr := msgaction.PlanRepost(msgaction.SecretRequestKindSpec, head, in.Update)
		if planErr != nil {
			if st != nil && st.head != nil {
				pendingConflicts = append(pendingConflicts, store.PendingSecretRequestKey{
					Key:       st.head.Key,
					Version:   st.head.Version,
					MessageID: st.head.MessageID.String(),
					RowID:     st.head.ID.String(),
					Status:    string(st.head.Status),
				})
				continue
			}
			return nil, planErr
		}
		plans[i] = plan
	}
	if len(submittingConflicts) > 0 {
		return nil, &store.SecretRequestSubmittingError{Keys: submittingConflicts}
	}
	if len(pendingConflicts) > 0 {
		return nil, &store.SecretRequestKeyPendingError{Existing: pendingConflicts}
	}

	// Render Markdown fallback and insert the secret-request message in the same tx.
	fallbackItems := make([]messages.SecretRequestFallbackItem, len(in.Requests))
	for i, req := range in.Requests {
		fallbackItems[i] = messages.SecretRequestFallbackItem{
			Key:         req.Key,
			Version:     plans[i].NextVersion,
			Description: req.Description,
			SecretType:  req.SecretType,
			Target:      req.Target,
		}
	}
	intro := strings.TrimSpace(in.Intro)
	introLen := len(intro)

	msg := *in.Message
	msg.Type = messages.TypeSecretRequest
	msg.Msg = messages.RenderSecretRequestFallback(intro, in.AgentName, in.ProjectName, fallbackItems, in.StandaloneURL)
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

	created := make([]store.CreatedSecretRequest, len(in.Requests))
	for i, req := range in.Requests {
		plan := plans[i]
		norm := norms[i]
		rowID := uuid.New()
		builder := client.SecretRequest.Create().
			SetID(rowID).
			SetProjectID(projectID).
			SetAgentID(agentID).
			SetAgentName(in.AgentName).
			SetKey(req.Key).
			SetKeyNorm(norm).
			SetVersion(plan.NextVersion).
			SetDescription(req.Description).
			SetSecretType(secretrequest.SecretType(req.SecretType)).
			SetMessageID(msgID).
			SetPosition(i).
			SetStatus(secretrequest.StatusPending).
			SetCreatedAt(now).
			SetExpiresAt(expiresAt)
		if i == 0 && introLen > 0 {
			builder.SetIntroLength(introLen)
		}
		if req.Target != "" {
			builder.SetTarget(req.Target)
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

		var replacedRow *store.SecretRequest
		if plan.ReplacesRowID != "" {
			prevUID, _ := parseUUID(plan.ReplacesRowID)
			if plan.MarkReplaced {
				ok, _, cerr := casActionRows[*ent.SecretRequest](
					ctx,
					1,
					func(ctx context.Context) (int, error) {
						return client.SecretRequest.Update().
							Where(
								secretrequest.IDEQ(prevUID),
								secretrequest.StatusEQ(secretrequest.StatusPending),
								secretrequest.ReplacedByIDIsNil(),
							).
							SetStatus(secretrequest.StatusReplaced).
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
				// Terminal prior version: link replaced_by_id without changing terminal status.
				_, uerr := client.SecretRequest.Update().
					Where(
						secretrequest.IDEQ(prevUID),
						secretrequest.ReplacedByIDIsNil(),
					).
					SetReplacedByID(rowID).
					SetReplacedAt(now).
					Save(ctx)
				if uerr != nil {
					return nil, mapError(uerr)
				}
			}
			if st := states[norm]; st != nil && st.head != nil {
				prev := entSecretRequestToStore(st.head)
				if plan.MarkReplaced {
					prev.Status = store.SecretRequestStatusReplaced
					prev.ResolvedAt = &now
				}
				prev.ReplacedByID = rowID.String()
				prev.ReplacedAt = &now
				replacedRow = prev
			}
		}

		created[i] = store.CreatedSecretRequest{
			Row:      entSecretRequestToStore(inserted),
			Replaced: replacedRow,
		}
	}

	in.Message.Type = msg.Type
	in.Message.Msg = msg.Msg
	return &store.SecretRequestPromptResult{
		Message:  &msg,
		Requests: created,
	}, nil
}

// GetSecretRequest implements store.SecretRequestStore.
func (c *CompositeStore) GetSecretRequest(ctx context.Context, id string) (*store.SecretRequest, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return nil, err
	}
	row, err := c.client.SecretRequest.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entSecretRequestToStore(row), nil
}

// GetSecretRequestsByMessageID implements store.SecretRequestStore.
func (c *CompositeStore) GetSecretRequestsByMessageID(ctx context.Context, messageID string) ([]*store.SecretRequest, error) {
	uid, err := parseUUID(messageID)
	if err != nil {
		return nil, err
	}
	rows, err := c.client.SecretRequest.Query().
		Where(secretrequest.MessageIDEQ(uid)).
		Order(ent.Asc(secretrequest.FieldPosition)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.SecretRequest, len(rows))
	for i, r := range rows {
		out[i] = entSecretRequestToStore(r)
	}
	return out, nil
}

// GetSecretRequestsByIDs implements store.SecretRequestStore.
func (c *CompositeStore) GetSecretRequestsByIDs(ctx context.Context, ids []string) (map[string]*store.SecretRequest, error) {
	out := make(map[string]*store.SecretRequest, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	uids := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		uid, err := parseUUID(id)
		if err != nil {
			continue
		}
		uids = append(uids, uid)
	}
	if len(uids) == 0 {
		return out, nil
	}
	rows, err := c.client.SecretRequest.Query().
		Where(secretrequest.IDIn(uids...)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	for _, r := range rows {
		out[r.ID.String()] = entSecretRequestToStore(r)
	}
	return out, nil
}

// ClaimSecretRequestsForFulfill implements store.SecretRequestStore.
func (c *CompositeStore) ClaimSecretRequestsForFulfill(ctx context.Context, in *store.ClaimSecretRequestsInput) (*store.ClaimSecretRequestsResult, error) {
	if in == nil || in.MessageID == "" || in.UserID == "" {
		return nil, fmt.Errorf("%w: messageID and userID are required", store.ErrInvalidInput)
	}
	msgUID, err := parseUUID(in.MessageID)
	if err != nil {
		return nil, err
	}
	now := in.Now.UTC().Truncate(time.Microsecond)
	if now.IsZero() {
		now = time.Now().UTC().Truncate(time.Microsecond)
	}

	return withMessageActionRetry(ctx, c, func(tx *CompositeStore) (*store.ClaimSecretRequestsResult, error) {
		q := tx.client.SecretRequest.Query().
			Where(secretrequest.MessageIDEQ(msgUID)).
			Order(ent.Asc(secretrequest.FieldPosition))
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

		// Idempotent replay: same user already fulfilled with this idempotencyKey.
		if in.IdempotencyKey != "" {
			matched := false
			for _, r := range rows {
				if r.Status == secretrequest.StatusFulfilled &&
					r.IdempotencyKey != nil && *r.IdempotencyKey == in.IdempotencyKey &&
					r.ResolvedByUserID != nil && *r.ResolvedByUserID == in.UserID {
					matched = true
					break
				}
			}
			if matched {
				storeRows := make([]*store.SecretRequest, len(rows))
				for i, r := range rows {
					storeRows[i] = entSecretRequestToStore(r)
				}
				return &store.ClaimSecretRequestsResult{
					Replayed: true,
					Rows:     storeRows,
				}, nil
			}
		}

		targetSet := make(map[uuid.UUID]bool, len(in.RowIDs))
		for _, id := range in.RowIDs {
			u, perr := parseUUID(id)
			if perr != nil {
				return nil, perr
			}
			targetSet[u] = true
		}

		var claimUIDs []uuid.UUID
		for _, r := range rows {
			if len(targetSet) > 0 && !targetSet[r.ID] {
				continue
			}
			if r.Status != secretrequest.StatusPending {
				return &store.ClaimSecretRequestsResult{
					Conflict: entSecretRequestToStore(r),
				}, nil
			}
			if !r.ExpiresAt.After(now) {
				exp := entSecretRequestToStore(r)
				exp.Status = store.SecretRequestStatusExpired
				return &store.ClaimSecretRequestsResult{
					Conflict: exp,
				}, nil
			}
			claimUIDs = append(claimUIDs, r.ID)
		}
		if len(targetSet) > 0 && len(claimUIDs) != len(targetSet) {
			return nil, fmt.Errorf("%w: one or more rowIds do not belong to this secret request card", store.ErrInvalidInput)
		}
		if len(claimUIDs) == 0 {
			return &store.ClaimSecretRequestsResult{
				Conflict: entSecretRequestToStore(rows[0]),
			}, nil
		}

		ok, reloaded, cerr := casActionRows[*ent.SecretRequest](
			ctx,
			len(claimUIDs),
			func(ctx context.Context) (int, error) {
				upd := tx.client.SecretRequest.Update().
					Where(
						secretrequest.MessageIDEQ(msgUID),
						secretrequest.IDIn(claimUIDs...),
						secretrequest.StatusEQ(secretrequest.StatusPending),
						secretrequest.ExpiresAtGT(now),
					).
					SetStatus(secretrequest.StatusFulfilling).
					SetClaimedAt(now).
					SetResolvedByUserID(in.UserID).
					SetResolvedByName(in.UserName).
					SetResolvedByEmail(in.UserEmail)
				if in.Surface != "" {
					upd.SetSurface(secretrequest.Surface(in.Surface))
				}
				if in.IdempotencyKey != "" {
					upd.SetIdempotencyKey(in.IdempotencyKey)
				}
				return upd.Save(ctx)
			},
			func(ctx context.Context) ([]*ent.SecretRequest, error) {
				return tx.client.SecretRequest.Query().
					Where(secretrequest.IDIn(claimUIDs...)).
					Order(ent.Asc(secretrequest.FieldPosition)).
					All(ctx)
			},
		)
		if cerr != nil {
			return nil, cerr
		}
		if !ok {
			for _, r := range reloaded {
				if r.Status != secretrequest.StatusPending {
					return &store.ClaimSecretRequestsResult{Conflict: entSecretRequestToStore(r)}, nil
				}
				if !r.ExpiresAt.After(now) {
					exp := entSecretRequestToStore(r)
					exp.Status = store.SecretRequestStatusExpired
					return &store.ClaimSecretRequestsResult{Conflict: exp}, nil
				}
			}
			return nil, errMessageActionRace
		}

		updated, err := tx.client.SecretRequest.Query().
			Where(secretrequest.IDIn(claimUIDs...)).
			Order(ent.Asc(secretrequest.FieldPosition)).
			All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		claimed := make([]*store.SecretRequest, len(updated))
		for i, r := range updated {
			claimed[i] = entSecretRequestToStore(r)
		}
		return &store.ClaimSecretRequestsResult{
			Claimed:   claimed,
			ClaimedAt: now,
		}, nil
	})
}

// errFinishFulfillConflict aborts the FinishSecretRequestFulfill transaction so
// earlier rows updated in the same loop are rolled back when a later row's CAS
// misses (N1).
type errFinishFulfillConflict struct {
	conflict *store.SecretRequest
}

func (e *errFinishFulfillConflict) Error() string { return "finish fulfill claim conflict" }

// FinishSecretRequestFulfill implements store.SecretRequestStore.
func (c *CompositeStore) FinishSecretRequestFulfill(ctx context.Context, in *store.FinishSecretRequestFulfillInput) (*store.FinishSecretRequestFulfillResult, error) {
	if in == nil || in.MessageID == "" || in.UserID == "" || len(in.RowIDs) == 0 {
		return nil, fmt.Errorf("%w: messageID, userID, and rowIDs are required", store.ErrInvalidInput)
	}
	msgUID, err := parseUUID(in.MessageID)
	if err != nil {
		return nil, err
	}
	rowUIDs := make([]uuid.UUID, len(in.RowIDs))
	for i, id := range in.RowIDs {
		u, perr := parseUUID(id)
		if perr != nil {
			return nil, perr
		}
		rowUIDs[i] = u
	}
	now := in.Now.UTC().Truncate(time.Microsecond)
	if now.IsZero() {
		now = time.Now().UTC().Truncate(time.Microsecond)
	}
	claimedAt := in.ClaimedAt.UTC().Truncate(time.Microsecond)

	res, err := withMessageActionRetry(ctx, c, func(tx *CompositeStore) (*store.FinishSecretRequestFulfillResult, error) {
		var respMsgID *uuid.UUID
		var respMsgCopy *store.Message
		if in.ResponseMessage != nil {
			m := *in.ResponseMessage
			if m.ID == "" {
				m.ID = uuid.New().String()
			}
			uid, perr := parseUUID(m.ID)
			if perr != nil {
				return nil, perr
			}
			respMsgID = &uid
			respMsgCopy = &m
		}

		// CAS each claimed row individually so each records its own secret_version.
		for i, uid := range rowUIDs {
			rowIDStr := in.RowIDs[i]
			ok, reloaded, cerr := casActionRows[*ent.SecretRequest](
				ctx,
				1,
				func(ctx context.Context) (int, error) {
					upd := tx.client.SecretRequest.Update().
						Where(
							secretrequest.MessageIDEQ(msgUID),
							secretrequest.IDEQ(uid),
							secretrequest.StatusEQ(secretrequest.StatusFulfilling),
							secretrequest.ResolvedByUserIDEQ(in.UserID),
							secretrequest.ClaimedAtEQ(claimedAt),
						).
						SetStatus(secretrequest.StatusFulfilled).
						SetResolvedAt(now)
					if in.Surface != "" {
						upd.SetSurface(secretrequest.Surface(in.Surface))
					}
					if in.IdempotencyKey != "" {
						upd.SetIdempotencyKey(in.IdempotencyKey)
					}
					if respMsgID != nil {
						upd.SetResponseMessageID(*respMsgID)
					}
					if ver, ok := in.SecretVersions[rowIDStr]; ok && ver > 0 {
						upd.SetSecretVersion(ver)
					}
					return upd.Save(ctx)
				},
				func(ctx context.Context) ([]*ent.SecretRequest, error) {
					return tx.client.SecretRequest.Query().
						Where(secretrequest.IDEQ(uid)).
						All(ctx)
				},
			)
			if cerr != nil {
				return nil, cerr
			}
			if !ok {
				var conflict *store.SecretRequest
				if len(reloaded) > 0 {
					conflict = entSecretRequestToStore(reloaded[0])
				}
				return nil, &errFinishFulfillConflict{conflict: conflict}
			}
		}

		if respMsgCopy != nil {
			if err := tx.CreateMessage(ctx, respMsgCopy); err != nil {
				return nil, err
			}
		}

		updated, err := tx.client.SecretRequest.Query().
			Where(secretrequest.MessageIDEQ(msgUID)).
			Order(ent.Asc(secretrequest.FieldPosition)).
			All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		outRows := make([]*store.SecretRequest, len(updated))
		for i, r := range updated {
			outRows[i] = entSecretRequestToStore(r)
		}
		return &store.FinishSecretRequestFulfillResult{
			Rows:            outRows,
			ResponseMessage: respMsgCopy,
		}, nil
	})
	var conflictErr *errFinishFulfillConflict
	if errors.As(err, &conflictErr) {
		return &store.FinishSecretRequestFulfillResult{Conflict: conflictErr.conflict}, nil
	}
	return res, err
}

// RollbackSecretRequestClaim implements store.SecretRequestStore.
func (c *CompositeStore) RollbackSecretRequestClaim(ctx context.Context, in *store.RollbackSecretRequestClaimInput) (int, error) {
	if in == nil || in.MessageID == "" || in.UserID == "" || len(in.RowIDs) == 0 {
		return 0, nil
	}
	msgUID, err := parseUUID(in.MessageID)
	if err != nil {
		return 0, err
	}
	rowUIDs := make([]uuid.UUID, 0, len(in.RowIDs))
	for _, id := range in.RowIDs {
		u, perr := parseUUID(id)
		if perr != nil {
			return 0, perr
		}
		rowUIDs = append(rowUIDs, u)
	}
	claimedAt := in.ClaimedAt.UTC().Truncate(time.Microsecond)

	n, err := c.client.SecretRequest.Update().
		Where(
			secretrequest.MessageIDEQ(msgUID),
			secretrequest.IDIn(rowUIDs...),
			secretrequest.StatusEQ(secretrequest.StatusFulfilling),
			secretrequest.ResolvedByUserIDEQ(in.UserID),
			secretrequest.ClaimedAtEQ(claimedAt),
		).
		SetStatus(secretrequest.StatusPending).
		ClearClaimedAt().
		ClearResolvedByUserID().
		ClearResolvedByName().
		ClearResolvedByEmail().
		ClearSurface().
		ClearIdempotencyKey().
		Save(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

// DismissSecretRequests implements store.SecretRequestStore.
func (c *CompositeStore) DismissSecretRequests(ctx context.Context, in *store.DismissSecretRequestsInput) (*store.DismissSecretRequestsResult, error) {
	if in == nil || in.MessageID == "" || in.UserID == "" {
		return nil, fmt.Errorf("%w: messageID and userID are required", store.ErrInvalidInput)
	}
	msgUID, err := parseUUID(in.MessageID)
	if err != nil {
		return nil, err
	}
	now := in.Now.UTC().Truncate(time.Microsecond)
	if now.IsZero() {
		now = time.Now().UTC().Truncate(time.Microsecond)
	}

	return withMessageActionRetry(ctx, c, func(tx *CompositeStore) (*store.DismissSecretRequestsResult, error) {
		q := tx.client.SecretRequest.Query().
			Where(secretrequest.MessageIDEQ(msgUID)).
			Order(ent.Asc(secretrequest.FieldPosition))
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

		// Determine targeted rows.
		var targeted []*ent.SecretRequest
		if len(in.RowIDs) > 0 {
			byID := make(map[string]*ent.SecretRequest, len(rows))
			for _, r := range rows {
				byID[r.ID.String()] = r
			}
			seen := make(map[string]bool, len(in.RowIDs))
			for _, id := range in.RowIDs {
				if seen[id] {
					continue
				}
				seen[id] = true
				r, ok := byID[id]
				if !ok {
					return nil, fmt.Errorf("%w: rowId %q does not belong to this secret request card", store.ErrInvalidInput, id)
				}
				targeted = append(targeted, r)
			}
		} else {
			targeted = rows
		}

		// Check idempotency replay by the same user.
		if in.IdempotencyKey != "" {
			var replayed []*store.SecretRequest
			for _, r := range targeted {
				if r.Status == secretrequest.StatusDismissed &&
					r.IdempotencyKey != nil && *r.IdempotencyKey == in.IdempotencyKey &&
					r.ResolvedByUserID != nil && *r.ResolvedByUserID == in.UserID {
					replayed = append(replayed, entSecretRequestToStore(r))
				}
			}
			if len(replayed) > 0 {
				return &store.DismissSecretRequestsResult{
					AlreadyDismissed: replayed,
					Replayed:         true,
				}, nil
			}
		}

		var toDismissUIDs []uuid.UUID
		var alreadyDismissed []*store.SecretRequest
		for _, r := range targeted {
			switch r.Status {
			case secretrequest.StatusFulfilling:
				return &store.DismissSecretRequestsResult{
					Submitting: entSecretRequestToStore(r),
				}, nil
			case secretrequest.StatusPending:
				if r.ExpiresAt.After(now) {
					toDismissUIDs = append(toDismissUIDs, r.ID)
				}
			case secretrequest.StatusDismissed:
				alreadyDismissed = append(alreadyDismissed, entSecretRequestToStore(r))
			}
		}

		if len(toDismissUIDs) == 0 {
			allReplaced := len(targeted) > 0
			for _, r := range targeted {
				if r.Status != secretrequest.StatusReplaced {
					allReplaced = false
					break
				}
			}
			var replacedRow *store.SecretRequest
			if allReplaced {
				replacedRow = entSecretRequestToStore(targeted[0])
			}
			return &store.DismissSecretRequestsResult{
				AlreadyDismissed: alreadyDismissed,
				Replaced:         replacedRow,
			}, nil
		}

		var respMsgID *uuid.UUID
		var respMsgCopy *store.Message
		if in.ResponseMessage != nil {
			m := *in.ResponseMessage
			if m.ID == "" {
				m.ID = uuid.New().String()
			}
			uid, perr := parseUUID(m.ID)
			if perr != nil {
				return nil, perr
			}
			respMsgID = &uid
			respMsgCopy = &m
		}

		ok, _, cerr := casActionRows[*ent.SecretRequest](
			ctx,
			len(toDismissUIDs),
			func(ctx context.Context) (int, error) {
				upd := tx.client.SecretRequest.Update().
					Where(
						secretrequest.MessageIDEQ(msgUID),
						secretrequest.IDIn(toDismissUIDs...),
						secretrequest.StatusEQ(secretrequest.StatusPending),
						secretrequest.ExpiresAtGT(now),
					).
					SetStatus(secretrequest.StatusDismissed).
					SetResolvedAt(now).
					SetResolvedByUserID(in.UserID).
					SetResolvedByName(in.UserName).
					SetResolvedByEmail(in.UserEmail)
				if in.Reason != "" {
					upd.SetDismissReason(in.Reason)
				}
				if in.Surface != "" {
					upd.SetSurface(secretrequest.Surface(in.Surface))
				}
				if in.IdempotencyKey != "" {
					upd.SetIdempotencyKey(in.IdempotencyKey)
				}
				if respMsgID != nil {
					upd.SetResponseMessageID(*respMsgID)
				}
				return upd.Save(ctx)
			},
			nil,
		)
		if cerr != nil {
			return nil, cerr
		}
		if !ok {
			return nil, errMessageActionRace
		}

		updated, err := tx.client.SecretRequest.Query().
			Where(secretrequest.IDIn(toDismissUIDs...)).
			Order(ent.Asc(secretrequest.FieldPosition)).
			All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		dismissed := make([]*store.SecretRequest, len(updated))
		for i, r := range updated {
			dismissed[i] = entSecretRequestToStore(r)
		}
		if respMsgCopy != nil {
			if in.FormatResponseMsg != nil {
				respMsgCopy.Msg = in.FormatResponseMsg(dismissed)
			}
			if err := tx.CreateMessage(ctx, respMsgCopy); err != nil {
				return nil, err
			}
		}
		return &store.DismissSecretRequestsResult{
			Dismissed:        dismissed,
			AlreadyDismissed: alreadyDismissed,
			ResponseMessage:  respMsgCopy,
		}, nil
	})
}

// WithdrawSecretRequests implements store.SecretRequestStore.
func (c *CompositeStore) WithdrawSecretRequests(ctx context.Context, in *store.WithdrawSecretRequestsInput) (*store.WithdrawSecretRequestsResult, error) {
	if in == nil || in.AgentID == "" || len(in.Keys) == 0 {
		return nil, fmt.Errorf("%w: agentID and at least one key are required", store.ErrInvalidInput)
	}
	agentUID, err := parseUUID(in.AgentID)
	if err != nil {
		return nil, err
	}
	now := in.Now.UTC().Truncate(time.Microsecond)
	if now.IsZero() {
		now = time.Now().UTC().Truncate(time.Microsecond)
	}

	return withMessageActionRetry(ctx, c, func(tx *CompositeStore) (*store.WithdrawSecretRequestsResult, error) {
		norms := make([]string, len(in.Keys))
		for i, k := range in.Keys {
			norms[i] = messages.SecretRequestKeyNorm(k)
		}
		sortedNorms := append([]string(nil), norms...)
		sort.Strings(sortedNorms)

		q := tx.client.SecretRequest.Query().
			Where(
				secretrequest.AgentIDEQ(agentUID),
				secretrequest.KeyNormIn(sortedNorms...),
			).
			Order(ent.Asc(secretrequest.FieldKeyNorm), ent.Asc(secretrequest.FieldVersion))
		if tx.isPostgres() {
			q = q.ForUpdate()
		}
		rows, err := q.All(ctx)
		if err != nil {
			return nil, mapError(err)
		}

		latest := make(map[string]*ent.SecretRequest, len(sortedNorms))
		for _, r := range rows {
			prev, ok := latest[r.KeyNorm]
			if !ok || r.Version > prev.Version {
				latest[r.KeyNorm] = r
			}
		}

		var failures []store.SecretRequestWithdrawFailure
		var toWithdraw []*ent.SecretRequest
		var alreadyWithdrawn []*store.SecretRequest
		seenWithdraw := make(map[uuid.UUID]bool)
		wholeSetMsgs := make(map[uuid.UUID]bool)

		for i, rawKey := range in.Keys {
			norm := norms[i]
			r, ok := latest[norm]
			if !ok {
				failures = append(failures, store.SecretRequestWithdrawFailure{
					Key:    rawKey,
					Reason: "not_found",
				})
				continue
			}
			switch r.Status {
			case secretrequest.StatusPending:
				if !r.ExpiresAt.After(now) {
					exp := entSecretRequestToStore(r)
					exp.Status = store.SecretRequestStatusExpired
					failures = append(failures, store.SecretRequestWithdrawFailure{
						Key:     rawKey,
						Reason:  msgaction.ConflictExpired,
						Request: exp,
					})
					continue
				}
				if !seenWithdraw[r.ID] {
					seenWithdraw[r.ID] = true
					toWithdraw = append(toWithdraw, r)
				}
				if in.WholeSet {
					wholeSetMsgs[r.MessageID] = true
				}
			case secretrequest.StatusWithdrawn:
				alreadyWithdrawn = append(alreadyWithdrawn, entSecretRequestToStore(r))
			case secretrequest.StatusFulfilled:
				failures = append(failures, store.SecretRequestWithdrawFailure{
					Key:     rawKey,
					Reason:  msgaction.ConflictAlreadyFulfilled,
					Request: entSecretRequestToStore(r),
				})
			case secretrequest.StatusDismissed:
				failures = append(failures, store.SecretRequestWithdrawFailure{
					Key:     rawKey,
					Reason:  msgaction.ConflictDismissed,
					Request: entSecretRequestToStore(r),
				})
			case secretrequest.StatusExpired:
				failures = append(failures, store.SecretRequestWithdrawFailure{
					Key:     rawKey,
					Reason:  msgaction.ConflictExpired,
					Request: entSecretRequestToStore(r),
				})
			case secretrequest.StatusFulfilling:
				failures = append(failures, store.SecretRequestWithdrawFailure{
					Key:     rawKey,
					Reason:  msgaction.ConflictSubmitting,
					Request: entSecretRequestToStore(r),
				})
			default:
				failures = append(failures, store.SecretRequestWithdrawFailure{
					Key:     rawKey,
					Reason:  string(r.Status),
					Request: entSecretRequestToStore(r),
				})
			}
		}

		if len(failures) > 0 {
			return nil, &store.WithdrawSecretRequestsError{Failures: failures}
		}

		// Expand --set to all other pending rows on the same card(s).
		if in.WholeSet && len(wholeSetMsgs) > 0 {
			msgUIDs := make([]uuid.UUID, 0, len(wholeSetMsgs))
			for m := range wholeSetMsgs {
				msgUIDs = append(msgUIDs, m)
			}
			sq := tx.client.SecretRequest.Query().
				Where(
					secretrequest.AgentIDEQ(agentUID),
					secretrequest.MessageIDIn(msgUIDs...),
					secretrequest.StatusEQ(secretrequest.StatusPending),
					secretrequest.ExpiresAtGT(now),
				).
				Order(ent.Asc(secretrequest.FieldPosition))
			if tx.isPostgres() {
				sq = sq.ForUpdate()
			}
			siblings, err := sq.All(ctx)
			if err != nil {
				return nil, mapError(err)
			}
			for _, s := range siblings {
				if !seenWithdraw[s.ID] {
					seenWithdraw[s.ID] = true
					toWithdraw = append(toWithdraw, s)
				}
			}
		}

		if len(toWithdraw) == 0 {
			return &store.WithdrawSecretRequestsResult{
				AlreadyWithdrawn: alreadyWithdrawn,
			}, nil
		}

		withdrawUIDs := make([]uuid.UUID, len(toWithdraw))
		for i, r := range toWithdraw {
			withdrawUIDs[i] = r.ID
		}

		ok, _, cerr := casActionRows[*ent.SecretRequest](
			ctx,
			len(withdrawUIDs),
			func(ctx context.Context) (int, error) {
				upd := tx.client.SecretRequest.Update().
					Where(
						secretrequest.IDIn(withdrawUIDs...),
						secretrequest.StatusEQ(secretrequest.StatusPending),
						secretrequest.ExpiresAtGT(now),
					).
					SetStatus(secretrequest.StatusWithdrawn).
					SetWithdrawnBy(store.SecretRequestWithdrawnByAgent).
					SetResolvedAt(now)
				if in.Reason != "" {
					upd.SetWithdrawnReason(in.Reason)
				}
				return upd.Save(ctx)
			},
			nil,
		)
		if cerr != nil {
			return nil, cerr
		}
		if !ok {
			return nil, errMessageActionRace
		}

		updated, err := tx.client.SecretRequest.Query().
			Where(secretrequest.IDIn(withdrawUIDs...)).
			Order(ent.Asc(secretrequest.FieldPosition)).
			All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		withdrawn := make([]*store.SecretRequest, len(updated))
		for i, r := range updated {
			withdrawn[i] = entSecretRequestToStore(r)
		}
		return &store.WithdrawSecretRequestsResult{
			Withdrawn:        withdrawn,
			AlreadyWithdrawn: alreadyWithdrawn,
		}, nil
	})
}

// ListAgentSecretRequests implements store.SecretRequestStore.
func (c *CompositeStore) ListAgentSecretRequests(ctx context.Context, opts *store.ListAgentSecretRequestsOptions) (*store.ListAgentSecretRequestsResult, error) {
	if opts == nil || opts.AgentID == "" {
		return nil, fmt.Errorf("%w: agentID is required", store.ErrInvalidInput)
	}
	agentUID, err := parseUUID(opts.AgentID)
	if err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = store.DefaultSecretRequestListLimit
	}
	if limit > store.MaxSecretRequestListLimit {
		limit = store.MaxSecretRequestListLimit
	}

	q := c.client.SecretRequest.Query().
		Where(secretrequest.AgentIDEQ(agentUID))

	switch opts.State {
	case "", store.SecretRequestListStatePending:
		q = q.Where(secretrequest.StatusIn(secretrequest.StatusPending, secretrequest.StatusFulfilling))
	case store.SecretRequestListStateFulfilled:
		q = q.Where(secretrequest.StatusEQ(secretrequest.StatusFulfilled))
	case store.SecretRequestListStateWithdrawn:
		q = q.Where(secretrequest.StatusEQ(secretrequest.StatusWithdrawn))
	case store.SecretRequestListStateDismissed:
		q = q.Where(secretrequest.StatusEQ(secretrequest.StatusDismissed))
	case store.SecretRequestListStateExpired:
		q = q.Where(secretrequest.StatusEQ(secretrequest.StatusExpired))
	case store.SecretRequestListStateAll:
		// no status filter
	default:
		return nil, fmt.Errorf("%w: invalid state %q", store.ErrInvalidInput, opts.State)
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
			secretrequest.Or(
				secretrequest.CreatedAtLT(ts),
				secretrequest.And(
					secretrequest.CreatedAtEQ(ts),
					secretrequest.IDLT(curUID),
				),
			),
		)
	}

	rows, err := q.Order(
		ent.Desc(secretrequest.FieldCreatedAt),
		ent.Desc(secretrequest.FieldID),
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

	items := make([]*store.SecretRequest, len(rows))
	for i, r := range rows {
		items[i] = entSecretRequestToStore(r)
	}
	return &store.ListAgentSecretRequestsResult{
		Items:      items,
		NextCursor: nextCursor,
	}, nil
}

func secretRequestViewerScopePredicate(projectIDs []string, viewerID string) (predicate.SecretRequest, bool, error) {
	projUIDs := make([]uuid.UUID, 0, len(projectIDs))
	for _, pid := range projectIDs {
		pid = strings.TrimSpace(pid)
		if pid == "" {
			continue
		}
		u, err := parseUUID(pid)
		if err != nil {
			return nil, false, err
		}
		projUIDs = append(projUIDs, u)
	}
	viewerID = strings.TrimSpace(viewerID)
	hasProjects := len(projUIDs) > 0
	hasViewer := viewerID != ""
	if !hasProjects && !hasViewer {
		return nil, false, nil
	}
	dmPred := secretrequest.And(
		secretrequest.ChatKeyHasPrefix("dm:agent:"),
		secretrequest.ChatKeyHasSuffix(":user:"+viewerID),
	)
	switch {
	case hasProjects && hasViewer:
		return secretrequest.Or(
			secretrequest.ProjectIDIn(projUIDs...),
			dmPred,
		), true, nil
	case hasProjects:
		return secretrequest.ProjectIDIn(projUIDs...), true, nil
	default:
		return dmPred, true, nil
	}
}

// ListOpenSecretRequests implements store.SecretRequestStore.
func (c *CompositeStore) ListOpenSecretRequests(ctx context.Context, projectIDs []string, viewerID string, now time.Time, limit int) ([]*store.SecretRequest, error) {
	scopePred, ok, err := secretRequestViewerScopePredicate(projectIDs, viewerID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []*store.SecretRequest{}, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC().Truncate(time.Microsecond)

	q := c.client.SecretRequest.Query().
		Where(
			secretrequest.StatusIn(secretrequest.StatusPending, secretrequest.StatusFulfilling),
			secretrequest.ExpiresAtGT(now),
			scopePred,
		).
		Order(
			ent.Desc(secretrequest.FieldCreatedAt),
			ent.Desc(secretrequest.FieldID),
		)
	if limit > 0 {
		q = q.Limit(limit)
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	items := make([]*store.SecretRequest, len(rows))
	for i, r := range rows {
		items[i] = entSecretRequestToStore(r)
	}
	return items, nil
}

// ListResolvedSecretRequests implements store.SecretRequestStore.
func (c *CompositeStore) ListResolvedSecretRequests(ctx context.Context, opts store.SecretRequestHistoryQuery) ([]*store.SecretRequest, error) {
	scopePred, ok, err := secretRequestViewerScopePredicate(opts.ProjectIDs, opts.ViewerID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []*store.SecretRequest{}, nil
	}

	q := c.client.SecretRequest.Query().
		Where(
			secretrequest.StatusIn(
				secretrequest.StatusFulfilled,
				secretrequest.StatusDismissed,
				secretrequest.StatusWithdrawn,
				secretrequest.StatusExpired,
			),
			secretrequest.ResolvedAtNotNil(),
			scopePred,
		)

	if pid := strings.TrimSpace(opts.FilterProjectID); pid != "" {
		pUID, perr := parseUUID(pid)
		if perr != nil {
			return []*store.SecretRequest{}, nil
		}
		q = q.Where(secretrequest.ProjectIDEQ(pUID))
	}
	if aid := strings.TrimSpace(opts.FilterAgentID); aid != "" {
		aUID, aerr := parseUUID(aid)
		if aerr != nil {
			return []*store.SecretRequest{}, nil
		}
		q = q.Where(secretrequest.AgentIDEQ(aUID))
	}
	if opts.CursorTime != nil && opts.CursorID != "" {
		curUID, uerr := parseUUID(opts.CursorID)
		if uerr != nil {
			return nil, fmt.Errorf("%w: invalid cursor id", store.ErrInvalidInput)
		}
		curTime := opts.CursorTime.UTC()
		q = q.Where(
			secretrequest.Or(
				secretrequest.ResolvedAtLT(curTime),
				secretrequest.And(
					secretrequest.ResolvedAtEQ(curTime),
					secretrequest.IDLT(curUID),
				),
			),
		)
	}

	q = q.Order(
		ent.Desc(secretrequest.FieldResolvedAt),
		ent.Desc(secretrequest.FieldID),
	)
	if opts.Limit > 0 {
		q = q.Limit(opts.Limit)
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	items := make([]*store.SecretRequest, len(rows))
	for i, r := range rows {
		items[i] = entSecretRequestToStore(r)
	}
	return items, nil
}

// ExpireSecretRequests implements store.SecretRequestStore.
func (c *CompositeStore) ExpireSecretRequests(ctx context.Context, now time.Time, limit int) ([]*store.SecretRequest, error) {
	now = now.UTC().Truncate(time.Microsecond)
	if limit <= 0 {
		limit = 200
	}
	return withMessageActionRetry(ctx, c, func(tx *CompositeStore) ([]*store.SecretRequest, error) {
		q := tx.client.SecretRequest.Query().
			Where(
				secretrequest.StatusEQ(secretrequest.StatusPending),
				secretrequest.ExpiresAtLTE(now),
			).
			Order(ent.Asc(secretrequest.FieldExpiresAt)).
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
		n, err := tx.client.SecretRequest.Update().
			Where(
				secretrequest.IDIn(ids...),
				secretrequest.StatusEQ(secretrequest.StatusPending),
				secretrequest.ExpiresAtLTE(now),
			).
			SetStatus(secretrequest.StatusExpired).
			SetResolvedAt(now).
			Save(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		if n == 0 {
			return nil, nil
		}
		updated, err := tx.client.SecretRequest.Query().
			Where(
				secretrequest.IDIn(ids...),
				secretrequest.StatusEQ(secretrequest.StatusExpired),
				secretrequest.ResolvedAtEQ(now),
			).
			All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		out := make([]*store.SecretRequest, len(updated))
		for i, r := range updated {
			out[i] = entSecretRequestToStore(r)
		}
		return out, nil
	})
}

// RecoverStaleSecretRequestClaims implements store.SecretRequestStore.
func (c *CompositeStore) RecoverStaleSecretRequestClaims(ctx context.Context, cutoff time.Time, limit int) ([]*store.SecretRequest, error) {
	cutoff = cutoff.UTC().Truncate(time.Microsecond)
	if limit <= 0 {
		limit = 200
	}
	return withMessageActionRetry(ctx, c, func(tx *CompositeStore) ([]*store.SecretRequest, error) {
		q := tx.client.SecretRequest.Query().
			Where(
				secretrequest.StatusEQ(secretrequest.StatusFulfilling),
				secretrequest.ClaimedAtLTE(cutoff),
			).
			Order(ent.Asc(secretrequest.FieldClaimedAt)).
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
		n, err := tx.client.SecretRequest.Update().
			Where(
				secretrequest.IDIn(ids...),
				secretrequest.StatusEQ(secretrequest.StatusFulfilling),
				secretrequest.ClaimedAtLTE(cutoff),
			).
			SetStatus(secretrequest.StatusPending).
			ClearClaimedAt().
			ClearResolvedByUserID().
			ClearResolvedByName().
			ClearResolvedByEmail().
			ClearSurface().
			ClearIdempotencyKey().
			Save(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		if n == 0 {
			return nil, nil
		}
		updated, err := tx.client.SecretRequest.Query().
			Where(
				secretrequest.IDIn(ids...),
				secretrequest.StatusEQ(secretrequest.StatusPending),
			).
			All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		out := make([]*store.SecretRequest, len(updated))
		for i, r := range updated {
			out[i] = entSecretRequestToStore(r)
		}
		return out, nil
	})
}

// WithdrawAgentSecretRequestsOnDelete implements store.SecretRequestStore.
func (c *CompositeStore) WithdrawAgentSecretRequestsOnDelete(ctx context.Context, agentID string, now time.Time) ([]*store.SecretRequest, error) {
	agentUID, err := parseUUID(agentID)
	if err != nil {
		return nil, err
	}
	now = now.UTC().Truncate(time.Microsecond)
	if now.IsZero() {
		now = time.Now().UTC().Truncate(time.Microsecond)
	}
	q := c.client.SecretRequest.Query().
		Where(
			secretrequest.AgentIDEQ(agentUID),
			secretrequest.StatusIn(secretrequest.StatusPending, secretrequest.StatusFulfilling),
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
	_, err = c.client.SecretRequest.Update().
		Where(
			secretrequest.IDIn(ids...),
			secretrequest.StatusIn(secretrequest.StatusPending, secretrequest.StatusFulfilling),
		).
		SetStatus(secretrequest.StatusWithdrawn).
		SetWithdrawnReason("agent deleted").
		SetWithdrawnBy(store.SecretRequestWithdrawnByHub).
		SetResolvedAt(now).
		ClearClaimedAt().
		Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	updated, err := c.client.SecretRequest.Query().
		Where(secretrequest.IDIn(ids...)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.SecretRequest, len(updated))
	for i, r := range updated {
		out[i] = entSecretRequestToStore(r)
	}
	store.RecordWithdrawnSecretRequests(ctx, out)
	return out, nil
}

// DeleteProjectSecretRequests implements store.SecretRequestStore.
func (c *CompositeStore) DeleteProjectSecretRequests(ctx context.Context, projectID string) (int, error) {
	uid, err := parseUUID(projectID)
	if err != nil {
		return 0, err
	}
	n, err := c.client.SecretRequest.Delete().
		Where(secretrequest.ProjectIDEQ(uid)).
		Exec(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

func deleteProjectSecretRequests(ctx context.Context, client *ent.Client, projectID uuid.UUID) error {
	_, err := client.SecretRequest.Delete().
		Where(secretrequest.ProjectIDEQ(projectID)).
		Exec(ctx)
	return err
}

func entSecretRequestToStore(r *ent.SecretRequest) *store.SecretRequest {
	if r == nil {
		return nil
	}
	out := &store.SecretRequest{
		ID:          r.ID.String(),
		ProjectID:   r.ProjectID.String(),
		AgentID:     r.AgentID.String(),
		AgentName:   r.AgentName,
		Key:         r.Key,
		KeyNorm:     r.KeyNorm,
		Version:     r.Version,
		Description: r.Description,
		SecretType:  string(r.SecretType),
		MessageID:   r.MessageID.String(),
		Position:    r.Position,
		IntroLength: r.IntroLength,
		Status:      string(r.Status),
		CreatedAt:   r.CreatedAt.UTC(),
		ExpiresAt:   r.ExpiresAt.UTC(),
	}
	if r.Target != nil {
		out.Target = *r.Target
	}
	if r.ConversationID != nil {
		out.ConversationID = r.ConversationID.String()
	}
	if r.ChatKey != nil {
		out.ChatKey = *r.ChatKey
	}
	if r.ReplacedByID != nil {
		out.ReplacedByID = r.ReplacedByID.String()
	}
	if r.ReplacedAt != nil {
		t := r.ReplacedAt.UTC()
		out.ReplacedAt = &t
	}
	if r.WithdrawnReason != nil {
		out.WithdrawnReason = *r.WithdrawnReason
	}
	if r.WithdrawnBy != nil {
		out.WithdrawnBy = *r.WithdrawnBy
	}
	if r.ResolvedByUserID != nil {
		out.ResolvedByUserID = *r.ResolvedByUserID
	}
	if r.ResolvedByName != nil {
		out.ResolvedByName = *r.ResolvedByName
	}
	if r.ResolvedByEmail != nil {
		out.ResolvedByEmail = *r.ResolvedByEmail
	}
	if r.DismissReason != nil {
		out.DismissReason = *r.DismissReason
	}
	if r.Surface != nil {
		out.Surface = string(*r.Surface)
	}
	if r.IdempotencyKey != nil {
		out.IdempotencyKey = *r.IdempotencyKey
	}
	if r.ResponseMessageID != nil {
		out.ResponseMessageID = r.ResponseMessageID.String()
	}
	if r.SecretVersion != nil {
		out.SecretVersion = *r.SecretVersion
	}
	if r.ClaimedAt != nil {
		t := r.ClaimedAt.UTC()
		out.ClaimedAt = &t
	}
	if r.ResolvedAt != nil {
		t := r.ResolvedAt.UTC()
		out.ResolvedAt = &t
	}
	return out
}
