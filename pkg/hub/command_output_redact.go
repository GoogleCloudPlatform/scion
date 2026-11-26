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

package hub

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// commandOutputShareMaxBytes is the maximum byte length of the redacted
	// output_tail shared back to the asking agent (design §4.9: 4 KiB).
	commandOutputShareMaxBytes = 4096

	// commandOutputRedactedMarker replaces matched secret patterns.
	commandOutputRedactedMarker = "[REDACTED]"
)

type commandOutputRedactRule struct {
	re *regexp.Regexp
	// replace transforms a match (with submatch indices from FindAllStringSubmatchIndex)
	// and reports whether a redaction occurred.
	replace func(src string, loc []int) (string, bool)
}

var commandOutputRedactRules = []commandOutputRedactRule{
	// 1. Complete PEM private key blocks (RSA, EC, OPENSSH, PKCS8, etc.).
	{
		re: regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
		replace: func(_ string, _ []int) (string, bool) {
			return commandOutputRedactedMarker, true
		},
	},
	// 2. Unterminated/truncated PEM private key blocks (e.g. cut off at capture boundary).
	{
		re: regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*$`),
		replace: func(_ string, _ []int) (string, bool) {
			return commandOutputRedactedMarker, true
		},
	},
	// 3. Bearer authorization tokens (runs before JWT so "Bearer eyJ..." counts once).
	{
		re: regexp.MustCompile(`(?i)\bBearer[ \t]+[A-Za-z0-9._~+/=-]{8,}`),
		replace: func(_ string, _ []int) (string, bool) {
			return "Bearer " + commandOutputRedactedMarker, true
		},
	},
	// 4. GitHub tokens (ghp_, gho_, ghs_, github_pat_).
	{
		re: regexp.MustCompile(`\b(?:gh[pos]_[A-Za-z0-9_]{6,}|github_pat_[A-Za-z0-9_]{6,})\b`),
		replace: func(_ string, _ []int) (string, bool) {
			return commandOutputRedactedMarker, true
		},
	},
	// 5. AWS access key IDs (AKIA..., ASIA...).
	{
		re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{12,}\b`),
		replace: func(_ string, _ []int) (string, bool) {
			return commandOutputRedactedMarker, true
		},
	},
	// 6. Google OAuth2 access tokens (ya29....).
	{
		re: regexp.MustCompile(`\bya29\.[A-Za-z0-9._-]{10,}`),
		replace: func(_ string, _ []int) (string, bool) {
			return commandOutputRedactedMarker, true
		},
	},
	// 7. Slack tokens (xoxa-, xoxb-, xoxp-, xoxr-).
	{
		re: regexp.MustCompile(`\bxox[abpr]-[A-Za-z0-9-]{8,}`),
		replace: func(_ string, _ []int) (string, bool) {
			return commandOutputRedactedMarker, true
		},
	},
	// 8. JWT-shaped tokens (eyJ... . ... . ...).
	{
		re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\b`),
		replace: func(_ string, _ []int) (string, bool) {
			return commandOutputRedactedMarker, true
		},
	},
	// 9. password|passwd|secret|token key-value assignments (env, shell, YAML, JSON).
	{
		re: regexp.MustCompile(`(?i)(\b[A-Za-z0-9_.-]*(?:password|passwd|secret|token)[A-Za-z0-9_.-]*"?[ \t]*[:=][ \t]*)("[^"\r\n]+"|'[^'\r\n]+'|[^\s"'` + "`" + `,;\}]+)`),
		replace: func(src string, loc []int) (string, bool) {
			prefix := src[loc[2]:loc[3]]
			val := src[loc[4]:loc[5]]
			trimmed := strings.Trim(val, `"'`)
			if trimmed == "" || trimmed == commandOutputRedactedMarker {
				return src[loc[0]:loc[1]], false
			}
			return prefix + commandOutputRedactedMarker, true
		},
	},
}

// redactCommandOutput applies best-effort secret redaction per design §4.9.
// Redaction is defense in depth; the approver's explicit share_output opt-in
// is the primary access control (D3).
func redactCommandOutput(input string) (redacted string, count int) {
	s := strings.ToValidUTF8(input, "\uFFFD")
	for _, rule := range commandOutputRedactRules {
		locs := rule.re.FindAllStringSubmatchIndex(s, -1)
		if len(locs) == 0 {
			continue
		}
		var b strings.Builder
		b.Grow(len(s))
		last := 0
		for _, loc := range locs {
			b.WriteString(s[last:loc[0]])
			rep, changed := rule.replace(s, loc)
			b.WriteString(rep)
			if changed {
				count++
			}
			last = loc[1]
		}
		b.WriteString(s[last:])
		s = b.String()
	}
	return s, count
}

// boundUTF8Tail returns the last maxBytes bytes of s, advancing past any
// leading UTF-8 continuation bytes so the result is always valid UTF-8 and
// at most maxBytes bytes long.
func boundUTF8Tail(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// redactAndBoundCommandOutput applies best-effort secret redaction to raw and
// then returns the last 4 KiB cut at a UTF-8 rune boundary (design §4.9).
// Redacting before slicing ensures secrets straddling the 4 KiB boundary are
// never partially exposed.
func redactAndBoundCommandOutput(raw []byte) (tail string, redactions int) {
	if len(raw) == 0 {
		return "", 0
	}
	redacted, redactions := redactCommandOutput(string(raw))
	return boundUTF8Tail(redacted, commandOutputShareMaxBytes), redactions
}
