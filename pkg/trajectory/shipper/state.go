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
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
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

// ArtifactKey is the logical identity of an artifact generation tracked in state.
type ArtifactKey struct {
	NativeRelativePath string
	FileID             string
	Generation         int
}

// State is the durable state persisted to <home>/.scion/trajectory-shipper/state.json.
// Unknown top-level JSON fields are preserved in Extra across load/save cycles.
type State struct {
	Version          int                       `json:"version"`
	Harness          string                    `json:"harness,omitempty"`
	OriginHubID      string                    `json:"origin_hub_id,omitempty"`
	Baselined        bool                      `json:"baselined"`
	BaselinedAt      *time.Time                `json:"baselined_at,omitempty"`
	BackfillExisting bool                      `json:"backfill_existing,omitempty"`
	Truncated        bool                      `json:"truncated,omitempty"`
	TruncatedReason  string                    `json:"truncated_reason,omitempty"`
	Artifacts        map[string]*ArtifactState `json:"artifacts"`

	// Extra preserves unknown JSON fields for forward compatibility.
	Extra map[string]json.RawMessage `json:"-"`
}

// ArtifactState holds durable per-artifact watermark and guard metadata.
// Unknown JSON fields are preserved in Extra across load/save cycles.
type ArtifactState struct {
	NativeRelativePath   string       `json:"native_relative_path"`
	ArtifactKind         ArtifactKind `json:"artifact_kind"`
	FileID               string       `json:"file_id"`
	Generation           int          `json:"generation"`
	RawCommittedOffset   int64        `json:"raw_committed_offset"`
	ParsedOffset         int64        `json:"parsed_offset"`
	GuardHash            string       `json:"guard_hash,omitempty"`
	GuardLen             int          `json:"guard_len,omitempty"`
	Baseline             bool         `json:"baseline"`
	BaselineOffset       int64        `json:"baseline_offset,omitempty"`
	OriginHubID          string       `json:"origin_hub_id,omitempty"`
	ConsecutiveFailures  int          `json:"consecutive_failures"`
	LastError            string       `json:"last_error,omitempty"`
	LastGenerationReason string       `json:"last_generation_reason,omitempty"`
	Truncated            bool         `json:"truncated"`
	UpdatedAt            *time.Time   `json:"updated_at,omitempty"`

	// Extra preserves unknown JSON fields for forward compatibility.
	Extra map[string]json.RawMessage `json:"-"`
}

// Key returns the logical (native_relative_path, file_id, generation) key for a.
func (a *ArtifactState) Key() ArtifactKey {
	return ArtifactKey{
		NativeRelativePath: a.NativeRelativePath,
		FileID:             a.FileID,
		Generation:         a.Generation,
	}
}

type stateAlias State

var knownStateFields = map[string]struct{}{
	"version":           {},
	"harness":           {},
	"origin_hub_id":     {},
	"baselined":         {},
	"baselined_at":      {},
	"backfill_existing": {},
	"truncated":         {},
	"truncated_reason":  {},
	"artifacts":         {},
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
	"native_relative_path":   {},
	"artifact_kind":          {},
	"file_id":                {},
	"generation":             {},
	"raw_committed_offset":   {},
	"parsed_offset":          {},
	"guard_hash":             {},
	"guard_len":              {},
	"baseline":               {},
	"baseline_offset":        {},
	"origin_hub_id":          {},
	"consecutive_failures":   {},
	"last_error":             {},
	"last_generation_reason": {},
	"truncated":              {},
	"updated_at":             {},
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

// LoadState reads state.json from homeDir through privatefs. If the state
// directory or state.json does not exist, it returns (nil, false, nil).
func LoadState(homeDir string) (*State, bool, error) {
	stateDir := StateDir(homeDir)
	tree, err := privatefs.OpenTree(stateDir, privatefs.TreeOptions{CreateAnchor: false})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("open shipper state dir %s: %w", stateDir, err)
	}
	defer func() { _ = tree.Close() }()

	data, err := tree.ReadPrivate(StateFileName, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read shipper state %s: %w", StatePath(homeDir), err)
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

// SaveState atomically writes st to state.json using privatefs (temp file +
// fsync + rename with owner-only permissions).
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

	stateDir := StateDir(homeDir)
	tree, err := privatefs.OpenTree(stateDir, privatefs.TreeOptions{CreateAnchor: true})
	if err != nil {
		return fmt.Errorf("open shipper state dir %s: %w", stateDir, err)
	}
	defer func() { _ = tree.Close() }()

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal shipper state: %w", err)
	}
	data = append(data, '\n')

	tmp, _, err := tree.CreateTemp("", "state-*.tmp", privatefs.Private)
	if err != nil {
		return fmt.Errorf("create temp shipper state file: %w", err)
	}

	cleanup := true
	defer func() {
		if cleanup {
			_ = tree.RemoveOpen(tmp)
		}
		_ = tmp.Close()
	}()

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp shipper state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("fsync temp shipper state file: %w", err)
	}
	if err := tree.RenameOpen(tmp, StateFileName); err != nil {
		return fmt.Errorf("rename shipper state file: %w", err)
	}
	cleanup = false
	return nil
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
	h := fnv.New64a()
	_, _ = h.Write(buf)
	return fmt.Sprintf("%016x", h.Sum64()), n, nil
}
