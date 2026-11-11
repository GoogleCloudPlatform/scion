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
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// newRetainedAgent creates agent "dev" in a fresh project with an
// agent-info.json holding content, and returns the project dir.
func newRetainedAgent(t *testing.T, content string) string {
	t.Helper()
	projectDir := filepath.Join(t.TempDir(), config.DotScion)
	home := config.GetAgentHomePath(projectDir, "dev")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if content != "" {
		if err := os.WriteFile(filepath.Join(home, "agent-info.json"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return projectDir
}

func TestCheckRetainedAgentFiles(t *testing.T) {
	t.Run("marked by a soft delete", func(t *testing.T) {
		projectDir := newRetainedAgent(t, `{"phase":"running"}`)
		if err := MarkAgentSoftDeleted("dev", projectDir, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := CheckRetainedAgentFiles("dev", projectDir); err != nil {
			t.Fatalf("marked files refused: %v", err)
		}
	})
	t.Run("live agent", func(t *testing.T) {
		projectDir := newRetainedAgent(t, `{"phase":"running"}`)
		if err := CheckRetainedAgentFiles("dev", projectDir); !errors.Is(err, ErrRetainedFilesNotMarked) {
			t.Fatalf("got %v, want ErrRetainedFilesNotMarked", err)
		}
	})
	t.Run("no agent-info.json", func(t *testing.T) {
		projectDir := newRetainedAgent(t, "")
		if err := CheckRetainedAgentFiles("dev", projectDir); !errors.Is(err, ErrRetainedFilesNotMarked) {
			t.Fatalf("got %v, want ErrRetainedFilesNotMarked", err)
		}
	})
	t.Run("unparseable agent-info.json", func(t *testing.T) {
		projectDir := newRetainedAgent(t, `{"phase":`)
		if err := CheckRetainedAgentFiles("dev", projectDir); !errors.Is(err, ErrRetainedFilesNotMarked) {
			t.Fatalf("got %v, want ErrRetainedFilesNotMarked", err)
		}
	})
	t.Run("agent-info.json is a symlink", func(t *testing.T) {
		projectDir := newRetainedAgent(t, "")
		target := filepath.Join(t.TempDir(), "info.json")
		if err := os.WriteFile(target, []byte(`{"phase":"deleted"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(config.GetAgentHomePath(projectDir, "dev"), "agent-info.json")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := CheckRetainedAgentFiles("dev", projectDir); !errors.Is(err, ErrRetainedFilesNotMarked) {
			t.Fatalf("got %v, want ErrRetainedFilesNotMarked", err)
		}
	})
	t.Run("agent home is a symlink", func(t *testing.T) {
		projectDir := newRetainedAgent(t, "")
		home := config.GetAgentHomePath(projectDir, "dev")
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "agent-info.json"), []byte(`{"phase":"deleted"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(home); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, home); err != nil {
			t.Fatal(err)
		}
		if err := CheckRetainedAgentFiles("dev", projectDir); !errors.Is(err, ErrRetainedFilesSymlink) {
			t.Fatalf("got %v, want ErrRetainedFilesSymlink", err)
		}
	})
	t.Run("agent dir is a symlink", func(t *testing.T) {
		projectDir := filepath.Join(t.TempDir(), config.DotScion)
		if err := os.MkdirAll(filepath.Join(projectDir, "agents"), 0o755); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		if err := os.MkdirAll(filepath.Join(outside, "home"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outside, "home", "agent-info.json"), []byte(`{"phase":"deleted"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(projectDir, "agents", "dev")); err != nil {
			t.Fatal(err)
		}
		if err := CheckRetainedAgentFiles("dev", projectDir); !errors.Is(err, ErrRetainedFilesSymlink) {
			t.Fatalf("got %v, want ErrRetainedFilesSymlink", err)
		}
	})
	t.Run("name with a path separator", func(t *testing.T) {
		projectDir := newRetainedAgent(t, `{"phase":"deleted"}`)
		if err := CheckRetainedAgentFiles("../dev", projectDir); err == nil {
			t.Fatal("a name outside the agents root was accepted")
		}
	})
}
