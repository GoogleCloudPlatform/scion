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
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/pathcanon"
)

// Round trigger reasons passed in RoundOptions.Reason.
const (
	ReasonPeriodic      = "periodic"
	ReasonSessionEnd    = "session_end"
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

	// OutcomeCaptureUnavailable indicates capture is disabled or the experiment
	// is off on the Hub (e.g. 404 / 403 capture_disabled). The shipper stops the
	// round and signals callers to back off rather than retry hot.
	OutcomeCaptureUnavailable ShipOutcome = "capture_unavailable"

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

// Config configures a Shipper instance.
type Config struct {
	// HomeDir is the agent home directory containing native harness artifacts
	// and .scion/trajectory-shipper/. Required.
	HomeDir string

	// RunID identifies the active agent run. Used to scope run-cap truncation
	// so hitting the per-run cap on run A does not disable shipping on run B.
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
	cfg       Config
	spec      HarnessSpec
	now       func() time.Time
	nextStart atomic.Int64
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

// ResetForRun clears run-scoped truncation state when starting a new run_id.
// It is safe to call even if state.json does not yet exist.
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
	if !st.Truncated {
		return nil
	}
	if runID != "" && st.TruncatedRunID == runID {
		return nil
	}
	clearRunTruncation(st)
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

	fid, err := fileIdentity(fi)
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
		NativeRelativePath: art.NativeRelativePath,
		ArtifactKind:       art.Kind,
		Inode:              fid.Ino,
		Device:             fid.Dev,
		FileID:             fid.String(),
		Generation:         0,
		RawCommittedOffset: offset,
		ParsedOffset:       offset,
		GuardHash:          guardHash,
		GuardLen:           guardLen,
		Baseline:           true,
		BaselineOffset:     offset,
		BaselineGuardHash:  guardHash,
		BaselineGuardLen:   guardLen,
		OriginHubID:        originHubID,
		UpdatedAt:          &now,
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
	Reason             string
	ArtifactsScanned   int
	SegmentsShipped    int
	BytesShipped       int64
	GenerationBumps    int
	CaptureUnavailable bool
	Truncated          bool
	BudgetExhausted    bool
	RefusedPaths       []RefusedPath
	ArtifactErrors     map[string]error
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
// and ships pending segments under the exclusive state lock. Watermarks advance
// only after Uploader.Ship returns OutcomeCommitted.
func (s *Shipper) RunRound(ctx context.Context, opts RoundOptions) (*RoundResult, error) {
	if s.cfg.Uploader == nil {
		return nil, errors.New("shipper: Uploader is required for RunRound")
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
	if n > 0 {
		start := int(s.nextStart.Load()) % n
		if start < 0 {
			start = 0
		}
		lastIdx := start
		for i := 0; i < n; i++ {
			idx := (start + i) % n
			lastIdx = idx
			art := disc.Artifacts[idx]
			if err := ctx.Err(); err != nil {
				s.nextStart.Store(int64((lastIdx + 1) % n))
				return res, err
			}
			if remainingBudget <= 0 {
				res.BudgetExhausted = true
				s.nextStart.Store(int64(idx))
				break
			}

			stopRound, err := s.shipArtifactLocked(ctx, resolvedHome, st, art, effectiveMaxSeg, opts.Final, &remainingBudget, res)
			if err != nil {
				s.nextStart.Store(int64((lastIdx + 1) % n))
				return res, err
			}
			if stopRound {
				s.nextStart.Store(int64((lastIdx + 1) % n))
				break
			}
			if remainingBudget <= 0 {
				s.nextStart.Store(int64((idx + 1) % n))
				if i+1 < n {
					res.BudgetExhausted = true
				}
				break
			}
		}
		if remainingBudget > 0 && !res.BudgetExhausted {
			s.nextStart.Store(int64((lastIdx + 1) % n))
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
	if existing, ok := st.Artifacts[art.NativeRelativePath]; ok && existing.Rejected {
		return false, nil
	}

	f, fi, err := openArtifactNoFollow(resolvedHome, art.NativeRelativePath)
	if err != nil {
		res.ArtifactErrors[art.NativeRelativePath] = fmt.Errorf("open artifact: %w", err)
		return false, nil
	}
	defer func() { _ = f.Close() }()

	fid, err := fileIdentity(fi)
	if err != nil {
		res.ArtifactErrors[art.NativeRelativePath] = err
		return false, nil
	}
	fileSize := fi.Size()

	entry, tracked := st.Artifacts[art.NativeRelativePath]
	if !tracked {
		now := s.now().UTC()
		entry = &ArtifactState{
			NativeRelativePath: art.NativeRelativePath,
			ArtifactKind:       art.Kind,
			Inode:              fid.Ino,
			Device:             fid.Dev,
			FileID:             fid.String(),
			Generation:         0,
			RawCommittedOffset: 0,
			ParsedOffset:       0,
			Baseline:           false,
			OriginHubID:        st.OriginHubID,
			UpdatedAt:          &now,
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

	// Complete deferred baseline on first successful open.
	if entry.NeedsBaseline {
		now := s.now().UTC()
		var offset int64
		var guardHash string
		var guardLen int
		if !st.BackfillExisting && !s.cfg.BackfillExisting {
			offset = fileSize
			if offset > 0 {
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
		entry.RawCommittedOffset = offset
		entry.ParsedOffset = offset
		entry.BaselineOffset = offset
		entry.BaselineGuardHash = guardHash
		entry.BaselineGuardLen = guardLen
		entry.GuardHash = guardHash
		entry.GuardLen = guardLen
		entry.NeedsBaseline = false
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
		if entry.Inode == 0 {
			entry.Inode = fid.Ino
		}
		// Keep Device current when broker reads via virtiofs/NFS with a
		// different st_dev on the same inode.
		entry.Device = fid.Dev
		entry.FileID = fid.String()
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
				if fileSize > entry.RawCommittedOffset && *remainingBudget < effectiveMaxSeg {
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
			*remainingBudget -= seg.RawSize

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

	// Preserve pre-baseline privacy: if the artifact was baselined with
	// BaselineOffset > 0 and the file still has intact pre-baseline bytes
	// (size >= BaselineOffset and matching BaselineGuardHash), start the new
	// generation at BaselineOffset rather than 0.
	var restartOffset int64
	var restartGuardHash string
	var restartGuardLen int
	if entry.Baseline && entry.BaselineOffset > 0 && fileSize >= entry.BaselineOffset {
		hash, gl, err := ComputeGuardHash(f, entry.BaselineOffset)
		if err == nil {
			preserve := false
			switch reason {
			case GenReasonConflict:
				if (entry.Inode == 0 || fid.Ino == entry.Inode) &&
					(entry.BaselineGuardLen == 0 || hash == entry.BaselineGuardHash) {
					preserve = true
				}
			default:
				// For inode_changed, guard_mismatch, or partial truncation
				// above BaselineOffset: preserve baseline if the recorded
				// baseline guard still matches.
				if entry.BaselineGuardLen > 0 && hash == entry.BaselineGuardHash {
					preserve = true
				}
			}
			if preserve {
				restartOffset = entry.BaselineOffset
				restartGuardHash = hash
				restartGuardLen = gl
			}
		}
	}

	entry.Generation++
	entry.RawCommittedOffset = restartOffset
	entry.ParsedOffset = restartOffset
	entry.GuardHash = restartGuardHash
	entry.GuardLen = restartGuardLen
	entry.Pending = nil
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
