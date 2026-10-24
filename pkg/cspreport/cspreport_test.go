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

package cspreport

import (
	"bytes"
	"encoding/json"
	"go/build"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recordingLog returns a logger whose JSON records land in buf.
func recordingLog(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// records decodes every JSON record written to buf.
func records(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := json.NewDecoder(buf)
	for dec.More() {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			t.Fatalf("decoding log record: %v", err)
		}
		out = append(out, rec)
	}
	return out
}

func post(h http.Handler, contentType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/csp-reports", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const reportingAPIBody = `[{"type":"csp-violation","age":10,"url":"https://hub.example/projects","user_agent":"UA",
"body":{"documentURL":"https://hub.example/projects","effectiveDirective":"require-trusted-types-for",
"blockedURL":"trusted-types-sink","sourceFile":"https://hub.example/assets/main.js?v=1#frag",
"lineNumber":42,"columnNumber":7,"sample":"Element innerHTML|<b>x</b>","disposition":"report"}}]`

const legacyBody = `{"csp-report":{"document-uri":"https://hub.example/","violated-directive":"script-src-elem",
"effective-directive":"script-src-elem","blocked-uri":"inline","source-file":"https://hub.example/",
"line-number":9,"script-sample":"alert(1)","disposition":"enforce"}}`

func TestReportingAPIReportIsLogged(t *testing.T) {
	var buf bytes.Buffer
	h := &Handler{Log: recordingLog(&buf)}
	rec := post(h, "application/reports+json", reportingAPIBody)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body %q", rec.Code, rec.Body.String())
	}
	got := records(t, &buf)
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1: %v", len(got), got)
	}
	r := got[0]
	want := map[string]any{
		"msg":         "csp_violation",
		"level":       "INFO",
		"directive":   "require-trusted-types-for",
		"blocked_uri": "trusted-types-sink",
		"source_file": "https://hub.example/assets/main.js",
		"line":        float64(42),
		"sample":      "Element innerHTML|<b>x</b>",
	}
	for k, v := range want {
		if r[k] != v {
			t.Errorf("record[%q] = %v, want %v", k, r[k], v)
		}
	}
}

func TestLegacyReportIsLogged(t *testing.T) {
	var buf bytes.Buffer
	h := &Handler{Log: recordingLog(&buf)}
	rec := post(h, "application/csp-report", legacyBody)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body %q", rec.Code, rec.Body.String())
	}
	got := records(t, &buf)
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1: %v", len(got), got)
	}
	r := got[0]
	want := map[string]any{
		"msg":         "csp_violation",
		"directive":   "script-src-elem",
		"blocked_uri": "inline",
		"source_file": "https://hub.example/",
		"line":        float64(9),
		"sample":      "alert(1)",
	}
	for k, v := range want {
		if r[k] != v {
			t.Errorf("record[%q] = %v, want %v", k, r[k], v)
		}
	}
}

func TestLegacyReportFallsBackToViolatedDirective(t *testing.T) {
	var buf bytes.Buffer
	h := &Handler{Log: recordingLog(&buf)}
	rec := post(h, "application/csp-report", `{"csp-report":{"violated-directive":"script-src","blocked-uri":"eval"}}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	got := records(t, &buf)
	if len(got) != 1 || got[0]["directive"] != "script-src" {
		t.Fatalf("records = %v, want one with directive script-src", got)
	}
}

func TestSampleIsTruncated(t *testing.T) {
	if MaxSampleRunes != 120 {
		t.Fatalf("MaxSampleRunes = %d, want 120", MaxSampleRunes)
	}
	// Multi-byte runes, so a byte cut would differ from a rune cut.
	long := strings.Repeat("é", MaxSampleRunes+30)
	body := `[{"type":"csp-violation","body":{"effectiveDirective":"script-src","sample":"` + long + `"}}]`
	var buf bytes.Buffer
	h := &Handler{Log: recordingLog(&buf)}
	if rec := post(h, "application/reports+json", body); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	got := records(t, &buf)
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	sample, _ := got[0]["sample"].(string)
	if want := strings.Repeat("é", MaxSampleRunes); sample != want {
		t.Errorf("sample has %d runes, want exactly %d", len([]rune(sample)), MaxSampleRunes)
	}

	// Positive control: a sample at the limit is kept whole.
	buf.Reset()
	exact := strings.Repeat("é", MaxSampleRunes)
	body = `[{"type":"csp-violation","body":{"effectiveDirective":"script-src","sample":"` + exact + `"}}]`
	if rec := post(h, "application/reports+json", body); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if got := records(t, &buf); len(got) != 1 || got[0]["sample"] != exact {
		t.Errorf("a sample of exactly %d runes was changed", MaxSampleRunes)
	}
}

func TestLongFieldsAreBounded(t *testing.T) {
	long := "https://hub.example/" + strings.Repeat("a", 2*maxFieldBytes)
	body := `[{"type":"csp-violation","body":{"effectiveDirective":"` + strings.Repeat("d", 2*maxFieldBytes) +
		`","blockedURL":"` + long + `","sourceFile":"` + long + `"}}]`
	var buf bytes.Buffer
	h := &Handler{Log: recordingLog(&buf)}
	if rec := post(h, "application/reports+json", body); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	got := records(t, &buf)
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	for _, k := range []string{"directive", "blocked_uri", "source_file"} {
		s, _ := got[0][k].(string)
		if len(s) != maxFieldBytes {
			t.Errorf("%s has %d bytes, want %d", k, len(s), maxFieldBytes)
		}
	}
}

func TestBodyCap(t *testing.T) {
	const cap16KiB = 16 * 1024
	if MaxBodyBytes != cap16KiB {
		t.Fatalf("MaxBodyBytes = %d, want 16 KiB (%d)", MaxBodyBytes, cap16KiB)
	}
	// A well-formed report padded with JSON whitespace to one byte over
	// the cap, so only its size can be the reason for the refusal.
	overCap := reportingAPIBody + strings.Repeat(" ", cap16KiB+1-len(reportingAPIBody))
	var buf bytes.Buffer
	h := &Handler{Log: recordingLog(&buf)}
	rec := post(h, "application/reports+json", overCap)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the cap: status = %d, want 413", rec.Code)
	}
	if got := records(t, &buf); len(got) != 0 {
		t.Fatalf("over the cap: %d records logged, want none", len(got))
	}

	// Positive control: the same report padded to exactly the cap is
	// accepted.
	atCap := reportingAPIBody + strings.Repeat(" ", cap16KiB-len(reportingAPIBody))
	if len(atCap) != cap16KiB || len(overCap) != cap16KiB+1 {
		t.Fatalf("test setup: bodies are %d and %d bytes", len(atCap), len(overCap))
	}
	rec = post(h, "application/reports+json", atCap)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("at the cap: status = %d, want 204", rec.Code)
	}
	if got := records(t, &buf); len(got) != 1 {
		t.Fatalf("at the cap: %d records logged, want 1", len(got))
	}
}

func TestMalformedReports(t *testing.T) {
	cases := []struct {
		name, contentType, body string
		want                    int
	}{
		{"not json", "application/reports+json", "{not json", http.StatusBadRequest},
		{"object where an array is expected", "application/reports+json", `{"type":"csp-violation"}`, http.StatusBadRequest},
		{"empty array", "application/reports+json", `[]`, http.StatusBadRequest},
		{"only other report types", "application/reports+json", `[{"type":"deprecation","body":{}}]`, http.StatusBadRequest},
		{"csp report without body", "application/reports+json", `[{"type":"csp-violation"}]`, http.StatusBadRequest},
		{"legacy without csp-report member", "application/csp-report", `{"other":{}}`, http.StatusBadRequest},
		{"legacy not json", "application/csp-report", `]`, http.StatusBadRequest},
		{"wrong field type", "application/csp-report", `{"csp-report":{"line-number":"nine"}}`, http.StatusBadRequest},
		{"plain json content type", "application/json", reportingAPIBody, http.StatusUnsupportedMediaType},
		{"no content type", "", reportingAPIBody, http.StatusUnsupportedMediaType},
		{"shapes swapped", "application/csp-report", reportingAPIBody, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := &Handler{Log: recordingLog(&buf)}
			rec := post(h, tc.contentType, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if got := records(t, &buf); len(got) != 0 {
				t.Fatalf("%d records logged, want none", len(got))
			}
		})
	}

	// Positive control: both shapes with a charset parameter are accepted.
	for _, ct := range []struct{ contentType, body string }{
		{"application/reports+json; charset=utf-8", reportingAPIBody},
		{"application/csp-report; charset=utf-8", legacyBody},
	} {
		var buf bytes.Buffer
		h := &Handler{Log: recordingLog(&buf)}
		if rec := post(h, ct.contentType, ct.body); rec.Code != http.StatusNoContent {
			t.Errorf("%s: status = %d, want 204", ct.contentType, rec.Code)
		}
		if got := records(t, &buf); len(got) != 1 {
			t.Errorf("%s: %d records logged, want 1", ct.contentType, len(got))
		}
	}
}

func TestMixedBatchLogsOnlyPolicyReports(t *testing.T) {
	body := `[{"type":"deprecation","body":{"id":"x"}},` + strings.TrimPrefix(reportingAPIBody, "[")
	var buf bytes.Buffer
	h := &Handler{Log: recordingLog(&buf)}
	if rec := post(h, "application/reports+json", body); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if got := records(t, &buf); len(got) != 1 {
		t.Fatalf("%d records logged, want 1", len(got))
	}
}

func TestRecordsPerRequestAreBounded(t *testing.T) {
	var buf bytes.Buffer
	h := &Handler{Log: recordingLog(&buf)}
	if rec := post(h, "application/reports+json", manyReports(maxRecordsPerRequest+5)); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	got := records(t, &buf)
	if len(got) != maxRecordsPerRequest+1 {
		t.Fatalf("%d records logged, want %d reports and one dropped count", len(got), maxRecordsPerRequest)
	}
	for _, r := range got[:maxRecordsPerRequest] {
		if r["msg"] != LogMessage {
			t.Fatalf("record %v, want %s", r, LogMessage)
		}
	}
	last := got[maxRecordsPerRequest]
	if last["msg"] != DroppedLogMessage || last["dropped"] != float64(5) {
		t.Fatalf("last record = %v, want %s with dropped=5", last, DroppedLogMessage)
	}

	// Positive control: a batch under the bound is logged in full, with no
	// dropped count.
	if rec := post(h, "application/reports+json", manyReports(3)); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	got = records(t, &buf)
	if len(got) != 3 {
		t.Fatalf("%d records logged, want 3", len(got))
	}
	for _, r := range got {
		if r["msg"] != LogMessage {
			t.Fatalf("record %v, want only %s records", r, LogMessage)
		}
	}
}

func TestLimiterRefusalAnswers429(t *testing.T) {
	var buf bytes.Buffer
	allow := false
	calls := 0
	h := &Handler{
		Log: recordingLog(&buf),
		Allow: func(*http.Request) (bool, int) {
			calls++
			return allow, 7
		},
	}
	rec := post(h, "application/reports+json", reportingAPIBody)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("refused: status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After = %q, want 7", got)
	}
	if got := records(t, &buf); len(got) != 0 {
		t.Fatalf("refused: %d records logged, want none", len(got))
	}

	// Positive control: the same request is logged once the limiter allows.
	allow = true
	if rec := post(h, "application/reports+json", reportingAPIBody); rec.Code != http.StatusNoContent {
		t.Fatalf("allowed: status = %d, want 204", rec.Code)
	}
	if got := records(t, &buf); len(got) != 1 {
		t.Fatalf("allowed: %d records logged, want 1", len(got))
	}
	if calls != 2 {
		t.Errorf("limiter consulted %d times, want 2", calls)
	}
}

// budget is a RecordLimiter with n tokens that counts how often it is asked.
type budget struct{ left, asked int }

func (b *budget) allow() (bool, int) {
	b.asked++
	if b.left == 0 {
		return false, 3
	}
	b.left--
	return true, 0
}

// manyReports is a Reporting API body with n policy reports.
func manyReports(n int) string {
	one := `{"type":"csp-violation","body":{"effectiveDirective":"script-src"}}`
	parts := make([]string, n)
	for i := range parts {
		parts[i] = one
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// TestRecordLimiterChargedPerRecord: the record limiter is charged once
// per record written, the dropped-count record included.
func TestRecordLimiterChargedPerRecord(t *testing.T) {
	var buf bytes.Buffer
	b := &budget{left: 100}
	h := &Handler{Log: recordingLog(&buf), AllowRecord: b.allow}
	if rec := post(h, "application/reports+json", manyReports(maxRecordsPerRequest+5)); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	got := records(t, &buf)
	if len(got) != maxRecordsPerRequest+1 {
		t.Fatalf("%d records written, want %d", len(got), maxRecordsPerRequest+1)
	}
	if b.asked != maxRecordsPerRequest+1 || b.left != 100-(maxRecordsPerRequest+1) {
		t.Fatalf("limiter asked %d times, %d tokens left; want %d charges", b.asked, b.left, maxRecordsPerRequest+1)
	}
}

// TestRecordLimiterStopsMidRequest: when the record limiter runs out
// during a request, the rest is not written, and with no token left the
// dropped-count record is not written either (TestRecordLimiterChargedPerRecord
// is the positive control for that record).
func TestRecordLimiterStopsMidRequest(t *testing.T) {
	var buf bytes.Buffer
	b := &budget{left: 3}
	h := &Handler{Log: recordingLog(&buf), AllowRecord: b.allow}
	if rec := post(h, "application/reports+json", manyReports(5)); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	got := records(t, &buf)
	if len(got) != 3 {
		t.Fatalf("%d records written, want 3", len(got))
	}
	for _, r := range got {
		if r["msg"] != LogMessage {
			t.Fatalf("record %v, want only %s records", r, LogMessage)
		}
	}

	if b.asked != 4 {
		t.Errorf("limiter asked %d times, want 4 (three records and the refused fourth; refused reports are not counted, so no dropped-count record is attempted)", b.asked)
	}
}

// TestRecordLimiterRefusingFirstRecordAnswers429: no record can be written,
// so the request is refused like an over-limit request.
func TestRecordLimiterRefusingFirstRecordAnswers429(t *testing.T) {
	var buf bytes.Buffer
	b := &budget{left: 0}
	h := &Handler{Log: recordingLog(&buf), AllowRecord: b.allow}
	rec := post(h, "application/reports+json", reportingAPIBody)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Errorf("Retry-After = %q, want 3", got)
	}
	if got := records(t, &buf); len(got) != 0 {
		t.Fatalf("%d records written, want none", len(got))
	}
	// Positive control: with a token, the same report is written.
	b.left = 1
	if rec := post(h, "application/reports+json", reportingAPIBody); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if got := records(t, &buf); len(got) != 1 {
		t.Fatalf("%d records written, want 1", len(got))
	}
}

func TestOnlyPostIsAccepted(t *testing.T) {
	h := &Handler{Log: recordingLog(&bytes.Buffer{})}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/csp-reports", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow = %q, want POST", got)
	}
}

// TestPackageStoresNothing pins that the report endpoint has no way to
// store a report: this package imports no database, file system or
// storage package, only the standard library pieces it needs to read a
// request and log. Logging is its only output.
func TestPackageStoresNothing(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("reading package imports: %v", err)
	}
	allowed := map[string]bool{
		"encoding/json": true, "errors": true, "io": true, "log/slog": true, "mime": true,
		"net/http": true, "strconv": true, "strings": true, "unicode": true, "unicode/utf8": true,
	}
	if len(pkg.Imports) == 0 {
		t.Fatal("no imports read; the check would pass vacuously")
	}
	for _, imp := range pkg.Imports {
		if !allowed[imp] {
			t.Errorf("unexpected import %q: the report endpoint must only log", imp)
		}
	}
}

// TestRecordsHoldPrintableTextOnly: characters that are not graphic (bidi
// overrides, zero-width characters, C0 and C1 controls, the line
// separator, private use) in report fields are replaced by U+FFFD;
// ordinary non-ASCII text and spaces are kept.
func TestRecordsHoldPrintableTextOnly(t *testing.T) {
	// The \u sequences are decoded by the JSON parser, so the fields hold
	// the characters themselves.
	body := `[{"type":"csp-violation","body":{"effectiveDirective":"script-src",` +
		`"sourceFile":"https://hub.example/a\u202eb.js","blockedURL":"x\u200by",` +
		`"sample":"caf\u00e9 \u65e5\u672c \u0007\u0085\u2066\u2028\ue000z"}}]`
	var buf bytes.Buffer
	h := &Handler{Log: recordingLog(&buf)}
	if rec := post(h, "application/reports+json", body); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	got := records(t, &buf)
	if len(got) != 1 {
		t.Fatalf("%d records, want 1", len(got))
	}
	want := map[string]string{
		"source_file": "https://hub.example/a\uFFFDb.js",
		"blocked_uri": "x\uFFFDy",
		// Positive control: the accented and CJK text is kept as is.
		"sample": "caf\u00e9 \u65e5\u672c \uFFFD\uFFFD\uFFFD\uFFFD\uFFFDz",
	}
	for k, v := range want {
		if got[0][k] != v {
			t.Errorf("%s = %q, want %q", k, got[0][k], v)
		}
	}
}
