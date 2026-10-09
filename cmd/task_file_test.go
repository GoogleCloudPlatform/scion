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
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTaskFile(t *testing.T, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "brief.md")
	require.NoError(t, os.WriteFile(p, content, 0o600))
	return p
}

func TestApplyTaskFile(t *testing.T) {
	brief := strings.Repeat("a line of the brief\n", 64*1024/20+1)

	t.Run("no flag leaves the task", func(t *testing.T) {
		got, err := applyTaskFile("do it", "", nil)
		require.NoError(t, err)
		assert.Equal(t, "do it", got)
	})
	t.Run("file becomes the task intact", func(t *testing.T) {
		got, err := applyTaskFile("", writeTaskFile(t, []byte(brief)), nil)
		require.NoError(t, err)
		assert.Equal(t, brief, got)
	})
	t.Run("task arguments come first", func(t *testing.T) {
		got, err := applyTaskFile("  read the brief ", writeTaskFile(t, []byte("the brief")), nil)
		require.NoError(t, err)
		assert.Equal(t, "read the brief\n\nthe brief", got)
	})
	t.Run("stdin", func(t *testing.T) {
		got, err := applyTaskFile("", "-", strings.NewReader(brief))
		require.NoError(t, err)
		assert.Equal(t, brief, got)
	})
	t.Run("exactly the limit is accepted", func(t *testing.T) {
		content := strings.Repeat("x", maxTaskFileBytes)
		got, err := applyTaskFile("", writeTaskFile(t, []byte(content)), nil)
		require.NoError(t, err)
		assert.Len(t, got, maxTaskFileBytes)
	})
	t.Run("file over the limit is rejected", func(t *testing.T) {
		_, err := applyTaskFile("", writeTaskFile(t, []byte(strings.Repeat("x", maxTaskFileBytes+1))), nil)
		require.Error(t, err)
		assert.True(t, errors.Is(err, errTaskFileTooLarge))
		assert.Contains(t, err.Error(), "96 KiB")
	})
	t.Run("stdin over the limit is rejected", func(t *testing.T) {
		_, err := applyTaskFile("", "-", strings.NewReader(strings.Repeat("x", maxTaskFileBytes+1)))
		require.Error(t, err)
		assert.True(t, errors.Is(err, errTaskFileTooLarge))
		assert.Contains(t, err.Error(), "stdin")
	})
	t.Run("file with task arguments over the limit is rejected", func(t *testing.T) {
		_, err := applyTaskFile("prefix", writeTaskFile(t, []byte(strings.Repeat("x", maxTaskFileBytes-2))), nil)
		require.Error(t, err)
		assert.True(t, errors.Is(err, errTaskFileTooLarge))
		assert.Contains(t, err.Error(), "task arguments")
	})
	t.Run("invalid UTF-8 is rejected", func(t *testing.T) {
		_, err := applyTaskFile("", writeTaskFile(t, []byte{'o', 'k', 0xff, 0xfe}), nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not valid UTF-8")
	})
	t.Run("empty file is rejected", func(t *testing.T) {
		_, err := applyTaskFile("", writeTaskFile(t, []byte(" \n")), nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "the file is empty or whitespace only")
	})
	t.Run("empty stdin is rejected as input", func(t *testing.T) {
		_, err := applyTaskFile("", "-", strings.NewReader("\n\t "))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--task-file stdin: the input is empty or whitespace only")
	})
	t.Run("invalid UTF-8 on stdin is rejected as input", func(t *testing.T) {
		_, err := applyTaskFile("", "-", strings.NewReader("ok\xff"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "the input is not valid UTF-8")
	})
	t.Run("missing file is rejected", func(t *testing.T) {
		_, err := applyTaskFile("", filepath.Join(t.TempDir(), "nope.md"), nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--task-file")
	})
}

func TestTaskFileFlag_RegisteredOnStartAndCreate(t *testing.T) {
	for _, c := range []string{"start", "create"} {
		cmd := startCmd
		if c == "create" {
			cmd = createCmd
		}
		f := cmd.Flags().Lookup("task-file")
		require.NotNil(t, f, "%s has no --task-file flag", c)
		assert.Contains(t, f.Usage, "96 KiB")
		assert.Contains(t, f.Usage, "~/.scion/task.md")
	}
}

// The flag is checked before any hub or local work, and a bad file is a
// usage error.
func TestTaskFileFlag_RejectsBeforeLaunch(t *testing.T) {
	resetHubStartGlobals(t)
	origTF, origCfg := taskFilePath, inlineConfigPath
	t.Cleanup(func() { taskFilePath, inlineConfigPath = origTF, origCfg })
	big := writeTaskFile(t, []byte(strings.Repeat("x", maxTaskFileBytes+1)))

	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"start", func() error { return RunAgent(startCmd, []string{"agent-x"}, false) }},
		{"create", func() error { return createCmd.RunE(createCmd, []string{"agent-x"}) }},
	} {
		t.Run(tc.name+"_too_large", func(t *testing.T) {
			taskFilePath, inlineConfigPath = big, ""
			err := tc.run()
			require.Error(t, err)
			assert.True(t, errors.Is(err, errTaskFileTooLarge), "err = %v", err)
			var ue *usageError
			assert.True(t, errors.As(err, &ue), "err = %T, want a usage error", err)
		})
		t.Run(tc.name+"_both_stdin", func(t *testing.T) {
			taskFilePath, inlineConfigPath = "-", "-"
			err := tc.run()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "cannot both read from stdin")
		})
	}
}

// A 64 KiB brief reaches the hub intact in the create request's task.
func TestStartAgentViaHub_SendsLargeTaskIntact(t *testing.T) {
	resetHubStartGlobals(t)
	const projectID, agentName = "proj-taskfile", "taskfile-agent"
	brief := strings.Repeat("brief <line> & more\n", 64*1024/20+1)
	task, err := applyTaskFile("", writeTaskFile(t, []byte(brief)), nil)
	require.NoError(t, err)

	stub := newHubStartStub(t, projectID, agentName, "")
	captureStdIO(t, func() {
		err = startAgentViaHub(nil, stub.hubCtx(t, projectID), agentName, task, false, nil)
	})
	require.NoError(t, err)
	require.Equal(t, 1, stub.createCalls)
	assert.Equal(t, brief, stub.createBody["task"])
}

// The largest task, made of the characters JSON escapes the most, still
// fits, with room for the rest of the request, in the control channel
// message that carries the create request from the Hub to a broker.
func TestMaxTaskFitsBrokerControlMessage(t *testing.T) {
	worst := strings.Repeat("<>&\x01", maxTaskFileBytes/4)
	require.Len(t, worst, maxTaskFileBytes)
	task, err := applyTaskFile("", writeTaskFile(t, []byte(worst)), nil)
	require.NoError(t, err)

	body, err := json.Marshal(hub.RemoteCreateAgentRequest{
		Slug:      "agent",
		Name:      "agent",
		ProjectID: "project",
		Config:    &hub.RemoteAgentConfig{Task: task},
	})
	require.NoError(t, err)
	msg, err := json.Marshal(wsprotocol.RequestEnvelope{
		Type:      "request",
		RequestID: "request",
		Method:    "POST",
		Path:      "/api/v1/agents",
		Body:      body,
	})
	require.NoError(t, err)
	const restOfRequest = 192 * 1024
	assert.LessOrEqual(t, len(msg)+restOfRequest, wsprotocol.DefaultMaxMessageSize,
		"a %d-byte task makes a %d-byte control message", len(task), len(msg))
}
