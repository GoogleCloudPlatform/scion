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

package agent

import (
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// RedactSkillRef returns the declared skill reference uri for user-visible
// text and the server-side log, deciding the whole reference:
//   - A reference that starts with an http(s) scheme (see urlStartPattern)
//     gets the free-text decision (see redactURLRun): the path-only form
//     when it holds no "@", "?", "#", whitespace or second scheme, else
//     "(redacted URL)" after its scheme.
//   - A reference that starts with a skill scheme (gh://, gcp-skill:// or
//     skill://) is kept when isSkillSchemeRefShown allows it, else it
//     becomes "(redacted URL)".
//   - Any other reference that holds "://" or an http(s) scheme anywhere
//     (git+https://..., a leading space, ssh://, s3://) becomes
//     "(redacted URL)" with no scheme.
//   - A reference that is not URL-like (a plain skill name) is kept.
func RedactSkillRef(uri string) string {
	loc := urlStartPattern.FindStringIndex(uri)
	if loc != nil && loc[0] == 0 {
		return redactURLRun(normalisedScheme(uri[:loc[1]]), uri[loc[1]:])
	}
	if rest, ok := skillSchemeRest(uri); ok {
		if isSkillSchemeRefShown(rest) {
			return uri
		}
		return RedactedURL
	}
	if loc != nil || strings.Contains(uri, "://") {
		return RedactedURL
	}
	return uri
}

// skillSchemes are the skill reference schemes whose references can be
// shown as they are (see isSkillSchemeRefShown).
var skillSchemes = []string{"gh://", "gcp-skill://", "skill://"}

// skillSchemeRest returns the part of uri after its skill scheme (one of
// skillSchemes, in any case, at the start of uri), and whether it has one.
func skillSchemeRest(uri string) (string, bool) {
	for _, scheme := range skillSchemes {
		if hasASCIIPrefixFold(uri, scheme) {
			return uri[len(scheme):], true
		}
	}
	return "", false
}

// hasASCIIPrefixFold reports whether s starts with prefix, an ASCII
// lower-case string, ignoring ASCII case only (so no Unicode letter, such
// as the Kelvin sign, matches an ASCII one).
func hasASCIIPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != prefix[i] {
			return false
		}
	}
	return true
}

// isSkillSchemeRefShown reports whether a skill-scheme reference, whose
// part after the scheme is rest, can be shown as it is: rest holds no
// http(s) scheme, no whitespace, no "?" or "#", and no ":" or "%" (so no
// user:password@ form, percent-encoded or not). An "@" is allowed, as in
// gh://o/r/s@ref or skill://scope/name@version.
func isSkillSchemeRefShown(rest string) bool {
	return !urlStartPattern.MatchString(rest) &&
		strings.IndexFunc(rest, unicode.IsSpace) < 0 &&
		!strings.ContainsAny(rest, "?#:%")
}

// urlStartPattern matches the start of an http(s) URL: the scheme, its
// colon and any run of "/" or "\" after it, in any case, so "https:h",
// "https:/h" and "https:\\h" count as URLs too.
var urlStartPattern = regexp.MustCompile(`(?i)https?:[/\\]*`)

// normalisedScheme returns "https://" or "http://" for a urlStartPattern
// match.
func normalisedScheme(match string) string {
	if len(match) >= len("https") && strings.EqualFold(match[:len("https")], "https") {
		return "https://"
	}
	return "http://"
}

// urlTrailingPunct is punctuation that, at the end of a URL that is kept,
// is taken to belong to the surrounding text, not to the URL.
const urlTrailingPunct = `.,;:!)]}"'<>`

// RedactedURL replaces a URL, after its scheme, that may carry a
// credential.
const RedactedURL = "(redacted URL)"

// unparseableURL replaces a URL, after its scheme, that does not parse.
const unparseableURL = "(unparseable URL)"

// RedactURLsInText returns s with every http(s) URL in it redacted for the
// server-side log. A URL starts at a urlStartPattern match and runs to the
// next whitespace only; a later scheme inside the run does not start a new
// URL. Each run is decided by redactURLRun, with its scheme written as
// "http://" or "https://" in lower case. For example,
// Get "https://h/x?q=v": EOF  becomes  Get "https://(redacted URL) EOF
// and two URLs joined with no whitespace become one "(redacted URL)".
// A scheme with nothing after it (as in "net/http: ...") is kept as it is.
// The rest of s is unchanged.
//
// Whitespace is the only boundary in free text: a URL holding a raw space
// ends at the space, and the text after it is kept as plain text. For
// example, "https://u:p w@h/x" becomes "https://(unparseable URL) w@h/x".
// The skill reference itself is decided as a whole (see RedactSkillRef).
//
// Only http and https are handled. URLs with other schemes (gs://, s3://,
// ftp://, ssh://, or scp-style git@host:path) are out of scope and logged
// as they are.
func RedactURLsInText(s string) string {
	matches := urlStartPattern.FindAllStringIndex(s, -1)
	if len(matches) == 0 {
		return s
	}
	var b strings.Builder
	prev := 0
	for _, loc := range matches {
		if loc[0] < prev {
			// Inside the previous run, which redactURLRun has decided.
			continue
		}
		end := len(s)
		if ws := strings.IndexFunc(s[loc[0]:], unicode.IsSpace); ws >= 0 {
			end = loc[0] + ws
		}
		b.WriteString(s[prev:loc[0]])
		if rest := s[loc[1]:end]; rest == "" {
			b.WriteString(s[loc[0]:end])
		} else {
			b.WriteString(redactURLRun(normalisedScheme(s[loc[0]:loc[1]]), rest))
		}
		prev = end
	}
	b.WriteString(s[prev:])
	return b.String()
}

// redactURLRun decides one URL: scheme is its normalised scheme and rest
// the text after the urlStartPattern match. When rest holds an "@", "?" or
// "#" anywhere, whitespace, or a second scheme, the URL may carry userinfo,
// a query or a fragment, and the result is scheme + "(redacted URL)" with
// nothing else of it kept. Otherwise the URL is kept, with the punctuation
// that ends it (see urlTrailingPunct; "]" only when the URL has no "[", as
// in an IPv6 host) kept as text, an artifact path replaced by
// logging.RedactedArtifactPath, and scheme + "(unparseable URL)" for a URL
// that does not parse.
func redactURLRun(scheme, rest string) string {
	if strings.ContainsAny(rest, "@?#") || strings.IndexFunc(rest, unicode.IsSpace) >= 0 || urlStartPattern.MatchString(rest) {
		return scheme + RedactedURL
	}
	rest, trailing := trimURLTrailing(rest)
	authority, path := rest, ""
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		authority, path = rest[:j], rest[j:]
	}
	// rest holds no "@", so u.User is always nil here; the check is a
	// defence in case that rule changes.
	u, err := url.Parse(scheme + authority + path)
	if err != nil || u.User != nil {
		return scheme + unparseableURL + trailing
	}
	if logging.IsCredentialURL(u) {
		path = logging.RedactedArtifactPath
	}
	return scheme + authority + path + trailing
}

// trimURLTrailing splits rest, a URL after its scheme with no query or
// fragment, into the URL and the punctuation that ends it (see
// urlTrailingPunct). "]" is trimmed only when rest has no "[".
func trimURLTrailing(rest string) (head, trailing string) {
	end := len(rest)
	for end > 0 && strings.IndexByte(urlTrailingPunct, rest[end-1]) >= 0 {
		if rest[end-1] == ']' && strings.Contains(rest[:end], "[") {
			break
		}
		end--
	}
	return rest[:end], rest[end:]
}

// ResolverMessageWithheld replaces the resolver message in the log text of
// a skill failure whose reference was redacted.
const ResolverMessageWithheld = "(resolver message withheld)"

// SkillRefRedacted reports whether RedactSkillRef changes uri. When it
// does, the resolver message for that reference is withheld from the log
// (see ResolverReasonForLog), since the message can quote the reference,
// or parts of it, in any form.
func SkillRefRedacted(uri string) bool {
	return RedactSkillRef(uri) != uri
}

// ResolverReasonForLog returns the resolver's reason text for the
// declared skill reference uri, for the server-side log:
// ResolverMessageWithheld when SkillRefRedacted(uri), else reason with
// its http(s) URLs redacted (see RedactURLsInText).
func ResolverReasonForLog(uri, reason string) string {
	if SkillRefRedacted(uri) {
		return ResolverMessageWithheld
	}
	return RedactURLsInText(reason)
}

// SkillNameWithheld replaces, in the log, a skill name or destination name
// that comes from a declared skill reference whose logged form is redacted
// (see LoggedSkillName).
const SkillNameWithheld = "(skill name withheld)"

// LoggedSkillName returns name, a skill name or destination name that
// comes from the declared skill reference uri, for the server-side log:
// SkillNameWithheld when SkillRefRedacted(uri), else name.
func LoggedSkillName(uri, name string) string {
	if SkillRefRedacted(uri) {
		return SkillNameWithheld
	}
	return name
}

// LoggedSkillRef is a declared skill reference as it is written to the
// log: its String and LogValue give the redacted form (see
// RedactSkillRef). Log a declared skill reference as
// LoggedSkillRef(uri), never as the bare string.
type LoggedSkillRef string

// String returns the redacted form of the reference.
func (r LoggedSkillRef) String() string { return RedactSkillRef(string(r)) }

// LogValue returns the redacted form of the reference as a string value.
func (r LoggedSkillRef) LogValue() slog.Value { return slog.StringValue(r.String()) }
