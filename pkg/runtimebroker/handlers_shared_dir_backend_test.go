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

package runtimebroker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func postSharedDirCreate(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// A reprovision passes the shared dir backend change to Manager.Reprovision
// and confirms it in the response.
func TestCreateAgentReprovision_SharedDirBackendChange(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()
	w := postSharedDirCreate(t, srv, `{
		"name": "reprovisioned-agent", "id": "agent-uuid-reprov", "slug": "reprovisioned-agent",
		"provisionOnly": true, "reprovision": true, "config": {"template": "claude"},
		"sharedDirBackendChanges": {"notes": "nfs"}, "allowEmptySharedDir": true
	}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	require.True(t, mgr.reprovisionCalled)
	assert.Equal(t, map[string]string{"notes": "nfs"}, mgr.lastOpts.SharedDirBackendChanges)
	assert.True(t, mgr.lastOpts.AllowEmptySharedDir)
	var resp CreateAgentResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.True(t, resp.Reprovisioned)
	assert.True(t, resp.SharedDirBackendsChanged)
}

// A change back to local takes the same path.
func TestCreateAgentReprovision_SharedDirBackendChangeToLocal(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()
	w := postSharedDirCreate(t, srv, `{
		"name": "reprovisioned-agent", "id": "agent-uuid-reprov", "slug": "reprovisioned-agent",
		"provisionOnly": true, "reprovision": true, "config": {"template": "claude"},
		"sharedDirBackendChanges": {"notes": "local", "cache": "nfs"}
	}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	require.True(t, mgr.reprovisionCalled)
	assert.Equal(t, map[string]string{"notes": "local", "cache": "nfs"}, mgr.lastOpts.SharedDirBackendChanges)
	assert.False(t, mgr.lastOpts.AllowEmptySharedDir)
	var resp CreateAgentResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.True(t, resp.SharedDirBackendsChanged)
}

// A reprovision without a change does not claim one.
func TestCreateAgentReprovision_NoSharedDirBackendChangeNoEcho(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()
	w := postSharedDirCreate(t, srv, `{
		"name": "reprovisioned-agent", "id": "agent-uuid-reprov", "slug": "reprovisioned-agent",
		"provisionOnly": true, "reprovision": true, "config": {"template": "claude"}
	}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Nil(t, mgr.lastOpts.SharedDirBackendChanges)
	var resp CreateAgentResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.SharedDirBackendsChanged)
}

// A shared dir backend change on a request that is not a reprovision is
// refused before anything is provisioned.
func TestCreateAgent_SharedDirBackendChangeWithoutReprovisionRefused(t *testing.T) {
	for name, extra := range map[string]string{
		"change":          `"sharedDirBackendChanges": {"notes": "nfs"}`,
		"change to local": `"sharedDirBackendChanges": {"notes": "local"}`,
		"allow empty":     `"allowEmptySharedDir": true`,
	} {
		t.Run(name, func(t *testing.T) {
			srv, mgr := newTestServerWithProvisionCapture()
			w := postSharedDirCreate(t, srv, `{
				"name": "plain-agent", "id": "agent-uuid-plain", "slug": "plain-agent",
				"provisionOnly": true, "config": {"template": "claude"}, `+extra+`
			}`)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.False(t, mgr.provisionCalled)
			assert.False(t, mgr.reprovisionCalled)
		})
	}
}
