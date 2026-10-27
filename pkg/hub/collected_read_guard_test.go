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

package hub

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// collectedFullPathField is the transfer.FileInfo field that holds a
// collected file's absolute path. The resource uploaders (uploadDirBlobs and
// uploadResourceFiles) read collected files through an os.Root opened at the
// collected directory (transfer.ReadCollectedFile / OpenCollectedFile), not
// by that path.
const collectedFullPathField = "FullPath"

// checkCollectedFullPathSource parses one source file (src, or the file at
// path when src is nil) and returns a problem for every selector naming the
// FullPath field. The check is by name: package hub has no other field or
// method called FullPath, so any selector of that name is a read of a
// collected entry's absolute path. Composite-literal keys (FileInfo{FullPath:
// ...}) build an entry rather than read one and are not selectors, so they
// pass.
func checkCollectedFullPathSource(fset *token.FileSet, path string, src any) ([]string, error) {
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	var problems []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != collectedFullPathField {
			return true
		}
		problems = append(problems, fmt.Sprintf("%s: %s reads a collected file by its absolute path (.%s); read it through an os.Root with transfer.ReadCollectedFile or transfer.OpenCollectedFile",
			fset.Position(sel.Pos()), enclosingFuncName(fset, f, fset.Position(sel.Pos()).Offset), collectedFullPathField))
		return true
	})
	return problems, nil
}

// entryPathReadFuncs are the file-system calls the entry-path check covers,
// by import path: function name to the index of its path argument. They
// read, list or resolve a path (writes such as os.Create are not covered).
var entryPathReadFuncs = map[string]map[string]int{
	"os": {
		"Open": 0, "OpenFile": 0, "ReadFile": 0, "Stat": 0, "Lstat": 0,
		"ReadDir": 0, "Readlink": 0, "OpenRoot": 0, "DirFS": 0,
	},
	"path/filepath": {"Walk": 0, "WalkDir": 0, "EvalSymlinks": 0, "Glob": 0},
	"io/fs":         {"WalkDir": 1, "ReadFile": 1, "ReadDir": 1, "Stat": 1, "Glob": 1, "Sub": 1},
	"io/ioutil":     {"ReadFile": 0, "ReadDir": 0},
}

// entryPathJoinExceptions lists the functions ("<file>:<func>") allowed to
// pass a directory-plus-entry-path to entryPathReadFuncs, each with its
// reason. None reads collected content: every reader of collected content
// goes through the collectedDir. An entry that no longer matches a reported
// site is stale and fails the guard.
var entryPathJoinExceptions = map[string]string{
	// The request's URL path (r.URL.Path, copied to name) joined under the
	// web assets directory: static web assets served by path, not collected
	// resource content. The path is cleaned and dot segments are refused
	// before the join.
	"web.go:tryServeStaticFile": "serves static web assets by request path, not collected content",
}

// importAliases maps the names a file uses for its imports to their paths.
func importAliases(f *ast.File) map[string]string {
	aliases := map[string]string{}
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, "\"")
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		aliases[name] = p
	}
	return aliases
}

// pkgCall returns the import path and function name of a call pkg.F(...),
// with pkg resolved through aliases.
func pkgCall(call *ast.CallExpr, aliases map[string]string) (string, string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", "", false
	}
	p, ok := aliases[id.Name]
	if !ok {
		return "", "", false
	}
	return p, sel.Sel.Name, true
}

// hasEntryPathJoin reports whether e contains a filepath.Join or path.Join
// call one of whose arguments selects a Path field (fi.Path,
// filepath.FromSlash(e.Path), ...) or mentions a variable of pathVars (one
// assigned such a selector in the same function).
func hasEntryPathJoin(e ast.Expr, aliases map[string]string, pathVars map[string]bool) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || found {
			return !found
		}
		if p, fn, ok := pkgCall(call, aliases); ok && fn == "Join" && (p == "path/filepath" || p == "path") {
			for _, arg := range call.Args {
				ast.Inspect(arg, func(m ast.Node) bool {
					if sel, ok := m.(*ast.SelectorExpr); ok && sel.Sel.Name == "Path" {
						found = true
					}
					if id, ok := m.(*ast.Ident); ok && pathVars[id.Name] {
						found = true
					}
					return !found
				})
			}
		}
		return !found
	})
	return found
}

// usesIdent reports whether e mentions one of the identifiers in names.
func usesIdent(e ast.Expr, names map[string]bool) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && names[id.Name] {
			found = true
		}
		return !found
	})
	return found
}

// checkEntryPathJoinSource parses one source file (src, or the file at path
// when src is nil) and returns a problem for every call to an
// entryPathReadFuncs function whose path argument contains a filepath.Join or
// path.Join with a Path-selecting argument, or mentions a variable assigned
// such an expression in the same function (one level of tracking). A
// function listed in exceptions ("<base name of path>:<func>") is allowed;
// each exception that allowed a site is recorded in used.
func checkEntryPathJoinSource(fset *token.FileSet, path string, src any, exceptions map[string]string, used map[string]bool) ([]string, error) {
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	aliases := importAliases(f)
	base := path[strings.LastIndexAny(path, `/\`)+1:]
	var problems []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		// pathVars: variables assigned an expression that selects a Path field
		// (p := fi.Path). joined: variables assigned a directory-plus-entry
		// join.
		pathVars := map[string]bool{}
		selectsPath := func(e ast.Expr) bool {
			found := false
			ast.Inspect(e, func(m ast.Node) bool {
				if sel, ok := m.(*ast.SelectorExpr); ok && sel.Sel.Name == "Path" {
					found = true
				}
				return !found
			})
			return found
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch st := n.(type) {
			case *ast.AssignStmt:
				if len(st.Lhs) == len(st.Rhs) {
					for i, rhs := range st.Rhs {
						if id, ok := st.Lhs[i].(*ast.Ident); ok && selectsPath(rhs) {
							pathVars[id.Name] = true
						}
					}
				}
			case *ast.ValueSpec:
				if len(st.Names) == len(st.Values) {
					for i, v := range st.Values {
						if selectsPath(v) {
							pathVars[st.Names[i].Name] = true
						}
					}
				}
			}
			return true
		})
		joined := map[string]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch st := n.(type) {
			case *ast.AssignStmt:
				if len(st.Lhs) == len(st.Rhs) {
					for i, rhs := range st.Rhs {
						if id, ok := st.Lhs[i].(*ast.Ident); ok && hasEntryPathJoin(rhs, aliases, pathVars) {
							joined[id.Name] = true
						}
					}
				}
			case *ast.ValueSpec:
				if len(st.Names) == len(st.Values) {
					for i, v := range st.Values {
						if hasEntryPathJoin(v, aliases, pathVars) {
							joined[st.Names[i].Name] = true
						}
					}
				}
			}
			return true
		})
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			p, name, ok := pkgCall(call, aliases)
			if !ok {
				return true
			}
			idx, covered := entryPathReadFuncs[p][name]
			if !covered || idx >= len(call.Args) {
				return true
			}
			arg := call.Args[idx]
			if !hasEntryPathJoin(arg, aliases, pathVars) && !usesIdent(arg, joined) {
				return true
			}
			key := base + ":" + fn.Name.Name
			if _, ok := exceptions[key]; ok {
				used[key] = true
				return true
			}
			problems = append(problems, fmt.Sprintf("%s: %s passes a path built from a directory and an entry's Path to %s.%s; read collected entries through the collectedDir (its os.Root and transfer.ReadCollectedFile)",
				fset.Position(call.Pos()), fn.Name.Name, p, name))
			return true
		})
	}
	return problems, nil
}

// TestCollectedFilesReadThroughRoot is a source guard. Property: no non-test
// file in pkg/hub (or an area moved out of it) reads a collected entry
// through a path instead of the collected directory's root. It reports
// (1) any selector of a collected entry's FullPath, and (2) any call to a
// file-system read, list or resolve function (entryPathReadFuncs: os.Open,
// os.ReadFile, os.Stat, os.Lstat, os.ReadDir, filepath.WalkDir, fs.ReadFile
// and the like) whose path argument is built from a directory and an entry's
// Path (a filepath.Join or path.Join with a Path-selecting argument, or with a
// variable assigned a Path selector in the same function; directly or through
// a variable assigned the join in the same function). Collected entries
// are read through the collectedDir: its os.Root and
// transfer.ReadCollectedFile. Exceptions are listed in entryPathJoinExceptions
// with their reasons; a stale exception fails.
//
// Limits: the check is syntactic and per function. Not seen: a path passed to
// a helper (in this package or another) that takes it as a string and reads
// it; a path built by string concatenation; a value carried through more than
// one variable on either side of the join; code in package-level function
// literals (var f = func() {...}), since only function declarations are
// walked; and writes. The persistence hooks take
// no directory path (only the collectedDir), which is the structural guard
// for the bootstrap and import flow.
func TestCollectedFilesReadThroughRoot(t *testing.T) {
	// pkgmove:scan-covers guardScanSubdirs
	// scanned through guardScanSubdirs: pkg/hub and every area moved out of it.
	files, err := globGuardScanDirs(".", "*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found to scan")
	}
	fset := token.NewFileSet()
	used := map[string]bool{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		problems, err := checkCollectedFullPathSource(fset, file, nil)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		joins, err := checkEntryPathJoinSource(fset, file, nil, entryPathJoinExceptions, used)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, p := range append(problems, joins...) {
			t.Error(p)
		}
	}
	for key := range entryPathJoinExceptions {
		if !used[key] {
			t.Errorf("stale entryPathJoinExceptions entry %s: no reported site remains; remove it", key)
		}
	}
}

// TestCheckEntryPathJoinSource feeds the entry-path check synthetic files:
// reads of a directory-plus-entry-path are reported in their join forms, and
// reads through a root, of fixed names, writes and listed exceptions pass.
func TestCheckEntryPathJoinSource(t *testing.T) {
	const imports = `import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
)
var _ = fs.ReadFile
var _ = path.Join
`
	flagged := map[string]string{
		"os.ReadFile of a join":       `func read(dir string, fi FileInfo) { _, _ = os.ReadFile(filepath.Join(dir, fi.Path)) }`,
		"os.Open of a FromSlash join": `func read(dir string, e *Entry) { _, _ = os.Open(filepath.Join(dir, filepath.FromSlash(e.Path))) }`,
		"os.Stat of a variable":       `func read(dir string, f FileInfo) { fp := filepath.Join(dir, f.Path); _, _ = os.Stat(fp) }`,
		"os.Lstat of a join":          `func read(d string, x FileInfo) { _, _ = os.Lstat(filepath.Join(d, x.Path)) }`,
		"os.ReadDir of a join":        `func read(d string, x FileInfo) { _, _ = os.ReadDir(filepath.Join(d, x.Path)) }`,
		"filepath.WalkDir of a join":  `func read(d string, x FileInfo) { _ = filepath.WalkDir(filepath.Join(d, x.Path), nil) }`,
		"fs.ReadFile of a path.Join":  `func read(fsys fs.FS, d string, x FileInfo) { _, _ = fs.ReadFile(fsys, path.Join(d, x.Path)) }`,
		"var declaration":             `func read(d string, x FileInfo) { var p = filepath.Join(d, x.Path); _, _ = os.Open(p) }`,
		"variable wrapped in a call":  `func read(d string, x FileInfo) { p := filepath.Join(d, x.Path); _, _ = os.Open(filepath.Clean(p)) }`,
		"call inside a func literal":  `func read(d string, x FileInfo) { f := func() { _, _ = os.ReadFile(filepath.Join(d, x.Path)) }; f() }`,
		"Path copied before the join": `func read(d string, x FileInfo) { p := x.Path; _, _ = os.Open(filepath.Join(d, p)) }`,
	}
	for name, body := range flagged {
		t.Run("flagged/"+name, func(t *testing.T) {
			src := "package hub\n" + imports + body + "\n"
			problems, err := checkEntryPathJoinSource(token.NewFileSet(), "flagged.go", src, nil, map[string]bool{})
			if err != nil {
				t.Fatal(err)
			}
			if len(problems) != 1 || !strings.Contains(problems[0], "read passes a path built from a directory and an entry's Path") {
				t.Fatalf("want one problem naming read, got %v", problems)
			}
		})
	}

	t.Run("flagged/aliased imports", func(t *testing.T) {
		src := `package hub
import (
	myos "os"
	fp "path/filepath"
)
func read(d string, x FileInfo) { _, _ = myos.ReadFile(fp.Join(d, x.Path)) }
`
		problems, err := checkEntryPathJoinSource(token.NewFileSet(), "flagged.go", src, nil, map[string]bool{})
		if err != nil {
			t.Fatal(err)
		}
		if len(problems) != 1 {
			t.Fatalf("want one problem, got %v", problems)
		}
	})

	allowed := map[string]string{
		"read through the root":      `func read(root *os.Root, fi FileInfo) { _, _ = root.Open(filepath.FromSlash(fi.Path)) }`,
		"read of a fixed name":       `func read(dir string) { _, _ = os.ReadFile(filepath.Join(dir, "config.yaml")) }`,
		"join without a Path field":  `func read(dir, rel string) { _, _ = os.Stat(filepath.Join(dir, rel)) }`,
		"write of an entry path":     `func write(dir string, f FileInfo) { _, _ = os.Create(filepath.Join(dir, f.Path)) }`,
		"variable from another func": `func a(d string, x FileInfo) { p := filepath.Join(d, x.Path); _ = p }` + "\n" + `func b(p string) { _, _ = os.Open(p) }`,
	}
	for name, body := range allowed {
		t.Run("allowed/"+name, func(t *testing.T) {
			src := "package hub\n" + imports + body + "\n"
			problems, err := checkEntryPathJoinSource(token.NewFileSet(), "allowed.go", src, nil, map[string]bool{})
			if err != nil {
				t.Fatal(err)
			}
			if len(problems) != 0 {
				t.Errorf("want no problems, got %v", problems)
			}
		})
	}

	t.Run("allowed/listed exception", func(t *testing.T) {
		src := "package hub\n" + imports + `func read(dir string, fi FileInfo) { _, _ = os.ReadFile(filepath.Join(dir, fi.Path)) }` + "\n"
		used := map[string]bool{}
		problems, err := checkEntryPathJoinSource(token.NewFileSet(), "x/allowed.go", src, map[string]string{"allowed.go:read": "test"}, used)
		if err != nil {
			t.Fatal(err)
		}
		if len(problems) != 0 || !used["allowed.go:read"] {
			t.Errorf("want the exception used and no problems, got %v (used %v)", problems, used)
		}
	})
}

// TestCheckCollectedFullPathSource feeds the source guard synthetic files: a
// read of FullPath is reported however it is reached, and code that reads
// through the root, or only builds an entry, passes.
func TestCheckCollectedFullPathSource(t *testing.T) {
	flagged := map[string]string{
		"os.ReadFile": `package hub
import ("os"; "github.com/GoogleCloudPlatform/scion/pkg/transfer")
func upload(fi transfer.FileInfo) { _, _ = os.ReadFile(fi.FullPath) }`,
		"os.Open of a slice element": `package hub
import ("os"; "github.com/GoogleCloudPlatform/scion/pkg/transfer")
func upload(files []transfer.FileInfo) { _, _ = os.Open(files[0].FullPath) }`,
		"assigned path": `package hub
import xfer "github.com/GoogleCloudPlatform/scion/pkg/transfer"
func upload(j struct{ fi xfer.FileInfo }) string { p := j.fi.FullPath; return p }`,
	}
	for name, src := range flagged {
		t.Run("flagged/"+name, func(t *testing.T) {
			problems, err := checkCollectedFullPathSource(token.NewFileSet(), "flagged.go", src)
			if err != nil {
				t.Fatal(err)
			}
			if len(problems) != 1 {
				t.Fatalf("want one problem, got %d: %v", len(problems), problems)
			}
			if !strings.Contains(problems[0], "upload reads a collected file") {
				t.Errorf("problem does not name the function: %s", problems[0])
			}
		})
	}

	allowed := map[string]string{
		"read through the root": `package hub
import ("os"; "github.com/GoogleCloudPlatform/scion/pkg/transfer")
func upload(root *os.Root, fi transfer.FileInfo) { _, _ = transfer.ReadCollectedFile(root, fi) }`,
		"build an entry": `package hub
import "github.com/GoogleCloudPlatform/scion/pkg/transfer"
func entry(p string) transfer.FileInfo { return transfer.FileInfo{Path: "a", FullPath: p} }`,
	}
	for name, src := range allowed {
		t.Run("allowed/"+name, func(t *testing.T) {
			problems, err := checkCollectedFullPathSource(token.NewFileSet(), "allowed.go", src)
			if err != nil {
				t.Fatal(err)
			}
			if len(problems) != 0 {
				t.Errorf("want no problems, got %v", problems)
			}
		})
	}
}
