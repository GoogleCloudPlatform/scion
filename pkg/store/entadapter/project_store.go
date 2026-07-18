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
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/project"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/projectcontributor"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/projectsyncstate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// maxCASRetries bounds the optimistic-concurrency retry loop on runtime broker
// read-modify-write paths (heartbeat and full update). A handful of retries is
// ample: contention on a single broker row is low and each retry only re-reads
// the lock_version token.
const maxCASRetries = 5

// ProjectStore implements store.ProjectStore, store.RuntimeBrokerStore,
// store.ProjectProviderStore and store.ProjectSyncStateStore using Ent ORM.
//
// These four interfaces form the project/broker domain (tables: projects,
// runtime_brokers, project_contributors, project_sync_state). They are grouped
// in one adapter because they are tightly coupled — projects reference brokers
// through contributors, and several computed fields on a project are derived
// from broker/contributor state.
type ProjectStore struct {
	client *ent.Client
}

// NewProjectStore creates a new Ent-backed ProjectStore.
func NewProjectStore(client *ent.Client) *ProjectStore {
	return &ProjectStore{client: client}
}

// =============================================================================
// JSON helpers
//
// Several columns are stored as raw JSON strings (matching the dual-write
// behavior of the legacy SQLite store) rather than typed Ent JSON fields, to
// keep the schema dialect-neutral and free of store/api type imports.
// =============================================================================

// marshalRawJSON marshals v to a JSON string. A nil pointer/slice/map marshals
// to "null", which unmarshalRawJSON treats as "leave the target untouched".
func marshalRawJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// unmarshalRawJSON unmarshals s into v, tolerating empty/"null" payloads.
func unmarshalRawJSON(s string, v any) {
	if s == "" || s == "null" {
		return
	}
	_ = json.Unmarshal([]byte(s), v)
}

// =============================================================================
// Project model mapping
// =============================================================================

// entProjectToStore converts an Ent Project entity to a store.Project model.
// Computed fields (AgentCount, ActiveBrokerCount, ProjectType, OwnerName) are
// not set here; callers that need them invoke populateProjectComputed.
func entProjectToStore(p *ent.Project) *store.Project {
	sp := &store.Project{
		ID:          p.ID.String(),
		Name:        p.Name,
		Slug:        p.Slug,
		Labels:      p.Labels,
		Annotations: p.Annotations,
		Created:     p.Created,
		Updated:     p.Updated,
		CreatedBy:   p.CreatedBy,
		OwnerID:     p.OwnerID,
		Visibility:  p.Visibility,
	}
	if p.GitRemote != nil {
		sp.GitRemote = *p.GitRemote
	}
	if p.DefaultRuntimeBrokerID != nil {
		sp.DefaultRuntimeBrokerID = *p.DefaultRuntimeBrokerID
	}
	if p.GithubInstallationID != nil {
		sp.GitHubInstallationID = p.GithubInstallationID
	}
	if p.SharedDirs != "" {
		var dirs []api.SharedDir
		unmarshalRawJSON(p.SharedDirs, &dirs)
		sp.SharedDirs = dirs
	}
	if p.GithubPermissions != "" {
		sp.GitHubPermissions = &store.GitHubTokenPermissions{}
		unmarshalRawJSON(p.GithubPermissions, sp.GitHubPermissions)
	}
	if p.GithubAppStatus != "" {
		sp.GitHubAppStatus = &store.GitHubAppProjectStatus{}
		unmarshalRawJSON(p.GithubAppStatus, sp.GitHubAppStatus)
	}
	if p.GitIdentity != "" {
		sp.GitIdentity = &store.GitIdentityConfig{}
		unmarshalRawJSON(p.GitIdentity, sp.GitIdentity)
	}
	return sp
}

// CreateProject creates a new project record.
func (s *ProjectStore) CreateProject(ctx context.Context, p *store.Project) error {
	uid, err := parseUUID(p.ID)
	if err != nil {
		return err
	}

	create := s.client.Project.Create().
		SetID(uid).
		SetName(p.Name).
		SetSlug(p.Slug).
		SetCreatedBy(p.CreatedBy).
		SetOwnerID(p.OwnerID)

	if p.Visibility != "" {
		create.SetVisibility(p.Visibility)
	}
	if p.GitRemote != "" {
		create.SetGitRemote(p.GitRemote)
	}
	if p.DefaultRuntimeBrokerID != "" {
		create.SetDefaultRuntimeBrokerID(p.DefaultRuntimeBrokerID)
	}
	if p.Labels != nil {
		create.SetLabels(p.Labels)
	}
	if p.Annotations != nil {
		create.SetAnnotations(p.Annotations)
	}
	if len(p.SharedDirs) > 0 {
		create.SetSharedDirs(marshalRawJSON(p.SharedDirs))
	}
	if p.GitHubInstallationID != nil {
		create.SetGithubInstallationID(*p.GitHubInstallationID)
	}
	if p.GitHubPermissions != nil {
		create.SetGithubPermissions(marshalRawJSON(p.GitHubPermissions))
	}
	if p.GitHubAppStatus != nil {
		create.SetGithubAppStatus(marshalRawJSON(p.GitHubAppStatus))
	}
	if p.GitIdentity != nil {
		create.SetGitIdentity(marshalRawJSON(p.GitIdentity))
	}

	created, err := create.Save(ctx)
	if err != nil {
		return mapError(err)
	}

	p.Created = created.Created
	p.Updated = created.Updated
	if p.Visibility == "" {
		p.Visibility = created.Visibility
	}
	return nil
}

// GetProject retrieves a project by ID, including computed fields.
func (s *ProjectStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}

	p, err := s.client.Project.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}

	sp := entProjectToStore(p)
	if err := s.populateProjectComputed(ctx, sp, uid); err != nil {
		return nil, err
	}
	return sp, nil
}

// GetProjectBySlug retrieves a project by its exact (case-sensitive) slug.
func (s *ProjectStore) GetProjectBySlug(ctx context.Context, slug string) (*store.Project, error) {
	p, err := s.client.Project.Query().Where(project.SlugEQ(slug)).Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	sp := entProjectToStore(p)
	if err := s.populateProjectComputed(ctx, sp, p.ID); err != nil {
		return nil, err
	}
	return sp, nil
}

// GetProjectBySlugCaseInsensitive retrieves a project by slug, ignoring case.
func (s *ProjectStore) GetProjectBySlugCaseInsensitive(ctx context.Context, slug string) (*store.Project, error) {
	p, err := s.client.Project.Query().Where(project.SlugEqualFold(slug)).First(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	sp := entProjectToStore(p)
	if err := s.populateProjectComputed(ctx, sp, p.ID); err != nil {
		return nil, err
	}
	return sp, nil
}

// GetProjectsByGitRemote returns all projects matching the git remote URL,
// ordered by creation time ascending. Returns an empty slice if none match.
func (s *ProjectStore) GetProjectsByGitRemote(ctx context.Context, gitRemote string) ([]*store.Project, error) {
	rows, err := s.client.Project.Query().
		Where(project.GitRemoteEQ(gitRemote)).
		Order(ent.Asc(project.FieldCreated)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	projects := make([]*store.Project, 0, len(rows))
	for _, p := range rows {
		sp := entProjectToStore(p)
		if err := s.populateProjectComputed(ctx, sp, p.ID); err != nil {
			return nil, err
		}
		projects = append(projects, sp)
	}
	return projects, nil
}

// NextAvailableSlug returns baseSlug if free, else baseSlug-1, baseSlug-2, ...
func (s *ProjectStore) NextAvailableSlug(ctx context.Context, baseSlug string) (string, error) {
	exists, err := s.client.Project.Query().Where(project.SlugEQ(baseSlug)).Exist(ctx)
	if err != nil {
		return "", err
	}
	if !exists {
		return baseSlug, nil
	}
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s-%d", baseSlug, i)
		exists, err := s.client.Project.Query().Where(project.SlugEQ(candidate)).Exist(ctx)
		if err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
	}
}

// UpdateProject updates an existing project.
func (s *ProjectStore) UpdateProject(ctx context.Context, p *store.Project) error {
	uid, err := parseUUID(p.ID)
	if err != nil {
		return err
	}

	update := s.client.Project.UpdateOneID(uid).
		SetName(p.Name).
		SetSlug(p.Slug).
		SetOwnerID(p.OwnerID).
		SetVisibility(p.Visibility)

	if p.GitRemote != "" {
		update.SetGitRemote(p.GitRemote)
	} else {
		update.ClearGitRemote()
	}
	if p.DefaultRuntimeBrokerID != "" {
		update.SetDefaultRuntimeBrokerID(p.DefaultRuntimeBrokerID)
	} else {
		update.ClearDefaultRuntimeBrokerID()
	}
	if p.Labels != nil {
		update.SetLabels(p.Labels)
	} else {
		update.ClearLabels()
	}
	if p.Annotations != nil {
		update.SetAnnotations(p.Annotations)
	} else {
		update.ClearAnnotations()
	}
	if len(p.SharedDirs) > 0 {
		update.SetSharedDirs(marshalRawJSON(p.SharedDirs))
	} else {
		update.ClearSharedDirs()
	}
	if p.GitHubInstallationID != nil {
		update.SetGithubInstallationID(*p.GitHubInstallationID)
	} else {
		update.ClearGithubInstallationID()
	}
	if p.GitHubPermissions != nil {
		update.SetGithubPermissions(marshalRawJSON(p.GitHubPermissions))
	} else {
		update.ClearGithubPermissions()
	}
	if p.GitHubAppStatus != nil {
		update.SetGithubAppStatus(marshalRawJSON(p.GitHubAppStatus))
	} else {
		update.ClearGithubAppStatus()
	}
	if p.GitIdentity != nil {
		update.SetGitIdentity(marshalRawJSON(p.GitIdentity))
	} else {
		update.ClearGitIdentity()
	}

	updated, err := update.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	p.Updated = updated.Updated
	return nil
}

// DeleteProject removes a project by ID.
func (s *ProjectStore) DeleteProject(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	if err := s.client.Project.DeleteOneID(uid).Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// ListProjects returns projects matching the filter criteria.
func (s *ProjectStore) ListProjects(ctx context.Context, filter store.ProjectFilter, opts store.ListOptions) (*store.ListResult[store.Project], error) {
	query := s.client.Project.Query()

	// Membership / ownership filtering mirrors the SQLite precedence:
	// MemberOrOwnerIDs > MemberProjectIDs > OwnerID.
	switch {
	case len(filter.MemberOrOwnerIDs) > 0:
		ids, err := parseUUIDs(filter.MemberOrOwnerIDs)
		if err != nil {
			return nil, err
		}
		if filter.OwnerID != "" {
			query.Where(project.Or(project.IDIn(ids...), project.OwnerIDEQ(filter.OwnerID)))
		} else {
			query.Where(project.IDIn(ids...))
		}
	case len(filter.MemberProjectIDs) > 0:
		ids, err := parseUUIDs(filter.MemberProjectIDs)
		if err != nil {
			return nil, err
		}
		query.Where(project.IDIn(ids...))
	case filter.OwnerID != "":
		query.Where(project.OwnerIDEQ(filter.OwnerID))
	}

	if filter.ExcludeOwnerID != "" {
		query.Where(project.OwnerIDNEQ(filter.ExcludeOwnerID))
	}
	if filter.Visibility != "" {
		query.Where(project.VisibilityEQ(filter.Visibility))
	}
	if filter.GitRemote != "" {
		query.Where(project.GitRemoteEQ(filter.GitRemote))
	} else if filter.GitRemotePrefix != "" {
		query.Where(project.GitRemoteHasPrefix(filter.GitRemotePrefix))
	}
	if filter.BrokerID != "" {
		brokerUID, err := parseUUID(filter.BrokerID)
		if err != nil {
			return nil, err
		}
		projectIDs, err := s.client.ProjectContributor.Query().
			Where(projectcontributor.BrokerIDEQ(brokerUID)).
			Select(projectcontributor.FieldProjectID).
			Strings(ctx)
		if err != nil {
			return nil, err
		}
		ids, err := parseUUIDs(projectIDs)
		if err != nil {
			return nil, err
		}
		query.Where(project.IDIn(ids...))
	}
	if filter.Name != "" {
		query.Where(project.NameEqualFold(filter.Name))
	}
	if filter.Slug != "" {
		query.Where(project.SlugEqualFold(filter.Slug))
	}

	totalCount, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	if opts.Cursor != "" {
		pred, err := s.projectCursorPredicate(ctx, opts.Cursor)
		if err != nil {
			return nil, err
		}
		query.Where(pred)
	}

	rows, err := query.
		Order(ent.Desc(project.FieldCreated), ent.Desc(project.FieldID)).
		Limit(limit + 1).
		All(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]store.Project, 0, len(rows))
	for _, p := range rows {
		sp := entProjectToStore(p)
		if err := s.populateProjectComputed(ctx, sp, p.ID); err != nil {
			return nil, err
		}
		items = append(items, *sp)
	}

	result := &store.ListResult[store.Project]{
		Items:      items,
		TotalCount: totalCount,
	}
	if len(items) > limit {
		result.NextCursor = items[limit-1].ID
		result.Items = items[:limit]
	}
	return result, nil
}

// projectCursorPredicate builds the keyset predicate for paginating after the
// project identified by cursor (a project ID).
func (s *ProjectStore) projectCursorPredicate(ctx context.Context, cursor string) (predicate.Project, error) {
	cursorUID, err := parseUUID(cursor)
	if err != nil {
		return nil, fmt.Errorf("invalid cursor: %w", err)
	}
	c, err := s.client.Project.Get(ctx, cursorUID)
	if err != nil {
		return nil, fmt.Errorf("invalid cursor: %w", mapError(err))
	}
	return project.Or(
		project.CreatedLT(c.Created),
		project.And(project.CreatedEQ(c.Created), project.IDLT(cursorUID)),
	), nil
}

// populateProjectComputed fills the computed (non-persisted) fields on a project:
// AgentCount, ActiveBrokerCount and ProjectType. This mirrors the derivations
// performed by the legacy SQLite store on read.
func (s *ProjectStore) populateProjectComputed(ctx context.Context, p *store.Project, uid uuid.UUID) error {
	agentCount, err := s.client.Agent.Query().Where(agent.ProjectIDEQ(uid)).Count(ctx)
	if err != nil {
		return err
	}
	p.AgentCount = agentCount

	contribs, err := s.client.ProjectContributor.Query().
		Where(projectcontributor.ProjectIDEQ(uid)).
		All(ctx)
	if err != nil {
		return err
	}

	onlineContrib := 0
	contribBrokerIDs := make([]uuid.UUID, 0, len(contribs))
	linked := false
	for _, c := range contribs {
		contribBrokerIDs = append(contribBrokerIDs, c.BrokerID)
		if c.Status == store.BrokerStatusOnline {
			onlineContrib++
		}
		// A contributor with a local path outside ~/.scion/projects/ indicates a
		// pre-existing local project that was linked to the hub.
		if c.LocalPath != "" && !strings.Contains(c.LocalPath, "/.scion/projects/") {
			linked = true
		}
	}

	autoQuery := s.client.RuntimeBroker.Query().Where(
		runtimebroker.AutoProvide(true),
		runtimebroker.StatusEQ(store.BrokerStatusOnline),
	)
	if len(contribBrokerIDs) > 0 {
		autoQuery = autoQuery.Where(runtimebroker.IDNotIn(contribBrokerIDs...))
	}
	autoOnline, err := autoQuery.Count(ctx)
	if err != nil {
		return err
	}
	p.ActiveBrokerCount = onlineContrib + autoOnline

	if linked {
		p.ProjectType = store.ProjectTypeLinked
	} else {
		p.ProjectType = store.ProjectTypeHubManaged
	}
	return nil
}

// parseUUIDs parses a slice of string UUIDs, skipping any that fail to parse.
func parseUUIDs(ids []string) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		uid, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		out = append(out, uid)
	}
	return out, nil
}

// =============================================================================
// RuntimeBroker operations
// =============================================================================

// entBrokerToStore converts an Ent RuntimeBroker entity to a store model.
func entBrokerToStore(b *ent.RuntimeBroker) *store.RuntimeBroker {
	sb := &store.RuntimeBroker{
		ID:              b.ID.String(),
		Name:            b.Name,
		Slug:            b.Slug,
		Version:         b.Version,
		Status:          b.Status,
		ConnectionState: b.ConnectionState,
		Endpoint:        b.Endpoint,
		AutoProvide:     b.AutoProvide,
		Created:         b.Created,
		Updated:         b.Updated,
		CreatedBy:       b.CreatedBy,
	}
	if b.LastHeartbeat != nil {
		sb.LastHeartbeat = *b.LastHeartbeat
	}
	sb.ConnectedHubID = b.ConnectedHubID
	sb.ConnectedSessionID = b.ConnectedSessionID
	sb.ConnectedAt = b.ConnectedAt
	unmarshalRawJSON(b.Capabilities, &sb.Capabilities)
	// Profiles are persisted in the "runtimes" column (legacy naming).
	unmarshalRawJSON(b.Runtimes, &sb.Profiles)
	unmarshalRawJSON(b.Labels, &sb.Labels)
	unmarshalRawJSON(b.Annotations, &sb.Annotations)
	return sb
}

// CreateRuntimeBroker creates a new runtime broker record.
func (s *ProjectStore) CreateRuntimeBroker(ctx context.Context, b *store.RuntimeBroker) error {
	uid, err := parseUUID(b.ID)
	if err != nil {
		return err
	}

	create := s.client.RuntimeBroker.Create().
		SetID(uid).
		SetName(b.Name).
		SetSlug(b.Slug).
		SetEndpoint(b.Endpoint).
		SetAutoProvide(b.AutoProvide).
		SetCapabilities(marshalRawJSON(b.Capabilities)).
		SetRuntimes(marshalRawJSON(b.Profiles)).
		SetLabels(marshalRawJSON(b.Labels)).
		SetAnnotations(marshalRawJSON(b.Annotations))

	if b.Version != "" {
		create.SetVersion(b.Version)
	}
	if b.Status != "" {
		create.SetStatus(b.Status)
	}
	if b.ConnectionState != "" {
		create.SetConnectionState(b.ConnectionState)
	}
	if !b.LastHeartbeat.IsZero() {
		create.SetLastHeartbeat(b.LastHeartbeat)
	}
	if b.CreatedBy != "" {
		create.SetCreatedBy(b.CreatedBy)
	}
	if b.ConnectedHubID != nil {
		create.SetConnectedHubID(*b.ConnectedHubID)
	}
	if b.ConnectedSessionID != nil {
		create.SetConnectedSessionID(*b.ConnectedSessionID)
	}
	if b.ConnectedAt != nil {
		create.SetConnectedAt(*b.ConnectedAt)
	}

	created, err := create.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	b.Created = created.Created
	b.Updated = created.Updated
	if b.Status == "" {
		b.Status = created.Status
	}
	if b.ConnectionState == "" {
		b.ConnectionState = created.ConnectionState
	}
	return nil
}

// GetRuntimeBroker retrieves a runtime broker by ID.
func (s *ProjectStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	b, err := s.client.RuntimeBroker.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entBrokerToStore(b), nil
}

// GetRuntimeBrokerByName retrieves a runtime broker by name (case-insensitive).
func (s *ProjectStore) GetRuntimeBrokerByName(ctx context.Context, name string) (*store.RuntimeBroker, error) {
	b, err := s.client.RuntimeBroker.Query().
		Where(runtimebroker.NameEqualFold(name)).
		First(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entBrokerToStore(b), nil
}

// UpdateRuntimeBroker updates an existing runtime broker using an optimistic
// concurrency (version-CAS) loop on the internal lock_version token so that
// concurrent writers cannot silently clobber one another. This is portable
// across SQLite (tests) and Postgres (production) without SELECT ... FOR UPDATE.
func (s *ProjectStore) UpdateRuntimeBroker(ctx context.Context, b *store.RuntimeBroker) error {
	uid, err := parseUUID(b.ID)
	if err != nil {
		return err
	}

	now := time.Now()
	for attempt := 0; attempt < maxCASRetries; attempt++ {
		cur, err := s.client.RuntimeBroker.Get(ctx, uid)
		if err != nil {
			return mapError(err)
		}

		update := s.client.RuntimeBroker.Update().
			Where(runtimebroker.IDEQ(uid), runtimebroker.LockVersionEQ(cur.LockVersion)).
			SetName(b.Name).
			SetSlug(b.Slug).
			SetVersion(b.Version).
			SetStatus(b.Status).
			SetConnectionState(b.ConnectionState).
			SetLastHeartbeat(b.LastHeartbeat).
			SetCapabilities(marshalRawJSON(b.Capabilities)).
			SetRuntimes(marshalRawJSON(b.Profiles)).
			SetLabels(marshalRawJSON(b.Labels)).
			SetAnnotations(marshalRawJSON(b.Annotations)).
			SetEndpoint(b.Endpoint).
			SetAutoProvide(b.AutoProvide).
			SetUpdated(now).
			AddLockVersion(1)
		if b.ConnectedHubID != nil {
			update.SetConnectedHubID(*b.ConnectedHubID)
		} else {
			update.ClearConnectedHubID()
		}
		if b.ConnectedSessionID != nil {
			update.SetConnectedSessionID(*b.ConnectedSessionID)
		} else {
			update.ClearConnectedSessionID()
		}
		if b.ConnectedAt != nil {
			update.SetConnectedAt(*b.ConnectedAt)
		} else {
			update.ClearConnectedAt()
		}
		affected, err := update.Save(ctx)
		if err != nil {
			return mapError(err)
		}
		if affected == 1 {
			b.Updated = now
			return nil
		}
		// affected == 0: another writer advanced lock_version between our read
		// and write — retry with the fresh value.
	}
	return store.ErrVersionConflict
}

// DeleteRuntimeBroker removes a runtime broker by ID.
func (s *ProjectStore) DeleteRuntimeBroker(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	if err := s.client.RuntimeBroker.DeleteOneID(uid).Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// ListRuntimeBrokers returns runtime brokers matching the filter criteria.
func (s *ProjectStore) ListRuntimeBrokers(ctx context.Context, filter store.RuntimeBrokerFilter, opts store.ListOptions) (*store.ListResult[store.RuntimeBroker], error) {
	query := s.client.RuntimeBroker.Query()

	if filter.Status != "" {
		query.Where(runtimebroker.StatusEQ(filter.Status))
	}
	if filter.ProjectID != "" {
		projectUID, err := parseUUID(filter.ProjectID)
		if err != nil {
			return nil, err
		}
		brokerIDStrs, err := s.client.ProjectContributor.Query().
			Where(projectcontributor.ProjectIDEQ(projectUID)).
			Select(projectcontributor.FieldBrokerID).
			Strings(ctx)
		if err != nil {
			return nil, err
		}
		brokerIDs, err := parseUUIDs(brokerIDStrs)
		if err != nil {
			return nil, err
		}
		// A broker provides for a project if it is an explicit contributor OR it
		// is configured to auto-provide for all projects.
		if len(brokerIDs) > 0 {
			query.Where(runtimebroker.Or(
				runtimebroker.IDIn(brokerIDs...),
				runtimebroker.AutoProvide(true),
			))
		} else {
			query.Where(runtimebroker.AutoProvide(true))
		}
	}
	if filter.Name != "" {
		query.Where(runtimebroker.NameEqualFold(filter.Name))
	}
	if filter.AutoProvide != nil {
		query.Where(runtimebroker.AutoProvide(*filter.AutoProvide))
	}

	totalCount, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	rows, err := query.
		Order(ent.Desc(runtimebroker.FieldCreated)).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]store.RuntimeBroker, 0, len(rows))
	for _, b := range rows {
		items = append(items, *entBrokerToStore(b))
	}
	return &store.ListResult[store.RuntimeBroker]{
		Items:      items,
		TotalCount: totalCount,
	}, nil
}

// FindEmbeddedBroker returns the single embedded broker if exactly one exists,
// or nil if zero or multiple embedded brokers are found. "Embedded" means the
// broker's labels JSON contains {"scion.io/broker-role": "embedded"}.
// Used to recover the broker ID from the DB when settings are lost.
func (s *ProjectStore) FindEmbeddedBroker(ctx context.Context) (*store.RuntimeBroker, error) {
	brokers, err := s.client.RuntimeBroker.Query().
		Where(brokerLabelContains("scion.io/broker-role", "embedded")).
		Limit(2). // Only need to know if there's exactly one
		All(ctx)
	if err != nil {
		return nil, err
	}
	if len(brokers) != 1 {
		return nil, nil // Zero or ambiguous — caller should generate a new ID
	}
	return entBrokerToStore(brokers[0]), nil
}

// brokerLabelContains returns an Ent predicate restricting results to runtime
// brokers whose `labels` JSON object contains the given key-value pair.
// This is the RuntimeBroker equivalent of the agent labelContains in dialect.go.
func brokerLabelContains(key, value string) predicate.RuntimeBroker {
	return func(s *entsql.Selector) {
		col := s.C(runtimebroker.FieldLabels)
		switch s.Dialect() {
		case dialect.Postgres:
			s.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString(col).
					WriteString(" @> ").
					Arg(fmt.Sprintf(`{%q:%q}`, key, value)).
					WriteString("::jsonb")
			}))
		default: // SQLite
			s.Where(entsql.P(func(b *entsql.Builder) {
				escapedKey := strings.ReplaceAll(key, `"`, `\"`)
				b.WriteString("json_extract(").
					WriteString(col).
					WriteString(", ").
					Arg(fmt.Sprintf(`$."%s"`, escapedKey)).
					WriteString(") = ").
					Arg(value)
			}))
		}
	}
}

// UpdateRuntimeBrokerHeartbeat updates the broker's status and last-heartbeat
// timestamp. It uses the same version-CAS loop as UpdateRuntimeBroker so that a
// high-frequency heartbeat cannot lose an interleaved write; the bump on
// lock_version serializes concurrent heartbeats on both SQLite and Postgres.
func (s *ProjectStore) UpdateRuntimeBrokerHeartbeat(ctx context.Context, id string, status string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}

	now := time.Now()
	for attempt := 0; attempt < maxCASRetries; attempt++ {
		cur, err := s.client.RuntimeBroker.Get(ctx, uid)
		if err != nil {
			return mapError(err)
		}
		affected, err := s.client.RuntimeBroker.Update().
			Where(runtimebroker.IDEQ(uid), runtimebroker.LockVersionEQ(cur.LockVersion)).
			SetStatus(status).
			SetLastHeartbeat(now).
			SetUpdated(now).
			AddLockVersion(1).
			Save(ctx)
		if err != nil {
			return mapError(err)
		}
		if affected == 1 {
			return nil
		}
	}
	return store.ErrVersionConflict
}

// ClaimRuntimeBrokerConnection records this hub instance as the owner of the
// broker's live control-channel socket. The newest connection wins
// (unconditional claim — mirrors a fresh socket replacing an old one): it sets
// the affinity columns and, in the same CAS write, bumps status to online and
// refreshes last_heartbeat. Uses the lock_version optimistic-concurrency loop,
// like UpdateRuntimeBrokerHeartbeat.
func (s *ProjectStore) ClaimRuntimeBrokerConnection(ctx context.Context, brokerID, hubInstanceID, sessionID string) error {
	uid, err := parseUUID(brokerID)
	if err != nil {
		return err
	}

	now := time.Now()
	for attempt := 0; attempt < maxCASRetries; attempt++ {
		cur, err := s.client.RuntimeBroker.Get(ctx, uid)
		if err != nil {
			return mapError(err)
		}
		affected, err := s.client.RuntimeBroker.Update().
			Where(runtimebroker.IDEQ(uid), runtimebroker.LockVersionEQ(cur.LockVersion)).
			SetConnectedHubID(hubInstanceID).
			SetConnectedSessionID(sessionID).
			SetConnectedAt(now).
			SetStatus(store.BrokerStatusOnline).
			SetLastHeartbeat(now).
			SetUpdated(now).
			AddLockVersion(1).
			Save(ctx)
		if err != nil {
			return mapError(err)
		}
		if affected == 1 {
			return nil
		}
	}
	return store.ErrVersionConflict
}

// ReleaseRuntimeBrokerConnection clears the broker's affinity ONLY IF it still
// names (hubInstanceID, sessionID) — a compare-and-clear that fixes the
// disconnect-race: a delayed disconnect from a stale owner/session must not
// clobber a live owner. Returns cleared=true when this caller owned the
// affinity and it was cleared; cleared=false (no-op) when affinity has already
// moved (or was already clear). Does not change status — the caller decides
// whether to stamp offline based on cleared.
func (s *ProjectStore) ReleaseRuntimeBrokerConnection(ctx context.Context, brokerID, hubInstanceID, sessionID string) (bool, error) {
	uid, err := parseUUID(brokerID)
	if err != nil {
		return false, err
	}

	now := time.Now()
	for attempt := 0; attempt < maxCASRetries; attempt++ {
		cur, err := s.client.RuntimeBroker.Get(ctx, uid)
		if err != nil {
			return false, mapError(err)
		}
		// Compare: only clear if affinity still names this exact (hub, session).
		if cur.ConnectedHubID == nil || *cur.ConnectedHubID != hubInstanceID ||
			cur.ConnectedSessionID == nil || *cur.ConnectedSessionID != sessionID {
			return false, nil
		}
		affected, err := s.client.RuntimeBroker.Update().
			Where(runtimebroker.IDEQ(uid), runtimebroker.LockVersionEQ(cur.LockVersion)).
			ClearConnectedHubID().
			ClearConnectedSessionID().
			ClearConnectedAt().
			SetUpdated(now).
			AddLockVersion(1).
			Save(ctx)
		if err != nil {
			return false, mapError(err)
		}
		if affected == 1 {
			return true, nil
		}
		// affected==0: lock_version moved under us; re-read and re-evaluate the
		// compare on the next iteration (affinity may have moved away).
	}
	return false, store.ErrVersionConflict
}

// ReleaseAndMarkBrokerOffline atomically clears broker affinity AND stamps
// status=offline in a single CAS write, ONLY IF affinity still names
// (hubInstanceID, sessionID). This eliminates the TOCTOU race between a
// separate release and a separate offline stamp: if a concurrent reconnect
// has already claimed the broker with a new session, the compare fails and
// this is a no-op — the new connection's online status is not clobbered.
func (s *ProjectStore) ReleaseAndMarkBrokerOffline(ctx context.Context, brokerID, hubInstanceID, sessionID string) (bool, error) {
	uid, err := parseUUID(brokerID)
	if err != nil {
		return false, err
	}

	now := time.Now()
	for attempt := 0; attempt < maxCASRetries; attempt++ {
		cur, err := s.client.RuntimeBroker.Get(ctx, uid)
		if err != nil {
			return false, mapError(err)
		}
		if cur.ConnectedHubID == nil || *cur.ConnectedHubID != hubInstanceID ||
			cur.ConnectedSessionID == nil || *cur.ConnectedSessionID != sessionID {
			return false, nil
		}
		affected, err := s.client.RuntimeBroker.Update().
			Where(runtimebroker.IDEQ(uid), runtimebroker.LockVersionEQ(cur.LockVersion)).
			ClearConnectedHubID().
			ClearConnectedSessionID().
			ClearConnectedAt().
			SetStatus(store.BrokerStatusOffline).
			SetLastHeartbeat(now).
			SetUpdated(now).
			AddLockVersion(1).
			Save(ctx)
		if err != nil {
			return false, mapError(err)
		}
		if affected == 1 {
			return true, nil
		}
	}
	return false, store.ErrVersionConflict
}

// MarkBrokerOffline sets a broker's status to offline. Used during startup
// repair to ensure stale broker records that were never properly shut down are
// explicitly marked offline. Uses the version-CAS loop for safe concurrency.
// Returns ErrNotFound if the broker doesn't exist.
func (s *ProjectStore) MarkBrokerOffline(ctx context.Context, brokerID string) error {
	uid, err := parseUUID(brokerID)
	if err != nil {
		return err
	}

	now := time.Now()
	for attempt := 0; attempt < maxCASRetries; attempt++ {
		cur, err := s.client.RuntimeBroker.Get(ctx, uid)
		if err != nil {
			return mapError(err)
		}
		// Already offline — nothing to do.
		if cur.Status == store.BrokerStatusOffline {
			return nil
		}
		affected, err := s.client.RuntimeBroker.Update().
			Where(runtimebroker.IDEQ(uid), runtimebroker.LockVersionEQ(cur.LockVersion)).
			SetStatus(store.BrokerStatusOffline).
			SetUpdated(now).
			AddLockVersion(1).
			Save(ctx)
		if err != nil {
			return mapError(err)
		}
		if affected == 1 {
			return nil
		}
	}
	return store.ErrVersionConflict
}

// ReapStaleBrokerAffinity clears affinity (connected_hub_id/connected_session_id/
// connected_at) for brokers that still claim affinity but whose last_heartbeat
// is older than staleBefore. Does not change broker status.
func (s *ProjectStore) ReapStaleBrokerAffinity(ctx context.Context, staleBefore time.Time) (int, error) {
	affected, err := s.client.RuntimeBroker.Update().
		Where(
			runtimebroker.ConnectedHubIDNotNil(),
			runtimebroker.LastHeartbeatLT(staleBefore),
		).
		ClearConnectedHubID().
		ClearConnectedSessionID().
		ClearConnectedAt().
		SetUpdated(time.Now()).
		Save(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return affected, nil
}

// =============================================================================
// ProjectProvider (project_contributors) operations
// =============================================================================

// entContributorToStore converts an Ent ProjectContributor to a store model.
func entContributorToStore(c *ent.ProjectContributor) store.ProjectProvider {
	pp := store.ProjectProvider{
		ProjectID:  c.ProjectID.String(),
		BrokerID:   c.BrokerID.String(),
		BrokerName: c.BrokerName,
		LocalPath:  c.LocalPath,
		Status:     c.Status,
		LinkedBy:   c.LinkedBy,
	}
	if c.LastSeen != nil {
		pp.LastSeen = *c.LastSeen
	}
	if c.LinkedAt != nil {
		pp.LinkedAt = *c.LinkedAt
	}
	return pp
}

// AddProjectProvider adds (or replaces) a broker as a provider to a project.
// Mirrors the legacy INSERT OR REPLACE via Ent's OnConflict upsert keyed on the
// (project_id, broker_id) unique index.
func (s *ProjectStore) AddProjectProvider(ctx context.Context, provider *store.ProjectProvider) error {
	projectUID, err := parseUUID(provider.ProjectID)
	if err != nil {
		return err
	}
	brokerUID, err := parseUUID(provider.BrokerID)
	if err != nil {
		return err
	}

	if provider.LinkedAt.IsZero() && provider.LinkedBy != "" {
		provider.LinkedAt = time.Now()
	}

	status := provider.Status
	if status == "" {
		status = store.BrokerStatusOffline
	}

	create := s.client.ProjectContributor.Create().
		SetProjectID(projectUID).
		SetBrokerID(brokerUID).
		SetBrokerName(provider.BrokerName).
		SetLocalPath(provider.LocalPath).
		SetStatus(status)
	if !provider.LastSeen.IsZero() {
		create.SetLastSeen(provider.LastSeen)
	}
	if provider.LinkedBy != "" {
		create.SetLinkedBy(provider.LinkedBy)
	}
	if !provider.LinkedAt.IsZero() {
		create.SetLinkedAt(provider.LinkedAt)
	}

	return create.
		OnConflictColumns(projectcontributor.FieldProjectID, projectcontributor.FieldBrokerID).
		UpdateNewValues().
		Exec(ctx)
}

// RemoveProjectProvider removes a broker from a project's providers.
func (s *ProjectStore) RemoveProjectProvider(ctx context.Context, projectID, brokerID string) error {
	projectUID, err := parseUUID(projectID)
	if err != nil {
		return err
	}
	brokerUID, err := parseUUID(brokerID)
	if err != nil {
		return err
	}
	n, err := s.client.ProjectContributor.Delete().
		Where(
			projectcontributor.ProjectIDEQ(projectUID),
			projectcontributor.BrokerIDEQ(brokerUID),
		).Exec(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// GetProjectProvider returns a specific provider by project and broker ID.
func (s *ProjectStore) GetProjectProvider(ctx context.Context, projectID, brokerID string) (*store.ProjectProvider, error) {
	projectUID, err := parseUUID(projectID)
	if err != nil {
		return nil, err
	}
	brokerUID, err := parseUUID(brokerID)
	if err != nil {
		return nil, err
	}
	c, err := s.client.ProjectContributor.Query().
		Where(
			projectcontributor.ProjectIDEQ(projectUID),
			projectcontributor.BrokerIDEQ(brokerUID),
		).Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	pp := entContributorToStore(c)
	return &pp, nil
}

// GetProjectProviders returns all providers for a project.
func (s *ProjectStore) GetProjectProviders(ctx context.Context, projectID string) ([]store.ProjectProvider, error) {
	projectUID, err := parseUUID(projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.client.ProjectContributor.Query().
		Where(projectcontributor.ProjectIDEQ(projectUID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	providers := make([]store.ProjectProvider, 0, len(rows))
	for _, c := range rows {
		providers = append(providers, entContributorToStore(c))
	}
	return providers, nil
}

// GetBrokerProjects returns all projects a broker provides for.
func (s *ProjectStore) GetBrokerProjects(ctx context.Context, brokerID string) ([]store.ProjectProvider, error) {
	brokerUID, err := parseUUID(brokerID)
	if err != nil {
		return nil, err
	}
	rows, err := s.client.ProjectContributor.Query().
		Where(projectcontributor.BrokerIDEQ(brokerUID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	providers := make([]store.ProjectProvider, 0, len(rows))
	for _, c := range rows {
		providers = append(providers, entContributorToStore(c))
	}
	return providers, nil
}

// UpdateProviderStatus updates a provider's status and last-seen timestamp.
func (s *ProjectStore) UpdateProviderStatus(ctx context.Context, projectID, brokerID, status string) error {
	projectUID, err := parseUUID(projectID)
	if err != nil {
		return err
	}
	brokerUID, err := parseUUID(brokerID)
	if err != nil {
		return err
	}
	n, err := s.client.ProjectContributor.Update().
		Where(
			projectcontributor.ProjectIDEQ(projectUID),
			projectcontributor.BrokerIDEQ(brokerUID),
		).
		SetStatus(status).
		SetLastSeen(time.Now()).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// =============================================================================
// ProjectSyncState (project_sync_state) operations
// =============================================================================

// entSyncStateToStore converts an Ent ProjectSyncState to a store model.
func entSyncStateToStore(s *ent.ProjectSyncState) *store.ProjectSyncState {
	state := &store.ProjectSyncState{
		ProjectID:     s.ProjectID.String(),
		BrokerID:      s.BrokerID,
		LastCommitSHA: s.LastCommitSha,
		FileCount:     s.FileCount,
		TotalBytes:    s.TotalBytes,
	}
	if s.LastSyncTime != nil {
		state.LastSyncTime = s.LastSyncTime
	}
	return state
}

// UpsertProjectSyncState creates or updates sync state for a project (optionally
// per broker). Mirrors the legacy ON CONFLICT(project_id, broker_id) upsert.
func (s *ProjectStore) UpsertProjectSyncState(ctx context.Context, state *store.ProjectSyncState) error {
	projectUID, err := parseUUID(state.ProjectID)
	if err != nil {
		return err
	}

	create := s.client.ProjectSyncState.Create().
		SetProjectID(projectUID).
		SetBrokerID(state.BrokerID).
		SetLastCommitSha(state.LastCommitSHA).
		SetFileCount(state.FileCount).
		SetTotalBytes(state.TotalBytes)
	if state.LastSyncTime != nil {
		create.SetLastSyncTime(*state.LastSyncTime)
	}

	return create.
		OnConflictColumns(projectsyncstate.FieldProjectID, projectsyncstate.FieldBrokerID).
		UpdateNewValues().
		Exec(ctx)
}

// GetProjectSyncState retrieves sync state for a project and optional broker.
func (s *ProjectStore) GetProjectSyncState(ctx context.Context, projectID, brokerID string) (*store.ProjectSyncState, error) {
	projectUID, err := parseUUID(projectID)
	if err != nil {
		return nil, err
	}
	row, err := s.client.ProjectSyncState.Query().
		Where(
			projectsyncstate.ProjectIDEQ(projectUID),
			projectsyncstate.BrokerIDEQ(brokerID),
		).Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entSyncStateToStore(row), nil
}

// ListProjectSyncStates returns all sync states for a project, ordered by broker.
func (s *ProjectStore) ListProjectSyncStates(ctx context.Context, projectID string) ([]store.ProjectSyncState, error) {
	projectUID, err := parseUUID(projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.client.ProjectSyncState.Query().
		Where(projectsyncstate.ProjectIDEQ(projectUID)).
		Order(ent.Asc(projectsyncstate.FieldBrokerID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	states := make([]store.ProjectSyncState, 0, len(rows))
	for _, row := range rows {
		states = append(states, *entSyncStateToStore(row))
	}
	return states, nil
}

// DeleteProjectSyncState removes sync state for a project and optional broker.
func (s *ProjectStore) DeleteProjectSyncState(ctx context.Context, projectID, brokerID string) error {
	projectUID, err := parseUUID(projectID)
	if err != nil {
		return err
	}
	n, err := s.client.ProjectSyncState.Delete().
		Where(
			projectsyncstate.ProjectIDEQ(projectUID),
			projectsyncstate.BrokerIDEQ(brokerID),
		).Exec(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}
