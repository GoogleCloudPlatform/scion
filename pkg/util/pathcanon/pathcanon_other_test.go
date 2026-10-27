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

//go:build !windows

package pathcanon

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/GoogleCloudPlatform/scion/internal/testutil/wintest"
)

func TestValidName_Unix(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", "/", "a\x00b"} {
		if err := ValidName(bad); !errors.Is(err, ErrInvalidName) {
			t.Errorf("ValidName(%q) = %v, want ErrInvalidName", bad, err)
		}
	}
	// Unix accepts names Windows refuses: callers keep today's checks.
	for _, ok := range []string{"a", ".hidden", "a.", "a ", "CON", "a:b", `a\b`, "..."} {
		if err := ValidName(ok); err != nil {
			t.Errorf("ValidName(%q) = %v, want nil", ok, err)
		}
	}
}

func TestValidInputPath_Unix(t *testing.T) {
	if err := ValidInputPath(""); err == nil {
		t.Error("empty path accepted")
	}
	if err := ValidInputPath("a\x00b"); err == nil {
		t.Error("NUL accepted")
	}
	for _, ok := range []string{"/a/b", "a/../b", "./x"} {
		if err := ValidInputPath(ok); err != nil {
			t.Errorf("ValidInputPath(%q) = %v", ok, err)
		}
	}
}

func TestResolve_Unix(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	wintest.MustSymlink(t, filepath.Join(root, "real"), filepath.Join(root, "link"))
	got, err := Resolve(filepath.Join(root, "link", "missing", "x"))
	if err != nil || got != filepath.Join(root, "real", "missing", "x") {
		t.Fatalf("Resolve through a link with a missing tail = %q, %v", got, err)
	}
	if got, err := Resolve(filepath.Join(root, "real", "..", "real")); err != nil || got != filepath.Join(root, "real") {
		t.Fatalf("Resolve with .. = %q, %v", got, err)
	}
	if _, err := Resolve(""); err == nil {
		t.Fatal("Resolve of the empty path succeeded")
	}
}

func TestWithin_SymlinkEscapesUnix(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	wintest.MustSymlink(t, outside, filepath.Join(root, "esc"))
	if in, err := Within(root, filepath.Join(root, "esc", "x")); err != nil || in {
		t.Fatalf("Within through an escaping symlink = %v, %v; want false", in, err)
	}
	if in, err := Within(root, filepath.Join(root, "real", "x")); err != nil || !in {
		t.Fatalf("control: Within of a real child = %v, %v; want true", in, err)
	}
	if in, err := Within(root, root); err != nil || !in {
		t.Fatalf("Within(root, root) = %v, %v", in, err)
	}
	if in, err := Within(root, root+"x"); err != nil || in {
		t.Fatalf("Within of a sibling sharing the prefix = %v, %v; want false", in, err)
	}
	if in, err := Within(root, filepath.Join(root, "..", filepath.Base(outside))); err != nil || in {
		t.Fatalf("Within with .. escaping = %v, %v; want false", in, err)
	}
}

func TestWithin_DanglingSymlinkRefusedUnix(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	wintest.MustSymlink(t, filepath.Join(outside, "missing"), filepath.Join(root, "s"))
	for _, p := range []string{filepath.Join(root, "s"), filepath.Join(root, "s", "x", "y")} {
		if in, err := Within(root, p); !errors.Is(err, ErrDanglingLink) || in {
			t.Fatalf("Within(root, %q) = %v, %v; want ErrDanglingLink", p, in, err)
		}
	}
	// Control: the same name as a real, missing directory is inside.
	if in, err := Within(root, filepath.Join(root, "s2", "x")); err != nil || !in {
		t.Fatalf("control: Within of a missing tail = %v, %v; want true", in, err)
	}
	// Once its target exists, the same entry resolves, to outside the root.
	if err := os.Mkdir(filepath.Join(outside, "missing"), 0o700); err != nil {
		t.Fatal(err)
	}
	if in, err := Within(root, filepath.Join(root, "s", "x")); err != nil || in {
		t.Fatalf("Within through the now-valid symlink = %v, %v; want false", in, err)
	}
}

func TestResolve_FileComponentIsNotDirUnix(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(file, "x"), filepath.Join(file, "x", "y")} {
		if _, err := Resolve(p); !errors.Is(err, syscall.ENOTDIR) {
			t.Fatalf("Resolve(%q) = %v, want ENOTDIR", p, err)
		}
	}
	if _, err := Resolve(file); err != nil {
		t.Fatalf("control: Resolve(file) = %v", err)
	}
}
