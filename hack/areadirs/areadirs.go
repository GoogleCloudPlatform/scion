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

// Package areadirs reads hack/area-dirs.txt, the list of pkg/hub area
// directories that the source-scanning guards must cover. Every Go test that
// needs the list reads it through Load, so the list's location and format
// can change in one place.
package areadirs

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Kind classifies an entry.
type Kind string

const (
	// Area is a feature area moved out of package hub.
	Area Kind = "area"
	// TestSupport is a non-test package that only tests may import.
	TestSupport Kind = "testsupport"
)

// Entry is one listed directory.
type Entry struct {
	Kind Kind
	// Dir is the directory as a clean slash path relative to pkg/hub.
	Dir string
}

// File is the list's path relative to the module root.
const File = "hack/area-dirs.txt"

// Load returns the entries of the list in the module rooted at moduleRoot,
// in file order. It fails on a malformed or empty list.
func Load(moduleRoot string) ([]Entry, error) {
	return ReadFile(filepath.Join(moduleRoot, filepath.FromSlash(File)))
}

// ReadFile parses the list at path.
func ReadFile(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only; a close error cannot lose data
	entries, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return entries, nil
}

// Parse reads entries in the list format: one "<kind> <dir>" per line,
// blank lines and #-comment lines ignored. Unknown kinds, extra fields,
// directories that are not clean relative slash paths, duplicates and an
// empty list are errors.
func Parse(r io.Reader) ([]Entry, error) {
	var out []Entry
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("line %d: want \"<kind> <dir>\", got %q", n, line)
		}
		kind, dir := Kind(fields[0]), fields[1]
		if kind != Area && kind != TestSupport {
			return nil, fmt.Errorf("line %d: unknown kind %q (want %q or %q)", n, kind, Area, TestSupport)
		}
		if !cleanRelDir(dir) {
			return nil, fmt.Errorf("line %d: directory %q is not a clean slash path inside pkg/hub", n, dir)
		}
		if seen[dir] {
			return nil, fmt.Errorf("line %d: directory %q listed twice", n, dir)
		}
		seen[dir] = true
		out = append(out, Entry{Kind: kind, Dir: dir})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no entries")
	}
	return out, nil
}

// segment is one path element of a listed directory. The shell reader
// (hack/lib/area-dirs.sh) applies the same rule.
var segment = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// cleanRelDir reports whether dir is a slash path of one or more elements,
// none empty, "." or "..", each made of letters, digits, '_', '.' or '-'.
func cleanRelDir(dir string) bool {
	for _, s := range strings.Split(dir, "/") {
		if s == "." || s == ".." || !segment.MatchString(s) {
			return false
		}
	}
	return true
}

// Dirs returns the directories of the entries, in order.
func Dirs(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Dir
	}
	return out
}
