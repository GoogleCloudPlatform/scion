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
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"path/filepath"
	"time"
)

const (
	// StateVersion is the schema version written to state.json.
	StateVersion = 1

	// StateDirRel is the home-relative directory holding shipper state and lock.
	StateDirRel = ".scion/trajectory-shipper"

	// StateFileName is the filename of the durable shipper state JSON file.
	StateFileName = "state.json"

	// LockFileName is the filename of the exclusive inter-process lock file.
	LockFileName = "state.lock"

	// GuardWindowBytes is the number of trailing bytes before
	// raw_committed_offset hashed with 64-bit FNV-1a to detect rewrites.
	GuardWindowBytes = 64
)

// Generation bump reasons recorded on ArtifactState.LastGenerationReason.
const (
	GenReasonTruncated     = "truncated"
	GenReasonGuardMismatch = "guard_mismatch"
	GenReasonInodeChanged  = "inode_changed"
	GenReasonConflict      = "segment_conflict"
	GenReasonRebaseline    = "rebaseline"
)

// Sentinel errors returned by shipper filesystem safety checks.
var (
	// ErrRootSquashChownRefused is returned when running as root (euid == 0)
	// and fchown on shipper state directories or files is refused (EPERM /
	// EACCES), such as on a root-squashed NFS home export. Callers on such
	// filesystems should run the shipper as the home directory's owner.
	ErrRootSquashChownRefused = errors.New("shipper: chown refused under root-squashed filesystem; run shipper as home owner")

	// ErrHardLinkRefused is returned when running as root (euid == 0) and an
	// artifact leaf has more than one hard link (st_nlink > 1).
	ErrHardLinkRefused = errors.New("shipper: hard-linked artifact refused when running as root")
)

// StateDir returns the absolute path to the shipper state directory under homeDir.
func StateDir(homeDir string) string {
	return filepath.Join(homeDir, filepath.FromSlash(StateDirRel))
}

// StatePath returns the absolute path to the shipper state file under homeDir.
func StatePath(homeDir string) string {
	return filepath.Join(StateDir(homeDir), StateFileName)
}

// LockPath returns the absolute path to the exclusive shipper lock file under homeDir.
func LockPath(homeDir string) string {
	return filepath.Join(StateDir(homeDir), LockFileName)
}

// ArtifactKey is the logical identity of an artifact generation tracked in
// state. Exported as part of the public API for downstream consumers (such as
// P6 broker harvest and P7 normalizers).
type ArtifactKey struct {
	NativeRelativePath string
	Inode              uint64
	Generation         int
}

// PendingSegment records an in-flight segment persisted before Uploader.Ship so
// that a lost ACK or crash followed by file growth replays the exact same byte
// range and raw_sha256 (resolving via idempotent replay rather than 409).
type PendingSegment struct {
	ByteEnd        int64  `json:"byte_end"`
	RawSHA256      string `json:"raw_sha256"`
	IncompleteTail bool   `json:"incomplete_tail"`
}

// State is the durable state persisted to <home>/.scion/trajectory-shipper/state.json.
// Unknown top-level JSON fields are preserved in Extra across load/save cycles.
type State struct {
	Version                  int                       `json:"version"`
	Harness                  string                    `json:"harness,omitempty"`
	OriginHubID              string                    `json:"origin_hub_id,omitempty"`
	Baselined                bool                      `json:"baselined"`
	BaselinedAt              *time.Time                `json:"baselined_at,omitempty"`
	LastRebaselineReason     string                    `json:"last_rebaseline_reason,omitempty"`
	LastRebaselineAt         *time.Time                `json:"last_rebaseline_at,omitempty"`
	RefusedPendingRebaseline bool                      `json:"refused_pending_rebaseline,omitempty"`
	BackfillExisting         bool                      `json:"backfill_existing,omitempty"`
	DeferredBaselinePrefixes []string                  `json:"deferred_baseline_prefixes,omitempty"`
	NextStartPath            string                    `json:"next_start_path,omitempty"`
	Truncated                bool                      `json:"truncated,omitempty"`
	TruncatedRunID           string                    `json:"truncated_run_id,omitempty"`
	TruncatedReason          string                    `json:"truncated_reason,omitempty"`
	Artifacts                map[string]*ArtifactState `json:"artifacts"`

	// Extra preserves unknown JSON fields for forward compatibility.
	Extra map[string]json.RawMessage `json:"-"`
}

// ArtifactState holds durable per-artifact watermark and guard metadata.
// Unknown JSON fields are preserved in Extra across load/save cycles.
type ArtifactState struct {
	NativeRelativePath    string          `json:"native_relative_path"`
	ArtifactKind          ArtifactKind    `json:"artifact_kind"`
	Inode                 uint64          `json:"inode,omitempty"`
	Device                uint64          `json:"device,omitempty"`
	FileID                string          `json:"file_id,omitempty"`
	Generation            int             `json:"generation"`
	GenerationStartOffset *int64          `json:"generation_start_offset,omitempty"`
	RawCommittedOffset    int64           `json:"raw_committed_offset"`
	ParsedOffset          int64           `json:"parsed_offset"`
	GuardHash             string          `json:"guard_hash,omitempty"`
	GuardLen              int             `json:"guard_len,omitempty"`
	Pending               *PendingSegment `json:"pending,omitempty"`
	Baseline              bool            `json:"baseline"`
	BaselineSet           bool            `json:"baseline_set,omitempty"`
	BaselineOffset        int64           `json:"baseline_offset,omitempty"`
	BaselineGuardHash     string          `json:"baseline_guard_hash,omitempty"`
	BaselineGuardLen      int             `json:"baseline_guard_len,omitempty"`
	RebaselineOffset      int64           `json:"rebaseline_offset,omitempty"`
	RebaselineGuardHash   string          `json:"rebaseline_guard_hash,omitempty"`
	RebaselineGuardLen    int             `json:"rebaseline_guard_len,omitempty"`
	NeedsBaseline         bool            `json:"needs_baseline,omitempty"`
	SkipToNextNewline     bool            `json:"skip_to_next_newline,omitempty"`
	OriginHubID           string          `json:"origin_hub_id,omitempty"`
	ConsecutiveFailures   int             `json:"consecutive_failures"`
	LastError             string          `json:"last_error,omitempty"`
	LastGenerationReason  string          `json:"last_generation_reason,omitempty"`
	Rejected              bool            `json:"rejected,omitempty"`
	RejectedReason        string          `json:"rejected_reason,omitempty"`
	RejectedRunID         string          `json:"rejected_run_id,omitempty"`
	Truncated             bool            `json:"truncated"`
	TruncatedRunID        string          `json:"truncated_run_id,omitempty"`
	UpdatedAt             *time.Time      `json:"updated_at,omitempty"`

	// Extra preserves unknown JSON fields for forward compatibility.
	Extra map[string]json.RawMessage `json:"-"`
}

// Key returns the logical (native_relative_path, inode, generation) key for a.
func (a *ArtifactState) Key() ArtifactKey {
	return ArtifactKey{
		NativeRelativePath: a.NativeRelativePath,
		Inode:              a.Inode,
		Generation:         a.Generation,
	}
}

type stateAlias State

var knownStateFields = map[string]struct{}{
	"version":                    {},
	"harness":                    {},
	"origin_hub_id":              {},
	"baselined":                  {},
	"baselined_at":               {},
	"last_rebaseline_reason":     {},
	"last_rebaseline_at":         {},
	"refused_pending_rebaseline": {},
	"backfill_existing":          {},
	"deferred_baseline_prefixes": {},
	"next_start_path":            {},
	"truncated":                  {},
	"truncated_run_id":           {},
	"truncated_reason":           {},
	"artifacts":                  {},
}

// UnmarshalJSON decodes State while preserving unknown fields in Extra.
func (s *State) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var alias stateAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	*s = State(alias)
	if s.Artifacts == nil {
		s.Artifacts = make(map[string]*ArtifactState)
	}
	for k := range knownStateFields {
		delete(raw, k)
	}
	if len(raw) > 0 {
		s.Extra = raw
	}
	return nil
}

// MarshalJSON encodes State along with any preserved unknown fields in Extra.
func (s State) MarshalJSON() ([]byte, error) {
	if s.Artifacts == nil {
		s.Artifacts = make(map[string]*ArtifactState)
	}
	knownBytes, err := json.Marshal(stateAlias(s))
	if err != nil {
		return nil, err
	}
	if len(s.Extra) == 0 {
		return knownBytes, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(knownBytes, &merged); err != nil {
		return nil, err
	}
	for k, v := range s.Extra {
		if _, isKnown := knownStateFields[k]; isKnown {
			continue
		}
		merged[k] = v
	}
	return json.Marshal(merged)
}

type artifactStateAlias ArtifactState

var knownArtifactStateFields = map[string]struct{}{
	"native_relative_path":    {},
	"artifact_kind":           {},
	"inode":                   {},
	"device":                  {},
	"file_id":                 {},
	"generation":              {},
	"generation_start_offset": {},
	"raw_committed_offset":    {},
	"parsed_offset":           {},
	"guard_hash":              {},
	"guard_len":               {},
	"pending":                 {},
	"baseline":                {},
	"baseline_set":            {},
	"baseline_offset":         {},
	"baseline_guard_hash":     {},
	"baseline_guard_len":      {},
	"rebaseline_offset":       {},
	"rebaseline_guard_hash":   {},
	"rebaseline_guard_len":    {},
	"needs_baseline":          {},
	"skip_to_next_newline":    {},
	"origin_hub_id":           {},
	"consecutive_failures":    {},
	"last_error":              {},
	"last_generation_reason":  {},
	"rejected":                {},
	"rejected_reason":         {},
	"rejected_run_id":         {},
	"truncated":               {},
	"truncated_run_id":        {},
	"updated_at":              {},
}

// UnmarshalJSON decodes ArtifactState while preserving unknown fields in Extra.
func (a *ArtifactState) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var alias artifactStateAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	*a = ArtifactState(alias)
	for k := range knownArtifactStateFields {
		delete(raw, k)
	}
	if len(raw) > 0 {
		a.Extra = raw
	}
	return nil
}

// MarshalJSON encodes ArtifactState along with any preserved unknown fields in Extra.
func (a ArtifactState) MarshalJSON() ([]byte, error) {
	knownBytes, err := json.Marshal(artifactStateAlias(a))
	if err != nil {
		return nil, err
	}
	if len(a.Extra) == 0 {
		return knownBytes, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(knownBytes, &merged); err != nil {
		return nil, err
	}
	for k, v := range a.Extra {
		if _, isKnown := knownArtifactStateFields[k]; isKnown {
			continue
		}
		merged[k] = v
	}
	return json.Marshal(merged)
}

// LoadState reads state.json from homeDir through pinned O_NOFOLLOW directory
// descriptors. If the state directory or state.json does not exist, it returns
// (nil, false, nil). On non-Unix platforms it fails closed with
// errors.ErrUnsupported.
func LoadState(homeDir string) (*State, bool, error) {
	data, exists, err := readStateFile(homeDir)
	if err != nil || !exists {
		return nil, false, err
	}

	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, false, fmt.Errorf("parse shipper state %s: %w", StatePath(homeDir), err)
	}
	if st.Artifacts == nil {
		st.Artifacts = make(map[string]*ArtifactState)
	}
	return &st, true, nil
}

// SaveState atomically writes st to state.json through pinned O_NOFOLLOW
// directory descriptors (temp file + fchmod 0600 + fchown to home owner when
// root + fsync + renameat). On non-Unix platforms it fails closed with
// errors.ErrUnsupported.
func SaveState(homeDir string, st *State) error {
	if st == nil {
		return errors.New("state is nil")
	}
	if st.Version == 0 {
		st.Version = StateVersion
	}
	if st.Artifacts == nil {
		st.Artifacts = make(map[string]*ArtifactState)
	}

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal shipper state: %w", err)
	}
	data = append(data, '\n')

	return writeStateFile(homeDir, data)
}

// WithLockedState acquires the exclusive inter-process state lock for homeDir,
// loads state.json (passing a newly initialized empty State if it does not yet
// exist), invokes fn, and atomically saves the state when fn returns save=true
// and err=nil.
//
// This is the supported read-modify-write helper for P7 normalizers advancing
// parsed_offset and P6 broker harvest consumers.
func WithLockedState(
	ctx context.Context,
	homeDir string,
	timeout time.Duration,
	fn func(st *State) (save bool, err error),
) error {
	if homeDir == "" {
		return errors.New("shipper: homeDir is required")
	}
	if fn == nil {
		return errors.New("shipper: callback is required")
	}
	lock, err := acquireStateLock(ctx, homeDir, timeout)
	if err != nil {
		return err
	}
	defer lock.Unlock()

	st, exists, err := LoadState(homeDir)
	if err != nil {
		return err
	}
	if !exists {
		st = &State{
			Version:   StateVersion,
			Artifacts: make(map[string]*ArtifactState),
		}
	}
	save, err := fn(st)
	if err != nil {
		return err
	}
	if !save {
		return nil
	}
	return SaveState(homeDir, st)
}

// ComputeGuardHash computes the 64-bit FNV-1a hex digest of the up to
// GuardWindowBytes (64) bytes immediately preceding offset.
func ComputeGuardHash(r io.ReaderAt, offset int64) (string, int, error) {
	if offset <= 0 {
		return "", 0, nil
	}
	n := int(min(offset, int64(GuardWindowBytes)))
	buf := make([]byte, n)
	readN, err := r.ReadAt(buf, offset-int64(n))
	if err != nil && !errors.Is(err, io.EOF) {
		return "", 0, err
	}
	if readN != n {
		return "", 0, io.ErrUnexpectedEOF
	}
	return hashGuardWindow(buf), n, nil
}

// ComputeGuardHashFromBytes computes the 64-bit FNV-1a guard hash at the end of
// committed using the committed byte slice itself plus any needed trailing
// bytes from prefix (the bytes immediately preceding committed). This avoids
// re-reading the committed range from disk after upload.
func ComputeGuardHashFromBytes(prefix, committed []byte) (string, int) {
	total := len(prefix) + len(committed)
	if total == 0 {
		return "", 0
	}
	if len(committed) >= GuardWindowBytes {
		tail := committed[len(committed)-GuardWindowBytes:]
		return hashGuardWindow(tail), GuardWindowBytes
	}
	needPrefix := GuardWindowBytes - len(committed)
	if needPrefix > len(prefix) {
		needPrefix = len(prefix)
	}
	window := make([]byte, 0, needPrefix+len(committed))
	window = append(window, prefix[len(prefix)-needPrefix:]...)
	window = append(window, committed...)
	return hashGuardWindow(window), len(window)
}

func hashGuardWindow(buf []byte) string {
	h := fnv.New64a()
	_, _ = h.Write(buf)
	return fmt.Sprintf("%016x", h.Sum64())
}
