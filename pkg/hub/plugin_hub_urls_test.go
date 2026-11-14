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
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetPluginHubCreds_PublicAndInternalURL checks the two hub URLs handed
// to broker plugins (ptone/scion#555): public_url, for user-facing links, is
// always the public endpoint (server.hub.public_url through HubEndpointFor);
// hub_url, for API calls, is server.hub.internal_url when set and otherwise
// the public endpoint, as before.
func TestGetPluginHubCreds_PublicAndInternalURL(t *testing.T) {
	endpoints := &HubEndpoints{Operator: "http://localhost:8080", Public: "https://hub.example.com"}

	t.Run("internal_url unset", func(t *testing.T) {
		srv := &Server{config: ServerConfig{HubEndpoint: "http://localhost:8080", HubEndpoints: endpoints}}
		creds := srv.getPluginHubCreds(context.Background(), "test-plugin")
		if got := creds["hub_url"]; got != "https://hub.example.com" {
			t.Errorf("hub_url = %q, want the public endpoint (unchanged default)", got)
		}
		if got := creds["public_url"]; got != "https://hub.example.com" {
			t.Errorf("public_url = %q, want the public endpoint", got)
		}
	})

	t.Run("internal_url set", func(t *testing.T) {
		srv := &Server{config: ServerConfig{
			HubEndpoint:  "http://localhost:8080",
			HubEndpoints: endpoints,
			InternalURL:  "http://127.0.0.1:9810",
		}}
		creds := srv.getPluginHubCreds(context.Background(), "test-plugin")
		if got := creds["hub_url"]; got != "http://127.0.0.1:9810" {
			t.Errorf("hub_url = %q, want server.hub.internal_url", got)
		}
		if got := creds["public_url"]; got != "https://hub.example.com" {
			t.Errorf("public_url = %q, want the public endpoint, not internal_url", got)
		}
		hubURL, publicURL := srv.PluginHubURLs()
		if hubURL != "http://127.0.0.1:9810" || publicURL != "https://hub.example.com" {
			t.Errorf("PluginHubURLs() = %q, %q", hubURL, publicURL)
		}
	})
}

// TestPluginHubURLKeysAreHubKeys checks that public_url is a hub wiring key:
// the live hub value overrides a cached one on reconfigure, and integration
// settings responses leave it out like hub_url.
func TestPluginHubURLKeysAreHubKeys(t *testing.T) {
	for _, k := range []string{"hub_url", "public_url"} {
		if !hubKeys[k] {
			t.Errorf("hubKeys lacks %q", k)
		}
		if !reconfigureRuntimeKeys[k] {
			t.Errorf("reconfigureRuntimeKeys lacks %q", k)
		}
	}
}

// server.hub.internal_url decides where broker plugins send their signed Hub
// API calls, so it is startup config only (ptone/scion#555): the
// server-config API refuses it in every mode, and GET leaves it out so an
// echo of GET is not refused.

const hubInternalURLSentinel = "http://192.0.2.77:8080"

func TestNeverAPIWritable_HubInternalURLRefused(t *testing.T) {
	body := `{"server":{"hub":{"internal_url":"http://127.0.0.1:8080"}}}`
	t.Run("file", func(t *testing.T) {
		rr, settingsPath := fileModePutServerConfig(t, &Server{}, body)
		assert.Equal(t, []string{config.HubInternalURLKey}, tiaErrorKeys(t, rr))
		_, err := os.Stat(settingsPath)
		assert.True(t, os.IsNotExist(err), "a refused PUT writes no settings file")
	})
	for _, workstation := range []bool{false, true} {
		t.Run(map[bool]string{false: "db", true: "workstation"}[workstation], func(t *testing.T) {
			workstationHome(t)
			srv, st, _ := newSQLiteHubInMode(t, workstation, nil)
			before := hubSettingRevisions(t, st)
			rr := putServerConfig(t, srv, body)
			assert.Equal(t, []string{config.HubInternalURLKey}, tiaErrorKeys(t, rr))
			assert.Equal(t, before, hubSettingRevisions(t, st), "a refused PUT writes no DB row")
		})
	}
}

func TestNeverAPIWritable_FileModeGetDoesNotEchoHubInternalURL(t *testing.T) {
	settingsPath := tempSettingsHome(t)
	yaml := "schema_version: \"1\"\nserver:\n  hub:\n    internal_url: " + hubInternalURLSentinel + "\n"
	require.NoError(t, os.WriteFile(settingsPath, []byte(yaml), 0o644))
	srv := &Server{}
	getRR := httptest.NewRecorder()
	srv.handleGetServerConfig(getRR)
	require.Equal(t, http.StatusOK, getRR.Code, getRR.Body.String())
	assert.NotContains(t, getRR.Body.String(), hubInternalURLSentinel)
	echo := httptest.NewRecorder()
	srv.handleAdminServerConfig(echo, adminRequest(http.MethodPut, "/api/v1/admin/server-config", getRR.Body.String()))
	assert.Equal(t, http.StatusOK, echo.Code, echo.Body.String())
	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "internal_url: "+hubInternalURLSentinel, "the file value is preserved")
}
