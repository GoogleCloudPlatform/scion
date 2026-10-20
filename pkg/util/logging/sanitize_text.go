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

package logging

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxSanitizedTextBytes bounds the text SanitizeText returns.
const MaxSanitizedTextBytes = 1024

// RedactedMarker replaces every value SanitizeText removes.
const RedactedMarker = "[REDACTED]"

// textCredentialWords matches the words that make a key credential-named
// wherever they appear in it (case-insensitive patterns only).
const textCredentialWords = `(?:TOKEN|SECRET|PASSWORD|PASSWD|PASSPHRASE|API[_-]?KEY|PRIVATE[_-]?KEY|ACCESS[_-]?KEY)`

// textValuePattern matches a value after a key and its separator: a
// quoted string, or a run up to whitespace or a delimiter (blankKeyValues
// extends an unquoted value over the words after it).
const textValuePattern = `("[^"]*"|'[^']*'|[^\s,;&"'}\]]+)`

// textValueDelimiters end an unquoted value.
const textValueDelimiters = ",;&\"'}]"

var (
	// A PEM block, or an unterminated one running to the end of the text.
	textPEMRe = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]+-----.*?(?:-----END [A-Z0-9 ]+-----|$)`)
	// The userinfo part of a URL: scheme://user:pass@host.
	textURLUserInfoRe = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9+.-]*://)[^\s/?#@]+@`)
	// A bearer authorization scheme followed by its credentials.
	textBearerRe = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=-]{8,}`)
	// A basic authorization scheme and the word after it; blankBasic
	// decides whether that word is credentials.
	textBasicRe = regexp.MustCompile(`(?i)(authorization:\s*)?\b(basic)\s+([A-Za-z0-9._~+/=-]{4,})`)
	// A JWT-shaped string: three base64url segments, the first a JSON object.
	textJWTRe = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`)
	// A bare credential-named key, in any case and with '_' or '-' between
	// words (or any SCION_ variable), followed by ':' or '=' and its value.
	// "auth" and "credential(s)" count only as a whole word of the key, so
	// prose such as "unauthorized: authentication required" is kept.
	textBareCredentialKeyRe = regexp.MustCompile(`(?i)\b(SCION_[A-Z0-9_]*|[A-Z0-9_-]*` + textCredentialWords + `[A-Z0-9_-]*|(?:[A-Z0-9]+[_-])*(?:AUTH|CREDENTIALS?)(?:[_-][A-Z0-9]+)*)(\s*[:=]\s*)` + textValuePattern)
	// A quoted credential-named key, in double quotes as in JSON or single
	// quotes as in YAML or Python output, followed by ':' and its value.
	textQuotedCredentialKeyRe = regexp.MustCompile(`(?i)("[A-Z0-9_.-]*(?:` + textCredentialWords + `|AUTH|CREDENTIAL)[A-Z0-9_.-]*"|'[A-Z0-9_.-]*(?:` + textCredentialWords + `|AUTH|CREDENTIAL)[A-Z0-9_.-]*')(\s*:\s*)` + textValuePattern)
	// A "value" key, as in a Kubernetes EnvVar in JSON
	// ({"name":"X","value":"v"}) or printed with %+v ({Name:X Value:v}).
	textValueKeyRe = regexp.MustCompile(`(?i)("value"|\bvalue)(\s*:\s*)` + textValuePattern)
	// Any KEY=VALUE pair (env entries, flags, query parameters).
	textKeyValueRe = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_.-]*)(=)` + textValuePattern)
	// A long run of base64 or hex characters.
	textLongRunRe = regexp.MustCompile(`[A-Za-z0-9+/_=-]{32,}`)
)

// SanitizeText returns text, untrusted error text such as a runtime
// failure's backend output, in the form a log record may carry. Such text
// can repeat parts of a request or start spec. So every
// KEY=VALUE value is blanked (the key stays), as are the values of
// credential-named keys and "value" keys, authorization credentials,
// JWT-shaped strings, URL userinfo, PEM blocks and long base64 or hex runs.
// The result is then truncated to MaxSanitizedTextBytes, with a marker giving
// the sanitized length.
//
// Every log sink renders error-valued attributes through this (see
// sanitizedErrorText), and the runtime broker passes its create and start
// failure causes through it.
func SanitizeText(text string) string {
	s := textPEMRe.ReplaceAllString(text, RedactedMarker)
	s = textURLUserInfoRe.ReplaceAllString(s, "${1}"+RedactedMarker+"@")
	s = textBearerRe.ReplaceAllString(s, "${1} "+RedactedMarker)
	s = textBasicRe.ReplaceAllStringFunc(s, blankBasic)
	s = textJWTRe.ReplaceAllString(s, RedactedMarker)
	s = blankKeyValues(textBareCredentialKeyRe, s)
	s = blankKeyValues(textQuotedCredentialKeyRe, s)
	s = blankKeyValues(textValueKeyRe, s)
	s = blankKeyValues(textKeyValueRe, s)
	s = textLongRunRe.ReplaceAllStringFunc(s, func(m string) string {
		if looksEncoded(m) {
			return RedactedMarker
		}
		return m
	})
	return truncateSanitizedText(s)
}

// blankBasic blanks the credentials of a textBasicRe match: always after
// "Authorization:", and otherwise only when the word after "basic" looks
// like encoded credentials (it holds a digit, '=', '+', '/' or '.'), so
// prose such as "basic validation failed" is kept.
func blankBasic(m string) string {
	g := textBasicRe.FindStringSubmatch(m)
	if g[1] == "" && !strings.ContainsAny(g[3], "0123456789=+/.") {
		return m
	}
	return g[1] + g[2] + " " + RedactedMarker
}

// blankKeyValues replaces the value of every match of re in s, whose
// groups are key, separator and value, with RedactedMarker, keeping the key
// and separator. An unquoted value also takes in the space-separated words
// after it on the same line (extendValue), so a value with spaces is
// blanked whole. A value an earlier rule already replaced is left as is.
func blankKeyValues(re *regexp.Regexp, s string) string {
	var b strings.Builder
	last := 0
	for _, m := range re.FindAllStringSubmatchIndex(s, -1) {
		// Every key regex has exactly three groups (key, separator, value)
		// and a value of at least one byte; skip anything else.
		if len(m) < 8 || m[6] < 0 || m[6] == m[7] {
			continue
		}
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
		b.WriteString(RedactedMarker)
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

// extendValue returns the end of an unquoted value that ends at i in s,
// extended over each following word that is separated from it by spaces or
// tabs only, ends at whitespace, a delimiter or the end of s, and is not a
// key: it holds no '=' or ':', and the next non-whitespace character after
// it (on any line) is not one either. A RedactedMarker counts as one word.
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
		if strings.HasPrefix(s[j:], RedactedMarker) {
			k = j + len(RedactedMarker)
		} else {
			for k < len(s) && !isValueEnd(s[k]) {
				k++
			}
		}
		if k == j || strings.ContainsAny(s[j:k], "=:") {
			return i
		}
		n := k
		for n < len(s) && isTextSpace(s[n]) {
			n++
		}
		if n < len(s) && (s[n] == '=' || s[n] == ':') {
			return i
		}
		i = k
	}
}

// isTextSpace reports whether c is a space, tab, CR or LF.
func isTextSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n':
		return true
	}
	return false
}

// isValueEnd reports whether c ends an unquoted value: whitespace or one of
// textValueDelimiters.
func isValueEnd(c byte) bool {
	switch c {
	case ',', ';', '&', '"', '\'', '}', ']':
		return true
	}
	return isTextSpace(c)
}

// looksEncoded reports whether run, a long run of base64/hex characters,
// looks like encoded data rather than a name: all hex, or a mix of upper
// case, lower case and digits. Kubernetes object and container names are
// lower case, so they are kept.
func looksEncoded(run string) bool {
	if strings.Contains(run, RedactedMarker) {
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

// truncateSanitizedText cuts s to at most MaxSanitizedTextBytes on a rune
// boundary and appends a marker with s's full length.
func truncateSanitizedText(s string) string {
	if len(s) <= MaxSanitizedTextBytes {
		return s
	}
	cut := MaxSanitizedTextBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("...[truncated, %d bytes]", len(s))
}
