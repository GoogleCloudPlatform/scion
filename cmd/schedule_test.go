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

// TestScheduleCreateRequests_Wake: --wake on create and create-recurring
// sets wake on the request (ptone/scion#7734); unset, the field is omitted.
func TestScheduleCreateRequests_Wake(t *testing.T) {
	origType, origAgent, origMessage, origIn := scheduleType, scheduleAgent, scheduleMessage, scheduleIn
	origName, origCron, origWake := scheduleName, scheduleCron, scheduleWake
	defer func() {
		scheduleType, scheduleAgent, scheduleMessage, scheduleIn = origType, origAgent, origMessage, origIn
		scheduleName, scheduleCron, scheduleWake = origName, origCron, origWake
	}()
	scheduleType, scheduleAgent, scheduleMessage, scheduleIn = "message", "lead", "recheck", "5m"
	scheduleName, scheduleCron = "poke", "*/5 * * * *"

	for _, cmdName := range []string{"create", "create-recurring"} {
		c := scheduleCreateCmd
		if cmdName == "create-recurring" {
			c = scheduleCreateRecurringCmd
		}
		flag := c.Flags().Lookup("wake")
		require.NotNil(t, flag, "%s has --wake", cmdName)
		assert.Equal(t, "false", flag.DefValue)
	}

	scheduleWake = true
	assert.True(t, buildCreateEventRequest().Wake)
	assert.True(t, buildCreateRecurringRequest().Wake)
	body, err := json.Marshal(buildCreateEventRequest())
	require.NoError(t, err)
	assert.Contains(t, string(body), `"wake":true`)

	scheduleWake = false
	body, err = json.Marshal(buildCreateEventRequest())
	require.NoError(t, err)
	assert.NotContains(t, string(body), "wake")
	body, err = json.Marshal(buildCreateRecurringRequest())
	require.NoError(t, err)
	assert.NotContains(t, string(body), "wake")
}

// TestPrintEventDetail_WakeAndInterrupt: scion schedule get prints Wake and
// Interrupt lines for message events (including false when omitted from payload)
// and omits them for non-message event types (ptone/scion#7791 d).
func TestPrintEventDetail_WakeAndInterrupt(t *testing.T) {
	t.Run("message event with wake and interrupt true", func(t *testing.T) {
		evt := &hubclient.ScheduledEvent{
			ID:        "evt-1",
			EventType: "message",
			Status:    "pending",
			ProjectID: "proj-1",
			Payload:   `{"agentName":"worker","message":"recheck","wake":true,"interrupt":true}`,
		}
		out := captureStdout(t, func() { printEventDetail(evt) })
		assert.Contains(t, out, "  Agent:      worker\n")
		assert.Contains(t, out, "  Message:    \"recheck\"\n")
		assert.Contains(t, out, "  Wake:       true\n")
		assert.Contains(t, out, "  Interrupt:  true\n")
	})

	t.Run("message event defaults omitted flags to false", func(t *testing.T) {
		evt := &hubclient.ScheduledEvent{
			ID:        "evt-2",
			EventType: "message",
			Status:    "pending",
			ProjectID: "proj-1",
			Payload:   `{"agentName":"worker","message":"recheck"}`,
		}
		out := captureStdout(t, func() { printEventDetail(evt) })
		assert.Contains(t, out, "  Wake:       false\n")
		assert.Contains(t, out, "  Interrupt:  false\n")
	})

	t.Run("dispatch_agent event omits wake and interrupt", func(t *testing.T) {
		evt := &hubclient.ScheduledEvent{
			ID:        "evt-3",
			EventType: "dispatch_agent",
			Status:    "pending",
			ProjectID: "proj-1",
			Payload:   `{"agentName":"worker","template":"default","task":"build"}`,
		}
		out := captureStdout(t, func() { printEventDetail(evt) })
		assert.NotContains(t, out, "Wake:")
		assert.NotContains(t, out, "Interrupt:")
	})
}

// TestPrintScheduleDetail_WakeAndInterrupt: scion schedule get prints Wake and
// Interrupt lines for recurring message schedules and omits them for
// dispatch_agent schedules (ptone/scion#7791 d).
func TestPrintScheduleDetail_WakeAndInterrupt(t *testing.T) {
	t.Run("message schedule with wake true and interrupt false", func(t *testing.T) {
		sched := &hubclient.Schedule{
			ID:        "sched-1",
			Name:      "morning-check",
			CronExpr:  "0 8 * * *",
			EventType: "message",
			Status:    "active",
			ProjectID: "proj-1",
			Payload:   `{"agentName":"watcher","message":"check builds","wake":true}`,
		}
		out := captureStdout(t, func() { printScheduleDetail(sched) })
		assert.Contains(t, out, "  Agent:      watcher\n")
		assert.Contains(t, out, "  Message:    \"check builds\"\n")
		assert.Contains(t, out, "  Wake:       true\n")
		assert.Contains(t, out, "  Interrupt:  false\n")
	})

	t.Run("message schedule with interrupt true and wake omitted", func(t *testing.T) {
		sched := &hubclient.Schedule{
			ID:        "sched-2",
			Name:      "urgent-ping",
			CronExpr:  "*/15 * * * *",
			EventType: "message",
			Status:    "active",
			ProjectID: "proj-1",
			Payload:   `{"agentName":"watcher","message":"urgent","interrupt":true}`,
		}
		out := captureStdout(t, func() { printScheduleDetail(sched) })
		assert.Contains(t, out, "  Wake:       false\n")
		assert.Contains(t, out, "  Interrupt:  true\n")
	})

	t.Run("dispatch_agent schedule omits wake and interrupt", func(t *testing.T) {
		sched := &hubclient.Schedule{
			ID:        "sched-3",
			Name:      "nightly-worker",
			CronExpr:  "0 2 * * *",
			EventType: "dispatch_agent",
			Status:    "active",
			ProjectID: "proj-1",
			Payload:   `{"agentName":"worker","template":"default","task":"nightly"}`,
		}
		out := captureStdout(t, func() { printScheduleDetail(sched) })
		assert.Contains(t, out, "  Template:   default\n")
		assert.NotContains(t, out, "Wake:")
		assert.NotContains(t, out, "Interrupt:")
	})
}
