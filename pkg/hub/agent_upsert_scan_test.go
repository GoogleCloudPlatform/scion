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

//go:build !no_sqlite

package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const entPkgPath = "github.com/GoogleCloudPlatform/scion/pkg/ent"

// agentCreateTypes are the generated Agent create builders; a conflict
// clause on either turns the insert into an upsert.
var agentCreateTypes = map[string]bool{"AgentCreate": true, "AgentCreateBulk": true}

// agentUpsertTypes are the generated Agent upsert builders.
var agentUpsertTypes = map[string]bool{"AgentUpsert": true, "AgentUpsertOne": true, "AgentUpsertBulk": true}

// entNamed reports whether t (or *t) is a named type from the generated ent
// package whose name is in names.
func entNamed(t types.Type, names map[string]bool) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	if !ok || n.Obj().Pkg() == nil {
		return false
	}
	return n.Obj().Pkg().Path() == entPkgPath && names[n.Obj().Name()]
}

// typedAgentUpserts type-checks files as package pkgPath and returns
// "file Func" for every function that puts a conflict clause on a value of
// an Agent create builder type, however that value was obtained (chained,
// held in a variable or field, returned by a helper), or that uses a value
// of an Agent upsert type.
func typedAgentUpserts(fset *token.FileSet, pkgPath string, files []*ast.File, imp types.Importer, rel func(*ast.File) string) ([]string, error) {
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
	var typeErrs []error
	conf := types.Config{Importer: imp, Error: func(err error) { typeErrs = append(typeErrs, err) }}
	_, _ = conf.Check(pkgPath, fset, files, info)
	if len(typeErrs) > 0 {
		return nil, errors.Join(typeErrs...)
	}
	var found []string
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			hit := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && (sel.Sel.Name == "OnConflict" || sel.Sel.Name == "OnConflictColumns") &&
					entNamed(info.TypeOf(sel.X), agentCreateTypes) {
					hit = true
				}
				if e, ok := n.(ast.Expr); ok {
					if tv, ok := info.Types[e]; ok && tv.Type != nil && entNamed(tv.Type, agentUpsertTypes) {
						hit = true
					}
				}
				return !hit
			})
			if hit {
				found = append(found, rel(file)+" "+funcKey(fn))
			}
		}
	}
	return found, nil
}

// listedPackage is the subset of `go list -json` output the scan uses.
type listedPackage struct {
	ImportPath string
	Dir        string
	Export     string
	GoFiles    []string
	CgoFiles   []string
	Deps       []string
	Standard   bool
}

// goToolHome is HOME as the test binary started with. Package variables are
// initialised before TestMain isolates HOME, and the go command derives its
// module cache and config location from HOME, so go list runs with this one.
var goToolHome = os.Getenv("HOME")

var (
	moduleListOnce sync.Once
	moduleListPkgs []listedPackage
	moduleListErr  error
)

// listModuleWithExports lists every package in the module and its
// dependencies, with compiler export data, once per test binary.
func listModuleWithExports(root string) ([]listedPackage, error) {
	moduleListOnce.Do(func() {
		goTool, err := exec.LookPath("go")
		if err != nil {
			moduleListErr = fmt.Errorf("locating the go command: %w", err)
			return
		}
		cmd := exec.Command(goTool, "list", "-buildvcs=false", "-export", "-deps",
			"-json=ImportPath,Dir,Export,GoFiles,CgoFiles,Deps,Standard", "./...")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "HOME="+goToolHome)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			moduleListErr = fmt.Errorf("go list: %w: %s", err, stderr.String())
			return
		}
		dec := json.NewDecoder(strings.NewReader(string(out)))
		for {
			var p listedPackage
			if err := dec.Decode(&p); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				moduleListErr = err
				return
			}
			moduleListPkgs = append(moduleListPkgs, p)
		}
	})
	return moduleListPkgs, moduleListErr
}

// exportImporter imports packages from the export data go list reported.
func exportImporter(fset *token.FileSet, pkgs []listedPackage) types.Importer {
	exports := make(map[string]string, len(pkgs))
	for _, p := range pkgs {
		if p.Export != "" {
			exports[p.ImportPath] = p.Export
		}
	}
	return importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		f, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %q", path)
		}
		return os.Open(f)
	})
}

// TestNoTypedAgentUpsertOutsideEnt: no package in the module outside the
// generated ent code upserts agents. This is the type-based companion to
// the syntactic scan in TestAgentRunIDStoreWritersAreAClosedSet: it follows
// a builder through variables, fields and helper returns.
func TestNoTypedAgentUpsertOutsideEnt(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	pkgs, err := listModuleWithExports(root)
	require.NoError(t, err)

	fset := token.NewFileSet()
	imp := exportImporter(fset, pkgs)
	entDir := filepath.Join(root, "pkg", "ent")
	var found []string
	checked := 0
	for _, p := range pkgs {
		if p.Standard || !strings.HasPrefix(p.Dir, root+string(filepath.Separator)) {
			continue
		}
		if p.Dir == entDir || strings.HasPrefix(p.Dir, entDir+string(filepath.Separator)) {
			continue
		}
		dependsOnEnt := false
		for _, d := range p.Deps {
			if d == entPkgPath {
				dependsOnEnt = true
				break
			}
		}
		if !dependsOnEnt {
			continue
		}
		require.Empty(t, p.CgoFiles, "package %s has cgo files the scan cannot type-check", p.ImportPath)
		var files []*ast.File
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, parser.SkipObjectResolution)
			require.NoError(t, err)
			files = append(files, f)
		}
		rel := func(f *ast.File) string {
			r, _ := filepath.Rel(root, fset.Position(f.Pos()).Filename)
			return filepath.ToSlash(r)
		}
		hits, err := typedAgentUpserts(fset, p.ImportPath, files, imp, rel)
		require.NoError(t, err, "type-check %s", p.ImportPath)
		found = append(found, hits...)
		checked++
	}
	sort.Strings(found)
	assert.Positive(t, checked, "no package depending on the ent code was checked")
	assert.Empty(t, found, "agent upsert outside the generated ent code")
}

// agentUpsertFixture holds functions that obtain an Agent create builder in
// different ways; the ones named upsert* put a conflict clause on it (or use
// an upsert builder) and must be found, the ones named plain* must not.
const agentUpsertFixture = `package fixture

import (
	"context"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
)

type holder struct{ create *ent.AgentCreate }

func newCreate(c *ent.Client) *ent.AgentCreate { return c.Agent.Create() }

func upsertChained(ctx context.Context, c *ent.Client) error {
	return c.Agent.Create().OnConflict().UpdateNewValues().Exec(ctx)
}

func upsertViaVariable(ctx context.Context, c *ent.Client) error {
	create := c.Agent.Create()
	create.SetName("n")
	return create.OnConflict().UpdateNewValues().Exec(ctx)
}

func upsertViaDeclaredVariable(ctx context.Context, c *ent.Client) error {
	var create *ent.AgentCreate
	create = c.Agent.Create()
	return create.OnConflictColumns("id").UpdateNewValues().Exec(ctx)
}

func upsertViaField(ctx context.Context, h holder) error {
	return h.create.OnConflict().UpdateNewValues().Exec(ctx)
}

func upsertViaHelper(ctx context.Context, c *ent.Client) error {
	return newCreate(c).OnConflict().UpdateNewValues().Exec(ctx)
}

func upsertViaBulkVariable(ctx context.Context, c *ent.Client) error {
	bulk := c.Agent.CreateBulk(c.Agent.Create())
	return bulk.OnConflict().UpdateNewValues().Exec(ctx)
}

func upsertBuilderHeld(c *ent.Client) {
	var up *ent.AgentUpsertOne
	_ = up
}

func plainCreateViaVariable(ctx context.Context, c *ent.Client) error {
	create := c.Agent.Create()
	return create.Exec(ctx)
}

func plainOtherUpsert(ctx context.Context, c *ent.Client) error {
	create := c.LaunchReaperState.Create()
	return create.OnConflict().UpdateNewValues().Exec(ctx)
}
`

// TestTypedAgentUpsertsFixture pins what the type-based scan finds,
// including a builder held in a variable.
func TestTypedAgentUpsertsFixture(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	pkgs, err := listModuleWithExports(root)
	require.NoError(t, err)

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", agentUpsertFixture, parser.SkipObjectResolution)
	require.NoError(t, err)
	hits, err := typedAgentUpserts(fset, "example.com/fixture", []*ast.File{f}, exportImporter(fset, pkgs),
		func(*ast.File) string { return "fixture.go" })
	require.NoError(t, err)
	sort.Strings(hits)
	assert.Equal(t, []string{
		"fixture.go upsertBuilderHeld",
		"fixture.go upsertChained",
		"fixture.go upsertViaBulkVariable",
		"fixture.go upsertViaDeclaredVariable",
		"fixture.go upsertViaField",
		"fixture.go upsertViaHelper",
		"fixture.go upsertViaVariable",
	}, hits)

	// The syntactic chain scan alone misses the variable case; the
	// type-based scan above is what catches it.
	create := ast.Expr(nil)
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "upsertViaVariable" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "OnConflict" {
				create = sel.X
			}
			return true
		})
		return false
	})
	require.NotNil(t, create)
	assert.False(t, chainFromAgentCreate(create))
}
