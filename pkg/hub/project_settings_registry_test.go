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
	// Pin which guard fired, for the same reason the missing-test-file case does:
	// an empty directory must stop at the no-.go-files check, and asserting only
	// that *an* error came back would also accept an error from a later guard or
	// from a genuine instrument failure.
	assert.Contains(t, err.Error(), "no .go files in",
		"the scan errored, but not on the empty-directory condition this control exists to reach")
	assert.Empty(t, strays)
	assert.Zero(t, sourceFileConsts)
}

// writeGoFixture writes one .go file into a scan fixture directory. The content
// is only ever parsed, never compiled, so it need not build.
func writeGoFixture(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
}

// TestScanForStrayProjectSettingConsts_NoTestFileIsAnError covers an input class
// the scan handles and nothing else supplies: a directory holding .go files,
// including the source file, but no _test.go file at all.
//
// The scan's three instrument-failure guards fire in sequence — no .go files,
// then source file absent, then no test file — and the empty-directory control
// above stops at the first. So the third was reachable only through a narrowed
// selection in production code, and never through the suite.
//
// This is a coverage gap rather than a guard on a guard: the case would be worth
// testing even if the third check did not exist, because "a file set with no test
// file" is a distinct input the function makes a decision about.
//
// Probe by pt-rev-3.
func TestScanForStrayProjectSettingConsts_NoTestFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeGoFixture(t, dir, projectSettingsSourceFile,
		"package hub\n\nconst projectSettingProbeSource = \"scion.io/probe-source\"\n")
	writeGoFixture(t, dir, "helper.go", "package hub\n")

	strays, sourceFileConsts, err := scanForStrayProjectSettingConsts(dir)

	require.Error(t, err,
		"a file set containing .go files and the source file but NO _test.go file returned a "+
			"clean result; the scan can no longer tell a complete selection from one narrowed "+
			"to exclude test files")
	// Pin which guard fired. Without this the test would also pass if the scan
	// failed for one of the two earlier reasons, which are already covered and
	// are not what this case is about.
	// The full phrase, not the bare "_test.go" substring: both earlier guards
	// interpolate dir into their messages, so the loose form's discriminating
	// power depends on t.TempDir() never containing "_test.go". It does not
	// today, and the tighter string costs nothing to stop depending on it.
	assert.Contains(t, err.Error(), "is a _test.go file",
		"the scan errored, but not on the missing-test-file condition this case exists to reach")
	assert.Empty(t, strays)
	assert.Zero(t, sourceFileConsts)
}

// TestScanForStrayProjectSettingConsts_SelectsFilesRegardlessOfBuildTags pins
// the selection behaviour that made os.ReadDir + parser.ParseFile the right
// replacement for the deprecated parser.ParseDir: every .go file in the
// directory is parsed, and a //go:build line does not remove one.
//
// WHAT THIS TEST DOES NOT DO, because the name and the fixture both suggest
// otherwise: IN A t.TempDir() THE BUILD TAG IS INERT. Nothing compiles these
// files, so //go:build is an ordinary comment here and os.ReadDir would list the
// file with or without it. This test cannot witness tag-blindness against a
// constraint that is actually in force, and a reader who sees //go:build in a
// fixture will assume it does.
//
// The load-bearing demonstration is a probe planted in the real pkg/hub, where
// a tag IS in force. It is not shippable — it mutates the package under test —
// so it is written out here rather than cited, and it runs against nothing but
// a clone of this repository:
//
//	printf '//go:build !no_sqlite\n\npackage hub\n\nconst projectSettingProbe = "x"\n' \
//	  > pkg/hub/probe_stray.go
//	go list -tags no_sqlite -f '{{join .GoFiles "\n"}}' ./pkg/hub | grep -c probe_stray.go  # 0
//	go list                 -f '{{join .GoFiles "\n"}}' ./pkg/hub | grep -c probe_stray.go  # 1
//	go test -tags no_sqlite ./pkg/hub/ -run NoConstantsOutsideSourceFile                    # FAIL
//	rm pkg/hub/probe_stray.go
//
// The 0 says the compiler cannot see the file under the tag; the 1 is the
// control proving the probe exists at all; the FAIL says the guard reports it
// regardless. Both counts are needed — a single number cannot distinguish
// "excluded by the tag" from "never planted". (Original probe: pt-rev-3,
// PR 597.) This test is the shippable remainder.
//
// Run those as five separate commands. The expected 0 makes grep exit 1, so
// under set -e or a && chain the block stops there — after the probe is planted
// and before the line that removes it.
//
// staticcheck's SA1019 recommends go/packages, and its advertised advantage is
// that it applies build tags. For this guard that advantage is a defect. A
// projectSetting* constant in a file excluded from the current build is still a
// constant the drift guard cannot see, still absent from projectSettingKeys, and
// still silently dropped when a project is cloned. It has to be caught anyway.
//
// Without this test the property is preserved but UNEXERCISED. Every //go:build
// !no_sqlite file in pkg/hub today is a _test.go file, and test files are exempt
// from the constant check regardless, so nothing else on the current tree would
// go red if tag-blindness were lost: a migration to go/packages would compile,
// pass every other test in this file, and reopen the data-loss path in silence.
// Correct behaviour that nothing would notice losing is indistinguishable from
// luck.
//
// That this test is the exception depends entirely on the fixture's tag being
// one NO build context satisfies — see the fixture below. With a realistic tag
// it went green through the migration too, and the paragraph above would have
// been describing this test as well.
//
// Probe by pt-rev-3.
func TestScanForStrayProjectSettingConsts_SelectsFilesRegardlessOfBuildTags(t *testing.T) {
	dir := t.TempDir()
	writeGoFixture(t, dir, projectSettingsSourceFile,
		"package hub\n\nconst projectSettingProbeSource = \"scion.io/probe-source\"\n")
	// Satisfies the test-file requirement without declaring anything.
	writeGoFixture(t, dir, "probe_selection_test.go", "package hub\n")
	// The stray carries a tag NOTHING EVER SETS, and the choice is load-bearing.
	// The obvious tag, !no_sqlite, is the real in-tree shape and is why this test
	// exists — but it is SATISFIED in the default build context, so a tag-aware
	// loader selects the file anyway and this test stays green through exactly
	// the migration it warns about. Measured, both tag modes including the one
	// CI runs: with !no_sqlite an ambient build.Default.MatchFile selection is
	// GREEN (a miss); with the tag below it is RED, while the shipped tag-blind
	// scan stays GREEN either way. The fixture's job is detection, not
	// resemblance.
	writeGoFixture(t, dir, "tagged_stray.go",
		"//go:build scion_never_defined_tag\n\npackage hub\n\n"+
			"const projectSettingTaggedStray = \"scion.io/tagged-stray\"\n")

	strays, sourceFileConsts, err := scanForStrayProjectSettingConsts(dir)
	require.NoError(t, err,
		"the scan errored on a three-file fixture it should handle; the assertions below "+
			"cannot distinguish a clean tree from a scan that never ran")

	// Positive control, same reason as in the guard above: a zero here means the
	// scan stopped parsing, not that the planted file was judged clean.
	require.NotZero(t, sourceFileConsts,
		"the stand-in source file declares a projectSetting* constant and none was counted; "+
			"the scan is not parsing, so the assertion below would pass vacuously")

	assert.Equal(t, []string{"tagged_stray.go: projectSettingTaggedStray"}, strays,
		"a projectSetting* constant in a file carrying a //go:build line was NOT reported, so "+
			"file selection has started depending on build tags — most likely a migration to "+
			"go/packages, which staticcheck SA1019 recommends. That property is required here: a "+
			"constant invisible to the current build is still dropped when a project is cloned. "+
			"Keep os.ReadDir + parser.ParseFile.")
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
	// Test files MUST be in the selected set. They are excluded from the
	// constant check by the _test.go skip in the loop below, not here, and the
	// difference is not cosmetic: excluding them at selection looks equivalent,
	// produces an identical result today, and silently shrinks what the checks
	// above can see. Without this assertion that mistake is invisible — the
	// suite stays green — so the requirement is stated as an executed check
	// rather than as a comment asking the next reader to be careful.
	if !slices.ContainsFunc(goFiles, func(n string) bool { return strings.HasSuffix(n, "_test.go") }) {
		return nil, 0, fmt.Errorf(
			"none of the %d .go files selected in %s is a _test.go file; the selection has been "+
				"narrowed to exclude test files. Select every .go file and let the _test.go skip "+
				"below do the exempting", len(goFiles), dir)
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
