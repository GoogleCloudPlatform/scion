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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// ── ptone/scion#3071: the file-tier PUT validates the effective GCP pair ──

// TestPutServerConfigFile_HubDefaultGCPIdentity_EnvModeDecides: with the
// environment setting the mode to block, clearing a stored account is
// accepted even though settings.yaml still says assign, and the PUT does
// not write the environment value into settings.yaml.
func TestPutServerConfigFile_HubDefaultGCPIdentity_EnvModeDecides(t *testing.T) {
	srv, s, settingsPath := fileModeGCPIdentityServer(t)
	setMode(srv, SAAssignCheckEnforce)
	sa := gcpIdentitySettingsSA(t, s, store.ScopeHub, "hub", true)
	put := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		srv.handlePutServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
		return rr
	}
	rr := put(fmt.Sprintf(`{"default_gcp_identity_mode":"assign","default_gcp_identity_service_account_id":%q}`, sa.ID))
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	t.Setenv("SCION_SERVER_DEFAULTGCPIDENTITYMODE", "block")
	rr = put(`{"default_gcp_identity_service_account_id":""}`)
	require.Equal(t, http.StatusOK, rr.Code,
		"the environment mode (block) needs no account; body: %s", rr.Body.String())
	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "default_gcp_identity_mode: assign", "the stored mode is unchanged")
	require.NotContains(t, string(data), "block", "the environment value is not written")
}

// TestPutServerConfigFile_HubDefaultGCPIdentity_EnvAssignNeedsAccount: with
// the environment setting the mode to assign and no account anywhere, a PUT
// that touches the pair is refused even though settings.yaml has no mode;
// an account in the environment satisfies it.
func TestPutServerConfigFile_HubDefaultGCPIdentity_EnvAssignNeedsAccount(t *testing.T) {
	srv, s, _ := fileModeGCPIdentityServer(t)
	setMode(srv, SAAssignCheckEnforce)
	sa := gcpIdentitySettingsSA(t, s, store.ScopeHub, "hub", true)
	put := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		srv.handlePutServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
		return rr
	}
	t.Setenv("SCION_SERVER_DEFAULTGCPIDENTITYMODE", "assign")
	rr := put(`{"default_gcp_identity_service_account_id":""}`)
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "body: %s", rr.Body.String())
	require.Contains(t, rr.Body.String(), "requires a service account")

	t.Setenv("SCION_SERVER_DEFAULTGCPIDENTITYSERVICEACCOUNTID", sa.ID)
	rr = put(`{"default_gcp_identity_service_account_id":""}`)
	require.Equal(t, http.StatusOK, rr.Code,
		"the environment account satisfies the environment mode; body: %s", rr.Body.String())
}

// TestEffectiveFileGCPIdentity: each key comes from the environment only
// when the environment sets it.
func TestEffectiveFileGCPIdentity(t *testing.T) {
	merged := map[string]any{"default_gcp_identity_mode": "assign", "default_gcp_identity_service_account_id": "file-sa"}
	got := effectiveFileGCPIdentity(newEnvKoanf(t, map[string]any{}), merged)
	require.Equal(t, "assign", got.DefaultGCPIdentityMode)
	require.Equal(t, "file-sa", got.DefaultGCPIdentityServiceAccountID)

	got = effectiveFileGCPIdentity(newEnvKoanf(t, map[string]any{"default_gcp_identity_mode": "block"}), merged)
	require.Equal(t, "block", got.DefaultGCPIdentityMode)
	require.Equal(t, "file-sa", got.DefaultGCPIdentityServiceAccountID)

	got = effectiveFileGCPIdentity(nil, merged)
	require.Equal(t, "assign", got.DefaultGCPIdentityMode)
}

// TestPutServerConfigFile_HubDefaultGCPIdentity_PinWarning: a save that is
// valid only because the environment pins the mode warns that the stored
// settings.yaml pair would be refused on its own; a stored pair that is
// valid on its own, or no env key at all, gives no warning.
func TestPutServerConfigFile_HubDefaultGCPIdentity_PinWarning(t *testing.T) {
	srv, s, _ := fileModeGCPIdentityServer(t)
	setMode(srv, SAAssignCheckEnforce)
	sa := gcpIdentitySettingsSA(t, s, store.ScopeHub, "hub", true)
	put := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		srv.handlePutServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
		return rr
	}
	// Control: no env key; a valid pair gives no warning.
	rr := put(fmt.Sprintf(`{"default_gcp_identity_mode":"assign","default_gcp_identity_service_account_id":%q}`, sa.ID))
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	require.Empty(t, gcpPinWarnings(putWarnings(t, rr)))

	t.Setenv("SCION_SERVER_DEFAULTGCPIDENTITYMODE", "block")
	// Control: env set, but the stored pair (assign + account) is valid alone.
	rr = put(fmt.Sprintf(`{"default_gcp_identity_service_account_id":%q}`, sa.ID))
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	require.Empty(t, gcpPinWarnings(putWarnings(t, rr)))

	// Clearing the account leaves stored assign without one: accepted under
	// the env pin, with the warning naming the field and the pinned key.
	rr = put(`{"default_gcp_identity_service_account_id":""}`)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	w := gcpPinWarnings(putWarnings(t, rr))
	require.Len(t, w, 1)
	require.Contains(t, w[0], "default_gcp_identity_service_account_id")
	require.Contains(t, w[0], "requires a service account")
	require.Contains(t, w[0], "the environment sets default_gcp_identity_mode")
	require.NotContains(t, w[0], sa.ID)
}

// gcpPinWarnings filters warnings to the stored-pair pin warning.
func gcpPinWarnings(all []string) []string {
	var out []string
	for _, w := range all {
		if strings.Contains(w, "default GCP identity saved in settings.yaml is not valid on its own") {
			out = append(out, w)
		}
	}
	return out
}
