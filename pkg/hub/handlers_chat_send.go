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
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// chatSendInput is the request-independent content of one chat v2 send.
// The HTTP handler (handleConversationSend) decodes it from the request
// body; other callers build it from stored data.
type chatSendInput struct {
	Content     string
	Attachments []string
	ReplyToID   string
	// Metadata is client-supplied metadata; only allowlisted keys reach
	// the agent (see allowedClientMetadataKeys).
	Metadata  map[string]string
	Interrupt bool
	Wake      bool
	OfferWake bool
	// OnPersisted, when set, is called for an agent-routed send right
	// after the row is stored and before dispatch. See
	// chatSendOptions.OnPersisted.
	OnPersisted func(messageID string)
	// BeforeWake, when set, is called right before a suspended primary is
	// woken, with the number of agent recipients. The HTTP handler uses it
	// to extend the connection's write deadline.
	BeforeWake func(recipients int)
}

// chatSendError is a send failure as a value. It carries exactly what the
// HTTP handler writes (status, code, message, details) so that a caller
// without a request can classify the failure instead.
type chatSendError struct {
	Status     int
	Code       string
	Message    string
	Details    map[string]interface{}
	RetryAfter time.Duration
}

func (e *chatSendError) Error() string {
	return fmt.Sprintf("chat send: %d %s: %s", e.Status, e.Code, e.Message)
}

// write writes the error as the HTTP response the send handler returns.
func (e *chatSendError) write(w http.ResponseWriter) {
	if e.RetryAfter > 0 {
		WriteAgentDMError(w, &AgentDMError{
			Code: e.Code, Message: e.Message, Details: e.Details,
			HTTPStatus: e.Status, RetryAfter: e.RetryAfter,
		})
		return
	}
	writeError(w, e.Status, e.Code, e.Message, e.Details)
}

func newChatSendError(status int, code, message string, details map[string]interface{}) *chatSendError {
	return &chatSendError{Status: status, Code: code, Message: message, Details: details}
}

// The constructors below mirror the HTTP helpers of the same name in
// errors.go, so the handler's responses are unchanged.

func chatSendBadRequest(message string) *chatSendError {
	return newChatSendError(http.StatusBadRequest, ErrCodeInvalidRequest, message, nil)
}

func chatSendValidationError(message string) *chatSendError {
	return newChatSendError(http.StatusBadRequest, ErrCodeValidationError, message, nil)
}

func chatSendForbidden() *chatSendError {
	return newChatSendError(http.StatusForbidden, ErrCodeForbidden, "Insufficient permissions", nil)
}

func chatSendNotFound(resource string) *chatSendError {
	return newChatSendError(http.StatusNotFound, ErrCodeNotFound, resource+" not found", nil)
}

func chatSendFromAgentDMError(e *AgentDMError) *chatSendError {
	return &chatSendError{Status: e.HTTPStatus, Code: e.Code, Message: e.Message, Details: e.Details, RetryAfter: e.RetryAfter}
}

// chatSendTarget is a conversation the sender has been authorized to post
// to by authorizeChatSend.
type chatSendTarget struct {
	Key  string
	IsDM bool
	// ProjectID is the topic's current project; empty for DMs (resolved
	// later from the key for agent DMs).
	ProjectID string
	// Topic is the topic loaded for authorization (nil for DMs); routing
	// reuses it rather than reading it again.
	Topic *WebChatTopic
	wcs   WebChatStore
}

// chatSendPath is the request path of a live send, used to label
// authorization denial logs for callers that have no request.
func chatSendPath(key string) string {
	return "/api/v1/chat/conversations/" + key + "/messages"
}

// authorizeChatSend runs the conversation access checks of a chat send:
// for a DM, a well-formed key naming the sender as a participant; for a
// topic, an existing topic whose current project exists and grants the
// sender read access. It needs no request, so every caller of
// sendChatMessage runs the same checks.
func (s *Server) authorizeChatSend(ctx context.Context, user UserIdentity, key string) (*chatSendTarget, *chatSendError) {
	if user == nil {
		return nil, chatSendForbidden()
	}
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil {
		return nil, newChatSendError(http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Chat not available", nil)
	}

	target := &chatSendTarget{Key: key, wcs: wcs}
	if strings.HasPrefix(key, "dm:") {
		target.IsDM = true
		// Validate DM key format before any further processing.
		if !validDMKey(key) {
			return nil, chatSendBadRequest("invalid DM key format")
		}
		// DM key: verify the caller is one of the two participants.
		if !isDMParticipant(key, user.ID()) {
			return nil, chatSendForbidden()
		}
		// DMs are not project-scoped; the project is derived from the
		// agent for an agent DM, and user-user DMs have none.
		return target, nil
	}

	// Topic key: look up the topic to get its project and check access.
	topic, err := wcs.GetTopic(ctx, key)
	if err != nil || topic == nil {
		return nil, chatSendNotFound("Thread")
	}
	project, err := s.store.GetProject(ctx, topic.ProjectID)
	if err != nil {
		return nil, chatSendNotFound("Project")
	}
	resource := projectResource(project)
	decision := s.authzService.CheckAccess(ctx, user, resource, ActionRead)
	if !decision.Allowed {
		logReq := (&http.Request{URL: &url.URL{Path: chatSendPath(key)}}).WithContext(ctx)
		logAuthzDenial(logReq, user, resource, ActionRead, decision.Reason)
		details := map[string]interface{}{
			"resource_type": resource.Type,
			"denied_action": string(ActionRead),
		}
		if decision.DeniedBy == DeniedByDelegationCeiling {
			details["denied_by"] = string(DeniedByDelegationCeiling)
		}
		details = addCeilingUnrecordedDetails(details, decision.adoptionDetailsCause())
		return nil, newChatSendError(http.StatusForbidden, ErrCodeForbidden, "Insufficient permissions", details)
	}
	target.ProjectID = topic.ProjectID
	target.Topic = topic
	return target, nil
}

// validateChatSendInput checks the content and attachments of a send and
// returns the trimmed content and the attachment references.
func (s *Server) validateChatSendInput(ctx context.Context, target *chatSendTarget, in chatSendInput) (string, []AttachmentRef, *chatSendError) {
	content := strings.TrimSpace(in.Content)
	if content == "" && len(in.Attachments) == 0 {
		return "", nil, chatSendValidationError("content or attachments required")
	}
	if utf8.RuneCountInString(content) > messages.MaxMessageLength {
		return "", nil, chatSendValidationError(fmt.Sprintf("message exceeds %d character limit", messages.MaxMessageLength))
	}
	if len(in.Attachments) > MaxAttachmentsPerMessage {
		return "", nil, chatSendValidationError(fmt.Sprintf("too many attachments: %d (max %d)", len(in.Attachments), MaxAttachmentsPerMessage))
	}

	// W7: Validate attachment IDs and collect metadata.
	var attachmentRefs []AttachmentRef
	if len(in.Attachments) > 0 && target.wcs != nil {
		for _, aid := range in.Attachments {
			meta, err := target.wcs.GetAttachment(ctx, aid)
			if err != nil || meta == nil {
				return "", nil, chatSendValidationError(fmt.Sprintf("attachment %q not found", aid))
			}
			// Verify the attachment belongs to the correct project.
			if target.ProjectID != "" && meta.ProjectID != target.ProjectID {
				return "", nil, chatSendValidationError(fmt.Sprintf("attachment %q does not belong to this project", aid))
			}
			attachmentRefs = append(attachmentRefs, AttachmentRef{
				ID:       meta.ID,
				Name:     meta.Filename,
				MimeType: meta.MimeType,
				Size:     meta.Size,
			})
		}
	}
	return content, attachmentRefs, nil
}

// sendChatMessage is the single chat v2 send path: conversation access
// check, validation, routing, persistence, publish and dispatch, for a
// message sent by user to the conversation key. It is used by the HTTP
// send handler and by any caller that sends on a user's behalf without a
// request, so they cannot drift apart. Authentication, rate limiting,
// body decoding and the idempotency cache stay with the HTTP handler.
//
// On success it returns the response body of the created message; the
// message is persisted. On failure nothing was persisted unless the
// error says so.
func (s *Server) sendChatMessage(ctx context.Context, user UserIdentity, key string, in chatSendInput) (*chatMessageResponse, *chatSendError) {
	// --- Authorize ---
	target, serr := s.authorizeChatSend(ctx, user, key)
	if serr != nil {
		return nil, serr
	}
	// A DM may only be sent by one of its two participants.
	// authorizeChatSend has checked this; it is checked again here so the
	// check sits in the function that sends user-to-user messages
	// (sendHumanToHuman), as hack/checksecuritymarkergates requires.
	if target.IsDM && !isDMParticipant(key, user.ID()) {
		return nil, chatSendForbidden()
	}

	// --- Validate ---
	content, attachmentRefs, serr := s.validateChatSendInput(ctx, target, in)
	if serr != nil {
		return nil, serr
	}

	isDM := target.IsDM
	projectID := target.ProjectID
	threadTopic := target.Topic

	// --- Resolve routing per design §3 ---
	senderEmail := user.Email()
	senderLabel := senderEmail
	if senderLabel == "" {
		senderLabel = user.ID()
	}

	// Resolve which project we're working in for agent resolution.
	if projectID == "" && isDM {
		projectID = resolveProjectFromDMKey(ctx, s, key)
	}

	// --- Resolve default agent (DM key or topic default) ---
	var defaultAgent *store.Agent
	// unresolvedDefaultAgent is set when the topic names a DefaultAgent that
	// does not resolve to a live agent (soft-deleted, or otherwise missing).
	// It is used below to report "Agent unreachable" instead of silently
	// falling through to a human-to-human message (nc-delivery-unreachable)
	// when no leading @mention overrides it.
	var unresolvedDefaultAgent *store.Agent
	// routingLookupFailed records a transient store error while resolving
	// recipients. The message then cannot be proven agentless, so it is
	// never marked no_recipient.
	routingLookupFailed := false
	if isDM {
		if agentID := parseAgentDMKey(key); agentID != "" {
			if dmAgent, err := s.store.GetAgent(ctx, agentID); err == nil && dmAgent != nil {
				defaultAgent = dmAgent
			}
		}
	} else if projectID != "" {
		topic := threadTopic
		if topic != nil && topic.DefaultAgent != "" {
			da, daErr := s.store.GetAgentBySlug(ctx, projectID, topic.DefaultAgent)
			// foreignProjectDefault stays out of scope here (DEF-31): a
			// default naming a real agent from a different project keeps the
			// existing human-to-human fallthrough rather than "Agent
			// unreachable" — nc-delivery-unreachable is about a default that
			// no longer resolves at all (deleted, or missing), not about
			// cross-project routing.
			foreignProjectDefault := false
			// transientLookupErr (review round 2, nit 1): a store error that
			// is not "not found" — a DB hiccup, not "this agent doesn't
			// exist" — must not be classified the same as a deleted or
			// missing default. Before nc-delivery-unreachable, that hiccup
			// degraded to an ordinary human-to-human message; keep that
			// fallthrough (leave defaultAgent and unresolvedDefaultAgent
			// nil) instead of permanently persisting "Agent unreachable
			// (deleted)" rows for a transient failure.
			transientLookupErr := false
			if daErr != nil && !errors.Is(daErr, store.ErrNotFound) {
				transientLookupErr = true
			} else if daErr != nil || da == nil {
				// Not found by slug — fall back to lookup by ID in case the
				// value is a UUID.
				da, daErr = s.store.GetAgent(ctx, topic.DefaultAgent)
				if daErr != nil && !errors.Is(daErr, store.ErrNotFound) {
					transientLookupErr = true
				} else if daErr == nil && da != nil && (da.ProjectID != projectID || !da.DeletedAt.IsZero()) {
					// Scope the fallback: reject agents from other projects or
					// soft-deleted agents — DEF-31.
					if da.ProjectID == projectID {
						// Same project, soft-deleted: keep the row around so
						// the caller can report "Agent unreachable (deleted)"
						// with the real slug/ID instead of a generic one.
						unresolvedDefaultAgent = da
					} else {
						foreignProjectDefault = true
					}
					da = nil
				}
			}
			routingLookupFailed = routingLookupFailed || transientLookupErr
			if !transientLookupErr {
				if daErr == nil && da != nil {
					defaultAgent = da
				} else if unresolvedDefaultAgent == nil && !foreignProjectDefault {
					// The named default doesn't resolve at all (bad data, or a
					// slug that no longer exists — most commonly because the
					// agent behind it was soft-deleted, which GetAgentBySlug
					// already excludes). Treat the same as deleted for reporting
					// purposes.
					unresolvedDefaultAgent = &store.Agent{Slug: topic.DefaultAgent}
				}
			}
		}
	}

	// --- Reply-to agent override (nc-reply-recipient); see resolveReplyTarget's
	// doc comment for the full rationale. unresolvedDefaultAgent is reused
	// here (see its declaration above) so the existing "Agent unreachable"
	// reporting path below also covers a deleted reply-to sender.
	if replyAgent, replyUnresolved, ok := s.resolveReplyTarget(ctx, key, projectID, in.ReplyToID); ok {
		defaultAgent = replyAgent
		unresolvedDefaultAgent = replyUnresolved
	}

	// --- Resolve routing via shared planner ---
	var plan RoutingPlan
	// planErr, when non-nil, means resolveRoutingAgents itself failed rather
	// than resolving to zero agents. It gates the unresolvedDefaultAgent
	// override below (review round 2, Consider 2): plan.Agents being empty
	// because of a planning error is not evidence the default agent is
	// unreachable, so that case must keep the pre-existing human-to-human
	// fallthrough instead of mislabelling the send "Agent unreachable
	// (deleted)".
	var planErr error
	if projectID != "" {
		plan, planErr = resolveRoutingAgents(ctx, s.store, projectID, content, defaultAgent)
		if planErr != nil {
			routingLookupFailed = true
			slog.Error("agent routing resolution failed", "error", planErr)
			// Fall through: plan.Agents will be empty, triggering human-to-human.
		}
	} else if defaultAgent != nil {
		// No project context but DM default resolved: single-recipient plan.
		plan.Agents = []*store.Agent{defaultAgent}
		plan.MentionNames = messages.ExtractMentions(content)
	}

	now := time.Now().UTC()

	// --- Agent routing ---
	if len(plan.Agents) > 0 {
		// DM registration happens inside sendAgentRouted, before its
		// watermark update — see the comment there.
		return s.sendAgentRouted(ctx, key, projectID, user, content, senderLabel, plan.Agents, plan.MentionNames, plan.MentionResults, attachmentRefs, now, in.ReplyToID, in.Metadata,
			chatSendOptions{Interrupt: in.Interrupt, Wake: in.Wake, OfferWake: in.OfferWake,
				OnPersisted: in.OnPersisted, BeforeWake: in.BeforeWake})
	}

	// --- Unresolvable topic default agent (nc-delivery-unreachable) ---
	// The topic names a default agent that no longer resolves (soft-deleted,
	// or missing) and no leading @mention overrode it (plan.Agents is empty,
	// or resolveRoutingAgents wouldn't have fallen through here). Report
	// "Agent unreachable" rather than silently sending a human-to-human
	// message to the thread. Only do so when resolveRoutingAgents actually
	// succeeded (review round 2, Consider 2): if planErr != nil, the empty
	// plan reflects a routing-plan failure, not the deleted default, so keep
	// the pre-existing human-to-human error handling below instead.
	if unresolvedDefaultAgent != nil && planErr == nil {
		return s.sendHumanToHuman(ctx, key, projectID, user, content, senderLabel, false, false, plan.MentionNames, attachmentRefs, now, in.ReplyToID,
			&unreachableAgentOverride{
				AgentSlug: unresolvedDefaultAgent.Slug,
				AgentID:   unresolvedDefaultAgent.ID,
				Reason:    "Agent unreachable (deleted)",
				Code:      dispatchFailureCodeAgentUnreachable,
			})
	}

	// --- Human-to-human message ---
	// No agent recipient was resolved. A thread message is no_recipient
	// unless a lookup failed or it is addressed to a person.
	members := s.projectMembersOnce(ctx, projectID)
	noRecipient := !isDM && !routingLookupFailed &&
		s.threadMessageUnaddressed(ctx, members, plan.MentionNames, in.ReplyToID, user.ID())
	mentionNames := plan.MentionNames
	if noRecipient {
		// When an agent has posted in the thread, address the reply to the
		// most recent other human poster (or the thread creator) with a
		// note instead of leaving it unseen; no agent is invoked.
		// Otherwise it stays no_recipient.
		creatorID := ""
		if threadTopic != nil {
			creatorID = threadTopic.CreatedBy
		}
		if noted, token, ok := s.unmentionedHumanNote(ctx, members, projectID, key, creatorID, user.ID(), content); ok {
			content = noted
			mentionNames = append(slices.Clone(mentionNames), token)
			noRecipient = false
		}
	}
	return s.sendHumanToHuman(ctx, key, projectID, user, content, senderLabel, isDM, noRecipient, mentionNames, attachmentRefs, now, in.ReplyToID, nil)
}
