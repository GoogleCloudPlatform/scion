/*
Copyright 2026 The Scion Authors.
*/

package dialects

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHasTurnHooks(t *testing.T) {
	for _, harness := range []string{"claude", "gemini-cli", "codex"} {
		if !HasTurnHooks(harness, "") {
			t.Errorf("HasTurnHooks(%q) = false, want true (built-in dialect)", harness)
		}
	}

	home := t.TempDir()
	if HasTurnHooks("hermes", home) {
		t.Error("HasTurnHooks(hermes) without dialect.yaml = true, want false")
	}
	if HasTurnHooks("opencode", "") {
		t.Error("HasTurnHooks with no agent home = true, want false")
	}

	path := HarnessDialectPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "elsewhere"), path); err != nil {
		t.Fatal(err)
	}
	if HasTurnHooks("opencode", home) {
		t.Error("HasTurnHooks with a symlinked dialect.yaml = true, want false")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("dialect: opencode\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !HasTurnHooks("opencode", home) {
		t.Error("HasTurnHooks with a staged dialect.yaml = false, want true")
	}
}
