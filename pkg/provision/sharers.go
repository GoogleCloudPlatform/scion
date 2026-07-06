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

package provision

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
)

// sharerMarker is the on-disk JSON shape stored per shared branch.
type sharerMarker struct {
	Branch       string   `json:"branch"`
	WorktreePath string   `json:"worktreePath"`
	Sharers      []string `json:"sharers"`
}

const sharerDir = "scion-sharers"

// sharerPath returns the marker file path for a branch under the base repo.
func sharerPath(base, branch string) string {
	return filepath.Join(base, ".git", sharerDir, sanitizeBranchName(branch)+".json")
}

// readMarker loads the marker file for a branch. Returns nil (no error) when
// the file does not exist.
func readMarker(path string) (*sharerMarker, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m sharerMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// writeMarkerAtomic writes the marker via a temp file + rename to avoid torn
// reads. The caller MUST hold the per-project advisory lock / provision mutex.
func writeMarkerAtomic(path string, m *sharerMarker) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	// Unique temp file (not a static path+".tmp") so concurrent writers don't
	// clobber each other's temp data before the atomic rename.
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// RegisterSharer adds agentID to the sharer list for the given branch
// worktree. The call is idempotent: re-registering an already-present agent is
// a no-op. worktreePath is recorded (and kept) so that teardown can locate the
// worktree directory even after the last sharer unregisters.
//
// Callers MUST hold the per-project advisory lock / provision mutex.
func RegisterSharer(base, branch, worktreePath, agentID string) error {
	p := sharerPath(base, branch)
	m, err := readMarker(p)
	if err != nil {
		return err
	}
	if m == nil {
		m = &sharerMarker{Branch: branch, WorktreePath: worktreePath}
	}
	if worktreePath != "" {
		m.WorktreePath = worktreePath
	}
	if !slices.Contains(m.Sharers, agentID) {
		m.Sharers = append(m.Sharers, agentID)
	}
	return writeMarkerAtomic(p, m)
}

// UnregisterSharer removes agentID from the sharer list for the given branch.
// It returns the remaining sharers and the recorded worktreePath. When the
// sharer list becomes empty the marker file is deleted, but worktreePath is
// still returned so the caller can remove the worktree directory.
//
// Unregistering an agent that is not in the list is a no-op (returns the
// current state). If no marker exists, remaining is nil and worktreePath is "".
//
// Callers MUST hold the per-project advisory lock / provision mutex.
func UnregisterSharer(base, branch, agentID string) (remaining []string, worktreePath string, err error) {
	p := sharerPath(base, branch)
	m, err := readMarker(p)
	if err != nil {
		return nil, "", err
	}
	if m == nil {
		return nil, "", nil
	}
	m.Sharers = slices.DeleteFunc(m.Sharers, func(s string) bool { return s == agentID })
	if len(m.Sharers) == 0 {
		if rerr := os.Remove(p); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			return nil, m.WorktreePath, rerr
		}
		return []string{}, m.WorktreePath, nil
	}
	if err := writeMarkerAtomic(p, m); err != nil {
		return nil, m.WorktreePath, err
	}
	return m.Sharers, m.WorktreePath, nil
}

// ListSharers returns the current sharer agent IDs and worktreePath for a
// branch. If no marker exists, sharers is nil and worktreePath is "".
func ListSharers(base, branch string) ([]string, string, error) {
	p := sharerPath(base, branch)
	m, err := readMarker(p)
	if err != nil {
		return nil, "", err
	}
	if m == nil {
		return nil, "", nil
	}
	return m.Sharers, m.WorktreePath, nil
}

// FindBranchForAgent scans all marker files under base to find which branch
// (and worktree path) agentID is sharing. Returns found=false when the agent
// is not present in any marker.
func FindBranchForAgent(base, agentID string) (branch, worktreePath string, found bool, err error) {
	dir := filepath.Join(base, ".git", sharerDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		m, err := readMarker(filepath.Join(dir, e.Name()))
		if err != nil {
			// A single corrupted/unreadable marker must not block the whole
			// scan (and thus all agent deletions). Skip it and keep looking;
			// dir-level failures are still returned above.
			slog.Warn("FindBranchForAgent: skipping unreadable sharer marker",
				"file", e.Name(), "error", err)
			continue
		}
		if m != nil && slices.Contains(m.Sharers, agentID) {
			return m.Branch, m.WorktreePath, true, nil
		}
	}
	return "", "", false, nil
}
