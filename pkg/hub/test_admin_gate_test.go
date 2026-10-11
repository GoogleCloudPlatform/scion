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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const tiaHubID = "ti-a0-hub"

// Broker ids are UUIDs in the store.
const (
	tiaPluginBrokerID    = "0a0a0a0a-0000-4000-8000-000000000001"
	tiaColocatedBrokerID = "0a0a0a0a-0000-4000-8000-000000000002"
	tiaRemoteBrokerID    = "0a0a0a0a-0000-4000-8000-000000000003"
	tiaRemoteOnlineID    = "0a0a0a0a-0000-4000-8000-000000000004"
	tiaStaleOfflineID    = "0a0a0a0a-0000-4000-8000-000000000005"
)

// tiaGateServerConfig is a server config that passes the test admin gate.
func tiaGateServerConfig() ServerConfig {
	cfg := testServerConfig()
	cfg.HubID = tiaHubID
	cfg.EnableTestIdentities = true
	cfg.Workstation = false
	cfg.TestAdminGate = TestAdminGateConfig{
		HubAdmin:        true,
		HubID:           tiaHubID,
		HubClass:        config.HubClassTest,
		MarkerHubID:     tiaHubID,
		MarkerCreatedAt: time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC),
	}
	return cfg
}

// tiaCaptureAudit redirects the test admin audit sink output for the test.
func tiaCaptureAudit(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := testAdminAuditOutput
	testAdminAuditOutput = &buf
	t.Cleanup(func() { testAdminAuditOutput = prev })
	return &buf
}

// tiaNewServer builds a server with cfg on a fresh migrated SQLite store.
func tiaNewServer(t *testing.T, cfg ServerConfig) (*Server, store.Store, error) {
	t.Helper()
	s, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	srv, err := newTestHubServer(t, cfg, s)
	if err == nil {
		waitUserScopedDataSweep(t, srv)
	}
	return srv, s, err
}

// enableTestAdminGateForTest turns the gate on for a server built without
// it. Test-only: production code assigns testAdminGate once, in New.
func enableTestAdminGateForTest(srv *Server) {
	srv.testAdminGate = testAdminGate{hubAdmin: true, hubID: tiaHubID, hubClass: config.HubClassTest}
}

// --- Gate capture and named refusals in New --------------------------------

func TestTestAdminGate_NewCapturesImmutableGate(t *testing.T) {
	tiaCaptureAudit(t)
	srv, _, err := tiaNewServer(t, tiaGateServerConfig())
	require.NoError(t, err)
	assert.True(t, srv.TestAdminTierOn())
	assert.True(t, srv.testAdminTierEnabled(testAdminKindHubAdmin))
	assert.False(t, srv.testAdminTierEnabled(testAdminKindSuperAdmin))
	assert.False(t, srv.testAdminTierEnabled(testAdminKind("human")))
}

func TestTestAdminGate_OffByDefault(t *testing.T) {
	srv, _ := testServer(t)
	assert.False(t, srv.TestAdminTierOn())
	assert.False(t, srv.testAdminTierEnabled(testAdminKindHubAdmin))
	assert.False(t, srv.testAdminTierEnabled(testAdminKindSuperAdmin))
}

func TestTestAdminGate_NewRefusesEachConditionWithNamedError(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ServerConfig)
		want   error
	}{
		{"G3 super-admin flag", func(c *ServerConfig) { c.TestAdminGate.SuperAdmin = true }, ErrTestSuperAdminRequiresPolicyRegistry},
		{"G1 test identities off", func(c *ServerConfig) { c.EnableTestIdentities = false }, ErrTestAdminRequiresTestIdentities},
		{"G2 hub id mismatch", func(c *ServerConfig) { c.TestAdminGate.HubID = "other-hub" }, ErrTestAdminHubIDMismatch},
		{"G2 empty hub id", func(c *ServerConfig) { c.TestAdminGate.HubID = "" }, ErrTestAdminHubIDMismatch},
		{"G10 workstation", func(c *ServerConfig) { c.Workstation = true }, ErrTestAdminWorkstationHub},
		{"G4 class absent", func(c *ServerConfig) { c.TestAdminGate.HubClass = "" }, ErrTestAdminHubClassNotTest},
		{"G4 class not exact", func(c *ServerConfig) { c.TestAdminGate.HubClass = "Test" }, ErrTestAdminHubClassNotTest},
		{"G6 marker absent", func(c *ServerConfig) { c.TestAdminGate.MarkerHubID = "" }, ErrTestAdminMarkerAbsent},
		{"G6 marker hub mismatch", func(c *ServerConfig) { c.TestAdminGate.MarkerHubID = "other-hub" }, ErrTestAdminMarkerHubMismatch},
		{"G8 federation enabled", func(c *ServerConfig) { c.Federation.Enabled = true }, ErrTestAdminFederationConfigured},
		{"G8 trusted issuer", func(c *ServerConfig) {
			c.Federation.TrustedIssuers = []config.TrustedIssuerConfig{{IssuerURL: "https://issuer.example.invalid"}}
		}, ErrTestAdminFederationConfigured},
		{"G7 shared signing secret", func(c *ServerConfig) { c.SharedSigningSecret = "synthetic-test-secret" }, ErrTestAdminSharedSigningSecret},
		{"G7 pre-configured user key", func(c *ServerConfig) { c.UserTokenConfig.SigningKey = bytes.Repeat([]byte{1}, 32) }, ErrTestAdminPreconfiguredSigningKey},
		{"G7 pre-configured agent key", func(c *ServerConfig) { c.AgentTokenConfig.SigningKey = bytes.Repeat([]byte{2}, 32) }, ErrTestAdminPreconfiguredSigningKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := tiaCaptureAudit(t)
			cfg := tiaGateServerConfig()
			tc.mutate(&cfg)
			srv, _, err := tiaNewServer(t, cfg)
			require.Error(t, err)
			assert.Nil(t, srv)
			assert.True(t, errors.Is(err, tc.want), "got %v, want %v", err, tc.want)
			var named *TestAdminGateError
			require.True(t, errors.As(err, &named))
			assert.NotEmpty(t, named.Name)
			assert.Empty(t, buf.String(), "a refused gate writes no banner")
		})
	}
}

func TestTestAdminGate_NewRefusesWithoutStore(t *testing.T) {
	err := validateTestAdminGateConfig(tiaGateServerConfig(), false)
	assert.True(t, errors.Is(err, ErrTestAdminRequiresUserStore), "got %v", err)
}

// --- G7: signingKeySource for every key class -----------------------------

func TestSigningKeySource_EveryClassHubLocalAfterNew(t *testing.T) {
	tiaCaptureAudit(t)
	cfg := tiaGateServerConfig()
	cfg.OIDCConfig.Enabled = true
	cfg.OIDCConfig.IssuerURL = "https://hub.example.invalid"
	srv, _, err := tiaNewServer(t, cfg)
	require.NoError(t, err, "a hub with store-resolved keys passes G7, OIDC included")

	classes := testAdminSigningKeyClasses(true)
	got := map[string]bool{}
	for _, c := range classes {
		got[c.Class] = true
		src := srv.signingKeySource(c.KeyName)
		assert.True(t, signingKeySourceIsHubLocal(src), "%s source %q", c.Class, src)
	}
	for _, want := range []string{"user_jwt", "agent_token", "oidc", "download_url", "artifact_view_url", "list_cursor"} {
		assert.True(t, got[want], "class %s is checked", want)
	}
	assert.Len(t, testAdminSigningKeyClasses(false), len(classes)-1, "OIDC is checked only when enabled")
}

func TestSigningKeySource_SharedAndPreconfiguredLabels(t *testing.T) {
	cfg := testServerConfig()
	cfg.SharedSigningSecret = "synthetic-test-secret"
	srv, _, err := tiaNewServer(t, cfg)
	require.NoError(t, err)
	for _, k := range []string{SecretKeyUserSigningKey, SecretKeyAgentSigningKey, SecretKeyDownloadSigningKey, SecretKeyArtifactViewKey, SecretKeyListCursorKey} {
		assert.Equal(t, signingKeySourceSharedSecret, srv.signingKeySource(k), k)
	}
	assert.Equal(t, signingKeySourceUnresolved, srv.signingKeySource(SecretKeyOIDCSigningKey), "OIDC off: unresolved")

	cfg = testServerConfig()
	cfg.UserTokenConfig.SigningKey = bytes.Repeat([]byte{7}, 32)
	srv, _, err = tiaNewServer(t, cfg)
	require.NoError(t, err)
	assert.Equal(t, signingKeySourceConfig, srv.signingKeySource(SecretKeyUserSigningKey))
}

func TestSigningKeySource_HubLocalClassification(t *testing.T) {
	for src, want := range map[string]bool{
		signingKeySourceSecretBackend: true,
		signingKeySourceStore:         true,
		signingKeySourceGenerated:     true,
		signingKeySourceEphemeral:     true,
		signingKeySourceConfig:        false,
		signingKeySourceSharedSecret:  false,
		signingKeySourceLegacyStore:   false,
		signingKeySourceUnresolved:    false,
		"":                            false,
	} {
		assert.Equal(t, want, signingKeySourceIsHubLocal(src), "source %q", src)
	}
}

// Each key class, one at a time, refuses the gate when its source is not
// hub-local.
func TestSigningKeySource_VerifyRefusesEachClass(t *testing.T) {
	for _, c := range testAdminSigningKeyClasses(false) {
		for _, bad := range []string{signingKeySourceSharedSecret, signingKeySourceConfig, signingKeySourceLegacyStore} {
			t.Run(c.Class+"/"+bad, func(t *testing.T) {
				srv := &Server{}
				for _, other := range testAdminSigningKeyClasses(false) {
					srv.recordSigningKeySource(other.KeyName, signingKeySourceStore)
				}
				require.NoError(t, srv.verifyTestAdminSigningKeys(false))
				srv.recordSigningKeySource(c.KeyName, bad)
				err := srv.verifyTestAdminSigningKeys(false)
				require.True(t, errors.Is(err, ErrTestAdminSigningKeyNotHubLocal), "got %v", err)
				assert.Contains(t, err.Error(), c.Class)
			})
		}
	}
	t.Run("oidc/unresolved", func(t *testing.T) {
		srv := &Server{}
		for _, other := range testAdminSigningKeyClasses(false) {
			srv.recordSigningKeySource(other.KeyName, signingKeySourceStore)
		}
		err := srv.verifyTestAdminSigningKeys(true)
		require.True(t, errors.Is(err, ErrTestAdminSigningKeyNotHubLocal), "got %v", err)
		assert.Contains(t, err.Error(), "oidc")
	})
	t.Run("unrecorded class is unresolved", func(t *testing.T) {
		srv := &Server{}
		err := srv.verifyTestAdminSigningKeys(false)
		require.True(t, errors.Is(err, ErrTestAdminSigningKeyNotHubLocal), "got %v", err)
	})
}

// --- Immutability ------------------------------------------------------------

func TestTestAdminGate_NoExportedMutator(t *testing.T) {
	gt := reflect.TypeOf(testAdminGate{})
	for i := 0; i < gt.NumField(); i++ {
		assert.False(t, gt.Field(i).IsExported(), "testAdminGate field %s must be unexported", gt.Field(i).Name)
	}
	assert.Equal(t, 0, gt.NumMethod(), "testAdminGate has no exported methods")
	assert.Equal(t, 0, reflect.PointerTo(gt).NumMethod(), "*testAdminGate has no exported methods")

	// Every exported *Server method that mentions the test admin tier is a
	// read or a startup check; none sets the gate.
	allowed := map[string]bool{"TestAdminTierOn": true, "VerifyTestAdminBrokerSet": true, "VerifyTestAdminFederationOff": true}
	st := reflect.TypeOf(&Server{})
	for i := 0; i < st.NumMethod(); i++ {
		name := st.Method(i).Name
		if strings.Contains(name, "TestAdmin") {
			assert.True(t, allowed[name], "unexpected exported test admin method %s", name)
		}
	}

	// In production code the gate field is assigned exactly once, in New.
	assignments := 0
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err)
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}
			ast.Inspect(fn, func(m ast.Node) bool {
				as, ok := m.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, lhs := range as.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "testAdminGate" {
						assignments++
						assert.Equal(t, "New", fn.Name.Name, "%s assigns testAdminGate outside New", fset.Position(as.Pos()))
					}
				}
				return true
			})
			return false
		})
	}
	assert.Equal(t, 1, assignments, "testAdminGate is assigned exactly once in production code")
}

// --- Marker: no route, no handler-reachable writer ---------------------------

func TestTestInfraMarker_NoRouteOrHubCodeTouchesIt(t *testing.T) {
	for key, meta := range routeMetadataTable {
		for _, s := range []string{key, meta.Pattern, meta.RouteID} {
			l := strings.ToLower(s)
			assert.False(t, strings.Contains(l, "marker"), "route %q must not touch the test infrastructure marker", s)
		}
	}
	// No production file in pkg/hub or pkg/store references the marker's
	// ent entity or the pkg/testinfra writer. The hub never reads it either:
	// cmd reads it once at startup and passes the result in ServerConfig.
	for _, dir := range []string{".", "../store"} {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			src := string(data)
			for _, banned := range []string{"TestInfraMarker", "testinframarker", "pkg/testinfra"} {
				assert.False(t, strings.Contains(src, banned), "%s references %s", path, banned)
			}
			return nil
		})
		require.NoError(t, err)
	}
}

// --- G4 / G8: settings write paths -------------------------------------------

// tiaErrorKeys decodes the keys detail of a 422.
func tiaErrorKeys(t *testing.T, rr *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp), rr.Body.String())
	raw, _ := resp.Error.Details["keys"].([]interface{})
	out := make([]string, 0, len(raw))
	for _, k := range raw {
		out = append(out, k.(string))
	}
	return out
}

var tiaHubClassBodies = map[string]string{
	"nested":          `{"server":{"hub":{"class":"test"}}}`,
	"flat dotted":     `{"server.hub.class":"test"}`,
	"partly dotted":   `{"server.hub":{"class":"test"}}`,
	"case folded":     `{"Server":{"HUB":{"Class":"test"}}}`,
	"empty value":     `{"server":{"hub":{"class":""}}}`,
	"null value":      `{"server":{"hub":{"class":null}}}`,
	"with other keys": `{"default_model":"m1","server":{"hub":{"class":"test","auto_suspend_stalled":true}}}`,
}

func TestNeverAPIWritable_HubClassRefusedInFileMode(t *testing.T) {
	for name, body := range tiaHubClassBodies {
		t.Run(name, func(t *testing.T) {
			srv := &Server{}
			rr, settingsPath := fileModePutServerConfig(t, srv, body)
			assert.Equal(t, []string{config.HubClassKey}, tiaErrorKeys(t, rr))
			_, err := os.Stat(settingsPath)
			assert.True(t, os.IsNotExist(err), "a refused PUT writes no settings file")
		})
	}
}

func TestNeverAPIWritable_HubClassRefusedInDBAndWorkstationMode(t *testing.T) {
	for _, workstation := range []bool{false, true} {
		for name, body := range tiaHubClassBodies {
			t.Run(map[bool]string{false: "db", true: "workstation"}[workstation]+"/"+name, func(t *testing.T) {
				settingsPath := workstationHome(t)
				srv, st, _ := newSQLiteHubInMode(t, workstation, nil)
				require.Equal(t, workstation, srv.layer0Editable())
				before := hubSettingRevisions(t, st)
				rr := putServerConfig(t, srv, body)
				assert.Equal(t, []string{config.HubClassKey}, tiaErrorKeys(t, rr))
				data, err := os.ReadFile(settingsPath)
				require.NoError(t, err)
				assert.NotContains(t, string(data), "class")
				assert.Equal(t, before, hubSettingRevisions(t, st), "a refused PUT writes no DB row")
			})
		}
	}
}

func TestNeverAPIWritable_HubClassHasNoDBSection(t *testing.T) {
	layer1, layer0, _ := opsettings.ClassifyKeys([]string{config.HubClassKey})
	assert.Empty(t, layer1, "no Layer-1 section can store server.hub.class")
	assert.Equal(t, []string{config.HubClassKey}, layer0)
}

func TestNeverAPIWritable_GetDoesNotEchoHubClass(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	settingsPath := filepath.Join(tmpHome, ".scion", "settings.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(settingsPath), 0o700))
	require.NoError(t, os.WriteFile(settingsPath, []byte("schema_version: \"1\"\nserver:\n  hub:\n    class: test\n"), 0o644))
	srv := &Server{}
	getRR := httptest.NewRecorder()
	srv.handleGetServerConfig(getRR)
	require.Equal(t, http.StatusOK, getRR.Code, getRR.Body.String())
	assert.NotContains(t, getRR.Body.String(), `"class"`)
	// The GET body echoes back cleanly.
	echo := httptest.NewRecorder()
	srv.handleAdminServerConfig(echo, adminRequest(http.MethodPut, "/api/v1/admin/server-config", getRR.Body.String()))
	assert.Equal(t, http.StatusOK, echo.Code, echo.Body.String())
	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "class: test", "the file value is preserved")
}

var tiaFederationBodies = map[string]string{
	"top-level":       `{"federation":{"enabled":false}}`,
	"settings layout": `{"server":{"federation":{"enabled":true}}}`,
	"flat dotted":     `{"server.federation.enabled":true}`,
}

func TestFederationRefusedWhileTierOn_FileMode(t *testing.T) {
	for name, body := range tiaFederationBodies {
		t.Run(name, func(t *testing.T) {
			srv := &Server{}
			enableTestAdminGateForTest(srv)
			rr, _ := fileModePutServerConfig(t, srv, body)
			keys := tiaErrorKeys(t, rr)
			require.NotEmpty(t, keys)
			assert.Contains(t, rr.Body.String(), errTestAdminFederationRefused.Error())
		})
	}
}

func TestFederationRefusedWhileTierOn_DBAndWorkstationMode(t *testing.T) {
	for _, workstation := range []bool{false, true} {
		for name, body := range tiaFederationBodies {
			t.Run(map[bool]string{false: "db", true: "workstation"}[workstation]+"/"+name, func(t *testing.T) {
				workstationHome(t)
				srv, st, _ := newSQLiteHubInMode(t, workstation, nil)
				enableTestAdminGateForTest(srv)
				before := hubSettingRevisions(t, st)
				// adminRequest carries a super-admin identity: the refusal
				// applies to every caller.
				rr := putServerConfig(t, srv, body)
				require.NotEmpty(t, tiaErrorKeys(t, rr))
				assert.Equal(t, before, hubSettingRevisions(t, st))
			})
		}
	}
}

func TestFederationAllowedPastGateWhileTierOff(t *testing.T) {
	srv := &Server{}
	assert.Empty(t, srv.gateProtectedSettingsKeys([]byte(`{"federation":{"enabled":false}}`)))
	enableTestAdminGateForTest(srv)
	assert.Equal(t, []string{"federation"}, srv.gateProtectedSettingsKeys([]byte(`{"federation":{"enabled":false}}`)))
}

func TestFederationSectionResetRefusedWhileTierOn(t *testing.T) {
	workstationHome(t)
	srv, _, _ := newSQLiteHubInMode(t, false, nil)
	enableTestAdminGateForTest(srv)
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfigSectionReset(rr, adminRequest(http.MethodDelete, "/api/v1/admin/server-config/sections/federation", ""))
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
}

func TestFederationSnapshotNotInstalledWhileTierOn(t *testing.T) {
	workstationHome(t)
	srv, _, _ := newSQLiteHubInMode(t, false, nil)
	enableTestAdminGateForTest(srv)
	ApplySnapshot(srv, Layer1Snapshot{FederationConfig: &config.FederationConfig{
		Enabled:        true,
		TrustedIssuers: []config.TrustedIssuerConfig{{IssuerURL: "https://issuer.example.invalid", JWKSURL: "https://issuer.example.invalid/jwks"}},
	}})
	assert.Nil(t, srv.federationAuth.Load(), "no federation authenticator while the tier is on")
}

// --- Health summary ----------------------------------------------------------

func TestHealthSummary_TestAdminIndicators(t *testing.T) {
	for _, on := range []bool{false, true} {
		srv, _ := testServer(t)
		if on {
			enableTestAdminGateForTest(srv)
		}
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp HealthSummaryResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, on, resp.Hub.TestAdminIdentitiesEnabled)
		assert.False(t, resp.Hub.TestSuperAdminIdentitiesEnabled, "the super-admin kind is refused by this binary")
		require.NotNil(t, resp.Hub.LiveTestAdminIdentities)
		assert.Equal(t, 0, *resp.Hub.LiveTestAdminIdentities)
		body := rec.Body.String()
		for _, k := range []string{`"test_admin_identities_enabled"`, `"test_super_admin_identities_enabled"`, `"live_test_admin_identities"`, `"test_identities_enabled"`} {
			assert.Contains(t, body, k)
		}
	}
}

// --- Banner ------------------------------------------------------------------

func TestTestAdminBanner_EmittedThroughSinkAtLogLevelError(t *testing.T) {
	buf := tiaCaptureAudit(t)
	var slogBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&slogBuf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	_, _, err := tiaNewServer(t, tiaGateServerConfig())
	require.NoError(t, err)

	out := buf.String()
	require.True(t, strings.HasPrefix(out, testAdminAuditTag+" "), "tagged line: %q", out)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 1, "A0 emits only the gate-start line")
	var rec map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(lines[0], testAdminAuditTag+" ")), &rec))
	assert.Equal(t, testAdminAuditEventGateStart, rec["event"])
	assert.Equal(t, "WARN", rec["level"])
	assert.Equal(t, tiaHubID, rec["hub_id"])
	assert.Equal(t, config.HubClassTest, rec["hub_class"])
	assert.Equal(t, "2026-10-11T00:00:00Z", rec["marker_created_at"])
	assert.Equal(t, []any{string(testAdminKindHubAdmin)}, rec["kinds"])
	assert.NotContains(t, slogBuf.String(), "TEST ADMIN TIER", "the banner does not go through slog")
}

type tiaFailWriter struct{}

func (tiaFailWriter) Write([]byte) (int, error) { return 0, errors.New("synthetic write failure") }

func TestTestAdminBanner_WriteFailureRefusesStartup(t *testing.T) {
	prev := testAdminAuditOutput
	testAdminAuditOutput = tiaFailWriter{}
	t.Cleanup(func() { testAdminAuditOutput = prev })
	_, _, err := tiaNewServer(t, tiaGateServerConfig())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "startup banner")
}

// --- Broker check ------------------------------------------------------------

func tiaBroker(t *testing.T, s store.Store, id, status string, labels map[string]string) {
	t.Helper()
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), &store.RuntimeBroker{
		ID: id, Name: "broker-" + id[len(id)-4:], Slug: "broker-" + id[len(id)-4:], Status: status, Labels: labels,
		Created: time.Now(), Updated: time.Now(),
	}))
}

func TestTestAdminBroker_RemoteRegistrationRefusedWhileTierOn(t *testing.T) {
	srv, _ := testServer(t)
	enableTestAdminGateForTest(srv)
	srv.SetEmbeddedBrokerID(tiaColocatedBrokerID)

	for _, body := range []map[string]interface{}{
		{"name": "remote-broker"},
		{"name": "remote-broker", "brokerId": tiaRemoteBrokerID},
	} {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/brokers", body)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), ErrCodeTestAdminRemoteBrokerRefused)
	}

	rec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/brokers/join",
		map[string]interface{}{"brokerId": tiaRemoteBrokerID, "joinToken": "synthetic-join-token"})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeTestAdminRemoteBrokerRefused)
}

func TestTestAdminBroker_ColocatedRegistrationAllowed(t *testing.T) {
	srv, _ := testServer(t)
	enableTestAdminGateForTest(srv)
	srv.SetEmbeddedBrokerID(tiaColocatedBrokerID)

	rr := httptest.NewRecorder()
	assert.False(t, srv.refuseTestAdminBrokerRegistration(rr, tiaColocatedBrokerID), "the co-located broker is admitted")
	rr = httptest.NewRecorder()
	assert.True(t, srv.refuseTestAdminBrokerRegistration(rr, tiaRemoteBrokerID))
	assert.Equal(t, http.StatusForbidden, rr.Code)

	// Through the HTTP route the co-located id passes the gate (whatever
	// the registration itself then answers, it is not the gate refusal).
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/brokers",
		map[string]interface{}{"name": "colocated", "brokerId": tiaColocatedBrokerID})
	assert.NotContains(t, rec.Body.String(), ErrCodeTestAdminRemoteBrokerRefused, rec.Body.String())
}

func TestTestAdminBroker_RegistrationUnaffectedWhileTierOff(t *testing.T) {
	srv, _ := testServer(t)
	rr := httptest.NewRecorder()
	assert.False(t, srv.refuseTestAdminBrokerRegistration(rr, tiaRemoteBrokerID))
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/brokers", map[string]interface{}{"name": "remote-broker"})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

func TestTestAdminBroker_AuthenticationAdmission(t *testing.T) {
	srv, s := testServer(t)
	tiaBroker(t, s, tiaPluginBrokerID, store.BrokerStatusOnline, map[string]string{pluginBrokerLabel: "chat"})
	ctx := context.Background()

	// Tier off: nothing is refused here.
	assert.NoError(t, srv.testAdminBrokerAdmission(ctx, tiaRemoteBrokerID))

	enableTestAdminGateForTest(srv)
	srv.SetEmbeddedBrokerID(tiaColocatedBrokerID)
	assert.NoError(t, srv.testAdminBrokerAdmission(ctx, tiaColocatedBrokerID))
	assert.NoError(t, srv.testAdminBrokerAdmission(ctx, tiaPluginBrokerID), "the hub's own plugin records")
	assert.ErrorIs(t, srv.testAdminBrokerAdmission(ctx, tiaRemoteBrokerID), ErrTestAdminRemoteBrokerRefused)
	assert.ErrorIs(t, srv.testAdminBrokerAdmission(ctx, ""), ErrTestAdminRemoteBrokerRefused)

	// The HMAC validator consults the admission before any credential.
	require.NotNil(t, srv.brokerAuthService)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runtime-brokers/connect", nil)
	req.Header.Set(HeaderBrokerID, tiaRemoteBrokerID)
	_, err := srv.brokerAuthService.ValidateBrokerSignature(ctx, req)
	assert.ErrorIs(t, err, ErrTestAdminRemoteBrokerRefused)
	_, err = srv.brokerAuthService.ValidateBrokerSignatureWithRotation(ctx, req)
	assert.ErrorIs(t, err, ErrTestAdminRemoteBrokerRefused)
}

func TestTestAdminBroker_StartupBrokerSetCheck(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	tiaBroker(t, s, tiaColocatedBrokerID, store.BrokerStatusOnline, nil)
	tiaBroker(t, s, tiaPluginBrokerID, store.BrokerStatusOnline, map[string]string{pluginBrokerLabel: "chat"})
	tiaBroker(t, s, tiaStaleOfflineID, store.BrokerStatusOffline, nil)

	// Tier off: no check.
	tiaBroker(t, s, tiaRemoteOnlineID, store.BrokerStatusOnline, nil)
	assert.NoError(t, srv.VerifyTestAdminBrokerSet(ctx))

	enableTestAdminGateForTest(srv)
	srv.SetEmbeddedBrokerID(tiaColocatedBrokerID)
	err := srv.VerifyTestAdminBrokerSet(ctx)
	require.ErrorIs(t, err, ErrTestAdminRemoteBrokerActive)
	assert.Contains(t, err.Error(), tiaRemoteOnlineID)

	require.NoError(t, s.DeleteRuntimeBroker(ctx, tiaRemoteOnlineID))
	assert.NoError(t, srv.VerifyTestAdminBrokerSet(ctx), "co-located, plugin and offline brokers pass")
}

func TestVerifyTestAdminFederationOff(t *testing.T) {
	workstationHome(t)
	srv, _, _ := newSQLiteHubInMode(t, false, nil)
	assert.NoError(t, srv.VerifyTestAdminFederationOff(), "tier off: no check")
	srv.config.Federation.Enabled = true
	assert.NoError(t, srv.VerifyTestAdminFederationOff(), "tier off: no check")

	enableTestAdminGateForTest(srv)
	assert.ErrorIs(t, srv.VerifyTestAdminFederationOff(), ErrTestAdminFederationConfigured)
	srv.config.Federation = config.FederationConfig{}
	assert.NoError(t, srv.VerifyTestAdminFederationOff())
}
