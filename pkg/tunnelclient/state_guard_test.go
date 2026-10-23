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

package tunnelclient

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// tempByName finds the places in src that act on a temporary file by name:
// any Tree Rename call (temporary files are renamed into place through
// their open handle, RenameOpen), and any Remove call whose argument is a
// temporary name. Temporary names are the second result of CreateTemp,
// variables built from a ".tmp" literal, and identifiers containing "tmp".
func tempByName(t *testing.T, src string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "state.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	temp := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range as.Rhs {
			if call, ok := rhs.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "CreateTemp" && len(as.Lhs) >= 2 {
					if id, ok := as.Lhs[1].(*ast.Ident); ok && id.Name != "_" {
						temp[id.Name] = true
					}
				}
			}
			hasTmpLit := false
			ast.Inspect(rhs, func(m ast.Node) bool {
				if lit, ok := m.(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.Contains(lit.Value, ".tmp") {
					hasTmpLit = true
				}
				return true
			})
			if hasTmpLit && i < len(as.Lhs) {
				if id, ok := as.Lhs[i].(*ast.Ident); ok {
					temp[id.Name] = true
				}
			}
		}
		return true
	})
	isTemp := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return ok && (temp[id.Name] || strings.Contains(strings.ToLower(id.Name), "tmp"))
	}
	var found []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "os" {
			switch sel.Sel.Name {
			case "Rename", "Remove", "RemoveAll", "Open", "OpenFile", "Create", "CreateTemp", "WriteFile", "ReadFile", "ReadDir", "Mkdir", "MkdirAll", "Lstat", "Stat", "Chmod":
				found = append(found, fset.Position(call.Pos()).String()+" os."+sel.Sel.Name+" (state files go through the privatefs Tree)")
			}
			return true
		}
		switch sel.Sel.Name {
		case "Rename":
			found = append(found, fset.Position(call.Pos()).String()+" Rename by name (use RenameOpen on the open handle)")
		case "Remove":
			if len(call.Args) == 1 && isTemp(call.Args[0]) {
				found = append(found, fset.Position(call.Pos()).String()+" Remove of a temporary name (use RemoveOpen on the open handle)")
			}
		}
		return true
	})
	return found
}

// TestStateGuard_NoTempFileByName fails if state.go renames or removes a
// temporary file by name. Temporary records and lock files must be renamed
// into place, and removed on failure, through their open handle, so a file
// swapped in under the temporary name never becomes the record or the
// lock. The behavioural regression test for the pattern is filelock's TestLockFile_ShareDeleteRename and privatefs's
// TestRenameOpen_SwappedTempNeverLands / TestRemoveOpen_DeletesHandleNotName
// on Windows.
func TestStateGuard_NoTempFileByName(t *testing.T) {
	// Positive control: the pre-r3 code is flagged at all three places.
	const old = `package tunnelclient
func open(t *privatefs.Tree, base, id string) error {
	tmpLock := base + "/." + id + ".lock.tmp"
	lf, _ := t.Create(tmpLock, true, privatefs.Private)
	_ = lf
	if err := t.Rename(tmpLock, base+"/"+id+".lock"); err != nil {
		_ = t.Remove(tmpLock)
		return err
	}
	return nil
}
func write(r *fileRecord) error {
	tmp, name, _ := r.tree.CreateTemp(r.base, ".tmp-*", privatefs.Private)
	_ = tmp
	defer func() { _ = r.tree.Remove(name) }()
	return nil
}
`
	if got := tempByName(t, old); len(got) != 3 {
		t.Fatalf("detector on the pre-r3 sample found %v, want the Rename and both temp Removes", got)
	}
	// Control: removing a visible name is fine.
	const ok = `package tunnelclient
func prune(t *privatefs.Tree, recRel string) { _ = t.Remove(recRel) }
`
	if got := tempByName(t, ok); len(got) != 0 {
		t.Fatalf("detector flagged a visible-name Remove: %v", got)
	}

	src, err := readSource("state.go")
	if err != nil {
		t.Fatal(err)
	}
	if got := tempByName(t, src); len(got) != 0 {
		t.Fatalf("state.go acts on temporary files by name:\n%s", strings.Join(got, "\n"))
	}
}

func readSource(name string) (string, error) {
	b, err := os.ReadFile(name)
	return string(b), err
}
