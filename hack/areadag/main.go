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
// module root (metadata only: no -export, no -compiled, so nothing compiles),
// and the module root itself (-root). The pkg/hub packages are the subset
// whose import path is pkg/hub or below it.
//
// Imports of a listed package are its go list Imports (non-test) and
// TestImports plus XTestImports (test), plus the imports of its
// IgnoredGoFiles, parsed here and classed by the _test.go suffix. go list
// drops a directory none of whose files match the default build (every file
// behind a tag such as integration, tzcontract or windows), so areadag also
// walks the module root, skipping what go skips (names starting with . or _,
// testdata, vendor, directories with their own go.mod), and parses every Go
// file of each directory go list did not list. A symlinked directory is not
// followed (rule 7 covers imports through one). go/build compiles a symlinked
// .go file, so one in a walked directory makes the input unusable (exit 2);
// the repository has none today. The union covers every build
// tag set and platform: a file counts whatever its constraint.
//
// The testsupport class comes only from dag.txt: a package is testsupport
// when its path is a testsupport entry or lies under one (prefix match), so a
// subpackage of a testsupport directory is testsupport even when undeclared.
// No name or directory convention elsewhere makes a package testsupport.
//
// Rules:
//
//  1. A declared package (leaf, area or testsupport) imports, in non-test or
//     test code, only declared leaves plus the areas listed on its line.
//     Imports of testsupport packages are rule 4's (allowed from tests, and
//     from testsupport non-test code). Any other pkg/hub subpackage is a
//     violation, declared or not.
//  2. No declared package imports pkg/hub itself.
//  3. An area listed on a leaf's line imports no pkg/hub package in non-test
//     code, so a leaf can never reach the rest of pkg/hub through it.
//  4. (C2) Over EVERY package of the module, a testsupport package appears only
//     in test imports, never in non-test imports. The only exception is a
//     testsupport importer: it is itself only reachable from tests.
//  5. Every pkg/hub subpackage imported by an alias file (zz_alias_*.go,
//     test or not) in the pkg/hub directory is declared, so a package move
//     cannot skip the DAG. pkgmove and hubshard extract both write alias files
//     under that name; the name, not the header marker, is the key.
//  6. (C2) No testsupport package is in the non-test import closure of any
//     package main of the module. With rule 4 this is implied; it is checked
//     on its own so that a production package mislabelled testsupport in
//     dag.txt still fails once any binary links it.
//  7. Every import of a path in this module (the module path or below it),
//     test or not, names a package that go list listed or the walk parsed.
//     The go command skips symlinked, ., _, testdata and vendor directories
//     only when matching ./...; an import still resolves to them by path. Such
//     a package would be invisible to rules 1-6, so importing one makes the
//     input unusable (exit 2: the check fails closed). This assumes the main
//     module requires no nested module under its own path (true today: the
//     nested modules under extras/ and cr-ci/ are not required by the main
//     module).
//
// Undeclared pkg/hub subpackages are not checked under rules 1-3 (feature
// packages carry no DAG obligation); they are still subject to rules 4 and 6,
// and importing one from a declared package violates rule 1.
//
// The run fails as unusable (exit 2), not clean, when a declared package does
// not exist, when a testsupport package is a package main (a walked directory
// counts as one when any non-test file in it declares package main, such as a
// //go:build ignore generator next to library files), or when any input count
// is zero: no module packages, no pkg/hub packages, no declarations, no
// testsupport declarations, no alias files, no import edges examined, or no
// package main; when a module import is unresolved (rule 7); and when a
// walked directory holds a symlinked .go file.
//
// Usage:
//
//	areadag -dag hack/areadag/dag.txt -list FILE [-root DIR] [-hub IMPORTPATH]
//
// -root defaults to the directory of the list file.
//
// Exit codes: 0 clean, 1 violations, 2 usage error, unreadable or malformed
// input, an input count of zero, a stale declaration, a testsupport package
// main, an unresolved module import (rule 7), or a symlinked .go file in a
// walked directory.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
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
	Name           string
	Dir            string
	GoFiles        []string
	CgoFiles       []string
	IgnoredGoFiles []string
	Imports        []string
	TestImports    []string
	XTestImports   []string
	Error          *struct{ Err string }
}

// pkgInfo is one package's name and its non-test and test import sets, keyed
// by import path, from go list (listed) or from parsing a directory go list
// did not list.
type pkgInfo struct {
	path    string
	name    string
	dir     string
	listed  bool
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
	root := flags.String("root", "", "the module root to walk for directories go list did not list (default: the list file's directory)")
	hub := flags.String("hub", defaultHub, "import path of pkg/hub")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *dagFile == "" || *listFile == "" || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage: areadag -dag FILE -list FILE [-root DIR] [-hub IMPORTPATH]")
		return 2
	}
	if *root == "" {
		*root = filepath.Dir(*listFile)
	}
	violations, summary, err := check(*dagFile, *listFile, *root, *hub)
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
func check(dagFile, listFile, root, hub string) ([]string, string, error) {
	d, err := dag.ReadFile(dagFile)
	if err != nil {
		return nil, "", err
	}
	pkgs, err := readList(listFile)
	if err != nil {
		return nil, "", err
	}
	if len(pkgs) == 0 {
		return nil, "", fmt.Errorf("%s: no packages (nothing to check)", listFile)
	}

	all := map[string]*pkgInfo{}
	listedDirs := map[string]bool{}
	var hubDir string
	for _, p := range pkgs {
		if p.Error != nil {
			return nil, "", fmt.Errorf("%s: go list reported an error for %s: %s", listFile, p.ImportPath, p.Error.Err)
		}
		if _, dup := all[p.ImportPath]; dup {
			return nil, "", fmt.Errorf("%s: %s is listed twice", listFile, p.ImportPath)
		}
		pi, err := collect(p, listFile)
		if err != nil {
			return nil, "", err
		}
		all[p.ImportPath] = pi
		listedDirs[pi.dir] = true
		if p.ImportPath == hub {
			hubDir = pi.dir
		}
	}
	if hubDir == "" {
		return nil, "", fmt.Errorf("%s: %s is not in the list (run go list at the module root, or pass -hub)", listFile, hub)
	}

	// The module path follows from where pkg/hub sits under the root.
	rootDir := realDir(root)
	hubRel, err := filepath.Rel(rootDir, hubDir)
	if err != nil || hubRel == "." || strings.HasPrefix(hubRel, "..") {
		return nil, "", fmt.Errorf("-root %s does not contain %s (%s)", root, hub, hubDir)
	}
	modPath, ok := strings.CutSuffix(hub, "/"+filepath.ToSlash(hubRel))
	if !ok || modPath == "" {
		return nil, "", fmt.Errorf("-root %s is not the module root: %s is at %s below it", root, hub, filepath.ToSlash(hubRel))
	}
	walked, err := walkUnlisted(rootDir, modPath, listedDirs)
	if err != nil {
		return nil, "", err
	}
	for _, pi := range walked {
		if _, dup := all[pi.path]; dup {
			return nil, "", fmt.Errorf("%s: directory %s maps to listed package %s", root, pi.dir, pi.path)
		}
		all[pi.path] = pi
	}
	nHub := 0
	for path := range all {
		if path == hub || strings.HasPrefix(path, hub+"/") {
			nHub++
		}
	}

	switch {
	case len(d.Entries) == 0:
		return nil, "", fmt.Errorf("%s: no declarations", dagFile)
	case d.Count(dag.TestSupport) == 0:
		return nil, "", fmt.Errorf("%s: no testsupport declarations, so the testsupport rules would check nothing", dagFile)
	}
	for _, e := range d.Entries {
		if _, ok := all[hub+"/"+e.Rel]; !ok {
			return nil, "", fmt.Errorf("%s:%d: declared %s %s is not a package of the module (stale declaration?)", dagFile, e.Line, e.Kind, e.Rel)
		}
	}

	supportRels := d.Rels(dag.TestSupport)
	isSupport := func(path string) bool {
		for _, rel := range supportRels {
			if path == hub+"/"+rel || strings.HasPrefix(path, hub+"/"+rel+"/") {
				return true
			}
		}
		return false
	}
	for _, path := range sortedKeys(all) {
		if isSupport(path) && all[path].name == "main" {
			return nil, "", fmt.Errorf("testsupport %s is a package main; a testsupport package must be a library imported by tests", strings.TrimPrefix(path, hub+"/"))
		}
	}

	var violations []string
	edges := 0

	// Rules 1-3: declared packages.
	for _, e := range d.Entries {
		pi := all[hub+"/"+e.Rel]
		listed := map[string]bool{}
		for _, a := range e.Areas {
			listed[a] = true
		}
		for _, side := range []struct {
			name string
			imps map[string]bool
		}{{"non-test", pi.nonTest}, {"test", pi.test}} {
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
				if isSupport(imp) {
					// Allowed from tests, and from testsupport non-test code;
					// a production import is reported once, under rule 4.
					continue
				}
				rel := strings.TrimPrefix(imp, hub+"/")
				t, ok := d.Lookup(rel)
				switch {
				case !ok:
					violations = append(violations, fmt.Sprintf("%s %s imports undeclared pkg/hub/%s in %s code (declare it in dag.txt, or drop the import)", e.Kind, e.Rel, rel, side.name))
				case t.Kind == dag.Area && !listed[rel]:
					violations = append(violations, fmt.Sprintf("%s %s imports area %s in %s code, which its dag.txt line does not list", e.Kind, e.Rel, rel, side.name))
				}
			}
		}
		if e.Kind == dag.Leaf {
			for _, a := range e.Areas {
				for _, imp := range sortedKeys(all[hub+"/"+a].nonTest) {
					if imp == hub || strings.HasPrefix(imp, hub+"/") {
						violations = append(violations, fmt.Sprintf("leaf %s lists area %s, but %s imports %s in non-test code (an area a leaf may import must import no pkg/hub package)", e.Rel, a, a, strings.TrimPrefix(imp, hub+"/")))
					}
				}
			}
		}
	}

	// Rule 4 (C2): testsupport is imported only by tests, module-wide.
	for _, path := range sortedKeys(all) {
		if isSupport(path) {
			continue
		}
		pi := all[path]
		for _, imp := range sortedKeys(pi.nonTest) {
			if !isSupport(imp) {
				continue
			}
			files, err := filesImporting(pi.dir, pi.srcFiles, imp)
			if err != nil {
				return nil, "", err
			}
			violations = append(violations, fmt.Sprintf("%s imports testsupport %s in non-test code (%s); testsupport packages may be imported only by _test.go files", path, strings.TrimPrefix(imp, hub+"/"), strings.Join(files, ", ")))
		}
	}

	// Rule 6 (C2): no package main reaches testsupport through non-test imports.
	mains := 0
	for _, path := range sortedKeys(all) {
		if all[path].name != "main" {
			continue
		}
		mains++
		for _, chain := range supportReach(path, all, isSupport) {
			violations = append(violations, fmt.Sprintf("package main %s reaches testsupport %s through non-test imports (%s); a binary must not link test support", path, strings.TrimPrefix(chain[len(chain)-1], hub+"/"), strings.Join(chain, " -> ")))
		}
	}

	// Rule 7: every import of a module path resolves to a package seen above.
	for _, path := range sortedKeys(all) {
		pi := all[path]
		for _, side := range []struct {
			name string
			imps map[string]bool
		}{{"non-test", pi.nonTest}, {"test", pi.test}} {
			for _, imp := range sortedKeys(side.imps) {
				if imp != modPath && !strings.HasPrefix(imp, modPath+"/") {
					continue
				}
				if _, ok := all[imp]; !ok {
					return nil, "", fmt.Errorf("import of module path %s that go list and the walk did not see (%s imports it in %s code; a symlinked, ., _, testdata or vendor directory, or symlinked files?): the rules cannot check it, so move the package to an ordinary directory", imp, path, side.name)
				}
			}
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
		imps, _, err := parseImports(f)
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
	if mains == 0 {
		return nil, "", errors.New("no package main in the module, so the binary closure rule would check nothing")
	}
	sort.Strings(violations)
	summary := fmt.Sprintf("%d module packages (%d from go list, %d tag-only directories parsed), %d pkg/hub packages, %d declared (%d leaf, %d area, %d testsupport), %d alias files, %d pkg/hub edges checked, %d package main closures checked",
		len(all), len(pkgs), len(walked), nHub, len(d.Entries), d.Count(dag.Leaf), d.Count(dag.Area), d.Count(dag.TestSupport), len(aliases), edges, mains)
	return violations, summary, nil
}

// supportReach returns, for each testsupport package reachable from start
// through non-test imports of module packages, one shortest import chain
// (start first). The search does not continue past a testsupport package.
func supportReach(start string, all map[string]*pkgInfo, isSupport func(string) bool) [][]string {
	parent := map[string]string{start: ""}
	queue := []string{start}
	var hits []string
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		pi, ok := all[cur]
		if !ok {
			continue // outside the module
		}
		for _, imp := range sortedKeys(pi.nonTest) {
			if _, seen := parent[imp]; seen {
				continue
			}
			parent[imp] = cur
			if isSupport(imp) {
				hits = append(hits, imp)
				continue
			}
			queue = append(queue, imp)
		}
	}
	sort.Strings(hits)
	var out [][]string
	for _, h := range hits {
		var chain []string
		for n := h; n != ""; n = parent[n] {
			chain = append([]string{n}, chain...)
		}
		out = append(out, chain)
	}
	return out
}

// walkUnlisted parses every directory under root that holds Go files but is
// not a listed package directory, skipping what the go command skips for
// ./...: names starting with . or _, testdata, vendor, and directories with
// their own go.mod. Symlinked directories are not followed (go ignores them);
// a symlinked .go file is an error (fail closed), because go/build compiles
// it.
func walkUnlisted(root, modPath string, listed map[string]bool) ([]*pkgInfo, error) {
	var out []*pkgInfo
	err := filepath.WalkDir(root, func(dir string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !de.IsDir() {
			return nil
		}
		if dir != root {
			name := de.Name()
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor" {
				return filepath.SkipDir
			}
			if fi, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !fi.IsDir() {
				return filepath.SkipDir
			}
		}
		if listed[dir] {
			return nil
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return err
		}
		path := modPath
		if rel != "." {
			path += "/" + filepath.ToSlash(rel)
		}
		pi := &pkgInfo{path: path, dir: dir, nonTest: map[string]bool{}, test: map[string]bool{}}
		files := 0
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				continue
			}
			if e.Type()&fs.ModeSymlink != 0 {
				// go/build compiles a symlink to a regular file as a source
				// file; fail closed rather than vouch for its target.
				return fmt.Errorf("symlinked .go file %s in a walked directory: areadag cannot vouch for it", filepath.Join(dir, name))
			}
			if !e.Type().IsRegular() {
				continue
			}
			files++
			if err := addFile(pi, name); err != nil {
				return err
			}
		}
		if files > 0 {
			out = append(out, pi)
		}
		return nil
	})
	return out, err
}

// addFile parses one file of pi's directory and adds its imports to the test
// or non-test set by the _test.go suffix. A non-test file declaring package
// main makes pi a package main.
func addFile(pi *pkgInfo, name string) error {
	imps, pkgName, err := parseImports(filepath.Join(pi.dir, name))
	if err != nil {
		return err
	}
	target := pi.test
	if !strings.HasSuffix(name, "_test.go") {
		target = pi.nonTest
		pi.srcFiles = append(pi.srcFiles, name)
		if pkgName == "main" || pi.name == "" {
			pi.name = pkgName
		}
	}
	for _, imp := range imps {
		target[imp] = true
	}
	return nil
}

// realDir returns dir as an absolute path with symlinks resolved, or as an
// absolute path when it cannot be resolved (a fixture directory that holds no
// file).
func realDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return filepath.Clean(dir)
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	return abs
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

// collect builds a listed package's import sets. A relative Dir (used by the
// test fixtures; go list always writes an absolute one) is resolved against
// the directory of the list file.
func collect(p listPkg, listFile string) (*pkgInfo, error) {
	dir := p.Dir
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(filepath.Dir(listFile), filepath.FromSlash(dir))
	}
	pi := &pkgInfo{path: p.ImportPath, name: p.Name, dir: realDir(dir), listed: true, nonTest: map[string]bool{}, test: map[string]bool{}}
	for _, imp := range p.Imports {
		pi.nonTest[imp] = true
	}
	for _, imp := range append(append([]string{}, p.TestImports...), p.XTestImports...) {
		pi.test[imp] = true
	}
	pi.srcFiles = append(append([]string{}, p.GoFiles...), p.CgoFiles...)
	name := pi.name
	for _, f := range p.IgnoredGoFiles {
		if !strings.HasSuffix(f, ".go") {
			continue
		}
		if err := addFile(pi, f); err != nil {
			return nil, err
		}
	}
	pi.name = name // go list's Name is authoritative for a listed package
	return pi, nil
}

// parseImports parses only the package clause and import block of a Go file.
func parseImports(file string) ([]string, string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
	if err != nil {
		return nil, "", err
	}
	var out []string
	for _, s := range f.Imports {
		p, err := strconv.Unquote(s.Path.Value)
		if err != nil {
			return nil, "", fmt.Errorf("%s: bad import path %s", file, s.Path.Value)
		}
		out = append(out, p)
	}
	return out, f.Name.Name, nil
}

// filesImporting names the files among names (in dir) that import imp.
func filesImporting(dir string, names []string, imp string) ([]string, error) {
	var out []string
	for _, name := range names {
		imps, _, err := parseImports(filepath.Join(dir, name))
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
