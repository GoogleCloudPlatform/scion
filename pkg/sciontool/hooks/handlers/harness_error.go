/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// harnessErrorReasonMaxRunes caps the harness_error reason stored in
// detail.message and sent to the Hub.
const harnessErrorReasonMaxRunes = 300

// harnessErrorSourceModelAPI names the source of a turn-end error that a
// harness reported on its completion hook: the model API call failed.
const harnessErrorSourceModelAPI = "model API"

// isTurnEndError reports whether event is the parent agent's turn ending
// on an error the harness reported: agent-end with Data.Error set, or
// agent-error (a harness's dedicated failed-turn hook). Such a
// turn puts the agent in harness_error: the harness is alive at its prompt
// but its turn failed. A sub-agent's agent-end is not the parent's turn and
// does not count. model-end and tool-end errors do not count either: the
// harness may retry a model call, and a failed tool is normal work.
func isTurnEndError(event *hooks.Event) bool {
	if isSubagentEvent(event) {
		return false
	}
	switch event.Name {
	case hooks.EventAgentError:
		return true
	case hooks.EventAgentEnd:
		return strings.TrimSpace(event.Data.Error) != ""
	}
	return false
}

// harnessErrorReason returns the harness_error reason for a turn-end error
// event, in the form "<source>: <text>".
func harnessErrorReason(event *hooks.Event) string {
	return formatHarnessErrorReason(harnessErrorSourceModelAPI, event.Data.Error)
}

// FormatHarnessTailReason renders "<source>: <text>" for text that is a
// harness's last output, oldest first: sciontool init uses it to append a
// crashed harness's pane tail to its crash reason. The text gets the same
// escape stripping, single-lining and redaction as a harness_error reason,
// and the whole reason the same harnessErrorReasonMaxRunes cap, but the
// text is cut from its start ("…" then the newest output), so the last
// line, usually the error, survives.
func FormatHarnessTailReason(source, text string) string {
	text = sanitizeTailEnd(singleLine(terminalEscapeRe.ReplaceAllString(text, "")))
	if text == "" {
		return truncateRunes(source, harnessErrorReasonMaxRunes)
	}
	prefix := source + ": "
	budget := harnessErrorReasonMaxRunes - utf8.RuneCountInString(prefix)
	if budget < 2 {
		return truncateRunes(prefix+text, harnessErrorReasonMaxRunes)
	}
	return prefix + truncateRunesFromStart(text, budget)
}

// The sanitize window of a tail (sanitizeTailEnd), sized in runes: a TUI
// tail is often mostly box drawing, 3 bytes a rune, so a byte-sized window
// can hold fewer runes than the reason keeps.
const (
	// tailContextRunes is the leading part of a cut window that is
	// sanitized but never kept: whatever straddles the window's start
	// (a key such as "Bearer", "API_KEY:" or "token=" cut off from its
	// value) sits there.
	tailContextRunes = 64
	// tailWindowRunes is the window before it shrinks to fit
	// tailSanitizeMaxBytes: the rune budget plus the context, with room.
	tailWindowRunes = harnessErrorReasonMaxRunes + 2*tailContextRunes
	// tailSanitizeMaxBytes bounds the window passed to
	// logging.SanitizeText, under its MaxSanitizedTextBytes (past which it
	// cuts the end, the newest output), with room for redaction markers.
	tailSanitizeMaxBytes = logging.MaxSanitizedTextBytes - 128
)

// sanitizeTailEnd returns the end of a single-lined tail, redacted by
// logging.SanitizeText and single-lined again. A long tail is cut to a
// window of its last runes: tailWindowRunes, shrunk until it fits
// tailSanitizeMaxBytes and SanitizeText's result fits its cap. The window
// is split at the first space after its first tailContextRunes runes into
// a context part and a kept part, joined by a newline, and sanitized
// together, so a key in the context still redacts its value in the kept
// part (the key patterns' separators match a newline). Only what follows
// the newline is kept. If redaction swallowed the newline (a PEM block
// running to the end), nothing is kept.
func sanitizeTailEnd(text string) string {
	runes := []rune(text)
	window := tailWindowRunes
	for {
		n := min(window, len(runes))
		for n > 2*tailContextRunes && len(string(runes[len(runes)-n:])) > tailSanitizeMaxBytes {
			n = n * 9 / 10
		}
		if n == len(runes) {
			out := singleLine(logging.SanitizeText(text))
			if len(out) < logging.MaxSanitizedTextBytes || n <= 2*tailContextRunes {
				return out
			}
			window = n * 3 / 4
			continue
		}
		context := string(runes[len(runes)-n : len(runes)-n+tailContextRunes])
		rest := string(runes[len(runes)-n+tailContextRunes:])
		out, ok := sanitizeSplitAtWord(context, rest)
		if !ok {
			return ""
		}
		if len(out) >= logging.MaxSanitizedTextBytes && n > 2*tailContextRunes {
			window = n * 3 / 4
			continue
		}
		j := strings.IndexByte(out, '\n')
		return singleLine(out[j+1:])
	}
}

// tailSplitAttempts bounds how many word boundaries sanitizeSplitAtWord
// tries.
const tailSplitAttempts = 8

// sanitizeSplitAtWord sanitizes context+rest with a newline in place of a
// space of rest, and returns the result when it still holds that newline.
// A redaction that spans the newline (a "Bearer" in the last word before
// it, whose replacement takes in the whitespace after the scheme) removes
// it; the split then moves to the next space, so the redacted value lands
// in the context. It reports false when no space of rest is left.
func sanitizeSplitAtWord(context, rest string) (string, bool) {
	at := 0
	for range tailSplitAttempts {
		i := strings.IndexByte(rest[at:], ' ')
		if i < 0 {
			return "", false
		}
		at += i
		out := logging.SanitizeText(context + rest[:at] + "\n" + rest[at+1:])
		if strings.IndexByte(out, '\n') >= 0 {
			return out, true
		}
		at++
	}
	return "", false
}

// truncateRunesFromStart cuts s to at most maxRunes runes by dropping its
// start, beginning with "…" when it was cut.
func truncateRunesFromStart(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return "…" + strings.TrimLeft(string(runes[len(runes)-(maxRunes-1):]), " ")
}

// formatHarnessErrorReason renders a harness_error reason as
// "<source>: <text>". The text is untrusted harness output: it is stripped
// of terminal escapes, put on one line, passed through the free-text
// credential redaction log records use (logging.SanitizeText) and the
// whole reason is cut to harnessErrorReasonMaxRunes runes.
func formatHarnessErrorReason(source, text string) string {
	text = sanitizeHarnessText(text)
	reason := source
	if text != "" {
		reason = source + ": " + text
	}
	return truncateRunes(reason, harnessErrorReasonMaxRunes)
}

// sanitizeHarnessText makes untrusted harness error text safe to store or
// export: terminal escapes stripped, one line, and free-text credentials
// redacted (logging.SanitizeText). It does not cut the text.
func sanitizeHarnessText(text string) string {
	// singleLine before SanitizeText is deliberate; redaction survives it (TestFormatHarnessErrorReason).
	text = singleLine(terminalEscapeRe.ReplaceAllString(text, ""))
	return singleLine(logging.SanitizeText(text))
}

// telemetryErrorText returns a hook event's error string as the
// telemetry handler exports it (span status, span and log attributes):
// sanitized like a harness_error reason and cut to
// harnessErrorReasonMaxRunes runes. Harness error strings can carry
// provider messages (claude StopFailure, antigravity Stop.error,
// grok-build errorDetails), which can echo credentials.
func telemetryErrorText(text string) string {
	return truncateRunes(sanitizeHarnessText(text), harnessErrorReasonMaxRunes)
}

// terminalEscapeRe matches terminal escape sequences (CSI such as colours,
// OSC such as titles and links, and two-byte escapes). The hook handlers
// run in sciontool's PID-1 path, which must not reference pkg/util (see
// cmd/sciontool/commands/execaudit_test.go), so util.StripANSI is not used.
var terminalEscapeRe = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)

// singleLine replaces every run of whitespace and control characters in s
// with one space and trims the ends.
func singleLine(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
}

// truncateRunes cuts s to at most maxRunes runes, ending in "…" when it
// was cut.
func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes-1]) + "…"
}
