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

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Default-agent delivery for agent posts into group conversations.
//
// An agent post into a group conversation that @mentions no agent is
// delivered to the conversation's default agent, as a human post is. The
// post itself has already passed agentGroupPostDecision. The default
// agent is then reached through deliverAgentMentionCopy, which reserves a
// slot in the per-pair loop/storm cap, runs the pairwise
// authorizeAgentMessage check, and registers the default agent as a
// participant only through groupAgentJoinAllowed.
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
	// ParentMessageID is the persisted post's ID.
	ParentMessageID string
	// Channel is the post's delivery channel.
	Channel string
}

// wakeGroupDefaultAgent delivers an agent's group post to the
// conversation's default agent when the post @mentions no agent. It
// returns nil when no default agent applies (not a group post, an
// excluded type, an agent mention present, or no default set); otherwise
// the result of the attempt. A refused delivery is reported in the result
// and never fails the post.
func (s *Server) wakeGroupDefaultAgent(ctx context.Context, in groupDefaultWakeInput) *messages.MentionResult {
	if in.Sender == nil || in.SenderIdent == nil || in.Conv == nil || in.Conv.Kind != "group" {
		return nil
	}
	if !mentionFanoutTypes[in.Type] || len(in.MentionResults) > 0 {
		return nil
	}

	gd, err := s.resolveGroupDefaultAgent(ctx, in.Conv.ConversationID, in.Sender.ProjectID)
	if err != nil {
		s.messageLog.Error("group default agent: lookup failed",
			"conversation_id", in.Conv.ConversationID, "error", err)
		return &messages.MentionResult{Status: "error", Error: "default agent lookup failed"}
	}
	if gd.Agent == nil {
		if gd.UnreachableSlug != "" {
			return &messages.MentionResult{Slug: gd.UnreachableSlug, Status: defaultAgentStatusUnreachable,
				Error: "the conversation's default agent no longer exists"}
		}
		return nil
	}
	def := gd.Agent
	if def.ID == in.Sender.ID {
		return &messages.MentionResult{Slug: def.Slug, Status: defaultAgentStatusSelf}
	}

	// The copy lands in the group conversation itself: the default agent
	// is configured on this conversation, so it belongs there whether the
	// sender named the conversation by ID or by thread key. A conversation
	// resolved by thread key carries no external_ref, so the stored one
	// is used for the copy's thread key.
	parent := in.Conv
	if parent.ExternalRef == "" && gd.ExternalRef != "" {
		withRef := *parent
		withRef.ExternalRef = gd.ExternalRef
		parent = &withRef
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
		Channel:            in.Channel,
	}, def, "default_agent")
	s.messageLog.Info("group default agent delivery",
		"sender_id", in.Sender.ID,
		"conversation_id", in.Conv.ConversationID,
		"parent_message_id", in.ParentMessageID,
		"status", out.Status)
	return &messages.MentionResult{Slug: def.Slug, Status: out.Status, Error: out.Error, AgentPhase: out.AgentPhase}
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
