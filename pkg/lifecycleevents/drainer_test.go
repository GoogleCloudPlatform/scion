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

package lifecycleevents

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

// fakeClock is a manually advanced clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type drainHarness struct {
	*harness
	clock    *fakeClock
	sink     *telemetryemit.MemorySink
	dlogs    *bytes.Buffer
	dcounter *telemetryemit.MemoryCounters
}

// newDrainHarness commits a create and a hard delete (two outbox rows)
// and returns a harness ready to drain them.
func newDrainHarness(t *testing.T) *drainHarness {
	h := newHarness(t)
	a := baseAgent()
	h.create(a)
	h.write(context.Background(), store.AgentWrite{Op: store.AgentWriteHardDelete, Before: clone(a)})
	return &drainHarness{harness: h, clock: &fakeClock{now: h.clock.Add(time.Second)}, sink: telemetryemit.NewMemorySink(),
		dlogs: &bytes.Buffer{}, dcounter: telemetryemit.NewMemoryCounters()}
}

func (h *drainHarness) drainer(owner string, mutate ...func(*DrainerConfig)) *Drainer {
	h.t.Helper()
	cfg := DrainerConfig{
		Store: h.db, Sink: h.sink, Counters: h.dcounter, Owner: owner, Now: h.clock.Now,
		Logger: slog.New(slog.NewTextHandler(h.dlogs, nil)),
	}
	for _, m := range mutate {
		m(&cfg)
	}
	d, err := NewDrainer(cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}

func (h *drainHarness) drain(d *Drainer) DrainResult {
	h.t.Helper()
	res, err := d.DrainOnce(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

func payloads(rows []store.LifecycleOutboxRow) [][]byte {
	out := make([][]byte, len(rows))
	for i, r := range rows {
		out[i] = append([]byte(nil), r.Payload...)
	}
	return out
}

func TestDrainerDeliversExactBytesAndDeletes(t *testing.T) {
	h := newDrainHarness(t)
	want := payloads(h.db.rows())
	res := h.drain(h.drainer("r1"))
	if res.Delivered != 2 || res.Remaining != 0 {
		t.Fatalf("result %+v", res)
	}
	got := h.sink.Delivered()
	if len(got) != 2 || !bytes.Equal(got[0], want[0]) || !bytes.Equal(got[1], want[1]) {
		t.Fatalf("delivered bytes differ from the stored payloads")
	}
	if len(h.db.rows()) != 0 {
		t.Fatal("delivered rows were not deleted")
	}
	if h.dcounter.Get(telemetryemit.MetricEventsDelivered, "") != 2 || h.dcounter.Get(telemetryemit.MetricEmitAttempts, "") != 2 {
		t.Errorf("counters: %s", h.dcounter)
	}
}

// TestDrainerRetriesSameBytes: a sink failing N times delivers the row on
// attempt N+1 with the same payload and event_id, after backoff.
func TestDrainerRetriesSameBytes(t *testing.T) {
	h := newDrainHarness(t)
	want := payloads(h.db.rows())
	h.sink.SetFailFunc(func(p []byte, attempt int) error {
		if attempt <= 3 {
			return telemetryemit.Transient("unavailable", errors.New("down"))
		}
		return nil
	})
	d := h.drainer("r1")
	for i := 0; i < 3; i++ {
		res := h.drain(d)
		if res.Delivered != 0 {
			t.Fatalf("pass %d delivered %d", i, res.Delivered)
		}
		// Not yet due again: no claim until the backoff elapses.
		if again := h.drain(d); again.Claimed != 0 {
			t.Fatalf("row claimed before its backoff elapsed")
		}
		h.clock.Advance(backoff(i) + time.Millisecond)
	}
	if res := h.drain(d); res.Delivered != 2 {
		t.Fatalf("final pass delivered %d", res.Delivered)
	}
	for i, p := range h.sink.Delivered() {
		if !bytes.Equal(p, want[i]) {
			t.Fatalf("retried payload %d differs", i)
		}
		if n := h.sink.Attempts(p); n != 4 {
			t.Fatalf("payload %d attempted %d times, want 4", i, n)
		}
	}
	if h.dcounter.Get(telemetryemit.MetricEmitFailures, "unavailable") != 6 {
		t.Errorf("counters: %s", h.dcounter)
	}
}

func TestBackoffSchedule(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	for i, w := range want {
		if got := backoff(i); got != w {
			t.Errorf("backoff(%d) = %s, want %s", i, got, w)
		}
	}
	if got := backoff(30); got != 5*time.Minute {
		t.Errorf("backoff cap = %s", got)
	}
}

// TestDrainerTransientOutageDropsNothingBeforeMaxAge: an outage of more
// than four hours drops nothing; only max_age does.
func TestDrainerTransientOutageDropsNothingBeforeMaxAge(t *testing.T) {
	h := newDrainHarness(t)
	h.sink.SetFailFunc(func([]byte, int) error { return telemetryemit.Transient(telemetryemit.ReasonCircuitOpen, nil) })
	d := h.drainer("r1")
	for elapsed := time.Duration(0); elapsed < 5*time.Hour; elapsed += 5 * time.Minute {
		h.drain(d)
		h.clock.Advance(5 * time.Minute)
	}
	rows := h.db.rows()
	if len(rows) != 2 || rows[0].PermanentFailures != 0 || rows[0].Attempts == 0 {
		t.Fatalf("after a 5 h outage: %+v", rows)
	}
	if h.dcounter.Get(telemetryemit.MetricOutboxDropped, "poison") != 0 {
		t.Fatal("transient failures counted toward poison")
	}
	h.clock.Advance(MaxOutboxAge)
	res := h.drain(d)
	if res.Dropped != 2 || h.dcounter.Get(telemetryemit.MetricOutboxDropped, "age") != 2 {
		t.Fatalf("max_age drop: %+v %s", res, h.dcounter)
	}
	logs := h.dlogs.String()
	for _, r := range rows {
		if !strings.Contains(logs, r.EventID) {
			t.Errorf("dropped event %s not in the drop log", r.EventID)
		}
	}
	if !strings.Contains(logs, "reason=age") {
		t.Errorf("drop log lacks the reason: %s", logs)
	}
}

func TestDrainerPoisonAfterFivePermanentFailures(t *testing.T) {
	h := newDrainHarness(t)
	h.sink.SetFailFunc(func([]byte, int) error {
		return telemetryemit.Permanent(telemetryemit.ReasonInvalidArgument, errors.New("rejected"))
	})
	d := h.drainer("r1")
	for i := 0; i < MaxPermanentFailures; i++ {
		h.drain(d)
		h.clock.Advance(backoffMax)
	}
	res := h.drain(d)
	if res.Dropped != 2 || h.dcounter.Get(telemetryemit.MetricOutboxDropped, "poison") != 2 {
		t.Fatalf("poison drop: %+v %s", res, h.dcounter)
	}
}

func TestDrainerRevalidationFailureIsPermanent(t *testing.T) {
	h := newDrainHarness(t)
	h.db.stateMu.Lock()
	for s, r := range h.db.outbox {
		r.Payload = []byte(`{"not":"an event"}`)
		h.db.outbox[s] = r
	}
	h.db.stateMu.Unlock()
	h.drain(h.drainer("r1"))
	for _, r := range h.db.rows() {
		if r.PermanentFailures != 1 {
			t.Fatalf("re-validation failure not permanent: %+v", r)
		}
	}
	if len(h.sink.Delivered()) != 0 || h.sink.Attempts([]byte(`{"not":"an event"}`)) != 0 {
		t.Fatal("an invalid stored payload reached the sink")
	}
}

// TestDrainerBlockedSinkKeepsRows: PermissionDenied/Unauthenticated block
// the sink without poisoning: claiming stops, rows keep their attempt
// counts, probes back off, and a success unblocks.
func TestDrainerBlockedSinkKeepsRows(t *testing.T) {
	h := newDrainHarness(t)
	var blocked = true
	var mu sync.Mutex
	h.sink.SetFailFunc(func([]byte, int) error {
		mu.Lock()
		defer mu.Unlock()
		if blocked {
			return telemetryemit.Blocked(telemetryemit.ReasonPermission, errors.New("denied"))
		}
		return nil
	})
	d := h.drainer("r1")
	res := h.drain(d)
	if res.Claimed != 2 || res.Failed != 1 {
		t.Fatalf("first pass %+v: one probe fails, the rest is released", res)
	}
	if b, reason := d.Blocked(); !b || reason != telemetryemit.ReasonPermission {
		t.Fatalf("not blocked: %v %q", b, reason)
	}
	if h.dcounter.Get(telemetryemit.MetricSinkBlocked, telemetryemit.ReasonPermission) != 1 {
		t.Errorf("blocked gauge: %s", h.dcounter)
	}
	for _, r := range h.db.rows() {
		if r.Attempts != 0 || r.PermanentFailures != 0 || r.LeaseOwner != "" {
			t.Fatalf("blocked sink changed a row's accounting or kept its lease: %+v", r)
		}
	}
	if again := h.drain(d); again.Claimed != 0 {
		t.Fatal("claimed while blocked before the probe was due")
	}
	h.clock.Advance(backoffBase)
	if probe := h.drain(d); probe.Claimed != 1 { // probe fails again: backoff doubles
		t.Fatalf("a blocked-sink probe claims one row, claimed %d", probe.Claimed)
	}
	h.clock.Advance(backoffBase)
	if again := h.drain(d); again.Claimed != 0 {
		t.Fatal("probe backoff did not grow")
	}
	mu.Lock()
	blocked = false
	mu.Unlock()
	h.clock.Advance(2 * backoffBase)
	if res := h.drain(d); res.Delivered != 1 {
		t.Fatalf("the successful probe delivers its one row, delivered %d", res.Delivered)
	}
	if res := h.drain(d); res.Delivered != 1 {
		t.Fatalf("after unblock the next pass delivers the rest, delivered %d", res.Delivered)
	}
	if b, _ := d.Blocked(); b || h.dcounter.Get(telemetryemit.MetricSinkBlocked, telemetryemit.ReasonPermission) != 0 {
		t.Fatal("still blocked after a successful delivery")
	}
	if n := strings.Count(h.dlogs.String(), "sink is blocked"); n != 1 {
		t.Errorf("blocked ERROR logged %d times within a minute, want 1", n)
	}
}

func TestDrainerOverflowDropsOldest(t *testing.T) {
	h := newDrainHarness(t)
	rows := h.db.rows()
	h.sink.SetFailFunc(func([]byte, int) error { return telemetryemit.Transient("x", nil) })
	d := h.drainer("r1", func(c *DrainerConfig) { c.MaxRows = 1 })
	res := h.drain(d)
	if res.Dropped != 1 || h.dcounter.Get(telemetryemit.MetricOutboxDropped, "overflow") != 1 {
		t.Fatalf("overflow: %+v %s", res, h.dcounter)
	}
	left := h.db.rows()
	if len(left) != 1 || left[0].Seq != rows[1].Seq {
		t.Fatalf("overflow must drop the oldest row: %+v", left)
	}
	if !strings.Contains(h.dlogs.String(), rows[0].EventID) {
		t.Error("overflow drop not logged with its event_id")
	}
}

func TestTrimSkipsLeasedRows(t *testing.T) {
	h := newDrainHarness(t)
	// Another replica holds a live lease on every row.
	other := h.drainer("r2")
	if _, err := h.db.ClaimLifecycleOutbox(context.Background(), "r2", h.clock.Now(), h.clock.Now().Add(time.Minute), 10); err != nil {
		t.Fatal(err)
	}
	_ = other
	h.clock.Advance(MaxOutboxAge + time.Second)
	// The lease expired with the clock jump; renew it to keep it live.
	for _, r := range h.db.rows() {
		if _, err := h.db.RenewLifecycleOutboxLease(context.Background(), r.Seq, "r2", h.clock.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	res := h.drain(h.drainer("r1"))
	if res.Dropped != 0 || len(h.db.rows()) != 2 {
		t.Fatalf("trim touched leased rows: %+v", res)
	}
}

// TestDrainerLeaseExpiryDuplicatesSameKey: a drainer that loses its lease
// mid-flight can only cause a duplicate with identical bytes.
func TestDrainerLeaseExpiryDuplicatesSameKey(t *testing.T) {
	h := newDrainHarness(t)
	want := payloads(h.db.rows())
	// r1 claims and then stalls (never completes); its lease expires.
	if _, err := h.db.ClaimLifecycleOutbox(context.Background(), "r1", h.clock.Now(), h.clock.Now().Add(DefaultLeaseDuration), 10); err != nil {
		t.Fatal(err)
	}
	// r1 "emitted" both before stalling.
	for _, p := range want {
		_ = h.sink.Emit(context.Background(), p)
	}
	h.clock.Advance(DefaultLeaseDuration + time.Second)
	if res := h.drain(h.drainer("r2")); res.Delivered != 2 {
		t.Fatalf("r2 delivered %d", res.Delivered)
	}
	got := h.sink.Delivered()
	if len(got) != 4 {
		t.Fatalf("deliveries = %d", len(got))
	}
	for i := 0; i < 2; i++ {
		if !bytes.Equal(got[i], got[i+2]) {
			t.Fatal("duplicate delivery differs from the original")
		}
	}
	// r1 cannot complete rows it no longer owns.
	for _, r := range h.db.rows() {
		t.Fatalf("row left behind: %+v", r)
	}
	if ok, _ := h.db.CompleteLifecycleOutboxRow(context.Background(), 1, "r1"); ok {
		t.Fatal("a stale owner completed a row")
	}
}

// Crash injection with a fake store and sink (design section 8):
// (b) commit then "die" before drain: a fresh drainer delivers;
// (c) sink succeeds then die before delete: redelivery, same bytes;
// (d) sink fails: backoff, then delivery (TestDrainerRetriesSameBytes).
func TestDrainerCrashInjection(t *testing.T) {
	t.Run("b: committed, never drained", func(t *testing.T) {
		h := newDrainHarness(t)
		want := payloads(h.db.rows())
		fresh := h.drainer("after-restart")
		if res := h.drain(fresh); res.Delivered != 2 {
			t.Fatalf("delivered %d", res.Delivered)
		}
		for i, p := range h.sink.Delivered() {
			if !bytes.Equal(p, want[i]) {
				t.Fatal("bytes changed across the restart")
			}
		}
	})
	t.Run("c: delivered, delete lost", func(t *testing.T) {
		h := newDrainHarness(t)
		want := payloads(h.db.rows())
		h.db.failComplete = errInjected
		h.drain(h.drainer("before-crash"))
		if len(h.db.rows()) != 2 {
			t.Fatal("rows must survive a lost delete")
		}
		h.db.failComplete = nil
		h.clock.Advance(DefaultLeaseDuration + time.Second)
		h.drain(h.drainer("after-restart"))
		got := h.sink.Delivered()
		if len(got) != 4 || !bytes.Equal(got[0], got[2]) || !bytes.Equal(got[1], got[3]) || !bytes.Equal(got[0], want[0]) {
			t.Fatal("redelivery after a lost delete is not byte-identical")
		}
		if len(h.db.rows()) != 0 {
			t.Fatal("rows not deleted after redelivery")
		}
	})
}

func TestDrainerExitWhenEmptyAndShutdown(t *testing.T) {
	h := newDrainHarness(t)
	d := h.drainer("r1", func(c *DrainerConfig) { c.ExitWhenEmpty = true })
	done := make(chan struct{})
	go func() { d.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("backlog-only drainer did not stop once the outbox was empty")
	}
	if len(h.sink.Delivered()) != 2 {
		t.Fatalf("delivered %d", len(h.sink.Delivered()))
	}

	// A normal drainer runs until cancelled, then makes a final pass.
	h2 := newDrainHarness(t)
	h2.sink.SetFailFunc(func([]byte, int) error { return telemetryemit.Transient("x", nil) })
	d2 := h2.drainer("r1", func(c *DrainerConfig) { c.PollInterval = time.Hour })
	ctx, cancel := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { d2.Run(ctx); close(done2) }()
	cancel()
	select {
	case <-done2:
	case <-time.After(10 * time.Second):
		t.Fatal("drainer did not stop on cancel")
	}
	if len(h2.db.rows()) != 2 {
		t.Fatal("undelivered rows must stay for the next start")
	}
}

func TestDrainerWakeIsNonBlocking(t *testing.T) {
	h := newDrainHarness(t)
	d := h.drainer("r1")
	for i := 0; i < 5; i++ {
		d.Wake()
	}
}

func TestDrainerSweepsOrphanCursors(t *testing.T) {
	h := newHarness(t)
	h.db.cursors["orphan-1"] = store.LifecycleCursor{AgentID: "orphan-1"}
	h.db.cursors["live-1"] = store.LifecycleCursor{AgentID: "live-1"}
	h.db.agents["live-1"] = true
	d, err := NewDrainer(DrainerConfig{Store: h.db, Sink: telemetryemit.NewMemorySink(), SweepBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	d.sweep(context.Background())
	if _, ok := h.db.cursor("orphan-1"); ok {
		t.Error("orphan cursor kept")
	}
	if _, ok := h.db.cursor("live-1"); !ok {
		t.Error("live cursor removed")
	}
}

func TestDrainerConfigValidation(t *testing.T) {
	if _, err := NewDrainer(DrainerConfig{}); err == nil {
		t.Error("drainer without store and sink accepted")
	}
	h := newHarness(t)
	if _, err := NewDrainer(DrainerConfig{Store: h.db, Sink: telemetryemit.NewMemorySink(), EmitTimeout: time.Minute, LeaseDuration: time.Second}); err == nil {
		t.Error("emit timeout longer than the lease accepted")
	}
}

func TestSanitizeError(t *testing.T) {
	long := errors.New(strings.Repeat("é", 300) + "\nsecond line")
	s := sanitizeError(long)
	if len(s) > maxLastErrorBytes || strings.ContainsAny(s, "\n") || strings.ToValidUTF8(s, "") != s {
		t.Fatalf("sanitized error %q", s)
	}
}

func TestInstallPolicy(t *testing.T) {
	cases := []struct {
		pinned              bool
		kService, persisted string
		resolved            string
		want                string
	}{
		{true, "", "", "h", HubIDSourcePinned},
		{true, "svc", "h", "h", HubIDSourcePinned},
		{false, "svc", "", "x", HubIDSourceKService},
		{false, "", "h\n", "h", HubIDSourcePersistedFile},
		{false, "", "other", "h", HubIDSourceHostname},
		{false, "", "", "h", HubIDSourceHostname},
	}
	for _, c := range cases {
		if got := ClassifyHubIDSource(c.pinned, c.kService, c.persisted, c.resolved); got != c.want {
			t.Errorf("ClassifyHubIDSource(%+v) = %s, want %s", c, got, c.want)
		}
	}
	states := []struct {
		enabled bool
		source  string
		sqlite  bool
		want    string
	}{
		{false, HubIDSourcePinned, false, telemetryemit.ObserverStateDisabledExperimentOff},
		{true, HubIDSourcePinned, false, telemetryemit.ObserverStateEnabled},
		{true, HubIDSourcePersistedFile, true, telemetryemit.ObserverStateEnabled},
		{true, HubIDSourcePersistedFile, false, telemetryemit.ObserverStateDisabledUnpinnedHubID},
		{true, HubIDSourceHostname, true, telemetryemit.ObserverStateDisabledUnpinnedHubID},
		{true, HubIDSourceKService, false, telemetryemit.ObserverStateDisabledUnpinnedHubID},
	}
	for _, s := range states {
		if got := ObserverState(s.enabled, s.source, s.sqlite); got != s.want {
			t.Errorf("ObserverState(%+v) = %s, want %s", s, got, s.want)
		}
	}
}

// countingStore counts the maintenance statements of a drainer.
type countingStore struct {
	*memDB
	mu           sync.Mutex
	trims, stats int
}

func (c *countingStore) TrimLifecycleOutbox(ctx context.Context, now time.Time, maxAge time.Duration, maxRows, maxPermanent int) (store.LifecycleOutboxTrim, error) {
	c.mu.Lock()
	c.trims++
	c.mu.Unlock()
	return c.memDB.TrimLifecycleOutbox(ctx, now, maxAge, maxRows, maxPermanent)
}

func (c *countingStore) LifecycleOutboxStats(ctx context.Context) (store.LifecycleOutboxStats, error) {
	c.mu.Lock()
	c.stats++
	c.mu.Unlock()
	return c.memDB.LifecycleOutboxStats(ctx)
}

// TestDrainerMaintenanceCadence is section 22 item 12 N5: Run claims every
// poll but trims and samples the gauges only every MaintenanceInterval (the
// first pass included), so an idle drainer runs one claim per tick.
func TestDrainerMaintenanceCadence(t *testing.T) {
	h := newDrainHarness(t)
	cs := &countingStore{memDB: h.db}
	d := h.drainer("me", func(c *DrainerConfig) {
		c.Store = cs
		// The drainer keeps the harness's fake clock (Now), the same clock
		// that stamped the rows, so the outbox age trim measures their age
		// on that clock and never drops them, whatever the real date. Only
		// the poll ticker runs in real time.
		c.PollInterval = time.Millisecond
		c.MaintenanceInterval = time.Hour
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(h.sink.Delivered()) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond) // many idle ticks
	cancel()
	<-done
	if got := len(h.sink.Delivered()); got != 2 {
		t.Fatalf("delivered %d rows, want 2", got)
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	// The first pass maintains, and the shutdown pass is a full DrainOnce.
	if cs.trims != 2 || cs.stats != 2 {
		t.Errorf("trims=%d stats=%d, want 2 each (first pass + final pass)", cs.trims, cs.stats)
	}
}
