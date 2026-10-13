package hub

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Guard: Handler must be declared in a scanned file. The scan covers the
// package directory and its sub directory, so it keeps working after the move.
func TestGuardScansHandler(t *testing.T) {
	fset := token.NewFileSet()
	found := false
	// pkgmove:scan-covers hub/sub
	// the loop reads both the package directory and sub.
	for _, dir := range []string{".", "sub"} {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			n := e.Name()
			if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "Handler" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("guard lost coverage: Handler not found")
	}
}
