/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

// A TUI tail is mostly box drawing (3-byte runes). Wherever the sanitize
// window's start falls relative to a credential, the credential is never
// kept without its key: the prefix ("Bearer", "API_KEY:", "token=") may
// fall outside the window, so the window's leading context is dropped.
func TestFormatHarnessTailReason_StraddlingCredentialIsRedacted(t *testing.T) {
	const token = "Xq7pL2mN9vR4tK8w" // short: no long-run redaction
	for _, source := range []string{"x", "Agent crashed with exit code 1", "harness did not start the initial task"} {
		for _, cred := range []string{
			"Authorization: Bearer " + token,
			"API_KEY: " + token,
			"token=" + token,
		} {
			for pad := 0; pad < 300; pad++ {
				text := strings.Repeat("─", 200) + " " + cred + " " + strings.Repeat("─", pad) + " Error: please run /login"
				got := FormatHarnessTailReason(source, text)
				if strings.Contains(got, token) {
					t.Fatalf("credential kept (%q, %q, pad %d): %q", source, cred, pad, got)
				}
				assert.True(t, utf8.ValidString(got))
				assert.LessOrEqual(t, utf8.RuneCountInString(got), harnessErrorReasonMaxRunes)
				assert.True(t, strings.HasSuffix(got, "Error: please run /login"), got)
			}
		}
	}
}

// The newest end of an ASCII tail is kept up to the rune budget.
func TestFormatHarnessTailReason_KeepsNewestEnd(t *testing.T) {
	text := strings.Repeat("old line of output ", 200) + "final: model not found"
	got := FormatHarnessTailReason("harness did not start the initial task", text)
	assert.True(t, strings.HasSuffix(got, "final: model not found"), got)
	assert.Greater(t, utf8.RuneCountInString(got), harnessErrorReasonMaxRunes-40, "too much of the budget lost: %q", got)
}
