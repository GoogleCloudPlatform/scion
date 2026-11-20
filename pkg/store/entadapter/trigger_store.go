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
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/durablequeueitem"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/triggerevent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/triggerfire"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/webhookendpoint"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// TriggerStore implements store.TriggerStore over Ent.
type TriggerStore struct {
	client *ent.Client
	inTx   bool
}

var _ store.TriggerStore = (*TriggerStore)(nil)

// NewTriggerStore creates a new Ent-backed TriggerStore.
func NewTriggerStore(client *ent.Client) *TriggerStore {
	return &TriggerStore{client: client}
}

// LockTriggerQuota takes the transaction-scoped per-project trigger and
// webhook-endpoint quota advisory lock on PostgreSQL (store.LockTriggerQuota,
// pg_advisory_xact_lock). SQLite serializes write transactions, so once inside
// WithTx it is a no-op there.
func (s *TriggerStore) LockTriggerQuota(ctx context.Context, projectID string) error {
	if !s.inTx {
		return fmt.Errorf("%w: LockTriggerQuota must be called inside WithTx", store.ErrInvalidInput)
	}
	return lockTriggerQuotaClient(ctx, s.client, projectID)
}

func lockTriggerQuotaClient(ctx context.Context, client *ent.Client, projectID string) error {
	if _, err := parseUUID(projectID); err != nil {
		return err
	}
	if !isPostgres(client) {
		return nil
	}
	var res sql.Result
	if err := client.Driver().Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)",
		[]any{int32(store.LockTriggerQuota), store.StableProjectHash(projectID)}, &res); err != nil {
		return fmt.Errorf("lock trigger quota: %w", err)
	}
	return nil
}

func (s *TriggerStore) withClientTx(ctx context.Context, fn func(client *ent.Client) error) error {
	if s.inTx {
		return fn(s.client)
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return markTransient(fmt.Errorf("begin transaction: %w", err))
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx.Client()); err != nil {
		return markTransient(err)
	}
	if err := tx.Commit(); err != nil {
		return markTransient(fmt.Errorf("commit transaction: %w", err))
	}
	return nil
}

func cursorAdvancesTimeID(rowTime time.Time, rowID string, cursorTime time.Time, cursorID uuid.UUID) bool {
	rt := rowTime.UTC()
	if !rt.Equal(cursorTime) {
		return rt.Before(cursorTime)
	}
	return rowID < cursorID.String()
}

// ============================================================================
// WebhookEndpoint conversions & operations
// ============================================================================

func entWebhookEndpointToStore(e *ent.WebhookEndpoint) *store.WebhookEndpoint {
	return &store.WebhookEndpoint{
		ID:                  e.ID.String(),
		ProjectID:           e.ProjectID.String(),
		Name:                e.Name,
		Description:         e.Description,
		OwnerPrincipalKind:  e.OwnerPrincipalKind,
		OwnerPrincipalID:    e.OwnerPrincipalID,
		OwnerCredentialKind: e.OwnerCredentialKind,
		SecretVersion:       e.SecretVersion,
		SignatureMode:       e.SignatureMode,
		Status:              e.Status,
		CreatedBy:           e.CreatedBy,
		LastReceivedAt:      e.LastReceivedAt,
		ReceiveCount:        e.ReceiveCount,
		RejectCount:         e.RejectCount,
		CreatedAt:           e.Created,
		UpdatedAt:           e.Updated,
		Fixture:             fixtureProvenanceFromColumns(e.FixtureRootID, e.FixtureIssuerID),
	}
}

func (s *TriggerStore) CreateWebhookEndpoint(ctx context.Context, ep *store.WebhookEndpoint) error {
	if ep == nil || ep.ID == "" || ep.ProjectID == "" || ep.Name == "" ||
		ep.OwnerPrincipalKind == "" || ep.OwnerPrincipalID == "" || ep.OwnerCredentialKind == "" {
		return store.ErrInvalidInput
	}
	uid, err := parseUUID(ep.ID)
	if err != nil {
		return err
	}
	pid, err := parseUUID(ep.ProjectID)
	if err != nil {
		return err
	}
	fixtureRoot, fixtureIssuer, err := fixtureStampColumns(ep.Fixture)
	if err != nil {
		return err
	}
	secretVersion := ep.SecretVersion
	if secretVersion <= 0 {
		secretVersion = 1
	}
	sigMode := ep.SignatureMode
	if sigMode == "" {
		sigMode = store.WebhookSignatureModeScionV1
	}
	status := ep.Status
	if status == "" {
		status = store.WebhookEndpointStatusActive
	}
	now := time.Now().UTC()
	createdAt := now
	if !ep.CreatedAt.IsZero() {
		createdAt = ep.CreatedAt.UTC()
	}
	updatedAt := createdAt
	if !ep.UpdatedAt.IsZero() {
		updatedAt = ep.UpdatedAt.UTC()
	}

	create := s.client.WebhookEndpoint.Create().
		SetID(uid).
		SetNillableFixtureRootID(fixtureRoot).
		SetNillableFixtureIssuerID(fixtureIssuer).
		SetProjectID(pid).
		SetName(ep.Name).
		SetOwnerPrincipalKind(ep.OwnerPrincipalKind).
		SetOwnerPrincipalID(ep.OwnerPrincipalID).
		SetOwnerCredentialKind(ep.OwnerCredentialKind).
		SetSecretVersion(secretVersion).
		SetSignatureMode(sigMode).
		SetStatus(status).
		SetReceiveCount(ep.ReceiveCount).
		SetRejectCount(ep.RejectCount).
		SetCreated(createdAt).
		SetUpdated(updatedAt)

	if ep.Description != "" {
		create.SetDescription(ep.Description)
	}
	if ep.CreatedBy != "" {
		create.SetCreatedBy(ep.CreatedBy)
	}
	if ep.LastReceivedAt != nil {
		create.SetLastReceivedAt(ep.LastReceivedAt.UTC())
	}

	created, err := create.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	*ep = *entWebhookEndpointToStore(created)
	return nil
}

func (s *TriggerStore) GetWebhookEndpoint(ctx context.Context, id string) (*store.WebhookEndpoint, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	e, err := s.client.WebhookEndpoint.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entWebhookEndpointToStore(e), nil
}

func (s *TriggerStore) GetWebhookEndpointByName(ctx context.Context, projectID, name string) (*store.WebhookEndpoint, error) {
	pid, err := parseGetID(projectID)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, store.ErrNotFound
	}
	e, err := s.client.WebhookEndpoint.Query().
		Where(webhookendpoint.ProjectIDEQ(pid), webhookendpoint.NameEQ(name)).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entWebhookEndpointToStore(e), nil
}

func (s *TriggerStore) ListWebhookEndpoints(ctx context.Context, filter store.WebhookEndpointFilter, opts store.ListOptions) (*store.ListResult[store.WebhookEndpoint], error) {
	query := s.client.WebhookEndpoint.Query()
	if filter.ProjectID != "" {
		pid, err := parseUUID(filter.ProjectID)
		if err != nil {
			return nil, err
		}
		query.Where(webhookendpoint.ProjectIDEQ(pid))
	}
	if filter.Name != "" {
		query.Where(webhookendpoint.NameEQ(filter.Name))
	}
	if filter.Status != "" {
		query.Where(webhookendpoint.StatusEQ(filter.Status))
	}
	if filter.OwnerPrincipalKind != "" {
		query.Where(webhookendpoint.OwnerPrincipalKindEQ(filter.OwnerPrincipalKind))
	}
	if filter.OwnerPrincipalID != "" {
		query.Where(webhookendpoint.OwnerPrincipalIDEQ(filter.OwnerPrincipalID))
	}

	totalCount, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}

	var (
		cursorCreated time.Time
		cursorID      uuid.UUID
	)
	if opts.Cursor != "" {
		cursorCreated, cursorID, err = decodeCursor(opts.Cursor)
		if err != nil {
			return nil, fmt.Errorf("invalid cursor: %w", err)
		}
		cursorCreated = cursorCreated.UTC()
		query.Where(webhookendpoint.Or(
			webhookendpoint.CreatedLT(cursorCreated),
			webhookendpoint.And(
				webhookendpoint.CreatedEQ(cursorCreated),
				webhookendpoint.IDLT(cursorID),
			),
		))
	}

	limit := clampLimit(opts.Limit)
	entities, err := query.
		Order(webhookendpoint.ByCreated(entsql.OrderDesc())).
		Order(webhookendpoint.ByID(entsql.OrderDesc())).
		Limit(limit + 1).
		All(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]store.WebhookEndpoint, 0, len(entities))
	for _, e := range entities {
		items = append(items, *entWebhookEndpointToStore(e))
	}

	result := &store.ListResult[store.WebhookEndpoint]{TotalCount: totalCount}
	if len(items) > limit {
		result.Items = items[:limit]
		last := items[limit-1]
		if opts.Cursor == "" || cursorAdvancesTimeID(last.CreatedAt, last.ID, cursorCreated, cursorID) {
			result.NextCursor = encodeCursor(last.CreatedAt.UTC(), last.ID)
		}
	} else {
		result.Items = items
	}
	return result, nil
}

func (s *TriggerStore) CountWebhookEndpoints(ctx context.Context, projectID string) (int, error) {
	pid, err := parseUUID(projectID)
	if err != nil {
		return 0, err
	}
	return s.client.WebhookEndpoint.Query().Where(webhookendpoint.ProjectIDEQ(pid)).Count(ctx)
}

func (s *TriggerStore) FlushWebhookEndpointCounters(ctx context.Context, id string, receiveDelta, rejectDelta int, lastReceivedAt *time.Time) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	u := s.client.WebhookEndpoint.UpdateOneID(uid).SetUpdated(now)
	if receiveDelta != 0 {
		u.AddReceiveCount(receiveDelta)
	}
	if rejectDelta != 0 {
		u.AddRejectCount(rejectDelta)
	}
	if lastReceivedAt != nil {
		u.SetLastReceivedAt(lastReceivedAt.UTC())
	}
	return mapError(u.Exec(ctx))
}

func (s *TriggerStore) DeleteWebhookEndpoint(ctx context.Context, projectID, id string) ([]string, []string, error) {
	pid, err := parseGetID(projectID)
	if err != nil {
		return nil, nil, err
	}
	uid, err := parseGetID(id)
	if err != nil {
		return nil, nil, err
	}

	var (
		pausedTriggerIDs  []string
		deletedSecretKeys []string
	)
	err = s.withClientTx(ctx, func(client *ent.Client) error {
		if lErr := lockTriggerQuotaClient(ctx, client, pid.String()); lErr != nil {
			return lErr
		}
		ep, getErr := client.WebhookEndpoint.Get(ctx, uid)
		if getErr != nil {
			return mapError(getErr)
		}
		if ep.ProjectID != pid {
			return store.ErrNotFound
		}

		// 1. Pause dependent triggers with status_reason = "source_deleted" (§4.1, §5.6).
		// We pause active triggers (and user-paused triggers with no reason so
		// they cannot be resumed against the deleted source without editing).
		deps, qErr := client.Trigger.Query().
			Where(
				trigger.ProjectIDEQ(pid),
				trigger.SourceKindEQ(store.TriggerSourceKindWebhook),
				trigger.SourceIDEQ(uid.String()),
				trigger.Or(
					trigger.StatusEQ(store.TriggerStatusActive),
					trigger.StatusReasonIsNil(),
					trigger.StatusReasonEQ(store.TriggerStatusReasonNone),
				),
			).
			Select(trigger.FieldID).
			All(ctx)
		if qErr != nil {
			return qErr
		}
		if len(deps) > 0 {
			depIDs := make([]uuid.UUID, 0, len(deps))
			for _, d := range deps {
				depIDs = append(depIDs, d.ID)
				pausedTriggerIDs = append(pausedTriggerIDs, d.ID.String())
			}
			now := time.Now().UTC()
			if _, uErr := client.Trigger.Update().
				Where(trigger.IDIn(depIDs...)).
				SetStatus(store.TriggerStatusPaused).
				SetStatusReason(store.TriggerStatusReasonSourceDeleted).
				SetUpdated(now).
				Save(ctx); uErr != nil {
				return mapError(uErr)
			}
		}

		// 2. Delete the endpoint's hub-scoped internal secret rows (§4.1, R1).
		prefix := store.WebhookEndpointSecretKeyPrefix(pid.String(), uid.String())
		secRows, sErr := client.Secret.Query().
			Where(
				secret.ScopeEQ(store.ScopeHub),
				secret.SecretTypeEQ(secret.SecretTypeInternal),
				secret.KeyHasPrefix(prefix),
			).
			Select(secret.FieldID, secret.FieldKey).
			All(ctx)
		if sErr != nil {
			return sErr
		}
		var secIDs []uuid.UUID
		for _, row := range secRows {
			if strings.HasPrefix(row.Key, prefix) {
				secIDs = append(secIDs, row.ID)
				deletedSecretKeys = append(deletedSecretKeys, row.Key)
			}
		}
		if len(secIDs) > 0 {
			if _, dErr := client.Secret.Delete().Where(secret.IDIn(secIDs...)).Exec(ctx); dErr != nil {
				return mapError(dErr)
			}
		}

		// 3. Delete the endpoint row itself.
		return mapError(client.WebhookEndpoint.DeleteOneID(uid).Exec(ctx))
	})
	if err != nil {
		return nil, nil, err
	}
	return pausedTriggerIDs, deletedSecretKeys, nil
}

// ============================================================================
// Trigger conversions & operations
// ============================================================================

func marshalMatchTypes(types []string) (string, error) {
	if len(types) == 0 {
		return "", fmt.Errorf("%w: match_types must not be empty", store.ErrInvalidInput)
	}
	raw, err := json.Marshal(types)
	if err != nil {
		return "", fmt.Errorf("%w: marshal match_types: %v", store.ErrInvalidInput, err)
	}
	return string(raw), nil
}

func unmarshalMatchTypes(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func marshalMatchRules(rules []store.TriggerMatchRule) (string, error) {
	if len(rules) == 0 {
		return "[]", nil
	}
	raw, err := json.Marshal(rules)
	if err != nil {
		return "", fmt.Errorf("%w: marshal match_rules: %v", store.ErrInvalidInput, err)
	}
	return string(raw), nil
}

func unmarshalMatchRules(raw string) ([]store.TriggerMatchRule, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "[]" {
		return nil, false
	}
	if trimmed[0] != '[' {
		return nil, true
	}
	var rawElems []json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &rawElems); err != nil {
		return nil, true
	}
	if len(rawElems) == 0 {
		return nil, false
	}
	out := make([]store.TriggerMatchRule, len(rawElems))
	for i, elemRaw := range rawElems {
		et := bytes.TrimSpace(elemRaw)
		if len(et) == 0 || et[0] != '{' {
			return nil, true
		}
		dec := json.NewDecoder(bytes.NewReader(et))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&out[i]); err != nil {
			return nil, true
		}
		if dec.More() || dec.Decode(&struct{}{}) != io.EOF {
			return nil, true
		}
		if strings.TrimSpace(out[i].Expr) == "" {
			return nil, true
		}
	}
	return out, false
}

func entTriggerToStore(e *ent.Trigger) *store.Trigger {
	matchRules, matchRulesCorrupt := unmarshalMatchRules(e.MatchRules)
	tr := &store.Trigger{
		ID:                e.ID.String(),
		ProjectID:         e.ProjectID.String(),
		Name:              e.Name,
		Description:       e.Description,
		SourceKind:        e.SourceKind,
		SourceID:          e.SourceID,
		MatchTypes:        unmarshalMatchTypes(e.MatchTypes),
		MatchDialect:      e.MatchDialect,
		MatchRule:         e.MatchRule,
		MatchRules:        matchRules,
		MatchRulesCorrupt: matchRulesCorrupt,
		ActionType:        e.ActionType,
		ActionPayload:     e.ActionPayload,
		SubjectDialect:    e.SubjectDialect,
		SubjectRule:       e.SubjectRule,
		SubjectMode:       e.SubjectMode,
		Status:            e.Status,
		StatusReason:      e.StatusReason,
		LastFireAt:        e.LastFireAt,
		LastFireStatus:    e.LastFireStatus,
		LastFireError:     e.LastFireError,
		FireCount:         e.FireCount,
		ErrorCount:        e.ErrorCount,
		CreatedBy:         e.CreatedBy,
		CreatedAt:         e.Created,
		UpdatedAt:         e.Updated,
		Fixture:           fixtureProvenanceFromColumns(e.FixtureRootID, e.FixtureIssuerID),
	}
	tr.InitiatorAttribution = initiatorAttributionFromColumns(
		e.InitiatorPrincipalKind, e.InitiatorPrincipalID,
		e.InitiatorCredentialKind, e.InitiatorCredentialID, e.InitiatorCredentialSnapshot,
		e.AttributionVersion, e.AuthorizationRevision,
	)
	tr.AuthorityCeiling = authorityCeilingFromColumns(
		e.AuthorityCeilingKind, e.AuthorityCeilingVersion, e.AuthorityCeilingPermissionIds,
		e.AuthorityCeilingBoundaryKind, e.AuthorityCeilingBoundaryProjectID, e.AuthorityCeilingSourceExpiresAt,
	)
	return tr
}

func setTriggerInitiatorAttribution(create *ent.TriggerCreate, attr store.InitiatorAttribution) {
	if attr.InitiatorPrincipalKind != "" {
		create.SetInitiatorPrincipalKind(attr.InitiatorPrincipalKind)
	}
	if attr.InitiatorPrincipalID != "" {
		create.SetInitiatorPrincipalID(attr.InitiatorPrincipalID)
	}
	if attr.InitiatorCredentialKind != "" {
		create.SetInitiatorCredentialKind(attr.InitiatorCredentialKind)
	}
	if attr.InitiatorCredentialID != "" {
		create.SetInitiatorCredentialID(attr.InitiatorCredentialID)
	}
	if attr.InitiatorCredentialSnapshot != "" {
		create.SetInitiatorCredentialSnapshot(attr.InitiatorCredentialSnapshot)
	}
	if attr.AttributionVersion != 0 {
		create.SetAttributionVersion(attr.AttributionVersion)
	}
	if attr.AuthorizationRevision != 0 {
		create.SetAuthorizationRevision(attr.AuthorizationRevision)
	}
}

func setTriggerAttributionUpdate(u *ent.TriggerUpdate, attr store.InitiatorAttribution) *ent.TriggerUpdate {
	if attr.InitiatorPrincipalKind != "" {
		u = u.SetInitiatorPrincipalKind(attr.InitiatorPrincipalKind)
	} else {
		u = u.ClearInitiatorPrincipalKind()
	}
	if attr.InitiatorPrincipalID != "" {
		u = u.SetInitiatorPrincipalID(attr.InitiatorPrincipalID)
	} else {
		u = u.ClearInitiatorPrincipalID()
	}
	if attr.InitiatorCredentialKind != "" {
		u = u.SetInitiatorCredentialKind(attr.InitiatorCredentialKind)
	} else {
		u = u.ClearInitiatorCredentialKind()
	}
	if attr.InitiatorCredentialID != "" {
		u = u.SetInitiatorCredentialID(attr.InitiatorCredentialID)
	} else {
		u = u.ClearInitiatorCredentialID()
	}
	if attr.InitiatorCredentialSnapshot != "" {
		u = u.SetInitiatorCredentialSnapshot(attr.InitiatorCredentialSnapshot)
	} else {
		u = u.ClearInitiatorCredentialSnapshot()
	}
	if attr.AttributionVersion != 0 {
		u = u.SetAttributionVersion(attr.AttributionVersion)
	} else {
		u = u.ClearAttributionVersion()
	}
	if attr.AuthorizationRevision != 0 {
		u = u.SetAuthorizationRevision(attr.AuthorizationRevision)
	} else {
		u = u.ClearAuthorizationRevision()
	}
	return u
}

func setTriggerAuthorityCeilingUpdate(u *ent.TriggerUpdate, c store.EffectCeiling) *ent.TriggerUpdate {
	u = u.SetAuthorityCeilingKind(string(c.Kind)).
		SetAuthorityCeilingVersion(int32(c.Version)).
		SetAuthorityCeilingBoundaryKind(c.BoundaryKind).
		SetAuthorityCeilingBoundaryProjectID(c.BoundaryProjectID)
	if ids := authorityCeilingPermissionIDsColumn(c); ids != nil {
		u = u.SetAuthorityCeilingPermissionIds(*ids)
	} else {
		u = u.ClearAuthorityCeilingPermissionIds()
	}
	if c.SourceExpiresAt != nil {
		u = u.SetAuthorityCeilingSourceExpiresAt(c.SourceExpiresAt.UTC())
	} else {
		u = u.ClearAuthorityCeilingSourceExpiresAt()
	}
	return u
}

func triggerRecordedAuthorityIs(kind, id string) predicate.Trigger {
	return trigger.And(
		trigger.InitiatorPrincipalKindEQ(kind),
		trigger.InitiatorPrincipalIDEQ(id),
		trigger.AttributionVersionGTE(1),
		trigger.InitiatorCredentialKindNotIn("", store.InitiatorCredentialKindLegacyUnknown),
	)
}

func triggerRecordedAuthoritySQLPredicates(tr *entsql.SelectTable, kind, id string) []*entsql.Predicate {
	return []*entsql.Predicate{
		entsql.EQ(tr.C(trigger.FieldInitiatorPrincipalKind), kind),
		entsql.EQ(tr.C(trigger.FieldInitiatorPrincipalID), id),
		entsql.GTE(tr.C(trigger.FieldAttributionVersion), 1),
		entsql.NotIn(tr.C(trigger.FieldInitiatorCredentialKind), "", store.InitiatorCredentialKindLegacyUnknown),
	}
}

func applyTriggerFilter(query *ent.TriggerQuery, filter store.TriggerFilter) error {
	if filter.ProjectID != "" {
		pid, err := parseUUID(filter.ProjectID)
		if err != nil {
			return err
		}
		query.Where(trigger.ProjectIDEQ(pid))
	}
	if filter.SourceKind != "" {
		query.Where(trigger.SourceKindEQ(filter.SourceKind))
	}
	if filter.SourceID != "" {
		query.Where(trigger.SourceIDEQ(filter.SourceID))
	}
	if filter.Status != "" {
		query.Where(trigger.StatusEQ(filter.Status))
	}
	if filter.Name != "" {
		query.Where(trigger.NameEQ(filter.Name))
	}
	if filter.InitiatorAgentID != "" {
		query.Where(triggerRecordedAuthorityIs(store.DelegationPrincipalAgent, filter.InitiatorAgentID))
	}
	if p := filter.AuthorityPrincipal; !p.IsZero() {
		if p.Kind == "" || p.ID == "" {
			return fmt.Errorf("%w: trigger authority principal needs both a kind and an ID", store.ErrInvalidInput)
		}
		query.Where(triggerRecordedAuthorityIs(p.Kind, p.ID))
	}
	if filter.SystemPausedOnly {
		query.Where(trigger.StatusEQ(store.TriggerStatusPaused))
		query.Where(trigger.StatusReasonNotNil(), trigger.StatusReasonNEQ(store.TriggerStatusReasonNone))
	}
	return nil
}

func (s *TriggerStore) CreateTrigger(ctx context.Context, tr *store.Trigger) error {
	if tr == nil || tr.ID == "" || tr.ProjectID == "" || tr.Name == "" || tr.SourceKind == "" || tr.ActionType == "" {
		return store.ErrInvalidInput
	}
	uid, err := parseUUID(tr.ID)
	if err != nil {
		return err
	}
	pid, err := parseUUID(tr.ProjectID)
	if err != nil {
		return err
	}
	matchTypesJSON, err := marshalMatchTypes(tr.MatchTypes)
	if err != nil {
		return err
	}
	matchRulesJSON, err := marshalMatchRules(tr.MatchRules)
	if err != nil {
		return err
	}
	if err := validateEdgeCeiling(tr.AuthorityCeiling); err != nil {
		return err
	}
	fixtureRoot, fixtureIssuer, err := fixtureStampColumns(tr.Fixture)
	if err != nil {
		return err
	}

	subjectDialect := tr.SubjectDialect
	if subjectDialect == "" {
		subjectDialect = store.TriggerDialectCEL
	}
	subjectMode := tr.SubjectMode
	if subjectMode == "" {
		subjectMode = store.TriggerSubjectModeNew
	}
	status := tr.Status
	if status == "" {
		status = store.TriggerStatusActive
	}
	payload := tr.ActionPayload
	if payload == "" {
		payload = "{}"
	}
	now := time.Now().UTC()
	createdAt := now
	if !tr.CreatedAt.IsZero() {
		createdAt = tr.CreatedAt.UTC()
	}
	updatedAt := createdAt
	if !tr.UpdatedAt.IsZero() {
		updatedAt = tr.UpdatedAt.UTC()
	}

	create := s.client.Trigger.Create().
		SetID(uid).
		SetNillableFixtureRootID(fixtureRoot).
		SetNillableFixtureIssuerID(fixtureIssuer).
		SetProjectID(pid).
		SetName(tr.Name).
		SetSourceKind(tr.SourceKind).
		SetSourceID(tr.SourceID).
		SetMatchTypes(matchTypesJSON).
		SetMatchDialect(tr.MatchDialect).
		SetMatchRule(tr.MatchRule).
		SetMatchRules(matchRulesJSON).
		SetActionType(tr.ActionType).
		SetActionPayload(payload).
		SetSubjectDialect(subjectDialect).
		SetSubjectRule(tr.SubjectRule).
		SetSubjectMode(subjectMode).
		SetStatus(status).
		SetFireCount(tr.FireCount).
		SetErrorCount(tr.ErrorCount).
		SetCreated(createdAt).
		SetUpdated(updatedAt)

	if tr.Description != "" {
		create.SetDescription(tr.Description)
	}
	if tr.StatusReason != "" {
		create.SetStatusReason(tr.StatusReason)
	}
	if tr.LastFireAt != nil {
		create.SetLastFireAt(tr.LastFireAt.UTC())
	}
	if tr.LastFireStatus != "" {
		create.SetLastFireStatus(tr.LastFireStatus)
	}
	if tr.LastFireError != "" {
		create.SetLastFireError(tr.LastFireError)
	}
	if tr.CreatedBy != "" {
		create.SetCreatedBy(tr.CreatedBy)
	}
	setTriggerInitiatorAttribution(create, tr.InitiatorAttribution)
	c := tr.AuthorityCeiling
	create.SetAuthorityCeilingKind(string(c.Kind)).
		SetAuthorityCeilingVersion(int32(c.Version)).
		SetNillableAuthorityCeilingPermissionIds(authorityCeilingPermissionIDsColumn(c)).
		SetAuthorityCeilingBoundaryKind(c.BoundaryKind).
		SetAuthorityCeilingBoundaryProjectID(c.BoundaryProjectID).
		SetNillableAuthorityCeilingSourceExpiresAt(utcPtr(c.SourceExpiresAt))

	created, err := create.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	tr.CreatedAt = created.Created
	tr.UpdatedAt = created.Updated
	tr.Status = created.Status
	tr.MatchDialect = created.MatchDialect
	tr.SubjectDialect = created.SubjectDialect
	tr.SubjectMode = created.SubjectMode
	return nil
}

func (s *TriggerStore) GetTrigger(ctx context.Context, id string) (*store.Trigger, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	e, err := s.client.Trigger.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entTriggerToStore(e), nil
}

func (s *TriggerStore) GetTriggerByName(ctx context.Context, projectID, name string) (*store.Trigger, error) {
	pid, err := parseGetID(projectID)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, store.ErrNotFound
	}
	e, err := s.client.Trigger.Query().
		Where(trigger.ProjectIDEQ(pid), trigger.NameEQ(name)).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entTriggerToStore(e), nil
}

func (s *TriggerStore) ListTriggers(ctx context.Context, filter store.TriggerFilter, opts store.ListOptions) (*store.ListResult[store.Trigger], error) {
	query := s.client.Trigger.Query()
	if err := applyTriggerFilter(query, filter); err != nil {
		return nil, err
	}

	totalCount, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}

	var (
		cursorCreated time.Time
		cursorID      uuid.UUID
	)
	if opts.Cursor != "" {
		cursorCreated, cursorID, err = decodeCursor(opts.Cursor)
		if err != nil {
			return nil, fmt.Errorf("invalid cursor: %w", err)
		}
		cursorCreated = cursorCreated.UTC()
		query.Where(trigger.Or(
			trigger.CreatedLT(cursorCreated),
			trigger.And(
				trigger.CreatedEQ(cursorCreated),
				trigger.IDLT(cursorID),
			),
		))
	}

	limit := clampLimit(opts.Limit)
	entities, err := query.
		Order(trigger.ByCreated(entsql.OrderDesc())).
		Order(trigger.ByID(entsql.OrderDesc())).
		Limit(limit + 1).
		All(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]store.Trigger, 0, len(entities))
	for _, e := range entities {
		items = append(items, *entTriggerToStore(e))
	}

	result := &store.ListResult[store.Trigger]{TotalCount: totalCount}
	if len(items) > limit {
		result.Items = items[:limit]
		last := items[limit-1]
		if opts.Cursor == "" || cursorAdvancesTimeID(last.CreatedAt, last.ID, cursorCreated, cursorID) {
			result.NextCursor = encodeCursor(last.CreatedAt.UTC(), last.ID)
		}
	} else {
		result.Items = items
	}
	return result, nil
}

func (s *TriggerStore) CountTriggers(ctx context.Context, filter store.TriggerFilter) (int, error) {
	query := s.client.Trigger.Query()
	if err := applyTriggerFilter(query, filter); err != nil {
		return 0, err
	}
	return query.Count(ctx)
}

func (s *TriggerStore) UpdateTrigger(
	ctx context.Context, tr *store.Trigger, fields store.TriggerFieldMask,
	prevRevision int, prevRevisionKnown bool, attribution *store.InitiatorAttribution,
) error {
	uid, err := parseUUID(tr.ID)
	if err != nil {
		return err
	}
	if attribution != nil {
		if err := validateEdgeCeiling(tr.AuthorityCeiling); err != nil {
			return err
		}
	}

	now := time.Now().UTC()
	update := s.client.Trigger.Update().Where(trigger.IDEQ(uid)).SetUpdated(now)
	if prevRevisionKnown {
		update = update.Where(trigger.AuthorizationRevisionEQ(prevRevision))
	} else {
		update = update.Where(trigger.AuthorizationRevisionIsNil())
	}

	if fields.Name {
		update = update.SetName(tr.Name)
	}
	if fields.Description {
		if tr.Description != "" {
			update = update.SetDescription(tr.Description)
		} else {
			update = update.ClearDescription()
		}
	}
	if fields.SourceKind {
		update = update.SetSourceKind(tr.SourceKind)
	}
	if fields.SourceID {
		update = update.SetSourceID(tr.SourceID)
	}
	if fields.MatchTypes {
		matchTypesJSON, mErr := marshalMatchTypes(tr.MatchTypes)
		if mErr != nil {
			return mErr
		}
		update = update.SetMatchTypes(matchTypesJSON)
	}
	if fields.MatchDialect {
		update = update.SetMatchDialect(tr.MatchDialect)
	}
	if fields.MatchRule {
		update = update.SetMatchRule(tr.MatchRule)
	}
	if fields.MatchRules {
		matchRulesJSON, mErr := marshalMatchRules(tr.MatchRules)
		if mErr != nil {
			return mErr
		}
		update = update.SetMatchRules(matchRulesJSON)
	}
	if fields.ActionType {
		update = update.SetActionType(tr.ActionType)
	}
	if fields.ActionPayload {
		update = update.SetActionPayload(tr.ActionPayload)
	}
	if fields.SubjectDialect {
		update = update.SetSubjectDialect(tr.SubjectDialect)
	}
	if fields.SubjectRule {
		update = update.SetSubjectRule(tr.SubjectRule)
	}
	if fields.SubjectMode {
		update = update.SetSubjectMode(tr.SubjectMode)
	}
	if fields.Status {
		update = update.SetStatus(tr.Status)
		if tr.StatusReason != "" {
			update = update.SetStatusReason(tr.StatusReason)
		} else {
			update = update.ClearStatusReason()
		}
	}
	if attribution != nil {
		update = setTriggerAttributionUpdate(update, *attribution)
		update = setTriggerAuthorityCeilingUpdate(update, tr.AuthorityCeiling)
	}

	affected, err := update.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	if affected == 0 {
		if _, getErr := s.client.Trigger.Get(ctx, uid); ent.IsNotFound(getErr) {
			return store.ErrNotFound
		}
		return store.ErrRevisionConflict
	}

	if attribution != nil {
		tr.InitiatorAttribution = *attribution
	}
	updated, err := s.client.Trigger.Get(ctx, uid)
	if err != nil {
		return mapError(err)
	}
	tr.UpdatedAt = updated.Updated
	tr.Status = updated.Status
	tr.StatusReason = updated.StatusReason
	if fields.MatchRules {
		tr.MatchRules, tr.MatchRulesCorrupt = unmarshalMatchRules(updated.MatchRules)
	}
	return nil
}

func (s *TriggerStore) UpdateTriggerStatus(ctx context.Context, id, status, reason string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	update := s.client.Trigger.UpdateOneID(uid).SetStatus(status).SetUpdated(time.Now().UTC())
	if reason != "" {
		update = update.SetStatusReason(reason)
	} else {
		update = update.ClearStatusReason()
	}
	return mapError(update.Exec(ctx))
}

func (s *TriggerStore) RecordTriggerFire(ctx context.Context, id string, firedAt time.Time, status, errMsg string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	firedAt = firedAt.UTC()
	now := time.Now().UTC()
	counters := s.client.Trigger.UpdateOneID(uid).AddFireCount(1).SetUpdated(now)
	if status == store.TriggerFireStatusFailed || status == store.TriggerFireStatusRefused {
		counters.AddErrorCount(1)
	}
	if err := counters.Exec(ctx); err != nil {
		return mapError(err)
	}
	last := s.client.Trigger.Update().
		Where(trigger.ID(uid), trigger.Or(trigger.LastFireAtIsNil(), trigger.LastFireAtLT(firedAt))).
		SetLastFireAt(firedAt).
		SetLastFireStatus(status).
		SetUpdated(now)
	if errMsg != "" {
		last.SetLastFireError(errMsg)
	} else {
		last.ClearLastFireError()
	}
	_, err = last.Save(ctx)
	return mapError(err)
}

func (s *TriggerStore) DeleteTrigger(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	return mapError(s.client.Trigger.DeleteOneID(uid).Exec(ctx))
}

func (s *TriggerStore) DeleteTriggersByProject(ctx context.Context, projectID string) (int, error) {
	pid, err := parseUUID(projectID)
	if err != nil {
		return 0, err
	}
	var triggersDeleted int
	err = s.withClientTx(ctx, func(client *ent.Client) error {
		prefix := store.WebhookEndpointProjectSecretKeyPrefix(pid.String())
		secRows, sErr := client.Secret.Query().
			Where(
				secret.ScopeEQ(store.ScopeHub),
				secret.SecretTypeEQ(secret.SecretTypeInternal),
				secret.KeyHasPrefix(prefix),
			).
			Select(secret.FieldID, secret.FieldKey).
			All(ctx)
		if sErr != nil {
			return sErr
		}
		var secIDs []uuid.UUID
		for _, row := range secRows {
			if strings.HasPrefix(row.Key, prefix) {
				secIDs = append(secIDs, row.ID)
			}
		}
		if len(secIDs) > 0 {
			if _, dErr := client.Secret.Delete().Where(secret.IDIn(secIDs...)).Exec(ctx); dErr != nil {
				return mapError(dErr)
			}
		}
		if _, dErr := client.WebhookEndpoint.Delete().Where(webhookendpoint.ProjectIDEQ(pid)).Exec(ctx); dErr != nil {
			return mapError(dErr)
		}
		if _, dErr := client.TriggerFire.Delete().Where(triggerfire.ProjectIDEQ(pid)).Exec(ctx); dErr != nil {
			return mapError(dErr)
		}
		if _, dErr := client.TriggerEvent.Delete().Where(triggerevent.ProjectIDEQ(pid)).Exec(ctx); dErr != nil {
			return mapError(dErr)
		}
		n, dErr := client.Trigger.Delete().Where(trigger.ProjectIDEQ(pid)).Exec(ctx)
		if dErr != nil {
			return mapError(dErr)
		}
		triggersDeleted = n
		return nil
	})
	return triggersDeleted, err
}

// ============================================================================
// TriggerEvent conversions & operations
// ============================================================================

func marshalStringMap(m map[string]string) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("%w: marshal string map: %v", store.ErrInvalidInput, err)
	}
	return string(raw), nil
}

func unmarshalStringMap(raw string) map[string]string {
	out := map[string]string{}
	if raw == "" || raw == "{}" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return map[string]string{}
	}
	return out
}

func encodeCompactNoEscapeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b, nil
}

func marshalStringListMap(m map[string][]string) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	clean := make(map[string][]string, len(m))
	for k, v := range m {
		if v == nil {
			clean[k] = []string{}
			continue
		}
		clean[k] = v
	}
	// Measure and persist with SetEscapeHTML(false) so '<', '>', and '&' match
	// the exact serialized byte count enforced by githubtrigger.Normalize.
	raw, err := encodeCompactNoEscapeJSON(clean)
	if err != nil {
		return "", fmt.Errorf("%w: marshal string list map: %v", store.ErrInvalidInput, err)
	}
	if len(raw) > store.MaxTriggerEventListsBytes {
		return "", fmt.Errorf("%w: trigger event lists exceed %d bytes", store.ErrInvalidInput, store.MaxTriggerEventListsBytes)
	}
	return string(raw), nil
}

func unmarshalStringListMap(raw string) map[string][]string {
	out := map[string][]string{}
	if raw == "" || raw == "{}" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return map[string][]string{}
	}
	for k, v := range out {
		if v == nil {
			out[k] = []string{}
		}
	}
	return out
}

func entTriggerEventToStore(e *ent.TriggerEvent) *store.TriggerEvent {
	return &store.TriggerEvent{
		ID:           e.ID.String(),
		ProjectID:    e.ProjectID.String(),
		SourceKind:   e.SourceKind,
		SourceID:     e.SourceID,
		Type:         e.Type,
		Subject:      e.Subject,
		DedupKey:     e.DedupKey,
		BodySHA256:   e.BodySha256,
		OccurredAt:   e.OccurredAt,
		ReceivedAt:   e.ReceivedAt,
		Attributes:   unmarshalStringMap(e.Attributes),
		Lists:        unmarshalStringListMap(e.Lists),
		Headers:      unmarshalStringMap(e.Headers),
		Payload:      e.Payload,
		PayloadBytes: e.PayloadBytes,
		Status:       e.Status,
		MatchCount:   e.MatchCount,
		FireCount:    e.FireCount,
		Error:        e.Error,
		ProcessedAt:  e.ProcessedAt,
		CreatedAt:    e.Created,
	}
}

func (s *TriggerStore) CreateTriggerEvent(ctx context.Context, evt *store.TriggerEvent) error {
	if evt == nil || evt.ID == "" || evt.ProjectID == "" || evt.SourceKind == "" ||
		evt.SourceID == "" || evt.Type == "" || evt.Subject == "" || evt.DedupKey == "" {
		return store.ErrInvalidInput
	}
	uid, err := parseUUID(evt.ID)
	if err != nil {
		return err
	}
	pid, err := parseUUID(evt.ProjectID)
	if err != nil {
		return err
	}
	attrsJSON, err := marshalStringMap(evt.Attributes)
	if err != nil {
		return err
	}
	listsJSON, err := marshalStringListMap(evt.Lists)
	if err != nil {
		return err
	}
	headersJSON, err := marshalStringMap(evt.Headers)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	receivedAt := now
	if !evt.ReceivedAt.IsZero() {
		receivedAt = evt.ReceivedAt.UTC()
	}
	occurredAt := receivedAt
	if !evt.OccurredAt.IsZero() {
		occurredAt = evt.OccurredAt.UTC()
	}
	createdAt := receivedAt
	if !evt.CreatedAt.IsZero() {
		createdAt = evt.CreatedAt.UTC()
	}
	status := evt.Status
	if status == "" {
		status = store.TriggerEventStatusPending
	}
	payloadBytes := evt.PayloadBytes
	if payloadBytes == 0 && evt.Payload != "" {
		payloadBytes = len(evt.Payload)
	}

	create := s.client.TriggerEvent.Create().
		SetID(uid).
		SetProjectID(pid).
		SetSourceKind(evt.SourceKind).
		SetSourceID(evt.SourceID).
		SetType(evt.Type).
		SetSubject(evt.Subject).
		SetDedupKey(evt.DedupKey).
		SetBodySha256(evt.BodySHA256).
		SetOccurredAt(occurredAt).
		SetReceivedAt(receivedAt).
		SetAttributes(attrsJSON).
		SetLists(listsJSON).
		SetHeaders(headersJSON).
		SetPayload(evt.Payload).
		SetPayloadBytes(payloadBytes).
		SetStatus(status).
		SetMatchCount(evt.MatchCount).
		SetFireCount(evt.FireCount).
		SetCreated(createdAt)

	if evt.Error != "" {
		create.SetError(evt.Error)
	}
	if evt.ProcessedAt != nil {
		create.SetProcessedAt(evt.ProcessedAt.UTC())
	}

	created, err := create.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	*evt = *entTriggerEventToStore(created)
	return nil
}

func (s *TriggerStore) GetTriggerEvent(ctx context.Context, id string) (*store.TriggerEvent, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	e, err := s.client.TriggerEvent.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entTriggerEventToStore(e), nil
}

func (s *TriggerStore) GetTriggerEventByDedupKey(ctx context.Context, sourceKind, sourceID, dedupKey string) (*store.TriggerEvent, error) {
	if sourceKind == "" || sourceID == "" || dedupKey == "" {
		return nil, store.ErrNotFound
	}
	e, err := s.client.TriggerEvent.Query().
		Where(
			triggerevent.SourceKindEQ(sourceKind),
			triggerevent.SourceIDEQ(sourceID),
			triggerevent.DedupKeyEQ(dedupKey),
		).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entTriggerEventToStore(e), nil
}

func (s *TriggerStore) GetTriggerEventByBodySHA256(ctx context.Context, projectID, sourceKind, bodySHA256 string) (*store.TriggerEvent, error) {
	if projectID == "" || sourceKind == "" || bodySHA256 == "" {
		return nil, store.ErrNotFound
	}
	pid, err := parseGetID(projectID)
	if err != nil {
		return nil, err
	}
	e, err := s.client.TriggerEvent.Query().
		Where(
			triggerevent.ProjectIDEQ(pid),
			triggerevent.SourceKindEQ(sourceKind),
			triggerevent.BodySha256EQ(bodySHA256),
		).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entTriggerEventToStore(e), nil
}

func (s *TriggerStore) ListTriggerEvents(ctx context.Context, filter store.TriggerEventFilter, opts store.ListOptions) (*store.ListResult[store.TriggerEvent], error) {
	query := s.client.TriggerEvent.Query()
	var pid uuid.UUID
	var hasPID bool
	if filter.ProjectID != "" {
		var err error
		pid, err = parseUUID(filter.ProjectID)
		if err != nil {
			return nil, err
		}
		hasPID = true
		query.Where(triggerevent.ProjectIDEQ(pid))
	}
	if filter.SourceKind != "" {
		query.Where(triggerevent.SourceKindEQ(filter.SourceKind))
	}
	if filter.SourceID != "" {
		query.Where(triggerevent.SourceIDEQ(filter.SourceID))
	}
	if filter.Status != "" {
		query.Where(triggerevent.StatusEQ(filter.Status))
	}
	if filter.Since != nil && !filter.Since.IsZero() {
		query.Where(triggerevent.ReceivedAtGTE(filter.Since.UTC()))
	}
	if filter.VisibleToAgentID != "" {
		epQ := s.client.WebhookEndpoint.Query().Where(
			webhookendpoint.OwnerPrincipalKindEQ(store.DelegationPrincipalAgent),
			webhookendpoint.OwnerPrincipalIDEQ(filter.VisibleToAgentID),
		)
		trQ := s.client.Trigger.Query().Where(triggerRecordedAuthorityIs(store.DelegationPrincipalAgent, filter.VisibleToAgentID))
		if hasPID {
			epQ.Where(webhookendpoint.ProjectIDEQ(pid))
			trQ.Where(trigger.ProjectIDEQ(pid))
		}
		hasOwnedEPs, err := epQ.Exist(ctx)
		if err != nil {
			return nil, err
		}
		hasOwnedTrs, err := trQ.Exist(ctx)
		if err != nil {
			return nil, err
		}
		var visPreds []predicate.TriggerEvent
		if hasOwnedEPs {
			agentID := filter.VisibleToAgentID
			visPreds = append(visPreds, triggerevent.And(
				triggerevent.SourceKindEQ(store.TriggerSourceKindWebhook),
				predicate.TriggerEvent(func(sel *entsql.Selector) {
					d := sel.Dialect()
					ep := entsql.Dialect(d).Table(webhookendpoint.Table).As("ep")
					conds := []*entsql.Predicate{
						entsql.P(func(b *entsql.Builder) {
							b.WriteString("CAST(").WriteString(ep.C(webhookendpoint.FieldID)).WriteString(" AS TEXT) = ").WriteString(sel.C(triggerevent.FieldSourceID))
						}),
						entsql.EQ(ep.C(webhookendpoint.FieldOwnerPrincipalKind), store.DelegationPrincipalAgent),
						entsql.EQ(ep.C(webhookendpoint.FieldOwnerPrincipalID), agentID),
					}
					if hasPID {
						conds = append(conds, entsql.EQ(ep.C(webhookendpoint.FieldProjectID), pid))
					}
					sub := entsql.Dialect(d).Select(ep.C(webhookendpoint.FieldID)).From(ep).Where(entsql.And(conds...))
					sel.Where(entsql.Exists(sub))
				}),
			))
		}
		if hasOwnedTrs {
			agentID := filter.VisibleToAgentID
			visPreds = append(visPreds, predicate.TriggerEvent(func(sel *entsql.Selector) {
				d := sel.Dialect()
				tf := entsql.Dialect(d).Table(triggerfire.Table).As("tf")
				tr := entsql.Dialect(d).Table(trigger.Table).As("tr")
				conds := append(
					[]*entsql.Predicate{entsql.ColumnsEQ(tf.C(triggerfire.FieldEventID), sel.C(triggerevent.FieldID))},
					triggerRecordedAuthoritySQLPredicates(tr, store.DelegationPrincipalAgent, agentID)...,
				)
				if hasPID {
					conds = append(conds, entsql.EQ(tr.C(trigger.FieldProjectID), pid))
				}
				sub := entsql.Dialect(d).
					Select(tf.C(triggerfire.FieldID)).
					From(tf).
					Join(tr).On(tf.C(triggerfire.FieldTriggerID), tr.C(trigger.FieldID)).
					Where(entsql.And(conds...))
				sel.Where(entsql.Exists(sub))
			}))
		}
		if len(visPreds) == 0 {
			return &store.ListResult[store.TriggerEvent]{Items: []store.TriggerEvent{}, TotalCount: 0}, nil
		}
		query.Where(triggerevent.Or(visPreds...))
	}

	totalCount, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}

	var (
		cursorReceived time.Time
		cursorID       uuid.UUID
	)
	if opts.Cursor != "" {
		cursorReceived, cursorID, err = decodeCursor(opts.Cursor)
		if err != nil {
			return nil, fmt.Errorf("invalid cursor: %w", err)
		}
		cursorReceived = cursorReceived.UTC()
		query.Where(triggerevent.Or(
			triggerevent.ReceivedAtLT(cursorReceived),
			triggerevent.And(
				triggerevent.ReceivedAtEQ(cursorReceived),
				triggerevent.IDLT(cursorID),
			),
		))
	}

	limit := clampLimit(opts.Limit)
	entities, err := query.
		Order(triggerevent.ByReceivedAt(entsql.OrderDesc())).
		Order(triggerevent.ByID(entsql.OrderDesc())).
		Limit(limit + 1).
		All(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]store.TriggerEvent, 0, len(entities))
	for _, e := range entities {
		items = append(items, *entTriggerEventToStore(e))
	}

	result := &store.ListResult[store.TriggerEvent]{TotalCount: totalCount}
	if len(items) > limit {
		result.Items = items[:limit]
		last := items[limit-1]
		if opts.Cursor == "" || cursorAdvancesTimeID(last.ReceivedAt, last.ID, cursorReceived, cursorID) {
			result.NextCursor = encodeCursor(last.ReceivedAt.UTC(), last.ID)
		}
	} else {
		result.Items = items
	}
	return result, nil
}

func (s *TriggerStore) CountPendingTriggerEvents(ctx context.Context, projectID string) (int, error) {
	pid, err := parseUUID(projectID)
	if err != nil {
		return 0, err
	}
	return s.client.TriggerEvent.Query().
		Where(
			triggerevent.ProjectIDEQ(pid),
			triggerevent.StatusEQ(store.TriggerEventStatusPending),
		).
		Count(ctx)
}

func (s *TriggerStore) MarkTriggerEventProcessed(ctx context.Context, id string, processedAt time.Time, matchCount, fireCount int) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	affected, err := s.client.TriggerEvent.Update().
		Where(triggerevent.IDEQ(uid), triggerevent.StatusEQ(store.TriggerEventStatusPending)).
		SetStatus(store.TriggerEventStatusProcessed).
		SetProcessedAt(processedAt.UTC()).
		SetMatchCount(matchCount).
		SetFireCount(fireCount).
		ClearError().
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	if affected == 0 {
		exists, exErr := s.client.TriggerEvent.Query().Where(triggerevent.IDEQ(uid)).Exist(ctx)
		if exErr != nil {
			return mapError(exErr)
		}
		if !exists {
			return store.ErrNotFound
		}
	}
	return nil
}

func (s *TriggerStore) MarkTriggerEventFailed(ctx context.Context, id string, processedAt time.Time, errMsg string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	u := s.client.TriggerEvent.Update().
		Where(triggerevent.IDEQ(uid), triggerevent.StatusEQ(store.TriggerEventStatusPending)).
		SetStatus(store.TriggerEventStatusFailed).
		SetProcessedAt(processedAt.UTC())
	if errMsg != "" {
		u.SetError(errMsg)
	} else {
		u.ClearError()
	}
	affected, err := u.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	if affected == 0 {
		exists, exErr := s.client.TriggerEvent.Query().Where(triggerevent.IDEQ(uid)).Exist(ctx)
		if exErr != nil {
			return mapError(exErr)
		}
		if !exists {
			return store.ErrNotFound
		}
	}
	return nil
}

func (s *TriggerStore) ReconcileOrphanedPendingTriggerEvents(ctx context.Context, queueKind string, olderThan, now time.Time, errMsg string, limit int) (int, error) {
	if queueKind == "" {
		return 0, store.ErrInvalidInput
	}
	olderThan = olderThan.UTC()
	now = now.UTC()

	const scanBatch = 200
	var (
		hasCursor      bool
		cursorReceived time.Time
		cursorID       uuid.UUID
		reconciled     int
	)

	for {
		q := s.client.TriggerEvent.Query().
			Where(
				triggerevent.StatusEQ(store.TriggerEventStatusPending),
				triggerevent.ReceivedAtLTE(olderThan),
			)
		if hasCursor {
			q.Where(triggerevent.Or(
				triggerevent.ReceivedAtGT(cursorReceived),
				triggerevent.And(
					triggerevent.ReceivedAtEQ(cursorReceived),
					triggerevent.IDGT(cursorID),
				),
			))
		}
		candidates, err := q.
			Order(triggerevent.ByReceivedAt()).
			Order(triggerevent.ByID()).
			Limit(scanBatch).
			Select(triggerevent.FieldID, triggerevent.FieldReceivedAt).
			All(ctx)
		if err != nil {
			return reconciled, mapError(err)
		}
		if len(candidates) == 0 {
			break
		}

		idStrs := make([]string, len(candidates))
		for i, c := range candidates {
			idStrs[i] = c.ID.String()
		}
		queuedRows, err := s.client.DurableQueueItem.Query().
			Where(
				durablequeueitem.KindEQ(queueKind),
				durablequeueitem.DedupKeyIn(idStrs...),
			).
			Select(durablequeueitem.FieldDedupKey).
			All(ctx)
		if err != nil {
			return reconciled, mapError(err)
		}
		queued := make(map[string]struct{}, len(queuedRows))
		for _, r := range queuedRows {
			queued[r.DedupKey] = struct{}{}
		}

		for _, c := range candidates {
			if _, ok := queued[c.ID.String()]; ok {
				continue
			}
			u := s.client.TriggerEvent.Update().
				Where(triggerevent.IDEQ(c.ID), triggerevent.StatusEQ(store.TriggerEventStatusPending)).
				SetStatus(store.TriggerEventStatusFailed).
				SetProcessedAt(now)
			if errMsg != "" {
				u.SetError(errMsg)
			} else {
				u.ClearError()
			}
			affected, uErr := u.Save(ctx)
			if uErr != nil {
				return reconciled, mapError(uErr)
			}
			if affected > 0 {
				reconciled++
				if limit > 0 && reconciled >= limit {
					return reconciled, nil
				}
			}
		}

		last := candidates[len(candidates)-1]
		hasCursor = true
		cursorReceived = last.ReceivedAt.UTC()
		cursorID = last.ID
		if len(candidates) < scanBatch {
			break
		}
	}
	return reconciled, nil
}

// ============================================================================
// TriggerFire conversions & operations
// ============================================================================

func entTriggerFireToStore(e *ent.TriggerFire) *store.TriggerFire {
	tf := &store.TriggerFire{
		ID:           e.ID.String(),
		ProjectID:    e.ProjectID.String(),
		TriggerID:    e.TriggerID.String(),
		EventID:      e.EventID.String(),
		Subject:      e.Subject,
		ActionType:   e.ActionType,
		Status:       e.Status,
		Error:        e.Error,
		ResultRef:    e.ResultRef,
		ExecutorKind: e.ExecutorKind,
		ExecutorID:   e.ExecutorID,
		LeaseUntil:   e.LeaseUntil,
		StartedAt:    e.StartedAt,
		FinishedAt:   e.FinishedAt,
		CreatedAt:    e.Created,
	}
	tf.InitiatorAttribution = initiatorAttributionFromColumns(
		e.InitiatorPrincipalKind, e.InitiatorPrincipalID,
		e.InitiatorCredentialKind, e.InitiatorCredentialID, e.InitiatorCredentialSnapshot,
		e.AttributionVersion, e.AuthorizationRevision,
	)
	return tf
}

func setTriggerFireInitiatorAttribution(create *ent.TriggerFireCreate, attr store.InitiatorAttribution) {
	if attr.InitiatorPrincipalKind != "" {
		create.SetInitiatorPrincipalKind(attr.InitiatorPrincipalKind)
	}
	if attr.InitiatorPrincipalID != "" {
		create.SetInitiatorPrincipalID(attr.InitiatorPrincipalID)
	}
	if attr.InitiatorCredentialKind != "" {
		create.SetInitiatorCredentialKind(attr.InitiatorCredentialKind)
	}
	if attr.InitiatorCredentialID != "" {
		create.SetInitiatorCredentialID(attr.InitiatorCredentialID)
	}
	if attr.InitiatorCredentialSnapshot != "" {
		create.SetInitiatorCredentialSnapshot(attr.InitiatorCredentialSnapshot)
	}
	if attr.AttributionVersion != 0 {
		create.SetAttributionVersion(attr.AttributionVersion)
	}
	if attr.AuthorizationRevision != 0 {
		create.SetAuthorizationRevision(attr.AuthorizationRevision)
	}
}

func (s *TriggerStore) ClaimTriggerFire(ctx context.Context, fire *store.TriggerFire) (bool, *store.TriggerFire, error) {
	if fire == nil || fire.ID == "" || fire.ProjectID == "" || fire.TriggerID == "" ||
		fire.EventID == "" || fire.ActionType == "" || fire.LeaseUntil.IsZero() {
		return false, nil, store.ErrInvalidInput
	}
	uid, err := parseUUID(fire.ID)
	if err != nil {
		return false, nil, err
	}
	pid, err := parseUUID(fire.ProjectID)
	if err != nil {
		return false, nil, err
	}
	tid, err := parseUUID(fire.TriggerID)
	if err != nil {
		return false, nil, err
	}
	eid, err := parseUUID(fire.EventID)
	if err != nil {
		return false, nil, err
	}
	now := time.Now().UTC()
	startedAt := now
	if !fire.StartedAt.IsZero() {
		startedAt = fire.StartedAt.UTC()
	}
	createdAt := startedAt
	if !fire.CreatedAt.IsZero() {
		createdAt = fire.CreatedAt.UTC()
	}
	status := fire.Status
	if status == "" {
		status = store.TriggerFireStatusFiring
	}

	create := s.client.TriggerFire.Create().
		SetID(uid).
		SetProjectID(pid).
		SetTriggerID(tid).
		SetEventID(eid).
		SetSubject(fire.Subject).
		SetActionType(fire.ActionType).
		SetStatus(status).
		SetExecutorKind(fire.ExecutorKind).
		SetExecutorID(fire.ExecutorID).
		SetLeaseUntil(fire.LeaseUntil.UTC()).
		SetStartedAt(startedAt).
		SetCreated(createdAt)
	if fire.Error != "" {
		create.SetError(fire.Error)
	}
	if fire.ResultRef != "" {
		create.SetResultRef(fire.ResultRef)
	}
	if fire.FinishedAt != nil {
		create.SetFinishedAt(fire.FinishedAt.UTC())
	}
	setTriggerFireInitiatorAttribution(create, fire.InitiatorAttribution)

	created, err := create.Save(ctx)
	if err != nil {
		mapped := mapError(err)
		if errors.Is(mapped, store.ErrAlreadyExists) {
			existing, qErr := s.client.TriggerFire.Query().
				Where(triggerfire.EventIDEQ(eid), triggerfire.TriggerIDEQ(tid)).
				Only(ctx)
			if qErr != nil {
				return false, nil, mapError(qErr)
			}
			return false, entTriggerFireToStore(existing), nil
		}
		return false, nil, mapped
	}
	*fire = *entTriggerFireToStore(created)
	return true, nil, nil
}

func (s *TriggerStore) FinishTriggerFire(ctx context.Context, id, status, resultRef, errMsg string, finishedAt time.Time) (bool, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return false, err
	}
	u := s.client.TriggerFire.Update().
		Where(triggerfire.IDEQ(uid), triggerfire.StatusEQ(store.TriggerFireStatusFiring)).
		SetStatus(status).
		SetFinishedAt(finishedAt.UTC())
	if resultRef != "" {
		u.SetResultRef(resultRef)
	} else {
		u.ClearResultRef()
	}
	if errMsg != "" {
		u.SetError(errMsg)
	} else {
		u.ClearError()
	}
	affected, err := u.Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return affected > 0, nil
}

func (s *TriggerStore) GetTriggerFire(ctx context.Context, id string) (*store.TriggerFire, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	e, err := s.client.TriggerFire.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entTriggerFireToStore(e), nil
}

func (s *TriggerStore) ListTriggerFires(ctx context.Context, filter store.TriggerFireFilter, opts store.ListOptions) (*store.ListResult[store.TriggerFire], error) {
	query := s.client.TriggerFire.Query()
	if filter.ProjectID != "" {
		pid, err := parseUUID(filter.ProjectID)
		if err != nil {
			return nil, err
		}
		query.Where(triggerfire.ProjectIDEQ(pid))
	}
	if filter.TriggerID != "" {
		tid, err := parseUUID(filter.TriggerID)
		if err != nil {
			return nil, err
		}
		query.Where(triggerfire.TriggerIDEQ(tid))
	}
	if filter.EventID != "" {
		eid, err := parseUUID(filter.EventID)
		if err != nil {
			return nil, err
		}
		query.Where(triggerfire.EventIDEQ(eid))
	}
	if filter.Status != "" {
		query.Where(triggerfire.StatusEQ(filter.Status))
	}

	totalCount, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}

	var (
		cursorCreated time.Time
		cursorID      uuid.UUID
	)
	if opts.Cursor != "" {
		cursorCreated, cursorID, err = decodeCursor(opts.Cursor)
		if err != nil {
			return nil, fmt.Errorf("invalid cursor: %w", err)
		}
		cursorCreated = cursorCreated.UTC()
		query.Where(triggerfire.Or(
			triggerfire.CreatedLT(cursorCreated),
			triggerfire.And(
				triggerfire.CreatedEQ(cursorCreated),
				triggerfire.IDLT(cursorID),
			),
		))
	}

	limit := clampLimit(opts.Limit)
	entities, err := query.
		Order(triggerfire.ByCreated(entsql.OrderDesc())).
		Order(triggerfire.ByID(entsql.OrderDesc())).
		Limit(limit + 1).
		All(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]store.TriggerFire, 0, len(entities))
	for _, e := range entities {
		items = append(items, *entTriggerFireToStore(e))
	}

	result := &store.ListResult[store.TriggerFire]{TotalCount: totalCount}
	if len(items) > limit {
		result.Items = items[:limit]
		last := items[limit-1]
		if opts.Cursor == "" || cursorAdvancesTimeID(last.CreatedAt, last.ID, cursorCreated, cursorID) {
			result.NextCursor = encodeCursor(last.CreatedAt.UTC(), last.ID)
		}
	} else {
		result.Items = items
	}
	return result, nil
}

func (s *TriggerStore) HasLiveTriggerFireForSubject(ctx context.Context, triggerID, subject string, now time.Time) (bool, error) {
	tid, err := parseUUID(triggerID)
	if err != nil {
		return false, err
	}
	return s.client.TriggerFire.Query().
		Where(
			triggerfire.TriggerIDEQ(tid),
			triggerfire.SubjectEQ(subject),
			triggerfire.StatusEQ(store.TriggerFireStatusFiring),
			triggerfire.LeaseUntilGT(now.UTC()),
		).
		Exist(ctx)
}

func (s *TriggerStore) AgentHasFiredTriggerForEvent(ctx context.Context, eventID, agentID string) (bool, error) {
	eid, err := parseGetID(eventID)
	if err != nil {
		return false, err
	}
	if agentID == "" {
		return false, nil
	}
	return s.client.TriggerFire.Query().
		Where(
			triggerfire.EventIDEQ(eid),
			predicate.TriggerFire(func(sel *entsql.Selector) {
				d := sel.Dialect()
				tr := entsql.Dialect(d).Table(trigger.Table).As("tr")
				conds := append(
					[]*entsql.Predicate{entsql.ColumnsEQ(tr.C(trigger.FieldID), sel.C(triggerfire.FieldTriggerID))},
					triggerRecordedAuthoritySQLPredicates(tr, store.DelegationPrincipalAgent, agentID)...,
				)
				sub := entsql.Dialect(d).Select(tr.C(trigger.FieldID)).From(tr).Where(entsql.And(conds...))
				sel.Where(entsql.Exists(sub))
			}),
		).
		Exist(ctx)
}

func (s *TriggerStore) ReapExpiredTriggerFires(ctx context.Context, now time.Time, limit int) (int, error) {
	now = now.UTC()
	q := s.client.TriggerFire.Query().
		Where(
			triggerfire.StatusEQ(store.TriggerFireStatusFiring),
			triggerfire.LeaseUntilLTE(now),
		).
		Order(triggerfire.ByLeaseUntil())
	if limit > 0 {
		q = q.Limit(limit)
	}
	expired, err := q.Select(triggerfire.FieldID, triggerfire.FieldTriggerID).All(ctx)
	if err != nil {
		return 0, err
	}
	reaped := 0
	for _, f := range expired {
		affected, uErr := s.client.TriggerFire.Update().
			Where(triggerfire.IDEQ(f.ID), triggerfire.StatusEQ(store.TriggerFireStatusFiring)).
			SetStatus(store.TriggerFireStatusFailed).
			SetError("hub restarted during fire").
			SetFinishedAt(now).
			Save(ctx)
		if uErr != nil {
			return reaped, mapError(uErr)
		}
		if affected > 0 {
			reaped++
			_ = s.RecordTriggerFire(ctx, f.TriggerID.String(), now, store.TriggerFireStatusFailed, "hub restarted during fire")
		}
	}
	return reaped, nil
}

func (s *TriggerStore) PurgeOldTriggerHistory(ctx context.Context, olderThan time.Time) (int, int, error) {
	cutoff := olderThan.UTC()
	firesDeleted, err := s.client.TriggerFire.Delete().
		Where(
			triggerfire.StatusNEQ(store.TriggerFireStatusFiring),
			triggerfire.CreatedLT(cutoff),
		).
		Exec(ctx)
	if err != nil {
		return 0, 0, mapError(err)
	}
	eventsDeleted, err := s.client.TriggerEvent.Delete().
		Where(
			triggerevent.StatusNEQ(store.TriggerEventStatusPending),
			triggerevent.CreatedLT(cutoff),
		).
		Exec(ctx)
	if err != nil {
		return 0, firesDeleted, mapError(err)
	}
	return eventsDeleted, firesDeleted, nil
}
