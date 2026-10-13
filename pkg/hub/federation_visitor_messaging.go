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
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Project visitors messaging agents (ptone/scion#4603): which sub-routes of
// a subtree route admit a visitor, the per-request check that the visitor
// may message the agent a sub-route names, the request fields a visitor may
// use, and the sender metadata the agent sees.

// visitorAgentRef names the agent a visitor sub-route acts on.
type visitorAgentRef struct {
	// ProjectID is the project path segment of the project form ({uuid} or
	// {uuid}__{slug}), "" on the other forms.
	ProjectID string
	// AgentID is the agent path segment: an agent ID on the by-ID form, an
	// agent ID or slug on the project form, "" on a DM route.
	AgentID string
	// DMKey is the conversation key of a DM route, "" on the other forms.
	DMKey string
}

// visitorSubRoute is one sub-path, under a subtree route pattern (a
// pattern ending in "/"), on which a visitor is admitted.
type visitorSubRoute struct {
	// Pattern is the route table pattern; it is a subtree pattern.
	Pattern string
	// Method is the admitted method. The route must also list it in
	// VisitorMethods (refuseVisitorRequest runs first).
	Method string
	// SubPath is the admitted path shape under Pattern, for documentation
	// and test labels.
	SubPath string
	// match reports whether a request has this shape and returns the agent
	// reference it names.
	match func(r *http.Request) (visitorAgentRef, bool)
}

// visitorAdmittedSubRoutes is the complete, deny-by-default list of the
// sub-routes of subtree patterns that admit a visitor: sending a message
// to an agent (project and by-ID forms), and the visitor's own DM history
// and read marker with an agent. On a subtree route a visitor request
// that matches no entry for the route's pattern and the request's method
// is refused (admitVisitorSubRoute).
var visitorAdmittedSubRoutes = []visitorSubRoute{
	{Pattern: "/api/v1/projects/", Method: http.MethodPost, SubPath: "{projectId}/agents/{agentId}/message",
		match: matchVisitorAgentSubRoute(ProjectAgentRouteActionMessage)},
	{Pattern: "/api/v1/agents/", Method: http.MethodPost, SubPath: "{agentId}/message",
		match: matchVisitorAgentSubRoute(AgentRouteActionMessage)},
	{Pattern: "/api/v1/chat/conversations/", Method: http.MethodGet, SubPath: "{dmKey}/messages",
		match: matchVisitorDMSubRoute("messages")},
	{Pattern: "/api/v1/chat/conversations/", Method: http.MethodGet, SubPath: "{dmKey}/read",
		match: matchVisitorDMSubRoute("read")},
	{Pattern: "/api/v1/chat/conversations/", Method: http.MethodPost, SubPath: "{dmKey}/read",
		match: matchVisitorDMSubRoute("read")},
}

// matchVisitorAgentSubRoute matches the agent sub-route id, resolved from
// the request's percent-encoded path exactly as routeGuard resolves it for
// the handler.
func matchVisitorAgentSubRoute(id AgentSubRouteID) func(r *http.Request) (visitorAgentRef, bool) {
	return func(r *http.Request) (visitorAgentRef, bool) {
		route, ok := ResolveAgentSubRoute(r.Method, r.URL.EscapedPath())
		if !ok || route.RouteID != id || route.AgentID == "" {
			return visitorAgentRef{}, false
		}
		return visitorAgentRef{ProjectID: route.ProjectID, AgentID: route.AgentID}, true
	}
}

// chatConversationsPrefix is the path prefix of the chat conversation
// routes (handleChatConversationRoutes).
const chatConversationsPrefix = "/api/v1/chat/conversations/"

// matchVisitorDMSubRoute matches {dmKey}/{action} under the chat
// conversation routes, with the path split exactly as
// handleChatConversationRoutes splits it. The key must be a DM key.
func matchVisitorDMSubRoute(action string) func(r *http.Request) (visitorAgentRef, bool) {
	return func(r *http.Request) (visitorAgentRef, bool) {
		rest, ok := strings.CutPrefix(r.URL.Path, chatConversationsPrefix)
		if !ok {
			return visitorAgentRef{}, false
		}
		key, tail, found := strings.Cut(rest, "/")
		if !found || tail != action || !strings.HasPrefix(key, "dm:") {
			return visitorAgentRef{}, false
		}
		return visitorAgentRef{DMKey: key}, true
	}
}

// routePatternIsSubtree reports whether a route table pattern (optionally
// method-qualified) matches a subtree of paths: it ends in "/" or in a
// "{name...}" wildcard.
func routePatternIsSubtree(pattern string) bool {
	if _, path, ok := strings.Cut(pattern, " "); ok {
		pattern = path
	}
	return strings.HasSuffix(pattern, "/") || strings.HasSuffix(pattern, "...}")
}

// matchVisitorSubRoute returns the agent reference of the first entry of
// visitorAdmittedSubRoutes for pattern and r's method that r matches.
func matchVisitorSubRoute(pattern string, r *http.Request) (visitorAgentRef, bool) {
	for _, sub := range visitorAdmittedSubRoutes {
		if sub.Pattern != pattern || sub.Method != r.Method {
			continue
		}
		if ref, ok := sub.match(r); ok {
			return ref, true
		}
	}
	return visitorAgentRef{}, false
}

// writeVisitorRefusal writes the uniform visitor refusal: 403,
// ErrCodeForbidden and visitorRouteRefusalMessage, with no details. Every
// refusal of a visitor uses it (a route or method not open to visitors, an
// unknown project or agent, no current grant, the agent's message mode,
// another principal's DM), and so does the auth middleware when the hub no
// longer accepts visitors from the issuer, so no response distinguishes
// these cases.
func writeVisitorRefusal(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, ErrCodeForbidden, visitorRouteRefusalMessage, nil)
}

// errVisitorTargetRefused is visitorTargetAgent's answer for a target the
// visitor may not act on: an unknown project or agent, or a DM key that is
// not the visitor's own DM with an agent.
var errVisitorTargetRefused = errors.New("visitor target refused")

// admitVisitorSubRoute is routeGuard's second visitor check; it runs after
// refuseVisitorRequest admitted the request's method. It admits every
// request whose identity is not a *VisitorIdentity, and a visitor on a
// route whose pattern is not a subtree (VisitorMethods alone decides
// there). On a subtree route it admits a visitor only when all of these
// hold, checked on every request with nothing cached:
//   - the federation.project_visitors experiment is enabled;
//   - the request matches an entry of visitorAdmittedSubRoutes for the
//     route's pattern and the request's method;
//   - the entry names an agent that exists (in the named project, on the
//     project form), and on a DM route the key is the visitor's own DM
//     with that agent;
//   - the visitor may message the agent now (authorizeAgentMessage: a
//     current project-visitor grant, through agent.message, and the agent's
//     message mode).
//
// Otherwise it writes the uniform refusal (writeVisitorRefusal) and
// returns false; a store error other than not-found is written as such.
func (s *Server) admitVisitorSubRoute(w http.ResponseWriter, r *http.Request, meta RouteMetadata) bool {
	visitor, ok := GetIdentityFromContext(r.Context()).(*VisitorIdentity)
	if !ok || visitor == nil || !routePatternIsSubtree(meta.Pattern) {
		return true
	}
	refuse := func(reason string) bool {
		slog.Info("visitor request refused",
			"route", meta.RouteID, "method", r.Method, "visitor_id", visitor.ID(), "reason", reason)
		writeVisitorRefusal(w)
		return false
	}
	if !s.experimentEnabled(experiments.FederationProjectVisitors) {
		return refuse("project visitors experiment is off")
	}
	ref, ok := matchVisitorSubRoute(meta.Pattern, r)
	if !ok {
		return refuse("sub-route is not open to visitors")
	}
	agent, err := s.visitorTargetAgent(r.Context(), visitor, ref)
	if errors.Is(err, errVisitorTargetRefused) {
		return refuse("unknown target or not the visitor's own DM")
	}
	if err != nil {
		writeErrorFromErr(w, err, "")
		return false
	}
	if allowed, reason, _ := s.authorizeAgentMessage(r.Context(), visitor, agent, false); !allowed {
		return refuse(reason)
	}
	return true
}

// visitorTargetAgent resolves the agent ref names. A DM key must be the
// visitor's own DM with an agent (visitorDMAgentID). An unknown project or
// agent, or a key that is not the visitor's DM, returns
// errVisitorTargetRefused; any other store error is returned as is.
func (s *Server) visitorTargetAgent(ctx context.Context, visitor *VisitorIdentity, ref visitorAgentRef) (*store.Agent, error) {
	if s.store == nil {
		return nil, errVisitorTargetRefused
	}
	var (
		agent *store.Agent
		err   error
	)
	switch {
	case ref.DMKey != "":
		agentID, ok := visitorDMAgentID(ref.DMKey, visitor.ID())
		if !ok {
			return nil, errVisitorTargetRefused
		}
		agent, err = s.store.GetAgent(ctx, agentID)
	case ref.ProjectID != "":
		var project *store.Project
		project, err = s.store.GetProject(ctx, resolveProjectID(ref.ProjectID))
		if err == nil {
			agent, err = s.resolveProjectAgent(ctx, project.ID, ref.AgentID)
		}
	case ref.AgentID != "":
		agent, err = s.store.GetAgent(ctx, ref.AgentID)
	default:
		return nil, errVisitorTargetRefused
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, errVisitorTargetRefused
	}
	if err != nil {
		return nil, err
	}
	if agent == nil {
		return nil, errVisitorTargetRefused
	}
	return agent, nil
}

// visitorDMAgentID returns the agent ID of key when key is the canonical
// DM key of an agent and the user visitorID (dm:agent:<id>:user:<visitorID>),
// and false for any other key.
func visitorDMAgentID(key, visitorID string) (string, bool) {
	kindA, agentID, kindB, userID, err := messages.ParseDMKey(key)
	if err != nil || kindA != "agent" || kindB != "user" || userID != visitorID {
		return "", false
	}
	canonical, err := messages.DMConversationKey("agent", agentID, "user", userID)
	if err != nil || canonical != key {
		return "", false
	}
	return agentID, true
}

// visitorMessageTypes are the structured message types a visitor may send.
var visitorMessageTypes = map[string]bool{
	"":                       true, // defaults to an instruction
	messages.TypeInstruction: true,
	messages.TypeChat:        true,
	messages.TypeReply:       true,
}

// visitorMessageRequestRefusal returns the name of the first field of req a
// visitor may not use when messaging the agent agentID, or "" when req is a
// direct text message the visitor may send. A visitor sends text only, in
// its envelope (so the agent sees the sender metadata), to the one agent,
// in the visitor's own DM with it: no plain delivery, interrupt, wake,
// notification subscription, mentions, attachments, client metadata
// (artifact references included), conversation reference, group or
// multi-recipient addressing, channel, prerendered delivery text, or a
// thread other than that DM.
func visitorMessageRequestRefusal(req *MessageRequest, visitorID, agentID string) string {
	switch {
	case req.Plain:
		return "plain"
	case req.Interrupt:
		return "interrupt"
	case req.Wake:
		return "wake"
	case req.Notify:
		return "notify"
	case len(req.Mentions) > 0:
		return "mentions"
	}
	sm := req.StructuredMessage
	if sm == nil {
		return ""
	}
	ownDM, _ := messages.DMConversationKey("agent", agentID, "user", visitorID)
	switch {
	case sm.Plain:
		return "structured_message.plain"
	case !visitorMessageTypes[sm.Type]:
		return "structured_message.type"
	case len(sm.Attachments) > 0:
		return "structured_message.attachments"
	case len(sm.Metadata) > 0:
		return "structured_message.metadata"
	case sm.ConversationID != "":
		return "structured_message.conversation_id"
	case messages.IsGroupRecipient(sm.Recipient):
		return "structured_message.recipient"
	case sm.Recipients != "":
		return "structured_message.recipients"
	case sm.Broadcasted:
		return "structured_message.broadcasted"
	case sm.ObserverOnly:
		return "structured_message.observer_only"
	case sm.Channel != "":
		return "structured_message.channel"
	case sm.DeliveryText != "":
		return "structured_message.delivery_text"
	case sm.ThreadID != "" && (ownDM == "" || sm.ThreadID != ownDM):
		return "structured_message.thread_id"
	}
	return ""
}

// stampVisitorSender returns md with the visitor sender metadata set when
// identity is a *VisitorIdentity: messages.MetadataSenderVisitor "true"
// and messages.MetadataSenderIssuer the issuer's host name
// (visitorIssuerLabel). md must already be stripped of hub-reserved keys
// (messaging.StripReservedMetadata). For any other identity it returns md
// unchanged. It never mutates md.
func stampVisitorSender(md map[string]string, identity Identity) map[string]string {
	visitor, ok := identity.(*VisitorIdentity)
	if !ok || visitor == nil {
		return md
	}
	out := make(map[string]string, len(md)+2)
	for k, v := range md {
		out[k] = v
	}
	out[messages.MetadataSenderVisitor] = "true"
	out[messages.MetadataSenderIssuer] = visitorIssuerLabel(visitor.IssuerURL())
	return out
}

// visitorIssuerLabel returns the display name of a trusted issuer: the
// host of its URL, or the URL itself when it has no host.
func visitorIssuerLabel(issuerURL string) string {
	if u, err := url.Parse(issuerURL); err == nil && u.Host != "" {
		return u.Host
	}
	return issuerURL
}

// userAddressName returns the name of u in a "user:<name>" address: the
// issuer-verified email of a federated account (never its synthetic
// address), otherwise the account email, or the account ID when it has no
// email.
func userAddressName(u *store.User) string {
	if u.IsFederated() && u.FederatedEmail != nil && *u.FederatedEmail != "" {
		return *u.FederatedEmail
	}
	if u.Email != "" {
		return u.Email
	}
	return u.ID
}
