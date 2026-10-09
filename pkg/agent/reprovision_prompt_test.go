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

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reprovisionPromptFixture provisions generation N of an empty-per-agent
// agent with genNTask as its task, on a runtime that records each Start's
// RunConfig. It returns the manager, the start function and prompt.md's path.
func reprovisionPromptFixture(t *testing.T, genNTask string) (Manager, func(task string, resume bool) runtime.RunConfig, string) {
	t.Helper()
	return reprovisionPromptFixtureWithTemplate(t, genNTask, "")
}

// reprovisionPromptFixtureWithTemplate is reprovisionPromptFixture with the
// default template's scion-agent.json replaced by templateJSON when it is
// not empty.
func reprovisionPromptFixtureWithTemplate(t *testing.T, genNTask, templateJSON string) (Manager, func(task string, resume bool) runtime.RunConfig, string) {
	t.Helper()
	tmpDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)
	projectScionDir := setupEmptyPerAgentStartProject(t, filepath.Join(tmpDir, "project"), "")
	if templateJSON != "" {
		if err := os.WriteFile(filepath.Join(projectScionDir, "templates", "default", "scion-agent.json"), []byte(templateJSON), 0644); err != nil {
			t.Fatal(err)
		}
	}

	var captured runtime.RunConfig
	var ran bool
	rt := emptyPerAgentStartRuntime(&captured, &ran)
	rt.NameFunc = func() string { return "docker" }
	mgr := NewManager(rt)
	if _, err := mgr.Provision(context.Background(), api.StartOptions{
		Name: "worker", ProjectPath: projectScionDir, NoAuth: true,
		EmptyPerAgentWorkspace: true, Task: genNTask,
	}); err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	start := func(task string, resume bool) runtime.RunConfig {
		t.Helper()
		captured, ran = runtime.RunConfig{}, false
		if _, err := mgr.Start(context.Background(), api.StartOptions{
			Name: "worker", ProjectPath: projectScionDir, NoAuth: true,
			EmptyPerAgentWorkspace: true, Task: task, Resume: resume,
			Env: map[string]string{"SCION_AGENT_ID": "agent-1", "SCION_PROJECT_ID": "proj-1"},
		}); err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		if !ran {
			t.Fatal("runtime Run was not called")
		}
		return captured
	}
	return mgr, start, filepath.Join(projectScionDir, "agents", "worker", "prompt.md")
}

func reprovisionWorker(t *testing.T, mgr Manager, promptPath, task string) {
	t.Helper()
	projectScionDir := filepath.Dir(filepath.Dir(filepath.Dir(promptPath)))
	if _, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name: "worker", ProjectPath: projectScionDir, NoAuth: true,
		EmptyPerAgentWorkspace: true, Task: task,
	}); err != nil {
		t.Fatalf("Reprovision failed: %v", err)
	}
}

func readPrompt(t *testing.T, promptPath string) string {
	t.Helper()
	data, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("read prompt.md: %v", err)
	}
	return string(data)
}

// TestReprovision_NoTask_LaterTasklessStartDoesNotReplayOldTask pins
// ptone/scion#3985: after a reprovision whose request carries no task, a
// later start with no task delivers no task, not generation N's.
func TestReprovision_NoTask_LaterTasklessStartDoesNotReplayOldTask(t *testing.T) {
	mgr, start, promptPath := reprovisionPromptFixture(t, "gen 1 task")
	reprovisionWorker(t, mgr, promptPath, "")

	if got := readPrompt(t, promptPath); got != "" {
		t.Fatalf("prompt.md after Reprovision = %q, want empty", got)
	}
	if cfg := start("", false); cfg.Task != "" {
		t.Fatalf("task-less start delivered %q, want no task", cfg.Task)
	}
}

// countArg returns how many entries of args equal want.
func countArg(args []string, want string) int {
	n := 0
	for _, a := range args {
		if a == want {
			n++
		}
	}
	return n
}

// TestReprovision_WithTask_DeliversNewTask pins that a reprovision that
// carries a task leaves that task, and only that task, for the next start:
// a task-less start (the reincarnation's own start never ran) delivers the
// new task. The subtests for a start carrying the same task (the
// reincarnation's start) document that the staged prompt.md does not make
// Start deliver the task twice; that guarantee comes from Start's structure
// (prompt.md is only a fallback for a start with no task, and the single
// task value goes to either RunConfig.Task or the task flag), not from
// Reprovision.
func TestReprovision_WithTask_DeliversNewTask(t *testing.T) {
	const newTask = "gen 2 preamble and handoff"

	t.Run("task-less start after reprovision", func(t *testing.T) {
		mgr, start, promptPath := reprovisionPromptFixture(t, "gen 1 task")
		reprovisionWorker(t, mgr, promptPath, newTask)
		if got := readPrompt(t, promptPath); got != newTask {
			t.Fatalf("prompt.md after Reprovision = %q, want %q", got, newTask)
		}
		if cfg := start("", false); cfg.Task != newTask {
			t.Fatalf("task-less start delivered %q, want %q", cfg.Task, newTask)
		}
	})

	t.Run("start carrying the same task", func(t *testing.T) {
		mgr, start, promptPath := reprovisionPromptFixture(t, "gen 1 task")
		reprovisionWorker(t, mgr, promptPath, newTask)
		cfg := start(newTask, false)
		if cfg.Task != newTask {
			t.Fatalf("start delivered %q, want %q", cfg.Task, newTask)
		}
		if n := countArg(cfg.CommandArgs, newTask); n != 0 {
			t.Fatalf("task also appears %d times in CommandArgs %v, want it delivered only as RunConfig.Task", n, cfg.CommandArgs)
		}
		if got := readPrompt(t, promptPath); got != newTask {
			t.Fatalf("prompt.md after Start = %q, want %q", got, newTask)
		}
	})

	t.Run("start carrying the same task, task_flag harness", func(t *testing.T) {
		mgr, start, promptPath := reprovisionPromptFixtureWithTemplate(t, "gen 1 task",
			`{"default_harness_config": "test-harness", "task_flag": "--input", "command_args": ["run"]}`)
		reprovisionWorker(t, mgr, promptPath, newTask)
		cfg := start(newTask, false)
		if cfg.Task != "" {
			t.Fatalf("RunConfig.Task = %q, want empty with a task flag", cfg.Task)
		}
		if n := countArg(cfg.CommandArgs, newTask); n != 1 {
			t.Fatalf("task appears %d times in CommandArgs %v, want exactly once", n, cfg.CommandArgs)
		}
	})
}

// TestStart_WithoutReprovision_StoredTaskBehaviourUnchanged is the control
// for ptone/scion#3985: without a reprovision, a stopped agent's stored
// task still applies to its next fresh start, and a resume never re-sends
// it.
func TestStart_WithoutReprovision_StoredTaskBehaviourUnchanged(t *testing.T) {
	_, start, promptPath := reprovisionPromptFixture(t, "gen 1 task")
	if cfg := start("", false); cfg.Task != "gen 1 task" {
		t.Fatalf("fresh start delivered %q, want the stored task", cfg.Task)
	}
	if cfg := start("", true); cfg.Task != "" {
		t.Fatalf("resume delivered %q, want no task", cfg.Task)
	}
	if got := readPrompt(t, promptPath); got != "gen 1 task" {
		t.Fatalf("prompt.md = %q, want the stored task kept", got)
	}
}

// TestReprovision_PromptWriteFailureLeavesSharedDirRecord pins the step
// order in Reprovision: a failed prompt.md write fails the reprovision
// before the shared dir backend change is recorded, so the record stays as
// it was ("recorded only after provisioning succeeds"). The hub then
// re-renders the previous config with the change cleared, which matches.
func TestReprovision_PromptWriteFailureLeavesSharedDirRecord(t *testing.T) {
	f, mountRoot := newPerDirFixture(t)
	f.writeRawGlobalSettings(t, perDirSettingsYAML(mountRoot, ""))
	workspace := filepath.Join(f.tmpDir, "checkout")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	opts := perDirStartOpts(f, "agent", "local")
	opts.Workspace = workspace
	_, err := NewManager(newSDSMockRuntime("docker", &sdsCapture{})).Start(context.Background(), opts)
	require.NoError(t, err)
	before := perDirRecord(t, f, "agent")
	require.Equal(t, &sharedDirStorageRecord{Backend: "local"}, before)

	// A directory at prompt.md's path makes the prompt write fail.
	promptPath := filepath.Join(config.ResolveAgentDir(f.projectScionDir, "agent"), "prompt.md")
	require.NoError(t, os.RemoveAll(promptPath))
	require.NoError(t, os.MkdirAll(promptPath, 0o755))

	_, err = NewManager(newSDSMockRuntime("docker", &sdsCapture{})).Reprovision(context.Background(), api.StartOptions{
		Name: "agent", ProjectPath: f.projectScionDir, Profile: "local", Workspace: workspace,
		SharedDirs: opts.SharedDirs, SharedDirBackendChanges: map[string]string{"notes": "nfs"},
		Task: "gen 2 task",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prompt.md")
	assert.Equal(t, before, perDirRecord(t, f, "agent"), "the shared dir backend change must not be recorded")
}
