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

package util

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// hostGitUtilImport is pkg/util's import path; CeilingOn is accepted only as
// a selector on its import name.
const hostGitUtilImport = "github.com/GoogleCloudPlatform/scion/pkg/util"

// hostGitDoorRoots are the trees whose non-test code runs git on the host.
var hostGitDoorRoots = []string{"pkg/util", "pkg/agent", "pkg/provision", "pkg/runtimebroker", "pkg/hub"}

// hostGitDoorFuncs are the only functions that may exec git directly: the
// one door (RunHostGit builds through HostGitCommand).
var hostGitDoorFuncs = map[string]bool{
	"pkg/util/git_host.go:HostGitCommand": true,
	"pkg/util/git_host.go:RunHostGit":     true,
}

// hostGitDoorEntry allows one function's process starts. count is the number
// of "git" string literals in the function body (composite literals
// included) plus the number of process starts whose command is not a string
// literal (a non-constant name, a named package constant that is "git", or
// another way of starting a process).
type hostGitDoorEntry struct {
	count  int
	reason string
}

// hostGitDoorReasonHub is the reason for git in the Hub's own source checkout.
const hostGitDoorReasonHub = "the hub's own source checkout, operated by the hub operator; not a project repository agents can write"

// hostGitDoorReasonNotGit is the reason for a process start whose command is
// chosen at run time and is not git.
const hostGitDoorReasonNotGit = "the command is chosen at run time and is not git"

// hostGitDoorAllowlist names, by file and function ("Type.Method" for a
// method, the variable for a package-level func literal), every function
// outside the door that starts git, or starts a process whose command is not
// a string constant, with its count (hostGitDoorEntry). A count that differs
// from the code, more or fewer, fails the test, so an entry cannot outlive
// its last site or cover a new one.
var hostGitDoorAllowlist = map[string]hostGitDoorEntry{
	// Permanent: reports the git version; reads no repository.
	"pkg/util/git.go:GetGitVersion":                {2, "git --version only; runs in no repository"},
	"pkg/hub/system_handlers.go:GatherDiagnostics": {5, "git --version only; runs in no repository"},

	// Permanent: accepted by security; conditions held by the tests each
	// reason names. These are the checks that read the repository before
	// anything runs in it, the door's own reads and the agent-start check, so
	// they cannot go through the door.
	"pkg/util/git_host.go:readFilterPins":                {1, "HostGitCommand's own config listing; held by TestHostGitGoesThroughOneDoor_CheckerArgsArePinned and TestHostGitCheckers_DoorListingsUseDoorEnv"},
	"pkg/util/git_config_policy.go:crossCheckAgainstGit": {1, "the policy check's own listing; held by TestHostGitGoesThroughOneDoor_CheckerArgsArePinned, TestHostGitCheckers_DoorListingsUseDoorEnv and TestHostGitCheckers_SeedListingsPinGitDir"},
	"pkg/util/git_seed_submodules.go:seedGitlinks":       {1, "the agent-start check's own index listing; held by TestHostGitGoesThroughOneDoor_CheckerArgsArePinned and TestHostGitCheckers_SeedListingsPinGitDir"},

	"pkg/util/git.go:BaseRepoGitCommand":                   {1, "migrates in phase 2"},
	"pkg/util/git.go:hardenedGitCommand":                   {1, "migrates in phase 2"},
	"pkg/util/git_host.go:hostGit":                         {1, "migrates in phase 2"},
	"pkg/agent/run.go:boundedRevParse":                     {1, "migrates in phase 2"},
	"pkg/agent/provision.go:worktreeConfigEnabled":         {1, "migrates in phase 2"},
	"pkg/provision/provision.go:sharedCheckoutGitCmd":      {1, "migrates in phase 2"},
	"pkg/provision/provision.go:runGitClone":               {1, "migrates in phase 2"},
	"pkg/provision/provision.go:checkBranchName":           {1, "migrates in phase 2"},
	"pkg/provision/provision.go:currentBranch":             {1, "migrates in phase 2"},
	"pkg/runtimebroker/workspace_git.go:workspaceGitProbe": {1, "migrates with the broker probe change (separate lane); becomes the single CeilingOff caller"},

	// Permanent: accepted by security; conditions held by
	// TestHostGitGoesThroughOneDoor (exact counts, step tables included). Git
	// in the Hub's own source checkout (self-update).
	// The Rebuild executors queue git in a step table, step{"<label>", "git",
	// []string{...}, dir}, run with exec.CommandContext(ctx, step.cmd,
	// step.args...): the table's literals and the dynamic start are counted.
	"pkg/hub/unitdrift/check.go:GitRevision":                                 {1, hostGitDoorReasonHub},
	"pkg/hub/maintenance/maintenance_executors.go:RebuildServerExecutor.Run": {6, hostGitDoorReasonHub},
	"pkg/hub/maintenance/maintenance_executors.go:RebuildWebExecutor.Run":    {6, hostGitDoorReasonHub},
	"pkg/hub/maintenance/maintenance_executors.go:CheckForUpdates":           {5, hostGitDoorReasonHub},

	// Process starts whose command is chosen at run time and is not git: the
	// container runtime, a mount helper, the Hub's build script, a binary
	// under verification, the installer's runner.
	"pkg/hub/maintenance/installer.go:ExecRunner.Run":                                  {1, hostGitDoorReasonNotGit + " (the installer's runner; its callers in this package pass sudo)"},
	"pkg/hub/maintenance/installer.go:ExecRunner.Start":                                {1, hostGitDoorReasonNotGit + " (the installer's runner; its callers in this package pass sudo)"},
	"pkg/hub/maintenance/maintenance_executors.go:BuildHarnessConfigImageExecutor.Run": {3, hostGitDoorReasonNotGit + " (the container runtime)"},
	"pkg/hub/maintenance/maintenance_executors.go:PullImagesExecutor.Run":              {1, hostGitDoorReasonNotGit + " (the container runtime)"},
	"pkg/hub/maintenance/maintenance_executors.go:VerifyScionBinary":                   {1, hostGitDoorReasonNotGit + " (the scion binary being verified)"},
	"pkg/hub/system_handlers.go:Server.runImageBuild":                                  {1, hostGitDoorReasonNotGit + " (the image build script)"},
	"pkg/runtimebroker/nfs_mount_run_unix.go:execRunCommand":                           {1, hostGitDoorReasonNotGit + " (the mount helper)"},
	"pkg/runtimebroker/pty_classifier.go:tmuxHasSession":                               {1, hostGitDoorReasonNotGit + " (the container runtime)"},
	"pkg/runtimebroker/pty_handlers.go:LocalPTYSession.startDockerExec":                {1, hostGitDoorReasonNotGit + " (the container runtime)"},
	"pkg/runtimebroker/pty_handlers.go:StreamPTYHandler.startDockerExec":               {1, hostGitDoorReasonNotGit + " (the container runtime)"},
	"pkg/runtimebroker/pty_handlers.go:activateTmuxSetTitles":                          {1, hostGitDoorReasonNotGit + " (the container runtime)"},
	"pkg/runtimebroker/pty_handlers.go:findContainerPIDByNonce":                        {1, hostGitDoorReasonNotGit + " (the container runtime)"},
	"pkg/runtimebroker/pty_handlers.go:killContainerPID":                               {1, hostGitDoorReasonNotGit + " (the container runtime)"},
	"pkg/runtimebroker/pty_handlers.go:queryTmuxActiveWindow":                          {1, hostGitDoorReasonNotGit + " (the container runtime)"},
}

// hostGitCeilingOffAllowlist names, by file and function, every use of
// CeilingOff (or any Ceiling value other than CeilingOn) outside git_host.go,
// with how many and why. Each needs every ancestor of its directory to be
// host-owned, a git subcommand that runs no configured command, and a caller
// that treats any unexpected outcome as unknown.
var hostGitCeilingOffAllowlist = map[string]hostGitDoorEntry{}

// TestHostGitGoesThroughOneDoor: host-side git runs through RunHostGit or
// HostGitCommand. It parses (no type checking) the non-test code of
// hostGitDoorRoots and fails on:
//   - a function outside hostGitDoorFuncs that starts git (exec.Command or
//     exec.CommandContext whose command is the literal "git", a path ending
//     in /git or a base name git.exe, or a named string constant equal to
//     one) or starts a process whose command is not a string literal or a
//     name that resolves to a string constant (for example a variable,
//     parameter, field or call, a step table's field among them), unless
//     hostGitDoorAllowlist names it with the exact count;
//   - os.StartProcess, syscall.Exec, syscall.ForkExec, syscall.StartProcess
//     and an exec.Cmd composite literal, counted as non-constant starts;
//   - any of those starters used as a value (assigned, or passed as an
//     argument) instead of called, counted as a non-constant start;
//   - an assignment to .Path or .Args of a variable bound, in the same
//     function, to exec.Command, exec.CommandContext, new(exec.Cmd) or an
//     exec.Cmd literal, counted as a non-constant start;
//   - a dot import of os/exec;
//   - in every non-test file of the module (not only hostGitDoorRoots),
//     outside git_host.go and hostGitCeilingOffAllowlist: CeilingOff; a
//     conversion written HostGitCeiling(x) or <pkg>.HostGitCeiling(x) whose
//     argument is not CeilingOn; a HostGitCeiling variable or slice element,
//     a Ceiling element of a HostGitOptions literal (including an element
//     literal whose type is elided in an array, slice or map of
//     HostGitOptions), and the value of an assignment to a .Ceiling field,
//     each when it is not CeilingOn; any unkeyed element in such a literal;
//     and an increment of a .Ceiling field. A value that contains a
//     HostGitCeiling conversion is judged by the conversion rule alone. A
//     .Ceiling write or increment is not flagged when its receiver is
//     provably of another type (below);
//   - a variable typed as RepoConfigRefusedError passed to a slog call or to
//     a call whose format uses %+v or %#v (Format and LogValue already make
//     both print Error(); this keeps code from relying on the fields).
//
// It also counts, as a git start, a constant non-git launcher given git as
// an argument (env git, timeout 5 git, sh -c "git status", xargs git), and
// flags (module-wide) any &x.Ceiling, any declaration named CeilingOn or
// CeilingOff outside git_host.go, and any alias or defined type over
// HostGitOptions declared outside pkg/util (so every HostGitOptions literal
// is spelled with that name). CeilingOn is accepted only as a selector on
// pkg/util's import name, or inside pkg/util as the bare identifier. A
// receiver counts as provably not a HostGitOptions only when every binding
// of its name in the function is of a non-alias struct type of the same
// package with no embedded field, and no package-level variable of the
// package has that name.
//
// This test is a tripwire, not a proof: it parses without type checking and
// catches the forms above. It does not detect a wrapper that starts
// processes in another package and is passed "git" by its callers (the
// wrapper itself is allowlisted as a non-constant start, its callers are not
// counted); an exec.Cmd built in one function and given its Path or Args in
// another; a step table built in a different function from the one that
// runs it; a process started through a package other than os, os/exec and
// syscall (for example golang.org/x/sys/unix); anything reached through
// reflection; a git command name assembled at run time and passed to a
// constant launcher other than as a string literal; git later in a shell
// string (sh -c "cd x && git ...", "exec git", "VAR=y git"); a conversion
// from a same-shaped struct into HostGitOptions; decoding into one
// (json.Unmarshal and the like); unsafe; a parenthesised conversion such as
// (util.HostGitCeiling)(1), and arithmetic on a conversion such as
// util.HostGitCeiling(util.CeilingOn) + 1; a parameter or local variable
// that shadows a package or local string constant and is used as the
// command (it is read as the constant); git command names that differ only
// in case (Git, GIT) or are a backslash path without .exe; and nested
// elided composite literals (for example [][]util.HostGitOptions{{{...}}}),
// whose inner literals are not checked.
func TestHostGitGoesThroughOneDoor(t *testing.T) {
	scan := scanHostGitDoorTree(t)
	// Non-vacuous: the door itself must be seen.
	if fn := scan.funcs["pkg/util/git_host.go:HostGitCommand"]; fn == nil || len(fn.gitExecs) != 1 {
		t.Fatal("did not find HostGitCommand's one git exec; the detector is broken")
	}
	for _, v := range scan.violations(hostGitDoorAllowlist, hostGitCeilingOffAllowlist) {
		t.Error(v)
	}
	for key, e := range hostGitDoorAllowlist {
		if strings.TrimSpace(e.reason) == "" || e.count < 1 {
			t.Errorf("allowlist entry %s needs a reason and a count", key)
		}
	}
}

// scanHostGitDoorTree parses every non-test Go file of the module (nested
// modules, vendor, node_modules, testdata and hidden directories excluded)
// and scans them: the Ceiling checks run on all of them, the process-start
// checks on hostGitDoorRoots.
func scanHostGitDoorTree(t *testing.T) *hostGitDoorScan {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	sources := map[string]*ast.File{}
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			if name := d.Name(); name == "testdata" || name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
				return filepath.SkipDir // a nested module
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		sources[filepath.ToSlash(rel)] = f
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A file known to be outside the door roots must be among them, so the
	// module-wide Ceiling checks really cover the module.
	if _, ok := sources["cmd/scion/main.go"]; !ok {
		t.Fatal("cmd/scion/main.go was not parsed; the walk is not reaching the module")
	}
	inRoots := 0
	for rel := range sources {
		for _, r := range hostGitDoorRoots {
			if strings.HasPrefix(rel, r+"/") {
				inRoots++
			}
		}
	}
	if inRoots < 100 || len(sources) <= inRoots {
		t.Fatalf("parsed %d files, %d under the door roots; the walk is not reaching the module", len(sources), inRoots)
	}
	return scanHostGitDoorFiles(fset, sources)
}

// scanHostGitDoorFiles scans parsed files keyed by module path: string
// constants are collected per package directory first, then every file is
// scanned.
func scanHostGitDoorFiles(fset *token.FileSet, sources map[string]*ast.File) *hostGitDoorScan {
	scan := newHostGitDoorScan()
	rels := make([]string, 0, len(sources))
	for rel := range sources {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		scan.addPackageDecls(path.Dir(rel), sources[rel])
	}
	for _, rel := range rels {
		scan.file(fset, rel, sources[rel])
	}
	return scan
}

// hostGitDoorFunc is what one function starts.
type hostGitDoorFunc struct {
	gitExecs []string // exec of a literal or constant git command
	dynExecs []string // process starts whose command is not a string constant
	gitLits  int      // "git" string literals in the body
	constGit int      // exec of a package-level constant equal to git
}

func (f *hostGitDoorFunc) needsEntry() bool { return len(f.gitExecs) > 0 || len(f.dynExecs) > 0 }

func (f *hostGitDoorFunc) count() int { return f.gitLits + len(f.dynExecs) + f.constGit }

// hostGitDoorScan collects what TestHostGitGoesThroughOneDoor checks, keyed
// by "file:function".
type hostGitDoorScan struct {
	funcs      map[string]*hostGitDoorFunc
	ceilingOff map[string][]string
	findings   []string
	consts     map[string]map[string]string // package dir -> name -> string value
	// plain are, per package dir, the names of non-alias struct types with
	// no embedded field.
	plain map[string]map[string]bool
	// pkgVars are, per package dir, the package-level variable names: a
	// function may refer to one, so its name is never provably of another
	// type there.
	pkgVars map[string]map[string]bool
}

func newHostGitDoorScan() *hostGitDoorScan {
	return &hostGitDoorScan{
		funcs:      map[string]*hostGitDoorFunc{},
		ceilingOff: map[string][]string{},
		consts:     map[string]map[string]string{},
		plain:      map[string]map[string]bool{},
		pkgVars:    map[string]map[string]bool{},
	}
}

// addPackageDecls records, under dir, the package-level string constants of
// f and its plain struct types (non-alias, no embedded field).
func (s *hostGitDoorScan) addPackageDecls(dir string, f *ast.File) {
	m := s.consts[dir]
	if m == nil {
		m = map[string]string{}
		s.consts[dir] = m
	}
	pt := s.plain[dir]
	if pt == nil {
		pt = map[string]bool{}
		s.plain[dir] = pt
	}
	pv := s.pkgVars[dir]
	if pv == nil {
		pv = map[string]bool{}
		s.pkgVars[dir] = pv
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		switch gd.Tok {
		case token.VAR:
			for _, spec := range gd.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					for _, n := range vs.Names {
						pv[n.Name] = true
					}
				}
			}
		case token.CONST:
			recordStringConsts(gd, m)
		case token.TYPE:
			for _, spec := range gd.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && ts.Assign == 0 && isPlainStruct(ts.Type) {
					pt[ts.Name.Name] = true
				}
			}
		}
	}
}

// isPlainStruct reports whether e is a struct type with no embedded field.
func isPlainStruct(e ast.Expr) bool {
	st, ok := e.(*ast.StructType)
	if !ok || st.Fields == nil {
		return false
	}
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			return false
		}
	}
	return true
}

// recordStringConsts records each name of gd bound to a string literal.
func recordStringConsts(gd *ast.GenDecl, into map[string]string) {
	for _, spec := range gd.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok || len(vs.Values) != len(vs.Names) {
			continue
		}
		for i, n := range vs.Names {
			if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if v, err := strconv.Unquote(lit.Value); err == nil {
					into[n.Name] = v
				}
			}
		}
	}
}

// violations applies the allowlists to what was collected.
func (s *hostGitDoorScan) violations(allow, ceilingAllow map[string]hostGitDoorEntry) []string {
	var out []string
	keys := map[string]bool{}
	for k, fn := range s.funcs {
		if fn.needsEntry() {
			keys[k] = true
		}
	}
	for k := range allow {
		keys[k] = true
	}
	for _, k := range sortedKeys(keys) {
		if hostGitDoorFuncs[k] {
			continue
		}
		fn := s.funcs[k]
		e, ok := allow[k]
		switch {
		case fn == nil || !fn.needsEntry():
			out = append(out, fmt.Sprintf("allowlist entry %s matches no git or non-constant process start; remove it", k))
		case !ok:
			out = append(out, fmt.Sprintf("%s starts git (%s) or a non-constant command (%s) and has no allowlist entry; use RunHostGit or HostGitCommand", k, strings.Join(fn.gitExecs, ", "), strings.Join(fn.dynExecs, ", ")))
		case fn.count() != e.count:
			out = append(out, fmt.Sprintf("%s: count %d (%d git literals, %d non-constant starts, %d constant git starts), allowlist says %d; update the entry", k, fn.count(), fn.gitLits, len(fn.dynExecs), fn.constGit, e.count))
		}
	}
	ckeys := map[string]bool{}
	for k := range s.ceilingOff {
		ckeys[k] = true
	}
	for k := range ceilingAllow {
		ckeys[k] = true
	}
	for _, k := range sortedKeys(ckeys) {
		got := s.ceilingOff[k]
		e, ok := ceilingAllow[k]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("CeilingOff in %s (%s) has no allowlist entry", k, strings.Join(got, ", ")))
		case len(got) != e.count:
			out = append(out, fmt.Sprintf("CeilingOff in %s: found %d (%s), allowlist says %d; update or remove the entry", k, len(got), strings.Join(got, ", "), e.count))
		}
	}
	return append(out, s.findings...)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// isGitLiteralValue reports whether a string names the git binary.
func isGitLiteralValue(v string) bool {
	return v == "git" || strings.HasSuffix(v, "/git") || strings.EqualFold(path.Base(strings.ReplaceAll(v, `\`, "/")), "git.exe")
}

// file scans one parsed file; rel is its slash-separated module path.
func (s *hostGitDoorScan) file(fset *token.FileSet, rel string, f *ast.File) {
	// The process-start and refusal checks cover hostGitDoorRoots; the
	// Ceiling checks cover every file.
	execChecked := false
	for _, r := range hostGitDoorRoots {
		if strings.HasPrefix(rel, r+"/") {
			execChecked = true
		}
	}
	execName, slogName, osName, syscallName, utilName := "", "", "", "", ""
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		name := path.Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		switch p {
		case "os/exec":
			execName = name
			if name == "." && execChecked {
				s.findings = append(s.findings, fmt.Sprintf("%s: dot import of os/exec; import it by name so process starts can be checked", rel))
			}
		case "log/slog":
			slogName = name
		case "os":
			osName = name
		case "syscall":
			syscallName = name
		case hostGitUtilImport:
			utilName = name
		}
	}
	if !execChecked {
		execName, slogName, osName, syscallName = "", "", "", ""
	}
	inUtil := path.Dir(rel) == "pkg/util"
	// isOn reports whether e is CeilingOn: outside pkg/util only a selector
	// on pkg/util's import name, inside it only the bare identifier (any other
	// declaration of that name is a finding below).
	isOn := func(e ast.Expr) bool {
		switch x := e.(type) {
		case *ast.Ident:
			return inUtil && x.Name == "CeilingOn"
		case *ast.SelectorExpr:
			id, ok := x.X.(*ast.Ident)
			return ok && utilName != "" && utilName != "." && id.Name == utilName && x.Sel.Name == "CeilingOn"
		}
		return false
	}
	pkgConsts := s.consts[path.Dir(rel)]
	pkgPlain := s.plain[path.Dir(rel)]
	pkgVars := s.pkgVars[path.Dir(rel)]
	// otherType reports whether e names (possibly through a pointer) a plain
	// struct type of this package other than HostGitOptions: a variable of
	// such a type cannot be, or embed, a HostGitOptions. An alias, a type with
	// an embedded field, an unknown or another package's type is not one.
	otherType := func(e ast.Expr) bool {
		if st, ok := e.(*ast.StarExpr); ok {
			e = st.X
		}
		id, ok := e.(*ast.Ident)
		return ok && id.Name != "HostGitOptions" && pkgPlain[id.Name]
	}
	// otherLiteral reports whether e is a literal (or its address) of such a
	// type.
	otherLiteral := func(e ast.Expr) bool {
		if u, ok := e.(*ast.UnaryExpr); ok {
			e = u.X
		}
		cl, ok := e.(*ast.CompositeLit)
		return ok && cl.Type != nil && otherType(cl.Type)
	}
	typed := map[string]bool{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gd.Specs {
			if vs, ok := spec.(*ast.ValueSpec); ok {
				bindValueSpec(vs, typed)
			}
		}
	}
	// Every Ceiling check skips git_host.go, which defines the type.
	ceilingChecked := rel != "pkg/util/git_host.go"
	// Any other declaration named CeilingOn or CeilingOff could stand in for
	// the real constants.
	if ceilingChecked {
		declared := func(id *ast.Ident) {
			if id != nil && (id.Name == "CeilingOn" || id.Name == "CeilingOff") {
				s.findings = append(s.findings, fmt.Sprintf("%s: declares %s; only pkg/util/git_host.go may", pos(fset, id), id.Name))
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ValueSpec:
				for _, id := range x.Names {
					declared(id)
				}
			case *ast.TypeSpec:
				declared(x.Name)
				// An alias or defined type over HostGitOptions would hide its
				// literals and writes from the checks below.
				if !inUtil && isTypeNamed(x.Type, "HostGitOptions") {
					s.findings = append(s.findings, fmt.Sprintf("%s: declares %s over HostGitOptions; use util.HostGitOptions directly", pos(fset, x.Name), x.Name.Name))
				}
			case *ast.FuncDecl:
				declared(x.Name)
			case *ast.Field:
				for _, id := range x.Names {
					declared(id)
				}
			case *ast.AssignStmt:
				if x.Tok == token.DEFINE {
					for _, lhs := range x.Lhs {
						if id, ok := lhs.(*ast.Ident); ok {
							declared(id)
						}
					}
				}
			case *ast.RangeStmt:
				if x.Tok == token.DEFINE {
					for _, e := range []ast.Expr{x.Key, x.Value} {
						if id, ok := e.(*ast.Ident); ok {
							declared(id)
						}
					}
				}
			}
			return true
		})
	}
	inspect := func(key string, node ast.Node) {
		fn := s.funcs[key]
		if fn == nil {
			fn = &hostGitDoorFunc{}
			s.funcs[key] = fn
		}
		localConsts := map[string]string{}
		// callFuns are the Fun expressions of calls: a starter selector that is
		// not one of them is used as a value.
		callFuns := map[ast.Expr]bool{}
		// cmdVars are variables bound to an *exec.Cmd built here.
		cmdVars := map[string]bool{}
		// A name is provably not a HostGitOptions only when EVERY binding of
		// it in the function (parameters, results and receivers, nested func
		// literals' included; var declarations; := assignments; range and type
		// switch variables) is of, or a literal of, another plain struct type
		// of this package. Any other binding removes the name.
		otherBind, badBind := map[string]bool{}, map[string]bool{}
		bind := func(name string, other bool) {
			if name == "_" {
				return
			}
			if other {
				otherBind[name] = true
			} else {
				badBind[name] = true
			}
		}
		bindFields := func(fl *ast.FieldList) {
			if fl == nil {
				return
			}
			for _, field := range fl.List {
				o := otherType(field.Type)
				for _, n := range field.Names {
					bind(n.Name, o)
				}
			}
		}
		if fd, ok := node.(*ast.FuncDecl); ok {
			bindFields(fd.Recv)
			bindFields(fd.Type.Params)
			bindFields(fd.Type.Results)
		}
		ast.Inspect(node, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncLit:
				bindFields(x.Type.Params)
				bindFields(x.Type.Results)
			case *ast.ValueSpec:
				for i, id := range x.Names {
					o := false
					switch {
					case x.Type != nil:
						o = otherType(x.Type)
					case i < len(x.Values):
						o = otherLiteral(x.Values[i])
					}
					bind(id.Name, o)
				}
			case *ast.AssignStmt:
				if x.Tok == token.DEFINE {
					for i, lhs := range x.Lhs {
						if id, ok := lhs.(*ast.Ident); ok {
							bind(id.Name, len(x.Lhs) == len(x.Rhs) && otherLiteral(x.Rhs[i]))
						}
					}
				}
			case *ast.RangeStmt:
				if x.Tok == token.DEFINE {
					for _, e := range []ast.Expr{x.Key, x.Value} {
						if id, ok := e.(*ast.Ident); ok {
							bind(id.Name, false)
						}
					}
				}
			}
			return true
		})
		// A package-level variable of the same name may be what the function
		// refers to, so it counts as a binding that is not provably other.
		nonOpts := func(name string) bool { return otherBind[name] && !badBind[name] && !pkgVars[name] }
		ceiling := func(n ast.Node) {
			s.ceilingOff[key] = append(s.ceilingOff[key], pos(fset, n))
		}
		// notOn reports whether a Ceiling value is neither CeilingOn nor counted
		// elsewhere (where CeilingOff is named, or as a HostGitCeiling
		// conversion).
		notOn := func(e ast.Expr) bool {
			return !isOn(e) && !mentionsIdent(e, "CeilingOff") && !mentionsIdent(e, "HostGitCeiling")
		}
		ast.Inspect(node, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.GenDecl:
				if x.Tok == token.CONST {
					recordStringConsts(x, localConsts)
				}
			case *ast.ValueSpec:
				bindValueSpec(x, typed)
				for i, n := range x.Names {
					var v ast.Expr
					if i < len(x.Values) {
						v = x.Values[i]
					}
					if v != nil && isCmdSource(v, execName) {
						cmdVars[n.Name] = true
					}
					// A HostGitCeiling variable set to anything but CeilingOn.
					if ceilingChecked && x.Type != nil && isTypeNamed(x.Type, "HostGitCeiling") && v != nil && notOn(v) {
						ceiling(x)
					}
				}
			case *ast.BasicLit:
				if x.Kind == token.STRING {
					if v, err := strconv.Unquote(x.Value); err == nil && isGitLiteralValue(v) {
						fn.gitLits++
					}
				}
			case *ast.SelectorExpr:
				if isStarterSelector(x, execName, osName, syscallName) && !callFuns[x] {
					fn.dynExecs = append(fn.dynExecs, pos(fset, x)+" starter used as a value")
				}
				if ceilingChecked && x.Sel.Name == "CeilingOff" {
					ceiling(x)
					return false
				}
			case *ast.Ident:
				if ceilingChecked && x.Name == "CeilingOff" {
					ceiling(x)
				}
			case *ast.CompositeLit:
				if x.Type != nil && execName != "" && execName != "." && isSelector(x.Type, execName, "Cmd") {
					fn.dynExecs = append(fn.dynExecs, pos(fset, x)+" exec.Cmd literal")
				}
				if !ceilingChecked || x.Type == nil {
					break
				}
				// A Ceiling other than CeilingOn in a HostGitOptions literal, and
				// a literal with unkeyed elements (which set Ceiling by position).
				optsLiteral := func(cl *ast.CompositeLit) {
					unkeyed := false
					for _, elt := range cl.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							unkeyed = true
							continue
						}
						if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Ceiling" && notOn(kv.Value) {
							ceiling(kv)
						}
					}
					if unkeyed {
						ceiling(cl)
					}
				}
				if isTypeNamed(x.Type, "HostGitOptions") {
					optsLiteral(x)
				}
				// Element literals whose type is elided ([]HostGitOptions{{...}},
				// []*HostGitOptions{{...}}, map[K]HostGitOptions{k: {...}}) get the
				// same checks; their own visit sees no type.
				elided := func(e ast.Expr) {
					if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
						e = u.X
					}
					if cl, ok := e.(*ast.CompositeLit); ok && cl.Type == nil {
						optsLiteral(cl)
					}
				}
				var keyOpts, valOpts bool
				switch t := x.Type.(type) {
				case *ast.ArrayType:
					valOpts = isTypeNamed(t.Elt, "HostGitOptions")
				case *ast.MapType:
					keyOpts = isTypeNamed(t.Key, "HostGitOptions")
					valOpts = isTypeNamed(t.Value, "HostGitOptions")
				}
				if keyOpts || valOpts {
					for _, elt := range x.Elts {
						if kv, ok := elt.(*ast.KeyValueExpr); ok {
							if keyOpts {
								elided(kv.Key)
							}
							if valOpts {
								elided(kv.Value)
							}
							continue
						}
						if valOpts {
							elided(elt)
						}
					}
				}
				// An element other than CeilingOn in a []HostGitCeiling literal.
				if at, ok := x.Type.(*ast.ArrayType); ok && isTypeNamed(at.Elt, "HostGitCeiling") {
					for _, elt := range x.Elts {
						if notOn(elt) {
							ceiling(elt)
						}
					}
				}
			case *ast.UnaryExpr:
				// &x.Ceiling: a pointer that can set it anywhere.
				if sel, ok := x.X.(*ast.SelectorExpr); ok && ceilingChecked && x.Op == token.AND && sel.Sel.Name == "Ceiling" {
					ceiling(x)
				}
			case *ast.IncDecStmt:
				if sel, ok := x.X.(*ast.SelectorExpr); ok && ceilingChecked && sel.Sel.Name == "Ceiling" && !receiverIsNonOpts(sel, nonOpts) {
					ceiling(x)
				}
			case *ast.AssignStmt:
				for i, lhs := range x.Lhs {
					var rhs ast.Expr
					switch {
					case len(x.Lhs) == len(x.Rhs):
						rhs = x.Rhs[i]
					case len(x.Rhs) == 1 && i == 0:
						rhs = x.Rhs[0]
					}
					if sel, ok := lhs.(*ast.SelectorExpr); ok {
						// <x>.Ceiling = <anything but CeilingOn>, unless x is
						// provably not a HostGitOptions. A value naming
						// CeilingOff, or a HostGitCeiling conversion, is counted
						// where it is.
						if ceilingChecked && sel.Sel.Name == "Ceiling" && !receiverIsNonOpts(sel, nonOpts) && (rhs == nil || notOn(rhs)) {
							ceiling(x)
						}
						// An exec.Cmd built here, then pointed at a program.
						if xid, ok := sel.X.(*ast.Ident); ok && cmdVars[xid.Name] && (sel.Sel.Name == "Path" || sel.Sel.Name == "Args") {
							fn.dynExecs = append(fn.dynExecs, pos(fset, x)+" exec.Cmd."+sel.Sel.Name+" set")
						}
					}
					id, ok := lhs.(*ast.Ident)
					if !ok || rhs == nil {
						continue
					}
					if isCmdSource(rhs, execName) {
						cmdVars[id.Name] = true
					}
					if call, ok := rhs.(*ast.CallExpr); ok {
						if fid, ok := call.Fun.(*ast.Ident); ok && fid.Name == "newRepoConfigRefusedError" {
							typed[id.Name] = true
						}
					}
				}
			case *ast.CallExpr:
				callFuns[x.Fun] = true
				s.classifyStart(fset, fn, x, execName, osName, syscallName, localConsts, pkgConsts)
				// A conversion to HostGitCeiling of anything but CeilingOn.
				if ceilingChecked && len(x.Args) == 1 && isTypeNamed(x.Fun, "HostGitCeiling") && !isOn(x.Args[0]) && !mentionsIdent(x.Args[0], "CeilingOff") {
					ceiling(x)
				}
				if refusalSink(x, slogName, typed) {
					s.findings = append(s.findings, fmt.Sprintf("%s: a RepoConfigRefusedError value reaches a slog call or a %%+v/%%#v format; log err.Error() or the wrapping error instead", pos(fset, x)))
				}
			}
			return true
		})
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if r := recvTypeName(d); r != "" {
				name = r + "." + name
			}
			inspect(rel+":"+name, d)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					key := rel + ":" + f.Name.Name
					if len(vs.Names) == 1 {
						key = rel + ":" + vs.Names[0].Name
					}
					inspect(key, vs)
				}
			}
		}
	}
}

// classifyStart records call in fn when it starts a process.
func (s *hostGitDoorScan) classifyStart(fset *token.FileSet, fn *hostGitDoorFunc, call *ast.CallExpr, execName, osName, syscallName string, localConsts, pkgConsts map[string]string) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return
	}
	switch {
	case osName != "" && pkg.Name == osName && sel.Sel.Name == "StartProcess",
		syscallName != "" && pkg.Name == syscallName && (sel.Sel.Name == "Exec" || sel.Sel.Name == "ForkExec" || sel.Sel.Name == "StartProcess"):
		fn.dynExecs = append(fn.dynExecs, pos(fset, call)+" "+pkg.Name+"."+sel.Sel.Name)
		return
	case execName == "" || pkg.Name != execName:
		return
	}
	arg := 0
	switch sel.Sel.Name {
	case "Command":
	case "CommandContext":
		arg = 1
	default:
		return
	}
	if len(call.Args) <= arg {
		return
	}
	switch x := call.Args[arg].(type) {
	case *ast.BasicLit:
		if v, err := strconv.Unquote(x.Value); err == nil && x.Kind == token.STRING {
			if isGitLiteralValue(v) {
				fn.gitExecs = append(fn.gitExecs, pos(fset, call))
				return
			}
			countLauncherGit(fset, fn, call, call.Args[arg+1:])
			return
		}
	case *ast.Ident:
		if v, ok := localConsts[x.Name]; ok {
			if isGitLiteralValue(v) {
				// The literal is in the body and already counted.
				fn.gitExecs = append(fn.gitExecs, pos(fset, call))
				return
			}
			countLauncherGit(fset, fn, call, call.Args[arg+1:])
			return
		}
		if v, ok := pkgConsts[x.Name]; ok {
			if isGitLiteralValue(v) {
				fn.gitExecs = append(fn.gitExecs, pos(fset, call))
				fn.constGit++
				return
			}
			countLauncherGit(fset, fn, call, call.Args[arg+1:])
			return
		}
	}
	fn.dynExecs = append(fn.dynExecs, pos(fset, call))
}

// countLauncherGit counts, for a constant non-git command, an argument that
// runs git through it (env git, timeout 5 git, sh -c "git status", xargs
// git): a string literal that is a git binary, or whose first word is one.
// A literal that is itself a git binary is already among the "git"
// literals; a "git ..." command line is counted as a constant git start.
func countLauncherGit(fset *token.FileSet, fn *hostGitDoorFunc, call *ast.CallExpr, args []ast.Expr) {
	for _, a := range args {
		lit, ok := a.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		v, err := strconv.Unquote(lit.Value)
		if err != nil {
			continue
		}
		words := strings.Fields(v)
		if len(words) == 0 || !isGitLiteralValue(words[0]) {
			continue
		}
		fn.gitExecs = append(fn.gitExecs, pos(fset, call)+" git through a launcher")
		if !isGitLiteralValue(v) {
			fn.constGit++
		}
		return
	}
}

// isStarterSelector reports whether sel names a function that starts a
// process: exec.Command, exec.CommandContext, os.StartProcess,
// syscall.Exec, syscall.ForkExec or syscall.StartProcess.
func isStarterSelector(sel *ast.SelectorExpr, execName, osName, syscallName string) bool {
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	switch {
	case execName != "" && pkg.Name == execName:
		return sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext"
	case osName != "" && pkg.Name == osName:
		return sel.Sel.Name == "StartProcess"
	case syscallName != "" && pkg.Name == syscallName:
		return sel.Sel.Name == "Exec" || sel.Sel.Name == "ForkExec" || sel.Sel.Name == "StartProcess"
	}
	return false
}

// isCmdSource reports whether e builds an *exec.Cmd: exec.Command(...),
// exec.CommandContext(...), new(exec.Cmd), exec.Cmd{...} or &exec.Cmd{...}.
func isCmdSource(e ast.Expr, execName string) bool {
	if execName == "" || execName == "." {
		return false
	}
	if u, ok := e.(*ast.UnaryExpr); ok {
		e = u.X
	}
	switch x := e.(type) {
	case *ast.CompositeLit:
		return x.Type != nil && isSelector(x.Type, execName, "Cmd")
	case *ast.CallExpr:
		if isSelector(x.Fun, execName, "Command") || isSelector(x.Fun, execName, "CommandContext") {
			return true
		}
		if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 {
			return isSelector(x.Args[0], execName, "Cmd")
		}
	}
	return false
}

// receiverIsNonOpts reports whether sel's receiver is a variable provably not
// a HostGitOptions.
func receiverIsNonOpts(sel *ast.SelectorExpr, nonOpts func(string) bool) bool {
	id, ok := sel.X.(*ast.Ident)
	return ok && nonOpts(id.Name)
}

func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg && sel.Sel.Name == name
}

func pos(fset *token.FileSet, n ast.Node) string {
	p := fset.Position(n.Pos())
	return fmt.Sprintf("%s:%d", filepath.Base(p.Filename), p.Line)
}

// bindValueSpec records names declared with the RepoConfigRefusedError type
// in typed.
func bindValueSpec(vs *ast.ValueSpec, typed map[string]bool) {
	if vs.Type == nil || !isTypeNamed(vs.Type, "RepoConfigRefusedError") {
		return
	}
	for _, n := range vs.Names {
		typed[n.Name] = true
	}
}

// isTypeNamed reports whether e is the type name, *name, pkg.name or
// *pkg.name.
func isTypeNamed(e ast.Expr, name string) bool {
	if st, ok := e.(*ast.StarExpr); ok {
		e = st.X
	}
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == name
	case *ast.SelectorExpr:
		return x.Sel.Name == name
	}
	return false
}

// mentionsIdent reports whether name appears as an identifier anywhere in e.
func mentionsIdent(e ast.Expr, name string) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

func recvTypeName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return ""
	}
	e := d.Recv.List[0].Type
	if st, ok := e.(*ast.StarExpr); ok {
		e = st.X
	}
	switch x := e.(type) {
	case *ast.IndexExpr:
		e = x.X
	case *ast.IndexListExpr:
		e = x.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// hostGitSlogMethods are the slog function and method names that take
// attributes.
var hostGitSlogMethods = map[string]bool{
	"Debug": true, "Info": true, "Warn": true, "Error": true, "Log": true, "LogAttrs": true,
	"DebugContext": true, "InfoContext": true, "WarnContext": true, "ErrorContext": true,
	"With": true, "Any": true, "Group": true,
}

// refusalSink reports whether call passes a typed RepoConfigRefusedError
// variable to slog or to a %+v / %#v format.
func refusalSink(call *ast.CallExpr, slogName string, typed map[string]bool) bool {
	hasTyped, verbose := false, false
	for _, a := range call.Args {
		for {
			if u, ok := a.(*ast.UnaryExpr); ok {
				a = u.X
				continue
			}
			if st, ok := a.(*ast.StarExpr); ok {
				a = st.X
				continue
			}
			if p, ok := a.(*ast.ParenExpr); ok {
				a = p.X
				continue
			}
			break
		}
		if id, ok := a.(*ast.Ident); ok && typed[id.Name] {
			hasTyped = true
		}
		if lit, ok := a.(*ast.BasicLit); ok && lit.Kind == token.STRING && (strings.Contains(lit.Value, "%+v") || strings.Contains(lit.Value, "%#v")) {
			verbose = true
		}
	}
	if !hasTyped {
		return false
	}
	if verbose {
		return true
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if pkg, ok := sel.X.(*ast.Ident); ok && slogName != "" && pkg.Name == slogName {
		return true
	}
	return hostGitSlogMethods[sel.Sel.Name]
}

// scanHostGitDoorSource scans fixture sources (module path -> source).
func scanHostGitDoorSource(t *testing.T, sources map[string]string) *hostGitDoorScan {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for rel, src := range sources {
		f, err := parser.ParseFile(fset, rel, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		files[rel] = f
	}
	return scanHostGitDoorFiles(fset, files)
}

// A raw git exec, or a process start whose command is not a string constant,
// added in pkg/agent fails the audit in every form; a constant non-git
// command does not.
func TestHostGitGoesThroughOneDoor_FlagsRawGitInAgent(t *testing.T) {
	const head = "package agent\nimport (\"context\"; \"os\"; \"os/exec\"; \"syscall\")\nconst fixtureGit = \"git\"\nconst fixtureRuntime = \"docker\"\nfunc use(f func(context.Context, string, ...string) *exec.Cmd) {}\n"
	for name, body := range map[string]string{
		"literal":         `_ = exec.Command("git", "status")`,
		"context":         `_ = exec.CommandContext(ctx, "git", "status")`,
		"bin path":        `_ = exec.Command("/usr/bin/git", "status")`,
		"lookpath":        `p, _ := exec.LookPath("git"); _ = exec.Command(p, "status")`,
		"local variable":  `bin := "git"; _ = exec.CommandContext(ctx, bin, "status")`,
		"package const":   `_ = exec.Command(fixtureGit, "status")`,
		"local const":     `const g = "git"; _ = exec.Command(g, "status")`,
		"variable":        `var name string; _ = exec.Command(name, "status")`,
		"field":           `var s struct{ cmd string }; _ = exec.Command(s.cmd)`,
		"exec.Cmd":        `_ = &exec.Cmd{Path: "/usr/bin/git"}`,
		"os.StartProcess": `_, _ = os.StartProcess("/usr/bin/git", nil, nil)`,
		"syscall.Exec":    `_ = syscall.Exec("/usr/bin/git", nil, nil)`,
		"starter value":   `run := exec.Command; _ = run("git", "status")`,
		"starter arg":     `use(exec.CommandContext)`,
		"mutated Path":    `c := exec.Command("true"); c.Path = "/usr/bin/git"`,
		"new Cmd Args":    `c := new(exec.Cmd); c.Args = []string{"/usr/bin/env"}`,
		"literal Cmd":     `c := &exec.Cmd{}; c.Path = "/usr/bin/env"`,
		"env launcher":    `_ = exec.Command("env", "git", "status")`,
		"timeout":         `_ = exec.CommandContext(ctx, "timeout", "5", "git", "fetch")`,
		"sh -c":           `_ = exec.Command("sh", "-c", "git status")`,
	} {
		t.Run(name, func(t *testing.T) {
			src := head + "func fixture(ctx context.Context) {\n" + body + "\n}\n"
			v := scanHostGitDoorSource(t, map[string]string{"pkg/agent/fixture.go": src}).violations(nil, nil)
			if len(v) != 1 || !strings.Contains(v[0], "pkg/agent/fixture.go:fixture") {
				t.Errorf("violations = %q, want one for pkg/agent/fixture.go:fixture", v)
			}
		})
	}
	dot := "package agent\nimport . \"os/exec\"\nfunc fixture() { _ = Command(\"git\") }\n"
	if v := scanHostGitDoorSource(t, map[string]string{"pkg/agent/dot.go": dot}).violations(nil, nil); len(v) != 1 || !strings.Contains(v[0], "dot import") {
		t.Errorf("dot import: violations %q, want one", v)
	}
	ok := head + "type urlish struct{ Path string }\nfunc fixture() { _ = exec.Command(\"podman\", \"ps\"); _ = exec.Command(\"gitk\"); _ = exec.Command(fixtureRuntime); _, _ = os.Getwd(); _ = syscall.Getpid(); c := exec.Command(\"true\"); c.Dir = \"/tmp\"; u := urlish{}; u.Path = \"x\"; _ = c; _ = exec.Command(\"env\", \"FOO=1\", \"podman\"); _ = exec.Command(\"sh\", \"-c\", \"gitk --all\") }\n"
	if v := scanHostGitDoorSource(t, map[string]string{"pkg/agent/fixture.go": ok}).violations(nil, nil); len(v) != 0 {
		t.Errorf("a constant non-git command was flagged: %q", v)
	}
	// A string constant in another file of the same package resolves.
	other := map[string]string{
		"pkg/agent/a.go": "package agent\nimport \"os/exec\"\nfunc fixture() { _ = exec.Command(sharedRuntime) }\n",
		"pkg/agent/b.go": "package agent\nconst sharedRuntime = \"docker\"\n",
	}
	if v := scanHostGitDoorSource(t, other).violations(nil, nil); len(v) != 0 {
		t.Errorf("a constant from another file was flagged: %q", v)
	}
}

// An allowlist count that does not match the code fails the audit, in both
// directions; a step table's "git" literals count; the door needs no entry.
func TestHostGitGoesThroughOneDoor_CountsMustMatch(t *testing.T) {
	src := "package util\nimport \"os/exec\"\nfunc two() { _ = exec.Command(\"git\"); _ = exec.Command(\"git\") }\n"
	scan := scanHostGitDoorSource(t, map[string]string{"pkg/util/x.go": src})
	for _, n := range []int{1, 3} {
		if v := scan.violations(map[string]hostGitDoorEntry{"pkg/util/x.go:two": {n, "r"}}, nil); len(v) != 1 {
			t.Errorf("count %d against 2 sites: violations %q, want one", n, v)
		}
	}
	if v := scan.violations(map[string]hostGitDoorEntry{"pkg/util/x.go:two": {2, "r"}}, nil); len(v) != 0 {
		t.Errorf("matching count: violations %q", v)
	}
	stale := newHostGitDoorScan().violations(map[string]hostGitDoorEntry{"pkg/util/x.go:gone": {1, "r"}}, nil)
	if len(stale) != 1 {
		t.Errorf("an entry with no site: violations %q, want one", stale)
	}
	door := "package util\nimport \"os/exec\"\nfunc HostGitCommand() { _ = exec.Command(\"git\") }\n"
	if v := scanHostGitDoorSource(t, map[string]string{"pkg/util/git_host.go": door}).violations(nil, nil); len(v) != 0 {
		t.Errorf("the door was flagged: %q", v)
	}

	// A step table run through a variable command: two "git" literals plus one
	// non-constant start make a count of 3.
	table := `package maintenance
import ("context"; "os/exec")
type step struct{ label, cmd string; args []string }
func run(ctx context.Context) {
	steps := []step{{"fetch", "git", []string{"fetch"}}}
	steps = append(steps, step{"reset", "git", []string{"reset", "--hard"}})
	for _, s := range steps {
		_ = exec.CommandContext(ctx, s.cmd, s.args...)
	}
}
`
	tscan := scanHostGitDoorSource(t, map[string]string{"pkg/hub/maintenance/run.go": table})
	const key = "pkg/hub/maintenance/run.go:run"
	if v := tscan.violations(nil, nil); len(v) != 1 || !strings.Contains(v[0], key) {
		t.Errorf("step table without an entry: violations %q, want one for %s", v, key)
	}
	if v := tscan.violations(map[string]hostGitDoorEntry{key: {2, "r"}}, nil); len(v) != 1 {
		t.Errorf("step table with count 2: violations %q, want one (the count is 3)", v)
	}
	if v := tscan.violations(map[string]hostGitDoorEntry{key: {3, "r"}}, nil); len(v) != 0 {
		t.Errorf("step table with count 3: violations %q", v)
	}
}

// Ceiling (c): a CeilingOff use without an allowlist entry fails the audit,
// as a literal field or an assignment; CeilingOn, git_host.go and unrelated
// Ceiling fields do not.
func TestHostGitGoesThroughOneDoor_CeilingOffNeedsEntry(t *testing.T) {
	const head = "package runtimebroker\nimport (\"context\"; \"github.com/GoogleCloudPlatform/scion/pkg/util\")\nvar _ = context.Background\nfunc opts() *util.HostGitOptions { return nil }\ntype holder struct{ o util.HostGitOptions }\ntype wrap struct{ util.HostGitOptions }\ntype root struct{ Ceiling int }\n"
	for name, fn := range map[string]string{
		"literal":            `func probe(ctx context.Context, d string) { _ = util.RunHostGit(ctx, d, util.HostGitOptions{Ceiling: util.CeilingOff, Audience: util.AudienceRemote}, "rev-parse") }`,
		"assignment":         `func probe(ctx context.Context, d string) { var o util.HostGitOptions; o.Ceiling = util.CeilingOff; _ = util.RunHostGit(ctx, d, o, "rev-parse") }`,
		"converted":          `func probe() { _ = util.HostGitOptions{Ceiling: util.HostGitCeiling(1)} }`,
		"parameter":          `func probe(o *util.HostGitOptions) { o.Ceiling = 1 }`,
		"call result":        `func probe() { opts().Ceiling = util.CeilingOn + 1 }`,
		"field":              `func probe(h *holder) { h.o.Ceiling = 1 }`,
		"increment":          `func probe(o *util.HostGitOptions) { o.Ceiling++ }`,
		"typed var":          `func probe() { var c util.HostGitCeiling = 1; _ = c }`,
		"slice":              `func probe() { _ = []util.HostGitCeiling{1} }`,
		"embedded":           `func probe(w *wrap) { w.Ceiling = 1 }`,
		"unkeyed":            `func probe() { _ = util.HostGitOptions{nil, nil} }`,
		"address":            `func probe(o *util.HostGitOptions) { p := &o.Ceiling; *p = 1 }`,
		"shadowed":           `func probe(c util.HostGitCeiling) { { o := root{}; _ = o }; o := util.HostGitOptions{}; o.Ceiling = c }`,
		"funclit param":      `func probe(c util.HostGitCeiling) { f := func(o root) { _ = o }; _ = f; o := util.HostGitOptions{}; o.Ceiling = c }`,
		"package var shadow": "var hopts util.HostGitOptions\nfunc probe(c util.HostGitCeiling) { { hopts := root{}; _ = hopts }; hopts.Ceiling = c }",
		"elided slice":       `func probe() { _ = []util.HostGitOptions{{Ceiling: 1}} }`,
		"elided map":         `func probe(c util.HostGitCeiling) { _ = map[string]util.HostGitOptions{"a": {Ceiling: c}} }`,
		"elided unkeyed":     `func probe() { _ = []util.HostGitOptions{{nil, nil}} }`,
		"elided pointer":     `func probe() { _ = []*util.HostGitOptions{{Ceiling: 1}} }`,
	} {
		t.Run(name, func(t *testing.T) {
			scan := scanHostGitDoorSource(t, map[string]string{"pkg/runtimebroker/probe.go": head + fn + "\n"})
			v := scan.violations(nil, nil)
			if len(v) != 1 || !strings.Contains(v[0], "CeilingOff in pkg/runtimebroker/probe.go:probe") {
				t.Errorf("violations = %q, want one CeilingOff finding", v)
			}
			allow := map[string]hostGitDoorEntry{"pkg/runtimebroker/probe.go:probe": {1, "fixture reason"}}
			if v := scan.violations(nil, allow); len(v) != 0 {
				t.Errorf("with an allowlist entry (count 1): violations %q", v)
			}
		})
	}
	for name, src := range map[string]string{
		"CeilingOn":           "package runtimebroker\nimport \"github.com/GoogleCloudPlatform/scion/pkg/util\"\nvar o = util.HostGitOptions{Ceiling: util.CeilingOn}\nfunc f() { o.Ceiling = util.CeilingOn }\n",
		"other Ceiling type":  "package hub\ntype chain struct{ Ceiling int }\nfunc f(c int) { x := chain{Ceiling: c}; x.Ceiling = c + 1; _ = x }\n",
		"typed other var":     "package hub\ntype root struct{ Ceiling int }\nfunc f(c int) { var r root; r.Ceiling = c; r.Ceiling++ }\n",
		"typed other param":   "package hub\ntype write struct{ Ceiling int }\nfunc f(w *write, c int) { w.Ceiling = c }\n",
		"zero HostGitCeiling": "package runtimebroker\nimport \"github.com/GoogleCloudPlatform/scion/pkg/util\"\nfunc f() { var c util.HostGitCeiling; c = util.CeilingOn; _ = c; _ = util.HostGitCeiling(util.CeilingOn) }\n",
	} {
		if v := scanHostGitDoorSource(t, map[string]string{"pkg/hub/" + name + ".go": src}).violations(nil, nil); len(v) != 0 {
			t.Errorf("%s was flagged: %q", name, v)
		}
	}
	// The Ceiling checks cover the whole module: a site outside the door
	// roots fails too.
	outside := "package main\nimport \"github.com/GoogleCloudPlatform/scion/pkg/util\"\nfunc probe() { _ = util.HostGitOptions{Ceiling: util.CeilingOff} }\n"
	if v := scanHostGitDoorSource(t, map[string]string{"cmd/scion/probe.go": outside}).violations(nil, nil); len(v) != 1 || !strings.Contains(v[0], "CeilingOff in cmd/scion/probe.go:probe") {
		t.Errorf("outside the door roots: violations %q, want one CeilingOff finding", v)
	}
	// An alias or defined type over HostGitOptions is a finding of its own, so
	// its literals and writes cannot slip past the checks.
	for name, src := range map[string]string{
		"alias":         "package runtimebroker\nimport \"github.com/GoogleCloudPlatform/scion/pkg/util\"\ntype alias = util.HostGitOptions\nfunc probe(a *alias) { a.Ceiling = 1 }\n",
		"alias literal": "package runtimebroker\nimport \"github.com/GoogleCloudPlatform/scion/pkg/util\"\ntype o = util.HostGitOptions\nfunc probe() { _ = o{Ceiling: 1} }\n",
		"defined type":  "package runtimebroker\nimport \"github.com/GoogleCloudPlatform/scion/pkg/util\"\ntype mine util.HostGitOptions\nfunc probe() { _ = mine{Ceiling: 1} }\n",
	} {
		v := scanHostGitDoorSource(t, map[string]string{"pkg/runtimebroker/probe.go": src}).violations(nil, nil)
		found := false
		for _, x := range v {
			found = found || strings.Contains(x, "over HostGitOptions")
		}
		if !found {
			t.Errorf("%s: violations %q, want the type over HostGitOptions flagged", name, v)
		}
	}
	// A stand-in for CeilingOn is not CeilingOn: a local constant of that name,
	// or a field of that name on a local struct, fails (and its declaration is
	// a finding of its own).
	for name, src := range map[string]string{
		"local const": "package runtimebroker\nimport \"github.com/GoogleCloudPlatform/scion/pkg/util\"\nconst CeilingOn = 1\nfunc probe(o *util.HostGitOptions) { o.Ceiling = CeilingOn }\n",
		"local field": "package runtimebroker\nimport \"github.com/GoogleCloudPlatform/scion/pkg/util\"\ntype st struct{ CeilingOn util.HostGitCeiling }\nfunc probe(o *util.HostGitOptions, s st) { o.Ceiling = s.CeilingOn }\n",
	} {
		v := scanHostGitDoorSource(t, map[string]string{"pkg/runtimebroker/probe.go": src}).violations(nil, nil)
		var write, decl bool
		for _, x := range v {
			write = write || strings.Contains(x, "CeilingOff in pkg/runtimebroker/probe.go:probe")
			decl = decl || strings.Contains(x, "declares CeilingOn")
		}
		if !write || !decl {
			t.Errorf("%s: violations %q, want the .Ceiling write and the declaration flagged", name, v)
		}
	}
	// A value carried through a variable is still counted where CeilingOff is
	// named.
	indirect := "package runtimebroker\nimport \"github.com/GoogleCloudPlatform/scion/pkg/util\"\nfunc probe() { c := util.CeilingOff; o := util.HostGitOptions{}; o.Ceiling = c; _ = o }\n"
	if v := scanHostGitDoorSource(t, map[string]string{"pkg/runtimebroker/probe.go": indirect}).violations(nil, nil); len(v) != 1 {
		t.Errorf("indirect CeilingOff: violations %q, want one entry", v)
	}
}

// A RepoConfigRefusedError variable passed to slog or to a %+v / %#v format
// fails the audit; %v and Error() do not.
func TestHostGitGoesThroughOneDoor_RefusalSinks(t *testing.T) {
	for body, want := range map[string]bool{
		`slog.Info("refused", "error", refused)`:           true,
		`slog.Warn("refused", slog.Any("error", refused))`: true,
		`logger.Error("refused", "error", refused)`:        true,
		`_ = fmt.Sprintf("%+v", refused)`:                  true,
		`_ = fmt.Errorf("x: %#v", *refused)`:               true,
		`_ = fmt.Sprintf("%v", refused)`:                   false,
		`slog.Info("refused", "error", refused.Error())`:   false,
		`_ = errors.As(err, &refused)`:                     false,
	} {
		src := "package agent\nimport (\"errors\"; \"fmt\"; \"log/slog\"; \"github.com/GoogleCloudPlatform/scion/pkg/util\")\nfunc f(err error, logger *slog.Logger) {\nvar refused *util.RepoConfigRefusedError\n" + body + "\n}\n"
		got := len(scanHostGitDoorSource(t, map[string]string{"pkg/agent/f.go": src}).violations(nil, nil)) > 0
		if got != want {
			t.Errorf("%s: flagged = %v, want %v", body, got, want)
		}
	}
}

// hostGitCheckerFuncs are the checks that exec git outside the door (file in
// pkg/util, function, and the one parameter allowed to carry the pins in).
var hostGitCheckerFuncs = []struct{ file, fn, pinParam string }{
	{"git_host.go", "readFilterPins", "lead"},
	{"git_config_policy.go", "crossCheckAgainstGit", ""},
	{"git_seed_submodules.go", "seedGitlinks", ""},
}

// The checks that exec git outside the door run the constant "git" with the
// HostGitArgs pins first: exactly one exec each, its command the literal
// "git", its arguments one slice built from HostGitArgs() (readFilterPins:
// from the lead parameter, which HostGitCommand builds from HostGitArgs()
// and passes in).
func TestHostGitGoesThroughOneDoor_CheckerArgsArePinned(t *testing.T) {
	fset := token.NewFileSet()
	decls := map[string]*ast.FuncDecl{}
	for _, file := range []string{"git_host.go", "git_config_policy.go", "git_seed_submodules.go"} {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
				decls[file+":"+fd.Name.Name] = fd
			}
		}
	}
	scan := scanHostGitDoorTree(t)
	for _, c := range hostGitCheckerFuncs {
		fd := decls[c.file+":"+c.fn]
		if fd == nil {
			t.Errorf("%s: %s not found", c.file, c.fn)
			continue
		}
		if err := checkPinnedGitExec(fd, "exec", c.pinParam); err != nil {
			t.Errorf("%s: %v", c.fn, err)
		}
		// The audit sees the same: one literal git exec, nothing dynamic.
		fn := scan.funcs["pkg/util/"+c.file+":"+c.fn]
		if fn == nil || len(fn.gitExecs) != 1 || len(fn.dynExecs) != 0 || fn.constGit != 0 {
			t.Errorf("%s: the audit does not see exactly one literal git exec", c.fn)
		}
	}
	if fd := decls["git_host.go:HostGitCommand"]; fd == nil {
		t.Error("HostGitCommand not found")
	} else if err := checkDoorLead(fd); err != nil {
		t.Errorf("HostGitCommand: %v", err)
	}
}

// checkPinnedGitExec checks that fd execs git exactly once, with the command
// the literal "git" and the arguments one slice built from HostGitArgs()
// (through append), or from pinParam when it is one of fd's parameters.
func checkPinnedGitExec(fd *ast.FuncDecl, execName, pinParam string) error {
	params := map[string]bool{}
	for _, field := range fd.Type.Params.List {
		for _, n := range field.Names {
			params[n.Name] = true
		}
	}
	locals := map[string]ast.Expr{}
	var calls []*ast.CallExpr
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if len(x.Lhs) == len(x.Rhs) {
				for i, lhs := range x.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						locals[id.Name] = x.Rhs[i]
					}
				}
			}
		case *ast.CallExpr:
			if isSelector(x.Fun, execName, "Command") || isSelector(x.Fun, execName, "CommandContext") {
				calls = append(calls, x)
			}
		}
		return true
	})
	if len(calls) != 1 {
		return fmt.Errorf("%d exec calls, want exactly 1", len(calls))
	}
	call := calls[0]
	nameIdx := 0
	if isSelector(call.Fun, execName, "CommandContext") {
		nameIdx = 1
	}
	if len(call.Args) != nameIdx+2 || !call.Ellipsis.IsValid() {
		return errors.New("the arguments are not one slice after the command")
	}
	lit, ok := call.Args[nameIdx].(*ast.BasicLit)
	if !ok || lit.Value != strconv.Quote("git") {
		return errors.New(`the command is not the literal "git"`)
	}
	var pinned func(e ast.Expr, depth int) bool
	pinned = func(e ast.Expr, depth int) bool {
		if depth > 8 {
			return false
		}
		switch x := e.(type) {
		case *ast.CallExpr:
			id, ok := x.Fun.(*ast.Ident)
			if !ok {
				return false
			}
			switch {
			case id.Name == "HostGitArgs" && len(x.Args) == 0:
				return true
			case id.Name == "append" && len(x.Args) >= 1:
				// append([]string{}, s...) copies s.
				if cl, ok := x.Args[0].(*ast.CompositeLit); ok && len(cl.Elts) == 0 && len(x.Args) == 2 && x.Ellipsis.IsValid() {
					return pinned(x.Args[1], depth+1)
				}
				return pinned(x.Args[0], depth+1)
			}
		case *ast.Ident:
			if pinParam != "" && x.Name == pinParam && params[x.Name] {
				return true
			}
			if v, ok := locals[x.Name]; ok {
				return pinned(v, depth+1)
			}
		}
		return false
	}
	if !pinned(call.Args[nameIdx+1], 0) {
		return errors.New("the arguments are not built from HostGitArgs()")
	}
	return nil
}

// checkDoorLead checks that HostGitCommand starts lead from HostGitArgs(),
// only appends to it afterwards, and passes it to readFilterPins.
func checkDoorLead(fd *ast.FuncDecl) error {
	var assigns []*ast.AssignStmt
	passed := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name == "lead" {
					assigns = append(assigns, x)
				}
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "readFilterPins" && len(x.Args) >= 3 {
				if a, ok := x.Args[2].(*ast.Ident); ok && a.Name == "lead" {
					passed = true
				}
			}
		}
		return true
	})
	if len(assigns) == 0 {
		return errors.New("no lead assignment")
	}
	for i, a := range assigns {
		if len(a.Lhs) != 1 || len(a.Rhs) != 1 {
			return errors.New("lead is assigned in a multi-value assignment")
		}
		call, ok := a.Rhs[0].(*ast.CallExpr)
		if !ok {
			return errors.New("lead is assigned something other than a call")
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok {
			return errors.New("lead is assigned an unexpected call")
		}
		if i == 0 {
			if a.Tok != token.DEFINE || id.Name != "HostGitArgs" || len(call.Args) != 0 {
				return errors.New("lead does not start as HostGitArgs()")
			}
			continue
		}
		if id.Name != "append" || len(call.Args) < 1 {
			return errors.New("lead is reassigned other than by append")
		}
		if base, ok := call.Args[0].(*ast.Ident); !ok || base.Name != "lead" {
			return errors.New("lead is reassigned from another base")
		}
	}
	if !passed {
		return errors.New("readFilterPins is not passed lead")
	}
	return nil
}

// The pin checks refuse arguments not built from HostGitArgs(), a command
// other than the literal "git", a second exec, and a door whose lead does not
// start from HostGitArgs().
func TestHostGitGoesThroughOneDoor_CheckerPinChecksRefuse(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", `package util
import ("context"; "os/exec")
func good(ctx context.Context) { args := append(HostGitArgs(), "config"); _ = exec.CommandContext(ctx, "git", args...) }
func goodParam(ctx context.Context, lead []string) { a := append(append([]string{}, lead...), "x"); _ = exec.CommandContext(ctx, "git", a...) }
func literalArgs(ctx context.Context) { args := []string{"config"}; _ = exec.CommandContext(ctx, "git", args...) }
func spread() { _ = exec.Command("git", "status") }
func otherName(ctx context.Context, bin string) { args := append(HostGitArgs(), "x"); _ = exec.CommandContext(ctx, bin, args...) }
func twice() { args := append(HostGitArgs(), "x"); _ = exec.Command("git", args...); _ = exec.Command("git", args...) }
func otherParam(ctx context.Context, extra []string) { _ = exec.CommandContext(ctx, "git", extra...) }
func doorGood(ctx context.Context) { lead := HostGitArgs(); lead = append(lead, "-c", "x"); readFilterPins(ctx, "", lead, nil) }
func doorBad(ctx context.Context) { lead := []string{}; readFilterPins(ctx, "", lead, nil) }
func doorRebased(ctx context.Context, other []string) { lead := HostGitArgs(); lead = append(other, "x"); readFilterPins(ctx, "", lead, nil) }
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	fds := map[string]*ast.FuncDecl{}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			fds[fd.Name.Name] = fd
		}
	}
	if err := checkPinnedGitExec(fds["good"], "exec", ""); err != nil {
		t.Errorf("good: %v", err)
	}
	if err := checkPinnedGitExec(fds["goodParam"], "exec", "lead"); err != nil {
		t.Errorf("goodParam: %v", err)
	}
	for _, name := range []string{"literalArgs", "spread", "otherName", "twice", "otherParam"} {
		if err := checkPinnedGitExec(fds[name], "exec", "lead"); err == nil {
			t.Errorf("%s passed the pin check", name)
		}
	}
	if err := checkDoorLead(fds["doorGood"]); err != nil {
		t.Errorf("doorGood: %v", err)
	}
	for _, name := range []string{"doorBad", "doorRebased"} {
		if err := checkDoorLead(fds[name]); err == nil {
			t.Errorf("%s passed the door lead check", name)
		}
	}
}
