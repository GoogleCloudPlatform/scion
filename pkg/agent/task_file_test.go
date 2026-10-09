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
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func readTaskFile(t *testing.T, home string) (string, os.FileMode) {
	t.Helper()
	path := filepath.Join(home, ".scion", "task.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read task file: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), st.Mode().Perm()
}

func TestDeliverTaskFile_Threshold(t *testing.T) {
	for _, tc := range []struct {
		name   string
		size   int
		inline bool
	}{
		{"small", 10, true},
		{"just_under", InlineTaskMaxBytes - 1, true},
		{"at_limit", InlineTaskMaxBytes, true},
		{"just_over", InlineTaskMaxBytes + 1, false},
		{"64KiB", 64 * 1024, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			task := strings.Repeat("x", tc.size)
			got, err := deliverTaskFile(home, task)
			if err != nil {
				t.Fatalf("deliverTaskFile: %v", err)
			}
			if tc.inline && got != task {
				t.Errorf("task of %d bytes was not passed inline (got %d bytes)", tc.size, len(got))
			}
			if !tc.inline {
				if got != taskFilePointer(tc.size) {
					t.Errorf("task of %d bytes: got %q, want the pointer task", tc.size, got)
				}
				if !strings.Contains(got, "~/.scion/task.md") || !strings.Contains(got, strconv.Itoa(tc.size)) {
					t.Errorf("pointer task %q does not name the path and size", got)
				}
			}
			content, mode := readTaskFile(t, home)
			if content != task {
				t.Errorf("task file has %d bytes, want the full %d-byte task", len(content), len(task))
			}
			if mode != 0o644 {
				t.Errorf("task file mode = %v, want 0644", mode)
			}
		})
	}
}

func TestDeliverTaskFile_OverwritesEarlierFile(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".scion", "task.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("old ", 5000)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := deliverTaskFile(home, "new task"); err != nil {
		t.Fatalf("deliverTaskFile: %v", err)
	}
	content, mode := readTaskFile(t, home)
	if content != "new task" {
		t.Errorf("task file = %q, want the new task only", content)
	}
	if mode != 0o644 {
		t.Errorf("task file mode = %v, want 0644", mode)
	}
}

func TestDeliverTaskFile_ReplacesReadOnlyFile(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".scion", "task.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old task"), 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := deliverTaskFile(home, "new task"); err != nil {
		t.Fatalf("deliverTaskFile: %v", err)
	}
	content, mode := readTaskFile(t, home)
	if content != "new task" || mode != 0o644 {
		t.Errorf("task file = %q mode %v, want the new task with mode 0644", content, mode)
	}
}

func TestDeliverTaskFile_NewDirectoryIsPrivate(t *testing.T) {
	home := t.TempDir()
	if _, err := deliverTaskFile(home, "a task"); err != nil {
		t.Fatalf("deliverTaskFile: %v", err)
	}
	st, err := os.Lstat(filepath.Join(home, ".scion"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Errorf(".scion mode = %v, want 0700", st.Mode().Perm())
	}
}

// TestDeliverTaskFile_DoesNotFollowLinks checks that a symbolic link at
// task.md is replaced by the task file, and a symbolic link at .scion
// makes delivery fail, without anything being written through either
// link.
func TestDeliverTaskFile_DoesNotFollowLinks(t *testing.T) {
	t.Run("link at task.md to an existing file", func(t *testing.T) {
		home := t.TempDir()
		outside := filepath.Join(t.TempDir(), "victim")
		if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(home, ".scion", "task.md")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		if _, err := deliverTaskFile(home, "a task"); err != nil {
			t.Fatalf("deliverTaskFile: %v", err)
		}
		if got, _ := os.ReadFile(outside); string(got) != "keep" {
			t.Errorf("link target = %q, want it unchanged", got)
		}
		st, err := os.Lstat(link)
		if err != nil {
			t.Fatal(err)
		}
		if !st.Mode().IsRegular() {
			t.Errorf("task.md is %v, want a regular file in place of the link", st.Mode().Type())
		}
		if content, mode := readTaskFile(t, home); content != "a task" || mode != 0o644 {
			t.Errorf("task file = %q mode %v", content, mode)
		}
	})

	t.Run("dangling link at task.md", func(t *testing.T) {
		home := t.TempDir()
		outside := filepath.Join(t.TempDir(), "created")
		if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(home, ".scion", "task.md")); err != nil {
			t.Fatal(err)
		}
		if _, err := deliverTaskFile(home, "a task"); err != nil {
			t.Fatalf("deliverTaskFile: %v", err)
		}
		if _, err := os.Lstat(outside); !os.IsNotExist(err) {
			t.Errorf("a file was created at the link target (lstat err %v)", err)
		}
		if content, _ := readTaskFile(t, home); content != "a task" {
			t.Errorf("task file = %q", content)
		}
	})

	for _, target := range []string{"outside", "inside"} {
		t.Run("link at .scion to a directory "+target+" the home", func(t *testing.T) {
			home := t.TempDir()
			dir := t.TempDir()
			if target == "inside" {
				dir = filepath.Join(home, "other")
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(dir, filepath.Join(home, ".scion")); err != nil {
				t.Fatal(err)
			}
			if _, err := deliverTaskFile(home, "a task"); err == nil {
				t.Fatal("deliverTaskFile succeeded with .scion a symbolic link")
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("files written through the .scion link: %v", entries)
			}
		})
	}
}

func TestDeliverTaskFile_EmptyTaskWritesNothing(t *testing.T) {
	home := t.TempDir()
	got, err := deliverTaskFile(home, "")
	if err != nil || got != "" {
		t.Fatalf("deliverTaskFile(\"\") = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".scion", "task.md")); !os.IsNotExist(err) {
		t.Errorf("empty task wrote a task file (stat err %v)", err)
	}
}

// TestStart_TaskFileInAgentHome checks that Start writes the full task to
// .scion/task.md in the agent home that the runtime receives, passes a
// large task as a pointer (also when the harness takes the task as a
// flag), and replaces the file on a later start with a new task.
func TestStart_TaskFileInAgentHome(t *testing.T) {
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: test-image:latest\n"), 0644)
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)
	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

	startErr := func(t *testing.T, name, agentCfg, task string, prepareHome func(home string)) (runtime.RunConfig, error) {
		t.Helper()
		var captured runtime.RunConfig
		mockRT := &runtime.MockRuntime{
			ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{}, nil
			},
			RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
				captured = config
				return "mock-id", nil
			},
		}
		agentDir := filepath.Join(projectScionDir, "agents", name)
		_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
		_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentCfg), 0644)
		if prepareHome != nil {
			prepareHome(filepath.Join(agentDir, "home"))
		}
		_, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
			Name:        name,
			ProjectPath: projectScionDir,
			Task:        task,
			NoAuth:      true,
		})
		return captured, err
	}
	start := func(t *testing.T, name, agentCfg, task string) runtime.RunConfig {
		t.Helper()
		cfg, err := startErr(t, name, agentCfg, task, nil)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		return cfg
	}
	const plainCfg = `{"harness": "generic", "command_args": ["run"]}`
	brief := strings.Repeat("brief line\n", 64*1024/11+1)

	t.Run("large task becomes a pointer", func(t *testing.T) {
		cfg := start(t, "big-task", plainCfg, brief)
		if cfg.Task != taskFilePointer(len(brief)) {
			t.Errorf("RunConfig.Task = %.80q, want the pointer task", cfg.Task)
		}
		if content, _ := readTaskFile(t, cfg.HomeDir); content != brief {
			t.Errorf("task file in the runtime's home has %d bytes, want %d", len(content), len(brief))
		}
	})

	t.Run("small task stays inline and is written", func(t *testing.T) {
		cfg := start(t, "small-task", plainCfg, "do something")
		if cfg.Task != "do something" {
			t.Errorf("RunConfig.Task = %q, want the task inline", cfg.Task)
		}
		if content, _ := readTaskFile(t, cfg.HomeDir); content != "do something" {
			t.Errorf("task file = %q", content)
		}
	})

	t.Run("task flag carries the pointer", func(t *testing.T) {
		cfg := start(t, "flag-task", `{"harness": "generic", "task_flag": "--input", "command_args": ["run"]}`, brief)
		args := cfg.CommandArgs
		if len(args) < 2 || args[len(args)-2] != "--input" || args[len(args)-1] != taskFilePointer(len(brief)) {
			t.Errorf("CommandArgs end = %.160q, want --input and the pointer task", args)
		}
	})

	t.Run("restart replaces an earlier task file", func(t *testing.T) {
		start(t, "restart-task", plainCfg, brief)
		cfg := start(t, "restart-task", plainCfg, "second task")
		if cfg.Task != "second task" {
			t.Errorf("RunConfig.Task = %q, want the new task", cfg.Task)
		}
		if content, _ := readTaskFile(t, cfg.HomeDir); content != "second task" {
			t.Errorf("task file has %d bytes, want only the new task", len(content))
		}
	})

	// The runtime home is the staging home for every runtime, including
	// the Kubernetes plain and NFS homes the home sync fills from it.
	t.Run("link at the task file is not written through", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "victim")
		if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := startErr(t, "link-task", plainCfg, brief, func(home string) {
			_ = os.MkdirAll(filepath.Join(home, ".scion"), 0o755)
			if err := os.Symlink(outside, filepath.Join(home, ".scion", "task.md")); err != nil {
				t.Fatal(err)
			}
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if got, _ := os.ReadFile(outside); string(got) != "keep" {
			t.Errorf("link target = %q, want it unchanged", got)
		}
		if content, _ := readTaskFile(t, cfg.HomeDir); content != brief {
			t.Errorf("task file has %d bytes, want %d", len(content), len(brief))
		}
	})
}
