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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setBrokerAgentCeiling overrides the seeded max_agents_per_broker limit
// (default 100, see seedLimitDefinitions) to a small value so tests can hit
// it without creating dozens of agents.
func setBrokerAgentCeiling(t *testing.T, s store.Store, value int64) {
	t.Helper()
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err, "max_agents_per_broker must be seeded by New()/seedLimitDefinitions")
	def.DefaultValue = value
	_, err = s.UpdateLimitDefinition(context.Background(), def)
	require.NoError(t, err)
}

// addProjectOnBroker creates a project wired to the given broker, the same
// way setupCreateAgentServer wires project1 to its broker, so a test can put
// a second project on the SAME broker (to prove the ceiling is shared) or on
// a freshly created broker (to prove ceilings across brokers are independent).
func addProjectOnBroker(t *testing.T, s store.Store, slug string, broker *store.RuntimeBroker) *store.Project {
	t.Helper()
	ctx := context.Background()
	project := &store.Project{
		ID:   tid("project-" + slug),
		Name: "Project " + slug,
		Slug: slug,
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))
	return project
}

// newTestBroker creates and registers an additional online runtime broker,
// independent of the one setupCreateAgentServer wires up by default.
func newTestBroker(t *testing.T, s store.Store, slug string) *store.RuntimeBroker {
	t.Helper()
	broker := &store.RuntimeBroker{
		ID:     tid("broker-" + slug),
		Name:   "Broker " + slug,
		Slug:   slug,
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), broker))
	return broker
}

// compactCaller is one identity class making requests through the real
// HTTP handler (or, for the unauthenticated case, the handler directly).
type compactCaller struct {
	name string
	do   func(t *testing.T, path string) *httptest.ResponseRecorder
}

// compactFixture holds one project with a mix of agents readable to
// different identity classes, a second project, and one caller per
// identity class.
type compactFixture struct {
	srv     *Server
	store   store.Store
	project *store.Project
	other   *store.Project
	owner   *store.User
	member  *store.User
	agents  []*store.Agent // agents in project, in creation order
	callers []compactCaller
	// shortCircuit holds callers that reach the global endpoint's empty
	// short-circuit responses (no identity, None scope).
	shortCircuit []compactCaller
}

func compactUser(t *testing.T, s store.Store, slug string) *store.User {
	t.Helper()
	u := &store.User{
		ID: tid("cv-" + slug), Email: "cv-" + slug + "@test.com", DisplayName: slug,
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(context.Background(), u))
	ensureHubMembership(context.Background(), s, u.ID)
	return u
}

func compactSetup(t *testing.T) *compactFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	f := &compactFixture{srv: srv, store: s}

	f.owner = compactUser(t, s, "owner")
	f.member = compactUser(t, s, "member")
	related := compactUser(t, s, "related")
	constrained := compactUser(t, s, "constrained")

	f.project = &store.Project{
		ID: tid("cv-project"), Name: "Compact View", Slug: "cv-project",
		OwnerID: f.owner.ID, CreatedBy: f.owner.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, f.project))
	srv.createProjectMembersGroup(ctx, f.project)
	createTestUserWithProjectRole(t, s, f.owner.ID, f.owner.Email, f.project.ID, store.ProjectRoleOwner)
	msgAuthzAddProjectMember(t, s, f.member.ID, f.project.ID, f.project.Slug, store.GroupMemberRoleMember)
	msgAuthzAddProjectMember(t, s, constrained.ID, f.project.ID, f.project.Slug, store.GroupMemberRoleMember)
	// related holds only agent.list on the project and owns two of its agents.
	grantProjectListOnly(t, s, related.ID, f.project.ID, "cv-list-only")

	f.other = &store.Project{
		ID: tid("cv-other"), Name: "Compact Other", Slug: "cv-other",
		OwnerID: f.owner.ID, CreatedBy: f.owner.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, f.other))
	srv.createProjectMembersGroup(ctx, f.other)
	createTestUserWithProjectRole(t, s, f.owner.ID, f.owner.Email, f.other.ID, store.ProjectRoleOwner)
	msgAuthzAddProjectMember(t, s, constrained.ID, f.other.ID, f.other.Slug, store.GroupMemberRoleMember)
	// A project-scoped access constraint removes agent.list in the other
	// project for constrained.
	pType := "user"
	_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "cv-block-other",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   strPtr(constrained.ID),
		ScopeType:            "project",
		ScopeID:              f.other.ID,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "compact view parity",
		CreatedBy:            f.owner.ID,
	})
	require.NoError(t, err)

	base := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	// updatedOrder makes the updated order differ from the created order.
	updatedOrder := []int{4, 0, 6, 2, 5, 1, 3}
	for i := 0; i < 7; i++ {
		owner := f.owner.ID
		if i == 1 || i == 4 {
			owner = related.ID
		}
		a := &store.Agent{
			ID: tid(fmt.Sprintf("cv-agent-%d", i)), Slug: fmt.Sprintf("cv-agent-%d", i), Name: fmt.Sprintf("Agent %d", i),
			Template:  []string{"", "claude"}[i%2],
			ProjectID: f.project.ID, Phase: []string{"running", "stopped"}[i%2],
			Activity:  []string{"", "idle", "executing"}[i%3],
			Labels:    map[string]string{"team": []string{"a", "b"}[i%2]},
			CreatedBy: owner, OwnerID: owner,
			AppliedConfig: &store.AgentAppliedConfig{
				CreatorName: fmt.Sprintf("Creator %d", i),
				Image:       "example.com/agent:latest",
				Model:       "model-x",
				Task:        strings.Repeat("task text ", 20),
				Env:         map[string]string{"SECRET_TOKEN": "s3cr3t"},
			},
		}
		if i >= 3 {
			a.Ancestry = []string{f.agentIDAt(0)}
		}
		if i == 5 {
			a.Ancestry = []string{f.agentIDAt(0), f.agentIDAt(3)}
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		lae := ""
		if i%3 == 0 {
			lae = base.Add(time.Duration(30+i) * time.Second).String()
		}
		setRawAgentTimes(t, s, a.ID,
			base.Add(time.Duration(i)*time.Second).String(),
			base.Add(time.Duration(10+updatedOrder[i])*time.Second).String(), lae)
		f.agents = append(f.agents, a)
	}
	for i := 0; i < 3; i++ {
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{
			ID: tid(fmt.Sprintf("cv-other-%d", i)), Slug: fmt.Sprintf("cv-other-%d", i), Name: fmt.Sprintf("Other %d", i),
			ProjectID: f.other.ID, Phase: "stopped", CreatedBy: f.owner.ID, OwnerID: f.owner.ID,
			AppliedConfig: &store.AgentAppliedConfig{CreatorName: "Other Creator"},
		}))
	}

	hubAdminID := tid("cv-hub-admin")
	createTestUserWithRole(t, s, hubAdminID, "cv-hub-admin@test.com", store.UserRoleMember, store.SystemRoleHubAdmin)
	hubAdmin, err := s.GetUser(ctx, hubAdminID)
	require.NoError(t, err)
	superAdminID := tid("cv-super-admin")
	createTestUserWithRole(t, s, superAdminID, "cv-super-admin@test.com", store.UserRoleAdmin, store.SystemRoleSuperAdmin)
	superAdmin, err := s.GetUser(ctx, superAdminID)
	require.NoError(t, err)

	agentTok, err := srv.GetAgentTokenService().GenerateAgentToken(f.agents[0].ID, f.project.ID, []AgentTokenScope{ScopeProjectRead}, nil)
	require.NoError(t, err)
	uatKey := mintScopedUAT(t, srv, f.owner.ID, f.project.ID, []string{"agent:manage"})

	asUser := func(u *store.User) func(t *testing.T, path string) *httptest.ResponseRecorder {
		return func(t *testing.T, path string) *httptest.ResponseRecorder {
			return doRequestAsUser(t, srv, u, http.MethodGet, path, nil)
		}
	}
	f.callers = []compactCaller{
		{"owner", asUser(f.owner)},
		{"member", asUser(f.member)},
		{"hub-admin-non-member", asUser(hubAdmin)},
		{"super-admin", asUser(superAdmin)},
		{"agent-jwt", func(t *testing.T, path string) *httptest.ResponseRecorder {
			return doRequestWithAgentToken(t, srv, http.MethodGet, path, nil, agentTok)
		}},
		{"scoped-uat", func(t *testing.T, path string) *httptest.ResponseRecorder {
			return doRequestWithUAT(t, srv, uatKey, http.MethodGet, path, nil)
		}},
		{"project-constrained", asUser(constrained)},
		{"relationship-grant", asUser(related)},
	}

	none := noScopeUser(t, s)
	f.shortCircuit = []compactCaller{
		{"none-scope", asUser(none)},
		{"unauthenticated", func(t *testing.T, path string) *httptest.ResponseRecorder {
			u, err := url.Parse(path)
			require.NoError(t, err)
			require.Equal(t, "/api/v1/agents", u.Path, "the unauthenticated short-circuit is a global endpoint path")
			return listAgentsUnauthenticated(srv, u.RawQuery)
		}},
	}
	return f
}

// withQuery joins a base path and query parts, skipping empty parts.
func withQuery(base string, parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return base
	}
	return base + "?" + strings.Join(kept, "&")
}

// dropParam removes every name=... part from an & separated query.
func dropParam(query, name string) string {
	var kept []string
	for _, p := range strings.Split(query, "&") {
		if p != "" && !strings.HasPrefix(p, name+"=") {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "&")
}

func decodeObject(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &m), string(body))
	return m
}

func decodeItems(t *testing.T, raw json.RawMessage) []map[string]json.RawMessage {
	t.Helper()
	var items []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &items))
	return items
}

// createFixtureAgent stores an agent in the fixture project with the given
// ancestry and applied role.
func createFixtureAgent(t *testing.T, f *bypassAgentsFixture, name string, ancestry []string, role AgentRole) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID: tid(name), Slug: tid(name), Name: name, ProjectID: f.proj.ID,
		Phase: "running", CreatedBy: ancestry[0], OwnerID: ancestry[0], Ancestry: ancestry,
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(role)},
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	return a
}

// createAsAgent posts a child-agent create in the fixture project with a
// token for agentID carrying the scopes of its stored role plus
// ScopeAgentCreate.
func createAsAgent(t *testing.T, f *bypassAgentsFixture, agentID string, req CreateAgentRequest) *httptest.ResponseRecorder {
	t.Helper()
	svc := f.srv.GetAgentTokenService()
	require.NotNil(t, svc)
	stored, err := f.store.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	role, _ := agentRoleAndScopes(stored)
	scopes := append([]AgentTokenScope{ScopeProjectRead, ScopeAgentCreate}, ScopesForRole(role)...)
	tok, err := svc.GenerateAgentToken(agentID, f.proj.ID, scopes, nil)
	require.NoError(t, err)
	return doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/agents", req, tok)
}

// grantFixtureRole binds userID to a seeded project role in the fixture
// project.
func grantFixtureRole(t *testing.T, f *bypassAgentsFixture, userID, role string) {
	t.Helper()
	grantProjectRole(t, f.store, userID, f.proj.ID, role)
}

func newChainFixture(t *testing.T, name string) *chainFixture {
	t.Helper()
	f := newUATCreateFixture(t, name)
	return &chainFixture{uatCreateFixture: f, client: f.withDispatcher(t)}
}

// deliverOf returns the hub delivery permissions in ids, sorted.
func deliverOf(ids []string) []string {
	var out []string
	for _, id := range ids {
		if hubDeliveryPermissionSet[id] {
			out = append(out, id)
		}
	}
	return sortedUniqueIDs(out)
}

// agentDelegatorEdges returns every edge delegated by agent agentID.
func agentDelegatorEdges(t *testing.T, s store.Store, agentID string) []*store.DelegationEdge {
	t.Helper()
	edges, err := s.GetDelegationEdgesForDelegator(context.Background(), store.DelegationPrincipalAgent, agentID)
	require.NoError(t, err)
	return edges
}

// legacyFixture is a chain fixture with an owner role binding and a legacy
// agent L whose only edge is unrecorded.
type legacyFixture struct {
	*chainFixture
	legacy *store.Agent
	sa     *store.GCPServiceAccount
}

func newLegacyFixture(t *testing.T, name string) *legacyFixture {
	t.Helper()
	f := newChainFixture(t, name)
	ctx := context.Background()
	f.srv.createProjectMembersGroup(ctx, f.proj)
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(ctx, f.proj.ID, f.owner.ID))
	legacy := createFixtureAgent(t, f.bypassAgentsFixture, name+"-l", []string{f.owner.ID}, AgentRoleFull)
	addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, legacy.ID, f.proj.ID)
	sa := bypassAgentsCreateSA(t, f.bypassAgentsFixture, f.proj.ID, true)
	return &legacyFixture{chainFixture: f, legacy: legacy, sa: sa}
}

// assertSAGateUnrecordedDenied asserts the SA gate's 403 for a delegation
// chain with an unrecorded hop.
func assertSAGateUnrecordedDenied(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, scaUnrecordedDenyMsg, decodeTargetAPIError(t, rec).Message)
}

// runIntentErrStore fails every run-intent write, including a start claim.
type runIntentErrStore struct {
	store.Store
}

// createRollbackSite is one create failure site: the dispatcher and server
// setup that make the create fail there, and the stage its rollback records.
type createRollbackSite struct {
	name      string
	disp      AgentDispatcher
	setup     func(t *testing.T, srv *Server)
	req       CreateAgentRequest
	wantStage string
}

// createRollbackSites returns one case per create failure site, each with a
// fresh dispatcher.
func createRollbackSites() []createRollbackSite {
	workspaceFiles := []transfer.FileInfo{{Path: "main.go", Size: 100, Hash: "sha256:abc123"}}
	return []createRollbackSite{
		{
			name:      "storage",
			disp:      &createAgentDispatcher{},
			req:       CreateAgentRequest{WorkspaceFiles: workspaceFiles},
			wantStage: createStageStorage,
		},
		{
			name: "upload URL",
			disp: &createAgentDispatcher{},
			setup: func(t *testing.T, srv *Server) {
				srv.SetStorage(signErrStorage{newMockStorage("test-bucket")})
			},
			req:       CreateAgentRequest{WorkspaceFiles: workspaceFiles},
			wantStage: createStageUploadURL,
		},
		{
			// Hub-managed workspace upload for a remote broker, with the
			// workspace storage mount hung.
			name: "workspace storage",
			disp: &createAgentDispatcher{},
			setup: func(t *testing.T, srv *Server) {
				tmpHome := t.TempDir()
				t.Setenv("HOME", tmpHome)
				mountRoot := filepath.Join(tmpHome, "nfs-mount")
				hangReadDirFor(t, mountRoot)
				srv.config.WorkspaceStorageConfig = nfsConfig(mountRoot)
				srv.SetStorage(newMockStorage("test-bucket"))
			},
			req:       CreateAgentRequest{Workspace: "subdir"},
			wantStage: createStageWorkspaceStorage,
		},
		{
			name:      "managed",
			disp:      &createAgentDispatcher{},
			setup:     func(t *testing.T, _ *Server) { useFailingManagedBackend(t) },
			req:       CreateAgentRequest{Profile: ManagedAgentsProfile},
			wantStage: createStageManaged,
		},
		{
			name: "run intent",
			disp: &createAgentDispatcher{},
			setup: func(t *testing.T, srv *Server) {
				srv.store = runIntentErrStore{srv.store}
			},
			wantStage: createStageRunIntent,
		},
		{
			name: "run intent with env gather",
			disp: &createAgentDispatcher{},
			setup: func(t *testing.T, srv *Server) {
				srv.store = runIntentErrStore{srv.store}
			},
			req:       CreateAgentRequest{GatherEnv: true},
			wantStage: createStageRunIntent,
		},
		{
			name:      "dispatch with env gather",
			disp:      &failingCreateDispatcher{createErr: errors.New("broker unavailable")},
			req:       CreateAgentRequest{GatherEnv: true},
			wantStage: createStageDispatchEnvGather,
		},
		{
			name:      "dispatch",
			disp:      &failingCreateDispatcher{createErr: errors.New("broker unavailable")},
			wantStage: createStageDispatch,
		},
		{
			name:      "missing env",
			disp:      &createAgentDispatcher{envReqs: &RemoteEnvRequirementsResponse{Needs: []string{"SOME_REQUIRED_KEY"}}},
			wantStage: createStageMissingEnv,
		},
		{
			name:      "provision",
			disp:      &skillFailDispatcher{provisionErr: brokerSkillError(http.StatusNotFound, "not_found", "")},
			req:       CreateAgentRequest{ProvisionOnly: true},
			wantStage: createStageProvision,
		},
	}
}

// createTxFaultStore injects failures into the writes of the agent-create
// transaction and its compensation, including inside WithTx.
type createTxFaultStore struct {
	store.Store
	// auditErrFor fails CreateMutationAudit for records of this mutation
	// type.
	auditErrFor string
	subErr      error
	deactErr    error
	// outerDeleteErr fails DeleteAgent outside a transaction, and every
	// FinalizeAgentDeletion (the conditional compensation's row delete).
	outerDeleteErr error
}

// agentAudits returns the mutation audit records of type for agentID.
func agentAudits(t *testing.T, s store.Store, mutationType, agentID string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{TargetType: "agent", MutationType: mutationType})
	require.NoError(t, err)
	var out []*store.MutationAuditRecord
	for _, r := range recs {
		if r.TargetID == agentID {
			out = append(out, r)
		}
	}
	return out
}

// compensationSummary is the AfterSummary of an agent_create_dispatch_failed
// record.
type compensationSummary struct {
	OriginalAuditID string `json:"original_audit_id"`
	OpID            string `json:"op_id"`
	Stage           string `json:"stage"`
	Error           string `json:"error"`
}

// assertCompensated asserts that agentID was rolled back by compensation:
// no agent row, no active edge, a create_compensation deactivation under
// the op ID the agent_create_dispatch_failed record names, and that record
// referencing the create's own audit record.
func assertCompensated(t *testing.T, s store.Store, agentID string) compensationSummary {
	t.Helper()
	ctx := context.Background()
	_, err := s.GetAgent(ctx, agentID)
	require.ErrorIs(t, err, store.ErrNotFound, "agent row deleted")
	assert.Empty(t, activeEdgesFor(t, s, agentID), "no active edge")

	created := agentAudits(t, s, mutationTypeAgentDelegation, agentID)
	require.Len(t, created, 1, "the create's audit record is kept")
	failed := agentAudits(t, s, mutationTypeAgentCreateDispatchFailed, agentID)
	require.Len(t, failed, 1)
	var sum compensationSummary
	require.NoError(t, json.Unmarshal([]byte(failed[0].AfterSummary), &sum))
	assert.Equal(t, created[0].ID, sum.OriginalAuditID)
	require.NotEmpty(t, sum.OpID)
	assert.NotEmpty(t, failed[0].ActorPrincipalKind)

	// The edge was deactivated with cause create_compensation under that op
	// ID: reactivating exactly that (cause, op ID) finds one edge. Undo it
	// afterwards so the caller sees the compensated state.
	n, err := s.ReactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agentID,
		store.EdgeDeactivationCreateCompensation, sum.OpID)
	require.NoError(t, err)
	require.Equal(t, 1, n, "one edge deactivated with cause create_compensation")
	_, err = s.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agentID,
		store.Deactivation{Cause: store.EdgeDeactivationCreateCompensation, OpID: sum.OpID})
	require.NoError(t, err)
	return sum
}

// serveWithRequestID serves r with request metadata carrying requestID, as
// the request-log middleware installs it, and returns the response and the
// compensation-failure records logged while it ran.
func serveWithRequestID(t *testing.T, h http.Handler, r *http.Request, requestID string) (*httptest.ResponseRecorder, []compensationFailureLog) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx := logging.ContextWithRequestMeta(r.Context(), &logging.RequestMeta{RequestID: requestID})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r.WithContext(ctx))
	slog.SetDefault(prev)

	var logs []compensationFailureLog
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		var l compensationFailureLog
		if json.Unmarshal(line, &l) == nil && l.Msg == "agent create compensation failed" {
			logs = append(logs, l)
		}
	}
	return rec, logs
}

// uatCreateFixture is the bypassAgents world plus a member user who holds
// agent.create in the fixture project.
type uatCreateFixture struct {
	*bypassAgentsFixture
	creator *store.User
	path    string
}

func newUATCreateFixture(t *testing.T, name string) *uatCreateFixture {
	t.Helper()
	f := bypassAgentsSetup(t)
	creator := hubMemberUser(t, f.store, name+"-creator")
	grantFixtureRole(t, f, creator.ID, store.ProjectRoleMember)
	return &uatCreateFixture{bypassAgentsFixture: f, creator: creator, path: "/api/v1/projects/" + f.proj.ID + "/agents"}
}

// minimalSelectors is agent:create plus the seven read selectors.
func minimalSelectors(t *testing.T) []string {
	t.Helper()
	createP, _ := registryPermission("agent.create")
	return append([]string{createP.UATScope}, readonlyRoleUATSelectors(t)...)
}

// assertCreateWroteNothing asserts no agent row for slug, no edge delegated
// by delegatorID, no agent audit record and no project subscription.
func assertCreateWroteNothing(t *testing.T, s store.Store, projectID, slug, delegatorID string) {
	t.Helper()
	ctx := context.Background()
	_, err := s.GetAgentBySlug(ctx, projectID, slug)
	assert.ErrorIs(t, err, store.ErrNotFound, "no agent row")
	edges, err := s.GetDelegationEdgesForDelegator(ctx, store.DelegationPrincipalUser, delegatorID)
	require.NoError(t, err)
	assert.Empty(t, edges, "no delegation edge")
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{TargetType: "agent"})
	require.NoError(t, err)
	assert.Empty(t, audits, "no agent audit record")
	subs, err := s.GetNotificationSubscriptionsByProject(ctx, projectID)
	require.NoError(t, err)
	assert.Empty(t, subs, "no subscription")
}

// assertCeilingDenial asserts a 403 carrying details.denied_by and returns
// the message.
func assertCeilingDenial(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	apiErr := decodeTargetAPIError(t, rec)
	assert.Equal(t, map[string]interface{}{"denied_by": string(DeniedByDelegationCeiling)}, apiErr.Details)
	return apiErr.Message
}

func recordCreatedEvents(t *testing.T, srv *Server) *createdRecordingPublisher {
	t.Helper()
	bus := NewChannelEventPublisher()
	t.Cleanup(bus.Close)
	pub := &createdRecordingPublisher{newDeleteRecordingPublisher(bus)}
	srv.events = pub
	return pub
}

// createRaceDispatcher runs hook inside DispatchAgentCreateWithGather, as a
// DELETE that lands while the broker create is in flight would.
type createRaceDispatcher struct {
	engineStubDispatcher
	hook func(a *store.Agent)
}

// raceAsyncClient is asyncLaunchClient whose broker delete can block.
type raceAsyncClient struct {
	*asyncLaunchClient
	mu       sync.Mutex
	deleteFn func(ctx context.Context) error
}

// newRaceAsyncCreateServer is newAsyncCreateServer (flag on) with a
// raceAsyncClient, so a test can interleave a DELETE with the accepted
// launch.
func newRaceAsyncCreateServer(t *testing.T) (*Server, store.Store, *store.Project, *raceAsyncClient) {
	t.Helper()
	ctx := context.Background()
	srv, s, project := setupCreateAgentServer(t, &createAgentDispatcher{})
	broker, err := s.GetRuntimeBroker(ctx, tid("broker-create"))
	require.NoError(t, err)
	broker.Endpoint = "http://localhost:9800"
	broker.Capabilities = &store.BrokerCapabilities{AsyncLaunch: true}
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

	client := &raceAsyncClient{asyncLaunchClient: &asyncLaunchClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}}
	d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	d.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings {
		return AsyncLaunchSettings{Enabled: true, Timeout: 5 * time.Minute, KeepaliveSeconds: 15}
	})
	srv.SetDispatcher(d)
	return srv, s, project, client
}

const (
	// deleteClaimed: the DELETE has claimed the row and is blocked in its
	// broker dispatch; it completes after the create answers.
	deleteClaimed deleteMode = "claimed"
	// deleteDone: the DELETE has finished (row gone or soft-deleted).
	deleteDone deleteMode = "done"
)

// assertDeleteLanded checks the row is hard-deleted, or soft-deleted when
// retention is on.
func assertDeleteLanded(t *testing.T, s store.Store, agentID string, retention time.Duration) {
	t.Helper()
	if retention == 0 {
		assert.True(t, agentGone(t, s, agentID), "hard delete removes the row")
		return
	}
	got := mustGetAgent(t, s, agentID)
	assert.False(t, got.DeletedAt.IsZero(), "soft delete keeps a tombstoned row")
}

// assertNoStoppedStatus checks the delete published no status with phase
// stopped (design ptone/scion#2483 R1: claim status, then deleted).
func assertNoStoppedStatus(t *testing.T, pub *createdRecordingPublisher) {
	t.Helper()
	for _, e := range pub.snapshot() {
		assert.False(t, e.kind == "status" && e.phase == string(state.PhaseStopped),
			"no stopped status: %+v", pub.snapshot())
	}
}

// hookStorage is mockStorage whose first GenerateSignedURL runs hook: the
// workspace-bootstrap create signs its upload URLs after the row is written
// and before it publishes created.
type hookStorage struct {
	*mockStorage
	once sync.Once
	hook func()
}

// insertTestAgentCredential records an active credential for agentID under
// jti, the way a production mint records it. Tests use it to seed a
// credential outside the dispatch path under test (e.g. a sibling agent's
// credential that must survive the test's revoke call).
func insertTestAgentCredential(t *testing.T, s store.AgentCredentialStore, agentID, projectID, jti string) {
	t.Helper()
	now := time.Now()
	cred := &store.AgentCredential{
		AgentID:      agentID,
		ProjectID:    projectID,
		TokenJTIHash: hashJTI(jti),
		IssuedAt:     now,
		ExpiresAt:    now.Add(10 * time.Hour),
	}
	if err := s.CreateAgentCredential(context.Background(), cred); err != nil {
		t.Fatalf("failed to insert test agent credential: %v", err)
	}
}

// getTestAgentCredential looks a credential back up by its plaintext jti
// (hashing it the same way the production code does) so a test can assert
// on RevokedAt/RevokeReason after exercising a dispatch or handler path.
func getTestAgentCredential(t *testing.T, s store.AgentCredentialStore, jti string) *store.AgentCredential {
	t.Helper()
	cred, err := s.GetAgentCredentialByJTIHash(context.Background(), hashJTI(jti))
	if err != nil {
		t.Fatalf("failed to look up test agent credential for jti %q: %v", jti, err)
	}
	return cred
}

// fakeMintingTokenGenerator implements AgentTokenGenerator for dispatcher
// tests. SignAgentToken mints a fake token and returns its credential for
// the caller to record, as production's AgentTokenService does, and
// remembers every jti it issued (in call order) so a test can look up what
// it minted afterward. GenerateAgentToken records the credential itself.
// failWith fails AuthorizeAgentToken and GenerateAgentToken.
type fakeMintingTokenGenerator struct {
	store    store.AgentCredentialStore
	jtis     []string
	failWith error
}

// revokeFailingCredentialStore wraps a real store.Store and makes
// RevokeAgentCredentialsByAgent always fail, so a test can verify a
// revoke-store error is logged and swallowed rather than masking or
// replacing the original dispatch/handler error.
type revokeFailingCredentialStore struct {
	store.Store
	failWith error
}

// apiErrorBody mirrors the JSON shape written by writeError, for assertions
// on the error code and message returned to the client.
type apiErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeAPIError(t *testing.T, body []byte) apiErrorBody {
	t.Helper()
	var resp apiErrorBody
	require.NoError(t, json.Unmarshal(body, &resp))
	return resp
}

// buildAgentRefreshRequest constructs a POST .../token/refresh request whose
// context carries the given agent identity and optional credential markers,
// without routing it through UnifiedAuthMiddleware, so the refresh handler's
// own status evaluation (including the "marker absent" fallback) can be
// exercised directly and deterministically.
func buildAgentRefreshRequest(agentID string, claims *AgentTokenClaims, credentialID string, legacy bool) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/token/refresh", nil)
	ctx := req.Context()
	identity := &agentIdentityWrapper{claims}
	ctx = contextWithIdentity(ctx, identity)
	if credentialID != "" {
		ctx = context.WithValue(ctx, agentCredentialIDContextKey{}, credentialID)
	}
	if legacy {
		ctx = context.WithValue(ctx, legacyTokenContextKey{}, true)
	}
	return req.WithContext(ctx)
}

// engineHookStore wraps the server's store to inject faults and observe the
// engine's store calls.
type engineHookStore struct {
	store.Store
	mu               sync.Mutex
	revokeErr        error
	revokeCalls      int
	revokeCtxErrs    []error
	onHasOutstanding func()
	// failDeletionWrite, when set, picks UpdateAgentDeletion writes to fail
	// with errInjectedDeletionWrite. It is cleared after the first match, so later
	// writes (such as abandon's) go through.
	failDeletionWrite func(set store.DeletionFields) bool
	// missClaims makes every claim write (BumpClaim) affect no row, as a
	// racing write would; claimMisses counts them.
	missClaims  bool
	claimMisses int
	// onDeletionWrite, when set, observes every UpdateAgentDeletion
	// predicate before the write runs.
	onDeletionWrite func(pred store.DeletionPredicate)
	// afterDeletionWrite, when set, observes every UpdateAgentDeletion
	// predicate and its result after the write ran.
	afterDeletionWrite func(pred store.DeletionPredicate, n int)
}

var errInjectedDeletionWrite = errors.New("injected deletion write error")

// deferredDeleteFixture is a server whose dispatcher is a real
// HTTPAgentDispatcher over a broker client that always defers, so every
// delete goes through deferredDelete → deferredDataOpResult →
// waitForDispatchDone, as in production.
type deferredDeleteFixture struct {
	srv    *Server
	store  store.Store // the raw store
	hooks  *engineHookStore
	bus    *ChannelEventPublisher
	pub    *deleteRecordingPublisher
	client *mockRuntimeBrokerClient
	agent  *store.Agent
}

func newDeferredDeleteFixture(t *testing.T, suffix string, dispatchEvents EventPublisher) *deferredDeleteFixture {
	t.Helper()
	srv, s := testServer(t)
	bus := NewChannelEventPublisher()
	t.Cleanup(bus.Close)
	pub := newDeleteRecordingPublisher(bus)
	srv.events = pub
	hooks := &engineHookStore{Store: s}
	srv.store = hooks
	client := &mockRuntimeBrokerClient{returnErr: ErrLifecycleDeferred}
	d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	if dispatchEvents == nil {
		dispatchEvents = bus
	}
	d.SetCrossNodeDeps(dispatchEvents, NoopCommandBus{})
	srv.SetDispatcher(d)
	agent := setupBrokerAgentInPhase(t, s, suffix, state.PhaseRunning)
	return &deferredDeleteFixture{srv: srv, store: s, hooks: hooks, bus: bus, pub: pub, client: client, agent: agent}
}

func setDeleteWaitTimeout(t *testing.T, fn func(ctx context.Context) time.Duration) {
	t.Helper()
	old := deleteWaitTimeoutFn
	deleteWaitTimeoutFn = fn
	t.Cleanup(func() { deleteWaitTimeoutFn = old })
}

// endIntent claims a dispatch intent (as an owning node would) and ends it.
func endIntent(t *testing.T, s store.Store, id string, ok bool) {
	t.Helper()
	ctx := context.Background()
	claimed, err := s.ClaimBrokerDispatch(ctx, id, "test-owner")
	require.NoError(t, err)
	require.True(t, claimed)
	if ok {
		require.NoError(t, s.CompleteBrokerDispatch(ctx, id, ""))
	} else {
		require.NoError(t, s.FailBrokerDispatch(ctx, id, "gave up", ""))
	}
}

func requireInDoubt(t *testing.T, f *deferredDeleteFixture, r *deleteResult) {
	t.Helper()
	require.Equal(t, http.StatusBadGateway, r.rec.Code, r.rec.Body.String())
	_, details := errorBody(t, r.rec)
	assert.Equal(t, store.DeletionCodeInDoubt, details["deletionCode"])
	got := mustGetAgent(t, f.store, f.agent.ID)
	assert.Equal(t, store.DeletionStateFailed, got.DeletionState)
	assert.Equal(t, store.DeletionCodeInDoubt, got.DeletionCode)
	assert.Equal(t, string(state.PhaseStopping), got.Phase, "in_doubt does not roll back to a live phase")
	assert.True(t, got.DeletedAt.IsZero())
}

// deletedSubscription subscribes subscriber (an agent slug, or a user ID
// when user is true) to DELETED on agent.
func deletedSubscription(t *testing.T, s store.Store, agent *store.Agent, subscriberType, subscriberID string) {
	t.Helper()
	require.NoError(t, s.CreateNotificationSubscription(context.Background(), &store.NotificationSubscription{
		ID: api.NewUUID(), Scope: store.SubscriptionScopeAgent, AgentID: agent.ID,
		SubscriberType: subscriberType, SubscriberID: subscriberID, ProjectID: agent.ProjectID,
		TriggerActivities: []string{"DELETED"}, CreatedAt: time.Now().Add(-time.Minute), CreatedBy: "test",
	}))
}

// launchingAgent creates a broker agent in phase with an active create
// launch whose deadline is timeout away.
func launchingAgent(t *testing.T, s store.Store, suffix string, phase state.Phase, timeout time.Duration) (*store.Agent, string) {
	t.Helper()
	agent := setupBrokerAgentInPhase(t, s, suffix, phase)
	launchID, err := s.BeginLaunch(context.Background(), agent.ID, store.LaunchKindCreate, timeout)
	require.NoError(t, err)
	return mustGetAgent(t, s, agent.ID), launchID
}

func launchReport(t *testing.T, s store.Store, a *store.Agent, launchID, reportState string, seq int64) store.LaunchReportAnswer {
	t.Helper()
	ans, _, err := s.ApplyLaunchReport(context.Background(), a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "inst-1", Seq: seq, State: reportState,
		Phase: string(state.PhaseStarting), Step: "pulling",
	})
	require.NoError(t, err)
	return ans
}

// setDeleteKnob overrides a timing knob for the test.
func setDeleteKnob(t *testing.T, knob *time.Duration, v time.Duration) {
	t.Helper()
	old := *knob
	*knob = v
	t.Cleanup(func() { *knob = old })
}

// recordedAgentEvent is one agent event as a subscriber would see it.
type recordedAgentEvent struct {
	kind     string // "status" | "deleted"
	phase    string
	activity string
	deletion *store.DeletionInfo
}

// deleteRecordingPublisher records agent status and deleted events (with the
// deletion view the real publisher would compute) and forwards everything to
// an inner publisher.
type deleteRecordingPublisher struct {
	EventPublisher
	mu     sync.Mutex
	events []recordedAgentEvent
}

func newDeleteRecordingPublisher(inner EventPublisher) *deleteRecordingPublisher {
	return &deleteRecordingPublisher{EventPublisher: inner}
}

// engineStubDispatcher is a goroutine-safe delete dispatcher whose behaviour
// is a per-test function.
type engineStubDispatcher struct {
	createAgentDispatcher
	mu      sync.Mutex
	calls   int
	softArg []bool
	fn      func(ctx context.Context, a *store.Agent) error
}

// blockingDelete returns a dispatch fn that signals entered and blocks
// until release is closed (or ctx ends), then returns err.
func blockingDelete(entered chan<- struct{}, release <-chan struct{}, err error) func(ctx context.Context, a *store.Agent) error {
	var once sync.Once
	return func(ctx context.Context, _ *store.Agent) error {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// deleteResult is an async DELETE's answer.
type deleteResult struct {
	rec     *httptest.ResponseRecorder
	elapsed time.Duration
}

func deleteAsync(t *testing.T, srv *Server, path string, headers map[string]string) <-chan deleteResult {
	t.Helper()
	out := make(chan deleteResult, 1)
	go func() {
		start := time.Now()
		rec := doRequestHeaders(t, srv, http.MethodDelete, path, nil, headers)
		out <- deleteResult{rec: rec, elapsed: time.Since(start)}
	}()
	return out
}

func waitDelete(t *testing.T, ch <-chan deleteResult, within time.Duration) deleteResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(within):
		t.Fatalf("DELETE did not answer within %s", within)
		return deleteResult{}
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}, within time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(within):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// errorBody decodes the standard error envelope.
func errorBody(t *testing.T, rec *httptest.ResponseRecorder) (string, map[string]interface{}) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string                 `json:"code"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body.Error.Code, body.Error.Details
}

// agentGone reports whether the row is hard-deleted.
func agentGone(t *testing.T, s store.Store, id string) bool {
	t.Helper()
	_, err := s.GetAgent(context.Background(), id)
	if errors.Is(err, store.ErrNotFound) {
		return true
	}
	require.NoError(t, err)
	return false
}

func mustGetAgent(t *testing.T, s store.Store, id string) *store.Agent {
	t.Helper()
	a, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return a
}

// ownerDeleteRequest makes agentID owned by a fresh user and returns a
// DELETE request authenticated as that user on ctx (the resource-owner
// bypass authorizes ActionDelete).
func ownerDeleteRequest(t *testing.T, s store.Store, ctx context.Context, agentID string) *http.Request {
	t.Helper()
	userID := tid("owner-" + agentID)
	require.NoError(t, s.CreateUser(context.Background(), &store.User{
		ID: userID, Email: userID + "@test.com", DisplayName: "Owner", Role: "member", Status: "active",
	}))
	a := mustGetAgent(t, s, agentID)
	a.OwnerID = userID
	a.CreatedBy = userID
	require.NoError(t, s.UpdateAgent(context.Background(), a))
	// The owner relationship requires active project access
	// (ptone/scion#2141); the binding grants no permission itself.
	grantProjectAccessOnly(t, s, userID, a.ProjectID)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/"+agentID, nil)
	return req.WithContext(contextWithIdentity(ctx, NewAuthenticatedUser(userID, userID+"@test.com", "Owner", "member", "cli")))
}

// engineTestServer is a test server with a recording publisher over a real
// channel bus and a stub dispatcher.
func engineTestServer(t *testing.T) (*Server, store.Store, *deleteRecordingPublisher, *engineStubDispatcher) {
	t.Helper()
	srv, s := testServer(t)
	bus := NewChannelEventPublisher()
	t.Cleanup(bus.Close)
	pub := newDeleteRecordingPublisher(bus)
	srv.events = pub
	disp := &engineStubDispatcher{}
	srv.SetDispatcher(disp)
	return srv, s, pub, disp
}

// setDeleteClock pins the engine's clock to now for the test.
func setDeleteClock(t *testing.T, now func() time.Time) {
	t.Helper()
	old := deleteClock
	deleteClock = now
	t.Cleanup(func() { deleteClock = old })
}

// fenceNow is a fixed engine time at second precision (notAfter goes on
// the wire as RFC3339), close to the real time so the store's own
// time-based views agree with the engine's.
func fenceNow(t *testing.T) time.Time {
	t.Helper()
	t0 := time.Now().UTC().Truncate(time.Second)
	setDeleteClock(t, func() time.Time { return t0 })
	return t0
}

func staleDispatchErr() error {
	return &brokerStatusError{StatusCode: http.StatusConflict, Body: staleDispatchBody}
}

// deleteSeed describes a delete marker to seed on an agent.
type deleteSeed struct {
	name    string
	state   string
	leaseIn time.Duration // lease_at = now + leaseIn
	code    string
	intent  bool // also insert an outstanding (pending) broker delete intent
}

var (
	seedLiveDeleting     = deleteSeed{name: "live deleting", state: store.DeletionStateDeleting, leaseIn: time.Minute}
	seedFailedIntent     = deleteSeed{name: "failed with outstanding intent", state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeRuntimeError, intent: true}
	seedInDoubtIntent    = deleteSeed{name: "in_doubt with outstanding intent", state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeInDoubt, intent: true}
	seedExpiredFinalize  = deleteSeed{name: "lease-expired finalizing", state: store.DeletionStateFinalizing, leaseIn: -time.Minute}
	seedRevokeFailedFinl = deleteSeed{name: "revoke_failed finalizing", state: store.DeletionStateFinalizing, leaseIn: -time.Minute, code: store.DeletionCodeRevokeFailed}

	// Every marker that must block start.
	blockingSeeds = []deleteSeed{seedLiveDeleting, seedFailedIntent, seedInDoubtIntent, seedExpiredFinalize, seedRevokeFailedFinl}
)

func seedAgentDeletion(t *testing.T, s store.Store, agentID string, d deleteSeed) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	lease := now.Add(d.leaseIn)
	st := d.state
	set := store.DeletionFields{State: &st, BumpClaim: true, LeaseAt: &lease, StartedAt: &now}
	if d.code != "" {
		code := d.code
		set.Code = &code
	}
	if d.state == store.DeletionStateFailed {
		set.FailedAt = &now
	}
	n, err := s.UpdateAgentDeletion(ctx, agentID, store.DeletionPredicate{}, set)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	if d.intent {
		require.NoError(t, s.InsertBrokerDispatch(ctx, &store.BrokerDispatch{
			ID: uuid.NewString(), BrokerID: uuid.NewString(), AgentID: agentID, Op: brokerDispatchOpDelete,
		}))
	}
}

func requireDeleteInProgress(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeDeleteInProgress)
}

// deleteGuardDispatcher counts start and stop dispatches.
type deleteGuardDispatcher struct {
	createAgentDispatcher
	starts int
	stops  int
}

type dispatchErrStore struct{ store.Store }

// deliverySetup creates a server, project, two agents with a broker and
// dispatcher, and a DM conversation — common scaffolding for delivery
// lifecycle tests.
func deliverySetup(t *testing.T) (
	srv *Server, s store.Store,
	project *store.Project,
	sender, target *store.Agent,
	convID string,
	dispatcher *recordingDispatcher,
) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID:      tid("delivery-owner"),
		Email:   "delivery-owner@test.example",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	project = &store.Project{
		ID:        tid("delivery-project"),
		Name:      "delivery-project",
		Slug:      "delivery-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, project))
	// The owner is a project member, so the fixture agents are in good
	// standing (ptone/scion#3433).
	ensureStandingRoot(t, s, project.ID, owner.ID)

	brokerID := tid("delivery-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "delivery-broker",
		Slug:   "delivery-broker",
		Status: store.BrokerStatusOnline,
	}))

	sender = &store.Agent{
		ID:              tid("delivery-sender"),
		Name:            "delivery-sender",
		Slug:            "delivery-sender",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
		Ancestry:        []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, sender))

	target = &store.Agent{
		ID:              tid("delivery-target"),
		Name:            "delivery-target",
		Slug:            "delivery-target",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
		Ancestry:        []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, target))

	// Create DM conversation.
	dmKey, err := messages.DMConversationKey("agent", sender.ID, "agent", target.ID)
	require.NoError(t, err)
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)
	convID = conv.ID

	dispatcher = &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	return srv, s, project, sender, target, convID, dispatcher
}

// deliveryDMInput creates a standard AgentDMInput for delivery tests.
func deliveryDMInput(sender, target *store.Agent, msg string) *AgentDMInput {
	return &AgentDMInput{
		SenderAgent: sender,
		SenderIdentity: &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: sender.ID},
			ProjectID: sender.ProjectID,
			Ancestry:  sender.Ancestry,
		}},
		TargetAgent: target,
		Msg:         msg,
		Type:        "instruction",
		ProjectID:   sender.ProjectID,
	}
}

// paritySetup creates two same-project agents with a DM conversation,
// a recording dispatcher, and a spy broker bus for parity testing.
func paritySetup(t *testing.T) (
	srv *Server, s store.Store,
	project *store.Project,
	senderAgent, targetAgent *store.Agent,
	convID string, dispatcher *recordingDispatcher,
	spyBus *spyBrokerBus,
) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID:      tid("parity-owner"),
		Email:   "parity-owner@test.example",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	project = &store.Project{
		ID:        tid("parity-project"),
		Name:      "parity-project",
		Slug:      "parity-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, project))
	// The agents' ancestry root is a member of the project, so they are
	// in good standing (ptone/scion#3433). The member binding also gives the
	// owner the active project access that the ancestry allow requires when
	// the owner messages its agents (ptone/scion#2141).
	ensureStandingRoot(t, s, project.ID, owner.ID)

	brokerID := tid("parity-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "parity-broker",
		Slug:   "parity-broker",
		Status: store.BrokerStatusOnline,
	}))

	senderAgent = &store.Agent{
		ID:              tid("parity-sender"),
		Name:            "parity-sender",
		Slug:            "parity-sender",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
		Ancestry:        []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, senderAgent))

	targetAgent = &store.Agent{
		ID:              tid("parity-target"),
		Name:            "parity-target",
		Slug:            "parity-target",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
		Ancestry:        []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, targetAgent))

	// Create DM conversation between the agents.
	dmKey, err := messages.DMConversationKey("agent", senderAgent.ID, "agent", targetAgent.ID)
	require.NoError(t, err)
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)
	convID = conv.ID

	dispatcher = &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	// Set up a spy broker bus.
	spyBus = &spyBrokerBus{}
	inproc := eventbus.NewInProcessEventBus(slog.Default())
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inproc},
		{Name: "spy", Bus: spyBus},
	}, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)

	return srv, s, project, senderAgent, targetAgent, convID, dispatcher, spyBus
}

// sendViaOutbound sends an agent DM through the outbound adapter using conv: addressing.
func sendViaOutbound(t *testing.T, srv *Server, sender *store.Agent, convID, msg string) *httptest.ResponseRecorder {
	t.Helper()

	reqBody, err := json.Marshal(OutboundMessageRequest{
		ConversationRef: "conv:" + convID,
		Msg:             msg,
		Type:            "instruction",
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+sender.ProjectID+"/agents/"+sender.ID+"/outbound-message",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: sender.ID},
		ProjectID: sender.ProjectID,
		Ancestry:  sender.Ancestry,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, sender.ID)
	return rr
}

// sendViaStructured sends an agent DM through the structured/inbound adapter.
func sendViaStructured(t *testing.T, srv *Server, sender, target *store.Agent, msg string) *httptest.ResponseRecorder {
	t.Helper()

	sm := &messages.StructuredMessage{
		Version:     messages.Version,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Type:        messages.TypeInstruction,
		Sender:      "agent:" + sender.Slug,
		SenderID:    sender.ID,
		Recipient:   "agent:" + target.Slug,
		RecipientID: target.ID,
		Msg:         msg,
	}

	reqBody, err := json.Marshal(MessageRequest{
		StructuredMessage: sm,
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+target.ProjectID+"/agents/"+target.ID+"/message",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: sender.ID},
		ProjectID: sender.ProjectID,
		Ancestry:  sender.Ancestry,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, target.ID)
	return rr
}

// captureSlogDefault swaps slog's default logger for a text handler writing
// to the returned buffer, and restores the previous default on test cleanup.
func captureSlogDefault(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

var errInjectedLookup = errors.New("injected lookup fault")

// assertReincarnateReplacedEdge asserts the reincarnation claim of agentID
// deactivated exactly the edge oldID, with cause reincarnate_replaced, under
// the operation ID its agent_reincarnate_claim audit records. It reads only.
func assertReincarnateReplacedEdge(t *testing.T, s store.Store, agentID, oldID string) {
	t.Helper()
	sum := auditSummary(t, s, mutationTypeAgentReincarnateClaim, agentID)
	opID, _ := sum["op_id"].(string)
	require.NotEmpty(t, opID, "the claim audit records an operation ID")
	replaced, err := s.GetDeactivatedDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, agentID,
		store.EdgeDeactivationReincarnateReplaced, opID)
	require.NoError(t, err)
	ids := make([]string, 0, len(replaced))
	for _, e := range replaced {
		ids = append(ids, e.ID)
	}
	assert.Equal(t, []string{oldID}, ids, "the replaced edge is deactivated under the claim's operation ID")
}

// assertEdgeKept asserts the reincarnation of agentID kept the edge old
// (got is the agent's single edge afterwards): same edge, delegator,
// provenance and ceiling; no edge deactivated with cause
// reincarnate_replaced; and the claim audit reports re_recorded=false and
// edges_replaced=0, the self-reincarnate shape.
func assertEdgeKept(t *testing.T, s store.Store, agentID string, old, got *store.DelegationEdge) {
	t.Helper()
	assert.Equal(t, old.ID, got.ID, "the edge is kept")
	assert.True(t, got.Active)
	assert.Equal(t, old.DelegatorType, got.DelegatorType)
	assert.Equal(t, old.DelegatorID, got.DelegatorID)
	assert.Equal(t, old.AuthorityProvenance, got.AuthorityProvenance)
	assert.Equal(t, old.Kind, got.Kind, "ceiling kind")
	sum := auditSummary(t, s, mutationTypeAgentReincarnateClaim, agentID)
	assert.Equal(t, false, sum["re_recorded"])
	assert.EqualValues(t, 0, sum["edges_replaced"])
	opID, _ := sum["op_id"].(string)
	require.NotEmpty(t, opID)
	replaced, err := s.GetDeactivatedDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, agentID,
		store.EdgeDeactivationReincarnateReplaced, opID)
	require.NoError(t, err)
	assert.Empty(t, replaced, "no edge is deactivated")
}

// assertCeilingFromSource asserts got is the ceiling sourceEffectCeiling
// derives for the requester: kind, version and permission IDs.
func assertCeilingFromSource(t *testing.T, srv *Server, requester Identity, got store.EffectCeiling) {
	t.Helper()
	want, _, err := srv.authzService.sourceEffectCeiling(context.Background(), requester)
	require.NoError(t, err)
	assert.Equal(t, want.Kind, got.Kind, "ceiling kind")
	assert.Equal(t, want.Version, got.Version, "ceiling version")
	assert.Equal(t, want.PermissionIDs, got.PermissionIDs, "ceiling permission IDs")
}

// seedAgentEdge records an active project-scoped edge delegating to agent,
// with recorded session provenance, and returns it.
func seedAgentEdge(t *testing.T, s store.Store, delegatorID string, agent *store.Agent) *store.DelegationEdge {
	t.Helper()
	ensureActiveUser(t, s, delegatorID)
	// The delegator is a project member, so the agent is in good standing
	// (ptone/scion#3433).
	ensureStandingRoot(t, s, agent.ProjectID, delegatorID)
	e := &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   delegatorID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    agent.ID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       agent.ProjectID,
		Role:          string(AgentRoleBaseline),
		Active:        true,
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion:    store.ProvenanceVersionV1,
			SourcePrincipalKind:  store.DelegationPrincipalUser,
			SourcePrincipalID:    delegatorID,
			SourceCredentialKind: store.SourceCredentialSession,
		},
		EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	require.NoError(t, s.CreateDelegationEdge(context.Background(), e))
	return e
}

// ensureActiveUser creates an active user with id unless one exists, so the
// delegator of a seeded edge is live for a restore.
func ensureActiveUser(t *testing.T, s store.Store, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.GetUser(ctx, id); err == nil {
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		require.NoError(t, err)
	}
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: id, Email: id + "@delegator.test", DisplayName: "Delegator",
		Role: store.UserRoleMember, Status: store.UserStatusActive,
	}))
}

// activeEdgeIDs returns the IDs of the agent's active delegation edges.
func activeEdgeIDs(t *testing.T, s store.Store, agentID string) []string {
	t.Helper()
	edges, err := s.GetDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, agentID)
	require.NoError(t, err)
	ids := make([]string, 0, len(edges))
	for _, e := range edges {
		ids = append(ids, e.ID)
	}
	return ids
}

// auditSummary decodes the AfterSummary of the single record of
// mutationType for agentID.
func auditSummary(t *testing.T, s store.Store, mutationType, agentID string) map[string]any {
	t.Helper()
	recs := agentAudits(t, s, mutationType, agentID)
	require.Len(t, recs, 1, "one %s record", mutationType)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(recs[0].AfterSummary), &m))
	return m
}

// softDeleteForTest soft-deletes agent through the delete engine.
func softDeleteForTest(t *testing.T, srv *Server, agentID string) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agentID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

func restoreForTest(t *testing.T, srv *Server, agentID string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agentID+"/restore", nil)
}

// assertNothingClaimed asserts agent is unchanged by a refused reincarnation:
// same state_version, no claim, no record, the original edge active and
// delegated by the original delegator, and no claim audit.
func assertNothingClaimed(t *testing.T, s store.Store, agent *store.Agent, edge *store.DelegationEdge) {
	t.Helper()
	got := mustGetAgent(t, s, agent.ID)
	assert.Equal(t, agent.StateVersion, got.StateVersion, "nothing is claimed")
	assert.Equal(t, store.ReincarnationStateNone, got.ReincarnationState)
	recs, err := s.ListAgentReincarnations(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Empty(t, recs, "no reincarnation record")
	edges, err := s.GetDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, agent.ID)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, edge.ID, edges[0].ID)
	assert.Equal(t, edge.DelegatorID, edges[0].DelegatorID)
	assert.Empty(t, agentAudits(t, s, mutationTypeAgentReincarnateClaim, agent.ID))
}

func readRuleSetup(t *testing.T, n int, callerOwns func(i int) bool) *readRuleFixture {
	t.Helper()
	f := &readRuleFixture{sortedListFixture: sortedListSetup(t)}
	ctx := context.Background()
	f.caller = &store.User{
		ID: tid("rr-caller"), Email: "rr-caller@test.com", DisplayName: "Caller",
		Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, f.store.CreateUser(ctx, f.caller))
	ensureHubMembership(ctx, f.store, f.caller.ID)
	grantProjectListOnly(t, f.store, f.caller.ID, f.project.ID, "rr-list-only")

	agents := f.createAgentsBulk(t, n, "rr", string(state.PhaseStopped), func(i int) string {
		switch {
		case callerOwns(i):
			return f.caller.ID
		case i%2 == 0:
			return f.member.ID
		default:
			return f.owner.ID
		}
	})
	for i, a := range agents {
		f.all = append(f.all, a.ID)
		if callerOwns(i) {
			f.readable = append(f.readable, a.ID)
		}
	}
	sort.Strings(f.all)
	sort.Strings(f.readable)
	return f
}

func markAgentSoftDeleted(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	ctx := context.Background()
	a, err := s.GetAgent(ctx, agentID)
	require.NoError(t, err)
	a.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(ctx, a))
}

// createFillerAgents adds n agents to projectID, all created after any agent
// already in the project, so earlier agents sort after them in store order.
func createFillerAgents(t *testing.T, s store.Store, projectID string, ancestry []string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		slug := fmt.Sprintf("filler-%04d", i)
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{
			ID:          tid(projectID + "-" + slug),
			Name:        slug,
			Slug:        slug,
			ProjectID:   projectID,
			MessageMode: store.MessageModeProject,
			Ancestry:    ancestry,
		}))
	}
}

const mentionBystanderSlug = "amf-bystander"

// enableConversationEnvelopeForMentionTests turns on the consolidated conversation
// envelope switch (writeDenyEnabled) so DeliveryText gets rendered — needed
// to assert on the rendered envelope's content. An empty fake settings store
// has no "messaging" section, which ConversationEnvelopeSwitch documents as
// defaulting to ON, but this makes the ON state explicit and independent of
// that default.
func enableConversationEnvelopeForMentionTests(t *testing.T, srv *Server) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("messaging", json.RawMessage(`{}`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("failed to refresh operational settings: %v", err)
	}
	srv.SetOperationalSettings(ops)
}

// mentionFanoutSetup reuses paritySetup (two same-project agents + DM conversation,
// recording dispatcher, broker proxy) and adds a third same-project agent,
// the bystander, which is only ever @mentioned in message bodies.
func mentionFanoutSetup(t *testing.T) (srv *Server, s store.Store, project *store.Project,
	sender, target, bystander *store.Agent, dmConvID string, dispatcher *recordingDispatcher) {
	t.Helper()
	srv, s, project, sender, target, dmConvID, dispatcher, _ = paritySetup(t)
	bystander = &store.Agent{
		ID:              tid("amf-bystander"),
		Name:            mentionBystanderSlug,
		Slug:            mentionBystanderSlug,
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: target.RuntimeBrokerID,
		MessageMode:     store.MessageModeProject,
		Ancestry:        sender.Ancestry,
	}
	require.NoError(t, s.CreateAgent(context.Background(), bystander))
	return
}

func dispatchesTo(d *recordingDispatcher, agentID string) []dispatchCall {
	var out []dispatchCall
	for _, c := range d.getCalls() {
		if c.Agent != nil && c.Agent.ID == agentID {
			out = append(out, c)
		}
	}
	return out
}

func agentCtx(ctx context.Context, a *store.Agent) context.Context {
	return contextWithIdentity(ctx, &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: a.ID},
		ProjectID: a.ProjectID,
		Ancestry:  a.Ancestry,
	}})
}

// wireWebBrokerForMentionTests swaps in a production-shaped broker proxy with a "web"
// spoke and the project's user-message subscription, so agent→user and
// agent→group-conversation sends take the deliveryUserBroker path end to end
// (handler → PublishUserMessage → MessageBrokerProxy.deliverToUser). The
// dispatcher is the same recordingDispatcher, so any mention fan-out would
// be observed.
func wireWebBrokerForMentionTests(t *testing.T, srv *Server, s store.Store, projectID string, dispatcher *recordingDispatcher) {
	t.Helper()
	if srv.webChatStore == nil {
		dbProvider, ok := s.(interface{ DB() *sql.DB })
		require.True(t, ok)
		wcs := NewWebChatStore(dbProvider.DB(), "sqlite3")
		require.NoError(t, wcs.Init())
		srv.SetWebChatStore(wcs)
	}
	if old := srv.GetMessageBrokerProxy(); old != nil {
		old.Stop()
	}
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: eventbus.NewInProcessEventBus(slog.Default())},
		{Name: "web", Bus: nullSpokeEventBus{}},
	}, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)
	srv.mu.RLock()
	proxy.webChatStore = srv.webChatStore
	srv.mu.RUnlock()
	proxy.subscribeProjectUserMessages(projectID)
}

// sortedFieldKeys returns m's keys, sorted.
func sortedFieldKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// runTokenGenerator signs real agent tokens. A non-empty jtiHash replaces
// the credential's hash, so a test can make the credential insert collide
// with an existing row.
type runTokenGenerator struct {
	svc     *AgentTokenService
	jtiHash string
}

// testRunScopeChecker builds a checker in mode (enforce included, which
// configuration cannot select).
func testRunScopeChecker(mode agentRunScopeMode, legacyUntil time.Time, agents agentRunReader) *agentRunScopeChecker {
	c := newAgentRunScopeChecker(AgentRunScope{mode: mode, legacyUntil: legacyUntil}, agents, slog.New(&captureHandler{}))
	if c == nil {
		panic("testRunScopeChecker: mode off has no checker")
	}
	return c
}

// runScopeAgent stores an agent whose current run is runID.
func runScopeAgent(t *testing.T, s store.Store, projectID, name, runID string) *store.Agent {
	t.Helper()
	ctx := context.Background()
	agent := createCredTestAgent(t, s, tid("run-scope-"+name), projectID, tid("user-cred-test"))
	if runID != "" {
		_, err := s.SetAgentRunID(ctx, agent.ID, runID, nil)
		require.NoError(t, err)
	}
	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	return got
}

func recordRun(run string) credRecord { return func() *string { return &run } }

// signRunToken signs a full-role token for agent naming runID and stores
// the credential row record asks for.
func signRunToken(t *testing.T, srv *Server, s store.Store, agent *store.Agent, runID string, record credRecord) string {
	t.Helper()
	tok, cred, err := srv.agentTokenService.SignAgentToken(AgentTokenGrant{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Scopes: ScopesForRole(AgentRoleFull), Ancestry: agent.Ancestry,
	}, runID)
	require.NoError(t, err)
	if run := record(); run != nil {
		cred.RunID = *run
		require.NoError(t, s.CreateAgentCredential(context.Background(), cred))
	}
	return tok
}

// agentRequest serves one agent-token request; runHeader, when set, is
// sent as AgentRunIDHeader.
func agentRequest(t *testing.T, h http.Handler, method, path, token, runHeader string) *httptest.ResponseRecorder {
	t.Helper()
	var body *bytes.Reader
	if method == http.MethodPost {
		body = bytes.NewReader([]byte("{}"))
	} else {
		body = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("X-Scion-Agent-Token", token)
	if runHeader != "" {
		req.Header.Set(AgentRunIDHeader, runHeader)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// rawResponse is a response's status, headers and body.
type rawResponse struct {
	Status int
	Header http.Header
	Body   string
}

func rawOf(rec *httptest.ResponseRecorder) rawResponse {
	return rawResponse{Status: rec.Code, Header: rec.Header().Clone(), Body: rec.Body.String()}
}

var errCredentialCreateForTest = errors.New("credential insert refused for testing")

// funcKey names a function declaration as Recv.Name (or Name).
func funcKey(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// chainFromAgentCreate reports whether the call/selector chain e starts
// from an Agent create builder (….Agent.Create() or ….Agent.CreateBulk(…)).
func chainFromAgentCreate(e ast.Expr) bool {
	for {
		switch x := e.(type) {
		case *ast.CallExpr:
			e = x.Fun
		case *ast.SelectorExpr:
			if x.Sel.Name == "Create" || x.Sel.Name == "CreateBulk" {
				if inner, ok := x.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "Agent" {
					return true
				}
			}
			e = x.X
		default:
			return false
		}
	}
}

// reissueFixture is a mint fixture holding a three-level chain in one
// project: user -> R (principal, session) -> P (bounded) -> A (bounded).
// P and A carry a bounded ceiling recorded before the artifact
// permissions existed, so neither is issued the artifact scopes.
type reissueFixture struct {
	*mintFixture
	// faults is installed as the server's store (and the authorization
	// service's) when the fixture is built; it is transparent until a test
	// arms it.
	faults              *reissueFaultStore
	root, parent, child *store.Agent
	operator            reissueOperator
}

// legacyBoundedCeiling is a bounded ceiling over the full role's coverage
// without the artifact permissions: the shape of a ceiling frozen before
// those permissions were added.
func legacyBoundedCeiling() store.EffectCeiling {
	var scopes []AgentTokenScope
	for _, s := range ScopesForRole(AgentRoleFull) {
		if !ceilingOptionalRoleScopes[s] {
			scopes = append(scopes, s)
		}
	}
	return boundedCeiling(agentScopeCoverage(scopes)...)
}

func newReissueFixture(t *testing.T, name string, topRole string) *reissueFixture {
	t.Helper()
	f, faults := newReissueMintFixture(t, name)
	setBackfillCompleted(t, f.store)
	// The test server enables dev auth, which raises every mint to the
	// full role; the re-issue is tested against production minting.
	f.srv.authzService.mintDevAuthOverride = false
	if topRole != store.ProjectRoleOwner {
		// Replace the owner with a user holding topRole.
		userID := tid(name + "-top")
		createDCUser(t, f.store, userID, name+"-top@test.com", f.projectID, topRole)
		f.userID = userID
	}
	r := f.agent(t, name+"-root", AgentRoleFull, state.PhaseRunning)
	f.edge(t, store.DelegationPrincipalUser, f.userID, r.ID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		store.AuthorityProvenance{ProvenanceVersion: 1, SourcePrincipalKind: store.DelegationPrincipalUser, SourcePrincipalID: f.userID, SourceCredentialKind: store.SourceCredentialSession})
	p := f.childAgent(t, name+"-parent", r, AgentRoleFull)
	a := f.childAgent(t, name+"-child", p, AgentRoleFull)
	return &reissueFixture{
		mintFixture: f, faults: faults, root: r, parent: p, child: a,
		operator: reissueOperator{UserID: DevUserID, CredentialKind: store.InitiatorCredentialKindSession},
	}
}

// newReissueMintFixture is newMintFixture with a reissueFaultStore installed
// on the server (and its authorization service) right after the server is
// built, before any audited setup (installStoreFault).
func newReissueMintFixture(t *testing.T, name string) (*mintFixture, *reissueFaultStore) {
	t.Helper()
	srv, s := testServer(t)
	faults, sw := installStoreFault(t, srv, func(inner store.Store, fault *storeFaultSwitch) *reissueFaultStore {
		return &reissueFaultStore{Store: inner, fault: fault}
	})
	faults.sw = sw
	srv.authzService.store = faults
	project := setupProjectWithBroker(t, s, name, name)
	client := &mintBrokerClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}
	disp := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	disp.SetTokenGenerator(srv)
	srv.SetDispatcher(disp)
	userID := tid(name + "-user")
	createDCUser(t, s, userID, name+"-user@test.com", project.ID, store.ProjectRoleOwner)
	return &mintFixture{
		srv: srv, store: s, disp: disp, client: client,
		projectID: project.ID, brokerID: tid("broker-" + name), userID: userID,
	}, faults
}

func reissueAudits(t *testing.T, s store.Store, agentID, mutationType string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		MutationType: mutationType, TargetType: "agent", TargetID: agentID,
	})
	require.NoError(t, err)
	return recs
}

func decodeReissueSummary(t *testing.T, rec *store.MutationAuditRecord) reissueAuditSummary {
	t.Helper()
	var s reissueAuditSummary
	require.NoError(t, json.Unmarshal([]byte(rec.AfterSummary), &s))
	return s
}

func artifactScopeStrings() []string {
	return []string{string(ScopeProjectArtifactRead), string(ScopeProjectArtifactWrite)}
}

func reissueSorted(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

// grantSuperAdmin binds the system super-admin role to userID, as the
// system reconciler does (only it may create super-admin bindings).
func grantSuperAdmin(t *testing.T, s store.Store, userID string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID,
		ScopeType: store.RoleScopeSystem, CreatedBy: store.SystemReconcileCreatedBy,
	})
	if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		require.NoError(t, err)
	}
}

// userReissueFixture is a mint fixture with production minting and the
// backfill marker set.
func newUserReissueFixture(t *testing.T, name string) *reissueFixture {
	t.Helper()
	f, faults := newReissueMintFixture(t, name)
	setBackfillCompleted(t, f.store)
	f.srv.authzService.mintDevAuthOverride = false
	return &reissueFixture{
		mintFixture: f,
		faults:      faults,
		operator:    reissueOperator{UserID: DevUserID, CredentialKind: store.InitiatorCredentialKindSession},
	}
}

func sessionProv(userID string) store.AuthorityProvenance {
	return store.AuthorityProvenance{
		ProvenanceVersion: 1, SourcePrincipalKind: store.DelegationPrincipalUser,
		SourcePrincipalID: userID, SourceCredentialKind: store.SourceCredentialSession,
	}
}

func withCursor(query, cursor string) string {
	return query + "&cursor=" + url.QueryEscape(cursor)
}

// globalCallSpyStore counts every store read the global sorted path can make.
type globalCallSpyStore struct {
	store.Store
	mu    sync.Mutex
	calls []string
}

// setRawAgentTimes overwrites an agent's stored time columns with literal
// text, for building exact ties that CreateAgent's own clock cannot produce.
// An empty lastActivity stores NULL.
func setRawAgentTimes(t *testing.T, s store.Store, id, created, updated, lastActivity string) {
	t.Helper()
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store must expose DB()")
	var lae any
	if lastActivity != "" {
		lae = lastActivity
	}
	_, err := dbProvider.DB().ExecContext(context.Background(),
		"UPDATE agents SET created = ?, updated = ?, last_activity_event = ? WHERE id = ?",
		created, updated, lae, id)
	require.NoError(t, err)
}

// noScopeUser creates a user with no hub membership and no project bindings,
// whose list scope resolves to None.
func noScopeUser(t *testing.T, s store.Store) *store.User {
	t.Helper()
	user := &store.User{
		ID: tid("sg-none-v"), Email: "sg-none-v@test.com",
		DisplayName: "No Scope User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(context.Background(), user))
	return user
}

// listAgentsUnauthenticated calls the global list handler with no identity
// in the request context, reaching its unauthenticated short-circuit.
func listAgentsUnauthenticated(srv *Server, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents?"+query, nil)
	rec := httptest.NewRecorder()
	srv.listAgents(rec, req)
	return rec
}

// globalSortedFixture is a hub-admin bound project-admin on the one project
// every test agent lives in, for the global endpoint's sorted-mode tests.
// Every row is then in scope and fully readable with every action granted,
// and each page costs 9 decisions per returned row plus 4 scope-capability
// decisions, all through real authzService decisions.
//
// The caller is deliberately store.UserRoleMember + SystemRoleHubAdmin, not
// UserRoleAdmin + SystemRoleSuperAdmin: authorizeAgentMessage's super-admin
// bypass (authorize_message.go) keys on the flat User.Role being "admin" and
// short-circuits ComputeMessageability to zero decisions. For the same
// reason the project role is project-admin, not project-owner:
// authorizeUserToAgent's project-owner bypass would also zero out
// messageability's decision cost.
type globalSortedFixture struct {
	srv     *Server
	store   store.Store
	admin   *store.User
	project *store.Project
}

func globalSortedSetup(t *testing.T) *globalSortedFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	adminID := tid("sg-admin")
	createTestUserWithRole(t, s, adminID, "sg-admin@test.com", store.UserRoleMember, store.SystemRoleHubAdmin)
	admin, err := s.GetUser(ctx, adminID)
	require.NoError(t, err)

	project := &store.Project{
		ID: tid("sg-project"), Name: "Sorted Global Project", Slug: "sg-project",
		OwnerID: adminID, CreatedBy: adminID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	createTestUserWithProjectRole(t, s, adminID, "sg-admin@test.com", project.ID, store.ProjectRoleAdmin)

	return &globalSortedFixture{srv: srv, store: s, admin: admin, project: project}
}

func decodeErrorMessage(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &e))
	return e.Error.Message
}

// assertResponsesEqualIgnoringServerTime compares two ListAgentsResponse
// JSON bodies for equality except the serverTime field.
func assertResponsesEqualIgnoringServerTime(t *testing.T, a, b []byte) {
	t.Helper()
	var am, bm map[string]interface{}
	require.NoError(t, json.Unmarshal(a, &am))
	require.NoError(t, json.Unmarshal(b, &bm))
	delete(am, "serverTime")
	delete(bm, "serverTime")
	aj, err := json.Marshal(am)
	require.NoError(t, err)
	bj, err := json.Marshal(bm)
	require.NoError(t, err)
	assert.JSONEq(t, string(aj), string(bj))
}

// grantProjectListOnly binds userID to a project-scoped role carrying only
// "agent.list" — no "agent.read" — so the project's agent.list gate passes
// but no agent is readable except through a resource-level relationship
// grant (ownership), independent of any role permission (pkg/hub/
// authz_relationship_rules.go; confirmed unconditional-of-role-bindings by
// TestAuthz_OwnerBypass, pkg/hub/authz_test.go). This is how a single
// caller, one project, one role, reads exactly its owned subset of agents —
// the project endpoint's agent.read is otherwise all-or-nothing per
// (principal, project) via role bindings and has no other per-resource
// visibility narrowing (needed to get a readable count R < n).
func grantProjectListOnly(t *testing.T, s store.Store, userID, projectID, roleName string) {
	t.Helper()
	rd := createTestRoleDefinition(t, s, roleName, store.RoleScopeProject, []string{"agent.list"})
	_, err := s.CreateRoleBinding(context.Background(), &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// referenceOrderIDs returns the authorized, filtered set's IDs in the
// sorted-mode total order for (sort, dir), independent of any HTTP
// pagination: a direct, single, unpaged store.Store.ListAgentMembers call.
func referenceOrderIDs(t *testing.T, s store.Store, projectID, sortKey, dir string, max int) []string {
	t.Helper()
	members, err := s.ListAgentMembers(context.Background(), store.AgentFilter{ProjectID: projectID}, sortKey, dir, max)
	require.NoError(t, err)
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m.ID
	}
	return ids
}

// walkAllPagesIDs drives the real HTTP endpoint page by page (sort=updated)
// and returns the concatenated agent IDs, as f.owner.
func walkAllPagesIDs(t *testing.T, f *sortedListFixture, dir string, limit int) []string {
	t.Helper()
	return walkAllPagesIDsAs(t, f, f.owner, dir, limit)
}

// walkAllPagesIDsAs is walkAllPagesIDs for a caller other than f.owner: the
// R<n walk needs a caller with a strict readable subset, via
// grantProjectListOnly.
func walkAllPagesIDsAs(t *testing.T, f *sortedListFixture, user *store.User, dir string, limit int) []string {
	t.Helper()
	var ids []string
	cursor := ""
	for pages := 0; ; pages++ {
		require.Lessf(t, pages, 5000, "walk did not terminate within a sane number of pages")
		q := fmt.Sprintf("sort=updated&dir=%s&limit=%d", dir, limit)
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		rec := doRequestAsUser(t, f.srv, user, http.MethodGet, f.listPath(q), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		resp := mustDecodeListAgentsResponse(t, rec.Body)
		for _, a := range resp.Agents {
			ids = append(ids, a.ID)
		}
		if resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
	}
	return ids
}

// racingAllMembersStore mutates every candidate's Labels (via the real
// store, bypassing the read path) the first time ListAgentMembers is
// called, simulating every page item racing between the member read and the
// full-row read -- the n-items generalization of
// mutatingAfterMembersStore, which only races one row.
type racingAllMembersStore struct {
	store.Store
	fault *storeFaultSwitch // nil: always active
	once  sync.Once
}

// ownerChangingAfterMembersStore mutates an agent's OwnerID (via the real
// store) the first time ListAgentMembers is called, simulating a write
// landing between the member read and the full-row read.
type ownerChangingAfterMembersStore struct {
	store.Store
	fault      *storeFaultSwitch // nil: always active
	once       sync.Once
	agentID    string
	newOwnerID string
}

// newFieldMutatingAfterMembersStore is the installStoreFault wrap func for
// fieldMutatingAfterMembersStore. Set agentID and mutate before arming.
func newFieldMutatingAfterMembersStore(inner store.Store, fault *storeFaultSwitch) *fieldMutatingAfterMembersStore {
	return &fieldMutatingAfterMembersStore{Store: inner, fault: fault}
}

// sortedListFixture builds a project with an owner (full capabilities) and a
// plain member (read-only: the read-pass and race tests need a
// caller for whom some agents are unreadable), for the sorted-mode project
// list tests.
type sortedListFixture struct {
	srv     *Server
	store   store.Store
	project *store.Project
	owner   *store.User
	member  *store.User // project member, no elevated role -- see grantMemberReadOnly
}

func sortedListSetup(t *testing.T) *sortedListFixture {
	t.Helper()
	srv, s := testServer(t)
	return sortedListSetupOn(t, srv, s)
}

// sortedListSetupWithFault is sortedListSetup with a switch-gated store
// wrapper (see installStoreFault) installed on the server BEFORE the
// fixture's audited setup (seedProjectCreatorMembership emits a mutation
// audit whose goroutine reads srv.store). Tests call fault.Arm() where they
// used to assign f.srv.store, which would race that goroutine
// (ptone/scion#3184).
func sortedListSetupWithFault[W store.Store](t *testing.T, wrap func(inner store.Store, fault *storeFaultSwitch) W) (*sortedListFixture, W, *storeFaultSwitch) {
	t.Helper()
	srv, s, wrapped, fault := testServerWithStoreFault(t, wrap)
	return sortedListSetupOn(t, srv, s), wrapped, fault
}

func mustDecodeListAgentsResponse(t *testing.T, rec interface{ Bytes() []byte }) ListAgentsResponse {
	t.Helper()
	var resp ListAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Bytes(), &resp))
	return resp
}

// newCountingAgentStore is the installStoreFault wrap func for
// countingAgentStore: it passes calls straight through, uncounted, until
// the switch is armed.
func newCountingAgentStore(inner store.Store, fault *storeFaultSwitch) *countingAgentStore {
	return &countingAgentStore{Store: inner, fault: fault}
}

// newRaceMembersStore returns an installStoreFault wrap func for a
// raceMembersStore over a countingAgentStore, both gated by the switch.
func newRaceMembersStore(memberCount int) func(store.Store, *storeFaultSwitch) *raceMembersStore {
	return func(inner store.Store, fault *storeFaultSwitch) *raceMembersStore {
		return &raceMembersStore{countingAgentStore: newCountingAgentStore(inner, fault), memberCount: memberCount}
	}
}

// newMutatingAfterMembersStore is the installStoreFault wrap func for
// mutatingAfterMembersStore. Set agentID and newLabels before arming.
func newMutatingAfterMembersStore(inner store.Store, fault *storeFaultSwitch) *mutatingAfterMembersStore {
	return &mutatingAfterMembersStore{Store: inner, fault: fault}
}

// newDeletingAfterMembersStore is the installStoreFault wrap func for
// deletingAfterMembersStore. Set agentID before arming.
func newDeletingAfterMembersStore(inner store.Store, fault *storeFaultSwitch) *deletingAfterMembersStore {
	return &deletingAfterMembersStore{Store: inner, fault: fault}
}

// newReprojectingListAgentsStore is the installStoreFault wrap func for
// reprojectingListAgentsStore. Set agentID and newProjectID before arming.
func newReprojectingListAgentsStore(inner store.Store, fault *storeFaultSwitch) *reprojectingListAgentsStore {
	return &reprojectingListAgentsStore{Store: inner, fault: fault}
}

// rawBodyWithoutServerTime returns rec's raw response body with the
// serverTime value blanked out, for an exact byte-for-byte comparison
// against another response.
func rawBodyWithoutServerTime(rec *httptest.ResponseRecorder) []byte {
	return serverTimeJSONRe.ReplaceAll(rec.Body.Bytes(), []byte(`"serverTime":""`))
}

func requireStandingReason(t *testing.T, err error, reason string) {
	t.Helper()
	require.Error(t, err)
	require.ErrorIs(t, err, errAgentNotInStanding)
	assert.Equal(t, reason, standingReason(err))
}

// markEdgeBackfillComplete records the delegation edge backfill marker, after
// which every agent caller needs an active delegation edge.
func markEdgeBackfillComplete(t *testing.T, s store.Store) {
	t.Helper()
	_, err := s.UpsertHubSetting(context.Background(), "migration_delegation_edge_backfill_v1",
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	require.NoError(t, err)
}

// addProjectEdge records an active delegation edge in the project scope and
// returns its ID.
func addProjectEdge(t *testing.T, s store.Store, delegatorType, delegatorID, delegateID, projectID string) string {
	t.Helper()
	id := uuid.NewString()
	require.NoError(t, s.CreateDelegationEdge(context.Background(), &store.DelegationEdge{
		ID:            id,
		DelegatorType: delegatorType,
		DelegatorID:   delegatorID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    delegateID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       projectID,
		Role:          string(AgentRoleFull),
		Active:        true,
	}))
	return id
}

// revokeDelegateEdges deactivates every active delegation edge of the agent
// delegateID, attributed to a test op ID, and fails the test when there was
// none to deactivate.
func revokeDelegateEdges(t *testing.T, s store.Store, delegateID string) {
	t.Helper()
	n, err := s.DeactivateDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, delegateID,
		store.Deactivation{Cause: store.EdgeDeactivationAgentHardDelete, OpID: "test-revoke-" + uuid.NewString()})
	require.NoError(t, err)
	require.Positive(t, n, "an active edge to revoke")
}

// decodeTargetAPIError decodes an error response body.
func decodeTargetAPIError(t *testing.T, rec *httptest.ResponseRecorder) APIError {
	t.Helper()
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body: %s", rec.Body.String())
	return resp.Error
}

// requestAsIdentity serves a request through the route mux with identity in
// the context (as both the caller and, for users, the user identity).
func requestAsIdentity(t *testing.T, srv *Server, identity Identity, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	ctx := contextWithIdentity(req.Context(), identity)
	if user, ok := identity.(UserIdentity); ok {
		ctx = context.WithValue(ctx, userContextKey{}, user)
	}
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

func hubMemberUser(t *testing.T, s store.Store, id string) *store.User {
	t.Helper()
	u := &store.User{
		ID: tid(id), Email: id + "@target.test", DisplayName: id,
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(context.Background(), u))
	ensureHubMembership(context.Background(), s, u.ID)
	return u
}

func authUser(u *store.User) *AuthenticatedUser {
	return NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "api")
}

// syncRecorder is a flushable response writer safe for concurrent reads.
type syncRecorder struct {
	mu     sync.Mutex
	header http.Header
	body   bytes.Buffer
	code   int
}

// mintBrokerClient records reset-auth calls on top of the dispatcher mock.
type mintBrokerClient struct {
	*mockRuntimeBrokerClient
	resetAuthCalled bool
	resetAuthToken  string
}

// mintFixture is a test server whose dispatcher mints through the server
// against a recording broker client.
type mintFixture struct {
	srv       *Server
	store     store.Store
	disp      *HTTPAgentDispatcher
	client    *mintBrokerClient
	projectID string
	brokerID  string
	userID    string
}

func newMintFixture(t *testing.T, name string) *mintFixture {
	t.Helper()
	srv, s := testServer(t)
	project := setupProjectWithBroker(t, s, name, name)
	client := &mintBrokerClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}
	disp := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	disp.SetTokenGenerator(srv)
	srv.SetDispatcher(disp)
	userID := tid(name + "-user")
	createDCUser(t, s, userID, name+"-user@test.com", project.ID, store.ProjectRoleOwner)
	return &mintFixture{
		srv: srv, store: s, disp: disp, client: client,
		projectID: project.ID, brokerID: tid("broker-" + name), userID: userID,
	}
}

func refreshedToken(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body.Token
}

// issueDeniedAudits returns the agent_token_issue_denied records for agent.
func issueDeniedAudits(t *testing.T, s store.Store, agentID string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		MutationType: mutationTypeAgentTokenIssueDenied, TargetType: "agent", TargetID: agentID,
	})
	require.NoError(t, err)
	return recs
}

// assertIssueDeniedAudit asserts one record naming the site and cause, and
// no scope list.
func assertIssueDeniedAudit(t *testing.T, s store.Store, agentID string, site mintSite, cause string) {
	t.Helper()
	recs := issueDeniedAudits(t, s, agentID)
	require.Len(t, recs, 1)
	var summary map[string]string
	require.NoError(t, json.Unmarshal([]byte(recs[0].AfterSummary), &summary))
	assert.Equal(t, map[string]string{"site": string(site), "deny_cause": cause}, summary)
	assert.NotContains(t, recs[0].AfterSummary, "scope")
}

// assertCredentialUnrevoked checks that the credential seeded under jti is
// unchanged and active.
func assertCredentialUnrevoked(t *testing.T, s store.AgentCredentialStore, jti string, before *store.AgentCredential) {
	t.Helper()
	after := getTestAgentCredential(t, s, jti)
	assert.Nil(t, after.RevokedAt, "credential is not revoked")
	assert.Nil(t, after.RevokedBy, "no revoker recorded")
	assert.Nil(t, after.RevokeReason, "no revoke reason recorded")
	assert.Equal(t, before, after, "credential row is unchanged")
}

// devCreatedChild creates a child through DevAuthMiddleware with no
// dispatcher mint, and returns its stored record.
func devCreatedChild(t *testing.T, f *mintFixture, name string) *store.Agent {
	t.Helper()
	f.srv.SetDispatcher(nil)
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects/"+f.projectID+"/agents", CreateAgentRequest{Name: name})
	require.True(t, rec.Code == http.StatusCreated || rec.Code == http.StatusAccepted, "create: %d %s", rec.Code, rec.Body.String())
	f.srv.SetDispatcher(f.disp)
	child, err := f.store.GetAgentBySlug(context.Background(), f.projectID, name)
	require.NoError(t, err)
	edges := activeEdgesFor(t, f.store, child.ID)
	require.Len(t, edges, 1)
	require.Equal(t, store.SourceCredentialDevLocal, edges[0].SourceCredentialKind)
	child.RuntimeBrokerID = f.brokerID
	child.Phase = string(state.PhaseStopped)
	require.NoError(t, f.store.UpdateAgent(context.Background(), child))
	return child
}

func newTZDispatchFixture(t *testing.T, hubDefault string) *tzDispatchFixture {
	t.Helper()
	ctx := context.Background()
	s := createTestStore(t)
	broker := &store.RuntimeBroker{
		ID:       tid("tz-dispatch-broker-" + t.Name()),
		Name:     "tz-dispatch-broker",
		Slug:     "tz-dispatch-broker-" + tidSlugSafe(t.Name()),
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	f := &tzDispatchFixture{store: s, client: &mockRuntimeBrokerClient{}, hubDefault: hubDefault}
	f.d = NewHTTPAgentDispatcherWithClient(s, f.client, false, slog.Default())
	f.d.SetHubID(tzDispatchHubID)
	f.d.SetHubAgentDefaultsProvider(func() opsettings.AgentDefaultsSettings {
		return opsettings.AgentDefaultsSettings{DefaultTimezone: f.hubDefault}
	})
	f.agent = &store.Agent{
		ID:              tid("tz-dispatch-agent-" + t.Name()),
		Name:            "tz-dispatch-agent",
		Slug:            "tz-dispatch-agent",
		ProjectID:       tid("tz-dispatch-project"),
		OwnerID:         tid("tz-dispatch-user"),
		RuntimeBrokerID: broker.ID,
		AppliedConfig:   &store.AgentAppliedConfig{HarnessConfig: "claude"},
	}
	return f
}

// tzTestEnvVar seeds one TZ env var into the store.
func tzTestEnvVar(t *testing.T, s store.Store, v store.EnvVar) {
	t.Helper()
	v.ID = api.NewUUID()
	if v.Key == "" {
		v.Key = agentTZEnvKey
	}
	if v.InjectionMode == "" {
		v.InjectionMode = store.InjectionModeAlways
	}
	if _, err := s.UpsertEnvVar(context.Background(), &v); err != nil {
		t.Fatalf("seeding %s-scoped %s: %v", v.Scope, v.Key, err)
	}
}

func (f *compactFixture) globalBase() string { return "/api/v1/agents" }
func (f *compactFixture) projectBase() string {
	return "/api/v1/projects/" + f.project.ID + "/agents"
}

func (f *compactFixture) caller(name string) compactCaller {
	for _, c := range append(append([]compactCaller{}, f.callers...), f.shortCircuit...) {
		if c.name == name {
			return c
		}
	}
	panic("no caller " + name)
}

// agentIDAt is the id the fixture gives to project agent i.
func (f *compactFixture) agentIDAt(i int) string { return tid(fmt.Sprintf("cv-agent-%d", i)) }

// requestBothViews issues the same request without view and with
// view=compact, recording the decision audit records of each.
func (f *compactFixture) requestBothViews(t *testing.T, c compactCaller, base, query string) viewPair {
	t.Helper()
	var p viewPair
	em := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(em)
	p.full = c.do(t, withQuery(base, query))
	p.fullAudit = em.records
	em = &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(em)
	p.compact = c.do(t, withQuery(base, query, "view=compact"))
	p.compactAudit = em.records
	f.srv.authzService.SetDecisionAuditEmitter(nil)
	return p
}

// walkParity walks one mode to its last page in both views, asserting
// parity on every page, and returns the number of items compared and the
// status of the first page.
func (f *compactFixture) walkParity(t *testing.T, c compactCaller, endpoint, base, mode string) (int, int) {
	t.Helper()
	compared, firstStatus := 0, 0
	query := mode
	for page := 0; page < 20; page++ {
		label := fmt.Sprintf("%s %s %q page %d", c.name, endpoint, mode, page)
		p := f.requestBothViews(t, c, base, query)
		if page == 0 {
			firstStatus = p.full.Code
		}
		compared += assertCompactParity(t, label, p)
		if p.full.Code != http.StatusOK {
			return compared, firstStatus
		}
		next := mustDecodeListAgentsResponse(t, p.full.Body).NextCursor
		if next == "" {
			return compared, firstStatus
		}
		// Continuation pages carry the cursor and drop fit, which is not
		// valid together with a cursor.
		query = withCursor(dropParam(mode, "fit"), next)
		query = strings.TrimPrefix(query, "&")
	}
	t.Fatalf("%s %s %q: walk did not end", c.name, endpoint, mode)
	return compared, firstStatus
}

func (f *legacyFixture) adoptionPreview(t *testing.T, admin *store.User, body map[string]interface{}) delegationAdoptionPreviewResponse {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, admin, http.MethodPost, delegationAdoptionPath+"/previews", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp delegationAdoptionPreviewResponse
	decodeJSONBody(t, rec, &resp)
	return resp
}

func (f *legacyFixture) adoptionCommit(t *testing.T, admin *store.User, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAsUser(t, f.srv, admin, http.MethodPost, delegationAdoptionPath+"/commits", body)
}

func (f *legacyFixture) adoptionRecords(t *testing.T) []*store.DelegationAdoption {
	t.Helper()
	recs, _, err := f.store.ListDelegationAdoptions(context.Background(), store.DelegationAdoptionFilter{})
	require.NoError(t, err)
	return recs
}

func (f *legacyFixture) adoptionAudits(t *testing.T, mutationType string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := f.store.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: mutationType})
	require.NoError(t, err)
	return recs
}

func (f *legacyFixture) recordFor(t *testing.T, agentID string) *store.DelegationAdoption {
	t.Helper()
	var found *store.DelegationAdoption
	for _, r := range f.adoptionRecords(t) {
		if r.DelegateID == agentID && r.Status != store.DelegationAdoptionReverted {
			found = r
		}
	}
	require.NotNil(t, found)
	return found
}

// legacyRecords returns the adoption records of the fixture's legacy agent.
func (f *legacyFixture) legacyRecords(t *testing.T) []*store.DelegationAdoption {
	t.Helper()
	var out []*store.DelegationAdoption
	for _, r := range f.adoptionRecords(t) {
		if r.DelegateID == f.legacy.ID {
			out = append(out, r)
		}
	}
	return out
}

func (f *legacyFixture) adoptionStatus(t *testing.T, admin *store.User) delegationAdoptionStatusResponse {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, admin, http.MethodGet, delegationAdoptionPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var status delegationAdoptionStatusResponse
	decodeJSONBody(t, rec, &status)
	return status
}

func (f *legacyFixture) assignBody(slug string) map[string]interface{} {
	return map[string]interface{}{
		"name": slug,
		"gcp_identity": map[string]interface{}{
			"metadata_mode": store.GCPMetadataModeAssign, "service_account_id": f.sa.ID,
		},
	}
}

// assertGateUnrecorded asserts that the SA gate denies the token's agent at
// surface with the unrecorded-provenance message, and that the gate's
// CheckAccess denies with ceiling_unrecorded.
func (f *legacyFixture) assertGateUnrecorded(t *testing.T, token, surface string) {
	t.Helper()
	identity := f.agentIdentityFor(t, token)
	ctx := contextWithIdentity(context.Background(), identity)
	assertUnrecordedDeny(t, f.srv.authzService.CheckAccess(ctx, identity, gcpServiceAccountResource(f.sa), ActionAssign))

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPatch, "/api/v1/agents/"+identity.ID(), nil).WithContext(ctx)
	require.False(t, f.srv.authorizeSAAssignment(rec, r, f.sa, surface))
	assertSAGateUnrecordedDenied(t, rec)
}

// seedLegacyAgent stores an agent under parent (nil: under the fixture
// owner) with an unrecorded edge of role.
func (f *legacyFixture) seedLegacyAgent(t *testing.T, name string, parent *store.Agent, role AgentRole) *store.Agent {
	t.Helper()
	ancestry := []string{f.owner.ID}
	delegatorType, delegatorID := store.DelegationPrincipalUser, f.owner.ID
	if parent != nil {
		ancestry = append(append([]string{}, parent.Ancestry...), parent.ID)
		delegatorType, delegatorID = store.DelegationPrincipalAgent, parent.ID
	}
	a := f.storeAgent(t, name, ancestry, role)
	require.NoError(t, f.store.CreateDelegationEdge(context.Background(), &store.DelegationEdge{
		DelegatorType: delegatorType, DelegatorID: delegatorID,
		DelegateType: store.DelegationPrincipalAgent, DelegateID: a.ID,
		ScopeType: store.RoleScopeProject, ScopeID: f.proj.ID, Role: string(role), Active: true,
	}))
	return a
}

// storeAgent stores a live agent in the fixture project with ancestry
// (root user first) and applied role.
func (f *legacyFixture) storeAgent(t *testing.T, name string, ancestry []string, role AgentRole) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID: tid(name), Slug: tid(name), Name: name, ProjectID: f.proj.ID,
		Phase: "running", CreatedBy: ancestry[0], OwnerID: ancestry[0], Ancestry: ancestry,
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(role)},
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	return a
}

func (f *legacyFixture) defaultAssignSA(t *testing.T) {
	t.Helper()
	f.setProjectAnnotation(t, projectSettingDefaultGCPIdentityMode, store.GCPMetadataModeAssign)
	f.setProjectAnnotation(t, projectSettingDefaultGCPIdentitySAID, f.sa.ID)
}

// withAssignedSA gives a stored agent the fixture service account in assign
// mode, as legacy agents created under a project-default service account
// carry. The GCP actAs layer accepts an assignment of the caller's own
// account.
func (f *legacyFixture) withAssignedSA(t *testing.T, a *store.Agent) {
	t.Helper()
	ctx := context.Background()
	stored, err := f.store.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	stored.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeAssign,
		ServiceAccountID: f.sa.ID, ServiceAccountEmail: f.sa.Email, ProjectID: f.sa.ProjectID}
	require.NoError(t, f.store.UpdateAgent(ctx, stored))
}

// launchFixture wires a secret backend with a progeny secret of the owner,
// and reports whether the last launched child received it.
func (f *legacyFixture) progenyLaunch(t *testing.T) func(child *store.Agent) bool {
	t.Helper()
	ctx := context.Background()
	backend := secret.NewLocalBackend(f.store, "test-hub-id", "test-secret")
	f.srv.SetSecretBackend(backend)
	disp := f.srv.GetDispatcher().(*HTTPAgentDispatcher)
	disp.SetSecretBackend(backend)
	disp.SetAuthzService(f.srv.authzService)
	_, _, err := backend.Set(ctx, &secret.SetSecretInput{
		Name: "PROGENY_KEY", Value: "progeny-value", SecretType: store.SecretTypeEnvironment, Target: "PROGENY_KEY",
		Scope: store.ScopeUser, ScopeID: f.owner.ID, AllowProgeny: true, InjectionMode: store.InjectionModeAlways,
		CreatedBy: f.owner.ID, UpdatedBy: f.owner.ID,
	})
	require.NoError(t, err)
	return func(child *store.Agent) bool {
		req := f.client.lastCreateReq
		require.NotNil(t, req)
		require.Equal(t, child.ID, req.ID)
		if _, ok := req.ResolvedEnv["PROGENY_KEY"]; ok {
			return true
		}
		for _, s := range req.ResolvedSecrets {
			if s.Name == "PROGENY_KEY" {
				return true
			}
		}
		return false
	}
}

func (runIntentErrStore) SwapRunIntent(context.Context, string, store.RunIntent) (store.RunIntent, time.Time, error) {
	return "", time.Time{}, errors.New("injected run intent fault")
}

// ClaimAgentStart fails too: a create-and-start records its run intent with
// its start claim.
func (runIntentErrStore) ClaimAgentStart(context.Context, string, string, store.StartClaimKind, string, time.Duration) (store.StartClaim, error) {
	return store.StartClaim{}, errors.New("injected run intent fault")
}

// useFailingManagedBackend swaps in a managed-agent backend whose create
// fails, for the rest of the test.
func useFailingManagedBackend(t *testing.T) {
	t.Helper()
	managedBackendMu.Lock()
	prev := managedBackendInst
	managedBackendInst = failingManagedAgentBackend{}
	managedBackendMu.Unlock()
	t.Cleanup(func() {
		managedBackendMu.Lock()
		managedBackendInst = prev
		managedBackendMu.Unlock()
	})
}

func (s *createTxFaultStore) wrap(tx store.Store) *createTxFaultStore {
	c := *s
	c.Store = tx
	c.outerDeleteErr = nil
	return &c
}

func (s *createTxFaultStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error { return fn(s.wrap(tx)) })
}

// FinalizeAgentDeletion is the conditional compensation's transaction
// (ptone/scion#3557): its hook sees the same faults as WithTx, and
// outerDeleteErr fails it (it is a row delete outside WithTx).
func (s *createTxFaultStore) FinalizeAgentDeletion(ctx context.Context, id string, pred store.DeletionPredicate, mode store.DeletionFinalizeMode, set store.DeletionFields, hook store.DeletionFinalizeHook) (int, error) {
	if s.outerDeleteErr != nil {
		return 0, s.outerDeleteErr
	}
	var wrapped store.DeletionFinalizeHook
	if hook != nil {
		wrapped = func(ctx context.Context, tx store.Store, a *store.Agent, m store.DeletionFinalizeMode) error {
			return hook(ctx, s.wrap(tx), a, m)
		}
	}
	return s.Store.FinalizeAgentDeletion(ctx, id, pred, mode, set, wrapped)
}

func (s *createTxFaultStore) CreateMutationAudit(ctx context.Context, r *store.MutationAuditRecord) error {
	if s.auditErrFor != "" && r.MutationType == s.auditErrFor {
		return errors.New("injected mutation audit write fault")
	}
	return s.Store.CreateMutationAudit(ctx, r)
}

func (s *createTxFaultStore) DeleteAgent(ctx context.Context, id string) error {
	if s.outerDeleteErr != nil {
		return s.outerDeleteErr
	}
	return s.Store.DeleteAgent(ctx, id)
}

func (s *createTxFaultStore) CreateNotificationSubscription(ctx context.Context, sub *store.NotificationSubscription) error {
	if s.subErr != nil {
		return s.subErr
	}
	return s.Store.CreateNotificationSubscription(ctx, sub)
}

func (s *createTxFaultStore) DeactivateDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string, d store.Deactivation) (int, error) {
	if s.deactErr != nil {
		return 0, s.deactErr
	}
	return s.Store.DeactivateDelegationEdgesForDelegate(ctx, delegateType, delegateID, d)
}

// createWithRequestID issues the fixture's create with request metadata
// carrying requestID, as the request-log middleware installs it, and
// returns the response and the compensation-failure records logged while
// it ran.
func (f *uatCreateFixture) createWithRequestID(t *testing.T, requestID string, req CreateAgentRequest) (*httptest.ResponseRecorder, []compensationFailureLog) {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, f.path, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	user := authUser(f.creator)
	ctx := contextWithIdentity(r.Context(), user)
	ctx = context.WithValue(ctx, userContextKey{}, user)
	return serveWithRequestID(t, f.srv.mux, r.WithContext(ctx), requestID)
}

// withDispatcher installs a dispatcher that mints through the server
// against a recording broker client.
func (f *uatCreateFixture) withDispatcher(t *testing.T) *mintBrokerClient {
	t.Helper()
	client := &mintBrokerClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}
	disp := NewHTTPAgentDispatcherWithClient(f.store, client, false, slog.Default())
	disp.SetTokenGenerator(f.srv)
	f.srv.SetDispatcher(disp)
	return client
}

// uat returns a V1 UAT identity for the creator holding exactly selectors.
func (f *uatCreateFixture) uat(t *testing.T, selectors ...string) *ScopedUserIdentity {
	t.Helper()
	c := uatCeilingFromSelectors(t, selectors...)
	return NewScopedUserIdentityWithCeiling(authUser(f.creator), f.proj.ID, selectors, "uat-"+f.creator.ID,
		permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: c.PermissionIDs})
}

func (f *uatCreateFixture) create(t *testing.T, identity Identity, req CreateAgentRequest) *httptest.ResponseRecorder {
	t.Helper()
	return requestAsIdentity(t, f.srv, identity, http.MethodPost, f.path, req)
}

// setProjectAnnotation writes one project annotation.
func (f *uatCreateFixture) setProjectAnnotation(t *testing.T, key, value string) {
	t.Helper()
	ctx := context.Background()
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[key] = value
	require.NoError(t, f.store.UpdateProject(ctx, proj))
}

// createdAgent returns the stored agent for slug and its single active edge.
func (f *uatCreateFixture) createdAgent(t *testing.T, rec *httptest.ResponseRecorder, slug string) (*store.Agent, *store.DelegationEdge) {
	t.Helper()
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	agent, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, slug)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	edges := activeEdgesFor(t, f.store, agent.ID)
	require.Len(t, edges, 1)
	return agent, edges[0]
}

func (d *createRaceDispatcher) DispatchAgentCreateWithGather(ctx context.Context, a *store.Agent) (*CreateDispatchResult, error) {
	if d.hook != nil {
		d.hook(a)
	}
	return d.engineStubDispatcher.DispatchAgentCreateWithGather(ctx, a)
}

func (c *raceAsyncClient) DeleteAgent(ctx context.Context, _, _, _, _ string, _ DeleteAgentOptions) error {
	c.mu.Lock()
	fn := c.deleteFn
	c.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(ctx)
}

func (c *raceAsyncClient) setDeleteFn(fn func(ctx context.Context) error) {
	c.mu.Lock()
	c.deleteFn = fn
	c.mu.Unlock()
}

func (h *hookStorage) GenerateSignedURL(ctx context.Context, objectPath string, opts storage.SignedURLOptions) (*storage.SignedURL, error) {
	h.once.Do(h.hook)
	return h.mockStorage.GenerateSignedURL(ctx, objectPath, opts)
}

func (f *fakeMintingTokenGenerator) GenerateAgentToken(agentID, projectID string, ancestry []string, role AgentRole, additionalScopes []AgentTokenScope) (string, error) {
	if f.failWith != nil {
		return "", f.failWith
	}
	token, cred, err := f.SignAgentToken(AgentTokenGrant{AgentID: agentID, ProjectID: projectID, Ancestry: ancestry}, "")
	if err != nil {
		return "", err
	}
	if err := f.store.CreateAgentCredential(context.Background(), cred); err != nil {
		return "", err
	}
	return token, nil
}

// AuthorizeAgentToken is the entry point every dispatcher mint site calls.
func (f *fakeMintingTokenGenerator) AuthorizeAgentToken(_ context.Context, agent *store.Agent) (AgentTokenGrant, error) {
	if f.failWith != nil {
		return AgentTokenGrant{}, f.failWith
	}
	return AgentTokenGrant{AgentID: agent.ID, ProjectID: agent.ProjectID, Ancestry: agent.Ancestry}, nil
}

func (f *fakeMintingTokenGenerator) SignAgentToken(grant AgentTokenGrant, runID string) (string, *store.AgentCredential, error) {
	jti := fmt.Sprintf("test-jti-%s-%d", grant.AgentID, len(f.jtis)+1)
	f.jtis = append(f.jtis, jti)
	now := time.Now()
	cred := &store.AgentCredential{
		AgentID:      grant.AgentID,
		ProjectID:    grant.ProjectID,
		TokenJTIHash: hashJTI(jti),
		RunID:        runID,
		IssuedAt:     now,
		ExpiresAt:    now.Add(10 * time.Hour),
	}
	return "fake-agent-jwt-" + jti, cred, nil
}

// lastJTI returns the most recently minted jti, for a test that only expects
// a single GenerateAgentToken call.
func (f *fakeMintingTokenGenerator) lastJTI() string {
	return f.jtis[len(f.jtis)-1]
}

func (s *revokeFailingCredentialStore) RevokeAgentCredentialsByAgent(ctx context.Context, agentID, revokedBy, reason string) (int, error) {
	return 0, s.failWith
}

func (h *engineHookStore) UpdateAgentDeletion(ctx context.Context, id string, pred store.DeletionPredicate, set store.DeletionFields) (int, error) {
	h.mu.Lock()
	if h.missClaims && set.BumpClaim {
		h.claimMisses++
		h.mu.Unlock()
		return 0, nil
	}
	if obs := h.onDeletionWrite; obs != nil {
		h.mu.Unlock()
		obs(pred)
		h.mu.Lock()
	}
	match := h.failDeletionWrite
	if match != nil && match(set) {
		h.failDeletionWrite = nil
		h.mu.Unlock()
		return 0, errInjectedDeletionWrite
	}
	after := h.afterDeletionWrite
	h.mu.Unlock()
	n, err := h.Store.UpdateAgentDeletion(ctx, id, pred, set)
	if after != nil && err == nil {
		after(pred, n)
	}
	return n, err
}

func (h *engineHookStore) setFailDeletionWrite(fn func(set store.DeletionFields) bool) {
	h.mu.Lock()
	h.failDeletionWrite = fn
	h.mu.Unlock()
}

func (h *engineHookStore) RevokeAgentCredentialsByAgent(ctx context.Context, agentID, by, reason string) (int, error) {
	h.mu.Lock()
	h.revokeCalls++
	h.revokeCtxErrs = append(h.revokeCtxErrs, ctx.Err())
	err := h.revokeErr
	h.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return h.Store.RevokeAgentCredentialsByAgent(ctx, agentID, by, reason)
}

func (h *engineHookStore) HasOutstandingBrokerDispatch(ctx context.Context, agentID, op string) (bool, error) {
	h.mu.Lock()
	hook := h.onHasOutstanding
	h.onHasOutstanding = nil
	h.mu.Unlock()
	if hook != nil {
		hook()
	}
	return h.Store.HasOutstandingBrokerDispatch(ctx, agentID, op)
}

func (h *engineHookStore) setRevokeErr(err error) {
	h.mu.Lock()
	h.revokeErr = err
	h.mu.Unlock()
}

func (f *deferredDeleteFixture) del(t *testing.T, query string) *deleteResult {
	t.Helper()
	r := waitDelete(t, deleteAsync(t, f.srv, "/api/v1/agents/"+f.agent.ID+query, nil), 10*time.Second)
	return &r
}

// pendingDeleteIntents lists the agent's outstanding delete intents.
func (f *deferredDeleteFixture) pendingDeleteIntents(t *testing.T) []store.BrokerDispatch {
	t.Helper()
	all, err := f.store.ListPendingDispatch(context.Background(), f.agent.RuntimeBrokerID)
	require.NoError(t, err)
	var out []store.BrokerDispatch
	for _, d := range all {
		if d.AgentID == f.agent.ID && d.Op == brokerDispatchOpDelete {
			out = append(out, d)
		}
	}
	return out
}

// drainOK runs the owning node's drain with the broker now answering the
// delete directly.
func (f *deferredDeleteFixture) drainOK(t *testing.T) {
	t.Helper()
	f.client.returnErr = nil
	f.srv.drainBrokerDispatch(context.Background(), f.agent.RuntimeBrokerID, nil)
}

func (f *deferredDeleteFixture) revokes() int {
	f.hooks.mu.Lock()
	defer f.hooks.mu.Unlock()
	return f.hooks.revokeCalls
}

func (p *deleteRecordingPublisher) PublishAgentStatus(ctx context.Context, a *store.Agent) {
	p.mu.Lock()
	p.events = append(p.events, recordedAgentEvent{
		kind: "status", phase: a.Phase, activity: a.Activity,
		deletion: store.ComputeAgentDeletion(a, time.Now()),
	})
	p.mu.Unlock()
	p.EventPublisher.PublishAgentStatus(ctx, a)
}

func (p *deleteRecordingPublisher) PublishAgentDeleted(ctx context.Context, agentID, projectID string) {
	p.mu.Lock()
	p.events = append(p.events, recordedAgentEvent{kind: "deleted"})
	p.mu.Unlock()
	p.EventPublisher.PublishAgentDeleted(ctx, agentID, projectID)
}

func (p *deleteRecordingPublisher) snapshot() []recordedAgentEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]recordedAgentEvent(nil), p.events...)
}

func (p *deleteRecordingPublisher) count(kind string) int {
	n := 0
	for _, e := range p.snapshot() {
		if e.kind == kind {
			n++
		}
	}
	return n
}

func (d *engineStubDispatcher) DispatchAgentDelete(ctx context.Context, a *store.Agent, _, _, soft bool, _ time.Time) error {
	d.mu.Lock()
	d.calls++
	d.softArg = append(d.softArg, soft)
	fn := d.fn
	d.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(ctx, a)
}

func (d *engineStubDispatcher) DispatchAgentStart(_ context.Context, a *store.Agent, _ string, _ bool) error {
	a.Phase = string(state.PhaseRunning)
	return nil
}

func (d *engineStubDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error { return nil }

func (d *engineStubDispatcher) setFn(fn func(ctx context.Context, a *store.Agent) error) {
	d.mu.Lock()
	d.fn = fn
	d.mu.Unlock()
}

func (d *engineStubDispatcher) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *deleteGuardDispatcher) DispatchAgentStart(_ context.Context, agent *store.Agent, _ string, _ bool) error {
	d.starts++
	agent.Phase = string(state.PhaseRunning)
	return nil
}

func (d *deleteGuardDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error {
	d.stops++
	return nil
}

func (d *dispatchErrStore) HasOutstandingBrokerDispatch(context.Context, string, string) (bool, error) {
	return false, errors.New("boom")
}

func (g runTokenGenerator) AuthorizeAgentToken(_ context.Context, agent *store.Agent) (AgentTokenGrant, error) {
	return AgentTokenGrant{AgentID: agent.ID, ProjectID: agent.ProjectID}, nil
}

func (g runTokenGenerator) SignAgentToken(grant AgentTokenGrant, runID string) (string, *store.AgentCredential, error) {
	token, cred, err := g.svc.SignAgentToken(grant, runID)
	if err == nil && g.jtiHash != "" {
		cred.TokenJTIHash = g.jtiHash
	}
	return token, cred, err
}

func (f *reissueFixture) bulk(t *testing.T, dryRun bool) *ScopeReissueBulkResponse {
	t.Helper()
	resp, err := f.srv.runScopeReissueBulk(context.Background(), f.operator, dryRun)
	require.NoError(t, err)
	return resp
}

func (f *reissueFixture) adminSession(t *testing.T) UserIdentity {
	t.Helper()
	id := tid(f.projectID + "-admin")
	if _, err := f.store.GetUser(context.Background(), id); err != nil {
		require.NoError(t, f.store.CreateUser(context.Background(), &store.User{
			ID: id, Email: id + "@test.com", DisplayName: "Admin", Role: "admin", Status: "active",
		}))
	}
	grantSuperAdmin(t, f.store, id)
	return NewAuthenticatedUser(id, id+"@test.com", "Admin", "admin", "")
}

func (f *reissueFixture) reload(t *testing.T, a *store.Agent) *store.Agent {
	t.Helper()
	got, err := f.store.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	return got
}

func (f *reissueFixture) grant(t *testing.T, a *store.Agent) []AgentTokenScope {
	t.Helper()
	g, err := f.srv.AuthorizeAgentToken(context.Background(), f.reload(t, a))
	require.NoError(t, err)
	return g.Scopes
}

func (f *reissueFixture) run(t *testing.T, a *store.Agent, dryRun bool) *ScopeReissueResponse {
	t.Helper()
	resp, err := f.srv.runScopeReissue(context.Background(), f.reload(t, a), f.operator, dryRun, "")
	require.NoError(t, err)
	return resp
}

// allEdges returns every edge of a, active or not.
func (f *reissueFixture) allEdges(t *testing.T, a *store.Agent) []*store.DelegationEdge {
	t.Helper()
	edges, err := f.store.ListAllDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, a.ID)
	require.NoError(t, err)
	return edges
}

func (f *reissueFixture) activeEdge(t *testing.T, a *store.Agent) *store.DelegationEdge {
	t.Helper()
	active, err := f.srv.authzService.activeProjectEdges(context.Background(), a.ID, f.projectID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	return active[0]
}

// walkAllows runs the step-10 chain walk for agent a on perm.
func (f *reissueFixture) walkAllows(t *testing.T, a *store.Agent, perm string) bool {
	t.Helper()
	resource, action, ok := reissuePermissionTarget(a, perm)
	require.True(t, ok)
	allowed, _, err := f.srv.authzService.walkDelegationChain(context.Background(), resource, action, perm, a.ID, true,
		store.RoleScopeProject, f.projectID, nil)
	require.NoError(t, err)
	return allowed
}

// legacyUAT stores a project-bounded access token of the fixture user whose
// frozen ceiling is the legacy (pre-artifact) full-role coverage, and
// returns it with the edge ceiling creation records from it.
func (f *reissueFixture) legacyUAT(t *testing.T, id string) (*store.UserAccessToken, store.EffectCeiling) {
	t.Helper()
	ctx := context.Background()
	exp := time.Now().Add(24 * time.Hour)
	tok := &store.UserAccessToken{
		ID: tid(id), UserID: f.userID, Name: id, Prefix: "scion_pat_", KeyHash: "hash-" + id,
		BoundaryKind: string(permissions.BoundaryKindProject), ProjectID: f.projectID,
		Scopes:               []string{"project:agent:manage"},
		CeilingVersion:       permissions.CeilingVersionV1,
		CeilingPermissionIDs: legacyBoundedCeiling().PermissionIDs,
		ExpiresAt:            &exp, Created: time.Now(),
	}
	require.NoError(t, f.store.CreateUserAccessToken(ctx, tok))
	u, err := f.store.GetUser(ctx, f.userID)
	require.NoError(t, err)
	identity := NewScopedUserIdentityWithBoundary(
		NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeAPI)),
		TokenBoundary{Kind: BoundaryKindProject, ProjectID: f.projectID}, tok.Scopes, tok.ID, tok.NormalizedCeiling())
	c, _, err := f.srv.authzService.sourceEffectCeiling(ctx, identity)
	require.NoError(t, err)
	return tok, c
}

func (g *globalCallSpyStore) record(name string) {
	g.mu.Lock()
	g.calls = append(g.calls, name)
	g.mu.Unlock()
}

func (g *globalCallSpyStore) ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	g.record("ListAgents")
	return g.Store.ListAgents(ctx, filter, opts)
}

func (g *globalCallSpyStore) CountAgents(ctx context.Context, filter store.AgentFilter) (int, error) {
	g.record("CountAgents")
	return g.Store.CountAgents(ctx, filter)
}

func (g *globalCallSpyStore) CountAgentsByPhaseIDs(ctx context.Context, filter store.AgentFilter) ([]store.IDPhase, error) {
	g.record("CountAgentsByPhaseIDs")
	return g.Store.CountAgentsByPhaseIDs(ctx, filter)
}

func (g *globalCallSpyStore) ListAgentMembers(ctx context.Context, filter store.AgentFilter, sort, dir string, max int) ([]store.AgentMember, error) {
	g.record("ListAgentMembers")
	return g.Store.ListAgentMembers(ctx, filter, sort, dir, max)
}

// mintGlobalCursor requests page 0 of the global endpoint with query and
// returns its nextCursor, failing the test if there is none.
func (f *globalSortedFixture) mintGlobalCursor(t *testing.T, user *store.User, query string) string {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, user, http.MethodGet, f.listPath(query), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotEmpty(t, resp.NextCursor, "query %q must produce a next cursor", query)
	return resp.NextCursor
}

// addSecondHubAdmin creates another hub-admin with the same project-admin
// binding as f.admin, so the two callers resolve an identical filter and
// differ only by identity.
func (f *globalSortedFixture) addSecondHubAdmin(t *testing.T) *store.User {
	t.Helper()
	id := tid("sg-admin-2")
	createTestUserWithRole(t, f.store, id, "sg-admin-2@test.com", store.UserRoleMember, store.SystemRoleHubAdmin)
	createTestUserWithProjectRole(t, f.store, id, "sg-admin-2@test.com", f.project.ID, store.ProjectRoleAdmin)
	u, err := f.store.GetUser(context.Background(), id)
	require.NoError(t, err)
	return u
}

func (f *globalSortedFixture) listPath(query string) string {
	p := "/api/v1/agents"
	if query != "" {
		p += "?" + query
	}
	return p
}

func (f *globalSortedFixture) createAgent(t *testing.T, slug, phase string) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID: tid("sg-agent-" + slug), Slug: slug, Name: slug,
		ProjectID: f.project.ID, Phase: phase,
		CreatedBy: f.admin.ID, OwnerID: f.admin.ID,
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	return a
}

// createAgentsBulk inserts n agents inside one transaction (fast for the
// 500+ row sizes the decision-count tests below need), mirroring
// sortedListFixture.createAgentsBulk in agent_sorted_project_list_test.go.
func (f *globalSortedFixture) createAgentsBulk(t *testing.T, n int, slugPrefix, phase string) []*store.Agent {
	t.Helper()
	agents := make([]*store.Agent, n)
	err := f.store.WithTx(context.Background(), func(tx store.Store) error {
		for i := 0; i < n; i++ {
			slug := fmt.Sprintf("%s-%d", slugPrefix, i)
			a := &store.Agent{
				ID: tid("sg-bulk-" + slug), Slug: slug, Name: slug,
				ProjectID: f.project.ID, Phase: phase,
				CreatedBy: f.admin.ID, OwnerID: f.admin.ID,
			}
			if err := tx.CreateAgent(context.Background(), a); err != nil {
				return err
			}
			agents[i] = a
		}
		return nil
	})
	require.NoError(t, err)
	return agents
}

func (r *racingAllMembersStore) ListAgentMembers(ctx context.Context, filter store.AgentFilter, sort, dir string, max int) ([]store.AgentMember, error) {
	members, err := r.Store.ListAgentMembers(ctx, filter, sort, dir, max)
	if err != nil || !r.fault.Active() {
		return members, err
	}
	r.once.Do(func() {
		for _, m := range members {
			a, gerr := r.GetAgent(ctx, m.ID)
			if gerr != nil {
				continue
			}
			a.Labels = map[string]string{"raced": "true"}
			_ = r.UpdateAgent(ctx, a)
		}
	})
	return members, nil
}

func (o *ownerChangingAfterMembersStore) ListAgentMembers(ctx context.Context, filter store.AgentFilter, sortKey, dir string, max int) ([]store.AgentMember, error) {
	members, err := o.Store.ListAgentMembers(ctx, filter, sortKey, dir, max)
	if err != nil || !o.fault.Active() {
		return members, err
	}
	o.once.Do(func() {
		a, gerr := o.GetAgent(ctx, o.agentID)
		if gerr != nil {
			return
		}
		a.OwnerID = o.newOwnerID
		_ = o.UpdateAgent(ctx, a)
	})
	return members, nil
}

// agentJWTFor mints a project-scoped agent token for agentID, usable against
// the sortedListFixture's project.
func (f *sortedListFixture) agentJWTFor(t *testing.T, agentID string) string {
	t.Helper()
	svc := f.srv.GetAgentTokenService()
	require.NotNil(t, svc)
	tok, err := svc.GenerateAgentToken(agentID, f.project.ID, []AgentTokenScope{ScopeProjectRead}, nil)
	require.NoError(t, err)
	return tok
}

func (f *sortedListFixture) listPath(query string) string {
	p := "/api/v1/projects/" + f.project.ID + "/agents"
	if query != "" {
		p += "?" + query
	}
	return p
}

// createAgent creates one agent, owned by the project owner unless
// ownerOverride is non-empty.
func (f *sortedListFixture) createAgent(t *testing.T, slug, phase string, labels map[string]string) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID: tid("sl-agent-" + slug), Slug: slug, Name: slug,
		ProjectID: f.project.ID, Phase: phase,
		CreatedBy: f.owner.ID, OwnerID: f.owner.ID,
		Labels: labels,
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	return a
}

// createAgentsBulk creates n agents in f.project inside one transaction
// (store.Store.WithTx), so a large fixture (hundreds to low thousands of
// rows) is fast regardless of the per-statement autocommit cost a loop of
// plain CreateAgent calls would otherwise pay. ownerFor, when non-nil,
// picks the OwnerID for agent index i (0-based); nil means every agent is
// owned by f.owner, matching createAgent's single-agent default.
func (f *sortedListFixture) createAgentsBulk(t *testing.T, n int, slugPrefix, phase string, ownerFor func(i int) string) []*store.Agent {
	t.Helper()
	agents := make([]*store.Agent, n)
	err := f.store.WithTx(context.Background(), func(tx store.Store) error {
		for i := 0; i < n; i++ {
			owner := f.owner.ID
			if ownerFor != nil {
				owner = ownerFor(i)
			}
			slug := fmt.Sprintf("%s-%d", slugPrefix, i)
			a := &store.Agent{
				ID: tid("sl-bulk-" + slug), Slug: slug, Name: slug,
				ProjectID: f.project.ID, Phase: phase,
				CreatedBy: owner, OwnerID: owner,
			}
			if err := tx.CreateAgent(context.Background(), a); err != nil {
				return err
			}
			agents[i] = a
		}
		return nil
	})
	require.NoError(t, err)
	return agents
}

func sortedListSetupOn(t *testing.T, srv *Server, s store.Store) *sortedListFixture {
	t.Helper()
	ctx := context.Background()
	f := &sortedListFixture{srv: srv, store: s}

	f.owner = &store.User{
		ID: tid("sl-owner"), Email: "sl-owner@test.com", DisplayName: "Owner",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, f.owner))
	ensureHubMembership(ctx, s, f.owner.ID)

	f.member = &store.User{
		ID: tid("sl-member"), Email: "sl-member@test.com", DisplayName: "Member",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, f.member))
	ensureHubMembership(ctx, s, f.member.ID)

	f.project = &store.Project{
		ID: tid("sl-project"), Name: "Sorted List Project", Slug: "sl-project",
		OwnerID: f.owner.ID, CreatedBy: f.owner.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, f.project))
	srv.seedProjectCreatorMembership(ctx, f.project)
	createTestUserWithProjectRole(t, s, f.owner.ID, f.owner.Email, f.project.ID, store.ProjectRoleOwner)
	msgAuthzAddProjectMember(t, s, f.member.ID, f.project.ID, f.project.Slug, store.GroupMemberRoleMember)

	return f
}

// serverTimeJSONRe matches the "serverTime":"..." field in a
// ListAgentsResponse's JSON encoding, so rawBodyWithoutServerTime can blank
// it out for an exact byte comparison of everything else.
var serverTimeJSONRe = regexp.MustCompile(`"serverTime":"[^"]*"`)

func (r *syncRecorder) Header() http.Header { return r.header }
func (r *syncRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.code == 0 {
		r.code = code
	}
}
func (r *syncRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.body.Write(p)
}
func (r *syncRecorder) Flush() {}
func (r *syncRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}

func (m *mintBrokerClient) ResetAuthAgent(_ context.Context, _, _, _, _, token, _ string) error {
	m.resetAuthCalled = true
	m.resetAuthToken = token
	return nil
}

// childAgent stores a running child of parent with a legacy bounded edge.
func (f *mintFixture) childAgent(t *testing.T, slug string, parent *store.Agent, role AgentRole) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID: tid(slug), Slug: slug, Name: slug, ProjectID: f.projectID, OwnerID: f.userID,
		RuntimeBrokerID: f.brokerID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Ancestry:      append(append([]string{}, parent.Ancestry...), parent.ID),
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(role)},
		Created:       time.Now(), Updated: time.Now(),
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	f.edge(t, store.DelegationPrincipalAgent, parent.ID, a.ID, legacyBoundedCeiling(), agentProv(parent.ID))
	return a
}

// agent stores an agent on the fixture broker, with ancestry [user].
func (f *mintFixture) agent(t *testing.T, slug string, role AgentRole, phase state.Phase) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID: tid(slug), Slug: slug, Name: slug, ProjectID: f.projectID, OwnerID: f.userID,
		RuntimeBrokerID: f.brokerID, Phase: string(phase), StateVersion: 1,
		Ancestry:      []string{f.userID},
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(role)},
		Created:       time.Now(), Updated: time.Now(),
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	return a
}

// edge records an active project edge for delegate with the given ceiling
// and provenance.
func (f *mintFixture) edge(t *testing.T, delegatorType, delegatorID, delegateID string, c store.EffectCeiling, p store.AuthorityProvenance) {
	t.Helper()
	require.NoError(t, f.store.CreateDelegationEdge(context.Background(), &store.DelegationEdge{
		DelegatorType: delegatorType, DelegatorID: delegatorID,
		DelegateType: store.DelegationPrincipalAgent, DelegateID: delegateID,
		ScopeType: store.RoleScopeProject, ScopeID: f.projectID,
		Role: string(AgentRoleFull), Active: true,
		AuthorityProvenance: p, EffectCeiling: c,
	}))
}

// tokenClaims validates a minted token.
func (f *mintFixture) tokenClaims(t *testing.T, token string) *AgentTokenClaims {
	t.Helper()
	require.NotEmpty(t, token)
	claims, err := f.srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)
	return claims
}

// refresh calls the refresh handler with a token minted for agent and the
// given presented ancestry.
func (f *mintFixture) refresh(t *testing.T, agent *store.Agent, presentedAncestry []string) *httptest.ResponseRecorder {
	t.Helper()
	presented, err := f.srv.agentTokenService.GenerateAgentToken(agent.ID, f.projectID,
		[]AgentTokenScope{ScopeAgentStatusUpdate, ScopeAgentTokenRefresh}, presentedAncestry)
	require.NoError(t, err)
	claims := f.tokenClaims(t, presented)
	rec := httptest.NewRecorder()
	f.srv.handleAgentTokenRefresh(rec, buildAgentRefreshRequest(agent.ID, claims, "", false), agent.ID)
	return rec
}

const tzDispatchHubID = "tz-dispatch-hub"

// assertCompactParity checks one full/compact response pair:
//   - same status; non-200 bodies identical;
//   - same top-level key set, and every top-level value other than agents
//     and serverTime byte-identical (nextCursor, totalCount, complete,
//     sort, dir, stats, _capabilities);
//   - the same agents in the same order;
//   - every compact item key is in the allowlist and exists in the full
//     item for the same agent with a byte-identical value (creatorName
//     against appliedConfig.creatorName), so compact is a strict subset;
//   - every allowlisted value the full item carries non-empty is present
//     in the compact item, and _capabilities/_messageability/deletion are
//     byte-identical including absence;
//   - no appliedConfig key at any depth of the compact body;
//   - identical decision audit records, in order.
//
// It returns the number of agent items compared.
func assertCompactParity(t *testing.T, label string, p viewPair) int {
	t.Helper()
	require.Equal(t, p.full.Code, p.compact.Code, "%s: status", label)
	assert.Equal(t, auditKeys(p.fullAudit), auditKeys(p.compactAudit), "%s: decision audit records", label)
	assert.Len(t, p.compactAudit, len(p.fullAudit), "%s: decision and audit count", label)

	fullBody, compBody := rawBodyWithoutServerTime(p.full), rawBodyWithoutServerTime(p.compact)
	if p.full.Code != http.StatusOK {
		assert.Equal(t, string(fullBody), string(compBody), "%s: non-200 body", label)
		return 0
	}
	assertNoKeyAtAnyDepth(t, label+": compact body", compBody, "appliedConfig")

	fm, cm := decodeObject(t, fullBody), decodeObject(t, compBody)
	require.Equal(t, rawKeys(fm), rawKeys(cm), "%s: top-level keys", label)
	for k := range fm {
		if k == "agents" || k == "serverTime" {
			continue
		}
		assert.Equal(t, string(fm[k]), string(cm[k]), "%s: top-level %q bytes", label, k)
	}

	fullItems, compItems := decodeItems(t, fm["agents"]), decodeItems(t, cm["agents"])
	require.Len(t, compItems, len(fullItems), "%s: item count", label)
	allow := map[string]bool{}
	for _, k := range compactItemAllowlist {
		allow[k] = true
	}
	for i := range fullItems {
		fi, ci := fullItems[i], compItems[i]
		require.Equal(t, string(fi["id"]), string(ci["id"]), "%s: item %d id/order", label, i)
		for k, cv := range ci {
			assert.True(t, allow[k], "%s: item %d: compact key %q not in allowlist", label, i, k)
			if k == "creatorName" {
				assert.Equal(t, string(creatorNameOf(t, fi)), string(cv), "%s: item %d creatorName", label, i)
				continue
			}
			fv, ok := fi[k]
			if assert.True(t, ok, "%s: item %d: compact key %q missing from full item", label, i, k) {
				assert.Equal(t, string(fv), string(cv), "%s: item %d key %q", label, i, k)
			}
		}
		for _, k := range compactItemAllowlist {
			if k == "creatorName" {
				if cn := creatorNameOf(t, fi); cn != nil && !isEmptyJSON(cn) {
					assert.Equal(t, string(cn), string(ci[k]), "%s: item %d creatorName present", label, i)
				}
				continue
			}
			if fv, ok := fi[k]; ok && !isEmptyJSON(fv) {
				assert.Contains(t, ci, k, "%s: item %d: full value of %q dropped from compact", label, i, k)
			}
		}
		for _, k := range []string{"_capabilities", "_messageability", "deletion"} {
			assert.Equal(t, string(fi[k]), string(ci[k]), "%s: item %d %s (including absence)", label, i, k)
		}
	}
	return len(fullItems)
}

func agentProv(parentID string) store.AuthorityProvenance {
	return store.AuthorityProvenance{
		ProvenanceVersion:    store.ProvenanceVersionV1,
		SourcePrincipalKind:  store.DelegationPrincipalAgent,
		SourcePrincipalID:    parentID,
		SourceCredentialKind: store.SourceCredentialAgent,
		SourceCredentialID:   "jti-" + parentID,
	}
}

// auditKeys reduces decision audit records to the fields that identify a
// decision and its outcome, in emission order.
func auditKeys(records []*store.DecisionAuditRecord) []string {
	out := make([]string, len(records))
	for i, r := range records {
		out[i] = strings.Join([]string{r.PrincipalKind, r.PrincipalID, r.ResourceType, r.ResourceID, r.Permission, r.Result, r.Reason}, "|")
	}
	return out
}

// creatorNameOf returns the full item's appliedConfig.creatorName, raw, or
// nil if absent.
func creatorNameOf(t *testing.T, fullItem map[string]json.RawMessage) json.RawMessage {
	t.Helper()
	ac, ok := fullItem["appliedConfig"]
	if !ok {
		return nil
	}
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(ac, &m))
	return m["creatorName"]
}

// isEmptyJSON reports whether raw is a value omitempty would drop.
func isEmptyJSON(raw json.RawMessage) bool {
	switch string(raw) {
	case `""`, `null`, `{}`, `[]`, `false`, `0`:
		return true
	}
	return false
}

// viewPair is one request made in both views, with what each cost.
type viewPair struct {
	full, compact           *httptest.ResponseRecorder
	fullAudit, compactAudit []*store.DecisionAuditRecord
}

// compactItemAllowlist is the exact JSON key set of a view=compact agent
// item when every field is populated.
var compactItemAllowlist = []string{
	"id", "slug", "name", "template", "projectId", "project", "labels",
	"phase", "activity", "containerStatus", "message", "messageMode", "ancestry",
	"createdBy", "creatorName", "created", "updated", "lastActivityEvent",
	"_capabilities", "_messageability", "deletion",
}

func rawKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// assertNoKeyAtAnyDepth fails if key appears as an object key anywhere in v.
func assertNoKeyAtAnyDepth(t *testing.T, label string, body []byte, key string) {
	t.Helper()
	var v interface{}
	require.NoError(t, json.Unmarshal(body, &v))
	var walk func(path string, v interface{})
	walk = func(path string, v interface{}) {
		switch x := v.(type) {
		case map[string]interface{}:
			for k, child := range x {
				if k == key {
					t.Errorf("%s: key %q found at %s", label, key, path)
				}
				walk(path+"."+k, child)
			}
		case []interface{}:
			for i, child := range x {
				walk(fmt.Sprintf("%s[%d]", path, i), child)
			}
		}
	}
	walk("$", v)
}

// chainFixture is the access-token create world with a minting dispatcher,
// for agents that create agents with their own production token.
type chainFixture struct {
	*uatCreateFixture
	client *mintBrokerClient
}

// signErrStorage fails every signed URL request.
type signErrStorage struct {
	*mockStorage
}

// compensationFailureLog is the ERROR record logCompensationFailure writes.
type compensationFailureLog struct {
	Msg           string `json:"msg"`
	AgentID       string `json:"agent_id"`
	CorrelationID string `json:"correlation_id"`
	OpID          string `json:"op_id"`
}

// createdRecordingPublisher is deleteRecordingPublisher plus agent.created.
type createdRecordingPublisher struct {
	*deleteRecordingPublisher
}

// deleteMode is how far the racing DELETE gets before the create's dispatch
// returns.
type deleteMode string

const staleDispatchBody = `{"error":{"code":"stale_dispatch","message":"delete dispatch arrived after its deadline; nothing was done"}}`

// readRuleFixture is one project whose agents have three owners: the
// project owner, a project member, and a caller holding only agent.list on
// the project. The caller can read exactly the agents it owns.
type readRuleFixture struct {
	*sortedListFixture
	caller   *store.User
	readable []string // ids the caller can read, sorted
	all      []string // every agent id in the project, sorted
}

// captureHandler records log messages and their attributes.
type captureHandler struct {
	mu   sync.Mutex
	recs []capturedLog
}

// credRecord decides the credential row stored for a signed token: nil
// stores none, otherwise the row records the returned run.
type credRecord func() *string

// reissueFaultStore injects the re-issue tests' store faults. It delegates
// everything until its switch is armed; then each configured fault applies:
//   - failParentOnce: the next GetAgent for failParentID fails (one shot,
//     re-armed by the test before each call);
//   - dupEdgeAgentID: that agent's active edge is reported twice;
//   - edgeReadErr: every agent-delegate edge read fails;
//   - auditFailInTx: the agent_scopes_reissued audit write inside a
//     transaction fails.
type reissueFaultStore struct {
	store.Store
	fault *storeFaultSwitch
	sw    *storeFaultSwitch

	failParentID   string
	failParentOnce atomic.Bool
	parentFired    atomic.Int32
	dupEdgeAgentID string
	edgeReadErr    bool
	auditFailInTx  bool
	auditFired     atomic.Int32
	// nilAgentInTxID: inside a transaction, GetAgent for this ID answers
	// no row and no error.
	nilAgentInTxID string
	// nilAgentAfterCommitID: once a transaction has committed, GetAgent for
	// this ID answers no row and no error.
	nilAgentAfterCommitID string
	committed             atomic.Bool
	// batchAuditFail: the agent_scopes_reissue_batch audit write fails.
	batchAuditFail bool
	// failUserID: GetUser for this ID fails.
	failUserID string
	// uatReadErr: GetUserAccessToken fails.
	uatReadErr bool
}

// fieldMutatingAfterMembersStore generalizes labelsNilToEmptyAfterMembersStore
// (and mutatingAfterMembersStore) to any single-field mutation applied after
// the first ListAgentMembers call: closes a gap where the original
// end-to-end test exercised only Labels nil->empty, when
// normalizeResourceForCompare normalizes both Labels and Ancestry, in both
// directions.
type fieldMutatingAfterMembersStore struct {
	store.Store
	fault   *storeFaultSwitch // nil: always active
	once    sync.Once
	agentID string
	mutate  func(a *store.Agent)
}

// countingAgentStore wraps a real store.Store and lets tests fake
// CountAgents/ListAgentMembers results, or count calls, without paying for
// thousands of real row inserts in the test SQLite backend.
type countingAgentStore struct {
	store.Store
	fault             *storeFaultSwitch // nil: always counting/faking
	mu                sync.Mutex
	countAgentsCalls  int
	membersCalls      int
	getByIDsCalls     int
	listAgentsCalls   int
	listAgentsIDs     [][]string // filter.IDs seen by each ListAgents call, in call order
	fakeCandidateSize int        // if > 0, CountAgents and ListAgentMembers report this size
	maxSeen           int        // last "max" ListAgentMembers was called with
}

// raceMembersStore always answers ListAgentMembers with memberCount rows
// (capped at the caller's max), independent of CountAgents' answer,
// simulating candidate growth between the two reads.
type raceMembersStore struct {
	*countingAgentStore
	memberCount int
}

// mutatingAfterMembersStore mutates an agent's labels (via the real store,
// bypassing the read path) the first time ListAgentMembers is called,
// simulating a write landing between the member read and the full-row
// read.
type mutatingAfterMembersStore struct {
	store.Store
	fault     *storeFaultSwitch // nil: always active
	once      sync.Once
	agentID   string
	newLabels map[string]string
}

type deletingAfterMembersStore struct {
	store.Store
	fault   *storeFaultSwitch // nil: always active
	once    sync.Once
	agentID string
}

type reprojectingListAgentsStore struct {
	store.Store
	fault        *storeFaultSwitch // nil: always active
	agentID      string
	newProjectID string
}

type tzDispatchFixture struct {
	d      *HTTPAgentDispatcher
	store  store.Store
	client *mockRuntimeBrokerClient
	agent  *store.Agent
	// hubDefault is read live by the dispatcher's agent-defaults provider.
	hubDefault string
}

// agentToken mints the production token for the stored agent.
func (f *chainFixture) agentToken(t *testing.T, agentID string) string {
	t.Helper()
	a, err := f.store.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	tok, err := f.srv.issueAgentTokenForTest(context.Background(), a)
	require.NoError(t, err)
	return tok
}

// createAsParent posts a create in the fixture project with token.
func (f *chainFixture) createAsParent(t *testing.T, token string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestWithAgentToken(t, f.srv, http.MethodPost, f.path, body, token)
}

// sessionParent creates an agent with the creator's session.
func (f *chainFixture) sessionParent(t *testing.T, slug string) (*store.Agent, *store.DelegationEdge) {
	t.Helper()
	return f.createdAgent(t, f.create(t, authUser(f.creator), CreateAgentRequest{Name: slug}), slug)
}

// childOf creates slug with parent's production token and returns it.
func (f *chainFixture) childOf(t *testing.T, parent *store.Agent, slug string) (*store.Agent, *store.DelegationEdge) {
	t.Helper()
	rec := f.createAsParent(t, f.agentToken(t, parent.ID), CreateAgentRequest{Name: slug})
	return f.createdAgent(t, rec, slug)
}

// mint returns a mintFixture view of the chain fixture, for refresh.
func (f *chainFixture) mint() *mintFixture {
	return &mintFixture{srv: f.srv, store: f.store, client: f.client, projectID: f.proj.ID}
}

// agentIdentityFor returns the request identity for a production token.
func (f *chainFixture) agentIdentityFor(t *testing.T, token string) *agentIdentityWrapper {
	t.Helper()
	claims, err := f.srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)
	return &agentIdentityWrapper{AgentTokenClaims: claims}
}

func (signErrStorage) GenerateSignedURL(context.Context, string, storage.SignedURLOptions) (*storage.SignedURL, error) {
	return nil, errors.New("injected signed URL fault")
}

func (p *createdRecordingPublisher) PublishAgentCreated(ctx context.Context, a *store.Agent) {
	p.mu.Lock()
	p.events = append(p.events, recordedAgentEvent{
		kind: "created", phase: a.Phase, activity: a.Activity,
		deletion: store.ComputeAgentDeletion(a, time.Now()),
	})
	p.mu.Unlock()
	p.deleteRecordingPublisher.PublishAgentCreated(ctx, a)
}

// PublishAgentRestored is the other created publisher (see EventPublisher).
func (p *createdRecordingPublisher) PublishAgentRestored(ctx context.Context, a *store.Agent, restoredAt time.Time) {
	p.mu.Lock()
	p.events = append(p.events, recordedAgentEvent{
		kind: "created", phase: a.Phase, activity: a.Activity,
		deletion: store.ComputeAgentDeletion(a, time.Now()),
	})
	p.mu.Unlock()
	p.deleteRecordingPublisher.PublishAgentRestored(ctx, a, restoredAt)
}

// kinds lists the recorded event kinds in order.
func (p *createdRecordingPublisher) kinds() []string {
	var out []string
	for _, e := range p.snapshot() {
		out = append(out, e.kind)
	}
	return out
}

func (f *readRuleFixture) globalPath(query string) string {
	return "/api/v1/agents?projectId=" + f.project.ID + "&" + query
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	rec := capturedLog{msg: r.Message, attrs: map[string]any{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.Any()
		return true
	})
	h.recs = append(h.recs, rec)
	return nil
}

// records returns the captured records named msg.
func (h *captureHandler) records(msg string) []capturedLog {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []capturedLog
	for _, r := range h.recs {
		if r.msg == msg {
			out = append(out, r)
		}
	}
	return out
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// capturedLog is one log record with its attributes.
type capturedLog struct {
	msg   string
	attrs map[string]any
}

func (s *reissueFaultStore) CreateMutationAudit(ctx context.Context, r *store.MutationAuditRecord) error {
	if s.fault.Active() && s.batchAuditFail && r.MutationType == mutationTypeAgentScopesReissueBatch {
		return errors.New("injected batch audit write fault")
	}
	return s.Store.CreateMutationAudit(ctx, r)
}

func (s *reissueFaultStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if s.fault.Active() && s.failUserID != "" && id == s.failUserID {
		return nil, errors.New("injected user read fault")
	}
	return s.Store.GetUser(ctx, id)
}

func (s *reissueFaultStore) GetUserAccessToken(ctx context.Context, id string) (*store.UserAccessToken, error) {
	if s.fault.Active() && s.uatReadErr {
		return nil, errors.New("injected access token read fault")
	}
	return s.Store.GetUserAccessToken(ctx, id)
}

// arm turns the configured faults on.
func (s *reissueFaultStore) arm() { s.sw.Arm() }

func (s *reissueFaultStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if s.fault.Active() && id == s.failParentID && s.failParentOnce.CompareAndSwap(true, false) {
		s.parentFired.Add(1)
		return nil, errors.New("injected agent read fault")
	}
	if s.fault.Active() && s.committed.Load() && id == s.nilAgentAfterCommitID {
		return nil, nil
	}
	return s.Store.GetAgent(ctx, id)
}

func (s *reissueFaultStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	if s.fault.Active() && s.edgeReadErr && delegateType == store.DelegationPrincipalAgent {
		return nil, errors.New("injected delegation edge read fault")
	}
	edges, err := s.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
	if err != nil || !s.fault.Active() || delegateID != s.dupEdgeAgentID || len(edges) == 0 {
		return edges, err
	}
	dup := *edges[0]
	dup.ID = "duplicate"
	return append(edges, &dup), nil
}

func (s *reissueFaultStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	if !s.fault.Active() {
		return s.Store.WithTx(ctx, fn)
	}
	err := s.Store.WithTx(ctx, func(tx store.Store) error {
		if s.auditFailInTx {
			tx = &auditFailStore{Store: tx, fired: &s.auditFired}
		}
		if s.nilAgentInTxID != "" {
			tx = &reissueNilAgentStore{Store: tx, id: s.nilAgentInTxID}
		}
		return fn(tx)
	})
	if err == nil {
		s.committed.Store(true)
	}
	return err
}

func (f *fieldMutatingAfterMembersStore) ListAgentMembers(ctx context.Context, filter store.AgentFilter, sortKey, dir string, max int) ([]store.AgentMember, error) {
	members, err := f.Store.ListAgentMembers(ctx, filter, sortKey, dir, max)
	if err != nil || !f.fault.Active() {
		return members, err
	}
	f.once.Do(func() {
		a, gerr := f.GetAgent(ctx, f.agentID)
		if gerr != nil {
			return
		}
		f.mutate(a)
		_ = f.UpdateAgent(ctx, a)
	})
	return members, nil
}

func (c *countingAgentStore) CountAgents(ctx context.Context, filter store.AgentFilter) (int, error) {
	if !c.fault.Active() {
		return c.Store.CountAgents(ctx, filter)
	}
	c.mu.Lock()
	c.countAgentsCalls++
	c.mu.Unlock()
	if c.fakeCandidateSize > 0 {
		return c.fakeCandidateSize, nil
	}
	return c.Store.CountAgents(ctx, filter)
}

func (c *countingAgentStore) ListAgentMembers(ctx context.Context, filter store.AgentFilter, sort, dir string, max int) ([]store.AgentMember, error) {
	if !c.fault.Active() {
		return c.Store.ListAgentMembers(ctx, filter, sort, dir, max)
	}
	c.mu.Lock()
	c.membersCalls++
	c.maxSeen = max
	c.mu.Unlock()
	if c.fakeCandidateSize > 0 {
		n := c.fakeCandidateSize
		if n > max {
			n = max
		}
		out := make([]store.AgentMember, n)
		for i := range out {
			out[i] = store.AgentMember{ID: fmt.Sprintf("fake-%d", i), ProjectID: filter.ProjectID}
		}
		return out, nil
	}
	return c.Store.ListAgentMembers(ctx, filter, sort, dir, max)
}

func (c *countingAgentStore) GetAgentsByIDs(ctx context.Context, ids []string) (map[string]*store.Agent, error) {
	if !c.fault.Active() {
		return c.Store.GetAgentsByIDs(ctx, ids)
	}
	c.mu.Lock()
	c.getByIDsCalls++
	c.mu.Unlock()
	return c.Store.GetAgentsByIDs(ctx, ids)
}

// ListAgents is overridden so tests can observe loadFullRowsForPage's actual
// full-row read: how many times it runs per request, and exactly which IDs
// it asks for (the old getByIDsCalls assertion in
// TestListProjectAgentsSorted_CandidateCeiling was vacuous after the
// full-row read moved from GetAgentsByIDs to ListAgents to honor
// includeDeleted, so it passed regardless of what the handler actually did).
func (c *countingAgentStore) ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	if !c.fault.Active() {
		return c.Store.ListAgents(ctx, filter, opts)
	}
	c.mu.Lock()
	c.listAgentsCalls++
	c.listAgentsIDs = append(c.listAgentsIDs, append([]string(nil), filter.IDs...))
	c.mu.Unlock()
	return c.Store.ListAgents(ctx, filter, opts)
}

func (r *raceMembersStore) ListAgentMembers(ctx context.Context, filter store.AgentFilter, sort, dir string, max int) ([]store.AgentMember, error) {
	if !r.fault.Active() {
		return r.Store.ListAgentMembers(ctx, filter, sort, dir, max)
	}
	r.mu.Lock()
	r.membersCalls++
	r.mu.Unlock()
	n := r.memberCount
	if n > max {
		n = max
	}
	out := make([]store.AgentMember, n)
	for i := range out {
		out[i] = store.AgentMember{ID: fmt.Sprintf("race-%d", i), ProjectID: filter.ProjectID}
	}
	return out, nil
}

func (m *mutatingAfterMembersStore) ListAgentMembers(ctx context.Context, filter store.AgentFilter, sort, dir string, max int) ([]store.AgentMember, error) {
	members, err := m.Store.ListAgentMembers(ctx, filter, sort, dir, max)
	if err != nil || !m.fault.Active() {
		return members, err
	}
	m.once.Do(func() {
		a, gerr := m.GetAgent(ctx, m.agentID)
		if gerr != nil {
			return
		}
		a.Labels = m.newLabels
		_ = m.UpdateAgent(ctx, a)
	})
	return members, nil
}

func (d *deletingAfterMembersStore) ListAgentMembers(ctx context.Context, filter store.AgentFilter, sort, dir string, max int) ([]store.AgentMember, error) {
	members, err := d.Store.ListAgentMembers(ctx, filter, sort, dir, max)
	if err != nil || !d.fault.Active() {
		return members, err
	}
	d.once.Do(func() { _ = d.DeleteAgent(ctx, d.agentID) })
	return members, nil
}

func (r *reprojectingListAgentsStore) ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	result, err := r.Store.ListAgents(ctx, filter, opts)
	if err != nil || !r.fault.Active() {
		return result, err
	}
	for i := range result.Items {
		if result.Items[i].ID == r.agentID {
			result.Items[i].ProjectID = r.newProjectID
		}
	}
	return result, nil
}

func (f *tzDispatchFixture) seedEnv(t *testing.T, scope, scopeID, value, mode string) {
	t.Helper()
	tzTestEnvVar(t, f.store, store.EnvVar{Scope: scope, ScopeID: scopeID, Value: value, InjectionMode: mode})
}

func (f *tzDispatchFixture) createTZ(t *testing.T) string {
	t.Helper()
	_, err := f.d.DispatchAgentCreate(context.Background(), f.agent)
	require.NoError(t, err)
	require.NotNil(t, f.client.lastCreateReq)
	return f.client.lastCreateReq.ResolvedEnv["TZ"]
}

func (f *tzDispatchFixture) startTZ(t *testing.T) string {
	t.Helper()
	require.NoError(t, f.d.DispatchAgentStart(context.Background(), f.agent, "", false))
	return f.client.lastResolvedEnv["TZ"]
}

func (f *tzDispatchFixture) restartTZ(t *testing.T) string {
	t.Helper()
	require.NoError(t, f.d.DispatchAgentRestart(context.Background(), f.agent))
	return f.client.lastRestartResolvedEnv["TZ"]
}

// reissueNilAgentStore answers GetAgent for id with no row and no error.
type reissueNilAgentStore struct {
	store.Store
	id string
}

// auditFailStore fails the agent_scopes_reissued audit write; the
// reissueFaultStore wraps a transaction's store with it.
type auditFailStore struct {
	store.Store
	fired *atomic.Int32
}

func (s *reissueNilAgentStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if id == s.id {
		return nil, nil
	}
	return s.Store.GetAgent(ctx, id)
}

func (s *auditFailStore) CreateMutationAudit(ctx context.Context, r *store.MutationAuditRecord) error {
	if r.MutationType == mutationTypeAgentScopesReissued {
		s.fired.Add(1)
		return errors.New("injected audit write fault")
	}
	return s.Store.CreateMutationAudit(ctx, r)
}
