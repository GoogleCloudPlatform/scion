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

// End-to-end routing tests for the api.AgentActionKeys branches wired into
// handleAgentAction (handlers_agents_core.go) and handleProjectAgentAction
// (handlers_projects_core.go). Originally written against task 2.1's
// temporary routing seam (ptone/scion#2195), driven through the real mux
// (srv.Handler()) rather than just the extracted
// authorizeAgentKeys/authorizeAgentKeysCrossProject functions the sibling
// file authorize_agentkeys_matrix_test.go covers.
//
// Task 2.2 (ptone/scion#2196) replaced that seam wholesale with the real
// ExecuteAgentKeys operation (execute_agent_keys.go), per the binding
// integration obligation recorded on ptone/scion#2196 and the design-owner
// ruling on ptone/scion#2195 (contract §3's phase-boundary clarification).
// Two consequences updated throughout this file:
//
//   - ValidateBody now runs before target resolution and authorization, so
//     every request below must carry a valid {"keys":...} body
//     (validKeysBody) -- an empty/nil body previously reached the
//     authorization decision under 2.1's seam because no validation existed
//     yet; under 2.2 it would 400 invalid_request before ever reaching the
//     authorization code these tests exist to pin.
//   - An operation ID is minted as soon as validation succeeds and appears
//     on every outcome from that point on, including denials and
//     resolution misses that previously had none under 2.1's temporary
//     seam (contract §3 invariant 3). None of these route tests installs a
//     dispatcher (via SetDispatcher) unless it says so explicitly, so an
//     *authorized* call now reaches real admission and ends in 503
//     keys_unavailable (no dispatcher configured, hence no immediate route)
//     -- replacing 2.1's "falls through to the generic unimplemented-action
//     404" placeholder outcome. The fixture agents do carry a real
//     RuntimeBrokerID (see newAgentKeysRouteFixture) for the tests in
//     execute_agent_keys_test.go that install the real HTTPAgentDispatcher
//     and need it to resolve.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
// not exist in any project: 404) on the top-level route. Task 2.2: both now
// carry the real operation ID (contract §3 invariant 3).
func TestAgentActionKeysRoute_TopLevel_CrossProjectAndMissing(t *testing.T) {
	f := newAgentKeysRouteFixture(t)
	token := f.agentToken(t, tid("agentkeys-route-caller-t"), f.projectA.ID, ScopeAgentLifecycle)

	t.Run("AK-21: existing target in a foreign project -> 422", func(t *testing.T) {
		rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInB.ID+"/keys", validKeysBody, token)
		assertKeysDenialOutcome(t, "top-level", rec, http.StatusUnprocessableEntity, "cross_project_keys_unsupported")
	})

	t.Run("AK-21b: nonexistent target ID -> 404 not_found", func(t *testing.T) {
		rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+tid("agentkeys-route-nonexistent")+"/keys", validKeysBody, token)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
		}
		env := decodeKeysError(t, rec.Body.Bytes())
		if env.Code != "not_found" {
			t.Errorf("code = %q, want not_found (not agent_not_found)", env.Code)
		}
		// Keys' own not_found shape (contract §3 invariant 3): a fixed,
		// sanitized message and a real operation ID, never the other
		// resolver's agent_not_found/{agent_slug,project_id} shape.
		assertKeysDenialOutcome(t, "top-level", rec, http.StatusNotFound, "not_found")
	})
}

// TestAgentActionKeysRoute_ProjectScoped_CrossProjectNoLookup pins AK-21c:
// on the project-scoped route, an agent-credential cross-project refusal
// must be decided before any agent-target lookup, so an existing and a
// nonexistent slug produce identical (modulo each response's own,
// necessarily distinct, operation ID) 422 responses and neither triggers a
// store lookup.
func TestAgentActionKeysRoute_ProjectScoped_CrossProjectNoLookup(t *testing.T) {
	f := newAgentKeysRouteFixture(t)
	token := f.agentToken(t, tid("agentkeys-route-caller-p1"), f.projectA.ID, ScopeAgentLifecycle)

	spy := &agentKeysLookupSpyStore{Store: f.store}
	f.srv.store = spy

	existing := doRequestWithAgentToken(t, f.srv, http.MethodPost,
		"/api/v1/projects/"+f.projectB.ID+"/agents/"+f.agentInB.Slug+"/keys", validKeysBody, token)
	nonexistent := doRequestWithAgentToken(t, f.srv, http.MethodPost,
		"/api/v1/projects/"+f.projectB.ID+"/agents/does-not-exist/keys", validKeysBody, token)
	// AK-21c names both {project} forms the route accepts: the canonical
	// UUID (exercised above) and the hosted {uuid}__{slug} form (contract
	// §2.1). resolveProjectID extracts the UUID from either before this
	// branch ever compares project.ID, so both must behave identically
	// (round-4 review finding 3).
	hostedForm := doRequestWithAgentToken(t, f.srv, http.MethodPost,
		"/api/v1/projects/"+f.projectB.ID+"__"+f.projectB.Slug+"/agents/"+f.agentInB.Slug+"/keys", validKeysBody, token)

	cases := map[string]*httptest.ResponseRecorder{
		"existing slug": existing, "nonexistent slug": nonexistent, "hosted {uuid}__{slug} project form": hostedForm,
	}
	for name, rec := range cases {
		assertKeysDenialOutcome(t, name, rec, http.StatusUnprocessableEntity, "cross_project_keys_unsupported")
	}

	// Identical except each response's own operation ID: a message or
	// details difference beyond that would reveal whether the slug exists.
	assertSameOutcomeModuloOperationID(t, "existing vs nonexistent slug", existing, nonexistent)
	assertSameOutcomeModuloOperationID(t, "canonical-UUID vs hosted-form", existing, hostedForm)

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
// operation_id, and decided before any keys-specific code -- including
// ValidateBody and the cross-project pre-check -- ever runs, per
// AK-21e/AK-21f). Unaffected by task 2.2: this gate runs in
// handleProjectAgents, entirely before handleProjectAgentAction (and so
// before ExecuteAgentKeys) is ever called, so the request body's content
// does not matter here -- nil is kept deliberately to underline that.
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
// gate's own "Agent not found" message), and no operation_id in the body.
func assertProjectResolutionGate404(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	env := decodeKeysError(t, rec.Body.Bytes())
	if env.Code != "not_found" {
		t.Errorf("code = %q, want not_found", env.Code)
	}
	if env.Message != "Project not found" {
		t.Errorf("message = %q, want %q", env.Message, "Project not found")
	}
	if _, present := env.Details["operation_id"]; present {
		t.Errorf("body must not carry an operation_id (decided before any keys-specific code runs): %s", rec.Body.String())
	}
}

// TestAgentActionKeysRoute_ProjectScoped_SameProjectMissingAgent pins that a
// same-project resolution miss reports the keys contract's own "not_found"
// code, not the route's other resolver's "agent_not_found" shape (contract
// §3 invariant 3), and -- task 2.2 -- now carries a real operation ID.
func TestAgentActionKeysRoute_ProjectScoped_SameProjectMissingAgent(t *testing.T) {
	f := newAgentKeysRouteFixture(t)
	token := f.agentToken(t, tid("agentkeys-route-caller-p2"), f.projectA.ID, ScopeAgentLifecycle)

	rec := doRequestWithAgentToken(t, f.srv, http.MethodPost,
		"/api/v1/projects/"+f.projectA.ID+"/agents/does-not-exist/keys", validKeysBody, token)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	env := decodeKeysError(t, rec.Body.Bytes())
	if env.Code != "not_found" {
		t.Errorf("code = %q, want not_found (not agent_not_found)", env.Code)
	}
	assertKeysDenialOutcome(t, "project-scoped", rec, http.StatusNotFound, "not_found")
}

// TestAgentActionKeysRoute_ProjectScoped_StoreErrorIsNotA404 pins round-2
// review finding 4: a store failure during target resolution must surface
// as a generic 5xx (writeErrorFromErr's mapping), never as a 404 that would
// tell an operator "agent does not exist" during a store outage. Task 2.2
// extends this: since beginAgentKeysRequest has already minted an operation
// ID by this point (contract §3 invariant 3), the 5xx must carry it too,
// via finishAgentKeysInternalError.
func TestAgentActionKeysRoute_ProjectScoped_StoreErrorIsNotA404(t *testing.T) {
	f := newAgentKeysRouteFixture(t)
	token := f.agentToken(t, tid("agentkeys-route-caller-p3"), f.projectA.ID, ScopeAgentLifecycle)

	spy := &agentKeysLookupSpyStore{Store: f.store, failLookups: true}
	f.srv.store = spy

	rec := doRequestWithAgentToken(t, f.srv, http.MethodPost,
		"/api/v1/projects/"+f.projectA.ID+"/agents/"+f.agentInA.Slug+"/keys", validKeysBody, token)
	assertKeysInternalErrorOutcome(t, "project-scoped", rec, spy)
}

// TestAgentActionKeysRoute_TopLevel_StoreErrorIsNotA404 is the top-level
// route's counterpart to the project-scoped test above:
// agentKeysLookupSpyStore.GetAgent already supports failLookups, so the same
// store-outage scenario applies here too.
func TestAgentActionKeysRoute_TopLevel_StoreErrorIsNotA404(t *testing.T) {
	f := newAgentKeysRouteFixture(t)
	token := f.agentToken(t, tid("agentkeys-route-caller-t3"), f.projectA.ID, ScopeAgentLifecycle)

	spy := &agentKeysLookupSpyStore{Store: f.store, failLookups: true}
	f.srv.store = spy

	rec := doRequestWithAgentToken(t, f.srv, http.MethodPost,
		"/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody, token)
	assertKeysInternalErrorOutcome(t, "top-level", rec, spy)
}

// assertKeysInternalErrorOutcome asserts rec is a generic 5xx (never a
// 404), carries a non-empty details.operation_id, and that the 5xx actually
// came from the keys branch's own agent-resolution attempt: a 500 from
// anywhere else would otherwise satisfy a status-only check too.
func assertKeysInternalErrorOutcome(t *testing.T, label string, rec *httptest.ResponseRecorder, spy *agentKeysLookupSpyStore) {
	t.Helper()
	if rec.Code == http.StatusNotFound {
		t.Fatalf("%s: a store failure must not surface as 404: %d %s", label, rec.Code, rec.Body.String())
	}
	if rec.Code < 500 {
		t.Fatalf("%s: status = %d, want a 5xx for a store failure: %s", label, rec.Code, rec.Body.String())
	}
	env := decodeKeysError(t, rec.Body.Bytes())
	opID, _ := env.Details["operation_id"].(string)
	if opID == "" {
		t.Errorf("%s: expected a non-empty operation_id on a post-validation internal error, got details=%v", label, env.Details)
	}
	if got := spy.lookupCount(); got == 0 {
		t.Fatalf("%s: expected the resolution attempt to reach the spied store (lookupCount=0)", label)
	}
}

// keysDenialFixedMessage is agentKeysOutcomeMessage's fixed, sanitized
// message for each outcome this file exercises (execute_agent_keys.go).
var keysDenialFixedMessage = map[string]string{
	"keys_denied":                    "Insufficient permissions",
	"cross_project_keys_unsupported": "Cross-project keys access is not supported for agent callers",
	"not_found":                      "Agent not found",
	"keys_unavailable":               "Keys dispatch is currently unavailable",
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

// assertSameOutcomeModuloOperationID asserts two recorded responses carry
// the same status, code, message and details, except that their
// operation_id values are each non-empty and (as freshly minted UUIDs per
// request) not required to match each other.
func assertSameOutcomeModuloOperationID(t *testing.T, label string, a, b *httptest.ResponseRecorder) {
	t.Helper()
	if a.Code != b.Code {
		t.Fatalf("%s: status differs: %d vs %d", label, a.Code, b.Code)
	}
	envA := decodeKeysError(t, a.Body.Bytes())
	envB := decodeKeysError(t, b.Body.Bytes())
	if envA.Code != envB.Code {
		t.Fatalf("%s: code differs: %q vs %q", label, envA.Code, envB.Code)
	}
	if envA.Message != envB.Message {
		t.Fatalf("%s: message differs: %q vs %q", label, envA.Message, envB.Message)
	}
	opA, _ := envA.Details["operation_id"].(string)
	opB, _ := envB.Details["operation_id"].(string)
	if opA == "" || opB == "" {
		t.Fatalf("%s: expected both responses to carry a non-empty operation_id: %q vs %q", label, opA, opB)
	}
	delete(envA.Details, "operation_id")
	delete(envB.Details, "operation_id")
	if len(envA.Details) != 0 || len(envB.Details) != 0 {
		t.Fatalf("%s: details differ beyond operation_id: %v vs %v", label, envA.Details, envB.Details)
	}
}

// TestAgentActionKeysRoute_BothShapesAgree drives both route shapes through
// the real mux for the same identity/target pair and asserts they reach the
// same outcome, and that the attach capability ComputeCapabilities projects
// for the caller agrees with the keys route's own allow/deny decision.
// Task 2.2: the "allowed" case now reaches real admission and ends in 503
// keys_unavailable (no dispatcher configured on the fixture server),
// replacing 2.1's "falls through to the generic unimplemented-action 404"
// placeholder.
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
				name:  "authorized agent, same project (admitted, no broker configured)",
				token: allowedToken, callerIdentity: authzHelperAgent(f.projectA.ID, ScopeAgentLifecycle),
				wantStatus: http.StatusServiceUnavailable, wantCode: "keys_unavailable", wantAttach: true,
			},
			{
				name:  "agent missing lifecycle scope",
				token: deniedToken, callerIdentity: authzHelperAgent(f.projectA.ID, ScopeAgentCreate),
				wantStatus: http.StatusForbidden, wantCode: "keys_denied", wantAttach: false,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				topLevel := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody, tc.token)
				projectScoped := doRequestWithAgentToken(t, f.srv, http.MethodPost,
					"/api/v1/projects/"+f.projectA.ID+"/agents/"+f.agentInA.Slug+"/keys", validKeysBody, tc.token)

				assertKeysDenialOutcome(t, "top-level", topLevel, tc.wantStatus, tc.wantCode)
				assertKeysDenialOutcome(t, "project-scoped", projectScoped, tc.wantStatus, tc.wantCode)

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
				name: "owner attaches to own agent (admitted, no broker configured)",
				user: f.owner, wantStatus: http.StatusServiceUnavailable, wantCode: "keys_unavailable", wantAttach: true,
			},
			{
				name: "non-owner denied",
				user: f.nonOwner, wantStatus: http.StatusForbidden, wantCode: "keys_denied", wantAttach: false,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				topLevel := doRequestAsUser(t, f.srv, tc.user, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody)
				projectScoped := doRequestAsUser(t, f.srv, tc.user, http.MethodPost,
					"/api/v1/projects/"+f.projectA.ID+"/agents/"+f.agentInA.Slug+"/keys", validKeysBody)

				assertKeysDenialOutcome(t, "top-level", topLevel, tc.wantStatus, tc.wantCode)
				assertKeysDenialOutcome(t, "project-scoped", projectScoped, tc.wantStatus, tc.wantCode)

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

	topLevel := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInB.ID+"/keys", validKeysBody, token)
	assertKeysDenialOutcome(t, "top-level", topLevel, http.StatusForbidden, "keys_denied")

	projectScoped := doRequestWithAgentToken(t, f.srv, http.MethodPost,
		"/api/v1/projects/"+f.projectB.ID+"/agents/"+f.agentInB.Slug+"/keys", validKeysBody, token)
	assertKeysDenialOutcome(t, "project-scoped", projectScoped, http.StatusUnprocessableEntity, "cross_project_keys_unsupported")
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
	createCredTestAgent(t, s, targetID, project.ID, user.ID) // Phase: "running" by default

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
	// project's agent, so this must be an authorization "allowed" outcome
	// (503 keys_unavailable: admitted, but no dispatcher configured on this
	// fixture server) -- asserted by exact message, not merely "not
	// 401", so this sanity check cannot pass on an unrelated failure that
	// also happens to avoid 401.
	before := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agents/"+targetID+"/keys", validKeysBody, token)
	assertKeysDenialOutcome(t, "before revocation", before, http.StatusServiceUnavailable, "keys_unavailable")

	require.NoError(t, s.RevokeAgentCredential(ctx, cred.ID, "test", "explicit"))

	after := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agents/"+targetID+"/keys", validKeysBody, token)
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
	createCredTestAgent(t, s, targetID, project.ID, user.ID) // Phase: "running" by default
	callerID := tid("agentkeys-route-expired-caller")
	createCredTestAgent(t, s, callerID, project.ID, user.ID)

	// Positive control (round-3 review finding 2): the same setup, minted
	// with the normal (non-expired) duration, must reach the gate and be
	// allowed -- proving the 401 asserted below comes from expiry
	// specifically, not from some unrelated misconfiguration in this
	// fixture that would 401 regardless of TokenDuration.
	validToken, err := srv.GenerateAgentToken(callerID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	before := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agents/"+targetID+"/keys", validKeysBody, validToken)
	assertKeysDenialOutcome(t, "control", before, http.StatusServiceUnavailable, "keys_unavailable")

	origDuration := srv.agentTokenService.config.TokenDuration
	srv.agentTokenService.config.TokenDuration = -1 * time.Hour // already expired at mint time
	expiredToken, err := srv.GenerateAgentToken(callerID, project.ID, nil, AgentRoleFull, nil)
	srv.agentTokenService.config.TokenDuration = origDuration
	require.NoError(t, err)

	rec := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agents/"+targetID+"/keys", validKeysBody, expiredToken)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an expired token (never reaching authorizeAgentKeys), got %d: %s",
			rec.Code, rec.Body.String())
	}

	// Round-3 review (finding 1): the same expired token on the
	// project-scoped route shape must also 401 before reaching either the
	// cross-project pre-check or authorizeAgentKeys.
	recP := doRequestWithAgentToken(t, srv, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/"+targetID+"/keys", validKeysBody, expiredToken)
	if recP.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an expired token on the project-scoped route, got %d: %s",
			recP.Code, recP.Body.String())
	}
}

// newValidationOrderingFixture builds an agentKeysRouteFixture plus a
// lookup-count spy store and a fully-controllable fake dispatcher, for
// TestAgentActionKeysRoute_ValidationPrecedesResolutionAndAuthorization.
func newValidationOrderingFixture(t *testing.T) (*agentKeysRouteFixture, *agentKeysLookupSpyStore, *fakeAgentKeysDispatcher) {
	t.Helper()
	f := newAgentKeysRouteFixture(t)
	spy := &agentKeysLookupSpyStore{Store: f.store}
	f.srv.store = spy
	d := newFakeAgentKeysDispatcher()
	f.srv.SetDispatcher(d)
	return f, spy, d
}

// assertValidationOrderingOutcome asserts rec is exactly the given
// pre-operation-ID validation failure (wantStatus/wantCode -- never merely
// "400 or 413", since that would not catch e.g. an oversized keys field
// regressing from 413 payload_too_large to 400, or an unknown field
// regressing from 400 to 413), with no operation_id in details, and that
// neither an agent lookup nor a dispatcher call ever happened -- proving
// validation ran, and refused the request with the correct outcome, before
// resolution or authorization got a chance to run at all.
func assertValidationOrderingOutcome(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string, spy *agentKeysLookupSpyStore, d *fakeAgentKeysDispatcher) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d: %s", rec.Code, wantStatus, rec.Body.String())
	}
	env := decodeKeysError(t, rec.Body.Bytes())
	if env.Code != wantCode {
		t.Errorf("code = %q, want %q", env.Code, wantCode)
	}
	if _, present := env.Details["operation_id"]; present {
		t.Errorf("a validation failure must not carry an operation_id: %v", env.Details)
	}
	if got := spy.lookupCount(); got != 0 {
		t.Errorf("expected zero agent lookups before validation, got %d", got)
	}
	if got := d.callCount(); got != 0 {
		t.Errorf("expected zero dispatcher calls before validation, got %d", got)
	}
}

// TestAgentActionKeysRoute_ValidationPrecedesResolutionAndAuthorization pins
// contract §3 invariant 2 (ValidateBody precedes both agent-target
// resolution and authorization) on both route shapes, against a
// nonexistent target, a foreign target (agent caller), and a denied caller.
// Before this test, only this file's header comment and
// execute_agent_keys.go's doc comments asserted the ordering; nothing
// failed if beginAgentKeysRequest moved after resolution or authorization.
func TestAgentActionKeysRoute_ValidationPrecedesResolutionAndAuthorization(t *testing.T) {
	invalidBodies := []struct {
		name       string
		body       map[string]string
		wantStatus int
		wantCode   string
	}{
		{name: "invalid: unknown field", body: map[string]string{"keys": "C-c", "unexpected": "field"},
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "invalid: missing keys", body: map[string]string{},
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "invalid: empty keys", body: map[string]string{"keys": ""},
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "oversized: keys field 4097B", body: map[string]string{"keys": strings.Repeat("a", agentkeys.MaxBytes+1)},
			wantStatus: http.StatusRequestEntityTooLarge, wantCode: "payload_too_large"},
		{name: "oversized: raw body > 32KiB", body: map[string]string{"keys": strings.Repeat("a", agentkeys.MaxHTTPBodyBytes+100)},
			wantStatus: http.StatusRequestEntityTooLarge, wantCode: "payload_too_large"},
	}

	for _, bc := range invalidBodies {
		t.Run(bc.name, func(t *testing.T) {
			t.Run("nonexistent target/top-level", func(t *testing.T) {
				f, spy, d := newValidationOrderingFixture(t)
				token := f.agentToken(t, tid("agentkeys-route-valorder-caller"), f.projectA.ID, ScopeAgentLifecycle)
				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost,
					"/api/v1/agents/"+tid("agentkeys-route-valorder-nonexistent")+"/keys", bc.body, token)
				assertValidationOrderingOutcome(t, rec, bc.wantStatus, bc.wantCode, spy, d)
			})
			t.Run("nonexistent target/project-scoped", func(t *testing.T) {
				f, spy, d := newValidationOrderingFixture(t)
				token := f.agentToken(t, tid("agentkeys-route-valorder-caller"), f.projectA.ID, ScopeAgentLifecycle)
				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost,
					"/api/v1/projects/"+f.projectA.ID+"/agents/does-not-exist/keys", bc.body, token)
				assertValidationOrderingOutcome(t, rec, bc.wantStatus, bc.wantCode, spy, d)
			})
			t.Run("foreign target (agent caller)/top-level", func(t *testing.T) {
				f, spy, d := newValidationOrderingFixture(t)
				token := f.agentToken(t, tid("agentkeys-route-valorder-caller"), f.projectA.ID, ScopeAgentLifecycle)
				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost,
					"/api/v1/agents/"+f.agentInB.ID+"/keys", bc.body, token)
				assertValidationOrderingOutcome(t, rec, bc.wantStatus, bc.wantCode, spy, d)
			})
			t.Run("foreign target (agent caller)/project-scoped", func(t *testing.T) {
				f, spy, d := newValidationOrderingFixture(t)
				token := f.agentToken(t, tid("agentkeys-route-valorder-caller"), f.projectA.ID, ScopeAgentLifecycle)
				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost,
					"/api/v1/projects/"+f.projectB.ID+"/agents/"+f.agentInB.Slug+"/keys", bc.body, token)
				assertValidationOrderingOutcome(t, rec, bc.wantStatus, bc.wantCode, spy, d)
			})
			t.Run("denied caller/top-level", func(t *testing.T) {
				f, spy, d := newValidationOrderingFixture(t)
				token := f.agentToken(t, tid("agentkeys-route-valorder-denied"), f.projectA.ID) // no lifecycle scope
				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost,
					"/api/v1/agents/"+f.agentInA.ID+"/keys", bc.body, token)
				assertValidationOrderingOutcome(t, rec, bc.wantStatus, bc.wantCode, spy, d)
			})
			t.Run("denied caller/project-scoped", func(t *testing.T) {
				f, spy, d := newValidationOrderingFixture(t)
				token := f.agentToken(t, tid("agentkeys-route-valorder-denied"), f.projectA.ID)
				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost,
					"/api/v1/projects/"+f.projectA.ID+"/agents/"+f.agentInA.Slug+"/keys", bc.body, token)
				assertValidationOrderingOutcome(t, rec, bc.wantStatus, bc.wantCode, spy, d)
			})
		})
	}
}

// TestAgentActionKeysRoute_Unauthenticated401_NoOperationID covers plan row
// AK-19: a /keys request with no credential at all (no Authorization
// header, no agent token header) must fail with 401 before any operation ID
// is minted, on both route shapes.
func TestAgentActionKeysRoute_Unauthenticated401_NoOperationID(t *testing.T) {
	for _, shape := range keysRouteShapes {
		t.Run(shape.name, func(t *testing.T) {
			f := newAgentKeysRouteFixture(t)

			body, err := json.Marshal(validKeysBody)
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, shape.path(f.agentInA), bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			// Deliberately no Authorization / agent-token header.

			rec := httptest.NewRecorder()
			f.srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s: status = %d, want 401; body: %s", shape.name, rec.Code, rec.Body.String())
			}
			env := decodeKeysError(t, rec.Body.Bytes())
			if _, ok := env.Details["operation_id"]; ok {
				t.Errorf("%s: expected no operation_id key at all, got details: %v", shape.name, env.Details)
			}
		})
	}
}

// keysParityOutcome is one expected /keys route result: the HTTP status and
// error code. An admitted call reaches real admission and ends in 503
// keys_unavailable, because the fixture server has no dispatcher.
type keysParityOutcome struct {
	status int
	code   string
}

var (
	keysParityAdmitted     = keysParityOutcome{http.StatusServiceUnavailable, "keys_unavailable"}
	keysParityDenied       = keysParityOutcome{http.StatusForbidden, "keys_denied"}
	keysParityCrossProject = keysParityOutcome{http.StatusUnprocessableEntity, "cross_project_keys_unsupported"}
)

// TestAgentActionKeysRoute_ParityTable pins the allow and deny result of the
// keys gate for user session, user access token (UAT) and agent callers, on
// both route shapes, for a same-project and a cross-project target. Every
// request goes through the real mux and the real authentication middleware.
// The expected values are the attach-based results (ptone/scion#3517): the
// gate's evaluator may change, these results may not.
func TestAgentActionKeysRoute_ParityTable(t *testing.T) {
	f := newAgentKeysRouteFixture(t)
	createTestUserWithProjectRole(t, f.store, f.owner.ID, f.owner.Email, f.projectA.ID, store.ProjectRoleOwner)
	createTestUserWithProjectRole(t, f.store, f.owner.ID, f.owner.Email, f.projectB.ID, store.ProjectRoleOwner)

	type requester func(t *testing.T, path string) *httptest.ResponseRecorder
	asUser := func(u *store.User) requester {
		return func(t *testing.T, path string) *httptest.ResponseRecorder {
			return doRequestAsUser(t, f.srv, u, http.MethodPost, path, validKeysBody)
		}
	}
	withUAT := func(projectID string, scopes ...string) requester {
		key := mintScopedUAT(t, f.srv, f.owner.ID, projectID, scopes)
		return func(t *testing.T, path string) *httptest.ResponseRecorder {
			return doRequestWithUAT(t, f.srv, key, http.MethodPost, path, validKeysBody)
		}
	}
	asAgent := func(name, projectID string, scopes ...AgentTokenScope) requester {
		token := f.agentToken(t, tid(name), projectID, scopes...)
		return func(t *testing.T, path string) *httptest.ResponseRecorder {
			return doRequestWithAgentToken(t, f.srv, http.MethodPost, path, validKeysBody, token)
		}
	}

	ownerSession := asUser(f.owner)
	nonOwnerSession := asUser(f.nonOwner)
	uatAttachA := withUAT(f.projectA.ID, "agent:attach")
	uatAttachB := withUAT(f.projectB.ID, "agent:attach")
	uatLifecycleA := withUAT(f.projectA.ID, "agent:lifecycle")
	uatMessageA := withUAT(f.projectA.ID, "agent:message")
	agentLifecycleA := asAgent("agentkeys-parity-lifecycle", f.projectA.ID, ScopeAgentLifecycle)
	agentCreateA := asAgent("agentkeys-parity-create", f.projectA.ID, ScopeAgentCreate)
	agentNotifyA := asAgent("agentkeys-parity-notify", f.projectA.ID, ScopeAgentNotify)
	agentNoScopeA := asAgent("agentkeys-parity-noscope", f.projectA.ID)

	cases := []struct {
		name         string
		do           requester
		target       *store.Agent
		wantTopLevel keysParityOutcome
		wantProject  keysParityOutcome
	}{
		// User session: attach authority on the target decides; project
		// boundaries do not apply to a human caller.
		{"user owner, same project", ownerSession, f.agentInA, keysParityAdmitted, keysParityAdmitted},
		{"user owner, cross project", ownerSession, f.agentInB, keysParityAdmitted, keysParityAdmitted},
		{"user non-owner, same project", nonOwnerSession, f.agentInA, keysParityDenied, keysParityDenied},
		{"user non-owner, cross project", nonOwnerSession, f.agentInB, keysParityDenied, keysParityDenied},

		// UAT: the token's project boundary and exact scopes apply on top of
		// the holder's attach authority.
		{"UAT attach scope, token project matches target", uatAttachA, f.agentInA, keysParityAdmitted, keysParityAdmitted},
		{"UAT attach scope, token project differs from target", uatAttachA, f.agentInB, keysParityDenied, keysParityDenied},
		{"UAT attach scope in other project, matching target", uatAttachB, f.agentInB, keysParityAdmitted, keysParityAdmitted},
		{"UAT lifecycle scope only, same project", uatLifecycleA, f.agentInA, keysParityDenied, keysParityDenied},
		{"UAT message scope only, same project", uatMessageA, f.agentInA, keysParityDenied, keysParityDenied},
		{"UAT lifecycle scope only, cross project", uatLifecycleA, f.agentInB, keysParityDenied, keysParityDenied},

		// Agent credential: lifecycle scope and the target's project are
		// required; a project mismatch has its own outcome. The project-scoped
		// route decides the mismatch before scope and before target lookup.
		{"agent lifecycle scope, same project", agentLifecycleA, f.agentInA, keysParityAdmitted, keysParityAdmitted},
		{"agent lifecycle scope, cross project", agentLifecycleA, f.agentInB, keysParityCrossProject, keysParityCrossProject},
		{"agent create scope only, same project", agentCreateA, f.agentInA, keysParityDenied, keysParityDenied},
		{"agent create scope only, cross project", agentCreateA, f.agentInB, keysParityDenied, keysParityCrossProject},
		{"agent notify scope only, same project", agentNotifyA, f.agentInA, keysParityDenied, keysParityDenied},
		{"agent with no scopes, same project", agentNoScopeA, f.agentInA, keysParityDenied, keysParityDenied},
		{"agent with no scopes, cross project", agentNoScopeA, f.agentInB, keysParityDenied, keysParityCrossProject},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			topLevel := tc.do(t, "/api/v1/agents/"+tc.target.ID+"/keys")
			assertKeysDenialOutcome(t, "top-level", topLevel, tc.wantTopLevel.status, tc.wantTopLevel.code)

			projectScoped := tc.do(t, "/api/v1/projects/"+tc.target.ProjectID+"/agents/"+tc.target.Slug+"/keys")
			assertKeysDenialOutcome(t, "project-scoped", projectScoped, tc.wantProject.status, tc.wantProject.code)
		})
	}
}
