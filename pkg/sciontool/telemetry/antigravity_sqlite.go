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

package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/trajectory/agydecode"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

const (
	// AgySQLiteStateFileName is the persisted cursor + union-find dedupe state
	// stored under <agent-home>/.scion/.
	AgySQLiteStateFileName = "agy-sqlite-state.json"

	// AgySQLiteWakeSocketName is the Unix domain datagram socket under
	// <agent-home>/.scion/ used by short-lived `sciontool hook` processes to
	// wake the live tailer in `sciontool init` immediately on PostInvocation.
	AgySQLiteWakeSocketName = "agy-sqlite-wake.sock"

	defaultAgySQLiteDebounce       = 500 * time.Millisecond
	defaultAgySQLitePollInterval   = 15 * time.Second
	agySQLiteStateSchemaVersion    = 1
	agyVersionDetectTimeout        = 2 * time.Second
	defaultAgyVersionMaxFailures   = 3
	defaultAgyVersionFailureWindow = 5 * time.Minute
)

// AgySQLiteStatePath returns <agentHome>/.scion/agy-sqlite-state.json, or ""
// when agentHome is empty.
func AgySQLiteStatePath(agentHome string) string {
	if agentHome == "" {
		return ""
	}
	return filepath.Join(agentHome, ".scion", AgySQLiteStateFileName)
}

// AgySQLiteWakePath returns <agentHome>/.scion/agy-sqlite-wake.sock, or ""
// when agentHome is empty.
func AgySQLiteWakePath(agentHome string) string {
	if agentHome == "" {
		return ""
	}
	return filepath.Join(agentHome, ".scion", AgySQLiteWakeSocketName)
}

// AgyConversationsDir returns <agentHome>/.gemini/antigravity-cli/conversations,
// or "" when agentHome is empty.
func AgyConversationsDir(agentHome string) string {
	if agentHome == "" {
		return ""
	}
	return filepath.Join(agentHome, ".gemini", "antigravity-cli", "conversations")
}

// AntigravitySQLiteConfig configures the live Track 1 Antigravity SQLite tailer.
//
// Calls ownership (single-source guard):
// PostInvocation hooks continue to own call counts (calls=1, token_status=not_reported),
// and this tailer emits token increments with calls=0. Keeping calls on PostInvocation
// ensures immediate call visibility, captures failed invocations, avoids cross-process
// handoff races when schema drift is detected mid-run, and guarantees seamless
// degradation to PostInvocation calls_only on drift.
type AntigravitySQLiteConfig struct {
	// AgentHome is the agent's home directory (used to derive default
	// ConversationsDir, StatePath, and WakePath).
	AgentHome string
	// ConversationsDir overrides <AgentHome>/.gemini/antigravity-cli/conversations.
	ConversationsDir string
	// StatePath overrides <AgentHome>/.scion/agy-sqlite-state.json.
	StatePath string
	// WakePath overrides <AgentHome>/.scion/agy-sqlite-wake.sock.
	// Set DisableWakeSocket to skip binding a Unix socket (e.g. unit tests
	// that drive PollOnce or TriggerWake directly).
	WakePath          string
	DisableWakeSocket bool
	// WakeOwner, when set and running as root, receives ownership of the wake
	// socket so agent-user `sciontool hook` processes can signal it.
	WakeOwner *OutboxOwner

	// Analytics receives canonical agent.usage events (calls=0 for tokens;
	// drift sentinels with nil tokens on unverified/malformed decodes).
	Analytics *UsageAnalytics
	// SessionUsage, when set, receives decoded token counts (calls=0) so
	// session-level metrics stay in sync with the live tailer.
	SessionUsage SessionUsageSink

	// Debounce coalesces rapid PostInvocation wakes (default 500ms).
	Debounce time.Duration
	// PollInterval is the fallback periodic poll interval (default 15s).
	PollInterval time.Duration
	// MaxDedupeRoots caps retained canonical roots in DedupeState via LRU
	// pruning (default agydecode.DefaultMaxDedupeRoots).
	MaxDedupeRoots int
	// BusyTimeoutMs overrides the SQLite read-only busy_timeout pragma.
	BusyTimeoutMs int
	// RunID scopes persisted schema-drift latching to a single agent run.
	// Defaults to SCION_LAUNCH_ID when empty. A new run with a different RunID
	// clears any persisted drift latch from an earlier run. When RunID is empty
	// (e.g. a local workstation run without a Hub-minted run ID), a persisted
	// drift latch stays active across restarts until either a non-empty RunID
	// is supplied or agy-sqlite-state.json is removed.
	RunID string
	// MaxVersionFailures is the minimum number of consecutive agy
	// version-detection failures (together with MinVersionFailureWindow)
	// before latching unverified schema drift and degrading supported_signals
	// to ["calls"] so the Hub never sees "measured" with zero tokens
	// (default defaultAgyVersionMaxFailures = 3).
	MaxVersionFailures int
	// MinVersionFailureWindow is the minimum elapsed duration from the first
	// consecutive agy version-detection failure before latching drift
	// (default defaultAgyVersionFailureWindow = 5m).
	MinVersionFailureWindow time.Duration

	// DetectVersion returns the installed agy binary version string (or an
	// error on transient detection failure, which is retried on subsequent
	// polls without advancing cursors or latching drift). When nil, defaults
	// to resolving `agy` via rootexec.Resolve and running `agy --version`
	// through procreap.OutputManaged.
	DetectVersion func(ctx context.Context) (string, error)
	// Now overrides time.Now for tests.
	Now func() time.Time
}

type agySQLitePersistedState struct {
	Version             int                           `json:"version"`
	Files               map[string]agySQLiteFileState `json:"files,omitempty"`
	Dedupe              *agydecode.DedupeState        `json:"dedupe,omitempty"`
	SchemaDriftDetected bool                          `json:"schema_drift_detected,omitempty"`
	DriftReason         string                        `json:"drift_reason,omitempty"`
	DriftRunID          string                        `json:"drift_run_id,omitempty"`
}

type agySQLiteFileState struct {
	Fingerprint     agydecode.FileFingerprint `json:"fingerprint"`
	Cursor          agydecode.Cursor          `json:"cursor"`
	BaselinePending bool                      `json:"baseline_pending,omitempty"`
}

// AntigravitySQLiteTailer tails ~/.gemini/antigravity-cli/conversations/*.db
// read-only using pkg/trajectory/agydecode and appends canonical token
// increments (calls=0) to the Track 1 analytics outbox.
type AntigravitySQLiteTailer struct {
	cfg AntigravitySQLiteConfig

	mu                    sync.Mutex
	state                 agySQLitePersistedState
	stateInitialized      bool
	versionResolved       bool
	versionFailures       int
	versionFirstFailureAt time.Time
	agyVersion            string
	driftEmitted          map[string]bool

	driftLatched atomic.Bool

	wakeCh     chan struct{}
	wakeCloser io.Closer
	cancel     context.CancelFunc
	done       chan struct{}
	started    bool
	stopped    bool
}

// NewAntigravitySQLiteTailer constructs a live Antigravity SQLite tailer and
// loads any persisted state from cfg.StatePath (or <AgentHome>/.scion/agy-sqlite-state.json).
func NewAntigravitySQLiteTailer(cfg AntigravitySQLiteConfig) *AntigravitySQLiteTailer {
	if cfg.ConversationsDir == "" {
		cfg.ConversationsDir = AgyConversationsDir(cfg.AgentHome)
	}
	if cfg.StatePath == "" {
		cfg.StatePath = AgySQLiteStatePath(cfg.AgentHome)
	}
	if cfg.WakePath == "" {
		cfg.WakePath = AgySQLiteWakePath(cfg.AgentHome)
	}
	if cfg.Debounce <= 0 {
		cfg.Debounce = defaultAgySQLiteDebounce
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultAgySQLitePollInterval
	}
	if cfg.MaxDedupeRoots <= 0 {
		cfg.MaxDedupeRoots = agydecode.DefaultMaxDedupeRoots
	}
	if cfg.RunID == "" {
		cfg.RunID = strings.TrimSpace(os.Getenv("SCION_LAUNCH_ID"))
	}
	if cfg.MaxVersionFailures <= 0 {
		cfg.MaxVersionFailures = defaultAgyVersionMaxFailures
	}
	if cfg.MinVersionFailureWindow <= 0 {
		cfg.MinVersionFailureWindow = defaultAgyVersionFailureWindow
	}
	if cfg.DetectVersion == nil {
		cfg.DetectVersion = detectAgyVersionManaged
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	t := &AntigravitySQLiteTailer{
		cfg:          cfg,
		wakeCh:       make(chan struct{}, 1),
		driftEmitted: make(map[string]bool),
	}
	t.loadState()
	return t
}

// SchemaDriftDetected reports whether the tailer has latched an unknown agy
// version or malformed protobuf decode. Once latched, token/quota debits are
// suppressed for the remainder of the run and meter_health reports
// schema_drift_detected=true while PostInvocation continues reporting calls.
func (t *AntigravitySQLiteTailer) SchemaDriftDetected() bool {
	if t == nil {
		return false
	}
	return t.driftLatched.Load()
}

// TriggerWake schedules a debounced poll (safe to call concurrently from any
// goroutine or the Unix wake socket reader).
func (t *AntigravitySQLiteTailer) TriggerWake() {
	if t == nil {
		return
	}
	select {
	case t.wakeCh <- struct{}{}:
	default:
	}
}

// Start starts the background wake listener, 500ms debounce loop, and 15s
// fallback ticker. Calling Start more than once is a no-op.
func (t *AntigravitySQLiteTailer) Start(ctx context.Context) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started || t.stopped {
		return nil
	}

	if !t.stateInitialized && t.cfg.Analytics != nil {
		if err := t.baselineExistingLocked(ctx); err != nil {
			log.Debug("Antigravity SQLite tailer initial baseline failed (will retry on first poll): %v", err)
		}
	}

	if !t.cfg.DisableWakeSocket && t.cfg.WakePath != "" {
		closer, err := startAgySQLiteWakeListener(t.cfg.WakePath, t.cfg.WakeOwner, t.TriggerWake)
		if err != nil {
			log.Debug("Antigravity SQLite tailer wake listener unavailable (falling back to %s ticker): %v", t.cfg.PollInterval, err)
		} else {
			t.wakeCloser = closer
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	t.cancel = cancel
	t.done = make(chan struct{})
	t.started = true

	go t.loop(runCtx)
	return nil
}

// Stop stops the background loop and wake listener, then executes one final
// bounded synchronous PollOnce (graceful-shutdown drain) so any final DB rows
// flushed as agy exited are appended to the outbox before the drainer stops.
func (t *AntigravitySQLiteTailer) Stop(ctx context.Context) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return nil
	}
	t.stopped = true
	cancel := t.cancel
	done := t.done
	closer := t.wakeCloser
	t.wakeCloser = nil
	t.mu.Unlock()

	if closer != nil {
		_ = closer.Close()
	}
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return t.PollOnce(ctx)
}

func (t *AntigravitySQLiteTailer) loop(ctx context.Context) {
	defer close(t.done)

	ticker := time.NewTicker(t.cfg.PollInterval)
	defer ticker.Stop()

	var debounceTimer *time.Timer
	var debounceC <-chan time.Time
	stopDebounce := func() {
		if debounceTimer == nil {
			return
		}
		if !debounceTimer.Stop() {
			select {
			case <-debounceTimer.C:
			default:
			}
		}
		debounceC = nil
	}
	defer stopDebounce()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.wakeCh:
			if debounceTimer == nil {
				debounceTimer = time.NewTimer(t.cfg.Debounce)
			} else {
				if !debounceTimer.Stop() {
					select {
					case <-debounceTimer.C:
					default:
					}
				}
				debounceTimer.Reset(t.cfg.Debounce)
			}
			debounceC = debounceTimer.C
		case <-debounceC:
			debounceC = nil
			if err := t.PollOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Debug("Antigravity SQLite tailer wake poll error: %v", err)
			}
		case <-ticker.C:
			if err := t.PollOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Debug("Antigravity SQLite tailer periodic poll error: %v", err)
			}
		}
	}
}

// PollOnce performs a single synchronous scan of ConversationsDir, skipping
// unchanged files by (db_mtime, db_size, wal_mtime, wal_size) fingerprint,
// decoding modified files against a cloned DedupeState, appending canonical
// usage increments to Analytics, and atomically persisting state only after
// appends succeed.
func (t *AntigravitySQLiteTailer) PollOnce(ctx context.Context) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	// Do not consume rows or advance persisted cursors when no analytics sink
	// is configured (O2).
	if t.cfg.Analytics == nil || t.cfg.ConversationsDir == "" {
		return nil
	}
	entries, err := os.ReadDir(t.cfg.ConversationsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read antigravity conversations dir: %w", err)
	}

	var dbNames []string
	present := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".db") {
			continue
		}
		dbNames = append(dbNames, name)
		present[name] = true
	}
	sort.Strings(dbNames)

	if t.state.Files == nil {
		t.state.Files = make(map[string]agySQLiteFileState)
	}
	if t.state.Dedupe == nil {
		t.state.Dedupe = agydecode.NewDedupeState()
	}

	// On first start with no persisted state file, baseline every existing DB
	// at its current end (fingerprint + max rowids) without emitting so
	// pre-existing historical sessions are never charged to the current run.
	if !t.stateInitialized {
		return t.baselineFilesLocked(ctx, dbNames)
	}

	dirty := false
	// Prune file entries for DBs removed from disk.
	for name := range t.state.Files {
		if !present[name] {
			delete(t.state.Files, name)
			dirty = true
		}
	}

	var firstErr error
	versionCheckedThisPoll := false
	var pollAgyVer string
	var pollVerErr error
	for _, name := range dbNames {
		if err := ctx.Err(); err != nil {
			return err
		}
		dbPath := filepath.Join(t.cfg.ConversationsDir, name)
		if err := verifyRegularAgyDB(dbPath); err != nil {
			log.Warn("Antigravity SQLite tailer skipping non-regular conversation file %s: %v", name, err)
			continue
		}
		fp, err := agydecode.Fingerprint(dbPath)
		if err != nil {
			continue
		}
		prevFile, hasPrev := t.state.Files[name]
		if hasPrev && prevFile.BaselinePending {
			// A DB that failed initial baselining must never be decoded from
			// rowid 0 (which would charge historical rows). Retry baselining
			// until it succeeds or disappears.
			baseFP, baseCursor, baseErr := agydecode.BaselineFile(ctx, dbPath, t.cfg.BusyTimeoutMs)
			if baseErr != nil {
				log.Warn("Antigravity SQLite tailer retry baseline error for %s (remains baseline_pending): %v", name, baseErr)
				if firstErr == nil {
					firstErr = baseErr
				}
				continue
			}
			t.state.Files[name] = agySQLiteFileState{
				Fingerprint: baseFP,
				Cursor:      baseCursor,
			}
			dirty = true
			continue
		}
		if hasPrev && prevFile.Fingerprint.Equal(fp) {
			continue
		}

		// Once drift is latched for this run, suppress all token debits and
		// avoid re-running `agy --version` or decoding rows on subsequent polls,
		// while advancing the rowid cursor via BaselineFile so rows written
		// during a latched run are never retroactively billed to the next run.
		if t.driftLatched.Load() {
			baseFP, baseCursor, baseErr := agydecode.BaselineFile(ctx, dbPath, t.cfg.BusyTimeoutMs)
			if baseErr != nil {
				if firstErr == nil {
					firstErr = baseErr
				}
				continue
			}
			t.state.Files[name] = agySQLiteFileState{
				Fingerprint: baseFP,
				Cursor:      baseCursor,
			}
			dirty = true
			continue
		}

		// Detect agy version at most once per poll (not per file) so a hanging
		// or missing binary never burns N * 2s across N DB files.
		if !versionCheckedThisPoll {
			versionCheckedThisPoll = true
			pollAgyVer, pollVerErr = t.resolveVersionLocked(ctx)
			if pollVerErr != nil {
				now := t.cfg.Now()
				if t.versionFailures == 0 {
					t.versionFirstFailureAt = now
				}
				t.versionFailures++
				elapsed := now.Sub(t.versionFirstFailureAt)
				if t.versionFailures >= t.cfg.MaxVersionFailures && elapsed >= t.cfg.MinVersionFailureWindow {
					// Persistent detection failure across both count and time
					// thresholds: degrade honestly to unverified drift
					// (calls_only) so the Hub never sees "measured" with zero tokens.
					reason := fmt.Sprintf("agy version detection failed %d consecutive times over %s: %v",
						t.versionFailures, elapsed.Round(time.Second), pollVerErr)
					if derr := t.latchVersionDetectDriftLocked(reason); derr != nil && firstErr == nil {
						firstErr = derr
					}
					dirty = true
					if firstErr == nil {
						firstErr = pollVerErr
					}
					break
				}
				// Transient version-detection failure: retry on the next poll
				// without advancing cursors or latching drift.
				log.Debug("Antigravity SQLite tailer version detection failed (%d/%d, elapsed %s/%s, will retry): %v",
					t.versionFailures, t.cfg.MaxVersionFailures, elapsed.Round(time.Second), t.cfg.MinVersionFailureWindow, pollVerErr)
				if firstErr == nil {
					firstErr = pollVerErr
				}
				break
			}
			t.versionFailures = 0
			t.versionFirstFailureAt = time.Time{}
		}
		agyVer := pollAgyVer

		// Decode against a cloned DedupeState and cloned Cursor so an outbox
		// append failure never mutates persisted state or suppresses re-emission
		// on the next poll.
		candDedupe := t.state.Dedupe.Clone()
		candCursor := prevFile.Cursor.Clone()
		res, err := agydecode.DecodeFile(ctx, dbPath, agydecode.Options{
			AgyVersion:    agyVer,
			Cursor:        candCursor,
			Dedupe:        candDedupe,
			BusyTimeoutMs: t.cfg.BusyTimeoutMs,
		})
		if err != nil {
			log.Debug("Antigravity SQLite tailer decode error for %s: %v", name, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		// Drift & quota-debit gate: check Result.Status (not only Record.Status)
		// and latch schema_drift_detected across polls. Malformed rows advance
		// the cursor and appear in only one poll; latching guarantees subsequent
		// polls never debit tokens after drift is detected.
		if res.Status != agydecode.StatusVerified {
			if err := t.handleDriftLocked(res); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			t.state.Files[name] = agySQLiteFileState{
				Fingerprint: res.Fingerprint,
				Cursor:      res.Cursor,
			}
			dirty = true
			continue
		}

		// Verified decode & no drift: emit canonical token increments (calls=0)
		// to the Track 1 outbox. Commit candDedupe and cursor ONLY after every
		// record appends cleanly.
		var sessionDelta SessionUsage
		appendFailed := false
		for _, rec := range res.Records {
			if rec.Status != agydecode.StatusVerified || rec.Tokens == nil {
				continue
			}
			obs := agyRecordToObservation(rec, res.AgyVersion, t.cfg.Now())
			outcome, err := t.cfg.Analytics.RecordUnique(obs)
			if err != nil {
				log.Debug("Antigravity SQLite tailer outbox append failed for %s (%s): %v", name, rec.EmissionKey, err)
				if firstErr == nil {
					firstErr = err
				}
				appendFailed = true
				break
			}
			// Only accumulate session usage on an actual append, not on a
			// replayed AppendCertainDuplicate (O5).
			if outcome == AppendWritten {
				accumulateSessionUsage(&sessionDelta, rec.Tokens)
			}
		}
		if appendFailed {
			// Do not advance cursor or commit candDedupe; next poll will
			// re-decode and re-emit with identical EmissionKeys.
			continue
		}

		t.state.Dedupe = candDedupe
		t.state.Files[name] = agySQLiteFileState{
			Fingerprint: res.Fingerprint,
			Cursor:      res.Cursor,
		}
		dirty = true

		if t.cfg.SessionUsage != nil && !sessionDelta.IsZero() {
			t.cfg.SessionUsage(sessionDelta)
		}
	}

	if dirty {
		t.state.Dedupe.Prune(t.cfg.MaxDedupeRoots)
		if err := t.saveStateLocked(); err != nil {
			log.Debug("Antigravity SQLite tailer state persist failed: %v", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (t *AntigravitySQLiteTailer) handleDriftLocked(res *agydecode.Result) error {
	reason := string(res.Status)
	if res.Status == agydecode.StatusUnverified {
		if res.AgyVersion == "" {
			reason = "unknown agy version"
		} else {
			reason = "unverified agy version " + res.AgyVersion
		}
	} else if len(res.Errors) > 0 {
		reason = fmt.Sprintf("malformed decode (%d errors): %s", len(res.Errors), res.Errors[0].Error)
	}

	if !t.driftLatched.Swap(true) {
		log.Warn("Antigravity SQLite tailer detected schema drift (%s); degrading to PostInvocation calls_only", reason)
	}
	t.state.SchemaDriftDetected = true
	t.state.DriftReason = reason
	t.state.DriftRunID = t.cfg.RunID

	// Emit at most one drift sentinel event per (session, status, version) with
	// calls=0 and nil tokens so quotas are never debited while the Hub records
	// token_status=unverified/malformed and meter_health.schema_drift_detected=true.
	sentinelKey := fmt.Sprintf("%s|%s|%s", res.SessionID, res.Status, res.AgyVersion)
	if t.driftEmitted[sentinelKey] || t.cfg.Analytics == nil {
		return nil
	}

	obs := UsageObservation{
		SourceKind:     "antigravity.sqlite_drift",
		Identity:       telemetrycontract.SourceIdentityComposite,
		SourceEventID:  CompositeSourceEventID("antigravity", "sqlite_drift", res.SessionID, string(res.Status), res.AgyVersion),
		EventTime:      t.cfg.Now(),
		SessionID:      res.SessionID,
		Status:         telemetrycontract.StatusSuccess,
		Calls:          0,
		HarnessVersion: res.AgyVersion,
	}
	if res.Status == agydecode.StatusMalformed {
		obs.TokenStatus = telemetrycontract.TokenStatusMalformed
		obs.MalformedFields = []string{"agy_sqlite_protobuf"}
	} else {
		obs.TokenStatus = telemetrycontract.TokenStatusUnverified
	}
	if _, err := t.cfg.Analytics.RecordUnique(obs); err != nil {
		return err
	}
	t.driftEmitted[sentinelKey] = true
	return nil
}

func (t *AntigravitySQLiteTailer) latchVersionDetectDriftLocked(reason string) error {
	if !t.driftLatched.Swap(true) {
		log.Warn("Antigravity SQLite tailer degrading to PostInvocation calls_only: %s", reason)
	}
	t.state.SchemaDriftDetected = true
	t.state.DriftReason = reason
	t.state.DriftRunID = t.cfg.RunID

	sentinelKey := "version_detect_failed|" + t.cfg.RunID
	if t.driftEmitted[sentinelKey] || t.cfg.Analytics == nil {
		return nil
	}
	obs := UsageObservation{
		SourceKind:    "antigravity.sqlite_drift",
		Identity:      telemetrycontract.SourceIdentityComposite,
		SourceEventID: CompositeSourceEventID("antigravity", "sqlite_drift", "version_detect_failed", t.cfg.RunID),
		EventTime:     t.cfg.Now().UTC().Truncate(24 * time.Hour),
		Status:        telemetrycontract.StatusSuccess,
		Calls:         0,
		TokenStatus:   telemetrycontract.TokenStatusUnverified,
	}
	if _, err := t.cfg.Analytics.RecordUnique(obs); err != nil {
		return err
	}
	t.driftEmitted[sentinelKey] = true
	return nil
}

func agyRecordToObservation(rec agydecode.Record, agyVersion string, now time.Time) UsageObservation {
	// Never feed high-resolution wall-clock time into fingerprinted fields when
	// the DB row lacks a timestamp: truncate to UTC day start so replays carry
	// a deterministic EventTime alongside RecordUnique's index dedupe.
	eventTime := now.UTC().Truncate(24 * time.Hour)
	var sourceTime time.Time
	if rec.Timestamp != nil && !rec.Timestamp.IsZero() {
		eventTime = rec.Timestamp.UTC()
		sourceTime = rec.Timestamp.UTC()
	}

	tok := rec.Tokens
	tokenStatus := telemetrycontract.TokenStatusNotReported
	if tok != nil {
		hasAny := tok.Input != nil || tok.Output != nil || tok.CacheRead != nil || tok.CacheWrite != nil || tok.Reasoning != nil
		if tok.Input != nil && tok.Output != nil {
			tokenStatus = telemetrycontract.TokenStatusReported
		} else if hasAny {
			tokenStatus = telemetrycontract.TokenStatusPartial
		}
	}

	obs := UsageObservation{
		SourceKind:              "antigravity.sqlite_usage",
		Identity:                rec.SourceIdentity,
		SourceEventID:           rec.EmissionKey,
		SourceTime:              sourceTime,
		EventTime:               eventTime,
		Model:                   rec.Model,
		SessionID:               rec.SessionID,
		Status:                  telemetrycontract.StatusSuccess,
		Calls:                   0, // PostInvocation hook owns calls=1
		TokenStatus:             tokenStatus,
		HarnessVersion:          agyVersion,
		ReasoningOverlapsOutput: tok != nil && tok.Reasoning != nil,
	}
	if tok != nil {
		obs.Input = tok.Input
		obs.Output = tok.Output
		obs.CacheRead = tok.CacheRead
		obs.CacheWrite = tok.CacheWrite
		obs.Reasoning = tok.Reasoning
		if tok.Input != nil {
			if tok.InputDerivation != "" {
				obs.InputDerivation = tok.InputDerivation
			} else {
				obs.InputDerivation = telemetrycontract.InputDerivationReported
			}
		}
	}
	return obs
}

func accumulateSessionUsage(dst *SessionUsage, tok *agydecode.TokenUsage) {
	if dst == nil || tok == nil {
		return
	}
	if tok.Input != nil && *tok.Input > 0 {
		dst.TokensInput += *tok.Input
	}
	if tok.Output != nil && *tok.Output > 0 {
		dst.TokensOutput += *tok.Output
	}
	if tok.CacheRead != nil && *tok.CacheRead > 0 {
		dst.TokensCached += *tok.CacheRead
	}
	if tok.Reasoning != nil && *tok.Reasoning > 0 {
		dst.TokensReasoning += *tok.Reasoning
	}
}

func (t *AntigravitySQLiteTailer) resolveVersionLocked(ctx context.Context) (string, error) {
	if t.versionResolved {
		return t.agyVersion, nil
	}
	if t.cfg.DetectVersion == nil {
		return "", errors.New("no agy version detector configured")
	}
	raw, err := t.cfg.DetectVersion(ctx)
	if err != nil {
		return "", err
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("empty agy version output")
	}
	t.agyVersion = agydecode.NormalizeVersion(trimmed)
	t.versionResolved = true
	return t.agyVersion, nil
}

// agyVersionOutput is a test-only seam for capturing the constructed exec.Cmd
// (including its hardened cmd.Env) without modifying production behavior.
var agyVersionOutput = procreap.OutputManaged

func detectAgyVersionManaged(ctx context.Context) (string, error) {
	return detectAgyVersionBinary(ctx, "agy")
}

func detectAgyVersionBinary(ctx context.Context, binaryName string) (string, error) {
	// Resolve against trusted root-owned SearchPath directories so an
	// agent-writable directory earlier on PATH cannot hijack root execution.
	agyPath, err := rootexec.Resolve(binaryName)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", binaryName, err)
	}
	cctx, cancel := context.WithTimeout(ctx, agyVersionDetectTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, agyPath, "--version")
	cmd.Env = rootexec.Env("HOME=/nonexistent")
	out, err := agyVersionOutput(cmd)
	if err != nil {
		return "", fmt.Errorf("run %s --version: %w", agyPath, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func verifyRegularAgyDB(dbPath string) error {
	fi, err := os.Lstat(dbPath)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("mode %s is not regular", fi.Mode())
	}
	for _, suffix := range []string{"-wal", "-journal", "-shm"} {
		compPath := dbPath + suffix
		if cfi, cerr := os.Lstat(compPath); cerr == nil {
			if !cfi.Mode().IsRegular() {
				return fmt.Errorf("%s mode %s is not regular", suffix, cfi.Mode())
			}
		} else if !errors.Is(cerr, os.ErrNotExist) {
			return cerr
		}
	}
	return nil
}

func (t *AntigravitySQLiteTailer) loadState() {
	t.state = agySQLitePersistedState{
		Version: agySQLiteStateSchemaVersion,
		Files:   make(map[string]agySQLiteFileState),
		Dedupe:  agydecode.NewDedupeState(),
	}
	if t.cfg.StatePath == "" {
		t.stateInitialized = true
		return
	}
	stateDir := filepath.Dir(t.cfg.StatePath)
	if err := verifyAgyParentDir(stateDir, t.cfg.WakeOwner); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Warn("Antigravity SQLite tailer refusing to load state from untrusted parent %s: %v", stateDir, err)
		}
		return
	}
	tree, err := privatefs.OpenTree(stateDir, privatefs.TreeOptions{})
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Warn("Antigravity SQLite tailer could not open state dir for %s (will re-baseline): %v", t.cfg.StatePath, err)
		}
		return
	}
	defer func() { _ = tree.Close() }()

	const maxStateFileBytes = 16 << 20
	data, err := tree.ReadPrivate(filepath.Base(t.cfg.StatePath), maxStateFileBytes)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Warn("Antigravity SQLite tailer could not read state file %s (will re-baseline): %v", t.cfg.StatePath, err)
		}
		return
	}
	var loaded agySQLitePersistedState
	if err := json.Unmarshal(data, &loaded); err != nil {
		log.Warn("Antigravity SQLite tailer ignoring corrupt state file %s: %v", t.cfg.StatePath, err)
		return
	}
	if loaded.Files == nil {
		loaded.Files = make(map[string]agySQLiteFileState)
	}
	if loaded.Dedupe == nil {
		loaded.Dedupe = agydecode.NewDedupeState()
	}
	if loaded.Version == 0 {
		loaded.Version = agySQLiteStateSchemaVersion
	}
	if loaded.SchemaDriftDetected {
		// Scope the persisted drift latch to the run that tripped it: keep it
		// latched across restarts within the same run, but clear it when a new
		// run starts (different RunID) so a later verified agy version resumes
		// token metering.
		if t.cfg.RunID != "" && loaded.DriftRunID != t.cfg.RunID {
			loaded.SchemaDriftDetected = false
			loaded.DriftReason = ""
			loaded.DriftRunID = ""
		} else {
			t.driftLatched.Store(true)
		}
	}
	t.state = loaded
	t.stateInitialized = true
}

func (t *AntigravitySQLiteTailer) baselineExistingLocked(ctx context.Context) error {
	if t.cfg.ConversationsDir == "" {
		t.stateInitialized = true
		return nil
	}
	entries, err := os.ReadDir(t.cfg.ConversationsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return t.baselineFilesLocked(ctx, nil)
		}
		return fmt.Errorf("read antigravity conversations dir for baseline: %w", err)
	}
	var dbNames []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".db") {
			dbNames = append(dbNames, entry.Name())
		}
	}
	sort.Strings(dbNames)
	return t.baselineFilesLocked(ctx, dbNames)
}

func (t *AntigravitySQLiteTailer) baselineFilesLocked(ctx context.Context, dbNames []string) error {
	if t.state.Files == nil {
		t.state.Files = make(map[string]agySQLiteFileState)
	}
	if t.state.Dedupe == nil {
		t.state.Dedupe = agydecode.NewDedupeState()
	}
	var firstErr error
	for _, name := range dbNames {
		if err := ctx.Err(); err != nil {
			return err
		}
		dbPath := filepath.Join(t.cfg.ConversationsDir, name)
		if err := verifyRegularAgyDB(dbPath); err != nil {
			log.Warn("Antigravity SQLite tailer skipping non-regular conversation file %s during baseline: %v", name, err)
			continue
		}
		fp, cur, err := agydecode.BaselineFile(ctx, dbPath, t.cfg.BusyTimeoutMs)
		if err != nil {
			// Record a per-file baseline_pending marker and keep going so one
			// corrupt or unreadable DB never blocks metering for the others.
			log.Warn("Antigravity SQLite tailer baseline error for %s (marked baseline_pending): %v", name, err)
			t.state.Files[name] = agySQLiteFileState{
				BaselinePending: true,
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		t.state.Files[name] = agySQLiteFileState{
			Fingerprint: fp,
			Cursor:      cur,
		}
	}
	t.stateInitialized = true
	if err := t.saveStateLocked(); err != nil {
		return err
	}
	return firstErr
}

func (t *AntigravitySQLiteTailer) saveStateLocked() error {
	if t.cfg.StatePath == "" {
		return nil
	}
	t.state.Version = agySQLiteStateSchemaVersion
	data, err := json.Marshal(t.state)
	if err != nil {
		return fmt.Errorf("marshal antigravity sqlite state: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(t.cfg.StatePath)
	base := filepath.Base(t.cfg.StatePath)
	if err := verifyAgyParentDir(dir, t.cfg.WakeOwner); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("Antigravity SQLite tailer skipping state save in untrusted parent %s: %v", dir, err)
		return nil
	}
	tree, err := privatefs.OpenTree(dir, privatefs.TreeOptions{CreateAnchor: true})
	if err != nil {
		return fmt.Errorf("open private state dir: %w", err)
	}
	defer func() { _ = tree.Close() }()

	// Write to a fresh private temp file, fsync, and atomically rename into
	// place so a crash/SIGKILL never leaves a torn JSON file and a symlink
	// planted at the state path is replaced rather than followed.
	tmp, tmpRel, err := tree.CreateTemp("", "."+base+".tmp-*", privatefs.Private)
	if err != nil {
		return fmt.Errorf("create temp antigravity sqlite state: %w", err)
	}
	defer func() {
		if tmpRel != "" {
			_ = tree.Remove(tmpRel)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp antigravity sqlite state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp antigravity sqlite state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp antigravity sqlite state: %w", err)
	}
	if err := tree.Rename(tmpRel, base); err != nil {
		return fmt.Errorf("rename antigravity sqlite state: %w", err)
	}
	tmpRel = ""
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("sync antigravity sqlite state dir: %w", err)
	}
	return nil
}
