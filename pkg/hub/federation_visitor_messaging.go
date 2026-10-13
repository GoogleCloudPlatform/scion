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
	"reflect"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Project visitors messaging agents (ptone/scion#4603): which sub-routes of
// a subtree route admit a visitor, the per-request check that the visitor
// may message the agent a sub-route names, the request fields a visitor may
// use, and the sender metadata the agent sees.

// visitorAccess is the per-request check a visitor sub-route needs
// (admitVisitorSubRoute). The zero value is no check, and a sub-route with
// it refuses every visitor.
type visitorAccess int

const (
	visitorAccessNone visitorAccess = iota
	// visitorAccessMessage: the visitor may message the named agent now
	// (authorizeAgentMessage).
	visitorAccessMessage
	// visitorAccessAgentList: the visitor holds a current grant on the
	// named project and agent.list there (visitorMayListProjectAgents).
	visitorAccessAgentList
	// visitorAccessAgentRead: the visitor holds a current grant on the
	// named agent's project and agent.read on the agent
	// (visitorMayReadAgent).
	visitorAccessAgentRead
)

// visitorAgentRef names the agent (or, on the agent list, the project) a
// visitor sub-route acts on, and the check the sub-route needs.
type visitorAgentRef struct {
	// ProjectID is the project path segment of the project form ({uuid} or
	// {uuid}__{slug}), "" on the other forms.
	ProjectID string
	// AgentID is the agent path segment: an agent ID on the by-ID form, an
	// agent ID or slug on the project form, "" on a DM route and on the
	// agent list.
	AgentID string
	// DMKey is the conversation key of a DM route, "" on the other forms.
	DMKey string
	// Access is the check of the matched sub-route (visitorSubRoute.Access).
	Access visitorAccess
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
	// Access is the per-request check a visitor must pass on the sub-path.
	Access visitorAccess
	// match reports whether a request has this shape and returns the agent
	// reference it names.
	match func(r *http.Request) (visitorAgentRef, bool)
}

// visitorAdmittedSubRoutes is the complete, deny-by-default list of the
// sub-routes of subtree patterns that admit a visitor: sending a message
// to an agent (project and by-ID forms), the visitor's own DM history and
// read marker with an agent, and listing and reading the agents of a
// project the visitor holds a grant on (project form only, answered with
// the visitor view, visitorAgentView). On a subtree route a visitor
// request that matches no entry for the route's pattern and the request's
// method is refused (admitVisitorSubRoute).
var visitorAdmittedSubRoutes = []visitorSubRoute{
	{Pattern: "/api/v1/projects/", Method: http.MethodPost, SubPath: "{projectId}/agents/{agentId}/message",
		Access: visitorAccessMessage, match: matchVisitorAgentSubRoute(ProjectAgentRouteActionMessage)},
	{Pattern: "/api/v1/agents/", Method: http.MethodPost, SubPath: "{agentId}/message",
		Access: visitorAccessMessage, match: matchVisitorAgentSubRoute(AgentRouteActionMessage)},
	{Pattern: "/api/v1/chat/conversations/", Method: http.MethodGet, SubPath: "{dmKey}/messages",
		Access: visitorAccessMessage, match: matchVisitorDMSubRoute("messages")},
	{Pattern: "/api/v1/chat/conversations/", Method: http.MethodGet, SubPath: "{dmKey}/read",
		Access: visitorAccessMessage, match: matchVisitorDMSubRoute("read")},
	{Pattern: "/api/v1/chat/conversations/", Method: http.MethodPost, SubPath: "{dmKey}/read",
		Access: visitorAccessMessage, match: matchVisitorDMSubRoute("read")},
	{Pattern: "/api/v1/projects/", Method: http.MethodGet, SubPath: "{projectId}/agents",
		Access: visitorAccessAgentList, match: matchVisitorProjectAgentsSubRoute(ProjectAgentRouteCollection)},
	{Pattern: "/api/v1/projects/", Method: http.MethodGet, SubPath: "{projectId}/agents/{agentId}",
		Access: visitorAccessAgentRead, match: matchVisitorProjectAgentsSubRoute(ProjectAgentRouteRoot)},
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

// matchVisitorProjectAgentsSubRoute matches the project-form agent
// sub-route id (the agent list or one agent), resolved from the request's
// percent-encoded path exactly as routeGuard resolves it for the handler.
// The returned reference names the project, and the agent on the agent
// route.
func matchVisitorProjectAgentsSubRoute(id AgentSubRouteID) func(r *http.Request) (visitorAgentRef, bool) {
	return func(r *http.Request) (visitorAgentRef, bool) {
		route, ok := ResolveAgentSubRoute(r.Method, r.URL.EscapedPath())
		if !ok || route.RouteID != id || route.ProjectID == "" {
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
// visitorAdmittedSubRoutes for pattern and r's method that r matches, with
// the entry's Access.
func matchVisitorSubRoute(pattern string, r *http.Request) (visitorAgentRef, bool) {
	for _, sub := range visitorAdmittedSubRoutes {
		if sub.Pattern != pattern || sub.Method != r.Method {
			continue
		}
		if ref, ok := sub.match(r); ok {
			ref.Access = sub.Access
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
// visitor may not act on: an unknown project or agent, a deleted agent, or
// a DM key that is not the visitor's own DM with an agent.
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
//   - the entry's Access check passes (below).
//
// The Access checks:
//   - visitorAccessMessage: the entry names an agent that exists and is
//     not deleted (in the named project, on the project form), on a DM
//     route the key is the visitor's own DM with that agent, and the
//     visitor may message the agent now (authorizeAgentMessage: a current
//     project-visitor grant, through agent.message, and the agent's
//     message mode);
//   - visitorAccessAgentList: the named project exists, the visitor holds
//     a current grant on it and agent.list there
//     (visitorMayListProjectAgents);
//   - visitorAccessAgentRead: the named agent exists in the named project
//     and is not deleted, the visitor holds a current grant on that project
//     and agent.read on the agent (visitorMayReadAgent); the agent's
//     message mode does not matter.
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
	switch ref.Access {
	case visitorAccessAgentList:
		allowed, err := s.visitorMayListProjectAgents(r.Context(), visitor, ref.ProjectID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return false
		}
		if !allowed {
			return refuse("unknown project, or no current grant with agent.list")
		}
		return true
	case visitorAccessMessage, visitorAccessAgentRead:
	default:
		return refuse("sub-route has no visitor check")
	}
	agent, err := s.visitorTargetAgent(r.Context(), visitor, ref)
	if errors.Is(err, errVisitorTargetRefused) {
		return refuse("unknown or deleted target, or not the visitor's own DM")
	}
	if err != nil {
		writeErrorFromErr(w, err, "")
		return false
	}
	if ref.Access == visitorAccessAgentRead {
		allowed, err := s.visitorMayReadAgent(r.Context(), visitor, agent)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return false
		}
		if !allowed {
			return refuse("deleted agent, or no current grant with agent.read")
		}
		return true
	}
	if allowed, reason, _ := s.authorizeAgentMessage(r.Context(), visitor, agent, false); !allowed {
		return refuse(reason)
	}
	return true
}

// visitorTargetAgent resolves the agent ref names. A DM key must be the
// visitor's own DM with an agent (visitorDMAgentID). An unknown project or
// agent, a deleted agent (on every form, so a visitor can neither message
// it nor read its DM), or a key that is not the visitor's DM, returns
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
	if agent == nil || !agent.DeletedAt.IsZero() {
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

// visitorRequestFields is the allow-list of the MessageRequest fields, by
// JSON name, a visitor may set. Every other field set to a non-empty value
// is refused (visitorMessageRequestRefusal), including fields added later.
var visitorRequestFields = map[string]bool{
	"message":            true,
	"structured_message": true,
}

// visitorStructuredFields is the allow-list of the StructuredMessage
// fields, by JSON name, a visitor may set. Every other field set to a
// non-empty value is refused, including fields added later.
var visitorStructuredFields = map[string]bool{
	// Checked by value in visitorMessageRequestRefusal.
	"msg":       true,
	"type":      true,
	"recipient": true, // a group recipient is refused; any other value is replaced by the target agent
	// Validated by messaging.ValidateLegacyMessage (the one version).
	"version": true,
	// Set by the hub, whatever the client sends (handleAgentMessage).
	"timestamp": true,
	"sender":    true,
	"sender_id": true,
}

// visitorMessageRequestRefusal returns the name of the first field of req a
// visitor may not use, or "" when req is a direct text message the visitor
// may send. A visitor sends text only, to the one target agent: req may set
// only the fields in visitorRequestFields and its structured message only
// those in visitorStructuredFields, with a type in visitorMessageTypes and
// no group recipient. A refused field is named by its JSON name, prefixed
// with "structured_message." inside the structured message.
func visitorMessageRequestRefusal(req *MessageRequest) string {
	if field := firstFieldOutside(req, visitorRequestFields); field != "" {
		return field
	}
	sm := req.StructuredMessage
	if sm == nil {
		return ""
	}
	if field := firstFieldOutside(sm, visitorStructuredFields); field != "" {
		return "structured_message." + field
	}
	switch {
	case !visitorMessageTypes[sm.Type]:
		return "structured_message.type"
	case messages.IsGroupRecipient(sm.Recipient):
		return "structured_message.recipient"
	}
	return ""
}

// firstFieldOutside returns the JSON name of the first field of the struct
// v points to that is not in allowed and holds a non-empty value (an empty
// slice or map counts as empty), or "" when there is none. Fields a request
// can never set (unexported, or tagged json:"-") are skipped.
func firstFieldOutside(v interface{}, allowed map[string]bool) string {
	rv := reflect.ValueOf(v).Elem()
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		if allowed[name] {
			continue
		}
		fv := rv.Field(i)
		if fv.IsZero() || ((fv.Kind() == reflect.Slice || fv.Kind() == reflect.Map) && fv.Len() == 0) {
			continue
		}
		return name
	}
	return ""
}

// userSenderAddress returns the "user:<name>" sender address of a user
// sender: the account ID for a project visitor (so a reply to the address
// reaches the visitor, see userAddressName), otherwise the email, or the
// account ID when it has no email.
func userSenderAddress(user UserIdentity) string {
	if isVisitorIdentity(user) {
		return "user:" + user.ID()
	}
	if email := user.Email(); email != "" {
		return "user:" + email
	}
	return "user:" + user.ID()
}

// visitorDeliveryConversation returns the conversation ID and DM key a
// message send reports to its sender: for a *VisitorIdentity, convID (the
// conversation the message was persisted in, "" when it was not) and the
// visitor's DM key with agent; for any other identity, "" for both, so
// no other sender's response changes.
func visitorDeliveryConversation(identity Identity, agent *store.Agent, convID string) (string, string) {
	visitor, ok := identity.(*VisitorIdentity)
	if !ok || visitor == nil || agent == nil {
		return "", ""
	}
	key, err := messages.DMConversationKey("agent", agent.ID, "user", visitor.ID())
	if err != nil {
		key = ""
	}
	return convID, key
}

// stampVisitorSender returns md with the visitor sender metadata set when
// identity is a *VisitorIdentity: messages.MetadataSenderVisitor "true",
// messages.MetadataSenderEmail the issuer-verified email,
// messages.MetadataSenderIssuer the issuer's display name
// (visitorIssuerLabel) and messages.MetadataSenderDisplay both together,
// as in "alice@corp.example (visitor via idp.example)". md must already be
// stripped of hub-reserved keys (messaging.StripReservedMetadata). For any
// other identity it returns md unchanged. It never mutates md.
func stampVisitorSender(md map[string]string, identity Identity) map[string]string {
	visitor, ok := identity.(*VisitorIdentity)
	if !ok || visitor == nil {
		return md
	}
	issuer := visitorIssuerLabel(visitor.IssuerURL())
	name := visitor.Email()
	if name == "" {
		name = visitor.ID()
	}
	out := make(map[string]string, len(md)+4)
	for k, v := range md {
		out[k] = v
	}
	out[messages.MetadataSenderVisitor] = "true"
	out[messages.MetadataSenderEmail] = visitor.Email()
	out[messages.MetadataSenderIssuer] = issuer
	out[messages.MetadataSenderDisplay] = name + " (visitor via " + issuer + ")"
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
// account ID of a federated account (a project visitor's address is its
// account ID, never an email, which could name a local account), otherwise
// the account email, or the account ID when it has no email.
func userAddressName(u *store.User) string {
	if u.IsFederated() {
		return u.ID
	}
	if u.Email != "" {
		return u.Email
	}
	return u.ID
}

// visitorRecipientRefusal is the agent-to-user send rule for project
// visitors: agent may send to the user u only when u is not a federated
// account, or when the DM between agent and u already exists. The DM key
// names this agent, and an agent can never create it (this rule refuses
// that send), so it exists only when the visitor started it with this
// agent. Otherwise it returns store.ErrNotFound, so the caller answers
// exactly as for an unknown addressee and a visitor hears only from agents
// it contacted. Any other store error is returned as is.
func (s *Server) visitorRecipientRefusal(ctx context.Context, agent *store.Agent, u *store.User) error {
	if !u.IsFederated() {
		return nil
	}
	key, err := messages.DMConversationKey("agent", agent.ID, "user", u.ID)
	if err != nil {
		return store.ErrNotFound
	}
	if _, err := s.store.GetConversationByExternalRef(ctx, "native", key); err != nil {
		return err
	}
	return nil
}
