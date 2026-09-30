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

// End-to-end routing tests for the api.AgentActionKeys early branches wired
// into handleAgentAction (handlers_agents_core.go) and
// handleProjectAgentAction (handlers_projects_core.go) -- task 2.1's round-1
// review (reviews/2.1-r1.md, findings 2 and 3) required these to be driven
// through the real mux (srv.Handler()), not just the extracted
// authorizeAgentKeys/authorizeAgentKeysCrossProject functions the sibling
// file authorize_agentkeys_matrix_test.go covers. No real keys handler
// exists yet (task 2.2 adds ExecuteAgentKeys): every "allowed" case below
// still ends in the generic `not_found`/"Action" 404 the two switches'
// shared default branch already produces for any action without a
// dispatch case, exactly like hitting any other not-yet-implemented action
// on these routes today. These tests only assert that the AUTHORIZATION
// decision reached before that point is correct and identical in shape
// across both route shapes.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

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

	agentA := &store.Agent{
		ID: tid("agentkeys-route-agent-a"), Slug: "agentkeys-route-agent-a", Name: "Agent A",
		ProjectID: projA.ID, Phase: string(state.PhaseRunning), OwnerID: owner.ID,
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))
	agentB := &store.Agent{
		ID: tid("agentkeys-route-agent-b"), Slug: "agentkeys-route-agent-b", Name: "Agent B",
		ProjectID: projB.ID, Phase: string(state.PhaseRunning), OwnerID: owner.ID,
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	return &agentKeysRouteFixture{
		srv: srv, store: s, projectA: projA, projectB: projB,
		agentInA: agentA, agentInB: agentB, owner: owner, nonOwner: nonOwner,
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

// errorEnvelope decodes a Hub error envelope response body.
func errorEnvelope(t *testing.T, body []byte) (code, message string) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &env), "response body: %s", string(body))
	return env.Error.Code, env.Error.Message
}

// unimplementedActionNotFoundMessage is NotFound(w, "Action")'s exact
// message -- what an *authorized* keys call currently produces (no
// dispatch case exists yet; contract §10's phase-boundary note). It is
// deliberately distinct from a target-resolution miss's message on either
// route shape (writeErrorFromErr's "Resource not found" on T,
// NotFound(w, "Agent")'s "Agent not found" on P), so tests can tell
// "authorized, no handler yet" apart from "target not found" even though
// both currently answer 404 not_found.
const unimplementedActionNotFoundMessage = "Action not found"

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

var errAgentKeysSpyStoreFailure = errors.New("agentKeysLookupSpyStore: simulated store failure")

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

// TestAgentActionKeysRoute_TopLevel_CrossProjectAndMissing pins AK-21 (an
// existing, foreign-project target: 422) and AK-21b (a target ID that does
// not exist in any project: 404) on the top-level route.
func TestAgentActionKeysRoute_TopLevel_CrossProjectAndMissing(t *testing.T) {
	f := newAgentKeysRouteFixture(t)
	token := f.agentToken(t, tid("agentkeys-route-caller-t"), f.projectA.ID, ScopeAgentLifecycle)

	t.Run("AK-21: existing target in a foreign project -> 422", func(t *testing.T) {
		rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInB.ID+"/keys", nil, token)
		assertKeysRouteOutcome(t, "top-level", rec, http.StatusUnprocessableEntity, "cross_project_keys_unsupported")
	})

	t.Run("AK-21b: nonexistent target ID -> 404 not_found", func(t *testing.T) {
		rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+tid("agentkeys-route-nonexistent")+"/keys", nil, token)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
		}
		if code, _ := errorEnvelope(t, rec.Body.Bytes()); code != "not_found" {
			t.Errorf("code = %q, want not_found (not agent_not_found)", code)
		}
		// Round-6 review finding 1: pin the keys-shaped not_found as
		// sanitized too (contract §3 invariant 3's non-operation-ID half
		// names "keys-shaped not_found" alongside keys_denied and
		// cross_project_keys_unsupported), not just "code == not_found and
		// message != the unimplemented-action one". writeErrorFromErr's
		// message for a store.ErrNotFound is the fixed "Resource not
		// found" on this route; assertSanitizedDenialBody's generic checks
		// (no operation_id, no details, no leaked "keys: " reason) apply
		// here exactly as they do to a 403/422 denial, even though this
		// 404 comes from writeErrorFromErr rather than
		// writeAgentKeysAuthzDenial.
		assertSanitizedDenialBody(t, "top-level", rec, "Resource not found")
	})
}

// TestAgentActionKeysRoute_ProjectScoped_CrossProjectNoLookup pins AK-21c:
// on the project-scoped route, an agent-credential cross-project refusal
// must be decided before any agent-target lookup, so an existing and a
// nonexistent slug produce identical 422 responses and neither triggers a
// store lookup.
func TestAgentActionKeysRoute_ProjectScoped_CrossProjectNoLookup(t *testing.T) {
	f := newAgentKeysRouteFixture(t)
	token := f.agentToken(t, tid("agentkeys-route-caller-p1"), f.projectA.ID, ScopeAgentLifecycle)

	spy := &agentKeysLookupSpyStore{Store: f.store}
	f.srv.store = spy

	existing := doRequestWithAgentToken(t, f.srv, http.MethodPost,
		"/api/v1/projects/"+f.projectB.ID+"/agents/"+f.agentInB.Slug+"/keys", nil, token)
	nonexistent := doRequestWithAgentToken(t, f.srv, http.MethodPost,
		"/api/v1/projects/"+f.projectB.ID+"/agents/does-not-exist/keys", nil, token)
	// AK-21c names both {project} forms the route accepts: the canonical
	// UUID (exercised above) and the hosted {uuid}__{slug} form (contract
	// §2.1). resolveProjectID extracts the UUID from either before this
	// branch ever compares project.ID, so both must behave identically
	// (round-4 review finding 3).
	hostedForm := doRequestWithAgentToken(t, f.srv, http.MethodPost,
		"/api/v1/projects/"+f.projectB.ID+"__"+f.projectB.Slug+"/agents/"+f.agentInB.Slug+"/keys", nil, token)

	cases := map[string]*httptest.ResponseRecorder{
		"existing slug": existing, "nonexistent slug": nonexistent, "hosted {uuid}__{slug} project form": hostedForm,
	}
	for name, rec := range cases {
		assertKeysRouteOutcome(t, name, rec, http.StatusUnprocessableEntity, "cross_project_keys_unsupported")
	}

	// Byte-for-byte, not just status/code (round-5 review finding 2): a
	// message or details difference that reveals whether the slug exists
	// would pass a status/code-only comparison.
	if !bytes.Equal(existing.Body.Bytes(), nonexistent.Body.Bytes()) {
		t.Fatalf("existing and nonexistent slug response bodies differ:\n%s\nvs\n%s",
			existing.Body.String(), nonexistent.Body.String())
	}
	if !bytes.Equal(existing.Body.Bytes(), hostedForm.Body.Bytes()) {
		t.Fatalf("canonical-UUID and hosted-form response bodies differ:\n%s\nvs\n%s",
			existing.Body.String(), hostedForm.Body.String())
	}

	if got := spy.lookupCount(); got != 0 {
		t.Fatalf("expected zero agent lookups before the cross-project refusal, got %d (GetAgent=%d GetAgentBySlug=%d)",
			got, atomic.LoadInt32(&spy.getAgentCalls), atomic.LoadInt32(&spy.getAgentBySlugCalls))
	}
}

// TestAgentActionKeysRoute_ProjectScoped_ProjectResolutionGate is round-3
// review finding 1: the contract §3 clarification says 2.1 implements
// invariant 1 (authentication precedes everything; on the project-scoped
// route, so does the shared project-resolution 404) "on both route shapes
// with real route/store-spy tests" -- but nothing previously drove a keys
// request through that gate on P with an unresolvable {project} (AK-21e: a
// nonexistent UUID; AK-21f: a bare slug, which never resolves). This test
// closes that gap for both caller kinds, asserting the shared gate's
// ordinary "Project not found" 404 (identical across callers, no
// operation_id, and decided before any keys-specific code -- including the
// cross-project pre-check -- ever runs, per AK-21e/AK-21f).
func TestAgentActionKeysRoute_ProjectScoped_ProjectResolutionGate(t *testing.T) {
	f := newAgentKeysRouteFixture(t)
	agentCallerToken := f.agentToken(t, tid("agentkeys-route-caller-projgate"), f.projectA.ID, ScopeAgentLifecycle)

	spy := &agentKeysLookupSpyStore{Store: f.store}
	f.srv.store = spy

	unresolvableProjectSegments := map[string]string{
		"AK-21e: nonexistent project UUID": tid("agentkeys-route-nonexistent-project"),
		// AK-21f: a *real* project's bare slug (not a made-up string) still
		// never resolves -- {project} only accepts a canonical UUID or the
		// hosted {uuid}__{slug} form (contract §2.1); this pins that the
		// resolver rejects a bare slug even when it names a project that
		// genuinely exists, not merely an arbitrary unresolvable value
		// (round-4 review finding 1).
		"AK-21f: bare project slug": f.projectA.Slug,
	}

	var bodies []string
	for name, projectSegment := range unresolvableProjectSegments {
		t.Run(name+"/agent caller", func(t *testing.T) {
			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost,
				"/api/v1/projects/"+projectSegment+"/agents/"+f.agentInA.Slug+"/keys", nil, agentCallerToken)
			assertProjectResolutionGate404(t, rec)
			bodies = append(bodies, rec.Body.String())
		})

		t.Run(name+"/user caller", func(t *testing.T) {
			rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost,
				"/api/v1/projects/"+projectSegment+"/agents/"+f.agentInA.Slug+"/keys", nil)
			assertProjectResolutionGate404(t, rec)
			bodies = append(bodies, rec.Body.String())
		})
	}

	// Identical across every combination of unresolvable project segment and
	// caller kind: this 404 comes from the shared, caller-agnostic project
	// resolver (handleProjectAgents), not from any keys-specific code.
	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Fatalf("expected identical bodies across all unresolvable-project cases; body[0]=%s body[%d]=%s", bodies[0], i, bodies[i])
		}
	}

	if got := spy.lookupCount(); got != 0 {
		t.Fatalf("expected zero agent lookups when the project itself never resolves, got %d", got)
	}
}

// assertProjectResolutionGate404 asserts the shared project resolver's
// ordinary 404 (contract AK-21e/AK-21f): code not_found, message "Project
// not found" (NotFound(w, "Project")'s exact text, distinct from the keys
// gate's own "Agent not found"/"Action not found" messages), and no
// operation_id in the body.
func assertProjectResolutionGate404(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	code, message := errorEnvelope(t, rec.Body.Bytes())
	if code != "not_found" {
		t.Errorf("code = %q, want not_found", code)
	}
	if message != "Project not found" {
		t.Errorf("message = %q, want %q", message, "Project not found")
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("operation_id")) {
		t.Errorf("body must not carry an operation_id (decided before any keys-specific code runs): %s", rec.Body.String())
	}
}

// TestAgentActionKeysRoute_ProjectScoped_SameProjectMissingAgent pins that a
// same-project resolution miss reports the keys contract's own "not_found"
// code, not the route's other resolver's "agent_not_found" shape (contract
// §3 invariant 3).
func TestAgentActionKeysRoute_ProjectScoped_SameProjectMissingAgent(t *testing.T) {
	f := newAgentKeysRouteFixture(t)
	token := f.agentToken(t, tid("agentkeys-route-caller-p2"), f.projectA.ID, ScopeAgentLifecycle)

	rec := doRequestWithAgentToken(t, f.srv, http.MethodPost,
		"/api/v1/projects/"+f.projectA.ID+"/agents/does-not-exist/keys", nil, token)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if code, _ := errorEnvelope(t, rec.Body.Bytes()); code != "not_found" {
		t.Errorf("code = %q, want not_found (not agent_not_found)", code)
	}
	// Round-6 review finding 1: pin the keys-shaped not_found as sanitized
	// too, the same way 403/422 denials already are. NotFound(w, "Agent")'s
	// fixed message is "Agent not found"; a regression that kept the
	// not_found code but carried the other resolver's
	// agent_not_found-style details (or an operation_id) would otherwise
	// pass this test.
	assertSanitizedDenialBody(t, "project-scoped", rec, "Agent not found")
}

// TestAgentActionKeysRoute_ProjectScoped_StoreErrorIsNotA404 pins round-2
// review finding 4: a store failure during target resolution must surface
// as a generic 5xx (writeErrorFromErr), never as a 404 that would tell an
// operator "agent does not exist" during a store outage.
func TestAgentActionKeysRoute_ProjectScoped_StoreErrorIsNotA404(t *testing.T) {
	f := newAgentKeysRouteFixture(t)
	token := f.agentToken(t, tid("agentkeys-route-caller-p3"), f.projectA.ID, ScopeAgentLifecycle)

	spy := &agentKeysLookupSpyStore{Store: f.store, failLookups: true}
	f.srv.store = spy

	rec := doRequestWithAgentToken(t, f.srv, http.MethodPost,
		"/api/v1/projects/"+f.projectA.ID+"/agents/"+f.agentInA.Slug+"/keys", nil, token)
	if rec.Code == http.StatusNotFound {
		t.Fatalf("a store failure must not surface as 404: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Code < 500 {
		t.Fatalf("status = %d, want a 5xx for a store failure: %s", rec.Code, rec.Body.String())
	}
	// Round-3 review (finding 2): pin that the 5xx actually comes from the
	// keys branch's own agent-resolution attempt, not from some earlier
	// failure that happens to also route through the spied store (a 500
	// from anywhere else would otherwise satisfy the two checks above too).
	if got := spy.lookupCount(); got == 0 {
		t.Fatalf("expected the resolution attempt to reach the spied store (lookupCount=0)")
	}
}

// keysDenialFixedMessage is writeAgentKeysAuthzDenial's fixed, sanitized
// message for each denial outcome it produces (authorize_agentkeys.go).
var keysDenialFixedMessage = map[string]string{
	"keys_denied":                    "Insufficient permissions",
	"cross_project_keys_unsupported": "Cross-project keys access is not supported for agent callers",
}

// assertKeysRouteOutcome asserts rec matches (wantStatus, wantCode) and,
// depending on the outcome, the exact body shape the seam promises:
//
//   - 404 not_found (authorized, unimplemented): the message is exactly the
//     unimplemented-action one -- distinguishing "authorized, no handler
//     yet" from a target-resolution miss, which answers the identical
//     status/code with a different message (round-2 review finding 3).
//   - 403 keys_denied / 422 cross_project_keys_unsupported (a denial from
//     writeAgentKeysAuthzDenial): the body is sanitized -- see
//     assertSanitizedDenialBody. This is the test the KeysAuthzDecision doc
//     comment asks for and the design ruling's omit-operation_id condition
//     depends on (round-5 review finding 1; ruling on ptone/scion#2195).
func assertKeysRouteOutcome(t *testing.T, label string, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("%s: status = %d, want %d: %s", label, rec.Code, wantStatus, rec.Body.String())
	}
	code, message := errorEnvelope(t, rec.Body.Bytes())
	if code != wantCode {
		t.Errorf("%s: code = %q, want %q", label, code, wantCode)
	}
	if wantStatus == http.StatusNotFound && wantCode == "not_found" {
		if message != unimplementedActionNotFoundMessage {
			t.Errorf("%s: message = %q, want %q (an authorized-but-unimplemented call, not a resolution miss)",
				label, message, unimplementedActionNotFoundMessage)
		}
		return
	}
	if fixedMessage, ok := keysDenialFixedMessage[wantCode]; ok {
		assertSanitizedDenialBody(t, label, rec, fixedMessage)
	}
}

// assertSanitizedDenialBody pins the design ruling's condition for a seam
// denial omitting operation_id (contract §3's phase-boundary clarification,
// ptone/scion#2195): the response must carry the exact, fixed message for
// its outcome, no operation_id, no details, and none of
// KeysAuthzDecision.Reason (tagged "keys: " for audit logs -- see
// authorize_agentkeys.go's denyAgentKeys/denyAgentKeysCrossProject -- but
// never meant to reach an HTTP body).
func assertSanitizedDenialBody(t *testing.T, label string, rec *httptest.ResponseRecorder, wantMessage string) {
	t.Helper()
	body := rec.Body.Bytes()
	_, message := errorEnvelope(t, body)
	if message != wantMessage {
		t.Errorf("%s: message = %q, want exactly %q", label, message, wantMessage)
	}
	if bytes.Contains(body, []byte("operation_id")) {
		t.Errorf("%s: body must not carry an operation_id: %s", label, string(body))
	}
	if bytes.Contains(body, []byte(`"details"`)) {
		t.Errorf("%s: body must not carry a details field: %s", label, string(body))
	}
	if bytes.Contains(body, []byte("keys: ")) {
		t.Errorf("%s: body must not leak the internal audit reason prefix: %s", label, string(body))
	}
}

// TestAgentActionKeysRoute_BothShapesAgree replaces the round-1 AC4 tests
// that passed even with this PR's production code deleted
// (TestAgentActionKeys_RouteMetadataCoversBothRouteShapes /
// TestAgentActionKeys_CapabilityProjectionConsistentAcrossRouteShapes,
// finding 3): it drives both route shapes through the real mux for the same
// identity/target pair and asserts they reach the same outcome, and that
// the attach capability ComputeCapabilities projects for the caller agrees
// with the keys route's own allow/deny decision. Round-2 review finding 8
// added the user-session cases: the P route's cross-project pre-check
// returning nil for a human caller (no blanket ban) was previously covered
// only at the function level (TestAuthorizeAgentKeysCrossProject), not
// end-to-end through the real mux.
func TestAgentActionKeysRoute_BothShapesAgree(t *testing.T) {
	f := newAgentKeysRouteFixture(t)

	t.Run("agent credential", func(t *testing.T) {
		allowedCallerID := tid("agentkeys-route-agree-allowed")
		deniedCallerID := tid("agentkeys-route-agree-denied")
		allowedToken := f.agentToken(t, allowedCallerID, f.projectA.ID, ScopeAgentLifecycle)
		deniedToken := f.agentToken(t, deniedCallerID, f.projectA.ID, ScopeAgentCreate) // no lifecycle scope

		cases := []struct {
			name           string
			token          string
			callerIdentity Identity
			wantStatus     int
			wantCode       string
			wantAttach     bool
		}{
			{
				name:  "authorized agent, same project (unimplemented handler, not a denial)",
				token: allowedToken, callerIdentity: authzHelperAgent(f.projectA.ID, ScopeAgentLifecycle),
				wantStatus: http.StatusNotFound, wantCode: "not_found", wantAttach: true,
			},
			{
				name:  "agent missing lifecycle scope",
				token: deniedToken, callerIdentity: authzHelperAgent(f.projectA.ID, ScopeAgentCreate),
				wantStatus: http.StatusForbidden, wantCode: "keys_denied", wantAttach: false,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				topLevel := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", nil, tc.token)
				projectScoped := doRequestWithAgentToken(t, f.srv, http.MethodPost,
					"/api/v1/projects/"+f.projectA.ID+"/agents/"+f.agentInA.Slug+"/keys", nil, tc.token)

				assertKeysRouteOutcome(t, "top-level", topLevel, tc.wantStatus, tc.wantCode)
				assertKeysRouteOutcome(t, "project-scoped", projectScoped, tc.wantStatus, tc.wantCode)

				// Capability projection must agree with the route decision:
				// the same identity/resource pair, evaluated independently
				// through ComputeCapabilities, must include ActionAttach
				// exactly when the keys route allowed the call.
				caps := f.srv.GetAuthzService().ComputeCapabilities(context.Background(), tc.callerIdentity, agentResource(f.agentInA))
				hasAttach := capabilityAllows(caps, ActionAttach)
				if hasAttach != tc.wantAttach {
					t.Errorf("capability projection attach=%v, want %v (route decision disagrees with capability projection)", hasAttach, tc.wantAttach)
				}
			})
		}
	})

	t.Run("user session", func(t *testing.T) {
		cases := []struct {
			name       string
			user       *store.User
			wantStatus int
			wantCode   string
			wantAttach bool
		}{
			{
				name: "owner attaches to own agent (unimplemented handler, not a denial)",
				user: f.owner, wantStatus: http.StatusNotFound, wantCode: "not_found", wantAttach: true,
			},
			{
				name: "non-owner denied",
				user: f.nonOwner, wantStatus: http.StatusForbidden, wantCode: "keys_denied", wantAttach: false,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				topLevel := doRequestAsUser(t, f.srv, tc.user, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", nil)
				projectScoped := doRequestAsUser(t, f.srv, tc.user, http.MethodPost,
					"/api/v1/projects/"+f.projectA.ID+"/agents/"+f.agentInA.Slug+"/keys", nil)

				assertKeysRouteOutcome(t, "top-level", topLevel, tc.wantStatus, tc.wantCode)
				assertKeysRouteOutcome(t, "project-scoped", projectScoped, tc.wantStatus, tc.wantCode)

				identity := NewAuthenticatedUser(tc.user.ID, tc.user.Email, tc.user.DisplayName, tc.user.Role, "api")
				caps := f.srv.GetAuthzService().ComputeCapabilities(context.Background(), identity, agentResource(f.agentInA))
				hasAttach := capabilityAllows(caps, ActionAttach)
				if hasAttach != tc.wantAttach {
					t.Errorf("capability projection attach=%v, want %v (route decision disagrees with capability projection)", hasAttach, tc.wantAttach)
				}
			})
		}
	})
}

// TestAgentActionKeysRoute_ExpectedDivergence_AgentMissingScopeCrossProject
// is round-2 review finding 8: for an agent credential that lacks
// ScopeAgentLifecycle AND targets a foreign-project agent, the two route
// shapes legitimately disagree, by contract construction (§3 invariants
// 4-5): P decides the project-boundary refusal before ever checking scope
// (422 cross_project_keys_unsupported), while T's single authorizeAgentKeys
// call checks scope first (403 keys_denied) since the top-level route
// resolves its target -- and therefore the caller's cross-project status --
// before authorizeAgentKeys runs at all, and that function checks scope
// ahead of the project comparison internally. This is not a bug: it is
// pinned here, explicitly, so a future change that reorders either path
// gets a failing test instead of silent disagreement, and so
// TestAgentActionKeysRoute_BothShapesAgree's name is not misread as "always
// identical".
func TestAgentActionKeysRoute_ExpectedDivergence_AgentMissingScopeCrossProject(t *testing.T) {
	f := newAgentKeysRouteFixture(t)
	token := f.agentToken(t, tid("agentkeys-route-divergence-caller"), f.projectA.ID, ScopeAgentCreate) // no lifecycle scope

	topLevel := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInB.ID+"/keys", nil, token)
	assertKeysRouteOutcome(t, "top-level", topLevel, http.StatusForbidden, "keys_denied")

	projectScoped := doRequestWithAgentToken(t, f.srv, http.MethodPost,
		"/api/v1/projects/"+f.projectB.ID+"/agents/"+f.agentInB.Slug+"/keys", nil, token)
	assertKeysRouteOutcome(t, "project-scoped", projectScoped, http.StatusUnprocessableEntity, "cross_project_keys_unsupported")
}

// TestAgentActionKeysRoute_RevokedAgentCredential_NeverReachesTheGate
// reuses the established agent credential-revocation path (finding 6:
// credential_revocation_test.go's TestRevokedTokenDeniedBeforeExpiry
// pattern, setupCredentialTestServer/createCredTestAgent/
// srv.GenerateAgentToken/RevokeAgentCredential) against a request shaped
// like the keys route, now that the routing from finding 2 exists. A
// revoked agent credential is rejected by the shared auth middleware
// (evaluateAgentCredentialStatus) before any handler runs, so it never
// reaches authorizeAgentKeys — the same "missing credential" state
// TestAuthorizeAgentKeys_MissingAndInvalidCredentials covers directly at
// the function level. An expired JWT (as opposed to a revoked credential
// record) fails signature/claims validation in the same shared middleware,
// even earlier, for the same reason; see
// TestAgentActionKeysRoute_ExpiredAgentToken_NeverReachesTheGate below for
// that case against a real keys request.
func TestAgentActionKeysRoute_RevokedAgentCredential_NeverReachesTheGate(t *testing.T) {
	srv, s, user, project := setupCredentialTestServer(t)
	ctx := context.Background()

	targetID := tid("agentkeys-route-revoke-target")
	createCredTestAgent(t, s, targetID, project.ID, user.ID)

	callerID := tid("agentkeys-route-revoke-caller")
	createCredTestAgent(t, s, callerID, project.ID, user.ID)

	token, err := srv.GenerateAgentToken(callerID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)

	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)
	cred, err := s.GetAgentCredentialByJTIHash(ctx, hashJTI(claims.ID))
	require.NoError(t, err)

	// Before revocation: the token authenticates and reaches the keys gate
	// (proves the setup is valid, so the post-revocation 401 below is
	// meaningful). The caller has full-role lifecycle scope on its own
	// project's agent, so this must be an authorization "allowed" (404
	// "Action", unimplemented handler) -- asserted by exact message
	// (round-2 review finding 3), not merely "not 401", so this sanity
	// check cannot pass on an unrelated failure that also happens to avoid
	// 401.
	before := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agents/"+targetID+"/keys", nil, token)
	if before.Code != http.StatusNotFound {
		t.Fatalf("token should authenticate and be allowed before revocation, got %d: %s", before.Code, before.Body.String())
	}
	if _, message := errorEnvelope(t, before.Body.Bytes()); message != unimplementedActionNotFoundMessage {
		t.Fatalf("expected the unimplemented-action message before revocation, got %q: %s", message, before.Body.String())
	}

	require.NoError(t, s.RevokeAgentCredential(ctx, cred.ID, "test", "explicit"))

	after := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agents/"+targetID+"/keys", nil, token)
	if after.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after revocation (never reaching authorizeAgentKeys), got %d: %s",
			after.Code, after.Body.String())
	}
}

// TestAgentActionKeysRoute_ExpiredAgentToken_NeverReachesTheGate is round-2
// review finding 6: an expired JWT (as opposed to a revoked credential
// record) must also be rejected by the shared auth middleware before any
// handler runs, on a request shaped like the keys route -- not merely
// proven in the abstract by agenttoken_test.go's token-service-level
// coverage.
func TestAgentActionKeysRoute_ExpiredAgentToken_NeverReachesTheGate(t *testing.T) {
	srv, s, user, project := setupCredentialTestServer(t)

	targetID := tid("agentkeys-route-expired-target")
	createCredTestAgent(t, s, targetID, project.ID, user.ID)
	callerID := tid("agentkeys-route-expired-caller")
	createCredTestAgent(t, s, callerID, project.ID, user.ID)

	// Positive control (round-3 review finding 2): the same setup, minted
	// with the normal (non-expired) duration, must reach the gate and be
	// allowed -- proving the 401 asserted below comes from expiry
	// specifically, not from some unrelated misconfiguration in this
	// fixture that would 401 regardless of TokenDuration.
	validToken, err := srv.GenerateAgentToken(callerID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	before := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agents/"+targetID+"/keys", nil, validToken)
	if before.Code != http.StatusNotFound {
		t.Fatalf("control: a non-expired token should authenticate and be allowed, got %d: %s", before.Code, before.Body.String())
	}
	if _, message := errorEnvelope(t, before.Body.Bytes()); message != unimplementedActionNotFoundMessage {
		t.Fatalf("control: expected the unimplemented-action message, got %q: %s", message, before.Body.String())
	}

	origDuration := srv.agentTokenService.config.TokenDuration
	srv.agentTokenService.config.TokenDuration = -1 * time.Hour // already expired at mint time
	expiredToken, err := srv.GenerateAgentToken(callerID, project.ID, nil, AgentRoleFull, nil)
	srv.agentTokenService.config.TokenDuration = origDuration
	require.NoError(t, err)

	rec := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agents/"+targetID+"/keys", nil, expiredToken)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an expired token (never reaching authorizeAgentKeys), got %d: %s",
			rec.Code, rec.Body.String())
	}

	// Round-3 review (finding 1): the same expired token on the
	// project-scoped route shape must also 401 before reaching either the
	// cross-project pre-check or authorizeAgentKeys.
	recP := doRequestWithAgentToken(t, srv, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/"+targetID+"/keys", nil, expiredToken)
	if recP.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an expired token on the project-scoped route, got %d: %s",
			recP.Code, recP.Body.String())
	}
}
