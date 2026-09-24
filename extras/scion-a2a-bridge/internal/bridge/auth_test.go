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
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
)

// testHandler is a minimal handler that echoes back the CallerIdentity if present.
func testHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller := callerIdentityFromContext(r.Context())
		if caller != nil {
			json.NewEncoder(w).Encode(map[string]string{
				"user_id":    caller.UserID,
				"email":      caller.Email,
				"token_type": caller.TokenType,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"caller":"none"}`))
	})
}

func TestAuthMiddleware_AllSchemes(t *testing.T) {
	// Set up a mock Hub for UAT validation.
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/me" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		auth := r.Header.Get("Authorization")
		switch auth {
		case "Bearer scion_pat_valid":
			_ = json.NewEncoder(w).Encode(userResponse{
				ID:    "uat-user-1",
				Email: "alice@example.com",
				Role:  "user",
			})
			return
		case "Bearer google-id-token-valid":
			// Not a scion_pat_* token — stands in for a Google credential the
			// Hub's external-bearer path admitted via its own trust config.
			// hubBearer has no prefix check, so this must be accepted.
			_ = json.NewEncoder(w).Encode(userResponse{
				ID:    "bearer-user-1",
				Email: "bearer-user@example.com",
				Role:  "member",
			})
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer hub.Close()

	tests := []struct {
		name       string
		scheme     string
		apiKey     string
		header     string // header name
		headerVal  string // header value
		wantStatus int
		wantCaller string // expected user_id from CallerIdentity, or "none"
	}{
		{
			name:       "apiKey/valid",
			scheme:     "apiKey",
			apiKey:     "my-secret",
			header:     "X-API-Key",
			headerVal:  "my-secret",
			wantStatus: http.StatusOK,
			wantCaller: "none",
		},
		{
			name:       "apiKey/invalid",
			scheme:     "apiKey",
			apiKey:     "my-secret",
			header:     "X-API-Key",
			headerVal:  "wrong-key",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "apiKey/missing",
			scheme:     "apiKey",
			apiKey:     "my-secret",
			header:     "",
			headerVal:  "",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "bearer/valid",
			scheme:     "bearer",
			apiKey:     "my-secret",
			header:     "Authorization",
			headerVal:  "Bearer my-secret",
			wantStatus: http.StatusOK,
			wantCaller: "none",
		},
		{
			name:       "bearer/invalid",
			scheme:     "bearer",
			apiKey:     "my-secret",
			header:     "Authorization",
			headerVal:  "Bearer wrong",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "none/no-header",
			scheme:     "none",
			header:     "",
			headerVal:  "",
			wantStatus: http.StatusOK,
			wantCaller: "none",
		},
		{
			name:       "hubUAT/valid",
			scheme:     "hubUAT",
			header:     "Authorization",
			headerVal:  "Bearer scion_pat_valid",
			wantStatus: http.StatusOK,
			wantCaller: "uat-user-1",
		},
		{
			name:       "hubUAT/invalid",
			scheme:     "hubUAT",
			header:     "Authorization",
			headerVal:  "Bearer scion_pat_bad",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "hubUAT/not-pat-prefix",
			scheme:     "hubUAT",
			header:     "Authorization",
			headerVal:  "Bearer not-a-pat",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "hubUAT/missing",
			scheme:     "hubUAT",
			header:     "",
			headerVal:  "",
			wantStatus: http.StatusUnauthorized,
		},
		{
			// B1: a Google-shaped (non scion_pat_) credential is admitted via
			// /auth/me under hubBearer, unlike hubUAT (see hubUAT/not-pat-prefix
			// above, and TestAuthMiddleware_HubUATVsHubBearer_PrefixGuard for the
			// direct contrast on the identical token).
			name:       "hubBearer/valid-google-token",
			scheme:     "hubBearer",
			header:     "Authorization",
			headerVal:  "Bearer google-id-token-valid",
			wantStatus: http.StatusOK,
			wantCaller: "bearer-user-1",
		},
		{
			// hubBearer also still accepts a scion_pat_* token — the Hub is
			// the sole arbiter, the bridge does not gate on the prefix.
			name:       "hubBearer/valid-pat-token",
			scheme:     "hubBearer",
			header:     "Authorization",
			headerVal:  "Bearer scion_pat_valid",
			wantStatus: http.StatusOK,
			wantCaller: "uat-user-1",
		},
		{
			name:       "hubBearer/rejected-by-hub",
			scheme:     "hubBearer",
			header:     "Authorization",
			headerVal:  "Bearer garbage-token",
			wantStatus: http.StatusUnauthorized,
		},
		{
			// B4 (empty token -> 401).
			name:       "hubBearer/missing",
			scheme:     "hubBearer",
			header:     "",
			headerVal:  "",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := state.NewSQLite(filepath.Join(dir, "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()

			cfg := &Config{
				Bridge: BridgeConfig{ExternalURL: "https://test"},
				Hub:    HubConfig{Endpoint: hub.URL, User: "admin@test"},
				Auth:   AuthConfig{Scheme: tt.scheme, APIKey: tt.apiKey},
			}

			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			b := New(store, nil, nil, cfg, nil, log)
			srv := NewServer(b, cfg, nil, log, testHandler())

			mw := srv.authMiddleware(testHandler())
			req := httptest.NewRequest(http.MethodPost, "/projects/p/agents/a/jsonrpc", nil)
			if tt.header != "" {
				req.Header.Set(tt.header, tt.headerVal)
			}

			w := httptest.NewRecorder()
			mw.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}

			if tt.wantStatus == http.StatusOK && tt.wantCaller != "" {
				var body map[string]string
				json.NewDecoder(w.Body).Decode(&body)
				if tt.wantCaller == "none" {
					if body["caller"] != "none" && body["user_id"] != "" {
						t.Errorf("expected no caller identity, got %v", body)
					}
				} else {
					if body["user_id"] != tt.wantCaller {
						t.Errorf("caller user_id = %q, want %q", body["user_id"], tt.wantCaller)
					}
				}
			}
		})
	}
}

func TestAuthMiddleware_HubJWT(t *testing.T) {
	signingKey := testSigningKey(t)

	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := &Config{
		Bridge: BridgeConfig{ExternalURL: "https://test"},
		Hub:    HubConfig{Endpoint: "http://hub", User: "admin@test"},
		Auth:   AuthConfig{Scheme: "hubJWT"},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := New(store, nil, nil, cfg, nil, log)
	srv := NewServer(b, cfg, nil, log, testHandler())
	srv.SetJWTValidator(NewJWTValidator(signingKey))

	// Valid JWT
	claims := validClaims()
	token := mintTestJWT(t, signingKey, claims)

	mw := srv.authMiddleware(testHandler())
	req := httptest.NewRequest(http.MethodPost, "/projects/p/agents/a/jsonrpc", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	w := httptest.NewRecorder()
	mw.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("valid JWT: status = %d, want 200", w.Code)
	}

	var body map[string]string
	json.NewDecoder(w.Body).Decode(&body)
	if body["user_id"] != "user-1" {
		t.Errorf("user_id = %q, want %q", body["user_id"], "user-1")
	}
	if body["token_type"] != "jwt" {
		t.Errorf("token_type = %q, want %q", body["token_type"], "jwt")
	}

	// Expired JWT
	expiredClaims := validClaims()
	expiredClaims.Expiry = jwt.NewNumericDate(time.Now().Add(-1 * time.Hour))
	expiredToken := mintTestJWT(t, signingKey, expiredClaims)

	req2 := httptest.NewRequest(http.MethodPost, "/projects/p/agents/a/jsonrpc", nil)
	req2.Header.Set("Authorization", "Bearer "+expiredToken)

	w2 := httptest.NewRecorder()
	mw.ServeHTTP(w2, req2)

	if w2.Code != http.StatusUnauthorized {
		t.Errorf("expired JWT: status = %d, want 401", w2.Code)
	}
}

func TestAuthMiddleware_PublicEndpoints(t *testing.T) {
	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := &Config{
		Bridge: BridgeConfig{ExternalURL: "https://test"},
		Hub:    HubConfig{Endpoint: "http://hub", User: "admin@test"},
		Auth:   AuthConfig{Scheme: "hubUAT"},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := New(store, nil, nil, cfg, nil, log)
	srv := NewServer(b, cfg, nil, log, testHandler())

	mw := srv.authMiddleware(testHandler())

	paths := []string{
		"/.well-known/agent-card.json",
		"/healthz",
		"/readyz",
		"/projects/test/agents/agent/.well-known/agent-card.json",
		"/projects/test/agents/agent/.well-known/agent-card.json",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			// No auth headers
			w := httptest.NewRecorder()
			mw.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("public endpoint %s: status = %d, want 200", path, w.Code)
			}
		})
	}
}

func TestPerUserTaskIsolation(t *testing.T) {
	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "iso-test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := &Config{
		Bridge: BridgeConfig{ExternalURL: "https://test"},
		Hub:    HubConfig{Endpoint: "http://hub", User: "admin@test"},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := New(store, nil, nil, cfg, nil, log)

	now := time.Now()

	// Create tasks for two different users.
	store.CreateTask(context.Background(), &state.Task{
		ID: "task-alice", ContextID: "ctx-1", ProjectID: "proj-1",
		AgentSlug: "agent-1", State: "working", CallerUserID: "user-alice",
		CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	})
	store.CreateTask(context.Background(), &state.Task{
		ID: "task-bob", ContextID: "ctx-1", ProjectID: "proj-1",
		AgentSlug: "agent-1", State: "working", CallerUserID: "user-bob",
		CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	})
	// Create a legacy task (no caller).
	store.CreateTask(context.Background(), &state.Task{
		ID: "task-legacy", ContextID: "ctx-1", ProjectID: "proj-1",
		AgentSlug: "agent-1", State: "working", CallerUserID: "",
		CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	})

	aliceCtx := withCallerIdentity(context.Background(), &CallerIdentity{
		UserID: "user-alice", Email: "alice@test", TokenType: "uat",
	})
	bobCtx := withCallerIdentity(context.Background(), &CallerIdentity{
		UserID: "user-bob", Email: "bob@test", TokenType: "uat",
	})
	legacyCtx := context.Background() // no caller identity

	// Alice can get her own task.
	result, err := b.GetTask(aliceCtx, "task-alice")
	if err != nil {
		t.Fatalf("Alice GetTask own: %v", err)
	}
	if result == nil {
		t.Fatal("Alice should be able to get her own task")
	}

	// Alice cannot get Bob's task.
	result, err = b.GetTask(aliceCtx, "task-bob")
	if err != nil {
		t.Fatalf("Alice GetTask bob: %v", err)
	}
	if result != nil {
		t.Fatal("Alice should NOT be able to get Bob's task")
	}

	// Alice cannot see legacy tasks.
	result, err = b.GetTask(aliceCtx, "task-legacy")
	if err != nil {
		t.Fatalf("Alice GetTask legacy: %v", err)
	}
	if result != nil {
		t.Fatal("Alice (per-user) should NOT see legacy tasks")
	}

	// Legacy caller can see legacy tasks.
	result, err = b.GetTask(legacyCtx, "task-legacy")
	if err != nil {
		t.Fatalf("Legacy GetTask legacy: %v", err)
	}
	if result == nil {
		t.Fatal("Legacy caller should see legacy tasks")
	}

	// Legacy caller can see per-user tasks (no isolation without CallerIdentity).
	result, err = b.GetTask(legacyCtx, "task-alice")
	if err != nil {
		t.Fatalf("Legacy GetTask alice: %v", err)
	}
	if result == nil {
		t.Fatal("Legacy caller should see all tasks")
	}

	// ListTasks: Alice only sees her tasks.
	results, err := b.ListTasks(aliceCtx, "ctx-1")
	if err != nil {
		t.Fatalf("Alice ListTasks: %v", err)
	}
	if len(results) != 1 || results[0].ID != "task-alice" {
		t.Errorf("Alice ListTasks = %d tasks, want 1 (task-alice)", len(results))
	}

	// ListTasks: Bob only sees his tasks.
	results, err = b.ListTasks(bobCtx, "ctx-1")
	if err != nil {
		t.Fatalf("Bob ListTasks: %v", err)
	}
	if len(results) != 1 || results[0].ID != "task-bob" {
		t.Errorf("Bob ListTasks = %d tasks, want 1 (task-bob)", len(results))
	}

	// ListTasks: Legacy sees all tasks.
	results, err = b.ListTasks(legacyCtx, "ctx-1")
	if err != nil {
		t.Fatalf("Legacy ListTasks: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("Legacy ListTasks = %d tasks, want 3", len(results))
	}

	// CancelTask: Alice cannot cancel Bob's task.
	cancelResult, err := b.CancelTask(aliceCtx, "task-bob")
	if err != nil {
		t.Fatalf("Alice CancelTask bob: %v", err)
	}
	if cancelResult != nil {
		t.Fatal("Alice should NOT be able to cancel Bob's task")
	}
}

func TestValidateConfig_NewSchemes(t *testing.T) {
	base := Config{
		Bridge: BridgeConfig{ExternalURL: "https://test"},
		Hub:    HubConfig{Endpoint: "http://hub", User: "admin@test"},
	}

	tests := []struct {
		name        string
		auth        AuthConfig
		hubOverride *HubConfig // if non-nil, overrides base Hub config
		wantErr     bool
	}{
		{
			name:    "hubUAT/valid",
			auth:    AuthConfig{Scheme: "hubUAT"},
			wantErr: false,
		},
		{
			name:        "hubJWT/valid",
			auth:        AuthConfig{Scheme: "hubJWT"},
			hubOverride: &HubConfig{Endpoint: "http://hub", User: "admin@test", SigningKey: "test-secret-key"},
			wantErr:     false,
		},
		{
			name:    "hubJWT/missing-signing-key",
			auth:    AuthConfig{Scheme: "hubJWT"},
			wantErr: true,
		},
		{
			name:    "hubUAT/with-ttl",
			auth:    AuthConfig{Scheme: "hubUAT", UATCacheTTL: 120 * time.Second},
			wantErr: false,
		},
		{
			name:    "hubUAT/ttl-too-high",
			auth:    AuthConfig{Scheme: "hubUAT", UATCacheTTL: 600 * time.Second},
			wantErr: true,
		},
		{
			name:    "hubUAT/negative-ttl",
			auth:    AuthConfig{Scheme: "hubUAT", UATCacheTTL: -1 * time.Second},
			wantErr: true,
		},
		{
			name:    "hubUAT/no-api-key-needed",
			auth:    AuthConfig{Scheme: "hubUAT"},
			wantErr: false,
		},
		{
			name:    "hubBearer/valid",
			auth:    AuthConfig{Scheme: "hubBearer"},
			wantErr: false,
		},
		{
			name:    "hubBearer/with-ttl",
			auth:    AuthConfig{Scheme: "hubBearer", UATCacheTTL: 120 * time.Second},
			wantErr: false,
		},
		{
			name:    "hubBearer/ttl-too-high",
			auth:    AuthConfig{Scheme: "hubBearer", UATCacheTTL: 600 * time.Second},
			wantErr: true,
		},
		{
			name:    "unsupported/scheme",
			auth:    AuthConfig{Scheme: "oauth2"},
			wantErr: true,
		},
		{
			name:    "apiKey/still-works",
			auth:    AuthConfig{Scheme: "apiKey", APIKey: "key"},
			wantErr: false,
		},
		{
			name:    "apiKey/missing-key",
			auth:    AuthConfig{Scheme: "apiKey"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			cfg.Auth = tt.auth
			if tt.hubOverride != nil {
				cfg.Hub = *tt.hubOverride
			}
			err := ValidateConfig(&cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateConfig() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

// wantPlainErrorBody reconstructs the exact bytes http.Error writes for a
// given message: the message plus a single trailing newline (net/http's
// Error calls fmt.Fprintln). Comparing against this — rather than a prefix
// or substring — fails the test if the wording changes, or if an underlying
// error reason ever leaks into the body.
func wantPlainErrorBody(message string) []byte {
	return []byte(message + "\n")
}

func newHubBearerMiddleware(t *testing.T, hubURL, scheme string) http.Handler {
	t.Helper()
	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := &Config{
		Bridge: BridgeConfig{ExternalURL: "https://test"},
		Hub:    HubConfig{Endpoint: hubURL, User: "admin@test"},
		Auth:   AuthConfig{Scheme: scheme},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := New(store, nil, nil, cfg, nil, log)
	srv := NewServer(b, cfg, nil, log, testHandler())
	return srv.authMiddleware(testHandler())
}

// TestAuthMiddleware_HubBearer_EmptyToken proves the empty-token guard in the
// hubBearer case (server.go) — not an incidental Hub-side rejection — is what
// rejects a missing credential. The mock Hub below is deliberately
// permissive: it treats ANY request (including one with no Authorization
// header) as a valid identity. If the "token == \"\"" guard were ever
// deleted, the request would fall through to uatV.Validate and this
// permissive Hub would admit it, turning this test's expected 401 into a 200
// and failing it.
func TestAuthMiddleware_HubBearer_EmptyToken(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(userResponse{ID: "should-never-be-reached", Email: "nobody@example.com", Role: "member"})
	}))
	defer hub.Close()

	mw := newHubBearerMiddleware(t, hub.URL, "hubBearer")

	req := httptest.NewRequest(http.MethodPost, "/projects/p/agents/a/jsonrpc", nil)
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (empty-token guard must reject before reaching the permissive mock Hub)", w.Code)
	}
	want := wantPlainErrorBody("unauthorized: missing bearer token")
	if !bytes.Equal(w.Body.Bytes(), want) {
		t.Errorf("body = %q, want %q", w.Body.Bytes(), want)
	}
}

// TestAuthMiddleware_HubBearer_RejectedByHub proves the exact rejection
// response the design mandates (§4.6): 401, fixed message, and the
// underlying Hub rejection reason never appears in the response body
// (logged at Info instead). Exact-byte comparison catches a mutation that
// leaks the reason (e.g. interpolating err.Error() into the message).
// TestAuthMiddleware_HubBearer_RejectedByHub also proves the rejection log
// line (server.go's "bearer token rejected by Scion Hub" Info log) never
// contains the token itself (review r2 O1; M9: adding `"token", token` to
// that log call must fail this test). hubBearer forwards third-party Google
// credentials, so a token leaking into logs is a real exposure, not just a
// hypothetical one.
func TestAuthMiddleware_HubBearer_RejectedByHub(t *testing.T) {
	const token = "some-token-the-hub-rejects"
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "very specific reason the caller must never see", http.StatusUnauthorized)
	}))
	defer hub.Close()

	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))

	cfg := &Config{
		Bridge: BridgeConfig{ExternalURL: "https://test"},
		Hub:    HubConfig{Endpoint: hub.URL, User: "admin@test"},
		Auth:   AuthConfig{Scheme: "hubBearer"},
	}
	b := New(store, nil, nil, cfg, nil, log)
	srv := NewServer(b, cfg, nil, log, testHandler())
	mw := srv.authMiddleware(testHandler())

	req := httptest.NewRequest(http.MethodPost, "/projects/p/agents/a/jsonrpc", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	want := wantPlainErrorBody("unauthorized: token rejected by Scion Hub")
	if !bytes.Equal(w.Body.Bytes(), want) {
		t.Errorf("body = %q, want %q", w.Body.Bytes(), want)
	}

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "rejected") {
		t.Fatalf("expected a rejection log line, got: %q", logOutput)
	}
	if strings.Contains(logOutput, token) {
		t.Errorf("rejection log leaked the bearer token: %q", logOutput)
	}
}

// TestAuthMiddleware_HubUATVsHubBearer_PrefixGuard sends the identical
// non-scion_pat_ credential to both schemes against the same Hub. B2 (hubUAT
// is unchanged) and hubBearer's defining difference (no prefix check) are
// proven side by side, against the same fixture, so a change that
// accidentally shares hubUAT's prefix guard with hubBearer (or removes
// hubUAT's) fails one half or the other.
func TestAuthMiddleware_HubUATVsHubBearer_PrefixGuard(t *testing.T) {
	const token = "not-a-scion-pat-but-a-valid-hub-credential"
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+token {
			_ = json.NewEncoder(w).Encode(userResponse{ID: "user-1", Email: "user@example.com", Role: "member"})
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer hub.Close()

	t.Run("hubUAT_still_rejects", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/projects/p/agents/a/jsonrpc", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		newHubBearerMiddleware(t, hub.URL, "hubUAT").ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("hubUAT status = %d, want 401", w.Code)
		}
		want := wantPlainErrorBody("unauthorized: expected scion_pat_* token")
		if !bytes.Equal(w.Body.Bytes(), want) {
			t.Errorf("hubUAT body = %q, want %q", w.Body.Bytes(), want)
		}
	})

	t.Run("hubBearer_accepts_same_token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/projects/p/agents/a/jsonrpc", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		newHubBearerMiddleware(t, hub.URL, "hubBearer").ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("hubBearer status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		var body map[string]string
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["user_id"] != "user-1" || body["token_type"] != "bearer" {
			t.Errorf("hubBearer caller = %+v, want user_id=user-1 token_type=bearer", body)
		}
	})
}

// TestAuthMiddleware_HubBearer_ProductionSnapshotWiring proves hubBearer
// works through the same snapshot-based validator wiring cmd/scion-a2a-bridge
// uses in production (main.go always calls SetSnapshot; authMiddleware then
// reads uatV from the snapshot, not from the constructor-time
// s.uatValidator). Every other hubBearer test in this file builds the
// middleware via newHubBearerMiddleware, which never calls SetSnapshot and so
// only ever exercises the s.uatValidator fallback branch (server.go:522-527)
// — a real deployment never takes that branch. Without adminoverlay.go's
// "hubUAT", "hubBearer" case in BuildAuthValidators, a real hubBearer
// deployment 500s on every request (snap.Auth.UATValidator == nil) despite
// every one of those other tests passing. See server_test.go:694-698 for the
// same SetSnapshot pattern used for hubJWT.
func TestAuthMiddleware_HubBearer_ProductionSnapshotWiring(t *testing.T) {
	const token = "hub-accepted-non-pat-credential"
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+token {
			_ = json.NewEncoder(w).Encode(userResponse{ID: "snapshot-user-1", Email: "snapshot@example.com", Role: "member"})
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer hub.Close()

	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	cfg := &Config{
		Bridge: BridgeConfig{ExternalURL: "https://test"},
		Hub:    HubConfig{Endpoint: hub.URL, User: "admin@test"},
		Auth:   AuthConfig{Scheme: "hubBearer"},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := New(store, nil, nil, cfg, nil, log)
	srv := NewServer(b, cfg, nil, log, testHandler())

	// Simulate what main.go always does in production: build and install a
	// snapshot from the effective config, exactly as cmd/scion-a2a-bridge does.
	srv.SetSnapshot(NewSnapshotHolder(BuildSnapshot(*cfg)))

	mw := srv.authMiddleware(testHandler())
	req := httptest.NewRequest(http.MethodPost, "/projects/p/agents/a/jsonrpc", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (through production snapshot wiring); body=%s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["user_id"] != "snapshot-user-1" || body["token_type"] != "bearer" {
		t.Errorf("caller = %+v, want user_id=snapshot-user-1 token_type=bearer", body)
	}
}
