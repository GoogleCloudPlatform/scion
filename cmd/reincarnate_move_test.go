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
// TestReincarnateMove_OldHubIgnoringBroker_Fails: a hub that predates
// --broker answers a dry run with a plain plan and no targetBrokerId. The
// CLI must fail rather than present that plan as the move.
func TestReincarnateMove_OldHubIgnoringBroker_Fails(t *testing.T) {
	calls := 0
	var dryRuns []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			DryRun bool `json:"dryRun"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		dryRuns = append(dryRuns, body.DryRun)
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

	// A real --broker move is dry-run first; the old hub's answer stops it
	// there, so the hub never receives a real request (which it would run
	// as an in-place reincarnation).
	reincarnateBroker, reincarnateDryRun = "b2", false
	err = reincarnateAgentViaHub(hubCtx, "agent-1", "handoff", false)
	require.Error(t, err)
	assert.Equal(t, "this hub does not support --broker; upgrade the hub", err.Error())
	assert.Equal(t, []bool{true}, dryRuns, "only the handshake dry run reaches the hub")

	reincarnateBroker, reincarnateDryRun = "b2", true
	err = reincarnateAgentViaHub(hubCtx, "agent-1", "", false)
	require.Error(t, err)
	assert.Equal(t, "this hub does not support --broker; upgrade the hub", err.Error())
	assert.Equal(t, 2, calls)

	// Without --broker the same response is a normal dry-run plan.
	reincarnateBroker = ""
	require.NoError(t, reincarnateAgentViaHub(hubCtx, "agent-1", "", false))
}

// moveHub answers reincarnate requests with dryRunBody for a dry run and
// realBody otherwise (status for each), recording the dry-run flags.
func moveHub(t *testing.T, dryStatus int, dryRunBody string, realBody string) (*HubContext, *[]bool) {
	t.Helper()
	var dryRuns []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			DryRun bool `json:"dryRun"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		dryRuns = append(dryRuns, body.DryRun)
		w.Header().Set("Content-Type", "application/json")
		if body.DryRun {
			w.WriteHeader(dryStatus)
			_, _ = w.Write([]byte(dryRunBody))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(realBody))
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: srv.URL, ProjectID: "proj-1"}, &dryRuns
}

func withBrokerFlags(t *testing.T, broker string, dryRun bool) {
	t.Helper()
	prevBroker, prevDryRun := reincarnateBroker, reincarnateDryRun
	t.Cleanup(func() { reincarnateBroker, reincarnateDryRun = prevBroker, prevDryRun })
	reincarnateBroker, reincarnateDryRun = broker, dryRun
}

// An eligible move: the handshake dry run, then the real request.
func TestReincarnateMove_HandshakeThenMove(t *testing.T) {
	withBrokerFlags(t, "b2", false)
	hubCtx, dryRuns := moveHub(t, http.StatusOK,
		`{"agentId":"agent-1","generation":2,"state":"planned","plan":{},"sourceBrokerId":"b1","targetBrokerId":"b2","moveVerdict":{"eligible":true,"sourceBroker":{"id":"b1"},"targetBroker":{"id":"b2"},"checks":[]}}`,
		`{"agentId":"agent-1","generation":2,"state":"pending","plan":{},"sourceBrokerId":"b1","targetBrokerId":"b2"}`)
	require.NoError(t, reincarnateAgentViaHub(hubCtx, "agent-1", "handoff", false))
	assert.Equal(t, []bool{true, false}, *dryRuns)
}

// A refused move stops at the handshake: no real request.
func TestReincarnateMove_HandshakeRefusalStops(t *testing.T) {
	withBrokerFlags(t, "b2", false)
	hubCtx, dryRuns := moveHub(t, http.StatusConflict,
		`{"error":{"code":"conflict","message":"target broker b2 does not serve this project","details":{"verdict":{"eligible":false,"sourceBroker":{"id":"b1"},"targetBroker":{"id":"b2"},"checks":[{"name":"access","result":"failed"}]}}}}`,
		`{}`)
	err := reincarnateAgentViaHub(hubCtx, "agent-1", "handoff", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not possible")
	assert.Equal(t, []bool{true}, *dryRuns)
}

// A hub that names the target but gives no verdict for another broker does
// not support the move.
func TestReincarnateMove_HandshakeWithoutVerdictStops(t *testing.T) {
	withBrokerFlags(t, "b2", false)
	hubCtx, dryRuns := moveHub(t, http.StatusOK,
		`{"agentId":"agent-1","generation":2,"state":"planned","plan":{},"sourceBrokerId":"b1","targetBrokerId":"b2"}`,
		`{}`)
	err := reincarnateAgentViaHub(hubCtx, "agent-1", "handoff", false)
	require.Error(t, err)
	assert.Equal(t, "this hub does not support --broker; upgrade the hub", err.Error())
	assert.Equal(t, []bool{true}, *dryRuns)
}

// A patched --broker move makes one dry run of the whole request (patch and
// target broker), checks both the patch and the move on it, then sends the
// real request.
func TestReincarnateMove_PatchedMoveSingleDryRun(t *testing.T) {
	setReincarnatePatchFlags(t, "", "", "", -1, "", "img:v2")
	withBrokerFlags(t, "b2", false)
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		if body["dryRun"] == true {
			_, _ = w.Write([]byte(`{"agentId":"agent-1","generation":2,"state":"planned","plan":{"patched":["image"]},"sourceBrokerId":"b1","targetBrokerId":"b2","moveVerdict":{"eligible":true,"sourceBroker":{"id":"b1"},"targetBroker":{"id":"b2"},"checks":[]}}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"agentId":"agent-1","generation":2,"state":"pending","plan":{"patched":["image"]},"sourceBrokerId":"b1","targetBrokerId":"b2"}`))
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	require.NoError(t, reincarnateAgentViaHub(&HubContext{Client: client, Endpoint: srv.URL, ProjectID: "proj-1"}, "agent-1", "handoff", false))
	require.Len(t, bodies, 2, "one dry run, then the real request")
	for i, b := range bodies {
		assert.Equal(t, "b2", b["targetBroker"], "request %d carries the target broker", i)
		assert.Equal(t, "img:v2", b["image"], "request %d carries the patch", i)
	}
	assert.Equal(t, true, bodies[0]["dryRun"])
	assert.Nil(t, bodies[1]["dryRun"])
}

// A patched --broker move whose dry run does not show the patch applied
// stops before the real request, like a patched in-place reincarnation.
func TestReincarnateMove_PatchedMoveIgnoredPatchStops(t *testing.T) {
	setReincarnatePatchFlags(t, "", "", "", -1, "", "img:v2")
	withBrokerFlags(t, "b2", false)
	hubCtx, dryRuns := moveHub(t, http.StatusOK,
		`{"agentId":"agent-1","generation":2,"state":"planned","plan":{},"sourceBrokerId":"b1","targetBrokerId":"b2","moveVerdict":{"eligible":true,"sourceBroker":{"id":"b1"},"targetBroker":{"id":"b2"},"checks":[]}}`,
		`{}`)
	require.Error(t, reincarnateAgentViaHub(hubCtx, "agent-1", "handoff", false))
	assert.Equal(t, []bool{true}, *dryRuns)
}

// TestReincarnateHelp_BrokerParagraph pins the --broker help: real moves
// are supported (no "dry-run only" wording), and the help names the move
// preconditions and points to the move guide.
func TestReincarnateHelp_BrokerParagraph(t *testing.T) {
	// Compare with whitespace collapsed, so a reflow of the paragraph does
	// not break the pins.
	long := strings.Join(strings.Fields(reincarnateCmd.Long), " ")
	assert.NotContains(t, long, "Only --dry-run is supported", "--broker moves are real; the help must not say dry-run only")
	for _, want := range []string{
		"--broker <name|id>",
		"same NFS export",
		"support agent move",
		"agent home is regenerated",
		"refused before anything changes",
		"dispatch unless the broker auto-provides",
		"already serves its project",
		"Patch flags combine with --broker",
		`"Moving an Agent to Another Runtime Broker"`,
	} {
		assert.Contains(t, long, want)
	}
}
