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

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Inbox, conversation and notification routes for user access tokens.
//
// The routes under /api/v1/messages, /api/v1/conversations and
// /api/v1/notifications act on the caller's own records. A user access
// token reaches them only with the self-scoped selectors inbox:read and
// inbox:write, and only for records inside its boundary
// (authorizeSelfScoped, filterSelfScopedRows). Target checks on projects
// and agents named by a record (project:read, agent:read) apply on top.

// Self-scoped permission IDs used by these routes.
const (
	permInboxRead  = "inbox.read"
	permInboxWrite = "inbox.write"
)

// inboxCredential classifies the credential of a request to an inbox,
// conversation or notification route.
type inboxCredential int

const (
	// inboxCredentialRefused is every credential these routes do not
	// accept: a missing identity, or a credential kind other than the
	// three below (federation, broker on-behalf-of, an unknown or empty
	// kind).
	inboxCredentialRefused inboxCredential = iota
	// inboxCredentialSession is an interactive session or a dev
	// credential (the sessionCredentialAllowed allowlist).
	inboxCredentialSession
	// inboxCredentialAgent is an agent credential; each route's agent
	// rules apply.
	inboxCredentialAgent
	// inboxCredentialToken is a user access token; the inbox selector and
	// boundary rules apply.
	inboxCredentialToken
)

// inboxCredentialOf classifies the request's credential. The credential
// recorded by the authentication middleware decides; when the context
// carries no credential record (an internal caller that set only an
// identity), the credential is derived from the identity, as
// AuthzRequestFromContext does. A session needs the sessionCredentialAllowed
// allowlist; a token needs a user access token identity; anything else is
// refused. The returned token is non-nil only for inboxCredentialToken.
func inboxCredentialOf(ctx context.Context) (inboxCredential, *ScopedUserIdentity) {
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) {
		return inboxCredentialRefused, nil
	}
	credential := GetCredentialContextFromContext(ctx)
	sessionAllowed := sessionCredentialAllowed(ctx)
	if credential.Kind == "" {
		credential = credentialContextForIdentity(identity)
		sessionAllowed = allowedMutationCredentials[credential.Kind]
	}
	token, isToken := identity.(*ScopedUserIdentity)
	switch {
	case sessionAllowed:
		if isToken {
			return inboxCredentialRefused, nil
		}
		return inboxCredentialSession, nil
	case credential.Kind == CredentialKindAgentJWT:
		if GetAgentIdentityFromContext(ctx) != nil {
			return inboxCredentialAgent, nil
		}
	case credential.Kind == CredentialKindUAT:
		if isToken && token != nil {
			return inboxCredentialToken, token
		}
	}
	return inboxCredentialRefused, nil
}

// tokenBoundaryProject returns the boundary project of a project-boundary
// token, or "" for a hub-boundary token.
func tokenBoundaryProject(token *ScopedUserIdentity) string {
	if b := token.Boundary(); b.Kind == BoundaryKindProject {
		return b.ProjectID
	}
	return ""
}

// authorizeInboxToken is the request-level check for a token on a route
// whose records are filtered row by row: the token must carry the self
// permission, the permission must be eligible for its boundary, and a
// project-boundary token's holder must currently be a member of the
// boundary project. It writes 403 on denial.
func (s *Server) authorizeInboxToken(w http.ResponseWriter, r *http.Request, token *ScopedUserIdentity, permissionID string) bool {
	return s.authorizeSelfScoped(w, r, permissionID, tokenBoundaryProject(token))
}

// conversationProject returns the project a conversation's self row
// belongs to: a group's project (empty for a group with no project), or,
// for a direct conversation, the project of the peer agent named in its
// key. A direct conversation between two users has no project. peer is
// the peer agent of a direct conversation, or nil. A store error other
// than not-found is returned.
func (s *Server) conversationProject(ctx context.Context, conv *store.Conversation, callerKind, callerID string) (projectID string, peer *store.Agent, err error) {
	if conv.Kind != "direct" {
		if conv.ProjectID == nil {
			return "", nil, nil
		}
		return *conv.ProjectID, nil, nil
	}
	kindA, idA, kindB, idB, parseErr := messages.ParseDMKey(conv.ExternalRef)
	if parseErr != nil {
		return "", nil, parseErr
	}
	peerKind, peerID := kindB, idB
	if kindB == callerKind && idB == callerID {
		peerKind, peerID = kindA, idA
	}
	if peerKind != "agent" {
		return "", nil, nil
	}
	agent, err := s.store.GetAgent(ctx, peerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", nil, nil
		}
		return "", nil, err
	}
	return agent.ProjectID, agent, nil
}

// tokenMayUseConversation reports whether a token may apply permissionID
// to conv as one of the holder's own records: the conversation's project
// (conversationProject) must pass check, and for a direct conversation with
// an agent the token must also pass agent:read on that agent. A direct
// conversation whose peer agent does not exist has no project, so it
// needs a hub boundary.
func (s *Server) tokenMayUseConversation(ctx context.Context, token *ScopedUserIdentity, check *selfScopeCheck, conv *store.Conversation) (bool, error) {
	projectID, peer, err := s.conversationProject(ctx, conv, token.Type(), token.ID())
	if err != nil {
		return false, err
	}
	if !check.allows(projectID) {
		return false, nil
	}
	if peer != nil {
		if !s.authzService.CheckAccess(ctx, token, agentResource(peer), ActionRead).Allowed {
			return false, nil
		}
	}
	return true, nil
}

// authorizeTokenConversation writes 403 (or the store error) and returns
// false unless the token may apply permissionID to conv
// (tokenMayUseConversation).
func (s *Server) authorizeTokenConversation(w http.ResponseWriter, r *http.Request, token *ScopedUserIdentity, permissionID string, conv *store.Conversation) bool {
	ok, err := s.tokenMayUseConversation(r.Context(), token, s.newSelfScopeCheck(r.Context(), token, permissionID), conv)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return false
	}
	if !ok {
		resourceType, action := selfPermissionResourceAction(permissionID)
		logAuthzDenial(r, token, Resource{Type: resourceType}, action, "conversation is outside the token boundary or its peer agent is not readable")
		writeForbiddenStructured(w, "", resourceType, action)
		return false
	}
	return true
}

// requireInboxCredential classifies the request credential and writes 403
// for a refused one. ok is false when a response was written.
func requireInboxCredential(w http.ResponseWriter, r *http.Request) (cls inboxCredential, token *ScopedUserIdentity, ok bool) {
	cls, token = inboxCredentialOf(r.Context())
	if cls == inboxCredentialRefused {
		Forbidden(w)
		return cls, nil, false
	}
	return cls, token, true
}
