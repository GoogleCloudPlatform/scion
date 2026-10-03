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

package substrate

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// runExec must wait on its child through procreap.RunManaged, which
// registers the child's PID so the process-wide SIGCHLD reaper leaves it to
// os/exec's own wait. TestRunExec_SucceedsUnderActiveReaper exercises that
// path under a live reaper, but the race it guards against (a reap pass
// landing between the child's exit and cmd.Wait) depends on scheduling, so
// that test passing does not prove a bare cmd.Run() would fail it.
// Observing the registration directly would need a test-only hook in
// procreap's production code. This guard checks the source instead: runExec
// must call procreap.RunManaged on the command it builds, and must not run
// or wait on any command itself.

// execUnmanagedCmdMethods are the exec.Cmd methods that start or wait on a
// process outside procreap's bookkeeping.
var execUnmanagedCmdMethods = map[string]bool{
	"Run":            true,
	"Start":          true,
	"Wait":           true,
	"Output":         true,
	"CombinedOutput": true,
}

// runManagedViolations parses src as a Go file and reports every way the
// function named funcName departs from "runs its command only through
// procreap.RunManaged": a missing function, a missing RunManaged call, or a
// direct start/wait method call on some value.
func runManagedViolations(src []byte, funcName string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "src.go", src, 0)
	if err != nil {
		return nil, err
	}
	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok && d.Recv == nil && d.Name.Name == funcName {
			fn = d
			break
		}
	}
	if fn == nil || fn.Body == nil {
		return []string{fmt.Sprintf("function %s not found", funcName)}, nil
	}

	var violations []string
	managed := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "procreap" {
			if sel.Sel.Name == "RunManaged" {
				managed = true
			}
			return true
		}
		if execUnmanagedCmdMethods[sel.Sel.Name] {
			violations = append(violations, fmt.Sprintf("%s: direct call to .%s()", fset.Position(call.Pos()), sel.Sel.Name))
		}
		return true
	})
	if !managed {
		violations = append(violations, fmt.Sprintf("%s does not call procreap.RunManaged", funcName))
	}
	return violations, nil
}

// TestRunExec_WaitsThroughRunManaged applies the guard to the real exec.go.
func TestRunExec_WaitsThroughRunManaged(t *testing.T) {
	src, err := os.ReadFile("exec.go")
	if err != nil {
		t.Fatalf("read exec.go: %v", err)
	}
	violations, err := runManagedViolations(src, "runExec")
	if err != nil {
		t.Fatalf("parse exec.go: %v", err)
	}
	if len(violations) > 0 {
		t.Errorf("runExec must run its command only through procreap.RunManaged:\n%s", strings.Join(violations, "\n"))
	}
}

// TestRunManagedViolations_FlagsUnmanagedFixtures are the positive controls:
// the guard must flag a runExec that never calls RunManaged and one that
// waits on the command itself, and must accept the managed shape.
func TestRunManagedViolations_FlagsUnmanagedFixtures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		src      string
		wantHits int
	}{
		{
			name: "managed",
			src: `package p
func runExec() { cmd := build(); _ = procreap.RunManaged(cmd) }`,
			wantHits: 0,
		},
		{
			name: "no RunManaged call",
			src: `package p
func runExec() { cmd := build(); _ = cmd }`,
			wantHits: 1,
		},
		{
			name: "bare cmd.Run",
			src: `package p
func runExec() { cmd := build(); _ = cmd.Run() }`,
			wantHits: 2,
		},
		{
			name: "RunManaged plus a direct Wait",
			src: `package p
func runExec() { cmd := build(); _ = procreap.RunManaged(cmd); _ = cmd.Wait() }`,
			wantHits: 1,
		},
		{
			name: "function missing",
			src: `package p
func other() {}`,
			wantHits: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits, err := runManagedViolations([]byte(tc.src), "runExec")
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			if len(hits) != tc.wantHits {
				t.Errorf("runManagedViolations() = %v, want %d violation(s)", hits, tc.wantHits)
			}
		})
	}
}
