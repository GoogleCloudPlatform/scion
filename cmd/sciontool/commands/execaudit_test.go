/*
Copyright 2025 The Scion Authors.
*/

package commands

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// moduleImportPrefix is this module's own import path prefix (see go.mod's
// `module` line). An import with this prefix names another package in this
// same repo; stripping the prefix gives that package's directory relative
// to the repo root, which is how execAuditReachablePackages walks the
// module's own import graph using only go/parser — no go/packages, no `go
// list`, no network or build step required.
const moduleImportPrefix = "github.com/GoogleCloudPlatform/scion/"

// execAuditRoot is where TestNoRawExecInPID1Path's reachability walk
// starts. Every sciontool subcommand — including `init`, the one that
// starts procreap.StartReaper's SIGCHLD reaper — is wired up as a cobra
// command somewhere under cmd/sciontool/commands (see root.go's
// rootCmd.AddCommand calls), so walking the in-module import graph from
// here reaches every package any subcommand's production code can call
// into.
//
// This deliberately does not try to isolate just runInit's own call graph:
// cobra subcommands share a package directory, so separating "runInit's
// callees" from "some other subcommand's callees" by anything short of a
// real call-graph analysis (which a parse-only, stdlib-only test can't
// justify) would mean re-deriving, and re-trusting, a hand-maintained list
// again — exactly what made the previous version of this test
// (execAuditDirs, a fixed list of 5 directories) miss reachable packages
// silently. Instead, this over-approximates reachability — it will also
// scan code that only some other subcommand (doctor, harness, metadata
// status, provision, ...) uses — and relies on execAuditFileAllowlist to
// name, and justify, each file confirmed to run in a different process
// than runInit's PID 1.
const execAuditRoot = "cmd/sciontool/commands"

// execAuditFileAllowlist lists source files that are exempt from
// TestNoRawExecInPID1Path's raw-exec scan, and from
// execAuditSymbolAllowlist's import restrictions below, because each is
// confirmed to never run inside sciontool init's PID-1 process while
// procreap.StartReaper's SIGCHLD reaper is active. Every reason here is
// meant to be independently checkable against this repo (root.go's command
// wiring, or the calling file's own imports) — not a citation of a document
// that won't ship with the code. Adding a file needs that same kind of
// confirmation, not just enough to make this test pass.
var execAuditFileAllowlist = map[string]string{
	"cmd/sciontool/commands/doctor.go": "root.go wires doctorCmd as its own top-level cobra " +
		"command (`sciontool doctor`); runInit never calls into it, and StartReaper is only " +
		"started by runInit",
	"cmd/sciontool/commands/harness.go": "root.go wires harnessCmd/harnessProvisionCmd as " +
		"their own command tree (`sciontool harness provision`); the harness child runInit's " +
		"Supervisor actually starts is a fresh execve of this same binary as a brand new " +
		"process, not an in-PID-1 call into this file",
	"cmd/sciontool/commands/metadata.go": "root.go wires metadataCmd/metadataStatusCmd as " +
		"their own command (`sciontool metadata status`); the metadata proxy runInit actually " +
		"starts lives in pkg/sciontool/metadata, which IS scanned (and IS routed through " +
		"procreap)",
	"cmd/sciontool/commands/provision.go": "root.go wires provisionCmd as its own top-level " +
		"cobra command (`sciontool provision`), a broker-host-side workspace-provisioning " +
		"entrypoint distinct from runInit",
	"pkg/provision/provision.go": "only called from provision.go's RunE (see above), via the " +
		"separate `sciontool provision` subcommand",
	"pkg/util/browser.go": "OpenBrowser has no caller anywhere in cmd/sciontool or " +
		"pkg/sciontool; it's dead code from runInit's perspective, used by a different " +
		"binary's login flow",
	"pkg/util/git.go": "these are the top-level `scion` CLI's (cmd/*.go) git helpers; " +
		"init.go's own use of this package is restricted to the pure string/error-" +
		"classification identifiers in execAuditSymbolAllowlist below, none of which touch " +
		"exec.Cmd",
}

// execAuditSymbolAllowlist closes a gap a per-file allowlist alone leaves
// open: without it, a scanned file could dodge this entire check by calling
// a *new* exec-backed helper in an allowlisted package instead of calling
// exec.Command itself — e.g. init.go calling a hypothetical
// util.CloneSharedWorkspace(...) instead of going through
// pkg/sciontool/procreap directly, the same shape of bug this test exists
// to catch. For each package directory below, a scanned (non-file-
// allowlisted) file that imports it may only reference the listed
// identifiers; anything else is a violation, and a package directory that
// execAuditRiskyPackageDirs flags but that has no entry here at all is
// treated as "nothing from it is allowed yet" rather than silently passing.
var execAuditSymbolAllowlist = map[string]map[string]bool{
	// init.go's only uses of pkg/util: NormalizeGitRemote/ClassifyGitError
	// are pure string/error-classification functions, and GitErrAuth is an
	// error-kind constant compared against — none call exec.Cmd. The
	// package's actual exec.Command call sites (pkg/util/git.go) are listed
	// in execAuditFileAllowlist above.
	"pkg/util": {"NormalizeGitRemote": true, "ClassifyGitError": true, "GitErrAuth": true},
}

// execAuditTrustedDir is excluded from scanning outright rather than via
// execAuditFileAllowlist: it's the safe implementation this whole guard
// exists to make sure everything else goes through, not a file that avoids
// the race for some other reason. (It's still reachable, and its own
// _test.go files intentionally call raw exec.Cmd methods to drive the
// reaper under test — irrelevant here since only non-test files are
// scanned at all.)
const execAuditTrustedDir = "pkg/sciontool/procreap"

// TestNoRawExecInPID1Path is a regression guard for a class of bug, not a
// single line: fix/broker-waitid-echild routed every exec.Cmd call site
// reachable from sciontool init's PID-1 process through
// pkg/sciontool/procreap's managed helpers (RunManaged/CombinedOutputManaged/
// OutputManaged, or the manual Gated+RegisterManagedPID/UnregisterManagedPID
// pattern used by Supervisor.Run and services.managedService.start), because
// a raw Run/Output/CombinedOutput call — or a raw Start() later Wait()ed —
// races procreap's SIGCHLD reaper for the child's exit status. Nothing in
// Go's type system stops a future change from adding a new raw call in this
// path and silently reintroducing that race.
//
// This test parses (does not build, vet, or run) every non-test .go file in
// every package reachable, via in-module imports, from execAuditRoot (see
// its doc comment), and fails if it finds a raw-exec violation (see
// findRawExecViolations) or a restricted-import violation (see
// findSymbolAllowlistViolations), unless the file is listed in
// execAuditFileAllowlist. It is a syntactic, not type-checked, heuristic —
// deliberately, to stay fast and dependency-free — so it can still be
// fooled by sufficiently indirect code (an exec.Cmd smuggled through an
// interface{}, returned from a same-file or cross-package helper function
// instead of constructed inline, or handed to a helper in another package
// that isn't itself scanned and calls .Run() on it). It is a tripwire for
// the straightforward regression a reviewer would actually expect someone
// to write, not a soundness proof.
func TestNoRawExecInPID1Path(t *testing.T) {
	repoRoot := repoRootForTest(t)
	reachable := execAuditReachablePackages(t, repoRoot)
	risky := execAuditRiskyPackageDirs()

	var dirs []string
	for d := range reachable {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	var violations []string
	for _, dir := range dirs {
		if dir == execAuditTrustedDir {
			t.Logf("skipping trusted implementation %s", dir)
			continue
		}

		absDir := filepath.Join(repoRoot, filepath.FromSlash(dir))
		entries, err := os.ReadDir(absDir)
		if err != nil {
			t.Fatalf("reading %s: %v", absDir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			relPath := dir + "/" + name
			if reason, ok := execAuditFileAllowlist[relPath]; ok {
				t.Logf("skipping allowlisted %s: %s", relPath, reason)
				continue
			}

			absPath := filepath.Join(absDir, name)
			vs, err := findRawExecViolations(absPath)
			if err != nil {
				t.Fatalf("parsing %s: %v", relPath, err)
			}
			for _, v := range vs {
				violations = append(violations, relPath+": "+v)
			}

			vs, err = findSymbolAllowlistViolations(absPath, risky)
			if err != nil {
				t.Fatalf("parsing %s: %v", relPath, err)
			}
			for _, v := range vs {
				violations = append(violations, relPath+": "+v)
			}
		}
	}
	sort.Strings(violations)

	if len(violations) > 0 {
		for _, v := range violations {
			t.Error(v)
		}
		t.Fatalf("%d violation(s) found in the PID-1 path outside pkg/sciontool/procreap; route "+
			"raw exec.Cmd calls through procreap.RunManaged/CombinedOutputManaged/OutputManaged "+
			"(or the Gated+RegisterManagedPID/UnregisterManagedPID pattern), and route calls into "+
			"a risky package through execAuditSymbolAllowlist — or add an execAuditFileAllowlist "+
			"entry with a citation if the file is genuinely unreachable from runInit", len(violations))
	}
}

// execAuditReachablePackages walks the in-module import graph starting at
// execAuditRoot (breadth-first) and returns every reached package as a
// directory path relative to repoRoot, execAuditRoot included. It only
// follows non-test files' imports — test-only imports don't reflect
// runInit's runtime call graph — and only within this module: stdlib and
// third-party imports can't call back into this module's own exec.Cmd call
// sites, so they're irrelevant to this walk.
func execAuditReachablePackages(t *testing.T, repoRoot string) map[string]bool {
	t.Helper()
	visited := map[string]bool{}
	queue := []string{execAuditRoot}
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if visited[dir] {
			continue
		}
		visited[dir] = true

		absDir := filepath.Join(repoRoot, filepath.FromSlash(dir))
		entries, err := os.ReadDir(absDir)
		if err != nil {
			t.Fatalf("reading %s: %v", absDir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, filepath.Join(absDir, name), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parsing imports of %s/%s: %v", dir, name, err)
			}
			for _, imp := range file.Imports {
				p, err := strconv.Unquote(imp.Path.Value)
				if err != nil || !strings.HasPrefix(p, moduleImportPrefix) {
					continue
				}
				rel := strings.TrimPrefix(p, moduleImportPrefix)
				if !visited[rel] {
					queue = append(queue, rel)
				}
			}
		}
	}
	return visited
}

// execAuditRiskyPackageDirs derives, from execAuditFileAllowlist, the set of
// package directories that contain at least one file known to call raw exec
// but not from runInit's path. A scanned file that imports one of these is
// checked against execAuditSymbolAllowlist by findSymbolAllowlistViolations.
func execAuditRiskyPackageDirs() map[string]bool {
	risky := map[string]bool{}
	for f := range execAuditFileAllowlist {
		risky[filepath.ToSlash(filepath.Dir(f))] = true
	}
	return risky
}

// repoRootForTest returns the absolute path of the repo root, derived from
// this test file's own location (cmd/sciontool/commands/execaudit_test.go
// is exactly three directories below it) rather than the working directory,
// so it doesn't depend on how `go test` was invoked.
func repoRootForTest(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	return root
}

// exprKey renders a simple identifier or a chain of field/selector accesses
// (e.g. "cmd" or "s.cmd") as a dotted string key, for correlating an
// exec.Cmd-holding variable or struct field across statements. It reports
// false for expressions it doesn't understand (index expressions, calls,
// etc.), which are simply not tracked — a false negative, not a false
// positive.
func exprKey(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name, true
	case *ast.SelectorExpr:
		base, ok := exprKey(v.X)
		if !ok {
			return "", false
		}
		return base + "." + v.Sel.Name, true
	default:
		return "", false
	}
}

// unwrapParen strips any wrapping parens, e.g. from `(&exec.Cmd{...}).Run()`.
func unwrapParen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// isExecCmdValue reports whether e directly constructs an exec.Cmd: a call
// to exec.Command/CommandContext, or an exec.Cmd{...} composite literal
// (bare, or address-of as `&exec.Cmd{...}`, the only form Go allows calling
// a method on inline).
func isExecCmdValue(e ast.Expr, execAlias string) bool {
	switch v := unwrapParen(e).(type) {
	case *ast.CallExpr:
		sel, ok := v.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || id.Name != execAlias {
			return false
		}
		return sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext"
	case *ast.UnaryExpr:
		if v.Op != token.AND {
			return false
		}
		return isExecCmdValue(v.X, execAlias)
	case *ast.CompositeLit:
		sel, ok := v.Type.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == execAlias && sel.Sel.Name == "Cmd"
	default:
		return false
	}
}

// pidArgKey extracts the tracked exec.Cmd key from an argument shaped like
// <key>.Process.Pid, which is how procreap.RegisterManagedPID/
// UnregisterManagedPID are always called (see managed.go, supervisor.go,
// services/manager.go).
func pidArgKey(e ast.Expr) (string, bool) {
	full, ok := exprKey(e)
	if !ok {
		return "", false
	}
	const suffix = ".Process.Pid"
	if !strings.HasSuffix(full, suffix) {
		return "", false
	}
	return strings.TrimSuffix(full, suffix), true
}

// findRawExecViolations parses a single Go source file and returns one
// human-readable description per raw-exec violation found: a bare
// os/exec.Cmd Run/Output/CombinedOutput call (on a tracked variable/field,
// or chained directly off a freshly-constructed exec.Cmd value), or a
// tracked exec.Cmd's Start() call that is also Wait()ed (directly, or via
// its embedded os.Process.Wait(), which reaps the child the same way)
// somewhere in the file without a matching procreap.Gated +
// RegisterManagedPID(<key>.Process.Pid, ...) / UnregisterManagedPID(<same
// key>.Process.Pid, ...) for that same key.
//
// Two known precision gaps, both accepted for a parse-only heuristic: (1)
// procreap.Gated's presence is checked file-wide, not tied to a specific
// Start() call, because correlating it to one would require inspecting the
// body of the closure passed to Gated; RegisterManagedPID/UnregisterManagedPID
// are checked per-key so a second, ungated exec.Cmd added to an already-
// gated file is still caught. (2) An exec.Cmd returned from a helper
// function (same-file or cross-package) rather than constructed inline is
// not tracked at all — findSymbolAllowlistViolations catches the
// cross-package version of this for the specific packages it restricts, but
// a same-file `func mkCmd() *exec.Cmd { ... }` helper is not caught here.
func findRawExecViolations(path string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}

	execAlias := importAlias(file, "os/exec", "exec")
	if execAlias == "" {
		return nil, nil // file doesn't import os/exec at all
	}
	procreapAlias := importAlias(file, "/sciontool/procreap", "procreap")

	// Pass 1: every identifier/selector-chain that this file assigns the
	// direct result of exec.Command/CommandContext or an exec.Cmd{...}
	// composite literal to.
	tracked := map[string]bool{}
	trackAssign := func(lhs, rhs []ast.Expr) {
		if len(lhs) != len(rhs) {
			return
		}
		for i, r := range rhs {
			if !isExecCmdValue(r, execAlias) {
				continue
			}
			if key, ok := exprKey(lhs[i]); ok {
				tracked[key] = true
			}
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			trackAssign(v.Lhs, v.Rhs)
		case *ast.ValueSpec:
			lhs := make([]ast.Expr, len(v.Names))
			for i, name := range v.Names {
				lhs[i] = name
			}
			trackAssign(lhs, v.Values)
		}
		return true
	})

	// Pass 2: violations, plus per-key Gated+Register/Unregister bookkeeping.
	var violations []string
	var hasGated bool
	registeredKeys := map[string]bool{}
	unregisteredKeys := map[string]bool{}
	startPos := map[string]token.Pos{}
	waitPos := map[string]token.Pos{}

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		// Direct chain: e.g. exec.Command(...).Run(), (&exec.Cmd{...}).Output().
		if isExecCmdValue(sel.X, execAlias) {
			switch sel.Sel.Name {
			case "Run", "Output", "CombinedOutput":
				violations = append(violations, fmt.Sprintf(
					"%s: %s() called directly on an exec.Cmd value, bypassing pkg/sciontool/procreap",
					fset.Position(call.Pos()), sel.Sel.Name))
			}
			return true
		}

		if procreapAlias != "" {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == procreapAlias {
				switch sel.Sel.Name {
				case "Gated":
					hasGated = true
				case "RegisterManagedPID":
					if len(call.Args) > 0 {
						if k, ok := pidArgKey(call.Args[0]); ok {
							registeredKeys[k] = true
						}
					}
				case "UnregisterManagedPID":
					if len(call.Args) > 0 {
						if k, ok := pidArgKey(call.Args[0]); ok {
							unregisteredKeys[k] = true
						}
					}
				}
				return true
			}
		}

		if key, ok := exprKey(sel.X); ok && tracked[key] {
			switch sel.Sel.Name {
			case "Run", "Output", "CombinedOutput":
				violations = append(violations, fmt.Sprintf(
					"%s: %s.%s() called directly instead of going through pkg/sciontool/procreap",
					fset.Position(call.Pos()), key, sel.Sel.Name))
			case "Start":
				startPos[key] = call.Pos()
			case "Wait":
				waitPos[key] = call.Pos()
			}
			return true
		}

		// <key>.Process.Wait(): os.Process.Wait also reaps the child, so a
		// tracked exec.Cmd's Start() paired with this is just as racy as
		// pairing it with <key>.Wait() directly.
		if sel.Sel.Name == "Wait" {
			if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "Process" {
				if innerKey, ok := exprKey(inner.X); ok && tracked[innerKey] {
					waitPos[innerKey] = call.Pos()
				}
			}
		}
		return true
	})

	for key, pos := range startPos {
		if _, waited := waitPos[key]; !waited {
			continue
		}
		if hasGated && registeredKeys[key] && unregisteredKeys[key] {
			continue // manual Gated+Register/Unregister pattern for this exec.Cmd (e.g. Supervisor.Run)
		}
		violations = append(violations, fmt.Sprintf(
			"%s: %s.Start() is Wait()ed elsewhere in this file without a matching procreap.Gated "+
				"+ RegisterManagedPID(%s.Process.Pid, ...)/UnregisterManagedPID(%s.Process.Pid, ...)",
			fset.Position(pos), key, key, key))
	}

	return violations, nil
}

// findSymbolAllowlistViolations parses a single Go source file and flags any
// reference to a "risky" package (one execAuditRiskyPackageDirs flags,
// because some other file in it is only exempt from findRawExecViolations
// via execAuditFileAllowlist) that isn't in that package's
// execAuditSymbolAllowlist entry. A risky package with no entry at all means
// nothing from it is approved yet.
func findSymbolAllowlistViolations(path string, risky map[string]bool) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}

	var violations []string
	for _, imp := range file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || !strings.HasPrefix(p, moduleImportPrefix) {
			continue
		}
		relDir := strings.TrimPrefix(p, moduleImportPrefix)
		if !risky[relDir] {
			continue
		}

		alias := relDir[strings.LastIndex(relDir, "/")+1:]
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		allowed, hasAllowlist := execAuditSymbolAllowlist[relDir]

		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || id.Name != alias {
				return true
			}
			if hasAllowlist && allowed[sel.Sel.Name] {
				return true
			}
			reason := fmt.Sprintf("%q has no execAuditSymbolAllowlist entry", relDir)
			if hasAllowlist {
				reason = fmt.Sprintf("not in execAuditSymbolAllowlist[%q]", relDir)
			}
			violations = append(violations, fmt.Sprintf(
				"%s: %s.%s references package %q, which contains exec.Cmd call sites outside "+
					"runInit's path, but %s", fset.Position(sel.Pos()), alias, sel.Sel.Name, relDir, reason))
			return true
		})
	}
	return violations, nil
}

// importAlias returns the local identifier a file uses to refer to an
// import, given either the import's exact path (e.g. "os/exec") or a path
// suffix (e.g. "/sciontool/procreap"). It returns "" if the file has no
// matching import. Matching by suffix lets this tolerate the internal
// package's full module path without hardcoding it twice.
func importAlias(file *ast.File, pathOrSuffix, defaultAlias string) string {
	for _, imp := range file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		if p != pathOrSuffix && !strings.HasSuffix(p, pathOrSuffix) {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return defaultAlias
	}
	return ""
}
