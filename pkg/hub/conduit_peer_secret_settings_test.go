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
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The relay-peer secret (server.hub.conduit.peer_secret, design §3.9) is
// never stored in the database or returned by the API: every settings write
// path refuses a non-empty value in file, DB and workstation mode, ignores
// an empty one (nothing is written), and GET never shows it.

const peerSecretSettingValue = "peer-secret-value-0123456789abcdefghij"

var peerSecretBodies = map[string]string{
	"nested":      `{"server":{"hub":{"conduit":{"peer_secret":"` + peerSecretSettingValue + `"}}}}`,
	"flat dotted": `{"server.hub.conduit.peer_secret":"` + peerSecretSettingValue + `"}`,
	"case folded": `{"Server":{"Hub":{"Conduit":{"Peer_Secret":"` + peerSecretSettingValue + `"}}}}`,
	"short":       `{"server":{"hub":{"conduit":{"peer_secret":"x"}}}}`,
	"with others": `{"default_model":"m1","server":{"hub":{"conduit":{"peer_secret":"` + peerSecretSettingValue + `","reconnect_window":"3s"}}}}`,
}

func TestNeverAPIWritable_PeerSecretRefusedInFileMode(t *testing.T) {
	for name, body := range peerSecretBodies {
		t.Run(name, func(t *testing.T) {
			srv := &Server{}
			rr, settingsPath := fileModePutServerConfig(t, srv, body)
			assert.Equal(t, []string{config.ConduitPeerSecretKey}, tiaErrorKeys(t, rr))
			assert.NotContains(t, rr.Body.String(), peerSecretSettingValue, "the refusal must not echo the value")
			_, err := os.Stat(settingsPath)
			assert.True(t, os.IsNotExist(err), "a refused PUT writes no settings file")
		})
	}
}

func TestNeverAPIWritable_PeerSecretRefusedInDBAndWorkstationMode(t *testing.T) {
	for _, workstation := range []bool{false, true} {
		for name, body := range peerSecretBodies {
			t.Run(map[bool]string{false: "db", true: "workstation"}[workstation]+"/"+name, func(t *testing.T) {
				settingsPath := workstationHome(t)
				srv, st, _ := newSQLiteHubInMode(t, workstation, nil)
				before := hubSettingRevisions(t, st)
				rr := putServerConfig(t, srv, body)
				assert.Equal(t, []string{config.ConduitPeerSecretKey}, tiaErrorKeys(t, rr))
				data, err := os.ReadFile(settingsPath)
				require.NoError(t, err)
				assert.NotContains(t, string(data), "peer_secret")
				assert.Equal(t, before, hubSettingRevisions(t, st), "a refused PUT writes no DB row")
			})
		}
	}
}

// writePeerSecretSettings writes a settings.yaml holding a relay-peer
// secret under a fresh HOME and returns its path.
func writePeerSecretSettings(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".scion", "settings.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	yaml := "schema_version: \"1\"\nserver:\n  hub:\n    conduit:\n      relay_mode: external\n      peer_secret: \"" + peerSecretSettingValue + "\"\n"
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	return path
}

func TestNeverAPIWritable_GetDoesNotShowPeerSecret(t *testing.T) {
	settingsPath := writePeerSecretSettings(t)
	srv := &Server{}
	getRR := httptest.NewRecorder()
	srv.handleGetServerConfig(getRR)
	require.Equal(t, http.StatusOK, getRR.Code, getRR.Body.String())
	assert.NotContains(t, getRR.Body.String(), peerSecretSettingValue)
	assert.Contains(t, getRR.Body.String(), `"relay_mode":"external"`, "other conduit keys are still shown")
	// The GET body echoes back cleanly, and the file value is preserved.
	echo := httptest.NewRecorder()
	srv.handleAdminServerConfig(echo, adminRequest(http.MethodPut, "/api/v1/admin/server-config", getRR.Body.String()))
	assert.Equal(t, http.StatusOK, echo.Code, echo.Body.String())
	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), peerSecretSettingValue)
}

func TestNeverAPIWritable_DBGetDoesNotShowPeerSecret(t *testing.T) {
	writePeerSecretSettings(t)
	srv, _, ops := newSQLiteHubInMode(t, false, nil)
	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.NotContains(t, rr.Body.String(), peerSecretSettingValue)
}

// TestDropEmptyPeerSecret: empty or null peer_secret members, and the
// objects their removal empties, are dropped; anything else is kept as
// sent, numbers included.
func TestDropEmptyPeerSecret(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"nested empty", `{"server":{"hub":{"conduit":{"peer_secret":""}}}}`, `{}`},
		{"null", `{"server":{"hub":{"conduit":{"peer_secret":null}}}}`, `{}`},
		{"dotted", `{"server.hub.conduit.peer_secret":"","default_model":"m1"}`, `{"default_model":"m1"}`},
		{"partly dotted", `{"server.hub":{"conduit":{"Peer_Secret":"","reconnect_window":"3s"}}}`, `{"server.hub":{"conduit":{"reconnect_window":"3s"}}}`},
		{"numbers kept", `{"max_turns":12345678901234567890,"server":{"hub":{"conduit":{"peer_secret":""}}}}`, `{"max_turns":12345678901234567890}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.JSONEq(t, tc.want, string(dropEmptyPeerSecret([]byte(tc.in))))
		})
	}
	assert.Contains(t, string(dropEmptyPeerSecret([]byte(`{"max_turns":12345678901234567890,"server.hub.conduit.peer_secret":""}`))),
		"12345678901234567890", "a number keeps its spelling")
	for _, body := range []string{
		`{"server":{"hub":{"conduit":{"peer_secret":"x"}}}}`,
		`{"server":{"hub":{"conduit":{}}}}`,
		`{"default_model":"m1"}`,
		`not json`,
	} {
		assert.Equal(t, body, string(dropEmptyPeerSecret([]byte(body))), "unchanged: %s", body)
	}
}

func TestNeverAPIWritable_EmptyPeerSecretIgnoredInFileMode(t *testing.T) {
	settingsPath := writePeerSecretSettings(t)
	srv := &Server{}
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"server":{"hub":{"conduit":{"peer_secret":"","reconnect_window":"3s"}}}}`))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), peerSecretSettingValue, "an empty value does not clear the stored secret")
	assert.Contains(t, string(data), "reconnect_window")
}

func TestNeverAPIWritable_EmptyPeerSecretIgnoredInDBAndWorkstationMode(t *testing.T) {
	for _, workstation := range []bool{false, true} {
		t.Run(map[bool]string{false: "db", true: "workstation"}[workstation], func(t *testing.T) {
			settingsPath := workstationHome(t)
			srv, st, _ := newSQLiteHubInMode(t, workstation, nil)
			before := hubSettingRevisions(t, st)
			rr := putServerConfig(t, srv, `{"default_model":"m-empty-secret","server":{"hub":{"conduit":{"peer_secret":""}}}}`)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			data, err := os.ReadFile(settingsPath)
			require.NoError(t, err)
			assert.NotContains(t, string(data), "peer_secret")
			if !workstation {
				assert.NotEqual(t, before, hubSettingRevisions(t, st), "the other key is written")
			}
		})
	}
}

// TestNeverAPIWritable_OnlyEmptyPeerSecretChangesNothing: a body holding
// only an empty (or null) peer_secret is accepted and changes nothing.
func TestNeverAPIWritable_OnlyEmptyPeerSecretChangesNothing(t *testing.T) {
	for _, workstation := range []bool{false, true} {
		for name, body := range map[string]string{
			"empty": `{"server":{"hub":{"conduit":{"peer_secret":""}}}}`,
			"null":  `{"server":{"hub":{"conduit":{"peer_secret":null}}}}`,
		} {
			t.Run(map[bool]string{false: "db", true: "workstation"}[workstation]+"/"+name, func(t *testing.T) {
				settingsPath := workstationHome(t)
				before, err := os.ReadFile(settingsPath)
				require.NoError(t, err)
				srv, st, _ := newSQLiteHubInMode(t, workstation, nil)
				revisions := hubSettingRevisions(t, st)
				rr := putServerConfig(t, srv, body)
				require.Less(t, rr.Code, 300, rr.Body.String())
				after, err := os.ReadFile(settingsPath)
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after), "settings.yaml unchanged")
				assert.Equal(t, revisions, hubSettingRevisions(t, st), "no DB row written")
			})
		}
	}
}

// TestNeverAPIWritable_NullPeerSecretIgnoredInFileMode: a null peer_secret
// in a file-mode PUT neither errors nor clears the stored secret.
func TestNeverAPIWritable_NullPeerSecretIgnoredInFileMode(t *testing.T) {
	settingsPath := writePeerSecretSettings(t)
	srv := &Server{}
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"server":{"hub":{"conduit":{"peer_secret":null,"reconnect_window":"4s"}}}}`))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), peerSecretSettingValue)
	assert.Contains(t, string(data), "4s")
}
