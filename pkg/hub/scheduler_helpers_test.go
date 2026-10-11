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

package hub

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// newEventHandlerTestServer builds a Server for the scheduler's event-handler
// tests. It exists so those tests do not construct `&Server{store: ms}` by
// literal and leave agentLifecycleLog nil.
//
// A nil *slog.Logger is not inert: calling Warn on one dereferences it and
// segfaults. Production is safe because NewServer always populates the field
// (server.go:749), so the hazard is confined to struct-literal Servers in
// tests — but it is the same shape of trap as the mock store's unimplemented
// methods, and it detonates the same way, taking the whole test binary down
// with a SIGSEGV rather than failing one test.
func newEventHandlerTestServer(st store.Store) *Server {
	return &Server{
		store:             st,
		agentLifecycleLog: slog.Default(),
		authzService:      NewAuthzService(st, slog.Default()),
	}
}

// mockScheduledEventStore is a minimal in-memory store for testing one-shot
// timer scheduling. It only implements the ScheduledEventStore methods needed
// by the Scheduler; all other Store interface methods panic if called.
type mockScheduledEventStore struct {
	store.Store     // embed to satisfy the interface; unused methods panic
	mu              sync.Mutex
	events          map[string]*store.ScheduledEvent
	agents          map[string]*store.Agent
	projects        map[string]*store.Project
	users           map[string]*store.User
	roleBindings    []*store.RoleBinding
	roleDefinitions map[string]*store.RoleDefinition
	audits          []*store.MutationAuditRecord
	schedules       map[string]*store.Schedule
	notifications   []*store.Notification
	getUserErr      error  // when set, GetUser fails with it
	getUserErrID    string // when set, getUserErr applies to this ID only
}

func newMockStore() *mockScheduledEventStore {
	return &mockScheduledEventStore{
		events:          make(map[string]*store.ScheduledEvent),
		agents:          make(map[string]*store.Agent),
		projects:        make(map[string]*store.Project),
		users:           make(map[string]*store.User),
		roleDefinitions: make(map[string]*store.RoleDefinition),
	}
}

func seedFullRoleDispatchCreator(ms *mockScheduledEventStore, projectID string) string {
	creatorID := "creator-agent"
	// The creator is owned by an active project member, so it is in good
	// standing (ptone/scion#3433).
	ownerID := "creator-owner"
	ms.users[ownerID] = &store.User{ID: ownerID, Email: "creator-owner@test.example", Role: store.UserRoleMember, Status: store.UserStatusActive}
	ms.roleDefinitions["rd-creator-member"] = &store.RoleDefinition{
		ID: "rd-creator-member", Name: store.ProjectRoleMember, ScopeType: store.RoleScopeProject,
		Permissions: []string{"agent.read"},
	}
	ms.roleBindings = append(ms.roleBindings, &store.RoleBinding{
		ID: "rb-creator-member", RoleDefinitionID: "rd-creator-member",
		PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: ownerID,
		ScopeType: store.RoleScopeProject, ScopeID: projectID,
	})
	ms.agents[creatorID] = &store.Agent{
		ID:            creatorID,
		ProjectID:     projectID,
		OwnerID:       ownerID,
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
	}
	return creatorID
}

// resolvingTemplateStore is mockScheduledEventStore with the one change that
// used to detonate it: a template getter that actually returns a template.
// Everything else is inherited.
type resolvingTemplateStore struct {
	*mockScheduledEventStore
}

func (m *mockScheduledEventStore) GetSchedule(_ context.Context, id string) (*store.Schedule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sc, ok := m.schedules[id]; ok {
		cp := *sc
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (m *mockScheduledEventStore) CreateNotification(_ context.Context, n *store.Notification) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *n
	m.notifications = append(m.notifications, &cp)
	return nil
}

func (m *mockScheduledEventStore) getNotifications() []*store.Notification {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*store.Notification(nil), m.notifications...)
}

// HasActiveAgentHold reports no holds: the mock records none.
func (m *mockScheduledEventStore) HasActiveAgentHold(_ context.Context, _ string) (bool, error) {
	return false, nil
}

func (m *mockScheduledEventStore) CreateScheduledEvent(_ context.Context, event *store.ScheduledEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.events[event.ID]; exists {
		return store.ErrAlreadyExists
	}
	if event.Status == "" {
		event.Status = store.ScheduledEventPending
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	cp := *event
	m.events[event.ID] = &cp
	return nil
}

func (m *mockScheduledEventStore) GetScheduledEvent(_ context.Context, id string) (*store.ScheduledEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	evt, ok := m.events[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *evt
	return &cp, nil
}

func (m *mockScheduledEventStore) ListPendingScheduledEvents(_ context.Context) ([]store.ScheduledEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []store.ScheduledEvent
	for _, evt := range m.events {
		if evt.Status == store.ScheduledEventPending {
			result = append(result, *evt)
		}
	}
	return result, nil
}

func (m *mockScheduledEventStore) UpdateScheduledEventStatus(_ context.Context, id string, status string, firedAt *time.Time, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	evt, ok := m.events[id]
	if !ok {
		return store.ErrNotFound
	}
	evt.Status = status
	evt.FiredAt = firedAt
	evt.Error = errMsg
	return nil
}

func (m *mockScheduledEventStore) CancelScheduledEvent(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	evt, ok := m.events[id]
	if !ok {
		return store.ErrNotFound
	}
	if evt.Status != store.ScheduledEventPending {
		return store.ErrNotFound
	}
	evt.Status = store.ScheduledEventCancelled
	return nil
}

func (m *mockScheduledEventStore) ListScheduledEvents(_ context.Context, _ store.ScheduledEventFilter, _ store.ListOptions) (*store.ListResult[store.ScheduledEvent], error) {
	return &store.ListResult[store.ScheduledEvent]{}, nil
}

func (m *mockScheduledEventStore) PurgeOldScheduledEvents(_ context.Context, _ time.Time) (int, error) {
	return 0, nil
}

func (m *mockScheduledEventStore) GetActiveProjectPreStartHook(_ context.Context, _ string) (*store.ProjectPreStartHook, error) {
	return nil, store.ErrNotFound
}

func (m *mockScheduledEventStore) GetActiveHubPreStartHook(_ context.Context) (*store.ProjectPreStartHook, error) {
	return nil, store.ErrNotFound
}

func (m *mockScheduledEventStore) GetAgent(_ context.Context, id string) (*store.Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.agents[id]; ok {
		cp := *a
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (m *mockScheduledEventStore) GetUser(_ context.Context, id string) (*store.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getUserErr != nil && (m.getUserErrID == "" || m.getUserErrID == id) {
		return nil, m.getUserErr
	}
	if u, ok := m.users[id]; ok {
		cp := *u
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (m *mockScheduledEventStore) GetAgentBySlug(_ context.Context, projectID, slug string) (*store.Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.agents {
		if a.ProjectID == projectID && a.Slug == slug {
			cp := *a
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *mockScheduledEventStore) GetProject(_ context.Context, id string) (*store.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if g, ok := m.projects[id]; ok {
		cp := *g
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (m *mockScheduledEventStore) GetProjectProviders(_ context.Context, _ string) ([]store.ProjectProvider, error) {
	return nil, nil
}

func (m *mockScheduledEventStore) CreateAgent(_ context.Context, agent *store.Agent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.agents[agent.ID] = agent
	return nil
}

// WithTx runs fn directly against m: this mock has no real transactions, and
// none of the tests that use it exercise rollback behavior. Needed because
// the scheduler's create path now writes the agent row and its identity-key
// row inside WithTx (commitAgentCreate); without this override that
// call panics on the embedded nil store.Store, same as any other unhandled
// method here.
func (m *mockScheduledEventStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return fn(m)
}

// ReplaceAgentIdentityKeys is a no-op: this mock has no identity-key
// storage, and the tests that use it exercise dispatch mechanics (template
// resolution, applied-config precedence), not the identity-key invariant
// itself -- that invariant is covered separately against the real ent-backed
// store (scheduler_identity_key_test.go).
func (m *mockScheduledEventStore) ReplaceAgentIdentityKeys(_ context.Context, _, _ string, _ []string) error {
	return nil
}

func (m *mockScheduledEventStore) CreateDelegationEdge(_ context.Context, _ *store.DelegationEdge) error {
	return nil // no-op for mock
}

// CreateMutationAudit records the audit row a scheduled create writes in
// its transaction.
func (m *mockScheduledEventStore) CreateMutationAudit(_ context.Context, r *store.MutationAuditRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audits = append(m.audits, r)
	return nil
}

func (m *mockScheduledEventStore) GetTemplate(_ context.Context, _ string) (*store.Template, error) {
	return nil, store.ErrNotFound
}

func (m *mockScheduledEventStore) GetTemplateBySlug(_ context.Context, _, _, _ string) (*store.Template, error) {
	return nil, store.ErrNotFound
}

func (m *mockScheduledEventStore) GetHarnessConfigBySlug(_ context.Context, _, _, _ string) (*store.HarnessConfig, error) {
	return nil, store.ErrNotFound
}

func (m *mockScheduledEventStore) GetEnvVar(_ context.Context, _, _, _ string) (*store.EnvVar, error) {
	return nil, store.ErrNotFound
}

func (m *mockScheduledEventStore) GetSecret(_ context.Context, _, _, _ string) (*store.Secret, error) {
	return nil, store.ErrNotFound
}

func (m *mockScheduledEventStore) ListGCPServiceAccounts(_ context.Context, _ store.GCPServiceAccountFilter) ([]store.GCPServiceAccount, error) {
	return nil, nil
}

func (m *mockScheduledEventStore) GetHubSetting(_ context.Context, _ string) (*store.HubSetting, error) {
	return nil, store.ErrNotFound
}

func (m *mockScheduledEventStore) ListSkillInjections(_ context.Context, _, _ string) ([]store.SkillInjection, error) {
	return nil, nil
}

func (m *mockScheduledEventStore) GetEffectiveGroups(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (m *mockScheduledEventStore) GetEffectiveGroupsForAgent(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

// GetDelegationEdgesForDelegate returns no edges; the mock store has no
// backfill marker, so an agent without an edge is evaluated pre-backfill.
func (m *mockScheduledEventStore) GetDelegationEdgesForDelegate(_ context.Context, _, _ string) ([]*store.DelegationEdge, error) {
	return nil, nil
}

func (m *mockScheduledEventStore) ListAccessConstraints(_ context.Context, _, _ int) ([]*store.AccessConstraint, error) {
	return nil, nil
}

func (m *mockScheduledEventStore) ListRoleBindingsForPrincipals(_ context.Context, principals []store.PrincipalRef, _ []string, _ []string) ([]*store.RoleBinding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*store.RoleBinding
	for _, b := range m.roleBindings {
		for _, p := range principals {
			if b.PrincipalType == p.Type && b.PrincipalID == p.ID {
				result = append(result, b)
			}
		}
	}
	return result, nil
}

func (m *mockScheduledEventStore) GetRoleDefinitionsByIDs(_ context.Context, ids []string) (map[string]*store.RoleDefinition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]*store.RoleDefinition)
	for _, id := range ids {
		if rd, ok := m.roleDefinitions[id]; ok {
			result[id] = rd
		}
	}
	return result, nil
}

// getEvent returns a snapshot of an event by ID (test helper, no error).
// It returns a copy so callers can read fields without holding the lock.
func (m *mockScheduledEventStore) getEvent(id string) *store.ScheduledEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	evt, ok := m.events[id]
	if !ok {
		return nil
	}
	cp := *evt
	return &cp
}

func (r *resolvingTemplateStore) GetTemplateBySlug(_ context.Context, slug, _, _ string) (*store.Template, error) {
	return &store.Template{
		ID:      "tmpl-resolvable",
		Slug:    slug,
		Name:    slug,
		Harness: "claude",
		// A declared harness config, not just a harness type: since
		// ptone/scion#601 item 2 only DefaultHarnessConfig feeds the
		// harness-config slot, and the panic trap below needs the slot
		// filled so populateAgentConfig reaches GetHarnessConfigBySlug.
		// Distinct from Harness so assertions prove which field supplied it.
		DefaultHarnessConfig: "claude-declared",
		ContentHash:          "d00dfeed",
		Status:               "active",
		// Scope: global — these tests exercise the scheduler dispatch
		// mechanics (which rung wins, applied-config precedence), not store
		// scope filtering, so this stub always resolves regardless of the
		// scope/scopeID arguments it's called with. It stands in for the
		// hub-wide default template these tests reference (e.g.
		// DefaultTemplate), so it must be scoped as such: ptone/scion#1916's
		// authorizeResolvedTemplate gate (handlers_agent_create_helpers.go)
		// treats a resolved candidate's scope as authoritative, and a
		// delegate agent's scheduled dispatch — like any principal without a
		// hub-member-equivalent grant of its own — can only pass that gate on
		// a global-scope template.
		Scope: store.TemplateScopeGlobal,
	}, nil
}
