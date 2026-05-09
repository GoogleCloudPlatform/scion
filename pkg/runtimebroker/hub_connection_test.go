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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// makeTestCreds creates BrokerCredentials with a base64-encoded secret key.
func makeTestCreds(name, brokerID, hubEndpoint string) *brokercredentials.BrokerCredentials {
	secretKey := base64.StdEncoding.EncodeToString([]byte("test-secret-key-32bytes!" + name + "12"))
	return &brokercredentials.BrokerCredentials{
		Name:         name,
		BrokerID:     brokerID,
		SecretKey:    secretKey,
		HubEndpoint:  hubEndpoint,
		AuthMode:     brokercredentials.AuthModeHMAC,
		RegisteredAt: time.Now(),
	}
}

// newTestServerWithInMemoryCreds creates a server with in-memory credentials for a "local" connection.
func newTestServerWithInMemoryCreds(creds *brokercredentials.BrokerCredentials) *Server {
	cfg := DefaultServerConfig()
	cfg.BrokerID = creds.BrokerID
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = creds.HubEndpoint
	cfg.InMemoryCredentials = creds
	// Most tests in this file focus on hub connection behavior, not auth gates.
	cfg.BrokerAuthEnabled = false
	cfg.ForceRuntime = "mock"

	mgr := &mockManager{}
	// NameFunc returns "docker" so resolveManagerForOpts matches the settings-resolved runtime.
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}

	return New(cfg, mgr, rt)
}

func TestHubConnection_SingleConnection(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")

	srv := newTestServerWithInMemoryCreds(creds)

	srv.hubMu.RLock()
	defer srv.hubMu.RUnlock()

	if len(srv.hubConnections) != 1 {
		t.Fatalf("expected 1 hub connection, got %d", len(srv.hubConnections))
	}

	conn, ok := srv.hubConnections["local"]
	if !ok {
		t.Fatal("expected 'local' connection to exist")
	}

	if conn.BrokerID != "broker-1" {
		t.Errorf("expected BrokerID 'broker-1', got %q", conn.BrokerID)
	}

	if conn.HubEndpoint != "http://localhost:8080" {
		t.Errorf("expected HubEndpoint 'http://localhost:8080', got %q", conn.HubEndpoint)
	}

	if conn.HubClient == nil {
		t.Error("expected HubClient to be set")
	}

	if len(conn.SecretKey) == 0 {
		t.Error("expected SecretKey to be decoded")
	}
}

func TestHubConnection_MultipleConnections(t *testing.T) {
	// Create a server with InMemory ("local") + MultiStore credentials
	tmpDir := t.TempDir()
	credDir := filepath.Join(tmpDir, "hub-credentials")
	if err := os.MkdirAll(credDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Write a second credential file
	prodCreds := makeTestCreds("hub-prod", "broker-prod", "https://hub.prod.example.com")
	prodData, _ := json.MarshalIndent(prodCreds, "", "  ")
	if err := os.WriteFile(filepath.Join(credDir, "hub-prod.json"), prodData, 0600); err != nil {
		t.Fatal(err)
	}

	localCreds := makeTestCreds("local", "broker-local", "http://localhost:8080")

	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-local"
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://localhost:8080"
	cfg.InMemoryCredentials = localCreds

	mgr := &mockManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	// The multi-store was initialized but pointed to default dir.
	// Manually set up the multi-store and add the prod connection.
	srv.multiCredStore = brokercredentials.NewMultiStore(credDir)
	multiCreds, _ := srv.multiCredStore.List()
	for i := range multiCreds {
		c := &multiCreds[i]
		if _, exists := srv.hubConnections[c.Name]; exists {
			continue
		}
		conn, err := srv.createHubConnection(c.Name, c)
		if err != nil {
			t.Fatalf("Failed to create connection %q: %v", c.Name, err)
		}
		srv.hubMu.Lock()
		srv.hubConnections[c.Name] = conn
		srv.hubMu.Unlock()
	}

	srv.hubMu.RLock()
	defer srv.hubMu.RUnlock()

	if len(srv.hubConnections) != 2 {
		t.Fatalf("expected 2 hub connections, got %d", len(srv.hubConnections))
	}

	if _, ok := srv.hubConnections["local"]; !ok {
		t.Error("expected 'local' connection")
	}
	if _, ok := srv.hubConnections["hub-prod"]; !ok {
		t.Error("expected 'hub-prod' connection")
	}
}

func TestMultiKeyBrokerAuth_MatchesAnyKey(t *testing.T) {
	secret1 := []byte("secret-key-for-hub-1-32bytes!!!!")
	secret2 := []byte("secret-key-for-hub-2-32bytes!!!!")

	middleware := NewMultiKeyBrokerAuthMiddleware(true, 5*time.Minute, false)
	middleware.UpdateKeys([]secretKeyEntry{
		{hubName: "hub-1", secretKey: secret1},
		{hubName: "hub-2", secretKey: secret2},
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Request signed with secret1 should pass
	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	signRequest(req1, "broker-1", secret1)
	rr1 := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr1, req1)

	if rr1.Code != http.StatusOK {
		t.Errorf("Expected 200 for request signed with hub-1 key, got %d", rr1.Code)
	}

	// Request signed with secret2 should also pass
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	signRequest(req2, "broker-1", secret2)
	rr2 := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusOK {
		t.Errorf("Expected 200 for request signed with hub-2 key, got %d", rr2.Code)
	}

	// Request signed with unknown key should fail
	wrongSecret := []byte("wrong-secret-key-32bytes!!!!!!!!")
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	signRequest(req3, "broker-1", wrongSecret)
	rr3 := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr3, req3)

	if rr3.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 for request signed with wrong key, got %d", rr3.Code)
	}
}

func TestMultiKeyBrokerAuth_Disabled(t *testing.T) {
	middleware := NewMultiKeyBrokerAuthMiddleware(false, 5*time.Minute, false)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	rr := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected 200 when disabled, got %d", rr.Code)
	}
}

func TestMultiKeyBrokerAuth_AllowUnauthenticated(t *testing.T) {
	secret := []byte("test-secret-key-32bytes!12345678")
	middleware := NewMultiKeyBrokerAuthMiddleware(true, 5*time.Minute, true)
	middleware.UpdateKeys([]secretKeyEntry{
		{hubName: "hub-1", secretKey: secret},
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Request without any HMAC headers should pass
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	rr := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected 200 for unauthenticated request, got %d", rr.Code)
	}
}

func TestMultiKeyBrokerAuth_UpdateKeys(t *testing.T) {
	oldSecret := []byte("old-secret-key-32bytes!123456789")
	newSecret := []byte("new-secret-key-32bytes!987654321")

	middleware := NewMultiKeyBrokerAuthMiddleware(true, 5*time.Minute, false)
	middleware.UpdateKeys([]secretKeyEntry{
		{hubName: "hub-1", secretKey: oldSecret},
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Request with old key should work
	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	signRequest(req1, "broker-1", oldSecret)
	rr1 := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusOK {
		t.Errorf("Expected 200 with old key, got %d", rr1.Code)
	}

	// Update keys to new secret only
	middleware.UpdateKeys([]secretKeyEntry{
		{hubName: "hub-1", secretKey: newSecret},
	})

	// Request with old key should now fail
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	signRequest(req2, "broker-1", oldSecret)
	rr2 := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 with old key after update, got %d", rr2.Code)
	}

	// Request with new key should work
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	signRequest(req3, "broker-1", newSecret)
	rr3 := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr3, req3)
	if rr3.Code != http.StatusOK {
		t.Errorf("Expected 200 with new key, got %d", rr3.Code)
	}
}

func TestMultiKeyBrokerAuth_ExpiredTimestamp(t *testing.T) {
	secret := []byte("test-secret-key-32bytes!12345678")
	middleware := NewMultiKeyBrokerAuthMiddleware(true, 5*time.Minute, false)
	middleware.UpdateKeys([]secretKeyEntry{
		{hubName: "hub-1", secretKey: secret},
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Create a request with old timestamp
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	oldTimestamp := time.Now().Add(-10 * time.Minute).Unix()
	ts := fmt.Sprintf("%d", oldTimestamp)
	nonce := "test-nonce"
	req.Header.Set(apiclient.HeaderBrokerID, "broker-1")
	req.Header.Set(apiclient.HeaderTimestamp, ts)
	req.Header.Set(apiclient.HeaderNonce, nonce)
	canonical := apiclient.BuildCanonicalString(req, ts, nonce)
	sig := apiclient.ComputeHMAC(secret, canonical)
	req.Header.Set(apiclient.HeaderSignature, base64.StdEncoding.EncodeToString(sig))

	rr := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 for expired timestamp, got %d", rr.Code)
	}
}

func TestHeartbeatService_ProjectFilter(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	manager := &heartbeatMockManager{
		agents: []api.AgentInfo{
			{Name: "agent-1", ProjectID: "grove-hub1", Phase: "running"},
			{Name: "agent-2", ProjectID: "grove-hub1", Phase: "running"},
			{Name: "agent-3", ProjectID: "grove-hub2", Phase: "running"},
			{Name: "agent-4", ProjectID: "grove-shared", Phase: "running"},
		},
	}

	// Filter: only include grove-hub1 groves
	groveFilter := func(projectID string) bool {
		return projectID == "grove-hub1"
	}

	svc := NewHeartbeatService(client, "test-host", time.Hour, manager, groveFilter, slog.Default())
	err := svc.ForceHeartbeat(context.Background())
	if err != nil {
		t.Fatalf("ForceHeartbeat failed: %v", err)
	}

	calls := client.getHeartbeatCalls()
	if len(calls) != 1 {
		t.Fatalf("Expected 1 heartbeat call, got %d", len(calls))
	}

	heartbeat := calls[0].Heartbeat

	// Should only include grove-hub1 (2 agents), not grove-hub2 or grove-shared
	if len(heartbeat.Projects) != 1 {
		t.Errorf("Expected 1 grove in heartbeat (filtered), got %d", len(heartbeat.Projects))
	}

	if len(heartbeat.Projects) > 0 && heartbeat.Projects[0].ProjectID != "grove-hub1" {
		t.Errorf("Expected grove-hub1, got %q", heartbeat.Projects[0].ProjectID)
	}

	if len(heartbeat.Projects) > 0 && heartbeat.Projects[0].AgentCount != 2 {
		t.Errorf("Expected 2 agents in grove-hub1, got %d", heartbeat.Projects[0].AgentCount)
	}
}

func TestHeartbeatService_NilProjectFilter(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	manager := &heartbeatMockManager{
		agents: []api.AgentInfo{
			{Name: "agent-1", ProjectID: "grove-1", Phase: "running"},
			{Name: "agent-2", ProjectID: "grove-2", Phase: "running"},
		},
	}

	// Nil filter: include all groves
	svc := NewHeartbeatService(client, "test-host", time.Hour, manager, nil, slog.Default())
	err := svc.ForceHeartbeat(context.Background())
	if err != nil {
		t.Fatalf("ForceHeartbeat failed: %v", err)
	}

	calls := client.getHeartbeatCalls()
	if len(calls) != 1 {
		t.Fatalf("Expected 1 heartbeat call, got %d", len(calls))
	}

	heartbeat := calls[0].Heartbeat
	if len(heartbeat.Projects) != 2 {
		t.Errorf("Expected 2 groves with nil filter, got %d", len(heartbeat.Projects))
	}
}

func TestResolveHydrator_WithConnectionHeader(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	// Verify the hydrator resolves via header
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil)
	req.Header.Set("X-Scion-Hub-Connection", "local")

	hydrator := srv.resolveHydrator(req)
	// In test mode cache is nil, so hydrator is nil -- that's expected
	// What we're testing is the routing logic
	srv.hubMu.RLock()
	conn := srv.hubConnections["local"]
	srv.hubMu.RUnlock()

	if conn == nil {
		t.Fatal("expected 'local' connection to exist")
	}

	// The hydrator from resolveHydrator should match the connection's hydrator
	if hydrator != conn.Hydrator {
		t.Error("expected resolveHydrator to return the local connection's hydrator")
	}
}

func TestResolveHydrator_FallbackToFirstAvailable(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	// Request without connection header should fall back to first available
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil)
	hydrator := srv.resolveHydrator(req)

	srv.hubMu.RLock()
	conn := srv.hubConnections["local"]
	srv.hubMu.RUnlock()

	if hydrator != conn.Hydrator {
		t.Error("expected resolveHydrator to fall back to first available hydrator")
	}
}

func TestResolveHydrator_UnknownConnection(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	// Request with unknown connection name should fall back
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil)
	req.Header.Set("X-Scion-Hub-Connection", "nonexistent")
	hydrator := srv.resolveHydrator(req)

	// Should fall back to any available hydrator
	srv.hubMu.RLock()
	conn := srv.hubConnections["local"]
	srv.hubMu.RUnlock()

	if hydrator != conn.Hydrator {
		t.Error("expected resolveHydrator to fall back when connection not found")
	}
}

func TestGlobalProjectRejection_MultiHub(t *testing.T) {
	// Create a server with two connections to simulate multi-hub mode
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	// Add a second connection to enable multi-hub mode
	creds2 := makeTestCreds("hub-prod", "broker-2", "https://hub.prod.example.com")
	conn2, err := srv.createHubConnection("hub-prod", creds2)
	if err != nil {
		t.Fatal(err)
	}
	srv.hubMu.Lock()
	srv.hubConnections["hub-prod"] = conn2
	srv.hubMu.Unlock()

	if !srv.isMultiHubMode() {
		t.Fatal("expected multi-hub mode with 2 connections")
	}

	// Try to create an agent with empty projectID (global grove)
	body := `{"name": "global-agent", "config": {"template": "claude"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected status %d for global grove in multi-hub mode, got %d: %s",
			http.StatusConflict, w.Code, w.Body.String())
	}

	// Verify error code
	var errResp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatal("expected error object in response")
	}
	if errObj["code"] != "global_grove_disabled" {
		t.Errorf("expected error code 'global_grove_disabled', got %q", errObj["code"])
	}
}

func TestGlobalProjectRejection_SingleHub_Allowed(t *testing.T) {
	// Single-hub mode: global grove should be allowed
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	if srv.isMultiHubMode() {
		t.Fatal("expected single-hub mode with 1 connection")
	}

	body := `{"name": "global-agent", "config": {"template": "claude"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// In single-hub mode, empty projectID should NOT be rejected
	if w.Code == http.StatusConflict {
		t.Error("single-hub mode should not reject global grove agents")
	}
}

func TestGlobalProjectRejection_WithProjectID_MultiHub(t *testing.T) {
	// Multi-hub mode: agents with a specific projectID should be allowed
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	creds2 := makeTestCreds("hub-prod", "broker-2", "https://hub.prod.example.com")
	conn2, _ := srv.createHubConnection("hub-prod", creds2)
	srv.hubMu.Lock()
	srv.hubConnections["hub-prod"] = conn2
	srv.hubMu.Unlock()

	body := `{
		"name": "scoped-agent",
		"groveId": "my-project",
		"grovePath": "/some/path/.scion",
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should NOT be rejected (has explicit projectID and projectPath)
	if w.Code == http.StatusConflict {
		t.Errorf("expected non-global grove to be allowed in multi-hub mode, got %d: %s",
			w.Code, w.Body.String())
	}
}

func TestGlobalProjectRejection_GitProjectWithProjectID_NoPath_MultiHub(t *testing.T) {
	// Multi-hub mode: a git-based grove has a projectID but no projectPath or
	// projectSlug (the broker resolves workspace from the git remote). This
	// should NOT be treated as the global grove.
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	creds2 := makeTestCreds("hub-prod", "broker-2", "https://hub.prod.example.com")
	conn2, _ := srv.createHubConnection("hub-prod", creds2)
	srv.hubMu.Lock()
	srv.hubConnections["hub-prod"] = conn2
	srv.hubMu.Unlock()

	body := `{
		"name": "git-grove-agent",
		"groveId": "abc-123-grove-id",
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should NOT be rejected — projectID is set, so this is not the global grove
	if w.Code == http.StatusConflict {
		t.Errorf("expected git-based grove (projectID set, no path) to be allowed in multi-hub mode, got %d: %s",
			w.Code, w.Body.String())
	}
}

func TestIsMultiHubMode(t *testing.T) {
	srv := newTestServer(t)

	// No connections = not multi-hub
	if srv.isMultiHubMode() {
		t.Error("expected single-hub mode with no connections")
	}

	// Add one connection
	srv.hubMu.Lock()
	srv.hubConnections["hub-1"] = &HubConnection{Name: "hub-1"}
	srv.hubMu.Unlock()

	if srv.isMultiHubMode() {
		t.Error("expected single-hub mode with 1 connection")
	}

	// Add second connection
	srv.hubMu.Lock()
	srv.hubConnections["hub-2"] = &HubConnection{Name: "hub-2"}
	srv.hubMu.Unlock()

	if !srv.isMultiHubMode() {
		t.Error("expected multi-hub mode with 2 connections")
	}
}

func TestIsGlobalProject(t *testing.T) {
	srv := newTestServer(t)

	tests := []struct {
		name      string
		projectID   string
		projectPath string
		expected  bool
	}{
		{"empty both", "", "", true},
		{"global id", "global", "/some/path", true},
		{"global id empty path", "global", "", true},
		{"empty id with path", "", "/some/path", false},
		{"projectID set empty path", "my-project", "", false},
		{"both set", "my-project", "/some/path", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := srv.isGlobalProject(tc.projectID, tc.projectPath)
			if result != tc.expected {
				t.Errorf("isGlobalProject(%q, %q) = %v, expected %v",
					tc.projectID, tc.projectPath, result, tc.expected)
			}
		})
	}
}

func TestConnectionStatus(t *testing.T) {
	conn := &HubConnection{
		Name:   "test",
		Status: ConnectionStatusDisconnected,
	}

	if conn.GetStatus() != ConnectionStatusDisconnected {
		t.Errorf("expected disconnected, got %v", conn.GetStatus())
	}

	conn.setStatus(ConnectionStatusConnected)

	if conn.GetStatus() != ConnectionStatusConnected {
		t.Errorf("expected connected, got %v", conn.GetStatus())
	}

	conn.setStatus(ConnectionStatusError)

	if conn.GetStatus() != ConnectionStatusError {
		t.Errorf("expected error, got %v", conn.GetStatus())
	}
}

func TestCredentialWatcher_AddConnection(t *testing.T) {
	tmpDir := t.TempDir()
	credDir := filepath.Join(tmpDir, "hub-credentials")
	if err := os.MkdirAll(credDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Start with one connection file
	creds1 := makeTestCreds("hub-one", "broker-1", "http://hub1.example.com")
	data1, _ := json.MarshalIndent(creds1, "", "  ")
	if err := os.WriteFile(filepath.Join(credDir, "hub-one.json"), data1, 0600); err != nil {
		t.Fatal(err)
	}

	// Create server without hub integration to avoid auto-creating connections
	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"

	mgr := &mockManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	// Manually set up multi-store and load initial connections
	srv.multiCredStore = brokercredentials.NewMultiStore(credDir)
	multiCreds, _ := srv.multiCredStore.List()
	for i := range multiCreds {
		c := &multiCreds[i]
		conn, err := srv.createHubConnection(c.Name, c)
		if err != nil {
			t.Fatal(err)
		}
		srv.hubMu.Lock()
		srv.hubConnections[c.Name] = conn
		srv.hubMu.Unlock()
	}

	srv.hubMu.RLock()
	initialCount := len(srv.hubConnections)
	srv.hubMu.RUnlock()

	if initialCount != 1 {
		t.Fatalf("expected 1 initial connection, got %d", initialCount)
	}

	// Add a new credential file
	creds2 := makeTestCreds("hub-two", "broker-2", "http://hub2.example.com")
	data2, _ := json.MarshalIndent(creds2, "", "  ")
	if err := os.WriteFile(filepath.Join(credDir, "hub-two.json"), data2, 0600); err != nil {
		t.Fatal(err)
	}

	// Trigger credential reload
	ctx := context.Background()
	if err := srv.checkAndReloadCredentials(ctx); err != nil {
		t.Fatalf("checkAndReloadCredentials failed: %v", err)
	}

	srv.hubMu.RLock()
	newCount := len(srv.hubConnections)
	_, exists := srv.hubConnections["hub-two"]
	srv.hubMu.RUnlock()

	if newCount != 2 {
		t.Errorf("expected 2 connections after add, got %d", newCount)
	}

	if !exists {
		t.Error("expected 'hub-two' connection to exist after reload")
	}
}

func TestCredentialWatcher_RemoveConnection(t *testing.T) {
	tmpDir := t.TempDir()
	credDir := filepath.Join(tmpDir, "hub-credentials")
	if err := os.MkdirAll(credDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Start with two connections
	creds1 := makeTestCreds("hub-one", "broker-1", "http://hub1.example.com")
	data1, _ := json.MarshalIndent(creds1, "", "  ")
	if err := os.WriteFile(filepath.Join(credDir, "hub-one.json"), data1, 0600); err != nil {
		t.Fatal(err)
	}

	creds2 := makeTestCreds("hub-two", "broker-2", "http://hub2.example.com")
	data2, _ := json.MarshalIndent(creds2, "", "  ")
	if err := os.WriteFile(filepath.Join(credDir, "hub-two.json"), data2, 0600); err != nil {
		t.Fatal(err)
	}

	// Create server without hub integration
	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"

	mgr := &mockManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	srv.multiCredStore = brokercredentials.NewMultiStore(credDir)
	multiCreds, _ := srv.multiCredStore.List()
	for i := range multiCreds {
		c := &multiCreds[i]
		conn, err := srv.createHubConnection(c.Name, c)
		if err != nil {
			t.Fatal(err)
		}
		srv.hubMu.Lock()
		srv.hubConnections[c.Name] = conn
		srv.hubMu.Unlock()
	}

	srv.hubMu.RLock()
	if len(srv.hubConnections) != 2 {
		t.Fatalf("expected 2 initial connections, got %d", len(srv.hubConnections))
	}
	srv.hubMu.RUnlock()

	// Remove hub-two credential file
	if err := os.Remove(filepath.Join(credDir, "hub-two.json")); err != nil {
		t.Fatal(err)
	}

	// Trigger credential reload
	ctx := context.Background()
	if err := srv.checkAndReloadCredentials(ctx); err != nil {
		t.Fatalf("checkAndReloadCredentials failed: %v", err)
	}

	srv.hubMu.RLock()
	_, exists := srv.hubConnections["hub-two"]
	count := len(srv.hubConnections)
	srv.hubMu.RUnlock()

	if exists {
		t.Error("expected 'hub-two' connection to be removed after credential deletion")
	}

	if count != 1 {
		t.Errorf("expected 1 connection after removal, got %d", count)
	}
}

func TestBuildAuthMiddleware_NoKeys(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.BrokerAuthEnabled = true
	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})

	// With no connections and no keys, middleware should be nil
	if srv.brokerAuthMiddleware != nil {
		t.Error("expected nil middleware when no keys available")
	}
}

func TestBuildAuthMiddleware_WithKeys(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")

	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://localhost:8080"
	cfg.InMemoryCredentials = creds
	cfg.BrokerAuthEnabled = true

	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})

	if srv.brokerAuthMiddleware == nil {
		t.Error("expected middleware to be created when keys available")
	}
}

func TestDefaultServerConfig_SecureBrokerAuthDefaults(t *testing.T) {
	cfg := DefaultServerConfig()
	if !cfg.BrokerAuthEnabled {
		t.Error("expected BrokerAuthEnabled to default to true")
	}
	if !cfg.BrokerAuthStrictMode {
		t.Error("expected BrokerAuthStrictMode to default to true")
	}
}

func TestRuntimeBroker_DefaultAuth_DeniesUnauthenticatedRequests(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")

	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://localhost:8080"
	cfg.InMemoryCredentials = creds

	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d for unauthenticated request, got %d", http.StatusUnauthorized, w.Code)
	}
}

func TestValidateBrokerAuthStartup_HubModeWithoutKeysLoopbackAllowed(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.Host = "127.0.0.1"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://localhost:9810"
	cfg.InMemoryCredentials = nil

	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
	if err := srv.validateBrokerAuthStartup(); err != nil {
		t.Fatalf("loopback hub mode without keys should be allowed (pending registration), got: %v", err)
	}
}

func TestValidateBrokerAuthStartup_HubModeWithoutKeysNonLoopbackFails(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.Host = "0.0.0.0"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://localhost:9810"
	cfg.InMemoryCredentials = nil

	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
	if err := srv.validateBrokerAuthStartup(); err == nil {
		t.Fatal("non-loopback hub mode without keys should fail")
	}
}

func TestValidateBrokerAuthStartup_NonLoopbackPermissiveModeFails(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")

	cfg := DefaultServerConfig()
	cfg.Host = "0.0.0.0"
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://localhost:8080"
	cfg.InMemoryCredentials = creds
	cfg.BrokerAuthStrictMode = false

	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
	if err := srv.validateBrokerAuthStartup(); err == nil {
		t.Fatal("expected startup validation to fail for non-loopback host without strict auth")
	}
}

func TestGetFirstHeartbeat_NoConnections(t *testing.T) {
	srv := newTestServer(t)

	hb := srv.getFirstHeartbeat()
	if hb != nil {
		t.Error("expected nil heartbeat when no connections")
	}
}

func TestHubConnection_Stop(t *testing.T) {
	conn := &HubConnection{
		Name:   "test",
		Status: ConnectionStatusConnected,
	}

	conn.Stop()

	if conn.GetStatus() != ConnectionStatusDisconnected {
		t.Errorf("expected disconnected after Stop, got %v", conn.GetStatus())
	}
}

// ============================================================================
// Phase 3: Co-located + Remote Combo Tests
// ============================================================================

func TestColocated_LocalConnection_GetsHeartbeat(t *testing.T) {
	// Co-located connections still need the broker heartbeat path so the hub
	// receives lifecycle updates from the embedded runtime broker.
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	srv.hubMu.RLock()
	conn, ok := srv.hubConnections["local"]
	srv.hubMu.RUnlock()

	if !ok {
		t.Fatal("expected 'local' connection to exist")
	}

	if !conn.IsColocated {
		t.Error("expected 'local' connection to be marked as co-located")
	}

	// Start the connection and verify the heartbeat service is created.
	cfg := srv.config
	cfg.HeartbeatEnabled = true
	cfg.ControlChannelEnabled = false // disable control channel for this test
	srv.config = cfg

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := conn.Start(ctx, srv); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if conn.Heartbeat == nil {
		t.Error("expected co-located connection to have heartbeat service")
	}
}

func TestColocated_RemoteConnection_GetsHeartbeat(t *testing.T) {
	// A non-co-located (remote) connection should get a heartbeat service
	// when HeartbeatEnabled is true.
	remoteCreds := makeTestCreds("hub-prod", "broker-1", "https://hub.prod.example.com")

	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://localhost:8080"
	cfg.HeartbeatEnabled = true
	cfg.ControlChannelEnabled = false

	mgr := &mockManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	conn, err := srv.createHubConnection("hub-prod", remoteCreds)
	if err != nil {
		t.Fatal(err)
	}
	// Explicitly NOT setting IsColocated — default is false

	srv.hubMu.Lock()
	srv.hubConnections["hub-prod"] = conn
	srv.hubMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := conn.Start(ctx, srv); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if conn.Heartbeat == nil {
		t.Error("expected remote connection to have heartbeat service")
	}
}

func TestColocated_ComboMode_HeartbeatPerConnection(t *testing.T) {
	// In combo mode (co-located local + remote), verify that both connections
	// get heartbeat services.
	tmpDir := t.TempDir()
	credDir := filepath.Join(tmpDir, "hub-credentials")
	if err := os.MkdirAll(credDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Write a remote credential file
	remoteCreds := makeTestCreds("hub-prod", "broker-1", "https://hub.prod.example.com")
	remoteData, _ := json.MarshalIndent(remoteCreds, "", "  ")
	if err := os.WriteFile(filepath.Join(credDir, "hub-prod.json"), remoteData, 0600); err != nil {
		t.Fatal(err)
	}

	// Create server with in-memory local creds
	localCreds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://localhost:8080"
	cfg.InMemoryCredentials = localCreds
	cfg.HeartbeatEnabled = true
	cfg.ControlChannelEnabled = false

	mgr := &mockManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	// Add remote connection from multi-store
	srv.multiCredStore = brokercredentials.NewMultiStore(credDir)
	multiCreds, _ := srv.multiCredStore.List()
	for i := range multiCreds {
		c := &multiCreds[i]
		if _, exists := srv.hubConnections[c.Name]; exists {
			continue
		}
		conn, err := srv.createHubConnection(c.Name, c)
		if err != nil {
			t.Fatalf("Failed to create connection %q: %v", c.Name, err)
		}
		srv.hubMu.Lock()
		srv.hubConnections[c.Name] = conn
		srv.hubMu.Unlock()
	}

	// Start all connections
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv.hubMu.RLock()
	for _, conn := range srv.hubConnections {
		if err := conn.Start(ctx, srv); err != nil {
			t.Fatalf("Start failed for %s: %v", conn.Name, err)
		}
	}
	srv.hubMu.RUnlock()

	// Verify: both local and remote connections have heartbeat services.
	srv.hubMu.RLock()
	localConn := srv.hubConnections["local"]
	remoteConn := srv.hubConnections["hub-prod"]
	srv.hubMu.RUnlock()

	if localConn == nil || remoteConn == nil {
		t.Fatal("expected both 'local' and 'hub-prod' connections to exist")
	}

	if !localConn.IsColocated {
		t.Error("expected 'local' to be co-located")
	}

	if localConn.Heartbeat == nil {
		t.Error("expected co-located 'local' connection to have heartbeat")
	}

	if remoteConn.IsColocated {
		t.Error("expected 'hub-prod' to NOT be co-located")
	}

	if remoteConn.Heartbeat == nil {
		t.Error("expected remote 'hub-prod' connection to have heartbeat")
	}
}

func TestColocated_CredentialWatcher_PreservesLocalConnection(t *testing.T) {
	// When credentials are reloaded, the "local" connection from InMemoryCredentials
	// must be preserved even if it's not in the multi-store.
	tmpDir := t.TempDir()
	credDir := filepath.Join(tmpDir, "hub-credentials")
	if err := os.MkdirAll(credDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Write one remote credential
	remoteCreds := makeTestCreds("hub-prod", "broker-1", "https://hub.prod.example.com")
	remoteData, _ := json.MarshalIndent(remoteCreds, "", "  ")
	if err := os.WriteFile(filepath.Join(credDir, "hub-prod.json"), remoteData, 0600); err != nil {
		t.Fatal(err)
	}

	// Create server with in-memory local creds
	localCreds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://localhost:8080"
	cfg.InMemoryCredentials = localCreds

	mgr := &mockManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	// Manually set up multi-store and add remote
	srv.multiCredStore = brokercredentials.NewMultiStore(credDir)
	multiCreds, _ := srv.multiCredStore.List()
	for i := range multiCreds {
		c := &multiCreds[i]
		if _, exists := srv.hubConnections[c.Name]; exists {
			continue
		}
		conn, err := srv.createHubConnection(c.Name, c)
		if err != nil {
			t.Fatal(err)
		}
		srv.hubMu.Lock()
		srv.hubConnections[c.Name] = conn
		srv.hubMu.Unlock()
	}

	srv.hubMu.RLock()
	if len(srv.hubConnections) != 2 {
		t.Fatalf("expected 2 connections, got %d", len(srv.hubConnections))
	}
	srv.hubMu.RUnlock()

	// Remove the remote credential file
	if err := os.Remove(filepath.Join(credDir, "hub-prod.json")); err != nil {
		t.Fatal(err)
	}

	// Trigger credential reload
	ctx := context.Background()
	if err := srv.checkAndReloadCredentials(ctx); err != nil {
		t.Fatalf("checkAndReloadCredentials failed: %v", err)
	}

	// Verify: local preserved, remote removed
	srv.hubMu.RLock()
	localConn, localExists := srv.hubConnections["local"]
	_, remoteExists := srv.hubConnections["hub-prod"]
	count := len(srv.hubConnections)
	srv.hubMu.RUnlock()

	if !localExists {
		t.Error("expected 'local' connection to be preserved after reload")
	}

	if remoteExists {
		t.Error("expected 'hub-prod' connection to be removed after credential deletion")
	}

	if count != 1 {
		t.Errorf("expected 1 connection after reload, got %d", count)
	}

	if localConn != nil && !localConn.IsColocated {
		t.Error("expected preserved 'local' connection to still be co-located")
	}
}

func TestColocated_CredentialWatcher_AddRemoteAlongsideLocal(t *testing.T) {
	// Start with only the local co-located connection, then add a remote one
	// via credential watcher.
	tmpDir := t.TempDir()
	credDir := filepath.Join(tmpDir, "hub-credentials")
	if err := os.MkdirAll(credDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Create server with in-memory local creds only
	localCreds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://localhost:8080"
	cfg.InMemoryCredentials = localCreds

	mgr := &mockManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	srv.multiCredStore = brokercredentials.NewMultiStore(credDir)

	srv.hubMu.RLock()
	if len(srv.hubConnections) != 1 {
		t.Fatalf("expected 1 initial connection (local), got %d", len(srv.hubConnections))
	}
	srv.hubMu.RUnlock()

	// Add a remote credential file
	remoteCreds := makeTestCreds("hub-staging", "broker-1", "https://hub.staging.example.com")
	remoteData, _ := json.MarshalIndent(remoteCreds, "", "  ")
	if err := os.WriteFile(filepath.Join(credDir, "hub-staging.json"), remoteData, 0600); err != nil {
		t.Fatal(err)
	}

	// Trigger credential reload
	ctx := context.Background()
	if err := srv.checkAndReloadCredentials(ctx); err != nil {
		t.Fatalf("checkAndReloadCredentials failed: %v", err)
	}

	// Verify: local preserved, remote added
	srv.hubMu.RLock()
	_, localExists := srv.hubConnections["local"]
	remoteConn, remoteExists := srv.hubConnections["hub-staging"]
	count := len(srv.hubConnections)
	srv.hubMu.RUnlock()

	if !localExists {
		t.Error("expected 'local' connection to be preserved")
	}

	if !remoteExists {
		t.Error("expected 'hub-staging' connection to be added")
	}

	if count != 2 {
		t.Errorf("expected 2 connections, got %d", count)
	}

	// Verify the newly added connection is NOT co-located
	if remoteConn != nil && remoteConn.IsColocated {
		t.Error("expected 'hub-staging' to NOT be co-located")
	}
}

func TestColocated_GlobalProjectRejection_ComboMode(t *testing.T) {
	// In combo mode with local + remote, multi-hub mode should reject global grove.
	localCreds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(localCreds)

	// Add a remote connection to trigger multi-hub mode
	remoteCreds := makeTestCreds("hub-prod", "broker-1", "https://hub.prod.example.com")
	conn, err := srv.createHubConnection("hub-prod", remoteCreds)
	if err != nil {
		t.Fatal(err)
	}
	srv.hubMu.Lock()
	srv.hubConnections["hub-prod"] = conn
	srv.hubMu.Unlock()

	if !srv.isMultiHubMode() {
		t.Fatal("expected multi-hub mode with local + remote")
	}

	// Try to create a global grove agent
	body := `{"name": "global-agent", "config": {"template": "claude"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected status %d for global grove in combo multi-hub mode, got %d: %s",
			http.StatusConflict, w.Code, w.Body.String())
	}
}

func TestColocated_MultipleRemoteConnections(t *testing.T) {
	// Test co-located mode with multiple remote connections alongside the local.
	tmpDir := t.TempDir()
	credDir := filepath.Join(tmpDir, "hub-credentials")
	if err := os.MkdirAll(credDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Write two remote credential files
	for _, name := range []string{"hub-prod", "hub-staging"} {
		creds := makeTestCreds(name, "broker-1", "https://"+name+".example.com")
		data, _ := json.MarshalIndent(creds, "", "  ")
		if err := os.WriteFile(filepath.Join(credDir, name+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}

	// Create server with local + multi-store
	localCreds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://localhost:8080"
	cfg.InMemoryCredentials = localCreds

	mgr := &mockManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	srv.multiCredStore = brokercredentials.NewMultiStore(credDir)
	multiCreds, _ := srv.multiCredStore.List()
	for i := range multiCreds {
		c := &multiCreds[i]
		if _, exists := srv.hubConnections[c.Name]; exists {
			continue
		}
		conn, err := srv.createHubConnection(c.Name, c)
		if err != nil {
			t.Fatalf("Failed to create connection %q: %v", c.Name, err)
		}
		srv.hubMu.Lock()
		srv.hubConnections[c.Name] = conn
		srv.hubMu.Unlock()
	}

	srv.hubMu.RLock()
	count := len(srv.hubConnections)
	srv.hubMu.RUnlock()

	if count != 3 {
		t.Fatalf("expected 3 hub connections (local + 2 remote), got %d", count)
	}

	// Verify co-located flags
	srv.hubMu.RLock()
	for name, conn := range srv.hubConnections {
		if name == "local" {
			if !conn.IsColocated {
				t.Errorf("expected 'local' to be co-located")
			}
		} else {
			if conn.IsColocated {
				t.Errorf("expected %q to NOT be co-located", name)
			}
		}
	}
	srv.hubMu.RUnlock()

	// Must be in multi-hub mode
	if !srv.isMultiHubMode() {
		t.Error("expected multi-hub mode with 3 connections")
	}
}

func TestLogHubConnections_NoConnections(t *testing.T) {
	// Verify logHubConnections doesn't panic with no connections
	srv := newTestServer(t)
	srv.logHubConnections() // should not panic
}

func TestLogHubConnections_WithConnections(t *testing.T) {
	// Verify logHubConnections doesn't panic with connections
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)
	srv.logHubConnections() // should not panic
}

func TestControlChannel_ConnectionNameHeader(t *testing.T) {
	// Verify that NewControlChannelClient stores the connectionName
	config := ControlChannelConfig{
		HubEndpoint: "https://hub.example.com",
		BrokerID:    "test-broker",
		SecretKey:   []byte("test-secret-key-12345678901234567890"),
	}

	cc := NewControlChannelClient(config, nil, nil, "hub-prod", slog.Default())

	if cc.connectionName != "hub-prod" {
		t.Errorf("expected connectionName 'hub-prod', got %q", cc.connectionName)
	}
}

func TestControlChannel_EmptyConnectionName(t *testing.T) {
	config := ControlChannelConfig{
		HubEndpoint: "https://hub.example.com",
		BrokerID:    "test-broker",
		SecretKey:   []byte("test-secret-key-12345678901234567890"),
	}

	cc := NewControlChannelClient(config, nil, nil, "", slog.Default())

	if cc.connectionName != "" {
		t.Errorf("expected empty connectionName, got %q", cc.connectionName)
	}
}

// ============================================================================
// Phase 4: Hub Connections API Endpoint Tests
// ============================================================================

func TestHandleHubConnections_SingleConnection(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/hub-connections", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp HubConnectionStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Mode != "single-hub" {
		t.Errorf("expected mode 'single-hub', got %q", resp.Mode)
	}

	if len(resp.Connections) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(resp.Connections))
	}

	conn := resp.Connections[0]
	if conn.Name != "local" {
		t.Errorf("expected connection name 'local', got %q", conn.Name)
	}
	if conn.HubEndpoint != "http://localhost:8080" {
		t.Errorf("expected endpoint 'http://localhost:8080', got %q", conn.HubEndpoint)
	}
	if conn.BrokerID != "broker-1" {
		t.Errorf("expected brokerId 'broker-1', got %q", conn.BrokerID)
	}
}

func TestHandleHubConnections_MultipleConnections(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	// Add a second connection
	creds2 := makeTestCreds("hub-prod", "broker-2", "https://hub.prod.example.com")
	conn2, err := srv.createHubConnection("hub-prod", creds2)
	if err != nil {
		t.Fatal(err)
	}
	srv.hubMu.Lock()
	srv.hubConnections["hub-prod"] = conn2
	srv.hubMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/hub-connections", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp HubConnectionStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Mode != "multi-hub" {
		t.Errorf("expected mode 'multi-hub', got %q", resp.Mode)
	}

	if len(resp.Connections) != 2 {
		t.Fatalf("expected 2 connections, got %d", len(resp.Connections))
	}

	// Build lookup
	connMap := make(map[string]HubConnectionInfo)
	for _, c := range resp.Connections {
		connMap[c.Name] = c
	}

	if _, ok := connMap["local"]; !ok {
		t.Error("expected 'local' connection in response")
	}
	if _, ok := connMap["hub-prod"]; !ok {
		t.Error("expected 'hub-prod' connection in response")
	}
}

func TestHandleHubConnections_ColocatedFlag(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	// Add a remote connection
	creds2 := makeTestCreds("hub-prod", "broker-2", "https://hub.prod.example.com")
	conn2, err := srv.createHubConnection("hub-prod", creds2)
	if err != nil {
		t.Fatal(err)
	}
	srv.hubMu.Lock()
	srv.hubConnections["hub-prod"] = conn2
	srv.hubMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/hub-connections", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	var resp HubConnectionStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	connMap := make(map[string]HubConnectionInfo)
	for _, c := range resp.Connections {
		connMap[c.Name] = c
	}

	localConn, ok := connMap["local"]
	if !ok {
		t.Fatal("expected 'local' connection")
	}
	if !localConn.IsColocated {
		t.Error("expected 'local' to be marked as co-located")
	}

	prodConn, ok := connMap["hub-prod"]
	if !ok {
		t.Fatal("expected 'hub-prod' connection")
	}
	if prodConn.IsColocated {
		t.Error("expected 'hub-prod' to NOT be marked as co-located")
	}
}

func TestHandleHubConnections_NoConnections(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/hub-connections", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp HubConnectionStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Mode != "single-hub" {
		t.Errorf("expected mode 'single-hub' for 0 connections, got %q", resp.Mode)
	}

	if len(resp.Connections) != 0 {
		t.Errorf("expected 0 connections, got %d", len(resp.Connections))
	}
}

func TestHandleHubConnections_MethodNotAllowed(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/hub-connections", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for POST, got %d", w.Code)
	}
}

func TestHandleHubConnections_ConnectionStatus(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	// Verify the status field is populated
	srv.hubMu.RLock()
	conn := srv.hubConnections["local"]
	srv.hubMu.RUnlock()

	// Set a known status
	conn.setStatus(ConnectionStatusConnected)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/hub-connections", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	var resp HubConnectionStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Connections) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(resp.Connections))
	}

	if resp.Connections[0].Status != "connected" {
		t.Errorf("expected status 'connected', got %q", resp.Connections[0].Status)
	}
}

func TestHydrateTemplate_ColocatedPassthrough(t *testing.T) {
	// Create a temporary templates directory with a template
	templatesDir := t.TempDir()
	templateDir := filepath.Join(templatesDir, "my-template")
	if err := os.MkdirAll(templateDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templateDir, "file.txt"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}

	srv := newTestServer(t)

	// Create a co-located connection with TemplatesDir set
	conn := &HubConnection{
		Name:         "local",
		IsColocated:  true,
		TemplatesDir: templatesDir,
	}

	cfg := &CreateAgentConfig{
		Template:   "my-template",
		TemplateID: "some-uuid",
	}

	// Co-located passthrough should resolve directly to the local directory
	path, err := srv.hydrateTemplate(context.Background(), cfg, conn)
	if err != nil {
		t.Fatalf("hydrateTemplate failed: %v", err)
	}
	if path != templateDir {
		t.Errorf("expected path %q, got %q", templateDir, path)
	}
}

func TestHydrateTemplate_ColocatedFallsBackWhenMissing(t *testing.T) {
	templatesDir := t.TempDir()
	// Don't create the template directory — it doesn't exist locally

	srv := newTestServer(t)

	conn := &HubConnection{
		Name:         "local",
		IsColocated:  true,
		TemplatesDir: templatesDir,
		// No Hydrator set — should return empty string
	}

	cfg := &CreateAgentConfig{
		Template:   "nonexistent-template",
		TemplateID: "some-uuid",
	}

	// Should fall through to hydrator (which is nil), returning empty string
	path, err := srv.hydrateTemplate(context.Background(), cfg, conn)
	if err != nil {
		t.Fatalf("hydrateTemplate failed: %v", err)
	}
	if path != "" {
		t.Errorf("expected empty path for missing local template, got %q", path)
	}
}

func TestHydrateTemplate_NonColocatedSkipsPassthrough(t *testing.T) {
	// Create a templates directory with a template
	templatesDir := t.TempDir()
	templateDir := filepath.Join(templatesDir, "my-template")
	if err := os.MkdirAll(templateDir, 0755); err != nil {
		t.Fatal(err)
	}

	srv := newTestServer(t)

	// Non-colocated connection — should NOT use passthrough even with TemplatesDir set
	conn := &HubConnection{
		Name:         "remote",
		IsColocated:  false,
		TemplatesDir: templatesDir,
		// No Hydrator — should return empty
	}

	cfg := &CreateAgentConfig{
		Template:   "my-template",
		TemplateID: "some-uuid",
	}

	path, err := srv.hydrateTemplate(context.Background(), cfg, conn)
	if err != nil {
		t.Fatalf("hydrateTemplate failed: %v", err)
	}
	if path != "" {
		t.Errorf("expected empty path for non-colocated connection, got %q", path)
	}
}
