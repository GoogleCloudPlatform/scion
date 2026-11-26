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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Default-agent delivery for agent posts into group conversations.
//
// An agent post into a group conversation that @mentions no agent and
// neither addresses nor @mentions a human is delivered to the
// conversation's default agent, as a human post is. The post itself has
// already passed agentGroupPostDecision. The default agent is then
// reached through deliverAgentMentionCopy, which reserves a slot in the
// per-pair loop/storm cap, runs the pairwise authorizeAgentMessage
// check, and registers the default agent as a participant only through
// groupAgentJoinAllowed.
//
// Loop guard: an agent's own post never reaches itself; the delivered
// copy is a TypeMention row, which never triggers a further delivery; and
// two agents that are each other's default in different conversations
// share the symmetric per-pair cap with mentions.

// Default-agent result statuses that are not mention-delivery statuses.
const (
	// defaultAgentStatusSelf: the sender is the default agent.
	defaultAgentStatusSelf = "self"
	// defaultAgentStatusUnreachable: the configured default agent was
	// deleted or no longer exists.
	defaultAgentStatusUnreachable = "unreachable"
)

// groupDefaultWakeInput is the input to wakeGroupDefaultAgent.
type groupDefaultWakeInput struct {
	// Sender is the posting agent's fresh store record.
	Sender *store.Agent
	// SenderIdent is the authenticated identity of the posting agent.
	SenderIdent Identity
	// Conv is the group conversation the post landed in.
	Conv *messaging.ConversationResult
	// Msg is the original body of the post.
	Msg string
	// Type is the post's message type.
	Type string
	// MentionResults is the post's mention fan-out result. Any entry means
	// the post @mentioned an agent, and the default agent is left alone.
	MentionResults []messages.MentionResult
	// AddressedToHuman is true when the request explicitly addressed a human
	// recipient (e.g. a user: recipient or resolved user recipient_id).
	AddressedToHuman bool
	// HumanMembers is the project's human-member list when already resolved
	// by the caller; nil means wakeGroupDefaultAgent resolves it if needed.
	HumanMembers []chatMemberEntry
	// ParentMessageID is the persisted post's ID.
	ParentMessageID string
	// ParentAttachments are the attachments stored as artifacts bound to
	// the post (ptone/scion#4596), delivered with the copy.
	ParentAttachments []AttachmentRef
	// Channel is the post's delivery channel.
	Channel string
	// Resolved is the single pre-effect resolution of the group default agent
	// computed before the origin message policy gate (Contract §6).
	Resolved *resolvedDefaultWakePlan
	// CopyAdmission is the pre-effect derived admission minted by the origin
	// message policy gate.
	CopyAdmission *derivedAdmission
}

// resolvedDefaultWakePlan holds the single pre-effect resolution of a group
// conversation's default agent.
type resolvedDefaultWakePlan struct {
	EarlyResult *messages.MentionResult
	Target      *resolvedCopyTarget
}

// resolveDefaultWakeCopyTarget performs side-effect-free resolution of the
// group conversation's default agent (read-only lookup + dry-run pairwise
// authorization) once, before the origin message policy gate (Contract §6).
func (s *Server) resolveDefaultWakeCopyTarget(
	ctx context.Context,
	sender *store.Agent,
	senderIdent Identity,
	conv *messaging.ConversationResult,
	msg string,
	msgType string,
	hasMentions bool,
	addressedToHuman bool,
	humanMembers []chatMemberEntry,
) *resolvedDefaultWakePlan {
	plan := &resolvedDefaultWakePlan{}
	if sender == nil || senderIdent == nil || conv == nil || conv.Kind != "group" {
		return plan
	}
	if !mentionFanoutTypes[msgType] || hasMentions {
		return plan
	}
	if addressedToHuman || s.groupPostMentionsHuman(ctx, sender.ProjectID, msg, humanMembers) {
		return plan
	}

	gd, err := s.resolveGroupDefaultAgent(ctx, conv.ConversationID, sender.ProjectID)
	if err != nil {
		s.messageLog.Error("group default agent: lookup failed",
			"conversation_id", conv.ConversationID, "error", err)
		plan.EarlyResult = &messages.MentionResult{Status: "error", Error: "default agent lookup failed"}
		return plan
	}
	if gd.Agent == nil {
		if gd.UnreachableSlug != "" {
			plan.EarlyResult = &messages.MentionResult{
				Slug:   gd.UnreachableSlug,
				Status: defaultAgentStatusUnreachable,
				Error:  "the conversation's default agent no longer exists",
			}
		}
		return plan
	}
	def := gd.Agent
	if def.ID == sender.ID {
		plan.EarlyResult = &messages.MentionResult{Slug: def.Slug, Status: defaultAgentStatusSelf}
		return plan
	}

	if allowed, _, _ := s.authorizeAgentMessage(ctx, senderIdent, def, false); !allowed {
		plan.EarlyResult = &messages.MentionResult{
			Slug:          def.Slug,
			Status:        "unauthorized",
			Error:         "message delivery denied",
			AgentPhase:    def.Phase,
			SuspendReason: def.SuspendReason,
		}
		return plan
	}

	parent := conv
	if parent.ExternalRef == "" && gd.ExternalRef != "" {
		withRef := *parent
		withRef.ExternalRef = gd.ExternalRef
		parent = &withRef
	}
	plan.Target = &resolvedCopyTarget{
		Agent:              def,
		Position:           "default_agent",
		ParentConv:         parent,
		ParentConvVerified: true,
	}
	return plan
}

// wakeGroupDefaultAgent delivers an agent's group post to the
// conversation's pre-resolved default agent when the post @mentions no agent
// and neither addresses nor @mentions a human. It never re-resolves the
// default agent (Contract §6).
func (s *Server) wakeGroupDefaultAgent(ctx context.Context, in groupDefaultWakeInput) *messages.MentionResult {
	if in.CopyAdmission != nil {
		defer in.CopyAdmission.Close()
	}
	if in.Sender == nil || in.SenderIdent == nil || in.Conv == nil || in.Conv.Kind != "group" {
		return nil
	}
	if !mentionFanoutTypes[in.Type] || len(in.MentionResults) > 0 {
		return nil
	}
	if in.AddressedToHuman || s.groupPostMentionsHuman(ctx, in.Sender.ProjectID, in.Msg, in.HumanMembers) {
		return nil
	}
	if in.Resolved == nil {
		return nil
	}
	if in.Resolved.EarlyResult != nil {
		res := *in.Resolved.EarlyResult
		return &res
	}
	if in.Resolved.Target == nil || in.Resolved.Target.Agent == nil {
		return nil
	}
	def := in.Resolved.Target.Agent
	parent := in.Resolved.Target.ParentConv
	if parent == nil {
		parent = in.Conv
	}

	aggregateCtx, cancel := context.WithTimeout(ctx, mentionFanoutAggregateTimeout)
	defer cancel()
	out := s.deliverAgentMentionCopy(ctx, aggregateCtx, agentMentionFanoutInput{
		Sender:             in.Sender,
		SenderIdent:        in.SenderIdent,
		Msg:                in.Msg,
		Type:               in.Type,
		ParentConv:         parent,
		ParentConvVerified: true,
		ParentMessageID:    in.ParentMessageID,
		ParentAttachments:  in.ParentAttachments,
		Channel:            in.Channel,
		CopyAdmission:      in.CopyAdmission,
	}, def, "default_agent")
	s.messageLog.Info("group default agent delivery",
		"sender_id", in.Sender.ID,
		"conversation_id", in.Conv.ConversationID,
		"parent_message_id", in.ParentMessageID,
		"status", out.Status)
	return &messages.MentionResult{
		Slug:          def.Slug,
		Status:        out.Status,
		Error:         out.Error,
		AgentPhase:    out.AgentPhase,
		SuspendReason: out.SuspendReason,
	}
}

// groupDefault is the result of resolveGroupDefaultAgent.
type groupDefault struct {
	// Agent is the live default agent, or nil.
	Agent *store.Agent
	// UnreachableSlug names a configured default agent of the project
	// that was deleted or no longer exists. Set only when Agent is nil.
	UnreachableSlug string
	// ExternalRef is the conversation's stored external_ref.
	ExternalRef string
}

// resolveGroupDefaultAgent returns the default agent of the group
// conversation conversationID. The source is the conversation's
// default_agent_id, then the default agent of the webchat topic linked to
// the conversation (the source a human post uses). As on the human path,
// a default that names an agent of another project counts as none, and a
// deleted or missing one is reported as unreachable. A non-nil error is a
// transient lookup failure.
func (s *Server) resolveGroupDefaultAgent(ctx context.Context, conversationID, projectID string) (groupDefault, error) {
	if conversationID == "" || projectID == "" {
		return groupDefault{}, nil
	}
	conv, err := s.store.GetConversation(ctx, conversationID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return groupDefault{}, nil
		}
		return groupDefault{}, err
	}
	if conv == nil || conv.ProjectID == nil || *conv.ProjectID != projectID {
		return groupDefault{}, nil
	}
	gd := groupDefault{ExternalRef: conv.ExternalRef}

	if conv.DefaultAgentID != nil && *conv.DefaultAgentID != "" {
		// default_agent_id is always an agent ID.
		a, err := s.store.GetAgent(ctx, *conv.DefaultAgentID)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				return groupDefault{}, err
			}
			a = nil
		}
		gd.Agent, gd.UnreachableSlug = classifyDefaultAgent(a, projectID, *conv.DefaultAgentID)
		return gd, nil
	}

	topic, err := s.linkedTopic(ctx, conv)
	if err != nil {
		return groupDefault{}, err
	}
	if topic == nil || topic.ProjectID != projectID || topic.DefaultAgent == "" {
		return gd, nil
	}
	// A topic default may be a slug or an ID: slug first, as the human
	// send path resolves it.
	a, err := s.store.GetAgentBySlug(ctx, projectID, topic.DefaultAgent)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return groupDefault{}, err
	}
	if err != nil || a == nil {
		a, err = s.store.GetAgent(ctx, topic.DefaultAgent)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				return groupDefault{}, err
			}
			a = nil
		}
	}
	gd.Agent, gd.UnreachableSlug = classifyDefaultAgent(a, projectID, topic.DefaultAgent)
	return gd, nil
}

// classifyDefaultAgent sorts a looked-up default agent a (nil when not
// found) configured as ref: a live agent of projectID is returned as the
// default; a deleted agent of projectID, or no agent at all, is reported
// by slug (or ref) as unreachable; an agent of another project is
// neither.
func classifyDefaultAgent(a *store.Agent, projectID, ref string) (*store.Agent, string) {
	switch {
	case a == nil:
		return nil, ref
	case a.ProjectID != projectID:
		return nil, ""
	case !a.DeletedAt.IsZero():
		return nil, a.Slug
	default:
		return a, ""
	}
}

// wokenAgentSlugs lists the agents a post reached: mention results that
// were delivered (or may have been), then the default agent when it was.
func wokenAgentSlugs(mentionResults []messages.MentionResult, def *messages.MentionResult) []string {
	var out []string
	for _, r := range mentionResults {
		if r.Status == "delivered" || r.Status == "ambiguous" {
			out = append(out, r.Slug)
		}
	}
	if def != nil && (def.Status == "delivered" || def.Status == "ambiguous") {
		out = append(out, def.Slug)
	}
	return out
}

// isGroupPostAddressedToHuman reports whether an outbound group-post request
// explicitly addressed a human recipient — via a user-resolved S1 recipient,
// a "user:" recipient prefix, a raw email recipient, or a recipient_id that
// resolves to a user. Posts addressed to a human are excluded from
// default-agent fan-out (ptone/scion#9446).
func (s *Server) isGroupPostAddressedToHuman(ctx context.Context, req *OutboundMessageRequest, result *routingResult, humanMembers []chatMemberEntry) bool {
	if result != nil {
		if result.ExplicitUserRecipient {
			return true
		}
		if strings.HasPrefix(strings.ToLower(result.Recipient), "user:") {
			return true
		}
	}
	if req == nil {
		return false
	}
	lowerRecip := strings.ToLower(req.Recipient)
	if strings.HasPrefix(lowerRecip, "user:") {
		return true
	}
	if req.Recipient != "" &&
		!strings.HasPrefix(lowerRecip, "agent:") &&
		!strings.HasPrefix(lowerRecip, "conv:") &&
		!strings.HasPrefix(lowerRecip, "thread:") &&
		strings.Contains(req.Recipient, "@") {
		return true
	}
	if req.RecipientID != "" && !strings.HasPrefix(lowerRecip, "agent:") {
		for _, m := range humanMembers {
			if m.Kind == "user" && m.ID == req.RecipientID {
				return true
			}
		}
		if u, err := s.store.GetUser(ctx, req.RecipientID); err == nil && u != nil {
			return true
		}
	}
	return false
}

// groupPostMentionsHuman reports whether msg contains a prose @mention
// targeting a human: a raw email (@name@domain), a @user: prefixed token, or
// any mention matching a project human member (by display name, hyphenated
// display-name slug, email, or email local part). Non-prose spans (fenced
// code blocks, inline code, and '>' blockquotes) are ignored, matching
// agent mention extraction.
func (s *Server) groupPostMentionsHuman(ctx context.Context, projectID, msg string, preResolvedHumans []chatMemberEntry) bool {
	opts := s.mentionOptions()
	names := messages.DedupMentionNames(
		messages.ExtractProseMentionsWithOptions(msg, opts),
		messages.ExtractProseWrappedMentionsWithOptions(msg, opts),
	)
	if len(names) == 0 {
		return false
	}
	for _, name := range names {
		if strings.Contains(name, "@") || strings.HasPrefix(strings.ToLower(name), "user:") {
			return true
		}
	}
	humans := preResolvedHumans
	if humans == nil && projectID != "" {
		humans = s.resolveProjectHumanMembers(ctx, projectID)
	}
	return len(mentionedHumanIDs(humans, names, "")) > 0
}
