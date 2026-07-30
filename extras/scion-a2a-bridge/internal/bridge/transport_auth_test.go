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
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
)

// newMockUATHub returns a Hub stub that accepts "Bearer scion_pat_valid".
func newMockUATHub(t *testing.T) *httptest.Server {
	t.Helper()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/me" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") == "Bearer scion_pat_valid" {
			_ = json.NewEncoder(w).Encode(userResponse{
				ID:    "uat-user-1",
				Email: "alice@example.com",
				Role:  "user",
			})
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(hub.Close)
	return hub
}

// newTransportAuthServer builds a Server with the given auth/bridge config.
func newTransportAuthServer(t *testing.T, hubURL string, auth AuthConfig, br BridgeConfig) *Server {
	t.Helper()
	dir := t.TempDir()
	store, err := state.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	br.ExternalURL = "https://test"
	cfg := &Config{
		Bridge: br,
		Hub:    HubConfig{Endpoint: hubURL, User: "admin@test"},
		Auth:   auth,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := New(store, nil, nil, cfg, nil, log)
	return NewServer(b, cfg, nil, log, testHandler())
}

// TestRESTAuthMiddleware_AllSchemes verifies the REST transport enforces the
// same auth schemes as JSON-RPC, including per-user hubUAT identity injection.
func TestRESTAuthMiddleware_AllSchemes(t *testing.T) {
	hub := newMockUATHub(t)

	tests := []struct {
		name       string
		auth       AuthConfig
		bridge     BridgeConfig
		header     string
		headerVal  string
		wantStatus int
		wantCaller string
	}{
		{
			name:       "apiKey/valid",
			auth:       AuthConfig{Scheme: "apiKey", APIKey: "my-secret"},
			header:     "X-API-Key",
			headerVal:  "my-secret",
			wantStatus: http.StatusOK,
		},
		{
			name:       "apiKey/invalid",
			auth:       AuthConfig{Scheme: "apiKey", APIKey: "my-secret"},
			header:     "X-API-Key",
			headerVal:  "nope",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "apiKey/missing",
			auth:       AuthConfig{Scheme: "apiKey", APIKey: "my-secret"},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "bearer/valid",
			auth:       AuthConfig{Scheme: "bearer", APIKey: "my-secret"},
			header:     "Authorization",
			headerVal:  "Bearer my-secret",
			wantStatus: http.StatusOK,
		},
		{
			name:       "hubUAT/valid",
			auth:       AuthConfig{Scheme: "hubUAT"},
			header:     "Authorization",
			headerVal:  "Bearer scion_pat_valid",
			wantStatus: http.StatusOK,
			wantCaller: "uat-user-1",
		},
		{
			name:       "hubUAT/invalid",
			auth:       AuthConfig{Scheme: "hubUAT"},
			header:     "Authorization",
			headerVal:  "Bearer scion_pat_bad",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "hubUAT/x-api-key-header",
			auth:       AuthConfig{Scheme: "hubUAT"},
			header:     "X-API-Key",
			headerVal:  "scion_pat_valid",
			wantStatus: http.StatusOK,
			wantCaller: "uat-user-1",
		},
		{
			name:       "none/bypass",
			auth:       AuthConfig{Scheme: "none"},
			wantStatus: http.StatusOK,
		},
		{
			name:       "rest_insecure/bypass",
			auth:       AuthConfig{Scheme: "apiKey", APIKey: "my-secret"},
			bridge:     BridgeConfig{RESTInsecure: true},
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTransportAuthServer(t, hub.URL, tt.auth, tt.bridge)
			h := srv.AuthHTTPMiddleware(testHandler())

			req := httptest.NewRequest(http.MethodPost, "/v1/message:send", nil)
			if tt.header != "" {
				req.Header.Set(tt.header, tt.headerVal)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", w.Code, tt.wantStatus, w.Body.String())
			}
			if tt.wantStatus != http.StatusOK {
				return
			}
			var body map[string]string
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if got := body["user_id"]; got != tt.wantCaller {
				t.Errorf("caller user_id = %q, want %q", got, tt.wantCaller)
			}
		})
	}
}

// TestRESTAuthMiddleware_HubJWT verifies hubJWT works on the REST transport and
// injects the caller identity.
func TestRESTAuthMiddleware_HubJWT(t *testing.T) {
	signingKey := testSigningKey(t)
	srv := newTransportAuthServer(t, "http://hub", AuthConfig{Scheme: "hubJWT"}, BridgeConfig{})
	srv.SetJWTValidator(NewJWTValidator(signingKey))

	h := srv.AuthHTTPMiddleware(testHandler())
	token := mintTestJWT(t, signingKey, validClaims())

	req := httptest.NewRequest(http.MethodPost, "/v1/message:send", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["user_id"] != "user-1" || body["token_type"] != "jwt" {
		t.Errorf("caller = %v, want user-1/jwt", body)
	}

	// Missing token must be rejected.
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, httptest.NewRequest(http.MethodPost, "/v1/message:send", nil))
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("missing token: status = %d, want 401", w2.Code)
	}
}

// TestRESTAuthMiddleware_MissingValidator ensures a misconfigured server fails
// closed with 500 rather than allowing the request through.
func TestRESTAuthMiddleware_MissingValidator(t *testing.T) {
	srv := newTransportAuthServer(t, "http://hub", AuthConfig{Scheme: "hubJWT"}, BridgeConfig{})
	// SetJWTValidator intentionally not called.
	h := srv.AuthHTTPMiddleware(testHandler())

	req := httptest.NewRequest(http.MethodPost, "/v1/message:send", nil)
	req.Header.Set("Authorization", "Bearer something")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// TestGRPCAuthInterceptors_AllSchemes verifies the gRPC transport enforces the
// same auth schemes and injects CallerIdentity for per-user schemes.
func TestGRPCAuthInterceptors_AllSchemes(t *testing.T) {
	hub := newMockUATHub(t)

	tests := []struct {
		name       string
		auth       AuthConfig
		bridge     BridgeConfig
		md         metadata.MD
		noMetadata bool
		wantCode   codes.Code
		wantCaller string
	}{
		{
			name:     "apiKey/valid",
			auth:     AuthConfig{Scheme: "apiKey", APIKey: "my-secret"},
			md:       metadata.Pairs("x-api-key", "my-secret"),
			wantCode: codes.OK,
		},
		{
			name:     "apiKey/invalid",
			auth:     AuthConfig{Scheme: "apiKey", APIKey: "my-secret"},
			md:       metadata.Pairs("x-api-key", "nope"),
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "bearer/valid",
			auth:     AuthConfig{Scheme: "bearer", APIKey: "my-secret"},
			md:       metadata.Pairs("authorization", "Bearer my-secret"),
			wantCode: codes.OK,
		},
		{
			name:     "bearer/invalid",
			auth:     AuthConfig{Scheme: "bearer", APIKey: "my-secret"},
			md:       metadata.Pairs("authorization", "Bearer wrong"),
			wantCode: codes.Unauthenticated,
		},
		{
			name:       "no-metadata",
			auth:       AuthConfig{Scheme: "apiKey", APIKey: "my-secret"},
			noMetadata: true,
			wantCode:   codes.Unauthenticated,
		},
		{
			name:       "hubUAT/valid",
			auth:       AuthConfig{Scheme: "hubUAT"},
			md:         metadata.Pairs("authorization", "Bearer scion_pat_valid"),
			wantCode:   codes.OK,
			wantCaller: "uat-user-1",
		},
		{
			name:     "hubUAT/invalid",
			auth:     AuthConfig{Scheme: "hubUAT"},
			md:       metadata.Pairs("authorization", "Bearer scion_pat_bad"),
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "none/bypass",
			auth:     AuthConfig{Scheme: "none"},
			wantCode: codes.OK,
		},
		{
			name:     "grpc_insecure/bypass",
			auth:     AuthConfig{Scheme: "apiKey", APIKey: "my-secret"},
			bridge:   BridgeConfig{GRPCInsecure: true},
			wantCode: codes.OK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTransportAuthServer(t, hub.URL, tt.auth, tt.bridge)

			ctx := context.Background()
			if !tt.noMetadata {
				md := tt.md
				if md == nil {
					md = metadata.MD{}
				}
				ctx = metadata.NewIncomingContext(ctx, md)
			}

			var gotCaller string
			handler := func(ctx context.Context, req interface{}) (interface{}, error) {
				if c := callerIdentityFromContext(ctx); c != nil {
					gotCaller = c.UserID
				}
				return "ok", nil
			}

			// Unary.
			_, err := srv.AuthUnaryInterceptor()(ctx, nil, &grpc.UnaryServerInfo{}, handler)
			if got := status.Code(err); got != tt.wantCode {
				t.Fatalf("unary code = %v, want %v (err: %v)", got, tt.wantCode, err)
			}
			if tt.wantCode == codes.OK && gotCaller != tt.wantCaller {
				t.Errorf("unary caller = %q, want %q", gotCaller, tt.wantCaller)
			}

			// Stream: the interceptor must also propagate the auth context.
			gotCaller = ""
			streamHandler := func(srv interface{}, ss grpc.ServerStream) error {
				if c := callerIdentityFromContext(ss.Context()); c != nil {
					gotCaller = c.UserID
				}
				return nil
			}
			err = srv.AuthStreamInterceptor()(nil, &fakeServerStream{ctx: ctx}, &grpc.StreamServerInfo{}, streamHandler)
			if got := status.Code(err); got != tt.wantCode {
				t.Fatalf("stream code = %v, want %v (err: %v)", got, tt.wantCode, err)
			}
			if tt.wantCode == codes.OK && gotCaller != tt.wantCaller {
				t.Errorf("stream caller = %q, want %q", gotCaller, tt.wantCaller)
			}
		})
	}
}

// fakeServerStream is a minimal grpc.ServerStream carrying a context.
type fakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f *fakeServerStream) Context() context.Context { return f.ctx }
