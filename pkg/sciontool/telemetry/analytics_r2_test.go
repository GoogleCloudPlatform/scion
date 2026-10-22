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
	_, _ = o.AppendUsage(requestIDDraft(5))
	if !failed {
		t.Fatal("dir sync fault was not injected")
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
	if st := o.Stats(); st.PersistedAppendFailed != 0 || st.SyncUncertain != 1 {
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
