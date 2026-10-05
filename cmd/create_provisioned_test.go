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
	require.Len(t, lines, 4, out)
	for _, l := range lines[1:] {
		if strings.HasPrefix(l, "po-agent") {
			assert.Contains(t, l, "created (not started)")
		} else {
			assert.NotContains(t, l, "not started")
		}
	}
}
