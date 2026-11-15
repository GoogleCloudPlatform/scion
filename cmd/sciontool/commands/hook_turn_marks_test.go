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

package commands

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// `sciontool hook` records the turn-start marker and the pending error for
// init's watchdogs. Paths point into a temp dir; no Hub is configured.
func TestProcessHookData_WritesTurnMarks(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	scrubScionEnv(t)
	setTestLogPath(t, filepath.Join(tmpDir, "agent.log"))
	oldTurn, oldPending := turnEventsFilePath, pendingHarnessErrorFilePath
	turnEventsFilePath = filepath.Join(tmpDir, "scion-turn-events")
	pendingHarnessErrorFilePath = filepath.Join(tmpDir, "scion-last-harness-error.json")
	oldDialect := hookDialect
	hookDialect = "claude"
	t.Cleanup(func() {
		turnEventsFilePath, pendingHarnessErrorFilePath = oldTurn, oldPending
		hookDialect = oldDialect
	})

	send := func(ev map[string]interface{}) {
		t.Helper()
		data, err := json.Marshal(ev)
		require.NoError(t, err)
		require.NoError(t, processHookData(data))
	}

	send(map[string]interface{}{"hook_event_name": "SessionStart", "session_id": "s", "source": "startup"})
	_, ok := handlers.ReadLastTurnEventAt(turnEventsFilePath)
	assert.False(t, ok, "session-start is not a turn-start event")

	send(map[string]interface{}{"hook_event_name": "UserPromptSubmit", "session_id": "s", "prompt": "go"})
	_, ok = handlers.ReadLastTurnEventAt(turnEventsFilePath)
	assert.True(t, ok, "prompt-submit did not touch the turn marker")
	_, ok = handlers.ReadPendingHarnessError(pendingHarnessErrorFilePath)
	assert.False(t, ok)
}
