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

// These tests use fileModeGCPIdentityServer and readSettingsYAML, which are
// defined in hub_gcp_identity_default_settings_test.go under the same build
// constraint.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// File mode: an explicit null clears default_thinking_level, a value in
// range stores it, and 0 is rejected rather than clearing it
// (ptone/scion#3898).
func TestServerConfigFile_ThinkingLevel(t *testing.T) {
	srv, _, settingsPath := fileModeGCPIdentityServer(t)
	put := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		srv.handlePutServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
		return rr
	}

	rr := put(`{"default_thinking_level": 30}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, 30, readSettingsYAML(t, settingsPath)["default_thinking_level"])

	for _, bad := range []string{"0", "101"} {
		rr = put(`{"default_thinking_level": ` + bad + `}`)
		assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, "level %s: %s", bad, rr.Body.String())
		assert.Equal(t, 30, readSettingsYAML(t, settingsPath)["default_thinking_level"])
	}

	rr = put(`{"default_thinking_level": null}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	raw := readSettingsYAML(t, settingsPath)
	assert.NotContains(t, raw, "default_thinking_level")
	assert.Equal(t, "UTC", raw["default_timezone"], "unrelated keys are kept")
}
