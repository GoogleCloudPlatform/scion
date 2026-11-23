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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnifiedAuthMiddleware_TrustedProxiesNeverAuthenticatesHeaders verifies
// that configuring ServerConfig.TrustedProxies never causes
// UnifiedAuthMiddleware to authenticate requests from X-Forwarded-User-*
// headers when no ProxyAuthenticator is configured.
func TestUnifiedAuthMiddleware_TrustedProxiesNeverAuthenticatesHeaders(t *testing.T) {
	s, err := newTestStore(t, ":memory:")
	require.NoError(t, err)

	cfg := DefaultServerConfig()
	cfg.TrustedProxies = []string{"192.0.2.0/24"}
	cfg.ProxyAuth = nil
	srv, err := newTestHubServer(t, cfg, s)
	require.NoError(t, err)

	reached := false
	handler := UnifiedAuthMiddleware(srv.authConfig)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("X-Forwarded-User-Id", "user-1")
	req.Header.Set("X-Forwarded-User-Email", "admin@example.com")
	req.Header.Set("X-Forwarded-User-Name", "Admin")
	req.Header.Set("X-Forwarded-User-Role", "admin")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, reached, "downstream handler must not be reached")
}
