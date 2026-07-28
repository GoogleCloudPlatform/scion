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
	"slices"
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
	strays, sourceFileConsts, err := scanForStrayProjectSettingConsts(".")

	// The scan reports instrument failures — nothing parsed, source file absent,
	// no constants found where they are known to be — as an error rather than as
	// a clean result, because every one of them would otherwise present as "no
	// strays found" and pass. Fatal, not logged: a broken instrument must not be
	// allowed to reach the assertion below and satisfy it vacuously.
	require.NoError(t, err, "the stray-constant scan could not have found anything")

	// Positive control: the source file is known to declare these constants, so
	// a zero here means the parse or the name matcher stopped working, not that
	// the tree is clean.
	require.NotZerof(t, sourceFileConsts,
		"parsed %s but found no projectSetting* constants in it; the parse or "+
			"isProjectSettingConstName is broken and this guard is checking nothing",
		projectSettingsSourceFile)

	assert.Emptyf(t, strays,
		"projectSetting* constants are declared outside %s: %v. The drift guard only "+
			"parses %s, so these would not be required to appear in projectSettingKeys "+
			"and the settings would be silently dropped on clone. Move them into %s.",
		projectSettingsSourceFile, strays, projectSettingsSourceFile, projectSettingsSourceFile)
}

// TestScanForStrayProjectSettingConsts_EmptyDirIsAnError is the negative control
// for the guard above, kept executable rather than asserted once by hand.
//
// The guard's pass condition is an empty result, which is exactly what a scan
// that examined nothing also produces. Pointed at an empty directory the scan
// must refuse to return a clean answer; if this test ever fails, the guard above
// has become a no-op and would stay green through the removal of the property it
// is protecting.
func TestScanForStrayProjectSettingConsts_EmptyDirIsAnError(t *testing.T) {
	strays, sourceFileConsts, err := scanForStrayProjectSettingConsts(t.TempDir())

	require.Error(t, err,
		"scanning an empty directory returned a clean result; the guard is a no-op")
	assert.Empty(t, strays)
	assert.Zero(t, sourceFileConsts)
}

// scanForStrayProjectSettingConsts parses every .go file directly in dir and
// reports (a) each projectSetting* constant declared outside
// projectSettingsSourceFile, as "file: name", and (b) how many such constants
// were declared inside it.
//
// It returns an error, rather than an empty result, for every condition under
// which it could not have found a stray: the directory could not be read, it
// held no .go files, projectSettingsSourceFile was not among them, or a file
// failed to parse. Distinguishing "found nothing" from "looked at nothing" is
// the entire reason this is a function returning an error instead of a loop
// inside the test.
//
// File selection reproduces what parser.ParseDir with a nil filter selected
// before it was deprecated: every .go file directly in dir, subdirectories
// excluded, test files INCLUDED. Test files are then exempted from the constant
// check by the _test.go skip below, which is where the exemption belongs —
// filtering them out of the file set instead would look equivalent while
// quietly changing which files the source-file and parse checks can see.
func scanForStrayProjectSettingConsts(dir string) (strays []string, sourceFileConsts int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, fmt.Errorf("reading %s: %w", dir, err)
	}

	goFiles := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		goFiles = append(goFiles, e.Name())
	}
	if len(goFiles) == 0 {
		return nil, 0, fmt.Errorf("no .go files in %s; the scan is not looking at anything", dir)
	}
	if !slices.Contains(goFiles, projectSettingsSourceFile) {
		return nil, 0, fmt.Errorf(
			"%s is not among the %d .go files in %s; has it been renamed? "+
				"Update projectSettingsSourceFile", projectSettingsSourceFile, len(goFiles), dir)
	}

	fset := token.NewFileSet()
	for _, name := range goFiles {
		file, parseErr := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if parseErr != nil {
			return nil, 0, fmt.Errorf("parsing %s: %w", name, parseErr)
		}

		isSourceFile := name == projectSettingsSourceFile
		// Test files may legitimately reference these names.
		if !isSourceFile && strings.HasSuffix(name, "_test.go") {
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
				for _, constName := range vs.Names {
					if !isProjectSettingConstName(constName.Name) {
						continue
					}
					if isSourceFile {
						sourceFileConsts++
						continue
					}
					strays = append(strays, name+": "+constName.Name)
				}
			}
		}
	}
	return strays, sourceFileConsts, nil
}

// TestProjectSettingKeys_Count pins the number of project settings, so that an
// addition or removal is visible in the diff of this file and gets a moment's
// thought about clone and the resolved endpoint.
func TestProjectSettingKeys_Count(t *testing.T) {
	assert.Len(t, projectSettingKeys, expectedProjectSettingCount,
		"the project settings contract changed size; if that is intended, update "+
			"expectedProjectSettingCount and the table in .design/project-templates.md §3.1")
}
