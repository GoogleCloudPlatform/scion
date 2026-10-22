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

package syncarchive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ent describes one crafted archive entry.
type ent struct {
	name string
	typ  byte
	mode int64
	body string
	link string
	size int64 // header size override for sparse/limit tests; 0 = len(body)
}

func file(name, body string) ent { return ent{name: name, typ: tar.TypeReg, mode: 0o644, body: body} }
func dir(name string) ent        { return ent{name: name, typ: tar.TypeDir, mode: 0o755} }
func symlink(name, target string) ent {
	return ent{name: name, typ: tar.TypeSymlink, mode: 0o777, link: target}
}
func hardlink(name, target string) ent { return ent{name: name, typ: tar.TypeLink, link: target} }

func buildTar(t *testing.T, ents ...ent) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range ents {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typ,
			Mode:     e.mode,
			Linkname: e.link,
			ModTime:  time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC),
			Format:   tar.FormatPAX,
		}
		if e.typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("header %q: %v", e.name, err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// layout is a scratch area: base/dest is the extraction target (with a
// pre-existing .scion), base/outside a sibling that must never change.
type layout struct {
	base, dest, outside string
}

func newLayout(t *testing.T) layout {
	t.Helper()
	base := t.TempDir()
	l := layout{base: base, dest: filepath.Join(base, "dest"), outside: filepath.Join(base, "outside")}
	for p, body := range map[string]string{
		filepath.Join(l.dest, ".scion", "settings.yaml"):               "original",
		filepath.Join(l.dest, ".scion", "agents", "mine", "prompt.md"): "mine",
		filepath.Join(l.outside, "victim.txt"):                         "untouched",
	} {
		mustWrite(t, p, body)
	}
	return l
}

func mustWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// snapshot records every path under root with its type, mode, link target
// and content.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			out[rel] = "link:" + target
		case fi.IsDir():
			out[rel] = fmt.Sprintf("dir:%o", fi.Mode().Perm())
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[rel] = fmt.Sprintf("file:%o:%s", fi.Mode().Perm(), b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func diffSnap(a, b map[string]string) []string {
	var d []string
	for k, v := range a {
		if b[k] != v {
			d = append(d, fmt.Sprintf("%s: %q -> %q", k, v, b[k]))
		}
	}
	for k, v := range b {
		if _, ok := a[k]; !ok {
			d = append(d, fmt.Sprintf("%s: added %q", k, v))
		}
	}
	sort.Strings(d)
	return d
}

// guard snapshots everything that must not change (the sibling directory
// and the destination's .scion) and checks it after the test body.
func (l layout) guard(t *testing.T) func() {
	t.Helper()
	outside := snapshot(t, l.outside)
	scion := snapshot(t, filepath.Join(l.dest, ".scion"))
	baseEntries := dirNames(t, l.base)
	return func() {
		t.Helper()
		if d := diffSnap(outside, snapshot(t, l.outside)); len(d) > 0 {
			t.Errorf("outside the destination changed: %v", d)
		}
		if d := diffSnap(scion, snapshot(t, filepath.Join(l.dest, ".scion"))); len(d) > 0 {
			t.Errorf(".scion changed: %v", d)
		}
		if got := dirNames(t, l.base); strings.Join(got, ",") != strings.Join(baseEntries, ",") {
			t.Errorf("entries next to the destination changed: %v -> %v", baseEntries, got)
		}
		if leftovers := findTemp(t, l.dest); len(leftovers) > 0 {
			t.Errorf("staging files left behind: %v", leftovers)
		}
	}
}

func dirNames(t *testing.T, p string) []string {
	t.Helper()
	des, err := os.ReadDir(p)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, de := range des {
		names = append(names, de.Name())
	}
	return names
}

func findTemp(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), ".sync-extract-") {
			found = append(found, p)
		}
		return nil
	})
	return found
}

func extract(t *testing.T, dest string, opts Options, ents ...ent) (Result, error) {
	t.Helper()
	return ExtractGzip(bytes.NewReader(gz(t, buildTar(t, ents...))), dest, opts)
}

// Archive names that are absolute or climb with ".." refuse the archive
// and write nothing outside the destination.
func TestExtract_RefusesUnsafeNames(t *testing.T) {
	for _, name := range []string{
		"/abs.txt",
		"../outside/victim.txt",
		"./../outside/victim.txt",
		"a/../../outside/victim.txt",
		"a/../b.txt",
		"sub/..",
		"..",
	} {
		t.Run(name, func(t *testing.T) {
			l := newLayout(t)
			check := l.guard(t)
			_, err := extract(t, l.dest, Options{}, file(name, "evil"))
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("err = %v, want ErrRefused", err)
			}
			check()
		})
	}
}

// Entries under .scion are skipped at any depth and in any spelling the
// host file system could treat as ".scion".
func TestExtract_SkipsProtectedDir(t *testing.T) {
	l := newLayout(t)
	check := l.guard(t)
	res, err := extract(t, l.dest, Options{},
		dir("./.scion/"),
		file("./.scion/settings.yaml", "evil"),
		file("./.scion/agents/mine/prompt.md", "evil"),
		file("./.SCION/settings.yaml", "evil"),
		file("./.Scion/new.yaml", "evil"),
		file("./.scion./settings.yaml", "evil"),
		file("./.scion /settings.yaml", "evil"),
		file("./.sc\u200bion/settings.yaml", "evil"),
		file("./.\u017fcion/settings.yaml", "evil"), // long s folds to s
		file("./sub/.scion/settings.yaml", "evil"),
		file("./keep.txt", "ok"),
	)
	if err != nil {
		t.Fatal(err)
	}
	check()
	if res.Protected != 10 || res.Files != 1 {
		t.Errorf("result = %+v, want 10 protected, 1 file", res)
	}
	for _, p := range []string{".SCION", ".Scion", ".scion.", ".scion ", ".sc\u200bion", ".\u017fcion", "sub/.scion"} {
		if _, err := os.Lstat(filepath.Join(l.dest, p)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s was created (err %v)", p, err)
		}
	}
}

// Symbolic links whose targets leave the destination or name .scion are
// skipped; in-tree relative links are written as they are.
func TestExtract_Symlinks(t *testing.T) {
	l := newLayout(t)
	check := l.guard(t)
	res, err := extract(t, l.dest, Options{},
		dir("./sub/"),
		file("./AGENTS.md", "agents"),
		symlink("./claude.md", "AGENTS.md"),
		symlink("./sub/up.md", "../AGENTS.md"),
		symlink("./sub/self", "."),
		symlink("./top", "sub/.."),
		symlink("./abs", "/etc/passwd"),
		symlink("./esc", "../outside/victim.txt"),
		symlink("./sub/esc", "../../outside"),
		symlink("./cfg", ".scion"),
		symlink("./cfg2", ".SCION/settings.yaml"),
		symlink("./sub/cfg", "../.scion"),
		symlink("./sub/cfg3", "self/../../.scion"),
		symlink("./empty", ""),
	)
	if err != nil {
		t.Fatal(err)
	}
	check()
	if res.Symlinks != 3 || res.UnsafeCount != 9 {
		t.Errorf("result = %+v, want 3 symlinks and 9 unsafe", res)
	}
	for p, want := range map[string]string{"claude.md": "AGENTS.md", "sub/up.md": "../AGENTS.md", "sub/self": "."} {
		if got, err := os.Readlink(filepath.Join(l.dest, p)); err != nil || got != want {
			t.Errorf("%s -> %q (%v), want %q", p, got, err, want)
		}
	}
	for _, p := range []string{"top", "abs", "esc", "sub/esc", "cfg", "cfg2", "sub/cfg", "sub/cfg3", "empty"} {
		if _, err := os.Lstat(filepath.Join(l.dest, p)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s was created (err %v)", p, err)
		}
	}
}

// No entry is written through a symbolic link, whether the link came from
// the archive or was already in the destination.
func TestExtract_NeverWritesThroughLinks(t *testing.T) {
	l := newLayout(t)
	// Pre-existing links in the destination: to .scion, and out of the tree.
	if err := os.Symlink(".scion", filepath.Join(l.dest, "cfglink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(l.outside, filepath.Join(l.dest, "outlink")); err != nil {
		t.Fatal(err)
	}
	check := l.guard(t)
	res, err := extract(t, l.dest, Options{},
		dir("./real/"),
		symlink("./inlink", "real"),
		file("./inlink/f.txt", "through archive link"),
		file("./cfglink/settings.yaml", "evil"),
		file("./outlink/victim.txt", "evil"),
		file("./outlink/new.txt", "evil"),
		symlink("./outlink/l", "x"),
		hardlink("./cfglink/h", "real/f"),
	)
	if err != nil {
		t.Fatal(err)
	}
	check()
	if res.UnsafeCount != 6 {
		t.Errorf("result = %+v, want 6 unsafe", res)
	}
	if _, err := os.Lstat(filepath.Join(l.dest, "real", "f.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("file written through archive link (err %v)", err)
	}
}

// A file or link entry replaces a link already at its path rather than
// writing through it, and a directory entry replaces a link to a directory.
func TestExtract_ReplacesLinksAtLeaf(t *testing.T) {
	l := newLayout(t)
	if err := os.Symlink(filepath.Join(l.outside, "victim.txt"), filepath.Join(l.dest, "victim")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".scion", filepath.Join(l.dest, "d")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(l.outside, filepath.Join(l.dest, "d2")); err != nil {
		t.Fatal(err)
	}
	check := l.guard(t)
	_, err := extract(t, l.dest, Options{},
		file("./victim", "replaced"),
		dir("./d/"),
		file("./d/settings.yaml", "in a new dir"),
		dir("./d2/"),
		file("./d2/victim.txt", "in a new dir"),
	)
	if err != nil {
		t.Fatal(err)
	}
	check()
	snap := snapshot(t, l.dest)
	for p, want := range map[string]string{
		"victim":               "file:644:replaced",
		"d":                    "dir:755",
		"d/settings.yaml":      "file:644:in a new dir",
		"d2/victim.txt":        "file:644:in a new dir",
		".scion/settings.yaml": "file:644:original",
	} {
		if snap[p] != want {
			t.Errorf("%s = %q, want %q", p, snap[p], want)
		}
	}
}

// Hard links are written only to regular files inside the destination
// that are not skipped.
func TestExtract_Hardlinks(t *testing.T) {
	l := newLayout(t)
	check := l.guard(t)
	res, err := extract(t, l.dest, Options{},
		file("./a.txt", "a"),
		hardlink("./b.txt", "./a.txt"),
		symlink("./s", "a.txt"),
		hardlink("./abs", "/etc/passwd"),
		hardlink("./esc", "../outside/victim.txt"),
		hardlink("./cfg", "./.scion/settings.yaml"),
		hardlink("./cfg2", ".SCION/settings.yaml"),
		hardlink("./vias", "./s"),
		hardlink("./missing", "./nope"),
	)
	if err != nil {
		t.Fatal(err)
	}
	check()
	if res.Hardlinks != 1 || res.UnsafeCount != 6 {
		t.Errorf("result = %+v, want 1 hardlink and 6 unsafe", res)
	}
	a, _ := os.Stat(filepath.Join(l.dest, "a.txt"))
	b, err := os.Stat(filepath.Join(l.dest, "b.txt"))
	if err != nil || !os.SameFile(a, b) {
		t.Errorf("b.txt is not a hard link of a.txt (err %v)", err)
	}
}

// Device nodes, FIFOs and unknown entry types are skipped.
func TestExtract_SkipsSpecialFiles(t *testing.T) {
	l := newLayout(t)
	check := l.guard(t)
	res, err := extract(t, l.dest, Options{},
		ent{name: "./null", typ: tar.TypeChar, mode: 0o666},
		ent{name: "./disk", typ: tar.TypeBlock, mode: 0o660},
		ent{name: "./fifo", typ: tar.TypeFifo, mode: 0o644},
		ent{name: "./cont", typ: tar.TypeCont, mode: 0o644},
		file("./ok", "ok"),
	)
	if err != nil {
		t.Fatal(err)
	}
	check()
	if res.UnsafeCount != 4 || res.Files != 1 {
		t.Errorf("result = %+v, want 4 unsafe, 1 file", res)
	}
	for _, p := range []string{"null", "disk", "fifo", "cont"} {
		if _, err := os.Lstat(filepath.Join(l.dest, p)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s was created (err %v)", p, err)
		}
	}
}

// The entry count and content size are bounded.
func TestExtract_Limits(t *testing.T) {
	t.Run("entries", func(t *testing.T) {
		l := newLayout(t)
		_, err := extract(t, l.dest, Options{MaxEntries: 3},
			file("./1", "x"), file("./2", "x"), file("./3", "x"), file("./4", "x"))
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "entries") {
			t.Fatalf("err = %v, want entry limit", err)
		}
		if _, err := os.Lstat(filepath.Join(l.dest, "4")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("entry past the limit written (err %v)", err)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		l := newLayout(t)
		_, err := extract(t, l.dest, Options{MaxBytes: 10},
			file("./1", "12345"), file("./2", "123456"))
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "bytes") {
			t.Fatalf("err = %v, want size limit", err)
		}
		if _, err := os.Lstat(filepath.Join(l.dest, "2")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("entry past the limit written (err %v)", err)
		}
	})
	t.Run("skipped entries count", func(t *testing.T) {
		l := newLayout(t)
		_, err := extract(t, l.dest, Options{MaxBytes: 10},
			file("./.scion/big", "12345678901"))
		if !errors.Is(err, ErrRefused) {
			t.Fatalf("err = %v, want size limit", err)
		}
	})
}

// A stream that ends inside a file leaves no partial file and no staging
// file behind.
func TestExtract_TruncatedStreamLeavesNoPartialFile(t *testing.T) {
	l := newLayout(t)
	check := l.guard(t)
	raw := buildTar(t, file("./done.txt", "complete"), file("./big.txt", strings.Repeat("x", 4096)))
	// Cut inside big.txt's content.
	cut := bytes.Index(raw, []byte("xxxx")) + 100
	_, err := Extract(bytes.NewReader(raw[:cut]), l.dest, Options{})
	if err == nil {
		t.Fatal("truncated archive extracted without error")
	}
	check()
	if _, err := os.Lstat(filepath.Join(l.dest, "big.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("partial file left (err %v)", err)
	}
	if b, err := os.ReadFile(filepath.Join(l.dest, "done.txt")); err != nil || string(b) != "complete" {
		t.Errorf("done.txt = %q, %v", b, err)
	}
}

// Options.Excludes skips the named paths and everything below them.
func TestExtract_Excludes(t *testing.T) {
	l := newLayout(t)
	res, err := extract(t, l.dest, Options{Excludes: []string{"build/out", "./tmp"}},
		file("./build/out/a", "x"), file("./build/outer", "kept"), file("./tmp", "x"), file("./tmp2", "kept"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Protected != 2 || res.Files != 2 {
		t.Errorf("result = %+v, want 2 protected, 2 files", res)
	}
}

// Permission bits are kept (umask applied, no setuid/setgid/sticky) and
// modification times are not restored, as "tar -xz -m" did as a non-root
// user.
func TestExtract_ModesAndTimes(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	l := newLayout(t)
	start := time.Now().Add(-time.Minute)
	_, err := extract(t, l.dest, Options{},
		ent{name: "./bin/", typ: tar.TypeDir, mode: 0o750},
		ent{name: "./bin/run", typ: tar.TypeReg, mode: 0o4777, body: "#!/bin/sh"},
		ent{name: "./ro.txt", typ: tar.TypeReg, mode: 0o400, body: "ro"},
		ent{name: "./locked/", typ: tar.TypeDir, mode: 0o500},
		ent{name: "./locked/f", typ: tar.TypeReg, mode: 0o644, body: "f"},
	)
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]fs.FileMode{"bin": 0o750, "bin/run": 0o755, "ro.txt": 0o400, "locked": 0o700, "locked/f": 0o644} {
		fi, err := os.Lstat(filepath.Join(l.dest, p))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want || fi.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
			t.Errorf("%s mode = %v, want %v", p, fi.Mode(), want)
		}
		if fi.ModTime().Before(start) {
			t.Errorf("%s mtime %v restored from the archive", p, fi.ModTime())
		}
	}
}

// IsProtectedName matches spellings of .scion and nothing else.
func TestIsProtectedName(t *testing.T) {
	for _, c := range []string{".scion", ".SCION", ".sCiOn", ".scion.", ".scion. .", ".sc\u200dion", "\ufeff.scion", ".\u017fcion"} {
		if !IsProtectedName(c) {
			t.Errorf("IsProtectedName(%q) = false", c)
		}
	}
	for _, c := range []string{"scion", ".scion2", "..scion", ".scio", ".scion-old", "x.scion"} {
		if IsProtectedName(c) {
			t.Errorf("IsProtectedName(%q) = true", c)
		}
	}
}

// An ordinary tree made by "tar -cz -C dir ." extracts exactly as the
// previous "tar -xz -m" extraction did, apart from .scion, which is now
// left alone.
func TestExtract_RegularTreeMatchesTar(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)

	src := t.TempDir()
	for p, body := range map[string]string{
		"AGENTS.md":               "agents",
		"README.md":               "readme",
		"pkg/a/a.go":              "package a",
		"pkg/a/deep/er/x.txt":     "deep",
		"docs/guide.md":           "guide",
		"name with spaces.txt":    "spaces",
		"colon:name.txt":          "colon",
		"back\\slash.txt":         "backslash",
		".hidden/conf":            "hidden",
		".scion/settings.yaml":    "agent copy",
		".scion/agents/x/home/.b": "agent copy",
	} {
		mustWrite(t, filepath.Join(src, p), body)
	}
	if err := os.Chmod(filepath.Join(src, "pkg/a/a.go"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "docs"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "empty", "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{"claude.md": "AGENTS.md", "docs/agents.md": "../AGENTS.md", "pkg/here": "a"} {
		if err := os.Symlink(target, filepath.Join(src, link)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(src, "README.md"), filepath.Join(src, "docs", "README.md")); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "ws.tgz")
	if out, err := exec.Command("tar", "-czf", archive, "-C", src, ".").CombinedOutput(); err != nil {
		t.Fatalf("tar create: %v: %s", err, out)
	}

	byTar := t.TempDir()
	if out, err := exec.Command("tar", "-xz", "-m", "-f", archive, "-C", byTar).CombinedOutput(); err != nil {
		t.Fatalf("tar extract: %v: %s", err, out)
	}
	if err := os.RemoveAll(filepath.Join(byTar, ".scion")); err != nil {
		t.Fatal(err)
	}

	byGo := t.TempDir()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	res, err := ExtractGzip(f, byGo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.UnsafeCount != 0 || res.Protected == 0 {
		t.Errorf("result = %+v, want no unsafe skips and the .scion entries protected", res)
	}
	if d := diffSnap(snapshot(t, byTar), snapshot(t, byGo)); len(d) > 0 {
		t.Errorf("extraction differs from tar:\n%s", strings.Join(d, "\n"))
	}
	a, _ := os.Stat(filepath.Join(byGo, "README.md"))
	b, _ := os.Stat(filepath.Join(byGo, "docs", "README.md"))
	if !os.SameFile(a, b) {
		t.Error("hard link not kept")
	}
}

// Writing into a destination that already holds the tree updates it in
// place, as tar did.
func TestExtract_OverwritesExistingTree(t *testing.T) {
	l := newLayout(t)
	mustWrite(t, filepath.Join(l.dest, "a.txt"), "old")
	mustWrite(t, filepath.Join(l.dest, "untouched.txt"), "keep")
	check := l.guard(t)
	if _, err := extract(t, l.dest, Options{}, file("./a.txt", "new"), symlink("./untouched2", "untouched.txt")); err != nil {
		t.Fatal(err)
	}
	check()
	snap := snapshot(t, l.dest)
	if snap["a.txt"] != "file:644:new" || snap["untouched.txt"] != "file:644:keep" || snap["untouched2"] != "link:untouched.txt" {
		t.Errorf("tree = %v", snap)
	}
}

// A directory in the way of a file is an error, not a silent change.
func TestExtract_FileOverDirectoryFails(t *testing.T) {
	l := newLayout(t)
	if err := os.MkdirAll(filepath.Join(l.dest, "d", "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := extract(t, l.dest, Options{}, file("./d", "x"))
	if err == nil || errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want a write error", err)
	}
	if fi, err := os.Lstat(filepath.Join(l.dest, "d")); err != nil || !fi.IsDir() {
		t.Errorf("directory replaced (err %v)", err)
	}
	if leftovers := findTemp(t, l.dest); len(leftovers) > 0 {
		t.Errorf("staging files left behind: %v", leftovers)
	}
}

// Corrupt input is an error.
func TestExtractGzip_NotGzip(t *testing.T) {
	if _, err := ExtractGzip(strings.NewReader("not gzip"), t.TempDir(), Options{}); err == nil {
		t.Fatal("want error")
	}
}
