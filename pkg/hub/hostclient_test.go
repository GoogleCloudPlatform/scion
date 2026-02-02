package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ptone/scion-agent/pkg/apiclient"
	"github.com/ptone/scion-agent/pkg/store"
	"github.com/ptone/scion-agent/pkg/store/sqlite"
)

func TestAuthenticatedHostClient_CreateAgent(t *testing.T) {
	// Create a test store with a host secret
	db, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	// Create a test host
	hostID := "test-host-123"
	secretKey := []byte("test-secret-key-32-bytes-long!!!")

	host := &store.RuntimeHost{
		ID:      hostID,
		Name:    "test-host",
		Slug:    "test-host",
		Mode:    store.HostModeConnected,
		Status:  store.HostStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := db.CreateRuntimeHost(context.Background(), host); err != nil {
		t.Fatalf("failed to create runtime host: %v", err)
	}

	secret := &store.HostSecret{
		HostID:    hostID,
		SecretKey: secretKey,
		Algorithm: store.HostSecretAlgorithmHMACSHA256,
		Status:    store.HostSecretStatusActive,
		CreatedAt: time.Now(),
	}
	if err := db.CreateHostSecret(context.Background(), secret); err != nil {
		t.Fatalf("failed to create host secret: %v", err)
	}

	// Create a test server that validates HMAC signatures
	var receivedHeaders http.Header
	var requestValidated bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()

		// Verify HMAC headers are present
		if r.Header.Get(apiclient.HeaderHostID) == "" {
			t.Error("missing X-Scion-Host-ID header")
		}
		if r.Header.Get(apiclient.HeaderTimestamp) == "" {
			t.Error("missing X-Scion-Timestamp header")
		}
		if r.Header.Get(apiclient.HeaderNonce) == "" {
			t.Error("missing X-Scion-Nonce header")
		}
		if r.Header.Get(apiclient.HeaderSignature) == "" {
			t.Error("missing X-Scion-Signature header")
		}

		// Verify host ID matches
		if got := r.Header.Get(apiclient.HeaderHostID); got != hostID {
			t.Errorf("wrong host ID: got %s, want %s", got, hostID)
		}

		requestValidated = true

		// Return success response
		resp := &RemoteAgentResponse{
			Created: true,
			Agent: &RemoteAgentInfo{
				ID:     "agent-1",
				Name:   "test-agent",
				Status: "created",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	// Create authenticated client
	client := NewAuthenticatedHostClient(db, true)

	// Make request
	req := &RemoteCreateAgentRequest{
		AgentID: "agent-1",
		Name:    "test-agent",
		GroveID: "grove-1",
	}

	resp, err := client.CreateAgent(context.Background(), hostID, server.URL, req)
	if err != nil {
		t.Fatalf("CreateAgent failed: %v", err)
	}

	if !requestValidated {
		t.Error("request was not validated by server")
	}

	if resp == nil || resp.Agent == nil {
		t.Fatal("expected non-nil response")
	}

	if resp.Agent.Name != "test-agent" {
		t.Errorf("wrong agent name: got %s, want test-agent", resp.Agent.Name)
	}

	// Verify all expected headers were set
	if receivedHeaders.Get("Content-Type") != "application/json" {
		t.Error("Content-Type header not set")
	}
}

func TestAuthenticatedHostClient_StartAgent(t *testing.T) {
	// Create a test store with a host secret
	db, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	// Create a test host
	hostID := "test-host-456"
	secretKey := []byte("another-secret-key-32-bytes!!!!!")

	host := &store.RuntimeHost{
		ID:      hostID,
		Name:    "test-host-2",
		Slug:    "test-host-2",
		Mode:    store.HostModeConnected,
		Status:  store.HostStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := db.CreateRuntimeHost(context.Background(), host); err != nil {
		t.Fatalf("failed to create runtime host: %v", err)
	}

	secret := &store.HostSecret{
		HostID:    hostID,
		SecretKey: secretKey,
		Algorithm: store.HostSecretAlgorithmHMACSHA256,
		Status:    store.HostSecretStatusActive,
		CreatedAt: time.Now(),
	}
	if err := db.CreateHostSecret(context.Background(), secret); err != nil {
		t.Fatalf("failed to create host secret: %v", err)
	}

	// Create a test server
	var receivedPath string
	var receivedMethod string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedMethod = r.Method

		// Verify signature is present
		if r.Header.Get(apiclient.HeaderSignature) == "" {
			t.Error("missing signature header")
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Create authenticated client
	client := NewAuthenticatedHostClient(db, false)

	// Make request
	err = client.StartAgent(context.Background(), hostID, server.URL, "my-agent")
	if err != nil {
		t.Fatalf("StartAgent failed: %v", err)
	}

	if receivedMethod != http.MethodPost {
		t.Errorf("wrong method: got %s, want POST", receivedMethod)
	}

	if receivedPath != "/api/v1/agents/my-agent/start" {
		t.Errorf("wrong path: got %s, want /api/v1/agents/my-agent/start", receivedPath)
	}
}

func TestAuthenticatedHostClient_MissingSecret(t *testing.T) {
	// Create a test store without a secret
	db, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	// Create a test host without a secret
	hostID := "test-host-no-secret"

	host := &store.RuntimeHost{
		ID:      hostID,
		Name:    "test-host-no-secret",
		Slug:    "test-host-no-secret",
		Mode:    store.HostModeConnected,
		Status:  store.HostStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := db.CreateRuntimeHost(context.Background(), host); err != nil {
		t.Fatalf("failed to create runtime host: %v", err)
	}

	// Create a test server that checks if request is unsigned
	var hasSignature bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hasSignature = r.Header.Get(apiclient.HeaderSignature) != ""

		// Return success anyway (simulating permissive mode)
		resp := &RemoteAgentResponse{Created: true}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	// Create authenticated client with debug mode to see warning
	client := NewAuthenticatedHostClient(db, true)

	// Make request - should succeed but without signature
	req := &RemoteCreateAgentRequest{
		AgentID: "agent-1",
		Name:    "test-agent",
		GroveID: "grove-1",
	}

	_, err = client.CreateAgent(context.Background(), hostID, server.URL, req)
	if err != nil {
		t.Fatalf("CreateAgent failed: %v", err)
	}

	// Request should have been sent without signature
	if hasSignature {
		t.Error("expected no signature when secret is missing")
	}
}

func TestAuthenticatedHostClient_ExpiredSecret(t *testing.T) {
	// Create a test store with an expired secret
	db, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	// Create a test host with expired secret
	hostID := "test-host-expired"
	secretKey := []byte("expired-secret-key-32-bytes!!!!!")

	host := &store.RuntimeHost{
		ID:      hostID,
		Name:    "test-host-expired",
		Slug:    "test-host-expired",
		Mode:    store.HostModeConnected,
		Status:  store.HostStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := db.CreateRuntimeHost(context.Background(), host); err != nil {
		t.Fatalf("failed to create runtime host: %v", err)
	}

	secret := &store.HostSecret{
		HostID:    hostID,
		SecretKey: secretKey,
		Algorithm: store.HostSecretAlgorithmHMACSHA256,
		Status:    store.HostSecretStatusActive,
		CreatedAt: time.Now().Add(-2 * time.Hour),
		ExpiresAt: time.Now().Add(-1 * time.Hour), // Expired 1 hour ago
	}
	if err := db.CreateHostSecret(context.Background(), secret); err != nil {
		t.Fatalf("failed to create host secret: %v", err)
	}

	// Create a test server
	var hasSignature bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hasSignature = r.Header.Get(apiclient.HeaderSignature) != ""
		resp := &RemoteAgentResponse{Created: true}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	// Create authenticated client
	client := NewAuthenticatedHostClient(db, true)

	// Make request - should proceed without signature due to expired secret
	req := &RemoteCreateAgentRequest{
		AgentID: "agent-1",
		Name:    "test-agent",
		GroveID: "grove-1",
	}

	_, err = client.CreateAgent(context.Background(), hostID, server.URL, req)
	if err != nil {
		t.Fatalf("CreateAgent failed: %v", err)
	}

	// Request should have been sent without signature
	if hasSignature {
		t.Error("expected no signature when secret is expired")
	}
}

func TestAuthenticatedHostClient_AllOperations(t *testing.T) {
	// Create a test store with a host secret
	db, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	// Create a test host
	hostID := "test-host-ops"
	secretKey := []byte("ops-test-secret-key-32-bytes!!!!")

	host := &store.RuntimeHost{
		ID:      hostID,
		Name:    "test-host-ops",
		Slug:    "test-host-ops",
		Mode:    store.HostModeConnected,
		Status:  store.HostStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := db.CreateRuntimeHost(context.Background(), host); err != nil {
		t.Fatalf("failed to create runtime host: %v", err)
	}

	secret := &store.HostSecret{
		HostID:    hostID,
		SecretKey: secretKey,
		Algorithm: store.HostSecretAlgorithmHMACSHA256,
		Status:    store.HostSecretStatusActive,
		CreatedAt: time.Now(),
	}
	if err := db.CreateHostSecret(context.Background(), secret); err != nil {
		t.Fatalf("failed to create host secret: %v", err)
	}

	// Track requests
	requests := make(map[string]string) // path -> method

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests[r.URL.Path] = r.Method

		// Verify signature on all requests
		if r.Header.Get(apiclient.HeaderSignature) == "" {
			t.Errorf("missing signature for %s %s", r.Method, r.URL.Path)
		}

		// Return appropriate responses
		switch {
		case r.URL.Path == "/api/v1/agents" && r.Method == "POST":
			resp := &RemoteAgentResponse{Created: true}
			json.NewEncoder(w).Encode(resp)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	client := NewAuthenticatedHostClient(db, false)
	ctx := context.Background()

	// Test all operations
	_, err = client.CreateAgent(ctx, hostID, server.URL, &RemoteCreateAgentRequest{Name: "test"})
	if err != nil {
		t.Errorf("CreateAgent failed: %v", err)
	}

	err = client.StartAgent(ctx, hostID, server.URL, "test-agent")
	if err != nil {
		t.Errorf("StartAgent failed: %v", err)
	}

	err = client.StopAgent(ctx, hostID, server.URL, "test-agent")
	if err != nil {
		t.Errorf("StopAgent failed: %v", err)
	}

	err = client.RestartAgent(ctx, hostID, server.URL, "test-agent")
	if err != nil {
		t.Errorf("RestartAgent failed: %v", err)
	}

	err = client.DeleteAgent(ctx, hostID, server.URL, "test-agent", true, true)
	if err != nil {
		t.Errorf("DeleteAgent failed: %v", err)
	}

	err = client.MessageAgent(ctx, hostID, server.URL, "test-agent", "hello", false)
	if err != nil {
		t.Errorf("MessageAgent failed: %v", err)
	}

	// Verify all requests were made
	expectedPaths := []string{
		"/api/v1/agents",
		"/api/v1/agents/test-agent/start",
		"/api/v1/agents/test-agent/stop",
		"/api/v1/agents/test-agent/restart",
		"/api/v1/agents/test-agent",
		"/api/v1/agents/test-agent/message",
	}

	for _, path := range expectedPaths {
		if _, ok := requests[path]; !ok {
			t.Errorf("missing request to %s", path)
		}
	}
}
