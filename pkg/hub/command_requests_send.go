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
	"time"

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
	approverRaw  string
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
		ValidationError(w, "command requests can be sent only to a user or a conversation, not to an agent or a broadcast",
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
		request:     normalized,
		update:      req.CommandRequestUpdate,
		approverRaw: strings.TrimSpace(req.CommandRequest.Approver),
	}, true
}

// isEligibleCommandRequestApprover reports whether u is an active, non-expired,
// non-federated (non-visitor) human user who can hold an interactive session
// and approve a command request (design §4.5, §4.7, D6, R2).
func isEligibleCommandRequestApprover(u *store.User, now time.Time) bool {
	if u == nil || u.IsFederated() {
		return false
	}
	if u.Status != "" && u.Status != store.UserStatusActive {
		return false
	}
	if u.ExpiresAt != nil && !u.ExpiresAt.After(now) {
		return false
	}
	return true
}

// resolveCommandRequestApprover resolves and validates the single named human
// approver for a command request send (design §4.5, D6):
//   - DM to a user: approver is the DM user (must be an active non-visitor user);
//     if --approver is supplied it must match that user.
//   - Conversation / thread: --approver is required and must resolve to an
//     active, non-visitor human participant of the target conversation.
//     Email-shaped inputs (containing '@') match email only, never display name (O1).
func (s *Server) resolveCommandRequestApprover(ctx context.Context, w http.ResponseWriter, cr *commandRequestPromptSend, result *routingResult) bool {
	isConv := (result.ConvResult != nil && result.ConvResult.Kind != "direct") ||
		(result.ConversationID != "" && !strings.HasPrefix(result.Recipient, "user:"))

	raw := strings.TrimSpace(cr.approverRaw)
	if strings.HasPrefix(raw, "agent:") || strings.HasPrefix(raw, "@agent:") {
		ValidationError(w, "command request approver must be a human user, not an agent",
			map[string]interface{}{"field": "command_request.approver"})
		return false
	}

	now := time.Now().UTC()
	if !isConv {
		if result.RecipientID == "" || !strings.HasPrefix(result.Recipient, "user:") {
			ValidationError(w, "command requests can be sent only to a user or a conversation, not to an agent",
				map[string]interface{}{"field": "recipient"})
			return false
		}
		dmUser, uerr := s.store.GetUser(ctx, result.RecipientID)
		if uerr != nil && !errors.Is(uerr, store.ErrNotFound) {
			writeErrorFromErr(w, uerr, "")
			return false
		}
		if !isEligibleCommandRequestApprover(dmUser, now) {
			ValidationError(w, "command request approver must be an active non-visitor human user",
				map[string]interface{}{"field": "command_request.approver"})
			return false
		}
		dmEmail := strings.TrimPrefix(result.Recipient, "user:")
		if raw != "" {
			clean := stripApproverPrefix(raw)
			if !strings.EqualFold(clean, dmEmail) && clean != result.RecipientID {
				ValidationError(w, "in a direct message, --approver must match the DM recipient",
					map[string]interface{}{"field": "command_request.approver"})
				return false
			}
		}
		cr.approverID = result.RecipientID
		cr.approverHint = dmEmail
		return true
	}

	// Group conversation / thread target: --approver is required (D6), and a
	// caller-supplied dm: thread_id is refused so group requests can never carry
	// a DM-shaped chat key (R1, design §4.9).
	if strings.HasPrefix(strings.TrimSpace(result.ThreadID), "dm:") {
		ValidationError(w, "a dm: thread_id cannot be used with a non-DM conversation",
			map[string]interface{}{"field": "thread_id"})
		return false
	}
	if raw == "" {
		ValidationError(w, "command requests sent to a conversation or thread require --approver naming one human participant",
			map[string]interface{}{"field": "command_request.approver"})
		return false
	}
	convID := result.ConversationID
	if convID == "" && result.ConvResult != nil {
		convID = result.ConvResult.ConversationID
	}
	if convID == "" {
		ValidationError(w, "cannot resolve conversation participants for --approver",
			map[string]interface{}{"field": "command_request.approver"})
		return false
	}

	parts, err := s.store.ListParticipants(ctx, convID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return false
	}
	clean := stripApproverPrefix(raw)
	emailShaped := strings.Contains(clean, "@")
	var matched *store.User
	ambiguous := false
	for _, p := range parts {
		if p.PrincipalKind != "user" {
			continue
		}
		u, uerr := s.store.GetUser(ctx, p.PrincipalID)
		if uerr != nil || u == nil {
			continue
		}
		// Match email (including federated email for eligibility rejection) or
		// exact user ID only — never user-controlled display names (O1/L2).
		var isMatch bool
		if emailShaped {
			isMatch = strings.EqualFold(clean, u.Email) || (u.FederatedEmail != nil && strings.EqualFold(clean, *u.FederatedEmail))
		} else {
			isMatch = clean == u.ID
		}
		if isMatch {
			if matched != nil && matched.ID != u.ID {
				ambiguous = true
				break
			}
			matched = u
		}
	}
	if ambiguous {
		ValidationError(w, "approver matches more than one human participant; specify the approver's email address",
			map[string]interface{}{"field": "command_request.approver"})
		return false
	}
	if matched == nil {
		if !emailShaped {
			ValidationError(w, "approver must identify a human participant by email or user ID; specify the approver's email address",
				map[string]interface{}{"field": "command_request.approver"})
			return false
		}
		ValidationError(w, "approver must be a human participant of the target conversation",
			map[string]interface{}{"field": "command_request.approver"})
		return false
	}
	if !isEligibleCommandRequestApprover(matched, now) {
		ValidationError(w, "command request approver must be an active non-visitor human user",
			map[string]interface{}{"field": "command_request.approver"})
		return false
	}
	cr.approverID = matched.ID
	if matched.Email != "" {
		cr.approverHint = matched.Email
	} else {
		cr.approverHint = matched.DisplayName
	}
	return true
}

func stripApproverPrefix(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "user:")
	s = strings.TrimPrefix(s, "@")
	return strings.TrimSpace(s)
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
	var limitErr *store.CommandRequestLimitError
	switch {
	case errors.As(err, &limitErr):
		s.emitCommandRequestRefused(ctx, &MessageActionEvent{
			ProjectID: agentProjectID(agent),
			AgentID:   agentIDOrEmpty(agent),
			AgentName: agentDisplayName(agent),
			ActorKind: MessageActionActorAgent,
			ActorID:   agentIDOrEmpty(agent),
			Reason:    msgaction.ConflictCmdRequestLimit,
		})
		writeError(w, http.StatusTooManyRequests, msgaction.ConflictCmdRequestLimit, limitErr.Error(), map[string]interface{}{
			"limit":   limitErr.Limit,
			"current": limitErr.Current,
		})
		return true
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
	if res == nil {
		return
	}
	if res.Replaced != nil && res.Replaced.ID != "" {
		old := res.Replaced
		chatKey := old.ChatKey
		if chatKey == "" && old.MessageID != "" {
			if prompt, err := s.store.GetMessage(ctx, old.MessageID); err == nil && prompt != nil {
				chatKey = prompt.ThreadID
			}
		}
		if chatKey != "" {
			view := commandRequestSSEView(old, res.Request, nil)
			s.publishActionUpdated(msgaction.KindCommandRequest, old.ProjectID, old.MessageID, chatKey, view)
		}
	}
	s.publishCommandRequestInboxHintsOnce(ctx, []*store.CommandRequest{res.Request, res.Replaced})
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
