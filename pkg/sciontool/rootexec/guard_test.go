/*
Copyright 2026 The Scion Authors.
*/

package rootexec

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// guardedDirs are the root-context packages this guard sweeps: every
// package whose code can run as root (or as a dropped child a root process
// forked) somewhere in a container actor's lifecycle. This is the narrow,
// enforceable slice of the wider "never resolve a bare command name against
// an inherited PATH" rule: a static check over exec.Command/
// exec.CommandContext call sites, not a full data-flow prover.
var guardedDirs = []string{
	"cmd/sciontool",
	"cmd/sciontool/commands",
	"pkg/sciontool/substrate",
	"pkg/sciontool/hooks",
	"pkg/sciontool/hooks/dialects",
	"pkg/sciontool/hooks/handlers",
	"pkg/sciontool/metadata",
	"pkg/sciontool/supervisor",
	"pkg/sciontool/services",
}

// execSiteAllowlist lists every exec.Command/exec.CommandContext call site
// in guardedDirs whose command argument this guard's static check cannot
// itself prove routes through rootexec.Resolve (or is otherwise not a
// PATH-searched bare name), keyed by "relative/path.go:line" (the line of
// the call's opening parenthesis) with a one-line reason. Each reason below
// is the record of why that site is exempt — there is no separate document
// to cross-reference.
//
// A revert of any of these call sites back to an actual bare name literal
// is still caught: the allowlist only excuses "this guard cannot prove the
// current expression safe by itself", never "any expression is fine here".
var execSiteAllowlist = map[string]string{
	// Runs before RunInit populates any workload-owned directory, so no
	// workload-influenceable PATH entry exists yet: realigning the "scion"
	// system account's uid/gid.
	"cmd/sciontool/commands/init.go:2248": "before workload setup (host-user realignment); no workload-influenceable PATH entry exists yet",
	"cmd/sciontool/commands/init.go:2253": "before workload setup (host-user realignment); no workload-influenceable PATH entry exists yet",
	"cmd/sciontool/commands/init.go:2381": "before workload setup (direct /etc/passwd,/etc/group sed fallback); no workload-influenceable PATH entry exists yet",
	"cmd/sciontool/commands/init.go:2390": "before workload setup (direct /etc/passwd,/etc/group sed fallback); no workload-influenceable PATH entry exists yet",

	// gitCloneWorkspace's clone-path git calls (including detectDefaultBranch,
	// which it calls into): configureGitCommand sets a Credential to (uid,
	// gid) whenever uid > 0, so the process runs as the workload's own
	// identity in that case. Under RequirePrivilegeDrop, gitCloneWorkspace
	// refuses uid/gid <= 0 before any git command, so these always run under
	// the Credential. Unenforced with uid 0, its scion-user fallback
	// resolves the image account; if that lookup fails, configureGitCommand
	// sets no Credential and git runs with init's own credentials, as at
	// base. Either way the call is not otherwise reachable by a
	// workload-influenceable PATH, which is what this guard itself checks
	// for.
	"cmd/sciontool/commands/init.go:2622": "runs as the workload uid via Credential whenever uid>0",
	"cmd/sciontool/commands/init.go:2717": "runs as the workload uid via Credential whenever uid>0",
	"cmd/sciontool/commands/init.go:2646": "runs as the workload uid via Credential whenever uid>0",
	"cmd/sciontool/commands/init.go:2661": "runs as the workload uid via Credential whenever uid>0",
	"cmd/sciontool/commands/init.go:2731": "runs as the workload uid via Credential whenever uid>0",
	"cmd/sciontool/commands/init.go:2742": "runs as the workload uid via Credential whenever uid>0",
	"cmd/sciontool/commands/init.go:2761": "runs as the workload uid via Credential whenever uid>0",
	"cmd/sciontool/commands/init.go:2788": "runs as the workload uid via Credential whenever uid>0",
	"cmd/sciontool/commands/init.go:2780": "runs as the workload uid via Credential whenever uid>0",
	"cmd/sciontool/commands/init.go:2792": "runs as the workload uid via Credential whenever uid>0",
	"cmd/sciontool/commands/init.go:2802": "runs as the workload uid via Credential whenever uid>0",
	"cmd/sciontool/commands/init.go:3149": "runs as the workload uid via Credential whenever uid>0 (git ls-remote for default-branch detection during clone)",

	// The harness-provision subcommand's own subprocess: under
	// RequirePrivilegeDrop, hooks.buildDroppedProvisionCmd sets Credential
	// on the *parent* fork that execs this subcommand in the first place
	// (or refuses outright if no valid workload uid/gid is available), so
	// the OS process is already the dropped workload uid/gid by the time
	// this code runs. Outside that mode, this subcommand's own process
	// simply inherits whatever credentials the pre-start hook runner used
	// to exec it (root, on a runtime with no privilege boundary to enforce).
	"cmd/sciontool/commands/harness.go:168": "runs as the workload uid under RequirePrivilegeDrop (buildDroppedProvisionCmd drops or refuses); inherits the pre-start hook runner's credentials otherwise",

	// hooks/exec_enforced.go's execViaFd: execScriptPath is a constructed
	// "/proc/self/fd/<n>" string, never a bare name — PATH is never
	// consulted for it, so there is nothing for this guard to resolve
	// through rootexec.
	"pkg/sciontool/hooks/exec_enforced.go:236": "constructed /proc/self/fd path, not a bare name; PATH is never consulted",

	// hooks/lifecycle.go's executeScript, non-enforced branch (returns
	// early via executeScriptEnforced when EnforcePrivilegeDrop is set):
	// path is an absolute path built by the caller, not a bare name — PATH
	// is never consulted for it, so there is nothing for this guard to
	// resolve through rootexec.
	"pkg/sciontool/hooks/lifecycle.go:311": "non-enforced branch (EnforcePrivilegeDrop unset); path is an absolute path, not a bare name",

	// supervisor.Run: args[0] is the operator/harness-selected entrypoint.
	// Run() sets a Credential before Start() whenever UID/GID are supplied,
	// and fails closed (ErrPrivilegeDropRequired) instead of falling
	// through to run as root when RequirePrivilegeDrop is set and they are
	// not — the same Go-level "drop before exec" model rootexec's own doc
	// comment describes as the preferred route: the bare-name lookup
	// happens in the (root) parent using its inherited PATH, and the
	// resulting process runs as the workload's own uid whenever that
	// happens.
	"pkg/sciontool/supervisor/supervisor.go:182": "runs as the workload uid via Credential whenever UID/GID>0, or fails closed under RequirePrivilegeDrop",

	// services.Manager.start: svc.spec.Command[0] comes from a workload-
	// supplied services.yaml. start() itself requires uid/gid>0 (or fails
	// closed with services.ErrPrivilegeDropRequired under
	// requirePrivilegeDrop) before any service is started — the identical
	// Go-level drop-before-exec model as supervisor.Run.
	"pkg/sciontool/services/manager.go:393": "runs as the workload uid via Credential whenever UID/GID>0, or fails closed under RequirePrivilegeDrop",

	// runExec's shPath is assigned a few lines above from execResolve("sh")
	// (production: rootexec.Resolve) and used here as both the exec target
	// and the "-c" interpreter, never a bare name — this guard's static
	// check cannot itself follow that value across the intervening
	// execUserCredential call to confirm it never changes.
	"pkg/sciontool/substrate/exec.go:107": "shPath comes from execResolve (rootexec.Resolve) a few lines above",
}

// aliasKind records what kind of exec constructor a package-level var
// aliases: argIndex is the position of the command-name argument (0 for
// exec.Command-shaped, 1 for exec.CommandContext-shaped, which takes a
// context.Context first).
type aliasKind struct{ argIndex int }

func TestNoRootContextExecUsesABareUnresolvedCommandName(t *testing.T) {
	repoRoot := findRepoRoot(t)

	fset := token.NewFileSet()
	var violations []string
	seenAllowlist := map[string]bool{}

	for _, dir := range guardedDirs {
		absDir := filepath.Join(repoRoot, dir)
		entries, err := os.ReadDir(absDir)
		if err != nil {
			t.Fatalf("read %s: %v", absDir, err)
		}

		// Pass 1: collect this package's own exec.Command/exec.CommandContext
		// aliases (e.g. "var execCommandContext = exec.CommandContext"),
		// across every non-test file in the directory.
		aliases := map[string]aliasKind{}
		files := map[string]*ast.File{}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(absDir, e.Name())
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			files[path] = f
			collectExecAliases(f, aliases)
		}

		// Pass 2: find every exec.Command/exec.CommandContext (direct or
		// aliased) call and classify its command-name argument.
		for path, f := range files {
			relPath, err := filepath.Rel(repoRoot, path)
			if err != nil {
				t.Fatalf("relpath %s: %v", path, err)
			}
			relPath = filepath.ToSlash(relPath)

			funcLikes := collectFuncLikes(f)

			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				argIndex, isExecCall := classifyExecCall(call, aliases)
				if !isExecCall {
					return true
				}
				if len(call.Args) <= argIndex {
					return true
				}
				arg := call.Args[argIndex]
				line := fset.Position(call.Pos()).Line
				siteKey := fmt.Sprintf("%s:%d", relPath, line)

				if isProvenSafe(arg, funcLikes, call.Pos()) {
					return true
				}
				if reason, ok := execSiteAllowlist[siteKey]; ok {
					seenAllowlist[siteKey] = true
					_ = reason
					return true
				}
				violations = append(violations, fmt.Sprintf(
					"%s: exec call's command argument is not provably routed through rootexec.Resolve (or an absolute/literal path), and is not on the allowlist",
					siteKey))
				return true
			})
		}
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Errorf("found %d root-context exec call(s) with an unresolved bare command name:\n%s",
			len(violations), strings.Join(violations, "\n"))
	}

	// Keep the allowlist honest: an entry for a site that no longer exists
	// (the call was removed, moved, or genuinely fixed) should be deleted,
	// not left to silently mask the next real regression at that line.
	var stale []string
	for site := range execSiteAllowlist {
		if !seenAllowlist[site] {
			stale = append(stale, site)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("execSiteAllowlist has %d stale entry/entries (no matching call site found) — remove them:\n%s",
			len(stale), strings.Join(stale, "\n"))
	}
}

// collectExecAliases scans f for top-level "var X = exec.Command" or
// "var X = exec.CommandContext" declarations and records them in aliases.
func collectExecAliases(f *ast.File, aliases map[string]aliasKind) {
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != len(vs.Values) {
				continue
			}
			for i, val := range vs.Values {
				sel, ok := val.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				pkgIdent, ok := sel.X.(*ast.Ident)
				if !ok || pkgIdent.Name != "exec" {
					continue
				}
				switch sel.Sel.Name {
				case "Command":
					aliases[vs.Names[i].Name] = aliasKind{argIndex: 0}
				case "CommandContext":
					aliases[vs.Names[i].Name] = aliasKind{argIndex: 1}
				}
			}
		}
	}
}

// classifyExecCall reports whether call is a (possibly aliased)
// exec.Command/exec.CommandContext invocation, and if so, which argument
// index carries the command name.
func classifyExecCall(call *ast.CallExpr, aliases map[string]aliasKind) (argIndex int, ok bool) {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		pkgIdent, ok := fun.X.(*ast.Ident)
		if !ok || pkgIdent.Name != "exec" {
			return 0, false
		}
		switch fun.Sel.Name {
		case "Command":
			return 0, true
		case "CommandContext":
			return 1, true
		}
		return 0, false
	case *ast.Ident:
		if k, ok := aliases[fun.Name]; ok {
			return k.argIndex, true
		}
		return 0, false
	default:
		return 0, false
	}
}

// funcLike is either a *ast.FuncDecl or a *ast.FuncLit, treated uniformly
// as "a function body with a position range".
type funcLike struct {
	body     *ast.BlockStmt
	pos, end token.Pos
}

// collectFuncLikes returns every function declaration and function literal
// in f, so isProvenSafe can find the SMALLEST one enclosing a given call —
// necessary because this codebase routinely builds an exec.Cmd inside a
// closure (e.g. configureSharedWorkspaceGit's runGitConfig) whose own local
// variables are what a backward scan must see.
func collectFuncLikes(f *ast.File) []funcLike {
	var out []funcLike
	ast.Inspect(f, func(n ast.Node) bool {
		switch fn := n.(type) {
		case *ast.FuncDecl:
			if fn.Body != nil {
				out = append(out, funcLike{body: fn.Body, pos: fn.Body.Pos(), end: fn.Body.End()})
			}
		case *ast.FuncLit:
			out = append(out, funcLike{body: fn.Body, pos: fn.Body.Pos(), end: fn.Body.End()})
		}
		return true
	})
	return out
}

// enclosingFunc finds the smallest funcLike containing pos.
func enclosingFunc(funcLikes []funcLike, pos token.Pos) *funcLike {
	var best *funcLike
	for i := range funcLikes {
		fl := funcLikes[i]
		if fl.pos <= pos && pos <= fl.end {
			if best == nil || (fl.end-fl.pos) < (best.end-best.pos) {
				b := fl
				best = &b
			}
		}
	}
	return best
}

// isProvenSafe reports whether arg (the command-name argument of an exec
// call at position callPos) is one of:
//   - a string literal containing "/" (not a bare name PATH would search for)
//   - an inline call to rootexec.Resolve(...)
//   - an identifier whose nearest preceding assignment, anywhere in the
//     smallest enclosing function/closure, is either an inline
//     rootexec.Resolve(...) call or a string literal containing "/"
//
// Anything else — including an identifier with no local assignment found
// (e.g. a function parameter), a bare string literal, or an identifier
// last assigned from a bare string literal — is NOT proven safe, and the
// call site must be on execSiteAllowlist instead.
func isProvenSafe(arg ast.Expr, funcLikes []funcLike, callPos token.Pos) bool {
	switch e := arg.(type) {
	case *ast.BasicLit:
		return literalIsPathLike(e)
	case *ast.CallExpr:
		return isRootexecResolveCall(e)
	case *ast.Ident:
		fl := enclosingFunc(funcLikes, callPos)
		if fl == nil {
			return false
		}
		rhs := lastAssignmentBefore(fl.body, e.Name, callPos)
		if rhs == nil {
			return false
		}
		switch r := rhs.(type) {
		case *ast.CallExpr:
			return isRootexecResolveCall(r)
		case *ast.BasicLit:
			return literalIsPathLike(r)
		default:
			return false
		}
	default:
		return false
	}
}

// literalIsPathLike reports whether a BasicLit STRING is an absolute path.
// A relative path containing a "/" (e.g. "bin/git" or "./git") is NOT
// path-like by this definition: os/exec still resolves it relative to the
// process's current working directory rather than searching PATH, and a
// workload that controls the cwd a root-context command runs in can plant
// a binary at that relative path exactly as it could plant one on PATH —
// only a leading "/" rules that out.
func literalIsPathLike(lit *ast.BasicLit) bool {
	if lit.Kind != token.STRING {
		return false
	}
	v := strings.Trim(lit.Value, "`\"")
	return filepath.IsAbs(v)
}

// isRootexecResolveCall reports whether call is exactly "rootexec.Resolve(...)".
func isRootexecResolveCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkgIdent, ok := sel.X.(*ast.Ident)
	return ok && pkgIdent.Name == "rootexec" && sel.Sel.Name == "Resolve"
}

// lastAssignmentBefore scans body for the textually-last assignment (":="
// or "=") to name at pos < before, and returns its right-hand side
// expression, or nil if none is found. It does not attempt real scoping —
// only "is there an assignment to this name earlier in this same
// function/closure body" — which is exactly the shape every call site in
// this codebase actually uses (resolve, then use a few lines later in the
// same function).
func lastAssignmentBefore(body *ast.BlockStmt, name string, before token.Pos) ast.Expr {
	var found ast.Expr
	var foundPos token.Pos
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if ok && assign.Pos() < before {
			for i, lhs := range assign.Lhs {
				id, idOk := lhs.(*ast.Ident)
				if !idOk || id.Name != name || i >= len(assign.Rhs) {
					continue
				}
				if assign.Pos() > foundPos {
					found = assign.Rhs[i]
					foundPos = assign.Pos()
				}
			}
		}
		// Always keep descending: a matching assignment can be nested
		// inside an if/for/switch/block at any depth within this function.
		return true
	})
	return found
}

// findRepoRoot walks up from this test file's own directory until it finds
// go.mod, so the guard works regardless of the working directory `go test`
// happens to run it from.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repository root (go.mod) walking up from " + thisFile)
		}
		dir = parent
	}
}

// TestLiteralIsPathLike_BareAndRelativeNamesAreNotPathLike is a synthetic
// negative control proving this guard actually has teeth: a bare command
// name ("sh") and a relative one ("bin/sh", "./sh") are never treated as
// path-like, so a real exec.Command("sh") call site with no allowlist entry
// would be flagged as a violation by the sweep above, not silently waved
// through because its literal happens to contain a slash somewhere. Only a
// leading "/" is proven safe.
func TestLiteralIsPathLike_BareAndRelativeNamesAreNotPathLike(t *testing.T) {
	tests := []struct {
		name string
		lit  string
		want bool
	}{
		{"bare name", `"sh"`, false},
		{"relative with slash", `"bin/sh"`, false},
		{"dot-relative", `"./sh"`, false},
		{"absolute path", `"/bin/sh"`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lit := &ast.BasicLit{Kind: token.STRING, Value: tc.lit}
			if got := literalIsPathLike(lit); got != tc.want {
				t.Errorf("literalIsPathLike(%s) = %v, want %v", tc.lit, got, tc.want)
			}
		})
	}
}

// TestGuardPipeline_BareOrRelativeCommandIsFlagged is a synthetic negative
// test: it runs the exact classify-then-prove-safe pipeline
// TestNoRootContextExecUsesABareUnresolvedCommandName applies to every real
// guarded-directory call site against hand-built exec.Command(...) calls,
// without planting them in a real guarded file (which would trip the live
// guard for real). It covers both a bare name ("sh") and a relative name
// containing a slash ("bin/sh"): a check that merely looks for a slash
// would wrongly treat "bin/sh" as already resolved, even though os/exec
// still resolves a relative name against the process's current working
// directory rather than a fixed location, exactly the PATH-like
// redirection risk this guard exists to catch. filepath.IsAbs correctly
// keeps flagging it as unresolved.
func TestGuardPipeline_BareOrRelativeCommandIsFlagged(t *testing.T) {
	for _, name := range []string{"sh", "bin/sh", "./sh"} {
		t.Run(name, func(t *testing.T) {
			src := `package synthetic

import "os/exec"

func run() {
	exec.Command("` + name + `")
}
`
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "synthetic.go", src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			aliases := map[string]aliasKind{}
			collectExecAliases(f, aliases)
			funcLikes := collectFuncLikes(f)

			var found bool
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				argIndex, isExecCall := classifyExecCall(call, aliases)
				if !isExecCall {
					return true
				}
				found = true
				if len(call.Args) <= argIndex {
					t.Fatalf("exec.Command call has no command-name argument")
				}
				arg := call.Args[argIndex]
				if isProvenSafe(arg, funcLikes, call.Pos()) {
					t.Errorf("isProvenSafe(%v) = true, want false: %q must be flagged, not proven safe", arg, name)
				}
				return true
			})
			if !found {
				t.Fatal("test bug: synthetic source has no exec.Command call for the pipeline to classify")
			}
		})
	}
}
