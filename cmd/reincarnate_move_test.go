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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReincarnateMove_VerdictFromRefusal round-trips a hub move refusal
// through the hub client: the request carries targetBroker, and the CLI
// recovers and prints the verdict from the error details.
func TestReincarnateMove_VerdictFromRefusal(t *testing.T) {
	var gotReq hubclient.ReincarnateAgentRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/reincarnate") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = w.Write([]byte(`{"error":{"code":"unsupported_capability","message":"broker b2 does not support agent move; upgrade it",
			"details":{"verdict":{"eligible":false,
				"sourceBroker":{"id":"id-1","name":"b1"},"targetBroker":{"id":"id-2","name":"b2"},
				"profile":"k8s","runtimeType":"kubernetes",
				"checks":[{"name":"workspace_mode","result":"passed"},
					{"name":"capability","result":"failed","message":"broker b2 does not support agent move; upgrade it"},
					{"name":"capacity","result":"not_evaluated"}]}}}}`))
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	_, err = client.ProjectAgents("proj-1").Reincarnate(context.Background(), "agent-1",
		&hubclient.ReincarnateAgentRequest{DryRun: true, TargetBroker: "b2"})
	require.Error(t, err)
	assert.Equal(t, "b2", gotReq.TargetBroker)
	assert.True(t, gotReq.DryRun)

	v := moveVerdictFromError(err)
	require.NotNil(t, v)
	assert.Equal(t, "id-2", v.TargetBroker.ID)
	require.Len(t, v.Checks, 3)

	var out bytes.Buffer
	printMoveVerdict(&out, v)
	text := out.String()
	assert.Contains(t, text, "Move eligibility: b1 (id-1) -> b2 (id-2)")
	assert.Contains(t, text, "k8s (kubernetes)")
	assert.Contains(t, text, "capability:")
	assert.Contains(t, text, "failed - broker b2 does not support agent move")
	assert.Contains(t, text, "not_evaluated")
}

func TestMoveVerdictFromError_NoVerdict(t *testing.T) {
	assert.Nil(t, moveVerdictFromError(errors.New("plain")))
	assert.Nil(t, moveVerdictFromError(nil))
}

// --broker without --dry-run is refused before any hub call, so an old hub
// that ignores --broker can never run a real in-place reincarnation.
func TestValidateReincarnateBrokerFlags(t *testing.T) {
	err := validateReincarnateBrokerFlags("b2", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--broker requires --dry-run")
	assert.NoError(t, validateReincarnateBrokerFlags("b2", true))
	assert.NoError(t, validateReincarnateBrokerFlags("", false))
}

// The command refuses --broker without --dry-run before resolving a hub.
// The environment points at no real hub, so a regression cannot reach one.
func TestReincarnateCmd_BrokerWithoutDryRun_RefusedBeforeHub(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_HUB_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("SCION_AGENT_NAME", "")
	prevBroker, prevDryRun, prevPath, prevHandoff := reincarnateBroker, reincarnateDryRun, projectPath, reincarnateHandoffFile
	t.Cleanup(func() {
		reincarnateBroker, reincarnateDryRun, projectPath, reincarnateHandoffFile = prevBroker, prevDryRun, prevPath, prevHandoff
	})
	reincarnateBroker, reincarnateDryRun, projectPath, reincarnateHandoffFile = "b2", false, t.TempDir(), ""

	err := reincarnateCmd.RunE(reincarnateCmd, []string{"agent-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--broker requires --dry-run")
}

// TestReincarnateMove_OldHubIgnoringBroker_Fails: a hub that predates
// --broker answers a dry run with a plain plan and no targetBrokerId. The
// CLI must fail rather than present that plan as the move.
func TestReincarnateMove_OldHubIgnoringBroker_Fails(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !strings.HasSuffix(r.URL.Path, "/reincarnate") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agentId":"agent-1","generation":2,"state":"planned","plan":{}}`))
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: srv.URL, ProjectID: "proj-1"}

	prevBroker, prevDryRun := reincarnateBroker, reincarnateDryRun
	t.Cleanup(func() { reincarnateBroker, reincarnateDryRun = prevBroker, prevDryRun })

	// --broker without --dry-run never reaches the hub, which would run a
	// real in-place reincarnation.
	reincarnateBroker, reincarnateDryRun = "b2", false
	err = reincarnateAgentViaHub(hubCtx, "agent-1", "handoff", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--broker requires --dry-run")
	assert.Equal(t, 0, calls, "the hub must receive no request")

	reincarnateBroker, reincarnateDryRun = "b2", true
	err = reincarnateAgentViaHub(hubCtx, "agent-1", "", false)
	require.Error(t, err)
	assert.Equal(t, "this hub does not support --broker; upgrade the hub", err.Error())
	assert.Equal(t, 1, calls)

	// Without --broker the same response is a normal dry-run plan.
	reincarnateBroker = ""
	require.NoError(t, reincarnateAgentViaHub(hubCtx, "agent-1", "", false))
}
