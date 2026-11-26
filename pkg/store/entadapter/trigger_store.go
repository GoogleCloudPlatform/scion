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
	"strconv"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/durablequeueitem"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/scheduledexecrun"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/triggerevent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/triggerfire"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/triggersubjectclaim"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/webhookendpoint"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// TriggerStore implements store.TriggerStore over Ent.
type TriggerStore struct {
	client *ent.Client
	inTx   bool
	// beforeCoalescePromoteHook is an optional test-only hook invoked inside
	// ClaimTriggerFireCoalescing right before the coalescing->firing CAS to
	// deterministically exercise concurrent reaper transitions.
	beforeCoalescePromoteHook func(ctx context.Context, client *ent.Client, fireID uuid.UUID)
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

func marshalWebhookRoutingHints(h *store.WebhookRoutingHints) (string, error) {
	if h == nil || h.IsEmpty() {
		return "", nil
	}
	b, err := json.Marshal(h)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func unmarshalWebhookRoutingHints(raw string) *store.WebhookRoutingHints {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return nil
	}
	var h store.WebhookRoutingHints
	if err := json.Unmarshal([]byte(raw), &h); err != nil {
		return nil
	}
	if h.IsEmpty() {
		return nil
	}
	return &h
}

func entWebhookEndpointToStore(e *ent.WebhookEndpoint) *store.WebhookEndpoint {
	var boundAgentID string
	if e.BoundAgentID != nil {
		boundAgentID = e.BoundAgentID.String()
	}
	authMode := e.AuthMode
	if authMode == "" {
		authMode = store.WebhookAuthModeHMAC
	}
	return &store.WebhookEndpoint{
		ID:                  e.ID.String(),
		ProjectID:           e.ProjectID.String(),
		Name:                e.Name,
		Description:         e.Description,
		OwnerPrincipalKind:  e.OwnerPrincipalKind,
		OwnerPrincipalID:    e.OwnerPrincipalID,
		OwnerCredentialKind: e.OwnerCredentialKind,
		AuthMode:            authMode,
		AllowProjectAgents:  e.AllowProjectAgents,
		MaxBodyBytes:        e.MaxBodyBytes,
		RatePerSecond:       e.RatePerSecond,
		RateBurst:           e.RateBurst,
		ReplayWindowSeconds: e.ReplayWindowSeconds,
		SecretVersion:       e.SecretVersion,
		SecretRotatedAt:     e.SecretRotatedAt,
		ConfigVersion:       e.ConfigVersion,
		SignatureMode:       e.SignatureMode,
		RoutingHints:        unmarshalWebhookRoutingHints(e.RoutingHints),
		Status:              e.Status,
		BoundAgentID:        boundAgentID,
		BoundLifetime:       e.BoundLifetime,
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
	authMode := ep.AuthMode
	if authMode == "" {
		authMode = store.WebhookAuthModeHMAC
	}
	maxBodyBytes := ep.MaxBodyBytes
	if maxBodyBytes <= 0 {
		maxBodyBytes = store.DefaultWebhookMaxBodyBytes
	}
	replayWindow := ep.ReplayWindowSeconds
	if replayWindow <= 0 {
		replayWindow = store.DefaultWebhookReplayWindowSeconds
	}
	secretVersion := ep.SecretVersion
	if secretVersion <= 0 && authMode != store.WebhookAuthModeHubToken {
		secretVersion = 1
	}
	configVersion := ep.ConfigVersion
	if configVersion <= 0 {
		configVersion = 1
	}
	sigMode := ep.SignatureMode
	if sigMode == "" {
		sigMode = store.WebhookSignatureModeScionV1
	}
	status := ep.Status
	if status == "" {
		status = store.WebhookEndpointStatusActive
	}
	var boundAgentUID *uuid.UUID
	if ep.BoundAgentID != "" {
		parsed, pErr := parseUUID(ep.BoundAgentID)
		if pErr != nil {
			return pErr
		}
		boundAgentUID = &parsed
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	createdAt := now
	if !ep.CreatedAt.IsZero() {
		createdAt = ep.CreatedAt.UTC().Truncate(time.Microsecond)
	}
	updatedAt := createdAt
	if !ep.UpdatedAt.IsZero() {
		updatedAt = ep.UpdatedAt.UTC().Truncate(time.Microsecond)
	}

	routingHintsJSON, err := marshalWebhookRoutingHints(ep.RoutingHints)
	if err != nil {
		return store.ErrInvalidInput
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
		SetAuthMode(authMode).
		SetAllowProjectAgents(ep.AllowProjectAgents).
		SetMaxBodyBytes(maxBodyBytes).
		SetRatePerSecond(ep.RatePerSecond).
		SetRateBurst(ep.RateBurst).
		SetReplayWindowSeconds(replayWindow).
		SetSecretVersion(secretVersion).
		SetConfigVersion(configVersion).
		SetSignatureMode(sigMode).
		SetRoutingHints(routingHintsJSON).
		SetStatus(status).
		SetBoundLifetime(ep.BoundLifetime).
		SetReceiveCount(ep.ReceiveCount).
		SetRejectCount(ep.RejectCount).
		SetCreated(createdAt).
		SetUpdated(updatedAt)

	if boundAgentUID != nil {
		create.SetBoundAgentID(*boundAgentUID)
	}
	if ep.SecretRotatedAt != nil {
		create.SetSecretRotatedAt(ep.SecretRotatedAt.UTC().Truncate(time.Microsecond))
	}
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
	if filter.BoundAgentID != "" {
		bid, err := parseUUID(filter.BoundAgentID)
		if err != nil {
			return nil, err
		}
		query.Where(webhookendpoint.BoundAgentIDEQ(bid))
	}
	if filter.VisibleToAgentID != "" {
		ownerPred := webhookendpoint.And(
			webhookendpoint.OwnerPrincipalKindEQ(store.DelegationPrincipalAgent),
			webhookendpoint.OwnerPrincipalIDEQ(filter.VisibleToAgentID),
		)
		if aid, err := uuid.Parse(filter.VisibleToAgentID); err == nil {
			query.Where(webhookendpoint.Or(ownerPred, webhookendpoint.BoundAgentIDEQ(aid)))
		} else {
			query.Where(ownerPred)
		}
	}

	var (
		totalCount int
		err        error
	)
	if !opts.SkipTotalCount {
		totalCount, err = query.Clone().Count(ctx)
		if err != nil {
			return nil, err
		}
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

func (s *TriggerStore) CountBoundWebhookEndpoints(ctx context.Context, projectID, agentID string) (int, error) {
	pid, err := parseUUID(projectID)
	if err != nil {
		return 0, err
	}
	aid, err := parseUUID(agentID)
	if err != nil {
		return 0, err
	}
	return s.client.WebhookEndpoint.Query().
		Where(webhookendpoint.ProjectIDEQ(pid), webhookendpoint.BoundAgentIDEQ(aid)).
		Count(ctx)
}

func (s *TriggerStore) UpdateWebhookEndpoint(ctx context.Context, ep *store.WebhookEndpoint, fields store.WebhookEndpointFieldMask) error {
	if ep == nil || ep.ID == "" {
		return store.ErrInvalidInput
	}
	uid, err := parseUUID(ep.ID)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	u := s.client.WebhookEndpoint.UpdateOneID(uid).
		AddConfigVersion(1).
		SetUpdated(now)
	if fields.Description {
		u.SetDescription(ep.Description)
	}
	if fields.Status {
		u.SetStatus(ep.Status)
	}
	if fields.Binding {
		if ep.BoundAgentID == "" {
			u.ClearBoundAgentID()
		} else {
			aid, pErr := parseUUID(ep.BoundAgentID)
			if pErr != nil {
				return pErr
			}
			u.SetBoundAgentID(aid)
		}
		u.SetBoundLifetime(ep.BoundLifetime)
	}
	if fields.AuthMode {
		u.SetAuthMode(ep.AuthMode)
	}
	if fields.AllowProjectAgents {
		u.SetAllowProjectAgents(ep.AllowProjectAgents)
	}
	if fields.SignatureMode {
		u.SetSignatureMode(ep.SignatureMode)
	}
	if fields.RoutingHints {
		rhJSON, rhErr := marshalWebhookRoutingHints(ep.RoutingHints)
		if rhErr != nil {
			return store.ErrInvalidInput
		}
		u.SetRoutingHints(rhJSON)
	}
	if fields.MaxBodyBytes {
		u.SetMaxBodyBytes(ep.MaxBodyBytes)
	}
	if fields.RatePerSecond {
		u.SetRatePerSecond(ep.RatePerSecond)
	}
	if fields.RateBurst {
		u.SetRateBurst(ep.RateBurst)
	}
	if fields.ReplayWindowSeconds {
		u.SetReplayWindowSeconds(ep.ReplayWindowSeconds)
	}
	updated, err := u.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	*ep = *entWebhookEndpointToStore(updated)
	return nil
}

func (s *TriggerStore) RotateWebhookEndpointSecret(ctx context.Context, projectID, id string, nextVersion int, rotatedAt time.Time) (*store.WebhookEndpoint, error) {
	pid, err := parseGetID(projectID)
	if err != nil {
		return nil, err
	}
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	if nextVersion < 1 {
		return nil, store.ErrInvalidInput
	}
	ts := rotatedAt.UTC().Truncate(time.Microsecond)
	u := s.client.WebhookEndpoint.Update().
		Where(
			webhookendpoint.IDEQ(uid),
			webhookendpoint.ProjectIDEQ(pid),
			webhookendpoint.SecretVersionEQ(nextVersion-1),
		).
		SetSecretVersion(nextVersion).
		AddConfigVersion(1).
		SetUpdated(ts)
	if nextVersion >= 2 {
		u = u.SetSecretRotatedAt(ts)
	}
	n, err := u.Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	if n == 0 {
		return nil, store.ErrNotFound
	}
	updated, err := s.client.WebhookEndpoint.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entWebhookEndpointToStore(updated), nil
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

func (s *TriggerStore) DeleteWebhookEndpointForBoundAgent(ctx context.Context, projectID, id, boundAgentID string) ([]string, []string, []string, error) {
	pid, err := parseGetID(projectID)
	if err != nil {
		return nil, nil, nil, err
	}
	uid, err := parseGetID(id)
	if err != nil {
		return nil, nil, nil, err
	}
	agentUID, err := parseGetID(boundAgentID)
	if err != nil {
		return nil, nil, nil, err
	}

	var (
		deletedTriggerIDs []string
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
		if ep.ProjectID != pid || ep.BoundAgentID == nil || *ep.BoundAgentID != agentUID || ep.BoundLifetime != store.WebhookBoundLifetimeAgent {
			return store.ErrNotFound
		}
		liveAgent, aErr := client.Agent.Query().
			Where(
				agent.IDEQ(agentUID),
				agent.DeletedAtIsNil(),
			).
			Exist(ctx)
		if aErr != nil {
			return aErr
		}
		if liveAgent {
			return store.ErrNotFound
		}

		// 1. Partition dependent triggers on this endpoint:
		//    - Triggers authored by the bound agent are deleted (Stage 2 §4.2).
		//    - Triggers authored by anyone else are paused with source_deleted.
		deps, qErr := client.Trigger.Query().
			Where(
				trigger.ProjectIDEQ(pid),
				trigger.SourceKindEQ(store.TriggerSourceKindWebhook),
				trigger.SourceIDEQ(uid.String()),
			).
			All(ctx)
		if qErr != nil {
			return qErr
		}
		var (
			delIDs   []uuid.UUID
			pauseIDs []uuid.UUID
		)
		for _, tr := range deps {
			if tr.InitiatorPrincipalKind != nil && *tr.InitiatorPrincipalKind == store.DelegationPrincipalAgent &&
				tr.InitiatorPrincipalID != nil && *tr.InitiatorPrincipalID == boundAgentID {
				delIDs = append(delIDs, tr.ID)
				deletedTriggerIDs = append(deletedTriggerIDs, tr.ID.String())
			} else if tr.Status == store.TriggerStatusActive || tr.StatusReason == store.TriggerStatusReasonNone {
				pauseIDs = append(pauseIDs, tr.ID)
				pausedTriggerIDs = append(pausedTriggerIDs, tr.ID.String())
			}
		}
		if len(delIDs) > 0 {
			if _, dErr := client.Trigger.Delete().Where(trigger.IDIn(delIDs...)).Exec(ctx); dErr != nil {
				return mapError(dErr)
			}
		}
		if len(pauseIDs) > 0 {
			now := time.Now().UTC().Truncate(time.Microsecond)
			if _, uErr := client.Trigger.Update().
				Where(trigger.IDIn(pauseIDs...)).
				SetStatus(store.TriggerStatusPaused).
				SetStatusReason(store.TriggerStatusReasonSourceDeleted).
				SetUpdated(now).
				Save(ctx); uErr != nil {
				return mapError(uErr)
			}
		}

		// 2. Delete the endpoint's hub-scoped internal secret rows.
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
		return nil, nil, nil, err
	}
	return deletedTriggerIDs, pausedTriggerIDs, deletedSecretKeys, nil
}

func (s *TriggerStore) UnbindWebhookEndpointForAgent(ctx context.Context, projectID, id, boundAgentID string) (bool, error) {
	pid, err := parseGetID(projectID)
	if err != nil {
		return false, err
	}
	uid, err := parseGetID(id)
	if err != nil {
		return false, err
	}
	agentUID, err := parseGetID(boundAgentID)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	n, err := s.client.WebhookEndpoint.Update().
		Where(
			webhookendpoint.IDEQ(uid),
			webhookendpoint.ProjectIDEQ(pid),
			webhookendpoint.BoundAgentIDEQ(agentUID),
			webhookendpoint.BoundLifetimeEQ(store.WebhookBoundLifetimePersistent),
			predicate.WebhookEndpoint(func(sel *entsql.Selector) {
				d := sel.Dialect()
				a := entsql.Dialect(d).Table(agent.Table).As("ag")
				sub := entsql.Dialect(d).
					Select(a.C(agent.FieldID)).
					From(a).
					Where(entsql.And(
						entsql.ColumnsEQ(a.C(agent.FieldID), sel.C(webhookendpoint.FieldBoundAgentID)),
						entsql.IsNull(a.C(agent.FieldDeletedAt)),
					))
				sel.Where(entsql.Not(entsql.Exists(sub)))
			}),
		).
		ClearBoundAgentID().
		SetBoundLifetime(store.WebhookBoundLifetimeNone).
		AddConfigVersion(1).
		SetUpdated(now).
		Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return n > 0, nil
}

// parseWebhookEndpointSecretKey parses "webhook_endpoint.<projectID>.<endpointID>.v<N>".
func parseWebhookEndpointSecretKey(key string) (endpointID uuid.UUID, version int, ok bool) {
	rest, found := strings.CutPrefix(key, "webhook_endpoint.")
	if !found {
		return uuid.Nil, 0, false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return uuid.Nil, 0, false
	}
	epUID, err := uuid.Parse(parts[1])
	if err != nil {
		return uuid.Nil, 0, false
	}
	verStr, hasV := strings.CutPrefix(parts[2], "v")
	if !hasV || verStr == "" {
		return uuid.Nil, 0, false
	}
	ver, err := strconv.Atoi(verStr)
	if err != nil || ver < 1 {
		return uuid.Nil, 0, false
	}
	return epUID, ver, true
}

func (s *TriggerStore) PurgeExpiredRotatedWebhookSecrets(ctx context.Context, now time.Time, graceWindow time.Duration) (int, error) {
	now = now.UTC()
	rows, err := s.client.Secret.Query().
		Where(
			secret.ScopeEQ(store.ScopeHub),
			secret.SecretTypeEQ(secret.SecretTypeInternal),
			secret.KeyHasPrefix("webhook_endpoint."),
		).
		Select(secret.FieldID, secret.FieldKey).
		All(ctx)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}

	type parsedRow struct {
		secretID   uuid.UUID
		endpointID uuid.UUID
		version    int
	}
	parsed := make([]parsedRow, 0, len(rows))
	epIDSet := make(map[uuid.UUID]struct{})
	for _, r := range rows {
		epUID, ver, ok := parseWebhookEndpointSecretKey(r.Key)
		if !ok {
			continue
		}
		parsed = append(parsed, parsedRow{secretID: r.ID, endpointID: epUID, version: ver})
		epIDSet[epUID] = struct{}{}
	}
	if len(epIDSet) == 0 {
		return 0, nil
	}

	epIDs := make([]uuid.UUID, 0, len(epIDSet))
	for id := range epIDSet {
		epIDs = append(epIDs, id)
	}
	eps, err := s.client.WebhookEndpoint.Query().
		Where(webhookendpoint.IDIn(epIDs...)).
		Select(webhookendpoint.FieldID, webhookendpoint.FieldSecretVersion, webhookendpoint.FieldSecretRotatedAt).
		All(ctx)
	if err != nil {
		return 0, err
	}
	epByID := make(map[uuid.UUID]*ent.WebhookEndpoint, len(eps))
	for _, ep := range eps {
		epByID[ep.ID] = ep
	}

	var toDelete []uuid.UUID
	for _, r := range parsed {
		ep, ok := epByID[r.endpointID]
		if !ok || r.version >= ep.SecretVersion {
			continue
		}
		// Delete any version strictly older than secret_version - 1, or
		// secret_version - 1 once the rotation grace window has elapsed.
		if r.version < ep.SecretVersion-1 ||
			ep.SecretRotatedAt == nil ||
			now.After(ep.SecretRotatedAt.UTC().Add(graceWindow)) {
			toDelete = append(toDelete, r.secretID)
		}
	}
	if len(toDelete) == 0 {
		return 0, nil
	}
	n, err := s.client.Secret.Delete().Where(secret.IDIn(toDelete...)).Exec(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

func (s *TriggerStore) ListOrphanedBoundWebhookEndpoints(ctx context.Context, limit int) ([]store.WebhookEndpoint, error) {
	if limit <= 0 {
		limit = 100
	}
	entities, err := s.client.WebhookEndpoint.Query().
		Where(
			webhookendpoint.BoundLifetimeIn(store.WebhookBoundLifetimeAgent, store.WebhookBoundLifetimePersistent),
			webhookendpoint.BoundAgentIDNotNil(),
			predicate.WebhookEndpoint(func(sel *entsql.Selector) {
				d := sel.Dialect()
				a := entsql.Dialect(d).Table(agent.Table).As("ag")
				sub := entsql.Dialect(d).
					Select(a.C(agent.FieldID)).
					From(a).
					Where(entsql.And(
						entsql.ColumnsEQ(a.C(agent.FieldID), sel.C(webhookendpoint.FieldBoundAgentID)),
						entsql.IsNull(a.C(agent.FieldDeletedAt)),
					))
				sel.Where(entsql.Not(entsql.Exists(sub)))
			}),
		).
		Order(webhookendpoint.ByCreated(entsql.OrderAsc()), webhookendpoint.ByID(entsql.OrderAsc())).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]store.WebhookEndpoint, 0, len(entities))
	for _, e := range entities {
		out = append(out, *entWebhookEndpointToStore(e))
	}
	return out, nil
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
		ID:                      e.ID.String(),
		ProjectID:               e.ProjectID.String(),
		Name:                    e.Name,
		Description:             e.Description,
		SourceKind:              e.SourceKind,
		SourceID:                e.SourceID,
		MatchTypes:              unmarshalMatchTypes(e.MatchTypes),
		MatchDialect:            e.MatchDialect,
		MatchRule:               e.MatchRule,
		MatchRules:              matchRules,
		MatchForm:               e.MatchForm,
		MatchRulesCorrupt:       matchRulesCorrupt,
		ActionType:              e.ActionType,
		ActionPayload:           e.ActionPayload,
		SubjectDialect:          e.SubjectDialect,
		SubjectRule:             e.SubjectRule,
		SubjectMode:             e.SubjectMode,
		CoalesceWindowSeconds:   e.CoalesceWindowSeconds,
		MaxFiresPerMinute:       e.MaxFiresPerMinute,
		MaxConcurrentDispatches: e.MaxConcurrentDispatches,
		Status:                  e.Status,
		StatusReason:            e.StatusReason,
		LastFireAt:              e.LastFireAt,
		LastFireStatus:          e.LastFireStatus,
		LastFireError:           e.LastFireError,
		FireCount:               e.FireCount,
		ErrorCount:              e.ErrorCount,
		CreatedBy:               e.CreatedBy,
		CreatedAt:               e.Created,
		UpdatedAt:               e.Updated,
		Fixture:                 fixtureProvenanceFromColumns(e.FixtureRootID, e.FixtureIssuerID),
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
	if len(filter.SourceIDs) > 0 {
		query.Where(trigger.SourceIDIn(filter.SourceIDs...))
	}
	if filter.Status != "" {
		query.Where(trigger.StatusEQ(filter.Status))
	}
	if filter.Name != "" {
		query.Where(trigger.NameEQ(filter.Name))
	}
	if len(filter.ActionTypes) > 0 {
		query.Where(trigger.ActionTypeIn(filter.ActionTypes...))
	}
	if filter.TargetAgentID != "" {
		// Pre-filter in SQL on action_payload containing the target agent ID;
		// callers still decode action_payload to confirm the exact agentId field.
		query.Where(trigger.ActionPayloadContains(filter.TargetAgentID))
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
		SetCoalesceWindowSeconds(tr.CoalesceWindowSeconds).
		SetMaxFiresPerMinute(tr.MaxFiresPerMinute).
		SetMaxConcurrentDispatches(tr.MaxConcurrentDispatches).
		SetStatus(status).
		SetFireCount(tr.FireCount).
		SetErrorCount(tr.ErrorCount).
		SetCreated(createdAt).
		SetUpdated(updatedAt)

	if tr.Description != "" {
		create.SetDescription(tr.Description)
	}
	if tr.MatchForm != "" {
		create.SetMatchForm(tr.MatchForm)
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

	var (
		totalCount int
		err        error
	)
	if !opts.SkipTotalCount {
		totalCount, err = query.Clone().Count(ctx)
		if err != nil {
			return nil, err
		}
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

const triggerProjectsBatchSize = 500

func (s *TriggerStore) ProjectsWithTriggersBySourceKind(ctx context.Context, projectIDs []string, sourceKind string) (map[string]bool, error) {
	out := make(map[string]bool)
	if len(projectIDs) == 0 || sourceKind == "" {
		return out, nil
	}

	seen := make(map[uuid.UUID]struct{}, len(projectIDs))
	pids := make([]uuid.UUID, 0, len(projectIDs))
	for _, raw := range projectIDs {
		uid, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		if _, dup := seen[uid]; dup {
			continue
		}
		seen[uid] = struct{}{}
		pids = append(pids, uid)
	}
	if len(pids) == 0 {
		return out, nil
	}

	for start := 0; start < len(pids); start += triggerProjectsBatchSize {
		end := start + triggerProjectsBatchSize
		if end > len(pids) {
			end = len(pids)
		}
		rows, err := s.client.Trigger.Query().
			Where(
				trigger.ProjectIDIn(pids[start:end]...),
				trigger.SourceKindEQ(sourceKind),
			).
			Unique(true).
			Select(trigger.FieldProjectID).
			All(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out[r.ProjectID.String()] = true
		}
	}
	return out, nil
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
	if fields.MatchForm {
		if tr.MatchForm != "" {
			update = update.SetMatchForm(tr.MatchForm)
		} else {
			update = update.ClearMatchForm()
		}
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
	if fields.CoalesceWindowSeconds {
		update = update.SetCoalesceWindowSeconds(tr.CoalesceWindowSeconds)
	}
	if fields.MaxFiresPerMinute {
		update = update.SetMaxFiresPerMinute(tr.MaxFiresPerMinute)
	}
	if fields.MaxConcurrentDispatches {
		update = update.SetMaxConcurrentDispatches(tr.MaxConcurrentDispatches)
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
	if fields.MatchForm {
		tr.MatchForm = updated.MatchForm
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
	if status == store.TriggerFireStatusFailed || status == store.TriggerFireStatusRefused || status == store.TriggerFireStatusExpired {
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
		if _, dErr := client.ScheduledExecRun.Delete().Where(scheduledexecrun.ProjectIDEQ(pid), scheduledexecrun.FireIDNotNil()).Exec(ctx); dErr != nil {
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
		PayloadRef:   e.PayloadRef,
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
		SetPayloadRef(evt.PayloadRef).
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
		ownerPred := webhookendpoint.And(
			webhookendpoint.OwnerPrincipalKindEQ(store.DelegationPrincipalAgent),
			webhookendpoint.OwnerPrincipalIDEQ(filter.VisibleToAgentID),
		)
		agentUID, agentUIDErr := uuid.Parse(filter.VisibleToAgentID)
		epQ := s.client.WebhookEndpoint.Query()
		if agentUIDErr == nil {
			epQ.Where(webhookendpoint.Or(ownerPred, webhookendpoint.BoundAgentIDEQ(agentUID)))
		} else {
			epQ.Where(ownerPred)
		}
		trQ := s.client.Trigger.Query().Where(triggerRecordedAuthorityIs(store.DelegationPrincipalAgent, filter.VisibleToAgentID))
		if hasPID {
			epQ.Where(webhookendpoint.ProjectIDEQ(pid))
			trQ.Where(trigger.ProjectIDEQ(pid))
		}
		hasVisibleEPs, err := epQ.Exist(ctx)
		if err != nil {
			return nil, err
		}
		hasOwnedTrs, err := trQ.Exist(ctx)
		if err != nil {
			return nil, err
		}
		var visPreds []predicate.TriggerEvent
		if hasVisibleEPs {
			agentID := filter.VisibleToAgentID
			visPreds = append(visPreds, triggerevent.And(
				triggerevent.SourceKindEQ(store.TriggerSourceKindWebhook),
				predicate.TriggerEvent(func(sel *entsql.Selector) {
					d := sel.Dialect()
					ep := entsql.Dialect(d).Table(webhookendpoint.Table).As("ep")
					ownerSQL := entsql.And(
						entsql.EQ(ep.C(webhookendpoint.FieldOwnerPrincipalKind), store.DelegationPrincipalAgent),
						entsql.EQ(ep.C(webhookendpoint.FieldOwnerPrincipalID), agentID),
					)
					epAccessPred := ownerSQL
					if agentUIDErr == nil {
						epAccessPred = entsql.Or(ownerSQL, entsql.EQ(ep.C(webhookendpoint.FieldBoundAgentID), agentUID))
					}
					conds := []*entsql.Predicate{
						entsql.P(func(b *entsql.Builder) {
							b.WriteString("CAST(").WriteString(ep.C(webhookendpoint.FieldID)).WriteString(" AS TEXT) = ").WriteString(sel.C(triggerevent.FieldSourceID))
						}),
						epAccessPred,
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
		// Stage 2 §5.4: an agent dispatched by a trigger fire (result_ref == agent.ID)
		// may also read/list the event that created it (indexed on result_ref).
		agentID := filter.VisibleToAgentID
		visPreds = append(visPreds, predicate.TriggerEvent(func(sel *entsql.Selector) {
			d := sel.Dialect()
			tf := entsql.Dialect(d).Table(triggerfire.Table).As("tf_res")
			conds := []*entsql.Predicate{
				entsql.ColumnsEQ(tf.C(triggerfire.FieldEventID), sel.C(triggerevent.FieldID)),
				entsql.EQ(tf.C(triggerfire.FieldResultRef), agentID),
			}
			if hasPID {
				conds = append(conds, entsql.EQ(tf.C(triggerfire.FieldProjectID), pid))
			}
			sub := entsql.Dialect(d).
				Select(tf.C(triggerfire.FieldID)).
				From(tf).
				Where(entsql.And(conds...))
			sel.Where(entsql.Exists(sub))
		}))
		query.Where(triggerevent.Or(visPreds...))
	}

	var (
		totalCount int
		err        error
	)
	if !opts.SkipTotalCount {
		totalCount, err = query.Clone().Count(ctx)
		if err != nil {
			return nil, err
		}
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

func (s *TriggerStore) SumTriggerEventOverflowBytes(ctx context.Context, projectID string, since time.Time) (int64, error) {
	pid, err := parseUUID(projectID)
	if err != nil {
		return 0, err
	}
	var rows []struct {
		SumPayloadBytes *int64 `sql:"sum_payload_bytes"`
	}
	err = s.client.TriggerEvent.Query().
		Where(
			triggerevent.ProjectIDEQ(pid),
			triggerevent.PayloadRefGT(""),
			triggerevent.CreatedGTE(since.UTC()),
		).
		Aggregate(ent.As(ent.Sum(triggerevent.FieldPayloadBytes), "sum_payload_bytes")).
		Scan(ctx, &rows)
	if err != nil {
		return 0, mapError(err)
	}
	if len(rows) == 0 || rows[0].SumPayloadBytes == nil {
		return 0, nil
	}
	return *rows[0].SumPayloadBytes, nil
}

const existingPayloadRefsChunkSize = 500

func (s *TriggerStore) ExistingTriggerEventPayloadRefs(ctx context.Context, projectID string, refs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(refs))
	if len(refs) == 0 {
		return out, nil
	}
	pid, err := parseUUID(projectID)
	if err != nil {
		return nil, err
	}
	for start := 0; start < len(refs); start += existingPayloadRefsChunkSize {
		end := min(start+existingPayloadRefsChunkSize, len(refs))
		chunk := refs[start:end]
		found, err := s.client.TriggerEvent.Query().
			Where(
				triggerevent.ProjectIDEQ(pid),
				triggerevent.PayloadRefIn(chunk...),
			).
			Select(triggerevent.FieldPayloadRef).
			Strings(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		for _, ref := range found {
			if ref != "" {
				out[ref] = true
			}
		}
	}
	return out, nil
}

// UpdateTriggerEventAttributes is enrichment-only (Stage 3 §5.5): it updates
// attributes and lists on a pending trigger_events row before rule evaluation
// and never modifies payload, type, subject, headers, dedup_key, or body_sha256.
func (s *TriggerStore) UpdateTriggerEventAttributes(ctx context.Context, id string, attributes map[string]string, lists map[string][]string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	attrsJSON, err := marshalStringMap(attributes)
	if err != nil {
		return err
	}
	listsJSON, err := marshalStringListMap(lists)
	if err != nil {
		return err
	}
	affected, err := s.client.TriggerEvent.Update().
		Where(triggerevent.IDEQ(uid), triggerevent.StatusEQ(store.TriggerEventStatusPending)).
		SetAttributes(attrsJSON).
		SetLists(listsJSON).
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
		ID:             e.ID.String(),
		ProjectID:      e.ProjectID.String(),
		TriggerID:      e.TriggerID.String(),
		EventID:        e.EventID.String(),
		Subject:        e.Subject,
		ActionType:     e.ActionType,
		Status:         e.Status,
		Error:          e.Error,
		ResultRef:      e.ResultRef,
		WaitUntil:      e.WaitUntil,
		Attempts:       e.Attempts,
		MatchedThrough: e.MatchedThrough,
		DecidedDialect: e.DecidedDialect,
		RuleTrace:      e.RuleTrace,
		ExecutorKind:   e.ExecutorKind,
		ExecutorID:     e.ExecutorID,
		LeaseUntil:     e.LeaseUntil,
		NotBefore:      e.NotBefore,
		DeferredAt:     e.DeferredAt,
		StartedAt:      e.StartedAt,
		FinishedAt:     e.FinishedAt,
		CreatedAt:      e.Created,
	}
	if e.TargetAgentID != nil {
		tf.TargetAgentID = e.TargetAgentID.String()
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

func parseTriggerFireUUIDs(fire *store.TriggerFire) (uid, pid, tid, eid uuid.UUID, err error) {
	if fire == nil || fire.ID == "" || fire.ProjectID == "" || fire.TriggerID == "" ||
		fire.EventID == "" || fire.ActionType == "" || fire.LeaseUntil.IsZero() {
		return uuid.Nil, uuid.Nil, uuid.Nil, uuid.Nil, store.ErrInvalidInput
	}
	uid, err = parseUUID(fire.ID)
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, uuid.Nil, err
	}
	pid, err = parseUUID(fire.ProjectID)
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, uuid.Nil, err
	}
	tid, err = parseUUID(fire.TriggerID)
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, uuid.Nil, err
	}
	eid, err = parseUUID(fire.EventID)
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, uuid.Nil, err
	}
	return uid, pid, tid, eid, nil
}

func buildTriggerFireCreate(client *ent.Client, uid, pid, tid, eid uuid.UUID, fire *store.TriggerFire, defaultNow time.Time) (*ent.TriggerFireCreate, error) {
	startedAt := defaultNow
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

	create := client.TriggerFire.Create().
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
	if fire.MatchedThrough != "" {
		create.SetMatchedThrough(fire.MatchedThrough)
	}
	if fire.DecidedDialect != "" {
		create.SetDecidedDialect(fire.DecidedDialect)
	}
	if fire.RuleTrace != "" {
		create.SetRuleTrace(fire.RuleTrace)
	}
	if fire.TargetAgentID != "" {
		taid, err := parseUUID(fire.TargetAgentID)
		if err != nil {
			return nil, err
		}
		create.SetTargetAgentID(taid)
	}
	if fire.NotBefore != nil {
		create.SetNotBefore(fire.NotBefore.UTC())
	}
	if fire.DeferredAt != nil {
		create.SetDeferredAt(fire.DeferredAt.UTC())
	}
	if fire.WaitUntil != nil {
		create.SetWaitUntil(fire.WaitUntil.UTC().Truncate(time.Microsecond))
	}
	if fire.Attempts > 0 {
		create.SetAttempts(fire.Attempts)
	}
	if fire.FinishedAt != nil {
		create.SetFinishedAt(fire.FinishedAt.UTC())
	}
	setTriggerFireInitiatorAttribution(create, fire.InitiatorAttribution)
	return create, nil
}

func (s *TriggerStore) ClaimTriggerFire(ctx context.Context, fire *store.TriggerFire) (bool, *store.TriggerFire, error) {
	uid, pid, tid, eid, err := parseTriggerFireUUIDs(fire)
	if err != nil {
		return false, nil, err
	}
	now := time.Now().UTC()
	create, err := buildTriggerFireCreate(s.client, uid, pid, tid, eid, fire, now)
	if err != nil {
		return false, nil, err
	}
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

func (s *TriggerStore) ClaimTriggerFireSubjectSkip(ctx context.Context, fire *store.TriggerFire, skipError string, now time.Time) (bool, bool, *store.TriggerFire, error) {
	uid, pid, tid, eid, err := parseTriggerFireUUIDs(fire)
	if err != nil {
		return false, false, nil, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	var (
		claimed     bool
		skippedBusy bool
		existingOut *store.TriggerFire
	)
	txErr := s.withClientTx(ctx, func(client *ent.Client) error {
		if isPostgres(client) {
			var res sql.Result
			lockKey := fire.TriggerID + "\x00" + fire.Subject
			if lErr := client.Driver().Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)",
				[]any{int32(store.LockTriggerSubjectOverlap), store.StableProjectHash(lockKey)}, &res); lErr != nil {
				return fmt.Errorf("lock trigger subject overlap: %w", lErr)
			}
		}

		ex, qErr := client.TriggerFire.Query().
			Where(triggerfire.EventIDEQ(eid), triggerfire.TriggerIDEQ(tid)).
			Only(ctx)
		if qErr == nil {
			claimed = false
			skippedBusy = false
			existingOut = entTriggerFireToStore(ex)
			return nil
		}
		if !ent.IsNotFound(qErr) {
			return mapError(qErr)
		}

		busy, bErr := hasLiveTriggerFireForSubjectClient(ctx, client, tid, fire.Subject, uuid.Nil, true, now)
		if bErr != nil {
			return mapError(bErr)
		}

		row := *fire
		if busy {
			row.Status = store.TriggerFireStatusSkipped
			row.Error = skipError
			fin := now
			row.FinishedAt = &fin
			skippedBusy = true
		} else {
			if row.Status == "" {
				row.Status = store.TriggerFireStatusFiring
			}
			skippedBusy = false
		}

		create, bErr := buildTriggerFireCreate(client, uid, pid, tid, eid, &row, now)
		if bErr != nil {
			return bErr
		}
		created, cErr := create.Save(ctx)
		if cErr != nil {
			return mapError(cErr)
		}
		*fire = *entTriggerFireToStore(created)
		claimed = true
		return nil
	})
	if txErr != nil {
		if !s.inTx && errors.Is(txErr, store.ErrAlreadyExists) {
			ex, qErr := s.client.TriggerFire.Query().
				Where(triggerfire.EventIDEQ(eid), triggerfire.TriggerIDEQ(tid)).
				Only(ctx)
			if qErr != nil {
				return false, false, nil, mapError(qErr)
			}
			return false, false, entTriggerFireToStore(ex), nil
		}
		return false, false, nil, txErr
	}
	return claimed, skippedBusy, existingOut, nil
}

const (
	errReapedDuringCoalesce = "hub restarted during coalesce"
	errReapedDuringDefer    = "hub restarted during defer"
)

func pendingOrReapedFirePredicate() predicate.TriggerFire {
	return triggerfire.Or(
		triggerfire.StatusIn(store.TriggerFireStatusCoalescing, store.TriggerFireStatusDeferred),
		triggerfire.And(
			triggerfire.StatusEQ(store.TriggerFireStatusFailed),
			triggerfire.ErrorIn(errReapedDuringCoalesce, errReapedDuringDefer),
		),
	)
}

func isReapedPendingFire(ex *ent.TriggerFire) bool {
	return ex.Status == store.TriggerFireStatusFailed &&
		(ex.Error == errReapedDuringCoalesce || ex.Error == errReapedDuringDefer)
}

func (s *TriggerStore) ClaimTriggerFireCoalescing(
	ctx context.Context,
	fire *store.TriggerFire,
	coalesceError string,
	now, notBefore time.Time,
) (bool, bool, []store.TriggerFire, *store.TriggerFire, error) {
	uid, pid, tid, eid, err := parseTriggerFireUUIDs(fire)
	if err != nil {
		return false, false, nil, nil, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	notBefore = notBefore.UTC()
	leaseHorizon := fire.LeaseUntil.UTC().Sub(now)
	if leaseHorizon <= 0 {
		leaseHorizon = 10 * time.Minute
	}

	var (
		claimed    bool
		promoted   bool
		superseded []store.TriggerFire
		existing   *store.TriggerFire
	)
	txErr := s.withClientTx(ctx, func(client *ent.Client) error {
		if isPostgres(client) {
			var res sql.Result
			lockKey := fire.TriggerID + "\x00" + fire.Subject
			if lErr := client.Driver().Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)",
				[]any{int32(store.LockTriggerSubjectOverlap), store.StableProjectHash(lockKey)}, &res); lErr != nil {
				return fmt.Errorf("lock trigger subject overlap: %w", lErr)
			}
		}

		ex, qErr := client.TriggerFire.Query().
			Where(triggerfire.EventIDEQ(eid), triggerfire.TriggerIDEQ(tid)).
			Only(ctx)
		if qErr == nil {
			// Existing row for (event_id, trigger_id).
			if ex.Status == store.TriggerFireStatusCoalescing || ex.Status == store.TriggerFireStatusDeferred {
				if ex.NotBefore != nil && ex.NotBefore.UTC().After(now) {
					claimed = false
					promoted = false
					existing = entTriggerFireToStore(ex)
					return nil
				}
				if s.beforeCoalescePromoteHook != nil {
					s.beforeCoalescePromoteHook(ctx, client, ex.ID)
				}
				// Coalesce/defer window elapsed: CAS promote coalescing|deferred -> firing.
				affected, uErr := client.TriggerFire.Update().
					Where(triggerfire.IDEQ(ex.ID), triggerfire.StatusEQ(ex.Status)).
					SetStatus(store.TriggerFireStatusFiring).
					SetLeaseUntil(now.Add(leaseHorizon)).
					Save(ctx)
				if uErr != nil {
					return mapError(uErr)
				}
				fresh, fErr := client.TriggerFire.Get(ctx, ex.ID)
				if fErr != nil {
					return mapError(fErr)
				}
				if affected > 0 {
					*fire = *entTriggerFireToStore(fresh)
					claimed = true
					promoted = true
					return nil
				}
				// Concurrent state transition (e.g. reaper marked the row
				// failed "hub restarted during coalesce/defer" between read and CAS):
				// fall through so reaped-pending is revived in place.
				ex = fresh
			}

			if isReapedPendingFire(ex) {
				// Stage 3 §6.3/§6.4: reaped coalescing/deferred row is re-fired in place
				// by the queue item's retry, never re-created/duplicated.
				affected, uErr := client.TriggerFire.Update().
					Where(
						triggerfire.IDEQ(ex.ID),
						triggerfire.StatusEQ(store.TriggerFireStatusFailed),
						triggerfire.ErrorEQ(ex.Error),
					).
					SetStatus(store.TriggerFireStatusFiring).
					ClearError().
					ClearFinishedAt().
					SetLeaseUntil(now.Add(leaseHorizon)).
					Save(ctx)
				if uErr != nil {
					return mapError(uErr)
				}
				if affected == 0 {
					fresh, fErr := client.TriggerFire.Get(ctx, ex.ID)
					if fErr != nil {
						return mapError(fErr)
					}
					claimed = false
					promoted = false
					existing = entTriggerFireToStore(fresh)
					return nil
				}
				fresh, fErr := client.TriggerFire.Get(ctx, ex.ID)
				if fErr != nil {
					return mapError(fErr)
				}
				*fire = *entTriggerFireToStore(fresh)
				claimed = true
				promoted = true
				return nil
			}

			claimed = false
			promoted = false
			existing = entTriggerFireToStore(ex)
			return nil
		}
		if !ent.IsNotFound(qErr) {
			return mapError(qErr)
		}

		// Supersede any unreleased coalescing, deferred, or reaped-pending rows for
		// (trigger_id, subject) via CAS so an older event's retry cannot revive
		// and double-dispatch alongside the newer event.
		coalesceOrReaped := pendingOrReapedFirePredicate()
		prevRows, pErr := client.TriggerFire.Query().
			Where(
				triggerfire.TriggerIDEQ(tid),
				triggerfire.SubjectEQ(fire.Subject),
				coalesceOrReaped,
			).
			All(ctx)
		if pErr != nil {
			return mapError(pErr)
		}
		for _, prev := range prevRows {
			affected, uErr := client.TriggerFire.Update().
				Where(triggerfire.IDEQ(prev.ID), coalesceOrReaped).
				SetStatus(store.TriggerFireStatusSkipped).
				SetError(coalesceError).
				SetFinishedAt(now).
				Save(ctx)
			if uErr != nil {
				return mapError(uErr)
			}
			if affected > 0 {
				sf := entTriggerFireToStore(prev)
				sf.Status = store.TriggerFireStatusSkipped
				sf.Error = coalesceError
				fin := now
				sf.FinishedAt = &fin
				superseded = append(superseded, *sf)
			}
		}

		row := *fire
		row.Status = store.TriggerFireStatusCoalescing
		nb := notBefore
		row.NotBefore = &nb
		row.LeaseUntil = notBefore.Add(leaseHorizon)
		create, bErr := buildTriggerFireCreate(client, uid, pid, tid, eid, &row, now)
		if bErr != nil {
			return bErr
		}
		created, cErr := create.Save(ctx)
		if cErr != nil {
			return mapError(cErr)
		}
		*fire = *entTriggerFireToStore(created)
		claimed = true
		promoted = false
		return nil
	})
	if txErr != nil {
		if !s.inTx && errors.Is(txErr, store.ErrAlreadyExists) {
			ex, qErr := s.client.TriggerFire.Query().
				Where(triggerfire.EventIDEQ(eid), triggerfire.TriggerIDEQ(tid)).
				Only(ctx)
			if qErr != nil {
				return false, false, nil, nil, mapError(qErr)
			}
			return false, false, nil, entTriggerFireToStore(ex), nil
		}
		return false, false, nil, nil, txErr
	}
	return claimed, promoted, superseded, existing, nil
}

func (s *TriggerStore) DeferTriggerFire(ctx context.Context, id string, deferredAt, notBefore, leaseUntil time.Time) (bool, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return false, err
	}
	var updated bool
	txErr := s.withClientTx(ctx, func(client *ent.Client) error {
		ex, qErr := client.TriggerFire.Get(ctx, uid)
		if qErr != nil {
			if ent.IsNotFound(qErr) {
				return nil
			}
			return mapError(qErr)
		}
		if ex.Status != store.TriggerFireStatusFiring {
			return nil
		}
		u := client.TriggerFire.Update().
			Where(triggerfire.IDEQ(uid), triggerfire.StatusEQ(store.TriggerFireStatusFiring)).
			SetStatus(store.TriggerFireStatusDeferred).
			SetNotBefore(notBefore.UTC()).
			SetLeaseUntil(leaseUntil.UTC())
		// Preserve the first deferral timestamp across subsequent deferrals
		// and hub restarts (Stage 3 §6.2 rev 1.16).
		if ex.DeferredAt == nil && !deferredAt.IsZero() {
			u.SetDeferredAt(deferredAt.UTC())
		}
		affected, uErr := u.Save(ctx)
		if uErr != nil {
			return mapError(uErr)
		}
		updated = affected > 0
		return nil
	})
	if txErr != nil {
		return false, txErr
	}
	return updated, nil
}

func (s *TriggerStore) FinishPendingTriggerFire(ctx context.Context, id, status, errMsg, matchedThrough, decidedDialect, ruleTrace string, finishedAt time.Time) (bool, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return false, err
	}
	u := s.client.TriggerFire.Update().
		Where(triggerfire.IDEQ(uid), pendingOrReapedFirePredicate()).
		SetStatus(status).
		SetFinishedAt(finishedAt.UTC())
	if errMsg != "" {
		u.SetError(errMsg)
	} else {
		u.ClearError()
	}
	if matchedThrough != "" {
		u.SetMatchedThrough(matchedThrough)
	}
	if decidedDialect != "" {
		u.SetDecidedDialect(decidedDialect)
	}
	if ruleTrace != "" {
		u.SetRuleTrace(ruleTrace)
	}
	affected, err := u.Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return affected > 0, nil
}

func (s *TriggerStore) FinishTriggerFire(ctx context.Context, id, status, resultRef, targetAgentID, errMsg string, finishedAt time.Time) (bool, error) {
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
	if targetAgentID != "" {
		taid, err := parseUUID(targetAgentID)
		if err != nil {
			return false, err
		}
		u.SetTargetAgentID(taid)
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

func (s *TriggerStore) ParkTriggerFireWaiting(
	ctx context.Context,
	id, projectID, targetAgentID, errMsg string,
	waitUntil time.Time,
	maxPerTarget, maxPerProject int,
) (bool, bool, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return false, false, err
	}
	pid, err := parseUUID(projectID)
	if err != nil {
		return false, false, err
	}
	taid, err := parseUUID(targetAgentID)
	if err != nil {
		return false, false, err
	}
	if waitUntil.IsZero() {
		return false, false, store.ErrInvalidInput
	}
	waitUntil = waitUntil.UTC().Truncate(time.Microsecond)

	var (
		parked      bool
		capExceeded bool
	)
	txErr := s.withClientTx(ctx, func(client *ent.Client) error {
		if isPostgres(client) {
			var res sql.Result
			if lErr := client.Driver().Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)",
				[]any{int32(store.LockTriggerOfflineQueue), store.StableProjectHash(projectID)}, &res); lErr != nil {
				return fmt.Errorf("lock trigger offline queue: %w", lErr)
			}
		}

		if maxPerTarget > 0 {
			targetCount, cErr := client.TriggerFire.Query().
				Where(
					triggerfire.TargetAgentIDEQ(taid),
					triggerfire.StatusEQ(store.TriggerFireStatusWaiting),
				).
				Count(ctx)
			if cErr != nil {
				return mapError(cErr)
			}
			if targetCount >= maxPerTarget {
				capExceeded = true
				return nil
			}
		}
		if maxPerProject > 0 {
			projCount, cErr := client.TriggerFire.Query().
				Where(
					triggerfire.ProjectIDEQ(pid),
					triggerfire.StatusEQ(store.TriggerFireStatusWaiting),
				).
				Count(ctx)
			if cErr != nil {
				return mapError(cErr)
			}
			if projCount >= maxPerProject {
				capExceeded = true
				return nil
			}
		}

		u := client.TriggerFire.Update().
			Where(triggerfire.IDEQ(uid), triggerfire.StatusEQ(store.TriggerFireStatusFiring)).
			SetStatus(store.TriggerFireStatusWaiting).
			SetTargetAgentID(taid).
			SetWaitUntil(waitUntil).
			SetAttempts(1).
			ClearFinishedAt()
		if errMsg != "" {
			u.SetError(errMsg)
		} else {
			u.ClearError()
		}
		affected, uErr := u.Save(ctx)
		if uErr != nil {
			return mapError(uErr)
		}
		parked = affected > 0
		return nil
	})
	if txErr != nil {
		return false, false, txErr
	}
	return parked, capExceeded, nil
}

func (s *TriggerStore) FinishWaitingTriggerFire(
	ctx context.Context,
	id, status, resultRef, targetAgentID, errMsg string,
	finishedAt time.Time,
	attempts int,
) (bool, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return false, err
	}
	u := s.client.TriggerFire.Update().
		Where(triggerfire.IDEQ(uid), triggerfire.StatusEQ(store.TriggerFireStatusWaiting)).
		SetStatus(status).
		SetFinishedAt(finishedAt.UTC())
	if attempts > 0 {
		u.SetAttempts(attempts)
	}
	if resultRef != "" {
		u.SetResultRef(resultRef)
	} else {
		u.ClearResultRef()
	}
	if targetAgentID != "" {
		taid, err := parseUUID(targetAgentID)
		if err != nil {
			return false, err
		}
		u.SetTargetAgentID(taid)
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

func (s *TriggerStore) ClaimWaitingTriggerFire(ctx context.Context, id string, attempts int, leaseUntil time.Time) (bool, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return false, err
	}
	if leaseUntil.IsZero() {
		return false, store.ErrInvalidInput
	}
	u := s.client.TriggerFire.Update().
		Where(triggerfire.IDEQ(uid), triggerfire.StatusEQ(store.TriggerFireStatusWaiting)).
		SetStatus(store.TriggerFireStatusFiring).
		SetLeaseUntil(leaseUntil.UTC()).
		ClearFinishedAt()
	if attempts > 0 {
		u.SetAttempts(attempts)
	}
	affected, err := u.Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return affected > 0, nil
}

func (s *TriggerStore) ReturnFiringTriggerFireToWaiting(ctx context.Context, id string, errMsg string) (bool, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return false, err
	}
	u := s.client.TriggerFire.Update().
		Where(triggerfire.IDEQ(uid), triggerfire.StatusEQ(store.TriggerFireStatusFiring)).
		SetStatus(store.TriggerFireStatusWaiting).
		ClearFinishedAt()
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

func (s *TriggerStore) ListWaitingFiresForTarget(ctx context.Context, targetAgentID string, limit int) ([]string, error) {
	taid, err := parseGetID(targetAgentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			return nil, nil
		}
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.client.TriggerFire.Query().
		Where(
			triggerfire.TargetAgentIDEQ(taid),
			triggerfire.StatusEQ(store.TriggerFireStatusWaiting),
		).
		Order(triggerfire.ByCreated()).
		Order(triggerfire.ByID()).
		Limit(limit).
		Select(triggerfire.FieldID).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID.String()
	}
	return out, nil
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

	var (
		totalCount int
		err        error
	)
	if !opts.SkipTotalCount {
		totalCount, err = query.Clone().Count(ctx)
		if err != nil {
			return nil, err
		}
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

func hasLiveTriggerFireForSubjectClient(ctx context.Context, client *ent.Client, tid uuid.UUID, subject string, excludeFireID uuid.UUID, includeFiring bool, now time.Time) (bool, error) {
	statuses := []string{store.TriggerFireStatusCoalescing, store.TriggerFireStatusDeferred}
	if includeFiring {
		statuses = append(statuses, store.TriggerFireStatusFiring)
	}
	q := client.TriggerFire.Query().
		Where(
			triggerfire.TriggerIDEQ(tid),
			triggerfire.SubjectEQ(subject),
			triggerfire.StatusIn(statuses...),
			triggerfire.LeaseUntilGT(now.UTC()),
		)
	if excludeFireID != uuid.Nil {
		q = q.Where(triggerfire.IDNEQ(excludeFireID))
	}
	return q.Exist(ctx)
}

func (s *TriggerStore) CountTriggerFiresSince(ctx context.Context, triggerID string, since time.Time, excludeStatuses ...string) (int, error) {
	tid, err := parseUUID(triggerID)
	if err != nil {
		return 0, err
	}
	preds := []predicate.TriggerFire{
		triggerfire.TriggerIDEQ(tid),
		triggerfire.CreatedGTE(since.UTC()),
	}
	if len(excludeStatuses) > 0 {
		preds = append(preds, triggerfire.StatusNotIn(excludeStatuses...))
	}
	n, err := s.client.TriggerFire.Query().Where(preds...).Count(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

// CountRecentTriggerFires counts non-skipped fires for triggerID created after
// since. When excludeFireID names the caller's in-flight fire row, it excludes
// that row AND any other in-flight ("firing") row ordered strictly after the
// caller's (created, id) pair so concurrent claimants never mutually skip each
// other (Stage 2 §7.4, P4a O1). Non-firing (completed/refused/failed) rows in
// the window always count.
func (s *TriggerStore) CountRecentTriggerFires(ctx context.Context, triggerID string, since time.Time, excludeFireID string) (int, error) {
	tid, err := parseUUID(triggerID)
	if err != nil {
		return 0, err
	}
	q := s.client.TriggerFire.Query().
		Where(
			triggerfire.TriggerIDEQ(tid),
			triggerfire.CreatedGT(since.UTC()),
			triggerfire.StatusNEQ(store.TriggerFireStatusSkipped),
		)
	if excludeFireID != "" {
		exID, parseErr := parseUUID(excludeFireID)
		if parseErr != nil {
			return 0, parseErr
		}
		q = q.Where(triggerfire.IDNEQ(exID))
		callerRow, getErr := s.client.TriggerFire.Get(ctx, exID)
		if getErr == nil {
			callerCreated := callerRow.Created.UTC()
			q = q.Where(triggerfire.Or(
				triggerfire.StatusNEQ(store.TriggerFireStatusFiring),
				triggerfire.CreatedLT(callerCreated),
				triggerfire.And(
					triggerfire.CreatedEQ(callerCreated),
					triggerfire.IDLT(exID),
				),
			))
		} else if !ent.IsNotFound(getErr) {
			return 0, getErr
		}
	}
	return q.Count(ctx)
}

func (s *TriggerStore) HasLiveTriggerFireForSubject(ctx context.Context, triggerID, subject string, now time.Time) (bool, error) {
	tid, err := parseUUID(triggerID)
	if err != nil {
		return false, err
	}
	return hasLiveTriggerFireForSubjectClient(ctx, s.client, tid, subject, uuid.Nil, true, now)
}

func (s *TriggerStore) HasLiveTriggerFireForSubjectExcluding(ctx context.Context, triggerID, subject, excludeFireID string, includeFiring bool, now time.Time) (bool, error) {
	tid, err := parseUUID(triggerID)
	if err != nil {
		return false, err
	}
	var exID uuid.UUID
	if excludeFireID != "" {
		exID, err = parseUUID(excludeFireID)
		if err != nil {
			return false, err
		}
	}
	return hasLiveTriggerFireForSubjectClient(ctx, s.client, tid, subject, exID, includeFiring, now)
}

func (s *TriggerStore) HasPendingTriggerFiresForEvent(ctx context.Context, eventID string) (bool, error) {
	eid, err := parseUUID(eventID)
	if err != nil {
		return false, err
	}
	return s.client.TriggerFire.Query().
		Where(
			triggerfire.EventIDEQ(eid),
			pendingOrReapedFirePredicate(),
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

func (s *TriggerStore) EventHasTriggerFireForResultRef(ctx context.Context, eventID, resultRef string) (bool, error) {
	eid, err := parseGetID(eventID)
	if err != nil {
		return false, err
	}
	if resultRef == "" {
		return false, nil
	}
	return s.client.TriggerFire.Query().
		Where(
			triggerfire.EventIDEQ(eid),
			triggerfire.ResultRefEQ(resultRef),
		).
		Exist(ctx)
}

func (s *TriggerStore) ReapExpiredTriggerFires(ctx context.Context, now time.Time, limit int) (int, error) {
	now = now.UTC()
	q := s.client.TriggerFire.Query().
		Where(
			triggerfire.StatusIn(store.TriggerFireStatusFiring, store.TriggerFireStatusCoalescing, store.TriggerFireStatusDeferred),
			triggerfire.LeaseUntilLTE(now),
			triggerfire.Not(triggerfire.HasExecRun()),
		).
		Order(triggerfire.ByLeaseUntil())
	if limit > 0 {
		q = q.Limit(limit)
	}
	expired, err := q.Select(triggerfire.FieldID, triggerfire.FieldTriggerID, triggerfire.FieldStatus).All(ctx)
	if err != nil {
		return 0, err
	}
	reaped := 0
	for _, f := range expired {
		errMsg := "hub restarted during fire"
		switch f.Status {
		case store.TriggerFireStatusCoalescing:
			errMsg = errReapedDuringCoalesce
		case store.TriggerFireStatusDeferred:
			errMsg = errReapedDuringDefer
		}
		affected, uErr := s.client.TriggerFire.Update().
			Where(
				triggerfire.IDEQ(f.ID),
				triggerfire.StatusEQ(f.Status),
				triggerfire.Not(triggerfire.HasExecRun()),
			).
			SetStatus(store.TriggerFireStatusFailed).
			SetError(errMsg).
			SetFinishedAt(now).
			Save(ctx)
		if uErr != nil {
			return reaped, mapError(uErr)
		}
		if affected > 0 {
			reaped++
			_, _ = s.client.TriggerSubjectClaim.Update().
				Where(
					triggersubjectclaim.FireIDEQ(f.ID),
					triggersubjectclaim.StateEQ(store.TriggerSubjectClaimStateActive),
				).
				SetState(store.TriggerSubjectClaimStateReleased).
				SetReleasedAt(now.Truncate(time.Microsecond)).
				Save(ctx)
			_ = s.RecordTriggerFire(ctx, f.TriggerID.String(), now, store.TriggerFireStatusFailed, errMsg)
		}
	}
	return reaped, nil
}

func (s *TriggerStore) PurgeOldTriggerHistory(ctx context.Context, olderThan time.Time) (int, int, error) {
	eventsDeleted, firesDeleted, _, err := s.PurgeOldTriggerHistoryWithPayloadRefs(ctx, olderThan)
	return eventsDeleted, firesDeleted, err
}

func (s *TriggerStore) PurgeOldTriggerHistoryWithPayloadRefs(ctx context.Context, olderThan time.Time) (int, int, []string, error) {
	cutoff := olderThan.UTC()
	var (
		firesDeleted  int
		eventsDeleted int
		payloadRefs   []string
	)
	err := s.withClientTx(ctx, func(c *ent.Client) error {
		purgeable := []predicate.TriggerFire{
			triggerfire.StatusNotIn(store.TriggerFireStatusFiring, store.TriggerFireStatusCoalescing, store.TriggerFireStatusDeferred, store.TriggerFireStatusWaiting),
			triggerfire.CreatedLT(cutoff),
		}
		ids, err := c.TriggerFire.Query().Where(purgeable...).IDs(ctx)
		if err != nil {
			return mapError(err)
		}
		for start := 0; start < len(ids); start += scheduledExecRunsByEventChunk {
			end := min(start+scheduledExecRunsByEventChunk, len(ids))
			chunk := ids[start:end]
			if _, err := c.ScheduledExecRun.Delete().Where(scheduledexecrun.FireIDIn(chunk...)).Exec(ctx); err != nil {
				return mapError(err)
			}
			n, err := c.TriggerFire.Delete().
				Where(append([]predicate.TriggerFire{triggerfire.IDIn(chunk...)}, purgeable...)...).
				Exec(ctx)
			if err != nil {
				return mapError(err)
			}
			firesDeleted += n
		}
		evPreds := []predicate.TriggerEvent{
			triggerevent.StatusNEQ(store.TriggerEventStatusPending),
			triggerevent.CreatedLT(cutoff),
		}
		refs, err := c.TriggerEvent.Query().
			Where(append(append([]predicate.TriggerEvent{}, evPreds...), triggerevent.PayloadRefGT(""))...).
			Select(triggerevent.FieldPayloadRef).
			Strings(ctx)
		if err != nil {
			return mapError(err)
		}
		evDel, err := c.TriggerEvent.Delete().
			Where(evPreds...).
			Exec(ctx)
		if err != nil {
			return mapError(err)
		}
		eventsDeleted = evDel
		payloadRefs = refs
		return nil
	})
	if err != nil {
		return 0, 0, nil, err
	}
	return eventsDeleted, firesDeleted, payloadRefs, nil
}
