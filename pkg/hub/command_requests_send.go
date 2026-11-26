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
	"errors"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// commandRequestPromptSend holds a validated command request send on its way
// through the outbound-message handler.
type commandRequestPromptSend struct {
	request      *messages.CommandRequest
	update       bool
	intro        string
	approverID   string
	approverHint string
	result       *store.CommandRequestPromptResult
}

// commandRequestStoreError wraps a store-level refusal of CreateCommandRequestPrompt
// so the outbound-message handler maps it to a 4xx status instead of 500.
type commandRequestStoreError struct{ err error }

func (e *commandRequestStoreError) Error() string { return e.err.Error() }
func (e *commandRequestStoreError) Unwrap() error { return e.err }

// prepareCommandRequestSend validates a send carrying command_request or
// command_request_update before routing (§4.5): experiment gate, mutual
// exclusion with decisions/secret_requests/attachments/wake, DM-only target
// in P1, and script/workdir/timeout normalization.
// Returns (nil, true) for an ordinary send, or (nil, false) after writing the error.
func (s *Server) prepareCommandRequestSend(w http.ResponseWriter, req *OutboundMessageRequest) (*commandRequestPromptSend, bool) {
	if req.CommandRequest == nil && !req.CommandRequestUpdate {
		return nil, true
	}
	if !s.experimentEnabled(experiments.CommandRequests) {
		writeError(w, http.StatusBadRequest, ErrCodeExperimentDisabled,
			"experiment_disabled: "+experiments.CommandRequests+" is off; command requests are not available on this hub",
			map[string]interface{}{"experiment": experiments.CommandRequests})
		return nil, false
	}
	if req.Decisions != nil || req.DecisionsUpdate {
		ValidationError(w, "cannot combine command_request with decisions in the same message",
			map[string]interface{}{"field": "command_request"})
		return nil, false
	}
	if req.SecretRequests != nil || req.SecretRequestsUpdate {
		ValidationError(w, "cannot combine command_request with secret_requests in the same message",
			map[string]interface{}{"field": "command_request"})
		return nil, false
	}
	if req.CommandRequest == nil {
		ValidationError(w, "command_request_update requires command_request",
			map[string]interface{}{"field": "command_request_update"})
		return nil, false
	}
	recipient := strings.TrimSpace(req.Recipient)
	if strings.HasPrefix(recipient, "agent:") || isBroadcastRecipient(recipient) {
		ValidationError(w, "command requests can be sent only as a direct message to a user, not to an agent or a broadcast",
			map[string]interface{}{"field": "recipient"})
		return nil, false
	}
	if strings.TrimSpace(req.ConversationID) != "" || strings.TrimSpace(req.ConversationRef) != "" || strings.TrimSpace(req.ThreadID) != "" && !strings.HasPrefix(strings.TrimSpace(req.ThreadID), "dm:") {
		ValidationError(w, "command requests are only supported in direct messages to a user",
			map[string]interface{}{"field": "recipient"})
		return nil, false
	}
	if len(req.Attachments) > 0 || len(req.AttachmentIDs) > 0 {
		ValidationError(w, "a message with command_request cannot carry attachments",
			map[string]interface{}{"field": "attachments"})
		return nil, false
	}
	if req.Wake {
		ValidationError(w, "a message with command_request cannot set wake",
			map[string]interface{}{"field": "wake"})
		return nil, false
	}
	normalized, err := messages.NormalizeCommandRequest("command_request", req.CommandRequest)
	if err != nil {
		writeCommandRequestValidationErrors(w, err)
		return nil, false
	}
	return &commandRequestPromptSend{
		request: normalized,
		update:  req.CommandRequestUpdate,
	}, true
}

func writeCommandRequestValidationErrors(w http.ResponseWriter, err error) {
	var errs messages.DecisionValidationErrors
	if errors.As(err, &errs) {
		ValidationError(w, "invalid command_request: "+errs.Error(), map[string]interface{}{"errors": errs})
		return
	}
	ValidationError(w, "invalid command_request: "+err.Error(), nil)
}

// writeCommandRequestStoreError answers a CreateCommandRequestPrompt failure.
// Returns false if err is not a command-request refusal.
func (s *Server) writeCommandRequestStoreError(w http.ResponseWriter, ctx context.Context, agent *store.Agent, err error) bool {
	var pendingErr *store.CommandRequestKeyPendingError
	switch {
	case errors.As(err, &pendingErr):
		s.emitCommandRequestRefused(ctx, &MessageActionEvent{
			RowID:     pendingErr.RowID,
			MessageID: pendingErr.MessageID,
			ProjectID: agentProjectID(agent),
			AgentID:   agentIDOrEmpty(agent),
			AgentName: agentDisplayName(agent),
			Key:       pendingErr.Key,
			Version:   pendingErr.Version,
			ActorKind: MessageActionActorAgent,
			ActorID:   agentIDOrEmpty(agent),
			Reason:    msgaction.ConflictCmdKeyPending,
		})
		writeError(w, http.StatusConflict, msgaction.ConflictCmdKeyPending, pendingErr.Error(), map[string]interface{}{
			"key":       pendingErr.Key,
			"version":   pendingErr.Version,
			"rowId":     pendingErr.RowID,
			"messageId": pendingErr.MessageID,
			"status":    pendingErr.Status,
		})
		return true
	case errors.Is(err, store.ErrInvalidInput):
		var storeErr *commandRequestStoreError
		if errors.As(err, &storeErr) {
			ValidationError(w, err.Error(), nil)
			return true
		}
	}
	return false
}

func (s *Server) storeCommandRequestPrompt(ctx context.Context, cr *commandRequestPromptSend, agent *store.Agent, storeMsg *store.Message, structuredMsg *messages.StructuredMessage) error {
	projectName := ""
	if storeMsg.ProjectID != "" {
		if proj, err := s.store.GetProject(ctx, storeMsg.ProjectID); err == nil && proj != nil {
			projectName = proj.Name
			if projectName == "" {
				projectName = proj.Slug
			}
		}
	}
	res, err := s.store.CreateCommandRequestPrompt(ctx, &store.CommandRequestPromptInput{
		Message:      storeMsg,
		Intro:        cr.intro,
		ProjectName:  projectName,
		ApproverHint: cr.approverHint,
		AgentID:      agent.ID,
		AgentName:    agentDisplayName(agent),
		ApproverID:   cr.approverID,
		Request:      cr.request,
		Update:       cr.update,
	})
	if err != nil {
		return &commandRequestStoreError{err: err}
	}
	cr.result = res
	structuredMsg.Msg = storeMsg.Msg
	structuredMsg.Type = storeMsg.Type
	return nil
}

func (s *Server) publishReplacedCommandRequest(ctx context.Context, res *store.CommandRequestPromptResult) {
	if res == nil || res.Replaced == nil || res.Replaced.ID == "" {
		return
	}
	old := res.Replaced
	chatKey := old.ChatKey
	if chatKey == "" && old.MessageID != "" {
		if prompt, err := s.store.GetMessage(ctx, old.MessageID); err == nil && prompt != nil {
			chatKey = prompt.ThreadID
		}
	}
	if chatKey == "" {
		return
	}
	view := commandRequestView(old, res.Request, nil)
	s.publishActionUpdated(msgaction.KindCommandRequest, old.ProjectID, old.MessageID, chatKey, view)
}

func (s *Server) emitCommandRequestSendAudit(ctx context.Context, res *store.CommandRequestPromptResult) {
	if res == nil || res.Request == nil {
		return
	}
	row := res.Request
	replacesID := ""
	if res.Replaced != nil {
		replacesID = res.Replaced.ID
	}
	details := map[string]string{
		"command_sha256":   row.CommandSHA256,
		"approver_user_id": row.ApproverUserID,
		"workdir_kind":     messages.CommandWorkdirKind(row.Workdir),
	}
	s.emitMessageActionEvent(ctx, &MessageActionEvent{
		Kind:          msgaction.KindCommandRequest,
		EventType:     MessageActionEventRequested,
		RowID:         row.ID,
		MessageID:     row.MessageID,
		ProjectID:     row.ProjectID,
		AgentID:       row.AgentID,
		AgentName:     row.AgentName,
		Key:           row.Key,
		Version:       row.Version,
		ReplacesRowID: replacesID,
		ActorKind:     MessageActionActorAgent,
		ActorID:       row.AgentID,
		Details:       details,
		Timestamp:     row.CreatedAt,
	})
	if res.Replaced != nil && res.Replaced.Status == store.CommandRequestStatusReplaced {
		old := res.Replaced
		s.emitMessageActionEvent(ctx, &MessageActionEvent{
			Kind:          msgaction.KindCommandRequest,
			EventType:     MessageActionEventReplaced,
			RowID:         old.ID,
			MessageID:     old.MessageID,
			ProjectID:     old.ProjectID,
			AgentID:       old.AgentID,
			AgentName:     old.AgentName,
			Key:           old.Key,
			Version:       old.Version,
			ReplacesRowID: row.ID,
			ActorKind:     MessageActionActorAgent,
			ActorID:       old.AgentID,
			Details: map[string]string{
				"command_sha256":   old.CommandSHA256,
				"approver_user_id": old.ApproverUserID,
			},
			Timestamp: row.CreatedAt,
		})
	}
}

func commandRequestSendResult(res *store.CommandRequestPromptResult) *messages.CommandRequestSendResult {
	if res == nil || res.Request == nil {
		return nil
	}
	out := &messages.CommandRequestSendResult{
		Key:           res.Request.Key,
		Version:       res.Request.Version,
		RowID:         res.Request.ID,
		CommandSHA256: res.Request.CommandSHA256,
	}
	if res.Replaced != nil {
		out.Replaced = &messages.CommandRequestReplaced{
			Version: res.Replaced.Version,
			Status:  res.Replaced.Status,
		}
	}
	return out
}
