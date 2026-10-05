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

package substrate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// execResolveGuardSymbol is the identifier
// TestSetExecResolveForTest_NotCalledOutsideTests and
// execResolveReferencesOutsideDeclaration both guard: SetExecResolveForTest
// (execuser.go) is exported, and therefore compiled into the production
// binary, only because pkg/runtime's tests need it and cannot reach an
// unexported symbol in this package. Nothing in production may call it —
// doing so would let a real exec silently resolve "sh", "su", or "whoami"
// to whatever a test last installed.
const execResolveGuardSymbol = "SetExecResolveForTest"

// execResolveGuardDeclRe matches only the exact declaration line, anchored
// so a longer identifier that merely starts with the symbol can never be
// mistaken for it: the symbol must be followed immediately by "(", and the
// match must start at the beginning of the (trimmed) line.
var execResolveGuardDeclRe = regexp.MustCompile(`^func ` + regexp.QuoteMeta(execResolveGuardSymbol) + `\(`)

// execResolveReferencesOutsideDeclaration scans data (one file's contents)
// for references to symbol that are not the symbol's own declaration, and
// returns one formatted "<line>: <text>" entry per reference found. See
// pkg/runtime/substrate_wipe_guard_test.go's referencesOutsideDeclaration,
// which this mirrors exactly (rule for rule) for the same reason: the two
// guarded symbols live in different packages, so the check itself can't be
// shared, only its shape.
func execResolveReferencesOutsideDeclaration(symbol string, data []byte) []string {
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
		if loc := execResolveGuardDeclRe.FindStringIndex(trimmed); loc != nil {
			rest := trimmed[loc[1]:]
			if !strings.Contains(rest, symbol) {
				continue // exactly the declaration; nothing else on the line
			}
		}
		hits = append(hits, fmt.Sprintf("%d: %s", i+1, trimmed))
	}
	return hits
}

// TestExecResolveReferencesOutsideDeclaration_DeclarationAndProseAreNotHits
// pins the two "must not false-positive" shapes: the real declaration line,
// and an ordinary doc-comment sentence that happens to mention the symbol.
func TestExecResolveReferencesOutsideDeclaration_DeclarationAndProseAreNotHits(t *testing.T) {
	data := []byte(`// SetExecResolveForTest overrides the resolver for a test.
// See SetExecResolveForTest's caller for details.
func SetExecResolveForTest(resolve func(name string) (string, error)) func() {
	return func() {}
}
`)
	if hits := execResolveReferencesOutsideDeclaration(execResolveGuardSymbol, data); len(hits) != 0 {
		t.Errorf("execResolveReferencesOutsideDeclaration() = %v, want none", hits)
	}
}

// TestExecResolveReferencesOutsideDeclaration_OrdinaryCallIsAHit is the base
// sanity case: a plain production call must be caught.
func TestExecResolveReferencesOutsideDeclaration_OrdinaryCallIsAHit(t *testing.T) {
	data := []byte(`func bad() {
	restore := SetExecResolveForTest(nil)
	_ = restore
}
`)
	hits := execResolveReferencesOutsideDeclaration(execResolveGuardSymbol, data)
	if len(hits) != 1 {
		t.Fatalf("execResolveReferencesOutsideDeclaration() = %v, want exactly one hit", hits)
	}
}

// TestExecResolveReferencesOutsideDeclaration_LongerIdentifierSharingTheLineIsAHit
// kills a guard that skips any line merely starting with "func "+symbol: a
// production function whose name is a longer identifier built from the
// symbol can still call the real symbol on the same line.
func TestExecResolveReferencesOutsideDeclaration_LongerIdentifierSharingTheLineIsAHit(t *testing.T) {
	data := []byte(`func SetExecResolveForTestAndLog() { SetExecResolveForTest(nil) }
`)
	hits := execResolveReferencesOutsideDeclaration(execResolveGuardSymbol, data)
	if len(hits) != 1 {
		t.Fatalf("execResolveReferencesOutsideDeclaration() = %v, want exactly one hit (the embedded call, not the longer declaration)", hits)
	}
}

// TestExecResolveReferencesOutsideDeclaration_LinknameCommentIsAHit kills a
// guard that treats every "//"-prefixed line as exempt: a //go:linkname
// directive naming this symbol, paired with a matching declaration
// elsewhere plus `import _ "unsafe"`, lets the linker resolve a call to an
// unexported production function straight to this exported symbol without
// any line of code ever spelling "SetExecResolveForTest(".
func TestExecResolveReferencesOutsideDeclaration_LinknameCommentIsAHit(t *testing.T) {
	data := []byte(`//go:linkname zzSetExecResolve github.com/GoogleCloudPlatform/scion/pkg/sciontool/substrate.SetExecResolveForTest
func zzSetExecResolve()
`)
	hits := execResolveReferencesOutsideDeclaration(execResolveGuardSymbol, data)
	if len(hits) != 1 {
		t.Fatalf("execResolveReferencesOutsideDeclaration() = %v, want exactly one hit (the go:linkname comment)", hits)
	}
}

// TestSetExecResolveForTest_NotCalledOutsideTests guards SetExecResolveForTest
// itself against a real production reference anywhere in the module: it
// fails if any non-test .go file references the identifier anywhere but its
// own declaration. Since the symbol can't be moved to an _test.go file (a
// cross-package test importing this package in its normal, non-test build
// would then fail to link), this substitutes a repo-wide scan using
// execResolveReferencesOutsideDeclaration, whose own false-positive/
// false-negative shapes are pinned directly above.
func TestSetExecResolveForTest_NotCalledOutsideTests(t *testing.T) {
	root := execResolveGuardModuleRoot(t)
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
		for _, hit := range execResolveReferencesOutsideDeclaration(execResolveGuardSymbol, data) {
			badHits = append(badHits, fmt.Sprintf("%s:%s", path, hit))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking module root %q: %v", root, err)
	}
	if len(badHits) > 0 {
		t.Errorf("%s referenced outside tests and its own declaration:\n%s", execResolveGuardSymbol, strings.Join(badHits, "\n"))
	}
}

// execResolveGuardModuleRoot walks up from the current working directory (a
// Go test's package directory) until it finds go.mod.
func execResolveGuardModuleRoot(t *testing.T) string {
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
