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
	"strconv"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunConversationMessages_PaginationAndTruncation(t *testing.T) {
	origProject := projectPath
	origFormat := outputFormat
	origLimit, origAll, origJSON, origFull := convMsgLimit, convMsgAll, convMsgJSON, convMsgFull
	origBefore, origAfter := convMsgBefore, convMsgAfter
	t.Cleanup(func() {
		projectPath, outputFormat = origProject, origFormat
		convMsgLimit, convMsgAll, convMsgJSON, convMsgFull = origLimit, origAll, origJSON, origFull
		convMsgBefore, convMsgAfter = origBefore, origAfter
	})

	convID := "11111111-1111-1111-1111-111111111111"
	now := time.Now().UTC()
	var seenCursors []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/conversations/"+convID+"/messages" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		cursor := r.URL.Query().Get("cursor")
		seenCursors = append(seenCursors, cursor)
		w.Header().Set("Content-Type", "application/json")
		switch cursor {
		case "":
			_ = json.NewEncoder(w).Encode(store.ListResult[store.Message]{
				Items: []store.Message{
					{ID: "m1", Sender: "agent-a", Msg: "hello", CreatedAt: now},
					{ID: "m2", Sender: "agent-b", Msg: "world", CreatedAt: now.Add(time.Second)},
				},
				NextCursor: "cur-2",
				TotalCount: 3,
			})
		case "cur-2":
			_ = json.NewEncoder(w).Encode(store.ListResult[store.Message]{
				Items: []store.Message{
					{ID: "m3", Sender: "agent-a", Msg: "done", CreatedAt: now.Add(2 * time.Second)},
				},
				NextCursor: "",
				TotalCount: 3,
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	isolateHubEnvForTest(t, server.URL, "")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	projectPath = setupConversationCreateProject(t, tmpHome, server.URL, "")
	convMsgBefore, convMsgAfter, convMsgFull = "", "", false

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	// Truncated table output.
	convMsgLimit, convMsgAll, convMsgJSON, outputFormat = 2, false, false, ""
	seenCursors = nil
	stdout, stderr := captureStdoutStderr(t, func() {
		require.NoError(t, runConversationMessages(cmd, []string{"conv:" + convID}))
	})
	assert.Contains(t, stdout, "hello")
	assert.Equal(t, "Showing 2 of 3 messages; use --limit or --all to see more.\n", stderr)
	assert.Equal(t, []string{""}, seenCursors)

	// Truncated JSON output preserves items/nextCursor/totalCount and adds truncated.
	convMsgLimit, convMsgAll, convMsgJSON, outputFormat = 2, false, true, ""
	stdout, stderr = captureStdoutStderr(t, func() {
		require.NoError(t, runConversationMessages(cmd, []string{"conv:" + convID}))
	})
	var parsed messageListJSONOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))
	assert.Len(t, parsed.Items, 2)
	assert.Equal(t, "cur-2", parsed.NextCursor)
	assert.Equal(t, 3, parsed.TotalCount)
	assert.True(t, parsed.Truncated)
	assert.Equal(t, "Showing 2 of 3 messages; use --limit or --all to see more.\n", stderr)

	// --all pages across both cursors and clears truncation.
	convMsgLimit, convMsgAll, convMsgJSON, outputFormat = 25, true, true, ""
	seenCursors = nil
	stdout, stderr = captureStdoutStderr(t, func() {
		require.NoError(t, runConversationMessages(cmd, []string{"conv:" + convID}))
	})
	var parsedAll messageListJSONOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsedAll))
	assert.Len(t, parsedAll.Items, 3)
	assert.Empty(t, parsedAll.NextCursor)
	assert.Equal(t, 3, parsedAll.TotalCount)
	assert.False(t, parsedAll.Truncated)
	assert.Empty(t, stderr)
	assert.Equal(t, []string{"", "cur-2"}, seenCursors)

	// Invalid --limit <= 0 rejected before calling hub.
	convMsgLimit, convMsgAll = 0, false
	err := runConversationMessages(cmd, []string{"conv:" + convID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--limit")
}

func TestRunConversationCatchUp_PaginationAndTruncation(t *testing.T) {
	origProject := projectPath
	origFormat := outputFormat
	origSince, origLimit, origAll, origJSON, origFull := convCatchUpSince, convCatchUpLimit, convCatchUpAll, convCatchUpJSON, convCatchUpFull
	t.Cleanup(func() {
		projectPath, outputFormat = origProject, origFormat
		convCatchUpSince, convCatchUpLimit, convCatchUpAll, convCatchUpJSON, convCatchUpFull = origSince, origLimit, origAll, origJSON, origFull
	})

	convID := "22222222-2222-2222-2222-222222222222"
	now := time.Now().UTC()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/conversations/"+convID+"/messages" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			_ = json.NewEncoder(w).Encode(store.ListResult[store.Message]{
				Items:      []store.Message{{ID: "c1", Sender: "agent-a", Msg: "first", CreatedAt: now}},
				NextCursor: "next-1",
				TotalCount: 2,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(store.ListResult[store.Message]{
			Items:      []store.Message{{ID: "c2", Sender: "agent-b", Msg: "second", CreatedAt: now.Add(time.Second)}},
			NextCursor: "",
			TotalCount: 2,
		})
	}))
	defer server.Close()

	isolateHubEnvForTest(t, server.URL, "")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	projectPath = setupConversationCreateProject(t, tmpHome, server.URL, "")
	convCatchUpSince, convCatchUpFull = "1h", false

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	// Truncated JSON.
	convCatchUpLimit, convCatchUpAll, convCatchUpJSON, outputFormat = 1, false, true, ""
	stdout, stderr := captureStdoutStderr(t, func() {
		require.NoError(t, runConversationCatchUp(cmd, []string{"conv:" + convID}))
	})
	var parsed messageListJSONOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))
	assert.Len(t, parsed.Items, 1)
	assert.True(t, parsed.Truncated)
	assert.Equal(t, "Showing 1 of 2 messages; use --limit or --all to see more.\n", stderr)

	// --all fetches all pages.
	convCatchUpLimit, convCatchUpAll, convCatchUpJSON, outputFormat = 50, true, false, ""
	stdout, stderr = captureStdoutStderr(t, func() {
		require.NoError(t, runConversationCatchUp(cmd, []string{"conv:" + convID}))
	})
	assert.Contains(t, stdout, "first")
	assert.Contains(t, stdout, "second")
	assert.Empty(t, stderr)
}

func TestRunMessagesList_IncludeReadAndPagination(t *testing.T) {
	origProject := projectPath
	origFormat := outputFormat
	origIncludeRead, origLimit, origAll, origJSON, origAgent := messagesIncludeRead, messagesLimit, messagesAll, messagesJSON, messagesAgent
	t.Cleanup(func() {
		projectPath, outputFormat = origProject, origFormat
		messagesIncludeRead, messagesLimit, messagesAll, messagesJSON, messagesAgent = origIncludeRead, origLimit, origAll, origJSON, origAgent
	})

	now := time.Now().UTC()
	var gotOnlyUnread string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/messages" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotOnlyUnread = r.URL.Query().Get("unread")
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			_ = json.NewEncoder(w).Encode(store.ListResult[store.Message]{
				Items:      []store.Message{{ID: "msg-1", AgentID: "a1", Type: "instruction", Msg: "one", CreatedAt: now}},
				NextCursor: "p2",
				TotalCount: 2,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(store.ListResult[store.Message]{
			Items:      []store.Message{{ID: "msg-2", AgentID: "a1", Type: "instruction", Msg: "two", CreatedAt: now.Add(time.Second)}},
			NextCursor: "",
			TotalCount: 2,
		})
	}))
	defer server.Close()

	isolateHubEnvForTest(t, server.URL, "")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	projectPath = setupConversationCreateProject(t, tmpHome, server.URL, "")
	messagesAgent = ""

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	// Verify --include-read flag is registered and --all is pagination.
	require.NotNil(t, messagesCmd.Flags().Lookup("include-read"))
	require.NotNil(t, messagesCmd.Flags().Lookup("limit"))
	require.NotNil(t, messagesCmd.Flags().Lookup("all"))

	// Default sends onlyUnread=true and emits truncation notice.
	messagesIncludeRead, messagesLimit, messagesAll, messagesJSON, outputFormat = false, 1, false, true, ""
	stdout, stderr := captureStdoutStderr(t, func() {
		require.NoError(t, runMessagesList(cmd, nil))
	})
	assert.Equal(t, "true", gotOnlyUnread)
	var parsed messageListJSONOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))
	assert.Len(t, parsed.Items, 1)
	assert.True(t, parsed.Truncated)
	assert.Equal(t, "Showing 1 of 2 messages; use --limit or --all to see more.\n", stderr)

	// --include-read + --all omits onlyUnread and fetches both pages.
	messagesIncludeRead, messagesLimit, messagesAll, messagesJSON, outputFormat = true, 50, true, true, ""
	stdout, stderr = captureStdoutStderr(t, func() {
		require.NoError(t, runMessagesList(cmd, nil))
	})
	assert.Empty(t, gotOnlyUnread)
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))
	assert.Len(t, parsed.Items, 2)
	assert.False(t, parsed.Truncated)
	assert.Empty(t, stderr)
}

func TestScheduleListViaHub_PaginationAndTruncation(t *testing.T) {
	origFormat := outputFormat
	origType, origStatus, origShow := scheduleType, scheduleStatus, scheduleListType
	origLimit, origAll := scheduleListLimit, scheduleListAll
	t.Cleanup(func() {
		outputFormat = origFormat
		scheduleType, scheduleStatus, scheduleListType = origType, origStatus, origShow
		scheduleListLimit, scheduleListAll = origLimit, origAll
	})

	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects/proj-1/scheduled-events":
			if r.URL.Query().Get("cursor") == "" {
				// Simulate visibleScheduledRows zeroing totalCount when authz filters rows.
				_ = json.NewEncoder(w).Encode(hubclient.ListScheduledEventsResponse{
					Events:     []hubclient.ScheduledEvent{{ID: "evt-1", EventType: "message", Status: "pending", FireAt: now, CreatedAt: now}},
					NextCursor: "ev-2",
					TotalCount: 0,
					ServerTime: now,
				})
				return
			}
			_ = json.NewEncoder(w).Encode(hubclient.ListScheduledEventsResponse{
				Events:     []hubclient.ScheduledEvent{{ID: "evt-2", EventType: "message", Status: "pending", FireAt: now, CreatedAt: now}},
				NextCursor: "",
				TotalCount: 0,
				ServerTime: now,
			})
		case "/api/v1/projects/proj-1/schedules":
			if r.URL.Query().Get("cursor") == "" {
				_ = json.NewEncoder(w).Encode(hubclient.ListSchedulesResponse{
					Schedules:  []hubclient.Schedule{{ID: "sch-1", Name: "daily", CronExpr: "0 9 * * *", Status: "active"}},
					NextCursor: "sc-2",
					TotalCount: 2,
					ServerTime: now,
				})
				return
			}
			_ = json.NewEncoder(w).Encode(hubclient.ListSchedulesResponse{
				Schedules:  []hubclient.Schedule{{ID: "sch-2", Name: "weekly", CronExpr: "0 9 * * 1", Status: "active"}},
				NextCursor: "",
				TotalCount: 2,
				ServerTime: now,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL}
	scheduleType, scheduleStatus, scheduleListType = "", "", ""

	// Truncated JSON: events has unknown totalCount=0 ("more available"), schedules has known totalCount=2.
	scheduleListLimit, scheduleListAll, outputFormat = 1, false, "json"
	stdout, stderr := captureStdoutStderr(t, func() {
		require.NoError(t, scheduleListViaHub(context.Background(), hubCtx, "proj-1"))
	})
	var parsed struct {
		Events struct {
			Events    []hubclient.ScheduledEvent `json:"events"`
			Truncated bool                       `json:"truncated"`
		} `json:"events"`
		Schedules struct {
			Schedules []hubclient.Schedule `json:"schedules"`
			Truncated bool                 `json:"truncated"`
		} `json:"schedules"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))
	assert.Len(t, parsed.Events.Events, 1)
	assert.True(t, parsed.Events.Truncated)
	assert.Len(t, parsed.Schedules.Schedules, 1)
	assert.True(t, parsed.Schedules.Truncated)
	assert.Contains(t, stderr, "Showing 1 scheduled events (more available); use --limit or --all to see more.\n")
	assert.Contains(t, stderr, "Showing 1 of 2 recurring schedules; use --limit or --all to see more.\n")

	// --all pages both endpoints and clears truncation.
	scheduleListLimit, scheduleListAll, outputFormat = 50, true, "json"
	stdout, stderr = captureStdoutStderr(t, func() {
		require.NoError(t, scheduleListViaHub(context.Background(), hubCtx, "proj-1"))
	})
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))
	assert.Len(t, parsed.Events.Events, 2)
	assert.False(t, parsed.Events.Truncated)
	assert.Len(t, parsed.Schedules.Schedules, 2)
	assert.False(t, parsed.Schedules.Truncated)
	assert.Empty(t, stderr)
}

func TestRunTemplateList_PaginationAndMaxPageSize100(t *testing.T) {
	orig := saveTemplateTestState()
	defer orig.restore()
	origFormat := outputFormat
	t.Cleanup(func() { outputFormat = origFormat })

	hermeticCLIEnv(t)
	var maxObservedLimit int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		if r.URL.Path != "/api/v1/templates" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if limStr := r.URL.Query().Get("limit"); limStr != "" {
			lim, err := strconv.Atoi(limStr)
			require.NoError(t, err)
			if lim > maxObservedLimit {
				maxObservedLimit = lim
			}
			// Enforce Hub's authorizedListMaxPageSize = 100.
			if lim > 100 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		scope := r.URL.Query().Get("scope")
		if scope == "global" {
			if r.URL.Query().Get("cursor") == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"templates":  []map[string]any{{"id": "t1", "name": "tpl-a"}},
					"nextCursor": "t-next",
					"totalCount": 2,
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"templates":  []map[string]any{{"id": "t2", "name": "tpl-b"}},
				"nextCursor": "",
				"totalCount": 2,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"templates": []any{}})
	}))
	defer server.Close()

	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	t.Setenv("SCION_AUTH_TOKEN", "test-token")

	// Truncated JSON with --limit 1.
	cmd := &cobra.Command{}
	registerTemplateListFlags(cmd)
	require.NoError(t, cmd.Flags().Set("limit", "1"))
	outputFormat = "json"

	stdout, stderr := captureStdoutStderr(t, func() {
		require.NoError(t, runTemplateList(cmd, nil))
	})
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))
	assert.Equal(t, true, parsed["truncated"])
	assert.Equal(t, "Showing 1 of 2 global templates; use --limit or --all to see more.\n", stderr)

	// --all pages with limit <= 100 and returns all global templates.
	cmdAll := &cobra.Command{}
	registerTemplateListFlags(cmdAll)
	require.NoError(t, cmdAll.Flags().Set("all", "true"))
	outputFormat = "json"
	maxObservedLimit = 0

	stdout, stderr = captureStdoutStderr(t, func() {
		require.NoError(t, runTemplateList(cmdAll, nil))
	})
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))
	assert.Equal(t, false, parsed["truncated"])
	assert.Empty(t, stderr)
	assert.LessOrEqual(t, maxObservedLimit, 100)
}

func TestRunHubProjects_PaginationAndPositionalArgGuard(t *testing.T) {
	origProject, origFormat, origHubJSON, origGlobal := projectPath, outputFormat, hubOutputJSON, globalMode
	origLimit, origAll := hubProjectsLimit, hubProjectsAll
	t.Cleanup(func() {
		projectPath, outputFormat, hubOutputJSON, globalMode = origProject, origFormat, origHubJSON, origGlobal
		hubProjectsLimit, hubProjectsAll = origLimit, origAll
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"projects":   []map[string]any{{"id": "p1", "name": "proj-a"}},
				"nextCursor": "p-next",
				"totalCount": 2,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"projects":   []map[string]any{{"id": "p2", "name": "proj-b"}},
			"nextCursor": "",
			"totalCount": 2,
		})
	}))
	defer server.Close()

	isolateHubEnvForTest(t, server.URL, "")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	projectPath, globalMode = "", true

	// Positional arg + --all is rejected.
	hubProjectsAll = true
	err := runHubProjects(&cobra.Command{}, []string{"proj-a"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--limit")
	hubProjectsAll = false

	// Truncated JSON wraps in object with total and truncated.
	hubProjectsLimit, hubProjectsAll, hubOutputJSON, outputFormat = 1, false, true, ""
	stdout, stderr := captureStdoutStderr(t, func() {
		require.NoError(t, runHubProjects(&cobra.Command{}, nil))
	})
	var parsed struct {
		Projects  []hubclient.Project `json:"projects"`
		Total     int                 `json:"total"`
		Truncated bool                `json:"truncated"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))
	assert.Len(t, parsed.Projects, 1)
	assert.Equal(t, 2, parsed.Total)
	assert.True(t, parsed.Truncated)
	assert.Equal(t, "Showing 1 of 2 projects; use --limit or --all to see more.\n", stderr)

	// --all fetches all pages.
	hubProjectsLimit, hubProjectsAll, hubOutputJSON, outputFormat = 500, true, true, ""
	stdout, stderr = captureStdoutStderr(t, func() {
		require.NoError(t, runHubProjects(&cobra.Command{}, nil))
	})
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))
	assert.Len(t, parsed.Projects, 2)
	assert.Equal(t, 2, parsed.Total)
	assert.False(t, parsed.Truncated)
	assert.Empty(t, stderr)
}

func TestRunHubBrokers_PaginationAndStaleTotalCount(t *testing.T) {
	origProject, origFormat, origHubJSON, origGlobal := projectPath, outputFormat, hubOutputJSON, globalMode
	origLimit, origAll := hubBrokersLimit, hubBrokersAll
	t.Cleanup(func() {
		projectPath, outputFormat, hubOutputJSON, globalMode = origProject, origFormat, origHubJSON, origGlobal
		hubBrokersLimit, hubBrokersAll = origLimit, origAll
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runtime-brokers" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			// Simulate Hub authenticated quirk where totalCount == page length even when nextCursor != "".
			_ = json.NewEncoder(w).Encode(map[string]any{
				"brokers":    []map[string]any{{"id": "b1", "name": "broker-a", "status": "online"}},
				"nextCursor": "b-next",
				"totalCount": 1,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"brokers":    []map[string]any{{"id": "b2", "name": "broker-b", "status": "online"}},
			"nextCursor": "",
			"totalCount": 1,
		})
	}))
	defer server.Close()

	isolateHubEnvForTest(t, server.URL, "")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	projectPath, globalMode = "", true

	// Truncated JSON with stale totalCount==1 emits "(more available)" notice.
	hubBrokersLimit, hubBrokersAll, hubOutputJSON, outputFormat = 1, false, true, ""
	stdout, stderr := captureStdoutStderr(t, func() {
		require.NoError(t, runHubBrokers(&cobra.Command{}, nil))
	})
	var parsed struct {
		Brokers   []hubclient.RuntimeBroker `json:"brokers"`
		Total     int                       `json:"total"`
		Truncated bool                      `json:"truncated"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))
	assert.Len(t, parsed.Brokers, 1)
	assert.True(t, parsed.Truncated)
	assert.Equal(t, "Showing 1 runtime brokers (more available); use --limit or --all to see more.\n", stderr)

	// --all fetches all pages and computes total=2.
	hubBrokersLimit, hubBrokersAll, hubOutputJSON, outputFormat = 50, true, true, ""
	stdout, stderr = captureStdoutStderr(t, func() {
		require.NoError(t, runHubBrokers(&cobra.Command{}, nil))
	})
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))
	assert.Len(t, parsed.Brokers, 2)
	assert.Equal(t, 2, parsed.Total)
	assert.False(t, parsed.Truncated)
	assert.Empty(t, stderr)
}
