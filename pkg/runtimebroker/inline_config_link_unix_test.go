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

//go:build unix

package runtimebroker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// applyInlineConfigUpdate neither reads nor writes through a scion-agent.json
// that is a symbolic link (to an existing file or dangling): the outside file
// is byte-identical afterwards (or, dangling, still absent). Control: a plain
// file is updated.
func TestApplyInlineConfigUpdateNeverWritesThroughLink(t *testing.T) {
	const outsideContent = `{"harness":"outside-marker"}`
	inline := &api.ScionConfig{MaxTurns: api.IntPtr(7)}
	for _, kind := range []string{"link", "dangling"} {
		t.Run(kind, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContext(t, cfg)
			projectScionDir := filepath.Join(t.TempDir(), "project", ".scion")
			agentDir := config.GetAgentDir(projectScionDir, "linked-agent", false)
			if err := os.MkdirAll(agentDir, 0o755); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "outside.json")
			if kind == "link" {
				if err := os.WriteFile(outside, []byte(outsideContent), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(outside, filepath.Join(agentDir, "scion-agent.json")); err != nil {
				t.Fatal(err)
			}

			srv.applyInlineConfigUpdate(context.Background(), "linked-agent", projectScionDir, inline, false)

			data, err := os.ReadFile(outside)
			if kind == "dangling" {
				if err == nil {
					t.Fatalf("the dangling link's target was created: %q", data)
				}
			} else if err != nil || !bytes.Equal(data, []byte(outsideContent)) {
				t.Fatalf("the outside file changed: %q, %v", data, err)
			}
			if fi, err := os.Lstat(filepath.Join(agentDir, "scion-agent.json")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("the planted link was replaced or removed: %v", err)
			}
		})
	}

	// Control: a plain file is updated.
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)
	projectScionDir := filepath.Join(t.TempDir(), "project", ".scion")
	agentDir := config.GetAgentDir(projectScionDir, "plain-agent", false)
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{"harness":"claude"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	srv.applyInlineConfigUpdate(context.Background(), "plain-agent", projectScionDir, inline, false)
	got, err := (&config.Template{Path: agentDir}).LoadConfig()
	if err != nil || !api.IntPtrEqual(got.MaxTurns, api.IntPtr(7)) {
		t.Fatalf("control: LoadConfig = %+v, %v; want MaxTurns 7", got, err)
	}
}
