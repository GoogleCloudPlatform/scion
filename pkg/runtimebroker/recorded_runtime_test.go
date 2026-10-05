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

package runtimebroker

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

// These tests pin how an existing-agent request carrying the agent's
// recorded runtime type picks its runtime (ptone/scion#2748): the recorded
// type first; any registered runtime that lists the agent otherwise; and a
// retryable 503, instead of the default runtime, when no runtime lists the
// agent and the recorded type is not registered.

const (
	rrAgent   = "worker"
	rrProject = "proj-rr"
)

// listCountingManager is a filteringMockManager that counts List calls, so a
// test can prove a runtime was never consulted at all.
type listCountingManager struct {
	filteringMockManager
	lists atomic.Int32
}

func (m *listCountingManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	m.lists.Add(1)
	return m.filteringMockManager.List(ctx, filter)
}

// rrManager is a listCountingManager that also counts every call that acts
// on an existing agent, on the manager or on its runtime, so a test can tell
// which runtime served a request. Start is not counted: restart's start leg
// resolves its runtime from the agent's saved profile, not from the
// recorded runtime type.
type rrManager struct {
	listCountingManager
	acts atomic.Int32
}

func (m *rrManager) acted() int32 { return m.acts.Load() }

func (m *rrManager) Stop(ctx context.Context, agentID, projectPath string) error {
	m.acts.Add(1)
	return m.listCountingManager.Stop(ctx, agentID, projectPath)
}

func (m *rrManager) Delete(ctx context.Context, agentID string, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	m.acts.Add(1)
	return m.listCountingManager.Delete(ctx, agentID, deleteFiles, projectPath, removeBranch)
}

func (m *rrManager) DeleteTarget(ctx context.Context, agentName string, ref runtime.RunRef, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	m.acts.Add(1)
	return m.listCountingManager.DeleteTarget(ctx, agentName, ref, deleteFiles, projectPath, removeBranch)
}

func (m *rrManager) Message(ctx context.Context, agentID, projectID string, message string, interrupt bool) error {
	m.acts.Add(1)
	return m.listCountingManager.Message(ctx, agentID, projectID, message, interrupt)
}

func (m *rrManager) MessageRaw(ctx context.Context, agentID, projectID string, keys string) error {
	m.acts.Add(1)
	return m.listCountingManager.MessageRaw(ctx, agentID, projectID, keys)
}

func (m *rrManager) SendKeys(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
	m.acts.Add(1)
	return m.listCountingManager.SendKeys(ctx, projectID, agentSlug, expectedAgentID, keys)
}

// newRRRuntime returns a mock runtime named name whose agent-level calls
// count as acts of m.
func newRRRuntime(name string, m *rrManager) *runtime.MockRuntime {
	return &runtime.MockRuntime{
		NameFunc:    func() string { return name },
		StopFunc:    func(context.Context, string) error { m.acts.Add(1); return nil },
		DeleteFunc:  func(context.Context, runtime.RunRef) error { m.acts.Add(1); return nil },
		GetLogsFunc: func(context.Context, string) (string, error) { m.acts.Add(1); return "log", nil },
		ExecFunc:    func(context.Context, string, []string) (string, error) { m.acts.Add(1); return "", nil },
		ExecWithStdinFunc: func(context.Context, string, []string, io.Reader) (string, error) {
			m.acts.Add(1)
			return "", nil
		},
	}
}

func rrAgentInfo(containerID string) api.AgentInfo {
	return api.AgentInfo{
		ContainerID: containerID,
		Name:        rrAgent,
		ProjectID:   rrProject,
		Labels: map[string]string{
			"scion.agent":      "true",
			"scion.name":       rrAgent,
			"scion.project_id": rrProject,
		},
	}
}

// newRecordedRuntimeServer returns a docker-default broker whose default
// runtime holds an agent named rrAgent in rrProject. With withK8s it also
// registers a kubernetes auxiliary runtime holding a same-named agent in the
// same project, so a lookup that strays to the wrong runtime finds a match
// there and the test can tell which runtime served the request.
func newRecordedRuntimeServer(t *testing.T, withK8s bool) (*Server, *rrManager, *rrManager) {
	t.Helper()
	setupTestScionEnv(t)

	defaultMgr := &rrManager{}
	defaultMgr.agents = []api.AgentInfo{rrAgentInfo("docker-container")}

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	srv := New(cfg, defaultMgr, newRRRuntime("docker", defaultMgr))

	var auxMgr *rrManager
	if withK8s {
		auxMgr = &rrManager{}
		auxMgr.agents = []api.AgentInfo{rrAgentInfo("k8s-pod")}
		srv.auxiliaryRuntimesMu.Lock()
		srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{Runtime: newRRRuntime("kubernetes", auxMgr), Manager: auxMgr}
		srv.auxiliaryRuntimesMu.Unlock()
	}
	return srv, defaultMgr, auxMgr
}

func rrQuery(recorded string) string {
	q := "?projectId=" + rrProject
	if recorded != "" {
		q += "&" + api.RecordedRuntimeQueryParam + "=" + recorded
	}
	return q
}

func serveRR(srv *Server, method, path string, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

// existingAgentRequests is every existing-agent route the hub dispatches,
// plus the bare GET.
type existingAgentRequest struct {
	name, method, action, body string
}

var existingAgentRequests = []existingAgentRequest{
	{"get", http.MethodGet, "", ""},
	{"delete", http.MethodDelete, "", ""},
	{"stop", http.MethodPost, "/stop", ""},
	{"restart", http.MethodPost, "/restart", ""},
	{"reset-auth", http.MethodPost, "/reset-auth", `{"token":"t"}`},
	{"message", http.MethodPost, "/message", `{"message":"hi"}`},
	{"has-prompt", http.MethodGet, "/has-prompt", ""},
	{"logs", http.MethodGet, "/logs", ""},
	{"exec", http.MethodPost, "/exec", `{"command":["true"]}`},
}

// servedBy reports whether the request for route tc was served by the
// runtime whose manager is m and whose agent entry has containerID: it acted
// on the agent, or — for the read-only routes — answered from its listing.
func servedBy(tc existingAgentRequest, w *httptest.ResponseRecorder, m *rrManager, containerID string) bool {
	switch tc.name {
	case "get":
		return strings.Contains(w.Body.String(), `"containerId":"`+containerID+`"`)
	case "has-prompt":
		return m.lists.Load() > 0
	default:
		return m.acted() > 0
	}
}

// TestRecordedRuntime_RecordedTypeServesEveryRoute pins, for every
// existing-agent route, that when both the default (docker) and an auxiliary
// (kubernetes) runtime list the agent, the runtime of the recorded type
// serves the request and the other runtime is neither listed nor acted on.
func TestRecordedRuntime_RecordedTypeServesEveryRoute(t *testing.T) {
	// "k8s" and "remote" are accepted because pkg/runtime.GetRuntime
	// (factory.go) already normalizes them to kubernetes; see
	// isKubernetesRuntimeName.
	for _, recorded := range []string{"kubernetes", "k8s", "remote", "docker"} {
		for _, tc := range existingAgentRequests {
			t.Run(recorded+"/"+tc.name, func(t *testing.T) {
				srv, defaultMgr, k8sMgr := newRecordedRuntimeServer(t, true)
				want, wantID, other := k8sMgr, "k8s-pod", defaultMgr
				if recorded == "docker" {
					want, wantID, other = defaultMgr, "docker-container", k8sMgr
				}

				w := serveRR(srv, tc.method, "/api/v1/agents/"+rrAgent+tc.action+rrQuery(recorded), tc.body)

				if w.Code >= 400 {
					t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
				}
				if !servedBy(tc, w, want, wantID) {
					t.Errorf("the %s runtime did not serve the request (lists=%d acts=%d body=%.120s)",
						recorded, want.lists.Load(), want.acted(), w.Body.String())
				}
				if n, a := other.lists.Load(), other.acted(); n != 0 || a != 0 {
					t.Errorf("the other runtime was consulted: lists=%d acts=%d", n, a)
				}
			})
		}
	}
}

// TestRecordedRuntime_PositiveMatchWins pins that a recorded type that does
// not hold the agent never hides a runtime that does: the agent, listed only
// by the default (docker) runtime, is served there whether or not a
// runtime of the recorded type is registered.
func TestRecordedRuntime_PositiveMatchWins(t *testing.T) {
	for _, tc := range existingAgentRequests {
		for _, k8sRegistered := range []bool{true, false} {
			name := tc.name + "/kubernetes-unregistered"
			if k8sRegistered {
				name = tc.name + "/kubernetes-registered"
			}
			t.Run(name, func(t *testing.T) {
				srv, defaultMgr, k8sMgr := newRecordedRuntimeServer(t, k8sRegistered)
				if k8sMgr != nil {
					k8sMgr.agents = nil
				}

				w := serveRR(srv, tc.method, "/api/v1/agents/"+rrAgent+tc.action+rrQuery("kubernetes"), tc.body)

				if w.Code >= 400 {
					t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
				}
				if !servedBy(tc, w, defaultMgr, "docker-container") {
					t.Errorf("the docker runtime holding the agent did not serve the request (lists=%d acts=%d body=%.120s)",
						defaultMgr.lists.Load(), defaultMgr.acted(), w.Body.String())
				}
				if k8sMgr != nil && k8sMgr.acted() != 0 {
					t.Errorf("the kubernetes runtime acted (%d) although it does not hold the agent", k8sMgr.acted())
				}
			})
		}
	}
}

// TestRecordedRuntime_FoundNowhereUnregisteredReturns503 pins the fail-closed
// case: no registered runtime lists the agent and the recorded type has no
// registered runtime, so the broker answers with a retryable 503 and nothing
// acts on the agent.
func TestRecordedRuntime_FoundNowhereUnregisteredReturns503(t *testing.T) {
	for _, recorded := range []string{"kubernetes", "k8s", "cloudrun"} {
		for _, tc := range existingAgentRequests {
			t.Run(recorded+"/"+tc.name, func(t *testing.T) {
				srv, defaultMgr, _ := newRecordedRuntimeServer(t, false)
				defaultMgr.agents = nil

				w := serveRR(srv, tc.method, "/api/v1/agents/"+rrAgent+tc.action+rrQuery(recorded), tc.body)

				assertErrorCode(t, w, http.StatusServiceUnavailable, ErrCodeRuntimeUnavailable)
				if got := w.Header().Get("Retry-After"); got != recordedRuntimeRetryAfterSeconds {
					t.Errorf("Retry-After = %q, want %q", got, recordedRuntimeRetryAfterSeconds)
				}
				if !strings.Contains(w.Body.String(), "is not available on this broker") {
					t.Errorf("body = %s, want the runtime-not-available message", w.Body.String())
				}
				if a := defaultMgr.acted(); a != 0 {
					t.Errorf("default runtime acted on the agent (%d)", a)
				}
			})
		}
	}
}

// TestRecordedRuntime_FoundNowhereRegisteredStaysInRecordedType covers an
// agent no runtime lists while the recorded type is registered: the request
// is handled within the recorded type as before (an idempotent stop), and
// the default runtime does not act.
func TestRecordedRuntime_FoundNowhereRegisteredStaysInRecordedType(t *testing.T) {
	srv, defaultMgr, k8sMgr := newRecordedRuntimeServer(t, true)
	defaultMgr.agents = nil
	k8sMgr.agents = nil

	w := serveRR(srv, http.MethodPost, "/api/v1/agents/"+rrAgent+"/stop"+rrQuery("kubernetes"), "")

	if w.Code == http.StatusServiceUnavailable {
		t.Fatalf("status = 503; a registered recorded type must not fail closed: %s", w.Body.String())
	}
	if defaultMgr.acted() != 0 {
		t.Errorf("default runtime acted (%d)", defaultMgr.acted())
	}
	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want the idempotent 202 for an agent that is gone; body = %s", w.Code, w.Body.String())
	}
	if k8sMgr.lists.Load() == 0 {
		t.Error("the recorded (kubernetes) runtime was not searched")
	}
}

func TestRecordedRuntime_KeysUnregisteredIsKeysUnavailable(t *testing.T) {
	srv, defaultMgr, _ := newRecordedRuntimeServer(t, false)
	defaultMgr.agents = nil
	var sent atomic.Bool
	defaultMgr.sendKeysFunc = func(context.Context, string, string, string, string) error {
		sent.Store(true)
		return nil
	}

	w := postKeys(t, srv, rrAgent, rrProject+"&"+api.RecordedRuntimeQueryParam+"=kubernetes", agentkeys.BrokerRequest{
		ProjectID:     rrProject,
		AgentID:       "agent-id",
		OperationID:   "op-1",
		ExecuteBefore: time.Now().UTC().Add(10 * time.Second),
		Keys:          "C-c",
	})

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != recordedRuntimeRetryAfterSeconds {
		t.Errorf("Retry-After = %q, want %q", got, recordedRuntimeRetryAfterSeconds)
	}
	res := decodeBrokerResult(t, w)
	if res.Outcome != agentkeys.OutcomeKeysUnavailable || res.OperationID != "op-1" {
		t.Errorf("result = %+v, want keys_unavailable echoing op-1", res)
	}
	if sent.Load() {
		t.Error("keys were sent through the default runtime")
	}
}

func TestRecordedRuntime_KeysServedByRuntimeHoldingAgent(t *testing.T) {
	for _, tc := range []struct {
		name        string
		k8sHolds    bool
		wantK8s     bool
		withK8s     bool
		dockerHolds bool
	}{
		{"recorded type holds it", true, true, true, true},
		{"only docker holds it, kubernetes registered", false, false, true, true},
		{"only docker holds it, kubernetes unregistered", false, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, defaultMgr, auxMgr := newRecordedRuntimeServer(t, tc.withK8s)
			var defaultSent, auxSent atomic.Bool
			defaultMgr.sendKeysFunc = func(context.Context, string, string, string, string) error {
				defaultSent.Store(true)
				return nil
			}
			if auxMgr != nil {
				if !tc.k8sHolds {
					auxMgr.agents = nil
				}
				auxMgr.sendKeysFunc = func(context.Context, string, string, string, string) error {
					auxSent.Store(true)
					return nil
				}
			}

			w := postKeys(t, srv, rrAgent, rrProject+"&"+api.RecordedRuntimeQueryParam+"=kubernetes", agentkeys.BrokerRequest{
				ProjectID:     rrProject,
				AgentID:       "agent-id",
				OperationID:   "op-2",
				ExecuteBefore: time.Now().UTC().Add(10 * time.Second),
				Keys:          "C-c",
			})

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			if auxSent.Load() != tc.wantK8s || defaultSent.Load() == tc.wantK8s {
				t.Errorf("keys sent: kubernetes=%v docker=%v, want kubernetes=%v", auxSent.Load(), defaultSent.Load(), tc.wantK8s)
			}
		})
	}
}

// TestRecordedRuntime_EmptyOrUnrecognisedKeepsPreviousBehaviour covers an
// absent recorded type (older hub, or no recorded runtime) and a value that
// names no known runtime: every registered runtime is searched, default
// first, exactly as before.
func TestRecordedRuntime_EmptyOrUnrecognisedKeepsPreviousBehaviour(t *testing.T) {
	for _, recorded := range []string{"", "local", "something-else"} {
		t.Run("default/"+recorded, func(t *testing.T) {
			srv, defaultMgr, auxMgr := newRecordedRuntimeServer(t, true)

			w := serveRR(srv, http.MethodPost, "/api/v1/agents/"+rrAgent+"/stop"+rrQuery(recorded), "")

			if w.Code >= 300 {
				t.Fatalf("status = %d, want success; body = %s", w.Code, w.Body.String())
			}
			if defaultMgr.StopCalls() != 1 || auxMgr.StopCalls() != 0 {
				t.Errorf("stop calls: docker=%d kubernetes=%d, want 1 and 0 (default first)",
					defaultMgr.StopCalls(), auxMgr.StopCalls())
			}
		})
		t.Run("no-k8s/"+recorded, func(t *testing.T) {
			srv, defaultMgr, _ := newRecordedRuntimeServer(t, false)

			w := serveRR(srv, http.MethodPost, "/api/v1/agents/"+rrAgent+"/stop"+rrQuery(recorded), "")

			if w.Code >= 300 {
				t.Fatalf("status = %d, want success; body = %s", w.Code, w.Body.String())
			}
			if defaultMgr.StopCalls() != 1 {
				t.Errorf("default stop calls = %d, want 1", defaultMgr.StopCalls())
			}
		})
	}
}

func TestCanonicalRuntimeName(t *testing.T) {
	cases := map[string]struct {
		want string
		ok   bool
	}{
		"kubernetes":         {"kubernetes", true},
		"k8s":                {"kubernetes", true},
		"remote":             {"kubernetes", true},
		"docker":             {"docker", true},
		"podman":             {"podman", true},
		"container":          {"container", true},
		"cloudrun":           {"cloudrun", true},
		"cloudrun-instances": {"cloudrun", true},
		"cloudrun-sandbox":   {"cloudrun-sandbox", true},
		"":                   {"", false},
		"local":              {"", false},
		"auto":               {"", false},
		"managed:x":          {"", false},
	}
	for in, tc := range cases {
		got, ok := canonicalRuntimeName(in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("canonicalRuntimeName(%q) = (%q,%v), want (%q,%v)", in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestRecordedRuntime_SignatureCoversParam pins that the recorded runtime
// parameter is covered by the hub's request signature on both broker entry
// points: the HMAC canonical string includes the raw query
// (apiclient.BuildCanonicalString), so a signed request whose runtime
// parameter is stripped, altered or added after signing is rejected before
// any handler runs.
func TestRecordedRuntime_SignatureCoversParam(t *testing.T) {
	secret := []byte("recorded-runtime-test-secret-key")
	path := "/api/v1/agents/" + rrAgent + "/stop"
	signedQuery := "projectId=" + rrProject + "&" + api.RecordedRuntimeQueryParam + "=kubernetes"

	cases := []struct {
		name      string
		signQuery string // query the hub signed
		sendQuery string // query the broker receives
		wantAuth  bool
	}{
		{"intact", signedQuery, signedQuery, true},
		{"stripped", signedQuery, "projectId=" + rrProject, false},
		{"altered", signedQuery, "projectId=" + rrProject + "&" + api.RecordedRuntimeQueryParam + "=docker", false},
		{"added", "projectId=" + rrProject, signedQuery, false},
	}

	newAuthServer := func(t *testing.T) (*Server, *rrManager) {
		srv, defaultMgr, _ := newRecordedRuntimeServer(t, false)
		defaultMgr.agents = nil
		mw := NewMultiKeyBrokerAuthMiddleware(true, 5*time.Minute, false)
		mw.UpdateKeys([]secretKeyEntry{{hubName: "hub", secretKey: secret}})
		srv.brokerAuthMiddleware = mw
		return srv, defaultMgr
	}
	// signedHeaders signs method+path?query the way the hub's transports do
	// (hmacBrokerSigner → apiclient.HMACAuth).
	signedHeaders := func(t *testing.T, query string) http.Header {
		req := httptest.NewRequest(http.MethodPost, "http://runtime-broker"+path+"?"+query, nil)
		auth := &apiclient.HMACAuth{BrokerID: "test-broker-id", SecretKey: secret}
		if err := auth.ApplyAuth(req); err != nil {
			t.Fatalf("sign: %v", err)
		}
		return req.Header
	}
	check := func(t *testing.T, wantAuth bool, status int, body string, defaultMgr *rrManager) {
		t.Helper()
		if wantAuth {
			// Authenticated: the request reached the recorded-runtime gate
			// (no runtime lists the agent and no kubernetes runtime is
			// registered → 503).
			if status != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503 from the handler; body = %s", status, body)
			}
			return
		}
		if status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body = %s", status, body)
		}
		if defaultMgr.lists.Load() != 0 || defaultMgr.acted() != 0 {
			t.Errorf("a request failing authentication reached a runtime")
		}
	}

	for _, tc := range cases {
		t.Run("http/"+tc.name, func(t *testing.T) {
			srv, defaultMgr := newAuthServer(t)
			r := httptest.NewRequest(http.MethodPost, path+"?"+tc.sendQuery, nil)
			for k, v := range signedHeaders(t, tc.signQuery) {
				r.Header[k] = v
			}
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)
			check(t, tc.wantAuth, w.Code, w.Body.String(), defaultMgr)
		})
		t.Run("control-channel/"+tc.name, func(t *testing.T) {
			srv, defaultMgr := newAuthServer(t)
			brokerConn, hubConn, cleanup := newWSPair(t)
			t.Cleanup(cleanup)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			client := &ControlChannelClient{
				config:      ControlChannelConfig{},
				conn:        brokerConn,
				handlers:    srv.Handler(),
				log:         slog.Default(),
				streams:     make(map[string]*StreamHandler),
				dispatchSem: make(chan struct{}, defaultMaxConcurrentDispatches),
				cancels:     make(map[string]context.CancelFunc),
				ctx:         ctx,
				cancel:      cancel,
			}
			headers := map[string]string{}
			// Sign once and reuse those exact headers (each signing uses a
			// fresh nonce).
			h := signedHeaders(t, tc.signQuery)
			for k := range h {
				headers[k] = h.Get(k)
			}
			client.wg.Add(1)
			go client.dispatchRequest(brokerConn, wsprotocol.RequestEnvelope{
				Type: "request", RequestID: "rr-" + tc.name, Method: http.MethodPost,
				Path: path, Query: tc.sendQuery, Headers: headers,
			})
			client.wg.Wait()
			var resp wsprotocol.ResponseEnvelope
			if err := hubConn.ReadJSON(&resp); err != nil {
				t.Fatalf("reading response envelope: %v", err)
			}
			check(t, tc.wantAuth, resp.StatusCode, string(resp.Body), defaultMgr)
			if tc.wantAuth && resp.Headers["Retry-After"] != recordedRuntimeRetryAfterSeconds {
				t.Errorf("Retry-After over the control channel = %q, want %q",
					resp.Headers["Retry-After"], recordedRuntimeRetryAfterSeconds)
			}
		})
	}
}

// TestRecordedRuntime_OtherAuxiliaryRuntimeIsNotUsed pins that the
// restriction also applies among auxiliary runtimes: an auxiliary runtime of
// another type that sorts first and holds a same-slug agent is never listed
// or acted on.
func TestRecordedRuntime_OtherAuxiliaryRuntimeIsNotUsed(t *testing.T) {
	for _, op := range []string{"stop", "delete"} {
		t.Run(op, func(t *testing.T) {
			srv, defaultMgr, k8sMgr := newRecordedRuntimeServer(t, true)
			otherMgr := &listCountingManager{}
			otherMgr.agents = []api.AgentInfo{rrAgentInfo("cloudrun-service")}
			otherRt := &runtime.MockRuntime{NameFunc: func() string { return "cloudrun" }}
			srv.auxiliaryRuntimesMu.Lock()
			srv.auxiliaryRuntimes["a-cloudrun"] = auxiliaryRuntime{Runtime: otherRt, Manager: otherMgr}
			srv.auxiliaryRuntimesMu.Unlock()

			var w *httptest.ResponseRecorder
			if op == "stop" {
				w = serveRR(srv, http.MethodPost, "/api/v1/agents/"+rrAgent+"/stop"+rrQuery("kubernetes"), "")
			} else {
				w = serveRR(srv, http.MethodDelete, "/api/v1/agents/"+rrAgent+rrQuery("kubernetes"), "")
			}

			if w.Code >= 300 {
				t.Fatalf("status = %d, want success; body = %s", w.Code, w.Body.String())
			}
			if k8sMgr.StopCalls()+k8sMgr.DeleteCalls() != 1 {
				t.Errorf("kubernetes runtime: stop=%d delete=%d, want one %s", k8sMgr.StopCalls(), k8sMgr.DeleteCalls(), op)
			}
			if n := otherMgr.lists.Load(); n != 0 || otherMgr.StopCalls() != 0 || otherMgr.DeleteCalls() != 0 {
				t.Errorf("cloudrun runtime used: lists=%d stop=%d delete=%d", n, otherMgr.StopCalls(), otherMgr.DeleteCalls())
			}
			if defaultMgr.lists.Load() != 0 || defaultMgr.StopCalls() != 0 || defaultMgr.DeleteCalls() != 0 {
				t.Error("default runtime used")
			}
		})
	}
}

// TestKnownRuntimeNamesMatchRuntimeNames pins knownRuntimeNames, and the
// kubernetes spelling, to the Name() values of the runtimes
// pkg/runtime.GetRuntime (factory.go) constructs, so a renamed or added
// runtime cannot drift from the recorded-runtime matching. GetRuntime's
// "cloudrun-instances" case constructs a CloudRunRuntime, so it reports
// "cloudrun".
func TestKnownRuntimeNamesMatchRuntimeNames(t *testing.T) {
	constructed := []runtime.Runtime{
		&runtime.DockerRuntime{},
		&runtime.PodmanRuntime{},
		&runtime.AppleContainerRuntime{},
		&runtime.CloudRunRuntime{},
		&runtime.CloudRunSandboxRuntime{},
		&runtime.SubstrateRuntime{},
		&runtime.KubernetesRuntime{},
	}
	names := map[string]bool{}
	for _, rt := range constructed {
		name := rt.Name()
		got, ok := canonicalRuntimeName(name)
		if !ok || got != name {
			t.Errorf("canonicalRuntimeName(%q) = (%q,%v), want the name itself", name, got, ok)
		}
		if name != "kubernetes" {
			names[name] = true
		}
	}
	for name := range knownRuntimeNames {
		if !names[name] {
			t.Errorf("knownRuntimeNames has %q, which no runtime GetRuntime constructs reports", name)
		}
	}
	if len(names) != len(knownRuntimeNames) {
		t.Errorf("runtime names %v, knownRuntimeNames %v", names, knownRuntimeNames)
	}
}

// TestRecordedRuntime_StartIsNotGated pins that start, which may create the
// agent, is not subject to the recorded-runtime check: an unregistered
// recorded type for an agent no runtime lists does not produce the
// runtime-unavailable 503.
func TestRecordedRuntime_StartIsNotGated(t *testing.T) {
	srv, defaultMgr, _ := newRecordedRuntimeServer(t, false)
	defaultMgr.agents = nil

	w := serveRR(srv, http.MethodPost, "/api/v1/agents/"+rrAgent+"/start"+rrQuery("cloudrun"), "")

	if w.Code == http.StatusServiceUnavailable && strings.Contains(w.Body.String(), "is not available on this broker") {
		t.Fatalf("start answered the recorded-runtime 503: %s", w.Body.String())
	}
}

// TestRecordedRuntime_AliasNamedRuntimeMatches pins that a runtime is matched
// by the canonical form of its Name(), so a runtime reporting an alias such as
// "k8s" serves an agent recorded as "kubernetes".
func TestRecordedRuntime_AliasNamedRuntimeMatches(t *testing.T) {
	srv, defaultMgr, _ := newRecordedRuntimeServer(t, false)
	defaultMgr.agents = nil
	auxMgr := &rrManager{}
	auxMgr.agents = []api.AgentInfo{rrAgentInfo("k8s-pod")}
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{Runtime: newRRRuntime("k8s", auxMgr), Manager: auxMgr}
	srv.auxiliaryRuntimesMu.Unlock()

	w := serveRR(srv, http.MethodPost, "/api/v1/agents/"+rrAgent+"/stop"+rrQuery("kubernetes"), "")

	if w.Code >= 300 {
		t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
	}
	if auxMgr.acted() == 0 {
		t.Error("the k8s-named runtime holding the agent did not act")
	}
}

// TestRecordedRuntime_RecordlessStopProbeFollowsRecordedType pins the stop
// path's record-less-actor probe under the recorded-type rule. A
// pre-restart substrate agent is listed by no runtime (it has no record),
// so the request is kept in its recorded type:
//   - recorded "substrate": the probe runs over the substrate runtime and
//     stop answers 409 identity-unknown, as it does without a recorded type;
//   - recorded "docker", with a docker runtime registered that does not hold
//     the agent: the probe sees only docker, finds no record-less actor, and
//     stop answers the idempotent 202 without touching substrate.
func TestRecordedRuntime_RecordlessStopProbeFollowsRecordedType(t *testing.T) {
	for _, tc := range []struct {
		recorded string
		want     int
	}{
		{"substrate", http.StatusConflict},
		{"docker", http.StatusAccepted},
	} {
		t.Run(tc.recorded, func(t *testing.T) {
			srv, fc := newTestSubstrateBrokerServer(t)
			runSubstrateAgentForProject(t, srv.manager, "dev", "projb", gapProjBID, testProjectScionDir(t, "projb"))
			simulateBrokerRestart(t)
			dockerMgr := &rrManager{}
			srv.auxiliaryRuntimesMu.Lock()
			srv.auxiliaryRuntimes["docker"] = auxiliaryRuntime{Runtime: newRRRuntime("docker", dockerMgr), Manager: dockerMgr}
			srv.auxiliaryRuntimesMu.Unlock()

			w := serveRR(srv, http.MethodPost, "/api/v1/agents/dev/stop?projectId="+gapProjBID+"&"+api.RecordedRuntimeQueryParam+"="+tc.recorded, "")

			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, tc.want, w.Body.String())
			}
			if tc.want == http.StatusConflict && decodeBrokerAPIError(t, w) != ErrCodeAgentIdentityUnknown {
				t.Errorf("body = %s, want %s", w.Body.String(), ErrCodeAgentIdentityUnknown)
			}
			if dockerMgr.acted() != 0 {
				t.Error("the docker runtime acted on an agent it does not hold")
			}
			fc.mu.Lock()
			defer fc.mu.Unlock()
			if n := len(fc.deleteActorCalls); n != 0 {
				t.Errorf("DeleteActor called %d time(s), want 0", n)
			}
		})
	}
}

// countingHandler counts log records by level.
type countingHandler struct {
	mu     sync.Mutex
	counts map[slog.Level]int
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if !strings.Contains(r.Message, "other than its recorded type") {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.counts[r.Level]++
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

// TestRecordedRuntime_MismatchLoggedOncePerAgent pins that an agent found in
// a runtime other than its recorded type is logged at warn level once per
// agent in the process, and at debug level on later requests.
func TestRecordedRuntime_MismatchLoggedOncePerAgent(t *testing.T) {
	srv, _, _ := newRecordedRuntimeServer(t, false)
	h := &countingHandler{counts: map[slog.Level]int{}}
	srv.agentLifecycleLog = slog.New(h)
	// Clear the process-wide record so earlier tests do not affect the count.
	q := "?projectId=" + rrProject + "&" + api.RecordedRuntimeQueryParam + "=kubernetes"
	recordedRuntimeMismatchLogged.Range(func(k, _ any) bool {
		recordedRuntimeMismatchLogged.Delete(k)
		return true
	})

	for i := 0; i < 3; i++ {
		if w := serveRR(srv, http.MethodGet, "/api/v1/agents/"+rrAgent+q, ""); w.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d; body = %s", i, w.Code, w.Body.String())
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.counts[slog.LevelWarn] != 1 || h.counts[slog.LevelDebug] != 2 {
		t.Errorf("mismatch log counts = %v, want 1 warn and 2 debug", h.counts)
	}
}

// TestRecordedRuntime_StatsNotGated pins that the placeholder stats route,
// which reads no runtime, does not run the recorded-runtime lookups.
func TestRecordedRuntime_StatsNotGated(t *testing.T) {
	srv, defaultMgr, auxMgr := newRecordedRuntimeServer(t, true)

	w := serveRR(srv, http.MethodGet, "/api/v1/agents/"+rrAgent+"/stats"+rrQuery("kubernetes"), "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
	}
	if n := defaultMgr.lists.Load() + auxMgr.lists.Load(); n != 0 {
		t.Errorf("stats made %d List calls, want 0", n)
	}
}
