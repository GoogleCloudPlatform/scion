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

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

// validKeysBody is a minimal, valid /keys request body -- everywhere a test
// below needs to get past ValidateBody so it can exercise the
// resolution/authorization/admission behavior beyond it.
var validKeysBody = map[string]string{"keys": "C-c"}

// agentKeysRouteFixture builds a server plus two real projects/agents so the
// tests below can exercise both /keys route shapes end-to-end.
type agentKeysRouteFixture struct {
	srv      *Server
	store    store.Store
	projectA *store.Project
	projectB *store.Project
	agentInA *store.Agent // target agent in project A
	agentInB *store.Agent // target agent in project B
	owner    *store.User  // owns agentInA and agentInB
	nonOwner *store.User  // a hub user with no ownership/role on either agent
}

func newAgentKeysRouteFixture(t *testing.T) *agentKeysRouteFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID: tid("agentkeys-route-owner"), Email: "agentkeys-route-owner@test.com",
		DisplayName: "Owner", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, owner))

	nonOwner := &store.User{
		ID: tid("agentkeys-route-nonowner"), Email: "agentkeys-route-nonowner@test.com",
		DisplayName: "Non-Owner", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, nonOwner))

	projA := &store.Project{ID: tid("agentkeys-route-proj-a"), Name: "Route A", Slug: "agentkeys-route-proj-a", OwnerID: owner.ID}
	require.NoError(t, s.CreateProject(ctx, projA))
	projB := &store.Project{ID: tid("agentkeys-route-proj-b"), Name: "Route B", Slug: "agentkeys-route-proj-b", OwnerID: owner.ID}
	require.NoError(t, s.CreateProject(ctx, projB))
	// The owner relationship on a project agent requires active project
	// access (ptone/scion#2141); the binding grants no permissions itself.
	grantProjectAccessOnly(t, s, owner.ID, projA.ID)
	grantProjectAccessOnly(t, s, owner.ID, projB.ID)

	// A real store.RuntimeBroker row, assigned to both fixture agents below,
	// so RuntimeBrokerID resolves to something real -- required for the
	// real-dispatcher integration tests (TestExecuteAgentKeys_RealHTTPDispatcher*
	// in execute_agent_keys_test.go), which route through
	// HTTPAgentDispatcher.DispatchAgentKeys and its own
	// getBrokerEndpoint(ctx, target.RuntimeBrokerID) store lookup. Tests that
	// use the fake dispatcher (fakeAgentKeysDispatcher) never look at this
	// broker row at all, so its presence does not change their behavior; an
	// authorized call with no dispatcher configured at all (the default
	// unless a test opts in via SetDispatcher) still ends in 503
	// keys_unavailable, since that determination is
	// s.GetDispatcher() == nil, not anything about the target agent.
	broker := &store.RuntimeBroker{
		ID: tid("agentkeys-route-broker"), Name: "agentkeys-route-broker", Slug: "agentkeys-route-broker",
		Endpoint: "http://broker.invalid:9800", Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agentA := &store.Agent{
		ID: tid("agentkeys-route-agent-a"), Slug: "agentkeys-route-agent-a", Name: "Agent A",
		ProjectID: projA.ID, Phase: string(state.PhaseRunning), OwnerID: owner.ID, RuntimeBrokerID: broker.ID,
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))
	agentB := &store.Agent{
		ID: tid("agentkeys-route-agent-b"), Slug: "agentkeys-route-agent-b", Name: "Agent B",
		ProjectID: projB.ID, Phase: string(state.PhaseRunning), OwnerID: owner.ID, RuntimeBrokerID: broker.ID,
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	return &agentKeysRouteFixture{
		srv: srv, store: s, projectA: projA, projectB: projB,
		agentInA: agentA, agentInB: agentB, owner: owner, nonOwner: nonOwner,
	}
}

func decodeKeysError(t *testing.T, body []byte) keysErrorEnvelope {
	t.Helper()
	var env struct {
		Error keysErrorEnvelope `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &env), "response body: %s", string(body))
	return env.Error
}

// agentKeysLookupSpyStore wraps a store.Store and counts calls to the two
// agent-resolution methods, so a test can prove no agent lookup ran before
// a denial (contract §3.1 invariant 4 / AK-21c). When failLookups is set,
// both methods return a generic (non-ErrNotFound) error instead of
// delegating, simulating a store outage (round-2 review finding 4).
type agentKeysLookupSpyStore struct {
	store.Store
	getAgentCalls       int32
	getAgentBySlugCalls int32
	failLookups         bool
}

// assertKeysDenialOutcome asserts rec matches (wantStatus, wantCode), that
// the message is the exact fixed string for that outcome (never
// KeysAuthzDecision.Reason or any other request-derived text), and that a
// real, non-empty operation ID is present (contract §3 invariant 3: every
// outcome from validation onward carries one).
func assertKeysDenialOutcome(t *testing.T, label string, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("%s: status = %d, want %d: %s", label, rec.Code, wantStatus, rec.Body.String())
	}
	env := decodeKeysError(t, rec.Body.Bytes())
	if env.Code != wantCode {
		t.Errorf("%s: code = %q, want %q", label, env.Code, wantCode)
	}
	if wantMessage, ok := keysDenialFixedMessage[wantCode]; ok && env.Message != wantMessage {
		t.Errorf("%s: message = %q, want exactly %q", label, env.Message, wantMessage)
	}
	opID, _ := env.Details["operation_id"].(string)
	if opID == "" {
		t.Errorf("%s: expected a non-empty operation_id in details, got %v", label, env.Details)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("keys: ")) {
		t.Errorf("%s: body must not leak the internal audit reason prefix: %s", label, rec.Body.String())
	}
}

// assertKeysDenialOutcomeAuditMatches asserts that the last "outcome"-event
// "agent keys audit" record captured in log carries the exact same
// operation_id as rec's response -- pinning "exactly one real operation ID"
// (contract §3's phase-boundary clarification / the binding operation-ID
// ruling) for outcomes where assertKeysDenialOutcome alone only checks
// non-emptiness, not equality with what was actually minted and audited. A
// second, freshly minted ID written into the response after the audit
// record used the real one would pass assertKeysDenialOutcome but fail this
// check. The caller must have installed log capture (installSentinelLogCapture)
// before making the request that produced rec.
func assertKeysDenialOutcomeAuditMatches(t *testing.T, label string, rec *httptest.ResponseRecorder, log *bytes.Buffer) {
	t.Helper()
	env := decodeKeysError(t, rec.Body.Bytes())
	opID, _ := env.Details["operation_id"].(string)
	outcome := lastOutcomeAuditRecord(t, log)
	if outcome["operation_id"] != opID {
		t.Errorf("%s: audit operation_id %q != response operation_id %q", label, outcome["operation_id"], opID)
	}
}

// enableCrossProjectMessaging enables the Hub-level cross_project_messaging_enabled
// flag by updating operational settings on the server.
func enableCrossProjectMessaging(t *testing.T, srv *Server) {
	t.Helper()
	ctx := context.Background()

	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("messaging", json.RawMessage(`{"cross_project_messaging_enabled": true}`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(ctx); err != nil {
		t.Fatalf("failed to refresh operational settings: %v", err)
	}
	srv.SetOperationalSettings(ops)
}

// crossProjectSetup creates two projects (A and B) with owners, members,
// and agents for cross-project testing. Returns all the fixtures.
type crossProjectFixture struct {
	srv      *Server
	store    store.Store
	ownerA   *store.User
	ownerB   *store.User
	memberA  *store.User
	projectA string
	projectB string
}

func crossProjectSetup(t *testing.T) crossProjectFixture {
	t.Helper()
	srv, s, ownerA, _, projectA := msgAuthzSetup(t)
	ctx := context.Background()

	// Create a second project.
	projectB := tid("msg-project-b")
	ownerB := &store.User{
		ID:          tid("msg-owner-b"),
		Email:       "owner-b@test.com",
		DisplayName: "Owner B",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require_NoError(t, s.CreateUser(ctx, ownerB))
	ensureHubMembership(ctx, s, ownerB.ID)

	projB := &store.Project{
		ID:        projectB,
		Name:      "project-b",
		Slug:      "project-b",
		OwnerID:   ownerB.ID,
		CreatedBy: ownerB.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require_NoError(t, s.CreateProject(ctx, projB))
	srv.seedProjectCreatorMembership(ctx, projB)

	// Add ownerB as member of project B.
	msgAuthzAddProjectMember(t, s, ownerB.ID, projectB, "project-b", store.GroupMemberRoleOwner)

	return crossProjectFixture{
		srv:      srv,
		store:    s,
		ownerA:   ownerA,
		ownerB:   ownerB,
		memberA:  nil, // use ownerA as origin user
		projectA: projectA,
		projectB: projectB,
	}
}

func newTestAgentIdentity(id, projectID string, scopes []AgentTokenScope) *grantGuardAgentIdentity {
	return &grantGuardAgentIdentity{id: id, projectID: projectID, scopes: scopes}
}

// msgAuthzSetup creates a server with a project, project owner, and project
// member, seeding the necessary memberships and policies for authorization
// tests. Returns (server, store, ownerUser, memberUser, projectID).
func msgAuthzSetup(t *testing.T) (*Server, store.Store, *store.User, *store.User, string) {
	t.Helper()
	srv, s := testServer(t)
	owner, member, projectID := msgAuthzSetupOn(t, srv, s)
	return srv, s, owner, member, projectID
}

// msgAuthzSetupWithFault is msgAuthzSetup with a switch-gated store wrapper
// (see installStoreFault) installed before the fixture's audited setup
// (seedProjectCreatorMembership emits a mutation audit whose goroutine
// reads srv.store). Tests call fault.Arm() where they used to assign
// srv.store, which would race that goroutine (ptone/scion#3184).
func msgAuthzSetupWithFault[W store.Store](t *testing.T, wrap func(inner store.Store, fault *storeFaultSwitch) W) (*Server, store.Store, *store.User, *store.User, string, W, *storeFaultSwitch) {
	t.Helper()
	srv, s, wrapped, fault := testServerWithStoreFault(t, wrap)
	owner, member, projectID := msgAuthzSetupOn(t, srv, s)
	return srv, s, owner, member, projectID, wrapped, fault
}

// msgAuthzAddProjectMember adds a user to the project's members group and creates the
// appropriate role binding.
func msgAuthzAddProjectMember(t *testing.T, s store.Store, userID, projectID, projectSlug, groupRole string) {
	t.Helper()
	ctx := context.Background()

	// Add to project members group (needed for policy evaluation)
	membersSlug := projectMembersGroupSlug(projectSlug)
	group, err := s.GetGroupBySlug(ctx, membersSlug)
	if err != nil {
		t.Fatalf("failed to get project members group %q: %v", membersSlug, err)
	}
	if err := s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    group.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   userID,
		Role:       groupRole,
	}); err != nil && err != store.ErrAlreadyExists {
		t.Fatalf("failed to add user to project members group: %v", err)
	}

	// Create role binding
	roleName := store.ProjectRoleMember
	if groupRole == store.GroupMemberRoleOwner {
		roleName = store.ProjectRoleOwner
	}
	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
	if err != nil {
		t.Fatalf("role definition %q not found: %v", roleName, err)
	}
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	if err != nil && err != store.ErrAlreadyExists &&
		!errors.Is(err, store.ErrBuiltInMembershipConflict) {
		t.Fatalf("failed to create role binding: %v", err)
	}
}

// msgAuthzGrantAgentMessage grants explicit agent.message permission to a user
// for a project. This creates a one-off role definition with the agent.message
// permission and binds it, so the test does not depend on agent.message being
// present in the project-member built-in role (which upstream may remove).
func msgAuthzGrantAgentMessage(t *testing.T, s store.Store, userID, projectID string) {
	t.Helper()
	ctx := context.Background()

	const roleName = "test-agent-message-sender"

	// Reuse an existing definition if one was already seeded by a prior test.
	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
	if err != nil {
		rd = &store.RoleDefinition{
			Name:        roleName,
			Description: "Test role granting explicit agent.message permission",
			ScopeType:   store.RoleScopeProject,
			Permissions: []string{"agent.message"},
			System:      false,
		}
		created, createErr := s.CreateRoleDefinition(ctx, rd)
		if createErr != nil {
			t.Fatalf("failed to create agent-message role definition: %v", createErr)
		}
		rd = created
	}

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	if err != nil && err != store.ErrAlreadyExists {
		t.Fatalf("failed to create agent.message role binding: %v", err)
	}
}

// require_NoError is a test helper that fails immediately on error.
func require_NoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// msgAuthzAgent creates a store.Agent with the specified properties and
// persists it. Returns the agent.
func msgAuthzAgent(t *testing.T, s store.Store, id, projectID, mode string, ancestry []string) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID:          tid(id),
		Name:        id,
		Slug:        id,
		ProjectID:   projectID,
		MessageMode: mode,
		Ancestry:    ancestry,
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require_NoError(t, s.CreateAgent(context.Background(), agent))
	return agent
}

// msgAuthzAgentIdentity builds an AgentIdentity for the given agent.
func msgAuthzAgentIdentity(agentID, projectID string, ancestry []string, scopes ...AgentTokenScope) AgentIdentity {
	return &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
		Scopes:    scopes,
		Ancestry:  ancestry,
	}}
}

// msgAuthzUserIdentity creates a UserIdentity (non-admin, not project-owner).
func msgAuthzUserIdentity(userID string) UserIdentity {
	return NewAuthenticatedUser(userID, userID+"@test.com", "Test User", store.UserRoleMember, "api")
}

// msgAuthzAdminIdentity creates a super-admin UserIdentity.
func msgAuthzAdminIdentity() UserIdentity {
	return NewAuthenticatedUser(tid("msg-superadmin"), "admin@test.com", "Super Admin", store.UserRoleAdmin, "api")
}

const (
	authzHelperProjectA = "authz-project-a"
	authzHelperProjectB = "authz-project-b"
)

var authzHelperAgentID = tid("authz-caller-agent")

// authzHelperCaptureLogs redirects the default slog logger into a buffer for the
// duration of the test and returns the buffer. The denial log line is a
// deliverable of #591, not decoration, so it is asserted on directly.
func authzHelperCaptureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// authzHelperDenialRecord returns the first "authorization denied" record in the
// captured log output, or nil if there is none.
func authzHelperDenialRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] == "authorization denied" {
			return rec
		}
	}
	return nil
}

// authzHelperAgent builds an agent identity in the given project with the given scopes.
func authzHelperAgent(projectID string, scopes ...AgentTokenScope) AgentIdentity {
	return &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: authzHelperAgentID},
		ProjectID: projectID,
		Scopes:    scopes,
	}}
}

func authzHelperAdmin() UserIdentity {
	return NewAuthenticatedUser(authzHelperAdminID, "admin@test.com", "Admin", store.UserRoleAdmin, "api")
}

// authzHelperSeedAdmin creates the admin user in the store with a super-admin
// role binding so that the AK1 kernel grants access. Call once per testServer.
func authzHelperSeedAdmin(t *testing.T, s store.Store) {
	t.Helper()
	createTestUserWithRole(t, s, authzHelperAdminID, "admin@test.com", "admin", store.SystemRoleSuperAdmin)
}

func authzHelperMember() UserIdentity {
	return NewAuthenticatedUser("authz-member", "member@test.com", "Member", "member", "api")
}

// authzHelperTargetAgent is the agent acted upon in lifecycle tests.
func authzHelperTargetAgent() *store.Agent {
	return &store.Agent{
		ID:        "authz-target-agent",
		Name:      "target",
		Slug:      "target",
		ProjectID: authzHelperProjectA,
		OwnerID:   "some-other-user",
	}
}

// agentToken mints a real, signed agent JWT for a synthetic caller "agent"
// in callerProjectID with the given scopes. The credential-status gate in
// the shared auth middleware (auth.go's evaluateAgentCredentialStatus) only
// consults a credential-ID-keyed store, not store.Agent by ID -- a token
// with no matching credential row authenticates via the documented legacy
// compatibility path -- so the caller does not need its own store.Agent row
// for these routing tests, unlike authorizeAgentKeys' *target*, which must
// be a real row.
func (f *agentKeysRouteFixture) agentToken(t *testing.T, callerAgentID, callerProjectID string, scopes ...AgentTokenScope) string {
	t.Helper()
	tok, err := f.srv.GetAgentTokenService().GenerateAgentToken(callerAgentID, callerProjectID, scopes, nil)
	require.NoError(t, err)
	return tok
}

// keysErrorEnvelope decodes a Hub error envelope response body, including
// the details map so callers can inspect operation_id presence/value.
type keysErrorEnvelope struct {
	Code    string                 `json:"code"`
	Message string                 `json:"message"`
	Details map[string]interface{} `json:"details"`
}

func (s *agentKeysLookupSpyStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	atomic.AddInt32(&s.getAgentCalls, 1)
	if s.failLookups {
		return nil, errAgentKeysSpyStoreFailure
	}
	return s.Store.GetAgent(ctx, id)
}

func (s *agentKeysLookupSpyStore) GetAgentBySlug(ctx context.Context, projectID, slug string) (*store.Agent, error) {
	atomic.AddInt32(&s.getAgentBySlugCalls, 1)
	if s.failLookups {
		return nil, errAgentKeysSpyStoreFailure
	}
	return s.Store.GetAgentBySlug(ctx, projectID, slug)
}

func (s *agentKeysLookupSpyStore) lookupCount() int32 {
	return atomic.LoadInt32(&s.getAgentCalls) + atomic.LoadInt32(&s.getAgentBySlugCalls)
}

// keysDenialFixedMessage is agentKeysOutcomeMessage's fixed, sanitized
// message for each outcome this file exercises (execute_agent_keys.go).
var keysDenialFixedMessage = map[string]string{
	"keys_denied":                    "Insufficient permissions",
	"cross_project_keys_unsupported": "Cross-project keys access is not supported for agent callers",
	"not_found":                      "Agent not found",
	"keys_unavailable":               "Keys dispatch is currently unavailable",
}

func msgAuthzSetupOn(t *testing.T, srv *Server, s store.Store) (owner, member *store.User, projectID string) {
	t.Helper()
	ctx := context.Background()

	projectID = tid(msgAuthzProjectID)

	owner = &store.User{
		ID:          tid("msg-owner"),
		Email:       "owner@test.com",
		DisplayName: "Project Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require_NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	member = &store.User{
		ID:          tid("msg-member"),
		Email:       "member@test.com",
		DisplayName: "Project Member",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require_NoError(t, s.CreateUser(ctx, member))
	ensureHubMembership(ctx, s, member.ID)

	project := &store.Project{
		ID:        projectID,
		Name:      "msg-authz-project",
		Slug:      "msg-authz-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require_NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)

	// Add member to the project members group so policies apply.
	msgAuthzAddProjectMember(t, s, member.ID, projectID, "msg-authz-project", store.GroupMemberRoleMember)

	// CO1: Seed a super-admin user for msgAuthzAdminIdentity(). The AK1
	// kernel requires role bindings — the User.Role field alone is not enough.
	createTestUserWithRole(t, s, tid("msg-superadmin"), "admin@test.com", "admin", store.SystemRoleSuperAdmin)

	return owner, member, projectID
}

// authzHelperAdminID is the stable UUID for the admin user in authorize_test.go.
// Must be a valid UUID because the store requires UUID primary keys.
var authzHelperAdminID = tid("authz-admin")

var errAgentKeysSpyStoreFailure = errors.New("agentKeysLookupSpyStore: simulated store failure")

const (
	msgAuthzProjectID = "msg-authz-project"
)

type grantGuardAgentIdentity struct {
	id        string
	projectID string
	scopes    []AgentTokenScope
	ancestry  []string
}

func (a *grantGuardAgentIdentity) ID() string         { return a.id }
func (a *grantGuardAgentIdentity) Type() string       { return "agent" }
func (a *grantGuardAgentIdentity) ProjectID() string  { return a.projectID }
func (a *grantGuardAgentIdentity) Ancestry() []string { return a.ancestry }
func (a *grantGuardAgentIdentity) OriginUserID() string {
	if len(a.ancestry) > 0 {
		return a.ancestry[0]
	}
	return ""
}
func (a *grantGuardAgentIdentity) TokenID() string { return "test-token" }

// localAncestryProvenance opts this fake into AncestryIsHubAttested: the
// marker is not inherited from Type() == "agent", so test fakes must opt in
// explicitly.
func (a *grantGuardAgentIdentity) localAncestryProvenance() ancestryProvenance {
	return ancestryProvenanceAgentJWT
}
func (a *grantGuardAgentIdentity) Scopes() []AgentTokenScope {
	return a.scopes
}
func (a *grantGuardAgentIdentity) HasScope(scope AgentTokenScope) bool {
	for _, s := range a.scopes {
		if s == scope {
			return true
		}
	}
	return false
}
