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

package shipper

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/pathcanon"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

func TestOpenArtifactNoFollowRefusesAllAncestorAndLeafSwaps(t *testing.T) {
	tests := []struct {
		name string
		swap func(t *testing.T, home, outside string)
	}{
		{
			name: "swap .claude ancestor for symlink",
			swap: func(t *testing.T, home, outside string) {
				target := filepath.Join(outside, "projects", "-workspace", "ok.jsonl")
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					t.Fatalf("mkdir outside target: %v", err)
				}
				if err := os.WriteFile(target, []byte("{\"secret\":true}\n"), 0o600); err != nil {
					t.Fatalf("write outside target: %v", err)
				}
				claudeDir := filepath.Join(home, ".claude")
				if err := os.RemoveAll(claudeDir); err != nil {
					t.Fatalf("remove .claude: %v", err)
				}
				if err := os.Symlink(outside, claudeDir); err != nil {
					t.Fatalf("symlink .claude: %v", err)
				}
			},
		},
		{
			name: "swap projects ancestor for symlink",
			swap: func(t *testing.T, home, outside string) {
				target := filepath.Join(outside, "-workspace", "ok.jsonl")
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					t.Fatalf("mkdir outside target: %v", err)
				}
				if err := os.WriteFile(target, []byte("{\"secret\":true}\n"), 0o600); err != nil {
					t.Fatalf("write outside target: %v", err)
				}
				projDir := filepath.Join(home, ".claude", "projects")
				if err := os.RemoveAll(projDir); err != nil {
					t.Fatalf("remove projects: %v", err)
				}
				if err := os.Symlink(outside, projDir); err != nil {
					t.Fatalf("symlink projects: %v", err)
				}
			},
		},
		{
			name: "swap intermediate directory for symlink",
			swap: func(t *testing.T, home, outside string) {
				target := filepath.Join(outside, "ok.jsonl")
				if err := os.WriteFile(target, []byte("{\"secret\":true}\n"), 0o600); err != nil {
					t.Fatalf("write outside target: %v", err)
				}
				workDir := filepath.Join(home, ".claude", "projects", "-workspace")
				if err := os.RemoveAll(workDir); err != nil {
					t.Fatalf("remove workDir: %v", err)
				}
				if err := os.Symlink(outside, workDir); err != nil {
					t.Fatalf("symlink workDir: %v", err)
				}
			},
		},
		{
			name: "swap leaf file for symlink",
			swap: func(t *testing.T, home, outside string) {
				secret := filepath.Join(outside, "secret.jsonl")
				if err := os.WriteFile(secret, []byte("{\"secret\":true}\n"), 0o600); err != nil {
					t.Fatalf("write secret: %v", err)
				}
				okFile := filepath.Join(home, ".claude", "projects", "-workspace", "ok.jsonl")
				if err := os.Remove(okFile); err != nil {
					t.Fatalf("remove okFile: %v", err)
				}
				if err := os.Symlink(secret, okFile); err != nil {
					t.Fatalf("symlink leaf: %v", err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			outside := t.TempDir()

			okFile := filepath.Join(home, ".claude", "projects", "-workspace", "ok.jsonl")
			if err := os.MkdirAll(filepath.Dir(okFile), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(okFile, []byte("{\"ok\":1}\n"), 0o644); err != nil {
				t.Fatalf("write okFile: %v", err)
			}

			// First verify Discover succeeds and openArtifactNoFollow works before swap.
			disc, err := Discover(home, ClaudeHarnessSpec())
			if err != nil || len(disc.Artifacts) != 1 {
				t.Fatalf("Discover: err=%v artifacts=%+v", err, disc)
			}
			resolvedHome, err := pathcanon.Resolve(home)
			if err != nil {
				t.Fatalf("Resolve home: %v", err)
			}
			rel := disc.Artifacts[0].NativeRelativePath
			f, fi, err := openArtifactNoFollow(resolvedHome, rel)
			if err != nil {
				t.Fatalf("openArtifactNoFollow before swap: %v", err)
			}
			_ = f.Close()
			if !fi.Mode().IsRegular() {
				t.Fatalf("expected regular file")
			}

			// Now perform the post-discovery swap and verify openArtifactNoFollow refuses it.
			tc.swap(t, home, outside)
			if f, _, err := openArtifactNoFollow(resolvedHome, rel); err == nil {
				_ = f.Close()
				t.Fatalf("expected swap to be refused")
			} else if !errors.Is(err, privatefs.ErrReparsePoint) {
				t.Fatalf("expected ErrReparsePoint, got %v", err)
			}
		})
	}
}
