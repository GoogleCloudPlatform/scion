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

// Command areadag checks the pkg/hub package DAG declared in
// hack/areadag/dag.txt (format: package hack/areadag/dag) against the import
// metadata of the module (`hack/check-areadag.sh`).
//
// Input: the JSON stream of `go list -deps=false -json ./...` run at the
// module root (metadata only: no -export, no -compiled, so nothing compiles).
// The pkg/hub packages are the subset whose import path is pkg/hub or below it.
//
// Imports of a package are its go list Imports (non-test) and TestImports plus
// XTestImports (test), plus the imports of its IgnoredGoFiles, parsed here and
// classed by the _test.go suffix. The ignored files are the ones another build
// tag set or platform compiles (integration, no_sqlite, windows, ...), so the
// rules hold for every tag set, not only the default build.
//
// Rules:
//
//  1. A declared package (leaf, area or testsupport) imports, in non-test or
//     test code, only declared leaves plus the areas listed on its line.
//     A testsupport package may also import other testsupport packages, and
//     any declared package may import testsupport packages from its tests
//     (rule 4 owns testsupport imports). Any other pkg/hub subpackage is a
//     violation, declared or not.
//  2. No declared package imports pkg/hub itself.
//  3. An area listed on a leaf's line imports no pkg/hub package in non-test
//     code, so a leaf can never reach the rest of pkg/hub through it.
//  4. (C2) Over EVERY package of the module, a testsupport package appears only
//     in test imports, never in non-test imports. The only exception is a
//     testsupport importer: it is itself only reachable from tests. The
//     testsupport class comes only from dag.txt; no name or directory
//     convention makes a package testsupport.
//  5. Every pkg/hub subpackage imported by an alias file (zz_alias_*.go,
//     test or not) in the pkg/hub directory is declared, so a package move
//     cannot skip the DAG. pkgmove and hubshard extract both write alias files
//     under that name; the name, not the header marker, is the key.
//
// Undeclared pkg/hub subpackages are not checked under rules 1-3 (feature
// packages carry no DAG obligation); they are still subject to rule 4, and
// importing one from a declared package violates rule 1.
//
// Every declared package must exist. The run fails as unusable (exit 2), not
// clean, when any input count is zero: no module packages, no pkg/hub
// packages, no declarations, no testsupport declarations, no alias files, or
// no import edges examined.
//
// Usage:
//
//	areadag -dag hack/areadag/dag.txt -list FILE [-hub IMPORTPATH]
//
// Exit codes: 0 clean, 1 violations, 2 usage error, unreadable or malformed
// input, or an input count of zero.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/hack/areadag/dag"
)

const defaultHub = "github.com/GoogleCloudPlatform/scion/pkg/hub"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// listPkg holds the go list -json fields areadag reads.
type listPkg struct {
	ImportPath     string
	Dir            string
	GoFiles        []string
	CgoFiles       []string
	IgnoredGoFiles []string
	Imports        []string
	TestImports    []string
	XTestImports   []string
	Error          *struct{ Err string }
}

// pkgImports is one package's non-test and test import sets, keyed by import
// path. The importing file is looked up only for a rule 4 violation message.
type pkgImports struct {
	path    string
	dir     string
	nonTest map[string]bool
	test    map[string]bool
	// srcFiles are the non-test files to search when naming a violating file.
	srcFiles []string
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("areadag", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dagFile := flags.String("dag", "", "the DAG declaration file (hack/areadag/dag.txt)")
	listFile := flags.String("list", "", "output of `go list -deps=false -json ./...` at the module root")
	hub := flags.String("hub", defaultHub, "import path of pkg/hub")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *dagFile == "" || *listFile == "" || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage: areadag -dag FILE -list FILE [-hub IMPORTPATH]")
		return 2
	}
	violations, summary, err := check(*dagFile, *listFile, *hub)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "areadag: %v\n", err)
		return 2
	}
	if len(violations) > 0 {
		for _, v := range violations {
			_, _ = fmt.Fprintf(stdout, "areadag: %s\n", v)
		}
		_, _ = fmt.Fprintf(stdout, "areadag: %d violation(s); %s\n", len(violations), summary)
		_, _ = fmt.Fprintln(stdout, "areadag: fix the import, or (for a deliberate new edge) declare it in hack/areadag/dag.txt")
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "areadag: OK: %s\n", summary)
	return 0
}

// check runs every rule and returns the violations (sorted) and a one-line
// summary of the input counts. An error means the input was unusable.
func check(dagFile, listFile, hub string) ([]string, string, error) {
	d, err := dag.ReadFile(dagFile)
	if err != nil {
		return nil, "", err
	}
	pkgs, err := readList(listFile)
	if err != nil {
		return nil, "", err
	}

	byPath := map[string]*pkgImports{}
	var hubDir string
	nHub := 0
	for _, p := range pkgs {
		if p.Error != nil {
			return nil, "", fmt.Errorf("%s: go list reported an error for %s: %s", listFile, p.ImportPath, p.Error.Err)
		}
		if _, dup := byPath[p.ImportPath]; dup {
			return nil, "", fmt.Errorf("%s: %s is listed twice", listFile, p.ImportPath)
		}
		pi, err := collect(p, listFile)
		if err != nil {
			return nil, "", err
		}
		byPath[p.ImportPath] = pi
		if p.ImportPath == hub || strings.HasPrefix(p.ImportPath, hub+"/") {
			nHub++
		}
		if p.ImportPath == hub {
			hubDir = pi.dir
		}
	}

	switch {
	case len(pkgs) == 0:
		return nil, "", fmt.Errorf("%s: no packages (nothing to check)", listFile)
	case hubDir == "":
		return nil, "", fmt.Errorf("%s: %s is not in the list (run go list at the module root, or pass -hub)", listFile, hub)
	case len(d.Entries) == 0:
		return nil, "", fmt.Errorf("%s: no declarations", dagFile)
	case d.Count(dag.TestSupport) == 0:
		return nil, "", fmt.Errorf("%s: no testsupport declarations, so the testsupport rule would check nothing", dagFile)
	}
	for _, e := range d.Entries {
		if _, ok := byPath[hub+"/"+e.Rel]; !ok {
			return nil, "", fmt.Errorf("%s:%d: declared %s %s is not a package in the list (stale declaration?)", dagFile, e.Line, e.Kind, e.Rel)
		}
	}

	var violations []string
	edges := 0

	// Rules 1-3: declared packages.
	for _, e := range d.Entries {
		pi := byPath[hub+"/"+e.Rel]
		listed := map[string]bool{}
		for _, a := range e.Areas {
			listed[a] = true
		}
		for _, side := range []struct {
			name string
			imps map[string]bool
			test bool
		}{{"non-test", pi.nonTest, false}, {"test", pi.test, true}} {
			for _, imp := range sortedKeys(side.imps) {
				if imp == pi.path {
					continue // an external test package importing its own package
				}
				if imp != hub && !strings.HasPrefix(imp, hub+"/") {
					continue
				}
				edges++
				if imp == hub {
					violations = append(violations, fmt.Sprintf("%s %s imports pkg/hub in %s code (no declared package may import pkg/hub)", e.Kind, e.Rel, side.name))
					continue
				}
				rel := strings.TrimPrefix(imp, hub+"/")
				t, ok := d.Lookup(rel)
				switch {
				case !ok:
					violations = append(violations, fmt.Sprintf("%s %s imports undeclared pkg/hub/%s in %s code (declare it in dag.txt, or drop the import)", e.Kind, e.Rel, rel, side.name))
				case t.Kind == dag.Leaf:
				case t.Kind == dag.TestSupport:
					// Allowed from tests, and from testsupport non-test code;
					// a production import is reported once, under rule 4.
				case t.Kind == dag.Area && !listed[rel]:
					violations = append(violations, fmt.Sprintf("%s %s imports area %s in %s code, which its dag.txt line does not list", e.Kind, e.Rel, rel, side.name))
				}
			}
		}
		if e.Kind == dag.Leaf {
			for _, a := range e.Areas {
				for _, imp := range sortedKeys(byPath[hub+"/"+a].nonTest) {
					if imp == hub || strings.HasPrefix(imp, hub+"/") {
						violations = append(violations, fmt.Sprintf("leaf %s lists area %s, but %s imports %s in non-test code (an area a leaf may import must import no pkg/hub package)", e.Rel, a, a, strings.TrimPrefix(imp, hub+"/")))
					}
				}
			}
		}
	}

	// Rule 4 (C2): testsupport is imported only by tests, module-wide.
	support := map[string]bool{}
	for _, rel := range d.Rels(dag.TestSupport) {
		support[hub+"/"+rel] = true
	}
	for _, path := range sortedKeys(byPath) {
		if support[path] {
			continue
		}
		pi := byPath[path]
		for _, imp := range sortedKeys(pi.nonTest) {
			if !support[imp] {
				continue
			}
			files, err := filesImporting(pi.dir, pi.srcFiles, imp)
			if err != nil {
				return nil, "", err
			}
			violations = append(violations, fmt.Sprintf("%s imports testsupport %s in non-test code (%s); testsupport packages may be imported only by _test.go files", path, strings.TrimPrefix(imp, hub+"/"), strings.Join(files, ", ")))
		}
	}

	// Rule 5: alias files import only declared packages.
	aliases, err := filepath.Glob(filepath.Join(hubDir, "zz_alias_*.go"))
	if err != nil {
		return nil, "", err
	}
	if len(aliases) == 0 {
		return nil, "", fmt.Errorf("no zz_alias_*.go files in %s, so the alias rule would check nothing", hubDir)
	}
	sort.Strings(aliases)
	for _, f := range aliases {
		imps, err := fileImports(f)
		if err != nil {
			return nil, "", err
		}
		for _, imp := range imps {
			if !strings.HasPrefix(imp, hub+"/") {
				continue
			}
			edges++
			rel := strings.TrimPrefix(imp, hub+"/")
			if _, ok := d.Lookup(rel); !ok {
				violations = append(violations, fmt.Sprintf("alias file pkg/hub/%s imports pkg/hub/%s, which dag.txt does not declare (a moved package must be declared)", filepath.Base(f), rel))
			}
		}
	}

	if edges == 0 {
		return nil, "", errors.New("no pkg/hub import edges examined, so nothing was checked")
	}
	sort.Strings(violations)
	summary := fmt.Sprintf("%d module packages, %d pkg/hub packages, %d declared (%d leaf, %d area, %d testsupport), %d alias files, %d pkg/hub edges checked",
		len(pkgs), nHub, len(d.Entries), d.Count(dag.Leaf), d.Count(dag.Area), d.Count(dag.TestSupport), len(aliases), edges)
	return violations, summary, nil
}

// readList decodes a go list -json stream.
func readList(file string) ([]listPkg, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(f)
	var pkgs []listPkg
	for {
		var p listPkg
		err := dec.Decode(&p)
		if err == io.EOF {
			return pkgs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		if p.ImportPath == "" || p.Dir == "" {
			return nil, fmt.Errorf("%s: a package without ImportPath or Dir", file)
		}
		pkgs = append(pkgs, p)
	}
}

// collect builds a package's import sets. A relative Dir (used by the test
// fixtures; go list always writes an absolute one) is resolved against the
// directory of the list file.
func collect(p listPkg, listFile string) (*pkgImports, error) {
	dir := p.Dir
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(filepath.Dir(listFile), filepath.FromSlash(dir))
	}
	pi := &pkgImports{path: p.ImportPath, dir: dir, nonTest: map[string]bool{}, test: map[string]bool{}}
	for _, imp := range p.Imports {
		pi.nonTest[imp] = true
	}
	for _, imp := range append(append([]string{}, p.TestImports...), p.XTestImports...) {
		pi.test[imp] = true
	}
	pi.srcFiles = append(append([]string{}, p.GoFiles...), p.CgoFiles...)
	for _, name := range p.IgnoredGoFiles {
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		imps, err := fileImports(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		target := pi.nonTest
		if strings.HasSuffix(name, "_test.go") {
			target = pi.test
		} else {
			pi.srcFiles = append(pi.srcFiles, name)
		}
		for _, imp := range imps {
			target[imp] = true
		}
	}
	return pi, nil
}

// fileImports parses only the import block of a Go file.
func fileImports(file string) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range f.Imports {
		p, err := strconv.Unquote(s.Path.Value)
		if err != nil {
			return nil, fmt.Errorf("%s: bad import path %s", file, s.Path.Value)
		}
		out = append(out, p)
	}
	return out, nil
}

// filesImporting names the files among names (in dir) that import imp.
func filesImporting(dir string, names []string, imp string) ([]string, error) {
	var out []string
	for _, name := range names {
		imps, err := fileImports(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		for _, i := range imps {
			if i == imp {
				out = append(out, name)
				break
			}
		}
	}
	if len(out) == 0 {
		out = []string{"file not identified"}
	}
	sort.Strings(out)
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
