/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// Health hub origin under WP2's identity guard (WP3 hub-origin plan v5):
// hub_id is this process's own resolution (process-fixed, held while
// unresolved); the outbox-subject group describes the outbox's origin and
// its sticky relocation marker; frozen bytes are never restamped.

// switchableHub is a resolver whose answer the test changes.
type switchableHub struct {
	mu  sync.Mutex
	hub string
	err error
}

func (s *switchableHub) set(hub string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hub, s.err = hub, err
}

func (s *switchableHub) resolve(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hub, s.err
}

// originHarness is a health pipeline with a REAL outbox drainer as its hub
// source (no hub override).
type originHarness struct {
	p       *Pipeline
	outbox  *AnalyticsOutbox
	drainer *AnalyticsDrainer
	mu      sync.Mutex
	sent    [][]byte
	fail    bool
}

func newOriginHarness(t *testing.T, o *AnalyticsOutbox, resolve HubIDResolver) *originHarness {
	t.Helper()
	setAnalyticsEnv(t, "claude")
	clearUsageSourceEnv(t)
	setTestVersion(t)
	h := &originHarness{outbox: o}
	h.drainer = NewAnalyticsDrainer(o, func(context.Context, []byte) error { return nil }, resolve)
	p := NewWithConfig(&Config{Enabled: true})
	p.health.sendOverride = func(_ context.Context, line []byte) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.fail {
			return errors.New("logging down")
		}
		h.sent = append(h.sent, append([]byte(nil), line...))
		return nil
	}
	p.health.periodic, p.health.retry = 1<<62, 1<<62 // driven by hand
	h.p = p
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { p.stopHealthLoop(); cancel() })
	handles := analyticsHandles{native: NewUsageAnalytics(o, telemetrycontract.UsageSourceNative), drainer: h.drainer, send: newCountingSend(nil)}
	p.startHealth(ctx, &GCPExporter{}, handles)
	p.stopHealthLoop()
	settleNow(p, nil, "")
	return h
}

func (h *originHarness) setFail(fail bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fail = fail
}

func (h *originHarness) sentEvents(t *testing.T) []telemetrycontract.HealthEvent {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []telemetrycontract.HealthEvent
	for _, l := range h.sent {
		var ev telemetrycontract.HealthEvent
		if err := telemetrycontract.Unmarshal(l, &ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

// periodic builds one periodic snapshot and tries to deliver the queue.
func (h *originHarness) periodic() {
	h.p.emitHealth(telemetrycontract.HealthTriggerPeriodic)
	h.p.flushHealth(context.Background())
}

func (h *originHarness) last(t *testing.T) telemetrycontract.HealthEvent {
	t.Helper()
	evs := h.sentEvents(t)
	if len(evs) == 0 {
		t.Fatal("no health event sent")
	}
	return evs[len(evs)-1]
}

func relocatedValue(ev telemetrycontract.HealthEvent) string {
	if ev.OutboxRelocated == nil {
		return "unknown"
	}
	return fmt.Sprint(*ev.OutboxRelocated)
}

// pinnedOutbox returns an outbox pinned to hub by a first process.
func pinnedOutbox(t *testing.T, hub string) *AnalyticsOutbox {
	t.Helper()
	o, err := OpenAnalyticsOutbox(filepath.Join(t.TempDir(), "ob"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.PinHubID(hub); err != nil {
		t.Fatal(err)
	}
	return o
}

// relocateFromOtherProcess has another process's drainer (its own outbox
// handle on the same directory) observe hub, confirm it after the debounce
// window and record a relocation.
func relocateFromOtherProcess(t *testing.T, o *AnalyticsOutbox, hub string) {
	t.Helper()
	d := NewAnalyticsDrainer(reopen(t, o), func(context.Context, []byte) error { return nil }, trustedHub(hub))
	runConfirmed(d)
	if d.ObservedHubID() != hub || d.VerifiedHubID() != "" {
		t.Fatalf("other process: observed %q verified %q", d.ObservedHubID(), d.VerifiedHubID())
	}
}

// Fixture 1: unresolved startup holds every event; nothing is frozen or
// sent, and no outbox-subject field is reported.
func TestHealthOriginUnresolvedStartupHolds(t *testing.T) {
	res := &switchableHub{err: errors.New("healthz unreachable")}
	h := newOriginHarness(t, pinnedOutbox(t, "hub-a"), res.resolve)
	h.drainer.RunOnce(context.Background())
	h.periodic()
	if n := len(h.sentEvents(t)); n != 0 {
		t.Fatalf("unresolved: %d events sent, want 0 (held)", n)
	}
	for i, item := range h.p.health.queue {
		if item.line != nil {
			t.Fatalf("queued event %d frozen before resolution", i)
		}
	}
	if c := h.p.health.analyticsHandles.counters(h.p.health.now()); c.Outbox != nil {
		t.Fatalf("outbox-subject group reported before resolution: %+v", c.Outbox)
	}
}

// Fixture 2 (WP4 H1): same hub: frozen with this process's hub (= the pin),
// relocated=false, counters 0.
func TestHealthOriginSameHub(t *testing.T) {
	h := newOriginHarness(t, pinnedOutbox(t, "hub-a"), trustedHub("hub-a"))
	h.drainer.RunOnce(context.Background())
	h.periodic()
	ev := h.last(t)
	if ev.HubID != "hub-a" || ev.OutboxPinnedHubID != "hub-a" || relocatedValue(ev) != "false" {
		t.Fatalf("same hub: hub_id %q pinned %q relocated %s", ev.HubID, ev.OutboxPinnedHubID, relocatedValue(ev))
	}
	if ev.OutboxHubMismatchTotal == nil || *ev.OutboxHubMismatchTotal != 0 || ev.OutboxRelocatedRefusedTotal == nil || *ev.OutboxRelocatedRefusedTotal != 0 {
		t.Fatalf("same hub counters: %v %v", ev.OutboxHubMismatchTotal, ev.OutboxRelocatedRefusedTotal)
	}
}

// Fixture 3 (WP4 H2): a copied home on another hub: health carries the NEW
// process hub, the outbox subject keeps the old pin with relocated=true;
// nothing retroactive (the pin is not rewritten, usage stays refused).
func TestHealthOriginRelocated(t *testing.T) {
	origin := pinnedOutbox(t, "hub-a")
	cp := copyOutbox(t, origin)
	h := newOriginHarness(t, cp, trustedHub("hub-z"))
	runConfirmed(h.drainer)
	h.periodic()
	ev := h.last(t)
	if ev.HubID != "hub-z" || ev.OutboxPinnedHubID != "hub-a" || relocatedValue(ev) != "true" {
		t.Fatalf("relocated: hub_id %q pinned %q relocated %s", ev.HubID, ev.OutboxPinnedHubID, relocatedValue(ev))
	}
	if ev.OutboxHubMismatchTotal == nil || *ev.OutboxHubMismatchTotal != 1 {
		t.Fatalf("hub mismatch total %v, want 1", ev.OutboxHubMismatchTotal)
	}
	if l := ledger(t, reopen(t, cp)); l.PinnedHubID != "hub-a" {
		t.Fatalf("the pin must never be rewritten: %q", l.PinnedHubID)
	}
	if got, err := cp.AppendUsage(draftFor("a", "rid1:after", outboxTestTS)); err != nil || got != AppendRefusedRelocated {
		t.Fatalf("usage on the relocated outbox = %d, %v; want refused", got, err)
	}
}

// Fixtures 4 and 9: a send fails, THEN another process records a relocation
// on the same outbox. The retry resends the original frozen bytes; only a
// NEW event shows relocated=true (the sticky marker wins over this process's
// cached verification), with this process's hub.
func TestHealthOriginRetryImmutableAcrossOtherProcessRelocation(t *testing.T) {
	o := pinnedOutbox(t, "hub-a")
	h := newOriginHarness(t, o, trustedHub("hub-a"))
	h.drainer.RunOnce(context.Background())
	h.p.flushHealth(context.Background()) // deliver the start event (built before resolution)
	h.setFail(true)
	h.periodic()
	if len(h.p.health.queue) == 0 || h.p.health.queue[0].line == nil {
		t.Fatal("the first send attempt must freeze the bytes")
	}
	frozen := append([]byte(nil), h.p.health.queue[0].line...)
	relocateFromOtherProcess(t, o, "hub-z")
	if h.drainer.VerifiedHubID() != "hub-a" {
		t.Fatal("this process's cached verification is unchanged by another process")
	}
	before := len(h.sentEvents(t))
	h.setFail(false)
	h.p.flushHealth(context.Background())
	evs := h.sentEvents(t)
	if len(evs) <= before || string(h.sent[before]) != string(frozen) {
		t.Fatal("the retry must send the original frozen bytes")
	}
	if relocatedValue(evs[before]) != "false" || evs[before].HubID != "hub-a" {
		t.Fatalf("frozen event changed: hub %q relocated %s", evs[before].HubID, relocatedValue(evs[before]))
	}
	h.periodic()
	next := h.last(t)
	if next.HubID != "hub-a" || relocatedValue(next) != "true" || next.OutboxPinnedHubID != "hub-a" {
		t.Fatalf("new event after the marker: hub %q pinned %q relocated %s", next.HubID, next.OutboxPinnedHubID, relocatedValue(next))
	}
}

// Fixture 5, outbox mode: the resolver changes after a failed first send.
// The retry keeps the frozen hub, and a NEW event also keeps the process
// hub: the drainer resolves once per process.
func TestHealthOriginResolverChangeOutboxMode(t *testing.T) {
	res := &switchableHub{hub: "hub-a"}
	h := newOriginHarness(t, pinnedOutbox(t, "hub-a"), res.resolve)
	h.drainer.RunOnce(context.Background())
	h.setFail(true)
	h.periodic()
	frozen := append([]byte(nil), h.p.health.queue[0].line...)
	res.set("hub-b", nil)
	h.drainer.RunOnce(context.Background())
	h.setFail(false)
	h.p.flushHealth(context.Background())
	if len(h.sent) == 0 || string(h.sent[0]) != string(frozen) {
		t.Fatal("outbox mode: the retry must send the frozen bytes")
	}
	h.periodic()
	if ev := h.last(t); ev.HubID != "hub-a" {
		t.Fatalf("outbox mode: a new event switched to %q; the process origin is fixed", ev.HubID)
	}
}

// Fixture 5, cached mode (no outbox): the same, through the cached /healthz
// resolution.
func TestHealthOriginResolverChangeCachedMode(t *testing.T) {
	h := newHealthHarness(t)
	res := &switchableHub{hub: "hub-a"}
	h.p.health.hubID = cachedHubID(res.resolve)
	h.mu.Lock()
	h.fail = true
	h.mu.Unlock()
	h.p.emitHealth(telemetrycontract.HealthTriggerPeriodic)
	h.p.flushHealth(context.Background())
	frozen := append([]byte(nil), h.p.health.queue[0].line...)
	res.set("hub-b", nil)
	h.mu.Lock()
	h.fail = false
	h.mu.Unlock()
	h.p.flushHealth(context.Background())
	if len(h.sent) == 0 || string(h.sent[0]) != string(frozen) {
		t.Fatal("cached mode: the retry must send the frozen bytes")
	}
	h.p.emitHealth(telemetrycontract.HealthTriggerPeriodic)
	h.p.flushHealth(context.Background())
	evs := h.sentEvents(t)
	if ev := evs[len(evs)-1]; ev.HubID != "hub-a" {
		t.Fatalf("cached mode: a new event switched to %q; the first resolution is kept", ev.HubID)
	}
	for _, ev := range evs {
		if ev.OutboxPinnedHubID != "" || ev.OutboxRelocated != nil {
			t.Fatalf("no outbox subject without a drainer: %+v", ev)
		}
	}
}

// Fixture 6 (WP4 H6): a relocation detected concurrently with health builds
// and freezes, repeated, in both process roles.
//
// Relocated process (its drainer confirms hub-z against pin hub-a, so it
// never verifies): the marker is asserted to be recorded, and no event ever
// reports relocated=false; all carry hub-z and none is rejected.
//
// Verified process (hub-a = pin) while another process records the
// relocation concurrently: relocated=false IS reachable (an event is built
// before the marker), every false event carries hub_id == the pin, no false
// follows a true in health_seq order (the marker is sticky), and the last
// event is true.
func TestHealthOriginConcurrentRelocationNeverFalseWithNewHub(t *testing.T) {
	origin := pinnedOutbox(t, "hub-a")
	for round := 0; round < 25; round++ {
		cp := copyOutbox(t, origin)
		h := newOriginHarness(t, cp, trustedHub("hub-z"))
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			runConfirmed(h.drainer)
		}()
		for i := 0; i < 4; i++ {
			h.periodic()
		}
		wg.Wait()
		h.periodic()
		if l := ledger(t, reopen(t, cp)); l.RelocatedHubID != "hub-z" {
			t.Fatalf("round %d: the relocation marker must be recorded first: %q", round, l.RelocatedHubID)
		}
		for _, ev := range h.sentEvents(t) {
			if relocatedValue(ev) == "false" {
				t.Fatalf("round %d: a relocated process reported relocated=false (hub %q, pin %q)", round, ev.HubID, ev.OutboxPinnedHubID)
			}
			if ev.HubID != "hub-z" {
				t.Fatalf("round %d: hub_id %q, want this process's hub-z", round, ev.HubID)
			}
		}
		if h.p.health.rejected != 0 {
			t.Fatalf("round %d: %d events rejected", round, h.p.health.rejected)
		}
		if ev := h.last(t); relocatedValue(ev) != "true" {
			t.Fatalf("round %d: the event after the relocation reports relocated %s", round, relocatedValue(ev))
		}
	}

	for round := 0; round < 25; round++ {
		o := pinnedOutbox(t, "hub-a")
		h := newOriginHarness(t, o, trustedHub("hub-a"))
		h.drainer.RunOnce(context.Background()) // verified: hub-a = pin
		h.periodic()                            // built before any marker: false is reachable
		other := reopen(t, o)
		var wg sync.WaitGroup
		var relocErr error
		wg.Add(1)
		go func() {
			defer wg.Done()
			relocErr = other.recordRelocation("hub-z")
		}()
		for i := 0; i < 4; i++ {
			h.periodic()
		}
		wg.Wait()
		if relocErr != nil {
			t.Fatalf("round %d: recordRelocation: %v", round, relocErr)
		}
		h.periodic()
		evs := h.sentEvents(t)
		sort.Slice(evs, func(i, j int) bool { return evs[i].HealthSeq < evs[j].HealthSeq })
		falses, seenTrue := 0, false
		for _, ev := range evs {
			switch relocatedValue(ev) {
			case "false":
				falses++
				if ev.HubID != ev.OutboxPinnedHubID {
					t.Fatalf("round %d: relocated=false with hub %q != pin %q", round, ev.HubID, ev.OutboxPinnedHubID)
				}
				if seenTrue {
					t.Fatalf("round %d: relocated=false after a true (seq %d): the marker is sticky", round, ev.HealthSeq)
				}
			case "true":
				seenTrue = true
			}
		}
		if falses == 0 {
			t.Fatalf("round %d: relocated=false never reported; the verified path must be reachable", round)
		}
		if ev := h.last(t); relocatedValue(ev) != "true" {
			t.Fatalf("round %d: the last event reports relocated %s, want true", round, relocatedValue(ev))
		}
		if h.p.health.rejected != 0 {
			t.Fatalf("round %d: %d events rejected", round, h.p.health.rejected)
		}
	}
}

// Fixture 7 (WP4 H5): mismatch, then a process back on the original hub:
// relocated stays true (sticky), the origin is unchanged, usage stays
// refused (no automatic unblocking), and hub_id == pin with relocated=true
// is valid.
func TestHealthOriginReturnToOrigin(t *testing.T) {
	o := pinnedOutbox(t, "hub-a")
	relocateFromOtherProcess(t, o, "hub-z")
	back := reopen(t, o)
	h := newOriginHarness(t, back, trustedHub("hub-a"))
	h.drainer.RunOnce(context.Background())
	h.periodic()
	ev := h.last(t)
	if ev.HubID != "hub-a" || ev.OutboxPinnedHubID != "hub-a" || relocatedValue(ev) != "true" {
		t.Fatalf("return to origin: hub %q pinned %q relocated %s", ev.HubID, ev.OutboxPinnedHubID, relocatedValue(ev))
	}
	if got, err := back.AppendUsage(draftFor("a", "rid1:back", outboxTestTS)); err != nil || got != AppendRefusedRelocated {
		t.Fatalf("usage after return to origin = %d, %v; want still refused", got, err)
	}
}

// Fixture 8: a relocation between an event's build-time snapshot and its
// freeze: the queued event keeps its build-time observation (relocated=false
// with hub_id = pin, still valid) and the next NEW event reports true.
func TestHealthOriginRelocationBetweenSnapshotAndFreeze(t *testing.T) {
	o := pinnedOutbox(t, "hub-a")
	h := newOriginHarness(t, o, trustedHub("hub-a"))
	h.drainer.RunOnce(context.Background())
	h.p.emitHealth(telemetrycontract.HealthTriggerPeriodic) // built, not yet frozen
	relocateFromOtherProcess(t, o, "hub-z")
	h.p.flushHealth(context.Background())
	first := h.last(t)
	if relocatedValue(first) != "false" || first.HubID != "hub-a" || first.OutboxPinnedHubID != "hub-a" {
		t.Fatalf("queued event must keep its build-time observation: hub %q relocated %s", first.HubID, relocatedValue(first))
	}
	h.periodic()
	if next := h.last(t); relocatedValue(next) != "true" {
		t.Fatalf("the next new event reports relocated %s, want true", relocatedValue(next))
	}
}

// outboxSubject's decision table, including verification that changes
// across the ledger read.
func TestOutboxSubjectRules(t *testing.T) {
	pinned := OutboxLedger{PinnedHubID: "hub-a", HubMismatchTotal: 2, RelocatedRefusedTotal: 3}
	marked := pinned
	marked.RelocatedHubID = "hub-z"
	cases := []struct {
		name                    string
		ledger                  OutboxLedger
		observed, before, after string
		wantNil                 bool
		wantRelocated           string
	}{
		{"unresolved", pinned, "", "", "", true, ""},
		{"no pin", OutboxLedger{}, "hub-a", "hub-a", "hub-a", true, ""},
		{"verified same origin", pinned, "hub-a", "hub-a", "hub-a", false, "false"},
		{"marker wins over verification", marked, "hub-a", "hub-a", "hub-a", false, "true"},
		{"relocated process", marked, "hub-z", "", "", false, "true"},
		{"verification appears during the read", pinned, "hub-a", "", "hub-a", false, "unknown"},
		{"verification lost during the read", pinned, "hub-a", "hub-a", "", false, "unknown"},
		{"observed but unverified", pinned, "hub-z", "", "", false, "unknown"},
	}
	for _, tc := range cases {
		got := outboxSubject(tc.ledger, tc.observed, tc.before, tc.after)
		if (got == nil) != tc.wantNil {
			t.Fatalf("%s: nil=%v, want %v", tc.name, got == nil, tc.wantNil)
		}
		if got == nil {
			continue
		}
		rel := "unknown"
		if got.Relocated != nil {
			rel = fmt.Sprint(*got.Relocated)
		}
		if rel != tc.wantRelocated || got.PinnedHubID != "hub-a" || got.HubMismatchTotal != 2 || got.RelocatedRefusedTotal != 3 {
			t.Fatalf("%s: %+v relocated %s, want %s", tc.name, got, rel, tc.wantRelocated)
		}
	}
}

// Debounce: a hub that differs from the pin is an unconfirmed CANDIDATE.
// Health holds its events (nothing frozen or sent); a later resolution equal
// to the pin verifies the process and the held events freeze with the pin
// and relocated=false.
func TestHealthOriginUnconfirmedCandidateHoldsThenVerifies(t *testing.T) {
	res := &switchableHub{hub: "hub-z"}
	h := newOriginHarness(t, pinnedOutbox(t, "hub-a"), res.resolve)
	clock := time.Date(2026, 10, 23, 5, 0, 0, 0, time.UTC)
	h.drainer.now = func() time.Time { return clock }
	h.drainer.RunOnce(context.Background()) // first sighting: candidate only
	h.periodic()
	if h.drainer.ObservedHubID() != "" || len(h.sentEvents(t)) != 0 {
		t.Fatalf("unconfirmed candidate: observed %q, %d sent; want held", h.drainer.ObservedHubID(), len(h.sentEvents(t)))
	}
	for i, item := range h.p.health.queue {
		if item.line != nil {
			t.Fatalf("queued event %d frozen during an unconfirmed candidate", i)
		}
	}
	res.set("hub-a", nil)
	clock = clock.Add(analyticsRelocationConfirm)
	h.drainer.RunOnce(context.Background()) // equal to the pin: verified
	h.periodic()
	evs := h.sentEvents(t)
	if len(evs) == 0 {
		t.Fatal("held events must be sent once verified")
	}
	for _, ev := range evs {
		if ev.HubID != "hub-a" {
			t.Fatalf("held event frozen with %q, want the verified hub-a", ev.HubID)
		}
	}
	if ev := h.last(t); relocatedValue(ev) != "false" || ev.OutboxPinnedHubID != "hub-a" || *ev.OutboxHubMismatchTotal != 0 {
		t.Fatalf("after verification: relocated %s pinned %q mismatch %d", relocatedValue(ev), ev.OutboxPinnedHubID, *ev.OutboxHubMismatchTotal)
	}
}

// Debounce: a different candidate restarts the confirmation window; health
// stays held until the second candidate is confirmed, then carries it.
func TestHealthOriginDifferentCandidateRestartsWindow(t *testing.T) {
	res := &switchableHub{hub: "hub-z"}
	h := newOriginHarness(t, pinnedOutbox(t, "hub-a"), res.resolve)
	clock := time.Date(2026, 10, 23, 5, 0, 0, 0, time.UTC)
	h.drainer.now = func() time.Time { return clock }
	h.drainer.RunOnce(context.Background()) // candidate hub-z
	res.set("hub-y", nil)
	clock = clock.Add(analyticsRelocationConfirm)
	h.drainer.RunOnce(context.Background()) // a different hub: the window restarts
	h.periodic()
	if h.drainer.ObservedHubID() != "" || len(h.sentEvents(t)) != 0 {
		t.Fatalf("restarted candidate: observed %q, %d sent; want held", h.drainer.ObservedHubID(), len(h.sentEvents(t)))
	}
	clock = clock.Add(analyticsRelocationConfirm)
	h.drainer.RunOnce(context.Background()) // hub-y confirmed
	h.periodic()
	ev := h.last(t)
	if ev.HubID != "hub-y" || ev.OutboxPinnedHubID != "hub-a" || relocatedValue(ev) != "true" || *ev.OutboxHubMismatchTotal != 1 {
		t.Fatalf("after confirmation: hub %q pinned %q relocated %s mismatch %d", ev.HubID, ev.OutboxPinnedHubID, relocatedValue(ev), *ev.OutboxHubMismatchTotal)
	}
}

// An outbox with a NIL drainer falls back to the cached no-outbox mode:
// hub_id comes from the /healthz resolution (first success kept) and the
// outbox-subject group is not reported.
func TestHealthOriginOutboxWithNilDrainerUsesCachedMode(t *testing.T) {
	setAnalyticsEnv(t, "claude")
	clearUsageSourceEnv(t)
	setTestVersion(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"hub_id":"hub-c"}`))
	}))
	defer srv.Close()
	t.Setenv("SCION_HUB_URL", "")
	t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
	o := pinnedOutbox(t, "hub-a")
	var mu sync.Mutex
	var sent [][]byte
	p := NewWithConfig(&Config{Enabled: true})
	p.health.sendOverride = func(_ context.Context, line []byte) error {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, append([]byte(nil), line...))
		return nil
	}
	p.health.periodic, p.health.retry = 1<<62, 1<<62
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { p.stopHealthLoop(); cancel() })
	handles := analyticsHandles{native: NewUsageAnalytics(o, telemetrycontract.UsageSourceNative), send: newCountingSend(nil)}
	if c := handles.counters(time.Now()); c.StatsFailed || c.Outbox != nil {
		t.Fatalf("nil drainer: stats failed %v, outbox subject %+v; want measured, subject nil", c.StatsFailed, c.Outbox)
	}
	p.startHealth(ctx, &GCPExporter{}, handles)
	p.stopHealthLoop()
	p.emitHealth(telemetrycontract.HealthTriggerPeriodic)
	p.flushHealth(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 1 {
		t.Fatalf("sent %d events, want 1", len(sent))
	}
	var ev telemetrycontract.HealthEvent
	if err := telemetrycontract.Unmarshal(sent[0], &ev); err != nil {
		t.Fatal(err)
	}
	if ev.HubID != "hub-c" || ev.OutboxPinnedHubID != "" || ev.OutboxRelocated != nil || ev.OutboxHubMismatchTotal != nil || ev.OutboxRelocatedRefusedTotal != nil {
		t.Fatalf("nil drainer: hub %q, subject pinned %q relocated %v mismatch %v refused %v; want the cached /healthz hub and no subject",
			ev.HubID, ev.OutboxPinnedHubID, ev.OutboxRelocated, ev.OutboxHubMismatchTotal, ev.OutboxRelocatedRefusedTotal)
	}
}
