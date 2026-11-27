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

package shipper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/pathcanon"
)

// fileIdentityFn allows internal tests to inject a fileIdentity failure.
var fileIdentityFn = fileIdentity

// Round trigger reasons passed in RoundOptions.Reason.
const (
	ReasonPeriodic      = "periodic"
	ReasonSessionEnd    = "session_end"
	ReasonModelEnd      = "model_end"
	ReasonPreStop       = "pre_stop"
	ReasonResumeCatchup = "resume_catchup"
)

// ShipOutcome classifies the Hub's response to uploading a single segment.
type ShipOutcome string

const (
	// OutcomeCommitted indicates the segment is durably committed on the Hub
	// (either newly committed or idempotently replayed with matching raw_sha256).
	OutcomeCommitted ShipOutcome = "committed"

	// OutcomeRetryable indicates a transient failure; the watermark must not
	// advance and the pending segment will be replayed on a future round.
	OutcomeRetryable ShipOutcome = "retryable"

	// OutcomeConflict indicates a terminal identity conflict (409 segment_conflict
	// or corrupt object). The shipper bumps artifact_generation, restarts at 0,
	// and records the error without overwriting prior segments.
	OutcomeConflict ShipOutcome = "conflict"

	// OutcomeCaptureUnavailable indicates capture is unavailable on the Hub
	// (e.g. 404 experiment off). The shipper stops the round and signals
	// callers to back off rather than retry hot.
	OutcomeCaptureUnavailable ShipOutcome = "capture_unavailable"

	// OutcomePolicyRefused indicates the Hub refused capture by policy
	// (e.g. 403 capture_disabled or capture_metadata_only). The shipper stops
	// the round and signals callers to call Rebaseline(ctx, reason) and back
	// off so no backlog accumulates while capture is disabled.
	OutcomePolicyRefused ShipOutcome = "policy_refused"

	// OutcomeRunCapExceeded indicates the Hub rejected the segment with 413
	// run_cap_exceeded. The shipper records truncated=true for the current run
	// and stops shipping until a new run starts.
	OutcomeRunCapExceeded ShipOutcome = "run_cap_exceeded"

	// OutcomeRejected indicates a permanent, non-retryable rejection for a
	// single artifact (e.g. 400 invalid_segment or 413 segment_too_large). The
	// shipper parks that artifact with a recorded reason so it is not retried
	// forever, without blocking sibling artifacts.
	OutcomeRejected ShipOutcome = "rejected"
)

// ShipResult is returned by Uploader.Ship.
type ShipResult struct {
	Outcome   ShipOutcome
	SegmentID string
	Message   string
}

// Uploader delivers a single prepared Segment to the Hub (prepare -> PUT -> commit).
type Uploader interface {
	Ship(ctx context.Context, seg Segment) (ShipResult, error)
}

// Prober is an optional interface implemented by Uploaders that can probe
// whether the Hub currently accepts capture (for example via segments:prepare
// without uploading payload bytes). When state.json records
// RefusedPendingRebaseline=true after a prior 404 or 403 policy refusal,
// RunRound probes before shipping so bytes written during the refusal or
// backoff window are rebaselined away instead of shipped retroactively.
type Prober interface {
	Probe(ctx context.Context, seg Segment) (ShipResult, error)
}

// Config configures a Shipper instance.
type Config struct {
	// HomeDir is the agent home directory containing native harness artifacts
	// and .scion/trajectory-shipper/. Required.
	HomeDir string

	// RunID identifies the active agent run. Required for RunRound and
	// ResumeCatchup (optional for Baseline and ResetForRun). Used to scope
	// run-cap truncation and per-artifact rejection parks so hitting a limit on
	// run A does not disable shipping on run B.
	RunID string

	// Harness names the built-in harness spec (for example "claude"). Ignored
	// when Spec is non-nil; defaults to "claude" when both are empty.
	Harness string

	// Spec overrides the harness discovery rules when non-nil.
	Spec *HarnessSpec

	// OriginHubID is recorded on state and outgoing segments for relocation
	// continuity. Existing origin_hub_id values in state.json are preserved.
	OriginHubID string

	// BackfillExisting starts baseline artifacts at offset 0 instead of current
	// EOF. Off by default so pre-existing history is never shipped.
	BackfillExisting bool

	// MaxSegmentBytes caps the raw byte size of a single segment (default 16 MiB).
	// Automatically clamped to MaxRoundBytes if larger.
	MaxSegmentBytes int64

	// MaxRoundBytes caps total raw bytes shipped in a single RunRound call
	// (default 64 MiB).
	MaxRoundBytes int64

	// LockTimeout bounds how long Baseline/RunRound/ResumeCatchup wait for the
	// exclusive state.lock (default 10s).
	LockTimeout time.Duration

	// Uploader ships prepared segments. Required for RunRound and ResumeCatchup;
	// may be nil if only Baseline or ResetForRun is called.
	Uploader Uploader

	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
}

// Shipper discovers native harness artifacts, plans byte-range segments,
// uploads them through an Uploader, and maintains durable watermarks under an
// exclusive inter-process lock.
type Shipper struct {
	cfg  Config
	spec HarnessSpec
	now  func() time.Time
}

// New constructs a validated Shipper.
func New(cfg Config) (*Shipper, error) {
	if cfg.HomeDir == "" {
		return nil, errors.New("shipper: HomeDir is required")
	}

	var spec HarnessSpec
	if cfg.Spec != nil {
		spec = *cfg.Spec
		if spec.Name == "" {
			return nil, errors.New("shipper: Spec.Name is required")
		}
	} else {
		name := cfg.Harness
		if name == "" {
			name = "claude"
		}
		var ok bool
		spec, ok = HarnessSpecFor(name)
		if !ok {
			return nil, fmt.Errorf("shipper: unknown harness %q", name)
		}
	}

	if cfg.MaxSegmentBytes <= 0 {
		cfg.MaxSegmentBytes = DefaultMaxSegmentBytes
	}
	if cfg.MaxRoundBytes <= 0 {
		cfg.MaxRoundBytes = DefaultMaxRoundBytes
	}
	if cfg.MaxSegmentBytes > cfg.MaxRoundBytes {
		cfg.MaxSegmentBytes = cfg.MaxRoundBytes
	}
	if cfg.LockTimeout <= 0 {
		cfg.LockTimeout = DefaultLockTimeout
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	return &Shipper{
		cfg:  cfg,
		spec: spec,
		now:  nowFn,
	}, nil
}

// BaselineResult summarizes a Baseline call.
type BaselineResult struct {
	AlreadyInitialized bool
	ArtifactsTracked   int
	RefusedPaths       []RefusedPath
	SkippedPaths       []RefusedPath
}

// Baseline initializes state.json under the exclusive lock if it does not yet
// exist, recording current EOF (or 0 when BackfillExisting is true) for every
// discovered artifact so pre-existing history is never shipped. Calling
// Baseline when state.json is already baselined is a safe no-op.
//
// If a discovered artifact cannot be opened or stat'd during baseline, a
// lazy-baseline entry (needs_baseline=true) is recorded and reported in
// BaselineResult.SkippedPaths so the artifact is baselined at its first
// successful open rather than shipped from offset 0.
func (s *Shipper) Baseline(ctx context.Context) (*BaselineResult, error) {
	lock, err := acquireStateLock(ctx, s.cfg.HomeDir, s.cfg.LockTimeout)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()

	st, exists, err := LoadState(s.cfg.HomeDir)
	if err != nil {
		return nil, err
	}
	if exists && st.Baselined {
		return &BaselineResult{
			AlreadyInitialized: true,
			ArtifactsTracked:   len(st.Artifacts),
		}, nil
	}

	st, refused, skipped, err := s.initBaselineLocked(st)
	if err != nil {
		return nil, err
	}
	return &BaselineResult{
		AlreadyInitialized: false,
		ArtifactsTracked:   len(st.Artifacts),
		RefusedPaths:       refused,
		SkippedPaths:       skipped,
	}, nil
}

// RebaselineResult summarizes a Rebaseline call.
type RebaselineResult struct {
	Reason           string
	ArtifactsRebased int
	RefusedPaths     []RefusedPath
	SkippedPaths     []RefusedPath
}

func int64Ptr(v int64) *int64 {
	return &v
}

// Rebaseline moves every discovered artifact's baseline, raw_committed_offset,
// and parsed_offset to the byte immediately after the last '\n' at or before
// EOF under the exclusive state lock, clears any pending segments, and records
// reason on state.json.
//
// When the current generation already has committed or pending bytes past its
// start (RawCommittedOffset > GenerationStartOffset, or Pending != nil) and
// the new offset differs from RawCommittedOffset, Generation is incremented
// with LastGenerationReason = GenReasonRebaseline so no generation ever
// contains a byte hole.
//
// Call this when the Hub refuses capture (404 or 403 capture_disabled /
// capture_metadata_only) before backing off, so no backlog accumulates while
// capture is disabled. It also persists RefusedPendingRebaseline=true so the
// first accepted probe after backoff (or across a restart) rebaselines past
// anything written during the backoff window before shipping new bytes.
func (s *Shipper) Rebaseline(ctx context.Context, reason string) (*RebaselineResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lock, err := acquireStateLock(ctx, s.cfg.HomeDir, s.cfg.LockTimeout)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()

	st, exists, err := LoadState(s.cfg.HomeDir)
	if err != nil {
		return nil, err
	}
	if !exists || st == nil {
		st = &State{
			Version:   StateVersion,
			Harness:   s.spec.Name,
			Artifacts: make(map[string]*ArtifactState),
		}
	}
	return s.rebaselineLocked(ctx, st, reason, true)
}

func (s *Shipper) rebaselineLocked(ctx context.Context, st *State, reason string, markRefusalPending bool) (*RebaselineResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resolvedHome, err := pathcanon.Resolve(s.cfg.HomeDir)
	if err != nil {
		return nil, fmt.Errorf("resolve home directory %s: %w", s.cfg.HomeDir, err)
	}
	disc, err := Discover(resolvedHome, s.spec)
	if err != nil {
		return nil, err
	}

	now := s.now().UTC()
	if st.Version == 0 {
		st.Version = StateVersion
	}
	if st.Harness == "" {
		st.Harness = s.spec.Name
	}
	if st.OriginHubID == "" {
		st.OriginHubID = s.cfg.OriginHubID
	}
	if st.Artifacts == nil {
		st.Artifacts = make(map[string]*ArtifactState)
	}
	st.Baselined = true
	st.BaselinedAt = &now
	st.LastRebaselineReason = reason
	st.LastRebaselineAt = &now
	st.RefusedPendingRebaseline = markRefusalPending
	st.DeferredBaselinePrefixes = mergeDeferredPrefixes(st.DeferredBaselinePrefixes, disc.Refused)

	discoveredSet := make(map[string]struct{}, len(disc.Artifacts))
	var skipped []RefusedPath
	rebased := 0

	for _, art := range disc.Artifacts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		discoveredSet[art.NativeRelativePath] = struct{}{}
		existing := st.Artifacts[art.NativeRelativePath]

		f, fi, oerr := openArtifactNoFollow(resolvedHome, art.NativeRelativePath)
		if oerr != nil {
			markRebaselineSkipped(st, art, existing, oerr, now, &skipped)
			continue
		}

		fid, fidErr := fileIdentityFn(fi)
		if fidErr != nil {
			_ = f.Close()
			markRebaselineSkipped(st, art, existing, fidErr, now, &skipped)
			continue
		}

		// Rebaseline to current EOF and, if EOF is mid-line (last byte != '\n'),
		// set SkipToNextNewline so the rest of that partial line is skipped
		// once completed and the next segment starts on a clean line boundary
		// without shipping any bytes from the refused partial line.
		offset := fi.Size()
		skipToNextNL := false
		if offset > 0 {
			var last [1]byte
			n, rerr := f.ReadAt(last[:], offset-1)
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				_ = f.Close()
				markRebaselineSkipped(st, art, existing, rerr, now, &skipped)
				continue
			}
			if n == 1 && last[0] != '\n' {
				skipToNextNL = true
			}
		}

		var guardHash string
		var guardLen int
		if offset > 0 {
			var gerr error
			guardHash, guardLen, gerr = ComputeGuardHash(f, offset)
			if gerr != nil {
				_ = f.Close()
				markRebaselineSkipped(st, art, existing, gerr, now, &skipped)
				continue
			}
		}
		_ = f.Close()

		gen := 0
		originHubID := st.OriginHubID
		lastGenReason := ""
		genStart := offset
		if existing != nil {
			gen = existing.Generation
			lastGenReason = existing.LastGenerationReason
			if existing.OriginHubID != "" {
				originHubID = existing.OriginHubID
			}
			hasWork := generationHasCommittedOrPending(existing)
			shrankOrRotated := existing.RawCommittedOffset > 0 && (offset < existing.RawCommittedOffset || (existing.Inode != 0 && fid.Ino != existing.Inode))
			// Bump if the file shrank/rotated, if an in-flight Pending segment
			// existed (it may already be committed on the Hub), or if advancing
			// offset within a generation that already has committed bytes would
			// create a byte hole.
			createsHole := existing.Pending != nil || (hasWork && offset != existing.RawCommittedOffset)
			if shrankOrRotated || createsHole {
				gen++
				lastGenReason = GenReasonRebaseline
			} else if hasWork && existing.GenerationStartOffset != nil {
				// Generation stayed the same and already has committed bytes
				// (idle artifact with offset == RawCommittedOffset): preserve
				// GenerationStartOffset so a subsequent Rebaseline knows this
				// generation still holds committed bytes.
				genStart = *existing.GenerationStartOffset
			}
		}

		// Mutate existing entry in place when present so unknown JSON fields
		// (Extra, preserved for P7 normalizers) survive Rebaseline. Preserve
		// the original run BaselineOffset/BaselineGuard* when already baselined
		// so a later truncation above the initial baseline still protects
		// pre-baseline history. Explicitly reset transient failure/rejection
		// state since the artifact is being freshly rebaselined.
		entry := existing
		preserveOrigBaseline := entry != nil && entry.Baseline && !entry.NeedsBaseline && entry.BaselineOffset > 0
		if entry == nil {
			entry = &ArtifactState{}
			st.Artifacts[art.NativeRelativePath] = entry
		}
		entry.NativeRelativePath = art.NativeRelativePath
		entry.ArtifactKind = art.Kind
		entry.Inode = fid.Ino
		entry.Device = fid.Dev
		entry.FileID = fid.String()
		entry.Generation = gen
		entry.GenerationStartOffset = int64Ptr(genStart)
		entry.RawCommittedOffset = offset
		entry.ParsedOffset = offset
		entry.GuardHash = guardHash
		entry.GuardLen = guardLen
		entry.Pending = nil
		entry.Baseline = true
		if !preserveOrigBaseline {
			entry.BaselineOffset = offset
			entry.BaselineGuardHash = guardHash
			entry.BaselineGuardLen = guardLen
		}
		entry.RebaselineOffset = offset
		entry.RebaselineGuardHash = guardHash
		entry.RebaselineGuardLen = guardLen
		entry.NeedsBaseline = false
		entry.SkipToNextNewline = skipToNextNL
		entry.OriginHubID = originHubID
		entry.ConsecutiveFailures = 0
		entry.LastError = ""
		entry.LastGenerationReason = lastGenReason
		entry.Rejected = false
		entry.RejectedReason = ""
		entry.RejectedRunID = ""
		entry.UpdatedAt = &now
		rebased++
	}

	// Clear any pending segments on tracked artifacts not seen in this scan.
	for path, entry := range st.Artifacts {
		if _, seen := discoveredSet[path]; !seen && entry != nil && entry.Pending != nil {
			entry.Pending = nil
			entry.UpdatedAt = &now
		}
	}

	if err := SaveState(s.cfg.HomeDir, st); err != nil {
		return nil, err
	}
	return &RebaselineResult{
		Reason:           reason,
		ArtifactsRebased: rebased,
		RefusedPaths:     disc.Refused,
		SkippedPaths:     skipped,
	}, nil
}

// findNextNewlineOffset scans forward from startOffset up to fileSize (bounded
// by maxScanBytes) for the first '\n'. If found, it returns (pos+1, true, nil).
// If the scan hits maxScanBytes without finding '\n', it returns
// (startOffset+maxScanBytes, true, nil) to avoid unbounded waiting. Otherwise
// it returns (startOffset, false, nil).
func findNextNewlineOffset(ctx context.Context, r io.ReaderAt, startOffset, fileSize, maxScanBytes int64) (int64, bool, error) {
	if fileSize <= startOffset {
		return startOffset, false, nil
	}
	const chunkSize int64 = 64 * 1024
	avail := fileSize - startOffset
	limit := avail
	capped := false
	if maxScanBytes > 0 && maxScanBytes < avail {
		limit = maxScanBytes
		capped = true
	}
	endAt := startOffset + limit
	buf := make([]byte, min(limit, chunkSize))
	pos := startOffset
	for pos < endAt {
		if err := ctx.Err(); err != nil {
			return startOffset, false, err
		}
		readLen := min(endAt-pos, int64(len(buf)))
		n, err := r.ReadAt(buf[:readLen], pos)
		if err != nil && !errors.Is(err, io.EOF) {
			return startOffset, false, err
		}
		if n == 0 {
			break
		}
		chunk := buf[:n]
		for i := 0; i < len(chunk); i++ {
			if chunk[i] == '\n' {
				return pos + int64(i) + 1, true, nil
			}
		}
		pos += int64(n)
	}
	if capped {
		return endAt, true, nil
	}
	return startOffset, false, nil
}

func generationHasCommittedOrPending(entry *ArtifactState) bool {
	if entry == nil {
		return false
	}
	if entry.Pending != nil {
		return true
	}
	if entry.GenerationStartOffset != nil {
		return entry.RawCommittedOffset > *entry.GenerationStartOffset
	}
	if entry.Generation == 0 {
		// Legacy state.json without generation_start_offset: gen 0 started at BaselineOffset.
		return entry.RawCommittedOffset > entry.BaselineOffset
	}
	// Legacy state.json for gen > 0 without generation_start_offset: bump conservatively.
	return true
}

func markRebaselineSkipped(
	st *State,
	art DiscoveredArtifact,
	existing *ArtifactState,
	err error,
	now time.Time,
	skipped *[]RefusedPath,
) {
	*skipped = append(*skipped, RefusedPath{
		NativeRelativePath: art.NativeRelativePath,
		Reason:             err.Error(),
	})
	if existing == nil {
		st.Artifacts[art.NativeRelativePath] = &ArtifactState{
			NativeRelativePath:    art.NativeRelativePath,
			ArtifactKind:          art.Kind,
			Generation:            0,
			GenerationStartOffset: int64Ptr(0),
			Baseline:              true,
			NeedsBaseline:         true,
			OriginHubID:           st.OriginHubID,
			LastError:             err.Error(),
			UpdatedAt:             &now,
		}
		return
	}
	// If the current generation already holds committed or pending bytes, bump
	// Generation now so the later lazy baseline starts a fresh generation
	// rather than jumping to EOF inside the old generation and leaving a hole.
	if generationHasCommittedOrPending(existing) {
		existing.Generation++
		existing.LastGenerationReason = GenReasonRebaseline
		existing.GenerationStartOffset = int64Ptr(0)
		existing.RawCommittedOffset = 0
		existing.ParsedOffset = 0
		existing.GuardHash = ""
		existing.GuardLen = 0
	}
	existing.Pending = nil
	existing.Baseline = true
	existing.NeedsBaseline = true
	existing.LastError = err.Error()
	existing.UpdatedAt = &now
}

// ResetForRun clears run-scoped truncation and per-artifact rejection parks
// when starting a new run_id. It is safe to call even if state.json does not
// yet exist.
func (s *Shipper) ResetForRun(ctx context.Context, runID string) error {
	lock, err := acquireStateLock(ctx, s.cfg.HomeDir, s.cfg.LockTimeout)
	if err != nil {
		return err
	}
	defer lock.Unlock()

	st, exists, err := LoadState(s.cfg.HomeDir)
	if err != nil || !exists {
		return err
	}
	changed := false
	if st.Truncated && (runID == "" || st.TruncatedRunID != runID) {
		clearRunTruncation(st)
		changed = true
	}
	for _, entry := range st.Artifacts {
		if entry.Rejected && (runID == "" || entry.RejectedRunID != runID) {
			entry.Rejected = false
			entry.RejectedReason = ""
			entry.RejectedRunID = ""
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return SaveState(s.cfg.HomeDir, st)
}

func clearRunTruncation(st *State) {
	st.Truncated = false
	st.TruncatedRunID = ""
	st.TruncatedReason = ""
	for _, entry := range st.Artifacts {
		entry.Truncated = false
		entry.TruncatedRunID = ""
	}
}

func (s *Shipper) initBaselineLocked(existing *State) (*State, []RefusedPath, []RefusedPath, error) {
	resolvedHome, err := pathcanon.Resolve(s.cfg.HomeDir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve home directory %s: %w", s.cfg.HomeDir, err)
	}
	disc, err := Discover(resolvedHome, s.spec)
	if err != nil {
		return nil, nil, nil, err
	}

	now := s.now().UTC()
	st := existing
	if st == nil {
		st = &State{
			Version:   StateVersion,
			Harness:   s.spec.Name,
			Artifacts: make(map[string]*ArtifactState),
		}
	}
	if st.Version == 0 {
		st.Version = StateVersion
	}
	if st.Harness == "" {
		st.Harness = s.spec.Name
	}
	if st.OriginHubID == "" {
		st.OriginHubID = s.cfg.OriginHubID
	}
	st.Baselined = true
	st.BaselinedAt = &now
	st.BackfillExisting = s.cfg.BackfillExisting
	st.DeferredBaselinePrefixes = mergeDeferredPrefixes(st.DeferredBaselinePrefixes, disc.Refused)

	var skipped []RefusedPath
	for _, art := range disc.Artifacts {
		if _, tracked := st.Artifacts[art.NativeRelativePath]; tracked {
			continue
		}
		entry, err := s.baselineArtifact(resolvedHome, art, st.OriginHubID, now)
		if err != nil {
			// Record a lazy-baseline entry so a transient open/stat error at
			// baseline time never causes pre-baseline history to ship from 0.
			skipped = append(skipped, RefusedPath{
				NativeRelativePath: art.NativeRelativePath,
				Reason:             err.Error(),
			})
			st.Artifacts[art.NativeRelativePath] = &ArtifactState{
				NativeRelativePath: art.NativeRelativePath,
				ArtifactKind:       art.Kind,
				Generation:         0,
				Baseline:           true,
				NeedsBaseline:      true,
				OriginHubID:        st.OriginHubID,
				LastError:          err.Error(),
				UpdatedAt:          &now,
			}
			continue
		}
		st.Artifacts[art.NativeRelativePath] = entry
	}

	if err := SaveState(s.cfg.HomeDir, st); err != nil {
		return nil, disc.Refused, skipped, err
	}
	return st, disc.Refused, skipped, nil
}

func (s *Shipper) baselineArtifact(resolvedHome string, art DiscoveredArtifact, originHubID string, now time.Time) (*ArtifactState, error) {
	f, fi, err := openArtifactNoFollow(resolvedHome, art.NativeRelativePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	fid, err := fileIdentityFn(fi)
	if err != nil {
		return nil, err
	}

	var offset int64
	var guardHash string
	var guardLen int
	if !s.cfg.BackfillExisting {
		offset = fi.Size()
		if offset > 0 {
			guardHash, guardLen, err = ComputeGuardHash(f, offset)
			if err != nil {
				return nil, err
			}
		}
	}

	return &ArtifactState{
		NativeRelativePath:    art.NativeRelativePath,
		ArtifactKind:          art.Kind,
		Inode:                 fid.Ino,
		Device:                fid.Dev,
		FileID:                fid.String(),
		Generation:            0,
		GenerationStartOffset: int64Ptr(offset),
		RawCommittedOffset:    offset,
		ParsedOffset:          offset,
		GuardHash:             guardHash,
		GuardLen:              guardLen,
		Baseline:              true,
		BaselineOffset:        offset,
		BaselineGuardHash:     guardHash,
		BaselineGuardLen:      guardLen,
		OriginHubID:           originHubID,
		UpdatedAt:             &now,
	}, nil
}

// RoundOptions configures a single RunRound execution.
type RoundOptions struct {
	// Final ships any trailing incomplete line at EOF with incomplete_tail=true.
	Final bool

	// Reason identifies the trigger (periodic, session_end, pre_stop, resume_catchup).
	Reason string

	// MaxBytes optionally overrides Config.MaxRoundBytes for this round when > 0.
	MaxBytes int64
}

// RoundResult summarizes a single RunRound or ResumeCatchup execution.
type RoundResult struct {
	Reason              string
	ArtifactsScanned    int
	SegmentsShipped     int
	BytesShipped        int64
	GenerationBumps     int
	CaptureUnavailable  bool
	PolicyRefused       bool
	PolicyRefusedReason string
	Truncated           bool
	BudgetExhausted     bool
	RefusedPaths        []RefusedPath
	ArtifactErrors      map[string]error
}

// ResumeCatchup ships any unshipped complete bytes left by a prior container or
// run before new work begins, under the exclusive lock.
func (s *Shipper) ResumeCatchup(ctx context.Context) (*RoundResult, error) {
	return s.RunRound(ctx, RoundOptions{
		Final:  false,
		Reason: ReasonResumeCatchup,
	})
}

// RunRound discovers artifacts, detects truncation/rotation/guard mismatches,
// and ships pending segments under the exclusive state lock. Config.RunID and
// Config.Uploader are required. Watermarks advance only after Uploader.Ship
// returns OutcomeCommitted.
func (s *Shipper) RunRound(ctx context.Context, opts RoundOptions) (*RoundResult, error) {
	if s.cfg.Uploader == nil {
		return nil, errors.New("shipper: Uploader is required for RunRound")
	}
	if s.cfg.RunID == "" {
		return nil, errors.New("shipper: RunID is required for RunRound")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	lock, err := acquireStateLock(ctx, s.cfg.HomeDir, s.cfg.LockTimeout)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()

	res := &RoundResult{
		Reason:         opts.Reason,
		ArtifactErrors: make(map[string]error),
	}

	st, exists, err := LoadState(s.cfg.HomeDir)
	if err != nil {
		return nil, err
	}
	if !exists || !st.Baselined {
		var refused []RefusedPath
		st, refused, _, err = s.initBaselineLocked(st)
		if err != nil {
			return nil, err
		}
		res.RefusedPaths = refused
	}

	if st.OriginHubID == "" && s.cfg.OriginHubID != "" {
		st.OriginHubID = s.cfg.OriginHubID
	}

	// Run-cap truncation applies only to the capped run. When a new run_id
	// starts, clear truncation and resume shipping.
	if st.Truncated {
		if s.cfg.RunID != "" && st.TruncatedRunID != "" && st.TruncatedRunID != s.cfg.RunID {
			clearRunTruncation(st)
			if err := SaveState(s.cfg.HomeDir, st); err != nil {
				return nil, err
			}
		} else {
			res.Truncated = true
			return res, nil
		}
	}

	resolvedHome, err := pathcanon.Resolve(s.cfg.HomeDir)
	if err != nil {
		return nil, fmt.Errorf("resolve home directory %s: %w", s.cfg.HomeDir, err)
	}

	disc, err := Discover(resolvedHome, s.spec)
	if err != nil {
		return nil, err
	}
	res.RefusedPaths = disc.Refused
	res.ArtifactsScanned = len(disc.Artifacts)

	roundMaxBytes := s.cfg.MaxRoundBytes
	if opts.MaxBytes > 0 {
		roundMaxBytes = opts.MaxBytes
	}
	effectiveMaxSeg := min(s.cfg.MaxSegmentBytes, roundMaxBytes)
	remainingBudget := roundMaxBytes

	n := len(disc.Artifacts)
	completedFullScan := true
	stateDirty := false
	if n > 0 {
		start := 0
		if st.NextStartPath != "" {
			idx := sort.Search(n, func(i int) bool {
				return disc.Artifacts[i].NativeRelativePath >= st.NextStartPath
			})
			if idx < n {
				start = idx
			}
		}
		lastIdx := start
		firstBudgetSkipIdx := -1
		for i := 0; i < n; i++ {
			idx := (start + i) % n
			lastIdx = idx
			art := disc.Artifacts[idx]
			if err := ctx.Err(); err != nil {
				st.NextStartPath = disc.Artifacts[(lastIdx+1)%n].NativeRelativePath
				_ = SaveState(s.cfg.HomeDir, st)
				return res, err
			}
			if remainingBudget <= 0 {
				res.BudgetExhausted = true
				completedFullScan = false
				if firstBudgetSkipIdx == -1 {
					firstBudgetSkipIdx = idx
				}
				break
			}

			budgetBefore := res.BudgetExhausted
			remainingBefore := remainingBudget
			stopRound, err := s.shipArtifactLocked(ctx, resolvedHome, st, art, effectiveMaxSeg, opts.Final, &remainingBudget, res)
			if err != nil {
				st.NextStartPath = disc.Artifacts[(lastIdx+1)%n].NativeRelativePath
				_ = SaveState(s.cfg.HomeDir, st)
				return res, err
			}
			if !budgetBefore && res.BudgetExhausted && remainingBudget == remainingBefore && firstBudgetSkipIdx == -1 {
				firstBudgetSkipIdx = idx
			}
			if stopRound {
				completedFullScan = false
				nextPath := disc.Artifacts[(lastIdx+1)%n].NativeRelativePath
				if st.NextStartPath != nextPath {
					st.NextStartPath = nextPath
					stateDirty = true
				}
				break
			}
			if remainingBudget <= 0 {
				if i+1 < n {
					res.BudgetExhausted = true
					completedFullScan = false
					if firstBudgetSkipIdx == -1 {
						firstBudgetSkipIdx = (idx + 1) % n
					}
				} else if firstBudgetSkipIdx == -1 {
					firstBudgetSkipIdx = (idx + 1) % n
				}
				break
			}
		}
		if completedFullScan || firstBudgetSkipIdx >= 0 {
			nextIdx := (lastIdx + 1) % n
			if firstBudgetSkipIdx >= 0 {
				nextIdx = firstBudgetSkipIdx
			}
			nextPath := disc.Artifacts[nextIdx].NativeRelativePath
			if st.NextStartPath != nextPath {
				st.NextStartPath = nextPath
				stateDirty = true
			}
		}
	}

	// Prune deferred baseline prefixes that are no longer refused once the
	// round has scanned every discovered artifact cleanly, so genuinely new
	// files created later in recovered directories start at offset 0.
	if completedFullScan && !res.BudgetExhausted && len(st.DeferredBaselinePrefixes) > 0 {
		pruned := pruneResolvedDeferredPrefixes(st.DeferredBaselinePrefixes, disc.Refused)
		if len(pruned) != len(st.DeferredBaselinePrefixes) {
			st.DeferredBaselinePrefixes = pruned
			stateDirty = true
		}
	}
	if stateDirty {
		if err := SaveState(s.cfg.HomeDir, st); err != nil {
			return res, err
		}
	}

	return res, nil
}

// shipArtifactLocked processes a single artifact under the already-held state
// lock. Per-artifact faults are recorded in res.ArtifactErrors without stopping
// sibling artifacts; fatal local errors (state persistence failure, context
// cancellation) are returned to the caller.
func (s *Shipper) shipArtifactLocked(
	ctx context.Context,
	resolvedHome string,
	st *State,
	art DiscoveredArtifact,
	effectiveMaxSeg int64,
	final bool,
	remainingBudget *int64,
	res *RoundResult,
) (stopRound bool, fatalErr error) {
	// If this untracked artifact lies under a deferred baseline prefix, record
	// its lazy-baseline entry BEFORE opening or stat'ing it so an open/fstat/
	// identity error still preserves NeedsBaseline=true even if the prefix is
	// pruned at the end of this round.
	entry, tracked := st.Artifacts[art.NativeRelativePath]
	if !tracked && matchesDeferredBaselinePrefix(art.NativeRelativePath, st.DeferredBaselinePrefixes) {
		now := s.now().UTC()
		entry = &ArtifactState{
			NativeRelativePath: art.NativeRelativePath,
			ArtifactKind:       art.Kind,
			Generation:         0,
			Baseline:           true,
			NeedsBaseline:      true,
			OriginHubID:        st.OriginHubID,
			UpdatedAt:          &now,
		}
		st.Artifacts[art.NativeRelativePath] = entry
		tracked = true
		if err := SaveState(s.cfg.HomeDir, st); err != nil {
			return true, err
		}
	}

	f, fi, err := openArtifactNoFollow(resolvedHome, art.NativeRelativePath)
	if err != nil {
		res.ArtifactErrors[art.NativeRelativePath] = fmt.Errorf("open artifact: %w", err)
		return false, nil
	}
	defer func() { _ = f.Close() }()

	fid, err := fileIdentityFn(fi)
	if err != nil {
		res.ArtifactErrors[art.NativeRelativePath] = err
		return false, nil
	}
	fileSize := fi.Size()

	if !tracked {
		now := s.now().UTC()
		entry = &ArtifactState{
			NativeRelativePath:    art.NativeRelativePath,
			ArtifactKind:          art.Kind,
			Inode:                 fid.Ino,
			Device:                fid.Dev,
			FileID:                fid.String(),
			Generation:            0,
			GenerationStartOffset: int64Ptr(0),
			RawCommittedOffset:    0,
			ParsedOffset:          0,
			Baseline:              false,
			OriginHubID:           st.OriginHubID,
			UpdatedAt:             &now,
		}
		st.Artifacts[art.NativeRelativePath] = entry
	} else {
		if entry.NativeRelativePath == "" {
			entry.NativeRelativePath = art.NativeRelativePath
		}
		if entry.ArtifactKind == "" {
			entry.ArtifactKind = art.Kind
		}
		if entry.OriginHubID == "" {
			entry.OriginHubID = st.OriginHubID
		}
	}

	// Clear a per-artifact rejection park when a new run_id starts.
	if entry.Rejected && s.cfg.RunID != "" && entry.RejectedRunID != "" && entry.RejectedRunID != s.cfg.RunID {
		now := s.now().UTC()
		entry.Rejected = false
		entry.RejectedReason = ""
		entry.RejectedRunID = ""
		entry.UpdatedAt = &now
		if err := SaveState(s.cfg.HomeDir, st); err != nil {
			return true, err
		}
	}

	// Complete deferred baseline on first successful open.
	if entry.NeedsBaseline {
		now := s.now().UTC()
		var offset int64
		var guardHash string
		var guardLen int
		skipToNextNL := false
		if !st.BackfillExisting && !s.cfg.BackfillExisting {
			offset = fileSize
			if offset > 0 {
				var last [1]byte
				n, rerr := f.ReadAt(last[:], offset-1)
				if rerr != nil && !errors.Is(rerr, io.EOF) {
					res.ArtifactErrors[art.NativeRelativePath] = fmt.Errorf("lazy baseline tail read: %w", rerr)
					return false, nil
				}
				if n == 1 && last[0] != '\n' {
					skipToNextNL = true
				}
				guardHash, guardLen, err = ComputeGuardHash(f, offset)
				if err != nil {
					res.ArtifactErrors[art.NativeRelativePath] = fmt.Errorf("lazy baseline guard hash: %w", err)
					return false, nil
				}
			}
		}
		entry.Inode = fid.Ino
		entry.Device = fid.Dev
		entry.FileID = fid.String()
		entry.GenerationStartOffset = int64Ptr(offset)
		entry.RawCommittedOffset = offset
		entry.ParsedOffset = offset
		if entry.BaselineOffset == 0 {
			entry.BaselineOffset = offset
			entry.BaselineGuardHash = guardHash
			entry.BaselineGuardLen = guardLen
		}
		entry.RebaselineOffset = offset
		entry.RebaselineGuardHash = guardHash
		entry.RebaselineGuardLen = guardLen
		entry.GuardHash = guardHash
		entry.GuardLen = guardLen
		entry.NeedsBaseline = false
		entry.SkipToNextNewline = skipToNextNL
		entry.LastError = ""
		entry.UpdatedAt = &now
		if err := SaveState(s.cfg.HomeDir, st); err != nil {
			return true, err
		}
	}

	// Detect truncation, inode replacement, or guard-hash mismatch.
	if bumpReason, err := detectGenerationBump(f, entry, fid, fileSize); err != nil {
		res.ArtifactErrors[art.NativeRelativePath] = fmt.Errorf("verify guard: %w", err)
		return false, nil
	} else if bumpReason != "" {
		if err := s.bumpGenerationLocked(f, st, entry, fid, fileSize, bumpReason, "", false, res); err != nil {
			return true, err
		}
	} else {
		// Adopt current file identity unconditionally when nothing has been
		// committed or left pending at offset 0 (avoids a spurious inode bump
		// if a zero-offset file is renamed before its first commit).
		if entry.Inode == 0 || (entry.RawCommittedOffset == 0 && entry.Pending == nil) {
			entry.Inode = fid.Ino
		}
		// Keep Device current when broker reads via virtiofs/NFS with a
		// different st_dev on the same inode.
		entry.Device = fid.Dev
		entry.FileID = fid.String()
	}

	// If still parked after run-change and generation-bump checks, skip shipping.
	if entry.Rejected {
		return false, nil
	}

	// If a prior rebaseline or lazy baseline landed mid-line, advance past the
	// remainder of that partial line to the next '\n' (without shipping any
	// bytes of the partial line) so the first segment of this generation starts
	// cleanly on a line boundary.
	if entry.SkipToNextNewline {
		nextOff, found, serr := findNextNewlineOffset(ctx, f, entry.RawCommittedOffset, fileSize, s.cfg.MaxSegmentBytes)
		if serr != nil {
			if errors.Is(serr, context.Canceled) || errors.Is(serr, context.DeadlineExceeded) {
				return true, serr
			}
			res.ArtifactErrors[art.NativeRelativePath] = fmt.Errorf("skip partial line: %w", serr)
			return false, nil
		}
		if !found {
			// Partial line still not terminated; hold back even on Final=true.
			return false, nil
		}
		gh, gl, gerr := ComputeGuardHash(f, nextOff)
		if gerr != nil {
			res.ArtifactErrors[art.NativeRelativePath] = fmt.Errorf("compute guard after partial line skip: %w", gerr)
			return false, nil
		}
		now := s.now().UTC()
		entry.RawCommittedOffset = nextOff
		entry.ParsedOffset = nextOff
		entry.GenerationStartOffset = int64Ptr(nextOff)
		entry.GuardHash = gh
		entry.GuardLen = gl
		entry.SkipToNextNewline = false
		entry.UpdatedAt = &now
		if err := SaveState(s.cfg.HomeDir, st); err != nil {
			return true, err
		}
	}

	for *remainingBudget > 0 {
		if err := ctx.Err(); err != nil {
			return true, err
		}

		var (
			raw            []byte
			byteEnd        int64
			incompleteTail bool
			ok             bool
			fromPending    bool
		)

		if entry.Pending != nil {
			if entry.Pending.ByteEnd <= entry.RawCommittedOffset {
				entry.Pending = nil
			} else if fileSize < entry.Pending.ByteEnd {
				if err := s.bumpGenerationLocked(f, st, entry, fid, fileSize, GenReasonTruncated, "", false, res); err != nil {
					return true, err
				}
				continue
			} else {
				pendingLen := entry.Pending.ByteEnd - entry.RawCommittedOffset
				if pendingLen > *remainingBudget {
					res.BudgetExhausted = true
					return false, nil
				}
				buf := make([]byte, pendingLen)
				n, rerr := f.ReadAt(buf, entry.RawCommittedOffset)
				if rerr != nil && !errors.Is(rerr, io.EOF) {
					res.ArtifactErrors[art.NativeRelativePath] = fmt.Errorf("read pending segment: %w", rerr)
					return false, nil
				}
				if int64(n) != pendingLen {
					if err := s.bumpGenerationLocked(f, st, entry, fid, fileSize, GenReasonTruncated, "", false, res); err != nil {
						return true, err
					}
					continue
				}
				sum := sha256.Sum256(buf)
				if hex.EncodeToString(sum[:]) != entry.Pending.RawSHA256 {
					if err := s.bumpGenerationLocked(f, st, entry, fid, fileSize, GenReasonGuardMismatch, "", false, res); err != nil {
						return true, err
					}
					continue
				}
				raw = buf
				byteEnd = entry.Pending.ByteEnd
				incompleteTail = entry.Pending.IncompleteTail
				ok = true
				fromPending = true
			}
		}

		if !fromPending {
			raw, byteEnd, incompleteTail, ok, err = PlanSegment(
				f,
				fileSize,
				entry.RawCommittedOffset,
				effectiveMaxSeg,
				*remainingBudget,
				final,
			)
			if err != nil {
				res.ArtifactErrors[art.NativeRelativePath] = fmt.Errorf("plan segment: %w", err)
				return false, nil
			}
			if !ok {
				if fileSize-entry.RawCommittedOffset > *remainingBudget {
					res.BudgetExhausted = true
				}
				return false, nil
			}
		}

		// Compute the post-commit guard hash from the exact raw buffer being
		// shipped (plus any needed preceding bytes before ByteStart) so a
		// concurrent file rewrite cannot poison the stored guard.
		nextGuardHash, nextGuardLen, err := computePostCommitGuard(f, entry.RawCommittedOffset, raw)
		if err != nil {
			res.ArtifactErrors[art.NativeRelativePath] = fmt.Errorf("compute post-commit guard: %w", err)
			return false, nil
		}

		originHubID := entry.OriginHubID
		if originHubID == "" {
			originHubID = st.OriginHubID
		}
		seg, err := BuildSegment(SegmentMeta{
			Harness:            s.spec.Name,
			NativeRelativePath: art.NativeRelativePath,
			ArtifactKind:       art.Kind,
			Generation:         entry.Generation,
			ByteStart:          entry.RawCommittedOffset,
			IncompleteTail:     incompleteTail,
			SessionID:          art.SessionID,
			OriginHubID:        originHubID,
		}, raw)
		if err != nil {
			res.ArtifactErrors[art.NativeRelativePath] = fmt.Errorf("build segment: %w", err)
			return false, nil
		}

		// If a prior refusal left RefusedPendingRebaseline=true and the
		// uploader supports probing (segments:prepare without uploading
		// payload), probe first before persisting pending or shipping so bytes
		// written during the refusal/backoff window never leave the container.
		if st.RefusedPendingRebaseline {
			if prober, ok := s.cfg.Uploader.(Prober); ok {
				probeRes, probeErr := prober.Probe(ctx, seg)
				*remainingBudget -= seg.RawSize
				switch probeRes.Outcome {
				case OutcomeCaptureUnavailable:
					res.CaptureUnavailable = true
					return true, nil
				case OutcomePolicyRefused:
					msg := probeRes.Message
					if msg == "" && probeErr != nil {
						msg = probeErr.Error()
					}
					if msg == "" {
						msg = "capture_policy_refused"
					}
					res.PolicyRefused = true
					res.PolicyRefusedReason = msg
					return true, nil
				case OutcomeRetryable, "":
					// Transient probe error (5xx / network / 401 / non-policy 403):
					// keep RefusedPendingRebaseline set and retry later.
					if probeErr != nil {
						res.ArtifactErrors[art.NativeRelativePath] = probeErr
					}
					return true, nil
				case OutcomeConflict:
					// 409 on probe means the Hub passed experiment/policy gates
					// AND already holds committed content at (gen, byte_start).
					// Bump generation first so rebaselineLocked cannot leave a
					// hole or overlap in the conflicted generation.
					msg := probeRes.Message
					if msg == "" && probeErr != nil {
						msg = probeErr.Error()
					}
					if msg == "" {
						msg = GenReasonConflict
					}
					if berr := s.bumpGenerationLocked(f, st, entry, fid, fileSize, GenReasonConflict, msg, true, res); berr != nil {
						return true, berr
					}
					if _, rerr := s.rebaselineLocked(ctx, st, "capture_accepted", false); rerr != nil {
						return true, rerr
					}
					return true, nil
				default:
					// OutcomeCommitted, OutcomeRejected, or OutcomeRunCapExceeded:
					// the Hub passed the experiment and policy gates, so capture
					// is accepted. Rebaseline past everything written during the
					// refusal/backoff window and clear RefusedPendingRebaseline.
					if _, rerr := s.rebaselineLocked(ctx, st, "capture_accepted", false); rerr != nil {
						return true, rerr
					}
					return true, nil
				}
			}
		}

		// Persist the pending segment record under the lock before calling Ship
		// so a lost ACK or crash followed by file growth replays the exact same
		// range and raw_sha256.
		if !fromPending {
			now := s.now().UTC()
			entry.Pending = &PendingSegment{
				ByteEnd:        seg.ByteEnd,
				RawSHA256:      seg.RawSHA256,
				IncompleteTail: seg.IncompleteTail,
			}
			entry.UpdatedAt = &now
			if err := SaveState(s.cfg.HomeDir, st); err != nil {
				return true, err
			}
		}

		shipRes, shipErr := s.cfg.Uploader.Ship(ctx, seg)
		now := s.now().UTC()
		// Charge every Ship attempt's raw bytes against the round budget so
		// repeated post-PUT failures (422, timeouts, network drops) cannot
		// exceed MaxRoundBytes in a single round.
		*remainingBudget -= seg.RawSize

		switch shipRes.Outcome {
		case OutcomeCommitted:
			if shipErr != nil {
				// Defensive: treat a non-nil error with committed outcome as retryable.
				entry.ConsecutiveFailures++
				entry.LastError = shipErr.Error()
				entry.UpdatedAt = &now
				if err := SaveState(s.cfg.HomeDir, st); err != nil {
					return true, err
				}
				res.ArtifactErrors[art.NativeRelativePath] = shipErr
				if *remainingBudget <= 0 {
					res.BudgetExhausted = true
				}
				return false, nil
			}
			entry.RawCommittedOffset = byteEnd
			entry.GuardHash = nextGuardHash
			entry.GuardLen = nextGuardLen
			entry.Pending = nil
			entry.ConsecutiveFailures = 0
			entry.LastError = ""
			entry.UpdatedAt = &now
			if err := SaveState(s.cfg.HomeDir, st); err != nil {
				return true, err
			}
			res.SegmentsShipped++
			res.BytesShipped += seg.RawSize

		case OutcomeConflict:
			msg := shipRes.Message
			if msg == "" && shipErr != nil {
				msg = shipErr.Error()
			}
			if msg == "" {
				msg = GenReasonConflict
			}
			if err := s.bumpGenerationLocked(f, st, entry, fid, fileSize, GenReasonConflict, msg, true, res); err != nil {
				return true, err
			}
			res.ArtifactErrors[art.NativeRelativePath] = fmt.Errorf("segment conflict: %s", msg)
			if *remainingBudget <= 0 {
				res.BudgetExhausted = true
			}
			return false, nil

		case OutcomeRejected:
			msg := shipRes.Message
			if msg == "" && shipErr != nil {
				msg = shipErr.Error()
			}
			if msg == "" {
				msg = "segment rejected"
			}
			entry.Pending = nil
			entry.Rejected = true
			entry.RejectedReason = msg
			entry.RejectedRunID = s.cfg.RunID
			entry.ConsecutiveFailures++
			entry.LastError = msg
			entry.UpdatedAt = &now
			if err := SaveState(s.cfg.HomeDir, st); err != nil {
				return true, err
			}
			res.ArtifactErrors[art.NativeRelativePath] = fmt.Errorf("segment rejected: %s", msg)
			return false, nil

		case OutcomeCaptureUnavailable:
			res.CaptureUnavailable = true
			return true, nil

		case OutcomePolicyRefused:
			msg := shipRes.Message
			if msg == "" && shipErr != nil {
				msg = shipErr.Error()
			}
			if msg == "" {
				msg = "capture_policy_refused"
			}
			res.PolicyRefused = true
			res.PolicyRefusedReason = msg
			return true, nil

		case OutcomeRunCapExceeded:
			msg := shipRes.Message
			if msg == "" && shipErr != nil {
				msg = shipErr.Error()
			}
			if msg == "" {
				msg = "run_cap_exceeded"
			}
			st.Truncated = true
			st.TruncatedRunID = s.cfg.RunID
			st.TruncatedReason = msg
			entry.Truncated = true
			entry.TruncatedRunID = s.cfg.RunID
			entry.UpdatedAt = &now
			if err := SaveState(s.cfg.HomeDir, st); err != nil {
				return true, err
			}
			res.Truncated = true
			return true, nil

		default:
			// OutcomeRetryable or empty outcome with error. Keep entry.Pending
			// intact so the next round replays the exact same byte range.
			errObj := shipErr
			if errObj == nil {
				if shipRes.Message != "" {
					errObj = errors.New(shipRes.Message)
				} else {
					errObj = errors.New("retryable upload failure")
				}
			}
			entry.ConsecutiveFailures++
			entry.LastError = errObj.Error()
			entry.UpdatedAt = &now
			if err := SaveState(s.cfg.HomeDir, st); err != nil {
				return true, err
			}
			res.ArtifactErrors[art.NativeRelativePath] = errObj
			if *remainingBudget <= 0 {
				res.BudgetExhausted = true
			}
			return false, nil
		}
	}

	if *remainingBudget <= 0 && fileSize > entry.RawCommittedOffset {
		res.BudgetExhausted = true
	}
	return false, nil
}

func (s *Shipper) bumpGenerationLocked(
	f *os.File,
	st *State,
	entry *ArtifactState,
	fid fileID,
	fileSize int64,
	reason string,
	errMsg string,
	countFailure bool,
	res *RoundResult,
) error {
	now := s.now().UTC()

	// Preserve both refusal-window privacy and pre-baseline privacy:
	// 1. If the artifact was rebaselined past a refusal window and the file
	//    still has intact bytes up to RebaselineOffset, start the new
	//    generation at RebaselineOffset so refused-window bytes cannot leak
	//    on a 409 conflict or inode rotation.
	// 2. Otherwise, if the file still has intact pre-baseline bytes up to
	//    BaselineOffset, start at BaselineOffset so pre-baseline bytes cannot
	//    leak after a truncation above the original baseline.
	// 3. Otherwise, restart at offset 0.
	var restartOffset int64
	var restartGuardHash string
	var restartGuardLen int
	var skipToNextNL bool

	tryPreserve := func(off int64, wantHash string, wantLen int) bool {
		if off <= 0 || fileSize < off {
			return false
		}
		hash, gl, err := ComputeGuardHash(f, off)
		if err != nil {
			return false
		}
		switch reason {
		case GenReasonConflict:
			if (entry.Inode == 0 || fid.Ino == entry.Inode) &&
				(wantLen == 0 || hash == wantHash) {
				restartOffset = off
				restartGuardHash = hash
				restartGuardLen = gl
				return true
			}
		default:
			if wantLen > 0 && hash == wantHash {
				restartOffset = off
				restartGuardHash = hash
				restartGuardLen = gl
				return true
			}
		}
		return false
	}

	if tryPreserve(entry.RebaselineOffset, entry.RebaselineGuardHash, entry.RebaselineGuardLen) {
		var last [1]byte
		if n, rerr := f.ReadAt(last[:], restartOffset-1); rerr == nil && n == 1 && last[0] != '\n' {
			skipToNextNL = true
		}
	} else if entry.Baseline {
		tryPreserve(entry.BaselineOffset, entry.BaselineGuardHash, entry.BaselineGuardLen)
	}

	entry.Generation++
	entry.GenerationStartOffset = int64Ptr(restartOffset)
	entry.RawCommittedOffset = restartOffset
	entry.ParsedOffset = restartOffset
	entry.GuardHash = restartGuardHash
	entry.GuardLen = restartGuardLen
	entry.Pending = nil
	entry.SkipToNextNewline = skipToNextNL
	entry.Rejected = false
	entry.RejectedReason = ""
	entry.RejectedRunID = ""
	entry.Inode = fid.Ino
	entry.Device = fid.Dev
	entry.FileID = fid.String()
	entry.LastGenerationReason = reason
	if countFailure {
		entry.ConsecutiveFailures++
	}
	if errMsg != "" {
		entry.LastError = errMsg
	}
	entry.UpdatedAt = &now
	if err := SaveState(s.cfg.HomeDir, st); err != nil {
		return err
	}
	res.GenerationBumps++
	return nil
}

func computePostCommitGuard(r io.ReaderAt, byteStart int64, raw []byte) (string, int, error) {
	if len(raw) >= GuardWindowBytes || byteStart <= 0 {
		h, n := ComputeGuardHashFromBytes(nil, raw)
		return h, n, nil
	}
	need := int(min(byteStart, int64(GuardWindowBytes-len(raw))))
	prefix := make([]byte, need)
	n, err := r.ReadAt(prefix, byteStart-int64(need))
	if err != nil && !errors.Is(err, io.EOF) {
		return "", 0, err
	}
	if n != need {
		return "", 0, io.ErrUnexpectedEOF
	}
	h, gl := ComputeGuardHashFromBytes(prefix, raw)
	return h, gl, nil
}

func detectGenerationBump(f *os.File, entry *ArtifactState, current fileID, fileSize int64) (string, error) {
	if entry.RawCommittedOffset <= 0 {
		return "", nil
	}
	if fileSize < entry.RawCommittedOffset {
		return GenReasonTruncated, nil
	}
	// Compare inode only (not st_dev), so a broker reading over virtiofs/NFS
	// with a different st_dev does not spuriously bump generation.
	if entry.Inode != 0 && current.Ino != entry.Inode {
		return GenReasonInodeChanged, nil
	}
	if entry.GuardLen > 0 {
		hash, _, err := ComputeGuardHash(f, entry.RawCommittedOffset)
		if err != nil {
			return "", err
		}
		if hash != entry.GuardHash {
			return GenReasonGuardMismatch, nil
		}
	}
	return "", nil
}

func mergeDeferredPrefixes(existing []string, refused []RefusedPath) []string {
	if len(existing) == 0 && len(refused) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(existing)+len(refused))
	for _, p := range existing {
		p = strings.Trim(p, "/")
		if p != "" {
			set[p] = struct{}{}
		}
	}
	for _, r := range refused {
		p := strings.Trim(r.NativeRelativePath, "/")
		if p != "" {
			set[p] = struct{}{}
		}
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func matchesDeferredBaselinePrefix(rel string, prefixes []string) bool {
	for _, p := range prefixes {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

func pruneResolvedDeferredPrefixes(prefixes []string, refused []RefusedPath) []string {
	if len(prefixes) == 0 {
		return nil
	}
	var keep []string
	for _, p := range prefixes {
		stillRefused := false
		for _, r := range refused {
			rp := strings.Trim(r.NativeRelativePath, "/")
			if rp == "" {
				continue
			}
			if rp == p || strings.HasPrefix(p, rp+"/") || strings.HasPrefix(rp, p+"/") {
				stillRefused = true
				break
			}
		}
		if stillRefused {
			keep = append(keep, p)
		}
	}
	return keep
}
