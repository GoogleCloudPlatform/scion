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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ptone/scion-agent/pkg/apiclient"
	"github.com/ptone/scion-agent/pkg/store"
	"github.com/ptone/scion-agent/pkg/store/sqlite"
)

func TestAuthenticatedBrokerClient_CreateAgent(t *testing.T) {
	// Create a test store with a broker secret
	db, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	// Create a test broker
	brokerID := "test-host-123"
	secretKey := []byte("test-secret-key-32-bytes-long!!!")

	broker := &store.RuntimeBroker{
		ID:      brokerID,
		Name:    "test-host",
		Slug:    "test-host",
				Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := db.CreateRuntimeBroker(context.Background(), broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	secret := &store.BrokerSecret{
		BrokerID:    brokerID,
		SecretKey: secretKey,
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		Status:    store.BrokerSecretStatusActive,
		CreatedAt: time.Now(),
	}
	if err := db.CreateBrokerSecret(context.Background(), secret); err != nil {
		t.Fatalf("failed to create broker secret: %v", err)
	}

	// Create a test server that validates HMAC signatures
	var receivedHeaders http.Header
	var requestValidated bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()

		// Verify HMAC headers are present
		if r.Header.Get(apiclient.HeaderBrokerID) == "" {
			t.Error("missing X-Scion-Broker-ID header")
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

		// Verify broker ID matches
		if got := r.Header.Get(apiclient.HeaderBrokerID); got != brokerID {
			t.Errorf("wrong broker ID: got %s, want %s", got, brokerID)
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
	client := NewAuthenticatedBrokerClient(db, true)

	// Make request
	req := &RemoteCreateAgentRequest{
		Slug: "agent-1",
		Name:    "test-agent",
		GroveID: "grove-1",
	}

	resp, err := client.CreateAgent(context.Background(), brokerID, server.URL, req)
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

func TestAuthenticatedBrokerClient_StartAgent(t *testing.T) {
	// Create a test store with a broker secret
	db, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	// Create a test broker
	brokerID := "test-host-456"
	secretKey := []byte("another-secret-key-32-bytes!!!!!")

	broker := &store.RuntimeBroker{
		ID:      brokerID,
		Name:    "test-host-2",
		Slug:    "test-host-2",
				Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := db.CreateRuntimeBroker(context.Background(), broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	secret := &store.BrokerSecret{
		BrokerID:    brokerID,
		SecretKey: secretKey,
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		Status:    store.BrokerSecretStatusActive,
		CreatedAt: time.Now(),
	}
	if err := db.CreateBrokerSecret(context.Background(), secret); err != nil {
		t.Fatalf("failed to create broker secret: %v", err)
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
	client := NewAuthenticatedBrokerClient(db, false)

	// Make request
	err = client.StartAgent(context.Background(), brokerID, server.URL, "my-agent")
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

func TestAuthenticatedBrokerClient_MissingSecret(t *testing.T) {
	// Create a test store without a secret
	db, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	// Create a test broker without a secret
	brokerID := "test-host-no-secret"

	broker := &store.RuntimeBroker{
		ID:      brokerID,
		Name:    "test-host-no-secret",
		Slug:    "test-host-no-secret",
				Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := db.CreateRuntimeBroker(context.Background(), broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
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
	client := NewAuthenticatedBrokerClient(db, true)

	// Make request - should succeed but without signature
	req := &RemoteCreateAgentRequest{
		Slug: "agent-1",
		Name:    "test-agent",
		GroveID: "grove-1",
	}

	_, err = client.CreateAgent(context.Background(), brokerID, server.URL, req)
	if err != nil {
		t.Fatalf("CreateAgent failed: %v", err)
	}

	// Request should have been sent without signature
	if hasSignature {
		t.Error("expected no signature when secret is missing")
	}
}

func TestAuthenticatedBrokerClient_ExpiredSecret(t *testing.T) {
	// Create a test store with an expired secret
	db, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	// Create a test broker with expired secret
	brokerID := "test-host-expired"
	secretKey := []byte("expired-secret-key-32-bytes!!!!!")

	broker := &store.RuntimeBroker{
		ID:      brokerID,
		Name:    "test-host-expired",
		Slug:    "test-host-expired",
				Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := db.CreateRuntimeBroker(context.Background(), broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	secret := &store.BrokerSecret{
		BrokerID:    brokerID,
		SecretKey: secretKey,
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		Status:    store.BrokerSecretStatusActive,
		CreatedAt: time.Now().Add(-2 * time.Hour),
		ExpiresAt: time.Now().Add(-1 * time.Hour), // Expired 1 hour ago
	}
	if err := db.CreateBrokerSecret(context.Background(), secret); err != nil {
		t.Fatalf("failed to create broker secret: %v", err)
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
	client := NewAuthenticatedBrokerClient(db, true)

	// Make request - should proceed without signature due to expired secret
	req := &RemoteCreateAgentRequest{
		Slug: "agent-1",
		Name:    "test-agent",
		GroveID: "grove-1",
	}

	_, err = client.CreateAgent(context.Background(), brokerID, server.URL, req)
	if err != nil {
		t.Fatalf("CreateAgent failed: %v", err)
	}

	// Request should have been sent without signature
	if hasSignature {
		t.Error("expected no signature when secret is expired")
	}
}

func TestAuthenticatedBrokerClient_AllOperations(t *testing.T) {
	// Create a test store with a broker secret
	db, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	// Create a test broker
	brokerID := "test-host-ops"
	secretKey := []byte("ops-test-secret-key-32-bytes!!!!")

	broker := &store.RuntimeBroker{
		ID:      brokerID,
		Name:    "test-host-ops",
		Slug:    "test-host-ops",
				Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := db.CreateRuntimeBroker(context.Background(), broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	secret := &store.BrokerSecret{
		BrokerID:    brokerID,
		SecretKey: secretKey,
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		Status:    store.BrokerSecretStatusActive,
		CreatedAt: time.Now(),
	}
	if err := db.CreateBrokerSecret(context.Background(), secret); err != nil {
		t.Fatalf("failed to create broker secret: %v", err)
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

	client := NewAuthenticatedBrokerClient(db, false)
	ctx := context.Background()

	// Test all operations
	_, err = client.CreateAgent(ctx, brokerID, server.URL, &RemoteCreateAgentRequest{Name: "test"})
	if err != nil {
		t.Errorf("CreateAgent failed: %v", err)
	}

	err = client.StartAgent(ctx, brokerID, server.URL, "test-agent")
	if err != nil {
		t.Errorf("StartAgent failed: %v", err)
	}

	err = client.StopAgent(ctx, brokerID, server.URL, "test-agent")
	if err != nil {
		t.Errorf("StopAgent failed: %v", err)
	}

	err = client.RestartAgent(ctx, brokerID, server.URL, "test-agent")
	if err != nil {
		t.Errorf("RestartAgent failed: %v", err)
	}

	err = client.DeleteAgent(ctx, brokerID, server.URL, "test-agent", true, true)
	if err != nil {
		t.Errorf("DeleteAgent failed: %v", err)
	}

	err = client.MessageAgent(ctx, brokerID, server.URL, "test-agent", "hello", false)
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
