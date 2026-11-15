/*
Copyright 2026 The Scion Authors.
*/

package dialects

import (
	"os"
	"path/filepath"
)

// builtinDialectHarnesses are the SCION_HARNESS values whose hook dialect
// is built in (RegisterBuiltins): their hook configuration ships with the
// harness image rather than as a bundled dialect.yaml.
var builtinDialectHarnesses = map[string]bool{
	"claude":     true,
	"gemini-cli": true,
	"codex":      true,
}

// HarnessDialectPath is where a harness bundle's dialect.yaml sits under
// the agent home (see DiscoverMappingDialect).
func HarnessDialectPath(agentHome string) string {
	return filepath.Join(agentHome, ".scion", "harness", "dialect.yaml")
}

// HasTurnHooks reports whether the harness declares turn hooks: it has a
// built-in dialect, or its bundle staged a dialect.yaml under agentHome. A
// harness without hooks (hermes) reports no turn events at all, so a
// watchdog waiting for one must not run for it.
func HasTurnHooks(harness, agentHome string) bool {
	if builtinDialectHarnesses[harness] {
		return true
	}
	if agentHome == "" {
		return false
	}
	fi, err := os.Lstat(HarnessDialectPath(agentHome))
	return err == nil && fi.Mode().IsRegular()
}
