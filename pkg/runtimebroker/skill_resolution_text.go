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
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Store a fixed reason phrase for skill resolver errors; full detail goes to
// server-side logs (ptone/scion#5061). The text the agent runtime returns
// to the hub (an API error body, a stored phase message, launch.error) is
// built here from the error code, the upstream HTTP status and a
// correlation id. The full resolver error is logged by
// logSkillResolutionFailure with the same correlation id.

// skillResolutionPhrases is the fixed reason phrase for each resolver
// error code. A code not listed gets skillResolutionDefaultPhrase.
var skillResolutionPhrases = map[string]string{
	agent.SkillErrCodeNotFound:            "skill not found",
	agent.SkillErrCodeForbidden:           "access denied",
	agent.SkillErrCodeRateLimited:         "rate limited by the skill source",
	agent.SkillErrCodeTimeout:             "timed out reaching the skill source",
	agent.SkillErrCodeUpstreamUnavailable: "skill source unavailable",
	agent.SkillErrCodeUnreachable:         "skill source unreachable",
	agent.SkillErrCodeInvalidURI:          "invalid skill URI",
}

// skillResolutionDefaultPhrase is the reason phrase for a resolver error
// code with no phrase of its own.
const skillResolutionDefaultPhrase = "resolver error"

// skillResolutionPhrase returns the fixed reason phrase for code.
func skillResolutionPhrase(code string) string {
	if p, ok := skillResolutionPhrases[code]; ok {
		return p
	}
	return skillResolutionDefaultPhrase
}

// maxPublicSkillRefLen bounds the skill reference shown to the user, as
// quoted in the text (without the quotes) and as the skill detail, so the
// correlation id at the end of the text survives the hub's cut of a stored
// failure reason to 512 bytes.
const maxPublicSkillRefLen = 200

// redactSkillRef returns the declared skill reference uri for user-visible
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
func redactSkillRef(uri string) string {
	loc := urlStartPattern.FindStringIndex(uri)
	if loc != nil && loc[0] == 0 {
		return redactURLRun(normalisedScheme(uri[:loc[1]]), uri[loc[1]:])
	}
	if rest, ok := skillSchemeRest(uri); ok {
		if isSkillSchemeRefShown(rest) {
			return uri
		}
		return redactedURL
	}
	if loc != nil || strings.Contains(uri, "://") {
		return redactedURL
	}
	return uri
}

// skillSchemes are the skill reference schemes whose references can be
// shown as they are (see isSkillSchemeRefShown).
var skillSchemes = []string{"gh://", "gcp-skill://", "skill://"}

// skillSchemeRest returns the part of uri after its skill scheme (one of
// skillSchemes, in any case, at the start of uri), and whether it has one.
func skillSchemeRest(uri string) (string, bool) {
	lower := strings.ToLower(uri)
	for _, scheme := range skillSchemes {
		if strings.HasPrefix(lower, scheme) {
			return uri[len(scheme):], true
		}
	}
	return "", false
}

// isSkillSchemeRefShown reports whether a skill-scheme reference, whose
// part after the scheme is rest, can be shown as it is: rest holds no
// http(s) scheme, no whitespace, no "?" or "#", and no ":" (so no
// user:password@ form). An "@" is allowed, as in gh://o/r/s@ref or
// skill://scope/name@version.
func isSkillSchemeRefShown(rest string) bool {
	return !urlStartPattern.MatchString(rest) &&
		strings.IndexFunc(rest, unicode.IsSpace) < 0 &&
		!strings.ContainsAny(rest, "?#:")
}

// publicSkillRef returns redactSkillRef(uri) cut to maxPublicSkillRefLen
// bytes (see capText), for the skill detail of an error response.
func publicSkillRef(uri string) string {
	return capText(redactSkillRef(uri), maxPublicSkillRefLen)
}

// quotedSkillRef returns redactSkillRef(uri) quoted as by %q. When the
// quoted text between the quotes is longer than maxPublicSkillRefLen bytes,
// it is cut at an escape boundary and ends in an ellipsis: the cap applies
// after quoting, because %q writes a control or invalid byte as four bytes.
func quotedSkillRef(uri string) string {
	ref := redactSkillRef(uri)
	q := strconv.Quote(ref)
	if len(q)-2 <= maxPublicSkillRefLen {
		return q
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(ref); {
		_, w := utf8.DecodeRuneInString(ref[i:])
		esc := strconv.Quote(ref[i : i+w])
		esc = esc[1 : len(esc)-1]
		if b.Len()-1+len(esc) > maxPublicSkillRefLen {
			b.WriteString("…")
			break
		}
		b.WriteString(esc)
		i += w
	}
	b.WriteByte('"')
	return b.String()
}

// capText returns s cut to at most limit bytes at a rune boundary, with an
// ellipsis appended when it was cut. limit should be at least
// utf8.UTFMax, so that a cut text keeps at least one rune.
func capText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// skillResolutionPublicText returns the user-visible text for e. For an
// install failure it is the existing install text, whose reason is already
// public (see agent.skillInstallError). For a resolver error it is a fixed
// reason phrase, the upstream HTTP status when one is known, and ref, the
// correlation id that also appears in the server-side log record. An empty
// ref is left out.
func skillResolutionPublicText(e *agent.SkillResolutionError, ref string) string {
	uri := quotedSkillRef(e.URI)
	if e.Code == agent.SkillErrCodeInstallFailed {
		return fmt.Sprintf("required skill %s could not be installed: %s", uri, e.Message)
	}
	text := fmt.Sprintf("required skill %s could not be resolved: %s", uri, skillResolutionPhrase(e.Code))
	if e.Status > 0 {
		text += fmt.Sprintf(" (status %d)", e.Status)
	}
	if ref != "" {
		text += " (ref " + ref + ")"
	}
	return text
}

// skillResolutionCorrelationID returns the id that ties the stored text of
// a skill resolution failure to its server-side log record: the run id when
// there is one, else a new random id.
func skillResolutionCorrelationID(runID string) string {
	if runID != "" {
		return runID
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
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

// redactedURL replaces a URL, after its scheme, that may carry a
// credential.
const redactedURL = "(redacted URL)"

// unparseableURL replaces a URL, after its scheme, that does not parse.
const unparseableURL = "(unparseable URL)"

// redactURLsInText returns s with every http(s) URL in it redacted for the
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
// The skill reference itself is decided as a whole (see redactSkillRef).
//
// Only http and https are handled. URLs with other schemes (gs://, s3://,
// ftp://, ssh://, or scp-style git@host:path) are out of scope and logged
// as they are.
func redactURLsInText(s string) string {
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
		return scheme + redactedURL
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

// skillLogText returns the full text of e for the server-side log, built
// from its parts: the reference as redactSkillRef gives it, and the message
// with its URLs redacted (see redactURLsInText).
func skillLogText(e *agent.SkillResolutionError) string {
	redacted := *e
	redacted.URI = redactSkillRef(e.URI)
	redacted.Message = e.Message
	if e.URI != "" && redacted.URI != e.URI {
		// The message may quote the reference itself.
		redacted.Message = strings.ReplaceAll(redacted.Message, e.URI, redacted.URI)
	}
	redacted.Message = redactURLsInText(redacted.Message)
	return redacted.Error()
}

// skillFailure is a skill resolution failure in a create or start error,
// with the correlation id that its user-visible text and its log record
// carry.
type skillFailure struct {
	err *agent.SkillResolutionError
	ref string
}

// skillFailureFrom returns the skill resolution failure in err's chain, with
// the correlation id for runID (see skillResolutionCorrelationID), or nil
// when there is none.
func skillFailureFrom(err error, runID string) *skillFailure {
	var skillErr *agent.SkillResolutionError
	if !errors.As(err, &skillErr) {
		return nil
	}
	return &skillFailure{err: skillErr, ref: skillResolutionCorrelationID(runID)}
}

// publicText returns the user-visible text of f (see
// skillResolutionPublicText).
func (f *skillFailure) publicText() string {
	return skillResolutionPublicText(f.err, f.ref)
}

// failureLogAttrs returns the error attributes of a general create or start
// failure log record. For a skill resolution failure f they are its
// user-visible text and correlation id: the full text is in the one record
// logSkillResolutionFailure writes. For any other error (f nil) they are
// err itself.
func failureLogAttrs(err error, f *skillFailure) []any {
	if f == nil {
		return []any{"error", err}
	}
	return []any{"error", f.publicText(), "correlation_id", f.ref}
}

// runtimeCauseLogAttrs returns runtimeFailureLogAttrs(err) for a general
// failure record when there is no skill failure f, and nothing when there
// is one: the runtime cause of a skill failure is the skill failure itself,
// whose full text belongs only in the record logSkillResolutionFailure
// writes.
func runtimeCauseLogAttrs(err error, f *skillFailure) []any {
	if f != nil {
		return nil
	}
	return runtimeFailureLogAttrs(err)
}

// spanErrorText returns err's text for a trace span status: for an error
// holding a skill resolution failure, the failure's redacted log text (see
// skillLogText), with a fixed suffix when err also holds
// context.DeadlineExceeded or context.Canceled; for any other error, err's
// text unchanged.
func spanErrorText(err error) string {
	var skillErr *agent.SkillResolutionError
	if !errors.As(err, &skillErr) {
		return err.Error()
	}
	text := skillLogText(skillErr)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		text += "; context deadline exceeded"
	case errors.Is(err, context.Canceled):
		text += "; context canceled"
	}
	return text
}

// skillFailureLogMsg is the message of the log record that carries the full
// text of a skill resolution or install failure.
const skillFailureLogMsg = "Required skill failed"

// logSkillResolutionFailure logs the full text of the skill failure f, with
// its URLs redacted, under f's correlation id. attrs are extra log
// attributes (agent_id, project_id, run_id).
func (s *Server) logSkillResolutionFailure(f *skillFailure, attrs ...any) {
	s.agentLifecycleLog.Warn(skillFailureLogMsg, append(attrs,
		"correlation_id", f.ref,
		"skill_uri", redactSkillRef(f.err.URI),
		"code", f.err.Code,
		"status", f.err.Status,
		"error", skillLogText(f.err))...)
}
