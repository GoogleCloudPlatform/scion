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

package maintenance

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/version/update"
)

// UpdateStateFileName is the update-state file under the install root. It
// is the contract between the hub and the boot-failure guard
// (rollback.sh): the guard swaps current and previous back only on the
// booleans in it, and never compares versions itself.
const UpdateStateFileName = "update-state.json"

// maxUpdateStateSize caps how much of the update-state file is read.
const maxUpdateStateSize = 4096

// UpdateStateRunIDDeploy is the runId deploy.sh (scion-layout.sh) writes
// for a release it installs; there is no update run for it.
const UpdateStateRunIDDeploy = "deploy.sh"

// UpdateState is the content of <install_root>/update-state.json:
//
//   - the hub that installs a release writes Target/From/RunID with
//     Verified=false, Migrated=false, PreviousSafe=true just before its
//     self-restart (deploy.sh writes the same with RunID "deploy.sh");
//   - the new binary, when it is Target, sets Migrated=true and computes
//     PreviousSafe at boot, before its first migration statement
//     (MarkUpdateStateBooting);
//   - boot reconciliation sets Verified=true once the hub runs Target
//     (MarkUpdateStateVerified).
//
// The guard rolls back only while Verified is false, and refuses when
// Migrated is true and PreviousSafe is not.
type UpdateState struct {
	Target   string `json:"target"`
	From     string `json:"from"`
	RunID    string `json:"runId"`
	Verified bool   `json:"verified"`
	Migrated bool   `json:"migrated"`
	// PreviousSafe is true when From is at or above the DowngradeFloor of
	// the binary that last computed it (the target, at boot): rolling back
	// to From is then safe after the target's migrations.
	PreviousSafe bool `json:"previous_safe"`
	// FromPublishedAt is the publish date of From (RFC 3339), written by the
	// installing hub when it knows it. It is only a hint for the
	// PreviousSafe computation of a preview or stable From against a
	// nightly floor; the guard ignores it.
	FromPublishedAt string `json:"from_published_at,omitempty"`
}

// ReadUpdateState reads <root>/update-state.json. It returns nil, nil when
// there is none. A file that is not a regular file (a symlink is not
// followed), larger than maxUpdateStateSize, or not one JSON object is an
// error.
func ReadUpdateState(root string) (*UpdateState, error) {
	if root == "" {
		return nil, nil
	}
	path := filepath.Join(root, UpdateStateFileName)
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (%s)", path, before.Mode().Type())
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, opened) {
		return nil, fmt.Errorf("%s changed while it was read", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxUpdateStateSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxUpdateStateSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxUpdateStateSize)
	}
	var st UpdateState
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &st); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return &st, nil
}

// WriteUpdateState replaces <root>/update-state.json with st atomically: a
// new temporary file in root (mode 0644, created exclusively) is renamed
// over it. The rename replaces a symlink at the path rather than writing
// through it.
func WriteUpdateState(root string, st UpdateState) error {
	if root == "" {
		return errors.New("no install root")
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	path := filepath.Join(root, UpdateStateFileName)
	tmp := path + ".tmp-" + hex.EncodeToString(suffix[:])
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	if serr := f.Sync(); err == nil {
		err = serr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	syncDir(root)
	return nil
}

// syncDir fsyncs directory dir so a rename in it survives a power loss.
// Every write of the file (WriteUpdateState) calls it, the verified write
// included. That one matters most: if its rename were lost, the previous
// file (matching target, verified=false) would let the guard swap a
// healthy release back. On Linux, where the guard runs, a directory fsync
// is supported. It is best-effort only because some platforms (Windows)
// cannot fsync a directory, and those have no boot-failure guard.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// PreviousSafe reports whether rolling back to from is safe for a binary
// whose downgrade floor is floor: from is at or above the floor, by the
// same comparison as the floor check of update-binary (update.BelowFloor).
// fromPublished dates a preview or stable from against a nightly floor
// (zero when unknown). An order that cannot be decided (a git build, an
// undated cross-channel from) is not safe.
func PreviousSafe(from string, fromPublished time.Time, floor string) bool {
	below, known := update.BelowFloor(floor, update.Ref{Version: from, Date: fromPublished})
	return known && !below
}

// MarkUpdateStateBooting runs at hub startup before the database is opened
// (and so before any migration statement). When the update-state file
// names this binary (running) as its target it sets Migrated=true (a crash
// partway through the migrations counts as migrated) and recomputes
// PreviousSafe against this binary's floor, then writes the file. It
// returns what it did, for the caller to log; a missing file or another
// target leaves everything alone.
func MarkUpdateStateBooting(root, running, floor string) (changed bool, st *UpdateState, err error) {
	st, err = ReadUpdateState(root)
	if err != nil || st == nil || running == "" || st.Target != running {
		return false, st, err
	}
	var published time.Time
	if st.FromPublishedAt != "" {
		published, _ = time.Parse(time.RFC3339, st.FromPublishedAt)
	}
	st.Migrated = true
	st.PreviousSafe = PreviousSafe(st.From, published, floor)
	return true, st, WriteUpdateState(root, *st)
}

// MarkUpdateStateVerified sets Verified=true when the update-state file
// names running as its target and is not verified yet. It is called by
// boot reconciliation once the hub runs the target, for an update run and
// for a deploy.sh install (runId "deploy.sh") alike.
func MarkUpdateStateVerified(root, running string) (changed bool, err error) {
	st, err := ReadUpdateState(root)
	if err != nil || st == nil || running == "" || st.Target != running || st.Verified {
		return false, err
	}
	st.Verified = true
	return true, WriteUpdateState(root, *st)
}
