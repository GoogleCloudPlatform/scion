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
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/managedagent"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// b7TestServer creates a test server with B3-B6 boundary services wired.
// The dev user is automatically granted constraint admin + read permissions.
func b7TestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	srv, s := testServer(t)

	// The dev user is already seeded by testServer → New() → seedDevUser() with
	// a super-admin role binding. Super-admin includes all permissions from the
	// registry, but access_constraint.admin is intentionally excluded from the
	// hub-admin built-in role (it requires explicit binding). For tests that need
	// admin-level access, add a dedicated role binding.
	rd := createTestRoleDefinition(t, s, "test-boundary-admin", store.RoleScopeSystem,
		[]string{PermissionConstraintAdmin, PermissionConstraintRead,
			"agent.read", "agent.create", "agent.delete", "project.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", DevUserID, store.RoleScopeSystem, "")

	// Ensure B3-B6 services are initialized.
	if srv.previewService == nil {
		srv.initBoundaryServices()
	}

	return srv, s
}

// b7SeedConstraint creates a constraint directly in the store for testing reads.
// Targets a specific seeded user to avoid restricting the dev admin.
func b7SeedConstraint(t *testing.T, s store.Store, name string) *store.AccessConstraint {
	t.Helper()

	// Seed a target user for the constraint subject.
	targetUserID := pvSeedUser(t, s, "constraint-target-"+name)

	principalType := "user"
	c := &store.AccessConstraint{
		Name:                 name,
		Purpose:              "Test constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &principalType,
		SubjectPrincipalID:   &targetUserID,
		ScopeType:            store.RoleScopeSystem,
		ScopeID:              "",
		MaximumPermissions:   []string{"agent.read", "agent.create"},
		CreatedBy:            DevUserID,
		UpdatedBy:            DevUserID,
	}
	created, err := s.CreateAccessConstraint(t.Context(), c)
	require.NoError(t, err)
	return created
}

// doRequestHeaders performs an HTTP request with custom headers against the test server.
func doRequestHeaders(t *testing.T, srv *Server, method, path string, body interface{}, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("failed to marshal body: %v", err)
		}
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func brokerLinkAuthzSetup(t *testing.T) *brokerLinkAuthzFixture {
	t.Helper()
	f := &brokerLinkAuthzFixture{bypassAgentsFixture: bypassAgentsSetup(t)}
	ctx := context.Background()

	// Owned by the project owner: linking needs broker.update on the broker
	// (owner or super-admin) in addition to project.update.
	f.unlinked = &store.RuntimeBroker{
		ID:          uuid.New().String(),
		Name:        "link-authz-unlinked",
		Slug:        "link-authz-unlinked",
		Status:      store.BrokerStatusOnline,
		AutoProvide: true,
		CreatedBy:   f.owner.ID,
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require.NoError(t, f.store.CreateRuntimeBroker(ctx, f.unlinked))

	f.member = &store.User{
		ID:          tid("link-authz-member"),
		Email:       "link-authz-member@example.com",
		DisplayName: "Link Authz Member",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, f.store.CreateUser(ctx, f.member))
	createTestUserWithProjectRole(t, f.store, f.member.ID, f.member.Email, f.proj.ID, store.ProjectRoleMember)

	// f.proj already has a default broker (f.broker) from bypassAgentsSetup;
	// clear it so "default set when none existed" is meaningful for the
	// owner-allowed case below.
	f.proj.DefaultRuntimeBrokerID = ""
	require.NoError(t, f.store.UpdateProject(ctx, f.proj))
	require.NoError(t, f.store.RemoveProjectProvider(ctx, f.proj.ID, f.broker.ID))

	return f
}

func createAgentPath(f *projectAgentAuthzFixture) string {
	return "/api/v1/projects/" + f.project.ID + "/agents"
}

// attachAutoProvideBroker gives f.project an auto-provide runtime broker, the
// same way authz_bypass_agents_test.go's bypassAgentsSetup does, so create
// requests in this file resolve a broker instead of failing at broker
// selection before they ever reach identity-key validation.
func attachAutoProvideBroker(t *testing.T, f *projectAgentAuthzFixture) {
	t.Helper()
	ctx := context.Background()
	broker := &store.RuntimeBroker{
		ID:          uuid.New().String(),
		Name:        "identity-key-test-broker",
		Slug:        "identity-key-test-broker",
		Status:      store.BrokerStatusOnline,
		AutoProvide: true,
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require.NoError(t, f.store.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, f.store.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  f.project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))
	f.project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, f.store.UpdateProject(ctx, f.project))
}

// deleteCountingEventPublisher counts PublishAgentDeleted calls, so a test
// can assert a concurrent double-delete only ever publishes once.
type deleteCountingEventPublisher struct {
	noopEventPublisher
	mu    sync.Mutex
	count int
}

// countQuotaReleases swaps srv's quota service for one that counts
// releaseAgentQuotas calls and returns the counter.
func countQuotaReleases(t *testing.T, srv *Server) func() int {
	t.Helper()
	cs := &quotaReleaseCountingStore{Store: srv.store}
	srv.quotaService = &QuotaService{store: cs, logger: slog.Default()}
	return func() int {
		cs.mu.Lock()
		defer cs.mu.Unlock()
		return cs.count
	}
}

// findIdentityKey returns the row in keys with the given agentID and key
// value, or nil if there is none.
func findIdentityKey(keys []*store.AgentIdentityKey, agentID, key string) *store.AgentIdentityKey {
	for _, k := range keys {
		if k.AgentID == agentID && k.Key == key {
			return k
		}
	}
	return nil
}

func callerPath(f *projectAgentAuthzFixture) string {
	return "/api/v1/projects/" + f.project.ID + "/agents/" + f.caller.ID
}

func postLaunchReport(t *testing.T, srv *Server, brokerID, agentID, identityBrokerID string, report AgentLaunchReport) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(report)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-brokers/"+brokerID+"/agents/"+agentID+"/launch", bytes.NewReader(body))
	if identityBrokerID != "" {
		req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity(identityBrokerID)))
	}
	rr := httptest.NewRecorder()
	srv.handleRuntimeBrokerRoutes(rr, req)
	return rr
}

// setupBrokerAgentInPhase creates a project, an online runtime broker, and an
// agent assigned to that broker in the given phase.
func setupBrokerAgentInPhase(t *testing.T, s store.Store, suffix string, phase state.Phase) *store.Agent {
	t.Helper()
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("proj-lc-resume-" + suffix),
		Name: "LC Resume Project " + suffix,
		Slug: "lc-resume-project-" + suffix,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:       tid("broker-lc-resume-" + suffix),
		Name:     "LC Resume Broker " + suffix,
		Slug:     "lc-resume-broker-" + suffix,
		Status:   store.BrokerStatusOnline,
		Endpoint: "http://localhost:9800",
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID:              tid("agent-lc-resume-" + suffix),
		Slug:            "agent-lc-resume-" + suffix + "-slug",
		Name:            "Agent LC Resume " + suffix,
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(phase),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	return agent
}

// def49Setup creates a project, two agents (attacker and target), a user
// (the legitimate sender, DevUserID), and a dispatcher so the handler
// doesn't fail with 503. It returns everything needed to exercise the
// caller-supplied conversation_id authorization path.
func def49Setup(t *testing.T) (srv *Server, s store.Store, projectID string, targetAgent *store.Agent, userID string) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	projectID = tid("def49-project")
	if err := s.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: "def49-project",
		Slug: "def49-project",
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	brokerID := tid("def49-broker")
	if err := s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "def49-broker",
		Slug:   "def49-broker",
		Status: store.BrokerStatusOnline,
	}); err != nil {
		t.Fatalf("CreateRuntimeBroker: %v", err)
	}
	if err := s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  projectID,
		BrokerID:   brokerID,
		BrokerName: "def49-broker",
		Status:     store.BrokerStatusOnline,
	}); err != nil {
		t.Fatalf("AddProjectProvider: %v", err)
	}

	targetAgent = &store.Agent{
		ID:              tid("def49-target-agent"),
		Name:            "def49-target-agent",
		Slug:            "def49-target-agent",
		ProjectID:       projectID,
		RuntimeBrokerID: brokerID,
		Phase:           "running",
	}
	if err := s.CreateAgent(ctx, targetAgent); err != nil {
		t.Fatalf("CreateAgent (target): %v", err)
	}

	userID = DevUserID
	_ = s.CreateUser(ctx, &store.User{
		ID:          userID,
		Email:       "dev@localhost",
		DisplayName: "Development User",
	})

	srv.SetDispatcher(&recordingDispatcher{})
	return srv, s, projectID, targetAgent, userID
}

func strPtr(s string) *string { return &s }

// alwaysDropUserBus is an eventbus.EventBus whose Publish always reports a
// subscriber-buffer-full drop, simulating InProcessEventBus.Publish when a
// project's user-message subscriber is saturated (ptone/scion#2311).
type alwaysDropUserBus struct{}

// stubManagedAgentBackend is a minimal managedagent.ManagedAgentBackend used
// to exercise the managed-runtime dispatch path (handlers_managed_agents.go)
// without touching real cloud config or the network. getManagedBackend
// returns managedBackendInst immediately when it's already non-nil, before
// any config/API-key loading — swapping that unexported package-level var
// from within this package's own test file is an *existing* seam, not a new
// production one (review round 3 finding #2).
type stubManagedAgentBackend struct{}

// newReincarnateAuthzUser creates and persists a real, non-admin member user
// with no role bindings of its own -- callers grant only what each test needs.
func newReincarnateAuthzUser(t *testing.T, s store.Store, idSuffix string) *store.User {
	t.Helper()
	ctx := context.Background()
	user := &store.User{
		ID:          tid("reincarnate-authz-" + idSuffix),
		Email:       "reincarnate-authz-" + idSuffix + "@test.com",
		DisplayName: "Reincarnate Authz " + idSuffix,
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	return user
}

// grantAgentLifecycleAtProject grants userID the agent.lifecycle permission
// at project scope, via a dedicated single-permission role binding -- the
// same mechanism uat_enforcement_test.go's grantPermissionViaRoleBinding
// uses, kept local so this grant is independent of exactly which built-in
// role bundles agent.lifecycle (project-owner and project-admin both do,
// but that is incidental to what this test needs to prove).
func grantAgentLifecycleAtProject(t *testing.T, s store.Store, userID, projectID string) {
	t.Helper()
	grantPermissionViaRoleBinding(t, s, userID, "agent.lifecycle", store.RoleScopeProject, projectID)
}

// grantAgentDelegationAtProject grants userID agent.create at project scope.
// A reincarnation requested by another principal requires delegation
// authority for the agent's role (CanDelegate) in addition to
// agent.lifecycle, whether or not it re-records the agent's edge.
func grantAgentDelegationAtProject(t *testing.T, s store.Store, userID, projectID string) {
	t.Helper()
	grantPermissionViaRoleBinding(t, s, userID, "agent.create", store.RoleScopeProject, projectID)
}

// grantProjectRole binds userID to a real, named, seeded project-scoped role
// (e.g. store.ProjectRoleAdmin), for the "a session user with the role" case
// -- as distinct from the PAT tests, which grant the bare permission
// directly and are not about role bundles at all.
func grantProjectRole(t *testing.T, s store.Store, userID, projectID, roleName string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
	require.NoError(t, err, "role definition %q not found", roleName)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// scopedIdentityFor wraps a real store.User as the UserIdentity a
// ScopedUserIdentity (the production PAT representation) decorates.
func scopedIdentityFor(user *store.User, projectID string, scopes []string) *ScopedUserIdentity {
	base := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "api")
	return NewScopedUserIdentity(base, projectID, scopes)
}

// moveFixture is a clone-per-agent Kubernetes agent on src, with dst a
// second broker on the same NFS export. Both advertise AgentMove unless a
// test downgrades them.
type moveFixture struct {
	srv     *Server
	s       store.Store
	disp    *reincarnateTestDispatcher
	project *store.Project
	src     *store.RuntimeBroker
	dst     *store.RuntimeBroker
	agent   *store.Agent
}

func moveFixtureStorage() *api.BrokerWorkspaceStorage {
	return &api.BrokerWorkspaceStorage{
		Backend: api.WorkspaceStorageBackendNFS,
		NFS: &api.BrokerNFSWorkspaceStorage{Server: "10.0.0.2", Export: "/scion-workspaces", SubPathRoot: "projects", Healthy: true,
			ExportID: moveTestExportID},
	}
}

// setupMoveFixture builds the fixture. dstIsProvider links dst to the
// project; mutate adjusts dst before it is stored.
func setupMoveFixture(t *testing.T, dstIsProvider bool, mutate func(dst *store.RuntimeBroker)) *moveFixture {
	t.Helper()
	ctx := context.Background()
	disp := newReincarnateTestDispatcher()
	srv, s, project, src := setupReincarnateTestServer(t, disp)

	src.WorkspaceStorage = moveFixtureStorage()
	src.Capabilities = &store.BrokerCapabilities{Reprovision: true, AgentMove: true}
	src.Profiles = moveFixtureProfiles()
	src.DefaultProfile = "k8s"
	require.NoError(t, s.UpdateRuntimeBroker(ctx, src))

	dst := &store.RuntimeBroker{
		ID:               tid("move-dst-" + t.Name()),
		Name:             "move-dst",
		Slug:             "move-dst-" + tidSlugSafe(t.Name()),
		Status:           store.BrokerStatusOnline,
		Capabilities:     &store.BrokerCapabilities{Reprovision: true, AgentMove: true},
		WorkspaceStorage: moveFixtureStorage(),
		Profiles:         moveFixtureProfiles(),
		DefaultProfile:   "k8s",
	}
	if mutate != nil {
		mutate(dst)
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, dst))
	if dstIsProvider {
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: project.ID, BrokerID: dst.ID, BrokerName: dst.Name, Status: store.BrokerStatusOnline,
		}))
	}

	agent := newReincarnateTestAgent(t, s, project, src, func(a *store.Agent) {
		a.Runtime = "kubernetes"
	})
	require.Equal(t, src.ID, agent.RuntimeBrokerID, "fixture agent runs on src")
	// The agent's last start placed its workspace on the export.
	require.NoError(t, s.SetAgentWorkspacePlacement(ctx, agent.ID, api.WorkspacePlacementExport))
	agent.WorkspacePlacement = api.WorkspacePlacementExport
	return &moveFixture{srv: srv, s: s, disp: disp, project: project, src: src, dst: dst, agent: agent}
}

// decodeMoveRefusal decodes an error response and its verdict.
func decodeMoveRefusal(t *testing.T, rec *httptest.ResponseRecorder) (code, message string, v MoveVerdict) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				Verdict *MoveVerdict `json:"verdict"`
			} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	require.NotNil(t, body.Error.Details.Verdict, "refusal must carry the verdict: %s", rec.Body.String())
	return body.Error.Code, body.Error.Message, *body.Error.Details.Verdict
}

// assertVerdictFailedAt checks every check before failed passed, failed
// failed, and every later one was not evaluated.
func assertVerdictFailedAt(t *testing.T, v MoveVerdict, failed string) {
	t.Helper()
	require.Len(t, v.Checks, len(moveCheckOrder))
	seen := false
	for i, c := range v.Checks {
		require.Equal(t, moveCheckOrder[i], c.Name)
		switch {
		case c.Name == failed:
			seen = true
			assert.Equal(t, MoveCheckFailed, c.Result, c.Name)
		case seen:
			assert.Equal(t, MoveCheckNotEvaluated, c.Result, c.Name)
		default:
			assert.Equal(t, MoveCheckPassed, c.Result, c.Name)
		}
	}
	require.True(t, seen, "check %s not in verdict", failed)
	assert.False(t, v.Eligible)
}

// reincarnateTestDispatcher is a fake AgentDispatcher recording every call the
// reincarnation worker makes, with optional injected failures per step.
type reincarnateTestDispatcher struct {
	mu sync.Mutex

	stopCalls        int
	reprovisionCalls int
	startCalls       int
	lastStartTask    string
	lastStartResume  *bool

	stopErr        error
	reprovisionErr error
	startErr       error

	// reprovisionImage, when non-empty, simulates the broker echoing back a
	// resolved container image on a successful reprovision response — as the
	// real HTTP dispatcher's applyBrokerResponse does by mutating
	// agent.AppliedConfig.Image in place (design §3.4 Amendment A11.1(b)).
	reprovisionImage string

	// startImage, when non-empty, simulates the broker echoing back a
	// resolved container image on a successful start response.
	startImage string

	// imageRegistry is the registry the fake reports through
	// ImageRegistryFor, as the HTTP dispatcher reports the registry it
	// rewrites images to.
	imageRegistry string
	// imageRegistryByBroker, when set, answers ImageRegistryFor per broker
	// instead of imageRegistry; registryCalls records each (broker,
	// profile) asked for.
	imageRegistryByBroker map[string]string
	registryCalls         [][2]string

	// reprovisionEcho and startEcho, when set, run on a successful dispatch
	// against the dispatched agent's AppliedConfig, simulating the broker
	// echo applyBrokerAgentConfig writes in place (HarnessConfig,
	// HarnessAuth, Image, Profile).
	reprovisionEcho func(cfg *store.AgentAppliedConfig)
	startEcho       func(cfg *store.AgentAppliedConfig)

	// rerenderErr is returned by every reprovision call after the first:
	// the worker's best-effort re-render of the previous config
	// (ptone/scion#1935). reprovisionErr applies to the first call only.
	// Move dispatch (agentMoveDispatcher): calls record the broker each
	// went to; errors make that call fail.
	moveProvisionCalls   []string // "<brokerID>|<expected workspace>"
	moveProvisionErr     error
	localOnlyDeleteCalls []string // "<brokerID>|<runID>"
	localOnlyDeleteErr   map[string]error
	// recreatedDeleteCalls records a forced move's rollback deletes
	// (DispatchAgentDeleteRecreated): "<brokerID>|<projectID>|<runID>".
	recreatedDeleteCalls []string
	recreatedDeleteErr   error
	// recreateFiles, when non-nil, models each broker's files for the
	// moved agent as a forced move sees them: a fresh-agent-dir provision
	// to a broker holding some is refused with the broker's 409 (marked
	// sent); one that proceeds creates them, even when it then fails with
	// a moveProvisionErr marked sent (a partial provision); a
	// moveProvisionErr not marked sent is returned before any of this,
	// as a pre-send failure; a recreated-files delete
	// removes them, and a localOnly delete does not (it leaves the
	// workspace a move created on the export).
	recreateFiles map[string]bool
	// recreateOwner is the move marker (reincarnation ID) of the files a
	// forced move's provision created on each broker; files a test put in
	// recreateFiles itself have none, so a rollback delete is refused with
	// move_marker_mismatch and removes nothing. moveProvisionFailsBeforeF4
	// makes a sent moveProvisionErr a refusal from before the broker's
	// absence check: nothing is created. moveProvisionIDs and
	// recreatedDeleteIDs record the reincarnation IDs each call carried.
	recreateOwner              map[string]string
	moveProvisionFailsBeforeF4 bool
	moveProvisionIDs           []string
	recreatedDeleteIDs         []string

	startBrokers []string
	// runStore, when set, makes DispatchAgentStart mint and record a run
	// on the row first, like the real dispatcher's beginRun, and record
	// startPlacement (when set), like a target's start report.
	runStore       store.Store
	startPlacement string
	// stopHook, when set, runs inside DispatchAgentStop.
	stopHook func()

	rerenderErr error
	// rerenderEcho, when set, runs on a successful re-render against the
	// dispatched AppliedConfig, simulating the broker's echo.
	rerenderEcho func(cfg *store.AgentAppliedConfig)
	// reprovisionConfigs snapshots the AppliedConfig of every reprovision
	// dispatch, in call order, as the worker sent it.
	reprovisionConfigs []store.AgentAppliedConfig
}

func newReincarnateTestDispatcher() *reincarnateTestDispatcher {
	return &reincarnateTestDispatcher{}
}

// waitForReincarnationSettled polls the store until the agent's most recent
// AgentReincarnation record reaches a terminal state (completed or failed) —
// the LAST write the worker makes on either path — or fails the test after
// timeout. The worker runs in a detached background goroutine (design §3.1),
// so tests synchronize on persisted store state, the actual observable
// outcome, rather than on dispatcher call timing: the failure path's
// UpdateAgent/UpdateAgentReincarnation calls happen strictly after
// DispatchAgentStart returns its error, so signaling on the dispatcher call
// itself would race the worker's own post-dispatch bookkeeping.
// waitForReincarnationSettled waits until BOTH the record and the agent row
// have reached a terminal, consistent state (design §3.4 Amendment A8.3): the
// record must be completed/failed, the agent's reincarnation_state must be
// back to ""/failed, and — for a completed record specifically — the agent's
// Generation must have reached rec.ToGeneration. Waiting on the record alone
// is not enough: the A6 CAS ordering always advances the record BEFORE the
// matching agent-row write (see tryAdvanceReincarnation's callers), so a
// caller that reads the agent row right after this returned could still
// observe it mid-transition — for example still "starting" a moment after
// the record already reads "completed". This raced several tests
// intermittently before the two waits were tied together.
func waitForReincarnationSettled(t *testing.T, s store.Store, agentID string) *store.AgentReincarnation {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for {
		list, err := s.ListAgentReincarnations(ctx, agentID)
		require.NoError(t, err)
		if len(list) > 0 {
			rec := list[0]
			switch rec.State {
			case store.AgentReincarnationStateCompleted, store.AgentReincarnationStateFailed:
				agent, err := s.GetAgent(ctx, agentID)
				require.NoError(t, err)
				agentSettled := agent.ReincarnationState == store.ReincarnationStateNone || agent.ReincarnationState == store.ReincarnationStateFailed
				generationSettled := rec.State != store.AgentReincarnationStateCompleted || agent.Generation >= rec.ToGeneration
				if agentSettled && generationSettled {
					return rec
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for reincarnation to settle for agent %s", agentID)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// setupReincarnateTestServer creates a project, an online broker with the
// reprovision capability, and wires the given dispatcher.
func setupReincarnateTestServer(t *testing.T, disp AgentDispatcher) (*Server, store.Store, *store.Project, *store.RuntimeBroker) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("reincarnate-project-" + t.Name()),
		Name: "Reincarnate Test Project",
		Slug: "reincarnate-test-project-" + tidSlugSafe(t.Name()),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:           tid("reincarnate-broker-" + t.Name()),
		Name:         "Reincarnate Test Broker",
		Slug:         "reincarnate-test-broker-" + tidSlugSafe(t.Name()),
		Status:       store.BrokerStatusOnline,
		Capabilities: &store.BrokerCapabilities{Reprovision: true},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	provider := &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddProjectProvider(ctx, provider))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	srv.SetDispatcher(disp)
	return srv, s, project, broker
}

// newReincarnateTestAgent creates a fully-formed agent (with CreateInputs, as
// the create path would leave it) ready for a reincarnate test.
func newReincarnateTestAgent(t *testing.T, s store.Store, project *store.Project, broker *store.RuntimeBroker, mutate func(a *store.Agent)) *store.Agent {
	t.Helper()
	ctx := context.Background()
	// The agent's creator is a live project member, so the agent is in good
	// standing (ptone/scion#3433).
	ensureStandingRoot(t, s, project.ID, tid("user-creator"))

	a := &store.Agent{
		ID:              tid("reincarnate-agent-" + t.Name()),
		Slug:            "reincarnate-agent-" + tidSlugSafe(t.Name()),
		Name:            "Reincarnate Test Agent",
		Template:        "",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           "running",
		CreatedBy:       tid("user-creator"),
		OwnerID:         tid("user-creator"),
		Ancestry:        []string{tid("user-creator")},
		MessageMode:     "project",
		Labels:          map[string]string{"team": "platform"},
		AppliedConfig: &store.AgentAppliedConfig{
			Image:       "old-image:v1",
			HarnessAuth: "api-key",
			CreatorName: "user-creator",
			AgentRole:   "baseline",
			Workspace:   "/tmp/reincarnate-workspace",
			// GitClone: clone-per-agent, the only workspace mode Phase 1
			// supports (design §3.4 Amendment A2); a test that wants the
			// non-clone-per-agent rejection path sets this to nil explicitly.
			GitClone: &api.GitCloneConfig{URL: "https://example.com/reincarnate-test-repo.git"},
			CreateInputs: &store.AgentCreateInputs{
				Workspace: "/tmp/reincarnate-workspace",
			},
		},
	}
	if mutate != nil {
		mutate(a)
	}
	require.NoError(t, s.CreateAgent(ctx, a))
	return a
}

func agentIdentityFor(agentID, projectID string, scopes ...AgentTokenScope) AgentIdentity {
	return &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
		Scopes:    scopes,
	}}
}

// delegatingRequesterFor returns an agent identity for requesterID that may
// reincarnate a baseline agent in projectID: the lifecycle scope plus every
// scope of the baseline role, which a requester other than the agent must
// hold to delegate the role (CanDelegate).
func delegatingRequesterFor(requesterID, projectID string) AgentIdentity {
	return agentIdentityFor(requesterID, projectID, append(ScopesForRole(AgentRoleBaseline), ScopeAgentLifecycle)...)
}

func reincarnateRequest(t *testing.T, agentID string, identity Identity, body interface{}) *http.Request {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/reincarnate", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	if identity != nil {
		req = req.WithContext(contextWithIdentity(req.Context(), identity))
	}
	return req
}

// blockingStopDispatcher blocks the first Stop call until released, so a
// test can pause a worker mid-flight and run a sweep concurrently.
type blockingStopDispatcher struct {
	*reincarnateTestDispatcher
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

// softDeleteAgent soft-deletes f.target via the ordinary DELETE route,
// forcing the soft-delete branch (SoftDeleteRetention > 0, no force) rather
// than relying on server defaults. Returns once the row is confirmed to
// carry a non-zero DeletedAt.
func softDeleteAgent(t *testing.T, f *projectAgentAuthzFixture) {
	t.Helper()
	f.srv.config.SoftDeleteRetention = 24 * time.Hour

	rec := doRequestAsUser(t, f.srv, f.member, http.MethodDelete, f.targetPath(), nil)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, rec.Code,
		"DELETE body: %s", rec.Body.String())

	deleted, err := f.store.GetAgent(context.Background(), f.target.ID)
	require.NoError(t, err)
	require.False(t, deleted.DeletedAt.IsZero(), "expected a soft delete, not a hard delete")
}

// seedSecret creates a secret in the backend for testing.
func seedSecret(t *testing.T, backend secret.SecretBackend, key, value, secretType, target, projectID string) {
	t.Helper()
	ctx := context.Background()
	if secretType == "" {
		secretType = store.SecretTypeEnvironment
	}
	if target == "" {
		target = key
	}
	input := &secret.SetSecretInput{
		Name:       key,
		Value:      value,
		SecretType: secretType,
		Target:     target,
		Scope:      store.ScopeProject,
		ScopeID:    projectID,
		CreatedBy:  "test-user",
		UpdatedBy:  "test-user",
	}
	_, _, err := backend.Set(ctx, input)
	if err != nil {
		t.Fatalf("failed to seed secret %q: %v", key, err)
	}
}

func setupAgentSecretTest(t *testing.T) (*Server, store.Store, string, string, string) {
	t.Helper()
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	projectID := tid("project-agent-secret")
	project := &store.Project{
		ID: projectID, Name: "Agent Secret Project", Slug: "agent-secret-project",
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agentID := tid("agent-secret-1")
	agent := &store.Agent{
		ID: agentID, Slug: "secret-agent", Name: "Secret Agent",
		ProjectID: projectID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	agentToken, err := srv.agentTokenService.GenerateAgentToken(agentID, projectID, nil, nil)
	if err != nil {
		t.Fatalf("failed to generate agent token: %v", err)
	}

	return srv, s, agentID, projectID, agentToken
}

// setupOfflineBrokerAgent creates a project, an offline broker, and an agent assigned to that broker.
func setupOfflineBrokerAgent(t *testing.T, s store.Store, suffix string) (*store.Project, *store.RuntimeBroker, *store.Agent) {
	t.Helper()
	ctx := context.Background()

	project := &store.Project{
		ID:   tid(fmt.Sprintf("project-offline-%s", suffix)),
		Name: fmt.Sprintf("Offline Project %s", suffix),
		Slug: fmt.Sprintf("offline-project-%s", suffix),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:     tid(fmt.Sprintf("broker-offline-%s", suffix)),
		Name:   fmt.Sprintf("Offline Broker %s", suffix),
		Slug:   fmt.Sprintf("offline-broker-%s", suffix),
		Status: store.BrokerStatusOffline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID:              tid(fmt.Sprintf("agent-offline-%s", suffix)),
		Slug:            fmt.Sprintf("agent-offline-%s-slug", suffix),
		Name:            fmt.Sprintf("Agent Offline %s", suffix),
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	return project, broker, agent
}

// deleteDispatcher tracks whether DispatchAgentDelete was called and can simulate errors.
type deleteDispatcher struct {
	createAgentDispatcher
	deleteErr        error
	deleteCalls      int
	lastDeleteFiles  bool
	lastRemoveBranch bool
}

// setupOnlineBrokerAgent creates a project, an online broker, and an agent assigned to that broker.
func setupOnlineBrokerAgent(t *testing.T, s store.Store, suffix string) (*store.Project, *store.RuntimeBroker, *store.Agent) {
	t.Helper()
	ctx := context.Background()

	project := &store.Project{
		ID:   tid(fmt.Sprintf("project-online-%s", suffix)),
		Name: fmt.Sprintf("Online Project %s", suffix),
		Slug: fmt.Sprintf("online-project-%s", suffix),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:       tid(fmt.Sprintf("broker-online-%s", suffix)),
		Name:     fmt.Sprintf("Online Broker %s", suffix),
		Slug:     fmt.Sprintf("online-broker-%s", suffix),
		Status:   store.BrokerStatusOnline,
		Endpoint: "http://localhost:9800",
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID:              tid(fmt.Sprintf("agent-online-%s", suffix)),
		Slug:            fmt.Sprintf("agent-online-%s-slug", suffix),
		Name:            fmt.Sprintf("Agent Online %s", suffix),
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	return project, broker, agent
}

// createAgentDispatcher is a mock dispatcher for createAgent handler tests.
// It allows controlling the status that DispatchAgentCreate reports back.
type createAgentDispatcher struct {
	createPhase   string // status to set on agent during DispatchAgentCreate
	createRuntime string
	createStatus  string
	envReqs       *RemoteEnvRequirementsResponse
	deleteCalled  bool
	deleteErr     error
	startCalled   bool
	execOutput    string
	execExitCode  int
	// capturedAgent records the agent passed to DispatchAgentCreate, so tests
	// that need the create-time agent.ID (e.g. to check quota reservations,
	// ptone/scion#1986) can read it back after the HTTP response, which for
	// a failure path never echoes the ID.
	capturedAgent *store.Agent
	logsErr       error
}

// failingCreateDispatcher is a mock dispatcher whose DispatchAgentCreateWithGather
// always returns an error, simulating a broker-side failure (e.g. auth resolution error).
// It tracks whether DispatchAgentDelete is called so tests can verify cleanup behaviour.
type failingCreateDispatcher struct {
	createAgentDispatcher
	createErr         error
	deleteCalledFiles bool
	deleteBranch      bool
}

// setupCreateAgentServer creates a test server with a dispatcher and a project+broker ready for agent creation.
func setupCreateAgentServer(t *testing.T, disp AgentDispatcher) (*Server, store.Store, *store.Project) {
	t.Helper()
	srv, s := testServer(t)
	project := setupCreateAgentProject(t, s)
	srv.SetDispatcher(disp)
	return srv, s, project
}

// setupCreateAgentProject seeds, through the raw store s, the project and
// online default broker that setupCreateAgentServer uses. Fixtures that
// install a store fault wrapper first (installStoreFault) call it on the
// server they built.
func setupCreateAgentProject(t *testing.T, s store.Store) *store.Project {
	t.Helper()
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("project-create"),
		Name: "Create Test Project",
		Slug: "create-test-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:     tid("broker-create"),
		Name:   "Create Test Broker",
		Slug:   "create-test-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	provider := &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddProjectProvider(ctx, provider))

	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))
	return project
}

// postAgentStatusAsAgent POSTs a status update for agent using an agent
// token with ScopeAgentStatusUpdate, the way sciontool does.
func postAgentStatusAsAgent(t *testing.T, srv *Server, agent *store.Agent, body string) *httptest.ResponseRecorder {
	t.Helper()
	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)
	token, err := tokenSvc.GenerateAgentToken(agent.ID, agent.ProjectID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/status", bytes.NewReader([]byte(body)))
	req.Header.Set("X-Scion-Agent-Token", token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// activeEdgesFor returns the active project edges for an agent.
func activeEdgesFor(t *testing.T, s store.Store, agentID string) []*store.DelegationEdge {
	t.Helper()
	edges, err := s.GetDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, agentID)
	require.NoError(t, err)
	var active []*store.DelegationEdge
	for _, e := range edges {
		if e.Active {
			active = append(active, e)
		}
	}
	return active
}

// assertEdgeDelegatorIsSourcePrincipal asserts that the edge's delegator is
// the principal its recorded provenance names as the source.
func assertEdgeDelegatorIsSourcePrincipal(t *testing.T, e *store.DelegationEdge) {
	t.Helper()
	require.NotEmpty(t, e.DelegatorID)
	assert.Equal(t, e.SourcePrincipalKind, e.DelegatorType, "delegator type is the source principal kind")
	assert.Equal(t, e.SourcePrincipalID, e.DelegatorID, "delegator ID is the source principal ID")
}

// hubScopedSAForAgent registers a hub-scoped service account created by a
// stranger. The creator is load-bearing: gcpServiceAccountResource sets
// OwnerID from CreatedBy, so seeding the account under the caller would
// satisfy the assign-time authorization through the resource-owner
// short-circuit and the test would pass without ever exercising scope. (That
// exact mistake produced a false pass earlier in P4.)
func hubScopedSAForAgent(t *testing.T, f *bypassAgentsFixture, verified bool) *store.GCPServiceAccount {
	t.Helper()
	return hubScopedSACreatedBy(t, f, tid("a-stranger"), verified)
}

// hubScopedSACreatedBy is the same, with the creator named. Who created the
// account is not bookkeeping here: the creator is one of the two principals
// §8.2 permits to assign it, and they are admitted through the resource-owner
// bypass, so this parameter selects between the admitted and refused cases.
func hubScopedSACreatedBy(t *testing.T, f *bypassAgentsFixture, creator string, verified bool) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID:    uuid.New().String(),
		Scope: store.ScopeHub,
		// Provenance only. Nothing may compare this against the hub ID; the
		// predicate keys on Scope alone.
		ScopeID:   "some-hub-instance",
		Email:     fmt.Sprintf("hub-sa-%s@proj.iam.gserviceaccount.com", uuid.New().String()[:8]),
		ProjectID: "gcp-proj",
		CreatedBy: creator,
		Verified:  verified,
		CreatedAt: time.Now(),
	}
	require.NoError(t, f.store.CreateGCPServiceAccount(context.Background(), sa))
	return sa
}

// hubAdminUser creates a hub administrator. Admins reach a hub-scoped account
// through the admin bypass, which is a different mechanism from the creator's
// resource-owner bypass — hence a distinct principal rather than a variation
// of the same one.
func hubAdminUser(t *testing.T, f *bypassAgentsFixture) *store.User {
	t.Helper()
	ctx := context.Background()
	u := &store.User{
		ID:          tid("hub-admin"),
		Email:       "hub-admin@example.com",
		DisplayName: "Hub Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, f.store.CreateUser(ctx, u))
	// CO1: Admin access requires a role binding; the role field alone is not enough.
	rd, err := f.store.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      u.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)
	return u
}

// createAgentAsOwner posts to the project agent route as the project owner.
//
// It first materialises the project's members group and grants the owner a
// project-owner role binding. The bypassAgents fixture builds its projects
// directly in the store, so neither the group nor the role binding that the
// project create handler would have made exists, and without them the owner
// has no rights over the project at all — agent create is refused before any
// service-account logic runs. Those tests never noticed because their callers
// are agents; these tests use a human caller, which is the realistic one for
// picking a hub-wide account. Both calls are idempotent.
func createAgentAsOwner(t *testing.T, f *bypassAgentsFixture, req CreateAgentRequest) *httptest.ResponseRecorder {
	t.Helper()
	f.srv.seedProjectCreatorMembership(context.Background(), f.proj)
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(context.Background(), f.proj.ID, f.owner.ID))
	return doRequestAsUser(t, f.srv, f.owner, http.MethodPost,
		"/api/v1/projects/"+f.proj.ID+"/agents", req)
}

// pendingAgentForPatch creates an agent in the 'created' phase, the only phase
// in which the PATCH path will touch GCP identity.
func pendingAgentForPatch(t *testing.T, f *bypassAgentsFixture, name string) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID:        uuid.New().String(),
		Slug:      name,
		Name:      name,
		ProjectID: f.proj.ID,
		Phase:     string(state.PhaseCreated),
		CreatedBy: f.owner.ID,
		OwnerID:   f.owner.ID,
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	return a
}

func patchAgentSAAsOwner(t *testing.T, f *bypassAgentsFixture, agentID, saID string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+agentID,
		map[string]interface{}{
			"gcp_identity": map[string]interface{}{
				"metadata_mode":      store.GCPMetadataModeAssign,
				"service_account_id": saID,
			},
		})
}

// setProjectDefaultSA configures the project's default GCP identity through
// the real settings route, and requires the route to ACCEPT it.
//
// This helper originally existed to demonstrate the opposite. Its comment read
// "nothing validates the service account ID on the way in... Site 3's failure
// was live, not latent," and that was true and correctly evidenced when it was
// written: the settings PUT wrote the ID unchecked. Defect #22 added write-time
// validation, so the demonstration no longer holds and the helper has inverted
// meaning — it now shows which defaults the PUT still admits.
//
// Only valid defaults may be set through here. A VERIFIED HUB-SCOPED account is
// one of them, deliberately: ReachableFromProject's ScopeHub arm returns true
// unconditionally (pkg/store/models.go:1552), because a hub-scoped account is
// legitimately pickable from any project. That is why this helper still has a
// caller. For defaults the PUT now refuses, see setStaleProjectDefaultSA.
func setProjectDefaultSA(t *testing.T, f *bypassAgentsFixture, saID string) {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPut,
		"/api/v1/projects/"+f.proj.ID+"/settings",
		map[string]interface{}{
			"defaultGCPIdentityMode":             store.GCPMetadataModeAssign,
			"defaultGCPIdentityServiceAccountID": saID,
		})
	require.Equal(t, http.StatusOK, rec.Code,
		"this default must remain settable through the API; got: %s", rec.Body.String())
}

// setStaleProjectDefaultSA writes the default-identity annotations straight to
// the store, bypassing the settings route.
//
// IT BYPASSES HTTP ON PURPOSE, NOT FOR CONVENIENCE. Since defect #22 the
// settings PUT validates the service account ID, so the states constructed here
// CANNOT be reached through the API at all — that is what #22 did. Do not
// "tidy" these callers back onto setProjectDefaultSA: they will fail, and the
// failure would misread as the API having regressed. If a caller's default is
// one the PUT should accept, it belongs on the HTTP helper instead, so that the
// acceptance stays covered at the API boundary.
//
// THE SCENARIOS ARE NOT DEAD, SO DO NOT DELETE THEM AS UNREACHABLE. A future
// reader can correctly observe that no API path produces this state and
// conclude the tests below are vestigial. They are not. The state arises in
// production as a STALE value: an account that was validly set as the default
// and was afterwards deleted, un-verified, or moved out of reach. #22 closed
// the write; it cannot close the drift, because the drift happens after the
// write. Guarding consumption against that is the separately-owned work on
// handlers_agents_core.go:655.
//
// Distinct from the inline annotation write in authz_bypass_agents_test.go:862,
// which constructs a VALID default directly. That one could go through the API;
// these cannot. Keeping the two apart is the point.
func setStaleProjectDefaultSA(t *testing.T, f *bypassAgentsFixture, saID string) {
	t.Helper()
	ctx := context.Background()
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModeAssign
	proj.Annotations[projectSettingDefaultGCPIdentitySAID] = saID
	require.NoError(t, f.store.UpdateProject(ctx, proj))
}

// createdAgentIdentity creates an agent with no explicit GCP identity and
// returns the identity the project default produced.
func createdAgentIdentity(t *testing.T, f *bypassAgentsFixture, name string) *store.GCPIdentityConfig {
	t.Helper()
	got := createdAgentIdentityOrNil(t, f, name)
	require.NotNil(t, got, "agent should have a resolved GCP identity")
	return got
}

// createdAgentIdentityOrNil is createdAgentIdentity's nil-tolerant twin, for
// the rungs of the ladder that deliberately leave AppliedConfig.GCPIdentity
// unset — nothing configured at all (at either the project or hub level), or
// a hub-default passthrough grant denied (e.g. non-embedded broker, or a
// runtime profile the grant does not cover) — so the broker can apply its
// own runtime-aware default (ptone/scion#2328) rather than an explicit
// "block" record.
func createdAgentIdentityOrNil(t *testing.T, f *bypassAgentsFixture, name string) *store.GCPIdentityConfig {
	t.Helper()
	rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: name})
	require.Equal(t, http.StatusCreated, rec.Code,
		"agent creation should succeed; got: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	got, err := f.store.GetAgent(context.Background(), resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig, "agent should have applied config")
	return got.AppliedConfig.GCPIdentity
}

// skillFailDispatcher is a createAgentDispatcher whose provision and start
// dispatches can be made to fail with a chosen error.
type skillFailDispatcher struct {
	createAgentDispatcher
	provisionErr     error
	startErr         error
	createErr        error
	provisionedAgent *store.Agent
}

// brokerSkillError builds the error the broker transport returns for a
// required skill the broker could not resolve.
func brokerSkillError(status int, cause, retryAfter string) error {
	body := `{"error":{"code":"skill_resolution_failed","message":"Failed to provision agent: required skill \"` +
		testSkillRef + `\" could not be resolved: ` + cause + `","details":{"skill":"` + testSkillRef + `","cause":"` + cause + `"}}}`
	return &brokerStatusError{StatusCode: status, Body: body, RetryAfter: retryAfter}
}

// createLifecycleTestAgent stores an agent on the test broker in the given phase.
func createLifecycleTestAgent(t *testing.T, s store.Store, project *store.Project, name string, phase state.Phase) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID:              tid("agent-" + name),
		Slug:            name,
		Name:            name,
		ProjectID:       project.ID,
		RuntimeBrokerID: tid("broker-create"),
		Phase:           string(phase),
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))
	return agent
}

func scopeCapsHasAction(caps *Capabilities, action string) bool {
	for _, a := range caps.Actions {
		if a == action {
			return true
		}
	}
	return false
}

// newLoginGrantServer returns a server backed by a real, seeded store and
// configured with the given default role and admin emails. Login-time
// demotion is enabled (as after a successful startup reconcile).
func newLoginGrantServer(t *testing.T, defaultRole string, adminEmails []string) (*Server, store.Store) {
	t.Helper()
	srv, s := testServer(t)
	srv.config.DefaultUserRole = defaultRole
	srv.config.AdminEmails = adminEmails
	srv.config.UserAccessMode = "open"
	srv.demotionSafe.Store(true)
	return srv, s
}

// createLoginGrantUser creates a user row directly in the store.
func createLoginGrantUser(t *testing.T, s store.Store, id, email, role, status string) *store.User {
	t.Helper()
	u := &store.User{
		ID:          tid(id),
		Email:       email,
		DisplayName: id,
		Role:        role,
		Status:      status,
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(context.Background(), u))
	return u
}

// createSystemBinding creates a system-scoped role binding for the user.
func createSystemBinding(t *testing.T, s store.Store, userID, roleName, createdBy string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        createdBy,
	})
	require.NoError(t, err)
}

// webLoginSettings returns live access settings for the web login tests.
func webLoginSettings(defaultRole string, adminEmails ...string) *staticAccessSettings {
	return &staticAccessSettings{adminEmails: adminEmails, defaultUserRole: defaultRole}
}

// webLoginPaths runs each web login test against both web login paths.
var webLoginPaths = []struct {
	name  string
	login func(t *testing.T, s store.Store, settings *staticAccessSettings, email string, opts ...func(*WebServer))
}{
	{"proxy", webProxyLogin},
	{"oauth", webOAuthLogin},
}

func grantUserActionOnResource(t *testing.T, s store.Store, userID, resourceType, resourceID string, action Action) {
	t.Helper()
	ctx := context.Background()

	// CO1: Policies no longer work. Use system-scoped role bindings instead.
	// This grants the permission hub-wide (not per-resource); callers that need
	// per-resource isolation should use project-scoped bindings instead.
	permissionID := resourceType + "." + string(action)
	roleName := "test-grant-" + userID + "-" + resourceType + "-" + resourceID + "-" + string(action)
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        roleName,
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{permissionID},
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// brokerAssocFixture is a project owned by projectOwner with no providers
// and no default broker, a broker owned by brokerOwner (a hub member with
// no binding on the project), and a second broker owned by projectOwner.
type brokerAssocFixture struct {
	srv          *Server
	store        store.Store
	project      *store.Project
	projectOwner *store.User
	brokerOwner  *store.User
	// otherBroker is owned by brokerOwner.
	otherBroker *store.RuntimeBroker
	// ownBroker is owned by projectOwner.
	ownBroker *store.RuntimeBroker
}

func brokerAssocSetup(t *testing.T, name string) *brokerAssocFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	f := &brokerAssocFixture{srv: srv, store: s}

	projectID := tid(name + "-project")
	ownerID := tid(name + "-project-owner")
	createRS1Project(t, s, projectID, ownerID)
	var err error
	f.project, err = s.GetProject(ctx, projectID)
	require.NoError(t, err)
	f.projectOwner, err = s.GetUser(ctx, ownerID)
	require.NoError(t, err)

	f.brokerOwner = newHubMemberUser(t, s, name+"-broker-owner")
	f.otherBroker = createReregistrationTestBroker(t, s, name+"-other-broker", f.brokerOwner.ID)
	f.ownBroker = createReregistrationTestBroker(t, s, name+"-own-broker", f.projectOwner.ID)
	return f
}

func assertNoProvider(t *testing.T, s store.Store, projectID, brokerID string) {
	t.Helper()
	_, err := s.GetProjectProvider(context.Background(), projectID, brokerID)
	assert.True(t, errors.Is(err, store.ErrNotFound), "no provider row expected, got %v", err)
}

func assertDefaultBroker(t *testing.T, s store.Store, projectID, want string) {
	t.Helper()
	p, err := s.GetProject(context.Background(), projectID)
	require.NoError(t, err)
	assert.Equal(t, want, p.DefaultRuntimeBrokerID)
}

func installBrokerAuditCapture(srv *Server) *mockAuditLogger {
	m := &mockAuditLogger{}
	srv.SetAuditLogger(m)
	return m
}

func brokerAuditEventsOfType(m *mockAuditLogger, eventType BrokerAuthEventType) []*BrokerAuthEvent {
	var out []*BrokerAuthEvent
	for _, e := range m.brokerEvents {
		if e.EventType == eventType {
			out = append(out, e)
		}
	}
	return out
}

// setBrokerAutoProvide writes the auto-provide setting of a broker directly
// in the store.
func setBrokerAutoProvide(t *testing.T, s store.Store, brokerID string, on bool) {
	t.Helper()
	ctx := context.Background()
	broker, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	broker.AutoProvide = on
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))
}

// mintHubBrokerUAT mints a hub-boundary user access token for userID with
// the given scopes and returns its key.
func mintHubBrokerUAT(t *testing.T, srv *Server, userID string, scopes ...string) string {
	t.Helper()
	key, token, err := srv.uatService.CreateTokenWithParams(rs4MintContext(userID), CreateTokenParams{
		UserID:   userID,
		Name:     "hub-broker-token",
		Boundary: TokenBoundary{Kind: BoundaryKindHub},
		Scopes:   scopes,
	})
	require.NoError(t, err)
	require.Equal(t, string(BoundaryKindHub), token.BoundaryKind)
	return key
}

func decodeBrokerRegistration(t *testing.T, body *bytes.Buffer) CreateBrokerRegistrationResponse {
	t.Helper()
	var resp CreateBrokerRegistrationResponse
	require.NoError(t, json.NewDecoder(body).Decode(&resp))
	return resp
}

// def135Fixture sets up a standard user, project, and running agent for
// DEF-135 tests. Returns the server, store, dispatcher, and fixture IDs.
type def135Fixture struct {
	srv        *Server
	store      store.Store
	dispatcher *def135Dispatcher
	user       *store.User
	project    *store.Project
	agent      *store.Agent
	topic      string
	senderRef  string
}

func setupDEF135(t *testing.T) def135Fixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("user-def135"),
		Email:       "def135@example.com",
		DisplayName: "DEF-135 Test User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("proj-def135"),
		Slug:      "def135-proj",
		Name:      "DEF-135 Test Project",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)

	agent := &store.Agent{
		ID:           tid("agent-def135"),
		Slug:         "def135-agent",
		Name:         "DEF-135 Agent",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	dispatcher := &def135Dispatcher{}
	srv.SetDispatcher(dispatcher)
	enableWriteDenySwitch(t, srv)

	topic := "scion.project." + project.ID + ".agent." + agent.Slug + ".messages"
	senderRef := "user:" + user.Email

	return def135Fixture{
		srv:        srv,
		store:      s,
		dispatcher: dispatcher,
		user:       user,
		project:    project,
		agent:      agent,
		topic:      topic,
		senderRef:  senderRef,
	}
}

// listConversationIDsAsUser calls GET /api/v1/conversations as the given user
// and returns the listed conversation IDs.
func listConversationIDsAsUser(t *testing.T, srv *Server, user *store.User) []string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations", nil)
	req = req.WithContext(contextWithIdentity(req.Context(),
		NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")))
	rr := httptest.NewRecorder()
	srv.handleListConversations(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "list body: %s", rr.Body.String())
	var result conversationListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	ids := make([]string, 0, len(result.Conversations))
	for _, c := range result.Conversations {
		ids = append(ids, c.ID)
	}
	return ids
}

func sortedStrings(s []string) []string {
	c := make([]string, len(s))
	copy(c, s)
	sort.Strings(c)
	return c
}

// routedTestEnv holds the common test fixtures for routed inbound tests.
type routedTestEnv struct {
	srv          *Server
	store        store.Store
	dispatcher   *recordingDispatcher
	webChatStore WebChatStore
	user         *store.User
	project      *store.Project
	agent1       *store.Agent // "alpha" — running, project mode
	agent2       *store.Agent // "beta" — running, project mode
	agent3       *store.Agent // "gamma" — stopped
}

func setupRoutedTestEnv(t *testing.T) routedTestEnv {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	// Wire a recording dispatcher so dispatch tests exercise the full path.
	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	// Enable the envelope switch for DeliveryText assertions.
	enableWriteDenySwitch(t, srv)

	// Wire webChatStore for reply affinity assertions.
	wcsDB := openTestMemorySQLite(t, "sqlite")
	wcs := NewWebChatStore(wcsDB, "sqlite")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)

	// Create a project owner (separate from the test sender).
	owner := &store.User{
		ID:          tid("owner-routed"),
		Email:       "owner-routed@example.com",
		DisplayName: "Routed Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	// Create a separate sender user (non-owner).
	user := &store.User{
		ID:          tid("user-routed"),
		Email:       "routed@example.com",
		DisplayName: "Routed User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("proj-routed"),
		Slug:      "routed-proj",
		Name:      "Routed Test Project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)
	msgAuthzAddProjectMember(t, s, user.ID, project.ID, project.Slug, store.GroupMemberRoleMember)

	// Grant explicit agent.message send authority. Upstream removed
	// agent.message from the project-member role; tests must not rely on
	// that implicit grant. The negative tests (InactiveUser, UnknownSender)
	// still verify denial for users without this explicit binding.
	msgAuthzGrantAgentMessage(t, s, user.ID, project.ID)

	agent1 := &store.Agent{
		ID:           tid("agent-alpha"),
		Slug:         "alpha",
		Name:         "Alpha Agent",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent1))

	agent2 := &store.Agent{
		ID:           tid("agent-beta"),
		Slug:         "beta",
		Name:         "Beta Agent",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent2))

	agent3 := &store.Agent{
		ID:           tid("agent-gamma"),
		Slug:         "gamma",
		Name:         "Gamma Agent",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseStopped),
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent3))

	return routedTestEnv{
		srv:          srv,
		store:        s,
		dispatcher:   dispatcher,
		webChatStore: wcs,
		user:         user,
		project:      project,
		agent1:       agent1,
		agent2:       agent2,
		agent3:       agent3,
	}
}

// newOnboardingSigningBroker inserts a broker with an active HMAC secret
// and returns it with the secret key, for signing requests as that broker.
func newOnboardingSigningBroker(t *testing.T, s store.Store, name string) (*store.RuntimeBroker, []byte) {
	t.Helper()
	b := &store.RuntimeBroker{ID: tid("onboarding-signer-" + name), Name: "Onboarding Signer " + name, Slug: "onboarding-signer-" + name}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), b))
	return b, seedBrokerSecret(t, s, b.ID)
}

// doBrokerSignedRequest sends an HMAC-signed request as brokerID through
// the full handler chain. When onBehalfOfEmail is non-empty the request
// carries an X-Scion-On-Behalf-Of header naming that user.
func doBrokerSignedRequest(t *testing.T, srv *Server, brokerID string, key []byte, onBehalfOfEmail, method, path string, body interface{}) *httptest.ResponseRecorder {
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
	if onBehalfOfEmail != "" {
		req.Header.Set(HeaderOnBehalfOf, "user:"+onBehalfOfEmail)
	}
	require.NoError(t, srv.brokerAuthService.SignRequest(req, brokerID, key))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// newPlainUser creates a bare "member"-role user with no group memberships
// and no role bindings — no hub-members catalog access, no broker
// permission of any kind. This represents the boundary the gate actually
// enforces: a caller who is authenticated but holds none of the allowed
// grants (super-admin, broker-self, or CreatedBy).
func newPlainUser(t *testing.T, s store.Store, name string) *store.User {
	t.Helper()
	u := &store.User{
		ID:          tid(name),
		Email:       name + "@test.com",
		DisplayName: name,
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(context.Background(), u))
	return u
}

// newHubMemberUser creates an ordinary hub-members-group member, exercising
// the real curated hub-member role (which carries broker.read) rather than
// the synthetic single-permission role from grantSystemBrokerReadPermission.
func newHubMemberUser(t *testing.T, s store.Store, name string) *store.User {
	t.Helper()
	u := newPlainUser(t, s, name)
	ensureHubMembership(context.Background(), s, u.ID)
	return u
}

// newSuperAdminUser creates a user with a system-scoped super-admin role
// binding, independent of hub-members catalog membership.
func newSuperAdminUser(t *testing.T, s store.Store, name string) *store.User {
	t.Helper()
	userID := tid(name)
	createTestUserWithRole(t, s, userID, name+"@test.com", "admin", store.SystemRoleSuperAdmin)
	u, err := s.GetUser(context.Background(), userID)
	require.NoError(t, err)
	return u
}

// createReregistrationTestBroker inserts a broker directly into the store,
// skipping HTTP registration, so no join token exists for it yet.
func createReregistrationTestBroker(t *testing.T, s store.Store, name, createdBy string) *store.RuntimeBroker {
	t.Helper()
	broker := &store.RuntimeBroker{
		ID:          tid("reregistration-broker-" + name),
		Name:        name,
		Slug:        slugify(name),
		Status:      store.BrokerStatusOffline,
		AutoProvide: false,
		Labels:      map[string]string{"env": "baseline"},
		Created:     time.Now(),
		Updated:     time.Now(),
		CreatedBy:   createdBy,
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), broker))
	return broker
}

// assertBrokerUnchanged re-reads the broker and confirms none of the
// registration-mutable fields moved, and that no join token was created.
func assertBrokerUnchanged(t *testing.T, s store.Store, brokerID string) {
	t.Helper()
	ctx := context.Background()

	broker, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	assert.False(t, broker.AutoProvide, "AutoProvide must not be flipped by a denied re-registration")
	assert.Equal(t, "baseline", broker.Labels["env"], "labels must not be overwritten by a denied re-registration")
	assert.Empty(t, broker.GCPHostServiceAccountEmail, "GCP host identity must not be set by a denied re-registration")

	_, err = s.GetJoinTokenByBrokerID(ctx, brokerID)
	assert.True(t, errors.Is(err, store.ErrNotFound), "no join token should exist after a denied re-registration; got err=%v", err)
}

// seedBrokerSecret creates an active HMAC secret for brokerID directly in
// the store and returns the key bytes, so rotate-secret tests can assert
// whether that value moved.
func seedBrokerSecret(t *testing.T, s store.Store, brokerID string) []byte {
	t.Helper()
	key := []byte("test-fixture-secret-key-0123456789ab")
	require.NoError(t, s.CreateBrokerSecret(context.Background(), &store.BrokerSecret{
		BrokerID:  brokerID,
		SecretKey: key,
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		CreatedAt: time.Now(),
		Status:    store.BrokerSecretStatusActive,
	}))
	return key
}

func rotateSecretAsUser(t *testing.T, srv *Server, user *store.User, brokerID string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	if user == nil {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/brokers/"+brokerID+"/rotate-secret", nil)
		srv.handleBrokerRotateSecret(rec, req, brokerID)
		return rec
	}
	return doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/brokers/"+brokerID+"/rotate-secret", nil)
}

// setupMutePinTest builds a server with a web chat store, one project and one
// topic in it, owned by nobody in particular — the dev-auth user is an admin so
// it can read the project.
func setupMutePinTest(t *testing.T) (*Server, store.Store, WebChatStore, *store.Project, string) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{
		ID: tid("mutepin-proj"), Name: "mutepin", Slug: "mutepin",
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	// The dev user is a member: chat tracks unread only in member projects.
	bindProjectMember(t, s, proj.ID, DevUserID)

	db := openTestMemorySQLite(t, "sqlite")
	wcs := NewWebChatStore(db, "sqlite")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv.SetWebChatStore(wcs)

	topicID := tid("mutepin-topic")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        topicID,
		ProjectID: proj.ID,
		Name:      "mutable",
		CreatedBy: DevUserID,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	return srv, s, wcs, proj, topicID
}

// setupChatAuthzTest builds a server whose project has a real members group and
// policy, so a user outside the project is genuinely refused rather than
// waved through by the dev-auth admin.
func setupChatAuthzTest(t *testing.T) (*Server, store.Store, WebChatStore, *store.Project) {
	t.Helper()
	srv, s, _, _, project := setupDemoPolicyTest(t)

	db := openTestMemorySQLite(t, "sqlite")
	wcs := NewWebChatStore(db, "sqlite")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv.SetWebChatStore(wcs)

	return srv, s, wcs, project
}

// unreadFixture seeds threads and DMs for the unread-count tests.
type unreadFixture struct {
	t    *testing.T
	s    store.Store
	wcs  WebChatStore
	proj *store.Project
	seq  int
}

func getUnreadCount(t *testing.T, rec *httptest.ResponseRecorder) chatUnreadCountResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp chatUnreadCountResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

// spacesUnreadTotal sums the unread rollup GET /api/v1/chat/spaces reports
// across every space: what the rail's Unread filter shows.
func spacesUnreadTotal(t *testing.T, srv *Server) int {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp chatSpacesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	total := 0
	for _, sp := range resp.Spaces {
		total += sp.UnreadCount
	}
	return total
}

// parseAdditiveEnvelope extracts and decodes the JSON envelope from DeliveryText.
func parseAdditiveEnvelope(t *testing.T, deliveryText string) additiveEnvelope {
	t.Helper()
	jsonStr := extractDEF169JSON(t, deliveryText) // reuse the existing helper
	var env additiveEnvelope
	if err := json.Unmarshal([]byte(jsonStr), &env); err != nil {
		t.Fatalf("failed to unmarshal envelope JSON: %v\nJSON: %s", err, jsonStr)
	}
	return env
}

// collectEnvelopes extracts agent slug -> envelope from dispatched messages.
func collectEnvelopes(t *testing.T, dispatched []brokerDispatchedMsg) map[string]additiveEnvelope {
	t.Helper()
	envelopes := make(map[string]additiveEnvelope)
	for _, d := range dispatched {
		if d.structured == nil {
			t.Errorf("dispatch to %q has nil structured message", d.agentSlug)
			continue
		}
		if d.structured.DeliveryText == "" {
			t.Fatalf("dispatch to %q has empty DeliveryText — writeDenyEnabled() path not reached", d.agentSlug)
		}
		envelopes[d.agentSlug] = parseAdditiveEnvelope(t, d.structured.DeliveryText)
	}
	return envelopes
}

// assertToEqual checks that two "to" arrays contain the same elements.
func assertToEqual(t *testing.T, label string, got, want []string) {
	t.Helper()
	gs := sortedTo(got)
	ws := sortedTo(want)
	if len(gs) != len(ws) {
		t.Errorf("%s: to length = %d, want %d; got %v, want %v", label, len(gs), len(ws), gs, ws)
		return
	}
	for i := range ws {
		if gs[i] != ws[i] {
			t.Errorf("%s: to[%d] = %q, want %q", label, i, gs[i], ws[i])
		}
	}
}

// extractDEF169JSON pulls the raw JSON from between delimiters.
func extractDEF169JSON(t *testing.T, deliveryText string) string {
	t.Helper()
	const begin = "---BEGIN SCION MESSAGE---"
	const end = "---END SCION MESSAGE---"

	startIdx := 0
	for i := 0; i+len(begin) <= len(deliveryText); i++ {
		if deliveryText[i:i+len(begin)] == begin {
			startIdx = i + len(begin) + 1 // skip newline after delimiter
			break
		}
	}
	endIdx := len(deliveryText)
	for i := len(deliveryText) - len(end); i >= 0; i-- {
		if deliveryText[i:i+len(end)] == end {
			endIdx = i - 1 // trim newline before end delimiter
			break
		}
	}
	if startIdx == 0 || endIdx <= startIdx {
		t.Fatalf("could not find delimiters in DeliveryText:\n%s", deliveryText)
	}
	return deliveryText[startIdx:endIdx]
}

// enableEnvelopeSwitch creates OperationalSettings with defaults (envelope
// switch is ON by compiled default when the messaging section is absent)
// and attaches them to the server so writeDenyEnabled() returns true.
func enableEnvelopeSwitch(t *testing.T, srv *Server, s store.Store) {
	t.Helper()
	ops := NewOperationalSettings(s, koanf.New("."), koanf.New("."))
	srv.SetOperationalSettings(ops)
}

// bindProjectMember gives userID the project member role so
// resolveProjectHumanMembers finds it.
func bindProjectMember(t *testing.T, s store.Store, projectID, userID string) {
	t.Helper()
	ctx := t.Context()
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	if err != nil {
		t.Fatalf("GetRoleDefinitionByName: %v", err)
	}
	if _, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	}); err != nil {
		t.Fatalf("CreateRoleBinding: %v", err)
	}
}

// addHumanMember creates a user and makes it a project member.
func addHumanMember(t *testing.T, s store.Store, projectID, email, name string) *store.User {
	t.Helper()
	u := &store.User{ID: api.NewUUID(), Email: email, DisplayName: name,
		Role: "member", Status: "active", Created: time.Now()}
	if err := s.CreateUser(t.Context(), u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	bindProjectMember(t, s, projectID, u.ID)
	return u
}

func noRecipientSetupProject(t *testing.T) (*Server, store.Store, string, *store.Agent, *brokerMockDispatcher, string) {
	t.Helper()
	srv, s, wcs, proj, db := setupSendTest(t)
	d := &brokerMockDispatcher{}
	srv.SetDispatcher(d)
	ctx := t.Context()

	a := &store.Agent{ID: tid("norcpt-agent"), ProjectID: proj.ID, Name: "Poster", Slug: "norcpt-agent",
		Phase: "running", OwnerID: DevUserID, CreatedBy: DevUserID}
	if err := s.CreateAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	topicID := tid("norcpt-topic")
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "norcpt",
		CreatedBy: "dev", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)
	topic, err := wcs.GetTopic(ctx, topicID)
	if err != nil || topic == nil || topic.ConversationID == "" {
		t.Fatalf("GetTopic: %v %+v", err, topic)
	}
	srv.ensureGroupParticipants(ctx, topic.ConversationID, []*store.Agent{a})
	seedAgentMessage(t, s, proj, topicID, a, "agent was here")
	return srv, s, topicID, a, d, proj.ID
}

// seedAgentMessage persists a message as if agent had sent it into topicID,
// so a later reply can reference it via reply_to_id. Returns the message ID.
func seedAgentMessage(t *testing.T, s store.Store, proj *store.Project, topicID string, agent *store.Agent, content string) string {
	t.Helper()
	id := api.NewUUID()
	if err := s.CreateMessage(t.Context(), &store.Message{
		ID:        id,
		ProjectID: proj.ID,
		Sender:    "agent:" + agent.Slug,
		SenderID:  agent.ID,
		Recipient: "thread:" + topicID,
		Msg:       content,
		Type:      "chat",
		AgentID:   agent.ID,
		ThreadID:  topicID,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seedAgentMessage: %v", err)
	}
	return id
}

// seedHumanMessage persists a message as if a human had sent it into
// topicID, for the "reply to a human message" case.
func seedHumanMessage(t *testing.T, s store.Store, proj *store.Project, topicID, senderID, content string) string {
	t.Helper()
	id := api.NewUUID()
	if err := s.CreateMessage(t.Context(), &store.Message{
		ID:        id,
		ProjectID: proj.ID,
		Sender:    "user:" + senderID,
		SenderID:  senderID,
		Recipient: "thread:" + topicID,
		Msg:       content,
		Type:      "chat",
		ThreadID:  topicID,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seedHumanMessage: %v", err)
	}
	return id
}

// spaceMembersStore wraps a real store to count ListAgents calls and to
// inject failures and hooks into the members endpoint's store reads.
type spaceMembersStore struct {
	store.Store
	// fault gates every override below; nil means always active.
	fault           *storeFaultSwitch
	listAgentsCalls int
	// failListAgentsOnCall makes the Nth ListAgents call (1-based) fail.
	failListAgentsOnCall int
	failProjectMembers   bool
	// onAgentPage, when set, runs after each successful ListAgents call with
	// the 1-based call number and whether that page was the last one.
	onAgentPage func(call int, last bool)
	// onEffectiveGroups, when set, runs on every GetEffectiveGroups call,
	// which the authorization service makes once per access decision for a
	// user principal.
	onEffectiveGroups func()
}

// newSpaceMembersStore is the installStoreFault wrap func for
// spaceMembersStore. Set its knobs and hooks before arming.
func newSpaceMembersStore(inner store.Store, fault *storeFaultSwitch) *spaceMembersStore {
	return &spaceMembersStore{Store: inner, fault: fault}
}

// createSpaceMembersAgents creates n agents in projectID owned by ownerID.
func createSpaceMembersAgents(t *testing.T, s store.Store, projectID, ownerID, prefix string, n int) []string {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("%s-%03d", prefix, i)
		a := &store.Agent{
			ID:        tid(name),
			ProjectID: projectID,
			Name:      name,
			Slug:      name,
			Phase:     "running",
			OwnerID:   ownerID,
			CreatedBy: ownerID,
			Ancestry:  []string{ownerID},
		}
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent %s: %v", name, err)
		}
		ids = append(ids, a.ID)
	}
	return ids
}

func createSpaceMembersProject(t *testing.T, s store.Store, name string) *store.Project {
	t.Helper()
	proj := &store.Project{ID: tid(name), Name: name, Slug: name, Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(context.Background(), proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return proj
}

func decodeSpaceMembers(t *testing.T, code int, body []byte) chatMembersResponse {
	t.Helper()
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	var resp chatMembersResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func identityOf(u *store.User) UserIdentity {
	return NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb))
}

// setupSendTest creates a project, webchat store, and a topic for send path testing.
func setupSendTest(t *testing.T) (*Server, store.Store, WebChatStore, *store.Project, *sql.DB) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{ID: tid("send-test"), Name: "send-test", Slug: "send-test", Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	db := openTestMemorySQLite(t, "sqlite")
	wcs := NewWebChatStore(db, "sqlite")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv.SetWebChatStore(wcs)

	return srv, s, wcs, proj, db
}

// setTopicConversationID creates a conversation for a topic and updates the topic's conversation_id.
// This is required after the G2 refactor made conversation resolution fatal.
func setTopicConversationID(t *testing.T, db *sql.DB, s store.Store, topicID, projectID string) {
	t.Helper()
	ctx := context.Background()
	pid := projectID
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:" + projectID + ":" + topicID,
		DriftState:  "active",
		ProjectID:   &pid,
	})
	if err != nil {
		t.Fatalf("UpsertConversation: %v", err)
	}
	_, err = db.ExecContext(ctx, "UPDATE webchat_topic SET conversation_id = ? WHERE id = ?", conv.ID, topicID)
	if err != nil {
		t.Fatalf("update topic conversation_id: %v", err)
	}
}

// setDMConversationID creates a conversation for a DM key.
// This is required after the G2 refactor made conversation resolution fatal.
func setDMConversationID(t *testing.T, s store.Store, dmKey, _ string) {
	t.Helper()
	ctx := context.Background()
	_, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	if err != nil {
		t.Fatalf("UpsertConversation for DM: %v", err)
	}
}

// newTestWebChatStoreWithMessages creates a WebChatStore backed by an in-memory
// SQLite DB, including a minimal messages table for search testing. It uses
// the production driver (modernc) and a DATETIME created column bound with a
// time.Time, so created holds the same time.Time.String() text the ent
// migrated table does. TestSearchChatMessages_PagesToExhaustionOnEntSchema
// covers paging on the real ent schema.
func newTestWebChatStoreWithMessages(t *testing.T) (WebChatStore, *sql.DB) {
	t.Helper()
	db := openTestMemorySQLite(t, "sqlite")

	store := NewWebChatStore(db, "sqlite")
	if err := store.Init(); err != nil {
		t.Fatalf("init store: %v", err)
	}

	// Create a minimal messages table matching the Ent schema columns
	// used by SearchChatMessages.
	const createMessages = `
CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    sender TEXT NOT NULL DEFAULT '',
    sender_id TEXT,
    recipient TEXT NOT NULL DEFAULT '',
    recipient_id TEXT,
    msg TEXT NOT NULL DEFAULT '',
    type TEXT NOT NULL DEFAULT 'instruction',
    channel TEXT,
    thread_id TEXT,
    created DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_created ON messages (created);
`
	if _, err := db.Exec(createMessages); err != nil {
		t.Fatalf("create messages table: %v", err)
	}

	return store, db
}

// insertTestMessage is a helper to insert a message row for search testing.
func insertTestMessage(t *testing.T, db *sql.DB, id, projectID, threadID, sender, msg string, created time.Time) {
	t.Helper()
	const query = `INSERT INTO messages (id, project_id, thread_id, sender, msg, channel, created) VALUES (?, ?, ?, ?, ?, 'web', ?)`
	_, err := db.Exec(query, id, projectID, threadID, sender, msg, created.UTC())
	if err != nil {
		t.Fatalf("insert test message: %v", err)
	}
}

// enableWriteDenySwitch configures OperationalSettings on the server with the
// consolidated ConversationEnvelopeSwitch ON. After this call, handlers that
// check s.writeDenyEnabled() will deny writes when conversation resolution fails.
func enableWriteDenySwitch(t *testing.T, srv *Server) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	fakeStore.seed("messaging", json.RawMessage(`{"conversation_envelope_switch":true}`))
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("ops.Refresh failed: %v", err)
	}
	srv.SetOperationalSettings(ops)
	if !srv.GetOperationalSettings().ConversationEnvelopeSwitch() {
		t.Fatalf("enableWriteDenySwitch: ConversationEnvelopeSwitch() is still false after setup")
	}
}

// readStateAtPublishSpy wraps noopEventPublisher and, on PublishUserMessage,
// snapshots both halves of the unread computation for the message's
// conversation at the moment of the call — i.e. what a client would see if
// it reacted to the SSE event the instant it arrives. `hasUnread` is
// computed exactly the way the rollup endpoints do it
// (handlers_chat_v2.go ~L155, ~L384, ~L3439: LastMessageID != LastReadMessageID),
// so this pins the actual user-visible invariant, not just one of its two
// inputs. Used to pin down that the sender's read watermark *and* the
// conversation's last-message watermark are both advanced before the
// message is published, not after, closing the self-unread flash race
// rather than narrowing it.
type readStateAtPublishSpy struct {
	noopEventPublisher
	wcs WebChatStore

	called                 bool
	messageID              string
	readStateAtPublish     *WebChatReadState
	lastMessageIDAtPublish string
	hasUnreadAtPublish     bool
}

// unreachableTestSetup creates a project and a topic whose default agent has
// the given phase (and is optionally soft-deleted), and wires the given
// dispatcher. Mirrors setupSendTest in handlers_chat_v2_test.go.
func unreachableTestSetup(t *testing.T, phase string, deleted bool, disp AgentDispatcher) (*Server, store.Store, string, *store.Agent) {
	t.Helper()
	srv, s, wcs, proj, db := setupSendTest(t)
	srv.SetDispatcher(disp)
	ctx := t.Context()
	a := &store.Agent{ID: tid("unreachable-" + phase), ProjectID: proj.ID, Name: "Unreachable", Slug: "unreachable-" + phase,
		Phase: phase, OwnerID: DevUserID, CreatedBy: DevUserID}
	if err := s.CreateAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	if deleted {
		a.DeletedAt = time.Now()
		if err := s.UpdateAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	topicID := tid("unreachable-topic-" + phase)
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "unreachable-" + phase,
		CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: a.Slug}); err != nil {
		t.Fatal(err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)
	return srv, s, topicID, a
}

// unreachableSend posts content to topicID and returns the HTTP status, the
// decoded JSON response body, and the persisted store row (nil if the
// response carried no message ID).
func unreachableSend(t *testing.T, srv *Server, s store.Store, topicID, content string) (int, map[string]any, *store.Message) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages",
		map[string]string{"content": content})
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	id, _ := resp["id"].(string)
	var m *store.Message
	if id != "" {
		m, _ = s.GetMessage(t.Context(), id)
	}
	t.Logf("HTTP %d body=%s", rec.Code, rec.Body.String())
	return rec.Code, resp, m
}

// transientAgentLookupStore wraps a store and injects a non-ErrNotFound error
// from GetAgentBySlug for a specific slug, to exercise the nit-1 fix: a
// transient store error resolving a topic's default agent must not be
// classified the same as "deleted".
type transientAgentLookupStore struct {
	store.Store
	failSlug string
	err      error
}

// errListAgentsStore wraps a store and forces ListAgents to fail, which is
// the seam resolveRoutingAgents (via listAllProjectAgents) uses to list
// project agents for mention resolution. Injecting a failure there is the
// cleanest way to force resolveRoutingAgents to return a non-nil error
// without adding a hacky new seam to production code.
type errListAgentsStore struct {
	store.Store
	err error
}

func userIdentityFor(u *store.User) UserIdentity {
	return NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb))
}

// deadlineRecorder is a ResponseRecorder that supports SetWriteDeadline,
// as a real connection does, and records every deadline set.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	mu        sync.Mutex
	deadlines []time.Time
}

// grantUserProjectAccess grants a human user project-member access,
// mirroring grantAgentProjectAccess for the "user" principal kind.
func grantUserProjectAccess(t *testing.T, s store.Store, userID, projectID string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err, "project-member role definition not found")
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    "user",
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// setupConvTestData creates a project, agent, and conversation for testing.
func setupConvTestData(t *testing.T, s store.Store) (project *store.Project, agent *store.Agent, conv *store.Conversation) {
	t.Helper()
	ctx := context.Background()

	project = &store.Project{
		ID:   api.NewUUID(),
		Name: "conv-test-project",
		Slug: "conv-test-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent = &store.Agent{
		ID:        api.NewUUID(),
		Name:      "conv-test-agent",
		Slug:      "conv-test-agent",
		ProjectID: project.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	now := time.Now().UTC()
	conv = &store.Conversation{
		ID:             api.NewUUID(),
		ProjectID:      &project.ID,
		Kind:           "group",
		Surface:        "native",
		DisplayName:    "Test Conversation",
		DriftState:     "active",
		LastActivityAt: now,
		CreatedAt:      now,
	}
	require.NoError(t, s.CreateConversation(ctx, conv))

	return project, agent, conv
}

// addConvParticipant adds a participant to a conversation for testing.
func addConvParticipant(t *testing.T, s store.Store, convID, principalKind, principalID string) {
	t.Helper()
	p := &store.ConversationParticipant{
		ID:             api.NewUUID(),
		ConversationID: convID,
		PrincipalKind:  principalKind,
		PrincipalID:    principalID,
		Role:           "member",
		JoinedAt:       time.Now().UTC(),
	}
	require.NoError(t, s.AddParticipant(context.Background(), p))
}

// agentContext returns a context with an agent identity set.
func agentContext(agentID, projectID string) context.Context {
	return contextWithIdentity(context.Background(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
	}})
}

// agentContextWithScopes returns a context with an agent identity that includes
// the given JWT scopes. Use this when calling endpoints that check authorization
// via s.authorize (e.g., project-level authz in handleCreateConversation).
func agentContextWithScopes(agentID, projectID string, scopes []AgentTokenScope) context.Context {
	return contextWithIdentity(context.Background(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
		Scopes:    scopes,
	}})
}

// convProjectID extracts the project ID from a conversation, returning empty if nil.
func convProjectID(c *store.Conversation) string {
	if c.ProjectID != nil {
		return *c.ProjectID
	}
	return ""
}

// grantAgentProjectAccess grants an agent a project-member role binding,
// giving it read access to the project. This is needed after the BOLA fix
// added an authorize check in handleCreateConversation.
func grantAgentProjectAccess(t *testing.T, s store.Store, agentID, projectID string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err, "project-member role definition not found")
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    "agent",
		PrincipalID:      agentID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// wireSharedWebChatStore wires a WebChatStore onto srv that shares the test
// store's underlying SQLite DB. Group creation now routes through
// WebChatStore.CreateTopic (chat-thread-bridge), so every test that creates
// a group conversation via the HTTP handler needs one wired — otherwise the
// handler returns 503. Sharing the DB is load-bearing: CreateTopic's
// dual-write must land in the same "conversations" table that
// store.GetConversation reads back from.
func wireSharedWebChatStore(t *testing.T, srv *Server, s store.Store) WebChatStore {
	t.Helper()
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "test store does not expose DB()")
	rawDB := dbProvider.DB()
	require.NotNil(t, rawDB, "store DB() returned nil")

	wcs := NewWebChatStore(rawDB, "sqlite")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)
	return wcs
}

// setupGroupConvTopicTest builds a server whose webChatStore shares the
// store's underlying SQLite DB. Sharing is load-bearing: CreateTopic's
// dual-write lands in the same "conversations" table that
// store.GetConversation reads from (see TestDEF96_PromoteDM_HistoryVisibleOnFirstRead
// for the same pattern). Without sharing, the handler's read-back after
// CreateTopic would always 500.
func setupGroupConvTopicTest(t *testing.T) (*Server, store.Store, WebChatStore, *topicEventSpy) {
	t.Helper()
	srv, s := testServer(t)

	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "test store does not expose DB()")
	rawDB := dbProvider.DB()
	require.NotNil(t, rawDB, "store DB() returned nil")

	wcs := NewWebChatStore(rawDB, "sqlite")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)

	spy := &topicEventSpy{}
	srv.events = spy

	return srv, s, wcs, spy
}

// doRequestWithAgentToken performs an HTTP request with an agent JWT token.
func doRequestWithAgentToken(t *testing.T, srv *Server, method, path string, body interface{}, token string) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("failed to marshal body: %v", err)
		}
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Scion-Agent-Token", token)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// spyBrokerBus records all published messages for inspection.
type spyBrokerBus struct {
	mu     sync.Mutex
	events []spyBrokerEvent
}

// foreignAttachSetup creates two projects, two agents (one per project),
// a DM conversation between them, and enables cross-project messaging.
func foreignAttachSetup(t *testing.T) (
	srv *Server, s store.Store,
	projectA, projectB *store.Project,
	agentA, agentB *store.Agent,
	convID string, dispatcher *recordingDispatcher,
	spyBus *spyBrokerBus,
) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID:      tid("fa-owner"),
		Email:   "fa-owner@test.example",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	projectA = &store.Project{
		ID:        tid("fa-project-a"),
		Name:      "fa-project-a",
		Slug:      "fa-project-a",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, projectA))
	// The agents' ancestry root is a member of the project, so they are
	// in good standing (ptone/scion#3433).
	ensureStandingRoot(t, s, projectA.ID, owner.ID)

	projectB = &store.Project{
		ID:   tid("fa-project-b"),
		Name: "fa-project-b",
		Slug: "fa-project-b",
	}
	require.NoError(t, s.CreateProject(ctx, projectB))
	_, err := s.UpdateProjectMessagingPolicy(ctx, projectB.ID, store.CrossProjectInboundAny, 1)
	require.NoError(t, err)

	brokerID := tid("fa-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "fa-broker",
		Slug:   "fa-broker",
		Status: store.BrokerStatusOnline,
	}))

	agentA = &store.Agent{
		ID:              tid("fa-agent-a"),
		Name:            "fa-agent-a",
		Slug:            "fa-agent-a",
		ProjectID:       projectA.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeHub,
		Ancestry:        []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))

	agentB = &store.Agent{
		ID:              tid("fa-agent-b"),
		Name:            "fa-agent-b",
		Slug:            "fa-agent-b",
		ProjectID:       projectB.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeHub,
		Ancestry:        []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	// Create DM conversation between the agents.
	dmKey, err := messages.DMConversationKey("agent", agentA.ID, "agent", agentB.ID)
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

	// Set up a spy broker bus so we can capture observer publications.
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

	enableCPM(t, srv, s)

	return srv, s, projectA, projectB, agentA, agentB, convID, dispatcher, spyBus
}

// sendOutboundDMWithAttachments sends an outbound message with optional attachments.
func sendOutboundDMWithAttachments(t *testing.T, srv *Server, sender *store.Agent, convID, projectID, msgText string, attachments []string) *httptest.ResponseRecorder {
	t.Helper()

	reqBody, err := json.Marshal(OutboundMessageRequest{
		ConversationRef: "conv:" + convID,
		Msg:             msgText,
		Type:            "instruction",
		Attachments:     attachments,
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+projectID+"/agents/"+sender.ID+"/outbound-message",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: sender.ID},
		ProjectID: projectID,
		Ancestry:  sender.Ancestry,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, sender.ID)
	return rr
}

// sendInboundAgentMessage sends a message through the inbound handleAgentMessage path.
func sendInboundAgentMessage(t *testing.T, srv *Server, senderAgent, targetAgent *store.Agent, msgText string, attachments []string) *httptest.ResponseRecorder {
	t.Helper()

	sm := &messages.StructuredMessage{
		Version:     messages.Version,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Type:        messages.TypeInstruction,
		Sender:      "agent:" + senderAgent.Slug,
		SenderID:    senderAgent.ID,
		Recipient:   "agent:" + targetAgent.Slug,
		RecipientID: targetAgent.ID,
		Msg:         msgText,
		Attachments: attachments,
	}

	reqBody, err := json.Marshal(MessageRequest{
		StructuredMessage: sm,
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+targetAgent.ProjectID+"/agents/"+targetAgent.ID+"/message",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: senderAgent.ID},
		ProjectID: senderAgent.ProjectID,
		Ancestry:  senderAgent.Ancestry,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, targetAgent.ID)
	return rr
}

// assignAgentGCPSA creates a GCP service account row with the given state
// and points the agent's applied config at it in assign mode.
func assignAgentGCPSA(t *testing.T, s store.Store, agent *store.Agent, suffix string, verified bool, status string) *store.GCPServiceAccount {
	t.Helper()
	ctx := context.Background()
	sa := &store.GCPServiceAccount{
		ID: tid("sa-start-" + suffix), Scope: store.ScopeProject, ScopeID: agent.ProjectID,
		Email: "start-" + suffix + "@p.iam.gserviceaccount.com", ProjectID: "gcp-proj",
		Verified: verified, VerificationStatus: status, CreatedBy: "dev", CreatedAt: time.Now(),
	}
	if verified {
		sa.VerifiedAt = time.Now()
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	if got.AppliedConfig == nil {
		got.AppliedConfig = &store.AgentAppliedConfig{}
	}
	got.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
		MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: sa.ID,
		ServiceAccountEmail: sa.Email, ProjectID: sa.ProjectID,
	}
	require.NoError(t, s.UpdateAgent(ctx, got))
	return sa
}

const flatSAPath = "/api/v1/gcp-service-accounts/"

func mkSA(t *testing.T, s store.Store, idName, email, scope, scopeID, createdBy string) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID:        tid(idName),
		Scope:     scope,
		ScopeID:   scopeID,
		Email:     email,
		ProjectID: "gcp-proj",
		CreatedBy: createdBy,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), sa))
	return sa
}

func saExists(t *testing.T, s store.Store, id string) bool {
	t.Helper()
	_, err := s.GetGCPServiceAccount(context.Background(), id)
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	require.NoError(t, err)
	return true
}

// newHubScopedSA builds a hub-scoped SA owned by nobody in particular. CreatedBy
// is deliberately a stranger: the owner short-circuit in checkAccessForUser
// would otherwise mask which branch of the authorization actually fired.
func newHubScopedSA(idName, email string) *store.GCPServiceAccount {
	return &store.GCPServiceAccount{
		ID:                 tid(idName),
		Scope:              store.ScopeHub,
		ScopeID:            "hub-instance-1",
		Email:              email,
		ProjectID:          "hub-gcp-project",
		DisplayName:        "Hub-wide SA",
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: store.GCPVerificationVerified,
		CreatedBy:          tid("user-somebody-else"),
		CreatedAt:          time.Now(),
	}
}

// seedListMix creates the three-account fixture the list tests share: one in
// the caller's project, one in a different project, one hub-scoped.
func seedListMix(t *testing.T, ctx context.Context, s store.Store, owner *store.User, project *store.Project) (mine, hub string) {
	t.Helper()

	other := &store.Project{
		ID: tid("project-list-other"), Name: "Other", Slug: "other-list",
		OwnerID: owner.ID, CreatedBy: owner.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, other))
	// The caller owns the other project too, through the project-owner
	// binding (Project.OwnerID grants nothing, ptone/scion#2586).
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: owner.ID,
		ScopeType: store.RoleScopeProject, ScopeID: other.ID, CreatedBy: owner.ID,
	})
	require.NoError(t, err)

	mk := func(idName, email, scope, scopeID, createdBy string) {
		require.NoError(t, s.CreateGCPServiceAccount(ctx, &store.GCPServiceAccount{
			ID: tid(idName), Scope: scope, ScopeID: scopeID, Email: email,
			ProjectID: "gcp-proj", CreatedBy: createdBy, CreatedAt: time.Now(),
		}))
	}
	mine = "mine@p.iam.gserviceaccount.com"
	hub = "hubwide@p.iam.gserviceaccount.com"
	mk("sa-list-mine", mine, store.ScopeProject, project.ID, owner.ID)
	mk("sa-list-other", "other@p.iam.gserviceaccount.com", store.ScopeProject, other.ID, owner.ID)
	// The hub-scoped account is created by a stranger on purpose.
	// gcpServiceAccountResource sets OwnerID from CreatedBy, and CheckAccess
	// short-circuits for a resource's owner — correctly, since whoever minted a
	// hub-scoped SA does keep rights over it. Seeding it under the project
	// owner would fire that short-circuit and make the scope tests pass for a
	// reason that has nothing to do with scope.
	mk("sa-list-hub", hub, store.ScopeHub, "hub-instance-1", tid("user-hub-sa-creator"))

	return mine, hub
}

func topLevelSAEmails(t *testing.T, srv *Server, user *store.User, query string) []string {
	t.Helper()
	rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/gcp-service-accounts?"+query, nil)
	require.Equal(t, http.StatusOK, rec.Code, "list failed: %s", rec.Body.String())

	var resp ListGCPServiceAccountsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	emails := make([]string, 0, len(resp.Items))
	for _, item := range resp.Items {
		emails = append(emails, item.Email)
	}
	return emails
}

func createTestProjectForSA(t *testing.T, srv *Server, s store.Store) string {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", map[string]string{
		"name": "test-project-sa",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "create project: %s", rec.Body.String())
	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))
	return project.ID
}

// mockGCPServiceAccountAdmin is a test implementation of GCPServiceAccountAdmin.
type mockGCPServiceAccountAdmin struct {
	createErr   error
	policyErr   error
	deleteErr   error
	createdSAs  []string // track created account IDs
	deletedSAs  []string // track deleted SA emails (cleanup calls)
	lastEmail   string
	lastProject string

	// Track IAM mutations for assertions.
	iamPolicies []mockIAMPolicyCall // SA-level SetIAMPolicy calls
}

func testServerWithMinting(t *testing.T) (*Server, store.Store, *mockGCPServiceAccountAdmin) {
	t.Helper()
	srv, s := testServer(t)
	mock := &mockGCPServiceAccountAdmin{}
	srv.SetGCPServiceAccountAdmin(mock)
	srv.SetGCPProjectID("test-hub-project")

	// Set a mock token generator so the hub SA email is available
	srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub-sa@test-hub-project.iam.gserviceaccount.com"})

	return srv, s, mock
}

// mockGCPTokenGenerator implements GCPTokenGenerator for testing.
type mockGCPTokenGenerator struct {
	email string
}

// mockGCPTokenGeneratorVerifyFail is a mock that fails VerifyImpersonation but succeeds on other ops.
type mockGCPTokenGeneratorVerifyFail struct {
	email     string
	verifyErr error
}

// setupGCPAuthzTest creates a test server with three users and a project:
//   - owner: project owner (non-admin member), in project members group
//   - member: project member (non-admin), in project members group
//   - outsider: hub member but NOT in project members group
//
// Returns the server, store, users, and project.
func setupGCPAuthzTest(t *testing.T) (*Server, store.Store, *store.User, *store.User, *store.User, *store.Project) {
	t.Helper()
	srv, s := testServer(t)
	owner, member, outsider, project := setupGCPAuthzFixture(t, srv, s)
	return srv, s, owner, member, outsider, project
}

// setupGCPAuthzFixture is setupGCPAuthzTest on a server the caller built,
// for tests that must install a store fault (installStoreFault) before this
// audited setup runs.
func setupGCPAuthzFixture(t *testing.T, srv *Server, s store.Store) (*store.User, *store.User, *store.User, *store.Project) {
	t.Helper()
	ctx := context.Background()

	owner := &store.User{
		ID:          tid("user-gcp-owner"),
		Email:       "gcp-owner@test.com",
		DisplayName: "GCP Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	member := &store.User{
		ID:          tid("user-gcp-member"),
		Email:       "gcp-member@test.com",
		DisplayName: "GCP Member",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	outsider := &store.User{
		ID:          tid("user-gcp-outsider"),
		Email:       "gcp-outsider@test.com",
		DisplayName: "GCP Outsider",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	for _, u := range []*store.User{owner, member, outsider} {
		require.NoError(t, s.CreateUser(ctx, u))
		ensureHubMembership(ctx, s, u.ID)
	}

	project := &store.Project{
		ID:        tid("project-gcp-authz"),
		Name:      "GCP Authz Project",
		Slug:      "gcp-authz-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create project members group and policies (simulates project creation handler)
	srv.seedProjectCreatorMembership(ctx, project)

	// Add member to project members group
	membersGroup, err := s.GetGroupBySlug(ctx, "project:gcp-authz-project:members")
	require.NoError(t, err)
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    membersGroup.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   member.ID,
		Role:       store.GroupMemberRoleMember,
	}))

	return owner, member, outsider, project
}

// webhookTestServer creates a test server with the webhook secret configured.
func webhookTestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	srv, s := testServer(t)
	srv.mu.Lock()
	srv.config.GitHubAppConfig.WebhookSecret = "test-webhook-secret"
	srv.config.GitHubAppConfig.WebhooksEnabled = true
	srv.config.GitHubAppConfig.AppID = 42
	srv.mu.Unlock()
	return srv, s
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal JSON: %v", err)
	}
	return data
}

// failingParticipantStore wraps a real store.Store and makes both
// AddParticipant and EnsureParticipant always fail, to exercise AC-12: a
// participant-row insert failure must never fail or alter the send response.
type failingParticipantStore struct {
	store.Store
}

// getHealthSummaryBrokers fetches the health summary and returns both the
// decoded runtime broker list and the raw JSON of each row keyed by broker ID.
func getHealthSummaryBrokers(t *testing.T, srv *Server) (HealthSummaryBrokers, map[string]map[string]json.RawMessage) {
	t.Helper()
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))

	var raw struct {
		Brokers struct {
			Items []map[string]json.RawMessage `json:"items"`
		} `json:"runtime_brokers"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw))
	rows := make(map[string]map[string]json.RawMessage, len(raw.Brokers.Items))
	for _, row := range raw.Brokers.Items {
		var id string
		require.NoError(t, json.Unmarshal(row["id"], &id))
		rows[id] = row
	}
	return resp.Brokers, rows
}

func createSummaryBroker(t *testing.T, s store.Store, b *store.RuntimeBroker) {
	t.Helper()
	if b.Slug == "" {
		b.Slug = b.Name
	}
	if b.Status == "" {
		b.Status = store.BrokerStatusOnline
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), b))
}

// pingFailStore wraps a real store but reports the database as unreachable,
// so GetHealthInfo's critical database check fails while everything else
// (stats queries, summary aggregation) still works.
type pingFailStore struct {
	store.Store
}

type mockIntegrationManager struct {
	plugins            map[string]map[string]string // name → config
	selfManaged        map[string]bool
	deploymentModes    map[string]plugin.DeploymentMode
	healthErr          error
	infoErr            error
	configureErr       error
	replaceConfigErr   error
	reconnectErr       error
	updateErr          error
	installErr         error
	configureCalls     []string
	replaceConfigCalls []string
	lastReplacedConfig map[string]string
	restartCalls       []string
	reconnectCalls     []string
	updateCalls        []string
	installCalls       []string
	loadOneCalls       []string
	loadOneEntries     []plugin.PluginEntry
	loadOneErr         error
	brokers            map[string]eventbus.EventBus // name → bus (for GetBroker)
}

func newMockIntegrationManager() *mockIntegrationManager {
	return &mockIntegrationManager{
		plugins:         make(map[string]map[string]string),
		selfManaged:     make(map[string]bool),
		deploymentModes: make(map[string]plugin.DeploymentMode),
	}
}

// integAdminServer creates a testServer with a super-admin user for
// integration tests that exercise authorization paths. Returns the server,
// the admin identity, and a context with the admin identity set.
// CO1: Replaces the old pattern of &Server{authzService: NewAuthzService(nil, ...)}.
// integSessionContext returns ctx carrying identity and an interactive
// session credential, the credential an admin's browser request carries.
func integSessionContext(ctx context.Context, identity Identity) context.Context {
	return contextWithCredentialContext(contextWithIdentity(ctx, identity), CredentialContext{Kind: CredentialKindInteractive})
}

func integAdminServer(t *testing.T, suffix string) (*Server, *AuthenticatedUser, context.Context) {
	t.Helper()
	srv, s := testServer(t)
	userID := tid("integ-" + suffix)
	email := "integ-" + suffix + "@example.com"
	createTestUserWithRole(t, s, userID, email, "admin", store.SystemRoleSuperAdmin)
	admin := NewAuthenticatedUser(userID, email, "Admin", "admin", "cli")
	ctx := integSessionContext(context.Background(), admin)
	return srv, admin, ctx
}

// validWebhookAction returns a minimal well-formed webhook action that passes
// validation (no execution identity required for webhook type).
func validWebhookAction() *store.LifecycleHookAction {
	return &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionWebhook,
		Method:         "POST",
		URL:            "https://hooks.example.com/webhook",
		Body:           `{"agent":"${AGENT_ID}"}`,
		TimeoutSeconds: 10,
		OnError:        store.LifecycleHookOnErrorLog,
	}
}

// validCreateRequest returns a well-formed create-hook request body (webhook
// type so no execution identity is needed).
func validCreateRequest() createLifecycleHookRequest {
	return createLifecycleHookRequest{
		Name:      "register-agent",
		ScopeType: store.LifecycleHookScopeHub,
		Trigger:   store.LifecycleHookTriggerRunning,
		Action:    validWebhookAction(),
		Enabled:   true,
	}
}

func createTestAgent(t *testing.T, s store.Store) *store.Agent {
	t.Helper()
	ctx := context.Background()
	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "test-project-" + api.NewUUID()[:8],
		Slug: "test-project-" + api.NewUUID()[:8],
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	agent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "test-agent-" + api.NewUUID()[:8],
		Slug:      "test-agent-" + api.NewUUID()[:8],
		ProjectID: project.ID,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return agent
}

// fakeLogQuerier records the options the handlers pass to the log
// query service.
type fakeLogQuerier struct {
	queryOpts []LogQueryOptions
	tailOpts  []LogQueryOptions
}

// setupMessageTestAgent creates a project, runtime broker, and agent for message tests.
func setupMessageTestAgent(t *testing.T, s store.Store, phase string) (projectID, agentID string) {
	t.Helper()
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:       tid("msg-broker"),
		Name:     "msg-broker",
		Slug:     "msg-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	project := &store.Project{
		ID:   tid("msg-project"),
		Slug: "msg-project",
		Name: "msg-project",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID:              tid("msg-agent"),
		Slug:            "msg-agent",
		Name:            "msg-agent",
		ProjectID:       project.ID,
		Phase:           phase,
		RuntimeBrokerID: broker.ID,
		Created:         time.Now(),
		Updated:         time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	return project.ID, agent.ID
}

// errorDispatcher wraps brokerMockDispatcher to return a fixed error.
type errorDispatcher struct {
	brokerMockDispatcher
	err        error
	deferCount int32
	calls      atomic.Int32
}

// setupMessagePrivacyTest builds a small world:
//
//   - project "msg-priv"
//   - agent "agent-priv" inside that project, owned by alice
//   - alice (manager/owner) — can manage the agent
//   - bob   (member with read-only policy) — can read but NOT manage
//   - three messages on the agent:
//     m1  alice → agent   (alice is sender)
//     m2  agent → alice   (alice is recipient)
//     m3  carol → agent   (neither alice nor bob is a participant)
func setupMessagePrivacyTest(t *testing.T) (
	srv *Server,
	s store.Store,
	alice, bob *store.User,
	agentID string,
) {
	t.Helper()

	srv, s = testServer(t)
	ctx := context.Background()

	// --- users ---
	alice = &store.User{
		ID: tid("msg-alice"), Email: "alice@msg.test",
		DisplayName: "Alice", Role: store.UserRoleMember, Status: "active",
		Created: time.Now(),
	}
	bob = &store.User{
		ID: tid("msg-bob"), Email: "bob@msg.test",
		DisplayName: "Bob", Role: store.UserRoleMember, Status: "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))
	require.NoError(t, s.CreateUser(ctx, bob))

	// --- project ---
	project := &store.Project{
		ID: tid("project-msg-priv"), Name: "Msg Privacy",
		Slug: "msg-priv", OwnerID: alice.ID, CreatedBy: alice.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)

	// --- agent (owned by alice → alice gets manage via owner bypass) ---
	agentID = tid("agent-msg-priv")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "agent-msg-priv", Name: "Privacy Agent",
		ProjectID: project.ID, OwnerID: alice.ID, Created: time.Now(), Updated: time.Now(),
	}))

	// --- give bob read-only access on agents via project membership (CO1: role bindings) ---
	createTestUserWithProjectRole(t, s, bob.ID, bob.Email, project.ID, store.ProjectRoleMember)

	// --- messages ---
	now := time.Now()

	// m1: alice → agent (alice is sender)
	require.NoError(t, s.CreateMessage(ctx, &store.Message{
		ID: uuid.NewString(), ProjectID: project.ID,
		Sender: "user:alice", SenderID: alice.ID,
		Recipient: "agent:privacy-agent", RecipientID: agentID,
		Msg: "hello from alice", Type: "instruction",
		AgentID: agentID, CreatedAt: now,
	}))

	// m2: agent → alice (alice is recipient)
	require.NoError(t, s.CreateMessage(ctx, &store.Message{
		ID: uuid.NewString(), ProjectID: project.ID,
		Sender: "agent:privacy-agent", SenderID: agentID,
		Recipient: "user:alice", RecipientID: alice.ID,
		Msg: "reply to alice", Type: "state-change",
		AgentID: agentID, CreatedAt: now,
	}))

	// m3: carol → agent (neither alice nor bob is a participant)
	carolID := tid("msg-carol")
	require.NoError(t, s.CreateMessage(ctx, &store.Message{
		ID: uuid.NewString(), ProjectID: project.ID,
		Sender: "user:carol", SenderID: carolID,
		Recipient: "agent:privacy-agent", RecipientID: agentID,
		Msg: "hello from carol", Type: "instruction",
		AgentID: agentID, CreatedAt: now,
	}))

	return srv, s, alice, bob, agentID
}

// setupProjectWithBroker creates a project with a registered runtime broker for
// agent creation tests.
func setupProjectWithBroker(t *testing.T, s store.Store, projectID, projectName string) *store.Project {
	t.Helper()
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     tid("broker-" + projectID),
		Name:   "Test Broker",
		Slug:   "test-broker-" + projectID,
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	project := &store.Project{
		ID:   tid(projectID),
		Name: projectName,
		Slug: projectID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	provider := &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddProjectProvider(ctx, provider))

	return project
}

const testOIDCIssuerURL = "https://scion.example.com"

// installTestOIDCKeyManager sets an OIDCKeyManager with a fresh RS256 key and
// the testOIDCIssuerURL issuer on srv. Routes that New() registers only when
// the OIDC provider is enabled stay unregistered.
func installTestOIDCKeyManager(t *testing.T, srv *Server) {
	t.Helper()

	// Generate a test RSA key pair.
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	kid := computeKeyID(&privKey.PublicKey)
	signingKey := &OIDCSigningKey{
		KeyID:      kid,
		PrivateKey: privKey,
		PublicKey:  &privKey.PublicKey,
		Active:     true,
	}

	// Create the RS256 signer (needed by identity token endpoint).
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: privKey},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid),
	)
	require.NoError(t, err)

	mgr := &OIDCKeyManager{
		activeKey: signingKey,
		allKeys:   []*OIDCSigningKey{signingKey},
		signer:    signer,
		issuerURL: testOIDCIssuerURL,
	}

	srv.oidcKeyManager = mgr
	srv.oidcIssuerURL = testOIDCIssuerURL
}

// testOIDCServerWithRoutes creates a Server with OIDC enabled via config so
// that routes are registered during New(). Use for mux-routing tests.
func testOIDCServerWithRoutes(t *testing.T) *Server {
	t.Helper()
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("Skipping test because sqlite driver is not registered (build with -tags sqlite to enable)")
		}
		t.Fatalf("failed to create test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken
	cfg.OIDCConfig = config.OIDCProviderConfig{
		Enabled:   true,
		IssuerURL: testOIDCIssuerURL,
	}

	srv, err := newTestHubServer(t, cfg, s)
	if err != nil {
		t.Fatalf("New() with OIDC failed: %v", err)
	}
	srv.SetHubID("test-hub-id")
	return srv
}

// postOutboundRequest sends a prepared OutboundMessageRequest for agentID.
// Shared by the raw conversation_id and conversation_ref with-thread cases
// below (they differ only in which fields of OutboundMessageRequest are
// set), so the two paths can't drift apart in how the request is built.
func postOutboundRequest(t *testing.T, srv *Server, projectID, agentID string, r OutboundMessageRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(r)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agentID)
	return rr
}

// setupWebChannelBroker registers a "web" channel on srv so requests carrying
// Channel:"web" pass validateChannelRegistered. Mirrors the broker portion of
// def158BrokerSetup, without the WebChatStore/read-switch machinery this file
// doesn't need.
func setupWebChannelBroker(t *testing.T, srv *Server, s store.Store, project *store.Project) {
	t.Helper()
	inprocessBus := eventbus.NewInProcessEventBus(slog.Default())
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inprocessBus},
		{Name: "web", Bus: nullSpokeEventBus{}},
	}, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)

	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return nil }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)
	proxy.subscribeProjectUserMessages(project.ID)
}

// def138Setup creates a project, an agent, and a user. The agent is
// configured as the sender for outbound messages.
func def138Setup(t *testing.T) (srv *Server, s store.Store, project *store.Project, agent *store.Agent, user *store.User) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	project = &store.Project{
		ID:   tid("def138-project"),
		Name: "def138-project",
		Slug: "def138-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	user = &store.User{
		ID:          tid("def138-user"),
		Email:       "def138@example.com",
		DisplayName: "DEF138 User",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	agent = &store.Agent{
		ID:        tid("def138-agent"),
		Name:      "def138-agent",
		Slug:      "def138-agent",
		ProjectID: project.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	return srv, s, project, agent, user
}

// postOutboundWithConv sends an outbound message with a conversation_id.
func postOutboundWithConv(t *testing.T, srv *Server, projectID, agentID, recipientEmail, msg, convID string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient:      "user:" + recipientEmail,
		Msg:            msg,
		ConversationID: convID,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agentID)
	return rr
}

// postOutboundNoConv sends an outbound message without a conversation_id.
func postOutboundNoConv(t *testing.T, srv *Server, projectID, agentID, recipientEmail, msg string) *httptest.ResponseRecorder {
	t.Helper()
	return postOutboundWithConv(t, srv, projectID, agentID, recipientEmail, msg, "")
}

// def141BrokerSetup creates a server with a broker, project, agent, and user.
// The broker is wired so that handler → PublishUserMessage → deliverToUser.
func def141BrokerSetup(t *testing.T) (srv *Server, s store.Store, project *store.Project, agent *store.Agent, user *store.User) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	project = &store.Project{
		ID:   tid("d141-broker-project"),
		Name: "d141-broker-project",
		Slug: "d141-broker-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	user = &store.User{
		ID:          tid("d141-broker-user"),
		Email:       "d141-broker@example.com",
		DisplayName: "D141 Broker User",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	agent = &store.Agent{
		ID:        tid("d141-broker-agent"),
		Name:      "d141-broker-agent",
		Slug:      "d141-broker-agent",
		ProjectID: project.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	bus := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = bus.Close() })

	proxy := NewMessageBrokerProxy(bus, s, events,
		func() AgentDispatcher { return &brokerMockDispatcher{} }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)

	return srv, s, project, agent, user
}

// postOutboundWithRef sends an outbound message with a conversation_ref.
func postOutboundWithRef(t *testing.T, srv *Server, projectID, agentID, recipientEmail, msg, convRef string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient:       "user:" + recipientEmail,
		Msg:             msg,
		ConversationRef: convRef,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agentID)
	return rr
}

// postOutboundRefOnly sends an outbound message with a conversation_ref and
// NO explicit recipient. This is the exact shape the CLI sends for conv:<uuid>,
// #<thread>, and @<agent> references (DEF-152).
func postOutboundRefOnly(t *testing.T, srv *Server, projectID, agentID, msg, convRef string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(OutboundMessageRequest{
		Msg:             msg,
		ConversationRef: convRef,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agentID)
	return rr
}

// postConvRefNoRecipient sends an outbound message with conversation_ref
// but NO explicit recipient — the exact shape the CLI produces.
func postConvRefNoRecipient(t *testing.T, srv *Server, projectID, agentID, msg, convRef string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(OutboundMessageRequest{
		Msg:             msg,
		ConversationRef: convRef,
	})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/"+agentID+"/outbound-message",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
	}}))
	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agentID)
	return rr
}

// def162Setup creates a server, project, agent, and human user wired for
// agent mention tests. The human is added as a project member via a
// role binding (PM1) with an unambiguous display name ("UniqueHuman162") that
// resolves to exactly one member (AC-3).
func def162Setup(t *testing.T) (srv *Server, s store.Store, project *store.Project, agent *store.Agent, human *store.User, topicID string) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	project = &store.Project{
		ID:   api.NewUUID(),
		Name: "def162-project",
		Slug: "def162-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	human = &store.User{
		ID:          api.NewUUID(),
		Email:       "uniquehuman162@example.com",
		DisplayName: "UniqueHuman162",
		Role:        "member",
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, human))

	agent = &store.Agent{
		ID:        api.NewUUID(),
		Name:      "NotifyBot",
		Slug:      "notifybot",
		ProjectID: project.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Add human as a project member via role binding (PM1).
	// resolveProjectHumanMembers now queries ListProjectMembers (role bindings).
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err, "project-member role definition must exist")
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      human.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Set up WebChatStore on the hub store's own database, as in
	// production: the topic's linked conversation is then the same row as
	// the thread:<project>:<topic> conversation an agent resolves, which
	// thread membership requires.
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store does not expose DB()")
	wcs := NewWebChatStore(dbProvider.DB(), "sqlite")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)

	// Create a topic for group conversations.
	topicID = api.NewUUID()
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: project.ID, Name: "def162-room",
		CreatedBy: human.ID, CreatedAt: time.Now(),
	}))

	return srv, s, project, agent, human, topicID
}

// def162GroupConv creates a group conversation whose ExternalRef encodes the
// given topicKey, and returns the conversation ID.
func def162GroupConv(t *testing.T, s store.Store, projectID, topicKey string) string {
	t.Helper()
	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:" + projectID + ":" + topicKey,
		ProjectID:   &projectID,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(context.Background(), conv)
	require.NoError(t, err)
	return created.ID
}

// postOutboundConvRef sends an agent outbound message to a conversation ref
// with the given message body. Returns the response recorder.
func postOutboundConvRef(t *testing.T, srv *Server, projectID, agentID, msg, convRef string) *httptest.ResponseRecorder {
	t.Helper()
	return postAgentOutboundRequest(t, srv, projectID, agentID, OutboundMessageRequest{
		Msg:             msg,
		ConversationRef: convRef,
	})
}

// def162MentionWaitTimeout is the deadline for a positive wait on the
// background thread-membership write (handlers_agent_messaging.go). 30s is
// generous headroom for a loaded CI runner; a passing run returns as soon as
// the row appears. Absence checks keep a short settle delay instead.
const def162MentionWaitTimeout = 30 * time.Second

// extractEnvelopeJSON pulls the raw JSON from between BEGIN/END SCION MESSAGE
// delimiters in a rendered DeliveryText string.
func extractEnvelopeJSON(t *testing.T, deliveryText string) string {
	t.Helper()
	const begin = "---BEGIN SCION MESSAGE---"
	const end = "---END SCION MESSAGE---"

	startIdx := strings.Index(deliveryText, begin)
	if startIdx < 0 {
		t.Fatalf("BEGIN delimiter not found in DeliveryText:\n%s", deliveryText)
	}
	startIdx += len(begin) + 1 // skip delimiter + newline

	endIdx := strings.LastIndex(deliveryText, end)
	if endIdx < 0 || endIdx <= startIdx {
		t.Fatalf("END delimiter not found in DeliveryText:\n%s", deliveryText)
	}
	endIdx-- // trim newline before end delimiter

	return deliveryText[startIdx:endIdx]
}

// sendAgentDM sends an agent-to-agent DM through the real HTTP handler.
func sendAgentDM(t *testing.T, srv *Server, sender *store.Agent, convID, projectID, msgText string) *httptest.ResponseRecorder {
	t.Helper()

	reqBody, err := json.Marshal(OutboundMessageRequest{
		ConversationRef: "conv:" + convID,
		Msg:             msgText,
		Type:            "instruction",
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+projectID+"/agents/"+sender.ID+"/outbound-message",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: sender.ID},
		ProjectID: projectID,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, sender.ID)
	return rr
}

// mustDMKey builds a DM key or fails the test.
func mustDMKey(t *testing.T, kindA, idA, kindB, idB string) string {
	t.Helper()
	key, err := messages.DMConversationKey(kindA, idA, kindB, idB)
	require.NoError(t, err)
	return key
}

// seedThreadConversation creates the native group conversation that a
// free-text thread_id resolves to in projectID, and returns it.
func seedThreadConversation(t *testing.T, s store.Store, projectID, threadID string) *store.Conversation {
	t.Helper()
	extRef, err := messaging.ThreadConversationExternalRef(projectID, threadID)
	require.NoError(t, err)
	pid := projectID
	conv, err := s.UpsertConversationByExternalRef(context.Background(), &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: extRef,
		DriftState:  "active",
		ProjectID:   &pid,
	})
	require.NoError(t, err)
	return conv
}

func countProjectConversations(t *testing.T, s store.Store, projectID string) int {
	t.Helper()
	res, err := s.ListConversations(context.Background(), store.ConversationFilter{ProjectID: projectID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	return len(res.Items)
}

// setupThreadTestChannels registers the given channels (each as a no-op
// spoke) on srv's broker proxy, so Channel:<name> passes
// validateChannelRegistered, and subscribes the project's user messages so a
// web send is persisted (asynchronously, by the in-process subscriber).
func setupThreadTestChannels(t *testing.T, srv *Server, s store.Store, project *store.Project, channels ...string) {
	t.Helper()
	buses := []eventbus.NamedEventBus{{Name: eventbus.InProcessBusName, Bus: eventbus.NewInProcessEventBus(slog.Default())}}
	for _, ch := range channels {
		buses = append(buses, eventbus.NamedEventBus{Name: ch, Bus: nullSpokeEventBus{}})
	}
	fanout := eventbus.NewFanOutEventBus(buses, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return nil }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)
	proxy.subscribeProjectUserMessages(project.ID)
}

// attachWebChatStore gives srv a real sqlite WebChatStore on s's database
// and returns it.
func attachWebChatStore(t *testing.T, srv *Server, s store.Store) WebChatStore {
	t.Helper()
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok)
	wcs := NewWebChatStore(dbProvider.DB(), "sqlite")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)
	return wcs
}

// assertOnlyControlMessage sends a plain control message (no thread) after a
// rejected send, waits for it to be persisted, then asserts it is the only
// message from the agent. The in-process subscriber persists in publish
// order, so a rejected message that had been published would be visible by
// the time the control row is.
func assertOnlyControlMessage(t *testing.T, srv *Server, s store.Store, project *store.Project, agent *store.Agent, user *store.User) {
	t.Helper()
	const control = "control message after rejection"
	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient: "user:" + user.Email,
		Msg:       control,
		Channel:   "web",
	})
	require.Equal(t, http.StatusOK, rr.Code, "control send: %s", rr.Body.String())
	waitForSenderMessage(t, s, agent.ID, control)

	rows, err := s.ListMessages(context.Background(), store.MessageFilter{SenderID: agent.ID}, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	require.Len(t, rows.Items, 1, "a rejected send must not persist a message")
	assert.Equal(t, control, rows.Items[0].Msg)
}

// permSeedUser ensures a user row exists so that group-membership / policy-binding
// foreign keys resolve. The Ent store enforces user/agent FK edges that the
// former raw-SQL store did not, so fixtures must create referenced principals.
func permSeedUser(t *testing.T, ctx context.Context, s store.Store, id string) {
	t.Helper()
	err := s.CreateUser(ctx, &store.User{
		ID: id, Email: id + "@example.com", DisplayName: "Seed User",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	})
	if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("seed user %s: %v", id, err)
	}
}

// projectAgentAuthzFixture exercises the project-scoped agent routes
// (GET/PATCH /api/v1/projects/{id}/agents/{agentId} and the project-scoped
// list) against every caller kind that matters: a project member, a hub user
// who is not a project member, a hub admin, another agent's JWT (same
// project, different agent), and an agent JWT scoped to a different project
// entirely. Shared by the list, get, and update authorization test files.
type projectAgentAuthzFixture struct {
	srv         *Server
	store       store.Store
	project     *store.Project
	other       *store.Project
	member      *store.User  // project owner/member
	plainMember *store.User  // project member, not owner or admin (no attach)
	nonMember   *store.User  // hub member, not a project member
	admin       *store.User  // hub admin (system-scope bypass)
	target      *store.Agent // the agent under test, in project
	caller      *store.Agent // a different agent in the same project
	stranger    *store.Agent // an agent in a different project
}

func projectAgentAuthzSetup(t *testing.T) *projectAgentAuthzFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	f := &projectAgentAuthzFixture{srv: srv, store: s}

	f.member = &store.User{
		ID: tid("paa-member"), Email: "paa-member@test.com",
		DisplayName: "Member", Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, f.member))
	ensureHubMembership(ctx, s, f.member.ID)

	f.plainMember = &store.User{
		ID: tid("paa-plain-member"), Email: "paa-plain-member@test.com",
		DisplayName: "PlainMember", Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, f.plainMember))
	ensureHubMembership(ctx, s, f.plainMember.ID)

	f.nonMember = &store.User{
		ID: tid("paa-nonmember"), Email: "paa-nonmember@test.com",
		DisplayName: "NonMember", Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, f.nonMember))
	ensureHubMembership(ctx, s, f.nonMember.ID)

	f.admin = &store.User{
		ID: tid("paa-admin"), Email: "paa-admin@test.com",
		DisplayName: "Admin", Role: store.UserRoleAdmin, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, f.admin))
	ensureHubMembership(ctx, s, f.admin.ID)
	saRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: saRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      f.admin.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	f.project = &store.Project{
		ID: tid("paa-proj"), Name: "PAA Project", Slug: "paa-project",
		OwnerID: f.member.ID, CreatedBy: f.member.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, f.project))
	srv.seedProjectCreatorMembership(ctx, f.project)
	msgAuthzAddProjectMember(t, s, f.plainMember.ID, f.project.ID, f.project.Slug, store.GroupMemberRoleMember)

	f.other = &store.Project{
		ID: tid("paa-other-proj"), Name: "PAA Other Project", Slug: "paa-other-project",
		OwnerID: f.member.ID, CreatedBy: f.member.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, f.other))
	srv.seedProjectCreatorMembership(ctx, f.other)

	mk := func(name, projectID string) *store.Agent {
		a := &store.Agent{
			ID: tid(name), Slug: tid(name), Name: name,
			ProjectID: projectID, Phase: string(state.PhaseStopped),
			CreatedBy: f.member.ID, OwnerID: f.member.ID,
			AppliedConfig: &store.AgentAppliedConfig{
				Env: map[string]string{"PLAIN_VAR": "plain-value", "GITHUB_TOKEN": "ghp_should_never_leak"},
				InlineConfig: &api.ScionConfig{
					Env: map[string]string{"INLINE_PLAIN_VAR": "inline-plain-value", "GITHUB_TOKEN": "ghp_should_never_leak"},
				},
			},
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}
	f.target = mk("paa-target", f.project.ID)
	f.caller = mk("paa-caller", f.project.ID)
	f.stranger = mk("paa-stranger", f.other.ID)

	return f
}

// setupProjectMembersTest creates a test server with two users and a project
// where "owner" has a project-owner role binding. "other" is a hub member
// with no project role.
func setupProjectMembersTest(t *testing.T) (srv *Server, st store.Store, owner *store.User, other *store.User, project *store.Project) {
	t.Helper()
	srv, st = testServer(t)
	ctx := context.Background()

	owner = &store.User{
		ID:          tid("pm-owner"),
		Email:       "owner@test.com",
		DisplayName: "Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, owner))

	other = &store.User{
		ID:          tid("pm-other"),
		Email:       "other@test.com",
		DisplayName: "Other",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, other))

	// Ensure both are hub members (get system-scoped bindings).
	ensureHubMembership(ctx, st, owner.ID)
	ensureHubMembership(ctx, st, other.ID)

	// Create project.
	project = &store.Project{
		ID:        tid("pm-project"),
		Name:      "Members Test Project",
		Slug:      "pm-test-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, st.CreateProject(ctx, project))

	// Create project-owner role binding for "owner" (simulates project creation).
	srv.seedProjectCreatorMembership(ctx, project)
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, project.ID, owner.ID))

	return srv, st, owner, other, project
}

func providersAuthzSetup(t *testing.T) *providersAuthzFixture {
	t.Helper()
	f := &providersAuthzFixture{bypassAgentsFixture: bypassAgentsSetup(t)}
	ctx := context.Background()

	f.target = &store.Project{
		ID:        tid("providers-target"),
		Name:      "Providers Target",
		Slug:      "providers-target",
		OwnerID:   f.owner.ID,
		CreatedBy: f.owner.ID,
	}
	require.NoError(t, f.store.CreateProject(ctx, f.target))
	// Project authority comes from the project-owner binding, not OwnerID
	// (ptone/scion#2586).
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(ctx, f.target.ID, f.owner.ID))

	// Both brokers are owned by the project owner: linking needs broker.update
	// on the broker in addition to project.update.
	mkBroker := func(name string) *store.RuntimeBroker {
		b := &store.RuntimeBroker{
			ID:        uuid.New().String(),
			Name:      name,
			Slug:      name,
			Status:    store.BrokerStatusOnline,
			CreatedBy: f.owner.ID,
			Created:   providersAuthzFixtureTime,
			Updated:   providersAuthzFixtureTime,
		}
		require.NoError(t, f.store.CreateRuntimeBroker(ctx, b))
		return b
	}
	f.linked = mkBroker("providers-linked")
	f.unlinked = mkBroker("providers-unlinked")
	require.NoError(t, f.store.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  f.target.ID,
		BrokerID:   f.linked.ID,
		BrokerName: f.linked.Name,
		Status:     store.BrokerStatusOnline,
	}))

	f.member = &store.User{
		ID:          tid("member-without-binding"),
		Email:       "member-without-binding@example.com",
		DisplayName: "Member Without Binding",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, f.store.CreateUser(ctx, f.member))
	ensureHubMembership(ctx, f.store, f.member.ID)

	return f
}

// providerCapacityView mirrors the fields of projectProviderView this test
// class cares about, decoded from the GET .../providers response body.
type providerCapacityView struct {
	BrokerID         string `json:"brokerId"`
	AgentLimit       *int64 `json:"agentLimit"`
	AgentCount       *int64 `json:"agentCount"`
	AgentLimitSource string `json:"agentLimitSource"`
}

// registerBrokerTestExistingBroker inserts a broker directly into the store
// (skipping HTTP registration) with a recorded creator and non-default
// field values, so a denied overwrite attempt can be checked for no effect.
func registerBrokerTestExistingBroker(t *testing.T, s store.Store, name, createdBy string) *store.RuntimeBroker {
	t.Helper()
	broker := &store.RuntimeBroker{
		ID:              tid("register-broker-" + name),
		Name:            name,
		Slug:            api.Slugify(name),
		Version:         "1.0.0-baseline",
		Status:          store.BrokerStatusOnline,
		ConnectionState: "connected",
		Capabilities:    &store.BrokerCapabilities{WebPTY: false, Sync: false, Attach: false},
		CreatedBy:       createdBy,
		Created:         time.Now(),
		Updated:         time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), broker))
	return broker
}

// countProjectQuotaReservations reads back the number of active
// max_projects_per_user reservations held by userID, so a test can assert a
// denied register call did not consume a quota slot.
func countProjectQuotaReservations(t *testing.T, s store.Store, userID string) int64 {
	t.Helper()
	ctx := context.Background()
	limitDef, err := s.GetLimitDefinitionByName(ctx, "max_projects_per_user")
	require.NoError(t, err)
	count, err := s.CountActiveReservations(ctx, limitDef.ID, userID, "system", "system")
	require.NoError(t, err)
	return count
}

// setUserProjectQuotaCeiling overrides the seeded max_projects_per_user limit
// (default 0, meaning unlimited/unenforced) to a finite value, so
// CheckAndReserve actually records a reservation instead of skipping
// enforcement entirely. Without this, a quota-unchanged assertion is
// vacuous: it would read 0 before and after regardless of ordering, because
// nothing ever reserves against an unlimited quota.
func setUserProjectQuotaCeiling(t *testing.T, s store.Store, value int64) {
	t.Helper()
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxProjectsPerUser)
	require.NoError(t, err, "max_projects_per_user must be seeded by New()/seedLimitDefinitions")
	def.DefaultValue = value
	_, err = s.UpdateLimitDefinition(context.Background(), def)
	require.NoError(t, err)
}

// seedHubMemberNoProjects creates an active member user and puts them in the
// hub-members group, exactly as a newly signed-in user would be. It
// deliberately creates no project.
func seedHubMemberNoProjects(t *testing.T, s store.Store, name string) *store.User {
	t.Helper()
	ctx := context.Background()

	user := &store.User{
		ID:          tid(name + "-user"),
		Email:       name + "@test.com",
		DisplayName: name,
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)
	return user
}

// enableReadSwitch configures OperationalSettings on the server with the
// ConversationReadSwitch flag ON. After this call, handlers that check
// s.GetOperationalSettings().ConversationReadSwitch() will enter the
// Phase 8 conversation-resolution branch.
func enableReadSwitch(t *testing.T, srv *Server) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	fakeStore.seed("messaging", json.RawMessage(`{"conversation_envelope_switch":true}`))
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("ops.Refresh failed: %v", err)
	}
	srv.SetOperationalSettings(ops)
	// Canary: verify the switch is actually on. Without this, a silent
	// failure in enableReadSwitch makes every delta==0 assertion pass
	// trivially — the handler never enters the read-switch block at all.
	if !srv.GetOperationalSettings().ConversationEnvelopeSwitch() {
		t.Fatalf("enableReadSwitch: ConversationEnvelopeSwitch() is still false after setup — " +
			"every FlagOn test in this file is vacuous without this guard")
	}
}

// seedConversation creates a conversation in the store with the given
// surface and externalRef, returning its auto-assigned ID. The caller uses
// this to set up the "conversation resolves" precondition.
func seedConversation(t *testing.T, s store.Store, surface, externalRef, kind string) string {
	t.Helper()
	conv := &store.Conversation{
		Surface:     surface,
		ExternalRef: externalRef,
		Kind:        kind,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(context.Background(), conv)
	if err != nil {
		t.Fatalf("seedConversation(%s, %s): %v", surface, externalRef, err)
	}
	return created.ID
}

// rsAgent creates an agent in the store with the given name and projectID.
// Prefixed "rs" (read-switch) to avoid collision with seedAgent in other
// test files in the same package.
func rsAgent(t *testing.T, s store.Store, agentName, projectID string) string {
	t.Helper()
	agentID := tid(agentName)
	agent := &store.Agent{
		ID:        agentID,
		Slug:      agentName,
		Name:      agentName,
		ProjectID: projectID,
		OwnerID:   DevUserID, // CO1: owner bypass grants manage access to the dev user
	}
	if err := s.CreateAgent(context.Background(), agent); err != nil {
		t.Fatalf("rsAgent(%s): %v", agentName, err)
	}
	return agentID
}

// rsProject creates a project in the store with required fields populated.
func rsProject(t *testing.T, s store.Store, projectName string) string {
	t.Helper()
	projectID := tid(projectName)
	project := &store.Project{
		ID:      projectID,
		Name:    projectName,
		Slug:    projectName,
		OwnerID: DevUserID,
	}
	if err := s.CreateProject(context.Background(), project); err != nil {
		t.Fatalf("rsProject(%s): %v", projectName, err)
	}
	return projectID
}

// makeDMKey builds a valid 5-part DM key for the dev user and the given agent UUID.
// Format: dm:agent:<agentUUID>:user:<userUUID> (sorted lexicographically by token).
func makeDMKey(agentUUID, userUUID string) string {
	// "agent:" < "user:" lexicographically, so agent token comes first.
	return fmt.Sprintf("dm:agent:%s:user:%s", agentUUID, userUUID)
}

// seedRolesTestUser creates a user in the store if it does not already exist.
// The store requires user principals to exist before role bindings can reference them.
func seedRolesTestUser(t *testing.T, s store.Store, id, email string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.GetUser(ctx, id); err == nil {
		return // already exists
	}
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: id, Email: email, DisplayName: email, Role: "member", Status: "active",
	}))
}

// createRoleViaAPI creates a role definition through the handler and returns it.
func createRoleViaAPI(t *testing.T, srv *Server, req createRoleDefinitionRequest) *store.RoleDefinition {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/roles", req)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var def store.RoleDefinition
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&def))
	return &def
}

// createBindingViaAPI creates a role binding through the handler and returns it.
func createBindingViaAPI(t *testing.T, srv *Server, req createRoleBindingRequest) *store.RoleBinding {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", req)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var rb store.RoleBinding
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&rb))
	return &rb
}

// doRequestAsIdentity performs an HTTP request with a custom identity injected
// into the context. This bypasses the dev-auth super-admin fast-path by using
// an AuthenticatedUser whose Role() != "admin", so that CanDelegate and
// permission-based route guards are actually exercised.
func doRequestAsIdentity(t *testing.T, srv *Server, identity Identity, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("failed to marshal body: %v", err)
		}
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Inject the custom identity directly into the context so the route
	// guard and handler see a non-admin user instead of the DevUser.
	ctx := contextWithIdentity(req.Context(), identity)
	req = req.WithContext(ctx)

	// Serve using the mux directly (bypassing auth middleware) since the
	// identity is already in the context. The route guards and handlers
	// call GetIdentityFromContext, which will find our injected identity.
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	return rec
}

// setupNonAdminUser creates a non-admin user identity and grants it specific
// permissions via a custom role binding. Returns the identity.
//
// The user gets a role binding granting the specified permissions at system scope.
func setupNonAdminUser(t *testing.T, st store.Store, perms []string) *AuthenticatedUser {
	t.Helper()
	ctx := t.Context()

	// Ensure the user exists in the store (CreateRoleBinding validates principal existence).
	seedRolesTestUser(t, st, nonAdminUserID, nonAdminUserEmail)

	user := NewAuthenticatedUser(nonAdminUserID, nonAdminUserEmail, "Scoped Admin", "member", "api")

	// Create a custom role definition with the requested permissions.
	rd, err := st.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "test-scoped-admin-" + t.Name(),
		Description: "Test scoped admin role",
		ScopeType:   store.RoleScopeSystem,
		Permissions: perms,
		System:      false,
	})
	require.NoError(t, err)

	// Bind it to our test user.
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      nonAdminUserID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test-setup",
	})
	require.NoError(t, err)

	return user
}

// doDeleteBindingWithCredentialKind creates a request to the generic DELETE
// role-bindings endpoint with a specific credential kind injected directly
// into the context, bypassing the auth middleware.
func doDeleteBindingWithCredentialKind(
	t *testing.T, srv *Server, user *store.User,
	credKind CredentialKind, bindingID string,
) *httptest.ResponseRecorder {
	t.Helper()

	path := "/api/v1/admin/role-bindings/" + bindingID
	req := httptest.NewRequest(http.MethodDelete, path, nil)

	ctx := req.Context()
	identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")
	ctx = contextWithIdentity(ctx, identity)
	ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: credKind})
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	srv.handleAdminRoleBindingByID(rec, req)
	return rec
}

// brokerAuthSetup creates a server with broker auth enabled, a runtime broker,
// and a non-admin user who has no access policies for the broker.
func brokerAuthSetup(t *testing.T) *brokerAuthFixture {
	t.Helper()

	// Use bypassAgentsServer which configures broker auth (HMAC).
	srv, s := bypassAgentsServer(t)
	ctx := context.Background()
	f := &brokerAuthFixture{srv: srv, store: s}

	// Create a runtime broker with HMAC secret.
	f.brokerSecret = []byte("broker-auth-test-secret-32bytes!")
	f.broker = &store.RuntimeBroker{
		ID:      uuid.New().String(),
		Name:    "auth-test-broker",
		Slug:    "auth-test-broker",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, f.broker))
	require.NoError(t, s.CreateBrokerSecret(ctx, &store.BrokerSecret{
		BrokerID:  f.broker.ID,
		SecretKey: f.brokerSecret,
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		Status:    store.BrokerSecretStatusActive,
	}))

	// Create a regular member user with no policies granting broker access.
	f.deniedUser = &store.User{
		ID:          tid("broker-auth-denied-user"),
		Email:       "denied@example.com",
		DisplayName: "Denied User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, f.deniedUser))

	return f
}

// getProjectErrStore wraps a store and forces GetProject to fail for one
// specific project ID with a caller-supplied error, leaving every other
// method (including GetProject for any other ID) untouched. Used to exercise
// getBrokerProjects' handling of a provider record whose project lookup
// fails, without needing a real deleted-row or connection-failure fixture.
type getProjectErrStore struct {
	store.Store
	fault     *storeFaultSwitch // nil: always active
	projectID string
	err       error
}

// countingBrokerLoadStore wraps a store.Store and counts calls to
// GetRuntimeBroker, optionally injecting an error, so a test can prove the
// heartbeat handler's lazily-loaded, memoised broker read (ptone/scion#2262,
// loadHeartbeatBroker) behaves as described: at most one read per heartbeat
// regardless of how many callers need it, no read at all when nothing needs
// it, and a failed read that the handler recovers from without crashing.
//
// updateRuntimeBrokerCalls counts broker row writes, so a test can prove the
// heartbeat handler writes the row only when the refreshed state changed.
type countingBrokerLoadStore struct {
	store.Store
	getRuntimeBrokerCalls    int
	getRuntimeBrokerErr      error
	updateRuntimeBrokerCalls int
}

func setupScheduledEventTest(t *testing.T) (*Server, store.Store, string) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	// Initialize the scheduler (normally done by Server.Start)
	srv.scheduler = NewScheduler(s, slog.Default())
	srv.scheduler.RegisterEventHandler("message", srv.messageEventHandler())

	project := &store.Project{
		ID:   tid("project-sched-test"),
		Name: "Scheduler Test Project",
		Slug: "sched-test-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))
	seedScheduleAuthorAgent(t, s, project.ID)

	return srv, s, project.ID
}

func doScheduledEventAgentRequest(t *testing.T, srv *Server, identity Identity, projectID string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	bodyBytes, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	if identity != nil {
		req = req.WithContext(contextWithIdentity(req.Context(), identity))
	}

	rec := httptest.NewRecorder()
	srv.handleScheduledEvents(rec, req, projectID, "")
	return rec
}

// federatedTestIdentity implements UserIdentity with Type() = "federated_user"
// and a UUID-based ID, enabling end-to-end authorization tests through the
// store layer which requires UUID user IDs.
type federatedTestIdentity struct {
	id          string
	email       string
	displayName string
	role        string
}

func setupScheduleTest(t *testing.T) (*Server, store.Store, string) {
	t.Helper()
	srv, s := testServer(t)
	return initScheduleTest(t, srv, s)
}

// initScheduleTest gives srv a scheduler with the message handler and creates
// the schedule test project.
func initScheduleTest(t *testing.T, srv *Server, s store.Store) (*Server, store.Store, string) {
	t.Helper()
	ctx := context.Background()

	srv.scheduler = NewScheduler(s, slog.Default())
	srv.scheduler.RegisterEventHandler("message", srv.messageEventHandler())

	project := &store.Project{
		ID:   tid("project-sched-recurring"),
		Name: "Schedule Test Project",
		Slug: "schedule-test-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))
	seedScheduleAuthorAgent(t, s, project.ID)

	return srv, s, project.ID
}

func doScheduleAgentRequest(t *testing.T, srv *Server, identity Identity, projectID, schedulePath, method string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	bodyBytes, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(method, "/api/v1/projects/"+projectID+"/schedules/"+schedulePath, bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	if identity != nil {
		req = req.WithContext(contextWithIdentity(req.Context(), identity))
	}

	rec := httptest.NewRecorder()
	srv.handleSchedules(rec, req, projectID, schedulePath)
	return rec
}

// setupScopedDispatchAgentOwner creates a project-owner user for
// scoped-UAT-vs-session-user comparisons in the dispatch_agent authoring gate
// tests below: the unscoped identity has full project-owner authority, and a
// ScopedUserIdentity wrapping the same user ID is used to exercise the gate.
func setupScopedDispatchAgentOwner(t *testing.T, srv *Server, s store.Store, projectID, userID string) UserIdentity {
	t.Helper()
	ctx := context.Background()

	ownerUser := NewAuthenticatedUser(userID, userID+"@test.com", "Dispatch Schedule Owner", "member", "api")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          userID,
		Email:       ownerUser.Email(),
		DisplayName: ownerUser.DisplayName(),
		Role:        "member",
		Status:      "active",
	}))

	project, err := s.GetProject(ctx, projectID)
	require.NoError(t, err)
	srv.seedProjectCreatorMembership(ctx, project)
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, projectID, userID))

	return ownerUser
}

// checkJSONError asserts that the response body is a valid JSON ErrorResponse.
func checkJSONError(t *testing.T, body string) {
	t.Helper()
	var errResp ErrorResponse
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Errorf("expected JSON error body, got non-JSON: %s", body)
		return
	}
	if errResp.Error.Code == "" {
		t.Errorf("expected non-empty error.code in JSON response, got: %s", body)
	}
	if errResp.Error.Message == "" {
		t.Errorf("expected non-empty error.message in JSON response, got: %s", body)
	}
}

func setupProjectSecretTest(t *testing.T) (*Server, string) {
	t.Helper()
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	projectID := tid("proj-secret-b64")
	project := &store.Project{
		ID:      projectID,
		Name:    "Encoding Test Project",
		Slug:    "encoding-test-project",
		OwnerID: DevUserID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	return srv, projectID
}

func setupBrokerSecretTest(t *testing.T) (*Server, string) {
	t.Helper()
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	brokerID := tid("broker-secret-b64")
	broker := &store.RuntimeBroker{
		ID:      brokerID,
		Name:    "Encoding Test Broker",
		Slug:    "encoding-test-broker",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}
	return srv, brokerID
}

// setupInjectedSkillsTest creates a test server with a project owned by alice
// and a second user bob who is NOT a project member. The dev user (from testServer)
// is a hub admin. All three identities can be used via doRequest (dev/admin),
// doRequestAsUser(alice), or doRequestAsUser(bob).
func setupInjectedSkillsTest(t *testing.T) (*Server, store.Store, *store.Project, *store.User, *store.User) {
	t.Helper()

	srv, s := testServer(t)
	ctx := context.Background()

	alice := &store.User{
		ID:          tid("si-user-alice"),
		Email:       "alice@skills-test.com",
		DisplayName: "Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))
	ensureHubMembership(ctx, s, alice.ID)

	bob := &store.User{
		ID:          tid("si-user-bob"),
		Email:       "bob@skills-test.com",
		DisplayName: "Bob",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, bob))
	ensureHubMembership(ctx, s, bob.ID)

	project := &store.Project{
		ID:        tid("si-project-alpha"),
		Name:      "Alpha Project",
		Slug:      "alpha-project",
		OwnerID:   alice.ID,
		CreatedBy: alice.ID,
	}
	require.NoError(t, s.CreateProject(ctx, project))
	// Create the project members group so authz works correctly.
	// seedProjectCreatorMembership also adds alice (CreatedBy) as an owner.
	srv.seedProjectCreatorMembership(ctx, project)

	return srv, s, project, alice, bob
}

// tiGroupWithSystemRole creates an explicit group, optionally owned by
// ownerID, and binds the named system role to it.
func tiGroupWithSystemRole(t *testing.T, s store.Store, name, roleName, ownerID string) *store.Group {
	t.Helper()
	ctx := context.Background()
	g := &store.Group{ID: tid(name), Name: name, Slug: name, GroupType: store.GroupTypeExplicit,
		Created: time.Now(), Updated: time.Now(), CreatedBy: ownerID, OwnerID: ownerID}
	require.NoError(t, s.CreateGroup(ctx, g))
	if roleName != "" {
		tiBindSystemRoleToGroup(t, s, g.ID, roleName)
	}
	return g
}

// tiCustomSystemRole creates a system-scoped custom role with perms and
// returns its name. Super-admin is direct-user-only, so a group carries
// hub-level authority through hub-admin or a custom system role.
func tiCustomSystemRole(t *testing.T, s store.Store, name string, perms ...string) string {
	t.Helper()
	_, err := s.CreateRoleDefinition(context.Background(), &store.RoleDefinition{
		ID: generateID(), Name: name, Description: "test identity clamp test role",
		ScopeType: store.RoleScopeSystem, Permissions: perms,
	})
	require.NoError(t, err)
	return name
}

func tiAddToGroup(t *testing.T, s store.Store, groupID, userID, role string) {
	t.Helper()
	require.NoError(t, s.AddGroupMember(context.Background(), &store.GroupMember{
		GroupID: groupID, MemberType: store.GroupMemberTypeUser, MemberID: userID, Role: role, AddedAt: time.Now()}))
}

// unwrapTestFixtureClamp returns the store under the authorization
// service's test-identity grant clamp.
func unwrapTestFixtureClamp(s store.Store) store.Store {
	if c, ok := s.(*testFixtureGrantClamp); ok {
		return c.Store
	}
	return s
}

// kindErrStore fails every user lookup with a store error.
type kindErrStore struct{ store.Store }

// enableTestIdentitiesForTest turns the feature on for a server built by a
// shared helper (the effect of --test-mode=member).
func enableTestIdentitiesForTest(srv *Server) {
	srv.testIdentities = newTestIdentityState(true)
	srv.authConfig.TestIdentitiesEnabled = true
}

// enableTestIdentityWebSessionsForTest gives srv a hub-local test identity
// web session key (the effect of a test-infra marker with a key), so test
// identity web sessions are available while test identities are on.
func enableTestIdentityWebSessionsForTest(srv *Server) {
	key := make([]byte, testIdentityWebKeyLen)
	for i := range key {
		key[i] = byte(i + 1)
	}
	srv.testIdentityWeb = newTestIdentityWebState(TestIdentityWebSessionKey{Key: key})
}

// newTestIdentityServer builds a test server with the feature on or off.
func newTestIdentityServer(t *testing.T, enabled bool) (*Server, store.Store) {
	t.Helper()
	srv, s := testServer(t)
	if enabled {
		enableTestIdentitiesForTest(srv)
	}
	return srv, s
}

// tiIssuer creates a super-admin user and returns its ID and a hub-bound
// access token carrying only test_identity:issue: the issuer credential.
func tiIssuer(t *testing.T, srv *Server, s store.Store, name string) (string, string) {
	t.Helper()
	id := hubConfigTokenUser(t, s, name, store.SystemRoleSuperAdmin)
	return id, mintHubConfigToken(t, srv, id, hubBoundary(), "test_identity:issue")
}

func tiPost(t *testing.T, srv *Server, token, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestWithToken(t, srv, token, http.MethodPost, path, body)
}

// tiIssue issues a test identity with token and returns the decoded
// response, requiring 201.
func tiIssue(t *testing.T, srv *Server, token string, body interface{}) TestIdentityTokenResponse {
	t.Helper()
	rec := tiPost(t, srv, token, "/api/v1/test-identities", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	return tiDecode(t, rec)
}

// tiStoreFixture writes a test-fixture row directly through the store's
// dedicated create path.
func tiStoreFixture(t *testing.T, s store.Store, issuer string, expiresAt time.Time) *store.User {
	t.Helper()
	u := &store.User{
		ID:          generateID(),
		Email:       "test-identity-" + generateID()[:12] + "@" + store.TestFixtureEmailDomain,
		DisplayName: "Stored fixture",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
		Kind:        store.UserKindTestFixture,
		ExpiresAt:   &expiresAt,
		IssuedBy:    &issuer,
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateTestFixtureUser(context.Background(), u))
	ensureHubMembership(context.Background(), s, u.ID)
	return u
}

func tiAuthMe(t *testing.T, srv *Server, token string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestWithToken(t, srv, token, http.MethodGet, "/api/v1/auth/me", nil)
}

// tiCreateProject creates a project as token and returns its ID.
func tiCreateProject(t *testing.T, srv *Server, token, name string) string {
	t.Helper()
	rec := doRequestWithToken(t, srv, token, http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: name})
	require.True(t, rec.Code == http.StatusCreated || rec.Code == http.StatusOK, "create project: %d %s", rec.Code, rec.Body.String())
	var p store.Project
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p), rec.Body.String())
	require.NotEmpty(t, p.ID, rec.Body.String())
	return p.ID
}

// setupUserTemplateTest creates a test server with two users (alice and bob)
// and returns the server, store, and both users. Both users have hub membership.
func setupUserTemplateTest(t *testing.T) (*Server, store.Store, *store.User, *store.User) {
	t.Helper()

	srv, s := testServer(t)
	ctx := context.Background()

	alice := &store.User{
		ID:          tid("ut-alice"),
		Email:       "alice@test.com",
		DisplayName: "Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))

	bob := &store.User{
		ID:          tid("ut-bob"),
		Email:       "bob@test.com",
		DisplayName: "Bob",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, bob))

	ensureHubMembership(ctx, s, alice.ID)
	ensureHubMembership(ctx, s, bob.ID)

	return srv, s, alice, bob
}

// createUserTemplate creates a user-scoped template directly in the store.
func createUserTemplate(t *testing.T, s store.Store, ownerID, name string) *store.Template {
	t.Helper()
	tmpl := &store.Template{
		ID:        api.NewUUID(),
		Name:      name,
		Slug:      api.Slugify(name),
		Harness:   "antigravity",
		Scope:     store.TemplateScopeUser,
		ScopeID:   ownerID,
		OwnerID:   ownerID,
		CreatedBy: ownerID,
		Status:    store.TemplateStatusActive,
	}
	require.NoError(t, s.CreateTemplate(context.Background(), tmpl))
	return tmpl
}

// parseErrorResponse decodes an HTTP response body into ErrorResponse.
func parseErrorResponse(t *testing.T, body []byte) ErrorResponse {
	t.Helper()
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(body, &resp), "response body: %s", string(body))
	return resp
}

// assertStructuredDenial verifies the 403 response has the expected structured
// denial detail and redacts internal information.
func assertStructuredDenial(t *testing.T, resp ErrorResponse, wantResourceType, wantAction string) {
	t.Helper()

	assert.Equal(t, ErrCodeForbidden, resp.Error.Code, "error code")
	require.NotNil(t, resp.Error.Details, "expected structured details in 403 response")

	if wantResourceType != "" {
		got, ok := resp.Error.Details["resource_type"]
		assert.True(t, ok, "expected details.resource_type")
		assert.Equal(t, wantResourceType, got, "details.resource_type")
	}
	if wantAction != "" {
		got, ok := resp.Error.Details["denied_action"]
		assert.True(t, ok, "expected details.denied_action")
		assert.Equal(t, wantAction, got, "details.denied_action")
	}
}

func newProvisionFixture(t *testing.T) *provisionFixture {
	t.Helper()
	srv, s := testServerNoDevAuth(t)
	return newProvisionFixtureOn(t, srv, s)
}

// testServerNoDevAuth is testServer with dev auth disabled. Provisioning is
// refused on a hub in dev-auth mode (row 4a), so the provisioning tests run
// on a hub that only has sign-in sessions; callers authenticate with
// session tokens (doRequestAsUser, provisionAs).
func testServerNoDevAuth(t *testing.T) (*Server, store.Store) {
	t.Helper()
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("Skipping test because sqlite driver is not registered (build with -tags sqlite to enable)")
		}
		t.Fatalf("failed to create test store: %v", err)
	}
	_ = s.DeleteHubSetting(context.Background(), "migration_delegation_edge_backfill_v1")
	cfg := testServerConfig()
	cfg.DevAuthToken = "" // dev-auth off
	srv, st := testServerWithStoreConfig(t, s, cfg)
	require.False(t, srv.authConfig.DevAuthEnabled)
	return srv, st
}

// provisionAs sends body (marshalled to JSON) as user.
func provisionAs(t *testing.T, srv *Server, user *store.User, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	return provisionRaw(t, srv, user, string(raw))
}

func provisionErr(t *testing.T, rec *httptest.ResponseRecorder) (code string, details map[string]interface{}) {
	t.Helper()
	resp := parseErrorResponse(t, rec.Body.Bytes())
	return resp.Error.Code, resp.Error.Details
}

// superAdminBindingCount counts system-scoped super-admin bindings for a user.
func superAdminBindingCount(t *testing.T, s store.Store, userID string) int {
	t.Helper()
	ctx := context.Background()

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)

	count := 0
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
			count++
		}
	}
	return count
}

// getDevUser triggers provisioning and returns the dev user record.
func getDevUser(t *testing.T, srv *Server, s store.Store) *store.User {
	t.Helper()
	ctx := context.Background()

	// Trigger dev user creation.
	doRequest(t, srv, http.MethodGet, "/api/v1/users", nil)

	result, err := s.ListUsers(ctx, store.UserFilter{}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	for i := range result.Items {
		if result.Items[i].Email == "dev@localhost" {
			return &result.Items[i]
		}
	}
	t.Fatal("dev user not found")
	return nil
}

// assertHubRoleAccess checks the immediate authz effect of a hub role:
// template.list is allowed for member and viewer; project.create is allowed
// only for member.
func assertHubRoleAccess(t *testing.T, srv *Server, s store.Store, userID string, wantProjectCreate bool) {
	t.Helper()
	ctx := context.Background()
	u, err := s.GetUser(ctx, userID)
	require.NoError(t, err)
	identity := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "web")

	d := srv.authzService.CheckAccess(ctx, identity, templateScopeResource(store.TemplateScopeGlobal, ""), ActionList)
	assert.True(t, d.Allowed, "template.list should be allowed; reason=%q", d.Reason)

	d = srv.authzService.CheckAccess(ctx, identity, Resource{Type: "project"}, ActionCreate)
	assert.Equal(t, wantProjectCreate, d.Allowed, "project.create allowed; reason=%q", d.Reason)
}

func (p *deleteCountingEventPublisher) PublishAgentDeleted(_ context.Context, _, _ string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.count++
}

func (p *deleteCountingEventPublisher) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.count
}

func (alwaysDropUserBus) Publish(context.Context, string, *messages.StructuredMessage) error {
	return eventbus.ErrSubscriberBufferFull
}

func (alwaysDropUserBus) Subscribe(string, eventbus.EventHandler) (eventbus.Subscription, error) {
	return nullSub{}, nil
}

func (alwaysDropUserBus) Close() error { return nil }

func (stubManagedAgentBackend) Name() string { return "stub" }

func (stubManagedAgentBackend) CreateAgent(ctx context.Context, cfg managedagent.CreateAgentConfig) (string, error) {
	return "stub-cloud-agent", nil
}

func (stubManagedAgentBackend) DeleteAgent(ctx context.Context, cloudAgentID string) error {
	return nil
}

func (stubManagedAgentBackend) CreateInteraction(ctx context.Context, req managedagent.InteractionRequest) (*managedagent.InteractionHandle, error) {
	return &managedagent.InteractionHandle{InteractionID: "stub-interaction"}, nil
}

func (stubManagedAgentBackend) GetInteraction(ctx context.Context, interactionID string) (*managedagent.InteractionState, error) {
	return &managedagent.InteractionState{InteractionID: interactionID}, nil
}

func (stubManagedAgentBackend) CancelInteraction(ctx context.Context, interactionID string) error {
	return nil
}

func (stubManagedAgentBackend) StreamInteraction(ctx context.Context, interactionID string, lastEventID string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("stubManagedAgentBackend: streaming not supported")
}

// moveCaller is the agent itself with agent-create scope (needed to
// dispatch to another broker).
func (f *moveFixture) moveCaller() AgentIdentity {
	return agentIdentityFor(f.agent.ID, f.project.ID, ScopeAgentCreate)
}

func (f *moveFixture) reincarnate(t *testing.T, body ReincarnateAgentRequest) *httptest.ResponseRecorder {
	t.Helper()
	req := reincarnateRequest(t, f.agent.ID, f.moveCaller(), body)
	rec := httptest.NewRecorder()
	f.srv.handleReincarnateAgent(rec, req, f.agent.ID)
	return rec
}

// assertNoMoveSideEffects checks that nothing a move would write was
// written: the agent row, its reincarnation history, provider links, the
// project's default broker, the dispatcher, and the agent count.
func (f *moveFixture) assertNoMoveSideEffects(t *testing.T, agentCount int) {
	t.Helper()
	ctx := context.Background()
	after, err := f.s.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	assert.Equal(t, f.agent.StateVersion, after.StateVersion, "agent row must be untouched")
	assert.Equal(t, f.agent.RuntimeBrokerID, after.RuntimeBrokerID, "agent must stay on its broker")
	assert.Equal(t, 1, after.Generation)
	assert.Equal(t, "", after.ReincarnationState)

	list, err := f.s.ListAgentReincarnations(ctx, f.agent.ID)
	require.NoError(t, err)
	assert.Empty(t, list, "no reincarnation record")

	project, err := f.s.GetProject(ctx, f.project.ID)
	require.NoError(t, err)
	assert.Equal(t, f.src.ID, project.DefaultRuntimeBrokerID, "project default broker unchanged")

	n, err := f.s.CountAgents(ctx, store.AgentFilter{})
	require.NoError(t, err)
	assert.Equal(t, agentCount, n, "no agent row created")

	assert.Zero(t, f.disp.stopCalls)
	assert.Zero(t, f.disp.reprovisionCalls)
	assert.Zero(t, f.disp.startCalls)
}

func (f *moveFixture) agentCount(t *testing.T) int {
	t.Helper()
	n, err := f.s.CountAgents(context.Background(), store.AgentFilter{})
	require.NoError(t, err)
	return n
}

// unprivilegedUser makes the agent's owner a user with no broker rights and
// only the project member role, and returns a request func acting as that
// user.
func (f *moveFixture) unprivilegedUser(t *testing.T) (*store.User, func(ReincarnateAgentRequest) *httptest.ResponseRecorder) {
	t.Helper()
	ctx := context.Background()
	user := &store.User{
		ID: tid("move-user-" + t.Name()), Email: "move-user@example.com", DisplayName: "Move User",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, f.s.CreateUser(ctx, user))
	// Reincarnating the agent records the user as its delegator, which
	// needs agent.create in the project. The project member role grants
	// it and no broker read.
	createTestUserWithProjectRole(t, f.s, user.ID, user.Email, f.project.ID, store.ProjectRoleMember)
	f.agent.OwnerID = user.ID
	f.agent.CreatedBy = user.ID
	require.NoError(t, f.s.UpdateAgent(ctx, f.agent))
	agent, err := f.s.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	f.agent = agent

	caller := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, string(ClientTypeWeb))
	return user, func(body ReincarnateAgentRequest) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		f.srv.handleReincarnateAgent(rec, reincarnateRequest(t, f.agent.ID, caller, body), f.agent.ID)
		return rec
	}
}

// addMoveBroker stores another move-eligible broker, linked to the project
// (and so visible to the agent caller) when provider is true.
func (f *moveFixture) addMoveBroker(t *testing.T, id, name, slug string, provider bool) *store.RuntimeBroker {
	t.Helper()
	ctx := context.Background()
	b := &store.RuntimeBroker{
		ID: tid(id + t.Name()), Name: name, Slug: slug,
		Status: store.BrokerStatusOnline, WorkspaceStorage: moveFixtureStorage(),
		Capabilities: &store.BrokerCapabilities{Reprovision: true, AgentMove: true},
		Profiles:     moveFixtureProfiles(), DefaultProfile: "k8s",
	}
	require.NoError(t, f.s.CreateRuntimeBroker(ctx, b))
	if provider {
		require.NoError(t, f.s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: f.project.ID, BrokerID: b.ID, BrokerName: b.Name, Status: store.BrokerStatusOnline,
		}))
	}
	return b
}

func (f *moveFixture) assertTargetResolvesTo(t *testing.T, target string, want *store.RuntimeBroker) {
	t.Helper()
	rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: target})
	require.Equal(t, http.StatusOK, rec.Code, "target %q: %s", target, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, want.ID, resp.TargetBrokerID, "target %q", target)
}

func moveFixtureProfiles() []store.BrokerProfile {
	return []store.BrokerProfile{{Name: "k8s", Type: "kubernetes", Available: true}}
}

// reprovisionSnapshot returns the reprovision call count and a copy of the
// dispatched configs.
func (d *reincarnateTestDispatcher) reprovisionSnapshot() (int, []store.AgentAppliedConfig) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reprovisionCalls, append([]store.AgentAppliedConfig(nil), d.reprovisionConfigs...)
}

func (d *reincarnateTestDispatcher) ImageRegistryFor(brokerID, profile string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.registryCalls = append(d.registryCalls, [2]string{brokerID, profile})
	if d.imageRegistryByBroker != nil {
		return d.imageRegistryByBroker[brokerID]
	}
	return d.imageRegistry
}

func (d *reincarnateTestDispatcher) DispatchAgentCreate(context.Context, *store.Agent) (*CreateDispatchResult, error) {
	return nil, nil
}
func (d *reincarnateTestDispatcher) DispatchAgentProvision(context.Context, *store.Agent) error {
	return nil
}
func (d *reincarnateTestDispatcher) DispatchAgentReprovision(_ context.Context, agent *store.Agent) error {
	d.mu.Lock()
	d.reprovisionCalls++
	if agent.AppliedConfig != nil {
		d.reprovisionConfigs = append(d.reprovisionConfigs, *agent.AppliedConfig)
	}
	err := d.reprovisionErr
	image := d.reprovisionImage
	echo := d.reprovisionEcho
	if d.reprovisionCalls > 1 {
		err, image, echo = d.rerenderErr, "", d.rerenderEcho
	}
	d.mu.Unlock()
	if err == nil && image != "" && agent.AppliedConfig != nil {
		agent.AppliedConfig.Image = image
	}
	if err == nil && echo != nil && agent.AppliedConfig != nil {
		echo(agent.AppliedConfig)
	}
	return err
}
func (d *reincarnateTestDispatcher) DispatchAgentProvisionForMove(_ context.Context, agent *store.Agent, expect, moveID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.moveProvisionCalls = append(d.moveProvisionCalls, agent.RuntimeBrokerID+"|"+expect)
	d.moveProvisionIDs = append(d.moveProvisionIDs, moveID)
	if d.moveProvisionErr != nil && (!errors.Is(d.moveProvisionErr, errProvisionSent) || d.moveProvisionFailsBeforeF4) {
		// Failed on the hub before the request was sent (the real
		// dispatcher marks only errors at or after the send), or refused
		// by the broker before its absence check: nothing is created.
		return d.moveProvisionErr
	}
	if d.recreateFiles != nil && expect == "fresh-agent-dir" {
		if d.recreateFiles[agent.RuntimeBrokerID] {
			return provisionSent(&brokerStatusError{StatusCode: http.StatusConflict, Body: freshAgentDirConflictBody})
		}
		d.recreateFiles[agent.RuntimeBrokerID] = true
		if d.recreateOwner == nil {
			d.recreateOwner = map[string]string{}
		}
		d.recreateOwner[agent.RuntimeBrokerID] = moveID
	}
	return d.moveProvisionErr
}

// moveMarkerMismatchBody is the broker's fixed, path-free 409 body for a
// forced move's rollback delete of files the move did not create.
const moveMarkerMismatchBody = `{"error":{"code":"move_marker_mismatch","message":"Failed to delete agent: its files on this broker were not created by this move; nothing was removed"}}`

// freshAgentDirConflictBody is the broker's fixed, path-free 409 body for a
// forced move's provision onto a broker that has files for the agent.
const freshAgentDirConflictBody = `{"error":{"code":"move_target_not_fresh","message":"Failed to provision moved agent: the agent already has files on this broker; refusing to recreate over them"}}`

func (d *reincarnateTestDispatcher) DispatchAgentDeleteLocalOnly(_ context.Context, agent *store.Agent) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.localOnlyDeleteCalls = append(d.localOnlyDeleteCalls, agent.RuntimeBrokerID+"|"+agent.RunID)
	return d.localOnlyDeleteErr[agent.RuntimeBrokerID]
}
func (d *reincarnateTestDispatcher) DispatchAgentDeleteRecreated(_ context.Context, agent *store.Agent, moveID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.recreatedDeleteCalls = append(d.recreatedDeleteCalls, agent.RuntimeBrokerID+"|"+agent.ProjectID+"|"+agent.RunID)
	d.recreatedDeleteIDs = append(d.recreatedDeleteIDs, moveID)
	if d.recreatedDeleteErr != nil {
		return d.recreatedDeleteErr
	}
	if d.recreateFiles != nil && d.recreateFiles[agent.RuntimeBrokerID] {
		// The broker removes only files marked by this move.
		if d.recreateOwner[agent.RuntimeBrokerID] != moveID {
			return &brokerStatusError{StatusCode: http.StatusConflict, Body: moveMarkerMismatchBody}
		}
		delete(d.recreateFiles, agent.RuntimeBrokerID)
		delete(d.recreateOwner, agent.RuntimeBrokerID)
	}
	return nil
}
func (d *reincarnateTestDispatcher) recreatedDeletes() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.recreatedDeleteCalls...)
}
func (d *reincarnateTestDispatcher) moveSnapshot() (provisions, localDeletes, startBrokers []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.moveProvisionCalls...), append([]string(nil), d.localOnlyDeleteCalls...), append([]string(nil), d.startBrokers...)
}
func (d *reincarnateTestDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, task string, resume bool) error {
	d.mu.Lock()
	if d.runStore != nil {
		runID := fmt.Sprintf("run-%s-%d", agent.RuntimeBrokerID, len(d.startBrokers)+1)
		if _, err := d.runStore.SetAgentRunID(ctx, agent.ID, runID, nil); err == nil {
			agent.RunID = runID
		}
		if d.startPlacement != "" {
			_ = d.runStore.SetAgentWorkspacePlacement(ctx, agent.ID, d.startPlacement)
		}
	}
	d.startBrokers = append(d.startBrokers, agent.RuntimeBrokerID)
	d.startCalls++
	d.lastStartTask = task
	d.lastStartResume = &resume
	err := d.startErr
	image := d.startImage
	echo := d.startEcho
	d.mu.Unlock()
	if err == nil && image != "" && agent.AppliedConfig != nil {
		agent.AppliedConfig.Image = image
	}
	if err == nil && echo != nil && agent.AppliedConfig != nil {
		echo(agent.AppliedConfig)
	}
	return err
}
func (d *reincarnateTestDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error {
	d.mu.Lock()
	d.stopCalls++
	err := d.stopErr
	hook := d.stopHook
	d.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}
func (d *reincarnateTestDispatcher) DispatchAgentRestart(context.Context, *store.Agent) error {
	return nil
}
func (d *reincarnateTestDispatcher) DispatchAgentResetAuth(context.Context, *store.Agent) error {
	return nil
}
func (d *reincarnateTestDispatcher) DispatchAgentDelete(context.Context, *store.Agent, bool, bool, bool, time.Time) error {
	return nil
}
func (d *reincarnateTestDispatcher) DispatchAgentMessage(context.Context, *store.Agent, string, bool, *messages.StructuredMessage) error {
	return nil
}
func (d *reincarnateTestDispatcher) DispatchAgentLogs(context.Context, *store.Agent, int) (string, error) {
	return "", nil
}
func (d *reincarnateTestDispatcher) DispatchAgentExec(context.Context, *store.Agent, []string, int) (string, int, error) {
	return "", 0, nil
}
func (d *reincarnateTestDispatcher) DispatchCheckAgentPrompt(context.Context, *store.Agent) (bool, error) {
	return false, nil
}
func (d *reincarnateTestDispatcher) DispatchAgentCreateWithGather(context.Context, *store.Agent) (*CreateDispatchResult, error) {
	return nil, nil
}
func (d *reincarnateTestDispatcher) DispatchFinalizeEnv(context.Context, *store.Agent, map[string]string) (*CreateDispatchResult, error) {
	return nil, nil
}

// resetReprovisions makes the next reprovision the "first" one again, so
// reprovisionErr applies to the next reincarnation's own reprovision and
// rerenderErr to its re-render.
func (d *reincarnateTestDispatcher) resetReprovisions(reprovisionErr error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reprovisionCalls = 0
	d.reprovisionConfigs = nil
	d.reprovisionErr = reprovisionErr
}

func (d *blockingStopDispatcher) DispatchAgentStop(ctx context.Context, a *store.Agent) error {
	first := false
	d.once.Do(func() { first = true })
	if first {
		close(d.entered)
		<-d.release
	}
	return d.reincarnateTestDispatcher.DispatchAgentStop(ctx, a)
}

func (d *deleteDispatcher) DispatchAgentDelete(_ context.Context, _ *store.Agent, deleteFiles, removeBranch, _ bool, _ time.Time) error {
	d.deleteCalls++
	d.lastDeleteFiles = deleteFiles
	d.lastRemoveBranch = removeBranch
	return d.deleteErr
}

func (d *createAgentDispatcher) DispatchAgentCreate(_ context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	d.capturedAgent = agent
	if d.createPhase != "" {
		agent.Phase = d.createPhase
	}
	if d.createRuntime != "" {
		agent.Runtime = d.createRuntime
	}
	if d.createStatus != "" {
		agent.ContainerStatus = d.createStatus
	}
	return nil, nil
}
func (d *createAgentDispatcher) DispatchAgentProvision(_ context.Context, agent *store.Agent) error {
	agent.Phase = string(state.PhaseCreated)
	return nil
}

func (d *createAgentDispatcher) DispatchAgentReprovision(_ context.Context, agent *store.Agent) error {
	agent.Phase = string(state.PhaseCreated)
	return nil
}
func (d *createAgentDispatcher) DispatchAgentStart(_ context.Context, _ *store.Agent, _ string, _ bool) error {
	d.startCalled = true
	return nil
}
func (d *createAgentDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *createAgentDispatcher) DispatchAgentRestart(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *createAgentDispatcher) DispatchAgentResetAuth(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *createAgentDispatcher) DispatchAgentDelete(_ context.Context, _ *store.Agent, _, _, _ bool, _ time.Time) error {
	d.deleteCalled = true
	return d.deleteErr
}
func (d *createAgentDispatcher) DispatchAgentMessage(_ context.Context, _ *store.Agent, _ string, _ bool, _ *messages.StructuredMessage) error {
	return nil
}
func (d *createAgentDispatcher) DispatchCheckAgentPrompt(_ context.Context, _ *store.Agent) (bool, error) {
	return false, nil
}
func (d *createAgentDispatcher) DispatchAgentCreateWithGather(_ context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	if _, err := d.DispatchAgentCreate(context.Background(), agent); err != nil {
		return nil, err
	}
	return envReqsResult(d.envReqs), nil
}

func (d *createAgentDispatcher) DispatchAgentLogs(_ context.Context, _ *store.Agent, _ int) (string, error) {
	return "", d.logsErr
}
func (d *createAgentDispatcher) DispatchAgentExec(_ context.Context, _ *store.Agent, _ []string, _ int) (string, int, error) {
	return d.execOutput, d.execExitCode, nil
}
func (d *createAgentDispatcher) DispatchFinalizeEnv(_ context.Context, _ *store.Agent, _ map[string]string) (*CreateDispatchResult, error) {
	return nil, nil
}

func (d *failingCreateDispatcher) DispatchAgentCreateWithGather(_ context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	d.capturedAgent = agent
	return nil, d.createErr
}
func (d *failingCreateDispatcher) DispatchAgentDelete(_ context.Context, _ *store.Agent, deleteFiles, removeBranch, _ bool, _ time.Time) error {
	d.deleteCalled = true
	d.deleteCalledFiles = deleteFiles
	d.deleteBranch = removeBranch
	return nil
}

func (d *skillFailDispatcher) DispatchAgentProvision(ctx context.Context, agent *store.Agent) error {
	d.provisionedAgent = agent
	if d.provisionErr != nil {
		return d.provisionErr
	}
	return d.createAgentDispatcher.DispatchAgentProvision(ctx, agent)
}

func (d *skillFailDispatcher) DispatchAgentCreate(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	if d.createErr != nil {
		return nil, d.createErr
	}
	return d.createAgentDispatcher.DispatchAgentCreate(ctx, agent)
}

func (d *skillFailDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, task string, resume bool) error {
	d.startCalled = true
	return d.startErr
}

// webOAuthLogin runs the web OAuth callback for email against a WebServer
// backed by s.
func webOAuthLogin(t *testing.T, s store.Store, settings *staticAccessSettings, email string, opts ...func(*WebServer)) {
	t.Helper()
	const secret = "test-session-secret-for-login-grant-tests-1234567890"
	ws := newTestWebServer(t, WebServerConfig{
		SessionSecret: secret,
		BaseURL:       "http://localhost:8080",
	})
	ws.oauthService = NewOAuthService(OAuthConfig{
		Web: OAuthClientConfig{
			Google: OAuthProviderConfig{
				ClientID:     "test-client-id",
				ClientSecret: "test-client-secret",
			},
		},
	}, nil)
	ws.oauthService.httpClient = &http.Client{
		Transport: &mockOAuthTransport{
			tokenJSON:    `{"access_token":"mock-token","token_type":"Bearer","expires_in":3600}`,
			userinfoJSON: `{"id":"id-` + email + `","email":"` + email + `","verified_email":true,"name":"OAuth User"}`,
		},
	}
	ws.SetStore(s)
	ws.SetAccessSettingsProvider(settings)
	var safe atomic.Bool
	safe.Store(true)
	ws.SetDemotionSafe(&safe)
	for _, o := range opts {
		o(ws)
	}

	reqSetup := httptest.NewRequest(http.MethodGet, "/auth/login/google", nil)
	recSetup := httptest.NewRecorder()
	sess, err := ws.sessionStore.Get(reqSetup, webSessionName)
	require.NoError(t, err)
	const oauthState = "test-state-login-grants"
	sess.Values[sessKeyOAuthState] = oauthState
	require.NoError(t, sess.Save(reqSetup, recSetup))
	cookies := recSetup.Result().Cookies()
	require.NotEmpty(t, cookies)

	req := httptest.NewRequest(http.MethodGet, "/auth/callback/google?code=test-code&state="+oauthState, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Header().Get("Location"), "error=", "OAuth callback must succeed")
}

// webProxyLogin performs one proxy-auth request for email against a
// WebServer backed by s.
func webProxyLogin(t *testing.T, s store.Store, settings *staticAccessSettings, email string, opts ...func(*WebServer)) {
	t.Helper()
	ws := newTestWebServer(t, WebServerConfig{
		AuthMode: "proxy",
		ProxyAuthenticator: &mockProxyAuthenticator{user: &ProxyUserInfo{
			Subject: "sub-" + email,
			Email:   email,
			Domain:  "example.com",
		}},
	})
	ws.SetAccessSettingsProvider(settings)
	ws.SetStore(s)
	var safe atomic.Bool
	safe.Store(true)
	ws.SetDemotionSafe(&safe)
	for _, o := range opts {
		o(ws)
	}

	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)
	require.NotEqual(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
}

func (f *brokerAssocFixture) providersPath() string {
	return "/api/v1/projects/" + f.project.ID + "/providers"
}

func (f *brokerAssocFixture) link(t *testing.T, broker *store.RuntimeBroker) {
	t.Helper()
	require.NoError(t, f.store.AddProjectProvider(context.Background(), &store.ProjectProvider{
		ProjectID: f.project.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline,
	}))
}

func (f def135Fixture) sendBrokerInbound(t *testing.T, msg *messages.StructuredMessage, surface, externalRef, parentRef string) *httptest.ResponseRecorder {
	t.Helper()
	payload := inboundMessageRequest{
		Topic:       f.topic,
		Message:     msg,
		Surface:     surface,
		ExternalRef: externalRef,
		ParentRef:   parentRef,
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))

	rec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rec, req)
	return rec
}

func (e routedTestEnv) doRoutedRequest(t *testing.T, req routedInboundRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)

	httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound/routed", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq = httpReq.WithContext(contextWithBrokerIdentity(httpReq.Context(), NewBrokerIdentity("test-broker")))

	rec := httptest.NewRecorder()
	e.srv.mux.ServeHTTP(rec, httpReq)
	return rec
}

// thread creates a topic in f.proj. When lastMsg is true it gets a latest
// message; the message ID is returned ("" otherwise). member adds userID as
// an active participant of the topic's conversation.
func (f *unreadFixture) thread(name, userID string, member, lastMsg bool) (topicID, msgID string) {
	f.t.Helper()
	ctx := context.Background()
	topicID = api.NewUUID()
	require.NoError(f.t, f.wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: f.proj.ID, Name: name,
		CreatedBy: "someone", CreatedAt: time.Now().UTC(),
	}))
	convID := topicConversationID(f.t, f.wcs, topicID)
	if member {
		require.NoError(f.t, f.s.EnsureParticipant(ctx, &store.ConversationParticipant{
			ConversationID: convID, PrincipalKind: "user", PrincipalID: userID, Role: "member",
		}))
	}
	if lastMsg {
		msgID = api.NewUUID()
		require.NoError(f.t, f.wcs.TouchTopicActivity(ctx, topicID, msgID))
	}
	return topicID, msgID
}

// dm registers a DM between userID and a fresh user peer with one message
// from the peer, and returns its key and that message's ID.
func (f *unreadFixture) dm(userID string) (key, msgID string) {
	f.t.Helper()
	return f.dmWith(userID, "user", api.NewUUID())
}

// dmWith registers a DM between userID and the peer of peerKind ("user" or
// "agent") and peerID, with one message from the peer, and returns its key
// and that message's ID.
func (f *unreadFixture) dmWith(userID, peerKind, peer string) (key, msgID string) {
	f.t.Helper()
	ctx := context.Background()
	key, err := messages.DMConversationKey(peerKind, peer, "user", userID)
	require.NoError(f.t, err)
	convID := seedConversation(f.t, f.s, "native", key, "direct")
	registerDMParticipants(ctx, f.wcs, key)
	f.seq++
	m := &store.Message{
		ID: api.NewUUID(), ProjectID: f.proj.ID, Sender: peerKind + ":peer", SenderID: peer,
		Recipient: "user:me", RecipientID: userID, Msg: "hello",
		Type: messages.TypeInstruction, Channel: "web", ThreadID: key, ConversationID: convID,
		CreatedAt: time.Now().UTC().Add(time.Duration(f.seq) * time.Second),
	}
	require.NoError(f.t, f.s.CreateMessage(ctx, m))
	return key, m.ID
}

// additiveEnvelope is a minimal struct for decoding the rendered envelope.
type additiveEnvelope struct {
	Type string   `json:"type"`
	To   []string `json:"to,omitempty"`
	From string   `json:"from"`
	Msg  string   `json:"msg"`
}

// sortedTo returns a sorted copy of the "to" array.
func sortedTo(to []string) []string {
	c := make([]string, len(to))
	copy(c, to)
	sort.Strings(c)
	return c
}

func (s *spaceMembersStore) ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	if !s.fault.Active() {
		return s.Store.ListAgents(ctx, filter, opts)
	}
	s.listAgentsCalls++
	if s.failListAgentsOnCall == s.listAgentsCalls {
		return nil, errors.New("injected list agents failure")
	}
	page, err := s.Store.ListAgents(ctx, filter, opts)
	if err == nil && s.onAgentPage != nil {
		s.onAgentPage(s.listAgentsCalls, page.NextCursor == "")
	}
	return page, err
}

func (s *spaceMembersStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	if s.onEffectiveGroups != nil && s.fault.Active() {
		s.onEffectiveGroups()
	}
	return s.Store.GetEffectiveGroups(ctx, userID)
}

func (s *spaceMembersStore) ListProjectMembers(ctx context.Context, projectID string) ([]*store.ProjectMembership, error) {
	if s.failProjectMembers && s.fault.Active() {
		return nil, errors.New("injected list project members failure")
	}
	return s.Store.ListProjectMembers(ctx, projectID)
}

func (p *readStateAtPublishSpy) PublishUserMessage(ctx context.Context, msg *store.Message, _ []AttachmentRef, _ []artifacts.MessageRef) {
	p.called = true
	p.messageID = msg.ID
	p.readStateAtPublish, _ = p.wcs.GetReadState(ctx, msg.SenderID, msg.ThreadID)

	if strings.HasPrefix(msg.ThreadID, "dm:") {
		dms, _ := p.wcs.ListDMs(ctx, msg.SenderID)
		for _, dm := range dms {
			if dm.ConversationKey == msg.ThreadID {
				p.lastMessageIDAtPublish = dm.LastMessageID
				break
			}
		}
	} else if topic, _ := p.wcs.GetTopic(ctx, msg.ThreadID); topic != nil {
		p.lastMessageIDAtPublish = topic.LastMessageID
	}

	lastRead := ""
	if p.readStateAtPublish != nil {
		lastRead = p.readStateAtPublish.LastReadMessageID
	}
	p.hasUnreadAtPublish = p.lastMessageIDAtPublish != "" && p.lastMessageIDAtPublish != lastRead
}

func (t *transientAgentLookupStore) GetAgentBySlug(ctx context.Context, projectID, slug string) (*store.Agent, error) {
	if slug == t.failSlug {
		return nil, t.err
	}
	return t.Store.GetAgentBySlug(ctx, projectID, slug)
}

func (e *errListAgentsStore) ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	return nil, e.err
}

func (r *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deadlines = append(r.deadlines, t)
	return nil
}

func (b *spyBrokerBus) Publish(_ context.Context, topic string, msg *messages.StructuredMessage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Deep copy the message to prevent mutation after capture.
	cp := *msg
	if msg.Metadata != nil {
		cp.Metadata = make(map[string]string, len(msg.Metadata))
		for k, v := range msg.Metadata {
			cp.Metadata[k] = v
		}
	}
	if msg.Attachments != nil {
		cp.Attachments = make([]string, len(msg.Attachments))
		copy(cp.Attachments, msg.Attachments)
	}
	b.events = append(b.events, spyBrokerEvent{topic: topic, msg: &cp})
	return nil
}

func (b *spyBrokerBus) Subscribe(_ string, _ eventbus.EventHandler) (eventbus.Subscription, error) {
	return &spyNoopSub{}, nil
}

func (b *spyBrokerBus) Close() error { return nil }

func (b *spyBrokerBus) getEvents() []spyBrokerEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := make([]spyBrokerEvent, len(b.events))
	copy(result, b.events)
	return result
}

type spyBrokerEvent struct {
	topic string
	msg   *messages.StructuredMessage
}

func (m *mockGCPServiceAccountAdmin) CreateServiceAccount(_ context.Context, projectID, accountID, _, _ string) (string, string, error) {
	if m.createErr != nil {
		return "", "", m.createErr
	}
	email := fmt.Sprintf("%s@%s.iam.gserviceaccount.com", accountID, projectID)
	m.createdSAs = append(m.createdSAs, accountID)
	m.lastEmail = email
	m.lastProject = projectID
	return email, "unique-id-123", nil
}

func (m *mockGCPServiceAccountAdmin) DeleteServiceAccount(_ context.Context, saEmail string) error {
	m.deletedSAs = append(m.deletedSAs, saEmail)
	return m.deleteErr
}

func (m *mockGCPServiceAccountAdmin) SetIAMPolicy(_ context.Context, saEmail, member, role string) error {
	m.iamPolicies = append(m.iamPolicies, mockIAMPolicyCall{
		SAEmail: saEmail,
		Member:  member,
		Role:    role,
	})
	return m.policyErr
}

type mockIAMPolicyCall struct {
	SAEmail string
	Member  string
	Role    string
}

func (m *mockGCPTokenGenerator) GenerateAccessToken(_ context.Context, _ string, _ []string) (*GCPAccessToken, error) {
	return &GCPAccessToken{AccessToken: "test-token", ExpiresIn: 3600, TokenType: "Bearer"}, nil
}

func (m *mockGCPTokenGenerator) GenerateIDToken(_ context.Context, _ string, _ string) (*GCPIDToken, error) {
	return &GCPIDToken{Token: "test-id-token"}, nil
}

func (m *mockGCPTokenGenerator) VerifyImpersonation(_ context.Context, _ string) error {
	return nil
}

func (m *mockGCPTokenGenerator) ServiceAccountEmail() string {
	return m.email
}

func (m *mockGCPTokenGeneratorVerifyFail) GenerateAccessToken(_ context.Context, _ string, _ []string) (*GCPAccessToken, error) {
	return &GCPAccessToken{AccessToken: "test-token", ExpiresIn: 3600, TokenType: "Bearer"}, nil
}

func (m *mockGCPTokenGeneratorVerifyFail) GenerateIDToken(_ context.Context, _ string, _ string) (*GCPIDToken, error) {
	return &GCPIDToken{Token: "test-id-token"}, nil
}

func (m *mockGCPTokenGeneratorVerifyFail) VerifyImpersonation(_ context.Context, _ string) error {
	return m.verifyErr
}

func (m *mockGCPTokenGeneratorVerifyFail) ServiceAccountEmail() string {
	return m.email
}

func (s *failingParticipantStore) AddParticipant(_ context.Context, _ *store.ConversationParticipant) error {
	return errors.New("injected AddParticipant failure")
}

func (s *failingParticipantStore) EnsureParticipant(_ context.Context, _ *store.ConversationParticipant) error {
	return errors.New("injected EnsureParticipant failure")
}

func (pingFailStore) Ping(context.Context) error { return errors.New("database is down") }

func (m *mockIntegrationManager) ListPlugins() []string {
	keys := make([]string, 0, len(m.plugins))
	for name := range m.plugins {
		keys = append(keys, "broker:"+name)
	}
	return keys
}

func (m *mockIntegrationManager) HasPlugin(pluginType, name string) bool {
	if pluginType != "broker" {
		return false
	}
	_, ok := m.plugins[name]
	return ok
}

func (m *mockIntegrationManager) GetPluginConfig(pluginType, name string) map[string]string {
	if pluginType != "broker" {
		return nil
	}
	cfg, ok := m.plugins[name]
	if !ok {
		return nil
	}
	out := make(map[string]string, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	return out
}

func (m *mockIntegrationManager) GetPluginConfigFile(pluginType, name string) string {
	if pluginType != "broker" {
		return ""
	}
	cfg, ok := m.plugins[name]
	if !ok {
		return ""
	}
	return cfg["config_file"]
}

func (m *mockIntegrationManager) IsSelfManaged(pluginType, name string) bool {
	if pluginType != "broker" {
		return false
	}
	return m.selfManaged[name]
}

func (m *mockIntegrationManager) GetDeploymentMode(pluginType, name string) plugin.DeploymentMode {
	if pluginType != "broker" {
		return plugin.DeploymentModePlugin
	}
	if mode, ok := m.deploymentModes[name]; ok {
		return mode
	}
	if m.selfManaged[name] {
		return plugin.DeploymentModeExternal
	}
	return plugin.DeploymentModePlugin
}

func (m *mockIntegrationManager) ConfigureBroker(name string, extra map[string]string) error {
	m.configureCalls = append(m.configureCalls, name)
	return m.configureErr
}

func (m *mockIntegrationManager) ReplaceBrokerConfig(name string, cfg map[string]string) error {
	m.replaceConfigCalls = append(m.replaceConfigCalls, name)
	m.lastReplacedConfig = make(map[string]string, len(cfg))
	for k, v := range cfg {
		m.lastReplacedConfig[k] = v
	}
	return m.replaceConfigErr
}

func (m *mockIntegrationManager) RestartBrokerPlugin(name string, cfg map[string]string) error {
	m.restartCalls = append(m.restartCalls, name)
	return m.replaceConfigErr
}

func (m *mockIntegrationManager) Reconnect(pluginType, name string) error {
	m.reconnectCalls = append(m.reconnectCalls, name)
	return m.reconnectErr
}

func (m *mockIntegrationManager) BrokerHealthCheck(name string) (string, string, map[string]string, error) {
	if m.healthErr != nil {
		return "", "", nil, m.healthErr
	}
	return "healthy", "all good", map[string]string{"connections": "5"}, nil
}

func (m *mockIntegrationManager) BrokerInfo(name string) (string, string, []string, error) {
	if m.infoErr != nil {
		return "", "", nil, m.infoErr
	}
	return "v0.8.2", "telegram", []string{"send", "receive"}, nil
}

func (m *mockIntegrationManager) UpdatePlugin(name string, repoPath string) error {
	m.updateCalls = append(m.updateCalls, name)
	return m.updateErr
}

func (m *mockIntegrationManager) InstallPlugin(name, repoPath, pluginsDir, configFile string) error {
	m.installCalls = append(m.installCalls, name)
	if m.installErr != nil {
		return m.installErr
	}
	m.plugins[name] = map[string]string{}
	if configFile != "" {
		m.plugins[name]["config_file"] = configFile
	}
	return nil
}

func (m *mockIntegrationManager) LoadOne(pluginType, name string, entry plugin.PluginEntry, pluginsDir string) error {
	m.loadOneCalls = append(m.loadOneCalls, name)
	m.loadOneEntries = append(m.loadOneEntries, entry)
	if m.loadOneErr != nil {
		return m.loadOneErr
	}
	if pluginType == "broker" {
		m.plugins[name] = entry.Config
	}
	return nil
}

func (m *mockIntegrationManager) GetBroker(name string) (eventbus.EventBus, error) {
	if m.brokers != nil {
		if b, ok := m.brokers[name]; ok {
			return b, nil
		}
	}
	return nil, fmt.Errorf("mock: GetBroker not wired")
}

func (m *mockIntegrationManager) GetGRPCBrokerAdapter(name string) plugin.GRPCBrokerClient {
	return nil
}

func (f *fakeLogQuerier) Query(_ context.Context, opts LogQueryOptions) (*LogQueryResult, error) {
	f.queryOpts = append(f.queryOpts, opts)
	return &LogQueryResult{}, nil
}

func (f *fakeLogQuerier) Tail(_ context.Context, opts LogQueryOptions) (<-chan CloudLogEntry, func(), error) {
	f.tailOpts = append(f.tailOpts, opts)
	ch := make(chan CloudLogEntry)
	close(ch) // end the stream at once
	return ch, func() {}, nil
}

func (f *fakeLogQuerier) GCPProjectID() string { return "gcp-proj" }

func (f *fakeLogQuerier) Close() error { return nil }

func (d *errorDispatcher) DispatchAgentMessage(_ context.Context, agent *store.Agent, msg string, urgent bool, structuredMsg *messages.StructuredMessage) error {
	n := d.calls.Add(1)
	if d.deferCount > 0 && int32(n) <= atomic.LoadInt32(&d.deferCount) {
		return ErrMessageDeferred
	}
	if d.err != nil {
		return d.err
	}
	return nil
}

// selfToken mints an agent JWT for f.target itself, for exercising the
// "an agent reads its own project-scoped record" self-read path.
func (f *projectAgentAuthzFixture) selfToken(t *testing.T, scopes ...AgentTokenScope) string {
	t.Helper()
	svc := f.srv.GetAgentTokenService()
	require.NotNil(t, svc)
	allScopes := append([]AgentTokenScope{ScopeProjectRead}, scopes...)
	tok, err := svc.GenerateAgentToken(f.target.ID, f.target.ProjectID, allScopes, nil)
	require.NoError(t, err)
	return tok
}

// callerToken mints an agent JWT for f.caller -- a different agent from
// f.target, in the same project.
func (f *projectAgentAuthzFixture) callerToken(t *testing.T, scopes ...AgentTokenScope) string {
	t.Helper()
	svc := f.srv.GetAgentTokenService()
	require.NotNil(t, svc)
	allScopes := append([]AgentTokenScope{ScopeProjectRead}, scopes...)
	tok, err := svc.GenerateAgentToken(f.caller.ID, f.caller.ProjectID, allScopes, nil)
	require.NoError(t, err)
	return tok
}

// strangerToken mints an agent JWT for f.stranger -- an agent in a different
// project from f.target.
func (f *projectAgentAuthzFixture) strangerToken(t *testing.T, scopes ...AgentTokenScope) string {
	t.Helper()
	svc := f.srv.GetAgentTokenService()
	require.NotNil(t, svc)
	allScopes := append([]AgentTokenScope{ScopeProjectRead}, scopes...)
	tok, err := svc.GenerateAgentToken(f.stranger.ID, f.stranger.ProjectID, allScopes, nil)
	require.NoError(t, err)
	return tok
}

func (f *projectAgentAuthzFixture) targetPath() string {
	return "/api/v1/projects/" + f.project.ID + "/agents/" + f.target.ID
}

// providersAuthzFixtureTime is a fixed timestamp for fixture brokers.
var providersAuthzFixtureTime = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

const (
	nonAdminUserID    = "11111111-1111-1111-1111-111111111111"
	nonAdminUserEmail = "scoped-admin@test.local"
)

func (g *getProjectErrStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	if g.fault.Active() && id == g.projectID {
		return nil, g.err
	}
	return g.Store.GetProject(ctx, id)
}

func (s *countingBrokerLoadStore) UpdateRuntimeBroker(ctx context.Context, broker *store.RuntimeBroker) error {
	s.updateRuntimeBrokerCalls++
	return s.Store.UpdateRuntimeBroker(ctx, broker)
}

func (s *countingBrokerLoadStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	s.getRuntimeBrokerCalls++
	if s.getRuntimeBrokerErr != nil {
		return nil, s.getRuntimeBrokerErr
	}
	return s.Store.GetRuntimeBroker(ctx, id)
}

// seedScheduleAuthorAgent stores the agent that authzHelperAgent names, in
// projectID, so the agent can author schedules: a schedule revision records
// the author's effect ceiling, which for an agent is computed from its
// stored row. The agent has no edge, the shape of an agent created before
// the edge backfill (which these tests leave incomplete).
func seedScheduleAuthorAgent(t *testing.T, s store.Store, projectID string) {
	t.Helper()
	require.NoError(t, s.CreateAgent(context.Background(), &store.Agent{
		ID:            authzHelperAgentID,
		Slug:          "schedule-author-agent",
		Name:          "schedule-author-agent",
		ProjectID:     projectID,
		Phase:         "running",
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
	}))
}

func (f *federatedTestIdentity) ID() string          { return f.id }
func (f *federatedTestIdentity) Type() string        { return "federated_user" }
func (f *federatedTestIdentity) Email() string       { return f.email }
func (f *federatedTestIdentity) DisplayName() string { return f.displayName }
func (f *federatedTestIdentity) Role() string        { return f.role }

// authzClassification opts this fake into principalContextForIdentity /
// credentialContextForIdentity classification as a federated user: those
// functions key on concrete type, and this fake is a distinct Go type from
// the production FederatedUserIdentity. It does not implement
// FederatedIdentity (no IssuerURL), so it is not caught by
// AncestryIsHubAttested's federated rejection either way; it is used here
// only to drive a real, allowed federated-user request end to end, not to
// test ancestry denial.
func (f *federatedTestIdentity) authzClassification() (PrincipalKind, CredentialKind) {
	return PrincipalKindFederatedUser, CredentialKindFederation
}

func (kindErrStore) GetUser(context.Context, string) (*store.User, error) {
	return nil, errors.New("store unavailable")
}

// brokerLinkAuthzFixture extends the shared bypassAgents fixture with a
// broker that is not yet a provider of f.proj, and a project member bound
// via project-member (agent.create, but no project.update).
type brokerLinkAuthzFixture struct {
	*bypassAgentsFixture
	// unlinked exists but is not (yet) a provider of f.proj.
	unlinked *store.RuntimeBroker
	// member holds project-member on f.proj: can create agents, cannot
	// update the project.
	member *store.User
}

// quotaReleaseCountingStore counts project quota releases: every
// releaseAgentQuotas call looks up max_agents_per_project once.
type quotaReleaseCountingStore struct {
	store.Store
	mu    sync.Mutex
	count int
}

const testSkillRef = "gh://owner/repo/my-skill@main"

type def135Dispatcher struct {
	mu        sync.Mutex
	calls     []def135DispatchCall
	returnErr error
}

// topicEventSpy embeds noopEventPublisher and records PublishChatTopicEvent
// calls, so tests can assert an SSE "created" event fired without wiring a
// full ChannelEventPublisher and subscribing to it.
type topicEventSpy struct {
	noopEventPublisher
	mu     sync.Mutex
	topics []struct {
		projectID string
		action    string
		topic     WebChatTopic
	}
}

type spyNoopSub struct{}

// postAgentOutboundRequest sends an agent outbound message request as agentID.
func postAgentOutboundRequest(t *testing.T, srv *Server, projectID, agentID string, outbound OutboundMessageRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(outbound)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/"+agentID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
	}}))
	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agentID)
	return rr
}

// waitForSenderMessage waits until a message with text msg from agentID has
// been persisted (the broker path persists asynchronously) and returns it.
func waitForSenderMessage(t *testing.T, s store.Store, agentID, msg string) store.Message {
	t.Helper()
	var found store.Message
	require.Eventually(t, func() bool {
		rows, err := s.ListMessages(context.Background(), store.MessageFilter{SenderID: agentID}, store.ListOptions{Limit: 50})
		if err != nil {
			return false
		}
		for _, m := range rows.Items {
			if m.Msg == msg {
				found = m
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "message %q was never persisted", msg)
	return found
}

// providersAuthzFixture extends the shared bypassAgents fixture with a project
// that has no default broker, a second broker that is not yet linked to it,
// and a hub member who holds no binding on the project.
type providersAuthzFixture struct {
	*bypassAgentsFixture
	// target is owned by owner, has one provider (linked) and no default broker.
	target *store.Project
	// linked is already a provider of target.
	linked *store.RuntimeBroker
	// unlinked exists but is not a provider of target.
	unlinked *store.RuntimeBroker
	// member is a hub member with no binding on target.
	member *store.User
}

// brokerAuthFixture holds the test world for broker auth gate tests.
type brokerAuthFixture struct {
	srv          *Server
	store        store.Store
	broker       *store.RuntimeBroker
	brokerSecret []byte
	deniedUser   *store.User
}

func tiBindSystemRoleToGroup(t *testing.T, s store.Store, groupID, roleName string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
	require.NoError(t, err)
	createdBy := "test"
	if roleName == store.SystemRoleSuperAdmin {
		createdBy = store.SystemReconcileCreatedBy // the store's rule for super-admin bindings
	}
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalGroup,
		PrincipalID: groupID, ScopeType: store.RoleScopeSystem, CreatedBy: createdBy})
	require.NoError(t, err)
}

func tiDecode(t *testing.T, rec *httptest.ResponseRecorder) TestIdentityTokenResponse {
	t.Helper()
	var resp TestIdentityTokenResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	return resp
}

// provisionFixture is a hub with one caller of each authority class.
type provisionFixture struct {
	srv *Server
	s   store.Store
	// superAdmin holds every permission.
	superAdmin *store.User
	// hubAdmin holds user.invite and user.read (detail authority).
	hubAdmin *store.User
	// inviter holds user.invite only, through a custom system role, and is
	// in no group: no detail authority.
	inviter *store.User
	// member is a hub member: user.read through the seeded hub-member
	// grants, no user.invite.
	member *store.User
	// viewer holds the hub-viewer grants: user.read, no user.invite.
	viewer *store.User
}

func newProvisionFixtureOn(t *testing.T, srv *Server, s store.Store) *provisionFixture {
	t.Helper()
	ctx := context.Background()
	f := &provisionFixture{srv: srv, s: s}

	createTestUserWithRole(t, s, tid("prov-super"), "prov-super@example.com", store.UserRoleAdmin, store.SystemRoleSuperAdmin)
	createTestUserWithRole(t, s, tid("prov-hubadmin"), "prov-hubadmin@example.com", store.UserRoleMember, store.SystemRoleHubAdmin)

	inviterID := tid("prov-inviter")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: inviterID, Email: "prov-inviter@example.com", DisplayName: "Inviter", Role: store.UserRoleMember, Status: store.UserStatusActive}))
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "prov-invite-only", ScopeType: store.RoleScopeSystem, Permissions: []string{"user.invite"},
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: inviterID,
		ScopeType: store.RoleScopeSystem, CreatedBy: "test",
	})
	require.NoError(t, err)

	f.member = hubMemberUser(t, s, "prov-member")

	viewerID := tid("prov-viewer")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: viewerID, Email: "prov-viewer@example.com", DisplayName: "Viewer", Role: store.UserRoleViewer, Status: store.UserStatusActive}))
	require.NoError(t, syncHubRoleGrants(ctx, s, viewerID, store.UserRoleViewer, "test"))

	for _, p := range []struct {
		id  string
		dst **store.User
	}{{tid("prov-super"), &f.superAdmin}, {tid("prov-hubadmin"), &f.hubAdmin}, {inviterID, &f.inviter}, {viewerID, &f.viewer}} {
		u, err := s.GetUser(ctx, p.id)
		require.NoError(t, err)
		*p.dst = u
	}
	return f
}

// provisionRaw sends a raw body as user (nil user: no credentials).
func provisionRaw(t *testing.T, srv *Server, user *store.User, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, provisionPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if user != nil {
		token, _, _, err := srv.userTokenService.GenerateTokenPair(user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (f *brokerLinkAuthzFixture) providerIDs(t *testing.T, projectID string) []string {
	t.Helper()
	providers, err := f.store.GetProjectProviders(context.Background(), projectID)
	require.NoError(t, err)
	ids := make([]string, 0, len(providers))
	for _, p := range providers {
		ids = append(ids, p.BrokerID)
	}
	return ids
}

func (f *brokerLinkAuthzFixture) defaultBroker(t *testing.T, projectID string) string {
	t.Helper()
	p, err := f.store.GetProject(context.Background(), projectID)
	require.NoError(t, err)
	return p.DefaultRuntimeBrokerID
}

func (c *quotaReleaseCountingStore) GetLimitDefinitionByName(ctx context.Context, name string) (*store.LimitDefinition, error) {
	if name == "max_agents_per_project" {
		c.mu.Lock()
		c.count++
		c.mu.Unlock()
	}
	return c.Store.GetLimitDefinitionByName(ctx, name)
}

func (d *def135Dispatcher) DispatchAgentMessage(_ context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, def135DispatchCall{
		Agent:             agent,
		Message:           message,
		Interrupt:         interrupt,
		StructuredMessage: structuredMsg,
	})
	return d.returnErr
}

func (d *def135Dispatcher) getCalls() []def135DispatchCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := make([]def135DispatchCall, len(d.calls))
	copy(result, d.calls)
	return result
}

// No-op implementations for the remaining AgentDispatcher methods.
func (d *def135Dispatcher) DispatchAgentCreate(_ context.Context, _ *store.Agent) (*CreateDispatchResult, error) {
	return nil, nil
}
func (d *def135Dispatcher) DispatchAgentProvision(_ context.Context, _ *store.Agent) error {
	return nil
}

func (d *def135Dispatcher) DispatchAgentReprovision(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *def135Dispatcher) DispatchAgentStart(_ context.Context, _ *store.Agent, _ string, _ bool) error {
	return nil
}
func (d *def135Dispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error    { return nil }
func (d *def135Dispatcher) DispatchAgentRestart(_ context.Context, _ *store.Agent) error { return nil }
func (d *def135Dispatcher) DispatchAgentResetAuth(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *def135Dispatcher) DispatchAgentDelete(_ context.Context, _ *store.Agent, _, _, _ bool, _ time.Time) error {
	return nil
}
func (d *def135Dispatcher) DispatchAgentLogs(_ context.Context, _ *store.Agent, _ int) (string, error) {
	return "", nil
}
func (d *def135Dispatcher) DispatchAgentExec(_ context.Context, _ *store.Agent, _ []string, _ int) (string, int, error) {
	return "", 0, nil
}
func (d *def135Dispatcher) DispatchCheckAgentPrompt(_ context.Context, _ *store.Agent) (bool, error) {
	return false, nil
}
func (d *def135Dispatcher) DispatchAgentCreateWithGather(_ context.Context, _ *store.Agent) (*CreateDispatchResult, error) {
	return nil, nil
}
func (d *def135Dispatcher) DispatchFinalizeEnv(_ context.Context, _ *store.Agent, _ map[string]string) (*CreateDispatchResult, error) {
	return nil, nil
}

type def135DispatchCall struct {
	Agent             *store.Agent
	Message           string
	Interrupt         bool
	StructuredMessage *messages.StructuredMessage
}

func (p *topicEventSpy) PublishChatTopicEvent(_ context.Context, projectID string, action string, topic WebChatTopic) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.topics = append(p.topics, struct {
		projectID string
		action    string
		topic     WebChatTopic
	}{projectID, action, topic})
}

func (p *topicEventSpy) events() []struct {
	projectID string
	action    string
	topic     WebChatTopic
} {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]struct {
		projectID string
		action    string
		topic     WebChatTopic
	}, len(p.topics))
	copy(out, p.topics)
	return out
}

func (s *spyNoopSub) Unsubscribe() error { return nil }

func (f *providersAuthzFixture) path() string {
	return "/api/v1/projects/" + f.target.ID + "/providers"
}

// providerIDs returns the broker IDs currently linked to the project.
func (f *providersAuthzFixture) providerIDs(t *testing.T, projectID string) []string {
	t.Helper()
	providers, err := f.store.GetProjectProviders(context.Background(), projectID)
	require.NoError(t, err)
	ids := make([]string, 0, len(providers))
	for _, p := range providers {
		ids = append(ids, p.BrokerID)
	}
	return ids
}

func (f *providersAuthzFixture) defaultBroker(t *testing.T, projectID string) string {
	t.Helper()
	p, err := f.store.GetProject(context.Background(), projectID)
	require.NoError(t, err)
	return p.DefaultRuntimeBrokerID
}

// asBrokerSelf sends an HMAC-signed request as the test broker.
func (f *brokerAuthFixture) asBrokerSelf(t *testing.T, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "broker-auth-nonce-" + uuid.New().String()
	req.Header.Set(HeaderBrokerID, f.broker.ID)
	req.Header.Set(HeaderTimestamp, timestamp)
	req.Header.Set(HeaderNonce, nonce)

	svc := f.srv.brokerAuthService
	require.NotNil(t, svc, "broker auth service must be configured")
	mac := hmac.New(sha256.New, f.brokerSecret)
	mac.Write(svc.buildCanonicalString(req, timestamp, nonce))
	req.Header.Set(HeaderSignature, base64.StdEncoding.EncodeToString(mac.Sum(nil)))

	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// asAgent sends a request carrying an agent JWT (non-user, non-broker identity).
func (f *brokerAuthFixture) asAgent(t *testing.T, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()

	// Create a project and agent so we can mint a valid agent token.
	ctx := context.Background()

	owner := &store.User{
		ID:          tid("broker-auth-agent-owner"),
		Email:       "agent-owner@example.com",
		DisplayName: "Agent Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	// Ignore error if already exists from a previous subtest.
	_ = f.store.CreateUser(ctx, owner)

	proj := &store.Project{
		ID:      tid("broker-auth-agent-proj"),
		Name:    "Agent Project",
		Slug:    "broker-auth-agent-proj",
		OwnerID: owner.ID,
	}
	_ = f.store.CreateProject(ctx, proj)

	agent := &store.Agent{
		ID:        tid("broker-auth-agent"),
		Slug:      "broker-auth-agent",
		Name:      "broker-auth-agent",
		ProjectID: proj.ID,
		Phase:     "running",
		CreatedBy: owner.ID,
		OwnerID:   owner.ID,
	}
	_ = f.store.CreateAgent(ctx, agent)

	// Mint an agent token.
	svc := f.srv.GetAgentTokenService()
	require.NotNil(t, svc)
	tok, err := svc.GenerateAgentToken(agent.ID, agent.ProjectID,
		[]AgentTokenScope{ScopeProjectRead}, nil)
	require.NoError(t, err)

	return doRequestWithAgentToken(t, f.srv, method, path, body, tok)
}

const provisionPath = "/api/v1/users"

// createProjectOwnerRoleBinding creates a project-owner role binding for the
// user on the project, with its audit record, in one transaction. Only
// tests use it; the create, register and clone handlers write the binding
// inside the project create transaction (createProjectWithOwner).
func (s *Server) createProjectOwnerRoleBinding(ctx context.Context, projectID, userID string) error {
	return s.store.WithTx(ctx, func(tx store.Store) error {
		return s.createProjectOwnerRoleBindingTx(ctx, tx, projectID, userID)
	})
}
