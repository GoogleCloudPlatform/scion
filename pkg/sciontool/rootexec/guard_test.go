/*
Copyright 2026 The Scion Authors.
*/

package rootexec

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/scanner"
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

// execSite identifies one exec.Command/exec.CommandContext call site
// without reference to its line number, so unrelated edits elsewhere in the
// file (or gofmt reflowing the call itself) never invalidate an entry:
//
//   - File is the repo-relative, slash-separated path.
//   - Func is the enclosing top-level declaration: "name" for a function,
//     "Recv.name" for a method (pointer receivers drop the "*"). A call
//     inside a closure is attributed to the top-level function containing
//     the closure; a call in a package-level var initializer uses the var's
//     name. Names Go lets a file repeat (func init, blank funcs
//     "func _()", blank methods "func (T) _()", and blank "_" vars and
//     consts) carry their ordinal among same-named declarations in file
//     order ("init#1", "init#2", "_#1", "T._#1"; see enclosingDeclName),
//     so moving a call from one init into another is also a change of
//     Func.
//   - Call is the call expression's normalized text (see
//     normalizeCallText): the Go tokens of the call, with comments, layout
//     and optional trailing commas/semicolons stripped, one space after each
//     comma and semicolon, and a space only where two tokens would otherwise
//     fuse. So `exec.Command("git",\n\t"init", p,\n)` and
//     `exec.Command("git", "init", p)` normalize identically, but any change
//     to the tokens themselves (a different command, argument or variable)
//     does not.
//   - Nth disambiguates textually identical calls within the same Func: 0
//     for the first in source order, 1 for the second, and so on. It is
//     almost always omitted, which is what makes a copy-pasted duplicate of
//     a listed call a new, unlisted site.
type execSite struct {
	File string
	Func string
	Call string
	Nth  int
}

func (s execSite) String() string {
	out := fmt.Sprintf("%s: %s: %s", s.File, s.Func, s.Call)
	if s.Nth > 0 {
		out += fmt.Sprintf(" (occurrence %d)", s.Nth)
	}
	return out
}

// Reasons shared by several execSiteAllowlist entries.
const (
	reasonHostUserRealign  = "before workload setup (host-user realignment); no workload-influenceable PATH entry exists yet"
	reasonSedFallback      = "before workload setup (direct /etc/passwd,/etc/group sed fallback); no workload-influenceable PATH entry exists yet"
	reasonCloneCredential  = "runs as the workload uid via Credential whenever uid>0"
	reasonDropOrFailClosed = "runs as the workload uid via Credential whenever UID/GID>0, or fails closed under RequirePrivilegeDrop"
)

// execSiteAllowlist lists every exec.Command/exec.CommandContext call site
// in guardedDirs whose command argument this guard's static check cannot
// itself prove routes through rootexec.Resolve (or is otherwise not a
// PATH-searched bare name), keyed by execSite (file + enclosing function +
// normalized call text) with a one-line reason. Each reason below is the
// record of why that site is exempt — there is no separate document to
// cross-reference.
//
// A revert of any of these call sites back to an actual bare name literal
// is still caught: the allowlist only excuses "this guard cannot prove the
// current expression safe by itself", never "any expression is fine here".
// Changing a listed call's text, moving it into a different function, or
// adding a second identical call all produce an unlisted site.
var execSiteAllowlist = map[execSite]string{
	// Runs before RunInit populates any workload-owned directory, so no
	// workload-influenceable PATH entry exists yet: realigning the "scion"
	// system account's uid/gid.
	{
		File: "cmd/sciontool/commands/init.go",
		Func: "adjustScionUser",
		Call: `exec.Command("groupmod", "-o", "-g", hostGID, "scion")`,
	}: reasonHostUserRealign,
	{
		File: "cmd/sciontool/commands/init.go",
		Func: "adjustScionUser",
		Call: `exec.Command("usermod", "-o", "-u", hostUID, "-g", hostGID, "scion")`,
	}: reasonHostUserRealign,
	{
		File: "cmd/sciontool/commands/init.go",
		Func: "directSetUIDAt",
		Call: "exec.Command(\"sed\", \"-i\", \"-E\", fmt.Sprintf(`s/^(%s:x:)[0-9]+:/\\1%s:/`, username, newGID), groupPath)",
	}: reasonSedFallback,
	{
		File: "cmd/sciontool/commands/init.go",
		Func: "directSetUIDAt",
		Call: "exec.Command(\"sed\", \"-i\", \"-E\", fmt.Sprintf(`s/^(%s:x:)[0-9]+:[0-9]+:/\\1%s:%s:/`, username, newUID, newGID), passwdPath)",
	}: reasonSedFallback,

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
	{
		File: "cmd/sciontool/commands/init.go",
		Func: "gitCloneWorkspace",
		Call: `exec.Command("git", "init", workspacePath)`,
	}: reasonCloneCredential,
	{
		File: "cmd/sciontool/commands/init.go",
		Func: "gitCloneWorkspace",
		Call: `exec.Command("git", "-C", workspacePath, "remote", "add", "origin", authURL)`,
	}: reasonCloneCredential,
	{
		File: "cmd/sciontool/commands/init.go",
		Func: "gitCloneWorkspace",
		Call: `exec.Command("git", fetchArgs...)`,
	}: reasonCloneCredential,
	{
		File: "cmd/sciontool/commands/init.go",
		Func: "gitCloneWorkspace",
		Call: `exec.Command("git", checkoutArgs...)`,
	}: reasonCloneCredential,
	{
		File: "cmd/sciontool/commands/init.go",
		Func: "gitCloneWorkspace",
		Call: `exec.Command("git", "-C", workspacePath, "config", cfg.key, cfg.value)`,
	}: reasonCloneCredential,
	{
		File: "cmd/sciontool/commands/init.go",
		Func: "gitCloneWorkspace",
		Call: `exec.Command("git", "-C", workspacePath, "remote", "set-url", "origin", buildAuthenticatedURL(cloneURL, ""))`,
	}: reasonCloneCredential,
	{
		File: "cmd/sciontool/commands/init.go",
		Func: "gitCloneWorkspace",
		Call: `exec.Command("git", "config", "--file", gitconfigPath, "credential.helper", credentialHelper)`,
	}: reasonCloneCredential,
	{
		File: "cmd/sciontool/commands/init.go",
		Func: "gitCloneWorkspace",
		Call: `exec.Command("git", "-C", workspacePath, "checkout", branchName)`,
	}: reasonCloneCredential,
	{
		File: "cmd/sciontool/commands/init.go",
		Func: "gitCloneWorkspace",
		Call: `exec.Command("git", "-C", workspacePath, "fetch", "origin", branchName)`,
	}: reasonCloneCredential,
	{
		File: "cmd/sciontool/commands/init.go",
		Func: "gitCloneWorkspace",
		Call: `exec.Command("git", "-C", workspacePath, "checkout", "-b", branchName, "origin/"+branchName)`,
	}: reasonCloneCredential,
	{
		File: "cmd/sciontool/commands/init.go",
		Func: "gitCloneWorkspace",
		Call: `exec.Command("git", "-C", workspacePath, "checkout", "-b", branchName)`,
	}: reasonCloneCredential,
	{
		File: "cmd/sciontool/commands/init.go",
		Func: "detectDefaultBranch",
		Call: `exec.Command("git", "-C", workspacePath, "ls-remote", "--symref", "origin", "HEAD")`,
	}: "runs as the workload uid via Credential whenever uid>0 (git ls-remote for default-branch detection during clone)",

	// The harness-provision subcommand's own subprocess: under
	// RequirePrivilegeDrop, hooks.buildDroppedProvisionCmd sets Credential
	// on the *parent* fork that execs this subcommand in the first place
	// (or refuses outright if no valid workload uid/gid is available), so
	// the OS process is already the dropped workload uid/gid by the time
	// this code runs. Outside that mode, this subcommand's own process
	// simply inherits whatever credentials the pre-start hook runner used
	// to exec it (root, on a runtime with no privilege boundary to enforce).
	{
		File: "cmd/sciontool/commands/harness.go",
		Func: "runHarnessProvisionSteps",
		Call: `exec.CommandContext(runCtx, prov.Command[0], prov.Command[1:]...)`,
	}: "runs as the workload uid under RequirePrivilegeDrop (buildDroppedProvisionCmd drops or refuses); inherits the pre-start hook runner's credentials otherwise",

	// hooks/exec_enforced.go's execViaFd: execScriptPath is a constructed
	// "/proc/self/fd/<n>" string, never a bare name — PATH is never
	// consulted for it, so there is nothing for this guard to resolve
	// through rootexec.
	{
		File: "pkg/sciontool/hooks/exec_enforced.go",
		Func: "execViaFd",
		Call: `exec.Command(execScriptPath)`,
	}: "constructed /proc/self/fd path, not a bare name; PATH is never consulted",

	// hooks/lifecycle.go's executeScript, non-enforced branch (returns
	// early via executeScriptEnforced when EnforcePrivilegeDrop is set):
	// path is an absolute path built by the caller, not a bare name — PATH
	// is never consulted for it, so there is nothing for this guard to
	// resolve through rootexec.
	{
		File: "pkg/sciontool/hooks/lifecycle.go",
		Func: "LifecycleManager.executeScript",
		Call: `exec.Command(path)`,
	}: "non-enforced branch (EnforcePrivilegeDrop unset); path is an absolute path, not a bare name",

	// supervisor.Run: args[0] is the operator/harness-selected entrypoint.
	// Run() sets a Credential before Start() whenever UID/GID are supplied,
	// and fails closed (ErrPrivilegeDropRequired) instead of falling
	// through to run as root when RequirePrivilegeDrop is set and they are
	// not — the same Go-level "drop before exec" model rootexec's own doc
	// comment describes as the preferred route: the bare-name lookup
	// happens in the (root) parent using its inherited PATH, and the
	// resulting process runs as the workload's own uid whenever that
	// happens.
	{
		File: "pkg/sciontool/supervisor/supervisor.go",
		Func: "Supervisor.Run",
		Call: `exec.Command(args[0], args[1:]...)`,
	}: reasonDropOrFailClosed,

	// services' (*managedService).start (services/manager.go):
	// svc.spec.Command[0] comes from a workload-supplied services.yaml.
	// start() itself requires uid/gid>0 (or fails closed with
	// services.ErrPrivilegeDropRequired under requirePrivilegeDrop) before
	// any service is started — the identical Go-level drop-before-exec
	// model as supervisor.Run.
	{
		File: "pkg/sciontool/services/manager.go",
		Func: "managedService.start",
		Call: `exec.Command(svc.spec.Command[0], svc.spec.Command[1:]...)`,
	}: reasonDropOrFailClosed,

	// runExec's shPath is assigned a few lines above from execResolve("sh")
	// (production: rootexec.Resolve) and used here as both the exec target
	// and the "-c" interpreter, never a bare name — this guard's static
	// check cannot itself follow that value across the intervening
	// execUserCredential call to confirm it never changes.
	{
		File: "pkg/sciontool/substrate/exec.go",
		Func: "runExec",
		Call: `execCommandContext(runCtx, shPath, "-c", cmdString)`,
	}: "shPath comes from execResolve (rootexec.Resolve) a few lines above",
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
	seenAllowlist := map[execSite]bool{}

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

			v, seen := checkFileExecSites(fset, relPath, f, aliases, execSiteAllowlist)
			violations = append(violations, v...)
			for _, s := range seen {
				seenAllowlist[s] = true
			}
		}
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Errorf("found %d root-context exec call(s) with an unresolved bare command name:\n%s",
			len(violations), strings.Join(violations, "\n"))
	}

	// Keep the allowlist honest: an entry for a site that no longer exists
	// (the call was removed, moved to another function, rewritten, or
	// genuinely fixed) should be deleted, not left to silently mask the
	// next real regression.
	if stale := staleAllowlistEntries(execSiteAllowlist, seenAllowlist); len(stale) > 0 {
		t.Errorf("execSiteAllowlist has %d stale entry/entries (no matching call site found) — remove or update them:\n%s",
			len(stale), strings.Join(stale, "\n"))
	}
}

// checkFileExecSites runs the guard over one parsed file: every (possibly
// aliased) exec.Command/exec.CommandContext call whose command argument is
// not provably safe must match an allowlist entry, or it is reported as a
// violation. It returns the violations and the allowlist entries matched.
func checkFileExecSites(fset *token.FileSet, relPath string, f *ast.File, aliases map[string]aliasKind, allowlist map[execSite]string) (violations []string, seen []execSite) {
	funcLikes := collectFuncLikes(f)
	occurrences := map[execSite]int{}

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

		// ast.Inspect visits nodes in source order, so Nth counts
		// textually identical calls in the same function top to bottom.
		base := execSite{
			File: relPath,
			Func: enclosingDeclName(f, call.Pos()),
			Call: normalizeCallText(fset, call),
		}
		site := base
		site.Nth = occurrences[base]
		occurrences[base]++

		if isProvenSafe(call.Args[argIndex], funcLikes, call.Pos()) {
			return true
		}
		if _, ok := allowlist[site]; ok {
			seen = append(seen, site)
			return true
		}
		violations = append(violations, fmt.Sprintf(
			"%s (line %d): exec call's command argument is not provably routed through rootexec.Resolve (or an absolute/literal path), and is not on the allowlist",
			site, fset.Position(call.Pos()).Line))
		return true
	})
	return violations, seen
}

// staleAllowlistEntries returns, sorted, every allowlist entry not in seen.
func staleAllowlistEntries(allowlist map[execSite]string, seen map[execSite]bool) []string {
	var stale []string
	for site := range allowlist {
		if !seen[site] {
			stale = append(stale, site.String())
		}
	}
	sort.Strings(stale)
	return stale
}

// enclosingDeclName names the top-level declaration containing pos:
// "name" for a function, "Recv.name" for a method (with any "*" and type
// parameters dropped from the receiver), the first declared name for a
// package-level var/const initializer, or "<file>" if pos is in none.
//
// Go lets these top-level names repeat within one file, so each carries
// its 1-based ordinal among same-named declarations in file order:
//
//   - func init: "init#1", "init#2", ...
//   - blank funcs func _() and blank var/const specs (first name _): one
//     shared counter, "_#1", "_#2", ...
//   - blank methods func (T) _(): one counter per receiver base type
//     (value and pointer receivers share it), "T._#1", "T._#2", ...
//
// Every other name that can hold a call is unique per file (or per
// receiver type), so it needs no ordinal: non-blank funcs, non-blank
// methods (including a method named init), and value specs whose first
// name is not _. Repeatable names that cannot hold an exec call (blank
// types, blank imports) never reach here. A multi-name value spec is one
// declaration, keyed by its first name, so swapping calls between its
// values is a reorder within one Func, like reordering a function body.
//
// The ordinal counts declarations, not lines, so it is stable across line
// shifts and reflows, yet moving a call from one repeated declaration into
// another changes its key. Adding or removing an earlier declaration of
// the same name renumbers the later ones, which errs on the strict side
// (stale entry plus violation) and is rare.
func enclosingDeclName(f *ast.File, pos token.Pos) string {
	repeats := map[string]int{} // ordinal so far per repeatable key
	numbered := func(key string) string {
		repeats[key]++
		return fmt.Sprintf("%s#%d", key, repeats[key])
	}
	ordinal := func(name string) string {
		if name != "init" && name != "_" {
			return name
		}
		return numbered(name)
	}
	for _, decl := range f.Decls {
		contains := pos >= decl.Pos() && pos <= decl.End()
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv != nil && len(d.Recv.List) > 0 {
				name := d.Name.Name
				if recv := receiverTypeName(d.Recv.List[0].Type); recv != "" {
					name = recv + "." + name
				}
				// Only blank method names repeat; a method named init is
				// an ordinary, unique method.
				if d.Name.Name == "_" {
					name = numbered(name)
				}
				if contains {
					return name
				}
				continue
			}
			if name := ordinal(d.Name.Name); contains {
				return name
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) == 0 {
					continue
				}
				if name := ordinal(vs.Names[0].Name); contains && pos >= vs.Pos() && pos <= vs.End() {
					return name
				}
			}
		}
	}
	return "<file>"
}

// receiverTypeName strips pointer and type-parameter syntax from a method
// receiver type, returning the bare type name.
func receiverTypeName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(e.X)
	case *ast.IndexExpr:
		return receiverTypeName(e.X)
	case *ast.IndexListExpr:
		return receiverTypeName(e.X)
	case *ast.ParenExpr:
		return receiverTypeName(e.X)
	case *ast.Ident:
		return e.Name
	}
	return ""
}

// normalizeCallText renders call as a canonical single line that depends
// only on its Go token sequence, not on how the tokens are laid out:
//
//  1. The call is printed with go/printer and re-tokenized with go/scanner
//     (comments are skipped).
//  2. Every semicolon — explicit, or inserted automatically at a line break
//     — becomes ";". A semicolon directly before a closing bracket or at the
//     very end is dropped (Go makes it optional there), as is a comma
//     directly before a closing bracket. So statement boundaries inside a
//     func-literal argument are kept whether they were written as line
//     breaks or as ";", and `func(){ x(); y() }` on one line and the same
//     body across several lines both normalize to `func(){x(); y()}`.
//  3. The tokens are joined with exactly one space after each comma and
//     semicolon, and otherwise with no space unless the two adjacent tokens
//     would re-scan as something else when concatenated (an identifier and
//     a keyword, `&` `&` vs `&&`, `-` `-` vs `--`, `/` `*` vs a comment
//     opener, ...), in which case they get a single space.
//
// Step 3 makes the result re-scan to exactly the token sequence it was built
// from, so two different token sequences never share a key, while gofmt
// reflowing the call (or a human wrapping its arguments) leaves it unchanged.
func normalizeCallText(fset *token.FileSet, call *ast.CallExpr) string {
	var printed bytes.Buffer
	if err := printer.Fprint(&printed, fset, call); err != nil {
		// Printing an in-memory AST node into a buffer does not fail in
		// practice; fall back to something that still never matches a
		// real allowlist entry.
		return fmt.Sprintf("<unprintable call: %v>", err)
	}

	toks := scanTokens(printed.Bytes())
	var kept []scannedToken
	for i, t := range toks {
		if t.tok == token.SEMICOLON || t.tok == token.COMMA {
			if i+1 == len(toks) {
				continue // trailing (automatic) semicolon at the end
			}
			switch toks[i+1].tok {
			case token.RPAREN, token.RBRACK, token.RBRACE:
				continue // optional separator before a closing bracket
			}
		}
		kept = append(kept, t)
	}

	var b strings.Builder
	for i, t := range kept {
		if i > 0 {
			prev := kept[i-1]
			if prev.tok == token.COMMA || prev.tok == token.SEMICOLON || !scansApart(prev, t) {
				b.WriteByte(' ')
			}
		}
		b.WriteString(t.lit)
	}
	return b.String()
}

// scannedToken is one go/scanner token with its source text: the literal
// for identifiers, keywords and literals, ";" for any semicolon, and the
// operator's spelling otherwise.
type scannedToken struct {
	tok token.Token
	lit string
}

// scanTokens tokenizes src with go/scanner, skipping comments and spelling
// every semicolon (explicit or automatically inserted) as ";".
func scanTokens(src []byte) []scannedToken {
	var toks []scannedToken
	tf := token.NewFileSet().AddFile("", -1, len(src))
	var s scanner.Scanner
	s.Init(tf, src, nil, 0) // mode 0: comments are skipped
	for {
		_, tk, lit := s.Scan()
		if tk == token.EOF {
			return toks
		}
		switch {
		case tk == token.SEMICOLON:
			lit = ";"
		case lit == "":
			lit = tk.String()
		}
		toks = append(toks, scannedToken{tk, lit})
	}
}

// scansApart reports whether a's text immediately followed by b's text
// re-scans as exactly the two tokens a and b, i.e. whether they can be
// written with no space between them without fusing into something else.
func scansApart(a, b scannedToken) bool {
	got := scanTokens([]byte(a.lit + b.lit))
	// Drop the semicolon the scanner inserts at EOF after an identifier,
	// literal, or closing bracket; it is not part of either token.
	if n := len(got); n > 0 && got[n-1].tok == token.SEMICOLON && b.tok != token.SEMICOLON {
		got = got[:n-1]
	}
	return len(got) == 2 && got[0] == a && got[1] == b
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

// runSyntheticGuard runs checkFileExecSites over src (as file "synthetic.go")
// against allowlist, returning the violations and stale allowlist entries
// exactly as TestNoRootContextExecUsesABareUnresolvedCommandName computes
// them for real guarded files.
func runSyntheticGuard(t *testing.T, src string, allowlist map[execSite]string) (violations, stale []string) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, src)
	}
	aliases := map[string]aliasKind{}
	collectExecAliases(f, aliases)
	violations, seenList := checkFileExecSites(fset, "synthetic.go", f, aliases, allowlist)
	seen := map[execSite]bool{}
	for _, s := range seenList {
		seen[s] = true
	}
	return violations, staleAllowlistEntries(allowlist, seen)
}

// syntheticInits has two init funcs, two blank vars and two blank methods
// on T, one of each holding an unprovable exec call;
// syntheticInitsAllowlist lists all three calls.
const syntheticInits = "package synthetic\n\nimport (\n\t\"os\"\n\t\"os/exec\"\n)\n\n" +
	"type T struct{}\n\n" +
	"func init() {\n\t_ = exec.Command(os.Args[1])\n}\n\nfunc init() {}\n\n" +
	"var _ = exec.Command(os.Args[2])\n\nvar _ = 0\n\n" +
	"func (T) _() {\n\t_ = exec.Command(os.Args[3])\n}\n\nfunc (*T) _() {}\n"

// syntheticBlankFuncs is syntheticInits with its two blank vars replaced
// by two blank funcs; func _ shares the blank vars' counter, so
// syntheticInitsAllowlist still lists every call.
var syntheticBlankFuncs = strings.Replace(syntheticInits,
	"var _ = exec.Command(os.Args[2])\n\nvar _ = 0\n",
	"func _() {\n\t_ = exec.Command(os.Args[2])\n}\n\nfunc _() {}\n", 1)

var syntheticInitsAllowlist = map[execSite]string{
	{File: "synthetic.go", Func: "init#1", Call: `exec.Command(os.Args[1])`}: "synthetic",
	{File: "synthetic.go", Func: "_#1", Call: `exec.Command(os.Args[2])`}:    "synthetic",
	{File: "synthetic.go", Func: "T._#1", Call: `exec.Command(os.Args[3])`}:  "synthetic",
}

// syntheticAllowlist is a one-entry allowlist for syntheticBase's single
// unprovable exec call (its command name is a parameter).
var syntheticAllowlist = map[execSite]string{
	{
		File: "synthetic.go",
		Func: "runner.run",
		Call: `exec.Command(name, "-x", arg)`,
	}: "synthetic",
}

const syntheticBase = `package synthetic

import "os/exec"

type runner struct{}

func (r *runner) run(name, arg string) {
	_ = exec.Command(name, "-x", arg)
}

func other(name, arg string) {}
`

// TestExecSiteAllowlist_KeyIgnoresLineShiftsAndFormatting proves the
// allowlist key is not line-based: inserting blank lines and comments
// above a listed call, and reflowing the call itself across lines with a
// trailing comma and an inline comment, still matches the same entry.
func TestExecSiteAllowlist_KeyIgnoresLineShiftsAndFormatting(t *testing.T) {
	variants := map[string]string{
		"unchanged": syntheticBase,
		"blank lines and comments above": strings.Replace(syntheticBase,
			"type runner struct{}",
			"\n\n\n// An unrelated doc comment.\n// Spanning lines.\n\ntype runner struct{}\n\n\n", 1),
		"call reflowed by hand": strings.Replace(syntheticBase,
			`_ = exec.Command(name, "-x", arg)`,
			"_ = exec.Command(\n\t\tname, // the command\n\t\t\"-x\",\n\t\targ,\n\t)", 1),
	}
	initVariants := map[string]string{
		"repeated init/_ decls unchanged": syntheticInits,
		"repeated blank funcs unchanged":  syntheticBlankFuncs,
		"repeated init/_ decls shifted and reflowed": strings.Replace(strings.Replace(strings.Replace(syntheticInits,
			"func init() {}", "// A doc comment.\n\n\nfunc init() {\n}", 1),
			"_ = exec.Command(os.Args[1])", "_ = exec.Command(\n\t\tos.Args[1], // why\n\t)", 1),
			"func (*T) _() {}", "// Another doc comment.\n\nfunc (*T) _() {\n}", 1),
	}
	for name, src := range initVariants {
		t.Run(name, func(t *testing.T) {
			violations, stale := runSyntheticGuard(t, src, syntheticInitsAllowlist)
			if len(violations) != 0 || len(stale) != 0 {
				t.Errorf("want listed sites to still match; violations=%v stale=%v", violations, stale)
			}
		})
	}
	for name, src := range variants {
		t.Run(name, func(t *testing.T) {
			violations, stale := runSyntheticGuard(t, src, syntheticAllowlist)
			if len(violations) != 0 || len(stale) != 0 {
				t.Errorf("want listed site to still match; violations=%v stale=%v", violations, stale)
			}
		})
	}
}

// TestExecSiteAllowlist_EnclosingFuncAttribution pins which top-level
// declaration a call is keyed under: a call inside a func literal belongs to
// the function (or method) containing the literal, and a call in a
// package-level var initializer belongs to the var — the first declared name
// for a multi-name spec, for every value in it.
func TestExecSiteAllowlist_EnclosingFuncAttribution(t *testing.T) {
	const header = "package synthetic\n\nimport (\n\t\"os\"\n\t\"os/exec\"\n)\n\ntype runner struct{}\n\n"
	tests := []struct {
		name  string
		decls string
		sites []execSite // Call and Func; File is filled in
	}{
		{
			name:  "closure in a function",
			decls: "func outer(name string) {\n\tf := func() {\n\t\t_ = exec.Command(name)\n\t}\n\tf()\n}\n",
			sites: []execSite{{Func: "outer", Call: `exec.Command(name)`}},
		},
		{
			name:  "nested closures in a method",
			decls: "func (r *runner) run(name string) {\n\tgo func() {\n\t\tdefer func() {\n\t\t\t_ = exec.Command(name)\n\t\t}()\n\t}()\n}\n",
			sites: []execSite{{Func: "runner.run", Call: `exec.Command(name)`}},
		},
		{
			name:  "package-level var initializer",
			decls: "var cmd = exec.Command(os.Getenv(\"X\"))\n",
			sites: []execSite{{Func: "cmd", Call: `exec.Command(os.Getenv("X"))`}},
		},
		{
			name:  "closure in a package-level var initializer",
			decls: "var start = func(name string) *exec.Cmd {\n\treturn exec.Command(name)\n}\n",
			sites: []execSite{{Func: "start", Call: `exec.Command(name)`}},
		},
		{
			name:  "multi-name var spec uses the first name for every value",
			decls: "var (\n\tunrelated = 1\n\ta, b = exec.Command(os.Args[1]), exec.Command(os.Args[2])\n)\n",
			sites: []execSite{
				{Func: "a", Call: `exec.Command(os.Args[1])`},
				{Func: "a", Call: `exec.Command(os.Args[2])`},
			},
		},
		{
			name:  "repeated init funcs are keyed by ordinal",
			decls: "func init() {}\n\nfunc helper() {}\n\nfunc init() {\n\t_ = exec.Command(os.Args[1])\n}\n",
			sites: []execSite{{Func: "init#2", Call: `exec.Command(os.Args[1])`}},
		},
		{
			name:  "repeated blank vars are keyed by ordinal",
			decls: "var _ = 1\n\nvar (\n\tother = 2\n\t_     = exec.Command(os.Args[1])\n)\n",
			sites: []execSite{{Func: "_#2", Call: `exec.Command(os.Args[1])`}},
		},
		{
			name:  "blank funcs and blank vars/consts share one ordinal",
			decls: "const _ = 1\n\nfunc _() {}\n\nvar _, _ = 2, 3\n\nfunc _() {\n\t_ = exec.Command(os.Args[1])\n}\n",
			sites: []execSite{{Func: "_#4", Call: `exec.Command(os.Args[1])`}},
		},
		{
			name: "blank methods are keyed by ordinal per receiver type",
			decls: "type other struct{}\n\nfunc (runner) _() {}\n\nfunc (other) _() {}\n\nfunc _() {}\n\n" +
				"func (r *runner) _() {\n\t_ = exec.Command(os.Args[1])\n}\n\nfunc (other) _() {\n\t_ = exec.Command(os.Args[2])\n}\n",
			sites: []execSite{
				{Func: "runner._#2", Call: `exec.Command(os.Args[1])`},
				{Func: "other._#2", Call: `exec.Command(os.Args[2])`},
			},
		},
		{
			name:  "a method named init is not numbered",
			decls: "func init() {}\n\nfunc (runner) init() {\n\t_ = exec.Command(os.Args[1])\n}\n",
			sites: []execSite{{Func: "runner.init", Call: `exec.Command(os.Args[1])`}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			allowlist := map[execSite]string{}
			for _, s := range tc.sites {
				s.File = "synthetic.go"
				allowlist[s] = "synthetic"
			}
			violations, stale := runSyntheticGuard(t, header+tc.decls, allowlist)
			if len(violations) != 0 || len(stale) != 0 {
				t.Errorf("want every call keyed as %v; violations=%v stale=%v", tc.sites, violations, stale)
			}
		})
	}
}

// TestExecSiteAllowlist_UnlistedSitesStillFail proves the re-keyed
// allowlist keeps the guard's strength: a new exec call, a listed call
// whose text changed, a listed call moved into another function, and a
// copy-pasted duplicate of a listed call all fail.
func TestExecSiteAllowlist_UnlistedSitesStillFail(t *testing.T) {
	tests := []struct {
		name      string
		src       string
		allowlist map[execSite]string // nil means syntheticAllowlist
		wantStale bool                // the original entry no longer matches anything
	}{
		{
			name: "new exec call added",
			src: strings.Replace(syntheticBase,
				"func other(name, arg string) {}",
				"func other(name, arg string) {\n\t_ = exec.Command(name)\n}", 1),
		},
		{
			name: "listed call's text changed",
			src: strings.Replace(syntheticBase,
				`exec.Command(name, "-x", arg)`, `exec.Command(name, "-y", arg)`, 1),
			wantStale: true,
		},
		{
			name: "listed call moved to a different function",
			src: strings.Replace(strings.Replace(syntheticBase,
				"\t_ = exec.Command(name, \"-x\", arg)\n", "", 1),
				"func other(name, arg string) {}",
				"func other(name, arg string) {\n\t_ = exec.Command(name, \"-x\", arg)\n}", 1),
			wantStale: true,
		},
		{
			name: "listed call moved between two init funcs",
			src: strings.Replace(syntheticInits,
				"func init() {\n\t_ = exec.Command(os.Args[1])\n}\n\nfunc init() {}\n",
				"func init() {}\n\nfunc init() {\n\t_ = exec.Command(os.Args[1])\n}\n", 1),
			allowlist: syntheticInitsAllowlist,
			wantStale: true,
		},
		{
			name: "listed call moved between two blank vars",
			src: strings.Replace(syntheticInits,
				"var _ = exec.Command(os.Args[2])\n\nvar _ = 0\n",
				"var _ = 0\n\nvar _ = exec.Command(os.Args[2])\n", 1),
			allowlist: syntheticInitsAllowlist,
			wantStale: true,
		},
		{
			name: "listed call moved between two blank methods",
			src: strings.Replace(syntheticInits,
				"func (T) _() {\n\t_ = exec.Command(os.Args[3])\n}\n\nfunc (*T) _() {}\n",
				"func (T) _() {}\n\nfunc (*T) _() {\n\t_ = exec.Command(os.Args[3])\n}\n", 1),
			allowlist: syntheticInitsAllowlist,
			wantStale: true,
		},
		{
			name: "listed call moved between two blank funcs",
			src: strings.Replace(syntheticBlankFuncs,
				"func _() {\n\t_ = exec.Command(os.Args[2])\n}\n\nfunc _() {}\n",
				"func _() {}\n\nfunc _() {\n\t_ = exec.Command(os.Args[2])\n}\n", 1),
			allowlist: syntheticInitsAllowlist,
			wantStale: true,
		},
		{
			name: "identical duplicate of listed call in the same function",
			src: strings.Replace(syntheticBase,
				"\t_ = exec.Command(name, \"-x\", arg)\n",
				"\t_ = exec.Command(name, \"-x\", arg)\n\t_ = exec.Command(name, \"-x\", arg)\n", 1),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			allowlist := tc.allowlist
			if allowlist == nil {
				allowlist = syntheticAllowlist
			}
			violations, stale := runSyntheticGuard(t, tc.src, allowlist)
			if len(violations) != 1 {
				t.Errorf("want exactly 1 violation, got %d: %v", len(violations), violations)
			}
			if gotStale := len(stale) > 0; gotStale != tc.wantStale {
				t.Errorf("stale = %v, want stale=%v", stale, tc.wantStale)
			}
		})
	}
}

// TestNormalizeCallText pins the normalized form allowlist entries are
// written in, including the cases where layout must not matter and where
// tokens must not fuse.
func TestNormalizeCallText(t *testing.T) {
	tests := []struct{ expr, want string }{
		{`exec.Command("git", "init", p)`, `exec.Command("git", "init", p)`},
		{"exec.Command(\n\t\"git\",\n\t\"init\", // c\n\tp,\n)", `exec.Command("git", "init", p)`},
		{`exec.Command("git", "-b", b, "origin/" + b)`, `exec.Command("git", "-b", b, "origin/"+b)`},
		{`exec.Command(args[0], args[1:]...)`, `exec.Command(args[0], args[1:]...)`},
		{`run(func(x int) {})`, `run(func(x int){})`},

		// Adjacent operators that would fuse keep a space, so distinct
		// token sequences never collide.
		{`f(a & &b)`, `f(a& &b)`},
		{`f(a && b)`, `f(a&&b)`},
		{`f(a - -b)`, `f(a- -b)`},
		{`f(a / *p)`, `f(a/ *p)`},
		{`f(x.y, 1.5, a...)`, `f(x.y, 1.5, a...)`},

		// Statement boundaries inside a func-literal argument are kept,
		// whether written as line breaks or as ";", and layout does not
		// matter.
		{"run(func() {\n\tx()\n\ty()\n})", `run(func(){x(); y()})`},
		{`run(func() { x(); y() })`, `run(func(){x(); y()})`},
		{"run(func() {\n\tx()\n\t(y)\n})", `run(func(){x(); (y)})`},
		{`run(func() { x()(y) })`, `run(func(){x()(y)})`},
		{"run(func() {\n\tif ok {\n\t\tx()\n\t}\n})", `run(func(){if ok{x()}})`},
		{`run(func() { for i := 0; i < n; i++ { x(i) } })`, `run(func(){for i:=0; i<n; i++{x(i)}})`},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			fset := token.NewFileSet()
			expr, err := parser.ParseExprFrom(fset, "", tc.expr, 0)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.expr, err)
			}
			call, ok := expr.(*ast.CallExpr)
			if !ok {
				t.Fatalf("%q is not a call", tc.expr)
			}
			if got := normalizeCallText(fset, call); got != tc.want {
				t.Errorf("normalizeCallText(%q) = %q, want %q", tc.expr, got, tc.want)
			}
		})
	}
}
