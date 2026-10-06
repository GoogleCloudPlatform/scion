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
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Workspace sync-to-finalize and env submit complete a create: they are
// where the broker run actually starts. A delete that wins after that run
// landed answers 409 delete_in_progress, with the outcome of the
// compensating delete in details.warnings, as the synchronous create does.
// No delete, a failed delete or an expired lease still answers 200
// (ptone/scion#3518).

// newLandingCompletionServer is a bootstrap server (storage, dev auth) whose
// dispatcher talks to a landingClient: every broker create runs onLand (the
// racing delete) and then answers with a running entry for the run it was
// sent. Launches are synchronous.
func newLandingCompletionServer(t *testing.T) (*Server, *asyncLaunchFixture, *landingClient) {
	t.Helper()
	srv, s, _, _ := testBootstrapServer(t)
	f := newAsyncLaunchFixtureOn(t, s, nil)
	c := &landingClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}, reportRunID: true}
	d := NewHTTPAgentDispatcherWithClient(s, c, false, slog.Default())
	d.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings { return AsyncLaunchSettings{} })
	srv.SetDispatcher(d)
	return srv, f, c
}

func TestWorkspaceFinalize_DeleteWonAfterLanding_Answers409(t *testing.T) {
	for i, del := range landingDeletes {
		t.Run(del.name, func(t *testing.T) {
			srv, f, c := newLandingCompletionServer(t)
			agent := f.agent(t, fmt.Sprintf("fin-%c", 'a'+i), string(state.PhaseProvisioning), false)
			c.onLand = func() { del.apply(t, f.store, agent.ID) }

			rec := doBootstrapRequest(t, srv, http.MethodPost,
				fmt.Sprintf("/api/v1/agents/%s/workspace/sync-to/finalize", agent.ID),
				SyncToFinalizeRequest{Manifest: &transfer.Manifest{Version: "1.0"}})
			require.NotNil(t, c.lastCreateReq, "dispatch ran: %d %s", rec.Code, rec.Body.String())
			runID := c.lastCreateReq.RunID

			if !del.compensate {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				var resp SyncToFinalizeResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				assert.True(t, resp.Applied)
				assert.Empty(t, c.deleteRuns, "no compensating delete")
				return
			}

			warnings := requireDeletedDuringCreate(t, rec, agent.ID)
			assert.Contains(t, warnings, landedRunRemovedWarning,
				"the compensating delete's outcome is reported")
			assert.Equal(t, []string{runID}, c.deleteRuns,
				"exactly one broker delete, scoped to the run that landed")
			var raw map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
			assert.NotContains(t, raw, "applied", "the 409 is not a finalize answer")
		})
	}
}

func TestSubmitAgentEnv_DeleteWonAfterLanding_Answers409(t *testing.T) {
	for i, del := range landingDeletes {
		t.Run(del.name, func(t *testing.T) {
			srv, f, c := newLandingCompletionServer(t)
			agent := f.agent(t, fmt.Sprintf("env-%c", 'a'+i), string(state.PhaseProvisioning), false)
			c.onLand = func() { del.apply(t, f.store, agent.ID) }

			rec := doBootstrapRequest(t, srv, http.MethodPost,
				fmt.Sprintf("/api/v1/projects/%s/agents/%s/env", agent.ProjectID, agent.Slug),
				map[string]interface{}{"env": map[string]string{"API_KEY": "v"}})
			require.NotNil(t, c.lastCreateReq, "dispatch ran: %d %s", rec.Code, rec.Body.String())
			runID := c.lastCreateReq.RunID

			if !del.compensate {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				var resp CreateAgentResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				require.NotNil(t, resp.Agent)
				assert.Equal(t, agent.ID, resp.Agent.ID, "the agent body is returned")
				assert.Empty(t, c.deleteRuns, "no compensating delete")
				return
			}

			warnings := requireDeletedDuringCreate(t, rec, agent.ID)
			assert.Contains(t, warnings, landedRunRemovedWarning,
				"the compensating delete's outcome is reported")
			assert.Equal(t, []string{runID}, c.deleteRuns,
				"exactly one broker delete, scoped to the run that landed")
		})
	}
}
