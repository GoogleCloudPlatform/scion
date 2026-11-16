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

package github

import (
	"sort"
	"strings"
	"unicode"
)

// CommandMatch holds the parsed mention and slash-command result from a body (§5.6).
type CommandMatch struct {
	Mentioned   bool
	HasCommand  bool
	Command     string
	CommandArgs string
	CommandLine string
}

func normalizeHandle(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "@")
	if len(s) > len("[bot]") && strings.EqualFold(s[len(s)-len("[bot]"):], "[bot]") {
		s = s[:len(s)-len("[bot]")]
	}
	return strings.TrimSpace(s)
}

// isCommandCreationAction reports whether (eventName, action) is one of the
// five Stage 3 creation actions eligible for mention/command parsing (§5.6, §16.3).
func isCommandCreationAction(eventName, action string) bool {
	switch eventName {
	case EventIssueComment, EventPullRequestReviewComment:
		return action == "created"
	case EventPullRequestReview:
		return action == "submitted"
	case EventIssues, EventPullRequest:
		return action == "opened"
	default:
		return false
	}
}

// buildHandleRunes returns deduplicated lowercased rune slices for slug + aliases,
// sorted longest-first so longer handles are tested first at a given '@'.
func buildHandleRunes(slug string, aliases []string) [][]rune {
	var list [][]rune
	seen := make(map[string]bool)
	add := func(raw string) {
		h := strings.ToLower(normalizeHandle(raw))
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		list = append(list, []rune(h))
	}
	add(slug)
	for _, a := range aliases {
		add(a)
	}
	sort.SliceStable(list, func(i, j int) bool {
		return len(list[i]) > len(list[j])
	})
	return list
}

// ParseCommand parses an app-mention command out of body using slug and aliases (§5.6).
// Fenced code blocks, inline backtick code spans, and '>' blockquote lines are
// skipped using CommonMark prose rules. The first matching command in reading
// order wins.
func ParseCommand(body, slug string, aliases []string, maxArgsBytes, maxLineBytes int) CommandMatch {
	if maxArgsBytes <= 0 {
		maxArgsBytes = MaxCommandArgsBytes
	}
	if maxLineBytes <= 0 {
		maxLineBytes = MaxCommandLineBytes
	}
	handles := buildHandleRunes(slug, aliases)
	if len(handles) == 0 || body == "" {
		return CommandMatch{}
	}

	text := normalizeNewlines(body)
	lines := strings.Split(text, "\n")
	prose := proseLines(lines)

	var out CommandMatch
	for i, line := range lines {
		if !prose[i] || !strings.ContainsRune(line, '@') {
			continue
		}
		mentioned, cmd, args, hasCmd := scanProseLine(line, handles)
		if mentioned {
			out.Mentioned = true
		}
		if hasCmd && !out.HasCommand {
			out.HasCommand = true
			out.Command = cmd
			out.CommandArgs = truncateUTF8(args, maxArgsBytes)
			out.CommandLine = truncateUTF8(strings.TrimSpace(line), maxLineBytes)
		}
		if out.Mentioned && out.HasCommand {
			break
		}
	}
	return out
}

func scanProseLine(line string, handles [][]rune) (mentioned bool, cmd, args string, hasCmd bool) {
	runes := []rune(line)
	inCode := make([]bool, len(runes))
	for _, span := range inlineCodeSpans(runes) {
		for k := span[0]; k < span[1]; k++ {
			inCode[k] = true
		}
	}

	for i := 0; i < len(runes); i++ {
		if runes[i] != '@' || inCode[i] {
			continue
		}
		leadingUnderscores, ok := mentionStartBoundary(runes, i)
		if !ok {
			continue
		}

		for _, h := range handles {
			end := i + 1 + len(h)
			if end > len(runes) {
				continue
			}
			if !matchHandleAt(runes, i+1, h, inCode) {
				continue
			}
			if !mentionEndBoundary(runes, end, leadingUnderscores, inCode) {
				continue
			}
			mentioned = true

			if !hasCmd {
				if parsedCmd, parsedArgs, cmdOK := parseCommandAfterMention(runes, end, inCode); cmdOK {
					hasCmd = true
					cmd = parsedCmd
					args = parsedArgs
				}
			}
			break
		}
	}
	return mentioned, cmd, args, hasCmd
}

func matchHandleAt(runes []rune, start int, targetLower []rune, inCode []bool) bool {
	for k, want := range targetLower {
		idx := start + k
		if inCode[idx] || unicode.ToLower(runes[idx]) != want {
			return false
		}
	}
	return true
}

func parseCommandAfterMention(runes []rune, mentionEnd int, inCode []bool) (cmd, args string, ok bool) {
	// Require ws+ (at least one horizontal whitespace character outside code) after mention.
	j := mentionEnd
	wsCount := 0
	for j < len(runes) && !inCode[j] && (runes[j] == ' ' || runes[j] == '\t') {
		wsCount++
		j++
	}
	if wsCount == 0 || j >= len(runes) || inCode[j] || runes[j] != '/' {
		return "", "", false
	}

	cmdStart := j + 1
	if cmdStart >= len(runes) || inCode[cmdStart] || !isASCIILetter(runes[cmdStart]) {
		return "", "", false
	}
	cmdEnd := cmdStart + 1
	for cmdEnd < len(runes) && !inCode[cmdEnd] && isCommandBodyRune(runes[cmdEnd]) {
		cmdEnd++
	}
	cmdLen := cmdEnd - cmdStart
	if cmdLen > MaxCommandNameRunes {
		return "", "", false
	}
	// Must end at EOL or whitespace ([ws+ args] EOL) so paths like /usr/bin/env do not match.
	if cmdEnd < len(runes) && !unicode.IsSpace(runes[cmdEnd]) {
		return "", "", false
	}

	cmd = strings.ToLower(string(runes[cmdStart:cmdEnd]))
	if cmdEnd < len(runes) {
		args = strings.TrimSpace(string(runes[cmdEnd:]))
	}
	return cmd, args, true
}

func isASCIILetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isCommandBodyRune(r rune) bool {
	return isASCIILetter(r) || (r >= '0' && r <= '9') || r == '_' || r == '-'
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-'
}

// Mention token boundary helpers below (mentionStartBoundary,
// mentionEndBoundary, mentionTrailingPunctOrSpace) mirror ExtractProseMentions,
// proseMentionEndBoundary, and proseMentionPunctOrSpace in
// pkg/messages/mentions.go so package github stays a stdlib-only leaf. Mirror
// CommonMark / mention-boundary fixes across both files.
func mentionStartBoundary(runes []rune, atIdx int) (leadingUnderscores int, ok bool) {
	if atIdx == 0 {
		return 0, true
	}
	prev := runes[atIdx-1]
	if prev == '_' {
		j := atIdx - 1
		for j >= 0 && runes[j] == '_' {
			j--
		}
		if j >= 0 && (isWordRune(runes[j]) || runes[j] == '@' || runes[j] == '`' || runes[j] == '.' || runes[j] == '+' || runes[j] == '/') {
			return 0, false
		}
		return atIdx - 1 - j, true
	}
	if isWordRune(prev) || prev == '@' || prev == '`' || prev == '.' || prev == '+' || prev == '/' {
		return 0, false
	}
	return 0, true
}

func mentionEndBoundary(runes []rune, end, leadingUnderscores int, inCode []bool) bool {
	if end >= len(runes) {
		return true
	}
	if inCode[end] {
		return false
	}
	next := runes[end]
	if next == '_' {
		if leadingUnderscores <= 0 {
			return false
		}
		closeEnd := end
		for closeEnd < len(runes) && closeEnd-end < leadingUnderscores && runes[closeEnd] == '_' {
			closeEnd++
		}
		if closeEnd >= len(runes) {
			return true
		}
		after := runes[closeEnd]
		if isWordRune(after) || after == '@' {
			return false
		}
		return mentionTrailingPunctOrSpace(runes, closeEnd)
	}
	if isWordRune(next) || next == '@' {
		return false
	}
	return mentionTrailingPunctOrSpace(runes, end)
}

func mentionTrailingPunctOrSpace(runes []rune, idx int) bool {
	next := runes[idx]
	if (next == '.' || next == '+') && idx+1 < len(runes) && isWordRune(runes[idx+1]) {
		return false
	}
	return unicode.IsSpace(next) ||
		next == ',' || next == '.' || next == '!' || next == '?' ||
		next == ')' || next == '(' || next == ':' || next == ';' ||
		next == '"' || next == '\'' || next == ']' || next == '[' ||
		next == '*' || next == '~' || next == '>'
}

// CommonMark prose and inline-code helpers below (proseLines, fenceIndentWidth,
// openingFence, closesFence, byteRunLength, inlineCodeSpans, findBacktickRun)
// are copied from pkg/messages/mentions.go (proseLines, fenceIndentWidth,
// openingFence, closesFence, runLength, inlineCodeSpans, findBacktickRun) to
// preserve this package's stdlib-only leaf invariant. Mirror CommonMark fixes
// between pkg/messages/mentions.go and this file.
func proseLines(lines []string) []bool {
	prose := make([]bool, len(lines))
	var fenceChar byte
	var fenceLen int
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if fenceChar != 0 {
			if fenceIndentWidth(line) <= 3 && closesFence(trimmed, fenceChar, fenceLen) {
				fenceChar = 0
				fenceLen = 0
			}
			continue
		}
		if ch, length, ok := openingFence(trimmed); ok && fenceIndentWidth(line) <= 3 {
			fenceChar = ch
			fenceLen = length
			continue
		}
		if strings.HasPrefix(trimmed, ">") {
			continue
		}
		prose[i] = true
	}
	return prose
}

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

func openingFence(trimmed string) (ch byte, length int, ok bool) {
	if trimmed == "" {
		return 0, 0, false
	}
	c := trimmed[0]
	if c != '`' && c != '~' {
		return 0, 0, false
	}
	n := byteRunLength(trimmed, c)
	if n < 3 {
		return 0, 0, false
	}
	if c == '`' && strings.ContainsRune(trimmed[n:], '`') {
		return 0, 0, false
	}
	return c, n, true
}

func closesFence(trimmed string, fenceChar byte, fenceLen int) bool {
	if trimmed == "" || trimmed[0] != fenceChar {
		return false
	}
	n := byteRunLength(trimmed, fenceChar)
	if n < fenceLen {
		return false
	}
	return strings.TrimSpace(trimmed[n:]) == ""
}

func byteRunLength(s string, c byte) int {
	n := 0
	for n < len(s) && s[n] == c {
		n++
	}
	return n
}

func inlineCodeSpans(runes []rune) [][2]int {
	var spans [][2]int
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
			continue
		}
		spans = append(spans, [2]int{openStart, closeEnd})
		i = closeEnd
	}
	return spans
}

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
