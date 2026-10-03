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

package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestScheduleCreateValidation(t *testing.T) {
	// Save and restore flags
	origType := scheduleType
	origIn := scheduleIn
	origAt := scheduleAt
	origAgent := scheduleAgent
	origMessage := scheduleMessage
	defer func() {
		scheduleType = origType
		scheduleIn = origIn
		scheduleAt = origAt
		scheduleAgent = origAgent
		scheduleMessage = origMessage
	}()

	t.Run("empty type rejected", func(t *testing.T) {
		scheduleType = ""
		scheduleIn = "30m"
		err := runScheduleCreate(nil, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported event type")
	})

	t.Run("missing timing", func(t *testing.T) {
		scheduleType = "message"
		scheduleIn = ""
		scheduleAt = ""
		err := runScheduleCreate(nil, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "either --in or --at is required")
	})

	t.Run("mutually exclusive timing", func(t *testing.T) {
		scheduleType = "message"
		scheduleIn = "30m"
		scheduleAt = "2026-03-18T15:00:00Z"
		err := runScheduleCreate(nil, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "mutually exclusive")
	})

	t.Run("unsupported type", func(t *testing.T) {
		scheduleType = "invalid"
		scheduleIn = "30m"
		scheduleAt = ""
		err := runScheduleCreate(nil, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported event type")
	})

	t.Run("message missing agent", func(t *testing.T) {
		scheduleType = "message"
		scheduleIn = "30m"
		scheduleAt = ""
		scheduleAgent = ""
		scheduleMessage = "hello"
		err := runScheduleCreate(nil, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "--agent is required")
	})

	t.Run("message missing message", func(t *testing.T) {
		scheduleType = "message"
		scheduleIn = "30m"
		scheduleAt = ""
		scheduleAgent = "worker-1"
		scheduleMessage = ""
		err := runScheduleCreate(nil, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "--message is required")
	})
}

func TestScheduleCommandStructure(t *testing.T) {
	// Verify the command group is correctly set up
	assert.Equal(t, "schedule", scheduleCmd.Use)

	// Verify subcommands are registered
	subcommands := scheduleCmd.Commands()
	names := make([]string, len(subcommands))
	for i, cmd := range subcommands {
		names[i] = cmd.Use
	}

	assert.Contains(t, names, "list")
	assert.Contains(t, names, "get <id>")
	assert.Contains(t, names, "cancel <id>")
	assert.Contains(t, names, "create")
}
