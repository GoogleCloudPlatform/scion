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

package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// settingsBypassScanRoots are the source trees (relative to this package)
// whose non-test Go files must not write a settings file except through the
// locked writer (writeSettingsFileAtomic, under LockSettingsDir).
var settingsBypassScanRoots = []string{".", "../../cmd", "../hub", "../hubsync", "../runtimebroker", "../agent"}

// settingsBypassAllowed lists the functions ("<file>:<func>", the file
// relative to its scan root) that still write a settings file directly, as
// far as this check can see (see its limits below). Each must be a known,
// tracked exception; an entry that no longer matches fails the test, so the
// list cannot go stale.
var settingsBypassAllowed = map[string]string{
	// Rewritten from a map, without the lock; moved to the locked writer
	// in a follow-up change (ptone/scion#3918, part B).
	"integration_config.go:AddSelfManagedPluginWithBrokerType": "part B",
	"integration_config.go:AddPluginToSettings":                "part B",
	"integration_config.go:AddPluginToMessageBrokerTypes":      "part B",
	"admin_settings.go:handlePutServerConfig":                  "part B",
	"handlers_github_app.go:persistGitHubAppConfig":            "part B",
	"handlers_projects_core.go:initHubManagedProject":          "part B",
	// The legacy hub.grove_id key migration: it runs from the settings
	// loaders, sometimes while the caller already holds the settings lock,
	// so it cannot take the (non-re-entrant) lock and uses compare-then-
	// rename instead (see its comment). Retired with the legacy loader in
	// ptone/scion#3919, or else moved to the locked writer in part B.
	"legacy_grove_migration.go:migrateLegacyYAMLKeysLocked": "ptone/scion#3919",
}

// settingsFileNames are the file names whose writes must use the writer.
var settingsFileNames = map[string]bool{
	"settings.yaml": true, "settings.yml": true, "settings.json": true, "state.yaml": true,
}

// settingsPathFuncs return a settings file path.
var settingsPathFuncs = map[string]bool{
	"GetSettingsPath": true, "newSettingsFilePath": true, "newSettingsFilePathWithout": true,
}

// rawWriteFuncs are the os functions that write a file, with the index of
// their destination path argument.
var rawWriteFuncs = map[string]int{
	"WriteFile": 0, "Create": 0, "OpenFile": 0, "Rename": 1,
	// The unlocked atomic writers: only writeSettingsFileAtomic (which
	// checks the lock) may call them for a settings file.
	"writeFileAtomic": 0, "writeFileAtomicMode": 0,
}

// rawWritePkgs are the import paths whose functions in rawWriteFuncs are
// raw writes (resolved through each file's imports, so an aliased import
// counts too).
var rawWritePkgs = map[string]bool{"os": true, "io/ioutil": true}

// TestSettingsWrites_NoBypassOfLockedWriter is a call-site check over the
// syntax trees of the scanned packages. A raw write is a call to
// os.WriteFile, os.Create, os.OpenFile or os.Rename (destination), the same
// functions of io/ioutil (under any import name), or writeFileAtomic or
// writeFileAtomicMode. It is a bypass of the locked writer, unless the
// enclosing function is allowed above, when:
//   - its path argument is, or is built (through local variables of the
//     same function) from, a settings file name literal or a settings path
//     function; or
//   - it is a call through a package-level function variable initialised to
//     one of those functions (such as renameFile = os.Rename): the
//     indirection hides what it writes, so every such call must be listed.
//
// It cannot follow a settings path passed into another function's
// parameter, or package-level variables holding paths; review covers those.
func TestSettingsWrites_NoBypassOfLockedWriter(t *testing.T) {
	found := map[string]bool{}
	for _, root := range settingsBypassScanRoots {
		// Parse each package directory as a whole, so package-level
		// function variables declared in one file are seen in the others.
		dirs := map[string][]string{}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if path != root && (d.Name() == "testdata" || (root == "." && path != ".")) {
					return filepath.SkipDir // pkg/config only, not its sub-packages
				}
				return nil
			}
			if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				dirs[filepath.Dir(path)] = append(dirs[filepath.Dir(path)], path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", root, err)
		}
		for _, paths := range dirs {
			fset := token.NewFileSet()
			files := map[string]*ast.File{}
			for _, path := range paths {
				f, err := parser.ParseFile(fset, path, nil, 0)
				if err != nil {
					t.Fatalf("parse %s: %v", path, err)
				}
				files[path] = f
			}
			funcVars := rawWriteFuncVars(files)
			for path, f := range files {
				rel, _ := filepath.Rel(root, path)
				pkgNames := rawWritePkgNames(f)
				for _, decl := range f.Decls {
					fn, ok := decl.(*ast.FuncDecl)
					if !ok || fn.Body == nil {
						continue
					}
					for _, pos := range settingsBypassCalls(fn, pkgNames, funcVars) {
						key := filepath.ToSlash(rel) + ":" + fn.Name.Name
						found[key] = true
						if _, ok := settingsBypassAllowed[key]; !ok {
							t.Errorf("%s: %s writes a settings file without the locked writer (use writeSettingsFileAtomic under LockSettingsDir)", fset.Position(pos), fn.Name.Name)
						}
					}
				}
			}
		}
	}
	var stale []string
	for key := range settingsBypassAllowed {
		if !found[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		t.Errorf("settingsBypassAllowed entry %s no longer writes a settings file directly; remove it", key)
	}
}

// rawWritePkgNames returns the names under which f imports the packages in
// rawWritePkgs.
func rawWritePkgNames(f *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || !rawWritePkgs[p] {
			continue
		}
		name := filepath.Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		names[name] = true
	}
	return names
}

// rawWriteFuncVars returns the package-level variables of a package that
// are initialised to a raw write function (var renameFile = os.Rename).
func rawWriteFuncVars(files map[string]*ast.File) map[string]bool {
	vars := map[string]bool{}
	for _, f := range files {
		pkgNames := rawWritePkgNames(f)
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					sel, ok := vs.Values[i].(*ast.SelectorExpr)
					if !ok {
						continue
					}
					pkg, ok := sel.X.(*ast.Ident)
					if _, raw := rawWriteFuncs[sel.Sel.Name]; ok && raw && pkgNames[pkg.Name] {
						vars[id.Name] = true
					}
				}
			}
		}
	}
	return vars
}

// settingsBypassCalls returns the positions of raw writes to a settings file
// path in fn, and of every call through a raw write function variable.
func settingsBypassCalls(fn *ast.FuncDecl, pkgNames, funcVars map[string]bool) []token.Pos {
	// Local assignments: name -> right-hand sides.
	assigns := map[string][]ast.Expr{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range s.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok {
					continue
				}
				if len(s.Rhs) == len(s.Lhs) {
					assigns[id.Name] = append(assigns[id.Name], s.Rhs[i])
				} else if len(s.Rhs) == 1 {
					assigns[id.Name] = append(assigns[id.Name], s.Rhs[0])
				}
			}
		case *ast.ValueSpec:
			for i, id := range s.Names {
				if i < len(s.Values) {
					assigns[id.Name] = append(assigns[id.Name], s.Values[i])
				}
			}
		}
		return true
	})

	var refers func(e ast.Expr, seen map[string]bool) bool
	refers = func(e ast.Expr, seen map[string]bool) bool {
		hit := false
		ast.Inspect(e, func(n ast.Node) bool {
			if hit {
				return false
			}
			switch x := n.(type) {
			case *ast.BasicLit:
				if x.Kind == token.STRING {
					if v, err := strconv.Unquote(x.Value); err == nil && settingsFileNames[filepath.Base(v)] {
						hit = true
					}
				}
			case *ast.CallExpr:
				if name := calleeName(x); settingsPathFuncs[name] {
					hit = true
				}
			case *ast.Ident:
				if !seen[x.Name] {
					seen[x.Name] = true
					for _, rhs := range assigns[x.Name] {
						if refers(rhs, seen) {
							hit = true
							break
						}
					}
				}
			}
			return !hit
		})
		return hit
	}

	var out []token.Pos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && funcVars[id.Name] {
			out = append(out, call.Pos())
			return true
		}
		name := calleeName(call)
		argIdx, ok := rawWriteFuncs[name]
		if !ok || argIdx >= len(call.Args) {
			return true
		}
		if sel, isSel := call.Fun.(*ast.SelectorExpr); isSel {
			if pkg, ok := sel.X.(*ast.Ident); !ok || !pkgNames[pkg.Name] {
				return true
			}
		} else if name != "writeFileAtomic" && name != "writeFileAtomicMode" {
			return true // a local function of that name, not os
		}
		if refers(call.Args[argIdx], map[string]bool{}) {
			out = append(out, call.Pos())
		}
		return true
	})
	return out
}

// calleeName returns the called function's name (the selector for pkg.F).
func calleeName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// The guard's own rules, on a synthetic package: an aliased os import, an
// io/ioutil write, a function variable initialised to os.Rename, a path
// built from a settings file name or from a settings path function, and an
// unqualified writeFileAtomic are caught; a write of another file, the
// source of a rename and a local function named WriteFile are not.
func TestSettingsBypassCalls_Rules(t *testing.T) {
	const src = `package p

import (
	osx "os"
	"io/ioutil"
	"path/filepath"
)

var renameIt = osx.Rename

func aliased(dir string) { _ = osx.WriteFile(filepath.Join(dir, "settings.yaml"), nil, 0644) }
func viaIoutil(dir string) {
	p := filepath.Join(dir, "state.yaml")
	_ = ioutil.WriteFile(p, nil, 0644)
}
func viaVar(a, b string) { _ = renameIt(a, b) }
func otherFile(dir string) { _ = osx.WriteFile(filepath.Join(dir, "notes.txt"), nil, 0644) }
func renameDest(dir string) { _ = osx.Rename("x.tmp", filepath.Join(dir, "settings.yml")) }
func renameSource(dir string) { _ = osx.Rename(filepath.Join(dir, "settings.json"), "x.bak") }
func WriteFile(name string) {}
func localWriteFile(dir string) { WriteFile(filepath.Join(dir, "settings.yaml")) }
func viaPathFunc(dir string) { _ = osx.WriteFile(GetSettingsPath(dir), nil, 0644) }
func unqualifiedAtomic(dir string) { _ = writeFileAtomic(filepath.Join(dir, "settings.yaml"), nil) }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	funcVars := rawWriteFuncVars(map[string]*ast.File{"p.go": f})
	if !funcVars["renameIt"] {
		t.Errorf("function variable not found: %v", funcVars)
	}
	pkgNames := rawWritePkgNames(f)
	got := map[string]int{}
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			got[fn.Name.Name] = len(settingsBypassCalls(fn, pkgNames, funcVars))
		}
	}
	want := map[string]int{
		"aliased": 1, "viaIoutil": 1, "viaVar": 1, "otherFile": 0,
		// os.Rename counts its destination only.
		"renameDest": 1, "renameSource": 0,
		// A local function named WriteFile is not os.WriteFile.
		"localWriteFile": 0,
		// A settings path function as the path counts.
		"viaPathFunc": 1,
		// The unlocked atomic helpers count unqualified.
		"unqualifiedAtomic": 1,
	}
	for name, n := range want {
		if got[name] != n {
			t.Errorf("%s: %d raw settings writes found, want %d", name, got[name], n)
		}
	}
}
