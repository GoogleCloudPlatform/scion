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
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestCopyTreeSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "tpl", "plugins", "p")
	writeFiles(t, src, map[string]string{
		"plugin.json":          minimalManifest,
		"skills/x/SKILL.md":    "skill",
		"bin/tool*":            "#!/bin/sh\n",
		"deep/a/b/c/file.txt":  "deep",
		"../../../outside/sec": "SECRET-OUTSIDE",
	})
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Symlink("/etc/passwd", filepath.Join(src, "skills/x/link")))
	must(os.Symlink("../../../outside", filepath.Join(src, "binlink")))
	must(os.Symlink("does-not-exist", filepath.Join(src, "dangling")))
	must(syscall.Mkfifo(filepath.Join(src, "fifo"), 0o644))

	dstDir := t.TempDir()
	rep, err := CopyTree(openRoot(t, src), ".", openRoot(t, dstDir), "out", DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	wantSkipped := []string{"binlink", "dangling", "fifo", "skills/x/link"}
	got := slices.Clone(rep.Skipped)
	slices.Sort(got)
	if !slices.Equal(got, wantSkipped) {
		t.Fatalf("skipped = %v, want %v", got, wantSkipped)
	}
	if rep.Files != 4 {
		t.Errorf("files = %d, want 4", rep.Files)
	}
	out := filepath.Join(dstDir, "out")
	for _, s := range wantSkipped {
		if _, err := os.Lstat(filepath.Join(out, s)); !os.IsNotExist(err) {
			t.Errorf("%s present in destination (err %v)", s, err)
		}
	}
	_ = filepath.Walk(out, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			t.Errorf("symlink copied: %s", p)
		}
		if fi.Mode().IsRegular() {
			b, _ := os.ReadFile(p)
			if strings.Contains(string(b), "SECRET-OUTSIDE") || strings.Contains(string(b), "root:") {
				t.Errorf("target content leaked into %s", p)
			}
		}
		return nil
	})
	if b, _ := os.ReadFile(filepath.Join(out, "deep/a/b/c/file.txt")); string(b) != "deep" {
		t.Errorf("deep file = %q", b)
	}
}

func TestCopyTreeModes(t *testing.T) {
	src := t.TempDir()
	writeFiles(t, src, map[string]string{"bin/exec*": "x", "plain": "y", "setuid": "z", "d/f": "w"})
	if err := os.Chmod(filepath.Join(src, "setuid"), 0o4755|os.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "d"), 0o700); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if _, err := CopyTree(openRoot(t, src), ".", openRoot(t, dst), "o", DefaultLimits); err != nil {
		t.Fatal(err)
	}
	check := func(rel string, want os.FileMode) {
		t.Helper()
		fi, err := os.Lstat(filepath.Join(dst, "o", rel))
		if err != nil {
			t.Fatal(err)
		}
		got := fi.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
		if got != want {
			t.Errorf("%s mode = %v, want %v", rel, got, want)
		}
	}
	check("bin/exec", 0o755)
	check("plain", 0o644)
	check("setuid", 0o755)
	check("d", 0o755)
	check(".", 0o755)
}

func TestCopyTreeLimits(t *testing.T) {
	src := t.TempDir()
	writeFiles(t, src, map[string]string{"a": "12345", "b": "12345", "c/d/e": "1"})
	cases := map[string]Limits{
		"files": {MaxFiles: 2, MaxBytes: 1 << 20, MaxDepth: 10},
		"bytes": {MaxFiles: 100, MaxBytes: 7, MaxDepth: 10},
		"depth": {MaxFiles: 100, MaxBytes: 1 << 20, MaxDepth: 1},
	}
	for name, lim := range cases {
		t.Run(name, func(t *testing.T) {
			dst := t.TempDir()
			_, err := CopyTree(openRoot(t, src), ".", openRoot(t, dst), "o", lim)
			if !IsLimitError(err) {
				t.Fatalf("err = %v, want limit error", err)
			}
			if _, err := os.Lstat(filepath.Join(dst, "o")); !os.IsNotExist(err) {
				t.Fatalf("destination not cleaned up: %v", err)
			}
		})
	}
	if got := (&LimitError{Limit: formatBytes(100 << 20)}).Error(); got != "bundle exceeds 100 MiB" {
		t.Errorf("message = %q", got)
	}
}

func TestCopyTreeDestinationExists(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFiles(t, src, map[string]string{"a": "1"})
	writeFiles(t, dst, map[string]string{"o/keep": "k"})
	if _, err := CopyTree(openRoot(t, src), ".", openRoot(t, dst), "o", DefaultLimits); err == nil {
		t.Fatal("want error")
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "o/keep")); string(b) != "k" {
		t.Fatal("existing destination modified")
	}
}

func TestCopyTreeSourceSymlinkRoot(t *testing.T) {
	base := t.TempDir()
	writeFiles(t, base, map[string]string{"real/a": "1"})
	if err := os.Symlink("real", filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyTree(openRoot(t, base), "link", openRoot(t, t.TempDir()), "o", DefaultLimits); err == nil {
		t.Fatal("want error for symlinked source")
	}
}
