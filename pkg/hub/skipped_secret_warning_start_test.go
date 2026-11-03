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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Hub-level tests for the skipped-secret warning (PR-1 validation spec): a
// secret the store cannot return is skipped, the start or restart goes on,
// and the requester's response carries one fixed-format warning naming the
// key and its scope kind, while the server logs one WARN line. Neither ever
// carries the skipped item's value, its ref (stored or resource-path form),
// its scope ID or the backend's error text.

// Sentinels carried by the failing item A. None may appear in a response
// body or in any captured log record.
const (
	swValueSentinel   = "SWVALUE-c41f-sentinel"
	swRefStored       = "gcpsm:projects/swref-proj-sentinel/secrets/SWREF-STORED-sentinel"
	swRefPath         = "projects/swref-proj-sentinel/secrets/SWREF-PATH-sentinel"
	swScopeIDSentinel = "SWSCOPEID-9b2d-sentinel"
	swErrSentinel     = "SWERR-backend-text-sentinel"
)

var swSentinels = []string{swValueSentinel, swRefStored, swRefPath, swScopeIDSentinel, swErrSentinel}

const (
	swItemA       = "SW_API_KEY"
	swItemB       = "SW_OK"
	swItemBValue  = "sw-ok-value"
	swItemUser    = "SW_DB_PASSWORD"
	swItemHub     = "SW_HUB_TOKEN"
	swItemBroker  = "SW_BROKER_TOKEN"
	swSkipLogMsg  = "launch material skipped: secret store read failed"
	swDeniedSumUp = "launch material withheld: items not authorized for delivery"
)

// swAccessErr is a Secret Manager access error whose text carries the
// resource-path ref and the backend sentinel, as a real gRPC error would.
func swAccessErr() error {
	return fmt.Errorf("%w: reading %s/versions/latest: rpc error: %s", secret.ErrValueAccess, swRefPath, swErrSentinel)
}

// swDrop makes swFaultBackend return no result for an item (class missing).
var swDrop = errors.New("sw: drop the result")

// swFaultBackend wraps a real backend. It lists every item as stored, but
// gives each item named in sentinel the sentinel ref and scope ID in its
// metadata (selection authorizes by the listed scope, not the metadata's
// scope ID). Each item named in faults gets that error from FetchValues
// instead of its value (swDrop: no result at all). An item it passes through
// to the real backend is fetched with its original metadata, as listed by
// the real backend. It records every name FetchValues was asked for.
type swFaultBackend struct {
	secret.SecretBackend
	faults   map[string]error
	sentinel map[string]bool

	mu       sync.Mutex
	fetched  []string
	original map[string]secret.SecretMeta // by ID, for the rewritten items
}

func (b *swFaultBackend) List(ctx context.Context, filter secret.Filter) ([]secret.SecretMeta, error) {
	metas, err := b.SecretBackend.List(ctx, filter)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range metas {
		if b.sentinel[metas[i].Name] {
			if b.original == nil {
				b.original = map[string]secret.SecretMeta{}
			}
			b.original[metas[i].ID] = metas[i]
			metas[i].SecretRef = swRefStored
			metas[i].ScopeID = swScopeIDSentinel
		}
	}
	return metas, nil
}

func (b *swFaultBackend) FetchValues(ctx context.Context, metas []secret.SecretMeta) (map[string]secret.FetchResult, error) {
	var pass []secret.SecretMeta
	faulted := map[string]error{}
	b.mu.Lock()
	for _, m := range metas {
		b.fetched = append(b.fetched, m.Name)
		if err, ok := b.faults[m.Name]; ok {
			faulted[m.ID] = err
			continue
		}
		if orig, ok := b.original[m.ID]; ok {
			m = orig
		}
		pass = append(pass, m)
	}
	b.mu.Unlock()
	out := map[string]secret.FetchResult{}
	if len(pass) > 0 {
		got, err := b.SecretBackend.FetchValues(ctx, pass)
		if err != nil {
			return nil, err
		}
		for id, r := range got {
			out[id] = r
		}
	}
	for id, err := range faulted {
		if errors.Is(err, swDrop) {
			continue
		}
		out[id] = secret.FetchResult{Err: err}
	}
	return out, nil
}

func (b *swFaultBackend) fetchedNames() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.fetched...)
}

// swLog is a JSON log sink safe for concurrent writers: a dispatcher, the
// server's loggers and slog.Default all write to one, so every record and
// attribute at every level is captured.
type swLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *swLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *swLog) bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte(nil), l.buf.Bytes()...)
}

func (l *swLog) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// skipRecords returns the skipped-secret WARN records for agentID.
func (l *swLog) skipRecords(t *testing.T, agentID string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range warnRecords(t, l.bytes(), swSkipLogMsg) {
		if r["agent_id"] == agentID {
			out = append(out, r)
		}
	}
	return out
}

// installSWDefaultLog points slog.Default at a capturing sink for the test,
// so loggers built from it afterwards (the server's) are captured too.
func installSWDefaultLog(t *testing.T) *swLog {
	t.Helper()
	l := &swLog{}
	prev := slog.Default()
	slog.SetDefault(l.logger())
	t.Cleanup(func() { slog.SetDefault(prev) })
	return l
}

// assertNoSWLeak checks that no sentinel of item A appears in the raw
// response body or in any captured log output, and that A's name and
// scope kind do appear in the body.
func assertNoSWLeak(t *testing.T, body []byte, logs ...*swLog) {
	t.Helper()
	for _, s := range swSentinels {
		assert.NotContains(t, string(body), s, "sentinel in the response body")
		for i, l := range logs {
			assert.NotContains(t, string(l.bytes()), s, "sentinel in captured log %d", i)
		}
	}
	assert.Contains(t, string(body), swItemA+" (project)", "the skipped key and its scope kind are named")
}

// swFixture is an ldFixture (golden authorization, delivery-eligible agent
// owned by the project owner) behind a test server, with a stopped agent,
// a failing backend and captured logs.
type swFixture struct {
	*ldFixture
	srv   *Server
	fb    *swFaultBackend
	logs  *swLog // slog.Default, the server's loggers, the dispatcher and authz
	owner *store.User
	admin *store.User // project admin: may start another user's agent
}

type swOptions struct {
	ld ldOptions
	// items stores more items (name -> scope) with the value v-<name>.
	items map[string]string
	// faults overrides the default fault set {A: access error}.
	faults map[string]error
}

func newSWFixture(t *testing.T, name string, opts swOptions) *swFixture {
	t.Helper()
	ctx := context.Background()
	logs := installSWDefaultLog(t)
	m := newLDFixture(t, name, opts.ld)
	logger := logs.logger()
	m.d.log = logger
	m.authz.logger = logger

	// The start handler places the agent on its project's broker.
	require.NoError(t, m.store.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: m.agent.ProjectID, BrokerID: m.broker.ID, BrokerName: m.broker.Name, Status: m.broker.Status,
	}))
	m.agent.Phase = string(state.PhaseStopped)
	require.NoError(t, m.store.UpdateAgent(ctx, m.agent))

	_, metaA, err := m.backend.Set(ctx, &secret.SetSecretInput{
		Name: swItemA, Value: swValueSentinel, SecretType: store.SecretTypeEnvironment, Target: swItemA,
		Scope: store.ScopeProject, ScopeID: m.agent.ProjectID, InjectionMode: store.InjectionModeAlways,
		CreatedBy: m.projectOwnerID,
	})
	require.NoError(t, err)
	require.NotNil(t, metaA)
	m.secret(t, swItemB, swItemBValue, store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
	for item, scope := range opts.items {
		m.secret(t, item, "v-"+item, store.SecretTypeEnvironment, scope, m.scopeID(scope))
	}

	faults := opts.faults
	if faults == nil {
		faults = map[string]error{swItemA: swAccessErr()}
	}
	fb := &swFaultBackend{SecretBackend: m.backend, faults: faults, sentinel: map[string]bool{swItemA: true}}
	m.d.SetSecretBackend(fb)
	m.client.startReturnResp = &RemoteAgentResponse{Agent: &RemoteAgentInfo{ID: m.agent.ID, Phase: string(state.PhaseRunning)}}

	srv, _ := testServerWithStore(t, m.store)
	srv.SetDispatcher(m.d)
	owner, err := m.store.GetUser(ctx, m.projectOwnerID)
	require.NoError(t, err)
	admin, err := m.store.GetUser(ctx, m.projectAdminID)
	require.NoError(t, err)
	return &swFixture{ldFixture: m, srv: srv, fb: fb, logs: logs, owner: owner, admin: admin}
}

// lifecycle posts a lifecycle action for the fixture agent as user.
func (f *swFixture) lifecycle(t *testing.T, user *store.User, action string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAsUser(t, f.srv, user, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/"+action, body)
}

// requireFetched is the non-empty precondition: the failing backend was
// asked for each name.
func requireFetched(t *testing.T, fb *swFaultBackend, names ...string) {
	t.Helper()
	got := fb.fetchedNames()
	require.NotEmpty(t, got, "precondition: the backend was hit")
	for _, n := range names {
		require.Contains(t, got, n, "precondition: %s reached the backend", n)
	}
}

// lifecycleWarnings (ephemeral_sqlite_helpers_test.go) decodes a lifecycle
// response's top-level warnings.

// sentSecretNames returns the names of the secrets the broker received.
func sentSecretNames(secrets []ResolvedSecret) []string {
	var names []string
	for _, s := range secrets {
		names = append(names, s.Name)
	}
	return names
}

// assertOneSkipLine checks the node logged exactly one skipped-secret WARN
// line for the agent, with exactly the given items and nothing else.
func assertOneSkipLine(t *testing.T, l *swLog, agentID string, purpose MaterialPurpose, items []skippedSecretLogItem) {
	t.Helper()
	recs := l.skipRecords(t, agentID)
	require.Len(t, recs, 1, "exactly one WARN line per dispatch")
	r := recs[0]
	assert.Equal(t, string(purpose), r["purpose"])
	assert.EqualValues(t, len(items), r["count"])
	var want []any
	for _, it := range items {
		want = append(want, map[string]any{"name": it.Name, "scope": it.Scope, "class": it.Class})
	}
	assert.Equal(t, want, r["items"])
	keys := map[string]bool{}
	for k := range r {
		keys[k] = true
	}
	for _, k := range []string{"time", "level", "msg", "agent_id", "purpose", "count", "items"} {
		delete(keys, k)
	}
	assert.Empty(t, keys, "the WARN line carries nothing else")
}

func swWarning(items, verb string) string {
	return skippedWarnPrefix + items + "; the agent was " + verb + " without them"
}

// Test 1: sync start.
func TestSkippedSecretWarning_SyncStart(t *testing.T) {
	f := newSWFixture(t, "sw-sync", swOptions{})
	rec := f.lifecycle(t, f.owner, "start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireFetched(t, f.fb, swItemA, swItemB)

	require.True(t, f.client.startCalled)
	assert.Contains(t, sentSecretNames(f.client.lastStartResolvedSecrets), swItemB, "the broker receives B")
	assert.NotContains(t, sentSecretNames(f.client.lastStartResolvedSecrets), swItemA, "the broker never receives A")
	assert.Equal(t, swItemBValue, f.client.lastResolvedEnv[swItemB])
	assert.NotContains(t, f.client.lastResolvedEnv, swItemA)

	assert.Equal(t, []string{swWarning(swItemA+" (project)", "started")}, lifecycleWarnings(t, rec.Body.Bytes()))
	assertOneSkipLine(t, f.logs, f.agent.ID, PurposeDispatch, []skippedSecretLogItem{{swItemA, "project", skipClassAccess}})
	assertNoSWLeak(t, rec.Body.Bytes(), f.logs)
}

// swAsyncClient accepts an asynchronous start as a broker would, after
// recording the send in the embedded mock.
type swAsyncClient struct {
	*mockRuntimeBrokerClient
}

func (c *swAsyncClient) StartAgent(ctx context.Context, brokerID, ep, agentID, projectID, task, projectPath, projectSlug, hc, hcID, hcHash string, env map[string]string, secrets []ResolvedSecret, inline *api.ScionConfig, dirs []api.SharedDir, sharedWS, resume bool, extras StartExtras) (*RemoteAgentResponse, error) {
	resp, err := c.mockRuntimeBrokerClient.StartAgent(ctx, brokerID, ep, agentID, projectID, task, projectPath, projectSlug, hc, hcID, hcHash, env, secrets, inline, dirs, sharedWS, resume, extras)
	if err != nil || !extras.AsyncLaunch {
		return resp, err
	}
	return &RemoteAgentResponse{LaunchPending: true, LaunchID: extras.LaunchID, LaunchInstanceID: "inst-1"}, nil
}

// Test 2: async-accepted start. The accepted lifecycle start answers 200
// with the starting agent and its launch (writeLaunchAccepted); the spec's
// "202-accepted" names that body.
func TestSkippedSecretWarning_AsyncAcceptedStart(t *testing.T) {
	f := newSWFixture(t, "sw-async", swOptions{})
	client := &swAsyncClient{mockRuntimeBrokerClient: f.client}
	f.d.client = client
	f.d.SetTokenGenerator(&fakeMintingTokenGenerator{store: f.store})
	f.d.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings { return asyncOn })
	f.srv.startClaimsOn = true
	setAsyncStartExperiment(t, f.srv, true)
	seedAsyncStartGate(f.srv, true)

	rec := f.lifecycle(t, f.owner, "start", map[string]bool{"acceptAsyncLaunch": true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireFetched(t, f.fb, swItemA, swItemB)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotNil(t, launchOf(body), "precondition: the start was accepted as an asynchronous launch")
	assert.True(t, f.client.lastStartExtras.AsyncLaunch)
	assert.Contains(t, sentSecretNames(f.client.lastStartResolvedSecrets), swItemB)
	assert.NotContains(t, sentSecretNames(f.client.lastStartResolvedSecrets), swItemA)

	assert.Equal(t, []string{swWarning(swItemA+" (project)", "started")}, lifecycleWarnings(t, rec.Body.Bytes()))
	assertOneSkipLine(t, f.logs, f.agent.ID, PurposeDispatch, []skippedSecretLogItem{{swItemA, "project", skipClassAccess}})
	assertNoSWLeak(t, rec.Body.Bytes(), f.logs)
}

// Test 3: restart.
// Test 3: restart. The restart endpoint is a stop leg followed by a start
// leg (startAgentCore, DispatchAgentStart): the start send carries B and not
// A, and the warning says "started", because the restart's start leg is a
// start. TestSkippedSecretWarning_DispatchAgentRestartVerb pins the
// "restarted" verb of DispatchAgentRestart (the restart mint site).
func TestSkippedSecretWarning_Restart(t *testing.T) {
	f := newSWFixture(t, "sw-restart", swOptions{})
	rec := f.lifecycle(t, f.owner, "restart", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireFetched(t, f.fb, swItemA, swItemB)

	require.True(t, f.client.stopCalled, "precondition: the restart's stop leg ran")
	require.True(t, f.client.startCalled, "precondition: the restart's start leg sent a start")
	assert.False(t, f.client.restartCalled, "the restart endpoint never sends a broker restart")
	assert.Contains(t, sentSecretNames(f.client.lastStartResolvedSecrets), swItemB, "the start leg carries B")
	assert.NotContains(t, sentSecretNames(f.client.lastStartResolvedSecrets), swItemA)
	assert.Equal(t, swItemBValue, f.client.lastResolvedEnv[swItemB])
	assert.NotContains(t, f.client.lastResolvedEnv, swItemA)

	assert.Equal(t, []string{swWarning(swItemA+" (project)", "started")}, lifecycleWarnings(t, rec.Body.Bytes()))
	assertOneSkipLine(t, f.logs, f.agent.ID, PurposeDispatch, []skippedSecretLogItem{{swItemA, "project", skipClassAccess}})
	assertNoSWLeak(t, rec.Body.Bytes(), f.logs)
}

// DispatchAgentRestart (the restart mint site) words its warning "restarted"
// and sends the broker restart with B and not A. It is reached by a
// reconcile "restart" row, not by the restart endpoint (see Test 3).
func TestSkippedSecretWarning_DispatchAgentRestartVerb(t *testing.T) {
	f := newSWFixture(t, "sw-restartverb", swOptions{})
	ctx, warns := withDispatchWarnings(context.Background())
	require.NoError(t, f.d.DispatchAgentRestart(ctx, f.agent))
	requireFetched(t, f.fb, swItemA, swItemB)

	require.True(t, f.client.restartCalled, "precondition: the broker restart was sent")
	assert.Contains(t, sentSecretNames(f.client.lastRestartExtras.ResolvedSecrets), swItemB, "the restart request carries B")
	assert.NotContains(t, sentSecretNames(f.client.lastRestartExtras.ResolvedSecrets), swItemA)
	assert.Equal(t, swItemBValue, f.client.lastRestartResolvedEnv[swItemB])
	assert.NotContains(t, f.client.lastRestartResolvedEnv, swItemA)

	assert.Equal(t, []string{swWarning(swItemA+" (project)", "restarted")}, warns.Warnings())
	assertOneSkipLine(t, f.logs, f.agent.ID, PurposeRestart, []skippedSecretLogItem{{swItemA, "project", skipClassAccess}})
	var all string
	for _, w := range warns.Warnings() {
		all += w
	}
	assertNoSWLeak(t, []byte(all), f.logs)
}

// swOwnerNode wraps the owner node's real dispatcher. Once its start or
// restart returns, it publishes the running status a broker report would,
// but only after the owner has settled the dispatch row: a requester whose
// wait ends on running before the row is done finds no result and misses
// the owner's warnings (the known limitation of a cross-node synchronous
// start, ptone/scion#6837). The order is forced so these tests check the
// round trip, not that race.
type swOwnerNode struct {
	*HTTPAgentDispatcher
	s   store.Store
	bus *ChannelEventPublisher
	t   *testing.T
}

func (o *swOwnerNode) publishWhenRowDone(ctx context.Context, a *store.Agent) {
	exec, ok := ExecutorContextFromContext(ctx)
	if !ok || exec.ID == "" {
		o.t.Errorf("owner dispatch has no dispatch row in its context")
		return
	}
	running := *a
	running.Phase = string(state.PhaseRunning)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if row, err := o.s.GetBrokerDispatch(context.Background(), exec.ID); err == nil && row.State == store.DispatchStateDone {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		o.bus.PublishAgentStatus(context.Background(), &running)
	}()
}

func (o *swOwnerNode) DispatchAgentStartWithOptions(ctx context.Context, a *store.Agent, task string, resume bool, opts StartDispatchOptions) (*StartDispatchResult, error) {
	res, err := o.HTTPAgentDispatcher.DispatchAgentStartWithOptions(ctx, a, task, resume, opts)
	if err == nil {
		o.publishWhenRowDone(ctx, a)
	}
	return res, err
}

func (o *swOwnerNode) DispatchAgentRestart(ctx context.Context, a *store.Agent) error {
	err := o.HTTPAgentDispatcher.DispatchAgentRestart(ctx, a)
	if err == nil {
		o.publishWhenRowDone(ctx, a)
	}
	return err
}

// DispatchAgentStop publishes the stopped status a broker report would, so
// a restart's deferred stop leg ends at once.
func (o *swOwnerNode) DispatchAgentStop(ctx context.Context, a *store.Agent) error {
	err := o.HTTPAgentDispatcher.DispatchAgentStop(ctx, a)
	stopped := *a
	stopped.Phase = string(state.PhaseStopped)
	o.bus.PublishAgentStatus(context.Background(), &stopped)
	return err
}

// swCrossNode is a two-node hub over the fixture's store: the fixture
// server is the requesting node, whose broker client defers every op; the
// owner node runs a real dispatcher with its own broker client. Each node
// has its own failing backend (same faults) and its own captured log.
type swCrossNode struct {
	reqFB, ownerFB   *swFaultBackend
	reqLog, ownerLog *swLog
	ownerClient      *mockRuntimeBrokerClient
}

func newSWCrossNode(t *testing.T, f *swFixture) *swCrossNode {
	t.Helper()
	setLifecycleTimings(t, 3*time.Second, 50*time.Millisecond, 0)
	bus := NewChannelEventPublisher()
	t.Cleanup(bus.Close)
	f.srv.events = bus
	c := &swCrossNode{reqLog: &swLog{}, ownerLog: &swLog{}}
	newFB := func() *swFaultBackend {
		return &swFaultBackend{SecretBackend: f.backend, faults: f.fb.faults, sentinel: f.fb.sentinel}
	}

	owner := &Server{
		store:             f.store,
		instanceID:        "hub-owner-" + uuid.NewString()[:8],
		agentLifecycleLog: c.ownerLog.logger(),
		events:            bus,
	}
	c.ownerClient = &mockRuntimeBrokerClient{}
	od := NewHTTPAgentDispatcherWithClient(f.store, c.ownerClient, false, c.ownerLog.logger())
	od.SetAuthzService(f.authz)
	od.SetHubID(f.hubID)
	c.ownerFB = newFB()
	od.SetSecretBackend(c.ownerFB)
	owner.SetDispatcher(&swOwnerNode{HTTPAgentDispatcher: od, s: f.store, bus: bus, t: t})
	owner.execDispatch = owner.executeDispatch
	owner.deliverMsg = owner.deliverMessage

	rd := NewHTTPAgentDispatcherWithClient(f.store, &mockRuntimeBrokerClient{returnErr: ErrLifecycleDeferred}, false, c.reqLog.logger())
	rd.SetAuthzService(f.authz)
	rd.SetHubID(f.hubID)
	rd.SetTokenGenerator(staticTokenGenerator{token: "test-token"})
	c.reqFB = newFB()
	rd.SetSecretBackend(c.reqFB)
	rd.SetCrossNodeDeps(bus, ownerSignalBus{owner: owner})
	f.srv.SetDispatcher(rd)
	return c
}

// Tests 4 and 5: routed (cross-node) start and restart. The requester runs
// its own selection against the same failing backend before the send
// defers, but reports nothing itself: no WARN line and no warning of its
// own. The owner raises the warning, it round-trips through
// LifecycleDispatchResult, and the requester's response carries the
// identical string. Dropping the round trip leaves the response without
// it, so this test fails.
func TestSkippedSecretWarning_CrossNode(t *testing.T) {
	for _, tc := range []struct {
		action  string
		verb    string
		purpose MaterialPurpose
	}{
		{"start", "started", PurposeDispatch},
		// The restart endpoint is a stop leg plus a start leg (see Test 3):
		// the owner executes the deferred stop row, then the deferred start
		// row, so the warning says "started".
		{"restart", "started", PurposeDispatch},
	} {
		t.Run(tc.action, func(t *testing.T) {
			f := newSWFixture(t, "sw-xn-"+tc.action, swOptions{})
			c := newSWCrossNode(t, f)
			rec := f.lifecycle(t, f.owner, tc.action, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			requireFetched(t, c.ownerFB, swItemA, swItemB)
			requireFetched(t, c.reqFB, swItemA)

			if tc.action == "restart" {
				require.True(t, c.ownerClient.stopCalled, "precondition: the owner ran the restart's stop leg")
			}
			require.True(t, c.ownerClient.startCalled, "precondition: the owner sent the start")
			assert.Contains(t, sentSecretNames(c.ownerClient.lastStartResolvedSecrets), swItemB)
			assert.NotContains(t, sentSecretNames(c.ownerClient.lastStartResolvedSecrets), swItemA)

			assert.Equal(t, []string{swWarning(swItemA+" (project)", tc.verb)}, lifecycleWarnings(t, rec.Body.Bytes()),
				"the owner's warning, and only it, reaches the requester's response")
			assertOneSkipLine(t, c.ownerLog, f.agent.ID, tc.purpose, []skippedSecretLogItem{{swItemA, "project", skipClassAccess}})
			assert.Empty(t, c.reqLog.skipRecords(t, f.agent.ID), "the requester logs no WARN line of its own")
			assert.Empty(t, f.logs.skipRecords(t, f.agent.ID), "nothing else logs one either")
			assertNoSWLeak(t, rec.Body.Bytes(), f.logs, c.reqLog, c.ownerLog)
		})
	}
}

// Test 6 (response classes): a not-found item and a missing result each
// warn; a decrypt failure is still the fixed 503 with no warning, even
// with an unreadable item alongside.
func TestSkippedSecretWarning_Classes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		class string
	}{
		{"not_found", store.ErrNotFound, skipClassNotFound},
		{"missing", swDrop, skipClassMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSWFixture(t, "sw-cls-"+tc.name, swOptions{faults: map[string]error{swItemA: tc.err}})
			rec := f.lifecycle(t, f.owner, "start", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			requireFetched(t, f.fb, swItemA, swItemB)
			assert.Equal(t, []string{swWarning(swItemA+" (project)", "started")}, lifecycleWarnings(t, rec.Body.Bytes()))
			assertOneSkipLine(t, f.logs, f.agent.ID, PurposeDispatch, []skippedSecretLogItem{{swItemA, "project", tc.class}})
		})
	}

	t.Run("decrypt_failure_is_503_without_warning", func(t *testing.T) {
		f := newSWFixture(t, "sw-cls-decrypt", swOptions{})
		foreign, err := secret.NewLegacyValueCipher("FAKE-foreign-secret").Encrypt("fake-undecryptable-value")
		require.NoError(t, err)
		require.NoError(t, f.store.CreateSecret(context.Background(), &store.Secret{
			ID: api.NewUUID(), Key: "SW_UNDECRYPTABLE", EncryptedValue: foreign, SecretType: store.SecretTypeEnvironment,
			Target: "SW_UNDECRYPTABLE", Scope: store.ScopeProject, ScopeID: f.agent.ProjectID,
			InjectionMode: store.InjectionModeAlways, CreatedBy: f.projectOwnerID,
		}))
		rec := f.lifecycle(t, f.owner, "start", nil)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
		requireFetched(t, f.fb, swItemA, "SW_UNDECRYPTABLE")
		assert.False(t, f.client.startCalled, "nothing reaches the broker")
		assert.NotContains(t, rec.Body.String(), skippedWarnPrefix, "no skipped-secret warning on a failed start")
		assert.NotContains(t, rec.Body.String(), swItemA)
		assert.Empty(t, f.logs.skipRecords(t, f.agent.ID), "no skipped-secret WARN line")
	})
}

// A send that fails (not deferred) still logs the skip, but the failed
// start's response never says the agent was started without them.
func TestSkippedSecretWarning_FailedSendLogsButDoesNotWarn(t *testing.T) {
	for _, action := range []string{"start", "restart"} {
		t.Run(action, func(t *testing.T) {
			f := newSWFixture(t, "sw-failsend-"+action, swOptions{})
			f.client.returnErr = errors.New("broker refused the request")
			ctx, warns := withDispatchWarnings(context.Background())
			var err error
			purpose := PurposeDispatch
			if action == "start" {
				err = f.d.DispatchAgentStart(ctx, f.agent, "task", false)
			} else {
				purpose = PurposeRestart
				err = f.d.DispatchAgentRestart(ctx, f.agent)
			}
			require.Error(t, err, "precondition: the send failed")
			requireFetched(t, f.fb, swItemA, swItemB)
			for _, w := range warns.Warnings() {
				assert.NotContains(t, w, skippedWarnPrefix, "a failed send adds no skipped-secret warning")
			}
			assertOneSkipLine(t, f.logs, f.agent.ID, purpose, []skippedSecretLogItem{{swItemA, "project", skipClassAccess}})
		})
	}
}

// Test 6 (policy is not a read failure): an item the delivery decision
// refuses gives no warning and no skipped-secret WARN line.
func TestSkippedSecretWarning_AuthzRefusedNotWarned(t *testing.T) {
	m := newLDFixture(t, "sw-refused", ldOptions{edge: withBoundedCeiling("agent.read")})
	l := &swLog{}
	m.d.log = l.logger()
	m.authz.logger = l.logger()
	_, meta, err := m.backend.Set(context.Background(), &secret.SetSecretInput{
		Name: swItemA, Value: swValueSentinel, SecretType: store.SecretTypeEnvironment, Target: swItemA,
		Scope: store.ScopeProject, ScopeID: m.agent.ProjectID, InjectionMode: store.InjectionModeAlways, CreatedBy: m.projectOwnerID,
	})
	require.NoError(t, err)
	require.NotNil(t, meta)
	fb := &swFaultBackend{SecretBackend: m.backend, faults: map[string]error{swItemA: swAccessErr()}}
	m.d.SetSecretBackend(fb)

	ctx, warns := withDispatchWarnings(context.Background())
	require.NoError(t, m.d.DispatchAgentRestart(ctx, m.agent))
	require.NotEmpty(t, warnRecords(t, l.bytes(), swDeniedSumUp), "precondition: the item was refused by the delivery decision")
	assert.NotContains(t, fb.fetchedNames(), swItemA, "a refused item is never fetched")
	assert.Empty(t, warns.Warnings())
	assert.Empty(t, l.skipRecords(t, m.agent.ID))
}

// Test 6 (policy is not a read failure): an item the test-admin
// restriction withholds is not fetched and not reported as skipped.
func TestSkippedSecretWarning_TestAdminWithheldNotWarned(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "sw-ta-iss", store.SystemRoleSuperAdmin)
	root := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin).Identity.ID
	projectID := tid("sw-ta-project")
	taProject(t, s, projectID, root)

	const hubID = "sw-ta-hub"
	backend := secret.NewLocalBackend(s, hubID, "test-secret")
	_, _, err := backend.Set(ctx, &secret.SetSecretInput{
		Name: swItemUser, Value: "v", SecretType: store.SecretTypeEnvironment, Target: swItemUser,
		Scope: store.ScopeUser, ScopeID: issuer.ID, InjectionMode: store.InjectionModeAlways, CreatedBy: issuer.ID,
	})
	require.NoError(t, err)
	fb := &swFaultBackend{SecretBackend: backend, faults: map[string]error{swItemUser: swAccessErr()}}
	d := NewHTTPAgentDispatcherWithClient(s, &mockRuntimeBrokerClient{}, false, slog.Default())
	d.SetHubID(hubID)
	d.SetAuthzService(srv.authzService)
	d.SetSecretBackend(fb)

	// The owner is not the test identity's root: its user scope is withheld.
	id := generateID()
	agent := &store.Agent{ID: id, Slug: "sw-ta-" + id[:8], Name: "sw-ta-" + id[:8], ProjectID: projectID,
		OwnerID: issuer.ID, CreatedBy: root, Ancestry: []string{root}, AppliedConfig: &store.AgentAppliedConfig{}}
	restricted, allowed, _ := d.testAdminRootScopes(ctx, agent)
	require.True(t, restricted && !allowed[store.ScopeUser], "precondition: the owner's user scope is withheld")
	got, skipped, err := d.selectDeliverySecrets(ctx, newDeliverySelector(srv.authzService, agent, PurposeRestart), agent)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Empty(t, skipped, "a withheld item is not a skipped one")
	assert.NotContains(t, fb.fetchedNames(), swItemUser, "a withheld item is never fetched")
}

// Selection level: the skipped list names exactly the failed items with
// their store scope and class, and never a delivered one.
func TestSelectDeliverySecrets_ReturnsSkipped(t *testing.T) {
	m := newLDFixture(t, "sw-sel", ldOptions{})
	for _, it := range []struct{ name, scope string }{
		{swItemA, store.ScopeProject}, {swItemHub, store.ScopeHub}, {swItemBroker, store.ScopeRuntimeBroker},
		{swItemUser, store.ScopeUser}, {swItemB, store.ScopeProject},
	} {
		m.secret(t, it.name, "v-"+it.name, store.SecretTypeEnvironment, it.scope, m.scopeID(it.scope))
	}
	fb := &swFaultBackend{SecretBackend: m.backend, faults: map[string]error{
		swItemA: swAccessErr(), swItemHub: store.ErrNotFound, swItemBroker: swDrop, swItemUser: swAccessErr(),
	}}
	m.d.SetSecretBackend(fb)
	got, skipped, err := m.d.selectDeliverySecrets(context.Background(), newDeliverySelector(m.authz, m.agent, PurposeRestart), m.agent)
	require.NoError(t, err)
	requireFetched(t, fb, swItemA, swItemHub, swItemBroker, swItemUser, swItemB)
	assert.Equal(t, []string{swItemB}, sentSecretNames(got))
	assert.ElementsMatch(t, []skippedSecret{
		{swItemA, store.ScopeProject, skipClassAccess},
		{swItemHub, store.ScopeHub, skipClassNotFound},
		{swItemBroker, store.ScopeRuntimeBroker, skipClassMissing},
		{swItemUser, store.ScopeUser, skipClassAccess},
	}, skipped)
}

// Test 7: no skip, no warnings key.
func TestSkippedSecretWarning_NoSkipNoWarningsKey(t *testing.T) {
	f := newSWFixture(t, "sw-noskip", swOptions{faults: map[string]error{}})
	rec := f.lifecycle(t, f.owner, "start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireFetched(t, f.fb, swItemA, swItemB)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.NotContains(t, raw, "warnings")
	assert.Empty(t, f.logs.skipRecords(t, f.agent.ID))
}

// Test 8: the audience rule. Project names go to every starter; user-scope
// names only to the owner; hub- and broker-scope items are always counts.
// The WARN log names everything in every case.
func TestSkippedSecretWarning_Audience(t *testing.T) {
	audienceFaults := map[string]error{
		swItemA: swAccessErr(), swItemUser: swAccessErr(), swItemHub: swAccessErr(), swItemBroker: swAccessErr(),
	}
	audienceItems := map[string]string{
		swItemUser: store.ScopeUser, swItemHub: store.ScopeHub, swItemBroker: store.ScopeRuntimeBroker,
	}
	ownerWant := swWarning(swItemA+" (project), "+swItemUser+" (user), 1 hub-scope secret, 1 broker-scope secret", "started")
	otherWant := swWarning(swItemA+" (project), 1 hub-scope secret, 1 broker-scope secret, 1 user-scope secret", "started")
	// Every name, sorted by name.
	logItems := []skippedSecretLogItem{
		{swItemA, "project", skipClassAccess},
		{swItemBroker, "broker", skipClassAccess},
		{swItemUser, "user", skipClassAccess},
		{swItemHub, "hub", skipClassAccess},
	}
	type requester struct {
		name string
		do   func(t *testing.T, f *swFixture) *httptest.ResponseRecorder
		want string
	}
	requesters := []requester{
		{"owner", func(t *testing.T, f *swFixture) *httptest.ResponseRecorder {
			return f.lifecycle(t, f.owner, "start", nil)
		}, ownerWant},
		{"non_owner_project_admin", func(t *testing.T, f *swFixture) *httptest.ResponseRecorder {
			return f.lifecycle(t, f.admin, "start", nil)
		}, otherWant},
		// Dev auth is a single local user; here it does not own the agent.
		{"dev_auth_non_owner", func(t *testing.T, f *swFixture) *httptest.ResponseRecorder {
			return doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/start", nil)
		}, otherWant},
	}
	for _, crossNode := range []bool{false, true} {
		for _, r := range requesters {
			name := r.name
			if crossNode {
				name = "cross_node/" + name
			}
			t.Run(name, func(t *testing.T) {
				f := newSWFixture(t, "sw-aud-"+strings.ReplaceAll(name, "/", "-"), swOptions{items: audienceItems, faults: audienceFaults})
				emitting := f.logs
				var c *swCrossNode
				if crossNode {
					c = newSWCrossNode(t, f)
					emitting = c.ownerLog
				}
				rec := r.do(t, f)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				fb := f.fb
				if crossNode {
					fb = c.ownerFB
				}
				requireFetched(t, fb, swItemA, swItemUser, swItemHub, swItemBroker)

				assert.Equal(t, []string{r.want}, lifecycleWarnings(t, rec.Body.Bytes()))
				body := rec.Body.String()
				if r.want == otherWant {
					assert.NotContains(t, body, swItemUser, "a non-owner never sees the user-scope name")
				} else {
					assert.Contains(t, body, swItemUser, "the owner sees the user-scope name")
				}
				assert.NotContains(t, body, swItemHub, "hub-scope names are never shown")
				assert.NotContains(t, body, swItemBroker, "broker-scope names are never shown")
				assertOneSkipLine(t, emitting, f.agent.ID, PurposeDispatch, logItems)
			})
		}
	}
}
