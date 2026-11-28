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
	"fmt"
	"math"
	"slices"
	"sort"
	"time"

	"entgo.io/ent/dialect"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/project"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/systemoneinstance"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// SystemOneInstanceStore implements store.SystemOneInstanceStore using Ent ORM.
type SystemOneInstanceStore struct {
	client *ent.Client
	inTx   bool
}

// NewSystemOneInstanceStore creates a new Ent-backed SystemOneInstanceStore.
func NewSystemOneInstanceStore(client *ent.Client) *SystemOneInstanceStore {
	return &SystemOneInstanceStore{client: client}
}

// usesRowLocks reports whether SELECT ... FOR UPDATE is supported (Postgres).
// Reads dialect directly from the driver without issuing a query.
func (s *SystemOneInstanceStore) usesRowLocks() bool {
	return s.client.Driver().Dialect() == dialect.Postgres
}

func entSystemOneInstanceToStore(row *ent.SystemOneInstance) *store.SystemOneInstance {
	inst := &store.SystemOneInstance{
		ID:       row.ID.String(),
		Name:     row.Name,
		Adapter:  row.Adapter,
		Model:    row.Model,
		Endpoint: row.Endpoint,
		Location: row.Location,
		Auth: store.SystemOneInstanceAuth{
			Kind:       string(row.AuthKind),
			SecretName: row.AuthSecretName,
			HeaderName: row.AuthHeaderName,
			Audience:   row.AuthAudience,
		},
		Limits: store.SystemOneInstanceLimits{
			MaxInFlight:    row.MaxInFlight,
			RatePerSec:     row.RatePerSec,
			MaxInputTokens: row.MaxInputTokens,
			CallTimeout:    time.Duration(row.CallTimeoutNs),
		},
		AllowPrivateNetwork: row.AllowPrivateNetwork,
		Enabled:             row.Enabled,
		Revision:            row.Revision,
		CreatedBy:           row.CreatedBy,
		UpdatedBy:           row.UpdatedBy,
		CreatedAt:           row.CreatedAt.UTC(),
		UpdatedAt:           row.UpdatedAt.UTC(),
	}
	if len(row.AuthScopes) > 0 {
		inst.Auth.Scopes = slices.Clone(row.AuthScopes)
	}
	if len(row.Options) > 0 {
		inst.Options = cloneStringMap(row.Options)
	}
	return inst
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func validateSystemOneInstanceFields(inst *store.SystemOneInstance) (string, error) {
	if inst == nil {
		return "", fmt.Errorf("%w: system one instance is nil", store.ErrInvalidInput)
	}
	if !store.IsValidSystemOneInstanceName(inst.Name) {
		return "", fmt.Errorf("%w: invalid system one instance name (must be a canonical slug)", store.ErrInvalidInput)
	}
	if inst.Adapter == "" {
		return "", fmt.Errorf("%w: adapter is required", store.ErrInvalidInput)
	}
	if inst.Endpoint == "" {
		return "", fmt.Errorf("%w: endpoint is required", store.ErrInvalidInput)
	}
	authKind := inst.Auth.Kind
	if authKind == "" {
		authKind = store.SystemOneAuthKindAPIKey
	}
	if !store.IsValidSystemOneAuthKind(authKind) {
		return "", fmt.Errorf("%w: unsupported auth kind", store.ErrInvalidInput)
	}
	if inst.Limits.MaxInFlight < 0 {
		return "", fmt.Errorf("%w: maxInFlight must be non-negative", store.ErrInvalidInput)
	}
	if inst.Limits.RatePerSec < 0 || math.IsNaN(inst.Limits.RatePerSec) || math.IsInf(inst.Limits.RatePerSec, 0) {
		return "", fmt.Errorf("%w: ratePerSec must be a finite non-negative number", store.ErrInvalidInput)
	}
	if inst.Limits.MaxInputTokens < 0 {
		return "", fmt.Errorf("%w: maxInputTokens must be non-negative", store.ErrInvalidInput)
	}
	if inst.Limits.CallTimeout < 0 {
		return "", fmt.Errorf("%w: callTimeout must be non-negative", store.ErrInvalidInput)
	}
	return authKind, nil
}

// CreateSystemOneInstance creates a new System One instance record at Revision=1.
func (s *SystemOneInstanceStore) CreateSystemOneInstance(ctx context.Context, inst *store.SystemOneInstance) error {
	authKind, err := validateSystemOneInstanceFields(inst)
	if err != nil {
		return err
	}

	var uid uuid.UUID
	if inst.ID != "" {
		uid, err = parseUUID(inst.ID)
		if err != nil {
			return err
		}
	} else {
		uid = uuid.New()
	}

	now := time.Now().UTC()
	create := s.client.SystemOneInstance.Create().
		SetID(uid).
		SetName(inst.Name).
		SetAdapter(inst.Adapter).
		SetModel(inst.Model).
		SetEndpoint(inst.Endpoint).
		SetLocation(inst.Location).
		SetAuthKind(systemoneinstance.AuthKind(authKind)).
		SetAuthSecretName(inst.Auth.SecretName).
		SetAuthHeaderName(inst.Auth.HeaderName).
		SetAuthAudience(inst.Auth.Audience).
		SetMaxInFlight(inst.Limits.MaxInFlight).
		SetRatePerSec(inst.Limits.RatePerSec).
		SetMaxInputTokens(inst.Limits.MaxInputTokens).
		SetCallTimeoutNs(int64(inst.Limits.CallTimeout)).
		SetAllowPrivateNetwork(inst.AllowPrivateNetwork).
		SetEnabled(inst.Enabled).
		SetRevision(1).
		SetCreatedBy(inst.CreatedBy).
		SetUpdatedBy(inst.UpdatedBy).
		SetCreatedAt(now).
		SetUpdatedAt(now)

	if len(inst.Auth.Scopes) > 0 {
		create.SetAuthScopes(slices.Clone(inst.Auth.Scopes))
	}
	if len(inst.Options) > 0 {
		create.SetOptions(cloneStringMap(inst.Options))
	}

	created, err := create.Save(ctx)
	if err != nil {
		return mapError(err)
	}

	inst.ID = created.ID.String()
	inst.Auth.Kind = authKind
	inst.Revision = created.Revision
	inst.CreatedAt = created.CreatedAt.UTC()
	inst.UpdatedAt = created.UpdatedAt.UTC()
	return nil
}

// GetSystemOneInstance retrieves a System One instance by UUID.
func (s *SystemOneInstanceStore) GetSystemOneInstance(ctx context.Context, id string) (*store.SystemOneInstance, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	row, err := s.client.SystemOneInstance.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entSystemOneInstanceToStore(row), nil
}

// GetSystemOneInstanceByName retrieves a System One instance by its unique slug Name.
func (s *SystemOneInstanceStore) GetSystemOneInstanceByName(ctx context.Context, name string) (*store.SystemOneInstance, error) {
	if name == "" {
		return nil, store.ErrNotFound
	}
	row, err := s.client.SystemOneInstance.Query().
		Where(systemoneinstance.NameEQ(name)).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entSystemOneInstanceToStore(row), nil
}

// ListSystemOneInstances lists System One instances ordered by Name ascending.
func (s *SystemOneInstanceStore) ListSystemOneInstances(
	ctx context.Context,
	filter store.SystemOneInstanceFilter,
	opts store.ListOptions,
) (*store.ListResult[store.SystemOneInstance], error) {
	q := s.client.SystemOneInstance.Query()
	if filter.Adapter != "" {
		q = q.Where(systemoneinstance.AdapterEQ(filter.Adapter))
	}
	if filter.Enabled != nil {
		q = q.Where(systemoneinstance.EnabledEQ(*filter.Enabled))
	}

	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	if opts.Cursor != "" {
		if !store.IsValidSystemOneInstanceName(opts.Cursor) {
			return nil, fmt.Errorf("%w: invalid system one instance cursor", store.ErrInvalidCursor)
		}
		q = q.Where(systemoneinstance.NameGT(opts.Cursor))
	}

	q = q.Order(ent.Asc(systemoneinstance.FieldName))
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
		nextCursor = rows[len(rows)-1].Name
	}

	items := make([]store.SystemOneInstance, len(rows))
	for i, r := range rows {
		items[i] = *entSystemOneInstanceToStore(r)
	}
	return &store.ListResult[store.SystemOneInstance]{
		Items:      items,
		TotalCount: total,
		NextCursor: nextCursor,
	}, nil
}

// UpdateSystemOneInstance updates an existing System One instance and bumps
// Revision by 1 whenever Adapter, Model, Endpoint, Auth, Options, or Limits
// changes.
func (s *SystemOneInstanceStore) UpdateSystemOneInstance(ctx context.Context, inst *store.SystemOneInstance) error {
	if inst == nil {
		return fmt.Errorf("%w: system one instance is nil", store.ErrInvalidInput)
	}
	if inst.ID == "" && inst.Name == "" {
		return fmt.Errorf("%w: id or name is required", store.ErrInvalidInput)
	}

	var lookupID uuid.UUID
	hasID := inst.ID != ""
	if hasID {
		var err error
		lookupID, err = parseUUID(inst.ID)
		if err != nil {
			return err
		}
	}

	run := func(client *ent.Client) error {
		q := client.SystemOneInstance.Query()
		if hasID {
			q = q.Where(systemoneinstance.IDEQ(lookupID))
		} else {
			q = q.Where(systemoneinstance.NameEQ(inst.Name))
		}
		if s.usesRowLocks() {
			q = q.ForUpdate()
		}

		existing, err := q.Only(ctx)
		if err != nil {
			return mapError(err)
		}

		candidate := inst.Clone()
		if candidate.Name == "" {
			candidate.Name = existing.Name
		} else if candidate.Name != existing.Name {
			return fmt.Errorf("%w: system one instance name is immutable (%q cannot be renamed to %q)", store.ErrInvalidInput, existing.Name, candidate.Name)
		}
		authKind, err := validateSystemOneInstanceFields(candidate)
		if err != nil {
			return err
		}
		candidate.Auth.Kind = authKind

		if inst.Revision > 0 && inst.Revision != existing.Revision {
			return store.ErrRevisionConflict
		}

		existingStore := entSystemOneInstanceToStore(existing)
		nextRevision := existing.Revision
		if store.SystemOneRuntimeConfigChanged(existingStore, candidate) {
			nextRevision = existing.Revision + 1
		}

		now := time.Now().UTC()
		upd := client.SystemOneInstance.Update().
			Where(
				systemoneinstance.IDEQ(existing.ID),
				systemoneinstance.RevisionEQ(existing.Revision),
			).
			SetAdapter(candidate.Adapter).
			SetModel(candidate.Model).
			SetEndpoint(candidate.Endpoint).
			SetLocation(candidate.Location).
			SetAuthKind(systemoneinstance.AuthKind(authKind)).
			SetAuthSecretName(candidate.Auth.SecretName).
			SetAuthHeaderName(candidate.Auth.HeaderName).
			SetAuthAudience(candidate.Auth.Audience).
			SetMaxInFlight(candidate.Limits.MaxInFlight).
			SetRatePerSec(candidate.Limits.RatePerSec).
			SetMaxInputTokens(candidate.Limits.MaxInputTokens).
			SetCallTimeoutNs(int64(candidate.Limits.CallTimeout)).
			SetAllowPrivateNetwork(candidate.AllowPrivateNetwork).
			SetEnabled(candidate.Enabled).
			SetRevision(nextRevision).
			SetUpdatedBy(candidate.UpdatedBy).
			SetUpdatedAt(now)

		if len(candidate.Auth.Scopes) > 0 {
			upd.SetAuthScopes(slices.Clone(candidate.Auth.Scopes))
		} else {
			upd.ClearAuthScopes()
		}
		if len(candidate.Options) > 0 {
			upd.SetOptions(cloneStringMap(candidate.Options))
		} else {
			upd.ClearOptions()
		}

		affected, err := upd.Save(ctx)
		if err != nil {
			return mapError(err)
		}
		if affected == 0 {
			return store.ErrRevisionConflict
		}

		inst.ID = existing.ID.String()
		inst.Name = candidate.Name
		inst.Auth.Kind = authKind
		inst.Revision = nextRevision
		inst.CreatedAt = existing.CreatedAt.UTC()
		inst.UpdatedAt = now
		return nil
	}

	if s.inTx {
		return run(s.client)
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return markTransient(fmt.Errorf("begin transaction: %w", err))
	}
	defer func() {
		_ = tx.Rollback()
	}()
	if err := run(tx.Client()); err != nil {
		return markTransient(err)
	}
	if err := tx.Commit(); err != nil {
		return markTransient(fmt.Errorf("commit transaction: %w", err))
	}
	return nil
}

// systemOnePruneFaultHook, when non-nil in tests, injects a failure inside
// pruneSystemOneInstanceTx to verify transactional rollback on delete+prune.
var systemOnePruneFaultHook func(ctx context.Context, instanceName string) error

// pruneSystemOneInstanceTx removes instanceName from every project's System One
// allowlist inside tx. Projects are locked in deterministic ID order on
// Postgres to maintain the canonical lock order (system_one_instances ->
// projects by ID ASC) and prevent deadlocks with concurrent add/set calls.
func pruneSystemOneInstanceTx(ctx context.Context, tx *ent.Tx, useLocks bool, instanceName string) (int, error) {
	if systemOnePruneFaultHook != nil {
		if err := systemOnePruneFaultHook(ctx, instanceName); err != nil {
			return 0, err
		}
	}
	q := tx.Project.Query().
		Where(project.SystemOneAllowedInstancesNotNil()).
		Order(ent.Asc(project.FieldID))
	if useLocks {
		q = q.ForUpdate()
	}
	projects, err := q.All(ctx)
	if err != nil {
		return 0, mapError(err)
	}

	updated := 0
	for _, p := range projects {
		found := false
		filtered := make([]string, 0, len(p.SystemOneAllowedInstances))
		for _, existing := range p.SystemOneAllowedInstances {
			if existing == instanceName {
				found = true
				continue
			}
			filtered = append(filtered, existing)
		}
		if !found {
			continue
		}
		sort.Strings(filtered)
		upd := tx.Project.UpdateOneID(p.ID)
		if len(filtered) == 0 {
			upd.ClearSystemOneAllowedInstances()
		} else {
			upd.SetSystemOneAllowedInstances(filtered)
		}
		if _, err := upd.Save(ctx); err != nil {
			return 0, mapError(err)
		}
		updated++
	}
	return updated, nil
}

// DeleteSystemOneInstance atomically prunes the instance name from all project
// allowlists and deletes the System One instance by UUID in a single
// transaction. If pruning fails, the transaction rolls back so no orphan
// allowlist entry can survive to authorize a future recreated instance.
func (s *SystemOneInstanceStore) DeleteSystemOneInstance(ctx context.Context, id string) error {
	uid, err := parseGetID(id)
	if err != nil {
		return err
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return markTransient(fmt.Errorf("begin transaction: %w", err))
	}
	defer func() { _ = tx.Rollback() }()

	q := tx.SystemOneInstance.Query().Where(systemoneinstance.IDEQ(uid))
	if s.usesRowLocks() {
		q = q.ForUpdate()
	}
	inst, err := q.Only(ctx)
	if err != nil {
		return mapError(err)
	}
	if _, err := pruneSystemOneInstanceTx(ctx, tx, s.usesRowLocks(), inst.Name); err != nil {
		return err
	}
	if err := tx.SystemOneInstance.DeleteOneID(inst.ID).Exec(ctx); err != nil {
		return mapError(err)
	}
	if err := tx.Commit(); err != nil {
		return markTransient(fmt.Errorf("commit transaction: %w", err))
	}
	return nil
}

// DeleteSystemOneInstanceByName atomically prunes the instance name from all
// project allowlists and deletes the System One instance by its unique slug
// Name in a single transaction.
func (s *SystemOneInstanceStore) DeleteSystemOneInstanceByName(ctx context.Context, name string) error {
	if name == "" {
		return store.ErrNotFound
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return markTransient(fmt.Errorf("begin transaction: %w", err))
	}
	defer func() { _ = tx.Rollback() }()

	q := tx.SystemOneInstance.Query().Where(systemoneinstance.NameEQ(name))
	if s.usesRowLocks() {
		q = q.ForUpdate()
	}
	inst, err := q.Only(ctx)
	if err != nil {
		return mapError(err)
	}
	if _, err := pruneSystemOneInstanceTx(ctx, tx, s.usesRowLocks(), inst.Name); err != nil {
		return err
	}
	if err := tx.SystemOneInstance.DeleteOneID(inst.ID).Exec(ctx); err != nil {
		return mapError(err)
	}
	if err := tx.Commit(); err != nil {
		return markTransient(fmt.Errorf("commit transaction: %w", err))
	}
	return nil
}

// ListSystemOneInstanceProjects returns the minimal references (ID, Name) of
// all projects whose System One allowlist contains instanceName, ordered by
// Name ascending then ID ascending. Returns an empty slice when no project
// allowlists the instance.
func (s *SystemOneInstanceStore) ListSystemOneInstanceProjects(ctx context.Context, instanceName string) ([]store.SystemOneInstanceProjectRef, error) {
	if instanceName == "" {
		return []store.SystemOneInstanceProjectRef{}, nil
	}
	rows, err := s.client.Project.Query().
		Where(project.SystemOneAllowedInstancesNotNil()).
		Order(ent.Asc(project.FieldName), ent.Asc(project.FieldID)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]store.SystemOneInstanceProjectRef, 0)
	for _, p := range rows {
		if slices.Contains(p.SystemOneAllowedInstances, instanceName) {
			out = append(out, store.SystemOneInstanceProjectRef{
				ID:   p.ID.String(),
				Name: p.Name,
			})
		}
	}
	return out, nil
}
