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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Chat routes for user access tokens.
//
// A chat write acts on the holder's behalf, so a user access token needs
// the self-scoped selector inbox:write for it, and inbox:read to read the
// holder's own chat records (direct messages, read state, preferences).
// The record's project decides the boundary: a topic's project, the
// project of the peer agent of a direct message, an agent's project for
// thread preferences. A record with no project (a direct message between
// users, a project-less upload, the holder's rail preferences) needs a hub
// boundary. A project-boundary token's holder must also be a current
// member of that project (selfScopeCheck). The target checks every caller
// passes (project:read, agent:read, agent:message, agent:attach) apply on
// top.

// chatCredentialAllowed reports whether the request's credential may use
// the chat routes: an interactive session or a dev credential (the
// sessionCredentialAllowed allowlist), a federated user, or a user access
// token. Every other credential is refused: a broker acting for a user,
// hub delivery, an agent, and a missing or unknown kind. A request with
// no credential record is classified from its identity.
func chatCredentialAllowed(ctx context.Context) bool {
	switch cls, _ := inboxCredentialOf(ctx); cls {
	case inboxCredentialSession, inboxCredentialToken:
		return true
	}
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) || IsScopedUserIdentity(identity) {
		return false
	}
	credential := GetCredentialContextFromContext(ctx)
	if credential.Kind == "" {
		credential = credentialContextForIdentity(identity)
	}
	if credential.Kind != CredentialKindFederation {
		return false
	}
	if _, federated := identity.(FederatedIdentity); !federated {
		return false
	}
	_, isUser := identity.(UserIdentity)
	return isUser
}

// chatRoute wraps a chat route handler: a credential chatCredentialAllowed
// refuses gets 403 before the handler runs.
func (s *Server) chatRoute(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !chatCredentialAllowed(r.Context()) {
			logAuthzDenial(r, GetIdentityFromContext(r.Context()), Resource{Type: "inbox"}, ActionRead, "credential cannot use chat routes")
			Forbidden(w)
			return
		}
		next(w, r)
	}
}

// chatToken returns the request's user access token, or nil when the
// caller is not using one.
func chatToken(ctx context.Context) *ScopedUserIdentity {
	cls, token := inboxCredentialOf(ctx)
	if cls != inboxCredentialToken {
		return nil
	}
	return token
}

// requireChatSelector is the request-level check for a token on a chat
// route: the token must carry the self permission permissionID, the
// permission must be eligible for its boundary, and a project-boundary
// token's holder must be a current member of the boundary project. It
// depends on no target, so it runs before any lookup and writes 403 on
// denial. token is nil for a caller that is not using a token, which
// always passes.
func (s *Server) requireChatSelector(w http.ResponseWriter, r *http.Request, permissionID string) (token *ScopedUserIdentity, ok bool) {
	token = chatToken(r.Context())
	if token == nil {
		return nil, true
	}
	return token, s.authorizeInboxToken(w, r, token, permissionID)
}

// chatConversationRecord returns the conversation shape the inbox helpers
// read for a chat conversation key: a direct conversation for a DM key, a
// group of topicProjectID for a topic key.
func chatConversationRecord(key, topicProjectID string) *store.Conversation {
	if strings.HasPrefix(key, "dm:") {
		return &store.Conversation{Kind: "direct", ExternalRef: key}
	}
	projectID := topicProjectID
	return &store.Conversation{Kind: "group", ProjectID: &projectID}
}

// chatTokenMayUse reports whether token may apply permissionID to the chat
// conversation key (tokenMayUseConversation): a topic's project must pass
// the self-scope check; a DM with an agent needs the agent's project to
// pass it and agent:read on the agent; a DM between users needs a hub
// boundary.
func (s *Server) chatTokenMayUse(ctx context.Context, token *ScopedUserIdentity, permissionID, key, topicProjectID string) (bool, error) {
	return s.tokenMayUseConversation(ctx, token, s.newSelfScopeCheck(ctx, token, permissionID), chatConversationRecord(key, topicProjectID))
}

// chatTokenMaySend reports whether token may post to the chat conversation
// key: inbox:write must pass the self-scope check for the conversation's
// project (a topic's project, a DM peer agent's project, none for a DM
// between users). The messaging check on an agent recipient
// (agent:message) applies separately.
func (s *Server) chatTokenMaySend(ctx context.Context, token *ScopedUserIdentity, key, topicProjectID string) (bool, error) {
	projectID, _, err := s.conversationProject(ctx, chatConversationRecord(key, topicProjectID), token.Type(), token.ID())
	if err != nil {
		return false, err
	}
	return s.newSelfScopeCheck(ctx, token, permInboxWrite).allows(projectID), nil
}

// chatTokenRefusal selects how a chat route answers a token refused for a
// conversation.
type chatTokenRefusal int

const (
	// chatRefuseForbidden answers 403.
	chatRefuseForbidden chatTokenRefusal = iota
	// chatRefuseNotFound answers as a missing thread (404 "Thread not
	// found"), as the route answers every caller it refuses.
	chatRefuseNotFound
)

// chatTokenRefusalReason is the logged reason for a token refused for a
// conversation.
const chatTokenRefusalReason = "conversation is outside the token boundary, or its peer agent is not readable by the token"

// writeChatTokenRefusal writes the refusal of token for the request.
func writeChatTokenRefusal(w http.ResponseWriter, r *http.Request, token *ScopedUserIdentity, permissionID string, refusal chatTokenRefusal) {
	if refusal == chatRefuseNotFound {
		logReferenceRefused(r.Context(), logging.RequestPath(r), chatTokenRefusalReason, token)
		NotFound(w, "Thread")
		return
	}
	resourceType, action := selfPermissionResourceAction(permissionID)
	logAuthzDenial(r, token, Resource{Type: resourceType}, action, chatTokenRefusalReason)
	writeForbiddenStructured(w, "", resourceType, action)
}

// authorizeChatTokenConversation applies chatTokenMayUse for a token
// caller and writes the refusal (or the store error). A caller that is not
// using a token (token nil) passes.
func (s *Server) authorizeChatTokenConversation(w http.ResponseWriter, r *http.Request, token *ScopedUserIdentity, permissionID, key, topicProjectID string, refusal chatTokenRefusal) bool {
	if token == nil {
		return true
	}
	ok, err := s.chatTokenMayUse(r.Context(), token, permissionID, key, topicProjectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return false
	}
	if !ok {
		writeChatTokenRefusal(w, r, token, permissionID, refusal)
		return false
	}
	return true
}

// authorizeChatTokenProject applies the self-scope check of permissionID to
// a record of projectID for a token caller and writes the refusal. A
// caller that is not using a token (token nil) passes.
func (s *Server) authorizeChatTokenProject(w http.ResponseWriter, r *http.Request, token *ScopedUserIdentity, permissionID, projectID string, refusal chatTokenRefusal) bool {
	if token == nil {
		return true
	}
	if s.newSelfScopeCheck(r.Context(), token, permissionID).allows(projectID) {
		return true
	}
	writeChatTokenRefusal(w, r, token, permissionID, refusal)
	return false
}

// chatTokenDMVisible reports whether token may read the DM key under
// inbox:read (chatTokenMayUse); a store error answers false. List and
// count handlers use it to drop the DMs a token may not read.
func (s *Server) chatTokenDMVisible(ctx context.Context, token *ScopedUserIdentity, check *selfScopeCheck, key string) bool {
	ok, err := s.tokenMayUseConversation(ctx, token, check, chatConversationRecord(key, ""))
	return err == nil && ok
}

// authorizeChatTokenMessageTarget re-checks, for a token editing or
// deleting one of its holder's messages, the conversation the message is
// in: a topic needs project:read on the topic's project and inbox:write
// for it; a DM needs inbox:write under the DM rule (chatTokenMayUse). It
// writes 403 on refusal. A caller that is not using a token (token nil)
// passes.
func (s *Server) authorizeChatTokenMessageTarget(w http.ResponseWriter, r *http.Request, token *ScopedUserIdentity, wcs WebChatStore, key string) bool {
	if token == nil {
		return true
	}
	ctx := r.Context()
	topicProjectID := ""
	if strings.HasPrefix(key, "dm:") {
		if !validDMKey(key) {
			writeChatTokenRefusal(w, r, token, permInboxWrite, chatRefuseForbidden)
			return false
		}
	} else {
		topic, err := wcs.GetTopic(ctx, key)
		if err != nil || topic == nil {
			writeChatTokenRefusal(w, r, token, permInboxWrite, chatRefuseForbidden)
			return false
		}
		project, err := s.store.GetProject(ctx, topic.ProjectID)
		if err != nil {
			writeChatTokenRefusal(w, r, token, permInboxWrite, chatRefuseForbidden)
			return false
		}
		resource := projectResource(project)
		if decision := s.authzService.CheckAccess(ctx, token, resource, ActionRead); !decision.Allowed {
			logAuthzDenial(r, token, resource, ActionRead, decision.Reason)
			writeForbiddenStructured(w, "", resource.Type, ActionRead)
			return false
		}
		topicProjectID = topic.ProjectID
	}
	return s.authorizeChatTokenConversation(w, r, token, permInboxWrite, key, topicProjectID, chatRefuseForbidden)
}
