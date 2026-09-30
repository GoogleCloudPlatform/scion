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

package messages

import (
	"strings"
	"unicode"
)

// MaxMentionRecipients caps the number of mention recipients to avoid spam.
const MaxMentionRecipients = 10

// DedupMentionNames unions any number of mention-name lists, deduplicating
// case-insensitively while preserving first-seen order (earlier lists win
// the earlier position — callers that want explicit/caller-supplied names to
// take precedence over body-extracted ones should pass them first). Shared
// by the hub's fan-out (union of explicit and body mentions) and the CLI
// (union of --cc and body mentions), which independently needed the exact
// same logic.
func DedupMentionNames(lists ...[]string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, list := range lists {
		for _, name := range list {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			lower := strings.ToLower(name)
			if seen[lower] {
				continue
			}
			seen[lower] = true
			out = append(out, name)
		}
	}
	return out
}

// MentionResult represents the outcome of a single mention fan-out attempt.
//
// Status is one of: "delivered", "not_found", "error" (the original set), or
// — for agent-authored mention fan-out — "suppressed" (blocked by the
// per-pair loop-protection cap), "unauthorized" (the recipient denied the
// sender a DM), "rate_limited" (the sender's aggregate send budget was
// exhausted mid-fan-out), "ambiguous" (dispatch may or may not have
// succeeded, mirroring AgentDMAmbiguous), or "timeout" (the aggregate
// fan-out deadline elapsed). "deduplicated" is a *response* status for an
// old CLI's redundant follow-up mention POST, not a MentionResult status.
type MentionResult struct {
	Slug   string `json:"slug"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"` // human-readable reason on failure

	// AgentPhase is the mentioned agent's lifecycle phase at delivery time
	// (e.g. "running", "stopped"), set when Status is "delivered". Additive
	// field: old clients that decode this struct simply ignore it.
	AgentPhase string `json:"agent_phase,omitempty"`
}

// ExtractMentions scans message text for @name tokens and returns a deduplicated
// list of mentioned names (without the @ prefix). Trailing punctuation (except
// underscores and hyphens) is stripped from each token.
//
// An @ preceded by a non-whitespace character (e.g. inside an email address like
// user@example.com) is not treated as a mention trigger.
func ExtractMentions(text string) []string {
	var mentions []string
	seen := make(map[string]bool)
	for _, word := range strings.Fields(text) {
		if !strings.HasPrefix(word, "@") {
			continue
		}
		name := strings.TrimPrefix(word, "@")
		name = strings.TrimRightFunc(name, func(r rune) bool {
			return unicode.IsPunct(r) && r != '_' && r != '-'
		})
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if !seen[lower] {
			seen[lower] = true
			mentions = append(mentions, name)
		}
	}
	return mentions
}

// ExtractProseMentions is like ExtractMentions but, for agent-authored
// bodies, ignores @-tokens that appear inside fenced code blocks (three or
// more backticks or tildes, from the opening fence through the matching
// closing fence or EOF), inline backtick-delimited code spans, and lines
// whose first non-space character is '>' (quoted/blockquote lines). Agents
// paste logs, diffs, and quoted transcripts far more often than humans do,
// and an @-token inside one of those is not a deliberate address.
//
// Human chat continues to use ExtractMentions unchanged — this function is
// only used for agent-authored sends.
func ExtractProseMentions(text string) []string {
	return ExtractMentions(stripNonProseSpans(text))
}

// stripNonProseSpans blanks fenced code blocks, inline code spans, and
// blockquote lines with spaces, preserving every other character (including
// newlines) so that ExtractMentions' word-boundary scanning is unaffected
// outside the stripped spans.
func stripNonProseSpans(text string) string {
	lines := strings.Split(text, "\n")
	var fenceChar byte // '`' or '~' while inside a fence, 0 otherwise.
	var fenceLen int   // length of the run that opened the current fence.
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if fenceChar != 0 {
			lines[i] = blankString(line)
			// The same ≤3-column indentation rule that gates an opener
			// (below) also gates a closer: a candidate closing run indented
			// 4 or more columns is an indented code block, i.e. still
			// fence content, not a close.
			if fenceIndentWidth(line) <= 3 && closesFence(trimmed, fenceChar, fenceLen) {
				fenceChar = 0
				fenceLen = 0
			}
			continue
		}
		// CommonMark allows at most 3 columns of leading indentation before
		// a fence opener; 4 or more makes it an indented code block instead,
		// which this function does not otherwise special-case, so such a
		// line is left as ordinary prose rather than starting a fence.
		if ch, length, ok := openingFence(trimmed); ok && fenceIndentWidth(line) <= 3 {
			fenceChar = ch
			fenceLen = length
			lines[i] = blankString(line)
			continue
		}
		if strings.HasPrefix(trimmed, ">") {
			lines[i] = blankString(line)
			continue
		}
		lines[i] = stripInlineBackticks(line)
	}
	return strings.Join(lines, "\n")
}

// fenceIndentWidth returns line's leading indentation width in columns, the
// quantity CommonMark actually gates fence openers and closers on: a space
// is one column, and a tab advances to the next multiple of 4 (CommonMark's
// tab-stop rule), not one column per tab character. Indentation of 4 or more
// columns makes a line an indented code block instead of a fence boundary.
func fenceIndentWidth(line string) int {
	width := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case ' ':
			width++
		case '\t':
			width += 4 - (width % 4)
		default:
			return width
		}
	}
	return width
}

// openingFence reports whether trimmed opens a fenced code block: a run of
// 3 or more backticks or tildes at the start of the line. An info string
// (e.g. "go" in "```go") may follow on the opening line — CommonMark allows
// that on the opener but not the closer, which is exactly what distinguishes
// them (see closesFence).
func openingFence(trimmed string) (ch byte, length int, ok bool) {
	if trimmed == "" {
		return 0, 0, false
	}
	c := trimmed[0]
	if c != '`' && c != '~' {
		return 0, 0, false
	}
	n := runLength(trimmed, c)
	if n < 3 {
		return 0, 0, false
	}
	// A backtick fence's info string may not itself contain a backtick per
	// CommonMark — that is precisely what lets the parser tell an opening
	// fence apart from an inline code span starting on the same line. A
	// tilde fence has no such restriction, since tildes and backticks don't
	// collide.
	if c == '`' && strings.ContainsRune(trimmed[n:], '`') {
		return 0, 0, false
	}
	return c, n, true
}

// closesFence reports whether trimmed validly closes a fence that was opened
// by a run of fenceLen fenceChar runes: nothing but a run of fenceChar at
// least fenceLen long, optionally followed by trailing whitespace, and
// nothing else. Two things this rules out, both real bugs a naive
// strings.HasPrefix match has: (1) a shorter run than the opener (a run of
// two backticks cannot close a block opened by a run of three, and a run of
// three cannot close one opened by a run of four); (2) a line with an info
// string or any other trailing content — a fenced code block containing a
// nested, language-tagged fence line as literal text is code, not a closing
// fence, because a closing fence carries no info string.
func closesFence(trimmed string, fenceChar byte, fenceLen int) bool {
	if trimmed == "" {
		return false
	}
	if trimmed[0] != fenceChar {
		return false
	}
	n := runLength(trimmed, fenceChar)
	if n < fenceLen {
		return false
	}
	return strings.TrimSpace(trimmed[n:]) == ""
}

// runLength returns the length of the leading run of byte c in s.
func runLength(s string, c byte) int {
	n := 0
	for n < len(s) && s[n] == c {
		n++
	}
	return n
}

// blankString replaces every byte of s with a space, so line length (and
// therefore any surrounding structure) is preserved while removing content.
func blankString(s string) string {
	return strings.Repeat(" ", len(s))
}

// stripInlineBackticks blanks backtick-delimited code spans within a single
// line, honouring run length the way CommonMark inline code spans do: a
// span opened by a run of N backticks is closed only by the next run of
// exactly N backticks, so a span opened and closed with a run of two
// backticks (used to wrap a literal single backtick inside the span) is not
// mis-closed by that inner single backtick. A backtick run with no matching
// closing run on the same line is not a code span at all — per CommonMark it
// is left as literal text, so an @-mention elsewhere on that line (before or
// after the stray backtick run) is still extracted. This function does not
// look across lines for a closing run.
//
// Known deviation: a blanked span is replaced with spaces (blankRunes), so a
// mention with no whitespace of its own before an adjacent span — e.g.
// "`x`@bar" — is still extracted, identically to how it would be if a space
// had been there instead of the span. This over-extracts relative to a
// strict CommonMark word-boundary reading in that one specific adjacency; it
// may occasionally treat an adjacent @-token as a mention that a stricter
// reading would suppress. It is consistent with the rest of this file's
// whitespace-preserving blanking strategy (used for fences and blockquotes
// too, for the same reason: it is simpler and safer than tracking a separate
// placeholder rune that must itself not introduce or remove a boundary).
func stripInlineBackticks(line string) string {
	runes := []rune(line)
	out := make([]rune, len(runes))
	copy(out, runes)

	i := 0
	for i < len(runes) {
		if runes[i] != '`' {
			i++
			continue
		}
		openStart := i
		openLen := 0
		for i < len(runes) && runes[i] == '`' {
			openLen++
			i++
		}

		closeStart, closeEnd := findBacktickRun(runes, i, openLen)
		if closeStart == -1 {
			// No matching closing run on this line: per CommonMark, an
			// unmatched backtick run is not a code span delimiter at all —
			// it is literal text. Leave it (and everything after it)
			// untouched and keep scanning from where the run ended, rather
			// than treating the rest of the line as code.
			continue
		}
		blankRunes(out, openStart, closeEnd)
		i = closeEnd
	}
	return string(out)
}

// findBacktickRun scans runes starting at from for the next run of
// backticks whose length equals want, returning its [start, end) bounds, or
// (-1, -1) if none exists. Runs of a different length are skipped over (they
// are literal backticks inside the span, per CommonMark), not treated as a
// close.
func findBacktickRun(runes []rune, from, want int) (start, end int) {
	i := from
	for i < len(runes) {
		if runes[i] != '`' {
			i++
			continue
		}
		runStart := i
		runLen := 0
		for i < len(runes) && runes[i] == '`' {
			runLen++
			i++
		}
		if runLen == want {
			return runStart, i
		}
	}
	return -1, -1
}

// blankRunes overwrites out[start:end] with spaces.
func blankRunes(out []rune, start, end int) {
	for i := start; i < end; i++ {
		out[i] = ' '
	}
}

// IsLeadingMention reports whether text begins with an @-mention token
// whose extracted name matches firstMentionName (case-insensitive).
// The extraction uses the same rules as ExtractMentions: TrimPrefix "@",
// then TrimRightFunc for trailing punctuation (except _ and -).
//
// This is used to detect the "leading @-mention override" pattern where
// a message starting with @agent-name signals explicit address override
// rather than additive mention routing.
func IsLeadingMention(text, firstMentionName string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "@") {
		return false
	}
	name := strings.TrimPrefix(fields[0], "@")
	name = strings.TrimRightFunc(name, func(r rune) bool {
		return unicode.IsPunct(r) && r != '_' && r != '-'
	})
	return name != "" && strings.EqualFold(name, firstMentionName)
}

// ParseCCFlag parses a --cc flag value into a slice of agent names.
// Names are comma-separated and whitespace-trimmed.
func ParseCCFlag(cc string) []string {
	if cc == "" {
		return nil
	}
	parts := strings.Split(cc, ",")
	var names []string
	seen := make(map[string]bool)
	for _, p := range parts {
		name := strings.TrimSpace(p)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if !seen[lower] {
			seen[lower] = true
			names = append(names, name)
		}
	}
	return names
}

// ParseCCFlags parses a repeatable --cc flag into a slice of agent names.
// Each occurrence may itself be a comma-separated list, so the historical
// comma-separated form and the repeatable form are both accepted and may be
// mixed. Names are whitespace-trimmed and de-duplicated case-insensitively
// across all occurrences, preserving first-seen order.
func ParseCCFlags(cc []string) []string {
	if len(cc) == 0 {
		return nil
	}
	return ParseCCFlag(strings.Join(cc, ","))
}

// AgentInfo holds the minimal agent data needed for mention resolution.
type AgentInfo struct {
	Slug string
	Name string
}

// ResolveMentions validates mention slugs against a set of known agents,
// deduplicates, excludes the primary recipient, and caps at MaxMentionRecipients.
// It returns a MentionResult for each input slug.
func ResolveMentions(mentionNames []string, knownAgents []AgentInfo, primaryRecipientSlug string) []MentionResult {
	if len(mentionNames) == 0 {
		return nil
	}

	// Build lookup map: lowercase slug -> original slug
	lookup := make(map[string]string, len(knownAgents))
	for _, a := range knownAgents {
		lookup[strings.ToLower(a.Slug)] = a.Slug
	}

	primaryLower := strings.ToLower(strings.TrimPrefix(primaryRecipientSlug, "agent:"))

	var results []MentionResult
	seen := make(map[string]bool)
	seen[primaryLower] = true // skip primary recipient
	deliveredCount := 0

	for _, name := range mentionNames {
		lower := strings.ToLower(name)
		if seen[lower] {
			continue
		}
		seen[lower] = true

		slug, ok := lookup[lower]
		if !ok {
			results = append(results, MentionResult{
				Slug:   name,
				Status: "not_found",
				Error:  "no matching agent in this project",
			})
			continue
		}

		if deliveredCount >= MaxMentionRecipients {
			results = append(results, MentionResult{
				Slug:   slug,
				Status: "error",
				Error:  "mention recipient cap reached",
			})
			continue
		}

		results = append(results, MentionResult{
			Slug:   slug,
			Status: "delivered",
		})
		deliveredCount++
	}

	return results
}

// DeliveredSlugs returns only the slugs from results that were successfully delivered.
func DeliveredSlugs(results []MentionResult) []string {
	var slugs []string
	for _, r := range results {
		if r.Status == "delivered" {
			slugs = append(slugs, r.Slug)
		}
	}
	return slugs
}
