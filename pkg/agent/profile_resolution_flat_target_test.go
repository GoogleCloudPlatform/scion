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

// These tests exercise unix-only, Windows-gated agent storage code.
//go:build unix

package agent

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// --- Source scan: settings loads on the start, provision and reprovision
// paths. ---

// nonForSettingsLoaders are the pkg/config settings loaders that return
// the loaded settings without the request's view, so a flat instance's
// policy never reaches what they return.
var nonForSettingsLoaders = map[string]bool{
	"LoadEffectiveSettings": true, "LoadGlobalSettings": true, "LoadGlobalSettingsWithOverlay": true,
	"LoadSettings": true, "LoadVersionedSettings": true,
	"LoadSettingsIgnoringEnvProjectID": true, "LoadSettingsFromDir": true, "LoadSingleFileVersioned": true,
}

// nonForLoadAllowList is every reviewed non-For settings load in pkg/agent,
// keyed "file func loader", with the reason it needs no flat policy.
var nonForLoadAllowList = map[string]string{
	"kept_agent_dir.go resolveKeptAgentDirRetention LoadGlobalSettings":    "reads only server.hub.soft_delete_retention, which has no profile tier",
	"list.go lazyHubProjectID LoadEffectiveSettings":                       "listing: reads only the project's hub settings",
	"nfs_worktree.go nfsExportWorkspaceOnAnyRuntime LoadEffectiveSettings": "reads only server.workspace_storage, which has no profile tier",
	"run.go AgentManager.Start LoadSettings":                               "reads only the project's hub settings (legacy settings type)",
}

// TestSettingsLoads_FlatPathsUseRequestView: every settings load in
// pkg/agent's production code goes through a For loader (which applies the
// flat instance policy of the request), except the reviewed sites in
// nonForLoadAllowList. A new or reverted load fails here.
func TestSettingsLoads_FlatPathsUseRequestView(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	found := map[string]bool{}
	var unreviewed []string
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			fnName := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				recv := fn.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				if id, ok := recv.(*ast.Ident); ok {
					fnName = id.Name + "." + fnName
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "config" && nonForSettingsLoaders[sel.Sel.Name] {
					key := name + " " + fnName + " " + sel.Sel.Name
					if _, ok := nonForLoadAllowList[key]; ok {
						found[key] = true
					} else {
						unreviewed = append(unreviewed, fmt.Sprintf("%s (%s)", key, fset.Position(sel.Pos())))
					}
				}
				return true
			})
		}
	}
	sort.Strings(unreviewed)
	if len(unreviewed) > 0 {
		t.Fatalf("settings loads that do not read the request's flat instance view (use LoadEffectiveSettingsFor or LoadGlobalSettingsWithOverlayFor, or review the site and add it to nonForLoadAllowList):\n%s",
			strings.Join(unreviewed, "\n"))
	}
	for key := range nonForLoadAllowList {
		if !found[key] {
			t.Errorf("allow-list entry %q matches no load; remove it", key)
		}
	}
}
