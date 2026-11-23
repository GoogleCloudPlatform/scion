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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	systemoneprovider "github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider/geminidecisions"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider/jevwire"
	systemoneruntime "github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

const (
	systemOneAdminPath    = "/api/v1/admin/system-one/instances"
	systemOneAdaptersPath = "/api/v1/admin/system-one/adapters"
)

type testSystemOnePublicResolver struct{}

func (testSystemOnePublicResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
}

type testSystemOnePort443Dialer struct {
	target string
}

func (d testSystemOnePort443Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var nd net.Dialer
	if port == "443" {
		return nd.DialContext(ctx, network, d.target)
	}
	return nd.DialContext(ctx, network, addr)
}

func testSystemOneEgressOptions(t *testing.T, tlsSrv *httptest.Server) []egress.Option {
	t.Helper()
	tr, ok := tlsSrv.Client().Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, tr.TLSClientConfig)
	tlsCfg := tr.TLSClientConfig.Clone()
	tlsCfg.ServerName = "127.0.0.1"
	return []egress.Option{
		egress.WithLoopbackServerForTesting(tlsSrv.URL, tlsSrv.Client()),
		egress.WithDialerForTesting(testSystemOnePublicResolver{}, testSystemOnePort443Dialer{
			target: tlsSrv.Listener.Addr().String(),
		}),
		egress.WithTLSConfigForTesting(tlsCfg),
	}
}

func newSystemOneTestServer(t *testing.T, enabled bool) (*Server, store.Store, *secret.LocalBackend) {
	t.Helper()
	srv, s := testServerWithOps(t, nil)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret-signing-key")
	srv.SetSecretBackend(localBackend)
	// Always isolate Hub tests from any ambient host/container ADC or metadata server.
	srv.SetSystemOneADCForTesting(t, func(context.Context, []string) (*SystemOneADCCredentials, error) {
		return nil, egress.ErrAuth
	})

	if enabled {
		ctx := context.Background()
		ops := srv.GetOperationalSettings()
		doc := fmt.Sprintf(`{"overrides":{%q:true}}`, experiments.SystemOne)
		_, err := ops.Update(ctx, "experiments", json.RawMessage(doc), "test", ops.SectionRevision("experiments"), "managed")
		require.NoError(t, err)
	}
	return srv, s, localBackend
}

func doRawJSONAsUser(t *testing.T, srv *Server, user *store.User, method, path, rawBody string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(rawBody))
	req.Header.Set("Content-Type", "application/json")
	tok, _, _, err := srv.userTokenService.GenerateTokenPair(user.ID, user.Email, user.DisplayName, user.Role, ClientTypeCLI)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestSystemOneInstances_AuthzAndGating(t *testing.T) {
	ctx := context.Background()

	t.Run("experiment off returns 404 to admin on all routes", func(t *testing.T) {
		srv, s, _ := newSystemOneTestServer(t, false)
		superAdmin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-off-super", store.SystemRoleSuperAdmin))
		require.NoError(t, err)

		for _, tc := range []struct {
			method string
			path   string
			body   any
		}{
			{http.MethodGet, systemOneAdminPath, nil},
			{http.MethodPost, systemOneAdminPath, map[string]any{"name": "inst-a", "adapter": "typesafe", "model": "jev-1.14.0", "auth": map[string]any{"kind": "api_key", "apiKey": "k"}}},
			{http.MethodGet, systemOneAdminPath + "/inst-a", nil},
			{http.MethodDelete, systemOneAdminPath + "/inst-a", nil},
			{http.MethodPost, systemOneAdminPath + "/inst-a/probe", map[string]any{"models": true}},
		} {
			rec := doRequestAsUser(t, srv, superAdmin, tc.method, tc.path, tc.body)
			assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s when hub.system_one is off: %s", tc.method, tc.path, rec.Body.String())
		}
	})

	t.Run("role seeding permissions", func(t *testing.T) {
		_, s, _ := newSystemOneTestServer(t, true)
		wantPerms := []string{
			"system_one_instance.read",
			"system_one_instance.create",
			"system_one_instance.delete",
			"system_one_instance.execute",
		}

		for _, roleName := range []string{store.SystemRoleSuperAdmin, store.SystemRoleHubAdmin} {
			rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
			require.NoError(t, err)
			for _, perm := range wantPerms {
				assert.Contains(t, rd.Permissions, perm, "role %q must hold %s", roleName, perm)
			}
		}

		for _, role := range []struct {
			name  string
			scope string
		}{
			{store.SystemRoleHubMember, store.RoleScopeSystem},
			{store.SystemRoleHubViewer, store.RoleScopeSystem},
			{store.ProjectRoleOwner, store.RoleScopeProject},
			{store.ProjectRoleAdmin, store.RoleScopeProject},
			{store.ProjectRoleMember, store.RoleScopeProject},
		} {
			rd, err := s.GetRoleDefinitionByName(ctx, role.name, role.scope)
			require.NoError(t, err)
			for _, perm := range wantPerms {
				assert.NotContains(t, rd.Permissions, perm, "role %q must not hold %s", role.name, perm)
			}
		}
	})

	t.Run("positive and negative caller authorization matrix", func(t *testing.T) {
		srv, s, _ := newSystemOneTestServer(t, true)

		superAdmin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-super", store.SystemRoleSuperAdmin))
		require.NoError(t, err)
		hubAdmin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-hubadmin", store.SystemRoleHubAdmin))
		require.NoError(t, err)
		hubMember, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-member", store.SystemRoleHubMember))
		require.NoError(t, err)
		hubViewer, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-viewer", store.SystemRoleHubViewer))
		require.NoError(t, err)

		// Unauthenticated -> 401.
		unauthReq := httptest.NewRequest(http.MethodGet, systemOneAdminPath, nil)
		unauthRec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(unauthRec, unauthReq)
		assert.Equal(t, http.StatusUnauthorized, unauthRec.Code)

		// Agent JWT -> 403.
		project := &store.Project{
			ID:      tid("s1-proj"),
			Name:    "System One Authz Project",
			Slug:    "s1-authz-project",
			OwnerID: hubMember.ID,
			Created: time.Now(),
			Updated: time.Now(),
		}
		require.NoError(t, s.CreateProject(ctx, project))
		agentTok, err := srv.GenerateAgentToken(tid("s1-agent"), project.ID, nil, AgentRoleFull, nil)
		require.NoError(t, err)
		rec := doRequestWithToken(t, srv, agentTok, http.MethodGet, systemOneAdminPath, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code)

		// Scoped admin UAT -> 403 session_required (CREDENTIAL_MANAGEMENT).
		uat := mintHubConfigToken(t, srv, superAdmin.ID, hubBoundary(), "hub_health:read")
		for _, tc := range []struct {
			method string
			path   string
		}{
			{http.MethodGet, systemOneAdminPath},
			{http.MethodPost, systemOneAdminPath},
			{http.MethodGet, systemOneAdminPath + "/inst-a"},
			{http.MethodDelete, systemOneAdminPath + "/inst-a"},
			{http.MethodPost, systemOneAdminPath + "/inst-a/probe"},
		} {
			uatRec := doRequestWithToken(t, srv, uat, tc.method, tc.path, map[string]any{})
			requireSessionOnlyRefusal(t, uatRec, authzop.ReasonCredentialManagement, tc.method+" "+tc.path)
		}

		// Non-admin human sessions (hub-member, hub-viewer) -> 403.
		for _, caller := range []*store.User{hubMember, hubViewer} {
			assert.Equal(t, http.StatusForbidden, doRequestAsUser(t, srv, caller, http.MethodGet, systemOneAdminPath, nil).Code)
			assert.Equal(t, http.StatusForbidden, doRequestAsUser(t, srv, caller, http.MethodPost, systemOneAdminPath, map[string]any{
				"name": "denied-inst", "adapter": "typesafe", "model": "jev-1.14.0",
				"auth": map[string]any{"kind": "api_key", "apiKey": "sentinel"},
			}).Code)
		}

		// Admin test-identity policy classifies all 4 System One permissions as deny.
		for _, perm := range []string{
			"system_one_instance.read",
			"system_one_instance.create",
			"system_one_instance.delete",
			"system_one_instance.execute",
		} {
			assert.Equal(t, testAdminPolicyDeny, testAdminPermissionPolicy[perm], "permission %s must be denied to test admin identities", perm)
		}

		// hub-admin and super-admin sessions succeed on create/list/get/delete.
		rec = doRequestAsUser(t, srv, hubAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
			"name":    "hubadmin-inst",
			"adapter": "typesafe",
			"model":   "jev-1.14.0",
			"auth":    map[string]any{"kind": "api_key", "apiKey": "hubadmin-key-1"},
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var hubAdminCreated SystemOneInstanceResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &hubAdminCreated))
		assert.Equal(t, "https://api.typesafe.ai", hubAdminCreated.Endpoint, "omitted endpoint defaults to https://api.typesafe.ai")

		rec = doRequestAsUser(t, srv, superAdmin, http.MethodGet, systemOneAdminPath+"/hubadmin-inst", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		// Unsupported methods -> 405; unknown subpaths -> 404.
		assert.Equal(t, http.StatusMethodNotAllowed, doRequestAsUser(t, srv, hubAdmin, http.MethodPut, systemOneAdminPath, map[string]any{}).Code)
		assert.Equal(t, http.StatusMethodNotAllowed, doRequestAsUser(t, srv, hubAdmin, http.MethodPut, systemOneAdminPath+"/hubadmin-inst", map[string]any{}).Code)
		assert.Equal(t, http.StatusMethodNotAllowed, doRequestAsUser(t, srv, hubAdmin, http.MethodGet, systemOneAdminPath+"/hubadmin-inst/probe", nil).Code)
		assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, hubAdmin, http.MethodPost, systemOneAdminPath+"/hubadmin-inst/unknown", map[string]any{}).Code)
		assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, hubAdmin, http.MethodPost, systemOneAdminPath+"/hubadmin-inst/probe/extra", map[string]any{}).Code)

		rec = doRequestAsUser(t, srv, hubAdmin, http.MethodDelete, systemOneAdminPath+"/hubadmin-inst", nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	})
}

func TestSystemOneInstances_CRUDAndProbe(t *testing.T) {
	ctx := context.Background()
	srv, s, _ := newSystemOneTestServer(t, true)
	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-crud-admin", store.SystemRoleHubAdmin))
	require.NoError(t, err)

	var (
		mu             sync.Mutex
		seenAuth       []string
		seenModels     []string
		seenDangerHdrs []string
	)

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenAuth = append(seenAuth, r.Header.Get("Authorization"))
		for name := range r.Header {
			lower := strings.ToLower(name)
			if strings.HasPrefix(lower, "x-typesafe-") || strings.Contains(lower, "danger-zone") {
				seenDangerHdrs = append(seenDangerHdrs, name)
			}
		}
		mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"jev-1.14.0","description":"Latest","release_date":"2026-06-01"},{"name":"jev-1.13.0","description":"Previous","release_date":"2026-03-01"},{"name":"bad\u0007model","description":"ControlCharDropped","release_date":"2026-01-01"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/systemone":
			var wireReq struct {
				Model     string                     `json:"model"`
				Questions map[string]json.RawMessage `json:"questions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&wireReq); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			mu.Lock()
			seenModels = append(seenModels, wireReq.Model)
			mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Typesafe-Request-Id", "req-probe-42")
			resp := fmt.Sprintf(`{
				"model": %q,
				"answers": {
					"safe": {"type": "noul", "noul": 0.9},
					"severity": {
						"type": "choice",
						"choice": "low",
						"confidence": 0.55,
						"probabilities": {"low": 0.7, "medium": 0.2, "high": 0.1}
					}
				},
				"usage": {"input_tokens": 24, "output_tokens": 6}
			}`, wireReq.Model)
			_, _ = w.Write([]byte(resp))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(tlsSrv.Close)

	srv.setSystemOneRuntimeOptions(
		systemoneruntime.WithEgressOptionsForTesting(
			testSystemOneEgressOptions(t, tlsSrv)...,
		),
	)

	const (
		sentinelKeyA  = "SENTINEL_TYPESAFE_KEY_ALPHA_9001"
		sentinelKeyB  = "SENTINEL_TYPESAFE_KEY_BETA_9002"
		sentinelInput = "SENTINEL_PROBE_SENSITIVE_TEXT_7788"
	)

	// Validation errors (400).
	t.Run("validation rejections", func(t *testing.T) {
		for _, badKind := range []string{"none", "google_id_token", "azure_entra", "unknown_kind"} {
			rec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
				"name":    "bad-auth-" + strings.ReplaceAll(badKind, "_", "-"),
				"adapter": "typesafe",
				"model":   "jev-1.14.0",
				"auth":    map[string]any{"kind": badKind, "apiKey": "k"},
			})
			require.Equal(t, http.StatusBadRequest, rec.Code, "kind=%s body=%s", badKind, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "auth kind not yet supported")
		}
		// TypeSafe does not declare google_access_token in SupportedAuth.
		tsADCRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
			"name":    "bad-auth-typesafe-adc",
			"adapter": "typesafe",
			"model":   "jev-1.14.0",
			"auth":    map[string]any{"kind": "google_access_token"},
		})
		require.Equal(t, http.StatusBadRequest, tsADCRec.Code, tsADCRec.Body.String())
		assert.Contains(t, tsADCRec.Body.String(), "does not support auth kind")

		badPayloads := []struct {
			name string
			body map[string]any
			want string
		}{
			{
				name: "invalid slug",
				body: map[string]any{"name": "Bad_Name!", "adapter": "typesafe", "model": "jev-1.14.0", "auth": map[string]any{"kind": "api_key", "apiKey": "k"}},
				want: "slug",
			},
			{
				name: "unknown adapter",
				body: map[string]any{"name": "bad-adapter", "adapter": "no-such-adapter", "model": "m", "auth": map[string]any{"kind": "api_key", "apiKey": "k"}},
				want: "unknown system one adapter",
			},
			{
				name: "allowPrivateNetwork rejected",
				body: map[string]any{"name": "bad-priv", "adapter": "typesafe", "model": "jev-1.14.0", "allowPrivateNetwork": true, "auth": map[string]any{"kind": "api_key", "apiKey": "k"}},
				want: "allowPrivateNetwork",
			},
			{
				name: "plaintext http endpoint rejected",
				body: map[string]any{"name": "bad-http", "adapter": "typesafe", "model": "jev-1.14.0", "endpoint": "http://api.typesafe.ai", "auth": map[string]any{"kind": "api_key", "apiKey": "k"}},
				want: "https",
			},
			{
				name: "missing model for typesafe",
				body: map[string]any{"name": "bad-model", "adapter": "typesafe", "auth": map[string]any{"kind": "api_key", "apiKey": "k"}},
				want: "model is required",
			},
			{
				name: "secret-like option key rejected",
				body: map[string]any{"name": "bad-opt", "adapter": "typesafe", "model": "jev-1.14.0", "options": map[string]string{"api_key": "x"}, "auth": map[string]any{"kind": "api_key", "apiKey": "k"}},
				want: "credentials",
			},
			{
				name: "excessive callTimeout rejected",
				body: map[string]any{"name": "bad-timeout", "adapter": "typesafe", "model": "jev-1.14.0", "limits": map[string]any{"callTimeout": "45s"}, "auth": map[string]any{"kind": "api_key", "apiKey": "k"}},
				want: "30s",
			},
		}
		for _, tc := range badPayloads {
			rec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, tc.body)
			require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", tc.name, rec.Body.String())
			assert.Contains(t, strings.ToLower(rec.Body.String()), strings.ToLower(tc.want), tc.name)
		}
	})

	// AC-1: Create two instances with distinct models and credentials.
	recA := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":     "typesafe-fast",
		"adapter":  "typesafe",
		"model":    "jev-1.14.0",
		"endpoint": tlsSrv.URL,
		"limits":   map[string]any{"callTimeout": "2s", "maxInFlight": 4},
		"auth":     map[string]any{"kind": "api_key", "apiKey": sentinelKeyA},
	})
	require.Equal(t, http.StatusCreated, recA.Code, recA.Body.String())
	var instA SystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(recA.Body.Bytes(), &instA))
	assert.Equal(t, "typesafe-fast", instA.Name)
	assert.Equal(t, "typesafe", instA.Adapter)
	assert.Equal(t, "jev-1.14.0", instA.Model)
	assert.Equal(t, int64(1), instA.Revision)
	assert.True(t, instA.Enabled)
	assert.Equal(t, "systemone.instance."+instA.ID, instA.Auth.SecretName)
	assert.NotContains(t, recA.Body.String(), sentinelKeyA)

	// Create second instance (also test default endpoint resolution on a third row, then use tlsSrv.URL for probe).
	recB := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":     "typesafe-prev",
		"adapter":  "typesafe",
		"model":    "jev-1.13.0",
		"endpoint": tlsSrv.URL,
		"auth":     map[string]any{"kind": "api_key", "apiKey": sentinelKeyB},
	})
	require.Equal(t, http.StatusCreated, recB.Code, recB.Body.String())
	var instB SystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(recB.Body.Bytes(), &instB))
	assert.Equal(t, "jev-1.13.0", instB.Model)
	assert.Equal(t, int64(1), instB.Revision)
	assert.NotEqual(t, instA.ID, instB.ID)

	// Duplicate name -> 409 and does not clobber existing secret.
	dupRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":     "typesafe-fast",
		"adapter":  "typesafe",
		"model":    "jev-1.14.0",
		"endpoint": tlsSrv.URL,
		"auth":     map[string]any{"kind": "api_key", "apiKey": "DUPLICATE_SHOULD_NOT_OVERWRITE"},
	})
	assert.Equal(t, http.StatusConflict, dupRec.Code, dupRec.Body.String())

	// List instances & get by name.
	listRec := doRequestAsUser(t, srv, admin, http.MethodGet, systemOneAdminPath+"?adapter=typesafe&enabled=true", nil)
	require.Equal(t, http.StatusOK, listRec.Code, listRec.Body.String())
	var listed ListSystemOneInstancesResponse
	require.NoError(t, json.Unmarshal(listRec.Body.Bytes(), &listed))
	assert.Equal(t, 2, listed.TotalCount)
	require.Len(t, listed.Instances, 2)

	// Store revision semantics (AC-1): toggling Enabled preserves Revision; changing Model bumps Revision.
	storedA, err := s.GetSystemOneInstanceByName(ctx, "typesafe-fast")
	require.NoError(t, err)
	storedA.Enabled = false
	require.NoError(t, s.UpdateSystemOneInstance(ctx, storedA))
	afterToggle, err := s.GetSystemOneInstanceByName(ctx, "typesafe-fast")
	require.NoError(t, err)
	assert.Equal(t, int64(1), afterToggle.Revision)

	afterToggle.Enabled = true
	afterToggle.Limits.MaxInFlight = 8
	require.NoError(t, s.UpdateSystemOneInstance(ctx, afterToggle))
	afterLimitsUpdate, err := s.GetSystemOneInstanceByName(ctx, "typesafe-fast")
	require.NoError(t, err)
	assert.Equal(t, int64(2), afterLimitsUpdate.Revision)

	// Probe models ({"models": true}) — also verifies L1 sanitization strips control char in second model name.
	modelsProbeRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/typesafe-fast/probe", map[string]any{
		"models": true,
	})
	require.Equal(t, http.StatusOK, modelsProbeRec.Code, modelsProbeRec.Body.String())
	var modelsProbe ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(modelsProbeRec.Body.Bytes(), &modelsProbe))
	assert.Equal(t, systemoneruntime.ClassSuccess, modelsProbe.Class)
	assert.Equal(t, []string{"jev-1.14.0", "jev-1.13.0"}, modelsProbe.Models)

	// Capture slog logs during inline spec probes to verify zero secret/input leakage.
	var logBuf bytes.Buffer
	origDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(origDefault) })
	srv.setSystemOneRuntimeOptions(
		systemoneruntime.WithEgressOptionsForTesting(
			testSystemOneEgressOptions(t, tlsSrv)...,
		),
	)

	inlineSpec := map[string]any{
		"version": 1,
		"questions": []map[string]any{
			{
				"id":           "safe",
				"type":         "binary",
				"instructions": "Assess whether probe.text is safe.",
				"criteria": map[string]any{
					"true":  "Safe to proceed",
					"false": "Unsafe",
				},
			},
			{
				"id":           "severity",
				"type":         "choice",
				"instructions": "Classify severity of probe.text.",
				"options": []map[string]any{
					{"id": "low", "description": "Low risk"},
					{"id": "medium", "description": "Moderate risk"},
					{"id": "high", "description": "High risk"},
				},
			},
		},
		"output": map[string]any{
			"question": "safe",
		},
	}

	// Probe both instances independently (AC-1).
	for _, tc := range []struct {
		name     string
		wantMod  string
		wantRev  int64
		wantAuth string
	}{
		{"typesafe-fast", "jev-1.14.0", 2, "Bearer " + sentinelKeyA},
		{"typesafe-prev", "jev-1.13.0", 1, "Bearer " + sentinelKeyB},
	} {
		probeRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/"+tc.name+"/probe", map[string]any{
			"spec":  inlineSpec,
			"probe": map[string]any{"text": sentinelInput},
		})
		require.Equal(t, http.StatusOK, probeRec.Code, probeRec.Body.String())

		var probeResp ProbeSystemOneInstanceResponse
		require.NoError(t, json.Unmarshal(probeRec.Body.Bytes(), &probeResp))
		assert.Equal(t, systemoneruntime.ClassSuccess, probeResp.Class)
		require.NotNil(t, probeResp.Decision)
		require.NotNil(t, probeResp.Decision.Bool)
		assert.True(t, *probeResp.Decision.Bool)
		require.NotNil(t, probeResp.Evaluation)
		assert.Equal(t, tc.wantMod, probeResp.Evaluation.ConfiguredModel)
		assert.Equal(t, tc.wantMod, probeResp.Evaluation.ResolvedModel)
		require.Len(t, probeResp.Evaluation.Attempts, 1)
		assert.Equal(t, tc.name, probeResp.Evaluation.Attempts[0].Instance)
		assert.Equal(t, tc.wantRev, probeResp.Evaluation.Attempts[0].InstanceRevision)
		assert.Equal(t, "req-probe-42", probeResp.Evaluation.Attempts[0].ProviderRequestID)
		assert.NotEmpty(t, probeResp.Evaluation.Input.SHA256)

		// Choice native confidence + portable metrics present.
		sevAns, ok := probeResp.Evaluation.Answers["severity"]
		require.True(t, ok)
		require.NotNil(t, sevAns.Choice)
		assert.Equal(t, "low", *sevAns.Choice)
		assert.Contains(t, sevAns.NativeMetrics, "typesafe.confidence")
		assert.Greater(t, sevAns.Metrics["p_top"].Value, 0.69)

		// Neither sentinel API key nor sentinel probe input appears in response or logs.
		assert.NotContains(t, probeRec.Body.String(), sentinelKeyA)
		assert.NotContains(t, probeRec.Body.String(), sentinelKeyB)
		assert.NotContains(t, probeRec.Body.String(), sentinelInput)
	}

	mu.Lock()
	assert.Contains(t, seenAuth, "Bearer "+sentinelKeyA)
	assert.Contains(t, seenAuth, "Bearer "+sentinelKeyB)
	assert.Equal(t, []string{"jev-1.14.0", "jev-1.13.0"}, seenModels)
	assert.Empty(t, seenDangerHdrs, "no X-Typesafe-* danger-zone header may ever be sent")
	mu.Unlock()

	logs := logBuf.String()
	assert.NotContains(t, logs, sentinelKeyA)
	assert.NotContains(t, logs, sentinelKeyB)
	assert.NotContains(t, logs, sentinelInput)

	// Capability rejection: uncertain_if threshold rule -> 400 category="capability".
	capRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/typesafe-fast/probe", map[string]any{
		"spec": map[string]any{
			"version": 1,
			"questions": []map[string]any{{
				"id": "q1", "type": "binary", "instructions": "Check probe.text.",
			}},
			"uncertain_if": []map[string]any{{
				"question": "q1", "metric": "p_top", "below": 0.8,
			}},
		},
		"probe": map[string]any{"text": "hello"},
	})
	require.Equal(t, http.StatusBadRequest, capRec.Code, capRec.Body.String())
	assert.Contains(t, capRec.Body.String(), "capability")

	// Input oversize (>16 KiB) -> 200 with ClassFailed, reason="input_too_large".
	oversizeRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/typesafe-fast/probe", map[string]any{
		"spec":  inlineSpec,
		"probe": map[string]any{"text": strings.Repeat("a", 17*1024)},
	})
	require.Equal(t, http.StatusOK, oversizeRec.Code, oversizeRec.Body.String())
	var oversizeResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(oversizeRec.Body.Bytes(), &oversizeResp))
	assert.Equal(t, systemoneruntime.ClassFailed, oversizeResp.Class)
	assert.Equal(t, "input_too_large", oversizeResp.Reason)

	// Bounded HTTP body limits: >64 KiB on create and >256 KiB on probe -> 413 Request Entity Too Large.
	tooLargeCreateRec := doRawJSONAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath,
		fmt.Sprintf(`{"name":"too-big","adapter":"typesafe","model":"jev-1.14.0","auth":{"kind":"api_key","apiKey":%q}}`, strings.Repeat("k", 70*1024)))
	assert.Equal(t, http.StatusRequestEntityTooLarge, tooLargeCreateRec.Code, tooLargeCreateRec.Body.String())

	tooLargeProbeRec := doRawJSONAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/typesafe-fast/probe",
		fmt.Sprintf(`{"probeText":%q}`, strings.Repeat("p", 260*1024)))
	assert.Equal(t, http.StatusRequestEntityTooLarge, tooLargeProbeRec.Code, tooLargeProbeRec.Body.String())
}

func TestSystemOneInstances_CredentialSeparationAndSecretIsolation(t *testing.T) {
	ctx := context.Background()
	srv, s, localBackend := newSystemOneTestServer(t, true)
	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-sec-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)

	const sentinelSecret = "SENTINEL_NEVER_LEAK_API_KEY_551199"

	// Malformed JSON containing sentinel secret must not echo the raw secret value.
	badJSONRec := doRawJSONAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath,
		fmt.Sprintf(`{"name":"leak-check","adapter":"typesafe","model":"jev-1.14.0","auth":%q}`, sentinelSecret))
	require.Equal(t, http.StatusBadRequest, badJSONRec.Code)
	assert.NotContains(t, badJSONRec.Body.String(), sentinelSecret)

	// Create instance with sentinel API key.
	createRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "sec-isolated",
		"adapter": "typesafe",
		"model":   "jev-1.14.0",
		"auth":    map[string]any{"kind": "api_key", "apiKey": sentinelSecret},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	assert.NotContains(t, createRec.Body.String(), sentinelSecret)

	var created SystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))
	secretKey := created.Auth.SecretName
	require.Equal(t, "systemone.instance."+created.ID, secretKey)

	// Verify stored secret is hub-scoped internal.
	sv, err := localBackend.Get(ctx, secretKey, store.ScopeHub, srv.hubID)
	require.NoError(t, err)
	assert.Equal(t, store.SecretTypeInternal, sv.SecretType)
	assert.Equal(t, store.ScopeHub, sv.Scope)
	assert.Equal(t, sentinelSecret, sv.Value)

	// Generic /api/v1/secrets endpoints must neither list nor read/overwrite/patch/delete systemone.instance.<id>.
	listSecretsRec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/secrets?scope=hub", nil)
	require.Equal(t, http.StatusOK, listSecretsRec.Code, listSecretsRec.Body.String())
	assert.NotContains(t, listSecretsRec.Body.String(), secretKey)
	assert.NotContains(t, listSecretsRec.Body.String(), sentinelSecret)

	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/secrets/"+secretKey+"?scope=hub", nil).Code)
	assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, srv, admin, http.MethodPut, "/api/v1/secrets/"+secretKey+"?scope=hub", SetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("OVERWRITE_ATTEMPT")),
	}).Code)
	newDesc := "tamper"
	assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, srv, admin, http.MethodPatch, "/api/v1/secrets/"+secretKey+"?scope=hub", PatchSecretRequest{
		Description: &newDesc,
	}).Code)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, admin, http.MethodDelete, "/api/v1/secrets/"+secretKey+"?scope=hub", nil).Code)

	// Generic /api/v1/env endpoints also reject the reserved namespace.
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/env/"+secretKey+"?scope=hub", nil).Code)
	assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, srv, admin, http.MethodPut, "/api/v1/env/"+secretKey+"?scope=hub", map[string]any{"value": "x"}).Code)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, admin, http.MethodDelete, "/api/v1/env/"+secretKey+"?scope=hub", nil).Code)

	// Project-scoped secret endpoint also rejects creating under systemone.instance.*.
	proj := &store.Project{
		ID:      tid("s1-sec-proj"),
		Name:    "Sec Proj",
		Slug:    "s1-sec-proj",
		OwnerID: admin.ID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, proj))
	assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, srv, admin, http.MethodPut, "/api/v1/projects/"+proj.ID+"/secrets/"+secretKey, SetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("PROJ_OVERWRITE")),
	}).Code)

	// Stored internal secret remains untouched after all generic API attempts.
	svAfter, err := localBackend.Get(ctx, secretKey, store.ScopeHub, srv.hubID)
	require.NoError(t, err)
	assert.Equal(t, sentinelSecret, svAfter.Value)

	// Missing/deleted underlying secret classifies as reason="auth" on /probe (brief item 1).
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("provider must not be contacted when credential resolution fails")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(tlsSrv.Close)
	srv.setSystemOneRuntimeOptions(
		systemoneruntime.WithEgressOptionsForTesting(
			testSystemOneEgressOptions(t, tlsSrv)...,
		),
	)
	instRow, err := s.GetSystemOneInstanceByName(ctx, "sec-isolated")
	require.NoError(t, err)
	instRow.Endpoint = tlsSrv.URL
	require.NoError(t, s.UpdateSystemOneInstance(ctx, instRow))

	require.NoError(t, localBackend.Delete(ctx, secretKey, store.ScopeHub, srv.hubID))

	missingProbeRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/sec-isolated/probe", map[string]any{
		"models": true,
	})
	require.Equal(t, http.StatusOK, missingProbeRec.Code, missingProbeRec.Body.String())
	var missingProbe ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(missingProbeRec.Body.Bytes(), &missingProbe))
	assert.Equal(t, systemoneruntime.ClassFailed, missingProbe.Class)
	assert.Equal(t, "auth", missingProbe.Reason)

	// Create a second instance and verify DELETE removes both the DB row and the internal secret.
	delCreateRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "sec-delete-me",
		"adapter": "typesafe",
		"model":   "jev-1.14.0",
		"auth":    map[string]any{"kind": "api_key", "apiKey": sentinelSecret},
	})
	require.Equal(t, http.StatusCreated, delCreateRec.Code, delCreateRec.Body.String())
	var delCreated SystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(delCreateRec.Body.Bytes(), &delCreated))

	delRec := doRequestAsUser(t, srv, admin, http.MethodDelete, systemOneAdminPath+"/sec-delete-me", nil)
	require.Equal(t, http.StatusNoContent, delRec.Code, delRec.Body.String())

	_, err = s.GetSystemOneInstanceByName(ctx, "sec-delete-me")
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = localBackend.Get(ctx, delCreated.Auth.SecretName, store.ScopeHub, srv.hubID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

type faultyDeleteSecretBackend struct {
	secret.SecretBackend
	mu         sync.Mutex
	failDelete bool
	setNames   []string
}

func (b *faultyDeleteSecretBackend) setFailDelete(v bool) {
	b.mu.Lock()
	b.failDelete = v
	b.mu.Unlock()
}

func (b *faultyDeleteSecretBackend) recordedSets() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.setNames))
	copy(out, b.setNames)
	return out
}

func (b *faultyDeleteSecretBackend) Set(ctx context.Context, input *secret.SetSecretInput) (bool, *secret.SecretMeta, error) {
	created, meta, err := b.SecretBackend.Set(ctx, input)
	if err == nil && input != nil {
		b.mu.Lock()
		b.setNames = append(b.setNames, input.Name)
		b.mu.Unlock()
	}
	return created, meta, err
}

func (b *faultyDeleteSecretBackend) Delete(ctx context.Context, name, scope, scopeID string) error {
	b.mu.Lock()
	fail := b.failDelete
	b.mu.Unlock()
	if fail {
		return errors.New("injected secret backend delete error")
	}
	return b.SecretBackend.Delete(ctx, name, scope, scopeID)
}

func TestSystemOneInstances_DeleteSecretFirstAndForgetRuntime(t *testing.T) {
	ctx := context.Background()
	srv, s, localBackend := newSystemOneTestServer(t, true)
	faultySecret := &faultyDeleteSecretBackend{SecretBackend: localBackend}
	srv.SetSecretBackend(faultySecret)

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"jev-1.14.0"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(tlsSrv.Close)

	srv.setSystemOneRuntimeOptions(
		systemoneruntime.WithEgressOptionsForTesting(
			testSystemOneEgressOptions(t, tlsSrv)...,
		),
	)

	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-del-order-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)

	createRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "del-secret-first",
		"adapter": "typesafe",
		"model":   "jev-1.14.0",
		"auth":    map[string]any{"kind": "api_key", "apiKey": "DEL_ORDER_KEY_901"},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var created SystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))

	instRow, err := s.GetSystemOneInstanceByName(ctx, "del-secret-first")
	require.NoError(t, err)
	instRow.Endpoint = tlsSrv.URL
	require.NoError(t, s.UpdateSystemOneInstance(ctx, instRow))

	// Warm the runtime transport cache for this instance before deletion.
	probeRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/del-secret-first/probe", map[string]any{
		"models": true,
	})
	require.Equal(t, http.StatusOK, probeRec.Code, probeRec.Body.String())

	// Secret-delete failure must return 500 and keep the DB row intact so the
	// operator can retry without orphaning the secret.
	faultySecret.setFailDelete(true)
	failDelRec := doRequestAsUser(t, srv, admin, http.MethodDelete, systemOneAdminPath+"/del-secret-first", nil)
	require.Equal(t, http.StatusInternalServerError, failDelRec.Code, failDelRec.Body.String())

	stillThere, err := s.GetSystemOneInstanceByName(ctx, "del-secret-first")
	require.NoError(t, err, "DB row must remain when secret delete fails")
	assert.Equal(t, created.ID, stillThere.ID)
	getRec := doRequestAsUser(t, srv, admin, http.MethodGet, systemOneAdminPath+"/del-secret-first", nil)
	assert.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	_, err = localBackend.Get(ctx, created.Auth.SecretName, store.ScopeHub, srv.hubID)
	require.NoError(t, err, "secret is still present before retry")

	// Retry once secret backend recovers: returns 204 and removes both secret and row.
	faultySecret.setFailDelete(false)
	okDelRec := doRequestAsUser(t, srv, admin, http.MethodDelete, systemOneAdminPath+"/del-secret-first", nil)
	require.Equal(t, http.StatusNoContent, okDelRec.Code, okDelRec.Body.String())

	_, err = localBackend.Get(ctx, created.Auth.SecretName, store.ScopeHub, srv.hubID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetSystemOneInstanceByName(ctx, "del-secret-first")
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Missing secret (ErrNotFound) during delete is treated as success so a
	// partially cleaned-up row can always be removed.
	createMissingRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "del-missing-secret",
		"adapter": "typesafe",
		"model":   "jev-1.14.0",
		"auth":    map[string]any{"kind": "api_key", "apiKey": "DEL_MISSING_KEY_902"},
	})
	require.Equal(t, http.StatusCreated, createMissingRec.Code, createMissingRec.Body.String())
	var createdMissing SystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(createMissingRec.Body.Bytes(), &createdMissing))

	require.NoError(t, localBackend.Delete(ctx, createdMissing.Auth.SecretName, store.ScopeHub, srv.hubID))
	missingDelRec := doRequestAsUser(t, srv, admin, http.MethodDelete, systemOneAdminPath+"/del-missing-secret", nil)
	require.Equal(t, http.StatusNoContent, missingDelRec.Code, missingDelRec.Body.String())
	_, err = s.GetSystemOneInstanceByName(ctx, "del-missing-secret")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

type failingCreateSystemOneInstanceStore struct {
	store.Store
	fault     *storeFaultSwitch
	mu        sync.Mutex
	failedIDs []string
}

func (w *failingCreateSystemOneInstanceStore) CreateSystemOneInstance(ctx context.Context, inst *store.SystemOneInstance) error {
	if !w.fault.Active() {
		return w.Store.CreateSystemOneInstance(ctx, inst)
	}
	w.mu.Lock()
	w.failedIDs = append(w.failedIDs, inst.ID)
	w.mu.Unlock()
	return errors.New("injected CreateSystemOneInstance store failure")
}

func (w *failingCreateSystemOneInstanceStore) lastFailedID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.failedIDs) == 0 {
		return ""
	}
	return w.failedIDs[len(w.failedIDs)-1]
}

func TestSystemOneInstances_CreateStoreFaultRollsBackSecret(t *testing.T) {
	ctx := context.Background()
	srv, s := testServerWithOps(t, nil)
	// Install store fault wrapper immediately after server construction, before
	// any audited setup (store_fault_helpers_test.go).
	faultStore, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *failingCreateSystemOneInstanceStore {
		return &failingCreateSystemOneInstanceStore{Store: inner, fault: f}
	})

	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret-signing-key")
	faultySecret := &faultyDeleteSecretBackend{SecretBackend: localBackend}
	srv.SetSecretBackend(faultySecret)

	var auditBuf syncBuffer
	srv.envSecretLog = slog.New(slog.NewJSONHandler(&auditBuf, nil))

	ops := srv.GetOperationalSettings()
	doc := fmt.Sprintf(`{"overrides":{%q:true}}`, experiments.SystemOne)
	_, err := ops.Update(ctx, "experiments", json.RawMessage(doc), "test", ops.SectionRevision("experiments"), "managed")
	require.NoError(t, err)

	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-rollback-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)

	const sentinelKey = "SENTINEL_ROLLBACK_KEY_778899"

	// Case 1: CreateSystemOneInstance fails -> handler returns 500 and deletes
	// the systemone.instance.<id> secret it wrote beforehand.
	fault.Arm()
	rec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "rollback-ok",
		"adapter": "typesafe",
		"model":   "jev-1.14.0",
		"auth":    map[string]any{"kind": "api_key", "apiKey": sentinelKey},
	})
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), sentinelKey)

	failedID1 := faultStore.lastFailedID()
	require.NotEmpty(t, failedID1, "CreateSystemOneInstance fault must have fired")
	secretName1 := "systemone.instance." + failedID1
	assert.Contains(t, faultySecret.recordedSets(), secretName1, "secret was written prior to store insert")

	_, err = localBackend.Get(ctx, secretName1, store.ScopeHub, srv.hubID)
	assert.ErrorIs(t, err, store.ErrNotFound, "rollback must remove systemone.instance.<id> secret")
	_, err = s.GetSystemOneInstanceByName(ctx, "rollback-ok")
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Case 2: Both CreateSystemOneInstance and secret rollback delete fail ->
	// audit log records instance_id and secret_name without leaking the key.
	faultySecret.setFailDelete(true)
	rec2 := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "rollback-fail-logged",
		"adapter": "typesafe",
		"model":   "jev-1.14.0",
		"auth":    map[string]any{"kind": "api_key", "apiKey": sentinelKey},
	})
	require.Equal(t, http.StatusInternalServerError, rec2.Code, rec2.Body.String())

	failedID2 := faultStore.lastFailedID()
	require.NotEmpty(t, failedID2)
	require.NotEqual(t, failedID1, failedID2)
	secretName2 := "systemone.instance." + failedID2

	logs := auditBuf.String()
	assert.Contains(t, logs, "Failed to roll back System One instance credential after store failure")
	assert.Contains(t, logs, failedID2)
	assert.Contains(t, logs, secretName2)
	assert.NotContains(t, logs, sentinelKey)
}

type bareSystemOneTestAdapter struct {
	name string
}

func (a *bareSystemOneTestAdapter) Name() string    { return a.name }
func (a *bareSystemOneTestAdapter) Version() string { return "0.1.0" }
func (a *bareSystemOneTestAdapter) Capabilities(_ systemoneprovider.InstanceView) (systemoneprovider.Capabilities, error) {
	return systemoneprovider.Capabilities{
		Primitives: map[string]systemoneprovider.PrimitiveLimits{
			spec.PrimitiveBinary: {Supported: true, DistributionCompleteness: systemoneprovider.CompletenessComplete},
		},
		MaxQuestionsPerRequest: 8,
		MaxInputTokens:         4096,
		Truncation:             systemoneprovider.TruncationNever,
		ReturnsDistribution:    true,
		SideEffectFree:         true,
		AuthScheme:             systemoneprovider.AuthBearer,
	}, nil
}
func (a *bareSystemOneTestAdapter) Validate(_ systemoneprovider.InstanceView, _ systemoneprovider.RuleSpec) error {
	return nil
}
func (a *bareSystemOneTestAdapter) Invoke(_ context.Context, _ systemoneprovider.Transport, _ systemoneprovider.InstanceView, _ systemoneprovider.ModelRequest) (systemoneprovider.ModelResult, error) {
	return systemoneprovider.ModelResult{}, nil
}

type emptyDefaultEndpointAdapter struct {
	bareSystemOneTestAdapter
}

func (a *emptyDefaultEndpointAdapter) DefaultEndpoint() string { return "   " }

type validatorErrorTestAdapter struct {
	bareSystemOneTestAdapter
	validateErr error
}

func (a *validatorErrorTestAdapter) DefaultEndpoint() string { return "https://provider.example.com" }

func (a *validatorErrorTestAdapter) ValidateInstance(_ systemoneprovider.InstanceView) error {
	return a.validateErr
}

func TestSystemOneInstances_EndpointDefaulterAndInstanceValidator(t *testing.T) {
	ctx := context.Background()
	srv, s, _ := newSystemOneTestServer(t, true)
	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-defaulter-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)

	// Built-in default registry exposes both typesafe and gemini_decisions.
	names := srv.SystemOneProviderRegistry().Names()
	assert.Contains(t, names, jevwire.AdapterNameTypeSafe)
	assert.Contains(t, names, geminidecisions.AdapterName)

	// 1. gemini_decisions: missing model -> 400 via InstanceValidator; omitted endpoint -> defaults to DefaultEndpoint.
	missingGeminiModel := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "gem-no-model",
		"adapter": geminidecisions.AdapterName,
		"auth":    map[string]any{"kind": "api_key", "apiKey": "SYNTH_GEMINI_KEY_1"},
	})
	require.Equal(t, http.StatusBadRequest, missingGeminiModel.Code, missingGeminiModel.Body.String())
	assert.Contains(t, missingGeminiModel.Body.String(), "instance model is required for gemini_decisions")
	assert.NotContains(t, missingGeminiModel.Body.String(), "SYNTH_GEMINI_KEY_1")

	gemCreatedRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "gem-default-ep",
		"adapter": geminidecisions.AdapterName,
		"model":   "gemini-decision-1",
		"auth":    map[string]any{"kind": "api_key", "apiKey": "SYNTH_GEMINI_KEY_2"},
	})
	require.Equal(t, http.StatusCreated, gemCreatedRec.Code, gemCreatedRec.Body.String())
	assert.NotContains(t, gemCreatedRec.Body.String(), "SYNTH_GEMINI_KEY_2")
	var gemCreated SystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(gemCreatedRec.Body.Bytes(), &gemCreated))
	assert.Equal(t, geminidecisions.DefaultEndpoint, gemCreated.Endpoint)
	assert.Equal(t, "gemini-decision-1", gemCreated.Model)

	// Custom auth.headerName is still rejected on gemini_decisions (placement comes only from adapter declaration).
	badHdrRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "gem-custom-hdr",
		"adapter": geminidecisions.AdapterName,
		"model":   "gemini-decision-1",
		"auth":    map[string]any{"kind": "api_key", "apiKey": "SYNTH_GEMINI_KEY_3", "headerName": "x-goog-api-key"},
	})
	require.Equal(t, http.StatusBadRequest, badHdrRec.Code, badHdrRec.Body.String())
	assert.Contains(t, badHdrRec.Body.String(), "custom auth headerName is not supported")

	// 2. typesafe regression: missing model -> 400 preserving exact message; omitted endpoint -> https://api.typesafe.ai.
	missingTSModel := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "ts-no-model",
		"adapter": jevwire.AdapterNameTypeSafe,
		"auth":    map[string]any{"kind": "api_key", "apiKey": "SYNTH_TS_KEY_1"},
	})
	require.Equal(t, http.StatusBadRequest, missingTSModel.Code, missingTSModel.Body.String())
	assert.Contains(t, missingTSModel.Body.String(), `model is required for adapter \"typesafe\"`)

	tsCreatedRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "ts-default-ep",
		"adapter": jevwire.AdapterNameTypeSafe,
		"model":   "jev-1.14.0",
		"auth":    map[string]any{"kind": "api_key", "apiKey": "SYNTH_TS_KEY_2"},
	})
	require.Equal(t, http.StatusCreated, tsCreatedRec.Code, tsCreatedRec.Body.String())
	var tsCreated SystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(tsCreatedRec.Body.Bytes(), &tsCreated))
	assert.Equal(t, jevwire.DefaultTypeSafeEndpoint, tsCreated.Endpoint)

	// 3. Adapter without EndpointDefaulter (or returning empty string) requires explicit endpoint (400 if omitted)
	// and allows empty model when InstanceValidator is not implemented.
	customSrv, customStore, _ := newSystemOneTestServer(t, true)
	customAdmin, err := customStore.GetUser(ctx, hubConfigTokenUser(t, customStore, "s1-custom-reg-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)

	customReg := systemoneprovider.NewRegistry()
	require.NoError(t, customReg.Register(&bareSystemOneTestAdapter{name: "no-defaulter"}))
	require.NoError(t, customReg.Register(&emptyDefaultEndpointAdapter{bareSystemOneTestAdapter: bareSystemOneTestAdapter{name: "empty-defaulter"}}))
	oversizeMsg := strings.Repeat("m", 255) + "é" + strings.Repeat("TAIL_NEVER_ECHOED", 20)
	require.NoError(t, customReg.Register(&validatorErrorTestAdapter{
		bareSystemOneTestAdapter: bareSystemOneTestAdapter{name: "long-err-adapter"},
		validateErr: &systemoneprovider.ProviderError{
			Reason:  systemoneprovider.FailInvalidRequest,
			Message: oversizeMsg,
		},
	}))
	require.NoError(t, customReg.Register(&validatorErrorTestAdapter{
		bareSystemOneTestAdapter: bareSystemOneTestAdapter{name: "ctrl-err-adapter"},
		validateErr: &systemoneprovider.ProviderError{
			Reason:  systemoneprovider.FailInvalidRequest,
			Message: strings.Repeat("c", 270) + "\ninjected-line",
		},
	}))
	customSrv.mu.Lock()
	customSrv.systemOneRegistry = customReg
	customSrv.rebuildSystemOneRuntimeLocked()
	customSrv.mu.Unlock()

	for _, adName := range []string{"no-defaulter", "empty-defaulter"} {
		noEpRec := doRequestAsUser(t, customSrv, customAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
			"name":    adName + "-missing-ep",
			"adapter": adName,
			"auth":    map[string]any{"kind": "api_key", "apiKey": "SYNTH_CUSTOM_KEY"},
		})
		require.Equal(t, http.StatusBadRequest, noEpRec.Code, "adapter=%s body=%s", adName, noEpRec.Body.String())
		assert.Contains(t, noEpRec.Body.String(), "endpoint is required")
	}

	okCustomRec := doRequestAsUser(t, customSrv, customAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":     "no-defaulter-explicit",
		"adapter":  "no-defaulter",
		"endpoint": "https://provider.example.com",
		"auth":     map[string]any{"kind": "api_key", "apiKey": "SYNTH_CUSTOM_KEY"},
	})
	require.Equal(t, http.StatusCreated, okCustomRec.Code, okCustomRec.Body.String())
	var okCustom SystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(okCustomRec.Body.Bytes(), &okCustom))
	assert.Equal(t, "https://provider.example.com", okCustom.Endpoint)
	assert.Empty(t, okCustom.Model)

	// 4. Adapter config error message is capped at 256 bytes on a UTF-8 boundary and control characters fall back.
	longErrRec := doRequestAsUser(t, customSrv, customAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "long-err-inst",
		"adapter": "long-err-adapter",
		"auth":    map[string]any{"kind": "api_key", "apiKey": "SYNTH_CUSTOM_KEY"},
	})
	require.Equal(t, http.StatusBadRequest, longErrRec.Code, longErrRec.Body.String())
	var longErrEnv ErrorResponse
	require.NoError(t, json.Unmarshal(longErrRec.Body.Bytes(), &longErrEnv))
	assert.LessOrEqual(t, len(longErrEnv.Error.Message), maxSystemOneAdapterErrorMessageBytes)
	assert.Equal(t, strings.Repeat("m", 255), longErrEnv.Error.Message, "2-byte UTF-8 rune straddling byte 256 must not be split")
	assert.NotContains(t, longErrRec.Body.String(), "TAIL_NEVER_ECHOED")

	ctrlErrRec := doRequestAsUser(t, customSrv, customAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "ctrl-err-inst",
		"adapter": "ctrl-err-adapter",
		"auth":    map[string]any{"kind": "api_key", "apiKey": "SYNTH_CUSTOM_KEY"},
	})
	require.Equal(t, http.StatusBadRequest, ctrlErrRec.Code, ctrlErrRec.Body.String())
	var ctrlErrEnv ErrorResponse
	require.NoError(t, json.Unmarshal(ctrlErrRec.Body.Bytes(), &ctrlErrEnv))
	assert.Equal(t, "adapter rejected instance configuration", ctrlErrEnv.Error.Message)

	assert.Equal(t, strings.Repeat("z", 256), truncateUTF8Bytes(strings.Repeat("z", 400), maxSystemOneAdapterErrorMessageBytes))
	assert.Equal(t, strings.Repeat("z", 255), truncateUTF8Bytes(strings.Repeat("z", 255)+"é", maxSystemOneAdapterErrorMessageBytes))
}

func TestSystemOneInstances_GeminiDecisionsVerticalProbeAndTypeSafeRegression(t *testing.T) {
	// Synthetic from docs (not recorded from the live API): ai.google.dev/gemini-api/docs/decisions-api & api/decisions.
	ctx := context.Background()
	srv, s, localBackend := newSystemOneTestServer(t, true)
	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-gemini-vert-admin", store.SystemRoleHubAdmin))
	require.NoError(t, err)

	var auditBuf syncBuffer
	srv.envSecretLog = slog.New(slog.NewJSONHandler(&auditBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var runtimeLogBuf syncBuffer
	origDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&runtimeLogBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(origDefault) })

	type wireCapture struct {
		method string
		path   string
		query  string
		goog   string
		auth   string
		hdrs   http.Header
		body   string
	}

	var (
		mu             sync.Mutex
		geminiStatus   = http.StatusOK
		geminiRespBody string
		captures       []wireCapture
	)

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw bytes.Buffer
		if r.Body != nil {
			_, _ = raw.ReadFrom(r.Body)
			_ = r.Body.Close()
		}
		mu.Lock()
		captures = append(captures, wireCapture{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			goog:   r.Header.Get("x-goog-api-key"),
			auth:   r.Header.Get("Authorization"),
			hdrs:   r.Header.Clone(),
			body:   raw.String(),
		})
		st := geminiStatus
		gBody := geminiRespBody
		mu.Unlock()

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1beta/decisions":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(st)
			_, _ = w.Write([]byte(gBody))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"jev-1.14.0","description":"Latest","release_date":"2026-06-01"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/systemone":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Typesafe-Request-Id", "req-ts-reg-1")
			_, _ = w.Write([]byte(`{
				"model": "jev-1.14.0",
				"answers": {
					"is_damaged": {"type": "noul", "noul": 0.92},
					"department": {
						"type": "choice",
						"choice": "support",
						"confidence": 0.88,
						"probabilities": {"billing": 0.04, "other": 0.02, "sales": 0.03, "support": 0.91}
					}
				},
				"usage": {"input_tokens": 31, "output_tokens": 4}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(tlsSrv.Close)

	srv.SetSystemOneRuntimeOptionsForTesting(
		t,
		systemoneruntime.WithEgressOptionsForTesting(
			testSystemOneEgressOptions(t, tlsSrv)...,
		),
	)

	const (
		sentinelGeminiKey   = "SENTINEL_GEMINI_HUB_SECRET_KEY_8414_ALPHA"
		sentinelTypeSafeKey = "SENTINEL_TYPESAFE_HUB_SECRET_KEY_8414_BETA"
		sentinelProbeText   = "SENTINEL_GEMINI_PROBE_SAMPLE_CRACKED_SCREEN_8414"
		sentinelSafetyErr   = "CANARY_GEMINI_400_SAFETY_ERROR_BODY_8414"
	)

	// 1. Create gemini_decisions instance; verify internal secret storage and non-disclosure.
	gemCreateRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":     "gemini-main",
		"adapter":  geminidecisions.AdapterName,
		"model":    "gemini-decision-1",
		"endpoint": tlsSrv.URL,
		"auth":     map[string]any{"kind": "api_key", "apiKey": sentinelGeminiKey},
	})
	require.Equal(t, http.StatusCreated, gemCreateRec.Code, gemCreateRec.Body.String())
	assert.NotContains(t, gemCreateRec.Body.String(), sentinelGeminiKey)

	var gemInst SystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(gemCreateRec.Body.Bytes(), &gemInst))
	assert.Equal(t, geminidecisions.AdapterName, gemInst.Adapter)
	assert.Equal(t, "gemini-decision-1", gemInst.Model)
	require.Equal(t, "systemone.instance."+gemInst.ID, gemInst.Auth.SecretName)

	sv, err := localBackend.Get(ctx, gemInst.Auth.SecretName, store.ScopeHub, srv.hubID)
	require.NoError(t, err)
	assert.Equal(t, store.SecretTypeInternal, sv.SecretType)
	assert.Equal(t, sentinelGeminiKey, sv.Value)

	// 2. Models probe on gemini_decisions returns 400 unsupported without making any network call.
	gemModelsRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/gemini-main/probe", map[string]any{
		"models": true,
	})
	require.Equal(t, http.StatusBadRequest, gemModelsRec.Code, gemModelsRec.Body.String())
	assert.Contains(t, gemModelsRec.Body.String(), "adapter does not support model listing")
	mu.Lock()
	assert.Empty(t, captures, "unsupported models probe must not contact provider")
	mu.Unlock()

	mixedProbeSpec := map[string]any{
		"version": 1,
		"questions": []map[string]any{
			{
				"id":           "is_damaged",
				"type":         "binary",
				"instructions": "Does `probe.text` describe physical hardware damage?",
				"criteria": map[string]any{
					"true":  "Physical damage is described.",
					"false": "No physical damage is described.",
				},
			},
			{
				"id":           "department",
				"type":         "choice",
				"instructions": "Route `probe.text` to the owning department.",
				"options": []map[string]any{
					{"id": "billing", "description": "Billing inquiries"},
					{"id": "sales", "description": "Sales inquiries"},
					{"id": "support", "description": "Technical support"},
					{"id": "other", "description": "Cannot determine", "abstain": true},
				},
			},
		},
		"output": map[string]any{
			"question": "department",
		},
	}

	// 3a. Gemini probe with documented 0.999 choice sum and measured usage.
	mu.Lock()
	geminiStatus = http.StatusOK
	geminiRespBody = `{
		"answers": {
			"is_damaged": {"type": "noul", "noul": 0.99, "confidence": 0.99},
			"department": {
				"type": "choice",
				"choice": "support",
				"probabilities": {"billing": 0.004, "other": 0.002, "sales": 0.003, "support": 0.99},
				"confidence": 0.99
			}
		},
		"usage": {"total_input_tokens": 58, "total_output_tokens": 0}
	}`
	mu.Unlock()

	probeOKRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/gemini-main/probe", map[string]any{
		"spec":   mixedProbeSpec,
		"probe":  map[string]any{"text": sentinelProbeText},
		"ruleId": "pv3-g2-hub-probe",
	})
	require.Equal(t, http.StatusOK, probeOKRec.Code, probeOKRec.Body.String())
	assert.NotContains(t, probeOKRec.Body.String(), sentinelGeminiKey)
	assert.NotContains(t, probeOKRec.Body.String(), sentinelProbeText)

	var probeOK ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(probeOKRec.Body.Bytes(), &probeOK))
	assert.Equal(t, systemoneruntime.ClassSuccess, probeOK.Class)
	require.NotNil(t, probeOK.Decision)
	assert.Equal(t, spec.KindJudgmentChoice, probeOK.Decision.Kind)
	require.NotNil(t, probeOK.Decision.String)
	assert.Equal(t, "support", *probeOK.Decision.String)

	require.NotNil(t, probeOK.Evaluation)
	assert.Equal(t, "gemini-decision-1", probeOK.Evaluation.ConfiguredModel)
	assert.Empty(t, probeOK.Evaluation.ResolvedModel)
	assert.Empty(t, probeOK.Evaluation.EffectiveResolvedModel())
	assert.True(t, probeOK.Evaluation.HasModelUnreported())
	assert.False(t, probeOK.Evaluation.HasUsageUnreported())
	assert.Equal(t, []string{systemoneprovider.NoteModelUnreported}, probeOK.Evaluation.Notes)
	require.NotNil(t, probeOK.Evaluation.Usage)
	assert.Equal(t, systemoneprovider.Usage{InputTokens: 58, OutputTokens: 0}, *probeOK.Evaluation.Usage)
	require.Len(t, probeOK.Evaluation.Attempts, 1)
	assert.Equal(t, geminidecisions.AdapterName, probeOK.Evaluation.Attempts[0].Adapter)
	assert.Equal(t, "gemini-decision-1", probeOK.Evaluation.Attempts[0].ConfiguredModel)
	assert.Empty(t, probeOK.Evaluation.Attempts[0].ReportedModel)
	assert.Equal(t, []string{systemoneprovider.NoteClamped001To099}, probeOK.Evaluation.Answers["is_damaged"].Notes)
	assert.Equal(t, []string{systemoneprovider.NoteOptionsSortedByProvider}, probeOK.Evaluation.Answers["department"].Notes)
	assert.Contains(t, probeOK.Evaluation.Answers["department"].NativeMetrics, geminidecisions.NativeMetricConfidence)

	mu.Lock()
	require.Len(t, captures, 1)
	cap1 := captures[0]
	mu.Unlock()
	assert.Equal(t, http.MethodPost, cap1.method)
	assert.Equal(t, "/v1beta/decisions", cap1.path)
	assert.Empty(t, cap1.query)
	assert.Equal(t, sentinelGeminiKey, cap1.goog)
	assert.Empty(t, cap1.auth, "Authorization header must never be sent for gemini_decisions")
	for hdr, vals := range cap1.hdrs {
		if strings.EqualFold(hdr, "x-goog-api-key") {
			continue
		}
		for _, v := range vals {
			assert.NotContains(t, v, sentinelGeminiKey, "secret leaked in header %s", hdr)
		}
	}
	assert.NotContains(t, cap1.body, sentinelGeminiKey)

	// 3b. Gemini probe with missing usage -> usage_unreported note and nil Usage (never 0 tokens as measured).
	mu.Lock()
	geminiStatus = http.StatusOK
	geminiRespBody = `{
		"answers": {
			"is_damaged": {"type": "noul", "noul": 0.15, "confidence": 0.85},
			"department": {
				"type": "choice",
				"choice": "billing",
				"probabilities": {"billing": 0.90, "other": 0.02, "sales": 0.05, "support": 0.03},
				"confidence": 0.90
			}
		}
	}`
	mu.Unlock()

	probeNoUsageRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/gemini-main/probe", map[string]any{
		"spec":  mixedProbeSpec,
		"probe": map[string]any{"text": sentinelProbeText},
	})
	require.Equal(t, http.StatusOK, probeNoUsageRec.Code, probeNoUsageRec.Body.String())
	assert.NotContains(t, probeNoUsageRec.Body.String(), `"usage"`, "missing usage must never serialize zero tokens as measured usage")

	var probeNoUsage ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(probeNoUsageRec.Body.Bytes(), &probeNoUsage))
	assert.Equal(t, systemoneruntime.ClassSuccess, probeNoUsage.Class)
	require.NotNil(t, probeNoUsage.Evaluation)
	assert.Nil(t, probeNoUsage.Evaluation.Usage)
	assert.True(t, probeNoUsage.Evaluation.HasUsageUnreported())
	assert.ElementsMatch(t, []string{systemoneprovider.NoteModelUnreported, systemoneprovider.NoteUsageUnreported}, probeNoUsage.Evaluation.Notes)

	// 3c. Gemini HTTP 400 safety block -> ClassFailed, reason="content_blocked", error body never leaked.
	mu.Lock()
	geminiStatus = http.StatusBadRequest
	geminiRespBody = fmt.Sprintf(`{"error":{"code":"safety","message":%q}}`, sentinelSafetyErr)
	mu.Unlock()

	probeBlockedRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/gemini-main/probe", map[string]any{
		"spec":  mixedProbeSpec,
		"probe": map[string]any{"text": sentinelProbeText},
	})
	require.Equal(t, http.StatusOK, probeBlockedRec.Code, probeBlockedRec.Body.String())
	assert.NotContains(t, probeBlockedRec.Body.String(), sentinelSafetyErr)
	assert.NotContains(t, probeBlockedRec.Body.String(), sentinelGeminiKey)

	var probeBlocked ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(probeBlockedRec.Body.Bytes(), &probeBlocked))
	assert.Equal(t, systemoneruntime.ClassFailed, probeBlocked.Class)
	assert.Equal(t, string(systemoneprovider.FailContentBlocked), probeBlocked.Reason)
	assert.Nil(t, probeBlocked.Decision)
	require.NotNil(t, probeBlocked.Evaluation)
	assert.Equal(t, "gemini-decision-1", probeBlocked.Evaluation.ConfiguredModel)
	assert.Empty(t, probeBlocked.Evaluation.ResolvedModel)
	assert.Equal(t, []string{systemoneprovider.NoteModelUnreported}, probeBlocked.Evaluation.Notes)
	assert.Nil(t, probeBlocked.Evaluation.Usage)
	require.Len(t, probeBlocked.Evaluation.Attempts, 1)
	assert.Equal(t, "4xx", probeBlocked.Evaluation.Attempts[0].HTTPStatusClass)

	// 4. TypeSafe regression on the same Hub server: still uses Authorization: Bearer and never x-goog-api-key.
	tsCreateRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":     "typesafe-reg",
		"adapter":  jevwire.AdapterNameTypeSafe,
		"model":    "jev-1.14.0",
		"endpoint": tlsSrv.URL,
		"auth":     map[string]any{"kind": "api_key", "apiKey": sentinelTypeSafeKey},
	})
	require.Equal(t, http.StatusCreated, tsCreateRec.Code, tsCreateRec.Body.String())
	assert.NotContains(t, tsCreateRec.Body.String(), sentinelTypeSafeKey)

	tsModelsRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/typesafe-reg/probe", map[string]any{
		"models": true,
	})
	require.Equal(t, http.StatusOK, tsModelsRec.Code, tsModelsRec.Body.String())

	tsProbeRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/typesafe-reg/probe", map[string]any{
		"spec":  mixedProbeSpec,
		"probe": map[string]any{"text": sentinelProbeText},
	})
	require.Equal(t, http.StatusOK, tsProbeRec.Code, tsProbeRec.Body.String())
	var tsProbe ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(tsProbeRec.Body.Bytes(), &tsProbe))
	assert.Equal(t, systemoneruntime.ClassSuccess, tsProbe.Class)
	require.NotNil(t, tsProbe.Evaluation)
	assert.Equal(t, "jev-1.14.0", tsProbe.Evaluation.ConfiguredModel)
	assert.Equal(t, "jev-1.14.0", tsProbe.Evaluation.ResolvedModel)
	require.NotNil(t, tsProbe.Evaluation.Usage)
	assert.Equal(t, systemoneprovider.Usage{InputTokens: 31, OutputTokens: 4}, *tsProbe.Evaluation.Usage)

	mu.Lock()
	require.Len(t, captures, 5)
	tsModelsCap := captures[3]
	tsProbeCap := captures[4]
	mu.Unlock()
	for _, tsCap := range []wireCapture{tsModelsCap, tsProbeCap} {
		assert.Equal(t, "Bearer "+sentinelTypeSafeKey, tsCap.auth)
		assert.Empty(t, tsCap.goog, "x-goog-api-key must never be sent for typesafe")
		assert.NotContains(t, tsCap.body, sentinelTypeSafeKey)
	}

	allLogs := auditBuf.String() + "\n" + runtimeLogBuf.String()
	for _, forbidden := range []string{
		sentinelGeminiKey,
		sentinelTypeSafeKey,
		sentinelProbeText,
		sentinelSafetyErr,
	} {
		assert.NotContains(t, allLogs, forbidden, "sensitive value leaked into logs")
	}
}

func TestSystemOneInstances_GeminiADCVerticalCreateGetListDeleteProbeAndRedaction(t *testing.T) {
	ctx := context.Background()
	srv, s, localBackend := newSystemOneTestServer(t, true)
	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-gemini-adc-admin", store.SystemRoleHubAdmin))
	require.NoError(t, err)

	var auditBuf, runtimeLogBuf bytes.Buffer
	srv.envSecretLog = slog.New(slog.NewJSONHandler(&auditBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	const (
		canaryADCToken    = "fake-adc-token-CANARY-hub-vertical"
		canaryAPIKeyInput = "AIzaSy-should-be-refused-CANARY"
		quotaProject      = "paid-gemini-proj-77"
		saEmail           = "gemini-decisions-sa@paid-gemini-proj-77.iam.gserviceaccount.com"
		testADCScope      = "https://www.googleapis.com/auth/synthetic.test.scope"
	)

	type adcWireCapture struct {
		auth   string
		goog   string
		qp     string
		body   string
		method string
		path   string
	}
	var (
		mu       sync.Mutex
		captures []adcWireCapture
		nextCode = http.StatusOK
		nextBody = `{
			"answers": {
				"is_damaged": {"type": "noul", "noul": 0.99, "confidence": 0.99}
			},
			"usage": {"total_input_tokens": 42, "total_output_tokens": 0}
		}`
		currentADCType = SystemOneADCTypeServiceAccount
		currentSAEmail = saEmail
		tokenErr       error
	)

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw bytes.Buffer
		_, _ = raw.ReadFrom(r.Body)
		_ = r.Body.Close()

		mu.Lock()
		captures = append(captures, adcWireCapture{
			auth:   r.Header.Get("Authorization"),
			goog:   r.Header.Get("x-goog-api-key"),
			qp:     r.Header.Get("X-Goog-User-Project"),
			body:   raw.String(),
			method: r.Method,
			path:   r.URL.Path,
		})
		st := nextCode
		body := nextBody
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		_, _ = w.Write([]byte(body))
	}))
	defer tlsSrv.Close()

	var adcProviderCalls atomic.Int32
	installFakeADC := func() {
		srv.SetSystemOneADCForTesting(t, func(_ context.Context, _ []string) (*SystemOneADCCredentials, error) {
			adcProviderCalls.Add(1)
			mu.Lock()
			ct := currentADCType
			em := currentSAEmail
			mu.Unlock()
			return &SystemOneADCCredentials{
				CredentialType:      ct,
				ServiceAccountEmail: em,
				TokenSource: funcTokenSource(func() (*oauth2.Token, error) {
					mu.Lock()
					tErr := tokenErr
					mu.Unlock()
					if tErr != nil {
						return nil, tErr
					}
					return &oauth2.Token{
						AccessToken: canaryADCToken,
						Expiry:      time.Now().Add(30 * time.Minute),
					}, nil
				}),
			}, nil
		})
	}
	installFakeADC()

	srv.SetSystemOneRuntimeOptionsForTesting(t,
		systemoneruntime.WithEgressOptionsForTesting(
			testSystemOneEgressOptions(t, tlsSrv)...,
		),
	)
	reg := srv.SystemOneProviderRegistry()
	srv.mu.Lock()
	srv.systemOneRuntime = systemoneruntime.New(
		systemoneruntime.Config{
			Registry:    reg,
			Instances:   &hubSystemOneInstanceSource{srv: srv},
			Credentials: (&hubSystemOneCredentialSource{srv: srv}).APIKeyToken,
			TokenSource: (&hubSystemOneCredentialSource{srv: srv}).GoogleAccessToken,
			MaxEvalTime: resolveSystemOneMaxEvalTime(srv.config),
			Logger:      slog.New(slog.NewJSONHandler(&runtimeLogBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		},
		srv.systemOneRuntimeOpts...,
	)
	srv.mu.Unlock()

	// 1. Create rejections (400): apiKey refused, bad scopes refused, bad quota_project refused, headerName/audience refused.
	badCreates := []struct {
		name string
		body map[string]any
		want string
	}{
		{
			name: "top_level_apiKey_refused",
			body: map[string]any{
				"name":    "gem-adc-bad-1",
				"adapter": geminidecisions.AdapterName,
				"model":   "gemini-decision-1",
				"apiKey":  canaryAPIKeyInput,
				"auth":    map[string]any{"kind": "google_access_token"},
			},
			want: "apiKey must not be provided",
		},
		{
			name: "nested_auth_apiKey_refused",
			body: map[string]any{
				"name":    "gem-adc-bad-2",
				"adapter": geminidecisions.AdapterName,
				"model":   "gemini-decision-1",
				"auth":    map[string]any{"kind": "google_access_token", "apiKey": canaryAPIKeyInput},
			},
			want: "apiKey must not be provided",
		},
		{
			name: "missing_scopes_refused",
			body: map[string]any{
				"name":    "gem-adc-bad-missing-scopes",
				"adapter": geminidecisions.AdapterName,
				"model":   "gemini-decision-1",
				"auth":    map[string]any{"kind": "google_access_token"},
			},
			want: "auth.scopes is required",
		},
		{
			name: "empty_scopes_slice_refused",
			body: map[string]any{
				"name":    "gem-adc-bad-3",
				"adapter": geminidecisions.AdapterName,
				"model":   "gemini-decision-1",
				"auth":    map[string]any{"kind": "google_access_token", "scopes": []string{}},
			},
			want: "auth.scopes is required",
		},
		{
			name: "bad_scope_prefix_refused",
			body: map[string]any{
				"name":    "gem-adc-bad-4",
				"adapter": geminidecisions.AdapterName,
				"model":   "gemini-decision-1",
				"auth":    map[string]any{"kind": "google_access_token", "scopes": []string{"https://evil.example/scope"}},
			},
			want: "https://www.googleapis.com/auth/",
		},
		{
			name: "bare_scope_prefix_refused",
			body: map[string]any{
				"name":    "gem-adc-bad-5",
				"adapter": geminidecisions.AdapterName,
				"model":   "gemini-decision-1",
				"auth":    map[string]any{"kind": "google_access_token", "scopes": []string{"https://www.googleapis.com/auth/"}},
			},
			want: "https://www.googleapis.com/auth/",
		},
		{
			name: "bad_quota_project_refused",
			body: map[string]any{
				"name":    "gem-adc-bad-6",
				"adapter": geminidecisions.AdapterName,
				"model":   "gemini-decision-1",
				"auth":    map[string]any{"kind": "google_access_token", "scopes": []string{testADCScope}},
				"options": map[string]string{"quota_project": "INVALID_UPPER"},
			},
			want: "quota_project",
		},
		{
			name: "headerName_refused",
			body: map[string]any{
				"name":    "gem-adc-bad-7",
				"adapter": geminidecisions.AdapterName,
				"model":   "gemini-decision-1",
				"auth":    map[string]any{"kind": "google_access_token", "scopes": []string{testADCScope}, "headerName": "Authorization"},
			},
			want: "headerName is not supported",
		},
		{
			name: "audience_refused",
			body: map[string]any{
				"name":    "gem-adc-bad-8",
				"adapter": geminidecisions.AdapterName,
				"model":   "gemini-decision-1",
				"auth":    map[string]any{"kind": "google_access_token", "scopes": []string{testADCScope}, "audience": "https://generativelanguage.googleapis.com"},
			},
			want: "audience is not supported",
		},
		{
			name: "quota_project_on_api_key_refused",
			body: map[string]any{
				"name":    "gem-key-bad-qp",
				"adapter": geminidecisions.AdapterName,
				"model":   "gemini-decision-1",
				"auth":    map[string]any{"kind": "api_key", "apiKey": "AIzaSy-synthetic"},
				"options": map[string]string{"quota_project": quotaProject},
			},
			want: "quota_project is only supported",
		},
	}
	for _, tc := range badCreates {
		rec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, tc.body)
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", tc.name, rec.Body.String())
		assert.Contains(t, rec.Body.String(), tc.want, tc.name)
		assert.NotContains(t, rec.Body.String(), canaryAPIKeyInput)
	}

	// 2. hub O1: create/get/list perform ZERO ADC discovery or metadata I/O on cold cache.
	//    Before any probe/evaluation, create/get/list render "unresolved (run probe)" with 0 provider calls.
	adcProviderCalls.Store(0)
	mu.Lock()
	currentADCType = SystemOneADCTypeServiceAccount
	currentSAEmail = saEmail
	mu.Unlock()
	installFakeADC()

	// 3. Create ADC instance: writes zero secrets, makes 0 ADC calls, stores ONLY options["quota_project"] and auth.{kind,scopes},
	//    and renders "unresolved (run probe)" until probe populates the in-memory cache.
	createRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "gemini-adc-main",
		"adapter": geminidecisions.AdapterName,
		"model":   "gemini-decision-1",
		"auth":    map[string]any{"kind": "google_access_token", "scopes": []string{testADCScope}},
		"options": map[string]string{"quota_project": quotaProject},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	assert.NotContains(t, createRec.Body.String(), canaryADCToken)
	assert.Zero(t, adcProviderCalls.Load(), "create must perform 0 ADC discovery or metadata calls")

	var created SystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))
	assert.Equal(t, "google_access_token", created.Auth.Kind)
	assert.Empty(t, created.Auth.SecretName)
	assert.Equal(t, []string{testADCScope}, created.Auth.Scopes)
	assert.Equal(t, "explicit: "+quotaProject, created.Auth.QuotaProject)
	assert.Equal(t, SystemOneADCUnresolved, created.Auth.CredentialType)
	assert.Empty(t, created.Auth.ServiceAccountEmail)
	assert.Equal(t, map[string]string{"quota_project": quotaProject}, created.Options)

	// Verify store round-trip persists nothing new (no credential_type, service_account_email, or duplicate quota_project).
	storedMain, err := s.GetSystemOneInstanceByName(ctx, "gemini-adc-main")
	require.NoError(t, err)
	assert.Equal(t, store.SystemOneInstanceAuth{
		Kind:   store.SystemOneAuthKindGoogleAccessToken,
		Scopes: []string{testADCScope},
	}, storedMain.Auth)
	assert.Equal(t, map[string]string{"quota_project": quotaProject}, storedMain.Options)

	secretsList, err := localBackend.List(ctx, secret.Filter{Scope: store.ScopeHub, ScopeID: "test-hub-id"})
	require.NoError(t, err)
	assert.Empty(t, secretsList, "creating a google_access_token instance must store zero secrets")

	// Cold GET /instances/{name} and GET /instances make 0 ADC calls and show "unresolved (run probe)".
	getRec := doRequestAsUser(t, srv, admin, http.MethodGet, systemOneAdminPath+"/gemini-adc-main", nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	assert.NotContains(t, getRec.Body.String(), canaryADCToken)
	var gotResp SystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &gotResp))
	assert.Equal(t, created.Auth, gotResp.Auth)
	assert.Zero(t, adcProviderCalls.Load(), "GET must perform 0 ADC discovery or metadata calls")

	listRec := doRequestAsUser(t, srv, admin, http.MethodGet, systemOneAdminPath, nil)
	require.Equal(t, http.StatusOK, listRec.Code, listRec.Body.String())
	assert.NotContains(t, listRec.Body.String(), canaryADCToken)
	var listResp ListSystemOneInstancesResponse
	require.NoError(t, json.Unmarshal(listRec.Body.Bytes(), &listResp))
	require.Len(t, listResp.Instances, 1)
	assert.Equal(t, created.Auth, listResp.Instances[0].Auth)
	assert.Zero(t, adcProviderCalls.Load(), "LIST must perform 0 ADC discovery or metadata calls")

	// 4. Probe success: resolves ADC once (1 provider call), sends Authorization: Bearer + X-Goog-User-Project on wire,
	//    no x-goog-api-key, populates provenance, and subsequent GET/LIST show resolved metadata with 0 additional ADC calls.
	binSpec := map[string]any{
		"version": 1,
		"form":    "portable",
		"questions": []map[string]any{
			{"id": "is_damaged", "type": "binary", "instructions": "Does `probe.text` describe physical damage?"},
		},
		"output": map[string]any{"question": "is_damaged"},
	}
	probeRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/gemini-adc-main/probe", map[string]any{
		"spec":  binSpec,
		"probe": map[string]any{"text": "Synthetic cracked glass report."},
	})
	require.Equal(t, http.StatusOK, probeRec.Code, probeRec.Body.String())
	assert.NotContains(t, probeRec.Body.String(), canaryADCToken)
	assert.Equal(t, int32(1), adcProviderCalls.Load(), "probe must resolve ADC exactly once")

	// Subsequent GET and LIST read the populated in-memory cache with 0 additional ADC discovery calls.
	getAfterProbe := doRequestAsUser(t, srv, admin, http.MethodGet, systemOneAdminPath+"/gemini-adc-main", nil)
	require.Equal(t, http.StatusOK, getAfterProbe.Code)
	var gotAfterProbeResp SystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(getAfterProbe.Body.Bytes(), &gotAfterProbeResp))
	assert.Equal(t, SystemOneADCTypeServiceAccount, gotAfterProbeResp.Auth.CredentialType)
	assert.Equal(t, saEmail, gotAfterProbeResp.Auth.ServiceAccountEmail)
	assert.Equal(t, int32(1), adcProviderCalls.Load(), "warm GET must perform 0 additional ADC discovery calls")

	listAfterProbe := doRequestAsUser(t, srv, admin, http.MethodGet, systemOneAdminPath, nil)
	require.Equal(t, http.StatusOK, listAfterProbe.Code)
	var listAfterProbeResp ListSystemOneInstancesResponse
	require.NoError(t, json.Unmarshal(listAfterProbe.Body.Bytes(), &listAfterProbeResp))
	require.Len(t, listAfterProbeResp.Instances, 1)
	assert.Equal(t, SystemOneADCTypeServiceAccount, listAfterProbeResp.Instances[0].Auth.CredentialType)
	assert.Equal(t, saEmail, listAfterProbeResp.Instances[0].Auth.ServiceAccountEmail)
	assert.Equal(t, int32(1), adcProviderCalls.Load(), "warm LIST must perform 0 additional ADC discovery calls")

	var probeOK ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(probeRec.Body.Bytes(), &probeOK))
	assert.Equal(t, systemoneruntime.ClassSuccess, probeOK.Class)
	require.NotNil(t, probeOK.Evaluation)
	require.NotNil(t, probeOK.Evaluation.Auth)
	assert.Equal(t, &systemoneruntime.AuthProvenance{
		AuthKind:            "google_access_token",
		Scopes:              []string{testADCScope},
		QuotaProject:        "explicit: " + quotaProject,
		CredentialType:      SystemOneADCTypeServiceAccount,
		ServiceAccountEmail: saEmail,
	}, probeOK.Evaluation.Auth)
	require.Len(t, probeOK.Evaluation.Attempts, 1)
	assert.Equal(t, 1, probeOK.Evaluation.Attempts[0].Requests)

	mu.Lock()
	require.Len(t, captures, 1)
	cap1 := captures[0]
	mu.Unlock()
	assert.Equal(t, "Bearer "+canaryADCToken, cap1.auth)
	assert.Equal(t, quotaProject, cap1.qp)
	assert.Empty(t, cap1.goog, "x-goog-api-key must never be sent on ADC probe")
	assert.NotContains(t, cap1.body, canaryADCToken)

	// 5. Create second ADC instance WITHOUT quota_project (optional/unset by default for service_account; Addendum D):
	//    GET and probe show "implicit (service account association)", wire omits X-Goog-User-Project, store options is nil.
	noQPCreate := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":     "gemini-adc-no-qp",
		"adapter":  geminidecisions.AdapterName,
		"model":    "gemini-decision-1",
		"endpoint": "https://generativelanguage.googleapis.com:443/",
		"auth":     map[string]any{"kind": "google_access_token", "scopes": []string{testADCScope}},
	})
	require.Equal(t, http.StatusCreated, noQPCreate.Code, noQPCreate.Body.String())
	var createdNoQP SystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(noQPCreate.Body.Bytes(), &createdNoQP))
	assert.Equal(t, systemoneprovider.QuotaProjectImplicit, createdNoQP.Auth.QuotaProject)
	assert.Empty(t, createdNoQP.Options)
	storedNoQP, err := s.GetSystemOneInstanceByName(ctx, "gemini-adc-no-qp")
	require.NoError(t, err)
	assert.Nil(t, storedNoQP.Options)

	mu.Lock()
	captures = nil
	mu.Unlock()
	implicitProbeRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/gemini-adc-no-qp/probe", map[string]any{
		"spec":  binSpec,
		"probe": map[string]any{"text": "Synthetic text."},
	})
	require.Equal(t, http.StatusOK, implicitProbeRec.Code, implicitProbeRec.Body.String())
	var implicitProbe ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(implicitProbeRec.Body.Bytes(), &implicitProbe))
	assert.Equal(t, systemoneruntime.ClassSuccess, implicitProbe.Class)
	require.NotNil(t, implicitProbe.Evaluation.Auth)
	assert.Equal(t, systemoneprovider.QuotaProjectImplicit, implicitProbe.Evaluation.Auth.QuotaProject)
	mu.Lock()
	require.Len(t, captures, 1)
	assert.Empty(t, captures[0].qp, "X-Goog-User-Project must be omitted when quota_project is unset")
	captures = nil
	currentADCType = SystemOneADCTypeAuthorizedUser
	currentSAEmail = ""
	mu.Unlock()
	installFakeADC()

	envChangedProbe := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/gemini-adc-no-qp/probe", map[string]any{
		"spec":  binSpec,
		"probe": map[string]any{"text": "Synthetic text."},
	})
	require.Equal(t, http.StatusOK, envChangedProbe.Code, envChangedProbe.Body.String())
	var envChangedResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(envChangedProbe.Body.Bytes(), &envChangedResp))
	assert.Equal(t, systemoneruntime.ClassFailed, envChangedResp.Class)
	assert.Equal(t, string(systemoneprovider.FailAuth), envChangedResp.Reason)
	require.NotNil(t, envChangedResp.Evaluation)
	require.Len(t, envChangedResp.Evaluation.Attempts, 1)
	assert.Equal(t, 0, envChangedResp.Evaluation.Attempts[0].Requests)
	mu.Lock()
	assert.Empty(t, captures, "authorized_user without quota_project must fail pre-call with 0 provider requests")
	mu.Unlock()

	// Once authorized_user is already resolved in-process, create without quota_project rejects (400) with 0 additional ADC calls.
	callsBeforeWarmCreate := adcProviderCalls.Load()
	authUserNoQPRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "gem-adc-user-no-qp",
		"adapter": geminidecisions.AdapterName,
		"model":   "gemini-decision-1",
		"auth":    map[string]any{"kind": "google_access_token", "scopes": []string{testADCScope}},
	})
	require.Equal(t, http.StatusBadRequest, authUserNoQPRec.Code, authUserNoQPRec.Body.String())
	assert.Contains(t, authUserNoQPRec.Body.String(), "quota_project is required")
	assert.Contains(t, authUserNoQPRec.Body.String(), `"reason":"invalid_request"`)
	assert.Equal(t, callsBeforeWarmCreate, adcProviderCalls.Load(), "create must read cached ADC metadata with 0 additional provider calls")

	// 6. Pre-call token endpoint 503 -> unavailable, requests=0, canary scrubbed.
	mu.Lock()
	currentADCType = SystemOneADCTypeServiceAccount
	currentSAEmail = saEmail
	tokenErr = &oauth2.RetrieveError{
		Response: &http.Response{StatusCode: http.StatusServiceUnavailable},
		Body:     []byte("sts outage echoing " + canaryADCToken),
	}
	mu.Unlock()
	installFakeADC()

	unavailRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/gemini-adc-main/probe", map[string]any{
		"spec":  binSpec,
		"probe": map[string]any{"text": "Synthetic text."},
	})
	require.Equal(t, http.StatusOK, unavailRec.Code, unavailRec.Body.String())
	assert.NotContains(t, unavailRec.Body.String(), canaryADCToken)
	var unavailResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(unavailRec.Body.Bytes(), &unavailResp))
	assert.Equal(t, systemoneruntime.ClassFailed, unavailResp.Class)
	assert.Equal(t, string(systemoneprovider.FailUnavailable), unavailResp.Reason)
	require.Len(t, unavailResp.Evaluation.Attempts, 1)
	assert.Equal(t, 0, unavailResp.Evaluation.Attempts[0].Requests)

	// 6b. Provider HTTP 403 ACCESS_TOKEN_SCOPE_INSUFFICIENT -> auth, requests=1, canary scrubbed.
	mu.Lock()
	tokenErr = nil
	nextCode = http.StatusForbidden
	nextBody = `{"error":{"code":403,"message":"Request had insufficient authentication scopes: ` + canaryADCToken + `","status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"ACCESS_TOKEN_SCOPE_INSUFFICIENT"}]}}`
	mu.Unlock()
	installFakeADC()

	scope403Rec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/gemini-adc-main/probe", map[string]any{
		"spec":  binSpec,
		"probe": map[string]any{"text": "Synthetic text."},
	})
	require.Equal(t, http.StatusOK, scope403Rec.Code, scope403Rec.Body.String())
	assert.NotContains(t, scope403Rec.Body.String(), canaryADCToken)
	var scope403Resp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(scope403Rec.Body.Bytes(), &scope403Resp))
	assert.Equal(t, systemoneruntime.ClassFailed, scope403Resp.Class)
	assert.Equal(t, string(systemoneprovider.FailAuth), scope403Resp.Reason)
	require.Len(t, scope403Resp.Evaluation.Attempts, 1)
	assert.Equal(t, 1, scope403Resp.Evaluation.Attempts[0].Requests)
	assert.Equal(t, "4xx", scope403Resp.Evaluation.Attempts[0].HTTPStatusClass)

	// 7. Delete removes row only (no secret existed), returning 204 then 404 on GET.
	delRec := doRequestAsUser(t, srv, admin, http.MethodDelete, systemOneAdminPath+"/gemini-adc-main", nil)
	require.Equal(t, http.StatusNoContent, delRec.Code)
	getDeletedRec := doRequestAsUser(t, srv, admin, http.MethodGet, systemOneAdminPath+"/gemini-adc-main", nil)
	assert.Equal(t, http.StatusNotFound, getDeletedRec.Code)

	allLogs := auditBuf.String() + "\n" + runtimeLogBuf.String()
	assert.NotContains(t, allLogs, canaryADCToken, "ADC canary token must never appear in audit or runtime logs")
	assert.NotContains(t, allLogs, canaryAPIKeyInput)
}

func TestSystemOneInstances_GeminiNamedSecretVerticalConcurrencyAuthzRotationAndMetadataPreservation(t *testing.T) {
	ctx := context.Background()
	srv, s, localBackend := newSystemOneTestServer(t, true)

	var auditBuf, runtimeLogBuf syncBuffer
	srv.envSecretLog = slog.New(slog.NewJSONHandler(&auditBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-g4d-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)

	const (
		envCanaryKey    = "CANARY_G4D_ENV_IGNORED_8414"
		hubCanaryKey    = "CANARY_G4D_HUB_SECRET_KEY_8414"
		projACanaryKey  = "CANARY_G4D_PROJA_SECRET_KEY_8414"
		projARotatedKey = "CANARY_G4D_PROJA_ROTATED_KEY_8414"
		projBCanaryKey  = "CANARY_G4D_PROJB_SECRET_KEY_8414"
		projSecKey      = "CANARY_G4D_UNAUTHZ_PROJ_KEY_8414"
		userCanaryKey   = "CANARY_G4D_USER_SCOPE_KEY_8414"
	)

	// Setting GEMINI_API_KEY in process env has zero effect.
	t.Setenv("GEMINI_API_KEY", envCanaryKey)

	var providerHits atomic.Int32
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerHits.Add(1)
		gotKey := r.Header.Get(geminidecisions.HeaderAPIKey)
		noul := 0.11
		switch gotKey {
		case hubCanaryKey:
			noul = 0.81
		case projACanaryKey:
			noul = 0.82
		case projARotatedKey:
			noul = 0.83
		case projBCanaryKey:
			noul = 0.84
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"answers":{"q_ok":{"type":"noul","noul":%.2f,"confidence":0.95}}}`, noul)
	}))
	t.Cleanup(tlsSrv.Close)

	reg := srv.SystemOneProviderRegistry()
	srv.mu.Lock()
	srv.systemOneRuntime = systemoneruntime.New(
		systemoneruntime.Config{
			Registry:    reg,
			Instances:   &hubSystemOneInstanceSource{srv: srv},
			Credentials: (&hubSystemOneCredentialSource{srv: srv}).APIKeyToken,
			TokenSource: (&hubSystemOneCredentialSource{srv: srv}).GoogleAccessToken,
			Logger:      slog.New(slog.NewJSONHandler(&runtimeLogBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		},
		systemoneruntime.WithEgressOptionsForTesting(
			testSystemOneEgressOptions(t, tlsSrv)...,
		),
	)
	srv.mu.Unlock()

	// Seed projects A, B, and Restricted.
	makeProj := func(slug string) *store.Project {
		p := &store.Project{
			ID:      tid("g4d-" + slug),
			Name:    slug,
			Slug:    slug,
			OwnerID: admin.ID,
			Created: time.Now(),
			Updated: time.Now(),
		}
		require.NoError(t, s.CreateProject(ctx, p))
		return p
	}
	projA := makeProj("g4d-proj-a")
	projB := makeProj("g4d-proj-b")
	projRestricted := makeProj("g4d-proj-restricted")

	// Seed Hub secret (variable), Project A secret (environment with injection_mode=always),
	// Project B secret (variable), Restricted project secret, and User-scope secret.
	_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
		Name:          "GEMINI_API_KEY",
		Value:         hubCanaryKey,
		SecretType:    store.SecretTypeEnvironment,
		Target:        "GEMINI_API_KEY",
		InjectionMode: store.InjectionModeAsNeeded,
		Scope:         store.ScopeHub,
		ScopeID:       srv.hubID,
		CreatedBy:     admin.ID,
		UpdatedBy:     admin.ID,
	})
	require.NoError(t, err)

	_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
		Name:          "GEMINI_API_KEY",
		Value:         projACanaryKey,
		SecretType:    store.SecretTypeEnvironment,
		Target:        "GEMINI_API_KEY",
		InjectionMode: store.InjectionModeAlways,
		Scope:         store.ScopeProject,
		ScopeID:       projA.ID,
		CreatedBy:     admin.ID,
		UpdatedBy:     admin.ID,
	})
	require.NoError(t, err)

	_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
		Name:          "GEMINI_API_KEY",
		Value:         projBCanaryKey,
		SecretType:    store.SecretTypeEnvironment,
		Target:        "GEMINI_API_KEY",
		InjectionMode: store.InjectionModeAsNeeded,
		Scope:         store.ScopeProject,
		ScopeID:       projB.ID,
		CreatedBy:     admin.ID,
		UpdatedBy:     admin.ID,
	})
	require.NoError(t, err)

	_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "GEMINI_API_KEY",
		Value:      projSecKey,
		SecretType: store.SecretTypeEnvironment,
		Target:     "GEMINI_API_KEY",
		Scope:      store.ScopeProject,
		ScopeID:    projRestricted.ID,
		CreatedBy:  admin.ID,
		UpdatedBy:  admin.ID,
	})
	require.NoError(t, err)

	_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "GEMINI_API_KEY",
		Value:      userCanaryKey,
		SecretType: store.SecretTypeEnvironment,
		Target:     "GEMINI_API_KEY",
		Scope:      store.ScopeUser,
		ScopeID:    admin.ID,
		CreatedBy:  admin.ID,
		UpdatedBy:  admin.ID,
	})
	require.NoError(t, err)

	// Snapshot Project A and Hub secret metadata BEFORE creating any System One instance.
	snapSecretMeta := func(name, scope, scopeID string) []byte {
		row, getErr := s.GetSecret(ctx, name, scope, scopeID)
		require.NoError(t, getErr)
		raw, mErr := json.Marshal(row)
		require.NoError(t, mErr)
		return raw
	}
	hubMetaBefore := snapSecretMeta("GEMINI_API_KEY", store.ScopeHub, srv.hubID)
	projAMetaBefore := snapSecretMeta("GEMINI_API_KEY", store.ScopeProject, projA.ID)

	// 1. Validation & mutual exclusion on create.
	for _, tc := range []struct {
		name string
		body map[string]any
		msg  string
	}{
		{
			name: "typesafe without apiKey or secretRef still fails 400",
			body: map[string]any{
				"name": "ts-no-key", "adapter": jevwire.AdapterNameTypeSafe, "model": "jev-1.14.0",
			},
			msg: "apiKey is required",
		},
		{
			name: "apiKey and secretRef mutually exclusive",
			body: map[string]any{
				"name": "gem-both", "adapter": geminidecisions.AdapterName, "model": "gemini-decision-1",
				"auth": map[string]any{
					"kind":      "api_key",
					"apiKey":    "SYNTH_INLINE_KEY",
					"secretRef": map[string]any{"name": "GEMINI_API_KEY", "scope": "hub"},
				},
			},
			msg: "mutually exclusive",
		},
		{
			name: "google_access_token and secretRef mutually exclusive",
			body: map[string]any{
				"name": "gem-adc-ref", "adapter": geminidecisions.AdapterName, "model": "gemini-decision-1",
				"auth": map[string]any{
					"kind":      "google_access_token",
					"scopes":    []string{"https://www.googleapis.com/auth/scope-a"},
					"secretRef": map[string]any{"name": "GEMINI_API_KEY"},
				},
			},
			msg: "auth.secretRef is not supported",
		},
		{
			name: "invalid secretRef.name grammar",
			body: map[string]any{
				"name": "gem-bad-name", "adapter": geminidecisions.AdapterName, "model": "gemini-decision-1",
				"auth": map[string]any{
					"kind":      "api_key",
					"secretRef": map[string]any{"name": "1INVALID-NAME"},
				},
			},
			msg: "invalid auth.secretRef.name",
		},
		{
			name: "reserved prefix in secretRef.name refused",
			body: map[string]any{
				"name": "gem-res-name", "adapter": geminidecisions.AdapterName, "model": "gemini-decision-1",
				"auth": map[string]any{
					"kind":      "api_key",
					"secretRef": map[string]any{"name": "systemone_instance_other"},
				},
			},
			msg: "invalid auth.secretRef.name",
		},
		{
			name: "invalid secretRef.scope refused",
			body: map[string]any{
				"name": "gem-bad-scope", "adapter": geminidecisions.AdapterName, "model": "gemini-decision-1",
				"auth": map[string]any{
					"kind":      "api_key",
					"secretRef": map[string]any{"name": "GEMINI_API_KEY", "scope": "user"},
				},
			},
			msg: "invalid auth.secretRef.scope",
		},
	} {
		rec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, tc.body)
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", tc.name, rec.Body.String())
		assert.Contains(t, rec.Body.String(), tc.msg, tc.name)
	}

	// 2. Create gemini_decisions without apiKey or secretRef -> defaults to GEMINI_API_KEY / project_or_hub.
	createDefRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "gemini-named-default",
		"adapter": geminidecisions.AdapterName,
		"model":   "gemini-decision-1",
	})
	require.Equal(t, http.StatusCreated, createDefRec.Code, createDefRec.Body.String())
	var createdDef SystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(createDefRec.Body.Bytes(), &createdDef))
	assert.Equal(t, "api_key", createdDef.Auth.Kind)
	assert.Empty(t, createdDef.Auth.SecretName)
	require.NotNil(t, createdDef.Auth.SecretRef)
	assert.Equal(t, "GEMINI_API_KEY", createdDef.Auth.SecretRef.Name)
	assert.Equal(t, "project_or_hub", createdDef.Auth.SecretRef.Scope)
	assert.Empty(t, createdDef.Options, "internal secret_scope storage key must not leak in response options")

	// Verify persisted DB row has explicit secret_scope discriminator and GET/LIST both show GEMINI_API_KEY + project_or_hub.
	storedDef, err := s.GetSystemOneInstanceByName(ctx, "gemini-named-default")
	require.NoError(t, err)
	assert.Equal(t, "GEMINI_API_KEY", storedDef.Auth.SecretName)
	assert.Equal(t, "project_or_hub", storedDef.Options[systemoneprovider.OptionSecretScope])

	getDefRec := doRequestAsUser(t, srv, admin, http.MethodGet, systemOneAdminPath+"/gemini-named-default", nil)
	require.Equal(t, http.StatusOK, getDefRec.Code)
	var gotDef SystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(getDefRec.Body.Bytes(), &gotDef))
	require.NotNil(t, gotDef.Auth.SecretRef)
	assert.Equal(t, "GEMINI_API_KEY", gotDef.Auth.SecretRef.Name)
	assert.Equal(t, "project_or_hub", gotDef.Auth.SecretRef.Scope)

	listDefRec := doRequestAsUser(t, srv, admin, http.MethodGet, systemOneAdminPath, nil)
	require.Equal(t, http.StatusOK, listDefRec.Code)
	assert.Contains(t, listDefRec.Body.String(), `"secretRef":{"name":"GEMINI_API_KEY","scope":"project_or_hub"}`)

	// Also create hub-only and project-only named-secret instances.
	for _, pair := range [][2]string{
		{"gemini-named-hub", "hub"},
		{"gemini-named-proj", "project"},
	} {
		rec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
			"name":     pair[0],
			"adapter":  geminidecisions.AdapterName,
			"model":    "gemini-decision-1",
			"endpoint": geminidecisions.DefaultEndpoint,
			"auth": map[string]any{
				"kind":      "api_key",
				"secretRef": map[string]any{"name": "GEMINI_API_KEY", "scope": pair[1]},
			},
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}

	// Secret metadata is byte-identical after instance create.
	assert.Equal(t, hubMetaBefore, snapSecretMeta("GEMINI_API_KEY", store.ScopeHub, srv.hubID))
	assert.Equal(t, projAMetaBefore, snapSecretMeta("GEMINI_API_KEY", store.ScopeProject, projA.ID))

	binSpec := json.RawMessage(`{"version":1,"questions":[{"id":"q_ok","type":"binary","instructions":"Check probe.text."}]}`)

	// 3. Probe without --project: fails with invalid_request (0 requests) on project_or_hub & project, succeeds on hub!
	providerHits.Store(0)
	noProjRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/gemini-named-default/probe", map[string]any{
		"spec":  binSpec,
		"probe": map[string]any{"text": "Synthetic sample."},
	})
	require.Equal(t, http.StatusOK, noProjRec.Code)
	var noProjResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(noProjRec.Body.Bytes(), &noProjResp))
	assert.Equal(t, systemoneruntime.ClassFailed, noProjResp.Class)
	assert.Equal(t, "invalid_request", noProjResp.Reason)
	assert.Zero(t, providerHits.Load())

	hubOnlyRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/gemini-named-hub/probe", map[string]any{
		"spec":  binSpec,
		"probe": map[string]any{"text": "Synthetic sample."},
	})
	require.Equal(t, http.StatusOK, hubOnlyRec.Code)
	var hubOnlyResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(hubOnlyRec.Body.Bytes(), &hubOnlyResp))
	assert.Equal(t, systemoneruntime.ClassSuccess, hubOnlyResp.Class)
	require.NotNil(t, hubOnlyResp.Evaluation.Auth.SecretRef)
	assert.Equal(t, "hub", hubOnlyResp.Evaluation.Auth.SecretRef.ScopeUsed)
	assert.Equal(t, 1, hubOnlyResp.Evaluation.Auth.SecretVersion)

	// 4. probe --project authorization: non-existent -> 404; unauthorized project -> 403; authorized project -> 200.
	notFoundProjRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/gemini-named-default/probe", map[string]any{
		"project": "does-not-exist-proj",
		"spec":    binSpec,
		"probe":   map[string]any{"text": "Synthetic sample."},
	})
	assert.Equal(t, http.StatusNotFound, notFoundProjRec.Code)

	// Scoped operator with system_one_instance.{read,execute} on hub and project.read ONLY on projA.
	scopedUser := &store.User{
		ID:          tid("g4d-scoped-op"),
		Email:       "g4d-scoped@test.com",
		DisplayName: "Scoped Operator",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, scopedUser))
	hubExecRole, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "g4d-s1-prober",
		Description: "System One probe operator",
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"system_one_instance.read", "system_one_instance.execute"},
	})
	require.NoError(t, err)
	ensureHubMembership(ctx, s, scopedUser.ID)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubExecRole.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      scopedUser.ID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        admin.ID,
	})
	require.NoError(t, err)
	projReadRole, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "g4d-proj-reader",
		Description: "Project reader (no secret_read)",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.read"},
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: projReadRole.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      scopedUser.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projA.ID,
		CreatedBy:        admin.ID,
	})
	require.NoError(t, err)

	providerHits.Store(0)
	unauthzProjRec := doRequestAsUser(t, srv, scopedUser, http.MethodPost, systemOneAdminPath+"/gemini-named-default/probe", map[string]any{
		"project": projRestricted.ID,
		"spec":    binSpec,
		"probe":   map[string]any{"text": "Synthetic sample."},
	})
	assert.Equal(t, http.StatusForbidden, unauthzProjRec.Code)
	assert.Zero(t, providerHits.Load())

	// L1: project.read alone (without project.secret_read) is refused with 403 for project_or_hub named-ref probes.
	readOnlyProjRec := doRequestAsUser(t, srv, scopedUser, http.MethodPost, systemOneAdminPath+"/gemini-named-default/probe", map[string]any{
		"project": projA.Slug,
		"spec":    binSpec,
		"probe":   map[string]any{"text": "Synthetic sample."},
	})
	assert.Equal(t, http.StatusForbidden, readOnlyProjRec.Code)
	assert.Zero(t, providerHits.Load())

	// Granting project.secret_read on projA authorizes the probe.
	projSecretReadRole, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "g4d-proj-secret-reader",
		Description: "Project secret reader",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.secret_read"},
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: projSecretReadRole.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      scopedUser.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projA.ID,
		CreatedBy:        admin.ID,
	})
	require.NoError(t, err)

	authzSlugRec := doRequestAsUser(t, srv, scopedUser, http.MethodPost, systemOneAdminPath+"/gemini-named-default/probe", map[string]any{
		"project": projA.Slug,
		"spec":    binSpec,
		"probe":   map[string]any{"text": "Synthetic sample."},
	})
	require.Equal(t, http.StatusOK, authzSlugRec.Code, authzSlugRec.Body.String())
	var authzSlugResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(authzSlugRec.Body.Bytes(), &authzSlugResp))
	assert.Equal(t, systemoneruntime.ClassSuccess, authzSlugResp.Class)
	assert.Equal(t, "project", authzSlugResp.Evaluation.Auth.SecretRef.ScopeUsed)
	assert.Equal(t, 1, authzSlugResp.Evaluation.Auth.SecretVersion)
	assert.InDelta(t, 0.82, authzSlugResp.Evaluation.Answers["q_ok"].Distribution["true"], 1e-6)

	// 5. Concurrent Project A & Project B probes through the SAME Hub instance & transport.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			rec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/gemini-named-default/probe", map[string]any{
				"project": projA.ID,
				"spec":    binSpec,
				"probe":   map[string]any{"text": "Sample A"},
			})
			if assert.Equal(t, http.StatusOK, rec.Code) {
				var pr ProbeSystemOneInstanceResponse
				if assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pr)) {
					assert.Equal(t, "project", pr.Evaluation.Auth.SecretRef.ScopeUsed)
					assert.InDelta(t, 0.82, pr.Evaluation.Answers["q_ok"].Distribution["true"], 1e-6)
				}
			}
		}()
		go func() {
			defer wg.Done()
			rec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/gemini-named-default/probe", map[string]any{
				"project": projB.ID,
				"spec":    binSpec,
				"probe":   map[string]any{"text": "Sample B"},
			})
			if assert.Equal(t, http.StatusOK, rec.Code) {
				var pr ProbeSystemOneInstanceResponse
				if assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pr)) {
					assert.Equal(t, "project", pr.Evaluation.Auth.SecretRef.ScopeUsed)
					assert.InDelta(t, 0.84, pr.Evaluation.Answers["q_ok"].Distribution["true"], 1e-6)
				}
			}
		}()
	}
	wg.Wait()

	// 6. Rotation of Project A secret bumps version (1 -> 2) and uses new value immediately.
	_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
		Name:          "GEMINI_API_KEY",
		Value:         projARotatedKey,
		SecretType:    store.SecretTypeEnvironment,
		Target:        "GEMINI_API_KEY",
		InjectionMode: store.InjectionModeAlways,
		Scope:         store.ScopeProject,
		ScopeID:       projA.ID,
		CreatedBy:     admin.ID,
		UpdatedBy:     admin.ID,
	})
	require.NoError(t, err)
	projAMetaAfterRotate := snapSecretMeta("GEMINI_API_KEY", store.ScopeProject, projA.ID)

	rotRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/gemini-named-default/probe", map[string]any{
		"project": projA.ID,
		"spec":    binSpec,
		"probe":   map[string]any{"text": "Rotated sample"},
	})
	require.Equal(t, http.StatusOK, rotRec.Code)
	var rotResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(rotRec.Body.Bytes(), &rotResp))
	assert.Equal(t, 2, rotResp.Evaluation.Auth.SecretVersion)
	assert.InDelta(t, 0.83, rotResp.Evaluation.Answers["q_ok"].Distribution["true"], 1e-6)

	// 7. Delete instances -> secret metadata (type, injection_mode, target, version) remains byte-identical!
	for _, instName := range []string{"gemini-named-default", "gemini-named-hub", "gemini-named-proj"} {
		delRec := doRequestAsUser(t, srv, admin, http.MethodDelete, systemOneAdminPath+"/"+instName, nil)
		require.Equal(t, http.StatusNoContent, delRec.Code)
	}
	assert.Equal(t, hubMetaBefore, snapSecretMeta("GEMINI_API_KEY", store.ScopeHub, srv.hubID))
	assert.Equal(t, projAMetaAfterRotate, snapSecretMeta("GEMINI_API_KEY", store.ScopeProject, projA.ID))

	// 7b. Delete-safety & planted-row collision/malformed discriminator tests (EM msgs 78332850 & cfc41ad5):
	// Create a real legacy/inline internal-secret victim instance and confirm planted named-ref or malformed rows
	// NEVER resolve or delete victim's systemone.instance.<id> secret or GEMINI_API_KEY!
	const victimInternalKey = "CANARY_G4D_VICTIM_INTERNAL_KEY_8414"
	victimCreateRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":     "victim-internal",
		"adapter":  geminidecisions.AdapterName,
		"model":    "gemini-decision-1",
		"endpoint": tlsSrv.URL,
		"auth":     map[string]any{"kind": "api_key", "apiKey": victimInternalKey},
	})
	require.Equal(t, http.StatusCreated, victimCreateRec.Code, victimCreateRec.Body.String())
	var victimCreated SystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(victimCreateRec.Body.Bytes(), &victimCreated))
	victimSecretName := victimCreated.Auth.SecretName
	require.Equal(t, "systemone.instance."+victimCreated.ID, victimSecretName)

	plantedCases := []*store.SystemOneInstance{
		{
			ID:       "99990001-0000-4000-8000-000000000001",
			Name:     "planted-collide-hub",
			Adapter:  geminidecisions.AdapterName,
			Model:    "gemini-decision-1",
			Endpoint: tlsSrv.URL,
			Auth:     store.SystemOneInstanceAuth{Kind: store.SystemOneAuthKindAPIKey, SecretName: victimSecretName},
			Options:  map[string]string{systemoneprovider.OptionSecretScope: "hub"},
			Enabled:  true,
		},
		{
			ID:       "99990002-0000-4000-8000-000000000002",
			Name:     "planted-collide-empty-scope",
			Adapter:  geminidecisions.AdapterName,
			Model:    "gemini-decision-1",
			Endpoint: tlsSrv.URL,
			Auth:     store.SystemOneInstanceAuth{Kind: store.SystemOneAuthKindAPIKey, SecretName: victimSecretName},
			Options:  map[string]string{systemoneprovider.OptionSecretScope: ""},
			Enabled:  true,
		},
		{
			ID:       "99990003-0000-4000-8000-000000000003",
			Name:     "planted-unknown-scope",
			Adapter:  geminidecisions.AdapterName,
			Model:    "gemini-decision-1",
			Endpoint: tlsSrv.URL,
			Auth:     store.SystemOneInstanceAuth{Kind: store.SystemOneAuthKindAPIKey, SecretName: "GEMINI_API_KEY"},
			Options:  map[string]string{systemoneprovider.OptionSecretScope: "unknown_scope"},
			Enabled:  true,
		},
		{
			ID:       "99990004-0000-4000-8000-000000000004",
			Name:     "planted-adc-secret-scope",
			Adapter:  geminidecisions.AdapterName,
			Model:    "gemini-decision-1",
			Endpoint: tlsSrv.URL,
			Auth: store.SystemOneInstanceAuth{
				Kind:   store.SystemOneAuthKindGoogleAccessToken,
				Scopes: []string{"https://www.googleapis.com/auth/scope-a"},
			},
			Options: map[string]string{systemoneprovider.OptionSecretScope: "hub"},
			Enabled: true,
		},
	}
	for _, planted := range plantedCases {
		require.NoError(t, s.CreateSystemOneInstance(ctx, planted), planted.Name)

		// Probe must fail closed with 0 provider requests and never read victim's secret.
		providerHits.Store(0)
		pRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/"+planted.Name+"/probe", map[string]any{
			"spec":  binSpec,
			"probe": map[string]any{"text": "Planted row check."},
		})
		require.Equal(t, http.StatusOK, pRec.Code, planted.Name)
		var pResp ProbeSystemOneInstanceResponse
		require.NoError(t, json.Unmarshal(pRec.Body.Bytes(), &pResp))
		assert.Equal(t, systemoneruntime.ClassFailed, pResp.Class, planted.Name)
		assert.Equal(t, "invalid_request", pResp.Reason, planted.Name)
		assert.Zero(t, providerHits.Load(), "%s must make 0 provider requests", planted.Name)

		// Delete must remove the planted row (204) and NEVER delete victim's internal secret or GEMINI_API_KEY.
		dRec := doRequestAsUser(t, srv, admin, http.MethodDelete, systemOneAdminPath+"/"+planted.Name, nil)
		require.Equal(t, http.StatusNoContent, dRec.Code, planted.Name)

		victimSV, vErr := localBackend.Get(ctx, victimSecretName, store.ScopeHub, srv.hubID)
		require.NoError(t, vErr, "victim internal secret must remain intact after deleting %s", planted.Name)
		assert.Equal(t, victimInternalKey, victimSV.Value)
	}
	assert.Equal(t, hubMetaBefore, snapSecretMeta("GEMINI_API_KEY", store.ScopeHub, srv.hubID))
	assert.Equal(t, projAMetaAfterRotate, snapSecretMeta("GEMINI_API_KEY", store.ScopeProject, projA.ID))

	// Legacy internal-secret row (Options: nil, no discriminator) still resolves and deletes its own secret cleanly.
	legacyProbeRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/victim-internal/probe", map[string]any{
		"spec":  binSpec,
		"probe": map[string]any{"text": "Legacy internal check."},
	})
	require.Equal(t, http.StatusOK, legacyProbeRec.Code)
	var legacyProbeResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(legacyProbeRec.Body.Bytes(), &legacyProbeResp))
	assert.Equal(t, systemoneruntime.ClassSuccess, legacyProbeResp.Class)

	legacyDelRec := doRequestAsUser(t, srv, admin, http.MethodDelete, systemOneAdminPath+"/victim-internal", nil)
	require.Equal(t, http.StatusNoContent, legacyDelRec.Code)
	_, err = localBackend.Get(ctx, victimSecretName, store.ScopeHub, srv.hubID)
	assert.ErrorIs(t, err, store.ErrNotFound, "legacy internal secret must be deleted when its owning instance is deleted")

	// 8. Redaction across all logs and HTTP responses.
	combinedLogs := auditBuf.String() + "\n" + runtimeLogBuf.String()
	for _, canary := range []string{
		envCanaryKey, hubCanaryKey, projACanaryKey, projARotatedKey, projBCanaryKey, projSecKey, userCanaryKey, victimInternalKey,
	} {
		assert.NotContains(t, combinedLogs, canary)
		assert.NotContains(t, createDefRec.Body.String(), canary)
		assert.NotContains(t, rotRec.Body.String(), canary)
	}
}

func TestSystemOneInstances_C1_NamedSecretRefusesInternalAndInfrastructureKeys(t *testing.T) {
	ctx := context.Background()
	srv, s, localBackend := newSystemOneTestServer(t, true)

	var auditBuf, runtimeLogBuf syncBuffer
	srv.envSecretLog = slog.New(slog.NewJSONHandler(&auditBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-c1-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)

	const (
		projInternalCanary = "CANARY_C1_PROJECT_INTERNAL_SECRET_8414"
		hubFallbackCanary  = "CANARY_C1_HUB_VALID_VARIABLE_8414"
		mutatedKeyCanary   = "CANARY_C1_MUTATED_TO_INTERNAL_8414"
		ownedKeyCanary     = "CANARY_C1_OWNED_INTERNAL_KEY_8414"
	)

	var (
		providerHits atomic.Int32
		wireHeaders  syncBuffer
	)
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerHits.Add(1)
		_, _ = fmt.Fprintf(&wireHeaders, "%s\n", r.Header.Get(geminidecisions.HeaderAPIKey))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"q_ok":{"type":"noul","noul":0.90,"confidence":0.95}}}`))
	}))
	t.Cleanup(tlsSrv.Close)

	reg := srv.SystemOneProviderRegistry()
	srv.mu.Lock()
	srv.systemOneRuntime = systemoneruntime.New(
		systemoneruntime.Config{
			Registry:    reg,
			Instances:   &hubSystemOneInstanceSource{srv: srv},
			Credentials: (&hubSystemOneCredentialSource{srv: srv}).APIKeyToken,
			TokenSource: (&hubSystemOneCredentialSource{srv: srv}).GoogleAccessToken,
			Logger:      slog.New(slog.NewJSONHandler(&runtimeLogBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		},
		systemoneruntime.WithEgressOptionsForTesting(
			testSystemOneEgressOptions(t, tlsSrv)...,
		),
	)
	srv.mu.Unlock()

	// 1. Seed user_signing_key and agent_signing_key via ensureSigningKey (exactly as Hub startup writes them)
	//    and assert the backend/store NEVER returns SecretType == "" for them (stored as SecretTypeInternal).
	userSignRaw, err := srv.ensureSigningKey(ctx, SecretKeyUserSigningKey, nil)
	require.NoError(t, err)
	agentSignRaw, err := srv.ensureSigningKey(ctx, SecretKeyAgentSigningKey, nil)
	require.NoError(t, err)

	userSignSV, err := localBackend.Get(ctx, SecretKeyUserSigningKey, store.ScopeHub, srv.hubID)
	require.NoError(t, err)
	assert.Equal(t, store.SecretTypeInternal, userSignSV.SecretType, "ensureSigningKey must persist user_signing_key as SecretTypeInternal, never empty")
	userSignB64 := userSignSV.Value
	require.NotEmpty(t, userSignB64)

	agentSignSV, err := localBackend.Get(ctx, SecretKeyAgentSigningKey, store.ScopeHub, srv.hubID)
	require.NoError(t, err)
	assert.Equal(t, store.SecretTypeInternal, agentSignSV.SecretType, "ensureSigningKey must persist agent_signing_key as SecretTypeInternal, never empty")
	agentSignB64 := agentSignSV.Value
	require.NotEmpty(t, agentSignB64)
	_ = userSignRaw
	_ = agentSignRaw

	// Seed a project and plant a project-scope internal secret row (PROJ_INTERNAL_KEY), plus a valid Hub variable
	// secret of the SAME name so project_or_hub proves NO fallback to Hub when the project row is internal!
	proj := &store.Project{
		ID:      tid("c1-proj"),
		Name:    "c1-proj",
		Slug:    "c1-proj",
		OwnerID: admin.ID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "PROJ_INTERNAL_KEY",
		Value:      projInternalCanary,
		SecretType: store.SecretTypeInternal,
		Target:     "PROJ_INTERNAL_KEY",
		Scope:      store.ScopeProject,
		ScopeID:    proj.ID,
		CreatedBy:  admin.ID,
		UpdatedBy:  admin.ID,
	})
	require.NoError(t, err)

	_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "PROJ_INTERNAL_KEY",
		Value:      hubFallbackCanary,
		SecretType: store.SecretTypeEnvironment,
		Target:     "PROJ_INTERNAL_KEY",
		Scope:      store.ScopeHub,
		ScopeID:    srv.hubID,
		CreatedBy:  admin.ID,
		UpdatedBy:  admin.ID,
	})
	require.NoError(t, err)

	// Create a legitimate instance-owned internal-secret instance (--key-stdin / apiKey path) to prove
	// (a) its systemone.instance.<id> secret cannot be referenced by another instance's secret_ref, AND
	// (b) the owning instance itself continues resolving normally!
	ownedCreateRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":     "c1-owned-victim",
		"adapter":  geminidecisions.AdapterName,
		"model":    "gemini-decision-1",
		"endpoint": tlsSrv.URL,
		"auth":     map[string]any{"kind": "api_key", "apiKey": ownedKeyCanary},
	})
	require.Equal(t, http.StatusCreated, ownedCreateRec.Code, ownedCreateRec.Body.String())
	var ownedCreated SystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(ownedCreateRec.Body.Bytes(), &ownedCreated))
	ownedSecretName := ownedCreated.Auth.SecretName
	require.Equal(t, "systemone.instance."+ownedCreated.ID, ownedSecretName)

	// 2. Authoring / create refusal (400 validation_error, content-free) for all internal secrets.
	for _, tc := range []struct {
		name  string
		ref   string
		scope string
	}{
		{name: "hub_user_signing_key", ref: SecretKeyUserSigningKey, scope: "hub"},
		{name: "proj_or_hub_user_signing_key", ref: SecretKeyUserSigningKey, scope: "project_or_hub"},
		{name: "hub_agent_signing_key", ref: SecretKeyAgentSigningKey, scope: "hub"},
		{name: "proj_or_hub_agent_signing_key", ref: SecretKeyAgentSigningKey, scope: "project_or_hub"},
		{name: "project_internal_row_direct", ref: "PROJ_INTERNAL_KEY", scope: "project"},
		{name: "project_internal_row_via_project_or_hub", ref: "PROJ_INTERNAL_KEY", scope: "project_or_hub"},
		{name: "another_instance_owned_secret_exact", ref: ownedSecretName, scope: "hub"},
		{name: "another_instance_owned_secret_sanitized", ref: strings.ReplaceAll(strings.ReplaceAll(ownedSecretName, ".", "_"), "-", "_"), scope: "hub"},
	} {
		rec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
			"name":    "bad-" + strings.ReplaceAll(tc.name, "_", "-"),
			"adapter": geminidecisions.AdapterName,
			"model":   "gemini-decision-1",
			"auth": map[string]any{
				"kind":      "api_key",
				"secretRef": map[string]any{"name": tc.ref, "scope": tc.scope},
			},
		})
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", tc.name, rec.Body.String())
		assert.Contains(t, rec.Body.String(), ErrCodeValidationError, tc.name)
		for _, secretVal := range []string{userSignB64, agentSignB64, projInternalCanary, ownedKeyCanary} {
			assert.NotContains(t, rec.Body.String(), secretVal, tc.name)
		}
	}

	binSpec := json.RawMessage(`{"version":1,"questions":[{"id":"q_ok","type":"binary","instructions":"Check probe.text."}]}`)

	// 3. Resolution refusal on crafted / legacy instance rows: auth with 0 provider requests (httptest receives NOTHING),
	//    including NO fallback to Hub when project row is internal in project_or_hub!
	craftedRows := []struct {
		id         string
		name       string
		secretName string
		scope      string
		wantReason string
	}{
		{id: "99991001-0000-4000-8000-000000000001", name: "crafted-user-sign-hub", secretName: SecretKeyUserSigningKey, scope: "hub", wantReason: "auth"},
		{id: "99991002-0000-4000-8000-000000000002", name: "crafted-agent-sign-hub", secretName: SecretKeyAgentSigningKey, scope: "hub", wantReason: "auth"},
		{id: "99991003-0000-4000-8000-000000000003", name: "crafted-user-sign-poh", secretName: SecretKeyUserSigningKey, scope: "project_or_hub", wantReason: "auth"},
		{id: "99991004-0000-4000-8000-000000000004", name: "crafted-proj-int-direct", secretName: "PROJ_INTERNAL_KEY", scope: "project", wantReason: "auth"},
		{id: "99991005-0000-4000-8000-000000000005", name: "crafted-proj-int-no-fb", secretName: "PROJ_INTERNAL_KEY", scope: "project_or_hub", wantReason: "auth"},
		{id: "99991006-0000-4000-8000-000000000006", name: "crafted-other-inst-san", secretName: "systemone_instance_other", scope: "hub", wantReason: "auth"},
	}
	for _, cr := range craftedRows {
		require.NoError(t, s.CreateSystemOneInstance(ctx, &store.SystemOneInstance{
			ID:       cr.id,
			Name:     cr.name,
			Adapter:  geminidecisions.AdapterName,
			Model:    "gemini-decision-1",
			Endpoint: geminidecisions.DefaultEndpoint,
			Auth:     store.SystemOneInstanceAuth{Kind: store.SystemOneAuthKindAPIKey, SecretName: cr.secretName},
			Options:  map[string]string{systemoneprovider.OptionSecretScope: cr.scope},
			Enabled:  true,
		}))

		providerHits.Store(0)
		pRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/"+cr.name+"/probe", map[string]any{
			"project": proj.ID,
			"spec":    binSpec,
			"probe":   map[string]any{"text": "Crafted row check."},
		})
		require.Equal(t, http.StatusOK, pRec.Code, cr.name)
		var pResp ProbeSystemOneInstanceResponse
		require.NoError(t, json.Unmarshal(pRec.Body.Bytes(), &pResp))
		assert.Equal(t, systemoneruntime.ClassFailed, pResp.Class, cr.name)
		assert.Equal(t, cr.wantReason, pResp.Reason, cr.name)
		require.NotNil(t, pResp.Evaluation, cr.name)
		for _, att := range pResp.Evaluation.Attempts {
			assert.Equal(t, 0, att.Requests, "%s must record 0 provider requests", cr.name)
		}
		assert.Zero(t, providerHits.Load(), "httptest provider must receive NO request for %s", cr.name)
		for _, secretVal := range []string{userSignB64, agentSignB64, projInternalCanary, hubFallbackCanary, ownedKeyCanary} {
			assert.NotContains(t, pRec.Body.String(), secretVal, cr.name)
		}
	}

	// 3b. Even when the secret name IS the provider-approved GEMINI_API_KEY, an internal row in project scope
	//     does NOT block global instance creation (N1 / §F11: create performs no project-scope scans),
	//     and IS refused at project and project_or_hub resolution with NO fallback to a valid Hub GEMINI_API_KEY!
	_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "GEMINI_API_KEY",
		Value:      projInternalCanary,
		SecretType: store.SecretTypeInternal,
		Target:     "GEMINI_API_KEY",
		Scope:      store.ScopeProject,
		ScopeID:    proj.ID,
		CreatedBy:  admin.ID,
		UpdatedBy:  admin.ID,
	})
	require.NoError(t, err)
	_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "GEMINI_API_KEY",
		Value:      mutatedKeyCanary,
		SecretType: store.SecretTypeEnvironment,
		Target:     "GEMINI_API_KEY",
		Scope:      store.ScopeHub,
		ScopeID:    srv.hubID,
		CreatedBy:  admin.ID,
		UpdatedBy:  admin.ID,
	})
	require.NoError(t, err)

	for _, sc := range []string{"project", "project_or_hub"} {
		instName := "c1-gem-proj-" + strings.ReplaceAll(sc, "_", "-")
		okCreate := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
			"name":    instName,
			"adapter": geminidecisions.AdapterName,
			"model":   "gemini-decision-1",
			"auth": map[string]any{
				"kind":      "api_key",
				"secretRef": map[string]any{"name": "GEMINI_API_KEY", "scope": sc},
			},
		})
		require.Equal(t, http.StatusCreated, okCreate.Code, "scope=%s body=%s", sc, okCreate.Body.String())

		providerHits.Store(0)
		pRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/"+instName+"/probe", map[string]any{
			"project": proj.ID,
			"spec":    binSpec,
			"probe":   map[string]any{"text": "Project internal GEMINI_API_KEY must fail with auth and no fallback."},
		})
		require.Equal(t, http.StatusOK, pRec.Code, sc)
		var pResp ProbeSystemOneInstanceResponse
		require.NoError(t, json.Unmarshal(pRec.Body.Bytes(), &pResp))
		assert.Equal(t, systemoneruntime.ClassFailed, pResp.Class, sc)
		assert.Equal(t, "auth", pResp.Reason, sc)
		require.Len(t, pResp.Evaluation.Attempts, 1, sc)
		assert.Equal(t, 0, pResp.Evaluation.Attempts[0].Requests, sc)
		assert.Zero(t, providerHits.Load(), sc)
	}

	require.NoError(t, s.CreateSystemOneInstance(ctx, &store.SystemOneInstance{
		ID:       "99991007-0000-4000-8000-000000000007",
		Name:     "crafted-gem-proj-int-no-fb",
		Adapter:  geminidecisions.AdapterName,
		Model:    "gemini-decision-1",
		Endpoint: geminidecisions.DefaultEndpoint,
		Auth:     store.SystemOneInstanceAuth{Kind: store.SystemOneAuthKindAPIKey, SecretName: "GEMINI_API_KEY"},
		Options:  map[string]string{systemoneprovider.OptionSecretScope: "project_or_hub"},
		Enabled:  true,
	}))
	providerHits.Store(0)
	noFBRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/crafted-gem-proj-int-no-fb/probe", map[string]any{
		"project": proj.ID,
		"spec":    binSpec,
		"probe":   map[string]any{"text": "Project internal GEMINI_API_KEY must not fall back to Hub."},
	})
	require.Equal(t, http.StatusOK, noFBRec.Code)
	var noFBResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(noFBRec.Body.Bytes(), &noFBResp))
	assert.Equal(t, systemoneruntime.ClassFailed, noFBResp.Class)
	assert.Equal(t, "auth", noFBResp.Reason)
	require.Len(t, noFBResp.Evaluation.Attempts, 1)
	assert.Equal(t, 0, noFBResp.Evaluation.Attempts[0].Requests)
	assert.Zero(t, providerHits.Load())

	// Remove the planted project internal row so we can test valid Hub GEMINI_API_KEY mutating to internal.
	require.NoError(t, s.DeleteSecret(ctx, "GEMINI_API_KEY", store.ScopeProject, proj.ID))

	// 4. Regression: a previously valid named secret (variable) that later becomes internal fails at the next resolution!
	mutCreateRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "c1-mutated-type",
		"adapter": geminidecisions.AdapterName,
		"model":   "gemini-decision-1",
		"auth": map[string]any{
			"kind":      "api_key",
			"secretRef": map[string]any{"name": "GEMINI_API_KEY", "scope": "hub"},
		},
	})
	require.Equal(t, http.StatusCreated, mutCreateRec.Code, mutCreateRec.Body.String())

	providerHits.Store(0)
	mutOKRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/c1-mutated-type/probe", map[string]any{
		"spec":  binSpec,
		"probe": map[string]any{"text": "Before type mutation."},
	})
	require.Equal(t, http.StatusOK, mutOKRec.Code)
	var mutOKResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(mutOKRec.Body.Bytes(), &mutOKResp))
	assert.Equal(t, systemoneruntime.ClassSuccess, mutOKResp.Class)
	assert.Equal(t, int32(1), providerHits.Load())

	// Mutate the stored row's SecretType to internal -> next resolution MUST fail closed with auth and 0 provider requests!
	_, err = s.UpdateSecretMeta(ctx, "GEMINI_API_KEY", store.ScopeHub, srv.hubID, &store.SecretMetaUpdate{
		SecretType: store.SecretTypeInternal,
		Target:     "GEMINI_API_KEY",
		UpdatedBy:  admin.ID,
	})
	require.NoError(t, err)

	providerHits.Store(0)
	mutFailRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/c1-mutated-type/probe", map[string]any{
		"spec":  binSpec,
		"probe": map[string]any{"text": "After type mutation to internal."},
	})
	require.Equal(t, http.StatusOK, mutFailRec.Code)
	var mutFailResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(mutFailRec.Body.Bytes(), &mutFailResp))
	assert.Equal(t, systemoneruntime.ClassFailed, mutFailResp.Class)
	assert.Equal(t, "auth", mutFailResp.Reason)
	require.Len(t, mutFailResp.Evaluation.Attempts, 1)
	assert.Equal(t, 0, mutFailResp.Evaluation.Attempts[0].Requests)
	assert.Zero(t, providerHits.Load(), "provider must receive 0 requests once secret row becomes internal")

	// 5. Instance-owned internal secret path (c1-owned-victim, no secret_scope) continues working normally!
	providerHits.Store(0)
	ownedProbeRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/c1-owned-victim/probe", map[string]any{
		"spec":  binSpec,
		"probe": map[string]any{"text": "Owned credential check."},
	})
	require.Equal(t, http.StatusOK, ownedProbeRec.Code)
	var ownedProbeResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(ownedProbeRec.Body.Bytes(), &ownedProbeResp))
	assert.Equal(t, systemoneruntime.ClassSuccess, ownedProbeResp.Class)
	assert.Equal(t, int32(1), providerHits.Load())

	// Confirm user_signing_key, agent_signing_key, and projInternalCanary never hit the wire or logs.
	wireStr := wireHeaders.String()
	allLogs := auditBuf.String() + "\n" + runtimeLogBuf.String()
	for _, forbidden := range []string{userSignB64, agentSignB64, projInternalCanary, hubFallbackCanary} {
		assert.NotContains(t, wireStr, forbidden)
		assert.NotContains(t, allLogs, forbidden)
	}
}

func TestSystemOneInstances_Item7_EndpointPinForNamedSecretAndADC(t *testing.T) {
	ctx := context.Background()
	srv, s, localBackend := newSystemOneTestServer(t, true)
	countingBackend := &countingSecretBackendWrapper{SecretBackend: localBackend}
	srv.SetSecretBackend(countingBackend)

	var auditBuf, runtimeLogBuf syncBuffer
	srv.envSecretLog = slog.New(slog.NewJSONHandler(&auditBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	superAdmin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-f9-super", store.SystemRoleSuperAdmin))
	require.NoError(t, err)
	// Built-in hub-admin user (not a member of victimProj, holds no secret permissions).
	hubAdmin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-f9-hubadmin", store.SystemRoleHubAdmin))
	require.NoError(t, err)
	projOwner, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-f9-projowner", store.SystemRoleHubMember))
	require.NoError(t, err)

	const (
		projSecretCanary = "CANARY_F9_PROJECT_GITHUB_TOKEN_8414"
		projGeminiCanary = "CANARY_F9_PROJECT_GEMINI_KEY_8414"
		hubSecretCanary  = "CANARY_F9_HUB_GEMINI_KEY_8414"
		ownedKeyCanary   = "CANARY_F9_OWNED_CUSTOM_EP_KEY_8414"
		adcCanaryToken   = "CANARY_F9_ADC_BEARER_TOKEN_8414"
		testADCScope     = "https://www.googleapis.com/auth/synthetic.f9.scope"
	)

	var (
		defaultProviderHits atomic.Int32
		attackerHits        atomic.Int32
		adcProviderCalls    atomic.Int32
		adcTokenMints       atomic.Int32
	)
	defaultProviderSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defaultProviderHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"q_ok":{"type":"noul","noul":0.92,"confidence":0.95}}}`))
	}))
	t.Cleanup(defaultProviderSrv.Close)

	attackerSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"q_ok":{"type":"noul","noul":0.92,"confidence":0.95}}}`))
	}))
	t.Cleanup(attackerSrv.Close)

	srv.SetSystemOneADCForTesting(t, func(_ context.Context, _ []string) (*SystemOneADCCredentials, error) {
		adcProviderCalls.Add(1)
		return &SystemOneADCCredentials{
			CredentialType:      SystemOneADCTypeServiceAccount,
			ServiceAccountEmail: "so-f9-sa@proj.iam.gserviceaccount.com",
			TokenSource: funcTokenSource(func() (*oauth2.Token, error) {
				adcTokenMints.Add(1)
				return &oauth2.Token{AccessToken: adcCanaryToken, Expiry: time.Now().Add(time.Hour)}, nil
			}),
		}, nil
	})

	reg := srv.SystemOneProviderRegistry()
	// Route canonical default (:443) to defaultProviderSrv AND allow loopback to attackerSrv for owned-credential regression.
	egOpts := append(
		testSystemOneEgressOptions(t, defaultProviderSrv),
		egress.WithLoopbackServerForTesting(attackerSrv.URL, attackerSrv.Client()),
	)
	srv.mu.Lock()
	srv.systemOneRuntime = systemoneruntime.New(
		systemoneruntime.Config{
			Registry:    reg,
			Instances:   &hubSystemOneInstanceSource{srv: srv},
			Credentials: (&hubSystemOneCredentialSource{srv: srv}).APIKeyToken,
			TokenSource: (&hubSystemOneCredentialSource{srv: srv}).GoogleAccessToken,
			Logger:      slog.New(slog.NewJSONHandler(&runtimeLogBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		},
		systemoneruntime.WithEgressOptionsForTesting(egOpts...),
	)
	srv.mu.Unlock()

	victimProj := &store.Project{
		ID:      tid("f9-victim-proj"),
		Name:    "f9-victim-proj",
		Slug:    "f9-victim-proj",
		OwnerID: projOwner.ID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, victimProj))

	_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "GITHUB_TOKEN",
		Value:      projSecretCanary,
		SecretType: store.SecretTypeEnvironment,
		Target:     "GITHUB_TOKEN",
		Scope:      store.ScopeProject,
		ScopeID:    victimProj.ID,
		CreatedBy:  projOwner.ID,
		UpdatedBy:  projOwner.ID,
	})
	require.NoError(t, err)

	_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "GEMINI_API_KEY",
		Value:      projGeminiCanary,
		SecretType: store.SecretTypeEnvironment,
		Target:     "GEMINI_API_KEY",
		Scope:      store.ScopeProject,
		ScopeID:    victimProj.ID,
		CreatedBy:  projOwner.ID,
		UpdatedBy:  projOwner.ID,
	})
	require.NoError(t, err)

	_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "GEMINI_API_KEY",
		Value:      hubSecretCanary,
		SecretType: store.SecretTypeEnvironment,
		Target:     "GEMINI_API_KEY",
		Scope:      store.ScopeHub,
		ScopeID:    srv.hubID,
		CreatedBy:  superAdmin.ID,
		UpdatedBy:  superAdmin.ID,
	})
	require.NoError(t, err)

	// 1b. Independent allowed-name endpoint pin (FV-R1 + Hub N2): GEMINI_API_KEY (environment/variable)
	//     with custom endpoints -> 400 at create with endpoint-pin error for project and project_or_hub!
	for _, sc := range []string{"project", "project_or_hub"} {
		for _, badEndpoint := range []string{
			attackerSrv.URL,
			"https://attacker.example",
			"https://generativelanguage.googleapis.com:8443",
			"https://generativelanguage.googleapis.com/v1beta",
		} {
			rec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
				"name":     "f9-bad-allowed-ref",
				"adapter":  geminidecisions.AdapterName,
				"model":    "gemini-decision-1",
				"endpoint": badEndpoint,
				"auth": map[string]any{
					"kind":      "api_key",
					"secretRef": map[string]any{"name": "GEMINI_API_KEY", "scope": sc},
				},
			})
			require.Equal(t, http.StatusBadRequest, rec.Code, "scope=%s endpoint=%q body=%s", sc, badEndpoint, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "default endpoint", "scope=%s endpoint=%q", sc, badEndpoint)
			assert.NotContains(t, rec.Body.String(), projGeminiCanary)
			assert.NotContains(t, rec.Body.String(), hubSecretCanary)
		}
	}

	// 2b. Forced rows with ALLOWED name GEMINI_API_KEY + custom endpoint -> auth with 0 SecretBackend.Get,
	//     0 attacker hits, and 0 default hits for both project and project_or_hub!
	for idx, sc := range []string{"project", "project_or_hub"} {
		forcedID := fmt.Sprintf("9999701%d-0000-4000-8000-000000000001", idx+1)
		forcedName := "f9-forced-gem-" + strings.ReplaceAll(sc, "_", "-")
		require.NoError(t, s.CreateSystemOneInstance(ctx, &store.SystemOneInstance{
			ID:       forcedID,
			Name:     forcedName,
			Adapter:  geminidecisions.AdapterName,
			Model:    "gemini-decision-1",
			Endpoint: attackerSrv.URL,
			Auth:     store.SystemOneInstanceAuth{Kind: store.SystemOneAuthKindAPIKey, SecretName: "GEMINI_API_KEY"},
			Options:  map[string]string{systemoneprovider.OptionSecretScope: sc},
			Enabled:  true,
		}))

		countingBackend.getCalls.Store(0)
		attackerHits.Store(0)
		defaultProviderHits.Store(0)
		rec := doRequestAsUser(t, srv, superAdmin, http.MethodPost, systemOneAdminPath+"/"+forcedName+"/probe", map[string]any{
			"project": victimProj.ID,
			"spec":    json.RawMessage(`{"version":1,"questions":[{"id":"q_ok","type":"binary","instructions":"Check probe.text."}]}`),
			"probe":   map[string]any{"text": "Allowed-name custom endpoint probe."},
		})
		require.Equal(t, http.StatusOK, rec.Code, "scope=%s body=%s", sc, rec.Body.String())
		var pResp ProbeSystemOneInstanceResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pResp))
		assert.Equal(t, systemoneruntime.ClassFailed, pResp.Class, sc)
		assert.Equal(t, "auth", pResp.Reason, sc)
		assert.Zero(t, countingBackend.getCalls.Load(), "allowed-name custom endpoint must make 0 SecretBackend.Get calls (scope=%s)", sc)
		assert.Zero(t, attackerHits.Load(), "attacker server must receive 0 requests (scope=%s)", sc)
		assert.Zero(t, defaultProviderHits.Load(), "default provider must receive 0 requests (scope=%s)", sc)
		assert.NotContains(t, rec.Body.String(), projGeminiCanary)
		assert.NotContains(t, rec.Body.String(), hubSecretCanary)
	}

	// Positive control for allowed-name project secret at canonical default endpoint.
	posCreate := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":     "f9-good-gem-proj",
		"adapter":  geminidecisions.AdapterName,
		"model":    "gemini-decision-1",
		"endpoint": geminidecisions.DefaultEndpoint,
		"auth": map[string]any{
			"kind":      "api_key",
			"secretRef": map[string]any{"name": "GEMINI_API_KEY", "scope": "project"},
		},
	})
	require.Equal(t, http.StatusCreated, posCreate.Code, posCreate.Body.String())
	countingBackend.getCalls.Store(0)
	defaultProviderHits.Store(0)
	posProbe := doRequestAsUser(t, srv, superAdmin, http.MethodPost, systemOneAdminPath+"/f9-good-gem-proj/probe", map[string]any{
		"project": victimProj.ID,
		"spec":    json.RawMessage(`{"version":1,"questions":[{"id":"q_ok","type":"binary","instructions":"Check probe.text."}]}`),
		"probe":   map[string]any{"text": "Positive control canonical endpoint."},
	})
	require.Equal(t, http.StatusOK, posProbe.Code, posProbe.Body.String())
	var posResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(posProbe.Body.Bytes(), &posResp))
	assert.Equal(t, systemoneruntime.ClassSuccess, posResp.Class)
	assert.Equal(t, int32(1), countingBackend.getCalls.Load())
	assert.Equal(t, int32(1), defaultProviderHits.Load())

	// 1. Least-privileged hub-admin PoC case (§F9): custom endpoint + project secret_ref -> 400 at create!
	for _, badEndpoint := range []string{
		attackerSrv.URL,
		"https://attacker.example",
		"http://generativelanguage.googleapis.com",
		"https://user:pw@generativelanguage.googleapis.com",
		"https://generativelanguage.googleapis.com:8443",
		"https://generativelanguage.googleapis.com/v1beta",
		"https://generativelanguage.googleapis.com?foo=1",
		"https://generativelanguage.googleapis.com#frag",
		"https://generativelanguage.googleapis.com.",
		"https://142.250.80.42",
		"https://xn--generativelanguage-123.googleapis.com",
	} {
		rec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
			"name":     "f9-bad-ref",
			"adapter":  geminidecisions.AdapterName,
			"model":    "gemini-decision-1",
			"endpoint": badEndpoint,
			"auth": map[string]any{
				"kind":      "api_key",
				"secretRef": map[string]any{"name": "GITHUB_TOKEN", "scope": "project"},
			},
		})
		require.Equal(t, http.StatusBadRequest, rec.Code, "endpoint=%q body=%s", badEndpoint, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), projSecretCanary)
	}

	// 2. Forced/planted secret_ref row with custom endpoint + hub-admin probe --project -> auth with 0 requests, attacker receives NOTHING!
	require.NoError(t, s.CreateSystemOneInstance(ctx, &store.SystemOneInstance{
		ID:       "99997001-0000-4000-8000-000000000001",
		Name:     "f9-forced-proj-ref",
		Adapter:  geminidecisions.AdapterName,
		Model:    "gemini-decision-1",
		Endpoint: attackerSrv.URL,
		Auth:     store.SystemOneInstanceAuth{Kind: store.SystemOneAuthKindAPIKey, SecretName: "GITHUB_TOKEN"},
		Options:  map[string]string{systemoneprovider.OptionSecretScope: "project"},
		Enabled:  true,
	}))

	binSpec := json.RawMessage(`{"version":1,"questions":[{"id":"q_ok","type":"binary","instructions":"Check probe.text."}]}`)
	attackerHits.Store(0)
	defaultProviderHits.Store(0)
	forcedRefRec := doRequestAsUser(t, srv, superAdmin, http.MethodPost, systemOneAdminPath+"/f9-forced-proj-ref/probe", map[string]any{
		"project": victimProj.ID,
		"spec":    binSpec,
		"probe":   map[string]any{"text": "Exfiltration attempt."},
	})
	require.Equal(t, http.StatusOK, forcedRefRec.Code, forcedRefRec.Body.String())
	var forcedRefResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(forcedRefRec.Body.Bytes(), &forcedRefResp))
	assert.Equal(t, systemoneruntime.ClassFailed, forcedRefResp.Class)
	assert.Equal(t, "auth", forcedRefResp.Reason)
	if forcedRefResp.Evaluation != nil {
		for _, a := range forcedRefResp.Evaluation.Attempts {
			assert.Zero(t, a.Requests)
		}
	}
	assert.Zero(t, attackerHits.Load(), "attacker httptest server must receive 0 requests")
	assert.Zero(t, defaultProviderHits.Load(), "default provider must receive 0 requests")
	assert.NotContains(t, forcedRefRec.Body.String(), projSecretCanary)

	// 3. ADC with custom endpoint -> 400 at create (0 ADC calls, 0 token mints), and forced row -> auth, 0 requests, 0 token mints!
	adcProviderCalls.Store(0)
	adcTokenMints.Store(0)
	for _, badEndpoint := range []string{
		attackerSrv.URL,
		"https://attacker.example",
		"https://generativelanguage.googleapis.com:8443",
		"https://generativelanguage.googleapis.com/v1beta",
	} {
		rec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
			"name":     "f9-bad-adc",
			"adapter":  geminidecisions.AdapterName,
			"model":    "gemini-decision-1",
			"endpoint": badEndpoint,
			"auth":     map[string]any{"kind": "google_access_token", "scopes": []string{testADCScope}},
		})
		require.Equal(t, http.StatusBadRequest, rec.Code, "ADC endpoint=%q body=%s", badEndpoint, rec.Body.String())
	}
	assert.Zero(t, adcProviderCalls.Load())
	assert.Zero(t, adcTokenMints.Load())

	require.NoError(t, s.CreateSystemOneInstance(ctx, &store.SystemOneInstance{
		ID:       "99997002-0000-4000-8000-000000000002",
		Name:     "f9-forced-adc",
		Adapter:  geminidecisions.AdapterName,
		Model:    "gemini-decision-1",
		Endpoint: attackerSrv.URL,
		Auth: store.SystemOneInstanceAuth{
			Kind:   store.SystemOneAuthKindGoogleAccessToken,
			Scopes: []string{testADCScope},
		},
		Enabled: true,
	}))
	attackerHits.Store(0)
	forcedADCRec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, systemOneAdminPath+"/f9-forced-adc/probe", map[string]any{
		"spec":  binSpec,
		"probe": map[string]any{"text": "Forced ADC custom endpoint."},
	})
	require.Equal(t, http.StatusOK, forcedADCRec.Code)
	var forcedADCResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(forcedADCRec.Body.Bytes(), &forcedADCResp))
	assert.Equal(t, systemoneruntime.ClassFailed, forcedADCResp.Class)
	assert.Equal(t, "auth", forcedADCResp.Reason)
	assert.Zero(t, attackerHits.Load(), "attacker server must receive 0 requests")
	assert.Zero(t, adcProviderCalls.Load(), "forced ADC row with custom endpoint must perform 0 ADC calls")
	assert.Zero(t, adcTokenMints.Load(), "forced ADC row with custom endpoint must mint 0 tokens")

	// 4. Adapter without EndpointDefaulter cannot use secret_ref or ADC -> 400 at create, forced row -> auth with 0 requests!
	noDefReg := systemoneprovider.NewRegistry()
	require.NoError(t, geminidecisions.Register(noDefReg))
	require.NoError(t, noDefReg.Register(jevwire.NewTypeSafe()))
	require.NoError(t, noDefReg.Register(noEndpointDefaulterTestAdapter{}))
	srv.mu.Lock()
	srv.systemOneRegistry = noDefReg
	srv.systemOneRuntime = systemoneruntime.New(
		systemoneruntime.Config{
			Registry:    noDefReg,
			Instances:   &hubSystemOneInstanceSource{srv: srv},
			Credentials: (&hubSystemOneCredentialSource{srv: srv}).APIKeyToken,
			TokenSource: (&hubSystemOneCredentialSource{srv: srv}).GoogleAccessToken,
			Logger:      slog.New(slog.NewJSONHandler(&runtimeLogBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		},
		systemoneruntime.WithEgressOptionsForTesting(egOpts...),
	)
	srv.mu.Unlock()

	for _, ep := range []string{"", "https://custom-nodefault.example"} {
		ndRefRec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
			"name":     "f9-nd-ref",
			"adapter":  "no_defaulter_test",
			"model":    "m1",
			"endpoint": ep,
			"auth": map[string]any{
				"kind":      "api_key",
				"secretRef": map[string]any{"name": "GEMINI_API_KEY", "scope": "hub"},
			},
		})
		require.Equal(t, http.StatusBadRequest, ndRefRec.Code, "no-defaulter secret_ref ep=%q: %s", ep, ndRefRec.Body.String())

		ndADCRec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
			"name":     "f9-nd-adc",
			"adapter":  "no_defaulter_test",
			"model":    "m1",
			"endpoint": ep,
			"auth":     map[string]any{"kind": "google_access_token", "scopes": []string{testADCScope}},
		})
		require.Equal(t, http.StatusBadRequest, ndADCRec.Code, "no-defaulter ADC ep=%q: %s", ep, ndADCRec.Body.String())
	}

	require.NoError(t, s.CreateSystemOneInstance(ctx, &store.SystemOneInstance{
		ID:       "99997003-0000-4000-8000-000000000003",
		Name:     "f9-forced-nd-ref",
		Adapter:  "no_defaulter_test",
		Model:    "m1",
		Endpoint: "https://custom-nodefault.example",
		Auth:     store.SystemOneInstanceAuth{Kind: store.SystemOneAuthKindAPIKey, SecretName: "GEMINI_API_KEY"},
		Options:  map[string]string{systemoneprovider.OptionSecretScope: "hub"},
		Enabled:  true,
	}))
	forcedTSRec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, systemOneAdminPath+"/f9-forced-nd-ref/probe", map[string]any{
		"spec":  binSpec,
		"probe": map[string]any{"text": "Forced no-defaulter secret_ref."},
	})
	require.Equal(t, http.StatusOK, forcedTSRec.Code)
	var forcedTSResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(forcedTSRec.Body.Bytes(), &forcedTSResp))
	assert.Equal(t, systemoneruntime.ClassFailed, forcedTSResp.Class)
	assert.Equal(t, "auth", forcedTSResp.Reason)

	// 5. Omitted and canonical default forms accepted for both secret_ref and ADC, normalized to canonical default.
	for i, goodEp := range []string{"", geminidecisions.DefaultEndpoint, "HTTPS://Generativelanguage.googleapis.com:443/"} {
		refName := fmt.Sprintf("f9-good-ref-%d", i)
		refRec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
			"name":     refName,
			"adapter":  geminidecisions.AdapterName,
			"model":    "gemini-decision-1",
			"endpoint": goodEp,
			"auth": map[string]any{
				"kind":      "api_key",
				"secretRef": map[string]any{"name": "GEMINI_API_KEY", "scope": "hub"},
			},
		})
		require.Equal(t, http.StatusCreated, refRec.Code, refRec.Body.String())
		var refCreated SystemOneInstanceResponse
		require.NoError(t, json.Unmarshal(refRec.Body.Bytes(), &refCreated))
		assert.Equal(t, geminidecisions.DefaultEndpoint, refCreated.Endpoint)

		adcName := fmt.Sprintf("f9-good-adc-%d", i)
		adcRec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
			"name":     adcName,
			"adapter":  geminidecisions.AdapterName,
			"model":    "gemini-decision-1",
			"endpoint": goodEp,
			"auth":     map[string]any{"kind": "google_access_token", "scopes": []string{testADCScope}},
		})
		require.Equal(t, http.StatusCreated, adcRec.Code, adcRec.Body.String())
		var adcCreated SystemOneInstanceResponse
		require.NoError(t, json.Unmarshal(adcRec.Body.Bytes(), &adcCreated))
		assert.Equal(t, geminidecisions.DefaultEndpoint, adcCreated.Endpoint)
	}

	// 6. Owned-credential custom-endpoint regression still works!
	ownedRec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":     "f9-owned-custom-ep",
		"adapter":  geminidecisions.AdapterName,
		"model":    "gemini-decision-1",
		"endpoint": attackerSrv.URL,
		"auth":     map[string]any{"kind": "api_key", "apiKey": ownedKeyCanary},
	})
	require.Equal(t, http.StatusCreated, ownedRec.Code, ownedRec.Body.String())
	attackerHits.Store(0)
	ownedProbeRec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, systemOneAdminPath+"/f9-owned-custom-ep/probe", map[string]any{
		"spec":  binSpec,
		"probe": map[string]any{"text": "Owned custom endpoint check."},
	})
	require.Equal(t, http.StatusOK, ownedProbeRec.Code)
	var ownedProbeResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(ownedProbeRec.Body.Bytes(), &ownedProbeResp))
	assert.Equal(t, systemoneruntime.ClassSuccess, ownedProbeResp.Class)
	assert.Equal(t, int32(1), attackerHits.Load())

	allLogs := auditBuf.String() + "\n" + runtimeLogBuf.String()
	for _, canary := range []string{projSecretCanary, hubSecretCanary, ownedKeyCanary, adcCanaryToken} {
		assert.NotContains(t, allLogs, canary)
	}
}

type noEndpointDefaulterTestAdapter struct{}

func (noEndpointDefaulterTestAdapter) Name() string    { return "no_defaulter_test" }
func (noEndpointDefaulterTestAdapter) Version() string { return "0.1.0" }
func (noEndpointDefaulterTestAdapter) Capabilities(systemoneprovider.InstanceView) (systemoneprovider.Capabilities, error) {
	return systemoneprovider.Capabilities{
		Primitives: map[string]systemoneprovider.PrimitiveLimits{
			spec.PrimitiveBinary: {Supported: true, DistributionCompleteness: systemoneprovider.CompletenessComplete},
		},
		MaxQuestionsPerRequest: 16,
		MaxInputTokens:         8192,
		Truncation:             systemoneprovider.TruncationNever,
		ReturnsDistribution:    true,
		SideEffectFree:         true,
		AuthScheme:             systemoneprovider.AuthBearer,
		SupportedAuth: map[string]systemoneprovider.AuthScheme{
			systemoneprovider.InstanceAuthAPIKey:                systemoneprovider.AuthBearer,
			string(systemoneprovider.AuthKindGoogleAccessToken): {Kind: systemoneprovider.AuthKindGoogleAccessToken},
		},
	}, nil
}
func (noEndpointDefaulterTestAdapter) Validate(systemoneprovider.InstanceView, systemoneprovider.RuleSpec) error {
	return nil
}
func (noEndpointDefaulterTestAdapter) Invoke(context.Context, systemoneprovider.Transport, systemoneprovider.InstanceView, systemoneprovider.ModelRequest) (systemoneprovider.ModelResult, error) {
	return systemoneprovider.ModelResult{}, nil
}

type countingSecretBackendWrapper struct {
	secret.SecretBackend
	getCalls atomic.Int32
}

func (w *countingSecretBackendWrapper) Get(ctx context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
	w.getCalls.Add(1)
	return w.SecretBackend.Get(ctx, name, scope, scopeID)
}

func TestSystemOneInstances_Item8_ProviderPinnedSecretNames(t *testing.T) {
	ctx := context.Background()
	srv, s, localBackend := newSystemOneTestServer(t, true)
	countingBackend := &countingSecretBackendWrapper{SecretBackend: localBackend}
	srv.SetSecretBackend(countingBackend)

	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-f10-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)
	hubAdmin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-f10-hubadmin", store.SystemRoleHubAdmin))
	require.NoError(t, err)

	var providerHits atomic.Int32
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"q_ok":{"type":"noul","noul":0.91,"confidence":0.95}}}`))
	}))
	t.Cleanup(tlsSrv.Close)

	srv.SetSystemOneRuntimeOptionsForTesting(t,
		systemoneruntime.WithEgressOptionsForTesting(
			testSystemOneEgressOptions(t, tlsSrv)...,
		),
	)

	proj := &store.Project{
		ID:      tid("f10-proj"),
		Name:    "f10-proj",
		Slug:    "f10-proj",
		OwnerID: admin.ID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	const (
		ghCanary  = "CANARY_F10_GITHUB_TOKEN_8414"
		antCanary = "CANARY_F10_ANTHROPIC_KEY_8414"
		gemCanary = "CANARY_F10_GEMINI_KEY_8414"
	)

	for _, seed := range []struct {
		name, val, scope, scopeID string
	}{
		{"GITHUB_TOKEN", ghCanary, store.ScopeProject, proj.ID},
		{"GITHUB_TOKEN", ghCanary, store.ScopeHub, srv.hubID},
		{"ANTHROPIC_API_KEY", antCanary, store.ScopeProject, proj.ID},
		{"ANTHROPIC_API_KEY", antCanary, store.ScopeHub, srv.hubID},
		{"GEMINI_API_KEY", gemCanary, store.ScopeHub, srv.hubID},
	} {
		_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
			Name:       seed.name,
			Value:      seed.val,
			SecretType: store.SecretTypeEnvironment,
			Target:     seed.name,
			Scope:      seed.scope,
			ScopeID:    seed.scopeID,
			CreatedBy:  admin.ID,
			UpdatedBy:  admin.ID,
		})
		require.NoError(t, err)
	}
	_, err = srv.ensureSigningKey(ctx, SecretKeyUserSigningKey, nil)
	require.NoError(t, err)
	_, err = srv.ensureSigningKey(ctx, SecretKeyAgentSigningKey, nil)
	require.NoError(t, err)

	// 1. Disallowed secret names on gemini_decisions (even with canonical Gemini endpoint) -> 400 at create,
	//    and crafted rows across hub, project, and project_or_hub -> auth with 0 provider requests and 0 SecretBackend.Get calls!
	rejectedNames := []string{
		"GITHUB_TOKEN",
		"ANTHROPIC_API_KEY",
		"gemini_api_key",
		"GEMINI_API_KEY_2",
		SecretKeyUserSigningKey,
		SecretKeyAgentSigningKey,
		"systemone.instance.00000000-0000-4000-8000-000000000088",
	}
	for i, badName := range rejectedNames {
		for j, sc := range []string{"hub", "project", "project_or_hub"} {
			rec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
				"name":     fmt.Sprintf("f10-bad-%d-%d", i, j),
				"adapter":  geminidecisions.AdapterName,
				"model":    "gemini-decision-1",
				"endpoint": geminidecisions.DefaultEndpoint,
				"auth": map[string]any{
					"kind":      "api_key",
					"secretRef": map[string]any{"name": badName, "scope": sc},
				},
			})
			require.Equal(t, http.StatusBadRequest, rec.Code, "name=%q scope=%s body=%s", badName, sc, rec.Body.String())

			craftedName := fmt.Sprintf("f10-crafted-%d-%d", i, j)
			require.NoError(t, s.CreateSystemOneInstance(ctx, &store.SystemOneInstance{
				ID:       fmt.Sprintf("999980%02d-0000-4000-8000-0000000000%02d", i, j),
				Name:     craftedName,
				Adapter:  geminidecisions.AdapterName,
				Model:    "gemini-decision-1",
				Endpoint: geminidecisions.DefaultEndpoint,
				Auth:     store.SystemOneInstanceAuth{Kind: store.SystemOneAuthKindAPIKey, SecretName: badName},
				Options:  map[string]string{systemoneprovider.OptionSecretScope: sc},
				Enabled:  true,
			}))

			binSpec := json.RawMessage(`{"version":1,"questions":[{"id":"q_ok","type":"binary","instructions":"Check."}]}`)

			// On the first crafted row, also verify that when the instance is NOT
			// allowlisted for proj, the D1 allowlist gate rejects with
			// no_eligible_instance and 0 SecretBackend.Get calls.
			if i == 0 && j == 0 {
				providerHits.Store(0)
				countingBackend.getCalls.Store(0)
				unlistedRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/"+craftedName+"/probe", map[string]any{
					"project": proj.ID,
					"spec":    binSpec,
					"probe":   map[string]any{"text": "Unlisted project check."},
				})
				require.Equal(t, http.StatusOK, unlistedRec.Code)
				var unlistedResp ProbeSystemOneInstanceResponse
				require.NoError(t, json.Unmarshal(unlistedRec.Body.Bytes(), &unlistedResp))
				assert.Equal(t, systemoneruntime.ClassFailed, unlistedResp.Class)
				assert.Equal(t, string(systemoneprovider.FailNoEligibleInstance), unlistedResp.Reason)
				assert.Zero(t, providerHits.Load())
				assert.Zero(t, countingBackend.getCalls.Load())
			}

			// Allowlist craftedName on proj so probe exercises G4 pinned-name refusal.
			_, _, err := s.SetProjectSystemOneInstances(ctx, proj.ID, []string{craftedName})
			require.NoError(t, err)

			providerHits.Store(0)
			countingBackend.getCalls.Store(0)
			pRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/"+craftedName+"/probe", map[string]any{
				"project": proj.ID,
				"spec":    binSpec,
				"probe":   map[string]any{"text": "Disallowed name check."},
			})
			require.Equal(t, http.StatusOK, pRec.Code)
			var pResp ProbeSystemOneInstanceResponse
			require.NoError(t, json.Unmarshal(pRec.Body.Bytes(), &pResp))
			assert.Equal(t, systemoneruntime.ClassFailed, pResp.Class, "%s/%s", badName, sc)
			if strings.Contains(badName, ".") {
				assert.Equal(t, "invalid_request", pResp.Reason)
			} else {
				assert.Equal(t, "auth", pResp.Reason, "%s/%s", badName, sc)
			}
			assert.Zero(t, providerHits.Load(), "provider must receive 0 requests for %s/%s", badName, sc)
			assert.Zero(t, countingBackend.getCalls.Load(), "SecretBackend.Get must never be called for %s/%s", badName, sc)
		}
	}

	// 2. Adapter without SecretNamePolicy (typesafe) cannot use secret_ref -> 400 at create, auth + 0 Get calls on crafted row.
	tsCreate := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":     "f10-ts-ref",
		"adapter":  jevwire.AdapterNameTypeSafe,
		"model":    "jev-1.14.0",
		"endpoint": "https://api.typesafe.ai",
		"auth": map[string]any{
			"kind":      "api_key",
			"secretRef": map[string]any{"name": "GEMINI_API_KEY", "scope": "hub"},
		},
	})
	require.Equal(t, http.StatusBadRequest, tsCreate.Code, tsCreate.Body.String())

	require.NoError(t, s.CreateSystemOneInstance(ctx, &store.SystemOneInstance{
		ID:       "99998099-0000-4000-8000-000000000099",
		Name:     "f10-crafted-ts",
		Adapter:  jevwire.AdapterNameTypeSafe,
		Model:    "jev-1.14.0",
		Endpoint: "https://api.typesafe.ai",
		Auth:     store.SystemOneInstanceAuth{Kind: store.SystemOneAuthKindAPIKey, SecretName: "GEMINI_API_KEY"},
		Options:  map[string]string{systemoneprovider.OptionSecretScope: "hub"},
		Enabled:  true,
	}))
	providerHits.Store(0)
	countingBackend.getCalls.Store(0)
	tsProbe := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, systemOneAdminPath+"/f10-crafted-ts/probe", map[string]any{
		"spec":  json.RawMessage(`{"version":1,"questions":[{"id":"q_ok","type":"binary","instructions":"Check."}]}`),
		"probe": map[string]any{"text": "TypeSafe crafted ref."},
	})
	require.Equal(t, http.StatusOK, tsProbe.Code)
	var tsProbeResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(tsProbe.Body.Bytes(), &tsProbeResp))
	assert.Equal(t, systemoneruntime.ClassFailed, tsProbeResp.Class)
	assert.Equal(t, "auth", tsProbeResp.Reason)
	assert.Zero(t, providerHits.Load())
	assert.Zero(t, countingBackend.getCalls.Load())

	// 3. Allowed GEMINI_API_KEY on gemini_decisions succeeds at create and probe!
	okCreate := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "f10-gem-ok",
		"adapter": geminidecisions.AdapterName,
		"model":   "gemini-decision-1",
		"auth": map[string]any{
			"kind":      "api_key",
			"secretRef": map[string]any{"name": "GEMINI_API_KEY", "scope": "hub"},
		},
	})
	require.Equal(t, http.StatusCreated, okCreate.Code, okCreate.Body.String())
	providerHits.Store(0)
	okProbe := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, systemOneAdminPath+"/f10-gem-ok/probe", map[string]any{
		"spec":  json.RawMessage(`{"version":1,"questions":[{"id":"q_ok","type":"binary","instructions":"Check."}]}`),
		"probe": map[string]any{"text": "Allowed GEMINI_API_KEY."},
	})
	require.Equal(t, http.StatusOK, okProbe.Code)
	var okProbeResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(okProbe.Body.Bytes(), &okProbeResp))
	assert.Equal(t, systemoneruntime.ClassSuccess, okProbeResp.Class)
	assert.Equal(t, int32(1), providerHits.Load())
}

type secondPolicyTestAdapter struct {
	noEndpointDefaulterTestAdapter
}

func (secondPolicyTestAdapter) Name() string            { return "second_policy_test" }
func (secondPolicyTestAdapter) DefaultEndpoint() string { return geminidecisions.DefaultEndpoint }
func (secondPolicyTestAdapter) AllowedSecretNames() []string {
	return []string{"SECOND_PROVIDER_API_KEY"}
}

func TestSystemOneInstances_Item9_OperatorConfiguredExtraSecretNames(t *testing.T) {
	ctx := context.Background()
	st := newBareTestStore(t)
	localBackend := secret.NewLocalBackend(st, "test-hub-id", "test-secret-signing-key")
	countingBackend := &countingSecretBackendWrapper{SecretBackend: localBackend}

	var providerHits atomic.Int32
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"q_ok":{"type":"noul","noul":0.89,"confidence":0.94}}}`))
	}))
	t.Cleanup(tlsSrv.Close)

	customReg := systemoneprovider.NewRegistry()
	require.NoError(t, customReg.Register(geminidecisions.New()))
	require.NoError(t, customReg.Register(jevwire.NewTypeSafe()))
	require.NoError(t, customReg.Register(secondPolicyTestAdapter{}))

	// Verify startup snapshot warnings directly for unregistered adapters,
	// adapters without SecretNamePolicy (typesafe), and invalid/reserved/infra extras.
	var warnBuf bytes.Buffer
	warnLogger := slog.New(slog.NewTextHandler(&warnBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	configuredExtras := map[string][]string{
		geminidecisions.AdapterName: {
			"GEMINI_API_KEY_BILLING_B",
			"1invalid-name",
			"systemone.instance.00000000-0000-4000-8000-000000000001",
			"SystemOne_Instance_ sneaky",
			SecretKeyUserSigningKey,
		},
		jevwire.AdapterNameTypeSafe: {"TYPESAFE_EXTRA_KEY"},
		"unregistered_adapter":      {"UNREGISTERED_EXTRA_KEY"},
	}
	snap := snapshotSystemOneExtraSecretNames(ServerConfig{
		StartupServerSettings: &config.V1ServerConfig{
			Hub: &config.V1ServerHubConfig{
				SystemOne: &config.V1ServerHubSystemOneConfig{SecretNames: configuredExtras},
			},
		},
	}, customReg, warnLogger)
	assert.Equal(t, map[string][]string{
		geminidecisions.AdapterName: {"GEMINI_API_KEY_BILLING_B"},
	}, snap)
	warnings := warnBuf.String()
	assert.Contains(t, warnings, "unregistered_adapter")
	assert.Contains(t, warnings, jevwire.AdapterNameTypeSafe)
	assert.Contains(t, warnings, "1invalid-name")
	assert.Contains(t, warnings, "systemone.instance.")
	assert.Contains(t, warnings, SecretKeyUserSigningKey)

	buildHubOnStore := func(t *testing.T, secretNames map[string][]string) *Server {
		t.Helper()
		cfg := DefaultServerConfig()
		cfg.DevAuthToken = testDevToken
		cfg.DevUserConfig = DevUserConfig{Username: "dev", DisplayName: "Development User", Email: "dev@localhost"}
		cfg.StartupServerSettings = &config.V1ServerConfig{
			Hub: &config.V1ServerHubConfig{
				SystemOne: &config.V1ServerHubSystemOneConfig{
					SecretNames: secretNames,
				},
			},
		}
		h, err := newTestHubServer(t, cfg, st)
		require.NoError(t, err)
		h.SetHubID("test-hub-id")
		h.systemOneRegistry = customReg
		h.mu.Lock()
		h.systemOneExtraSecretNamesSnapshotted = false
		h.mu.Unlock()
		h.initSystemOneRuntime()
		h.SetSecretBackend(countingBackend)
		h.SetSystemOneADCForTesting(t, func(context.Context, []string) (*SystemOneADCCredentials, error) {
			return nil, egress.ErrAuth
		})
		h.SetSystemOneRuntimeOptionsForTesting(t,
			systemoneruntime.WithEgressOptionsForTesting(
				testSystemOneEgressOptions(t, tlsSrv)...,
			),
		)
		ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())
		_, err = ops.Refresh(ctx)
		require.NoError(t, err)
		doc := fmt.Sprintf(`{"overrides":{%q:true}}`, experiments.SystemOne)
		_, err = ops.Update(ctx, "experiments", json.RawMessage(doc), "test", ops.SectionRevision("experiments"), "managed")
		require.NoError(t, err)
		h.SetOperationalSettings(ops)
		return h
	}

	// Start Hub #1 with GEMINI_API_KEY_BILLING_B configured for gemini_decisions
	// (plus ignored invalid/reserved/no-policy entries).
	srv1 := buildHubOnStore(t, configuredExtras)
	admin, err := st.GetUser(ctx, hubConfigTokenUser(t, st, "s1-item9-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)
	hubAdmin, err := st.GetUser(ctx, hubConfigTokenUser(t, st, "s1-item9-hubadmin", store.SystemRoleHubAdmin))
	require.NoError(t, err)

	const billingBCanary = "CANARY_ITEM9_BILLING_B_KEY_8414"
	_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "GEMINI_API_KEY_BILLING_B",
		Value:      billingBCanary,
		SecretType: store.SecretTypeEnvironment,
		Target:     "GEMINI_API_KEY_BILLING_B",
		Scope:      store.ScopeHub,
		ScopeID:    "test-hub-id",
		CreatedBy:  admin.ID,
		UpdatedBy:  admin.ID,
	})
	require.NoError(t, err)
	_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "TYPESAFE_EXTRA_KEY",
		Value:      "CANARY_ITEM9_TYPESAFE_EXTRA",
		SecretType: store.SecretTypeEnvironment,
		Target:     "TYPESAFE_EXTRA_KEY",
		Scope:      store.ScopeHub,
		ScopeID:    "test-hub-id",
		CreatedBy:  admin.ID,
		UpdatedBy:  admin.ID,
	})
	require.NoError(t, err)

	specDoc := json.RawMessage(`{"version":1,"questions":[{"id":"q_ok","type":"binary","instructions":"Check."}]}`)

	// 1. Positive: configured extra GEMINI_API_KEY_BILLING_B works for gemini_decisions!
	createRec := doRequestAsUser(t, srv1, hubAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "item9-gem-billing-b",
		"adapter": geminidecisions.AdapterName,
		"model":   "gemini-decision-1",
		"auth": map[string]any{
			"kind":      "api_key",
			"secretRef": map[string]any{"name": "GEMINI_API_KEY_BILLING_B", "scope": "hub"},
		},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())

	providerHits.Store(0)
	probe1 := doRequestAsUser(t, srv1, hubAdmin, http.MethodPost, systemOneAdminPath+"/item9-gem-billing-b/probe", map[string]any{
		"spec":  specDoc,
		"probe": map[string]any{"text": "Billing B check."},
	})
	require.Equal(t, http.StatusOK, probe1.Code, probe1.Body.String())
	var probe1Resp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(probe1.Body.Bytes(), &probe1Resp))
	assert.Equal(t, systemoneruntime.ClassSuccess, probe1Resp.Class)
	assert.Equal(t, int32(1), providerHits.Load())

	// 2. Negative: extra for gemini_decisions does NOT apply to second_policy_test!
	crossAdapterRec := doRequestAsUser(t, srv1, hubAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "item9-cross-adapter",
		"adapter": "second_policy_test",
		"model":   "m1",
		"auth": map[string]any{
			"kind":      "api_key",
			"secretRef": map[string]any{"name": "GEMINI_API_KEY_BILLING_B", "scope": "hub"},
		},
	})
	require.Equal(t, http.StatusBadRequest, crossAdapterRec.Code, crossAdapterRec.Body.String())

	// 3. Negative: adapter without SecretNamePolicy (typesafe) + configured extra is STILL refused!
	tsExtraRec := doRequestAsUser(t, srv1, hubAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":     "item9-ts-extra",
		"adapter":  jevwire.AdapterNameTypeSafe,
		"model":    "jev-1.14.0",
		"endpoint": "https://api.typesafe.ai",
		"auth": map[string]any{
			"kind":      "api_key",
			"secretRef": map[string]any{"name": "TYPESAFE_EXTRA_KEY", "scope": "hub"},
		},
	})
	require.Equal(t, http.StatusBadRequest, tsExtraRec.Code, tsExtraRec.Body.String())

	// 4. Negative: reserved / Hub-infrastructure extras configured in settings were ignored and are refused at create!
	for _, badExtra := range []string{SecretKeyUserSigningKey, "systemone.instance.00000000-0000-4000-8000-000000000001"} {
		badRec := doRequestAsUser(t, srv1, hubAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
			"name":    "item9-bad-extra",
			"adapter": geminidecisions.AdapterName,
			"model":   "gemini-decision-1",
			"auth": map[string]any{
				"kind":      "api_key",
				"secretRef": map[string]any{"name": badExtra, "scope": "hub"},
			},
		})
		require.Equal(t, http.StatusBadRequest, badRec.Code, "bad extra %q must be refused", badExtra)
	}

	// 5. Negative: instance options cannot supply or expand permitted secret names!
	optExpandRec := doRequestAsUser(t, srv1, hubAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "item9-opt-expand",
		"adapter": geminidecisions.AdapterName,
		"model":   "gemini-decision-1",
		"options": map[string]string{
			"secret_names":         "UNAPPROVED_KEY",
			"allowed_secret_names": "UNAPPROVED_KEY",
		},
		"auth": map[string]any{
			"kind":      "api_key",
			"secretRef": map[string]any{"name": "UNAPPROVED_KEY", "scope": "hub"},
		},
	})
	require.Equal(t, http.StatusBadRequest, optExpandRec.Code, optExpandRec.Body.String())

	// 6. Immutable snapshot before restart: mutating StartupServerSettings in memory
	// without restarting the Hub does NOT revoke GEMINI_API_KEY_BILLING_B yet.
	srv1.config.StartupServerSettings.Hub.SystemOne.SecretNames = nil
	providerHits.Store(0)
	probeBeforeRestart := doRequestAsUser(t, srv1, hubAdmin, http.MethodPost, systemOneAdminPath+"/item9-gem-billing-b/probe", map[string]any{
		"spec":  specDoc,
		"probe": map[string]any{"text": "Before restart check."},
	})
	require.Equal(t, http.StatusOK, probeBeforeRestart.Code)
	var beforeRestartResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(probeBeforeRestart.Body.Bytes(), &beforeRestartResp))
	assert.Equal(t, systemoneruntime.ClassSuccess, beforeRestartResp.Class)
	assert.Equal(t, int32(1), providerHits.Load())

	// 7. Removal + Hub restart fails closed: start Hub #2 against the same SQLite
	// store and secret backend with GEMINI_API_KEY_BILLING_B removed -> existing
	// instance fails closed with class=failed, reason="auth", 0 provider requests,
	// and 0 SecretBackend.Get calls!
	srv2 := buildHubOnStore(t, nil)
	providerHits.Store(0)
	countingBackend.getCalls.Store(0)
	probeAfterRestart := doRequestAsUser(t, srv2, hubAdmin, http.MethodPost, systemOneAdminPath+"/item9-gem-billing-b/probe", map[string]any{
		"spec":  specDoc,
		"probe": map[string]any{"text": "After restart removal check."},
	})
	require.Equal(t, http.StatusOK, probeAfterRestart.Code, probeAfterRestart.Body.String())
	var afterRestartResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(probeAfterRestart.Body.Bytes(), &afterRestartResp))
	assert.Equal(t, systemoneruntime.ClassFailed, afterRestartResp.Class)
	assert.Equal(t, "auth", afterRestartResp.Reason)
	assert.Zero(t, providerHits.Load(), "removed extra after Hub restart must make 0 provider requests")
	assert.Zero(t, countingBackend.getCalls.Load(), "removed extra after Hub restart must fail pre-lookup with 0 SecretBackend.Get calls")
	assert.NotContains(t, probeAfterRestart.Body.String(), billingBCanary)
}

type secretReadSpyStore struct {
	store.Store
	projectReads atomic.Int32
	hubReads     atomic.Int32
}

func (w *secretReadSpyStore) ListSecrets(ctx context.Context, filter store.SecretFilter) ([]store.Secret, error) {
	if filter.Scope == store.ScopeProject || filter.Scope == "" {
		w.projectReads.Add(1)
	}
	if filter.Scope == store.ScopeHub || filter.Scope == "" {
		w.hubReads.Add(1)
	}
	return w.Store.ListSecrets(ctx, filter)
}

func (w *secretReadSpyStore) GetSecret(ctx context.Context, key, scope, scopeID string) (*store.Secret, error) {
	if scope == store.ScopeProject {
		w.projectReads.Add(1)
	}
	if scope == store.ScopeHub {
		w.hubReads.Add(1)
	}
	return w.Store.GetSecret(ctx, key, scope, scopeID)
}

type secretReadSpyBackend struct {
	secret.SecretBackend
	projectReads atomic.Int32
	hubReads     atomic.Int32
}

func (w *secretReadSpyBackend) Get(ctx context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
	if scope == store.ScopeProject {
		w.projectReads.Add(1)
	}
	if scope == store.ScopeHub {
		w.hubReads.Add(1)
	}
	return w.SecretBackend.Get(ctx, name, scope, scopeID)
}

func (w *secretReadSpyBackend) GetMeta(ctx context.Context, name, scope, scopeID string) (*secret.SecretMeta, error) {
	if scope == store.ScopeProject {
		w.projectReads.Add(1)
	}
	if scope == store.ScopeHub {
		w.hubReads.Add(1)
	}
	return w.SecretBackend.GetMeta(ctx, name, scope, scopeID)
}

// TestSystemOneInstances_N1_CreateDoesNotScanProjectSecrets (Hub N1 / §F11):
// Proves global instance creation never reads or scans project-scope secrets or
// metadata, only validates the Hub row when scope is hub/project_or_hub, allows
// missing Hub rows at create, and enforces bad project rows solely at resolution
// time per evaluating project with no Hub fallback.
func TestSystemOneInstances_N1_CreateDoesNotScanProjectSecrets(t *testing.T) {
	ctx := context.Background()
	srv, s, localBackend := newSystemOneTestServer(t, true)
	spyStore, _ := installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *secretReadSpyStore {
		return &secretReadSpyStore{Store: inner}
	})
	spyBackend := &secretReadSpyBackend{SecretBackend: localBackend}
	srv.SetSecretBackend(spyBackend)

	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-n1-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)
	projMember, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-n1-member", store.SystemRoleHubMember))
	require.NoError(t, err)

	const (
		hubKeyCanary      = "CANARY_N1_HUB_GEMINI_KEY_8414"
		badProjFileCanary = "CANARY_N1_BAD_PROJECT_FILE_SECRET_8414"
	)

	var (
		providerHits atomic.Int32
		lastAPIKey   atomic.Value
	)
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerHits.Add(1)
		lastAPIKey.Store(r.Header.Get(geminidecisions.HeaderAPIKey))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"q_ok":{"type":"noul","noul":0.94,"confidence":0.95}}}`))
	}))
	t.Cleanup(tlsSrv.Close)

	reg := srv.SystemOneProviderRegistry()
	srv.mu.Lock()
	srv.systemOneRuntime = systemoneruntime.New(
		systemoneruntime.Config{
			Registry:           reg,
			Instances:          &hubSystemOneInstanceSource{srv: srv},
			Credentials:        (&hubSystemOneCredentialSource{srv: srv}).APIKeyToken,
			TokenSource:        (&hubSystemOneCredentialSource{srv: srv}).GoogleAccessToken,
			AllowedSecretNames: srv.allowedSecretNamesFor,
		},
		systemoneruntime.WithEgressOptionsForTesting(testSystemOneEgressOptions(t, tlsSrv)...),
	)
	srv.mu.Unlock()

	badProj := &store.Project{
		ID:      tid("n1-bad-proj"),
		Name:    "n1-bad-proj",
		Slug:    "n1-bad-proj",
		OwnerID: projMember.ID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	cleanProj := &store.Project{
		ID:      tid("n1-clean-proj"),
		Name:    "n1-clean-proj",
		Slug:    "n1-clean-proj",
		OwnerID: admin.ID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, badProj))
	require.NoError(t, s.CreateProject(ctx, cleanProj))

	resetSpy := func() {
		spyStore.projectReads.Store(0)
		spyStore.hubReads.Store(0)
		spyBackend.projectReads.Store(0)
		spyBackend.hubReads.Store(0)
	}

	// 1. Missing Hub row at create is OK for all scopes, with 0 project-scope reads.
	for _, sc := range []string{"hub", "project", "project_or_hub"} {
		resetSpy()
		rec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
			"name":    "n1-missing-hub-" + strings.ReplaceAll(sc, "_", "-"),
			"adapter": geminidecisions.AdapterName,
			"model":   "gemini-2.5-flash",
			"auth": map[string]any{
				"kind":      "api_key",
				"secretRef": map[string]any{"name": "GEMINI_API_KEY", "scope": sc},
			},
		})
		require.Equal(t, http.StatusCreated, rec.Code, "missing Hub row must be OK at create (scope=%s): %s", sc, rec.Body.String())
		assert.Zero(t, spyStore.projectReads.Load(), "create must do 0 project-scope store reads (scope=%s)", sc)
		assert.Zero(t, spyBackend.projectReads.Load(), "create must do 0 project-scope backend reads (scope=%s)", sc)
	}

	// 2. Hub file and internal rows -> create 400 under hub and project_or_hub,
	//    but IGNORED (201) under project; 0 project-scope reads across all scopes.
	for _, badHubType := range []string{store.SecretTypeFile, store.SecretTypeInternal} {
		_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
			Name:       "GEMINI_API_KEY",
			Value:      hubKeyCanary,
			SecretType: badHubType,
			Target:     "GEMINI_API_KEY",
			Scope:      store.ScopeHub,
			ScopeID:    srv.hubID,
			CreatedBy:  admin.ID,
			UpdatedBy:  admin.ID,
		})
		require.NoError(t, err)

		for _, sc := range []string{"hub", "project_or_hub"} {
			resetSpy()
			rec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
				"name":    "n1-bad-hub-" + badHubType + "-" + strings.ReplaceAll(sc, "_", "-"),
				"adapter": geminidecisions.AdapterName,
				"model":   "gemini-2.5-flash",
				"auth": map[string]any{
					"kind":      "api_key",
					"secretRef": map[string]any{"name": "GEMINI_API_KEY", "scope": sc},
				},
			})
			require.Equal(t, http.StatusBadRequest, rec.Code, "Hub %s row must fail create under %s: %s", badHubType, sc, rec.Body.String())
			assert.Zero(t, spyStore.projectReads.Load(), "create must do 0 project-scope store reads (type=%s scope=%s)", badHubType, sc)
			assert.Zero(t, spyBackend.projectReads.Load(), "create must do 0 project-scope backend reads (type=%s scope=%s)", badHubType, sc)
		}

		resetSpy()
		projRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
			"name":    "n1-ignore-hub-" + badHubType + "-proj",
			"adapter": geminidecisions.AdapterName,
			"model":   "gemini-2.5-flash",
			"auth": map[string]any{
				"kind":      "api_key",
				"secretRef": map[string]any{"name": "GEMINI_API_KEY", "scope": "project"},
			},
		})
		require.Equal(t, http.StatusCreated, projRec.Code, "Hub %s row must be ignored at create under project scope: %s", badHubType, projRec.Body.String())
		assert.Zero(t, spyStore.projectReads.Load())
		assert.Zero(t, spyBackend.projectReads.Load())
		assert.Zero(t, spyStore.hubReads.Load(), "project-only create should not read Hub secrets")
		assert.Zero(t, spyBackend.hubReads.Load(), "project-only create should not read Hub secret metadata")
	}

	// 3. Valid Hub variable GEMINI_API_KEY + unrelated project holding file-typed GEMINI_API_KEY:
	//    create returns 201 for both project_or_hub and project with 0 project-scope reads.
	_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "GEMINI_API_KEY",
		Value:      hubKeyCanary,
		SecretType: store.SecretTypeEnvironment,
		Target:     "GEMINI_API_KEY",
		Scope:      store.ScopeHub,
		ScopeID:    srv.hubID,
		CreatedBy:  admin.ID,
		UpdatedBy:  admin.ID,
	})
	require.NoError(t, err)
	_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "GEMINI_API_KEY",
		Value:      badProjFileCanary,
		SecretType: store.SecretTypeFile,
		Target:     "GEMINI_API_KEY",
		Scope:      store.ScopeProject,
		ScopeID:    badProj.ID,
		CreatedBy:  projMember.ID,
		UpdatedBy:  projMember.ID,
	})
	require.NoError(t, err)

	for _, sc := range []string{"project_or_hub", "project"} {
		resetSpy()
		rec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
			"name":    "n1-cross-proj-" + strings.ReplaceAll(sc, "_", "-"),
			"adapter": geminidecisions.AdapterName,
			"model":   "gemini-2.5-flash",
			"auth": map[string]any{
				"kind":      "api_key",
				"secretRef": map[string]any{"name": "GEMINI_API_KEY", "scope": sc},
			},
		})
		require.Equal(t, http.StatusCreated, rec.Code, "unrelated project file secret must NOT block create (scope=%s): %s", sc, rec.Body.String())
		assert.Zero(t, spyStore.projectReads.Load(), "create must do 0 project-scope store reads (scope=%s)", sc)
		assert.Zero(t, spyBackend.projectReads.Load(), "create must do 0 project-scope backend reads (scope=%s)", sc)
	}

	binSpec := json.RawMessage(`{"version":1,"questions":[{"id":"q_ok","type":"binary","instructions":"Check probe.text."}]}`)

	// 4. Evaluation/probe in badProj (holds file-typed GEMINI_API_KEY) -> auth, 0 provider requests, NO Hub fallback.
	for _, sc := range []string{"project_or_hub", "project"} {
		instName := "n1-cross-proj-" + strings.ReplaceAll(sc, "_", "-")
		providerHits.Store(0)
		pRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/"+instName+"/probe", map[string]any{
			"project": badProj.ID,
			"spec":    binSpec,
			"probe":   map[string]any{"text": "Bad project evaluation."},
		})
		require.Equal(t, http.StatusOK, pRec.Code, sc)
		var pResp ProbeSystemOneInstanceResponse
		require.NoError(t, json.Unmarshal(pRec.Body.Bytes(), &pResp))
		assert.Equal(t, systemoneruntime.ClassFailed, pResp.Class, sc)
		assert.Equal(t, "auth", pResp.Reason, sc)
		assert.Zero(t, providerHits.Load(), "bad project must make 0 provider requests and not fall back to Hub (scope=%s)", sc)
		assert.NotContains(t, pRec.Body.String(), badProjFileCanary)
		assert.NotContains(t, pRec.Body.String(), hubKeyCanary)
	}

	// 5. Evaluation/probe in cleanProj (no project row) under project_or_hub falls back to Hub row and succeeds!
	providerHits.Store(0)
	lastAPIKey.Store("")
	cleanRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/n1-cross-proj-project-or-hub/probe", map[string]any{
		"project": cleanProj.ID,
		"spec":    binSpec,
		"probe":   map[string]any{"text": "Clean project fallback to Hub."},
	})
	require.Equal(t, http.StatusOK, cleanRec.Code, cleanRec.Body.String())
	var cleanResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(cleanRec.Body.Bytes(), &cleanResp))
	assert.Equal(t, systemoneruntime.ClassSuccess, cleanResp.Class)
	assert.Equal(t, int32(1), providerHits.Load())
	assert.Equal(t, hubKeyCanary, lastAPIKey.Load())
	assert.NotContains(t, cleanRec.Body.String(), hubKeyCanary)

	// 6. Missing project context is rejected BEFORE any project secret resolution.
	resetSpy()
	providerHits.Store(0)
	missingProjRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/n1-cross-proj-project-or-hub/probe", map[string]any{
		"spec":  binSpec,
		"probe": map[string]any{"text": "Missing project context."},
	})
	require.Equal(t, http.StatusOK, missingProjRec.Code, missingProjRec.Body.String())
	var missingProjResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(missingProjRec.Body.Bytes(), &missingProjResp))
	assert.Equal(t, systemoneruntime.ClassFailed, missingProjResp.Class)
	assert.Equal(t, string(systemoneprovider.FailInvalidRequest), missingProjResp.Reason)
	assert.Zero(t, spyStore.projectReads.Load(), "missing project context must reject before any project store read")
	assert.Zero(t, spyBackend.projectReads.Load(), "missing project context must reject before any project backend read")
	assert.Zero(t, providerHits.Load())
}

// TestSystemOneInstances_N2_HubFileTypeAllowedNameRefused (Hub N2):
// Proves that even with the allowed name GEMINI_API_KEY on the canonical
// default endpoint, a Hub-scope file-typed secret is refused at create (400)
// and on a forced row at resolution (auth, 0 provider hits).
func TestSystemOneInstances_N2_HubFileTypeAllowedNameRefused(t *testing.T) {
	ctx := context.Background()
	srv, s, localBackend := newSystemOneTestServer(t, true)

	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-n2-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)

	const fileSecretCanary = "CANARY_N2_HUB_FILE_GEMINI_KEY_8414"
	var providerHits atomic.Int32
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"q_ok":{"type":"noul","noul":0.94,"confidence":0.95}}}`))
	}))
	t.Cleanup(tlsSrv.Close)

	reg := srv.SystemOneProviderRegistry()
	srv.mu.Lock()
	srv.systemOneRuntime = systemoneruntime.New(
		systemoneruntime.Config{
			Registry:           reg,
			Instances:          &hubSystemOneInstanceSource{srv: srv},
			Credentials:        (&hubSystemOneCredentialSource{srv: srv}).APIKeyToken,
			TokenSource:        (&hubSystemOneCredentialSource{srv: srv}).GoogleAccessToken,
			AllowedSecretNames: srv.allowedSecretNamesFor,
		},
		systemoneruntime.WithEgressOptionsForTesting(testSystemOneEgressOptions(t, tlsSrv)...),
	)
	srv.mu.Unlock()

	_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "GEMINI_API_KEY",
		Value:      fileSecretCanary,
		SecretType: store.SecretTypeFile,
		Target:     "/etc/secret/gemini.key",
		Scope:      store.ScopeHub,
		ScopeID:    srv.hubID,
		CreatedBy:  admin.ID,
		UpdatedBy:  admin.ID,
	})
	require.NoError(t, err)

	for _, sc := range []string{"hub", "project_or_hub"} {
		rec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
			"name":    "n2-file-hub-" + strings.ReplaceAll(sc, "_", "-"),
			"adapter": geminidecisions.AdapterName,
			"model":   "gemini-2.5-flash",
			"auth": map[string]any{
				"kind":      "api_key",
				"secretRef": map[string]any{"name": "GEMINI_API_KEY", "scope": sc},
			},
		})
		require.Equal(t, http.StatusBadRequest, rec.Code, "Hub file GEMINI_API_KEY must fail create (scope=%s): %s", sc, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), fileSecretCanary)
	}

	require.NoError(t, s.CreateSystemOneInstance(ctx, &store.SystemOneInstance{
		ID:       "99998001-0000-4000-8000-000000000001",
		Name:     "n2-forced-hub-file",
		Adapter:  geminidecisions.AdapterName,
		Model:    "gemini-2.5-flash",
		Endpoint: geminidecisions.DefaultEndpoint,
		Auth:     store.SystemOneInstanceAuth{Kind: store.SystemOneAuthKindAPIKey, SecretName: "GEMINI_API_KEY"},
		Options:  map[string]string{systemoneprovider.OptionSecretScope: "hub"},
		Enabled:  true,
	}))

	providerHits.Store(0)
	pRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/n2-forced-hub-file/probe", map[string]any{
		"spec":  json.RawMessage(`{"version":1,"questions":[{"id":"q_ok","type":"binary","instructions":"Check probe.text."}]}`),
		"probe": map[string]any{"text": "Forced hub file secret check."},
	})
	require.Equal(t, http.StatusOK, pRec.Code, pRec.Body.String())
	var pResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(pRec.Body.Bytes(), &pResp))
	assert.Equal(t, systemoneruntime.ClassFailed, pResp.Class)
	assert.Equal(t, "auth", pResp.Reason)
	assert.Zero(t, providerHits.Load(), "forced hub file secret must make 0 provider requests")
	assert.NotContains(t, pRec.Body.String(), fileSecretCanary)
}

// TestSystemOneInstances_L1_ProbeProjectRequiresSecretRead (Security L1):
// Verifies that probing a project- or project_or_hub-scoped named-secret
// instance with --project requires BOTH project.read and project.secret_read on
// the validated project before any secret lookup, ADC mint, or provider I/O —
// even when project_or_hub would fall back to the Hub secret. Hub-scoped refs,
// owned API-key instances, and ADC instances remain unaffected.
func TestSystemOneInstances_L1_ProbeProjectRequiresSecretRead(t *testing.T) {
	ctx := context.Background()
	srv, s, localBackend := newSystemOneTestServer(t, true)
	countingBackend := &countingSecretBackendWrapper{SecretBackend: localBackend}
	srv.SetSecretBackend(countingBackend)

	superAdmin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-l1-super", store.SystemRoleSuperAdmin))
	require.NoError(t, err)
	hubAdmin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-l1-hubadmin", store.SystemRoleHubAdmin))
	require.NoError(t, err)
	readOnlyProber, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-l1-readonly", store.SystemRoleHubMember))
	require.NoError(t, err)
	secretReadProber, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-l1-secretread", store.SystemRoleHubMember))
	require.NoError(t, err)
	projOwner, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-l1-projowner", store.SystemRoleHubMember))
	require.NoError(t, err)

	const (
		projKeyCanary  = "CANARY_L1_PROJECT_GEMINI_KEY_8414"
		hubKeyCanary   = "CANARY_L1_HUB_GEMINI_KEY_8414"
		ownedKeyCanary = "CANARY_L1_OWNED_API_KEY_8414"
		adcTokenCanary = "CANARY_L1_ADC_BEARER_TOKEN_8414"
		testADCScope   = "https://www.googleapis.com/auth/synthetic.l1.scope"
	)

	var (
		providerHits     atomic.Int32
		adcProviderCalls atomic.Int32
		adcTokenMints    atomic.Int32
	)
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"q_ok":{"type":"noul","noul":0.93,"confidence":0.95}}}`))
	}))
	t.Cleanup(tlsSrv.Close)

	srv.SetSystemOneADCForTesting(t, func(_ context.Context, _ []string) (*SystemOneADCCredentials, error) {
		adcProviderCalls.Add(1)
		return &SystemOneADCCredentials{
			CredentialType:      SystemOneADCTypeServiceAccount,
			ServiceAccountEmail: "so-l1-sa@proj.iam.gserviceaccount.com",
			TokenSource: funcTokenSource(func() (*oauth2.Token, error) {
				adcTokenMints.Add(1)
				return &oauth2.Token{AccessToken: adcTokenCanary, Expiry: time.Now().Add(time.Hour)}, nil
			}),
		}, nil
	})

	reg := srv.SystemOneProviderRegistry()
	srv.mu.Lock()
	srv.systemOneRuntime = systemoneruntime.New(
		systemoneruntime.Config{
			Registry:           reg,
			Instances:          &hubSystemOneInstanceSource{srv: srv},
			Credentials:        (&hubSystemOneCredentialSource{srv: srv}).APIKeyToken,
			TokenSource:        (&hubSystemOneCredentialSource{srv: srv}).GoogleAccessToken,
			AllowedSecretNames: srv.allowedSecretNamesFor,
		},
		systemoneruntime.WithEgressOptionsForTesting(testSystemOneEgressOptions(t, tlsSrv)...),
	)
	srv.mu.Unlock()

	projWithSecret := &store.Project{
		ID:      tid("l1-proj-with-secret"),
		Name:    "l1-proj-with-secret",
		Slug:    "l1-proj-with-secret",
		OwnerID: projOwner.ID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	projNoSecret := &store.Project{
		ID:      tid("l1-proj-no-secret"),
		Name:    "l1-proj-no-secret",
		Slug:    "l1-proj-no-secret",
		OwnerID: projOwner.ID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, projWithSecret))
	require.NoError(t, s.CreateProject(ctx, projNoSecret))

	_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "GEMINI_API_KEY",
		Value:      projKeyCanary,
		SecretType: store.SecretTypeEnvironment,
		Target:     "GEMINI_API_KEY",
		Scope:      store.ScopeProject,
		ScopeID:    projWithSecret.ID,
		CreatedBy:  projOwner.ID,
		UpdatedBy:  projOwner.ID,
	})
	require.NoError(t, err)
	_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "GEMINI_API_KEY",
		Value:      hubKeyCanary,
		SecretType: store.SecretTypeEnvironment,
		Target:     "GEMINI_API_KEY",
		Scope:      store.ScopeHub,
		ScopeID:    srv.hubID,
		CreatedBy:  superAdmin.ID,
		UpdatedBy:  superAdmin.ID,
	})
	require.NoError(t, err)

	// Wire roles: both custom probers can execute System One probes.
	// readOnlyProber holds only project.read on both projects;
	// secretReadProber holds both project.read and project.secret_read on both projects.
	hubExecRole, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "l1-s1-prober",
		Description: "System One probe operator",
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"system_one_instance.read", "system_one_instance.execute"},
	})
	require.NoError(t, err)
	projReadOnlyRole, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "l1-proj-read-only",
		Description: "Project read only",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.read"},
	})
	require.NoError(t, err)
	projReadAndSecretRole, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "l1-proj-read-and-secret",
		Description: "Project read and secret_read",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.read", "project.secret_read"},
	})
	require.NoError(t, err)

	for _, u := range []*store.User{readOnlyProber, secretReadProber} {
		ensureHubMembership(ctx, s, u.ID)
		_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: hubExecRole.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      u.ID,
			ScopeType:        store.RoleScopeSystem,
			ScopeID:          "",
			CreatedBy:        superAdmin.ID,
		})
		require.NoError(t, err)
	}
	for _, p := range []*store.Project{projWithSecret, projNoSecret} {
		_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: projReadOnlyRole.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      readOnlyProber.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          p.ID,
			CreatedBy:        superAdmin.ID,
		})
		require.NoError(t, err)
		_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: projReadAndSecretRole.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      secretReadProber.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          p.ID,
			CreatedBy:        superAdmin.ID,
		})
		require.NoError(t, err)
	}

	// Create all 5 instances.
	for _, body := range []map[string]any{
		{
			"name": "l1-proj-ref", "adapter": geminidecisions.AdapterName, "model": "gemini-2.5-flash",
			"auth": map[string]any{"kind": "api_key", "secretRef": map[string]any{"name": "GEMINI_API_KEY", "scope": "project"}},
		},
		{
			"name": "l1-poh-ref", "adapter": geminidecisions.AdapterName, "model": "gemini-2.5-flash",
			"auth": map[string]any{"kind": "api_key", "secretRef": map[string]any{"name": "GEMINI_API_KEY", "scope": "project_or_hub"}},
		},
		{
			"name": "l1-hub-ref", "adapter": geminidecisions.AdapterName, "model": "gemini-2.5-flash",
			"auth": map[string]any{"kind": "api_key", "secretRef": map[string]any{"name": "GEMINI_API_KEY", "scope": "hub"}},
		},
		{
			"name": "l1-owned", "adapter": geminidecisions.AdapterName, "model": "gemini-2.5-flash",
			"auth": map[string]any{"kind": "api_key", "apiKey": ownedKeyCanary},
		},
		{
			"name": "l1-adc", "adapter": geminidecisions.AdapterName, "model": "gemini-2.5-flash",
			"auth": map[string]any{"kind": "google_access_token", "scopes": []string{testADCScope}},
		},
	} {
		rec := doRequestAsUser(t, srv, superAdmin, http.MethodPost, systemOneAdminPath, body)
		require.Equal(t, http.StatusCreated, rec.Code, "create %v: %s", body["name"], rec.Body.String())
	}

	binSpec := json.RawMessage(`{"version":1,"questions":[{"id":"q_ok","type":"binary","instructions":"Check probe.text."}]}`)
	resetCounters := func() {
		countingBackend.getCalls.Store(0)
		adcProviderCalls.Store(0)
		adcTokenMints.Store(0)
		providerHits.Store(0)
	}

	// 1. Negative: principals with project.read but NO project.secret_read (hub-admin and readOnlyProber)
	//    are refused with 403 for project and project_or_hub named-ref probes — including when projNoSecret
	//    has NO project secret (would-be Hub fallback) — with 0 SecretBackend.Get, 0 ADC mints, 0 provider hits.
	for _, actor := range []struct {
		label string
		user  *store.User
	}{
		{"hub_admin", hubAdmin},
		{"project_read_only", readOnlyProber},
	} {
		for _, tc := range []struct {
			instName string
			projID   string
			desc     string
		}{
			{"l1-proj-ref", projWithSecret.ID, "project_scope_with_secret"},
			{"l1-poh-ref", projWithSecret.ID, "project_or_hub_with_secret"},
			{"l1-poh-ref", projNoSecret.ID, "project_or_hub_no_project_secret_fallback"},
		} {
			resetCounters()
			rec := doRequestAsUser(t, srv, actor.user, http.MethodPost, systemOneAdminPath+"/"+tc.instName+"/probe", map[string]any{
				"project": tc.projID,
				"spec":    binSpec,
				"probe":   map[string]any{"text": "Unauthorized secret spend attempt."},
			})
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s/%s: %s", actor.label, tc.desc, rec.Body.String())
			assert.Zero(t, countingBackend.getCalls.Load(), "%s/%s must make 0 SecretBackend.Get calls", actor.label, tc.desc)
			assert.Zero(t, adcProviderCalls.Load(), "%s/%s must make 0 ADC calls", actor.label, tc.desc)
			assert.Zero(t, adcTokenMints.Load(), "%s/%s must mint 0 ADC tokens", actor.label, tc.desc)
			assert.Zero(t, providerHits.Load(), "%s/%s must make 0 provider requests", actor.label, tc.desc)
			for _, canary := range []string{projKeyCanary, hubKeyCanary, ownedKeyCanary, adcTokenCanary} {
				assert.NotContains(t, rec.Body.String(), canary)
			}
		}
	}

	// 2. Positive: principal with BOTH project.read and project.secret_read succeeds across project and project_or_hub
	//    (both project-secret hit and Hub fallback on projNoSecret).
	for _, tc := range []struct {
		instName      string
		projID        string
		wantScopeUsed string
	}{
		{"l1-proj-ref", projWithSecret.ID, "project"},
		{"l1-poh-ref", projWithSecret.ID, "project"},
		{"l1-poh-ref", projNoSecret.ID, "hub"},
	} {
		resetCounters()
		rec := doRequestAsUser(t, srv, secretReadProber, http.MethodPost, systemOneAdminPath+"/"+tc.instName+"/probe", map[string]any{
			"project": tc.projID,
			"spec":    binSpec,
			"probe":   map[string]any{"text": "Authorized probe."},
		})
		require.Equal(t, http.StatusOK, rec.Code, "%s/%s: %s", tc.instName, tc.projID, rec.Body.String())
		var pResp ProbeSystemOneInstanceResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pResp))
		assert.Equal(t, systemoneruntime.ClassSuccess, pResp.Class, "%s/%s", tc.instName, tc.projID)
		require.NotNil(t, pResp.Evaluation.Auth)
		require.NotNil(t, pResp.Evaluation.Auth.SecretRef)
		assert.Equal(t, tc.wantScopeUsed, pResp.Evaluation.Auth.SecretRef.ScopeUsed)
		assert.Equal(t, int32(1), providerHits.Load())
	}

	// 3. Unaffected: hub-scope named ref, owned credential, and ADC probes with project.read only
	//    (both with --project and without --project) behave exactly as before.
	for _, actor := range []struct {
		label string
		user  *store.User
	}{
		{"hub_admin", hubAdmin},
		{"project_read_only", readOnlyProber},
	} {
		for _, instName := range []string{"l1-hub-ref", "l1-owned", "l1-adc"} {
			for _, projArg := range []string{"", projWithSecret.ID} {
				resetCounters()
				body := map[string]any{
					"spec":  binSpec,
					"probe": map[string]any{"text": "Unaffected probe."},
				}
				if projArg != "" {
					body["project"] = projArg
				}
				rec := doRequestAsUser(t, srv, actor.user, http.MethodPost, systemOneAdminPath+"/"+instName+"/probe", body)
				require.Equal(t, http.StatusOK, rec.Code, "%s/%s/proj=%q: %s", actor.label, instName, projArg, rec.Body.String())
				var pResp ProbeSystemOneInstanceResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pResp))
				assert.Equal(t, systemoneruntime.ClassSuccess, pResp.Class, "%s/%s/proj=%q", actor.label, instName, projArg)
				assert.Equal(t, int32(1), providerHits.Load(), "%s/%s/proj=%q", actor.label, instName, projArg)
			}
		}
	}
}

type systemOneCountingFailingDialer struct {
	dials atomic.Int32
}

func (d *systemOneCountingFailingDialer) DialContext(_ context.Context, _, addr string) (net.Conn, error) {
	d.dials.Add(1)
	return nil, fmt.Errorf("outbound provider dial to %s must not occur during GET /adapters", addr)
}

type systemOneCountingSecretBackend struct {
	secret.SecretBackend
	getCalls atomic.Int32
	setCalls atomic.Int32
	delCalls atomic.Int32
}

func (b *systemOneCountingSecretBackend) Get(ctx context.Context, key, scope, scopeID string) (*secret.SecretWithValue, error) {
	b.getCalls.Add(1)
	return b.SecretBackend.Get(ctx, key, scope, scopeID)
}

func (b *systemOneCountingSecretBackend) Set(ctx context.Context, input *secret.SetSecretInput) (bool, *secret.SecretMeta, error) {
	b.setCalls.Add(1)
	return b.SecretBackend.Set(ctx, input)
}

func (b *systemOneCountingSecretBackend) Delete(ctx context.Context, key, scope, scopeID string) error {
	b.delCalls.Add(1)
	return b.SecretBackend.Delete(ctx, key, scope, scopeID)
}

func TestSystemOneAdapters_CapabilitiesAndNoIO(t *testing.T) {
	ctx := context.Background()

	t.Run("experiment off returns 404 to admin", func(t *testing.T) {
		srv, s, _ := newSystemOneTestServer(t, false)
		superAdmin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-adapters-off-super", store.SystemRoleSuperAdmin))
		require.NoError(t, err)

		rec := doRequestAsUser(t, srv, superAdmin, http.MethodGet, systemOneAdaptersPath, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	})

	t.Run("authz and method enforcement", func(t *testing.T) {
		srv, s, _ := newSystemOneTestServer(t, true)
		hubAdmin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-adapters-admin", store.SystemRoleHubAdmin))
		require.NoError(t, err)
		hubMember, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-adapters-member", store.SystemRoleHubMember))
		require.NoError(t, err)
		hubViewer, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-adapters-viewer", store.SystemRoleHubViewer))
		require.NoError(t, err)

		// Unauthenticated -> 401.
		unauthReq := httptest.NewRequest(http.MethodGet, systemOneAdaptersPath, nil)
		unauthRec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(unauthRec, unauthReq)
		assert.Equal(t, http.StatusUnauthorized, unauthRec.Code)

		// Non-admin roles -> 403.
		for _, u := range []*store.User{hubMember, hubViewer} {
			rec := doRequestAsUser(t, srv, u, http.MethodGet, systemOneAdaptersPath, nil)
			assert.Equal(t, http.StatusForbidden, rec.Code, "user %s", u.Email)
		}

		// Unsupported methods -> 405 for admin when experiment is enabled.
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			rec := doRequestAsUser(t, srv, hubAdmin, method, systemOneAdaptersPath, nil)
			assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, "method %s", method)
		}
	})

	t.Run("returns capabilities with operator extras and performs zero I/O, ADC, or secret reads", func(t *testing.T) {
		srv, s, localBackend := newSystemOneTestServer(t, true)
		hubAdmin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-adapters-cap-admin", store.SystemRoleHubAdmin))
		require.NoError(t, err)

		// Seed a Hub secret with a canary value and wrap the backend to assert zero reads.
		const secretCanary = "canary-never-read-by-adapters-endpoint-999"
		_, _, err = localBackend.Set(ctx, &secret.SetSecretInput{
			Name:       "GEMINI_API_KEY",
			Value:      secretCanary,
			SecretType: store.SecretTypeEnvironment,
			Target:     "GEMINI_API_KEY",
			Scope:      store.ScopeHub,
			ScopeID:    srv.hubID,
			CreatedBy:  hubAdmin.ID,
			UpdatedBy:  hubAdmin.ID,
		})
		require.NoError(t, err)

		countingBackend := &systemOneCountingSecretBackend{SecretBackend: localBackend}
		srv.SetSecretBackend(countingBackend)

		// Configure operator extra secret names snapshot (including one invalid name to verify filtering).
		srv.mu.Lock()
		srv.config.StartupServerSettings = &config.V1ServerConfig{
			Hub: &config.V1ServerHubConfig{
				SystemOne: &config.V1ServerHubSystemOneConfig{
					SecretNames: map[string][]string{
						geminidecisions.AdapterName: {"GEMINI_TEAM_KEY", "systemone.instance.bad"},
						jevwire.AdapterNameTypeSafe: {"TYPESAFE_IGNORED"},
					},
				},
			},
		}
		srv.systemOneExtraSecretNamesSnapshotted = false
		srv.systemOneExtraSecretNames = nil
		srv.mu.Unlock()

		// Install counting ADC provider that fails if ever invoked.
		var adcCalls atomic.Int32
		srv.SetSystemOneADCForTesting(t, func(context.Context, []string) (*SystemOneADCCredentials, error) {
			adcCalls.Add(1)
			return nil, errors.New("ADC discovery must not be called by /adapters")
		})

		// Install counting, failing egress dialer so any outbound provider I/O fails and is counted.
		egressDialer := &systemOneCountingFailingDialer{}
		srv.SetSystemOneRuntimeOptionsForTesting(t, systemoneruntime.WithEgressOptionsForTesting(
			egress.WithDialerForTesting(testSystemOnePublicResolver{}, egressDialer),
		))

		rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, systemOneAdaptersPath, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		// Assert zero secret reads, zero ADC discovery, zero outbound provider dials, and content-free response.
		assert.Zero(t, countingBackend.getCalls.Load(), "GET /adapters must not read secrets")
		assert.Zero(t, countingBackend.setCalls.Load(), "GET /adapters must not write secrets")
		assert.Zero(t, countingBackend.delCalls.Load(), "GET /adapters must not delete secrets")
		assert.Zero(t, adcCalls.Load(), "GET /adapters must not trigger ADC discovery")
		assert.Zero(t, egressDialer.dials.Load(), "GET /adapters must not perform outbound provider I/O")
		assert.NotContains(t, rec.Body.String(), secretCanary)

		var resp ListSystemOneAdaptersResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

		byID := make(map[string]SystemOneAdapterCapabilityDTO, len(resp.Adapters))
		for _, a := range resp.Adapters {
			byID[a.ID] = a
		}

		// Verify gemini_decisions capabilities.
		gem, ok := byID[geminidecisions.AdapterName]
		require.True(t, ok, "gemini_decisions adapter must be present")
		assert.Equal(t, "Gemini Decisions", gem.DisplayName)
		assert.Equal(t, geminidecisions.DefaultEndpoint, gem.DefaultEndpoint)
		assert.True(t, gem.ModelRequired)
		assert.False(t, gem.SupportsModelListing)
		assert.Equal(t, []string{"GEMINI_API_KEY", "GEMINI_TEAM_KEY"}, gem.AllowedSecretNames)
		assert.Equal(t, "GEMINI_API_KEY", gem.DefaultSecretName)
		assert.Equal(t, []string{"hub", "project", "project_or_hub"}, gem.AllowedSecretScopes)
		assert.Equal(t, "project_or_hub", gem.DefaultSecretScope)

		gemModes := make(map[string]SystemOneAdapterAuthModeDTO, len(gem.AuthModes))
		for _, m := range gem.AuthModes {
			gemModes[m.Mode] = m
		}
		require.Contains(t, gemModes, "owned_api_key")
		assert.True(t, gemModes["owned_api_key"].CustomEndpointAllowed)
		assert.Equal(t, store.SystemOneAuthKindAPIKey, gemModes["owned_api_key"].AuthKind)

		require.Contains(t, gemModes, "secret_ref")
		assert.False(t, gemModes["secret_ref"].CustomEndpointAllowed)
		assert.Equal(t, store.SystemOneAuthKindAPIKey, gemModes["secret_ref"].AuthKind)

		require.Contains(t, gemModes, "google_access_token")
		assert.False(t, gemModes["google_access_token"].CustomEndpointAllowed)
		assert.True(t, gemModes["google_access_token"].ScopesRequired)
		assert.Equal(t, store.SystemOneAuthKindGoogleAccessToken, gemModes["google_access_token"].AuthKind)

		// Verify typesafe capabilities (no SecretNamePolicy -> owned_api_key only).
		ts, ok := byID[jevwire.AdapterNameTypeSafe]
		require.True(t, ok, "typesafe adapter must be present")
		assert.Equal(t, "TypeSafe", ts.DisplayName)
		assert.Equal(t, jevwire.DefaultTypeSafeEndpoint, ts.DefaultEndpoint)
		assert.True(t, ts.ModelRequired)
		assert.True(t, ts.SupportsModelListing)
		assert.Empty(t, ts.AllowedSecretNames)
		assert.Empty(t, ts.AllowedSecretScopes)
		assert.Empty(t, ts.DefaultSecretScope)
		require.Len(t, ts.AuthModes, 1)
		assert.Equal(t, "owned_api_key", ts.AuthModes[0].Mode)
		assert.Equal(t, "bearer", ts.AuthModes[0].TransportScheme)
		assert.True(t, ts.AuthModes[0].CustomEndpointAllowed)
	})
}
