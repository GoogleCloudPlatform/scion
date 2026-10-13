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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// currentAgentEnvKeys are the environment variables that identify the agent
// a command runs inside, or point it at a Hub. An agent container sets them
// ambiently, so tests set every one explicitly. SCION_CLI_MODE and
// SCION_HOST_UID are cleared too, so the command resolves its Hub and
// project as it would on a workstation or in CI.
var currentAgentEnvKeys = []string{
	"SCION_AGENT_ID",
	"SCION_AGENT_NAME",
	"SCION_AGENT_SLUG",
	"SCION_AGENT_MODE",
	"SCION_AGENT_BRANCH",
	"SCION_AGENT_HOME",
	"SCION_AGENT_SEQ",
	"SCION_AGENT_WORKSPACE",
	"SCION_PROJECT",
	"SCION_PROJECT_ID",
	"SCION_HUB_ENDPOINT",
	"SCION_HUB_URL",
	"SCION_AUTH_TOKEN",
	"SCION_TRANSPORT_TOKEN",
	"SCION_TRANSPORT_TOKEN_FILE",
	"SCION_HUB_TOKEN",
	"SCION_CLI_MODE",
	"SCION_HOST_UID",
}

// setCurrentAgentEnv clears every currentAgentEnvKeys variable, then sets
// the current agent's identity. Empty values leave a variable cleared, so
// setCurrentAgentEnv(t, "", "", "", "") is "not inside an agent".
func setCurrentAgentEnv(t *testing.T, name, slug, id, projectID string) {
	t.Helper()
	for _, key := range currentAgentEnvKeys {
		t.Setenv(key, "")
	}
	t.Setenv("SCION_AGENT_NAME", name)
	t.Setenv("SCION_AGENT_SLUG", slug)
	t.Setenv("SCION_AGENT_ID", id)
	t.Setenv("SCION_PROJECT_ID", projectID)
}

const (
	selfAgentName = "self-agent"
	selfAgentID   = "6c1f2a9e-0d4b-4e7a-9b3c-2f8e5d1a7c40"
	selfProjectID = "project-self-4843"
)

func TestCurrentAgentTarget(t *testing.T) {
	for _, tc := range []struct {
		name            string
		envName         string
		envSlug         string
		envID           string
		envProjectID    string
		targets         []string
		targetProjectID string
		want            string
	}{
		{name: "matches name", envName: selfAgentName, envProjectID: selfProjectID,
			targets: []string{selfAgentName}, targetProjectID: selfProjectID, want: selfAgentName},
		{name: "matches name given unslugified", envName: "Self Agent", envProjectID: selfProjectID,
			targets: []string{selfAgentName}, targetProjectID: selfProjectID, want: selfAgentName},
		{name: "matches slug", envSlug: selfAgentName, envProjectID: selfProjectID,
			targets: []string{selfAgentName}, targetProjectID: selfProjectID, want: selfAgentName},
		{name: "matches ID", envName: selfAgentName, envID: selfAgentID, envProjectID: selfProjectID,
			targets: []string{selfAgentID}, targetProjectID: selfProjectID, want: selfAgentID},
		{name: "first self target among several", envName: selfAgentName, envProjectID: selfProjectID,
			targets: []string{"other-agent", selfAgentName}, targetProjectID: selfProjectID, want: selfAgentName},
		{name: "other agent", envName: selfAgentName, envID: selfAgentID, envProjectID: selfProjectID,
			targets: []string{"other-agent"}, targetProjectID: selfProjectID, want: ""},
		{name: "not inside an agent", envProjectID: selfProjectID,
			targets: []string{selfAgentName}, targetProjectID: selfProjectID, want: ""},
		{name: "same name in another known project", envName: selfAgentName, envProjectID: selfProjectID,
			targets: []string{selfAgentName}, targetProjectID: "project-other", want: ""},
		{name: "target project unknown fails closed", envName: selfAgentName, envProjectID: selfProjectID,
			targets: []string{selfAgentName}, targetProjectID: "", want: selfAgentName},
		{name: "own project unknown fails closed", envName: selfAgentName,
			targets: []string{selfAgentName}, targetProjectID: "project-other", want: selfAgentName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setCurrentAgentEnv(t, tc.envName, tc.envSlug, tc.envID, tc.envProjectID)
			assert.Equal(t, tc.want, currentAgentTarget(tc.targets, tc.targetProjectID))
		})
	}
}

func TestRefuseCurrentAgentTarget(t *testing.T) {
	setCurrentAgentEnv(t, selfAgentName, selfAgentName, selfAgentID, selfProjectID)

	err := refuseCurrentAgentTarget([]string{"other-agent", selfAgentName}, selfProjectID, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `agent "self-agent" is the current agent`)
	assert.Contains(t, err.Error(), "--self is required")
	assert.Contains(t, err.Error(), "re-run the same command with --self added")
	assert.NotContains(t, err.Error(), "scion delete", "the error must not print a rebuilt command")

	assert.NoError(t, refuseCurrentAgentTarget([]string{selfAgentName}, selfProjectID, true),
		"--self allows the current agent")
	assert.NoError(t, refuseCurrentAgentTarget([]string{"other-agent"}, selfProjectID, false),
		"other agents are not refused")
}
