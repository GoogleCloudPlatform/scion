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
	"iter"
	"log/slog"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// routeKey is a context key for passing project/agent routing info to the executor.
type routeKey struct{}

// RouteInfo carries the project and agent slugs extracted from the HTTP path
// so the executor knows which Scion agent to route to.
type RouteInfo struct {
	ProjectSlug string
	AgentSlug   string
}

// WithRouteInfo attaches routing metadata to a context.
func WithRouteInfo(ctx context.Context, info RouteInfo) context.Context {
	return context.WithValue(ctx, routeKey{}, info)
}

// RouteInfoFrom extracts routing metadata from a context.
func RouteInfoFrom(ctx context.Context) (RouteInfo, bool) {
	info, ok := ctx.Value(routeKey{}).(RouteInfo)
	return info, ok
}

// ScionExecutor implements a2asrv.AgentExecutor, bridging the SDK's event model
// to the Scion Hub message routing. Each Execute call:
//  1. Translates the SDK message to a Scion StructuredMessage
//  2. Sends it to the target agent via Hub
//  3. Polls the event log for the agent's response events
//  4. Translates the response back to SDK events
type ScionExecutor struct {
	bridge *Bridge
	log    *slog.Logger
}

var _ a2asrv.AgentExecutor = (*ScionExecutor)(nil)

// NewScionExecutor creates a new executor that routes A2A requests to Scion agents.
func NewScionExecutor(bridge *Bridge, log *slog.Logger) *ScionExecutor {
	return &ScionExecutor{bridge: bridge, log: log}
}

// Execute implements a2asrv.AgentExecutor. It routes the incoming A2A message
// to a Scion agent and yields events as the agent responds via the event log.
func (e *ScionExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		route, ok := RouteInfoFrom(ctx)
		if !ok {
			yield(nil, fmt.Errorf("missing route info in context: %w", a2a.ErrInternalError))
			return
		}

		taskID := execCtx.TaskID

		if e.bridge.hubClient == nil {
			yield(nil, fmt.Errorf("hub client not configured: %w", a2a.ErrInternalError))
			return
		}

		// Resolve the Scion agent context (agent ID, project ID).
		agentCtx, err := e.bridge.resolveContext(ctx, route.ProjectSlug, route.AgentSlug, "")
		if err != nil {
			yield(nil, fmt.Errorf("resolve agent: %w", err))
			return
		}

		// Emit submitted task.
		if execCtx.StoredTask == nil {
			task := a2a.NewSubmittedTask(execCtx, execCtx.Message)
			if !yield(task, nil) {
				return
			}
		}

		// Per-user identity propagation: use the caller's credentials for Hub
		// writes, sender field, and broker subscriptions (mirrors Bridge.SendMessage).
		caller := callerIdentityFromContext(ctx) // nil in legacy mode
		var writeClient hubclient.Client = e.bridge.hubClient
		senderUser := e.bridge.config.Hub.User

		if caller != nil {
			senderUser = caller.Email
			var clientErr error
			writeClient, clientErr = e.bridge.callerHubClient(caller)
			if clientErr != nil {
				yield(nil, fmt.Errorf("creating per-user hub client: %w", clientErr))
				return
			}
		}

		// Translate A2A message parts to Scion format.
		scionMsg := TranslateA2APartsToScion(execCtx.Message.Parts)
		scionMsg.Sender = fmt.Sprintf("user:%s", senderUser)
		scionMsg.Recipient = fmt.Sprintf("agent:%s", agentCtx.AgentSlug)
		scionMsg.Metadata = map[string]string{"a2aTaskId": string(taskID)}

		// Request broker subscription for responses.
		if e.bridge.broker != nil {
			if caller != nil {
				e.bridge.subscribeAllUserTopics(agentCtx.ProjectID)
			} else {
				e.bridge.subscribeAdminUserTopics(agentCtx.ProjectID)
			}
		}

		// Register active task for broker correlation (local cache).
		aKey := agentKey(agentCtx.ProjectID, agentCtx.AgentSlug)
		e.bridge.registerActiveTask(string(taskID), aKey)
		defer e.bridge.unregisterActiveTask(string(taskID), aKey)

		// Send to Hub using the per-user or admin client.
		if _, err := writeClient.Agents().SendStructuredMessage(ctx, agentCtx.AgentID, scionMsg, false, false, false); err != nil {
			e.log.Error("failed to send message to agent", "error", err, "task_id", taskID, "agent_id", agentCtx.AgentID)
			failMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Failed to route message to agent"))
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, failMsg), nil)
			return
		}

		// Emit working status.
		if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
			return
		}

		if e.bridge.metrics != nil {
			e.bridge.metrics.TasksCreated.WithLabelValues(agentCtx.ProjectID).Inc()
		}

		// Poll the event log for agent response events.
		timeout := e.bridge.config.Timeouts.SendMessage
		if timeout == 0 {
			timeout = 120 * time.Second
		}

		ev, err := e.bridge.waitForTaskEvent(ctx, string(taskID), timeout)
		if err != nil {
			var failMsg *a2a.Message
			if errors.Is(err, ErrTimeout) {
				failMsg = a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(fmt.Sprintf("Timeout waiting for agent response after %v", timeout)))
			} else {
				failMsg = a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Request cancelled"))
			}
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, failMsg), nil)
			// Metric increment is handled by the CAS winner in
			// processAndAppendEvent — not duplicated here.
			return
		}

		// Convert the event to SDK format.
		sdkEvent, sdkErr := taskEventToSDKEvent(execCtx, ev)
		if sdkErr != nil {
			e.log.Error("failed to convert event to SDK format", "error", sdkErr, "task_id", taskID)
			failMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Failed to process agent response"))
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, failMsg), nil)
			return
		}

		yield(sdkEvent, nil)
	}
}

// taskEventToSDKEvent converts a stored TaskEvent to an SDK a2a.Event.
func taskEventToSDKEvent(execCtx *a2asrv.ExecutorContext, ev *state.TaskEvent) (a2a.Event, error) {
	switch ev.Kind {
	case "message":
		var su TaskStatusUpdate
		if err := json.Unmarshal(ev.Payload, &su); err != nil {
			return nil, fmt.Errorf("unmarshal message event: %w", err)
		}
		// Build SDK parts from the message.
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
		statusMsg := a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, sdkParts...)
		return a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, statusMsg), nil
	case "status":
		var su TaskStatusUpdate
		if err := json.Unmarshal(ev.Payload, &su); err != nil {
			return nil, fmt.Errorf("unmarshal status event: %w", err)
		}
		sdkState := mapBridgeStateToSDK(su.Status.State)
		return a2a.NewStatusUpdateEvent(execCtx, sdkState, nil), nil
	case "artifact":
		// Return as completed with a text message about the artifact.
		return a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil
	default:
		return nil, fmt.Errorf("unknown event kind: %s", ev.Kind)
	}
}

// mapBridgeStateToSDK maps bridge task states to SDK task states.
func mapBridgeStateToSDK(state string) a2a.TaskState {
	switch state {
	case TaskStateSubmitted:
		return a2a.TaskStateSubmitted
	case TaskStateWorking:
		return a2a.TaskStateWorking
	case TaskStateCompleted:
		return a2a.TaskStateCompleted
	case TaskStateFailed:
		return a2a.TaskStateFailed
	case TaskStateCanceled:
		return a2a.TaskStateCanceled
	case TaskStateInputRequired:
		return a2a.TaskStateInputRequired
	case TaskStateRejected:
		return a2a.TaskStateRejected
	default:
		return a2a.TaskStateWorking
	}
}

// Cancel implements a2asrv.AgentExecutor. It sends an interrupt to the Scion
// agent and emits a canceled status.
func (e *ScionExecutor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		taskID := execCtx.TaskID

		// Look up the stored task to find the agent.
		if execCtx.StoredTask != nil && e.bridge.hubClient != nil {
			route, ok := RouteInfoFrom(ctx)
			if !ok {
				e.log.Error("cancel: missing route info in context", "task_id", taskID)
				yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
				return
			}

			// Per-user identity propagation for cancel interrupts.
			caller := callerIdentityFromContext(ctx)
			var cancelClient hubclient.Client = e.bridge.hubClient
			senderUser := e.bridge.config.Hub.User
			if caller != nil {
				senderUser = caller.Email
				if cc, err := e.bridge.callerHubClient(caller); err == nil {
					cancelClient = cc
				} else {
					e.log.Warn("cancel: failed to create per-user client, falling back to admin",
						"error", err, "task_id", taskID)
				}
			}

			if agent := e.bridge.lookupAgent(ctx, route.ProjectSlug, route.AgentSlug); agent != nil {
				interruptMsg := &messages.StructuredMessage{
					Version:   1,
					Timestamp: time.Now().UTC().Format(time.RFC3339),
					Sender:    fmt.Sprintf("user:%s", senderUser),
					Recipient: fmt.Sprintf("agent:%s", route.AgentSlug),
					Msg:       "Task cancelled by A2A client.",
					Type:      messages.TypeInstruction,
					Metadata:  map[string]string{"a2aTaskId": string(taskID)},
				}
				if _, err := cancelClient.Agents().SendStructuredMessage(ctx, agent.ID, interruptMsg, true, false, false); err != nil {
					e.log.Error("failed to send cancel interrupt", "error", err, "task_id", taskID)
				}
			}
		}

		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}
