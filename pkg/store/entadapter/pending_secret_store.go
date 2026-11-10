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
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	entpendingsecret "github.com/GoogleCloudPlatform/scion/pkg/ent/pendingsecret"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	entsecret "github.com/GoogleCloudPlatform/scion/pkg/ent/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// pendingSecretWriteAttempts bounds CreateOrReplacePendingSecret's retries
// after losing a race on the (key, scope, scope_id) unique index: the retry
// sees the winner's row and decides against it.
const pendingSecretWriteAttempts = 3

// PendingSecretStore implements store.PendingSecretStore using Ent ORM.
type PendingSecretStore struct {
	client *ent.Client
	inTx   bool // true when client wraps an ambient WithTx transaction
	// txFencedWrite is the ambient transaction's fenced-write mark, shared
	// with its SecretStore (newTxCompositeStore): a pending value write is
	// a fenced write, so a later policy transition in that transaction is
	// refused. Nil outside WithTx.
	txFencedWrite *atomic.Bool
}

// markFencedWrite records a fenced write in the ambient transaction.
func (s *PendingSecretStore) markFencedWrite() {
	if s.inTx && s.txFencedWrite != nil {
		s.txFencedWrite.Store(true)
	}
}

// fencePendingValue applies the encryption policy fence to a pending
// value ref on c (a transaction client): the policy is read share-locked
// on PostgreSQL, so a policy transition waits for the write to commit.
func fencePendingValue(ctx context.Context, c *ent.Client, valueRef string) error {
	policy, err := loadEncryptionPolicyForWrite(ctx, c)
	if err != nil {
		return err
	}
	if !policy.AllowsWrite(store.PendingValuePayload(valueRef)) {
		return store.ErrEncryptionPolicyMismatch
	}
	return nil
}

// NewPendingSecretStore creates a new Ent-backed PendingSecretStore.
func NewPendingSecretStore(client *ent.Client) *PendingSecretStore {
	return &PendingSecretStore{client: client}
}

var _ store.PendingSecretStore = (*PendingSecretStore)(nil)

// entPendingSecretToStore converts an Ent PendingSecret to the store model.
// The value ref is copied only when includeRef is true.
func entPendingSecretToStore(e *ent.PendingSecret, includeRef bool) store.PendingSecret {
	p := store.PendingSecret{
		ID:               e.ID.String(),
		Key:              e.Key,
		SecretType:       string(e.SecretType),
		Target:           e.Target,
		Scope:            e.Scope,
		ScopeID:          e.ScopeID,
		AllowProgeny:     e.AllowProgeny,
		InjectionMode:    string(e.InjectionMode),
		Description:      e.Description,
		ValueLen:         e.ValueLen,
		AgentID:          e.AgentID,
		AgentSlug:        e.AgentSlug,
		ProjectID:        e.ProjectID,
		ProjectName:      e.ProjectName,
		Created:          e.Created,
		Updated:          e.Updated,
		ReplacesSecretID: e.ReplacesSecretID,
		ReplacesVersion:  e.ReplacesVersion,
		ExpiresAt:        e.ExpiresAt,
	}
	if p.Target == "" {
		p.Target = p.Key
	}
	if includeRef {
		p.ValueRef = e.ValueRef
	}
	return p
}

// CreateOrReplacePendingSecret implements store.PendingSecretStore.
func (s *PendingSecretStore) CreateOrReplacePendingSecret(ctx context.Context, p *store.PendingSecret, now time.Time) (*store.PendingSecretWriteResult, error) {
	if p == nil {
		return nil, store.ErrInvalidInput
	}
	var (
		res *store.PendingSecretWriteResult
		err error
	)
	s.markFencedWrite()
	for attempt := 0; attempt < pendingSecretWriteAttempts; attempt++ {
		err = runInEntTx(ctx, s.client, s.inTx, func(c *ent.Client) error {
			var txErr error
			res, txErr = createOrReplacePendingSecret(ctx, c, p, now)
			return txErr
		})
		// A lost race on the unique index is retried in a new transaction,
		// which sees the winner's row. Inside an ambient transaction there
		// is no retry: the caller owns that transaction.
		if !errors.Is(err, store.ErrAlreadyExists) || s.inTx {
			break
		}
	}
	if errors.Is(err, store.ErrAlreadyExists) && !s.inTx {
		// Every attempt lost a race on the unique index: report a
		// transient conflict the caller can retry, not an existing row.
		return nil, fmt.Errorf("%w: the pending secret write lost %d races for its key", store.ErrTransient, pendingSecretWriteAttempts)
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

func createOrReplacePendingSecret(ctx context.Context, c *ent.Client, p *store.PendingSecret, now time.Time) (*store.PendingSecretWriteResult, error) {
	// Lock order: the policy, the owner's pending writes, then the
	// pending row.
	if err := fencePendingValue(ctx, c, p.ValueRef); err != nil {
		return nil, err
	}
	if isPostgres(c) {
		// Serialize the owner's pending writes so the caps are checked
		// and applied atomically. SQLite serializes writers already.
		if err := lockPendingSecretOwner(ctx, c, p.ScopeID); err != nil {
			return nil, err
		}
	}
	q := c.PendingSecret.Query().Where(
		entpendingsecret.KeyEQ(p.Key),
		entpendingsecret.ScopeEQ(p.Scope),
		entpendingsecret.ScopeIDEQ(p.ScopeID),
	)
	if isPostgres(c) {
		q = q.ForUpdate()
	}
	existing, err := q.Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return nil, mapError(err)
	}

	res := &store.PendingSecretWriteResult{}
	created := now
	expiresAt := now.Add(store.PendingSecretTTL)
	if existing != nil {
		switch {
		case !now.Before(existing.ExpiresAt):
			// An expired row is gone: it never blocks a new write.
		case existing.AgentID != p.AgentID:
			return nil, store.ErrPendingSecretHeldByOtherAgent
		default:
			res.Replaced = true
			created = existing.Created
			expiresAt = existing.ExpiresAt
		}
		if err := c.PendingSecret.DeleteOneID(existing.ID).Exec(ctx); err != nil {
			return nil, mapError(err)
		}
		res.DroppedValueRefs = append(res.DroppedValueRefs, existing.ValueRef)
	}

	// A replacement of the same agent's row takes that row's place; any
	// other write adds a row and must stay within the caps. The row of
	// this key, if any, is already deleted above, so the counts below
	// never include it.
	if !res.Replaced {
		if err := checkPendingSecretCaps(ctx, c, p, now); err != nil {
			return nil, err
		}
	}

	id := uuid.New()
	if p.ID != "" {
		id, err = parseUUID(p.ID)
		if err != nil {
			return nil, err
		}
	}
	secretType := p.SecretType
	if secretType == "" {
		secretType = store.SecretTypeEnvironment
	}
	injectionMode := p.InjectionMode
	if injectionMode == "" {
		injectionMode = store.InjectionModeAsNeeded
	}
	target := p.Target
	if target == "" {
		target = p.Key
	}

	create := c.PendingSecret.Create().
		SetID(id).
		SetKey(p.Key).
		SetSecretType(entpendingsecret.SecretType(secretType)).
		SetTarget(target).
		SetScope(p.Scope).
		SetScopeID(p.ScopeID).
		SetAllowProgeny(p.AllowProgeny).
		SetInjectionMode(entpendingsecret.InjectionMode(injectionMode)).
		SetDescription(p.Description).
		SetValueRef(p.ValueRef).
		SetValueLen(p.ValueLen).
		SetAgentID(p.AgentID).
		SetAgentSlug(p.AgentSlug).
		SetProjectID(p.ProjectID).
		SetProjectName(p.ProjectName).
		SetCreated(created).
		SetUpdated(now).
		SetExpiresAt(expiresAt)
	if p.ReplacesSecretID != "" {
		create = create.SetReplacesSecretID(p.ReplacesSecretID).SetReplacesVersion(p.ReplacesVersion)
	}
	row, err := create.Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	stored := entPendingSecretToStore(row, true)
	res.Secret = &stored
	return res, nil
}

// lockPendingSecretOwner takes the transaction-scoped per-owner pending
// secret write lock (store.LockPendingSecretOwnerWrite) on PostgreSQL. It is
// released when the transaction ends.
func lockPendingSecretOwner(ctx context.Context, c *ent.Client, scopeID string) error {
	var res sql.Result
	if err := c.Driver().Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)",
		[]any{int32(store.LockPendingSecretOwnerWrite), store.StableProjectHash(scopeID)}, &res); err != nil {
		return fmt.Errorf("lock pending secret owner: %w", err)
	}
	return nil
}

// checkPendingSecretCaps returns a *store.PendingSecretCapError when one
// more live pending secret by p.AgentID, or for p's owner, would exceed
// its cap.
func checkPendingSecretCaps(ctx context.Context, c *ent.Client, p *store.PendingSecret, now time.Time) error {
	agentCount, err := c.PendingSecret.Query().Where(
		entpendingsecret.AgentIDEQ(p.AgentID),
		entpendingsecret.ExpiresAtGT(now),
	).Count(ctx)
	if err != nil {
		return mapError(err)
	}
	if agentCount >= store.PendingSecretAgentCap {
		return &store.PendingSecretCapError{Cap: store.PendingSecretCapAgent, Limit: store.PendingSecretAgentCap}
	}
	ownerCount, err := c.PendingSecret.Query().Where(
		entpendingsecret.ScopeEQ(p.Scope),
		entpendingsecret.ScopeIDEQ(p.ScopeID),
		entpendingsecret.ExpiresAtGT(now),
	).Count(ctx)
	if err != nil {
		return mapError(err)
	}
	if ownerCount >= store.PendingSecretOwnerCap {
		return &store.PendingSecretCapError{Cap: store.PendingSecretCapOwner, Limit: store.PendingSecretOwnerCap}
	}
	return nil
}

// GetPendingSecret implements store.PendingSecretStore.
func (s *PendingSecretStore) GetPendingSecret(ctx context.Context, id string) (*store.PendingSecret, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		// A malformed ID names no row.
		return nil, store.ErrNotFound
	}
	row, err := s.client.PendingSecret.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	p := entPendingSecretToStore(row, true)
	return &p, nil
}

// ListPendingSecretsByOwner implements store.PendingSecretStore.
func (s *PendingSecretStore) ListPendingSecretsByOwner(ctx context.Context, scope, scopeID string) ([]store.PendingSecret, error) {
	rows, err := s.client.PendingSecret.Query().
		Where(
			entpendingsecret.ScopeEQ(scope),
			entpendingsecret.ScopeIDEQ(scopeID),
		).
		Order(ent.Asc(entpendingsecret.FieldKey)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]store.PendingSecret, 0, len(rows))
	for _, row := range rows {
		out = append(out, entPendingSecretToStore(row, false))
	}
	return out, nil
}

// DeletePendingSecret implements store.PendingSecretStore.
func (s *PendingSecretStore) DeletePendingSecret(ctx context.Context, id string) error {
	uid, err := uuid.Parse(id)
	if err != nil {
		return store.ErrNotFound
	}
	n, err := s.client.PendingSecret.Delete().Where(entpendingsecret.IDEQ(uid)).Exec(ctx)
	if err != nil {
		return mapError(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ClaimPendingSecret implements store.PendingSecretStore.
func (s *PendingSecretStore) ClaimPendingSecret(ctx context.Context, id, scope, scopeID string, now time.Time) (*store.PendingSecret, error) {
	return s.claimPendingSecret(ctx, id, scope, scopeID, now, false)
}

// ClaimPendingSecretForApproval implements store.PendingSecretStore.
func (s *PendingSecretStore) ClaimPendingSecretForApproval(ctx context.Context, id, scope, scopeID string, now time.Time) (*store.PendingSecret, error) {
	return s.claimPendingSecret(ctx, id, scope, scopeID, now, true)
}

// claimPendingSecret reads the row, then deletes it in a transaction whose
// first statement is the delete, conditioned on the owner and expiry. The
// delete's row count decides between concurrent claims: the delete takes
// the write lock (SQLite) or the row lock (PostgreSQL) before anything else
// in the transaction, and a second claimant then deletes no row. The row
// is immutable, so the data read before the delete is the data deleted.
// When checkExisting is set, the approved secret the row replaces is
// checked after the delete, in the same transaction; a mismatch rolls the
// delete back.
func (s *PendingSecretStore) claimPendingSecret(ctx context.Context, id, scope, scopeID string, now time.Time, checkExisting bool) (*store.PendingSecret, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, store.ErrNotFound
	}
	row, err := s.client.PendingSecret.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	err = runInEntTx(ctx, s.client, s.inTx, func(c *ent.Client) error {
		n, err := c.PendingSecret.Delete().Where(
			entpendingsecret.IDEQ(uid),
			entpendingsecret.ScopeEQ(scope),
			entpendingsecret.ScopeIDEQ(scopeID),
			entpendingsecret.ExpiresAtGT(now),
		).Exec(ctx)
		if err != nil {
			return mapError(err)
		}
		if n == 0 {
			return store.ErrNotFound
		}
		if checkExisting {
			return checkPendingSecretExisting(ctx, c, row)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	p := entPendingSecretToStore(row, true)
	return &p, nil
}

// checkPendingSecretExisting compares the approved secret of row's key and
// owner with the one row recorded at write time.
func checkPendingSecretExisting(ctx context.Context, c *ent.Client, row *ent.PendingSecret) error {
	current, err := c.Secret.Query().Where(
		entsecret.KeyEQ(row.Key),
		entsecret.ScopeEQ(row.Scope),
		entsecret.ScopeIDEQ(row.ScopeID),
	).Only(ctx)
	switch {
	case ent.IsNotFound(err):
		if row.ReplacesSecretID != "" {
			return store.ErrPendingSecretExistingChanged
		}
		return nil
	case err != nil:
		return mapError(err)
	}
	if current.ID.String() != row.ReplacesSecretID || current.Version != row.ReplacesVersion {
		return store.ErrPendingSecretExistingChanged
	}
	return nil
}

// RestorePendingSecret implements store.PendingSecretStore.
func (s *PendingSecretStore) RestorePendingSecret(ctx context.Context, p *store.PendingSecret) error {
	if p == nil {
		return store.ErrInvalidInput
	}
	uid, err := parseUUID(p.ID)
	if err != nil {
		return err
	}
	s.markFencedWrite()
	return runInEntTx(ctx, s.client, s.inTx, func(c *ent.Client) error {
		if err := fencePendingValue(ctx, c, p.ValueRef); err != nil {
			return err
		}
		create := c.PendingSecret.Create().
			SetID(uid).
			SetKey(p.Key).
			SetSecretType(entpendingsecret.SecretType(p.SecretType)).
			SetTarget(p.Target).
			SetScope(p.Scope).
			SetScopeID(p.ScopeID).
			SetAllowProgeny(p.AllowProgeny).
			SetInjectionMode(entpendingsecret.InjectionMode(p.InjectionMode)).
			SetDescription(p.Description).
			SetValueRef(p.ValueRef).
			SetValueLen(p.ValueLen).
			SetAgentID(p.AgentID).
			SetAgentSlug(p.AgentSlug).
			SetProjectID(p.ProjectID).
			SetProjectName(p.ProjectName).
			SetCreated(p.Created).
			SetUpdated(p.Updated).
			SetExpiresAt(p.ExpiresAt)
		if p.ReplacesSecretID != "" {
			create = create.SetReplacesSecretID(p.ReplacesSecretID).SetReplacesVersion(p.ReplacesVersion)
		}
		if _, err := create.Save(ctx); err != nil {
			return mapError(err)
		}
		return nil
	})
}

// DeletePendingSecretsByScope implements store.PendingSecretStore.
func (s *PendingSecretStore) DeletePendingSecretsByScope(ctx context.Context, scope, scopeID string) ([]string, error) {
	return s.deletePendingSecretsWhere(ctx,
		entpendingsecret.ScopeEQ(scope),
		entpendingsecret.ScopeIDEQ(scopeID),
	)
}

// DeletePendingSecretsByProject implements store.PendingSecretStore.
func (s *PendingSecretStore) DeletePendingSecretsByProject(ctx context.Context, projectID string) ([]string, error) {
	return s.deletePendingSecretsWhere(ctx, entpendingsecret.ProjectIDEQ(projectID))
}

// deletePendingSecretsWhere deletes the rows matching preds in one
// transaction and returns the value refs of the rows it deleted.
func (s *PendingSecretStore) deletePendingSecretsWhere(ctx context.Context, preds ...predicate.PendingSecret) ([]string, error) {
	var refs []string
	err := runInEntTx(ctx, s.client, s.inTx, func(c *ent.Client) error {
		q := c.PendingSecret.Query().Where(preds...)
		if isPostgres(c) {
			q = q.ForUpdate()
		}
		rows, err := q.All(ctx)
		if err != nil {
			return mapError(err)
		}
		if len(rows) == 0 {
			return nil
		}
		ids := make([]uuid.UUID, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
			refs = append(refs, row.ValueRef)
		}
		if _, err := c.PendingSecret.Delete().Where(entpendingsecret.IDIn(ids...)).Exec(ctx); err != nil {
			return mapError(err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return refs, nil
}

// ScanPendingSecretPayloads implements store.PendingSecretStore.
func (s *PendingSecretStore) ScanPendingSecretPayloads(ctx context.Context, afterID string, limit int) ([]store.SecretPayload, error) {
	if limit < 1 || limit > store.MaxSecretPayloadScan {
		return nil, fmt.Errorf("%w: scan limit must be between 1 and %d", store.ErrInvalidInput, store.MaxSecretPayloadScan)
	}
	q := s.client.PendingSecret.Query()
	if afterID != "" {
		uid, err := parseUUID(afterID)
		if err != nil {
			return nil, err
		}
		q = q.Where(entpendingsecret.IDGT(uid))
	}
	rows, err := q.Order(ent.Asc(entpendingsecret.FieldID)).Limit(limit).All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]store.SecretPayload, 0, len(rows))
	for _, r := range rows {
		value, ref := store.PendingValuePayload(r.ValueRef)
		out = append(out, store.SecretPayload{
			ID:             r.ID.String(),
			Key:            r.Key,
			Scope:          r.Scope,
			ScopeID:        r.ScopeID,
			EncryptedValue: value,
			SecretRef:      ref,
		})
	}
	return out, nil
}

// ListPendingSecretScopeIDs implements store.PendingSecretStore.
func (s *PendingSecretStore) ListPendingSecretScopeIDs(ctx context.Context, scope string) ([]string, error) {
	ids, err := s.client.PendingSecret.Query().
		Where(entpendingsecret.ScopeEQ(scope)).
		Unique(true).
		Select(entpendingsecret.FieldScopeID).
		Strings(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return ids, nil
}

// DeleteExpiredPendingSecrets implements store.PendingSecretStore.
func (s *PendingSecretStore) DeleteExpiredPendingSecrets(ctx context.Context, now time.Time, limit int) ([]store.PendingSecret, error) {
	if limit <= 0 {
		return nil, nil
	}
	var out []store.PendingSecret
	err := runInEntTx(ctx, s.client, s.inTx, func(c *ent.Client) error {
		q := c.PendingSecret.Query().
			Where(entpendingsecret.ExpiresAtLTE(now)).
			Order(ent.Asc(entpendingsecret.FieldExpiresAt)).
			Limit(limit)
		if isPostgres(c) {
			q = q.ForUpdate()
		}
		rows, err := q.All(ctx)
		if err != nil {
			return mapError(err)
		}
		out = make([]store.PendingSecret, 0, len(rows))
		for _, row := range rows {
			n, err := c.PendingSecret.Delete().Where(
				entpendingsecret.IDEQ(row.ID),
				entpendingsecret.ExpiresAtLTE(now),
			).Exec(ctx)
			if err != nil {
				return mapError(err)
			}
			if n == 1 {
				out = append(out, entPendingSecretToStore(row, true))
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
