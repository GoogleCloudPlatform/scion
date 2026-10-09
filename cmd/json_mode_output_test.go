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
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireSingleJSONDoc asserts that stdout holds exactly one JSON document
// and nothing else (whitespace aside), and returns it decoded.
func requireSingleJSONDoc(t *testing.T, stdout string) map[string]interface{} {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(stdout)))
	var doc map[string]interface{}
	require.NoError(t, dec.Decode(&doc), "stdout is not a JSON document: %q", stdout)
	// Anything after the document other than whitespace is an error here.
	_, err := dec.Token()
	require.ErrorIs(t, err, io.EOF, "stdout has more than the JSON document: %q", stdout)
	return doc
}

// ptone/scion#3494: in JSON mode the stop --all --rm confirmation never
// writes to stdout. With --yes it is skipped; without it, the list and the
// prompt go to stderr. Text mode keeps the list on stdout.
func TestConfirmStopAllRm_JSONModeKeepsStdoutClean(t *testing.T) {
	origConfirm := autoConfirm
	t.Cleanup(func() { autoConfirm = origConfirm })

	t.Run("json/yes", func(t *testing.T) {
		setJSONOutput(t)
		autoConfirm = true
		var ok bool
		stdout, stderr := captureStdIO(t, func() { ok = confirmStopAllRm([]string{"alpha", "beta"}) })
		assert.True(t, ok)
		assert.Empty(t, stdout)
		assert.Empty(t, stderr)
	})
	t.Run("json/prompt", func(t *testing.T) {
		setJSONOutput(t)
		autoConfirm = false
		withStdin(t, "y\n")
		var ok bool
		stdout, stderr := captureStdIO(t, func() { ok = confirmStopAllRm([]string{"alpha", "beta"}) })
		assert.True(t, ok)
		assert.Empty(t, stdout)
		assert.Contains(t, stderr, "The following 2 agent(s) will be stopped and removed:")
		assert.Contains(t, stderr, "  - alpha")
		assert.Contains(t, stderr, "Continue? (y/N): ")
	})
	t.Run("json/prompt declined", func(t *testing.T) {
		setJSONOutput(t)
		autoConfirm = false
		withStdin(t, "\n")
		var ok bool
		stdout, _ := captureStdIO(t, func() { ok = confirmStopAllRm([]string{"alpha"}) })
		assert.False(t, ok, "the default is no")
		assert.Empty(t, stdout)
	})
	t.Run("text", func(t *testing.T) {
		orig := outputFormat
		outputFormat = ""
		t.Cleanup(func() { outputFormat = orig })
		autoConfirm = true
		var ok bool
		stdout, _ := captureStdIO(t, func() { ok = confirmStopAllRm([]string{"alpha"}) })
		assert.True(t, ok)
		assert.Contains(t, stdout, "The following 1 agent(s) will be stopped and removed:")
		assert.Contains(t, stdout, "  - alpha")
	})
}

// localDeleteProject points the delete command at a fresh local project
// (no Hub) and returns its .scion directory.
func localDeleteProject(t *testing.T) string {
	t.Helper()
	orig := saveDeleteTestState()
	t.Cleanup(orig.restore)
	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	noHub = true
	preserveBranch = true
	deleteStopped = false
	deleteForce = false
	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, "agents"), 0o755))
	projectPath = projectDir
	return projectDir
}

// ptone/scion#3494: local-mode scion delete --format json writes only the
// JSON document on stdout; its progress lines do not go there.
func TestDeleteLocal_JSONStdoutIsOnlyTheDocument(t *testing.T) {
	projectDir := localDeleteProject(t)
	agentDir := createAgentDir(t, projectDir, "real-agent")
	setJSONOutput(t)

	var err error
	stdout, _ := captureStdIO(t, func() {
		err = deleteCmd.RunE(deleteCmd, []string{"real-agent", "missing-agent"})
	})
	require.Error(t, err)
	assert.True(t, isReportedInJSON(err), "partial failure exits 1 without an extra banner")
	assert.NoDirExists(t, agentDir)

	doc := requireSingleJSONDoc(t, stdout)
	assert.Equal(t, "partial", doc["status"])
	assert.Equal(t, "delete", doc["command"])
	results, _ := doc["results"].([]interface{})
	require.Len(t, results, 2)
	assert.Equal(t, "success", results[0].(map[string]interface{})["status"])
	assert.Equal(t, "error", results[1].(map[string]interface{})["status"])
}

// stoppedAgentsRuntime is a mock runtime listing stopped agents; deleting
// any agent named in failDelete fails.
func stoppedAgentsRuntime(projectDir string, names []string, failDelete map[string]bool) *runtime.MockRuntime {
	infos := make([]api.AgentInfo, 0, len(names))
	for _, n := range names {
		infos = append(infos, api.AgentInfo{
			Name:            n,
			ContainerID:     "c-" + n,
			Phase:           "stopped",
			ContainerStatus: "Exited (0)",
			ProjectPath:     projectDir,
			Labels:          map[string]string{"scion.name": n},
		})
	}
	return &runtime.MockRuntime{
		ListFunc: func(_ context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			name, ok := filter["scion.name"]
			if !ok {
				return infos, nil
			}
			for _, a := range infos {
				if a.Name == name {
					return []api.AgentInfo{a}, nil
				}
			}
			return nil, nil
		},
		DeleteFunc: func(_ context.Context, ref runtime.RunRef) error {
			for n := range failDelete {
				if ref.ID == "c-"+n {
					return errors.New("container busy")
				}
			}
			return nil
		},
	}
}

// ptone/scion#3494: local scion delete --stopped --format json writes one
// JSON result and, if any delete failed, exits 1 (reported in JSON) like
// the Hub path does (ptone/scion#2894).
func TestDeleteStoppedLocal_JSON(t *testing.T) {
	t.Run("partial failure", func(t *testing.T) {
		projectDir := localDeleteProject(t)
		goodDir := createAgentDir(t, projectDir, "good")
		setJSONOutput(t)
		mgr := agent.NewManager(stoppedAgentsRuntime(projectDir, []string{"good", "bad"}, map[string]bool{"bad": true}))

		var err error
		stdout, stderr := captureStdIO(t, func() { err = deleteStoppedLocal(mgr, projectDir) })
		require.Error(t, err)
		assert.True(t, isReportedInJSON(err))
		assert.Empty(t, stderr)
		assert.NoDirExists(t, goodDir)

		doc := requireSingleJSONDoc(t, stdout)
		assert.Equal(t, "partial", doc["status"])
		assert.Equal(t, "delete", doc["command"])
		results, _ := doc["results"].([]interface{})
		require.Len(t, results, 2)
		r0 := results[0].(map[string]interface{})
		r1 := results[1].(map[string]interface{})
		assert.Equal(t, map[string]interface{}{"agent": "good", "status": "success"}, r0)
		assert.Equal(t, "bad", r1["agent"])
		assert.Equal(t, "error", r1["status"])
		assert.Contains(t, r1["error"], "container busy")
	})
	t.Run("success", func(t *testing.T) {
		projectDir := localDeleteProject(t)
		setJSONOutput(t)
		mgr := agent.NewManager(stoppedAgentsRuntime(projectDir, []string{"good"}, nil))

		var err error
		stdout, _ := captureStdIO(t, func() { err = deleteStoppedLocal(mgr, projectDir) })
		require.NoError(t, err)
		doc := requireSingleJSONDoc(t, stdout)
		assert.Equal(t, "success", doc["status"])
		results, _ := doc["results"].([]interface{})
		require.Len(t, results, 1)
	})
	t.Run("none found", func(t *testing.T) {
		projectDir := localDeleteProject(t)
		setJSONOutput(t)
		mgr := agent.NewManager(stoppedAgentsRuntime(projectDir, nil, nil))

		var err error
		stdout, _ := captureStdIO(t, func() { err = deleteStoppedLocal(mgr, projectDir) })
		require.NoError(t, err)
		doc := requireSingleJSONDoc(t, stdout)
		assert.Equal(t, "success", doc["status"])
		assert.Equal(t, "No stopped agents found.", doc["message"])
		assert.Equal(t, []interface{}{}, doc["results"])
	})
	t.Run("text mode is unchanged", func(t *testing.T) {
		projectDir := localDeleteProject(t)
		orig := outputFormat
		outputFormat = ""
		t.Cleanup(func() { outputFormat = orig })
		mgr := agent.NewManager(stoppedAgentsRuntime(projectDir, []string{"bad"}, map[string]bool{"bad": true}))

		var err error
		stdout, stderr := captureStdIO(t, func() { err = deleteStoppedLocal(mgr, projectDir) })
		require.NoError(t, err, "text mode keeps its exit status")
		assert.Empty(t, stdout)
		assert.Contains(t, stderr, "Failed to delete agent 'bad'")
	})
}
