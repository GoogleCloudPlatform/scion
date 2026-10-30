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

//go:build windows

package pathcanon

// One negative test per Windows path alias form, each with a
// positive control: the same file under its legitimate name is Within the
// root, or the legitimate name is accepted.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/GoogleCloudPlatform/scion/internal/testutil/wintest"
)

// fixture creates root/<inside>/file and a sibling directory outside root.
func fixture(t *testing.T) (root, inside, outside string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "root")
	inside = filepath.Join(root, "longdirectoryname-inside")
	outside = filepath.Join(base, "longdirectoryname-outside")
	for _, d := range []string{inside, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(inside, "file.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, inside, outside
}

func mustWithin(t *testing.T, root, p string, want bool) {
	t.Helper()
	got, err := Within(root, p)
	if err != nil {
		t.Fatalf("Within(%q, %q): %v", root, p, err)
	}
	if got != want {
		t.Fatalf("Within(%q, %q) = %v, want %v", root, p, got, want)
	}
}

func TestWithin_ShortName8dot3(t *testing.T) {
	root, inside, outside := fixture(t)
	shortIn := wintest.ShortPath(t, inside)
	shortOut := wintest.ShortPath(t, outside)
	if !strings.Contains(filepath.Base(shortIn), "~") || !strings.Contains(filepath.Base(shortOut), "~") {
		t.Fatalf("no 8.3 short names were generated (%q, %q): the Windows job must enable 8dot3 name creation on the test volume (fsutil 8dot3name set); this test fails rather than passing vacuously", shortIn, shortOut)
	}
	// The short name of a directory inside the root resolves inside...
	mustWithin(t, root, filepath.Join(shortIn, "file.txt"), true)
	// ...and the short name of one outside resolves outside, even with a
	// short-name root.
	mustWithin(t, root, shortOut, false)
	mustWithin(t, wintest.ShortPath(t, root), filepath.Join(inside, "file.txt"), true)
	// Control: the long name.
	mustWithin(t, root, filepath.Join(inside, "file.txt"), true)
	r1, _ := Resolve(shortIn)
	r2, _ := Resolve(inside)
	if r1 != r2 {
		t.Fatalf("Resolve(short) = %q, Resolve(long) = %q; want one canonical form", r1, r2)
	}
}

func TestValidName_ADS(t *testing.T) {
	for _, bad := range []string{"a:stream", "a::$DATA", "file.txt:secret:$DATA", ":x"} {
		if err := ValidName(bad); !errors.Is(err, ErrInvalidName) {
			t.Errorf("ValidName(%q) = %v, want ErrInvalidName", bad, err)
		}
	}
	root, inside, _ := fixture(t)
	if _, err := Within(root, filepath.Join(inside, "file.txt:secret")); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("Within of an ADS path: %v, want ErrInvalidName", err)
	}
	// Control.
	if err := ValidName("a.stream"); err != nil {
		t.Fatalf("control: ValidName(a.stream) = %v", err)
	}
	mustWithin(t, root, filepath.Join(inside, "file.txt"), true)
}

func TestValidName_DeviceNames(t *testing.T) {
	for _, bad := range []string{"CON", "con", "nul.txt", "NUL.tar.gz", "COM1", "com9.log", "LPT1", "LPT¹", "COM²", "lpt³.txt", "AUX", "PRN", "CONIN$", "CONOUT$", "CON .txt"} {
		if err := ValidName(bad); !errors.Is(err, ErrInvalidName) {
			t.Errorf("ValidName(%q) = %v, want ErrInvalidName", bad, err)
		}
	}
	root, inside, _ := fixture(t)
	if _, err := Resolve(filepath.Join(inside, "NUL")); err == nil {
		t.Fatal("Resolve of a device name succeeded")
	}
	if _, err := Within(root, filepath.Join(inside, "con.txt")); err == nil {
		t.Fatal("Within of a device-name path succeeded")
	}
	// Control: names that only start like a device name.
	for _, ok := range []string{"CONSOLE", "nullable.txt", "COM10", "LPT", "auxiliary", "conf.d"} {
		if err := ValidName(ok); err != nil {
			t.Errorf("control: ValidName(%q) = %v", ok, err)
		}
	}
	mustWithin(t, root, filepath.Join(inside, "console.txt"), true)
}

func TestValidInputPath_ExtendedAndUNC(t *testing.T) {
	for _, bad := range []string{`\\?\C:\Windows`, `\\.\pipe\x`, `\\server\share\x`, `//server/share/x`, `\/server\share`, `\??\C:\x`, `//?/C:/x`, `C:\dir\a:b`, `C:\dir\NUL`} {
		if err := ValidInputPath(bad); !errors.Is(err, ErrInvalidName) {
			t.Errorf("ValidInputPath(%q) = %v, want ErrInvalidName", bad, err)
		}
	}
	// Control: ordinary absolute, drive-relative and relative paths.
	for _, ok := range []string{`C:\Users\u\out.json`, `C:out.json`, `rel\out.json`, `..\x\y`, `.\x`, `\rooted\x`, `D:/mixed/sep`} {
		if err := ValidInputPath(ok); err != nil {
			t.Errorf("control: ValidInputPath(%q) = %v", ok, err)
		}
	}
}

func TestWithin_CaseInsensitive(t *testing.T) {
	root, inside, _ := fixture(t)
	mustWithin(t, strings.ToUpper(root), strings.ToLower(filepath.Join(inside, "file.txt")), true)
	mustWithin(t, root, strings.ToUpper(filepath.Join(inside, "missing", "tail")), true)
	// Control: a sibling whose name only extends the root's is outside,
	// whatever the case.
	sib := root + "SIBLING"
	if err := os.Mkdir(sib, 0o700); err != nil {
		t.Fatal(err)
	}
	mustWithin(t, strings.ToLower(root), strings.ToUpper(sib), false)

	// NTFS compares names through its uppercase table, not Unicode case
	// folding: "Kit" and "\u212Ait" (Kelvin sign) are two distinct
	// directories, although strings.EqualFold calls them equal.
	base := t.TempDir()
	kit, kelvin := filepath.Join(base, "Kit"), filepath.Join(base, "\u212Ait")
	for _, d := range []string{kit, kelvin} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.EqualFold("Kit", "\u212Ait") {
		t.Fatal("precondition: strings.EqualFold no longer equates K and the Kelvin sign; this case no longer discriminates")
	}
	mustWithin(t, kit, filepath.Join(kelvin, "x"), false)
	mustWithin(t, kit, kelvin, false)
	// Control: a plain case variant of the same directory is inside.
	mustWithin(t, kit, filepath.Join(base, "KIT", "x"), true)
}

func TestValidName_TrailingDotSpace(t *testing.T) {
	for _, bad := range []string{"a.", "a ", "a. ", "file.txt.", "dir ", "..."} {
		if err := ValidName(bad); !errors.Is(err, ErrInvalidName) {
			t.Errorf("ValidName(%q) = %v, want ErrInvalidName", bad, err)
		}
	}
	root, inside, _ := fixture(t)
	// The file system strips trailing dots and spaces, so these name the
	// same file and resolve to the same canonical form.
	r1, err := Resolve(filepath.Join(inside, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{inside + `\file.txt.`, inside + `\file.txt  `, inside + `.\file.txt`} {
		r2, err := Resolve(alias)
		if err != nil || r2 != r1 {
			t.Fatalf("Resolve(%q) = %q, %v; want %q", alias, r2, err, r1)
		}
	}
	if err := ValidName("a.b"); err != nil {
		t.Fatalf("control: ValidName(a.b) = %v", err)
	}
	mustWithin(t, root, inside+`\file.txt.`, true)
}

func TestWithin_MixedSeparators(t *testing.T) {
	root, inside, outside := fixture(t)
	slashed := strings.ReplaceAll(filepath.Join(inside, "file.txt"), `\`, "/")
	mustWithin(t, root, slashed, true)
	mustWithin(t, strings.ReplaceAll(root, `\`, "/"), filepath.Join(inside, "file.txt"), true)
	mustWithin(t, root, strings.ReplaceAll(outside, `\`, "/"), false)
	mustWithin(t, root, strings.ReplaceAll(root, `\`, "/")+`/..\`+filepath.Base(outside), false)
	// Control.
	mustWithin(t, root, filepath.Join(inside, "file.txt"), true)
}

func TestWithin_JunctionEscapes(t *testing.T) {
	root, inside, outside := fixture(t)
	wintest.MustJunction(t, outside, filepath.Join(root, "j"))
	mustWithin(t, root, filepath.Join(root, "j", "x"), false)
	mustWithin(t, root, filepath.Join(root, "j"), false)
	mustWithin(t, root, filepath.Join(inside, "x"), true)
}

func TestWithin_SymlinkEscapes(t *testing.T) {
	root, inside, outside := fixture(t)
	wintest.MustSymlink(t, outside, filepath.Join(root, "s"))
	mustWithin(t, root, filepath.Join(root, "s", "x"), false)
	mustWithin(t, root, filepath.Join(inside, "x"), true)
}

func TestResolve_OutputPrefixConsistent(t *testing.T) {
	_, inside, _ := fixture(t)
	existing, err := Resolve(filepath.Join(inside, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	missing, err := Resolve(filepath.Join(inside, "no", "such"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{existing, missing} {
		if !strings.HasPrefix(r, `\\?\`) {
			t.Fatalf("Resolve output %q lacks the \\\\?\\ prefix", r)
		}
		again, err := Resolve(r)
		if err != nil || again != r {
			t.Fatalf("Resolve(Resolve(p)) = %q, %v; want %q (idempotent)", again, err, r)
		}
	}
	if !strings.HasSuffix(missing, `\no\such`) {
		t.Fatalf("missing tail not appended: %q", missing)
	}
	// Resolve output may be passed straight back (it is not caller input),
	// but the same \\?\ string as caller input is refused by ValidInputPath.
	if err := ValidInputPath(existing); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("ValidInputPath(%q) = %v, want ErrInvalidName", existing, err)
	}
}

// A dangling symlink is an existing entry that cannot be followed. It must
// not be taken for a not-yet-existing tail: creating below it would create
// below its target, outside the root.
func TestWithin_DanglingSymlinkRefused(t *testing.T) {
	root, _, outside := fixture(t)
	// os.Symlink makes a directory symlink only if the target is a
	// directory when the link is created: create it, link, then remove it.
	target := filepath.Join(outside, "missing")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	wintest.MustSymlink(t, target, filepath.Join(root, "s"))
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(root, "s"), filepath.Join(root, "s", "x"), filepath.Join(root, "s", "x", "y")} {
		if in, err := Within(root, p); !errors.Is(err, ErrDanglingLink) || in {
			t.Fatalf("Within(root, %q) = %v, %v; want ErrDanglingLink", p, in, err)
		}
		if _, err := Resolve(p); !errors.Is(err, ErrDanglingLink) {
			t.Fatalf("Resolve(%q) = %v, want ErrDanglingLink", p, err)
		}
	}
	// Control: the same name as a real, missing directory is a tail inside
	// the root.
	mustWithin(t, root, filepath.Join(root, "s2", "x"), true)
	// Once its target exists again, the same entry resolves, to outside
	// the root.
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	mustWithin(t, root, filepath.Join(root, "s", "x"), false)
}

// A link made to a missing target is a file symlink on Windows (os.Symlink
// picks the type from the target at creation): dangling, it is refused.
func TestWithin_DanglingFileSymlinkRefused(t *testing.T) {
	root, _, outside := fixture(t)
	wintest.MustSymlink(t, filepath.Join(outside, "never"), filepath.Join(root, "sf"))
	for _, p := range []string{filepath.Join(root, "sf"), filepath.Join(root, "sf", "x")} {
		if in, err := Within(root, p); !errors.Is(err, ErrDanglingLink) || in {
			t.Fatalf("Within(root, %q) = %v, %v; want ErrDanglingLink", p, in, err)
		}
		if _, err := Resolve(p); !errors.Is(err, ErrDanglingLink) {
			t.Fatalf("Resolve(%q) = %v, want ErrDanglingLink", p, err)
		}
	}
	// Control: the same name as a missing tail.
	mustWithin(t, root, filepath.Join(root, "sf2", "x"), true)
}

func TestWithin_DanglingJunctionRefused(t *testing.T) {
	root, _, outside := fixture(t)
	target := filepath.Join(outside, "gone")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	wintest.MustJunction(t, target, filepath.Join(root, "j"))
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(root, "j"), filepath.Join(root, "j", "x")} {
		if in, err := Within(root, p); !errors.Is(err, ErrDanglingLink) || in {
			t.Fatalf("Within(root, %q) = %v, %v; want ErrDanglingLink", p, in, err)
		}
	}
	mustWithin(t, root, filepath.Join(root, "j2", "x"), true)
	// Once its target exists again, the same junction resolves outside.
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	mustWithin(t, root, filepath.Join(root, "j", "x"), false)
}

// Nothing can exist below a regular file: Resolve refuses with ENOTDIR, as
// on unix, rather than appending a tail.
func TestResolve_FileComponentIsNotDir(t *testing.T) {
	_, inside, _ := fixture(t)
	file := filepath.Join(inside, "file.txt")
	for _, p := range []string{filepath.Join(file, "x"), filepath.Join(file, "x", "y")} {
		if _, err := Resolve(p); !errors.Is(err, syscall.ENOTDIR) {
			t.Fatalf("Resolve(%q) = %v, want ENOTDIR", p, err)
		}
	}
	// Control: the file itself resolves.
	if _, err := Resolve(file); err != nil {
		t.Fatalf("control: Resolve(file) = %v", err)
	}
}
