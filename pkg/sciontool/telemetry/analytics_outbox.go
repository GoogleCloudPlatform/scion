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
//	ready-N.jsonl   final bytes written by Finalize; never modified
//	cursor          "N offset" of the next ready line to send
//	index/<hash>    persisted occurrence count for one source identity
//
//	intent          the in-flight append (see below)
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
	analyticsLockPoll         = 10 * time.Millisecond
	analyticsMaxLineBytes     = 1 << 20
)

// AnalyticsOutboxDirName is the outbox directory under the agent home's
// .scion directory.
const AnalyticsOutboxDirName = "analytics-outbox"

// DefaultAnalyticsOutboxDir returns $HOME/.scion/analytics-outbox, or "" if
// the home directory is unknown.
func DefaultAnalyticsOutboxDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".scion", AnalyticsOutboxDirName)
}

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
)

// ErrAnalyticsOutboxLocked is returned when the outbox lock could not be
// taken within analyticsLockWait.
var ErrAnalyticsOutboxLocked = errors.New("analytics outbox: lock wait timed out")

// AnalyticsOutboxStats are the outbox's fixed-cardinality counters, for WP3
// diagnostics. Counters other than PendingBytes, OldestPendingAge and
// PersistedOverflow are per process.
type AnalyticsOutboxStats struct {
	Appended, CertainDuplicates, Overflow, AppendFailed int64
	Finalized, Delivered, SendFailures, Torn            int64
	HubIDMismatch, Conflicts, Quarantined               int64
	PersistedOverflow                                   int64
	PendingBytes                                        int64
	OldestPendingAge                                    time.Duration
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

	// openFile opens a ready file for Drain (test seam for read faults).
	openFile func(name string) (readyFile, error)
	// crashAt, when set, makes appendUsage stop right after the named step
	// ("intent" or "line") without cleaning up, simulating a crash (tests).
	crashAt string
}

// readyFile is what Drain reads a ready file through.
type readyFile interface {
	io.ReadSeekCloser
}

// errSimulatedCrash is returned by appendUsage at a crashAt step (tests).
var errSimulatedCrash = errors.New("analytics outbox: simulated crash")

// OpenAnalyticsOutbox opens (creating if needed) the outbox at dir.
func OpenAnalyticsOutbox(dir string) (*AnalyticsOutbox, error) {
	if dir == "" {
		return nil, errors.New("analytics outbox: empty directory")
	}
	o := &AnalyticsOutbox{
		dir:        dir,
		maxPending: analyticsOutboxMaxPending,
		indexTTL:   analyticsIndexTTL,
		indexMax:   analyticsIndexMaxEntries,
		now:        time.Now,
		openFile:   func(name string) (readyFile, error) { return os.Open(name) },
	}
	for _, d := range []string{dir, o.indexDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("analytics outbox: %w", err)
		}
	}
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return nil, err
	}
	unlock, err := o.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, err := os.Stat(o.path("epoch")); errors.Is(err, os.ErrNotExist) {
		if err := o.writeFileAtomic("epoch", []byte(telemetrycontract.NewEventID())); err != nil {
			return nil, err
		}
	}
	if err := o.recoverIntentLocked(); err != nil {
		return nil, err
	}
	return o, nil
}

// Dir returns the outbox directory.
func (o *AnalyticsOutbox) Dir() string { return o.dir }

func (o *AnalyticsOutbox) path(name string) string { return filepath.Join(o.dir, name) }
func (o *AnalyticsOutbox) indexDir() string        { return filepath.Join(o.dir, "index") }

// lock takes the outbox's exclusive flock, waiting up to analyticsLockWait.
func (o *AnalyticsOutbox) lock() (func(), error) {
	f, err := os.OpenFile(o.path("lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("analytics outbox: %w", err)
	}
	deadline := time.Now().Add(analyticsLockWait)
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("analytics outbox: locking: %w", err)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, ErrAnalyticsOutboxLocked
		}
		time.Sleep(analyticsLockPoll)
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
	case err != nil:
		o.appendFailed.Add(1)
	case outcome == AppendWritten:
		o.appended.Add(1)
	case outcome == AppendCertainDuplicate:
		o.certainDup.Add(1)
	case outcome == AppendOverflow:
		o.overflow.Add(1)
	}
	return outcome, err
}

func (o *AnalyticsOutbox) appendUsage(ev *telemetrycontract.UsageEvent) (AppendOutcome, error) {
	unlock, err := o.lock()
	if err != nil {
		return 0, err
	}
	defer unlock()
	if err := o.recoverIntentLocked(); err != nil {
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
	if len(line)+1 > analyticsMaxLineBytes {
		return 0, fmt.Errorf("analytics outbox: event of %d bytes exceeds the line limit", len(line))
	}

	pending, _, err := o.pendingBytes()
	if err != nil {
		return 0, err
	}
	if pending+int64(len(line))+1 > o.maxPending {
		// Consume the seq so the refusal is a visible gap downstream; the
		// index is untouched (the observation was not accepted).
		if err := o.writeFileAtomic("seq", []byte(strconv.FormatInt(seq, 10))); err != nil {
			return 0, err
		}
		n, _ := o.readInt("overflow")
		_ = o.writeFileAtomic("overflow", []byte(strconv.FormatInt(n+1, 10)))
		return AppendOverflow, nil
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
	if err := o.appendLine("staging.jsonl", line); err != nil {
		// Not durable: drop the intent so a retry is a first sighting.
		_ = os.Remove(o.path("intent"))
		return 0, err
	}
	if o.crashAt == "line" {
		return 0, errSimulatedCrash
	}
	if err := o.commitIntent(in); err != nil {
		return 0, err
	}
	return AppendWritten, nil
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
	b, err := os.ReadFile(o.path("intent"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("analytics outbox: %w", err)
	}
	var in appendIntent
	if json.Unmarshal(b, &in) != nil || in.EventID == "" {
		// A torn intent is impossible with atomic rename; treat as absent.
		return os.Remove(o.path("intent"))
	}
	last, err := o.lastStagingEventID()
	if err != nil {
		return err
	}
	if last == in.EventID {
		return o.commitIntent(in)
	}
	if err := os.Remove(o.path("intent")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("analytics outbox: %w", err)
	}
	return syncDir(o.dir)
}

// lastStagingEventID returns the event_id of the staging file's last
// complete (newline-terminated) line, or "" if there is none or the file
// ends in a torn fragment.
func (o *AnalyticsOutbox) lastStagingEventID() (string, error) {
	f, err := os.Open(o.path("staging.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("analytics outbox: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("analytics outbox: %w", err)
	}
	size := info.Size()
	if size == 0 {
		return "", nil
	}
	start := size - analyticsMaxLineBytes - 1
	if start < 0 {
		start = 0
	}
	tail := make([]byte, size-start)
	if _, err := f.ReadAt(tail, start); err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("analytics outbox: %w", err)
	}
	if tail[len(tail)-1] != '\n' {
		return "", nil
	}
	body := tail[:len(tail)-1]
	if i := bytes.LastIndexByte(body, '\n'); i >= 0 {
		body = body[i+1:]
	}
	var head struct {
		EventID string `json:"event_id"`
	}
	if json.Unmarshal(body, &head) != nil {
		return "", nil
	}
	return head.EventID, nil
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
		fmt.Fprintf(h, "%d:", len(p))
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// appendLine durably appends line plus a newline to name. If the file does
// not end in a newline (a torn line left by a crashed writer), a newline is
// written first so the torn fragment stays a separate, undecodable line.
func (o *AnalyticsOutbox) appendLine(name string, line []byte) error {
	p := o.path(name)
	_, statErr := os.Stat(p)
	created := errors.Is(statErr, os.ErrNotExist)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("analytics outbox: %w", err)
	}
	defer f.Close()
	buf := make([]byte, 0, len(line)+2)
	if info, err := f.Stat(); err == nil && info.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, info.Size()-1); err == nil && last[0] != '\n' {
			buf = append(buf, '\n')
		}
	}
	buf = append(buf, line...)
	buf = append(buf, '\n')
	if _, err := f.Write(buf); err != nil {
		return fmt.Errorf("analytics outbox: writing %s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("analytics outbox: syncing %s: %w", name, err)
	}
	if created {
		return syncDir(o.dir)
	}
	return nil
}

// PinHubID records hubID as the outbox's hub on first call; later calls
// with a different value keep the pinned one and report a mismatch.
func (o *AnalyticsOutbox) PinHubID(hubID string) (pinned string, err error) {
	if hubID == "" {
		return "", errors.New("analytics outbox: empty hub ID")
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

// PinnedHubID returns the pinned hub ID, or "".
func (o *AnalyticsOutbox) PinnedHubID() string {
	id, _ := o.readTrimmed("hub_id")
	return id
}

// Finalize moves staging lines to a new ready file as final bytes: lines
// that carry hub_id are copied unchanged; lines without it get the pinned
// hub ID and are re-marshaled. It does nothing until a hub ID is pinned.
// Undecodable (torn) lines are dropped and counted. A crash after the
// ready file is synced but before staging is truncated repeats the copy;
// the duplicate lines are byte-identical, which is neutral downstream.
func (o *AnalyticsOutbox) Finalize() (int, error) {
	unlock, err := o.lock()
	if err != nil {
		return 0, err
	}
	defer unlock()
	if err := o.recoverIntentLocked(); err != nil {
		return 0, err
	}
	hubID, err := o.readTrimmed("hub_id")
	if err != nil || hubID == "" {
		return 0, err
	}
	data, err := os.ReadFile(o.path("staging.jsonl"))
	if errors.Is(err, os.ErrNotExist) || len(data) == 0 {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("analytics outbox: %w", err)
	}
	var out bytes.Buffer
	n := 0
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var ev telemetrycontract.UsageEvent
		if err := telemetrycontract.Unmarshal(line, &ev); err != nil {
			o.torn.Add(1)
			continue
		}
		final := line
		if ev.HubID == "" {
			ev.HubID = hubID
			if final, err = telemetrycontract.Marshal(&ev); err != nil {
				o.torn.Add(1)
				continue
			}
		}
		out.Write(final)
		out.WriteByte('\n')
		n++
	}
	if n > 0 {
		next, err := o.nextReadyNumber()
		if err != nil {
			return 0, err
		}
		if err := o.writeFileAtomic(readyName(next), out.Bytes()); err != nil {
			return 0, err
		}
	}
	if err := truncateSync(o.path("staging.jsonl")); err != nil {
		return 0, err
	}
	o.finalized.Add(int64(n))
	return n, nil
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
// destination accepted it.
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
						_ = f.Close()
						return sent, err
					}
					sent++
					o.delivered.Add(1)
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

// ExpireIndex removes occurrence index entries older than the index TTL,
// then the oldest entries beyond the entry cap.
func (o *AnalyticsOutbox) ExpireIndex() (removed int, err error) {
	unlock, err := o.lock()
	if err != nil {
		return 0, err
	}
	defer unlock()
	entries, err := os.ReadDir(o.indexDir())
	if err != nil {
		return 0, fmt.Errorf("analytics outbox: %w", err)
	}
	type aged struct {
		name string
		mod  time.Time
	}
	cutoff := o.now().Add(-o.indexTTL)
	var keep []aged
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if os.Remove(filepath.Join(o.indexDir(), e.Name())) == nil {
				removed++
			}
			continue
		}
		keep = append(keep, aged{e.Name(), info.ModTime()})
	}
	if excess := len(keep) - o.indexMax; excess > 0 {
		sort.Slice(keep, func(i, j int) bool { return keep[i].mod.Before(keep[j].mod) })
		for _, a := range keep[:excess] {
			if os.Remove(filepath.Join(o.indexDir(), a.name)) == nil {
				removed++
			}
		}
	}
	if removed > 0 {
		return removed, syncDir(o.indexDir())
	}
	return removed, nil
}

// Stats returns the outbox counters. Pending figures are read from disk.
func (o *AnalyticsOutbox) Stats() AnalyticsOutboxStats {
	s := AnalyticsOutboxStats{
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
	}
	s.PersistedOverflow, _ = o.readInt("overflow")
	pending, oldest, err := o.pendingBytes()
	if err == nil {
		s.PendingBytes = pending
		if !oldest.IsZero() {
			s.OldestPendingAge = o.now().Sub(oldest)
		}
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
	return writeFileAtomic(o.indexDir(), name, []byte(b.String()))
}

func (o *AnalyticsOutbox) writeFileAtomic(name string, data []byte) error {
	return writeFileAtomic(o.dir, name, data)
}

// writeFileAtomic writes dir/name via a synced temporary file, rename and
// directory sync.
func writeFileAtomic(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return fmt.Errorf("analytics outbox: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
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

func truncateSync(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("analytics outbox: %w", err)
	}
	defer f.Close()
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("analytics outbox: %w", err)
	}
	return f.Sync()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("analytics outbox: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("analytics outbox: syncing %s: %w", dir, err)
	}
	return nil
}
