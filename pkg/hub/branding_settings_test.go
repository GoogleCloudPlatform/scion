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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertJSONHasNoBooleans(t *testing.T, v interface{}, path string) {
	t.Helper()
	switch val := v.(type) {
	case bool:
		t.Errorf("unexpected boolean value %v at %s", val, path)
	case map[string]interface{}:
		for k, child := range val {
			assertJSONHasNoBooleans(t, child, path+"."+k)
		}
	case []interface{}:
		for _, child := range val {
			assertJSONHasNoBooleans(t, child, path+"[]")
		}
	}
}

func TestBrandingSettings_PutLiveReloadPublicAndMaintenance(t *testing.T) {
	tempSettingsHome(t)
	srv, st := testServerWithOps(t, nil)
	ops := srv.GetOperationalSettings()
	ops.SetEventPublisher(NewChannelEventPublisher())
	propCtx, stopProp := context.WithCancel(context.Background())
	ops.StartPropagation(propCtx, srv)
	t.Cleanup(func() {
		stopProp()
		ops.StopPropagation()
	})

	// 1. Default unbranded GET /api/v1/settings/public omits "branding".
	rec := doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/settings/public", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var rawUnbranded map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rawUnbranded))
	_, hasBranding := rawUnbranded["branding"]
	assert.False(t, hasBranding, "default public settings must omit branding")

	// 2. Failing WCAG AA contrast returns HTTP 422 naming mode, token pair, and ratios.
	badBody := json.RawMessage(`{
		"server": {
			"hub": {
				"branding": {
					"name": "Acme Cloud",
					"palette": {
						"light": {
							"primary": "#eab308"
						}
					}
				}
			}
		}
	}`)
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/admin/server-config", badBody)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "server.hub.branding.palette.light.primary")
	assert.NotContains(t, rec.Body.String(), "server.hub.branding.server.hub.branding")
	assert.Contains(t, rec.Body.String(), "light.bg")
	assert.Contains(t, rec.Body.String(), "4.50:1")

	// Dark-mode WCAG AA contrast violation also returns HTTP 422 naming dark mode and token pair,
	// and same-origin /auth/logout asset path is rejected with HTTP 422.
	badDarkBody := json.RawMessage(`{
		"server": {
			"hub": {
				"branding": {
					"palette": {
						"dark": {
							"primary": "#1e293b"
						}
					}
				}
			}
		}
	}`)
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/admin/server-config", badDarkBody)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "server.hub.branding.palette.dark.primary")
	assert.Contains(t, rec.Body.String(), "dark.")
	assert.Contains(t, rec.Body.String(), "4.50:1")

	for _, badLogoPath := range []string{"/auth/logout.svg", "/x/%2e%2e/api/v1/foo.png"} {
		badAssetPathBody := json.RawMessage(`{"server":{"hub":{"branding":{"logo":{"light":"` + badLogoPath + `"}}}}}`)
		rec = doRequest(t, srv, http.MethodPut, "/api/v1/admin/server-config", badAssetPathBody)
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "path %s: %s", badLogoPath, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "server.hub.branding.logo.light")
	}

	// Neutral-scale-only background override (R1) passes 200 OK and re-derives light.primary.
	neutralOnlyPut := json.RawMessage(`{
		"server": {
			"hub": {
				"branding": {
					"palette": {
						"neutral": {
							"50": "#fafaf9"
						}
					}
				}
			}
		}
	}`)
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/admin/server-config", neutralOnlyPut)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "#fafaf9", srv.ResolvedBranding().Light.Bg)
	assert.Equal(t, config.DefaultPrimaryScale()["600"], srv.ResolvedBranding().Light.Primary)

	// 3. Valid save applies live without restart; partial deep-merge preserves earlier keys.
	put1 := json.RawMessage(`{
		"server": {
			"hub": {
				"branding": {
					"name": "Acme Cloud",
					"subtitle": "Engineering Platform",
					"logo": {
						"light": "https://cdn.example.com/acme-light.svg"
					}
				}
			}
		}
	}`)
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/admin/server-config", put1)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "Acme Cloud", srv.ResolvedBranding().Name)

	// Partial update sets accent_color and logo.dark while preserving name, subtitle, and logo.light.
	put2 := json.RawMessage(`{
		"server": {
			"hub": {
				"branding": {
					"accent_color": "#0d9488",
					"logo": {
						"dark": "https://cdn.example.com/acme-dark.svg"
					}
				}
			}
		}
	}`)
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/admin/server-config", put2)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	resolved := srv.ResolvedBranding()
	assert.Equal(t, "Acme Cloud", resolved.Name)
	assert.Equal(t, "Engineering Platform", resolved.Subtitle)
	assert.Equal(t, "https://cdn.example.com/acme-light.svg", resolved.LogoLight)
	assert.Equal(t, "https://cdn.example.com/acme-dark.svg", resolved.LogoDark)
	assert.True(t, resolved.HasCustomPalette)

	// 4. GET /api/v1/settings/public exposes non-boolean branding object.
	rec = doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/settings/public", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var rawBranded map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rawBranded))
	brandObj, ok := rawBranded["branding"].(map[string]interface{})
	require.True(t, ok, "expected branding object in public settings")
	assert.Equal(t, "Acme Cloud", brandObj["name"])
	assert.Equal(t, "shown", brandObj["attribution"])
	assertJSONHasNoBooleans(t, brandObj, "branding")

	// 5. Maintenance mode exempts GET /api/v1/settings/public while blocking other API routes.
	member := createTestUser(t, st, "brand-member", "member@example.com")
	srv.maintenance.Set(true, "Scheduled upgrade")
	t.Cleanup(func() { srv.maintenance.Set(false, "") })

	recMaintPublic := doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/settings/public", nil)
	require.Equal(t, http.StatusOK, recMaintPublic.Code, "GET /api/v1/settings/public must pass through maintenance mode")
	var maintPayload PublicSettingsResponse
	require.NoError(t, json.Unmarshal(recMaintPublic.Body.Bytes(), &maintPayload))
	require.NotNil(t, maintPayload.Branding)
	assert.Equal(t, "Acme Cloud", maintPayload.Branding.Name)

	recMaintOther := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/projects", nil)
	assert.Equal(t, http.StatusServiceUnavailable, recMaintOther.Code, "non-exempt API routes must return 503 during maintenance mode")
	srv.maintenance.Set(false, "")

	// 6. Section reset restores compiled stock Scion defaults live.
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/admin/server-config/sections/"+opsettings.BrandingSectionName, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, config.DefaultBrandName, srv.ResolvedBranding().Name)
	assert.False(t, srv.ResolvedBranding().IsCustomized())
	assert.Nil(t, srv.ResolvedBranding().Public())
}

var scionDataScriptRegex = regexp.MustCompile(`<script id="__SCION_DATA__" type="application/json">(.*?)</script>`)

func TestWebServer_PreAuthSPAShellTitleAndInitialBranding(t *testing.T) {
	srv, _ := testServerWithOps(t, nil)

	assetsDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(assetsDir, "assets"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(assetsDir, "assets", "main.js"), []byte("console.log('boot');"), 0o644))

	ws := NewWebServer(WebServerConfig{
		AssetsDir:     assetsDir,
		SessionSecret: "0123456789abcdef0123456789abcdef",
		BaseURL:       "http://localhost:8080",
	})
	ws.SetBrandingSettingsProvider(srv)
	ws.SetMaintenanceState(srv.GetMaintenanceState())
	handler := ws.Handler()

	// Unbranded anonymous /login shell.
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "<title>Scion</title>")
	m := scionDataScriptRegex.FindStringSubmatch(body)
	require.Len(t, m, 2)
	var unbrandedData map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(m[1]), &unbrandedData))
	assert.Equal(t, "Scion", unbrandedData["title"])
	_, exists := unbrandedData["branding"]
	assert.False(t, exists, "unbranded shell must omit branding from __SCION_DATA__")

	// Apply live branding update and verify anonymous /login shell reflects it immediately,
	// including while maintenance mode is active.
	_, err := srv.GetOperationalSettings().Update(
		context.Background(),
		opsettings.BrandingSectionName,
		json.RawMessage(`{"name":"Acme <Portal>","short_name":"Acme","attribution":"hidden","accent_color":"#0d9488"}`),
		"admin@example.com",
		-1,
		"managed",
	)
	require.NoError(t, err)
	ApplySnapshot(srv, srv.GetOperationalSettings().Snapshot())
	srv.GetMaintenanceState().Set(true, "Maintenance window")

	req = httptest.NewRequest(http.MethodGet, "/login", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body = rec.Body.String()
	// html/template escapes '<' and '>' in <title> preventing HTML injection.
	assert.Contains(t, body, "<title>Acme &lt;Portal&gt;</title>")
	m = scionDataScriptRegex.FindStringSubmatch(body)
	require.Len(t, m, 2)
	var brandedData struct {
		Path     string                 `json:"path"`
		Title    string                 `json:"title"`
		Branding *config.PublicBranding `json:"branding"`
	}
	require.NoError(t, json.Unmarshal([]byte(m[1]), &brandedData))
	assert.Equal(t, "/login", brandedData.Path)
	assert.Equal(t, "Acme <Portal>", brandedData.Title)
	require.NotNil(t, brandedData.Branding)
	assert.Equal(t, "Acme <Portal>", brandedData.Branding.Name)
	assert.Equal(t, "Acme", brandedData.Branding.ShortName)
	assert.Equal(t, "hidden", brandedData.Branding.Attribution)

	// Audit and session protocol identifiers remain "scion_sess" / "Scion" regardless of UI branding.
	assert.Equal(t, "scion_sess", webSessionName)
}

func TestBrandingSettings_FileModePutFailsClosedOnMarshalError(t *testing.T) {
	srv, _, settingsPath := fileModeGCPIdentityServer(t)
	srv.config.ConfigPath = settingsPath

	// Seed settings.yaml with a YAML value (.nan) under server.hub.branding that yaml.v3 decodes
	// into float64(NaN), causing json.Marshal on the merged branding map to fail. File-mode PUT
	// must fail closed (422) and leave settings.yaml untouched rather than skipping validation.
	poisonedYAML := "schema_version: \"1\"\ndefault_timezone: UTC\nserver:\n  hub:\n    port: 9810\n    branding:\n      poison: .nan\n"
	require.NoError(t, os.WriteFile(settingsPath, []byte(poisonedYAML), 0o644))

	rr := httptest.NewRecorder()
	srv.handlePutServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"server":{"hub":{"branding":{"name":"Acme","palette":{"light":{"primary":"#eab308"}}}}}}`))
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "expected fail-closed 422, got %d: %s", rr.Code, rr.Body.String())

	after, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Equal(t, poisonedYAML, string(after), "failed validation must not overwrite settings.yaml")

	// Valid clean settings.yaml validates merged branding and rejects low-contrast light.primary in file mode too.
	cleanYAML := "schema_version: \"1\"\ndefault_timezone: UTC\nserver:\n  hub:\n    port: 9810\n"
	require.NoError(t, os.WriteFile(settingsPath, []byte(cleanYAML), 0o644))

	rr = httptest.NewRecorder()
	srv.handlePutServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"server":{"hub":{"branding":{"palette":{"light":{"primary":"#eab308"}}}}}}`))
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "server.hub.branding.palette.light.primary")
}
