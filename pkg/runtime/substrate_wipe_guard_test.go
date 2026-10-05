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

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// wipeGuardSymbol is the identifier TestWipeSubstrateAgentStateForTest_NotCalledOutsideTests
// and referencesOutsideDeclaration both guard: WipeSubstrateAgentStateForTest
// (substrate_runtime.go) is exported, and therefore compiled into the
// production binary, only because pkg/runtimebroker's tests need it and
// cannot reach an unexported symbol in this package. Nothing in production
// may call it — doing so would silently turn every live agent record-less.
const wipeGuardSymbol = "WipeSubstrateAgentStateForTest"

// wipeGuardDeclRe matches only the exact declaration line, anchored so a
// longer identifier that merely starts with the symbol (e.g. a hypothetical
// WipeSubstrateAgentStateForTestAll) can never be mistaken for it: the
// symbol must be followed immediately by "(", and the match must start at
// the beginning of the (trimmed) line.
var wipeGuardDeclRe = regexp.MustCompile(`^func ` + regexp.QuoteMeta(wipeGuardSymbol) + `\(`)

// referencesOutsideDeclaration scans data (one file's contents) for
// references to symbol that are not the symbol's own declaration, and
// returns one formatted "<line>: <text>" entry per reference found. It is
// deliberately a pure function over in-memory bytes, not a filesystem walk,
// so the edge cases below (a longer identifier sharing the declaration
// line, a go:linkname comment) can be pinned directly without writing
// scratch files into the module the real guard test walks.
//
// Rules, in order:
//   - a line with no occurrence of symbol at all is never a reference;
//   - a comment line ("//...") is not a reference UNLESS it names symbol
//     together with a go:linkname directive — that pairs with a matching
//     //go:linkname on a real declaration elsewhere to make the linker
//     resolve a call to the unexported... to this exported symbol without
//     ever writing "WipeSubstrateAgentStateForTest(" as code, so a plain
//     substring/prefix scan of non-comment lines alone would miss it;
//   - a line matching wipeGuardDeclRe is the declaration, and the portion of
//     the line at and after the match is stripped before checking for a
//     leftover reference — this exempts the declaration signature itself
//     (including its own return-type parenthesis) while still catching a
//     reference to the *real* symbol that happens to share a line with a
//     longer identifier's declaration (e.g.
//     "func WipeSubstrateAgentStateForTestAll() { WipeSubstrateAgentStateForTest() }" —
//     wipeGuardDeclRe does not match that line at all, since "WipeSubstrateAgentStateForTest"
//     is immediately followed by "A", not "(", so the whole line is scanned
//     as a reference);
//   - anything else containing symbol is a reference.
func referencesOutsideDeclaration(symbol string, data []byte) []string {
	var hits []string
	for i, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, symbol) {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			if strings.Contains(trimmed, "go:linkname") {
				hits = append(hits, fmt.Sprintf("%d: %s", i+1, trimmed))
			}
			continue
		}
		if loc := wipeGuardDeclRe.FindStringIndex(trimmed); loc != nil {
			rest := trimmed[loc[1]:]
			if !strings.Contains(rest, symbol) {
				continue // exactly the declaration; nothing else on the line
			}
		}
		hits = append(hits, fmt.Sprintf("%d: %s", i+1, trimmed))
	}
	return hits
}

// TestReferencesOutsideDeclaration_DeclarationAndProseAreNotHits pins the
// two "must not false-positive" shapes: the real declaration line (however
// its return type is spelled), and an ordinary doc-comment sentence that
// happens to mention the symbol in prose.
func TestReferencesOutsideDeclaration_DeclarationAndProseAreNotHits(t *testing.T) {
	data := []byte(`// WipeSubstrateAgentStateForTest clears the maps for a test.
// See WipeSubstrateAgentStateForTest's caller for details.
func WipeSubstrateAgentStateForTest() (restore func()) {
	return func() {}
}
`)
	if hits := referencesOutsideDeclaration(wipeGuardSymbol, data); len(hits) != 0 {
		t.Errorf("referencesOutsideDeclaration() = %v, want none", hits)
	}
}

// TestReferencesOutsideDeclaration_OrdinaryCallIsAHit is the base sanity
// case: a plain production call must be caught.
func TestReferencesOutsideDeclaration_OrdinaryCallIsAHit(t *testing.T) {
	data := []byte(`func bad() {
	restore := WipeSubstrateAgentStateForTest()
	_ = restore
}
`)
	hits := referencesOutsideDeclaration(wipeGuardSymbol, data)
	if len(hits) != 1 {
		t.Fatalf("referencesOutsideDeclaration() = %v, want exactly one hit", hits)
	}
}

// TestReferencesOutsideDeclaration_LongerIdentifierSharingTheLineIsAHit
// kills a guard that skips any line merely starting with "func "+symbol: a
// production function whose name is a longer identifier built from the
// symbol can still call the real symbol on the same line.
func TestReferencesOutsideDeclaration_LongerIdentifierSharingTheLineIsAHit(t *testing.T) {
	data := []byte(`func WipeSubstrateAgentStateForTestAll() { WipeSubstrateAgentStateForTest() }
`)
	hits := referencesOutsideDeclaration(wipeGuardSymbol, data)
	if len(hits) != 1 {
		t.Fatalf("referencesOutsideDeclaration() = %v, want exactly one hit (the embedded call, not the longer declaration)", hits)
	}
}

// TestReferencesOutsideDeclaration_LinknameCommentIsAHit kills a guard that
// treats every "//"-prefixed line as exempt: a //go:linkname directive
// naming this symbol, paired with a matching declaration elsewhere plus
// `import _ "unsafe"`, lets the linker resolve a call to an unexported
// production function straight to this exported symbol without any line of
// code ever spelling "WipeSubstrateAgentStateForTest(".
func TestReferencesOutsideDeclaration_LinknameCommentIsAHit(t *testing.T) {
	data := []byte(`//go:linkname zzWipe github.com/GoogleCloudPlatform/scion/pkg/runtime.WipeSubstrateAgentStateForTest
func zzWipe()
`)
	hits := referencesOutsideDeclaration(wipeGuardSymbol, data)
	if len(hits) != 1 {
		t.Fatalf("referencesOutsideDeclaration() = %v, want exactly one hit (the go:linkname comment)", hits)
	}
}

// TestWipeSubstrateAgentStateForTest_NotCalledOutsideTests guards
// WipeSubstrateAgentStateForTest itself against a real production reference
// anywhere in the module: it fails if any non-test .go file references the
// identifier anywhere but its own declaration. Since the symbol can't be
// moved to an _test.go file (a cross-package test importing this package in
// its normal, non-test build would then fail to link), this substitutes a
// repo-wide scan using referencesOutsideDeclaration, whose own
// false-positive/false-negative shapes are pinned directly above.
func TestWipeSubstrateAgentStateForTest_NotCalledOutsideTests(t *testing.T) {
	root := findModuleRootForTest(t)
	var badHits []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			switch info.Name() {
			case "vendor", "testdata", "node_modules", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, hit := range referencesOutsideDeclaration(wipeGuardSymbol, data) {
			badHits = append(badHits, fmt.Sprintf("%s:%s", path, hit))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking module root %q: %v", root, err)
	}
	if len(badHits) > 0 {
		t.Errorf("%s referenced outside tests and its own declaration:\n%s", wipeGuardSymbol, strings.Join(badHits, "\n"))
	}
}

// findModuleRootForTest walks up from the current working directory (a Go
// test's package directory) until it finds go.mod.
func findModuleRootForTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod by walking up from the test's working directory")
		}
		dir = parent
	}
}
