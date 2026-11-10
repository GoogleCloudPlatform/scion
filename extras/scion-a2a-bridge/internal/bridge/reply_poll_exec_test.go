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

package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	hubstore "github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Executor-level tests of reply polling, against an HTTP stand-in for the
// hub's agent message and conversation routes. The stand-in holds one
// direct conversation between the caller and the agent; the test adds the
// agent's messages to it after the send, as the hub would file them.

const (
	execTestAgentID     = "agent-uuid-exec"
	execTestProjectID   = "proj-uuid-exec"
	execTestProject     = "proj-exec"
	execTestAgentSlug   = "helper"
	execTestCallerID    = "caller-uuid-exec"
	execTestCallerToken = "caller-credential-exec"
	execTestAdminToken  = "admin-credential-exec"
	execTestAdminUserID = "admin-uuid-exec"
	execTestConvID      = "conv-exec-1"
	execTestDMKey       = "dm:agent:agent-uuid-exec:user:caller-uuid-exec"
)

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// fakeHubRequest is one request the stand-in served.
type fakeHubRequest struct {
	method string
	path   string
	query  url.Values
	bearer string
}

// fakeConvHub is the HTTP stand-in for the hub.
type fakeConvHub struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []fakeHubRequest
	// dmExists: the conversation exists (a send creates it). hideDM: the
	// caller's list omits it (as the hub does for a credential that may
	// not read the agent, or when the send stored no conversation).
	dmExists bool
	hideDM   bool
	rows     []hubstore.Message // oldest first
	seq      int
	base     time.Time
	// Status overrides; n is the 1-based call count of the route, and 0
	// answers normally.
	listStatus func(n int) int
	pageStatus func(n int) int
	getStatus  func(n int) int
	listCalls  int
	pageCalls  int
	getCalls   int
	sendCalls  int
	// sendStatus is the status a send answers ("" means "delivered");
	// omitMessageID answers without the message ID, as an older hub does.
	sendStatus    string
	omitMessageID bool
	// sent receives the stored ID of each sent message.
	sent chan string
}

func newFakeConvHub(t *testing.T) *fakeConvHub {
	t.Helper()
	f := &fakeConvHub{
		base: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC),
		sent: make(chan string, 16),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// addRowLocked appends a message to the conversation. Created times and
// IDs both increase, so the hub's keyset order matches insertion order.
func (f *fakeConvHub) addRowLocked(senderID, sender, msgType, threadID, text string) string {
	f.seq++
	id := fmt.Sprintf("m%04d", f.seq)
	f.rows = append(f.rows, hubstore.Message{
		ID:             id,
		Sender:         sender,
		SenderID:       senderID,
		Msg:            text,
		Type:           msgType,
		ThreadID:       threadID,
		ConversationID: execTestConvID,
		CreatedAt:      f.base.Add(time.Duration(f.seq) * 10 * time.Millisecond),
	})
	return id
}

// addAgentRow files a message from the agent in the conversation.
func (f *fakeConvHub) addAgentRow(msgType, threadID, text string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addRowLocked(execTestAgentID, "agent:"+execTestAgentSlug, msgType, threadID, text)
}

// addCallerRow files a message from the caller in the conversation.
func (f *fakeConvHub) addCallerRow(text string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addRowLocked(execTestCallerID, "user:caller@example.com", messages.TypeInstruction, "", text)
}

// seedConversation makes the conversation exist with one earlier exchange.
func (f *fakeConvHub) seedConversation() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dmExists = true
	f.addRowLocked(execTestCallerID, "user:caller@example.com", messages.TypeInstruction, "", "an earlier request")
	f.addRowLocked(execTestAgentID, "agent:"+execTestAgentSlug, messages.TypeInstruction, execTestDMKey, "an earlier reply")
}

func (f *fakeConvHub) snapshot() []fakeHubRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeHubRequest(nil), f.requests...)
}

func (f *fakeConvHub) counts() (list, page, get, send int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls, f.pageCalls, f.getCalls, f.sendCalls
}

func (f *fakeConvHub) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeConvHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, fakeHubRequest{
		method: r.Method,
		path:   r.URL.Path,
		query:  r.URL.Query(),
		bearer: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
	})
	f.mu.Unlock()

	path := r.URL.Path
	messagesPath := "/api/v1/conversations/" + execTestConvID + "/messages"
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/v1/agents/") && strings.HasSuffix(path, "/message"):
		f.mu.Lock()
		f.sendCalls++
		f.dmExists = true
		id := f.addRowLocked(execTestCallerID, "user:caller@example.com", messages.TypeInstruction, "", "the request")
		status := f.sendStatus
		if status == "" {
			status = "delivered"
		}
		resp := map[string]any{"status": status, "agent": execTestAgentSlug}
		if !f.omitMessageID {
			resp["message_id"] = id
		}
		f.mu.Unlock()
		select {
		case f.sent <- id:
		default:
		}
		f.writeJSON(w, resp)
	case r.Method == http.MethodGet && path == "/api/v1/conversations":
		f.mu.Lock()
		f.listCalls++
		n := f.listCalls
		statusFn := f.listStatus
		listed := f.dmExists && !f.hideDM
		f.mu.Unlock()
		if statusFn != nil {
			if status := statusFn(n); status != 0 {
				http.Error(w, "refused", status)
				return
			}
		}
		convs := []map[string]any{}
		if listed {
			convs = append(convs, map[string]any{
				"id": execTestConvID, "kind": "direct",
				"dmPeer": map[string]any{"kind": "agent", "id": execTestAgentID},
			})
		}
		f.writeJSON(w, map[string]any{"conversations": convs})
	case r.Method == http.MethodGet && strings.HasPrefix(path, messagesPath+"/"):
		id := strings.TrimPrefix(path, messagesPath+"/")
		f.mu.Lock()
		f.getCalls++
		n := f.getCalls
		statusFn := f.getStatus
		var found *hubstore.Message
		for i := range f.rows {
			if f.rows[i].ID == id {
				m := f.rows[i]
				found = &m
			}
		}
		f.mu.Unlock()
		if statusFn != nil {
			if status := statusFn(n); status != 0 {
				http.Error(w, "refused", status)
				return
			}
		}
		if found == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		f.writeJSON(w, found)
	case r.Method == http.MethodGet && path == messagesPath:
		f.mu.Lock()
		f.pageCalls++
		n := f.pageCalls
		statusFn := f.pageStatus
		newestFirst := make([]hubstore.Message, 0, len(f.rows))
		for i := len(f.rows) - 1; i >= 0; i-- {
			newestFirst = append(newestFirst, f.rows[i])
		}
		f.mu.Unlock()
		if statusFn != nil {
			if status := statusFn(n); status != 0 {
				http.Error(w, "refused", status)
				return
			}
		}
		q := r.URL.Query()
		offset, _ := strconv.Atoi(q.Get("cursor"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 {
			limit = 50
		}
		if offset > len(newestFirst) {
			offset = len(newestFirst)
		}
		end := offset + limit
		next := ""
		if end < len(newestFirst) {
			next = strconv.Itoa(end)
		} else {
			end = len(newestFirst)
		}
		f.writeJSON(w, map[string]any{"items": newestFirst[offset:end], "nextCursor": next})
	default:
		http.NotFound(w, r)
	}
}

// execTestAgents lists the one agent; every other call goes to the hub
// stand-in through the real hub client.
type execTestAgents struct {
	hubclient.AgentService
}

func (a execTestAgents) List(context.Context, *hubclient.ListAgentsOptions) (*hubclient.ListAgentsResponse, error) {
	return &hubclient.ListAgentsResponse{Agents: []hubclient.Agent{{
		ID: execTestAgentID, Name: execTestAgentSlug, Slug: execTestAgentSlug, ProjectID: execTestProjectID,
	}}}, nil
}

// execTestHub is the bridge's admin hub client: the real client against
// the stand-in, with the agent list stubbed.
type execTestHub struct {
	hubclient.Client
	agents execTestAgents
}

func (h execTestHub) Agents() hubclient.AgentService { return h.agents }

// newExecTestBridge returns a plugin-mode bridge whose hub is the stand-in,
// with a debug-level log sink.
func newExecTestBridge(t *testing.T, f *fakeConvHub, mutate func(*Config)) (*Bridge, state.Store, *syncBuffer) {
	t.Helper()
	store, err := state.NewSQLite(filepath.Join(t.TempDir(), "exec-test.db"))
	if err != nil {
		t.Fatalf("state store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := &Config{
		Hub:      HubConfig{Endpoint: f.srv.URL, User: "admin@example.com", UserID: execTestAdminUserID},
		Timeouts: TimeoutConfig{SendMessage: 4 * time.Second},
		Projects: []ProjectConfig{{Slug: execTestProject, ExposedAgents: []string{execTestAgentSlug}}},
		Bridge:   BridgeConfig{ExternalURL: "https://bridge.test.example.com"},
	}
	if mutate != nil {
		mutate(cfg)
	}
	admin, err := hubclient.New(f.srv.URL, hubclient.WithBearerToken(execTestAdminToken))
	if err != nil {
		t.Fatalf("admin hub client: %v", err)
	}
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	b := New(store, execTestHub{Client: admin, agents: execTestAgents{AgentService: admin.Agents()}}, nil, cfg, nil, log)
	t.Cleanup(b.Shutdown)
	return b, store, logs
}

// execTestCaller is a user caller with a user access credential.
func execTestCaller() *CallerIdentity {
	return &CallerIdentity{
		UserID: execTestCallerID, Email: "caller@example.com", Role: "member",
		RawToken: execTestCallerToken, TokenType: "uat",
	}
}

// execOutcome is what the executor yielded.
type execOutcome struct {
	states []a2a.TaskState
	final  a2a.TaskState
	text   string
	err    error
	// slotsHeldAtFinal is the number of reply poll read slots held when the
	// final event was yielded.
	slotsHeldAtFinal int
}

// runExec runs one executor call and collects its status events.
func runExec(ctx context.Context, b *Bridge, taskID string, caller *CallerIdentity, stored bool, text string) execOutcome {
	ctx = WithRouteInfo(ctx, RouteInfo{ProjectSlug: execTestProject, AgentSlug: execTestAgentSlug})
	if caller != nil {
		ctx = WithCallerIdentity(ctx, caller)
	}
	execCtx := &a2asrv.ExecutorContext{
		TaskID:    a2a.TaskID(taskID),
		ContextID: "ctx-" + taskID,
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text)),
	}
	if stored {
		execCtx.StoredTask = &a2a.Task{ID: a2a.TaskID(taskID), ContextID: "ctx-" + taskID}
	}
	var out execOutcome
	for ev, err := range NewScionExecutor(b, b.log).Execute(ctx, execCtx) {
		if err != nil {
			out.err = err
			break
		}
		su, ok := ev.(*a2a.TaskStatusUpdateEvent)
		if !ok {
			continue
		}
		out.states = append(out.states, su.Status.State)
		out.final = su.Status.State
		out.text = ""
		if su.Status.Message != nil {
			var parts []string
			for _, p := range su.Status.Message.Parts {
				if p != nil {
					parts = append(parts, p.Text())
				}
			}
			out.text = strings.Join(parts, "\n")
		}
		out.slotsHeldAtFinal = len(b.replyPollSem)
	}
	return out
}

// runExecAsync runs runExec in a goroutine.
func runExecAsync(ctx context.Context, b *Bridge, taskID string, caller *CallerIdentity, stored bool, text string) <-chan execOutcome {
	done := make(chan execOutcome, 1)
	go func() { done <- runExec(ctx, b, taskID, caller, stored, text) }()
	return done
}

// awaitSend waits for the stand-in to receive a send and returns its ID.
func (f *fakeConvHub) awaitSend(t *testing.T) string {
	t.Helper()
	select {
	case id := <-f.sent:
		return id
	case <-time.After(10 * time.Second):
		t.Fatal("no message was sent to the agent")
		return ""
	}
}

// awaitOutcome waits for an executor call to end.
func awaitOutcome(t *testing.T, done <-chan execOutcome) execOutcome {
	t.Helper()
	select {
	case out := <-done:
		return out
	case <-time.After(20 * time.Second):
		t.Fatal("the executor call did not end")
		return execOutcome{}
	}
}

// assertNeutralLogs checks that no log line, at any level, carries message
// text, user IDs, conversation IDs, DM keys or topic strings.
func assertNeutralLogs(t *testing.T, logs *syncBuffer, markers ...string) {
	t.Helper()
	out := logs.String()
	forbidden := append([]string{execTestCallerID, execTestAdminUserID, execTestConvID, "dm:", "scion.project.", "caller@example.com"}, markers...)
	for _, m := range forbidden {
		if m != "" && strings.Contains(out, m) {
			t.Errorf("a log line contains %q; logs:\n%s", m, out)
		}
	}
}

// TestReplyPoll_LocalRefusalsAndStatusMapping covers the local caller's
// preflight and read status mapping (D7).
func TestReplyPoll_LocalRefusalsAndStatusMapping(t *testing.T) {
	refuse := func(status int) func(int) int { return func(int) int { return status } }

	t.Run("no inbox read: refused at the list step, nothing sent", func(t *testing.T) {
		f := newFakeConvHub(t)
		f.listStatus = refuse(http.StatusForbidden)
		b, _, logs := newExecTestBridge(t, f, nil)
		out := runExec(context.Background(), b, "task-d7a", execTestCaller(), false, "the request")
		if out.final != a2a.TaskStateFailed || out.text != replyMsgForbidden {
			t.Fatalf("call ended %v %q, want failed %q", out.final, out.text, replyMsgForbidden)
		}
		if _, _, _, sends := f.counts(); sends != 0 {
			t.Fatalf("sends = %d, want 0", sends)
		}
		assertNeutralLogs(t, logs, "the request")
	})

	t.Run("read refused on an existing conversation: nothing sent", func(t *testing.T) {
		f := newFakeConvHub(t)
		f.seedConversation()
		f.pageStatus = refuse(http.StatusForbidden)
		b, _, logs := newExecTestBridge(t, f, nil)
		out := runExec(context.Background(), b, "task-d7b", execTestCaller(), false, "the request")
		if out.final != a2a.TaskStateFailed || out.text != replyMsgForbidden {
			t.Fatalf("call ended %v %q, want failed %q", out.final, out.text, replyMsgForbidden)
		}
		if list, page, _, sends := f.counts(); sends != 0 || list != 1 || page != 1 {
			t.Fatalf("list/page/send = %d/%d/%d, want 1/1/0", list, page, sends)
		}
		assertNeutralLogs(t, logs, "the request", "an earlier reply")
	})

	t.Run("first contact, conversation not listed after the send: fails at once", func(t *testing.T) {
		f := newFakeConvHub(t)
		f.hideDM = true
		b, _, logs := newExecTestBridge(t, f, func(c *Config) { c.Timeouts.SendMessage = 10 * time.Second })
		start := time.Now()
		out := runExec(context.Background(), b, "task-d7c", execTestCaller(), false, "the request")
		elapsed := time.Since(start)
		if out.final != a2a.TaskStateFailed || out.text != replyMsgNotListed {
			t.Fatalf("call ended %v %q, want failed %q", out.final, out.text, replyMsgNotListed)
		}
		if elapsed > 5*time.Second {
			t.Fatalf("call took %v, want it to fail at the first poll, not at the deadline", elapsed)
		}
		if list, _, _, sends := f.counts(); sends != 1 || list != 2 {
			t.Fatalf("list/send = %d/%d, want 2/1 (preflight, one poll, no retry)", list, sends)
		}
		assertNeutralLogs(t, logs, "the request")
	})

	t.Run("full access: the reply is returned", func(t *testing.T) {
		f := newFakeConvHub(t)
		f.seedConversation()
		b, _, logs := newExecTestBridge(t, f, nil)
		done := runExecAsync(context.Background(), b, "task-d7d", execTestCaller(), false, "the request")
		f.awaitSend(t)
		f.addAgentRow(messages.TypeInstruction, execTestDMKey, "the agent's answer")
		out := awaitOutcome(t, done)
		if out.final != a2a.TaskStateCompleted || out.text != "the agent's answer" {
			t.Fatalf("call ended %v %q, want completed with the agent's answer", out.final, out.text)
		}
		assertNeutralLogs(t, logs, "the request", "the agent's answer", "an earlier reply")
	})

	t.Run("401 on a poll mid-call: fails at once, no admin read", func(t *testing.T) {
		f := newFakeConvHub(t)
		f.seedConversation()
		// Page read 1 is the preflight; read 2 is the first poll.
		f.pageStatus = func(n int) int {
			if n >= 2 {
				return http.StatusUnauthorized
			}
			return 0
		}
		b, _, logs := newExecTestBridge(t, f, func(c *Config) { c.Timeouts.SendMessage = 10 * time.Second })
		start := time.Now()
		out := runExec(context.Background(), b, "task-d7e", execTestCaller(), false, "the request")
		if out.final != a2a.TaskStateFailed || out.text != replyMsgUnauthorized {
			t.Fatalf("call ended %v %q, want failed %q", out.final, out.text, replyMsgUnauthorized)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("call took %v, want it to fail at the first poll", elapsed)
		}
		if _, page, _, _ := f.counts(); page != 2 {
			t.Fatalf("page reads = %d, want 2 (no retry after 401)", page)
		}
		for _, r := range f.snapshot() {
			if strings.HasPrefix(r.path, "/api/v1/conversations") && r.bearer != execTestCallerToken {
				t.Fatalf("conversation read %s %s used another credential", r.method, r.path)
			}
		}
		assertNeutralLogs(t, logs, "the request")
	})

	t.Run("recorded preflight requests: list without a limit, read with after", func(t *testing.T) {
		f := newFakeConvHub(t)
		f.seedConversation()
		f.pageStatus = refuse(http.StatusForbidden)
		b, _, _ := newExecTestBridge(t, f, nil)
		runExec(context.Background(), b, "task-d7f", execTestCaller(), false, "the request")
		var sawList, sawRead bool
		for _, r := range f.snapshot() {
			switch {
			case r.method == http.MethodGet && r.path == "/api/v1/conversations":
				sawList = true
				if r.query.Has("limit") {
					t.Errorf("conversation list carries limit=%q, want no limit", r.query.Get("limit"))
				}
				if r.query.Get("kind") != "direct" {
					t.Errorf("conversation list kind=%q, want direct", r.query.Get("kind"))
				}
			case r.method == http.MethodGet && r.path == "/api/v1/conversations/"+execTestConvID+"/messages":
				sawRead = true
				if r.query.Get("after") == "" || r.query.Get("limit") != "1" {
					t.Errorf("preflight read query %v, want after=<now-1s> and limit=1", r.query)
				}
			}
		}
		if !sawList || !sawRead {
			t.Fatalf("preflight list seen %v, read seen %v; want both", sawList, sawRead)
		}
	})
}

// TestReplyPoll_LegacyWithoutUserIDFailsAtOnce: a call without a caller,
// with hub.user_id unset; the hub stored the message without a
// conversation, so the admin's conversation with the agent is never listed
// and the call fails at once (D6 b).
func TestReplyPoll_LegacyWithoutUserIDFailsAtOnce(t *testing.T) {
	f := newFakeConvHub(t)
	f.hideDM = true
	b, _, logs := newExecTestBridge(t, f, func(c *Config) {
		c.Hub.UserID = ""
		c.Timeouts.SendMessage = 10 * time.Second
	})
	start := time.Now()
	out := runExec(context.Background(), b, "task-d6b", nil, false, "the request")
	if out.final != a2a.TaskStateFailed || out.text != replyMsgNotListed {
		t.Fatalf("call ended %v %q, want failed %q", out.final, out.text, replyMsgNotListed)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("call took %v, want it to fail at the first poll, not at the deadline", elapsed)
	}
	for _, r := range f.snapshot() {
		if r.bearer != execTestAdminToken {
			t.Fatalf("request %s %s used another credential than the admin's", r.method, r.path)
		}
	}
	assertNeutralLogs(t, logs, "the request")
}

// TestReplyPoll_Selection covers which conversation message is the reply
// (D11).
func TestReplyPoll_Selection(t *testing.T) {
	t.Run("a state-change notification row is skipped, the reply is returned", func(t *testing.T) {
		f := newFakeConvHub(t)
		f.seedConversation()
		b, _, _ := newExecTestBridge(t, f, nil)
		done := runExecAsync(context.Background(), b, "task-d11a", execTestCaller(), false, "the request")
		f.awaitSend(t)
		f.addAgentRow(messages.TypeStateChange, "", "helper has reached a state of COMPLETED")
		f.addAgentRow(messages.TypeInstruction, execTestDMKey, "the reply")
		out := awaitOutcome(t, done)
		if out.final != a2a.TaskStateCompleted || out.text != "the reply" {
			t.Fatalf("call ended %v %q, want completed with the reply", out.final, out.text)
		}
	})

	t.Run("a group-set row from the agent is returned", func(t *testing.T) {
		f := newFakeConvHub(t)
		f.seedConversation()
		b, _, _ := newExecTestBridge(t, f, nil)
		done := runExecAsync(context.Background(), b, "task-d11b", execTestCaller(), false, "the request")
		f.awaitSend(t)
		f.addAgentRow(messages.TypeGroupSet, execTestDMKey, "a reply to several recipients")
		out := awaitOutcome(t, done)
		if out.final != a2a.TaskStateCompleted || out.text != "a reply to several recipients" {
			t.Fatalf("call ended %v %q, want completed with the group-set reply", out.final, out.text)
		}
	})

	t.Run("the agent's outbound input-needed row ends the call input-required", func(t *testing.T) {
		f := newFakeConvHub(t)
		f.seedConversation()
		b, _, _ := newExecTestBridge(t, f, nil)
		done := runExecAsync(context.Background(), b, "task-d11c", execTestCaller(), false, "the request")
		f.awaitSend(t)
		f.addAgentRow(messages.TypeInputNeeded, execTestDMKey, "which file do you mean?")
		out := awaitOutcome(t, done)
		if out.final != a2a.TaskStateInputRequired || out.text != "which file do you mean?" {
			t.Fatalf("call ended %v %q, want input-required with the question", out.final, out.text)
		}
	})

	t.Run("a WAITING_FOR_INPUT notification row alone is skipped; the call keeps waiting", func(t *testing.T) {
		f := newFakeConvHub(t)
		f.seedConversation()
		b, _, _ := newExecTestBridge(t, f, func(c *Config) { c.Timeouts.SendMessage = 3 * time.Second })
		done := runExecAsync(context.Background(), b, "task-d11d", execTestCaller(), false, "the request")
		f.awaitSend(t)
		f.addAgentRow(messages.TypeInputNeeded, "", "helper is waiting for input: a question for someone else")
		out := awaitOutcome(t, done)
		want := fmt.Sprintf("Timeout waiting for agent response after %v", 3*time.Second)
		if out.final != a2a.TaskStateFailed || out.text != want {
			t.Fatalf("call ended %v %q, want failed %q", out.final, out.text, want)
		}
	})
}

// TestReplyPoll_SameCallerConcurrency covers the same-caller check (D13).
func TestReplyPoll_SameCallerConcurrency(t *testing.T) {
	t.Run("a second concurrent call by the same caller is refused at once", func(t *testing.T) {
		f := newFakeConvHub(t)
		f.seedConversation()
		b, _, _ := newExecTestBridge(t, f, func(c *Config) { c.Timeouts.SendMessage = 10 * time.Second })
		first := runExecAsync(context.Background(), b, "task-d13-1", execTestCaller(), false, "the first request")
		f.awaitSend(t)
		start := time.Now()
		second := runExec(context.Background(), b, "task-d13-2", execTestCaller(), false, "the second request")
		if second.final != a2a.TaskStateFailed || second.text != replyMsgBusy {
			t.Fatalf("second call ended %v %q, want failed %q", second.final, second.text, replyMsgBusy)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("second call took %v, want an immediate refusal", elapsed)
		}
		if _, _, _, sends := f.counts(); sends != 1 {
			t.Fatalf("sends = %d, want 1 (the second call sends nothing)", sends)
		}
		f.addAgentRow(messages.TypeInstruction, execTestDMKey, "the first reply")
		if out := awaitOutcome(t, first); out.final != a2a.TaskStateCompleted || out.text != "the first reply" {
			t.Fatalf("first call ended %v %q, want completed with the first reply", out.final, out.text)
		}
		// The check is released when the call ends.
		third := runExecAsync(context.Background(), b, "task-d13-3", execTestCaller(), false, "the third request")
		f.awaitSend(t)
		f.addAgentRow(messages.TypeInstruction, execTestDMKey, "the third reply")
		if out := awaitOutcome(t, third); out.final != a2a.TaskStateCompleted || out.text != "the third reply" {
			t.Fatalf("third call ended %v %q, want completed with the third reply", out.final, out.text)
		}
	})

	t.Run("a concurrent call by a different user caller is not refused", func(t *testing.T) {
		f := newFakeConvHub(t)
		f.seedConversation()
		b, _, _ := newExecTestBridge(t, f, func(c *Config) { c.Timeouts.SendMessage = 3 * time.Second })
		first := runExecAsync(context.Background(), b, "task-d13-other-1", execTestCaller(), false, "the first request")
		f.awaitSend(t)
		other := &CallerIdentity{
			UserID: "other-caller-uuid-exec", Email: "other@example.com", Role: "member",
			RawToken: "other-caller-credential-exec", TokenType: "uat",
		}
		second := runExecAsync(context.Background(), b, "task-d13-other-2", other, false, "the other caller's request")
		f.awaitSend(t)
		if _, _, _, sends := f.counts(); sends != 2 {
			t.Fatalf("sends = %d, want 2 (the other caller's call is sent)", sends)
		}
		f.addAgentRow(messages.TypeInstruction, execTestDMKey, "the first reply")
		if out := awaitOutcome(t, first); out.final != a2a.TaskStateCompleted || out.text != "the first reply" {
			t.Fatalf("first call ended %v %q, want completed with the first reply", out.final, out.text)
		}
		if out := awaitOutcome(t, second); out.text == replyMsgBusy {
			t.Fatalf("the other caller's call was refused as a concurrent call by the same caller")
		}
	})

	t.Run("with reply_poll.enabled false the second call is not refused", func(t *testing.T) {
		f := newFakeConvHub(t)
		f.seedConversation()
		disabled := false
		b, _, _ := newExecTestBridge(t, f, func(c *Config) {
			c.ReplyPoll.Enabled = &disabled
			c.Timeouts.SendMessage = 2 * time.Second
		})
		first := runExecAsync(context.Background(), b, "task-d13-off-1", execTestCaller(), false, "the first request")
		f.awaitSend(t)
		second := runExec(context.Background(), b, "task-d13-off-2", execTestCaller(), false, "the second request")
		if second.text == replyMsgBusy {
			t.Fatalf("second call was refused with the switch off")
		}
		awaitOutcome(t, first)
		if _, _, _, sends := f.counts(); sends != 2 {
			t.Fatalf("sends = %d, want 2", sends)
		}
	})

	t.Run("the agent's answer to the caller's message on another channel ends the call", func(t *testing.T) {
		f := newFakeConvHub(t)
		f.seedConversation()
		b, _, _ := newExecTestBridge(t, f, nil)
		done := runExecAsync(context.Background(), b, "task-d13-interleave", execTestCaller(), false, "the request")
		f.awaitSend(t)
		f.addCallerRow("a message from the web chat")
		f.addAgentRow(messages.TypeInstruction, execTestDMKey, "the answer to the web message")
		out := awaitOutcome(t, done)
		if out.final != a2a.TaskStateCompleted || out.text != "the answer to the web message" {
			t.Fatalf("call ended %v %q, want completed with the answer to the web message", out.final, out.text)
		}
	})
}

// TestReplyPoll_ContinuationReturnsSecondReply: plugin mode, an
// input-required call continued on the same task returns the second
// reply, not the first turn's (D14).
func TestReplyPoll_ContinuationReturnsSecondReply(t *testing.T) {
	f := newFakeConvHub(t)
	f.seedConversation()
	b, _, _ := newExecTestBridge(t, f, nil)
	const taskID = "task-d14"

	done := runExecAsync(context.Background(), b, taskID, execTestCaller(), false, "the request")
	f.awaitSend(t)
	f.addAgentRow(messages.TypeInputNeeded, execTestDMKey, "the first turn's question")
	if out := awaitOutcome(t, done); out.final != a2a.TaskStateInputRequired || out.text != "the first turn's question" {
		t.Fatalf("first turn ended %v %q, want input-required with the question", out.final, out.text)
	}

	done = runExecAsync(context.Background(), b, taskID, execTestCaller(), true, "the answer to the question")
	f.awaitSend(t)
	f.addAgentRow(messages.TypeInstruction, execTestDMKey, "the second turn's reply")
	out := awaitOutcome(t, done)
	if out.final != a2a.TaskStateCompleted || out.text != "the second turn's reply" {
		t.Fatalf("second turn ended %v %q, want completed with the second reply", out.final, out.text)
	}
}

// TestReplyPoll_EmptyAnchor covers sends without a message ID (D15).
func TestReplyPoll_EmptyAnchor(t *testing.T) {
	t.Run("delivered but not stored: fails at once", func(t *testing.T) {
		f := newFakeConvHub(t)
		f.seedConversation()
		f.omitMessageID = true
		f.sendStatus = sendStatusNotPersisted
		b, _, _ := newExecTestBridge(t, f, func(c *Config) { c.Timeouts.SendMessage = 10 * time.Second })
		start := time.Now()
		out := runExec(context.Background(), b, "task-d15a", execTestCaller(), false, "the request")
		if out.final != a2a.TaskStateFailed || out.text != replyMsgNotRecorded {
			t.Fatalf("call ended %v %q, want failed %q", out.final, out.text, replyMsgNotRecorded)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("call took %v, want an immediate failure", elapsed)
		}
	})

	t.Run("no message ID: no poller, no mark, a pushed question ends the call", func(t *testing.T) {
		f := newFakeConvHub(t)
		f.seedConversation()
		f.omitMessageID = true
		b, _, logs := newExecTestBridge(t, f, nil)
		const taskID = "task-d15b"
		done := runExecAsync(context.Background(), b, taskID, execTestCaller(), false, "the request")
		f.awaitSend(t)
		// The executor clears the mark right after the send answers.
		deadline := time.Now().Add(2 * time.Second)
		for b.taskPollerLive(taskID) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if b.taskPollerLive(taskID) {
			t.Fatal("task is still marked as having a live poller without an anchor")
		}
		_, pagesBefore, getsBefore, _ := f.counts()
		push := &messages.StructuredMessage{
			Version: 1, Sender: "agent:" + execTestAgentSlug, Type: messages.TypeInputNeeded,
			Msg: "a pushed question", Timestamp: time.Now().UTC().Format(time.RFC3339),
			Metadata: map[string]string{"a2aTaskId": taskID},
		}
		if err := b.HandleBrokerMessage(context.Background(), projectkeys.UserTopic(execTestProjectID, execTestCallerID), push); err != nil {
			t.Fatalf("HandleBrokerMessage: %v", err)
		}
		out := awaitOutcome(t, done)
		if out.final != a2a.TaskStateInputRequired || out.text != "a pushed question" {
			t.Fatalf("call ended %v %q, want input-required with the pushed question", out.final, out.text)
		}
		if _, pages, gets, _ := f.counts(); pages != pagesBefore || gets != getsBefore {
			t.Fatalf("conversation reads after the send: pages %d->%d, anchor reads %d->%d; want none", pagesBefore, pages, getsBefore, gets)
		}
		// No poller: the pushed question reached the accepted-message line,
		// which carries no text (D9).
		if !strings.Contains(logs.String(), "handling broker message") {
			t.Fatalf("the pushed question did not reach the accepted-message line; logs:\n%s", logs.String())
		}
		assertNeutralLogs(t, logs, "the request", "a pushed question", "an earlier reply")
	})
}

// TestReplyPoll_RollbackSwitch: with reply_poll.enabled false the call
// waits on pushed messages as before (no same-caller check, preflight,
// poller or filter), and the push path's user check still applies (D16).
func TestReplyPoll_RollbackSwitch(t *testing.T) {
	f := newFakeConvHub(t)
	f.seedConversation()
	disabled := false
	b, _, logs := newExecTestBridge(t, f, func(c *Config) { c.ReplyPoll.Enabled = &disabled })
	const taskID = "task-d16"
	done := runExecAsync(context.Background(), b, taskID, execTestCaller(), false, "the request")
	f.awaitSend(t)
	f.addAgentRow(messages.TypeInstruction, execTestDMKey, "a reply in the conversation")

	pushed := func(text string) *messages.StructuredMessage {
		return &messages.StructuredMessage{
			Version: 1, Sender: "agent:" + execTestAgentSlug, Type: messages.TypeInstruction,
			Msg: text, Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			Metadata: map[string]string{"a2aTaskId": taskID},
		}
	}
	// Another user's topic: dropped by the user check.
	if err := b.HandleBrokerMessage(context.Background(), projectkeys.UserTopic(execTestProjectID, "another-user"), pushed("another user's reply")); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}
	if err := b.HandleBrokerMessage(context.Background(), projectkeys.UserTopic(execTestProjectID, execTestCallerID), pushed("the pushed reply")); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}
	out := awaitOutcome(t, done)
	if out.final != a2a.TaskStateCompleted || out.text != "the pushed reply" {
		t.Fatalf("call ended %v %q, want completed with the pushed reply", out.final, out.text)
	}
	for _, r := range f.snapshot() {
		if strings.HasPrefix(r.path, "/api/v1/conversations") {
			t.Fatalf("conversation request %s %s with the switch off, want none", r.method, r.path)
		}
	}
	// No poller: the accepted push reached the accepted-message line, which
	// carries no text; the drop line for the other topic neither (D9).
	if !strings.Contains(logs.String(), "handling broker message") || !strings.Contains(logs.String(), "reason="+pushDropTopicUser) {
		t.Fatalf("missing the accepted-message line or the topic_user drop line; logs:\n%s", logs.String())
	}
	assertNeutralLogs(t, logs, "the request", "the pushed reply", "another user's reply", "a reply in the conversation", "another-user")
}

// recordingHostCallbacks records the subscriptions the bridge requests.
type recordingHostCallbacks struct {
	mu       sync.Mutex
	patterns []string
}

func (r *recordingHostCallbacks) RequestSubscription(pattern string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.patterns = append(r.patterns, pattern)
	return nil
}

func (r *recordingHostCallbacks) CancelSubscription(string) error { return nil }

func (r *recordingHostCallbacks) has(pattern string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.patterns {
		if p == pattern {
			return true
		}
	}
	return false
}

// TestAdminSubscriptionUsesAdminUserID: the admin subscription is on the
// admin user ID's topic, where the hub publishes replies to the admin, and
// a reply on that topic is accepted for a call without a caller (D16, O8).
func TestAdminSubscriptionUsesAdminUserID(t *testing.T) {
	store, err := state.NewSQLite(filepath.Join(t.TempDir(), "admin-topic.db"))
	if err != nil {
		t.Fatalf("state store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := &Config{Hub: HubConfig{User: "bridge-admin@example.com", UserID: "admin-user-id"}}
	b := New(store, nil, nil, cfg, nil, slog.New(slog.NewTextHandler(&syncBuffer{}, nil)))
	t.Cleanup(b.Shutdown)
	broker := NewBrokerServer(b.HandleBrokerMessage, b.log, context.Background())
	hc := &recordingHostCallbacks{}
	broker.SetHostCallbacks(hc)
	b.SetBroker(broker)

	b.subscribeAdminUserTopics("proj1")
	if want := projectkeys.UserTopic("proj1", "admin-user-id"); !hc.has(want) {
		t.Fatalf("admin subscription patterns %v, want %q", hc.patterns, want)
	}
	if hc.has(projectkeys.UserTopic("proj1", "bridge-admin@example.com")) {
		t.Fatalf("admin subscription on the admin's email topic: %v", hc.patterns)
	}

	b.registerActiveTask("legacy-task", agentKey("proj1", "helper"))
	msg := &messages.StructuredMessage{Sender: "agent:helper", Type: messages.TypeInstruction, Msg: "a reply"}
	got, err := b.correlateToTask(context.Background(), "proj1", "helper", projectkeys.UserTopic("proj1", "admin-user-id"), msg)
	if err != nil || got != "legacy-task" {
		t.Fatalf("reply on the admin ID topic correlated to %q, %v; want legacy-task", got, err)
	}
}

// TestReplyPoll_ExecutorJoinsPollerBeforeYield: when the deadline ends a
// call while a poll read is in flight, the poller has exited (its read
// slot released) before the final event is yielded (D8, join).
func TestReplyPoll_ExecutorJoinsPollerBeforeYield(t *testing.T) {
	f := newFakeConvHub(t)
	f.seedConversation()
	release := make(chan struct{})
	defer close(release)
	// Every poll read after the preflight blocks until the test returns.
	f.pageStatus = func(n int) int {
		if n >= 2 {
			<-release
		}
		return 0
	}
	b, _, _ := newExecTestBridge(t, f, func(c *Config) { c.Timeouts.SendMessage = 2 * time.Second })
	out := runExec(context.Background(), b, "task-d8-join", execTestCaller(), false, "the request")
	if out.final != a2a.TaskStateFailed {
		t.Fatalf("call ended %v %q, want failed at the deadline", out.final, out.text)
	}
	if out.slotsHeldAtFinal != 0 {
		t.Fatalf("%d poll read slots held when the final event was yielded, want 0", out.slotsHeldAtFinal)
	}
}

// TestExecutor_PolledWaitIgnoresAppendedPushEvent: while a local caller's
// call waits with a live reply poller, an agent message appended to the
// task without the poller's mark (as another bridge process that saw the
// pushed copy would append it) does not end the call; the call ends with
// the polled reply.
func TestExecutor_PolledWaitIgnoresAppendedPushEvent(t *testing.T) {
	f := newFakeConvHub(t)
	f.seedConversation()
	b, store, _ := newExecTestBridge(t, f, nil)
	const taskID = "task-polled-wait-appended"

	done := runExecAsync(context.Background(), b, taskID, execTestCaller(), false, "the request")
	f.awaitSend(t)
	if !b.taskPollerLive(taskID) {
		t.Fatal("task is not marked as having a live poller after the send")
	}

	// Append the agent's message directly, past the push path's drop.
	b.processAndAppendEvent(context.Background(), taskID, execTestAgentSlug, &messages.StructuredMessage{
		Version: 1, Sender: "agent:" + execTestAgentSlug, Type: messages.TypeInstruction,
		Msg: "an appended copy", Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}, "")

	// The wait reads the log every 100ms; give it several reads.
	select {
	case out := <-done:
		t.Fatalf("call ended on the appended event: %v %q (states %v), want it still waiting", out.final, out.text, out.states)
	case <-time.After(700 * time.Millisecond):
	}

	f.addAgentRow(messages.TypeInstruction, execTestDMKey, "the polled reply")
	start := time.Now()
	out := awaitOutcome(t, done)
	if out.final != a2a.TaskStateCompleted || out.text != "the polled reply" {
		t.Fatalf("call ended %v %q (states %v), want completed with the polled reply", out.final, out.text, out.states)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("polled reply took %v to end the call, want it delivered promptly", elapsed)
	}

	// The appended event is in the task's log ahead of the poller's reply,
	// so the wait read it and skipped it.
	events, err := store.ReadTaskEvents(context.Background(), taskID, 0, 100)
	if err != nil {
		t.Fatalf("ReadTaskEvents: %v", err)
	}
	appendedAt, polledAt := -1, -1
	for i, ev := range events {
		if !isResponseEvent(ev) {
			continue
		}
		if strings.HasPrefix(ev.DedupKey, replyPollDedupPrefix) {
			if polledAt < 0 {
				polledAt = i
			}
		} else if appendedAt < 0 {
			appendedAt = i
		}
	}
	if appendedAt < 0 || polledAt < 0 || appendedAt > polledAt {
		t.Fatalf("task log response events: appended at %d, polled at %d; want the appended event before the polled reply", appendedAt, polledAt)
	}
}
