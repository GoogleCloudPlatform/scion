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
	"slices"
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

	// shadowDropReasonQueueFull labels shadow evaluations dropped because the
	// bounded shadow worker queue was full.
	shadowDropReasonQueueFull = "queue_full"
)

// OTelMessagePolicyMetrics exports content-free message policy counters and
// latency histograms (consumer design §5.8).
type OTelMessagePolicyMetrics struct {
	evaluations     metric.Int64Counter
	durationMs      metric.Float64Histogram
	shadowDrops     metric.Int64Counter
	admissionErrors metric.Int64Counter
	uncovered       metric.Int64Counter
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
	drops, err := meter.Int64Counter("scion.message_policy.shadow.drops", metric.WithUnit("{evaluation}"))
	if err != nil {
		return nil, err
	}
	admErrs, err := meter.Int64Counter("scion.message_policy.admission.errors", metric.WithUnit("{error}"))
	if err != nil {
		return nil, err
	}
	uncov, err := meter.Int64Counter("scion.message_policy.uncovered", metric.WithUnit("{message}"))
	if err != nil {
		return nil, err
	}
	return &OTelMessagePolicyMetrics{
		evaluations:     evals,
		durationMs:      dur,
		shadowDrops:     drops,
		admissionErrors: admErrs,
		uncovered:       uncov,
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
	if policyCount <= 0 {
		return
	}
	s.messagePolicyShadowDrops.Add(int64(policyCount))
	if m := s.messagePolicyMetrics.Load(); m != nil {
		m.shadowDrops.Add(context.Background(), int64(policyCount), metric.WithAttributes(
			attribute.String("reason", shadowDropReasonQueueFull),
		))
	}
	slog.Warn("message policy shadow queue full; dropping shadow evaluation batch",
		"project_id", projectID,
		"policy_count", policyCount,
	)
}

func (s *Server) recordMessagePolicyShadowCapDrop(projectID string, droppedCount int) {
	if droppedCount <= 0 {
		return
	}
	s.messagePolicyShadowDrops.Add(int64(droppedCount))
	if m := s.messagePolicyMetrics.Load(); m != nil {
		m.shadowDrops.Add(context.Background(), int64(droppedCount), metric.WithAttributes(
			attribute.String("reason", messagepolicy.ReasonPolicyCapExceeded),
		))
	}
	slog.Warn("message policy shadow per-send cap exceeded; dropping excess shadow policies",
		"project_id", projectID,
		"dropped_count", droppedCount,
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

func (s *Server) recordMessagePolicyUncovered(snap h2FactSnapshot) {
	if snap.uncoveredPath == "" {
		return
	}
	s.messagePolicyUncovered.Add(1)
	if m := s.messagePolicyMetrics.Load(); m != nil {
		m.uncovered.Add(context.Background(), 1, metric.WithAttributes(
			attribute.String("uncovered_path", snap.uncoveredPath),
		))
	}
	log := s.messageLog
	if log == nil {
		log = slog.Default()
	}
	log.Info("message policy gate: admitting send on uncovered path",
		"uncovered_path", snap.uncoveredPath,
		"project_id", snap.projectID,
		"sender_id", snap.senderID,
		"conversation_id", snap.convID,
	)
}

// messagePolicyGateRuntime holds the shared Evaluator wrapper, bounded shadow
// worker pool, and deterministic test seams for a Hub Server.
type messagePolicyGateRuntime struct {
	evaluator *messagepolicy.Evaluator

	// Test seams (nil in production).
	newSendBudget        func() *ruleengine.BudgetTracker
	newSystemOneBudget   func() time.Duration
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
	policies         []store.MessagePolicy
	snapshot         h2FactSnapshot
	facts            messagepolicy.Facts
	hasFacts         bool
	activation       messagepolicy.Activation
	hasActivation    bool
	factsUnavailable bool
	droppedCount     int
	messageID        string
}

type h2FactSnapshot struct {
	uncoveredPath   string
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
}

type h2PolicyOutcome struct {
	humanMembers       []chatMemberEntry
	humanMembersLoaded bool
	admittedDecisions  []*store.MessagePolicyDecision
	shadowJob          *messagePolicyShadowJob
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

func (g *messagePolicyGateRuntime) systemOneSendBudget() time.Duration {
	if g.newSystemOneBudget != nil {
		if d := g.newSystemOneBudget(); d > 0 {
			if d > messagepolicy.MaxSystemOneSendBudget {
				return messagepolicy.MaxSystemOneSendBudget
			}
			return d
		}
	}
	return messagepolicy.DefaultSystemOneSendBudget
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
	if ctx.Err() != nil {
		return
	}
	bodyTruncated := len(job.snapshot.msgBody) > messagepolicy.MaxBodyBytes
	hasSystemOne := false
	for i := range job.policies {
		if strings.TrimSpace(job.policies[i].SystemOneRule) != "" {
			hasSystemOne = true
			break
		}
	}
	factsUnavailable := job.factsUnavailable
	facts := job.facts
	act := job.activation
	if (!bodyTruncated || hasSystemOne) && !job.hasFacts && !factsUnavailable {
		builtFacts, _, _, factsErr := s.buildH2MessagePolicyFacts(ctx, job.snapshot, nil, false)
		if ctx.Err() != nil {
			return
		}
		if factsErr != nil {
			factsUnavailable = true
		} else {
			facts = builtFacts
			act = messagepolicy.NewActivation(facts)
		}
	}
	budget := s.messagePolicyGate.makeSendBudget()
	soBudget := s.messagePolicyGate.systemOneSendBudget()
	tplData := messagepolicy.TemplateData{
		Recipient: job.snapshot.recipientSlug,
	}
	for i := range job.policies {
		if ctx.Err() != nil {
			return
		}
		pol := &job.policies[i]
		polHasS1 := strings.TrimSpace(pol.SystemOneRule) != ""
		var classification messagepolicy.Classification
		var elapsed time.Duration
		switch {
		case !polHasS1 && bodyTruncated:
			classification = messagepolicy.Classification{
				Class:  messagepolicy.ClassFailed,
				Reason: messagepolicy.ReasonBodyTruncated,
			}
		case factsUnavailable:
			classification = messagepolicy.Classification{
				Class:  messagepolicy.ClassFailed,
				Reason: messagepolicy.ReasonFactsUnavailable,
			}
		default:
			start := time.Now()
			classification = s.evalOneMessagePolicyWithFacts(ctx, pol, job.snapshot.projectID, facts, act, budget, soBudget)
			elapsed = time.Since(start)
		}
		if ctx.Err() != nil {
			return
		}

		s.recordMessagePolicyEvalMetric(classification.Class, store.MessagePolicyModeShadow, job.snapshot.surface, elapsed)
		if classification.AdmissionError {
			s.recordMessagePolicyAdmissionError(job.snapshot.projectID, pol.ID, pol.Name, classification.Reason)
		}

		tplData.Policy = pol.Name
		tplData.JudgmentFlags = classification.Judgments.Flags
		tplData.JudgmentLabels = classification.Judgments.Labels
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
	if ctx.Err() != nil {
		writeError(w, 499, "request_canceled", "request context done", nil)
		return h2PolicyOutcome{}, false
	}

	snap := deriveH2FactSnapshot(in)
	if snap.uncoveredPath != "" {
		s.recordMessagePolicyUncovered(snap)
		return h2PolicyOutcome{}, true
	}
	if snap.surface == "" {
		return h2PolicyOutcome{}, true
	}

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
	if ctx.Err() != nil {
		writeError(w, 499, "request_canceled", "request context done", nil)
		return h2PolicyOutcome{}, false
	}

	var enforcePols []store.MessagePolicy
	var shadowPols []store.MessagePolicy
	droppedShadow := 0
	for i := range polList.Items {
		p := &polList.Items[i]
		if !p.Matches(snap.surface, snap.senderID, snap.recipientKind, "") {
			continue
		}
		if p.Mode == store.MessagePolicyModeEnforce {
			enforcePols = append(enforcePols, *p)
		} else if len(shadowPols) < store.MaxActiveMatchingMessagePolicies {
			shadowPols = append(shadowPols, *p)
		} else {
			droppedShadow++
		}
	}

	if len(enforcePols) == 0 && len(shadowPols) == 0 {
		return h2PolicyOutcome{}, true
	}

	// Fast path: only shadow policies matched. Defer enqueueing until the
	// message is persisted so shadow decision rows never reference an unstored
	// message UUID, and avoid running participant/mention lookups on the hot path.
	if len(enforcePols) == 0 {
		return h2PolicyOutcome{
			shadowJob: &messagePolicyShadowJob{
				policies:     shadowPols,
				snapshot:     snap,
				droppedCount: droppedShadow,
			},
		}, true
	}

	bodyTruncated := len(snap.msgBody) > messagepolicy.MaxBodyBytes
	hasSystemOneEnforce := false
	for i := 0; i < len(enforcePols) && i < store.MaxActiveMatchingMessagePolicies; i++ {
		if strings.TrimSpace(enforcePols[i].SystemOneRule) != "" {
			hasSystemOneEnforce = true
			break
		}
	}
	var (
		facts              messagepolicy.Facts
		humanMembers       []chatMemberEntry
		humanMembersLoaded bool
		factsErr           error
		act                messagepolicy.Activation
	)
	if !bodyTruncated || hasSystemOneEnforce {
		facts, humanMembers, humanMembersLoaded, factsErr = s.buildH2MessagePolicyFacts(ctx, snap, nil, false)
		if factsErr == nil {
			act = messagepolicy.NewActivation(facts)
		}
	}
	budget := s.messagePolicyGate.makeSendBudget()
	soBudget := s.messagePolicyGate.systemOneSendBudget()

	type enforceEvalResult struct {
		policy         *store.MessagePolicy
		classification messagepolicy.Classification
		outcome        string
		elapsed        time.Duration
	}

	results := make([]enforceEvalResult, len(enforcePols))
	if hasSystemOneEnforce && factsErr == nil {
		// Evaluate enforce policies carrying a system_one_rule concurrently under
		// the shared synchronous System One send deadline (consumer design §5.5).
		soCtx, soCancel := context.WithTimeout(ctx, soBudget)
		var wg sync.WaitGroup
		for i := 0; i < len(enforcePols) && i < store.MaxActiveMatchingMessagePolicies; i++ {
			pol := &enforcePols[i]
			if strings.TrimSpace(pol.SystemOneRule) == "" {
				continue
			}
			wg.Add(1)
			go func(idx int, p *store.MessagePolicy) {
				defer wg.Done()
				start := time.Now()
				cls := s.evalOneMessagePolicyWithFacts(soCtx, p, snap.projectID, facts, act, budget, soBudget)
				results[idx] = enforceEvalResult{
					policy:         p,
					classification: cls,
					elapsed:        time.Since(start),
				}
			}(i, pol)
		}
		wg.Wait()
		soCancel()
	}

	var firstReject *enforceEvalResult

	for i := range enforcePols {
		pol := &enforcePols[i]
		polHasS1 := strings.TrimSpace(pol.SystemOneRule) != ""
		var classification messagepolicy.Classification
		var elapsed time.Duration
		switch {
		case i >= store.MaxActiveMatchingMessagePolicies:
			classification = messagepolicy.Classification{
				Class:  messagepolicy.ClassFailed,
				Reason: messagepolicy.ReasonPolicyCapExceeded,
			}
		case !polHasS1 && bodyTruncated:
			classification = messagepolicy.Classification{
				Class:  messagepolicy.ClassFailed,
				Reason: messagepolicy.ReasonBodyTruncated,
			}
		case factsErr != nil:
			classification = messagepolicy.Classification{
				Class:  messagepolicy.ClassFailed,
				Reason: messagepolicy.ReasonFactsUnavailable,
			}
		case polHasS1 && results[i].policy != nil:
			classification = results[i].classification
			elapsed = results[i].elapsed
		default:
			start := time.Now()
			classification = s.evalOneMessagePolicyWithFacts(ctx, pol, snap.projectID, facts, act, budget, soBudget)
			elapsed = time.Since(start)
		}

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
		results[i] = enforceEvalResult{
			policy:         pol,
			classification: classification,
			outcome:        outcome,
			elapsed:        elapsed,
		}
		if outcome == messagepolicy.OutcomeReject && firstReject == nil {
			firstReject = &results[i]
		}
	}

	if ctx.Err() != nil {
		writeError(w, 499, "request_canceled", "request context done", nil)
		return h2PolicyOutcome{}, false
	}

	tplData := messagepolicy.TemplateData{
		Recipient: snap.recipientSlug,
	}

	if firstReject != nil {
		for i := range results {
			r := &results[i]
			tplData.Policy = r.policy.Name
			tplData.JudgmentFlags = r.classification.Judgments.Flags
			tplData.JudgmentLabels = r.classification.Judgments.Labels
			action := store.MessagePolicyActionAdmitted
			if r.outcome == messagepolicy.OutcomeReject {
				action = store.MessagePolicyActionRejected
			}
			dec := buildMessagePolicyDecisionRecord(
				r.policy,
				snap,
				"",
				r.classification,
				action,
				r.outcome,
				tplData,
				r.elapsed,
			)
			s.persistMessagePolicyDecision(ctx, dec)
		}
		if len(shadowPols) > 0 {
			s.recordMessagePolicyShadowCapDrop(snap.projectID, droppedShadow)
			s.enqueueMessagePolicyShadow(messagePolicyShadowJob{
				policies:         shadowPols,
				snapshot:         snap,
				facts:            facts,
				hasFacts:         (!bodyTruncated || hasSystemOneEnforce) && factsErr == nil,
				activation:       act,
				hasActivation:    (!bodyTruncated || hasSystemOneEnforce) && factsErr == nil,
				factsUnavailable: factsErr != nil,
				messageID:        "",
			})
		}

		pol := firstReject.policy
		tplData.Policy = pol.Name
		tplData.JudgmentFlags = firstReject.classification.Judgments.Flags
		tplData.JudgmentLabels = firstReject.classification.Judgments.Labels
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

	admittedDecisions := make([]*store.MessagePolicyDecision, 0, len(results))
	for i := range results {
		r := &results[i]
		tplData.Policy = r.policy.Name
		tplData.JudgmentFlags = r.classification.Judgments.Flags
		tplData.JudgmentLabels = r.classification.Judgments.Labels
		admittedDecisions = append(admittedDecisions, buildMessagePolicyDecisionRecord(
			r.policy,
			snap,
			"",
			r.classification,
			store.MessagePolicyActionAdmitted,
			r.outcome,
			tplData,
			r.elapsed,
		))
	}

	var shadowJob *messagePolicyShadowJob
	if len(shadowPols) > 0 {
		shadowJob = &messagePolicyShadowJob{
			policies:         shadowPols,
			snapshot:         snap,
			facts:            facts,
			hasFacts:         (!bodyTruncated || hasSystemOneEnforce) && factsErr == nil,
			activation:       act,
			hasActivation:    (!bodyTruncated || hasSystemOneEnforce) && factsErr == nil,
			factsUnavailable: factsErr != nil,
			droppedCount:     droppedShadow,
		}
	}

	return h2PolicyOutcome{
		humanMembers:       humanMembers,
		humanMembersLoaded: humanMembersLoaded,
		admittedDecisions:  admittedDecisions,
		shadowJob:          shadowJob,
	}, true
}

// recordAdmittedH2PolicyDecisions persists admitted enforce decisions and
// enqueues matching shadow policies once the outbound message has been stored
// (or accepted by the in-process persisting broker subscriber, which stores
// the pre-assigned PersistMessageID). Deferring until after storage prevents
// decision rows from referencing a message_id that failed channel validation,
// broker admission, or database persistence.
func (s *Server) recordAdmittedH2PolicyDecisions(ctx context.Context, out *h2PolicyOutcome, persistedMessageID string) {
	if out == nil {
		return
	}
	for _, dec := range out.admittedDecisions {
		dec.MessageID = persistedMessageID
		s.persistMessagePolicyDecision(ctx, dec)
	}
	out.admittedDecisions = nil
	if out.shadowJob != nil {
		job := *out.shadowJob
		job.messageID = persistedMessageID
		out.shadowJob = nil
		s.recordMessagePolicyShadowCapDrop(job.snapshot.projectID, job.droppedCount)
		s.enqueueMessagePolicyShadow(job)
	}
}

func (s *Server) evalOneMessagePolicy(
	ctx context.Context,
	pol *store.MessagePolicy,
	act messagepolicy.Activation,
	budget *ruleengine.BudgetTracker,
) messagepolicy.Classification {
	return s.evalOneMessagePolicyWithFacts(ctx, pol, "", messagepolicy.Facts{}, act, budget, s.messagePolicyGate.systemOneSendBudget())
}

func (s *Server) evalOneMessagePolicyWithFacts(
	ctx context.Context,
	pol *store.MessagePolicy,
	projectID string,
	facts messagepolicy.Facts,
	act messagepolicy.Activation,
	budget *ruleengine.BudgetTracker,
	soMaxEvalTime time.Duration,
) messagepolicy.Classification {
	if fn := s.messagePolicyGate.evalOverride; fn != nil {
		if c, handled := fn(ctx, pol, act, budget); handled {
			return c
		}
	}
	ev := s.messagePolicyGate.evaluator
	if ev == nil {
		return messagepolicy.ClassifyError(&ruleengine.UnsupportedDialectError{
			Dialect:  ruleengine.DialectCEL,
			Consumer: messagepolicy.ConsumerName,
			Reason:   ruleengine.ReasonConsumerNotRegistered,
		})
	}
	if strings.TrimSpace(pol.SystemOneRule) == "" {
		return ev.EvaluateViolation(ctx, pol.ID, pol.Revision, pol.ViolationRule, act, messagepolicy.WithBudget(budget))
	}
	return ev.EvaluatePolicy(
		ctx,
		pol.ID,
		pol.Revision,
		pol.ViolationRule,
		pol.SystemOneRule,
		projectID,
		facts,
		[]messagepolicy.EvalOption{messagepolicy.WithBudget(budget)},
		[]messagepolicy.EvalOption{messagepolicy.WithMaxEvalTime(soMaxEvalTime)},
	)
}

func deriveH2FactSnapshot(in *h2PolicyInput) h2FactSnapshot {
	res := in.routing
	req := in.req
	agent := in.agent

	var isGroup bool
	if res.ConvResult != nil {
		isGroup = res.ConvResult.Kind == "group"
	} else {
		isGroup = strings.HasPrefix(res.Recipient, "thread:") ||
			strings.HasPrefix(res.Recipient, "conv:") ||
			(req.ThreadID != "" && !strings.HasPrefix(req.ThreadID, "dm:") && !strings.HasPrefix(req.ThreadID, "agent:"))
	}

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
		return snap
	}

	snap.convKind = "direct"

	// A direct ConvResult whose DM key names two agents, with RecipientID empty
	// or equal to the non-sender agent, is an uncovered agent_dm send in M1.
	if res.ConvResult != nil && res.ConvResult.Kind == "direct" {
		if kindA, idA, kindB, idB, err := messages.ParseDMKey(res.ConvResult.ExternalRef); err == nil && kindA == "agent" && kindB == "agent" {
			var peerAgentID string
			var senderInKey bool
			switch {
			case idA == agent.ID:
				peerAgentID = idB
				senderInKey = true
			case idB == agent.ID:
				peerAgentID = idA
				senderInKey = true
			}
			if senderInKey && (res.RecipientID == "" || res.RecipientID == peerAgentID) {
				snap.uncoveredPath = messagepolicy.SurfaceAgentDM
				snap.recipientKind = "agent"
				snap.recipientID = peerAgentID
				if strings.HasPrefix(res.Recipient, "agent:") {
					snap.recipientSlug = strings.TrimPrefix(res.Recipient, "agent:")
				} else {
					snap.recipientSlug = peerAgentID
				}
				return snap
			}
		}
	}

	// Never classify a recipient as "user" unless its ID came from user resolution.
	if res.UserResolved && res.RecipientID != "" {
		snap.surface = messagepolicy.SurfaceAgentToUser
		snap.recipientKind = "user"
		snap.recipientID = res.RecipientID
		if strings.HasPrefix(res.Recipient, "user:") {
			snap.recipientSlug = strings.TrimPrefix(res.Recipient, "user:")
		} else if res.ConvResult == nil {
			snap.recipientSlug = res.Recipient
		}
		if snap.recipientSlug == "" {
			snap.recipientSlug = snap.recipientID
		}
	}
	return snap
}

func (s *Server) buildH2MessagePolicyFacts(
	ctx context.Context,
	snap h2FactSnapshot,
	preloadedHumans []chatMemberEntry,
	humansLoaded bool,
) (messagepolicy.Facts, []chatMemberEntry, bool, error) {
	var factsErr error

	mentionOpts := s.mentionOptions()
	var (
		mentionedAgentIDs []string
		projectAgents     []store.Agent
		agentsLoaded      bool
	)
	if snap.projectID != "" && strings.Contains(snap.msgBody, "@") {
		proseAgentMentions := dropSelfMentionName(messages.ExtractProseMentionsWithOptions(snap.msgBody, mentionOpts), snap.senderSlug)
		if len(proseAgentMentions) > 0 {
			agents, err := listAllProjectAgents(ctx, s.store, snap.projectID)
			if err != nil {
				factsErr = errors.Join(factsErr, err)
			} else {
				projectAgents = agents
				agentsLoaded = true
				if len(agents) > 0 {
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
	}

	var mentionedUserIDs []string
	humans := preloadedHumans
	if snap.projectID != "" && strings.Contains(snap.msgBody, "@") {
		if wrapped := messages.ExtractProseWrappedMentionsWithOptions(snap.msgBody, mentionOpts); len(wrapped) > 0 {
			if !humansLoaded {
				loadedHumans, err := s.projectHumanMembersStrict(ctx, snap.projectID)
				if err != nil {
					factsErr = errors.Join(factsErr, err)
				} else {
					humans = loadedHumans
					humansLoaded = true
				}
			}
			if humansLoaded {
				mentionedUserIDs = mentionedHumanIDs(humans, wrapped, "")
			}
		}
	}

	var agentParticipantIDs []string
	var userParticipantIDs []string
	var unmentionedAgentSlugs []string
	if snap.convKind == "group" && snap.convID != "" {
		parts, err := s.store.ListParticipants(ctx, snap.convID)
		if err != nil {
			factsErr = errors.Join(factsErr, err)
		} else {
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
			if len(agentParticipantIDs) > 0 && snap.projectID != "" {
				if !agentsLoaded {
					agents, err := listAllProjectAgents(ctx, s.store, snap.projectID)
					if err != nil {
						factsErr = errors.Join(factsErr, err)
					} else {
						projectAgents = agents
						agentsLoaded = true
					}
				}
				if agentsLoaded && len(projectAgents) > 0 {
					idToSlug := make(map[string]string, len(projectAgents))
					for i := range projectAgents {
						if projectAgents[i].Slug != "" {
							idToSlug[projectAgents[i].ID] = projectAgents[i].Slug
						}
					}
					for _, pid := range agentParticipantIDs {
						if pid == snap.senderID || slices.Contains(mentionedAgentIDs, pid) {
							continue
						}
						if slug := idToSlug[pid]; slug != "" && !slices.Contains(unmentionedAgentSlugs, slug) {
							unmentionedAgentSlugs = append(unmentionedAgentSlugs, slug)
						}
					}
					slices.Sort(unmentionedAgentSlugs)
				}
			}
		}
	}

	if factsErr != nil && ctx.Err() == nil {
		s.messageLog.Warn("message policy gate: fact lookup failed",
			"project_id", snap.projectID,
			"surface", snap.surface,
			"error", factsErr,
		)
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
			Kind:                  snap.convKind,
			AgentParticipantIDs:   agentParticipantIDs,
			UserParticipantIDs:    userParticipantIDs,
			UnmentionedAgentSlugs: unmentionedAgentSlugs,
		},
	}
	return facts, humans, humansLoaded, factsErr
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
		Evaluation:     messagepolicy.MarshalEvaluationProjection(classification.Evaluation),
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
