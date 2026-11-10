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
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

const outsideAgentConfig = `{"harness":"outside-marker"}`

// plantAgentConfig makes agentDir/scion-agent.json the placed entry kind
// ("link" to an existing outside file, "dangling" link, or "hardlink" to an
// outside file) and returns the outside path.
func plantAgentConfig(t *testing.T, agentDir, kind string) string {
	t.Helper()
	outside := filepath.Join(t.TempDir(), "outside.json")
	if kind != "dangling" {
		if err := os.WriteFile(outside, []byte(outsideAgentConfig), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entry := filepath.Join(agentDir, agentConfigFileName)
	switch kind {
	case "link", "dangling":
		if err := os.Symlink(outside, entry); err != nil {
			t.Fatal(err)
		}
	case "hardlink":
		if err := os.Link(outside, entry); err != nil {
			t.Fatalf("hard link: %v", err)
		}
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	return outside
}

// requireOutsideUntouched checks the outside file is byte-identical (or, for
// a dangling link, still absent) and the placed entry is still there.
func requireOutsideUntouched(t *testing.T, agentDir, outside, kind string) {
	t.Helper()
	data, err := os.ReadFile(outside)
	if kind == "dangling" {
		if err == nil {
			t.Fatalf("the dangling link's target was created: %q", data)
		}
	} else if err != nil || !bytes.Equal(data, []byte(outsideAgentConfig)) {
		t.Fatalf("the outside file changed: %q, %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(agentDir, agentConfigFileName)); err != nil {
		t.Fatalf("the planted entry is gone: %v", err)
	}
}

var refusedAgentConfigKinds = []string{"link", "dangling", "hardlink"}

// An agent dir with a populated home and a record, whose scion-agent.json is
// a link (to an existing file or dangling) or a hard link, is refused: no
// config is written through it and nothing is removed.
func TestHandleAgentDirRefusedConfigEntryIsNeverWrittenThrough(t *testing.T) {
	for _, kind := range refusedAgentConfigKinds {
		t.Run(kind, func(t *testing.T) {
			agentDir, agentHome := splitLayout(t)
			if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"),
				[]byte(`{"name":"a","template":"default","harnessConfig":"generic"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			outside := plantAgentConfig(t, agentDir, kind)

			err := handleAgentDirWithoutConfigGuarded(testAgentDirGuard("a", agentDir), "a", agentDir, agentHome, filepath.Join(agentDir, "workspace"))
			if !errors.Is(err, config.ErrAgentStateConflict) {
				t.Fatalf("err = %v; want ErrAgentStateConflict", err)
			}
			if bytes.Contains([]byte(err.Error()), []byte(outside)) || bytes.Contains([]byte(err.Error()), []byte(agentDir)) {
				t.Errorf("error names a path: %v", err)
			}
			requireOutsideUntouched(t, agentDir, outside, kind)
			if _, err := os.Stat(filepath.Join(agentHome, ".history")); err != nil {
				t.Errorf("home data removed: %v", err)
			}
		})
	}
}

// With an empty home and no agent record, a refused config entry is the only
// thing that keeps the dir from the "no home state" branch (removal as stale,
// then re-provision): the handler refuses with ErrAgentStateConflict and the
// dir and the placed entry stay.
func TestHandleAgentDirRefusedConfigEntryEmptyHomeIsKept(t *testing.T) {
	for _, kind := range refusedAgentConfigKinds {
		t.Run(kind, func(t *testing.T) {
			agentDir, agentHome := splitLayout(t)
			if err := os.Remove(filepath.Join(agentHome, ".history")); err != nil {
				t.Fatal(err)
			}
			outside := plantAgentConfig(t, agentDir, kind)

			err := handleAgentDirWithoutConfigGuarded(testAgentDirGuard("a", agentDir), "a", agentDir, agentHome, filepath.Join(agentDir, "workspace"))
			if !errors.Is(err, config.ErrAgentStateConflict) {
				t.Fatalf("err = %v; want ErrAgentStateConflict", err)
			}
			if _, err := os.Stat(agentDir); err != nil {
				t.Fatalf("agent dir removed: %v", err)
			}
			requireOutsideUntouched(t, agentDir, outside, kind)
		})
	}
}

// The provisioned and kept-dir decisions treat a refused config entry as
// present: never re-provision over it, never expire or purge the dir.
func TestRefusedConfigEntryCountsAsPresent(t *testing.T) {
	for _, kind := range refusedAgentConfigKinds {
		t.Run(kind, func(t *testing.T) {
			agentDir, agentHome := splitLayout(t)
			if err := os.Remove(filepath.Join(agentHome, ".history")); err != nil {
				t.Fatal(err)
			}
			outside := plantAgentConfig(t, agentDir, kind)

			if !agentStateProvisioned(agentDir) {
				t.Error("agentStateProvisioned = false; a refused entry must count as provisioned")
			}

			recordKeptAgentDir("a", agentDir, "test")
			origNow := keptAgentDirNow
			t.Cleanup(func() { keptAgentDirNow = origNow })
			later := time.Now().Add(72 * time.Hour)
			keptAgentDirNow = func() time.Time { return later }
			if _, expired := keptAgentDirExpired(agentDir, time.Hour, later); expired {
				t.Error("keptAgentDirExpired = true; a dir with a refused config entry must not expire")
			}
			if purgeExpiredKeptAgentDirGuarded(testAgentDirGuard("a", agentDir), "a", agentDir, agentHome, time.Hour) {
				t.Error("the kept-dir sweep purged a dir with a refused config entry")
			}
			if _, err := os.Stat(agentDir); err != nil {
				t.Fatalf("agent dir removed: %v", err)
			}
			requireOutsideUntouched(t, agentDir, outside, kind)
		})
	}

	// Control: without any config entry the same kept dir expires and is
	// purged.
	agentDir, agentHome := splitLayout(t)
	if err := os.Remove(filepath.Join(agentHome, ".history")); err != nil {
		t.Fatal(err)
	}
	recordKeptAgentDir("a", agentDir, "test")
	origNow := keptAgentDirNow
	t.Cleanup(func() { keptAgentDirNow = origNow })
	later := time.Now().Add(72 * time.Hour)
	keptAgentDirNow = func() time.Time { return later }
	if !purgeExpiredKeptAgentDirGuarded(testAgentDirGuard("a", agentDir), "a", agentDir, agentHome, time.Hour) {
		t.Error("control: an expired kept dir with no config entry was not purged")
	}
}

// The shared writer never writes through a refused entry, replaces a plain
// file, and creates the file when absent; the reader refuses a refused entry.
func TestWriteAgentConfigFileNeverWritesThroughLink(t *testing.T) {
	for _, kind := range refusedAgentConfigKinds {
		t.Run(kind, func(t *testing.T) {
			agentDir := t.TempDir()
			outside := plantAgentConfig(t, agentDir, kind)
			if err := WriteAgentConfigFile(agentDir, []byte(`{"harness":"new"}`)); !errors.Is(err, config.ErrAgentStateConflict) {
				t.Fatalf("WriteAgentConfigFile err = %v; want ErrAgentStateConflict", err)
			}
			if _, err := ReadAgentConfigFile(agentDir); !errors.Is(err, config.ErrAgentStateConflict) {
				t.Errorf("ReadAgentConfigFile err = %v; want ErrAgentStateConflict", err)
			}
			requireOutsideUntouched(t, agentDir, outside, kind)
		})
	}
	t.Run("absent then regular", func(t *testing.T) {
		agentDir := t.TempDir()
		for _, content := range []string{`{"harness":"one"}`, `{"harness":"two"}`} {
			if err := WriteAgentConfigFile(agentDir, []byte(content)); err != nil {
				t.Fatalf("WriteAgentConfigFile: %v", err)
			}
			got, err := ReadAgentConfigFile(agentDir)
			if err != nil || string(got) != content {
				t.Fatalf("ReadAgentConfigFile = %q, %v; want %q", got, err, content)
			}
		}
		entries, _ := os.ReadDir(agentDir)
		if len(entries) != 1 {
			t.Errorf("agent dir holds %d entries; want only %s (no temp file left)", len(entries), agentConfigFileName)
		}
	})
}

// ProvisionAgent's config write refuses a placed link at the agent's
// scion-agent.json and leaves the outside file unchanged.
func TestProvisionAgentNeverWritesConfigThroughLink(t *testing.T) {
	for _, kind := range []string{"link", "dangling"} {
		t.Run(kind, func(t *testing.T) {
			projectScionDir := hubDefaultsFixture(t, "schema_version: \"1\"\ndefault_harness_config: test-harness\nharness_configs:\n  test-harness:\n    harness: test-harness\n", "")
			agentDir := config.GetAgentDir(projectScionDir, "linked-agent", false)
			if err := os.MkdirAll(agentDir, 0o755); err != nil {
				t.Fatal(err)
			}
			outside := plantAgentConfig(t, agentDir, kind)
			_, _, _, err := ProvisionAgent(context.Background(), "linked-agent", "", "", "test-harness", projectScionDir, "", "", "", "")
			if err == nil {
				t.Fatal("ProvisionAgent succeeded over a linked scion-agent.json")
			}
			requireOutsideUntouched(t, agentDir, outside, kind)
		})
	}
}

// An agent directory that is itself a symbolic link (agents/<name> linked to
// an outside directory) is refused by the shared writer and reader: nothing
// is written into the outside directory and the error names no path.
func TestAgentConfigFileRefusesLinkedAgentDir(t *testing.T) {
	tmp := t.TempDir()
	outsideDir := filepath.Join(tmp, "outside-dir")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(tmp, "project", ".scion", "agents", "a")
	if err := os.MkdirAll(filepath.Dir(agentDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, agentDir); err != nil {
		t.Fatal(err)
	}

	err := WriteAgentConfigFile(agentDir, []byte(`{"harness":"new"}`))
	if !errors.Is(err, config.ErrAgentStateConflict) {
		t.Fatalf("WriteAgentConfigFile err = %v; want ErrAgentStateConflict", err)
	}
	if bytes.Contains([]byte(err.Error()), []byte(tmp)) {
		t.Errorf("error names a path: %v", err)
	}
	if _, err := ReadAgentConfigFile(agentDir); !errors.Is(err, config.ErrAgentStateConflict) {
		t.Errorf("ReadAgentConfigFile err = %v; want ErrAgentStateConflict", err)
	}
	if entries, _ := os.ReadDir(outsideDir); len(entries) != 0 {
		t.Fatalf("the outside directory gained %d entries; nothing may be written there", len(entries))
	}

	// Control: the same layout with a real directory works.
	realDir := filepath.Join(tmp, "project", ".scion", "agents", "b")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgentConfigFile(realDir, []byte(`{"harness":"ok"}`)); err != nil {
		t.Fatalf("control: WriteAgentConfigFile: %v", err)
	}
}
