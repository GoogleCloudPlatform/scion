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
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Federation in the server-config API (ptone/scion#4326): GET reports one
// value in both places it shows federation, the PUT location is the
// top-level federation member, and a null federation is refused instead of
// answering 200 with nothing changed.

// fedErrorBody decodes a structured error response.
func fedErrorBody(t *testing.T, rr *httptest.ResponseRecorder) (code, message string, details map[string]any) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body), rr.Body.String())
	return body.Error.Code, body.Error.Message, body.Error.Details
}

// DB tier: server.federation comes from the snapshot, like the top-level
// federation, not from the settings.yaml seed.
func TestServerConfigDB_GetFederationOneSource(t *testing.T) {
	settingsPath := tempSettingsHome(t)
	require.NoError(t, os.WriteFile(settingsPath, []byte(fileFederationSeed), 0o600))
	srv, _, ops := newTestDBServer(t)

	resp := getServerConfigDB(t, srv, ops)
	assert.Nil(t, resp.Federation, "no federation is in effect")
	require.NotNil(t, resp.Server)
	assert.Nil(t, resp.Server.Federation, "server.federation must not show the settings.yaml seed")

	// A stored section is reported whether or not it has trusted issuers
	// (see TestServerConfigDB_GetDisabledIssuerlessFederationShown).
	rr := putServerConfigDB(t, srv, ops, `{"federation":{"enabled":false,`+
		`"trusted_issuers":[{"issuer_url":"https://issuer-z.example.com","issuer_type":"hub"}],`+
		`"algorithms":["ES256"],"refresh_interval":"3h"}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	resp = getServerConfigDB(t, srv, ops)
	require.NotNil(t, resp.Federation)
	require.NotNil(t, resp.Server)
	require.NotNil(t, resp.Server.Federation)
	assert.Equal(t, resp.Federation, resp.Server.Federation, "both places report the same federation")
	assert.Equal(t, []string{"ES256"}, resp.Federation.Algorithms)
	assert.Equal(t, "3h0m0s", resp.Federation.RefreshInterval, "GET formats the interval as a Go duration")
	require.Len(t, resp.Federation.TrustedIssuers, 1, "only the stored issuer, not the settings.yaml seed issuers")
	assert.Equal(t, "https://issuer-z.example.com", resp.Federation.TrustedIssuers[0].IssuerURL)
}

// DB tier: a stored federation section that is disabled and has no
// trusted issuers is reported by GET in both places, with its stored
// algorithms and intervals, so the Federation page shows (and diffs
// against) the stored values rather than defaults (ptone/scion#5694).
func TestServerConfigDB_GetDisabledIssuerlessFederationShown(t *testing.T) {
	tempSettingsHome(t)
	srv, _, ops := newTestDBServer(t)

	resp := getServerConfigDB(t, srv, ops)
	assert.Nil(t, resp.Federation, "nothing stored: no federation")

	rr := putServerConfigDB(t, srv, ops, `{"federation":{"enabled":false,"algorithms":["ES256"],"refresh_interval":"3h"}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	resp = getServerConfigDB(t, srv, ops)
	require.NotNil(t, resp.Federation, "the stored disabled, issuer-less section is shown")
	require.NotNil(t, resp.Federation.Enabled)
	assert.False(t, *resp.Federation.Enabled)
	assert.Equal(t, []string{"ES256"}, resp.Federation.Algorithms)
	assert.Equal(t, "3h0m0s", resp.Federation.RefreshInterval)
	assert.Empty(t, resp.Federation.TrustedIssuers)
	require.NotNil(t, resp.Server)
	assert.Equal(t, resp.Federation, resp.Server.Federation, "both places report the same federation")
}

// TestServerConfigDB_EnvValuesAgainstDisabledIssuerlessFederation: the
// server-config GET reports a stored federation section that is disabled
// and has no trusted issuers (ptone/scion#5694), and env_values compares each
// SCION_SERVER_FEDERATION_* value with that reported view. A value equal to
// the applied stored value is in effect; a different one is not and is
// labelled as differing from the saved row. A federation key whose name
// looks secret is masked and its value never appears.
func TestServerConfigDB_EnvValuesAgainstDisabledIssuerlessFederation(t *testing.T) {
	secretKey := "server.federation.client_secret"
	values := dbEnvValuesGET(t, map[string][2]string{
		"federation": {"managed", `{"enabled":false,"algorithms":["RS256"],"refresh_interval":"3h0m0s"}`},
	}, nil, map[string]any{
		"server.federation.enabled":          true,
		"server.federation.algorithms":       []any{"ES256"},
		"server.federation.refresh_interval": "3h0m0s",
		secretKey:                            envSecretSentinel(secretKey),
	}, nil)
	wantEnvValue(t, values, "server.federation.enabled", EnvKeyValue{Value: true, InEffect: boolPtr(false), SavedDiffers: true})
	wantEnvValue(t, values, "server.federation.algorithms", EnvKeyValue{Value: []any{"ES256"}, InEffect: boolPtr(false), SavedDiffers: true})
	wantEnvValue(t, values, "server.federation.refresh_interval", EnvKeyValue{Value: "3h0m0s", InEffect: boolPtr(true)})
	wantEnvValue(t, values, secretKey, maskedEnvKeyValue())
	raw, err := json.Marshal(values)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), envSecretSentinel(secretKey), "a secret-looking federation value is never shown")
}

// File tier: both places read the same settings.yaml block.
func TestServerConfigFile_GetFederationOneSource(t *testing.T) {
	settingsPath := setTempScionHome(t)
	require.NoError(t, os.WriteFile(settingsPath, []byte(fileFederationSeed), 0o600))
	rr := httptest.NewRecorder()
	(&Server{}).handleAdminServerConfig(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp ServerConfigResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotNil(t, resp.Federation)
	require.NotNil(t, resp.Server)
	assert.Equal(t, resp.Federation, resp.Server.Federation)
	assert.Len(t, resp.Federation.TrustedIssuers, 2)
}

func TestServerConfigDB_NullFederationRefused(t *testing.T) {
	tempSettingsHome(t)
	srv, fake, ops := newTestDBServer(t)
	rr := putServerConfigDB(t, srv, ops, `{"federation":{"enabled":false,"algorithms":["RS256"]}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	before := storedRow(fake, "federation")
	require.NotNil(t, before)

	for _, body := range []string{
		`{"federation":null}`,
		`{"Federation": null}`,
		`{"federation":null,"default_model":"m"}`,
	} {
		rr := putServerConfigDB(t, srv, ops, body)
		require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "%s: %s", body, rr.Body.String())
		code, msg, details := fedErrorBody(t, rr)
		assert.Equal(t, ErrCodeValidationError, code, body)
		assert.Contains(t, msg, "DELETE /api/v1/admin/server-config/sections/federation", body)
		assert.Equal(t, []any{"federation"}, details["keys"], body)
	}
	after := storedRow(fake, "federation")
	require.NotNil(t, after)
	assert.Equal(t, before.Revision, after.Revision, "a refused PUT writes nothing")
	assert.Nil(t, storedRow(fake, "agent_defaults"), "a refused PUT writes nothing")
}

func TestServerConfigFile_NullFederationRefused(t *testing.T) {
	settingsPath := setTempScionHome(t)
	require.NoError(t, os.WriteFile(settingsPath, []byte(fileFederationSeed), 0o600))
	rr := httptest.NewRecorder()
	(&Server{}).handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", `{"federation":null}`))
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	code, _, details := fedErrorBody(t, rr)
	assert.Equal(t, ErrCodeValidationError, code)
	assert.Equal(t, []any{"federation"}, details["keys"])

	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Equal(t, fileFederationSeed, string(data), "settings.yaml is not touched")
}

// The PUT location is the top-level federation member. Federation sent
// under server.federation is refused on a database-backed hub (it would
// otherwise be dropped).
func TestServerConfigDB_FederationUnderServerRefused(t *testing.T) {
	tempSettingsHome(t)
	srv, fake, ops := newTestDBServer(t)
	rr := putServerConfigDB(t, srv, ops, `{"server":{"federation":{"enabled":false,"algorithms":["ES256"]}}}`)
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "server.federation")
	assert.Nil(t, storedRow(fake, "federation"), "nothing is written")

	rr = putServerConfigDB(t, srv, ops, `{"federation":{"enabled":false,"algorithms":["ES256"]}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.NotNil(t, storedRow(fake, "federation"))
}
