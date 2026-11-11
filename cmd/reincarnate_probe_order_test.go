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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fullResponseHub is a fake hub whose whole reincarnate response comes
// from respond; it records request bodies.
func fullResponseHub(t *testing.T, respond func(body map[string]interface{}) string) (*HubContext, *[]map[string]interface{}) {
	t.Helper()
	var bodies []map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/reincarnate") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		data, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		_ = json.Unmarshal(data, &body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respond(body)))
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: srv.URL, ProjectID: "proj-1"}, &bodies
}

// With a patch flag and --broker together, the dry-run probe carries both
// and its checks run in order: patch, then the move handshake. Each case
// fails exactly the first check its response does not satisfy.
func TestReincarnateAgentViaHub_ProbeChecksPatchAndMoveInOrder(t *testing.T) {
	const (
		patched = `"patched":["image"]`
		move    = `"sourceBrokerId":"b1","targetBrokerId":"b2","moveVerdict":{}`
	)
	resp := func(plan string, top string) string {
		out := `{"agentId":"agent-1","generation":2,"state":"planned","plan":{` + plan + `}`
		if top != "" {
			out += "," + top
		}
		return out + "}"
	}
	for _, tc := range []struct {
		name     string
		response string
		wantErr  string
		requests int
	}{
		{"nothing applied", resp("", ""), "does not support reincarnate patch flags", 1},
		{"patch, no move", resp(patched, ""), "does not support --broker", 1},
		{"all applied", resp(patched, move), "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hubCtx, bodies := fullResponseHub(t, func(map[string]interface{}) string { return tc.response })
			prevImage, prevDry, prevBroker := reincarnateImage, reincarnateDryRun, reincarnateBroker
			t.Cleanup(func() { reincarnateImage, reincarnateDryRun, reincarnateBroker = prevImage, prevDry, prevBroker })
			reincarnateImage, reincarnateDryRun, reincarnateBroker = "img:v2", false, "b2"

			err := reincarnateAgentViaHub(hubCtx, "agent-1", "handoff", false)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			require.Len(t, *bodies, tc.requests)
			probe := (*bodies)[0]
			assert.Equal(t, true, probe["dryRun"])
			assert.Equal(t, "b2", probe["targetBroker"])
			assert.Equal(t, "img:v2", probe["image"])
		})
	}
}

// The per-agent shared dir backend flags were removed
// (server.project_storage places every agent's shared dirs).
func TestReincarnateCmd_SharedDirBackendFlagsRemoved(t *testing.T) {
	assert.Nil(t, reincarnateCmd.Flags().Lookup("shared-dir-backend"))
	assert.Nil(t, reincarnateCmd.Flags().Lookup("allow-empty-shared-dir"))
}
