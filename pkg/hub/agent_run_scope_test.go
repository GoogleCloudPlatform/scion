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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fakes -------------------------------------------------------------

// fakeAgentRunReader returns agent or err and counts reads.
type fakeAgentRunReader struct {
	agent *store.Agent
	err   error
	calls atomic.Int32
}

func (f *fakeAgentRunReader) GetAgent(context.Context, string) (*store.Agent, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	a := *f.agent
	return &a, nil
}

// fakeRunScopeMetrics records outcomes.
type fakeRunScopeMetrics struct {
	mu           sync.Mutex
	seen         []string
	routeClasses []string
}

func (m *fakeRunScopeMetrics) RecordAgentRunScope(source, outcome, mode, routeClass string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen = append(m.seen, source+"/"+outcome+"/"+mode)
	m.routeClasses = append(m.routeClasses, routeClass)
}

// capturedLog is one log record with its attributes.
type capturedLog struct {
	msg   string
	attrs map[string]any
}

// captureHandler records log messages and their attributes.
type captureHandler struct {
	mu   sync.Mutex
	recs []capturedLog
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

// outcomeName is outcome without the current run.
func (c *agentRunScopeChecker) outcomeName(ctx context.Context, claims *AgentTokenClaims, cs agentTokenCredentialState, header string) string {
	o, _ := c.outcome(ctx, claims, cs, header)
	return o
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

// --- setting -----------------------------------------------------------

// TestParseAgentRunScopeRejectsEnforce: configuration selects only off or
// observe; enforce and any other value are refused, and the setting has
// no exported field through which a mode could be set.
func TestParseAgentRunScopeRejectsEnforce(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		want    string
		wantErr bool
	}{
		{mode: "", want: "off"},
		{mode: "off", want: "off"},
		{mode: "observe", want: "observe"},
		{mode: "enforce", wantErr: true},
		{mode: "Enforce", wantErr: true},
		{mode: "ENFORCE", wantErr: true},
		{mode: " observe", wantErr: true},
		{mode: "on", wantErr: true},
		{mode: "true", wantErr: true},
		{mode: "2", wantErr: true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			got, err := ParseAgentRunScope(tc.mode, "")
			if tc.wantErr {
				require.Error(t, err)
				assert.Equal(t, AgentRunScope{}, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.String())
		})
	}

	_, err := ParseAgentRunScope("observe", "2026-13-01")
	require.Error(t, err)

	typ := reflect.TypeOf(AgentRunScope{})
	for i := 0; i < typ.NumField(); i++ {
		assert.False(t, typ.Field(i).IsExported(), "AgentRunScope.%s is exported", typ.Field(i).Name)
	}
}

// TestAgentRunScopeOffReadsNothing: with the mode off there is no checker,
// so nothing reads the token's run or the agent row, and a server built
// with the default configuration has none.
func TestAgentRunScopeOffReadsNothing(t *testing.T) {
	reader := &fakeAgentRunReader{agent: &store.Agent{}}
	off, err := ParseAgentRunScope("off", "2020-01-01T00:00:00Z")
	require.NoError(t, err)
	assert.Nil(t, newAgentRunScopeChecker(off, reader, nil))
	assert.Nil(t, newAgentRunScopeChecker(AgentRunScope{}, reader, nil))
	assert.Zero(t, reader.calls.Load())

	srv, s := testServer(t)
	assert.Nil(t, srv.authConfig.AgentRunScope)

	// A token for a run the agent is no longer on is accepted.
	srv, s, _, project := setupCredentialTestServerOn(t, srv, s)
	agent := runScopeAgent(t, s, project.ID, "off-reads-nothing", "run-current")
	tok := signRunToken(t, srv, s, agent, "run-old", recordRun("run-old"))
	rec := agentRequest(t, srv.Handler(), http.MethodGet, "/api/v1/agents/"+agent.ID, tok, "")
	assert.NotEqual(t, http.StatusUnauthorized, rec.Code, "body: %s", rec.Body.String())
}

// --- outcome classification ----------------------------------------------

// TestAgentRunScopeNoBindingBranches: a token's run counts only when its
// credential row records the same run. Every branch where the row does not
// back the claim is unbound and decided without reading the agent row; a
// token without a run is accepted only until legacy_until.
func TestAgentRunScopeNoBindingBranches(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	cred := func(run string) agentTokenCredentialState {
		return agentTokenCredentialState{evaluated: true, cred: &store.AgentCredential{RunID: run}}
	}
	noRow := agentTokenCredentialState{evaluated: true}
	notEvaluated := agentTokenCredentialState{}

	for _, tc := range []struct {
		name        string
		claim       string
		cs          agentTokenCredentialState
		legacyUntil time.Time
		agentRun    string
		header      string
		agentErr    error
		want        string
		wantReads   int32
	}{
		// No binding: the claim is not backed by the credential row.
		{name: "claim without a credential row", claim: "run-a", cs: noRow, agentRun: "run-a", want: runScopeOutcomeUnbound},
		{name: "claim without a credential evaluation", claim: "run-a", cs: notEvaluated, agentRun: "run-a", want: runScopeOutcomeUnbound},
		{name: "claim differs from the credential row", claim: "run-a", cs: cred("run-b"), agentRun: "run-a", want: runScopeOutcomeUnbound},
		{name: "claim against a row without a run", claim: "run-a", cs: cred(""), agentRun: "run-a", want: runScopeOutcomeUnbound},
		{name: "no claim against a row with a run", claim: "", cs: cred("run-a"), agentRun: "run-a", want: runScopeOutcomeUnbound},
		// No claim: accepted until legacy_until.
		{name: "no claim before legacy_until", claim: "", cs: cred(""), legacyUntil: future, agentRun: "run-a", want: runScopeOutcomeUnscoped},
		{name: "no claim without a row before legacy_until", claim: "", cs: noRow, legacyUntil: future, agentRun: "run-a", want: runScopeOutcomeUnscoped},
		{name: "no claim without legacy_until", claim: "", cs: cred(""), agentRun: "run-a", want: runScopeOutcomeUnscoped},
		{name: "no claim after legacy_until", claim: "", cs: cred(""), legacyUntil: past, agentRun: "run-a", want: runScopeOutcomeLegacyEnded},
		{name: "no claim without a row after legacy_until", claim: "", cs: noRow, legacyUntil: past, agentRun: "run-a", want: runScopeOutcomeLegacyEnded},
		// No claim with a header: the header is compared with the agent's
		// current run (one read); it can only refuse.
		{name: "no claim, header names the current run", claim: "", cs: cred(""), legacyUntil: future, agentRun: "run-a", header: "run-a", want: runScopeOutcomeUnscoped, wantReads: 1},
		{name: "no claim, header names another run", claim: "", cs: cred(""), legacyUntil: future, agentRun: "run-a", header: "run-b", want: runScopeOutcomeHeader, wantReads: 1},
		{name: "no claim without a row, header names another run", claim: "", cs: noRow, legacyUntil: future, agentRun: "run-a", header: "run-b", want: runScopeOutcomeHeader, wantReads: 1},
		{name: "no claim, header, agent gone", claim: "", cs: cred(""), legacyUntil: future, agentErr: store.ErrNotFound, header: "run-b", want: runScopeOutcomeUnscoped, wantReads: 1},
		{name: "no claim, header, agent read fails", claim: "", cs: cred(""), legacyUntil: future, agentErr: errors.New("store unavailable"), header: "run-b", want: runScopeOutcomeUnavailable, wantReads: 1},
		{name: "no claim after legacy_until, header", claim: "", cs: cred(""), legacyUntil: past, agentRun: "run-a", header: "run-a", want: runScopeOutcomeLegacyEnded},
		// With a claim, the header is compared with the claim, before any read.
		{name: "claim, header names another run", claim: "run-a", cs: cred("run-a"), agentRun: "run-a", header: "run-b", want: runScopeOutcomeHeader},
		// Bound claims are compared with the agent's current run.
		{name: "current run", claim: "run-a", cs: cred("run-a"), agentRun: "run-a", want: runScopeOutcomeBound, wantReads: 1},
		{name: "superseded run", claim: "run-a", cs: cred("run-a"), agentRun: "run-b", want: runScopeOutcomeSuperseded, wantReads: 1},
		{name: "agent row without a run", claim: "run-a", cs: cred("run-a"), agentRun: "", want: runScopeOutcomeSuperseded, wantReads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &fakeAgentRunReader{agent: &store.Agent{ID: "agent-1", RunID: tc.agentRun}, err: tc.agentErr}
			c := testRunScopeChecker(agentRunScopeObserve, tc.legacyUntil, reader)
			claims := &AgentTokenClaims{RunID: tc.claim}
			claims.Subject = "agent-1"
			assert.Equal(t, tc.want, c.outcomeName(context.Background(), claims, tc.cs, tc.header))
			assert.Equal(t, tc.wantReads, reader.calls.Load(), "agent reads")
		})
	}

	t.Run("agent gone", func(t *testing.T) {
		claims := &AgentTokenClaims{RunID: "run-a"}
		claims.Subject = "agent-1"
		gone := testRunScopeChecker(agentRunScopeObserve, time.Time{}, &fakeAgentRunReader{err: store.ErrNotFound})
		assert.Equal(t, runScopeOutcomeAgentGone, gone.outcomeName(context.Background(), claims, cred("run-a"), ""))
		deleted := testRunScopeChecker(agentRunScopeObserve, time.Time{}, &fakeAgentRunReader{agent: &store.Agent{RunID: "run-a", DeletedAt: time.Now()}})
		assert.Equal(t, runScopeOutcomeAgentGone, deleted.outcomeName(context.Background(), claims, cred("run-a"), ""))
	})
}

// TestAgentRunScopeVerdicts: observe allows every outcome; enforce allows
// only a bound token or a token without a run before legacy_until.
func TestAgentRunScopeVerdicts(t *testing.T) {
	for _, tc := range []struct {
		outcome string
		enforce runScopeVerdict
	}{
		{runScopeOutcomeBound, runScopeAllow},
		{runScopeOutcomeUnscoped, runScopeAllow},
		{runScopeOutcomeUnbound, runScopeDeny},
		{runScopeOutcomeLegacyEnded, runScopeDeny},
		{runScopeOutcomeHeader, runScopeDeny},
		{runScopeOutcomeSuperseded, runScopeDeny},
		{runScopeOutcomeAgentGone, runScopeDeny},
		{runScopeOutcomeUnavailable, runScopeUnavailable},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			claims, cs, reader, header, legacyUntil := runScopeInputsFor(tc.outcome)
			observe := testRunScopeChecker(agentRunScopeObserve, legacyUntil, reader)
			require.Equal(t, tc.outcome, observe.outcomeName(context.Background(), claims, cs, header))
			assert.Equal(t, runScopeAllow, observe.check(context.Background(), claims, cs, runScopeRequest{header: header}, runScopeSourceHTTP), "observe")
			assert.Equal(t, tc.enforce, testRunScopeChecker(agentRunScopeEnforce, legacyUntil, reader).
				check(context.Background(), claims, cs, runScopeRequest{header: header}, runScopeSourceHTTP), "enforce")
		})
	}
}

// runScopeInputsFor returns check inputs and a legacy_until that produce
// outcome.
func runScopeInputsFor(outcome string) (*AgentTokenClaims, agentTokenCredentialState, *fakeAgentRunReader, string, time.Time) {
	claims := &AgentTokenClaims{RunID: "run-a"}
	claims.Subject = "agent-1"
	cs := agentTokenCredentialState{evaluated: true, cred: &store.AgentCredential{RunID: "run-a"}}
	reader := &fakeAgentRunReader{agent: &store.Agent{ID: "agent-1", RunID: "run-a"}}
	header := ""
	legacyUntil := time.Now().Add(-time.Hour)
	switch outcome {
	case runScopeOutcomeUnscoped:
		claims.RunID, cs.cred.RunID = "", ""
		legacyUntil = time.Now().Add(time.Hour)
	case runScopeOutcomeUnbound:
		cs.cred.RunID = "run-b"
	case runScopeOutcomeLegacyEnded:
		claims.RunID, cs.cred.RunID = "", ""
	case runScopeOutcomeHeader:
		header = "run-b"
	case runScopeOutcomeSuperseded:
		reader.agent.RunID = "run-b"
	case runScopeOutcomeAgentGone:
		reader.err = store.ErrNotFound
	case runScopeOutcomeUnavailable:
		reader.err = errors.New("store unavailable")
	}
	return claims, cs, reader, header, legacyUntil
}

// TestAgentRunScopeStoreErrorObserveAllowsEnforceUnavailable: when the
// agent row cannot be read, observe allows the request and enforce
// answers 503, through the middleware.
func TestAgentRunScopeStoreErrorObserveAllowsEnforceUnavailable(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)
	agent := runScopeAgent(t, s, project.ID, "store-error", "run-current")
	tok := signRunToken(t, srv, s, agent, "run-current", recordRun("run-current"))
	failing := &fakeAgentRunReader{err: errors.New("agent store unavailable for testing")}

	for _, tc := range []struct {
		mode       agentRunScopeMode
		wantStatus int
	}{
		{mode: agentRunScopeObserve},
		{mode: agentRunScopeEnforce, wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(tc.mode.String(), func(t *testing.T) {
			srv.authConfig.AgentRunScope = testRunScopeChecker(tc.mode, time.Time{}, failing)
			rec := agentRequest(t, srv.Handler(), http.MethodGet, "/api/v1/agents/"+agent.ID, tok, "")
			if tc.wantStatus == 0 {
				assert.NotEqual(t, http.StatusUnauthorized, rec.Code, "body: %s", rec.Body.String())
				assert.NotEqual(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
				return
			}
			require.Equal(t, tc.wantStatus, rec.Code, "body: %s", rec.Body.String())
			assert.Equal(t, ErrCodeUnavailable, decodeAPIError(t, rec.Body.Bytes()).Error.Code)
			assert.NotContains(t, rec.Body.String(), "agent store unavailable for testing")
		})
	}
}

// --- end to end ----------------------------------------------------------

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

// credRecord decides the credential row stored for a signed token: nil
// stores none, otherwise the row records the returned run.
type credRecord func() *string

func recordRun(run string) credRecord { return func() *string { return &run } }

var recordNothing credRecord = func() *string { return nil }

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

// TestAgentTokenRefusedIsByteIdentical: in enforce mode every refusal
// cause produces the same response, status, headers and body byte for
// byte, on an ordinary agent route and on token refresh, and no response
// names a run.
func TestAgentTokenRefusedIsByteIdentical(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)
	ctx := context.Background()
	srv.authConfig.AgentRunScope = testRunScopeChecker(agentRunScopeEnforce, time.Now().Add(-time.Hour), s)
	h := srv.Handler()

	type cause struct {
		name   string
		agent  *store.Agent
		token  string
		header string
	}
	var causes []cause
	// add stores an agent and a token for it. In tokRun, credRun and
	// header, "CURRENT" stands for the agent's current run; credRun
	// "NONE" stores no credential row.
	add := func(name, tokRun, credRun, header string) {
		agent := runScopeAgent(t, s, project.ID, name, "run-current-"+name)
		cur := func(v string) string { return strings.ReplaceAll(v, "CURRENT", agent.RunID) }
		record := recordNothing
		if credRun != "NONE" {
			record = recordRun(cur(credRun))
		}
		causes = append(causes, cause{name: name, agent: agent, token: signRunToken(t, srv, s, agent, cur(tokRun), record), header: cur(header)})
	}
	add("claim-without-row", "CURRENT", "NONE", "")
	add("claim-differs-from-row", "CURRENT", "run-elsewhere", "")
	add("claim-against-empty-row", "CURRENT", "", "")
	add("no-claim-against-row-run", "", "run-elsewhere", "")
	add("legacy-ended", "", "", "")
	add("legacy-ended-without-row", "", "NONE", "")
	add("legacy-ended-header-current", "", "", "CURRENT")
	add("superseded", "run-old", "run-old", "")
	add("superseded-header-current", "run-old", "run-old", "CURRENT")
	add("header-mismatch", "CURRENT", "CURRENT", "run-elsewhere")
	gone := runScopeAgent(t, s, project.ID, "agent-gone", "run-gone")
	goneTok := signRunToken(t, srv, s, gone, "run-gone", recordRun("run-gone"))
	require.NoError(t, s.DeleteAgent(ctx, gone.ID))
	causes = append(causes, cause{name: "agent-gone", agent: gone, token: goneTok})

	want := httptest.NewRecorder()
	writeAgentTokenRefused(want)
	wantRaw := rawOf(want)
	require.Equal(t, http.StatusUnauthorized, wantRaw.Status)
	require.Equal(t, agentTokenRefusedBody, wantRaw.Body)

	for _, c := range causes {
		for _, route := range []struct{ method, path string }{
			{http.MethodGet, "/api/v1/agents/" + c.agent.ID},
			{http.MethodPost, "/api/v1/agents/" + c.agent.ID + "/token/refresh"},
		} {
			t.Run(c.name+" "+route.method, func(t *testing.T) {
				got := rawOf(agentRequest(t, h, route.method, route.path, c.token, c.header))
				assert.Equal(t, wantRaw, got)
				assert.NotContains(t, got.Body, "run-")
			})
		}
	}

	t.Run("bound token is served", func(t *testing.T) {
		agent := runScopeAgent(t, s, project.ID, "bound", "run-bound")
		tok := signRunToken(t, srv, s, agent, "run-bound", recordRun("run-bound"))
		for _, header := range []string{"", "run-bound"} {
			rec := agentRequest(t, h, http.MethodGet, "/api/v1/agents/"+agent.ID, tok, header)
			assert.NotEqual(t, http.StatusUnauthorized, rec.Code, "body: %s", rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "run-bound")
		}
	})
}

// TestAgentRunHeaderDoesNotElevate: AgentRunIDHeader can only cause a
// refusal. Naming the current run does not admit a token without a run
// after legacy_until or a token for an earlier run, a refresh keeps the
// presented token's run whatever the header says, and the header is never
// echoed.
func TestAgentRunHeaderDoesNotElevate(t *testing.T) {
	ctx := context.Background()
	claims := func(run string) *AgentTokenClaims {
		c := &AgentTokenClaims{RunID: run}
		c.Subject = "agent-1"
		return c
	}
	cred := func(run string) agentTokenCredentialState {
		return agentTokenCredentialState{evaluated: true, cred: &store.AgentCredential{RunID: run}}
	}
	reader := &fakeAgentRunReader{agent: &store.Agent{ID: "agent-1", RunID: "run-current"}}

	t.Run("checker", func(t *testing.T) {
		enforce := testRunScopeChecker(agentRunScopeEnforce, time.Now().Add(-time.Hour), reader)
		assert.Equal(t, runScopeDeny, enforce.check(ctx, claims(""), cred(""), runScopeRequest{header: "run-current"}, runScopeSourceHTTP), "no run after legacy_until")
		assert.Equal(t, runScopeDeny, enforce.check(ctx, claims("run-old"), cred("run-old"), runScopeRequest{header: "run-current"}, runScopeSourceHTTP), "earlier run")
		assert.Equal(t, runScopeDeny, enforce.check(ctx, claims("run-current"), cred("run-current"), runScopeRequest{header: "run-old"}, runScopeSourceHTTP), "header differs")
		assert.Equal(t, runScopeAllow, enforce.check(ctx, claims("run-current"), cred("run-current"), runScopeRequest{header: "run-current"}, runScopeSourceHTTP), "header matches")

		lenient := testRunScopeChecker(agentRunScopeEnforce, time.Now().Add(time.Hour), reader)
		assert.Equal(t, runScopeOutcomeUnscoped, lenient.outcomeName(ctx, claims(""), cred(""), "run-current"), "a token without a run stays unscoped")
		assert.Equal(t, runScopeDeny, lenient.check(ctx, claims(""), cred(""), runScopeRequest{header: "run-old"}, runScopeSourceHTTP), "no run, header names another run")
		assert.Equal(t, runScopeAllow, lenient.check(ctx, claims(""), cred(""), runScopeRequest{header: "run-current"}, runScopeSourceHTTP), "no run, header names the current run")
	})

	t.Run("refresh keeps the presented run", func(t *testing.T) {
		srv, s, _, project := setupCredentialTestServer(t)
		srv.authConfig.AgentRunScope = testRunScopeChecker(agentRunScopeObserve, time.Time{}, s)
		h := srv.Handler()
		for _, tc := range []struct {
			name  string
			claim string
		}{
			{name: "no run", claim: ""},
			{name: "earlier run", claim: "run-old"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				agent := runScopeAgent(t, s, project.ID, "elevate-"+tc.name, "run-current-elevate")
				tok := signRunToken(t, srv, s, agent, tc.claim, recordRun(tc.claim))
				rec := agentRequest(t, h, http.MethodPost, "/api/v1/agents/"+agent.ID+"/token/refresh", tok, agent.RunID)
				require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
				assert.Empty(t, rec.Header().Get(AgentRunIDHeader), "header echoed")
				assert.NotContains(t, rec.Body.String(), "run-", "a run in the body")

				var resp struct {
					Token string `json:"token"`
				}
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				newClaims, err := srv.agentTokenService.ValidateAgentToken(resp.Token)
				require.NoError(t, err)
				assert.Equal(t, tc.claim, newClaims.RunID)
				newCred, err := s.GetAgentCredentialByJTIHash(ctx, hashJTI(newClaims.ID))
				require.NoError(t, err)
				assert.Equal(t, tc.claim, newCred.RunID)
			})
		}
	})
}

// failingCredentialCreateStore fails CreateAgentCredential while armed.
type failingCredentialCreateStore struct {
	store.Store
	fault *storeFaultSwitch
}

var errCredentialCreateForTest = errors.New("credential insert refused for testing")

func (f *failingCredentialCreateStore) CreateAgentCredential(ctx context.Context, cred *store.AgentCredential) error {
	if f.fault.Active() {
		return errCredentialCreateForTest
	}
	return f.Store.CreateAgentCredential(ctx, cred)
}

// TestAgentTokenRefreshRecordFailureIsGeneric500: when the refreshed
// token's credential cannot be recorded, refresh answers the same generic
// 500 in every mode, without the cause.
func TestAgentTokenRefreshRecordFailureIsGeneric500(t *testing.T) {
	want := httptest.NewRecorder()
	writeError(want, http.StatusInternalServerError, ErrCodeInternalError, "failed to generate refreshed token", nil)

	for _, mode := range []agentRunScopeMode{agentRunScopeOff, agentRunScopeObserve, agentRunScopeEnforce} {
		t.Run(mode.String(), func(t *testing.T) {
			srv, s, _, fault := testServerWithStoreFault(t, func(inner store.Store, fault *storeFaultSwitch) *failingCredentialCreateStore {
				return &failingCredentialCreateStore{Store: inner, fault: fault}
			})
			srv, s, _, project := setupCredentialTestServerOn(t, srv, s)
			if mode != agentRunScopeOff {
				srv.authConfig.AgentRunScope = testRunScopeChecker(mode, time.Time{}, s)
			}
			agent := runScopeAgent(t, s, project.ID, "refresh-500-"+mode.String(), "run-refresh")
			tok, err := srv.issueAgentTokenForTest(context.Background(), agent)
			require.NoError(t, err)
			fault.Arm()

			rec := agentRequest(t, srv.Handler(), http.MethodPost, "/api/v1/agents/"+agent.ID+"/token/refresh", tok, "")
			assert.Equal(t, rawOf(want), rawOf(rec))
			assert.NotContains(t, rec.Body.String(), errCredentialCreateForTest.Error())
		})
	}
}

// --- conduit ---------------------------------------------------------------

// TestConduitTokenRunClose4401: a conduit session whose Hello names a run
// other than its token's is closed 4401 in enforce mode, before the
// incarnation check; observe leaves the existing 4409; a token without a
// run carries no binding.
func TestConduitTokenRunClose4401(t *testing.T) {
	t.Run("binding", func(t *testing.T) {
		c := testRunScopeChecker(agentRunScopeEnforce, time.Time{}, &fakeAgentRunReader{agent: &store.Agent{}})
		claims := &AgentTokenClaims{RunID: "run-a"}
		b := c.conduitBinding(context.Background(), claims, runScopeRequest{})
		require.NotNil(t, b)
		assert.Equal(t, "run-a", b.RunID)
		assert.True(t, b.Enforce)
		assert.Nil(t, c.conduitBinding(context.Background(), &AgentTokenClaims{}, runScopeRequest{}))

		o := testRunScopeChecker(agentRunScopeObserve, time.Time{}, &fakeAgentRunReader{agent: &store.Agent{}})
		assert.False(t, o.conduitBinding(context.Background(), claims, runScopeRequest{}).Enforce)
	})

	for _, tc := range []struct {
		name      string
		mode      agentRunScopeMode
		presented string // "current" is the token's (and agent's) run
		wantCode  uint32 // 0: admitted
	}{
		{name: "enforce, Hello names another run", mode: agentRunScopeEnforce, presented: "other-run", wantCode: conduit.CloseUnauthenticated},
		{name: "enforce, Hello names the token's run", mode: agentRunScopeEnforce, presented: "current"},
		{name: "observe, Hello names another run", mode: agentRunScopeObserve, presented: "other-run", wantCode: relay.CloseSupersededIncarnation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelayFixture(t, nil)
			f.srv.authConfig.AgentRunScope = testRunScopeChecker(tc.mode, time.Time{}, f.store)
			f.public.Close()
			f.public = httptest.NewServer(f.srv.Handler())
			t.Cleanup(f.public.Close)

			tok := signRunToken(t, f.srv, f.store, f.launched, f.launched.RunID, recordRun(f.launched.RunID))
			presented := tc.presented
			if presented == "current" {
				presented = f.launched.RunID
			}
			_, _, err := f.dial(t, tok, presented)
			if tc.wantCode == 0 {
				require.NoError(t, err)
				return
			}
			var ce *conduit.CloseError
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, tc.wantCode, ce.Code)
		})
	}
}

// --- logging -----------------------------------------------------------------

// TestAgentRunScopeLogDedup: a refusable outcome is logged with the
// request's ids (never the token or its scopes) at most once per agent,
// token run and outcome per window; every outcome is counted, with its
// route class, and the counts are summarised once a minute; the dedup set
// stays bounded.
func TestAgentRunScopeLogDedup(t *testing.T) {
	newChecker := func(t *testing.T) (*agentRunScopeChecker, *captureHandler, *fakeRunScopeMetrics, *time.Time) {
		logs := &captureHandler{}
		metrics := &fakeRunScopeMetrics{}
		reader := &fakeAgentRunReader{agent: &store.Agent{ID: "agent-1", RunID: "run-current"}}
		c := newAgentRunScopeChecker(AgentRunScope{mode: agentRunScopeObserve}, reader, slog.New(logs))
		var m agentRunScopeMetrics = metrics
		c.metrics.Store(&m)
		now := time.Unix(1_800_000_000, 0)
		c.now = func() time.Time { return now }
		return c, logs, metrics, &now
	}
	tokenFor := func(run string) (*AgentTokenClaims, agentTokenCredentialState) {
		claims := &AgentTokenClaims{RunID: run, ProjectID: "project-1",
			Scopes: []AgentTokenScope{ScopeAgentStatusUpdate}}
		claims.Subject = "agent-1"
		claims.ID = "jti-" + run
		return claims, agentTokenCredentialState{evaluated: true, cred: &store.AgentCredential{RunID: run}}
	}
	req := runScopeRequest{header: "", method: http.MethodPost, path: "/api/v1/agents/agent-1/status",
		route: "/api/v1/agents/", remoteAddr: "192.0.2.10:4000"}

	t.Run("fields", func(t *testing.T) {
		c, logs, metrics, _ := newChecker(t)
		claims, cs := tokenFor("run-old")
		c.check(context.Background(), claims, cs, req, runScopeSourceHTTP)

		recs := logs.records("agent_token_run_superseded")
		require.Len(t, recs, 1)
		assert.Equal(t, map[string]any{
			"agent_id":       "agent-1",
			"project_id":     "project-1",
			"outcome":        runScopeOutcomeSuperseded,
			"source":         runScopeSourceHTTP,
			"mode":           "observe",
			"token_run_id":   "run-old",
			"current_run_id": "run-current",
			"header_run_id":  "",
			"route":          "/api/v1/agents/",
			"route_class":    runScopeRouteAgentStatus,
			"path":           "/api/v1/agents/agent-1/status",
			"method":         http.MethodPost,
			"jti_hash":       hashJTI("jti-run-old")[:8],
			"remote_addr":    "192.0.2.10:4000",
		}, recs[0].attrs)
		for _, v := range recs[0].attrs {
			assert.NotContains(t, fmt.Sprint(v), string(ScopeAgentStatusUpdate), "scopes logged")
			assert.NotContains(t, fmt.Sprint(v), "jti-run-old", "raw jti logged")
		}
		assert.Equal(t, []string{"http/superseded/observe"}, metrics.seen)
		assert.Equal(t, []string{runScopeRouteAgentStatus}, metrics.routeClasses)
	})

	t.Run("dedup per token run", func(t *testing.T) {
		c, logs, metrics, now := newChecker(t)
		older, olderCS := tokenFor("run-older")
		old, oldCS := tokenFor("run-old")
		check := func(claims *AgentTokenClaims, cs agentTokenCredentialState) {
			c.check(context.Background(), claims, cs, req, runScopeSourceHTTP)
		}
		superseded := func() []string {
			var runs []string
			for _, r := range logs.records("agent_token_run_superseded") {
				runs = append(runs, r.attrs["token_run_id"].(string))
			}
			return runs
		}

		check(older, olderCS)
		check(old, oldCS) // a second run of the same agent is logged too
		check(old, oldCS)
		assert.Equal(t, []string{"run-older", "run-old"}, superseded())
		*now = now.Add(runScopeLogDedupWindow - time.Second)
		check(old, oldCS)
		assert.Equal(t, []string{"run-older", "run-old"}, superseded())
		*now = now.Add(time.Second)
		check(old, oldCS)
		assert.Equal(t, []string{"run-older", "run-old", "run-old"}, superseded())
		assert.Len(t, metrics.seen, 5, "every outcome counted")
	})

	t.Run("summary per minute", func(t *testing.T) {
		c, logs, _, now := newChecker(t)
		old, oldCS := tokenFor("run-old")
		unscoped, unscopedCS := tokenFor("")
		for i := 0; i < 3; i++ {
			c.check(context.Background(), old, oldCS, req, runScopeSourceHTTP)
		}
		c.check(context.Background(), unscoped, unscopedCS, req, runScopeSourceHTTP)
		bound, boundCS := tokenFor("run-current")
		c.check(context.Background(), bound, boundCS, req, runScopeSourceHTTP) // not counted
		assert.Empty(t, logs.records("agent_token_run_scope_summary"), "window still open")

		*now = now.Add(runScopeLogSummaryWindow)
		c.check(context.Background(), old, oldCS, req, runScopeSourceHTTP)
		recs := logs.records("agent_token_run_scope_summary")
		require.Len(t, recs, 1)
		assert.Equal(t, map[string]any{
			"mode":                    "observe",
			"window_start":            time.Unix(1_800_000_000, 0).UTC().Format(time.RFC3339),
			"window_seconds":          int64(60),
			"count":                   int64(4),
			runScopeOutcomeSuperseded: int64(3),
			runScopeOutcomeUnscoped:   int64(1),
		}, recs[0].attrs)
	})

	t.Run("bounded", func(t *testing.T) {
		d := newRunScopeLogDedup(3, time.Minute)
		t0 := time.Unix(1_800_000_000, 0)
		for i := 0; i < 10; i++ {
			assert.True(t, d.first(uuid.NewString(), t0))
			assert.LessOrEqual(t, len(d.seen), 3)
		}
		assert.True(t, d.first("k", t0))
		assert.False(t, d.first("k", t0.Add(59*time.Second)))
		assert.True(t, d.first("k", t0.Add(61*time.Second)))
	})
}
