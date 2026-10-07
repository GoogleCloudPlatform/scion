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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// sanitizeCrossProjectObserver strips body and attachment content from an
// observer StructuredMessage so that cross-project broker publications do not
// leak payload to unrelated project members. Metadata keys unrelated to
// attachments and artifact references are preserved.
func sanitizeCrossProjectObserver(msg *messages.StructuredMessage) {
	msg.Msg = ""
	msg.Attachments = nil
	msg.ArtifactRefsAdmitted = false
	if msg.Metadata != nil {
		sanitized := make(map[string]string, len(msg.Metadata))
		for k, v := range msg.Metadata {
			if k != attachmentsMetadataKey && k != artifacts.MessageMetadataKey {
				sanitized[k] = v
			}
		}
		msg.Metadata = sanitized
	}
}

// OutboundMessageRequest is the request body for POST /api/v1/agents/{id}/outbound-message.
type OutboundMessageRequest struct {
	Recipient   string            `json:"recipient,omitempty"`
	RecipientID string            `json:"recipient_id,omitempty"`
	Msg         string            `json:"msg"`
	Type        string            `json:"type,omitempty"`
	Urgent      bool              `json:"urgent,omitempty"`
	Attachments []string          `json:"attachments,omitempty"`
	Channel     string            `json:"channel,omitempty"`
	ThreadID    string            `json:"thread_id,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	// ConversationID is an explicit conversation assertion from the caller.
	// When set, the hub authorizes the agent for this conversation and
	// persists the message into it, bypassing the DeriveConversationKey
	// derivation. When empty, derivation from ThreadID or sender/recipient
	// principals applies as before. See DEF-138 §3.1 rules 1-3.
	ConversationID string `json:"conversation_id,omitempty"`
	// ConversationRef is a human-readable conversation reference (DEF-142).
	// Accepted forms: conv:<uuid>, @<agent-slug>, @<email>, #<thread-name>.
	// Mutually exclusive with ConversationID — setting both is a 400.
	// When set, the hub resolves the reference to a ConversationID via
	// messaging.Resolve, then routes through the existing DEF-138 path.
	ConversationRef string `json:"conversation_ref,omitempty"`

	// Wake requests that a suspended target agent be resumed before
	// message delivery (#1691). Only meaningful for agent-to-agent DMs;
	// ignored for user and group recipients (zero resumes invoked).
	Wake bool `json:"wake,omitempty"`

	// This outbound path never accepted a raw field. A "raw" member in the
	// request body is an unknown field and is ignored by JSON decoding; it
	// is not a message-raw ingress (see .design/agent-keys-raw-inventory.md).
}

// deliveryPath identifies how an outbound message should be persisted and dispatched.
type deliveryPath int

const (
	// deliveryAgentDM: agent-to-agent direct message (DEF-164 path).
	// Persists via CreateMessage, dispatches via managedAgentMessage or runtime broker,
	// publishes observer-only event to MessageBrokerProxy.
	deliveryAgentDM deliveryPath = iota

	// deliveryUserBroker: user message routed through MessageBrokerProxy.
	// Persistence and SSE publishing handled by the broker's deliverToUser callback.
	deliveryUserBroker

	// deliveryUserDirect: user message persisted and dispatched directly.
	// Persists via CreateMessage, publishes SSE via events.PublishUserMessage,
	// dispatches via ChannelRegistry. Used when MessageBrokerProxy is unavailable.
	deliveryUserDirect
)

// routingResult captures all addressing, conversation, and transport decisions
// made by resolveOutboundRouting. The handler uses this result to build the
// message envelope, persist, and dispatch.
type routingResult struct {
	// Recipient addressing (S1 output, or S5 DEF-152 derivation).
	Recipient   string // Wire format: "user:<name>" or "agent:<slug>"
	RecipientID string // UUID

	// Conversation (S3 + S4 output).
	ConversationID string
	// Asserted is true only when the caller supplied an explicit conversation_id
	// and it was authorized (DEF-138 Rule 1). False for derived conversations (Rules 2/3).
	Asserted bool
	// ConvResult holds the full conversation metadata when S4 resolved or
	// created a conversation. Nil if conversation resolution failed (write-deny
	// disabled) or was skipped.
	ConvResult *messaging.ConversationResult

	// Transport (S2 + S6 output).
	Channel  string
	ThreadID string

	// Delivery path selector.
	DeliveryPath deliveryPath

	// Agent-to-agent specific (DEF-164). Non-nil only when DeliveryPath == deliveryAgentDM.
	TargetAgent *store.Agent

	// Group addressing metadata (S6 DEF-160).
	// Recipients is the JSON-encoded map of participant IDs for group messages.
	Recipients string
	GroupID    string

	// Provenance flags for downstream logic.
	// ConvRefResolved tracks whether the request entered via conversation_ref (DEF-160).
	ConvRefResolved bool
	// Def152DerivedRecipient tracks whether S5 derived the addressee from the conversation.
	Def152DerivedRecipient bool
}

// outboundThreadState is the result of outboundThreadConversationState.
type outboundThreadState int

const (
	// outboundThreadMissing: nothing the resolver would reuse exists, so it
	// would mint a new, participant-less group conversation.
	outboundThreadMissing outboundThreadState = iota
	// outboundThreadExists: a live webchat topic or a native conversation
	// with the thread's external_ref exists and will be reused.
	outboundThreadExists
	// outboundThreadDeleted: the thread is a webchat topic that has been
	// soft-deleted. The resolver would reuse its conversation, but nobody can
	// see it any more.
	outboundThreadDeleted
)

// outboundThreadGateApplies reports whether the ptone/scion#2026
// existing-thread check applies to an outbound send on channel (after S2
// reply affinity). It applies only to native delivery: the web channel or no
// channel. External channel plugins (Slack, Teams, Google Chat, ...) deliver
// to the ThreadID themselves, so for them the thread need not exist as a
// conversation on the Hub. ChannelToSurfaceStrict is used rather than
// ChannelToSurface because the latter maps unknown channel names (plugins
// can be registered under any name) to "native".
//
// An empty channel is gated on purpose, even though on a broker deployment
// with no reply affinity it can fan out to plugin spokes (Slack and Teams
// accept an empty channel and deliver to the ThreadID). A direct API caller
// sending a plugin-format thread_id with no channel therefore gets a 422 and
// must name the channel. The CLI cannot reach this case: --thread-id
// requires --channel.
func outboundThreadGateApplies(channel string) bool {
	surface, err := messaging.ChannelToSurfaceStrict(channel)
	return err == nil && surface == "native"
}

// outboundThreadConversationState reports whether a free-text thread key
// (extRef = "thread:<project>:<threadID>") names a conversation that
// ResolveOrCreateConversationByKey would reuse rather than mint, and whether
// that conversation is still visible:
//
//   - a live webchat topic for threadID (when tl is set): exists. A topic that
//     has no conversation_id yet also counts; the resolver refuses to mint
//     for it and reports its own error.
//   - a soft-deleted webchat topic for threadID: deleted. This is stricter
//     than the resolver, which reuses a deleted topic's conversation.
//   - otherwise, a native conversation whose external_ref is extRef: exists.
//   - otherwise: missing.
//
// Lookup failures other than "not found" are returned as errors.
//
// The check and the resolver's upsert are not atomic: if the conversation is
// deleted in between, the resolver mints a fresh row. The window is tiny and
// needs a concurrent deletion, so it is accepted.
func outboundThreadConversationState(
	ctx context.Context,
	cr messaging.ConversationReader,
	tl messaging.TopicConversationLookup,
	extRef, threadID string,
) (outboundThreadState, error) {
	if tl != nil {
		_, err := tl.GetTopicConversationID(ctx, threadID)
		if err == nil {
			return outboundThreadExists, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return outboundThreadMissing, fmt.Errorf("topic lookup: %w", err)
		}
		_, err = tl.GetTopicConversationIDIncludingDeleted(ctx, threadID)
		if err == nil {
			return outboundThreadDeleted, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return outboundThreadMissing, fmt.Errorf("topic lookup (including deleted): %w", err)
		}
	}
	if _, err := cr.GetConversationByExternalRef(ctx, "native", extRef); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return outboundThreadMissing, nil
		}
		return outboundThreadMissing, fmt.Errorf("conversation lookup: %w", err)
	}
	return outboundThreadExists, nil
}

// resolveOutboundRouting consolidates recipient resolution, conversation
// authorization, and transport selection into a single routing decision.
// Returns a routingResult that the handler uses to build the message envelope
// and select the dispatch path, or an error with an HTTP status code.
//
// Encapsulates stages S1-S6 from the original inline implementation:
//
//	S1: Recipient resolution (UUID/email lookup)
//	S2: Channel affinity + validation
//	S3: ConversationRef resolution (DEF-142)
//	S4: Conversation authorization (DEF-138)
//	S5: Addressee derivation (DEF-152)
//	S6: Group/direct routing fixups (DEF-160/161/158)
func (s *Server) resolveOutboundRouting(
	ctx context.Context,
	w http.ResponseWriter,
	req *OutboundMessageRequest,
	agent *store.Agent,
) (*routingResult, error) {
	result := &routingResult{}

	// ─────────────────────────────────────────────────────────────────────────
	// S1: Recipient Resolution (UUID/email lookup)
	// ─────────────────────────────────────────────────────────────────────────
	recipientID := req.RecipientID
	recipient := req.Recipient

	if recipientID == "" && recipient != "" {
		// Explicit recipient string provided without an ID — resolve the user.
		// Accept "user:<identifier>" or bare "<identifier>".
		identifier := strings.TrimPrefix(recipient, "user:")

		// Exact resolution only — UUID or email (DEF-126 P2).
		// Display-name substring matching is removed.
		if _, parseErr := uuid.Parse(identifier); parseErr == nil {
			// Token is a UUID — direct lookup by primary key.
			u, err := s.store.GetUser(ctx, identifier)
			if err == nil {
				recipientID = u.ID
				name := u.Email
				if name == "" {
					name = u.ID
				}
				recipient = "user:" + name
			} else if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusBadRequest, ErrCodeAddrUnknown,
					fmt.Sprintf("user:%s is not a valid addressee. No user exists with that ID.", identifier), nil)
				return nil, err
			} else {
				s.messageLog.Error("user lookup by ID failed", "identifier", identifier, "error", err)
				writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
					"user lookup failed due to an internal error", nil)
				return nil, err
			}
		} else if strings.Contains(identifier, "@") {
			// Token contains @ — exact email lookup, case-folded.
			u, err := s.store.GetUserByEmail(ctx, identifier)
			if err == nil {
				recipientID = u.ID
				name := u.Email
				if name == "" {
					name = u.ID
				}
				recipient = "user:" + name
			} else if errors.Is(err, store.ErrNotSingular) {
				writeError(w, http.StatusBadRequest, ErrCodeAddrAmbiguous,
					fmt.Sprintf("user:%s is not a valid addressee. Multiple users match that email; resolve the duplicate before sending.", identifier), nil)
				return nil, err
			} else if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusBadRequest, ErrCodeAddrUnknown,
					fmt.Sprintf("user:%s is not a valid addressee. No user exists with that email.", identifier), nil)
				return nil, err
			} else {
				s.messageLog.Error("user lookup by email failed", "identifier", identifier, "error", err)
				writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
					"user lookup failed due to an internal error", nil)
				return nil, err
			}
		} else {
			// Token is neither a UUID nor an email — refuse.
			err := fmt.Errorf("malformed address")
			writeError(w, http.StatusBadRequest, ErrCodeAddrMalformed,
				fmt.Sprintf("user:%s is not a valid addressee. Address a user by exact email (user:name@example.com) or by id. Names are not unique and cannot be resolved.", identifier), nil)
			return nil, err
		}
	} else if recipientID != "" && req.ConversationID == "" && req.ConversationRef == "" {
		// A25.8 R1 (p2a-u2 review): a caller-supplied recipient_id is raw
		// payload, never resolved by the UUID/email branch above (which only
		// runs when recipientID starts empty). Left unvalidated, it flows
		// straight into DeriveConversationKey as RecipientKind:"user" (Rules
		// 2/3 below) and then into the new A25.6/A25.7 participant
		// registration — an agent could supply another agent's UUID as
		// recipient_id and the hub would write a "user:<that-agent's-uuid>"
		// participant row for a principal that was never a user. Resolve it
		// with GetUser before any key derivation or write, exactly like the
		// UUID arm above.
		//
		// Scoped to skip when ConversationID/ConversationRef is set (DEF-138
		// Rule 1, A25.9 spec correction to A25.8 R1): recipientID is never
		// used to derive a key, create a conversation, or register
		// participants on this path; the asserted conversation is
		// authorized independently in S4. recipientID can legitimately be
		// an agent's ID there (TestOutboundDMAuthz_ConversationID_
		// Allowed_Persisted for ConversationID; TestHandleAgentOutboundMessage_
		// A259_R1_ConversationRef_AgentPeer_Allowed for ConversationRef),
		// and requiring it to be a user would break those paths for no
		// security benefit.
		u, err := s.store.GetUser(ctx, recipientID)
		if err == nil {
			recipientID = u.ID
			if recipient == "" {
				name := u.Email
				if name == "" {
					name = u.ID
				}
				recipient = "user:" + name
			}
		} else if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusBadRequest, ErrCodeAddrUnknown,
				fmt.Sprintf("recipient_id %s is not a valid addressee. No user exists with that ID.", recipientID), nil)
			return nil, err
		} else {
			s.messageLog.Error("user lookup by recipient_id failed", "recipient_id", recipientID, "error", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"user lookup failed due to an internal error", nil)
			return nil, err
		}
	}

	// DEF-152: relax the guard so that a request carrying a conversation_ref
	// (but no explicit recipient) can reach the resolver. The resolver derives
	// the addressing from the conversation itself. Requests with NEITHER a
	// recipient NOR a conversation_ref are still rejected.
	if recipientID == "" && recipient == "" && req.ConversationRef == "" {
		err := fmt.Errorf("recipient required")
		ValidationError(w, "recipient is required — use 'user:<email>' or 'user:<id>' for a user, '@<agent>' for an agent, or 'conv:<id>' for a conversation", nil)
		return nil, err
	}

	// Ownership check: verify the DM key IDs match the actual sender (agent)
	// and recipient (user). Only validate when recipientID is known — when
	// addressee derivation (S5) is needed, recipientID is still empty here.
	// F4: Guard added intentionally — when recipientID is empty (conv-ref path),
	// the ownership check would erroneously fail. S5 derives the recipient later
	// and ParseDMKey in S5 validates the key.
	if req.ThreadID != "" && strings.HasPrefix(req.ThreadID, "dm:") && recipientID != "" {
		dmAgentID, dmUserID := parseDMKeyIDs(req.ThreadID)
		if dmAgentID != agent.ID || dmUserID != recipientID {
			err := fmt.Errorf("DM thread_id mismatch")
			BadRequest(w, "DM thread_id does not match the sender and recipient")
			return nil, err
		}
	}

	// ─────────────────────────────────────────────────────────────────────────
	// S2: Channel Affinity + Validation
	// ─────────────────────────────────────────────────────────────────────────
	// Reply affinity: when the agent sends an untagged reply (no explicit channel),
	// check webchat_conversation_context for the (recipient, project, agent) triple.
	// If a row exists, route to the channel the user last spoke from.
	s.mu.RLock()
	wcsAffinity := s.webChatStore
	s.mu.RUnlock()
	if req.Channel == "" && req.ConversationRef == "" && recipientID != "" && wcsAffinity != nil && s.GetMessageBrokerProxy() != nil {
		if lastCh, err := wcsAffinity.GetLastChannel(ctx, recipientID, agent.ProjectID, agent.ID); err != nil {
			s.messageLog.Error("Failed to look up reply affinity",
				"recipient_id", recipientID, "agent_id", agent.ID, "error", err)
			// Non-fatal: fall through to fan-out-to-all behavior.
		} else if lastCh != "" {
			req.Channel = lastCh
		}
	}

	// Validate channel against registered channels.
	if !s.validateChannelRegistered(w, req.Channel) {
		return nil, fmt.Errorf("channel validation failed")
	}

	// Validate the message envelope after S1-S2 resolution but before
	// conversation resolution (DEF-16: validation must run before creating
	// a conversation row). We validate using the S1-resolved recipient values.
	validationMsg := &messages.StructuredMessage{
		Sender:         "agent:" + agent.Slug,
		SenderID:       agent.ID,
		Recipient:      recipient,
		RecipientID:    recipientID,
		Msg:            req.Msg,
		Type:           req.Type,
		Urgent:         req.Urgent,
		Attachments:    req.Attachments,
		Channel:        req.Channel,
		ThreadID:       req.ThreadID,
		Metadata:       req.Metadata,
		ConversationID: req.ConversationID,
	}
	if err := messaging.ValidateLegacyMessage(validationMsg); err != nil {
		ValidationError(w, err.Error(), nil)
		return nil, err
	}

	// ─────────────────────────────────────────────────────────────────────────
	// S3: ConversationRef Resolution (DEF-142)
	// ─────────────────────────────────────────────────────────────────────────
	// Resolve ConversationRef → ConversationID before the DEF-138 routing block.
	// The resolved ID then flows through the existing explicit authorization path.
	convRefResolved := false
	if req.ConversationRef != "" {
		authKind, authID := authenticatedSender(ctx)
		if authKind == "" || authID == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
				"authenticated identity required for conversation_ref", nil)
			return nil, fmt.Errorf("authentication required")
		}
		resolveResult, resolveErr := messaging.Resolve(ctx, s.store, req.ConversationRef, messaging.ResolveContext{
			SenderPrincipalKind: authKind,
			SenderPrincipalID:   authID,
			ProjectID:           agent.ProjectID, // from the authenticated agent, NOT from the request
		})
		if resolveErr != nil {
			var resErr *messaging.ResolutionError
			if errors.As(resolveErr, &resErr) {
				// DEF-142 AC-3: disclosure decision delegates to the allowlist.
				if disclosableResolutionReason(resErr.Reason) {
					writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
						"conversation_ref resolution failed: "+resErr.Error(), nil)
				} else {
					s.messageLog.Info("DEF-142: conversation_ref resolution denied",
						"conversation_ref", req.ConversationRef,
						"reason", resErr.Reason,
					)
					writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
						"conversation_ref could not be resolved", nil)
				}
				return nil, resolveErr
			}
			// ParseReference returns store.ErrInvalidInput for malformed refs.
			if errors.Is(resolveErr, store.ErrInvalidInput) {
				writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
					"conversation_ref resolution failed: "+resolveErr.Error(), nil)
				return nil, resolveErr
			}
			s.messageLog.Error("DEF-142: Resolve failed for conversation_ref",
				"conversation_ref", req.ConversationRef,
				"error", resolveErr,
			)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"conversation reference resolution failed", nil)
			return nil, resolveErr
		}
		// Promote to ConversationID so the existing DEF-138 authorization
		// block handles it identically to a caller-supplied UUID.
		req.ConversationID = resolveResult.ConversationID
		convRefResolved = true
	}

	// ─────────────────────────────────────────────────────────────────────────
	// S4: Conversation Authorization (DEF-138)
	// ─────────────────────────────────────────────────────────────────────────
	// DEF-138 §3.1 conversation routing rules:
	//   Rule 1: Caller named a conversation → authorize it, then use it.
	//   Rule 2: Caller named a thread       → derive thread:{project}:{thread}.
	//   Rule 3: Caller named only principals → derive dm:{kind}:{id}:{kind}:{id}.
	var convResult *messaging.ConversationResult
	var asserted bool
	if req.ConversationID != "" {
		// Rule 1: explicit conversation assertion from the caller.
		authKind, authID := authenticatedSender(ctx)
		if authKind == "" || authID == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
				"authenticated identity required for caller-supplied conversation_id", nil)
			return nil, fmt.Errorf("authentication required")
		}

		conv, convErr := s.store.GetConversation(ctx, req.ConversationID)
		if convErr != nil {
			if errors.Is(convErr, store.ErrNotFound) {
				writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
					"caller-supplied conversation_id does not exist", nil)
				return nil, convErr
			}
			s.messageLog.Error("DEF-138: GetConversation failed for caller-supplied conversation_id",
				"conversation_id", req.ConversationID,
				"error", convErr,
			)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"conversation lookup failed", nil)
			return nil, convErr
		}
		if conv == nil {
			err := fmt.Errorf("conversation not found")
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
				"caller-supplied conversation_id does not exist", nil)
			return nil, err
		}

		// Authority differs by conversation kind. The DM key IS the ACL
		// for direct conversations; group conversations are scoped by
		// project containment.
		switch conv.Kind {
		case "direct":
			if err := messages.CheckDMParticipantKey(conv.Kind, conv.ExternalRef, authKind, authID); err != nil {
				s.messageLog.Warn("DEF-138: direct conversation authorization failed (outbound)",
					"conversation_id", conv.ID,
					"auth_kind", authKind,
					"auth_id", authID,
					"error", err,
				)
				writeError(w, http.StatusForbidden, ErrCodeForbidden,
					"authenticated sender is not a participant in the direct conversation", nil)
				return nil, err
			}
		case "group":
			// Deny when either project ID is unset (empty or zero UUID).
			const zeroUUID = "00000000-0000-0000-0000-000000000000"
			convProjUnset := conv.ProjectID == nil || *conv.ProjectID == "" || *conv.ProjectID == zeroUUID
			agentProjUnset := agent.ProjectID == "" || agent.ProjectID == zeroUUID
			if convProjUnset || agentProjUnset || *conv.ProjectID != agent.ProjectID {
				s.messageLog.Warn("DEF-138: group conversation project mismatch or unset project (outbound)",
					"conversation_id", conv.ID,
					"conv_project_id", conv.ProjectID,
					"agent_project_id", agent.ProjectID,
				)
				writeError(w, http.StatusForbidden, ErrCodeForbidden,
					"conversation does not belong to the agent's project", nil)
				return nil, fmt.Errorf("project mismatch")
			}

			// Auto-register sender as participant (listing concern, non-fatal).
			// Group conversation authorization is project-based, not participant-based,
			// so this only ensures the conversation appears in the sender's listing.
			if authKind, authID := authenticatedSender(ctx); authID != "" {
				if ensureErr := s.store.EnsureParticipant(ctx, &store.ConversationParticipant{
					ConversationID: req.ConversationID,
					PrincipalKind:  authKind,
					PrincipalID:    authID,
					Role:           "member",
				}); ensureErr != nil {
					s.messageLog.Warn("auto-register sender as participant failed (listing gap, not access)",
						"conversation_id", req.ConversationID,
						"principal_kind", authKind,
						"principal_id", authID,
						"error", ensureErr)
				}
			}
		default:
			// Unknown conversation kind — fail closed.
			s.messageLog.Warn("DEF-138: unknown conversation kind, denying (outbound)",
				"conversation_id", conv.ID,
				"kind", conv.Kind,
			)
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"unsupported conversation kind", nil)
			return nil, fmt.Errorf("unknown kind")
		}

		// Authorization passed — honour the caller's assertion.
		asserted = true
		convResult = &messaging.ConversationResult{
			ConversationID: req.ConversationID,
			ExternalRef:    conv.ExternalRef,
			Kind:           conv.Kind,
			Surface:        conv.Surface,
			DisplayName:    conv.DisplayName,
		}
	} else {
		// Rules 2/3: derive conversation from the caller's own address.
		extRef, kind, projID, deriveErr := messaging.DeriveConversationKey(messaging.KeyInputs{
			ThreadID:      req.ThreadID,
			ProjectID:     agent.ProjectID,
			SenderKind:    "agent",
			SenderID:      agent.ID,
			RecipientKind: "user",
			RecipientID:   recipientID,
		})
		if deriveErr != nil {
			if s.writeDenyEnabled() {
				messaging.WriteDenialMetrics.Inc("outbound.derive")
				writeError(w, http.StatusConflict, ErrCodeConversationNotResolved,
					"conversation key derivation failed: "+deriveErr.Error(), nil)
				return nil, deriveErr
			}
			s.messageLog.Warn("skipping conversation resolution: key derivation refused (write-deny OFF)",
				"thread_id", req.ThreadID, "agent_id", agent.ID, "error", deriveErr)
		} else {
			var keyOpts []messaging.ConversationByKeyOption
			s.mu.RLock()
			wcs := s.webChatStore
			s.mu.RUnlock()
			// #2026: on native delivery (web or no channel), a free-text
			// (non-dm:) thread_id may only address a thread conversation that
			// already exists and is visible. Minting one here would create a
			// participant-less group conversation that the recipient never
			// sees, while the sender is told "sent". Reject instead, before
			// any conversation or message row is written. External channels
			// are exempt: their plugin delivers to the ThreadID itself.
			if kind == "group" && outboundThreadGateApplies(req.Channel) {
				var tl messaging.TopicConversationLookup
				if wcs != nil {
					tl = wcs
				}
				state, stateErr := outboundThreadConversationState(ctx, s.store, tl, extRef, req.ThreadID)
				if stateErr != nil {
					s.messageLog.Error("thread conversation lookup failed",
						"thread_id", req.ThreadID, "agent_id", agent.ID, "error", stateErr)
					writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
						"thread conversation lookup failed", nil)
					return nil, stateErr
				}
				switch state {
				case outboundThreadMissing:
					writeError(w, http.StatusUnprocessableEntity, ErrCodeUnprocessable,
						fmt.Sprintf("thread_id %q does not match an existing conversation; "+
							"address the conversation with conv:<uuid> (see 'scion conversation list'), "+
							"or omit thread_id to message the recipient directly", req.ThreadID), nil)
					return nil, fmt.Errorf("thread %q does not resolve to an existing conversation", req.ThreadID)
				case outboundThreadDeleted:
					writeError(w, http.StatusUnprocessableEntity, ErrCodeUnprocessable,
						fmt.Sprintf("thread_id %q refers to a deleted conversation; "+
							"address an active conversation with conv:<uuid> (see 'scion conversation list'), "+
							"or omit thread_id to message the recipient directly", req.ThreadID), nil)
					return nil, fmt.Errorf("thread %q refers to a deleted conversation", req.ThreadID)
				}
			}
			if wcs != nil {
				keyOpts = append(keyOpts, messaging.WithKeyTopicLookup(wcs))
			}
			// A25.6 F1/F3: register both DM principals as participants so
			// the conversation is discoverable via `conversation list`.
			keyOpts = append(keyOpts, messaging.WithParticipants(s.store))
			var convErr error
			convResult, convErr = messaging.ResolveOrCreateConversationByKey(ctx, s.store, s.messageLog, extRef, kind, projID, keyOpts...)
			if convErr != nil {
				if s.writeDenyEnabled() {
					messaging.WriteDenialMetrics.Inc("outbound.resolve")
					s.messageLog.Error("conversation resolution failed", "error", convErr)
					writeError(w, http.StatusConflict, ErrCodeConversationNotResolved, "conversation resolution failed", nil)
					return nil, convErr
				}
				s.messageLog.Warn("conversation resolution failed (write-deny OFF, continuing)", "error", convErr)
			} else {
				if err := messaging.ValidateAttributed(convResult.ConversationID); err != nil {
					if s.writeDenyEnabled() {
						messaging.WriteDenialMetrics.Inc("outbound.validate")
						writeError(w, http.StatusConflict, ErrCodeConversationNotResolved, err.Error(), nil)
						return nil, err
					}
					s.messageLog.Warn("ValidateAttributed failed (write-deny OFF, continuing)", "error", err)
				}
			}
		}
	}

	// ─────────────────────────────────────────────────────────────────────────
	// S5: Addressee Derivation (DEF-152)
	// ─────────────────────────────────────────────────────────────────────────
	// When a conversation_ref resolved without an explicit recipient, derive the
	// addressee from the resolved conversation.
	def152DerivedRecipient := false
	var targetAgent *store.Agent
	if recipientID == "" && recipient == "" && convResult != nil {
		switch convResult.Kind {
		case "direct":
			// Parse the DM key to identify the other participant.
			kindA, idA, kindB, idB, parseErr := messages.ParseDMKey(convResult.ExternalRef)
			if parseErr != nil {
				s.messageLog.Error("DEF-152: cannot parse DM key for addressee derivation",
					"external_ref", convResult.ExternalRef, "error", parseErr)
				writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
					"failed to derive addressee from direct conversation", nil)
				return nil, parseErr
			}
			// The authenticated sender is one side of the DM; the other is the addressee.
			addrKind, addrID, ok := nonSenderDMSide(ctx, kindA, idA, kindB, idB)
			if !ok {
				// Sender is not named in the DM key.
				senderKind, senderID := authenticatedSender(ctx)
				s.messageLog.Error("DEF-152: authenticated sender not found in DM key",
					"auth_kind", senderKind, "auth_id", senderID,
					"external_ref", convResult.ExternalRef)
				writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
					"failed to derive addressee: sender not found in conversation", nil)
				return nil, fmt.Errorf("sender not in DM key")
			}
			if addrKind == "agent" {
				// DEF-164: agent-to-agent delivery.
				// Look up the target and signal deliveryAgentDM path.
				var agentLookupErr error
				targetAgent, agentLookupErr = s.store.GetAgent(ctx, addrID)
				if agentLookupErr != nil {
					s.messageLog.Error("DEF-164: target agent lookup failed",
						"addr_id", addrID, "error", agentLookupErr)
					writeErrorFromErr(w, agentLookupErr, "")
					return nil, agentLookupErr
				}
				recipient = "agent:" + targetAgent.Slug
				recipientID = targetAgent.ID
				def152DerivedRecipient = true
			} else if addrKind != "user" {
				// The other participant is neither a user nor an agent — fail closed.
				writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
					"conversation_ref resolved to a non-user addressee; "+
						"this endpoint delivers to users only", nil)
				return nil, fmt.Errorf("non-user addressee")
			} else {
				// addrKind == "user"
				u, lookupErr := s.store.GetUser(ctx, addrID)
				if lookupErr != nil {
					s.messageLog.Error("DEF-152: user lookup for derived addressee failed",
						"addr_id", addrID, "error", lookupErr)
					writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
						"failed to look up derived addressee", nil)
					return nil, lookupErr
				}
				recipientID = u.ID
				name := u.Email
				if name == "" {
					name = u.ID
				}
				recipient = "user:" + name
				def152DerivedRecipient = true
			}
		case "group":
			// Group conversations have no single recipient. Fall through to S6
			// which will set up thread-based addressing.
		default:
			// Unknown conversation kind with no explicit recipient — fail
			// closed rather than guessing.
			err := fmt.Errorf("cannot derive addressee for conversation of kind %q", convResult.Kind)
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, err.Error(), nil)
			return nil, err
		}
	}

	// ─────────────────────────────────────────────────────────────────────────
	// S6: Group/Direct Routing Fixups (DEF-160/161/158)
	// ─────────────────────────────────────────────────────────────────────────
	// This stage handles the conv-ref path's transport selection and recipient
	// patching for group and direct conversations.
	var recipients string // JSON-encoded map for group messages
	var groupID string

	if convRefResolved {
		if convResult == nil {
			// ConversationRef was resolved (S3), but conversation resolution
			// failed (S4). This can happen when write-deny is disabled.
			// Log and continue — downstream persistence will also skip.
			s.messageLog.Warn("DEF-160: conv-ref resolved but conversation not resolved; skipping S6 fixups")
		} else {
			switch convResult.Kind {
			case "group":
				// DEF-160/161: group conv-ref routing. Three cases:
				//  1. "thread:"-prefixed ExternalRef → legacy group conversation
				//     with a thread key derived from the external ref.
				//  2. Empty ExternalRef → native group conversation (created via
				//     the conversation API); route by ConversationID alone.
				//  3. Any other non-empty ExternalRef → unexpected format; fail
				//     closed rather than silently misrouting.
				if strings.HasPrefix(convResult.ExternalRef, "thread:") {
					// Legacy path: parse the external ref to extract the thread ID.
					_, threadID, parseErr := messaging.ParseThreadConversationExternalRef(convResult.ExternalRef)
					if parseErr != nil {
						s.messageLog.Error("DEF-160: cannot parse thread external_ref",
							"external_ref", convResult.ExternalRef, "error", parseErr)
						writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
							"conversation has unparseable external_ref", nil)
						return nil, parseErr
					}

					// DEF-161 (group half): if caller supplied a user recipient, log and
					// discard it. The thread key is the address, not a user.
					if recipient != "" || recipientID != "" {
						s.messageLog.Info("DEF-161: discarding caller-supplied recipient on group conv-ref — thread key is the address",
							"supplied_recipient", recipient, "supplied_recipient_id", recipientID,
							"thread_key", threadID, "conversation_id", convResult.ConversationID)
					}

					// Overwrite recipient with the thread key.
					recipient = "thread:" + threadID
					recipientID = threadID
					if req.ThreadID == "" {
						req.ThreadID = threadID
					}
				} else if convResult.ExternalRef == "" {
					// Native path: the conversation was created via the native
					// conversation API and has no legacy thread key. Routing is
					// by ConversationID (already set in S4). Discard any
					// caller-supplied user recipient — the conversation is the
					// address, same as the legacy path (DEF-161).
					if recipient != "" || recipientID != "" {
						s.messageLog.Info("DEF-161: discarding caller-supplied recipient on native group conv-ref — conversation is the address",
							"supplied_recipient", recipient, "supplied_recipient_id", recipientID,
							"conversation_id", convResult.ConversationID)
					}
					// Set the recipient to the conversation ID. The persistence
					// layer requires a non-empty recipient (ent schema: NotEmpty),
					// and for native group conversations the conversation itself
					// is the address — mirroring "thread:<id>" for legacy groups.
					recipient = "conv:" + convResult.ConversationID
					recipientID = convResult.ConversationID
				} else {
					// Unexpected ExternalRef format — fail closed rather than
					// silently misrouting to the native path.
					s.messageLog.Error("DEF-160: unexpected group external_ref format",
						"external_ref", convResult.ExternalRef, "conversation_id", convResult.ConversationID)
					writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
						"conversation has unexpected external_ref format", nil)
					return nil, fmt.Errorf("unexpected external_ref format: %s", convResult.ExternalRef)
				}

				// Derive channel from surface when empty (DEF-158).
				// This runs for both legacy and native group conversations.
				if req.Channel == "" {
					derivedCh, derivErr := messaging.SurfaceToChannel(convResult.Surface)
					if derivErr != nil {
						s.messageLog.Error("DEF-158: cannot map surface to channel",
							"surface", convResult.Surface, "error", derivErr)
						writeError(w, http.StatusServiceUnavailable, "channel_derivation_failed",
							"cannot determine delivery channel: conversation surface is unknown or empty", nil)
						return nil, derivErr
					}
					req.Channel = derivedCh
				}

			case "direct":
				// A supplied dm: thread_id must match the conversation's own key.
				if err := s.checkDirectThreadIDMatchesDMKey(w, convResult.ConversationID, convResult.ExternalRef, req.ThreadID); err != nil {
					return nil, err
				}

				// DEF-158: backfill ThreadID with the DM key for direct conversations.
				// F5: validDMKey guard intentionally removed — ParseDMKey in S5 already
				// validated the key format. The DM key cannot reach this point unparsed.
				if req.ThreadID == "" {
					req.ThreadID = convResult.ExternalRef
				}

				// DEF-161 (direct half): an explicit recipient must match the DM
				// key's non-sender participant; do NOT silently overwrite it.
				explicitRecipient := (req.Recipient != "" || req.RecipientID != "") && !def152DerivedRecipient
				if err := s.checkDirectRecipientMatchesDMKey(ctx, w, convResult.ConversationID, convResult.ExternalRef, recipient, recipientID, explicitRecipient); err != nil {
					return nil, err
				}

				// DEF-168: The affinity re-run that was here (DEF-158) has been
				// removed. When a conv-ref is resolved, the conversation's own
				// surface is authoritative — affinity can disagree and cause
				// misrouting (live bug: native DM routed to discord because
				// affinity said "discord" for an unrelated context).

				// Fall back to surface → channel mapping.
				if req.Channel == "" {
					derivedCh, derivErr := messaging.SurfaceToChannel(convResult.Surface)
					if derivErr != nil {
						s.messageLog.Error("DEF-158: cannot map surface to channel",
							"surface", convResult.Surface, "error", derivErr)
						writeError(w, http.StatusServiceUnavailable, "channel_derivation_failed",
							"cannot determine delivery channel: conversation surface is unknown or empty", nil)
						return nil, derivErr
					}
					req.Channel = derivedCh
				}
			}
		}
	}

	// DEF-161 (raw conversation_id path): the direct-half recipient check
	// above only ran when the conversation was named via a resolved
	// conversation_ref (convRefResolved). Apply the identical check when the
	// caller instead asserted a direct conversation directly by
	// conversation_id (Rule 1), so both ways of naming the same conversation
	// enforce the same recipient consistency.
	//
	// convResult != nil is not checked here: asserted is only ever set true
	// together with convResult (see the Rule 1 branch above), so the two are
	// never observed apart. def152DerivedRecipient is not checked either:
	// it is only ever set when the caller supplied no explicit recipient,
	// but this path requires one (a raw conversation_id with no recipient
	// and no conversation_ref is rejected earlier), so it is always false
	// here.
	if !convRefResolved && asserted && convResult.Kind == "direct" {
		// Same dm: thread_id consistency check as the conv-ref path above.
		if err := s.checkDirectThreadIDMatchesDMKey(w, convResult.ConversationID, convResult.ExternalRef, req.ThreadID); err != nil {
			return nil, err
		}

		explicitRecipient := req.Recipient != "" || req.RecipientID != ""
		if err := s.checkDirectRecipientMatchesDMKey(ctx, w, convResult.ConversationID, convResult.ExternalRef, recipient, recipientID, explicitRecipient); err != nil {
			return nil, err
		}
	}

	// Propagate recipients and group_id from metadata for group-set messages.
	if req.Metadata != nil {
		if r, ok := req.Metadata["recipients"]; ok {
			recipients = r
		}
		if gid, ok := req.Metadata["group_id"]; ok {
			groupID = gid
		}
	}

	// Determine delivery path.
	var deliveryPath deliveryPath
	if targetAgent != nil {
		// DEF-164: agent-to-agent message.
		deliveryPath = deliveryAgentDM
	} else if s.GetMessageBrokerProxy() != nil {
		deliveryPath = deliveryUserBroker
	} else {
		deliveryPath = deliveryUserDirect
	}

	// Populate and return the routing result.
	result.Recipient = recipient
	result.RecipientID = recipientID
	if convResult != nil {
		result.ConversationID = convResult.ConversationID
	}
	result.Asserted = asserted
	result.ConvResult = convResult
	result.Channel = req.Channel
	result.ThreadID = req.ThreadID
	result.DeliveryPath = deliveryPath
	result.TargetAgent = targetAgent
	result.Recipients = recipients
	result.GroupID = groupID
	result.ConvRefResolved = convRefResolved
	result.Def152DerivedRecipient = def152DerivedRecipient

	return result, nil
}

// nonSenderDMSide returns the DM key participant that is NOT the
// authenticated sender. This is the canonical "who is the other side of this
// DM" selection: it is used by S5 addressee derivation to pick the addressee,
// and reused below so the direct-conversation recipient check agrees with
// derivation on which half of the key is "the recipient". ok is false when
// the authenticated sender is not named in the key at all (should not happen
// once S4 has authorized the conversation; callers fail closed on !ok).
func nonSenderDMSide(ctx context.Context, kindA, idA, kindB, idB string) (addrKind, addrID string, ok bool) {
	senderKind, senderID := authenticatedSender(ctx)
	switch {
	case kindA == senderKind && idA == senderID:
		return kindB, idB, true
	case kindB == senderKind && idB == senderID:
		return kindA, idA, true
	default:
		return "", "", false
	}
}

// checkDirectRecipientMatchesDMKey applies the ptone/scion#2212 direct-conversation
// recipient check: when the caller supplied an explicit recipient alongside
// an asserted direct conversation, that recipient must match the DM key's
// non-sender participant by ID, and by kind when the recipient carries one —
// the same participant S5 addressee derivation would have picked (see
// nonSenderDMSide). For direct conversations the DM key IS the ACL and is
// derivable — a mismatch is an authorization-shaped error, not a shape
// mismatch. This is shared by both ways a caller can assert a direct
// conversation: a resolved conversation_ref and a raw conversation_id, so the
// two behave identically.
func (s *Server) checkDirectRecipientMatchesDMKey(ctx context.Context, w http.ResponseWriter, conversationID, externalRef, recipient, recipientID string, explicitRecipientSupplied bool) error {
	if !explicitRecipientSupplied {
		return nil
	}
	kindA, idA, kindB, idB, parseErr := messages.ParseDMKey(externalRef)
	if parseErr != nil {
		s.messageLog.Error("DEF-161: cannot parse DM key for recipient validation",
			"external_ref", externalRef, "conversation_id", conversationID, "error", parseErr)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"conversation has an invalid DM key; cannot validate recipient", nil)
		return parseErr
	}
	addrKind, addrID, ok := nonSenderDMSide(ctx, kindA, idA, kindB, idB)
	if !ok {
		s.messageLog.Error("DEF-161: authenticated sender not found in DM key for recipient validation",
			"external_ref", externalRef, "conversation_id", conversationID)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"cannot validate recipient: sender not found in conversation", nil)
		return fmt.Errorf("sender not in DM key")
	}
	// The supplied recipient must match the non-sender participant by ID,
	// and by kind when a kind was supplied — not merely appear somewhere in
	// the key — otherwise a caller could supply their own ID (always "in"
	// the key) or the right ID under the wrong kind, and both would
	// incorrectly read as consistent. A recipient with no kind (recipient_id
	// only) carries no kind to compare, so it cannot be "the wrong kind";
	// the ID comparison alone still rejects the sender's own ID.
	recipientKind, hasKind := messages.PrincipalKindFromAddress(recipient)
	if (hasKind && recipientKind != addrKind) || recipientID != addrID {
		s.messageLog.Warn("DEF-161: supplied recipient does not match the direct conversation's non-sender participant",
			"recipient_kind", recipientKind, "recipient_id", recipientID,
			"dm_key_addr_kind", addrKind, "dm_key_addr_id", addrID,
			"external_ref", externalRef)
		// Both call sites share this single body; do not fork it per path.
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"a recipient may not be supplied with a direct conversation — "+
				"the conversation is the address; remove the recipient and retry", nil)
		return fmt.Errorf("recipient does not match DM key non-sender participant")
	}
	return nil
}

// checkDirectThreadIDMatchesDMKey applies the ptone/scion#2211 direct-conversation
// thread_id check: when the caller supplied a dm:-prefixed thread_id
// alongside an asserted direct conversation, that thread_id must equal the
// conversation's own DM key (ExternalRef) — a mismatched dm: thread_id names
// a different conversation than the one the caller just asserted. A no-op
// when thread_id is empty (still backfilled elsewhere) or not dm:-prefixed
// (some other addressing scheme). Shared by both the conversation_ref and
// the raw conversation_id paths, so the two behave identically.
func (s *Server) checkDirectThreadIDMatchesDMKey(w http.ResponseWriter, conversationID, externalRef, threadID string) error {
	if !strings.HasPrefix(threadID, "dm:") {
		return nil
	}
	if threadID != externalRef {
		s.messageLog.Warn("supplied dm: thread_id does not match the direct conversation's key",
			"thread_id", threadID, "external_ref", externalRef, "conversation_id", conversationID)
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"thread_id does not match the direct conversation's key — "+
				"omit thread_id or use the conversation's own dm: key", nil)
		return fmt.Errorf("thread_id does not match DM key")
	}
	return nil
}

// handleAgentOutboundMessage handles POST /api/v1/agents/{id}/outbound-message.
// Agents use this to send messages to human inboxes. Authenticated via agent
// token (self-access only). An explicit addressee is required — a recipient,
// recipient_id, or conversation_ref; there is no default recipient, and a
// request naming none is rejected with 400 (see resolveOutboundRouting).
func (s *Server) handleAgentOutboundMessage(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	agentIdent := GetAgentIdentityFromContext(ctx)
	if agentIdent == nil {
		Unauthorized(w)
		return
	}
	if agentIdent.ID() != id {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only send outbound messages as themselves", nil)
		return
	}

	var req OutboundMessageRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Type == "" {
		req.Type = "input-needed"
	}

	// The end-of-turn assistant-reply mirror is retired: sciontool no
	// longer sends it, but agents running an older sciontool still do on
	// every turn. Accept and discard it so those agents see success, and
	// nothing is persisted, routed to a channel, or shown in chat.
	if req.Type == messages.TypeAssistantReply {
		writeJSON(w, http.StatusOK, map[string]interface{}{"status": "dropped"})
		return
	}

	if req.Msg == "" {
		ValidationError(w, "msg is required", nil)
		return
	}
	if msgLen := utf8.RuneCountInString(req.Msg); msgLen > messages.MaxMessageLength {
		ValidationError(w, fmt.Sprintf("message exceeds %d character limit (current: %d chars). Consider splitting into multiple messages using multiple scion message invocations", messages.MaxMessageLength, msgLen), nil)
		return
	}

	// Validate DM key format when the thread_id looks like a DM key.
	// Non-DM thread IDs (topic UUIDs, etc.) pass through as-is.
	if req.ThreadID != "" && strings.HasPrefix(req.ThreadID, "dm:") && !validDMKey(req.ThreadID) {
		BadRequest(w, "invalid DM key format")
		return
	}

	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// DEF-142: mutual exclusion check — both ref and id is a client error.
	if req.ConversationRef != "" && req.ConversationID != "" {
		ValidationError(w, "conversation_ref and conversation_id are mutually exclusive — set one or neither", nil)
		return
	}

	// Call resolveOutboundRouting to handle all routing decisions (S1-S6).
	result, err := s.resolveOutboundRouting(ctx, w, &req, agent)
	if err != nil {
		// resolveOutboundRouting already wrote the error response.
		return
	}

	// ── Agent DM path: delegate to shared operation (#1688) ─────────────
	// When routing identified an agent-to-agent DM, or the conversation's DM
	// key names two agents and the recipientID matches, delegate to the
	// shared ExecuteAgentDM operation. This consolidates rate limiting,
	// authorization, attachment rejection, persistence, dispatch, and
	// observer publication into one code path.
	var outboundTargetAgentID string
	if result.DeliveryPath == deliveryAgentDM && result.TargetAgent != nil {
		// Case (a): conversation_ref path — target already resolved.
		outboundTargetAgentID = result.TargetAgent.ID
	} else if result.ConvResult != nil && result.ConvResult.Kind == "direct" &&
		strings.HasPrefix(result.ConvResult.ExternalRef, "dm:") {
		// Case (b): check if the DM key names two agents and the
		// recipientID matches the non-sender side.
		kindA, idA, kindB, idB, parseErr := messages.ParseDMKey(result.ConvResult.ExternalRef)
		if parseErr == nil {
			if kindA == "agent" && kindB == "agent" {
				if idA == agent.ID && idB == result.RecipientID {
					outboundTargetAgentID = idB
				} else if idB == agent.ID && idA == result.RecipientID {
					outboundTargetAgentID = idA
				}
			}
		}
	}

	if outboundTargetAgentID != "" {
		// Re-read the target agent to get its current mode. The record
		// from S5 may come from the same request, but using the fresh
		// store record ensures the mode is current at decision time.
		freshTarget, targetErr := s.store.GetAgent(ctx, outboundTargetAgentID)
		if targetErr != nil {
			s.messageLog.Error("outbound DM: target agent re-read failed",
				"target_id", outboundTargetAgentID, "error", targetErr)
			writeErrorFromErr(w, targetErr, "")
			return
		}

		// Delegate to the shared agent DM operation (#1688).
		dmResult, dmErr := s.ExecuteAgentDM(ctx, &AgentDMInput{
			SenderAgent:    agent,
			SenderIdentity: agentIdent,
			TargetAgent:    freshTarget,
			Msg:            req.Msg,
			Type:           req.Type,
			Urgent:         req.Urgent,
			Attachments:    req.Attachments,
			Metadata:       req.Metadata,
			ConversationID: result.ConversationID,
			ConvResult:     result.ConvResult,
			Asserted:       result.Asserted,
			Channel:        result.Channel,
			ThreadID:       result.ThreadID,
			ProjectID:      agent.ProjectID,
			GroupID:        result.GroupID,
			Wake:           req.Wake,
		})
		if dmErr != nil {
			WriteAgentDMError(w, dmErr)
			return
		}

		// Agent-authored @mention fan-out: the DM branch's primary is
		// freshTarget. Fan-out runs synchronously before the response is
		// written, so an old CLI's own follow-up mention POST (sent right
		// after this response) always finds the row this call just created
		// and is recognized as a duplicate rather than delivered twice.
		// ParentConv is intentionally omitted (nil): this branch is reached
		// only when the parent conversation is a direct (agent-to-agent DM)
		// conversation, which is never reused for a mention regardless of
		// verification, so there is nothing to pass.
		mentionResults := s.fanOutAgentMentions(ctx, agentMentionFanoutInput{
			Sender:          agent,
			SenderIdent:     agentIdent,
			Primary:         freshTarget,
			Msg:             req.Msg,
			Type:            req.Type,
			ParentMessageID: dmResult.MessageID,
			Channel:         result.Channel,
		})
		WriteAgentDMResult(w, dmResult, mentionResults)
		return
	}

	// ── Non-agent-DM paths: rate limit, then build and dispatch ─────────
	// Rate limiting for user delivery paths (agent DMs are rate limited
	// inside ExecuteAgentDM above).
	if !s.allowChatSend(w, agentIdent.ID(), chatSenderAgent) {
		return
	}

	// Agent-authored @mention fan-out extracts mentions from the ORIGINAL
	// body, before translateMentionsInbound rewrites @email tokens to
	// @firstname-lastname for human-facing display below.
	originalMsgForMentions := req.Msg

	// Translate @email mentions to @firstname-lastname for user-facing
	// messages. Resolved once here and handed to fan-out below too — both
	// need the same project human-member list, and it costs one member-list
	// query plus one GetUser per member, so resolving it twice on the same
	// request would double that cost for no benefit.
	var humanMembers []chatMemberEntry
	if agent.ProjectID != "" {
		humanMembers = s.resolveProjectHumanMembers(ctx, agent.ProjectID)
		if len(humanMembers) > 0 {
			req.Msg = translateMentionsInbound(req.Msg, humanMembers)
		}
	}

	// Build storeMsg and structuredMsg from the routing result.
	// deliveryUserDirect below persists this row as the dispatch itself;
	// Ent defaults dispatch_state to "pending" if left unset (nc-promote-busy).
	// A no-op on the deliveryUserBroker path, which persists its own row.
	storeMsg := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      agent.ProjectID,
		Sender:         "agent:" + agent.Slug,
		SenderID:       agent.ID,
		Recipient:      result.Recipient,
		RecipientID:    result.RecipientID,
		Msg:            req.Msg,
		Type:           req.Type,
		Urgent:         req.Urgent,
		AgentID:        agent.ID,
		Channel:        result.Channel,
		ThreadID:       result.ThreadID,
		ConversationID: result.ConversationID,
		GroupID:        result.GroupID,
		DispatchState:  store.MessageDispatchDispatched,
		CreatedAt:      time.Now(),
	}

	// ptone/scion#2100: stamp Version and the row's CreatedAt (RFC3339 UTC)
	// so observers and the legacy envelope see real values, as
	// ExecuteAgentDM does.
	structuredMsg := &messages.StructuredMessage{
		Version:              messages.Version,
		Timestamp:            storeMsg.CreatedAt.UTC().Format(time.RFC3339),
		Sender:               storeMsg.Sender,
		SenderID:             storeMsg.SenderID,
		Recipient:            storeMsg.Recipient,
		RecipientID:          storeMsg.RecipientID,
		Msg:                  storeMsg.Msg,
		Type:                 storeMsg.Type,
		Urgent:               storeMsg.Urgent,
		Attachments:          req.Attachments,
		Channel:              result.Channel,
		ThreadID:             result.ThreadID,
		Metadata:             req.Metadata,
		ConversationID:       result.ConversationID,
		ConversationAsserted: result.Asserted,
		Recipients:           result.Recipients,
	}

	// Backfill native-chat side-effects for agent→user DM messages.
	// When DeriveConversationKey produced a dm: key from the principal pair
	// (Case 3: caller supplied no ThreadID), stamp the fields and registry
	// rows the native-chat subsystem needs to display the message in the
	// DM channel. Without this, the message is persisted but invisible to
	// native-chat (missing Channel/ThreadID, no webchat_dm rows, no SSE
	// DM fan-out, no broker watermark update).
	if result.ConvResult != nil && strings.HasPrefix(result.ConvResult.ExternalRef, "dm:") && storeMsg.ThreadID == "" {
		extRef := result.ConvResult.ExternalRef
		storeMsg.ThreadID = extRef
		structuredMsg.ThreadID = extRef
		// Backfill req.ThreadID so the W6 DM notification guard fires
		// on the non-broker path.
		req.ThreadID = extRef

		// Default Channel to "web" only when no channel was determined
		// by reply affinity or the caller. When affinity has already
		// routed to a different channel (e.g. "discord"), respect that
		// decision — the ThreadID backfill alone is sufficient for the
		// broker's DM registration and watermark paths.
		if storeMsg.Channel == "" {
			storeMsg.Channel = "web"
			structuredMsg.Channel = "web"
		}

		// Create webchat_dm registry rows so the DM appears in the
		// native-chat rail listing (ListDMs query).
		s.mu.RLock()
		dmWcs := s.webChatStore
		s.mu.RUnlock()
		if dmWcs != nil {
			registerDMParticipants(ctx, dmWcs, extRef)
		}
	}

	// Artifact references (ptone/scion#3222): keep only those the sending
	// agent can read under its own request credential. The flag lets
	// deliverToUser record them on the broker path.
	outboundArtifactRefs, outboundArtifactWarning := []artifacts.MessageRef(nil), ""
	structuredMsg.Metadata, outboundArtifactRefs, outboundArtifactWarning = s.admitMessageArtifacts(ctx, structuredMsg.Metadata)
	structuredMsg.ArtifactRefsAdmitted = len(outboundArtifactRefs) > 0

	// Process attachments.
	attachmentRefs := s.ingestAgentAttachments(ctx, agent.ProjectID, agent.ID, req.Attachments)
	if encoded, ok := attachmentRefsMetadata(attachmentRefs); ok {
		if structuredMsg.Metadata == nil {
			structuredMsg.Metadata = make(map[string]string, 1)
		}
		structuredMsg.Metadata[attachmentsMetadataKey] = encoded
	}

	// Dispatch based on delivery path.
	switch result.DeliveryPath {
	case deliveryUserBroker:
		// Broker path: PublishUserMessage handles persistence and SSE.
		if bp := s.GetMessageBrokerProxy(); bp != nil {
			// Ensure the persisting user-message subscription first, as the
			// notification path does: it is otherwise only created on agent
			// lifecycle events.
			persisting := bp.subscribeProjectUserMessages(agent.ProjectID)
			err := bp.PublishUserMessage(ctx, agent.ProjectID, result.RecipientID, structuredMsg)
			if err != nil && persisting && !errors.Is(err, eventbus.ErrSubscriberBufferFull) && !inProcessPublishFailed(err) {
				// The inprocess spoke queued the persisting deliverToUser, so
				// the message is stored; only channel spoke delivery failed (a
				// non-observer spoke returned an error, or no spoke is
				// registered for the channel). Reporting failure would make
				// the sender retry and duplicate the row and the plugin card
				// (ptone/scion#2757). Without the persisting subscription
				// nothing was stored, so the error is still returned.
				s.messageLog.Warn("Outbound message stored; channel spoke delivery failed",
					"agent_id", agent.ID, "recipient_id", result.RecipientID,
					"project_id", agent.ProjectID, "error", err)
				err = nil
			}
			if err != nil {
				s.messageLog.Error("Failed to dispatch outbound message through broker",
					"agent_id", agent.ID, "recipient_id", result.RecipientID,
					"project_id", agent.ProjectID, "persisting", persisting, "error", err)
				if errors.Is(err, eventbus.ErrSubscriberBufferFull) {
					// The in-process bus could not queue this delivery for at
					// least one matching subscriber, normally the per-project
					// persistence subscriber, so hub persistence most likely did
					// not happen. At-least-once caveats (ptone/scion#2311): other
					// fan-out spokes (e.g. an external chat channel) may already
					// have the message, so a retry can duplicate it there; if the
					// drop hit a non-delivering pattern subscriber, the message may
					// in fact be persisted; attachments ingested above are
					// re-ingested on retry.
					writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
						"Message delivery failed: recipient is temporarily overloaded, retry later", nil)
					return
				}
				writeError(w, http.StatusBadGateway, ErrCodeDeliveryFailed,
					"Message delivery failed: "+err.Error(), nil)
				return
			}
			s.messageLog.Info("Outbound message dispatched through broker",
				"agent_id", agent.ID, "recipient_id", result.RecipientID, "project_id", agent.ProjectID)
		}

	case deliveryUserDirect:
		// Direct path: persist, link attachments, publish SSE, dispatch to channels.
		if err := s.store.CreateMessage(ctx, storeMsg); err != nil {
			s.messageLog.Error("Failed to persist outbound message", "error", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"Failed to persist message", nil)
			return
		}
		// W7: Link before publishing so a client that refetches on the SSE
		// event already sees the attachments.
		s.mu.RLock()
		wcs := s.webChatStore
		cr := s.channelRegistry
		s.mu.RUnlock()
		linkAttachmentRefs(ctx, wcs, storeMsg.ID, attachmentRefs, s.messageLog)
		delete(structuredMsg.Metadata, attachmentsMetadataKey) // strip internal transport key
		s.recordMessageArtifacts(ctx, storeMsg.ID, outboundArtifactRefs)
		s.events.PublishUserMessage(ctx, storeMsg, attachmentRefs)
		if cr != nil && cr.Len() > 0 {
			cr.Dispatch(ctx, structuredMsg)
		}
	}

	// Agent-authored @mention fan-out: the primary already succeeded (both
	// delivery-path branches above return early on failure), so fan-out
	// now. Primary is nil — a user/group-conversation
	// recipient has no single agent primary to exclude.
	//
	// ParentConvVerified is result.Asserted (DEF-138 Rule 1): true only
	// when the caller referenced this group conversation by an existing,
	// already-authorized ID. A group conversation derived from the
	// caller's own free-text thread_id (Rules 2/3) is minted on demand, so
	// its external_ref embeds whatever the caller chose to send; fan-out
	// treats that the same as no group context at all.
	mentionResults := s.fanOutAgentMentions(ctx, agentMentionFanoutInput{
		Sender:             agent,
		SenderIdent:        agentIdent,
		Primary:            nil,
		Msg:                originalMsgForMentions,
		Type:               req.Type,
		ParentConv:         result.ConvResult,
		ParentConvVerified: result.Asserted,
		ParentMessageID:    storeMsg.ID,
		Channel:            result.Channel,
		HumanMembers:       humanMembers,
	})

	// Fire notifications (both broker and non-broker paths).
	// W6-mention: mention notifications for agent → group messages.
	if req.ThreadID != "" && !strings.HasPrefix(req.ThreadID, "dm:") && !strings.HasPrefix(req.ThreadID, "agent:") {
		names := messages.ExtractMentions(req.Msg)
		if len(names) > 0 {
			senderName := agent.Name
			if senderName == "" {
				senderName = agent.Slug
			}
			go s.fireHumanMentionNotifications(context.Background(), names, agent.ProjectID,
				req.ThreadID, "", senderName, req.Msg)
		}
	}

	// W6: DM notification for agent → human replies (non-broker path only).
	if bp := s.GetMessageBrokerProxy(); bp == nil {
		if cn := s.getChatNotifier(); cn != nil && req.ThreadID != "" && strings.HasPrefix(req.ThreadID, "dm:") && result.RecipientID != "" {
			senderName := agent.Name
			if senderName == "" {
				senderName = agent.Slug
			}
			go cn.NotifyDMReceived(context.Background(), result.RecipientID, ChatMessageContext{
				SenderID:        agent.ID,
				SenderName:      senderName,
				ConversationKey: req.ThreadID,
				Preview:         req.Msg,
				ProjectID:       agent.ProjectID,
			})
		}
	}

	outboundLogAttrs := []any{
		"agent_id", agent.ID,
		"agent_name", agent.Name,
		"project_id", agent.ProjectID,
		"recipient_id", result.RecipientID,
		"msg_type", req.Type,
	}
	if result.ConversationID != "" {
		outboundLogAttrs = append(outboundLogAttrs, "conversation_id", result.ConversationID)
	}
	s.logMessage("outbound message sent", outboundLogAttrs...)

	respBody := map[string]interface{}{
		"message_id":   storeMsg.ID,
		"status":       "sent",
		"recipient":    result.Recipient,
		"recipient_id": result.RecipientID,
	}
	// #2026: report the conversation the message landed in, so a caller
	// that addressed a thread or a user can tell where it went.
	if result.ConversationID != "" {
		respBody["conversation_id"] = result.ConversationID
	}
	if len(mentionResults) > 0 {
		respBody["mention_results"] = mentionResults
	}
	if outboundArtifactWarning != "" {
		respBody["artifact_warning"] = outboundArtifactWarning
	}
	writeJSON(w, http.StatusOK, respBody)
}

// handleAgentGitHubTokenRefresh handles POST /api/v1/agents/{id}/refresh-token.
// An agent can request a fresh GitHub App installation token when its current
// token is nearing expiry. This is a self-access operation: the agent must
// present a valid Hub auth token whose subject matches the target agent ID.
func (s *Server) handleAgentGitHubTokenRefresh(w http.ResponseWriter, r *http.Request, id string) {
	agentIdent := GetAgentIdentityFromContext(r.Context())
	if agentIdent == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
			"agent authentication required for GitHub token refresh", nil)
		return
	}

	// Enforce self-access: agents can only refresh their own GitHub token
	if agentIdent.ID() != id {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"agents can only refresh their own GitHub token", nil)
		return
	}

	// Require the token refresh scope
	if !agentIdent.HasScope(ScopeAgentTokenRefresh) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"missing required scope: agent:token:refresh", nil)
		return
	}

	ctx := r.Context()

	// Look up the agent to get its project
	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if agent.ProjectID == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"agent has no project associated", nil)
		return
	}

	project, err := s.store.GetProject(ctx, agent.ProjectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if project.GitHubInstallationID == nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"project has no GitHub App installation", nil)
		return
	}

	token, expiry, err := s.MintGitHubAppTokenForProject(ctx, project)
	if err != nil {
		// Classify the error to return an appropriate status code.
		// Configuration errors (bad key, wrong app_id) are 502 (upstream auth failed),
		// not 500 (our server is broken).
		statusCode := http.StatusBadGateway
		errCode := ErrCodeRuntimeError
		if mintErr, ok := err.(*githubapp.TokenMintError); ok {
			switch mintErr.ErrorCode {
			case githubapp.ErrCodePrivateKeyInvalid, githubapp.ErrCodeAppNotFound:
				statusCode = http.StatusBadGateway
				errCode = ErrCodeRuntimeError
			case githubapp.ErrCodeInstallationRevoked, githubapp.ErrCodeInstallationSuspended:
				statusCode = http.StatusUnprocessableEntity
				errCode = ErrCodeUnprocessable
			case githubapp.ErrCodePermissionDenied, githubapp.ErrCodeRepoNotAccessible:
				statusCode = http.StatusForbidden
				errCode = ErrCodeForbidden
			}
		}
		writeError(w, statusCode, errCode,
			"failed to mint GitHub token: "+err.Error(), nil)
		return
	}

	if token == "" {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"GitHub App not configured on Hub", nil)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"token":      token,
		"expires_at": expiry,
	})
}

// restoreAgent restores a soft-deleted agent.
func (s *Server) restoreAgent(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if agent.DeletedAt.IsZero() {
		BadRequest(w, "Agent is not in deleted state")
		return
	}

	// Delete in progress (design ptone/scion#2483 §2.1): a soft-deleted row
	// must not come back while deleteBlocksStart holds. Authz already ran in
	// the caller.
	if ref := s.startGate(ctx, agent, startEntryRestore); ref.refuses() {
		ref.write(w)
		return
	}

	// Identity-key rows persist through soft-delete (only a hard delete or
	// purge frees them -- see composite.go's DeleteAgent/DeleteProject/
	// PurgeDeletedAgents), so restoring an agent should normally find its own
	// keys already reserved and in place. But an agent soft-deleted before
	// this invariant existed, or before a backfill of it, may have no key
	// rows at all, leaving a window where another agent could since have
	// taken its slug or display-name key. restoreAgentTx re-asserts the keys
	// in the same transaction as the restore, turning that window into a
	// defensive revalidation: a genuine collision surfaces as the same
	// store.ErrIdentityKeyConflict (409) a create or rename would get, rather
	// than silently restoring an agent whose key now belongs to someone else.
	// The same transaction checks that every delegator of the edges the soft
	// delete deactivated is live (otherwise a 409, or a 503 when the lookup
	// fails), reactivates those edges (a conflicting active edge is a 409)
	// and writes the agent_restore audit record. It also refuses an agent
	// whose guard user (its owner, ancestry root or schedule creator) no
	// longer exists, normally a deleted user but possibly a purged legacy
	// root agent (ptone/scion#2769; errAgentOwnerUserMissing, see
	// lockAgentGuardUserTx).
	if err := s.restoreAgentTx(ctx, agent, auditActorFromContext(ctx)); err != nil {
		if errors.Is(err, errAgentNotSoftDeleted) {
			BadRequest(w, "Agent is not in deleted state")
			return
		}
		if errors.Is(err, errAgentOwnerUserMissing) {
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"cannot restore the agent: the user or agent it belongs to no longer exists", nil)
			return
		}
		if errors.Is(err, errRestoreEdgeConflict) {
			Conflict(w, "The agent already has an active delegation that conflicts with the one being restored")
			return
		}
		if errors.Is(err, errRestoreDelegatorNotLive) {
			Conflict(w, "A principal that delegated to this agent is not active, so its delegation cannot be restored")
			return
		}
		if errors.Is(err, errRestoreDelegatorLookup) {
			slog.Error("restore: delegator lookup failed", "agent_id", agent.ID, "error", err)
			writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, "Could not verify the agent's delegators; retry the restore", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Marked as a restore so web clients that tombstoned the ID on deleted
	// bring it back (ptone/scion#2951). restoreAgentTx stamps the restored
	// row's Updated with the restore time, so that is the marker.
	s.events.PublishAgentRestored(ctx, agent, agent.Updated)

	// Answer with the same enriched shape as GET /agents/{id}, so the
	// restored agent carries its deletion view (null) and project/broker
	// names like every other agent response.
	s.writeAgentGetResponse(w, r, agent)
}

// MessageRequest is the request body for sending a message to an agent.
type MessageRequest struct {
	// Plain text message (legacy field, used for backwards compatibility).
	Message string `json:"message,omitempty"`

	// Structured message (new field, used by default).
	StructuredMessage *messages.StructuredMessage `json:"structured_message,omitempty"`

	// There is no raw field: a request carrying "raw" (top level or inside
	// structured_message) is rejected with raw_input_removed before it is
	// decoded (rejectRetiredRawMessageBody in raw_tombstone.go).

	// Plain delivers the message as plain text without ---BEGIN SCION MESSAGE---
	// envelope formatting. Merged onto StructuredMessage when set.
	Plain bool `json:"plain,omitempty"`

	// Interrupt the harness before sending.
	Interrupt bool `json:"interrupt,omitempty"`

	// Notify subscribes the sender to status notifications for this agent
	// (COMPLETED, WAITING_FOR_INPUT, LIMITS_EXCEEDED, STALLED, ERROR).
	Notify bool `json:"notify,omitempty"`

	// Wake resumes a suspended agent before delivering the message.
	Wake bool `json:"wake,omitempty"`

	// Mentions lists agent slugs to receive mention notifications (max 10).
	// The primary recipient is automatically excluded from mention fan-out.
	Mentions []string `json:"mentions,omitempty"`

	// Conversation resolution fields (Phase 11).
	// When Surface and ExternalRef are set, the hub resolves (or creates) a
	// conversation before dispatching the message.
	Surface     string `json:"surface,omitempty"`
	ExternalRef string `json:"external_ref,omitempty"`
	ParentRef   string `json:"parent_ref,omitempty"`
}

func (s *Server) handleAgentMessage(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	messaging.RecordStep(ctx, "handle_agent_message_enter")

	var req MessageRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}
	messaging.RecordStep(ctx, "request_parsed")

	// The retired end-of-turn assistant-reply type is dropped here too, as
	// in handleAgentOutboundMessage: accepted with 200 so a caller sees
	// success, but never persisted, dispatched or published.
	if req.StructuredMessage != nil && req.StructuredMessage.Type == messages.TypeAssistantReply {
		writeJSON(w, http.StatusOK, map[string]interface{}{"status": "dropped"})
		return
	}

	// Determine the message content and structured message to forward
	var plainMessage string
	var structuredMsg *messages.StructuredMessage

	if req.StructuredMessage != nil {
		structuredMsg = req.StructuredMessage
		plainMessage = req.StructuredMessage.Msg
		if req.Plain {
			structuredMsg.Plain = true
		}
		// B5 SECURITY FIX: ALWAYS derive sender identity from the
		// authenticated context. Client-supplied Sender and SenderID are
		// untrusted inputs that must never be used as conversation key
		// inputs — the DM key IS the access authority for direct
		// conversations and there is no second check to catch a wrong one.
		//
		// This also fixes the downstream broker path: the broker receives
		// the published message and inherits these (now auth-derived) fields.
		structuredMsg.Sender = "user:unknown"
		structuredMsg.SenderID = ""
		if user := GetUserIdentityFromContext(ctx); user != nil {
			structuredMsg.SenderID = user.ID()
			if email := user.Email(); email != "" {
				structuredMsg.Sender = "user:" + email
			} else {
				structuredMsg.Sender = "user:" + user.ID()
			}
		} else if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
			structuredMsg.SenderID = agentIdent.ID()
			senderSlug := agentIdent.ID() // fallback to UUID
			if senderAgent, err := s.store.GetAgent(ctx, agentIdent.ID()); err == nil {
				senderSlug = senderAgent.Slug
			} else {
				s.messageLog.Warn("failed to resolve agent slug for sender, using UUID fallback",
					"agent_id", agentIdent.ID(), "error", err)
			}
			structuredMsg.Sender = "agent:" + senderSlug
		}
		defaultInboundStructured(structuredMsg)
		messaging.RecordStep(ctx, "sender_identity_extracted")
	} else if req.Message != "" {
		plainMessage = req.Message
		// Build a structured message from the plain text so that downstream
		// logging and the broker receive a fully-populated payload.
		sender := "user:unknown"
		senderID := ""
		if user := GetUserIdentityFromContext(ctx); user != nil {
			senderID = user.ID()
			if email := user.Email(); email != "" {
				sender = "user:" + email
			} else {
				sender = "user:" + user.ID()
			}
		} else if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
			senderID = agentIdent.ID()
			senderSlug := agentIdent.ID() // fallback to UUID
			if senderAgent, err := s.store.GetAgent(ctx, agentIdent.ID()); err == nil {
				senderSlug = senderAgent.Slug
			} else {
				s.messageLog.Warn("failed to resolve agent slug for sender, using UUID fallback",
					"agent_id", agentIdent.ID(), "error", err)
			}
			sender = "agent:" + senderSlug
		}
		structuredMsg = messages.NewInstruction(sender, "agent:"+id, plainMessage)
		structuredMsg.SenderID = senderID
		structuredMsg.Plain = req.Plain
		messaging.RecordStep(ctx, "sender_identity_extracted")
	} else {
		ValidationError(w, "message or structured_message is required", nil)
		return
	}

	// Validate the assembled message through the new envelope choke point.
	// The structuredMsg is still the primary type during the transition;
	// ValidateLegacyMessage converts internally and validates both old and
	// new invariants (Phase 7, AC-8).
	if err := messaging.ValidateLegacyMessage(structuredMsg); err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}
	messaging.RecordStep(ctx, "message_validated")

	// Validate DM key format when the thread_id looks like a DM key.
	if structuredMsg != nil && structuredMsg.ThreadID != "" &&
		strings.HasPrefix(structuredMsg.ThreadID, "dm:") && !validDMKey(structuredMsg.ThreadID) {
		BadRequest(w, "invalid DM key format")
		return
	}

	// Cap mentions to avoid oversized responses and wasted server resources (R1).
	if len(req.Mentions) > messages.MaxMentionRecipients {
		req.Mentions = req.Mentions[:messages.MaxMentionRecipients]
	}

	// Detect group[] recipient for multi-target fan-out.
	if structuredMsg != nil && messages.IsGroupRecipient(structuredMsg.Recipient) {
		s.handleGroupMessage(w, r, id, structuredMsg, plainMessage, req.Interrupt)
		return
	}

	// R-7: Hoist GetAgent before the mentions block so we don't call it twice.
	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	messaging.RecordStep(ctx, "agent_loaded")

	// Old-CLI skew dedup gate. Placed here, before any conversation
	// resolution runs, rather than immediately before ExecuteAgentDM, so a
	// deduplicated POST cannot mint a conversation for a send that turns out
	// not to happen. An old CLI still fans out @mentions client-side by
	// POSTing its own Type=mention message to each mentioned agent
	// (cmd/message.go sendMentionMessages). Against a new hub,
	// fanOutAgentMentions has (synchronously, on the primary request)
	// already delivered that same mention. Detect the duplicate by content
	// match within a short window and short-circuit before this handler's
	// conversation-resolution block runs at all — a deduplicated POST must
	// not create a conversation (e.g. a fresh sender<->recipient DM) for a
	// send that turns out not to happen, and must not log DEF-3/divergence
	// data for a message that is never persisted.
	if structuredMsg != nil && structuredMsg.Type == messages.TypeMention {
		if senderAgentIdent := GetAgentIdentityFromContext(ctx); senderAgentIdent != nil {
			if existing, found := s.recentDuplicateMention(ctx, senderAgentIdent.ID(), agent.ID, plainMessage); found {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(MessageDeliveryResponse{
					MessageID:  existing.ID,
					Status:     "deduplicated",
					Agent:      agent.Slug,
					AgentPhase: agent.Phase,
				})
				return
			}
		}
	}

	// Resuming a suspended agent to deliver a message requires the
	// lifecycle permission that starting the agent requires. The check runs
	// before conversation resolution, persistence and dispatch, so a refused
	// request resumes nothing and stores nothing. A migrating recipient is
	// never resumed (see the migration gate below), so the rule does not
	// apply to it.
	if req.Wake && !reincarnationInFlight(agent) {
		if denial := s.wakeResumeDenial(ctx, GetIdentityFromContext(ctx), agent); denial != nil {
			WriteAgentDMError(w, denial)
			return
		}
	}

	// ── Foreign attachment rejection (#1687) — inbound path ──────────────
	// When the authenticated sender is an agent in a different project,
	// reject any attachments before persistence, dispatch, or publication.
	if structuredMsg != nil && len(structuredMsg.Attachments) > 0 {
		if senderAgent := GetAgentIdentityFromContext(ctx); senderAgent != nil {
			if senderAgent.ProjectID() != "" && senderAgent.ProjectID() != agent.ProjectID {
				writeError(w, http.StatusUnprocessableEntity, ErrCodeUnsupportedCapability,
					"cross-project attachment transfer is not supported; send text-only messages across projects",
					map[string]interface{}{
						"reason": string(MessageDenialCrossProjectAttachUnsupported),
					})
				return
			}
		}
	}

	// AC-33 + Phase 5 D1: Cross-project mention check with fan-out support.
	// For same-project mentions, validate all agents belong to the same project.
	// For project-qualified mentions (@project/agent), evaluate per-recipient
	// cross-project policy independently.
	if len(req.Mentions) > 0 {
		senderIdentity := GetIdentityFromContext(ctx)
		var qualifiedRefs []QualifiedAgentRef
		var sameProjectMentions []messaging.Addressee

		// Include the primary recipient agent in same-project validation.
		sameProjectMentions = append(sameProjectMentions, messaging.Addressee{
			PrincipalKind: "agent",
			PrincipalID:   agent.ID,
		})

		for _, slug := range req.Mentions {
			ref, refErr := ParseQualifiedAgentRef(slug)
			if refErr != nil {
				s.messageLog.Warn("malformed mention reference, skipping", "slug", slug, "error", refErr)
				continue
			}
			if ref.ProjectSlug != "" {
				// Project-qualified reference: evaluate cross-project policy per-target.
				qualifiedRefs = append(qualifiedRefs, ref)
			} else {
				// Same-project mention: add to batch validation.
				if mentionAgent, lookupErr := s.store.GetAgentBySlug(ctx, agent.ProjectID, slug); lookupErr == nil && mentionAgent != nil {
					sameProjectMentions = append(sameProjectMentions, messaging.Addressee{
						PrincipalKind: "agent",
						PrincipalID:   mentionAgent.ID,
					})
				}
			}
		}

		// Validate same-project mentions.
		if crossErr := messaging.ValidateCrossProjectAddressees(ctx, s.store, sameProjectMentions); crossErr != nil {
			ValidationError(w, crossErr.Error(), nil)
			return
		}

		// Evaluate cross-project qualified mentions per-target.
		if len(qualifiedRefs) > 0 {
			targets, resolveErr := s.ResolveFanOutTargets(ctx, qualifiedRefs, agent.ProjectID)
			if resolveErr != nil {
				ValidationError(w, resolveErr.Error(), nil)
				return
			}
			result := s.EvaluateFanOutTargets(ctx, senderIdentity, targets)
			// Log denied fan-out targets for audit (D7).
			for _, t := range result.Targets {
				if !t.Decision.Allowed {
					LogCrossProjectDecision(CrossProjectAuditEntry{
						Timestamp:        time.Now(),
						Action:           "deny",
						SenderID:         agent.ID,
						SenderProjectID:  agent.ProjectID,
						RecipientID:      t.AgentSlug,
						RecipientProject: t.ProjectID,
						DecisionCode:     t.DenialCode,
						Reason:           t.Decision.Reason,
						CrossProject:     true,
						Surface:          "mention_fanout",
					})
				}
			}
		}
	}

	// Phase 11: Conversation resolution for broker plugins using the SDK path
	// (e.g. Google Chat).  Same logic as handleBrokerInbound.
	if req.ExternalRef != "" && req.Surface == "" {
		ValidationError(w, "external_ref requires surface to be set", nil)
		return
	}
	if req.Surface != "" && req.ExternalRef != "" {
		var keyOpts []messaging.ConversationByKeyOption
		keyOpts = append(keyOpts, messaging.WithSurface(req.Surface))
		if req.ParentRef != "" {
			keyOpts = append(keyOpts, messaging.WithParentRef(req.ParentRef))
		}
		if agent.ID != "" {
			agentID := agent.ID
			keyOpts = append(keyOpts, messaging.WithDefaultAgentID(&agentID))
		}
		s.mu.RLock()
		wcs := s.webChatStore
		s.mu.RUnlock()
		if wcs != nil {
			keyOpts = append(keyOpts, messaging.WithKeyTopicLookup(wcs))
		}
		convResult, convErr := messaging.ResolveOrCreateConversationByKey(
			ctx, s.store, s.messageLog, req.ExternalRef, "group", &agent.ProjectID, keyOpts...)
		if convErr != nil {
			if s.writeDenyEnabled() {
				messaging.WriteDenialMetrics.Inc("agent_msg.phase11")
				s.messageLog.Error("conversation resolution failed", "error", convErr)
				writeError(w, http.StatusConflict, ErrCodeConversationNotResolved, "conversation resolution failed", nil)
				return
			}
			s.messageLog.Warn("conversation resolution failed (write-deny OFF, continuing)", "error", convErr)
		} else {
			if structuredMsg.Metadata == nil {
				structuredMsg.Metadata = make(map[string]string)
			}
			structuredMsg.Metadata["conversation_id"] = convResult.ConversationID
		}
	}

	// Ownership check: verify the DM key IDs match the actual participants.
	// The agent in the DM key must match the target agent; the user must match
	// the AUTHENTICATED identity (not the client-supplied SenderID, which can
	// be spoofed).
	//
	// parseDMKeyIDs's second slot is always semantically a USER (A25.7 R2):
	// the authenticated principal must actually be a user, not merely have a
	// UUID that happens to equal that slot's value — otherwise an agent
	// sender can name its own UUID there and pass. See the commit history
	// for the phantom-participant-row rationale.
	if structuredMsg != nil && structuredMsg.ThreadID != "" &&
		strings.HasPrefix(structuredMsg.ThreadID, "dm:") {
		dmAgentID, dmUserID := parseDMKeyIDs(structuredMsg.ThreadID)
		var authenticatedUserID string
		isAuthenticatedUser := false
		if user := GetUserIdentityFromContext(ctx); user != nil {
			authenticatedUserID = user.ID()
			isAuthenticatedUser = true
		} else if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
			authenticatedUserID = agentIdent.ID()
		}
		if !isAuthenticatedUser || dmAgentID != agent.ID || dmUserID != authenticatedUserID {
			BadRequest(w, "DM thread_id does not match the sender and recipient")
			return
		}
	}

	// Wake handling: if requested, resume a suspended agent before message delivery.
	// For agent-to-agent DMs, wake is handled by ExecuteAgentDM after admission
	// checks so that denied requests cannot resume an agent (#1691 AC-2).
	// For user-to-agent messages, wake is handled inline here using the shared helper.
	senderIsAgent := GetAgentIdentityFromContext(ctx) != nil

	// Migration gate (design agent-reincarnate §3.7, Amendment A25 2a.2):
	// while the recipient is mid-`scion reincarnate`, skip both the wake
	// attempt and the phase-conflict check below — a migrating agent is
	// necessarily non-"running" for most of the migration, and neither
	// waking it nor rejecting the sender with an ordinary 409 is correct.
	// The message is still persisted below and the deferred short-circuit
	// right before dispatch takes over.
	// deferDelivery keeps the message without dispatching it: set for a
	// migrating recipient, or when a wake finds another start in progress.
	deferDelivery := reincarnationInFlight(agent)

	if req.Wake && !senderIsAgent && !deferDelivery {
		wakeResult, wakeErr := s.wakeAgentForDM(ctx, agent)
		if wakeErr != nil {
			WriteAgentDMError(w, wakeErr)
			return
		}
		// Phase mutation is applied in place on the agent record. When
		// another start is already in progress, the message is kept,
		// deferred, exactly as for a migrating recipient.
		if wakeResult != nil && wakeResult.Outcome == WakeDeferred {
			deferDelivery = true
		}
	}

	// Reject messages to non-running agents when --wake is not set.
	// For agent-to-agent DMs, phase validation is handled by ExecuteAgentDM.
	if !req.Wake && !senderIsAgent && !deferDelivery {
		switch state.Phase(agent.Phase) {
		case state.PhaseRunning:
			// OK — proceed to deliver
		case state.PhaseSuspended:
			writeError(w, http.StatusConflict, ErrCodeAgentNotRunning,
				fmt.Sprintf("Agent %q is suspended. Use --wake to resume and deliver.", agent.Slug), nil)
			return
		case state.PhaseStopped:
			writeError(w, http.StatusConflict, ErrCodeAgentNotRunning,
				fmt.Sprintf("Agent %q is stopped. Use 'scion resume' to restart it with its previous state.", agent.Slug), nil)
			return
		case state.PhaseError:
			writeError(w, http.StatusConflict, ErrCodeAgentNotRunning,
				fmt.Sprintf("Agent %q is in error state. Use 'scion resume --force' to best-effort resume its previous session, or 'scion start' for a fresh one.", agent.Slug), nil)
			return
		default:
			writeError(w, http.StatusConflict, ErrCodeAgentNotRunning,
				fmt.Sprintf("Agent %q is not yet running (phase: %s). Wait for it to reach running state.", agent.Slug, agent.Phase), nil)
			return
		}
	}

	// Populate recipient slug and ID from the resolved agent.
	structuredMsg.Recipient = "agent:" + agent.Slug
	structuredMsg.RecipientID = agent.ID
	messaging.RecordStep(ctx, "recipient_stamped")

	// Default the channel to "web" for messages sent through the web UI.
	// Only tag as "web" when the authenticated user's client type is
	// actually "web" — CLI and API callers should not be tagged.
	if structuredMsg.Channel == "" {
		if user := GetUserIdentityFromContext(ctx); user != nil {
			if au, ok := user.(*AuthenticatedUser); ok && au.ClientType() == "web" {
				structuredMsg.Channel = "web"
			}
		}
	}

	if !s.checkBrokerAvailability(w, r, agent) {
		return
	}

	// Log the inbound message to the dedicated message log. This fires
	// before persistence and before the migration-gate decision below
	// (reincarnating), so it must NOT claim a dispatch outcome (A25.6 O6,
	// report-7-gteam-2a): a message that ends up deferred would otherwise
	// be misread by anyone grepping logs for "dispatched" as delivered. The
	// authoritative outcome is logged separately once known (e.g. "dm
	// dispatch outcome", "agent DM: message dispatched").
	logAttrs := []any{
		"agent_id", agent.ID,
		"agent_name", agent.Name,
		"project_id", agent.ProjectID,
	}
	if structuredMsg != nil {
		logAttrs = append(logAttrs, structuredMsg.LogAttrs()...)
	}
	s.logMessage("message received for delivery", logAttrs...)

	// Persist to message store before delivery attempt. Set dispatch_state
	// to "dispatched" (no new pending rows per delivery policy).
	var persistedMsgID string
	// artifactWarning reports artifact references not attached on the
	// non-agent-sender path below (admitMessageArtifacts).
	var artifactWarning string
	// dispatchMsg (#2257, design auto-offload-large-dm §4.4) is the object
	// actually rendered a second time and dispatched below; it defaults to
	// today's structuredMsg (possibly nil) and is only replaced with an
	// offloaded copy on the human/broker-sender branch, after persistence
	// and the unchanged first render. Nothing derived after dispatch
	// (mention fan-out, observers, the HTTP response) may read it — those
	// keep using structuredMsg / plainMessage.
	dispatchMsg := structuredMsg
	// F2b (design doc §3.3): the resolved conversation's ID when it is a
	// group conversation, hoisted above the conversation-resolution block
	// (like persistedMsgID) so every processMentions call site below —
	// including branches outside that block's scope — can register
	// dispatched mention recipients as participants. Empty for direct
	// conversations (a mention target is by definition not a DM
	// participant, invariant D-1) or when nothing resolved.
	var groupConversationID string
	// groupConversationThreadKey is the same resolved group conversation's
	// own canonical external_ref, passed to processMentions alongside
	// groupConversationID so a mention row's thread key can be set from this
	// server-resolved value instead of from the primary message's own
	// caller-supplied thread_id. Empty whenever groupConversationID is.
	var groupConversationThreadKey string
	// mentionParticipantGroupID is groupConversationID, but only when the
	// group came from an existing, caller-referenced conversation — the
	// same condition that gates groupConversationThreadKey. It is what
	// processMentions actually registers mentioned agents into, kept
	// separate from groupConversationID (which registerGroupPrimary still
	// uses unconditionally for the PRIMARY recipient) so a mention's
	// participant registration follows the same rule as its thread key.
	var mentionParticipantGroupID string
	if structuredMsg != nil {
		// Migration gate: a human-sender message to a migrating recipient
		// (reincarnating, computed above) is persisted with DispatchState
		// "deferred" and short-circuits before dispatch below, instead of
		// the pre-existing optimistic "dispatched" value.
		humanMsgDispatchState := store.MessageDispatchDispatched
		if deferDelivery {
			humanMsgDispatchState = store.MessageDispatchDeferred
		}
		storeMsg := &store.Message{
			ID:            api.NewUUID(),
			ProjectID:     agent.ProjectID,
			Sender:        structuredMsg.Sender,
			SenderID:      structuredMsg.SenderID,
			Recipient:     structuredMsg.Recipient,
			RecipientID:   structuredMsg.RecipientID,
			Msg:           structuredMsg.Msg,
			Type:          structuredMsg.Type,
			Urgent:        structuredMsg.Urgent,
			Broadcasted:   structuredMsg.Broadcasted,
			AgentID:       agent.ID,
			Channel:       structuredMsg.Channel,
			ThreadID:      structuredMsg.ThreadID,
			DispatchState: humanMsgDispatchState,
			CreatedAt:     time.Now(),
		}

		// Phase 2 D4: stamp server-derived cross-project provenance.
		// RecipientProjectID is always the target agent's project.
		recipientProjectID := agent.ProjectID
		storeMsg.RecipientProjectID = &recipientProjectID
		// SenderProjectID is derived from the authenticated sender.
		// For human senders, SenderProjectID remains nil (no project-level provenance).
		if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
			senderProjID := agentIdent.ProjectID()
			storeMsg.SenderProjectID = &senderProjID
		}

		// Phase 5 dual-write: resolve-or-create conversation for user/agent → agent messages.
		// If the CLI already resolved a conversation_id (S4 conversation references),
		// use it directly instead of re-resolving.
		var convResult *messaging.ConversationResult
		// assertedConvRow holds the *store.Conversation row looked up below
		// when the caller supplied an authorized conversation_id (design
		// auto-offload-large-dm §4.3, r4 #6). The offload check after
		// dispatch-text rendering reuses this row instead of a second
		// lookup; for every other conversation source (ConversationRef, the
		// derived path, or none held) it stays nil and the offload check
		// falls back to GetConversation(storeMsg.ConversationID) itself,
		// only when the body actually qualifies.
		var assertedConvRow *store.Conversation
		// groupConvIsExistingReference is true only when the group
		// conversation came from the caller referencing an
		// already-existing conversation by ID (looked up and checked below,
		// never minted). The other branch derives a conversation key from
		// free-text thread_id and creates the conversation on demand if it
		// doesn't exist yet, which means its external_ref embeds whatever
		// text the caller chose to send — not a property that can identify
		// a conversation the mentioned agent already belongs to. Only the
		// looked-up case is used to key a mention row's thread identity.
		groupConvIsExistingReference := false
		if structuredMsg.ConversationID != "" {
			// DEF-49 SECURITY: authorize the caller-supplied conversation_id
			// against the authenticated sender before honouring it.
			// The else branch below derives the conversation key from the
			// authenticated context (B5); this branch must verify the caller's
			// assertion is consistent with that identity.
			authKind, authID := authenticatedSender(ctx)
			if authKind == "" || authID == "" {
				writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
					"authenticated identity required for caller-supplied conversation_id", nil)
				return
			}

			conv, convErr := s.store.GetConversation(ctx, structuredMsg.ConversationID)
			if convErr != nil {
				if errors.Is(convErr, store.ErrNotFound) {
					writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
						"caller-supplied conversation_id does not exist", nil)
					return
				}
				s.messageLog.Error("DEF-49: GetConversation failed for caller-supplied conversation_id",
					"conversation_id", structuredMsg.ConversationID,
					"error", convErr,
				)
				writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
					"conversation lookup failed", nil)
				return
			}
			if conv == nil {
				// Defensive: GetConversation should not return (nil, nil),
				// but if it does, fail closed.
				writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
					"caller-supplied conversation_id does not exist", nil)
				return
			}
			assertedConvRow = conv

			// Authority differs by conversation kind. Direct conversations
			// have ProjectID == nil (global), so project scoping cannot be
			// the universal check. The DM key IS the ACL for direct rows.
			//
			// `agent` is the recipient agent (resolved from the URL path at
			// :668), not the sender. The group case is therefore a containment
			// check — "the conversation belongs to the addressed agent's
			// project" — not a sender-participation check.
			switch conv.Kind {
			case "direct":
				if err := messages.CheckDMParticipantKey(conv.Kind, conv.ExternalRef, authKind, authID); err != nil {
					s.messageLog.Warn("DEF-49: direct conversation authorization failed",
						"conversation_id", conv.ID,
						"auth_kind", authKind,
						"error", err,
					)
					writeError(w, http.StatusForbidden, ErrCodeForbidden,
						"authenticated sender is not a participant in the direct conversation", nil)
					return
				}
			case "group":
				// Deny when either project ID is unset (empty or zero UUID).
				// Two unset IDs comparing equal would authorize a request
				// that has no project context — the same class of bug that
				// isUnsetProjectID (validate.go:136) guards against.
				const zeroUUID = "00000000-0000-0000-0000-000000000000"
				convProjUnset := conv.ProjectID == nil || *conv.ProjectID == "" || *conv.ProjectID == zeroUUID
				agentProjUnset := agent.ProjectID == "" || agent.ProjectID == zeroUUID
				if convProjUnset || agentProjUnset || *conv.ProjectID != agent.ProjectID {
					s.messageLog.Warn("DEF-49: group conversation project mismatch or unset project",
						"conversation_id", conv.ID,
						"conv_project_id", conv.ProjectID,
						"agent_project_id", agent.ProjectID,
					)
					writeError(w, http.StatusForbidden, ErrCodeForbidden,
						"conversation does not belong to the agent's project", nil)
					return
				}

				// Auto-register both sender and recipient as participants.
				if authKind, authID := authenticatedSender(ctx); authID != "" {
					if ensureErr := s.store.EnsureParticipant(ctx, &store.ConversationParticipant{
						ConversationID: structuredMsg.ConversationID,
						PrincipalKind:  authKind,
						PrincipalID:    authID,
						Role:           "member",
					}); ensureErr != nil {
						s.messageLog.Warn("auto-register sender as participant failed (listing gap)",
							"conversation_id", structuredMsg.ConversationID,
							"principal_kind", authKind,
							"principal_id", authID,
							"error", ensureErr)
					}
				}
				// Also register the recipient agent.
				if ensureErr := s.store.EnsureParticipant(ctx, &store.ConversationParticipant{
					ConversationID: structuredMsg.ConversationID,
					PrincipalKind:  "agent",
					PrincipalID:    agent.ID,
					Role:           "member",
				}); ensureErr != nil {
					s.messageLog.Warn("auto-register recipient agent as participant failed (listing gap)",
						"conversation_id", structuredMsg.ConversationID,
						"principal_kind", "agent",
						"principal_id", agent.ID,
						"error", ensureErr)
				}
				groupConvIsExistingReference = true
			default:
				// Unknown conversation kind — fail closed.
				s.messageLog.Warn("DEF-49: unknown conversation kind, denying",
					"conversation_id", conv.ID,
					"kind", conv.Kind,
				)
				writeError(w, http.StatusForbidden, ErrCodeForbidden,
					"unsupported conversation kind", nil)
				return
			}

			// Authorization passed — honour the caller's assertion.
			storeMsg.ConversationID = structuredMsg.ConversationID
			convResult = &messaging.ConversationResult{
				ConversationID: structuredMsg.ConversationID,
				ExternalRef:    conv.ExternalRef,
				Kind:           conv.Kind,
				Surface:        conv.Surface,
				DisplayName:    conv.DisplayName,
			}
		} else {
			// B5 SECURITY: derive sender identity for the conversation key
			// from the authenticated context, never from the message payload.
			// authenticatedSender is the B5 choke point — the always-override
			// at the top of handleAgentMessage already stamped structuredMsg
			// fields, but using authenticatedSender here makes the invariant
			// locally visible and satisfies the security-marker gate.
			authKind, authID := authenticatedSender(ctx)
			extRef, kind, projID, deriveErr := messaging.DeriveConversationKey(messaging.KeyInputs{
				ThreadID:      structuredMsg.ThreadID,
				ProjectID:     agent.ProjectID,
				SenderKind:    authKind,
				SenderID:      authID,
				RecipientKind: "agent",
				RecipientID:   agent.ID,
			})
			if deriveErr != nil {
				if s.writeDenyEnabled() {
					messaging.WriteDenialMetrics.Inc("agent_msg.derive")
					writeError(w, http.StatusConflict, ErrCodeConversationNotResolved,
						"conversation key derivation failed: "+deriveErr.Error(), nil)
					return
				}
				s.messageLog.Warn("skipping conversation resolution: key derivation refused (write-deny OFF)",
					"thread_id", structuredMsg.ThreadID, "error", deriveErr)
			} else {
				var keyOpts []messaging.ConversationByKeyOption
				s.mu.RLock()
				wcs := s.webChatStore
				s.mu.RUnlock()
				if wcs != nil {
					keyOpts = append(keyOpts, messaging.WithKeyTopicLookup(wcs))
				}
				// A25.6 F1/F3: register both DM principals as participants so
				// the conversation is discoverable via `conversation list`
				// (this is handleAgentMessage's no-ConversationID branch,
				// covering user->agent and agent->agent 1:1 sends,
				// report-7-gteam-2a cases (a) and (b)). O3 (A25.7): despite
				// the old name for this branch, it is NOT limited to a
				// server-derived key — Case 1 of DeriveConversationKey (a
				// caller-supplied "dm:" ThreadID) is reachable here too. That
				// is safe only because the ownership check above (R2, A25.7)
				// already verified the key names the authenticated sender
				// (by kind AND id) and the target agent before this point.
				keyOpts = append(keyOpts, messaging.WithParticipants(s.store))
				var convErr error
				convResult, convErr = messaging.ResolveOrCreateConversationByKey(ctx, s.store, s.messageLog, extRef, kind, projID, keyOpts...)
				if convErr != nil {
					if s.writeDenyEnabled() {
						messaging.WriteDenialMetrics.Inc("agent_msg.resolve")
						s.messageLog.Error("conversation resolution failed", "error", convErr)
						writeError(w, http.StatusConflict, ErrCodeConversationNotResolved, "conversation resolution failed", nil)
						return
					}
					s.messageLog.Warn("conversation resolution failed (write-deny OFF, continuing)", "error", convErr)
					convResult = nil
				}
			}
		}
		if convResult != nil && storeMsg.ConversationID == "" {
			storeMsg.ConversationID = convResult.ConversationID
		}
		messaging.RecordStep(ctx, "conversation_resolved")
		if convResult != nil {
			if err := messaging.ValidateAttributed(storeMsg.ConversationID); err != nil {
				if s.writeDenyEnabled() {
					messaging.WriteDenialMetrics.Inc("agent_msg.validate")
					writeError(w, http.StatusConflict, ErrCodeConversationNotResolved, err.Error(), nil)
					return
				}
				s.messageLog.Warn("ValidateAttributed failed (write-deny OFF, continuing)", "error", err)
			}
		}
		if convResult != nil && convResult.Kind == "group" {
			groupConversationID = convResult.ConversationID
			if groupConvIsExistingReference {
				groupConversationThreadKey = convResult.ExternalRef
				mentionParticipantGroupID = convResult.ConversationID
			}
		}
		// Always log divergence — even when convResult is nil, that is a divergence signal.
		oldRouting := messaging.OldRoutingFromMessage(structuredMsg.SenderID, agent.ID, structuredMsg.ThreadID)
		convID := ""
		actualRef := ""
		if convResult != nil {
			convID = convResult.ConversationID
			actualRef = convResult.ExternalRef
		}
		// DEF-49: lookupFailed was removed — both lookup-failure cases now
		// deny before reaching this point, so the "conv-lookup-failed"
		// divergence entry is unreachable dead code. Removed per AC-D-7.
		match, reason := messaging.ComputeDivergenceMatch(oldRouting, actualRef, convID)
		messaging.LogDivergence(s.messageLog, messaging.DivergenceEntry{
			MessageID:  storeMsg.ID,
			OldRouting: oldRouting,
			NewRouting: messaging.NewRoutingStr(convID),
			Match:      match,
			Reason:     reason,
		})
		messaging.RecordStep(ctx, "divergence_logged")
		// DEF-3: Independent consistency check against prior messages.
		if consistent := messaging.CheckConversationConsistency(ctx, s.store, storeMsg.ID, convID, structuredMsg.ThreadID, structuredMsg.SenderID, agent.ID, s.messageLog); !consistent {
			s.messageLog.Warn("DEF-3: conversation consistency mismatch (structured agent message)",
				"message_id", storeMsg.ID, "conversation_id", convID, "agent_id", agent.ID)
		}
		// Propagate GroupID from metadata so CLI-originated group[] messages
		// preserve correlation in the store.
		groupID := ""
		if structuredMsg.Metadata != nil {
			if gid, ok := structuredMsg.Metadata["group_id"]; ok {
				storeMsg.GroupID = gid
				groupID = gid
			}
		}

		// ── Agent DM fork (#1688) ────────────────────────────────────────
		// When the sender is an agent, delegate to the shared ExecuteAgentDM
		// operation. This consolidates rate limiting, authorization,
		// persistence, dispatch, and observer publication into one code path
		// shared with the outbound handler.
		if senderAgentIdent := GetAgentIdentityFromContext(ctx); senderAgentIdent != nil {
			senderAgentRec, senderErr := s.store.GetAgent(ctx, senderAgentIdent.ID())
			if senderErr != nil {
				s.messageLog.Error("agent DM: sender agent lookup failed",
					"sender_id", senderAgentIdent.ID(), "error", senderErr)
				writeErrorFromErr(w, senderErr, "")
				return
			}

			dmResult, dmErr := s.ExecuteAgentDM(ctx, &AgentDMInput{
				SenderAgent:    senderAgentRec,
				SenderIdentity: senderAgentIdent,
				TargetAgent:    agent,
				Msg:            plainMessage,
				Type:           structuredMsg.Type,
				Plain:          structuredMsg.Plain,
				Urgent:         structuredMsg.Urgent,
				Interrupt:      req.Interrupt,
				Attachments:    structuredMsg.Attachments,
				Metadata:       structuredMsg.Metadata,
				ConversationID: storeMsg.ConversationID,
				ConvResult:     convResult,
				Asserted:       structuredMsg.ConversationID != "" && storeMsg.ConversationID == structuredMsg.ConversationID,
				Channel:        structuredMsg.Channel,
				ThreadID:       structuredMsg.ThreadID,
				ProjectID:      agent.ProjectID,
				GroupID:        groupID,
				Wake:           req.Wake,
			})
			if dmErr != nil {
				WriteAgentDMError(w, dmErr)
				return
			}
			messaging.RecordStep(ctx, "agent_dm_executed")

			// Review round 2 finding #2 (see registerGroupPrimary): the
			// primary recipient `agent` was dispatched above via
			// ExecuteAgentDM and must be registered on a thread-derived
			// group — this is the agent-sender path (scion message
			// --thread-id between agents).
			s.registerGroupPrimary(ctx, groupConversationID, agent)

			// Post-delivery adapter concerns: notification subscription
			// and mention processing stay outside the core operation.
			if req.Notify {
				var notifySubscriberType, notifySubscriberID, createdBy string
				createdBy = senderAgentIdent.ID()
				notifySubscriberType = store.SubscriberTypeAgent
				notifySubscriberID = senderAgentRec.Slug
				s.createNotifySubscription(ctx, agent.ID, agent.ProjectID, notifySubscriberType, notifySubscriberID, createdBy)
			}

			// Agent-authored @mention fan-out: body mentions plus the
			// explicit Mentions field, resolved and delivered through
			// fanOutAgentMentions — not processMentions, which stays for the
			// human/broker sender branch of this handler. ParentConvVerified
			// is groupConvIsExistingReference: true only when the caller
			// referenced this group conversation by an existing,
			// already-authorized ID, never one minted on demand from
			// free-text thread_id.
			var groupConv *messaging.ConversationResult
			if convResult != nil && convResult.Kind == "group" {
				groupConv = convResult
			}
			mentionResults := s.fanOutAgentMentions(ctx, agentMentionFanoutInput{
				Sender:             senderAgentRec,
				SenderIdent:        senderAgentIdent,
				Primary:            agent,
				Msg:                plainMessage,
				Type:               structuredMsg.Type,
				Explicit:           req.Mentions,
				ParentConv:         groupConv,
				ParentConvVerified: groupConvIsExistingReference,
				ParentMessageID:    dmResult.MessageID,
				Channel:            structuredMsg.Channel,
			})

			// Use "dispatched" for accepted, "ambiguous" for ambiguous (#1689),
			// "deferred" while the recipient is mid-migration (design
			// agent-reincarnate §3.7). API wording does not promise harness
			// consumption (AC-1).
			deliveryStatus := "dispatched"
			httpStatus := http.StatusOK
			var deferredNote string
			switch dmResult.Outcome {
			case AgentDMAmbiguous:
				deliveryStatus = "ambiguous"
				httpStatus = http.StatusAccepted
			case AgentDMDeferred:
				deliveryStatus = "deferred"
				httpStatus = http.StatusAccepted
				deferredNote = deferredReason(agent)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(httpStatus)
			_ = json.NewEncoder(w).Encode(MessageDeliveryResponse{
				MessageID:       dmResult.MessageID,
				Status:          deliveryStatus,
				Agent:           agent.Slug,
				AgentPhase:      agent.Phase,
				MentionResults:  mentionResults,
				Deferred:        deferredNote,
				ArtifactWarning: dmResult.ArtifactWarning,
			})
			return
		}

		// #2257 P1 (design auto-offload-large-dm §4.2 item 1): strip
		// hub-reserved offload metadata keys before persist/render/dispatch,
		// so a client cannot spoof body_offloaded/body_chars/body_sha256.
		// admitMessageArtifacts performs that strip, then re-adds only the
		// artifact references the sender can read under its own request
		// credential (ptone/scion#3222).
		var artifactRefs []artifacts.MessageRef
		structuredMsg.Metadata, artifactRefs, artifactWarning = s.admitMessageArtifacts(ctx, structuredMsg.Metadata)
		structuredMsg.ArtifactRefsAdmitted = len(artifactRefs) > 0

		if err := s.store.CreateMessage(ctx, storeMsg); err != nil {
			s.messageLog.Error("Failed to persist message", "error", err)
		} else {
			persistedMsgID = storeMsg.ID
			s.recordMessageArtifacts(ctx, storeMsg.ID, artifactRefs)
		}
		messaging.RecordStep(ctx, "message_persisted")
		// B11/B13: only publish when persistence succeeded — publishing an
		// unpersisted message is not legal.
		if persistedMsgID != "" {
			// Publish SSE event so connected browser clients can update the
			// per-agent conversation view in real time — mirrors the agent→user
			// publish path in handleAgentOutboundMessage.
			s.events.PublishUserMessage(ctx, storeMsg, nil)
			messaging.RecordStep(ctx, "sse_published")
		}

		// Phase 9b(ii): render the delivery envelope from the persisted row
		// and conversation result when the envelope switch is ON.
		if s.writeDenyEnabled() && persistedMsgID != "" {
			structuredMsg.DeliveryText = messaging.RenderDeliveryText(messaging.RenderDeliveryInput{
				MessageID:  storeMsg.ID,
				ConvResult: convResult,
				Msg:        structuredMsg,
				CreatedAt:  storeMsg.CreatedAt,
			})
		}

		// #2257 (design auto-offload-large-dm §4.4): offload an
		// over-threshold body onto the DISPATCHED copy. The persisted row
		// and the DeliveryText rendered just above keep the full body.
		// persistedMsgID == "" (CreateMessage failed) means Qualifies'
		// MessageID guard never offloads — the body is delivered inline, as
		// today.
		pol := s.offloadPolicy()
		if messaging.Qualifies(storeMsg.Msg, structuredMsg.Plain, pol) {
			convRow := assertedConvRow
			if convRow == nil && storeMsg.ConversationID != "" {
				convRow, _ = s.store.GetConversation(ctx, storeMsg.ConversationID)
			}
			canRead := convRow != nil && s.recipientCanReadConversation(ctx, convRow, agent)

			deliverMsg, off := messaging.OffloadForDelivery(messaging.OffloadInput{
				Msg:                  structuredMsg,
				PersistedBody:        storeMsg.Msg,
				MessageID:            persistedMsgID,
				ConversationID:       storeMsg.ConversationID,
				RecipientCanReadConv: canRead,
				FetchByID:            false, // P1/P2: literal false (design §8.1, §10 P1/P2).
			}, pol)
			if off.Offloaded {
				if s.writeDenyEnabled() {
					deliverMsg.DeliveryText = messaging.RenderDeliveryText(messaging.RenderDeliveryInput{
						MessageID:  storeMsg.ID,
						ConvResult: convResult,
						Msg:        deliverMsg,
						CreatedAt:  storeMsg.CreatedAt,
					})
				}
				dispatchMsg = deliverMsg
			}
		}
	}

	// Migration gate (design agent-reincarnate §3.7, Amendment A25 2a.2/R2
	// p2a-r1 review): the recipient is mid-`scion reincarnate` and
	// persistence itself failed above — the message is neither saved nor
	// dispatched, so the sender must NOT be told it is safe on catch-up.
	// This must be checked before any dispatch attempt below.
	if deferDelivery && persistedMsgID == "" {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"failed to persist message; agent is reincarnating, retry", nil)
		return
	}

	// deliveryStatus is set by whichever dispatch branch below runs; unused
	// (left "") when reincarnating, since that response always says "deferred".
	var deliveryStatus string

	// Migration gate (design agent-reincarnate §3.7, Amendment A25 2a.2):
	// skip the actual dispatch attempt while the recipient is mid-`scion
	// reincarnate` — the message is already persisted above (visible in
	// conversation history for the new generation's catch-up). R1 (p2a-r1
	// review): post-delivery adapter work (notify subscription, @mention
	// fan-out) below is NOT gated by `reincarnating` — a mentioned agent is
	// a different, very likely non-migrating recipient, and the sender's
	// notify subscription is independent of whether this specific message
	// reached the migrating primary. Gating them here silently dropped both
	// with no way for the sender to tell.
	if !deferDelivery {
		// Managed agent path: deliver message directly via backend, bypass broker.
		if isManagedAgentRuntime(agent.Runtime) {
			if err := s.managedAgentMessage(ctx, agent, plainMessage, req.Interrupt); err != nil {
				if persistedMsgID != "" {
					if markErr := s.markFailed(ctx, persistedMsgID, err.Error()); markErr != nil {
						s.messageLog.Error("Failed to mark message as failed", "id", persistedMsgID, "error", markErr)
					}
				}
				RuntimeError(w, "Failed to send message to managed agent: "+err.Error())
				return
			}

			agent.Phase = string(state.PhaseRunning)
			agent.Activity = "working"
			_ = s.store.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
				Phase:    agent.Phase,
				Activity: agent.Activity,
			})
			// Publish from a re-read: a delete that claimed the row meanwhile
			// must not be painted over (design ptone/scion#2483 note F).
			s.publishAgentStatusFresh(ctx, agent)

			// B11/B13: reflect persistence failure in the response status.
			// The request still succeeds (dispatch worked), but the caller
			// should know the message was not persisted.
			deliveryStatus = "delivered"
			if persistedMsgID == "" {
				deliveryStatus = "delivered_not_persisted"
			}
		} else {
			// If a dispatcher is available, dispatch the message to the runtime broker
			dispatcher := s.GetDispatcher()
			if dispatcher == nil {
				ServiceNotReady(w, "Message dispatch is not available yet — the server may still be starting up")
				return
			}
			if agent.RuntimeBrokerID == "" {
				ServiceNotReady(w, "Agent has no runtime broker assigned — the server may still be starting up")
				return
			}

			// Synchronous delivery with 30s retry deadline for transient broker failures.
			// ptone/scion#1839: carry the persisted message ID so a broker
			// that accepts (buffers) the message and later fails to flush it
			// can report the failure back against this row, as the
			// agent-to-agent (ExecuteAgentDM) and web chat paths do.
			dispatchCtx := ctx
			if persistedMsgID != "" {
				dispatchCtx = withDispatchMessageID(ctx, persistedMsgID)
			}
			retryCtx, retryCancel := context.WithTimeout(dispatchCtx, 30*time.Second)
			defer retryCancel()

			if err := dispatchWithBrokerRetry(retryCtx, dispatcher, agent, dispatchMsg.Msg, req.Interrupt, dispatchMsg); err != nil {
				if persistedMsgID != "" {
					if markErr := s.markFailed(ctx, persistedMsgID, err.Error()); markErr != nil {
						s.messageLog.Error("Failed to mark message as failed", "id", persistedMsgID, "error", markErr)
					}
				}
				if errors.Is(err, ErrBrokerTimeout) {
					GatewayTimeout(w, "Broker unreachable after 30s deadline")
				} else if isBrokerAgentNotFound(err) {
					// The broker answered that the agent has no running
					// container: a state conflict, not a broker failure.
					writeError(w, http.StatusConflict, ErrCodeAgentNotRunning,
						"Agent has no running container; the message was not delivered", nil)
				} else if writeBrokerRuntimeUnavailable(w, err, agent.Runtime) {
					// Written: the agent's runtime is not available on its
					// broker right now, a retryable 503 (ptone/scion#2748).
					s.messageLog.Warn("Message not delivered: agent's runtime not available on broker",
						"agent_id", agent.ID, "runtime", agent.Runtime)
				} else if req.Wake {
					RuntimeError(w, "Agent resumed successfully but message delivery failed: "+err.Error())
				} else {
					RuntimeError(w, "Failed to send message to runtime broker: "+err.Error())
				}
				return
			}
			messaging.RecordStep(ctx, "broker_dispatched")

			// Publish agent-to-agent messages through the broker so plugin observers
			// (Telegram, broker-log) can see them. ObserverOnly prevents the hub's own
			// subscription from re-dispatching.
			//
			// #1687: For cross-project DMs, strip body and attachment metadata from
			// the observer message so unrelated project members receive no content
			// through the broker publication sink.
			if strings.HasPrefix(structuredMsg.Sender, "agent:") &&
				strings.HasPrefix(structuredMsg.Recipient, "agent:") {
				if bp := s.GetMessageBrokerProxy(); bp != nil {
					observerMsg := *structuredMsg
					observerMsg.ObserverOnly = true
					isCrossProjectObs := false
					if senderAgent := GetAgentIdentityFromContext(ctx); senderAgent != nil {
						isCrossProjectObs = senderAgent.ProjectID() != "" && senderAgent.ProjectID() != agent.ProjectID
					}
					if isCrossProjectObs {
						sanitizeCrossProjectObserver(&observerMsg)
					}
					if err := bp.PublishMessage(ctx, agent.ProjectID, &observerMsg); err != nil {
						s.messageLog.Error("Failed to publish agent-to-agent observer message",
							"agent_id", agent.ID, "error", err)
					}
				}
			}

			// B11/B13: reflect persistence failure in the response status.
			deliveryStatus = "delivered"
			if persistedMsgID == "" {
				deliveryStatus = "delivered_not_persisted"
			}
		}
	}

	// Create notification subscription if requested. Not gated by
	// `reincarnating` (R1, p2a-r1 review): this subscribes the sender to
	// the AGENT's future status changes, independent of whether this one
	// message was dispatched or deferred.
	if req.Notify {
		var notifySubscriberType, notifySubscriberID, createdBy string
		if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
			createdBy = agentIdent.ID()
			if creatorAgent, err := s.store.GetAgent(ctx, agentIdent.ID()); err == nil {
				notifySubscriberType = store.SubscriberTypeAgent
				notifySubscriberID = creatorAgent.Slug
			}
		} else if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			createdBy = userIdent.ID()
			notifySubscriberType = store.SubscriberTypeUser
			notifySubscriberID = userIdent.ID()
		}
		s.createNotifySubscription(ctx, agent.ID, agent.ProjectID, notifySubscriberType, notifySubscriberID, createdBy)
	}

	// Review round 2 finding #2 (see registerGroupPrimary): the
	// dispatched path's primary must also be registered on a thread-derived
	// group. Skipped when deferred (R1, p2a-r1 review, accepted as-is):
	// registerGroupPrimary's semantics are "participant = dispatched", and
	// a deferred message was never dispatched to this primary.
	if !deferDelivery {
		s.registerGroupPrimary(ctx, groupConversationID, agent)
	}

	// Process @mentions: validate slugs, fan out mention messages to
	// resolved agents. Not gated by `reincarnating` (R1, p2a-r1 review): a
	// mentioned agent is a different recipient from the primary and is very
	// likely not itself migrating; processMentions (R3, p2a-r1 review)
	// applies its own migration gate per mentioned recipient.
	var mentionResults []messages.MentionResult
	if len(req.Mentions) > 0 && structuredMsg != nil {
		mentionResults = s.processMentions(ctx, req.Mentions, agent, structuredMsg, mentionParticipantGroupID, groupConversationThreadKey)
	}

	if deferDelivery {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(MessageDeliveryResponse{
			MessageID:       persistedMsgID,
			Status:          "deferred",
			Agent:           agent.Slug,
			AgentPhase:      agent.Phase,
			MentionResults:  mentionResults,
			Deferred:        deferredReason(agent),
			ArtifactWarning: artifactWarning,
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(MessageDeliveryResponse{
		MessageID:       persistedMsgID,
		Status:          deliveryStatus,
		Agent:           agent.Slug,
		AgentPhase:      agent.Phase,
		MentionResults:  mentionResults,
		ArtifactWarning: artifactWarning,
	})
}

// MessageDeliveryResponse is the JSON response for a successful agent message delivery.
type MessageDeliveryResponse struct {
	MessageID      string                   `json:"message_id"`
	Status         string                   `json:"status"`
	Agent          string                   `json:"agent"`
	AgentPhase     string                   `json:"agent_phase"`
	MentionResults []messages.MentionResult `json:"mention_results,omitempty"`
	// Deferred is set (design agent-reincarnate §3.7) when Status is
	// "deferred": the recipient is mid-`scion reincarnate`, the message was
	// saved to conversation history, and dispatch was deliberately skipped.
	// The CLI keys on this field to print its deferred notice.
	Deferred string `json:"deferred,omitempty"`
	// ArtifactWarning is set when artifact references the request named
	// were not attached (ptone/scion#3222).
	ArtifactWarning string `json:"artifact_warning,omitempty"`
}

// GroupMessageRecipientResult represents the delivery status for one recipient in a group[] delivery.
type GroupMessageRecipientResult struct {
	Recipient string `json:"recipient"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

// GroupMessageResponse is the JSON response for a group[] message delivery.
type GroupMessageResponse struct {
	GroupID   string                        `json:"group_id"`
	Delivered int                           `json:"delivered"`
	Failed    int                           `json:"failed"`
	Results   []GroupMessageRecipientResult `json:"results"`
	// Deferred counts recipients whose message was saved for catch-up but
	// not dispatched because they are mid-`scion reincarnate` (design
	// agent-reincarnate §3.7). F4 (p2a-r2 review): additive field, kept out
	// of Failed so a truthfully deferred recipient is not reported as a
	// failure.
	Deferred int `json:"deferred,omitempty"`
}

// handleGroupMessage fans out a structured message to multiple recipients parsed from group[].
func (s *Server) handleGroupMessage(w http.ResponseWriter, r *http.Request, anchorID string, msg *messages.StructuredMessage, plainMessage string, interrupt bool) {
	ctx := r.Context()

	// #2257 P2 (design auto-offload-large-dm §4.2 item 1): strip hub-reserved
	// offload metadata keys before any recipient copy is rendered or
	// dispatched. Every `agentMsg := *msg` copy below aliases msg.Metadata's
	// map, so stripping it once here covers all of them.
	msg.Metadata = messaging.StripReservedMetadata(msg.Metadata)

	recipients, err := messages.ParseGroupRecipient(msg.Recipient)
	if err != nil {
		ValidationError(w, "invalid group[] recipient: "+err.Error(), nil)
		return
	}

	// Resolve the anchor agent for project context.
	anchorAgent, err := s.store.GetAgent(ctx, anchorID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	projectID := anchorAgent.ProjectID

	// Phase 5 D2: Reject cross-project group/broadcast/plugin forwarding
	// BEFORE any effects. Group messages retain project boundaries — each
	// agent recipient must be in the anchor agent's project.
	for _, recip := range recipients {
		if recip.Kind == messages.RecipientAgent {
			ref, refErr := ParseQualifiedAgentRef(recip.Name)
			if refErr != nil {
				writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
					fmt.Sprintf("malformed agent reference %q: %v", recip.Name, refErr), nil)
				return
			}
			if ref.ProjectSlug != "" {
				// Explicit cross-project reference in a group context: resolve
				// the slug to a project ID so the boundary check compares the
				// same type (ID vs ID) on both sides.
				refProject, refErr := s.store.GetProjectBySlug(ctx, ref.ProjectSlug)
				if refErr != nil || refProject == nil {
					writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
						fmt.Sprintf("project %q not found", ref.ProjectSlug), nil)
					return
				}
				boundary := ValidateCrossProjectGroupBoundary(projectID, refProject.ID, "group message")
				if boundary != nil {
					writeError(w, http.StatusForbidden, ErrCodeForbidden,
						boundary.Reason, map[string]interface{}{
							"code": string(boundary.Code),
						})
					return
				}
			}
		}
	}

	recipientStrs := make([]string, len(recipients))
	for i, r := range recipients {
		recipientStrs[i] = r.String()
	}
	recipientsSet := messages.FormatGroupRecipients(msg.Sender, recipientStrs)

	groupID := api.NewUUID()
	results := make([]GroupMessageRecipientResult, len(recipients))
	delivered := 0

	dispatcher := s.GetDispatcher()

	// Phase 3 msg-authz: extract sender identity once for per-recipient checks.
	senderIdentity := GetIdentityFromContext(ctx)

	// Note: retries are sequential — large groups with unreachable members
	// may block for up to N × 30s. Future work: parallel dispatch.
	for i, recip := range recipients {
		recipStr := recip.String()

		switch recip.Kind {
		case messages.RecipientAgent:
			agent, err := s.store.GetAgentBySlug(ctx, projectID, api.Slugify(recip.Name))
			if err != nil {
				results[i] = GroupMessageRecipientResult{Recipient: recipStr, Status: "failed", Error: "agent not found: " + recip.Name}
				continue
			}

			// Phase 3 msg-authz: Check message authorization per group recipient.
			allowed, _, _ := s.authorizeAgentMessage(ctx, senderIdentity, agent, false)
			if !allowed {
				results[i] = GroupMessageRecipientResult{
					Recipient: recipStr,
					Status:    "unauthorized",
					Error:     "message delivery denied",
				}
				continue
			}

			// Migration gate (design agent-reincarnate §3.7, R3 p2a-r1
			// review): a group[] agent recipient is gated the same as any
			// other delivery path — persist as deferred, skip dispatch,
			// report "deferred" rather than dispatching into a stopped or
			// absent container and claiming "delivered".
			recipDeferred := reincarnationInFlight(agent)
			recipDispatchState := store.MessageDispatchDispatched
			if recipDeferred {
				recipDispatchState = store.MessageDispatchDeferred
			}

			// ptone/scion#1839: per-member deliverability gate, mirroring
			// the phase gate on direct sends (handleAgentMessage,
			// ExecuteAgentDM, deliverToAgent). A non-running member cannot
			// receive terminal input; dispatching would leave a
			// "dispatched" row the broker then silently drops. Evaluated
			// before the row is built so it is persisted already failed
			// (one write; the SSE event carries the true state) and stays
			// visible in history. Deferred (reincarnating) members are
			// exempt: they are saved for catch-up instead. A missing
			// dispatcher or runtime broker is known here too, so those rows
			// are also persisted already failed rather than published as
			// "dispatched" and marked failed afterwards. All reasons are
			// hub-generated, so they are safe to return to the caller.
			var gateReason *string
			if !recipDeferred {
				var reason string
				if gateErr := validateGroupMemberDeliverable(agent); gateErr != nil {
					reason = gateErr.Message
				} else if dispatcher == nil {
					reason = "dispatcher not available"
				} else if agent.RuntimeBrokerID == "" {
					reason = "agent has no runtime broker"
				}
				if reason != "" {
					recipDispatchState = store.MessageDispatchFailed
					gateReason = &reason
				}
			}

			agentMsg := *msg
			agentMsg.Type = messages.TypeGroupSet
			agentMsg.Recipient = "agent:" + agent.Slug
			agentMsg.RecipientID = agent.ID
			agentMsg.Recipients = recipientsSet

			storeMsg := &store.Message{
				ID:            api.NewUUID(),
				ProjectID:     projectID,
				Sender:        agentMsg.Sender,
				SenderID:      agentMsg.SenderID,
				Recipient:     agentMsg.Recipient,
				RecipientID:   agentMsg.RecipientID,
				Msg:           agentMsg.Msg,
				Type:          agentMsg.Type,
				Urgent:        agentMsg.Urgent,
				AgentID:       agent.ID,
				GroupID:       groupID,
				DispatchState: recipDispatchState,
				CreatedAt:     time.Now(),

				DispatchFailureReason: gateReason,
			}
			// Phase 5 dual-write: resolve-or-create conversation for group set message.
			// B5 SECURITY: derive sender from authenticated context, never payload.
			var convResult *messaging.ConversationResult
			if agent.ID != "" {
				if authKind, authID := authenticatedSender(ctx); authID != "" {
					var convErr error
					convResult, convErr = messaging.ResolveOrCreateDMConversation(ctx, s.store, s.store, s.messageLog, authKind, authID, "agent", agent.ID)
					if convErr != nil {
						if s.writeDenyEnabled() {
							messaging.WriteDenialMetrics.Inc("group.agent_recipient")
							s.messageLog.Error("conversation resolution failed", "error", convErr)
							results[i] = GroupMessageRecipientResult{Recipient: recipStr, Status: "failed", Error: "conversation resolution failed"}
							continue
						}
						s.messageLog.Warn("conversation resolution failed (write-deny OFF, continuing)", "error", convErr)
					} else {
						storeMsg.ConversationID = convResult.ConversationID
					}
				}
			}
			// Always log divergence — even when convResult is nil, that is a divergence signal.
			oldRouting := messaging.OldRoutingFromMessage(agentMsg.SenderID, agent.ID, "")
			convID := ""
			actualRef := ""
			if convResult != nil {
				convID = convResult.ConversationID
				actualRef = convResult.ExternalRef
			}
			match, reason := messaging.ComputeDivergenceMatch(oldRouting, actualRef, convID)
			messaging.LogDivergence(s.messageLog, messaging.DivergenceEntry{
				MessageID:  storeMsg.ID,
				OldRouting: oldRouting,
				NewRouting: messaging.NewRoutingStr(convID),
				Match:      match,
				Reason:     reason,
			})
			// DEF-3: Independent consistency check against prior messages.
			if consistent := messaging.CheckConversationConsistency(ctx, s.store, storeMsg.ID, convID, "", agentMsg.SenderID, agent.ID, s.messageLog); !consistent {
				s.messageLog.Warn("DEF-3: conversation consistency mismatch (agent-to-agent DM)",
					"message_id", storeMsg.ID, "conversation_id", convID, "agent_id", agent.ID)
			}
			persisted := false
			if err := s.store.CreateMessage(ctx, storeMsg); err != nil {
				s.messageLog.Error("Failed to persist set message", "recipient", recipStr, "error", err)
			} else {
				persisted = true
				// B11/B13: only publish when persistence succeeded.
				s.events.PublishUserMessage(ctx, storeMsg, nil)
			}

			// Phase 9e: render the delivery envelope for group[] agent
			// recipients. Gated on persistence success (matching the
			// processMentions pattern, not the looser broadcastDirect one)
			// so unpersisted messages never carry fabricated envelope data.
			// Stamped before dispatch so the agent receives the new format.
			//
			// The observer copy below (`observerMsg := agentMsg`) inherits
			// DeliveryText, which newly exposes the per-recipient DM
			// conversation identity (id, kind, surface, display name) to
			// project-scoped plugin observers. This is acceptable because
			// those observers already receive the message body itself via
			// bp.PublishMessage — conversation metadata discloses strictly
			// less than the content they already hold. Across a group[]
			// fan-out to N agent recipients, observers receive N envelopes,
			// each naming a different DM conversation.
			if s.writeDenyEnabled() && persisted {
				agentMsg.DeliveryText = messaging.RenderDeliveryText(messaging.RenderDeliveryInput{
					MessageID:  storeMsg.ID,
					ConvResult: convResult,
					Msg:        &agentMsg,
					CreatedAt:  storeMsg.CreatedAt,
				})
			}

			// Migration gate: the row is already persisted above as
			// deferred; skip dispatch and report it as such. F1 (p2a-r2
			// review): "deferred" means saved for catch-up — if persistence
			// itself failed above, report failure instead, never deferred.
			if recipDeferred {
				if !persisted {
					results[i] = GroupMessageRecipientResult{Recipient: recipStr, Status: "failed",
						Error: "failed to persist message; agent is reincarnating, retry"}
					continue
				}
				results[i] = GroupMessageRecipientResult{Recipient: recipStr, Status: "deferred"}
				continue
			}

			// ptone/scion#1839: the row was persisted already failed by the
			// gate above (phase, no dispatcher, or no runtime broker).
			if gateReason != nil {
				results[i] = GroupMessageRecipientResult{Recipient: recipStr, Status: "failed", Error: *gateReason}
				continue
			}

			// ptone/scion#1839: carry the per-member persisted message ID
			// (RecipientID = agent.ID, Recipient = "agent:<slug>") so a
			// post-acceptance broker failure is reported against this row.
			groupDispatchCtx := ctx
			if persisted {
				groupDispatchCtx = withDispatchMessageID(ctx, storeMsg.ID)
			}
			retryCtx, retryCancel := context.WithTimeout(groupDispatchCtx, 30*time.Second)
			if err := dispatchWithBrokerRetry(retryCtx, dispatcher, agent, plainMessage, interrupt, &agentMsg); err != nil {
				retryCancel()
				if persisted {
					if markErr := s.markFailed(ctx, storeMsg.ID, err.Error()); markErr != nil {
						s.messageLog.Error("Failed to mark set message as failed", "id", storeMsg.ID, "error", markErr)
					}
				}
				results[i] = GroupMessageRecipientResult{Recipient: recipStr, Status: "failed", Error: err.Error()}
				continue
			}
			retryCancel()

			// Publish agent-to-agent messages through the broker for plugin observers.
			if strings.HasPrefix(agentMsg.Sender, "agent:") {
				if bp := s.GetMessageBrokerProxy(); bp != nil {
					observerMsg := agentMsg
					observerMsg.ObserverOnly = true
					if err := bp.PublishMessage(ctx, projectID, &observerMsg); err != nil {
						s.messageLog.Error("Failed to publish group[] observer message",
							"recipient", recipStr, "error", err)
					}
				}
			}

			results[i] = GroupMessageRecipientResult{Recipient: recipStr, Status: "delivered"}
			delivered++

		case messages.RecipientUser:
			userRecip := "user:" + recip.Name
			userID := ""

			// DEF-126 P2: exact resolution only — UUID or email.
			// Display-name substring matching removed (no uniqueness constraint).
			// OQ-A2: any member that fails to resolve refuses the whole send.
			identifier := recip.Name
			if _, parseErr := uuid.Parse(identifier); parseErr == nil {
				u, lookupErr := s.store.GetUser(ctx, identifier)
				if lookupErr == nil {
					userID = u.ID
					name := u.Email
					if name == "" {
						name = u.ID
					}
					userRecip = "user:" + name
				} else if errors.Is(lookupErr, store.ErrNotFound) {
					writeError(w, http.StatusBadRequest, ErrCodeAddrUnknown,
						fmt.Sprintf("user:%s is not a valid addressee. No user exists with that ID.", identifier), nil)
					return
				} else {
					s.messageLog.Error("user lookup by ID failed", "identifier", identifier, "error", lookupErr)
					writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
						"user lookup failed due to an internal error", nil)
					return
				}
			} else if strings.Contains(identifier, "@") {
				u, lookupErr := s.store.GetUserByEmail(ctx, identifier)
				if lookupErr == nil {
					userID = u.ID
					name := u.Email
					if name == "" {
						name = u.ID
					}
					userRecip = "user:" + name
				} else if errors.Is(lookupErr, store.ErrNotSingular) {
					writeError(w, http.StatusBadRequest, ErrCodeAddrAmbiguous,
						fmt.Sprintf("user:%s is not a valid addressee. Multiple users match that email; resolve the duplicate before sending.", identifier), nil)
					return
				} else if errors.Is(lookupErr, store.ErrNotFound) {
					writeError(w, http.StatusBadRequest, ErrCodeAddrUnknown,
						fmt.Sprintf("user:%s is not a valid addressee. No user exists with that email.", identifier), nil)
					return
				} else {
					s.messageLog.Error("user lookup by email failed", "identifier", identifier, "error", lookupErr)
					writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
						"user lookup failed due to an internal error", nil)
					return
				}
			} else {
				writeError(w, http.StatusBadRequest, ErrCodeAddrMalformed,
					fmt.Sprintf("user:%s is not a valid addressee. Address a user by exact email (user:name@example.com) or by id. Names are not unique and cannot be resolved.", identifier), nil)
				return
			}

			userMsg := *msg
			userMsg.Type = messages.TypeGroupSet
			userMsg.Recipient = userRecip
			userMsg.RecipientID = userID
			userMsg.Recipients = recipientsSet

			storeMsg := &store.Message{
				ID:          api.NewUUID(),
				ProjectID:   projectID,
				Sender:      userMsg.Sender,
				SenderID:    userMsg.SenderID,
				Recipient:   userMsg.Recipient,
				RecipientID: userMsg.RecipientID,
				Msg:         userMsg.Msg,
				Type:        userMsg.Type,
				Urgent:      userMsg.Urgent,
				AgentID:     anchorAgent.ID,
				GroupID:     groupID,
				// This persist *is* the delivery; Ent defaults
				// dispatch_state to "pending" if left unset (nc-promote-busy).
				DispatchState: store.MessageDispatchDispatched,
				CreatedAt:     time.Now(),
			}
			// Phase 5 dual-write: resolve-or-create conversation for group set message to user.
			// B5 SECURITY: derive sender from authenticated context, never payload.
			var convResult *messaging.ConversationResult
			if userID != "" {
				if authKind, authID := authenticatedSender(ctx); authID != "" {
					var convErr error
					convResult, convErr = messaging.ResolveOrCreateDMConversation(ctx, s.store, s.store, s.messageLog, authKind, authID, "user", userID)
					if convErr != nil {
						if s.writeDenyEnabled() {
							messaging.WriteDenialMetrics.Inc("group.user_recipient")
							s.messageLog.Error("conversation resolution failed", "error", convErr)
							results[i] = GroupMessageRecipientResult{Recipient: recipStr, Status: "failed", Error: "conversation resolution failed"}
							continue
						}
						s.messageLog.Warn("conversation resolution failed (write-deny OFF, continuing)", "error", convErr)
					} else {
						storeMsg.ConversationID = convResult.ConversationID
					}
				}
			}
			// Always log divergence — even when convResult is nil, that is a divergence signal.
			oldRouting := messaging.OldRoutingFromMessage(userMsg.SenderID, userID, "")
			convID := ""
			actualRef := ""
			if convResult != nil {
				convID = convResult.ConversationID
				actualRef = convResult.ExternalRef
			}
			match, reason := messaging.ComputeDivergenceMatch(oldRouting, actualRef, convID)
			messaging.LogDivergence(s.messageLog, messaging.DivergenceEntry{
				MessageID:  storeMsg.ID,
				OldRouting: oldRouting,
				NewRouting: messaging.NewRoutingStr(convID),
				Match:      match,
				Reason:     reason,
			})
			// DEF-3: Independent consistency check against prior messages.
			if consistent := messaging.CheckConversationConsistency(ctx, s.store, storeMsg.ID, convID, "", userMsg.SenderID, userID, s.messageLog); !consistent {
				s.messageLog.Warn("DEF-3: conversation consistency mismatch (user-to-agent DM)",
					"message_id", storeMsg.ID, "conversation_id", convID)
			}
			if err := s.store.CreateMessage(ctx, storeMsg); err != nil {
				s.messageLog.Error("Failed to persist set message", "recipient", recipStr, "error", err)
			} else {
				// B11/B13: only publish when persistence succeeded.
				s.events.PublishUserMessage(ctx, storeMsg, nil)
			}

			results[i] = GroupMessageRecipientResult{Recipient: recipStr, Status: "delivered"}
			delivered++
		}
	}

	// F4 (p2a-r2 review): count failures explicitly (status "failed" or
	// "unauthorized") instead of "everything that isn't delivered", so a
	// truthfully deferred recipient is never counted as a failure.
	var failedCount, deferredCount int
	for _, r := range results {
		switch r.Status {
		case "failed", "unauthorized":
			failedCount++
		case "deferred":
			deferredCount++
		}
	}

	s.logMessage("set message dispatched",
		"project_id", projectID,
		"group_id", groupID,
		"total", len(recipients),
		"delivered", delivered,
		"failed", failedCount,
		"deferred", deferredCount,
	)

	resp := GroupMessageResponse{
		GroupID:   groupID,
		Delivered: delivered,
		Failed:    failedCount,
		Deferred:  deferredCount,
		Results:   results,
	}
	writeJSON(w, http.StatusOK, resp)
}

// BroadcastMessageRequest is the request body for broadcasting a message via the broker.
type BroadcastMessageRequest struct {
	StructuredMessage *messages.StructuredMessage `json:"structured_message"`
	Interrupt         bool                        `json:"interrupt,omitempty"`
}

// handleProjectBroadcast handles POST /api/v1/projects/{projectId}/broadcast.
// It publishes a broadcast message to the project's message broker topic,
// which fans out to all running agents in the project.
func (s *Server) handleProjectBroadcast(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	// Require user or agent authentication
	ctx := r.Context()
	userIdent := GetUserIdentityFromContext(ctx)
	agentIdent := GetAgentIdentityFromContext(ctx)
	if userIdent == nil && agentIdent == nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Broadcast requires user or agent authentication", nil)
		return
	}

	// Phase 3 msg-authz + Phase 5 D2: Agent callers must be in the same project.
	// Broadcasts retain project boundaries — cross-project broadcasts are denied
	// even in hub mode. ScopeAgentLifecycle no longer required — messaging is a
	// first-class axis (D1). Per-recipient authorization happens below via
	// authorizeAgentMessage.
	if agentIdent != nil && userIdent == nil {
		if agentIdent.ProjectID() != projectID {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"cross-project broadcast is not supported; group/broadcast/plugin channels retain project boundaries",
				map[string]interface{}{
					"code": string(MessageDenialCrossProjectGroupsUnsupported),
				})
			return
		}
	}

	// Phase 3 msg-authz: User callers — verify project exists and user has
	// basic project read access. This prevents outsiders from broadcasting.
	// The actual per-agent authorization happens per-recipient below via
	// authorizeAgentMessage. The project-level ActionAttach check is replaced
	// with ActionRead as a fast-fail gate (D1: messaging separated from attach).
	if userIdent != nil {
		project, err := s.store.GetProject(ctx, projectID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				NotFound(w, "Project")
			} else {
				writeErrorFromErr(w, err, "")
			}
			return
		}
		if !s.authorize(w, r, projectResource(project), ActionRead) {
			return // authorize writes 403
		}
	}

	// A retired raw member (top level or structured_message.raw) is rejected
	// before decoding, so no sender is stamped, no target is computed and
	// nothing is published.
	if s.rejectRetiredRawMessageBody(w, r, rawIngressBroadcast,
		agentKeysAuditTarget{ProjectID: projectID}, "", rawTombstonePreAuthMaxBodyBytes, "structured_message") {
		return
	}

	var req BroadcastMessageRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.StructuredMessage == nil {
		ValidationError(w, "structured_message is required", nil)
		return
	}

	// B5 SECURITY FIX: ALWAYS derive sender identity from the
	// authenticated context, same as handleAgentMessage. Client-supplied
	// Sender and SenderID are untrusted and must not be used as DM key
	// inputs or for routing decisions (self-skip).
	req.StructuredMessage.Sender = "user:unknown"
	req.StructuredMessage.SenderID = ""
	if userIdent != nil {
		req.StructuredMessage.SenderID = userIdent.ID()
		if email := userIdent.Email(); email != "" {
			req.StructuredMessage.Sender = "user:" + email
		} else {
			req.StructuredMessage.Sender = "user:" + userIdent.ID()
		}
	} else if agentIdent != nil {
		req.StructuredMessage.SenderID = agentIdent.ID()
		senderSlug := agentIdent.ID() // fallback to UUID
		if senderAgent, err := s.store.GetAgent(ctx, agentIdent.ID()); err == nil {
			senderSlug = senderAgent.Slug
		} else {
			s.messageLog.Warn("failed to resolve agent slug for sender, using UUID fallback",
				"agent_id", agentIdent.ID(), "error", err)
		}
		req.StructuredMessage.Sender = "agent:" + senderSlug
	}

	// B5 SECURITY FIX: force Broadcasted = true server-side. The client
	// must not control whether its message is treated as a broadcast —
	// that is a routing fact the server knows. Without this, a client
	// setting Broadcasted=false walks the message through the DM
	// dual-write in deliverToAgent, creating a DM conversation per
	// running agent.
	req.StructuredMessage.Broadcasted = true

	// ptone/scion#2100: fill what a minimal client payload omits, as
	// handleAgentMessage does.
	defaultInboundStructured(req.StructuredMessage)

	// Use authenticated identity for self-skip, not the Sender field.
	// The Sender field is a display label; the auth identity is the
	// security-relevant identity. A forged Sender could change which
	// agents are targeted.
	authKind, authID := authenticatedSender(ctx)

	// Compute broadcast targeting: list all agents, classify by phase.
	allResult, err := s.store.ListAgents(ctx, store.AgentFilter{
		ProjectID: projectID,
	}, store.ListOptions{})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	var targeted int
	skippedBreakdown := make(map[string]int)
	for _, agent := range allResult.Items {
		// Skip the sending agent to avoid self-delivery.
		if authKind == "agent" && agent.ID == authID {
			continue
		}
		if agent.Phase == string(state.PhaseRunning) {
			targeted++
		} else {
			skippedBreakdown[agent.Phase]++
		}
	}
	skipped := 0
	for _, c := range skippedBreakdown {
		skipped += c
	}

	// Collect running agents from the already-fetched list for direct fan-out.
	var runningAgents []store.Agent
	for _, agent := range allResult.Items {
		// Skip the sending agent to avoid self-delivery.
		if authKind == "agent" && agent.ID == authID {
			continue
		}
		if agent.Phase == string(state.PhaseRunning) {
			runningAgents = append(runningAgents, agent)
		}
	}

	// Phase 3 msg-authz: Pre-filter recipients through authorizeAgentMessage.
	// A project owner's broadcast reaches lineage/branch agents; a member's
	// reaches only project-mode agents; none-mode agents never reached
	// (except by super-admin). Per D4 constraint in the design doc.
	senderIdentity := GetIdentityFromContext(ctx)
	var authorizedAgents []store.Agent
	for i := range runningAgents {
		a := &runningAgents[i]
		allowed, _, _ := s.authorizeAgentMessage(ctx, senderIdentity, a, false)
		if allowed {
			authorizedAgents = append(authorizedAgents, runningAgents[i])
		}
	}
	// Update targeted count to reflect filtering.
	filtered := len(runningAgents) - len(authorizedAgents)
	targeted = len(authorizedAgents)
	if filtered > 0 {
		skipped += filtered
		// Don't expose unauthorized count in response — information leakage (MEDIUM-1).
	}
	runningAgents = authorizedAgents

	// Log the broadcast
	logAttrs := []any{"project_id", projectID}
	logAttrs = append(logAttrs, req.StructuredMessage.LogAttrs()...)
	s.logMessage("broadcast message published", logAttrs...)

	proxy := s.GetMessageBrokerProxy()
	if proxy == nil {
		// No broker configured — use direct fan-out with pre-filtered list.
		if !s.broadcastDirect(w, r, projectID, req.StructuredMessage, req.Interrupt, runningAgents) {
			return
		}
	} else {
		// Phase 3 msg-authz: Broker is available. PublishBroadcast fans out to
		// ALL subscribed agents, which bypasses our per-recipient filter. Instead,
		// publish per-agent messages through the broker for each authorized agent.
		for _, agent := range runningAgents {
			agentMsg := *req.StructuredMessage
			agentMsg.Recipient = "agent:" + agent.Slug
			agentMsg.RecipientID = agent.ID
			if err := proxy.PublishMessage(ctx, projectID, &agentMsg); err != nil {
				s.messageLog.Error("Failed to publish filtered broadcast to agent",
					"agent_id", agent.ID, "agent_slug", agent.Slug, "error", err)
			}
		}
	}

	s.writeBroadcastResponse(w, targeted+skipped, targeted, skipped, skippedBreakdown)
}

// BroadcastAcceptedResponse is the JSON response for a broadcast message.
type BroadcastAcceptedResponse struct {
	Status           string         `json:"status"`
	Total            int            `json:"total"`
	Targeted         int            `json:"targeted"`
	Skipped          int            `json:"skipped"`
	SkippedBreakdown map[string]int `json:"skipped_breakdown,omitempty"`
}

func (s *Server) writeBroadcastResponse(w http.ResponseWriter, total, targeted, skipped int, skippedBreakdown map[string]int) {
	resp := BroadcastAcceptedResponse{
		Status:   "accepted",
		Total:    total,
		Targeted: targeted,
		Skipped:  skipped,
	}
	if len(skippedBreakdown) > 0 {
		resp.SkippedBreakdown = skippedBreakdown
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(resp)
}

// broadcastDirect fans out a broadcast message directly to the given running agents
// without using the message broker. The caller provides pre-filtered running agents
// (already excluding the sender) from the same ListAgents query used for targeting counts.
// Returns true on success (caller writes 202 response), false if an error response was written.
func (s *Server) broadcastDirect(w http.ResponseWriter, r *http.Request, projectID string, msg *messages.StructuredMessage, interrupt bool, runningAgents []store.Agent) bool {
	ctx := r.Context()
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		ServiceNotReady(w, "Message dispatch is not available yet — the server may still be starting up")
		return false
	}

	// #2257 P2 (design auto-offload-large-dm §4.2 item 1): strip hub-reserved
	// offload metadata keys before any recipient copy is rendered or
	// dispatched. Every `agentMsg := *msg` copy below aliases msg.Metadata's
	// map, so stripping it once here covers all of them. No offload here —
	// broadcast rows have no ConversationID (design §10 P4 site 2).
	msg.Metadata = messaging.StripReservedMetadata(msg.Metadata)

	for _, agent := range runningAgents {
		agentMsg := *msg
		agentMsg.Recipient = "agent:" + agent.Slug
		agentMsg.RecipientID = agent.ID

		storeMsg := &store.Message{
			ID:            api.NewUUID(),
			ProjectID:     projectID,
			Sender:        agentMsg.Sender,
			SenderID:      agentMsg.SenderID,
			Recipient:     agentMsg.Recipient,
			RecipientID:   agentMsg.RecipientID,
			Msg:           agentMsg.Msg,
			Type:          agentMsg.Type,
			Urgent:        agentMsg.Urgent,
			Broadcasted:   true,
			AgentID:       agent.ID,
			DispatchState: store.MessageDispatchDispatched,
			CreatedAt:     time.Now(),
		}
		persisted := true
		if err := s.store.CreateMessage(ctx, storeMsg); err != nil {
			persisted = false
			s.messageLog.Error("Failed to persist broadcast message", "agent_id", agent.ID, "error", err)
		}

		// Phase 9b(ii): render the delivery envelope for this broadcast
		// recipient. ConvResult is nil — broadcasts deliberately skip
		// conversation resolution (no conversation for broadcasts).
		if s.writeDenyEnabled() {
			agentMsg.DeliveryText = messaging.RenderDeliveryText(messaging.RenderDeliveryInput{
				MessageID:  storeMsg.ID,
				ConvResult: nil,
				Msg:        &agentMsg,
				CreatedAt:  storeMsg.CreatedAt,
			})
		}

		// ptone/scion#1839: carry the per-agent persisted message ID
		// (RecipientID = agent.ID) so a post-acceptance broker failure is
		// reported against this row. Only when the row exists — an ID with
		// no row would point the broker's report at nothing.
		broadcastDispatchCtx := ctx
		if persisted {
			broadcastDispatchCtx = withDispatchMessageID(ctx, storeMsg.ID)
		}
		retryCtx, retryCancel := context.WithTimeout(broadcastDispatchCtx, 30*time.Second)
		dispatchErr := dispatchWithBrokerRetry(retryCtx, dispatcher, &agent, agentMsg.Msg, interrupt, &agentMsg)
		retryCancel()

		if dispatchErr != nil {
			s.messageLog.Error("Failed to deliver broadcast message to agent",
				"agent_id", agent.ID,
				"agentSlug", agent.Slug, "error", dispatchErr)
			if persisted {
				if markErr := s.markFailed(ctx, storeMsg.ID, dispatchErr.Error()); markErr != nil {
					s.messageLog.Error("Failed to mark broadcast message as failed", "id", storeMsg.ID, "error", markErr)
				}
			}
			s.publishBroadcastDeliveryFailed(ctx, &agent, &agentMsg, dispatchErr)
		}
	}
	return true
}

// defaultInboundStructured fills Version, Timestamp (RFC3339 UTC) and Type
// when a client-supplied structured message omits them (e.g. the web UI
// sends a minimal structured_message), so everything the hub publishes
// carries them (ptone/scion#2100). Client-supplied values are kept.
func defaultInboundStructured(msg *messages.StructuredMessage) {
	if msg.Version == 0 {
		msg.Version = messages.Version
	}
	if msg.Timestamp == "" {
		msg.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	if msg.Type == "" {
		msg.Type = messages.TypeInstruction
	}
}

// publishBroadcastDeliveryFailed publishes a DELIVERY_FAILED notification to the
// message sender when a per-agent broadcast delivery fails.
func (s *Server) publishBroadcastDeliveryFailed(ctx context.Context, targetAgent *store.Agent, msg *messages.StructuredMessage, deliveryErr error) {
	if !strings.HasPrefix(msg.Sender, "agent:") || msg.SenderID == "" {
		return
	}
	// ptone/scion#1838: detach from the (possibly expired/cancelled)
	// dispatch ctx, as publishDeliveryFailed does, with the notice budget
	// (deliveryNoticeTimeout) since the notice itself goes via the broker.
	ctx, cancel := detachedContext(ctx, deliveryNoticeTimeout)
	defer cancel()
	senderAgent, err := s.store.GetAgent(ctx, msg.SenderID)
	if err != nil {
		return
	}

	// ptone/scion#1841: sanitize the (possibly broker-supplied) error text
	// before it reaches the sender's terminal, as publishDeliveryFailed does.
	reason := "unknown error"
	if deliveryErr != nil {
		reason = sanitizeFailureReason(deliveryErr.Error())
	}
	failMsg := fmt.Sprintf("Broadcast delivery failed to agent %q: %s", targetAgent.Slug, reason)
	structuredMsg := newDeliveryNotice(msg.Sender, senderAgent.ID, failMsg, "DELIVERY_FAILED", messages.SystemCategoryDeliveryFailed)

	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return
	}
	if err := dispatcher.DispatchAgentMessage(ctx, senderAgent, failMsg, false, structuredMsg); err != nil {
		s.messageLog.Error("Failed to dispatch broadcast DELIVERY_FAILED notification",
			"sender_id", msg.SenderID, "target_agent", targetAgent.Slug, "error", err)
	}
}

// processMentions validates mention slugs against project agents, fans out
// NewMention messages to each valid recipient, and returns per-slug results.
// The primary recipient (the agent the message was sent to) is excluded.
//
// groupConversationID, when non-empty, names a group conversation that the
// dispatched mention recipients should be recorded as participants of
// (design doc §3.3, F2b). Pass "" to skip participant registration (direct
// conversations, no conversation resolved, or a group conversation that was
// not an existing, caller-referenced one — matching the same condition
// groupConversationThreadKey uses, so registration and thread key stay
// consistent with each other). The primary recipient is registered
// separately by handleAgentMessage's callers — either the caller-supplied
// conversation_id case's own pre-dispatch registration, or (for a
// thread-derived group) registerGroupPrimary, called at each dispatch path
// right before it calls this function.
//
// groupConversationThreadKey is that same group conversation's own canonical
// external_ref, or "" when groupConversationID is. It is the ONLY source for
// a mention row's ThreadID: the primary message's own thread_id is never
// copied onto a mention row, in any shape. When no group context was
// resolved, a mention row simply gets no thread key at all — the fresh
// conversation it lands in (see fanOutAgentMentions's equivalent treatment)
// already identifies it.
func (s *Server) processMentions(ctx context.Context, mentionSlugs []string, primaryAgent *store.Agent, originalMsg *messages.StructuredMessage, groupConversationID, groupConversationThreadKey string) []messages.MentionResult {
	if len(mentionSlugs) == 0 {
		return nil
	}

	// #2257 P2 (design auto-offload-large-dm §4.2 item 1): strip hub-reserved
	// offload metadata keys on originalMsg before any copy is derived from
	// it. messages.NewMention below builds each mention's own fresh
	// metadata map (not copied from originalMsg), so this is currently a
	// defence-in-depth no-op for the mention copies themselves — it is kept
	// here (rather than relying only on the caller's own strip) so this
	// function's behaviour does not depend on caller discipline.
	originalMsg.Metadata = messaging.StripReservedMetadata(originalMsg.Metadata)

	// List every project agent for resolution, walking all pages so a
	// mention of an agent beyond the first page still resolves.
	projectAgents, err := listAllProjectAgents(ctx, s.store, primaryAgent.ProjectID)
	if err != nil {
		// Report every mention as failed rather than dropping them: the
		// caller must be able to tell "lookup failed" from "no such agent".
		s.messageLog.Error("Failed to list project agents for mention resolution",
			"project_id", primaryAgent.ProjectID, "error", err)
		// With no known agents, ResolveMentions applies its usual
		// de-duplication and primary-recipient skip and reports every
		// remaining mention as not_found; relabel those as errors.
		failed := messages.ResolveMentions(mentionSlugs, nil, primaryAgent.Slug)
		for i := range failed {
			failed[i].Status, failed[i].Error = "error", "mention resolution unavailable"
		}
		return failed
	}

	// Build the AgentInfo slice and a slug-to-agent map for dispatch.
	agentInfos := make([]messages.AgentInfo, 0, len(projectAgents))
	agentBySlug := make(map[string]*store.Agent, len(projectAgents))
	for i := range projectAgents {
		a := &projectAgents[i]
		agentInfos = append(agentInfos, messages.AgentInfo{Slug: a.Slug, Name: a.Name})
		agentBySlug[strings.ToLower(a.Slug)] = a
	}

	// Resolve mentions using the shared package.
	results := messages.ResolveMentions(mentionSlugs, agentInfos, primaryAgent.Slug)

	// Phase 3 msg-authz: extract sender identity once for per-mention checks.
	senderIdentity := GetIdentityFromContext(ctx)

	// Aggregate timeout for all mention dispatches (O1): 30s total to avoid
	// blocking the HTTP response for up to N × 10s in the worst case.
	aggregateCtx, aggregateCancel := context.WithTimeout(ctx, 30*time.Second)
	defer aggregateCancel()

	// Fan out mention messages for each delivered slug.
	for i, r := range results {
		if r.Status != "delivered" {
			continue
		}

		// Check aggregate timeout before starting each dispatch.
		if aggregateCtx.Err() != nil {
			results[i].Status = "timeout"
			results[i].Error = "aggregate mention dispatch timeout exceeded"
			continue
		}

		mentionAgent, ok := agentBySlug[strings.ToLower(r.Slug)]
		if !ok {
			results[i].Status = "error"
			results[i].Error = "agent resolved but not found for dispatch"
			continue
		}

		// Phase 3 msg-authz: Check message authorization per mention recipient.
		mentionAllowed, _, _ := s.authorizeAgentMessage(ctx, senderIdentity, mentionAgent, false)
		if !mentionAllowed {
			results[i].Status = "unauthorized"
			results[i].Error = "message delivery denied"
			continue
		}

		mentionMsg := messages.NewMention(originalMsg.Sender, "agent:"+r.Slug, originalMsg.Msg, originalMsg.Recipient)
		mentionMsg.SenderID = originalMsg.SenderID
		mentionMsg.RecipientID = mentionAgent.ID
		mentionMsg.Channel = originalMsg.Channel
		// A mention row's thread key comes only from the verified group
		// conversation (if any); the primary message's thread_id is never
		// copied onto it.
		mentionMsg.ThreadID = groupConversationThreadKey
		// #2257 P2: strip on this copy too (U5(a) row), even though
		// NewMention's own metadata never carries client input today.
		mentionMsg.Metadata = messaging.StripReservedMetadata(mentionMsg.Metadata)

		// Migration gate (design agent-reincarnate §3.7, R3 p2a-r1 review):
		// a mentioned agent is a recipient in its own right, independent of
		// the primary. If IT is mid-`scion reincarnate`, persist the
		// mention as deferred and skip its dispatch, rather than dispatching
		// into a stopped/absent container and reporting "delivered".
		mentionDeferred := reincarnationInFlight(mentionAgent)
		mentionDispatchState := store.MessageDispatchDispatched
		if mentionDeferred {
			mentionDispatchState = store.MessageDispatchDeferred
		}

		// F3 (p2a-r2 review, A25.2 option (a)): a deferred mention has no
		// conversation by default (Phase 9b, see the comment below) — the
		// preamble tells the new generation "messages ... were saved to
		// your conversations", which would be false for a mention with no
		// conversation to find. Give a deferred mention the sender <->
		// mentioned-agent DM conversation, the same move group[] makes for
		// its agent recipients. This does not disclose the parent
		// conversation (D-1 holds): it is a new/existing DM between the
		// original sender and the mentioned agent, not the primary's
		// conversation. Non-deferred mentions are unchanged.
		//
		// If resolution fails, F1's rule applies: report error, never
		// deferred — a mention we cannot make reachable by catch-up must
		// not claim to be saved for it. No row is persisted for this case,
		// matching every other pre-persist rejection in this loop.
		var mentionConvID string
		if mentionDeferred {
			authKind, authID := authenticatedSender(ctx)
			if authID == "" {
				results[i].Status = "error"
				results[i].Error = "agent is reincarnating and no authenticated sender to link a catch-up conversation"
				continue
			}
			convResult, convErr := messaging.ResolveOrCreateDMConversation(ctx, s.store, s.store, s.messageLog, authKind, authID, "agent", mentionAgent.ID)
			if convErr != nil {
				s.messageLog.Error("processMentions: failed to resolve DM conversation for deferred mention",
					"slug", r.Slug, "error", convErr)
				results[i].Status = "error"
				results[i].Error = "agent is reincarnating and the catch-up conversation could not be resolved"
				continue
			}
			mentionConvID = convResult.ConversationID
		}

		// Persist the mention message.
		storeMsg := &store.Message{
			ID:             api.NewUUID(),
			ProjectID:      primaryAgent.ProjectID,
			Sender:         mentionMsg.Sender,
			SenderID:       mentionMsg.SenderID,
			Recipient:      mentionMsg.Recipient,
			RecipientID:    mentionMsg.RecipientID,
			Msg:            mentionMsg.Msg,
			Type:           mentionMsg.Type,
			AgentID:        mentionAgent.ID,
			Channel:        mentionMsg.Channel,
			ThreadID:       mentionMsg.ThreadID,
			ConversationID: mentionConvID,
			DispatchState:  mentionDispatchState,
			CreatedAt:      time.Now(),
		}
		if mentionMsg.Metadata != nil {
			storeMsg.GroupID = mentionMsg.Metadata["group_id"]
		}
		var persisted bool
		if createErr := s.store.CreateMessage(ctx, storeMsg); createErr != nil {
			s.messageLog.Error("Failed to persist mention message", "slug", r.Slug, "error", createErr)
		} else {
			persisted = true
		}
		// B11/B13: only publish when persistence succeeded.
		if persisted {
			s.events.PublishUserMessage(ctx, storeMsg, nil)
		}

		// Phase 9b(ii): render the delivery envelope for this mention
		// recipient. The parent conversation IS resolved (convResult is
		// live in the calling handleAgentMessage scope), but it is
		// deliberately NOT propagated here: the mention target is not
		// necessarily a participant in the parent conversation. For a
		// direct conversation, the mention target is by definition not a
		// participant (invariant D-1). Stamping the parent's conversation
		// ID onto a delivery to a non-participant would disclose the
		// identity of a conversation that agent has no access to.
		// The group case (where the target IS a participant) is an open
		// question — it requires a participant check and is out of scope
		// for Phase 9b.
		if s.writeDenyEnabled() && persisted {
			mentionMsg.DeliveryText = messaging.RenderDeliveryText(messaging.RenderDeliveryInput{
				MessageID:  storeMsg.ID,
				ConvResult: nil,
				Msg:        mentionMsg,
				CreatedAt:  storeMsg.CreatedAt,
			})
		}

		// Migration gate: skip dispatch and report the mention as deferred
		// rather than "delivered" — unless persistence itself failed above,
		// in which case F1 (p2a-r2 review) applies: the message is neither
		// saved nor dispatched, so it must be reported as an error, never
		// as deferred (which promises catch-up will find it).
		if mentionDeferred {
			if !persisted {
				results[i].Status = "error"
				results[i].Error = "failed to persist message; agent is reincarnating, retry"
				continue
			}
			results[i].Status = "deferred"
			continue
		}

		// Dispatch to the mentioned agent's runtime.
		dispatcher := s.GetDispatcher()
		if dispatcher == nil {
			results[i].Status = "error"
			results[i].Error = "dispatch not available"
			continue
		}
		if mentionAgent.RuntimeBrokerID == "" {
			results[i].Status = "error"
			results[i].Error = "agent has no runtime broker"
			continue
		}

		// Per-dispatch timeout is the lesser of 10s or the remaining aggregate budget.
		// ptone/scion#1839: carry the persisted mention row's ID
		// (RecipientID = mentionAgent.ID) so a post-acceptance broker
		// failure is reported against it.
		mentionDispatchParent := aggregateCtx
		if persisted {
			mentionDispatchParent = withDispatchMessageID(aggregateCtx, storeMsg.ID)
		}
		dispatchCtx, cancel := context.WithTimeout(mentionDispatchParent, 10*time.Second)
		if dispatchErr := dispatchWithBrokerRetry(dispatchCtx, dispatcher, mentionAgent, mentionMsg.Msg, false, mentionMsg); dispatchErr != nil {
			cancel()
			if aggregateCtx.Err() != nil {
				results[i].Status = "timeout"
				results[i].Error = "aggregate mention dispatch timeout exceeded"
			} else {
				results[i].Status = "error"
				results[i].Error = "dispatch failed: " + dispatchErr.Error()
			}
			if persisted {
				if markErr := s.markFailed(ctx, storeMsg.ID, dispatchErr.Error()); markErr != nil {
					s.messageLog.Error("Failed to mark mention message as failed", "id", storeMsg.ID, "error", markErr)
				}
			}
			continue
		}
		cancel()

		// F2b (design doc §3.3): the mention target was actually dispatched
		// into a group conversation — record it as a participant. Best
		// effort, never affects the response (AC-12).
		if groupConversationID != "" {
			s.ensureGroupParticipants(ctx, groupConversationID, []*store.Agent{mentionAgent})
		}
	}

	return results
}

// authenticatedSender returns the principal kind ("user" or "agent") and ID
// from the request's authenticated context. Returns ("", "") when no
// authenticated identity is present.
//
// B5 SECURITY: DM conversation key inputs MUST come from the authenticated
// context, never from the client-supplied message payload. The key IS the
// access control list for direct conversations — any guess on any input to
// the key derivation is a guess on the ACL.
func authenticatedSender(ctx context.Context) (kind, id string) {
	if user := GetUserIdentityFromContext(ctx); user != nil {
		return "user", user.ID()
	}
	if agent := GetAgentIdentityFromContext(ctx); agent != nil {
		return "agent", agent.ID()
	}
	return "", ""
}

// disclosableResolutionReason reports whether a ResolutionError reason is safe
// to return to the caller in full. This is the single artefact every reason
// passes through; unknown reasons are collapsed by default (safe).
//
// DEF-142 AC-3 ALLOWLIST: only "ambiguous" and "no-shared-project" are
// disclosed. Ambiguity candidates are group conversations scoped to the
// caller's own project (contained by ResolveContext.ProjectID being
// server-derived, never from request JSON), and no-shared-project is a
// caller-side configuration error. Everything else — including any future
// reason added to ResolutionError — collapses into one generic response.
// A new reason is collapsed until someone deliberately decides it is safe
// to disclose and adds it here.
func disclosableResolutionReason(reason string) bool {
	switch reason {
	case "ambiguous", "no-shared-project":
		return true
	default:
		return false
	}
}

// validateChannelRegistered checks that `channel` is registered with the
// message broker. It writes an HTTP error and returns false when:
//   - channel is empty (no-op, returns true — caller decides whether empty is OK)
//   - the broker proxy is unavailable (503)
//   - the channel is not in the broker's registered list (422)
//
// Extracted from the inline validation at the original call site so the
// DEF-158 post-affinity re-run uses the same rule (DRY). Both call sites
// must remain — the original guards the happy path; the DEF-158 site guards
// the conv-ref path where Channel is set after the first check.
func (s *Server) validateChannelRegistered(w http.ResponseWriter, channel string) bool {
	if channel == "" {
		return true
	}
	bp := s.GetMessageBrokerProxy()
	if bp == nil {
		writeError(w, http.StatusServiceUnavailable, "broker_unavailable",
			"cannot validate channel: message broker is not available", nil)
		return false
	}
	channels := bp.ListChannels()
	for _, ch := range channels {
		if ch.Name == channel {
			return true
		}
	}
	available := make([]string, len(channels))
	for i, ch := range channels {
		available[i] = ch.Name
	}
	if len(available) == 0 {
		ValidationError(w, fmt.Sprintf("channel %q is not registered; no channels are currently available", channel), nil)
	} else {
		ValidationError(w, fmt.Sprintf("channel %q is not registered; available channels: %s", channel, strings.Join(available, ", ")), nil)
	}
	return false
}
