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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Message modes in group conversations.
//
// The pairwise mode matrix (evaluateAgentMessageModes) decides whether one
// agent may message another. A group conversation has no single
// recipient, so agent posts into a group conversation, and agent
// participant rows in one, are decided here instead:
//
//   - none: the agent never posts to a group conversation and is never
//     made a participant of one.
//   - project, hub: the agent may post to any group conversation of its
//     project and is registered as a participant when it does.
//   - lineage, branch: the agent may post only to a group conversation it
//     is already an active participant of, and posting never makes it a
//     participant. It becomes a participant only when a human whom its
//     mode already lets message it (an ancestor, the project owner, or a
//     platform admin) adds it, or causes it to be added.
//
// Every path that posts an agent's message into a group conversation calls
// agentGroupPostDecision, and every path that writes an agent participant
// row into a group conversation calls groupAgentJoinAllowed. A future
// delivery path for agent posts (for example, delivery to a thread's
// default agent) must call both: agentGroupPostDecision for the sender
// before anything is written, then the pairwise authorizeAgentMessage for
// each delivered agent, and groupAgentJoinAllowed before registering any
// of them as a participant.

// groupPostDecision is the result of agentGroupPostDecision.
type groupPostDecision struct {
	// Allowed reports whether the agent may post to the conversation.
	Allowed bool
	// MayJoin reports whether posting may register the agent as a
	// participant of the conversation. It is false whenever Allowed is.
	MayJoin bool
	// Reason is a stable, short reason for logs and the denial body.
	Reason string
}

// Stable reasons for group conversation mode decisions.
const (
	groupModeReasonSenderNone        = "sender_message_mode_none"
	groupModeReasonNotParticipant    = "sender_not_group_participant"
	groupModeReasonUnknownMode       = "sender_message_mode_unknown"
	groupModeReasonProjectCell       = "sender_in_project_cell"
	groupModeReasonParticipant       = "sender_is_group_participant"
	groupModeReasonTargetNone        = "target_message_mode_none"
	groupModeReasonTargetNarrowAdder = "target_message_mode_requires_authorized_human"
)

// agentGroupPostDecision decides whether sender may post into the group
// conversation conversationID. The caller has already checked that the
// conversation is a group conversation of sender's project. A non-nil
// error means the participant lookup failed; callers fail closed.
func (s *Server) agentGroupPostDecision(ctx context.Context, sender *store.Agent, conversationID string) (groupPostDecision, error) {
	if sender == nil {
		return groupPostDecision{Reason: groupModeReasonUnknownMode}, nil
	}
	switch effectiveGroupMessageMode(sender.MessageMode) {
	case store.MessageModeNone:
		return groupPostDecision{Reason: groupModeReasonSenderNone}, nil
	case store.MessageModeProject, store.MessageModeHub:
		return groupPostDecision{Allowed: true, MayJoin: true, Reason: groupModeReasonProjectCell}, nil
	case store.MessageModeLineage, store.MessageModeBranch:
		member, err := isConversationParticipant(ctx, s.store, conversationID, "agent", sender.ID)
		if err != nil {
			return groupPostDecision{}, err
		}
		if !member {
			return groupPostDecision{Reason: groupModeReasonNotParticipant}, nil
		}
		return groupPostDecision{Allowed: true, Reason: groupModeReasonParticipant}, nil
	default:
		return groupPostDecision{Reason: groupModeReasonUnknownMode}, nil
	}
}

// groupAgentJoinAllowed decides whether adder may cause target to become a
// participant of a group conversation of target's project. adder is the
// principal whose action writes the row: the caller of the participant
// API, the creator of the conversation, or the sender of the message
// that would register target. It returns a stable reason for logs.
func (s *Server) groupAgentJoinAllowed(ctx context.Context, adder Identity, target *store.Agent) (bool, string) {
	if target == nil {
		return false, groupModeReasonUnknownMode
	}
	switch effectiveGroupMessageMode(target.MessageMode) {
	case store.MessageModeProject, store.MessageModeHub:
		return true, groupModeReasonProjectCell
	case store.MessageModeNone:
		return false, groupModeReasonTargetNone
	case store.MessageModeLineage, store.MessageModeBranch:
		if isNilIdentity(adder) {
			return false, groupModeReasonTargetNarrowAdder
		}
		switch adder.Type() {
		case "user", "dev", "federated_user":
		default:
			return false, groupModeReasonTargetNarrowAdder
		}
		if allowed, _, _ := s.authorizeAgentMessage(ctx, adder, target, false); !allowed {
			return false, groupModeReasonTargetNarrowAdder
		}
		return true, groupModeReasonParticipant
	default:
		return false, groupModeReasonUnknownMode
	}
}

// checkAgentGroupPost runs agentGroupPostDecision for an agent sender and
// writes the refusal when it is denied or the lookup fails. ok reports
// whether the post may proceed.
func (s *Server) checkAgentGroupPost(ctx context.Context, w http.ResponseWriter, sender *store.Agent, conversationID string) (groupPostDecision, bool) {
	decision, err := s.agentGroupPostDecision(ctx, sender, conversationID)
	if err != nil {
		s.messageLog.Error("group post: participant lookup failed",
			"conversation_id", conversationID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"conversation participant lookup failed", nil)
		return groupPostDecision{Reason: "lookup_failed"}, false
	}
	if !decision.Allowed {
		mode := ""
		senderID := ""
		if sender != nil {
			mode = sender.MessageMode
			senderID = sender.ID
		}
		s.messageLog.Info("group post refused by message mode",
			"conversation_id", conversationID,
			"sender_id", senderID,
			"mode", mode,
			"reason", decision.Reason)
		writeGroupModeDenied(w, decision.Reason, mode)
		return decision, false
	}
	return decision, true
}

// writeGroupModeDenied writes the refusal for a group conversation action
// that the agent's message mode does not allow. It uses the same status
// and code as a refused agent message.
func writeGroupModeDenied(w http.ResponseWriter, reason, mode string) {
	writeError(w, http.StatusForbidden, ErrCodeMessageDenied, "Message delivery denied", map[string]interface{}{
		"reason": reason,
		"mode":   mode,
	})
}

// groupModeMayPostAsParticipant reports whether mode can ever post to a
// group conversation: the project cell always, lineage and branch as an
// existing participant. none and unknown modes never can.
func groupModeMayPostAsParticipant(mode string) bool {
	switch effectiveGroupMessageMode(mode) {
	case store.MessageModeLineage, store.MessageModeBranch, store.MessageModeProject, store.MessageModeHub:
		return true
	}
	return false
}

// loadGroupPostSender reads the sending agent's current record for a group
// post decision on a path where the sender is not the addressed agent. A
// failed read writes the refusal; ok reports whether the caller may go on.
func (s *Server) loadGroupPostSender(ctx context.Context, w http.ResponseWriter, senderID string) (*store.Agent, bool) {
	sender, err := s.store.GetAgent(ctx, senderID)
	if err != nil || sender == nil {
		s.messageLog.Warn("group post: sender agent lookup failed",
			"sender_id", senderID, "error", err)
		writeGroupModeDenied(w, groupModeReasonUnknownMode, "")
		return nil, false
	}
	return sender, true
}

// checkInboundAgentGroupPost is checkAgentGroupPost for a sender known
// only by its authenticated ID.
func (s *Server) checkInboundAgentGroupPost(ctx context.Context, w http.ResponseWriter, senderID, conversationID string) (groupPostDecision, bool) {
	sender, ok := s.loadGroupPostSender(ctx, w, senderID)
	if !ok {
		return groupPostDecision{Reason: groupModeReasonUnknownMode}, false
	}
	return s.checkAgentGroupPost(ctx, w, sender, conversationID)
}

// effectiveGroupMessageMode maps an unset mode to project, the column
// default for every stored agent (as messageability.go does). Any other
// value is returned as is; values outside the five modes are refused by
// the callers.
func effectiveGroupMessageMode(mode string) string {
	if mode == "" {
		return store.MessageModeProject
	}
	return mode
}
