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
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

func TestGenerateAgentCard_OAuthScheme(t *testing.T) {
	cfg := &Config{
		Bridge: BridgeConfig{
			ExternalURL: "https://bridge.example.com",
		},
		Auth: AuthConfig{
			Scheme: "oauth",
			OAuth: OAuthConfig{
				AuthorizationURL: "https://accounts.google.com/o/oauth2/v2/auth",
				TokenURL:         "https://oauth2.googleapis.com/token",
				Scopes:           []string{"openid", "email", "profile"},
			},
		},
		Projects: []ProjectConfig{
			{
				Slug:          "default",
				ExposedAgents: []string{"assistant"},
			},
		},
	}

	b := &Bridge{
		config:     cfg,
		agentCache: make(map[string]*agentCacheEntry),
		log:        slog.Default(),
	}

	card := b.GenerateAgentCard(context.Background(), "default", "assistant")

	if got := card["preferredTransport"]; got != "JSONRPC" {
		t.Errorf("expected preferredTransport=JSONRPC, got %v", got)
	}

	if got := card["url"]; got != "https://bridge.example.com/projects/default/agents/assistant" {
		t.Errorf("unexpected card url: %v", got)
	}

	// Verify supportedInterfaces includes /jsonrpc URL.
	ifaces, ok := card["supportedInterfaces"].([]map[string]interface{})
	if !ok || len(ifaces) == 0 {
		t.Fatalf("expected supportedInterfaces, got %#v", card["supportedInterfaces"])
	}
	if ifaces[0]["url"] != "https://bridge.example.com/projects/default/agents/assistant/jsonrpc" {
		t.Errorf("expected interface url to be /jsonrpc, got %v", ifaces[0]["url"])
	}

	// Verify securitySchemes
	schemes, ok := card["securitySchemes"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected securitySchemes map, got %#v", card["securitySchemes"])
	}
	oauthScheme, ok := schemes["oauth2"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected oauth2 entry in securitySchemes, got %#v", schemes)
	}
	if oauthScheme["type"] != "oauth2" {
		t.Errorf("expected type=oauth2, got %v", oauthScheme["type"])
	}

	// Verify security requirements
	sec, ok := card["security"].([]map[string]interface{})
	if !ok || len(sec) == 0 {
		t.Fatalf("expected security slice, got %#v", card["security"])
	}
	scopes, ok := sec[0]["oauth2"].([]string)
	if !ok || len(scopes) != 3 {
		t.Errorf("expected 3 oauth2 scopes in security requirement, got %#v", sec[0])
	}
}

func TestServer_GE_DiscoveryAndJSONRPC_Routes(t *testing.T) {
	var hits atomic.Int32
	mockUserInfo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		auth := r.Header.Get("Authorization")
		if auth == "Bearer valid-oauth-token" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"sub":"ge-user-42","email":"ge-user@enterprise.com"}`))
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer mockUserInfo.Close()

	cfg := &Config{
		Bridge: BridgeConfig{
			ExternalURL: "https://bridge.example.com",
		},
		Hub: HubConfig{
			Endpoint: "http://localhost:8080",
			User:     "admin",
		},
		Auth: AuthConfig{
			Scheme: "oauth",
			OAuth: OAuthConfig{
				UserInfoURL: mockUserInfo.URL,
				CacheTTL:    time.Minute,
			},
		},
		Projects: []ProjectConfig{
			{
				Slug:          "default",
				ExposedAgents: []string{"assistant"},
			},
		},
	}

	b := &Bridge{
		config:     cfg,
		agentCache: make(map[string]*agentCacheEntry),
		log:        slog.Default(),
	}

	var capturedCaller *CallerIdentity
	var capturedRoute RouteInfo
	sdkHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedCaller = callerIdentityFromContext(r.Context())
		capturedRoute, _ = RouteInfoFrom(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{"status":"ok"}}`))
	})

	srv := NewServer(b, cfg, nil, slog.Default(), sdkHandler)
	handler := srv.Handler()

	// 1. Top-level /.well-known/agent.json must be publicly accessible without OAuth token.
	req := httptest.NewRequest(http.MethodGet, "/.well-known/agent.json", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /.well-known/agent.json returned %d, expected 200", rec.Code)
	}

	// 2. Per-agent /.well-known/agent.json (used by Gemini Enterprise / ADK) must be publicly accessible.
	req = httptest.NewRequest(http.MethodGet, "/projects/default/agents/assistant/.well-known/agent.json", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /projects/default/agents/assistant/.well-known/agent.json returned %d, expected 200", rec.Code)
	}
	var card map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
		t.Fatalf("failed to unmarshal agent card: %v", err)
	}
	if card["securitySchemes"] == nil {
		t.Errorf("expected securitySchemes in returned agent card")
	}

	// 3. POST directly to agent_card.url (/projects/default/agents/assistant) without token -> 401.
	req = httptest.NewRequest(http.MethodPost, "/projects/default/agents/assistant", strings.NewReader(`{"jsonrpc":"2.0","method":"message/send","id":"1"}`))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST without token returned %d, expected 401", rec.Code)
	}

	// 4. POST to /projects/default/agents/assistant with standard Authorization: Bearer valid-oauth-token -> 200.
	req = httptest.NewRequest(http.MethodPost, "/projects/default/agents/assistant", strings.NewReader(`{"jsonrpc":"2.0","method":"message/send","id":"1"}`))
	req.Header.Set("Authorization", "Bearer valid-oauth-token")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST with valid Authorization Bearer returned %d: %s", rec.Code, rec.Body.String())
	}
	if capturedCaller == nil || capturedCaller.Email != "ge-user@enterprise.com" || capturedCaller.TokenType != "oauth" {
		t.Fatalf("unexpected capturedCaller: %#v", capturedCaller)
	}
	if capturedRoute.ProjectSlug != "default" || capturedRoute.AgentSlug != "assistant" {
		t.Fatalf("unexpected route info: %#v", capturedRoute)
	}

	// 5. Gemini Enterprise Cloud Run / Vertex Agent Engine header precedence:
	// Authorization carries the Cloud Run P4SA invoker token (which is invalid for UserInfo),
	// while X-Goog-Agent-User-Authorization carries the end-user OAuth token.
	capturedCaller = nil
	req = httptest.NewRequest(http.MethodPost, "/projects/default/agents/assistant", strings.NewReader(`{"jsonrpc":"2.0","method":"message/send","id":"2"}`))
	req.Header.Set("Authorization", "Bearer cloud-run-invoker-id-token")
	req.Header.Set(AgentUserAuthHeader, "Bearer valid-oauth-token")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST with X-Goog-Agent-User-Authorization returned %d: %s", rec.Code, rec.Body.String())
	}
	if capturedCaller == nil || capturedCaller.Email != "ge-user@enterprise.com" {
		t.Fatalf("expected X-Goog-Agent-User-Authorization to take precedence, got caller=%#v", capturedCaller)
	}

	// Verify caching: both requests with "valid-oauth-token" should have resulted in only 1 network call to mockUserInfo.
	if gotHits := hits.Load(); gotHits != 1 {
		t.Errorf("expected mockUserInfo to be called once due to caching, got %d hits", gotHits)
	}
}

func TestOAuthValidator_JWTDecodeMode(t *testing.T) {
	validator := NewOAuthValidator(OAuthConfig{
		UserInfoURL: "jwt_decode",
	})

	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"user-999","email":"jwtuser@example.com"}`))
	token := header + "." + payload + ".sig"

	identity, err := validator.Validate(context.Background(), token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if identity.UserID != "user-999" || identity.Email != "jwtuser@example.com" || identity.TokenType != "oauth" {
		t.Errorf("unexpected identity: %#v", identity)
	}
}

func TestEndToEnd_GeminiEnterprise_A2A_OAuth_MultiTurn(t *testing.T) {
	// 1. Mock OIDC UserInfo server supporting Alice and Bob OAuth tokens.
	mockUserInfo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer ge-oauth-token-alice":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"sub":"ge-user-alice","email":"alice@enterprise.com"}`))
		case "Bearer ge-oauth-token-bob":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"sub":"ge-user-bob","email":"bob@enterprise.com"}`))
		default:
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
	}))
	defer mockUserInfo.Close()

	// 2. Setup Bridge with SQLite state store and mock Hub client.
	dir := t.TempDir()
	store, err := state.NewSQLite(dir + "/e2e.db")
	if err != nil {
		t.Fatalf("state.NewSQLite: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	var lastSender atomic.Value
	var turnCount atomic.Int32

	var b *Bridge
	agents := &mockAgentService{
		listFn: func(ctx context.Context, opts *hubclient.ListAgentsOptions) (*hubclient.ListAgentsResponse, error) {
			return &hubclient.ListAgentsResponse{
				Agents: []hubclient.Agent{
					{ID: "agent-uuid-1", Name: "assistant", Slug: "assistant"},
				},
			}, nil
		},
		sendFn: func(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt, notify, wake bool) (*hubclient.MessageResponse, error) {
			lastSender.Store(msg.Sender)
			turn := turnCount.Add(1)
			taskID := msg.Metadata["a2aTaskId"]

			// Simulate agent replying asynchronously via message event with state transition.
			go func() {
				time.Sleep(10 * time.Millisecond)
				replyText := "Turn 1 reply from Scion agent"
				endState := TaskStateInputRequired
				if turn > 1 {
					replyText = "Turn 2 reply from Scion agent"
					endState = TaskStateCompleted
				}
				msgPayload, _ := json.Marshal(TaskStatusUpdate{
					TaskID: taskID,
					Status: TaskStatus{
						State: endState,
						Message: &Message{
							Role:  "agent",
							Parts: []Part{{Text: replyText}},
						},
					},
					Final: true,
				})
				_, _ = store.AppendTaskEvent(context.Background(), &state.TaskEvent{
					TaskID:  taskID,
					Kind:    "message",
					Payload: msgPayload,
					Final:   true,
				})
				_, _ = store.UpdateTaskState(context.Background(), taskID, endState)
			}()
			return &hubclient.MessageResponse{}, nil
		},
	}

	cfg := &Config{
		Bridge: BridgeConfig{
			ExternalURL: "https://bridge.example.com",
		},
		Hub: HubConfig{
			Endpoint: "http://localhost:8080",
			User:     "admin",
		},
		Auth: AuthConfig{
			Scheme: "oauth",
			OAuth: OAuthConfig{
				AuthorizationURL: "https://accounts.google.com/o/oauth2/v2/auth",
				TokenURL:         "https://oauth2.googleapis.com/token",
				Scopes:           []string{"openid", "email", "profile"},
				UserInfoURL:      mockUserInfo.URL,
				CacheTTL:         time.Minute,
			},
		},
		Timeouts: TimeoutConfig{
			SendMessage: 5 * time.Second,
		},
		Projects: []ProjectConfig{
			{
				Slug:          "default",
				ExposedAgents: []string{"assistant"},
			},
		},
	}

	log := slog.Default()
	b = New(store, &mockHubClient{agents: agents}, nil, cfg, nil, log)
	t.Cleanup(func() { b.Shutdown() })

	executor := NewScionExecutor(b, log)
	routeAuth := RouteKeyAuthenticator()
	innerStore := taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{
		Authenticator: routeAuth,
	})
	scopedStore := NewScopedTaskStore(innerStore)
	sdkRequestHandler := a2asrv.NewHandler(
		executor,
		a2asrv.WithLogger(log),
		a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{
			Streaming:         true,
			PushNotifications: false,
		}),
		a2asrv.WithTaskStore(scopedStore),
	)
	b.SetSDKRequestHandler(sdkRequestHandler)
	sdkJSONRPCHandler := a2asrv.NewJSONRPCHandler(sdkRequestHandler)
	srv := NewServer(b, cfg, nil, log, sdkJSONRPCHandler)
	handler := srv.Handler()

	// Step A: GE discovers /.well-known/agent.json (unauthenticated).
	discReq := httptest.NewRequest(http.MethodGet, "/projects/default/agents/assistant/.well-known/agent.json", nil)
	discRec := httptest.NewRecorder()
	handler.ServeHTTP(discRec, discReq)
	if discRec.Code != http.StatusOK {
		t.Fatalf("discovery GET /.well-known/agent.json failed: %d", discRec.Code)
	}

	// Step B: Turn 1 — GE sends POST directly to agent_card.url (/projects/default/agents/assistant)
	// with Cloud Run invoker token in Authorization and Alice's OAuth token in X-Goog-Agent-User-Authorization.
	turn1Body := `{"jsonrpc":"2.0","id":"turn-1","method":"message/send","params":{"message":{"role":"user","parts":[{"type":"text","text":"Hello turn 1"}]}}}`
	t1Req := httptest.NewRequest(http.MethodPost, "/projects/default/agents/assistant", strings.NewReader(turn1Body))
	t1Req.Header.Set("Content-Type", "application/json")
	t1Req.Header.Set("Authorization", "Bearer cloud-run-p4sa-invoker-id-token")
	t1Req.Header.Set(AgentUserAuthHeader, "Bearer ge-oauth-token-alice")
	t1Rec := httptest.NewRecorder()
	handler.ServeHTTP(t1Rec, t1Req)

	if t1Rec.Code != http.StatusOK {
		t.Fatalf("Turn 1 POST returned HTTP %d: %s", t1Rec.Code, t1Rec.Body.String())
	}
	t.Logf("Turn 1 response body: %s", t1Rec.Body.String())
	if gotSender, _ := lastSender.Load().(string); gotSender != "user:alice@enterprise.com" {
		t.Fatalf("Turn 1 expected Hub message Sender=user:alice@enterprise.com, got %q (body: %s)", gotSender, t1Rec.Body.String())
	}

	var t1Resp struct {
		Result struct {
			ID     string `json:"id"`
			Status struct {
				State   string `json:"state"`
				Message struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"message"`
			} `json:"status"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(t1Rec.Body.Bytes(), &t1Resp); err != nil {
		t.Fatalf("Turn 1 unmarshal response: %v (body: %s)", err, t1Rec.Body.String())
	}
	if t1Resp.Error != nil {
		t.Fatalf("Turn 1 returned JSON-RPC error: %+v", t1Resp.Error)
	}
	taskID := t1Resp.Result.ID
	if taskID == "" {
		t.Fatalf("Turn 1 response missing task ID: %s", t1Rec.Body.String())
	}
	if t1Resp.Result.Status.State != "input-required" {
		t.Errorf("Turn 1 expected state=input-required, got %q", t1Resp.Result.Status.State)
	}
	if len(t1Resp.Result.Status.Message.Parts) == 0 || t1Resp.Result.Status.Message.Parts[0].Text != "Turn 1 reply from Scion agent" {
		t.Errorf("Turn 1 expected artifact reply text, got %+v", t1Resp.Result.Status.Message)
	}

	// Step C: Multi-turn user isolation check — Bob attempts to send Turn 2 on Alice's taskID -> must be rejected.
	bobBody := `{"jsonrpc":"2.0","id":"turn-bob","method":"message/send","params":{"id":"` + taskID + `","message":{"role":"user","parts":[{"type":"text","text":"Hijack attempt"}]}}}`
	bobReq := httptest.NewRequest(http.MethodPost, "/projects/default/agents/assistant", strings.NewReader(bobBody))
	bobReq.Header.Set("Content-Type", "application/json")
	bobReq.Header.Set(AgentUserAuthHeader, "Bearer ge-oauth-token-bob")
	bobRec := httptest.NewRecorder()
	handler.ServeHTTP(bobRec, bobReq)
	if !strings.Contains(bobRec.Body.String(), "error") {
		t.Fatalf("expected JSON-RPC error when Bob accesses Alice's task, got: %s", bobRec.Body.String())
	}

	// Step D: Turn 2 — Alice sends follow-up message on the same taskID -> succeeds and returns Turn 2 reply.
	turn2Body := `{"jsonrpc":"2.0","id":"turn-2","method":"message/send","params":{"id":"` + taskID + `","message":{"role":"user","parts":[{"type":"text","text":"Follow-up turn 2"}]}}}`
	t2Req := httptest.NewRequest(http.MethodPost, "/projects/default/agents/assistant", strings.NewReader(turn2Body))
	t2Req.Header.Set("Content-Type", "application/json")
	t2Req.Header.Set("Authorization", "Bearer cloud-run-p4sa-invoker-id-token")
	t2Req.Header.Set(AgentUserAuthHeader, "Bearer ge-oauth-token-alice")
	t2Rec := httptest.NewRecorder()
	handler.ServeHTTP(t2Rec, t2Req)

	if t2Rec.Code != http.StatusOK {
		t.Fatalf("Turn 2 POST returned HTTP %d: %s", t2Rec.Code, t2Rec.Body.String())
	}
	var t2Resp struct {
		Result struct {
			ID     string `json:"id"`
			Status struct {
				State   string `json:"state"`
				Message struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"message"`
			} `json:"status"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(t2Rec.Body.Bytes(), &t2Resp); err != nil {
		t.Fatalf("Turn 2 unmarshal response: %v", err)
	}
	if t2Resp.Error != nil {
		t.Fatalf("Turn 2 returned JSON-RPC error: %+v", t2Resp.Error)
	}
	if t2Resp.Result.Status.State != "completed" {
		t.Errorf("Turn 2 expected state=completed, got %q", t2Resp.Result.Status.State)
	}
	if len(t2Resp.Result.Status.Message.Parts) == 0 || t2Resp.Result.Status.Message.Parts[0].Text != "Turn 2 reply from Scion agent" {
		t.Errorf("Turn 2 expected 'Turn 2 reply from Scion agent', got %+v", t2Resp.Result.Status.Message)
	}

	// Step E: Verify A2A v1.0 JSON-RPC ("SendMessage") also works seamlessly on the same endpoint.
	v1Body := `{"jsonrpc":"2.0","id":"turn-v1","method":"SendMessage","params":{"message":{"messageId":"m-v1","role":"user","parts":[{"text":"Hello v1.0"}]}}}`
	v1Req := httptest.NewRequest(http.MethodPost, "/projects/default/agents/assistant", strings.NewReader(v1Body))
	v1Req.Header.Set("Content-Type", "application/json")
	v1Req.Header.Set(AgentUserAuthHeader, "Bearer ge-oauth-token-alice")
	v1Rec := httptest.NewRecorder()
	handler.ServeHTTP(v1Rec, v1Req)
	if v1Rec.Code != http.StatusOK || !strings.Contains(v1Rec.Body.String(), `"task"`) {
		t.Errorf("expected A2A v1.0 SendMessage response with 'task' envelope, got HTTP %d: %s", v1Rec.Code, v1Rec.Body.String())
	}
}

