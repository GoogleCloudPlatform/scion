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
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// safariReportToBody is a single report as Safari sends it to a report-to
// endpoint: one object with "type", "url" and "body" (no array), sent
// with Content-Type application/csp-report. The body members and their
// order are WebKit's (CSPViolationReportBody); a Trusted Types sink report
// has an empty sample and no source file.
const safariReportToBody = `{"type":"csp-violation","url":"https://hub.example/projects/p1/chat/c1",` +
	`"body":{"documentURL":"https://hub.example/projects/p1/chat/c1","disposition":"report","referrer":"",` +
	`"effectiveDirective":"require-trusted-types-for","blockedURL":"trusted-types-sink",` +
	`"originalPolicy":"require-trusted-types-for 'script'; report-to csp","statusCode":200,"sample":""}}`

// safariReportURIBody is the older form as Safari sends it to a report-uri
// endpoint (WebKit's member names and order).
const safariReportURIBody = `{"type":"csp-violation","url":"https://hub.example/projects/p1/chat/c1",` +
	`"csp-report":{"document-uri":"https://hub.example/projects/p1/chat/c1","referrer":"",` +
	`"violated-directive":"img-src","effective-directive":"img-src","original-policy":"img-src 'self'",` +
	`"blocked-uri":"https://cdn.example/a.png","status-code":200,` +
	`"source-file":"https://hub.example/assets/main.js","line-number":3,"column-number":9}}`

// reportingAPIArrayBody is a Reporting API array as Chromium sends it.
const reportingAPIArrayBody = `[{"type":"csp-violation","age":10,"url":"https://hub.example/projects/p1/chat/c1",` +
	`"user_agent":"UA","body":{"documentURL":"https://hub.example/projects/p1/chat/c1",` +
	`"effectiveDirective":"require-trusted-types-for","blockedURL":"trusted-types-sink",` +
	`"sourceFile":"https://hub.example/assets/main.js","lineNumber":42,"columnNumber":7,` +
	`"sample":"Element innerHTML|<b>x</b>","disposition":"report","statusCode":200}}]`

// TestEveryReportShapeIsAccepted: each body shape is logged with either
// report content type, since browsers do not agree on the pairing.
func TestEveryReportShapeIsAccepted(t *testing.T) {
	shapes := []struct {
		name, body, directive string
	}{
		{"single report-to object", safariReportToBody, "require-trusted-types-for"},
		{"older form", safariReportURIBody, "img-src"},
		{"older form without type", legacyBody, "script-src-elem"},
		{"Reporting API array", reportingAPIArrayBody, "require-trusted-types-for"},
	}
	for _, shape := range shapes {
		for _, contentType := range []string{"application/csp-report", "application/reports+json"} {
			t.Run(shape.name+"/"+contentType, func(t *testing.T) {
				var buf bytes.Buffer
				h := &Handler{Log: recordingLog(&buf)}
				if rec := post(h, contentType, shape.body); rec.Code != http.StatusNoContent {
					t.Fatalf("status = %d, want 204; body %q", rec.Code, rec.Body.String())
				}
				got := records(t, &buf)
				if len(got) != 1 {
					t.Fatalf("%d records, want 1", len(got))
				}
				if got[0]["msg"] != LogMessage || got[0]["directive"] != shape.directive {
					t.Fatalf("record = %v, want %s with directive %q", got[0], LogMessage, shape.directive)
				}
			})
		}
	}
}

// TestSafariReportToFields: the fields of Safari's single report-to object
// reach the record.
func TestSafariReportToFields(t *testing.T) {
	var buf bytes.Buffer
	h := &Handler{Log: recordingLog(&buf)}
	if rec := post(h, "application/csp-report", safariReportToBody); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	got := records(t, &buf)
	if len(got) != 1 {
		t.Fatalf("%d records, want 1", len(got))
	}
	want := map[string]any{
		"directive":     "require-trusted-types-for",
		"blocked_uri":   "trusted-types-sink",
		"source_file":   "",
		"line":          float64(0),
		"document_path": "/projects/p1/chat/c1",
	}
	for k, v := range want {
		if got[0][k] != v {
			t.Errorf("%s = %v, want %v", k, got[0][k], v)
		}
	}
	if _, ok := got[0]["repeats"]; ok {
		t.Errorf("repeats on a first record: %v", got[0])
	}
}

// TestDocumentPathIsPathOnly: the page is logged as its path; the host,
// user information, query and fragment are not logged.
func TestDocumentPathIsPathOnly(t *testing.T) {
	const marker = "Secret7c1d"
	bodies := map[string]string{
		"single report-to object": `{"type":"csp-violation","body":{"effectiveDirective":"img-src",` +
			`"documentURL":"https://user:` + marker + `@hub.example/projects/p1/chat/c1?token=` + marker + `#` + marker + `"}}`,
		"older form": `{"csp-report":{"effective-directive":"img-src",` +
			`"document-uri":"https://hub.example/projects/p1/chat/c1?token=` + marker + `#` + marker + `"}}`,
		"report url when the body has none": `[{"type":"csp-violation","url":"https://hub.example/projects/p1/chat/c1?q=` + marker + `",` +
			`"body":{"effectiveDirective":"img-src"}}]`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			h := &Handler{Log: recordingLog(&buf)}
			if rec := post(h, "application/csp-report", body); rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", rec.Code)
			}
			out := buf.String()
			got := records(t, bytes.NewBufferString(out))
			if len(got) != 1 || got[0]["document_path"] != "/projects/p1/chat/c1" {
				t.Fatalf("records = %v, want one with document_path /projects/p1/chat/c1", got)
			}
			if strings.Contains(out, marker) || strings.Contains(out, "hub.example") {
				t.Fatalf("log holds the host or the query: %s", out)
			}
		})
	}
}

func TestDocumentPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://hub.example/projects/p1/chat/c1", "/projects/p1/chat/c1"},
		{"https://hub.example", "/"},
		{"https://hub.example/", "/"},
		{"https://hub.example/a?b=c#d", "/a"},
		{"https://u:p@hub.example/a", "/a"},
		{"//hub.example/a?b", "/a"},
		{"about:blank", "about:"},
		{"data:text/html,<b>x</b>", "data:"},
		{"blob:https://hub.example/1234", "blob:"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := documentPath(tc.in); got != tc.want {
			t.Errorf("documentPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestURLOrigin(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://cdn.example/a.png", "https://cdn.example"},
		{"https://cdn.example", "https://cdn.example"},
		{"wss://hub.example:8443/ws", "wss://hub.example:8443"},
		{"//cdn.example/a", "//cdn.example"},
		{"trusted-types-sink", "trusted-types-sink"},
		{"inline", "inline"},
		{"data:", "data:"},
		{"blob:", "blob:"},
		{"/relative/path", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := urlOrigin(tc.in); got != tc.want {
			t.Errorf("urlOrigin(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// fakeClock is a settable clock for a Dedupe.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// dedupeHandler returns a Handler with a Dedupe on clock, logging to buf,
// and a pointer to the number of times the record limiter was asked.
func dedupeHandler(buf *bytes.Buffer, clock *fakeClock, interval time.Duration, maxKeys int) (*Handler, *int) {
	d := NewDedupe(interval, maxKeys)
	d.now = clock.now
	asked := 0
	return &Handler{
		Log:    recordingLog(buf),
		Dedupe: d,
		AllowRecord: func() (bool, int) {
			asked++
			return true, 0
		},
	}, &asked
}

// TestRepeatsAreCollapsed: repeats of one violation within the interval
// answer 204 and are not logged or charged to the record limiter; after
// the interval a summary record counts them and shows the latest one, and
// the next report is logged again.
func TestRepeatsAreCollapsed(t *testing.T) {
	var buf bytes.Buffer
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	h, asked := dedupeHandler(&buf, clock, 10*time.Minute, 16)

	for i := 0; i < 50; i++ {
		if rec := post(h, "application/csp-report", safariReportToBody); rec.Code != http.StatusNoContent {
			t.Fatalf("report %d: status = %d, want 204", i+1, rec.Code)
		}
	}
	if got := records(t, &buf); len(got) != 1 {
		t.Fatalf("%d records for 50 identical reports, want 1", len(got))
	}
	if *asked != 1 {
		t.Fatalf("record limiter asked %d times, want 1", *asked)
	}

	// The same violation in a Reporting API array is a repeat too.
	if rec := post(h, "application/reports+json", reportingAPIArrayBody); rec.Code != http.StatusNoContent {
		t.Fatalf("array: status = %d, want 204", rec.Code)
	}
	if got := records(t, &buf); len(got) != 0 {
		t.Fatalf("%d records for a repeat in an array, want 0", len(got))
	}

	// Positive control: another page is another violation.
	other := strings.ReplaceAll(safariReportToBody, "/chat/c1", "/chat/c2")
	if rec := post(h, "application/csp-report", other); rec.Code != http.StatusNoContent {
		t.Fatalf("other page: status = %d, want 204", rec.Code)
	}
	if got := records(t, &buf); len(got) != 1 || got[0]["document_path"] != "/projects/p1/chat/c2" {
		t.Fatalf("records = %v, want one for /projects/p1/chat/c2", got)
	}

	clock.t = clock.t.Add(10 * time.Minute)
	if rec := post(h, "application/csp-report", safariReportToBody); rec.Code != http.StatusNoContent {
		t.Fatalf("after the interval: status = %d, want 204", rec.Code)
	}
	got := records(t, &buf)
	if len(got) != 2 {
		t.Fatalf("records after the interval = %v, want a summary and a record", got)
	}
	// The summary: 49 single-object repeats and the array one; the latest
	// repeat is the array report, so its fields are shown.
	sum := got[0]
	if sum["msg"] != RepeatsLogMessage || sum["repeats"] != float64(50) ||
		sum["document_path"] != "/projects/p1/chat/c1" || sum["sink"] != "Element innerHTML" || sum["line"] != float64(42) {
		t.Fatalf("summary = %v, want %s with repeats=50 and the array report's fields", sum, RepeatsLogMessage)
	}
	if got[1]["msg"] != LogMessage || got[1]["line"] != float64(0) {
		t.Fatalf("record after the summary = %v, want the new report", got[1])
	}
}

// TestSummaryShowsFirstAndLatest: when the first report of a violation
// differs from the later ones in the fields outside the key, the records
// show both: the record holds the first, the summary the latest, with when
// each was seen.
func TestSummaryShowsFirstAndLatest(t *testing.T) {
	var buf bytes.Buffer
	start := time.Unix(1_700_000_000, 0).UTC()
	clock := &fakeClock{t: start}
	h, _ := dedupeHandler(&buf, clock, 10*time.Minute, 16)
	report := func(path string, line int) string {
		return fmt.Sprintf(`{"type":"csp-violation","body":{"documentURL":"https://hub.example/p",`+
			`"effectiveDirective":"img-src","blockedURL":"https://cdn.example/%s","sourceFile":"https://hub.example/m.js","lineNumber":%d}}`, path, line)
	}
	post(h, "application/csp-report", report("first.png", 1))
	for i := 0; i < 3; i++ {
		clock.t = clock.t.Add(time.Minute)
		post(h, "application/csp-report", report("later.png", 7))
	}
	latest := clock.t
	clock.t = start.Add(10 * time.Minute)
	// Another violation's report (another directive): it finds the first
	// one's interval over.
	post(h, "application/csp-report", strings.Replace(report("x", 1), `"img-src"`, `"font-src"`, 1))
	got := records(t, &buf)
	if len(got) != 3 || got[0]["blocked_uri"] != "https://cdn.example/first.png" || got[0]["line"] != float64(1) || got[2]["directive"] != "font-src" {
		t.Fatalf("records = %v, want the first report's record first", got)
	}
	sum := got[1]
	if sum["msg"] != RepeatsLogMessage || sum["repeats"] != float64(3) ||
		sum["blocked_uri"] != "https://cdn.example/later.png" || sum["line"] != float64(7) ||
		sum["first_logged"] != start.Format(time.RFC3339) || sum["latest_seen"] != latest.Format(time.RFC3339) {
		t.Fatalf("summary = %v, want repeats=3, the later report's fields, first_logged %s and latest_seen %s",
			sum, start.Format(time.RFC3339), latest.Format(time.RFC3339))
	}
}

// TestSummaryWithoutReturn: a violation that does not come back still gets
// its summary once its interval is over and any report arrives; one
// without repeats gets none.
func TestSummaryWithoutReturn(t *testing.T) {
	var buf bytes.Buffer
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	h, _ := dedupeHandler(&buf, clock, 10*time.Minute, 16)
	for i := 0; i < 4; i++ {
		post(h, "application/csp-report", safariReportToBody)
	}
	post(h, "application/csp-report", strings.ReplaceAll(safariReportToBody, "/chat/c1", "/chat/c2"))
	records(t, &buf)
	clock.t = clock.t.Add(10 * time.Minute)
	post(h, "application/csp-report", strings.ReplaceAll(safariReportToBody, "/chat/c1", "/chat/c3"))
	var sums []map[string]any
	for _, r := range records(t, &buf) {
		if r["msg"] == RepeatsLogMessage {
			sums = append(sums, r)
		}
	}
	if len(sums) != 1 || sums[0]["repeats"] != float64(3) || sums[0]["document_path"] != "/projects/p1/chat/c1" {
		t.Fatalf("summaries = %v, want one for /projects/p1/chat/c1 with repeats=3", sums)
	}
	if h.Dedupe.size() != 1 {
		t.Fatalf("%d entries tracked after the sweep, want 1", h.Dedupe.size())
	}
}

// TestSummaryOnEarlyRemoval: an entry removed early to make room still
// gets its summary, so no repeat goes uncounted.
func TestSummaryOnEarlyRemoval(t *testing.T) {
	var buf bytes.Buffer
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	h, _ := dedupeHandler(&buf, clock, 10*time.Minute, 1)
	post(h, "application/csp-report", safariReportToBody)
	post(h, "application/csp-report", safariReportToBody)
	post(h, "application/csp-report", strings.ReplaceAll(safariReportToBody, "/chat/c1", "/chat/c2"))
	got := records(t, &buf)
	if len(got) != 3 || got[1]["msg"] != RepeatsLogMessage || got[1]["repeats"] != float64(1) || got[2]["msg"] != LogMessage {
		t.Fatalf("records = %v, want a record, the removed entry's summary (repeats=1), then the new record", got)
	}
}

// TestSummaryLeavesSampleOut: with RawSamples on, a report record holds
// its sample, but a summary never holds the latest repeat's.
func TestSummaryLeavesSampleOut(t *testing.T) {
	const marker = "Secret41aa"
	var buf bytes.Buffer
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	h, _ := dedupeHandler(&buf, clock, 10*time.Minute, 16)
	h.RawSamples = true
	body := `{"type":"csp-violation","body":{"documentURL":"https://hub.example/a","effectiveDirective":"script-src",` +
		`"blockedURL":"inline","sample":"%s"}}`
	post(h, "application/csp-report", fmt.Sprintf(body, "first"))
	post(h, "application/csp-report", fmt.Sprintf(body, marker))
	clock.t = clock.t.Add(10 * time.Minute)
	post(h, "application/csp-report", fmt.Sprintf(body, "next"))
	out := buf.String()
	got := records(t, bytes.NewBufferString(out))
	if len(got) != 3 || got[0]["sample"] != "first" || got[1]["msg"] != RepeatsLogMessage || got[2]["sample"] != "next" {
		t.Fatalf("records = %v, want a record with its sample, a summary, a record with its sample", got)
	}
	if _, ok := got[1]["sample"]; ok || strings.Contains(out, marker) {
		t.Fatalf("a summary holds a sample: %s", out)
	}
}

// TestRequestBoundCountsAfterCollapsing: repeats in one request are
// collapsed before the per-request bound, so identical reports past it are
// not counted as dropped; distinct ones past it are.
func TestRequestBoundCountsAfterCollapsing(t *testing.T) {
	var buf bytes.Buffer
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	h, asked := dedupeHandler(&buf, clock, 10*time.Minute, 64)
	if rec := post(h, "application/reports+json", manyReports(maxRecordsPerRequest+5)); rec.Code != http.StatusNoContent {
		t.Fatalf("identical: status = %d, want 204", rec.Code)
	}
	if got := records(t, &buf); len(got) != 1 || got[0]["msg"] != LogMessage || *asked != 1 {
		t.Fatalf("identical: records %v and %d limiter calls, want one record and 1 call", got, *asked)
	}
	parts := make([]string, maxRecordsPerRequest+5)
	for i := range parts {
		parts[i] = fmt.Sprintf(`{"type":"csp-violation","body":{"effectiveDirective":"d%d"}}`, i)
	}
	if rec := post(h, "application/reports+json", "["+strings.Join(parts, ",")+"]"); rec.Code != http.StatusNoContent {
		t.Fatalf("distinct: status = %d, want 204", rec.Code)
	}
	got := records(t, &buf)
	if len(got) != maxRecordsPerRequest+1 || got[maxRecordsPerRequest]["msg"] != DroppedLogMessage || got[maxRecordsPerRequest]["dropped"] != float64(5) {
		t.Fatalf("distinct: %d records, want %d (%d reports and a dropped count of 5)", len(got), maxRecordsPerRequest+1, maxRecordsPerRequest)
	}
	if h.Dedupe.size() != 1+maxRecordsPerRequest {
		t.Fatalf("%d entries tracked, want %d (dropped reports are not tracked)", h.Dedupe.size(), 1+maxRecordsPerRequest)
	}
}

// TestRepeatsKeyOnBlockedOrigin: blocked URIs on one origin are the same
// violation; another origin, or another directive, is not.
func TestRepeatsKeyOnBlockedOrigin(t *testing.T) {
	var buf bytes.Buffer
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	h, _ := dedupeHandler(&buf, clock, 10*time.Minute, 16)
	report := func(directive, blocked string) string {
		return fmt.Sprintf(`{"type":"csp-violation","body":{"documentURL":"https://hub.example/",`+
			`"effectiveDirective":%q,"blockedURL":%q}}`, directive, blocked)
	}
	bodies := []string{
		report("img-src", "https://cdn.example/a.png"),
		report("img-src", "https://cdn.example/b.png"),
		report("img-src", "https://other.example/a.png"),
		report("connect-src", "https://cdn.example/a.png"),
	}
	for _, body := range bodies {
		if rec := post(h, "application/csp-report", body); rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
	}
	got := records(t, &buf)
	if len(got) != 3 {
		t.Fatalf("%d records, want 3 (b.png repeats a.png)", len(got))
	}
}

// TestRepeatsInOneRequestAreCollapsed: identical reports in one array give
// one record and 204.
func TestRepeatsInOneRequestAreCollapsed(t *testing.T) {
	var buf bytes.Buffer
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	h, asked := dedupeHandler(&buf, clock, 10*time.Minute, 16)
	if rec := post(h, "application/reports+json", manyReports(5)); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if got := records(t, &buf); len(got) != 1 || *asked != 1 {
		t.Fatalf("%d records and %d limiter calls, want 1 and 1", len(got), *asked)
	}
}

// TestDedupeIsBounded: however many violations arrive, at most maxKeys
// are tracked, each new one is still logged, and the entries kept are the
// newest ones (early removal is oldest first).
func TestDedupeIsBounded(t *testing.T) {
	var buf bytes.Buffer
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	const maxKeys = 4
	h, _ := dedupeHandler(&buf, clock, 10*time.Minute, maxKeys)
	const n = 100
	start := clock.t
	for i := 0; i < n; i++ {
		clock.t = start.Add(time.Duration(i) * time.Second)
		body := strings.ReplaceAll(safariReportToBody, "/chat/c1", fmt.Sprintf("/chat/c%d", i))
		if rec := post(h, "application/csp-report", body); rec.Code != http.StatusNoContent {
			t.Fatalf("report %d: status = %d, want 204", i, rec.Code)
		}
		if size := h.Dedupe.size(); size > maxKeys {
			t.Fatalf("after %d violations %d are tracked, want at most %d", i+1, size, maxKeys)
		}
	}
	if got := records(t, &buf); len(got) != n {
		t.Fatalf("%d records for %d distinct violations, want %d", len(got), n, n)
	}
	for i := n - maxKeys; i < n; i++ {
		if !h.Dedupe.tracks(fmt.Sprintf("/chat/c%d", i)) {
			t.Fatalf("/chat/c%d is not tracked; want the %d newest entries kept", i, maxKeys)
		}
	}

	// Entries whose interval is over are removed first: after the
	// interval, a full table is emptied down to the new entry (one that was
	// never tracked).
	clock.t = clock.t.Add(10 * time.Minute)
	if rec := post(h, "application/csp-report", strings.ReplaceAll(safariReportToBody, "/chat/c1", "/chat/new")); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if size := h.Dedupe.size(); size != 1 {
		t.Fatalf("after the interval %d entries are tracked, want 1", size)
	}
}

// TestRefusedRecordIsNotCountedAsSeen: a report the record limiter refuses
// is not logged, and the same violation is logged once the limiter
// allows it.
func TestRefusedRecordIsNotCountedAsSeen(t *testing.T) {
	var buf bytes.Buffer
	d := NewDedupe(10*time.Minute, 16)
	allow := false
	h := &Handler{
		Log:         recordingLog(&buf),
		Dedupe:      d,
		AllowRecord: func() (bool, int) { return allow, 3 },
	}
	rec := post(h, "application/csp-report", safariReportToBody)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "3" {
		t.Fatalf("refused: status = %d, Retry-After %q, want 429 and 3", rec.Code, rec.Header().Get("Retry-After"))
	}
	if d.size() != 0 {
		t.Fatalf("a refused report is tracked")
	}
	allow = true
	if rec := post(h, "application/csp-report", safariReportToBody); rec.Code != http.StatusNoContent {
		t.Fatalf("allowed: status = %d, want 204", rec.Code)
	}
	if got := records(t, &buf); len(got) != 1 {
		t.Fatalf("%d records, want 1", len(got))
	}
}

// TestCollapsedRecordsLeaveSamplesOut: with Dedupe on, records still hold
// no sample, query or fragment by default, in every report shape.
func TestCollapsedRecordsLeaveSamplesOut(t *testing.T) {
	const marker = "Secret9b2e"
	bodies := []string{
		`{"type":"csp-violation","body":{"documentURL":"https://hub.example/a?x=` + marker + `",` +
			`"effectiveDirective":"require-trusted-types-for","blockedURL":"trusted-types-sink",` +
			`"sample":"Element innerHTML|` + marker + `"}}`,
		`{"csp-report":{"document-uri":"https://hub.example/b#` + marker + `","effective-directive":"script-src-elem",` +
			`"blocked-uri":"https://cdn.example/s.js?k=` + marker + `","script-sample":"` + marker + `"}}`,
		`[{"type":"csp-violation","body":{"documentURL":"https://hub.example/c","effectiveDirective":"script-src",` +
			`"blockedURL":"inline","sourceFile":"https://hub.example/m.js?` + marker + `","sample":"` + marker + `"}}]`,
	}
	var buf bytes.Buffer
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	h, _ := dedupeHandler(&buf, clock, 10*time.Minute, 16)
	for _, body := range bodies {
		for i := 0; i < 2; i++ {
			if rec := post(h, "application/csp-report", body); rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", rec.Code)
			}
		}
	}
	out := buf.String()
	if strings.Contains(out, marker) {
		t.Fatalf("log holds a sample, query or fragment: %s", out)
	}
	// Positive control: one record per violation, with the sink kept.
	got := records(t, bytes.NewBufferString(out))
	if len(got) != len(bodies) || got[0]["sink"] != "Element innerHTML" {
		t.Fatalf("records = %v, want %d with the first naming its sink", got, len(bodies))
	}
}

// TestNilDedupeLogsEveryReport: without a Dedupe, identical reports are
// all logged.
func TestNilDedupeLogsEveryReport(t *testing.T) {
	var buf bytes.Buffer
	h := &Handler{Log: recordingLog(&buf)}
	for i := 0; i < 3; i++ {
		post(h, "application/csp-report", safariReportToBody)
	}
	if got := records(t, &buf); len(got) != 3 {
		t.Fatalf("%d records, want 3", len(got))
	}
}

// pageReport is a report of its own violation for each page path.
func pageReport(page string) string {
	return strings.ReplaceAll(safariReportToBody, "/chat/c1", "/chat/"+page)
}

// summaryPaths returns the document_path of every summary record in buf.
func summaryPaths(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	var out []string
	for _, r := range records(t, buf) {
		if r["msg"] == RepeatsLogMessage {
			p, _ := r["document_path"].(string)
			out = append(out, p)
		}
	}
	return out
}

// TestFullTableRemovesFinishedEntries: a new violation that finds the
// table full removes every entry whose interval is over, with their
// summaries, before any other entry, even between the once-per-interval
// checks.
func TestFullTableRemovesFinishedEntries(t *testing.T) {
	var buf bytes.Buffer
	start := time.Unix(1_700_000_000, 0)
	clock := &fakeClock{t: start}
	h, _ := dedupeHandler(&buf, clock, 10*time.Minute, 4)
	post(h, "application/csp-report", pageReport("x")) // the first check runs here
	clock.t = start.Add(time.Minute)
	post(h, "application/csp-report", pageReport("a"))
	post(h, "application/csp-report", pageReport("a"))
	clock.t = start.Add(2 * time.Minute)
	post(h, "application/csp-report", pageReport("b"))
	clock.t = start.Add(3 * time.Minute)
	post(h, "application/csp-report", pageReport("c"))
	// The next check: x's interval is over (no repeats, no summary); d
	// takes its place. The table is full again: a, b, c, d.
	clock.t = start.Add(10 * time.Minute)
	post(h, "application/csp-report", pageReport("d"))
	records(t, &buf)
	if h.Dedupe.size() != 4 {
		t.Fatalf("%d entries tracked, want 4", h.Dedupe.size())
	}
	// a and b are now over, but the next check is not due until minute 20.
	clock.t = start.Add(12 * time.Minute)
	post(h, "application/csp-report", pageReport("e"))
	if got := summaryPaths(t, &buf); len(got) != 1 || got[0] != "/projects/p1/chat/a" {
		t.Fatalf("summaries = %v, want one for /projects/p1/chat/a", got)
	}
	if h.Dedupe.size() != 3 {
		t.Fatalf("%d entries tracked, want 3 (a and b removed, e added)", h.Dedupe.size())
	}
}

// TestFullTableRemovesOldestEntry: when no entry's interval is over, the
// entry with the oldest record is removed to make room.
func TestFullTableRemovesOldestEntry(t *testing.T) {
	var buf bytes.Buffer
	start := time.Unix(1_700_000_000, 0)
	clock := &fakeClock{t: start}
	h, _ := dedupeHandler(&buf, clock, 10*time.Minute, 2)
	post(h, "application/csp-report", pageReport("a"))
	clock.t = start.Add(time.Minute)
	post(h, "application/csp-report", pageReport("b"))
	clock.t = start.Add(2 * time.Minute)
	post(h, "application/csp-report", pageReport("c"))
	records(t, &buf)
	// b is still tracked: its repeat is collapsed. a was removed: its
	// report is logged again.
	post(h, "application/csp-report", pageReport("b"))
	if got := records(t, &buf); len(got) != 0 {
		t.Fatalf("records for a repeat of b = %v, want none", got)
	}
	// Adding a back removes the oldest entry again, now b (with its repeat,
	// so its summary comes first), not c.
	post(h, "application/csp-report", pageReport("a"))
	got := records(t, &buf)
	if len(got) != 2 || got[0]["msg"] != RepeatsLogMessage || got[0]["document_path"] != "/projects/p1/chat/b" ||
		got[1]["msg"] != LogMessage || got[1]["document_path"] != "/projects/p1/chat/a" {
		t.Fatalf("records for a = %v, want b's summary, then a's report logged again", got)
	}
}

// TestFinishedIntervalsAreCheckedOncePerInterval: the check for finished
// intervals on any report runs at most once per interval, so an entry
// whose interval ends just after a check gets its summary at the next
// check, up to about one interval later.
func TestFinishedIntervalsAreCheckedOncePerInterval(t *testing.T) {
	var buf bytes.Buffer
	start := time.Unix(1_700_000_000, 0)
	clock := &fakeClock{t: start}
	h, _ := dedupeHandler(&buf, clock, 10*time.Minute, 16)
	post(h, "application/csp-report", pageReport("a")) // check at minute 0; a logged at 0
	post(h, "application/csp-report", pageReport("a"))
	clock.t = start.Add(9 * time.Minute)
	post(h, "application/csp-report", pageReport("x")) // no check (9 < 10); x logged at 9
	post(h, "application/csp-report", pageReport("x"))
	clock.t = start.Add(10 * time.Minute)
	post(h, "application/csp-report", pageReport("y")) // check at 10: a is over, x is not
	if got := summaryPaths(t, &buf); len(got) != 1 || got[0] != "/projects/p1/chat/a" {
		t.Fatalf("summaries at minute 10 = %v, want a's", got)
	}
	// x is over at minute 19, but the next check is due at minute 20.
	clock.t = start.Add(19 * time.Minute)
	post(h, "application/csp-report", pageReport("z"))
	if got := summaryPaths(t, &buf); len(got) != 0 {
		t.Fatalf("summaries at minute 19 = %v, want none (next check at 20)", got)
	}
	clock.t = start.Add(20 * time.Minute)
	post(h, "application/csp-report", pageReport("w"))
	if got := summaryPaths(t, &buf); len(got) != 1 || got[0] != "/projects/p1/chat/x" {
		t.Fatalf("summaries at minute 20 = %v, want x's", got)
	}
}

// TestSummaryOnRefusedAndDroppedReports: a summary found while handling a
// report that is then refused (429) or dropped (past the per-request
// bound) is still written.
func TestSummaryOnRefusedAndDroppedReports(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	t.Run("refused", func(t *testing.T) {
		var buf bytes.Buffer
		clock := &fakeClock{t: start}
		d := NewDedupe(10*time.Minute, 16)
		d.now = clock.now
		allow := true
		h := &Handler{Log: recordingLog(&buf), Dedupe: d, AllowRecord: func() (bool, int) { return allow, 1 }}
		post(h, "application/csp-report", pageReport("a"))
		post(h, "application/csp-report", pageReport("a"))
		records(t, &buf)
		clock.t = start.Add(10 * time.Minute)
		allow = false
		if rec := post(h, "application/csp-report", pageReport("b")); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", rec.Code)
		}
		if got := summaryPaths(t, &buf); len(got) != 1 || got[0] != "/projects/p1/chat/a" {
			t.Fatalf("summaries = %v, want a's", got)
		}
	})
	t.Run("dropped", func(t *testing.T) {
		var buf bytes.Buffer
		clock := &fakeClock{t: start}
		h, _ := dedupeHandler(&buf, clock, 10*time.Minute, 64)
		post(h, "application/csp-report", pageReport("x")) // check at minute 0
		clock.t = start.Add(time.Minute)
		post(h, "application/csp-report", pageReport("a")) // a logged at 1
		post(h, "application/csp-report", pageReport("a"))
		clock.t = start.Add(10 * time.Minute)
		post(h, "application/csp-report", pageReport("y")) // check at 10: a not over yet
		records(t, &buf)
		// Minute 12: a is over and no check is due before 20. One request
		// holds 10 new violations and then a's return, past the bound: a's
		// report is dropped, and its summary is written.
		clock.t = start.Add(12 * time.Minute)
		parts := make([]string, 0, maxRecordsPerRequest+1)
		for i := 0; i < maxRecordsPerRequest; i++ {
			parts = append(parts, fmt.Sprintf(`{"type":"csp-violation","body":{"effectiveDirective":"d%d"}}`, i))
		}
		a := pageReport("a")
		parts = append(parts, a)
		if rec := post(h, "application/reports+json", "["+strings.Join(parts, ",")+"]"); rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
		var sums, dropped int
		for _, r := range records(t, &buf) {
			switch r["msg"] {
			case RepeatsLogMessage:
				if r["document_path"] == "/projects/p1/chat/a" && r["repeats"] == float64(1) {
					sums++
				}
			case DroppedLogMessage:
				if r["dropped"] == float64(1) {
					dropped++
				}
			}
		}
		if sums != 1 || dropped != 1 {
			t.Fatalf("a's summaries %d and dropped-count records %d, want 1 and 1", sums, dropped)
		}
	})
}

// tracks reports whether an entry's key ends with suffix (its page path).
func (d *Dedupe) tracks(suffix string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k := range d.entries {
		if strings.HasSuffix(k, suffix) {
			return true
		}
	}
	return false
}

// TestOldestEntryTieBreak: entries whose records were written at the same
// time are removed in key order, so the choice does not depend on map
// order.
func TestOldestEntryTieBreak(t *testing.T) {
	var buf bytes.Buffer
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	h, _ := dedupeHandler(&buf, clock, 10*time.Minute, 2)
	// Same time for all three; the keys differ only in the page path, and
	// ".../chat/a" sorts before ".../chat/b".
	post(h, "application/csp-report", pageReport("b"))
	post(h, "application/csp-report", pageReport("a"))
	post(h, "application/csp-report", pageReport("c"))
	if h.Dedupe.tracks("/chat/a") || !h.Dedupe.tracks("/chat/b") || !h.Dedupe.tracks("/chat/c") {
		t.Fatalf("after a full-table insert at one time, want a removed (smallest key) and b, c kept")
	}
}
