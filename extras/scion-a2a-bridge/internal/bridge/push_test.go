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
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
)

func newTestBridge(t *testing.T) *Bridge {
	t.Helper()
	dir := t.TempDir()
	store, err := state.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	cfg := &Config{
		Timeouts: TimeoutConfig{PushRetryMax: 3},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(store, nil, nil, cfg, log)
}

func TestPushDispatcherSendsWebhook(t *testing.T) {
	var received atomic.Int32
	var lastContentType, lastAuthHeader string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		lastContentType = r.Header.Get("Content-Type")
		lastAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	dir := t.TempDir()
	store, err := state.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now()
	store.CreateTask(&state.Task{
		ID: "task-1", ContextID: "ctx-1", GroveID: "g1", AgentSlug: "a1",
		State: "working", CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	})
	store.SetPushConfig(&state.PushNotificationConfig{
		ID:              "push-1",
		TaskID:          "task-1",
		URL:             ts.URL,
		Token:           "my-token",
		AuthScheme:      "",
		AuthCredentials: "",
		CreatedAt:       now,
	})

	cfg := &Config{Timeouts: TimeoutConfig{PushRetryMax: 3}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pd := NewPushDispatcher(store, cfg, log)

	event := StreamEvent{
		StatusUpdate: &TaskStatusUpdate{
			TaskID: "task-1",
			Status: TaskStatus{State: TaskStateCompleted},
			Final:  true,
		},
	}

	pd.sendWithRetry(state.PushNotificationConfig{
		ID:    "push-1",
		URL:   ts.URL,
		Token: "my-token",
	}, event)

	if received.Load() != 1 {
		t.Errorf("received = %d, want 1", received.Load())
	}
	if lastContentType != "application/a2a+json" {
		t.Errorf("Content-Type = %q, want %q", lastContentType, "application/a2a+json")
	}
	if lastAuthHeader != "Bearer my-token" {
		t.Errorf("Authorization = %q, want %q", lastAuthHeader, "Bearer my-token")
	}
}

func TestPushDispatcherAuthScheme(t *testing.T) {
	var lastAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	dir := t.TempDir()
	store, err := state.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := &Config{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pd := NewPushDispatcher(store, cfg, log)

	// Use send directly (not sendWithRetry) for auth header verification.
	if err := pd.send(state.PushNotificationConfig{
		ID:              "push-1",
		URL:             ts.URL,
		AuthScheme:      "ApiKey",
		AuthCredentials: "secret-key-123",
	}, StreamEvent{
		StatusUpdate: &TaskStatusUpdate{
			TaskID: "task-1",
			Status: TaskStatus{State: TaskStateWorking},
		},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	if lastAuth != "ApiKey secret-key-123" {
		t.Errorf("Authorization = %q, want %q", lastAuth, "ApiKey secret-key-123")
	}
}

func TestPushDispatcherRetriesOnFailure(t *testing.T) {
	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := attempts.Add(1)
		if count <= 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	dir := t.TempDir()
	store, err := state.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// PushRetryMax=1 means: 1 initial attempt + 1 retry = 2 total, with 2s backoff.
	cfg := &Config{Timeouts: TimeoutConfig{PushRetryMax: 1}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pd := NewPushDispatcher(store, cfg, log)

	pd.sendWithRetry(state.PushNotificationConfig{
		ID:  "push-1",
		URL: ts.URL,
	}, StreamEvent{
		StatusUpdate: &TaskStatusUpdate{
			TaskID: "task-1",
			Status: TaskStatus{State: TaskStateWorking},
		},
	})

	if attempts.Load() != 2 {
		t.Errorf("attempts = %d, want 2 (1 failure + 1 success)", attempts.Load())
	}
}

func TestPushDispatcherDeletesAfterExhaustedRetries(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	dir := t.TempDir()
	store, err := state.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now()
	store.CreateTask(&state.Task{
		ID: "task-1", ContextID: "ctx-1", GroveID: "g1", AgentSlug: "a1",
		State: "working", CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	})
	store.SetPushConfig(&state.PushNotificationConfig{
		ID: "push-del", TaskID: "task-1", URL: ts.URL, CreatedAt: now,
	})

	configs, _ := store.GetPushConfigsByTask("task-1")
	if len(configs) != 1 {
		t.Fatalf("expected 1 config before, got %d", len(configs))
	}

	// Use PushRetryMax=0 so it only makes one attempt (no retries), then deletes.
	cfg := &Config{Timeouts: TimeoutConfig{PushRetryMax: 0}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pd := NewPushDispatcher(store, cfg, log)

	pd.sendWithRetry(state.PushNotificationConfig{
		ID:  "push-del",
		URL: ts.URL,
	}, StreamEvent{
		StatusUpdate: &TaskStatusUpdate{
			TaskID: "task-1",
			Status: TaskStatus{State: TaskStateWorking},
		},
	})

	configs, _ = store.GetPushConfigsByTask("task-1")
	if len(configs) != 0 {
		t.Errorf("expected 0 configs after exhausted retries, got %d", len(configs))
	}
}

func TestPushDispatcherWebhookPayload(t *testing.T) {
	var receivedBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	dir := t.TempDir()
	store, err := state.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := &Config{Timeouts: TimeoutConfig{PushRetryMax: 0}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pd := NewPushDispatcher(store, cfg, log)

	event := StreamEvent{
		ArtifactUpdate: &TaskArtifactUpdate{
			TaskID: "task-1",
			Artifact: Artifact{
				ArtifactID: "art-1",
				Parts:      []Part{{Text: "result text"}},
				LastChunk:  true,
			},
		},
	}

	pd.sendWithRetry(state.PushNotificationConfig{
		ID:  "push-1",
		URL: ts.URL,
	}, event)

	var decoded StreamEvent
	if err := json.Unmarshal(receivedBody, &decoded); err != nil {
		t.Fatalf("unmarshal webhook body: %v", err)
	}
	if decoded.ArtifactUpdate == nil {
		t.Fatal("expected artifact update in webhook payload")
	}
	if decoded.ArtifactUpdate.Artifact.ArtifactID != "art-1" {
		t.Errorf("ArtifactID = %q, want %q", decoded.ArtifactUpdate.Artifact.ArtifactID, "art-1")
	}
}

func TestBridgePushConfigCRUD(t *testing.T) {
	b := newTestBridge(t)

	now := time.Now()
	b.store.CreateTask(&state.Task{
		ID: "task-1", ContextID: "ctx-1", GroveID: "g1", AgentSlug: "a1",
		State: "working", CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	})

	// Set config.
	cfg, err := b.SetPushNotificationConfig(nil, "task-1", "https://example.com/webhook", "tok123", "", "")
	if err != nil {
		t.Fatalf("SetPushNotificationConfig: %v", err)
	}
	if cfg.URL != "https://example.com/webhook" {
		t.Errorf("URL = %q, want %q", cfg.URL, "https://example.com/webhook")
	}
	if cfg.ID == "" {
		t.Error("expected non-empty config ID")
	}

	// Get configs.
	configs, err := b.GetPushNotificationConfig(nil, "task-1")
	if err != nil {
		t.Fatalf("GetPushNotificationConfig: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("got %d configs, want 1", len(configs))
	}

	// Delete config.
	if err := b.DeletePushNotificationConfig(nil, cfg.ID); err != nil {
		t.Fatalf("DeletePushNotificationConfig: %v", err)
	}

	configs, err = b.GetPushNotificationConfig(nil, "task-1")
	if err != nil {
		t.Fatalf("GetPushNotificationConfig after delete: %v", err)
	}
	if len(configs) != 0 {
		t.Errorf("got %d configs after delete, want 0", len(configs))
	}
}

func TestBridgeSetPushConfigTaskNotFound(t *testing.T) {
	b := newTestBridge(t)

	_, err := b.SetPushNotificationConfig(nil, "nonexistent-task", "https://example.com/webhook", "", "", "")
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
}
