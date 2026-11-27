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

import "strings"

// MarkdownCommandRequestKey returns the deterministic version-1 key for a
// Markdown Execute command request (§4.14 step 5): "md-<message_id>-<sha256[:16]>".
func MarkdownCommandRequestKey(messageID, commandSHA256 string) string {
	prefix := commandSHA256
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	return "md-" + strings.ToLower(strings.TrimSpace(messageID)) + "-" + strings.ToLower(prefix)
}

// IsEligibleShellFenceInfoString reports whether a fenced code block's info
// string authorizes Markdown Execute (§4.14): first whitespace-delimited token
// is bash, sh, shell, or console (case-insensitive), and no token is
// "no-execute" (case-insensitive).
func IsEligibleShellFenceInfoString(info string) bool {
	fields := strings.Fields(info)
	if len(fields) == 0 {
		return false
	}
	switch strings.ToLower(fields[0]) {
	case "bash", "sh", "shell", "console":
	default:
		return false
	}
	for _, tok := range fields[1:] {
		if strings.EqualFold(tok, "no-execute") {
			return false
		}
	}
	return true
}

// HasVerbatimExecutableShellFence reports whether script appears verbatim in
// body as a complete top-level fenced shell block (§4.14 step 3):
//   - line-start (column 0, no indentation or blockquote prefix) opening fence
//     of >= 3 backticks or >= 3 tildes;
//   - eligible info string (first word bash|sh|shell|console, no no-execute);
//   - body bytes (CRLF normalized to LF) equal to script (LF-normalized; a
//     caller omitting the single terminating newline before the closing fence
//     also matches);
//   - closing fence of the same character and at least the same length at
//     column 0 (followed only by optional spaces/tabs).
//
// Non-eligible top-level fences (e.g. ```markdown, ```python, ```sh no-execute)
// are still scanned to their matching closing fence so a shell fence nested
// inside another fenced block cannot be mistaken for a top-level block.
func HasVerbatimExecutableShellFence(body, script string) bool {
	if script == "" || strings.ContainsRune(script, '\r') {
		return false
	}
	normBody := strings.ReplaceAll(body, "\r\n", "\n")
	lines := strings.Split(normBody, "\n")

	inFence := false
	var fenceChar byte
	fenceLen := 0
	fenceEligible := false
	var blockLines []string

	for _, line := range lines {
		if !inFence {
			ch, n, info, ok := parseTopLevelOpeningFence(line)
			if !ok {
				continue
			}
			inFence = true
			fenceChar = ch
			fenceLen = n
			fenceEligible = IsEligibleShellFenceInfoString(info)
			blockLines = blockLines[:0]
			continue
		}

		if isTopLevelClosingFence(line, fenceChar, fenceLen) {
			if fenceEligible && len(blockLines) > 0 {
				// Each line inside a fenced block is terminated by '\n'.
				// Strip at most one trailing '\n' from both sides so a fence
				// with a trailing blank line matches consistently (O4) while
				// any other byte difference still fails closed.
				blockContent := strings.Join(blockLines, "\n") + "\n"
				if strings.TrimSuffix(blockContent, "\n") == strings.TrimSuffix(script, "\n") {
					return true
				}
			}
			inFence = false
			fenceChar = 0
			fenceLen = 0
			fenceEligible = false
			blockLines = blockLines[:0]
			continue
		}

		if fenceEligible {
			blockLines = append(blockLines, line)
		}
	}

	// Unclosed fence at EOF is not a complete fenced block.
	return false
}

func parseTopLevelOpeningFence(line string) (fenceChar byte, fenceLen int, info string, ok bool) {
	if len(line) < 3 {
		return 0, 0, "", false
	}
	ch := line[0]
	if ch != '`' && ch != '~' {
		return 0, 0, "", false
	}
	n := 0
	for n < len(line) && line[n] == ch {
		n++
	}
	if n < 3 {
		return 0, 0, "", false
	}
	rest := line[n:]
	// CommonMark §4.5: backtick fence info strings cannot contain backticks.
	if ch == '`' && strings.ContainsRune(rest, '`') {
		return 0, 0, "", false
	}
	return ch, n, rest, true
}

func isTopLevelClosingFence(line string, fenceChar byte, minLen int) bool {
	// CommonMark §4.5: a closing code fence may be indented 0..3 spaces.
	// Matching CommonMark here prevents a 1..3-space closer from being
	// swallowed into the block body (O2).
	i := 0
	for i < len(line) && i < 3 && line[i] == ' ' {
		i++
	}
	if len(line)-i < minLen || line[i] != fenceChar {
		return false
	}
	n := 0
	for i < len(line) && line[i] == fenceChar {
		i++
		n++
	}
	if n < minLen {
		return false
	}
	for ; i < len(line); i++ {
		if line[i] != ' ' && line[i] != '\t' {
			return false
		}
	}
	return true
}
