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

package transfer

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWriteFileInRootMode_ExactModeOverUmaskAndExistingFile(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)

	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	if err := WriteFileInRootMode(root, "a/bin/run", []byte("x"), ModeExecutable); err != nil {
		t.Fatal(err)
	}
	// An existing executable file rewritten as regular loses the bit.
	if err := os.WriteFile(filepath.Join(dir, "old"), []byte("y"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileInRootMode(root, "old", []byte("z"), ModeRegular); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{"a/bin/run": 0o755, "old": 0o644} {
		info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s: mode = %v, want %v", p, got, want)
		}
	}
}
