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

// Static guards. They parse source with go/parser, so they run (and must
// pass) on every OS, Linux included.

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// repoRoot walks up from the package directory to the directory holding
// go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the package directory")
		}
		dir = parent
	}
}

// allowEntry is one line of .github/windows-test-packages.txt: a package
// and, for a line with an anchored -run (./cmd, ./pkg/config), the test
// names that run on Windows. names is nil for a whole-package line.
type allowEntry struct {
	pkg   string
	names map[string]bool
}

// windowsTestEntries reads .github/windows-test-packages.txt: one package
// per line, optionally followed by go test flags, '#' starts a comment. A
// flagged line must carry -run=^(A|B|...)$ or -run=^A$ (anchored names),
// which windows.yml also requires to run and pass.
func windowsTestEntries(t *testing.T, root string) []allowEntry {
	t.Helper()
	f, err := os.Open(filepath.Join(root, ".github", "windows-test-packages.txt"))
	if err != nil {
		t.Fatalf("read the Windows test allowlist: %v", err)
	}
	defer func() { _ = f.Close() }()
	var out []allowEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimSuffix(sc.Text(), "\r"))
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		e := allowEntry{pkg: fields[0]}
		for _, fl := range fields[1:] {
			run, ok := strings.CutPrefix(fl, "-run=")
			if !ok {
				continue
			}
			body, ok := strings.CutPrefix(run, "^")
			if ok {
				body, ok = strings.CutSuffix(body, "$")
			}
			if ok && strings.HasPrefix(body, "(") && strings.HasSuffix(body, ")") {
				body = body[1 : len(body)-1]
			}
			if !ok || body == "" || strings.ContainsAny(body, "()^$.*+?[]{}\\") {
				t.Fatalf("allowlist line %q: -run must be anchored names, ^(A|B)$ or ^A$", line)
			}
			e.names = map[string]bool{}
			for _, n := range strings.Split(body, "|") {
				e.names[n] = true
			}
		}
		if len(fields) > 1 && e.names == nil {
			t.Fatalf("allowlist line %q: flags without an anchored -run", line)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// skipOnLinkPath reports, per top-level function in src, whether it both
// creates a symlink or junction (os.Symlink, MustSymlink, MustJunction, or
// a "mklink" string) and can skip (t.Skip, t.Skipf, t.SkipNow, or a
// testing.Short() check). It also returns how many functions create links.
//
// keep, when not nil, limits the check to the functions it accepts (the
// tests an anchored allowlist line runs on Windows, and every helper).
func skipOnLinkPath(fset *token.FileSet, file *ast.File, keep func(name string) bool) (offenders []string, linkFuncs int) {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if keep != nil && !keep(fn.Name.Name) {
			continue
		}
		links, skips := false, false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				switch x.Sel.Name {
				case "Symlink", "MustSymlink", "MustJunction":
					links = true
				case "Skip", "Skipf", "SkipNow":
					skips = true
				case "Short":
					if id, ok := x.X.(*ast.Ident); ok && id.Name == "testing" {
						skips = true
					}
				}
			case *ast.Ident:
				if x.Name == "MustSymlink" || x.Name == "MustJunction" {
					links = true
				}
			case *ast.BasicLit:
				if x.Kind == token.STRING && strings.Contains(strings.ToLower(x.Value), "mklink") {
					links = true
				}
			}
			return true
		})
		if links {
			linkFuncs++
			if skips {
				offenders = append(offenders, fset.Position(fn.Pos()).String()+" "+fn.Name.Name)
			}
		}
	}
	return offenders, linkFuncs
}

// TestNoSkipOnSymlinkPaths fails if any function in a test file of an
// allowlisted Windows package (or in internal/testutil/wintest) that
// creates a symlink or junction can also skip. A symlink test that skips
// silently on the runner reports green while testing nothing; the helpers
// fail instead.
func TestNoSkipOnSymlinkPaths(t *testing.T) {
	// Positive control: the detector flags a skip on a link path and
	// accepts a link path without one.
	const sample = `package p
func TestBad(t *testing.T) { if err := os.Symlink("a", "b"); err != nil { t.Skip("no privilege") } }
func TestShort(t *testing.T) { if testing.Short() { return }; wintest.MustJunction(t, "a", "b") }
func TestGood(t *testing.T) { wintest.MustSymlink(t, "a", "b") }
func TestUnrelatedSkip(t *testing.T) { t.Skip("no link here") }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sample_test.go", sample, 0)
	if err != nil {
		t.Fatal(err)
	}
	if off, n := skipOnLinkPath(fset, f, nil); len(off) != 2 || n != 3 {
		t.Fatalf("detector on the sample: offenders %v, link functions %d; want TestBad and TestShort, 3", off, n)
	}
	// An anchored line checks only its named tests and the helpers.
	if off, n := skipOnLinkPath(fset, f, func(name string) bool { return name == "TestGood" || name == "TestBad" }); len(off) != 1 || n != 2 {
		t.Fatalf("detector on the sample, limited to TestGood and TestBad: offenders %v, link functions %d; want TestBad, 2", off, n)
	}

	root := repoRoot(t)
	entries := windowsTestEntries(t, root)
	if len(entries) == 0 {
		t.Fatal("the Windows test allowlist is empty")
	}
	// dirs maps a package directory to the test names run on Windows; nil
	// means the whole package (a plain line wins over anchored ones).
	dirs := map[string]map[string]bool{filepath.Join(root, "internal", "testutil", "wintest"): nil}
	self := false
	for _, e := range entries {
		p := e.pkg
		if strings.Contains(p, "...") {
			t.Fatalf("allowlist line %q: wildcards are not allowed", p)
		}
		d := filepath.Join(root, filepath.FromSlash(p))
		names, seen := dirs[d]
		switch {
		case e.names == nil:
			dirs[d] = nil
		case !seen:
			dirs[d] = map[string]bool{}
			names = dirs[d]
			fallthrough
		case names != nil:
			for n := range e.names {
				names[n] = true
			}
		}
		if filepath.ToSlash(p) == "./pkg/util/privatefs" {
			self = true
		}
	}
	if !self {
		t.Fatal("the Windows test allowlist does not include ./pkg/util/privatefs, which holds this guard")
	}
	var offenders []string
	linkFuncs := 0
	for dir, names := range dirs {
		var keep func(string) bool
		if names != nil {
			// Anchored: only the named tests run on Windows; helpers are
			// always checked.
			keep = func(n string) bool { return !strings.HasPrefix(n, "Test") || names[n] }
		}
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 0 {
			t.Fatalf("no Go files in %s", dir)
		}
		for _, name := range files {
			if !strings.HasSuffix(name, "_test.go") && !strings.Contains(dir, "wintest") {
				continue
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, name, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			off, n := skipOnLinkPath(fset, f, keep)
			offenders = append(offenders, off...)
			linkFuncs += n
		}
	}
	if linkFuncs == 0 {
		t.Fatal("no function creating a symlink or junction was found: the guard would pass vacuously")
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("functions that create a symlink or junction must fail, never skip (use wintest.MustSymlink/MustJunction):\n%s", strings.Join(offenders, "\n"))
	}
}

// accountNames are names a verifier must never compare or look up: they
// are localised, and a local group may be created with any name.
var accountNames = []string{"Administrators", "SYSTEM", "Everyone", "Users", "Administrator", "LocalSystem"}

// nameLookups returns the name-based account APIs called, and the account
// names used as string literals, in file.
func nameLookups(fset *token.FileSet, file *ast.File) []string {
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			name := x.Sel.Name
			if name == "LookupAccountName" || name == "LookupAccountSid" || name == "LookupSID" ||
				name == "LookupAccount" || strings.HasPrefix(name, "NetLocalGroup") || strings.HasPrefix(name, "NetUser") {
				found = append(found, fset.Position(x.Pos()).String()+" "+name)
			}
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(x.Value)
			if err != nil {
				return true
			}
			upper := strings.ToUpper(v)
			if strings.Contains(upper, `BUILTIN\`) || strings.Contains(upper, `NT AUTHORITY\`) {
				found = append(found, fset.Position(x.Pos()).String()+" "+x.Value)
			}
			for _, a := range accountNames {
				if strings.EqualFold(strings.TrimSpace(v), a) {
					found = append(found, fset.Position(x.Pos()).String()+" "+x.Value)
				}
			}
		}
		return true
	})
	return found
}

// TestVerifierUsesNoNameLookup fails if the Windows create or verify code
// looks up or compares account names. SIDs come only from the token,
// CreateWellKnownSid and StringToSid.
func TestVerifierUsesNoNameLookup(t *testing.T) {
	// Positive control: the detector flags a lookup and a name literal.
	const sample = `package p
func f() { windows.LookupSID("", "Administrators"); _ = name == "BUILTIN\\Users" }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sample.go", sample, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := nameLookups(fset, f); len(got) != 3 {
		t.Fatalf("detector on the sample found %v, want the call and both literals", got)
	}

	// replace_windows.go holds the follow-then-verify rule, whose owner set
	// (SYSTEM, Administrators) is by SID only too.
	for _, name := range []string{"privatefs_windows.go", "replace_windows.go"} {
		fset = token.NewFileSet()
		f, err = parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := nameLookups(fset, f); len(got) > 0 {
			t.Fatalf("%s uses account names:\n%s", name, strings.Join(got, "\n"))
		}
	}
}

// pathOpeners are calls that resolve a path by name (following links or
// not): none may appear on the walk below a Tree's anchor, where every
// component is opened relative to its parent's handle.
var pathOpeners = map[string]bool{
	"CreateFile": true, "CreateDirectory": true, "GetFileAttributes": true, "GetFileAttributesEx": true,
	"Open": true, "OpenFile": true, "Stat": true, "Lstat": true, "Mkdir": true, "MkdirAll": false,
	"ReadDir": true, "Readlink": true, "EvalSymlinks": true, "Resolve": true, "OpenDir": true,
	"openFollow": true, "createAnchor": true, "OpenTree": true,
}

// walkFuncs is the MkdirAll/MkdirAllEach path below the anchor (also the
// walk of every other Tree operation).
var walkFuncs = []string{"MkdirAll", "MkdirAllEach", "verifyExisting", "walk", "openDirComp", "openDirAt", "mkdirAt", "checkHandleNoForeignWriters"}

// handleWalkViolations reports, for the walk functions in file, any call to
// a path-based opener, and any ntCreate whose RootDirectory argument is a
// literal 0 (an absolute, by-name open). It also returns how many walk
// functions and ntCreate calls it inspected.
func handleWalkViolations(fset *token.FileSet, file *ast.File) (bad []string, funcs, ntCreates int) {
	want := map[string]bool{}
	for _, n := range walkFuncs {
		want[n] = true
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || !want[fn.Name.Name] {
			continue
		}
		funcs++
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch f := call.Fun.(type) {
			case *ast.Ident:
				name = f.Name
			case *ast.SelectorExpr:
				name = f.Sel.Name
			}
			if pathOpeners[name] {
				bad = append(bad, fmt.Sprintf("%s %s calls %s", fset.Position(call.Pos()), fn.Name.Name, name))
			}
			if name == "ntCreate" {
				ntCreates++
				if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Value == "0" {
					bad = append(bad, fmt.Sprintf("%s %s calls ntCreate with RootDirectory 0", fset.Position(call.Pos()), fn.Name.Name))
				}
			}
			return true
		})
	}
	return bad, funcs, ntCreates
}

// TestTreeWalkIsHandleRelative: the property ensureSSHDir relies on: every
// component below a
// Tree's anchor, on the MkdirAll path, is opened relative to its parent's
// handle (ntCreate with a RootDirectory), and no path-based open appears
// anywhere on that path. Behavioural tests: the junction/symlink refusal and
// FollowLinks tests, and cmd's TestEnsureSSHDir_* tests.
func TestTreeWalkIsHandleRelative(t *testing.T) {
	// Positive control: the detector flags a path open and a root-less
	// ntCreate in a walk function, and ignores other functions.
	const sample = `package p
func walk() { windows.CreateFile(nil, 0, 0, nil, 0, 0, 0); ntCreate(0, "x") }
func openDirAt(parent int) { ntCreate(parent, "x") }
func other() { os.Open("x") }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sample.go", sample, 0)
	if err != nil {
		t.Fatal(err)
	}
	if bad, funcs, nts := handleWalkViolations(fset, f); len(bad) != 2 || funcs != 2 || nts != 2 {
		t.Fatalf("detector on the sample: %v, %d functions, %d ntCreate calls; want 2 violations, 2, 2", bad, funcs, nts)
	}
	const diagSample = `package p
func (t *Tree) warnMutableLinkParent(h int, name string) error { windows.CloseHandle(h); return nil }
func (t *Tree) openDirComp() { t.warnMutableLinkParent(1, "x") }
func (t *Tree) walk() { _ = t.warnMutableLinkParent(1, "x"); t.warnMutableLinkParent(1, "y") }
`
	df, err := parser.ParseFile(token.NewFileSet(), "diag.go", diagSample, 0)
	if err != nil {
		t.Fatal(err)
	}
	if msgs := diagnosticViolations(token.NewFileSet(), df); len(msgs) != 4 {
		t.Fatalf("diagnostic detector on the sample: %v; want 4 (returns a value, closes h, used as a value, called from walk)", msgs)
	}

	fset = token.NewFileSet()
	f, err = parser.ParseFile(fset, "privatefs_windows.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	bad, funcs, nts := handleWalkViolations(fset, f)
	if funcs != len(walkFuncs) || nts == 0 {
		t.Fatalf("privatefs_windows.go: found %d of the walk functions %v and %d ntCreate calls: the guard would pass vacuously", funcs, walkFuncs, nts)
	}
	if len(bad) > 0 {
		t.Fatalf("the walk below a Tree's anchor must open every component relative to its parent's handle:\n%s", strings.Join(bad, "\n"))
	}
	// The one by-path open reachable from the walk is the diagnostic
	// warnMutableLinkParent (an accepted residual, warned). It must stay unable to influence a result: no return
	// value.
	if msgs := diagnosticViolations(fset, f); len(msgs) > 0 {
		t.Fatalf("warnMutableLinkParent must stay diagnostic only:\n%s", strings.Join(msgs, "\n"))
	}
}

// diagnosticViolations checks that warnMutableLinkParent cannot influence
// the walk: it returns nothing, never closes or duplicates its handle
// parameter, and is called only as a bare statement from openDirComp.
func diagnosticViolations(fset *token.FileSet, file *ast.File) []string {
	var msgs []string
	found, calls := false, 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if fn.Name.Name == "warnMutableLinkParent" {
			found = true
			if fn.Type.Results != nil && len(fn.Type.Results.List) > 0 {
				msgs = append(msgs, "warnMutableLinkParent returns a value")
			}
			var hname string
			if ps := fn.Type.Params.List; len(ps) > 0 && len(ps[0].Names) > 0 {
				hname = ps[0].Names[0].Name
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "CloseHandle" && sel.Sel.Name != "DuplicateHandle") {
					return true
				}
				for _, a := range call.Args {
					if id, ok := a.(*ast.Ident); ok && id.Name == hname {
						msgs = append(msgs, fmt.Sprintf("%s %s on the walk's handle %s", fset.Position(call.Pos()), sel.Sel.Name, hname))
					}
				}
				return true
			})
		}
		// Every call of warnMutableLinkParent: a bare statement in openDirComp.
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ExprStmt:
				if isDiagCall(x.X) {
					calls++
					if fn.Name.Name != "openDirComp" {
						msgs = append(msgs, fmt.Sprintf("%s warnMutableLinkParent called from %s", fset.Position(x.Pos()), fn.Name.Name))
					}
					return false
				}
			case *ast.CallExpr:
				if isDiagCall(x) {
					msgs = append(msgs, fmt.Sprintf("%s warnMutableLinkParent used as a value", fset.Position(x.Pos())))
				}
			}
			return true
		})
	}
	if !found {
		msgs = append(msgs, "warnMutableLinkParent not found")
	}
	if calls == 0 {
		msgs = append(msgs, "warnMutableLinkParent is never called: the check would pass vacuously")
	}
	return msgs
}

func isDiagCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "warnMutableLinkParent"
}
