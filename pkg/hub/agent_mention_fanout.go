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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// mentionOptions returns the current hub-wide mention parsing options,
// reflecting whether the hub.code_span_mentions experiment is enabled.
func (s *Server) mentionOptions() messages.MentionOptions {
	return messages.MentionOptions{
		CodeSpanMentions: s.experimentEnabled(experiments.CodeSpanMentions),
	}
}

// ---------------------------------------------------------------------------
// Server-side @mention fan-out for agent-authored sends.
//
// fanOutAgentMentions is the single entry point every agent send adapter
// calls after its primary delivery succeeds. It is the counterpart to
// processMentions (handlers_agent_messaging.go), which stays in place for
// human/broker senders. Unlike processMentions, every recipient goes through
// ExecuteAgentDM, so mention deliveries are charged to the sender's rate
// budget and authorized like a direct DM. Each mention gets its own
// sender-to-mentioned-agent conversation unless the parent is a verified
// in-project group conversation, in which case the mention lands there
// instead.
// ---------------------------------------------------------------------------

// mentionFanoutAggregateTimeout bounds the whole fan-out call, across every
// mention it attempts. It is well under a typical client's own request
// timeout so that a slow or unreachable mention recipient cannot make an
// already-successful primary send look like it failed by holding the HTTP
// response open. A package variable (not a constant) so tests can shrink it.
var mentionFanoutAggregateTimeout = 10 * time.Second

// mentionDispatchTimeout bounds a single mention's delivery attempt inside
// the aggregate budget above, so one slow or unreachable recipient cannot
// consume the whole aggregate budget on its own and starve every mention
// after it in the same fan-out call. A package variable so tests can shrink
// it.
var mentionDispatchTimeout = 5 * time.Second

// mentionFanoutTypes is the allow-list of message types that trigger
// server-side mention fan-out for agent senders. Notably absent: "mention"
// itself (parsing a mention row's body for more mentions would let a fan-out
// chain recurse forever), "assistant-reply" (retired: the outbound handler
// drops it before fan-out), "state-change", "system", and "group-set" (the
// CLI keeps its own client-side fan-out for group[] sends, which excludes
// group members).
var mentionFanoutTypes = map[string]bool{
	"":                       true,
	messages.TypeInstruction: true,
	messages.TypeInputNeeded: true,
	messages.TypeReply:       true,
	messages.TypeChat:        true,
}

// agentMentionFanoutInput is the normalized input to fanOutAgentMentions.
// Adapters construct this from their respective request shapes after their
// own primary delivery has already succeeded.
type agentMentionFanoutInput struct {
	// Sender is the sending agent's fresh store record.
	Sender *store.Agent

	// SenderIdent is the authenticated agent identity, used for the
	// per-recipient authorization pre-check and passed through to
	// ExecuteAgentDM.
	SenderIdent Identity

	// Primary is the primary recipient agent, when the parent send was an
	// agent-to-agent DM. Nil for a group-conversation or user-recipient
	// send, which have no single agent primary.
	Primary *store.Agent

	// Msg is the ORIGINAL message body — before any inbound translation
	// (e.g. translateMentionsInbound) — so body mentions are extracted from
	// what the agent actually wrote.
	Msg string

	// Type is the parent message's type; only types in mentionFanoutTypes
	// trigger fan-out.
	Type string

	// Explicit lists caller-supplied mention slugs (the CLI --cc flag, or
	// MessageRequest.Mentions), unioned with the body-extracted mentions.
	// Not code-stripped — an explicit mention is by definition deliberate.
	Explicit []string

	// ParentConv is the conversation the parent message landed in, if any.
	// When it is a group conversation belonging to the sender's own
	// project AND ParentConvVerified is true, mention rows land in that
	// same conversation and the mentioned agent is registered as a
	// participant. Otherwise (nil, a direct conversation, a group
	// conversation from a different project, or ParentConvVerified false),
	// each mention row gets its own fresh sender-to-mentioned-agent
	// conversation instead of the parent's.
	ParentConv *messaging.ConversationResult

	// ParentConvVerified must be true for ParentConv to be eligible for
	// reuse at all — false (the default) means "not reused," regardless of
	// ParentConv's contents. Callers set it true only when ParentConv came
	// from the caller referencing an already-existing, authorized
	// conversation by ID — never one derived on demand from the sender's
	// own free-text thread_id. A conversation minted from free text has an
	// external_ref that embeds whatever the sender chose to write, which is
	// not evidence the mentioned agent belongs there. Requiring this flag
	// (rather than trusting ParentConv.Kind alone) means a future caller
	// that forgets to check its own provenance fails closed instead of
	// silently reusing an unverified conversation.
	ParentConvVerified bool

	// ParentMessageID is the parent message's persisted ID, stamped into
	// mention metadata as "mention_of".
	ParentMessageID string

	// ParentAttachments are the attachments stored as artifacts bound to
	// the parent message (ptone/scion#4596). Each copy delivers them into
	// its target's home, and the target is recorded as an addressee of
	// the parent for them, immediately before the copy is dispatched.
	ParentAttachments []AttachmentRef

	// Channel is the delivery channel to stamp on mention deliveries.
	Channel string

	// HumanMembers, when non-nil, is the caller's own already-resolved
	// project human-member list — reused here to avoid a second
	// resolveProjectHumanMembers call (one member-list query plus one
	// GetUser per member) on the same request. Nil means the caller did not
	// resolve it, and dropHumanMentionResults fetches it itself if needed.
	HumanMembers []chatMemberEntry

	// HasPendingAttachments is true during pre-effect resolution when the
	// origin send carries artifact attachments (AttachRefs) that will be bound
	// after persistence, allowing empty-body attachment-only copies to be
	// planned before persistence.
	HasPendingAttachments bool

	// CopyAdmission is the pre-effect derived admission minted by the origin
	// message policy gate. Passed through to ExecuteAgentDM in copy mode.
	CopyAdmission *derivedAdmission
}

// resolvedCopyTarget is one authorized, resolved copy recipient (either a
// @mention copy or a group default-agent wake copy).
type resolvedCopyTarget struct {
	Agent              *store.Agent
	Position           string // "body" or "default_agent"
	ParentConv         *messaging.ConversationResult
	ParentConvVerified bool
}

// resolvedMentionPlan holds the single pre-effect resolution of @mentions for
// an agent-authored send. Both the message policy planner/admission and
// deliverResolvedAgentMentions consume this exact plan without re-resolving.
type resolvedMentionPlan struct {
	Results           []messages.MentionResult
	Targets           map[string]*store.Agent
	UnauthorizedSlugs map[string]*store.Agent
	AuthorizedOrdered []resolvedCopyTarget
	LookupFailed      bool
}

// resolveMentionCopyTargets performs side-effect-free @mention extraction,
// self-mention drop, project agent lookup, ResolveMentions dedupe and cap,
// human-mention drop, and dry-run pairwise authorization once, before the
// origin message policy gate (Contract §6).
func (s *Server) resolveMentionCopyTargets(ctx context.Context, in agentMentionFanoutInput) *resolvedMentionPlan {
	plan := &resolvedMentionPlan{}
	if in.Sender == nil || in.SenderIdent == nil {
		return plan
	}
	if !mentionFanoutTypes[in.Type] {
		return plan
	}

	names := messages.DedupMentionNames(in.Explicit, messages.ExtractProseMentionsWithOptions(in.Msg, s.mentionOptions()))
	if len(names) == 0 {
		return plan
	}

	// Drop a self-mention before resolution, not after: MaxMentionRecipients
	// caps how many *other* agents a message can page, and a name the
	// sender wrote for itself should never eat one of those slots.
	names = dropSelfMentionName(names, in.Sender.Slug)
	if len(names) == 0 {
		return plan
	}
	primarySlug := ""
	if in.Primary != nil && in.Primary.ProjectID == in.Sender.ProjectID {
		primarySlug = in.Primary.Slug
	}

	projectAgents, err := listAllProjectAgents(ctx, s.store, in.Sender.ProjectID)
	if err != nil {
		s.messageLog.Error("mention fan-out: failed to list project agents",
			"sender_id", in.Sender.ID, "project_id", in.Sender.ProjectID, "error", err)
		failed := messages.ResolveMentions(names, nil, primarySlug)
		failed = s.dropHumanMentionResults(ctx, in.Sender.ProjectID, failed, in.HumanMembers)
		if len(failed) == 0 {
			return plan
		}
		for i := range failed {
			failed[i].Status, failed[i].Error = "error", "mention resolution unavailable"
		}
		plan.Results = failed
		plan.LookupFailed = true
		return plan
	}

	agentInfos := make([]messages.AgentInfo, 0, len(projectAgents))
	agentBySlug := make(map[string]*store.Agent, len(projectAgents))
	for i := range projectAgents {
		a := &projectAgents[i]
		agentInfos = append(agentInfos, messages.AgentInfo{Slug: a.Slug, Name: a.Name})
		agentBySlug[strings.ToLower(a.Slug)] = a
	}

	results := messages.ResolveMentions(names, agentInfos, primarySlug)
	if len(results) == 0 {
		return plan
	}

	results = s.dropHumanMentionResults(ctx, in.Sender.ProjectID, results, in.HumanMembers)
	if len(results) == 0 {
		return plan
	}

	if in.Msg == "" && len(in.ParentAttachments) == 0 && !in.HasPendingAttachments {
		for i := range results {
			if results[i].Status == "delivered" {
				results[i].Status, results[i].Error = "error", errMentionCopyNeedsText
			}
		}
		plan.Results = results
		return plan
	}

	plan.Results = results
	plan.Targets = make(map[string]*store.Agent, len(results))
	plan.UnauthorizedSlugs = make(map[string]*store.Agent)
	seenTargetIDs := make(map[string]bool, len(results))
	for i := range results {
		if results[i].Status != "delivered" {
			continue
		}
		slugKey := strings.ToLower(results[i].Slug)
		target, ok := agentBySlug[slugKey]
		if !ok || target == nil {
			continue
		}
		allowed, _, _ := s.authorizeAgentMessage(ctx, in.SenderIdent, target, false)
		if !allowed {
			plan.UnauthorizedSlugs[slugKey] = target
			continue
		}
		plan.Targets[slugKey] = target
		if !seenTargetIDs[target.ID] {
			seenTargetIDs[target.ID] = true
			plan.AuthorizedOrdered = append(plan.AuthorizedOrdered, resolvedCopyTarget{
				Agent:              target,
				Position:           "body",
				ParentConv:         in.ParentConv,
				ParentConvVerified: in.ParentConvVerified,
			})
		}
	}
	return plan
}

// fanOutAgentMentions resolves and delivers @-mentions for direct unit tests.
// Production send paths call resolveMentionCopyTargets once before the policy
// gate and then deliverResolvedAgentMentions with that exact resolved plan.
func (s *Server) fanOutAgentMentions(ctx context.Context, in agentMentionFanoutInput) []messages.MentionResult {
	resolved := s.resolveMentionCopyTargets(ctx, in)
	return s.deliverResolvedAgentMentions(ctx, in, resolved)
}

// deliverResolvedAgentMentions delivers copies for an already-resolved mention
// plan without re-resolving mentions (Contract §6).
func (s *Server) deliverResolvedAgentMentions(ctx context.Context, in agentMentionFanoutInput, resolved *resolvedMentionPlan) []messages.MentionResult {
	if in.Sender == nil || in.SenderIdent == nil || resolved == nil || len(resolved.Results) == 0 {
		return nil
	}
	// Close the admission when mention fan-out finishes (only when there are
	// actual mention results; an empty fan-out leaves the admission open for
	// mutually exclusive default-agent wake).
	if in.CopyAdmission != nil {
		defer in.CopyAdmission.Close()
	}

	results := make([]messages.MentionResult, len(resolved.Results))
	copy(results, resolved.Results)

	if resolved.LookupFailed {
		return results
	}

	if in.Msg == "" && len(in.ParentAttachments) == 0 {
		for i := range results {
			if results[i].Status == "delivered" {
				results[i].Status, results[i].Error = "error", errMentionCopyNeedsText
			}
		}
		return results
	}

	// Bound the whole call well under a typical client's own request
	// timeout, so a slow or unreachable mention recipient cannot make an
	// already-successful primary send look like it failed.
	aggregateCtx, cancel := context.WithTimeout(ctx, mentionFanoutAggregateTimeout)
	defer cancel()

	counts := map[string]int{}
	budgetExhausted := false

	for i := range results {
		if results[i].Status != "delivered" {
			counts[results[i].Status]++
			continue
		}

		if budgetExhausted {
			results[i].Status = "rate_limited"
			results[i].Error = "sender rate budget exhausted by an earlier mention in this fan-out"
			counts["rate_limited"]++
			continue
		}
		if aggregateCtx.Err() != nil {
			results[i].Status = "timeout"
			results[i].Error = "aggregate mention dispatch timeout exceeded"
			counts["timeout"]++
			continue
		}

		slugKey := strings.ToLower(results[i].Slug)
		if unauthTarget, wasUnauth := resolved.UnauthorizedSlugs[slugKey]; wasUnauth {
			results[i].Status = "unauthorized"
			results[i].Error = "message delivery denied"
			if unauthTarget != nil {
				results[i].AgentPhase = unauthTarget.Phase
			}
			counts["unauthorized"]++
			continue
		}

		target, ok := resolved.Targets[slugKey]
		if !ok || target == nil {
			results[i].Status = "error"
			results[i].Error = "agent resolved but not found for dispatch"
			counts["error"]++
			continue
		}

		out := s.deliverAgentMentionCopy(ctx, aggregateCtx, in, target, "body")
		results[i].Status = out.Status
		results[i].Error = out.Error
		results[i].AgentPhase = out.AgentPhase
		if out.BudgetExhausted {
			budgetExhausted = true
		}
		counts[results[i].Status]++
	}

	// Summary line, deliberately body-free: counts per status only, no
	// message content or agent names.
	s.messageLog.Info("agent mention fan-out",
		"sender_id", in.Sender.ID,
		"parent_message_id", in.ParentMessageID,
		"attempted", len(results),
		"status_counts", counts,
	)

	return results
}

// mentionCopyOutcome is the result of one deliverAgentMentionCopy call.
type mentionCopyOutcome struct {
	// Status is a MentionResult status: delivered, ambiguous, suppressed,
	// unauthorized, timeout, rate_limited or error.
	Status string
	// Error is the human-readable reason when Status is not delivered.
	Error string
	// AgentPhase is the target's lifecycle phase.
	AgentPhase string
	// SuspendReason is why target was suspended when AgentPhase is suspended.
	SuspendReason string
	// BudgetExhausted reports that the sender's rate budget refused the
	// delivery, so later deliveries in the same call should not be tried.
	BudgetExhausted bool
}

// deliverAgentMentionCopy delivers one TypeMention copy of an
// agent-authored message to target: it reserves a slot in the per-pair
// loop/storm cap, authorizes the sender pairwise, resolves the
// conversation the copy lands in, delivers it through ExecuteAgentDM and,
// when the copy landed in a verified group conversation, registers target
// as a participant if the sender's message mode allows it. position is
// stamped as the copy's mention_position metadata.
//
// ctx is the caller's request context, used for the participant write;
// aggregateCtx bounds authorization and delivery.
func (s *Server) deliverAgentMentionCopy(ctx, aggregateCtx context.Context, in agentMentionFanoutInput, target *store.Agent, position string) mentionCopyOutcome {
	// Reserve a slot in the per-pair loop/storm cap before authorizing
	// or creating any conversation, so a suppressed pair never creates
	// one either. Reservation and admission happen together so two
	// concurrent fan-outs for the same pair cannot both slip through
	// when only one slot remains.
	if !s.mentionPairLimiter.Reserve(in.Sender.ID, target.ID) {
		return mentionCopyOutcome{Status: "suppressed",
			Error:         "mention loop protection: too many mentions between these agents; retry later",
			AgentPhase:    target.Phase,
			SuspendReason: target.SuspendReason}
	}

	// Authorize before any conversation is created: a mention must
	// never reach an agent the sender could not otherwise DM, and a
	// denied mention must leave no conversation behind. ExecuteAgentDM
	// re-checks this same authorization; that second check is harmless
	// and keeps the invariant local to the one function responsible for
	// agent DM delivery.
	if allowed, _, _ := s.authorizeAgentMessage(aggregateCtx, in.SenderIdent, target, false); !allowed {
		s.mentionPairLimiter.Release(in.Sender.ID, target.ID)
		// authorizeAgentMessage has no error return; if the aggregate
		// deadline expired during the check, an internal store call
		// inside it fails closed the same way a genuine denial would.
		// Distinguish the two so a deadline is reported as a timeout,
		// not a false "the recipient denied this".
		if aggregateCtx.Err() != nil {
			return mentionCopyOutcome{Status: "timeout", Error: "aggregate mention dispatch timeout exceeded",
				AgentPhase: target.Phase, SuspendReason: target.SuspendReason}
		}
		return mentionCopyOutcome{Status: "unauthorized", Error: "message delivery denied",
			AgentPhase: target.Phase, SuspendReason: target.SuspendReason}
	}

	// When message policies are enabled, verify the pre-effect derived
	// admission and Seam A facts fingerprint BEFORE resolving/creating a DM
	// conversation or recording an attachment addressee, so a refused copy
	// leaves zero side effects (R5, Contract §8).
	if s.experimentEnabled(experiments.MessagePolicies) {
		if admErr := s.verifyCopyAdmissionPreflight(aggregateCtx, in, target, position); admErr != nil {
			s.mentionPairLimiter.Release(in.Sender.ID, target.ID)
			return mentionCopyOutcome{Status: "error", Error: admErr.Error(),
				AgentPhase: target.Phase, SuspendReason: target.SuspendReason}
		}
	}

	convResult, convID, threadID, registerAfter, convOK := s.resolveMentionConversation(aggregateCtx, in, target)
	if !convOK {
		s.mentionPairLimiter.Release(in.Sender.ID, target.ID)
		return mentionCopyOutcome{Status: "error", Error: "conversation resolution failed",
			AgentPhase: target.Phase, SuspendReason: target.SuspendReason}
	}

	// mention_source names where the mention came from, for the
	// mentioned agent's own benefit. It is the verified group's own
	// reference when the mention landed there (registerAfter is true
	// only in that case — see resolveMentionConversation); otherwise
	// it is just the sender, never the parent message's own recipient.
	mentionSource := "agent:" + in.Sender.Slug
	if registerAfter {
		mentionSource = threadID
	}
	metadata := map[string]string{
		"mention_source":   mentionSource,
		"mention_position": position,
	}
	if in.ParentMessageID != "" {
		metadata["mention_of"] = in.ParentMessageID
	}

	// The parent's attachments stored as artifacts (ptone/scion#4596): the
	// target is recorded as an addressee of the parent, in state pending,
	// only now, after the pair limiter, the pairwise authorization and the
	// conversation resolution, so a mention any of them stops records
	// nothing; and before the copy is dispatched, so the delivery decision
	// at dispatch already sees it.
	var deliveries []messages.AttachmentDelivery
	addresseeRow := ""
	if len(in.ParentAttachments) > 0 && in.ParentMessageID != "" {
		via := "body-mention"
		if position == "default_agent" {
			via = "default-agent"
		}
		addresseeRow = s.recordAttachmentAddressee(ctx, in.ParentMessageID, target.ID, via)
		deliveries = attachmentDeliveries(in.ParentMessageID, in.ParentAttachments)
	}

	// Bound this one mention's delivery attempt inside the aggregate
	// budget, so a single slow or unreachable recipient cannot consume
	// the whole budget and starve every mention after it.
	dispatchCtx, dispatchCancel := context.WithTimeout(aggregateCtx, mentionDispatchTimeout)
	dmResult, dmErr := s.ExecuteAgentDM(dispatchCtx, &AgentDMInput{
		Deliveries:     deliveries,
		SenderAgent:    in.Sender,
		SenderIdentity: in.SenderIdent,
		TargetAgent:    target,
		Msg:            in.Msg,
		Type:           messages.TypeMention,
		Metadata:       metadata,
		ConversationID: convID,
		ConvResult:     convResult,
		Channel:        in.Channel,
		ThreadID:       threadID,
		ProjectID:      in.Sender.ProjectID,
		// A mention must not wake a stopped or user-suspended agent —
		// that would be a surprising side effect of merely being
		// mentioned — but it must also not be silently dropped just
		// because the recipient isn't running right now: it still gets a
		// persisted row and a dispatch attempt, the same as any other
		// message to an agent that isn't currently reachable. An agent
		// the hub suspended (shouldAutoWake) is woken in the background
		// and gets the mention once ready (AgentDMWakeQueued).
		SkipPhaseGate:       true,
		AttachmentAddressee: addresseeRow,
		// Trusted copy-intent provenance (Contract §5B): ONLY
		// deliverAgentMentionCopy may set copyIntent; ExecuteAgentDM uses
		// copyIntent alone as the mode discriminator.
		copyIntent:               true,
		copyAdmission:            in.CopyAdmission,
		copyBoundAttachmentCount: len(in.ParentAttachments),
		copyParentMessageID:      in.ParentMessageID,
	})
	dispatchCancel()
	switch {
	case dmErr != nil:
		s.finishAttachmentAddressee(context.WithoutCancel(ctx), addresseeRow, errors.New(dmErr.Message))
	case dmResult.Outcome == AgentDMWakeQueued:
		// The background wake settles it when it delivers or fails.
	default:
		s.finishAttachmentAddressee(context.WithoutCancel(ctx), addresseeRow, nil)
	}
	if dmErr != nil {
		s.mentionPairLimiter.Release(in.Sender.ID, target.ID)
		switch dmErr.Code {
		case ErrCodeRateLimited:
			return mentionCopyOutcome{Status: "rate_limited", Error: dmErr.Message,
				AgentPhase: target.Phase, SuspendReason: target.SuspendReason, BudgetExhausted: true}
		case ErrCodeMessageDenied:
			return mentionCopyOutcome{Status: "unauthorized", Error: dmErr.Message,
				AgentPhase: target.Phase, SuspendReason: target.SuspendReason}
		default:
			return mentionCopyOutcome{Status: "error", Error: dmErr.Message,
				AgentPhase: target.Phase, SuspendReason: target.SuspendReason}
		}
	}

	out := mentionCopyOutcome{
		AgentPhase:    target.Phase,
		SuspendReason: target.SuspendReason,
	}
	switch dmResult.Outcome {
	case AgentDMAmbiguous:
		out.Status = "ambiguous"
	default:
		out.Status = "delivered"
	}

	// The sender must be allowed to add the target under its message
	// mode (group_message_mode.go). A wake-queued mention counts as
	// delivered and is registered before its wake on purpose: a participant
	// is a listing index, not an ACL.
	if out.Status == "delivered" && registerAfter {
		if joinOK, joinReason := s.groupAgentJoinAllowed(aggregateCtx, in.SenderIdent, target); !joinOK {
			s.messageLog.Info("mention target not registered as group participant: message mode",
				"conversation_id", convID, "agent_id", target.ID, "reason", joinReason)
			return out
		}
		// A short, detached context of its own: the aggregate budget
		// may already be nearly spent by the delivery above, and this
		// write matters even if it is. Same pattern as the dispatch
		// side's own post-dispatch state transitions.
		registerCtx, registerCancel := finalizationContext(ctx)
		ackIDs := s.ensureGroupParticipants(registerCtx, convID, []*store.Agent{target})
		registerCancel()
		for _, id := range ackIDs {
			in.CopyAdmission.RecordAcknowledgedRegistration(id)
		}
	}
	return out
}

// resolveMentionConversation picks the conversation a mention row belongs
// to: a mention into a group conversation that actually belongs to the
// sender's own project stays in that conversation; every other mention (a
// DM parent, no parent at all, or a group conversation from a different
// project) gets its own fresh sender-to-mentioned-agent conversation. The
// parent's own conversation or thread identity is never reused for a
// mention row outside a verified in-project group.
//
// The final bool return (ok) reports whether the caller should proceed with
// delivery at all: false means conversation resolution failed in a
// configuration where an unresolved conversation must deny the write, and
// the mention must not be sent. When ok is true but convResult is nil, the
// caller proceeds without a conversation, matching processMentions'
// existing best-effort posture on the same failure.
func (s *Server) resolveMentionConversation(ctx context.Context, in agentMentionFanoutInput, target *store.Agent) (convResult *messaging.ConversationResult, convID, threadID string, registerAfter, ok bool) {
	if in.ParentConvVerified && in.ParentConv != nil && in.ParentConv.Kind == "group" && s.groupConversationBelongsToProject(ctx, in.ParentConv.ConversationID, in.Sender.ProjectID) {
		return in.ParentConv, in.ParentConv.ConversationID, in.ParentConv.ExternalRef, true, true
	}

	dmConv, convErr := messaging.ResolveOrCreateDMConversation(ctx, s.store, s.store, s.messageLog,
		"agent", in.Sender.ID, "agent", target.ID)
	if convErr != nil {
		s.messageLog.Error("mention fan-out: DM conversation resolution failed, denying the mention",
			"sender_id", in.Sender.ID, "target_id", target.ID, "error", convErr)
		return nil, "", "", false, false
	}
	return dmConv, dmConv.ConversationID, dmConv.ExternalRef, false, true
}

// groupConversationBelongsToProject reports whether the given conversation
// is a project-scoped group conversation belonging to projectID.
// messaging.ConversationResult carries no ProjectID, so a caller that only
// checked Kind=="group" could otherwise reuse a group conversation from a
// DIFFERENT project than the sender's — the mentioned agent (always
// resolved in the sender's own project) would then be registered as a
// participant of a group conversation it has no business being in. On any
// lookup failure this fails closed (false), which sends the caller down the
// per-mention DM fallback instead of trusting an unverified group context.
func (s *Server) groupConversationBelongsToProject(ctx context.Context, conversationID, projectID string) bool {
	if conversationID == "" || projectID == "" {
		return false
	}
	conv, err := s.store.GetConversation(ctx, conversationID)
	if err != nil || conv == nil {
		s.messageLog.Warn("mention fan-out: group conversation project check failed; falling back to a DM",
			"conversation_id", conversationID, "error", err)
		return false
	}
	return conv.ProjectID != nil && *conv.ProjectID == projectID
}

// dropSelfMentionName removes any name equal (case-insensitively) to
// selfSlug. Applied before mention resolution and its recipient cap, so a
// self-mention can never consume one of the limited mention slots a message
// gets.
func dropSelfMentionName(names []string, selfSlug string) []string {
	if len(names) == 0 {
		return names
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if strings.EqualFold(name, selfSlug) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// dropHumanMentionResults removes any "not_found" result that actually
// addresses a human project member — their canonical @firstname-lastname
// display-name slug (the same derivation translateMentionsInbound/Outbound
// use), or a raw email address — rather than an unknown agent. Those results
// are dropped silently (no entry at all), the same treatment a self-mention
// gets, because a human mention is not an error, just a different kind of
// recipient this function does not handle.
//
// Agent names are resolved first. A raw-email "not_found" token is dropped
// unconditionally — recognizable as a human address without needing the
// project's human-member list at all, regardless of whether the project has
// any human members. The member list is only fetched, for the display-name
// slug case, when a non-email "not_found" name remains; a body whose every
// mention matches an agent or a raw email costs nothing extra. Checking
// only "not_found" results (never a name that already resolved to an agent)
// means a human display-name slug that happens to collide with a real agent
// slug still reaches that agent.
//
// preResolvedHumans, when non-nil, is used instead of fetching the member
// list here — a caller that already resolved it for its own purposes (e.g.
// translateMentionsInbound on the same request) hands it in rather than
// paying for the same member-list query and per-member GetUser calls twice.
func (s *Server) dropHumanMentionResults(ctx context.Context, projectID string, results []messages.MentionResult, preResolvedHumans []chatMemberEntry) []messages.MentionResult {
	// A raw-email "not_found" token is recognizable as a human address on
	// its own — it needs no project-member lookup to identify. Drop those
	// first so the recognition does not depend on whether the project
	// happens to have any human members at all.
	pending := make([]messages.MentionResult, 0, len(results))
	out := make([]messages.MentionResult, 0, len(results))
	hasNotFound := false
	for _, r := range results {
		if r.Status == "not_found" && strings.Contains(r.Slug, "@") {
			continue
		}
		if r.Status == "not_found" {
			hasNotFound = true
		}
		pending = append(pending, r)
	}
	if !hasNotFound {
		return pending
	}

	humans := preResolvedHumans
	if humans == nil {
		humans = s.resolveProjectHumanMembers(ctx, projectID)
	}
	humanNames := humanMentionNameSet(humans)
	if len(humanNames) == 0 {
		return pending
	}

	for _, r := range pending {
		if r.Status == "not_found" && humanNames[strings.ToLower(r.Slug)] {
			continue
		}
		out = append(out, r)
	}
	return out
}

// humanMentionNameSet builds the set of mention names that address a human
// project member: their canonical @firstname-lastname display-name slug and
// their raw email, both lower-cased for case-insensitive lookup.
func humanMentionNameSet(humans []chatMemberEntry) map[string]bool {
	names := make(map[string]bool, len(humans)*2)
	for _, m := range humans {
		if m.Kind != "user" {
			continue
		}
		if m.Email != "" {
			names[strings.ToLower(m.Email)] = true
		}
		if m.DisplayName != "" {
			if slug := strings.ToLower(strings.ReplaceAll(m.DisplayName, " ", "-")); slug != "" {
				names[slug] = true
			}
		}
	}
	return names
}
