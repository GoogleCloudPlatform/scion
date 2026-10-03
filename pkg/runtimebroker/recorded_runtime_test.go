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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

// These tests pin that an existing-agent request carrying the agent's
// recorded runtime type only ever reaches runtimes of that type, and gets a
// retryable 503 instead of the default runtime when none is registered
// (ptone/scion#2748).

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
func newRecordedRuntimeServer(t *testing.T, withK8s bool) (*Server, *listCountingManager, *listCountingManager) {
	t.Helper()
	setupTestScionEnv(t)

	defaultMgr := &listCountingManager{}
	defaultMgr.agents = []api.AgentInfo{rrAgentInfo("docker-container")}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	srv := New(cfg, defaultMgr, rt)

	var auxMgr *listCountingManager
	if withK8s {
		auxMgr = &listCountingManager{}
		auxMgr.agents = []api.AgentInfo{rrAgentInfo("k8s-pod")}
		auxRt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
		srv.auxiliaryRuntimesMu.Lock()
		srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{Runtime: auxRt, Manager: auxMgr}
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
var existingAgentRequests = []struct {
	name, method, action, body string
}{
	{"get", http.MethodGet, "", ""},
	{"delete", http.MethodDelete, "", ""},
	{"stop", http.MethodPost, "/stop", ""},
	{"restart", http.MethodPost, "/restart", ""},
	{"reset-auth", http.MethodPost, "/reset-auth", `{"token":"t"}`},
	{"message", http.MethodPost, "/message", `{"message":"hi"}`},
	{"has-prompt", http.MethodPost, "/has-prompt", ""},
	{"logs", http.MethodGet, "/logs", ""},
	{"exec", http.MethodPost, "/exec", `{"command":["true"]}`},
}

func TestRecordedRuntime_UnregisteredReturns503WithoutDefaultFallback(t *testing.T) {
	for _, recorded := range []string{"kubernetes", "k8s", "cloudrun"} {
		for _, tc := range existingAgentRequests {
			t.Run(recorded+"/"+tc.name, func(t *testing.T) {
				srv, defaultMgr, _ := newRecordedRuntimeServer(t, false)

				w := serveRR(srv, tc.method, "/api/v1/agents/"+rrAgent+tc.action+rrQuery(recorded), tc.body)

				assertErrorCode(t, w, http.StatusServiceUnavailable, ErrCodeRuntimeUnavailable)
				if got := w.Header().Get("Retry-After"); got != recordedRuntimeRetryAfterSeconds {
					t.Errorf("Retry-After = %q, want %q", got, recordedRuntimeRetryAfterSeconds)
				}
				if !strings.Contains(w.Body.String(), "is not available on this broker") {
					t.Errorf("body = %s, want the runtime-not-available message", w.Body.String())
				}
				if n := defaultMgr.lists.Load(); n != 0 {
					t.Errorf("default runtime was listed %d time(s); it must not be consulted", n)
				}
				if defaultMgr.StopCalls() != 0 || defaultMgr.DeleteCalls() != 0 {
					t.Errorf("default runtime acted on the agent (stop=%d delete=%d)",
						defaultMgr.StopCalls(), defaultMgr.DeleteCalls())
				}
			})
		}
	}
}

func TestRecordedRuntime_KeysUnregisteredIsKeysUnavailable(t *testing.T) {
	srv, defaultMgr, _ := newRecordedRuntimeServer(t, false)
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
	if sent.Load() || defaultMgr.lists.Load() != 0 {
		t.Errorf("default runtime was used (sent=%v lists=%d)", sent.Load(), defaultMgr.lists.Load())
	}
}

func TestRecordedRuntime_RegisteredTypeIsTheOnlyRuntimeUsed(t *testing.T) {
	// "remote" is accepted because pkg/runtime.GetRuntime (factory.go)
	// already normalizes it to kubernetes; see isKubernetesRuntimeName.
	for _, recorded := range []string{"kubernetes", "k8s", "remote"} {
		t.Run(recorded+"/stop", func(t *testing.T) {
			srv, defaultMgr, auxMgr := newRecordedRuntimeServer(t, true)

			w := serveRR(srv, http.MethodPost, "/api/v1/agents/"+rrAgent+"/stop"+rrQuery(recorded), "")

			if w.Code >= 300 {
				t.Fatalf("status = %d, want success; body = %s", w.Code, w.Body.String())
			}
			if auxMgr.StopCalls() != 1 || defaultMgr.StopCalls() != 0 {
				t.Errorf("stop calls: kubernetes=%d docker=%d, want 1 and 0", auxMgr.StopCalls(), defaultMgr.StopCalls())
			}
			if n := defaultMgr.lists.Load(); n != 0 {
				t.Errorf("default runtime was listed %d time(s)", n)
			}
		})
		t.Run(recorded+"/delete", func(t *testing.T) {
			srv, defaultMgr, auxMgr := newRecordedRuntimeServer(t, true)

			w := serveRR(srv, http.MethodDelete, "/api/v1/agents/"+rrAgent+rrQuery(recorded), "")

			if w.Code >= 300 {
				t.Fatalf("status = %d, want success; body = %s", w.Code, w.Body.String())
			}
			if auxMgr.DeleteCalls() != 1 || defaultMgr.DeleteCalls() != 0 {
				t.Errorf("delete calls: kubernetes=%d docker=%d, want 1 and 0", auxMgr.DeleteCalls(), defaultMgr.DeleteCalls())
			}
			if auxMgr.lastDeleteContainerID != "k8s-pod" {
				t.Errorf("deleted container %q, want k8s-pod", auxMgr.lastDeleteContainerID)
			}
			if n := defaultMgr.lists.Load(); n != 0 {
				t.Errorf("default runtime was listed %d time(s)", n)
			}
		})
	}
}

func TestRecordedRuntime_KeysRegisteredTypeIsTheOnlyRuntimeUsed(t *testing.T) {
	srv, defaultMgr, auxMgr := newRecordedRuntimeServer(t, true)
	var defaultSent, auxSent atomic.Bool
	defaultMgr.sendKeysFunc = func(context.Context, string, string, string, string) error {
		defaultSent.Store(true)
		return nil
	}
	auxMgr.sendKeysFunc = func(context.Context, string, string, string, string) error {
		auxSent.Store(true)
		return nil
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
	if !auxSent.Load() || defaultSent.Load() {
		t.Errorf("keys sent: kubernetes=%v docker=%v, want true and false", auxSent.Load(), defaultSent.Load())
	}
}

// TestRecordedRuntime_MatchingDefaultSkipsAuxiliary covers a recorded type
// equal to the default runtime's: the auxiliary runtime of another type is
// never consulted.
func TestRecordedRuntime_MatchingDefaultSkipsAuxiliary(t *testing.T) {
	srv, defaultMgr, auxMgr := newRecordedRuntimeServer(t, true)
	// Only the auxiliary runtime holds the agent: a lookup that strayed
	// there would stop it.
	defaultMgr.agents = nil

	w := serveRR(srv, http.MethodPost, "/api/v1/agents/"+rrAgent+"/stop"+rrQuery("docker"), "")

	if w.Code >= 300 {
		t.Fatalf("status = %d, want success; body = %s", w.Code, w.Body.String())
	}
	if auxMgr.StopCalls() != 0 || auxMgr.lists.Load() != 0 {
		t.Errorf("kubernetes runtime was used (stop=%d lists=%d)", auxMgr.StopCalls(), auxMgr.lists.Load())
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
		"kubernetes":       {"kubernetes", true},
		"k8s":              {"kubernetes", true},
		"remote":           {"kubernetes", true},
		"docker":           {"docker", true},
		"podman":           {"podman", true},
		"container":        {"container", true},
		"cloudrun":         {"cloudrun", true},
		"cloudrun-sandbox": {"cloudrun-sandbox", true},
		"":                 {"", false},
		"local":            {"", false},
		"auto":             {"", false},
		"managed:x":        {"", false},
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

	newAuthServer := func(t *testing.T) (*Server, *listCountingManager) {
		srv, defaultMgr, _ := newRecordedRuntimeServer(t, false)
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
	check := func(t *testing.T, wantAuth bool, status int, body string, defaultMgr *listCountingManager) {
		t.Helper()
		if wantAuth {
			// Authenticated: the request reached the recorded-runtime gate
			// (no kubernetes runtime registered → 503).
			if status != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503 from the handler; body = %s", status, body)
			}
			return
		}
		if status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body = %s", status, body)
		}
		if defaultMgr.lists.Load() != 0 || defaultMgr.StopCalls() != 0 {
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
