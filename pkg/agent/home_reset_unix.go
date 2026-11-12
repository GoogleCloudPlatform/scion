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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/util/pathcanon"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// resetAgentHome moves agentHome aside, in its own parent, as
// removingHomePrefix+resetID, and records resetID in the agent directory's
// homeResetRecordFile. Reprovision calls it after every precondition has
// passed and before ProvisionAgent composes the fresh home.
//
// It is idempotent for a retry of the same reset: a missing home is not an
// error, and when the moved-aside name already exists the rename is
// skipped. A home that is a symlink, or not a directory, is refused with
// ErrReprovisionRefused before anything changes.
func resetAgentHome(agentDir, agentHome, resetID string) error {
	if !api.ValidResetHomeID(resetID) {
		return fmt.Errorf("%w: invalid home reset ID %q", ErrReprovisionRefused, resetID)
	}
	if agentDir == "" || agentHome == "" || filepath.Base(agentHome) != "home" {
		return fmt.Errorf("%w: cannot reset agent home %q", ErrReprovisionRefused, agentHome)
	}
	homeParent := filepath.Dir(agentHome)
	aside := filepath.Join(homeParent, removingHomePrefix+resetID)
	if ok, err := pathcanon.Within(homeParent, aside); err != nil || !ok {
		return fmt.Errorf("%w: moved-aside home %s is not inside %s", ErrReprovisionRefused, aside, homeParent)
	}

	fi, err := os.Lstat(agentHome)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Nothing to move: a retry after the move, or a home never created.
	case err != nil:
		return fmt.Errorf("reset agent home: %w", err)
	case fi.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%w: agent home %s is a symbolic link; it cannot be reset", ErrReprovisionRefused, agentHome)
	case !fi.IsDir():
		return fmt.Errorf("%w: agent home %s is not a directory", ErrReprovisionRefused, agentHome)
	default:
		if _, err := os.Lstat(aside); err == nil {
			// A retry of the same reset: the old home is already aside, and
			// agentHome is what the earlier attempt started to compose.
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("reset agent home: %w", err)
		}
		// Same parent, so a single rename on one filesystem. The reset ID
		// is unique to this reset, and the name was just checked absent.
		if err := os.Rename(agentHome, aside); err != nil {
			return fmt.Errorf("reset agent home: move %s aside: %w", agentHome, err)
		}
	}
	return writeHomeResetRecord(agentDir, resetID)
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
