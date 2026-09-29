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

// execAuditAllowlist lists source files that are exempt from
// TestNoRawExecInPID1Path even though they call exec.Command/CommandContext
// directly. Every entry must be a file that sciontool init's runInit call
// graph never reaches while the SIGCHLD reaper (procreap.StartReaper) is
// active — see fix/broker-waitid-echild's exec-site audit for the full
// reachability argument behind each one. Adding a file here needs the same
// scrutiny as that audit; do not add an entry just to make this test pass.
var execAuditAllowlist = map[string]string{
	// Separate `sciontool doctor` subcommand/process invocation (its own
	// os.Args, own RunE). runInit never calls into it, and StartReaper is
	// only started by runInit.
	"cmd/sciontool/commands/doctor.go": "separate `sciontool doctor` subcommand, not reachable from runInit",
	// Separate `sciontool harness provision` subcommand: its own process
	// (e.g. a k8s init container), or the fresh execve child `sciontool
	// harness` started by the supervised Supervisor — either way, not
	// running inside the PID-1 process that owns the reaper.
	"cmd/sciontool/commands/harness.go": "separate `sciontool harness provision` subcommand, not reachable from runInit",
	// Separate `sciontool metadata status` subcommand. The metadata proxy
	// that runInit actually starts lives in pkg/sciontool/metadata, which
	// IS scanned below (and IS routed through procreap).
	"cmd/sciontool/commands/metadata.go": "separate `sciontool metadata status` subcommand, not reachable from runInit",
}

// execAuditDirs are the directories (relative to the repo root) containing
// every source file reachable from sciontool init's runInit while
// procreap.StartReaper's SIGCHLD reaper is active. This must track the
// "Reachable from sciontool init" column of fix/broker-waitid-echild's
// exec-site audit table: if a new package with its own exec.Cmd call sites
// becomes reachable from runInit, add it here too, or this test cannot see
// it.
var execAuditDirs = []string{
	"cmd/sciontool/commands",
	"pkg/sciontool/supervisor",
	"pkg/sciontool/hooks",
	"pkg/sciontool/services",
	"pkg/sciontool/metadata",
}

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
// This test parses (does not build, vet, or run) every non-test .go file
// under execAuditDirs and fails if it finds one, unless the file is listed
// in execAuditAllowlist. It is a syntactic, not type-checked, heuristic —
// deliberately, to stay fast and dependency-free — so it can be fooled by
// sufficiently indirect code (e.g. an exec.Cmd smuggled through an
// interface{} or a helper function in another package). It is a tripwire
// for the straightforward regression a reviewer would actually expect
// someone to write, not a soundness proof.
func TestNoRawExecInPID1Path(t *testing.T) {
	repoRoot := repoRootForTest(t)

	var violations []string
	for _, dir := range execAuditDirs {
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
			if reason, ok := execAuditAllowlist[relPath]; ok {
				t.Logf("skipping allowlisted %s: %s", relPath, reason)
				continue
			}
			vs, err := findRawExecViolations(filepath.Join(absDir, name))
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
		t.Fatalf("%d raw exec.Cmd call(s) found in the PID-1 path outside pkg/sciontool/procreap; "+
			"route them through procreap.RunManaged/CombinedOutputManaged/OutputManaged (or the "+
			"Gated+RegisterManagedPID/UnregisterManagedPID pattern), or add the file to "+
			"execAuditAllowlist with a citation if it is genuinely unreachable from runInit", len(violations))
	}
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

// findRawExecViolations parses a single Go source file and returns one
// human-readable description per raw-exec violation found (position, plus
// what's wrong): a bare os/exec.Cmd.Run/Output/CombinedOutput call (whether
// on a tracked variable/field or chained directly off exec.Command(...)),
// or a tracked exec.Cmd's Start() call that is also Wait()ed somewhere in
// the file without the file also using procreap's
// Gated/RegisterManagedPID/UnregisterManagedPID trio.
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

	isExecCall := func(e ast.Expr) bool {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || id.Name != execAlias {
			return false
		}
		return sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext"
	}

	// Pass 1: every identifier/selector-chain that this file assigns the
	// direct result of exec.Command/CommandContext to.
	tracked := map[string]bool{}
	trackAssign := func(lhs, rhs []ast.Expr) {
		if len(lhs) != len(rhs) {
			return
		}
		for i, r := range rhs {
			if !isExecCall(r) {
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

	// Pass 2: violations, plus whether this file uses the manual
	// Gated+Register/Unregister pattern anywhere (Supervisor.Run and
	// services.managedService.start split Start() and Wait() across two
	// methods of the same struct, so this is tracked file-wide rather than
	// per-function).
	var violations []string
	var hasGated, hasRegister, hasUnregister bool
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

		// Direct chain: exec.Command(...).Run()/.Output()/.CombinedOutput().
		if inner, ok := sel.X.(*ast.CallExpr); ok && isExecCall(inner) {
			switch sel.Sel.Name {
			case "Run", "Output", "CombinedOutput":
				innerSel := inner.Fun.(*ast.SelectorExpr)
				violations = append(violations, fmt.Sprintf(
					"%s: %s() called directly on %s.%s(...), bypassing pkg/sciontool/procreap",
					fset.Position(call.Pos()), sel.Sel.Name, execAlias, innerSel.Sel.Name))
			}
			return true
		}

		if procreapAlias != "" {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == procreapAlias {
				switch sel.Sel.Name {
				case "Gated":
					hasGated = true
				case "RegisterManagedPID":
					hasRegister = true
				case "UnregisterManagedPID":
					hasUnregister = true
				}
				return true
			}
		}

		key, ok := exprKey(sel.X)
		if !ok || !tracked[key] {
			return true
		}
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
	})

	for key, pos := range startPos {
		if _, waited := waitPos[key]; !waited {
			continue
		}
		if hasGated && hasRegister && hasUnregister {
			continue // manual Gated+Register/Unregister pattern (e.g. Supervisor.Run)
		}
		violations = append(violations, fmt.Sprintf(
			"%s: %s.Start() is Wait()ed elsewhere in this file without going through "+
				"procreap.Gated + RegisterManagedPID/UnregisterManagedPID", fset.Position(pos), key))
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
