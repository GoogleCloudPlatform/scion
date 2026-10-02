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

//go:build tzcontract

// Package tzcontract is the real-binary timestamp contract test. It starts
// `scion server start --foreground` with a non-UTC TZ in the process
// environment, against SQLite and (when SCION_TEST_POSTGRES_URL is set)
// Postgres, writes known instants through the HTTP API, and reads them back
// over REST and SSE. Every timestamp on the wire must be the same instant as
// the one written, rendered in UTC with a "Z" suffix, and never the zero time.
//
// Running the real binary, rather than an in-process hub.New, is the point:
// it proves that the server process pins itself to UTC before any work.
//
// Run with:
//
//	go test -tags tzcontract -count=1 -v ./pkg/hub/tzcontract/...
//
// See the test-tz-contract Makefile target.
package tzcontract

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	tzTokyo     = "Asia/Tokyo"
	tzKathmandu = "Asia/Kathmandu" // numeric zone abbreviation "+0545"
)

// offsetZone is the client-side zone for instants the test supplies itself.
// time.Parse of "+02:00" yields a nameless FixedZone, the shape that once made
// SQLite rows unreadable.
var offsetZone = time.FixedZone("", 2*60*60)

func TestTimestampContract(t *testing.T) {
	type backendCase struct {
		name string
		make func(t *testing.T) backend
	}
	cases := []backendCase{{name: "sqlite", make: newSQLiteBackend}}
	if pg := os.Getenv("SCION_TEST_POSTGRES_URL"); pg != "" {
		cases = append(cases, backendCase{name: "postgres", make: func(t *testing.T) backend {
			return newPostgresBackend(t, pg)
		}})
	} else {
		t.Log("SCION_TEST_POSTGRES_URL is not set: the Postgres cases are skipped")
	}

	for _, bc := range cases {
		t.Run(bc.name, func(t *testing.T) {
			t.Run("Tokyo", func(t *testing.T) { runTokyoContract(t, bc.make(t)) })
			t.Run("Kathmandu", func(t *testing.T) { runKathmanduThresholds(t, bc.make(t)) })
		})
	}
}

// ---------------------------------------------------------------------------
// Shared setup
// ---------------------------------------------------------------------------

// registerBroker registers and joins a runtime broker record so that the hub
// accepts agent creation. No broker process runs: dispatches to it fail, and
// the test only relies on writes that happen before a dispatch.
func registerBroker(t *testing.T, h *hubProc) {
	t.Helper()
	reg := h.mustJSON("POST", "/api/v1/brokers", map[string]any{
		"name": "tzcontract-broker", "autoProvide": true,
	}, 200, 201)
	h.mustJSON("POST", "/api/v1/brokers/join", map[string]any{
		"brokerId": str(reg, "brokerId"), "joinToken": str(reg, "joinToken"),
		"hostname": "tzcontract", "version": "test",
	}, 200, 201)
}

// createProject creates a project and checks its create-time stamps.
func createProject(t *testing.T, h *hubProc, name string) string {
	t.Helper()
	w := openWindow()
	// Test projects use a git remote. The host name never resolves.
	p := h.mustJSON("POST", "/api/v1/projects", map[string]any{
		"name": name, "gitRemote": "https://tzcontract.invalid/scion/" + name + ".git",
	}, 200, 201)
	w.close()
	wireTimeIn(t, "project.created", p["created"], w)
	wireTimeIn(t, "project.updated", p["updated"], w)
	return str(p, "id")
}

// createRunningAgent creates an agent record (provision only; the dispatch
// to the absent broker fails and is reported as a warning) and reports it
// running with activity "working". It returns the agent ID and the window of
// the status write.
func createRunningAgent(t *testing.T, h *hubProc, projectID, name string) (string, window) {
	t.Helper()
	cw := openWindow()
	resp := h.mustJSON("POST", "/api/v1/agents", map[string]any{
		"name": name, "projectId": projectID, "provisionOnly": true,
	}, 200, 201)
	cw.close()
	agent := obj(resp, "agent")
	if agent == nil {
		t.Fatalf("create agent %s: no agent in response", name)
	}
	id := str(agent, "id")
	wireTimeIn(t, "agent create response .created", agent["created"], cw)

	sw := openWindow()
	h.mustJSON("POST", "/api/v1/agents/"+id+"/status", map[string]any{
		"phase": "running", "activity": "working",
	}, 200, 204)
	sw.close()

	got := h.mustJSON("GET", "/api/v1/agents/"+id, nil, 200)
	wireTimeIn(t, "agent.created", got["created"], cw)
	wireTimeIn(t, "agent.updated", got["updated"], sw)
	wireTimeIn(t, "agent.lastSeen", got["lastSeen"], sw)
	wireTimeIn(t, "agent.lastActivityEvent", got["lastActivityEvent"], sw)
	return id, sw
}

// createScheduledEvent schedules a message event with a "+02:00" fireAt and
// checks it on the create response, GET by ID and the list.
func createScheduledEvent(t *testing.T, h *hubProc, projectID string) {
	t.Helper()
	fireAt := time.Now().Add(48 * time.Hour).Truncate(time.Second).In(offsetZone)
	wireFireAt := fireAt.Format(time.RFC3339)
	if !strings.HasSuffix(wireFireAt, "+02:00") {
		t.Fatalf("fixture: fireAt %q has no +02:00 offset", wireFireAt)
	}
	w := openWindow()
	evt := h.mustJSON("POST", "/api/v1/projects/"+projectID+"/scheduled-events", map[string]any{
		"eventType": "message", "fireAt": wireFireAt,
		"agentName": "tzcontract-scheduled-target", "message": "tz contract",
	}, 200, 201)
	w.close()
	id := str(evt, "id")
	wireTimeEq(t, "scheduled event create .fireAt", evt["fireAt"], fireAt)
	wireTimeIn(t, "scheduled event create .createdAt", evt["createdAt"], w)

	got := h.mustJSON("GET", "/api/v1/projects/"+projectID+"/scheduled-events/"+id, nil, 200)
	wireTimeEq(t, "scheduled event GET .fireAt", got["fireAt"], fireAt)
	wireTimeIn(t, "scheduled event GET .createdAt", got["createdAt"], w)

	lst := h.mustJSON("GET", "/api/v1/projects/"+projectID+"/scheduled-events", nil, 200)
	item := findBy(list(lst, "events"), "id", id)
	if item == nil {
		t.Fatalf("scheduled event %s missing from the list", id)
	}
	wireTimeEq(t, "scheduled event list .fireAt", item["fireAt"], fireAt)
	wireTime(t, "scheduled event list .serverTime", lst["serverTime"])
}

// createTopic creates a chat-v2 topic and checks its create time on the
// response and the thread list.
func createTopic(t *testing.T, h *hubProc, projectID, name string) string {
	t.Helper()
	w := openWindow()
	topic := h.mustJSON("POST", "/api/v1/chat/spaces/"+projectID+"/threads", map[string]any{"name": name}, 200, 201)
	w.close()
	id := str(topic, "id")
	createdAt := wireTimeIn(t, "topic create .createdAt", topic["createdAt"], w)
	threads := h.mustJSON("GET", "/api/v1/chat/spaces/"+projectID+"/threads", nil, 200)
	th := findBy(list(threads, "threads"), "id", id)
	if th == nil {
		t.Fatalf("topic %s missing from the thread list", id)
	}
	wireTimeEq(t, "thread list .createdAt", th["createdAt"], createdAt)
	return id
}

// assertServerLogsUTC checks that every structured log line the server wrote
// carries a UTC "time" value. slog formats time.Now() in time.Local, so this
// fails if the process is not pinned to UTC.
func assertServerLogsUTC(t *testing.T, h *hubProc) {
	t.Helper()
	if strings.Contains(h.logText(), "via rclone") {
		t.Errorf("unexpected rclone activity in the hub log")
	}
	n := 0
	for _, line := range strings.Split(h.logText(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		v, ok := rec["time"]
		if !ok {
			continue
		}
		n++
		s, _ := v.(string)
		if !strings.HasSuffix(s, "Z") {
			t.Errorf("server log time %q is not UTC: %.200s", s, line)
			return
		}
	}
	if n < 5 {
		t.Errorf("found %d structured server log lines, want at least 5", n)
	}
}

// ---------------------------------------------------------------------------
// TZ=Asia/Tokyo: wire contract, then legacy rows (SQLite)
// ---------------------------------------------------------------------------

func runTokyoContract(t *testing.T, b backend) {
	home := t.TempDir()
	h := startHub(t, b, tzTokyo, home)

	registerBroker(t, h)
	projectID := createProject(t, h, "tzcontract")
	agentID, _ := createRunningAgent(t, h, projectID, "tzc-agent")

	// Agent messaging: REST and the SSE message event (events.go
	// PublishUserMessage) must carry the same instant.
	step(t, "agent message", func(t *testing.T) {
		sse := h.subscribe("agent." + agentID + ".message")
		w := openWindow()
		// The message is stored and published before the hub dispatches it
		// to the (absent) broker, whose retry runs for 30s. Do not wait.
		h.doWithTimeout("POST", "/api/v1/agents/"+agentID+"/message",
			map[string]any{"message": "tz contract agent message"}, 3*time.Second)
		ev := sse.wait(t, ".message", func(d map[string]any) bool {
			return str(d, "msg") == "tz contract agent message"
		})
		w.close()
		sseAt := wireTimeIn(t, "SSE agent message .createdAt", ev.Data["createdAt"], w)

		msgs := h.mustJSON("GET", "/api/v1/agents/"+agentID+"/messages", nil, 200)
		m := findBy(list(msgs, "items"), "id", str(ev.Data, "id"))
		if m == nil {
			t.Fatalf("agent message %s missing from GET /agents/{id}/messages", str(ev.Data, "id"))
		}
		wireTimeEq(t, "REST agent message .createdAt", m["createdAt"], sseAt)

		inbox := h.mustJSON("GET", "/api/v1/messages?agent="+url.QueryEscape(agentID), nil, 200)
		for _, it := range list(inbox, "items") {
			if str(it, "id") == str(m, "id") {
				wireTimeEq(t, "REST /messages .createdAt", it["createdAt"], sseAt)
			}
		}
	})

	// Chat-v2: send, history, SSE, topic activity (webchat_topic) and edit
	// (webchat_message_ext).
	var topicID, chatMsgID string
	step(t, "chat-v2", func(t *testing.T) {
		topicID = createTopic(t, h, projectID, "tzcontract-tokyo")

		sse := h.subscribe("project." + projectID + ".chat.message")
		w := openWindow()
		sent := h.mustJSON("POST", "/api/v1/chat/conversations/"+topicID+"/messages",
			map[string]any{"content": "tz contract chat message"}, 200, 201)
		w.close()
		chatMsgID = str(sent, "id")
		sentAt := wireTimeIn(t, "chat send .createdAt", sent["createdAt"], w)

		ev := sse.wait(t, ".chat.message", func(d map[string]any) bool { return str(d, "id") == chatMsgID })
		wireTimeEq(t, "SSE chat message .createdAt", ev.Data["createdAt"], sentAt)

		hist := h.mustJSON("GET", "/api/v1/chat/conversations/"+topicID+"/messages", nil, 200)
		hm := findBy(list(hist, "messages"), "id", chatMsgID)
		if hm == nil {
			t.Fatalf("chat message %s missing from history", chatMsgID)
		}
		wireTimeEq(t, "chat history .createdAt", hm["createdAt"], sentAt)

		threads := h.mustJSON("GET", "/api/v1/chat/spaces/"+projectID+"/threads", nil, 200)
		th := findBy(list(threads, "threads"), "id", topicID)
		wireTimeIn(t, "thread.lastActivityAt after send", th["lastActivityAt"], w)

		ew := openWindow()
		h.mustJSON("PUT", "/api/v1/chat/conversations/"+topicID+"/messages/"+chatMsgID,
			map[string]any{"content": "tz contract chat message (edited)"}, 200)
		ew.close()
		hist = h.mustJSON("GET", "/api/v1/chat/conversations/"+topicID+"/messages", nil, 200)
		ext := obj(obj(hist, "messageExtensions"), chatMsgID)
		if ext == nil {
			t.Fatalf("no messageExtensions entry for the edited message")
		}
		wireTimeIn(t, "chat history messageExtensions .editedAt", ext["editedAt"], ew)
	})

	step(t, "scheduled event fireAt +02:00", func(t *testing.T) {
		createScheduledEvent(t, h, projectID)
	})

	// Notifications: created by the dispatcher on an agent status change.
	step(t, "notification", func(t *testing.T) {
		notifAgentID, _ := createRunningAgent(t, h, projectID, "tzc-notify")
		h.mustJSON("POST", "/api/v1/notifications/subscriptions", map[string]any{
			"scope": "agent", "agentId": notifAgentID, "projectId": projectID,
			"triggerActivities": []string{"COMPLETED"},
		}, 200, 201)
		sse := h.subscribe("notification.created")
		w := openWindow()
		h.mustJSON("POST", "/api/v1/agents/"+notifAgentID+"/status",
			map[string]any{"activity": "completed"}, 200, 204)
		ev := sse.wait(t, "notification.created", func(d map[string]any) bool {
			return str(d, "agentId") == notifAgentID
		})
		w.close()
		sseAt := wireTimeIn(t, "SSE notification .createdAt", ev.Data["createdAt"], w)

		status, out := h.do("GET", "/api/v1/notifications", nil)
		if status != 200 {
			t.Fatalf("GET /notifications: %d %s", status, truncate(out, 400))
		}
		var notifs []map[string]any
		if err := json.Unmarshal(out, &notifs); err != nil {
			t.Fatalf("decode notifications: %v: %s", err, truncate(out, 400))
		}
		n := findBy(notifs, "agentId", notifAgentID)
		if n == nil {
			t.Fatalf("no notification for agent %s in %s", notifAgentID, truncate(out, 400))
		}
		restAt := wireTimeIn(t, "REST notification .createdAt", n["createdAt"], w)
		// The SSE notification event uses millisecond precision.
		if !restAt.Truncate(time.Millisecond).Equal(sseAt) {
			t.Errorf("SSE notification createdAt %s is not REST %s at millisecond precision",
				sseAt.Format(time.RFC3339Nano), restAt.Format(time.RFC3339Nano))
		}
	})

	// Access-constraint window (normalised at ingest). This replaces the
	// design's policy validFrom: there is no policy write path.
	step(t, "access constraint window +02:00", func(t *testing.T) {
		acProject := createProject(t, h, "tzcontract-ac")
		grantConstraintAdmin(t, h, acProject)
		notBefore := time.Now().Add(24 * time.Hour).Truncate(time.Second).In(offsetZone)
		expiresAt := time.Now().Add(72 * time.Hour).Truncate(time.Second).In(offsetZone)
		draft := map[string]any{
			"name":               "tzcontract window",
			"purpose":            "timestamp contract test",
			"subject":            map[string]any{"kind": "all_principals"},
			"scope":              map[string]any{"type": "project", "id": acProject},
			"maximumPermissions": []string{"agent.read", "access_constraint.admin"},
			"appliesWhen": map[string]any{
				"notBefore": notBefore.Format(time.RFC3339),
				"expiresAt": expiresAt.Format(time.RFC3339),
			},
		}
		pw := openWindow()
		prev := h.mustJSON("POST", "/api/v1/admin/access-constraint-previews",
			map[string]any{"operation": "create", "draft": draft}, 200)
		pw.close()
		wireTimeIn(t, "preview .generatedAt", prev["generatedAt"], pw)
		wireTime(t, "preview .expiresAt", prev["expiresAt"])

		create := map[string]any{"previewToken": str(prev, "previewToken")}
		for k, v := range draft {
			create[k] = v
		}
		w := openWindow()
		ac := h.mustJSON("POST", "/api/v1/admin/access-constraints", create, 200, 201)
		w.close()
		check := func(label string, c map[string]any) {
			aw := obj(c, "appliesWhen")
			if aw == nil {
				t.Fatalf("%s: no appliesWhen in %v", label, c)
			}
			wireTimeEq(t, label+" .appliesWhen.notBefore", aw["notBefore"], notBefore)
			wireTimeEq(t, label+" .appliesWhen.expiresAt", aw["expiresAt"], expiresAt)
			wireTimeIn(t, label+" .createdAt", c["createdAt"], w)
			wireTimeIn(t, label+" .updatedAt", c["updatedAt"], w)
		}
		check("access constraint create", ac)
		check("access constraint GET", h.mustJSON("GET", "/api/v1/admin/access-constraints/"+str(ac, "id"), nil, 200))
	})

	assertServerLogsUTC(t, h)
	h.stop()

	if b.isSQLite() {
		t.Run("legacy SQLite rows", func(t *testing.T) {
			if topicID == "" || chatMsgID == "" {
				t.Skip("chat-v2 fixture did not complete")
			}
			runLegacyRows(t, b, home, projectID, topicID, chatMsgID)
		})
	}
}

// legacyForm is one legacy on-disk timestamp (written before the UTC store boundary) and the instant it denotes.
type legacyForm struct {
	name, stored string
	want         time.Time
}

// webchatLegacyForms are the three legacy webchat TEXT forms: RFC 3339 with
// a local offset, Go Time.String() with an alphabetic abbreviation and a
// monotonic suffix, and Time.String() with a four-digit numeric abbreviation.
var webchatLegacyForms = []legacyForm{
	{"rfc3339 +09:00", "2026-10-01T13:00:00.25+09:00", time.Date(2026, 10, 1, 4, 0, 0, 250000000, time.UTC)},
	{"String() JST m=", "2026-10-01 14:30:00.5 +0900 JST m=+1.500000001", time.Date(2026, 10, 1, 5, 30, 0, 500000000, time.UTC)},
	{"String() +0545 +0545", "2026-10-01 12:45:00 +0545 +0545", time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC)},
}

// runLegacyRows seeds legacy rows into a stopped SQLite hub, restarts it
// with TZ=Asia/Tokyo and asserts every row is served as Z at the right
// instant.
func runLegacyRows(t *testing.T, b backend, home, projectID, topicID, chatMsgID string) {
	db := b.openDB(t)

	// Ent row: Time.String() of a Tokyo-local time.Now(), as an unpinned
	// hub wrote it before the UTC store boundary.
	entStored := "2026-10-01 15:04:58.96 +0900 JST m=+0.071234567"
	entWant := time.Date(2026, 10, 1, 6, 4, 58, 960000000, time.UTC)
	execOne(t, db, `UPDATE projects SET created = ? WHERE id = ?`, entStored, projectID)

	// One webchat topic per legacy form, in created_at and last_activity_at.
	topicIDs := make([]string, len(webchatLegacyForms))
	for i, f := range webchatLegacyForms {
		topicIDs[i] = fmt.Sprintf("tzcontract-legacy-%d", i)
		if _, err := db.Exec(`INSERT INTO webchat_topic (id, project_id, name, is_general, created_by, created_at, last_activity_at)
			VALUES (?, ?, ?, 0, 'tzcontract', ?, ?)`, topicIDs[i], projectID, "legacy "+f.name, f.stored, f.stored); err != nil {
			t.Fatalf("seed webchat_topic %q: %v", f.name, err)
		}
	}
	// The edited chat message carries the alphabetic String() form.
	editForm := webchatLegacyForms[1]
	execOne(t, db, `UPDATE webchat_message_ext SET edited_at = ? WHERE message_id = ?`, editForm.stored, chatMsgID)
	_ = db.Close()

	h := startHub(t, b, tzTokyo, home)
	defer h.stop()

	p := h.mustJSON("GET", "/api/v1/projects/"+projectID, nil, 200)
	wireTimeEq(t, "legacy ent projects.created", p["created"], entWant)

	threads := h.mustJSON("GET", "/api/v1/chat/spaces/"+projectID+"/threads", nil, 200)
	for i, f := range webchatLegacyForms {
		th := findBy(list(threads, "threads"), "id", topicIDs[i])
		if th == nil {
			t.Errorf("legacy topic %q missing from the thread list", f.name)
			continue
		}
		wireTimeEq(t, "legacy webchat_topic.created_at ("+f.name+")", th["createdAt"], f.want)
		wireTimeEq(t, "legacy webchat_topic.last_activity_at ("+f.name+")", th["lastActivityAt"], f.want)
	}

	hist := h.mustJSON("GET", "/api/v1/chat/conversations/"+topicID+"/messages", nil, 200)
	ext := obj(obj(hist, "messageExtensions"), chatMsgID)
	if ext == nil {
		t.Fatalf("no messageExtensions entry for the seeded message")
	}
	wireTimeEq(t, "legacy webchat_message_ext.edited_at ("+editForm.name+")", ext["editedAt"], editForm.want)

	assertServerLogsUTC(t, h)
}

// step runs one section of a scenario that shares a hub process. The hub
// helpers fail the test that started the hub, so sections are not subtests.
func step(t *testing.T, name string, f func(t *testing.T)) {
	t.Helper()
	t.Logf("step: %s", name)
	f(t)
}

// grantConstraintAdmin binds the dev user to a project-scoped role holding
// access_constraint.admin. Constraint creation refuses any change that would
// leave a scope with no direct constraint admin.
func grantConstraintAdmin(t *testing.T, h *hubProc, projectID string) {
	t.Helper()
	me := h.mustJSON("GET", "/api/v1/auth/me", nil, 200)
	userID := str(me, "id")
	if userID == "" {
		userID = str(obj(me, "user"), "id")
	}
	if userID == "" {
		t.Fatalf("GET /auth/me: no user id in %v", me)
	}
	w := openWindow()
	role := h.mustJSON("POST", "/api/v1/admin/roles", map[string]any{
		"name": "tzcontract-constraint-admin", "description": "timestamp contract test",
		"scopeType": "project", "permissions": []string{"access_constraint.admin"},
	}, 200, 201)
	binding := h.mustJSON("POST", "/api/v1/admin/role-bindings", map[string]any{
		"roleDefinitionId": str(role, "id"), "principalType": "user", "principalId": userID,
		"scopeType": "project", "scopeId": projectID,
	}, 200, 201)
	w.close()
	wireTimeIn(t, "role definition .createdAt", role["createdAt"], w)
	wireTimeIn(t, "role binding .createdAt", binding["createdAt"], w)
}

func execOne(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	res, err := db.Exec(query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("%s: %d rows affected, want 1", query, n)
	}
}

// ---------------------------------------------------------------------------
// TZ=Asia/Kathmandu: fireAt, readability, stale and stalled thresholds
// ---------------------------------------------------------------------------

func runKathmanduThresholds(t *testing.T, b backend) {
	home := t.TempDir()
	h := startHub(t, b, tzKathmandu, home)

	registerBroker(t, h)
	projectID := createProject(t, h, "tzcontract-ktm")
	staleID, _ := createRunningAgent(t, h, projectID, "tzc-stale")
	stalledID, _ := createRunningAgent(t, h, projectID, "tzc-stalled")
	healthyID, _ := createRunningAgent(t, h, projectID, "tzc-healthy")
	createScheduledEvent(t, h, projectID)

	// A chat message, so the webchat tables hold rows written under this TZ.
	topicID := createTopic(t, h, projectID, "tzcontract-kathmandu")
	h.mustJSON("POST", "/api/v1/chat/conversations/"+topicID+"/messages",
		map[string]any{"content": "tz contract kathmandu"}, 200, 201)

	assertServerLogsUTC(t, h)
	h.stop()

	// Age the fixtures while the hub is down. MarkStaleAgentsOffline takes
	// running agents whose last_seen is over 2 minutes old;
	// MarkStalledAgents takes running agents whose last_activity_event is
	// older than the stalled threshold (5 minutes) but whose last_seen is
	// recent. Both run once when the scheduler starts.
	db := b.openDB(t)
	staleSeen := shiftAgentTime(t, b, db, staleID, "last_seen", -10*time.Minute)
	shiftAgentTime(t, b, db, stalledID, "last_activity_event", -10*time.Minute)
	_ = db.Close()

	if b.isSQLite() {
		assertSQLiteTablesReadable(t, b)
	}

	h = startHub(t, b, tzKathmandu, home)
	defer h.stop()

	deadline := time.Now().Add(150 * time.Second)
	var stale, stalled map[string]any
	for {
		stale = h.mustJSON("GET", "/api/v1/agents/"+staleID, nil, 200)
		stalled = h.mustJSON("GET", "/api/v1/agents/"+stalledID, nil, 200)
		if str(stale, "activity") == "offline" && str(stalled, "activity") == "stalled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after 150s: stale agent activity %q (want offline), stalled agent activity %q (want stalled)",
				str(stale, "activity"), str(stalled, "activity"))
		}
		time.Sleep(2 * time.Second)
	}
	wireTimeEq(t, "aged agent.lastSeen", stale["lastSeen"], staleSeen)

	healthy := h.mustJSON("GET", "/api/v1/agents/"+healthyID, nil, 200)
	if got := str(healthy, "activity"); got != "working" {
		t.Errorf("healthy agent activity = %q, want working (thresholds selected the wrong agents)", got)
	}

	// Everything stays readable and Z after the restart.
	agents := h.mustJSON("GET", "/api/v1/agents?projectId="+url.QueryEscape(projectID), nil, 200)
	items := list(agents, "agents")
	if len(items) == 0 {
		items = list(agents, "items")
	}
	for _, id := range []string{staleID, stalledID, healthyID} {
		a := findBy(items, "id", id)
		if a == nil {
			t.Errorf("agent %s missing from the agent list", id)
			continue
		}
		wireTime(t, "agent list .created", a["created"])
		wireTime(t, "agent list .lastSeen", a["lastSeen"])
	}
	lst := h.mustJSON("GET", "/api/v1/projects/"+projectID+"/scheduled-events", nil, 200)
	if len(list(lst, "events")) == 0 {
		t.Errorf("scheduled events list is empty after restart")
	}
	for _, e := range list(lst, "events") {
		wireTime(t, "scheduled event list .fireAt after restart", e["fireAt"])
	}
	hist := h.mustJSON("GET", "/api/v1/chat/conversations/"+topicID+"/messages", nil, 200)
	for _, m := range list(hist, "messages") {
		wireTime(t, "chat history .createdAt after restart", m["createdAt"])
	}

	assertServerLogsUTC(t, h)
	if b.isSQLite() {
		h.stop()
		assertSQLiteTablesReadable(t, b)
	}
}

// shiftAgentTime moves one agent timestamp by d and returns the new instant.
// SQLite stores ent times as Time.String() text; the shifted value is written
// in the canonical UTC form.
func shiftAgentTime(t *testing.T, b backend, db *sql.DB, agentID, column string, d time.Duration) time.Time {
	t.Helper()
	if !b.isSQLite() {
		var shifted time.Time
		q := fmt.Sprintf(`UPDATE agents SET %[1]s = %[1]s + make_interval(secs => $1) WHERE id = $2 RETURNING %[1]s`, column)
		if err := db.QueryRow(q, d.Seconds(), agentID).Scan(&shifted); err != nil {
			t.Fatalf("shift %s: %v", column, err)
		}
		return shifted
	}
	var stored string
	if err := db.QueryRow(fmt.Sprintf(`SELECT CAST(%s AS TEXT) FROM agents WHERE id = ?`, column), agentID).Scan(&stored); err != nil {
		t.Fatalf("read %s: %v", column, err)
	}
	if !canonicalEntTime.MatchString(stored) {
		t.Fatalf("agents.%s = %q: not the canonical UTC form", column, stored)
	}
	cur, err := time.Parse("2006-01-02 15:04:05.999999999 -0700 MST", stored)
	if err != nil {
		t.Fatalf("parse agents.%s = %q: %v", column, stored, err)
	}
	shifted := cur.Add(d).UTC()
	execOne(t, db, fmt.Sprintf(`UPDATE agents SET %s = ? WHERE id = ?`, column), shifted.String(), agentID)
	return shifted
}

// canonicalEntTime is t.UTC().String(): no "T", no monotonic suffix, UTC.
var canonicalEntTime = regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d(\.\d+)? \+0000 UTC$`)

// assertSQLiteTablesReadable scans every stored time value written under
// Asia/Kathmandu. Ent DATETIME columns must hold the canonical UTC form
// (a four-digit numeric abbreviation such as "+0545 +0545" makes every ent
// read of the table fail). Webchat *_at TEXT columns must hold RFC 3339 "Z".
func assertSQLiteTablesReadable(t *testing.T, b backend) {
	t.Helper()
	db := b.openDB(t)
	defer func() { _ = db.Close() }()

	tables := []string{}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, n)
	}
	_ = rows.Close()

	checked := 0
	for _, table := range tables {
		cols, err := db.Query(fmt.Sprintf(`SELECT name, type FROM pragma_table_info('%s')`, table))
		if err != nil {
			t.Fatalf("table_info %s: %v", table, err)
		}
		type col struct{ name, typ string }
		var timeCols []col
		for cols.Next() {
			var c col
			if err := cols.Scan(&c.name, &c.typ); err != nil {
				t.Fatalf("scan table_info %s: %v", table, err)
			}
			typ := strings.ToUpper(c.typ)
			isEnt := typ == "DATETIME" || typ == "TIMESTAMP" || typ == "DATE"
			isWebchat := strings.HasPrefix(table, "webchat_") && strings.HasSuffix(c.name, "_at") && typ == "TEXT"
			if isEnt || isWebchat {
				timeCols = append(timeCols, c)
			}
		}
		_ = cols.Close()

		for _, c := range timeCols {
			vals, err := db.Query(fmt.Sprintf(`SELECT CAST("%s" AS TEXT) FROM "%s" WHERE "%s" IS NOT NULL AND "%s" != ''`,
				c.name, table, c.name, c.name))
			if err != nil {
				t.Fatalf("read %s.%s: %v", table, c.name, err)
			}
			for vals.Next() {
				var v string
				if err := vals.Scan(&v); err != nil {
					t.Fatalf("scan %s.%s: %v", table, c.name, err)
				}
				checked++
				if strings.HasPrefix(table, "webchat_") && strings.ToUpper(c.typ) == "TEXT" {
					ts, err := time.Parse(time.RFC3339Nano, v)
					if err != nil || !strings.HasSuffix(v, "Z") || ts.IsZero() {
						t.Errorf("%s.%s = %q: want RFC 3339 UTC (Z)", table, c.name, v)
					}
					continue
				}
				if !canonicalEntTime.MatchString(v) {
					t.Errorf("%s.%s = %q: want the canonical UTC form (t.UTC().String())", table, c.name, v)
				}
			}
			_ = vals.Close()
		}
	}
	if checked == 0 {
		t.Errorf("no stored time values found: the readability scan checked nothing")
	}
}
