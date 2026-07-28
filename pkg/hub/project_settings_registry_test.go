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
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// projectSettingsSourceFile is the file that declares both the projectSetting*
// constants and the projectSettingKeys registry.
const projectSettingsSourceFile = "project_settings_handlers.go"

// expectedProjectSettingCount is the number of project settings defined by
// .design/project-templates.md §3.1. It is asserted separately from the drift
// guard so that adding or removing a setting shows up as a deliberate edit in
// the diff rather than passing silently.
const expectedProjectSettingCount = 16

// isProjectSettingConstName reports whether an identifier names a project
// settings annotation key constant.
//
// The singular "projectSetting" prefix is the naming convention for these keys.
// Plural "projectSettings*" names (projectSettingsSourceFile, and any future
// helper such as a projectSettingsPrefix) are deliberately excluded: they are
// not annotation keys, and matching them would trip the guard on a constant
// that has no business being in the registry.
func isProjectSettingConstName(name string) bool {
	return strings.HasPrefix(name, "projectSetting") &&
		!strings.HasPrefix(name, "projectSettings")
}

// declaredProjectSettingConstants parses projectSettingsSourceFile and returns
// the name and value of every projectSetting* constant, in declaration order.
//
// This derives the expected set from the source *independently of the registry
// itself*, which is the whole point: a test that restated projectSettingKeys
// would be copy-pasted from it and would pass forever, including when both were
// wrong together.
func declaredProjectSettingConstants(t *testing.T) (names []string, values []string) {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, projectSettingsSourceFile, nil, 0)
	require.NoError(t, err, "failed to parse %s", projectSettingsSourceFile)

	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !isProjectSettingConstName(name.Name) {
					continue
				}
				require.Less(t, i, len(vs.Values),
					"constant %s has no value expression", name.Name)
				lit, ok := vs.Values[i].(*ast.BasicLit)
				require.True(t, ok && lit.Kind == token.STRING,
					"constant %s is not a string literal", name.Name)
				value, err := strconv.Unquote(lit.Value)
				require.NoError(t, err, "failed to unquote value of %s", name.Name)

				names = append(names, name.Name)
				values = append(values, value)
			}
		}
	}
	return names, values
}

// TestProjectSettingKeys_NoDrift is the drift guard for the project settings
// registry.
//
// The failure mode it exists to catch is someone adding a projectSetting*
// constant and wiring it into applyProjectSettingsToAnnotations, but forgetting
// projectSettingKeys. Nothing else notices: GET/PUT /settings keep working, and
// the omission only surfaces later as a setting silently dropped when a project
// is cloned.
//
// If a projectSetting* constant is ever *deliberately* excluded from the
// registry, this test must be edited to record that exclusion and say why.
// That friction is intentional — exclusion is a change to the project-settings
// contract and should be argued for in review, not defaulted into.
func TestProjectSettingKeys_NoDrift(t *testing.T) {
	names, values := declaredProjectSettingConstants(t)
	require.NotEmpty(t, names, "parsed no projectSetting* constants from %s; "+
		"the guard is not actually looking at anything", projectSettingsSourceFile)

	registry := make(map[string]int, len(projectSettingKeys))
	for _, key := range projectSettingKeys {
		registry[key]++
	}

	// 1. Every declared constant is registered.
	for i, value := range values {
		assert.Containsf(t, registry, value,
			"constant %s (%q) is declared in %s but missing from projectSettingKeys. "+
				"Unregistered settings are silently dropped when a project is cloned. "+
				"Add it to projectSettingKeys.",
			names[i], value, projectSettingsSourceFile)
	}

	// 2. Nothing is registered that is not a declared constant, and nothing is
	//    registered twice. This catches a raw string literal or a duplicated
	//    entry sneaking into the registry.
	declared := make(map[string]string, len(values))
	for i, value := range values {
		declared[value] = names[i]
	}
	for key, count := range registry {
		assert.Containsf(t, declared, key,
			"projectSettingKeys contains %q, which is not any projectSetting* constant "+
				"declared in %s. Use the constant, not a string literal.",
			key, projectSettingsSourceFile)
		assert.Equalf(t, 1, count, "projectSettingKeys contains %q %d times", key, count)
	}

	// 3. Registry order matches constant declaration order, which matches the
	//    table in .design/project-templates.md §3.1. Keeping all three in the
	//    same order is what makes them diffable by eye.
	assert.Equal(t, values, projectSettingKeys,
		"projectSettingKeys does not match the projectSetting* constants in declaration order; "+
			"keep the registry, the constants and the §3.1 table in the same order")
}

// TestProjectSettingKeys_NoConstantsOutsideSourceFile closes the one gap in the
// drift guard above: that guard parses a single file, so a projectSetting*
// constant declared in some other pkg/hub file would be invisible to it, would
// not be required to appear in the registry, and would therefore be silently
// dropped on clone — the very failure mode the guard exists to prevent, just
// displaced by one file.
//
// Rather than widen the parse (which would start matching unrelated constants
// across the package), this asserts the narrower and more useful property: the
// source file is the *only* declaration site, so parsing it is sufficient.
func TestProjectSettingKeys_NoConstantsOutsideSourceFile(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	require.NoError(t, err, "failed to parse the pkg/hub directory")
	require.NotEmpty(t, pkgs, "parsed no packages; this guard is not looking at anything")

	sawSourceFile := false
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			if filepath.Base(path) == projectSettingsSourceFile {
				sawSourceFile = true
				continue
			}
			// Test files may legitimately reference these names.
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, name := range vs.Names {
						assert.Falsef(t, isProjectSettingConstName(name.Name),
							"%s declares %s outside %s. The drift guard only parses %s, "+
								"so this constant would not be required to appear in "+
								"projectSettingKeys and the setting would be silently dropped "+
								"on clone. Move it into %s.",
							path, name.Name, projectSettingsSourceFile,
							projectSettingsSourceFile, projectSettingsSourceFile)
					}
				}
			}
		}
	}

	// Anti-vacuity: if the source file were renamed, every file would be
	// skipped by the filepath.Base check above and this test would pass while
	// checking nothing.
	require.True(t, sawSourceFile,
		"did not encounter %s while scanning the package; has it been renamed? "+
			"Update projectSettingsSourceFile.", projectSettingsSourceFile)
}

// TestProjectSettingKeys_Count pins the number of project settings, so that an
// addition or removal is visible in the diff of this file and gets a moment's
// thought about clone and the resolved endpoint.
func TestProjectSettingKeys_Count(t *testing.T) {
	assert.Len(t, projectSettingKeys, expectedProjectSettingCount,
		"the project settings contract changed size; if that is intended, update "+
			"expectedProjectSettingCount and the table in .design/project-templates.md §3.1")
}
