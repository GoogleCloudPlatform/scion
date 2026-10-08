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
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// profilingTestUsers returns a hub admin and a hub member of s.
func profilingTestUsers(t *testing.T, s store.Store) (admin, member *store.User) {
	t.Helper()
	ctx := context.Background()
	a, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "prof-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)
	m, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "prof-member", store.SystemRoleHubMember))
	require.NoError(t, err)
	return a, m
}

func decodeProfilingAdmin(t *testing.T, rec *httptest.ResponseRecorder) profilingResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body profilingResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}

// TestAdminProfiling_AdminRoundTrip: an admin session reads and writes the
// setting; an explicit null resets it to off.
func TestAdminProfiling_AdminRoundTrip(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	admin, _ := profilingTestUsers(t, s)

	got := decodeProfilingAdmin(t, doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/admin/profiling", nil))
	require.False(t, got.ReadinessMarks, "default is off")

	got = decodeProfilingAdmin(t, doRequestAsUser(t, srv, admin, http.MethodPut, "/api/v1/admin/profiling", map[string]any{"readiness_marks": true}))
	require.True(t, got.ReadinessMarks)
	require.True(t, srv.ReadinessMarksEnabled())
	got = decodeProfilingAdmin(t, doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/admin/profiling", nil))
	require.True(t, got.ReadinessMarks)

	// An empty body leaves the value unchanged.
	got = decodeProfilingAdmin(t, doRequestAsUser(t, srv, admin, http.MethodPut, "/api/v1/admin/profiling", map[string]any{}))
	require.True(t, got.ReadinessMarks)

	got = decodeProfilingAdmin(t, doRequestAsUser(t, srv, admin, http.MethodPut, "/api/v1/admin/profiling", map[string]any{"readiness_marks": nil}))
	require.False(t, got.ReadinessMarks, "explicit null resets to off")
	require.False(t, srv.ReadinessMarksEnabled())
}

// TestAdminProfiling_MemberRefused: a member session can neither read nor
// write the admin route, and the setting stays as it was.
func TestAdminProfiling_MemberRefused(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	_, member := profilingTestUsers(t, s)

	rec := doRequestAsUser(t, srv, member, http.MethodPut, "/api/v1/admin/profiling", map[string]any{"readiness_marks": true})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/admin/profiling", nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.False(t, srv.ReadinessMarksEnabled())
}

// TestAdminProfiling_TokenRefused: a user access token is refused on the
// admin route, even a hub admin's token carrying the hub configuration
// selectors, with the session-only reason; the setting stays as it was.
func TestAdminProfiling_TokenRefused(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	admin, _ := profilingTestUsers(t, s)
	key := mintHubConfigToken(t, srv, admin.ID, hubBoundary(), "hub_config:read", "hub_config:update")

	for _, method := range []string{http.MethodPut, http.MethodGet} {
		var body any
		if method == http.MethodPut {
			body = map[string]any{"readiness_marks": true}
		}
		rec := doRequestWithToken(t, srv, key, method, "/api/v1/admin/profiling", body)
		require.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", method, rec.Body.String())
		_, details := errorCodeAndDetails(t, rec)
		require.Equal(t, "HOST_OPERATIONS", details["reason"], "%s: %s", method, rec.Body.String())
	}
	require.False(t, srv.ReadinessMarksEnabled())
}

// TestProfilingClient_Response: GET /api/v1/profiling needs a signed-in
// caller and returns exactly one key, readinessMarks.
func TestProfilingClient_Response(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	admin, member := profilingTestUsers(t, s)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/profiling", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())

	for _, on := range []bool{false, true} {
		doRequestAsUser(t, srv, admin, http.MethodPut, "/api/v1/admin/profiling", map[string]any{"readiness_marks": on})
		rec := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/profiling", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var raw map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
		keys := make([]string, 0, len(raw))
		for k := range raw {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		require.Equal(t, []string{"readinessMarks"}, keys)
		want := "false"
		if on {
			want = "true"
		}
		require.Equal(t, want, string(raw["readinessMarks"]))
	}
}

// renderShell renders the SPA shell for path, as user (nil = signed out),
// with the hub server as the profiling source (nil = none, the shell as
// it was before the setting existed).
func renderShell(t *testing.T, provider ProfilingSettingsProvider, user *store.User, path string) string {
	t.Helper()
	ws := newTestWebServer(t, WebServerConfig{Host: "127.0.0.1"})
	if provider != nil {
		ws.SetProfilingSettingsProvider(provider)
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if user != nil {
		req = req.WithContext(setWebSessionUser(req.Context(), &webSessionUser{
			UserID: user.ID, Email: user.Email, Name: user.DisplayName, Role: user.Role,
		}))
	}
	rec := httptest.NewRecorder()
	ws.spaHandler()(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body, err := io.ReadAll(rec.Result().Body)
	require.NoError(t, err)
	return string(body)
}

// shellInitialData returns the raw fields of the shell's initial data.
func shellInitialData(t *testing.T, html string) map[string]json.RawMessage {
	t.Helper()
	const open = `<script id="__SCION_DATA__" type="application/json">`
	start := strings.Index(html, open)
	require.GreaterOrEqual(t, start, 0, "no initial data in the shell")
	start += len(open)
	end := strings.Index(html[start:], "</script>")
	require.Greater(t, end, 0)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(html[start:start+end]), &fields))
	return fields
}

// TestShellReadinessMarks_Hydration: the shell's initial data carries
// readinessMarks only for a signed-in user while the setting is on, with
// the value GET /api/v1/profiling returns to that user. Off, or signed
// out, the shell is byte-identical to one rendered with no profiling
// source.
func TestShellReadinessMarks_Hydration(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	admin, member := profilingTestUsers(t, s)

	baselineMember := renderShell(t, nil, member, "/projects")
	baselineLogin := renderShell(t, nil, nil, "/login")

	// Off: unchanged bytes, signed in or out.
	require.Equal(t, baselineMember, renderShell(t, srv, member, "/projects"))
	require.Equal(t, baselineLogin, renderShell(t, srv, nil, "/login"))
	_, present := shellInitialData(t, baselineMember)["readinessMarks"]
	require.False(t, present)

	doRequestAsUser(t, srv, admin, http.MethodPut, "/api/v1/admin/profiling", map[string]any{"readiness_marks": true})
	require.True(t, srv.ReadinessMarksEnabled())

	// On, signed out: still unchanged bytes.
	require.Equal(t, baselineLogin, renderShell(t, srv, nil, "/login"))

	// On, signed in: the field equals what the same member reads from the API.
	fields := shellInitialData(t, renderShell(t, srv, member, "/projects"))
	hydrated, present := fields["readinessMarks"]
	require.True(t, present, "member shell with the setting on carries readinessMarks")
	rec := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/profiling", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var api map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &api))
	require.Equal(t, string(api["readinessMarks"]), string(hydrated))
	require.Equal(t, "true", string(hydrated))
}
