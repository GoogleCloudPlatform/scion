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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// Producer identity guard (design wp2-producer-identity-v1.3): staged
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
	if _, err := o.Finalize("hub-A"); err != nil {
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
	if _, err := o.Finalize("hub-A"); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, o, draftFor("a", "rid1:staged", outboxTestTS), AppendWritten)
	cp := copyOutbox(t, o)
	readyBefore := drainAll(t, o)

	c := &capture{}
	d := NewAnalyticsDrainer(cp, c.send, trustedHub("hub-B"))
	d.RunOnce(context.Background())
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
	if n, err := r.Finalize("hub-A"); !errors.Is(err, ErrAnalyticsOutboxRelocated) || n != 0 {
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
			if _, err := o.Finalize("hub-A"); err != nil {
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
	dB.RunOnce(context.Background())
	wg.Wait()
	close(outcomes)
	for got := range outcomes {
		if got != AppendWritten && got != AppendRefusedRelocated {
			t.Errorf("racing append outcome = %d", got)
		}
	}
	for _, line := range stagingLines(t, o) {
		if decodeUsage(t, line).HubID != "" {
			t.Errorf("staged line stamped: %s", line)
		}
	}
	if n, err := o.Finalize(dA.VerifiedHubID()); !errors.Is(err, ErrAnalyticsOutboxRelocated) || n != 0 {
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
	if _, err := reopen(t, o).PinHubID("hub-B"); err != nil {
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
		NewAnalyticsDrainer(o, c.send, trustedHub(hub)).RunOnce(context.Background())
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
