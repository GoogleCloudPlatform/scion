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

package chatapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/extras/scion-chat-app/internal/identity"
	"github.com/GoogleCloudPlatform/scion/extras/scion-chat-app/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"gopkg.in/yaml.v3"
)

// Tests for the async create opt-in and the launch watchers
// (ptone/scion#4671, phase F1). Fixtures avoid the not_launched end reason.

// --- fixtures ---

func launchingAgent(slug string) *hubclient.Agent {
	return &hubclient.Agent{
		ID: "id-" + slug, Slug: slug, Name: slug, Phase: "provisioning",
		Launch: &hubclient.AgentLaunch{ID: "launch-" + slug, State: "active", Active: true, Kind: "create", Step: "pod_create"},
	}
}

func runningAgent(slug string) *hubclient.Agent {
	return &hubclient.Agent{
		ID: "id-" + slug, Slug: slug, Name: slug, Phase: "running",
		Launch: &hubclient.AgentLaunch{ID: "launch-" + slug, State: "ended", Kind: "create", EndReason: "succeeded"},
	}
}

func failedAgent(slug string) *hubclient.Agent {
	return &hubclient.Agent{
		ID: "id-" + slug, Slug: slug, Name: slug, Phase: "error", Message: "image pull failed",
		Launch: &hubclient.AgentLaunch{ID: "launch-" + slug, State: "ended", Kind: "create", Error: "image_pull_failed", EndReason: "failed"},
	}
}

// recorder is an ordered, concurrency-safe log of what the router did.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fmt.Sprintf(format, args...))
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// index returns the position of the first event with the prefix, -1 if none.
func (r *recorder) index(prefix string) int {
	for i, e := range r.snapshot() {
		if strings.HasPrefix(e, prefix) {
			return i
		}
	}
	return -1
}

// syncMessenger is a Messenger safe for use from launch watchers. A call
// with a done context is refused and not recorded.
type syncMessenger struct {
	rec     *recorder
	mu      sync.Mutex
	n       int
	sends   []SendMessageRequest
	updates []SendMessageRequest
	names   []string
	failAll bool
	// holdEnter and holdLeave, when set, block the first SendMessage
	// (closing holdEnter) until holdLeave is closed.
	holdEnter chan struct{}
	holdLeave chan struct{}
	held      bool
}

func (m *syncMessenger) SendMessage(ctx context.Context, req SendMessageRequest) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	m.mu.Lock()
	if m.holdEnter != nil && !m.held {
		m.held = true
		m.mu.Unlock()
		close(m.holdEnter)
		<-m.holdLeave
		m.mu.Lock()
	}
	defer m.mu.Unlock()
	if m.failAll {
		return "", errors.New("chat api down")
	}
	m.n++
	name := fmt.Sprintf("spaces/test/messages/m%d", m.n)
	m.sends = append(m.sends, req)
	if req.Card != nil {
		m.rec.add("post-card %s", cardStatus(req.Card))
	} else {
		m.rec.add("post %s|%s", req.ThreadID, req.Text)
	}
	return name, nil
}

func (m *syncMessenger) SendCard(ctx context.Context, spaceID string, card Card) (string, error) {
	return m.SendMessage(ctx, SendMessageRequest{SpaceID: spaceID, Card: &card})
}

func (m *syncMessenger) UpdateMessage(ctx context.Context, name string, req SendMessageRequest) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updates = append(m.updates, req)
	m.names = append(m.names, name)
	m.rec.add("update %s %s", name, cardStatus(req.Card))
	return nil
}

func (m *syncMessenger) OpenDialog(context.Context, string, Dialog) error   { return nil }
func (m *syncMessenger) UpdateDialog(context.Context, string, Dialog) error { return nil }
func (m *syncMessenger) GetUser(context.Context, string) (*ChatUser, error) { return nil, nil }
func (m *syncMessenger) SetAgentIdentity(context.Context, AgentIdentity) error {
	return nil
}
func (m *syncMessenger) UploadMedia(context.Context, string, string, io.Reader) (string, error) {
	return "", nil
}

func (m *syncMessenger) posts() []SendMessageRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]SendMessageRequest(nil), m.sends...)
}

func cardStatus(c *Card) string {
	if c == nil || len(c.Sections) == 0 {
		return ""
	}
	for _, w := range c.Sections[0].Widgets {
		if w.Label == "Status" {
			return w.Content
		}
	}
	return ""
}

// asyncHub is a scripted agent service: Get returns script[i] for the i-th
// call and the last entry after that; hold, when set, blocks the Get call
// with that index until released.
type asyncHub struct {
	rec *recorder

	mu         sync.Mutex
	script     []*hubclient.Agent
	getErr     error
	gets       int
	holdAt     int
	holdEnter  chan struct{}
	holdLeave  chan struct{}
	createReqs []*hubclient.CreateAgentRequest
	createResp *hubclient.Agent
	starts     int
	sends      []*messages.StructuredMessage
}

func newAsyncHub(rec *recorder, created *hubclient.Agent, script ...*hubclient.Agent) *asyncHub {
	return &asyncHub{rec: rec, createResp: created, script: script, holdAt: -1}
}

func (h *asyncHub) client() *stubClient {
	c := newStubClient()
	c.agents.getFunc = h.get
	c.agents.createFunc = func(_ context.Context, req *hubclient.CreateAgentRequest) (*hubclient.CreateAgentResponse, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.createReqs = append(h.createReqs, req)
		return &hubclient.CreateAgentResponse{Agent: h.createResp}, nil
	}
	c.agents.startFunc = func(context.Context, string) error {
		h.mu.Lock()
		h.starts++
		h.mu.Unlock()
		h.rec.add("start")
		return nil
	}
	c.agents.sendStructuredMessageFunc = func(_ context.Context, id string, msg *messages.StructuredMessage, _, _, _ bool) (*hubclient.MessageResponse, error) {
		h.mu.Lock()
		h.sends = append(h.sends, msg)
		h.mu.Unlock()
		h.rec.add("send %s|%s", id, msg.Msg)
		return &hubclient.MessageResponse{}, nil
	}
	return c
}

func (h *asyncHub) get(ctx context.Context, id string) (*hubclient.Agent, error) {
	h.mu.Lock()
	i := h.gets
	h.gets++
	hold := i == h.holdAt
	var a *hubclient.Agent
	if len(h.script) > 0 {
		a = h.script[min(i, len(h.script)-1)]
	}
	err := h.getErr
	h.mu.Unlock()
	if hold {
		close(h.holdEnter)
		select {
		case <-h.holdLeave:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, errors.New("agent not found")
	}
	h.rec.add("get %s", a.Phase)
	cp := *a
	return &cp, nil
}

func (h *asyncHub) setScript(script ...*hubclient.Agent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.script = script
	h.gets = 0
}

func (h *asyncHub) holdGet(i int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.holdAt = i
	h.holdEnter = make(chan struct{})
	h.holdLeave = make(chan struct{})
}

func (h *asyncHub) lastCreate(t *testing.T) *hubclient.CreateAgentRequest {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.createReqs) == 0 {
		t.Fatal("no create request")
	}
	return h.createReqs[len(h.createReqs)-1]
}

func (h *asyncHub) sendCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sends)
}

func (h *asyncHub) getCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gets
}

// newAsyncRouter builds a router with a linked space, a registered user
// (for the relays, which resolve the user through the identity mapper),
// a broker, a concurrency-safe messenger and a short poll interval.
func newAsyncRouter(t *testing.T, client *stubClient, rec *recorder) (*CommandRouter, *syncMessenger) {
	t.Helper()
	router, _, store := newTestRouterWithHub(t, client)
	if err := store.SetUserMapping(&state.UserMapping{
		PlatformUserID: "user-1",
		Platform:       "googlechat",
		HubUserID:      "hub-user-1",
		HubUserEmail:   "user@example.com",
		RegisteredBy:   "manual",
	}); err != nil {
		t.Fatalf("seeding user mapping: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router.idMapper = identity.NewMapper(store, nil, router.hubURL, nil, log)
	router.broker = NewBrokerServer(nil, log)
	router.launchPollInterval = 5 * time.Millisecond
	m := &syncMessenger{rec: rec}
	router.messenger = m
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = router.Shutdown(ctx)
	})
	return router, m
}

// waitWatchers waits for every launch watcher to return.
func waitWatchers(t *testing.T, r *CommandRouter) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		r.bg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("launch watchers did not finish")
	}
}

func threadEvent() *ChatEvent {
	e := testEvent()
	e.ThreadID = "spaces/test/threads/t1"
	return e
}

// --- config ---

func TestConfig_AsyncCreateDefaultTrue(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want bool
	}{
		{"hub:\n  endpoint: x\n", true},
		{"hub:\n  async_create: true\n", true},
		{"hub:\n  async_create: false\n", false},
	} {
		var cfg Config
		if err := yaml.Unmarshal([]byte(tc.yaml), &cfg); err != nil {
			t.Fatalf("unmarshal %q: %v", tc.yaml, err)
		}
		if got := cfg.AsyncCreateEnabled(); got != tc.want {
			t.Errorf("AsyncCreateEnabled for %q = %v, want %v", tc.yaml, got, tc.want)
		}
	}
}

// --- cmdCreate ---

func TestCmdCreate_SendsAsyncOptIn(t *testing.T) {
	rec := &recorder{}
	hub := newAsyncHub(rec, runningAgent("a1"))
	router, _ := newAsyncRouter(t, hub.client(), rec)
	if _, err := router.cmdCreate(context.Background(), testEvent(), []string{"a1"}); err != nil {
		t.Fatal(err)
	}
	if !hub.lastCreate(t).AcceptAsyncLaunch {
		t.Error("create did not send AcceptAsyncLaunch by default")
	}
}

func TestCmdCreate_AsyncCreateFalse_NoOptInSyncReply(t *testing.T) {
	rec := &recorder{}
	hub := newAsyncHub(rec, runningAgent("a1"))
	router, _ := newAsyncRouter(t, hub.client(), rec)
	router.SetAsyncCreate(false)
	resp, err := router.cmdCreate(context.Background(), testEvent(), []string{"a1"})
	if err != nil {
		t.Fatal(err)
	}
	if hub.lastCreate(t).AcceptAsyncLaunch {
		t.Error("async_create false: create still sent AcceptAsyncLaunch")
	}
	if want := "Agent `a1` created (ID: `id-a1`)."; resp.Message.Text != want {
		t.Errorf("reply = %q, want %q", resp.Message.Text, want)
	}
}

func TestCmdCreate_SyncAnswerUnchanged(t *testing.T) {
	rec := &recorder{}
	hub := newAsyncHub(rec, runningAgent("a1"), runningAgent("a1"))
	router, m := newAsyncRouter(t, hub.client(), rec)
	resp, err := router.cmdCreate(context.Background(), threadEvent(), []string{"a1"})
	if err != nil {
		t.Fatal(err)
	}
	waitWatchers(t, router)
	if want := "Agent `a1` created (ID: `id-a1`)."; resp.Message.Text != want {
		t.Errorf("reply = %q, want %q", resp.Message.Text, want)
	}
	if n := hub.getCount(); n != 0 {
		t.Errorf("sync answer: %d agent reads, want 0", n)
	}
	if p := m.posts(); len(p) != 0 {
		t.Errorf("sync answer: %d follow-up posts, want 0: %+v", len(p), p)
	}
}

func TestCmdCreate_AsyncPostsRunningInSameThread(t *testing.T) {
	rec := &recorder{}
	hub := newAsyncHub(rec, launchingAgent("a1"), launchingAgent("a1"), runningAgent("a1"))
	router, m := newAsyncRouter(t, hub.client(), rec)
	resp, err := router.cmdCreate(context.Background(), threadEvent(), []string{"a1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.Message.Text, "Creating agent `a1`") {
		t.Errorf("immediate reply = %q, want a 'Creating agent' reply", resp.Message.Text)
	}
	waitWatchers(t, router)
	posts := m.posts()
	if len(posts) != 1 {
		t.Fatalf("got %d follow-up posts, want 1: %+v", len(posts), posts)
	}
	p := posts[0]
	if p.SpaceID != "spaces/test" || p.ThreadID != "spaces/test/threads/t1" {
		t.Errorf("follow-up posted to %q/%q, want spaces/test and the command's thread", p.SpaceID, p.ThreadID)
	}
	if !strings.Contains(p.Text, "is running") {
		t.Errorf("follow-up = %q, want a running post", p.Text)
	}
}

func TestCmdCreate_AsyncPostsFailure(t *testing.T) {
	rec := &recorder{}
	hub := newAsyncHub(rec, launchingAgent("a1"), launchingAgent("a1"), failedAgent("a1"))
	router, m := newAsyncRouter(t, hub.client(), rec)
	if _, err := router.cmdCreate(context.Background(), threadEvent(), []string{"a1"}); err != nil {
		t.Fatal(err)
	}
	waitWatchers(t, router)
	posts := m.posts()
	if len(posts) != 1 {
		t.Fatalf("got %d follow-up posts, want 1: %+v", len(posts), posts)
	}
	want := "Agent `a1` failed to start (image_pull_failed): image pull failed"
	if posts[0].Text != want {
		t.Errorf("failure post = %q, want %q", posts[0].Text, want)
	}
	if posts[0].ThreadID != "spaces/test/threads/t1" {
		t.Errorf("failure posted to thread %q, want the command's thread", posts[0].ThreadID)
	}
}

// TestCmdCreate_PollsWithCreatorClient checks that the launch watcher reads
// the agent with the client that made the create, not the admin client.
func TestCmdCreate_PollsWithCreatorClient(t *testing.T) {
	rec := &recorder{}
	hub := newAsyncHub(rec, launchingAgent("a1"), launchingAgent("a1"), runningAgent("a1"))
	router, _ := newAsyncRouter(t, hub.client(), rec)
	adminRec := &recorder{}
	admin := newAsyncHub(adminRec, nil, runningAgent("a1"))
	router.adminClient = admin.client()
	if _, err := router.cmdCreate(context.Background(), threadEvent(), []string{"a1"}); err != nil {
		t.Fatal(err)
	}
	waitWatchers(t, router)
	if n := admin.getCount(); n != 0 {
		t.Errorf("admin client read the agent %d times, want 0", n)
	}
	if n := hub.getCount(); n < 2 {
		t.Errorf("creator client read the agent %d times, want at least 2 (launching, running)", n)
	}
}

// --- cmdThread ---

func TestCmdThread_SendsAsyncOptIn(t *testing.T) {
	rec := &recorder{}
	hub := newAsyncHub(rec, runningAgent("a1"))
	router, _ := newAsyncRouter(t, hub.client(), rec)
	if _, err := router.cmdThread(context.Background(), threadEvent(), []string{"a1"}); err != nil {
		t.Fatal(err)
	}
	if !hub.lastCreate(t).AcceptAsyncLaunch {
		t.Error("thread create did not send AcceptAsyncLaunch by default")
	}
}

func TestCmdThread_AsyncSkipsStart_CardStartingThenRunning(t *testing.T) {
	rec := &recorder{}
	hub := newAsyncHub(rec, launchingAgent("a1"), launchingAgent("a1"), runningAgent("a1"))
	router, m := newAsyncRouter(t, hub.client(), rec)
	resp, err := router.cmdThread(context.Background(), threadEvent(), []string{"a1"})
	if err != nil {
		t.Fatal(err)
	}
	waitWatchers(t, router)
	if hub.starts != 0 {
		t.Errorf("async create: Start called %d times, want 0", hub.starts)
	}
	if resp != nil {
		t.Errorf("async thread: want no synchronous response (the card is posted), got %+v", resp)
	}
	if i := rec.index("post-card Starting…"); i < 0 {
		t.Fatalf("no card with status Starting… posted: %v", rec.snapshot())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.updates) != 1 || cardStatus(m.updates[0].Card) != "Running" {
		t.Fatalf("card updates = %v, want one update to Running", rec.snapshot())
	}
	if m.names[0] != "spaces/test/messages/m2" {
		t.Errorf("updated message %q, want the posted card spaces/test/messages/m2", m.names[0])
	}
}

func TestCmdThread_AsyncInstructionOnlyAfterRunning(t *testing.T) {
	rec := &recorder{}
	hub := newAsyncHub(rec, launchingAgent("a1"), launchingAgent("a1"), launchingAgent("a1"), runningAgent("a1"))
	router, _ := newAsyncRouter(t, hub.client(), rec)
	if _, err := router.cmdThread(context.Background(), threadEvent(), []string{"a1", "do", "X"}); err != nil {
		t.Fatal(err)
	}
	waitWatchers(t, router)
	running := rec.index("get running")
	send := rec.index("send a1|do X")
	if running < 0 || send < 0 {
		t.Fatalf("missing running read or instruction send: %v", rec.snapshot())
	}
	if send < running {
		t.Errorf("instruction sent before the agent read as running: %v", rec.snapshot())
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if th := hub.sends[0].ThreadID; th != "spaces/test/threads/m1" {
		t.Errorf("instruction thread = %q, want the kickoff thread spaces/test/threads/m1", th)
	}
}

func TestCmdThread_AsyncFailure_NoSend_FailurePosted(t *testing.T) {
	rec := &recorder{}
	hub := newAsyncHub(rec, launchingAgent("a1"), launchingAgent("a1"), failedAgent("a1"))
	router, m := newAsyncRouter(t, hub.client(), rec)
	if _, err := router.cmdThread(context.Background(), threadEvent(), []string{"a1", "do", "X"}); err != nil {
		t.Fatal(err)
	}
	waitWatchers(t, router)
	if n := hub.sendCount(); n != 0 {
		t.Errorf("failed launch: %d sends, want 0", n)
	}
	var failure *SendMessageRequest
	for _, p := range m.posts() {
		if strings.Contains(p.Text, "failed to start (image_pull_failed)") {
			p := p
			failure = &p
		}
	}
	if failure == nil {
		t.Fatalf("no failure post: %v", rec.snapshot())
	}
	if failure.ThreadID != "spaces/test/threads/m1" {
		t.Errorf("failure posted to %q, want the agent's thread", failure.ThreadID)
	}
	if rec.index("update spaces/test/messages/m2 Failed to start") < 0 {
		t.Errorf("card not updated to Failed to start: %v", rec.snapshot())
	}
}

func TestCmdThread_SyncAnswerStillStarts(t *testing.T) {
	rec := &recorder{}
	hub := newAsyncHub(rec, runningAgent("a1"))
	router, _ := newAsyncRouter(t, hub.client(), rec)
	resp, err := router.cmdThread(context.Background(), threadEvent(), []string{"a1"})
	if err != nil {
		t.Fatal(err)
	}
	if hub.starts != 1 {
		t.Errorf("sync create: Start called %d times, want 1", hub.starts)
	}
	if resp == nil || resp.Message == nil || cardStatus(resp.Message.Card) != "Running" {
		t.Errorf("sync create: want the Running card as the response, got %+v", resp)
	}
}

// --- relays ---

func TestCmdMessage_RelayWaitsForLaunch(t *testing.T) {
	rec := &recorder{}
	hub := newAsyncHub(rec, nil, launchingAgent("a1"), launchingAgent("a1"), runningAgent("a1"))
	router, m := newAsyncRouter(t, hub.client(), rec)
	resp, err := router.cmdMessage(context.Background(), threadEvent(), []string{"a1", "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if want := launchStartingNotice("a1"); resp.Message.Text != want {
		t.Errorf("reply = %q, want the starting notice %q", resp.Message.Text, want)
	}
	waitWatchers(t, router)
	running := rec.index("get running")
	send := rec.index("send a1|hello")
	if running < 0 || send < 0 || send < running {
		t.Fatalf("message not sent after the agent read as running: %v", rec.snapshot())
	}
	posts := m.posts()
	if len(posts) != 1 || !strings.Contains(posts[0].Text, "sent to *a1*") || posts[0].ThreadID != "spaces/test/threads/t1" {
		t.Errorf("want one 'sent' post in the command's thread, got %+v", posts)
	}
}

func TestCmdMessage_RelayLaunchFailed_NoSend(t *testing.T) {
	rec := &recorder{}
	hub := newAsyncHub(rec, nil, launchingAgent("a1"), failedAgent("a1"))
	router, m := newAsyncRouter(t, hub.client(), rec)
	if _, err := router.cmdMessage(context.Background(), threadEvent(), []string{"a1", "hello"}); err != nil {
		t.Fatal(err)
	}
	waitWatchers(t, router)
	if n := hub.sendCount(); n != 0 {
		t.Errorf("failed launch: %d sends, want 0", n)
	}
	posts := m.posts()
	if len(posts) != 1 || !strings.Contains(posts[0].Text, "failed to start (image_pull_failed)") ||
		!strings.Contains(posts[0].Text, "1 message was not delivered") {
		t.Errorf("want one failure post naming the undelivered message, got %+v", posts)
	}
}

func respondEvent(agent string) *ChatEvent {
	e := threadEvent()
	e.Type = EventDialogSubmit
	e.ActionID = "agent.respond." + agent
	e.DialogData = map[string]string{e.ActionID: "yes please"}
	return e
}

func TestRespondRelay_WaitsForLaunch(t *testing.T) {
	rec := &recorder{}
	hub := newAsyncHub(rec, nil, launchingAgent("a1"), launchingAgent("a1"), runningAgent("a1"))
	router, m := newAsyncRouter(t, hub.client(), rec)
	if _, err := router.handleDialogSubmit(context.Background(), respondEvent("a1")); err != nil {
		t.Fatal(err)
	}
	waitWatchers(t, router)
	running := rec.index("get running")
	send := rec.index("send a1|yes please")
	if running < 0 || send < 0 || send < running {
		t.Fatalf("response not sent after the agent read as running: %v", rec.snapshot())
	}
	posts := m.posts()
	if len(posts) != 2 || posts[0].Text != launchStartingNotice("a1") || !strings.Contains(posts[1].Text, "Response sent") {
		t.Errorf("want the starting notice then 'Response sent', got %+v", posts)
	}
}

func TestRespondRelay_GetErrorStillSends(t *testing.T) {
	rec := &recorder{}
	hub := newAsyncHub(rec, nil)
	hub.getErr = errors.New("hub unavailable")
	router, m := newAsyncRouter(t, hub.client(), rec)
	if _, err := router.handleDialogSubmit(context.Background(), respondEvent("a1")); err != nil {
		t.Fatal(err)
	}
	waitWatchers(t, router)
	if n := hub.sendCount(); n != 1 {
		t.Errorf("read error: %d sends, want 1 (sent as before)", n)
	}
	posts := m.posts()
	if len(posts) != 1 || !strings.Contains(posts[0].Text, "Response sent") {
		t.Errorf("want 'Response sent', got %+v", posts)
	}
}

// TestRelay_OrderedBehindThreadInstruction checks that a message relayed to
// an agent whose /thread instruction is still waiting is sent after it, even
// when the relay's own read already shows the agent running.
func TestRelay_OrderedBehindThreadInstruction(t *testing.T) {
	rec := &recorder{}
	hub := newAsyncHub(rec, launchingAgent("a1"), runningAgent("a1"))
	hub.holdGet(0) // the watcher's first read blocks
	router, _ := newAsyncRouter(t, hub.client(), rec)
	if _, err := router.cmdThread(context.Background(), threadEvent(), []string{"a1", "first"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-hub.holdEnter:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not read the agent")
	}
	resp, err := router.cmdMessage(context.Background(), threadEvent(), []string{"a1", "second"})
	if err != nil {
		t.Fatal(err)
	}
	if want := launchStartingNotice("a1"); resp.Message.Text != want {
		t.Errorf("relay reply = %q, want the starting notice (queued behind the instruction)", resp.Message.Text)
	}
	close(hub.holdLeave)
	waitWatchers(t, router)
	first, second := rec.index("send a1|first"), rec.index("send a1|second")
	if first < 0 || second < 0 || second < first {
		t.Errorf("want the instruction sent before the relayed message: %v", rec.snapshot())
	}
}

// TestRelay_QueueBoundedAndFailurePostedOnce checks that the per-agent
// queue is capped, that a failed launch drains it without sending and posts
// the failure once, and that the queue entry is removed.
func TestRelay_QueueBoundedAndFailurePostedOnce(t *testing.T) {
	rec := &recorder{}
	hub := newAsyncHub(rec, nil, launchingAgent("a1"), failedAgent("a1"))
	hub.holdGet(1) // the watcher's first read blocks until all relays are queued
	router, m := newAsyncRouter(t, hub.client(), rec)
	for i := 0; i < maxDeferredPerAgent; i++ {
		// Every relay's own read shows the agent launching.
		hub.mu.Lock()
		hub.script = []*hubclient.Agent{launchingAgent("a1"), failedAgent("a1")}
		if i > 0 {
			hub.gets = 0
			hub.holdAt = -1
		}
		hub.mu.Unlock()
		resp, err := router.cmdMessage(context.Background(), threadEvent(), []string{"a1", fmt.Sprintf("m%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if resp.Message.Text != launchStartingNotice("a1") {
			t.Fatalf("relay %d reply = %q, want the starting notice", i, resp.Message.Text)
		}
		if i == 0 {
			<-hub.holdEnter
		}
	}
	hub.mu.Lock()
	hub.gets = 0
	hub.mu.Unlock()
	resp, err := router.cmdMessage(context.Background(), threadEvent(), []string{"a1", "overflow"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Text != launchQueueFullText("a1") {
		t.Errorf("relay past the cap: reply = %q, want the queue-full text", resp.Message.Text)
	}
	hub.mu.Lock()
	hub.script = []*hubclient.Agent{failedAgent("a1")}
	hub.mu.Unlock()
	close(hub.holdLeave)
	waitWatchers(t, router)
	if n := hub.sendCount(); n != 0 {
		t.Errorf("failed launch: %d sends, want 0", n)
	}
	posts := m.posts()
	if len(posts) != 1 {
		t.Fatalf("got %d failure posts, want 1: %+v", len(posts), posts)
	}
	if want := fmt.Sprintf("%d messages were not delivered", maxDeferredPerAgent); !strings.Contains(posts[0].Text, want) {
		t.Errorf("failure post = %q, want it to say %q", posts[0].Text, want)
	}
	router.launchMu.Lock()
	n := len(router.launchQueues)
	router.launchMu.Unlock()
	if n != 0 {
		t.Errorf("%d launch queues left after the launch ended, want 0", n)
	}
}

// TestShutdown_StopsWatchersWithoutPosting checks that Shutdown ends a
// watcher whose launch is still in flight and that nothing is posted or
// sent after it.
func TestShutdown_StopsWatchersWithoutPosting(t *testing.T) {
	rec := &recorder{}
	hub := newAsyncHub(rec, launchingAgent("a1"), launchingAgent("a1"))
	router, m := newAsyncRouter(t, hub.client(), rec)
	if _, err := router.cmdCreate(context.Background(), threadEvent(), []string{"a1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := router.cmdMessage(context.Background(), threadEvent(), []string{"a1", "hello"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := router.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v (watchers did not stop)", err)
	}
	// The launch now succeeds; a watcher still running would post and send.
	hub.setScript(runningAgent("a1"))
	time.Sleep(50 * time.Millisecond)
	if p := m.posts(); len(p) != 0 {
		t.Errorf("posts after Shutdown: %+v", p)
	}
	if n := hub.sendCount(); n != 0 {
		t.Errorf("%d sends after Shutdown, want 0", n)
	}
}

// TestCmdThread_RelayDuringPostsQueuesBehindInstruction checks that /thread
// registers its instruction before posting the kickoff and the card: a
// message relayed while those posts are in flight, whose own read already
// shows the agent running, is sent after the instruction.
func TestCmdThread_RelayDuringPostsQueuesBehindInstruction(t *testing.T) {
	rec := &recorder{}
	hub := newAsyncHub(rec, launchingAgent("a1"), runningAgent("a1"))
	router, m := newAsyncRouter(t, hub.client(), rec)
	m.holdEnter = make(chan struct{})
	m.holdLeave = make(chan struct{})
	threadDone := make(chan error, 1)
	go func() {
		_, err := router.cmdThread(context.Background(), threadEvent(), []string{"a1", "first"})
		threadDone <- err
	}()
	select {
	case <-m.holdEnter: // the kickoff post is in flight
	case <-time.After(5 * time.Second):
		t.Fatal("kickoff not posted")
	}
	resp, err := router.cmdMessage(context.Background(), threadEvent(), []string{"a1", "second"})
	if err != nil {
		t.Fatal(err)
	}
	close(m.holdLeave)
	if err := <-threadDone; err != nil {
		t.Fatal(err)
	}
	waitWatchers(t, router)
	first, second := rec.index("send a1|first"), rec.index("send a1|second")
	if first < 0 || second < 0 || second < first {
		t.Errorf("a relay during the /thread posts overtook the instruction (relay reply %q): %v", resp.Message.Text, rec.snapshot())
	}
}

// TestCmdCreate_RecreatedAgentGetsOwnQueue checks that queues are keyed by
// agent ID: an agent re-created with the same name while the old one's
// launch is still followed gets its own watcher and outcome.
func TestCmdCreate_RecreatedAgentGetsOwnQueue(t *testing.T) {
	rec := &recorder{}
	oldA, newA := launchingAgent("a1"), launchingAgent("a1")
	oldA.ID, newA.ID = "id-old", "id-new"
	oldFailed := failedAgent("a1")
	oldFailed.ID = "id-old"
	newRunning := runningAgent("a1")
	newRunning.ID = "id-new"
	release := make(chan struct{})
	hub := newAsyncHub(rec, oldA)
	c := hub.client()
	c.agents.getFunc = func(ctx context.Context, id string) (*hubclient.Agent, error) {
		if id == "id-old" {
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			cp := *oldFailed
			return &cp, nil
		}
		cp := *newRunning
		return &cp, nil
	}
	router, m := newAsyncRouter(t, c, rec)
	if _, err := router.cmdCreate(context.Background(), threadEvent(), []string{"a1"}); err != nil {
		t.Fatal(err)
	}
	hub.mu.Lock()
	hub.createResp = newA
	hub.mu.Unlock()
	if _, err := router.cmdCreate(context.Background(), threadEvent(), []string{"a1"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(fmt.Sprint(rec.snapshot()), "is running") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	waitWatchers(t, router)
	var running, failed int
	for _, p := range m.posts() {
		if strings.Contains(p.Text, "is running (ID: `id-new`)") {
			running++
		}
		if strings.Contains(p.Text, "failed to start") {
			failed++
		}
	}
	if running != 1 || failed != 1 {
		t.Errorf("want one running post for the new agent and one failure for the old, got running=%d failed=%d: %+v", running, failed, m.posts())
	}
}

// TestCmdThread_AfterShutdown_NoStaleCard checks that /thread after Shutdown
// starts no watcher and posts nothing (no "Starting…" card left behind).
func TestCmdThread_AfterShutdown_NoStaleCard(t *testing.T) {
	rec := &recorder{}
	hub := newAsyncHub(rec, launchingAgent("a1"), runningAgent("a1"))
	router, m := newAsyncRouter(t, hub.client(), rec)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := router.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	resp, err := router.cmdThread(context.Background(), threadEvent(), []string{"a1", "do", "X"})
	if err != nil {
		t.Fatal(err)
	}
	waitWatchers(t, router)
	if resp == nil || resp.Message == nil || !strings.Contains(resp.Message.Text, "not following its launch") {
		t.Errorf("want the not-following reply, got %+v", resp)
	}
	if p := m.posts(); len(p) != 0 {
		t.Errorf("after Shutdown: %d posts (kickoff or Starting… card), want 0: %v", len(p), rec.snapshot())
	}
	if n := hub.sendCount(); n != 0 {
		t.Errorf("after Shutdown: %d sends, want 0", n)
	}
}

// TestCmdCreate_EmptyResponse checks that a create answer without an agent
// is reported, not dereferenced.
func TestCmdCreate_EmptyResponse(t *testing.T) {
	rec := &recorder{}
	hub := newAsyncHub(rec, nil)
	router, m := newAsyncRouter(t, hub.client(), rec)
	var resp *EventResponse
	var err error
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("empty create answer: cmdCreate panicked: %v", p)
			}
		}()
		resp, err = router.cmdCreate(context.Background(), threadEvent(), []string{"a1"})
	}()
	if err != nil {
		t.Fatal(err)
	}
	if want := "Failed to create agent: received empty response from server"; resp == nil || resp.Message == nil || resp.Message.Text != want {
		t.Errorf("empty create answer: reply = %+v, want %q", resp, want)
	}
	waitWatchers(t, router)
	if p := m.posts(); len(p) != 0 {
		t.Errorf("empty create answer: %d posts, want 0", len(p))
	}
}
