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

// The scan lists and type-checks package cmd's files as the default build
// (SQLite on) sees them, so it runs in that build only.

//go:build !no_sqlite

package cmd

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

const (
	scionModule        = "github.com/GoogleCloudPlatform/scion"
	entadapterPkgPath  = scionModule + "/pkg/store/entadapter"
	entcPkgPath        = scionModule + "/pkg/ent/entc"
	storePkgPath       = scionModule + "/pkg/store"
	compositeStoreRecv = "(*" + entadapterPkgPath + ".CompositeStore)"
	cmdPkgPath         = scionModule + "/cmd"
)

// hubMigrateEntryPoints are the functions and methods, by their
// types.Func.FullName, that migrate the hub database schema. A use of any
// of them, called or not, counts. cmd.migrateStore is listed too: it wraps
// MigrateWithSchemaLock with no guard of its own, so only initStore, which
// turns the guard on first, may use it.
var hubMigrateEntryPoints = map[string]bool{
	compositeStoreRecv + ".Migrate":               true,
	compositeStoreRecv + ".MigrateWithSchemaLock": true,
	"(" + storePkgPath + ".Store).Migrate":        true,
	entadapterPkgPath + ".PreMigrate":             true,
	entcPkgPath + ".AutoMigrate":                  true,
	cmdPkgPath + ".migrateStore":                  true,
}

// allowedHubMigrateUses is the complete list of functions in package cmd
// ("file func") allowed to use a hub migrate entry point, and which one.
// Each also has to use the guard named in requiredGuards.
var allowedHubMigrateUses = map[string]map[string]bool{
	// The shared helpers every maintenance command goes through.
	"hub_db_migrate.go migrateHubStore":  {compositeStoreRecv + ".Migrate": true},
	"hub_db_migrate.go migrateHubSchema": {entadapterPkgPath + ".PreMigrate": true, entcPkgPath + ".AutoMigrate": true},
	// Hub server startup: initStore turns the guard on, then calls
	// migrateStore, which calls MigrateWithSchemaLock. initStore is the only
	// function allowed to call migrateStore.
	"server_foreground.go initStore":    {cmdPkgPath + ".migrateStore": true},
	"server_foreground.go migrateStore": {compositeStoreRecv + ".MigrateWithSchemaLock": true},
}

// requiredGuards maps a function in package cmd to the guard it must use.
var requiredGuards = map[string]string{
	"hub_db_migrate.go migrateHubStore":  compositeStoreRecv + ".EnableMaintenanceSchemaShadowGuard",
	"hub_db_migrate.go migrateHubSchema": entadapterPkgPath + ".CheckSchemaShadowing",
	"server_foreground.go initStore":     compositeStoreRecv + ".EnableSchemaShadowGuard",
}

// TestHubMigrationCallsUseSharedHelpers fails when a function in package cmd
// uses one of hubMigrateEntryPoints directly, other than the allowed uses
// above, so a new maintenance command in package cmd that migrates through
// them cannot skip the PostgreSQL search_path shadow guard
// (ptone/scion#4432). It type-checks the package, so it matches the store
// and ent functions themselves, not method names.
//
// Limits: it checks package cmd only, and only the entry points listed. It
// does not see a migration reached through another package, or a Migrate
// call through an interface other than store.Store (for example a local
// interface{ Migrate(context.Context) error } holding a *CompositeStore).
// No such path exists today. Outside package cmd, non-test code migrates the
// hub database only in entadapter itself, apart from the SQLite-only legacy
// conversion (entc.MigrateAlphaSQLite), the pkg/store/enttest test helpers
// and the perf/bench seed tool.
func TestHubMigrationCallsUseSharedHelpers(t *testing.T) {
	pkg, imp := cmdPackageForScan(t)
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range pkg.GoFiles {
		f, err := parser.ParseFile(fset, filepath.Join(pkg.Dir, name), nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		files = append(files, f)
	}
	uses, err := funcUsesByEnclosingFunc(fset, pkg.ImportPath, files, imp(fset))
	require.NoError(t, err)

	var violations []string
	for fn, used := range uses {
		for name := range used {
			if hubMigrateEntryPoints[name] && !allowedHubMigrateUses[fn][name] {
				violations = append(violations, fn+" uses "+name)
			}
		}
	}
	sort.Strings(violations)
	assert.Empty(t, violations,
		"migrate the hub database through migrateHubStore or migrateHubSchema (cmd/hub_db_migrate.go) so the PostgreSQL search_path shadow guard runs")

	for fn, allowed := range allowedHubMigrateUses {
		for name := range allowed {
			assert.True(t, uses[fn][name], "%s no longer uses %s: update allowedHubMigrateUses", fn, name)
		}
	}
	for fn, guard := range requiredGuards {
		assert.True(t, uses[fn][guard], "%s must use %s", fn, guard)
	}
}

// TestFuncUsesByEnclosingFunc_MatchesTypes checks the scan on a fixture:
// hub migrate entry points are found however they are reached, and an
// unrelated Migrate method is not.
func TestFuncUsesByEnclosingFunc_MatchesTypes(t *testing.T) {
	_, imp := cmdPackageForScan(t)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", hubMigrateFixture, parser.SkipObjectResolution)
	require.NoError(t, err)
	uses, err := funcUsesByEnclosingFunc(fset, "example.com/fixture", []*ast.File{f}, imp(fset))
	require.NoError(t, err)

	found := func(fn string) []string {
		var got []string
		for name := range uses["fixture.go "+fn] {
			if hubMigrateEntryPoints[name] {
				got = append(got, name)
			}
		}
		sort.Strings(got)
		return got
	}
	assert.Equal(t, []string{compositeStoreRecv + ".Migrate"}, found("viaVariable"))
	assert.Equal(t, []string{compositeStoreRecv + ".Migrate"}, found("viaMethodValue"))
	assert.Equal(t, []string{"(" + storePkgPath + ".Store).Migrate"}, found("viaInterface"))
	assert.Equal(t, []string{entcPkgPath + ".AutoMigrate", entadapterPkgPath + ".PreMigrate"}, found("rawClient"))
	assert.Empty(t, found("unrelatedMigrate"))
}

const hubMigrateFixture = `package fixture

import (
	"context"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

type other struct{}

func (other) Migrate(context.Context) error { return nil }

func viaVariable(ctx context.Context, c *ent.Client) error {
	cs := entadapter.NewCompositeStore(c)
	return cs.Migrate(ctx)
}

func viaMethodValue(ctx context.Context, cs *entadapter.CompositeStore) error {
	migrate := cs.Migrate
	return migrate(ctx)
}

func viaInterface(ctx context.Context, s store.Store) error { return s.Migrate(ctx) }

func rawClient(ctx context.Context, c *ent.Client) error {
	if err := entadapter.PreMigrate(ctx, c); err != nil {
		return err
	}
	return entc.AutoMigrate(ctx, c)
}

func unrelatedMigrate(ctx context.Context) error { return other{}.Migrate(ctx) }
`

// funcUsesByEnclosingFunc type-checks files as package pkgPath and returns,
// for each top-level function ("file func", methods as "file Recv.func"),
// the FullName of every function or method it refers to.
func funcUsesByEnclosingFunc(fset *token.FileSet, pkgPath string, files []*ast.File, imp types.Importer) (map[string]map[string]bool, error) {
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
	var typeErrs []error
	conf := types.Config{Importer: imp, Error: func(err error) { typeErrs = append(typeErrs, err) }}
	_, _ = conf.Check(pkgPath, fset, files, info)
	if len(typeErrs) > 0 {
		return nil, errors.Join(typeErrs...)
	}
	uses := map[string]map[string]bool{}
	for _, file := range files {
		base := filepath.Base(fset.Position(file.Pos()).Filename)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := base + " " + fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				key = base + " " + types.ExprString(fn.Recv.List[0].Type) + "." + fn.Name.Name
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				if f, ok := info.Uses[id].(*types.Func); ok {
					if uses[key] == nil {
						uses[key] = map[string]bool{}
					}
					uses[key][f.FullName()] = true
				}
				return true
			})
		}
	}
	return uses, nil
}

// scanPackage is the subset of `go list -json` output the scan uses.
type scanPackage struct {
	ImportPath string
	Dir        string
	Export     string
	GoFiles    []string
	CgoFiles   []string
	Imports    []string
}

var (
	cmdScanOnce    sync.Once
	cmdScanPkg     scanPackage
	cmdScanExports map[string]string
	cmdScanErr     error
)

// cmdPackageForScan lists package cmd (non-test files) and compiler export
// data for its imports and their dependencies, once per test binary. It
// does not compile package cmd itself. go list inherits the environment, so
// GOFLAGS (for example a -gcflags setting) applies to it as to go test.
func cmdPackageForScan(t *testing.T) (scanPackage, func(*token.FileSet) types.Importer) {
	t.Helper()
	cmdScanOnce.Do(func() {
		goTool, err := exec.LookPath("go")
		if err != nil {
			cmdScanErr = fmt.Errorf("locating the go command: %w", err)
			return
		}
		run := func(args ...string) ([]byte, error) {
			c := exec.Command(goTool, args...)
			c.Env = append(os.Environ(), "HOME="+goToolHome)
			var stderr strings.Builder
			c.Stderr = &stderr
			out, err := c.Output()
			if err != nil {
				return nil, fmt.Errorf("go %s: %w: %s", strings.Join(args, " "), err, stderr.String())
			}
			return out, nil
		}
		out, err := run("list", "-buildvcs=false", "-json=ImportPath,Dir,GoFiles,CgoFiles,Imports", ".")
		if err != nil {
			cmdScanErr = err
			return
		}
		if err := json.Unmarshal(out, &cmdScanPkg); err != nil {
			cmdScanErr = err
			return
		}
		if len(cmdScanPkg.CgoFiles) > 0 {
			cmdScanErr = fmt.Errorf("package cmd has cgo files the scan cannot type-check: %v", cmdScanPkg.CgoFiles)
			return
		}
		// The fixture's imports, in case package cmd stops importing one.
		imports := append([]string{scionModule + "/pkg/ent", entcPkgPath, storePkgPath, entadapterPkgPath}, cmdScanPkg.Imports...)
		out, err = run(append([]string{"list", "-buildvcs=false", "-export", "-deps", "-json=ImportPath,Export"}, imports...)...)
		if err != nil {
			cmdScanErr = err
			return
		}
		cmdScanExports = map[string]string{}
		dec := json.NewDecoder(strings.NewReader(string(out)))
		for {
			var p scanPackage
			if err := dec.Decode(&p); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				cmdScanErr = err
				return
			}
			if p.Export != "" {
				cmdScanExports[p.ImportPath] = p.Export
			}
		}
	})
	require.NoError(t, cmdScanErr)
	return cmdScanPkg, func(fset *token.FileSet) types.Importer {
		return importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
			f, ok := cmdScanExports[path]
			if !ok {
				return nil, fmt.Errorf("no export data for %q", path)
			}
			return os.Open(f)
		})
	}
}

// goToolHome is HOME as the test binary started with; tests change HOME
// with t.Setenv, and the go command finds its caches through it.
var goToolHome = os.Getenv("HOME")
