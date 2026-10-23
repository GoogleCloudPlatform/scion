/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// Producer identity guard (design wp2-producer-identity-v1.5 and later): staged
// lines are provisional, finalized only under a hub this process verified
// against the pin; a mismatch records a relocation, refuses new appends and
// holds provisional lines, while already-final lines replay unchanged.

// trustedHub is the explicit test-only resolver stub: it always reports id.
func trustedHub(id string) HubIDResolver {
	return func(context.Context) (string, error) { return id, nil }
}

// copyOutbox copies an outbox directory (a copied agent home).
func copyOutbox(t *testing.T, src *AnalyticsOutbox) *AnalyticsOutbox {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "outbox")
	err := filepath.Walk(src.Dir(), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src.Dir(), path)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	})
	if err != nil {
		t.Fatalf("copying outbox: %v", err)
	}
	o, err := OpenAnalyticsOutbox(dst)
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(copy): %v", err)
	}
	return o
}

// runConfirmed runs the drainer twice, the second run
// analyticsRelocationConfirm later, so a differing hub is confirmed.
func runConfirmed(d *AnalyticsDrainer) {
	clock := time.Date(2026, 10, 22, 1, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return clock }
	d.RunOnce(context.Background())
	clock = clock.Add(analyticsRelocationConfirm)
	d.RunOnce(context.Background())
}

func reopen(t *testing.T, o *AnalyticsOutbox) *AnalyticsOutbox {
	t.Helper()
	r, err := OpenAnalyticsOutbox(o.Dir())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	return r
}

func draftFor(agentID, sourceEventID string, observed time.Time) *telemetrycontract.UsageEvent {
	ev := draftUsage(telemetrycontract.SourceIdentityRequestID, sourceEventID, 1)
	ev.ProducerNamespace = usageProducerNamespace("p", agentID)
	ev.AgentID = agentID
	ev.ObservedTS = telemetrycontract.FormatTS(observed)
	return ev
}

func drainAll(t *testing.T, o *AnalyticsOutbox) []string {
	t.Helper()
	c := &capture{}
	if _, err := o.Drain(context.Background(), c.send); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	return c.lines
}

func ledger(t *testing.T, o *AnalyticsOutbox) OutboxLedger {
	t.Helper()
	l, err := o.LedgerSnapshot()
	if err != nil {
		t.Fatalf("LedgerSnapshot: %v", err)
	}
	return l
}

// T1: independent hubs mint distinct agent UUIDs, so byte-identical
// deterministic source IDs still give distinct primary keys.
func TestGuardT1IndependentHubsDistinctKeys(t *testing.T) {
	a, b := testOutbox(t), testOutbox(t)
	evA := mustAppend(t, a, draftFor("agent-uuid-1", "rid1:same", outboxTestTS), AppendWritten)
	evB := mustAppend(t, b, draftFor("agent-uuid-2", "rid1:same", outboxTestTS), AppendWritten)
	if evA.SourceEventID != evB.SourceEventID {
		t.Fatal("fixture: source IDs must be identical")
	}
	if evA.ProducerNamespace == evB.ProducerNamespace || evA.EventID == evB.EventID {
		t.Errorf("keys collapsed: (%s,%s) vs (%s,%s)", evA.ProducerNamespace, evA.EventID, evB.ProducerNamespace, evB.EventID)
	}
}

// T2 (unsupported copy): both copies see the same source record at
// different instants: same key, different bytes (a WP4 quarantine). With
// identical bytes the rows are indistinguishable from a replay; that
// residual is unsupported and not asserted as detected.
func TestGuardT2CopiedHomeSameSourceSameKeyDifferentBytes(t *testing.T) {
	orig := testOutbox(t)
	cp := copyOutbox(t, orig)
	e1 := mustAppend(t, orig, draftFor("a", "rid1:x", outboxTestTS), AppendWritten)
	e2 := mustAppend(t, cp, draftFor("a", "rid1:x", outboxTestTS.Add(time.Millisecond)), AppendWritten)
	if e1.ProducerNamespace != e2.ProducerNamespace || e1.EventID != e2.EventID {
		t.Fatalf("copied home must derive the same key: %s vs %s", e1.EventID, e2.EventID)
	}
	if l1, l2 := stagingLines(t, orig)[0], stagingLines(t, cp)[0]; l1 == l2 {
		t.Error("bytes must differ (observed_ts) so WP4 quarantines instead of merging")
	}
}

// T3: final lines replay byte-identically after a move or copy; a later
// PinHubID with another hub never restamps them.
func TestGuardT3FinalLinesReplayUnchanged(t *testing.T) {
	o := testOutbox(t)
	_, _ = o.PinHubID("hub-A")
	mustAppend(t, o, draftFor("a", "rid1:r", outboxTestTS), AppendWritten)
	if _, err := o.finalize(context.Background(), "hub-A"); err != nil {
		t.Fatal(err)
	}
	cp := copyOutbox(t, o)
	want := drainAll(t, o)
	if _, err := cp.PinHubID("hub-B"); err != nil {
		t.Fatal(err)
	}
	got := drainAll(t, cp)
	if len(want) != 1 || len(got) != 1 || want[0] != got[0] {
		t.Fatalf("replay changed: %q vs %q", want, got)
	}
	if ev := decodeUsage(t, got[0]); ev.HubID != "hub-A" {
		t.Errorf("replayed hub_id = %q, want origin hub-A", ev.HubID)
	}
}

// T4: a copied home under another hub: the relocation persists across
// reopen, new appends are refused (seq gap, counted), provisional lines are
// held, and already-final lines drain unchanged with the origin hub.
func TestGuardT4CrossHubCopyRefusesAndHolds(t *testing.T) {
	o := testOutbox(t)
	_, _ = o.PinHubID("hub-A")
	mustAppend(t, o, draftFor("a", "rid1:final", outboxTestTS), AppendWritten)
	if _, err := o.finalize(context.Background(), "hub-A"); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, o, draftFor("a", "rid1:staged", outboxTestTS), AppendWritten)
	cp := copyOutbox(t, o)
	readyBefore := drainAll(t, o)

	c := &capture{}
	d := NewAnalyticsDrainer(cp, c.send, trustedHub("hub-B"))
	runConfirmed(d)
	if got := d.VerifiedHubID(); got != "" {
		t.Errorf("VerifiedHubID after mismatch = %q, want empty", got)
	}
	if got := d.ObservedHubID(); got != "hub-B" {
		t.Errorf("ObservedHubID = %q, want hub-B", got)
	}
	if len(c.lines) != 1 || c.lines[0] != readyBefore[0] {
		t.Fatalf("final line must drain unchanged: %q vs %q", c.lines, readyBefore)
	}

	r := reopen(t, cp)
	l := ledger(t, r)
	if l.RelocatedHubID != "hub-B" || l.HubMismatchTotal != 1 || l.PinnedHubID != "hub-A" {
		t.Fatalf("ledger after reopen = %+v", l)
	}
	seqBefore := l.LastSeq
	ev := draftFor("a", "rid1:new", outboxTestTS)
	if got, err := r.AppendUsage(ev); err != nil || got != AppendRefusedRelocated {
		t.Fatalf("append on relocated outbox = %d, %v", got, err)
	}
	l = ledger(t, r)
	if l.LastSeq != seqBefore+1 || l.RelocatedRefusedTotal != 1 {
		t.Errorf("refusal must consume a seq and count: seq %d->%d, refused %d", seqBefore, l.LastSeq, l.RelocatedRefusedTotal)
	}
	if s := r.Stats(); !s.Relocated || s.PersistedRelocatedRefused != 1 || s.PersistedHubMismatch != 1 || s.RelocatedRefused != 1 {
		t.Errorf("stats = %+v", s)
	}
	if n, err := r.finalize(context.Background(), "hub-A"); !errors.Is(err, ErrAnalyticsOutboxRelocated) || n != 0 {
		t.Errorf("Finalize on relocated outbox = %d, %v", n, err)
	}
	if len(stagingLines(t, r)) != 1 || l.PendingBytes == 0 {
		t.Error("provisional line must stay held (pending), not finalized or dropped")
	}
}

// T5 (suspected fork): copies appending different sources continue one
// (epoch, seq) space: the same (epoch, seq) with distinct event IDs.
func TestGuardT5CopiedHomeSeqCollision(t *testing.T) {
	orig := testOutbox(t)
	cp := copyOutbox(t, orig)
	e1 := mustAppend(t, orig, draftFor("a", "rid1:one", outboxTestTS), AppendWritten)
	e2 := mustAppend(t, cp, draftFor("a", "rid1:two", outboxTestTS), AppendWritten)
	if e1.ProducerEpoch != e2.ProducerEpoch || e1.ProducerSeq != e2.ProducerSeq || e1.EventID == e2.EventID {
		t.Errorf("want same (epoch, seq), distinct event_id: %s/%d/%s vs %s/%d/%s",
			e1.ProducerEpoch, e1.ProducerSeq, e1.EventID, e2.ProducerEpoch, e2.ProducerSeq, e2.EventID)
	}
}

// T6/T7: lines appended before this process verified (pre-detection, or a
// lazy hook with no drainer) are provisional; a verifying drainer on the
// pinned hub finalizes them, one on another hub holds them.
func TestGuardT6T7ProvisionalUntilVerified(t *testing.T) {
	for _, tc := range []struct {
		name, resolved string
		wantSent       bool
	}{{"same hub", "hub-A", true}, {"other hub", "hub-B", false}} {
		t.Run(tc.name, func(t *testing.T) {
			o := testOutbox(t)
			_, _ = o.PinHubID("hub-A")
			// A hook process: its own handle, no drainer.
			hook := reopen(t, o)
			ev := mustAppend(t, hook, draftFor("a", "rid1:lazy", outboxTestTS), AppendWritten)
			if ev.HubID != "" || strings.Contains(stagingLines(t, o)[0], `"hub_id":"hub-A"`) {
				t.Fatal("staged line must be provisional (no hub_id)")
			}
			c := &capture{}
			d := NewAnalyticsDrainer(o, c.send, trustedHub(tc.resolved))
			d.RunOnce(context.Background())
			if tc.wantSent {
				if len(c.lines) != 1 || decodeUsage(t, c.lines[0]).HubID != "hub-A" || decodeUsage(t, c.lines[0]).EventID != ev.EventID {
					t.Fatalf("sent = %q", c.lines)
				}
				return
			}
			if len(c.lines) != 0 || len(stagingLines(t, o)) != 1 {
				t.Fatalf("other hub: sent %q, staged %d; want held", c.lines, len(stagingLines(t, o)))
			}
		})
	}
}

// T8 and production nil resolver: without a verification nothing is
// finalized (provisional lines held); already-final lines still drain.
func TestGuardT8UnverifiedHoldsButFinalLinesDrain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		resolve func(*error) HubIDResolver
	}{
		{"nil resolver", func(*error) HubIDResolver { return nil }},
		{"failing resolver", func(e *error) HubIDResolver {
			return func(context.Context) (string, error) {
				if *e != nil {
					return "", *e
				}
				return "hub-A", nil
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := testOutbox(t)
			_, _ = o.PinHubID("hub-A")
			mustAppend(t, o, draftFor("a", "rid1:final", outboxTestTS), AppendWritten)
			if _, err := o.finalize(context.Background(), "hub-A"); err != nil {
				t.Fatal(err)
			}
			mustAppend(t, o, draftFor("a", "rid1:held", outboxTestTS), AppendWritten)
			resolveErr := errors.New("hub unreachable")
			c := &capture{}
			d := NewAnalyticsDrainer(o, c.send, tc.resolve(&resolveErr))
			clock := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
			d.now = func() time.Time { return clock }
			d.RunOnce(context.Background())
			if len(c.lines) != 1 || len(stagingLines(t, o)) != 1 {
				t.Fatalf("sent %d (want the 1 final line), staged %d (want 1 held)", len(c.lines), len(stagingLines(t, o)))
			}
			if d.ObservedHubID() != "" || d.VerifiedHubID() != "" {
				t.Error("nothing may be published before a successful resolution")
			}
			if tc.name == "nil resolver" {
				return
			}
			resolveErr = nil
			clock = clock.Add(time.Hour)
			d.RunOnce(context.Background())
			if len(c.lines) != 2 || d.VerifiedHubID() != "hub-A" {
				t.Fatalf("after an equal resolve: sent %d, verified %q", len(c.lines), d.VerifiedHubID())
			}
		})
	}
}

// T9: a relocation recorded by another process after this one verified is
// never bypassed: two drainers on one outbox; A verifies, B records the
// relocation, A's finalize is refused. Appends racing the relocation are
// written provisional or refused, never stamped.
func TestGuardT9TwoDrainersAndConcurrentAppends(t *testing.T) {
	o := testOutbox(t)
	_, _ = o.PinHubID("hub-A")
	ca, cb := &capture{}, &capture{}
	dA := NewAnalyticsDrainer(o, ca.send, trustedHub("hub-A"))
	dA.RunOnce(context.Background())
	if dA.VerifiedHubID() != "hub-A" {
		t.Fatal("drainer A must verify hub-A")
	}

	var wg sync.WaitGroup
	outcomes := make(chan AppendOutcome, 40)
	for i := 0; i < 4; i++ {
		h := reopen(t, o)
		wg.Add(1)
		go func(i int, h *AnalyticsOutbox) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				got, err := h.AppendUsage(draftFor("a", "rid1:c"+string(rune('a'+i))+string(rune('a'+j)), outboxTestTS))
				if err != nil {
					t.Errorf("append: %v", err)
					return
				}
				outcomes <- got
			}
		}(i, h)
	}
	dB := NewAnalyticsDrainer(reopen(t, o), cb.send, trustedHub("hub-B"))
	runConfirmed(dB)
	wg.Wait()
	close(outcomes)
	written, refused := 0, 0
	for got := range outcomes {
		switch got {
		case AppendWritten:
			written++
		case AppendRefusedRelocated:
			refused++
		default:
			t.Errorf("racing append outcome = %d", got)
		}
	}
	l := ledger(t, o)
	if int64(refused) != l.RelocatedRefusedTotal || l.LastSeq != 40 || written != len(stagingLines(t, o)) {
		t.Errorf("accounting: written %d (staged %d), refused %d (ledger %d), last seq %d (want 40)",
			written, len(stagingLines(t, o)), refused, l.RelocatedRefusedTotal, l.LastSeq)
	}
	for _, line := range stagingLines(t, o) {
		if decodeUsage(t, line).HubID != "" {
			t.Errorf("staged line stamped: %s", line)
		}
	}
	if n, err := o.finalize(context.Background(), dA.VerifiedHubID()); !errors.Is(err, ErrAnalyticsOutboxRelocated) || n != 0 {
		t.Fatalf("A's stale verification bypassed the relocation: %d, %v", n, err)
	}
	dA.RunOnce(context.Background())
	if len(ca.lines)+len(cb.lines) != 0 {
		t.Errorf("held lines were sent: %d", len(ca.lines)+len(cb.lines))
	}
}

// The per-file commit re-checks the relocation under the lock: a
// relocation recorded after rotation refuses the ready file.
func TestGuardCommitRechecksRelocation(t *testing.T) {
	o := testOutbox(t)
	_, _ = o.PinHubID("hub-A")
	mustAppend(t, o, draftFor("a", "rid1:x", outboxTestTS), AppendWritten)
	if _, err := o.rotateStaging("hub-A"); err != nil {
		t.Fatal(err)
	}
	files, err := o.finalizingFiles()
	if err != nil || len(files) != 1 {
		t.Fatalf("finalizing files = %v, %v", files, err)
	}
	if err := reopen(t, o).recordRelocation("hub-B"); err != nil {
		t.Fatal(err)
	}
	if _, err := o.finalizeFile(files[0], "hub-A"); !errors.Is(err, ErrAnalyticsOutboxRelocated) {
		t.Fatalf("commit after relocation = %v, want ErrAnalyticsOutboxRelocated", err)
	}
	if ready, _ := o.readyFiles(); len(ready) != 0 {
		t.Errorf("ready files = %v, want none", ready)
	}
	if _, err := os.Stat(filepath.Join(o.Dir(), files[0])); err != nil {
		t.Errorf("finalizing file must stay held: %v", err)
	}
}

// T10: a staged line from an older build that already carries hub_id is
// copied byte-identically when verified, and held when relocated.
func TestGuardT10LegacyStampedLine(t *testing.T) {
	for _, relocated := range []bool{false, true} {
		o := testOutbox(t)
		_, _ = o.PinHubID("hub-A")
		mustAppend(t, o, draftFor("a", "rid1:legacy", outboxTestTS), AppendWritten)
		var ev telemetrycontract.UsageEvent
		if err := telemetrycontract.Unmarshal([]byte(stagingLines(t, o)[0]), &ev); err != nil {
			t.Fatal(err)
		}
		ev.HubID = "hub-A"
		legacy, err := telemetrycontract.Marshal(&ev)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(o.Dir(), "staging.jsonl"), append(legacy, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		c := &capture{}
		hub := "hub-A"
		if relocated {
			hub = "hub-B"
		}
		runConfirmed(NewAnalyticsDrainer(o, c.send, trustedHub(hub)))
		if relocated {
			if len(c.lines) != 0 || len(stagingLines(t, o)) != 1 {
				t.Errorf("relocated: legacy line sent %d, staged %d; want held", len(c.lines), len(stagingLines(t, o)))
			}
			continue
		}
		if len(c.lines) != 1 || c.lines[0] != string(legacy) {
			t.Errorf("legacy line not byte-identical: %q", c.lines)
		}
	}
}

// No-pin copy (UNSUPPORTED, provenance UNKNOWN): provisional lines made on
// hub A before any pin, copied, then first resolved on hub B are pinned and
// finalized under B. The guard cannot prove their origin; this documents
// the outcome rather than claiming it is closed.
func TestGuardNoPinCopyIsUnknownProvenance(t *testing.T) {
	o := testOutbox(t)
	mustAppend(t, o, draftFor("a", "rid1:nopin", outboxTestTS), AppendWritten)
	cp := copyOutbox(t, o)
	c := &capture{}
	NewAnalyticsDrainer(cp, c.send, trustedHub("hub-B")).RunOnce(context.Background())
	if len(c.lines) != 1 || decodeUsage(t, c.lines[0]).HubID != "hub-B" {
		t.Fatalf("documented outcome: finalized under the first verified hub; got %q", c.lines)
	}
	if l := ledger(t, cp); l.RelocatedHubID != "" || l.HubMismatchTotal != 0 {
		t.Errorf("no pin means no detectable mismatch; ledger %+v", l)
	}
}

// A nil drainer (analytics disabled) publishes nothing.
func TestGuardNilDrainerAccessors(t *testing.T) {
	var d *AnalyticsDrainer
	if d.ObservedHubID() != "" || d.VerifiedHubID() != "" {
		t.Error("nil drainer accessors must return empty")
	}
}

// Supported Scion path, unsupported for analytics continuity (temporary
// limitation): unlink, then `scion hub link` to another hub re-registers
// the same home as a NEW agent (new agent ID, so a new namespace). Old
// final bytes drain unchanged; new observations are refused (fail-closed,
// no pin or epoch reset); the sticky marker survives restarts, including a
// later process that resolves the original hub again.
func TestGuardRelinkToAnotherHubAsNewAgent(t *testing.T) {
	o := testOutbox(t)
	_, _ = o.PinHubID("hub-A")
	mustAppend(t, o, draftFor("agent-old", "rid1:final", outboxTestTS), AppendWritten)
	if _, err := o.finalize(context.Background(), "hub-A"); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, o, draftFor("agent-old", "rid1:staged", outboxTestTS), AppendWritten)
	readyBytes, err := os.ReadFile(filepath.Join(o.Dir(), readyName(1)))
	if err != nil {
		t.Fatal(err)
	}
	epoch := ledger(t, o).Epoch

	// Process 1 after re-link: hub B, new agent ID.
	c := &capture{}
	d := NewAnalyticsDrainer(o, c.send, trustedHub("hub-B"))
	runConfirmed(d)
	if len(c.lines) != 1 || c.lines[0]+"\n" != string(readyBytes) {
		t.Fatalf("old final bytes must drain unchanged: %q", c.lines)
	}
	if got, err := o.AppendUsage(draftFor("agent-new", "rid1:new", outboxTestTS)); err != nil || got != AppendRefusedRelocated {
		t.Fatalf("new agent's observation = %d, %v; want refused", got, err)
	}

	// Restarts: the marker is sticky; no pin or epoch reset, even when a
	// later process resolves hub A again.
	for _, hub := range []string{"hub-B", "hub-A"} {
		r := reopen(t, o)
		c := &capture{}
		dr := NewAnalyticsDrainer(r, c.send, trustedHub(hub))
		dr.RunOnce(context.Background())
		l := ledger(t, r)
		if l.RelocatedHubID != "hub-B" || l.PinnedHubID != "hub-A" || l.Epoch != epoch {
			t.Fatalf("after restart (resolving %s): ledger %+v", hub, l)
		}
		if got, err := r.AppendUsage(draftFor("agent-new", "rid1:again-"+hub, outboxTestTS)); err != nil || got != AppendRefusedRelocated {
			t.Fatalf("after restart (resolving %s): append = %d, %v", hub, got, err)
		}
		if len(c.lines) != 0 || len(stagingLines(t, r)) != 1 {
			t.Fatalf("after restart (resolving %s): sent %d, staged %d; want held", hub, len(c.lines), len(stagingLines(t, r)))
		}
	}
	if l := ledger(t, o); l.RelocatedRefusedTotal != 3 {
		t.Errorf("relocated_refused = %d, want 3", l.RelocatedRefusedTotal)
	}
}

// Same-hub project re-link (supported, analytics continuous): the home is
// moved to a new project ID, so new observations carry a new namespace,
// while the outbox's epoch, seq and counters continue. Old final lines and
// their keys are untouched (no reattribution); the guard stays neutral
// (the resolve equals the pin). WP4 keys the counter stream by epoch, with
// the namespace as a label (design v1.4 section B).
func TestGuardSameHubProjectRelinkContinuity(t *testing.T) {
	o := testOutbox(t)
	_, _ = o.PinHubID("hub-A")
	nsOld, nsNew := usageProducerNamespace("p-old", "a"), usageProducerNamespace("p-new", "a")
	old := draftFor("a", "rid1:before", outboxTestTS)
	old.ProducerNamespace, old.ProjectID = nsOld, "p-old"
	mustAppend(t, o, old, AppendWritten)
	o.maxPending = 1 // force one counted overflow before the move
	over := draftFor("a", "rid1:overflow", outboxTestTS)
	over.ProducerNamespace, over.ProjectID = nsOld, "p-old"
	mustAppend(t, o, over, AppendOverflow)
	o.maxPending = analyticsOutboxMaxPending
	c := &capture{}
	NewAnalyticsDrainer(o, c.send, trustedHub("hub-A")).RunOnce(context.Background())
	if len(c.lines) != 1 {
		t.Fatalf("before the move: sent %d", len(c.lines))
	}
	oldLine := c.lines[0]
	before := ledger(t, o)

	// The re-link moves the project config dir (home included).
	movedDir := filepath.Join(t.TempDir(), "moved", "outbox")
	if err := os.MkdirAll(filepath.Dir(movedDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(o.Dir(), movedDir); err != nil {
		t.Fatal(err)
	}
	m, err := OpenAnalyticsOutbox(movedDir)
	if err != nil {
		t.Fatal(err)
	}
	after := ledger(t, m)
	if after.Epoch != before.Epoch || after.LastSeq != before.LastSeq || after.OverflowTotal != 1 || after.PinnedHubID != "hub-A" {
		t.Fatalf("epoch/seq/counters must survive the move: before %+v after %+v", before, after)
	}

	nw := draftFor("a", "rid1:after", outboxTestTS)
	nw.ProducerNamespace, nw.ProjectID = nsNew, "p-new"
	ev := mustAppend(t, m, nw, AppendWritten)
	if ev.ProducerEpoch != before.Epoch || ev.ProducerSeq != before.LastSeq+1 {
		t.Errorf("seq must continue in the same epoch: %s/%d", ev.ProducerEpoch, ev.ProducerSeq)
	}
	c2 := &capture{}
	d := NewAnalyticsDrainer(m, c2.send, trustedHub("hub-A"))
	d.RunOnce(context.Background())
	if d.VerifiedHubID() != "hub-A" || len(c2.lines) != 1 {
		t.Fatalf("same hub must verify and finalize: verified %q, sent %d", d.VerifiedHubID(), len(c2.lines))
	}
	got := decodeUsage(t, c2.lines[0])
	if got.ProducerNamespace != nsNew || got.ProjectID != "p-new" || got.HubID != "hub-A" {
		t.Errorf("new observation = ns %q project %q hub %q", got.ProducerNamespace, got.ProjectID, got.HubID)
	}
	prev := decodeUsage(t, oldLine)
	if prev.ProducerNamespace != nsOld || prev.EventID == got.EventID {
		t.Errorf("old key must stay distinct and unchanged: %s/%s vs %s/%s", prev.ProducerNamespace, prev.EventID, got.ProducerNamespace, got.EventID)
	}
	if l := ledger(t, m); l.RelocatedHubID != "" || l.HubMismatchTotal != 0 || l.OverflowTotal != 1 {
		t.Errorf("same-hub move must not look like a relocation: %+v", l)
	}
}

// Cross-namespace duplicate seq (WP4 continuity defect, epoch-wide scope):
// a copied home where one copy's observations carry another namespace
// still allocates from the shared epoch, so distinct keys
// (namespace, event_id) claim the same (epoch, seq). This is evidence of a
// continuity conflict, not proof of its cause.
func TestGuardCrossNamespaceDuplicateSeq(t *testing.T) {
	orig := testOutbox(t)
	cp := copyOutbox(t, orig)
	e1 := mustAppend(t, orig, draftFor("a", "rid1:one", outboxTestTS), AppendWritten)
	other := draftFor("a", "rid1:two", outboxTestTS)
	other.ProducerNamespace, other.ProjectID = usageProducerNamespace("p-other", "a"), "p-other"
	e2 := mustAppend(t, cp, other, AppendWritten)
	if e1.ProducerEpoch != e2.ProducerEpoch || e1.ProducerSeq != e2.ProducerSeq {
		t.Fatalf("want the same (epoch, seq): %s/%d vs %s/%d", e1.ProducerEpoch, e1.ProducerSeq, e2.ProducerEpoch, e2.ProducerSeq)
	}
	if e1.ProducerNamespace == e2.ProducerNamespace || e1.EventID == e2.EventID {
		t.Errorf("want distinct keys: (%s,%s) vs (%s,%s)", e1.ProducerNamespace, e1.EventID, e2.ProducerNamespace, e2.EventID)
	}
}

// scriptedHub returns the next ID from ids on each resolution (the last
// one repeats).
func scriptedHub(ids ...string) HubIDResolver {
	var mu sync.Mutex
	i := 0
	return func(context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		id := ids[i]
		if i < len(ids)-1 {
			i++
		}
		return id, nil
	}
}

// Debounce (false-positive mitigation, not identity proof): one differing
// resolution records nothing and publishes nothing; a later agreeing one
// at least analyticsRelocationConfirm after the first confirms; an equal
// one in between clears the candidate; a different one restarts it. While
// unconfirmed, finalize is held and final lines drain unchanged.
func TestGuardRelocationDebounce(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ids           []string
		steps         []time.Duration // clock offsets of successive runs
		wantRelocated string
		wantVerified  string
	}{
		{"single differing read", []string{"hub-B"}, []time.Duration{0}, "", ""},
		{"agreeing too early", []string{"hub-B", "hub-B"}, []time.Duration{0, analyticsRelocationConfirm - time.Second}, "", ""},
		{"agreeing after the delay", []string{"hub-B", "hub-B"}, []time.Duration{0, analyticsRelocationConfirm}, "hub-B", ""},
		{"misroute clears", []string{"hub-B", "hub-A"}, []time.Duration{0, analyticsRelocationConfirm}, "", "hub-A"},
		{"different hub restarts", []string{"hub-B", "hub-C", "hub-C"}, []time.Duration{0, analyticsRelocationConfirm, analyticsRelocationConfirm}, "", ""},
		{"restarted then confirmed", []string{"hub-B", "hub-C", "hub-C"}, []time.Duration{0, analyticsRelocationConfirm, 2 * analyticsRelocationConfirm}, "hub-C", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := testOutbox(t)
			_, _ = o.PinHubID("hub-A")
			mustAppend(t, o, draftFor("a", "rid1:final", outboxTestTS), AppendWritten)
			if _, err := o.finalize(context.Background(), "hub-A"); err != nil {
				t.Fatal(err)
			}
			mustAppend(t, o, draftFor("a", "rid1:staged", outboxTestTS), AppendWritten)
			c := &capture{}
			d := NewAnalyticsDrainer(o, c.send, scriptedHub(tc.ids...))
			start := time.Date(2026, 10, 22, 1, 0, 0, 0, time.UTC)
			for _, off := range tc.steps {
				clock := start.Add(off)
				d.now = func() time.Time { return clock }
				d.RunOnce(context.Background())
			}
			l := ledger(t, o)
			if l.RelocatedHubID != tc.wantRelocated || d.VerifiedHubID() != tc.wantVerified {
				t.Fatalf("relocated %q (want %q), verified %q (want %q)", l.RelocatedHubID, tc.wantRelocated, d.VerifiedHubID(), tc.wantVerified)
			}
			if tc.wantRelocated == "" && tc.wantVerified == "" && d.ObservedHubID() != "" {
				t.Errorf("unconfirmed: ObservedHubID = %q, want empty", d.ObservedHubID())
			}
			wantSent := 1
			if tc.wantVerified != "" {
				wantSent = 2
			}
			if len(c.lines) != wantSent {
				t.Errorf("sent %d, want %d (final line always; provisional only once verified)", len(c.lines), wantSent)
			}
		})
	}
}

// R2 publication order: ObservedHubID and VerifiedHubID are still empty
// while PinHubID (and, on a confirmed mismatch, recordRelocation) holds
// the outbox lock; they are published only afterwards.
func TestGuardPublicationOrder(t *testing.T) {
	type seen struct{ observed, verified string }
	watch := func(o *AnalyticsOutbox, d **AnalyticsDrainer, first int32) (*[]seen, func()) {
		var mu sync.Mutex
		var got []seen
		var n atomic.Int32
		o.lockAcquiredHook = func() {
			if n.Add(1) > first {
				return
			}
			mu.Lock()
			got = append(got, seen{(*d).ObservedHubID(), (*d).VerifiedHubID()})
			mu.Unlock()
		}
		return &got, func() { o.lockAcquiredHook = nil }
	}

	t.Run("equal hub", func(t *testing.T) {
		o := testOutbox(t)
		_, _ = o.PinHubID("hub-A")
		d := NewAnalyticsDrainer(o, (&capture{}).send, trustedHub("hub-A"))
		got, stop := watch(o, &d, 1) // the first acquisition is PinHubID's
		d.RunOnce(context.Background())
		stop()
		if len(*got) != 1 || (*got)[0] != (seen{}) {
			t.Fatalf("accessors during PinHubID = %+v, want empty", *got)
		}
		if d.ObservedHubID() != "hub-A" || d.VerifiedHubID() != "hub-A" {
			t.Errorf("after: observed %q verified %q", d.ObservedHubID(), d.VerifiedHubID())
		}
	})

	t.Run("confirmed mismatch", func(t *testing.T) {
		o := testOutbox(t)
		_, _ = o.PinHubID("hub-A")
		d := NewAnalyticsDrainer(o, (&capture{}).send, trustedHub("hub-B"))
		clock := time.Date(2026, 10, 22, 1, 0, 0, 0, time.UTC)
		d.now = func() time.Time { return clock }
		d.RunOnce(context.Background()) // candidate
		clock = clock.Add(analyticsRelocationConfirm)
		got, stop := watch(o, &d, 2) // PinHubID, then recordRelocation
		d.RunOnce(context.Background())
		stop()
		if len(*got) != 2 || (*got)[0] != (seen{}) || (*got)[1] != (seen{}) {
			t.Fatalf("accessors during PinHubID/recordRelocation = %+v, want empty", *got)
		}
		if d.ObservedHubID() != "hub-B" || d.VerifiedHubID() != "" || ledger(t, o).RelocatedHubID != "hub-B" {
			t.Errorf("after: observed %q verified %q", d.ObservedHubID(), d.VerifiedHubID())
		}
	})
}

// O2: dead lines are persisted with a successful commit only, so a refused
// commit (retried later) never counts them twice.
func TestGuardDeadLettersPersistOnlyWithCommit(t *testing.T) {
	writeBad := func(o *AnalyticsOutbox) {
		f, err := os.OpenFile(filepath.Join(o.Dir(), "staging.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString("{\"not\":\"an event\"}\n")
		_ = f.Close()
	}
	// Refused commits: counted zero times, however often retried.
	o := testOutbox(t)
	_, _ = o.PinHubID("hub-A")
	mustAppend(t, o, draftFor("a", "rid1:x", outboxTestTS), AppendWritten)
	writeBad(o)
	if _, err := o.rotateStaging("hub-A"); err != nil {
		t.Fatal(err)
	}
	files, _ := o.finalizingFiles()
	if err := o.recordRelocation("hub-B"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := o.finalizeFile(files[0], "hub-A"); !errors.Is(err, ErrAnalyticsOutboxRelocated) {
			t.Fatalf("finalizeFile = %v", err)
		}
	}
	if l := ledger(t, o); l.DeadLettersTotal != 0 {
		t.Errorf("refused commits dead-lettered %d lines, want 0", l.DeadLettersTotal)
	}
	// A successful commit: counted once.
	o2 := testOutbox(t)
	_, _ = o2.PinHubID("hub-A")
	mustAppend(t, o2, draftFor("a", "rid1:y", outboxTestTS), AppendWritten)
	writeBad(o2)
	if n, err := o2.finalize(context.Background(), "hub-A"); err != nil || n != 1 {
		t.Fatalf("finalize = %d, %v", n, err)
	}
	if l := ledger(t, o2); l.DeadLettersTotal != 1 {
		t.Errorf("dead letters = %d, want 1", l.DeadLettersTotal)
	}
}

// F2: the commit never replaces an existing file (the ready number is
// chosen under the lock, and the link fails with EEXIST).
func TestGuardCommitNeverReplaces(t *testing.T) {
	o := testOutbox(t)
	dst := filepath.Join(o.Dir(), readyName(7))
	if err := os.WriteFile(dst, []byte("other drainer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := o.writeFileFromCommit(o.Dir(), "ready", func(w io.Writer) error {
		_, err := w.Write([]byte("mine\n"))
		return err
	}, func(link func(string) error) error { return link(dst) })
	if err == nil {
		t.Fatal("commit replaced an existing ready file")
	}
	if data, _ := os.ReadFile(dst); string(data) != "other drainer\n" {
		t.Errorf("existing file changed: %q", data)
	}
	if tmps, _ := filepath.Glob(filepath.Join(o.Dir(), ".ready.tmp-*")); len(tmps) != 0 {
		t.Errorf("temp files left: %v", tmps)
	}
}

// Debounce: a process that exits with only an unconfirmed candidate
// leaves no marker and nothing persisted; the next process starts over.
func TestGuardCandidateDoesNotSurviveExit(t *testing.T) {
	o := testOutbox(t)
	_, _ = o.PinHubID("hub-A")
	mustAppend(t, o, draftFor("a", "rid1:staged", outboxTestTS), AppendWritten)
	d1 := NewAnalyticsDrainer(o, (&capture{}).send, trustedHub("hub-B"))
	clock := time.Date(2026, 10, 22, 1, 0, 0, 0, time.UTC)
	d1.now = func() time.Time { return clock }
	d1.RunOnce(context.Background()) // candidate only; then the process exits
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	d1.Stop(ctx)
	cancel()

	r := reopen(t, o)
	if l := ledger(t, r); l.RelocatedHubID != "" || l.HubMismatchTotal != 0 {
		t.Fatalf("candidate persisted: %+v", l)
	}
	// The next process: a read past the old candidate's window is a new
	// first sighting, not a confirmation.
	d2 := NewAnalyticsDrainer(r, (&capture{}).send, trustedHub("hub-B"))
	later := clock.Add(analyticsRelocationConfirm)
	d2.now = func() time.Time { return later }
	d2.RunOnce(context.Background())
	if l := ledger(t, r); l.RelocatedHubID != "" || d2.ObservedHubID() != "" {
		t.Errorf("new process confirmed on its first read: relocated %q observed %q", l.RelocatedHubID, d2.ObservedHubID())
	}
	if len(stagingLines(t, r)) != 1 {
		t.Error("provisional line must stay held while unconfirmed")
	}
}

// r2 O1: the inner "too early" check refuses an early agreeing reading
// even when the outer resolveAfter gate is bypassed.
func TestGuardInnerTooEarlyGuard(t *testing.T) {
	o := testOutbox(t)
	_, _ = o.PinHubID("hub-A")
	d := NewAnalyticsDrainer(o, (&capture{}).send, trustedHub("hub-B"))
	clock := time.Date(2026, 10, 22, 1, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return clock }
	d.RunOnce(context.Background()) // candidate
	clock = clock.Add(analyticsRelocationConfirm - time.Second)
	d.resolveAfter = time.Time{} // bypass the outer gate
	d.RunOnce(context.Background())
	if l := ledger(t, o); l.RelocatedHubID != "" || d.ObservedHubID() != "" || d.VerifiedHubID() != "" {
		t.Fatalf("early agreement recorded or published: relocated %q observed %q", l.RelocatedHubID, d.ObservedHubID())
	}
	if want := clock.Add(time.Second); !d.resolveAfter.Equal(want) {
		t.Errorf("resolveAfter = %v, want the candidate's window end %v", d.resolveAfter, want)
	}
}

// r2 O2: the in-memory HubIDMismatch counts confirmed relocations only,
// like the persisted hub_mismatch.
func TestGuardInMemoryMismatchCountsConfirmedOnly(t *testing.T) {
	o := testOutbox(t)
	_, _ = o.PinHubID("hub-A")
	d := NewAnalyticsDrainer(o, (&capture{}).send, trustedHub("hub-B"))
	clock := time.Date(2026, 10, 22, 1, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return clock }
	d.RunOnce(context.Background())
	if got := o.MemoryStats().HubIDMismatch; got != 0 {
		t.Fatalf("candidate counted: HubIDMismatch = %d", got)
	}
	clock = clock.Add(analyticsRelocationConfirm)
	d.RunOnce(context.Background())
	if got, l := o.MemoryStats().HubIDMismatch, ledger(t, o); got != 1 || l.HubMismatchTotal != 1 {
		t.Errorf("after confirmation: memory %d, persisted %d; want 1, 1", got, l.HubMismatchTotal)
	}
}

// r2 O3: without hard links the commit falls back to a rename (no-replace
// under the lock, counted); if the destination already exists the
// fallback refuses, finalize fails, and the drainer counts it.
func TestGuardCommitLinkFallback(t *testing.T) {
	noLink := func(string, string) error { return &os.LinkError{Op: "link", Err: syscall.EPERM} }

	o := testOutbox(t)
	o.link = noLink
	_, _ = o.PinHubID("hub-A")
	mustAppend(t, o, draftFor("a", "rid1:x", outboxTestTS), AppendWritten)
	c := &capture{}
	NewAnalyticsDrainer(o, c.send, trustedHub("hub-A")).RunOnce(context.Background())
	if len(c.lines) != 1 || o.MemoryStats().CommitLinkFallbacks != 1 {
		t.Fatalf("fallback: sent %d, fallbacks %d", len(c.lines), o.MemoryStats().CommitLinkFallbacks)
	}

	o2 := testOutbox(t)
	o2.link = func(_, dst string) error {
		_ = os.WriteFile(dst, []byte("someone else\n"), 0o644) // dst appears, then link fails
		return &os.LinkError{Op: "link", Err: syscall.EPERM}
	}
	_, _ = o2.PinHubID("hub-A")
	mustAppend(t, o2, draftFor("a", "rid1:y", outboxTestTS), AppendWritten)
	c2 := &capture{}
	d2 := NewAnalyticsDrainer(o2, c2.send, trustedHub("hub-A"))
	d2.RunOnce(context.Background())
	if o2.MemoryStats().FinalizeFailures != 1 || o2.MemoryStats().CommitLinkFallbacks != 0 {
		t.Errorf("refused fallback: failures %d, fallbacks %d", o2.MemoryStats().FinalizeFailures, o2.MemoryStats().CommitLinkFallbacks)
	}
	files, _ := o2.finalizingFiles()
	if len(files) != 1 {
		t.Errorf("finalizing file must stay held; got %v", files)
	}
}

// r2 O4: leftover .ready.tmp-* files older than the sweep age are removed
// at finalize; younger ones (a commit in progress elsewhere) are kept.
func TestGuardSweepsOldTempFiles(t *testing.T) {
	o := testOutbox(t)
	_, _ = o.PinHubID("hub-A")
	old := filepath.Join(o.Dir(), ".ready.tmp-old")
	young := filepath.Join(o.Dir(), ".ready.tmp-young")
	for _, p := range []string{old, young} {
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * analyticsTmpSweepAge)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if _, err := o.finalize(context.Background(), "hub-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old temp file not swept: %v", err)
	}
	if _, err := os.Stat(young); err != nil {
		t.Errorf("young temp file removed: %v", err)
	}
}
