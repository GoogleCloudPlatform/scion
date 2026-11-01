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
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/imagecheck"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clearTrustedProxiesEnvForHub unsets every SCION_SERVER_* name of
// server.trusted_proxies for the test, so an inherited value cannot mark
// the setting set.
func clearTrustedProxiesEnvForHub(t *testing.T) {
	t.Helper()
	for _, e := range config.ServerEnvEntries() {
		if e.Path != config.TrustedProxiesKey {
			continue
		}
		for _, name := range append([]string{e.Canonical}, e.Aliases...) {
			t.Setenv(name, "") // restored after the test
			require.NoError(t, os.Unsetenv(name))
		}
	}
}

// requireTrustedProxiesState loads the server config as the hub does at
// startup and checks server.trusted_proxies' state and the transport
// policy's networks: an explicit empty list is set and trusts no proxy, a
// list is set and trusts exactly its entries, and no setting trusts only a
// proxy on this host.
func requireTrustedProxiesState(t *testing.T, when string, wantSet bool, wantList []string) {
	t.Helper()
	gc, err := config.LoadGlobalConfig("")
	require.NoError(t, err, when)
	assert.Equal(t, wantSet, gc.TrustedProxiesSet, "%s: set", when)
	assert.Equal(t, strings.Join(wantList, "|"), strings.Join(gc.TrustedProxies, "|"), "%s: list", when)
	nets, usedDefault, err := config.TransportTrustedProxies(gc.TrustedProxies, gc.TrustedProxiesSet)
	require.NoError(t, err, when)
	got := make([]string, 0, len(nets))
	for _, n := range nets {
		got = append(got, n.String())
	}
	switch {
	case !wantSet:
		assert.True(t, usedDefault, "%s: not set uses the loopback default", when)
		assert.Equal(t, []string{"127.0.0.1/32", "::1/128"}, got, "%s: not set trusts only a proxy on this host", when)
	case len(wantList) == 0:
		assert.False(t, usedDefault, "%s: an explicit empty list is not the default", when)
		assert.Empty(t, got, "%s: an explicit empty list trusts no proxy", when)
	default:
		assert.False(t, usedDefault, "%s: a list is not the default", when)
		assert.Len(t, got, len(wantList), "%s: a list trusts exactly its entries", when)
	}
}

// server.trusted_proxies keeps its state through every hub handler that
// rewrites settings.yaml through the settings struct (file mode, no
// operational settings): the system runtime PUT and the image registry
// PUT. An explicit [] stays set and trusts no proxy, distinct from no
// setting (the loopback default) and from a list. The legacy migration
// path is covered in pkg/config (TestTrustedProxies_StateSurvivesLegacyMigration).
func TestTrustedProxies_StateSurvivesHubSettingsRewrites(t *testing.T) {
	rewrites := map[string]struct {
		put      func(t *testing.T, srv *Server) *httptest.ResponseRecorder
		wantFile string // what the rewritten file holds for the PUT's own change
	}{
		"system runtime PUT": {
			put: func(t *testing.T, srv *Server) *httptest.ResponseRecorder {
				rr := httptest.NewRecorder()
				req := adminRequest(http.MethodPut, "/api/v1/system/runtime", `{"runtime":"podman"}`)
				req.RemoteAddr = "127.0.0.1:1234"
				srv.handleSystemRuntime(rr, req)
				return rr
			},
			wantFile: "runtime: podman",
		},
		"image registry PUT": {
			put: func(t *testing.T, srv *Server) *httptest.ResponseRecorder {
				return putSystemRegistry(t, srv, "rewrite.example")
			},
			wantFile: "image_registry: rewrite.example",
		},
	}
	states := map[string]struct {
		lines      string // the server block's trusted_proxies lines
		wantSet    bool
		wantList   []string
		wantInFile string // "" when the key must stay absent
	}{
		"explicit empty": {lines: "  trusted_proxies: []\n", wantSet: true, wantInFile: "trusted_proxies: []"},
		"list":           {lines: "  trusted_proxies:\n    - 10.0.0.0/8\n", wantSet: true, wantList: []string{"10.0.0.0/8"}, wantInFile: "- 10.0.0.0/8"},
		"not set":        {lines: "", wantSet: false},
	}
	for rname, rw := range rewrites {
		for sname, st := range states {
			t.Run(rname+"/"+sname, func(t *testing.T) {
				settingsPath := tempSettingsHome(t)
				clearTrustedProxiesEnvForHub(t)
				t.Setenv("SCION_ACTIVE_PROFILE", "")
				content := "schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n" +
					"server:\n  log_level: info\n" + st.lines
				require.NoError(t, os.WriteFile(settingsPath, []byte(content), 0o644))
				requireTrustedProxiesState(t, "before the PUT", st.wantSet, st.wantList)

				srv := &Server{maintenance: NewMaintenanceState(false, ""), imageChecker: imagecheck.NewChecker()}
				require.Nil(t, srv.GetOperationalSettings(), "file mode: the PUT rewrites settings.yaml")
				rr := rw.put(t, srv)
				require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

				data, err := os.ReadFile(settingsPath)
				require.NoError(t, err)
				require.Contains(t, string(data), rw.wantFile, "the PUT rewrote settings.yaml")
				if st.wantInFile == "" {
					assert.NotContains(t, string(data), "trusted_proxies", "an absent key stays absent")
				} else {
					assert.Contains(t, string(data), st.wantInFile, "the rewritten file keeps the key")
				}
				requireTrustedProxiesState(t, "after the PUT", st.wantSet, st.wantList)
			})
		}
	}
}
