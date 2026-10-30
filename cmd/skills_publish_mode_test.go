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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// TestBuildSkillManifest_CarriesMode pins that `scion skills publish`
// records each file's normalised mode in the finalize manifest
// (ptone/scion#5657); without it the Hub serves every skill file 0644.
func TestBuildSkillManifest_CarriesMode(t *testing.T) {
	files := []publishFile{
		{path: "SKILL.md", size: 8, hash: transfer.HashBytes([]byte("SKILL.md")), mode: publishFileMode(0o644)},
		{path: "run.sh", size: 6, hash: transfer.HashBytes([]byte("run.sh")), mode: publishFileMode(0o755)},
		{path: "suid.sh", size: 7, hash: transfer.HashBytes([]byte("suid.sh")), mode: publishFileMode(os.ModeSetuid | os.ModeSticky | 0o4777)},
		{path: "group.txt", size: 9, hash: transfer.HashBytes([]byte("group.txt")), mode: publishFileMode(0o664)},
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
		if want := transfer.HashBytes([]byte(f.Path)); f.Hash != want {
			t.Errorf("%s: manifest hash %q, want the collected %q", f.Path, f.Hash, want)
		}
	}
}

// TestBuildSkillManifest_RequiresCollectedHash pins that the manifest is
// built only from hashes recorded at collection.
func TestBuildSkillManifest_RequiresCollectedHash(t *testing.T) {
	_, err := buildSkillManifest([]publishFile{{path: "SKILL.md", size: 1, mode: "0644"}})
	if err == nil {
		t.Fatal("buildSkillManifest accepted a file with no collected hash")
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
