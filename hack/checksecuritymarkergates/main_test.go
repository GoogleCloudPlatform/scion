package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGate_ResolveOutboundRoutingDelegation verifies that removing the
// resolveOutboundRouting call from handleAgentOutboundMessage causes a gate
// failure. This is the delegation gate added in #1497.
func TestGate_ResolveOutboundRoutingDelegation(t *testing.T) {
	hamPath := repoRelPath(t, "pkg/hub/handlers_agent_messaging.go")
	fset := token.NewFileSet()
	f := mustParseTest(t, fset, hamPath)

	// Positive: the delegation call exists.
	if n := countIdentsInFunc(f, "handleAgentOutboundMessage", "resolveOutboundRouting"); n < 1 {
		t.Fatalf("expected resolveOutboundRouting in handleAgentOutboundMessage, found x%d", n)
	}

	// Mutation: remove the delegation call and verify countIdentsInFunc
	// returns 0 (which would cause the gate to fail).
	mutated := removeIdentFromFunc(t, hamPath, "handleAgentOutboundMessage", "resolveOutboundRouting")
	mf := mustParseSource(t, fset, mutated)
	if n := countIdentsInFunc(mf, "handleAgentOutboundMessage", "resolveOutboundRouting"); n != 0 {
		t.Fatalf("mutated file should have 0 resolveOutboundRouting in handleAgentOutboundMessage, found x%d", n)
	}
}

// TestGate_ParseDMKeyIDsInResolveOutboundRouting verifies that removing
// parseDMKeyIDs from resolveOutboundRouting causes a gate failure.
func TestGate_ParseDMKeyIDsInResolveOutboundRouting(t *testing.T) {
	hamPath := repoRelPath(t, "pkg/hub/handlers_agent_messaging.go")
	fset := token.NewFileSet()
	f := mustParseTest(t, fset, hamPath)

	// Positive: the call exists in resolveOutboundRouting.
	if n := countIdentsInFunc(f, "resolveOutboundRouting", "parseDMKeyIDs"); n < 1 {
		t.Fatalf("expected parseDMKeyIDs in resolveOutboundRouting, found x%d", n)
	}

	// Mutation: remove parseDMKeyIDs and verify the count drops.
	mutated := removeIdentFromFunc(t, hamPath, "resolveOutboundRouting", "parseDMKeyIDs")
	mf := mustParseSource(t, fset, mutated)
	if n := countIdentsInFunc(mf, "resolveOutboundRouting", "parseDMKeyIDs"); n != 0 {
		t.Fatalf("mutated file should have 0 parseDMKeyIDs in resolveOutboundRouting, found x%d", n)
	}
}

// TestGate_ValidateLegacyMessageInResolveOutboundRouting verifies that removing
// ValidateLegacyMessage from resolveOutboundRouting causes a gate failure.
func TestGate_ValidateLegacyMessageInResolveOutboundRouting(t *testing.T) {
	hamPath := repoRelPath(t, "pkg/hub/handlers_agent_messaging.go")
	fset := token.NewFileSet()
	f := mustParseTest(t, fset, hamPath)

	// Positive: the call exists in resolveOutboundRouting.
	if n := countIdentsInFunc(f, "resolveOutboundRouting", "ValidateLegacyMessage"); n < 1 {
		t.Fatalf("expected ValidateLegacyMessage in resolveOutboundRouting, found x%d", n)
	}

	// Mutation: remove ValidateLegacyMessage and verify the count drops.
	mutated := removeIdentFromFunc(t, hamPath, "resolveOutboundRouting", "ValidateLegacyMessage")
	mf := mustParseSource(t, fset, mutated)
	if n := countIdentsInFunc(mf, "resolveOutboundRouting", "ValidateLegacyMessage"); n != 0 {
		t.Fatalf("mutated file should have 0 ValidateLegacyMessage in resolveOutboundRouting, found x%d", n)
	}
}

// TestGate_ValidateAttributedInResolveOutboundRouting verifies that removing
// ValidateAttributed from resolveOutboundRouting causes a gate failure.
func TestGate_ValidateAttributedInResolveOutboundRouting(t *testing.T) {
	hamPath := repoRelPath(t, "pkg/hub/handlers_agent_messaging.go")
	fset := token.NewFileSet()
	f := mustParseTest(t, fset, hamPath)

	// Positive: the call exists in resolveOutboundRouting.
	if n := countIdentsInFunc(f, "resolveOutboundRouting", "ValidateAttributed"); n < 1 {
		t.Fatalf("expected ValidateAttributed in resolveOutboundRouting, found x%d", n)
	}

	// Mutation: remove ValidateAttributed and verify the count drops.
	mutated := removeIdentFromFunc(t, hamPath, "resolveOutboundRouting", "ValidateAttributed")
	mf := mustParseSource(t, fset, mutated)
	if n := countIdentsInFunc(mf, "resolveOutboundRouting", "ValidateAttributed"); n != 0 {
		t.Fatalf("mutated file should have 0 ValidateAttributed in resolveOutboundRouting, found x%d", n)
	}
}

// TestGate_FullBinaryMutationDelegation runs the actual gate-checker binary
// against a mutated codebase where resolveOutboundRouting is removed from
// handleAgentOutboundMessage. The binary must exit non-zero.
func TestGate_FullBinaryMutationDelegation(t *testing.T) {
	runMutationBinaryTest(t, "pkg/hub/handlers_agent_messaging.go",
		"handleAgentOutboundMessage", "resolveOutboundRouting")
}

// TestGate_FullBinaryMutationParseDMKeyIDs runs the actual gate-checker binary
// against a mutated codebase where parseDMKeyIDs is removed from
// resolveOutboundRouting. The binary must exit non-zero.
func TestGate_FullBinaryMutationParseDMKeyIDs(t *testing.T) {
	runMutationBinaryTest(t, "pkg/hub/handlers_agent_messaging.go",
		"resolveOutboundRouting", "parseDMKeyIDs")
}

// TestGate_FullBinaryMutationValidateLegacyMessage runs the actual gate-checker
// binary against a mutated codebase where ValidateLegacyMessage is removed from
// resolveOutboundRouting. The binary must exit non-zero.
func TestGate_FullBinaryMutationValidateLegacyMessage(t *testing.T) {
	runMutationBinaryTest(t, "pkg/hub/handlers_agent_messaging.go",
		"resolveOutboundRouting", "ValidateLegacyMessage")
}

// TestGate_FullBinaryMutationValidateAttributed runs the actual gate-checker
// binary against a mutated codebase where ValidateAttributed is removed from
// resolveOutboundRouting. The binary must exit non-zero.
func TestGate_FullBinaryMutationValidateAttributed(t *testing.T) {
	runMutationBinaryTest(t, "pkg/hub/handlers_agent_messaging.go",
		"resolveOutboundRouting", "ValidateAttributed")
}

// ---------- helpers ----------

// repoRelPath resolves a repo-relative path from the repo root.
func repoRelPath(t *testing.T, rel string) string {
	t.Helper()
	// The gate checker runs from the repo root (cd "$(dirname "$0")/..").
	// During tests, we need to find the repo root.
	root := findRepoRoot(t)
	return filepath.Join(root, rel)
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	// Walk up from the current directory looking for go.mod.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (no go.mod found)")
		}
		dir = parent
	}
}

func mustParseTest(t *testing.T, fset *token.FileSet, path string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return f
}

func mustParseSource(t *testing.T, fset *token.FileSet, src string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(fset, "mutated.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse mutated source: %v", err)
	}
	return f
}

// removeIdentFromFunc reads the source file, finds every occurrence of
// `symbol` as a standalone identifier within the function `funcName`, and
// renames it to `__removed__` so the AST no longer matches. Returns the
// modified source as a string.
//
// This is intentionally coarse: we do a token-level rewrite rather than a
// textual find/replace to avoid collateral damage to identifiers that happen
// to be substrings of the symbol name.
func removeIdentFromFunc(t *testing.T, path, funcName, symbol string) string {
	t.Helper()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	// Find the function body bounds.
	var bodyStart, bodyEnd int
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		if fd.Name.Name == funcName {
			bodyStart = int(fd.Body.Pos()) - 1 // Pos is 1-based
			bodyEnd = int(fd.Body.End()) - 1
			break
		}
	}
	if bodyStart == 0 && bodyEnd == 0 {
		t.Fatalf("function %s not found in %s", funcName, path)
	}

	// Find all idents matching `symbol` inside the function body.
	type replacement struct {
		start, end int
	}
	var replacements []replacement
	ast.Inspect(f, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if !ok || ident.Name != symbol {
			return true
		}
		pos := int(ident.Pos()) - 1
		end := int(ident.End()) - 1
		if pos >= bodyStart && end <= bodyEnd {
			replacements = append(replacements, replacement{pos, end})
		}
		return true
	})

	if len(replacements) == 0 {
		t.Fatalf("no occurrences of %s found in %s body in %s", symbol, funcName, path)
	}

	// Apply replacements in reverse order to preserve offsets.
	result := string(src)
	for i := len(replacements) - 1; i >= 0; i-- {
		r := replacements[i]
		result = result[:r.start] + "__removed__" + result[r.end:]
	}

	return result
}

// buildGateChecker compiles the gate-checker binary once per test binary run
// and returns the path. Subtests that need it call this helper.
func buildGateChecker(t *testing.T, root string) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "gate-checker")
	buildCmd := exec.Command("go", "build", "-o", binPath, "./hack/checksecuritymarkergates")
	buildCmd.Dir = root
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build gate checker: %v\n%s", err, out)
	}
	return binPath
}

// prepareOverlayWorkspace creates a temporary directory that overlays guarded
// files via symlinks. If mutatedContent is non-empty, the file at relPath is
// written with that content instead of symlinked. When mutatedContent is empty
// all files are symlinked unchanged (clean baseline).
func prepareOverlayWorkspace(t *testing.T, root, relPath, mutatedContent string) string {
	t.Helper()

	tmpDir := t.TempDir()

	guardedFiles := []string{
		"pkg/hub/handlers_agent_messaging.go",
		"pkg/hub/handlers_chat_v2.go",
		"pkg/hub/messagebroker.go",
		"pkg/hub/handlers_broker_inbound.go",
		"pkg/hub/notifications.go",
		"pkg/hub/server.go",
	}

	handlerGlob, _ := filepath.Glob(filepath.Join(root, "pkg/hub/handlers_*.go"))

	allFiles := make(map[string]bool)
	for _, f := range guardedFiles {
		allFiles[f] = true
	}
	for _, f := range handlerGlob {
		rel, _ := filepath.Rel(root, f)
		allFiles[rel] = true
	}

	for rel := range allFiles {
		dir := filepath.Dir(filepath.Join(tmpDir, rel))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if mutatedContent != "" && rel == relPath {
			if err := os.WriteFile(filepath.Join(tmpDir, rel), []byte(mutatedContent), 0o644); err != nil {
				t.Fatalf("write mutated file: %v", err)
			}
		} else {
			src := filepath.Join(root, rel)
			dst := filepath.Join(tmpDir, rel)
			if _, err := os.Stat(dst); err == nil {
				continue
			}
			if err := os.Symlink(src, dst); err != nil {
				t.Fatalf("symlink %s -> %s: %v", src, dst, err)
			}
		}
	}

	return tmpDir
}

// runMutationBinaryTest builds the gate checker, creates a temporary workspace
// with the specified file mutated (symbol removed from funcName), and runs the
// binary. Verifies:
//  1. Exit code is exactly 1 (gate failure), not 2 (analysis error).
//  2. Output contains the exact FAIL row for the specific mutation, not just
//     the symbol name anywhere in output.
//  3. Baseline recovery: the same overlay with the original (unmutated) file
//     exits 0 with "all gates pass".
func runMutationBinaryTest(t *testing.T, relPath, funcName, symbol string) {
	t.Helper()

	root := findRepoRoot(t)
	fullPath := filepath.Join(root, relPath)

	binPath := buildGateChecker(t, root)

	// Create the mutated file.
	mutated := removeIdentFromFunc(t, fullPath, funcName, symbol)

	// --- Phase 1: mutated overlay must fail with exit code 1 ---

	mutatedDir := prepareOverlayWorkspace(t, root, relPath, mutated)

	cmd := exec.Command(binPath)
	cmd.Dir = mutatedDir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected gate checker to fail after removing %s from %s, but it passed.\nOutput:\n%s",
			symbol, funcName, out)
	}

	// Assert exit code is exactly 1 (gate failure), not 2 (analysis error).
	exitCode := cmd.ProcessState.ExitCode()
	if exitCode != 1 {
		t.Fatalf("expected exit code 1 (gate failure), got %d — exit 2 means "+
			"'could not analyse' (environment failure, not a security statement).\nOutput:\n%s",
			exitCode, out)
	}

	// Assert the EXACT expected FAIL row for this mutation. The gate checker
	// emits "FAIL [REQUIRED] <symbol> in <funcName> (...)" for each failed
	// required gate. A bare symbol-name substring check is too weak: NOTICE
	// and PASS lines also print symbol names (e.g. DEF-37 exemptions print
	// on every run).
	expectedFailPrefix := "FAIL [REQUIRED] " + symbol + " in " + funcName
	outStr := string(out)
	if !strings.Contains(outStr, expectedFailPrefix) {
		t.Errorf("gate checker output does not contain the expected FAIL row.\n"+
			"  want (prefix): %s\n"+
			"  full output:\n%s", expectedFailPrefix, outStr)
	}

	// --- Phase 2: clean baseline must pass with exit 0 ---

	baselineDir := prepareOverlayWorkspace(t, root, relPath, "" /* no mutation */)

	baselineCmd := exec.Command(binPath)
	baselineCmd.Dir = baselineDir
	baselineOut, baselineErr := baselineCmd.CombinedOutput()
	if baselineErr != nil {
		t.Fatalf("baseline (unmutated) gate checker run failed unexpectedly.\n"+
			"  exit code: %d\n  output:\n%s",
			baselineCmd.ProcessState.ExitCode(), baselineOut)
	}
	if !strings.Contains(string(baselineOut), "all gates pass") {
		t.Errorf("baseline run did not report 'all gates pass'.\n  output:\n%s", baselineOut)
	}
}
