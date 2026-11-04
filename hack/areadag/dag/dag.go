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

// Package dag reads hack/areadag/dag.txt, the declaration of the pkg/hub
// package DAG that hack/areadag checks. It is a separate, standard-library-only
// package so that every reader of the file (the areadag check, and tests or
// tools that need the list of declared pkg/hub directories) parses it the same
// way.
//
// File format: one declaration per line, whitespace-separated columns; a #
// starts a comment that runs to the end of the line; blank lines are skipped.
//
//	<kind> <package> [<area>...]
//
// kind is one of:
//
//	leaf         may import other leaves, and only the areas listed on its line;
//	             a listed area must itself import no pkg/hub package in non-test
//	             code (checked by areadag, not here);
//	area         may import leaves, and only the areas listed on its line;
//	testsupport  non-test code that only _test.go files may import (C2); may
//	             import leaves, other testsupport packages, and the listed areas.
//
// package is the directory relative to pkg/hub (for example internal/principal).
// The optional trailing columns name the declared areas the package may import
// beyond leaves (and, for any kind, testsupport packages in its tests). Leaves
// are always allowed, so they are never listed.
package dag

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
)

// Kind is a declared package kind.
type Kind string

// The declared package kinds.
const (
	Leaf        Kind = "leaf"
	Area        Kind = "area"
	TestSupport Kind = "testsupport"
)

// Entry is one declaration line.
type Entry struct {
	Kind Kind
	// Rel is the package directory relative to pkg/hub, slash-separated.
	Rel string
	// Areas are the declared areas this package may import, relative to pkg/hub.
	Areas []string
	// Line is the 1-based line number in the file, for messages.
	Line int
}

// Dag is a parsed declaration file.
type Dag struct {
	// Entries are in file order.
	Entries []Entry
	byRel   map[string]int
}

// Lookup returns the entry declaring rel (relative to pkg/hub).
func (d *Dag) Lookup(rel string) (Entry, bool) {
	i, ok := d.byRel[rel]
	if !ok {
		return Entry{}, false
	}
	return d.Entries[i], true
}

// Rels returns the declared package directories of the given kinds, relative
// to pkg/hub, sorted. No kinds means every kind.
func (d *Dag) Rels(kinds ...Kind) []string {
	var out []string
	for _, e := range d.Entries {
		if len(kinds) == 0 || containsKind(kinds, e.Kind) {
			out = append(out, e.Rel)
		}
	}
	sort.Strings(out)
	return out
}

// Count returns the number of entries of kind k.
func (d *Dag) Count(k Kind) int {
	n := 0
	for _, e := range d.Entries {
		if e.Kind == k {
			n++
		}
	}
	return n
}

func containsKind(kinds []Kind, k Kind) bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}

// ReadFile parses the declaration file at path.
func ReadFile(file string) (*Dag, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	d, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return d, nil
}

// Parse parses a declaration file. It rejects an unknown kind, a malformed or
// duplicate package, and a listed name that is not a declared area (including
// the package itself). It does not consult the module: whether each package
// exists, and what it imports, is areadag's job.
func Parse(r io.Reader) (*Dag, error) {
	d := &Dag{byRel: map[string]int{}}
	sc := bufio.NewScanner(r)
	n := 0
	for sc.Scan() {
		n++
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 2 {
			return nil, fmt.Errorf("line %d: want \"<kind> <package> [<area>...]\", got %q", n, strings.TrimSpace(line))
		}
		e := Entry{Kind: Kind(fields[0]), Rel: fields[1], Areas: fields[2:], Line: n}
		switch e.Kind {
		case Leaf, Area, TestSupport:
		default:
			return nil, fmt.Errorf("line %d: unknown kind %q (want leaf, area or testsupport)", n, fields[0])
		}
		for _, rel := range append([]string{e.Rel}, e.Areas...) {
			if err := checkRel(rel); err != nil {
				return nil, fmt.Errorf("line %d: %w", n, err)
			}
		}
		if prev, dup := d.byRel[e.Rel]; dup {
			return nil, fmt.Errorf("line %d: %s is already declared on line %d", n, e.Rel, d.Entries[prev].Line)
		}
		d.byRel[e.Rel] = len(d.Entries)
		d.Entries = append(d.Entries, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for _, e := range d.Entries {
		seen := map[string]bool{}
		for _, a := range e.Areas {
			if a == e.Rel {
				return nil, fmt.Errorf("line %d: %s lists itself", e.Line, e.Rel)
			}
			if seen[a] {
				return nil, fmt.Errorf("line %d: %s lists %s twice", e.Line, e.Rel, a)
			}
			seen[a] = true
			t, ok := d.Lookup(a)
			if !ok {
				return nil, fmt.Errorf("line %d: %s lists %s, which is not declared", e.Line, e.Rel, a)
			}
			if t.Kind != Area {
				return nil, fmt.Errorf("line %d: %s lists %s, which is a %s; only areas are listed (leaves are always allowed, and testsupport packages are allowed in tests)", e.Line, e.Rel, a, t.Kind)
			}
		}
	}
	return d, nil
}

// checkRel rejects a package path that is not a clean, relative path below
// pkg/hub.
func checkRel(rel string) error {
	bad := rel == "" || path.Clean(rel) != rel || strings.HasPrefix(rel, "/")
	for _, seg := range strings.Split(rel, "/") {
		if seg == "." || seg == ".." || !validSegment(seg) {
			bad = true
		}
	}
	if bad {
		return fmt.Errorf("package %q must be a clean slash-separated path relative to pkg/hub, segments of [A-Za-z0-9_.-] (pkg/hub itself cannot be declared)", rel)
	}
	return nil
}

func validSegment(seg string) bool {
	if seg == "" {
		return false
	}
	for _, r := range seg {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
		default:
			return false
		}
	}
	return true
}
