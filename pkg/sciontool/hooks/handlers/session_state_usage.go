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

package handlers

import (
	"encoding/json"
	"fmt"
	"os"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
)

// AddUsage adds natively derived usage (telemetry.SessionUsage) to the open
// session in the state file. The init daemon calls it for each increment the
// native usage deriver records, because harnesses whose usage source is
// native (Claude, Copilot, Codex, Gemini CLI) report usage only through
// their own telemetry, never on hook events.
//
// It takes the same lock as the hook processes, so it neither loses nor
// is lost to a concurrent hook's update, and it follows the same rules as
// CloseOpenSession: nothing is followed through a symlink, the lock file is
// never created, and the state is rewritten in place so the file keeps its
// workload ownership. It reports whether the usage was added. Usage is not
// added, without an error, when there is no state file (no hook has
// persisted the session yet, or the session already ended and was
// reported), when the session is not open, or when it is a closed
// tombstone.
func (s *FileSessionState) AddUsage(u telemetry.SessionUsage) (bool, error) {
	if u.IsZero() {
		return false, nil
	}
	added := false
	err := s.withLockedStateNoFollow(syscall.O_RDWR, func(_ int, _ string, f *os.File, file sessionStateFile) error {
		if file.Closed || !file.Aggregator.Open {
			return nil
		}
		agg := telemetry.NewAggregator()
		agg.RestoreState(file.Aggregator)
		agg.RecordUsage(u)
		data, err := json.Marshal(sessionStateFile{Version: sessionStateVersion, Aggregator: agg.State()})
		if err != nil {
			return fmt.Errorf("encoding state: %w", err)
		}
		if err := writeSessionStateInPlace(f, data); err != nil {
			return fmt.Errorf("writing state: %w", err)
		}
		added = true
		return nil
	})
	return added, err
}
