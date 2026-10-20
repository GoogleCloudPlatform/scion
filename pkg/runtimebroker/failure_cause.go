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

package runtimebroker

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// maxLoggedCauseBytes bounds a sanitized failure cause in a log record.
const maxLoggedCauseBytes = 1024

// redactedMarker replaces every value sanitizeFailureCause removes.
const redactedMarker = "[REDACTED]"

// causeCredentialWords matches the words that make a key credential-named
// wherever they appear in it (case-insensitive patterns only).
const causeCredentialWords = `(?:TOKEN|SECRET|PASSWORD|PASSWD|PASSPHRASE|API[_-]?KEY|PRIVATE[_-]?KEY|ACCESS[_-]?KEY)`

// causeValuePattern matches a value after a key and its separator: a
// quoted string, or a run up to whitespace or a delimiter (blankKeyValues
// extends an unquoted value over the words after it).
const causeValuePattern = `("[^"]*"|'[^']*'|[^\s,;&"'}\]]+)`

// causeValueDelimiters end an unquoted value.
const causeValueDelimiters = ",;&\"'}]"

var (
	// A PEM block, or an unterminated one running to the end of the text.
	causePEMRe = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]+-----.*?(?:-----END [A-Z0-9 ]+-----|$)`)
	// The userinfo part of a URL: scheme://user:pass@host.
	causeURLUserInfoRe = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9+.-]*://)[^\s/?#@]+@`)
	// A bearer authorization scheme followed by its credentials.
	causeBearerRe = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=-]{8,}`)
	// A basic authorization scheme and the word after it; blankBasic
	// decides whether that word is credentials.
	causeBasicRe = regexp.MustCompile(`(?i)(authorization:\s*)?\b(basic)\s+([A-Za-z0-9._~+/=-]{4,})`)
	// A JWT-shaped string: three base64url segments, the first a JSON object.
	causeJWTRe = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`)
	// A bare credential-named key, in any case and with '_' or '-' between
	// words (or any SCION_ variable), followed by ':' or '=' and its value.
	// "auth" and "credential(s)" count only as a whole word of the key, so
	// prose such as "unauthorized: authentication required" is kept.
	causeBareCredentialKeyRe = regexp.MustCompile(`(?i)\b(SCION_[A-Z0-9_]*|[A-Z0-9_-]*` + causeCredentialWords + `[A-Z0-9_-]*|(?:[A-Z0-9]+[_-])*(?:AUTH|CREDENTIALS?)(?:[_-][A-Z0-9]+)*)(\s*[:=]\s*)` + causeValuePattern)
	// A quoted credential-named key, in double quotes as in JSON or single
	// quotes as in YAML or Python output, followed by ':' and its value.
	causeQuotedCredentialKeyRe = regexp.MustCompile(`(?i)("[A-Z0-9_.-]*(?:` + causeCredentialWords + `|AUTH|CREDENTIAL)[A-Z0-9_.-]*"|'[A-Z0-9_.-]*(?:` + causeCredentialWords + `|AUTH|CREDENTIAL)[A-Z0-9_.-]*')(\s*:\s*)` + causeValuePattern)
	// A "value" key, as in a Kubernetes EnvVar in JSON
	// ({"name":"X","value":"v"}) or printed with %+v ({Name:X Value:v}).
	causeValueKeyRe = regexp.MustCompile(`(?i)("value"|\bvalue)(\s*:\s*)` + causeValuePattern)
	// Any KEY=VALUE pair (env entries, flags, query parameters).
	causeKeyValueRe = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_.-]*)(=)` + causeValuePattern)
	// A long run of base64 or hex characters.
	causeLongRunRe = regexp.MustCompile(`[A-Za-z0-9+/_=-]{32,}`)
)

// sanitizeFailureCause returns text, a runtime failure's backend error text,
// in the form a broker log record may carry. The text is untrusted: a
// runtime's output can repeat parts of the agent's start spec. So every
// KEY=VALUE value is blanked (the key stays), as are the values of
// credential-named keys and "value" keys, authorization credentials,
// JWT-shaped strings, URL userinfo, PEM blocks and long base64 or hex runs.
// The result is then truncated to maxLoggedCauseBytes, with a marker giving
// the sanitized length.
//
// The records of the create and start failures whose client response is
// the fixed "Failed to <op>" text (synchronous create, async launch, create
// preflight, start, restart, and start context preparation) pass their
// cause and error fields through this. The text never reaches a client
// response.
func sanitizeFailureCause(text string) string {
	s := causePEMRe.ReplaceAllString(text, redactedMarker)
	s = causeURLUserInfoRe.ReplaceAllString(s, "${1}"+redactedMarker+"@")
	s = causeBearerRe.ReplaceAllString(s, "${1} "+redactedMarker)
	s = causeBasicRe.ReplaceAllStringFunc(s, blankBasic)
	s = causeJWTRe.ReplaceAllString(s, redactedMarker)
	s = blankKeyValues(causeBareCredentialKeyRe, s)
	s = blankKeyValues(causeQuotedCredentialKeyRe, s)
	s = blankKeyValues(causeValueKeyRe, s)
	s = blankKeyValues(causeKeyValueRe, s)
	s = causeLongRunRe.ReplaceAllStringFunc(s, func(m string) string {
		if looksEncoded(m) {
			return redactedMarker
		}
		return m
	})
	return truncateFailureCause(s)
}

// blankBasic blanks the credentials of a causeBasicRe match: always after
// "Authorization:", and otherwise only when the word after "basic" looks
// like encoded credentials (it holds a digit, '=', '+', '/' or '.'), so
// prose such as "basic validation failed" is kept.
func blankBasic(m string) string {
	g := causeBasicRe.FindStringSubmatch(m)
	if g[1] == "" && !strings.ContainsAny(g[3], "0123456789=+/.") {
		return m
	}
	return g[1] + g[2] + " " + redactedMarker
}

// blankKeyValues replaces the value of every match of re in s, whose
// groups are key, separator and value, with redactedMarker, keeping the key
// and separator. An unquoted value also takes in the space-separated words
// after it on the same line (extendValue), so a value with spaces is
// blanked whole. A value an earlier rule already replaced is left as is.
func blankKeyValues(re *regexp.Regexp, s string) string {
	var b strings.Builder
	last := 0
	for _, m := range re.FindAllStringSubmatchIndex(s, -1) {
		if m[0] < last {
			continue // inside a value already blanked
		}
		value := s[m[6]:m[7]]
		if strings.HasPrefix(value, "[REDACTED") {
			continue
		}
		end := m[1]
		if value[0] != '"' && value[0] != '\'' {
			end = extendValue(s, end)
		}
		b.WriteString(s[last:m[6]])
		b.WriteString(redactedMarker)
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

// extendValue returns the end of an unquoted value that ends at i in s,
// extended over each following word that is separated from it by spaces or
// tabs only, ends at whitespace, a delimiter or the end of s, and is not a
// key: it holds no '=' or ':', and the next non-whitespace character after
// it (on any line) is not one either. A redactedMarker counts as one word.
func extendValue(s string, i int) int {
	for {
		j := i
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		if j == i {
			return i
		}
		k := j
		if strings.HasPrefix(s[j:], redactedMarker) {
			k = j + len(redactedMarker)
		} else {
			for k < len(s) && !strings.ContainsRune(" \t\r\n"+causeValueDelimiters, rune(s[k])) {
				k++
			}
		}
		if k == j || strings.ContainsAny(s[j:k], "=:") {
			return i
		}
		n := k
		for n < len(s) && strings.ContainsRune(" \t\r\n", rune(s[n])) {
			n++
		}
		if n < len(s) && (s[n] == '=' || s[n] == ':') {
			return i
		}
		i = k
	}
}

// looksEncoded reports whether run, a long run of base64/hex characters,
// looks like encoded data rather than a name: all hex, or a mix of upper
// case, lower case and digits. Kubernetes object and container names are
// lower case, so they are kept.
func looksEncoded(run string) bool {
	if strings.Contains(run, redactedMarker) {
		return false
	}
	allHex := true
	var upper, lower, digit bool
	for _, r := range run {
		switch {
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= '0' && r <= '9':
			digit = true
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			allHex = false
		}
	}
	return allHex || (upper && lower && digit)
}

// truncateFailureCause cuts s to at most maxLoggedCauseBytes on a rune
// boundary and appends a marker with s's full length.
func truncateFailureCause(s string) string {
	if len(s) <= maxLoggedCauseBytes {
		return s
	}
	cut := maxLoggedCauseBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("...[truncated, %d bytes]", len(s))
}
