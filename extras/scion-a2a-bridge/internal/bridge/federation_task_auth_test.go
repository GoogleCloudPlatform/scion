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

package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/identity"
	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

type taskRouteAuthFixture struct {
	srv            *Server
	ts             *httptest.Server
	bridge         *Bridge
	taskStore      *ScopedTaskStore
	store          state.Store
	hub            *httptest.Server
	mu             sync.Mutex
	hubAuthMeCode  int
	hubAuthMeID    string
	hubSendCode    int
	hubSendCalls   int
	hubAuthMeCalls int
	hubCreateCalls int
}

func newTaskRouteAuthFixture(t *testing.T, scheme, apiKey string, withMinter bool) *taskRouteAuthFixture {
	t.Helper()

	f := &taskRouteAuthFixture{
		hubAuthMeCode: http.StatusOK,
		hubAuthMeID:   "11111111-1111-4111-8111-111111111111",
		hubSendCode:   http.StatusOK,
	}

	f.hub = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/me":
			f.mu.Lock()
			f.hubAuthMeCalls++
			code := f.hubAuthMeCode
			id := f.hubAuthMeID
			f.mu.Unlock()
			if code != http.StatusOK {
				http.Error(w, "refused", code)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":           id,
				"email":        "visitor@example.com",
				"display_name": "Visitor",
				"role":         "member",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents":
			f.mu.Lock()
			f.hubCreateCalls++
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"agent": map[string]any{
					"id":        "agent-created-001",
					"name":      "brand-new-agent",
					"slug":      "brand-new-agent",
					"projectId": "proj1",
					"status":    "running",
				},
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/agents"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"agents": []map[string]any{{
					"id":        "agent-001",
					"name":      "agent1",
					"slug":      "agent1",
					"projectId": "proj1",
					"status":    "running",
				}},
				"totalCount": 1,
			})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/message"):
			f.mu.Lock()
			f.hubSendCalls++
			code := f.hubSendCode
			f.mu.Unlock()
			if code != http.StatusOK {
				http.Error(w, "send failed", code)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"conversationId": "conv-001",
				"messageId":      "msg-001",
			})
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(f.hub.Close)

	dir := t.TempDir()
	st, err := state.NewSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f.store = st

	cfg := &Config{
		Bridge: BridgeConfig{ExternalURL: "https://bridge.example.com"},
		Hub:    HubConfig{Endpoint: f.hub.URL, User: "admin@test"},
		Auth:   AuthConfig{Scheme: scheme, APIKey: apiKey},
		Projects: []ProjectConfig{
			{Slug: "proj1", ExposedAgents: []string{"agent1"}},
		},
		Timeouts: TimeoutConfig{SendMessage: 3 * time.Second},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	adminClient, err := hubclient.New(f.hub.URL, hubclient.WithBearerToken("admin-token"))
	if err != nil {
		t.Fatal(err)
	}

	var minter *identity.TokenMinter
	if withMinter {
		key := bytes.Repeat([]byte("k"), 32)
		minter, err = identity.NewTokenMinter(key)
		if err != nil {
			t.Fatal(err)
		}
	}

	b := New(st, adminClient, minter, cfg, nil, log)
	t.Cleanup(b.Shutdown)
	f.bridge = b

	executor := NewScionExecutor(b, log)
	innerStore := taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{
		Authenticator: RouteKeyAuthenticator(),
	})
	f.taskStore = NewScopedTaskStore(innerStore)

	sdkReqHandler := a2asrv.NewHandler(
		executor,
		a2asrv.WithLogger(log),
		a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{
			Streaming:         true,
			PushNotifications: false,
		}),
		a2asrv.WithTaskStore(f.taskStore),
	)
	guardedHandler := NewFederationTaskAuthHandler(sdkReqHandler)
	b.SetSDKRequestHandler(guardedHandler)

	jsonrpcHandler := a2asrv.NewJSONRPCHandler(guardedHandler)
	v0RESTHandler := a2av0.NewRESTHandler(guardedHandler)

	srv := NewServer(b, cfg, nil, log, jsonrpcHandler)
	srv.SetV0RESTHandler(v0RESTHandler)
	f.srv = srv
	f.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(f.ts.Close)

	return f
}

func (f *taskRouteAuthFixture) seedTask(t *testing.T, taskID string, caller *CallerIdentity, st a2a.TaskState) {
	t.Helper()
	ctx := WithRouteInfo(context.Background(), RouteInfo{ProjectSlug: "proj1", AgentSlug: "agent1"})
	if caller != nil {
		ctx = WithCallerIdentity(ctx, caller)
	}
	task := &a2a.Task{
		ID:        a2a.TaskID(taskID),
		ContextID: "ctx-" + taskID,
		Status:    a2a.TaskStatus{State: st},
		History: []*a2a.Message{
			a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("seed-content-"+taskID)),
		},
	}
	if _, err := f.taskStore.Create(ctx, task); err != nil {
		t.Fatalf("seed task %s: %v", taskID, err)
	}
}

func (f *taskRouteAuthFixture) doRequest(t *testing.T, method, path string, body []byte, headers map[string]string) (int, []byte) {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, f.ts.URL+path, bodyReader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := f.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do request %s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody
}

func TestFederationTaskRoutesAuth_FederationAgentRefused(t *testing.T) {
	f := newTaskRouteAuthFixture(t, "federation", "", true)

	// Seed tasks for agent A and agent B so any local-store read would return real data.
	agentACaller := &CallerIdentity{
		TokenType: "federation",
		AgentID:   "agent-a",
		IssuerURL: "https://hub-a.example.com",
	}
	agentBCaller := &CallerIdentity{
		TokenType: "federation",
		AgentID:   "agent-b",
		IssuerURL: "https://hub-a.example.com",
	}
	f.seedTask(t, "task-agent-a", agentACaller, a2a.TaskStateWorking)
	f.seedTask(t, "task-agent-b", agentBCaller, a2a.TaskStateWorking)

	agentAToken := buildTestJWT(t, map[string]any{
		"iss":        "https://hub-a.example.com",
		"sub":        "agent-a",
		"project_id": "proj-a",
	})
	// Unsigned token claiming agent B's subject.
	agentBClaimToken := buildTestJWT(t, map[string]any{
		"iss":        "https://hub-a.example.com",
		"sub":        "agent-b",
		"project_id": "proj-b",
	})

	tokens := []struct {
		name   string
		token  string
		taskID string
	}{
		{name: "agent_a_own_task", token: agentAToken, taskID: "task-agent-a"},
		{name: "agent_a_reading_agent_b_task", token: agentAToken, taskID: "task-agent-b"},
		{name: "unsigned_agent_b_claim_token", token: agentBClaimToken, taskID: "task-agent-b"},
	}

	for _, tc := range tokens {
		headers := map[string]string{"Authorization": "Bearer " + tc.token}

		t.Run("jsonrpc/"+tc.name, func(t *testing.T) {
			rpcMethods := []struct {
				name    string
				path    string
				payload string
			}{
				{
					name:    "GetTask_jsonrpc_path",
					path:    "/projects/proj1/agents/agent1/jsonrpc",
					payload: `{"jsonrpc":"2.0","id":"1","method":"GetTask","params":{"id":"` + tc.taskID + `"}}`,
				},
				{
					name:    "GetTask_root_path",
					path:    "/projects/proj1/agents/agent1",
					payload: `{"jsonrpc":"2.0","id":"2","method":"GetTask","params":{"id":"` + tc.taskID + `"}}`,
				},
				{
					name:    "ListTasks",
					path:    "/projects/proj1/agents/agent1/jsonrpc",
					payload: `{"jsonrpc":"2.0","id":"3","method":"ListTasks","params":{}}`,
				},
				{
					name:    "SubscribeToTask",
					path:    "/projects/proj1/agents/agent1/jsonrpc",
					payload: `{"jsonrpc":"2.0","id":"4","method":"SubscribeToTask","params":{"id":"` + tc.taskID + `"}}`,
				},
				{
					name:    "CreateTaskPushNotificationConfig",
					path:    "/projects/proj1/agents/agent1/jsonrpc",
					payload: `{"jsonrpc":"2.0","id":"5","method":"CreateTaskPushNotificationConfig","params":{"taskId":"` + tc.taskID + `","url":"https://callback.example.com"}}`,
				},
				{
					name:    "GetTaskPushNotificationConfig",
					path:    "/projects/proj1/agents/agent1/jsonrpc",
					payload: `{"jsonrpc":"2.0","id":"6","method":"GetTaskPushNotificationConfig","params":{"taskId":"` + tc.taskID + `","id":"cfg-1"}}`,
				},
				{
					name:    "ListTaskPushNotificationConfigs",
					path:    "/projects/proj1/agents/agent1/jsonrpc",
					payload: `{"jsonrpc":"2.0","id":"7","method":"ListTaskPushNotificationConfigs","params":{"taskId":"` + tc.taskID + `"}}`,
				},
				{
					name:    "DeleteTaskPushNotificationConfig",
					path:    "/projects/proj1/agents/agent1/jsonrpc",
					payload: `{"jsonrpc":"2.0","id":"8","method":"DeleteTaskPushNotificationConfig","params":{"taskId":"` + tc.taskID + `","id":"cfg-1"}}`,
				},
				{
					name:    "GetExtendedAgentCard",
					path:    "/projects/proj1/agents/agent1/jsonrpc",
					payload: `{"jsonrpc":"2.0","id":"9","method":"GetExtendedAgentCard","params":{}}`,
				},
			}

			for _, m := range rpcMethods {
				t.Run(m.name, func(t *testing.T) {
					status, body := f.doRequest(t, http.MethodPost, m.path, []byte(m.payload), headers)
					if status != http.StatusOK {
						t.Fatalf("status = %d, want 200 JSON-RPC envelope; body: %s", status, body)
					}
					bodyStr := string(body)
					if !strings.Contains(bodyStr, `"error"`) {
						t.Fatalf("expected JSON-RPC error, got: %s", bodyStr)
					}
					if !strings.Contains(bodyStr, "-31403") {
						t.Errorf("expected unauthorized code -31403, got: %s", bodyStr)
					}
					if !strings.Contains(bodyStr, "forbidden: task route is not available for federation agent callers") {
						t.Errorf("expected fixed non-leaky error message, got: %s", bodyStr)
					}
					if strings.Contains(bodyStr, "seed-content-") || strings.Contains(bodyStr, tc.taskID) {
						t.Errorf("response body must not include task data or task ID: %s", bodyStr)
					}
				})
			}
		})

		t.Run("v0rest/"+tc.name, func(t *testing.T) {
			restRoutes := []struct {
				name   string
				method string
				path   string
				body   string
			}{
				{
					name:   "get_task",
					method: http.MethodGet,
					path:   "/projects/proj1/agents/agent1/tasks/" + tc.taskID,
				},
				{
					name:   "list_tasks",
					method: http.MethodGet,
					path:   "/projects/proj1/agents/agent1/tasks",
				},
				{
					name:   "subscribe_task",
					method: http.MethodPost,
					path:   "/projects/proj1/agents/agent1/tasks/" + tc.taskID + ":subscribe",
				},
				{
					name:   "create_push_config",
					method: http.MethodPost,
					path:   "/projects/proj1/agents/agent1/tasks/" + tc.taskID + "/pushNotificationConfigs",
					body:   `{"url":"https://callback.example.com"}`,
				},
				{
					name:   "get_push_config",
					method: http.MethodGet,
					path:   "/projects/proj1/agents/agent1/tasks/" + tc.taskID + "/pushNotificationConfigs/cfg-1",
				},
				{
					name:   "list_push_configs",
					method: http.MethodGet,
					path:   "/projects/proj1/agents/agent1/tasks/" + tc.taskID + "/pushNotificationConfigs",
				},
				{
					name:   "delete_push_config",
					method: http.MethodDelete,
					path:   "/projects/proj1/agents/agent1/tasks/" + tc.taskID + "/pushNotificationConfigs/cfg-1",
				},
				{
					name:   "extended_agent_card",
					method: http.MethodGet,
					path:   "/projects/proj1/agents/agent1/extendedAgentCard",
				},
			}

			for _, r := range restRoutes {
				t.Run(r.name, func(t *testing.T) {
					var reqBody []byte
					if r.body != "" {
						reqBody = []byte(r.body)
					}
					status, body := f.doRequest(t, r.method, r.path, reqBody, headers)
					bodyStr := string(body)
					if r.name == "subscribe_task" {
						// SSE streaming endpoints return HTTP 200 and emit the 403 error event in the stream.
						if status != http.StatusOK {
							t.Fatalf("status = %d, want 200 SSE stream; body: %s", status, bodyStr)
						}
						if !strings.Contains(bodyStr, `"code":403`) || !strings.Contains(bodyStr, `"PERMISSION_DENIED"`) {
							t.Errorf("expected 403 PERMISSION_DENIED error event, got: %s", bodyStr)
						}
					} else if status != http.StatusForbidden {
						t.Fatalf("status = %d, want 403; body: %s", status, bodyStr)
					}
					if !strings.Contains(bodyStr, "forbidden: task route is not available for federation agent callers") {
						t.Errorf("expected fixed non-leaky error message, got: %s", bodyStr)
					}
					if strings.Contains(bodyStr, "seed-content-") {
						t.Errorf("response body must not include task data: %s", bodyStr)
					}
				})
			}
		})
	}
}

func TestFederationTaskRoutesAuth_FederatedUserAllowedAndHubFailureFailsClosed(t *testing.T) {
	f := newTaskRouteAuthFixture(t, "federation", "", true)

	visitorCaller := &CallerIdentity{
		TokenType: "federation",
		UserID:    f.hubAuthMeID,
		Email:     "visitor@example.com",
		IssuerURL: "https://hub-a.example.com",
		Subject:   "user-sub-1",
	}
	f.seedTask(t, "task-visitor-1", visitorCaller, a2a.TaskStateCompleted)

	userToken := buildTestJWT(t, map[string]any{
		"iss":              "https://hub-a.example.com",
		"sub":              "user-sub-1",
		"email":            "visitor@example.com",
		"scion_token_kind": "user",
	})
	headers := map[string]string{"Authorization": "Bearer " + userToken}

	t.Run("jsonrpc_get_and_list_succeed_for_verified_federated_user", func(t *testing.T) {
		status, body := f.doRequest(t, http.MethodPost, "/projects/proj1/agents/agent1/jsonrpc",
			[]byte(`{"jsonrpc":"2.0","id":"u1","method":"GetTask","params":{"id":"task-visitor-1"}}`), headers)
		if status != http.StatusOK || strings.Contains(string(body), `"error"`) {
			t.Fatalf("GetTask failed: status=%d body=%s", status, body)
		}
		if !strings.Contains(string(body), "task-visitor-1") {
			t.Errorf("expected task-visitor-1 in response: %s", body)
		}

		status, body = f.doRequest(t, http.MethodPost, "/projects/proj1/agents/agent1/jsonrpc",
			[]byte(`{"jsonrpc":"2.0","id":"u2","method":"ListTasks","params":{}}`), headers)
		if status != http.StatusOK || strings.Contains(string(body), `"error"`) {
			t.Fatalf("ListTasks failed: status=%d body=%s", status, body)
		}
		if !strings.Contains(string(body), "task-visitor-1") {
			t.Errorf("expected task-visitor-1 in list response: %s", body)
		}
	})

	t.Run("v0rest_get_and_list_succeed_for_verified_federated_user", func(t *testing.T) {
		status, body := f.doRequest(t, http.MethodGet, "/projects/proj1/agents/agent1/tasks/task-visitor-1", nil, headers)
		if status != http.StatusOK {
			t.Fatalf("GET /tasks/task-visitor-1 status=%d body=%s", status, body)
		}
		status, body = f.doRequest(t, http.MethodGet, "/projects/proj1/agents/agent1/tasks", nil, headers)
		if status != http.StatusOK {
			t.Fatalf("GET /tasks status=%d body=%s", status, body)
		}
	})

	t.Run("hub_verification_failure_fails_closed", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			hubCode  int
			wantCode int
		}{
			{name: "hub_401", hubCode: http.StatusUnauthorized, wantCode: http.StatusUnauthorized},
			{name: "hub_403", hubCode: http.StatusForbidden, wantCode: http.StatusForbidden},
			{name: "hub_500", hubCode: http.StatusInternalServerError, wantCode: http.StatusBadGateway},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f.mu.Lock()
				f.hubAuthMeCode = tc.hubCode
				f.mu.Unlock()
				t.Cleanup(func() {
					f.mu.Lock()
					f.hubAuthMeCode = http.StatusOK
					f.mu.Unlock()
				})

				status, _ := f.doRequest(t, http.MethodGet, "/projects/proj1/agents/agent1/tasks/task-visitor-1", nil, headers)
				if status != tc.wantCode {
					t.Errorf("v0 REST status = %d, want %d", status, tc.wantCode)
				}
				status, _ = f.doRequest(t, http.MethodPost, "/projects/proj1/agents/agent1/jsonrpc",
					[]byte(`{"jsonrpc":"2.0","id":"u3","method":"GetTask","params":{"id":"task-visitor-1"}}`), headers)
				if status != tc.wantCode {
					t.Errorf("JSON-RPC status = %d, want %d", status, tc.wantCode)
				}
			})
		}
	})
}

func TestFederationTaskRoutesAuth_OtherSchemesUnchanged(t *testing.T) {
	schemes := []struct {
		scheme      string
		apiKey      string
		authHeaders map[string]string
		needAuth    bool
	}{
		{
			scheme:      "apiKey",
			apiKey:      "secret-api-key",
			authHeaders: map[string]string{"X-API-Key": "secret-api-key"},
			needAuth:    true,
		},
		{
			scheme:      "bearer",
			apiKey:      "secret-bearer-key",
			authHeaders: map[string]string{"Authorization": "Bearer secret-bearer-key"},
			needAuth:    true,
		},
		{
			scheme:      "none",
			apiKey:      "",
			authHeaders: nil,
			needAuth:    false,
		},
	}

	for _, sc := range schemes {
		t.Run(sc.scheme, func(t *testing.T) {
			f := newTaskRouteAuthFixture(t, sc.scheme, sc.apiKey, true)
			f.seedTask(t, "task-legacy-1", nil, a2a.TaskStateCompleted)

			if sc.needAuth {
				status, _ := f.doRequest(t, http.MethodGet, "/projects/proj1/agents/agent1/tasks/task-legacy-1", nil, nil)
				if status != http.StatusUnauthorized {
					t.Errorf("unauthenticated v0 REST status = %d, want 401", status)
				}
				status, _ = f.doRequest(t, http.MethodPost, "/projects/proj1/agents/agent1/jsonrpc",
					[]byte(`{"jsonrpc":"2.0","id":"1","method":"GetTask","params":{"id":"task-legacy-1"}}`), nil)
				if status != http.StatusUnauthorized {
					t.Errorf("unauthenticated JSON-RPC status = %d, want 401", status)
				}
			}

			// Authenticated (or open under "none"): GetTask and ListTasks succeed on both JSON-RPC and v0 REST.
			status, body := f.doRequest(t, http.MethodPost, "/projects/proj1/agents/agent1/jsonrpc",
				[]byte(`{"jsonrpc":"2.0","id":"2","method":"GetTask","params":{"id":"task-legacy-1"}}`), sc.authHeaders)
			if status != http.StatusOK || strings.Contains(string(body), `"error"`) {
				t.Fatalf("JSON-RPC GetTask failed: status=%d body=%s", status, body)
			}

			status, body = f.doRequest(t, http.MethodPost, "/projects/proj1/agents/agent1/jsonrpc",
				[]byte(`{"jsonrpc":"2.0","id":"3","method":"ListTasks","params":{}}`), sc.authHeaders)
			if status != http.StatusOK || strings.Contains(string(body), `"error"`) {
				t.Fatalf("JSON-RPC ListTasks failed: status=%d body=%s", status, body)
			}

			status, body = f.doRequest(t, http.MethodGet, "/projects/proj1/agents/agent1/tasks/task-legacy-1", nil, sc.authHeaders)
			if status != http.StatusOK {
				t.Fatalf("v0 REST GetTask failed: status=%d body=%s", status, body)
			}

			status, body = f.doRequest(t, http.MethodGet, "/projects/proj1/agents/agent1/tasks", nil, sc.authHeaders)
			if status != http.StatusOK {
				t.Fatalf("v0 REST ListTasks failed: status=%d body=%s", status, body)
			}
		})
	}
}

func TestCancelTask_BestEffortAndNoAdminFallback(t *testing.T) {
	t.Run("executor_hub_send_error_cancels_locally_for_legacy_caller", func(t *testing.T) {
		f := newTaskRouteAuthFixture(t, "none", "", true)
		f.seedTask(t, "task-cancel-fail", nil, a2a.TaskStateWorking)

		f.mu.Lock()
		f.hubSendCode = http.StatusInternalServerError
		f.mu.Unlock()

		status, body := f.doRequest(t, http.MethodPost, "/projects/proj1/agents/agent1/jsonrpc",
			[]byte(`{"jsonrpc":"2.0","id":"c1","method":"CancelTask","params":{"id":"task-cancel-fail"}}`), nil)
		if status != http.StatusOK || strings.Contains(string(body), `"error"`) {
			t.Fatalf("CancelTask failed: status=%d body=%s", status, body)
		}

		ctx := WithRouteInfo(context.Background(), RouteInfo{ProjectSlug: "proj1", AgentSlug: "agent1"})
		stored, err := f.taskStore.Get(ctx, "task-cancel-fail")
		if err != nil {
			t.Fatalf("taskStore.Get: %v", err)
		}
		if stored.Task.Status.State != a2a.TaskStateCanceled {
			t.Errorf("task state = %v, want %v", stored.Task.Status.State, a2a.TaskStateCanceled)
		}
	})

	t.Run("executor_federation_agent_cancel_refused_before_hub", func(t *testing.T) {
		f := newTaskRouteAuthFixture(t, "federation", "", true)
		agentCaller := &CallerIdentity{
			TokenType: "federation",
			AgentID:   "agent-a",
			IssuerURL: "https://hub-a.example.com",
		}
		f.seedTask(t, "task-fed-cancel-refused", agentCaller, a2a.TaskStateWorking)

		agentToken := buildTestJWT(t, map[string]any{
			"iss":        "https://hub-a.example.com",
			"sub":        "agent-a",
			"project_id": "proj-a",
		})
		headers := map[string]string{"Authorization": "Bearer " + agentToken}

		status, body := f.doRequest(t, http.MethodPost, "/projects/proj1/agents/agent1/jsonrpc",
			[]byte(`{"jsonrpc":"2.0","id":"c2","method":"CancelTask","params":{"id":"task-fed-cancel-refused"}}`), headers)
		assertFederationAgentRefusalJSONRPC(t, status, body, "seed-content-", "task-fed-cancel-refused")

		f.mu.Lock()
		calls := f.hubSendCalls
		f.mu.Unlock()
		if calls != 0 {
			t.Errorf("hubSendCalls = %d, want 0", calls)
		}

		ctx := WithCallerIdentity(
			WithRouteInfo(context.Background(), RouteInfo{ProjectSlug: "proj1", AgentSlug: "agent1"}),
			agentCaller,
		)
		stored, err := f.taskStore.Get(ctx, "task-fed-cancel-refused")
		if err != nil {
			t.Fatalf("taskStore.Get: %v", err)
		}
		if stored.Task.Status.State != a2a.TaskStateWorking {
			t.Errorf("task state = %v, want %v", stored.Task.Status.State, a2a.TaskStateWorking)
		}
	})

	t.Run("executor_per_caller_client_failure_no_admin_fallback", func(t *testing.T) {
		// Verify ScionExecutor.Cancel fails closed without falling back to the
		// admin client when callerHubClient fails (withMinter=false).
		f := newTaskRouteAuthFixture(t, "federation", "", false)
		agentCaller := &CallerIdentity{
			TokenType: "federation",
			AgentID:   "agent-a",
			IssuerURL: "https://hub-a.example.com",
		}
		ctx := WithCallerIdentity(
			WithRouteInfo(context.Background(), RouteInfo{ProjectSlug: "proj1", AgentSlug: "agent1"}),
			agentCaller,
		)
		exec := NewScionExecutor(f.bridge, slog.New(slog.NewTextHandler(io.Discard, nil)))
		execCtx := &a2asrv.ExecutorContext{
			TaskID:     "task-no-fallback",
			ContextID:  "ctx-no-fallback",
			StoredTask: &a2a.Task{ID: "task-no-fallback", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}},
		}
		var gotErr error
		for _, err := range exec.Cancel(ctx, execCtx) {
			if err != nil {
				gotErr = err
			}
		}
		if gotErr == nil {
			t.Fatal("expected error when per-caller client cannot be built, got nil")
		}

		f.mu.Lock()
		calls := f.hubSendCalls
		f.mu.Unlock()
		if calls != 0 {
			t.Errorf("hubSendCalls = %d, want 0 (must not fall back to admin client)", calls)
		}
	})

	t.Run("executor_hub_send_success_marks_canceled", func(t *testing.T) {
		f := newTaskRouteAuthFixture(t, "apiKey", "secret-key", true)
		f.seedTask(t, "task-cancel-ok", nil, a2a.TaskStateWorking)
		headers := map[string]string{"X-API-Key": "secret-key"}

		status, body := f.doRequest(t, http.MethodPost, "/projects/proj1/agents/agent1/jsonrpc",
			[]byte(`{"jsonrpc":"2.0","id":"c4","method":"CancelTask","params":{"id":"task-cancel-ok"}}`), headers)
		if status != http.StatusOK || strings.Contains(string(body), `"error"`) {
			t.Fatalf("CancelTask failed: status=%d body=%s", status, body)
		}

		ctx := WithRouteInfo(context.Background(), RouteInfo{ProjectSlug: "proj1", AgentSlug: "agent1"})
		stored, err := f.taskStore.Get(ctx, "task-cancel-ok")
		if err != nil {
			t.Fatalf("taskStore.Get: %v", err)
		}
		if stored.Task.Status.State != a2a.TaskStateCanceled {
			t.Errorf("task state = %v, want %v", stored.Task.Status.State, a2a.TaskStateCanceled)
		}
	})

	t.Run("v0rest_cancel_hub_send_error_cancels_locally_for_legacy_caller", func(t *testing.T) {
		f := newTaskRouteAuthFixture(t, "none", "", true)
		f.seedTask(t, "task-v0-cancel-fail", nil, a2a.TaskStateWorking)

		f.mu.Lock()
		f.hubSendCode = http.StatusInternalServerError
		f.mu.Unlock()

		status, body := f.doRequest(t, http.MethodPost, "/projects/proj1/agents/agent1/tasks/task-v0-cancel-fail:cancel", nil, nil)
		if status != http.StatusOK {
			t.Fatalf("v0 REST cancel status = %d, want 200; body: %s", status, body)
		}

		ctx := WithRouteInfo(context.Background(), RouteInfo{ProjectSlug: "proj1", AgentSlug: "agent1"})
		stored, err := f.taskStore.Get(ctx, "task-v0-cancel-fail")
		if err != nil {
			t.Fatalf("taskStore.Get: %v", err)
		}
		if stored.Task.Status.State != a2a.TaskStateCanceled {
			t.Errorf("task state = %v, want %v", stored.Task.Status.State, a2a.TaskStateCanceled)
		}
	})

	t.Run("bridge_cancel_task_hub_send_failure_cancels_locally", func(t *testing.T) {
		f := newTaskRouteAuthFixture(t, "none", "", true)
		f.mu.Lock()
		f.hubSendCode = http.StatusInternalServerError
		f.mu.Unlock()

		now := time.Now()
		if err := f.store.CreateTask(context.Background(), &state.Task{
			ID:        "state-task-1",
			ContextID: "ctx-1",
			ProjectID: "proj1",
			AgentSlug: "agent1",
			AgentID:   "agent-001",
			State:     TaskStateWorking,
			CreatedAt: now,
			UpdatedAt: now,
			Metadata:  "{}",
		}); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}

		if _, err := f.bridge.CancelTask(context.Background(), "state-task-1"); err != nil {
			t.Fatalf("Bridge.CancelTask returned unexpected error: %v", err)
		}

		got, err := f.store.GetTask(context.Background(), "state-task-1")
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.State != TaskStateCanceled {
			t.Errorf("state task State = %q, want %q", got.State, TaskStateCanceled)
		}
	})
}

func (f *taskRouteAuthFixture) mustMarshalStoredTask(t *testing.T, taskID string, caller *CallerIdentity) []byte {
	t.Helper()
	ctx := WithRouteInfo(context.Background(), RouteInfo{ProjectSlug: "proj1", AgentSlug: "agent1"})
	if caller != nil {
		ctx = WithCallerIdentity(ctx, caller)
	}
	stored, err := f.taskStore.Get(ctx, a2a.TaskID(taskID))
	if err != nil {
		t.Fatalf("taskStore.Get(%s): %v", taskID, err)
	}
	raw, err := json.Marshal(stored.Task)
	if err != nil {
		t.Fatalf("marshal task %s: %v", taskID, err)
	}
	return raw
}

func assertFederationAgentRefusalJSONRPC(t *testing.T, status int, body []byte, forbiddenSubstrings ...string) {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 JSON-RPC envelope; body: %s", status, body)
	}
	bodyStr := string(body)
	if !strings.Contains(bodyStr, `"error"`) {
		t.Fatalf("expected JSON-RPC error, got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "-31403") {
		t.Errorf("expected unauthorized code -31403, got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "forbidden: task route is not available for federation agent callers") {
		t.Errorf("expected fixed error message, got: %s", bodyStr)
	}
	for _, s := range forbiddenSubstrings {
		if s != "" && strings.Contains(bodyStr, s) {
			t.Errorf("response body must not include %q: %s", s, bodyStr)
		}
	}
}

func assertFederationAgentRefusalV0REST(t *testing.T, streaming bool, status int, body []byte, forbiddenSubstrings ...string) {
	t.Helper()
	bodyStr := string(body)
	if streaming {
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200 SSE stream; body: %s", status, bodyStr)
		}
		if !strings.Contains(bodyStr, `"code":403`) || !strings.Contains(bodyStr, `"PERMISSION_DENIED"`) {
			t.Errorf("expected 403 PERMISSION_DENIED error event, got: %s", bodyStr)
		}
	} else if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body: %s", status, bodyStr)
	}
	if !strings.Contains(bodyStr, "forbidden: task route is not available for federation agent callers") {
		t.Errorf("expected fixed error message, got: %s", bodyStr)
	}
	for _, s := range forbiddenSubstrings {
		if s != "" && strings.Contains(bodyStr, s) {
			t.Errorf("response body must not include %q: %s", s, bodyStr)
		}
	}
}

func TestFederationTaskRoutesAuth_ExistingTaskCancelAndFollowUpRefused(t *testing.T) {
	f := newTaskRouteAuthFixture(t, "federation", "", true)
	f.mu.Lock()
	f.hubSendCode = http.StatusUnauthorized
	f.mu.Unlock()

	agentBCaller := &CallerIdentity{
		TokenType: "federation",
		AgentID:   "agent-b",
		IssuerURL: "https://hub-a.example.com",
	}

	seededTasks := []struct {
		name   string
		taskID string
		state  a2a.TaskState
	}{
		{name: "working", taskID: "task-b-working", state: a2a.TaskStateWorking},
		{name: "input_required", taskID: "task-b-input", state: a2a.TaskStateInputRequired},
		{name: "completed", taskID: "task-b-completed", state: a2a.TaskStateCompleted},
		{name: "canceled", taskID: "task-b-canceled", state: a2a.TaskStateCanceled},
	}

	beforeSnapshots := make(map[string][]byte, len(seededTasks))
	for _, st := range seededTasks {
		f.seedTask(t, st.taskID, agentBCaller, st.state)
		beforeSnapshots[st.taskID] = f.mustMarshalStoredTask(t, st.taskID, agentBCaller)
	}

	targetCases := []struct {
		name   string
		taskID string
		seeded bool
	}{
		{name: "working", taskID: "task-b-working", seeded: true},
		{name: "input_required", taskID: "task-b-input", seeded: true},
		{name: "completed", taskID: "task-b-completed", seeded: true},
		{name: "canceled", taskID: "task-b-canceled", seeded: true},
		{name: "nonexistent", taskID: "task-b-missing", seeded: false},
	}

	agentBToken := buildTestJWT(t, map[string]any{
		"iss":        "https://hub-a.example.com",
		"sub":        "agent-b",
		"project_id": "proj-b",
	})
	headers := map[string]string{"Authorization": "Bearer " + agentBToken}

	assertUnchanged := func(t *testing.T) {
		t.Helper()
		f.mu.Lock()
		calls := f.hubSendCalls
		f.mu.Unlock()
		if calls != 0 {
			t.Errorf("hubSendCalls = %d, want 0", calls)
		}
		for _, st := range seededTasks {
			after := f.mustMarshalStoredTask(t, st.taskID, agentBCaller)
			if !bytes.Equal(beforeSnapshots[st.taskID], after) {
				t.Errorf("stored task %s modified:\nbefore: %s\nafter:  %s", st.taskID, beforeSnapshots[st.taskID], after)
			}
		}
	}

	for _, tc := range targetCases {
		t.Run("jsonrpc_cancel/"+tc.name, func(t *testing.T) {
			payload := `{"jsonrpc":"2.0","id":"1","method":"CancelTask","params":{"id":"` + tc.taskID + `"}}`
			status, body := f.doRequest(t, http.MethodPost, "/projects/proj1/agents/agent1/jsonrpc", []byte(payload), headers)
			assertFederationAgentRefusalJSONRPC(t, status, body, "seed-content-", tc.taskID)
			assertUnchanged(t)
		})

		t.Run("v0rest_cancel/"+tc.name, func(t *testing.T) {
			status, body := f.doRequest(t, http.MethodPost, "/projects/proj1/agents/agent1/tasks/"+tc.taskID+":cancel", nil, headers)
			assertFederationAgentRefusalV0REST(t, false, status, body, "seed-content-", "ctx-"+tc.taskID)
			assertUnchanged(t)
		})

		t.Run("jsonrpc_send_with_task_id/"+tc.name, func(t *testing.T) {
			payload := `{"jsonrpc":"2.0","id":"2","method":"SendMessage","params":{"message":{"messageId":"m-1","role":"ROLE_USER","taskId":"` + tc.taskID + `","parts":[{"text":"follow-up"}]}}}`
			status, body := f.doRequest(t, http.MethodPost, "/projects/proj1/agents/agent1/jsonrpc", []byte(payload), headers)
			assertFederationAgentRefusalJSONRPC(t, status, body, "seed-content-", tc.taskID)
			assertUnchanged(t)
		})

		t.Run("v0rest_send_with_task_id/"+tc.name, func(t *testing.T) {
			payload := `{"message":{"message_id":"m-2","role":"user","task_id":"` + tc.taskID + `","parts":[{"kind":"text","text":"follow-up"}]}}`
			status, body := f.doRequest(t, http.MethodPost, "/projects/proj1/agents/agent1/message:send", []byte(payload), headers)
			assertFederationAgentRefusalV0REST(t, false, status, body, "seed-content-", tc.taskID)
			assertUnchanged(t)
		})

		t.Run("jsonrpc_stream_with_task_id/"+tc.name, func(t *testing.T) {
			payload := `{"jsonrpc":"2.0","id":"3","method":"SendStreamingMessage","params":{"message":{"messageId":"m-3","role":"ROLE_USER","taskId":"` + tc.taskID + `","parts":[{"text":"follow-up-stream"}]}}}`
			status, body := f.doRequest(t, http.MethodPost, "/projects/proj1/agents/agent1/jsonrpc", []byte(payload), headers)
			assertFederationAgentRefusalJSONRPC(t, status, body, "seed-content-", tc.taskID)
			assertUnchanged(t)
		})

		t.Run("v0rest_stream_with_task_id/"+tc.name, func(t *testing.T) {
			payload := `{"message":{"message_id":"m-4","role":"user","task_id":"` + tc.taskID + `","parts":[{"kind":"text","text":"follow-up-stream"}]}}`
			status, body := f.doRequest(t, http.MethodPost, "/projects/proj1/agents/agent1/message:stream", []byte(payload), headers)
			assertFederationAgentRefusalV0REST(t, true, status, body, "seed-content-", tc.taskID)
			assertUnchanged(t)
		})
	}
}

func TestFederationTaskRoutesAuth_OpportunisticSweepSkippedForFederationAgent(t *testing.T) {
	agentToken := buildTestJWT(t, map[string]any{
		"iss":        "https://hub-a.example.com",
		"sub":        "agent-a",
		"project_id": "proj-a",
	})
	userToken := buildTestJWT(t, map[string]any{
		"iss":              "https://hub-a.example.com",
		"sub":              "user-sub-1",
		"email":            "visitor@example.com",
		"scion_token_kind": "user",
	})

	t.Run("jsonrpc", func(t *testing.T) {
		f := newTaskRouteAuthFixture(t, "federation", "", true)
		if got := f.bridge.lastSweptAt.Load(); got != 0 {
			t.Fatalf("initial lastSweptAt = %d, want 0", got)
		}

		// Federation agent request must not trigger opportunistic sweep.
		_, _ = f.doRequest(t, http.MethodPost, "/projects/proj1/agents/agent1/jsonrpc",
			[]byte(`{"jsonrpc":"2.0","id":"1","method":"ListTasks","params":{}}`),
			map[string]string{"Authorization": "Bearer " + agentToken})
		if got := f.bridge.lastSweptAt.Load(); got != 0 {
			t.Fatalf("lastSweptAt after federation agent JSON-RPC = %d, want 0", got)
		}

		// Verified federated user request triggers opportunistic sweep.
		status, _ := f.doRequest(t, http.MethodPost, "/projects/proj1/agents/agent1/jsonrpc",
			[]byte(`{"jsonrpc":"2.0","id":"2","method":"ListTasks","params":{}}`),
			map[string]string{"Authorization": "Bearer " + userToken})
		if status != http.StatusOK {
			t.Fatalf("verified federated user JSON-RPC status = %d, want 200", status)
		}
		if got := f.bridge.lastSweptAt.Load(); got == 0 {
			t.Fatalf("lastSweptAt after verified federated user JSON-RPC = 0, want non-zero")
		}
	})

	t.Run("v0rest", func(t *testing.T) {
		f := newTaskRouteAuthFixture(t, "federation", "", true)
		if got := f.bridge.lastSweptAt.Load(); got != 0 {
			t.Fatalf("initial lastSweptAt = %d, want 0", got)
		}

		// Federation agent request must not trigger opportunistic sweep.
		_, _ = f.doRequest(t, http.MethodGet, "/projects/proj1/agents/agent1/tasks", nil,
			map[string]string{"Authorization": "Bearer " + agentToken})
		if got := f.bridge.lastSweptAt.Load(); got != 0 {
			t.Fatalf("lastSweptAt after federation agent v0 REST = %d, want 0", got)
		}

		// Verified federated user request triggers opportunistic sweep.
		status, _ := f.doRequest(t, http.MethodGet, "/projects/proj1/agents/agent1/tasks", nil,
			map[string]string{"Authorization": "Bearer " + userToken})
		if status != http.StatusOK {
			t.Fatalf("verified federated user v0 REST status = %d, want 200", status)
		}
		if got := f.bridge.lastSweptAt.Load(); got == 0 {
			t.Fatalf("lastSweptAt after verified federated user v0 REST = 0, want non-zero")
		}
	})

	t.Run("non_federation_scheme_runs_sweep", func(t *testing.T) {
		f := newTaskRouteAuthFixture(t, "apiKey", "secret-key", true)
		status, _ := f.doRequest(t, http.MethodGet, "/projects/proj1/agents/agent1/tasks", nil,
			map[string]string{"X-API-Key": "secret-key"})
		if status != http.StatusOK {
			t.Fatalf("apiKey v0 REST status = %d, want 200", status)
		}
		if got := f.bridge.lastSweptAt.Load(); got == 0 {
			t.Fatalf("lastSweptAt after apiKey v0 REST = 0, want non-zero")
		}
	})
}

func TestFederationTaskRoutesAuth_AllHandlerMethodsClassifiedAndEnforced(t *testing.T) {
	handlerType := reflect.TypeFor[a2asrv.RequestHandler]()
	interfaceMethods := make(map[string]struct{}, handlerType.NumMethod())
	for i := 0; i < handlerType.NumMethod(); i++ {
		name := handlerType.Method(i).Name
		interfaceMethods[name] = struct{}{}
		if _, ok := federationAgentMethodAllowList[name]; !ok {
			t.Errorf("a2asrv.RequestHandler method %q is not classified in federationAgentMethodAllowList", name)
		}
	}
	for name := range federationAgentMethodAllowList {
		if _, ok := interfaceMethods[name]; !ok {
			t.Errorf("federationAgentMethodAllowList contains unknown method %q not on a2asrv.RequestHandler", name)
		}
	}

	type transportSpec struct {
		jsonrpcPayload string
		restMethod     string
		restPath       string
		restBody       string
		restStreaming  bool
	}
	type methodCase struct {
		policy  federationAgentMethodPolicy
		denied  transportSpec
		newTask *transportSpec // set only for federationAgentAllowNewTaskOnly
	}

	const seededID = "task-complete-1"
	methodTable := map[string]methodCase{
		"GetTask": {
			policy: federationAgentDeny,
			denied: transportSpec{
				jsonrpcPayload: `{"jsonrpc":"2.0","id":"1","method":"GetTask","params":{"id":"` + seededID + `"}}`,
				restMethod:     http.MethodGet,
				restPath:       "/projects/proj1/agents/agent1/tasks/" + seededID,
			},
		},
		"ListTasks": {
			policy: federationAgentDeny,
			denied: transportSpec{
				jsonrpcPayload: `{"jsonrpc":"2.0","id":"2","method":"ListTasks","params":{}}`,
				restMethod:     http.MethodGet,
				restPath:       "/projects/proj1/agents/agent1/tasks",
			},
		},
		"CancelTask": {
			policy: federationAgentDeny,
			denied: transportSpec{
				jsonrpcPayload: `{"jsonrpc":"2.0","id":"3","method":"CancelTask","params":{"id":"` + seededID + `"}}`,
				restMethod:     http.MethodPost,
				restPath:       "/projects/proj1/agents/agent1/tasks/" + seededID + ":cancel",
			},
		},
		"SubscribeToTask": {
			policy: federationAgentDeny,
			denied: transportSpec{
				jsonrpcPayload: `{"jsonrpc":"2.0","id":"4","method":"SubscribeToTask","params":{"id":"` + seededID + `"}}`,
				restMethod:     http.MethodPost,
				restPath:       "/projects/proj1/agents/agent1/tasks/" + seededID + ":subscribe",
				restStreaming:  true,
			},
		},
		"GetTaskPushConfig": {
			policy: federationAgentDeny,
			denied: transportSpec{
				jsonrpcPayload: `{"jsonrpc":"2.0","id":"5","method":"GetTaskPushNotificationConfig","params":{"taskId":"` + seededID + `","id":"cfg-1"}}`,
				restMethod:     http.MethodGet,
				restPath:       "/projects/proj1/agents/agent1/tasks/" + seededID + "/pushNotificationConfigs/cfg-1",
			},
		},
		"ListTaskPushConfigs": {
			policy: federationAgentDeny,
			denied: transportSpec{
				jsonrpcPayload: `{"jsonrpc":"2.0","id":"6","method":"ListTaskPushNotificationConfigs","params":{"taskId":"` + seededID + `"}}`,
				restMethod:     http.MethodGet,
				restPath:       "/projects/proj1/agents/agent1/tasks/" + seededID + "/pushNotificationConfigs",
			},
		},
		"CreateTaskPushConfig": {
			policy: federationAgentDeny,
			denied: transportSpec{
				jsonrpcPayload: `{"jsonrpc":"2.0","id":"7","method":"CreateTaskPushNotificationConfig","params":{"taskId":"` + seededID + `","url":"https://callback.example.com"}}`,
				restMethod:     http.MethodPost,
				restPath:       "/projects/proj1/agents/agent1/tasks/" + seededID + "/pushNotificationConfigs",
				restBody:       `{"url":"https://callback.example.com"}`,
			},
		},
		"DeleteTaskPushConfig": {
			policy: federationAgentDeny,
			denied: transportSpec{
				jsonrpcPayload: `{"jsonrpc":"2.0","id":"8","method":"DeleteTaskPushNotificationConfig","params":{"taskId":"` + seededID + `","id":"cfg-1"}}`,
				restMethod:     http.MethodDelete,
				restPath:       "/projects/proj1/agents/agent1/tasks/" + seededID + "/pushNotificationConfigs/cfg-1",
			},
		},
		"GetExtendedAgentCard": {
			policy: federationAgentDeny,
			denied: transportSpec{
				jsonrpcPayload: `{"jsonrpc":"2.0","id":"9","method":"GetExtendedAgentCard","params":{}}`,
				restMethod:     http.MethodGet,
				restPath:       "/projects/proj1/agents/agent1/extendedAgentCard",
			},
		},
		"SendMessage": {
			policy: federationAgentAllowNewTaskOnly,
			denied: transportSpec{
				jsonrpcPayload: `{"jsonrpc":"2.0","id":"10","method":"SendMessage","params":{"message":{"messageId":"m-10","role":"ROLE_USER","taskId":"` + seededID + `","parts":[{"text":"hi"}]}}}`,
				restMethod:     http.MethodPost,
				restPath:       "/projects/proj1/agents/agent1/message:send",
				restBody:       `{"message":{"message_id":"m-10r","role":"user","task_id":"` + seededID + `","parts":[{"kind":"text","text":"hi"}]}}`,
			},
			newTask: &transportSpec{
				jsonrpcPayload: `{"jsonrpc":"2.0","id":"11","method":"SendMessage","params":{"message":{"messageId":"m-11","role":"ROLE_USER","parts":[{"text":"new task"}]}}}`,
				restMethod:     http.MethodPost,
				restPath:       "/projects/proj1/agents/agent1/message:send",
				restBody:       `{"message":{"message_id":"m-11r","role":"user","parts":[{"kind":"text","text":"new task"}]}}`,
			},
		},
		"SendStreamingMessage": {
			policy: federationAgentAllowNewTaskOnly,
			denied: transportSpec{
				jsonrpcPayload: `{"jsonrpc":"2.0","id":"12","method":"SendStreamingMessage","params":{"message":{"messageId":"m-12","role":"ROLE_USER","taskId":"` + seededID + `","parts":[{"text":"hi"}]}}}`,
				restMethod:     http.MethodPost,
				restPath:       "/projects/proj1/agents/agent1/message:stream",
				restBody:       `{"message":{"message_id":"m-12r","role":"user","task_id":"` + seededID + `","parts":[{"kind":"text","text":"hi"}]}}`,
				restStreaming:  true,
			},
			newTask: &transportSpec{
				jsonrpcPayload: `{"jsonrpc":"2.0","id":"13","method":"SendStreamingMessage","params":{"message":{"messageId":"m-13","role":"ROLE_USER","parts":[{"text":"new stream task"}]}}}`,
				restMethod:     http.MethodPost,
				restPath:       "/projects/proj1/agents/agent1/message:stream",
				restBody:       `{"message":{"message_id":"m-13r","role":"user","parts":[{"kind":"text","text":"new stream task"}]}}`,
				restStreaming:  true,
			},
		},
	}

	// Ensure every interface method is covered by the transport test matrix.
	for name := range interfaceMethods {
		tc, ok := methodTable[name]
		if !ok {
			t.Fatalf("a2asrv.RequestHandler method %q is missing from transport test table", name)
		}
		if got := federationAgentMethodAllowList[name]; got != tc.policy {
			t.Errorf("policy for %q = %v, want %v", name, got, tc.policy)
		}
	}

	f := newTaskRouteAuthFixture(t, "federation", "", true)
	f.mu.Lock()
	f.hubSendCode = http.StatusUnauthorized
	f.mu.Unlock()

	agentCaller := &CallerIdentity{
		TokenType: "federation",
		AgentID:   "agent-a",
		IssuerURL: "https://hub-a.example.com",
	}
	f.seedTask(t, seededID, agentCaller, a2a.TaskStateWorking)
	beforeSnapshot := f.mustMarshalStoredTask(t, seededID, agentCaller)

	agentToken := buildTestJWT(t, map[string]any{
		"iss":        "https://hub-a.example.com",
		"sub":        "agent-a",
		"project_id": "proj-a",
	})
	headers := map[string]string{"Authorization": "Bearer " + agentToken}

	for name, tc := range methodTable {
		t.Run(name+"/denied", func(t *testing.T) {
			f.mu.Lock()
			beforeCalls := f.hubSendCalls
			f.mu.Unlock()

			status, body := f.doRequest(t, http.MethodPost, "/projects/proj1/agents/agent1/jsonrpc",
				[]byte(tc.denied.jsonrpcPayload), headers)
			assertFederationAgentRefusalJSONRPC(t, status, body, "seed-content-", seededID)

			var restBody []byte
			if tc.denied.restBody != "" {
				restBody = []byte(tc.denied.restBody)
			}
			status, body = f.doRequest(t, tc.denied.restMethod, tc.denied.restPath, restBody, headers)
			assertFederationAgentRefusalV0REST(t, tc.denied.restStreaming, status, body, "seed-content-", "ctx-"+seededID)

			f.mu.Lock()
			afterCalls := f.hubSendCalls
			f.mu.Unlock()
			if afterCalls != beforeCalls {
				t.Errorf("%s denied path made %d Hub send calls, want 0", name, afterCalls-beforeCalls)
			}
			afterSnapshot := f.mustMarshalStoredTask(t, seededID, agentCaller)
			if !bytes.Equal(beforeSnapshot, afterSnapshot) {
				t.Errorf("%s modified seeded task:\nbefore: %s\nafter:  %s", name, beforeSnapshot, afterSnapshot)
			}
		})

		if tc.newTask != nil {
			t.Run(name+"/new_task_reaches_hub", func(t *testing.T) {
				f.mu.Lock()
				beforeCalls := f.hubSendCalls
				f.mu.Unlock()

				status, body := f.doRequest(t, http.MethodPost, "/projects/proj1/agents/agent1/jsonrpc",
					[]byte(tc.newTask.jsonrpcPayload), headers)
				if status != http.StatusOK {
					t.Fatalf("JSON-RPC new-task status = %d, want 200; body: %s", status, body)
				}
				if strings.Contains(string(body), "forbidden: task route is not available for federation agent callers") {
					t.Errorf("JSON-RPC new-task %s should not be blocked by task-route gate: %s", name, body)
				}

				status, body = f.doRequest(t, tc.newTask.restMethod, tc.newTask.restPath,
					[]byte(tc.newTask.restBody), headers)
				if status != http.StatusOK {
					t.Fatalf("v0 REST new-task status = %d, want 200; body: %s", status, body)
				}
				if strings.Contains(string(body), "forbidden: task route is not available for federation agent callers") {
					t.Errorf("v0 REST new-task %s should not be blocked by task-route gate: %s", name, body)
				}

				f.mu.Lock()
				afterCalls := f.hubSendCalls
				f.mu.Unlock()
				if afterCalls-beforeCalls != 2 {
					t.Errorf("%s new-task Hub send calls = %d, want 2 (JSON-RPC + v0 REST)", name, afterCalls-beforeCalls)
				}
			})
		}
	}
}

func TestFederationTaskRoutesAuth_FederationAgentDoesNotAutoProvision(t *testing.T) {
	f := newTaskRouteAuthFixture(t, "federation", "", true)
	f.bridge.config.Projects = []ProjectConfig{
		{
			Slug:            "proj1",
			AutoProvision:   true,
			DefaultTemplate: "default-tmpl",
		},
	}

	agentToken := buildTestJWT(t, map[string]any{
		"iss":        "https://hub-a.example.com",
		"sub":        "agent-a",
		"project_id": "proj-a",
	})
	headers := map[string]string{"Authorization": "Bearer " + agentToken}

	t.Run("jsonrpc", func(t *testing.T) {
		payload := `{"jsonrpc":"2.0","id":"ap1","method":"SendMessage","params":{"message":{"messageId":"m-ap1","role":"ROLE_USER","parts":[{"text":"hello"}]}}}`
		status, body := f.doRequest(t, http.MethodPost, "/projects/proj1/agents/brand-new-agent/jsonrpc", []byte(payload), headers)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", status, body)
		}
		f.mu.Lock()
		creates := f.hubCreateCalls
		sends := f.hubSendCalls
		f.mu.Unlock()
		if creates != 0 {
			t.Errorf("hubCreateCalls = %d, want 0 (federation agent must not trigger auto-provisioning)", creates)
		}
		if sends != 0 {
			t.Errorf("hubSendCalls = %d, want 0", sends)
		}
	})

	t.Run("v0rest", func(t *testing.T) {
		payload := `{"message":{"message_id":"m-ap2","role":"user","parts":[{"kind":"text","text":"hello"}]}}`
		_, _ = f.doRequest(t, http.MethodPost, "/projects/proj1/agents/brand-new-agent/message:send", []byte(payload), headers)
		f.mu.Lock()
		creates := f.hubCreateCalls
		sends := f.hubSendCalls
		f.mu.Unlock()
		if creates != 0 {
			t.Errorf("hubCreateCalls = %d, want 0 (federation agent must not trigger auto-provisioning)", creates)
		}
		if sends != 0 {
			t.Errorf("hubSendCalls = %d, want 0", sends)
		}
	})
}

func TestFederationTaskRoutesAuth_DurableHandlerSubscribeRefusedBeforeStore(t *testing.T) {
	// Wrap a DurableRequestHandler with nil stores: if SubscribeToTask fell
	// through to DurableRequestHandler, it would hit the nil sdkStore instead
	// of returning errFederationTaskRouteUnavailable.
	durable := NewDurableRequestHandler(nil, nil, nil, nil)
	guarded := NewFederationTaskAuthHandler(durable)

	agentCaller := &CallerIdentity{
		TokenType: "federation",
		AgentID:   "agent-a",
		IssuerURL: "https://hub-a.example.com",
	}
	ctx := WithCallerIdentity(
		WithRouteInfo(context.Background(), RouteInfo{ProjectSlug: "proj1", AgentSlug: "agent1"}),
		agentCaller,
	)

	var gotErr error
	for _, err := range guarded.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: "task-durable-1"}) {
		gotErr = err
	}
	if gotErr != errFederationTaskRouteUnavailable {
		t.Fatalf("SubscribeToTask err = %v, want %v", gotErr, errFederationTaskRouteUnavailable)
	}

	// Verify the same refusal over the JSON-RPC transport wired as in standalone mode.
	jsonrpcHandler := a2asrv.NewJSONRPCHandler(guarded)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","id":"d1","method":"SubscribeToTask","params":{"id":"task-durable-1"}}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	jsonrpcHandler.ServeHTTP(w, req)

	assertFederationAgentRefusalJSONRPC(t, w.Code, w.Body.Bytes(), "task-durable-1")
}
