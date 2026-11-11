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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/stretchr/testify/require"
)

// seedSystemSettingsHome points HOME at a fresh directory holding a minimal
// global settings.yaml and returns that file's path.
func seedSystemSettingsHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "settings.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: \"1\"\n"), 0o644))
	return path
}

// doLoopbackSystemRequest sends a request from loopback through the full
// server handler; no headers means an unauthenticated request.
func doLoopbackSystemRequest(srv *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestSystemMutatingRoutesRequireHubAdmin(t *testing.T) {
	srv, s := testWorkstationServer(t)
	ctx := context.Background()
	_, _, member, project := setupCredentialTestServerOn(t, srv, s)

	memberToken, _, _, err := srv.userTokenService.GenerateTokenPair(
		member.ID, member.Email, member.DisplayName, member.Role, ClientTypeWeb)
	require.NoError(t, err)

	agent := createCredTestAgent(t, s, tid("agent-system-routes"), project.ID, member.ID)
	agentToken, err := srv.GenerateAgentToken(agent.ID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)

	devBefore, err := s.GetUser(ctx, DevUserID)
	require.NoError(t, err)

	routes := []struct {
		method, path, body string
	}{
		{http.MethodPut, "/api/v1/system/identity", `{"displayName":"Taken Over","email":"attacker@example.com"}`},
		{http.MethodPatch, "/api/v1/system/workstation-settings", `{"auto_inject_gcloud_adc":true}`},
	}
	refused := []struct {
		who     string
		headers map[string]string
		want    int
	}{
		{"member user", map[string]string{"Authorization": "Bearer " + memberToken}, http.StatusForbidden},
		{"agent", map[string]string{"X-Scion-Agent-Token": agentToken}, http.StatusForbidden},
		{"unauthenticated", nil, http.StatusUnauthorized},
	}
	for _, rt := range routes {
		for _, c := range refused {
			t.Run(rt.path+"/"+c.who, func(t *testing.T) {
				settingsPath := seedSystemSettingsHome(t)
				rec := doLoopbackSystemRequest(srv, rt.method, rt.path, rt.body, c.headers)
				require.Equal(t, c.want, rec.Code, "%s %s as %s: %s", rt.method, rt.path, c.who, rec.Body.String())

				data, err := os.ReadFile(settingsPath)
				require.NoError(t, err)
				require.Equal(t, "version: \"1\"\n", string(data))
				dev, err := s.GetUser(ctx, DevUserID)
				require.NoError(t, err)
				require.Equal(t, devBefore.Email, dev.Email)
				require.Equal(t, devBefore.DisplayName, dev.DisplayName)
			})
		}
	}

	dev := map[string]string{"Authorization": "Bearer " + testDevToken}
	t.Run("/api/v1/system/identity/dev super-admin", func(t *testing.T) {
		settingsPath := seedSystemSettingsHome(t)
		rec := doLoopbackSystemRequest(srv, http.MethodPut, "/api/v1/system/identity",
			`{"displayName":"Desk Owner","email":"owner@example.com"}`, dev)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		data, err := os.ReadFile(settingsPath)
		require.NoError(t, err)
		require.Contains(t, string(data), "owner@example.com")
	})
	t.Run("/api/v1/system/workstation-settings/dev super-admin", func(t *testing.T) {
		settingsPath := seedSystemSettingsHome(t)
		rec := doLoopbackSystemRequest(srv, http.MethodPatch, "/api/v1/system/workstation-settings",
			`{"auto_inject_gcloud_adc":true}`, dev)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		data, err := os.ReadFile(settingsPath)
		require.NoError(t, err)
		require.Contains(t, string(data), "auto_inject_gcloud_adc: true")
	})
}

// TestSystemIdentityAppliesWithoutRestart pins GoogleCloudPlatform/scion#4588:
// the identity saved by the onboarding wizard is what dev-token requests
// authenticate as on the very next request, not only after a restart.
func TestSystemIdentityAppliesWithoutRestart(t *testing.T) {
	srv, s := testWorkstationServer(t)
	ctx := context.Background()
	dev := map[string]string{"Authorization": "Bearer " + testDevToken}

	me := func(t *testing.T) (displayName, email string) {
		t.Helper()
		rec := doLoopbackSystemRequest(srv, http.MethodGet, "/api/v1/auth/me", "", dev)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body struct {
			DisplayName string `json:"displayName"`
			Email       string `json:"email"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		return body.DisplayName, body.Email
	}

	seedSystemSettingsHome(t)
	name, email := me(t)
	require.NotEqual(t, "Desk Owner", name)
	require.NotEqual(t, "owner@example.com", email)

	rec := doLoopbackSystemRequest(srv, http.MethodPut, "/api/v1/system/identity",
		`{"displayName":"Desk Owner","email":"owner@example.com"}`, dev)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	name, email = me(t)
	require.Equal(t, "Desk Owner", name)
	require.Equal(t, "owner@example.com", email)

	// A partial update changes only the field it names.
	rec = doLoopbackSystemRequest(srv, http.MethodPut, "/api/v1/system/identity",
		`{"displayName":"Renamed Owner"}`, dev)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	name, email = me(t)
	require.Equal(t, "Renamed Owner", name)
	require.Equal(t, "owner@example.com", email)

	// The live identity stays the trusted local dev user, and the stored
	// record agrees with it.
	require.True(t, isTrustedLocalDevUser(srv.authConfig.currentDevUser()))
	stored, err := s.GetUser(ctx, DevUserID)
	require.NoError(t, err)
	require.Equal(t, "Renamed Owner", stored.DisplayName)
	require.Equal(t, "owner@example.com", stored.Email)
}

// TestDevIdentityCallSitesFollowSetIdentity pins the paths that rebuild the
// development identity outside the auth middleware (relay re-check, conduit
// renewal, pending config saver): after an identity change they return the
// new display name and email, and still the trusted local dev user.
func TestDevIdentityCallSitesFollowSetIdentity(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()
	require.NotNil(t, srv.authConfig.DevUser)
	srv.authConfig.DevUser.SetIdentity("Desk Owner", "owner@example.com")

	requireNewIdentity := func(t *testing.T, ident Identity) {
		t.Helper()
		require.True(t, isTrustedLocalDevUser(ident), "identity %T", ident)
		du := ident.(*DevUser)
		require.Equal(t, "Desk Owner", du.DisplayName())
		require.Equal(t, "owner@example.com", du.Email())
	}

	t.Run("relay re-check", func(t *testing.T) {
		ident, verdict, reason := srv.relayUserIdentity(ctx,
			conduitAdmissionClaims{ID: DevUserID, AuthType: AuthTypeDevToken})
		require.Equal(t, conduitAuthzAllowed, verdict, reason)
		requireNewIdentity(t, ident)
	})
	t.Run("conduit renewal", func(t *testing.T) {
		ident, err := srv.conduitRenewalIdentity(ctx, &grant.RecheckClaims{}, DevUserID)
		require.NoError(t, err)
		requireNewIdentity(t, ident)
	})
	t.Run("pending config saver", func(t *testing.T) {
		d, err := srv.resolvePendingDevSaver(ctx)
		require.NoError(t, err)
		require.Equal(t, saverApply, d.out, d.reason)
		requireNewIdentity(t, d.identity)
	})
}

func TestSystemReadRoutesStayWorkstationOnly(t *testing.T) {
	srv, s := testWorkstationServer(t)
	_, _, member, _ := setupCredentialTestServerOn(t, srv, s)
	memberToken, _, _, err := srv.userTokenService.GenerateTokenPair(
		member.ID, member.Email, member.DisplayName, member.Role, ClientTypeWeb)
	require.NoError(t, err)

	rec := doLoopbackSystemRequest(srv, http.MethodGet, "/api/v1/system/status", "",
		map[string]string{"Authorization": "Bearer " + memberToken})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestSystemIdentityIs404OutsideWorkstation(t *testing.T) {
	srv, _ := testServer(t)
	require.False(t, srv.workstation)
	settingsPath := seedSystemSettingsHome(t)

	dev := map[string]string{"Authorization": "Bearer " + testDevToken}
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodPut, "/api/v1/system/identity", `{"displayName":"Desk Owner","email":"owner@example.com"}`},
		{http.MethodPatch, "/api/v1/system/workstation-settings", `{"auto_inject_gcloud_adc":true}`},
		{http.MethodGet, "/api/v1/system/status", ""},
	} {
		rec := doLoopbackSystemRequest(srv, rt.method, rt.path, rt.body, dev)
		require.Equal(t, http.StatusNotFound, rec.Code, "%s %s: %s", rt.method, rt.path, rec.Body.String())
	}
	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	require.Equal(t, "version: \"1\"\n", string(data))
}
