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

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

// WorkflowLogEntry is a single buffered log line for a workflow run.
type WorkflowLogEntry struct {
	Stream    string
	Line      string // plain UTF-8 text (matches the client-facing wire format, design §3.4)
	Timestamp time.Time
}

// WorkflowRunSubscriber is a channel that receives workflow run events.
type WorkflowRunSubscriber chan WorkflowRunEvent

// WorkflowRunEvent is an event emitted for a specific workflow run.
type WorkflowRunEventKind string

const (
	WorkflowRunEventLog      WorkflowRunEventKind = "log"
	WorkflowRunEventStatus   WorkflowRunEventKind = "status"
	WorkflowRunEventTerminal WorkflowRunEventKind = "terminal"
)

// WorkflowRunEvent carries a single event to subscribers.
type WorkflowRunEvent struct {
	Kind      WorkflowRunEventKind
	RunID     string
	Status    string
	Log       *WorkflowLogEntry
	ExitCode  int
	Error     string
	Timestamp time.Time
}

// WorkflowRunDispatcher manages the lifecycle dispatch for workflow runs.
// It selects a broker for queued runs, sends run_workflow commands, and
// updates the store in response to broker events.
type WorkflowRunDispatcher struct {
	store          store.Store
	controlChannel *ControlChannelManager
	log            *slog.Logger

	// Per-run event subscribers (for WSS log streaming).
	subsMu sync.RWMutex
	subs   map[string][]WorkflowRunSubscriber

	// Per-run buffered logs (for replay when a WSS client connects late).
	logsMu sync.RWMutex
	logs   map[string][]WorkflowLogEntry
}

// NewWorkflowRunDispatcher creates a new WorkflowRunDispatcher.
func NewWorkflowRunDispatcher(st store.Store, cc *ControlChannelManager, log *slog.Logger) *WorkflowRunDispatcher {
	return &WorkflowRunDispatcher{
		store:          st,
		controlChannel: cc,
		log:            log,
		subs:           make(map[string][]WorkflowRunSubscriber),
		logs:           make(map[string][]WorkflowLogEntry),
	}
}

// DispatchAsync picks a broker and sends a run_workflow command to it,
// then transitions the run from queued → provisioning. This runs in a
// goroutine (fire-and-forget from the HTTP handler perspective) so that
// the 201 response returns before the broker round-trip completes.
//
// The function uses a background context so it is not cancelled when the
// originating HTTP request finishes.
func (d *WorkflowRunDispatcher) DispatchAsync(runID string) {
	go d.dispatch(context.Background(), runID)
}

// dispatch is the synchronous inner dispatch logic.
func (d *WorkflowRunDispatcher) dispatch(ctx context.Context, runID string) {
	run, err := d.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		d.log.Error("WorkflowRunDispatcher: failed to fetch run", "runID", runID, "error", err)
		return
	}
	if run.Status != store.WorkflowRunStatusQueued {
		// Already advanced (e.g. cancel was called before dispatch fired).
		return
	}

	// Pick a broker for this grove.
	brokerID, err := d.selectBroker(ctx, run.GroveID)
	if err != nil {
		d.log.Warn("WorkflowRunDispatcher: no broker available", "runID", runID, "groveID", run.GroveID, "error", err)
		now := time.Now()
		errMsg := fmt.Sprintf("no online broker for grove %s: %v", run.GroveID, err)
		_, _ = d.store.TransitionWorkflowRun(ctx, runID, store.WorkflowRunTransition{
			Status:       store.WorkflowRunStatusFailed,
			ErrorMessage: &errMsg,
			FinishedAt:   &now,
		}, []string{store.WorkflowRunStatusQueued})
		return
	}

	// Transition queued → provisioning and stamp broker ID.
	_, err = d.store.TransitionWorkflowRun(ctx, runID, store.WorkflowRunTransition{
		Status:   store.WorkflowRunStatusProvisioning,
		BrokerID: &brokerID,
	}, []string{store.WorkflowRunStatusQueued})
	if err != nil {
		d.log.Warn("WorkflowRunDispatcher: transition to provisioning failed (race?)", "runID", runID, "error", err)
		return
	}

	// Build the run_workflow payload and send to the broker.
	payload := map[string]interface{}{
		"runId":          run.ID,
		"groveId":        run.GroveID,
		"sourceYaml":     run.SourceYaml,
		"inputsJson":     run.InputsJSON,
		"timeoutSeconds": 3600,
	}
	payloadBytes, _ := json.Marshal(payload)

	req := &wsprotocol.RequestEnvelope{
		Type:   wsprotocol.TypeRequest,
		Method: "POST",
		Path:   "/api/v1/workflow-runs",
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		Body: payloadBytes,
	}

	resp, err := d.controlChannel.TunnelRequest(ctx, brokerID, req)
	if err != nil {
		d.log.Error("WorkflowRunDispatcher: tunnel request failed", "runID", runID, "brokerID", brokerID, "error", err)
		now := time.Now()
		errMsg := fmt.Sprintf("broker dispatch failed: %v", err)
		_, _ = d.store.TransitionWorkflowRun(ctx, runID, store.WorkflowRunTransition{
			Status:       store.WorkflowRunStatusFailed,
			ErrorMessage: &errMsg,
			FinishedAt:   &now,
		}, []string{store.WorkflowRunStatusProvisioning})
		return
	}

	if resp.StatusCode >= 300 {
		d.log.Error("WorkflowRunDispatcher: broker rejected run", "runID", runID, "status", resp.StatusCode, "body", string(resp.Body))
		now := time.Now()
		errMsg := fmt.Sprintf("broker rejected run (HTTP %d)", resp.StatusCode)
		_, _ = d.store.TransitionWorkflowRun(ctx, runID, store.WorkflowRunTransition{
			Status:       store.WorkflowRunStatusFailed,
			ErrorMessage: &errMsg,
			FinishedAt:   &now,
		}, []string{store.WorkflowRunStatusProvisioning})
		return
	}

	d.log.Info("WorkflowRunDispatcher: run dispatched to broker", "runID", runID, "brokerID", brokerID)
}

// selectBroker picks an online broker that serves the given grove.
func (d *WorkflowRunDispatcher) selectBroker(ctx context.Context, groveID string) (string, error) {
	providers, err := d.store.GetGroveProviders(ctx, groveID)
	if err != nil {
		return "", fmt.Errorf("failed to get grove providers: %w", err)
	}

	// Prefer connected brokers (control channel connected).
	for _, p := range providers {
		if p.Status == store.BrokerStatusOnline && d.controlChannel.IsConnected(p.BrokerID) {
			return p.BrokerID, nil
		}
	}
	// Fallback: any online provider.
	for _, p := range providers {
		if p.Status == store.BrokerStatusOnline {
			return p.BrokerID, nil
		}
	}

	return "", fmt.Errorf("no online broker for grove %s", groveID)
}

// SendCancel sends a cancel_workflow command to the broker that owns the run.
func (d *WorkflowRunDispatcher) SendCancel(ctx context.Context, runID, brokerID string) error {
	payload := map[string]interface{}{
		"runId": runID,
	}
	payloadBytes, _ := json.Marshal(payload)

	req := &wsprotocol.RequestEnvelope{
		Type:   wsprotocol.TypeRequest,
		Method: "DELETE",
		Path:   fmt.Sprintf("/api/v1/workflow-runs/%s", runID),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		Body: payloadBytes,
	}

	resp, err := d.controlChannel.TunnelRequest(ctx, brokerID, req)
	if err != nil {
		return fmt.Errorf("cancel tunnel failed: %w", err)
	}
	if resp.StatusCode >= 300 && resp.StatusCode != 404 {
		return fmt.Errorf("broker cancel returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// HandleWorkflowStatusEvent processes a workflow_status event from a broker.
func (d *WorkflowRunDispatcher) HandleWorkflowStatusEvent(ctx context.Context, brokerID string, payload wsprotocol.WorkflowStatusPayload) {
	d.log.Info("WorkflowRunDispatcher: workflow status event", "runID", payload.RunID, "status", payload.Status)

	fromStatuses := []string{store.WorkflowRunStatusProvisioning}
	if payload.Status == store.WorkflowRunStatusRunning {
		fromStatuses = []string{store.WorkflowRunStatusProvisioning}
	}

	var startedAt *time.Time
	if payload.Status == store.WorkflowRunStatusRunning {
		now := time.Now()
		startedAt = &now
	}

	_, err := d.store.TransitionWorkflowRun(ctx, payload.RunID, store.WorkflowRunTransition{
		Status:    payload.Status,
		StartedAt: startedAt,
	}, fromStatuses)
	if err != nil {
		d.log.Warn("WorkflowRunDispatcher: status transition failed", "runID", payload.RunID, "status", payload.Status, "error", err)
	}

	// Notify WSS subscribers.
	d.publishEvent(payload.RunID, WorkflowRunEvent{
		Kind:      WorkflowRunEventStatus,
		RunID:     payload.RunID,
		Status:    payload.Status,
		Timestamp: time.Now(),
	})
}

// HandleWorkflowOutputEvent processes a workflow_output (terminal) event from a broker.
func (d *WorkflowRunDispatcher) HandleWorkflowOutputEvent(ctx context.Context, brokerID string, payload wsprotocol.WorkflowOutputPayload) {
	d.log.Info("WorkflowRunDispatcher: workflow output event", "runID", payload.RunID, "exitCode", payload.ExitCode)

	status := store.WorkflowRunStatusSucceeded
	if payload.ExitCode != 0 {
		status = store.WorkflowRunStatusFailed
	}
	if payload.Error != nil && *payload.Error == "canceled" {
		status = store.WorkflowRunStatusCanceled
	}
	if payload.Error != nil && *payload.Error == "timed_out" {
		status = store.WorkflowRunStatusTimedOut
	}

	now := time.Now()
	transition := store.WorkflowRunTransition{
		Status:       status,
		FinishedAt:   &now,
		ResultJSON:   payload.ResultJSON,
		ErrorMessage: payload.Error,
	}
	if payload.TraceKey != "" {
		transition.TraceURL = &payload.TraceKey
	}

	fromStatuses := []string{
		store.WorkflowRunStatusProvisioning,
		store.WorkflowRunStatusRunning,
	}
	_, err := d.store.TransitionWorkflowRun(ctx, payload.RunID, transition, fromStatuses)
	if err != nil {
		d.log.Warn("WorkflowRunDispatcher: output transition failed", "runID", payload.RunID, "error", err)
	}

	// Publish terminal event to WSS subscribers.
	errMsg := ""
	if payload.Error != nil {
		errMsg = *payload.Error
	}
	d.publishEvent(payload.RunID, WorkflowRunEvent{
		Kind:      WorkflowRunEventTerminal,
		RunID:     payload.RunID,
		Status:    status,
		ExitCode:  payload.ExitCode,
		Error:     errMsg,
		Timestamp: time.Now(),
	})

	// Clean up buffered logs and subscribers after a short delay so any
	// in-flight WSS readers can drain their queues.
	go func() {
		time.Sleep(30 * time.Second)
		d.cleanupRun(payload.RunID)
	}()
}

// HandleWorkflowLogEvent processes a workflow_log event from a broker.
func (d *WorkflowRunDispatcher) HandleWorkflowLogEvent(brokerID string, payload wsprotocol.WorkflowLogPayload) {
	entry := WorkflowLogEntry{
		Stream:    payload.Stream,
		Line:      payload.Line,
		Timestamp: time.Now(),
	}

	// Buffer for replay.
	d.logsMu.Lock()
	d.logs[payload.RunID] = append(d.logs[payload.RunID], entry)
	d.logsMu.Unlock()

	// Notify live subscribers.
	d.publishEvent(payload.RunID, WorkflowRunEvent{
		Kind:      WorkflowRunEventLog,
		RunID:     payload.RunID,
		Log:       &entry,
		Timestamp: entry.Timestamp,
	})
}

// Subscribe registers a subscriber channel for events on a workflow run.
// Returns a list of buffered log entries for replay, and an unsubscribe function.
func (d *WorkflowRunDispatcher) Subscribe(runID string) ([]WorkflowLogEntry, WorkflowRunSubscriber, func()) {
	ch := make(WorkflowRunSubscriber, 128)

	// Capture buffered logs before subscribing.
	d.logsMu.RLock()
	buffered := make([]WorkflowLogEntry, len(d.logs[runID]))
	copy(buffered, d.logs[runID])
	d.logsMu.RUnlock()

	d.subsMu.Lock()
	d.subs[runID] = append(d.subs[runID], ch)
	d.subsMu.Unlock()

	unsub := func() {
		d.subsMu.Lock()
		defer d.subsMu.Unlock()
		subs := d.subs[runID]
		for i, s := range subs {
			if s == ch {
				d.subs[runID] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
		// Close the channel to unblock any waiting reader.
		close(ch)
	}

	return buffered, ch, unsub
}

// publishEvent sends an event to all subscribers of a run.
func (d *WorkflowRunDispatcher) publishEvent(runID string, evt WorkflowRunEvent) {
	d.subsMu.RLock()
	subs := d.subs[runID]
	d.subsMu.RUnlock()

	for _, s := range subs {
		select {
		case s <- evt:
		default:
			// Subscriber queue full; drop to avoid blocking.
		}
	}
}

// cleanupRun removes buffered state for a completed run.
func (d *WorkflowRunDispatcher) cleanupRun(runID string) {
	d.logsMu.Lock()
	delete(d.logs, runID)
	d.logsMu.Unlock()

	d.subsMu.Lock()
	delete(d.subs, runID)
	d.subsMu.Unlock()
}
