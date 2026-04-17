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
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFormatScheduleDuration(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		expected string
	}{
		{"1 second", time.Second, "1 second"},
		{"30 seconds", 30 * time.Second, "30 seconds"},
		{"1 minute", time.Minute, "1 minute"},
		{"5 minutes", 5 * time.Minute, "5 minutes"},
		{"1 hour", time.Hour, "1 hour"},
		{"3 hours", 3 * time.Hour, "3 hours"},
		{"1 day", 24 * time.Hour, "1 day"},
		{"7 days", 7 * 24 * time.Hour, "7 days"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatScheduleDuration(tt.duration)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFormatScheduleTime(t *testing.T) {
	t.Run("pending event in future shows relative time", func(t *testing.T) {
		future := time.Now().Add(30 * time.Minute)
		result := formatScheduleTime(future, "pending")
		assert.Contains(t, result, "in ")
		assert.Contains(t, result, "minute")
	})

	t.Run("pending event in past shows now", func(t *testing.T) {
		past := time.Now().Add(-1 * time.Minute)
		result := formatScheduleTime(past, "pending")
		assert.Equal(t, "now", result)
	})

	t.Run("fired event shows relative past time", func(t *testing.T) {
		past := time.Now().Add(-5 * time.Minute)
		result := formatScheduleTime(past, "fired")
		assert.Contains(t, result, "ago")
	})
}

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

	t.Run("missing type", func(t *testing.T) {
		scheduleType = ""
		scheduleIn = "30m"
		err := runScheduleCreate(nil, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "--type is required")
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

// ============================================================================
// Workflow schedule validation tests (Phase 4a)
// ============================================================================

func TestScheduleCreateWorkflowRunValidation(t *testing.T) {
	// Save and restore all flags touched in the tests.
	save := func() (string, string, string, string, string, string, string) {
		return scheduleType, scheduleIn, scheduleAt, scheduleAgent, scheduleMessage, scheduleWorkflow, scheduleInputsFile
	}
	restore := func(typ, in, at, agent, msg, wf, inp string) {
		scheduleType = typ
		scheduleIn = in
		scheduleAt = at
		scheduleAgent = agent
		scheduleMessage = msg
		scheduleWorkflow = wf
		scheduleInputsFile = inp
	}
	typ, in, at, agent, msg, wf, inp := save()
	defer restore(typ, in, at, agent, msg, wf, inp)

	t.Run("workflow_run requires --workflow", func(t *testing.T) {
		scheduleType = "workflow_run"
		scheduleIn = "30m"
		scheduleAt = ""
		scheduleAgent = ""
		scheduleMessage = ""
		scheduleWorkflow = "" // missing
		scheduleInputsFile = ""
		err := runScheduleCreate(nil, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "--workflow is required")
	})

	t.Run("workflow_run rejects --agent", func(t *testing.T) {
		scheduleType = "workflow_run"
		scheduleIn = "1h"
		scheduleAt = ""
		scheduleAgent = "some-agent" // must not be set
		scheduleMessage = ""
		scheduleWorkflow = "some.yaml"
		scheduleInputsFile = ""
		err := runScheduleCreate(nil, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "--agent and --message cannot be used with --workflow")
	})

	t.Run("workflow_run rejects --message", func(t *testing.T) {
		scheduleType = "workflow_run"
		scheduleIn = "1h"
		scheduleAt = ""
		scheduleAgent = ""
		scheduleMessage = "hello" // must not be set
		scheduleWorkflow = "some.yaml"
		scheduleInputsFile = ""
		err := runScheduleCreate(nil, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "--agent and --message cannot be used with --workflow")
	})

	t.Run("message type rejects --workflow", func(t *testing.T) {
		scheduleType = "message"
		scheduleIn = "30m"
		scheduleAt = ""
		scheduleAgent = "worker-1"
		scheduleMessage = "hi"
		scheduleWorkflow = "some.yaml" // must not be set for message type
		scheduleInputsFile = ""
		err := runScheduleCreate(nil, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "--workflow cannot be used with event type")
	})

	t.Run("workflow_run reads yaml file and propagates error on missing file", func(t *testing.T) {
		scheduleType = "workflow_run"
		scheduleIn = "30m"
		scheduleAt = ""
		scheduleAgent = ""
		scheduleMessage = ""
		scheduleWorkflow = "/nonexistent/path/flow.duck.yaml"
		scheduleInputsFile = ""
		err := runScheduleCreate(nil, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to read workflow file")
	})
}

func TestScheduleCreateRecurringWorkflowRunValidation(t *testing.T) {
	save := func() (string, string, string, string, string, string, string) {
		return scheduleType, scheduleName, scheduleCron, scheduleAgent, scheduleMessage, scheduleWorkflow, scheduleInputsFile
	}
	restore := func(typ, name, cron, agent, msg, wf, inp string) {
		scheduleType = typ
		scheduleName = name
		scheduleCron = cron
		scheduleAgent = agent
		scheduleMessage = msg
		scheduleWorkflow = wf
		scheduleInputsFile = inp
	}
	typ, name, cron, agent, msg, wf, inp := save()
	defer restore(typ, name, cron, agent, msg, wf, inp)

	t.Run("workflow_run requires --workflow", func(t *testing.T) {
		scheduleType = "workflow_run"
		scheduleName = "my-sched"
		scheduleCron = "0 * * * *"
		scheduleAgent = ""
		scheduleMessage = ""
		scheduleWorkflow = ""
		scheduleInputsFile = ""
		err := runScheduleCreateRecurring(nil, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "--workflow is required")
	})

	t.Run("workflow_run rejects --agent", func(t *testing.T) {
		scheduleType = "workflow_run"
		scheduleName = "my-sched"
		scheduleCron = "0 * * * *"
		scheduleAgent = "bad-agent"
		scheduleMessage = ""
		scheduleWorkflow = "flow.yaml"
		scheduleInputsFile = ""
		err := runScheduleCreateRecurring(nil, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "--agent and --message cannot be used with --workflow")
	})
}
