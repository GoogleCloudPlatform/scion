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

package runtimebroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Event capture helpers
// ---------------------------------------------------------------------------

// capturedEvent records a single event sent via SendEvent.
type capturedEvent struct {
	eventType string
	payload   []byte
}

// eventSink accumulates events emitted by the executor.
type eventSink struct {
	mu     sync.Mutex
	events []capturedEvent
}

func (s *eventSink) send(eventType string, payload interface{}) {
	data, _ := json.Marshal(payload)
	s.mu.Lock()
	s.events = append(s.events, capturedEvent{eventType: eventType, payload: data})
	s.mu.Unlock()
}

func (s *eventSink) all() []capturedEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]capturedEvent, len(s.events))
	copy(cp, s.events)
	return cp
}

// waitForEvent blocks until an event of the given type is received or timeout.
func (s *eventSink) waitForEvent(t *testing.T, eventType string, timeout time.Duration) capturedEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, ev := range s.all() {
			if ev.eventType == eventType {
				return ev
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for event %q", eventType)
	return capturedEvent{}
}

// outputPayload decodes a workflow_output event payload.
func outputPayload(t *testing.T, ev capturedEvent) wsprotocol.WorkflowOutputPayload {
	t.Helper()
	var p wsprotocol.WorkflowOutputPayload
	require.NoError(t, json.Unmarshal(ev.payload, &p))
	return p
}

// outputError extracts the Error string from a workflow_output payload.
func outputError(t *testing.T, ev capturedEvent) string {
	t.Helper()
	p := outputPayload(t, ev)
	if p.Error == nil {
		return ""
	}
	return *p.Error
}

// ---------------------------------------------------------------------------
// Fake runtime
// ---------------------------------------------------------------------------

// fakeRuntime implements workflowRuntime for testing.
// It is configured per-test via exported fields.
type fakeRuntime struct {
	mu sync.Mutex

	// RunFunc replaces Run when set.
	RunFunc func(ctx context.Context, config scionrt.RunConfig) (string, error)
	// RunCfgCapture records the RunConfig passed to Run for assertion.
	RunCfgCapture *scionrt.RunConfig

	// ContainerID is the ID returned by Run when RunFunc is nil.
	ContainerID string

	// Logs is the string returned by GetLogs; it can be changed between polls.
	Logs string

	// Phase is the phase reported by List; use "" for running, "stopped" for done.
	Phase string

	// StopCalled records whether Stop was called.
	StopCalled bool
	// DeleteCalled records whether Delete was called.
	DeleteCalled bool

	// ListErr, if non-nil, is returned by List.
	ListErr error
	// LogsErr, if non-nil, is returned by GetLogs.
	LogsErr error
}

func (f *fakeRuntime) Run(ctx context.Context, config scionrt.RunConfig) (string, error) {
	f.mu.Lock()
	f.RunCfgCapture = &config
	fn := f.RunFunc
	id := f.ContainerID
	f.mu.Unlock()

	if fn != nil {
		return fn(ctx, config)
	}
	if id == "" {
		id = "fake-container-id"
	}
	return id, nil
}

func (f *fakeRuntime) Stop(ctx context.Context, id string) error {
	f.mu.Lock()
	f.StopCalled = true
	f.mu.Unlock()
	return nil
}

func (f *fakeRuntime) Delete(ctx context.Context, id string) error {
	f.mu.Lock()
	f.DeleteCalled = true
	f.mu.Unlock()
	return nil
}

func (f *fakeRuntime) GetLogs(ctx context.Context, id string) (string, error) {
	f.mu.Lock()
	logs := f.Logs
	err := f.LogsErr
	f.mu.Unlock()
	return logs, err
}

func (f *fakeRuntime) List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
	f.mu.Lock()
	phase := f.Phase
	err := f.ListErr
	id := f.ContainerID
	f.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if id == "" {
		id = "fake-container-id"
	}
	return []api.AgentInfo{
		{
			ContainerID: id,
			Phase:       phase,
		},
	}, nil
}

// setPhase updates Phase under the lock (safe for concurrent use from goroutines).
func (f *fakeRuntime) setPhase(phase string) {
	f.mu.Lock()
	f.Phase = phase
	f.mu.Unlock()
}

// setLogs updates Logs under the lock.
func (f *fakeRuntime) setLogs(logs string) {
	f.mu.Lock()
	f.Logs = logs
	f.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Constructor helpers
// ---------------------------------------------------------------------------

// newContainerTestExecutor builds a WorkflowExecutor with the given fake runtime
// and event sink for container-path tests.
func newContainerTestExecutor(rt *fakeRuntime, sink *eventSink) *WorkflowExecutor {
	ex := newWorkflowExecutorWithRuntime(
		"test-broker",
		rt,
		"test-agent-image:latest",
		func(_ string) *ControlChannelClient { return nil },
		slog.Default(),
	)
	ex.testEventSink = sink.send
	return ex
}

// ---------------------------------------------------------------------------
// Legacy HTTP handler tests (no actual container invocation)
// ---------------------------------------------------------------------------

// mockControlChannel captures events sent by the executor without a real
// WebSocket connection. Kept for backward-compat with pre-3c tests that use
// the HTTP-only path and check 202 responses.
type mockControlChannel struct {
	mu     sync.Mutex
	events []capturedEvent
}

func (m *mockControlChannel) SendEvent(eventType string, payload interface{}) error {
	data, _ := json.Marshal(payload)
	m.mu.Lock()
	m.events = append(m.events, capturedEvent{eventType: eventType, payload: data})
	m.mu.Unlock()
	return nil
}

func (m *mockControlChannel) captured() []capturedEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]capturedEvent, len(m.events))
	copy(cp, m.events)
	return cp
}

// newTestExecutor returns a WorkflowExecutor wired to a nil control channel
// (HTTP-layer tests do not need event delivery).
func newTestExecutor(t *testing.T) (*WorkflowExecutor, *mockControlChannel) {
	t.Helper()
	mock := &mockControlChannel{}
	ex := newWorkflowExecutorWithRuntime(
		"test-broker",
		&fakeRuntime{Phase: "stopped"},
		"test-image:latest",
		func(_ string) *ControlChannelClient { return nil },
		slog.Default(),
	)
	_ = mock
	return ex, mock
}

func TestWorkflowExecutor_HandleCreate_MissingBody(t *testing.T) {
	exec, _ := newTestExecutor(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflow-runs", strings.NewReader("not json"))
	w := httptest.NewRecorder()

	exec.HandleCreateWorkflowRun(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestWorkflowExecutor_HandleCreate_MissingRunID(t *testing.T) {
	exec, _ := newTestExecutor(t)

	body := `{"sourceYaml":"version: \"0.7\"\n"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflow-runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	exec.HandleCreateWorkflowRun(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestWorkflowExecutor_HandleCreate_MissingSourceYAML(t *testing.T) {
	exec, _ := newTestExecutor(t)

	body := `{"runId":"run-1","groveId":"grove-1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflow-runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	exec.HandleCreateWorkflowRun(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestWorkflowExecutor_HandleCreate_Accepted(t *testing.T) {
	exec, _ := newTestExecutor(t)

	body := `{"runId":"run-accepted","groveId":"grove-1","sourceYaml":"version: \"0.7\"\nname: test\n"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflow-runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	exec.HandleCreateWorkflowRun(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code)

	var resp map[string]string
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, "accepted", resp["status"])
	assert.Equal(t, "run-accepted", resp["runId"])
}

func TestWorkflowExecutor_HandleCreate_Idempotent(t *testing.T) {
	// Sending the same runID twice should return 202 both times without
	// starting a second execution.
	exec, _ := newTestExecutor(t)

	body := `{"runId":"run-idem","groveId":"g1","sourceYaml":"version: \"0.7\"\nname: t\n"}`
	do := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/workflow-runs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		exec.HandleCreateWorkflowRun(w, req)
		return w.Code
	}

	assert.Equal(t, http.StatusAccepted, do())

	// Second call while the goroutine hasn't finished yet (run still registered).
	assert.Equal(t, http.StatusAccepted, do())
}

func TestWorkflowExecutor_HandleCancel_NotFound(t *testing.T) {
	exec, _ := newTestExecutor(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/workflow-runs/nonexistent", nil)
	w := httptest.NewRecorder()

	exec.HandleCancelWorkflowRun(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestWorkflowExecutor_HandleCancel_CancelsRun(t *testing.T) {
	exec, _ := newTestExecutor(t)

	// Register a run manually so we can cancel it without waiting for goroutine.
	cancelCalled := false
	exec.mu.Lock()
	exec.runs["run-to-cancel"] = &activeWorkflowRun{
		runID:     "run-to-cancel",
		startedAt: time.Now(),
		cancel: func() {
			cancelCalled = true
		},
	}
	exec.mu.Unlock()

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/workflow-runs/run-to-cancel", nil)
	w := httptest.NewRecorder()

	exec.HandleCancelWorkflowRun(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, cancelCalled, "cancel function should have been called")
}

func TestWorkflowExecutor_HandleCancel_MethodNotAllowed(t *testing.T) {
	exec, _ := newTestExecutor(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workflow-runs/some-run", nil)
	w := httptest.NewRecorder()

	exec.HandleCancelWorkflowRun(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

// ---------------------------------------------------------------------------
// SendEvent shape (integration-level format verification)
// ---------------------------------------------------------------------------

func TestPtrStr(t *testing.T) {
	s := "hello"
	p := ptrStr(s)
	require.NotNil(t, p)
	assert.Equal(t, s, *p)
}

func TestWorkflowExecutor_SendEventPayloads(t *testing.T) {
	// Verify that the wsprotocol payload types round-trip correctly through JSON.
	t.Run("WorkflowStatusPayload", func(t *testing.T) {
		p := wsprotocol.WorkflowStatusPayload{
			RunID:  "r1",
			Status: "running",
			At:     time.Now().Format(time.RFC3339Nano),
		}
		data, err := json.Marshal(p)
		require.NoError(t, err)
		var back wsprotocol.WorkflowStatusPayload
		require.NoError(t, json.Unmarshal(data, &back))
		assert.Equal(t, p.RunID, back.RunID)
		assert.Equal(t, p.Status, back.Status)
	})

	t.Run("WorkflowOutputPayload_Succeeded", func(t *testing.T) {
		res := `{"answer":42}`
		p := wsprotocol.WorkflowOutputPayload{
			RunID:      "r2",
			ExitCode:   0,
			ResultJSON: &res,
		}
		data, err := json.Marshal(p)
		require.NoError(t, err)
		var back wsprotocol.WorkflowOutputPayload
		require.NoError(t, json.Unmarshal(data, &back))
		assert.Equal(t, 0, back.ExitCode)
		require.NotNil(t, back.ResultJSON)
		assert.Equal(t, res, *back.ResultJSON)
	})

	t.Run("WorkflowOutputPayload_Canceled", func(t *testing.T) {
		errStr := "canceled"
		p := wsprotocol.WorkflowOutputPayload{
			RunID:    "r3",
			ExitCode: 2,
			Error:    &errStr,
		}
		data, _ := json.Marshal(p)
		var back wsprotocol.WorkflowOutputPayload
		require.NoError(t, json.Unmarshal(data, &back))
		require.NotNil(t, back.Error)
		assert.Equal(t, "canceled", *back.Error)
	})

	t.Run("WorkflowLogPayload", func(t *testing.T) {
		p := wsprotocol.WorkflowLogPayload{
			RunID:     "r4",
			Stream:    "stdout",
			Line:      "hello world\n",
			Timestamp: time.Now().Format(time.RFC3339Nano),
		}
		data, _ := json.Marshal(p)
		var back wsprotocol.WorkflowLogPayload
		require.NoError(t, json.Unmarshal(data, &back))
		assert.Equal(t, "stdout", back.Stream)
		assert.Equal(t, p.Line, back.Line)
	})
}

// TestWorkflowLogWireFormat verifies that the log wire format emitted by the
// executor matches design §3.4: a plain UTF-8 string in the "line" field, with
// no base64 encoding. This is a round-trip test: executor emits an event via
// testEventSink, the payload is marshalled to JSON, and the "line" field is
// decoded back as a plain string.
func TestWorkflowLogWireFormat(t *testing.T) {
	sink := &eventSink{}
	rt := &fakeRuntime{
		ContainerID: "ctr-wire",
		Logs:        "log line one\nlog line two\n",
		Phase:       "stopped",
	}
	ex := newContainerTestExecutor(rt, sink)

	req := workflowRunRequest{
		RunID:      "run-wire",
		GroveID:    "grove-1",
		SourceYAML: "version: \"0.7\"\nname: test\n",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &activeWorkflowRun{runID: req.RunID, cancel: cancel, startedAt: time.Now()}
	ex.mu.Lock()
	ex.runs[req.RunID] = run
	ex.mu.Unlock()

	ex.executeRun(ctx, "", req, run, cancel)

	// Find the first log event and verify the JSON wire format.
	for _, ev := range sink.all() {
		if ev.eventType != wsprotocol.EventWorkflowLog {
			continue
		}

		// Unmarshal as a raw map to inspect the wire field names directly.
		var raw map[string]interface{}
		require.NoError(t, json.Unmarshal(ev.payload, &raw), "log event must be valid JSON")

		// Design §3.4 specifies "line" (plain string), not "chunk" (base64).
		lineVal, hasLine := raw["line"]
		require.True(t, hasLine, "log event must have a 'line' field (not 'chunk')")
		lineStr, isStr := lineVal.(string)
		require.True(t, isStr, "'line' field must be a plain string")
		assert.NotEmpty(t, lineStr, "'line' field must not be empty")

		_, hasChunk := raw["chunk"]
		assert.False(t, hasChunk, "log event must NOT have a 'chunk' field")
		return
	}
	t.Fatal("expected at least one workflow_log event")
}

// ---------------------------------------------------------------------------
// Container-path tests
// ---------------------------------------------------------------------------

// TestExecutor_RunSucceeds verifies the happy path: fake runtime returns logs
// and exits with phase "stopped" (exit code 0). Executor must emit
// workflow_log + workflow_output(exitCode=0).
func TestExecutor_RunSucceeds(t *testing.T) {
	sink := &eventSink{}
	rt := &fakeRuntime{
		ContainerID: "ctr-success",
		Logs:        "hello\n",
		Phase:       "stopped", // container is immediately done
	}
	ex := newContainerTestExecutor(rt, sink)

	req := workflowRunRequest{
		RunID:      "run-success",
		GroveID:    "grove-1",
		SourceYAML: "version: \"0.7\"\nname: test\n",
	}

	// Drive executeRun directly so we can wait for it synchronously.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &activeWorkflowRun{
		runID:     req.RunID,
		groveID:   req.GroveID,
		cancel:    cancel,
		startedAt: time.Now(),
	}
	ex.mu.Lock()
	ex.runs[req.RunID] = run
	ex.mu.Unlock()

	ex.executeRun(ctx, "", req, run, cancel)

	evs := sink.all()

	// Expect at least one workflow_log event with "hello".
	var gotLog bool
	for _, ev := range evs {
		if ev.eventType == wsprotocol.EventWorkflowLog {
			var p wsprotocol.WorkflowLogPayload
			require.NoError(t, json.Unmarshal(ev.payload, &p))
			if strings.Contains(p.Line, "hello") {
				gotLog = true
			}
		}
	}
	assert.True(t, gotLog, "expected workflow_log event with 'hello'")

	// Expect a workflow_output with exit code 0.
	var outEv capturedEvent
	for _, ev := range evs {
		if ev.eventType == wsprotocol.EventWorkflowOutput {
			outEv = ev
		}
	}
	require.NotEmpty(t, outEv.eventType, "expected workflow_output event")
	p := outputPayload(t, outEv)
	assert.Equal(t, 0, p.ExitCode)
	assert.Nil(t, p.Error)
}

// TestExecutor_RunFails verifies that phase "error" from the fake runtime
// results in workflow_output with a non-zero exit code.
func TestExecutor_RunFails(t *testing.T) {
	sink := &eventSink{}
	rt := &fakeRuntime{
		ContainerID: "ctr-fail",
		Logs:        "something went wrong\n",
		Phase:       "error",
	}
	ex := newContainerTestExecutor(rt, sink)

	req := workflowRunRequest{
		RunID:      "run-fail",
		SourceYAML: "version: \"0.7\"\nname: test\n",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &activeWorkflowRun{runID: req.RunID, cancel: cancel, startedAt: time.Now()}
	ex.mu.Lock()
	ex.runs[req.RunID] = run
	ex.mu.Unlock()

	ex.executeRun(ctx, "", req, run, cancel)

	var outEv capturedEvent
	for _, ev := range sink.all() {
		if ev.eventType == wsprotocol.EventWorkflowOutput {
			outEv = ev
		}
	}
	require.NotEmpty(t, outEv.eventType)
	p := outputPayload(t, outEv)
	assert.NotEqual(t, 0, p.ExitCode, "failed run should have non-zero exit code")
	require.NotNil(t, p.Error)
	assert.Contains(t, *p.Error, "quack exited")
}

// TestExecutor_Cancel verifies that canceling the context stops and deletes
// the container and emits workflow_output with Error="canceled".
func TestExecutor_Cancel(t *testing.T) {
	sink := &eventSink{}
	// Phase stays "running" (empty) forever — simulate a long-running container.
	rt := &fakeRuntime{
		ContainerID: "ctr-cancel",
		Phase:       "running",
	}
	ex := newContainerTestExecutor(rt, sink)

	req := workflowRunRequest{
		RunID:      "run-cancel",
		SourceYAML: "version: \"0.7\"\nname: test\n",
	}

	ctx, cancel := context.WithCancel(context.Background())
	run := &activeWorkflowRun{runID: req.RunID, cancel: cancel, startedAt: time.Now()}
	ex.mu.Lock()
	ex.runs[req.RunID] = run
	ex.mu.Unlock()

	// Cancel after a brief delay so the goroutine starts.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	ex.executeRun(ctx, "", req, run, cancel)

	// Container must have been stopped and deleted.
	rt.mu.Lock()
	stopped := rt.StopCalled
	deleted := rt.DeleteCalled
	rt.mu.Unlock()
	assert.True(t, stopped, "Stop must be called on cancel")
	assert.True(t, deleted, "Delete must be called on cancel")

	// Check the output event says canceled.
	var outEv capturedEvent
	for _, ev := range sink.all() {
		if ev.eventType == wsprotocol.EventWorkflowOutput {
			outEv = ev
		}
	}
	require.NotEmpty(t, outEv.eventType)
	errMsg := outputError(t, outEv)
	assert.Equal(t, "canceled", errMsg)
}

// TestExecutor_Timeout verifies that when the container runs past the timeout,
// the executor emits workflow_output with Error="timed_out" and calls Stop+Delete.
func TestExecutor_Timeout(t *testing.T) {
	sink := &eventSink{}
	rt := &fakeRuntime{
		ContainerID: "ctr-timeout",
		Phase:       "running", // never exits on its own
	}
	ex := newContainerTestExecutor(rt, sink)

	req := workflowRunRequest{
		RunID:          "run-timeout",
		SourceYAML:     "version: \"0.7\"\nname: test\n",
		TimeoutSeconds: 1, // very short timeout
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &activeWorkflowRun{runID: req.RunID, cancel: cancel, startedAt: time.Now()}
	ex.mu.Lock()
	ex.runs[req.RunID] = run
	ex.mu.Unlock()

	ex.executeRun(ctx, "", req, run, cancel)

	rt.mu.Lock()
	stopped := rt.StopCalled
	deleted := rt.DeleteCalled
	rt.mu.Unlock()
	assert.True(t, stopped, "Stop must be called on timeout")
	assert.True(t, deleted, "Delete must be called on timeout")

	var outEv capturedEvent
	for _, ev := range sink.all() {
		if ev.eventType == wsprotocol.EventWorkflowOutput {
			outEv = ev
		}
	}
	require.NotEmpty(t, outEv.eventType)
	assert.Equal(t, "timed_out", outputError(t, outEv))
}

// TestExecutor_ContainerCreateFails verifies that if runtime.Run returns an
// error, the executor emits workflow_output with a failure message.
func TestExecutor_ContainerCreateFails(t *testing.T) {
	sink := &eventSink{}
	rt := &fakeRuntime{}
	rt.RunFunc = func(_ context.Context, _ scionrt.RunConfig) (string, error) {
		return "", errors.New("image not found")
	}
	ex := newContainerTestExecutor(rt, sink)

	req := workflowRunRequest{
		RunID:      "run-ctr-fail",
		SourceYAML: "version: \"0.7\"\nname: test\n",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &activeWorkflowRun{runID: req.RunID, cancel: cancel, startedAt: time.Now()}
	ex.mu.Lock()
	ex.runs[req.RunID] = run
	ex.mu.Unlock()

	ex.executeRun(ctx, "", req, run, cancel)

	var outEv capturedEvent
	for _, ev := range sink.all() {
		if ev.eventType == wsprotocol.EventWorkflowOutput {
			outEv = ev
		}
	}
	require.NotEmpty(t, outEv.eventType)
	p := outputPayload(t, outEv)
	assert.Equal(t, 2, p.ExitCode)
	require.NotNil(t, p.Error)
	assert.Contains(t, *p.Error, "image not found")
}

// TestExecutor_LabelsPresent verifies that the RunConfig passed to the runtime
// has the correct kind and workflow-run-id labels.
func TestExecutor_LabelsPresent(t *testing.T) {
	sink := &eventSink{}
	rt := &fakeRuntime{
		ContainerID: "ctr-labels",
		Phase:       "stopped",
	}
	ex := newContainerTestExecutor(rt, sink)

	const runID = "run-labels-test"
	req := workflowRunRequest{
		RunID:      runID,
		SourceYAML: "version: \"0.7\"\nname: test\n",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &activeWorkflowRun{runID: req.RunID, cancel: cancel, startedAt: time.Now()}
	ex.mu.Lock()
	ex.runs[req.RunID] = run
	ex.mu.Unlock()

	ex.executeRun(ctx, "", req, run, cancel)

	rt.mu.Lock()
	cfg := rt.RunCfgCapture
	rt.mu.Unlock()

	require.NotNil(t, cfg, "RunConfig must have been captured")
	assert.Equal(t, "workflow-run", cfg.Labels["scion.scion/kind"])
	assert.Equal(t, runID, cfg.Labels["scion.scion/workflow-run-id"])
}

// TestExecutor_RunSucceeds_LogPolling verifies that new log lines appended
// between polls are forwarded as separate workflow_log events.
func TestExecutor_RunSucceeds_LogPolling(t *testing.T) {
	sink := &eventSink{}
	rt := &fakeRuntime{
		ContainerID: "ctr-poll",
		Phase:       "running",
	}
	ex := newContainerTestExecutor(rt, sink)

	req := workflowRunRequest{
		RunID:      "run-poll",
		SourceYAML: "version: \"0.7\"\nname: test\n",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &activeWorkflowRun{runID: req.RunID, cancel: cancel, startedAt: time.Now()}
	ex.mu.Lock()
	ex.runs[req.RunID] = run
	ex.mu.Unlock()

	// After 100ms, append more logs and mark container done.
	go func() {
		time.Sleep(120 * time.Millisecond)
		rt.setLogs("first line\nsecond line\n")
		time.Sleep(120 * time.Millisecond)
		rt.setPhase("stopped")
	}()

	ex.executeRun(ctx, "", req, run, cancel)

	// We should have received at least one log event with content from the logs.
	var gotContent bool
	for _, ev := range sink.all() {
		if ev.eventType == wsprotocol.EventWorkflowLog {
			var p wsprotocol.WorkflowLogPayload
			_ = json.Unmarshal(ev.payload, &p)
			if strings.Contains(p.Line, "line") {
				gotContent = true
			}
		}
	}
	assert.True(t, gotContent, "expected log content from polling")
}

// TestExecutor_ImageFromConfig verifies that the executor uses the agentImage
// field (set at construction) as the Image in RunConfig.
func TestExecutor_ImageFromConfig(t *testing.T) {
	sink := &eventSink{}
	rt := &fakeRuntime{ContainerID: "ctr-img", Phase: "stopped"}
	const wantImage = "my-custom-agent:v2"

	ex := newWorkflowExecutorWithRuntime(
		"test-broker",
		rt,
		wantImage,
		func(_ string) *ControlChannelClient { return nil },
		slog.Default(),
	)
	ex.testEventSink = sink.send

	req := workflowRunRequest{
		RunID:      "run-img",
		SourceYAML: "version: \"0.7\"\nname: test\n",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &activeWorkflowRun{runID: req.RunID, cancel: cancel, startedAt: time.Now()}
	ex.mu.Lock()
	ex.runs[req.RunID] = run
	ex.mu.Unlock()

	ex.executeRun(ctx, "", req, run, cancel)

	rt.mu.Lock()
	cfg := rt.RunCfgCapture
	rt.mu.Unlock()

	require.NotNil(t, cfg)
	assert.Equal(t, wantImage, cfg.Image)
}

// TestExecutor_VolumeMount verifies that the workflow tmpdir is mounted at
// /workflow inside the container.
func TestExecutor_VolumeMount(t *testing.T) {
	sink := &eventSink{}
	rt := &fakeRuntime{ContainerID: "ctr-vol", Phase: "stopped"}
	ex := newContainerTestExecutor(rt, sink)

	req := workflowRunRequest{
		RunID:      "run-vol",
		SourceYAML: "version: \"0.7\"\nname: test\n",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &activeWorkflowRun{runID: req.RunID, cancel: cancel, startedAt: time.Now()}
	ex.mu.Lock()
	ex.runs[req.RunID] = run
	ex.mu.Unlock()

	ex.executeRun(ctx, "", req, run, cancel)

	rt.mu.Lock()
	cfg := rt.RunCfgCapture
	rt.mu.Unlock()

	require.NotNil(t, cfg)
	require.NotEmpty(t, cfg.Volumes, "expected at least one volume mount")
	var found bool
	for _, v := range cfg.Volumes {
		if v.Target == "/workflow" {
			found = true
		}
	}
	assert.True(t, found, "expected /workflow volume mount")
}

// TestExecutor_CommandArgs verifies that the quack command arguments are
// passed correctly in RunConfig.
func TestExecutor_CommandArgs(t *testing.T) {
	sink := &eventSink{}
	rt := &fakeRuntime{ContainerID: "ctr-cmd", Phase: "stopped"}
	ex := newContainerTestExecutor(rt, sink)

	req := workflowRunRequest{
		RunID:      "run-cmd",
		SourceYAML: "version: \"0.7\"\nname: test\n",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &activeWorkflowRun{runID: req.RunID, cancel: cancel, startedAt: time.Now()}
	ex.mu.Lock()
	ex.runs[req.RunID] = run
	ex.mu.Unlock()

	ex.executeRun(ctx, "", req, run, cancel)

	rt.mu.Lock()
	cfg := rt.RunCfgCapture
	rt.mu.Unlock()

	require.NotNil(t, cfg)
	assert.Equal(t, "quack", cfg.CommandArgs[0])

	// Must pass the workflow file path.
	var hasWorkflowFile bool
	for _, a := range cfg.CommandArgs {
		if strings.Contains(a, "workflow.yaml") {
			hasWorkflowFile = true
		}
	}
	assert.True(t, hasWorkflowFile, "quack command must reference workflow.yaml")

	// Must pass --trace-dir.
	joinedArgs := strings.Join(cfg.CommandArgs, " ")
	assert.Contains(t, joinedArgs, "--trace-dir")
}

// ---------------------------------------------------------------------------
// itoa helper
// ---------------------------------------------------------------------------

func TestItoa(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{42, "42"},
		{-5, "-5"},
		{1000, "1000"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, itoa(c.in), fmt.Sprintf("itoa(%d)", c.in))
	}
}
