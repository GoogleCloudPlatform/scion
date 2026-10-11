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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Unit tests for GET /api/v1/test-infra/status (ptone/scion#4240, phase W).
// No store is needed: the handler only reads the server's gate values, so
// these run in the no_sqlite build too. The routed, unauthenticated tests
// are in handlers_test_infra_status_routed_test.go.

func serveTestInfraStatus(t *testing.T, srv *Server) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handleTestInfraStatus(rec, httptest.NewRequest(http.MethodGet, "/api/v1/test-infra/status", nil))
	return rec
}

func TestTestInfraStatus_AllFalseOnNonTestHub(t *testing.T) {
	rec := serveTestInfraStatus(t, &Server{})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"testIdentities":false,"testHubAdmin":false,"testSuperAdmin":false}`, rec.Body.String())
}

func TestTestInfraStatus_MemberTierOn(t *testing.T) {
	srv := &Server{testIdentities: newTestIdentityState(true)}
	rec := serveTestInfraStatus(t, srv)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"testIdentities":true,"testHubAdmin":false,"testSuperAdmin":false}`, rec.Body.String())
}

// TestTestInfraStatus_BodyGolden pins the body to exactly three booleans:
// no hub id, version, counts or any other field.
func TestTestInfraStatus_BodyGolden(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		srv := &Server{testIdentities: newTestIdentityState(enabled)}
		rec := serveTestInfraStatus(t, srv)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json"))

		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		keys := make([]string, 0, len(body))
		for k, v := range body {
			keys = append(keys, k)
			_, isBool := v.(bool)
			assert.True(t, isBool, "%s must be a bool, got %T", k, v)
		}
		sort.Strings(keys)
		assert.Equal(t, []string{"testHubAdmin", "testIdentities", "testSuperAdmin"}, keys)

		want := `{"testIdentities":false,"testHubAdmin":false,"testSuperAdmin":false}`
		if enabled {
			want = `{"testIdentities":true,"testHubAdmin":false,"testSuperAdmin":false}`
		}
		assert.Equal(t, want, strings.TrimSpace(rec.Body.String()), "exact body")
	}

	// The response type itself has exactly the three bool fields, so a new
	// field cannot be added without changing this test.
	typ := reflect.TypeOf(TestInfraStatusResponse{})
	require.Equal(t, 3, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		assert.Equal(t, reflect.Bool, typ.Field(i).Type.Kind(), typ.Field(i).Name)
	}
}

// TestTestInfraStatus_AdminGatesFalseUntilAdminTier pins the seam: the two
// admin-tier values are false whatever the member flag, until the admin
// tier fills them in from its own startup gate.
func TestTestInfraStatus_AdminGatesFalseUntilAdminTier(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		g := (&Server{testIdentities: newTestIdentityState(enabled)}).testInfraGates()
		assert.Equal(t, enabled, g.TestIdentities)
		assert.False(t, g.TestHubAdmin)
		assert.False(t, g.TestSuperAdmin)
	}
}

// TestTestInfraStatus_RouteIsPublic checks how the route is declared: public
// in the route metadata and skipped by the auth middleware.
func TestTestInfraStatus_RouteIsPublic(t *testing.T) {
	meta, ok := routeMetadataTable["GET /api/v1/test-infra/status"]
	require.True(t, ok, "route must be in routeMetadataTable")
	assert.Equal(t, RoutePublic, meta.Classification)
	assert.Empty(t, meta.Permission)
	assert.True(t, isUnauthenticatedEndpoint("/api/v1/test-infra/status"))
}
