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

// Routed tests for GET /api/v1/test-infra/status (ptone/scion#4240, phase
// W): through the full handler chain, without authentication, the way the
// login page calls it.

func TestTestInfraStatusRouted_UnauthenticatedFlagOff(t *testing.T) {
	srv, _ := newTestIdentityServer(t, false)
	rec := doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/test-infra/status", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"testIdentities":false,"testHubAdmin":false,"testSuperAdmin":false}`, rec.Body.String())
}

func TestTestInfraStatusRouted_UnauthenticatedMemberOn(t *testing.T) {
	srv, _ := newTestIdentityServer(t, true)
	rec := doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/test-infra/status", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"testIdentities":true,"testHubAdmin":false,"testSuperAdmin":false}`, rec.Body.String())
}

func TestTestInfraStatusRouted_SameForSignedInAndInvalidToken(t *testing.T) {
	srv, _ := newTestIdentityServer(t, true)
	want := `{"testIdentities":true,"testHubAdmin":false,"testSuperAdmin":false}`

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/test-infra/status", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, want, rec.Body.String(), "signed-in caller")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test-infra/status", nil)
	req.Header.Set("Authorization", "Bearer not-a-valid-token")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, want, rec.Body.String(), "invalid-token caller")
}

func TestTestInfraStatusRouted_ReadOnly(t *testing.T) {
	srv, _ := newTestIdentityServer(t, true)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := doRequestNoAuth(t, srv, method, "/api/v1/test-infra/status", nil)
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, "%s: %s", method, rec.Body.String())
	}
}
