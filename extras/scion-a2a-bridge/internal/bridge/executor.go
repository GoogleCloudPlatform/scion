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
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"google.golang.org/grpc"

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
//  3. Waits for the agent response via the broker
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
// to a Scion agent and yields events as the agent responds.
func (e *ScionExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if execCtx == nil {
			yield(nil, fmt.Errorf("executor context is nil: %w", a2a.ErrInternalError))
			return
		}
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
		// TODO(multi-turn): Pass execCtx.ContextID here to reuse existing
		// Scion contexts for multi-turn conversations. Currently always creates
		// a new context, breaking agents that use input-required → completed flows.
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

		// Register active task for broker correlation.
		aKey := agentKey(agentCtx.ProjectID, agentCtx.AgentSlug)
		e.bridge.registerActiveTask(string(taskID), aKey)
		defer e.bridge.unregisterActiveTask(string(taskID), aKey)

		// Set up response channel.
		responseCh := make(chan *messages.StructuredMessage, 1)
		e.bridge.addWaiter(string(taskID), &waiter{
			ch:        responseCh,
			agentSlug: agentCtx.AgentSlug,
			projectID: agentCtx.ProjectID,
		})
		defer e.bridge.removeWaiter(string(taskID))

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

		// Wait for agent response.
		timeout := e.bridge.config.Timeouts.SendMessage
		if timeout == 0 {
			timeout = 120 * time.Second
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()

		select {
		case response, ok := <-responseCh:
			if !ok || response == nil {
				failMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Agent response channel closed unexpectedly"))
				yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, failMsg), nil)

				if e.bridge.metrics != nil {
					e.bridge.metrics.TasksCompleted.WithLabelValues("failed").Inc()
				}
				return
			}

			agentMsg, _ := TranslateScionToA2AParts(response)

			// Emit completed status with agent message. Content is delivered
			// in the status message only — emitting it again as an artifact
			// would duplicate it and confuse A2A clients that aggregate
			// artifacts separately from status messages.
			statusMsg := a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, agentMsg.Parts...)
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, statusMsg), nil)

			if e.bridge.metrics != nil {
				e.bridge.metrics.TasksCompleted.WithLabelValues("completed").Inc()
			}

		case <-timer.C:
			failMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(fmt.Sprintf("Timeout waiting for agent response after %v", timeout)))
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, failMsg), nil)

			if e.bridge.metrics != nil {
				e.bridge.metrics.TasksCompleted.WithLabelValues("failed").Inc()
			}

		case <-ctx.Done():
			failMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Request cancelled"))
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, failMsg), nil)

			if e.bridge.metrics != nil {
				e.bridge.metrics.TasksCompleted.WithLabelValues("failed").Inc()
			}
		}
	}
}

// Cancel implements a2asrv.AgentExecutor. It sends an interrupt to the Scion
// agent and emits a canceled status.
func (e *ScionExecutor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		taskID := execCtx.TaskID

		// Look up the stored task to find the agent and send an interrupt.
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

// RouteInfoMiddleware wraps an http.Handler to inject a fixed RouteInfo into the
// request context. Used for transports (REST) that don't have per-request
// project/agent routing.
func RouteInfoMiddleware(route RouteInfo, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := WithRouteInfo(r.Context(), route)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RouteInfoUnaryInterceptor returns a gRPC unary server interceptor that injects
// a fixed RouteInfo into the request context.
func RouteInfoUnaryInterceptor(route RouteInfo) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		return handler(WithRouteInfo(ctx, route), req)
	}
}

// RouteInfoStreamInterceptor returns a gRPC stream server interceptor that
// injects a fixed RouteInfo into the stream context.
func RouteInfoStreamInterceptor(route RouteInfo) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		wrapped := &routeInfoServerStream{ServerStream: ss, ctx: WithRouteInfo(ss.Context(), route)}
		return handler(srv, wrapped)
	}
}

// routeInfoServerStream wraps a grpc.ServerStream to override its Context.
type routeInfoServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *routeInfoServerStream) Context() context.Context {
	return s.ctx
}
