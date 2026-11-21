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
	"iter"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// errFederationTaskRouteUnavailable is returned when a bridge-local task
// operation is requested by a federation agent caller.
var errFederationTaskRouteUnavailable = a2a.NewError(
	a2a.ErrUnauthorized,
	"forbidden: task route is not available for federation agent callers",
)

type federationAgentMethodPolicy int

const (
	federationAgentDeny federationAgentMethodPolicy = iota
	federationAgentAllowNewTaskOnly
)

// federationAgentMethodAllowList classifies every method of
// a2asrv.RequestHandler for federation agent callers. Any method not listed
// defaults to federationAgentDeny (fail closed).
var federationAgentMethodAllowList = map[string]federationAgentMethodPolicy{
	"SendMessage":          federationAgentAllowNewTaskOnly,
	"SendStreamingMessage": federationAgentAllowNewTaskOnly,
	"GetTask":              federationAgentDeny,
	"ListTasks":            federationAgentDeny,
	"CancelTask":           federationAgentDeny,
	"SubscribeToTask":      federationAgentDeny,
	"GetTaskPushConfig":    federationAgentDeny,
	"ListTaskPushConfigs":  federationAgentDeny,
	"CreateTaskPushConfig": federationAgentDeny,
	"DeleteTaskPushConfig": federationAgentDeny,
	"GetExtendedAgentCard": federationAgentDeny,
}

// isFederationAgentCaller reports whether ctx carries a federation agent
// caller identity (not a federated user).
func isFederationAgentCaller(ctx context.Context) bool {
	caller := callerIdentityFromContext(ctx)
	return caller != nil && caller.TokenType == "federation" && caller.IsAgent()
}

func sendRequestTaskID(req *a2a.SendMessageRequest) a2a.TaskID {
	if req == nil || req.Message == nil {
		return ""
	}
	return req.Message.TaskID
}

// FederationTaskAuthHandler wraps an a2asrv.RequestHandler to enforce
// authentication rules for bridge task routes under the federation scheme.
// For federation agent callers, only new-task SendMessage and
// SendStreamingMessage (no taskId) are permitted through to the inner handler;
// every other method and any send/stream referencing an existing taskId fail
// closed with errFederationTaskRouteUnavailable.
type FederationTaskAuthHandler struct {
	inner a2asrv.RequestHandler
}

var _ a2asrv.RequestHandler = (*FederationTaskAuthHandler)(nil)

// NewFederationTaskAuthHandler returns a RequestHandler that guards
// bridge task routes under the federation scheme.
func NewFederationTaskAuthHandler(inner a2asrv.RequestHandler) *FederationTaskAuthHandler {
	return &FederationTaskAuthHandler{inner: inner}
}

func (h *FederationTaskAuthHandler) checkFederationAgentAccess(ctx context.Context, method string, taskID a2a.TaskID) error {
	if !isFederationAgentCaller(ctx) {
		return nil
	}
	if federationAgentMethodAllowList[method] == federationAgentAllowNewTaskOnly && taskID == "" {
		return nil
	}
	return errFederationTaskRouteUnavailable
}

func (h *FederationTaskAuthHandler) GetTask(ctx context.Context, req *a2a.GetTaskRequest) (*a2a.Task, error) {
	if err := h.checkFederationAgentAccess(ctx, "GetTask", ""); err != nil {
		return nil, err
	}
	return h.inner.GetTask(ctx, req)
}

func (h *FederationTaskAuthHandler) ListTasks(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	if err := h.checkFederationAgentAccess(ctx, "ListTasks", ""); err != nil {
		return nil, err
	}
	return h.inner.ListTasks(ctx, req)
}

func (h *FederationTaskAuthHandler) CancelTask(ctx context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error) {
	if err := h.checkFederationAgentAccess(ctx, "CancelTask", ""); err != nil {
		return nil, err
	}
	return h.inner.CancelTask(ctx, req)
}

func (h *FederationTaskAuthHandler) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	if err := h.checkFederationAgentAccess(ctx, "SendMessage", sendRequestTaskID(req)); err != nil {
		return nil, err
	}
	return h.inner.SendMessage(ctx, req)
}

func (h *FederationTaskAuthHandler) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	if err := h.checkFederationAgentAccess(ctx, "SendStreamingMessage", sendRequestTaskID(req)); err != nil {
		return func(yield func(a2a.Event, error) bool) {
			yield(nil, err)
		}
	}
	return h.inner.SendStreamingMessage(ctx, req)
}

func (h *FederationTaskAuthHandler) SubscribeToTask(ctx context.Context, req *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	if err := h.checkFederationAgentAccess(ctx, "SubscribeToTask", ""); err != nil {
		return func(yield func(a2a.Event, error) bool) {
			yield(nil, err)
		}
	}
	return h.inner.SubscribeToTask(ctx, req)
}

func (h *FederationTaskAuthHandler) GetTaskPushConfig(ctx context.Context, req *a2a.GetTaskPushConfigRequest) (*a2a.PushConfig, error) {
	if err := h.checkFederationAgentAccess(ctx, "GetTaskPushConfig", ""); err != nil {
		return nil, err
	}
	return h.inner.GetTaskPushConfig(ctx, req)
}

func (h *FederationTaskAuthHandler) ListTaskPushConfigs(ctx context.Context, req *a2a.ListTaskPushConfigRequest) (*a2a.ListTaskPushConfigResponse, error) {
	if err := h.checkFederationAgentAccess(ctx, "ListTaskPushConfigs", ""); err != nil {
		return nil, err
	}
	return h.inner.ListTaskPushConfigs(ctx, req)
}

func (h *FederationTaskAuthHandler) CreateTaskPushConfig(ctx context.Context, req *a2a.PushConfig) (*a2a.PushConfig, error) {
	if err := h.checkFederationAgentAccess(ctx, "CreateTaskPushConfig", ""); err != nil {
		return nil, err
	}
	return h.inner.CreateTaskPushConfig(ctx, req)
}

func (h *FederationTaskAuthHandler) DeleteTaskPushConfig(ctx context.Context, req *a2a.DeleteTaskPushConfigRequest) error {
	if err := h.checkFederationAgentAccess(ctx, "DeleteTaskPushConfig", ""); err != nil {
		return err
	}
	return h.inner.DeleteTaskPushConfig(ctx, req)
}

func (h *FederationTaskAuthHandler) GetExtendedAgentCard(ctx context.Context, req *a2a.GetExtendedAgentCardRequest) (*a2a.AgentCard, error) {
	if err := h.checkFederationAgentAccess(ctx, "GetExtendedAgentCard", ""); err != nil {
		return nil, err
	}
	return h.inner.GetExtendedAgentCard(ctx, req)
}
