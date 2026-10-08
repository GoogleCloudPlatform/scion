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
	// Header, three rows, a blank line, and the start hint.
	require.Len(t, lines, 6, out)
	for _, l := range lines[1:4] {
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
	po := func(name string) api.AgentInfo {
		return api.AgentInfo{Name: name, Phase: "created", ProvisionedOnly: true}
	}
	for _, tc := range []struct {
		name   string
		agents []api.AgentInfo
		want   string
	}{
		{"none", []api.AgentInfo{{Name: "a", Phase: "running"}}, ""},
		{"stale flag after leaving created", []api.AgentInfo{{Name: "a", Phase: "running", ProvisionedOnly: true}}, ""},
		{"one", []api.AgentInfo{po("a"), {Name: "b", Phase: "running"}},
			"Agent 'a' is provisioned but not started. Run 'scion start a' to start it."},
		{"several", []api.AgentInfo{po("a"), po("b")},
			"2 agents are provisioned but not started (a, b). Run 'scion start NAME' to start one."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, provisionedOnlyListHint(tc.agents))
		})
	}
}

func TestDisplayAgents_ProvisionedOnlyHint(t *testing.T) {
	prev := outputFormat
	outputFormat = ""
	t.Cleanup(func() { outputFormat = prev })

	var err error
	out := captureStdout(t, func() {
		err = displayAgents([]api.AgentInfo{{Name: "po-agent", Phase: "created", ProvisionedOnly: true}}, false, false)
	})
	require.NoError(t, err)
	assert.Contains(t, out, "created (not started)")
	assert.True(t, strings.HasSuffix(strings.TrimSpace(out), "Run 'scion start po-agent' to start it."), out)

	outputFormat = "json"
	out = captureStdout(t, func() {
		err = displayAgents([]api.AgentInfo{{Name: "po-agent", Phase: "created", ProvisionedOnly: true}}, false, false)
	})
	require.NoError(t, err)
	assert.NotContains(t, out, "scion start")
}
