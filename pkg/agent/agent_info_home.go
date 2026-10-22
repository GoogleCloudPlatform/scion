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

package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/agenthome"
	"github.com/GoogleCloudPlatform/scion/pkg/agentinfolock"
)

// agent-info.json reads and writes are confined to the agent home
// (pkg/agenthome): the file is read without following a link at the leaf
// or at any directory component (a link there is refused, never read
// through, and a file with more than one hard link is refused on unix), and
// written by a temporary file renamed over the leaf, so a link at the leaf
// is replaced, never written through.

// agentInfoFileName is agent-info.json, in the agent home.
const agentInfoFileName = agenthome.AgentInfoFile

// maxAgentInfoSize bounds an agent-info.json read.
const maxAgentInfoSize = agenthome.MaxAgentInfoSize

// readHomeAgentInfo reads agent-info.json from the agent home at agentHome,
// confined to the home (agenthome.ReadAgentInfo). A missing home or file
// satisfies errors.Is(err, fs.ErrNotExist).
func readHomeAgentInfo(agentHome string) ([]byte, error) {
	return agenthome.ReadAgentInfo(agentHome)
}

// updateHomeAgentInfo performs a read-modify-write of agent-info.json in the
// agent home at agentHome under the agent-info lock, confined to the home.
// modify receives the current content and returns the new content, or nil
// for no write. The file keeps its permission bits. With missingOK, a
// missing home or file is not an error (nothing is written); otherwise the
// read error is returned.
func updateHomeAgentInfo(agentHome string, missingOK bool, modify func(data []byte) ([]byte, error)) error {
	infoPath := filepath.Join(agentHome, agentInfoFileName)
	return agentinfolock.Update(infoPath, agentinfolock.Options{}, func() error {
		home, err := agenthome.Open(agentHome)
		if err != nil {
			if missingOK && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		defer func() { _ = home.Close() }()
		data, mode, err := home.ReadFileMode(agentInfoFileName, maxAgentInfoSize)
		if err != nil {
			if missingOK && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		newData, err := modify(data)
		if err != nil || newData == nil {
			return err
		}
		return writeHomeAgentInfo(home, newData, mode)
	})
}

// writeHomeAgentInfo replaces agent-info.json in home with data (mode
// mode), as writeAgentInfoFile does for a path: the content is synced
// before the rename and the home directory after it. An error wrapping
// ErrAgentInfoNotDurable means the content was replaced but may not
// survive a crash.
func writeHomeAgentInfo(home *agenthome.Home, data []byte, mode os.FileMode) error {
	if err := home.WriteFile(agentInfoFileName, data, mode); err != nil {
		return err
	}
	if err := home.SyncDir("."); err != nil && !isDirSyncUnsupported(err) {
		return fmt.Errorf("%w: sync directory %s: %w", ErrAgentInfoNotDurable, home.Path(), err)
	}
	return nil
}
