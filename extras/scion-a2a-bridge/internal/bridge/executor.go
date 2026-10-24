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

		caller := callerIdentityFromContext(ctx) // nil in legacy mode
		federatedUser := caller != nil && caller.IsFederatedUser()

		// Resolve the Scion agent context (agent ID, project ID). A
		// federated user is answered with a failed task below: the uniform
		// refusal for an agent that is unknown or not exposed (the server
		// lets its request for an agent that is not exposed reach the
		// executor), the same answer as when the hub refuses the send, so
		// these cannot be told apart; a routing failure for any other error.
		var agentCtx *state.Context
		var launching *hubclient.Agent
		var resolveErr error
		if federatedUser && e.bridge.AuthorizeExposed(route.ProjectSlug, route.AgentSlug) != nil {
			resolveErr = fmt.Errorf("%w: %q", ErrAgentNotFound, route.AgentSlug)
		} else {
			agentCtx, launching, resolveErr = e.bridge.resolveContextLaunch(ctx, route.ProjectSlug, route.AgentSlug, "")
		}
		if resolveErr != nil && !federatedUser {
			yield(nil, fmt.Errorf("resolve agent: %w", resolveErr))
			return
		}

		// Constraint 2: Prepare barrier before yielding the initial task.
		// The barrier provides deterministic signaling between the consumer
		// (store.Create) and producer (ClaimExecution) goroutines.
		var barrier *CreateBarrier
		if bs := e.bridge.barrierStore; bs != nil {
			barrier = bs.PrepareBarrier(string(taskID))
			defer barrier.Cancel() // cleanup on every exit path
		}

		// Emit submitted task.
		if execCtx.StoredTask == nil {
			task := a2a.NewSubmittedTask(execCtx, execCtx.Message)
			if !yield(task, nil) {
				return // defer barrier.Cancel() runs
			}
		}

		if resolveErr != nil {
			// Federated user only (handled above for everyone else).
			failMsg := federatedUserRefusalMessage()
			if errors.Is(resolveErr, ErrAgentNotFound) {
				e.log.Debug("federated user: agent not found or not exposed", "error", resolveErr, "task_id", taskID)
			} else {
				// A hub or store failure is not a refusal: the caller may retry.
				e.log.Error("federated user: failed to resolve agent", "error", resolveErr, "task_id", taskID)
				failMsg = a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Failed to route message to agent"))
			}
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, failMsg), nil)
			return
		}

		// Per-user/agent identity propagation: use the caller's credentials for Hub
		// writes, sender field, and broker subscriptions (mirrors Bridge.SendMessage).
		var writeClient hubclient.Client = e.bridge.hubClient
		senderLabel := fmt.Sprintf("user:%s", e.bridge.config.Hub.User)

		if caller != nil {
			senderLabel = caller.SenderLabel()
			var clientErr error
			writeClient, clientErr = e.bridge.callerHubClient(caller)
			if clientErr != nil {
				yield(nil, fmt.Errorf("creating per-caller hub client: %w", clientErr))
				return
			}
		}

		// Translate A2A message parts to Scion format.
		scionMsg := TranslateA2APartsToScion(execCtx.Message.Parts)
		scionMsg.Sender = senderLabel
		scionMsg.Recipient = fmt.Sprintf("agent:%s", agentCtx.AgentSlug)
		if !federatedUser {
			// The hub accepts only a direct text message from a federated
			// user (no metadata); its reply is correlated through the
			// user's topic instead.
			scionMsg.Metadata = map[string]string{"a2aTaskId": string(taskID)}
		}

		// Request broker subscription for responses. User callers get
		// their replies on their own user topic: a federated user only on
		// its account's topic, other users through the project's user
		// topics.
		if e.bridge.broker != nil {
			switch {
			case federatedUser:
				e.bridge.subscribeUserTopic(agentCtx.ProjectID, caller.UserID)
			case caller != nil && !caller.IsAgent():
				e.bridge.subscribeAllUserTopics(agentCtx.ProjectID)
			default:
				e.bridge.subscribeAdminUserTopics(agentCtx.ProjectID)
			}
		}

		// A federated user gets the hub's decision on the message before
		// anything about a launch. The hub refuses a message to an agent
		// that is still launching, so the send cannot be asked yet; the
		// caller's read of its conversation with the agent
		// (recheckFederatedUserAccess) is admitted by the same hub check
		// as the send. A refusal ends the task at once with the uniform
		// refusal, the same answer as for a running agent, with no launch
		// step, launch outcome or wait; any other error is a routing
		// failure. Only a caller the hub allows waits for the launch.
		if federatedUser && hubclient.LaunchActive(launching) {
			if err := e.bridge.recheckFederatedUserAccess(ctx, caller, agentCtx.AgentID); err != nil {
				failMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Failed to route message to agent"))
				if isHubRefusal(err) {
					e.log.Debug("federated user: hub refused the message to a launching agent", "error", err, "task_id", taskID, "agent_id", agentCtx.AgentID)
					failMsg = federatedUserRefusalMessage()
				} else {
					e.log.Error("federated user: access check before the launch wait failed", "error", err, "task_id", taskID, "agent_id", agentCtx.AgentID)
				}
				yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, failMsg), nil)
				return
			}
		}

		// An agent still launching (an asynchronous create, or an adopted
		// agent another client is launching) is waited for before the
		// send. This runs before the execution lease is claimed, so no
		// lease is held during the wait. The task reports working with the
		// launch step, and fails without a send when the launch fails.
		if hubclient.LaunchActive(launching) {
			waitCtx, cancelWait := context.WithCancel(ctx)
			defer cancelWait()
			consumerGone := false
			report := launchStepReporter(func(text string) {
				if consumerGone {
					return
				}
				msg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(text))
				if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, msg), nil) {
					consumerGone = true
					cancelWait()
				}
			})
			report(launchStep(launching))
			waitErr := e.bridge.waitForAgentLaunch(waitCtx, launching, report)
			cancelWait()
			if consumerGone {
				return
			}
			if waitErr != nil {
				e.log.Warn("agent did not start; message not sent", "error", waitErr, "task_id", taskID, "agent_id", agentCtx.AgentID)
				failText := launchFailureText(waitErr)
				if federatedUser {
					failText = federatedUserLaunchFailureText(waitErr)
				}
				failMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(failText))
				yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, failMsg), nil)
				return
			}
		}

		// Register active task for broker correlation (local cache). It
		// records the caller, so that in plugin mode a federated user's
		// task takes replies only from that account's user topic, and an
		// agent caller's task none from another user's topic.
		aKey := agentKey(agentCtx.ProjectID, agentCtx.AgentSlug)
		e.bridge.registerActiveTaskFor(string(taskID), aKey, caller)
		defer e.bridge.unregisterActiveTask(string(taskID), aKey)

		// Claim execution lease before Hub send to prevent duplicate sends.
		// The lease tracks which replica is executing this task; if we crash,
		// the janitor reaps tasks with expired leases.
		//
		// CRIT-3: All claim errors fail closed. No Hub side effects without
		// a confirmed lease. "Degraded mode" bypass is forbidden.
		//
		// Constraint 2: For new task submissions, the barrier replaces the
		// retry loop. BarrierTaskStore.Await blocks until Create commits,
		// then a single ClaimExecution attempt succeeds.
		if e.bridge.sdkTaskStore != nil {
			ownerID := OwnerID()
			leaseTimeout := e.bridge.config.Timeouts.SendMessage
			if leaseTimeout == 0 {
				leaseTimeout = 120 * time.Second
			}

			// Wait for barrier if this is a new task (Create in flight).
			if barrier != nil && execCtx.StoredTask == nil {
				if barrierErr := barrier.Await(ctx); barrierErr != nil {
					e.log.Error("barrier await failed", "error", barrierErr, "task_id", taskID)
					failMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Task creation failed: "+barrierErr.Error()))
					yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, failMsg), nil)
					return
				}
			}

			// Single ClaimExecution attempt — row guaranteed to exist after barrier.
			claimed, claimErr := e.bridge.sdkTaskStore.ClaimExecution(ctx, string(taskID), ownerID, leaseTimeout)

			if claimErr != nil {
				// CRIT-3: Fail closed on any DB error. Never proceed to Hub send.
				e.log.Error("failed to claim execution lease", "error", claimErr, "task_id", taskID)
				failMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Failed to acquire execution lease"))
				yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, failMsg), nil)
				return
			}
			if !claimed {
				e.log.Warn("execution lease held by another replica", "task_id", taskID)
				failMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Task execution already in progress on another replica"))
				yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, failMsg), nil)
				return
			}
			// Release the lease on completion (normal or error).
			defer func() {
				if releaseErr := e.bridge.sdkTaskStore.ReleaseExecution(context.Background(), string(taskID), ownerID); releaseErr != nil {
					e.log.Error("failed to release execution lease", "error", releaseErr, "task_id", taskID)
				}
			}()
		}

		// Send to Hub using the per-user or admin client.
		if _, err := writeClient.Agents().SendStructuredMessage(ctx, agentCtx.AgentID, scionMsg, false, false, false); err != nil {
			e.log.Error("failed to send message to agent", "error", err, "task_id", taskID, "agent_id", agentCtx.AgentID)
			failMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Failed to route message to agent"))
			if federatedUser && isHubRefusal(err) {
				failMsg = federatedUserRefusalMessage()
			}
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

		ev, err := e.bridge.waitForTaskEvent(ctx, string(taskID), timeout, e.bridge.sdkTaskStore)
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

		// Revocation: a federated user's access is re-checked with the hub
		// before the reply is forwarded. A refusal ends the task with the
		// uniform refusal; the reply is not forwarded.
		if federatedUser {
			if failMsg := e.bridge.federatedUserReplyRefusal(ctx, caller, agentCtx.AgentID); failMsg != nil {
				e.log.Info("federated user reply not forwarded", "task_id", taskID, "agent_id", agentCtx.AgentID)
				yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, failMsg), nil)
				return
			}
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
// Constraint 4: Embeds the bridge event's autoincrement ID (_bridgeEventID)
// into the SDK event metadata so that PostgresTaskStore.Update can advance
// last_event_cursor to the exact event applied (not MAX(id)).
func taskEventToSDKEvent(execCtx *a2asrv.ExecutorContext, ev *state.TaskEvent) (a2a.Event, error) {
	var sdkEvent a2a.Event
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
		sdkEvent = a2a.NewStatusUpdateEvent(execCtx, responseSDKState(su.Status.State), statusMsg)
	case "status":
		var su TaskStatusUpdate
		if err := json.Unmarshal(ev.Payload, &su); err != nil {
			return nil, fmt.Errorf("unmarshal status event: %w", err)
		}
		sdkState := mapBridgeStateToSDK(su.Status.State)
		sdkEvent = a2a.NewStatusUpdateEvent(execCtx, sdkState, nil)
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
		artState := responseSDKState(au.State)
		if len(artParts) == 0 {
			sdkEvent = a2a.NewStatusUpdateEvent(execCtx, artState, nil)
		} else {
			artMsg := a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, artParts...)
			sdkEvent = a2a.NewStatusUpdateEvent(execCtx, artState, artMsg)
		}
	default:
		return nil, fmt.Errorf("unknown event kind: %s", ev.Kind)
	}

	// Carry the exact bridge event ID through the SDK pipeline.
	if mc, ok := sdkEvent.(interface{ SetMeta(string, any) }); ok {
		mc.SetMeta(bridgeEventIDKey, ev.ID)
	}
	return sdkEvent, nil
}

// responseSDKState returns the SDK state for a content response event. A
// response recorded in the input-required state is reported as
// input-required; any other response completes the task. Answering on the
// same task uses the normal follow-up path, which this mapping does not
// change.
func responseSDKState(state string) a2a.TaskState {
	if state == TaskStateInputRequired {
		return a2a.TaskStateInputRequired
	}
	return a2a.TaskStateCompleted
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

			// Per-user/agent identity propagation for cancel interrupts.
			caller := callerIdentityFromContext(ctx)
			// A federated user gets no interrupt: the hub accepts only a
			// text message from it, and the bridge never sends for it as
			// the admin.
			if caller.IsFederatedUser() {
				yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
				return
			}
			var cancelClient hubclient.Client = e.bridge.hubClient
			senderLabel := fmt.Sprintf("user:%s", e.bridge.config.Hub.User)
			if caller != nil {
				senderLabel = caller.SenderLabel()
				if cc, err := e.bridge.callerHubClient(caller); err == nil {
					cancelClient = cc
				} else {
					e.log.Warn("cancel: failed to create per-caller client, falling back to admin",
						"error", err, "task_id", taskID)
				}
			}

			if agent := e.bridge.lookupAgent(ctx, route.ProjectSlug, route.AgentSlug); agent != nil {
				interruptMsg := &messages.StructuredMessage{
					Version:   1,
					Timestamp: time.Now().UTC().Format(time.RFC3339),
					Sender:    senderLabel,
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
