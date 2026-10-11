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

package commands

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
)

// unsetUsageSource removes SCION_USAGE_SOURCE for the test (restored after).
func unsetUsageSource(t *testing.T) {
	t.Helper()
	t.Setenv("SCION_USAGE_SOURCE", "")
	if err := os.Unsetenv("SCION_USAGE_SOURCE"); err != nil {
		t.Fatal(err)
	}
}

// The init daemon runs the held-report check under the same condition the
// harness's hooks hold reports under: SCION_USAGE_SOURCE=native in the
// harness's environment, where init's own environment wins over the overlay
// even when the variable is empty (supervisor.mergeEnvOverlay).
func TestNativeUsageSourceSelected(t *testing.T) {
	native := map[string]string{"SCION_USAGE_SOURCE": "native"}
	hooksOverlay := map[string]string{"SCION_USAGE_SOURCE": "hooks"}
	for _, tc := range []struct {
		name    string
		env     *string
		overlay map[string]string
		want    bool
	}{
		{"unset, no overlay", nil, nil, false},
		{"unset, overlay native", nil, native, true},
		{"unset, overlay hooks", nil, hooksOverlay, false},
		{"env native", ptr("native"), nil, true},
		{"env hooks wins over overlay native", ptr("hooks"), native, false},
		{"env empty wins over overlay native", ptr(""), native, false},
		{"env native wins over overlay hooks", ptr("native"), hooksOverlay, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == nil {
				unsetUsageSource(t)
			} else {
				t.Setenv("SCION_USAGE_SOURCE", *tc.env)
			}
			if got := nativeUsageSourceSelected(tc.overlay); got != tc.want {
				t.Errorf("nativeUsageSourceSelected = %v, want %v", got, tc.want)
			}
		})
	}
}

func ptr(s string) *string { return &s }

// fakeTicker replaces heldReportTicker with an unbuffered channel the test
// drives, and counts how many tickers were created and stopped.
type fakeTicker struct {
	ticks   chan time.Time
	created int
	stopped int
}

func useFakeTicker(t *testing.T) *fakeTicker {
	t.Helper()
	f := &fakeTicker{ticks: make(chan time.Time)}
	orig := heldReportTicker
	heldReportTicker = func() (<-chan time.Time, func()) {
		f.created++
		return f.ticks, func() { f.stopped++ }
	}
	t.Cleanup(func() { heldReportTicker = orig })
	return f
}

// writeHeldReports writes a state file with one held report per entry of
// holdUntil, for session IDs held-0, held-1, ...
func writeHeldReports(t *testing.T, home string, holdUntil ...time.Time) {
	t.Helper()
	var pending []map[string]any
	for i, until := range holdUntil {
		pending = append(pending, map[string]any{
			"summary": map[string]any{
				"SessionID":    "held-" + string(rune('0'+i)),
				"StartedAt":    time.Now().Add(-time.Hour).UTC(),
				"EndedAt":      time.Now().Add(-time.Minute).UTC(),
				"Status":       "completed",
				"APICallCount": 5,
				"TokensInput":  500,
			},
			"hold_until": until.UTC(),
			"hold_cap":   until.UTC(),
		})
	}
	data, err := json.Marshal(map[string]any{"version": 1, "aggregator": map[string]any{}, "pending": pending})
	if err != nil {
		t.Fatal(err)
	}
	path := handlers.NewFileSessionState(home).Path
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{path: data, path + ".lock": nil} {
		if err := os.WriteFile(name, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Each tick sends the held reports that are due, once; a report still in
// its quiet period waits. Stop waits for the check in progress, stops the
// ticker, and may be called again.
func TestStartHeldReportSender_SendsDueReportsOnTick(t *testing.T) {
	home, fake, newClient := backstopEnv(t)
	t.Setenv("SCION_USAGE_SOURCE", "native")
	ticker := useFakeTicker(t)
	writeHeldReports(t, home, time.Now().Add(-time.Second), time.Now().Add(time.Hour))

	stop := startHeldReportSender(home, nil, newClient)
	ticker.ticks <- time.Now()
	ticker.ticks <- time.Now() // the second check finds nothing new
	stop()
	stop()

	reports := fake.Reports()
	if len(reports) != 1 || reports[0].Session.ID != "held-0" || reports[0].Tokens.Input != 500 {
		t.Fatalf("reports = %+v, want the due report held-0 once", reports)
	}
	if ticker.created != 1 || ticker.stopped != 1 {
		t.Errorf("tickers created %d, stopped %d; want 1 and 1", ticker.created, ticker.stopped)
	}

	// The report still in its quiet period is left for later, and the
	// shutdown backstop sends it at once.
	reportOpenSessionAtShutdown(home, exitOutcome{}, newClient)
	reports = fake.Reports()
	if len(reports) != 2 || reports[1].Session.ID != "held-1" {
		t.Fatalf("reports after shutdown = %+v, want held-1 sent by the backstop", reports)
	}
	if _, err := os.Stat(handlers.NewFileSessionState(home).Path); !os.IsNotExist(err) {
		t.Errorf("state file after every report was sent: %v, want it absent", err)
	}
}

// No check runs, and no ticker is created, when no hook can hold a report:
// the usage source is not native, the Hub is not configured, or no agent
// home is known.
func TestStartHeldReportSender_NotStartedWithoutNativeUsageOrHub(t *testing.T) {
	home, _, newClient := backstopEnv(t)
	unconfigured := func() *hub.Client { return hub.NewClientWithConfig("", "", "") }
	for _, tc := range []struct {
		name      string
		source    string
		overlay   map[string]string
		home      string
		newClient func() *hub.Client
	}{
		{"source hooks", "hooks", nil, home, newClient},
		{"source unset", "", nil, home, newClient},
		{"no hub", "native", nil, home, unconfigured},
		{"no home", "native", nil, "", newClient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.source == "" {
				unsetUsageSource(t)
			} else {
				t.Setenv("SCION_USAGE_SOURCE", tc.source)
			}
			ticker := useFakeTicker(t)
			stop := startHeldReportSender(tc.home, tc.overlay, tc.newClient)
			stop()
			if ticker.created != 0 {
				t.Errorf("a held-report ticker was created")
			}
		})
	}
	// An overlay that selects native usage does start it.
	unsetUsageSource(t)
	ticker := useFakeTicker(t)
	startHeldReportSender(home, map[string]string{"SCION_USAGE_SOURCE": "native"}, newClient)()
	if ticker.created != 1 {
		t.Errorf("overlay-selected native usage: tickers created %d, want 1", ticker.created)
	}
}

// fakeHeldStore and fakeReporter drive sendDueHeldReports directly.
type fakeHeldStore struct {
	mu        sync.Mutex
	claim     []telemetry.SessionSummary
	claimErr  error
	claims    int
	completed [][]telemetry.SessionSummary
	retried   [][]telemetry.SessionSummary
}

func (f *fakeHeldStore) ClaimDueHeldReportsNoFollow() ([]telemetry.SessionSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	return f.claim, f.claimErr
}

func (f *fakeHeldStore) CompleteReportsNoFollow(s ...telemetry.SessionSummary) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(s) > 0 {
		f.completed = append(f.completed, s)
	}
	return nil
}

func (f *fakeHeldStore) RetryReportsNoFollow(s ...telemetry.SessionSummary) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(s) > 0 {
		f.retried = append(f.retried, s)
	}
	return nil
}

func (f *fakeHeldStore) completedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.completed)
}

type fakeReporter struct {
	sent []string
	fail map[string]bool
}

func (f *fakeReporter) ReportMetrics(_ context.Context, p hub.MetricsPayload) error {
	f.sent = append(f.sent, p.Session.ID)
	if f.fail[p.Session.ID] {
		return errors.New("synthetic hub failure")
	}
	return nil
}

func reporterOf(r metricsReporter) func() metricsReporter { return func() metricsReporter { return r } }

// Every claimed report is sent once. One the Hub accepted is completed; one
// whose send failed is not, but put back to be retried. A failed claim, or
// no configured Hub client, sends nothing (and without a client nothing is
// even claimed).
func TestSendDueHeldReports(t *testing.T) {
	a := telemetry.SessionSummary{SessionID: "a"}
	b := telemetry.SessionSummary{SessionID: "b"}
	store := &fakeHeldStore{claim: []telemetry.SessionSummary{a, b}}
	rep := &fakeReporter{fail: map[string]bool{"a": true}}
	sendDueHeldReports(context.Background(), store, reporterOf(rep))
	if !reflect.DeepEqual(rep.sent, []string{"a", "b"}) {
		t.Errorf("sent %q, want a and b once each", rep.sent)
	}
	if !reflect.DeepEqual(store.completed, [][]telemetry.SessionSummary{{b}}) {
		t.Errorf("completed %+v, want only b", store.completed)
	}
	if !reflect.DeepEqual(store.retried, [][]telemetry.SessionSummary{{a}}) {
		t.Errorf("retried %+v, want only a (its send failed)", store.retried)
	}

	store = &fakeHeldStore{claim: []telemetry.SessionSummary{a}, claimErr: errors.New("synthetic")}
	rep = &fakeReporter{}
	sendDueHeldReports(context.Background(), store, reporterOf(rep))
	if len(rep.sent) != 0 || len(store.completed) != 0 || len(store.retried) != 0 {
		t.Errorf("after a failed claim: sent %q, completed %+v, retried %+v; want nothing", rep.sent, store.completed, store.retried)
	}

	store = &fakeHeldStore{claim: []telemetry.SessionSummary{a}}
	sendDueHeldReports(context.Background(), store, func() metricsReporter { return nil })
	if store.claims != 0 {
		t.Errorf("without a Hub client: %d claims, want none", store.claims)
	}
}

// blockingReporter blocks in ReportMetrics until released.
type blockingReporter struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingReporter) ReportMetrics(context.Context, hub.MetricsPayload) error {
	close(b.entered)
	<-b.release
	return nil
}

// stop waits for the check in progress to finish (here a send that ignores
// cancellation and then succeeds), so that check has completed or put back
// its reports before RunInit goes on to the shutdown backstop and exits.
func TestRunHeldReportSender_StopWaitsForSendInProgress(t *testing.T) {
	store := &fakeHeldStore{claim: []telemetry.SessionSummary{{SessionID: "a"}}}
	rep := &blockingReporter{entered: make(chan struct{}), release: make(chan struct{})}
	ticks := make(chan time.Time)
	stopCalled := make(chan struct{})
	stop := runHeldReportSender(store, reporterOf(rep), ticks, func() { close(stopCalled) })

	ticks <- time.Now()
	<-rep.entered
	stopped := make(chan int)
	go func() {
		stop()
		stopped <- store.completedCount()
	}()
	<-stopCalled
	select {
	case <-stopped:
		t.Fatal("stop returned while a send was in progress")
	case <-time.After(200 * time.Millisecond):
	}
	close(rep.release)
	if n := <-stopped; n != 1 {
		t.Fatalf("reports completed when stop returned = %d, want 1", n)
	}
}

// ctxReporter blocks in ReportMetrics until its context is cancelled.
type ctxReporter struct{ entered chan struct{} }

func (c *ctxReporter) ReportMetrics(ctx context.Context, _ hub.MetricsPayload) error {
	close(c.entered)
	<-ctx.Done()
	return ctx.Err()
}

// stop cancels a send in progress rather than waiting out the Hub call: the
// report is put back to be sent again (by the shutdown backstop), not
// completed, and stop returns once the check has finished.
func TestRunHeldReportSender_StopCancelsSendInProgress(t *testing.T) {
	// Only the cancellation can end the send: its own timeout is far away.
	orig := heldReportSendTimeout
	heldReportSendTimeout = time.Hour
	t.Cleanup(func() { heldReportSendTimeout = orig })
	a := telemetry.SessionSummary{SessionID: "a"}
	store := &fakeHeldStore{claim: []telemetry.SessionSummary{a}}
	rep := &ctxReporter{entered: make(chan struct{})}
	ticks := make(chan time.Time)
	stop := runHeldReportSender(store, reporterOf(rep), ticks, func() {})

	ticks <- time.Now()
	<-rep.entered
	stop()
	if len(store.completed) != 0 || !reflect.DeepEqual(store.retried, [][]telemetry.SessionSummary{{a}}) {
		t.Fatalf("completed %+v, retried %+v; want the cancelled report put back, not completed", store.completed, store.retried)
	}
}

// tokenHub is a fake Hub that accepts session metrics only with one agent
// token, and records the token of every request.
type tokenHub struct {
	mu       sync.Mutex
	accept   string
	tokens   []string
	accepted []hub.MetricsPayload
	// onReject, if set, runs after a request is rejected (outside the lock).
	onReject func()
}

func (h *tokenHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	token := r.Header.Get("X-Scion-Agent-Token")
	h.tokens = append(h.tokens, token)
	if token != h.accept {
		onReject := h.onReject
		h.mu.Unlock()
		if onReject != nil {
			onReject()
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	defer h.mu.Unlock()
	var p hub.MetricsPayload
	if err := json.Unmarshal(body, &p); err == nil {
		h.accepted = append(h.accepted, p)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

func (h *tokenHub) snapshot() ([]string, []hub.MetricsPayload) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.tokens...), append([]hub.MetricsPayload(nil), h.accepted...)
}

// tokenHubEnv points hub.NewClient at a tokenHub, with the token file in a
// test directory holding the synthetic token initial. It returns the agent
// home, the fake Hub and a func that rewrites the token file.
func tokenHubEnv(t *testing.T, initial, accept string) (string, *tokenHub, func(string)) {
	t.Helper()
	home := t.TempDir()
	scrubScionEnv(t)
	t.Setenv("HOME", home)
	log.SetLogPath(filepath.Join(home, "agent.log"))
	t.Cleanup(hub.SetTokenHome(home))
	fake := &tokenHub{accept: accept}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
	t.Setenv("SCION_AGENT_ID", "hook-test-agent")
	t.Setenv("SCION_USAGE_SOURCE", "native")
	writeToken := func(token string) {
		t.Helper()
		if err := hub.WriteTokenFile(token, 0, 0); err != nil {
			t.Fatalf("writing token file: %v", err)
		}
	}
	writeToken(initial)
	return home, fake, writeToken
}

// The agent token rotates after init started the held-report check: the
// next check sends with the current token, not the one from startup.
func TestStartHeldReportSender_UsesCurrentToken(t *testing.T) {
	home, fake, writeToken := tokenHubEnv(t, "synthetic-token-a", "synthetic-token-b")
	ticker := useFakeTicker(t)
	writeHeldReports(t, home, time.Now().Add(-time.Second))

	stop := startHeldReportSender(home, nil, hub.NewClient)
	writeToken("synthetic-token-b")
	ticker.ticks <- time.Now()
	// The ticks are unbuffered: this one is taken only after the first
	// check has finished, so stop (which cancels a send in progress) cannot
	// cut that check short.
	ticker.ticks <- time.Now()
	stop()

	tokens, accepted := fake.snapshot()
	if !reflect.DeepEqual(tokens, []string{"synthetic-token-b"}) || len(accepted) != 1 || accepted[0].Session.ID != "held-0" {
		t.Fatalf("tokens %q, accepted %+v; want held-0 sent once with the rotated token", tokens, accepted)
	}
	if _, err := os.Stat(handlers.NewFileSessionState(home).Path); !os.IsNotExist(err) {
		t.Errorf("state file after the report was sent: %v, want it absent", err)
	}
}

// The Hub rejects the token (401), and the token is then refreshed: the
// report is not completed but kept, and the next check sends it once, with
// the refreshed token. (The fake Hub rotates the token file when it rejects,
// as the daemon's token refresh would; the unbuffered ticks make each check
// finish before the next starts.)
func TestStartHeldReportSender_RejectedSendIsRetried(t *testing.T) {
	home, fake, writeToken := tokenHubEnv(t, "synthetic-token-a", "synthetic-token-b")
	fake.onReject = func() { writeToken("synthetic-token-b") }
	ticker := useFakeTicker(t)
	writeHeldReports(t, home, time.Now().Add(-time.Second))

	stop := startHeldReportSender(home, nil, hub.NewClient)
	ticker.ticks <- time.Now() // rejected; the token is refreshed
	ticker.ticks <- time.Now() // retried with the refreshed token
	ticker.ticks <- time.Now() // nothing left
	stop()

	tokens, accepted := fake.snapshot()
	if len(accepted) != 1 || accepted[0].Session.ID != "held-0" {
		t.Fatalf("accepted %+v, want held-0 once", accepted)
	}
	if !reflect.DeepEqual(tokens, []string{"synthetic-token-a", "synthetic-token-b"}) {
		t.Fatalf("request tokens %q, want the rejected token then the refreshed one", tokens)
	}
	if _, err := os.Stat(handlers.NewFileSessionState(home).Path); !os.IsNotExist(err) {
		t.Errorf("state file after the retry: %v, want it absent", err)
	}
}
