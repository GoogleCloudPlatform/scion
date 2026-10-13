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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scion create labels the phase of a provision-only agent the same way
// scion list does (ptone/scion#2929).
func TestCreateOutput_HubProvisionedOnlyPhase(t *testing.T) {
	for _, tc := range []struct {
		provisionedOnly bool
		want            string
	}{
		{true, "Phase: created (not started)\n"},
		{false, "Phase: created\n"},
	} {
		var buf bytes.Buffer
		writeHubCreateText(&buf, "po-agent", &hubclient.CreateAgentResponse{
			Agent: &hubclient.Agent{Slug: "po-agent", Phase: "created", ProvisionedOnly: tc.provisionedOnly},
		}, "")
		assert.Contains(t, buf.String(), tc.want)
	}
}

func TestHubAgentToAgentInfo_ProvisionedOnly(t *testing.T) {
	info := hubAgentToAgentInfo(hubclient.Agent{Name: "a", Phase: "created", ProvisionedOnly: true})
	assert.True(t, info.ProvisionedOnly)
}

func TestDisplayAgents_ProvisionedOnlyLabel(t *testing.T) {
	prev := outputFormat
	outputFormat = ""
	t.Cleanup(func() { outputFormat = prev })

	agents := []api.AgentInfo{
		{Name: "po-agent", Template: "default", Phase: "created", ProvisionedOnly: true},
		{Name: "full-agent", Template: "default", Phase: "created"},
		// A stale flag on an agent that has left created adds no suffix.
		{Name: "run-agent", Template: "default", Phase: "running", ProvisionedOnly: true},
	}
	var err error
	out := captureStdout(t, func() { err = displayAgents(agents, false, true) })
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	// Header and three rows; the start hint goes to stderr.
	require.Len(t, lines, 4, out)
	for _, l := range lines[1:] {
		if strings.HasPrefix(l, "po-agent") {
			assert.Contains(t, l, "created (not started)")
		} else {
			assert.NotContains(t, l, "not started")
		}
	}
}

// scion list ends with a hint to start provision-only agents
// (ptone/scion#2875).
func TestProvisionedOnlyListHint(t *testing.T) {
	po := func(project, name string) api.AgentInfo {
		return api.AgentInfo{Name: name, Project: project, Phase: "created", ProvisionedOnly: true}
	}
	var many []api.AgentInfo
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		many = append(many, po("p", n))
	}
	for _, tc := range []struct {
		name   string
		agents []api.AgentInfo
		all    bool
		want   string
	}{
		{"none", []api.AgentInfo{{Name: "a", Phase: "running"}}, false, ""},
		{"stale flag after leaving created", []api.AgentInfo{{Name: "a", Phase: "running", ProvisionedOnly: true}}, false, ""},
		{"one", []api.AgentInfo{{Name: "b", Phase: "running"}, po("p", "a")}, false,
			"Agent 'a' is provisioned but not started. Run 'scion start a' to start it."},
		{"one with --all", []api.AgentInfo{po("p", "a")}, true,
			"Agent 'p/a' is provisioned but not started. Run 'scion start a' in project p to start it."},
		{"several", []api.AgentInfo{po("p", "a"), po("p", "b")}, false,
			"2 agents are provisioned but not started (a, b). Run 'scion start NAME' to start one."},
		{"several with --all", []api.AgentInfo{po("p", "a"), po("q", "b")}, true,
			"2 agents are provisioned but not started (p/a, q/b). Run 'scion start NAME' to start one."},
		{"capped", many, false,
			"7 agents are provisioned but not started (a, b, c, d, e and 2 more). Run 'scion start NAME' to start one."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, provisionedOnlyListHint(tc.agents, tc.all))
		})
	}
}

func TestDisplayAgents_ProvisionedOnlyHint(t *testing.T) {
	prev := outputFormat
	outputFormat = ""
	t.Cleanup(func() { outputFormat = prev })

	agents := []api.AgentInfo{{Name: "po-agent", Phase: "created", ProvisionedOnly: true}}
	var err error
	stdout, stderr := captureStdoutStderr(t, func() { err = displayAgents(agents, false, false) })
	require.NoError(t, err)
	assert.Contains(t, stdout, "created (not started)")
	assert.NotContains(t, stdout, "scion start")
	assert.Equal(t, "Agent 'po-agent' is provisioned but not started. Run 'scion start po-agent' to start it.", strings.TrimSpace(stderr))

	outputFormat = "json"
	stdout, stderr = captureStdoutStderr(t, func() { err = displayAgents(agents, false, false) })
	require.NoError(t, err)
	assert.NotContains(t, stdout+stderr, "scion start")
}
