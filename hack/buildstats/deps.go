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

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

// DepCount is the transitive dependency count of one package, as
// `go list -deps` sees it. The package itself is not counted, so Total
// equals `go list -f '{{len .Deps}}' PKG`.
type DepCount struct {
	Package string `json:"package"`
	Test    bool   `json:"test"`
	Total   int    `json:"total"`
	NonStd  int    `json:"non_std"`
}

// depsListFields are the go list -json fields parseGoList needs. JSON is used
// rather than -f because test-variant entries ("p [p.test]") contain spaces.
const depsListFields = "ImportPath,Standard,DepOnly,Deps"

// goListPkg is one go list -json object.
type goListPkg struct {
	ImportPath string
	Standard   bool
	DepOnly    bool
	Deps       []string
}

// depsArgs returns the go list command for pkgs.
func depsArgs(pkgs []string, test bool) []string {
	args := []string{"go", "list", "-deps", "-json=" + depsListFields}
	if test {
		args = append(args, "-test")
	}
	return append(args, pkgs...)
}

// stripTestVariant maps "p [q.test]" to "p".
func stripTestVariant(path string) string {
	if i := strings.Index(path, " ["); i >= 0 {
		return path[:i]
	}
	return path
}

// parseGoList computes per-root counts from go list -deps -json output. Roots are
// the entries with DepOnly=false. Without -test each root's Deps is already
// the transitive closure. With -test the roots are p, "p [p.test]",
// "p_test [p.test]" and the generated "p.test"; the test closure of p is the
// Deps of "p.test" with variant suffixes stripped and p itself removed. A
// package without test files has no "p.test", so its build closure is used.
func parseGoList(r io.Reader, test bool) ([]DepCount, error) {
	type entry struct {
		deps    []string
		depOnly bool
	}
	std := map[string]bool{}
	entries := map[string]*entry{}
	var roots []string
	dec := json.NewDecoder(r)
	for {
		var p goListPkg
		err := dec.Decode(&p)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode go list output: %w", err)
		}
		if p.ImportPath == "" {
			return nil, errors.New("go list entry without ImportPath")
		}
		entries[p.ImportPath] = &entry{deps: p.Deps, depOnly: p.DepOnly}
		if p.Standard {
			std[stripTestVariant(p.ImportPath)] = true
		}
		if isRoot(p.ImportPath, p.DepOnly, test) {
			roots = append(roots, p.ImportPath)
		}
	}
	sort.Strings(roots)

	var out []DepCount
	for _, root := range roots {
		deps := entries[root].deps
		if test {
			if tm, ok := entries[root+".test"]; ok {
				deps = tm.deps
			}
		}
		seen := map[string]bool{}
		for _, d := range deps {
			d = stripTestVariant(d)
			if d == root || (test && strings.HasSuffix(d, ".test")) {
				continue
			}
			seen[d] = true
		}
		c := DepCount{Package: root, Test: test, Total: len(seen)}
		for d := range seen {
			if !std[d] {
				c.NonStd++
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// isRoot reports whether a go list entry is one of the packages named on the
// command line. With -test, go list also reports the test variants
// ("p [p.test]") and the generated test main ("p.test") as non-DepOnly.
func isRoot(path string, depOnly, test bool) bool {
	if depOnly {
		return false
	}
	if test && (strings.Contains(path, " [") || strings.HasSuffix(path, ".test")) {
		return false
	}
	return true
}

func printDeps(p *printer, deps []DepCount) {
	p.println("\ngo list -deps (package itself excluded):")
	t, done := p.table(tabwriter.AlignRight)
	t.println("total\tnon-std\t mode\t package\t")
	for _, d := range deps {
		mode := "build"
		if d.Test {
			mode = "test"
		}
		t.printf("%d\t%d\t %s\t %s\t\n", d.Total, d.NonStd, mode, d.Package)
	}
	done()
}
