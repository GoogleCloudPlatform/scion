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

// causeValuePattern matches a value after a key and its separator: a
// quoted string, or a run up to whitespace or a delimiter.
const causeValuePattern = `("[^"]*"|'[^']*'|[^\s,;&"'}\]]+)`

var (
	// A PEM block, or an unterminated one running to the end of the text.
	causePEMRe = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]+-----.*?(?:-----END [A-Z0-9 ]+-----|$)`)
	// The userinfo part of a URL: scheme://user:pass@host.
	causeURLUserInfoRe = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9+.-]*://)[^\s/?#@]+@`)
	// An HTTP authorization scheme followed by its credentials.
	causeAuthSchemeRe = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
	// A JWT-shaped string: three base64url segments, the first a JSON object.
	causeJWTRe = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`)
	// An environment-style credential-named key (or any SCION_ variable)
	// followed by ':' or '=' and its value.
	causeEnvCredentialKeyRe = regexp.MustCompile(`\b(SCION_[A-Z0-9_]*|[A-Z0-9_]*(?:TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIAL|API_?KEY|PRIVATE_?KEY|ACCESS_?KEY|AUTH)[A-Z0-9_]*)(\s*[:=]\s*)` + causeValuePattern)
	// A quoted credential-named key, as in JSON, followed by ':' and its
	// value.
	causeQuotedCredentialKeyRe = regexp.MustCompile(`(?i)("[A-Za-z0-9_.-]*(?:token|secret|password|passwd|credential|api_?key|private_?key|access_?key|auth)[A-Za-z0-9_.-]*")(\s*:\s*)` + causeValuePattern)
	// Any KEY=VALUE pair (env entries, flags, query parameters).
	causeKeyValueRe = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_.-]*)(=)` + causeValuePattern)
	// A long run of base64 or hex characters.
	causeLongRunRe = regexp.MustCompile(`[A-Za-z0-9+/_=-]{32,}`)
)

// sanitizeFailureCause returns text, a runtime failure's backend error text,
// in the form a broker log record may carry. The text is untrusted: a
// runtime's output can repeat parts of the agent's start spec. So every
// KEY=VALUE value is blanked (the key stays), as are credential-named
// keys' values, authorization schemes, JWT-shaped strings, URL userinfo,
// PEM blocks and long base64 or hex runs. The result is then truncated to
// maxLoggedCauseBytes, with a marker giving the sanitized length.
//
// Every log site that records a create or start failure's cause calls this
// on that field. The text never reaches a client response.
func sanitizeFailureCause(text string) string {
	s := causePEMRe.ReplaceAllString(text, redactedMarker)
	s = causeURLUserInfoRe.ReplaceAllString(s, "${1}"+redactedMarker+"@")
	s = causeAuthSchemeRe.ReplaceAllString(s, "${1} "+redactedMarker)
	s = causeJWTRe.ReplaceAllString(s, redactedMarker)
	s = blankKeyValues(causeEnvCredentialKeyRe, s)
	s = blankKeyValues(causeQuotedCredentialKeyRe, s)
	s = blankKeyValues(causeKeyValueRe, s)
	s = causeLongRunRe.ReplaceAllStringFunc(s, func(m string) string {
		if looksEncoded(m) {
			return redactedMarker
		}
		return m
	})
	return truncateFailureCause(s)
}

// blankKeyValues replaces the value of every match of re in s, whose
// groups are key, separator and value, with redactedMarker, keeping the key
// and separator. A value an earlier rule already replaced is left as is.
func blankKeyValues(re *regexp.Regexp, s string) string {
	return re.ReplaceAllStringFunc(s, func(m string) string {
		g := re.FindStringSubmatch(m)
		if strings.HasPrefix(g[3], "[REDACTED") {
			return m
		}
		return g[1] + g[2] + redactedMarker
	})
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
