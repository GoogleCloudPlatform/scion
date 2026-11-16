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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
)

const (
	scionModulePrefix              = "github.com/GoogleCloudPlatform/scion"
	ruleEngineImport               = scionModulePrefix + "/pkg/ruleengine"
	triggerRuleImport              = scionModulePrefix + "/pkg/triggerrule"
	hubRuleEngineRel     GoRelPath = "pkg/hub/rule_engine.go"
	triggerRuleHelperRel GoRelPath = "pkg/triggerrule/triggerrule.go"
)

type GoRelPath = string

func TestRuleEngine_NewRuleEngineAndPureRegistration(t *testing.T) {
	eng, cel, err := newRuleEngine()
	require.NoError(t, err)
	require.NotNil(t, eng)
	require.NotNil(t, cel)
	assert.Equal(t, []string{ruleengine.DialectCEL}, eng.Dialects())

	celReg, ok := eng.Dialect(ruleengine.DialectCEL)
	require.True(t, ok)
	assert.False(t, celReg.Declares().PerformsIO)

	// Registration must perform registration only: nil store and nil secretBackend
	// prove neither database nor settings-store reads occur during registration,
	// while MaxEvalTime is read from the already-loaded in-memory ServerConfig.
	srv := &Server{
		config: ServerConfig{
			StartupServerSettings: &config.V1ServerConfig{
				Hub: &config.V1ServerHubConfig{
					SystemOne: &config.V1ServerHubSystemOneConfig{
						MaxEvalTime: "6s",
					},
				},
			},
		},
		rules:      eng,
		celDialect: cel,
	}
	srv.initSystemOneRuntime()
	require.NoError(t, registerRuleConsumers(srv))

	assert.Same(t, eng, srv.RuleEngine())
	assert.Same(t, cel, srv.CELDialect())
	assert.Equal(t, []string{ruleengine.DialectCEL, systemone.DialectName}, eng.Dialects())

	soDialect, ok := eng.Dialect(systemone.DialectName)
	require.True(t, ok)
	assert.True(t, soDialect.Declares().PerformsIO)
	assert.Equal(t, 6*time.Second, soDialect.Declares().MaxEvalTime)
}

func TestRuleEngine_SingleConstructionPointGuard(t *testing.T) {
	root := findGoModuleRoot(t)
	violations, hubCallCount, err := scanRuleEngineConstructionSites(root)
	require.NoError(t, err)
	assert.Empty(t, violations,
		"ruleengine.NewEngine must only be called in %s (and triggerrule.NewDefault must never be called from non-test code):\n  %s",
		hubRuleEngineRel, strings.Join(violations, "\n  "))
	assert.Equal(t, 1, hubCallCount,
		"expected exactly 1 ruleengine.NewEngine call in %s", hubRuleEngineRel)

	// Self-test: verify the AST guard catches direct, aliased, value-reference,
	// dot-imported ruleengine.NewEngine calls as well as triggerrule.NewDefault.
	tmp := t.TempDir()
	writeGo := func(rel, src string) {
		p := filepath.Join(tmp, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(src), 0o644))
	}
	writeGo("pkg/hub/rule_engine.go", "package hub\nimport \""+ruleEngineImport+"\"\nfunc f() { _ = ruleengine.NewEngine() }\n")
	cleanV, cleanCnt, err := scanRuleEngineConstructionSites(tmp)
	require.NoError(t, err)
	assert.Empty(t, cleanV)
	assert.Equal(t, 1, cleanCnt)

	writeGo("pkg/hub/other.go", "package hub\nimport re \""+ruleEngineImport+"\"\nvar _ = re.NewEngine\n")
	writeGo("pkg/messaging/dot.go", "package messaging\nimport . \""+ruleEngineImport+"\"\nfunc g() { _ = NewEngine() }\n")
	writeGo("cmd/bad.go", "package cmd\nimport \""+triggerRuleImport+"\"\nfunc h() { _, _ = triggerrule.NewDefault() }\n")

	badV, _, err := scanRuleEngineConstructionSites(tmp)
	require.NoError(t, err)
	assert.Len(t, badV, 3, "expected all 3 planted construction violations, got: %v", badV)
}

func findGoModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above working directory")
		}
		dir = parent
	}
}

func scanRuleEngineConstructionSites(root string) ([]string, int, error) {
	fset := token.NewFileSet()
	var violations []string
	hubCallCount := 0

	err := filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			name := d.Name()
			if p != root && (name == "vendor" || name == "testdata" || name == "node_modules" || name == "extras" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}

		relSlash, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		relSlash = filepath.ToSlash(relSlash)

		f, parseErr := parser.ParseFile(fset, p, nil, 0)
		if parseErr != nil {
			return parseErr
		}

		reAliases := make(map[string]bool)
		reDot := false
		trAliases := make(map[string]bool)
		trDot := false

		for _, imp := range f.Imports {
			impPath, unqErr := strconv.Unquote(imp.Path.Value)
			if unqErr != nil {
				continue
			}
			var local string
			if imp.Name != nil {
				local = imp.Name.Name
			}
			switch impPath {
			case ruleEngineImport:
				switch local {
				case "_":
				case ".":
					reDot = true
				case "":
					reAliases["ruleengine"] = true
				default:
					reAliases[local] = true
				}
			case triggerRuleImport:
				switch local {
				case "_":
				case ".":
					trDot = true
				case "":
					trAliases["triggerrule"] = true
				default:
					trAliases[local] = true
				}
			}
		}

		if len(reAliases) == 0 && !reDot && len(trAliases) == 0 && !trDot {
			return nil
		}

		ast.Inspect(f, func(n ast.Node) bool {
			switch expr := n.(type) {
			case *ast.SelectorExpr:
				pkgIdent, ok := expr.X.(*ast.Ident)
				if !ok {
					return true
				}
				if reAliases[pkgIdent.Name] && expr.Sel.Name == "NewEngine" {
					if relSlash == hubRuleEngineRel {
						hubCallCount++
					} else if relSlash != triggerRuleHelperRel {
						pos := fset.Position(expr.Pos())
						violations = append(violations, fmt.Sprintf("%s:%d references ruleengine.NewEngine", relSlash, pos.Line))
					}
				}
				if trAliases[pkgIdent.Name] && expr.Sel.Name == "NewDefault" {
					pos := fset.Position(expr.Pos())
					violations = append(violations, fmt.Sprintf("%s:%d references triggerrule.NewDefault (bypassing shared Hub rule engine)", relSlash, pos.Line))
				}
			case *ast.Ident:
				if reDot && expr.Name == "NewEngine" {
					if relSlash == hubRuleEngineRel {
						hubCallCount++
					} else if relSlash != triggerRuleHelperRel {
						pos := fset.Position(expr.Pos())
						violations = append(violations, fmt.Sprintf("%s:%d references dot-imported ruleengine.NewEngine", relSlash, pos.Line))
					}
				}
				if trDot && expr.Name == "NewDefault" {
					pos := fset.Position(expr.Pos())
					violations = append(violations, fmt.Sprintf("%s:%d references dot-imported triggerrule.NewDefault", relSlash, pos.Line))
				}
			}
			return true
		})
		return nil
	})
	return violations, hubCallCount, err
}
