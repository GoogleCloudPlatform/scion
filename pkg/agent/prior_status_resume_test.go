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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func hasEnvKey(env []string, key string) bool {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

// TestStart_FreshStartWithPriorCompleted_SendsCompletionNoticeOnly verifies
// that a fresh start/restart (Resume=false, Task="") of an agent whose prior
// run completed passes only a completion notice, preserves prompt.md on disk,
// and does not set SCION_INITIAL_TASK=1 (ptone/scion#8900).
func TestStart_FreshStartWithPriorCompleted_SendsCompletionNoticeOnly(t *testing.T) {
	_, projectScionDir := newResumePhaseTestFixture(t, func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		return nil, nil
	})
	agentDir := filepath.Join(projectScionDir, "agents", "resume-test")
	const origPrompt = "Read your brief at /brief.md and follow it"
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.md"), []byte(origPrompt), 0o644); err != nil {
		t.Fatal(err)
	}

	ts := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	var got runtime.RunConfig
	mgr := NewManager(&runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
		RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
			got = cfg
			return "mock-id", nil
		},
	})
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "resume-test",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		Resume:      false,
		PriorStatus: &api.PriorStatus{
			Activity: "completed",
			Summary:  "PR #123 opened",
			At:       ts,
		},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	wantNotice := "Your previous run completed at 2026-11-23T12:00:00Z (PR #123 opened). Wait for new instructions; do not restart the original task unless instructed."
	if got.Task != wantNotice {
		t.Errorf("RunConfig.Task =\n  %q\nwant:\n  %q", got.Task, wantNotice)
	}
	if hasEnvKey(got.Env, envInitialTask) {
		t.Errorf("SCION_INITIAL_TASK must not be set for a completion notice; env=%v", got.Env)
	}
	diskPrompt, err := os.ReadFile(filepath.Join(agentDir, "prompt.md"))
	if err != nil {
		t.Fatalf("read prompt.md: %v", err)
	}
	if string(diskPrompt) != origPrompt {
		t.Errorf("prompt.md on disk was modified: got %q, want %q", string(diskPrompt), origPrompt)
	}
}

// TestStart_EphemeralResumeWithPriorCompleted_SendsCompletionNoticeOnly
// verifies that resuming an ephemeral agent whose prior run completed sends
// the completion notice instead of replaying prompt.md.
func TestStart_EphemeralResumeWithPriorCompleted_SendsCompletionNoticeOnly(t *testing.T) {
	_, projectScionDir := newResumePhaseTestFixture(t, func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		return nil, nil
	})
	agentDir := filepath.Join(projectScionDir, "agents", "resume-test")
	const origPrompt = "the original task"
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.md"), []byte(origPrompt), 0o644); err != nil {
		t.Fatal(err)
	}

	var got runtime.RunConfig
	mgr := NewManager(ephemeralMockRuntime{&runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
		RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
			got = cfg
			return "mock-id", nil
		},
	}})
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "resume-test",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		Resume:      true,
		Persistence: api.PersistenceEphemeral,
		PriorStatus: &api.PriorStatus{
			Activity: "completed",
			Summary:  "all checks green",
		},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got.Resume {
		t.Error("RunConfig.Resume = true, want false for ephemeral agent")
	}
	wantNotice := "Your previous run completed (all checks green). Wait for new instructions; do not restart the original task unless instructed."
	if got.Task != wantNotice {
		t.Errorf("RunConfig.Task = %q, want %q", got.Task, wantNotice)
	}
	if hasEnvKey(got.Env, envInitialTask) {
		t.Error("SCION_INITIAL_TASK must not be set for a completion notice")
	}
	diskPrompt, err := os.ReadFile(filepath.Join(agentDir, "prompt.md"))
	if err != nil {
		t.Fatalf("read prompt.md: %v", err)
	}
	if string(diskPrompt) != origPrompt {
		t.Errorf("prompt.md on disk was modified: got %q, want %q", string(diskPrompt), origPrompt)
	}
}

// TestStart_ExplicitTaskOverridesPriorCompleted verifies that an explicit new
// task still wins over PriorStatus, overwrites prompt.md, and sets
// SCION_INITIAL_TASK=1.
func TestStart_ExplicitTaskOverridesPriorCompleted(t *testing.T) {
	_, projectScionDir := newResumePhaseTestFixture(t, func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		return nil, nil
	})
	agentDir := filepath.Join(projectScionDir, "agents", "resume-test")
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.md"), []byte("old task"), 0o644); err != nil {
		t.Fatal(err)
	}

	var got runtime.RunConfig
	mgr := NewManager(&runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
		RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
			got = cfg
			return "mock-id", nil
		},
	})
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "resume-test",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		Resume:      false,
		Task:        "new explicit task",
		PriorStatus: &api.PriorStatus{
			Activity: "completed",
			Summary:  "old summary",
		},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got.Task != "new explicit task" {
		t.Errorf("RunConfig.Task = %q, want %q", got.Task, "new explicit task")
	}
	if !hasEnvKey(got.Env, envInitialTask) {
		t.Error("SCION_INITIAL_TASK must be set for an explicit task")
	}
	diskPrompt, err := os.ReadFile(filepath.Join(agentDir, "prompt.md"))
	if err != nil {
		t.Fatalf("read prompt.md: %v", err)
	}
	if string(diskPrompt) != "new explicit task" {
		t.Errorf("prompt.md = %q, want %q", string(diskPrompt), "new explicit task")
	}
}

func writeTestContainerScriptHarnessConfig(t *testing.T, name, taskFlag string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	taskFlagLine := ""
	if taskFlag != "" {
		taskFlagLine = "  task_flag: " + taskFlag + "\n"
	}
	cfgYAML := "harness: " + name + "\n" +
		"image: test-image:latest\n" +
		"provisioner:\n" +
		"  type: container-script\n" +
		"  interface_version: 1\n" +
		"  command: [\"python3\", \"$HOME/.scion/harness/provision.py\"]\n" +
		"  timeout: 10s\n" +
		"  lifecycle_events: [\"pre-start\"]\n" +
		"command:\n" +
		"  base: [\"" + name + "\"]\n" +
		"  resume_flag: \"--continue\"\n" +
		taskFlagLine
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "provision.py"), []byte("#!/usr/bin/env python3\nimport sys\nsys.exit(0)\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestStart_SuspendedResumeWithPriorCompleted_TaskFlagVsNonTaskFlag verifies
// that on suspended resume (Resume=true, Task=""):
//   - task_flag harnesses receive the status-aware completion notice without
//     overwriting prompt.md or setting SCION_INITIAL_TASK=1;
//   - non-task_flag harnesses remain promptless.
func TestStart_SuspendedResumeWithPriorCompleted_TaskFlagVsNonTaskFlag(t *testing.T) {
	ts := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	prior := &api.PriorStatus{
		Activity: "completed",
		Summary:  "PR merged",
		At:       ts,
	}

	t.Run("task_flag harness receives status-aware resume notice", func(t *testing.T) {
		_, projectScionDir := newResumePhaseTestFixture(t, func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return nil, nil
		})
		agentDir := filepath.Join(projectScionDir, "agents", "resume-test")
		if err := os.WriteFile(filepath.Join(agentDir, "prompt.md"), []byte("original brief"), 0o644); err != nil {
			t.Fatal(err)
		}
		hcDir := writeTestContainerScriptHarnessConfig(t, "gemini-cli", "--prompt-interactive")

		var got runtime.RunConfig
		mgr := NewManager(&runtime.MockRuntime{
			ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
			RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
				got = cfg
				return "mock-id", nil
			},
		})
		if _, err := mgr.Start(context.Background(), api.StartOptions{
			Name:              "resume-test",
			ProjectPath:       projectScionDir,
			BrokerMode:        true,
			NoAuth:            true,
			Resume:            true,
			HarnessConfig:     "gemini-cli",
			HarnessConfigPath: hcDir,
			PriorStatus:       prior,
		}); err != nil {
			t.Fatalf("Start: %v", err)
		}

		wantNotice := harness.ResumePromptForPriorStatus(prior)
		if got.Task != wantNotice {
			t.Errorf("RunConfig.Task =\n  %q\nwant:\n  %q", got.Task, wantNotice)
		}
		if hasEnvKey(got.Env, envInitialTask) {
			t.Error("SCION_INITIAL_TASK must not be set for a resume status notice")
		}
		diskPrompt, err := os.ReadFile(filepath.Join(agentDir, "prompt.md"))
		if err != nil {
			t.Fatalf("read prompt.md: %v", err)
		}
		if string(diskPrompt) != "original brief" {
			t.Errorf("prompt.md modified: got %q, want %q", string(diskPrompt), "original brief")
		}
	})

	t.Run("non-task_flag harness stays promptless", func(t *testing.T) {
		_, projectScionDir := newResumePhaseTestFixture(t, func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return nil, nil
		})
		agentDir := filepath.Join(projectScionDir, "agents", "resume-test")
		if err := os.WriteFile(filepath.Join(agentDir, "prompt.md"), []byte("original brief"), 0o644); err != nil {
			t.Fatal(err)
		}
		hcDir := writeTestContainerScriptHarnessConfig(t, "claude", "")

		var got runtime.RunConfig
		mgr := NewManager(&runtime.MockRuntime{
			ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
			RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
				got = cfg
				return "mock-id", nil
			},
		})
		if _, err := mgr.Start(context.Background(), api.StartOptions{
			Name:              "resume-test",
			ProjectPath:       projectScionDir,
			BrokerMode:        true,
			NoAuth:            true,
			Resume:            true,
			HarnessConfig:     "claude",
			HarnessConfigPath: hcDir,
			PriorStatus:       prior,
		}); err != nil {
			t.Fatalf("Start: %v", err)
		}

		if got.Task != "" {
			t.Errorf("RunConfig.Task = %q, want empty (promptless) for non-task_flag harness", got.Task)
		}
		if hasEnvKey(got.Env, envInitialTask) {
			t.Error("SCION_INITIAL_TASK must not be set for a promptless resume")
		}
	})
}

func writeTestDeclarativeGenericHarnessConfig(t *testing.T, name, taskFlag string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	taskFlagLine := ""
	if taskFlag != "" {
		taskFlagLine = "  task_flag: " + taskFlag + "\n"
	}
	cfgYAML := "harness: " + name + "\n" +
		"image: test-image:latest\n" +
		"command:\n" +
		"  base: [\"" + name + "\"]\n" +
		"  resume_flag: \"--continue\"\n" +
		taskFlagLine
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestStart_WakeForMessage_TaskFlagHarness_DefersResumePrompt (T2) verifies
// that on a message-triggered wake (WakeForMessage=true + OnDeferredResume):
//   - task_flag harnesses (both ContainerScriptHarness and
//     DeclarativeGenericHarness) start promptless with no task_flag in argv,
//     invoke OnDeferredResume once with WakeResumeInstruction and
//     ResumePromptForPriorStatus, omit SCION_INITIAL_TASK, and preserve
//     prompt.md;
//   - non-task_flag harnesses remain unchanged and do not invoke the callback.
func TestStart_WakeForMessage_TaskFlagHarness_DefersResumePrompt(t *testing.T) {
	ts := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	prior := &api.PriorStatus{
		Activity: "completed",
		Summary:  "PR merged",
		At:       ts,
	}

	tests := []struct {
		name           string
		harnessName    string
		taskFlag       string
		declarativeGen bool
		wantDeferred   bool
		wantArgv       []string
	}{
		{
			name:         "container-script task_flag harness defers prompt",
			harnessName:  "gemini-cli",
			taskFlag:     "--prompt-interactive",
			wantDeferred: true,
			wantArgv:     []string{"gemini-cli", "--continue"},
		},
		{
			name:           "declarative-generic task_flag harness defers prompt",
			harnessName:    "custom-cli",
			taskFlag:       "--prompt",
			declarativeGen: true,
			wantDeferred:   true,
			wantArgv:       []string{"custom-cli", "--continue"},
		},
		{
			name:         "non-task_flag harness stays unchanged",
			harnessName:  "claude",
			taskFlag:     "",
			wantDeferred: false,
			wantArgv:     []string{"claude", "--continue"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, projectScionDir := newResumePhaseTestFixture(t, func(context.Context, map[string]string) ([]api.AgentInfo, error) {
				return nil, nil
			})
			agentDir := filepath.Join(projectScionDir, "agents", "resume-test")
			const origPrompt = "original brief"
			if err := os.WriteFile(filepath.Join(agentDir, "prompt.md"), []byte(origPrompt), 0o644); err != nil {
				t.Fatal(err)
			}

			var hcDir string
			if tc.declarativeGen {
				hcDir = writeTestDeclarativeGenericHarnessConfig(t, tc.harnessName, tc.taskFlag)
			} else {
				hcDir = writeTestContainerScriptHarnessConfig(t, tc.harnessName, tc.taskFlag)
			}

			var got runtime.RunConfig
			mgr := NewManager(&runtime.MockRuntime{
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
				RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
					got = cfg
					return "mock-id", nil
				},
			})

			var deferredCalls int
			var gotWake, gotStandalone string
			if _, err := mgr.Start(context.Background(), api.StartOptions{
				Name:              "resume-test",
				ProjectPath:       projectScionDir,
				BrokerMode:        true,
				NoAuth:            true,
				Resume:            true,
				WakeForMessage:    true,
				HarnessConfig:     tc.harnessName,
				HarnessConfigPath: hcDir,
				PriorStatus:       prior,
				OnDeferredResume: func(wake, standalone string) {
					deferredCalls++
					gotWake = wake
					gotStandalone = standalone
				},
			}); err != nil {
				t.Fatalf("Start: %v", err)
			}

			if got.Task != "" {
				t.Errorf("RunConfig.Task = %q, want empty (promptless)", got.Task)
			}
			gotArgv := got.Harness.GetCommand(got.Task, got.Resume, nil)
			if strings.Join(gotArgv, " ") != strings.Join(tc.wantArgv, " ") {
				t.Errorf("argv = %v, want %v", gotArgv, tc.wantArgv)
			}
			if hasEnvKey(got.Env, envInitialTask) {
				t.Error("SCION_INITIAL_TASK must be absent on message wake")
			}
			diskPrompt, err := os.ReadFile(filepath.Join(agentDir, "prompt.md"))
			if err != nil {
				t.Fatalf("read prompt.md: %v", err)
			}
			if string(diskPrompt) != origPrompt {
				t.Errorf("prompt.md modified: got %q, want %q", string(diskPrompt), origPrompt)
			}

			if tc.wantDeferred {
				if deferredCalls != 1 {
					t.Fatalf("OnDeferredResume called %d times, want 1", deferredCalls)
				}
				if wantWake := harness.WakeResumeInstruction(prior); gotWake != wantWake {
					t.Errorf("wake instruction =\n  %q\nwant:\n  %q", gotWake, wantWake)
				}
				if wantStandalone := harness.ResumePromptForPriorStatus(prior); gotStandalone != wantStandalone {
					t.Errorf("standalone instruction =\n  %q\nwant:\n  %q", gotStandalone, wantStandalone)
				}
			} else if deferredCalls != 0 {
				t.Errorf("OnDeferredResume called %d times, want 0", deferredCalls)
			}
		})
	}
}

// TestStart_NonWakeResume_Unchanged (T3) verifies that non-wake resumes
// (manual resume, etc.) produce byte-for-byte identical argv to before moving
// DefaultResumePrompt selection from GetCommand into AgentManager.Start,
// including nil prior status, working prior status, and completed prior status.
func TestStart_NonWakeResume_Unchanged(t *testing.T) {
	ts := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	workingPrior := &api.PriorStatus{Activity: "working", Summary: "editing tests", At: ts}
	completedPrior := &api.PriorStatus{Activity: "completed", Summary: "PR merged", At: ts}

	tests := []struct {
		name        string
		harnessName string
		taskFlag    string
		prior       *api.PriorStatus
		wantArgv    []string
	}{
		{
			name:        "task_flag prior nil gets DefaultResumePrompt",
			harnessName: "gemini-cli",
			taskFlag:    "--prompt-interactive",
			prior:       nil,
			wantArgv:    []string{"gemini-cli", "--continue", "--prompt-interactive", harness.DefaultResumePrompt},
		},
		{
			name:        "task_flag prior working gets status-aware prompt",
			harnessName: "gemini-cli",
			taskFlag:    "--prompt-interactive",
			prior:       workingPrior,
			wantArgv:    []string{"gemini-cli", "--continue", "--prompt-interactive", harness.ResumePromptForPriorStatus(workingPrior)},
		},
		{
			name:        "task_flag prior completed gets completion-aware prompt",
			harnessName: "gemini-cli",
			taskFlag:    "--prompt-interactive",
			prior:       completedPrior,
			wantArgv:    []string{"gemini-cli", "--continue", "--prompt-interactive", harness.ResumePromptForPriorStatus(completedPrior)},
		},
		{
			name:        "non-task_flag prior nil stays promptless",
			harnessName: "claude",
			taskFlag:    "",
			prior:       nil,
			wantArgv:    []string{"claude", "--continue"},
		},
		{
			name:        "non-task_flag prior working stays promptless",
			harnessName: "claude",
			taskFlag:    "",
			prior:       workingPrior,
			wantArgv:    []string{"claude", "--continue"},
		},
		{
			name:        "non-task_flag prior completed stays promptless",
			harnessName: "claude",
			taskFlag:    "",
			prior:       completedPrior,
			wantArgv:    []string{"claude", "--continue"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, projectScionDir := newResumePhaseTestFixture(t, func(context.Context, map[string]string) ([]api.AgentInfo, error) {
				return nil, nil
			})
			hcDir := writeTestContainerScriptHarnessConfig(t, tc.harnessName, tc.taskFlag)

			var got runtime.RunConfig
			mgr := NewManager(&runtime.MockRuntime{
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
				RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
					got = cfg
					return "mock-id", nil
				},
			})

			if _, err := mgr.Start(context.Background(), api.StartOptions{
				Name:              "resume-test",
				ProjectPath:       projectScionDir,
				BrokerMode:        true,
				NoAuth:            true,
				Resume:            true,
				WakeForMessage:    false,
				HarnessConfig:     tc.harnessName,
				HarnessConfigPath: hcDir,
				PriorStatus:       tc.prior,
			}); err != nil {
				t.Fatalf("Start: %v", err)
			}

			gotArgv := got.Harness.GetCommand(got.Task, got.Resume, nil)
			if len(gotArgv) != len(tc.wantArgv) {
				t.Fatalf("argv = %#v, want %#v", gotArgv, tc.wantArgv)
			}
			for i := range gotArgv {
				if gotArgv[i] != tc.wantArgv[i] {
					t.Errorf("argv[%d] = %q, want %q", i, gotArgv[i], tc.wantArgv[i])
				}
			}
			if hasEnvKey(got.Env, envInitialTask) {
				t.Error("SCION_INITIAL_TASK must be absent on non-wake resume")
			}
		})
	}
}
