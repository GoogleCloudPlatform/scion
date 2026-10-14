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
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupSelfDeleteViaHub prepares a run of the delete command against a mock
// Hub for project selfProjectID, with the current agent's identity set from
// name, id and ownProjectID (empty values clear it). It returns the project
// directory and the DELETE requests the Hub receives, keyed by agent name.
func setupSelfDeleteViaHub(t *testing.T, name, id, ownProjectID string) (string, map[string]url.Values) {
	t.Helper()
	orig := saveDeleteTestState()
	t.Cleanup(orig.restore)

	setCurrentAgentEnv(t, name, name, id, ownProjectID)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	noHub = false
	preserveBranch = true
	deleteStopped = false
	deleteForce = false
	deleteCleanupOnly = false
	deleteSelf = false

	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, "agents"), 0o755))
	projectPath = projectDir
	// projectPath stands in for an explicit --project flag, whose own
	// project ID wins over SCION_PROJECT_ID (ptone/scion#3123).
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "project-id"), []byte(selfProjectID+"\n"), 0o644))

	server, queries := newDeleteQueryRecordingHubServer(t, selfProjectID, nil)
	t.Cleanup(server.Close)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	t.Setenv("SCION_HUB_URL", server.URL)
	return projectDir, queries
}

// runDelete runs the delete command's RunE with a copy of args (RunE
// slugifies them in place) and returns its stderr and error.
func runDelete(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var runErr error
	stderr := captureStderr(t, func() {
		runErr = deleteCmd.RunE(deleteCmd, append([]string(nil), args...))
	})
	return stderr, runErr
}

// Acceptance 1: inside an agent, naming the current agent without --self is
// refused before any delete, and the message names --self.
func TestDeleteCmd_CurrentAgentWithoutSelfRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		targets []string
	}{
		{name: "by name", targets: []string{selfAgentName}},
		{name: "by unslugified name", targets: []string{"Self Agent"}},
		{name: "by ID", targets: []string{selfAgentID}},
		{name: "among other agents", targets: []string{"other-agent", selfAgentName, "third-agent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectDir, queries := setupSelfDeleteViaHub(t, selfAgentName, selfAgentID, selfProjectID)
			agentDir := createAgentDir(t, projectDir, selfAgentName)
			otherDir := createAgentDir(t, projectDir, "other-agent")

			_, err := runDelete(t, tc.targets...)
			require.Error(t, err, "deleting the current agent without --self must be refused")
			assert.Contains(t, err.Error(), "is the current agent")
			assert.Contains(t, err.Error(), "--self is required")
			assert.Empty(t, queries, "no agent may be deleted on the Hub when the command is refused")
			assert.DirExists(t, agentDir)
			assert.DirExists(t, otherDir, "a refusal leaves the other named agents in place too")
		})
	}
}

// Acceptance 2: with --self, deleting the current agent proceeds as before.
func TestDeleteCmd_CurrentAgentWithSelfDeletes(t *testing.T) {
	projectDir, queries := setupSelfDeleteViaHub(t, selfAgentName, selfAgentID, selfProjectID)
	agentDir := createAgentDir(t, projectDir, selfAgentName)
	deleteSelf = true

	stderr, err := runDelete(t, selfAgentName)
	require.NoError(t, err, "--self must allow deleting the current agent")
	assert.Contains(t, queries, selfAgentName, "the Hub must receive the DELETE")
	assert.Contains(t, stderr, "Agent 'self-agent' deleted via Hub.")
	assert.NoDirExists(t, agentDir)
}

// Acceptance 3: deleting another agent from inside an agent is unchanged.
func TestDeleteCmd_OtherAgentFromInsideAgentDeletes(t *testing.T) {
	_, queries := setupSelfDeleteViaHub(t, selfAgentName, selfAgentID, selfProjectID)

	_, err := runDelete(t, "other-agent")
	require.NoError(t, err)
	assert.Contains(t, queries, "other-agent")
}

// An agent that targets another project (here the --project ID differs from
// its own SCION_PROJECT_ID) is not refused for an agent there that has its
// name.
func TestDeleteCmd_SameNameInOtherProjectDeletes(t *testing.T) {
	_, queries := setupSelfDeleteViaHub(t, selfAgentName, selfAgentID, "project-agent-home")

	_, err := runDelete(t, selfAgentName)
	require.NoError(t, err, "an agent with the same name in another project is not the current agent")
	assert.Contains(t, queries, selfAgentName)
}

// Acceptance 4: outside an agent, delete behaves as before.
func TestDeleteCmd_OutsideAgentUnchanged(t *testing.T) {
	_, queries := setupSelfDeleteViaHub(t, "", "", "")

	_, err := runDelete(t, selfAgentName)
	require.NoError(t, err)
	assert.Contains(t, queries, selfAgentName)
}

// --cleanup-only removes the agent's own files, so it is refused for the
// current agent without --self as well.
func TestDeleteCmd_CleanupOnlyCurrentAgentRefused(t *testing.T) {
	projectDir, queries := setupSelfDeleteViaHub(t, selfAgentName, selfAgentID, selfProjectID)
	agentDir := createAgentDir(t, projectDir, selfAgentName)
	deleteCleanupOnly = true

	_, err := runDelete(t, selfAgentName)
	require.Error(t, err, "--cleanup-only of the current agent without --self must be refused")
	assert.Contains(t, err.Error(), "--self is required")
	assert.DirExists(t, agentDir, "the current agent's files are kept")
	assert.Empty(t, queries)
}

// Without a Hub the target project is unknown, so a name match alone
// refuses (fail closed): none of the named agents is deleted locally.
func TestDeleteCmd_LocalCurrentAgentWithoutSelfRefused(t *testing.T) {
	projectDir := setupSelfDeleteLocal(t)
	agentDir := createAgentDir(t, projectDir, selfAgentName)
	otherDir := createAgentDir(t, projectDir, "other-agent")

	_, err := runDelete(t, "other-agent", selfAgentName)
	require.Error(t, err, "a local delete of the current agent without --self must be refused")
	assert.Contains(t, err.Error(), "--self is required")
	assert.DirExists(t, agentDir)
	assert.DirExists(t, otherDir, "a refusal leaves the other named agents in place too")
}

// Without a Hub, --self deletes the current agent as usual.
func TestDeleteCmd_LocalCurrentAgentWithSelfDeletes(t *testing.T) {
	projectDir := setupSelfDeleteLocal(t)
	agentDir := createAgentDir(t, projectDir, selfAgentName)
	deleteSelf = true

	_, err := runDelete(t, selfAgentName)
	require.NoError(t, err, "--self must allow a local delete of the current agent")
	assert.NoDirExists(t, agentDir)
}

// setupSelfDeleteLocal prepares a local (no Hub) run of the delete command
// inside agent selfAgentName of project selfProjectID, and returns the
// project directory.
func setupSelfDeleteLocal(t *testing.T) string {
	t.Helper()
	orig := saveDeleteTestState()
	t.Cleanup(orig.restore)

	setCurrentAgentEnv(t, selfAgentName, selfAgentName, selfAgentID, selfProjectID)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	noHub = true
	preserveBranch = true
	deleteStopped = false
	deleteForce = false
	deleteCleanupOnly = false
	deleteSelf = false

	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, "agents"), 0o755))
	projectPath = projectDir
	return projectDir
}

// Without a Hub, --cleanup-only --force removes local files without any Hub
// check. The guard runs first, so the current agent's files are kept unless
// --self is given.
func TestDeleteCmd_LocalCleanupOnlyForceCurrentAgentRefused(t *testing.T) {
	projectDir := setupSelfDeleteLocal(t)
	agentDir := createAgentDir(t, projectDir, selfAgentName)
	deleteCleanupOnly = true
	deleteForce = true

	_, err := runDelete(t, selfAgentName)
	require.Error(t, err, "--cleanup-only --force of the current agent without a Hub must be refused without --self")
	assert.Contains(t, err.Error(), "--self is required")
	assert.DirExists(t, agentDir, "the current agent's files are kept")
}
