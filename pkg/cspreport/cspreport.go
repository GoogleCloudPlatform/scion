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
// Two body shapes are accepted: the Reporting API (Content-Type
// application/reports+json, a JSON array of reports) and the older
// single-report form (Content-Type application/csp-report, an object with a
// "csp-report" member). Reports are only logged: nothing is stored and
// nothing is sent back besides the status code.
package cspreport

import (
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

	// MaxSampleRunes is the length the sample is cut to before it is logged.
	MaxSampleRunes = 120

	// maxFieldBytes bounds the other string fields of a record.
	maxFieldBytes = 512

	// maxRecordsPerRequest bounds the records one request can write. A
	// browser batches a few reports at a time.
	maxRecordsPerRequest = 10

	// LogMessage is the message of the record written for each report.
	LogMessage = "csp_violation"

	// DroppedLogMessage is the message of the one record written when a
	// request holds more reports than maxRecordsPerRequest; its "dropped"
	// attribute counts the reports that were not logged.
	DroppedLogMessage = "csp_violation_dropped"

	contentTypeReports   = "application/reports+json"
	contentTypeCSPReport = "application/csp-report"
	reportTypeCSP        = "csp-violation"
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
// record. Log receives the records; a nil Log uses slog.Default(). A nil
// Allow or AllowRecord lets everything through.
type Handler struct {
	Allow       Limiter
	AllowRecord RecordLimiter
	Log         *slog.Logger
}

// Violation is one logged report.
type Violation struct {
	Directive  string
	BlockedURI string
	SourceFile string
	Line       int
	Sample     string
}

// ServeHTTP answers 204 when at least one record was written, 429 when the
// request limiter refuses or the record limiter refuses the first record,
// 413 when the body is over MaxBodyBytes, 415 for another content type and
// 400 for a malformed body. Reports refused by either limiter are neither
// logged nor counted. The dropped-count record covers only reports past
// maxRecordsPerRequest, and is written only while the record limiter
// allows it.
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
	var violations []Violation
	dropped := 0
	if mediaType == contentTypeReports {
		violations, dropped, err = parseReports(body)
	} else {
		violations, err = parseCSPReport(body)
	}
	if err != nil {
		http.Error(w, "malformed report", http.StatusBadRequest)
		return
	}
	log := h.Log
	if log == nil {
		log = slog.Default()
	}
	for i, v := range violations {
		if ok, retry := h.allowRecord(); !ok {
			if i == 0 {
				tooMany(w, retry)
				return
			}
			break
		}
		log.LogAttrs(r.Context(), slog.LevelInfo, LogMessage,
			slog.String("directive", v.Directive),
			slog.String("blocked_uri", v.BlockedURI),
			slog.String("source_file", v.SourceFile),
			slog.Int("line", v.Line),
			slog.String("sample", v.Sample),
		)
	}
	if dropped > 0 {
		if ok, _ := h.allowRecord(); ok {
			log.LogAttrs(r.Context(), slog.LevelInfo, DroppedLogMessage, slog.Int("dropped", dropped))
		}
	}
	w.WriteHeader(http.StatusNoContent)
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

// reportingAPIReport is one entry of an application/reports+json body.
type reportingAPIReport struct {
	Type string `json:"type"`
	Body *struct {
		EffectiveDirective string `json:"effectiveDirective"`
		BlockedURL         string `json:"blockedURL"`
		SourceFile         string `json:"sourceFile"`
		LineNumber         int    `json:"lineNumber"`
		Sample             string `json:"sample"`
	} `json:"body"`
}

// legacyCSPReport is an application/csp-report body.
type legacyCSPReport struct {
	Report *struct {
		EffectiveDirective string `json:"effective-directive"`
		ViolatedDirective  string `json:"violated-directive"`
		BlockedURI         string `json:"blocked-uri"`
		SourceFile         string `json:"source-file"`
		LineNumber         int    `json:"line-number"`
		ScriptSample       string `json:"script-sample"`
	} `json:"csp-report"`
}

var errMalformed = errors.New("malformed report")

// parseReports reads a Reporting API body. Entries of other report types
// are skipped; a body with no policy report in it is malformed. Policy
// reports past maxRecordsPerRequest are counted in dropped, not returned.
func parseReports(body []byte) (out []Violation, dropped int, err error) {
	var reports []reportingAPIReport
	if err := json.Unmarshal(body, &reports); err != nil {
		return nil, 0, errMalformed
	}
	for _, rep := range reports {
		if rep.Type != reportTypeCSP || rep.Body == nil {
			continue
		}
		if len(out) == maxRecordsPerRequest {
			dropped++
			continue
		}
		b := rep.Body
		out = append(out, newViolation(b.EffectiveDirective, b.BlockedURL, b.SourceFile, b.LineNumber, b.Sample))
	}
	if len(out) == 0 {
		return nil, 0, errMalformed
	}
	return out, dropped, nil
}

// parseCSPReport reads an application/csp-report body.
func parseCSPReport(body []byte) ([]Violation, error) {
	var rep legacyCSPReport
	if err := json.Unmarshal(body, &rep); err != nil || rep.Report == nil {
		return nil, errMalformed
	}
	b := rep.Report
	directive := b.EffectiveDirective
	if directive == "" {
		directive = b.ViolatedDirective
	}
	return []Violation{newViolation(directive, b.BlockedURI, b.SourceFile, b.LineNumber, b.ScriptSample)}, nil
}

// newViolation bounds every field of a record. URLs keep their scheme,
// host and path; the query and fragment are not needed to find the code
// that caused a report.
func newViolation(directive, blockedURI, sourceFile string, line int, sample string) Violation {
	if line < 0 {
		line = 0
	}
	return Violation{
		Directive:  truncateBytes(directive, maxFieldBytes),
		BlockedURI: truncateBytes(withoutQuery(blockedURI), maxFieldBytes),
		SourceFile: truncateBytes(withoutQuery(sourceFile), maxFieldBytes),
		Line:       line,
		Sample:     truncateRunes(sample, MaxSampleRunes),
	}
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
