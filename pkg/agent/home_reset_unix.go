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

//go:build unix

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/util/pathcanon"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// resetAgentHome moves agentHome aside, in its own parent, as
// removingHomePrefix+resetID (with a "-<n>" suffix when that name is
// taken), and records resetID in the agent directory's
// homeResetRecordFile. Reprovision calls it after every precondition has
// passed and before ProvisionAgent composes the fresh home. Both steps run
// under the agent directory's file lock (provision.WithAgentDirLock), the
// lock ProvisionAgentDir takes.
//
// A home that exists is always moved, so a retry of the same reset never
// composes over what an earlier attempt left at the home path. A missing
// home is not an error. A home that is a symlink, or not a directory, is
// refused with ErrReprovisionRefused before anything changes.
//
// Once the home has been moved, a failure (writing the record) carries
// ErrHomeResetApplied; any failure before the move leaves the home intact
// and does not.
func resetAgentHome(ctx context.Context, agentDir, agentHome, resetID string) error {
	if !api.ValidResetHomeID(resetID) {
		return fmt.Errorf("%w: invalid home reset ID %q", ErrReprovisionRefused, resetID)
	}
	if agentDir == "" || agentHome == "" || filepath.Base(agentHome) != "home" {
		return fmt.Errorf("%w: cannot reset agent home %q", ErrReprovisionRefused, agentHome)
	}
	homeParent := filepath.Dir(agentHome)
	return provision.WithAgentDirLock(ctx, agentDir, 0, func() error {
		moved := false
		fi, err := os.Lstat(agentHome)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// Nothing to move: a home never created.
		case err != nil:
			return fmt.Errorf("reset agent home: %w", err)
		case fi.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("%w: agent home %s is a symbolic link; it cannot be reset", ErrReprovisionRefused, agentHome)
		case !fi.IsDir():
			return fmt.Errorf("%w: agent home %s is not a directory", ErrReprovisionRefused, agentHome)
		default:
			aside, err := freeAsideName(homeParent, removingHomePrefix+resetID)
			if err != nil {
				return fmt.Errorf("reset agent home: %w", err)
			}
			// Same parent, so a single rename on one filesystem; the name
			// was just checked absent, under the lock.
			if err := os.Rename(agentHome, aside); err != nil {
				return fmt.Errorf("reset agent home: move %s aside: %w", agentHome, err)
			}
			moved = true
		}
		if err := writeHomeResetRecord(agentDir, resetID); err != nil {
			if moved {
				return homeResetAppliedError{err}
			}
			return err
		}
		return nil
	})
}

// freeAsideName returns the first of dir/base, dir/base-1, dir/base-2, ...
// that does not exist, contained in dir.
func freeAsideName(dir, base string) (string, error) {
	for n := 0; n < 1000; n++ {
		name := base
		if n > 0 {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		candidate := filepath.Join(dir, name)
		if ok, err := pathcanon.Within(dir, candidate); err != nil || !ok {
			return "", fmt.Errorf("moved-aside home %s is not inside %s", candidate, dir)
		}
		if _, err := os.Lstat(candidate); errors.Is(err, fs.ErrNotExist) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("no free name for the moved-aside home in %s", dir)
}

// writeHomeResetRecord writes homeResetRecordFile in agentDir through a
// temporary file and a rename, so a reader never sees a partial record.
func writeHomeResetRecord(agentDir, resetID string) error {
	data, err := json.Marshal(homeResetRecord{ResetID: resetID})
	if err != nil {
		return err
	}
	tree, err := privatefs.OpenTree(agentDir, privatefs.TreeOptions{})
	if err != nil {
		return fmt.Errorf("record home reset: %w", err)
	}
	defer func() { _ = tree.Close() }()
	f, rel, err := tree.CreateTemp("", homeResetRecordFile+".tmp-*", privatefs.Private)
	if err != nil {
		return fmt.Errorf("record home reset: %w", err)
	}
	renamed := false
	defer func() {
		_ = f.Close()
		if !renamed {
			_ = tree.Remove(rel)
		}
	}()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("record home reset: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("record home reset: %w", err)
	}
	if err := tree.Rename(rel, homeResetRecordFile); err != nil {
		return fmt.Errorf("record home reset: %w", err)
	}
	renamed = true
	return nil
}
