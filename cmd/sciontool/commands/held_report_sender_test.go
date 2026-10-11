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
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
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
	claim     []telemetry.SessionSummary
	claimErr  error
	completed [][]telemetry.SessionSummary
}

func (f *fakeHeldStore) ClaimDueHeldReportsNoFollow() ([]telemetry.SessionSummary, error) {
	return f.claim, f.claimErr
}

func (f *fakeHeldStore) CompleteReportsNoFollow(s ...telemetry.SessionSummary) error {
	f.completed = append(f.completed, s)
	return nil
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

// Every claimed report is sent once and then completed, even when a send
// fails (as the shutdown backstop does, so a rejected report is not retried
// forever); a failed claim sends nothing.
func TestSendDueHeldReports(t *testing.T) {
	a := telemetry.SessionSummary{SessionID: "a"}
	b := telemetry.SessionSummary{SessionID: "b"}
	store := &fakeHeldStore{claim: []telemetry.SessionSummary{a, b}}
	rep := &fakeReporter{fail: map[string]bool{"a": true}}
	sendDueHeldReports(store, rep)
	if !reflect.DeepEqual(rep.sent, []string{"a", "b"}) {
		t.Errorf("sent %q, want a and b once each", rep.sent)
	}
	if len(store.completed) != 1 || len(store.completed[0]) != 2 {
		t.Errorf("completed %+v, want both in one call", store.completed)
	}

	store = &fakeHeldStore{claim: []telemetry.SessionSummary{a}, claimErr: errors.New("synthetic")}
	rep = &fakeReporter{}
	sendDueHeldReports(store, rep)
	if len(rep.sent) != 0 || len(store.completed) != 0 {
		t.Errorf("after a failed claim: sent %q, completed %+v; want nothing", rep.sent, store.completed)
	}
}

// RunInit starts the held-report check after the startup tombstone clear,
// with the agent home and the harness env overlay, and stops it before the
// shutdown backstop, so no check is sending while the backstop runs.
func TestRunInit_StopsHeldReportSenderBeforeBackstop(t *testing.T) {
	var homes []string
	r := pinBackstopCallSites(t, `touch "$1"; sleep 0.5; exit 0`, func(t *testing.T, order *[]string) {
		orig := runStartHeldReportSender
		runStartHeldReportSender = func(home string, _ map[string]string, newClient func() *hub.Client) func() {
			homes = append(homes, home)
			if newClient == nil {
				t.Error("held-report sender got a nil client factory")
			}
			*order = append(*order, "held sender start")
			stopped := false
			return func() {
				if !stopped {
					stopped = true
					*order = append(*order, "held sender stop")
				}
			}
		}
		t.Cleanup(func() { runStartHeldReportSender = orig })
	})
	if len(homes) != 1 || len(r.clearHomes) != 1 || homes[0] != r.clearHomes[0] {
		t.Fatalf("start calls = %q, want one with the agent home %q", homes, r.clearHomes)
	}
	want := []string{"clear", "held sender start", "held sender stop", "backstop", "stopping report", "sidecar shutdown", "session-end hooks"}
	if !reflect.DeepEqual(r.order, want) {
		t.Errorf("call order = %q, want %q", r.order, want)
	}
}
