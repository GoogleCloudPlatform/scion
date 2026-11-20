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

package commands

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type initEnvironSite struct {
	File string
	Decl string
}

func (s initEnvironSite) String() string {
	return fmt.Sprintf("%s:%s", s.File, s.Decl)
}

// allowedInitEnvironSites lists the only non-test init*.go declarations
// permitted to reference os.Environ:
//   - secret staging / environ-purge re-exec;
//   - workload .scion/scion-env file handoff;
//   - supervisor workload handoffs (scheduled exec and SSH sessions).
//
// Startup child processes (hooks, git, etc.) must build their environments
// through initenv or rootexec rather than inheriting os.Environ().
var allowedInitEnvironSites = map[initEnvironSite]string{
	{File: "init.go", Decl: "reExecWithCleanEnv"}:      "environ-purge re-exec after secret staging",
	{File: "init.go", Decl: "writeEnvFile"}:            "writes SCION_* workload recovery file ~/.scion/scion-env",
	{File: "init_conduit.go", Decl: "harnessShellEnv"}: "supervisor scheduled-exec workload env handoff",
	{File: "init_conduit.go", Decl: "sshSessionEnv"}:   "supervisor SSH-session workload env handoff",
}

func findOSEnvironRefs(filename string, src any) ([]initEnvironSite, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}

	osAliases := map[string]bool{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != "os" {
			continue
		}
		if imp.Name != nil {
			if imp.Name.Name != "_" && imp.Name.Name != "." {
				osAliases[imp.Name.Name] = true
			}
		} else {
			osAliases["os"] = true
		}
	}
	if len(osAliases) == 0 {
		return nil, nil
	}

	base := filepath.Base(filename)
	var refs []initEnvironSite

	scanNode := func(declName string, node ast.Node) {
		ast.Inspect(node, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Environ" {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || !osAliases[ident.Name] {
				return true
			}
			refs = append(refs, initEnvironSite{File: base, Decl: declName})
			return true
		})
	}

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				name = recvTypeName(d.Recv.List[0].Type) + "." + name
			}
			if d.Body != nil {
				scanNode(name, d.Body)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				declName := "_"
				if len(vs.Names) > 0 {
					declName = vs.Names[0].Name
				}
				for _, val := range vs.Values {
					scanNode(declName, val)
				}
			}
		}
	}

	return refs, nil
}

func recvTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return recvTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return recvTypeName(t.X)
	default:
		return ""
	}
}

func TestInitFiles_NoStrayOSEnviron(t *testing.T) {
	matches, err := filepath.Glob("init*.go")
	if err != nil {
		t.Fatalf("glob init*.go: %v", err)
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		t.Fatal("expected at least one init*.go file")
	}

	seenAllowed := make(map[initEnvironSite]bool, len(allowedInitEnvironSites))
	var unexpected []string

	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		refs, err := findOSEnvironRefs(path, nil)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, ref := range refs {
			if _, ok := allowedInitEnvironSites[ref]; ok {
				seenAllowed[ref] = true
				continue
			}
			unexpected = append(unexpected, ref.String())
		}
	}

	if len(unexpected) > 0 {
		t.Errorf("unexpected os.Environ reference(s) in non-test init*.go files (use initenv or rootexec for startup children):\n  %s",
			strings.Join(unexpected, "\n  "))
	}

	for site := range allowedInitEnvironSites {
		if !seenAllowed[site] {
			t.Errorf("stale entry in allowedInitEnvironSites: %s no longer references os.Environ; remove it from the allowlist", site)
		}
	}
}

func TestFindOSEnvironRefs_DetectsDirectAliasedAndValueRefs(t *testing.T) {
	src := `package commands
import (
	"os"
	myos "os"
)

// os.Environ() in a comment must be ignored.
func directCall() []string { return os.Environ() }
func aliasedCall() []string { return myos.Environ() }
var valueRef = func() func() []string { return os.Environ }
`
	refs, err := findOSEnvironRefs("init_synthetic.go", src)
	if err != nil {
		t.Fatalf("findOSEnvironRefs: %v", err)
	}
	want := []initEnvironSite{
		{File: "init_synthetic.go", Decl: "directCall"},
		{File: "init_synthetic.go", Decl: "aliasedCall"},
		{File: "init_synthetic.go", Decl: "valueRef"},
	}
	if len(refs) != len(want) {
		t.Fatalf("refs = %v, want %v", refs, want)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Errorf("refs[%d] = %v, want %v", i, refs[i], want[i])
		}
	}
}
