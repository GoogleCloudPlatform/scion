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
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

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

		// Set up response channel. Reject if a waiter already exists (concurrent
		// request to the same task).
		responseCh := make(chan *messages.StructuredMessage, 1)
		if !e.bridge.addWaiter(string(taskID), &waiter{
			ch:        responseCh,
			agentSlug: agentCtx.AgentSlug,
			projectID: agentCtx.ProjectID,
		}) {
			yield(nil, fmt.Errorf("concurrent request for task %s: %w", taskID, a2a.ErrInternalError))
			return
		}
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

// SSEWriteDeadlineMiddleware wraps an http.Handler to clear the write deadline
// for SSE (text/event-stream) responses, allowing long-lived streaming
// connections while keeping WriteTimeout enabled for non-streaming endpoints.
func SSEWriteDeadlineMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&sseDeadlineWriter{ResponseWriter: w}, r)
	})
}

// sseWriteDeadline is the rolling per-write deadline for SSE connections.
// Each write resets the deadline, so an active stream stays alive while
// a stalled one is reaped after this duration.
const sseWriteDeadline = 60 * time.Second

// sseDeadlineWriter intercepts WriteHeader and Write to apply a rolling
// per-write deadline for SSE streams (Content-Type: text/event-stream).
// Non-SSE responses are passed through unchanged.
type sseDeadlineWriter struct {
	http.ResponseWriter
	isSSE   bool
	checked bool
}

// detectSSE checks the Content-Type header once and caches the result.
func (s *sseDeadlineWriter) detectSSE() {
	if !s.checked {
		ct := s.ResponseWriter.Header().Get("Content-Type")
		s.isSSE = strings.HasPrefix(ct, "text/event-stream")
		s.checked = true
	}
}

// extendDeadline sets a rolling write deadline for SSE connections.
func (s *sseDeadlineWriter) extendDeadline() {
	s.detectSSE()
	if s.isSSE {
		rc := http.NewResponseController(s.ResponseWriter)
		_ = rc.SetWriteDeadline(time.Now().Add(sseWriteDeadline))
	}
}

func (s *sseDeadlineWriter) WriteHeader(code int) {
	s.extendDeadline()
	s.ResponseWriter.WriteHeader(code)
}

func (s *sseDeadlineWriter) Write(b []byte) (int, error) {
	s.extendDeadline()
	return s.ResponseWriter.Write(b)
}

func (s *sseDeadlineWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *sseDeadlineWriter) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

// MaxBytesReaderMiddleware wraps an http.Handler to limit the request body size,
// preventing memory exhaustion from oversized payloads. This mirrors the
// MaxBytesReader applied in the JSON-RPC path (server.go handleJSONRPC).
func MaxBytesReaderMiddleware(maxBytes int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		next.ServeHTTP(w, r)
	})
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

// grpcHeaderLookup adapts gRPC incoming metadata to a headerLookup. gRPC
// metadata keys are always lowercase, so canonical HTTP names are lowered here.
func grpcHeaderLookup(ctx context.Context) (headerLookup, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, false
	}
	return func(name string) string {
		if vals := md.Get(strings.ToLower(name)); len(vals) > 0 {
			return vals[0]
		}
		return ""
	}, true
}

// authenticateGRPC runs the shared authenticator against gRPC metadata and maps
// failures onto gRPC status codes.
func (s *Server) authenticateGRPC(ctx context.Context) (context.Context, error) {
	if s.config.Auth.Scheme == "none" || s.config.Bridge.GRPCInsecure {
		return ctx, nil
	}
	lookup, ok := grpcHeaderLookup(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing metadata")
	}
	authCtx, authErr := s.authenticate(ctx, lookup)
	if authErr != nil {
		if authErr.internal {
			return nil, status.Error(codes.Internal, authErr.msg)
		}
		return nil, status.Error(codes.Unauthenticated, authErr.msg)
	}
	return authCtx, nil
}

// AuthUnaryInterceptor returns a gRPC unary interceptor that validates caller
// credentials using the same schemes as the HTTP transports (including the
// per-user hubUAT/hubJWT schemes, which inject a CallerIdentity into the
// context). Auth is skipped when auth.scheme is "none" or bridge.grpc_insecure
// is set.
func (s *Server) AuthUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		authCtx, err := s.authenticateGRPC(ctx)
		if err != nil {
			return nil, err
		}
		return handler(authCtx, req)
	}
}

// AuthStreamInterceptor returns a gRPC stream interceptor with the same
// semantics as AuthUnaryInterceptor.
func (s *Server) AuthStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		authCtx, err := s.authenticateGRPC(ss.Context())
		if err != nil {
			return err
		}
		return handler(srv, &ctxServerStream{ServerStream: ss, ctx: authCtx})
	}
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
		wrapped := &ctxServerStream{ServerStream: ss, ctx: WithRouteInfo(ss.Context(), route)}
		return handler(srv, wrapped)
	}
}

// ctxServerStream wraps a grpc.ServerStream to override its Context, letting
// interceptors inject values (auth identity, route info) that downstream
// handlers observe.
type ctxServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *ctxServerStream) Context() context.Context {
	return s.ctx
}
