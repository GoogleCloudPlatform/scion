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
// of packages — which silently misses whatever a future change adds to that
// list's blind spots. Instead, this over-approximates reachability — it
// will also scan code that only some other subcommand (doctor, harness,
// metadata status, provision, ...) uses — and relies on
// execAuditFileAllowlist to name, and justify, each file confirmed to run
// in a different process than runInit's PID 1.
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
	// error-kind constant compared against — none call exec.Cmd. GetHomeDir
	// (used by pkg/sciontool/substrate for the agent-home convention) is a
	// pure string/path join with no exec.Cmd involved either. ParseBoolEnv
	// (pkg/util/env.go, the shared bool-env parser used by init.go and
	// pkg/sciontool/{hub,telemetry,autoexpose}) only reads an environment
	// variable and parses it, with no exec.Cmd. The package's actual
	// exec.Command call sites (pkg/util/git.go) are listed in
	// execAuditFileAllowlist above.
	"pkg/util": {"NormalizeGitRemote": true, "ClassifyGitError": true, "GitErrAuth": true, "GetHomeDir": true, "ParseBoolEnv": true},
	// init.go's only use of pkg/provision: GitTokenCredentialHelper is a
	// string constant (the git credential helper value it writes to the
	// agent's gitconfig); no exec.Cmd is reachable through it.
	"pkg/provision": {"GitTokenCredentialHelper": true},
}

// execAuditTrustedDir is excluded from scanning outright rather than via
// execAuditFileAllowlist: it's the safe implementation this whole guard
// exists to make sure everything else goes through, not a file that avoids
// the race for some other reason. (It's still reachable, and its own
// _test.go files intentionally call raw exec.Cmd methods to drive the
// reaper under test — irrelevant here since only non-test files are
// scanned at all.)
const execAuditTrustedDir = "pkg/sciontool/procreap"

// TestNoRawExecInPID1Path is a regression guard: every exec.Cmd call site
// reachable from sciontool init's PID-1 process while
// procreap.StartReaper's SIGCHLD reaper is active must go through
// pkg/sciontool/procreap's managed helpers (RunManaged/CombinedOutputManaged/
// OutputManaged, or the manual Gated+RegisterManagedPID/UnregisterManagedPID
// pattern used by Supervisor.Run and services.managedService.start), because
// a raw Run/Output/CombinedOutput call — or a raw Start() later Wait()ed —
// races that reaper for the child's exit status. Nothing in Go's type
// system stops a future change from adding a new raw call in this path and
// silently reintroducing that race.
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
	riskyFuncs := execAuditRiskyFuncNames(t, repoRoot)

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

			vs, err = findRiskyFuncCallViolations(absPath, riskyFuncs[dir])
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
			"(or the Gated+RegisterManagedPID/UnregisterManagedPID pattern); route calls into a "+
			"risky package through execAuditSymbolAllowlist, or a same-package reference to a risky "+
			"allowlisted-file function or variable by not referencing it at all; or add an "+
			"execAuditFileAllowlist entry with a citation if the file is genuinely unreachable from "+
			"runInit", len(violations))
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

// execAuditFuncInfo is what execAuditRiskyFuncNames records per top-level,
// non-method function and per package-level var declared with an
// initializer in an execAuditFileAllowlist file, before the fixpoint below
// turns it into a plain risky/not-risky verdict. A var declared without an
// initializer and assigned its value elsewhere (e.g. in an `init` function)
// has no expression here to inspect, so it is never recorded at all — see
// execAuditRiskyFuncNames's doc comment for that limit.
type execAuditFuncInfo struct {
	// direct is true if the function's body, or the var's initializer
	// expression, references os/exec, or references a not-explicitly-
	// approved symbol from an imported risky package (see
	// execAuditRiskyPackageDirs/execAuditSymbolAllowlist) — i.e. it reaches
	// exec.Cmd in zero further same-package hops.
	direct bool
	// refs is every bare identifier the function's body (or the var's
	// initializer) mentions — a superset that includes ordinary local
	// variables, parameters and selector members, not just other top-level
	// names; the fixpoint below only cares whether one of them happens to
	// name an already-risky function or var in the same directory.
	refs map[string]bool
}

// inspectExecAuditRefs walks n — a top-level function's body, or a
// package-level var's initializer expression — and returns the
// execAuditFuncInfo describing whether it directly reaches os/exec or a
// not-explicitly-approved symbol from a risky imported package (via
// aliasToRiskyDir), and every bare identifier it references. This is the one
// piece of inspection logic execAuditRiskyFuncNames uses for both function
// bodies and var initializers, so the two can't drift out of sync — a
// function and a var initializer that reach exec.Cmd the same way (a direct
// exec.alias.X selector, or a selector into a risky imported package) must
// be judged identically.
func inspectExecAuditRefs(n ast.Node, execAlias string, aliasToRiskyDir map[string]string) execAuditFuncInfo {
	info := execAuditFuncInfo{refs: map[string]bool{}}
	ast.Inspect(n, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := v.X.(*ast.Ident); ok {
				if execAlias != "" && id.Name == execAlias {
					info.direct = true
				}
				if riskyDir, ok := aliasToRiskyDir[id.Name]; ok &&
					!execAuditSymbolAllowlist[riskyDir][v.Sel.Name] {
					info.direct = true
				}
			}
		case *ast.Ident:
			info.refs[v.Name] = true
		}
		return true
	})
	return info
}

// execAuditRiskyFuncNames is the same-package counterpart to
// execAuditRiskyPackageDirs/execAuditSymbolAllowlist: for each
// execAuditFileAllowlist file, it finds every top-level, non-method
// function and every package-level var declaration with an initializer
// (e.g. the `*cobra.Command` vars — doctorCmd, provisionCmd, and the rest —
// whose Run/RunE closures call into the risky functions below) that reaches
// os/exec or a risky imported package — directly, or transitively through
// any number of other same-package functions or vars (a wrapper around a
// risky function, a risky function passed around as a value, or a var whose
// initializer references one, is still risky) — and returns their names,
// keyed by directory. A non-allowlisted file elsewhere in the *same
// directory* could otherwise dodge this whole test by referencing one of
// these unqualified (Go's same-package reference syntax) instead of calling
// exec.Command directly — the same shape of backdoor execAuditSymbolAllowlist
// closes across package boundaries, just within one. Keyed by directory,
// since an unqualified reference is only ever resolved within its own
// package: two different directories reusing a name isn't a collision here.
// `init` and `_` can never be referenced by name, so both are excluded from
// the candidate set entirely: Go forbids referencing `init` by name, and `_`
// (the blank identifier, e.g. a `var _ = someType(nil)` interface-
// satisfaction assertion) can never be the target of an unqualified
// reference either. Several allowlisted files each declare an `init`, and a
// future one could declare a blank-identifier var — without this exclusion,
// whichever file's entry a map iteration visited last would silently win
// the shared `byDir[dir]["init"]` (or `byDir[dir]["_"]`) slot, making the
// result depend on random map order the moment any allowlisted `init`/`_`
// became risky.
//
// This is a fixpoint over bare-identifier references, not a call graph: a
// plain function's body, or a var's initializer, referencing a risky name in
// *any* position — not just as a call — is enough, so a function value (`f
// := riskyFunc`), a wrapper (`func w() { riskyFunc() }`), or a var
// referencing one in its initializer (`var v = &cobra.Command{RunE:
// riskyFunc}`) is still caught.
//
// Known limits, none of which exist in this codebase today: methods, and
// types whose methods reach exec, are excluded from the risky set itself (an
// unqualified reference can never resolve to either — Go can only resolve a
// bare identifier to a package-level function or var); a package-level var
// declared without an initializer and assigned its value elsewhere (e.g. in
// an `init` function) is invisible too, since there is no initializer
// expression for `inspectExecAuditRefs` to inspect at the declaration site.
// A reference through any of these would be a silent miss. Separately, a
// local variable, parameter, or struct field that happens to share a name
// with a risky same-package function or var (a "shadow") would be a false
// positive (something extra to justify) rather than a miss — also not
// present today.
func execAuditRiskyFuncNames(t *testing.T, repoRoot string) map[string]map[string]bool {
	t.Helper()
	riskyPkgDirs := execAuditRiskyPackageDirs()

	// byDir[dir][funcName] holds each allowlisted-file function's info,
	// before the fixpoint below resolves it to a risky/not-risky verdict.
	byDir := map[string]map[string]execAuditFuncInfo{}

	for relPath := range execAuditFileAllowlist {
		absPath := filepath.Join(repoRoot, filepath.FromSlash(relPath))
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, absPath, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", relPath, err)
		}
		dir := filepath.ToSlash(filepath.Dir(relPath))
		execAlias := importAlias(file, "os/exec", "exec")

		// aliasToRiskyDir maps this file's own import aliases to the risky
		// package directory they refer to, so a selector into one (e.g.
		// provision.ProvisionShared from a file that imports pkg/provision)
		// counts as direct even though it's a value reference, not exec.X.
		aliasToRiskyDir := map[string]string{}
		for _, imp := range file.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil || !strings.HasPrefix(p, moduleImportPrefix) {
				continue
			}
			relDir := strings.TrimPrefix(p, moduleImportPrefix)
			if !riskyPkgDirs[relDir] {
				continue
			}
			alias := relDir[strings.LastIndex(relDir, "/")+1:]
			if imp.Name != nil {
				alias = imp.Name.Name
			}
			aliasToRiskyDir[alias] = relDir
		}

		recordInfo := func(name string, body ast.Node) {
			if byDir[dir] == nil {
				byDir[dir] = map[string]execAuditFuncInfo{}
			}
			byDir[dir][name] = inspectExecAuditRefs(body, execAlias, aliasToRiskyDir)
		}

		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv != nil || d.Body == nil || d.Name.Name == "init" {
					// Methods are excluded: an unqualified reference can
					// never reach one. init is excluded: it can't be
					// referenced by name at all (see doc comment above).
					continue
				}
				recordInfo(d.Name.Name, d.Body)
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || len(vs.Values) == 0 {
						continue // no initializer to inspect, e.g. `var x int`
					}
					for i, name := range vs.Names {
						if name.Name == "_" {
							// `var _ = ...` (e.g. an interface-satisfaction
							// assertion) can't be referenced by name either —
							// same reason init is excluded above.
							continue
						}
						valueIdx := i
						if len(vs.Values) != len(vs.Names) {
							// e.g. `var a, b = f()`: a single multi-value
							// initializer shared across names. Conservatively
							// apply that one expression's analysis to each.
							valueIdx = 0
						}
						recordInfo(name.Name, vs.Values[valueIdx])
					}
				}
			}
		}
	}

	// Fixpoint per directory: a function is risky if it's direct, or if it
	// references (as a bare identifier, anywhere in its body) any
	// already-risky function's name in the same directory. Loops until a
	// full pass adds nothing new; this always terminates since each
	// directory has a finite set of candidate functions.
	result := map[string]map[string]bool{}
	for dir, funcs := range byDir {
		riskySet := map[string]bool{}
		for name, info := range funcs {
			if info.direct {
				riskySet[name] = true
			}
		}
		for changed := true; changed; {
			changed = false
			for name, info := range funcs {
				if riskySet[name] {
					continue
				}
				for ref := range info.refs {
					if riskySet[ref] {
						riskySet[name] = true
						changed = true
						break
					}
				}
			}
		}
		result[dir] = riskySet
	}
	return result
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

// isExecCmdType reports whether t is the exec.Cmd type or a pointer to it
// (e.g. the type in `var c exec.Cmd` or `var c *exec.Cmd`).
func isExecCmdType(t ast.Expr, execAlias string) bool {
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	sel, ok := t.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == execAlias && sel.Sel.Name == "Cmd"
}

// isExecCmdValue reports whether e directly constructs an exec.Cmd: a call
// to exec.Command/CommandContext, a call to new(exec.Cmd), or an
// exec.Cmd{...} composite literal (bare, or address-of as
// `&exec.Cmd{...}`, the only form Go allows calling a method on inline).
// It does not cover a zero-value `var c exec.Cmd` with no initializer —
// findRawExecViolations' pass 1 handles that case separately, since it
// needs the declaration's type, not its (absent) value.
func isExecCmdValue(e ast.Expr, execAlias string) bool {
	switch v := unwrapParen(e).(type) {
	case *ast.CallExpr:
		if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "new" && len(v.Args) == 1 {
			return isExecCmdType(v.Args[0], execAlias)
		}
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
		return isExecCmdType(v.Type, execAlias)
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
// or chained directly off a freshly-constructed exec.Cmd value — including
// a zero-value `var c exec.Cmd`/`var c *exec.Cmd` or `new(exec.Cmd)`, not
// just exec.Command(...) or an `exec.Cmd{...}` literal), or a tracked
// exec.Cmd's Start() call that is also Wait()ed (directly, or via its
// embedded os.Process.Wait(), which reaps the child the same way) somewhere
// in the file without a matching procreap.Gated +
// RegisterManagedPID(<key>.Process.Pid, ...) / UnregisterManagedPID(<same
// key>.Process.Pid, ...) for that same key.
//
// Known precision gaps, all accepted for a parse-only heuristic: (1)
// procreap.Gated's presence is checked file-wide, not tied to a specific
// Start() call, because correlating it to one would require inspecting the
// body of the closure passed to Gated; RegisterManagedPID/UnregisterManagedPID
// are checked per-key so a second, ungated exec.Cmd added to an already-
// gated file is still caught. (2) An exec.Cmd returned from a helper
// function (same-file or cross-package) rather than constructed inline is
// not tracked at all — findSymbolAllowlistViolations and
// findRiskyFuncCallViolations catch the cross-package and same-package
// versions of this for the specific packages/functions they restrict, but a
// same-file `func mkCmd() *exec.Cmd { ... }` helper local to a scanned file
// is not caught here. (3) Start()/Wait() correlation is by textual key
// within one file only: a struct field Start()ed in one file and Wait()ed
// in another, or reached via two methods with different receiver names
// (`a.cmd.Start()` in one, `b.cmd.Wait()` in the other, same underlying
// field), is not tracked as the same key and so isn't caught.
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
			if len(v.Values) == 0 {
				// `var c exec.Cmd` (or `var c *exec.Cmd`): a zero-value/nil
				// declaration with no initializer expression for
				// trackAssign to inspect — track by declared type instead.
				if v.Type != nil && isExecCmdType(v.Type, execAlias) {
					for _, name := range v.Names {
						tracked[name.Name] = true
					}
				}
				return true
			}
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
					"%s: %s() called directly on an exec.Cmd value instead of going through "+
						"pkg/sciontool/procreap",
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
// nothing from it is approved yet. A dot or blank import of a risky package
// is flagged outright, since a dot import (`import . ".../pkg/util"`) would
// make its calls unqualified identifiers this function's alias-matching
// can't see at all, silently defeating the restriction.
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

		if imp.Name != nil && (imp.Name.Name == "." || imp.Name.Name == "_") {
			violations = append(violations, fmt.Sprintf(
				"%s: %q import of risky package %q defeats execAuditSymbolAllowlist's alias-based "+
					"check; import it normally (qualified) instead",
				fset.Position(imp.Pos()), imp.Name.Name, relDir))
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

// findRiskyFuncCallViolations parses a single Go source file and flags any
// bare (unqualified) reference to a name in riskyFuncs — the same-package
// counterpart to findSymbolAllowlistViolations's cross-package check. Go
// only resolves an unqualified identifier within the same package, so a
// hit here means this file (which the caller has already confirmed is not
// itself in execAuditFileAllowlist) references a function or package-level
// var that execAuditRiskyFuncNames determined (directly, or transitively
// through other same-package functions or vars) reaches os/exec or a risky
// imported package — the same shape of backdoor as calling into a risky
// imported package, just within one package instead of across two. This
// flags any *ast.Ident, not just a CallExpr.Fun, so a function value (`f :=
// riskyFunc`), a wrapper's own reference, or a reference to a risky var
// (e.g. `doctorCmd.Run(doctorCmd, nil)`) is caught, not just a direct
// `riskyFunc(...)` call — see execAuditRiskyFuncNames's doc comment for the
// (empty today) false-positive risk this accepts in exchange.
func findRiskyFuncCallViolations(path string, riskyFuncs map[string]bool) ([]string, error) {
	if len(riskyFuncs) == 0 {
		return nil, nil
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}

	var violations []string
	ast.Inspect(file, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || !riskyFuncs[id.Name] {
			return true
		}
		violations = append(violations, fmt.Sprintf(
			"%s: references %s, a same-package function or variable that (transitively) reaches "+
				"os/exec or a risky package (see execAuditFileAllowlist/execAuditRiskyFuncNames); "+
				"route through pkg/sciontool/procreap instead, or justify this reference explicitly",
			fset.Position(id.Pos()), id.Name))
		return true
	})
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
