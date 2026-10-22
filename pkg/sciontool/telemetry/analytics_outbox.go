/*
Copyright 2026 The Scion Authors.
*/

package telemetry

// analytics_outbox.go is the durable accepted-observation ledger for
// agent.usage analytics events (WP2 design note
// telemetry/design/wp2-design.md, sections 2 and 4). It is the dedicated
// analytics path: producers (the native UsageDeriver in the pipeline
// process, and short-lived hook processes) append final event bytes here,
// and the pipeline's drainer finalizes and ships them to Cloud Logging. It
// never touches the OTLP receiver, the UsageDeriver or the agent log.
//
// Layout of the outbox directory:
//
//	lock            flock target serializing every mutation
//	epoch           random ID, created once with the outbox
//	seq             last assigned producer_seq
//	hub_id          pinned hub ID (written once, never overwritten)
//	overflow        persisted count of events refused because the outbox was full
//	staging.jsonl   appended by producers; lines may lack hub_id
//	finalizing-N.jsonl  staging rotated by Finalize, being streamed to ready
//	intent.corrupt-*    unreadable intents, kept for inspection
//	dead_letters    persisted count of dead-lettered lines
//	sync_uncertain, commit_deferred  one byte per append that needed recovery
//	ready-N.jsonl   final bytes written by Finalize; never modified
//	cursor          "N offset" of the next ready line to send
//	index/<hash>    persisted occurrence count for one source identity
//
//	intent          the in-flight append (see below)
//	append_failed   one byte per event lost to a lock timeout or I/O error
//	oversize        persisted count of events refused for exceeding the line cap
//	dead_letter.jsonl  lines Cloud Logging rejected permanently (kept, counted)
//	quarantine-N.jsonl  a ready file whose tail was found truncated; kept
//	                    byte-for-byte for explicit recovery, never sent
//
// Crash semantics per append (all under the lock): (1) write intent
// {event_id, seq, index entry}; (2) append and fsync the event line;
// (3) persist seq and the index entry; (4) remove intent. Every locked
// mutation first recovers a leftover intent: if the staging file's last
// complete line is the intent's event, steps 3-4 are completed; otherwise
// the event never became durable and the intent is discarded, so a source
// retry is accepted as a first sighting again. The index therefore never
// claims an event that is not durable, and a durable event is never missing
// from the index.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"golang.org/x/sys/unix"
)

// Outbox bounds (design section 2).
const (
	analyticsOutboxMaxPending = 64 << 20
	analyticsIndexTTL         = 24 * time.Hour
	analyticsIndexMaxEntries  = 100_000
	analyticsLockWait         = 2 * time.Second
	// analyticsMaxLineBytes caps one event line well below Cloud Logging's
	// per-entry limit (256 KB), so a line can never be rejected for size.
	analyticsMaxLineBytes = 64 << 10
	// analyticsMaxHubIDBytes bounds the pinned hub ID (an unauthenticated
	// /healthz value stamped into every event).
	analyticsMaxHubIDBytes = 128
)

// AnalyticsOutboxDirName is the outbox directory under the agent home's
// .scion directory.
const AnalyticsOutboxDirName = "analytics-outbox"

// AnalyticsOutboxDirFor returns the outbox directory for the AGENT home
// (<home>/.scion/analytics-outbox), or "" if home is empty. Both producers
// must pass the agent home: sciontool init (which runs as root with
// HOME=/root) passes its resolved agent home, and hook processes (which run
// as the agent user) pass $HOME -- the same pair that already locates
// agent-info.json.
func AnalyticsOutboxDirFor(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".scion", AnalyticsOutboxDirName)
}

// OutboxOwner is the agent user that must own every outbox file. When the
// creating process runs as root (sciontool init), each created directory
// and file is chowned to it so agent-user hook processes can lock and
// append.
type OutboxOwner struct{ UID, GID int }

// AppendOutcome is what AppendUsage did with an event.
type AppendOutcome int

const (
	// AppendWritten means the event line is durable in staging.
	AppendWritten AppendOutcome = iota
	// AppendCertainDuplicate means a verified request ID was already
	// recorded; nothing was written.
	AppendCertainDuplicate
	// AppendOverflow means the outbox was full; the event was refused and
	// its producer_seq was consumed, leaving a visible gap.
	AppendOverflow
	// AppendOversize means the event line exceeded analyticsMaxLineBytes;
	// it was refused like an overflow (seq consumed, persisted count).
	AppendOversize
)

// ErrAnalyticsOutboxLocked is returned when the outbox lock could not be
// taken within analyticsLockWait.
var ErrAnalyticsOutboxLocked = errors.New("analytics outbox: lock wait timed out")

// AnalyticsOutboxStats are the outbox's fixed-cardinality counters, for WP3
// diagnostics. PendingBytes, RetainedBytes, OldestPendingAge, HeldIntent
// and the Persisted* fields are read from disk (shared by all processes and
// kept across restarts); the rest are per process.
type AnalyticsOutboxStats struct {
	Appended, CertainDuplicates, Overflow, AppendFailed int64
	Finalized, Delivered, SendFailures, Torn            int64
	HubIDMismatch, Conflicts, Quarantined, DeadLetters  int64
	SyncUncertain, CorruptIntents, Unresolved           int64
	CommitDeferred                                      int64
	PersistedSyncUncertain, PersistedCommitDeferred     int64
	// HeldIntent is true while an unresolved append holds the outbox.
	HeldIntent                                  bool
	PersistedOverflow, PersistedOversize        int64
	PersistedAppendFailed, PersistedDeadLetters int64
	PendingBytes, RetainedBytes                 int64
	OldestPendingAge                            time.Duration
}

// AnalyticsOutbox is a handle on one outbox directory. It is safe for use
// by one goroutine for Finalize/Drain and any number of goroutines and
// processes for AppendUsage.
type AnalyticsOutbox struct {
	dir        string
	maxPending int64
	indexTTL   time.Duration
	indexMax   int
	now        func() time.Time

	appended, certainDup, overflow, appendFailed atomic.Int64
	finalized, delivered, sendFailures, torn     atomic.Int64
	hubIDMismatch, conflicts, quarantined        atomic.Int64

	// owner, when set and the process runs as root, receives ownership of
	// every created file and directory.
	owner   *OutboxOwner
	geteuid func() int
	chown   func(path string, uid, gid int) error

	deadLetters, syncUncertain, corruptIntents, unresolved, commitDeferred atomic.Int64

	// lockWait overrides analyticsLockWait and lockAcquiredHook runs in the
	// lock goroutine right after the flock is acquired (test seams).
	lockWait         time.Duration
	lockAcquiredHook func()

	// expireBatch is ExpireIndex's per-lock batch size and beforeExpireBatch
	// runs before each batch (test seams).
	expireBatch       int
	beforeExpireBatch func()

	// fsyncFile and fsyncDir are the file and directory sync calls (test
	// seams for sync faults).
	fsyncFile func(*os.File) error
	fsyncDir  func(dir string) error

	// openFile opens a ready file for Drain (test seam for read faults).
	openFile func(name string) (readyFile, error)
	// truncateHook, when set, replaces the truncate step (tests).
	truncateHook func() error
	// crashAt, when set, makes appendUsage stop right after the named step
	// ("intent" or "line") without cleaning up, simulating a crash (tests).
	crashAt string
}

// readyFile is what Drain reads a ready file through.
type readyFile interface {
	io.ReadSeekCloser
}

// ErrAnalyticsOutboxHeld means an in-flight append's line is visible in
// staging but could not be made durable (staging sync keeps failing). The
// intent is kept and appends and finalization are held until a later
// operation can sync and commit it (or finds the line gone): an uncertain
// event is never claimed durable, and nothing ships behind it.
var ErrAnalyticsOutboxHeld = errors.New("analytics outbox: held on an unresolved append (staging sync failing)")

// errAppendUnresolved wraps ErrAnalyticsOutboxHeld for the append whose own
// line is the unresolved one (counted as unresolved, not as lost).
var errAppendUnresolved = fmt.Errorf("this append is unresolved: %w", ErrAnalyticsOutboxHeld)

// errSimulatedCrash is returned by appendUsage at a crashAt step (tests).
var errSimulatedCrash = errors.New("analytics outbox: simulated crash")

// OpenAnalyticsOutbox opens (creating if needed) the outbox at dir, owned by
// the current user.
func OpenAnalyticsOutbox(dir string) (*AnalyticsOutbox, error) {
	return OpenAnalyticsOutboxOwned(dir, nil)
}

// OpenAnalyticsOutboxOwned opens (creating if needed) the outbox at dir.
// When owner is set and the process runs as root, everything the outbox
// creates is chowned to owner.
func OpenAnalyticsOutboxOwned(dir string, owner *OutboxOwner) (*AnalyticsOutbox, error) {
	o := newOutboxHandle(dir, owner)
	if err := o.open(); err != nil {
		return nil, err
	}
	return o, nil
}

// outboxGeteuid and outboxChown are the process euid and chown used by new
// outbox handles (test seams for the root-init ownership path).
var (
	outboxGeteuid = os.Geteuid
	outboxChown   = os.Lchown
)

func newOutboxHandle(dir string, owner *OutboxOwner) *AnalyticsOutbox {
	return &AnalyticsOutbox{
		dir:        dir,
		maxPending: analyticsOutboxMaxPending,
		indexTTL:   analyticsIndexTTL,
		indexMax:   analyticsIndexMaxEntries,
		now:        time.Now,
		owner:      owner,
		geteuid:    outboxGeteuid,
		chown:      outboxChown,
		openFile:   func(name string) (readyFile, error) { return os.Open(name) },
		fsyncFile:  func(f *os.File) error { return f.Sync() },
		fsyncDir:   syncDir,
	}
}

func (o *AnalyticsOutbox) open() error {
	if o.dir == "" {
		return errors.New("analytics outbox: empty directory")
	}
	// The agent home normally exists; create it (unchowned) only if a test
	// or unusual layout lacks it. Then create the home's .scion, the
	// outbox and its index one level at a time so each new one is chowned.
	if err := os.MkdirAll(filepath.Dir(filepath.Dir(o.dir)), 0o755); err != nil {
		return fmt.Errorf("analytics outbox: %w", err)
	}
	for _, d := range []string{filepath.Dir(o.dir), o.dir, o.indexDir()} {
		if err := o.mkdir(d); err != nil {
			return err
		}
	}
	unlock, err := o.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Stat(o.path("epoch")); errors.Is(err, os.ErrNotExist) {
		if err := o.writeFileAtomic("epoch", []byte(telemetrycontract.NewEventID())); err != nil {
			return err
		}
	}
	// A held outbox still opens (hooks need a handle to count failures);
	// mutating operations stay blocked until the intent resolves.
	if err := o.recoverIntentLocked(); err != nil && !errors.Is(err, ErrAnalyticsOutboxHeld) {
		return err
	}
	return nil
}

// mkdir creates d (whose parent must exist) if missing, chowns it, and
// syncs the parent directory.
func (o *AnalyticsOutbox) mkdir(d string) error {
	if _, err := os.Stat(d); err == nil {
		return nil
	}
	if err := os.Mkdir(d, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("analytics outbox: %w", err)
	}
	if err := o.own(d); err != nil {
		return err
	}
	return syncDir(filepath.Dir(d))
}

// own chowns a path this process created to the outbox owner, when one is
// set and the process runs as root.
func (o *AnalyticsOutbox) own(path string) error {
	if o.owner == nil || o.geteuid() != 0 || o.owner.UID <= 0 {
		return nil
	}
	if err := o.chown(path, o.owner.UID, o.owner.GID); err != nil {
		return fmt.Errorf("analytics outbox: chown %s: %w", path, err)
	}
	return nil
}

// Dir returns the outbox directory.
func (o *AnalyticsOutbox) Dir() string { return o.dir }

func (o *AnalyticsOutbox) path(name string) string { return filepath.Join(o.dir, name) }
func (o *AnalyticsOutbox) indexDir() string        { return filepath.Join(o.dir, "index") }

// lock takes the outbox's exclusive flock, waiting up to analyticsLockWait.
func (o *AnalyticsOutbox) lock() (func(), error) {
	_, statErr := os.Stat(o.path("lock"))
	f, err := os.OpenFile(o.path("lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("analytics outbox: %w", err)
	}
	if errors.Is(statErr, os.ErrNotExist) {
		if err := o.own(o.path("lock")); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	// A blocking flock queues in the kernel instead of polling, so a
	// waiter is not starved by peers that re-take the lock immediately. It
	// runs in a goroutine so the wait stays bounded. Ownership of the lock
	// and the file is handed over under mu: exactly one side releases them
	// -- the caller if the result was delivered, the goroutine if the wait
	// was abandoned first.
	const (
		waiting = iota
		delivered
		abandoned
	)
	var mu sync.Mutex
	state := waiting
	done := make(chan error, 1) // buffered: the goroutine never blocks on send
	go func() {
		var err error
		for {
			err = unix.Flock(int(f.Fd()), unix.LOCK_EX)
			if !errors.Is(err, unix.EINTR) {
				break
			}
		}
		if o.lockAcquiredHook != nil {
			o.lockAcquiredHook()
		}
		mu.Lock()
		if state == abandoned {
			mu.Unlock()
			if err == nil {
				_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
			}
			_ = f.Close()
			return
		}
		state = delivered
		mu.Unlock()
		done <- err
	}()
	wait := o.lockWait
	if wait <= 0 {
		wait = analyticsLockWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("analytics outbox: locking: %w", err)
		}
	case <-timer.C:
		mu.Lock()
		if state == delivered {
			// The goroutine handed over just before the timeout: the
			// result is (or is about to be) in done, and it is ours to
			// release.
			mu.Unlock()
			if err := <-done; err == nil {
				_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
			}
			_ = f.Close()
			return nil, ErrAnalyticsOutboxLocked
		}
		state = abandoned // the goroutine releases when it gets the lock
		mu.Unlock()
		return nil, ErrAnalyticsOutboxLocked
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}

// AppendUsage assigns the event's ledger fields -- source_occurrence,
// identity_ambiguous, event_id, producer_epoch, producer_seq and (when
// pinned) hub_id -- under the outbox lock, then durably appends its final
// bytes to staging. The caller fills every other field. On
// AppendCertainDuplicate and AppendOverflow nothing is written.
func (o *AnalyticsOutbox) AppendUsage(ev *telemetrycontract.UsageEvent) (AppendOutcome, error) {
	outcome, err := o.appendUsage(ev)
	switch {
	case errors.Is(err, errAppendUnresolved):
		// Not known lost: the intent persists and decides later.
		o.unresolved.Add(1)
	case err != nil:
		o.appendFailed.Add(1)
		// Persisted, lock-free evidence of the loss (a hook process's
		// in-memory counter dies with it).
		o.bumpAppendFailed()
	case outcome == AppendWritten:
		o.appended.Add(1)
	case outcome == AppendCertainDuplicate:
		o.certainDup.Add(1)
	case outcome == AppendOverflow, outcome == AppendOversize:
		o.overflow.Add(1)
	}
	return outcome, err
}

// bumpAppendFailed appends one byte to the append_failed file with
// O_APPEND, without the lock (it may be the lock that failed); the file
// size is the persisted count.
func (o *AnalyticsOutbox) bumpAppendFailed() { o.bumpPersisted("append_failed") }

// bumpPersisted appends one byte to the named counter file (O_APPEND, no
// lock needed); its size is the persisted count, visible across hook
// processes and restarts.
func (o *AnalyticsOutbox) bumpPersisted(name string) {
	p := o.path(name)
	_, statErr := os.Stat(p)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write([]byte{'x'})
	_ = f.Sync()
	_ = f.Close()
	if errors.Is(statErr, os.ErrNotExist) {
		_ = o.own(p)
	}
}

// refuseLocked consumes seq and increments the persisted counter file name
// (overflow or oversize), leaving a visible seq gap for a refused event.
func (o *AnalyticsOutbox) refuseLocked(seq int64, counter string) error {
	if err := o.writeFileAtomic("seq", []byte(strconv.FormatInt(seq, 10))); err != nil {
		return err
	}
	n, _ := o.readInt(counter)
	return o.writeFileAtomic(counter, []byte(strconv.FormatInt(n+1, 10)))
}

func (o *AnalyticsOutbox) appendUsage(ev *telemetrycontract.UsageEvent) (outcome AppendOutcome, err error) {
	unlock, err := o.lock()
	if err != nil {
		return 0, err
	}
	defer unlock()
	if err := o.recoverIntentLocked(); err != nil {
		if errors.Is(err, ErrAnalyticsOutboxHeld) {
			// This observation is refused while held: consume a seq for it
			// (after the held intent's), so the loss is a visible gap.
			o.consumeSeqDuringHold()
		}
		return 0, err
	}

	epoch, err := o.readTrimmed("epoch")
	if err != nil || epoch == "" {
		return 0, fmt.Errorf("analytics outbox: reading epoch: %v", err)
	}
	seq, err := o.readInt("seq")
	if err != nil {
		return 0, err
	}
	seq++
	// Any failure from here on consumes the seq (best effort), so the lost
	// observation is a visible gap; AppendUsage also persists the count.
	committed := false
	defer func() {
		if err != nil && !committed && !errors.Is(err, errSimulatedCrash) {
			if cur, rerr := o.readInt("seq"); rerr == nil && cur < seq {
				_ = o.writeFileAtomic("seq", []byte(strconv.FormatInt(seq, 10)))
			}
		}
	}()

	ev.ProducerEpoch = epoch
	ev.ProducerSeq = seq
	if hubID, _ := o.readTrimmed("hub_id"); hubID != "" {
		ev.HubID = hubID
	}

	// Occurrence from the persisted index. A verified request ID repeat is
	// a certain duplicate only if its semantic content matches a recorded
	// observation; conflicting content is written as a further occurrence
	// (WP4 quarantines a request_id group with more than one distinct row).
	var indexName, fingerprint string
	var entry indexEntry
	occurrence := int64(1)
	if ev.SourceIdentity != telemetrycontract.SourceIdentityNone {
		indexName = analyticsIndexName(ev.ProducerNamespace, ev.SourceKind, ev.SourceEventID)
		if entry, err = o.readIndex(indexName); err != nil {
			return 0, err
		}
		if ev.SourceIdentity == telemetrycontract.SourceIdentityRequestID {
			if fingerprint, err = usageSemanticFingerprint(ev); err != nil {
				return 0, err
			}
			if entry.count > 0 {
				if entry.has(fingerprint) {
					return AppendCertainDuplicate, nil
				}
				o.conflicts.Add(1)
			}
		}
		occurrence = entry.count + 1
	}
	ev.SourceOccurrence = occurrence
	if occurrence > 1 || ev.SourceIdentity == telemetrycontract.SourceIdentityNone {
		ev.IdentityAmbiguous = true
	}
	if ev.SourceIdentity == telemetrycontract.SourceIdentityNone {
		ev.EventID = telemetrycontract.NewEventID()
	} else {
		ev.EventID = usageEventID(ev.ProducerNamespace, ev.SourceKind, ev.SourceEventID, occurrence)
	}

	line, err := telemetrycontract.Marshal(ev)
	if err != nil {
		return 0, err
	}
	// Refusals consume the seq (a visible gap) and leave the index
	// untouched (the observation was not accepted).
	if len(line)+1 > analyticsMaxLineBytes {
		committed = true
		return AppendOversize, o.refuseLocked(seq, "oversize")
	}
	retained, err := o.retainedBytes()
	if err != nil {
		return 0, err
	}
	if retained+int64(len(line))+1 > o.maxPending {
		committed = true
		return AppendOverflow, o.refuseLocked(seq, "overflow")
	}

	next := entry
	if indexName != "" {
		next.count = occurrence
		if fingerprint != "" && !next.has(fingerprint) {
			next.fingerprints = append(append([]string(nil), entry.fingerprints...), fingerprint)
		}
	}
	in := appendIntent{EventID: ev.EventID, Seq: seq, Index: indexName, Count: next.count, Fingerprints: next.fingerprints}
	if err := o.writeIntent(in); err != nil {
		return 0, err
	}
	if o.crashAt == "intent" {
		return 0, errSimulatedCrash
	}
	if before, err := o.appendLineAt("staging.jsonl", line); err != nil {
		// An error does not prove the line is absent (a failed fsync can
		// follow a complete write). Try to take the write back, then let
		// recovery decide from the file itself: either the line is absent
		// (intent discarded, the event is lost and counted) or it is the
		// last complete line (seq and index committed, the event stands).
		// Never "present but unindexed".
		if before >= 0 {
			if terr := o.truncateTo("staging.jsonl", before); terr != nil {
				err = errors.Join(err, terr)
			}
		}
		landed, rerr := o.recoverIntent()
		if errors.Is(rerr, ErrAnalyticsOutboxHeld) {
			// Visible but not durable: neither accepted nor lost yet.
			committed = true // seq stays with the intent
			return 0, errAppendUnresolved
		}
		if rerr != nil {
			return 0, errors.Join(err, rerr)
		}
		if landed {
			// The first sync failed, but recovery synced the line and
			// committed it: durable now, not a loss.
			committed = true
			o.syncUncertain.Add(1)
			o.bumpPersisted("sync_uncertain")
			return AppendWritten, nil
		}
		return 0, err
	}
	if o.crashAt == "line" {
		return 0, errSimulatedCrash
	}
	committed = true // the line is durable; a failed commit is completed by recovery
	if err := o.commitIntent(in); err != nil {
		// Not a loss: the intent stays and the next locked operation
		// completes it from the durable line.
		o.commitDeferred.Add(1)
		o.bumpPersisted("commit_deferred")
		return AppendWritten, nil
	}
	return AppendWritten, nil
}

// truncateTo cuts name back to size and syncs it, undoing an append whose
// durability is uncertain.
func (o *AnalyticsOutbox) truncateTo(name string, size int64) error {
	if o.truncateHook != nil {
		return o.truncateHook()
	}
	f, err := os.OpenFile(o.path(name), os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("analytics outbox: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(size); err != nil {
		return fmt.Errorf("analytics outbox: truncating %s: %w", name, err)
	}
	return o.fsyncFile(f)
}

// consumeSeqDuringHold advances seq past both the persisted value and the
// held intent's seq (best effort; caller holds the lock).
func (o *AnalyticsOutbox) consumeSeqDuringHold() {
	cur, err := o.readInt("seq")
	if err != nil {
		return
	}
	if b, err := os.ReadFile(o.path("intent")); err == nil {
		var in appendIntent
		if json.Unmarshal(b, &in) == nil && in.Seq > cur {
			cur = in.Seq
		}
	}
	_ = o.writeFileAtomic("seq", []byte(strconv.FormatInt(cur+1, 10)))
}

// appendIntent is the in-flight append record (see the file comment).
type appendIntent struct {
	EventID      string   `json:"event_id"`
	Seq          int64    `json:"seq"`
	Index        string   `json:"index,omitempty"`
	Count        int64    `json:"count,omitempty"`
	Fingerprints []string `json:"fingerprints,omitempty"`
}

func (o *AnalyticsOutbox) writeIntent(in appendIntent) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return o.writeFileAtomic("intent", b)
}

// commitIntent persists the intent's seq and index entry, then removes it.
func (o *AnalyticsOutbox) commitIntent(in appendIntent) error {
	if o.crashAt == "commit" {
		return errSimulatedCrash
	}
	if cur, err := o.readInt("seq"); err != nil {
		return err
	} else if in.Seq > cur {
		if err := o.writeFileAtomic("seq", []byte(strconv.FormatInt(in.Seq, 10))); err != nil {
			return err
		}
	}
	if in.Index != "" {
		if err := o.writeIndex(in.Index, indexEntry{count: in.Count, fingerprints: in.Fingerprints}); err != nil {
			return err
		}
	}
	if err := os.Remove(o.path("intent")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("analytics outbox: %w", err)
	}
	return syncDir(o.dir)
}

// recoverIntentLocked completes or discards a leftover intent (caller
// holds the lock). Appends are serialized, so the intent's line, if it
// became durable, is the staging file's last complete line.
func (o *AnalyticsOutbox) recoverIntentLocked() error {
	_, err := o.recoverIntent()
	return err
}

// recoverIntent is recoverIntentLocked that reports whether the intent's
// line was found and committed.
func (o *AnalyticsOutbox) recoverIntent() (landed bool, err error) {
	b, err := os.ReadFile(o.path("intent"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("analytics outbox: %w", err)
	}
	var in appendIntent
	if json.Unmarshal(b, &in) != nil || in.EventID == "" {
		// Unreadable intent (atomic rename makes this unexpected). Its line,
		// if it landed, is staging's last complete line: re-derive that
		// event's seq and index entry from the line itself (idempotent for
		// an already indexed event), after making staging durable. Then
		// keep the intent for inspection as intent.corrupt-<ns>.
		if err := o.syncStaging(); err != nil {
			return false, fmt.Errorf("%w: %v", ErrAnalyticsOutboxHeld, err)
		}
		if err := o.reindexLastStagingLine(); err != nil {
			return false, err
		}
		o.corruptIntents.Add(1)
		name := fmt.Sprintf("intent.corrupt-%d", o.now().UnixNano())
		if err := os.Rename(o.path("intent"), o.path(name)); err != nil {
			return false, fmt.Errorf("analytics outbox: quarantining intent: %w", err)
		}
		return false, o.fsyncDir(o.dir)
	}
	last, err := o.lastStagingEventID()
	if err != nil {
		return false, err
	}
	if last == in.EventID {
		// Visible is not durable: commit the index only after staging
		// (and its directory entry) sync. Otherwise keep the intent and
		// hold.
		if err := o.syncStaging(); err != nil {
			return true, fmt.Errorf("%w: %v", ErrAnalyticsOutboxHeld, err)
		}
		return true, o.commitIntent(in)
	}
	// The line is absent -- but "absent" must be durable too (a truncate
	// whose sync failed could reappear after a crash). Hold until staging
	// syncs, as the commit branch does.
	if err := o.syncStaging(); err != nil {
		return false, fmt.Errorf("%w: %v", ErrAnalyticsOutboxHeld, err)
	}
	// The line did not survive: the observation is lost. Consume its seq so
	// the loss is a visible gap downstream (the index is left untouched, so
	// a source retry is a clean first sighting).
	if cur, err := o.readInt("seq"); err != nil {
		return false, err
	} else if in.Seq > cur {
		if err := o.writeFileAtomic("seq", []byte(strconv.FormatInt(in.Seq, 10))); err != nil {
			return false, err
		}
	}
	if err := os.Remove(o.path("intent")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("analytics outbox: %w", err)
	}
	return false, o.fsyncDir(o.dir)
}

// syncStaging fsyncs the staging file and the outbox directory, making a
// visible staged line durable.
func (o *AnalyticsOutbox) syncStaging() error {
	f, err := os.Open(o.path("staging.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return o.fsyncDir(o.dir)
	}
	if err != nil {
		return fmt.Errorf("analytics outbox: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := o.fsyncFile(f); err != nil {
		return fmt.Errorf("analytics outbox: syncing staging: %w", err)
	}
	return o.fsyncDir(o.dir)
}

// lastStagingEventID returns the event_id of the staging file's last
// complete (newline-terminated) line, or "" if there is none or the file
// ends in a torn fragment.
func (o *AnalyticsOutbox) lastStagingEventID() (string, error) {
	body, err := o.lastStagingLine()
	if err != nil || body == nil {
		return "", err
	}
	var head struct {
		EventID string `json:"event_id"`
	}
	if json.Unmarshal(body, &head) != nil {
		return "", nil
	}
	return head.EventID, nil
}

// lastStagingLine returns the staging file's last complete
// (newline-terminated) line, or nil if there is none or the file ends in a
// torn fragment.
func (o *AnalyticsOutbox) lastStagingLine() ([]byte, error) {
	f, err := os.Open(o.path("staging.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("analytics outbox: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("analytics outbox: %w", err)
	}
	size := info.Size()
	if size == 0 {
		return nil, nil
	}
	start := size - analyticsMaxLineBytes - 1
	if start < 0 {
		start = 0
	}
	tail := make([]byte, size-start)
	if _, err := f.ReadAt(tail, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("analytics outbox: %w", err)
	}
	if tail[len(tail)-1] != '\n' {
		return nil, nil
	}
	body := tail[:len(tail)-1]
	if i := bytes.LastIndexByte(body, '\n'); i >= 0 {
		body = body[i+1:]
	}
	return body, nil
}

// reindexLastStagingLine makes sure the event on staging's last complete
// line is reflected in seq and its occurrence index entry (count at least
// its occurrence; its fingerprint recorded for a verified request ID). It
// is idempotent for an event that is already indexed.
func (o *AnalyticsOutbox) reindexLastStagingLine() error {
	body, err := o.lastStagingLine()
	if err != nil || body == nil {
		return err
	}
	var ev telemetrycontract.UsageEvent
	if telemetrycontract.Unmarshal(body, &ev) != nil {
		return nil // torn or foreign: nothing to index
	}
	if cur, err := o.readInt("seq"); err != nil {
		return err
	} else if ev.ProducerSeq > cur {
		if err := o.writeFileAtomic("seq", []byte(strconv.FormatInt(ev.ProducerSeq, 10))); err != nil {
			return err
		}
	}
	if ev.SourceIdentity == telemetrycontract.SourceIdentityNone {
		return nil
	}
	name := analyticsIndexName(ev.ProducerNamespace, ev.SourceKind, ev.SourceEventID)
	entry, err := o.readIndex(name)
	if err != nil {
		return err
	}
	changed := false
	if entry.count < ev.SourceOccurrence {
		entry.count, changed = ev.SourceOccurrence, true
	}
	if ev.SourceIdentity == telemetrycontract.SourceIdentityRequestID {
		fp, err := usageSemanticFingerprint(&ev)
		if err != nil {
			return err
		}
		if !entry.has(fp) {
			entry.fingerprints = append(entry.fingerprints, fp)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return o.writeIndex(name, entry)
}

// usageSemanticFingerprint hashes the event's source-determined content:
// everything except observation and ledger fields (event_id, observed_ts,
// producer_build, harness_version, hub_id, producer_epoch/seq,
// source_occurrence, identity_ambiguous). A retransmission matches; a
// conflicting payload (or a changed normalization) does not.
func usageSemanticFingerprint(ev *telemetrycontract.UsageEvent) (string, error) {
	c := *ev
	c.EventID, c.ObservedTS, c.ProducerBuild, c.HarnessVersion, c.HubID = "", "", "", "", ""
	c.ProducerEpoch, c.ProducerSeq, c.SourceOccurrence, c.IdentityAmbiguous = "", 0, 0, false
	b, err := json.Marshal(&c)
	if err != nil {
		return "", fmt.Errorf("analytics outbox: fingerprint: %w", err)
	}
	return lengthPrefixedHash(string(b)), nil
}

// usageEventID is the deterministic event ID for a source observation
// (design section 4): u1- plus 32 hex characters of sha256 over the
// length-prefixed namespace, source kind, source event ID and occurrence.
func usageEventID(namespace, sourceKind, sourceEventID string, occurrence int64) string {
	sum := lengthPrefixedHash(namespace, sourceKind, sourceEventID, strconv.FormatInt(occurrence, 10))
	return "u1-" + sum[:32]
}

func analyticsIndexName(namespace, sourceKind, sourceEventID string) string {
	return lengthPrefixedHash(namespace, sourceKind, sourceEventID)
}

// lengthPrefixedHash hashes parts unambiguously: each part is preceded by
// its decimal length and a colon, so no concatenation of different parts
// can collide.
func lengthPrefixedHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = fmt.Fprintf(h, "%d:", len(p))
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// appendLine durably appends line plus a newline to name. If the file does
// not end in a newline (a torn line left by a crashed writer), a newline is
// written first so the torn fragment stays a separate, undecodable line.
func (o *AnalyticsOutbox) appendLine(name string, line []byte) error {
	_, err := o.appendLineAt(name, line)
	return err
}

// appendLineAt is appendLine that also reports the file size before the
// write (-1 if the file could not be opened), so a caller can truncate a
// write whose durability is uncertain.
func (o *AnalyticsOutbox) appendLineAt(name string, line []byte) (int64, error) {
	p := o.path(name)
	_, statErr := os.Stat(p)
	created := errors.Is(statErr, os.ErrNotExist)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return -1, fmt.Errorf("analytics outbox: %w", err)
	}
	defer func() { _ = f.Close() }()
	// The pre-append size is the truncate-back point; without it nothing
	// may be written (a wrong offset could cut committed lines).
	info, err := f.Stat()
	if err != nil {
		return -1, fmt.Errorf("analytics outbox: stat %s: %w", name, err)
	}
	before := info.Size()
	buf := make([]byte, 0, len(line)+2)
	if before > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, before-1); err != nil {
			return -1, fmt.Errorf("analytics outbox: reading %s: %w", name, err)
		}
		if last[0] != '\n' {
			buf = append(buf, '\n')
		}
	}
	buf = append(buf, line...)
	buf = append(buf, '\n')
	if _, err := f.Write(buf); err != nil {
		return before, fmt.Errorf("analytics outbox: writing %s: %w", name, err)
	}
	if err := o.fsyncFile(f); err != nil {
		return before, fmt.Errorf("analytics outbox: syncing %s: %w", name, err)
	}
	if created {
		if err := o.own(p); err != nil {
			return before, err
		}
		if err := o.fsyncDir(o.dir); err != nil {
			return before, err
		}
	}
	return before, nil
}

// PinHubID records hubID as the outbox's hub on first call; later calls
// with a different value keep the pinned one and report a mismatch.
func (o *AnalyticsOutbox) PinHubID(hubID string) (pinned string, err error) {
	if err := validHubID(hubID); err != nil {
		return "", err
	}
	unlock, err := o.lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	current, err := o.readTrimmed("hub_id")
	if err != nil {
		return "", err
	}
	if current != "" {
		if current != hubID {
			o.hubIDMismatch.Add(1)
		}
		return current, nil
	}
	if err := o.writeFileAtomic("hub_id", []byte(hubID)); err != nil {
		return "", err
	}
	return hubID, nil
}

// validHubID bounds a hub ID taken from an unauthenticated /healthz body:
// 1-128 bytes of [A-Za-z0-9._:-].
func validHubID(id string) error {
	if id == "" || len(id) > analyticsMaxHubIDBytes {
		return fmt.Errorf("analytics outbox: hub ID length %d outside 1..%d", len(id), analyticsMaxHubIDBytes)
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._:-", c)) {
			return fmt.Errorf("analytics outbox: hub ID contains %q; want [A-Za-z0-9._:-]", c)
		}
	}
	return nil
}

// PinnedHubID returns the pinned hub ID, or "".
func (o *AnalyticsOutbox) PinnedHubID() string {
	id, _ := o.readTrimmed("hub_id")
	return id
}

// Finalize and Drain must be called from one goroutine, sequentially (the
// pipeline's drainer): Drain reads ready files and the cursor without the
// lock, and only Finalize creates finalizing and ready files (rotating
// staging under the lock, streaming outside it).
//
// Finalize moves staging lines to a new ready file as final bytes: lines
// that carry hub_id are copied unchanged; lines without it get the pinned
// hub ID and are re-marshaled. It does nothing until a hub ID is pinned.
// Undecodable (torn) lines are dropped and counted. A crash after the
// ready file is synced but before staging is truncated repeats the copy;
// the duplicate lines are byte-identical, which is neutral downstream.
func (o *AnalyticsOutbox) Finalize() (int, error) {
	return o.FinalizeContext(context.Background())
}

// FinalizeContext is Finalize that stops between files when ctx is done.
// Leftover finalizing files are streamed even when rotation is held or
// fails (a stuck intent stalls only staging, not what is already rotated).
func (o *AnalyticsOutbox) FinalizeContext(ctx context.Context) (int, error) {
	// Under the lock, only recover an intent and rotate staging to a
	// finalizing file (cheap renames), so appenders never wait for the
	// copy. The finalizing files are then streamed into ready files
	// without the lock: only this goroutine touches them.
	_, rotErr := o.rotateStaging()
	hubID := o.PinnedHubID()
	if hubID == "" {
		return 0, rotErr
	}
	files, err := o.finalizingFiles()
	if err != nil {
		return 0, errors.Join(rotErr, err)
	}
	total := 0
	for _, name := range files {
		if err := ctx.Err(); err != nil {
			return total, errors.Join(rotErr, err)
		}
		n, err := o.finalizeFile(name, hubID)
		total += n
		if err != nil {
			o.finalized.Add(int64(total))
			return total, errors.Join(rotErr, err)
		}
	}
	o.finalized.Add(int64(total))
	return total, rotErr
}

// rotateStaging renames a non-empty staging file to finalizing-<n>.jsonl
// under the lock, after recovering any intent (its line is then either
// committed or absent). It returns the pinned hub ID ("" = not yet, do
// nothing).
func (o *AnalyticsOutbox) rotateStaging() (string, error) {
	unlock, err := o.lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	if err := o.recoverIntentLocked(); err != nil {
		return "", err
	}
	hubID, err := o.readTrimmed("hub_id")
	if err != nil || hubID == "" {
		return "", err
	}
	info, err := os.Stat(o.path("staging.jsonl"))
	if errors.Is(err, os.ErrNotExist) || err == nil && info.Size() == 0 {
		return hubID, nil
	}
	if err != nil {
		return "", fmt.Errorf("analytics outbox: %w", err)
	}
	// Monotonic, never-reused name (highest existing + 1, zero-padded so
	// lexical order is numeric order); an existing file is never replaced.
	existing, err := o.finalizingFiles()
	if err != nil {
		return "", err
	}
	next := int64(1)
	if len(existing) > 0 {
		last := existing[len(existing)-1]
		n, perr := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(last, "finalizing-"), ".jsonl"), 10, 64)
		if perr != nil {
			return "", fmt.Errorf("analytics outbox: unexpected file %s", last)
		}
		next = n + 1
	}
	name := fmt.Sprintf("finalizing-%020d.jsonl", next)
	if _, err := os.Stat(o.path(name)); err == nil {
		return "", fmt.Errorf("analytics outbox: %s already exists", name)
	}
	if err := os.Rename(o.path("staging.jsonl"), o.path(name)); err != nil {
		return "", fmt.Errorf("analytics outbox: rotating staging: %w", err)
	}
	return hubID, o.fsyncDir(o.dir)
}

// finalizingFiles lists finalizing files oldest first (including leftovers
// from a crash mid-finalize).
func (o *AnalyticsOutbox) finalizingFiles() ([]string, error) {
	entries, err := os.ReadDir(o.dir)
	if err != nil {
		return nil, fmt.Errorf("analytics outbox: %w", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "finalizing-") && strings.HasSuffix(e.Name(), ".jsonl") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out) // zero-padded numbers: lexical order is numeric order
	return out, nil
}

// finalizeFile streams one finalizing file into a new ready file (lines
// with hub_id copied byte for byte, others stamped and re-marshaled,
// undecodable lines dropped and counted), then removes it. A crash after
// the ready file is renamed but before the remove repeats the copy; the
// duplicate lines are byte-identical, which is neutral downstream.
func (o *AnalyticsOutbox) finalizeFile(name, hubID string) (int, error) {
	src, err := os.Open(o.path(name))
	if err != nil {
		return 0, fmt.Errorf("analytics outbox: %w", err)
	}
	defer func() { _ = src.Close() }()
	next, err := o.nextReadyNumber()
	if err != nil {
		return 0, err
	}
	n := 0
	errNothing := errors.New("nothing to finalize")
	err = o.writeFileFrom(o.dir, readyName(next), func(w io.Writer) error {
		r := bufio.NewReaderSize(src, 64<<10)
		for {
			line, readErr := r.ReadBytes('\n')
			line = bytes.TrimSuffix(line, []byte("\n"))
			if len(line) > 0 {
				var ev telemetrycontract.UsageEvent
				final := line
				uerr := telemetrycontract.Unmarshal(line, &ev)
				if uerr == nil && ev.HubID == "" {
					ev.HubID = hubID
					final, uerr = telemetrycontract.Marshal(&ev)
				}
				if uerr != nil {
					// A torn fragment, or a line this build cannot accept
					// (e.g. written by an older build): never drop it --
					// keep it in the dead-letter file (persisted count).
					o.torn.Add(1)
					if derr := o.deadLetter(line); derr != nil {
						return derr
					}
				} else {
					if _, werr := w.Write(append(final, '\n')); werr != nil {
						return werr
					}
					n++
				}
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					break
				}
				return readErr
			}
		}
		if n == 0 {
			return errNothing
		}
		return nil
	})
	if err != nil && !errors.Is(err, errNothing) {
		return 0, err
	}
	if err := os.Remove(o.path(name)); err != nil {
		return n, fmt.Errorf("analytics outbox: %w", err)
	}
	return n, o.fsyncDir(o.dir)
}

func readyName(n int64) string { return fmt.Sprintf("ready-%d.jsonl", n) }

// readyFiles lists ready file numbers in ascending order.
func (o *AnalyticsOutbox) readyFiles() ([]int64, error) {
	entries, err := os.ReadDir(o.dir)
	if err != nil {
		return nil, fmt.Errorf("analytics outbox: %w", err)
	}
	var out []int64
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "ready-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(name, "ready-"), ".jsonl"), 10, 64)
		if err == nil {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func (o *AnalyticsOutbox) nextReadyNumber() (int64, error) {
	files, err := o.readyFiles()
	if err != nil {
		return 0, err
	}
	cursorFile, _, err := o.readCursor()
	if err != nil {
		return 0, err
	}
	next := cursorFile
	if len(files) > 0 && files[len(files)-1] >= next {
		next = files[len(files)-1] + 1
	}
	if next < 1 {
		next = 1
	}
	return next, nil
}

// permanentSendError marks a send failure the destination will repeat for
// the same bytes (invalid argument, oversized entry, unparsable line).
type permanentSendError struct{ err error }

func (e permanentSendError) Error() string { return "permanent: " + e.err.Error() }
func (e permanentSendError) Unwrap() error { return e.err }

// PermanentSendError wraps err as a permanent send failure.
func PermanentSendError(err error) error { return permanentSendError{err} }

// IsPermanentSendError reports whether err was wrapped by PermanentSendError.
func IsPermanentSendError(err error) bool {
	var p permanentSendError
	return errors.As(err, &p)
}

// deadLetter durably appends a permanently rejected line to
// dead_letter.jsonl and increments the persisted dead_letters count. The
// file counts against the outbox cap (retainedBytes), so a destination
// that rejects everything ends in visible overflow, not unbounded disk.
func (o *AnalyticsOutbox) deadLetter(line []byte) error {
	if err := o.appendLine("dead_letter.jsonl", line); err != nil {
		return err
	}
	o.deadLetters.Add(1)
	n, _ := o.readInt("dead_letters")
	return o.writeFileAtomic("dead_letters", []byte(strconv.FormatInt(n+1, 10)))
}

// retainedBytes is everything the outbox keeps on disk for events:
// undelivered bytes plus dead-lettered and quarantined files. The cap
// applies to it.
func (o *AnalyticsOutbox) retainedBytes() (int64, error) {
	pending, _, err := o.pendingBytes()
	if err != nil {
		return 0, err
	}
	kept, err := o.keptBytes()
	return pending + kept, err
}

// keptBytes sums dead-letter and quarantine files (kept, not delivered).
func (o *AnalyticsOutbox) keptBytes() (int64, error) {
	entries, err := os.ReadDir(o.dir)
	if err != nil {
		return 0, fmt.Errorf("analytics outbox: %w", err)
	}
	var kept int64
	for _, e := range entries {
		name := e.Name()
		if name == "dead_letter.jsonl" || strings.HasPrefix(name, "quarantine-") {
			if info, err := e.Info(); err == nil {
				kept += info.Size()
			}
		}
	}
	return kept, nil
}

func (o *AnalyticsOutbox) readCursor() (file, offset int64, err error) {
	s, err := o.readTrimmed("cursor")
	if err != nil || s == "" {
		return 0, 0, err
	}
	a, b, ok := strings.Cut(s, " ")
	if !ok {
		return 0, 0, fmt.Errorf("analytics outbox: malformed cursor %q", s)
	}
	if file, err = strconv.ParseInt(a, 10, 64); err == nil {
		offset, err = strconv.ParseInt(b, 10, 64)
	}
	if err != nil {
		return 0, 0, fmt.Errorf("analytics outbox: malformed cursor %q", s)
	}
	return file, offset, nil
}

func (o *AnalyticsOutbox) writeCursor(file, offset int64) error {
	return o.writeFileAtomic("cursor", []byte(fmt.Sprintf("%d %d", file, offset)))
}

// AnalyticsSendFunc delivers one final event line. A nil error means the
// destination accepted it; an error wrapped by PermanentSendError means it
// never will (the line is dead-lettered); any other error is retried.
type AnalyticsSendFunc func(ctx context.Context, line []byte) error

// Drain sends ready lines in order from the cursor, committing the cursor
// after each accepted line and deleting a ready file once fully sent. It
// stops at the first send error and returns it; the same bytes are sent
// again next time. Called only from the pipeline's drainer goroutine.
func (o *AnalyticsOutbox) Drain(ctx context.Context, send AnalyticsSendFunc) (int, error) {
	files, err := o.readyFiles()
	if err != nil {
		return 0, err
	}
	curFile, curOffset, err := o.readCursor()
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, n := range files {
		if n < curFile {
			// Fully sent before a crash interrupted the delete.
			_ = os.Remove(o.path(readyName(n)))
			continue
		}
		offset := int64(0)
		if n == curFile {
			offset = curOffset
		}
		f, err := o.openFile(o.path(readyName(n)))
		if err != nil {
			return sent, fmt.Errorf("analytics outbox: %w", err)
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			_ = f.Close()
			return sent, fmt.Errorf("analytics outbox: %w", err)
		}
		r := bufio.NewReaderSize(f, 64<<10)
		truncated := false
		for {
			if err := ctx.Err(); err != nil {
				_ = f.Close()
				return sent, err
			}
			line, readErr := r.ReadBytes('\n')
			if readErr == nil {
				if body := line[:len(line)-1]; len(body) > 0 {
					if err := send(ctx, body); err != nil {
						o.sendFailures.Add(1)
						if !IsPermanentSendError(err) {
							_ = f.Close()
							return sent, err
						}
						// The destination will never accept this line:
						// keep it in the dead-letter file (counted) and move
						// on instead of blocking every later event.
						if derr := o.deadLetter(body); derr != nil {
							_ = f.Close()
							return sent, derr
						}
					} else {
						sent++
						o.delivered.Add(1)
					}
				}
				offset += int64(len(line))
				if err := o.writeCursor(n, offset); err != nil {
					_ = f.Close()
					return sent, err
				}
				continue
			}
			if !errors.Is(readErr, io.EOF) {
				// A read failure is never completion: keep the cursor and
				// the file, and retry later.
				_ = f.Close()
				return sent, fmt.Errorf("analytics outbox: reading %s: %w", readyName(n), readErr)
			}
			// Clean EOF. Only a complete-line boundary finishes the file;
			// leftover bytes mean the file was truncated.
			truncated = len(line) > 0
			break
		}
		_ = f.Close()
		if truncated {
			// Ready files are written whole, so a partial tail is
			// corruption: keep the file byte-for-byte as quarantine-N for
			// explicit recovery (never sent, never counted delivered), and
			// move on so later events are not blocked behind it.
			o.quarantined.Add(1)
			if err := os.Rename(o.path(readyName(n)), o.path(fmt.Sprintf("quarantine-%d.jsonl", n))); err != nil {
				return sent, fmt.Errorf("analytics outbox: quarantining %s: %w", readyName(n), err)
			}
			if err := syncDir(o.dir); err != nil {
				return sent, err
			}
			if err := o.writeCursor(n+1, 0); err != nil {
				return sent, err
			}
			continue
		}
		if err := o.writeCursor(n+1, 0); err != nil {
			return sent, err
		}
		if err := os.Remove(o.path(readyName(n))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return sent, fmt.Errorf("analytics outbox: %w", err)
		}
		if err := syncDir(o.dir); err != nil {
			return sent, err
		}
	}
	return sent, nil
}

// analyticsExpireBatch bounds how many index entries one lock hold
// removes, so appenders (lock wait 2 s) never starve behind expiry.
const analyticsExpireBatch = 500

// ExpireIndex removes occurrence index entries older than the index TTL,
// then the oldest entries beyond the entry cap. It lists the index without
// the lock and removes candidates in short locked batches, re-checking
// each entry's age under the lock (an appender may have just updated it).
func (o *AnalyticsOutbox) ExpireIndex() (removed int, err error) {
	entries, err := os.ReadDir(o.indexDir())
	if err != nil {
		return 0, fmt.Errorf("analytics outbox: %w", err)
	}
	cutoff := o.now().Add(-o.indexTTL)
	var expired, keep []indexAge
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			// Remove if still older than the cutoff under the lock.
			expired = append(expired, indexAge{e.Name(), cutoff})
		} else {
			keep = append(keep, indexAge{e.Name(), info.ModTime()})
		}
	}
	if excess := len(keep) - o.indexMax; excess > 0 {
		sort.Slice(keep, func(i, j int) bool { return keep[i].mod.Before(keep[j].mod) })
		for _, a := range keep[:excess] {
			// Evict only if unchanged since listing.
			expired = append(expired, indexAge{a.name, a.mod.Add(time.Nanosecond)})
		}
	}
	batch := o.expireBatch
	if batch <= 0 {
		batch = analyticsExpireBatch
	}
	for start := 0; start < len(expired); start += batch {
		end := start + batch
		if end > len(expired) {
			end = len(expired)
		}
		if o.beforeExpireBatch != nil {
			o.beforeExpireBatch()
		}
		n, err := o.removeIndexBatch(expired[start:end])
		removed += n
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// indexAge is an index entry name with the modification-time limit below
// which it may be removed.
type indexAge struct {
	name string
	mod  time.Time
}

// removeIndexBatch removes the named entries under one lock hold, each only
// if its current mtime is still before the recorded limit.
func (o *AnalyticsOutbox) removeIndexBatch(batch []indexAge) (int, error) {
	unlock, err := o.lock()
	if err != nil {
		return 0, err
	}
	defer unlock()
	removed := 0
	for _, a := range batch {
		p := filepath.Join(o.indexDir(), a.name)
		info, err := os.Stat(p)
		if err != nil || !info.ModTime().Before(a.mod) {
			continue
		}
		if os.Remove(p) == nil {
			removed++
		}
	}
	if removed > 0 {
		return removed, o.fsyncDir(o.indexDir())
	}
	return 0, nil
}

// OutboxLedger is a typed, read-only snapshot of the outbox's on-disk
// ledger: everything shared by all producer processes and kept across
// restarts. It is the only supported way for other code (WP3 diagnostics)
// to read the ledger; the file names stay private here.
type OutboxLedger struct {
	Dir         string
	Epoch       string // never "" for an opened outbox
	LastSeq     int64  // last allocated producer_seq
	PinnedHubID string // "" until pinned

	OverflowTotal, OversizeTotal, AppendFailedTotal, DeadLettersTotal int64
	SyncUncertainTotal, CommitDeferredTotal                           int64

	Held          bool  // an unresolved append intent is on disk
	PendingBytes  int64 // undelivered: staging, finalizing, ready past the cursor
	RetainedBytes int64 // PendingBytes (same read) + dead-letter + quarantine files
	// OldestPending is the oldest pending file's modification time; zero
	// when nothing is pending.
	OldestPending time.Time
}

// LedgerSnapshot reads every ledger value once under the outbox lock, so
// the values are mutually consistent (no in-flight append between them).
// A counter file that does not exist yet reads as 0; any other read or
// parse error, or ErrAnalyticsOutboxLocked, is returned.
func (o *AnalyticsOutbox) LedgerSnapshot() (OutboxLedger, error) {
	unlock, err := o.lock()
	if err != nil {
		return OutboxLedger{Dir: o.dir}, err
	}
	defer unlock()
	return o.ledgerLocked()
}

// ledgerLocked reads the ledger; the caller holds the lock.
func (o *AnalyticsOutbox) ledgerLocked() (OutboxLedger, error) {
	d := OutboxLedger{Dir: o.dir}
	var err error
	if d.Epoch, err = o.readTrimmed("epoch"); err != nil {
		return d, err
	}
	if d.Epoch == "" {
		return d, errors.New("analytics outbox: epoch missing")
	}
	if d.PinnedHubID, err = o.readTrimmed("hub_id"); err != nil {
		return d, err
	}
	for name, dst := range map[string]*int64{
		"seq": &d.LastSeq, "overflow": &d.OverflowTotal, "oversize": &d.OversizeTotal, "dead_letters": &d.DeadLettersTotal,
	} {
		if *dst, err = o.readInt(name); err != nil {
			return d, err
		}
	}
	for name, dst := range map[string]*int64{
		"append_failed": &d.AppendFailedTotal, "sync_uncertain": &d.SyncUncertainTotal, "commit_deferred": &d.CommitDeferredTotal,
	} {
		if *dst, err = o.byteCounter(name); err != nil {
			return d, err
		}
	}
	if _, err := os.Stat(o.path("intent")); err == nil {
		d.Held = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return d, fmt.Errorf("analytics outbox: %w", err)
	}
	if d.PendingBytes, d.OldestPending, err = o.pendingBytes(); err != nil {
		return d, err
	}
	kept, err := o.keptBytes()
	if err != nil {
		return d, err
	}
	d.RetainedBytes = d.PendingBytes + kept
	return d, nil
}

// byteCounter reads a one-byte-per-event counter file (its size).
func (o *AnalyticsOutbox) byteCounter(name string) (int64, error) {
	info, err := os.Stat(o.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("analytics outbox: %w", err)
	}
	return info.Size(), nil
}

// MemoryStats returns only this process's in-memory counters (no disk
// access); the Persisted*/disk-derived fields are left zero. Use
// LedgerSnapshot for the ledger.
func (o *AnalyticsOutbox) MemoryStats() AnalyticsOutboxStats {
	return AnalyticsOutboxStats{
		Appended:          o.appended.Load(),
		CertainDuplicates: o.certainDup.Load(),
		Overflow:          o.overflow.Load(),
		AppendFailed:      o.appendFailed.Load(),
		Finalized:         o.finalized.Load(),
		Delivered:         o.delivered.Load(),
		SendFailures:      o.sendFailures.Load(),
		Torn:              o.torn.Load(),
		HubIDMismatch:     o.hubIDMismatch.Load(),
		Conflicts:         o.conflicts.Load(),
		Quarantined:       o.quarantined.Load(),
		DeadLetters:       o.deadLetters.Load(),
		SyncUncertain:     o.syncUncertain.Load(),
		CorruptIntents:    o.corruptIntents.Load(),
		Unresolved:        o.unresolved.Load(),
		CommitDeferred:    o.commitDeferred.Load(),
	}
}

// Stats returns MemoryStats plus the disk-derived fields from one
// LedgerSnapshot (best effort: a snapshot error leaves them zero).
func (o *AnalyticsOutbox) Stats() AnalyticsOutboxStats {
	s := o.MemoryStats()
	d, err := o.LedgerSnapshot()
	if err != nil {
		return s
	}
	s.HeldIntent = d.Held
	s.PersistedOverflow = d.OverflowTotal
	s.PersistedOversize = d.OversizeTotal
	s.PersistedAppendFailed = d.AppendFailedTotal
	s.PersistedSyncUncertain = d.SyncUncertainTotal
	s.PersistedCommitDeferred = d.CommitDeferredTotal
	s.PersistedDeadLetters = d.DeadLettersTotal
	s.PendingBytes = d.PendingBytes
	s.RetainedBytes = d.RetainedBytes
	if !d.OldestPending.IsZero() {
		s.OldestPendingAge = o.now().Sub(d.OldestPending)
	}
	return s
}

// pendingBytes sums undelivered bytes (staging plus ready files past the
// cursor) and reports the oldest pending file's modification time. It reads
// only file metadata and is safe without the lock; AppendUsage calls it
// under the lock for an exact cap check.
func (o *AnalyticsOutbox) pendingBytes() (int64, time.Time, error) {
	var total int64
	var oldest time.Time
	note := func(info os.FileInfo) {
		if oldest.IsZero() || info.ModTime().Before(oldest) {
			oldest = info.ModTime()
		}
	}
	if info, err := os.Stat(o.path("staging.jsonl")); err == nil && info.Size() > 0 {
		total += info.Size()
		note(info)
	}
	finalizing, err := o.finalizingFiles()
	if err != nil {
		return 0, time.Time{}, err
	}
	for _, name := range finalizing {
		if info, err := os.Stat(o.path(name)); err == nil {
			total += info.Size()
			note(info)
		}
	}
	files, err := o.readyFiles()
	if err != nil {
		return 0, time.Time{}, err
	}
	curFile, curOffset, err := o.readCursor()
	if err != nil {
		return 0, time.Time{}, err
	}
	for _, n := range files {
		if n < curFile {
			continue
		}
		info, err := os.Stat(o.path(readyName(n)))
		if err != nil {
			continue
		}
		size := info.Size()
		if n == curFile {
			size -= curOffset
		}
		if size > 0 {
			total += size
			note(info)
		}
	}
	return total, oldest, nil
}

func (o *AnalyticsOutbox) readTrimmed(name string) (string, error) {
	b, err := os.ReadFile(o.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("analytics outbox: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

func (o *AnalyticsOutbox) readInt(name string) (int64, error) {
	s, err := o.readTrimmed(name)
	if err != nil || s == "" {
		return 0, err
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("analytics outbox: malformed %s %q", name, s)
	}
	return n, nil
}

// indexEntry is one persisted occurrence record: the number of accepted
// observations and, for verified request IDs, the distinct semantic
// fingerprints seen. File format: the count on the first line, then one
// fingerprint per line.
type indexEntry struct {
	count        int64
	fingerprints []string
}

func (e indexEntry) has(fp string) bool {
	for _, f := range e.fingerprints {
		if f == fp {
			return true
		}
	}
	return false
}

func (o *AnalyticsOutbox) readIndex(name string) (indexEntry, error) {
	b, err := os.ReadFile(filepath.Join(o.indexDir(), name))
	if errors.Is(err, os.ErrNotExist) {
		return indexEntry{}, nil
	}
	if err != nil {
		return indexEntry{}, fmt.Errorf("analytics outbox: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	n, err := strconv.ParseInt(strings.TrimSpace(lines[0]), 10, 64)
	if err != nil || n < 1 {
		// Unreadable entry: treat the identity as seen once with unknown
		// content, so the next observation is an ambiguous repeat (or a
		// conflict) rather than a silent first sighting or duplicate.
		return indexEntry{count: 1}, nil
	}
	return indexEntry{count: n, fingerprints: lines[1:]}, nil
}

func (o *AnalyticsOutbox) writeIndex(name string, e indexEntry) error {
	var b strings.Builder
	b.WriteString(strconv.FormatInt(e.count, 10))
	for _, fp := range e.fingerprints {
		b.WriteString("\n" + fp)
	}
	return o.writeFileIn(o.indexDir(), name, []byte(b.String()))
}

func (o *AnalyticsOutbox) writeFileAtomic(name string, data []byte) error {
	return o.writeFileIn(o.dir, name, data)
}

// writeFileIn writes dir/name via a synced, owner-chowned temporary file,
// rename and directory sync.
func (o *AnalyticsOutbox) writeFileIn(dir, name string, data []byte) error {
	return o.writeFileFrom(dir, name, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// writeFileFrom is writeFileIn with streamed content.
func (o *AnalyticsOutbox) writeFileFrom(dir, name string, fill func(io.Writer) error) error {
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return fmt.Errorf("analytics outbox: %w", err)
	}
	tmpName := tmp.Name()
	err = fill(tmp)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = o.own(tmpName)
	}
	if err == nil {
		err = os.Rename(tmpName, filepath.Join(dir, name))
	}
	if err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("analytics outbox: writing %s: %w", name, err)
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("analytics outbox: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("analytics outbox: syncing %s: %w", dir, err)
	}
	return nil
}
