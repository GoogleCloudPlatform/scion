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
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the reincarnate patch flags (ptone/scion#3302).

// setReincarnatePatchFlags sets the patch flags and restores them on cleanup.
func setReincarnatePatchFlags(t *testing.T, sa, role, model string, thinking int, auth, image string) {
	t.Helper()
	pSA, pRole, pModel, pTL, pAuth, pImage := reincarnateServiceAccount, reincarnateRole, reincarnateModel, reincarnateThinkingLevel, reincarnateHarnessAuth, reincarnateImage
	pBroker, pDryRun := reincarnateBroker, reincarnateDryRun
	t.Cleanup(func() {
		reincarnateServiceAccount, reincarnateRole, reincarnateModel, reincarnateThinkingLevel, reincarnateHarnessAuth, reincarnateImage = pSA, pRole, pModel, pTL, pAuth, pImage
		reincarnateBroker, reincarnateDryRun = pBroker, pDryRun
	})
	reincarnateServiceAccount, reincarnateRole, reincarnateModel, reincarnateThinkingLevel, reincarnateHarnessAuth, reincarnateImage = sa, role, model, thinking, auth, image
}

func TestValidateReincarnatePatchFlags(t *testing.T) {
	cases := []struct {
		name    string
		role    string
		tl      int
		auth    string
		wantErr string
	}{
		{name: "unset", tl: -1},
		{name: "valid", role: "readonly", tl: 50, auth: "vertex-ai"},
		{name: "bad role", role: "admin", tl: -1, wantErr: "invalid role"},
		{name: "thinking too high", tl: 101, wantErr: "--thinking-level"},
		{name: "thinking negative", tl: -2, wantErr: "--thinking-level"},
		{name: "bad harness auth", tl: -1, auth: "oauth", wantErr: "--harness-auth"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setReincarnatePatchFlags(t, "", tc.role, "", tc.tl, tc.auth, "")
			err := validateReincarnatePatchFlags()
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestApplyReincarnatePatchFlags(t *testing.T) {
	setReincarnatePatchFlags(t, "sa-1", "baseline", "m1", 0, "api-key", "img:v1")
	req := &hubclient.ReincarnateAgentRequest{}
	applyReincarnatePatchFlags(req)
	assert.Equal(t, "sa-1", req.ServiceAccount)
	assert.Equal(t, "baseline", req.Role)
	assert.Equal(t, "m1", req.Model)
	require.NotNil(t, req.ThinkingLevel, "thinking level 0 is a value, not unset")
	assert.Equal(t, 0, *req.ThinkingLevel)
	assert.Equal(t, "api-key", req.HarnessAuth)
	assert.Equal(t, "img:v1", req.Image)
	assert.Equal(t, []string{"serviceAccount", "role", "image", "model", "thinkingLevel", "harnessAuth"}, requestedPatchFields(req))

	setReincarnatePatchFlags(t, "", "", "", -1, "", "")
	req = &hubclient.ReincarnateAgentRequest{}
	applyReincarnatePatchFlags(req)
	assert.Nil(t, req.ThinkingLevel)
	assert.Empty(t, requestedPatchFields(req))
}

// patchHub is a fake hub reincarnate endpoint that records each request and
// answers with the given plan.patched list.
type patchHub struct {
	mu       sync.Mutex
	requests []hubclient.ReincarnateAgentRequest
	patched  []string
}

func (h *patchHub) serve(t *testing.T) *HubContext {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/reincarnate") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req hubclient.ReincarnateAgentRequest
		_ = json.Unmarshal(body, &req)
		h.mu.Lock()
		h.requests = append(h.requests, req)
		h.mu.Unlock()
		state, code := "planned", http.StatusOK
		if !req.DryRun {
			state, code = "pending", http.StatusAccepted
		}
		resp := hubclient.ReincarnateAgentResponse{AgentID: "agent-1", Generation: 2, State: state,
			Plan: hubclient.ReincarnationPlan{Patched: h.patched}}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: srv.URL, ProjectID: "proj-1"}
}

// TestReincarnatePatch_OldHubIgnoringPatch_NoRealRequest: a hub that
// predates the patch fields ignores --role. The CLI checks the plan with a
// dry run first and never sends the real request.
func TestReincarnatePatch_OldHubIgnoringPatch_NoRealRequest(t *testing.T) {
	hub := &patchHub{} // answers with no plan.patched
	hubCtx := hub.serve(t)
	setReincarnatePatchFlags(t, "", "readonly", "", -1, "", "")
	reincarnateBroker, reincarnateDryRun = "", false

	err := reincarnateAgentViaHub(hubCtx, "agent-1", "handoff", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support reincarnate patch flags")
	require.Len(t, hub.requests, 1)
	assert.True(t, hub.requests[0].DryRun, "only the dry-run probe may reach the hub")
	assert.Equal(t, "readonly", hub.requests[0].Role)
}

// TestReincarnatePatch_SupportingHub: the probe passes, then the real
// request carries the patch.
func TestReincarnatePatch_SupportingHub(t *testing.T) {
	hub := &patchHub{patched: []string{"role", "thinkingLevel"}}
	hubCtx := hub.serve(t)
	setReincarnatePatchFlags(t, "", "readonly", "", 30, "", "")
	reincarnateBroker, reincarnateDryRun = "", false

	require.NoError(t, reincarnateAgentViaHub(hubCtx, "agent-1", "handoff", false))
	require.Len(t, hub.requests, 2)
	assert.True(t, hub.requests[0].DryRun)
	real := hub.requests[1]
	assert.False(t, real.DryRun)
	assert.Equal(t, "readonly", real.Role)
	require.NotNil(t, real.ThinkingLevel)
	assert.Equal(t, 30, *real.ThinkingLevel)
	assert.Equal(t, "handoff", real.Handoff)
}

// TestReincarnatePatch_DryRunOnOldHubFails: a dry run whose plan lacks the
// patch is reported, not shown as if patched.
func TestReincarnatePatch_DryRunOnOldHubFails(t *testing.T) {
	hub := &patchHub{}
	hubCtx := hub.serve(t)
	setReincarnatePatchFlags(t, "sa-1", "", "", -1, "", "")
	reincarnateBroker, reincarnateDryRun = "", true

	err := reincarnateAgentViaHub(hubCtx, "agent-1", "", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support reincarnate patch flags")
	require.Len(t, hub.requests, 1)
}

// TestReincarnatePatch_NoFlagsSingleRequest: without patch flags there is
// no probe; behaviour is unchanged.
func TestReincarnatePatch_NoFlagsSingleRequest(t *testing.T) {
	hub := &patchHub{}
	hubCtx := hub.serve(t)
	setReincarnatePatchFlags(t, "", "", "", -1, "", "")
	reincarnateBroker, reincarnateDryRun = "", false

	require.NoError(t, reincarnateAgentViaHub(hubCtx, "agent-1", "handoff", false))
	require.Len(t, hub.requests, 1)
	assert.False(t, hub.requests[0].DryRun)
}

// TestReincarnatePatch_CombinesWithBroker: patch flags pass through with
// --broker --dry-run.
func TestReincarnatePatch_CombinesWithBroker(t *testing.T) {
	hub := &patchHub{patched: []string{"model"}}
	hubCtx := hub.serve(t)
	setReincarnatePatchFlags(t, "", "", "m2", -1, "", "")
	reincarnateBroker, reincarnateDryRun = "b2", true

	// The fake hub omits targetBrokerId, so the move check fails after the
	// request; what matters is the request carried both.
	_ = reincarnateAgentViaHub(hubCtx, "agent-1", "", false)
	require.Len(t, hub.requests, 1)
	assert.Equal(t, "b2", hub.requests[0].TargetBroker)
	assert.Equal(t, "m2", hub.requests[0].Model)
}
