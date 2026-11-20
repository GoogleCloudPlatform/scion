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

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agenthome"
)

// homeLayerFixture is a layer directory root/layer with a home/ below it,
// an agent home, and a secret file outside both.
type homeLayerFixture struct {
	base, root, layerHome, agentHome, secret string
}

func newHomeLayerFixture(t *testing.T) *homeLayerFixture {
	t.Helper()
	base := t.TempDir()
	f := &homeLayerFixture{
		base:      base,
		root:      filepath.Join(base, "root"),
		layerHome: filepath.Join(base, "root", "layer", "home"),
		agentHome: filepath.Join(base, "agent-home"),
		secret:    filepath.Join(base, "outside", "secret"),
	}
	writeHomeLayerFile(t, f.secret, "host-secret")
	if err := os.MkdirAll(f.agentHome, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *homeLayerFixture) overlay(t *testing.T) (bool, error) {
	t.Helper()
	home, err := agenthome.Open(f.agentHome)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = home.Close() }()
	return overlayHomeLayer(home, func() (*agenthome.Home, error) {
		return agenthome.OpenBelow(f.root, "layer")
	})
}

func writeHomeLayerFile(t *testing.T, p, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// assertNoSecret fails when the outside secret's content reached the agent
// home under any name.
func assertNoSecret(t *testing.T, agentHome string) {
	t.Helper()
	_ = filepath.Walk(agentHome, func(p string, info os.FileInfo, err error) error {
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		if data, _ := os.ReadFile(p); strings.Contains(string(data), "host-secret") {
			t.Errorf("outside file content copied into the agent home at %s", p)
		}
		return nil
	})
}

// TestOverlayHomeLayer_CopiesAndMerges: a plain home/ is copied over the
// agent home, keeping entries the layer does not hold and replacing those
// it does.
func TestOverlayHomeLayer_CopiesAndMerges(t *testing.T) {
	f := newHomeLayerFixture(t)
	writeHomeLayerFile(t, filepath.Join(f.layerHome, ".bashrc"), "layer")
	writeHomeLayerFile(t, filepath.Join(f.layerHome, ".config", "app", "x.yaml"), "x")
	writeHomeLayerFile(t, filepath.Join(f.agentHome, ".bashrc"), "old")
	writeHomeLayerFile(t, filepath.Join(f.agentHome, "keep.txt"), "keep")

	copied, err := f.overlay(t)
	if err != nil || !copied {
		t.Fatalf("overlayHomeLayer() = %v, %v; want true, nil", copied, err)
	}
	for rel, want := range map[string]string{".bashrc": "layer", ".config/app/x.yaml": "x", "keep.txt": "keep"} {
		got, err := os.ReadFile(filepath.Join(f.agentHome, rel))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", rel, got, err, want)
		}
	}
}

// TestOverlayHomeLayer_Absent: a layer without home/, or without a
// directory at all, copies nothing and is not an error.
func TestOverlayHomeLayer_Absent(t *testing.T) {
	f := newHomeLayerFixture(t)
	if copied, err := f.overlay(t); err != nil || copied {
		t.Fatalf("missing layer: overlayHomeLayer() = %v, %v; want false, nil", copied, err)
	}
	if err := os.MkdirAll(filepath.Dir(f.layerHome), 0o755); err != nil {
		t.Fatal(err)
	}
	if copied, err := f.overlay(t); err != nil || copied {
		t.Fatalf("missing home/: overlayHomeLayer() = %v, %v; want false, nil", copied, err)
	}
}

// TestOverlayHomeLayer_RefusesLinks: a symbolic link anywhere in the layer
// fails the copy before anything is written, with an error naming the
// entry relative to the layer directory, and the link target is never
// read.
func TestOverlayHomeLayer_RefusesLinks(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, f *homeLayerFixture)
		wantMsg string
	}{
		{
			name: "file link in home",
			setup: func(t *testing.T, f *homeLayerFixture) {
				writeHomeLayerFile(t, filepath.Join(f.layerHome, "a-first.txt"), "plain")
				if err := os.Symlink(f.secret, filepath.Join(f.layerHome, ".bashrc")); err != nil {
					t.Fatal(err)
				}
			},
			wantMsg: "home/.bashrc is a symbolic link",
		},
		{
			name: "nested link",
			setup: func(t *testing.T, f *homeLayerFixture) {
				if err := os.MkdirAll(filepath.Join(f.layerHome, ".ssh"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.secret, filepath.Join(f.layerHome, ".ssh", "id_rsa")); err != nil {
					t.Fatal(err)
				}
			},
			wantMsg: "home/.ssh/id_rsa is a symbolic link",
		},
		{
			name: "directory link in home",
			setup: func(t *testing.T, f *homeLayerFixture) {
				if err := os.MkdirAll(f.layerHome, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(f.secret), filepath.Join(f.layerHome, "outside")); err != nil {
					t.Fatal(err)
				}
			},
			wantMsg: "home/outside is a symbolic link",
		},
		{
			name: "home is a link",
			setup: func(t *testing.T, f *homeLayerFixture) {
				if err := os.MkdirAll(filepath.Dir(f.layerHome), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(f.secret), f.layerHome); err != nil {
					t.Fatal(err)
				}
			},
			wantMsg: "home is a symbolic link",
		},
		{
			name: "layer directory is a link",
			setup: func(t *testing.T, f *homeLayerFixture) {
				writeHomeLayerFile(t, filepath.Join(f.base, "elsewhere", "home", "x"), "x")
				if err := os.MkdirAll(f.root, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(f.base, "elsewhere"), filepath.Join(f.root, "layer")); err != nil {
					t.Fatal(err)
				}
			},
			wantMsg: "its directory is a symbolic link",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newHomeLayerFixture(t)
			tc.setup(t, f)
			copied, err := f.overlay(t)
			if err == nil || copied {
				t.Fatalf("overlayHomeLayer() = %v, %v; want a refusal", copied, err)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantMsg) {
				t.Errorf("error %q does not contain %q", msg, tc.wantMsg)
			}
			if strings.Contains(msg, f.base) {
				t.Errorf("error %q names a host path", msg)
			}
			entries, rerr := os.ReadDir(f.agentHome)
			if rerr != nil || len(entries) != 0 {
				t.Errorf("agent home has %d entries (%v); a refused layer must write nothing", len(entries), rerr)
			}
			assertNoSecret(t, f.agentHome)
		})
	}
}

// TestProvisionAgent_HomeLayerSymlinkRefused: ProvisionAgent fails, naming
// the file, when a template's or the harness-config's home/ holds a link
// to a file outside it, and never copies the target into the agent home.
func TestProvisionAgent_HomeLayerSymlinkRefused(t *testing.T) {
	tests := []struct {
		name     string
		linkIn   func(scionDir string) string
		wantMsgs []string
	}{
		{
			name:     "template home",
			linkIn:   func(scionDir string) string { return filepath.Join(scionDir, "templates", "test-tpl", "home") },
			wantMsgs: []string{"template home test-tpl", "home/.bashrc is a symbolic link"},
		},
		{
			name:     "harness-config home",
			linkIn:   func(scionDir string) string { return filepath.Join(scionDir, "harness-configs", "test", "home") },
			wantMsgs: []string{"harness-config home test", "home/.bashrc is a symbolic link"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			oldWd, _ := os.Getwd()
			_ = os.Chdir(tmpDir)
			defer func() { _ = os.Chdir(oldWd) }()
			t.Setenv("HOME", tmpDir)

			runCmd(t, tmpDir, "git", "init")
			runCmd(t, tmpDir, "git", "config", "user.email", "test@example.com")
			runCmd(t, tmpDir, "git", "config", "user.name", "Test User")
			writeHomeLayerFile(t, filepath.Join(tmpDir, ".gitignore"), ".scion/agents/\n")
			runCmd(t, tmpDir, "git", "add", ".gitignore")
			runCmd(t, tmpDir, "git", "commit", "-m", "init")

			scionDir := filepath.Join(tmpDir, ".scion")
			seedTestHarnessConfig(t, scionDir, "test", "test")
			tplDir := filepath.Join(scionDir, "templates", "test-tpl")
			writeHomeLayerFile(t, filepath.Join(tplDir, "scion-agent.json"), `{"default_harness_config": "test"}`)
			writeHomeLayerFile(t, filepath.Join(tplDir, "home", "plain.txt"), "plain")

			secret := filepath.Join(t.TempDir(), "secret")
			writeHomeLayerFile(t, secret, "host-secret")
			linkDir := tc.linkIn(scionDir)
			if err := os.MkdirAll(linkDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(secret, filepath.Join(linkDir, ".bashrc")); err != nil {
				t.Fatal(err)
			}

			_, _, _, err := ProvisionAgent(context.Background(), "test-agent", "test-tpl", "", "", scionDir, "", "", "", "")
			if err == nil {
				t.Fatal("ProvisionAgent succeeded; want a refusal of the linked home file")
			}
			for _, want := range tc.wantMsgs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error %q names the link target", err)
			}
			agentsDir := filepath.Join(scionDir, "agents")
			if _, statErr := os.Stat(agentsDir); statErr == nil {
				assertNoSecret(t, agentsDir)
			}
		})
	}
}
