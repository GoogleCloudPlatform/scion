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
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

// countingClient is a per-client Limiter with left tokens that counts how
// often it is asked.
type countingClient struct{ left, asked int }

func (c *countingClient) allow(*http.Request) bool {
	c.asked++
	if c.left == 0 {
		return false
	}
	c.left--
	return true
}

// byMessage counts the records in recs by message.
func byMessage(recs []map[string]any) map[any]int {
	out := map[any]int{}
	for _, r := range recs {
		out[r["msg"]]++
	}
	return out
}

// TestRepeatFloodFromOneClientIsAnswered204: one client sends the same
// report a thousand times, far past its budget of requests. Every request
// gets 204; only the first report is charged to the per-client budget and
// logged, the repeats are counted, and one summary record follows the
// interval.
func TestRepeatFloodFromOneClientIsAnswered204(t *testing.T) {
	const n = 1000
	var buf bytes.Buffer
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	h, asked := dedupeHandler(&buf, clock, 10*time.Minute, 16)
	h.now = clock.now
	client := &countingClient{left: 5}
	h.Allow = client.allow
	for i := 0; i < n; i++ {
		if rec := post(h, "application/csp-report", safariReportToBody); rec.Code != http.StatusNoContent {
			t.Fatalf("report %d: status = %d, want 204", i+1, rec.Code)
		}
		clock.t = clock.t.Add(100 * time.Millisecond)
	}
	if client.asked != 1 {
		t.Errorf("per-client budget asked %d times, want 1 (repeats are not charged)", client.asked)
	}
	if *asked != 1 {
		t.Errorf("record budget asked %d times, want 1", *asked)
	}
	if got := byMessage(records(t, &buf)); got[LogMessage] != 1 || len(got) != 1 {
		t.Fatalf("records by message %v, want one %s record only", got, LogMessage)
	}
	if c := h.Counts(); c.Collapsed != n-1 || c.RefusedReports != 0 || c.RefusedRequests != 0 {
		t.Fatalf("counts = %+v, want %d collapsed and nothing refused", c, n-1)
	}
	// After the interval, one summary counts the repeats.
	clock.t = clock.t.Add(10 * time.Minute)
	post(h, "application/csp-report", pageReport("other"))
	var repeats []float64
	for _, r := range records(t, &buf) {
		if r["msg"] == RepeatsLogMessage {
			repeats = append(repeats, r["repeats"].(float64))
		}
	}
	if len(repeats) != 1 || repeats[0] != n-1 {
		t.Fatalf("summary repeats = %v, want [%d]", repeats, n-1)
	}
}

// TestOverBudgetFloodIsAnswered204WithoutPerReportRecords: a client past
// its budget sends a thousand distinct reports over five minutes. Every
// request gets 204, no report record is written, and the refusals show in
// at most one summary record per interval whose counts add up to all of
// them.
func TestOverBudgetFloodIsAnswered204WithoutPerReportRecords(t *testing.T) {
	const n = 1000
	start := time.Unix(1_700_000_000, 0)
	var buf bytes.Buffer
	clock := &fakeClock{t: start}
	h, asked := dedupeHandler(&buf, clock, 10*time.Minute, 16)
	h.now = clock.now
	h.RefusedInterval = time.Minute
	h.afterFunc = func(time.Duration, func()) func() bool { return func() bool { return true } } // flushes come from requests here
	client := &countingClient{left: 0}
	h.Allow = client.allow
	step := 5 * time.Minute / n
	for i := 0; i < n; i++ {
		if rec := post(h, "application/csp-report", pageReport(fmt.Sprintf("p%d", i))); rec.Code != http.StatusNoContent {
			t.Fatalf("report %d: status = %d, want 204", i+1, rec.Code)
		}
		clock.t = clock.t.Add(step)
	}
	// A later request, past the interval, carries the remaining count.
	clock.t = clock.t.Add(time.Minute)
	client.left = 1
	post(h, "application/csp-report", pageReport("allowed"))

	recs := records(t, &buf)
	got := byMessage(recs)
	if got[LogMessage] != 1 {
		t.Fatalf("%d report records, want only the allowed one", got[LogMessage])
	}
	// Summaries at 0, 1, 2, 3 and 4 minutes, and the rest with the
	// allowed request at about 6 minutes.
	if summaries := got[RefusedLogMessage]; summaries != 6 {
		t.Fatalf("%d refusal summaries, want 6 (one per minute while refusals arrive, then one for the rest)", summaries)
	}
	var total float64
	var times []time.Time
	for _, r := range recs {
		if r["msg"] != RefusedLogMessage {
			continue
		}
		total += r["refused_reports"].(float64)
		ts, err := time.Parse(time.RFC3339Nano, r["latest_refused"].(string))
		if err != nil {
			t.Fatalf("latest_refused: %v", err)
		}
		times = append(times, ts)
	}
	if total != n {
		t.Fatalf("refusal summaries count %v reports, want %d", total, n)
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	for i := 1; i < len(times); i++ {
		if times[i].Sub(times[i-1]) < time.Minute-step {
			t.Errorf("summaries %d and %d are %v apart, want about a minute or more", i-1, i, times[i].Sub(times[i-1]))
		}
	}
	if c := h.Counts(); c.RefusedReports != n {
		t.Errorf("counts = %+v, want %d refused reports", c, n)
	}
	if *asked != 1 {
		t.Errorf("record budget asked %d times, want 1 (refused reports never reach it)", *asked)
	}
	if h.Dedupe.size() != 1 {
		t.Errorf("Dedupe tracks %d violations, want 1 (refused reports are not tracked)", h.Dedupe.size())
	}
}

// TestUnreadableRequestOverBudgetIsAnswered204: a request that cannot be
// read is charged to the per-client budget; within it, the error code is
// answered, and past it, 204, with the request counted as refused.
func TestUnreadableRequestOverBudgetIsAnswered204(t *testing.T) {
	overCap := reportingAPIBody + strings.Repeat(" ", MaxBodyBytes+1-len(reportingAPIBody))
	cases := []struct {
		name, contentType, body string
		code                    int
	}{
		{"malformed", "application/reports+json", `{"not":"a report"}`, http.StatusBadRequest},
		{"content type", "text/plain", reportingAPIBody, http.StatusUnsupportedMediaType},
		{"too large", "application/reports+json", overCap, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			client := &countingClient{left: 1}
			h := &Handler{Log: recordingLog(&buf), Allow: client.allow}
			// Within the budget: the error code.
			if rec := post(h, tc.contentType, tc.body); rec.Code != tc.code {
				t.Fatalf("within the budget: status = %d, want %d", rec.Code, tc.code)
			}
			if got := records(t, &buf); len(got) != 0 {
				t.Fatalf("within the budget: records %v, want none", got)
			}
			// Past it: 204 and one refusal summary.
			if rec := post(h, tc.contentType, tc.body); rec.Code != http.StatusNoContent {
				t.Fatalf("past the budget: status = %d, want 204", rec.Code)
			}
			got := records(t, &buf)
			if len(got) != 1 || got[0]["msg"] != RefusedLogMessage || got[0]["refused_requests"] != float64(1) {
				t.Fatalf("past the budget: records %v, want one %s record with refused_requests 1", got, RefusedLogMessage)
			}
			if client.asked != 2 {
				t.Errorf("per-client budget asked %d times, want 2", client.asked)
			}
			if c := h.Counts(); c.RefusedRequests != 1 || c.RefusedReports != 0 {
				t.Errorf("counts = %+v, want one refused request", c)
			}
		})
	}
}

// TestRefusalSummaryHoldsCountsOnly: a refusal summary carries counts and
// times, and nothing from the client or its reports, whatever the reports
// hold.
func TestRefusalSummaryHoldsCountsOnly(t *testing.T) {
	var buf bytes.Buffer
	h := &Handler{Log: recordingLog(&buf), Allow: func(*http.Request) bool { return false }, RawSamples: true}
	req := strings.NewReader(reportingAPIBody)
	r, _ := http.NewRequest(http.MethodPost, "/api/v1/csp-reports", req)
	r.Header.Set("Content-Type", "application/reports+json")
	r.RemoteAddr = "192.0.2.77:4444"
	r.Header.Set("User-Agent", "Mozilla/5.0 (iPhone)")
	rec := &recorder{header: http.Header{}}
	h.ServeHTTP(rec, r)
	if rec.code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.code)
	}
	got := records(t, &buf)
	if len(got) != 1 {
		t.Fatalf("records %v, want one", got)
	}
	keys := make([]string, 0, len(got[0]))
	for k := range got[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := "first_refused,latest_refused,level,msg,refused_reports,refused_requests,time"
	if strings.Join(keys, ",") != want {
		t.Fatalf("summary keys = %v, want %s", keys, want)
	}
	raw := fmt.Sprint(got[0])
	for _, s := range []string{"192.0.2.77", "iPhone", "hub.example", "innerHTML", "main.js"} {
		if strings.Contains(raw, s) {
			t.Errorf("summary holds %q: %s", s, raw)
		}
	}
}

// recorder is a minimal http.ResponseWriter.
type recorder struct {
	header http.Header
	code   int
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *recorder) WriteHeader(code int)        { r.code = code }

// TestRefusalSummaryIsWrittenByALaterRequest: refusals counted after a
// summary wait for the interval, and are then written by the next request
// of any kind, with the time of the latest refusal.
func TestRefusalSummaryIsWrittenByALaterRequest(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	var buf bytes.Buffer
	clock := &fakeClock{t: start}
	allow := false
	h := &Handler{
		Log:             recordingLog(&buf),
		Allow:           func(*http.Request) bool { return allow },
		RefusedInterval: time.Minute,
		now:             clock.now,
		afterFunc:       func(time.Duration, func()) func() bool { return func() bool { return true } }, // this test checks the flush by a request
	}
	post(h, "application/reports+json", reportingAPIBody) // summarized at once
	if got := byMessage(records(t, &buf)); got[RefusedLogMessage] != 1 {
		t.Fatalf("first refusal: records %v, want one summary", got)
	}
	clock.t = start.Add(10 * time.Second)
	post(h, "application/reports+json", reportingAPIBody)
	clock.t = start.Add(20 * time.Second)
	post(h, "application/reports+json", reportingAPIBody)
	if got := records(t, &buf); len(got) != 0 {
		t.Fatalf("within the interval: records %v, want none", got)
	}
	clock.t = start.Add(2 * time.Minute)
	allow = true
	post(h, "application/reports+json", reportingAPIBody)
	got := records(t, &buf)
	var sum map[string]any
	for _, r := range got {
		if r["msg"] == RefusedLogMessage {
			sum = r
		}
	}
	if sum == nil {
		t.Fatalf("records %v, want a refusal summary", got)
	}
	if sum["refused_reports"] != float64(2) {
		t.Errorf("refused_reports = %v, want 2", sum["refused_reports"])
	}
	if want := start.Add(10 * time.Second).UTC().Format(time.RFC3339); sum["first_refused"] != want {
		t.Errorf("first_refused = %v, want %s", sum["first_refused"], want)
	}
	if want := start.Add(20 * time.Second).UTC().Format(time.RFC3339); sum["latest_refused"] != want {
		t.Errorf("latest_refused = %v, want %s", sum["latest_refused"], want)
	}
	if c := byMessage(got); c[LogMessage] != 1 {
		t.Errorf("records %v, want the allowed report's record too", c)
	}
}

// unreadBody fails the test if the handler reads it.
type unreadBody struct{ t *testing.T }

func (b unreadBody) Read([]byte) (int, error) {
	b.t.Error("the body of a request past the read ceiling was read")
	return 0, io.EOF
}

// TestReadCeilingRefusesBeforeReading: a request the read ceiling refuses
// gets 204, whatever its content type, without its body being read or the
// per-client budget being asked; it is counted as a refused request, and
// its summary holds counts and times only.
func TestReadCeilingRefusesBeforeReading(t *testing.T) {
	for _, contentType := range []string{"application/csp-report", "text/plain"} {
		t.Run(contentType, func(t *testing.T) {
			var buf bytes.Buffer
			client := &countingClient{left: 10}
			reads := 0
			h := &Handler{
				Log:       recordingLog(&buf),
				AllowRead: func(*http.Request) bool { reads++; return false },
				Allow:     client.allow,
			}
			r, _ := http.NewRequest(http.MethodPost, "/api/v1/csp-reports", io.NopCloser(unreadBody{t}))
			r.Header.Set("Content-Type", contentType)
			r.RemoteAddr = "192.0.2.90:4444"
			rec := &recorder{header: http.Header{}}
			h.ServeHTTP(rec, r)
			if rec.code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", rec.code)
			}
			if reads != 1 || client.asked != 0 {
				t.Errorf("read ceiling asked %d times, per-client budget %d; want 1 and 0", reads, client.asked)
			}
			got := records(t, &buf)
			if len(got) != 1 || got[0]["msg"] != RefusedLogMessage || got[0]["refused_requests"] != float64(1) {
				t.Fatalf("records %v, want one %s record with refused_requests 1", got, RefusedLogMessage)
			}
			if strings.Contains(fmt.Sprint(got[0]), "192.0.2.90") {
				t.Errorf("summary holds the client address: %v", got[0])
			}
			if c := h.Counts(); c.RefusedRequests != 1 || c.RefusedReports != 0 {
				t.Errorf("counts = %+v, want one refused request", c)
			}
		})
	}
	// Positive control: within the ceiling the same report is read and logged.
	var buf bytes.Buffer
	h := &Handler{Log: recordingLog(&buf), AllowRead: func(*http.Request) bool { return true }}
	if rec := post(h, "application/csp-report", safariReportToBody); rec.Code != http.StatusNoContent {
		t.Fatalf("within the ceiling: status = %d, want 204", rec.Code)
	}
	if got := byMessage(records(t, &buf)); got[LogMessage] != 1 {
		t.Fatalf("within the ceiling: records %v, want one %s record", got, LogMessage)
	}
}

// TestReportsLookedAtPerRequestAreBounded: of a request with more than
// maxReportsPerRequest reports, only the first maxReportsPerRequest reach
// Dedupe; the rest are counted in the dropped-count record.
func TestReportsLookedAtPerRequestAreBounded(t *testing.T) {
	const n = maxReportsPerRequest + 50
	t.Run("repeats", func(t *testing.T) {
		var buf bytes.Buffer
		clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
		h, asked := dedupeHandler(&buf, clock, 10*time.Minute, 16)
		if rec := post(h, "application/reports+json", manyReports(n)); rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
		if c := h.Counts(); c.Collapsed != maxReportsPerRequest-1 {
			t.Fatalf("collapsed %d, want %d (only the first %d looked at)", c.Collapsed, maxReportsPerRequest-1, maxReportsPerRequest)
		}
		got := records(t, &buf)
		var dropped []float64
		for _, r := range got {
			if r["msg"] == DroppedLogMessage {
				dropped = append(dropped, r["dropped"].(float64))
			}
		}
		if byMessage(got)[LogMessage] != 1 || len(dropped) != 1 || dropped[0] != n-maxReportsPerRequest {
			t.Fatalf("records %v, want one report record and dropped=%d", byMessage(got), n-maxReportsPerRequest)
		}
		if *asked != 2 {
			t.Errorf("record budget asked %d times, want 2 (one record, one dropped-count record)", *asked)
		}
	})
	t.Run("distinct", func(t *testing.T) {
		var buf bytes.Buffer
		clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
		h, _ := dedupeHandler(&buf, clock, 10*time.Minute, 1024)
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprintf(`{"type":"csp-violation","body":{"effectiveDirective":"d%d"}}`, i)
		}
		if rec := post(h, "application/reports+json", "["+strings.Join(parts, ",")+"]"); rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
		var dropped float64
		for _, r := range records(t, &buf) {
			if r["msg"] == DroppedLogMessage {
				dropped = r["dropped"].(float64)
			}
		}
		if dropped != n-maxRecordsPerRequest {
			t.Fatalf("dropped = %v, want %d", dropped, n-maxRecordsPerRequest)
		}
		if size := h.Dedupe.size(); size != maxRecordsPerRequest {
			t.Errorf("Dedupe tracks %d, want %d (dropped reports are not tracked)", size, maxRecordsPerRequest)
		}
	})
}

// TestClientBudgetChargedOncePerRequest: with several reports in one
// request, the per-client budget is asked once, for the first report that
// is not a repeat, and a refusal holds for the rest of the request.
func TestClientBudgetChargedOncePerRequest(t *testing.T) {
	rep := func(page string) string {
		return fmt.Sprintf(`{"type":"csp-violation","body":{"documentURL":"https://hub.example/%s","effectiveDirective":"script-src"}}`, page)
	}
	repeat := func(page string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = page
		}
		return out
	}
	batch := func(pages ...string) string {
		parts := make([]string, len(pages))
		for i, p := range pages {
			parts[i] = rep(p)
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	cases := []struct {
		name               string
		seed               string // logged first, with its own budget
		left               int
		body               string
		asked, written     int
		recordAsked        int
		collapsed, refused int64
	}{
		{name: "budget for one request", left: 1, body: batch("a", "b", "c"), asked: 1, written: 3, recordAsked: 3},
		{name: "no budget", left: 0, body: batch("a", "b", "c"), asked: 1, refused: 3},
		{name: "repeat first", seed: "a", left: 0, body: batch("a", "b", "c"), asked: 1, collapsed: 1, refused: 2},
		{name: "repeats only", seed: "a", left: 0, body: batch("a", "a", "a"), asked: 0, collapsed: 3},
		{name: "repeat between", seed: "b", left: 1, body: batch("a", "b", "c"), asked: 1, written: 2, recordAsked: 2, collapsed: 1},
		// Repeats past the entry cap: the cut-off report would need a
		// dropped-count record, which is charged like a report record.
		{name: "repeats past the entry cap", seed: "a", left: 0, body: batch(repeat("a", maxReportsPerRequest+1)...),
			asked: 1, collapsed: maxReportsPerRequest, refused: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
			h, recordAsked := dedupeHandler(&buf, clock, 10*time.Minute, 16)
			h.now = clock.now
			if tc.seed != "" {
				h.Allow = func(*http.Request) bool { return true }
				post(h, "application/reports+json", batch(tc.seed))
				records(t, &buf)
			}
			seedAsked := *recordAsked
			client := &countingClient{left: tc.left}
			h.Allow = client.allow
			if rec := post(h, "application/reports+json", tc.body); rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", rec.Code)
			}
			if client.asked != tc.asked {
				t.Errorf("per-client budget asked %d times, want %d", client.asked, tc.asked)
			}
			if got := *recordAsked - seedAsked; got != tc.recordAsked {
				t.Errorf("record budget asked %d times, want %d", got, tc.recordAsked)
			}
			got := byMessage(records(t, &buf))
			if got[LogMessage] != tc.written || got[DroppedLogMessage] != 0 {
				t.Errorf("records %v, want %d report records and no dropped-count record", got, tc.written)
			}
			c := h.Counts()
			if c.Collapsed != tc.collapsed || c.RefusedReports != tc.refused {
				t.Errorf("counts = %+v, want %d collapsed and %d refused reports", c, tc.collapsed, tc.refused)
			}
		})
	}
}

// TestRefusalsAtTheEndOfABurstAreFlushedByTimer: refusals left pending
// after a summary are written by the flush timer once the interval is
// over, with no further request; the timer is armed once per pending
// stretch, and armed again for the next one.
func TestRefusalsAtTheEndOfABurstAreFlushedByTimer(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	var buf bytes.Buffer
	clock := &fakeClock{t: start}
	var timers []time.Duration
	var fire func()
	h := &Handler{
		Log:             recordingLog(&buf),
		Allow:           func(*http.Request) bool { return false },
		RefusedInterval: time.Minute,
		now:             clock.now,
		afterFunc: func(d time.Duration, f func()) func() bool {
			timers = append(timers, d)
			fire = f
			return func() bool { return true }
		},
	}
	post(h, "application/reports+json", reportingAPIBody) // summarized at once
	records(t, &buf)
	clock.t = start.Add(10 * time.Second)
	post(h, "application/reports+json", reportingAPIBody)
	clock.t = start.Add(20 * time.Second)
	post(h, "application/reports+json", reportingAPIBody)
	if len(timers) != 1 || timers[0] != 50*time.Second {
		t.Fatalf("timers armed %v, want one for 50s", timers)
	}
	if got := records(t, &buf); len(got) != 0 {
		t.Fatalf("before the timer: records %v, want none", got)
	}
	clock.t = start.Add(time.Minute)
	fire()
	got := records(t, &buf)
	if len(got) != 1 || got[0]["msg"] != RefusedLogMessage || got[0]["refused_reports"] != float64(2) {
		t.Fatalf("after the timer: records %v, want one summary with refused_reports 2", got)
	}
	// The next pending stretch arms a new timer.
	clock.t = start.Add(70 * time.Second)
	post(h, "application/reports+json", reportingAPIBody)
	if len(timers) != 2 || timers[1] != 50*time.Second {
		t.Fatalf("timers armed %v, want a second one for 50s", timers)
	}
	// A timer that finds nothing pending writes nothing.
	clock.t = start.Add(2 * time.Minute)
	fire()
	records(t, &buf)
	fire()
	if got := records(t, &buf); len(got) != 0 {
		t.Fatalf("an empty flush wrote %v", got)
	}
	if c := h.Counts(); c.RefusedReports != 4 {
		t.Errorf("counts = %+v, want 4 refused reports", c)
	}
}

// TestCloseStopsTheTimerAndWritesPendingRefusals: Close stops the armed
// flush timer, writes the pending refusals at once, and arms no timer
// afterwards; a second Close writes nothing.
func TestCloseStopsTheTimerAndWritesPendingRefusals(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	var buf bytes.Buffer
	clock := &fakeClock{t: start}
	armed, stopped := 0, 0
	h := &Handler{
		Log:             recordingLog(&buf),
		Allow:           func(*http.Request) bool { return false },
		RefusedInterval: time.Minute,
		now:             clock.now,
		afterFunc: func(time.Duration, func()) func() bool {
			armed++
			return func() bool { stopped++; return true }
		},
	}
	post(h, "application/reports+json", reportingAPIBody) // summarized at once
	clock.t = start.Add(10 * time.Second)
	post(h, "application/reports+json", reportingAPIBody) // pending, arms the timer
	records(t, &buf)
	if armed != 1 {
		t.Fatalf("timers armed %d, want 1", armed)
	}
	h.Close()
	if stopped != 1 {
		t.Errorf("timer stopped %d times, want 1", stopped)
	}
	got := records(t, &buf)
	if len(got) != 1 || got[0]["msg"] != RefusedLogMessage || got[0]["refused_reports"] != float64(1) {
		t.Fatalf("after Close: records %v, want one summary with refused_reports 1", got)
	}
	// After Close: still answered and counted, no timer.
	clock.t = start.Add(20 * time.Second)
	if rec := post(h, "application/reports+json", reportingAPIBody); rec.Code != http.StatusNoContent {
		t.Fatalf("after Close: status = %d, want 204", rec.Code)
	}
	if armed != 1 {
		t.Errorf("a timer was armed after Close")
	}
	h.Close()
	h.Close()
	if c := h.Counts(); c.RefusedReports != 3 {
		t.Errorf("counts = %+v, want 3 refused reports", c)
	}
	// The third refusal was pending at the second Close and is written then.
	if got := records(t, &buf); len(got) != 1 || got[0]["refused_reports"] != float64(1) {
		t.Fatalf("second Close: records %v, want one summary with refused_reports 1", got)
	}
}

// TestParseBodyStopsAtTheLimit: parseBody builds at most limit violations,
// in body order, and counts the policy reports past them; reports of other
// types are skipped and not counted.
func TestParseBodyStopsAtTheLimit(t *testing.T) {
	parts := []string{`{"type":"deprecation","body":{"id":"x"}}`}
	for i := 0; i < 150; i++ {
		parts = append(parts, fmt.Sprintf(`{"type":"csp-violation","body":{"effectiveDirective":"d%d"}}`, i))
		if i%50 == 0 {
			parts = append(parts, `{"type":"intervention","body":{"id":"y"}}`)
		}
	}
	got, extra, err := parseBody([]byte("["+strings.Join(parts, ",")+"]"), 100)
	if err != nil {
		t.Fatalf("parseBody: %v", err)
	}
	if len(got) != 100 || extra != 50 {
		t.Fatalf("got %d violations and %d extra, want 100 and 50", len(got), extra)
	}
	if got[0].Directive != "d0" || got[99].Directive != "d99" {
		t.Errorf("violations %q..%q, want d0..d99 in body order", got[0].Directive, got[99].Directive)
	}
	// Positive control: under the limit nothing is extra.
	if got, extra, err := parseBody([]byte(manyReports(5)), 100); err != nil || len(got) != 5 || extra != 0 {
		t.Errorf("5 reports: got %d, extra %d, err %v; want 5, 0, nil", len(got), extra, err)
	}
}

// TestTimerAfterCloseWritesNothing: a flush timer armed before Close that
// fires after it writes nothing, even with refusals pending and the
// interval over; the next Close writes them, once.
func TestTimerAfterCloseWritesNothing(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	var buf bytes.Buffer
	clock := &fakeClock{t: start}
	var fire func()
	h := &Handler{
		Log:             recordingLog(&buf),
		Allow:           func(*http.Request) bool { return false },
		RefusedInterval: time.Minute,
		now:             clock.now,
		afterFunc: func(_ time.Duration, f func()) func() bool {
			fire = f
			return func() bool { return false } // already running: cannot be stopped
		},
	}
	post(h, "application/reports+json", reportingAPIBody) // summarized at once
	clock.t = start.Add(10 * time.Second)
	post(h, "application/reports+json", reportingAPIBody) // pending, arms the timer
	h.Close()                                             // writes the pending one
	if got := records(t, &buf); len(got) != 2 || got[1]["msg"] != RefusedLogMessage || got[1]["refused_reports"] != float64(1) {
		t.Fatalf("first Close: records %v, want the first summary and one with refused_reports 1", got)
	}
	clock.t = start.Add(20 * time.Second)
	post(h, "application/reports+json", reportingAPIBody) // after Close: pending, no timer
	clock.t = start.Add(5 * time.Minute)
	fire()
	if got := records(t, &buf); len(got) != 0 {
		t.Fatalf("the timer wrote %v after Close, want nothing", got)
	}
	h.Close()
	got := records(t, &buf)
	if len(got) != 1 || got[0]["refused_reports"] != float64(1) {
		t.Fatalf("second Close: records %v, want one summary with refused_reports 1", got)
	}
	if c := h.Counts(); c.RefusedReports != 3 {
		t.Errorf("counts = %+v, want 3 refused reports", c)
	}
}

// TestDroppedRecordRefusedByTheRecordBudget: when the per-client budget
// allows the dropped-count record but the record budget refuses it, no
// dropped-count record is written and the dropped reports are counted as
// refused.
func TestDroppedRecordRefusedByTheRecordBudget(t *testing.T) {
	var buf bytes.Buffer
	d := NewDedupe(10*time.Minute, 16)
	recBudget := &budget{left: 1}
	h := &Handler{Log: recordingLog(&buf), Dedupe: d, AllowRecord: recBudget.allow}
	one := `{"type":"csp-violation","body":{"effectiveDirective":"script-src"}}`
	post(h, "application/reports+json", "["+one+"]") // seeds the violation, takes the record token
	records(t, &buf)
	client := &countingClient{left: 1}
	h.Allow = client.allow
	if rec := post(h, "application/reports+json", manyReports(maxReportsPerRequest+1)); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if client.asked != 1 || client.left != 0 {
		t.Errorf("per-client budget asked %d times, %d left; want asked once and charged", client.asked, client.left)
	}
	if recBudget.asked != 2 {
		t.Errorf("record budget asked %d times, want 2 (the seed and the refused dropped-count record)", recBudget.asked)
	}
	got := byMessage(records(t, &buf))
	if got[DroppedLogMessage] != 0 || got[LogMessage] != 0 || got[RefusedLogMessage] != 1 {
		t.Fatalf("records %v, want only a refusal summary", got)
	}
	if c := h.Counts(); c.RefusedReports != 1 || c.Collapsed != maxReportsPerRequest {
		t.Errorf("counts = %+v, want 1 refused report and %d collapsed", c, maxReportsPerRequest)
	}
}
