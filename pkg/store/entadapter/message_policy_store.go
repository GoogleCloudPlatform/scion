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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"slices"
	"sort"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/messagepolicydecision"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

var _ store.MessagePolicyStore = (*CompositeStore)(nil)
var _ store.MessagePolicyStore = (*MessagePolicyStore)(nil)

const (
	// messagePolicyDecisionDeleteBatch bounds IN(...) lists in decision trim deletes
	// well within SQLite and PostgreSQL parameter limits.
	messagePolicyDecisionDeleteBatch = 500

	// messagePolicyDecisionTrimPassLimit caps total decision rows selected and
	// deleted in a single TrimMessagePolicyDecisions pass so maintenance stays
	// bounded in memory and lock duration.
	messagePolicyDecisionTrimPassLimit = 2000

	// Advisory lock prefixes in the upper 32 bits of Postgres's 64-bit
	// pg_advisory_xact_lock(bigint) keyspace (distinct from durable_queue's
	// 0x5C10_2001 and zero-prefixed store.AdvisoryLockKey values).
	messagePolicyDecisionTrimLockKey  int64 = (int64(0x5C10_2002) << 32) | 1
	messagePolicyProjectCreateLockPfx int64 = 0x5C10_2003
)

func messagePolicyProjectCreateLockKey(projectID uuid.UUID) int64 {
	h := fnv.New32a()
	_, _ = h.Write(projectID[:])
	return (messagePolicyProjectCreateLockPfx << 32) | int64(h.Sum32())
}

func lockMessagePolicyProjectCreate(ctx context.Context, client *ent.Client, projectID uuid.UUID) error {
	if client.Driver().Dialect() != dialect.Postgres {
		return nil
	}
	var res sql.Result
	if err := client.Driver().Exec(ctx, "SELECT pg_advisory_xact_lock($1)", []any{messagePolicyProjectCreateLockKey(projectID)}, &res); err != nil {
		return fmt.Errorf("lock message policy create for project %s: %w", projectID, err)
	}
	return nil
}

func lockMessagePolicyDecisionTrim(ctx context.Context, client *ent.Client) error {
	if client.Driver().Dialect() != dialect.Postgres {
		return nil
	}
	var res sql.Result
	if err := client.Driver().Exec(ctx, "SELECT pg_advisory_xact_lock($1)", []any{messagePolicyDecisionTrimLockKey}, &res); err != nil {
		return fmt.Errorf("lock message policy decision trim: %w", err)
	}
	return nil
}

// MessagePolicyStore implements store.MessagePolicyStore using Ent ORM.
type MessagePolicyStore struct {
	client                *ent.Client
	inTx                  bool
	decisionTrimPassLimit int // test override; <= 0 uses messagePolicyDecisionTrimPassLimit
}

// NewMessagePolicyStore creates a new Ent-backed MessagePolicyStore.
func NewMessagePolicyStore(client *ent.Client) *MessagePolicyStore {
	return &MessagePolicyStore{client: client}
}

// usesRowLocks reports whether SELECT ... FOR UPDATE is supported (Postgres).
// Reads dialect directly from the driver without issuing a query.
func (s *MessagePolicyStore) usesRowLocks() bool {
	return s.client.Driver().Dialect() == dialect.Postgres
}

func (s *MessagePolicyStore) effectiveTrimPassLimit() int {
	if s.decisionTrimPassLimit > 0 {
		return s.decisionTrimPassLimit
	}
	return messagePolicyDecisionTrimPassLimit
}

func entMessagePolicyToStore(row *ent.MessagePolicy) *store.MessagePolicy {
	out := &store.MessagePolicy{
		ID:                row.ID.String(),
		ProjectID:         row.ProjectID.String(),
		Name:              row.Name,
		Description:       row.Description,
		Enabled:           row.Enabled,
		Mode:              string(row.Mode),
		Scope:             row.Scope.Clone(),
		ViolationRule:     row.ViolationRule,
		SystemOneRule:     row.SystemOneRule,
		OnUncertain:       row.OnUncertain,
		OnFailed:          string(row.OnFailed),
		RejectReason:      row.RejectReason,
		RejectRemediation: row.RejectRemediation,
		Revision:          row.Revision,
		CreatedBy:         row.CreatedBy,
		UpdatedBy:         row.UpdatedBy,
		CreatedAt:         row.CreatedAt.UTC(),
		UpdatedAt:         row.UpdatedAt.UTC(),
	}
	if len(row.SystemOneRefs) > 0 {
		out.SystemOneRefs = slices.Clone(row.SystemOneRefs)
	}
	if len(row.OnFailedReasons) > 0 {
		out.OnFailedReasons = cloneStringMap(row.OnFailedReasons)
	}
	return out
}

func entMessagePolicyDecisionToStore(row *ent.MessagePolicyDecision) *store.MessagePolicyDecision {
	var msgID string
	if row.MessageID != nil {
		msgID = row.MessageID.String()
	}
	var eval json.RawMessage
	if row.Evaluation != "" {
		eval = json.RawMessage(row.Evaluation)
	}
	return &store.MessagePolicyDecision{
		ID:             row.ID.String(),
		ProjectID:      row.ProjectID.String(),
		PolicyID:       row.PolicyID.String(),
		PolicyName:     row.PolicyName,
		PolicyRevision: row.PolicyRevision,
		Mode:           string(row.Mode),
		Surface:        row.Surface,
		SenderKind:     row.SenderKind,
		SenderID:       row.SenderID,
		RecipientKind:  row.RecipientKind,
		RecipientID:    row.RecipientID,
		MessageID:      msgID,
		Class:          string(row.Class),
		Action:         string(row.Action),
		Reason:         row.Reason,
		CELErrorReason: row.CelErrorReason,
		Evaluation:     eval,
		ElapsedMs:      row.ElapsedMs,
		CreatedAt:      row.CreatedAt.UTC(),
	}
}

func validateMessagePolicyFields(policy *store.MessagePolicy) (string, error) {
	if policy == nil {
		return "", fmt.Errorf("%w: message policy is nil", store.ErrInvalidInput)
	}
	if !store.IsValidMessagePolicyName(policy.Name) {
		return "", fmt.Errorf("%w: invalid message policy name %q (must be a canonical slug of 1..%d chars)", store.ErrInvalidInput, policy.Name, store.MaxMessagePolicyNameLen)
	}
	mode := policy.Mode
	if mode == "" {
		mode = store.MessagePolicyModeShadow
	}
	if !store.IsValidMessagePolicyMode(mode) {
		return "", fmt.Errorf("%w: invalid message policy mode %q", store.ErrInvalidInput, mode)
	}
	if len(policy.Scope.Surfaces) == 0 {
		return "", fmt.Errorf("%w: scope.surfaces must not be empty", store.ErrInvalidInput)
	}
	for _, s := range policy.Scope.Surfaces {
		if s == "" {
			return "", fmt.Errorf("%w: scope.surfaces must not contain empty values", store.ErrInvalidInput)
		}
	}
	if policy.ViolationRule == "" {
		return "", fmt.Errorf("%w: violation_rule is required", store.ErrInvalidInput)
	}
	if len(policy.ViolationRule) > store.MaxMessagePolicyViolationRuleBytes {
		return "", fmt.Errorf("%w: violation_rule exceeds %d bytes", store.ErrInvalidInput, store.MaxMessagePolicyViolationRuleBytes)
	}
	if len(policy.SystemOneRule) > store.MaxMessagePolicySystemOneRuleBytes {
		return "", fmt.Errorf("%w: system_one_rule exceeds %d bytes", store.ErrInvalidInput, store.MaxMessagePolicySystemOneRuleBytes)
	}
	if policy.OnUncertain != "" && !store.IsValidMessagePolicyOutcome(policy.OnUncertain) {
		return "", fmt.Errorf("%w: invalid on_uncertain outcome %q", store.ErrInvalidInput, policy.OnUncertain)
	}
	if !store.IsValidMessagePolicyOutcome(policy.OnFailed) {
		return "", fmt.Errorf("%w: on_failed must be %q or %q", store.ErrInvalidInput, store.MessagePolicyOutcomeAdmit, store.MessagePolicyOutcomeReject)
	}
	for reason, outcome := range policy.OnFailedReasons {
		if reason == "" || !store.IsValidMessagePolicyOutcome(outcome) {
			return "", fmt.Errorf("%w: invalid on_failed_reasons entry %q -> %q", store.ErrInvalidInput, reason, outcome)
		}
	}
	if len(policy.RejectReason) > store.MaxMessagePolicyTemplateBytes {
		return "", fmt.Errorf("%w: reject_reason exceeds %d bytes", store.ErrInvalidInput, store.MaxMessagePolicyTemplateBytes)
	}
	if len(policy.RejectRemediation) > store.MaxMessagePolicyTemplateBytes {
		return "", fmt.Errorf("%w: reject_remediation exceeds %d bytes", store.ErrInvalidInput, store.MaxMessagePolicyTemplateBytes)
	}
	return mode, nil
}

// CreateMessagePolicy creates a new message policy at Revision=1.
func (s *MessagePolicyStore) CreateMessagePolicy(ctx context.Context, policy *store.MessagePolicy) error {
	mode, err := validateMessagePolicyFields(policy)
	if err != nil {
		return err
	}
	projectUUID, err := parseUUID(policy.ProjectID)
	if err != nil {
		return err
	}
	if projectUUID == uuid.Nil {
		return fmt.Errorf("%w: project_id must not be nil UUID", store.ErrInvalidInput)
	}

	var uid uuid.UUID
	if policy.ID != "" {
		uid, err = parseUUID(policy.ID)
		if err != nil {
			return err
		}
		if uid == uuid.Nil {
			return fmt.Errorf("%w: id must not be nil UUID", store.ErrInvalidInput)
		}
	} else {
		uid = uuid.New()
	}

	return runInEntTx(ctx, s.client, s.inTx, func(client *ent.Client) error {
		if err := lockMessagePolicyProjectCreate(ctx, client, projectUUID); err != nil {
			return err
		}
		count, err := client.MessagePolicy.Query().
			Where(messagepolicy.ProjectIDEQ(projectUUID)).
			Count(ctx)
		if err != nil {
			return mapError(err)
		}
		if count >= store.MaxMessagePoliciesPerProject {
			return fmt.Errorf("%w: project %s already has the maximum of %d message policies",
				store.ErrQuotaExceeded, projectUUID, store.MaxMessagePoliciesPerProject)
		}

		now := time.Now().UTC().Truncate(time.Microsecond)
		create := client.MessagePolicy.Create().
			SetID(uid).
			SetProjectID(projectUUID).
			SetName(policy.Name).
			SetDescription(policy.Description).
			SetEnabled(policy.Enabled).
			SetMode(messagepolicy.Mode(mode)).
			SetScope(policy.Scope.Clone()).
			SetViolationRule(policy.ViolationRule).
			SetSystemOneRule(policy.SystemOneRule).
			SetOnUncertain(policy.OnUncertain).
			SetOnFailed(messagepolicy.OnFailed(policy.OnFailed)).
			SetRejectReason(policy.RejectReason).
			SetRejectRemediation(policy.RejectRemediation).
			SetRevision(1).
			SetCreatedBy(policy.CreatedBy).
			SetUpdatedBy(policy.UpdatedBy).
			SetCreatedAt(now).
			SetUpdatedAt(now)

		if len(policy.SystemOneRefs) > 0 {
			create.SetSystemOneRefs(slices.Clone(policy.SystemOneRefs))
		}
		if len(policy.OnFailedReasons) > 0 {
			create.SetOnFailedReasons(cloneStringMap(policy.OnFailedReasons))
		}

		created, err := create.Save(ctx)
		if err != nil {
			return mapError(err)
		}

		policy.ID = created.ID.String()
		policy.Mode = string(created.Mode)
		policy.Revision = created.Revision
		policy.CreatedAt = created.CreatedAt.UTC()
		policy.UpdatedAt = created.UpdatedAt.UTC()
		return nil
	})
}

// GetMessagePolicy retrieves a message policy by UUID.
func (s *MessagePolicyStore) GetMessagePolicy(ctx context.Context, id string) (*store.MessagePolicy, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	row, err := s.client.MessagePolicy.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entMessagePolicyToStore(row), nil
}

// GetMessagePolicyByName retrieves a message policy by (projectID, name).
func (s *MessagePolicyStore) GetMessagePolicyByName(ctx context.Context, projectID, name string) (*store.MessagePolicy, error) {
	projectUUID, err := parseGetID(projectID)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, store.ErrNotFound
	}
	row, err := s.client.MessagePolicy.Query().
		Where(
			messagepolicy.ProjectIDEQ(projectUUID),
			messagepolicy.NameEQ(name),
		).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entMessagePolicyToStore(row), nil
}

type messagePolicyCursor struct {
	Name string `json:"n"`
	ID   string `json:"id"`
}

func encodeMessagePolicyCursor(name, id string) string {
	raw, _ := json.Marshal(messagePolicyCursor{Name: name, ID: id})
	return base64.URLEncoding.EncodeToString(raw)
}

func decodeMessagePolicyCursor(cursor string) (string, uuid.UUID, error) {
	raw, err := base64.URLEncoding.DecodeString(cursor)
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("%w: base64 decode: %w", store.ErrInvalidCursor, err)
	}
	var c messagePolicyCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", uuid.Nil, fmt.Errorf("%w: decode cursor: %w", store.ErrInvalidCursor, err)
	}
	if !store.IsValidMessagePolicyName(c.Name) {
		return "", uuid.Nil, fmt.Errorf("%w: invalid cursor policy name", store.ErrInvalidCursor)
	}
	uid, err := uuid.Parse(c.ID)
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("%w: invalid cursor id: %w", store.ErrInvalidCursor, err)
	}
	return c.Name, uid, nil
}

// ListMessagePolicies lists message policies ordered by (name ASC, id ASC).
func (s *MessagePolicyStore) ListMessagePolicies(
	ctx context.Context,
	filter store.MessagePolicyFilter,
	opts store.ListOptions,
) (*store.ListResult[store.MessagePolicy], error) {
	q := s.client.MessagePolicy.Query()
	if filter.ProjectID != "" {
		projectUUID, err := parseUUID(filter.ProjectID)
		if err != nil {
			return nil, err
		}
		q = q.Where(messagepolicy.ProjectIDEQ(projectUUID))
	}
	if filter.Enabled != nil {
		q = q.Where(messagepolicy.EnabledEQ(*filter.Enabled))
	}
	if filter.Mode != "" {
		if !store.IsValidMessagePolicyMode(filter.Mode) {
			return &store.ListResult[store.MessagePolicy]{Items: []store.MessagePolicy{}}, nil
		}
		q = q.Where(messagepolicy.ModeEQ(messagepolicy.Mode(filter.Mode)))
	}

	var total int
	if !opts.SkipTotalCount {
		var err error
		total, err = q.Clone().Count(ctx)
		if err != nil {
			return nil, mapError(err)
		}
	}

	if opts.Cursor != "" {
		curName, curID, err := decodeMessagePolicyCursor(opts.Cursor)
		if err != nil {
			return nil, err
		}
		q = q.Where(messagepolicy.Or(
			messagepolicy.NameGT(curName),
			messagepolicy.And(
				messagepolicy.NameEQ(curName),
				messagepolicy.IDGT(curID),
			),
		))
	}

	q = q.Order(
		messagepolicy.ByName(entsql.OrderAsc()),
		messagepolicy.ByID(entsql.OrderAsc()),
	)
	if opts.Limit > 0 {
		q = q.Limit(opts.Limit + 1)
	}

	rows, err := q.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	nextCursor := ""
	if opts.Limit > 0 && len(rows) > opts.Limit {
		rows = rows[:opts.Limit]
		last := rows[len(rows)-1]
		nextCursor = encodeMessagePolicyCursor(last.Name, last.ID.String())
	}

	items := make([]store.MessagePolicy, len(rows))
	for i, r := range rows {
		items[i] = *entMessagePolicyToStore(r)
	}
	return &store.ListResult[store.MessagePolicy]{
		Items:      items,
		TotalCount: total,
		NextCursor: nextCursor,
	}, nil
}

// UpdateMessagePolicy updates an existing message policy and increments
// Revision by 1 on every edit (consumer design §5.3).
func (s *MessagePolicyStore) UpdateMessagePolicy(ctx context.Context, policy *store.MessagePolicy) error {
	if policy == nil {
		return fmt.Errorf("%w: message policy is nil", store.ErrInvalidInput)
	}
	hasID := policy.ID != ""
	var lookupID uuid.UUID
	var lookupProjectID uuid.UUID
	if hasID {
		var err error
		lookupID, err = parseUUID(policy.ID)
		if err != nil {
			return err
		}
	} else {
		if policy.ProjectID == "" || policy.Name == "" {
			return fmt.Errorf("%w: id or (project_id, name) is required", store.ErrInvalidInput)
		}
		var err error
		lookupProjectID, err = parseUUID(policy.ProjectID)
		if err != nil {
			return err
		}
	}

	return runInEntTx(ctx, s.client, s.inTx, func(client *ent.Client) error {
		q := client.MessagePolicy.Query()
		if hasID {
			q = q.Where(messagepolicy.IDEQ(lookupID))
		} else {
			q = q.Where(
				messagepolicy.ProjectIDEQ(lookupProjectID),
				messagepolicy.NameEQ(policy.Name),
			)
		}
		if s.usesRowLocks() {
			q = q.ForUpdate()
		}

		existing, err := q.Only(ctx)
		if err != nil {
			return mapError(err)
		}

		candidate := policy.Clone()
		if candidate.ProjectID == "" {
			candidate.ProjectID = existing.ProjectID.String()
		} else if candidate.ProjectID != existing.ProjectID.String() {
			return fmt.Errorf("%w: project_id cannot be changed", store.ErrInvalidInput)
		}
		if candidate.Name == "" {
			candidate.Name = existing.Name
		}

		mode, err := validateMessagePolicyFields(candidate)
		if err != nil {
			return err
		}

		if policy.Revision > 0 && policy.Revision != existing.Revision {
			return store.ErrRevisionConflict
		}

		nextRevision := existing.Revision + 1
		now := time.Now().UTC().Truncate(time.Microsecond)
		upd := client.MessagePolicy.Update().
			Where(
				messagepolicy.IDEQ(existing.ID),
				messagepolicy.RevisionEQ(existing.Revision),
			).
			SetName(candidate.Name).
			SetDescription(candidate.Description).
			SetEnabled(candidate.Enabled).
			SetMode(messagepolicy.Mode(mode)).
			SetScope(candidate.Scope.Clone()).
			SetViolationRule(candidate.ViolationRule).
			SetSystemOneRule(candidate.SystemOneRule).
			SetOnUncertain(candidate.OnUncertain).
			SetOnFailed(messagepolicy.OnFailed(candidate.OnFailed)).
			SetRejectReason(candidate.RejectReason).
			SetRejectRemediation(candidate.RejectRemediation).
			SetRevision(nextRevision).
			SetUpdatedBy(candidate.UpdatedBy).
			SetUpdatedAt(now)

		if len(candidate.SystemOneRefs) > 0 {
			upd.SetSystemOneRefs(slices.Clone(candidate.SystemOneRefs))
		} else {
			upd.ClearSystemOneRefs()
		}
		if len(candidate.OnFailedReasons) > 0 {
			upd.SetOnFailedReasons(cloneStringMap(candidate.OnFailedReasons))
		} else {
			upd.ClearOnFailedReasons()
		}

		affected, err := upd.Save(ctx)
		if err != nil {
			return mapError(err)
		}
		if affected == 0 {
			return store.ErrRevisionConflict
		}

		policy.ID = existing.ID.String()
		policy.ProjectID = existing.ProjectID.String()
		policy.Name = candidate.Name
		policy.Mode = mode
		policy.Revision = nextRevision
		policy.CreatedAt = existing.CreatedAt.UTC()
		policy.UpdatedAt = now
		return nil
	})
}

// DeleteMessagePolicy deletes a message policy by UUID.
func (s *MessagePolicyStore) DeleteMessagePolicy(ctx context.Context, id string) error {
	uid, err := parseGetID(id)
	if err != nil {
		return err
	}
	if err := s.client.MessagePolicy.DeleteOneID(uid).Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// CreateMessagePolicyDecision records one content-free policy evaluation decision.
func (s *MessagePolicyStore) CreateMessagePolicyDecision(ctx context.Context, decision *store.MessagePolicyDecision) error {
	if decision == nil {
		return fmt.Errorf("%w: message policy decision is nil", store.ErrInvalidInput)
	}
	projectUUID, err := parseUUID(decision.ProjectID)
	if err != nil {
		return err
	}
	if projectUUID == uuid.Nil {
		return fmt.Errorf("%w: project_id must not be nil UUID", store.ErrInvalidInput)
	}
	policyUUID, err := parseUUID(decision.PolicyID)
	if err != nil {
		return err
	}
	if policyUUID == uuid.Nil {
		return fmt.Errorf("%w: policy_id must not be nil UUID", store.ErrInvalidInput)
	}
	var msgUUID *uuid.UUID
	if decision.MessageID != "" {
		parsedMsgID, err := parseUUID(decision.MessageID)
		if err != nil {
			return err
		}
		if parsedMsgID == uuid.Nil {
			return fmt.Errorf("%w: message_id must not be nil UUID", store.ErrInvalidInput)
		}
		msgUUID = &parsedMsgID
	}

	if decision.PolicyRevision < 1 {
		return fmt.Errorf("%w: policy_revision must be >= 1", store.ErrInvalidInput)
	}
	if !store.IsValidMessagePolicyMode(decision.Mode) {
		return fmt.Errorf("%w: invalid decision mode %q", store.ErrInvalidInput, decision.Mode)
	}
	if decision.Surface == "" {
		return fmt.Errorf("%w: surface is required", store.ErrInvalidInput)
	}
	if !store.IsValidMessagePolicyClass(decision.Class) {
		return fmt.Errorf("%w: invalid decision class %q", store.ErrInvalidInput, decision.Class)
	}
	if !store.IsValidMessagePolicyDecisionAction(decision.Action) {
		return fmt.Errorf("%w: invalid decision action %q", store.ErrInvalidInput, decision.Action)
	}
	if decision.ElapsedMs < 0 {
		return fmt.Errorf("%w: elapsed_ms must be non-negative", store.ErrInvalidInput)
	}
	if len(decision.Evaluation) > 0 && !json.Valid(decision.Evaluation) {
		return fmt.Errorf("%w: evaluation must be valid JSON", store.ErrInvalidInput)
	}

	var uid uuid.UUID
	if decision.ID != "" {
		uid, err = parseUUID(decision.ID)
		if err != nil {
			return err
		}
		if uid == uuid.Nil {
			return fmt.Errorf("%w: id must not be nil UUID", store.ErrInvalidInput)
		}
	} else {
		uid = uuid.New()
	}

	createdAt := decision.CreatedAt.UTC()
	if decision.CreatedAt.IsZero() {
		createdAt = time.Now().UTC().Truncate(time.Microsecond)
	}

	create := s.client.MessagePolicyDecision.Create().
		SetID(uid).
		SetProjectID(projectUUID).
		SetPolicyID(policyUUID).
		SetPolicyName(decision.PolicyName).
		SetPolicyRevision(decision.PolicyRevision).
		SetMode(messagepolicydecision.Mode(decision.Mode)).
		SetSurface(decision.Surface).
		SetSenderKind(decision.SenderKind).
		SetSenderID(decision.SenderID).
		SetRecipientKind(decision.RecipientKind).
		SetRecipientID(decision.RecipientID).
		SetNillableMessageID(msgUUID).
		SetClass(messagepolicydecision.Class(decision.Class)).
		SetAction(messagepolicydecision.Action(decision.Action)).
		SetReason(decision.Reason).
		SetCelErrorReason(decision.CELErrorReason).
		SetEvaluation(string(decision.Evaluation)).
		SetElapsedMs(decision.ElapsedMs).
		SetCreatedAt(createdAt)

	created, err := create.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	decision.ID = created.ID.String()
	decision.CreatedAt = created.CreatedAt.UTC()
	return nil
}

// ListMessagePolicyDecisions lists decision records ordered newest first
// (created_at DESC, id DESC), with keyset pagination via opts.Cursor.
func (s *MessagePolicyStore) ListMessagePolicyDecisions(
	ctx context.Context,
	filter store.MessagePolicyDecisionFilter,
	opts store.ListOptions,
) (*store.ListResult[store.MessagePolicyDecision], error) {
	q := s.client.MessagePolicyDecision.Query()
	if filter.ProjectID != "" {
		projectUUID, err := parseUUID(filter.ProjectID)
		if err != nil {
			return nil, err
		}
		q = q.Where(messagepolicydecision.ProjectIDEQ(projectUUID))
	}
	if filter.PolicyID != "" {
		policyUUID, err := parseUUID(filter.PolicyID)
		if err != nil {
			return nil, err
		}
		q = q.Where(messagepolicydecision.PolicyIDEQ(policyUUID))
	}
	if filter.Mode != "" {
		if !store.IsValidMessagePolicyMode(filter.Mode) {
			return &store.ListResult[store.MessagePolicyDecision]{Items: []store.MessagePolicyDecision{}}, nil
		}
		q = q.Where(messagepolicydecision.ModeEQ(messagepolicydecision.Mode(filter.Mode)))
	}
	if filter.Surface != "" {
		q = q.Where(messagepolicydecision.SurfaceEQ(filter.Surface))
	}
	if filter.Class != "" {
		if !store.IsValidMessagePolicyClass(filter.Class) {
			return &store.ListResult[store.MessagePolicyDecision]{Items: []store.MessagePolicyDecision{}}, nil
		}
		q = q.Where(messagepolicydecision.ClassEQ(messagepolicydecision.Class(filter.Class)))
	}
	if filter.Action != "" {
		if !store.IsValidMessagePolicyDecisionAction(filter.Action) {
			return &store.ListResult[store.MessagePolicyDecision]{Items: []store.MessagePolicyDecision{}}, nil
		}
		q = q.Where(messagepolicydecision.ActionEQ(messagepolicydecision.Action(filter.Action)))
	}

	var total int
	if !opts.SkipTotalCount {
		var err error
		total, err = q.Clone().Count(ctx)
		if err != nil {
			return nil, mapError(err)
		}
	}

	if opts.Cursor != "" {
		cursorCreated, cursorID, err := decodeCursor(opts.Cursor)
		if err != nil {
			return nil, fmt.Errorf("invalid cursor: %w", err)
		}
		q = q.Where(messagepolicydecision.Or(
			messagepolicydecision.CreatedAtLT(cursorCreated),
			messagepolicydecision.And(
				messagepolicydecision.CreatedAtEQ(cursorCreated),
				messagepolicydecision.IDLT(cursorID),
			),
		))
	}

	limit := clampLimit(opts.Limit)
	rows, err := q.
		Order(
			messagepolicydecision.ByCreatedAt(entsql.OrderDesc()),
			messagepolicydecision.ByID(entsql.OrderDesc()),
		).
		Limit(limit + 1).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	nextCursor := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		nextCursor = encodeCursor(last.CreatedAt, last.ID.String())
	}

	items := make([]store.MessagePolicyDecision, len(rows))
	for i, r := range rows {
		items[i] = *entMessagePolicyDecisionToStore(r)
	}
	return &store.ListResult[store.MessagePolicyDecision]{
		Items:      items,
		TotalCount: total,
		NextCursor: nextCursor,
	}, nil
}

func deleteMessagePolicyDecisionIDs(ctx context.Context, client *ent.Client, ids []uuid.UUID) (int, error) {
	total := 0
	for start := 0; start < len(ids); start += messagePolicyDecisionDeleteBatch {
		chunk := ids[start:min(len(ids), start+messagePolicyDecisionDeleteBatch)]
		n, err := client.MessagePolicyDecision.Delete().
			Where(messagepolicydecision.IDIn(chunk...)).
			Exec(ctx)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

type groupedMessagePolicyDecisionProject struct {
	ProjectID uuid.UUID `json:"project_id"`
	Count     int       `json:"count"`
}

// TrimMessagePolicyDecisions trims decision records older than maxAge (when
// maxAge > 0) and then trims each project's oldest remaining rows above
// maxRowsPerProject (when maxRowsPerProject > 0), in bounded batches safe on
// both SQLite and PostgreSQL.
func (s *MessagePolicyStore) TrimMessagePolicyDecisions(
	ctx context.Context,
	now time.Time,
	maxAge time.Duration,
	maxRowsPerProject int,
) (store.MessagePolicyDecisionTrim, error) {
	now = now.UTC()
	passLimit := s.effectiveTrimPassLimit()

	var trim store.MessagePolicyDecisionTrim
	err := runInEntTx(ctx, s.client, s.inTx, func(client *ent.Client) error {
		if err := lockMessagePolicyDecisionTrim(ctx, client); err != nil {
			return err
		}
		budget := passLimit

		if maxAge > 0 && budget > 0 {
			cutoff := now.Add(-maxAge)
			agedIDs, err := client.MessagePolicyDecision.Query().
				Where(messagepolicydecision.CreatedAtLT(cutoff)).
				Order(
					messagepolicydecision.ByCreatedAt(entsql.OrderAsc()),
					messagepolicydecision.ByID(entsql.OrderAsc()),
				).
				Limit(budget + 1).
				IDs(ctx)
			if err != nil {
				return err
			}
			if len(agedIDs) > budget {
				agedIDs = agedIDs[:budget]
				trim.More = true
			}
			budget -= len(agedIDs)
			deleted, err := deleteMessagePolicyDecisionIDs(ctx, client, agedIDs)
			if err != nil {
				return err
			}
			trim.AgedDeleted = deleted
		}

		if maxRowsPerProject > 0 {
			var buckets []groupedMessagePolicyDecisionProject
			if err := client.MessagePolicyDecision.Query().
				GroupBy(messagepolicydecision.FieldProjectID).
				Aggregate(ent.Count()).
				Scan(ctx, &buckets); err != nil {
				return err
			}

			var overflowing []groupedMessagePolicyDecisionProject
			for _, b := range buckets {
				if b.Count > maxRowsPerProject {
					overflowing = append(overflowing, b)
				}
			}
			sort.Slice(overflowing, func(i, j int) bool {
				ei := overflowing[i].Count - maxRowsPerProject
				ej := overflowing[j].Count - maxRowsPerProject
				if ei != ej {
					return ei > ej
				}
				return bytes.Compare(overflowing[i].ProjectID[:], overflowing[j].ProjectID[:]) < 0
			})

			for _, b := range overflowing {
				excess := b.Count - maxRowsPerProject
				if excess <= 0 {
					continue
				}
				if budget <= 0 {
					trim.More = true
					break
				}
				toDelete := min(excess, budget)
				if excess > budget {
					trim.More = true
				}
				ids, err := client.MessagePolicyDecision.Query().
					Where(messagepolicydecision.ProjectIDEQ(b.ProjectID)).
					Order(
						messagepolicydecision.ByCreatedAt(entsql.OrderAsc()),
						messagepolicydecision.ByID(entsql.OrderAsc()),
					).
					Limit(toDelete).
					IDs(ctx)
				if err != nil {
					return err
				}
				budget -= len(ids)
				deleted, err := deleteMessagePolicyDecisionIDs(ctx, client, ids)
				if err != nil {
					return err
				}
				trim.OverflowDeleted += deleted
			}
		}

		return nil
	})
	if err != nil {
		return store.MessagePolicyDecisionTrim{}, mapError(err)
	}
	return trim, nil
}

// deleteProjectMessagePolicies removes a project's message policy decision rows
// and message policy rows as part of CompositeStore.DeleteProject. Neither table
// has a DB-level FK edge to projects, so the cascade is explicit inside the
// project-deletion transaction.
func deleteProjectMessagePolicies(ctx context.Context, client *ent.Client, projectID uuid.UUID) error {
	if _, err := client.MessagePolicyDecision.Delete().
		Where(messagepolicydecision.ProjectIDEQ(projectID)).
		Exec(ctx); err != nil {
		return err
	}
	if _, err := client.MessagePolicy.Delete().
		Where(messagepolicy.ProjectIDEQ(projectID)).
		Exec(ctx); err != nil {
		return err
	}
	return nil
}
