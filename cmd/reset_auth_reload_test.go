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

package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// Tests for ptone/scion#5460: reset-auth output when the credential was
// delivered but the running agent did not confirm the reload.

// newResetAuthHub serves the reset-auth routes with the given status and
// body for every request.
func newResetAuthHub(t *testing.T, status int, body string) *HubContext {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "reset-auth") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: srv.URL, ProjectID: "proj-5460"}
}

func TestResetAuthViaHub_ReloadUnconfirmed(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "")
	hubCtx := newResetAuthHub(t, http.StatusBadGateway,
		`{"error":{"code":"reload_unconfirmed","message":"`+api.ReloadUnconfirmedMessage+`"}}`)

	err := resetAuthViaHub(hubCtx, "dev")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not confirm the reload")
	assert.NotContains(t, err.Error(), "scion hub status", "the hub is healthy; no hub-check hint")
}

func TestReissueScopesViaHub_ReloadUnconfirmed(t *testing.T) {
	hubCtx := newResetAuthHub(t, http.StatusOK,
		`{"op_id":"op1","agent_id":"a1","dispatched":true,"reload_unconfirmed":"`+api.ReloadUnconfirmedMessage+`","message":"m"}`)

	err := reissueScopesViaHub(hubCtx, "dev", false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not confirm the reload")
	assert.NotContains(t, err.Error(), "not delivered")
}

func TestReissueScopesAllViaHub_ReloadUnconfirmed(t *testing.T) {
	hubCtx := newResetAuthHub(t, http.StatusOK, `{"batch_op_id":"b1","dry_run":false,"total":1,`+
		`"succeeded":[{"id":"a1","name":"dev"}],"noop":[],"refused":[],"push_failed":[],`+
		`"agents":[{"id":"a1","name":"dev","outcome":"changed","reload_unconfirmed":"`+api.ReloadUnconfirmedMessage+`"}],`+
		`"depth_unresolved":[],"batch_audit_recorded":true}`)

	err := reissueScopesAllViaHub(hubCtx, true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not confirm the reload")
	assert.NotContains(t, err.Error(), "did not receive their new token")
}
