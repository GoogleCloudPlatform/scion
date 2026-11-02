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
	"encoding/json"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		err := scheduleCreateArgs(nil, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported event type")
	})

	t.Run("missing timing", func(t *testing.T) {
		scheduleType = "message"
		scheduleIn = ""
		scheduleAt = ""
		err := scheduleCreateArgs(nil, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "either --in or --at is required")
	})

	t.Run("mutually exclusive timing", func(t *testing.T) {
		scheduleType = "message"
		scheduleIn = "30m"
		scheduleAt = "2026-03-18T15:00:00Z"
		err := scheduleCreateArgs(nil, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "mutually exclusive")
	})

	t.Run("unsupported type", func(t *testing.T) {
		scheduleType = "invalid"
		scheduleIn = "30m"
		scheduleAt = ""
		err := scheduleCreateArgs(nil, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported event type")
	})

	t.Run("message missing agent", func(t *testing.T) {
		scheduleType = "message"
		scheduleIn = "30m"
		scheduleAt = ""
		scheduleAgent = ""
		scheduleMessage = "hello"
		err := scheduleCreateArgs(nil, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "--agent is required")
	})

	t.Run("message missing message", func(t *testing.T) {
		scheduleType = "message"
		scheduleIn = "30m"
		scheduleAt = ""
		scheduleAgent = "worker-1"
		scheduleMessage = ""
		err := scheduleCreateArgs(nil, nil)
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

func TestScheduleWhen(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	t.Cleanup(clitime.SetNow(func() time.Time { return now }))

	tests := []struct {
		name   string
		t      time.Time
		status string
		want   string
	}{
		{"pending overdue reads now", now.Add(-5 * time.Minute), "pending", "now"},
		{"pending long overdue reads now", now.Add(-3 * 24 * time.Hour), "pending", "now"},
		{"pending due exactly now", now, "pending", "now"},
		{"pending future", now.Add(5 * time.Minute), "pending", "in 5m"},
		{"pending future under a minute", now.Add(30 * time.Second), "pending", "in <1m"},
		{"fired past", now.Add(-2 * time.Hour), "fired", "2h ago"},
		{"cancelled past", now.Add(-5 * time.Minute), "cancelled", "5m ago"},
		{"active future next run", now.Add(3 * time.Hour), "active", "in 3h"},
		{"paused future next run", now.Add(25 * time.Hour), "paused", "in 1d"},
		{"pending zero time", time.Time{}, "pending", "never"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, scheduleWhen(tc.t, tc.status))
		})
	}
}

func TestScheduleStatusLabel(t *testing.T) {
	assert.Equal(t, "active", scheduleStatusLabel("active", ""))
	assert.Equal(t, "paused", scheduleStatusLabel("paused", ""))
	assert.Equal(t, "paused (creator_deleted)", scheduleStatusLabel("paused", "creator_deleted"))
	assert.Equal(t, "paused (cron_zone_unsupported)", scheduleStatusLabel("paused", "cron_zone_unsupported"))
}

// TestPrintScheduleDetail_StatusReason: the detail view prints the system
// pause reason, and prints no Reason line when there is none.
func TestPrintScheduleDetail_StatusReason(t *testing.T) {
	var sched hubclient.Schedule
	require.NoError(t, json.Unmarshal([]byte(
		`{"id":"s1","name":"nightly","cronExpr":"0 3 * * *","eventType":"message","status":"paused","statusReason":"creator_deleted"}`,
	), &sched))
	assert.Equal(t, "creator_deleted", sched.StatusReason)

	out := captureStdout(t, func() { printScheduleDetail(&sched) })
	assert.Contains(t, out, "  Status:     paused\n")
	assert.Contains(t, out, "  Reason:     creator_deleted\n")

	sched.StatusReason = ""
	out = captureStdout(t, func() { printScheduleDetail(&sched) })
	assert.NotContains(t, out, "Reason:")
}

// TestBuildCreateRecurringRequest_TimeZone: --time-zone is passed through to
// the request unchanged (the hub validates it), and is omitted when unset.
func TestBuildCreateRecurringRequest_TimeZone(t *testing.T) {
	origName, origCron, origZone := scheduleName, scheduleCron, scheduleTimeZone
	origType, origAgent, origMessage := scheduleType, scheduleAgent, scheduleMessage
	defer func() {
		scheduleName, scheduleCron, scheduleTimeZone = origName, origCron, origZone
		scheduleType, scheduleAgent, scheduleMessage = origType, origAgent, origMessage
	}()
	scheduleName, scheduleCron = "digest", "0 7 * * *"
	scheduleType, scheduleAgent, scheduleMessage = "message", "lead", "digest"

	scheduleTimeZone = "America/Los_Angeles"
	req := buildCreateRecurringRequest()
	assert.Equal(t, "America/Los_Angeles", req.TimeZone)
	assert.Equal(t, "0 7 * * *", req.CronExpr)
	body, err := json.Marshal(req)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"timeZone":"America/Los_Angeles"`)

	scheduleTimeZone = ""
	body, err = json.Marshal(buildCreateRecurringRequest())
	require.NoError(t, err)
	assert.NotContains(t, string(body), "timeZone", "an unset zone is omitted (UTC)")

	flag := scheduleCreateRecurringCmd.Flags().Lookup("time-zone")
	require.NotNil(t, flag, "create-recurring has --time-zone")
	assert.Equal(t, "", flag.DefValue)
}

// TestScheduleZoneLabels: the detail view always names the zone, and the
// list's cron column names it only when it is not UTC.
func TestScheduleZoneLabels(t *testing.T) {
	assert.Equal(t, "UTC", scheduleZoneLabel(""))
	assert.Equal(t, "Europe/London", scheduleZoneLabel("Europe/London"))
	assert.Equal(t, "0 9 * * *", scheduleCronLabel("0 9 * * *", ""))
	assert.Equal(t, "0 9 * * * (Europe/London)", scheduleCronLabel("0 9 * * *", "Europe/London"))

	var sched hubclient.Schedule
	require.NoError(t, json.Unmarshal([]byte(
		`{"id":"s1","name":"nightly","cronExpr":"0 3 * * *","timeZone":"Asia/Tokyo","eventType":"message","status":"active"}`,
	), &sched))
	out := captureStdout(t, func() { printScheduleDetail(&sched) })
	assert.Contains(t, out, "  Zone:       Asia/Tokyo\n")
	sched.TimeZone = ""
	out = captureStdout(t, func() { printScheduleDetail(&sched) })
	assert.Contains(t, out, "  Zone:       UTC\n")
}
