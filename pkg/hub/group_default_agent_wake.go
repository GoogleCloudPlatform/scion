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
	defaultAgentStatusSelf = "self"
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

	def, err := s.resolveGroupDefaultAgent(ctx, in.Conv.ConversationID, in.Sender.ProjectID)
	if err != nil {
		s.messageLog.Error("group default agent: lookup failed",
			"conversation_id", in.Conv.ConversationID, "error", err)
		return &messages.MentionResult{Status: "error", Error: "default agent lookup failed"}
	}
	if def == nil {
		return nil
	}
	if def.ID == in.Sender.ID {
		return &messages.MentionResult{Slug: def.Slug, Status: defaultAgentStatusSelf}
	}

	aggregateCtx, cancel := context.WithTimeout(ctx, mentionFanoutAggregateTimeout)
	defer cancel()
	// The copy lands in the group conversation itself: the default agent
	// is configured on this conversation, so it belongs there whether the
	// sender named the conversation by ID or by thread key.
	out := s.deliverAgentMentionCopy(ctx, aggregateCtx, agentMentionFanoutInput{
		Sender:             in.Sender,
		SenderIdent:        in.SenderIdent,
		Msg:                in.Msg,
		Type:               in.Type,
		ParentConv:         in.Conv,
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

// resolveGroupDefaultAgent returns the default agent of the group
// conversation conversationID, or nil when it has none. The source is the
// conversation's default_agent_id, then, for a native thread, the thread
// topic's default agent (the source a human post uses). A default outside
// projectID, deleted, or no longer resolvable counts as none. A non-nil
// error is a transient lookup failure.
func (s *Server) resolveGroupDefaultAgent(ctx context.Context, conversationID, projectID string) (*store.Agent, error) {
	if conversationID == "" || projectID == "" {
		return nil, nil
	}
	conv, err := s.store.GetConversation(ctx, conversationID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if conv == nil || conv.ProjectID == nil || *conv.ProjectID != projectID {
		return nil, nil
	}
	if conv.DefaultAgentID != nil && *conv.DefaultAgentID != "" {
		return s.lookupProjectDefaultAgent(ctx, projectID, *conv.DefaultAgentID)
	}

	refProject, threadID, perr := messaging.ParseThreadConversationExternalRef(conv.ExternalRef)
	if perr != nil || refProject != projectID {
		return nil, nil
	}
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil {
		return nil, nil
	}
	topic, err := wcs.GetTopic(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if topic == nil || topic.ProjectID != projectID || topic.DefaultAgent == "" {
		return nil, nil
	}
	return s.lookupProjectDefaultAgent(ctx, projectID, topic.DefaultAgent)
}

// lookupProjectDefaultAgent resolves ref, a slug or an agent ID, to a live
// agent of projectID, as the human send path resolves a topic default:
// slug first, then ID. It returns nil when ref names no live agent of the
// project.
func (s *Server) lookupProjectDefaultAgent(ctx context.Context, projectID, ref string) (*store.Agent, error) {
	a, err := s.store.GetAgentBySlug(ctx, projectID, ref)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if err != nil || a == nil {
		a, err = s.store.GetAgent(ctx, ref)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, nil
			}
			return nil, err
		}
	}
	if a == nil || a.ProjectID != projectID || !a.DeletedAt.IsZero() {
		return nil, nil
	}
	return a, nil
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
