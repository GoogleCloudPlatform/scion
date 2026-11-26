/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// APIOutcome is the result of one model API attempt, as a native usage
// rule recognized it: claude_code api_request/api_error, gemini_cli
// api_response/api_error, codex sse_event response.completed (success or
// failure) and a failed codex.api_request (ptone/scion#7858 design §4.2
// (2)). It is a per-attempt observation: a retried attempt produces its own
// outcome.
type APIOutcome struct {
	// At is the record's time (time_unix_nano, else
	// observed_time_unix_nano), or when it was received when it has none.
	At time.Time
	// OK is true for a successful attempt.
	OK bool
	// Status is the HTTP status the attempt reported, 0 when none.
	Status int
	// Message is the failure's error text as the harness reported it,
	// untrusted and unredacted, capped at apiOutcomeMessageMaxRunes.
	// Empty for a success. A consumer that stores or displays it must
	// redact it first.
	Message string
	// Model is the model attribute, if any.
	Model string
}

// APIOutcomeObserver receives each APIOutcome the usage deriver derives. It
// is called from the OTLP receiver's request goroutines, possibly
// concurrently, and must not block.
type APIOutcomeObserver func(APIOutcome)

// apiOutcomeMessageMaxRunes bounds APIOutcome.Message. The harness_error
// reason it feeds is capped at 300 runes anyway.
const apiOutcomeMessageMaxRunes = 1024

// apiOutcomeStatusAttrs are the attributes a status code is read from, in
// order: claude_code and gemini_cli use status_code, codex
// http.response.status_code, and gemini_cli also sets http.status_code.
var apiOutcomeStatusAttrs = []string{"status_code", "http.response.status_code", "http.status_code"}

// apiOutcomeMessageAttrs are the attributes a failure's text is read from,
// in order: codex and gemini_cli set error.message, claude_code error,
// grok-build error_category.
var apiOutcomeMessageAttrs = []string{"error.message", "error", "error_category"}

// apiOutcomeFromRecord builds the outcome of a record a usage rule matched
// as increment.
func apiOutcomeFromRecord(record *logspb.LogRecord, increment usageIncrement, received time.Time) APIOutcome {
	outcome := APIOutcome{
		At:    received,
		OK:    increment.Status == telemetrycontract.StatusSuccess,
		Model: increment.Model,
	}
	if t := record.GetTimeUnixNano(); t != 0 {
		outcome.At = time.Unix(0, int64(t))
	} else if t := record.GetObservedTimeUnixNano(); t != 0 {
		outcome.At = time.Unix(0, int64(t))
	}
	for _, key := range apiOutcomeStatusAttrs {
		if !logAttrPresent(record.Attributes, key) {
			continue
		}
		if n, err := logAttrInt(record.Attributes, key); err == nil && n > 0 && n < 1000 {
			outcome.Status = int(n)
			break
		}
	}
	if !outcome.OK {
		for _, key := range apiOutcomeMessageAttrs {
			if msg := strings.TrimSpace(logAttrString(record.Attributes, key)); msg != "" {
				outcome.Message = capRunes(providerErrorMessage(msg), apiOutcomeMessageMaxRunes)
				break
			}
		}
	}
	return outcome
}

// providerErrorMessage returns the provider's own message when msg is a
// JSON error body ({"error":{"message":...}}, as gemini_cli reports it),
// surrounding whitespace allowed, and msg unchanged otherwise.
func providerErrorMessage(msg string) string {
	trimmed := strings.TrimSpace(msg)
	if !strings.HasPrefix(trimmed, "{") {
		return msg
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(trimmed), &body); err != nil {
		return msg
	}
	if m := strings.TrimSpace(body.Error.Message); m != "" {
		return m
	}
	return msg
}

// capRunes cuts s to at most n runes.
func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// SetAPIOutcomeObserver sets the observer that receives each model API
// outcome the native usage deriver derives. A nil observer stops
// forwarding. Safe to call at any time, including before or after the
// usage deriver is created or activated. Outcomes are derived only when
// native usage derivation is active (SCION_USAGE_SOURCE=native, a harness
// with a log rule), that is when the harness's telemetry is enabled.
func (p *Pipeline) SetAPIOutcomeObserver(observer APIOutcomeObserver) {
	if p == nil {
		return
	}
	if observer == nil {
		p.apiOutcomeObserver.Store(nil)
		return
	}
	p.apiOutcomeObserver.Store(&observer)
}

// forwardAPIOutcome passes o to the current observer, if any. It is the
// UsageDeriver's apiOutcome hook for derivers this pipeline creates.
func (p *Pipeline) forwardAPIOutcome(o APIOutcome) {
	if observer := p.apiOutcomeObserver.Load(); observer != nil {
		(*observer)(o)
	}
}
