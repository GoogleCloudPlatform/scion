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
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the deletion detail visibility rule (ptone/scion#3122): code,
// error and claim of the deletion view reach unscoped local platform admins
// only; every other caller gets the same view without them.

// deletionDetailKeys are the DeletionInfo JSON keys only admins see.
var deletionDetailKeys = []string{"code", "error", "claim"}

// deletionJSON returns the JSON object of a deletion view, or nil for a nil
// view.
func deletionJSON(t *testing.T, info *store.DeletionInfo) map[string]json.RawMessage {
	t.Helper()
	if info == nil {
		return nil
	}
	raw, err := json.Marshal(info)
	require.NoError(t, err)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

// rawDeletionObject decodes a deletion value taken from a response body:
// nil for an explicit null.
func rawDeletionObject(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	if string(raw) == "null" {
		return nil
	}
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &m), string(raw))
	return m
}

// assertGenericDeletionOf asserts that generic is exactly the admin view
// without the detail keys: same presence (both null or both objects), no
// code, error or claim, and every other key byte-identical.
func assertGenericDeletionOf(t *testing.T, admin, generic map[string]json.RawMessage, label string) {
	t.Helper()
	if admin == nil {
		assert.Nil(t, generic, "%s: deletion is null for admin, so it must be null for this caller", label)
		return
	}
	require.NotNil(t, generic, "%s: deletion is an object for admin, so it must be an object for this caller", label)
	for _, k := range deletionDetailKeys {
		_, present := generic[k]
		assert.False(t, present, "%s: %q must be absent from the generic view", label, k)
	}
	want := map[string]string{}
	for k, v := range admin {
		want[k] = string(v)
	}
	for _, k := range deletionDetailKeys {
		delete(want, k)
	}
	got := map[string]string{}
	for k, v := range generic {
		got[k] = string(v)
	}
	assert.Equal(t, want, got, "%s: state, stage, soft and timestamps match the admin view", label)
}

// seedDeletionWithError seeds a delete marker and then sets its error text.
func seedDeletionWithError(t *testing.T, s store.Store, agentID string, d deleteSeed, errText string) {
	t.Helper()
	seedAgentDeletion(t, s, agentID, d)
	if errText == "" {
		return
	}
	n, err := s.UpdateAgentDeletion(context.Background(), agentID, store.DeletionPredicate{}, store.DeletionFields{Error: &errText})
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

// adminPublishCtx is a context carrying an unscoped local platform admin, the
// one caller class that sees the detail on REST. Status events are published
// with the request context (performAgentDelete publishes the claim snapshot
// with it), so the SSE tests publish with this context: the event must still
// carry the generic view.
func adminPublishCtx(t *testing.T) context.Context {
	t.Helper()
	ctx := contextWithIdentity(context.Background(), NewAuthenticatedUser("u-pub-admin", "pub-admin@test.com", "Admin", "admin", "web"))
	require.True(t, callerSeesDeletionDetail(ctx), "the publish context is an admin context")
	return ctx
}

// --- the redaction function ---

func TestRedactDeletionForCaller(t *testing.T) {
	lease := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	exp := lease.Add(store.DeletionDisplayTTL)
	in := &store.DeletionInfo{
		State: store.DeletionStateFailed, Code: store.DeletionCodeRuntimeError, Error: "dial tcp 10.0.0.7:9800: refused",
		Soft: true, Claim: 4, StartedAt: lease.Add(-time.Minute), LeaseExpiresAt: &lease, ExpiresAt: &exp,
		Stage: store.DeletionStageFinalizing,
	}
	before := *in

	assert.Nil(t, redactDeletionForCaller(nil, true), "nil stays nil for an admin")
	assert.Nil(t, redactDeletionForCaller(nil, false), "nil stays nil for anyone else")

	admin := redactDeletionForCaller(in, true)
	require.NotNil(t, admin)
	assert.Equal(t, before, *admin, "an admin gets the view unchanged")

	generic := redactDeletionForCaller(in, false)
	require.NotNil(t, generic)
	assert.Empty(t, generic.Code)
	assert.Empty(t, generic.Error)
	assert.Zero(t, generic.Claim)
	assert.Equal(t, before.State, generic.State)
	assert.Equal(t, before.Stage, generic.Stage)
	assert.Equal(t, before.Soft, generic.Soft)
	assert.Equal(t, before.StartedAt, generic.StartedAt)
	assert.Equal(t, before.LeaseExpiresAt, generic.LeaseExpiresAt)
	assert.Equal(t, before.ExpiresAt, generic.ExpiresAt)
	assert.Equal(t, before, *in, "the input is not modified")

	adminJSON := deletionJSON(t, admin)
	for _, k := range deletionDetailKeys {
		assert.Contains(t, adminJSON, k, "admin JSON carries %q", k)
	}
	assertGenericDeletionOf(t, adminJSON, deletionJSON(t, generic), "generic JSON")
}

// --- the predicate ---

// TestCallerSeesDeletionDetail_OnlyUnscopedLocalAdmins pins the caller
// classes: only an unscoped local user with the admin role (including the
// dev-auth user) sees the detail. A missing identity, a typed-nil user, an
// agent, a broker, a member, a scoped admin token and a federated admin all
// get the generic view.
func TestCallerSeesDeletionDetail_OnlyUnscopedLocalAdmins(t *testing.T) {
	admin := NewAuthenticatedUser("u-admin", "admin@test.com", "Admin", "admin", "web")
	member := NewAuthenticatedUser("u-member", "member@test.com", "Member", "member", "web")
	agent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: "a-1"},
		ProjectID: "p-1",
		Scopes:    []AgentTokenScope{ScopeProjectRead, ScopeAgentLifecycle},
	}}
	cases := []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"nil context", nil, false},
		{"no identity", context.Background(), false},
		{"typed-nil user", contextWithIdentity(context.Background(), (*AuthenticatedUser)(nil)), false},
		{"agent", contextWithIdentity(context.Background(), agent), false},
		{"broker", contextWithIdentity(context.Background(), NewBrokerIdentity("b-1")), false},
		{"member", contextWithIdentity(context.Background(), member), false},
		{"scoped admin token", contextWithIdentity(context.Background(), NewScopedUserIdentity(admin, "p-1", []string{"agent:manage"})), false},
		{"federated admin", contextWithIdentity(context.Background(), NewFederatedUserIdentity("https://issuer.example", "sub", "admin@example.com", "Admin", "admin", nil)), false},
		{"unscoped local admin", contextWithIdentity(context.Background(), admin), true},
		{"dev-auth user", contextWithIdentity(context.Background(), &DevUser{id: DevUserID}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, callerSeesDeletionDetail(tc.ctx))
		})
	}
}

// --- enrichment (every list builder, compact, single GET) fails closed ---

// TestEnrichAgent_DeletionDetailFailsClosed calls the two enrichment
// functions every agent-returning REST surface uses with each caller class
// on the context: only the unscoped local admin gets code, error and claim;
// every other context, including one with no identity, gets the generic
// view with the same state and timestamps.
func TestEnrichAgent_DeletionDetailFailsClosed(t *testing.T) {
	srv, s := testServer(t)
	agent := setupBrokerAgentInPhase(t, s, "redact-enrich", state.PhaseRunning)
	seedDeletionWithError(t, s, agent.ID, deleteSeed{state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeRuntimeError}, "broker said: disk /var/lib/x full")

	admin := NewAuthenticatedUser("u-admin", "admin@test.com", "Admin", "admin", "web")
	ctxs := map[string]context.Context{
		"no identity":        context.Background(),
		"agent":              contextWithIdentity(context.Background(), &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: agent.ID}, ProjectID: agent.ProjectID}}),
		"broker":             contextWithIdentity(context.Background(), NewBrokerIdentity("b-1")),
		"member":             contextWithIdentity(context.Background(), NewAuthenticatedUser("u-m", "m@test.com", "M", "member", "web")),
		"scoped admin token": contextWithIdentity(context.Background(), NewScopedUserIdentity(admin, agent.ProjectID, []string{"agent:manage"})),
		"federated admin":    contextWithIdentity(context.Background(), NewFederatedUserIdentity("https://issuer.example", "sub", "admin@example.com", "Admin", "admin", nil)),
	}
	adminCtx := contextWithIdentity(context.Background(), admin)

	single := func(ctx context.Context) map[string]json.RawMessage {
		a := mustGetAgent(t, s, agent.ID)
		srv.enrichAgent(ctx, a, nil, nil)
		return deletionJSON(t, a.Deletion)
	}
	list := func(ctx context.Context) map[string]json.RawMessage {
		items := []store.Agent{*mustGetAgent(t, s, agent.ID)}
		srv.enrichAgents(ctx, items)
		return deletionJSON(t, items[0].Deletion)
	}

	for surface, get := range map[string]func(context.Context) map[string]json.RawMessage{"enrichAgent": single, "enrichAgents": list} {
		adminView := get(adminCtx)
		require.NotNil(t, adminView, "%s: admin view", surface)
		assert.JSONEq(t, `"runtime_error"`, string(adminView["code"]), "%s: admin sees the code", surface)
		assert.JSONEq(t, `"broker said: disk /var/lib/x full"`, string(adminView["error"]), "%s: admin sees the error", surface)
		assert.Contains(t, adminView, "claim", "%s: admin sees the claim", surface)
		for name, ctx := range ctxs {
			assertGenericDeletionOf(t, adminView, get(ctx), surface+" "+name)
		}
	}
}

// TestWriteAgentGetResponse_DeletionAdminVsNonAdmin checks the single agent
// GET body for an admin and a non-admin caller.
func TestWriteAgentGetResponse_DeletionAdminVsNonAdmin(t *testing.T) {
	srv, s := testServer(t)
	agent := setupBrokerAgentInPhase(t, s, "redact-get", state.PhaseRunning)
	seedDeletionWithError(t, s, agent.ID, deleteSeed{state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeInDoubt}, "teardown on broker b-7 unconfirmed")

	get := func(identity Identity) map[string]json.RawMessage {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
		req = req.WithContext(contextWithIdentity(req.Context(), identity))
		rec := httptest.NewRecorder()
		srv.writeAgentGetResponse(rec, req, mustGetAgent(t, s, agent.ID))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return rawDeletionObject(t, decodeObject(t, rec.Body.Bytes())["deletion"])
	}
	adminView := get(NewAuthenticatedUser("u-admin", "admin@test.com", "Admin", "admin", "web"))
	require.NotNil(t, adminView)
	assert.JSONEq(t, `"in_doubt"`, string(adminView["code"]))
	assert.JSONEq(t, `"teardown on broker b-7 unconfirmed"`, string(adminView["error"]))
	assertGenericDeletionOf(t, adminView, get(NewAuthenticatedUser("u-m", "m@test.com", "M", "member", "web")), "member GET")
}

// --- lifecycle and managed-agent responses ---

// lifecycleAs calls handleAgentLifecycle (authorization already ran in its
// caller) with identity on the request context and returns the response's
// deletion object.
func lifecycleAs(t *testing.T, srv *Server, identity Identity, agentID, action string) map[string]json.RawMessage {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/"+action, nil)
	req = req.WithContext(contextWithIdentity(req.Context(), identity))
	rec := httptest.NewRecorder()
	srv.handleAgentLifecycle(rec, req, agentID, action)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	raw, ok := decodeObject(t, rec.Body.Bytes())["deletion"]
	require.True(t, ok, "lifecycle response carries deletion: %s", rec.Body.String())
	return rawDeletionObject(t, raw)
}

// A stop on an agent a delete holds is a 200 no-op that returns the agent
// with its deletion view (handleAgentLifecycle's deleteStopNoop branch).
func TestLifecycleStopNoop_DeletionAdminVsNonAdmin(t *testing.T) {
	srv, s := testServer(t)
	agent := setupBrokerAgentInPhase(t, s, "redact-stopnoop", state.PhaseRunning)
	seedDeletionWithError(t, s, agent.ID, seedLiveDeleting, "previous attempt: broker timeout")

	adminView := lifecycleAs(t, srv, NewAuthenticatedUser("u-admin", "admin@test.com", "Admin", "admin", "web"), agent.ID, "stop")
	require.NotNil(t, adminView)
	assert.JSONEq(t, `"deleting"`, string(adminView["state"]))
	assert.Contains(t, adminView, "claim")
	assert.Contains(t, adminView, "error")
	assertGenericDeletionOf(t, adminView, lifecycleAs(t, srv, NewAuthenticatedUser("u-m", "m@test.com", "M", "member", "web"), agent.ID, "stop"), "member stop no-op")
	assertGenericDeletionOf(t, adminView, lifecycleAs(t, srv, &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: "a-peer"}, ProjectID: agent.ProjectID}}, agent.ID, "stop"), "agent stop no-op")
}

// raceClaimErrorDispatcher seeds a live delete claim with an error text from
// inside the start dispatch, so the start answers from the claimed row.
type raceClaimErrorDispatcher struct {
	deleteGuardDispatcher
	t       *testing.T
	s       store.Store
	errText string
}

func (d *raceClaimErrorDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, task string, resume bool) error {
	if err := d.deleteGuardDispatcher.DispatchAgentStart(ctx, agent, task, resume); err != nil {
		return err
	}
	seedDeletionWithError(d.t, d.s, agent.ID, seedLiveDeleting, d.errText)
	return nil
}

// A start that loses the race to a delete claim answers from the stored row
// (handleAgentLifecycle's final response).
func TestLifecycleStartRacedByDelete_DeletionAdminVsNonAdmin(t *testing.T) {
	views := map[string]map[string]json.RawMessage{}
	for _, c := range []struct {
		name     string
		identity Identity
	}{
		{"admin", NewAuthenticatedUser("u-admin", "admin@test.com", "Admin", "admin", "web")},
		{"member", NewAuthenticatedUser("u-m", "m@test.com", "M", "member", "web")},
	} {
		srv, s := testServer(t)
		srv.SetDispatcher(&raceClaimErrorDispatcher{t: t, s: s, errText: "claim error detail"})
		srv.SetEventPublisher(&trackingEventPublisher{})
		agent := setupBrokerAgentInPhase(t, s, "redact-race-"+c.name, state.PhaseStopped)
		views[c.name] = lifecycleAs(t, srv, c.identity, agent.ID, "start")
	}
	require.NotNil(t, views["admin"])
	assert.JSONEq(t, `"claim error detail"`, string(views["admin"]["error"]))
	assert.Contains(t, views["admin"], "claim")
	// The two runs use separate servers, so their timestamps differ:
	// compare the shape, not the bytes.
	require.NotNil(t, views["member"])
	for _, k := range deletionDetailKeys {
		assert.NotContains(t, views["member"], k)
	}
	assert.JSONEq(t, string(views["admin"]["state"]), string(views["member"]["state"]))
	for _, k := range []string{"startedAt", "leaseExpiresAt", "soft"} {
		assert.Contains(t, views["member"], k)
	}
}

// A stop whose run was replaced while it was in flight is not recorded and
// answers from the current row (handleAgentLifecycle's stop branch when
// recordStopStatus reports not recorded). The row carries a failed delete
// with an error text, which only the admin may see.
func TestLifecycleStopNotRecorded_DeletionAdminVsNonAdmin(t *testing.T) {
	views := map[string]map[string]json.RawMessage{}
	for _, c := range []struct {
		name     string
		identity Identity
	}{
		{"admin", NewAuthenticatedUser("u-admin", "admin@test.com", "Admin", "admin", "web")},
		{"member", NewAuthenticatedUser("u-m", "m@test.com", "M", "member", "web")},
	} {
		srv, s := testServer(t)
		_, _, agent := setupOnlineBrokerAgent(t, s, "redact-stop-notrec-"+c.name)
		_, err := s.SetAgentRunID(context.Background(), agent.ID, "run-old")
		require.NoError(t, err)
		seedDeletionWithError(t, s, agent.ID, deleteSeed{state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeRuntimeError}, "broker said: stop detail")
		client := &runSwapStopClient{s: s, agentID: agent.ID}
		srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
		views[c.name] = lifecycleAs(t, srv, c.identity, agent.ID, "stop")
		require.Equal(t, "run-old", client.lastStopRunID, "%s: the stop went out for the old run", c.name)
		require.Equal(t, "run-new", mustGetAgent(t, s, agent.ID).RunID, "%s: the row moved to the new run, so the stop was not recorded", c.name)
	}
	require.NotNil(t, views["admin"])
	assert.JSONEq(t, `"failed"`, string(views["admin"]["state"]))
	assert.JSONEq(t, `"runtime_error"`, string(views["admin"]["code"]))
	assert.JSONEq(t, `"broker said: stop detail"`, string(views["admin"]["error"]))
	assert.Contains(t, views["admin"], "claim")
	// Separate servers, so the timestamps differ: compare the shape.
	require.NotNil(t, views["member"])
	for _, k := range deletionDetailKeys {
		assert.NotContains(t, views["member"], k, "member sees %s", k)
	}
	assert.JSONEq(t, string(views["admin"]["state"]), string(views["member"]["state"]))
	for _, k := range []string{"startedAt", "soft"} {
		assert.Contains(t, views["member"], k)
	}
}

// claimAfterStatusStore seeds a live delete claim right after the first
// successful UpdateAgentStatus, so a managed lifecycle action answers from
// the claimed row.
type claimAfterStatusStore struct {
	store.Store
	once  sync.Once
	claim func()
}

func (c *claimAfterStatusStore) UpdateAgentStatus(ctx context.Context, id string, su store.AgentStatusUpdate) error {
	if err := c.Store.UpdateAgentStatus(ctx, id, su); err != nil {
		return err
	}
	c.once.Do(c.claim)
	return nil
}

func TestManagedLifecycle_DeletionAdminVsNonAdmin(t *testing.T) {
	views := map[string]map[string]json.RawMessage{}
	for _, c := range []struct {
		name     string
		identity Identity
	}{
		{"admin", NewAuthenticatedUser("u-admin", "admin@test.com", "Admin", "admin", "web")},
		{"member", NewAuthenticatedUser("u-m", "m@test.com", "M", "member", "web")},
	} {
		srv, s := testServer(t)
		agent := setupBrokerAgentInPhase(t, s, "redact-managed-"+c.name, state.PhaseStopped)
		agent.Runtime = ManagedRuntimePrefix + "test"
		require.NoError(t, s.UpdateAgent(context.Background(), agent))
		srv.store = &claimAfterStatusStore{Store: s, claim: func() {
			seedDeletionWithError(t, s, agent.ID, seedLiveDeleting, "managed claim detail")
		}}
		views[c.name] = lifecycleAs(t, srv, c.identity, agent.ID, "start")
	}
	require.NotNil(t, views["admin"])
	assert.JSONEq(t, `"managed claim detail"`, string(views["admin"]["error"]))
	assert.Contains(t, views["admin"], "claim")
	require.NotNil(t, views["member"])
	for _, k := range deletionDetailKeys {
		assert.NotContains(t, views["member"], k)
	}
	assert.JSONEq(t, string(views["admin"]["state"]), string(views["member"]["state"]))
}

// --- delete 202 and failure bodies ---

// errorMessageAndDetails decodes the standard error envelope's message and
// details.
func errorMessageAndDetails(t *testing.T, rec *httptest.ResponseRecorder) (string, string, map[string]interface{}) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string                 `json:"code"`
			Message string                 `json:"message"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body.Error.Code, body.Error.Message, body.Error.Details
}

// TestWriteDeletionFailure_AdminVsNonAdmin: same status, API code and
// Retry-After for both; the admin body carries the deletion code and the
// failure text, the non-admin body a generic message and no deletion code.
func TestWriteDeletionFailure_AdminVsNonAdmin(t *testing.T) {
	cases := []struct {
		code       string
		wantStatus int
		wantAPI    string
	}{
		{store.DeletionCodeConflict, http.StatusConflict, ErrCodeConflict},
		{store.DeletionCodeRuntimeUnavailable, http.StatusServiceUnavailable, brokerCodeRuntimeUnavailable},
		{store.DeletionCodeRuntimeError, http.StatusBadGateway, ErrCodeRuntimeError},
		{store.DeletionCodeInDoubt, http.StatusBadGateway, ErrCodeRuntimeError},
		{store.DeletionCodeAbandoned, http.StatusBadGateway, ErrCodeRuntimeError},
		{store.DeletionCodeRevokeFailed, http.StatusBadGateway, ErrCodeRuntimeError},
	}
	const detail = "rpc error: broker b-3 at 10.1.2.3 said no"
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			adminRec := httptest.NewRecorder()
			writeDeletionFailure(adminRec, "agent-1", tc.code, detail, "9", true)
			userRec := httptest.NewRecorder()
			writeDeletionFailure(userRec, "agent-1", tc.code, detail, "9", false)

			assert.Equal(t, tc.wantStatus, adminRec.Code)
			assert.Equal(t, tc.wantStatus, userRec.Code, "status does not depend on the caller")
			assert.Equal(t, adminRec.Header().Get("Retry-After"), userRec.Header().Get("Retry-After"))

			apiCode, msg, details := errorMessageAndDetails(t, adminRec)
			assert.Equal(t, tc.wantAPI, apiCode)
			assert.Equal(t, detail, msg)
			assert.Equal(t, tc.code, details["deletionCode"])
			assert.Equal(t, "agent-1", details["agentId"])

			apiCode, msg, details = errorMessageAndDetails(t, userRec)
			assert.Equal(t, tc.wantAPI, apiCode, "API error code does not depend on the caller")
			assert.Equal(t, genericDeleteFailedMessage, msg)
			assert.NotContains(t, details, "deletionCode")
			assert.Equal(t, "agent-1", details["agentId"])
			assert.NotContains(t, userRec.Body.String(), detail)
			assert.NotContains(t, userRec.Body.String(), "deletionCode")
		})
	}
	// An empty message: the admin gets the code in the default message, the
	// non-admin the same generic message.
	adminRec := httptest.NewRecorder()
	writeDeletionFailure(adminRec, "agent-1", store.DeletionCodeFinalizeFailed, "", "", true)
	_, msg, _ := errorMessageAndDetails(t, adminRec)
	assert.Contains(t, msg, store.DeletionCodeFinalizeFailed)
	userRec := httptest.NewRecorder()
	writeDeletionFailure(userRec, "agent-1", store.DeletionCodeFinalizeFailed, "", "", false)
	_, msg, _ = errorMessageAndDetails(t, userRec)
	assert.Equal(t, genericDeleteFailedMessage, msg)
}

// TestDeleteBrokerFailure_BodyAdminVsNonAdmin runs a real DELETE whose
// broker call fails, as the dev admin and as the agent's (non-admin) owner.
func TestDeleteBrokerFailure_BodyAdminVsNonAdmin(t *testing.T) {
	const detail = "broker boom: /var/run/secret path"
	srv, s, _, disp := engineTestServer(t)
	disp.setFn(func(context.Context, *store.Agent) error { return errors.New(detail) })

	adminAgent := setupBrokerAgentInPhase(t, s, "redact-fail-admin", state.PhaseRunning)
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+adminAgent.ID, nil)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	_, msg, details := errorMessageAndDetails(t, rec)
	assert.Contains(t, msg, detail)
	assert.Equal(t, store.DeletionCodeRuntimeError, details["deletionCode"])

	userAgent := setupBrokerAgentInPhase(t, s, "redact-fail-owner", state.PhaseRunning)
	req := ownerDeleteRequest(t, s, context.Background(), userAgent.ID)
	rec = httptest.NewRecorder()
	srv.performAgentDelete(rec, req, mustGetAgent(t, s, userAgent.ID))
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	apiCode, msg, details := errorMessageAndDetails(t, rec)
	assert.Equal(t, ErrCodeRuntimeError, apiCode)
	assert.Equal(t, genericDeleteFailedMessage, msg)
	assert.NotContains(t, details, "deletionCode")
	assert.NotContains(t, rec.Body.String(), "broker boom")

	// The stored row keeps the full detail for admins and audits.
	got := mustGetAgent(t, s, userAgent.ID)
	assert.Equal(t, store.DeletionCodeRuntimeError, got.DeletionCode)
	assert.Contains(t, got.DeletionError, "broker boom")
}

// TestDeleteJoinFailure_BodyAdminVsNonAdmin covers the joiner's failure
// answer (resolveJoinFromRow) for both caller classes.
func TestDeleteJoinFailure_BodyAdminVsNonAdmin(t *testing.T) {
	srv, s := testServer(t)
	agent := setupBrokerAgentInPhase(t, s, "redact-join", state.PhaseRunning)
	seedDeletionWithError(t, s, agent.ID, deleteSeed{state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeConflict}, "broker: ambiguous container c-91")
	claim := mustGetAgent(t, s, agent.ID).DeletionClaim

	adminRec := httptest.NewRecorder()
	require.True(t, srv.resolveJoinFromRow(adminRec, context.Background(), agent.ID, claim, true))
	userRec := httptest.NewRecorder()
	require.True(t, srv.resolveJoinFromRow(userRec, context.Background(), agent.ID, claim, false))

	assert.Equal(t, http.StatusConflict, adminRec.Code)
	assert.Equal(t, http.StatusConflict, userRec.Code)
	_, msg, details := errorMessageAndDetails(t, adminRec)
	assert.Equal(t, "broker: ambiguous container c-91", msg)
	assert.Equal(t, store.DeletionCodeConflict, details["deletionCode"])
	_, msg, details = errorMessageAndDetails(t, userRec)
	assert.Equal(t, genericDeleteFailedMessage, msg)
	assert.NotContains(t, details, "deletionCode")
	assert.NotContains(t, userRec.Body.String(), "c-91")
}

// TestDeleteJoinNotCompleted_BodyAdminVsNonAdmin covers resolveJoinFromRow's
// other failure answer: the marker was cleared (state none) at a claim at or
// above the observed one, so the delete did not complete.
func TestDeleteJoinNotCompleted_BodyAdminVsNonAdmin(t *testing.T) {
	srv, s := testServer(t)
	agent := setupBrokerAgentInPhase(t, s, "redact-join-cleared", state.PhaseRunning)
	seedDeletionWithError(t, s, agent.ID, deleteSeed{state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeRuntimeError}, "old failure text")
	none := store.DeletionStateNone
	n, err := s.UpdateAgentDeletion(context.Background(), agent.ID, store.DeletionPredicate{}, store.DeletionFields{State: &none})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	cleared := mustGetAgent(t, s, agent.ID)
	require.Equal(t, store.DeletionStateNone, cleared.DeletionState)
	require.Positive(t, cleared.DeletionClaim)

	adminRec := httptest.NewRecorder()
	require.True(t, srv.resolveJoinFromRow(adminRec, context.Background(), agent.ID, cleared.DeletionClaim, true))
	userRec := httptest.NewRecorder()
	require.True(t, srv.resolveJoinFromRow(userRec, context.Background(), agent.ID, cleared.DeletionClaim, false))

	assert.Equal(t, http.StatusBadGateway, adminRec.Code)
	assert.Equal(t, http.StatusBadGateway, userRec.Code)
	_, msg, details := errorMessageAndDetails(t, adminRec)
	assert.Equal(t, "agent delete did not complete", msg)
	assert.Equal(t, store.DeletionCodeRuntimeError, details["deletionCode"])
	apiCode, msg, details := errorMessageAndDetails(t, userRec)
	assert.Equal(t, ErrCodeRuntimeError, apiCode)
	assert.Equal(t, genericDeleteFailedMessage, msg)
	assert.NotContains(t, details, "deletionCode")
	assert.NotContains(t, userRec.Body.String(), "did not complete")
}

// TestDeleteJoinerDeadline202_BodyAdminVsNonAdmin covers the joiner's 202
// at its deadline (joinAgentDeletion's timer) for both caller classes: a
// live delete held by another request never resolves before the deadline.
func TestDeleteJoinerDeadline202_BodyAdminVsNonAdmin(t *testing.T) {
	srv, s := testServer(t)
	agent := setupBrokerAgentInPhase(t, s, "redact-join-202", state.PhaseRunning)
	seedDeletionWithError(t, s, agent.ID, deleteSeed{state: store.DeletionStateDeleting, leaseIn: time.Hour}, "joiner detail text")
	claim := mustGetAgent(t, s, agent.ID).DeletionClaim

	join := func(isAdmin bool) map[string]json.RawMessage {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
		rec := httptest.NewRecorder()
		srv.joinAgentDeletion(rec, req, agent.ID, claim, time.Now().Add(200*time.Millisecond), isAdmin)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		obj := decodeObject(t, rec.Body.Bytes())
		assert.JSONEq(t, `"`+agent.ID+`"`, string(obj["agentId"]))
		return rawDeletionObject(t, obj["deletion"])
	}
	adminView := join(true)
	require.NotNil(t, adminView)
	assert.Contains(t, adminView, "claim")
	assert.JSONEq(t, `"joiner detail text"`, string(adminView["error"]))
	userView := join(false)
	require.NotNil(t, userView)
	for _, k := range deletionDetailKeys {
		assert.NotContains(t, userView, k, "non-admin joiner 202 has no %q", k)
	}
	assertGenericDeletionOf(t, adminView, userView, "joiner 202")
}

// TestDeleteAccepted202_BodyAdminVsNonAdmin covers the 202 body, through the
// writer for both classes and through a real slow DELETE for both.
func TestDeleteAccepted202_BodyAdminVsNonAdmin(t *testing.T) {
	t.Run("writer", func(t *testing.T) {
		srv, s := testServer(t)
		agent := setupBrokerAgentInPhase(t, s, "redact-202w", state.PhaseRunning)
		seedDeletionWithError(t, s, agent.ID, seedLiveDeleting, "lease renew detail")
		body := func(isAdmin bool) map[string]json.RawMessage {
			rec := httptest.NewRecorder()
			srv.writeDeleteAccepted(rec, agent.ID, isAdmin)
			require.Equal(t, http.StatusAccepted, rec.Code)
			obj := decodeObject(t, rec.Body.Bytes())
			assert.JSONEq(t, `"`+agent.ID+`"`, string(obj["agentId"]))
			return rawDeletionObject(t, obj["deletion"])
		}
		adminView := body(true)
		require.NotNil(t, adminView)
		assert.Contains(t, adminView, "claim")
		assert.Contains(t, adminView, "error")
		assertGenericDeletionOf(t, adminView, body(false), "202 writer")
	})

	t.Run("slow delete", func(t *testing.T) {
		setDeleteKnob(t, &deleteSyncWait, 300*time.Millisecond)
		srv, s, _, disp := engineTestServer(t)
		entered, release := make(chan struct{}), make(chan struct{})
		disp.setFn(blockingDelete(entered, release, nil))
		var releaseOnce sync.Once
		releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(releaseAll)

		adminAgent := setupBrokerAgentInPhase(t, s, "redact-202-admin", state.PhaseRunning)
		res := waitDelete(t, deleteAsync(t, srv, "/api/v1/agents/"+adminAgent.ID, nil), 5*time.Second)
		require.Equal(t, http.StatusAccepted, res.rec.Code, res.rec.Body.String())
		adminView := rawDeletionObject(t, decodeObject(t, res.rec.Body.Bytes())["deletion"])
		require.NotNil(t, adminView)
		assert.Contains(t, adminView, "claim", "admin 202 carries the claim")

		userAgent := setupBrokerAgentInPhase(t, s, "redact-202-owner", state.PhaseRunning)
		req := ownerDeleteRequest(t, s, context.Background(), userAgent.ID)
		rec := httptest.NewRecorder()
		srv.performAgentDelete(rec, req, mustGetAgent(t, s, userAgent.ID))
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		userView := rawDeletionObject(t, decodeObject(t, rec.Body.Bytes())["deletion"])
		require.NotNil(t, userView)
		for _, k := range deletionDetailKeys {
			assert.NotContains(t, userView, k, "non-admin 202 has no %q", k)
		}
		assert.JSONEq(t, `"deleting"`, string(userView["state"]))
		assert.Contains(t, userView, "leaseExpiresAt")
		assert.Contains(t, userView, "startedAt")

		releaseAll()
		for _, id := range []string{adminAgent.ID, userAgent.ID} {
			require.Eventually(t, func() bool { return agentGone(t, s, id) }, 5*time.Second, 20*time.Millisecond)
		}
	})
}

// --- SSE ---

// TestPublishAgentStatus_DeletionGenericForEverySubscriber: the status
// event is published with an admin request context (as performAgentDelete
// does for an admin's delete) and is marshaled once and fanned out as
// bytes, so every subscriber, on
// the agent subject and on the project subject, receives the same generic
// view: no code, error or claim, with state and timestamps intact.
func TestPublishAgentStatus_DeletionGenericForEverySubscriber(t *testing.T) {
	pub := NewChannelEventPublisher()
	defer pub.Close()
	subs := map[string]<-chan Event{}
	for _, pattern := range []string{"agent.a1.status", "project.g1.agent.status", "project.*.agent.status", "project.>"} {
		ch, unsub := pub.Subscribe(pattern)
		defer unsub()
		subs[pattern] = ch
	}

	now := time.Now()
	failedAt := now.Add(-time.Minute)
	agent := &store.Agent{
		ID: "a1", ProjectID: "g1", Phase: "running",
		DeletionState: store.DeletionStateFailed, DeletionCode: store.DeletionCodeRuntimeError,
		DeletionError: "broker said: token /x invalid", DeletionClaim: 3,
		DeletionStartedAt: &failedAt, DeletionFailedAt: &failedAt, DeletionLeaseAt: &failedAt,
	}
	pub.PublishAgentStatus(adminPublishCtx(t), agent)
	adminView := deletionJSON(t, store.ComputeAgentDeletion(agent, now))

	var first string
	for pattern, ch := range subs {
		select {
		case evt := <-ch:
			if first == "" {
				first = string(evt.Data)
			}
			assert.Equal(t, first, string(evt.Data), "%s: every subscriber gets the same bytes", pattern)
			got := rawDeletionObject(t, decodeObject(t, evt.Data)["deletion"])
			assertGenericDeletionOf(t, adminView, got, "SSE "+pattern)
			assert.NotContains(t, string(evt.Data), "token /x invalid")
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: timed out waiting for event", pattern)
		}
	}
}

// TestSSEHandler_AgentStatusDeletionGenericForAdminSession: even an admin
// web session, receiving an event published with an admin request context,
// gets the generic view over SSE; admins read the detail
// fields from the REST agent.
func TestSSEHandler_AgentStatusDeletionGenericForAdminSession(t *testing.T) {
	ws := newDevAuthWebServer(t)
	pub := NewChannelEventPublisher()
	ws.SetEventPublisher(pub)
	ws.SetAuthzService(NewAuthzService(mockSuperAdminStore(DevUserID), nil))
	t.Cleanup(pub.Close)

	ts := httptest.NewServer(ws.Handler())
	defer ts.Close()
	// One deadline for the connect and the whole stream read below, so a
	// stalled stream fails the test instead of hanging it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events?sub=project.redact1.>", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	failedAt := time.Now().Add(-time.Minute)
	agent := &store.Agent{
		ID: tid("redact-sse"), ProjectID: "redact1", Phase: "running",
		DeletionState: store.DeletionStateFailed, DeletionCode: store.DeletionCodeConflict,
		DeletionError: "sse-secret-detail", DeletionClaim: 2,
		DeletionStartedAt: &failedAt, DeletionFailedAt: &failedAt,
	}
	publishCtx := adminPublishCtx(t)
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				pub.PublishAgentStatus(publishCtx, agent)
			case <-stop:
				return
			}
		}
	}()
	var frame string
	buf := make([]byte, 8192)
	deadline := time.Now().Add(5 * time.Second)
	// Read until a whole update frame is buffered: its event line, its
	// data line and the blank line that ends it.
	complete := func() bool {
		i := strings.Index(frame, "event: update")
		return i >= 0 && strings.Contains(frame[i:], "\n\n")
	}
	for !complete() {
		require.True(t, time.Now().Before(deadline), "timed out waiting for SSE event")
		n, err := resp.Body.Read(buf)
		require.NoError(t, err)
		frame += string(buf[:n])
	}
	close(stop)
	frame = frame[strings.Index(frame, "event: update"):]
	frame = frame[:strings.Index(frame, "\n\n")]
	assert.Contains(t, frame, `"deletion":{`)
	assert.Contains(t, frame, `"state":"failed"`)
	assert.NotContains(t, frame, "sse-secret-detail")
	assert.NotContains(t, frame, `"code":`)
	assert.NotContains(t, frame, `"claim":`)
}

// --- structural guard ---

// TestDeletionViewOnlyBuiltThroughRedaction parses every non-test Go file of
// pkg/hub and checks that store.ComputeAgentDeletion is called only from
// deletion_redact.go, so every response and event that carries a deletion
// view gets it through redactDeletionForCaller. It also checks that every
// deletionViewForCaller call outside the events publisher decides the
// caller with callerSeesDeletionDetail or a variable, never with the
// literal true.
func TestDeletionViewOnlyBuiltThroughRedaction(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var computeSites, viewSites []string
	eventSites := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err, name)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				if fn.Sel.Name == "ComputeAgentDeletion" {
					computeSites = append(computeSites, fset.Position(call.Pos()).String())
					assert.Equal(t, "deletion_redact.go", name, "store.ComputeAgentDeletion called outside the redaction helper at %s", fset.Position(call.Pos()))
				}
			case *ast.Ident:
				if fn.Name == "deletionViewForCaller" && len(call.Args) == 3 {
					viewSites = append(viewSites, fset.Position(call.Pos()).String())
					if lit, ok := call.Args[2].(*ast.Ident); ok && lit.Name == "true" {
						t.Errorf("deletionViewForCaller with a literal true at %s", fset.Position(call.Pos()))
					}
					lit, isIdent := call.Args[2].(*ast.Ident)
					isFalse := isIdent && lit.Name == "false"
					if isFalse {
						assert.Equal(t, "events.go", name, "only the event publisher uses the generic view unconditionally (%s)", fset.Position(call.Pos()))
					}
					// The status event is fanned out as one payload to
					// every subscriber, so it must never take the
					// publishing request's caller view.
					if name == "events.go" {
						eventSites++
						assert.True(t, isFalse, "deletionViewForCaller in events.go must pass the literal false (%s)", fset.Position(call.Pos()))
					}
				}
			}
			return true
		})
	}
	sort.Strings(viewSites)
	assert.Len(t, computeSites, 1, "only deletionViewForCaller computes the view: %v", computeSites)
	// enrichAgents, enrichAgent, three lifecycle responses (stop no-op,
	// stop not recorded, final), the managed response, the 202 writer and
	// the status event.
	assert.Len(t, viewSites, 8, "deletion view call sites: %v", viewSites)
	assert.Equal(t, 1, eventSites, "events.go builds the status event's deletion view once")
}
