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
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
)

func withForceFlag(t *testing.T, force bool) {
	t.Helper()
	prev := reincarnateForce
	t.Cleanup(func() { reincarnateForce = prev })
	reincarnateForce = force
}

// forceHub answers reincarnate requests with dryRunBody (dryStatus) for a
// dry run and realStatus/realBody otherwise, recording each request body.
func forceHub(t *testing.T, dryStatus int, dryRunBody string, realStatus int, realBody string) (*HubContext, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		if body["dryRun"] == true {
			w.WriteHeader(dryStatus)
			_, _ = w.Write([]byte(dryRunBody))
			return
		}
		w.WriteHeader(realStatus)
		_, _ = w.Write([]byte(realBody))
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: srv.URL, ProjectID: "proj-1"}, &bodies
}

const recreateVerdictBody = `{"agentId":"agent-1","generation":2,"state":"planned","plan":{},"sourceBrokerId":"b1","targetBrokerId":"b2",
	"moveVerdict":{"eligible":true,"sourceBroker":{"id":"b1"},"targetBroker":{"id":"b2"},"transferMode":"recreate",
	"checks":[{"name":"same_export","result":"waived","message":"not needed: the workspace is recreated on the target (--force)"},
		{"name":"workspace_recreatable","result":"passed","message":"fresh clone of example.com/r on the target"}],
	"consequences":[{"kind":"lost","item":"local-work","text":"uncommitted files"},
		{"kind":"kept","item":"identity","text":"agent ID"},
		{"kind":"recreated","item":"workspace","text":"fresh clone"},
		{"kind":"carried","item":"handoff","text":"the handoff"}]}}`

func TestValidateReincarnateForceFlag(t *testing.T) {
	err := validateReincarnateForceFlag(true, "")
	require.Error(t, err)
	var uerr *usageError
	assert.True(t, errors.As(err, &uerr), "a usage error, got %T", err)
	assert.Contains(t, err.Error(), "--force needs --broker")
	assert.NoError(t, validateReincarnateForceFlag(true, "b2"))
	assert.NoError(t, validateReincarnateForceFlag(false, ""))
}

// A hub that ignores forceMove answers an export-eligible move without a
// transfer mode: the CLI stops at the handshake.
func TestReincarnateForce_OldHubHandshakeFails(t *testing.T) {
	withBrokerFlags(t, "b2", false)
	withForceFlag(t, true)
	hubCtx, bodies := forceHub(t, http.StatusOK,
		`{"agentId":"agent-1","generation":2,"state":"planned","plan":{},"sourceBrokerId":"b1","targetBrokerId":"b2","moveVerdict":{"eligible":true,"sourceBroker":{"id":"b1"},"targetBroker":{"id":"b2"},"checks":[]}}`,
		http.StatusAccepted, `{}`)
	err := reincarnateAgentViaHub(hubCtx, "agent-1", "handoff", false)
	require.Error(t, err)
	assert.Equal(t, "this hub does not support --force; upgrade the hub", err.Error())
	require.Len(t, *bodies, 1, "only the handshake dry run reaches the hub")
	assert.Equal(t, true, (*bodies)[0]["forceMove"])
}

// --dry-run --force is its own handshake.
func TestReincarnateForce_OldHubDryRunFails(t *testing.T) {
	withBrokerFlags(t, "b2", true)
	withForceFlag(t, true)
	hubCtx, _ := forceHub(t, http.StatusOK,
		`{"agentId":"agent-1","generation":2,"state":"planned","plan":{},"sourceBrokerId":"b1","targetBrokerId":"b2","moveVerdict":{"eligible":true,"sourceBroker":{"id":"b1"},"targetBroker":{"id":"b2"},"checks":[]}}`,
		http.StatusAccepted, `{}`)
	err := reincarnateAgentViaHub(hubCtx, "agent-1", "", false)
	require.Error(t, err)
	assert.Equal(t, "this hub does not support --force; upgrade the hub", err.Error())
}

// A recreate verdict passes the handshake, and both requests carry
// forceMove; the hub accepts the forced move.
func TestReincarnateForce_HandshakeThenRealRequest(t *testing.T) {
	withBrokerFlags(t, "b2", false)
	withForceFlag(t, true)
	hubCtx, bodies := forceHub(t, http.StatusOK, recreateVerdictBody,
		http.StatusAccepted, `{"agentId":"agent-1","generation":2,"state":"pending","plan":{},"sourceBrokerId":"b1","targetBrokerId":"b2","transferMode":"recreate"}`)
	err := reincarnateAgentViaHub(hubCtx, "agent-1", "handoff", false)
	require.NoError(t, err)
	require.Len(t, *bodies, 2)
	for i, b := range *bodies {
		assert.Equal(t, true, b["forceMove"], "request %d carries forceMove", i)
		assert.Equal(t, "b2", b["targetBroker"], "request %d", i)
	}
}

// Without --force the request does not carry forceMove.
func TestReincarnateForce_NotSentWithoutFlag(t *testing.T) {
	withBrokerFlags(t, "b2", true)
	withForceFlag(t, false)
	hubCtx, bodies := forceHub(t, http.StatusOK,
		`{"agentId":"agent-1","generation":2,"state":"planned","plan":{},"sourceBrokerId":"b1","targetBrokerId":"b2","moveVerdict":{"eligible":true,"sourceBroker":{"id":"b1"},"targetBroker":{"id":"b2"},"checks":[]}}`,
		http.StatusAccepted, `{}`)
	require.NoError(t, reincarnateAgentViaHub(hubCtx, "agent-1", "", false))
	require.Len(t, *bodies, 1)
	assert.NotContains(t, (*bodies)[0], "forceMove")
}

func TestPrintMoveVerdict_RecreateModeAndConsequences(t *testing.T) {
	var resp hubclient.ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal([]byte(recreateVerdictBody), &resp))
	var out bytes.Buffer
	printMoveVerdict(&out, resp.MoveVerdict)
	text := out.String()
	assert.Contains(t, text, "Transfer mode:")
	assert.Contains(t, text, "recreate (the workspace is recreated on the target)")
	assert.Contains(t, text, "waived (--force) - not needed")
	assert.NotContains(t, text, " waived -", "waived renders with its --force label")
	// Consequences are grouped kept, carried, recreated, lost.
	idx := func(s string) int { return strings.Index(text, s) }
	for _, s := range []string{"kept       identity:", "carried    handoff:", "recreated  workspace:", "lost       local-work:"} {
		require.GreaterOrEqual(t, idx(s), 0, "missing %q in:\n%s", s, text)
	}
	assert.Less(t, idx("kept       identity:"), idx("carried    handoff:"))
	assert.Less(t, idx("carried    handoff:"), idx("recreated  workspace:"))
	assert.Less(t, idx("recreated  workspace:"), idx("lost       local-work:"))
	assert.True(t, strings.HasSuffix(text, "Uncommitted and unpushed work in the agent's workspace will not survive this move.\n"), text)
}

func TestPrintMoveVerdict_ExportModeHasNoLossLine(t *testing.T) {
	var out bytes.Buffer
	printMoveVerdict(&out, &hubclient.MoveVerdict{
		Eligible: true, TransferMode: "export",
		Checks: []hubclient.MoveCheck{{Name: "same_export", Result: "passed"}},
	})
	assert.Contains(t, out.String(), "export (the workspace is carried through the shared NFS export)")
	assert.NotContains(t, out.String(), "will not survive")
}

// A message-less workspace_recreatable pass outside recreate mode
// says why, for a refused shared-agent verdict and an eligible export
// verdict alike; a recreate verdict keeps the hub's message.
func TestPrintMoveVerdict_WorkspaceRecreatableNotNeeded(t *testing.T) {
	const line = "workspace_recreatable:       passed - not needed (workspace carried by the shared export)"
	refused := &hubclient.MoveVerdict{Checks: []hubclient.MoveCheck{
		{Name: "workspace_on_export", Result: "passed", Message: "confirmed at move time"},
		{Name: "workspace_recreatable", Result: "passed"},
		{Name: "target_health", Result: "failed", Message: "broker b2 reports its NFS export mount unhealthy"},
	}}
	eligible := &hubclient.MoveVerdict{Eligible: true, TransferMode: "export", Checks: []hubclient.MoveCheck{
		{Name: "workspace_recreatable", Result: "passed"},
	}}
	for name, v := range map[string]*hubclient.MoveVerdict{"refused shared agent": refused, "eligible export": eligible} {
		var out bytes.Buffer
		printMoveVerdict(&out, v)
		assert.Contains(t, out.String(), line, name)
	}
	var out bytes.Buffer
	printMoveVerdict(&out, &hubclient.MoveVerdict{Eligible: true, TransferMode: "recreate", Checks: []hubclient.MoveCheck{
		{Name: "workspace_recreatable", Result: "passed", Message: "fresh clone of r on the target"},
	}})
	assert.Contains(t, out.String(), "passed - fresh clone of r on the target")
	assert.NotContains(t, out.String(), "not needed (workspace carried")
}

// refusalError returns the hub client error for a refusal body.
func refusalError(t *testing.T, status int, body string) error {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	_, err = client.ProjectAgents("proj-1").Reincarnate(context.Background(), "agent-1",
		&hubclient.ReincarnateAgentRequest{DryRun: true, TargetBroker: "b2"})
	require.Error(t, err)
	return err
}

// The hint depends on forceWouldApply alone: the hub decides.
func TestPrintMoveForceHint(t *testing.T) {
	refusal := func(check string, would bool) string {
		w := ""
		if would {
			w = `,"forceWouldApply":true`
		}
		return `{"error":{"code":"conflict","message":"m","details":{"verdict":{"eligible":false,"sourceBroker":{"id":"b1"},"targetBroker":{"id":"b2"},
			"checks":[{"name":"workspace_mode","result":"passed"},{"name":"` + check + `","result":"failed","message":"m"},{"name":"workspace_recreatable","result":"not_evaluated"}]}` + w + `}}}`
	}
	cases := []struct {
		name  string
		check string
		would bool
		force bool
		want  bool
	}{
		{"same_export, would apply", "same_export", true, false, true},
		{"workspace_on_export, would apply", "workspace_on_export", true, false, true},
		{"target_profile, would apply", "target_profile", true, false, true},
		{"any other check, would apply", "capacity", true, false, true},
		{"already forced", "same_export", true, true, false},
		{"hub says force would not apply", "same_export", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := refusalError(t, http.StatusConflict, refusal(tc.check, tc.would))
			var out bytes.Buffer
			printMoveForceHint(&out, err, tc.force)
			if tc.want {
				assert.Equal(t, moveForceHint+"\n", out.String())
			} else {
				assert.Empty(t, out.String())
			}
		})
	}
	var out bytes.Buffer
	printMoveForceHint(&out, errors.New("plain"), false)
	assert.Empty(t, out.String())
}

// --dry-run --force --json writes the response, with the transfer
// mode and consequences, as JSON on stdout, and none of the human-only
// lines.
func TestReincarnateForce_DryRunJSON(t *testing.T) {
	withBrokerFlags(t, "b2", true)
	withForceFlag(t, true)
	prevFormat := outputFormat
	t.Cleanup(func() { outputFormat = prevFormat })
	outputFormat = "json"
	hubCtx, _ := forceHub(t, http.StatusOK, recreateVerdictBody, http.StatusAccepted, `{}`)
	var runErr error
	stdout := captureStdout(t, func() { runErr = reincarnateAgentViaHub(hubCtx, "agent-1", "", false) })
	require.NoError(t, runErr)
	var resp hubclient.ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal([]byte(stdout), &resp), stdout)
	require.NotNil(t, resp.MoveVerdict)
	assert.Equal(t, "recreate", resp.MoveVerdict.TransferMode)
	assert.Len(t, resp.MoveVerdict.Consequences, 4)
	for _, human := range []string{"Transfer mode:", "Move consequences:", "will not survive", "--force was not needed", "Dry run only"} {
		assert.NotContains(t, stdout, human)
	}
}

// In human output, an export verdict under --force says --force was
// not needed.
func TestReincarnateForce_ExportVerdictSaysForceNotNeeded(t *testing.T) {
	withBrokerFlags(t, "b2", true)
	withForceFlag(t, true)
	hubCtx, _ := forceHub(t, http.StatusOK,
		`{"agentId":"agent-1","generation":2,"state":"planned","plan":{},"sourceBrokerId":"b1","targetBrokerId":"b2","moveVerdict":{"eligible":true,"sourceBroker":{"id":"b1"},"targetBroker":{"id":"b2"},"transferMode":"export","checks":[{"name":"same_export","result":"passed"}]}}`,
		http.StatusAccepted, `{}`)
	var runErr error
	stdout := captureStdout(t, func() { runErr = reincarnateAgentViaHub(hubCtx, "agent-1", "", false) })
	require.NoError(t, runErr)
	assert.Contains(t, stdout, "--force was not needed: the workspace is carried through the shared NFS export.")
	assert.Contains(t, stdout, "export (the workspace is carried through the shared NFS export)")
	assert.NotContains(t, stdout, "will not survive")
}

// Brokers advertise forced moves in the static set, its names and the
// registration list alike.
func TestStaticCapabilities_AgentMoveRecreateAdvertised(t *testing.T) {
	rt := runtime.NewDockerRuntime()
	assert.True(t, runtimebroker.StaticCapabilities(rt, "").AgentMoveRecreate)
	assert.True(t, slices.Contains(runtimebroker.StaticCapabilityNames(rt), "agentMoveRecreate"))
	assert.True(t, slices.Contains(brokerRegistrationCapabilities(), "agentMoveRecreate"))
}

// moveRefusalBody is a refused move's 409 with forceWouldApply set as given.
func moveRefusalBody(would bool) string {
	w := ""
	if would {
		w = `,"forceWouldApply":true`
	}
	return `{"error":{"code":"conflict","message":"the workspace is not on a shared export","details":{"verdict":{"eligible":false,"sourceBroker":{"id":"b1"},"targetBroker":{"id":"b2"},
		"checks":[{"name":"same_export","result":"failed","message":"m"},{"name":"workspace_recreatable","result":"not_evaluated"}]}` + w + `}}}`
}

// The pre-force handoff warning has this exact text.
func TestMoveForceHint_Text(t *testing.T) {
	assert.Equal(t, "A forced move does not preserve this agent's home or workspace: it restarts on the target with a fresh home and the target's workspace. "+
		"Put anything worth keeping into the handoff, then re-run with --force (preview with --dry-run --force).", moveForceHint)
}

// A move refused without --force that --force could make prints the
// handoff warning on stderr in both human and --json output; stdout carries
// no part of it, and in --json mode no human verdict lines either. There
// is no warning when the hub says --force would not apply, or with --force.
func TestReincarnateMove_ForceWarningOnStderrInBothModes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		json  bool
		would bool
		force bool
		want  bool
	}{
		{"human, would apply", false, true, false, true},
		{"json, would apply", true, true, false, true},
		{"human, would not apply", false, false, false, false},
		{"json, would not apply", true, false, false, false},
		{"json, already forced", true, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withBrokerFlags(t, "b2", false)
			withForceFlag(t, tc.force)
			prevFormat := outputFormat
			t.Cleanup(func() { outputFormat = prevFormat })
			if tc.json {
				outputFormat = "json"
			} else {
				outputFormat = ""
			}
			hubCtx, _ := forceHub(t, http.StatusConflict, moveRefusalBody(tc.would), http.StatusConflict, moveRefusalBody(tc.would))
			var runErr error
			stdout, stderr := captureStdoutStderr(t, func() { runErr = reincarnateAgentViaHub(hubCtx, "agent-1", "handoff", false) })
			require.Error(t, runErr)
			assert.NotContains(t, stdout, moveForceHint, "stdout never carries the warning")
			if tc.want {
				assert.Contains(t, stderr, moveForceHint)
			} else {
				assert.NotContains(t, stderr, moveForceHint)
			}
			if tc.json {
				assert.NotContains(t, stdout, "same_export", "no human verdict lines in --json mode")
			}
		})
	}
}
