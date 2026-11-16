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
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	// messagePolicyShadowWorkers bounds concurrent shadow-policy evaluation
	// and decision persistence goroutines per Hub replica.
	messagePolicyShadowWorkers = 2

	// messagePolicyShadowQueueCap bounds pending shadow evaluation batches so
	// spikes never allocate unbounded memory or goroutines.
	messagePolicyShadowQueueCap = 256

	// messagePolicyShadowDrainTimeout bounds how long Server shutdown waits
	// for queued shadow evaluations to finish before cancelling remaining work.
	messagePolicyShadowDrainTimeout = 5 * time.Second

	// messagePolicyDecisionWriteTimeout bounds persistence of one decision row
	// on a detached context so client disconnects do not lose audit decisions.
	messagePolicyDecisionWriteTimeout = 5 * time.Second

	// maxDecisionRecipientIDBytes matches the Ent schema MaxLen(128) on
	// message_policy_decisions.recipient_id.
	maxDecisionRecipientIDBytes = 128

	// maxDecisionCELReasonBytes matches the Ent schema MaxLen(64) on
	// message_policy_decisions.cel_error_reason.
	maxDecisionCELReasonBytes = 64
)

// OTelMessagePolicyMetrics exports content-free message policy counters and
// latency histograms (consumer design §5.8).
type OTelMessagePolicyMetrics struct {
	evaluations     metric.Int64Counter
	durationMs      metric.Float64Histogram
	shadowDrops     metric.Int64Counter
	admissionErrors metric.Int64Counter
}

// NewOTelMessagePolicyMetrics constructs OTel instruments for the message
// policy gate.
func NewOTelMessagePolicyMetrics(mp metric.MeterProvider) (*OTelMessagePolicyMetrics, error) {
	if mp == nil {
		return nil, errors.New("otel message policy metrics: nil MeterProvider")
	}
	meter := mp.Meter(instrumentationScope)
	evals, err := meter.Int64Counter("scion.message_policy.evaluations", metric.WithUnit("{evaluation}"))
	if err != nil {
		return nil, err
	}
	dur, err := meter.Float64Histogram("scion.message_policy.evaluation.duration", metric.WithUnit("ms"))
	if err != nil {
		return nil, err
	}
	drops, err := meter.Int64Counter("scion.message_policy.shadow.drops", metric.WithUnit("{batch}"))
	if err != nil {
		return nil, err
	}
	admErrs, err := meter.Int64Counter("scion.message_policy.admission.errors", metric.WithUnit("{error}"))
	if err != nil {
		return nil, err
	}
	return &OTelMessagePolicyMetrics{
		evaluations:     evals,
		durationMs:      dur,
		shadowDrops:     drops,
		admissionErrors: admErrs,
	}, nil
}

// SetMessagePolicyMetrics attaches OTel instruments for message policy
// evaluation, latency, shadow drops, and admission errors.
func (s *Server) SetMessagePolicyMetrics(m *OTelMessagePolicyMetrics) {
	s.messagePolicyMetrics.Store(m)
}

func (s *Server) recordMessagePolicyEvalMetric(class, mode, surface string, elapsed time.Duration) {
	m := s.messagePolicyMetrics.Load()
	if m == nil {
		return
	}
	m.evaluations.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("class", class),
		attribute.String("mode", mode),
		attribute.String("surface", surface),
	))
	m.durationMs.Record(context.Background(), float64(elapsed)/float64(time.Millisecond), metric.WithAttributes(
		attribute.String("mode", mode),
		attribute.String("surface", surface),
	))
}

func (s *Server) recordMessagePolicyShadowDrop(projectID string, policyCount int) {
	s.messagePolicyShadowDrops.Add(1)
	if m := s.messagePolicyMetrics.Load(); m != nil {
		m.shadowDrops.Add(context.Background(), 1)
	}
	slog.Warn("message policy shadow queue full; dropping shadow evaluation batch",
		"project_id", projectID,
		"policy_count", policyCount,
	)
}

func (s *Server) recordMessagePolicyAdmissionError(projectID, policyID, policyName, reason string) {
	s.messagePolicyAdmissionErrors.Add(1)
	if m := s.messagePolicyMetrics.Load(); m != nil {
		m.admissionErrors.Add(context.Background(), 1, metric.WithAttributes(
			attribute.String("reason", reason),
		))
	}
	slog.Error("message policy evaluation encountered ruleengine consumer admission failure",
		"project_id", projectID,
		"policy_id", policyID,
		"policy", policyName,
		"reason", reason,
	)
}

// messagePolicyGateRuntime holds the shared Evaluator wrapper, bounded shadow
// worker pool, and deterministic test seams for a Hub Server.
type messagePolicyGateRuntime struct {
	evaluator *messagepolicy.Evaluator

	// Test seams (nil in production).
	newSendBudget        func() *ruleengine.BudgetTracker
	evalOverride         func(ctx context.Context, pol *store.MessagePolicy, act messagepolicy.Activation, tracker *ruleengine.BudgetTracker) (messagepolicy.Classification, bool)
	testShadowBeforeEval func()

	mu         sync.Mutex
	started    bool
	stopped    bool
	shadowCh   chan messagePolicyShadowJob
	workerCtx  context.Context
	workerStop context.CancelFunc
	workersWG  sync.WaitGroup
}

type messagePolicyShadowJob struct {
	policies      []store.MessagePolicy
	snapshot      h2FactSnapshot
	activation    messagepolicy.Activation
	hasActivation bool
	messageID     string
}

type h2FactSnapshot struct {
	surface         string
	projectID       string
	senderID        string
	senderSlug      string
	recipientKind   string
	recipientID     string
	recipientSlug   string
	msgType         string
	msgBody         string
	hasChoices      bool
	urgent          bool
	attachmentCount int
	convKind        string
	convID          string
}

type h2PolicyInput struct {
	agent               *store.Agent
	req                 *OutboundMessageRequest
	routing             *routingResult
	hasChoices          bool
	artifactAttachments int
	messageID           string
}

type h2PolicyOutcome struct {
	humanMembers       []chatMemberEntry
	humanMembersLoaded bool
}

// initMessagePolicyGate binds the gate to the Server's shared ruleengine.Engine.
func (s *Server) initMessagePolicyGate() error {
	ev, err := messagepolicy.New(s.RuleEngine())
	if err != nil {
		return err
	}
	s.messagePolicyGate.evaluator = ev
	return nil
}

// MessagePolicyEvaluator returns the Hub's shared *messagepolicy.Evaluator
// bound to s.RuleEngine(), for policy CRUD cache invalidation and dry-run test
// handlers.
func (s *Server) MessagePolicyEvaluator() *messagepolicy.Evaluator {
	if s == nil {
		return nil
	}
	return s.messagePolicyGate.evaluator
}

func (g *messagePolicyGateRuntime) makeSendBudget() *ruleengine.BudgetTracker {
	if g.newSendBudget != nil {
		return g.newSendBudget()
	}
	return messagepolicy.NewSendBudget()
}

func (s *Server) enqueueMessagePolicyShadow(job messagePolicyShadowJob) {
	rt := &s.messagePolicyGate
	rt.mu.Lock()
	if rt.stopped {
		rt.mu.Unlock()
		return
	}
	if !rt.started {
		rt.started = true
		rt.shadowCh = make(chan messagePolicyShadowJob, messagePolicyShadowQueueCap)
		rt.workerCtx, rt.workerStop = context.WithCancel(context.Background())
		for i := 0; i < messagePolicyShadowWorkers; i++ {
			rt.workersWG.Add(1)
			go s.runMessagePolicyShadowWorker(rt.workerCtx, rt.shadowCh)
		}
	}
	select {
	case rt.shadowCh <- job:
		rt.mu.Unlock()
	default:
		rt.mu.Unlock()
		s.recordMessagePolicyShadowDrop(job.snapshot.projectID, len(job.policies))
	}
}

func (s *Server) runMessagePolicyShadowWorker(ctx context.Context, ch <-chan messagePolicyShadowJob) {
	defer s.messagePolicyGate.workersWG.Done()
	for job := range ch {
		if hook := s.messagePolicyGate.testShadowBeforeEval; hook != nil {
			hook()
		}
		if ctx.Err() != nil {
			continue
		}
		s.processMessagePolicyShadowJob(ctx, job)
	}
}

func (s *Server) processMessagePolicyShadowJob(ctx context.Context, job messagePolicyShadowJob) {
	act := job.activation
	if !job.hasActivation {
		facts, _, _ := s.buildH2MessagePolicyFacts(ctx, job.snapshot, nil, false)
		act = messagepolicy.NewActivation(facts)
	}
	budget := s.messagePolicyGate.makeSendBudget()
	tplData := messagepolicy.TemplateData{
		Recipient: job.snapshot.recipientSlug,
	}
	for i := range job.policies {
		pol := &job.policies[i]
		start := time.Now()
		classification := s.evalOneMessagePolicy(ctx, pol, act, budget)
		elapsed := time.Since(start)

		s.recordMessagePolicyEvalMetric(classification.Class, store.MessagePolicyModeShadow, job.snapshot.surface, elapsed)
		if classification.AdmissionError {
			s.recordMessagePolicyAdmissionError(job.snapshot.projectID, pol.ID, pol.Name, classification.Reason)
		}

		tplData.Policy = pol.Name
		outcome := messagepolicy.ResolveOutcome(
			classification.Class,
			classification.Reason,
			pol.OnUncertain,
			pol.OnFailed,
			pol.OnFailedReasons,
		)
		dec := buildMessagePolicyDecisionRecord(
			pol,
			job.snapshot,
			job.messageID,
			classification,
			store.MessagePolicyActionShadow,
			outcome,
			tplData,
			elapsed,
		)
		s.persistMessagePolicyDecision(ctx, dec)
	}
}

// stopMessagePolicyGate stops new shadow submissions and drains queued shadow
// jobs up to min(ctx deadline, messagePolicyShadowDrainTimeout) before joining
// all worker goroutines. Safe to call multiple times or when never started.
func (s *Server) stopMessagePolicyGate(ctx context.Context) {
	rt := &s.messagePolicyGate
	rt.mu.Lock()
	if rt.stopped {
		rt.mu.Unlock()
		return
	}
	rt.stopped = true
	if !rt.started {
		rt.mu.Unlock()
		return
	}
	close(rt.shadowCh)
	workerStop := rt.workerStop
	rt.mu.Unlock()

	done := make(chan struct{})
	go func() {
		rt.workersWG.Wait()
		close(done)
	}()

	drainCtx, cancel := context.WithTimeout(ctx, messagePolicyShadowDrainTimeout)
	defer cancel()

	select {
	case <-done:
	case <-drainCtx.Done():
		if workerStop != nil {
			workerStop()
		}
		<-done
	}
	if workerStop != nil {
		workerStop()
	}
}

// evaluateH2MessagePolicies runs the H2 outbound message policy gate for
// non-agent-DM sends (agent_to_user and agent_to_conversation).
//
// When hub.message_policies is disabled, it performs zero store reads and
// returns (_, true). When an enforce policy rejects the send, it writes the
// HTTP 422 message_policy_rejected response, records all evaluated decisions,
// and returns (_, false) before any message persistence, attachment ingestion,
// mention fan-out, default-agent wake, or channel dispatch occurs.
func (s *Server) evaluateH2MessagePolicies(ctx context.Context, w http.ResponseWriter, in *h2PolicyInput) (h2PolicyOutcome, bool) {
	if !s.experimentEnabled(experiments.MessagePolicies) {
		return h2PolicyOutcome{}, true
	}
	if in == nil || in.agent == nil || in.agent.ProjectID == "" || in.req == nil || in.routing == nil {
		return h2PolicyOutcome{}, true
	}

	snap := deriveH2FactSnapshot(in)

	enabledTrue := true
	polList, err := s.store.ListMessagePolicies(ctx, store.MessagePolicyFilter{
		ProjectID: in.agent.ProjectID,
		Enabled:   &enabledTrue,
	}, store.ListOptions{
		Limit:          store.MaxMessagePoliciesPerProject,
		SkipTotalCount: true,
	})
	if err != nil {
		s.messageLog.Error("message policy gate: failed to load project policies (fail-closed)",
			"project_id", in.agent.ProjectID,
			"agent_id", in.agent.ID,
			"error", err,
		)
		writeErrorFromErr(w, err, "")
		return h2PolicyOutcome{}, false
	}

	var enforcePols []store.MessagePolicy
	var shadowPols []store.MessagePolicy
	matchedCount := 0
	for i := range polList.Items {
		p := &polList.Items[i]
		if !p.Matches(snap.surface, snap.senderID, snap.recipientKind, "") {
			continue
		}
		if p.Mode == store.MessagePolicyModeEnforce {
			enforcePols = append(enforcePols, *p)
		} else {
			shadowPols = append(shadowPols, *p)
		}
		matchedCount++
		if matchedCount >= store.MaxActiveMatchingMessagePolicies {
			break
		}
	}

	if matchedCount == 0 {
		return h2PolicyOutcome{}, true
	}

	// Fast path: only shadow policies matched. Enqueue off the hot path without
	// blocking or running participant/mention lookups synchronously.
	if len(enforcePols) == 0 {
		s.enqueueMessagePolicyShadow(messagePolicyShadowJob{
			policies:  shadowPols,
			snapshot:  snap,
			messageID: in.messageID,
		})
		return h2PolicyOutcome{}, true
	}

	facts, humanMembers, humanMembersLoaded := s.buildH2MessagePolicyFacts(ctx, snap, nil, false)
	act := messagepolicy.NewActivation(facts)
	budget := s.messagePolicyGate.makeSendBudget()

	type enforceEvalResult struct {
		policy         *store.MessagePolicy
		classification messagepolicy.Classification
		outcome        string
		elapsed        time.Duration
	}

	results := make([]enforceEvalResult, 0, len(enforcePols))
	var firstReject *enforceEvalResult

	for i := range enforcePols {
		pol := &enforcePols[i]
		start := time.Now()
		classification := s.evalOneMessagePolicy(ctx, pol, act, budget)
		elapsed := time.Since(start)

		s.recordMessagePolicyEvalMetric(classification.Class, store.MessagePolicyModeEnforce, snap.surface, elapsed)
		if classification.AdmissionError {
			s.recordMessagePolicyAdmissionError(snap.projectID, pol.ID, pol.Name, classification.Reason)
		}

		outcome := messagepolicy.ResolveOutcome(
			classification.Class,
			classification.Reason,
			pol.OnUncertain,
			pol.OnFailed,
			pol.OnFailedReasons,
		)
		entry := enforceEvalResult{
			policy:         pol,
			classification: classification,
			outcome:        outcome,
			elapsed:        elapsed,
		}
		results = append(results, entry)
		if outcome == messagepolicy.OutcomeReject && firstReject == nil {
			firstReject = &results[len(results)-1]
		}
	}

	// message_id is NULL (empty) whenever the send is rejected so decision rows
	// never reference an unpersisted message UUID.
	recordedMsgID := in.messageID
	if firstReject != nil {
		recordedMsgID = ""
	}

	tplData := messagepolicy.TemplateData{
		Recipient: snap.recipientSlug,
	}
	for i := range results {
		r := &results[i]
		tplData.Policy = r.policy.Name
		action := store.MessagePolicyActionAdmitted
		if r.outcome == messagepolicy.OutcomeReject {
			action = store.MessagePolicyActionRejected
		}
		dec := buildMessagePolicyDecisionRecord(
			r.policy,
			snap,
			recordedMsgID,
			r.classification,
			action,
			r.outcome,
			tplData,
			r.elapsed,
		)
		s.persistMessagePolicyDecision(ctx, dec)
	}

	if len(shadowPols) > 0 {
		s.enqueueMessagePolicyShadow(messagePolicyShadowJob{
			policies:      shadowPols,
			snapshot:      snap,
			activation:    act,
			hasActivation: true,
			messageID:     recordedMsgID,
		})
	}

	if firstReject != nil {
		pol := firstReject.policy
		tplData.Policy = pol.Name
		reason := renderPolicyRejectReason(pol, tplData)
		remediation := messagepolicy.RenderTemplate(pol.RejectRemediation, tplData)

		s.messageLog.Info("outbound message rejected by message policy",
			"code", string(MessageDenialPolicyRejected),
			"policy", pol.Name,
			"policy_revision", pol.Revision,
			"class", firstReject.classification.Class,
			"surface", snap.surface,
			"project_id", snap.projectID,
			"sender_id", snap.senderID,
			"recipient_kind", snap.recipientKind,
			"recipient_id", snap.recipientID,
		)

		details := map[string]interface{}{
			"policy":          pol.Name,
			"policy_revision": pol.Revision,
			"class":           firstReject.classification.Class,
		}
		if remediation != "" {
			details["remediation"] = remediation
		}
		writeError(w, http.StatusUnprocessableEntity, string(MessageDenialPolicyRejected), reason, details)
		return h2PolicyOutcome{}, false
	}

	return h2PolicyOutcome{
		humanMembers:       humanMembers,
		humanMembersLoaded: humanMembersLoaded,
	}, true
}

func (s *Server) evalOneMessagePolicy(
	ctx context.Context,
	pol *store.MessagePolicy,
	act messagepolicy.Activation,
	budget *ruleengine.BudgetTracker,
) messagepolicy.Classification {
	if fn := s.messagePolicyGate.evalOverride; fn != nil {
		if c, handled := fn(ctx, pol, act, budget); handled {
			return c
		}
	}
	ev := s.messagePolicyGate.evaluator
	if ev == nil {
		re := s.RuleEngine()
		if re == nil {
			return messagepolicy.ClassifyError(&ruleengine.UnsupportedDialectError{
				Dialect:  ruleengine.DialectCEL,
				Consumer: messagepolicy.ConsumerName,
				Reason:   ruleengine.ReasonConsumerNotRegistered,
			})
		}
		var err error
		ev, err = messagepolicy.New(re)
		if err != nil {
			return messagepolicy.ClassifyError(err)
		}
	}
	return ev.EvaluateViolation(ctx, pol.ID, pol.Revision, pol.ViolationRule, act, messagepolicy.WithBudget(budget))
}

func deriveH2FactSnapshot(in *h2PolicyInput) h2FactSnapshot {
	res := in.routing
	req := in.req
	agent := in.agent

	isGroup := (res.ConvResult != nil && res.ConvResult.Kind == "group") ||
		strings.HasPrefix(res.Recipient, "thread:") ||
		strings.HasPrefix(res.Recipient, "conv:") ||
		(req.ThreadID != "" && !strings.HasPrefix(req.ThreadID, "dm:") && !strings.HasPrefix(req.ThreadID, "agent:"))

	snap := h2FactSnapshot{
		projectID:       agent.ProjectID,
		senderID:        agent.ID,
		senderSlug:      agent.Slug,
		msgType:         req.Type,
		msgBody:         req.Msg,
		hasChoices:      in.hasChoices,
		urgent:          req.Urgent,
		attachmentCount: len(req.Attachments) + in.artifactAttachments,
		convID:          res.ConversationID,
	}
	if snap.convID == "" && res.ConvResult != nil {
		snap.convID = res.ConvResult.ConversationID
	}

	if isGroup {
		snap.surface = messagepolicy.SurfaceAgentToConversation
		snap.convKind = "group"
		snap.recipientKind = "conversation"
		snap.recipientID = snap.convID
		if snap.recipientID == "" {
			snap.recipientID = res.RecipientID
		}
		switch {
		case res.ConvResult != nil && res.ConvResult.DisplayName != "":
			snap.recipientSlug = res.ConvResult.DisplayName
		case strings.HasPrefix(res.Recipient, "thread:"):
			snap.recipientSlug = strings.TrimPrefix(res.Recipient, "thread:")
		case strings.HasPrefix(res.Recipient, "conv:"):
			snap.recipientSlug = strings.TrimPrefix(res.Recipient, "conv:")
		case req.ThreadID != "":
			snap.recipientSlug = req.ThreadID
		default:
			snap.recipientSlug = snap.recipientID
		}
	} else {
		snap.surface = messagepolicy.SurfaceAgentToUser
		snap.convKind = "direct"
		snap.recipientKind = "user"
		snap.recipientID = res.RecipientID
		snap.recipientSlug = strings.TrimPrefix(res.Recipient, "user:")
		if snap.recipientSlug == "" {
			snap.recipientSlug = res.RecipientID
		}
	}
	return snap
}

func (s *Server) buildH2MessagePolicyFacts(
	ctx context.Context,
	snap h2FactSnapshot,
	preloadedHumans []chatMemberEntry,
	humansLoaded bool,
) (messagepolicy.Facts, []chatMemberEntry, bool) {
	var mentionedAgentIDs []string
	if snap.projectID != "" && strings.Contains(snap.msgBody, "@") {
		proseAgentMentions := dropSelfMentionName(messages.ExtractProseMentions(snap.msgBody), snap.senderSlug)
		if len(proseAgentMentions) > 0 {
			if agents, err := listAllProjectAgents(ctx, s.store, snap.projectID); err == nil && len(agents) > 0 {
				slugToID := make(map[string]string, len(agents))
				for i := range agents {
					if agents[i].Slug != "" {
						slugToID[strings.ToLower(agents[i].Slug)] = agents[i].ID
					}
				}
				seen := make(map[string]bool, len(proseAgentMentions))
				for _, m := range proseAgentMentions {
					if id, ok := slugToID[strings.ToLower(m)]; ok && !seen[id] {
						seen[id] = true
						mentionedAgentIDs = append(mentionedAgentIDs, id)
					}
				}
			}
		}
	}

	var mentionedUserIDs []string
	humans := preloadedHumans
	if snap.projectID != "" && strings.Contains(snap.msgBody, "@") {
		if wrapped := messages.ExtractProseWrappedMentions(snap.msgBody); len(wrapped) > 0 {
			if !humansLoaded {
				humans = s.resolveProjectHumanMembers(ctx, snap.projectID)
				humansLoaded = true
			}
			mentionedUserIDs = mentionedHumanIDs(humans, wrapped, "")
		}
	}

	var agentParticipantIDs []string
	var userParticipantIDs []string
	if snap.convKind == "group" && snap.convID != "" {
		if parts, err := s.store.ListParticipants(ctx, snap.convID); err == nil {
			for _, p := range parts {
				if p.PrincipalID == "" {
					continue
				}
				switch p.PrincipalKind {
				case "agent":
					agentParticipantIDs = append(agentParticipantIDs, p.PrincipalID)
				case "user":
					userParticipantIDs = append(userParticipantIDs, p.PrincipalID)
				}
			}
		}
	}

	facts := messagepolicy.Facts{
		Surface: snap.surface,
		Sender: messagepolicy.SenderFacts{
			Kind:      "agent",
			ID:        snap.senderID,
			Slug:      snap.senderSlug,
			ProjectID: snap.projectID,
		},
		Recipient: messagepolicy.RecipientFacts{
			Kind: snap.recipientKind,
			ID:   snap.recipientID,
			Slug: snap.recipientSlug,
		},
		Message: messagepolicy.MessageFacts{
			Type:            snap.msgType,
			Body:            snap.msgBody,
			HasChoices:      snap.hasChoices,
			Urgent:          snap.urgent,
			AttachmentCount: snap.attachmentCount,
		},
		MentionedAgentIDs: mentionedAgentIDs,
		MentionedUserIDs:  mentionedUserIDs,
		Conversation: messagepolicy.ConversationFacts{
			Kind:                snap.convKind,
			AgentParticipantIDs: agentParticipantIDs,
			UserParticipantIDs:  userParticipantIDs,
		},
	}
	return facts, humans, humansLoaded
}

func renderPolicyRejectReason(pol *store.MessagePolicy, tplData messagepolicy.TemplateData) string {
	if pol == nil {
		return messagepolicy.DefaultRejectReason("")
	}
	if strings.TrimSpace(pol.RejectReason) == "" {
		return messagepolicy.DefaultRejectReason(pol.Name)
	}
	rendered := strings.TrimSpace(messagepolicy.RenderTemplate(pol.RejectReason, tplData))
	if rendered == "" {
		return messagepolicy.DefaultRejectReason(pol.Name)
	}
	return rendered
}

func buildMessagePolicyDecisionRecord(
	pol *store.MessagePolicy,
	snap h2FactSnapshot,
	messageID string,
	classification messagepolicy.Classification,
	action string,
	outcome string,
	tplData messagepolicy.TemplateData,
	elapsed time.Duration,
) *store.MessagePolicyDecision {
	var reason string
	switch {
	case classification.Class == messagepolicy.ClassPass:
		reason = ""
	case classification.Class == messagepolicy.ClassViolation || outcome == messagepolicy.OutcomeReject:
		reason = renderPolicyRejectReason(pol, tplData)
	default:
		reason = classification.Reason
	}

	var celErrReason string
	if classification.Class == messagepolicy.ClassFailed {
		celErrReason = truncateASCIIOrUTF8(classification.Reason, maxDecisionCELReasonBytes)
	}

	elapsedMs := elapsed.Milliseconds()
	if elapsedMs < 0 {
		elapsedMs = 0
	}

	return &store.MessagePolicyDecision{
		ProjectID:      snap.projectID,
		PolicyID:       pol.ID,
		PolicyName:     pol.Name,
		PolicyRevision: pol.Revision,
		Mode:           pol.Mode,
		Surface:        snap.surface,
		SenderKind:     "agent",
		SenderID:       snap.senderID,
		RecipientKind:  snap.recipientKind,
		RecipientID:    truncateASCIIOrUTF8(snap.recipientID, maxDecisionRecipientIDBytes),
		MessageID:      messageID,
		Class:          classification.Class,
		Action:         action,
		Reason:         reason,
		CELErrorReason: celErrReason,
		ElapsedMs:      elapsedMs,
	}
}

func (s *Server) persistMessagePolicyDecision(ctx context.Context, dec *store.MessagePolicyDecision) {
	if s.store == nil || dec == nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), messagePolicyDecisionWriteTimeout)
	defer cancel()
	if err := s.store.CreateMessagePolicyDecision(writeCtx, dec); err != nil {
		s.messageLog.Error("failed to persist message policy decision",
			"project_id", dec.ProjectID,
			"policy_id", dec.PolicyID,
			"mode", dec.Mode,
			"action", dec.Action,
			"class", dec.Class,
			"error", err,
		)
	}
}

func truncateASCIIOrUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}
