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
