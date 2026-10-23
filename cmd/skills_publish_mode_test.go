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

package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBuildSkillManifest_CarriesMode pins that `scion skills publish`
// records each file's normalised mode in the finalize manifest
// (ptone/scion#5657); without it the Hub serves every skill file 0644.
func TestBuildSkillManifest_CarriesMode(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	files := []publishFile{
		{path: "SKILL.md", absPath: write("SKILL.md"), size: 8, mode: publishFileMode(0o644)},
		{path: "run.sh", absPath: write("run.sh"), size: 6, mode: publishFileMode(0o755)},
		{path: "suid.sh", absPath: write("suid.sh"), size: 7, mode: publishFileMode(os.ModeSetuid | os.ModeSticky | 0o4777)},
		{path: "group.txt", absPath: write("group.txt"), size: 9, mode: publishFileMode(0o664)},
	}
	m, err := buildSkillManifest(files)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"SKILL.md": "0644", "run.sh": "0755", "suid.sh": "0755", "group.txt": "0644"}
	for _, f := range m.Files {
		if f.Mode != want[f.Path] {
			t.Errorf("%s: manifest mode %q, want %q", f.Path, f.Mode, want[f.Path])
		}
		if f.Hash == "" {
			t.Errorf("%s: manifest hash missing", f.Path)
		}
	}
}

func TestPublishFileMode_NoExecBitsOnWindows(t *testing.T) {
	orig := localModesCarryExecBit
	localModesCarryExecBit = func() bool { return false }
	t.Cleanup(func() { localModesCarryExecBit = orig })

	if got := publishFileMode(0o777); got != "0644" {
		t.Errorf("publishFileMode = %q, want 0644 on a platform without exec bits", got)
	}
}
