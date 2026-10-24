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
	"net/http"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Sending attachments stored as artifacts by id (ptone/scion#4596, design
// section 6.2): the attachment_ids field of the agent send requests and of
// a human CLI send, its validation, the duplicate-name and delivery-budget
// checks shared with web send, and the delivery list a send sets on the
// messages it dispatches to agents.

// attachmentNotAvailable is the one answer for an attachment id a sender
// may not use: unknown, not its own unsent upload, or already sent.
func attachmentNotAvailable(id string) string {
	return fmt.Sprintf("attachment %q is not available in this conversation", id)
}

// validateAttachmentIDs checks the attachment ids a sender (senderKind,
// senderID) names on a send and returns their references, built by
// newArtifactAttachmentRef, or the validation message to answer with 400.
// pathCount is the number of hub-local path attachments of the same send,
// which count against MaxAttachmentsPerMessage too. With the experiment off
// or the artifact service not ready, every id is refused with the answer of
// an unknown id; there is no AttachmentStore lookup for these ids. route
// and caller label the refusal log line.
func (s *Server) validateAttachmentIDs(ctx context.Context, senderKind, senderID string, ids []string, pathCount int, route string, caller Identity) ([]AttachmentRef, string) {
	if len(ids) == 0 {
		return nil, ""
	}
	if len(ids)+pathCount > MaxAttachmentsPerMessage {
		return nil, fmt.Sprintf("too many attachments: %d (max %d)", len(ids)+pathCount, MaxAttachmentsPerMessage)
	}
	if !s.experimentEnabledAttachments() {
		logReferenceRefused(ctx, route, "attachments stored as artifacts are not available", caller)
		return nil, attachmentNotAvailable(ids[0])
	}
	svc := s.attachmentService()
	refs := make([]AttachmentRef, 0, len(ids))
	for _, id := range ids {
		info, state, err := svc.UsableAttachment(ctx, id, senderKind, senderID)
		if err != nil {
			logReferenceRefused(ctx, route, "attachment lookup failed: "+err.Error(), caller)
			return nil, attachmentNotAvailable(id)
		}
		if state != artifacts.AttachmentUsable {
			logReferenceRefused(ctx, route, "attachment is not an unsent upload of the sender", caller)
			return nil, attachmentNotAvailable(id)
		}
		refs = append(refs, newArtifactAttachmentRef(info))
	}
	if msg := duplicateAttachmentRefusal(refs); msg != "" {
		return nil, msg
	}
	return refs, ""
}

// experimentEnabledAttachments reports whether attachments stored as
// artifacts can be created and named: the experiment is on and the
// artifact service is ready.
func (s *Server) experimentEnabledAttachments() bool {
	return s.attachmentArtifactsReady() && s.experimentEnabled(experiments.AttachmentArtifacts)
}

// duplicateAttachmentRefusal returns the validation message for two refs
// that share a name, compared case-insensitively after SanitizeFilename, or
// "". A collision after the delivery reduction names both files as sent.
// Delivery writes the attachments of one message into one directory under
// their names, so names must be unique within a message.
func duplicateAttachmentRefusal(refs []AttachmentRef) string {
	seen := make(map[string]bool, len(refs))
	// Attachments stored as artifacts are delivered under the name the hub
	// reduces them to (reduceAttachmentName), which maps distinct names (a
	// space and an underscore, a non-ASCII letter, a long name past its cut)
	// to one file, so they must also be unique after that reduction.
	reduced := make(map[string]string, len(refs))
	for _, ref := range refs {
		name := ref.Name
		if clean, err := SanitizeFilename(ref.Name); err == nil {
			name = clean
		}
		key := strings.ToLower(name)
		if seen[key] {
			return fmt.Sprintf("two attachments are named %q; rename one", name)
		}
		seen[key] = true
		if ref.artifact {
			r := strings.ToLower(reduceAttachmentName(ref.Name))
			if first, ok := reduced[r]; ok {
				return fmt.Sprintf("two attachments, %q and %q, are delivered under one file name; rename one", first, ref.Name)
			}
			reduced[r] = ref.Name
		}
	}
	return ""
}

// attachmentDeliveryMaxBytes returns the messaging setting
// attachment_delivery_max_bytes (0: delivery is off).
func (s *Server) attachmentDeliveryMaxBytes() int64 {
	if ops := s.GetOperationalSettings(); ops != nil {
		return ops.AttachmentDeliveryMaxBytes()
	}
	return DefaultAttachmentDeliveryMaxBytes
}

// checkAttachmentDeliveryBudget returns the validation message for a send
// with an agent recipient whose artifact attachments exceed the delivery
// budget (messaging.attachment_delivery_max_bytes), or "". With the budget
// at 0, delivery is off and any artifact attachment is refused. The sender
// learns at send time instead of a recipient silently missing a file.
func (s *Server) checkAttachmentDeliveryBudget(refs []AttachmentRef) string {
	var total int64
	n := 0
	for _, ref := range refs {
		if ref.artifact {
			total += ref.Size
			n++
		}
	}
	if n == 0 {
		return ""
	}
	limit := s.attachmentDeliveryMaxBytes()
	if limit == 0 {
		return "attachments cannot be delivered to agents on this hub: attachment delivery is off"
	}
	if total > limit {
		return fmt.Sprintf("attachments total %s; the limit for delivery to agents is %s", formatAttachmentBytes(total), formatAttachmentBytes(limit))
	}
	return ""
}

// formatAttachmentBytes renders n bytes for a validation message.
func formatAttachmentBytes(n int64) string {
	const mib = 1 << 20
	if n >= mib {
		return fmt.Sprintf("%.1f MiB", float64(n)/mib)
	}
	if n >= 1<<10 {
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// attachmentDeliveries returns the delivery list of the artifact
// attachments of refs, bound to message messageID: what a dispatch to an
// agent carries (StructuredMessage.AttachmentDeliveries). Only references
// built by newArtifactAttachmentRef are listed.
func attachmentDeliveries(messageID string, refs []AttachmentRef) []messages.AttachmentDelivery {
	var out []messages.AttachmentDelivery
	for _, ref := range refs {
		if !ref.artifact {
			continue
		}
		out = append(out, messages.AttachmentDelivery{
			ArtifactID: ref.ID, MessageID: messageID, Name: ref.Name,
			MimeType: ref.MimeType, Size: ref.Size, SHA256: ref.sha256,
			ProjectedPath: ref.projectedPath, ProjectedMount: ref.projectedMount,
		})
	}
	return out
}

// recordAttachmentAddressee records agent agentID as an addressee of
// primary message primaryID for its attachments (design section 6.4), in
// state pending, and returns the row id, or "" when nothing was recorded
// (logged; the copy is then delivered without the attachments). A pending
// row grants read, so it is written only once the copy is sure to be
// attempted, immediately before it is dispatched. A row already recorded
// for the same agent and message (a second copy, for example a mention and
// the default-agent wake) is reused and set to pending for this attempt,
// unless it is delivered: then it is left as it is and nothing is tracked
// for this copy.
func (s *Server) recordAttachmentAddressee(ctx context.Context, primaryID, agentID, via string) string {
	row := &store.MessageAddressee{
		MessageID: primaryID, PrincipalKind: artifacts.PrincipalKindAgent, PrincipalID: agentID,
		Via: via, DeliveryState: addresseePending,
	}
	err := s.store.AddAddressee(ctx, row)
	if err == nil {
		return row.ID
	}
	if !errors.Is(err, store.ErrAlreadyExists) {
		s.messageLog.Error("Failed to record an attachment addressee", "message", primaryID, "agent", agentID, "error", err)
		return ""
	}
	rows, listErr := s.store.ListAddressees(ctx, primaryID)
	if listErr != nil {
		s.messageLog.Error("Failed to read attachment addressees", "message", primaryID, "error", listErr)
		return ""
	}
	for _, r := range rows {
		if r.PrincipalKind == artifacts.PrincipalKindAgent && r.PrincipalID == agentID {
			if r.DeliveryState == addresseeDelivered {
				// The agent already received a copy: leave its grant as it is,
				// whatever happens to this one.
				return ""
			}
			if err := s.store.UpdateDeliveryState(ctx, r.ID, addresseePending, nil); err != nil {
				s.messageLog.Error("Failed to reset an attachment addressee", "message", primaryID, "agent", agentID, "error", err)
				return ""
			}
			return r.ID
		}
	}
	return ""
}

// finishAttachmentAddressee records the outcome of the copy attempt for
// addressee row rowID: delivered, or failed with reason. A failed row
// grants nothing.
func (s *Server) finishAttachmentAddressee(ctx context.Context, rowID string, failure error) {
	if rowID == "" {
		return
	}
	state, reason := addresseeDelivered, (*string)(nil)
	if failure != nil {
		state = addresseeFailed
		msg := failure.Error()
		reason = &msg
	}
	if err := s.store.UpdateDeliveryState(ctx, rowID, state, reason); err != nil {
		s.messageLog.Error("Failed to record an attachment addressee outcome", "addressee", rowID, "error", err)
	}
}

// duplicateAttachmentNameWithPaths returns the duplicate-name validation
// message when an artifact attachment of refs has the name of one of the
// hub-local path attachments of the same send (their base names), or "".
func duplicateAttachmentNameWithPaths(refs []AttachmentRef, paths []string) string {
	if len(refs) == 0 || len(paths) == 0 {
		return ""
	}
	all := append([]AttachmentRef(nil), refs...)
	for _, p := range paths {
		all = append(all, AttachmentRef{Name: filepath.Base(p)})
	}
	return duplicateAttachmentRefusal(all)
}

// validateAgentSendAttachments runs the attachment_ids checks of an agent
// send: the sender must pass agentMaySend (403 otherwise, ok false and
// nothing written by this function), then validateAttachmentIDs, the
// duplicate-name check against the send's path attachments, and, when a
// recipient is an agent, the delivery budget. It returns the references,
// or the 400 validation message.
func (s *Server) validateAgentSendAttachments(ctx context.Context, caller Identity, sender *store.Agent, ids, paths []string, toAgent bool, route string) (refs []AttachmentRef, msg string, ok bool) {
	if len(ids) == 0 {
		return nil, "", true
	}
	if a, may := s.agentMaySend(ctx); !may || a.ID != sender.ID {
		return nil, "", false
	}
	refs, msg = s.validateAttachmentIDs(ctx, artifacts.PrincipalKindAgent, sender.ID, ids, len(paths), route, caller)
	if msg == "" {
		msg = duplicateAttachmentNameWithPaths(refs, paths)
	}
	if msg == "" && toAgent {
		msg = s.checkAttachmentDeliveryBudget(refs)
	}
	if msg != "" {
		return nil, msg, true
	}
	return refs, "", true
}

// storeOutboundBeforeBroker stores an agent-to-user message that carries
// attachments stored as artifacts before it is published to the message
// broker (ptone/scion#4596, decision D2), so the attachments are bound to
// the stored row. In order:
//  1. a message naming the reserved in-process channel is refused before
//     anything is stored (the answer ErrReservedChannel produces);
//  2. the conversation is resolved as deliverToUser would
//     (resolveUserMessageConversation); a failure while write-deny is on
//     is answered before anything is stored;
//  3. storeRow stores the row, binds the attachments, stamps the
//     watermarks (the caller arms afterUserMessagePersisted for it) and
//     publishes the SSE event;
//  4. the message is published to the broker with StoredMessageID, so
//     deliverToUser stores no second row.
//
// After step 3 the message is stored: a broker failure is logged and the
// send succeeds (ptone/scion#2757), since a retry would duplicate the row
// and fail on the bound ids anyway. It reports false when it wrote an
// error, and whether the persisting subscriber wrote the message record.
func (s *Server) storeOutboundBeforeBroker(ctx context.Context, w http.ResponseWriter, bp *MessageBrokerProxy, agent *store.Agent,
	storeMsg *store.Message, structuredMsg *messages.StructuredMessage, storeRow func(context.Context) error) (ok, recordLogged bool) {
	if structuredMsg.Channel == eventbus.InProcessBusName {
		s.messageLog.Warn("Outbound message names a reserved channel",
			"agent_id", agent.ID, "channel", structuredMsg.Channel)
		ValidationError(w, fmt.Sprintf("channel %q is reserved for internal use", structuredMsg.Channel), nil)
		return false, false
	}
	if !bp.resolveUserMessageConversation(ctx, agent.ProjectID, storeMsg, structuredMsg) {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to resolve the message's conversation", nil)
		return false, false
	}
	structuredMsg.ConversationID = storeMsg.ConversationID
	if err := storeRow(ctx); err != nil {
		s.messageLog.Error("Failed to persist outbound message", "agent_id", agent.ID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to persist message", nil)
		return false, false
	}
	structuredMsg.StoredMessageID = storeMsg.ID
	persisting := bp.subscribeProjectUserMessages(agent.ProjectID)
	if err := bp.PublishUserMessage(ctx, agent.ProjectID, storeMsg.RecipientID, structuredMsg); err != nil {
		s.messageLog.Warn("Outbound message stored; broker publish failed",
			"agent_id", agent.ID, "recipient_id", storeMsg.RecipientID,
			"project_id", agent.ProjectID, "message_id", storeMsg.ID, "error", err)
		return true, false
	}
	return true, persisting
}

// validateMessageAttachments validates the attachment_ids of a send on
// POST /api/v1/agents/{id}/message, whose recipient is an agent. An agent
// sender goes through validateAgentSendAttachments. A user sender is
// admitted only when attachmentWriterKind is "user" (a session or a user
// access token); any other caller's ids get the answer of an unknown id, as
// with the experiment off. A token also needs inbox:write, eligible for its
// boundary, and allowed for the target agent's project (ptone/scion#4596,
// section 9); otherwise the send is refused (ok false).
func (s *Server) validateMessageAttachments(ctx context.Context, target *store.Agent, ids, paths []string) ([]AttachmentRef, string, bool) {
	const route = "agent-message"
	if sender := GetAgentIdentityFromContext(ctx); sender != nil {
		rec, err := s.store.GetAgent(ctx, sender.ID())
		if err != nil || rec == nil {
			return nil, "", false
		}
		return s.validateAgentSendAttachments(ctx, sender, rec, ids, paths, true, route)
	}
	user := GetUserIdentityFromContext(ctx)
	if user == nil || s.attachmentWriterKind(ctx) != artifacts.PrincipalKindUser {
		logReferenceRefused(ctx, route, "caller may not name attachments stored as artifacts", GetIdentityFromContext(ctx))
		return nil, attachmentNotAvailable(ids[0]), true
	}
	if token := chatToken(ctx); token != nil {
		check := s.newSelfScopeCheck(ctx, token, permInboxWrite)
		if target == nil || !check.allows(tokenBoundaryProject(token)) || !check.allows(target.ProjectID) {
			logReferenceRefused(ctx, route, "token may not send attachments to this agent's project", token)
			return nil, "", false
		}
	}
	refs, msg := s.validateAttachmentIDs(ctx, artifacts.PrincipalKindUser, user.ID(), ids, len(paths), route, user)
	if msg == "" {
		msg = duplicateAttachmentNameWithPaths(refs, paths)
	}
	if msg == "" {
		msg = s.checkAttachmentDeliveryBudget(refs)
	}
	if msg != "" {
		return nil, msg, true
	}
	return refs, "", true
}

// agentSendReachesAgents reports whether an agent send can deliver its
// attachments to an agent (ptone/scion#4596, D11): its target is an agent,
// it is a group post (the group's default agent may be woken), or its body
// @mentions an agent of the sender's project (mention copies deliver the
// primary's attachments). A lookup failure answers true, so the budget is
// checked.
func (s *Server) agentSendReachesAgents(ctx context.Context, sender *store.Agent, toAgent, groupPost bool, body string) bool {
	if toAgent || groupPost {
		return true
	}
	names := dropSelfMentionName(messages.ExtractProseMentions(body), sender.Slug)
	if len(names) == 0 {
		return false
	}
	agents, err := listAllProjectAgents(ctx, s.store, sender.ProjectID)
	if err != nil {
		return true
	}
	for _, n := range names {
		for i := range agents {
			if strings.EqualFold(agents[i].Slug, n) || strings.EqualFold(agents[i].Name, n) {
				return true
			}
		}
	}
	return false
}
