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
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/uploadsession"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// UploadSessionStore implements store.UploadSessionStore using Ent.
type UploadSessionStore struct {
	client *ent.Client
	inTx   bool
}

// NewUploadSessionStore creates a new Ent-backed UploadSessionStore.
func NewUploadSessionStore(client *ent.Client) *UploadSessionStore {
	return &UploadSessionStore{client: client}
}

var _ store.UploadSessionStore = (*UploadSessionStore)(nil)
var _ store.UploadSessionStore = (*CompositeStore)(nil)

func uploadNowUTC() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

func truncUTC(t time.Time) time.Time {
	return t.UTC().Truncate(time.Microsecond)
}

func entUploadSessionToStore(row *ent.UploadSession) *store.UploadSession {
	if row == nil {
		return nil
	}
	out := &store.UploadSession{
		ID:            row.ID,
		HubID:         row.HubID,
		OwnerKind:     row.OwnerKind,
		OwnerID:       row.OwnerID,
		TargetKind:    store.UploadTargetKind(row.TargetKind),
		Overwrite:     row.Overwrite,
		FileName:      row.FileName,
		MediaType:     row.MediaType,
		DeclaredSize:  row.DeclaredSize,
		Received:      row.Received,
		HashState:     slices.Clone(row.HashState),
		Parts:         slices.Clone(row.Parts),
		StagingRef:    row.StagingRef,
		State:         store.UploadSessionState(row.State),
		Takeovers:     row.Takeovers,
		Result:        slices.Clone(row.Result),
		IdleExpiresAt: truncUTC(row.IdleExpiresAt),
		HardExpiresAt: truncUTC(row.HardExpiresAt),
		CreatedAt:     truncUTC(row.CreatedAt),
		UpdatedAt:     truncUTC(row.UpdatedAt),
	}
	if row.Head != nil {
		out.Head = slices.Clone(*row.Head)
	}
	if row.ScopeProjectID != nil {
		out.ScopeProjectID = *row.ScopeProjectID
	}
	if row.ScopeSharedDir != nil {
		out.ScopeSharedDir = *row.ScopeSharedDir
	}
	if row.DestPath != nil {
		out.DestPath = *row.DestPath
	}
	if row.LeaseToken != nil {
		out.LeaseToken = *row.LeaseToken
	}
	if row.LeaseUntil != nil {
		t := truncUTC(*row.LeaseUntil)
		out.LeaseUntil = &t
	}
	if row.ArtifactID != nil {
		out.ArtifactID = *row.ArtifactID
	}
	return out
}

// CreateUploadSession inserts s in state uploading after checking q atomically
// (§3.8). On Postgres it acquires pg_advisory_xact_lock(LockUploadQuota) inside
// the transaction; on SQLite the single-writer transaction serializes creates.
func (s *UploadSessionStore) CreateUploadSession(ctx context.Context, sess *store.UploadSession, q store.UploadQuota) error {
	if sess == nil || sess.ID == "" || sess.HubID == "" || sess.OwnerKind == "" || sess.OwnerID == "" || sess.FileName == "" || sess.StagingRef == "" {
		return fmt.Errorf("%w: upload session requires id, hub_id, owner, file_name, and staging_ref", store.ErrInvalidInput)
	}
	if !sess.TargetKind.Valid() {
		return fmt.Errorf("%w: invalid target_kind %q", store.ErrInvalidInput, sess.TargetKind)
	}
	if sess.DeclaredSize < 0 {
		return fmt.Errorf("%w: declared_size must be non-negative", store.ErrInvalidInput)
	}
	if sess.IdleExpiresAt.IsZero() || sess.HardExpiresAt.IsZero() {
		return fmt.Errorf("%w: idle_expires_at and hard_expires_at are required", store.ErrInvalidInput)
	}

	now := uploadNowUTC()
	createdAt := now
	if !sess.CreatedAt.IsZero() {
		createdAt = truncUTC(sess.CreatedAt)
	}
	updatedAt := createdAt
	idleExp := truncUTC(sess.IdleExpiresAt)
	hardExp := truncUTC(sess.HardExpiresAt)

	err := runInEntTx(ctx, s.client, s.inTx, func(c *ent.Client) error {
		d := c.Driver().Dialect()
		if d == dialect.Postgres {
			var lockRes stdsql.Result
			if err := c.Driver().Exec(ctx, "SELECT pg_advisory_xact_lock($1)", []any{int64(store.LockUploadQuota)}, &lockRes); err != nil {
				return fmt.Errorf("lock upload quota: %w", err)
			}
		}

		// Owner active sessions count + pending bytes: state IN (uploading, finalizing).
		if q.MaxSessionsPerUser > 0 || q.MaxPendingBytesPerUser > 0 {
			ownerSel := entsql.Dialect(d).
				Select("COUNT(*)", "COALESCE(SUM(declared_size), 0)").
				From(entsql.Table(uploadsession.Table)).
				Where(entsql.And(
					entsql.EQ(uploadsession.FieldOwnerKind, sess.OwnerKind),
					entsql.EQ(uploadsession.FieldOwnerID, sess.OwnerID),
					entsql.In(uploadsession.FieldState,
						string(store.UploadStateUploading),
						string(store.UploadStateFinalizing),
					),
				))
			query, args := ownerSel.Query()
			var rows entsql.Rows
			if err := c.Driver().Query(ctx, query, args, &rows); err != nil {
				return fmt.Errorf("query owner upload quota: %w", err)
			}
			var ownerCount int
			var ownerBytes int64
			if rows.Next() {
				if err := rows.Scan(&ownerCount, &ownerBytes); err != nil {
					_ = rows.Close()
					return fmt.Errorf("scan owner upload quota: %w", err)
				}
			}
			if err := rows.Close(); err != nil {
				return fmt.Errorf("close owner upload quota rows: %w", err)
			}
			if q.MaxSessionsPerUser > 0 && ownerCount >= q.MaxSessionsPerUser {
				return fmt.Errorf("%w: active upload sessions (%d) reached max_sessions_per_user (%d)",
					store.ErrQuotaExceeded, ownerCount, q.MaxSessionsPerUser)
			}
			if q.MaxPendingBytesPerUser > 0 && ownerBytes+sess.DeclaredSize > q.MaxPendingBytesPerUser {
				return fmt.Errorf("%w: owner pending bytes (%d + %d) exceeds max_pending_bytes_per_user (%d)",
					store.ErrQuotaExceeded, ownerBytes, sess.DeclaredSize, q.MaxPendingBytesPerUser)
			}
		}

		// Hub-wide pending bytes over all sessions still holding staging
		// (state != completed; rows are deleted once staging is cleaned up).
		if q.MaxPendingBytesHub > 0 {
			hubSel := entsql.Dialect(d).
				Select("COALESCE(SUM(declared_size), 0)").
				From(entsql.Table(uploadsession.Table)).
				Where(entsql.NEQ(uploadsession.FieldState, string(store.UploadStateCompleted)))
			query, args := hubSel.Query()
			var rows entsql.Rows
			if err := c.Driver().Query(ctx, query, args, &rows); err != nil {
				return fmt.Errorf("query hub upload quota: %w", err)
			}
			var hubBytes int64
			if rows.Next() {
				if err := rows.Scan(&hubBytes); err != nil {
					_ = rows.Close()
					return fmt.Errorf("scan hub upload quota: %w", err)
				}
			}
			if err := rows.Close(); err != nil {
				return fmt.Errorf("close hub upload quota rows: %w", err)
			}
			if hubBytes+sess.DeclaredSize > q.MaxPendingBytesHub {
				return fmt.Errorf("%w: hub pending bytes (%d + %d) exceeds max_pending_bytes_hub (%d)",
					store.ErrQuotaExceeded, hubBytes, sess.DeclaredSize, q.MaxPendingBytesHub)
			}
		}

		create := c.UploadSession.Create().
			SetID(sess.ID).
			SetHubID(sess.HubID).
			SetOwnerKind(sess.OwnerKind).
			SetOwnerID(sess.OwnerID).
			SetTargetKind(uploadsession.TargetKind(sess.TargetKind)).
			SetOverwrite(sess.Overwrite).
			SetFileName(sess.FileName).
			SetMediaType(sess.MediaType).
			SetDeclaredSize(sess.DeclaredSize).
			SetReceived(0).
			SetStagingRef(sess.StagingRef).
			SetState(uploadsession.StateUploading).
			SetTakeovers(0).
			SetIdleExpiresAt(idleExp).
			SetHardExpiresAt(hardExp).
			SetCreatedAt(createdAt).
			SetUpdatedAt(updatedAt)

		if sess.ScopeProjectID != "" {
			create.SetScopeProjectID(sess.ScopeProjectID)
		}
		if sess.ScopeSharedDir != "" {
			create.SetScopeSharedDir(sess.ScopeSharedDir)
		}
		if sess.DestPath != "" {
			create.SetDestPath(sess.DestPath)
		}
		if len(sess.HashState) > 0 {
			create.SetHashState(slices.Clone(sess.HashState))
		}
		if len(sess.Head) > 0 {
			create.SetHead(slices.Clone(sess.Head))
		}
		if len(sess.Parts) > 0 {
			create.SetParts(slices.Clone(sess.Parts))
		}
		if sess.ArtifactID != "" {
			create.SetArtifactID(sess.ArtifactID)
		}

		if _, err := create.Save(ctx); err != nil {
			return mapError(err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	sess.State = store.UploadStateUploading
	sess.Received = 0
	sess.Takeovers = 0
	sess.IdleExpiresAt = idleExp
	sess.HardExpiresAt = hardExp
	sess.CreatedAt = createdAt
	sess.UpdatedAt = updatedAt
	return nil
}

// GetUploadSession fetches an upload session by ID.
func (s *UploadSessionStore) GetUploadSession(ctx context.Context, id string) (*store.UploadSession, error) {
	if id == "" {
		return nil, store.ErrNotFound
	}
	row, err := s.client.UploadSession.Get(ctx, id)
	if err != nil {
		return nil, mapError(err)
	}
	return entUploadSessionToStore(row), nil
}

// ClaimUploadLease atomically acquires a lease on id when state == wantState,
// received == atOffset, and the lease is free or expired (§3.4, §3.7).
// Taking over an expired lease increments takeovers. Never modifies idle_expires_at.
func (s *UploadSessionStore) ClaimUploadLease(
	ctx context.Context,
	id string,
	wantState store.UploadSessionState,
	atOffset int64,
	token string,
	ttl time.Duration,
) (*store.UploadSession, error) {
	if id == "" || token == "" || ttl <= 0 {
		return nil, fmt.Errorf("%w: id, token, and positive ttl are required", store.ErrInvalidInput)
	}
	if !wantState.Valid() || wantState.IsTerminal() {
		return nil, store.ErrStateChanged
	}

	now := uploadNowUTC()
	leaseUntil := now.Add(ttl)
	d := s.client.Driver().Dialect()

	preds := []*entsql.Predicate{
		entsql.EQ(uploadsession.FieldID, id),
		entsql.EQ(uploadsession.FieldState, string(wantState)),
		entsql.EQ(uploadsession.FieldReceived, atOffset),
		entsql.Or(
			entsql.IsNull(uploadsession.FieldLeaseUntil),
			entsql.LT(uploadsession.FieldLeaseUntil, timeArg(d, now)),
		),
	}
	if wantState == store.UploadStateUploading {
		preds = append(preds,
			entsql.GT(uploadsession.FieldIdleExpiresAt, timeArg(d, now)),
			entsql.GT(uploadsession.FieldHardExpiresAt, timeArg(d, now)),
		)
	}

	ub := entsql.Dialect(d).
		Update(uploadsession.Table).
		Set(uploadsession.FieldLeaseToken, token).
		Set(uploadsession.FieldLeaseUntil, timeArg(d, leaseUntil)).
		Set(uploadsession.FieldUpdatedAt, timeArg(d, now)).
		Set(uploadsession.FieldTakeovers, entsql.Expr("takeovers + CASE WHEN lease_until IS NOT NULL THEN 1 ELSE 0 END")).
		Where(entsql.And(preds...))

	query, args := ub.Query()
	var res stdsql.Result
	if err := s.client.Driver().Exec(ctx, query, args, &res); err != nil {
		return nil, mapError(err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("claim upload lease rows affected: %w", err)
	}
	if affected == 0 {
		cur, gerr := s.GetUploadSession(ctx, id)
		if gerr != nil {
			return nil, gerr
		}
		if cur.State != wantState {
			return nil, store.ErrStateChanged
		}
		if wantState == store.UploadStateUploading && (!cur.IdleExpiresAt.After(now) || !cur.HardExpiresAt.After(now)) {
			return nil, store.ErrStateChanged
		}
		if cur.Received != atOffset {
			return nil, &store.OffsetMismatchError{Expected: atOffset, Received: cur.Received}
		}
		return nil, store.ErrBusy
	}

	return s.GetUploadSession(ctx, id)
}

// CommitUploadChunk advances received, hash_state, head (COALESCE), parts (if
// non-nil), and idle_expires_at, clearing the lease, fenced by
// lease_token == token AND state == uploading (§3.4, §3.7).
func (s *UploadSessionStore) CommitUploadChunk(ctx context.Context, id, token string, upd store.UploadChunkCommit) error {
	if id == "" || token == "" {
		return fmt.Errorf("%w: id and token are required", store.ErrInvalidInput)
	}
	if upd.Received < 0 || upd.IdleExpiresAt.IsZero() {
		return fmt.Errorf("%w: non-negative received and idle_expires_at are required", store.ErrInvalidInput)
	}

	now := uploadNowUTC()
	idleExp := truncUTC(upd.IdleExpiresAt)
	d := s.client.Driver().Dialect()

	idleArg := timeArg(d, idleExp)
	ub := entsql.Dialect(d).
		Update(uploadsession.Table).
		Set(uploadsession.FieldReceived, upd.Received).
		Set(uploadsession.FieldHashState, upd.HashState).
		Set(uploadsession.FieldIdleExpiresAt, entsql.ExprFunc(func(b *entsql.Builder) {
			b.WriteString("CASE WHEN ").Ident(uploadsession.FieldHardExpiresAt).
				WriteString(" < ").Arg(idleArg).
				WriteString(" THEN ").Ident(uploadsession.FieldHardExpiresAt).
				WriteString(" ELSE ").Arg(idleArg).
				WriteString(" END")
		})).
		SetNull(uploadsession.FieldLeaseToken).
		SetNull(uploadsession.FieldLeaseUntil).
		Set(uploadsession.FieldUpdatedAt, timeArg(d, now))

	if len(upd.Head) > 0 {
		ub.Set(uploadsession.FieldHead, entsql.ExprFunc(func(b *entsql.Builder) {
			b.WriteString("COALESCE(").Ident(uploadsession.FieldHead).WriteString(", ").Arg(upd.Head).WriteByte(')')
		}))
	}
	if upd.Parts != nil {
		partsJSON, err := json.Marshal(upd.Parts)
		if err != nil {
			return fmt.Errorf("marshal upload parts: %w", err)
		}
		ub.Set(uploadsession.FieldParts, partsJSON)
	}

	ub.Where(entsql.And(
		entsql.EQ(uploadsession.FieldID, id),
		entsql.EQ(uploadsession.FieldLeaseToken, token),
		entsql.EQ(uploadsession.FieldState, string(store.UploadStateUploading)),
	))

	query, args := ub.Query()
	var res stdsql.Result
	if err := s.client.Driver().Exec(ctx, query, args, &res); err != nil {
		return mapError(err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("commit upload chunk rows affected: %w", err)
	}
	if affected == 0 {
		if _, gerr := s.GetUploadSession(ctx, id); gerr != nil {
			return gerr
		}
		return store.ErrLeaseLost
	}
	return nil
}

// ReleaseUploadLease clears the lease when lease_token == token without
// modifying idle_expires_at (§3.4, §3.7).
func (s *UploadSessionStore) ReleaseUploadLease(ctx context.Context, id, token string) error {
	if id == "" || token == "" {
		return fmt.Errorf("%w: id and token are required", store.ErrInvalidInput)
	}

	now := uploadNowUTC()
	d := s.client.Driver().Dialect()

	ub := entsql.Dialect(d).
		Update(uploadsession.Table).
		SetNull(uploadsession.FieldLeaseToken).
		SetNull(uploadsession.FieldLeaseUntil).
		Set(uploadsession.FieldUpdatedAt, timeArg(d, now)).
		Where(entsql.And(
			entsql.EQ(uploadsession.FieldID, id),
			entsql.EQ(uploadsession.FieldLeaseToken, token),
		))

	query, args := ub.Query()
	var res stdsql.Result
	if err := s.client.Driver().Exec(ctx, query, args, &res); err != nil {
		return mapError(err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("release upload lease rows affected: %w", err)
	}
	if affected == 0 {
		if _, gerr := s.GetUploadSession(ctx, id); gerr != nil {
			return gerr
		}
		return store.ErrLeaseLost
	}
	return nil
}

// TransitionUploadSession conditionally transitions id from one of the
// non-terminal states in from to to (§3.7).
func (s *UploadSessionStore) TransitionUploadSession(
	ctx context.Context,
	id string,
	from []store.UploadSessionState,
	to store.UploadSessionState,
	token string,
	result json.RawMessage,
) error {
	if id == "" || !to.Valid() {
		return fmt.Errorf("%w: id and valid target state are required", store.ErrInvalidInput)
	}
	if len(from) == 0 {
		return store.ErrStateChanged
	}
	fromVals := make([]any, 0, len(from))
	for _, st := range from {
		if !st.Valid() || st.IsTerminal() {
			// Terminal states are sticky: never transition out of a terminal state.
			return store.ErrStateChanged
		}
		fromVals = append(fromVals, string(st))
	}

	now := uploadNowUTC()
	d := s.client.Driver().Dialect()

	ub := entsql.Dialect(d).
		Update(uploadsession.Table).
		Set(uploadsession.FieldState, string(to)).
		Set(uploadsession.FieldUpdatedAt, timeArg(d, now))

	if to.IsTerminal() {
		ub.SetNull(uploadsession.FieldLeaseToken).
			SetNull(uploadsession.FieldLeaseUntil)
	}
	if len(result) > 0 {
		ub.Set(uploadsession.FieldResult, []byte(result))
	}

	preds := []*entsql.Predicate{
		entsql.EQ(uploadsession.FieldID, id),
		entsql.In(uploadsession.FieldState, fromVals...),
	}
	if token != "" {
		preds = append(preds, entsql.EQ(uploadsession.FieldLeaseToken, token))
	} else {
		preds = append(preds, entsql.Or(
			entsql.IsNull(uploadsession.FieldLeaseUntil),
			entsql.LT(uploadsession.FieldLeaseUntil, timeArg(d, now)),
		))
	}
	ub.Where(entsql.And(preds...))

	query, args := ub.Query()
	var res stdsql.Result
	if err := s.client.Driver().Exec(ctx, query, args, &res); err != nil {
		return mapError(err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("transition upload session rows affected: %w", err)
	}
	if affected == 0 {
		cur, gerr := s.GetUploadSession(ctx, id)
		if gerr != nil {
			return gerr
		}
		if cur.State.IsTerminal() || !slices.Contains(from, cur.State) {
			return store.ErrStateChanged
		}
		if token != "" {
			return store.ErrLeaseLost
		}
		return store.ErrBusy
	}
	return nil
}

// ListExpiredUploadSessions returns up to limit sessions eligible for sweeper
// action as of now (§3.7).
func (s *UploadSessionStore) ListExpiredUploadSessions(ctx context.Context, now time.Time, limit int) ([]*store.UploadSession, error) {
	if limit <= 0 {
		limit = 100
	}
	nowUTC := truncUTC(now)
	terminalCutoff := nowUTC.Add(-store.UploadTerminalRetention)
	finalizingCutoff := nowUTC.Add(-store.UploadFinalizingStuckAge)

	rows, err := s.client.UploadSession.Query().
		Where(uploadsession.Or(
			// 1. uploading sessions past idle or hard expiry with free/expired lease
			uploadsession.And(
				uploadsession.StateEQ(uploadsession.StateUploading),
				uploadsession.Or(
					uploadsession.IdleExpiresAtLT(nowUTC),
					uploadsession.HardExpiresAtLT(nowUTC),
				),
				uploadsession.Or(
					uploadsession.LeaseUntilIsNil(),
					uploadsession.LeaseUntilLT(nowUTC),
				),
			),
			// 2. finalizing sessions stuck past 1h: either a held/expired lease
			// whose lease_until < now-1h, or a released finalizing row
			// (lease_until IS NULL, e.g. after a transient finalize error) whose
			// updated_at < now-1h (§3.7).
			uploadsession.And(
				uploadsession.StateEQ(uploadsession.StateFinalizing),
				uploadsession.Or(
					uploadsession.LeaseUntilLT(finalizingCutoff),
					uploadsession.And(
						uploadsession.LeaseUntilIsNil(),
						uploadsession.UpdatedAtLT(finalizingCutoff),
					),
				),
			),
			// 3. terminal sessions past 15m retention window
			uploadsession.And(
				uploadsession.StateIn(
					uploadsession.StateCompleted,
					uploadsession.StateFailed,
					uploadsession.StateAborted,
					uploadsession.StateExpired,
				),
				uploadsession.UpdatedAtLT(terminalCutoff),
			),
		)).
		Order(ent.Asc(uploadsession.FieldUpdatedAt)).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.UploadSession, 0, len(rows))
	for _, r := range rows {
		out = append(out, entUploadSessionToStore(r))
	}
	return out, nil
}

// TouchUploadSessionUpdatedAt advances updated_at to at without modifying
// state, lease, or idle_expires_at.
func (s *UploadSessionStore) TouchUploadSessionUpdatedAt(ctx context.Context, id string, at time.Time) error {
	if id == "" {
		return store.ErrNotFound
	}
	d := s.client.Driver().Dialect()
	ub := entsql.Dialect(d).
		Update(uploadsession.Table).
		Set(uploadsession.FieldUpdatedAt, timeArg(d, truncUTC(at))).
		Where(entsql.EQ(uploadsession.FieldID, id))
	query, args := ub.Query()
	var res stdsql.Result
	if err := s.client.Driver().Exec(ctx, query, args, &res); err != nil {
		return mapError(err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("touch upload session rows affected: %w", err)
	}
	if affected == 0 {
		return store.ErrNotFound
	}
	return nil
}

// DeleteUploadSession deletes an upload session row by ID.
func (s *UploadSessionStore) DeleteUploadSession(ctx context.Context, id string) error {
	if id == "" {
		return store.ErrNotFound
	}
	if err := s.client.UploadSession.DeleteOneID(id).Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}
