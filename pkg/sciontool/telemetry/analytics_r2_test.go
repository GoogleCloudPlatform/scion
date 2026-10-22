/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
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
	if _, err := o.rotateStaging(); err != nil {
		t.Fatal(err)
	}
	if left, _ := o.finalizingFiles(); len(left) != 1 {
		t.Fatalf("finalizing files = %v", left)
	}
	if o.Stats().PendingBytes == 0 {
		t.Error("a rotated, unfinalized file must count as pending")
	}
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:b", 1), AppendWritten) // appenders not blocked
	n, err := o.Finalize()
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
		if n, err := o.Finalize(); n != 0 || !errors.Is(err, ErrAnalyticsOutboxHeld) {
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
		if _, err := o.rotateStaging(); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := o.finalizingFiles()
	want := []string{"finalizing-00000000000000000001.jsonl", "finalizing-00000000000000000002.jsonl"}
	if len(files) != 2 || files[0] != want[0] || files[1] != want[1] {
		t.Errorf("finalizing files = %v, want %v", files, want)
	}
	if n, err := o.Finalize(); err != nil || n != 2 {
		t.Errorf("Finalize = %d, %v", n, err)
	}
}

// WP2 r3 O4: leftover finalizing files are still delivered while a stuck
// intent holds rotation.
func TestOutboxFinalizeStreamsLeftoversWhileHeld(t *testing.T) {
	o := testOutbox(t)
	_, _ = o.PinHubID("hub")
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:old", 1), AppendWritten)
	if _, err := o.rotateStaging(); err != nil {
		t.Fatal(err)
	}
	o.fsyncFile = func(*os.File) error { return errInjectedSync }
	o.truncateHook = func() error { return errInjectedSync }
	if _, err := o.AppendUsage(requestIDDraft(5)); !errors.Is(err, ErrAnalyticsOutboxHeld) {
		t.Fatalf("append = %v; want held", err)
	}
	o.fsyncFile = func(f *os.File) error { return f.Sync() }
	o.fsyncDir = func(string) error { return errInjectedSync } // staging stays unsyncable
	n, err := o.Finalize()
	if n != 1 || !errors.Is(err, ErrAnalyticsOutboxHeld) {
		t.Errorf("Finalize while held = %d, %v; want the leftover finalized and the hold reported", n, err)
	}
}
