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

// These tests use captureSlogDefault, which is defined in
// agent_endpoint_wire_test.go under the same build constraint.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// AC4 at the handler: github_app's schema forbids extra keys, so a stored
// key the section does not model is dropped from the written row with a
// warning that names it (and not its value), and the save succeeds.
func TestPutServerConfigDB_GitHubAppDropsSchemaForbiddenStoredKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	logs := captureSlogDefault(t)
	srv, fakeStore, ops := newTestDBServer(t)
	fakeStore.seedWithOrigin("github_app",
		json.RawMessage(`{"app_id":42,"private_key_path":"/etc/ghapp/key.pem","stale_leaf":"secret-ish-value"}`), "managed")

	rr := putServerConfigDB(t, srv, ops, `{"server":{"github_app":{"app_id":43}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	row := githubAppRowRaw(t, fakeStore)
	assert.Equal(t, map[string]interface{}{"app_id": float64(43), "private_key_path": "/etc/ghapp/key.pem"}, row)
	out := logs.String()
	assert.Contains(t, out, "github_app.stale_leaf")
	assert.False(t, strings.Contains(out, "secret-ish-value"), "the warning must not log the value")
}

// Finding 5 at the handler: a stored github_app value that fails the schema
// does not block a save that leaves it out; it is dropped with a warning
// that names its path and not its value.
func TestPutServerConfigDB_GitHubAppDropsInvalidCarriedValue(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	logs := captureSlogDefault(t)
	srv, fakeStore, ops := newTestDBServer(t)
	fakeStore.seedWithOrigin("github_app",
		json.RawMessage(`{"app_id":"not-a-number-value","private_key_path":"/etc/ghapp/key.pem"}`), "managed")

	rr := putServerConfigDB(t, srv, ops, `{"server":{"github_app":{"installation_url":"https://github.com/apps/y"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	row := githubAppRowRaw(t, fakeStore)
	assert.Equal(t, map[string]interface{}{
		"installation_url": "https://github.com/apps/y",
		"private_key_path": "/etc/ghapp/key.pem",
	}, row)
	out := logs.String()
	assert.Contains(t, out, "github_app.app_id")
	assert.False(t, strings.Contains(out, "not-a-number-value"), "the warning must not log the value")
}
