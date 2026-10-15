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

package agentplugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTreeDigest(t *testing.T) {
	src := t.TempDir()
	writeFiles(t, src, map[string]string{"plugin.json": minimalManifest, "bin/x*": "x", "skills/s/SKILL.md": "s"})
	d1, err := TreeDigest(openRoot(t, src), ".")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d1, "sha256:") || len(d1) != len("sha256:")+64 {
		t.Fatalf("digest = %q", d1)
	}

	dst := t.TempDir()
	if _, err := CopyTree(openRoot(t, src), ".", openRoot(t, dst), "c", DefaultLimits); err != nil {
		t.Fatal(err)
	}
	if d2, _ := TreeDigest(openRoot(t, dst), "c"); d2 != d1 {
		t.Fatalf("digest changed across copy: %s vs %s", d1, d2)
	}

	mutate := map[string]func(dir string){
		"content": func(dir string) { writeFiles(t, dir, map[string]string{"skills/s/SKILL.md": "changed"}) },
		"exec":    func(dir string) { _ = os.Chmod(filepath.Join(dir, "bin/x"), 0o644) },
		"newfile": func(dir string) { writeFiles(t, dir, map[string]string{"extra": ""}) },
		"newdir":  func(dir string) { _ = os.Mkdir(filepath.Join(dir, "empty"), 0o755) },
	}
	for name, fn := range mutate {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if _, err := CopyTree(openRoot(t, src), ".", openRoot(t, dir), "c", DefaultLimits); err != nil {
				t.Fatal(err)
			}
			fn(filepath.Join(dir, "c"))
			if d, _ := TreeDigest(openRoot(t, dir), "c"); d == d1 {
				t.Fatalf("digest unchanged after %s", name)
			}
		})
	}
}
