/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"golang.org/x/sys/unix"
)

var errInjectedSync = errors.New("injected sync failure")

// failNextFileSync makes the next file fsync fail once, after the write
// has completed (the data is in the file, its durability unknown).
func failNextFileSync(o *AnalyticsOutbox) {
	failed := false
	o.fsyncFile = func(f *os.File) error {
		if !failed {
			failed = true
			return errInjectedSync
		}
		return f.Sync()
	}
}

func eventIDsInStaging(t *testing.T, o *AnalyticsOutbox) map[string]int {
	t.Helper()
	ids := map[string]int{}
	for _, l := range stagingLines(t, o) {
		ids[decodeUsage(t, l).EventID]++
	}
	return ids
}

// WP2 r2 R1 (a): a file-sync fault after a complete write, verified
// request ID. The write is taken back (truncated), the event is reported
// lost, and the retry is accepted once. Never two lines with one event_id.
func TestOutboxFileSyncFaultRequestID(t *testing.T) {
	o := testOutbox(t)
	failNextFileSync(o)
	if _, err := o.AppendUsage(requestIDDraft(5)); err == nil {
		t.Fatal("append with a failed fsync reported success though the write was taken back")
	}
	if _, err := os.Stat(filepath.Join(o.Dir(), "intent")); !os.IsNotExist(err) {
		t.Error("intent must be resolved after the failed append")
	}
	retry := mustAppend(t, o, requestIDDraft(5), AppendWritten)
	mustAppend(t, o, requestIDDraft(5), AppendCertainDuplicate)
	ids := eventIDsInStaging(t, o)
	if len(ids) != 1 || ids[retry.EventID] != 1 {
		t.Errorf("staging event IDs = %v; want exactly one line for the event", ids)
	}
	if retry.ProducerSeq != 2 {
		t.Errorf("retry seq = %d, want 2 (the failed append consumed seq 1)", retry.ProducerSeq)
	}
	if st := o.Stats(); st.PersistedAppendFailed != 1 {
		t.Errorf("append_failed = %d, want 1 (the event really was lost)", st.PersistedAppendFailed)
	}
}

// WP2 r2 R1 (b): when the write cannot be taken back, recovery finds the
// line and commits it; the append is reported written (sync uncertain),
// and a composite retry is occurrence 2, ambiguous -- never a second
// occurrence-1 line.
func TestOutboxFileSyncFaultUntruncatableComposite(t *testing.T) {
	o := testOutbox(t)
	calls := 0
	o.fsyncFile = func(f *os.File) error {
		calls++
		// Fail the append's sync; the truncate is stubbed to fail too, so
		// the line stays.
		if calls == 1 {
			return errInjectedSync
		}
		return f.Sync()
	}
	o.truncateHook = func() error { return errInjectedSync }
	first, err := o.AppendUsage(draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:x", 1))
	if err != nil || first != AppendWritten {
		t.Fatalf("append = %v, %v; want written (line present, committed by recovery)", first, err)
	}
	retry := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:x", 1), AppendWritten)
	if retry.SourceOccurrence != 2 || !retry.IdentityAmbiguous {
		t.Errorf("retry = occurrence %d ambiguous %v; want 2, true", retry.SourceOccurrence, retry.IdentityAmbiguous)
	}
	ids := eventIDsInStaging(t, o)
	for id, n := range ids {
		if n != 1 {
			t.Errorf("event_id %s appears %d times", id, n)
		}
	}
	if st := o.Stats(); st.SyncUncertain != 1 || st.PersistedAppendFailed != 0 {
		t.Errorf("stats = %+v; want sync_uncertain 1, append_failed 0", st)
	}
}

// WP2 r2 R1 (c): a directory-sync fault when staging.jsonl is created.
func TestOutboxDirSyncFaultOnStagingCreate(t *testing.T) {
	o := testOutbox(t)
	failed := false
	o.fsyncDir = func(dir string) error {
		if !failed && dir == o.Dir() {
			if _, err := os.Stat(filepath.Join(dir, "staging.jsonl")); err == nil {
				failed = true
				return errInjectedSync
			}
		}
		return syncDir(dir)
	}
	if _, err := o.AppendUsage(requestIDDraft(5)); err == nil {
		t.Fatal("append with a failed directory sync on create must fail (the write is taken back)")
	}
	if !failed {
		t.Fatal("dir sync fault was not injected")
	}
	if got := o.Stats().PersistedAppendFailed; got != 1 {
		t.Errorf("append_failed = %d, want 1", got)
	}
	// Whatever the first outcome, the retry must not produce a second
	// line with the same event_id.
	_, _ = o.AppendUsage(requestIDDraft(5))
	for id, n := range eventIDsInStaging(t, o) {
		if n != 1 {
			t.Errorf("event_id %s appears %d times", id, n)
		}
	}
	if got := len(stagingLines(t, o)); got != 1 {
		t.Errorf("staging lines = %d, want 1", got)
	}
}

// WP2 r2 R1 (d): a fault inside commitIntent after the line is durable is
// not a loss: the append is reported written, append_failed is not bumped,
// and the next operation completes the commit (the retry is a certain
// duplicate).
func TestOutboxCommitFaultIsCompletedByRecovery(t *testing.T) {
	o := testOutbox(t)
	o.crashAt = "commit"
	got, err := o.AppendUsage(requestIDDraft(5))
	if err != nil || got != AppendWritten {
		t.Fatalf("append = %v, %v; want written", got, err)
	}
	o.crashAt = ""
	mustAppend(t, o, requestIDDraft(5), AppendCertainDuplicate)
	if st := o.Stats(); st.PersistedAppendFailed != 0 || st.CommitDeferred != 1 || st.PersistedCommitDeferred != 1 {
		t.Errorf("stats = %+v", st)
	}
	if n := len(stagingLines(t, o)); n != 1 {
		t.Errorf("staging lines = %d, want 1", n)
	}
}

// token-analyst-2: a malformed intent is quarantined (kept, counted), not
// silently discarded.
func TestOutboxMalformedIntentQuarantined(t *testing.T) {
	o := testOutbox(t)
	_ = os.WriteFile(filepath.Join(o.Dir(), "intent"), []byte("{not json"), 0o600)
	mustAppend(t, o, requestIDDraft(5), AppendWritten)
	matches, _ := filepath.Glob(filepath.Join(o.Dir(), "intent.corrupt-*"))
	if len(matches) != 1 || o.Stats().CorruptIntents != 1 {
		t.Errorf("corrupt intents kept %v, counted %d", matches, o.Stats().CorruptIntents)
	}
}

// WP2 r2 O2: dead letters count against the cap and are counted from a
// persisted counter.
func TestOutboxDeadLettersCountAgainstCap(t *testing.T) {
	o, lines := preparedReady(t, 2)
	reject := func(context.Context, []byte) error { return PermanentSendError(errors.New("no")) }
	if _, err := o.Drain(context.Background(), reject); err != nil {
		t.Fatal(err)
	}
	st := o.Stats()
	if st.PersistedDeadLetters != 2 || st.PendingBytes != 0 || st.RetainedBytes < int64(len(lines[0])+len(lines[1])) {
		t.Fatalf("stats = %+v", st)
	}
	o.maxPending = st.RetainedBytes + 10
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:new", 1), AppendOverflow)
}

// WP2 r2 O4: Finalize rotates staging under the lock and streams it
// outside; a finalizing file left by a crash is finalized next time.
func TestOutboxFinalizeLeftoverRotatedFile(t *testing.T) {
	o := testOutbox(t)
	_, _ = o.PinHubID("hub")
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:a", 1), AppendWritten)
	if _, err := o.rotateStaging(o.PinnedHubID()); err != nil {
		t.Fatal(err)
	}
	if left, _ := o.finalizingFiles(); len(left) != 1 {
		t.Fatalf("finalizing files = %v", left)
	}
	if o.Stats().PendingBytes == 0 {
		t.Error("a rotated, unfinalized file must count as pending")
	}
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:b", 1), AppendWritten) // appenders not blocked
	n, err := o.Finalize(o.PinnedHubID())
	if err != nil || n != 2 {
		t.Fatalf("Finalize = %d, %v; want 2 (leftover + new)", n, err)
	}
	c := &capture{}
	_, _ = o.Drain(context.Background(), c.send)
	if len(c.lines) != 2 || !strings.Contains(c.lines[0], "cmp1:a") || !strings.Contains(c.lines[1], "cmp1:b") {
		t.Errorf("sent = %q", c.lines)
	}
}

// token-analyst-2 (b987f781) #1: with staging sync failing persistently,
// a visible line is never committed as durable. The append is unresolved,
// the outbox holds (no appends, nothing finalized), and after a "machine
// crash" the reopened outbox decides from what actually survived.
func TestOutboxPersistentSyncFailureHoldsThenResolves(t *testing.T) {
	for _, lineSurvives := range []bool{false, true} {
		o := testOutbox(t)
		_, _ = o.PinHubID("hub")
		o.fsyncFile = func(*os.File) error { return errInjectedSync }
		o.truncateHook = func() error { return errInjectedSync }

		_, err := o.AppendUsage(requestIDDraft(5))
		if !errors.Is(err, ErrAnalyticsOutboxHeld) {
			t.Fatalf("append under persistent sync failure = %v; want held", err)
		}
		st := o.Stats()
		if !st.HeldIntent || st.Unresolved != 1 || st.PersistedAppendFailed != 0 {
			t.Errorf("stats after unresolved append = %+v", st)
		}
		// Held: a new event cannot be appended (and is counted lost), and
		// nothing is finalized behind the unresolved one.
		if _, err := o.AppendUsage(draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:other", 1)); !errors.Is(err, ErrAnalyticsOutboxHeld) {
			t.Errorf("append while held = %v", err)
		}
		if n, err := o.Finalize(o.PinnedHubID()); n != 0 || !errors.Is(err, ErrAnalyticsOutboxHeld) {
			t.Errorf("Finalize while held = %d, %v", n, err)
		}
		staged, _ := os.ReadFile(filepath.Join(o.Dir(), "staging.jsonl"))
		if len(staged) == 0 {
			t.Fatal("premise: the uncertain line should be visible")
		}
		if !lineSurvives {
			// The unsynced line is lost in the crash.
			_ = os.Truncate(filepath.Join(o.Dir(), "staging.jsonl"), 0)
		}

		// Reopen after the crash with storage healthy.
		reopened, err := OpenAnalyticsOutbox(o.Dir())
		if err != nil {
			t.Fatal(err)
		}
		if reopened.Stats().HeldIntent {
			t.Error("the intent must resolve once staging can be synced")
		}
		want := AppendWritten
		if lineSurvives {
			want = AppendCertainDuplicate
		}
		mustAppend(t, reopened, requestIDDraft(5), want)
		if got := len(stagingLines(t, reopened)); got != 1 {
			t.Errorf("lineSurvives=%v: staging lines = %d, want exactly 1", lineSurvives, got)
		}
		// Seq accounting is explicit: the held append had seq 1, the
		// append refused during the hold had seq 2 (both consumed). A new
		// event gets seq 3 either way; when the held line was lost, seq 1
		// is a real gap (no line carries it).
		next := mustAppend(t, reopened, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:after", 1), AppendWritten)
		if !lineSurvives {
			// The retry above took seq 3; this one is 4.
			if next.ProducerSeq != 4 {
				t.Errorf("line lost: next seq = %d, want 4", next.ProducerSeq)
			}
		} else if next.ProducerSeq != 3 {
			t.Errorf("line survived: next seq = %d, want 3", next.ProducerSeq)
		}
		seqs := map[int64]bool{}
		for _, l := range stagingLines(t, reopened) {
			seqs[decodeUsage(t, l).ProducerSeq] = true
		}
		if seqs[1] != lineSurvives || seqs[2] {
			t.Errorf("lineSurvives=%v: staged seqs = %v; seq 1 present only if the line survived, seq 2 (refused during the hold) never", lineSurvives, seqs)
		}
	}
}

// token-analyst-2 (b987f781) #2: a malformed intent whose verified-request
// line fully landed is re-indexed from the line itself, so the
// retransmission is a certain duplicate -- no second line with that
// event_id.
func TestOutboxMalformedIntentWithLandedLineIsReindexed(t *testing.T) {
	o := testOutbox(t)
	o.crashAt = "line"
	if _, err := o.AppendUsage(requestIDDraft(5)); !errors.Is(err, errSimulatedCrash) {
		t.Fatalf("simulated crash = %v", err)
	}
	_ = os.WriteFile(filepath.Join(o.Dir(), "intent"), []byte("{garbled"), 0o600)

	reopened, err := OpenAnalyticsOutbox(o.Dir())
	if err != nil {
		t.Fatal(err)
	}
	mustAppend(t, reopened, requestIDDraft(5), AppendCertainDuplicate)
	next := mustAppend(t, reopened, draftUsage(telemetrycontract.SourceIdentityRequestID, "rid1:req-B", 1), AppendWritten)
	if next.ProducerSeq != 2 {
		t.Errorf("seq after re-index = %d, want 2 (re-derived from the landed line)", next.ProducerSeq)
	}
	if got := len(stagingLines(t, reopened)); got != 2 {
		t.Errorf("staging lines = %d, want 2", got)
	}
	if reopened.Stats().CorruptIntents != 1 {
		t.Errorf("corrupt intents = %d", reopened.Stats().CorruptIntents)
	}
}

// WP2 r3 R2: the truncate-back cuts only the faulted append. Committed
// lines before it (including a torn tail left by a crashed writer) stay
// byte-identical and indexed.
func TestOutboxTruncateBackKeepsCommittedLines(t *testing.T) {
	o := testOutbox(t)
	mustAppend(t, o, requestIDDraft(5), AppendWritten)
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:c", 1), AppendWritten)
	// A torn fragment from a crashed writer.
	f, _ := os.OpenFile(filepath.Join(o.Dir(), "staging.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"torn":`)
	_ = f.Close()
	before, _ := os.ReadFile(filepath.Join(o.Dir(), "staging.jsonl"))

	failNextFileSync(o)
	if _, err := o.AppendUsage(draftUsage(telemetrycontract.SourceIdentityRequestID, "rid1:third", 1)); err == nil {
		t.Fatal("faulted append reported success")
	}
	after, _ := os.ReadFile(filepath.Join(o.Dir(), "staging.jsonl"))
	if string(after) != string(before) {
		t.Fatalf("truncate-back changed committed bytes:\nbefore %q\nafter  %q", before, after)
	}
	mustAppend(t, o, requestIDDraft(5), AppendCertainDuplicate)
	if c := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:c", 1), AppendWritten); c.SourceOccurrence != 2 {
		t.Errorf("composite repeat occurrence = %d, want 2", c.SourceOccurrence)
	}
	// The torn fragment stayed its own line: the next append wrote the
	// separator, and only valid lines decode.
	lines := stagingLines(t, o)
	if len(lines) != 4 || lines[2] != `{"torn":` {
		t.Errorf("staging lines = %q", lines)
	}
}

// WP2 r3 O6: ExpireIndex re-checks each entry under the lock (an entry
// refreshed after listing survives) and works over several batches.
func TestOutboxExpireIndexRecheckAndBatches(t *testing.T) {
	o := testOutbox(t)
	for _, id := range []string{"cmp1:a", "cmp1:b", "cmp1:c"} {
		mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, id, 1), AppendWritten)
	}
	o.now = func() time.Time { return time.Now().Add(analyticsIndexTTL + time.Hour) }
	o.expireBatch = 1
	refreshed := analyticsIndexName("sciontool/project/p/agent/a", "claude.api_request", "cmp1:b")
	batches := 0
	o.beforeExpireBatch = func() {
		batches++
		if batches == 1 {
			// An appender refreshes cmp1:b after the listing: it is now
			// newer than the cutoff and must survive.
			future := time.Now().Add(analyticsIndexTTL + 2*time.Hour)
			_ = os.Chtimes(filepath.Join(o.indexDir(), refreshed), future, future)
		}
	}
	removed, err := o.ExpireIndex()
	if err != nil || removed != 2 || batches != 3 {
		t.Errorf("ExpireIndex = %d, %v over %d batches; want 2 removed over 3 batches", removed, err, batches)
	}
	if _, err := os.Stat(filepath.Join(o.indexDir(), refreshed)); err != nil {
		t.Error("an entry refreshed after listing must survive")
	}
}

// WP2 r3 O1: finalizing files get monotonic names and never replace a
// leftover.
func TestOutboxFinalizingNamesMonotonic(t *testing.T) {
	o := testOutbox(t)
	_, _ = o.PinHubID("hub")
	for i := 0; i < 2; i++ {
		mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:"+string(rune('a'+i)), 1), AppendWritten)
		if _, err := o.rotateStaging(o.PinnedHubID()); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := o.finalizingFiles()
	want := []string{"finalizing-00000000000000000001.jsonl", "finalizing-00000000000000000002.jsonl"}
	if len(files) != 2 || files[0] != want[0] || files[1] != want[1] {
		t.Errorf("finalizing files = %v, want %v", files, want)
	}
	if n, err := o.Finalize(o.PinnedHubID()); err != nil || n != 2 {
		t.Errorf("Finalize = %d, %v", n, err)
	}
}

// WP2 r3 O4: leftover finalizing files are still delivered while a stuck
// intent holds rotation.
func TestOutboxFinalizeStreamsLeftoversWhileHeld(t *testing.T) {
	o := testOutbox(t)
	_, _ = o.PinHubID("hub")
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:old", 1), AppendWritten)
	if _, err := o.rotateStaging(o.PinnedHubID()); err != nil {
		t.Fatal(err)
	}
	o.fsyncFile = func(*os.File) error { return errInjectedSync }
	o.truncateHook = func() error { return errInjectedSync }
	if _, err := o.AppendUsage(requestIDDraft(5)); !errors.Is(err, ErrAnalyticsOutboxHeld) {
		t.Fatalf("append = %v; want held", err)
	}
	o.fsyncFile = func(f *os.File) error { return f.Sync() }
	o.fsyncDir = func(string) error { return errInjectedSync } // staging stays unsyncable
	n, err := o.Finalize(o.PinnedHubID())
	if n != 1 || !errors.Is(err, ErrAnalyticsOutboxHeld) {
		t.Errorf("Finalize while held = %d, %v; want the leftover finalized and the hold reported", n, err)
	}
}

// token-analyst-2 (60daf2e8): the lock goroutine acquires the flock right
// after the caller's wait timed out (between acquisition and the handoff
// check). The lock must still be released, not leaked.
func TestOutboxLockTimeoutHandoffReleases(t *testing.T) {
	o := testOutbox(t)
	holder, err := os.OpenFile(filepath.Join(o.Dir(), "lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	o.lockWait = 50 * time.Millisecond
	acquired := make(chan struct{})
	proceed := make(chan struct{})
	o.lockAcquiredHook = func() {
		close(acquired)
		<-proceed // stay between acquisition and the handoff check
	}
	errc := make(chan error, 1)
	go func() {
		_, err := o.lock()
		errc <- err
	}()
	if err := <-errc; !errors.Is(err, ErrAnalyticsOutboxLocked) {
		t.Fatalf("lock() = %v; want a timeout", err)
	}
	// Release the holder: the abandoned goroutine now acquires the flock
	// and parks in the hook, after the caller has given up.
	_ = unix.Flock(int(holder.Fd()), unix.LOCK_UN)
	<-acquired
	close(proceed)

	// The lock must become available again.
	o.lockAcquiredHook = nil
	o.lockWait = 2 * time.Second
	unlock, err := o.lock()
	if err != nil {
		t.Fatalf("lock leaked by the abandoned goroutine: %v", err)
	}
	unlock()
	_ = holder.Close()
}

// WP2 r4 O2: discarding an intent requires "absent" to be durable: if
// staging cannot be synced after the truncate-back, the outbox holds
// instead of discarding (a reappearing line would otherwise be unindexed).
func TestOutboxDiscardRequiresDurableAbsence(t *testing.T) {
	o := testOutbox(t)
	o.fsyncFile = func(*os.File) error {
		return errInjectedSync // append sync, truncate sync, recovery sync all fail
	}
	if _, err := o.AppendUsage(requestIDDraft(5)); !errors.Is(err, ErrAnalyticsOutboxHeld) {
		t.Fatalf("append = %v; want held (absence not durable)", err)
	}
	if !o.Stats().HeldIntent {
		t.Error("intent must be kept until absence is durable")
	}
	o.fsyncFile = func(f *os.File) error { return f.Sync() }
	reopened, _ := OpenAnalyticsOutbox(o.Dir())
	if reopened.Stats().HeldIntent {
		t.Error("intent must resolve once staging syncs")
	}
	ev := mustAppend(t, reopened, requestIDDraft(5), AppendWritten)
	if ev.ProducerSeq != 2 || len(stagingLines(t, reopened)) != 1 {
		t.Errorf("retry seq %d lines %d; want 2 (gap for the lost attempt) and 1", ev.ProducerSeq, len(stagingLines(t, reopened)))
	}
}

// WP2 r4 O3: a complete staging line the current build cannot accept is
// dead-lettered (kept, counted), not dropped.
func TestOutboxFinalizeDeadLettersInvalidLines(t *testing.T) {
	o := testOutbox(t)
	_, _ = o.PinHubID("hub")
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:ok", 1), AppendWritten)
	f, _ := os.OpenFile(filepath.Join(o.Dir(), "staging.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"schema_version":99,"event_type":"agent.usage"}` + "\n")
	_ = f.Close()
	if n, err := o.Finalize(o.PinnedHubID()); err != nil || n != 1 {
		t.Fatalf("Finalize = %d, %v", n, err)
	}
	dl, _ := os.ReadFile(filepath.Join(o.Dir(), "dead_letter.jsonl"))
	if !strings.Contains(string(dl), `"schema_version":99`) || o.Stats().PersistedDeadLetters != 1 {
		t.Errorf("invalid line not kept: dead letters %q, count %d", dl, o.Stats().PersistedDeadLetters)
	}
}

// WP2 r4 O5: hub ID resolution backs off while unresolved.
func TestAnalyticsDrainerResolveBackoff(t *testing.T) {
	o := testOutbox(t)
	calls := 0
	d := NewAnalyticsDrainer(o, (&capture{}).send, func(context.Context) (string, error) { calls++; return "", errors.New("no hub_id") })
	clock := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return clock }
	for i := 0; i < 5; i++ {
		d.RunOnce(context.Background())
		clock = clock.Add(500 * time.Millisecond)
	}
	if calls != 2 {
		t.Errorf("resolver called %d times in 2.5 s, want 2 (backoff 1 s, then 2 s)", calls)
	}
}

// WP3 interface: LedgerSnapshot reflects every write path's persisted
// evidence, independently of the producing handle (a fresh handle reads
// what another wrote), and MemoryStats touches no disk fields.
func TestOutboxLedgerSnapshotTracksWritePaths(t *testing.T) {
	o := testOutbox(t)
	_, _ = o.PinHubID("hub-snap")

	mustAppend(t, o, requestIDDraft(5), AppendWritten) // seq 1

	failNextFileSync(o) // truncated back: lost, append_failed, seq 2
	_, _ = o.AppendUsage(draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:lost", 1))

	calls := 0 // first sync fails, truncate stubbed to fail, recovery sync ok: sync_uncertain, seq 3
	o.fsyncFile = func(f *os.File) error {
		calls++
		if calls == 1 {
			return errInjectedSync
		}
		return f.Sync()
	}
	o.truncateHook = func() error { return errInjectedSync }
	_, _ = o.AppendUsage(draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:uncertain", 1))
	o.truncateHook = nil

	o.fsyncFile = func(f *os.File) error { return f.Sync() }
	o.crashAt = "commit" // commit deferred: written, seq 4 (after the sync-uncertain case, so the leftover intent's recovery sync is not the injected fault)
	_, _ = o.AppendUsage(draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:deferred", 1))
	o.crashAt = ""

	big := draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:big", 1) // oversize, seq 5
	big.SessionID = strings.Repeat("s", analyticsMaxLineBytes)
	mustAppend(t, o, big, AppendOversize)

	_, _ = o.Finalize(o.PinnedHubID())
	_, _ = o.Drain(context.Background(), func(context.Context, []byte) error { return PermanentSendError(errors.New("no")) })

	o.maxPending = 1 // overflow, seq 6
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:overflow", 1), AppendOverflow)

	fresh, err := OpenAnalyticsOutbox(o.Dir())
	if err != nil {
		t.Fatal(err)
	}
	d, err := fresh.LedgerSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	want := OutboxLedger{
		Dir: o.Dir(), Epoch: d.Epoch, LastSeq: 6, PinnedHubID: "hub-snap",
		OverflowTotal: 1, OversizeTotal: 1, AppendFailedTotal: 1, DeadLettersTotal: 3,
		SyncUncertainTotal: 1, CommitDeferredTotal: 1,
		Held: false, PendingBytes: 0, RetainedBytes: d.RetainedBytes,
	}
	if d.Epoch == "" || d.RetainedBytes == 0 {
		t.Errorf("epoch %q retained %d", d.Epoch, d.RetainedBytes)
	}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("snapshot\n got %+v\nwant %+v", d, want)
	}
	if m := fresh.MemoryStats(); m != (AnalyticsOutboxStats{}) {
		t.Errorf("a fresh handle's MemoryStats must be all zero (no disk fields): %+v", m)
	}
	if s := fresh.Stats(); s.PersistedDeadLetters != 3 || s.PersistedAppendFailed != 1 {
		t.Errorf("Stats must carry the snapshot: %+v", s)
	}

	// A malformed ledger file is an error, not a silent zero.
	_ = os.WriteFile(filepath.Join(o.Dir(), "seq"), []byte("x"), 0o600)
	if _, err := fresh.LedgerSnapshot(); err == nil {
		t.Error("malformed seq must be an error")
	}
}

// WP2 r5 R1: a hook's lazy open that times out on the lock (another
// process holds the outbox) still persists append_failed, lock-free.
func TestLazyOpenLockTimeoutPersistsAppendFailed(t *testing.T) {
	setAnalyticsEnv(t, "antigravity")
	o := testOutbox(t) // initialized outbox, as the pipeline leaves it
	holder, err := os.OpenFile(filepath.Join(o.Dir(), "lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	hook := NewLazyUsageAnalytics(o.Dir(), nil, telemetrycontract.UsageSourceHooks)
	_, err = hook.Record(UsageObservation{SourceKind: "hook.antigravity.model_end", Status: "success", Calls: 1, TokenStatus: "not_reported"})
	if err == nil {
		t.Fatal("record under a held lock succeeded")
	}
	_ = unix.Flock(int(holder.Fd()), unix.LOCK_UN)
	_ = holder.Close()
	fresh, err := OpenAnalyticsOutbox(o.Dir())
	if err != nil {
		t.Fatal(err)
	}
	d, err := fresh.LedgerSnapshot()
	if err != nil || d.AppendFailedTotal != 1 || hook.Invalid() != 1 {
		t.Errorf("append_failed = %d (err %v), invalid = %d; want 1 and 1", d.AppendFailedTotal, err, hook.Invalid())
	}
}

// WP2 r5 R1 (pipeline side): a lock timeout at pipeline start does not
// disable analytics when the outbox is already initialized.
func TestPipelineOpenToleratesLockTimeout(t *testing.T) {
	o := testOutbox(t)
	holder, err := os.OpenFile(filepath.Join(o.Dir(), "lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAnalyticsOutbox(o.Dir()); !errors.Is(err, ErrAnalyticsOutboxLocked) {
		t.Fatalf("premise: a strict open under the held lock must time out, got %v", err)
	}
	tolerant, err := openAnalyticsOutboxTolerant(o.Dir(), nil)
	_ = unix.Flock(int(holder.Fd()), unix.LOCK_UN)
	_ = holder.Close()
	if err != nil || tolerant == nil {
		t.Fatalf("tolerant open under a held lock = %v", err)
	}
	mustAppend(t, tolerant, requestIDDraft(5), AppendWritten)
}

// Audit: a pipeline whose outbox cannot be opened still leaves evidence
// for every accepted increment (here no directory can exist, so the
// per-process invalid counter; with a directory, persisted append_failed).
func TestAnalyticsStartFailureKeepsEvidence(t *testing.T) {
	setAnalyticsEnv(t, "claude")
	blocker := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocker, nil, 0o600)
	a := analyticsState{outboxDir: filepath.Join(blocker, "home", ".scion", AnalyticsOutboxDirName)}
	a.start(context.Background(), &GCPExporter{})
	if a.disabledReason != "outbox_unavailable" || a.native == nil || a.drainer != nil {
		t.Fatalf("state = %+v", a)
	}
	d := bareUsageDeriver(claudeUsageRule{})
	d.analytics = a.native
	feed(d, claudeLogs(claudeRecord("", "1", 1_000, fullClaudeTokens)))
	if a.native.Invalid() != 1 || d.Diagnostics().Derived != 1 {
		t.Errorf("invalid = %d derived = %d; the counters keep working and the loss is counted", a.native.Invalid(), d.Diagnostics().Derived)
	}
	if got := (&Pipeline{analytics: a}).AnalyticsOutboxDir(); got != a.outboxDir {
		t.Errorf("AnalyticsOutboxDir in the failed state = %q, want %q (and no panic)", got, a.outboxDir)
	}
}

// Audit: a recovery I/O error at the start of an append consumes a seq,
// like a hold, so the refused observation is a gap.
func TestOutboxRecoveryErrorConsumesSeq(t *testing.T) {
	o := testOutbox(t)
	if err := os.Mkdir(filepath.Join(o.Dir(), "intent"), 0o700); err != nil { // unreadable intent
		t.Fatal(err)
	}
	if _, err := o.AppendUsage(requestIDDraft(5)); err == nil {
		t.Fatal("append with an unreadable intent succeeded")
	}
	_ = os.Remove(filepath.Join(o.Dir(), "intent"))
	next := mustAppend(t, o, requestIDDraft(5), AppendWritten)
	if next.ProducerSeq != 2 || o.Stats().PersistedAppendFailed != 1 {
		t.Errorf("seq %d append_failed %d; want 2 (gap) and 1", next.ProducerSeq, o.Stats().PersistedAppendFailed)
	}
}

// WP2 r5 O1: the delivered-at-timeout branch. The goroutine hands the lock
// to the caller, and the caller's timer fires before the send; the caller
// must take ownership and release. Hooks force this exact interleaving.
func TestOutboxLockTimeoutDeliveredBranchReleases(t *testing.T) {
	o := testOutbox(t)
	o.lockWait = time.Millisecond
	reached := make(chan struct{})
	timedOut := make(chan struct{})
	o.lockDeliveredHook = func() {
		close(reached) // state == delivered
		<-timedOut     // hold the send until the caller has timed out
	}
	o.lockTimeoutHook = func() {
		<-reached // the timer fires only after the handoff
		close(timedOut)
	}
	if _, err := o.lock(); !errors.Is(err, ErrAnalyticsOutboxLocked) {
		t.Fatalf("lock() = %v; want a timeout through the delivered branch", err)
	}
	o.lockDeliveredHook, o.lockTimeoutHook = nil, nil
	o.lockWait = 100 * time.Millisecond
	unlock, err := o.lock()
	if err != nil {
		t.Fatalf("lock leaked after the delivered-branch timeout: %v", err)
	}
	unlock()
}

// failedStartState builds the root-init failed-start state over an existing
// outbox whose recovery fails (an unreadable intent), with ownership seams
// recording every chown. It returns the state, its dir and the chown log.
func failedStartState(t *testing.T) (analyticsState, string, func() map[string]OutboxOwner) {
	t.Helper()
	setAnalyticsEnv(t, "claude")
	dir := filepath.Join(t.TempDir(), "home", ".scion", AnalyticsOutboxDirName)
	if err := os.MkdirAll(filepath.Join(dir, "intent"), 0o700); err != nil { // pre-existing, unreadable intent
		t.Fatal(err)
	}
	var mu sync.Mutex
	chowned := map[string]OutboxOwner{}
	prevEuid, prevChown := outboxGeteuid, outboxChown
	outboxGeteuid = func() int { return 0 }
	outboxChown = func(p string, uid, gid int) error {
		mu.Lock()
		defer mu.Unlock()
		rel, _ := filepath.Rel(dir, p)
		if i := strings.Index(rel, ".tmp-"); i > 0 && strings.HasPrefix(filepath.Base(rel), ".") {
			base := filepath.Base(rel)
			rel = filepath.Join(filepath.Dir(rel), base[1:strings.Index(base, ".tmp-")])
		}
		chowned[rel] = OutboxOwner{uid, gid}
		return nil
	}
	t.Cleanup(func() { outboxGeteuid, outboxChown = prevEuid, prevChown })
	a := analyticsState{outboxDir: dir, owner: &OutboxOwner{UID: 1000, GID: 1001}}
	a.start(context.Background(), &GCPExporter{})
	if a.disabledReason != "outbox_unavailable" || a.drainer != nil {
		t.Fatalf("premise: start must fail with the directory present: %+v", a)
	}
	return a, dir, func() map[string]OutboxOwner {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]OutboxOwner{}
		for k, v := range chowned {
			out[k] = v
		}
		return out
	}
}

// WP2 r6 R1 + tele-em: in the failed-start state every exported method is
// safe, and every file or directory the outbox code created is owned by
// the agent user with mode 0600 (files) / 0700 (directories). Hook-side
// evidence keeps working afterwards.
func TestFailedStartStateMethodsAndOwnership(t *testing.T) {
	a, dir, chowned := failedStartState(t)
	p := &Pipeline{analytics: a}

	// A native increment in the failed state leaves owned evidence.
	d := bareUsageDeriver(claudeUsageRule{})
	d.analytics = a.native
	feed(d, claudeLogs(claudeRecord("", "1", 1_000, fullClaudeTokens)))

	// Pipeline methods.
	if got := p.AnalyticsOutboxDir(); got != dir {
		t.Errorf("AnalyticsOutboxDir = %q, want %q", got, dir)
	}
	if _, _, ok, err := p.AnalyticsLedger(); ok || err != nil {
		t.Errorf("AnalyticsLedger in the failed state = ok %v, err %v; want not ok, no error", ok, err)
	}
	p.SetAnalyticsOutbox(filepath.Dir(filepath.Dir(dir)), 1000, 1001)

	// UsageAnalytics methods.
	u := a.native
	if u.Dir() != dir || u.Outbox() != nil || u.Invalid() != 1 || u.DroppedNoIdentity() != 0 {
		t.Errorf("producer: dir %q outbox %v invalid %d dropped %d", u.Dir(), u.Outbox(), u.Invalid(), u.DroppedNoIdentity())
	}
	if _, err := u.Record(UsageObservation{SourceKind: "x", Status: "success", Calls: 1, TokenStatus: "not_reported"}); err == nil {
		t.Error("Record in the failed state must report the error")
	}

	// AnalyticsOutbox methods on the nil outbox the failed state has.
	var o *AnalyticsOutbox
	_ = o.Dir()
	_ = o.PinnedHubID()
	_ = o.Stats()
	_ = o.MemoryStats()
	for name, err := range map[string]error{
		"AppendUsage":     func() error { _, err := o.AppendUsage(requestIDDraft(1)); return err }(),
		"PinHubID":        func() error { _, err := o.PinHubID("hub"); return err }(),
		"Finalize":        func() error { _, err := o.Finalize(o.PinnedHubID()); return err }(),
		"FinalizeContext": func() error { _, err := o.FinalizeContext(context.Background(), o.PinnedHubID()); return err }(),
		"Drain":           func() error { _, err := o.Drain(context.Background(), nil); return err }(),
		"ExpireIndex":     func() error { _, err := o.ExpireIndex(); return err }(),
		"LedgerSnapshot":  func() error { _, err := o.LedgerSnapshot(); return err }(),
	} {
		if err == nil {
			t.Errorf("nil outbox %s must return an error", name)
		}
	}
	// Drainer methods on the nil drainer the failed state has.
	var dr *AnalyticsDrainer
	dr.Start(context.Background())
	dr.Stop(context.Background())
	_ = dr.RunOnce(context.Background())
	_ = dr.Backoff()
	a.stop(context.Background())

	// Every created path: chowned to the agent user, with the right mode.
	owners := chowned()
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == "." || rel == "intent" {
			return nil // created by the test (MkdirAll for the unreadable intent), not by the outbox
		}
		if got, ok := owners[rel]; !ok || got != (OutboxOwner{1000, 1001}) {
			t.Errorf("%s not owned by the agent user (got %v, %v)", rel, got, ok)
		}
		want := os.FileMode(0o600)
		if info.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode %v, want %v", rel, info.Mode().Perm(), want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := owners["append_failed"]; !ok {
		t.Error("append_failed evidence was not created/owned")
	}
}

// WP2 r6 O1: append error, then recovery finds the line durable but the
// commit fails: written (commit deferred), not counted as lost.
func TestOutboxAppendErrorWithFailedCommitIsDeferred(t *testing.T) {
	o := testOutbox(t)
	calls := 0
	o.fsyncFile = func(f *os.File) error {
		calls++
		if calls == 1 {
			return errInjectedSync // the append's own sync
		}
		return f.Sync()
	}
	o.truncateHook = func() error { return errInjectedSync } // the line stays
	o.crashAt = "commit"                                     // recovery's commit fails
	got, err := o.AppendUsage(requestIDDraft(5))
	if err != nil || got != AppendWritten {
		t.Fatalf("append = %v, %v; want written (deferred)", got, err)
	}
	o.crashAt, o.truncateHook = "", nil
	mustAppend(t, o, requestIDDraft(5), AppendCertainDuplicate)
	if st := o.Stats(); st.PersistedAppendFailed != 0 || st.PersistedCommitDeferred != 1 {
		t.Errorf("stats = %+v; want no append_failed, one commit_deferred", st)
	}
}

// WP2 r6 O2: a lazy open whose recovery fails (lock held) consumes a seq.
func TestLazyOpenRecoveryErrorConsumesSeq(t *testing.T) {
	setAnalyticsEnv(t, "antigravity")
	o := testOutbox(t)
	if err := os.Mkdir(filepath.Join(o.Dir(), "intent"), 0o700); err != nil {
		t.Fatal(err)
	}
	hook := NewLazyUsageAnalytics(o.Dir(), nil, telemetrycontract.UsageSourceHooks)
	if _, err := hook.Record(UsageObservation{SourceKind: "hook.x.model_end", Status: "success", Calls: 1, TokenStatus: "not_reported"}); err == nil {
		t.Fatal("record with a failing open succeeded")
	}
	_ = os.Remove(filepath.Join(o.Dir(), "intent"))
	next := mustAppend(t, o, requestIDDraft(5), AppendWritten)
	if next.ProducerSeq != 2 || o.Stats().PersistedAppendFailed != 1 {
		t.Errorf("seq %d append_failed %d; want 2 (gap) and 1", next.ProducerSeq, o.Stats().PersistedAppendFailed)
	}
}

// Pre-review B1: a quarantine whose cursor commit was lost leaves the
// cursor at (N, offset>0) with no ready-N. New events must not be written
// to a reused ready-N (Drain would seek them to the stale offset), and a
// later quarantine must never overwrite kept bytes.
func TestOutboxStaleCursorAfterQuarantineLosesNothing(t *testing.T) {
	o, lines := preparedReady(t, 2)
	files, _ := o.readyFiles()
	n := files[0]
	// Simulate the interrupted quarantine: ready-N renamed, cursor stale.
	_ = os.Rename(filepath.Join(o.Dir(), readyName(n)), filepath.Join(o.Dir(), fmt.Sprintf("quarantine-%d.jsonl", n)))
	if err := o.writeCursor(n, int64(len(lines[0])+1)); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:after1", 1), AppendWritten)
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:after2", 1), AppendWritten)
	if _, err := o.Finalize(o.PinnedHubID()); err != nil {
		t.Fatal(err)
	}
	if next, _ := o.readyFiles(); len(next) != 1 || next[0] == n {
		t.Fatalf("new ready files %v must not reuse number %d", next, n)
	}
	c := &capture{}
	if _, err := o.Drain(context.Background(), c.send); err != nil {
		t.Fatal(err)
	}
	if len(c.lines) != 2 || !strings.Contains(c.lines[0], "cmp1:after1") || !strings.Contains(c.lines[1], "cmp1:after2") {
		t.Errorf("events after the stale cursor = %q; both must be delivered whole", c.lines)
	}
	// A second quarantine for the same number never overwrites the first.
	_ = os.WriteFile(filepath.Join(o.Dir(), readyName(n)), []byte(`{"truncated`), 0o600)
	_ = o.writeCursor(n, 0)
	if _, err := o.Drain(context.Background(), c.send); err != nil {
		t.Fatal(err)
	}
	q, _ := filepath.Glob(filepath.Join(o.Dir(), fmt.Sprintf("quarantine-%d*.jsonl", n)))
	if len(q) != 2 {
		t.Errorf("quarantine files = %v; the first must be kept", q)
	}
}

// Pre-review B2: a failed chown never leaves a root-owned file behind; the
// next attempt re-creates and re-chowns it.
func TestOutboxChownFailureIsRetried(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "home", ".scion", AnalyticsOutboxDirName)
	failures := map[string]int{}
	owned := map[string]bool{}
	prevEuid, prevChown := outboxGeteuid, outboxChown
	outboxGeteuid = func() int { return 0 }
	outboxChown = func(p string, uid, gid int) error {
		base := filepath.Base(p)
		if (base == "lock" || base == "staging.jsonl") && failures[base] == 0 {
			failures[base]++
			return errors.New("injected chown failure")
		}
		owned[base] = true
		return nil
	}
	defer func() { outboxGeteuid, outboxChown = prevEuid, prevChown }()
	owner := &OutboxOwner{UID: 1000, GID: 1001}

	if _, err := OpenAnalyticsOutboxOwned(dir, owner); err == nil {
		t.Fatal("open with a failing lock chown succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, "lock")); !os.IsNotExist(err) {
		t.Fatal("a lock file whose chown failed must be removed")
	}
	o, err := OpenAnalyticsOutboxOwned(dir, owner) // retry: re-created, chowned
	if err != nil || !owned["lock"] {
		t.Fatalf("retry open = %v, lock owned %v", err, owned["lock"])
	}
	if _, err := o.AppendUsage(draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:a", 1)); err == nil {
		t.Fatal("append with a failing staging chown succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, "staging.jsonl")); !os.IsNotExist(err) {
		t.Error("a staging file whose chown failed must be removed (else it stays root-owned)")
	}
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:b", 1), AppendWritten)
	if !owned["staging.jsonl"] || !owned["append_failed"] {
		t.Errorf("owned = %v; staging and the append_failed evidence must end up agent-owned", owned)
	}
}

// Pre-review M1: a missing epoch at append time consumes a seq.
func TestOutboxMissingEpochConsumesSeq(t *testing.T) {
	o := testOutbox(t)
	epoch, _ := os.ReadFile(filepath.Join(o.Dir(), "epoch"))
	_ = os.Remove(filepath.Join(o.Dir(), "epoch"))
	if _, err := o.AppendUsage(requestIDDraft(5)); err == nil {
		t.Fatal("append without an epoch succeeded")
	}
	_ = os.WriteFile(filepath.Join(o.Dir(), "epoch"), epoch, 0o600)
	if next := mustAppend(t, o, requestIDDraft(5), AppendWritten); next.ProducerSeq != 2 {
		t.Errorf("seq after the refusal = %d, want 2 (gap)", next.ProducerSeq)
	}
}

// Pre-review M2: recovery must not take an older line with the same
// deterministic event_id (index expired) for the intent's line.
func TestOutboxRecoveryMatchesSeqNotJustEventID(t *testing.T) {
	o := testOutbox(t)
	first := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:x", 1), AppendWritten)
	// The index entry expires; the same identity is observed again and
	// gets occurrence 1 (same event_id); crash after the intent.
	_ = os.RemoveAll(o.indexDir())
	_ = os.Mkdir(o.indexDir(), 0o700)
	o.crashAt = "intent"
	if _, err := o.AppendUsage(draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:x", 1)); !errors.Is(err, errSimulatedCrash) {
		t.Fatalf("simulated crash = %v", err)
	}
	o.crashAt = ""
	reopened, _ := OpenAnalyticsOutbox(o.Dir())
	next := mustAppend(t, reopened, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:other", 1), AppendWritten)
	// The crashed observation (seq 2) was lost: a gap, so the next is 3,
	// and the older line (seq 1, same event_id) was not mistaken for it.
	if first.ProducerSeq != 1 || next.ProducerSeq != 3 || len(stagingLines(t, reopened)) != 2 {
		t.Errorf("first seq %d, next seq %d, lines %d; want 1, 3, 2", first.ProducerSeq, next.ProducerSeq, len(stagingLines(t, reopened)))
	}
}

// WP2 r7 O1: a failed pipeline start (no observation in flight) does not
// consume a seq; O2: Dir() on a lazy producer does not open the outbox.
func TestFailedPipelineStartNoSpuriousGapAndLazyDirNoOpen(t *testing.T) {
	a, dir, _ := failedStartState(t)
	_ = a
	if _, err := os.Stat(filepath.Join(dir, "seq")); !os.IsNotExist(err) {
		t.Error("a failed pipeline start must not write a seq gap")
	}

	lazyDir := filepath.Join(t.TempDir(), "home", ".scion", AnalyticsOutboxDirName)
	lazy := NewLazyUsageAnalytics(lazyDir, nil, telemetrycontract.UsageSourceHooks)
	if got := lazy.Dir(); got != lazyDir {
		t.Errorf("Dir = %q, want %q", got, lazyDir)
	}
	if _, err := os.Stat(lazyDir); !os.IsNotExist(err) {
		t.Error("Dir() must not open (create) a lazy outbox")
	}
}
