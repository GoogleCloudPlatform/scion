package hub

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardWalksTree(t *testing.T) {
	fset := token.NewFileSet()
	// pkgmove:scan-covers hub/other
	// a stale marker for an earlier move; it does not cover hub/sub.
	// pkgmove:scan-covers hub/...
	// the walk covers the package directory and everything under it.
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
			return err
		}
		_, err = parser.ParseFile(fset, p, nil, parser.PackageClauseOnly)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
