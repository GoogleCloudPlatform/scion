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
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

const wantEmptyGenericMsg = "generic harness needs a task, harness args or command_args"

func setupGenericHarnessProject(t *testing.T, extraSettings string) string {
	t.Helper()
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"image":"test-image:latest"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	settings := "schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n" + extraSettings
	if err := os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	if err := os.MkdirAll(filepath.Join(projectScionDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	return projectScionDir
}

func TestStart_GenericHarnessEmptyCommandRejected(t *testing.T) {
	t.Run("built-in generic with no task or args", func(t *testing.T) {
		projectScionDir := setupGenericHarnessProject(t, "")
		runs := 0
		mgr := NewManager(&runtime.MockRuntime{
			ListFunc:        func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
			ImageExistsFunc: func(context.Context, string) (bool, error) { return true, nil },
			RunFunc: func(context.Context, runtime.RunConfig) (string, error) {
				runs++
				return "mock-id", nil
			},
		})

		opts := api.StartOptions{
			Name:          "gen-empty",
			ProjectPath:   projectScionDir,
			HarnessConfig: "generic",
			BrokerMode:    true,
		}
		if err := mgr.Preflight(context.Background(), opts); err == nil || !strings.Contains(err.Error(), wantEmptyGenericMsg) {
			t.Fatalf("Preflight error = %v, want substring %q", err, wantEmptyGenericMsg)
		}
		if _, err := mgr.Start(context.Background(), opts); err == nil || !strings.Contains(err.Error(), wantEmptyGenericMsg) {
			t.Fatalf("Start error = %v, want substring %q", err, wantEmptyGenericMsg)
		}
		if runs != 0 {
			t.Fatalf("Runtime.Run called %d times, want 0", runs)
		}
	})

	t.Run("built-in generic with SCION_MODEL only still fails", func(t *testing.T) {
		projectScionDir := setupGenericHarnessProject(t, "")
		runs := 0
		mgr := NewManager(&runtime.MockRuntime{
			ListFunc:        func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
			ImageExistsFunc: func(context.Context, string) (bool, error) { return true, nil },
			RunFunc: func(context.Context, runtime.RunConfig) (string, error) {
				runs++
				return "mock-id", nil
			},
		})

		opts := api.StartOptions{
			Name:          "gen-model-only",
			ProjectPath:   projectScionDir,
			HarnessConfig: "generic",
			BrokerMode:    true,
			Env:           map[string]string{"SCION_MODEL": "some-model"},
		}
		if err := mgr.Preflight(context.Background(), opts); err == nil || !strings.Contains(err.Error(), wantEmptyGenericMsg) {
			t.Fatalf("Preflight error = %v, want substring %q", err, wantEmptyGenericMsg)
		}
		if _, err := mgr.Start(context.Background(), opts); err == nil || !strings.Contains(err.Error(), wantEmptyGenericMsg) {
			t.Fatalf("Start error = %v, want substring %q", err, wantEmptyGenericMsg)
		}
		if runs != 0 {
			t.Fatalf("Runtime.Run called %d times, want 0", runs)
		}
	})
}

func TestStart_GenericHarnessProvisionThenStart(t *testing.T) {
	projectScionDir := setupGenericHarnessProject(t, "")
	runs := 0
	var gotCmd []string
	mgr := NewManager(&runtime.MockRuntime{
		ListFunc:        func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
		ImageExistsFunc: func(context.Context, string) (bool, error) { return true, nil },
		RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
			runs++
			gotCmd = cfg.Harness.GetCommand(cfg.Task, cfg.Resume, cfg.CommandArgs)
			return "mock-id", nil
		},
	})

	baseOpts := api.StartOptions{
		Name:          "gen-provision",
		ProjectPath:   projectScionDir,
		HarnessConfig: "generic",
		BrokerMode:    true,
	}
	if _, err := mgr.Provision(context.Background(), baseOpts); err != nil {
		t.Fatalf("Provision without task should succeed: %v", err)
	}

	if _, err := mgr.Start(context.Background(), baseOpts); err == nil || !strings.Contains(err.Error(), wantEmptyGenericMsg) {
		t.Fatalf("Start without task error = %v, want substring %q", err, wantEmptyGenericMsg)
	}
	if runs != 0 {
		t.Fatalf("Runtime.Run called %d times before task provided, want 0", runs)
	}

	withTask := baseOpts
	withTask.Task = "echo hello"
	if _, err := mgr.Start(context.Background(), withTask); err != nil {
		t.Fatalf("Start with task failed: %v", err)
	}
	if runs != 1 {
		t.Fatalf("Runtime.Run called %d times, want 1", runs)
	}
	if !reflect.DeepEqual(gotCmd, []string{"echo hello"}) {
		t.Errorf("command = %v, want [echo hello]", gotCmd)
	}
}

func TestStart_GenericHarnessValidStartsUnchanged(t *testing.T) {
	t.Run("with task", func(t *testing.T) {
		projectScionDir := setupGenericHarnessProject(t, "")
		var got []string
		mgr := NewManager(&runtime.MockRuntime{
			ListFunc:        func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
			ImageExistsFunc: func(context.Context, string) (bool, error) { return true, nil },
			RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
				got = cfg.Harness.GetCommand(cfg.Task, cfg.Resume, cfg.CommandArgs)
				return "mock-id", nil
			},
		})
		opts := api.StartOptions{
			Name:          "gen-task",
			ProjectPath:   projectScionDir,
			HarnessConfig: "generic",
			Task:          "echo hello",
			BrokerMode:    true,
		}
		if err := mgr.Preflight(context.Background(), opts); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		if _, err := mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if !reflect.DeepEqual(got, []string{"echo hello"}) {
			t.Errorf("command = %v, want [echo hello]", got)
		}
	})

	t.Run("with inline command_args", func(t *testing.T) {
		projectScionDir := setupGenericHarnessProject(t, "")
		var got []string
		mgr := NewManager(&runtime.MockRuntime{
			ListFunc:        func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
			ImageExistsFunc: func(context.Context, string) (bool, error) { return true, nil },
			RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
				got = cfg.Harness.GetCommand(cfg.Task, cfg.Resume, cfg.CommandArgs)
				return "mock-id", nil
			},
		})
		opts := api.StartOptions{
			Name:          "gen-cmdargs",
			ProjectPath:   projectScionDir,
			HarnessConfig: "generic",
			InlineConfig:  &api.ScionConfig{CommandArgs: []string{"python3", "agent.py"}},
			BrokerMode:    true,
		}
		if err := mgr.Preflight(context.Background(), opts); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		if _, err := mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if !reflect.DeepEqual(got, []string{"python3", "agent.py"}) {
			t.Errorf("command = %v, want [python3 agent.py]", got)
		}
	})

	t.Run("with settings harness_configs.generic.args", func(t *testing.T) {
		extraSettings := "harness_configs:\n  generic:\n    harness: generic\n    args: [\"bash\", \"-lc\", \"sleep 30\"]\n"
		projectScionDir := setupGenericHarnessProject(t, extraSettings)
		var got []string
		mgr := NewManager(&runtime.MockRuntime{
			ListFunc:        func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
			ImageExistsFunc: func(context.Context, string) (bool, error) { return true, nil },
			RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
				got = cfg.Harness.GetCommand(cfg.Task, cfg.Resume, cfg.CommandArgs)
				return "mock-id", nil
			},
		})
		opts := api.StartOptions{
			Name:          "gen-settings-args",
			ProjectPath:   projectScionDir,
			HarnessConfig: "generic",
			BrokerMode:    true,
		}
		if err := mgr.Preflight(context.Background(), opts); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		if _, err := mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if !reflect.DeepEqual(got, []string{"bash", "-lc", "sleep 30"}) {
			t.Errorf("command = %v, want [bash -lc 'sleep 30']", got)
		}
	})

	t.Run("with custom on-disk generic harness-config args", func(t *testing.T) {
		projectScionDir := setupGenericHarnessProject(t, "")
		hcDir := filepath.Join(os.Getenv("HOME"), ".scion", "harness-configs", "generic")
		if err := os.MkdirAll(hcDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nimage: test-image:latest\nargs: [\"node\", \"worker.js\"]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var got []string
		mgr := NewManager(&runtime.MockRuntime{
			ListFunc:        func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
			ImageExistsFunc: func(context.Context, string) (bool, error) { return true, nil },
			RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
				got = cfg.Harness.GetCommand(cfg.Task, cfg.Resume, cfg.CommandArgs)
				return "mock-id", nil
			},
		})
		opts := api.StartOptions{
			Name:          "gen-ondisk-args",
			ProjectPath:   projectScionDir,
			HarnessConfig: "generic",
			BrokerMode:    true,
		}
		if err := mgr.Preflight(context.Background(), opts); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		if _, err := mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if !reflect.DeepEqual(got, []string{"node", "worker.js"}) {
			t.Errorf("command = %v, want [node worker.js]", got)
		}
	})

	t.Run("custom on-disk generic harness-config without args is not rejected early", func(t *testing.T) {
		projectScionDir := setupGenericHarnessProject(t, "")
		hcDir := filepath.Join(os.Getenv("HOME"), ".scion", "harness-configs", "generic")
		if err := os.MkdirAll(hcDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nimage: test-image:latest\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runs := 0
		mgr := NewManager(&runtime.MockRuntime{
			ListFunc:        func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
			ImageExistsFunc: func(context.Context, string) (bool, error) { return true, nil },
			RunFunc: func(context.Context, runtime.RunConfig) (string, error) {
				runs++
				return "mock-id", nil
			},
		})
		opts := api.StartOptions{
			Name:          "gen-custom-empty",
			ProjectPath:   projectScionDir,
			HarnessConfig: "generic",
			BrokerMode:    true,
		}
		if err := mgr.Preflight(context.Background(), opts); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		if _, err := mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if runs != 1 {
			t.Fatalf("Runtime.Run called %d times, want 1", runs)
		}
	})

	t.Run("non-generic harness with no task is unaffected", func(t *testing.T) {
		projectScionDir := setupGenericHarnessProject(t, "")
		hcDir := filepath.Join(os.Getenv("HOME"), ".scion", "harness-configs", "claude")
		if err := os.MkdirAll(hcDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: claude\nuser: scion\nimage: test-image:latest\ncommand:\n  base: [\"claude\"]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var got []string
		mgr := NewManager(&runtime.MockRuntime{
			ListFunc:        func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
			ImageExistsFunc: func(context.Context, string) (bool, error) { return true, nil },
			RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
				got = cfg.Harness.GetCommand(cfg.Task, cfg.Resume, cfg.CommandArgs)
				return "mock-id", nil
			},
		})
		opts := api.StartOptions{
			Name:          "claude-notask",
			ProjectPath:   projectScionDir,
			HarnessConfig: "claude",
			BrokerMode:    true,
			NoAuth:        true,
		}
		if err := mgr.Preflight(context.Background(), opts); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		if _, err := mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if !reflect.DeepEqual(got, []string{"claude"}) {
			t.Errorf("command = %v, want [claude]", got)
		}
	})

	t.Run("non-generic harness command is unchanged when settings args are present", func(t *testing.T) {
		extraSettings := "harness_configs:\n  claude:\n    harness: claude\n    args: [\"--unexpected-from-settings\"]\n"
		projectScionDir := setupGenericHarnessProject(t, extraSettings)
		hcDir := filepath.Join(os.Getenv("HOME"), ".scion", "harness-configs", "claude")
		if err := os.MkdirAll(hcDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: claude\nuser: scion\nimage: test-image:latest\ncommand:\n  base: [\"claude\"]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var got []string
		mgr := NewManager(&runtime.MockRuntime{
			ListFunc:        func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
			ImageExistsFunc: func(context.Context, string) (bool, error) { return true, nil },
			RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
				got = cfg.Harness.GetCommand(cfg.Task, cfg.Resume, cfg.CommandArgs)
				return "mock-id", nil
			},
		})
		opts := api.StartOptions{
			Name:          "claude-settings-args",
			ProjectPath:   projectScionDir,
			HarnessConfig: "claude",
			BrokerMode:    true,
			NoAuth:        true,
		}
		if err := mgr.Preflight(context.Background(), opts); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		if _, err := mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if !reflect.DeepEqual(got, []string{"claude"}) {
			t.Errorf("command = %v, want [claude] (settings args must not alter non-generic harness)", got)
		}
	})
}

// TestStart_GenericHarnessSkipsModelFlag verifies that generic and
// shell-based declarative-generic harnesses do not have "--model <name>"
// prepended to CommandArgs when an effective model is present, while still
// receiving SCION_MODEL in their environment (ptone/scion#6386).
func TestStart_GenericHarnessSkipsModelFlag(t *testing.T) {
	hasEnv := func(env []string, want string) bool {
		for _, kv := range env {
			if kv == want {
				return true
			}
		}
		return false
	}

	t.Run("built-in generic exec form with SCION_MODEL", func(t *testing.T) {
		projectScionDir := setupGenericHarnessProject(t, "")
		var gotCfg runtime.RunConfig
		var gotCmd []string
		mgr := NewManager(&runtime.MockRuntime{
			ListFunc:        func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
			ImageExistsFunc: func(context.Context, string) (bool, error) { return true, nil },
			RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
				gotCfg = cfg
				gotCmd = cfg.Harness.GetCommand(cfg.Task, cfg.Resume, cfg.CommandArgs)
				return "mock-id", nil
			},
		})
		opts := api.StartOptions{
			Name:          "gen-model-exec",
			ProjectPath:   projectScionDir,
			HarnessConfig: "generic",
			Task:          "/usr/local/bin/run.sh",
			Env:           map[string]string{"SCION_MODEL": "some-model"},
			BrokerMode:    true,
		}
		if _, err := mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if !reflect.DeepEqual(gotCmd, []string{"/usr/local/bin/run.sh"}) {
			t.Errorf("GetCommand() = %v, want [/usr/local/bin/run.sh] (must not prepend --model)", gotCmd)
		}
		if !hasEnv(gotCfg.Env, "SCION_MODEL=some-model") {
			t.Errorf("expected SCION_MODEL=some-model in env, got %v", gotCfg.Env)
		}
	})

	t.Run("built-in generic with command_args sh -c and SCION_MODEL", func(t *testing.T) {
		projectScionDir := setupGenericHarnessProject(t, "")
		var gotCmd []string
		mgr := NewManager(&runtime.MockRuntime{
			ListFunc:        func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
			ImageExistsFunc: func(context.Context, string) (bool, error) { return true, nil },
			RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
				gotCmd = cfg.Harness.GetCommand(cfg.Task, cfg.Resume, cfg.CommandArgs)
				return "mock-id", nil
			},
		})
		opts := api.StartOptions{
			Name:          "gen-model-sh",
			ProjectPath:   projectScionDir,
			HarnessConfig: "generic",
			InlineConfig:  &api.ScionConfig{CommandArgs: []string{"sh", "-c"}},
			Task:          "npm test && echo ok",
			Env:           map[string]string{"SCION_MODEL": "some-model"},
			BrokerMode:    true,
		}
		if _, err := mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if !reflect.DeepEqual(gotCmd, []string{"sh", "-c", "npm test && echo ok"}) {
			t.Errorf("GetCommand() = %v, want [sh -c 'npm test && echo ok']", gotCmd)
		}
	})

	t.Run("declarative generic with command.base sh -c and SCION_MODEL", func(t *testing.T) {
		projectScionDir := setupGenericHarnessProject(t, "")
		hcDir := filepath.Join(os.Getenv("HOME"), ".scion", "harness-configs", "generic-sh")
		if err := os.MkdirAll(hcDir, 0o755); err != nil {
			t.Fatal(err)
		}
		const cfgYAML = "harness: generic\nuser: scion\nimage: test-image:latest\ncommand:\n  base: [\"sh\", \"-c\"]\n"
		if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(cfgYAML), 0o644); err != nil {
			t.Fatal(err)
		}
		var gotCmd []string
		mgr := NewManager(&runtime.MockRuntime{
			ListFunc:        func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
			ImageExistsFunc: func(context.Context, string) (bool, error) { return true, nil },
			RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
				gotCmd = cfg.Harness.GetCommand(cfg.Task, cfg.Resume, cfg.CommandArgs)
				return "mock-id", nil
			},
		})
		opts := api.StartOptions{
			Name:          "gen-decl-sh-model",
			ProjectPath:   projectScionDir,
			HarnessConfig: "generic-sh",
			Task:          "npm test && echo ok",
			Env:           map[string]string{"SCION_MODEL": "some-model"},
			BrokerMode:    true,
		}
		if _, err := mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if !reflect.DeepEqual(gotCmd, []string{"sh", "-c", "npm test && echo ok"}) {
			t.Errorf("GetCommand() = %v, want [sh -c 'npm test && echo ok']", gotCmd)
		}
	})
}

// TestStart_GenericHarnessShellFormResumeRefused verifies that resuming a
// shell-form (sh -c) generic harness without a task is refused up front before
// starting a container, while exec-form resume and explicit-task resume remain
// supported (ptone/scion#9878).
func TestStart_GenericHarnessShellFormResumeRefused(t *testing.T) {
	const wantResumeMsg = "generic harness with shell-form command (sh -c) does not support resume without a task; start the agent fresh or pass a task explicitly"
	const wantStartMsg = "generic harness with shell-form command (sh -c) requires a task or command string"

	t.Run("built-in generic with command_args sh -c refuses resume without task", func(t *testing.T) {
		projectScionDir := setupGenericHarnessProject(t, "")
		runs := 0
		mgr := NewManager(&runtime.MockRuntime{
			ListFunc:        func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
			ImageExistsFunc: func(context.Context, string) (bool, error) { return true, nil },
			RunFunc: func(context.Context, runtime.RunConfig) (string, error) {
				runs++
				return "mock-id", nil
			},
		})
		startOpts := api.StartOptions{
			Name:          "gen-sh-resume",
			ProjectPath:   projectScionDir,
			HarnessConfig: "generic",
			InlineConfig:  &api.ScionConfig{CommandArgs: []string{"sh", "-c"}},
			Task:          "echo initial",
			BrokerMode:    true,
		}
		if _, err := mgr.Start(context.Background(), startOpts); err != nil {
			t.Fatalf("initial Start failed: %v", err)
		}
		if runs != 1 {
			t.Fatalf("initial Start called Runtime.Run %d times, want 1", runs)
		}

		resumeOpts := api.StartOptions{
			Name:          "gen-sh-resume",
			ProjectPath:   projectScionDir,
			HarnessConfig: "generic",
			Resume:        true,
			BrokerMode:    true,
		}
		if _, err := mgr.Start(context.Background(), resumeOpts); err == nil || !strings.Contains(err.Error(), wantResumeMsg) {
			t.Fatalf("Start(resume) error = %v, want substring %q", err, wantResumeMsg)
		}
		if runs != 1 {
			t.Fatalf("Runtime.Run called %d times after refused resume, want 1", runs)
		}
	})

	t.Run("declarative generic with command.base sh -c refuses resume without task", func(t *testing.T) {
		projectScionDir := setupGenericHarnessProject(t, "")
		hcDir := filepath.Join(os.Getenv("HOME"), ".scion", "harness-configs", "generic-sh")
		if err := os.MkdirAll(hcDir, 0o755); err != nil {
			t.Fatal(err)
		}
		const cfgYAML = "harness: generic\nuser: scion\nimage: test-image:latest\ncommand:\n  base: [\"sh\", \"-c\"]\n"
		if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(cfgYAML), 0o644); err != nil {
			t.Fatal(err)
		}
		runs := 0
		mgr := NewManager(&runtime.MockRuntime{
			ListFunc:        func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
			ImageExistsFunc: func(context.Context, string) (bool, error) { return true, nil },
			RunFunc: func(context.Context, runtime.RunConfig) (string, error) {
				runs++
				return "mock-id", nil
			},
		})
		startOpts := api.StartOptions{
			Name:          "gen-decl-sh-resume",
			ProjectPath:   projectScionDir,
			HarnessConfig: "generic-sh",
			Task:          "echo initial",
			BrokerMode:    true,
		}
		if _, err := mgr.Start(context.Background(), startOpts); err != nil {
			t.Fatalf("initial Start failed: %v", err)
		}
		if runs != 1 {
			t.Fatalf("initial Start called Runtime.Run %d times, want 1", runs)
		}

		resumeOpts := api.StartOptions{
			Name:          "gen-decl-sh-resume",
			ProjectPath:   projectScionDir,
			HarnessConfig: "generic-sh",
			Resume:        true,
			BrokerMode:    true,
		}
		if _, err := mgr.Start(context.Background(), resumeOpts); err == nil || !strings.Contains(err.Error(), wantResumeMsg) {
			t.Fatalf("Start(resume) error = %v, want substring %q", err, wantResumeMsg)
		}
		if runs != 1 {
			t.Fatalf("Runtime.Run called %d times after refused resume, want 1", runs)
		}
	})

	t.Run("declarative generic with command.base sh -c refuses fresh start without task", func(t *testing.T) {
		projectScionDir := setupGenericHarnessProject(t, "")
		hcDir := filepath.Join(os.Getenv("HOME"), ".scion", "harness-configs", "generic-sh")
		if err := os.MkdirAll(hcDir, 0o755); err != nil {
			t.Fatal(err)
		}
		const cfgYAML = "harness: generic\nuser: scion\nimage: test-image:latest\ncommand:\n  base: [\"sh\", \"-c\"]\n"
		if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(cfgYAML), 0o644); err != nil {
			t.Fatal(err)
		}
		runs := 0
		mgr := NewManager(&runtime.MockRuntime{
			ListFunc:        func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
			ImageExistsFunc: func(context.Context, string) (bool, error) { return true, nil },
			RunFunc: func(context.Context, runtime.RunConfig) (string, error) {
				runs++
				return "mock-id", nil
			},
		})
		opts := api.StartOptions{
			Name:          "gen-decl-sh-notask",
			ProjectPath:   projectScionDir,
			HarnessConfig: "generic-sh",
			BrokerMode:    true,
		}
		if err := mgr.Preflight(context.Background(), opts); err == nil || !strings.Contains(err.Error(), wantStartMsg) {
			t.Fatalf("Preflight(start) error = %v, want substring %q", err, wantStartMsg)
		}
		if _, err := mgr.Start(context.Background(), opts); err == nil || !strings.Contains(err.Error(), wantStartMsg) {
			t.Fatalf("Start(start) error = %v, want substring %q", err, wantStartMsg)
		}
		if runs != 0 {
			t.Fatalf("Runtime.Run called %d times, want 0", runs)
		}
	})

	t.Run("shell-form resume with explicit task succeeds", func(t *testing.T) {
		projectScionDir := setupGenericHarnessProject(t, "")
		var gotCmd []string
		mgr := NewManager(&runtime.MockRuntime{
			ListFunc:        func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
			ImageExistsFunc: func(context.Context, string) (bool, error) { return true, nil },
			RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
				gotCmd = cfg.Harness.GetCommand(cfg.Task, cfg.Resume, cfg.CommandArgs)
				return "mock-id", nil
			},
		})
		startOpts := api.StartOptions{
			Name:          "gen-sh-resume-task",
			ProjectPath:   projectScionDir,
			HarnessConfig: "generic",
			InlineConfig:  &api.ScionConfig{CommandArgs: []string{"sh", "-c"}},
			Task:          "echo initial",
			BrokerMode:    true,
		}
		if _, err := mgr.Start(context.Background(), startOpts); err != nil {
			t.Fatalf("initial Start failed: %v", err)
		}

		resumeOpts := api.StartOptions{
			Name:          "gen-sh-resume-task",
			ProjectPath:   projectScionDir,
			HarnessConfig: "generic",
			Task:          "echo resumed",
			Resume:        true,
			BrokerMode:    true,
		}
		if _, err := mgr.Start(context.Background(), resumeOpts); err != nil {
			t.Fatalf("Start(resume with task) failed: %v", err)
		}
		if !reflect.DeepEqual(gotCmd, []string{"sh", "-c", "echo resumed"}) {
			t.Errorf("GetCommand() = %v, want [sh -c 'echo resumed']", gotCmd)
		}
	})

	t.Run("exec-form generic resume without task is unchanged", func(t *testing.T) {
		projectScionDir := setupGenericHarnessProject(t, "")
		var gotCmd []string
		mgr := NewManager(&runtime.MockRuntime{
			ListFunc:        func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
			ImageExistsFunc: func(context.Context, string) (bool, error) { return true, nil },
			RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
				gotCmd = cfg.Harness.GetCommand(cfg.Task, cfg.Resume, cfg.CommandArgs)
				return "mock-id", nil
			},
		})
		startOpts := api.StartOptions{
			Name:          "gen-exec-resume",
			ProjectPath:   projectScionDir,
			HarnessConfig: "generic",
			InlineConfig:  &api.ScionConfig{CommandArgs: []string{"python3", "agent.py"}},
			BrokerMode:    true,
		}
		if _, err := mgr.Start(context.Background(), startOpts); err != nil {
			t.Fatalf("initial Start failed: %v", err)
		}

		resumeOpts := api.StartOptions{
			Name:          "gen-exec-resume",
			ProjectPath:   projectScionDir,
			HarnessConfig: "generic",
			Resume:        true,
			BrokerMode:    true,
		}
		if _, err := mgr.Start(context.Background(), resumeOpts); err != nil {
			t.Fatalf("Start(exec resume) = %v, want nil", err)
		}
		if !reflect.DeepEqual(gotCmd, []string{"python3", "agent.py"}) {
			t.Errorf("GetCommand() = %v, want [python3 agent.py]", gotCmd)
		}
	})
}
