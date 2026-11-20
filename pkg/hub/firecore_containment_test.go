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

package hub

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFireCoreAuthorityCallersContained enforces Stage 1 §16 criterion 4:
//   - resolveRecordedAuthority and fireStanding are called only from
//     firecore_*.go and action_*.go;
//   - fireAction is called only from firecore_*.go, action_*.go, the scheduler
//     adapters (scheduler.go, server.go), and trigger_dispatch.go;
//   - the temporary legacy layer (firecore_legacy.go, EventHandler,
//     RegisterEventHandler, legacyAction, resolveScheduledAuthority,
//     scheduledFireStanding, authorizeScheduledMessageFire) no longer exists.
func TestFireCoreAuthorityCallersContained(t *testing.T) {
	t.Parallel()

	_, err := os.Stat("firecore_legacy.go")
	assert.True(t, os.IsNotExist(err), "firecore_legacy.go must be deleted after PR 2")

	// pkgmove:scan-covers guardScanSubdirs
	// Reason: scans all non-test Go files in pkg/hub and extracted subpackages
	// to enforce §16.4 containment of resolveRecordedAuthority, fireStanding,
	// and fireAction, and absence of removed legacy symbols.
	files, err := readGuardScanDirs(".")
	require.NoError(t, err)

	forbiddenDecls := map[string]bool{
		"EventHandler":                  true,
		"RegisterEventHandler":          true,
		"legacyAction":                  true,
		"newLegacyAction":               true,
		"resolveScheduledAuthority":     true,
		"scheduledFireStanding":         true,
		"authorizeScheduledMessageFire": true,
		"ScheduledAuthority":            true,
	}

	isCoreOrAction := func(rel string) bool {
		base := filepath.Base(rel)
		return rel == base && (strings.HasPrefix(base, "firecore_") || strings.HasPrefix(base, "action_"))
	}
	allowedFireActionFile := func(rel string) bool {
		return isCoreOrAction(rel) ||
			rel == "scheduler.go" ||
			rel == "server.go" ||
			rel == "trigger_dispatch.go"
	}

	fset := token.NewFileSet()
	for _, entry := range files {
		if entry.IsDir() {
			continue
		}
		rel := entry.Name()
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, rel, nil, 0)
		require.NoError(t, err, rel)

		for _, decl := range parsed.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if forbiddenDecls[d.Name.Name] {
					t.Errorf("legacy symbol %s must not exist (%s)", d.Name.Name, fset.Position(d.Pos()))
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok && forbiddenDecls[ts.Name.Name] {
						t.Errorf("legacy type %s must not exist (%s)", ts.Name.Name, fset.Position(ts.Pos()))
					}
				}
			}
		}

		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var callee string
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				callee = fn.Sel.Name
			case *ast.Ident:
				callee = fn.Name
			default:
				return true
			}
			switch callee {
			case "resolveRecordedAuthority", "fireStanding":
				if !isCoreOrAction(rel) {
					t.Errorf("%s called outside firecore_*/action_* at %s", callee, fset.Position(call.Pos()))
				}
			case "fireAction":
				if !allowedFireActionFile(rel) {
					t.Errorf("fireAction called outside allowed core/adapter files at %s", fset.Position(call.Pos()))
				}
			}
			return true
		})
	}
}
