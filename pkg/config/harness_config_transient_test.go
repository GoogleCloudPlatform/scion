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

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

func TestIsHarnessConfigTransientFile(t *testing.T) {
	cases := map[string]bool{
		"config.yaml.bak.20261003T193320Z":        true,
		"provision.py.bak.20261003T193320Z":       true,
		"home/.claude/x.json.bak.20261003T19332Z": true,
		".config.yaml.tmp-123456789":              true,
		"home/.provision.py.tmp-42":               true,
		"config.yaml":                             false,
		"dialect.yaml":                            false,
		"provision.py":                            false,
		"notes.bak.md":                            false,
		"config.yaml.bak":                         false,
		"file.tmp-1":                              false,
		".bashrc":                                 false,
	}
	for name, want := range cases {
		if got := IsHarnessConfigTransientFile(name); got != want {
			t.Errorf("IsHarnessConfigTransientFile(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestHarnessConfigTransientMatchesProducers ties the predicate to the names
// the producers actually write, so a format change cannot drift silently.
func TestHarnessConfigTransientMatchesProducers(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.yaml")
	writeFile(t, target, "harness: claude\n")

	backup, err := backupFile(target, time.Date(2026, 10, 3, 19, 33, 20, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !IsHarnessConfigTransientFile(backup) {
		t.Errorf("backupFile output %q is not treated as transient", backup)
	}

	// Same name template as the atomic-write helpers.
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = tmp.Close()
	if !IsHarnessConfigTransientFile(tmp.Name()) {
		t.Errorf("atomic-write temp %q is not treated as transient", tmp.Name())
	}
}

func harnessConfigDirWithTransientFiles(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "harness: claude\n")
	writeFile(t, filepath.Join(dir, "dialect.yaml"), "dialect: claude\n")
	writeFile(t, filepath.Join(dir, "config.yaml.bak.20261003T193320Z"), "old\n")
	writeFile(t, filepath.Join(dir, "provision.py.bak.20261003T193320Z"), "old\n")
	writeFile(t, filepath.Join(dir, ".provision.py.tmp-123456"), "partial\n")
	return dir
}

func TestCollectFilesHarnessConfigTransientPatterns(t *testing.T) {
	dir := harnessConfigDirWithTransientFiles(t)
	files, err := transfer.CollectFiles(dir, HarnessConfigTransientPatterns)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		got = append(got, f.Path)
	}
	want := []string{"config.yaml", "dialect.yaml"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("collected %v, want %v", got, want)
	}
}

func TestComputeHarnessConfigRevisionIgnoresTransientFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "harness: claude\n")
	writeFile(t, filepath.Join(dir, "dialect.yaml"), "dialect: claude\n")
	before := ComputeHarnessConfigRevision(dir)
	if before == "" {
		t.Fatal("expected non-empty revision")
	}

	writeFile(t, filepath.Join(dir, "config.yaml.bak.20261003T193320Z"), "old\n")
	if got := ComputeHarnessConfigRevision(dir); got != before {
		t.Errorf("revision changed after adding a .bak file: %q -> %q", before, got)
	}
	writeFile(t, filepath.Join(dir, "provision.py.bak.20261003T193320Z"), "old\n")
	writeFile(t, filepath.Join(dir, ".provision.py.tmp-123456"), "partial\n")
	if got := ComputeHarnessConfigRevision(dir); got != before {
		t.Errorf("revision changed after adding backup/temp files: %q -> %q", before, got)
	}

	// A normal file still counts.
	writeFile(t, filepath.Join(dir, "dialect.yaml"), "dialect: changed\n")
	if got := ComputeHarnessConfigRevision(dir); got == before {
		t.Error("revision unchanged after editing dialect.yaml")
	}
}
