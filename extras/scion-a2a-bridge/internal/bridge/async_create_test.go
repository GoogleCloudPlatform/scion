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
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// The bridge opts its auto-provision create in to an asynchronous launch
// (bridge.async_create, default true). On an asynchronous answer, and for an
// adopted agent that is still launching, every path waits for the agent to
// run before the first send: the task reports working with the launch step,
// and fails without a send when the launch fails. A follow-up to a task whose
// agent is still launching waits the same way (ptone/scion#4671).

const launchTestAgentID = "agent-launch-1"

const launchFailureWant = "Agent failed to start (image_pull_failed): image pull back-off"

func launchingAgent(step string) *hubclient.Agent {
	return &hubclient.Agent{
		ID: launchTestAgentID, Name: resolveTestSlug, Slug: resolveTestSlug, ProjectID: "proj-1",
		Phase:  "provisioning",
		Launch: &hubclient.AgentLaunch{ID: "launch-1", State: "active", Active: true, Kind: "create", Step: step},
	}
}

func runningAgent() *hubclient.Agent {
	return &hubclient.Agent{
		ID: launchTestAgentID, Name: resolveTestSlug, Slug: resolveTestSlug, ProjectID: "proj-1",
		Phase:  "running",
		Launch: &hubclient.AgentLaunch{ID: "launch-1", State: "ended", Kind: "create"},
	}
}

// failedLaunchAgent is an agent whose launch ended with an error code. It
// does not use the not_launched end reason.
func failedLaunchAgent() *hubclient.Agent {
	return &hubclient.Agent{
		ID: launchTestAgentID, Name: resolveTestSlug, Slug: resolveTestSlug, ProjectID: "proj-1",
		Phase:   "error",
		Message: "image pull back-off",
		Launch:  &hubclient.AgentLaunch{ID: "launch-1", State: "ended", Kind: "create", Error: "image_pull_failed", EndReason: "failed"},
	}
}

func syncCreatedAgent() *hubclient.Agent {
	return &hubclient.Agent{ID: launchTestAgentID, Name: resolveTestSlug, Slug: resolveTestSlug, ProjectID: "proj-1", Phase: "running"}
}

// launchHub scripts the hub side of a launch: Create answers created, Get
// answers the script in order (repeating the last entry), and each
// non-interrupt send records how many Gets had been answered before it.
type launchHub struct {
	created *hubclient.Agent
	script  []*hubclient.Agent
	getErr  error
	// hold, when set, makes Get answer the first script entry until it is
	// closed, and the hub refuse sends until then.
	hold chan struct{}

	gets       atomic.Int32
	creates    atomic.Int32
	mu         sync.Mutex
	createReqs []*hubclient.CreateAgentRequest
	sendGets   []int32
	sent       chan struct{}
	onSend     func(msg *messages.StructuredMessage)
	// sendDelay, when set, delays the completion of a send; completed
	// records the message texts in the order their sends completed.
	sendDelay func(msg *messages.StructuredMessage) time.Duration
	completed []string
	// refused counts sends refused while held.
	refused int
}

func newLaunchHub(created *hubclient.Agent, script ...*hubclient.Agent) *launchHub {
	return &launchHub{created: created, script: script, sent: make(chan struct{}, 16)}
}

func (h *launchHub) service() *mockAgentService {
	return &mockAgentService{
		createFn: func(_ context.Context, req *hubclient.CreateAgentRequest) (*hubclient.CreateAgentResponse, error) {
			h.creates.Add(1)
			h.mu.Lock()
			h.createReqs = append(h.createReqs, req)
			h.mu.Unlock()
			if h.created == nil {
				return nil, errors.New("create refused")
			}
			a := *h.created
			return &hubclient.CreateAgentResponse{Agent: &a}, nil
		},
		getFn: func(_ context.Context, _ string) (*hubclient.Agent, error) {
			if h.getErr != nil {
				h.gets.Add(1)
				return nil, h.getErr
			}
			n := int(h.gets.Add(1))
			idx := min(n, len(h.script)) - 1
			if h.hold != nil {
				// Held: the first entry until hold is closed, then the last.
				select {
				case <-h.hold:
					idx = len(h.script) - 1
				default:
					idx = 0
				}
			}
			a := *h.script[idx]
			return &a, nil
		},
		sendFn: func(_ context.Context, _ string, msg *messages.StructuredMessage, interrupt, _, _ bool) (*hubclient.MessageResponse, error) {
			if interrupt {
				return nil, nil
			}
			if h.hold != nil {
				// While held the agent is launching: the hub refuses
				// the message (409 agent_not_running).
				select {
				case <-h.hold:
				default:
					h.mu.Lock()
					h.refused++
					h.mu.Unlock()
					return nil, errors.New("agent_not_running: agent is still launching")
				}
			}
			h.mu.Lock()
			h.sendGets = append(h.sendGets, h.gets.Load())
			h.mu.Unlock()
			if h.onSend != nil {
				h.onSend(msg)
			}
			if h.sendDelay != nil {
				time.Sleep(h.sendDelay(msg))
			}
			h.mu.Lock()
			h.completed = append(h.completed, msg.Msg)
			h.mu.Unlock()
			h.sent <- struct{}{}
			return nil, nil
		},
	}
}

func (h *launchHub) sends() []int32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int32(nil), h.sendGets...)
}

func (h *launchHub) waitSend(t *testing.T) {
	t.Helper()
	select {
	case <-h.sent:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the send")
	}
}

// newLaunchBridge is an auto-provisioning bridge with a short launch poll.
func newLaunchBridge(t *testing.T, agents *mockAgentService, asyncCreate *bool) *Bridge {
	t.Helper()
	b := newAutoProvisionBridge(t, agents)
	b.config.Bridge.AsyncCreate = asyncCreate
	b.launchPollInterval = 5 * time.Millisecond
	return b
}

func onlyTask(t *testing.T, b *Bridge) *state.Task {
	t.Helper()
	tasks, err := b.store.ListTasksByAgent(context.Background(), "proj-1", resolveTestSlug)
	if err != nil {
		t.Fatalf("ListTasksByAgent: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	return &tasks[0]
}

func waitTaskState(t *testing.T, b *Bridge, taskID, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		task, err := b.store.GetTask(context.Background(), taskID)
		if err == nil && task != nil && task.State == want {
			return
		}
		if time.Now().After(deadline) {
			got := "<none>"
			if task != nil {
				got = task.State
			}
			t.Fatalf("task %s state = %s, want %s", taskID, got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func taskRegistered(b *Bridge, taskID string) bool {
	b.tasksMu.RLock()
	defer b.tasksMu.RUnlock()
	_, ok := b.activeTasks[taskID]
	return ok
}

// --- opt-in and configuration ---

func TestResolveContext_AsyncCreateOptIn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setting *bool
		want    bool
	}{
		{"unset", nil, true},
		{"false", boolPtr(false), false},
		{"true", boolPtr(true), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := newLaunchHub(syncCreatedAgent())
			b := newLaunchBridge(t, hub.service(), tc.setting)
			if _, err := b.resolveContext(context.Background(), "proj-1", resolveTestSlug, ""); err != nil {
				t.Fatalf("resolveContext: %v", err)
			}
			if len(hub.createReqs) != 1 {
				t.Fatalf("creates = %d, want 1", len(hub.createReqs))
			}
			if got := hub.createReqs[0].AcceptAsyncLaunch; got != tc.want {
				t.Errorf("AcceptAsyncLaunch = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestConfig_AsyncCreateDefault(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want bool
	}{
		{"absent", "bridge:\n  listen_address: \":8443\"\n", true},
		{"false", "bridge:\n  async_create: false\n", false},
		{"true", "bridge:\n  async_create: true\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if err := yaml.Unmarshal([]byte(tc.yaml), &cfg); err != nil {
				t.Fatalf("yaml: %v", err)
			}
			if got := cfg.asyncCreateEnabled(); got != tc.want {
				t.Errorf("asyncCreateEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- SDK executor ---

type execEvent struct {
	kind  string // "task" or the status state
	text  string
	sends int
}

// runExecute runs the executor for one new task and records each event,
// with the number of sends seen when it was yielded. It stops after the
// working event that follows a send.
func runExecute(t *testing.T, b *Bridge, hub *launchHub) []execEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = WithRouteInfo(ctx, RouteInfo{ProjectSlug: "proj-1", AgentSlug: resolveTestSlug})
	execCtx := &a2asrv.ExecutorContext{
		TaskID:    "task-exec-1",
		ContextID: "ctx-exec-1",
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello")),
	}
	var events []execEvent
	for ev, err := range NewScionExecutor(b, b.log).Execute(ctx, execCtx) {
		if err != nil {
			t.Fatalf("Execute yielded error: %v", err)
		}
		sends := len(hub.sends())
		switch e := ev.(type) {
		case *a2a.Task:
			events = append(events, execEvent{kind: "task", sends: sends})
		case *a2a.TaskStatusUpdateEvent:
			text := ""
			if e.Status.Message != nil && len(e.Status.Message.Parts) > 0 {
				text = e.Status.Message.Parts[0].Text()
			}
			events = append(events, execEvent{kind: string(e.Status.State), text: text, sends: sends})
			if e.Status.State == a2a.TaskStateWorking && sends > 0 {
				return events
			}
		default:
			t.Fatalf("unexpected event %T", ev)
		}
	}
	return events
}

func TestExecute_AsyncCreate_WaitsThenSends(t *testing.T) {
	hub := newLaunchHub(launchingAgent("pod_create"), launchingAgent("container_create"), runningAgent())
	b := newLaunchBridge(t, hub.service(), nil)

	events := runExecute(t, b, hub)

	working := string(a2a.TaskStateWorking)
	want := []execEvent{
		{kind: "task"},
		{kind: working, text: "provisioning agent (pod_create)"},
		{kind: working, text: "provisioning agent (container_create)"},
		{kind: working, sends: 1},
	}
	if len(events) != len(want) {
		t.Fatalf("events = %+v, want %+v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, events[i], want[i])
		}
	}
	if s := hub.sends(); len(s) != 1 || s[0] != 2 {
		t.Errorf("sends (gets answered before each) = %v, want [2]: one send, after the read that showed running", s)
	}
}

func TestExecute_AsyncCreate_FailedNoSend(t *testing.T) {
	hub := newLaunchHub(launchingAgent("pod_create"), failedLaunchAgent())
	b := newLaunchBridge(t, hub.service(), nil)

	events := runExecute(t, b, hub)

	if n := len(hub.sends()); n != 0 {
		t.Fatalf("sends = %d, want 0 after a failed launch", n)
	}
	if len(events) == 0 {
		t.Fatal("no events")
	}
	last := events[len(events)-1]
	if last.kind != string(a2a.TaskStateFailed) || last.text != launchFailureWant {
		t.Errorf("last event = %+v, want failed %q", last, launchFailureWant)
	}
	if len(events) != 3 || events[1].text != "provisioning agent (pod_create)" {
		t.Errorf("events = %+v, want task, working provisioning, failed", events)
	}
}

func TestExecute_SyncCreate_NoWait(t *testing.T) {
	hub := newLaunchHub(syncCreatedAgent(), runningAgent())
	b := newLaunchBridge(t, hub.service(), nil)

	events := runExecute(t, b, hub)

	want := []execEvent{{kind: "task"}, {kind: string(a2a.TaskStateWorking), sends: 1}}
	if len(events) != len(want) || events[0] != want[0] || events[1] != want[1] {
		t.Errorf("events = %+v, want %+v (today's sequence)", events, want)
	}
	if n := hub.gets.Load(); n != 0 {
		t.Errorf("agent reads = %d, want 0 for a synchronous create", n)
	}
}

// --- legacy SendMessage ---

func TestSendMessage_Legacy_AsyncCreateFailed_Blocking(t *testing.T) {
	hub := newLaunchHub(launchingAgent("pod_create"), failedLaunchAgent())
	b := newLaunchBridge(t, hub.service(), nil)

	_, err := b.SendMessage(context.Background(), "proj-1", resolveTestSlug, "", "", testParts, true)
	if err == nil || !strings.Contains(err.Error(), launchFailureWant) {
		t.Fatalf("SendMessage error = %v, want it to contain %q", err, launchFailureWant)
	}
	if n := len(hub.sends()); n != 0 {
		t.Errorf("sends = %d, want 0", n)
	}
	if task := onlyTask(t, b); task.State != TaskStateFailed {
		t.Errorf("task state = %s, want failed", task.State)
	}
}

func TestSendMessage_Legacy_AsyncCreate_NonBlockingSendsAfterRunning(t *testing.T) {
	hub := newLaunchHub(launchingAgent("pod_create"), launchingAgent("container_create"), runningAgent())
	b := newLaunchBridge(t, hub.service(), nil)

	res, err := b.SendMessage(context.Background(), "proj-1", resolveTestSlug, "", "", testParts, false)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if res.Status.State != TaskStateSubmitted {
		t.Errorf("state = %s, want submitted", res.Status.State)
	}
	hub.waitSend(t)
	if s := hub.sends(); len(s) != 1 || s[0] != 2 {
		t.Errorf("sends (gets answered before each) = %v, want [2]", s)
	}
	waitTaskState(t, b, res.ID, TaskStateWorking)
}

func TestSendMessage_Legacy_LaunchWaitKeepsTaskAlive(t *testing.T) {
	hub := newLaunchHub(launchingAgent("pod_create"), launchingAgent("pod_create"), runningAgent())
	hub.hold = make(chan struct{})
	b := newLaunchBridge(t, hub.service(), nil)
	b.launchKeepAlive = 20 * time.Millisecond

	res, err := b.SendMessage(context.Background(), "proj-1", resolveTestSlug, "", "", testParts, false)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	// Wait longer than the reap cutoff, then reap: a waiting task that is
	// kept alive is not stale.
	time.Sleep(400 * time.Millisecond)
	b.reapStaleTasks(context.Background(), 200*time.Millisecond)
	task, err := b.store.GetTask(context.Background(), res.ID)
	if err != nil || task == nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != TaskStateWorking {
		t.Fatalf("task state during the launch wait = %s, want working (not reaped)", task.State)
	}
	close(hub.hold)
	hub.waitSend(t)
}

func TestSendMessage_Legacy_TaskEndedDuringWait_NoSend(t *testing.T) {
	hub := newLaunchHub(launchingAgent("pod_create"), launchingAgent("pod_create"), runningAgent())
	hub.hold = make(chan struct{})
	b := newLaunchBridge(t, hub.service(), nil)

	res, err := b.SendMessage(context.Background(), "proj-1", resolveTestSlug, "", "", testParts, false)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	waitTaskState(t, b, res.ID, TaskStateWorking)
	if _, err := b.store.UpdateTaskState(context.Background(), res.ID, TaskStateCanceled); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	close(hub.hold)
	// The send goroutine ends either by sending or by unregistering the
	// task without a send.
	deadline := time.Now().Add(5 * time.Second)
	for len(hub.sends()) == 0 && taskRegistered(b, res.ID) {
		if time.Now().After(deadline) {
			t.Fatal("send goroutine did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(hub.sends()); n != 0 {
		t.Errorf("sends = %d, want 0 for a task cancelled during the launch wait", n)
	}
}

// --- streaming ---

func readStreamUntil(t *testing.T, events <-chan StreamEvent, stop func(TaskStatusUpdate) bool) []TaskStatusUpdate {
	t.Helper()
	var got []TaskStatusUpdate
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("stream closed; got %+v", got)
			}
			if ev.StatusUpdate == nil {
				continue
			}
			got = append(got, *ev.StatusUpdate)
			if stop(*ev.StatusUpdate) {
				return got
			}
		case <-timeout:
			t.Fatalf("timed out reading the stream; got %+v", got)
		}
	}
}

func statusText(su TaskStatusUpdate) string {
	if su.Status.Message == nil || len(su.Status.Message.Parts) == 0 {
		return ""
	}
	return su.Status.Message.Parts[0].Text
}

func hasStatus(got []TaskStatusUpdate, state, text string) bool {
	for _, su := range got {
		if su.Status.State == state && statusText(su) == text {
			return true
		}
	}
	return false
}

func TestSendStreamingMessage_AsyncCreateFailed(t *testing.T) {
	hub := newLaunchHub(launchingAgent("pod_create"), failedLaunchAgent())
	b := newLaunchBridge(t, hub.service(), nil)

	_, events, cleanup, err := b.SendStreamingMessage(context.Background(), "proj-1", resolveTestSlug, "", testParts)
	if err != nil {
		t.Fatalf("SendStreamingMessage: %v", err)
	}
	defer cleanup()
	got := readStreamUntil(t, events, func(su TaskStatusUpdate) bool { return su.Final })

	if !hasStatus(got, TaskStateWorking, "provisioning agent (pod_create)") {
		t.Errorf("stream = %+v, want a working provisioning event", got)
	}
	last := got[len(got)-1]
	if last.Status.State != TaskStateFailed || statusText(last) != launchFailureWant {
		t.Errorf("final event = %+v, want failed %q", last, launchFailureWant)
	}
	if n := len(hub.sends()); n != 0 {
		t.Errorf("sends = %d, want 0", n)
	}
}

func TestSendStreamingMessage_AsyncCreateWaitsThenSends(t *testing.T) {
	hub := newLaunchHub(launchingAgent("pod_create"), launchingAgent("container_create"), runningAgent())
	b := newLaunchBridge(t, hub.service(), nil)

	_, events, cleanup, err := b.SendStreamingMessage(context.Background(), "proj-1", resolveTestSlug, "", testParts)
	if err != nil {
		t.Fatalf("SendStreamingMessage: %v", err)
	}
	defer cleanup()
	got := readStreamUntil(t, events, func(su TaskStatusUpdate) bool {
		return su.Status.State == TaskStateWorking && statusText(su) == ""
	})
	for _, text := range []string{"provisioning agent (pod_create)", "provisioning agent (container_create)"} {
		if !hasStatus(got, TaskStateWorking, text) {
			t.Errorf("stream = %+v, want working %q", got, text)
		}
	}
	if s := hub.sends(); len(s) != 1 || s[0] != 2 {
		t.Errorf("sends (gets answered before each) = %v, want [2]", s)
	}
}

// --- adopting a launching agent ---

func TestResolveContext_AdoptLaunchingAgentWaits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setting *bool
		relist  bool
	}{
		{"listed/async_create unset", nil, false},
		{"listed/async_create false", boolPtr(false), false},
		{"relisted after failed create/async_create unset", nil, true},
		{"relisted after failed create/async_create false", boolPtr(false), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := newLaunchHub(nil, runningAgent())
			agents := hub.service()
			listed := []hubclient.Agent{*launchingAgent("pod_create")}
			if tc.relist {
				agents.listFn, _ = listing(nil, listed)
			} else {
				agents.listFn, _ = listing(listed, listed)
			}
			b := newLaunchBridge(t, agents, tc.setting)

			if _, err := b.SendMessage(context.Background(), "proj-1", resolveTestSlug, "", "", testParts, false); err != nil {
				t.Fatalf("SendMessage: %v", err)
			}
			hub.waitSend(t)
			if s := hub.sends(); len(s) != 1 || s[0] != 1 {
				t.Errorf("sends (gets answered before each) = %v, want [1]: after the read that showed running", s)
			}
			if wantCreates := map[bool]int32{false: 0, true: 1}[tc.relist]; hub.creates.Load() != wantCreates {
				t.Errorf("creates = %d, want %d", hub.creates.Load(), wantCreates)
			}
		})
	}
}

// --- follow-ups ---

func newLaunchFollowUpBridge(t *testing.T, hub *launchHub) (*Bridge, state.Store) {
	t.Helper()
	b, store := newFollowUpTestBridge(t, hub.service())
	b.launchPollInterval = 5 * time.Millisecond
	seedTask(t, store, "task-1", "ctx-1", "proj-1", "agent-a", launchTestAgentID, TaskStateWorking)
	return b, store
}

func TestSendFollowUp_WaitsForLaunchingAgent(t *testing.T) {
	t.Run("blocking", func(t *testing.T) {
		hub := newLaunchHub(nil, launchingAgent("pod_create"), launchingAgent("container_create"), runningAgent())
		b, store := newLaunchFollowUpBridge(t, hub)
		hub.onSend = func(*messages.StructuredMessage) { injectResponseEvent(t, store, "task-1", "done") }

		if _, err := b.SendMessage(context.Background(), "proj-1", "agent-a", "", "task-1", testParts, true); err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
		if s := hub.sends(); len(s) != 1 || s[0] != 3 {
			t.Errorf("sends (gets answered before each) = %v, want [3]: after the read that showed running", s)
		}
	})
	t.Run("non-blocking", func(t *testing.T) {
		hub := newLaunchHub(nil, launchingAgent("pod_create"), launchingAgent("container_create"), runningAgent())
		b, _ := newLaunchFollowUpBridge(t, hub)

		if _, err := b.SendMessage(context.Background(), "proj-1", "agent-a", "", "task-1", testParts, false); err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
		hub.waitSend(t)
		if s := hub.sends(); len(s) != 1 || s[0] != 3 {
			t.Errorf("sends (gets answered before each) = %v, want [3]", s)
		}
	})
}

func TestSendFollowUp_LaunchFailed_NoSend(t *testing.T) {
	hub := newLaunchHub(nil, launchingAgent("pod_create"), failedLaunchAgent())
	b, store := newLaunchFollowUpBridge(t, hub)

	_, err := b.SendMessage(context.Background(), "proj-1", "agent-a", "", "task-1", testParts, true)
	if err == nil || !strings.Contains(err.Error(), launchFailureWant) {
		t.Fatalf("SendMessage error = %v, want it to contain %q", err, launchFailureWant)
	}
	if n := len(hub.sends()); n != 0 {
		t.Errorf("sends = %d, want 0", n)
	}
	task, err := store.GetTask(context.Background(), "task-1")
	if err != nil || task == nil || task.State != TaskStateFailed {
		t.Errorf("task = %+v (%v), want failed", task, err)
	}
}

func TestSendFollowUp_GetErrorStillSends(t *testing.T) {
	hub := newLaunchHub(nil, runningAgent())
	hub.getErr = errors.New("hub unavailable")
	b, store := newLaunchFollowUpBridge(t, hub)
	hub.onSend = func(*messages.StructuredMessage) { injectResponseEvent(t, store, "task-1", "done") }

	if _, err := b.SendMessage(context.Background(), "proj-1", "agent-a", "", "task-1", testParts, true); err != nil {
		t.Fatalf("SendMessage: %v (a failed read before a follow-up must not fail it)", err)
	}
	if n := len(hub.sends()); n != 1 {
		t.Errorf("sends = %d, want 1", n)
	}
}

// A follow-up sent while the task's first message still waits for the
// launching agent is delivered after that message, not before it.
func TestSendFollowUp_WaitsForPendingFirstSend(t *testing.T) {
	for _, blocking := range []bool{true, false} {
		name := map[bool]string{true: "blocking follow-up", false: "non-blocking follow-up"}[blocking]
		t.Run(name, func(t *testing.T) {
			hub := newLaunchHub(launchingAgent("pod_create"), launchingAgent("pod_create"), runningAgent())
			hub.hold = make(chan struct{})
			// The first send takes a while to complete: a follow-up that
			// does not wait for it completes first.
			hub.sendDelay = func(msg *messages.StructuredMessage) time.Duration {
				if msg.Msg == "first" {
					return 150 * time.Millisecond
				}
				return 0
			}
			var b *Bridge
			var firstTask atomic.Value
			hub.onSend = func(msg *messages.StructuredMessage) {
				if msg.Msg == "second" {
					injectResponseEvent(t, b.store, firstTask.Load().(string), "done")
				}
			}
			b = newLaunchBridge(t, hub.service(), nil)
			res, err := b.SendMessage(context.Background(), "proj-1", resolveTestSlug, "", "", []Part{{Text: "first"}}, false)
			if err != nil {
				t.Fatalf("SendMessage: %v", err)
			}
			firstTask.Store(res.ID)
			waitTaskState(t, b, res.ID, TaskStateWorking)

			followUpErr := make(chan error, 1)
			go func() {
				_, err := b.SendMessage(context.Background(), "proj-1", resolveTestSlug, "", res.ID, []Part{{Text: "second"}}, blocking)
				followUpErr <- err
			}()
			time.Sleep(50 * time.Millisecond) // the follow-up is waiting too
			close(hub.hold)
			hub.waitSend(t)
			hub.waitSend(t)
			if err := <-followUpErr; err != nil {
				t.Fatalf("follow-up: %v", err)
			}
			hub.mu.Lock()
			order := append([]string(nil), hub.completed...)
			hub.mu.Unlock()
			if len(order) != 2 || order[0] != "first" || order[1] != "second" {
				t.Errorf("send completion order = %v, want [first second]: the follow-up must not overtake the first message", order)
			}
		})
	}
}

// A follow-up waiting for the task's first send is not sent when that send
// fails because the launch failed.
func TestSendFollowUp_FirstSendFailed_NoSend(t *testing.T) {
	hub := newLaunchHub(launchingAgent("pod_create"), launchingAgent("pod_create"), failedLaunchAgent())
	hub.hold = make(chan struct{})
	b := newLaunchBridge(t, hub.service(), nil)
	res, err := b.SendMessage(context.Background(), "proj-1", resolveTestSlug, "", "", []Part{{Text: "first"}}, false)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	waitTaskState(t, b, res.ID, TaskStateWorking)
	followUpErr := make(chan error, 1)
	go func() {
		_, err := b.SendMessage(context.Background(), "proj-1", resolveTestSlug, "", res.ID, []Part{{Text: "second"}}, true)
		followUpErr <- err
	}()
	time.Sleep(50 * time.Millisecond)
	close(hub.hold)
	select {
	case err = <-followUpErr:
	case <-time.After(5 * time.Second):
		t.Fatal("follow-up did not return")
	}
	if err == nil || !errors.Is(err, errTaskEndedBeforeFollowUp) {
		t.Errorf("follow-up error = %v, want errTaskEndedBeforeFollowUp", err)
	}
	if n := len(hub.sends()); n != 0 {
		t.Errorf("sends = %d, want 0: the follow-up must not be sent after the first send failed", n)
	}
}
