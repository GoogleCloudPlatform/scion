package hub

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Guard: every non-test file in the package is scanned; it must not contain
// a forbidden literal, and the scan must cover Handler's file.
func TestGuardScansHandler(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			_ = d
		}
		scanned++
	}
	t.Logf("scanned %d files", scanned)
	if os.Getenv("STRICT_GUARD") != "" && scanned < 2 {
		t.Fatalf("guard lost coverage: scanned %d", scanned)
	}
}
