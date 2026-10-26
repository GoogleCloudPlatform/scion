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
	"strings"
	"testing"
)

// TestServerConfigGET_EnvValuesRequireHubConfigRead: the server-config GET,
// and so env_values, is served only to a caller with hub.config.read (the
// route guard; an admin route). A plain member, a caller with an unrelated
// hub permission, and an unauthenticated caller are refused and never see
// env_values; a super-admin and a hub.config.read holder get it.
func TestServerConfigGET_EnvValuesRequireHubConfigRead(t *testing.T) {
	const path = "/api/v1/admin/server-config"
	const key = "server.auth.authorized_domains"
	srv, s := testServerWithOps(t, nil)
	ops := srv.GetOperationalSettings()
	if ops == nil {
		t.Fatal("test server has no operational settings")
	}
	// This node's environment sets one key.
	ops.envKoanf = newEnvKoanf(t, map[string]any{key: []any{"env.example"}})
	ops.envOverrides = map[string]bool{key: true}

	reader := createTestUser(t, s, "envval-reader", "envval-reader@test.com")
	grantSystemPermissions(t, s, reader.ID, "test-envval-reader", []string{"hub.config.read"})
	other := createTestUser(t, s, "envval-other", "envval-other@test.com")
	grantSystemPermissions(t, s, other.ID, "test-envval-other", []string{"hub.experiments.update"})
	member := createTestUser(t, s, "envval-member", "envval-member@test.com")

	t.Run("super-admin gets env_values", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"env_values"`) || !strings.Contains(rec.Body.String(), "env.example") {
			t.Fatalf("super-admin body lacks env_values: %s", rec.Body.String())
		}
	})
	t.Run("hub.config.read holder gets env_values", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, reader, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"env_values"`) {
			t.Fatalf("reader body lacks env_values: %s", rec.Body.String())
		}
	})
	for _, tc := range []struct {
		name string
		do   func() (int, string)
		want int
	}{
		{"plain member refused", func() (int, string) {
			rec := doRequestAsUser(t, srv, member, http.MethodGet, path, nil)
			return rec.Code, rec.Body.String()
		}, http.StatusForbidden},
		{"unrelated hub permission refused", func() (int, string) {
			rec := doRequestAsUser(t, srv, other, http.MethodGet, path, nil)
			return rec.Code, rec.Body.String()
		}, http.StatusForbidden},
		{"unauthenticated refused", func() (int, string) {
			rec := doRequestNoAuth(t, srv, http.MethodGet, path, nil)
			return rec.Code, rec.Body.String()
		}, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := tc.do()
			if code != tc.want {
				t.Fatalf("expected %d, got %d: %s", tc.want, code, body)
			}
			if strings.Contains(body, "env_values") || strings.Contains(body, "env.example") {
				t.Fatalf("refused response carries env_values: %s", body)
			}
		})
	}
}
