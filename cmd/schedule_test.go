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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/spf13/cobra"
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

func newScheduleTestHubContext(t *testing.T, handler http.HandlerFunc, cred hubsync.CredentialKind) *HubContext {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return &HubContext{
		Client:         client,
		Endpoint:       srv.URL,
		ProjectID:      "proj-1",
		CredentialKind: cred,
	}
}

func TestScheduleList_CallerScopeNotice(t *testing.T) {
	origFormat, origListType, origStatus, origType := outputFormat, scheduleListType, scheduleStatus, scheduleType
	t.Cleanup(func() {
		outputFormat, scheduleListType, scheduleStatus, scheduleType = origFormat, origListType, origStatus, origType
	})
	scheduleListType, scheduleStatus, scheduleType = "", "", ""

	sampleSched := hubclient.Schedule{
		ID:        "8b4f6ec0-7d2a-45be-9199-265afce2148c",
		Name:      "my-retro",
		CronExpr:  "0 7 * * *",
		EventType: "message",
		Status:    "active",
	}

	t.Run("agent caller non-empty text and json", func(t *testing.T) {
		hubCtx := newScheduleTestHubContext(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/projects/proj-1/scheduled-events":
				_ = json.NewEncoder(w).Encode(hubclient.ListScheduledEventsResponse{
					Scope:      hubclient.ScheduleListScopeCaller,
					Events:     []hubclient.ScheduledEvent{},
					ServerTime: time.Now().UTC(),
				})
			case "/api/v1/projects/proj-1/schedules":
				_ = json.NewEncoder(w).Encode(hubclient.ListSchedulesResponse{
					Scope:      hubclient.ScheduleListScopeCaller,
					Schedules:  []hubclient.Schedule{sampleSched},
					TotalCount: 1,
					ServerTime: time.Now().UTC(),
				})
			default:
				http.NotFound(w, r)
			}
		}, hubsync.CredentialKindHubToken) // bearer token works via server Scope="caller" too

		outputFormat = ""
		out := captureStdout(t, func() {
			require.NoError(t, scheduleListViaHub(context.Background(), hubCtx, "proj-1"))
		})
		assert.Contains(t, out, "held by this agent")
		assert.Contains(t, out, "other project schedules are hidden")
		assert.Contains(t, out, "RECURRING SCHEDULES")
		assert.Contains(t, out, "my-retro")

		outputFormat = "json"
		jsonOut := captureStdout(t, func() {
			require.NoError(t, scheduleListViaHub(context.Background(), hubCtx, "proj-1"))
		})
		var parsed map[string]any
		require.NoError(t, json.Unmarshal([]byte(jsonOut), &parsed))
		assert.Equal(t, hubclient.ScheduleListScopeCaller, parsed["scope"])
	})

	t.Run("agent caller empty list fallback via CredentialKindAgentToken", func(t *testing.T) {
		hubCtx := newScheduleTestHubContext(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/projects/proj-1/scheduled-events":
				_ = json.NewEncoder(w).Encode(hubclient.ListScheduledEventsResponse{
					Events:     []hubclient.ScheduledEvent{},
					ServerTime: time.Now().UTC(),
				})
			case "/api/v1/projects/proj-1/schedules":
				_ = json.NewEncoder(w).Encode(hubclient.ListSchedulesResponse{
					Schedules:  []hubclient.Schedule{},
					ServerTime: time.Now().UTC(),
				})
			default:
				http.NotFound(w, r)
			}
		}, hubsync.CredentialKindAgentToken)

		outputFormat = ""
		out := captureStdout(t, func() {
			require.NoError(t, scheduleListViaHub(context.Background(), hubCtx, "proj-1"))
		})
		assert.Contains(t, out, "No scheduled events or recurring schedules held by this agent")
		assert.Contains(t, out, "other project schedules are hidden")
	})

	t.Run("user caller has no agent scope notice", func(t *testing.T) {
		var scheds []hubclient.Schedule
		hubCtx := newScheduleTestHubContext(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/projects/proj-1/scheduled-events":
				_ = json.NewEncoder(w).Encode(hubclient.ListScheduledEventsResponse{
					Events:     []hubclient.ScheduledEvent{},
					ServerTime: time.Now().UTC(),
				})
			case "/api/v1/projects/proj-1/schedules":
				_ = json.NewEncoder(w).Encode(hubclient.ListSchedulesResponse{
					Schedules:  scheds,
					ServerTime: time.Now().UTC(),
				})
			default:
				http.NotFound(w, r)
			}
		}, hubsync.CredentialKindOAuth)

		outputFormat = ""
		emptyOut := captureStdout(t, func() {
			require.NoError(t, scheduleListViaHub(context.Background(), hubCtx, "proj-1"))
		})
		assert.Equal(t, "No scheduled events or recurring schedules found.\n", emptyOut)

		scheds = []hubclient.Schedule{sampleSched}
		nonEmptyOut := captureStdout(t, func() {
			require.NoError(t, scheduleListViaHub(context.Background(), hubCtx, "proj-1"))
		})
		assert.NotContains(t, nonEmptyOut, "held by this agent")
		assert.Contains(t, nonEmptyOut, "RECURRING SCHEDULES")

		outputFormat = "json"
		jsonOut := captureStdout(t, func() {
			require.NoError(t, scheduleListViaHub(context.Background(), hubCtx, "proj-1"))
		})
		assert.NotContains(t, jsonOut, `"scope"`)
	})
}

func TestScheduleGet_FullUUIDSurfaces403InsteadOf404(t *testing.T) {
	origFormat := outputFormat
	outputFormat = ""
	t.Cleanup(func() { outputFormat = origFormat })

	const fullUUID = "8b4f6ec0-7d2a-45be-9199-265afce2148c"
	const forbiddenMsg = "Schedule or scheduled event is not visible to this agent (agent callers can only access schedules and events they hold)"

	t.Run("event 404 + schedule 403 surfaces schedule 403", func(t *testing.T) {
		hubCtx := newScheduleTestHubContext(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/projects/proj-1/scheduled-events/" + fullUUID:
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"Scheduled event not found"}}`))
			case "/api/v1/projects/proj-1/schedules/" + fullUUID:
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"` + forbiddenMsg + `"}}`))
			default:
				http.NotFound(w, r)
			}
		}, hubsync.CredentialKindAgentToken)

		err := scheduleGetViaHub(context.Background(), hubCtx, "proj-1", fullUUID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not visible to this agent")
		assert.NotContains(t, err.Error(), "not found")
	})

	t.Run("event 403 + schedule 404 surfaces event 403", func(t *testing.T) {
		hubCtx := newScheduleTestHubContext(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/projects/proj-1/scheduled-events/" + fullUUID:
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"` + forbiddenMsg + `"}}`))
			case "/api/v1/projects/proj-1/schedules/" + fullUUID:
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"Schedule not found"}}`))
			default:
				http.NotFound(w, r)
			}
		}, hubsync.CredentialKindAgentToken)

		err := scheduleGetViaHub(context.Background(), hubCtx, "proj-1", fullUUID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not visible to this agent")
		assert.NotContains(t, err.Error(), "not found")
	})

	t.Run("both 404 from older hub adds agent caller-scope hint", func(t *testing.T) {
		hubCtx := newScheduleTestHubContext(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"Resource not found"}}`))
		}, hubsync.CredentialKindAgentToken)

		err := scheduleGetViaHub(context.Background(), hubCtx, "proj-1", fullUUID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "hidden from agent lists")
	})
}

func TestResolveScheduleID_CallerScopeHintAndNonUUID36Chars(t *testing.T) {
	// 36-char schedule name that is NOT a valid UUID.
	const nonUUID36 = "process-improvement-lead-retro-night"
	require.Len(t, nonUUID36, 36)

	var listHits int
	agentHubCtx := newScheduleTestHubContext(t, func(w http.ResponseWriter, r *http.Request) {
		listHits++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects/proj-1/scheduled-events":
			_ = json.NewEncoder(w).Encode(hubclient.ListScheduledEventsResponse{
				Scope:      hubclient.ScheduleListScopeCaller,
				Events:     []hubclient.ScheduledEvent{},
				ServerTime: time.Now().UTC(),
			})
		case "/api/v1/projects/proj-1/schedules":
			_ = json.NewEncoder(w).Encode(hubclient.ListSchedulesResponse{
				Scope:      hubclient.ScheduleListScopeCaller,
				Schedules:  []hubclient.Schedule{},
				ServerTime: time.Now().UTC(),
			})
		default:
			http.NotFound(w, r)
		}
	}, hubsync.CredentialKindAgentToken)

	_, _, err := resolveScheduleID(context.Background(), agentHubCtx, "proj-1", nonUUID36, "")
	require.Error(t, err)
	assert.Greater(t, listHits, 0, "36-char non-UUID must go through prefix lookup rather than bypassing as UUID")
	assert.Contains(t, err.Error(), "held by this agent")
	assert.Contains(t, err.Error(), "not visible")
	assert.Contains(t, err.Error(), "ID or ID prefix")

	for _, targetType := range []string{"schedule", "event"} {
		_, _, err := resolveScheduleID(context.Background(), agentHubCtx, "proj-1", "8b4f6ec0", targetType)
		require.Error(t, err, targetType)
		assert.Contains(t, err.Error(), "held by this agent")
	}

	userHubCtx := newScheduleTestHubContext(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"events": []any{}, "schedules": []any{}, "serverTime": time.Now().UTC()})
	}, hubsync.CredentialKindOAuth)

	_, _, err = resolveScheduleID(context.Background(), userHubCtx, "proj-1", "8b4f6ec0", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no schedule or event found with ID prefix "8b4f6ec0"`)
	assert.NotContains(t, err.Error(), "held by this agent")
}

func TestWrapScheduleHubError(t *testing.T) {
	agentCtx := &HubContext{CredentialKind: hubsync.CredentialKindAgentToken}
	userCtx := &HubContext{CredentialKind: hubsync.CredentialKindOAuth}

	// Older Hub generic 409 gets hint for agent, not for user, and newer Hub hint is not duplicated.
	conflictOld := newScheduleTestHubContext(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"Resource already exists"}}`))
	}, hubsync.CredentialKindAgentToken)
	_, rawConflict := conflictOld.Client.Schedules("proj-1").Create(context.Background(), &hubclient.CreateScheduleRequest{Name: "x"})
	require.Error(t, rawConflict)

	wrappedAgent := wrapScheduleHubError(agentCtx, rawConflict)
	assert.Contains(t, wrappedAgent.Error(), "scion schedule list")
	assert.Contains(t, wrappedAgent.Error(), "not visible to this agent")

	wrappedUser := wrapScheduleHubError(userCtx, rawConflict)
	assert.NotContains(t, wrappedUser.Error(), "not visible to this agent")

	// Specific scope/policy 403 (not bare "Insufficient permissions") is preserved without mislabeling.
	scope403 := newScheduleTestHubContext(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"missing required scope agent:create"}}`))
	}, hubsync.CredentialKindAgentToken)
	_, rawScope403 := scope403.Client.Schedules("proj-1").Resume(context.Background(), "8b4f6ec0-7d2a-45be-9199-265afce2148c")
	require.Error(t, rawScope403)
	assert.NotContains(t, wrapScheduleHubError(agentCtx, rawScope403).Error(), "agent callers can only access")
}

func saveScheduleFlags(t *testing.T) {
	t.Helper()
	origType := scheduleType
	origStatus := scheduleStatus
	origIn := scheduleIn
	origAt := scheduleAt
	origAgent := scheduleAgent
	origMessage := scheduleMessage
	origInterrupt := scheduleInterrupt
	origWake := scheduleWake
	origTimeout := scheduleTimeout
	origName := scheduleName
	origCron := scheduleCron
	origTimeZone := scheduleTimeZone
	origListType := scheduleListType
	origFormat := outputFormat
	t.Cleanup(func() {
		scheduleType = origType
		scheduleStatus = origStatus
		scheduleIn = origIn
		scheduleAt = origAt
		scheduleAgent = origAgent
		scheduleMessage = origMessage
		scheduleInterrupt = origInterrupt
		scheduleWake = origWake
		scheduleTimeout = origTimeout
		scheduleName = origName
		scheduleCron = origCron
		scheduleTimeZone = origTimeZone
		scheduleListType = origListType
		outputFormat = origFormat
	})
}

// resetScheduleFlagSet rebuilds c's FlagSet via c.ResetFlags() + register so
// pflag's sticky argsLenAtDash is reset to -1 between parses.
func resetScheduleFlagSet(t *testing.T, c *cobra.Command, register func(*cobra.Command)) {
	t.Helper()
	c.ResetFlags()
	register(c)
}

func TestScheduleCreateExecValidation_CobraParsing(t *testing.T) {
	saveScheduleFlags(t)
	t.Cleanup(func() {
		resetScheduleFlagSet(t, scheduleCreateCmd, registerScheduleCreateFlags)
		resetScheduleFlagSet(t, scheduleCreateRecurringCmd, registerScheduleCreateRecurringFlags)
	})

	parseAndValidateCreate := func(t *testing.T, cliArgs []string) ([]string, error) {
		t.Helper()
		resetScheduleFlagSet(t, scheduleCreateCmd, registerScheduleCreateFlags)
		if err := scheduleCreateCmd.ParseFlags(cliArgs); err != nil {
			return nil, err
		}
		posArgs := scheduleCreateCmd.Flags().Args()
		if err := scheduleCreateArgs(scheduleCreateCmd, posArgs); err != nil {
			return nil, err
		}
		return extractExecCommandArgs(scheduleCreateCmd, posArgs)
	}

	t.Run("exec preserves child flags after dash", func(t *testing.T) {
		argv, err := parseAndValidateCreate(t, []string{
			"--type", "exec", "--agent", "worker", "--in", "5m", "--timeout", "55m",
			"--", "sh", "-c", "echo hi", "--unknown-to-scion",
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"sh", "-c", "echo hi", "--unknown-to-scion"}, argv)
		secs, err := parseExecTimeoutSeconds(scheduleTimeout)
		require.NoError(t, err)
		assert.Equal(t, 3300, secs)
	})

	t.Run("exec default timeout omitted as 0", func(t *testing.T) {
		argv, err := parseAndValidateCreate(t, []string{
			"--type", "exec", "--agent", "worker", "--in", "10m", "--", "/bin/true",
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"/bin/true"}, argv)
		secs, err := parseExecTimeoutSeconds(scheduleTimeout)
		require.NoError(t, err)
		assert.Equal(t, 0, secs)
	})

	invalidCases := []struct {
		name    string
		cliArgs []string
		wantErr string
	}{
		{
			name:    "exec missing dash and command",
			cliArgs: []string{"--type", "exec", "--agent", "worker", "--in", "5m"},
			wantErr: "'--'",
		},
		{
			name:    "exec positional arg without dash",
			cliArgs: []string{"--type", "exec", "--agent", "worker", "--in", "5m", "/bin/true"},
			wantErr: "'--'",
		},
		{
			name:    "exec positional arg before dash",
			cliArgs: []string{"--type", "exec", "--agent", "worker", "--in", "5m", "stray", "--", "/bin/true"},
			wantErr: "before '--'",
		},
		{
			name:    "exec empty after dash",
			cliArgs: []string{"--type", "exec", "--agent", "worker", "--in", "5m", "--"},
			wantErr: "after '--'",
		},
		{
			name:    "exec empty argv0",
			cliArgs: []string{"--type", "exec", "--agent", "worker", "--in", "5m", "--", ""},
			wantErr: "argv[0]",
		},
		{
			name:    "exec missing agent",
			cliArgs: []string{"--type", "exec", "--in", "5m", "--", "/bin/true"},
			wantErr: "--agent is required",
		},
		{
			name:    "exec rejects --message",
			cliArgs: []string{"--type", "exec", "--agent", "worker", "--in", "5m", "--message", "hi", "--", "/bin/true"},
			wantErr: "--message",
		},
		{
			name:    "exec rejects --interrupt",
			cliArgs: []string{"--type", "exec", "--agent", "worker", "--in", "5m", "--interrupt", "--", "/bin/true"},
			wantErr: "--interrupt",
		},
		{
			name:    "exec rejects --wake",
			cliArgs: []string{"--type", "exec", "--agent", "worker", "--in", "5m", "--wake", "--", "/bin/true"},
			wantErr: "--wake",
		},
		{
			name:    "exec timeout invalid syntax",
			cliArgs: []string{"--type", "exec", "--agent", "worker", "--in", "5m", "--timeout", "60", "--", "/bin/true"},
			wantErr: "--timeout",
		},
		{
			name:    "exec timeout zero",
			cliArgs: []string{"--type", "exec", "--agent", "worker", "--in", "5m", "--timeout", "0s", "--", "/bin/true"},
			wantErr: "--timeout",
		},
		{
			name:    "exec timeout negative",
			cliArgs: []string{"--type", "exec", "--agent", "worker", "--in", "5m", "--timeout", "-5s", "--", "/bin/true"},
			wantErr: "--timeout",
		},
		{
			name:    "exec timeout sub-second",
			cliArgs: []string{"--type", "exec", "--agent", "worker", "--in", "5m", "--timeout", "1500ms", "--", "/bin/true"},
			wantErr: "whole seconds",
		},
		{
			name:    "exec timeout exceeds 3600s",
			cliArgs: []string{"--type", "exec", "--agent", "worker", "--in", "5m", "--timeout", "61m", "--", "/bin/true"},
			wantErr: "3600s",
		},
		{
			name:    "message rejects --timeout",
			cliArgs: []string{"--type", "message", "--agent", "worker", "--in", "5m", "--message", "hi", "--timeout", "60s"},
			wantErr: "--timeout",
		},
		{
			name:    "message rejects dash argv",
			cliArgs: []string{"--type", "message", "--agent", "worker", "--in", "5m", "--message", "hi", "--", "/bin/true"},
			wantErr: "'--'",
		},
	}

	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseAndValidateCreate(t, tc.cliArgs)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}

	t.Run("exec rejects >64 args and NUL byte", func(t *testing.T) {
		many := make([]string, 65)
		for i := range many {
			many[i] = "x"
		}
		_, err := parseAndValidateCreate(t, append([]string{"--type", "exec", "--agent", "worker", "--in", "5m", "--"}, many...))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "64")

		_, err = parseAndValidateCreate(t, []string{"--type", "exec", "--agent", "worker", "--in", "5m", "--", "/bin/echo", "bad\x00arg"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "NUL")
	})
}

func TestScheduleCreateAndRecurringExec_ViaHub(t *testing.T) {
	saveScheduleFlags(t)

	var gotOneShot map[string]any
	var gotRecurring map[string]any
	fireAt := time.Date(2026, 11, 15, 23, 0, 0, 0, time.UTC)

	hubCtx := newScheduleTestHubContext(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/scheduled-events":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotOneShot))
			_ = json.NewEncoder(w).Encode(hubclient.ScheduledEvent{
				ID:        "evt-exec-1",
				ProjectID: "proj-1",
				EventType: "exec",
				FireAt:    fireAt,
				Status:    "pending",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/schedules":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotRecurring))
			_ = json.NewEncoder(w).Encode(hubclient.Schedule{
				ID:        "sch-exec-1",
				ProjectID: "proj-1",
				Name:      "hourly-probe",
				CronExpr:  "0 * * * *",
				TimeZone:  "America/New_York",
				EventType: "exec",
				Status:    "active",
				NextRunAt: &fireAt,
			})
		default:
			http.NotFound(w, r)
		}
	}, hubsync.CredentialKindOAuth)

	resetScheduleFlagSet(t, scheduleCreateCmd, registerScheduleCreateFlags)
	t.Cleanup(func() { resetScheduleFlagSet(t, scheduleCreateCmd, registerScheduleCreateFlags) })
	require.NoError(t, scheduleCreateCmd.ParseFlags([]string{
		"--type", "exec", "--agent", "worker", "--in", "15m", "--timeout", "2m",
		"--", "/usr/local/bin/probe", "--verbose",
	}))
	posArgs := scheduleCreateCmd.Flags().Args()
	require.NoError(t, scheduleCreateArgs(scheduleCreateCmd, posArgs))
	outputFormat = ""
	out := captureStdout(t, func() {
		require.NoError(t, scheduleCreateViaHub(context.Background(), hubCtx, "proj-1", scheduleCreateCmd, posArgs))
	})
	assert.Contains(t, out, "Scheduled event created: evt-exec-1")
	assert.Contains(t, out, "  Type:    exec")
	assert.Equal(t, "exec", gotOneShot["eventType"])
	assert.Equal(t, "worker", gotOneShot["agentName"])
	assert.Equal(t, "15m", gotOneShot["fireIn"])
	assert.Equal(t, []any{"/usr/local/bin/probe", "--verbose"}, gotOneShot["command"])
	assert.Equal(t, float64(120), gotOneShot["timeoutSeconds"])
	assert.NotContains(t, gotOneShot, "message")
	assert.NotContains(t, gotOneShot, "wake")
	assert.NotContains(t, gotOneShot, "interrupt")

	resetScheduleFlagSet(t, scheduleCreateRecurringCmd, registerScheduleCreateRecurringFlags)
	t.Cleanup(func() { resetScheduleFlagSet(t, scheduleCreateRecurringCmd, registerScheduleCreateRecurringFlags) })
	require.NoError(t, scheduleCreateRecurringCmd.ParseFlags([]string{
		"--type", "exec", "--name", "hourly-probe", "--cron", "0 * * * *",
		"--time-zone", "America/New_York", "--agent", "worker",
		"--", "/usr/local/bin/probe",
	}))
	recArgs := scheduleCreateRecurringCmd.Flags().Args()
	require.NoError(t, validateScheduleCreateRecurring(scheduleCreateRecurringCmd, recArgs))
	out = captureStdout(t, func() {
		require.NoError(t, scheduleCreateRecurringViaHub(context.Background(), hubCtx, "proj-1", scheduleCreateRecurringCmd, recArgs))
	})
	assert.Contains(t, out, "Recurring schedule created: sch-exec-1")
	assert.Equal(t, "exec", gotRecurring["eventType"])
	assert.Equal(t, "hourly-probe", gotRecurring["name"])
	assert.Equal(t, "0 * * * *", gotRecurring["cronExpr"])
	assert.Equal(t, "America/New_York", gotRecurring["timeZone"])
	assert.Equal(t, "worker", gotRecurring["agentName"])
	assert.Equal(t, []any{"/usr/local/bin/probe"}, gotRecurring["command"])
	assert.NotContains(t, gotRecurring, "timeoutSeconds", "unset --timeout is omitted so Hub applies 60s default")
}

func TestPrintDetail_ExecPayloadAndRedaction(t *testing.T) {
	t.Run("revealed exec event payload", func(t *testing.T) {
		evt := &hubclient.ScheduledEvent{
			ID:        "evt-exec-rev",
			EventType: "exec",
			Status:    "pending",
			ProjectID: "proj-1",
			Payload:   `{"agentId":"a-1","agentName":"worker","command":["/bin/sh","-c","echo hello world"],"timeoutSeconds":0}`,
		}
		out := captureStdout(t, func() { printEventDetail(evt) })
		assert.Contains(t, out, "  Agent:      worker\n")
		assert.Contains(t, out, `  Command:    ["/bin/sh", "-c", "echo hello world"]`+"\n")
		assert.Contains(t, out, "  Timeout:    60s\n")
		assert.NotContains(t, out, "Wake:")
		assert.NotContains(t, out, "Interrupt:")
	})

	t.Run("redacted exec schedule payload", func(t *testing.T) {
		sched := &hubclient.Schedule{
			ID:        "sch-exec-red",
			Name:      "secret-job",
			CronExpr:  "*/10 * * * *",
			EventType: "exec",
			Status:    "active",
			ProjectID: "proj-1",
			Payload:   `{"agentId":"a-1","agentName":"worker","timeoutSeconds":300,"commandRedacted":true,"argc":3,"commandSha256":"deadbeefcafebabe"}`,
		}
		out := captureStdout(t, func() { printScheduleDetail(sched) })
		assert.Contains(t, out, "  Agent:      worker\n")
		assert.Contains(t, out, "  Command:    <redacted, 3 args, sha256:deadbeefcafebabe>\n")
		assert.Contains(t, out, "  Timeout:    300s\n")
		assert.NotContains(t, out, "Wake:")
		assert.NotContains(t, out, "Interrupt:")
	})

	t.Run("corrupt payload bare redacted fallback and agentId fallback", func(t *testing.T) {
		evt := &hubclient.ScheduledEvent{
			ID:        "evt-corrupt",
			EventType: "exec",
			Status:    "failed",
			ProjectID: "proj-1",
			Payload:   `{"commandRedacted":true}`,
		}
		out := captureStdout(t, func() { printEventDetail(evt) })
		assert.Contains(t, out, "  Command:    <redacted>\n")
		assert.NotContains(t, out, "Timeout:")
		assert.NotContains(t, out, "Agent:")

		evtIDOnly := &hubclient.ScheduledEvent{
			ID:        "evt-id-only",
			EventType: "exec",
			Status:    "pending",
			ProjectID: "proj-1",
			Payload:   `{"agentId":"agent-uuid-1","command":["/bin/true"],"timeoutSeconds":45}`,
		}
		out = captureStdout(t, func() { printEventDetail(evtIDOnly) })
		assert.Contains(t, out, "  Agent:      agent-uuid-1\n")
		assert.Contains(t, out, "  Timeout:    45s\n")
	})
}

func TestScheduleGetAndHistory_ExecSummary(t *testing.T) {
	saveScheduleFlags(t)
	now := time.Date(2026, 11, 15, 12, 10, 0, 0, time.UTC)
	t.Cleanup(clitime.SetNow(func() time.Time { return now }))

	const evtUUID = "11111111-2222-4333-8444-555555555555"
	const schedUUID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

	exitZero := 0
	exitTwo := 2
	durZero := int64(145)
	durTwo := int64(820)
	durTimeout := int64(60000)
	bytesZero := int64(0)
	bytesSome := int64(2048)
	fired1 := now.Add(-2 * time.Minute)
	fired2 := now.Add(-12 * time.Minute)
	fired3 := now.Add(-22 * time.Minute)
	fired4 := now.Add(-32 * time.Minute)

	historyEvents := []hubclient.ScheduledEvent{
		{
			ID:        "h1111111-0000-4000-8000-000000000001",
			EventType: "exec",
			Status:    "failed",
			FiredAt:   &fired1,
			Error:     "exit status 2",
			ExecSummary: &hubclient.ExecSummary{
				Outcome:     "nonzero",
				ExitCode:    &exitTwo,
				DurationMS:  &durTwo,
				OutputBytes: &bytesSome,
				StartedAt:   fired1,
			},
		},
		{
			ID:        "h2222222-0000-4000-8000-000000000002",
			EventType: "exec",
			Status:    "fired",
			FiredAt:   &fired2,
			ExecSummary: &hubclient.ExecSummary{
				Outcome:     "ok",
				ExitCode:    &exitZero,
				DurationMS:  &durZero,
				OutputBytes: &bytesZero,
				StartedAt:   fired2,
			},
		},
		{
			ID:        "h3333333-0000-4000-8000-000000000003",
			EventType: "exec",
			Status:    "failed",
			FiredAt:   &fired3,
			Error:     "timed out after 60s",
			ExecSummary: &hubclient.ExecSummary{
				Outcome:     "timeout",
				DurationMS:  &durTimeout,
				OutputBytes: &bytesSome,
				StartedAt:   fired3,
			},
		},
		{
			ID:        "h4444444-0000-4000-8000-000000000004",
			EventType: "exec",
			Status:    "skipped",
			FiredAt:   &fired4,
			Error:     "target not running",
		},
	}

	hubCtx := newScheduleTestHubContext(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects/proj-1/scheduled-events/" + evtUUID:
			_ = json.NewEncoder(w).Encode(hubclient.ScheduledEvent{
				ID:          evtUUID,
				ProjectID:   "proj-1",
				EventType:   "exec",
				Status:      "fired",
				FireAt:      fired2,
				FiredAt:     &fired2,
				Payload:     `{"agentId":"a-1","agentName":"worker","timeoutSeconds":60,"commandRedacted":true,"argc":2,"commandSha256":"abcd1234"}`,
				ExecSummary: historyEvents[1].ExecSummary,
			})
		case "/api/v1/projects/proj-1/scheduled-events/" + schedUUID:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not found"}}`))
		case "/api/v1/projects/proj-1/schedules/" + schedUUID:
			_ = json.NewEncoder(w).Encode(hubclient.Schedule{
				ID:            schedUUID,
				ProjectID:     "proj-1",
				Name:          "periodic-check",
				CronExpr:      "*/10 * * * *",
				EventType:     "exec",
				Status:        "active",
				LastRunAt:     &fired1,
				LastRunStatus: "failed",
				LastRunError:  "exit status 2",
				RunCount:      4,
				ErrorCount:    2,
				Payload:       `{"agentId":"a-1","agentName":"worker","command":["/usr/bin/check"],"timeoutSeconds":60}`,
			})
		case "/api/v1/projects/proj-1/schedules/" + schedUUID + "/history":
			_ = json.NewEncoder(w).Encode(hubclient.ListScheduledEventsResponse{
				Events:     historyEvents,
				TotalCount: len(historyEvents),
				ServerTime: now,
			})
		default:
			http.NotFound(w, r)
		}
	}, hubsync.CredentialKindOAuth)

	// 1. schedule get <event-id> text & json
	outputFormat = ""
	evtOut := captureStdout(t, func() {
		require.NoError(t, scheduleGetViaHub(context.Background(), hubCtx, "proj-1", evtUUID))
	})
	assert.Contains(t, evtOut, "  Command:    <redacted, 2 args, sha256:abcd1234>\n")
	assert.Contains(t, evtOut, "  Outcome:    ok\n")
	assert.Contains(t, evtOut, "  Exit Code:  0\n")
	assert.Contains(t, evtOut, "  Duration:   145ms\n")
	assert.Contains(t, evtOut, "  Output:     0 B\n")

	outputFormat = "json"
	evtJSON := captureStdout(t, func() {
		require.NoError(t, scheduleGetViaHub(context.Background(), hubCtx, "proj-1", evtUUID))
	})
	assert.Contains(t, evtJSON, `"execSummary"`)
	assert.Contains(t, evtJSON, `"exitCode": 0`)

	// 2. schedule get <schedule-id> shows target, argv, timeout, last outcome and exit code
	outputFormat = ""
	schedOut := captureStdout(t, func() {
		require.NoError(t, scheduleGetViaHub(context.Background(), hubCtx, "proj-1", schedUUID))
	})
	assert.Contains(t, schedOut, "  Agent:      worker\n")
	assert.Contains(t, schedOut, `  Command:    ["/usr/bin/check"]`+"\n")
	assert.Contains(t, schedOut, "  Timeout:    60s\n")
	assert.Contains(t, schedOut, "  Last Outcome: nonzero\n")
	assert.Contains(t, schedOut, "  Last Exit Code: 2\n")

	// 3. schedule history <schedule-id> text & json
	outputFormat = ""
	histOut := captureStdout(t, func() {
		require.NoError(t, scheduleHistoryViaHub(context.Background(), hubCtx, "proj-1", schedUUID))
	})
	assert.Contains(t, histOut, "OUTCOME")
	assert.Contains(t, histOut, "EXIT")
	assert.Contains(t, histOut, "DURATION")
	assert.Contains(t, histOut, "OUTPUT")
	assert.Contains(t, histOut, "h1111111")
	assert.Contains(t, histOut, "nonzero")
	assert.Contains(t, histOut, "820ms")
	assert.Contains(t, histOut, "2048 B")
	assert.Contains(t, histOut, "h2222222")
	assert.Contains(t, histOut, "ok")
	assert.Contains(t, histOut, "145ms")
	assert.Contains(t, histOut, "0 B")
	assert.Contains(t, histOut, "timeout")
	assert.Contains(t, histOut, "target not running")

	outputFormat = "json"
	histJSON := captureStdout(t, func() {
		require.NoError(t, scheduleHistoryViaHub(context.Background(), hubCtx, "proj-1", schedUUID))
	})
	assert.Contains(t, histJSON, `"execSummary"`)

	// 4. Non-exec schedule history keeps its existing 4-column layout
	msgHistOut := captureStdout(t, func() {
		printScheduleHistory([]hubclient.ScheduledEvent{
			{
				ID:        "m1111111-0000-4000-8000-000000000001",
				EventType: "message",
				Status:    "fired",
				FiredAt:   &fired1,
			},
		})
	})
	assert.Contains(t, msgHistOut, "ID")
	assert.Contains(t, msgHistOut, "STATUS")
	assert.NotContains(t, msgHistOut, "OUTCOME")
}
