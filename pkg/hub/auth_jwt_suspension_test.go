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

package hub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// F-REV-04: Direct regression test for the JWT suspension store-failure path.
//
// The JWT auth middleware checks user suspension status by calling
// UserStore.GetUser. Prior to the fix (F-REV-03, commit 5dff865), a store
// error caused the middleware to silently continue, admitting suspended users.
// The fix returns 503 on non-ErrNotFound store errors. This test proves that
// behavior.

// stubUserStore implements the subset of store.UserStore needed by the JWT
// auth middleware. It returns a configurable error from GetUser.
type stubUserStore struct {
	store.UserStore
	getUser func(ctx context.Context, id string) (*store.User, error)
}

func (s *stubUserStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if s.getUser != nil {
		return s.getUser(ctx, id)
	}
	return nil, store.ErrNotFound
}

func TestJWTAuth_StoreError_Returns503(t *testing.T) {
	userTokenSvc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("failed to create user token service: %v", err)
	}

	accessToken, _, _, err := userTokenSvc.GenerateTokenPair(
		"user-store-err", "store-err@example.com", "Store Err User", "member", ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("failed to generate tokens: %v", err)
	}

	storeErr := errors.New("database connection lost")

	cfg := AuthConfig{
		Mode:         "production",
		UserTokenSvc: userTokenSvc,
		UserStore: &stubUserStore{
			getUser: func(_ context.Context, _ string) (*store.User, error) {
				return nil, storeErr
			},
		},
		Debug: false,
	}

	middleware := UnifiedAuthMiddleware(cfg)
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not be reached on store error")
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 on store error, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestJWTAuth_SuspendedUser_Returns403(t *testing.T) {
	userTokenSvc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("failed to create user token service: %v", err)
	}

	accessToken, _, _, err := userTokenSvc.GenerateTokenPair(
		"user-suspended", "suspended@example.com", "Suspended User", "member", ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("failed to generate tokens: %v", err)
	}

	cfg := AuthConfig{
		Mode:         "production",
		UserTokenSvc: userTokenSvc,
		UserStore: &stubUserStore{
			getUser: func(_ context.Context, _ string) (*store.User, error) {
				return &store.User{
					ID:     "user-suspended",
					Email:  "suspended@example.com",
					Status: store.UserStatusSuspended,
				}, nil
			},
		},
		Debug: false,
	}

	middleware := UnifiedAuthMiddleware(cfg)
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not be reached for suspended user")
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for suspended user, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestJWTAuth_ActiveUser_PassesThrough(t *testing.T) {
	userTokenSvc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("failed to create user token service: %v", err)
	}

	accessToken, _, _, err := userTokenSvc.GenerateTokenPair(
		"user-active", "active@example.com", "Active User", "member", ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("failed to generate tokens: %v", err)
	}

	cfg := AuthConfig{
		Mode:         "production",
		UserTokenSvc: userTokenSvc,
		UserStore: &stubUserStore{
			getUser: func(_ context.Context, _ string) (*store.User, error) {
				return &store.User{
					ID:     "user-active",
					Email:  "active@example.com",
					Status: store.UserStatusActive,
				}, nil
			},
		},
		Debug: false,
	}

	middleware := UnifiedAuthMiddleware(cfg)
	var gotIdentity Identity
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIdentity = GetIdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for active user, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotIdentity == nil {
		t.Fatal("expected identity in context")
	}
	if gotIdentity.ID() != "user-active" {
		t.Errorf("expected user ID 'user-active', got %q", gotIdentity.ID())
	}
}

func TestJWTAuth_DeletedUser_PassesThroughToHandler(t *testing.T) {
	userTokenSvc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("failed to create user token service: %v", err)
	}

	accessToken, _, _, err := userTokenSvc.GenerateTokenPair(
		"user-deleted", "deleted@example.com", "Deleted User", "member", ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("failed to generate tokens: %v", err)
	}

	cfg := AuthConfig{
		Mode:         "production",
		UserTokenSvc: userTokenSvc,
		UserStore: &stubUserStore{
			getUser: func(_ context.Context, _ string) (*store.User, error) {
				return nil, store.ErrNotFound
			},
		},
		Debug: false,
	}

	middleware := UnifiedAuthMiddleware(cfg)
	var handlerReached bool
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerReached = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// ErrNotFound (deleted user) falls through — downstream handlers will
	// fail closed when the identity has no matching store record.
	if !handlerReached {
		t.Error("expected handler to be reached for deleted user (ErrNotFound)")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for deleted user pass-through, got %d", rec.Code)
	}
}

func TestJWTAuth_NoUserStore_PassesThrough(t *testing.T) {
	userTokenSvc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("failed to create user token service: %v", err)
	}

	accessToken, _, _, err := userTokenSvc.GenerateTokenPair(
		"user-nostore", "nostore@example.com", "No Store User", "member", ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("failed to generate tokens: %v", err)
	}

	cfg := AuthConfig{
		Mode:         "production",
		UserTokenSvc: userTokenSvc,
		UserStore:    nil, // no user store configured
		Debug:        false,
	}

	middleware := UnifiedAuthMiddleware(cfg)
	var handlerReached bool
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerReached = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !handlerReached {
		t.Error("expected handler to be reached when no UserStore is configured")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 when no UserStore, got %d", rec.Code)
	}
}
