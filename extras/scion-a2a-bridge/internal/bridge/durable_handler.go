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
	"fmt"
	"iter"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
)

// DurableRequestHandler wraps the SDK's RequestHandler and intercepts
// SubscribeToTask to provide durable, ownership-enforcing cross-replica
// subscription (Constraint 3). All other methods delegate to the inner handler.
type DurableRequestHandler struct {
	inner      a2asrv.RequestHandler
	sdkStore   *PostgresTaskStore
	eventStore *state.PostgresStore
	notifier   *Notifier
}

// Compile-time check.
var _ a2asrv.RequestHandler = (*DurableRequestHandler)(nil)

// NewDurableRequestHandler creates a DurableRequestHandler wrapping the SDK handler.
func NewDurableRequestHandler(inner a2asrv.RequestHandler, sdkStore *PostgresTaskStore, eventStore *state.PostgresStore, notifier *Notifier) *DurableRequestHandler {
	return &DurableRequestHandler{
		inner:      inner,
		sdkStore:   sdkStore,
		eventStore: eventStore,
		notifier:   notifier,
	}
}

// SubscribeToTask implements a durable, ownership-enforcing subscribe that
// bypasses localManager.Resubscribe. It reads the task snapshot + cursor
// from a2a_sdk_tasks, yields the snapshot, then streams new events from
// a2a_task_events with id > cursor (Constraint 3, no old-replay).
func (h *DurableRequestHandler) SubscribeToTask(
	ctx context.Context, req *a2a.SubscribeToTaskRequest,
) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		// 1. Derive owner_key from authenticated context.
		ownerKey, ok, err := buildOwnerKey(ctx)
		if err != nil || !ok {
			yield(nil, fmt.Errorf("%w: authentication required", a2a.ErrTaskNotFound))
			return
		}

		// 2. Transactional snapshot + cursor (ownership enforced).
		storedTask, cursor, err := h.sdkStore.GetOwnedTaskSnapshotAndCursor(
			ctx, string(req.ID), ownerKey)
		if err != nil {
			yield(nil, a2a.ErrTaskNotFound)
			return
		}

		// 3. Yield task snapshot (contains all history up to cursor).
		// Strip _bridgeEventID from metadata before yielding to prevent
		// internal metadata from leaking to the client.
		task := storedTask.Task
		stripBridgeEventID(task)
		if !yield(task, nil) {
			return
		}
		if task.Status.State.Terminal() {
			return
		}

		// 4. Stream only NEW events (id > cursor), no old-replay.
		var notifyCh <-chan struct{}
		if h.notifier != nil {
			var cleanup func()
			notifyCh, cleanup = h.notifier.Register(string(req.ID))
			defer cleanup()
		}
		pollInterval := 200 * time.Millisecond

		for {
			events, readErr := h.eventStore.ReadTaskEvents(
				ctx, string(req.ID), cursor, 50)
			if readErr != nil {
				yield(nil, fmt.Errorf("event read failed: %w", readErr))
				return
			}
			for _, ev := range events {
				cursor = ev.ID
				sdkEvent, convErr := taskEventToSDKResubscribeEvent(req.ID, &ev)
				if convErr != nil {
					continue
				}
				if !yield(sdkEvent, nil) {
					return
				}
				if ev.Final {
					return
				}
			}

			select {
			case <-notifyCh:
			case <-time.After(pollInterval):
			case <-ctx.Done():
				return
			}
		}
	}
}

// stripBridgeEventID removes internal _bridgeEventID from task metadata
// to prevent leaking internal cursor data in user-visible output.
func stripBridgeEventID(task *a2a.Task) {
	if task == nil {
		return
	}
	deleteBridgeMeta(task)
	for _, msg := range task.History {
		if msg == nil {
			continue
		}
		deleteBridgeMeta(msg)
	}
}

// deleteBridgeMeta removes _bridgeEventID from any type with Meta()/SetMeta().
func deleteBridgeMeta(v any) {
	if mc, ok := v.(interface{ Meta() map[string]any }); ok {
		if m := mc.Meta(); m != nil {
			delete(m, bridgeEventIDKey)
		}
	}
}

// stripBridgeEventIDFromEvent removes _bridgeEventID from an individual
// event's metadata. For *a2a.Task events, also strips from the task's
// history messages. For other event types (TaskStatusUpdateEvent, etc.),
// strips from the event's own metadata and any embedded message.
func stripBridgeEventIDFromEvent(ev a2a.Event) {
	if ev == nil {
		return
	}
	switch e := ev.(type) {
	case *a2a.Task:
		stripBridgeEventID(e)
	default:
		// Strip from the event's own metadata.
		deleteBridgeMeta(e)
	}
}

// taskEventToSDKResubscribeEvent converts a stored TaskEvent to an SDK event
// for the durable subscribe stream. Unlike taskEventToSDKEvent (which uses
// an ExecutorContext), this creates standalone events from the raw event data.
func taskEventToSDKResubscribeEvent(taskID a2a.TaskID, ev *state.TaskEvent) (a2a.Event, error) {
	switch ev.Kind {
	case "message":
		var su TaskStatusUpdate
		if err := json.Unmarshal(ev.Payload, &su); err != nil {
			return nil, fmt.Errorf("unmarshal message event: %w", err)
		}
		var sdkParts []*a2a.Part
		if su.Status.Message != nil {
			for _, p := range su.Status.Message.Parts {
				if p.Text != "" {
					sdkParts = append(sdkParts, a2a.NewTextPart(p.Text))
				}
				if p.URL != "" {
					sdkParts = append(sdkParts, &a2a.Part{Content: a2a.URL(p.URL)})
				}
			}
		}
		if len(sdkParts) == 0 {
			sdkParts = append(sdkParts, a2a.NewTextPart("[empty response]"))
		}
		msg := a2a.NewMessage(a2a.MessageRoleAgent, sdkParts...)
		return &a2a.TaskStatusUpdateEvent{
			TaskID: taskID,
			Status: a2a.TaskStatus{
				State:   a2a.TaskStateCompleted,
				Message: msg,
			},
		}, nil
	case "status":
		var su TaskStatusUpdate
		if err := json.Unmarshal(ev.Payload, &su); err != nil {
			return nil, fmt.Errorf("unmarshal status event: %w", err)
		}
		sdkState := mapBridgeStateToSDK(su.Status.State)
		return &a2a.TaskStatusUpdateEvent{
			TaskID: taskID,
			Status: a2a.TaskStatus{State: sdkState},
		}, nil
	case "artifact":
		var au TaskArtifactUpdate
		if err := json.Unmarshal(ev.Payload, &au); err != nil {
			return nil, fmt.Errorf("unmarshal artifact event: %w", err)
		}
		var artParts []*a2a.Part
		for _, p := range au.Artifact.Parts {
			if p.Text != "" {
				artParts = append(artParts, a2a.NewTextPart(p.Text))
			}
			if p.URL != "" {
				artParts = append(artParts, &a2a.Part{Content: a2a.URL(p.URL)})
			}
		}
		if len(artParts) == 0 {
			return &a2a.TaskStatusUpdateEvent{
				TaskID: taskID,
				Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
			}, nil
		}
		return &a2a.TaskArtifactUpdateEvent{
			TaskID: taskID,
			Artifact: &a2a.Artifact{
				Parts: artParts,
			},
		}, nil
	default:
		return nil, fmt.Errorf("unknown event kind: %s", ev.Kind)
	}
}

// --- Strip _bridgeEventID from all user-visible paths ---
//
// Internal _bridgeEventID metadata is used for cursor tracking between
// processAndAppendEvent and PostgresTaskStore.Update. It must never appear
// in any user-visible response: GetTask, ListTasks, CancelTask, SendMessage,
// SendStreamingMessage, or SubscribeToTask (already handled above).

func (h *DurableRequestHandler) GetTask(ctx context.Context, req *a2a.GetTaskRequest) (*a2a.Task, error) {
	task, err := h.inner.GetTask(ctx, req)
	if err != nil {
		return nil, err
	}
	stripBridgeEventID(task)
	return task, nil
}

func (h *DurableRequestHandler) ListTasks(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	resp, err := h.inner.ListTasks(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp != nil {
		for _, task := range resp.Tasks {
			stripBridgeEventID(task)
		}
	}
	return resp, nil
}

func (h *DurableRequestHandler) CancelTask(ctx context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error) {
	task, err := h.inner.CancelTask(ctx, req)
	if err != nil {
		return nil, err
	}
	stripBridgeEventID(task)
	return task, nil
}

func (h *DurableRequestHandler) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	result, err := h.inner.SendMessage(ctx, req)
	if err != nil {
		return nil, err
	}
	stripBridgeEventIDFromEvent(result)
	return result, nil
}

func (h *DurableRequestHandler) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		for ev, err := range h.inner.SendStreamingMessage(ctx, req) {
			if ev != nil {
				stripBridgeEventIDFromEvent(ev)
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

func (h *DurableRequestHandler) GetTaskPushConfig(ctx context.Context, req *a2a.GetTaskPushConfigRequest) (*a2a.PushConfig, error) {
	return h.inner.GetTaskPushConfig(ctx, req)
}

func (h *DurableRequestHandler) ListTaskPushConfigs(ctx context.Context, req *a2a.ListTaskPushConfigRequest) (*a2a.ListTaskPushConfigResponse, error) {
	return h.inner.ListTaskPushConfigs(ctx, req)
}

func (h *DurableRequestHandler) CreateTaskPushConfig(ctx context.Context, req *a2a.PushConfig) (*a2a.PushConfig, error) {
	return h.inner.CreateTaskPushConfig(ctx, req)
}

func (h *DurableRequestHandler) DeleteTaskPushConfig(ctx context.Context, req *a2a.DeleteTaskPushConfigRequest) error {
	return h.inner.DeleteTaskPushConfig(ctx, req)
}

func (h *DurableRequestHandler) GetExtendedAgentCard(ctx context.Context, req *a2a.GetExtendedAgentCardRequest) (*a2a.AgentCard, error) {
	return h.inner.GetExtendedAgentCard(ctx, req)
}
