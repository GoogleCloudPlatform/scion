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

//go:build !no_sqlite

// Package hub — shared fixtures for the runtime material selection tests
// (material_*_test.go, handlers_agent_secret_*_test.go).
package hub

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// materialFixture bundles a project, an active owner-role member root user,
// and an agent descending from that root, with a token carrying
// ScopeProjectSecretRead. The delegation edge backfill marker is absent in
// test servers (testServer, handlers_test.go), so the pre-backfill ceiling
// exception admits a hub-attested agent with no recorded edge unless a test
// explicitly calls setBackfillCompleted or creates its own edge.
type materialFixture struct {
	Server    *Server
	Store     store.Store
	ProjectID string
	UserID    string
	AgentID   string
	Token     string
}

// newMaterialFixture builds a materialFixture with a fresh project, an
// active project-owner member user, and an agent whose ancestry is
// [UserID].
func newMaterialFixture(t *testing.T, name string) *materialFixture {
	t.Helper()
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	projectID := tid("project-" + name)
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: name, Slug: "proj-" + name, Created: time.Now(), Updated: time.Now(),
	}))

	userID := tid("user-" + name)
	createDCUser(t, s, userID, name+"@test.com", projectID, store.ProjectRoleOwner)

	agentID := tid("agent-" + name)
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "agent-" + name, Name: "Agent " + name,
		ProjectID: projectID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Ancestry: []string{userID},
		Created:  time.Now(), Updated: time.Now(),
	}))

	token := mintRecordedMaterialToken(t, srv, s, agentID, projectID, []AgentTokenScope{ScopeProjectSecretRead}, []string{userID})

	return &materialFixture{Server: srv, Store: s, ProjectID: projectID, UserID: userID, AgentID: agentID, Token: token}
}

// reissueToken regenerates f.Token with the given scopes/ancestry, for tests
// that need a token shape other than the fixture default.
func (f *materialFixture) reissueToken(t *testing.T, scopes []AgentTokenScope, ancestry []string) {
	t.Helper()
	f.Token = mintRecordedMaterialToken(t, f.Server, f.Store, f.AgentID, f.ProjectID, scopes, ancestry)
}

// mintRecordedMaterialToken signs an agent token and records its credential
// row, the way a production mint does, so revocation applies to it.
func mintRecordedMaterialToken(t *testing.T, srv *Server, s store.AgentCredentialStore, agentID, projectID string, scopes []AgentTokenScope, ancestry []string) string {
	t.Helper()
	token, err := srv.agentTokenService.GenerateAgentToken(agentID, projectID, scopes, ancestry)
	require.NoError(t, err)
	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)
	insertTestAgentCredential(t, s, agentID, projectID, claims.ID)
	return token
}

// getAgent reloads the fixture's agent record from the store.
func (f *materialFixture) getAgent(t *testing.T) *store.Agent {
	t.Helper()
	a, err := f.Store.GetAgent(context.Background(), f.AgentID)
	require.NoError(t, err)
	return a
}

// setBackfillCompleted marks the delegation edge backfill migration as
// complete, so the delegation ceiling stops applying its pre-backfill
// no-edge exception (authz_delegation_ceiling.go).
func setBackfillCompleted(t *testing.T, s store.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.GetHubSetting(ctx, "migration_delegation_edge_backfill_v1"); err == nil {
		return
	}
	_, err := s.UpsertHubSetting(ctx, "migration_delegation_edge_backfill_v1",
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	require.NoError(t, err, "failed to set backfill marker")
}

// newFullAgentIdentity builds an AgentIdentity for direct (non-HTTP) unit
// tests of materialRuntimePrecheck and friends.
func newFullAgentIdentity(agentID, projectID string, ancestry []string, scopes []AgentTokenScope) AgentIdentity {
	claims := &AgentTokenClaims{ProjectID: projectID, Scopes: scopes, Ancestry: ancestry}
	claims.Subject = agentID
	return &agentIdentityWrapper{claims}
}

// materialFailingStore wraps a store.Store and injects errors, or canned
// results, for specific method calls. Used to exercise runtime material
// reads' fail-closed paths on genuine store faults (as opposed to
// store.ErrNotFound), and to
// exercise the delegation ceiling's duplicate-active-edge invariant-
// violation branch, which the partial unique index on
// (delegate_type, delegate_id, scope_type, scope_id) WHERE active=true makes
// unreachable through normal store writes (see
// TestDelegationCeiling_DuplicateEdgesFailClosed, delegation_ceiling_test.go).
type materialFailingStore struct {
	store.Store

	getUserErr error
	// getUserErrAfterCalls, if > 0, makes getUserErr apply starting with that
	// call number (e.g. 1 lets the first GetUser call through and fails the
	// second onward). 0 (the default) fails every call, as before.
	getUserErrAfterCalls int
	getUserCalls         int
	// listRoleBindingsForPrincipalErr injects a failure into the singular
	// ListRoleBindingsForPrincipal, which CheckEffectiveMembership calls for
	// a principal's direct bindings.
	listRoleBindingsForPrincipalErr error
	// listRoleBindingsForPrincipalsErr injects a failure into the plural
	// ListRoleBindingsForPrincipals, which CheckEffectiveMembership calls for
	// group-derived bindings and SystemAuthorityProof calls for the
	// principal's system-scope bindings.
	listRoleBindingsForPrincipalsErr error
	getDelegationEdgesForDelegateErr error
	// getDelegationEdgesForDelegateErrAfterCalls, if > 0, makes
	// getDelegationEdgesForDelegateErr apply starting with the call after
	// that number (reads 1..N pass, later reads fail), as for
	// getUserErrAfterCalls. 0 fails every call.
	getDelegationEdgesForDelegateErrAfterCalls int
	getDelegationEdgesForDelegateCalls         int
	// getDelegationEdgesForDelegateResults records the error each
	// GetDelegationEdgesForDelegate call returned, in call order.
	getDelegationEdgesForDelegateResults []error
	// getDelegationEdgesForDelegateHook, if set, is called before each
	// GetDelegationEdgesForDelegate call with its 1-based call number.
	getDelegationEdgesForDelegateHook func(call int)
	delegationEdgesOverride           []*store.DelegationEdge
	listProgenySecretsErr             error
	// createAgentCalls counts CreateAgent calls, including those made on
	// the transaction store inside WithTx.
	createAgentCalls int
}

func (f *materialFailingStore) CreateAgent(ctx context.Context, agent *store.Agent) error {
	f.createAgentCalls++
	return f.Store.CreateAgent(ctx, agent)
}

// WithTx runs fn on the underlying transaction store, wrapped so CreateAgent
// calls inside the transaction are counted.
func (f *materialFailingStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return f.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&createAgentCountingStore{Store: tx, calls: &f.createAgentCalls})
	})
}

// createAgentCountingStore counts CreateAgent calls into a shared counter.
type createAgentCountingStore struct {
	store.Store
	calls *int
}

func (c *createAgentCountingStore) CreateAgent(ctx context.Context, agent *store.Agent) error {
	*c.calls++
	return c.Store.CreateAgent(ctx, agent)
}

func (f *materialFailingStore) ListProgenySecrets(ctx context.Context, ancestorIDs []string) ([]store.Secret, error) {
	if f.listProgenySecretsErr != nil {
		return nil, f.listProgenySecretsErr
	}
	return f.Store.ListProgenySecrets(ctx, ancestorIDs)
}

func (f *materialFailingStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	f.getUserCalls++
	if f.getUserErr != nil && (f.getUserErrAfterCalls == 0 || f.getUserCalls > f.getUserErrAfterCalls) {
		return nil, f.getUserErr
	}
	return f.Store.GetUser(ctx, id)
}

func (f *materialFailingStore) ListRoleBindingsForPrincipal(ctx context.Context, principalType, principalID string) ([]*store.RoleBinding, error) {
	if f.listRoleBindingsForPrincipalErr != nil {
		return nil, f.listRoleBindingsForPrincipalErr
	}
	return f.Store.ListRoleBindingsForPrincipal(ctx, principalType, principalID)
}

func (f *materialFailingStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes, scopeIDs []string) ([]*store.RoleBinding, error) {
	if f.listRoleBindingsForPrincipalsErr != nil {
		return nil, f.listRoleBindingsForPrincipalsErr
	}
	return f.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

func (f *materialFailingStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	f.getDelegationEdgesForDelegateCalls++
	if f.getDelegationEdgesForDelegateHook != nil {
		f.getDelegationEdgesForDelegateHook(f.getDelegationEdgesForDelegateCalls)
	}
	edges, err := f.getDelegationEdgesForDelegate(ctx, delegateType, delegateID)
	f.getDelegationEdgesForDelegateResults = append(f.getDelegationEdgesForDelegateResults, err)
	return edges, err
}

func (f *materialFailingStore) getDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	if f.getDelegationEdgesForDelegateErr != nil &&
		(f.getDelegationEdgesForDelegateErrAfterCalls == 0 || f.getDelegationEdgesForDelegateCalls > f.getDelegationEdgesForDelegateErrAfterCalls) {
		return nil, f.getDelegationEdgesForDelegateErr
	}
	if f.delegationEdgesOverride != nil {
		return f.delegationEdgesOverride, nil
	}
	return f.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
}

// callCountingStore wraps a store.Store and counts calls to
// GetDelegationEdgesForDelegate, for the "evaluated once per request"
// regression.
type callCountingStore struct {
	store.Store
	getDelegationEdgesForDelegateCalls int
}

func (c *callCountingStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	c.getDelegationEdgesForDelegateCalls++
	return c.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
}

// countingSecretBackend wraps a secret.SecretBackend and counts GetMeta and
// Get calls, for the "denied request reads no metadata" regression and the
// "no value access before authorization" runtime-subset regression.
type countingSecretBackend struct {
	secret.SecretBackend
	getMetaCalls int
	getCalls     int
}

func (c *countingSecretBackend) GetMeta(ctx context.Context, name, scope, scopeID string) (*secret.SecretMeta, error) {
	c.getMetaCalls++
	return c.SecretBackend.GetMeta(ctx, name, scope, scopeID)
}

func (c *countingSecretBackend) Get(ctx context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
	c.getCalls++
	return c.SecretBackend.Get(ctx, name, scope, scopeID)
}

// raceSecretBackend wraps a secret.SecretBackend and lets a test rewrite the
// Get() result after the real backend has answered, simulating check 9's
// record-race window between GetMeta and Get.
type raceSecretBackend struct {
	secret.SecretBackend
	overrideGet func(sv *secret.SecretWithValue, err error) (*secret.SecretWithValue, error)
}

func (r *raceSecretBackend) Get(ctx context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
	sv, err := r.SecretBackend.Get(ctx, name, scope, scopeID)
	if r.overrideGet != nil {
		return r.overrideGet(sv, err)
	}
	return sv, err
}

// erroringMetaBackend wraps a secret.SecretBackend and returns a fixed error
// from GetMeta, for the "backend error text stays neutral" regression.
type erroringMetaBackend struct {
	secret.SecretBackend
	err error
}

func (e *erroringMetaBackend) GetMeta(ctx context.Context, name, scope, scopeID string) (*secret.SecretMeta, error) {
	if e.err != nil {
		return nil, e.err
	}
	return e.SecretBackend.GetMeta(ctx, name, scope, scopeID)
}

// erroringListBackend wraps a secret.SecretBackend and returns a fixed error
// from List for one scope only, so the agent secret list's per-scope error
// exits can be exercised without disturbing the other scope.
type erroringListBackend struct {
	secret.SecretBackend
	scope string
	err   error
}

func (e *erroringListBackend) List(ctx context.Context, filter secret.Filter) ([]secret.SecretMeta, error) {
	if e.err != nil && filter.Scope == e.scope {
		return nil, e.err
	}
	return e.SecretBackend.List(ctx, filter)
}

// recordingMaterialAuditor embeds a real LogAuditLogger (so it satisfies the
// full AuditLogger interface with production behaviour) and additionally
// records every MaterialSelectionEvent it receives, for tests that need to
// inspect audit-only fields (Reason, Detail, SharingSource, Grant) that
// never reach the HTTP response.
type recordingMaterialAuditor struct {
	*LogAuditLogger
	events           []*MaterialSelectionEvent
	secretReadEvents []*AgentSecretReadEvent
}

func newRecordingMaterialAuditor() *recordingMaterialAuditor {
	return &recordingMaterialAuditor{LogAuditLogger: NewLogAuditLogger("[test]", false)}
}

func (r *recordingMaterialAuditor) LogMaterialSelectionEvent(ctx context.Context, e *MaterialSelectionEvent) error {
	r.events = append(r.events, e)
	return r.LogAuditLogger.LogMaterialSelectionEvent(ctx, e)
}

func (r *recordingMaterialAuditor) LogAgentSecretReadEvent(ctx context.Context, e *AgentSecretReadEvent) error {
	r.secretReadEvents = append(r.secretReadEvents, e)
	return r.LogAuditLogger.LogAgentSecretReadEvent(ctx, e)
}

// plainAuditLogger implements only the base AuditLogger interface — not
// materialSelectionAuditor — so logMaterialSelection must fall back to slog
// without erroring (TestMaterialAudit_EmittedWithoutAuditLoggerInterfaceChange).
type plainAuditLogger struct{}

func (plainAuditLogger) LogBrokerAuthEvent(context.Context, *BrokerAuthEvent) error { return nil }
func (plainAuditLogger) LogGCPTokenEvent(context.Context, *GCPTokenEvent) error     { return nil }
func (plainAuditLogger) LogGCSLinkFetchEvent(context.Context, *GCSLinkFetchEvent) error {
	return nil
}
func (plainAuditLogger) LogInviteAuditEvent(context.Context, *InviteAuditEvent) error { return nil }
func (plainAuditLogger) LogLifecycleHookEvent(context.Context, *LifecycleHookEvent) error {
	return nil
}
func (plainAuditLogger) LogLifecycleHookExecutionEvent(context.Context, *LifecycleHookExecutionEvent) error {
	return nil
}
func (plainAuditLogger) LogAgentSecretReadEvent(context.Context, *AgentSecretReadEvent) error {
	return nil
}
func (plainAuditLogger) RecordSAAssignment(context.Context, *store.SAAssignmentEvent) error {
	return nil
}
