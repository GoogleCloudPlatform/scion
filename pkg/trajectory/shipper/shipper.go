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
	"errors"
	"fmt"
	"os"
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
	// advance and the segment will be retried on a future round.
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
	// run_cap_exceeded. The shipper records truncated=true and stops shipping
	// for this run.
	OutcomeRunCapExceeded ShipOutcome = "run_cap_exceeded"
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
	MaxSegmentBytes int64

	// MaxRoundBytes caps total raw bytes shipped in a single RunRound call
	// (default 64 MiB).
	MaxRoundBytes int64

	// LockTimeout bounds how long Baseline/RunRound/ResumeCatchup wait for the
	// exclusive state.lock (default 10s).
	LockTimeout time.Duration

	// Uploader ships prepared segments. Required for RunRound and ResumeCatchup;
	// may be nil if only Baseline is called.
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
}

// Baseline initializes state.json under the exclusive lock if it does not yet
// exist, recording current EOF (or 0 when BackfillExisting is true) for every
// discovered artifact so pre-existing history is never shipped. Calling
// Baseline when state.json is already baselined is a safe no-op.
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

	st, refused, err := s.initBaselineLocked(st)
	if err != nil {
		return nil, err
	}
	return &BaselineResult{
		AlreadyInitialized: false,
		ArtifactsTracked:   len(st.Artifacts),
		RefusedPaths:       refused,
	}, nil
}

func (s *Shipper) initBaselineLocked(existing *State) (*State, []RefusedPath, error) {
	disc, err := Discover(s.cfg.HomeDir, s.spec)
	if err != nil {
		return nil, nil, err
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

	for _, art := range disc.Artifacts {
		if _, tracked := st.Artifacts[art.NativeRelativePath]; tracked {
			continue
		}
		entry, err := s.baselineArtifact(art, st.OriginHubID, now)
		if err != nil {
			// Fault isolation: skip unreadable artifact during baseline rather
			// than aborting the whole home baseline.
			continue
		}
		st.Artifacts[art.NativeRelativePath] = entry
	}

	if err := SaveState(s.cfg.HomeDir, st); err != nil {
		return nil, disc.Refused, err
	}
	return st, disc.Refused, nil
}

func (s *Shipper) baselineArtifact(art DiscoveredArtifact, originHubID string, now time.Time) (*ArtifactState, error) {
	f, err := os.Open(art.AbsPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
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
		FileID:             fid,
		Generation:         0,
		RawCommittedOffset: offset,
		ParsedOffset:       0,
		GuardHash:          guardHash,
		GuardLen:           guardLen,
		Baseline:           true,
		BaselineOffset:     offset,
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
		st, refused, err = s.initBaselineLocked(st)
		if err != nil {
			return nil, err
		}
		res.RefusedPaths = refused
	}

	if st.OriginHubID == "" && s.cfg.OriginHubID != "" {
		st.OriginHubID = s.cfg.OriginHubID
	}

	if st.Truncated {
		res.Truncated = true
		return res, nil
	}

	disc, err := Discover(s.cfg.HomeDir, s.spec)
	if err != nil {
		return nil, err
	}
	res.RefusedPaths = disc.Refused
	res.ArtifactsScanned = len(disc.Artifacts)

	remainingBudget := s.cfg.MaxRoundBytes
	if opts.MaxBytes > 0 {
		remainingBudget = opts.MaxBytes
	}

	resolvedHome, err := pathcanon.Resolve(s.cfg.HomeDir)
	if err != nil {
		return nil, fmt.Errorf("resolve home directory %s: %w", s.cfg.HomeDir, err)
	}

	for _, art := range disc.Artifacts {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if remainingBudget <= 0 {
			res.BudgetExhausted = true
			break
		}

		stopRound, err := s.shipArtifactLocked(ctx, resolvedHome, st, art, opts.Final, &remainingBudget, res)
		if err != nil {
			return res, err
		}
		if stopRound {
			break
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
	final bool,
	remainingBudget *int64,
	res *RoundResult,
) (stopRound bool, fatalErr error) {
	within, err := pathcanon.Within(resolvedHome, art.AbsPath)
	if err != nil || !within {
		res.ArtifactErrors[art.NativeRelativePath] = fmt.Errorf("artifact path outside home: %v", err)
		return false, nil
	}

	f, err := os.Open(art.AbsPath)
	if err != nil {
		res.ArtifactErrors[art.NativeRelativePath] = fmt.Errorf("open artifact: %w", err)
		return false, nil
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		res.ArtifactErrors[art.NativeRelativePath] = fmt.Errorf("stat artifact: %w", err)
		return false, nil
	}
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
			FileID:             fid,
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

	// Detect truncation, inode replacement, or guard-hash mismatch.
	if bumpReason, err := detectGenerationBump(f, entry, fid, fileSize); err != nil {
		res.ArtifactErrors[art.NativeRelativePath] = fmt.Errorf("verify guard: %w", err)
		return false, nil
	} else if bumpReason != "" {
		now := s.now().UTC()
		entry.Generation++
		entry.RawCommittedOffset = 0
		entry.GuardHash = ""
		entry.GuardLen = 0
		entry.FileID = fid
		entry.LastGenerationReason = bumpReason
		entry.UpdatedAt = &now
		if err := SaveState(s.cfg.HomeDir, st); err != nil {
			return true, err
		}
		res.GenerationBumps++
	} else if entry.FileID == "" {
		entry.FileID = fid
	}

	for *remainingBudget > 0 {
		if err := ctx.Err(); err != nil {
			return true, err
		}

		raw, byteEnd, incompleteTail, ok, err := PlanSegment(
			f,
			fileSize,
			entry.RawCommittedOffset,
			s.cfg.MaxSegmentBytes,
			*remainingBudget,
			final,
		)
		if err != nil {
			res.ArtifactErrors[art.NativeRelativePath] = fmt.Errorf("plan segment: %w", err)
			return false, nil
		}
		if !ok {
			if fileSize > entry.RawCommittedOffset && *remainingBudget < s.cfg.MaxSegmentBytes {
				res.BudgetExhausted = true
			}
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
			guardHash, guardLen, err := ComputeGuardHash(f, byteEnd)
			if err != nil {
				res.ArtifactErrors[art.NativeRelativePath] = fmt.Errorf("compute guard hash: %w", err)
				return false, nil
			}
			entry.RawCommittedOffset = byteEnd
			entry.GuardHash = guardHash
			entry.GuardLen = guardLen
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
			entry.Generation++
			entry.RawCommittedOffset = 0
			entry.GuardHash = ""
			entry.GuardLen = 0
			entry.LastGenerationReason = GenReasonConflict
			entry.ConsecutiveFailures++
			entry.LastError = msg
			entry.UpdatedAt = &now
			if err := SaveState(s.cfg.HomeDir, st); err != nil {
				return true, err
			}
			res.GenerationBumps++
			res.ArtifactErrors[art.NativeRelativePath] = fmt.Errorf("segment conflict: %s", msg)
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
			st.TruncatedReason = msg
			entry.Truncated = true
			entry.UpdatedAt = &now
			if err := SaveState(s.cfg.HomeDir, st); err != nil {
				return true, err
			}
			res.Truncated = true
			return true, nil

		default:
			// OutcomeRetryable or empty outcome with error.
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

func detectGenerationBump(f *os.File, entry *ArtifactState, currentFileID string, fileSize int64) (string, error) {
	if entry.RawCommittedOffset <= 0 {
		return "", nil
	}
	if fileSize < entry.RawCommittedOffset {
		return GenReasonTruncated, nil
	}
	if entry.FileID != "" && currentFileID != entry.FileID {
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
