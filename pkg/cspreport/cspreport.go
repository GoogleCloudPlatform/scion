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

// Package cspreport receives the policy reports browsers send for the web
// pages and writes each one to the log as a csp_violation record.
//
// Both report content types are accepted: application/reports+json and
// application/csp-report. The body is read by its shape, whichever of the
// two types it is sent with, since browsers do not agree on the pairing:
// a JSON array of Reporting API reports, a single Reporting API report (an
// object with "type" and "body", as Safari sends it), or the older
// single-report form (an object with a "csp-report" member). Reports are
// only logged: nothing is persisted in application storage and nothing is
// sent back besides the status code.
//
// A record holds the directive, the sink of a Trusted Types report, the
// blocked URI and source file (reduced, see reduceURL), the line and the
// path of the page. The report's sample, which can hold the start of page
// content such as a chat message, is left out unless Handler.RawSamples is
// set. With Handler.Dedupe, repeats of the same violation are collapsed
// into one record per interval, and a summary record (RepeatsLogMessage)
// later counts them and shows the latest (see Dedupe and repeatAttrs).
package cspreport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxBodyBytes caps the request body. A larger body gets 413 and is not
	// parsed.
	MaxBodyBytes = 16 << 10

	// MaxSampleRunes is the length the sample is cut to before it is
	// logged (only with Handler.RawSamples).
	MaxSampleRunes = 120

	// maxFieldBytes bounds the other string fields of a record.
	maxFieldBytes = 512

	// maxRecordsPerRequest bounds the records one request can write. A
	// browser batches a few reports at a time. Repeats collapsed by Dedupe
	// do not count.
	maxRecordsPerRequest = 10

	// LogMessage is the message of the record written for each report.
	LogMessage = "csp_violation"

	// DroppedLogMessage is the message of the one record written when a
	// request holds more reports to log than maxRecordsPerRequest; its
	// "dropped" attribute counts the reports that were not logged.
	DroppedLogMessage = "csp_violation_dropped"

	// RepeatsLogMessage is the message of the summary record Dedupe writes
	// for a violation whose repeats were collapsed (see repeatAttrs).
	RepeatsLogMessage = "csp_violation_repeats"

	contentTypeReports   = "application/reports+json"
	contentTypeCSPReport = "application/csp-report"
	reportTypeCSP        = "csp-violation"

	// directiveTrustedTypes is the directive of a Trusted Types sink
	// report; its sample starts with the sink name and a '|'.
	directiveTrustedTypes = "require-trusted-types-for"
)

// Limiter decides whether a request may be handled now. retryAfterSeconds
// is used only when allowed is false.
type Limiter func(r *http.Request) (allowed bool, retryAfterSeconds int)

// RecordLimiter decides whether one more record may be written now. It is
// asked once per record, the dropped-count record included.
// retryAfterSeconds is used only when allowed is false.
type RecordLimiter func() (allowed bool, retryAfterSeconds int)

// Handler is the report endpoint. Allow is asked once per request, before
// the content type is checked and the body is read, so refused and
// malformed requests count against it too. AllowRecord is asked once per
// report record and dropped-count record written; a report collapsed by
// Dedupe, and a Dedupe summary record, do not ask it. With a Dedupe,
// AllowRecord is called under the Dedupe's lock, so it must not block or
// call back into the handler. Log
// receives the records; a nil Log uses slog.Default(). A nil Allow or
// AllowRecord lets everything through, and a nil Dedupe logs every report.
type Handler struct {
	Allow       Limiter
	AllowRecord RecordLimiter
	Dedupe      *Dedupe
	Log         *slog.Logger
	// RawSamples adds each report's sample, cut to MaxSampleRunes, to its
	// record as "sample". Off by default: a sample can hold the start of
	// page content (a Trusted Types sample is the text passed to the
	// sink), so it is logged only when an operator turns this on to
	// diagnose a report.
	RawSamples bool
}

// Violation is one parsed report. Sample is logged only with
// Handler.RawSamples. DocumentPath is the path of the page the report is
// about (see documentPath).
type Violation struct {
	Directive    string
	Sink         string
	BlockedURI   string
	SourceFile   string
	Line         int
	DocumentPath string
	Sample       string
}

// ServeHTTP answers 204 when every report was written, collapsed as a
// repeat by Dedupe, or counted as dropped; 429 when the request limiter
// refuses or the record limiter refuses a report before any record of the
// request was written; 413 when the body is over MaxBodyBytes, 415 for
// another content type and 400 for a malformed body. Reports refused by
// either limiter are neither logged nor counted. The dropped-count record
// covers only the reports to log past maxRecordsPerRequest (repeats are
// collapsed first), and is written only while the record limiter allows
// it. Dedupe summary records are written whatever the answer.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.Allow != nil {
		if ok, retry := h.Allow(r); !ok {
			tooMany(w, retry)
			return
		}
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mediaType != contentTypeReports && mediaType != contentTypeCSPReport) {
		http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "report too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "unreadable report", http.StatusBadRequest)
		return
	}
	violations, err := parseBody(body)
	if err != nil {
		http.Error(w, "malformed report", http.StatusBadRequest)
		return
	}
	log := h.Log
	if log == nil {
		log = slog.Default()
	}
	written, dropped := 0, 0
reports:
	for _, v := range violations {
		verdict, summaries, retry := h.Dedupe.admit(v, written < maxRecordsPerRequest, h.allowRecord)
		for i := range summaries {
			// A summary belongs to earlier requests, not this one.
			log.LogAttrs(context.Background(), slog.LevelInfo, RepeatsLogMessage, repeatAttrs(&summaries[i])...)
		}
		switch verdict {
		case verdictRepeat:
			continue
		case verdictDropped:
			dropped++
			continue
		case verdictRefused:
			if written == 0 {
				tooMany(w, retry)
				return
			}
			break reports
		}
		written++
		attrs := []slog.Attr{
			slog.String("directive", v.Directive),
			slog.String("blocked_uri", v.BlockedURI),
			slog.String("source_file", v.SourceFile),
			slog.Int("line", v.Line),
			slog.String("document_path", v.DocumentPath),
		}
		if v.Sink != "" {
			attrs = append(attrs, slog.String("sink", v.Sink))
		}
		if h.RawSamples {
			attrs = append(attrs, slog.String("sample", v.Sample))
		}
		log.LogAttrs(r.Context(), slog.LevelInfo, LogMessage, attrs...)
	}
	if dropped > 0 {
		if ok, _ := h.allowRecord(); ok {
			log.LogAttrs(r.Context(), slog.LevelInfo, DroppedLogMessage, slog.Int("dropped", dropped))
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// repeatAttrs are the attributes of a summary record (RepeatsLogMessage):
// the latest repeat's "directive", "blocked_uri", "source_file", "line",
// "document_path" and, when set, "sink", as in a report record; "repeats",
// the number of repeats collapsed after the violation's record;
// "first_logged", when that record was written; and "latest_seen", when
// the latest repeat arrived. The latest repeat's sample is never logged.
func repeatAttrs(r *repeatSummary) []slog.Attr {
	v := r.Latest
	attrs := []slog.Attr{
		slog.String("directive", v.Directive),
		slog.String("blocked_uri", v.BlockedURI),
		slog.String("source_file", v.SourceFile),
		slog.Int("line", v.Line),
		slog.String("document_path", v.DocumentPath),
	}
	if v.Sink != "" {
		attrs = append(attrs, slog.String("sink", v.Sink))
	}
	return append(attrs,
		slog.Int("repeats", r.Count),
		slog.Time("first_logged", r.First.UTC()),
		slog.Time("latest_seen", r.LatestSeen.UTC()),
	)
}

// allowRecord asks AllowRecord, allowing everything when it is nil.
func (h *Handler) allowRecord() (bool, int) {
	if h.AllowRecord == nil {
		return true, 0
	}
	return h.AllowRecord()
}

// tooMany answers 429 with a Retry-After of at least one second.
func tooMany(w http.ResponseWriter, retry int) {
	if retry < 1 {
		retry = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	http.Error(w, "too many reports", http.StatusTooManyRequests)
}

// reportingAPIReport is one Reporting API report: an entry of an array
// body, or the whole body when a browser sends a single report.
type reportingAPIReport struct {
	Type string `json:"type"`
	URL  string `json:"url"`
	Body *struct {
		DocumentURL        string `json:"documentURL"`
		EffectiveDirective string `json:"effectiveDirective"`
		BlockedURL         string `json:"blockedURL"`
		SourceFile         string `json:"sourceFile"`
		LineNumber         int    `json:"lineNumber"`
		Sample             string `json:"sample"`
	} `json:"body"`
}

// legacyCSPReport is the "csp-report" member of the older single-report
// form.
type legacyCSPReport struct {
	DocumentURI        string `json:"document-uri"`
	EffectiveDirective string `json:"effective-directive"`
	ViolatedDirective  string `json:"violated-directive"`
	BlockedURI         string `json:"blocked-uri"`
	SourceFile         string `json:"source-file"`
	LineNumber         int    `json:"line-number"`
	ScriptSample       string `json:"script-sample"`
}

// singleReport is an object body: either the older form (with a
// "csp-report" member) or one Reporting API report.
type singleReport struct {
	reportingAPIReport
	Legacy *legacyCSPReport `json:"csp-report"`
}

var errMalformed = errors.New("malformed report")

// parseBody reads a report body by its shape: a JSON array is a list of
// Reporting API reports, and an object is either the older form or a
// single Reporting API report. Entries of other report types are skipped;
// a body with no policy report in it is malformed. Every policy report is
// returned (MaxBodyBytes bounds how many); the handler applies
// maxRecordsPerRequest after collapsing repeats.
func parseBody(body []byte) ([]Violation, error) {
	if firstByte(body) == '[' {
		var reports []reportingAPIReport
		if err := json.Unmarshal(body, &reports); err != nil {
			return nil, errMalformed
		}
		var out []Violation
		for _, rep := range reports {
			if v, ok := rep.violation(); ok {
				out = append(out, v)
			}
		}
		if len(out) == 0 {
			return nil, errMalformed
		}
		return out, nil
	}
	var rep singleReport
	if err := json.Unmarshal(body, &rep); err != nil {
		return nil, errMalformed
	}
	if b := rep.Legacy; b != nil {
		directive := b.EffectiveDirective
		if directive == "" {
			directive = b.ViolatedDirective
		}
		return []Violation{newViolation(directive, b.BlockedURI, b.SourceFile, b.LineNumber, b.DocumentURI, b.ScriptSample)}, nil
	}
	v, ok := rep.violation()
	if !ok {
		return nil, errMalformed
	}
	return []Violation{v}, nil
}

// firstByte returns the first byte of body that is not JSON white space,
// or 0.
func firstByte(body []byte) byte {
	for _, c := range body {
		switch c {
		case ' ', '\t', '\r', '\n':
		default:
			return c
		}
	}
	return 0
}

// violation returns the report as a Violation, or false when it is not a
// policy report with a body. The page is the body's documentURL, or the
// report's url when the body has none.
func (rep reportingAPIReport) violation() (Violation, bool) {
	if rep.Type != reportTypeCSP || rep.Body == nil {
		return Violation{}, false
	}
	b := rep.Body
	document := b.DocumentURL
	if document == "" {
		document = rep.URL
	}
	return newViolation(b.EffectiveDirective, b.BlockedURL, b.SourceFile, b.LineNumber, document, b.Sample), true
}

// newViolation bounds every field of a record. URLs are reduced by
// reduceURL, and the page by documentPath; the sink is taken from the
// sample of a Trusted Types report.
func newViolation(directive, blockedURI, sourceFile string, line int, documentURL, sample string) Violation {
	if line < 0 {
		line = 0
	}
	return Violation{
		Directive:    truncateBytes(directive, maxFieldBytes),
		Sink:         sinkName(directive, sample),
		BlockedURI:   truncateBytes(reduceURL(blockedURI), maxFieldBytes),
		SourceFile:   truncateBytes(reduceURL(sourceFile), maxFieldBytes),
		Line:         line,
		DocumentPath: truncateBytes(documentPath(documentURL), maxFieldBytes),
		Sample:       truncateRunes(sample, MaxSampleRunes),
	}
}

// key is what Dedupe tells repeats apart by: the directive, the origin of
// the blocked URI and the page path.
func (v Violation) key() string {
	return v.Directive + "\x00" + urlOrigin(v.BlockedURI) + "\x00" + v.DocumentPath
}

// trustedTypesSinks is the closed list of sink names a Trusted Types
// report's sample can start with (the Trusted Types specification's
// "Interface member" names, and the global functions). Any other text
// before the '|' is not logged.
var trustedTypesSinks = map[string]bool{
	"Element innerHTML":               true,
	"Element outerHTML":               true,
	"Element insertAdjacentHTML":      true,
	"Element setHTMLUnsafe":           true,
	"ShadowRoot innerHTML":            true,
	"ShadowRoot setHTMLUnsafe":        true,
	"Document write":                  true,
	"Document writeln":                true,
	"Document parseHTMLUnsafe":        true,
	"DOMParser parseFromString":       true,
	"Range createContextualFragment":  true,
	"HTMLIFrameElement srcdoc":        true,
	"HTMLScriptElement src":           true,
	"HTMLScriptElement text":          true,
	"HTMLScriptElement innerText":     true,
	"HTMLScriptElement textContent":   true,
	"SVGScriptElement href":           true,
	"Window setTimeout":               true,
	"Window setInterval":              true,
	"WorkerGlobalScope setTimeout":    true,
	"WorkerGlobalScope setInterval":   true,
	"WorkerGlobalScope importScripts": true,
	"Function":                        true,
	"eval":                            true,
}

// sinkName returns the sink a Trusted Types report names: the part of its
// sample before the first '|' (for example "Element innerHTML"), only when
// it is one of trustedTypesSinks. Anything else, and every other
// directive's sample, gives "".
func sinkName(directive, sample string) string {
	if directive != directiveTrustedTypes {
		return ""
	}
	i := strings.IndexByte(sample, '|')
	if i <= 0 {
		return ""
	}
	if name := sample[:i]; trustedTypesSinks[name] {
		return name
	}
	return ""
}

// reduceURL keeps what is needed to find the code behind a report and
// drops what can carry data:
//   - a value without a scheme (a keyword such as "inline", "eval" or
//     "trusted-types-sink", or a path) is kept without its query and
//     fragment; a scheme-relative value ("//host/path") also loses its
//     user information;
//   - an http, https, ws or wss URL keeps its scheme, host and path; the
//     user information, query and fragment are dropped;
//   - any other scheme (data:, blob:, javascript:, ...) is reduced to the
//     scheme alone, for example "data:".
//
// The bytes that are kept are not re-encoded.
func reduceURL(s string) string {
	scheme, rest, ok := splitScheme(s)
	if !ok {
		s = withoutQuery(s)
		if strings.HasPrefix(s, "//") {
			return "//" + withoutUserInfo(s[2:])
		}
		return s
	}
	switch strings.ToLower(scheme) {
	case "http", "https", "ws", "wss":
	default:
		return strings.ToLower(scheme) + ":"
	}
	rest = withoutQuery(rest)
	if !strings.HasPrefix(rest, "//") {
		return strings.ToLower(scheme) + ":"
	}
	return scheme + "://" + withoutUserInfo(rest[2:])
}

// withoutUserInfo takes "authority/path" (the part after "//") and drops
// the user information from the authority.
func withoutUserInfo(s string) string {
	authority, path := s, ""
	if i := strings.IndexByte(s, '/'); i >= 0 {
		authority, path = s[:i], s[i:]
	}
	if i := strings.LastIndexByte(authority, '@'); i >= 0 {
		authority = authority[i+1:]
	}
	return authority + path
}

// splitScheme splits s at the colon after a URL scheme (an ASCII letter,
// then letters, digits, '+', '-' or '.').
func splitScheme(s string) (scheme, rest string, ok bool) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
		case i > 0 && ((c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.'):
		case i > 0 && c == ':':
			return s[:i], s[i+1:], true
		default:
			return "", "", false
		}
	}
	return "", "", false
}

// withoutQuery drops everything from the first '?' or '#'.
func withoutQuery(s string) string {
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		return s[:i]
	}
	return s
}

// printable returns s as valid UTF-8 with every character that is not
// graphic (controls, format characters, line and paragraph separators,
// private-use and unassigned code points) replaced by U+FFFD, so a record
// holds printable text only. Letters, marks, numbers, punctuation,
// symbols and ordinary spaces are kept.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if !unicode.IsGraphic(r) {
			return utf8.RuneError
		}
		return r
	}, strings.ToValidUTF8(s, "\uFFFD"))
}

// truncateRunes returns s as printable text cut to at most n runes.
func truncateRunes(s string, n int) string {
	s = printable(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for count := 0; count < n; count++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i]
}

// truncateBytes returns s as printable text cut to at most n bytes on a
// rune boundary.
func truncateBytes(s string, n int) string {
	s = printable(s)
	if len(s) <= n {
		return s
	}
	i := n
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}
