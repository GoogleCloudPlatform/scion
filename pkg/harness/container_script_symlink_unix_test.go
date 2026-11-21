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

package harness

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestContainerScriptHarness_Provision_RefusesLinkedConfigFiles: every
// file Provision stages from the harness-config directory is read without
// following a symbolic link; a link to a file outside the directory fails
// Provision with an error naming the file, never a host path, and the
// target is never copied into the agent home.
func TestContainerScriptHarness_Provision_RefusesLinkedConfigFiles(t *testing.T) {
	for _, name := range []string{"config.yaml", "provision.py", "dialect.yaml", "capture_auth.py", "scion_harness.py"} {
		t.Run(name, func(t *testing.T) {
			h, dir := newTestContainerScriptHarness(t)
			h.entry.Provisioner.Lib = "vendored"
			writeFile(t, filepath.Join(dir, "scion_harness.py"), "LIB_VERSION = 1\n")
			secret := filepath.Join(t.TempDir(), "secret")
			writeFile(t, secret, "host-secret")
			p := filepath.Join(dir, name)
			_ = os.Remove(p)
			if err := os.Symlink(secret, p); err != nil {
				t.Fatal(err)
			}

			agentHome := filepath.Join(t.TempDir(), "home")
			err := h.Provision(context.Background(), "a", filepath.Dir(agentHome), agentHome, "")
			if err == nil {
				t.Fatal("Provision succeeded; want a refusal of the linked file")
			}
			want := "harness-config file " + name + " is a symbolic link"
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), dir) {
				t.Errorf("error %q names a host path", err)
			}
			_ = filepath.Walk(agentHome, func(p string, info os.FileInfo, err error) error {
				if err == nil && info.Mode().IsRegular() {
					if data, _ := os.ReadFile(p); strings.Contains(string(data), "host-secret") {
						t.Errorf("link target copied into the agent home at %s", p)
					}
				}
				return nil
			})
		})
	}
}

// TestContainerScriptHarness_Provision_LinkedConfigDirRefused: a
// harness-config directory that is itself a link is not followed.
func TestContainerScriptHarness_Provision_LinkedConfigDirRefused(t *testing.T) {
	_, dir := newTestContainerScriptHarness(t)
	link := filepath.Join(t.TempDir(), "linked-hc")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	h, err := NewContainerScriptHarness(link, config.HarnessConfigEntry{
		Harness:     "testharness",
		Provisioner: &config.HarnessProvisionerConfig{Type: "container-script"},
	})
	if err != nil {
		t.Fatal(err)
	}
	agentHome := filepath.Join(t.TempDir(), "home")
	err = h.Provision(context.Background(), "a", filepath.Dir(agentHome), agentHome, "")
	if err == nil || !strings.Contains(err.Error(), "harness-config directory is a symbolic link") {
		t.Fatalf("Provision() = %v, want a linked-directory refusal", err)
	}
	if strings.Contains(err.Error(), link) {
		t.Errorf("error %q names a host path", err)
	}
}

// TestContainerScriptHarness_GetCommand_LinkedSystemPromptIgnored: the
// agent can replace a system prompt file in its home with a symbolic link;
// the link is never followed, so the target's content never reaches the
// command line, and a plain file at the next location still applies.
func TestContainerScriptHarness_GetCommand_LinkedSystemPromptIgnored(t *testing.T) {
	newHarness := func(t *testing.T) *ContainerScriptHarness {
		h, err := NewContainerScriptHarness(t.TempDir(), config.HarnessConfigEntry{
			Harness:          "claude",
			SystemPromptFile: ".claude/system-prompt.md",
			Provisioner:      &config.HarnessProvisionerConfig{Type: "container-script"},
			Command: &config.HarnessCommandConfig{
				Base:             []string{"claude"},
				SystemPromptFlag: "--system-prompt",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		h.agentHome = t.TempDir()
		return h
	}
	secret := filepath.Join(t.TempDir(), "secret")
	writeFile(t, secret, "host-secret")
	staged := filepath.Join(".scion", "harness", "inputs", "system-prompt.md")
	native := filepath.Join(".claude", "system-prompt.md")
	symlink := func(t *testing.T, target, link string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("staged file linked, native plain", func(t *testing.T) {
		h := newHarness(t)
		symlink(t, secret, filepath.Join(h.agentHome, staged))
		writeFile(t, filepath.Join(h.agentHome, native), "Native prompt.\n")
		want := []string{"claude", "--system-prompt", "Native prompt."}
		if got := h.GetCommand("", false, nil); !reflect.DeepEqual(got, want) {
			t.Errorf("GetCommand() = %q, want %q", got, want)
		}
	})
	t.Run("both linked", func(t *testing.T) {
		h := newHarness(t)
		symlink(t, secret, filepath.Join(h.agentHome, staged))
		symlink(t, secret, filepath.Join(h.agentHome, native))
		want := []string{"claude"}
		if got := h.GetCommand("", false, nil); !reflect.DeepEqual(got, want) {
			t.Errorf("GetCommand() = %q, want %q", got, want)
		}
	})
	t.Run("linked directory component", func(t *testing.T) {
		h := newHarness(t)
		outside := filepath.Join(t.TempDir(), "claude-dir")
		writeFile(t, filepath.Join(outside, "system-prompt.md"), "host-secret")
		symlink(t, outside, filepath.Join(h.agentHome, ".claude"))
		want := []string{"claude"}
		if got := h.GetCommand("", false, nil); !reflect.DeepEqual(got, want) {
			t.Errorf("GetCommand() = %q, want %q", got, want)
		}
	})
}
