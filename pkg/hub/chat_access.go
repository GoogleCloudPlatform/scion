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
	"slices"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Chat permission model (ptone/scion#5519).
//
// Whether a principal may engage in a project's chat is decided at the
// project level, by two resolvers:
//
//   - chatSpaceAccess decides what a caller may do in a project's chat
//     space (list and create threads and groups, set the space emoji).
//   - chatThreadAccess decides what a caller may do in one thread (a topic
//     or a project group conversation).
//
// For a user (a session, a dev credential or a user access token) read is
// project.read and every chat write is project.chat_post, checked on the
// account (chatPostIdentity); a user access token also needs inbox:write
// for the project (the token-scope step). An agent is decided by its
// message mode (agentThreadPostPolicy), never by project read. Every other
// caller kind gets nothing.
//
// The resolvers decide whether the caller may engage at all. The existing
// participation guards (sender-only edit and delete, authorizeAgentMessage
// per agent recipient, the participant-add rules, DM key participation)
// still decide each specific act after them. Direct messages are not
// decided here: project read never opens a DM, which stays on key
// participation.

// Chat actions a ChatAccess may list. The web reads them from the
// _capabilities field with the same helpers it uses for project
// capabilities.
const (
	ChatActionRead            = "read"
	ChatActionPost            = "post"
	ChatActionCreateThread    = "create_thread"
	ChatActionCreateGroup     = "create_group"
	ChatActionRename          = "rename"
	ChatActionDelete          = "delete"
	ChatActionSetDefaultAgent = "set_default_agent"
	ChatActionAddParticipant  = "add_participant"
	ChatActionEditOwn         = "edit_own"
	ChatActionDeleteOwn       = "delete_own"
	ChatActionSetEmoji        = "set_emoji"
)

// Reasons a ChatAccess gives for missing write actions.
const (
	// ChatReasonReadOnly: the caller reads the chat but its account does
	// not hold project.chat_post.
	ChatReasonReadOnly = "read_only"
	// ChatReasonTokenScope: the caller's user access token may not write
	// chat in the project (no inbox:write for it).
	ChatReasonTokenScope = "token_scope"
)

// errChatThreadUnresolved is the error of an agent decision for a lineage or
// branch agent made without the thread's conversation row
// (chatThreadRef.ConversationID).
var errChatThreadUnresolved = errors.New("chat thread access: the agent decision needs the thread's conversation id")

// chatThreadWriteActions are the thread actions that need project.chat_post.
var chatThreadWriteActions = []string{
	ChatActionPost,
	ChatActionRename,
	ChatActionDelete,
	ChatActionSetDefaultAgent,
	ChatActionAddParticipant,
	ChatActionEditOwn,
}

// chatSpaceWriteActions are the space actions that need project.chat_post.
var chatSpaceWriteActions = []string{
	ChatActionCreateThread,
	ChatActionCreateGroup,
}

// ChatAccess is the outcome of a chat resolver: the actions the caller may
// take and, when write actions are missing although the caller may read,
// why. A caller that may not read gets no actions; its route answers as
// for a missing thread or space. It is serialized as the _capabilities
// field of chat responses.
type ChatAccess struct {
	Actions []string `json:"actions"`
	Reason  string   `json:"reason,omitempty"`

	// The decision that refused read or post, for the refusal site's
	// authorization log (logAuthzDenial). Never serialized.
	readDenial Decision
	postDenial Decision
}

// Can reports whether action is one of the allowed actions.
func (a ChatAccess) Can(action string) bool {
	return slices.Contains(a.Actions, action)
}

// chatThreadRef names the thread a chatThreadAccess decision is about.
type chatThreadRef struct {
	// Key is the chat conversation key of a topic; empty for a group
	// conversation addressed by id.
	Key string
	// Project is the thread's current project. It must be non-nil for a
	// user caller; an agent caller is not decided by it.
	Project *store.Project
	// ConversationID is the thread's conversation row. The agent branch
	// needs it for the lineage and branch modes, which allow only an
	// existing participant, so a caller deciding an agent must resolve it
	// first: for those modes an empty ConversationID is an error
	// (errChatThreadUnresolved), never "not a participant". A user is
	// decided by Project alone.
	ConversationID string
	// skipTokenScope leaves out the token-scope step for a user access
	// token. Only the agent message route's thread-key posts set it
	// (decideUserThreadPost): a token there keeps that route's own checks
	// in Phase 1 (design section 8.3 item 4); Phase 2 decides whether to
	// unify it with chat sends.
	skipTokenScope bool
	// readAllowed records that this request already holds an allowed
	// project-read decision for Project and this caller, made by
	// authorizeChatThreadRead with the same check (project.read on the
	// caller), so the user branch does not repeat it.
	//
	// Invariant: it may be set true ONLY immediately after
	// authorizeChatThreadRead allowed the read for the same principal,
	// project and thread in the same request, and never from request input.
	// The sole setter today is handleConversationHistory
	// (handlers_chat_v2.go).
	readAllowed bool
}

// chatThreadAccess is the single decision point for what caller may do in
// one thread (ptone/scion#5519 design section 3.3).
//
//   - A user (session, dev credential or access token) needs project.read
//     on the thread's project for anything; without it the result is empty
//     and the route answers as for a missing thread. With it, read and
//     delete_own (retracting its own messages) are allowed, and every write
//     action when the account holds project.chat_post. A token that may not
//     write chat in the project loses every write, delete_own included
//     (reason token_scope); a reader without project.chat_post has reason
//     read_only.
//   - An agent is decided by agentThreadPostPolicy alone: no project read
//     and no project equality.
//   - Every other caller kind gets nothing.
//
// A non-nil error is a store fault in the agent branch; callers fail
// closed.
func (s *Server) chatThreadAccess(ctx context.Context, caller Identity, thread chatThreadRef) (ChatAccess, error) {
	if isNilIdentity(caller) {
		return ChatAccess{}, nil
	}
	if agentIdent, ok := caller.(AgentIdentity); ok {
		agent, err := s.store.GetAgent(ctx, agentIdent.ID())
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return ChatAccess{}, nil
			}
			return ChatAccess{}, err
		}
		return s.agentThreadPostPolicy(ctx, agent, thread.ConversationID)
	}
	user, ok := caller.(UserIdentity)
	if !ok || thread.Project == nil {
		return ChatAccess{}, nil
	}
	return s.chatUserAccess(ctx, user, thread.Project,
		[]string{ChatActionRead, ChatActionDeleteOwn}, chatThreadWriteActions,
		[]string{ChatActionDeleteOwn}, !thread.skipTokenScope, thread.readAllowed), nil
}

// chatSpaceAccess is the single decision point for what caller may do in
// project's chat space: read it (project.read), create threads and groups
// (project.chat_post) and set its emoji (project.update). Only users are
// decided here; every other caller kind gets nothing. The token-scope step
// and the reasons are those of chatThreadAccess.
func (s *Server) chatSpaceAccess(ctx context.Context, caller Identity, project *store.Project) ChatAccess {
	user, ok := caller.(UserIdentity)
	if !ok || isNilIdentity(caller) || project == nil {
		return ChatAccess{}
	}
	access := s.chatUserAccess(ctx, user, project, []string{ChatActionRead}, chatSpaceWriteActions, nil, true, false)
	if !access.Can(ChatActionRead) {
		return access
	}
	if access.Reason != ChatReasonTokenScope &&
		s.authzService.CheckAccess(ctx, user, projectResource(project), ActionUpdate).Allowed {
		access.Actions = append(access.Actions, ChatActionSetEmoji)
	}
	return access
}

// chatUserAccess is the user branch shared by the two resolvers. base is
// granted with project.read, writes in addition with project.chat_post,
// and tokenDropped lists the base actions a token without chat write scope
// loses. tokenScope selects the token-scope step. readAllowed skips the
// read check when the caller already made it (chatThreadRef.readAllowed).
func (s *Server) chatUserAccess(ctx context.Context, user UserIdentity, project *store.Project, base, writes, tokenDropped []string, tokenScope, readAllowed bool) ChatAccess {
	if s.authzService == nil {
		return ChatAccess{}
	}
	resource := projectResource(project)
	// The read ceiling. It is checked on the caller itself, so a token's
	// own ceiling applies.
	if !readAllowed {
		read := s.authzService.CheckAccess(ctx, user, resource, ActionRead)
		if !read.Allowed {
			return ChatAccess{readDenial: read}
		}
	}
	access := ChatAccess{Actions: slices.Clone(base)}
	post := s.authzService.CheckAccess(ctx, chatPostIdentity(user), resource, ActionChatPost)
	if post.Allowed {
		access.Actions = append(access.Actions, writes...)
	} else {
		access.postDenial = post
	}
	if token, isToken := user.(*ScopedUserIdentity); tokenScope && isToken && token != nil &&
		!s.newSelfScopeCheck(ctx, token, permInboxWrite).allows(project.ID) {
		access.Actions = slices.DeleteFunc(access.Actions, func(a string) bool {
			return slices.Contains(writes, a) || slices.Contains(tokenDropped, a)
		})
		access.Reason = ChatReasonTokenScope
		access.postDenial = Decision{Allowed: false, Reason: "token may not write chat in this project (inbox:write)"}
		return access
	}
	if !post.Allowed {
		access.Reason = ChatReasonReadOnly
	}
	return access
}

// agentThreadPostPolicy decides what agent may do in a thread by its
// message mode, exactly as agentGroupPostDecision does: project and hub
// modes may post (including the cross-project posting hub mode permits by
// design), lineage and branch only as an existing participant of
// conversationID, and none and unknown modes may not. Project read and
// project equality are not applied.
//
// It is kept apart from the user branch of chatThreadAccess so that a
// change to chat permissions cannot change agent posting unnoticed, and so
// the two policies can move independently.
//
// Callers: chatThreadAccess (its agent branch) only. The agent messaging
// paths (precheckThreadGroupPost, checkAgentGroupPost) still call
// agentGroupPostDecision directly until they are routed through
// chatThreadAccess (ptone/scion#5519 Phase 2).
func (s *Server) agentThreadPostPolicy(ctx context.Context, agent *store.Agent, conversationID string) (ChatAccess, error) {
	// The participant rule needs the conversation row: the caller must
	// resolve it (chatThreadRef.ConversationID).
	if agent != nil && conversationID == "" {
		switch effectiveGroupMessageMode(agent.MessageMode) {
		case store.MessageModeLineage, store.MessageModeBranch:
			return ChatAccess{}, errChatThreadUnresolved
		}
	}
	decision, err := s.agentGroupPostDecision(ctx, agent, conversationID)
	if err != nil {
		return ChatAccess{}, err
	}
	if !decision.Allowed {
		return ChatAccess{postDenial: Decision{Allowed: false, Reason: decision.Reason}}, nil
	}
	return ChatAccess{Actions: []string{ChatActionPost}}, nil
}
