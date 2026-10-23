/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

var outboxTestTS = time.Date(2026, 10, 22, 0, 5, 0, 0, time.UTC)

func testOutbox(t *testing.T) *AnalyticsOutbox {
	t.Helper()
	o, err := OpenAnalyticsOutbox(filepath.Join(t.TempDir(), "outbox"))
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox: %v", err)
	}
	return o
}

// draftUsage is an event as a producer hands it to AppendUsage: every field
// except the ledger fields the outbox assigns.
func draftUsage(identity, sourceEventID string, input int64) *telemetrycontract.UsageEvent {
	env := telemetrycontract.NewEnvelope(telemetrycontract.EventTypeUsage, "pending", "sciontool/project/p/agent/a", outboxTestTS, outboxTestTS)
	env.AgentID, env.ProjectID = "a", "p"
	ev := &telemetrycontract.UsageEvent{
		Envelope:            env,
		UsageSource:         telemetrycontract.UsageSourceNative,
		SourceKind:          "claude.api_request",
		SourceSchemaVersion: "1",
		SourceIdentity:      identity,
		SourceEventID:       sourceEventID,
		Harness:             "claude",
		Model:               "m",
		ModelSource:         telemetrycontract.ModelSourceEvent,
		Status:              telemetrycontract.StatusSuccess,
		Calls:               1,
		Input:               &input,
		InputDerivation:     telemetrycontract.InputDerivationReported,
		TokenStatus:         telemetrycontract.TokenStatusPartial,
		MalformedFields:     []string{},
	}
	if identity == telemetrycontract.SourceIdentityNone {
		ev.SourceEventID = ""
	}
	return ev
}

func mustAppend(t *testing.T, o *AnalyticsOutbox, ev *telemetrycontract.UsageEvent, want AppendOutcome) *telemetrycontract.UsageEvent {
	t.Helper()
	got, err := o.AppendUsage(ev)
	if err != nil {
		t.Fatalf("AppendUsage: %v", err)
	}
	if got != want {
		t.Fatalf("AppendUsage outcome = %d, want %d", got, want)
	}
	return ev
}

func stagingLines(t *testing.T, o *AnalyticsOutbox) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(o.Dir(), "staging.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return nonEmptyLines(data)
}

func nonEmptyLines(data []byte) []string {
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// capture is a send func recording delivered lines.
type capture struct {
	mu    sync.Mutex
	lines []string
	fail  error
}

func (c *capture) send(_ context.Context, line []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail != nil {
		return c.fail
	}
	c.lines = append(c.lines, string(line))
	return nil
}

func decodeUsage(t *testing.T, line string) telemetrycontract.UsageEvent {
	t.Helper()
	var ev telemetrycontract.UsageEvent
	if err := telemetrycontract.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("decoding %s: %v", line, err)
	}
	return ev
}

func TestOutboxAssignsLedgerFields(t *testing.T) {
	o := testOutbox(t)
	first := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityRequestID, "rid1:x", 1), AppendWritten)
	second := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:y", 2), AppendWritten)

	if first.ProducerSeq != 1 || second.ProducerSeq != 2 {
		t.Errorf("seq = %d, %d; want 1, 2", first.ProducerSeq, second.ProducerSeq)
	}
	if first.ProducerEpoch == "" || first.ProducerEpoch != second.ProducerEpoch {
		t.Errorf("epoch = %q, %q; want one non-empty epoch", first.ProducerEpoch, second.ProducerEpoch)
	}
	if first.SourceOccurrence != 1 || first.IdentityAmbiguous {
		t.Errorf("first sighting = occurrence %d ambiguous %v", first.SourceOccurrence, first.IdentityAmbiguous)
	}
	// Deterministic: recomputed independently from the documented formula.
	if want := usageEventID("sciontool/project/p/agent/a", "claude.api_request", "rid1:x", 1); first.EventID != want || !strings.HasPrefix(want, "u1-") || len(want) != 35 {
		t.Errorf("event_id = %q, want %q", first.EventID, want)
	}
	lines := stagingLines(t, o)
	if len(lines) != 2 || decodeUsage(t, lines[0]).EventID != first.EventID {
		t.Fatalf("staging = %q", lines)
	}

	// Epoch and seq persist across reopen (process restart).
	again, err := OpenAnalyticsOutbox(o.Dir())
	if err != nil {
		t.Fatal(err)
	}
	third := mustAppend(t, again, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:z", 3), AppendWritten)
	if third.ProducerSeq != 3 || third.ProducerEpoch != first.ProducerEpoch {
		t.Errorf("after reopen: seq %d epoch %q", third.ProducerSeq, third.ProducerEpoch)
	}
}

// A verified request ID repeat is a certain duplicate (not written, no seq
// consumed), within a process and after restart.
func TestOutboxRequestIDRepeatIsCertainDuplicate(t *testing.T) {
	o := testOutbox(t)
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityRequestID, "rid1:x", 1), AppendWritten)
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityRequestID, "rid1:x", 1), AppendCertainDuplicate)
	reopened, _ := OpenAnalyticsOutbox(o.Dir())
	mustAppend(t, reopened, draftUsage(telemetrycontract.SourceIdentityRequestID, "rid1:x", 1), AppendCertainDuplicate)
	next := mustAppend(t, reopened, draftUsage(telemetrycontract.SourceIdentityRequestID, "rid1:other", 1), AppendWritten)
	if next.ProducerSeq != 2 {
		t.Errorf("seq after duplicates = %d, want 2", next.ProducerSeq)
	}
	if got := len(stagingLines(t, o)); got != 2 {
		t.Errorf("staging lines = %d, want 2", got)
	}
	if o.Stats().CertainDuplicates != 1 {
		t.Errorf("certain duplicates = %d, want 1", o.Stats().CertainDuplicates)
	}
}

// Composite repeats are never collapsed: each new observation is a further
// occurrence, ambiguous, with its own event ID -- including after restart,
// where the persisted index continues the numbering.
func TestOutboxCompositeRepeatsAreAmbiguousOccurrences(t *testing.T) {
	o := testOutbox(t)
	a := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:same", 5), AppendWritten)
	b := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:same", 5), AppendWritten)
	reopened, _ := OpenAnalyticsOutbox(o.Dir())
	c := mustAppend(t, reopened, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:same", 5), AppendWritten)

	for i, tc := range []struct {
		ev        *telemetrycontract.UsageEvent
		occ       int64
		ambiguous bool
	}{{a, 1, false}, {b, 2, true}, {c, 3, true}} {
		if tc.ev.SourceOccurrence != tc.occ || tc.ev.IdentityAmbiguous != tc.ambiguous {
			t.Errorf("observation %d: occurrence %d ambiguous %v; want %d %v", i+1, tc.ev.SourceOccurrence, tc.ev.IdentityAmbiguous, tc.occ, tc.ambiguous)
		}
	}
	if a.EventID == b.EventID || b.EventID == c.EventID {
		t.Errorf("occurrences must have distinct event IDs: %s %s %s", a.EventID, b.EventID, c.EventID)
	}
}

func TestOutboxIdentityNoneIsAmbiguousWithRandomID(t *testing.T) {
	o := testOutbox(t)
	a := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityNone, "", 1), AppendWritten)
	b := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityNone, "", 1), AppendWritten)
	if !a.IdentityAmbiguous || !b.IdentityAmbiguous || a.SourceOccurrence != 1 || b.SourceOccurrence != 1 {
		t.Errorf("none: %+v / %+v", a, b)
	}
	if a.EventID == b.EventID || strings.HasPrefix(a.EventID, "u1-") {
		t.Errorf("none event IDs must be random UUIDs: %q %q", a.EventID, b.EventID)
	}
}

func TestOutboxFinalizeWaitsForHubAndStampsOnce(t *testing.T) {
	o := testOutbox(t)
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:a", 1), AppendWritten)
	if n, err := o.finalize(context.Background(), o.PinnedHubID()); !errors.Is(err, ErrAnalyticsHubNotVerified) || n != 0 {
		t.Fatalf("Finalize before hub pinned = %d, %v; want 0, ErrAnalyticsHubNotVerified (held)", n, err)
	}
	c := &capture{}
	if n, _ := o.Drain(context.Background(), c.send); n != 0 {
		t.Fatalf("nothing may be sent without hub_id; sent %d", n)
	}

	if pinned, err := o.PinHubID("hub-A"); err != nil || pinned != "hub-A" {
		t.Fatalf("PinHubID = %q, %v", pinned, err)
	}
	// Appended after pinning: still provisional (B1: never stamped at
	// append); only an authorized finalize stamps it.
	late := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:b", 2), AppendWritten)
	if late.HubID != "" {
		t.Errorf("post-pin append hub_id = %q, want provisional (empty)", late.HubID)
	}
	if n, err := o.finalize(context.Background(), "hub-B"); !errors.Is(err, ErrAnalyticsHubNotVerified) || n != 0 {
		t.Fatalf("Finalize authorized by another hub = %d, %v; want refused", n, err)
	}
	if n, err := o.finalize(context.Background(), "hub-A"); err != nil || n != 2 {
		t.Fatalf("Finalize = %d, %v; want 2", n, err)
	}
	if len(stagingLines(t, o)) != 0 {
		t.Error("staging must be empty after Finalize")
	}
	if _, err := o.Drain(context.Background(), c.send); err != nil {
		t.Fatal(err)
	}
	if len(c.lines) != 2 {
		t.Fatalf("sent = %q; want 2", c.lines)
	}
	for i, line := range c.lines {
		if ev := decodeUsage(t, line); ev.HubID != "hub-A" {
			t.Errorf("finalized line %d hub_id = %q", i, ev.HubID)
		}
	}
}

// Drain stops at the first failure and resends the same bytes later; a
// crash after a send but before the cursor commit resends identical bytes.
func TestOutboxDrainRetryAndCrashResendSameBytes(t *testing.T) {
	o := testOutbox(t)
	_, _ = o.PinHubID("hub")
	for i := 0; i < 3; i++ {
		mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, fmt.Sprintf("cmp1:%d", i), int64(i)), AppendWritten)
	}
	if _, err := o.finalize(context.Background(), o.PinnedHubID()); err != nil {
		t.Fatal(err)
	}
	c := &capture{fail: errors.New("unavailable")}
	if n, err := o.Drain(context.Background(), c.send); err == nil || n != 0 {
		t.Fatalf("Drain with failing sender = %d, %v", n, err)
	}
	c.fail = nil

	// Send one line, then "crash" before its cursor commit by restoring the
	// previous cursor.
	sendOne := &capture{}
	stopAfterOne := func(ctx context.Context, line []byte) error {
		if len(sendOne.lines) == 1 {
			return errors.New("crash")
		}
		return sendOne.send(ctx, line)
	}
	cursorBefore, _ := os.ReadFile(filepath.Join(o.Dir(), "cursor"))
	_, _ = o.Drain(context.Background(), stopAfterOne)
	if cursorBefore == nil {
		_ = os.Remove(filepath.Join(o.Dir(), "cursor"))
	} else {
		_ = os.WriteFile(filepath.Join(o.Dir(), "cursor"), cursorBefore, 0o600)
	}

	if _, err := o.Drain(context.Background(), c.send); err != nil {
		t.Fatal(err)
	}
	if len(c.lines) != 3 || c.lines[0] != sendOne.lines[0] {
		t.Fatalf("resend after crash must repeat identical bytes: %q vs %q", c.lines, sendOne.lines)
	}
	if st := o.Stats(); st.PendingBytes != 0 {
		t.Errorf("pending after full drain = %d", st.PendingBytes)
	}
	if files, _ := o.readyFiles(); len(files) != 0 {
		t.Errorf("ready files left after drain: %v", files)
	}
}

// A crash after Finalize wrote the ready file but before staging was
// truncated repeats the copy; the duplicate lines are byte-identical.
func TestOutboxFinalizeCrashRepeatIsByteIdentical(t *testing.T) {
	o := testOutbox(t)
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:a", 1), AppendWritten)
	staged, _ := os.ReadFile(filepath.Join(o.Dir(), "staging.jsonl"))
	_, _ = o.PinHubID("hub")
	_, _ = o.finalize(context.Background(), o.PinnedHubID())
	_ = os.WriteFile(filepath.Join(o.Dir(), "staging.jsonl"), staged, 0o600) // truncate "lost"
	_, _ = o.finalize(context.Background(), o.PinnedHubID())
	c := &capture{}
	_, _ = o.Drain(context.Background(), c.send)
	if len(c.lines) != 2 || c.lines[0] != c.lines[1] {
		t.Fatalf("repeated finalize must give identical lines: %q", c.lines)
	}
}

// A torn line (crashed writer, no trailing newline) is isolated by the next
// append and dropped and counted by Finalize; valid lines survive.
func TestOutboxTornLineIsIsolatedAndCounted(t *testing.T) {
	o := testOutbox(t)
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:a", 1), AppendWritten)
	f, _ := os.OpenFile(filepath.Join(o.Dir(), "staging.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"schema_version":1,"event_ty`)
	_ = f.Close()
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:b", 2), AppendWritten)

	lines := stagingLines(t, o)
	if len(lines) != 3 || !strings.HasPrefix(lines[1], `{"schema_version":1,"event_ty`) || !strings.HasSuffix(lines[1], "event_ty") {
		t.Fatalf("torn fragment must stay its own line: %q", lines)
	}
	_, _ = o.PinHubID("hub")
	if n, _ := o.finalize(context.Background(), o.PinnedHubID()); n != 2 {
		t.Errorf("finalized %d, want 2", n)
	}
	// The next writer terminated the fragment with a newline, so it is a
	// complete (invalid) line now; it is dead-lettered, never dropped.
	if st := o.Stats(); st.InvalidLines != 1 || st.PersistedDeadLetters != 1 {
		t.Errorf("invalid lines = %d, dead letters = %d; want 1 and 1", st.InvalidLines, st.PersistedDeadLetters)
	}
}

// Overflow refuses the event but consumes its seq (a visible gap) and
// persists the count; delivered data no longer counts against the cap.
func TestOutboxOverflowLeavesSeqGapAndDeliveredFreesCap(t *testing.T) {
	o := testOutbox(t)
	_, _ = o.PinHubID("hub")
	first := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:a", 1), AppendWritten)
	line := stagingLines(t, o)[0]
	o.maxPending = int64(len(line)+1) + 10 // room for exactly one line

	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:b", 2), AppendOverflow)
	if st := o.Stats(); st.Overflow != 1 || st.PersistedOverflow != 1 {
		t.Errorf("overflow counters = %+v", st)
	}
	// Refused event wrote nothing and did not touch the index.
	if len(stagingLines(t, o)) != 1 {
		t.Fatal("overflowed event was written")
	}

	_, _ = o.finalize(context.Background(), o.PinnedHubID())
	c := &capture{}
	_, _ = o.Drain(context.Background(), c.send)
	next := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:b", 2), AppendWritten)
	if next.ProducerSeq != first.ProducerSeq+2 {
		t.Errorf("seq after overflow = %d, want %d (gap of one)", next.ProducerSeq, first.ProducerSeq+2)
	}
	if next.SourceOccurrence != 1 {
		t.Errorf("an overflowed observation must not consume an occurrence; got %d", next.SourceOccurrence)
	}
}

// The index horizon is explicit: after 24 h a repeat is a first sighting
// again (WP4 quarantines same-key/different-content); the entry cap evicts
// oldest first.
func TestOutboxIndexExpiryHorizon(t *testing.T) {
	o := testOutbox(t)
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:old", 1), AppendWritten)
	o.now = func() time.Time { return time.Now().Add(analyticsIndexTTL + time.Hour) }
	if removed, err := o.ExpireIndex(); err != nil || removed != 1 {
		t.Fatalf("ExpireIndex = %d, %v; want 1", removed, err)
	}
	again := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:old", 1), AppendWritten)
	if again.SourceOccurrence != 1 {
		t.Errorf("beyond the horizon occurrence = %d, want 1", again.SourceOccurrence)
	}

	o.now = time.Now
	o.indexMax = 2
	for _, id := range []string{"cmp1:a", "cmp1:b", "cmp1:c"} {
		mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, id, 1), AppendWritten)
		time.Sleep(10 * time.Millisecond) // distinct mtimes
	}
	if removed, _ := o.ExpireIndex(); removed != 2 { // "cmp1:old" and "cmp1:a"
		t.Errorf("cap eviction removed %d, want 2", removed)
	}
	if c := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:c", 1), AppendWritten); c.SourceOccurrence != 2 {
		t.Errorf("newest entry must survive cap eviction; occurrence = %d", c.SourceOccurrence)
	}
}

// Documented crash window: ledger state is persisted before the line, so a
// crash in between leaves a seq gap and a consumed occurrence, never a
// reused seq.
func TestOutboxCrashBetweenLedgerAndLineLeavesGap(t *testing.T) {
	o := testOutbox(t)
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:a", 1), AppendWritten)
	// Simulate the crashed append of a second observation of cmp1:a:
	// seq and index updated, line never written.
	_ = o.writeFileAtomic("seq", []byte("2"))
	_ = o.writeIndex(analyticsIndexName("sciontool/project/p/agent/a", "claude.api_request", "cmp1:a"), indexEntry{count: 2})

	next := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:a", 1), AppendWritten)
	if next.ProducerSeq != 3 || next.SourceOccurrence != 3 || !next.IdentityAmbiguous {
		t.Errorf("after crash window: seq %d occurrence %d ambiguous %v; want 3 3 true", next.ProducerSeq, next.SourceOccurrence, next.IdentityAmbiguous)
	}
}

// Concurrent producers (separate handles, as separate hook processes have)
// and a concurrent drainer: every seq is unique and gap-free and every
// event is delivered; duplicates, if any, are byte-identical.
func TestOutboxConcurrentWritersAndDrainer(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "outbox")
	drainer, err := OpenAnalyticsOutbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = drainer.PinHubID("hub")
	const writers, perWriter = 4, 25
	c := &capture{}
	ctx, cancel := context.WithCancel(context.Background())
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for ctx.Err() == nil {
			_, _ = drainer.finalize(context.Background(), drainer.PinnedHubID())
			_, _ = drainer.Drain(context.Background(), c.send)
			time.Sleep(time.Millisecond)
		}
	}()
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			o, err := OpenAnalyticsOutbox(dir)
			if err != nil {
				t.Error(err)
				return
			}
			for i := 0; i < perWriter; i++ {
				if _, err := o.AppendUsage(draftUsage(telemetrycontract.SourceIdentityComposite, fmt.Sprintf("cmp1:%d-%d", w, i), 1)); err != nil {
					t.Error(err)
				}
			}
		}(w)
	}
	wg.Wait()
	cancel()
	<-drained
	_, _ = drainer.finalize(context.Background(), drainer.PinnedHubID())
	_, _ = drainer.Drain(context.Background(), c.send)

	byID := map[string]string{}
	var seqs []int
	for _, line := range c.lines {
		ev := decodeUsage(t, line)
		if prev, ok := byID[ev.EventID]; ok {
			if prev != line {
				t.Fatalf("same event_id with different bytes: %s / %s", prev, line)
			}
			continue
		}
		byID[ev.EventID] = line
		seqs = append(seqs, int(ev.ProducerSeq))
	}
	if len(byID) != writers*perWriter {
		t.Fatalf("delivered %d distinct events, want %d", len(byID), writers*perWriter)
	}
	sort.Ints(seqs)
	for i, s := range seqs {
		if s != i+1 {
			t.Fatalf("seq %d at position %d: seqs must be 1..%d without gaps or reuse", s, i, len(seqs))
		}
	}
}

func TestOutboxStatsPending(t *testing.T) {
	o := testOutbox(t)
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:a", 1), AppendWritten)
	st := o.Stats()
	if st.PendingBytes == 0 || st.Appended != 1 {
		t.Errorf("stats = %+v", st)
	}
	data, _ := os.ReadFile(filepath.Join(o.Dir(), "staging.jsonl"))
	if int64(len(data)) != st.PendingBytes || !bytes.HasSuffix(data, []byte("\n")) {
		t.Errorf("pending %d vs staging %d bytes", st.PendingBytes, len(data))
	}
}

// --- token-analyst-2 r1 findings: crash recovery, conflicts, read faults --

func requestIDDraft(input int64) *telemetrycontract.UsageEvent {
	return draftUsage(telemetrycontract.SourceIdentityRequestID, "rid1:req-A", input)
}

// Finding 1: a crash after the line is durable but before seq/index are
// committed is completed on recovery: the retry is a certain duplicate and
// exactly one line exists, with no seq reuse.
func TestOutboxCrashAfterLineBeforeIndexRecovers(t *testing.T) {
	o := testOutbox(t)
	o.crashAt = "line"
	if _, err := o.AppendUsage(requestIDDraft(5)); !errors.Is(err, errSimulatedCrash) {
		t.Fatalf("simulated crash = %v", err)
	}
	if _, err := os.Stat(filepath.Join(o.Dir(), "intent")); err != nil {
		t.Fatal("intent must survive the crash")
	}
	restarted, err := OpenAnalyticsOutbox(o.Dir())
	if err != nil {
		t.Fatal(err)
	}
	mustAppend(t, restarted, requestIDDraft(5), AppendCertainDuplicate)
	next := mustAppend(t, restarted, draftUsage(telemetrycontract.SourceIdentityRequestID, "rid1:req-B", 1), AppendWritten)
	if next.ProducerSeq != 2 {
		t.Errorf("seq after recovered crash = %d, want 2 (no reuse)", next.ProducerSeq)
	}
	if got := len(stagingLines(t, restarted)); got != 2 {
		t.Errorf("staging lines = %d, want 2", got)
	}
}

// Finding 1: a crash after the intent but before the line is durable must
// NOT suppress the source retry: the event is accepted as a first sighting.
func TestOutboxCrashBeforeLineDoesNotSuppressRetry(t *testing.T) {
	for _, torn := range []bool{false, true} {
		o := testOutbox(t)
		o.crashAt = "intent"
		if _, err := o.AppendUsage(requestIDDraft(5)); !errors.Is(err, errSimulatedCrash) {
			t.Fatalf("simulated crash = %v", err)
		}
		if torn { // the crash also left half a line behind
			f, _ := os.OpenFile(filepath.Join(o.Dir(), "staging.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			_, _ = f.WriteString(`{"schema_version":1,"event_id":"u1-`)
			_ = f.Close()
		}
		restarted, _ := OpenAnalyticsOutbox(o.Dir())
		ev := mustAppend(t, restarted, requestIDDraft(5), AppendWritten)
		// A clean first sighting; the lost attempt's seq 1 is consumed, so
		// the loss is a visible gap.
		if ev.SourceOccurrence != 1 || ev.IdentityAmbiguous || ev.ProducerSeq != 2 {
			t.Errorf("torn=%v: retry = occurrence %d ambiguous %v seq %d; want occurrence 1, certain, seq 2 (gap)", torn, ev.SourceOccurrence, ev.IdentityAmbiguous, ev.ProducerSeq)
		}
	}
}

// Recovery also runs before Finalize, so a crashed writer's durable line is
// indexed before the drainer moves it out of staging.
func TestOutboxFinalizeRecoversIntentFirst(t *testing.T) {
	o := testOutbox(t)
	_, _ = o.PinHubID("hub")
	writer, _ := OpenAnalyticsOutbox(o.Dir())
	writer.crashAt = "line"
	_, _ = writer.AppendUsage(requestIDDraft(5))
	if n, err := o.finalize(context.Background(), o.PinnedHubID()); err != nil || n != 1 {
		t.Fatalf("Finalize = %d, %v", n, err)
	}
	c := &capture{}
	_, _ = o.Drain(context.Background(), c.send)
	mustAppend(t, o, requestIDDraft(5), AppendCertainDuplicate)
}

// Finding 2: the same verified request ID with different content is a
// conflict, written as a further (ambiguous) occurrence for WP4 to
// quarantine -- never silently discarded. Identical content stays a
// certain duplicate; observation-only fields do not matter.
func TestOutboxRequestIDConflictIsSurfaced(t *testing.T) {
	o := testOutbox(t)
	mustAppend(t, o, requestIDDraft(5), AppendWritten)

	retransmit := requestIDDraft(5)
	retransmit.ObservedTS = telemetrycontract.FormatTS(outboxTestTS.Add(time.Hour))
	retransmit.ProducerBuild, retransmit.HarnessVersion = "other-build", "9.9"
	mustAppend(t, o, retransmit, AppendCertainDuplicate)

	for name, mutate := range map[string]func(*telemetrycontract.UsageEvent){
		"tokens": func(e *telemetrycontract.UsageEvent) { n := int64(6); e.Input = &n },
		"model":  func(e *telemetrycontract.UsageEvent) { e.Model = "other" },
		"status": func(e *telemetrycontract.UsageEvent) { e.Status = telemetrycontract.StatusError },
	} {
		ev := requestIDDraft(5)
		mutate(ev)
		mustAppend(t, o, ev, AppendWritten)
		if !ev.IdentityAmbiguous || ev.SourceOccurrence < 2 {
			t.Errorf("%s conflict = occurrence %d ambiguous %v", name, ev.SourceOccurrence, ev.IdentityAmbiguous)
		}
		// The same conflicting content again is now a recorded duplicate.
		again := requestIDDraft(5)
		mutate(again)
		mustAppend(t, o, again, AppendCertainDuplicate)
	}
	if got := o.Stats().Conflicts; got != 3 {
		t.Errorf("conflicts = %d, want 3", got)
	}
	if got := len(stagingLines(t, o)); got != 4 {
		t.Errorf("staging lines = %d, want 4 (original + 3 conflicts)", got)
	}
}

// failingReady fails reads after limit bytes.
type failingReady struct {
	*os.File
	limit, read int64
}

func (f *failingReady) Read(p []byte) (int, error) {
	if f.read >= f.limit {
		return 0, errors.New("injected read failure")
	}
	if rem := f.limit - f.read; int64(len(p)) > rem {
		p = p[:rem]
	}
	n, err := f.File.Read(p)
	f.read += int64(n)
	return n, err
}

func preparedReady(t *testing.T, n int) (*AnalyticsOutbox, []string) {
	t.Helper()
	o := testOutbox(t)
	_, _ = o.PinHubID("hub")
	for i := 0; i < n; i++ {
		mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, fmt.Sprintf("cmp1:%d", i), int64(i)), AppendWritten)
	}
	if _, err := o.finalize(context.Background(), o.PinnedHubID()); err != nil {
		t.Fatal(err)
	}
	files, _ := o.readyFiles()
	data, _ := os.ReadFile(filepath.Join(o.Dir(), readyName(files[0])))
	return o, nonEmptyLines(data)
}

// Finding 3: a mid-file read failure is never completion: the cursor stays
// at the last acknowledged line and the file is kept; the rest is sent
// later.
func TestOutboxDrainReadFailureKeepsUnsentBytes(t *testing.T) {
	o, lines := preparedReady(t, 3)
	firstLen := int64(len(lines[0]) + 1)
	o.openFile = func(name string) (readyFile, error) {
		f, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		return &failingReady{File: f, limit: firstLen + 5}, nil
	}
	c := &capture{}
	n, err := o.Drain(context.Background(), c.send)
	if err == nil || n != 1 {
		t.Fatalf("Drain with read failure = %d, %v; want 1 sent and an error", n, err)
	}
	if files, _ := o.readyFiles(); len(files) != 1 {
		t.Fatal("ready file deleted after a read failure")
	}
	if file, off, _ := o.readCursor(); off != firstLen {
		t.Errorf("cursor = %d/%d, want offset %d (only the acknowledged line)", file, off, firstLen)
	}
	o.openFile = func(name string) (readyFile, error) { return os.Open(name) }
	if _, err := o.Drain(context.Background(), c.send); err != nil {
		t.Fatal(err)
	}
	if len(c.lines) != 3 || c.lines[1] != lines[1] || c.lines[2] != lines[2] {
		t.Errorf("after recovery sent %d lines", len(c.lines))
	}
}

// Finding 3: a truncated ready file (partial final line) is quarantined
// byte-for-byte, never counted as delivered; complete lines before it are
// sent, and later files are not blocked.
func TestOutboxDrainTruncatedReadyIsQuarantined(t *testing.T) {
	o, lines := preparedReady(t, 2)
	files, _ := o.readyFiles()
	path := filepath.Join(o.Dir(), readyName(files[0]))
	data, _ := os.ReadFile(path)
	truncated := data[:len(data)-10]
	_ = os.WriteFile(path, truncated, 0o600)
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:later", 9), AppendWritten)
	_, _ = o.finalize(context.Background(), o.PinnedHubID())

	c := &capture{}
	if _, err := o.Drain(context.Background(), c.send); err != nil {
		t.Fatal(err)
	}
	if len(c.lines) != 2 || c.lines[0] != lines[0] || decodeUsage(t, c.lines[1]).SourceEventID != "cmp1:later" {
		t.Fatalf("sent = %q", c.lines)
	}
	q, err := os.ReadFile(filepath.Join(o.Dir(), fmt.Sprintf("quarantine-%d.jsonl", files[0])))
	if err != nil || string(q) != string(truncated) {
		t.Errorf("quarantine must keep the truncated file byte-for-byte: %v", err)
	}
	if st := o.Stats(); st.Quarantined != 1 || st.Delivered != 2 {
		t.Errorf("stats = %+v", st)
	}
}
