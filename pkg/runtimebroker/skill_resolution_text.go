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

// unparseableSkillRef stands in for an http(s) skill reference that does
// not parse as a URL.
const unparseableSkillRef = "(unparseable skill URL)"

// maxPublicSkillRefLen bounds the skill reference shown to the user, as
// quoted in the text (without the quotes) and as the skill detail, so the
// correlation id at the end of the text survives the hub's cut of a stored
// failure reason to 512 bytes.
const maxPublicSkillRefLen = 200

// redactSkillRef returns the declared skill reference uri for user-visible
// text. An http(s) reference loses its userinfo, query and fragment, and
// its path is redacted by logging.RedactURL where that applies; any other
// reference (gh://, a registry name) is kept.
func redactSkillRef(uri string) string {
	lower := strings.ToLower(uri)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return uri
	}
	u, err := url.Parse(uri)
	if err != nil {
		return unparseableSkillRef
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return logging.RedactURL(u)
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

// urlStartPattern matches the start of an http(s) URL inside free text.
var urlStartPattern = regexp.MustCompile(`(?i)https?://`)

// urlTrailingPunct is punctuation that, at the end of a URL with no query
// or fragment, is taken to belong to the surrounding text, not to the URL.
const urlTrailingPunct = `.,;:!)]}"'<>`

// redactedLogValue replaces a query or fragment in a logged URL.
const redactedLogValue = "REDACTED"

// redactURLsInText returns s with every http(s) URL in it redacted for the
// server-side log. Each URL runs from its "http://" or "https://" to the
// next whitespace or the next "http://" or "https://", so two URLs joined
// with no separator are redacted one by one, and a URL holding a raw quote
// or angle bracket is redacted whole. In each URL:
//   - A URL holding an "@" anywhere (userinfo, or an "@" in its path, query
//     or fragment) becomes "(redacted URL)" after its scheme, and nothing
//     else of it is kept. This loses the host from the log line, which is
//     accepted: it is the one rule that never keeps userinfo.
//   - Otherwise, everything after the first "?" up to the next whitespace
//     (the query, any fragment and any closing punctuation) becomes
//     "?REDACTED", and a fragment with no query becomes "#REDACTED". For
//     example, Get "https://h/x?q=v": EOF  becomes
//     Get "https://h/x?REDACTED EOF
//   - An artifact path becomes logging.RedactedArtifactPath.
//   - A URL that does not parse after this becomes "(unparseable URL)"
//     after its scheme.
//
// Punctuation ending a URL with no query or fragment (see
// urlTrailingPunct; "]" only when the URL has no "[", as in an IPv6 host)
// is kept as text. The rest of s is unchanged.
//
// Only http and https are handled. URLs with other schemes (gs://, s3://,
// ftp://, ssh://, or scp-style git@host:path) are out of scope and logged
// as they are.
func redactURLsInText(s string) string {
	starts := urlStartPattern.FindAllStringIndex(s, -1)
	if len(starts) == 0 {
		return s
	}
	var b strings.Builder
	prev := 0
	for i, loc := range starts {
		b.WriteString(s[prev:loc[0]])
		end := len(s)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		if ws := strings.IndexFunc(s[loc[0]:end], unicode.IsSpace); ws >= 0 {
			end = loc[0] + ws
		}
		b.WriteString(redactURLToken(s[loc[0]:end], loc[1]-loc[0]))
		prev = end
	}
	b.WriteString(s[prev:])
	return b.String()
}

// redactURLToken redacts one URL found by redactURLsInText. schemeLen is
// the length of its "http://" or "https://".
func redactURLToken(token string, schemeLen int) string {
	scheme := strings.ToLower(token[:schemeLen])
	rest := token[schemeLen:]
	q := strings.IndexAny(rest, "?#")
	if strings.Contains(rest, "@") {
		trailing := ""
		if q < 0 {
			_, trailing = trimURLTrailing(rest)
		}
		return scheme + redactedURL + trailing
	}
	var suffix, trailing string
	if q >= 0 {
		if rest[q] == '?' {
			suffix = "?" + redactedLogValue
		} else {
			suffix = "#" + redactedLogValue
		}
		rest = rest[:q]
	} else {
		rest, trailing = trimURLTrailing(rest)
	}
	authority, path := rest, ""
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		authority, path = rest[:j], rest[j:]
	}
	// A URL with an "@" was replaced above, so u.User is always nil here;
	// the check is a defence in case that rule changes.
	u, err := url.Parse(scheme + authority + path)
	if err != nil || u.User != nil {
		return scheme + unparseableURL + suffix + trailing
	}
	if logging.IsCredentialURL(u) {
		path = logging.RedactedArtifactPath
	}
	return scheme + authority + path + suffix + trailing
}

// redactedURL replaces a logged URL, after its scheme, that holds an "@".
const redactedURL = "(redacted URL)"

// unparseableURL replaces a logged URL, after its scheme, that does not
// parse once its query and fragment are removed.
const unparseableURL = "(unparseable URL)"

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

// skillResolutionLogError returns the text of err for the server-side log,
// with its URLs redacted (see redactURLsInText).
func skillResolutionLogError(err error) string {
	if err == nil {
		return ""
	}
	return redactURLsInText(err.Error())
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

// spanErrorText returns err's text for a trace span status: for a skill
// resolution failure, the text with its URLs redacted (see
// redactURLsInText); for any other error, err's text unchanged.
func spanErrorText(err error) string {
	var skillErr *agent.SkillResolutionError
	if errors.As(err, &skillErr) {
		return skillResolutionLogError(err)
	}
	return err.Error()
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
		"skill_uri", redactURLsInText(f.err.URI),
		"code", f.err.Code,
		"status", f.err.Status,
		"error", skillResolutionLogError(f.err))...)
}
