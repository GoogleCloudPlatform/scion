/*
Copyright 2025 The Scion Authors.
*/

package dialects

import "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"

// RegisterBuiltins registers the built-in harness dialects.
// This creates a single extension point for adding future dialects.
func RegisterBuiltins(processor *hooks.HarnessProcessor) {
	processor.RegisterDialect(NewClaudeDialect())
	processor.RegisterDialect(NewGeminiDialect())
	processor.RegisterDialect(NewCodexDialect())
	processor.RegisterDialect(NewOpenCodeDialect())
}
