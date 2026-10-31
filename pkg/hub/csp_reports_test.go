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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/cspreport"
)

const routedCSPReport = `[{"type":"csp-violation","body":{"effectiveDirective":"require-trusted-types-for",` +
	`"blockedURL":"trusted-types-sink","sourceFile":"https://hub.example/assets/main.js","lineNumber":3,"sample":"x"}}]`

// routedCSPReportN is a report that is its own violation for each n, so
// the hub does not collapse it as a repeat of another n.
func routedCSPReportN(n int) string {
	return fmt.Sprintf(`[{"type":"csp-violation","body":{"effectiveDirective":"script-src-%d"}}]`, n)
}

// postCSPReport sends a report, without signing in, from remoteAddr.
func postCSPReport(h http.Handler, remoteAddr, body string) *httptest.ResponseRecorder {
	return postCSPReportAs(h, remoteAddr, "application/reports+json", body)
}

// postCSPReportAs sends a report with contentType, without signing in,
// from remoteAddr.
func postCSPReportAs(h http.Handler, remoteAddr, contentType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/csp-reports", strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// cspRecordCount counts csp_violation records in buf.
func cspRecordCount(t *testing.T, buf *bytes.Buffer) int {
	t.Helper()
	n := 0
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for dec.More() {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			t.Fatalf("decoding log record: %v", err)
		}
		if rec["msg"] == cspreport.LogMessage {
			n++
		}
	}
	return n
}

// cspMessageCount counts the records with message msg in buf.
func cspMessageCount(t *testing.T, buf *bytes.Buffer, msg string) int {
	t.Helper()
	n := 0
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for dec.More() {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			t.Fatalf("decoding log record: %v", err)
		}
		if rec["msg"] == msg {
			n++
		}
	}
	return n
}

// cspDroppedCounts returns the dropped values of the csp_violation_dropped
// records in buf.
func cspDroppedCounts(t *testing.T, buf *bytes.Buffer) []float64 {
	t.Helper()
	var out []float64
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for dec.More() {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			t.Fatalf("decoding log record: %v", err)
		}
		if rec["msg"] == cspreport.DroppedLogMessage {
			d, _ := rec["dropped"].(float64)
			out = append(out, d)
		}
	}
	return out
}

// routedCSPServer is a test hub whose report records go to buf.
func routedCSPServer(t *testing.T, buf *bytes.Buffer) *Server {
	t.Helper()
	srv, _ := testServer(t)
	if srv.cspReports == nil {
		t.Fatal("New did not set up the report endpoint")
	}
	srv.cspReports.Log = slog.New(slog.NewJSONHandler(buf, nil))
	return srv
}

// TestCSPReportRouteNeedsNoSignIn: the hub accepts a report sent without
// signing in and logs it; other methods are refused.
func TestCSPReportRouteNeedsNoSignIn(t *testing.T) {
	var buf bytes.Buffer
	srv := routedCSPServer(t, &buf)

	rec := postCSPReport(srv.Handler(), "192.0.2.10:5000", routedCSPReport)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body %q", rec.Code, rec.Body.String())
	}
	if n := cspRecordCount(t, &buf); n != 1 {
		t.Fatalf("%d records logged, want 1", n)
	}

	get := httptest.NewRequest(http.MethodGet, "/api/v1/csp-reports", nil)
	getRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(getRec, get)
	if getRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: status = %d, want 405", getRec.Code)
	}

	// Negative control: the same signed-out request to a route that needs
	// a signed-in caller is refused, so the 204 above comes from the route
	// being open, not from the test request being signed in.
	if rec := signedOutGet(srv.Handler(), "/api/v1/experiments"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("protected route without sign-in: status = %d, want 401", rec.Code)
	}
}

// signedOutGet sends a GET without signing in.
func signedOutGet(h http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "192.0.2.99:5000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestCSPReportRouteRateLimited: past its burst, one client's report is
// refused: it is answered 204 like any other, is not logged, and shows in
// one refusal summary record; the requests before it (each its own
// violation) were logged.
func TestCSPReportRouteRateLimited(t *testing.T) {
	var buf bytes.Buffer
	srv := routedCSPServer(t, &buf)
	// A fixed clock, so no bucket refills while the burst is sent.
	now := time.Unix(1_700_000_000, 0)
	srv.cspReportLimiter.perIP.nowFunc = func() time.Time { return now }
	srv.cspReportLimiter.global.nowFunc = func() time.Time { return now }

	for i := 0; i < cspReportBurst; i++ {
		if rec := postCSPReport(srv.Handler(), "192.0.2.20:5000", routedCSPReportN(i)); rec.Code != http.StatusNoContent {
			t.Fatalf("request %d: status = %d, want 204", i+1, rec.Code)
		}
	}
	if n := cspRecordCount(t, &buf); n != cspReportBurst {
		t.Fatalf("%d records logged within the burst, want %d", n, cspReportBurst)
	}
	buf.Reset()
	rec := postCSPReport(srv.Handler(), "192.0.2.20:5000", routedCSPReportN(cspReportBurst))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("past the burst: status = %d, want 204", rec.Code)
	}
	if n := cspRecordCount(t, &buf); n != 0 {
		t.Fatalf("%d records for the refused report, want none", n)
	}
	if n := cspMessageCount(t, &buf, cspreport.RefusedLogMessage); n != 1 {
		t.Fatalf("%d refusal summary records, want 1", n)
	}
	if c := srv.cspReports.Counts(); c.RefusedReports != 1 {
		t.Fatalf("counts = %+v, want one refused report", c)
	}
}

// TestCSPReportRouteRepeatFloodIsNotCharged: one client sends a read
// ceiling's worth of one identical report, as a page that repeats a
// violation does. Every request gets 204 and one record is written; the
// repeats are not charged to the client's budget, so its next distinct
// reports are still logged.
func TestCSPReportRouteRepeatFloodIsNotCharged(t *testing.T) {
	var buf bytes.Buffer
	srv := routedCSPServer(t, &buf)
	now := time.Unix(1_700_000_000, 0)
	readNow := now
	srv.cspReportLimiter.read.nowFunc = func() time.Time { return readNow }
	srv.cspReportLimiter.perIP.nowFunc = func() time.Time { return now }
	srv.cspReportLimiter.global.nowFunc = func() time.Time { return now }

	const flood = cspReportReadBurst
	if flood <= cspReportBurst {
		t.Fatalf("test setup: the flood (%d) must exceed the per-client burst (%d)", flood, cspReportBurst)
	}
	for i := 0; i < flood; i++ {
		rec := postCSPReportAs(srv.Handler(), "192.0.2.70:5000", "application/csp-report", safariCSPReport)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("report %d: status = %d, want 204", i+1, rec.Code)
		}
	}
	if n := cspRecordCount(t, &buf); n != 1 {
		t.Fatalf("%d records for %d identical reports, want 1", n, flood)
	}
	if c := srv.cspReports.Counts(); c.Collapsed != flood-1 || c.RefusedReports != 0 || c.RefusedRequests != 0 {
		t.Fatalf("counts = %+v, want %d collapsed and none refused", c, flood-1)
	}
	// The flood took one request of the client's budget. The read ceiling
	// refills (its clock moves on); the per-client budget does not.
	readNow = readNow.Add(time.Minute)
	buf.Reset()
	for i := 0; i < cspReportBurst-1; i++ {
		if rec := postCSPReport(srv.Handler(), "192.0.2.70:5000", routedCSPReportN(i)); rec.Code != http.StatusNoContent {
			t.Fatalf("distinct report %d: status = %d, want 204", i+1, rec.Code)
		}
	}
	if n := cspRecordCount(t, &buf); n != cspReportBurst-1 {
		t.Fatalf("%d distinct reports logged after the flood, want %d", n, cspReportBurst-1)
	}
	// Positive control: the budget is now used up.
	buf.Reset()
	postCSPReport(srv.Handler(), "192.0.2.70:5000", routedCSPReportN(cspReportBurst))
	if n := cspRecordCount(t, &buf); n != 0 {
		t.Fatalf("a report past the budget was logged")
	}
}

// TestCSPReportRouteReadCeiling: past its read ceiling, a client's request
// is answered 204 without being read, even one that would otherwise get
// 400, and is counted in the refusal summary; another client is served.
func TestCSPReportRouteReadCeiling(t *testing.T) {
	var buf bytes.Buffer
	srv := routedCSPServer(t, &buf)
	now := time.Unix(1_700_000_000, 0)
	srv.cspReportLimiter.read.nowFunc = func() time.Time { return now }
	srv.cspReportLimiter.perIP.nowFunc = func() time.Time { return now }
	srv.cspReportLimiter.global.nowFunc = func() time.Time { return now }

	for i := 0; i < cspReportReadBurst; i++ {
		rec := postCSPReportAs(srv.Handler(), "192.0.2.80:5000", "application/csp-report", safariCSPReport)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("report %d: status = %d, want 204", i+1, rec.Code)
		}
	}
	buf.Reset()
	// Within the ceiling this body gets 400 (TestCSPReportRouteBodyCapAndMalformed).
	rec := postCSPReport(srv.Handler(), "192.0.2.80:5000", `{"not":"a report"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("past the read ceiling: status = %d, want 204", rec.Code)
	}
	if n := cspMessageCount(t, &buf, cspreport.RefusedLogMessage); n != 1 {
		t.Fatalf("%d refusal summary records, want 1", n)
	}
	if c := srv.cspReports.Counts(); c.RefusedRequests != 1 {
		t.Fatalf("counts = %+v, want one refused request", c)
	}
	// Positive control: another client is served, and its bad body gets 400.
	if rec := postCSPReport(srv.Handler(), "192.0.2.81:5000", `{"not":"a report"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("another client: status = %d, want 400", rec.Code)
	}
}

// TestCSPReportRouteBodyCapAndMalformed: the route applies the body cap
// and refuses malformed reports, logging nothing for either.
func TestCSPReportRouteBodyCapAndMalformed(t *testing.T) {
	var buf bytes.Buffer
	srv := routedCSPServer(t, &buf)

	big := routedCSPReport + strings.Repeat(" ", cspreport.MaxBodyBytes)
	if rec := postCSPReport(srv.Handler(), "192.0.2.30:5000", big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the cap: status = %d, want 413", rec.Code)
	}
	if rec := postCSPReport(srv.Handler(), "192.0.2.30:5000", "{"); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed: status = %d, want 400", rec.Code)
	}
	if n := cspRecordCount(t, &buf); n != 0 {
		t.Fatalf("%d records logged, want none", n)
	}
	// Positive control: a well-formed report on the same route is logged.
	if rec := postCSPReport(srv.Handler(), "192.0.2.30:5000", routedCSPReport); rec.Code != http.StatusNoContent {
		t.Fatalf("well-formed: status = %d, want 204", rec.Code)
	}
	if n := cspRecordCount(t, &buf); n != 1 {
		t.Fatalf("%d records logged, want 1", n)
	}
}

// TestCSPReportThroughWebServer: on the web server, where browsers send
// reports, the route reaches the hub without sign-in.
func TestCSPReportThroughWebServer(t *testing.T) {
	var buf bytes.Buffer
	srv := routedCSPServer(t, &buf)
	ws := newTestWebServer(t, WebServerConfig{})
	ws.MountHubAPI(srv.Handler(), func(context.Context) error { return nil })

	rec := postCSPReport(ws.Handler(), "192.0.2.40:5000", routedCSPReport)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body %q", rec.Code, rec.Body.String())
	}
	if n := cspRecordCount(t, &buf); n != 1 {
		t.Fatalf("%d records logged, want 1", n)
	}

	// Negative control: a route that needs a signed-in caller refuses the
	// same signed-out request on the web server.
	if rec := signedOutGet(ws.Handler(), "/api/v1/experiments"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("protected route without sign-in: status = %d, want 401", rec.Code)
	}
}

// TestCSPReportRouteChargesHubWidePerRecord: a request with more reports
// than the per-request cap takes one hub-wide token per record written,
// the dropped-count record included.
func TestCSPReportRouteChargesHubWidePerRecord(t *testing.T) {
	var buf bytes.Buffer
	srv := routedCSPServer(t, &buf)
	now := time.Unix(1_700_000_000, 0)
	srv.cspReportLimiter.perIP.nowFunc = func() time.Time { return now }
	srv.cspReportLimiter.global.nowFunc = func() time.Time { return now }

	// Each report is its own violation, so none is collapsed as a repeat.
	parts := make([]string, 15)
	for i := range parts {
		parts[i] = fmt.Sprintf(`{"type":"csp-violation","body":{"effectiveDirective":"script-src-%d"}}`, i)
	}
	if rec := postCSPReport(srv.Handler(), "192.0.2.50:5000", "["+strings.Join(parts, ",")+"]"); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	// 10 report records and one dropped-count record.
	if n := cspRecordCount(t, &buf); n != 10 {
		t.Fatalf("%d csp_violation records, want 10", n)
	}
	if d := cspDroppedCounts(t, &buf); len(d) != 1 || d[0] != 5 {
		t.Fatalf("dropped-count records %v, want one with dropped=5", d)
	}
	const written = 11
	left := cspReportGlobalBurst - written
	for i := 0; i < left; i++ {
		if !srv.cspReportLimiter.AllowRecord() {
			t.Fatalf("hub-wide token %d of the %d left was refused", i+1, left)
		}
	}
	if srv.cspReportLimiter.AllowRecord() {
		t.Fatalf("more than %d hub-wide tokens were left: the request was not charged per record", left)
	}
}

// safariCSPReport is one report as Safari sends it to the report-to
// endpoint: a single object (no array) with Content-Type
// application/csp-report.
const safariCSPReport = `{"type":"csp-violation","url":"https://hub.example/projects/p1/chat/c1",` +
	`"body":{"documentURL":"https://hub.example/projects/p1/chat/c1","disposition":"report","referrer":"",` +
	`"effectiveDirective":"require-trusted-types-for","blockedURL":"trusted-types-sink",` +
	`"originalPolicy":"require-trusted-types-for 'script'; report-to csp","statusCode":200,"sample":""}}`

// TestCSPReportRouteCollapsesOnePageLoad: one page load's worth of
// identical reports from one client, each sent as its own request in
// Safari's shape, all get 204 and give one record.
func TestCSPReportRouteCollapsesOnePageLoad(t *testing.T) {
	var buf bytes.Buffer
	srv := routedCSPServer(t, &buf)
	now := time.Unix(1_700_000_000, 0)
	srv.cspReportLimiter.perIP.nowFunc = func() time.Time { return now }
	srv.cspReportLimiter.global.nowFunc = func() time.Time { return now }

	for i := 0; i < cspReportBurst; i++ {
		rec := postCSPReportAs(srv.Handler(), "192.0.2.60:5000", "application/csp-report", safariCSPReport)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("report %d: status = %d, want 204; body %q", i+1, rec.Code, rec.Body.String())
		}
	}
	if n := cspRecordCount(t, &buf); n != 1 {
		t.Fatalf("%d records for %d identical reports, want 1", n, cspReportBurst)
	}
	// Repeats take no hub-wide record token: only the one record did.
	left := cspReportGlobalBurst - 1
	for i := 0; i < left; i++ {
		if !srv.cspReportLimiter.AllowRecord() {
			t.Fatalf("hub-wide token %d of the %d left was refused", i+1, left)
		}
	}
	if srv.cspReportLimiter.AllowRecord() {
		t.Fatal("more hub-wide tokens than the burst minus one record")
	}
}

// TestCSPReportHandlerCollapsesRepeats: the hub's report handler collapses
// repeats.
func TestCSPReportHandlerCollapsesRepeats(t *testing.T) {
	if newCSPReportHandler(newCSPReportLimiter(nil), false).Dedupe == nil {
		t.Fatal("the report handler logs every repeat")
	}
}

// TestCSPReportMetricsExportCounts: the report endpoint's refused and
// collapsed totals are read by the OTel counters at collection time.
func TestCSPReportMetricsExportCounts(t *testing.T) {
	var buf bytes.Buffer
	srv := routedCSPServer(t, &buf)
	now := time.Unix(1_700_000_000, 0)
	srv.cspReportLimiter.read.nowFunc = func() time.Time { return now }
	srv.cspReportLimiter.perIP.nowFunc = func() time.Time { return now }
	srv.cspReportLimiter.global.nowFunc = func() time.Time { return now }

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if err := srv.RegisterCSPReportMetrics(mp); err != nil {
		t.Fatalf("RegisterCSPReportMetrics: %v", err)
	}
	collect := func() map[string]int64 {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect: %v", err)
		}
		out := map[string]int64{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok || !strings.HasPrefix(m.Name, "scion.hub.csp_reports.") {
					continue
				}
				if !sum.IsMonotonic {
					t.Errorf("%s is not a monotonic counter", m.Name)
				}
				for _, dp := range sum.DataPoints {
					out[m.Name] += dp.Value
				}
			}
		}
		return out
	}

	// Three identical reports (two collapsed), then the client's budget
	// used up with distinct reports, then one refused report.
	for i := 0; i < 3; i++ {
		postCSPReportAs(srv.Handler(), "192.0.2.90:5000", "application/csp-report", safariCSPReport)
	}
	for i := 0; i < cspReportBurst-1; i++ {
		postCSPReport(srv.Handler(), "192.0.2.90:5000", routedCSPReportN(i))
	}
	postCSPReport(srv.Handler(), "192.0.2.90:5000", routedCSPReportN(cspReportBurst))
	got := collect()
	want := map[string]int64{
		"scion.hub.csp_reports.refused_requests": 0,
		"scion.hub.csp_reports.refused_reports":  1,
		"scion.hub.csp_reports.collapsed":        2,
	}
	for name, v := range want {
		if g, ok := got[name]; !ok || g != v {
			t.Errorf("%s = %d (present %v), want %d", name, g, ok, v)
		}
	}
	if err := srv.RegisterCSPReportMetrics(nil); err == nil {
		t.Error("a nil MeterProvider was accepted")
	}
}

// TestCSPReportPendingRefusalsWrittenOnCleanup: the hub's background
// cleanup closes the report handler, which writes the refusals still
// pending.
func TestCSPReportPendingRefusalsWrittenOnCleanup(t *testing.T) {
	var buf bytes.Buffer
	srv := routedCSPServer(t, &buf)
	now := time.Unix(1_700_000_000, 0)
	srv.cspReportLimiter.read.nowFunc = func() time.Time { return now }
	srv.cspReportLimiter.perIP.nowFunc = func() time.Time { return now }
	srv.cspReportLimiter.global.nowFunc = func() time.Time { return now }

	for i := 0; i < cspReportBurst; i++ {
		postCSPReport(srv.Handler(), "192.0.2.95:5000", routedCSPReportN(i))
	}
	// Two refusals: the first is summarized at once, the second is left
	// pending for up to a minute.
	postCSPReport(srv.Handler(), "192.0.2.95:5000", routedCSPReportN(cspReportBurst))
	postCSPReport(srv.Handler(), "192.0.2.95:5000", routedCSPReportN(cspReportBurst+1))
	if n := cspMessageCount(t, &buf, cspreport.RefusedLogMessage); n != 1 {
		t.Fatalf("%d refusal summaries before cleanup, want 1", n)
	}
	if err := srv.CleanupBackgroundResources(context.Background()); err != nil {
		t.Fatalf("CleanupBackgroundResources: %v", err)
	}
	if n := cspMessageCount(t, &buf, cspreport.RefusedLogMessage); n != 2 {
		t.Fatalf("%d refusal summaries after cleanup, want 2 (the pending refusal written)", n)
	}
}

// TestCSPReportRefusalsDuringTheDrainWrittenAfterIt: combined mode runs the
// background cleanup (which closes the report handler) before the HTTP
// drain; a refusal counted during the drain is written by the after-drain
// close (CloseAuditWriter), once.
func TestCSPReportRefusalsDuringTheDrainWrittenAfterIt(t *testing.T) {
	var buf bytes.Buffer
	srv := routedCSPServer(t, &buf)
	now := time.Unix(1_700_000_000, 0)
	srv.cspReportLimiter.read.nowFunc = func() time.Time { return now }
	srv.cspReportLimiter.perIP.nowFunc = func() time.Time { return now }
	srv.cspReportLimiter.global.nowFunc = func() time.Time { return now }

	for i := 0; i < cspReportBurst; i++ {
		postCSPReport(srv.Handler(), "192.0.2.96:5000", routedCSPReportN(i))
	}
	postCSPReport(srv.Handler(), "192.0.2.96:5000", routedCSPReportN(cspReportBurst)) // summarized at once
	if err := srv.CleanupBackgroundResources(context.Background()); err != nil {
		t.Fatalf("CleanupBackgroundResources: %v", err)
	}
	buf.Reset()
	// During the drain: refused and counted, left pending (within the minute).
	postCSPReport(srv.Handler(), "192.0.2.96:5000", routedCSPReportN(cspReportBurst+1))
	if n := cspMessageCount(t, &buf, cspreport.RefusedLogMessage); n != 0 {
		t.Fatalf("%d refusal summaries during the drain, want 0", n)
	}
	if err := srv.CloseAuditWriter(context.Background()); err != nil {
		t.Fatalf("CloseAuditWriter: %v", err)
	}
	if n := cspMessageCount(t, &buf, cspreport.RefusedLogMessage); n != 1 {
		t.Fatalf("%d refusal summaries after the drain, want 1", n)
	}
	_ = srv.CloseAuditWriter(context.Background())
	if n := cspMessageCount(t, &buf, cspreport.RefusedLogMessage); n != 1 {
		t.Fatalf("%d refusal summaries after a second close, want still 1", n)
	}
	if c := srv.cspReports.Counts(); c.RefusedReports != 2 {
		t.Fatalf("counts = %+v, want 2 refused reports", c)
	}
}
