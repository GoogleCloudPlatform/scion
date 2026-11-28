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
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// originPolicyEvalInput is the unified input for origin-mode message policy
// evaluation across E1/E2 (H2), E3a, E3b, and E3c.
type originPolicyEvalInput struct {
	sendID                 string
	sender                 *store.Agent
	senderIdent            Identity
	requireActorCrossCheck bool
	originSnap             h2FactSnapshot
	copyTargets            []resolvedCopyTarget
	copyBody               string
	copyAttachmentCount    int
	copyUrgent             bool
	preloadedHumans        []chatMemberEntry
	humansLoaded           bool
}

// originPolicyRejection carries the structured HTTP 422 (or 499/5xx) refusal
// when an origin send is rejected by the message policy gate.
type originPolicyRejection struct {
	httpStatus int
	code       string
	message    string
	details    map[string]interface{}
	storeErr   error
}

func (r *originPolicyRejection) writeHTTP(w http.ResponseWriter) {
	if r == nil {
		return
	}
	if r.storeErr != nil {
		writeErrorFromErr(w, r.storeErr, "")
		return
	}
	writeError(w, r.httpStatus, r.code, r.message, r.details)
}

func (r *originPolicyRejection) toAgentDMError() *AgentDMError {
	if r == nil {
		return nil
	}
	if r.storeErr != nil {
		return &AgentDMError{
			Code:       ErrCodeInternalError,
			Message:    "failed to evaluate message policies",
			HTTPStatus: http.StatusInternalServerError,
		}
	}
	return &AgentDMError{
		Code:       r.code,
		Message:    r.message,
		HTTPStatus: r.httpStatus,
		Details:    r.details,
	}
}

type policyEvalUnit struct {
	isOrigin               bool
	snap                   h2FactSnapshot
	facts                  messagepolicy.Facts
	act                    messagepolicy.Activation
	hasFacts               bool
	hasUnmentioned         bool
	factsUnavailable       bool
	unmentionedUnavailable bool
}

type matchedPolicyUnits struct {
	policy   store.MessagePolicy
	unitIdxs []int
}

type unitPolicyEvalRow struct {
	policy         *store.MessagePolicy
	unitIdx        int
	snap           h2FactSnapshot
	classification messagepolicy.Classification
	outcome        string
	elapsed        time.Duration
}

// verifySenderAuthorityCrossCheck enforces D5: the eval/credential project is
// always the sender's store project, cross-checked that the store agent record
// matches the authenticated or delegated actor ID (never the delegated issuer).
// A conflicting token project claim never overrides the stored agent's
// ProjectID (O1). Missing/unresolvable sender or invalid actor fails closed.
func verifySenderAuthorityCrossCheck(sender *store.Agent, senderIdent Identity) (string, error) {
	if sender == nil || sender.ID == "" || sender.ProjectID == "" {
		return "", errors.New("message policy gate: missing sender agent or sender project")
	}
	if senderIdent == nil {
		return "", errors.New("message policy gate: missing sender identity")
	}
	switch senderIdent.(type) {
	case AgentIdentity, *DelegatedAgentIdentity:
	default:
		return "", errors.New("message policy gate: invalid non-agent actor identity")
	}
	if sender.ID != senderIdent.ID() {
		return "", errors.New("message policy gate: sender agent ID does not match authenticated actor")
	}
	return sender.ProjectID, nil
}

// evaluateOriginMessagePolicies evaluates all matching enforce and shadow
// message policies across the origin unit and all resolved copy units in one
// coherent pre-effect pass (Contract §§3–8).
//
// When admitted, it mints a derivedAdmission pre-effect via mintCopyAdmission
// if len(in.copyTargets) > 0. When rejected, it persists enforce decision rows,
// enqueues shadow jobs, mints no admission, and returns a non-nil rejection.
func (s *Server) evaluateOriginMessagePolicies(ctx context.Context, in originPolicyEvalInput) (h2PolicyOutcome, *originPolicyRejection) {
	if !s.experimentEnabled(experiments.MessagePolicies) {
		return h2PolicyOutcome{}, nil
	}
	if ctx.Err() != nil {
		return h2PolicyOutcome{}, &originPolicyRejection{
			httpStatus: 499,
			code:       "request_canceled",
			message:    "request context done",
		}
	}

	if in.originSnap.uncoveredPath != "" {
		s.recordMessagePolicyUncovered(in.originSnap)
		return h2PolicyOutcome{}, nil
	}
	if in.originSnap.surface == "" && len(in.copyTargets) == 0 {
		return h2PolicyOutcome{}, nil
	}

	var evalProjectID string
	if in.requireActorCrossCheck || in.senderIdent != nil {
		var authErr error
		evalProjectID, authErr = verifySenderAuthorityCrossCheck(in.sender, in.senderIdent)
		if authErr != nil {
			s.messageLog.Error("message policy gate: sender authority cross-check failed (fail-closed)",
				"error", authErr,
			)
			return h2PolicyOutcome{}, &originPolicyRejection{
				httpStatus: http.StatusForbidden,
				code:       ErrCodeMessageDenied,
				message:    "Message delivery denied",
			}
		}
	} else {
		if in.sender == nil || in.sender.ProjectID == "" {
			return h2PolicyOutcome{}, nil
		}
		evalProjectID = in.sender.ProjectID
	}
	in.originSnap.projectID = evalProjectID

	enabledTrue := true
	polList, err := s.store.ListMessagePolicies(ctx, store.MessagePolicyFilter{
		ProjectID: evalProjectID,
		Enabled:   &enabledTrue,
	}, store.ListOptions{
		Limit:          store.MaxMessagePoliciesPerProject,
		SkipTotalCount: true,
	})
	if err != nil {
		s.messageLog.Error("message policy gate: failed to load project policies (fail-closed)",
			"project_id", evalProjectID,
			"agent_id", in.sender.ID,
			"error", err,
		)
		return h2PolicyOutcome{}, &originPolicyRejection{storeErr: err}
	}
	if ctx.Err() != nil {
		return h2PolicyOutcome{}, &originPolicyRejection{
			httpStatus: 499,
			code:       "request_canceled",
			message:    "request context done",
		}
	}

	// Build unit list: unit 0 is origin, units 1..K are authorized copy targets.
	units := make([]policyEvalUnit, 0, 1+len(in.copyTargets))
	units = append(units, policyEvalUnit{
		isOrigin: true,
		snap:     in.originSnap,
	})
	for _, ct := range in.copyTargets {
		if ct.Agent == nil {
			continue
		}
		units = append(units, policyEvalUnit{
			isOrigin: false,
			snap: h2FactSnapshot{
				surface:         messagepolicy.SurfaceMentionDelivery,
				projectID:       evalProjectID,
				senderID:        in.sender.ID,
				senderSlug:      in.sender.Slug,
				recipientKind:   "agent",
				recipientID:     ct.Agent.ID,
				recipientSlug:   ct.Agent.Slug,
				msgType:         "mention",
				introBody:       in.copyBody,
				msgBody:         in.copyBody,
				urgent:          in.copyUrgent,
				attachmentCount: in.copyAttachmentCount,
			},
		})
	}

	// Match policies across all units and apply per-send caps ONCE per policy
	// (Contract §7: enforce <= 10 active + <= 40 cap rows; shadow <= 10 active,
	// excess counted only).
	var (
		enforceMatches    []matchedPolicyUnits
		shadowMatches     []matchedPolicyUnits
		droppedShadow     int
		policySnapshotIDs []string
	)
	for i := range polList.Items {
		p := &polList.Items[i]
		var matchedIdxs []int
		for ui := range units {
			u := &units[ui]
			if u.snap.surface == "" {
				continue
			}
			recipAgentID := ""
			if u.snap.recipientKind == "agent" {
				recipAgentID = u.snap.recipientID
			}
			if p.Matches(u.snap.surface, u.snap.senderID, u.snap.recipientKind, recipAgentID) {
				matchedIdxs = append(matchedIdxs, ui)
			}
		}
		if len(matchedIdxs) == 0 {
			continue
		}
		policySnapshotIDs = append(policySnapshotIDs, p.ID)
		if p.Mode == store.MessagePolicyModeEnforce {
			enforceMatches = append(enforceMatches, matchedPolicyUnits{
				policy:   *p,
				unitIdxs: matchedIdxs,
			})
		} else if len(shadowMatches) < store.MaxActiveMatchingMessagePolicies {
			shadowMatches = append(shadowMatches, matchedPolicyUnits{
				policy:   *p,
				unitIdxs: matchedIdxs,
			})
		} else {
			droppedShadow++
		}
	}

	// Build Seam A copy snapshots pre-effect (lookup-only, zero conversation
	// creation) whenever there are authorized copy targets so both evaluation
	// and copy admission pinning use the exact same snapshot.
	humanMembers := in.preloadedHumans
	humanMembersLoaded := in.humansLoaded
	// Compute copy-snapshot unmentioned_agent_slugs needs strictly from active
	// ENFORCE policies (R2): shadow-only unmentioned lookups must never drive
	// enforce I/O or copy admission pinning/refusals.
	needUnmentionedCopies := false
	for i := 0; i < len(enforceMatches) && i < store.MaxActiveMatchingMessagePolicies; i++ {
		if slices.ContainsFunc(enforceMatches[i].unitIdxs, func(idx int) bool { return idx > 0 }) &&
			policyUsesUnmentionedAgentSlugs(&enforceMatches[i].policy) {
			needUnmentionedCopies = true
			break
		}
	}
	var copySnapshots []copyTargetSnapshot
	if len(in.copyTargets) > 0 {
		copySnapshots = make([]copyTargetSnapshot, 0, len(in.copyTargets))
		unitIdx := 1
		for _, ct := range in.copyTargets {
			if ct.Agent == nil {
				continue
			}
			ts, humans, loaded := s.buildCopyTargetSnapshot(
				ctx,
				in.sender,
				ct,
				in.copyBody,
				in.copyAttachmentCount,
				in.copyUrgent,
				humanMembers,
				humanMembersLoaded,
				needUnmentionedCopies,
			)
			humanMembers = humans
			humanMembersLoaded = loaded
			copySnapshots = append(copySnapshots, ts)
			units[unitIdx].snap = ts.snap
			units[unitIdx].facts = ts.facts
			units[unitIdx].factsUnavailable = ts.factsErr != nil
			units[unitIdx].unmentionedUnavailable = ts.unmentionedErr != nil
			units[unitIdx].hasUnmentioned = needUnmentionedCopies
			if ts.factsErr == nil {
				units[unitIdx].act = messagepolicy.NewActivation(ts.facts)
				units[unitIdx].hasFacts = true
			}
			unitIdx++
		}
	}

	mintAdmission := func() *derivedAdmission {
		if len(copySnapshots) == 0 {
			return nil
		}
		sendID := in.sendID
		if sendID == "" {
			sendID = api.NewUUID()
		}
		return mintCopyAdmission(copyAdmissionMintInput{
			sendID:             sendID,
			senderAgentID:      in.sender.ID,
			senderIdentityKind: identityKindOf(in.senderIdent),
			evalProjectID:      evalProjectID,
			copyBody:           in.copyBody,
			snapshots:          copySnapshots,
			policySnapshotIDs:  policySnapshotIDs,
		})
	}

	// Fast path: no enforce policies matched.
	if len(enforceMatches) == 0 {
		var shadowJob *messagePolicyShadowJob
		if len(shadowMatches) > 0 {
			shadowJob = buildMultiUnitShadowJob(shadowMatches, units, droppedShadow, evalProjectID)
		}
		return h2PolicyOutcome{
			humanMembers:       humanMembers,
			humanMembersLoaded: humanMembersLoaded,
			shadowJob:          shadowJob,
			copyAdmission:      mintAdmission(),
		}, nil
	}

	activeCount := len(enforceMatches)
	if activeCount > store.MaxActiveMatchingMessagePolicies {
		activeCount = store.MaxActiveMatchingMessagePolicies
	}
	activeEnforce := enforceMatches[:activeCount]
	capExceededEnforce := enforceMatches[activeCount:]

	// Build origin facts if any active enforce policy matches unit 0.
	originMatched := false
	originHasS1 := false
	needUnmentionedOrigin := false
	for i := range activeEnforce {
		if slices.Contains(activeEnforce[i].unitIdxs, 0) {
			originMatched = true
			if !activeEnforce[i].policy.HasChain() && strings.TrimSpace(activeEnforce[i].policy.SystemOneRule) != "" {
				originHasS1 = true
			}
			if policyUsesUnmentionedAgentSlugs(&activeEnforce[i].policy) {
				needUnmentionedOrigin = true
			}
		}
	}
	originBodyTruncated := len(units[0].snap.msgBody) > messagepolicy.MaxBodyBytes
	if originMatched && (!originBodyTruncated || originHasS1) {
		builtFacts, humans, loaded, factsErr, unmentionedErr := s.buildH2MessagePolicyFacts(
			ctx, units[0].snap, humanMembers, humanMembersLoaded, needUnmentionedOrigin,
		)
		humanMembers = humans
		humanMembersLoaded = loaded
		units[0].factsUnavailable = factsErr != nil
		units[0].unmentionedUnavailable = unmentionedErr != nil
		units[0].hasUnmentioned = needUnmentionedOrigin
		if factsErr == nil {
			units[0].facts = builtFacts
			units[0].act = messagepolicy.NewActivation(builtFacts)
			units[0].hasFacts = true
		}
	}

	budget := s.messagePolicyGate.makeSendBudget()
	soBudget := s.messagePolicyGate.systemOneSendBudget()

	rows := s.evaluateMatchedPoliciesAcrossUnits(ctx, activeEnforce, units, evalProjectID, budget, soBudget, store.MessagePolicyModeEnforce)

	// Append ONE policy_cap_exceeded row per excess enforce policy (Contract §7).
	for i := range capExceededEnforce {
		pol := &capExceededEnforce[i].policy
		firstUnitIdx := capExceededEnforce[i].unitIdxs[0]
		cls := messagepolicy.Classification{
			Class:  messagepolicy.ClassFailed,
			Reason: messagepolicy.ReasonPolicyCapExceeded,
		}
		s.recordMessagePolicyEvalMetric(cls.Class, store.MessagePolicyModeEnforce, units[firstUnitIdx].snap.surface, 0)
		outcome := messagepolicy.ResolveOutcome(cls.Class, cls.Reason, pol.OnUncertain, pol.OnFailed, pol.OnFailedReasons)
		rows = append(rows, unitPolicyEvalRow{
			policy:         pol,
			unitIdx:        firstUnitIdx,
			snap:           units[firstUnitIdx].snap,
			classification: cls,
			outcome:        outcome,
		})
	}

	if ctx.Err() != nil {
		return h2PolicyOutcome{}, &originPolicyRejection{
			httpStatus: 499,
			code:       "request_canceled",
			message:    "request context done",
		}
	}

	var firstReject *unitPolicyEvalRow
	for i := range rows {
		if rows[i].outcome == messagepolicy.OutcomeReject {
			firstReject = &rows[i]
			break
		}
	}

	if firstReject != nil {
		for i := range rows {
			r := &rows[i]
			tplData := messagepolicy.TemplateData{
				Policy:         r.policy.Name,
				Recipient:      r.snap.recipientSlug,
				JudgmentFlags:  r.classification.Judgments.Flags,
				JudgmentLabels: r.classification.Judgments.Labels,
			}
			action := store.MessagePolicyActionAdmitted
			if r.outcome == messagepolicy.OutcomeReject {
				action = store.MessagePolicyActionRejected
			}
			dec := buildMessagePolicyDecisionRecord(
				r.policy,
				r.snap,
				"",
				r.classification,
				action,
				r.outcome,
				tplData,
				r.elapsed,
			)
			s.persistMessagePolicyDecision(ctx, dec)
		}
		if len(shadowMatches) > 0 {
			s.recordMessagePolicyShadowCapDrop(evalProjectID, droppedShadow)
			job := buildMultiUnitShadowJob(shadowMatches, units, 0, evalProjectID)
			s.enqueueMessagePolicyShadow(*job)
		}

		pol := firstReject.policy
		tplData := messagepolicy.TemplateData{
			Policy:         pol.Name,
			Recipient:      firstReject.snap.recipientSlug,
			JudgmentFlags:  firstReject.classification.Judgments.Flags,
			JudgmentLabels: firstReject.classification.Judgments.Labels,
		}
		reason := renderClassificationRejectReason(pol, firstReject.classification, tplData)
		remediation := renderClassificationRejectRemediation(pol, firstReject.classification, tplData)

		s.messageLog.Info("outbound message rejected by message policy",
			"code", string(MessageDenialPolicyRejected),
			"policy", pol.Name,
			"policy_revision", pol.Revision,
			"class", firstReject.classification.Class,
			"surface", firstReject.snap.surface,
			"project_id", evalProjectID,
			"sender_id", firstReject.snap.senderID,
			"recipient_kind", firstReject.snap.recipientKind,
			"recipient_id", firstReject.snap.recipientID,
		)

		details := map[string]interface{}{
			"policy":          pol.Name,
			"policy_revision": pol.Revision,
			"class":           firstReject.classification.Class,
		}
		if remediation != "" {
			details["remediation"] = remediation
		}
		return h2PolicyOutcome{}, &originPolicyRejection{
			httpStatus: http.StatusUnprocessableEntity,
			code:       string(MessageDenialPolicyRejected),
			message:    reason,
			details:    details,
		}
	}

	admittedDecisions := make([]*store.MessagePolicyDecision, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		tplData := messagepolicy.TemplateData{
			Policy:         r.policy.Name,
			Recipient:      r.snap.recipientSlug,
			JudgmentFlags:  r.classification.Judgments.Flags,
			JudgmentLabels: r.classification.Judgments.Labels,
		}
		admittedDecisions = append(admittedDecisions, buildMessagePolicyDecisionRecord(
			r.policy,
			r.snap,
			"",
			r.classification,
			store.MessagePolicyActionAdmitted,
			r.outcome,
			tplData,
			r.elapsed,
		))
	}

	var shadowJob *messagePolicyShadowJob
	if len(shadowMatches) > 0 {
		shadowJob = buildMultiUnitShadowJob(shadowMatches, units, droppedShadow, evalProjectID)
	}

	return h2PolicyOutcome{
		humanMembers:       humanMembers,
		humanMembersLoaded: humanMembersLoaded,
		admittedDecisions:  admittedDecisions,
		shadowJob:          shadowJob,
		copyAdmission:      mintAdmission(),
	}, nil
}

// evaluateMatchedPoliciesAcrossUnits evaluates active policies (enforce or
// shadow) across origin and copy units:
//   - Origin (unit 0) never shares with copies (different surface).
//   - Recipient-independent policies on copy units (surface mention_delivery)
//     are evaluated ONCE per send and fanned out to every valid matched copy
//     recipient (lowest RecipientID is primary with Usage; secondaries carry
//     CacheHit=true, unique EvalID, SourceEvalID, cleared Usage).
//   - Recipient-dependent policies on copy units are evaluated once per
//     matched copy unit (no cross-recipient sharing).
func (s *Server) evaluateMatchedPoliciesAcrossUnits(
	ctx context.Context,
	matches []matchedPolicyUnits,
	units []policyEvalUnit,
	projectID string,
	budget *ruleengine.BudgetTracker,
	soBudget time.Duration,
	mode string,
) []unitPolicyEvalRow {
	if len(matches) == 0 {
		return nil
	}

	ev := s.messagePolicyGate.evaluator

	// Determine recipient-independence per matched policy (only needed when it
	// matches copy units).
	isIndependent := make([]bool, len(matches))
	for i := range matches {
		hasCopy := slices.ContainsFunc(matches[i].unitIdxs, func(idx int) bool { return idx > 0 })
		if !hasCopy || ev == nil {
			continue
		}
		pol := &matches[i].policy
		if pol.HasChain() {
			continue
		}
		indep, err := ev.IsRecipientIndependentCached(ctx, pol.ID, pol.Revision, pol.ViolationRule, pol.SystemOneRule)
		if err == nil && indep {
			isIndependent[i] = true
		}
	}

	// Collect per-context System One evaluations under one shared soCtx for
	// this mode (enforce or shadow, never shared across modes).
	soCtx, soCancel := context.WithTimeout(ctx, soBudget)
	defer soCancel()

	// 1. Origin unit (unit 0) System One batch.
	originS1Res := make(map[int]policyEvalResult)
	if len(units) > 0 && !units[0].factsUnavailable && units[0].hasFacts {
		var originPols []store.MessagePolicy
		var originMatchIdxs []int
		for i := range matches {
			if slices.Contains(matches[i].unitIdxs, 0) && !matches[i].policy.HasChain() && strings.TrimSpace(matches[i].policy.SystemOneRule) != "" {
				originPols = append(originPols, matches[i].policy)
				originMatchIdxs = append(originMatchIdxs, i)
			}
		}
		if len(originPols) > 0 {
			res := make([]policyEvalResult, len(originPols))
			s.evalPlannedSystemOnePolicies(soCtx, originPols, len(originPols), projectID, units[0].facts, units[0].act, budget, soBudget, units[0].unmentionedUnavailable, res)
			for j, mi := range originMatchIdxs {
				if res[j].policy != nil {
					originS1Res[mi] = res[j]
				}
			}
		}
	}

	// 2. Copy independent System One batch (evaluated ONCE on mention_delivery
	// using the primary valid copy unit's facts).
	indepCopyS1Res := make(map[int]policyEvalResult)
	var indepS1Pols []store.MessagePolicy
	var indepS1MatchIdxs []int
	var indepFactsUnit *policyEvalUnit
	for i := range matches {
		if !isIndependent[i] || matches[i].policy.HasChain() || strings.TrimSpace(matches[i].policy.SystemOneRule) == "" {
			continue
		}
		// Find first valid copy unit matched by this policy.
		for _, ui := range matches[i].unitIdxs {
			if ui > 0 && !units[ui].factsUnavailable && units[ui].hasFacts {
				if indepFactsUnit == nil {
					indepFactsUnit = &units[ui]
				}
				indepS1Pols = append(indepS1Pols, matches[i].policy)
				indepS1MatchIdxs = append(indepS1MatchIdxs, i)
				break
			}
		}
	}
	if len(indepS1Pols) > 0 && indepFactsUnit != nil {
		res := make([]policyEvalResult, len(indepS1Pols))
		s.evalPlannedSystemOnePolicies(soCtx, indepS1Pols, len(indepS1Pols), projectID, indepFactsUnit.facts, indepFactsUnit.act, budget, soBudget, false, res)
		for j, mi := range indepS1MatchIdxs {
			if res[j].policy != nil {
				indepCopyS1Res[mi] = res[j]
			}
		}
	}

	// 3. Copy dependent System One batches: one plan per copy unit (U1: no
	// cross-recipient sharing for dependent units).
	//
	// O2 disclosure: dependent per-recipient copy units run SEQUENTIALLY under
	// the shared soCtx/soBudget in Slice B (groups within each unit still run
	// concurrently via RunGroupPlan). With many copies and a slow dependent
	// System One policy, later units can hit the shared deadline and fail as
	// ClassFailed, yielding a latency-caused 422 under on_failed=reject.
	// Cross-unit concurrency is deferred to a separate follow-up.
	type unitMatchKey struct {
		matchIdx int
		unitIdx  int
	}
	depCopyS1Res := make(map[unitMatchKey]policyEvalResult)
	for ui := 1; ui < len(units); ui++ {
		if units[ui].factsUnavailable || !units[ui].hasFacts {
			continue
		}
		var depPols []store.MessagePolicy
		var depMatchIdxs []int
		for i := range matches {
			if isIndependent[i] || matches[i].policy.HasChain() || strings.TrimSpace(matches[i].policy.SystemOneRule) == "" {
				continue
			}
			if slices.Contains(matches[i].unitIdxs, ui) {
				depPols = append(depPols, matches[i].policy)
				depMatchIdxs = append(depMatchIdxs, i)
			}
		}
		if len(depPols) > 0 {
			res := make([]policyEvalResult, len(depPols))
			s.evalPlannedSystemOnePolicies(soCtx, depPols, len(depPols), projectID, units[ui].facts, units[ui].act, budget, soBudget, units[ui].unmentionedUnavailable, res)
			for j, mi := range depMatchIdxs {
				if res[j].policy != nil {
					depCopyS1Res[unitMatchKey{matchIdx: mi, unitIdx: ui}] = res[j]
				}
			}
		}
	}

	// Assemble decision rows in deterministic order.
	rows := make([]unitPolicyEvalRow, 0, len(matches)*2)
	for i := range matches {
		pol := &matches[i].policy
		polHasS1 := !pol.HasChain() && strings.TrimSpace(pol.SystemOneRule) != ""

		// 3a. Origin unit row (if matched).
		if slices.Contains(matches[i].unitIdxs, 0) {
			u := &units[0]
			bodyTruncated := len(u.snap.msgBody) > messagepolicy.MaxBodyBytes
			var cls messagepolicy.Classification
			var elapsed time.Duration
			switch {
			case pol.HasChain():
				cls, elapsed = s.evaluateChainPolicyOnUnit(ctx, pol, mode, 0, u.snap, u, budget)
			case !polHasS1 && bodyTruncated:
				cls = messagepolicy.Classification{Class: messagepolicy.ClassFailed, Reason: messagepolicy.ReasonBodyTruncated}
			case u.factsUnavailable || (u.unmentionedUnavailable && policyUsesUnmentionedAgentSlugs(pol)):
				cls = messagepolicy.Classification{Class: messagepolicy.ClassFailed, Reason: messagepolicy.ReasonFactsUnavailable}
			case polHasS1 && originS1Res[i].policy != nil:
				cls = originS1Res[i].classification
				elapsed = originS1Res[i].elapsed
			default:
				start := time.Now()
				cls = s.evalOneMessagePolicyWithFacts(ctx, pol, projectID, u.facts, u.act, budget, soBudget)
				elapsed = time.Since(start)
			}
			rows = append(rows, s.finalizeEvalRow(pol, 0, u.snap, cls, elapsed, mode))
		}

		// 3b. Copy unit rows (if matched).
		var matchedCopies []int
		for _, ui := range matches[i].unitIdxs {
			if ui > 0 {
				matchedCopies = append(matchedCopies, ui)
			}
		}
		if len(matchedCopies) == 0 {
			continue
		}

		if isIndependent[i] {
			// Partition into unavailable-facts copy units vs valid copy units.
			var validCopies []int
			var unavailCopies []int
			for _, ui := range matchedCopies {
				u := &units[ui]
				if u.factsUnavailable || (u.unmentionedUnavailable && policyUsesUnmentionedAgentSlugs(pol)) {
					unavailCopies = append(unavailCopies, ui)
				} else {
					validCopies = append(validCopies, ui)
				}
			}

			for _, ui := range unavailCopies {
				cls := messagepolicy.Classification{Class: messagepolicy.ClassFailed, Reason: messagepolicy.ReasonFactsUnavailable}
				rows = append(rows, s.finalizeEvalRow(pol, ui, units[ui].snap, cls, 0, mode))
			}

			if len(validCopies) > 0 {
				// Deterministic primary selection: lowest RecipientID (Contract §4 R4).
				sort.Slice(validCopies, func(a, b int) bool {
					return units[validCopies[a]].snap.recipientID < units[validCopies[b]].snap.recipientID
				})
				primaryUnitIdx := validCopies[0]
				primaryUnit := &units[primaryUnitIdx]
				bodyTruncated := len(primaryUnit.snap.msgBody) > messagepolicy.MaxBodyBytes

				var primaryCls messagepolicy.Classification
				var elapsed time.Duration
				switch {
				case !polHasS1 && bodyTruncated:
					primaryCls = messagepolicy.Classification{Class: messagepolicy.ClassFailed, Reason: messagepolicy.ReasonBodyTruncated}
				case polHasS1 && indepCopyS1Res[i].policy != nil:
					primaryCls = indepCopyS1Res[i].classification
					elapsed = indepCopyS1Res[i].elapsed
				default:
					start := time.Now()
					primaryCls = s.evalOneMessagePolicyWithFacts(ctx, pol, projectID, primaryUnit.facts, primaryUnit.act, budget, soBudget)
					elapsed = time.Since(start)
				}

				// Primary row carries usage; secondary rows carry CacheHit=true,
				// unique member EvalID, cleared Usage, and SourceEvalID.
				rows = append(rows, s.finalizeEvalRow(pol, primaryUnitIdx, primaryUnit.snap, primaryCls, elapsed, mode))
				sourceEvalID := primaryCls.SourceEvalID
				if sourceEvalID == "" && primaryCls.Evaluation != nil {
					sourceEvalID = primaryCls.Evaluation.EvalID
				}
				for _, secIdx := range validCopies[1:] {
					secCls := primaryCls
					if primaryCls.Evaluation != nil {
						secCls.Evaluation = messagepolicy.CloneEvaluationForSecondary(primaryCls.Evaluation, pol.ID, pol.Revision)
						secCls.SourceEvalID = sourceEvalID
					}
					rows = append(rows, s.finalizeEvalRow(pol, secIdx, units[secIdx].snap, secCls, elapsed, mode))
				}
			}
			continue
		}

		// Dependent policy: evaluate once per matched copy unit (U1).
		for _, ui := range matchedCopies {
			u := &units[ui]
			bodyTruncated := len(u.snap.msgBody) > messagepolicy.MaxBodyBytes
			var cls messagepolicy.Classification
			var elapsed time.Duration
			switch {
			case pol.HasChain():
				cls, elapsed = s.evaluateChainPolicyOnUnit(ctx, pol, mode, ui, u.snap, u, budget)
			case !polHasS1 && bodyTruncated:
				cls = messagepolicy.Classification{Class: messagepolicy.ClassFailed, Reason: messagepolicy.ReasonBodyTruncated}
			case u.factsUnavailable || (u.unmentionedUnavailable && policyUsesUnmentionedAgentSlugs(pol)):
				cls = messagepolicy.Classification{Class: messagepolicy.ClassFailed, Reason: messagepolicy.ReasonFactsUnavailable}
			case polHasS1 && depCopyS1Res[unitMatchKey{matchIdx: i, unitIdx: ui}].policy != nil:
				r := depCopyS1Res[unitMatchKey{matchIdx: i, unitIdx: ui}]
				cls = r.classification
				elapsed = r.elapsed
			default:
				start := time.Now()
				cls = s.evalOneMessagePolicyWithFacts(ctx, pol, projectID, u.facts, u.act, budget, soBudget)
				elapsed = time.Since(start)
			}
			rows = append(rows, s.finalizeEvalRow(pol, ui, u.snap, cls, elapsed, mode))
		}
	}

	return rows
}

func chainStepInputsFromStore(in []store.MessagePolicyStep) []messagepolicy.ChainStepInput {
	out := make([]messagepolicy.ChainStepInput, len(in))
	for i, st := range in {
		out[i] = messagepolicy.ChainStepInput{
			ID:          st.ID,
			Role:        st.Role,
			Dialect:     st.Dialect,
			Expr:        st.Expr,
			Reason:      st.Reason,
			Remediation: st.Remediation,
		}
	}
	return out
}

// evaluateChainPolicyOnUnit evaluates a persisted chain-native policy on one
// unit under P1 runtime boundaries (D3 chain_disabled, D4 chain_unsupported).
// It never executes the legacy sentinel columns.
func (s *Server) evaluateChainPolicyOnUnit(
	ctx context.Context,
	pol *store.MessagePolicy,
	mode string,
	unitIdx int,
	snap h2FactSnapshot,
	u *policyEvalUnit,
	budget *ruleengine.BudgetTracker,
) (messagepolicy.Classification, time.Duration) {
	// D3: hub.message_policy_chains off -> failed/chain_disabled via on_failed*.
	if !s.experimentEnabled(experiments.MessagePolicyChains) {
		return messagepolicy.Classification{
			Class:  messagepolicy.ClassFailed,
			Reason: messagepolicy.ReasonChainDisabled,
		}, 0
	}

	// D4: P1 supports E1/E2 origin enforce CEL-only chains only. Any other path
	// (shadow, copy unit, agent_dm, mention_delivery) or non-CEL chain shape
	// returns failed/chain_unsupported via on_failed* without running the chain
	// or the legacy sentinel.
	if pol == nil || pol.Chain == nil ||
		mode != store.MessagePolicyModeEnforce ||
		unitIdx != 0 ||
		!messagepolicy.IsP1ChainSurface(snap.surface) {
		return messagepolicy.Classification{
			Class:  messagepolicy.ClassFailed,
			Reason: messagepolicy.ReasonChainUnsupported,
		}, 0
	}
	steps := chainStepInputsFromStore(pol.Chain.Steps)
	if !messagepolicy.IsP1ChainSupported(steps) {
		return messagepolicy.Classification{
			Class:  messagepolicy.ClassFailed,
			Reason: messagepolicy.ReasonChainUnsupported,
		}, 0
	}

	if len(snap.msgBody) > messagepolicy.MaxBodyBytes {
		return messagepolicy.Classification{
			Class:  messagepolicy.ClassFailed,
			Reason: messagepolicy.ReasonBodyTruncated,
		}, 0
	}
	if u == nil || u.factsUnavailable || !u.hasFacts {
		return messagepolicy.Classification{
			Class:  messagepolicy.ClassFailed,
			Reason: messagepolicy.ReasonFactsUnavailable,
		}, 0
	}

	ev := s.messagePolicyGate.evaluator
	if ev == nil {
		return messagepolicy.ClassifyError(&ruleengine.UnsupportedDialectError{
			Dialect:  ruleengine.DialectCEL,
			Consumer: messagepolicy.ConsumerName,
			Reason:   ruleengine.ReasonConsumerNotRegistered,
		}), 0
	}

	start := time.Now()
	cls := ev.EvaluateChain(
		ctx,
		pol.ID,
		pol.Revision,
		steps,
		pol.Chain.RejectReason,
		pol.Chain.RejectRemediation,
		u.facts,
		messagepolicy.WithBudget(budget),
	)
	return cls, time.Since(start)
}

func (s *Server) finalizeEvalRow(
	pol *store.MessagePolicy,
	unitIdx int,
	snap h2FactSnapshot,
	cls messagepolicy.Classification,
	elapsed time.Duration,
	mode string,
) unitPolicyEvalRow {
	s.recordMessagePolicyEvalMetric(cls.Class, mode, snap.surface, elapsed)
	if cls.AdmissionError {
		s.recordMessagePolicyAdmissionError(snap.projectID, pol.ID, pol.Name, cls.Reason)
	}
	outcome := messagepolicy.ResolveOutcome(
		cls.Class,
		cls.Reason,
		pol.OnUncertain,
		pol.OnFailed,
		pol.OnFailedReasons,
	)
	return unitPolicyEvalRow{
		policy:         pol,
		unitIdx:        unitIdx,
		snap:           snap,
		classification: cls,
		outcome:        outcome,
		elapsed:        elapsed,
	}
}

func buildMultiUnitShadowJob(shadowMatches []matchedPolicyUnits, units []policyEvalUnit, droppedCount int, projectID string) *messagePolicyShadowJob {
	pols := make([]store.MessagePolicy, len(shadowMatches))
	for i := range shadowMatches {
		pols[i] = shadowMatches[i].policy
	}
	unitsCopy := make([]policyEvalUnit, len(units))
	copy(unitsCopy, units)
	matchesCopy := make([]matchedPolicyUnits, len(shadowMatches))
	for i := range shadowMatches {
		matchesCopy[i] = matchedPolicyUnits{
			policy:   shadowMatches[i].policy,
			unitIdxs: slices.Clone(shadowMatches[i].unitIdxs),
		}
	}
	// Restore A1/A2 shadowCanReuseFacts separation per unit (R2): if any
	// matched shadow policy for unit ui reads conversation.unmentioned_agent_slugs
	// and enforce did not compute unmentioned slugs for unit ui, clear hasFacts
	// on the shadow copy so the shadow worker rebuilds facts in its own context.
	for ui := range unitsCopy {
		shadowNeedsUnmentioned := false
		for i := range matchesCopy {
			if slices.Contains(matchesCopy[i].unitIdxs, ui) && policyUsesUnmentionedAgentSlugs(&matchesCopy[i].policy) {
				shadowNeedsUnmentioned = true
				break
			}
		}
		if shadowNeedsUnmentioned && !unitsCopy[ui].hasUnmentioned {
			unitsCopy[ui].hasFacts = false
			unitsCopy[ui].unmentionedUnavailable = false
		}
	}
	snap := h2FactSnapshot{projectID: projectID}
	if len(unitsCopy) > 0 {
		snap = unitsCopy[0].snap
	}
	return &messagePolicyShadowJob{
		policies:      pols,
		snapshot:      snap,
		droppedCount:  droppedCount,
		multiUnits:    unitsCopy,
		shadowMatches: matchesCopy,
		projectID:     projectID,
	}
}

func (s *Server) processMultiUnitShadowJob(ctx context.Context, job messagePolicyShadowJob) {
	if ctx.Err() != nil {
		return
	}
	units := job.multiUnits
	// Lazily build facts for any unit (origin or copy) whose shadow policies
	// need facts (or unmentioned_agent_slugs) that enforce did not build (R2).
	for ui := range units {
		if units[ui].hasFacts || units[ui].factsUnavailable {
			continue
		}
		unitMatched := false
		unitHasS1 := false
		needUnmentioned := false
		for i := range job.shadowMatches {
			if slices.Contains(job.shadowMatches[i].unitIdxs, ui) {
				unitMatched = true
				if !job.shadowMatches[i].policy.HasChain() && strings.TrimSpace(job.shadowMatches[i].policy.SystemOneRule) != "" {
					unitHasS1 = true
				}
				if policyUsesUnmentionedAgentSlugs(&job.shadowMatches[i].policy) {
					needUnmentioned = true
				}
			}
		}
		unitBodyTruncated := len(units[ui].snap.msgBody) > messagepolicy.MaxBodyBytes
		if unitMatched && (!unitBodyTruncated || unitHasS1) {
			builtFacts, _, _, factsErr, unmentionedErr := s.buildH2MessagePolicyFacts(ctx, units[ui].snap, nil, false, needUnmentioned)
			if ctx.Err() != nil {
				return
			}
			units[ui].factsUnavailable = factsErr != nil
			units[ui].unmentionedUnavailable = unmentionedErr != nil
			units[ui].hasUnmentioned = needUnmentioned
			if factsErr == nil {
				units[ui].facts = builtFacts
				units[ui].act = messagepolicy.NewActivation(builtFacts)
				units[ui].hasFacts = true
			}
		}
	}

	budget := s.messagePolicyGate.makeSendBudget()
	soBudget := s.messagePolicyGate.systemOneSendBudget()
	rows := s.evaluateMatchedPoliciesAcrossUnits(ctx, job.shadowMatches, units, job.projectID, budget, soBudget, store.MessagePolicyModeShadow)
	if ctx.Err() != nil {
		return
	}
	for i := range rows {
		if ctx.Err() != nil {
			return
		}
		r := &rows[i]
		tplData := messagepolicy.TemplateData{
			Policy:         r.policy.Name,
			Recipient:      r.snap.recipientSlug,
			JudgmentFlags:  r.classification.Judgments.Flags,
			JudgmentLabels: r.classification.Judgments.Labels,
		}
		dec := buildMessagePolicyDecisionRecord(
			r.policy,
			r.snap,
			job.messageID,
			r.classification,
			store.MessagePolicyActionShadow,
			r.outcome,
			tplData,
			r.elapsed,
		)
		s.persistMessagePolicyDecision(ctx, dec)
	}
}
