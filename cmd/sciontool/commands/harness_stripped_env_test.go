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
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// harnessStripSourceDirs hold the code that removes names from the
// environment the harness inherits: os.Unsetenv in sciontool init (this
// package) and removeEnvVar in the supervisor's child environment.
var harnessStripSourceDirs = []string{".", filepath.Join("..", "..", "..", "pkg", "sciontool", "supervisor")}

// harnessStripNameExprs resolves the non-literal arguments of the strip
// calls. A new strip call with another argument needs an entry here and
// its name in stagedsecrets.HarnessStrippedEnvNames.
var harnessStripNameExprs = map[string]string{
	"envHubTokenStaged":                     envHubTokenStaged,
	"envResolvedAuthStaged":                 envResolvedAuthStaged,
	"stagedsecrets.EnvVar":                  stagedsecrets.EnvVar,
	"transportauth.EnvTransportToken":       transportauth.EnvTransportToken,
	"transportauth.EnvTransportTokenExpiry": transportauth.EnvTransportTokenExpiry,
	"hooks.NativeTelemetryPolicyKey":        hooks.NativeTelemetryPolicyKey,
}

// isHarnessStripCall reports whether call is os.Unsetenv(name) or
// removeEnvVar(env, name), and returns the name argument.
func isHarnessStripCall(call *ast.CallExpr) (ast.Expr, bool) {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		if x, ok := fn.X.(*ast.Ident); ok && x.Name == "os" && fn.Sel.Name == "Unsetenv" && len(call.Args) == 1 {
			return call.Args[0], true
		}
	case *ast.Ident:
		if fn.Name == "removeEnvVar" && len(call.Args) == 2 {
			return call.Args[1], true
		}
	}
	return nil, false
}

// harnessStripNameOf returns the env name expr denotes, or "" when it is
// neither a string literal nor an entry of harnessStripNameExprs.
func harnessStripNameOf(expr ast.Expr) (string, string) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			if v, err := strconv.Unquote(e.Value); err == nil {
				return v, e.Value
			}
		}
		return "", e.Value
	case *ast.Ident:
		return harnessStripNameExprs[e.Name], e.Name
	case *ast.SelectorExpr:
		if x, ok := e.X.(*ast.Ident); ok {
			key := x.Name + "." + e.Sel.Name
			return harnessStripNameExprs[key], key
		}
	}
	return "", "unsupported expression"
}

// Every name sciontool init removes from the harness environment is listed
// in stagedsecrets.HarnessStrippedEnvNames (and so reserved for staged
// resolved-auth entries), and every listed name is removed somewhere.
func TestHarnessStrippedEnvNames_MatchInitStripCalls(t *testing.T) {
	stripped := map[string]bool{}
	for _, dir := range harnessStripSourceDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		for _, ent := range entries {
			name := ent.Name()
			if ent.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				arg, ok := isHarnessStripCall(call)
				if !ok {
					return true
				}
				env, src := harnessStripNameOf(arg)
				if env == "" {
					t.Errorf("%s: strip call with argument %s: add it to harnessStripNameExprs and its name to stagedsecrets.HarnessStrippedEnvNames", fset.Position(call.Pos()), src)
					return true
				}
				stripped[env] = true
				return true
			})
		}
	}
	for _, control := range []string{"PORT", "SCION_EXTRA_PATH"} {
		if !stripped[control] {
			t.Fatalf("control: the scan found no strip call for %s", control)
		}
	}
	listed := stagedsecrets.HarnessStrippedEnvNames()
	for _, env := range slices.Sorted(maps.Keys(stripped)) {
		if !slices.Contains(listed, env) {
			t.Errorf("%s is removed from the harness environment but missing from stagedsecrets.HarnessStrippedEnvNames", env)
		}
		if !stagedsecrets.ReservedResolvedAuthEnvName(env) {
			t.Errorf("%s is removed from the harness environment but not reserved for staged resolved-auth entries", env)
		}
	}
	for _, env := range listed {
		if !stripped[env] {
			t.Errorf("stagedsecrets.HarnessStrippedEnvNames lists %s, but no strip call removes it", env)
		}
	}
}
