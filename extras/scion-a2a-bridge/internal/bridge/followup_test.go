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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// --- Mock hubclient ---

// mockAgentService implements hubclient.AgentService for testing.
type mockAgentService struct {
	sendFn func(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt, notify, wake bool) (*hubclient.MessageResponse, error)
	listFn func(ctx context.Context, opts *hubclient.ListAgentsOptions) (*hubclient.ListAgentsResponse, error)
}

func (m *mockAgentService) SendStructuredMessage(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt, notify, wake bool) (*hubclient.MessageResponse, error) {
	if m.sendFn != nil {
		return m.sendFn(ctx, agentID, msg, interrupt, notify, wake)
	}
	return nil, nil
}

func (m *mockAgentService) List(ctx context.Context, opts *hubclient.ListAgentsOptions) (*hubclient.ListAgentsResponse, error) {
	if m.listFn != nil {
		return m.listFn(ctx, opts)
	}
	return &hubclient.ListAgentsResponse{}, nil
}

func (m *mockAgentService) Get(ctx context.Context, agentID string) (*hubclient.Agent, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockAgentService) Create(ctx context.Context, req *hubclient.CreateAgentRequest) (*hubclient.CreateAgentResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockAgentService) Update(ctx context.Context, agentID string, req *hubclient.UpdateAgentRequest) (*hubclient.Agent, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockAgentService) ResetAuth(ctx context.Context, agentID string) error {
	return fmt.Errorf("not implemented")
}
func (m *mockAgentService) Delete(ctx context.Context, agentID string, opts *hubclient.DeleteAgentOptions) error {
	return fmt.Errorf("not implemented")
}
func (m *mockAgentService) Start(ctx context.Context, agentID string) error {
	return fmt.Errorf("not implemented")
}
func (m *mockAgentService) Stop(ctx context.Context, agentID string) error {
	return fmt.Errorf("not implemented")
}
func (m *mockAgentService) Suspend(ctx context.Context, agentID string) error {
	return fmt.Errorf("not implemented")
}
func (m *mockAgentService) Restart(ctx context.Context, agentID string) error {
	return fmt.Errorf("not implemented")
}
func (m *mockAgentService) StopAll(ctx context.Context) (*hubclient.StopAllResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockAgentService) SendMessage(ctx context.Context, agentID string, message string, interrupt bool) error {
	return fmt.Errorf("not implemented")
}
func (m *mockAgentService) BroadcastMessage(ctx context.Context, msg *messages.StructuredMessage, interrupt bool) (*hubclient.BroadcastResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockAgentService) SubmitEnv(ctx context.Context, agentID string, req *hubclient.SubmitEnvRequest) (*hubclient.CreateAgentResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockAgentService) Restore(ctx context.Context, agentID string) (*hubclient.Agent, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockAgentService) Exec(ctx context.Context, agentID string, command []string, timeout int) (*hubclient.ExecResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockAgentService) GetLogs(ctx context.Context, agentID string, opts *hubclient.GetLogsOptions) (string, error) {
	return "", fmt.Errorf("not implemented")
}
func (m *mockAgentService) SendOutboundMessage(ctx context.Context, agentID string, msg *hubclient.OutboundMessageRequest) error {
	return fmt.Errorf("not implemented")
}
func (m *mockAgentService) GetCloudLogs(ctx context.Context, agentID string, opts *hubclient.GetCloudLogsOptions) (*hubclient.CloudLogsResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockAgentService) StreamCloudLogs(ctx context.Context, agentID string, opts *hubclient.GetCloudLogsOptions, handler func(hubclient.CloudLogEntry)) error {
	return fmt.Errorf("not implemented")
}

// mockHubClient implements hubclient.Client for testing, delegating to a mockAgentService.
type mockHubClient struct {
	agents *mockAgentService
}

func (m *mockHubClient) Agents() hubclient.AgentService                               { return m.agents }
func (m *mockHubClient) ProjectAgents(string) hubclient.AgentService                  { return m.agents }
func (m *mockHubClient) Projects() hubclient.ProjectService                           { return nil }
func (m *mockHubClient) RuntimeBrokers() hubclient.RuntimeBrokerService               { return nil }
func (m *mockHubClient) Templates() hubclient.TemplateService                         { return nil }
func (m *mockHubClient) HarnessConfigs() hubclient.HarnessConfigService               { return nil }
func (m *mockHubClient) Workspace() hubclient.WorkspaceService                        { return nil }
func (m *mockHubClient) Users() hubclient.UserService                                 { return nil }
func (m *mockHubClient) Env() hubclient.EnvService                                    { return nil }
func (m *mockHubClient) Secrets() hubclient.SecretService                             { return nil }
func (m *mockHubClient) Auth() hubclient.AuthService                                  { return nil }
func (m *mockHubClient) Notifications() hubclient.NotificationService                 { return nil }
func (m *mockHubClient) Tokens() hubclient.TokenService                               { return nil }
func (m *mockHubClient) Subscriptions() hubclient.SubscriptionService                 { return nil }
func (m *mockHubClient) SubscriptionTemplates() hubclient.SubscriptionTemplateService { return nil }
func (m *mockHubClient) ScheduledEvents(string) hubclient.ScheduledEventService       { return nil }
func (m *mockHubClient) Schedules(string) hubclient.ScheduleService                   { return nil }
func (m *mockHubClient) GCPServiceAccounts(string) hubclient.GCPServiceAccountService { return nil }
func (m *mockHubClient) Messages() hubclient.MessageService                           { return nil }
func (m *mockHubClient) AllowList() hubclient.AllowListService                        { return nil }
func (m *mockHubClient) Invites() hubclient.InviteService                             { return nil }
func (m *mockHubClient) Skills() hubclient.SkillService                               { return nil }
func (m *mockHubClient) SkillRegistries() hubclient.SkillRegistryService              { return nil }
func (m *mockHubClient) Health(ctx context.Context) (*hubclient.HealthResponse, error) {
	return &hubclient.HealthResponse{}, nil
}

// --- Test helpers ---

// newFollowUpTestBridge creates a Bridge wired to a mock hub client and real SQLite store.
func newFollowUpTestBridge(t *testing.T, agents *mockAgentService) (*Bridge, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	cfg := &Config{
		Hub: HubConfig{User: "test-user"},
		Timeouts: TimeoutConfig{
			SendMessage: 2 * time.Second, // short for tests
		},
		Projects: []ProjectConfig{
			{Slug: "proj-1", ExposedAgents: []string{"agent-a"}},
		},
		Bridge: BridgeConfig{ExternalURL: "https://test.example.com"},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := &mockHubClient{agents: agents}
	b := New(store, hub, nil, cfg, nil, log)
	t.Cleanup(func() { b.Shutdown() })

	return b, store
}

// seedTask inserts a task into the store for testing.
func seedTask(t *testing.T, store *state.Store, id, contextID, projectID, agentSlug, agentID, taskState string) {
	t.Helper()
	now := time.Now()
	if err := store.CreateTask(&state.Task{
		ID:        id,
		ContextID: contextID,
		ProjectID: projectID,
		AgentSlug: agentSlug,
		AgentID:   agentID,
		State:     taskState,
		CreatedAt: now,
		UpdatedAt: now,
		Metadata:  "{}",
	}); err != nil {
		t.Fatalf("seed task %s: %v", id, err)
	}
}

var testParts = []Part{{Text: "follow-up message"}}

// --- sendFollowUp tests ---

func TestSendFollowUp_ValidTaskRoutesMessage(t *testing.T) {
	var captured struct {
		mu      sync.Mutex
		agentID string
		msg     *messages.StructuredMessage
	}

	agents := &mockAgentService{
		sendFn: func(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt, notify, wake bool) (*hubclient.MessageResponse, error) {
			captured.mu.Lock()
			defer captured.mu.Unlock()
			captured.agentID = agentID
			captured.msg = msg
			return nil, nil
		},
	}
	b, store := newFollowUpTestBridge(t, agents)
	seedTask(t, store, "task-1", "ctx-1", "proj-1", "agent-a", "agent-id-123", TaskStateWorking)

	result, err := b.SendMessage(context.Background(), "proj-1", "agent-a", "", "task-1", testParts, false)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if result.ID != "task-1" {
		t.Errorf("result.ID = %q, want %q", result.ID, "task-1")
	}
	if result.ContextID != "ctx-1" {
		t.Errorf("result.ContextID = %q, want %q", result.ContextID, "ctx-1")
	}
	if result.Status.State != TaskStateWorking {
		t.Errorf("result.Status.State = %q, want %q", result.Status.State, TaskStateWorking)
	}

	// Wait for the non-blocking goroutine to complete.
	// We can't call Shutdown() here because the cleanup already does it.
	// Instead, poll until the captured message is set.
	deadline := time.After(5 * time.Second)
	for {
		captured.mu.Lock()
		done := captured.msg != nil
		captured.mu.Unlock()
		if done {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for message to be sent")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	captured.mu.Lock()
	defer captured.mu.Unlock()
	if captured.agentID != "agent-id-123" {
		t.Errorf("sent to agentID = %q, want %q", captured.agentID, "agent-id-123")
	}
	if captured.msg == nil {
		t.Fatal("no message was sent")
	}
	if captured.msg.Metadata["a2aTaskId"] != "task-1" {
		t.Errorf("metadata a2aTaskId = %q, want %q", captured.msg.Metadata["a2aTaskId"], "task-1")
	}
	if captured.msg.Sender != "user:test-user" {
		t.Errorf("sender = %q, want %q", captured.msg.Sender, "user:test-user")
	}
	if captured.msg.Recipient != "agent:agent-a" {
		t.Errorf("recipient = %q, want %q", captured.msg.Recipient, "agent:agent-a")
	}
}

func TestSendFollowUp_UnknownTaskReturnsError(t *testing.T) {
	agents := &mockAgentService{}
	b, _ := newFollowUpTestBridge(t, agents)

	_, err := b.SendMessage(context.Background(), "proj-1", "agent-a", "", "nonexistent-task", testParts, false)
	if err == nil {
		t.Fatal("expected error for unknown task")
	}
	if !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("error = %v, want ErrAgentNotFound", err)
	}
}

func TestSendFollowUp_TerminalStateReturnsErrTaskTerminal(t *testing.T) {
	terminalStates := []string{TaskStateCompleted, TaskStateFailed, TaskStateCanceled, TaskStateRejected}

	for _, ts := range terminalStates {
		t.Run(ts, func(t *testing.T) {
			agents := &mockAgentService{}
			b, store := newFollowUpTestBridge(t, agents)
			seedTask(t, store, "task-"+ts, "ctx-1", "proj-1", "agent-a", "aid", ts)

			_, err := b.SendMessage(context.Background(), "proj-1", "agent-a", "", "task-"+ts, testParts, false)
			if err == nil {
				t.Fatal("expected error for terminal state task")
			}
			if !errors.Is(err, ErrTaskTerminal) {
				t.Errorf("error = %v, want ErrTaskTerminal", err)
			}
		})
	}
}

func TestSendFollowUp_WrongProjectReturnsError(t *testing.T) {
	agents := &mockAgentService{}
	b, store := newFollowUpTestBridge(t, agents)
	seedTask(t, store, "task-1", "ctx-1", "proj-1", "agent-a", "aid", TaskStateWorking)

	_, err := b.SendMessage(context.Background(), "proj-2", "agent-a", "", "task-1", testParts, false)
	if err == nil {
		t.Fatal("expected error for wrong project")
	}
	if !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("error = %v, want ErrAgentNotFound", err)
	}
}

func TestSendFollowUp_WrongAgentReturnsError(t *testing.T) {
	agents := &mockAgentService{}
	b, store := newFollowUpTestBridge(t, agents)
	seedTask(t, store, "task-1", "ctx-1", "proj-1", "agent-a", "aid", TaskStateWorking)

	_, err := b.SendMessage(context.Background(), "proj-1", "agent-b", "", "task-1", testParts, false)
	if err == nil {
		t.Fatal("expected error for wrong agent")
	}
	if !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("error = %v, want ErrAgentNotFound", err)
	}
}

func TestSendFollowUp_UpdatesTaskStateToWorking(t *testing.T) {
	agents := &mockAgentService{
		sendFn: func(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt, notify, wake bool) (*hubclient.MessageResponse, error) {
			return nil, nil
		},
	}
	b, store := newFollowUpTestBridge(t, agents)
	seedTask(t, store, "task-1", "ctx-1", "proj-1", "agent-a", "aid", TaskStateInputRequired)

	_, err := b.SendMessage(context.Background(), "proj-1", "agent-a", "", "task-1", testParts, false)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// The state should be updated to working immediately (before the send goroutine).
	task, err := store.GetTask("task-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != TaskStateWorking {
		t.Errorf("task state = %q, want %q", task.State, TaskStateWorking)
	}
}

func TestSendFollowUp_BlockingTimeout_CleansUpActiveTask(t *testing.T) {
	// Create a send function that succeeds but never triggers a response.
	agents := &mockAgentService{
		sendFn: func(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt, notify, wake bool) (*hubclient.MessageResponse, error) {
			return nil, nil
		},
	}

	dir := t.TempDir()
	store, err := state.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	cfg := &Config{
		Hub: HubConfig{User: "test-user"},
		Timeouts: TimeoutConfig{
			SendMessage: 100 * time.Millisecond, // very short for test
		},
		Projects: []ProjectConfig{{Slug: "proj-1", ExposedAgents: []string{"agent-a"}}},
		Bridge:   BridgeConfig{ExternalURL: "https://test.example.com"},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := &mockHubClient{agents: agents}
	b := New(store, hub, nil, cfg, nil, log)
	t.Cleanup(func() { b.Shutdown() })

	seedTask(t, store, "task-1", "ctx-1", "proj-1", "agent-a", "aid", TaskStateWorking)

	_, err = b.SendMessage(context.Background(), "proj-1", "agent-a", "", "task-1", testParts, true)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if got := err.Error(); !strings.Contains(got, "timeout") {
		t.Errorf("error = %q, want timeout message", got)
	}

	// Verify activeTask was cleaned up.
	b.tasksMu.RLock()
	_, exists := b.activeTasks["task-1"]
	b.tasksMu.RUnlock()
	if exists {
		t.Error("expected activeTask to be cleaned up after timeout")
	}

	// Verify waiter was cleaned up.
	b.mu.RLock()
	_, waiterExists := b.waiters["task-1"]
	b.mu.RUnlock()
	if waiterExists {
		t.Error("expected waiter to be cleaned up after timeout")
	}

	// Verify the DB state was set to failed on timeout.
	task, getErr := store.GetTask("task-1")
	if getErr != nil {
		t.Fatalf("GetTask: %v", getErr)
	}
	if task.State != TaskStateFailed {
		t.Errorf("task state = %q, want %q after blocking timeout", task.State, TaskStateFailed)
	}
}

func TestSendFollowUp_BlockingSendFailure_CleansUpActiveTask(t *testing.T) {
	sendErr := fmt.Errorf("hub unreachable")
	agents := &mockAgentService{
		sendFn: func(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt, notify, wake bool) (*hubclient.MessageResponse, error) {
			return nil, sendErr
		},
	}
	b, store := newFollowUpTestBridge(t, agents)
	seedTask(t, store, "task-1", "ctx-1", "proj-1", "agent-a", "aid", TaskStateWorking)

	_, err := b.SendMessage(context.Background(), "proj-1", "agent-a", "", "task-1", testParts, true)
	if err == nil {
		t.Fatal("expected send error")
	}

	// Verify activeTask was cleaned up.
	b.tasksMu.RLock()
	_, exists := b.activeTasks["task-1"]
	b.tasksMu.RUnlock()
	if exists {
		t.Error("expected activeTask to be cleaned up after send failure")
	}

	// Verify the DB state was set to failed on send failure.
	task, getErr := store.GetTask("task-1")
	if getErr != nil {
		t.Fatalf("GetTask: %v", getErr)
	}
	if task.State != TaskStateFailed {
		t.Errorf("task state = %q, want %q after blocking send failure", task.State, TaskStateFailed)
	}
}

func TestSendFollowUp_NonBlocking_RegistersActiveTask(t *testing.T) {
	sendCh := make(chan struct{})
	agents := &mockAgentService{
		sendFn: func(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt, notify, wake bool) (*hubclient.MessageResponse, error) {
			<-sendCh // Block until test releases
			return nil, nil
		},
	}
	b, store := newFollowUpTestBridge(t, agents)
	seedTask(t, store, "task-1", "ctx-1", "proj-1", "agent-a", "aid", TaskStateWorking)

	result, err := b.SendMessage(context.Background(), "proj-1", "agent-a", "", "task-1", testParts, false)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if result.Status.State != TaskStateWorking {
		t.Errorf("status.state = %q, want %q", result.Status.State, TaskStateWorking)
	}

	// Active task should be registered while goroutine is in flight.
	b.tasksMu.RLock()
	entry, exists := b.activeTasks["task-1"]
	b.tasksMu.RUnlock()
	if !exists {
		t.Error("expected activeTask to be registered for non-blocking follow-up")
	}
	if entry.aKey != "proj-1:agent-a" {
		t.Errorf("activeTask aKey = %q, want %q", entry.aKey, "proj-1:agent-a")
	}

	// Check agentTasks reverse map.
	b.tasksMu.RLock()
	taskIDs := b.agentTasks["proj-1:agent-a"]
	b.tasksMu.RUnlock()
	found := false
	for _, id := range taskIDs {
		if id == "task-1" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected task-1 in agentTasks reverse map")
	}

	// Release the goroutine and wait for shutdown.
	close(sendCh)
}

func TestSendFollowUp_NonBlocking_SendFailure_CleansUp(t *testing.T) {
	agents := &mockAgentService{
		sendFn: func(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt, notify, wake bool) (*hubclient.MessageResponse, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}
	b, store := newFollowUpTestBridge(t, agents)
	seedTask(t, store, "task-1", "ctx-1", "proj-1", "agent-a", "aid", TaskStateWorking)

	_, err := b.SendMessage(context.Background(), "proj-1", "agent-a", "", "task-1", testParts, false)
	if err != nil {
		t.Fatalf("SendMessage should not fail in non-blocking mode: %v", err)
	}

	// Wait for background goroutine to complete by polling for the task state change.
	deadline := time.After(5 * time.Second)
	for {
		task, _ := store.GetTask("task-1")
		if task != nil && task.State == TaskStateFailed {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for non-blocking send failure to update state")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// After send failure, activeTask should be unregistered.
	b.tasksMu.RLock()
	_, exists := b.activeTasks["task-1"]
	b.tasksMu.RUnlock()
	if exists {
		t.Error("expected activeTask to be cleaned up after non-blocking send failure")
	}

	// Task state should be set to failed.
	task, err := store.GetTask("task-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != TaskStateFailed {
		t.Errorf("task state = %q, want %q after send failure", task.State, TaskStateFailed)
	}
}

func TestSendFollowUp_BlockingSuccess_CleansUpActiveTask(t *testing.T) {
	agents := &mockAgentService{
		sendFn: func(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt, notify, wake bool) (*hubclient.MessageResponse, error) {
			return nil, nil
		},
	}
	b, store := newFollowUpTestBridge(t, agents)
	seedTask(t, store, "task-1", "ctx-1", "proj-1", "agent-a", "aid", TaskStateInputRequired)

	// Start blocking SendMessage in a goroutine.
	type sendResult struct {
		result *TaskResult
		err    error
	}
	resultCh := make(chan sendResult, 1)
	go func() {
		r, err := b.SendMessage(context.Background(), "proj-1", "agent-a", "", "task-1", testParts, true)
		resultCh <- sendResult{r, err}
	}()

	// Wait for the waiter to be registered.
	var waiterFound bool
	for i := 0; i < 100; i++ {
		b.mu.RLock()
		_, waiterFound = b.waiters["task-1"]
		b.mu.RUnlock()
		if waiterFound {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !waiterFound {
		t.Fatal("waiter not registered within timeout")
	}

	// Simulate a response from the agent.
	b.mu.RLock()
	w := b.waiters["task-1"]
	b.mu.RUnlock()

	response := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Msg:       "Here is my response",
		Type:      messages.TypeAssistantReply,
	}
	w.ch <- response

	// Wait for result.
	select {
	case sr := <-resultCh:
		if sr.err != nil {
			t.Fatalf("SendMessage: %v", sr.err)
		}
		if sr.result.ID != "task-1" {
			t.Errorf("result.ID = %q, want %q", sr.result.ID, "task-1")
		}
		if sr.result.Status.State != TaskStateWorking {
			t.Errorf("result.Status.State = %q, want %q", sr.result.Status.State, TaskStateWorking)
		}
		if sr.result.Status.Message == nil {
			t.Fatal("expected status message in result")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for blocking result")
	}

	// Verify activeTask was cleaned up on success path (Bug 1 fix).
	b.tasksMu.RLock()
	_, exists := b.activeTasks["task-1"]
	b.tasksMu.RUnlock()
	if exists {
		t.Error("expected activeTask to be cleaned up after successful blocking follow-up")
	}

	// Verify the DB state was refreshed to working on success.
	task, getErr := store.GetTask("task-1")
	if getErr != nil {
		t.Fatalf("GetTask: %v", getErr)
	}
	if task.State != TaskStateWorking {
		t.Errorf("task state = %q, want %q after blocking success", task.State, TaskStateWorking)
	}
}

func TestSendFollowUp_BlockingContextCancel_CleansUp(t *testing.T) {
	agents := &mockAgentService{
		sendFn: func(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt, notify, wake bool) (*hubclient.MessageResponse, error) {
			return nil, nil
		},
	}
	b, store := newFollowUpTestBridge(t, agents)
	seedTask(t, store, "task-1", "ctx-1", "proj-1", "agent-a", "aid", TaskStateWorking)

	ctx, cancel := context.WithCancel(context.Background())

	type sendResult struct {
		result *TaskResult
		err    error
	}
	resultCh := make(chan sendResult, 1)
	go func() {
		r, err := b.SendMessage(ctx, "proj-1", "agent-a", "", "task-1", testParts, true)
		resultCh <- sendResult{r, err}
	}()

	// Wait for waiter to be registered.
	for i := 0; i < 100; i++ {
		b.mu.RLock()
		_, ok := b.waiters["task-1"]
		b.mu.RUnlock()
		if ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Cancel the context.
	cancel()

	select {
	case sr := <-resultCh:
		if sr.err == nil {
			t.Fatal("expected context canceled error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cancel")
	}

	// Verify cleanup.
	b.tasksMu.RLock()
	_, exists := b.activeTasks["task-1"]
	b.tasksMu.RUnlock()
	if exists {
		t.Error("expected activeTask to be cleaned up after context cancel")
	}

	// Verify the DB state was set to failed on context cancel.
	task, getErr := store.GetTask("task-1")
	if getErr != nil {
		t.Fatalf("GetTask: %v", getErr)
	}
	if task.State != TaskStateFailed {
		t.Errorf("task state = %q, want %q after context cancel", task.State, TaskStateFailed)
	}
}

func TestSendFollowUp_InputRequiredToWorking(t *testing.T) {
	agents := &mockAgentService{
		sendFn: func(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt, notify, wake bool) (*hubclient.MessageResponse, error) {
			return nil, nil
		},
	}
	b, store := newFollowUpTestBridge(t, agents)
	seedTask(t, store, "task-1", "ctx-1", "proj-1", "agent-a", "aid", TaskStateInputRequired)

	result, err := b.SendMessage(context.Background(), "proj-1", "agent-a", "", "task-1", testParts, false)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if result.Status.State != TaskStateWorking {
		t.Errorf("status.state = %q, want %q", result.Status.State, TaskStateWorking)
	}

	// Verify the store was updated from input-required to working.
	task, err := store.GetTask("task-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != TaskStateWorking {
		t.Errorf("stored state = %q, want %q", task.State, TaskStateWorking)
	}
}

func TestSendFollowUp_SubmittedStateAllowed(t *testing.T) {
	agents := &mockAgentService{
		sendFn: func(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt, notify, wake bool) (*hubclient.MessageResponse, error) {
			return nil, nil
		},
	}
	b, store := newFollowUpTestBridge(t, agents)
	seedTask(t, store, "task-1", "ctx-1", "proj-1", "agent-a", "aid", TaskStateSubmitted)

	_, err := b.SendMessage(context.Background(), "proj-1", "agent-a", "", "task-1", testParts, false)
	if err != nil {
		t.Fatalf("SendMessage should allow follow-up on submitted task: %v", err)
	}
}

func TestSendFollowUp_ResolvesAgentIDViaLookup(t *testing.T) {
	var mu sync.Mutex
	var capturedAgentID string
	agents := &mockAgentService{
		sendFn: func(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt, notify, wake bool) (*hubclient.MessageResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			capturedAgentID = agentID
			return nil, nil
		},
		listFn: func(ctx context.Context, opts *hubclient.ListAgentsOptions) (*hubclient.ListAgentsResponse, error) {
			return &hubclient.ListAgentsResponse{
				Agents: []hubclient.Agent{
					{ID: "new-agent-id", Slug: "agent-a", ProjectID: "proj-1"},
				},
			}, nil
		},
	}
	b, store := newFollowUpTestBridge(t, agents)
	// Seed task with an old agent ID that should be overridden by lookup.
	seedTask(t, store, "task-1", "ctx-1", "proj-1", "agent-a", "old-agent-id", TaskStateWorking)

	_, err := b.SendMessage(context.Background(), "proj-1", "agent-a", "", "task-1", testParts, false)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Poll until the send function is called.
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		val := capturedAgentID
		mu.Unlock()
		if val != "" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for send")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if capturedAgentID != "new-agent-id" {
		t.Errorf("message sent to %q, want %q (should use re-resolved agent ID)", capturedAgentID, "new-agent-id")
	}
}

// --- Server-layer tests for handleSendMessage with TaskID ---

func TestHandleSendMessage_PassesTaskIDToSendMessage(t *testing.T) {
	dir := t.TempDir()
	store, err := state.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	defer store.Close()

	var mu sync.Mutex
	var capturedMeta map[string]string
	agents := &mockAgentService{
		sendFn: func(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt, notify, wake bool) (*hubclient.MessageResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			capturedMeta = msg.Metadata
			return nil, nil
		},
	}

	cfg := &Config{
		Bridge:   BridgeConfig{ExternalURL: "https://test.example.com"},
		Hub:      HubConfig{User: "test-user"},
		Auth:     AuthConfig{Scheme: "apiKey", APIKey: "test-key"},
		Projects: []ProjectConfig{{Slug: "proj-1", ExposedAgents: []string{"agent-a"}}},
		Timeouts: TimeoutConfig{SendMessage: 2 * time.Second},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := &mockHubClient{agents: agents}
	bridge := New(store, hub, nil, cfg, nil, log)
	defer bridge.Shutdown()
	srv := NewServer(bridge, cfg, nil, log)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	seedTask(t, store, "existing-task", "ctx-1", "proj-1", "agent-a", "aid", TaskStateWorking)

	params := SendMessageParams{
		TaskID: "existing-task",
		Message: Message{
			Role:  RoleUser,
			Parts: []Part{{Text: "follow up"}},
		},
		Configuration: &SendMessageConfig{
			Blocking: boolPtr(false),
		},
	}

	rpcResp := doRPC(t, ts, "/projects/proj-1/agents/agent-a/jsonrpc",
		"message/send", params, "test-key")

	if rpcResp.Error != nil {
		t.Fatalf("unexpected error: code=%d msg=%s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	// Poll until the send function captures metadata.
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		done := capturedMeta != nil
		mu.Unlock()
		if done {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for send to complete")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if capturedMeta["a2aTaskId"] != "existing-task" {
		t.Errorf("metadata a2aTaskId = %q, want %q", capturedMeta["a2aTaskId"], "existing-task")
	}
}

func TestHandleSendMessage_ErrTaskTerminal_ReturnsCorrectError(t *testing.T) {
	dir := t.TempDir()
	store, err := state.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	defer store.Close()

	agents := &mockAgentService{}
	cfg := &Config{
		Bridge:   BridgeConfig{ExternalURL: "https://test.example.com"},
		Hub:      HubConfig{User: "test-user"},
		Auth:     AuthConfig{Scheme: "apiKey", APIKey: "test-key"},
		Projects: []ProjectConfig{{Slug: "proj-1", ExposedAgents: []string{"agent-a"}}},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := &mockHubClient{agents: agents}
	bridge := New(store, hub, nil, cfg, nil, log)
	defer bridge.Shutdown()
	srv := NewServer(bridge, cfg, nil, log)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	seedTask(t, store, "done-task", "ctx-1", "proj-1", "agent-a", "aid", TaskStateCompleted)

	params := SendMessageParams{
		TaskID: "done-task",
		Message: Message{
			Role:  RoleUser,
			Parts: []Part{{Text: "try to follow up"}},
		},
	}

	rpcResp := doRPC(t, ts, "/projects/proj-1/agents/agent-a/jsonrpc",
		"message/send", params, "test-key")

	if rpcResp.Error == nil {
		t.Fatal("expected error for terminal task")
	}
	if rpcResp.Error.Code != ErrCodeInvalidParams {
		t.Errorf("error code = %d, want %d", rpcResp.Error.Code, ErrCodeInvalidParams)
	}
	if rpcResp.Error.Message != "task is in a terminal state" {
		t.Errorf("error message = %q, want %q", rpcResp.Error.Message, "task is in a terminal state")
	}
}

func TestHandleSendMessage_UnknownTaskID_ReturnsAgentNotFound(t *testing.T) {
	dir := t.TempDir()
	store, err := state.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	defer store.Close()

	agents := &mockAgentService{}
	cfg := &Config{
		Bridge:   BridgeConfig{ExternalURL: "https://test.example.com"},
		Hub:      HubConfig{User: "test-user"},
		Auth:     AuthConfig{Scheme: "apiKey", APIKey: "test-key"},
		Projects: []ProjectConfig{{Slug: "proj-1", ExposedAgents: []string{"agent-a"}}},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := &mockHubClient{agents: agents}
	bridge := New(store, hub, nil, cfg, nil, log)
	defer bridge.Shutdown()
	srv := NewServer(bridge, cfg, nil, log)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	params := SendMessageParams{
		TaskID: "no-such-task",
		Message: Message{
			Role:  RoleUser,
			Parts: []Part{{Text: "follow up"}},
		},
	}

	rpcResp := doRPC(t, ts, "/projects/proj-1/agents/agent-a/jsonrpc",
		"message/send", params, "test-key")

	if rpcResp.Error == nil {
		t.Fatal("expected error for unknown task ID")
	}
	if rpcResp.Error.Code != ErrCodeInvalidParams {
		t.Errorf("error code = %d, want %d", rpcResp.Error.Code, ErrCodeInvalidParams)
	}
	if rpcResp.Error.Message != "agent not found" {
		t.Errorf("error message = %q, want %q", rpcResp.Error.Message, "agent not found")
	}
}

func TestHandleSendMessage_NoTaskID_RoutesToNewTask(t *testing.T) {
	// When TaskID is empty, SendMessage should try to create a new task (and fail
	// because there's no real hub client to resolve the context). This verifies
	// the router correctly falls through to the new-task path.
	dir := t.TempDir()
	store, err := state.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	defer store.Close()

	agents := &mockAgentService{
		listFn: func(ctx context.Context, opts *hubclient.ListAgentsOptions) (*hubclient.ListAgentsResponse, error) {
			return &hubclient.ListAgentsResponse{
				Agents: []hubclient.Agent{
					{ID: "agent-id-1", Slug: "agent-a", ProjectID: "proj-1"},
				},
			}, nil
		},
		sendFn: func(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt, notify, wake bool) (*hubclient.MessageResponse, error) {
			return nil, nil
		},
	}
	cfg := &Config{
		Bridge:   BridgeConfig{ExternalURL: "https://test.example.com"},
		Hub:      HubConfig{User: "test-user"},
		Auth:     AuthConfig{Scheme: "apiKey", APIKey: "test-key"},
		Projects: []ProjectConfig{{Slug: "proj-1", ExposedAgents: []string{"agent-a"}}},
		Timeouts: TimeoutConfig{SendMessage: 2 * time.Second},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := &mockHubClient{agents: agents}
	bridge := New(store, hub, nil, cfg, nil, log)
	defer bridge.Shutdown()
	srv := NewServer(bridge, cfg, nil, log)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	params := SendMessageParams{
		Message: Message{
			Role:  RoleUser,
			Parts: []Part{{Text: "new message"}},
		},
		Configuration: &SendMessageConfig{
			Blocking: boolPtr(false),
		},
	}

	rpcResp := doRPC(t, ts, "/projects/proj-1/agents/agent-a/jsonrpc",
		"message/send", params, "test-key")

	// Should succeed — the new task path creates a context and task.
	if rpcResp.Error != nil {
		t.Fatalf("unexpected error: code=%d msg=%s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	resultBytes, err2 := json.Marshal(rpcResp.Result)
	if err2 != nil {
		t.Fatalf("marshal result: %v", err2)
	}
	var result TaskResult
	if err2 = json.Unmarshal(resultBytes, &result); err2 != nil {
		t.Fatalf("unmarshal result: %v", err2)
	}

	if result.ID == "" {
		t.Error("expected non-empty task ID for new task")
	}
	if result.Status.State != TaskStateSubmitted {
		t.Errorf("status.state = %q, want %q", result.Status.State, TaskStateSubmitted)
	}
}

func TestSendFollowUp_SendMessageParams_TaskIDField(t *testing.T) {
	// Verify the TaskID field is correctly parsed from JSON.
	raw := `{"taskId":"my-task-123","message":{"role":"user","parts":[{"text":"hi"}]}}`
	var params SendMessageParams
	if err := json.Unmarshal([]byte(raw), &params); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if params.TaskID != "my-task-123" {
		t.Errorf("TaskID = %q, want %q", params.TaskID, "my-task-123")
	}
}

func TestSendFollowUp_ConcurrentFollowUps_SameTask(t *testing.T) {
	// Verify that concurrent follow-ups for the same task don't panic or corrupt state.
	var mu sync.Mutex
	sendCount := 0
	agents := &mockAgentService{
		sendFn: func(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt, notify, wake bool) (*hubclient.MessageResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			sendCount++
			return nil, nil
		},
	}
	b, store := newFollowUpTestBridge(t, agents)
	seedTask(t, store, "task-1", "ctx-1", "proj-1", "agent-a", "aid", TaskStateWorking)

	// Send 5 concurrent follow-ups.
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = b.SendMessage(context.Background(), "proj-1", "agent-a", "", "task-1",
				[]Part{{Text: "concurrent follow-up"}}, false)
		}()
	}
	wg.Wait()

	// Wait for all goroutines to complete.
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		n := sendCount
		mu.Unlock()
		if n >= 5 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out: only %d/5 sends completed", n)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Verify that agentTasks has at most one entry for the task — concurrent
	// registerActiveTask calls must not produce duplicate entries.
	b.tasksMu.RLock()
	taskIDs := b.agentTasks["proj-1:agent-a"]
	dupes := 0
	for _, id := range taskIDs {
		if id == "task-1" {
			dupes++
		}
	}
	b.tasksMu.RUnlock()
	if dupes > 1 {
		t.Errorf("agentTasks has %d entries for task-1, want at most 1", dupes)
	}
}

func TestSendFollowUp_MessageContentTranslated(t *testing.T) {
	// Verify the A2A parts are correctly translated to Scion format.
	var capturedMsg *messages.StructuredMessage
	agents := &mockAgentService{
		sendFn: func(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt, notify, wake bool) (*hubclient.MessageResponse, error) {
			capturedMsg = msg
			return nil, nil
		},
	}
	b, store := newFollowUpTestBridge(t, agents)
	seedTask(t, store, "task-1", "ctx-1", "proj-1", "agent-a", "aid", TaskStateWorking)

	parts := []Part{
		{Text: "part one"},
		{Text: "part two"},
	}

	// Use blocking mode with a goroutine to inject a response.
	type result struct {
		r   *TaskResult
		err error
	}
	ch := make(chan result, 1)
	go func() {
		r, err := b.SendMessage(context.Background(), "proj-1", "agent-a", "", "task-1", parts, true)
		ch <- result{r, err}
	}()

	// Wait for waiter.
	for i := 0; i < 100; i++ {
		b.mu.RLock()
		_, ok := b.waiters["task-1"]
		b.mu.RUnlock()
		if ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Inject response.
	b.mu.RLock()
	w := b.waiters["task-1"]
	b.mu.RUnlock()
	w.ch <- &messages.StructuredMessage{
		Version: 1, Msg: "response", Type: messages.TypeAssistantReply,
	}

	res := <-ch
	if res.err != nil {
		t.Fatalf("SendMessage: %v", res.err)
	}

	// Verify the translated message content.
	if capturedMsg.Msg != "part one\npart two" {
		t.Errorf("translated msg = %q, want %q", capturedMsg.Msg, "part one\npart two")
	}
	if capturedMsg.Type != messages.TypeInstruction {
		t.Errorf("type = %q, want %q", capturedMsg.Type, messages.TypeInstruction)
	}
}

// --- Helpers ---

func boolPtr(b bool) *bool { return &b }
