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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createTestState captures and restores package-level vars for test isolation.
type createTestState struct {
	home        string
	projectPath string
	noHub       bool
}

func saveCreateTestState() createTestState {
	return createTestState{
		home:        os.Getenv("HOME"),
		projectPath: projectPath,
		noHub:       noHub,
	}
}

func (s createTestState) restore() {
	_ = os.Setenv("HOME", s.home)
	projectPath = s.projectPath
	noHub = s.noHub
}

func TestCreateCmd_HarnessFlagDefaultsToEmpty(t *testing.T) {
	// Regression test: the --harness flag on createCmd must default to ""
	// (not "h"). Previously StringVar was called with "h" as the default
	// value instead of the empty string, causing harnessConfigFlag to be
	// non-empty even when neither --harness nor --harness-config was passed.
	f := createCmd.Flags().Lookup("harness")
	require.NotNil(t, f, "--harness flag must be registered on createCmd")
	assert.Equal(t, "", f.DefValue, "--harness default value must be empty string")
}

func TestCreateAgent_DuplicateReturnsError(t *testing.T) {
	orig := saveCreateTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	noHub = true

	// Set up project directory with an existing agent
	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, "agents"), 0755))
	projectPath = projectDir

	createAgentDir(t, projectDir, "my-agent")

	// Attempt to create an agent with the same name — should fail
	err := createCmd.RunE(createCmd, []string{"my-agent"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

// TestCreateOutput_SaysNotStarted checks that every scion create output says
// the agent was provisioned but not started and names the command that
// starts it.
func TestCreateOutput_SaysNotStarted(t *testing.T) {
	const name = "my-agent"
	const startCmd = "scion start my-agent"

	t.Run("hint", func(t *testing.T) {
		hint := createNotStartedHint(name)
		assert.Contains(t, hint, "not started")
		assert.Contains(t, hint, startCmd)
	})

	t.Run("local text", func(t *testing.T) {
		var buf bytes.Buffer
		writeLocalCreateResult(&buf, name)
		out := buf.String()
		assert.Contains(t, out, "Agent 'my-agent' created successfully.")
		assert.Contains(t, out, "not started")
		assert.Contains(t, out, startCmd)
	})

	t.Run("local json", func(t *testing.T) {
		r := localCreateResult(name)
		assert.Equal(t, "success", r.Status)
		assert.Equal(t, "create", r.Command)
		assert.Equal(t, name, r.Agent)
		assert.Contains(t, r.Message, startCmd)
		assert.Equal(t, false, r.Details["started"])
		assert.Equal(t, true, r.Details["provisioned"])
		assert.Equal(t, startCmd, r.Details["startCommand"])
	})

	t.Run("hub json details", func(t *testing.T) {
		details := map[string]interface{}{"phase": "created"}
		addCreateNotStartedDetails(details, name, true)
		assert.Equal(t, false, details["started"])
		assert.Equal(t, true, details["provisioned"])
		assert.Equal(t, startCmd, details["startCommand"])
		assert.Equal(t, "created", details["phase"], "existing details are kept")
	})
}

// TestCreateOutput_Hub checks the text and JSON results of a create through a
// Hub, with and without a provisioning failure warning.
func TestCreateOutput_Hub(t *testing.T) {
	const name = "my-agent"
	const startCmd = "scion start my-agent"
	agent := &hubclient.Agent{Slug: name, Phase: "created", RuntimeBrokerName: "broker-a"}
	provisionWarning := api.ProvisionFailedWarningPrefix + "connection refused"

	t.Run("text provisioned", func(t *testing.T) {
		var buf bytes.Buffer
		writeHubCreateText(&buf, name, &hubclient.CreateAgentResponse{
			Agent:    agent,
			Warnings: []string{"some other warning"},
		}, "/p/agents/my-agent")
		out := buf.String()
		assert.Contains(t, out, "Agent 'my-agent' created via Hub on broker broker-a.")
		assert.Contains(t, out, "Agent directory: /p/agents/my-agent")
		lines := strings.Split(strings.TrimSpace(out), "\n")
		require.GreaterOrEqual(t, len(lines), 2)
		assert.Equal(t, "Warning: some other warning", lines[len(lines)-2])
		assert.Equal(t, createNotStartedHint(name), lines[len(lines)-1], "the hint comes last, after the warnings")
	})

	t.Run("text not provisioned", func(t *testing.T) {
		var buf bytes.Buffer
		writeHubCreateText(&buf, name, &hubclient.CreateAgentResponse{
			Agent:    agent,
			Warnings: []string{provisionWarning},
		}, "")
		out := buf.String()
		assert.NotContains(t, out, "is provisioned")
		assert.NotContains(t, out, "Agent directory:")
		lines := strings.Split(strings.TrimSpace(out), "\n")
		assert.Equal(t, "Warning: "+provisionWarning, lines[len(lines)-2])
		assert.Equal(t, createNotProvisionedHint(name), lines[len(lines)-1])
		assert.Contains(t, lines[len(lines)-1], startCmd)
	})

	t.Run("text without agent", func(t *testing.T) {
		var buf bytes.Buffer
		writeHubCreateText(&buf, name, &hubclient.CreateAgentResponse{}, "")
		assert.Equal(t, "Agent 'my-agent' created via Hub.\n"+createNotStartedHint(name)+"\n", buf.String())
	})

	t.Run("json provisioned", func(t *testing.T) {
		r := hubCreateResult(name, &hubclient.CreateAgentResponse{Agent: agent})
		assert.Contains(t, r.Message, "is provisioned but not started")
		assert.Equal(t, false, r.Details["started"])
		assert.Equal(t, true, r.Details["provisioned"])
		assert.Equal(t, startCmd, r.Details["startCommand"])
		assert.Equal(t, name, r.Details["slug"])
		assert.Equal(t, "broker-a", r.Details["runtimeBrokerName"])
	})

	t.Run("json not provisioned", func(t *testing.T) {
		r := hubCreateResult(name, &hubclient.CreateAgentResponse{Agent: agent, Warnings: []string{provisionWarning}})
		assert.NotContains(t, r.Message, "is provisioned")
		assert.Contains(t, r.Message, "not fully provisioned")
		assert.Equal(t, false, r.Details["provisioned"])
		assert.Equal(t, []string{provisionWarning}, r.Warnings)
	})
}

func TestCreateCmd_HelpSaysNotStarted(t *testing.T) {
	assert.Contains(t, createCmd.Short, "without starting")
	assert.Contains(t, createCmd.Long, "scion start <agent-name>")
}
