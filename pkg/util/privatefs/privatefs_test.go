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

package privatefs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestForMode(t *testing.T) {
	for m, want := range map[fs.FileMode]Protection{
		0o600: Private, 0o700: Private, 0o400: Private, 0: Private,
		0o640: Inherit, 0o604: Inherit, 0o644: Inherit, 0o755: Inherit,
	} {
		if got := ForMode(m); got != want {
			t.Errorf("ForMode(%04o) = %v, want %v", m, got, want)
		}
	}
}

func openTestTree(t *testing.T) (*Tree, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "anchor")
	tr, err := OpenTree(dir, TreeOptions{CreateAnchor: true})
	if err != nil {
		t.Fatalf("OpenTree: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr, dir
}

func TestOpenTree_MissingAnchorWithoutCreate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	if _, err := OpenTree(dir, TreeOptions{}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("OpenTree of a missing anchor: %v, want fs.ErrNotExist", err)
	}
	// Control: with CreateAnchor the same anchor opens.
	tr, err := OpenTree(dir, TreeOptions{CreateAnchor: true})
	if err != nil {
		t.Fatalf("OpenTree with CreateAnchor: %v", err)
	}
	_ = tr.Close()
}

func TestTree_RejectsInvalidRelativePaths(t *testing.T) {
	tr, dir := openTestTree(t)
	for _, rel := range []string{"", ".", "..", "a/../b", "a//b", "a/", "/abs", filepath.Join(dir, "x")} {
		if _, err := tr.Create(rel, true, Private); err == nil {
			t.Errorf("Create(%q) succeeded", rel)
		}
	}
	// Control: a valid nested name works.
	if err := tr.MkdirAll("a", Private); err != nil {
		t.Fatal(err)
	}
	f, err := tr.Create("a/b", true, Private)
	if err != nil {
		t.Fatalf("Create(a/b): %v", err)
	}
	_ = f.Close()
}

// The portable round trip every caller relies on: create, write, read
// back, replace atomically, list, remove.
func TestTree_RoundTrip(t *testing.T) {
	tr, dir := openTestTree(t)
	if err := tr.MkdirAll("d/e", Private); err != nil {
		t.Fatal(err)
	}
	f, err := tr.Create("d/e/x", true, Private)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("one"); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(f); err != nil {
		t.Fatalf("CheckPrivate of a new Private file: %v", err)
	}
	if owned, err := OwnedByCurrentUser(f); err != nil || !owned {
		t.Fatalf("OwnedByCurrentUser = %v, %v", owned, err)
	}
	if err := CheckNoForeignWriters(f); err != nil {
		t.Fatalf("CheckNoForeignWriters: %v", err)
	}
	_ = f.Close()
	if _, err := tr.Create("d/e/x", true, Private); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("exclusive Create of an existing file: %v, want fs.ErrExist", err)
	}
	got, err := tr.ReadPrivate("d/e/x", 0)
	if err != nil || string(got) != "one" {
		t.Fatalf("ReadPrivate = %q, %v", got, err)
	}
	if got, err := tr.ReadPrivate("d/e/x", 2); err != nil || string(got) != "on" {
		t.Fatalf("ReadPrivate with max 2 = %q, %v", got, err)
	}

	tmp, name, err := tr.CreateTemp("d/e", ".tmp-*", Private)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.WriteString("two"); err != nil {
		t.Fatal(err)
	}
	_ = tmp.Close()
	if err := tr.Rename(name, "d/e/x"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got, _ := tr.ReadPrivate("d/e/x", 0); string(got) != "two" {
		t.Fatalf("after Rename: %q", got)
	}

	if err := tr.WriteFile("d/e/y", []byte("three"), Private); err != nil {
		t.Fatal(err)
	}
	if got, _ := tr.ReadPrivate("d/e/y", 0); string(got) != "three" {
		t.Fatalf("after WriteFile: %q", got)
	}

	d, err := tr.OpenDir("d/e")
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(d); err != nil {
		t.Fatalf("CheckPrivate of a Private directory: %v", err)
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil || len(entries) != 2 {
		t.Fatalf("ReadDir = %v, %v; want x and y", entries, err)
	}

	for _, rel := range []string{"d/e/x", "d/e/y"} {
		if err := tr.Remove(rel); err != nil {
			t.Fatalf("Remove(%s): %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "d", "e", "x")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("x after Remove: %v", err)
	}
	if err := tr.Remove("d/e/x"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Remove of a missing file: %v, want fs.ErrNotExist", err)
	}
}

func TestOpenPrivate_RejectsCreateFlags(t *testing.T) {
	tr, _ := openTestTree(t)
	if err := tr.WriteFile("x", []byte("a"), Private); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []int{os.O_RDWR | os.O_CREATE, os.O_WRONLY | os.O_APPEND, os.O_RDWR | os.O_EXCL, os.O_WRONLY | os.O_RDWR} {
		if f, err := tr.OpenPrivate("x", flag); err == nil {
			_ = f.Close()
			t.Errorf("OpenPrivate with flag %#x succeeded", flag)
		}
	}
	f, err := tr.OpenPrivate("x", os.O_RDWR|os.O_TRUNC)
	if err != nil {
		t.Fatalf("OpenPrivate(O_RDWR|O_TRUNC): %v", err)
	}
	fi, _ := f.Stat()
	_ = f.Close()
	if fi.Size() != 0 {
		t.Fatalf("size after O_TRUNC = %d", fi.Size())
	}
}
