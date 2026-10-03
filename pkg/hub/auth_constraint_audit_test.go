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
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

const constraintAuditAuthTestPath = "/api/v1/admin/access-constraints/constraint-2405/audit"

func canonicalConstraintAuditNotFound(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	NotFound(rec, "Access Constraint")
	return rec
}

func TestUnifiedConstraintAuditExpiredCredentialIsIndistinguishable(t *testing.T) {
	tokenService, err := NewUserTokenService(UserTokenConfig{AccessTokenDuration: -time.Minute})
	if err != nil {
		t.Fatalf("NewUserTokenService() error = %v", err)
	}
	expiredToken, _, _, err := tokenService.GenerateTokenPair(
		"expired-user", "expired@example.com", "Expired User", "admin", ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("GenerateTokenPair() error = %v", err)
	}

	called := false
	handler := UnifiedAuthMiddleware(AuthConfig{UserTokenSvc: tokenService})(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) { called = true },
	))
	req := httptest.NewRequest(http.MethodGet, constraintAuditAuthTestPath, nil)
	req.Header.Set("Authorization", "Bearer "+expiredToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assertConstraintAuditNotFound(t, rec)
	if called {
		t.Fatal("expired authentication invoked the next handler")
	}
	if strings.Contains(rec.Body.String(), "expired") || strings.Contains(rec.Body.String(), "token") {
		t.Errorf("normalized response leaked credential rejection detail: %s", rec.Body.String())
	}
}

func assertConstraintAuditNotFound(t *testing.T, got *httptest.ResponseRecorder) {
	t.Helper()
	want := canonicalConstraintAuditNotFound(t)
	if got.Code != want.Code {
		t.Fatalf("status = %d, want %d; body: %s", got.Code, want.Code, got.Body.String())
	}
	if !reflect.DeepEqual(got.Header(), want.Header()) {
		t.Errorf("headers = %#v, want %#v", got.Header(), want.Header())
	}
	if got.Body.String() != want.Body.String() {
		t.Errorf("body = %q, want %q", got.Body.String(), want.Body.String())
	}
}

func TestConstraintAuditAuthFailureRoute(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		target string
		want   bool
	}{
		{name: "canonical", method: http.MethodGet, target: constraintAuditAuthTestPath, want: true},
		{name: "query ignored", method: http.MethodGet, target: constraintAuditAuthTestPath + "?pageSize=10", want: true},
		{name: "post", method: http.MethodPost, target: constraintAuditAuthTestPath},
		{name: "lowercase method", method: "get", target: constraintAuditAuthTestPath},
		{name: "missing id", method: http.MethodGet, target: "/api/v1/admin/access-constraints//audit"},
		{name: "extra segment", method: http.MethodGet, target: constraintAuditAuthTestPath + "/events"},
		{name: "suffix", method: http.MethodGet, target: constraintAuditAuthTestPath + "-export"},
		{name: "prefix", method: http.MethodGet, target: "/prefix" + constraintAuditAuthTestPath},
		{name: "case confusion", method: http.MethodGet, target: "/api/v1/admin/access-constraints/constraint-2405/Audit"},
		{name: "trailing slash", method: http.MethodGet, target: constraintAuditAuthTestPath + "/"},
		{name: "dot segment", method: http.MethodGet, target: "/api/v1/admin/access-constraints/./audit"},
		{name: "dot dot segment", method: http.MethodGet, target: "/api/v1/admin/access-constraints/../audit"},
		{name: "encoded slash in id", method: http.MethodGet, target: "/api/v1/admin/access-constraints/constraint%2F2405/audit"},
		{name: "double encoded slash in id", method: http.MethodGet, target: "/api/v1/admin/access-constraints/constraint%252F2405/audit"},
		{name: "encoded id character", method: http.MethodGet, target: "/api/v1/admin/access-constraints/constraint-%32%34%30%35/audit"},
		{name: "similar admin route", method: http.MethodGet, target: "/api/v1/admin/access-constraints/constraint-2405"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.target, nil)
			if got := isConstraintAuditAuthFailureRoute(req); got != tc.want {
				t.Errorf("isConstraintAuditAuthFailureRoute(%s %s) = %v, want %v (Path=%q RawPath=%q)",
					tc.method, tc.target, got, tc.want, req.URL.Path, req.URL.RawPath)
			}
		})
	}
}

func TestConstraintAuditAuthFailuresAreIndistinguishable(t *testing.T) {
	const devToken = "scion_dev_test_token_12345678901234567890123456789012"
	devCfg := DevUserConfig{Username: "audit-dev", DisplayName: "Audit Dev", Email: "audit-dev@localhost"}

	for _, middleware := range []struct {
		name string
		wrap func(http.Handler) http.Handler
	}{
		{
			name: "unified",
			wrap: UnifiedAuthMiddleware(AuthConfig{
				DevAuthEnabled: true,
				DevAuthToken:   devToken,
				DevUserCfg:     devCfg,
			}),
		},
		{
			name: "development",
			wrap: DevAuthMiddleware(devToken, devCfg),
		},
	} {
		t.Run(middleware.name, func(t *testing.T) {
			for _, authHeader := range []string{
				"",
				"Basic not-a-bearer-token",
				"Bearer definitely-invalid",
			} {
				name := authHeader
				if name == "" {
					name = "missing"
				}
				t.Run(name, func(t *testing.T) {
					called := false
					handler := middleware.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
						called = true
					}))
					req := httptest.NewRequest(http.MethodGet, constraintAuditAuthTestPath, nil)
					if authHeader != "" {
						req.Header.Set("Authorization", authHeader)
					}
					rec := httptest.NewRecorder()
					handler.ServeHTTP(rec, req)

					assertConstraintAuditNotFound(t, rec)
					if called {
						t.Fatal("failed authentication invoked the next handler")
					}
					if strings.Contains(rec.Body.String(), "token") || strings.Contains(rec.Body.String(), "constraint-2405") {
						t.Errorf("normalized response leaked authentication or resource detail: %s", rec.Body.String())
					}
				})
			}
		})
	}
}

func TestConstraintAuditValidCredentialsRetainIdentity(t *testing.T) {
	const devToken = "scion_dev_test_token_12345678901234567890123456789012"
	devCfg := DevUserConfig{Username: "audit-dev", DisplayName: "Audit Dev", Email: "audit-dev@localhost"}

	for _, middleware := range []struct {
		name string
		wrap func(http.Handler) http.Handler
	}{
		{
			name: "unified",
			wrap: UnifiedAuthMiddleware(AuthConfig{
				DevAuthEnabled: true,
				DevAuthToken:   devToken,
				DevUserCfg:     devCfg,
			}),
		},
		{name: "development", wrap: DevAuthMiddleware(devToken, devCfg)},
	} {
		t.Run(middleware.name, func(t *testing.T) {
			called := false
			handler := middleware.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				user := GetUserFromContext(r.Context())
				if user == nil || user.ID() != DevUserID || user.Email() != devCfg.Email {
					t.Errorf("dev identity = %#v, want ID %q and email %q", user, DevUserID, devCfg.Email)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			req := httptest.NewRequest(http.MethodGet, constraintAuditAuthTestPath, nil)
			req.Header.Set("Authorization", "Bearer "+devToken)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if !called {
				t.Fatal("valid authentication did not invoke the next handler")
			}
			if rec.Code != http.StatusNoContent {
				t.Errorf("status = %d, want %d; body: %s", rec.Code, http.StatusNoContent, rec.Body.String())
			}

			// Once authentication succeeds, downstream responses pass through
			// unchanged rather than being mistaken for authentication failures.
			downstreamUnauthorized := middleware.wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "downstream rejection", nil)
			}))
			rec = httptest.NewRecorder()
			downstreamUnauthorized.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "downstream rejection") {
				t.Errorf("downstream response = %d %s, want unchanged 401", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestConstraintAuditAuthNormalizationDoesNotMatchNeighborRoutes(t *testing.T) {
	const devToken = "scion_dev_test_token_12345678901234567890123456789012"
	for _, middleware := range []struct {
		name string
		wrap func(http.Handler) http.Handler
	}{
		{
			name: "unified",
			wrap: UnifiedAuthMiddleware(AuthConfig{
				DevAuthEnabled: true,
				DevAuthToken:   devToken,
			}),
		},
		{name: "development", wrap: DevAuthMiddleware(devToken, DevUserConfig{})},
	} {
		t.Run(middleware.name, func(t *testing.T) {
			for _, tc := range []struct {
				name   string
				method string
				target string
			}{
				{name: "non-get", method: http.MethodPost, target: constraintAuditAuthTestPath},
				{name: "constraint detail", method: http.MethodGet, target: "/api/v1/admin/access-constraints/constraint-2405"},
				{name: "extra segment", method: http.MethodGet, target: constraintAuditAuthTestPath + "/events"},
				{name: "suffix", method: http.MethodGet, target: constraintAuditAuthTestPath + "-export"},
				{name: "encoded slash", method: http.MethodGet, target: "/api/v1/admin/access-constraints/constraint%2F2405/audit"},
				{name: "double encoded slash", method: http.MethodGet, target: "/api/v1/admin/access-constraints/constraint%252F2405/audit"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					called := false
					handler := middleware.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
						called = true
					}))
					rec := httptest.NewRecorder()
					handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, nil))
					if rec.Code != http.StatusUnauthorized {
						t.Errorf("status = %d, want 401; body: %s", rec.Code, rec.Body.String())
					}
					if called {
						t.Fatal("failed authentication invoked the next handler")
					}
				})
			}
		})
	}
}
