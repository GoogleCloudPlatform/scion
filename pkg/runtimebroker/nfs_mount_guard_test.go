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

package runtimebroker

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// mountCommandNames are the command names (compared after path.Base, so
// "/bin/mount" counts) that mount or unmount a filesystem.
var mountCommandNames = map[string]bool{"mount": true, "umount": true, "mount.nfs": true, "mount.nfs4": true}

// mountSyscallPackages are the import paths whose mount-family functions
// the guard refuses outside the exec layer.
var mountSyscallPackages = map[string]bool{"syscall": true, "golang.org/x/sys/unix": true}

// isMountSyscall reports whether name is a mount-family function, or a raw
// mount-family syscall number, of syscall or golang.org/x/sys/unix.
func isMountSyscall(name string) bool {
	switch name {
	case "Mount", "Unmount", "MountSetattr", "Fsmount", "Fsopen", "Fspick", "OpenTree", "MoveMount",
		"SYS_MOUNT", "SYS_UMOUNT", "SYS_UMOUNT2", "SYS_FSMOUNT", "SYS_MOVE_MOUNT", "SYS_OPEN_TREE",
		"SYS_FSOPEN", "SYS_FSCONFIG", "SYS_FSPICK", "SYS_MOUNT_SETATTR":
		return true
	}
	return strings.HasPrefix(name, "Fsconfig")
}

// allowedMountExecs are the only functions (file:func) that may run a
// mount command: the exec layer of the NFS reconciler.
var allowedMountExecs = map[string]bool{
	"pkg/runtimebroker/nfs_mount_exec.go:Mount":                true,
	"pkg/runtimebroker/nfs_mount_exec.go:RemountRequiredFlags": true,
	"pkg/runtimebroker/nfs_mount_exec.go:Unmount":              true,
}

// moduleRoot returns the repository root (the directory holding go.mod),
// two levels above this package.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	return root
}

// parseModuleGoFiles parses every non-test Go file of the root module
// (directories with their own go.mod, vendor, node_modules, testdata and
// hidden directories are skipped) and calls fn for each.
func parseModuleGoFiles(t *testing.T, root string, fn func(rel string, fset *token.FileSet, f *ast.File)) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" || name == "testdata") {
				return filepath.SkipDir
			}
			if path != root {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Errorf("parse %s: %v", path, perr)
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		fn(filepath.ToSlash(rel), fset, f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// mountSites scans one parsed file. It returns a violation for every
// reference (a call, or the function used as a value) to a mount-family
// function or raw mount-family syscall number (SYS_MOUNT and the like) of
// syscall or golang.org/x/sys/unix, under any import name or dot-imported,
// and for every string literal, in a
// function or at package level (so a named constant counts), whose
// path.Base is a mount command name, unless it sits in an allowed exec
// function. allowed lists the allowed functions that hold such a literal.
func mountSites(rel string, fset *token.FileSet, f *ast.File) (violations, allowed []string) {
	pkgNames := map[string]bool{} // local names of the syscall packages
	dotImported := false
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if !mountSyscallPackages[p] {
			continue
		}
		name := path.Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		switch name {
		case ".":
			dotImported = true
		case "_":
		default:
			pkgNames[name] = true
		}
	}
	// funcAt maps a position to the enclosing top-level function name.
	type span struct {
		from, to token.Pos
		name     string
	}
	var funcs []span
	for _, decl := range f.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok {
			funcs = append(funcs, span{fd.Pos(), fd.End(), fd.Name.Name})
		}
	}
	funcAt := func(pos token.Pos) string {
		for _, s := range funcs {
			if pos >= s.from && pos < s.to {
				return s.name
			}
		}
		return ""
	}
	// Names that are not references to a package-level identifier: selector
	// names (x.Name), declared function and method names, struct field
	// names and composite-literal keys. With a dot import, a method or field
	// called Mount must not count.
	selNames := map[*ast.Ident]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			selNames[n.Sel] = true
		case *ast.FuncDecl:
			selNames[n.Name] = true
		case *ast.Field:
			for _, name := range n.Names {
				selNames[name] = true
			}
		case *ast.KeyValueExpr:
			if k, ok := n.Key.(*ast.Ident); ok {
				selNames[k] = true
			}
		}
		return true
	})
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := n.X.(*ast.Ident); ok && pkgNames[pkg.Name] && isMountSyscall(n.Sel.Name) {
				violations = append(violations, fmt.Sprintf("%s: %s.%s reference", fset.Position(n.Pos()), pkg.Name, n.Sel.Name))
			}
		case *ast.Ident:
			if dotImported && !selNames[n] && isMountSyscall(n.Name) {
				if obj := n.Obj; obj == nil || obj.Kind != ast.Fun {
					violations = append(violations, fmt.Sprintf("%s: %s reference (dot-imported syscall package)", fset.Position(n.Pos()), n.Name))
				}
			}
		case *ast.BasicLit:
			if n.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(n.Value)
			if err != nil || !mountCommandNames[path.Base(v)] {
				return true
			}
			key := rel + ":" + funcAt(n.Pos())
			if allowedMountExecs[key] {
				allowed = append(allowed, key)
				return true
			}
			violations = append(violations, fmt.Sprintf("%s: %q outside the NFS exec layer (%s)", fset.Position(n.Pos()), v, key))
		}
		return true
	})
	return violations, allowed
}

// TestNFSMountGuard_OnlyExecLayerRunsMount: no non-test Go file in the
// module runs a mount command or calls a mount syscall, except the
// reconciler's exec layer. Any new mount site has to go through it, and so
// through HardenNFSMountOptions. (A command name built at run time from
// pieces is not detected.)
func TestNFSMountGuard_OnlyExecLayerRunsMount(t *testing.T) {
	root := moduleRoot(t)
	found := map[string]bool{}
	parseModuleGoFiles(t, root, func(rel string, fset *token.FileSet, f *ast.File) {
		violations, allowed := mountSites(rel, fset, f)
		for _, v := range violations {
			t.Errorf("%s; broker mounts go through ExecMountChecker", v)
		}
		for _, a := range allowed {
			found[a] = true
		}
	})
	for key := range allowedMountExecs {
		if !found[key] {
			t.Errorf("%s no longer names a mount command; update this guard", key)
		}
	}
}

// TestNFSMountGuard_SelfTest runs mountSites over small synthetic files,
// one per form the guard must catch, and over clean ones.
func TestNFSMountGuard_SelfTest(t *testing.T) {
	bad := map[string]string{
		"unix.Mount":           `package p; import "golang.org/x/sys/unix"; func f() { unix.Mount("a", "b", "nfs", 0, "") }`,
		"unix.Unmount":         `package p; import "golang.org/x/sys/unix"; func f() { unix.Unmount("b", 0) }`,
		"syscall.Unmount":      `package p; import "syscall"; func f() { syscall.Unmount("b", 0) }`,
		"aliased MountSetattr": `package p; import sys "golang.org/x/sys/unix"; func f() { sys.MountSetattr(0, "", 0, nil) }`,
		"Fsopen":               `package p; import "golang.org/x/sys/unix"; func f() { unix.Fsopen("nfs", 0) }`,
		"FsconfigSetString":    `package p; import "golang.org/x/sys/unix"; func f() { unix.FsconfigSetString(0, "k", "v") }`,
		"OpenTree":             `package p; import "golang.org/x/sys/unix"; func f() { unix.OpenTree(0, "", 0) }`,
		"MoveMount":            `package p; import "golang.org/x/sys/unix"; func f() { unix.MoveMount(0, "", 0, "", 0) }`,
		"dot import":           `package p; import . "golang.org/x/sys/unix"; func f() { Mount("a", "b", "nfs", 0, "") }`,
		"literal mount":        `package p; import "os/exec"; func f() { exec.Command("mount", "-t", "nfs") }`,
		"absolute path":        `package p; import "os/exec"; func f() { exec.Command("/bin/mount") }`,
		"absolute umount":      `package p; import "os/exec"; func f() { exec.Command("/usr/bin/umount", "x") }`,
		"mount.nfs4":           `package p; import "os/exec"; func f() { exec.Command("mount.nfs4") }`,
		"named constant":       `package p; import "os/exec"; const mountCmd = "mount"; func f() { exec.Command(mountCmd) }`,
		"struct field":         `package p; import "os/exec"; func f() { _ = exec.Cmd{Path: "/sbin/mount"} }`,
		"function as value":    `package p; import "golang.org/x/sys/unix"; var m = unix.Mount`,
		"unmount as value":     `package p; import "syscall"; func f() func(string, int) error { return syscall.Unmount }`,
		"raw SYS_MOUNT":        `package p; import "golang.org/x/sys/unix"; func f() { unix.Syscall6(unix.SYS_MOUNT, 0, 0, 0, 0, 0, 0) }`,
		"raw SYS_UMOUNT2":      `package p; import "syscall"; func f() { syscall.RawSyscall(syscall.SYS_UMOUNT2, 0, 0, 0) }`,
		"raw SYS_OPEN_TREE":    `package p; import sys "golang.org/x/sys/unix"; const n = sys.SYS_OPEN_TREE`,
		"dot SYS_FSOPEN":       `package p; import . "golang.org/x/sys/unix"; func f() { Syscall(SYS_FSOPEN, 0, 0, 0) }`,
	}
	for name, src := range bad {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name+".go", src, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if v, _ := mountSites("pkg/x/"+name+".go", fset, f); len(v) == 0 {
			t.Errorf("%s: not caught", name)
		}
	}
	clean := map[string]string{
		"checker method":         `package p; type c struct{}; func (c) Mount() {}; func f(x c) { x.Mount() }`,
		"dot import, own method": `package p; import . "golang.org/x/sys/unix"; type c struct{}; func (c) Mount() {}; func f(x c) { x.Mount(); _ = Getuid() }`,
		"other SYS_ number":      `package p; import "golang.org/x/sys/unix"; func f() { unix.Syscall(unix.SYS_GETPID, 0, 0, 0) }`,
		"other unix call":        `package p; import "golang.org/x/sys/unix"; func f() { unix.Getuid() }`,
		"mountpoint":             `package p; import "os/exec"; func f() { exec.Command("mountpoint", "-q", "/x") }`,
		"proc mounts":            `package p; import "os"; func f() { os.Open("/proc/mounts") }`,
		"docker flag":            `package p; func f() []string { return []string{"--mount", "type=tmpfs"} }`,
	}
	for name, src := range clean {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name+".go", src, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if v, _ := mountSites("pkg/x/"+name+".go", fset, f); len(v) != 0 {
			t.Errorf("%s: false positive %v", name, v)
		}
	}
	// The exec layer's own literal is allowed only in its allowed functions.
	src := `package runtimebroker; func (e *ExecMountChecker) Mount() { e.runCommand(nil, "mount") }; func other() { run("mount") }`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "nfs_mount_exec.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	v, a := mountSites("pkg/runtimebroker/nfs_mount_exec.go", fset, f)
	if len(a) != 1 || len(v) != 1 || !strings.Contains(v[0], "nfs_mount_exec.go:other") {
		t.Errorf("exec layer: violations %v, allowed %v; want only other() refused", v, a)
	}
}

// funcDecl returns the named function or method declared in a file of this
// package.
func funcDecl(t *testing.T, file, name string) *ast.FuncDecl {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range f.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name.Name == name {
			return fd
		}
	}
	t.Fatalf("%s: func %s not found", file, name)
	return nil
}

// callsNamed returns the calls in n whose function is name (a plain
// identifier) or x.name.
func callsNamed(n ast.Node, name string) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(n, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == name {
				calls = append(calls, call)
			}
		case *ast.SelectorExpr:
			if fn.Sel.Name == name {
				calls = append(calls, call)
			}
		}
		return true
	})
	return calls
}

// TestNFSMountGuard_ExecMountHardensOptions: ExecMountChecker.Mount passes
// its options through HardenNFSMountOptions before it runs mount, and the
// in-place remount runs with the required flags.
func TestNFSMountGuard_ExecMountHardensOptions(t *testing.T) {
	mount := funcDecl(t, "nfs_mount_exec.go", "Mount")
	harden := callsNamed(mount, "HardenNFSMountOptions")
	run := callsNamed(mount, "runCommand")
	if len(harden) != 1 || len(run) != 1 || harden[0].Pos() > run[0].Pos() {
		t.Fatalf("ExecMountChecker.Mount must call HardenNFSMountOptions once before runCommand (harden=%d run=%d)", len(harden), len(run))
	}
	remount := funcDecl(t, "nfs_mount_exec.go", "RemountRequiredFlags")
	found := false
	ast.Inspect(remount, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "requiredNFSMountFlags" {
			found = true
		}
		return true
	})
	if !found {
		t.Error("ExecMountChecker.RemountRequiredFlags must build its options from requiredNFSMountFlags")
	}
}

// TestNFSMountGuard_ReconcilerUsesEffectiveOptions: every reconciler pass
// gets its mount options from effectiveNFSMountOptions, and every
// MountChecker.Mount call in the reconciler passes those options.
func TestNFSMountGuard_ReconcilerUsesEffectiveOptions(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "nfs_mount.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	passes := 0
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		// Identifiers assigned from effectiveNFSMountOptions in this func.
		effective := map[string]bool{}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				return true
			}
			id, ok := as.Lhs[0].(*ast.Ident)
			if !ok {
				return true
			}
			if c, ok := as.Rhs[0].(*ast.CallExpr); ok {
				if fn, ok := c.Fun.(*ast.Ident); ok && fn.Name == "effectiveNFSMountOptions" {
					effective[id.Name] = true
					return true
				}
			}
			if effective[id.Name] {
				t.Errorf("%s: %s reassigned after effectiveNFSMountOptions", fd.Name.Name, id.Name)
			}
			return true
		})
		for _, call := range callsNamed(fd.Body, "reconcileShares") {
			passes++
			if len(call.Args) != 3 {
				t.Fatalf("reconcileShares call with %d args", len(call.Args))
			}
			switch a := call.Args[2].(type) {
			case *ast.CallExpr:
				if fn, ok := a.Fun.(*ast.Ident); ok && fn.Name == "effectiveNFSMountOptions" {
					continue
				}
			case *ast.Ident:
				if effective[a.Name] {
					continue
				}
			}
			t.Errorf("%s: reconcileShares options do not come from effectiveNFSMountOptions", fd.Name.Name)
		}
		if fd.Recv == nil {
			continue
		}
		for _, call := range callsNamed(fd.Body, "Mount") {
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || len(call.Args) != 5 {
				continue
			}
			if inner, ok := sel.X.(*ast.SelectorExpr); !ok || inner.Sel.Name != "checker" {
				continue
			}
			if fd.Name.Name != "reconcileShareLocked" {
				t.Errorf("%s calls checker.Mount; only reconcileShareLocked may", fd.Name.Name)
				continue
			}
			if id, ok := call.Args[4].(*ast.Ident); !ok || id.Name != "mountOpts" {
				t.Errorf("reconcileShareLocked: checker.Mount must pass mountOpts (the pass's effective options)")
			}
		}
	}
	if passes < 2 {
		t.Errorf("found %d reconcileShares calls, want at least 2 (Reconcile and EnsureShareMounted)", passes)
	}
	// reconcileShareLocked's mountOpts is its parameter, set only by
	// reconcileShares from its own parameter.
	rsl := funcDecl(t, "nfs_mount.go", "reconcileShareLocked")
	ast.Inspect(rsl.Body, func(n ast.Node) bool {
		if as, ok := n.(*ast.AssignStmt); ok {
			for _, l := range as.Lhs {
				if id, ok := l.(*ast.Ident); ok && id.Name == "mountOpts" {
					t.Error("reconcileShareLocked reassigns mountOpts")
				}
			}
		}
		return true
	})
}
