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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	systemoneruntime "github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
)

const systemOneAdminPath = "/api/v1/admin/system-one/instances"

func newSystemOneTestServer(t *testing.T, enabled bool) (*Server, store.Store, *secret.LocalBackend) {
	t.Helper()
	srv, s := testServerWithOps(t, nil)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret-signing-key")
	srv.SetSecretBackend(localBackend)

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
			egress.WithLoopbackServerForTesting(tlsSrv.URL, tlsSrv.Client()),
		),
	)

	const (
		sentinelKeyA  = "SENTINEL_TYPESAFE_KEY_ALPHA_9001"
		sentinelKeyB  = "SENTINEL_TYPESAFE_KEY_BETA_9002"
		sentinelInput = "SENTINEL_PROBE_SENSITIVE_TEXT_7788"
	)

	// Validation errors (400).
	t.Run("validation rejections", func(t *testing.T) {
		for _, badKind := range []string{"none", "google_access_token", "google_id_token", "azure_entra", "unknown_kind"} {
			rec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
				"name":    "bad-auth-" + strings.ReplaceAll(badKind, "_", "-"),
				"adapter": "typesafe",
				"model":   "jev-1.14.0",
				"auth":    map[string]any{"kind": badKind, "apiKey": "k"},
			})
			require.Equal(t, http.StatusBadRequest, rec.Code, "kind=%s body=%s", badKind, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "auth kind not yet supported")
		}

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
			egress.WithLoopbackServerForTesting(tlsSrv.URL, tlsSrv.Client()),
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
			egress.WithLoopbackServerForTesting(tlsSrv.URL, tlsSrv.Client()),
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
			egress.WithLoopbackServerForTesting(tlsSrv.URL, tlsSrv.Client()),
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
