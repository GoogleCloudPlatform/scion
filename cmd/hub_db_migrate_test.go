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

package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hubMigrateEntryPoints are the functions and methods that migrate the hub
// database schema: CompositeStore.Migrate and MigrateWithSchemaLock, and the
// raw-client entadapter.PreMigrate and entc.AutoMigrate.
var hubMigrateEntryPoints = map[string]bool{
	"Migrate":               true,
	"MigrateWithSchemaLock": true,
	"PreMigrate":            true,
	"AutoMigrate":           true,
}

// allowedHubMigrateCalls lists, per file in package cmd, the entry points it
// may call directly. Every other migration must go through migrateHubStore
// or migrateHubSchema, which run the PostgreSQL search_path shadow guard.
var allowedHubMigrateCalls = map[string]map[string]bool{
	// The shared helpers themselves.
	"hub_db_migrate.go": {"Migrate": true, "PreMigrate": true, "AutoMigrate": true},
	// Hub server startup: migrateStore runs MigrateWithSchemaLock after
	// initStore turns the guard on (checked below).
	"server_foreground.go": {"MigrateWithSchemaLock": true},
}

// TestHubMigrationCallsUseSharedHelpers fails when a file in package cmd
// migrates the hub database without the search_path shadow guard, so a new
// maintenance command cannot skip it (ptone/scion#4432).
func TestHubMigrationCallsUseSharedHelpers(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)

	fset := token.NewFileSet()
	var violations []string
	guardCalls := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var fn string
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				fn = fun.Sel.Name
			case *ast.Ident:
				fn = fun.Name
			default:
				return true
			}
			if name == "server_foreground.go" && fn == "EnableSchemaShadowGuard" {
				guardCalls++
			}
			if hubMigrateEntryPoints[fn] && !allowedHubMigrateCalls[name][fn] {
				violations = append(violations, fset.Position(call.Pos()).String()+": "+fn)
			}
			return true
		})
	}
	sort.Strings(violations)
	assert.Empty(t, violations,
		"migrate the hub database through migrateHubStore or migrateHubSchema (cmd/hub_db_migrate.go) so the PostgreSQL search_path shadow guard runs")
	assert.Equal(t, 1, guardCalls, "hub server startup (initStore) must turn on the search_path shadow guard before migrating")
}
