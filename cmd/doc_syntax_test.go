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

package cmd

// Parse-check validates that fenced `scion ...` code examples in docs resolve
// to real commands with valid flags. It does NOT verify that a command does what
// the prose says it does — only that the syntax is recognised by the cobra tree.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// extractScionLines returns every `scion ...` command from fenced code blocks.
func extractScionLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err, "reading %s", path)
	var lines []string
	inFence := false
	for _, raw := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(raw), "```") {
			inFence = !inFence
			continue
		}
		if !inFence {
			continue
		}
		s := strings.TrimSpace(raw)
		s = strings.TrimPrefix(s, "$ ")
		s = strings.TrimPrefix(s, "# ") // shell prompt, not comment
		if strings.HasPrefix(s, "#") {  // comment line
			continue
		}
		if !strings.HasPrefix(s, "scion ") {
			continue
		}
		// Skip placeholders, usage patterns, shell interpolations, and
		// cobra built-ins (help/completion) that aren't discoverable via Find.
		if strings.ContainsAny(s, "<>$") || strings.Contains(s, "[flags]") || strings.Contains(s, "...") {
			continue
		}
		if strings.HasPrefix(s, "scion help ") || s == "scion help" {
			continue
		}
		lines = append(lines, s)
	}
	return lines
}

// findCommandProblems validates that each scion command line resolves to a real
// cobra command with valid flags. It returns a list of human-readable problems.
//
// I-2 fix: cobra's Find returns the deepest match and leaves unrecognised
// tokens in rest without error. When the resolved command is a pure group
// (has subcommands but no Run/RunE of its own) and the first unconsumed
// token is not a flag, it must match a registered subcommand — otherwise
// the doc example references a command that doesn't exist. Commands that
// have their own RunE accept positional args, so the unconsumed token is
// valid in that case.
func findCommandProblems(lines []string, source string) []string {
	var problems []string
	for _, line := range lines {
		args := strings.Fields(line)[1:] // strip "scion"
		cmd, rest, findErr := rootCmd.Find(args)
		if findErr != nil {
			problems = append(problems,
				fmt.Sprintf("command not found: %s (from %s)", line, source))
			continue
		}

		// I-2: detect unconsumed subcommand-like tokens on pure group
		// commands (no Run/RunE). Runnable commands accept positional
		// args, so non-flag tokens are valid there.
		if cmd.HasSubCommands() && !cmd.Runnable() && len(rest) > 0 {
			first := rest[0]
			if !strings.HasPrefix(first, "-") {
				found := false
				for _, sub := range cmd.Commands() {
					if sub.Name() == first {
						found = true
						break
					}
				}
				if !found {
					problems = append(problems,
						fmt.Sprintf("unknown subcommand %q for %q: %s (from %s)",
							first, cmd.Name(), line, source))
				}
			}
		}

		// Validate flag names exist without calling ParseFlags, which
		// mutates global cobra state (Changed bits + bound variables)
		// and breaks other tests in the full suite.
		for _, tok := range rest {
			if !strings.HasPrefix(tok, "-") {
				continue // positional arg or flag value
			}
			name := strings.TrimLeft(tok, "-")
			if i := strings.Index(name, "="); i >= 0 {
				name = name[:i]
			}
			if name == "" {
				continue
			}
			if cmd.Flags().Lookup(name) == nil {
				problems = append(problems,
					fmt.Sprintf("unknown flag --%s: %s (from %s)", name, line, source))
			}
		}
	}
	return problems
}

// findDenyListProblems returns problems for any command lines that contain
// deny-listed patterns.
func findDenyListProblems(lines []string, denyPatterns []string, source string) []string {
	var problems []string
	for _, line := range lines {
		for _, pat := range denyPatterns {
			if strings.Contains(line, pat) {
				problems = append(problems,
					fmt.Sprintf("deny-listed pattern %q in code block: %s (from %s)",
						pat, line, source))
			}
		}
	}
	return problems
}

func TestDocSyntax(t *testing.T) {
	docFiles := []string{
		"../resources/platform_skills/scion-messaging/SKILL.md",
		"../docs-site/src/content/docs/hosted/user/messaging.md",
		"../docs-site/src/content/docs/reference/cli.md",
		"../docs-site/src/content/docs/glossary.md",
	}

	denyPatterns := []string{
		"scion message conv:", "scion message \"conv:",
		"scion message #", "scion message \"#",
		"scion msg conv:", "scion msg \"conv:",
		"scion msg #", "scion msg \"#",
	}

	totalLines := 0
	for _, rel := range docFiles {
		abs, err := filepath.Abs(rel)
		require.NoError(t, err)
		_, statErr := os.Stat(abs)
		require.NoError(t, statErr, "doc file missing: %s — update docFiles or restore the file", rel)
		lines := extractScionLines(t, abs)
		totalLines += len(lines)
		for _, p := range findCommandProblems(lines, rel) {
			t.Error(p)
		}
		for _, p := range findDenyListProblems(lines, denyPatterns, rel) {
			t.Error(p)
		}
	}
	// A drop means either the extractor broke or examples were deleted.
	// Raise this floor when adding examples; never lower it.
	// Floor: raise when adding examples to doc files. Current floor reflects
	// the docs on the C6-CLI branch (messaging docs are not yet updated with
	// new examples — the floor will be raised when that work lands).
	require.GreaterOrEqual(t, totalLines, 2,
		"expected at least 2 scion command lines across all doc files; got %d — "+
			"raise this floor when adding examples, never lower it", totalLines)

	// Deny-list scan of cobra tree Long and Example fields.
	// Guards against gated forms (conv:, #) appearing as runnable
	// examples in --help output.
	t.Run("cobra_help_deny_list", func(t *testing.T) {
		var cobraLines []string
		var walkCmd func(c *cobra.Command)
		walkCmd = func(c *cobra.Command) {
			for _, text := range []string{c.Long, c.Example} {
				if text == "" {
					continue
				}
				for _, line := range strings.Split(text, "\n") {
					s := strings.TrimSpace(line)
					if strings.HasPrefix(s, "scion ") {
						cobraLines = append(cobraLines, s)
					}
				}
			}
			for _, sub := range c.Commands() {
				walkCmd(sub)
			}
		}
		walkCmd(rootCmd)
		for _, p := range findDenyListProblems(cobraLines, denyPatterns, "cobra-help-text") {
			t.Error(p)
		}
	})

	// Rule 10: prove parse-check catches bad syntax.
	// These subtests call the same findCommandProblems / findDenyListProblems
	// functions used by the main body — deleting those functions would break
	// these subtests too (I-3 fix).
	t.Run("catches_bad_command", func(t *testing.T) {
		tmp := filepath.Join(t.TempDir(), "bad.md")
		require.NoError(t, os.WriteFile(tmp, []byte("```bash\nscion nonexistent-command --fake-flag\n```\n"), 0644))
		lines := extractScionLines(t, tmp)
		require.Len(t, lines, 1)
		problems := findCommandProblems(lines, tmp)
		assert.NotEmpty(t, problems,
			"expected findCommandProblems to reject unknown command")
	})

	// Rule 10: prove deny-list catches gated forms.
	t.Run("catches_deny_listed_pattern", func(t *testing.T) {
		tmp := filepath.Join(t.TempDir(), "deny.md")
		require.NoError(t, os.WriteFile(tmp, []byte("```bash\nscion message conv:abc123 \"hello\"\n```\n"), 0644))
		lines := extractScionLines(t, tmp)
		require.Len(t, lines, 1)
		problems := findDenyListProblems(lines, denyPatterns, tmp)
		assert.NotEmpty(t, problems,
			"expected findDenyListProblems to catch gated conv: pattern")
	})

	// Rule 10: prove the I-2 blind-spot fix catches unconsumed subcommands.
	t.Run("catches_unconsumed_subcommand", func(t *testing.T) {
		tmp := filepath.Join(t.TempDir(), "bad-sub.md")
		require.NoError(t, os.WriteFile(tmp, []byte("```bash\nscion schedule message --in 5m\n```\n"), 0644))
		lines := extractScionLines(t, tmp)
		require.Len(t, lines, 1)

		// Verify the blind spot: rootCmd.Find succeeds (no error) but
		// "message" is left as an unconsumed non-flag token after
		// "schedule", which is a pure group with no "message" subcommand.
		args := strings.Fields(lines[0])[1:] // ["schedule", "message", "--in", "5m"]
		cmd, rest, err := rootCmd.Find(args)
		require.NoError(t, err, "Find should succeed (returns deepest match)")
		require.True(t, cmd.HasSubCommands(), "schedule should have subcommands")
		require.False(t, cmd.Runnable(), "schedule should be a pure group (no Run/RunE)")
		require.True(t, len(rest) > 0, "should have unconsumed args")
		assert.Equal(t, "message", rest[0],
			"first unconsumed token should be 'message'")

		// The same function used by the main body should catch this.
		problems := findCommandProblems(lines, tmp)
		assert.NotEmpty(t, problems,
			"expected findCommandProblems to catch unconsumed subcommand 'message' on 'schedule'")
	})
}
