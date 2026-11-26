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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// copyFactsFingerprint pins the pre-effect Seam A snapshot for one copy target:
// conversation kind, conversation ID (group) or DM key (direct), planned
// attempted attachment count (O3), sorted participant ID lists, sorted
// unmentioned_agent_slugs used, and copy body + surface hash.
//
// Disclosed O3 consequence (O-e): if post-persistence origin attachment binding
// drops any reference, attachment-bearing copies are refused with copy-facts
// drift before any copy addressee, DM creation, persistence, or dispatch; the
// already-admitted origin remains delivered.
type copyFactsFingerprint struct {
	Surface               string
	BodyHash              string
	ConvKind              string
	ConvRef               string
	AttachmentCount       int
	AgentParticipantIDs   []string
	UserParticipantIDs    []string
	UnmentionedAgentSlugs []string
}

func hashCopyBody(surface, body string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(surface))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(body))
	return hex.EncodeToString(h.Sum(nil))
}

func sortedClone(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	cp := slices.Clone(in)
	slices.Sort(cp)
	return cp
}

func computeCopyFactsFingerprint(surface, body, convKind, convRef string, attachmentCount int, facts messagepolicy.Facts) copyFactsFingerprint {
	return copyFactsFingerprint{
		Surface:               surface,
		BodyHash:              hashCopyBody(surface, body),
		ConvKind:              convKind,
		ConvRef:               convRef,
		AttachmentCount:       attachmentCount,
		AgentParticipantIDs:   sortedClone(facts.Conversation.AgentParticipantIDs),
		UserParticipantIDs:    sortedClone(facts.Conversation.UserParticipantIDs),
		UnmentionedAgentSlugs: sortedClone(facts.Conversation.UnmentionedAgentSlugs),
	}
}

func (f copyFactsFingerprint) Equal(other copyFactsFingerprint) bool {
	return f.Surface == other.Surface &&
		f.BodyHash == other.BodyHash &&
		f.ConvKind == other.ConvKind &&
		f.ConvRef == other.ConvRef &&
		f.AttachmentCount == other.AttachmentCount &&
		slices.Equal(f.AgentParticipantIDs, other.AgentParticipantIDs) &&
		slices.Equal(f.UserParticipantIDs, other.UserParticipantIDs) &&
		slices.Equal(f.UnmentionedAgentSlugs, other.UnmentionedAgentSlugs)
}

// copyTargetSnapshot captures one resolved copy target's pre-effect Seam A
// facts, lookup errors, and pinned fingerprint.
type copyTargetSnapshot struct {
	target          resolvedCopyTarget
	snap            h2FactSnapshot
	facts           messagepolicy.Facts
	factsErr        error
	unmentionedErr  error
	needUnmentioned bool
	convRef         string
	fingerprint     copyFactsFingerprint
}

type copyAdmittedTarget struct {
	agentID         string
	fingerprint     copyFactsFingerprint
	needUnmentioned bool
}

// copyAdmissionRegistry is a per-send registry owned by the origin handler
// invocation. Entries are registered by pointer on mint and removed when the
// fan-out/wake returns, the aggregate context finishes, or the origin handler
// defers cleanup.
//
// acknowledgedRegistrations records agent participant IDs absent from the
// pre-effect baseline P0 for which this send's E3b registerGroupPrimary or a
// delivered copy's ensureGroupParticipants returned nil (R1).
//
// Limitation (documented per R1): store.EnsureParticipant returning nil does
// NOT prove this send inserted the row — a concurrent third-party addition of
// the same ID during this send's acknowledged registration cannot be
// attributed separately and is fact-equivalent, and an acknowledged new ID may
// later be absent or re-added without detection. Baseline P0 removals and
// additions outside acknowledgedRegistrations remain strictly refused.
type copyAdmissionRegistry struct {
	mu                        sync.Mutex
	closed                    bool
	entries                   map[*derivedAdmission]struct{}
	acknowledgedRegistrations map[string]struct{}
}

func (r *copyAdmissionRegistry) register(adm *derivedAdmission) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[*derivedAdmission]struct{}, 1)
	}
	r.entries[adm] = struct{}{}
}

func (r *copyAdmissionRegistry) closeEntry(adm *derivedAdmission) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, adm)
	r.closed = true
}

func (r *copyAdmissionRegistry) isOpen(adm *derivedAdmission) bool {
	if r == nil || adm == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	_, ok := r.entries[adm]
	return ok
}

func (r *copyAdmissionRegistry) recordAcknowledgedRegistration(adm *derivedAdmission, agentID string) {
	if r == nil || adm == nil || agentID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if _, open := r.entries[adm]; !open {
		return
	}
	if _, inBaseline := adm.baselineAgentParticipants[agentID]; inBaseline {
		return
	}
	if r.acknowledgedRegistrations == nil {
		r.acknowledgedRegistrations = make(map[string]struct{}, 1)
	}
	r.acknowledgedRegistrations[agentID] = struct{}{}
}

func (r *copyAdmissionRegistry) acknowledgedSet() map[string]struct{} {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.acknowledgedRegistrations) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(r.acknowledgedRegistrations))
	for id := range r.acknowledgedRegistrations {
		out[id] = struct{}{}
	}
	return out
}

// derivedAdmission is the trusted, unexported, pre-effect proof minted ONLY by
// mintCopyAdmission after the origin message policy gate admits a send.
//
// Runtime linkage is enforced by per-send registry pointer identity plus
// originMessageID (attached once after origin persistence). sendID is
// diagnostic only (not a cross-send linkage check).
type derivedAdmission struct {
	registry                  *copyAdmissionRegistry
	sendID                    string // diagnostic only
	senderAgentID             string
	senderIdentityKind        string
	evalProjectID             string
	copyBodyHash              string
	targets                   map[string]copyAdmittedTarget
	baselineAgentParticipants map[string]struct{}
	policySnapshotIDs         []string

	mu              sync.Mutex
	originMessageID string
	attached        bool
}

type copyAdmissionMintInput struct {
	sendID             string
	senderAgentID      string
	senderIdentityKind string
	evalProjectID      string
	copyBody           string
	snapshots          []copyTargetSnapshot
	policySnapshotIDs  []string
}

// mintCopyAdmission is the SOLE function permitted to construct a
// derivedAdmission value (enforced by TestCopyIntent_ASTGuard). It is called
// pre-effect by the origin gate immediately after the origin plan admits.
func mintCopyAdmission(in copyAdmissionMintInput) *derivedAdmission {
	reg := &copyAdmissionRegistry{}
	targets := make(map[string]copyAdmittedTarget, len(in.snapshots))
	baselineAgents := make(map[string]struct{})
	for _, ts := range in.snapshots {
		if ts.target.Agent == nil {
			continue
		}
		targets[ts.target.Agent.ID] = copyAdmittedTarget{
			agentID:         ts.target.Agent.ID,
			fingerprint:     ts.fingerprint,
			needUnmentioned: ts.needUnmentioned,
		}
		if ts.fingerprint.ConvKind == "group" {
			for _, pid := range ts.fingerprint.AgentParticipantIDs {
				baselineAgents[pid] = struct{}{}
			}
		}
	}
	adm := &derivedAdmission{
		registry:                  reg,
		sendID:                    in.sendID,
		senderAgentID:             in.senderAgentID,
		senderIdentityKind:        in.senderIdentityKind,
		evalProjectID:             in.evalProjectID,
		copyBodyHash:              hashCopyBody(messagepolicy.SurfaceMentionDelivery, in.copyBody),
		targets:                   targets,
		baselineAgentParticipants: baselineAgents,
		policySnapshotIDs:         slices.Clone(in.policySnapshotIDs),
	}
	reg.register(adm)
	return adm
}

// AttachMessageID binds the persisted origin message ID to the admission once.
func (a *derivedAdmission) AttachMessageID(msgID string) {
	if a == nil || msgID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.attached {
		a.originMessageID = msgID
		a.attached = true
	}
}

// RecordAcknowledgedRegistration records an agent participant ID absent from
// P0 for which this send's own E3b registerGroupPrimary or delivered copy's
// ensureGroupParticipants returned nil (R1).
func (a *derivedAdmission) RecordAcknowledgedRegistration(agentID string) {
	if a == nil || a.registry == nil || agentID == "" {
		return
	}
	a.registry.recordAcknowledgedRegistration(a, agentID)
}

// verifyParentLinkage enforces originMessageID parent linkage against the
// admission's registry-bound origin message ID (sendID != "" is a diagnostic
// sanity check only).
//
// Disclosed F2 consequence: if an origin row fails to persist (no persisted
// originMessageID attached), copies carrying a ParentMessageID are refused
// fail-closed rather than delivered with a dangling mention_of.
func (a *derivedAdmission) verifyParentLinkage(parentMessageID string) error {
	if a == nil || a.sendID == "" {
		return errCopyAdmissionMismatch
	}
	a.mu.Lock()
	attached := a.attached
	originID := a.originMessageID
	a.mu.Unlock()

	if parentMessageID != "" {
		if !attached || originID != parentMessageID {
			return errCopyAdmissionMismatch
		}
		return nil
	}
	if attached {
		return errCopyAdmissionMismatch
	}
	return nil
}

// Close releases the admission from its per-send registry. Subsequent copy
// verifications fail closed.
func (a *derivedAdmission) Close() {
	if a == nil || a.registry == nil {
		return
	}
	a.registry.closeEntry(a)
}

func (a *derivedAdmission) isOpen() bool {
	if a == nil || a.registry == nil {
		return false
	}
	return a.registry.isOpen(a)
}

func identityKindOf(ident Identity) string {
	if ident == nil {
		return ""
	}
	switch ident.(type) {
	case AgentIdentity:
		return "agent"
	case *DelegatedAgentIdentity:
		return "delegated_agent"
	case UserIdentity:
		return "user"
	default:
		return ident.Type()
	}
}

// buildCopyTargetSnapshot constructs pre-effect Seam A facts and fingerprint
// for one authorized copy target without creating any conversation.
func (s *Server) buildCopyTargetSnapshot(
	ctx context.Context,
	sender *store.Agent,
	target resolvedCopyTarget,
	copyBody string,
	attachmentCount int,
	urgent bool,
	preloadedHumans []chatMemberEntry,
	humansLoaded bool,
	needUnmentioned bool,
) (copyTargetSnapshot, []chatMemberEntry, bool) {
	snap := h2FactSnapshot{
		surface:         messagepolicy.SurfaceMentionDelivery,
		projectID:       sender.ProjectID,
		senderID:        sender.ID,
		senderSlug:      sender.Slug,
		recipientKind:   "agent",
		recipientID:     target.Agent.ID,
		recipientSlug:   target.Agent.Slug,
		msgType:         messages.TypeMention,
		introBody:       copyBody,
		msgBody:         copyBody,
		hasChoices:      false,
		urgent:          urgent,
		attachmentCount: attachmentCount,
	}

	var (
		convRef  string
		factsErr error
	)

	if target.ParentConvVerified && target.ParentConv != nil && target.ParentConv.Kind == "group" &&
		s.groupConversationBelongsToProject(ctx, target.ParentConv.ConversationID, sender.ProjectID) {
		snap.convKind = "group"
		snap.convID = target.ParentConv.ConversationID
		convRef = target.ParentConv.ConversationID
	} else {
		snap.convKind = "direct"
		dmKey, keyErr := messages.DMConversationKey("agent", sender.ID, "agent", target.Agent.ID)
		if keyErr != nil {
			factsErr = errors.Join(factsErr, keyErr)
		} else {
			convRef = dmKey
			if s.store != nil {
				conv, lookupErr := s.store.GetConversationByExternalRef(ctx, "native", dmKey)
				if lookupErr != nil && !errors.Is(lookupErr, store.ErrNotFound) {
					factsErr = errors.Join(factsErr, lookupErr)
				} else if conv != nil {
					snap.convID = conv.ID
				}
			}
		}
	}

	builtFacts, humans, loaded, buildErr, unmentionedErr := s.buildH2MessagePolicyFacts(ctx, snap, preloadedHumans, humansLoaded, needUnmentioned)
	if buildErr != nil {
		factsErr = errors.Join(factsErr, buildErr)
	}

	fp := computeCopyFactsFingerprint(snap.surface, copyBody, snap.convKind, convRef, attachmentCount, builtFacts)
	return copyTargetSnapshot{
		target:          target,
		snap:            snap,
		facts:           builtFacts,
		factsErr:        factsErr,
		unmentionedErr:  unmentionedErr,
		needUnmentioned: needUnmentioned,
		convRef:         convRef,
		fingerprint:     fp,
	}, humans, loaded
}

var (
	errCopyAdmissionMissing   = errors.New("copy delivery refused: missing derived admission")
	errCopyAdmissionClosed    = errors.New("copy delivery refused: derived admission closed or expired")
	errCopyAdmissionMismatch  = errors.New("copy delivery refused: derived admission mismatch")
	errCopyAdmissionTarget    = errors.New("copy delivery refused: target not in admitted copy set")
	errCopyAdmissionFacts     = errors.New("copy delivery refused: copy facts drifted from admitted snapshot")
	errCopyAdmissionFactsRead = errors.New("copy delivery refused: copy facts lookup failed")
)

func (s *Server) verifyCopyAdmissionCommon(
	ctx context.Context,
	adm *derivedAdmission,
	sender *store.Agent,
	senderIdent Identity,
	target *store.Agent,
	copyBody string,
	parentMessageID string,
) (copyAdmittedTarget, error) {
	if adm == nil {
		return copyAdmittedTarget{}, errCopyAdmissionMissing
	}
	if ctx != nil && ctx.Err() != nil {
		return copyAdmittedTarget{}, errCopyAdmissionClosed
	}
	if !adm.isOpen() {
		return copyAdmittedTarget{}, errCopyAdmissionClosed
	}
	if err := adm.verifyParentLinkage(parentMessageID); err != nil {
		return copyAdmittedTarget{}, err
	}
	if sender == nil || senderIdent == nil || target == nil {
		return copyAdmittedTarget{}, errCopyAdmissionMismatch
	}
	if adm.senderAgentID != sender.ID ||
		adm.senderIdentityKind != identityKindOf(senderIdent) ||
		adm.evalProjectID != sender.ProjectID {
		return copyAdmittedTarget{}, errCopyAdmissionMismatch
	}
	if adm.copyBodyHash != hashCopyBody(messagepolicy.SurfaceMentionDelivery, copyBody) {
		return copyAdmittedTarget{}, errCopyAdmissionMismatch
	}
	admittedTarget, ok := adm.targets[target.ID]
	if !ok {
		return copyAdmittedTarget{}, errCopyAdmissionTarget
	}
	return admittedTarget, nil
}

// verifyCopyFingerprintMatches checks the live Seam A fingerprint against the
// admitted pre-effect baseline under the R1 participant normalization rule:
//   - Surface, BodyHash, ConvKind, ConvRef, AttachmentCount (O3), and
//     UserParticipantIDs must match identically.
//   - For group conversations, let P0 be admitted.AgentParticipantIDs, L be
//     live.AgentParticipantIDs, and R be the per-send acknowledged-registration
//     set (IDs absent from P0 whose registration returned nil in this send).
//     Require P0 ⊆ L ⊆ P0 ∪ R.
//   - All participant-derived fields (UnmentionedAgentSlugs) are normalized
//     consistently against L \ R (without mutating judged policy facts).
func (s *Server) verifyCopyFingerprintMatches(
	ctx context.Context,
	adm *derivedAdmission,
	admittedTarget copyAdmittedTarget,
	liveFP copyFactsFingerprint,
	liveFacts messagepolicy.Facts,
	sender *store.Agent,
) error {
	admitted := admittedTarget.fingerprint
	if liveFP.Surface != admitted.Surface ||
		liveFP.BodyHash != admitted.BodyHash ||
		liveFP.ConvKind != admitted.ConvKind ||
		liveFP.ConvRef != admitted.ConvRef ||
		liveFP.AttachmentCount != admitted.AttachmentCount ||
		!slices.Equal(liveFP.UserParticipantIDs, admitted.UserParticipantIDs) {
		return errCopyAdmissionFacts
	}

	if admitted.ConvKind != "group" {
		if !slices.Equal(liveFP.AgentParticipantIDs, admitted.AgentParticipantIDs) ||
			!slices.Equal(liveFP.UnmentionedAgentSlugs, admitted.UnmentionedAgentSlugs) {
			return errCopyAdmissionFacts
		}
		return nil
	}

	ackSet := adm.registry.acknowledgedSet()
	if !participantSetMatchesBaseline(admitted.AgentParticipantIDs, liveFP.AgentParticipantIDs, ackSet) {
		return errCopyAdmissionFacts
	}

	if !admittedTarget.needUnmentioned {
		if !slices.Equal(liveFP.UnmentionedAgentSlugs, admitted.UnmentionedAgentSlugs) {
			return errCopyAdmissionFacts
		}
		return nil
	}

	// Normalize participant-derived UnmentionedAgentSlugs consistently against
	// L \ R (excluding IDs acknowledged by this send) for verify comparison.
	if len(ackSet) == 0 {
		if !slices.Equal(liveFP.UnmentionedAgentSlugs, admitted.UnmentionedAgentSlugs) {
			return errCopyAdmissionFacts
		}
		return nil
	}

	normalizedSlugs, err := s.computeNormalizedUnmentionedSlugs(ctx, sender, liveFacts, ackSet)
	if err != nil {
		return errCopyAdmissionFactsRead
	}
	if !slices.Equal(normalizedSlugs, admitted.UnmentionedAgentSlugs) {
		return errCopyAdmissionFacts
	}
	return nil
}

// participantSetMatchesBaseline enforces P0 ⊆ L ⊆ P0 ∪ R (R1).
func participantSetMatchesBaseline(p0, live []string, ackSet map[string]struct{}) bool {
	liveSet := make(map[string]struct{}, len(live))
	for _, id := range live {
		liveSet[id] = struct{}{}
	}
	p0Set := make(map[string]struct{}, len(p0))
	for _, id := range p0 {
		p0Set[id] = struct{}{}
		if _, ok := liveSet[id]; !ok {
			return false
		}
	}
	for _, id := range live {
		if _, inP0 := p0Set[id]; inP0 {
			continue
		}
		if _, inAck := ackSet[id]; inAck {
			continue
		}
		return false
	}
	return true
}

func (s *Server) computeNormalizedUnmentionedSlugs(
	ctx context.Context,
	sender *store.Agent,
	liveFacts messagepolicy.Facts,
	ackSet map[string]struct{},
) ([]string, error) {
	if sender == nil || sender.ProjectID == "" {
		return nil, nil
	}
	var baselineLiveIDs []string
	for _, pid := range liveFacts.Conversation.AgentParticipantIDs {
		if _, inAck := ackSet[pid]; inAck {
			continue
		}
		baselineLiveIDs = append(baselineLiveIDs, pid)
	}
	if len(baselineLiveIDs) == 0 {
		return nil, nil
	}
	projectAgents, err := listAllProjectAgents(ctx, s.store, sender.ProjectID)
	if err != nil {
		return nil, err
	}
	idToSlug := make(map[string]string, len(projectAgents))
	for i := range projectAgents {
		if projectAgents[i].Slug != "" {
			idToSlug[projectAgents[i].ID] = projectAgents[i].Slug
		}
	}
	var slugs []string
	for _, pid := range baselineLiveIDs {
		if pid == sender.ID || slices.Contains(liveFacts.MentionedAgentIDs, pid) {
			continue
		}
		if slug := idToSlug[pid]; slug != "" && !slices.Contains(slugs, slug) {
			slugs = append(slugs, slug)
		}
	}
	slices.Sort(slugs)
	return slugs, nil
}

// verifyCopyAdmissionPreflight runs inside deliverAgentMentionCopy BEFORE
// resolveMentionConversation and recordAttachmentAddressee so that a refused
// copy creates no DM conversation, no attachment addressee, no message row,
// and no dispatch (R5, O3, Contract §8).
func (s *Server) verifyCopyAdmissionPreflight(
	ctx context.Context,
	in agentMentionFanoutInput,
	target *store.Agent,
	position string,
) error {
	admittedTarget, err := s.verifyCopyAdmissionCommon(ctx, in.CopyAdmission, in.Sender, in.SenderIdent, target, in.Msg, in.ParentMessageID)
	if err != nil {
		return err
	}
	ts, _, _ := s.buildCopyTargetSnapshot(
		ctx,
		in.Sender,
		resolvedCopyTarget{
			Agent:              target,
			Position:           position,
			ParentConv:         in.ParentConv,
			ParentConvVerified: in.ParentConvVerified,
		},
		in.Msg,
		len(in.ParentAttachments),
		false,
		in.HumanMembers,
		in.HumanMembers != nil,
		admittedTarget.needUnmentioned,
	)
	if ts.factsErr != nil || (admittedTarget.needUnmentioned && ts.unmentionedErr != nil) {
		return errCopyAdmissionFactsRead
	}
	return s.verifyCopyFingerprintMatches(ctx, in.CopyAdmission, admittedTarget, ts.fingerprint, ts.facts, in.Sender)
}

// verifyCopyAdmissionAtDelivery runs inside ExecuteAgentDM in copy mode
// (input.copyIntent == true) after resolveMentionConversation and before
// migration, Phase 1b, and persistence (Contract §5A, §5B, §5C, O3).
func (s *Server) verifyCopyAdmissionAtDelivery(ctx context.Context, input *AgentDMInput) error {
	admittedTarget, err := s.verifyCopyAdmissionCommon(ctx, input.copyAdmission, input.SenderAgent, input.SenderIdentity, input.TargetAgent, input.Msg, input.copyParentMessageID)
	if err != nil {
		return err
	}

	var convKind, convRef, convID string
	if input.ConvResult != nil && input.ConvResult.Kind == "group" {
		convKind = "group"
		convID = input.ConvResult.ConversationID
		if convID == "" {
			convID = input.ConversationID
		}
		convRef = convID
	} else {
		convKind = "direct"
		dmKey, keyErr := messages.DMConversationKey("agent", input.SenderAgent.ID, "agent", input.TargetAgent.ID)
		if keyErr != nil {
			return fmt.Errorf("%w: %v", errCopyAdmissionFactsRead, keyErr)
		}
		convRef = dmKey
		convID = input.ConversationID
	}

	snap := h2FactSnapshot{
		surface:         messagepolicy.SurfaceMentionDelivery,
		projectID:       input.SenderAgent.ProjectID,
		senderID:        input.SenderAgent.ID,
		senderSlug:      input.SenderAgent.Slug,
		recipientKind:   "agent",
		recipientID:     input.TargetAgent.ID,
		recipientSlug:   input.TargetAgent.Slug,
		msgType:         messages.TypeMention,
		introBody:       input.Msg,
		msgBody:         input.Msg,
		urgent:          input.Urgent,
		attachmentCount: input.copyBoundAttachmentCount,
		convKind:        convKind,
		convID:          convID,
	}

	builtFacts, _, _, factsErr, unmentionedErr := s.buildH2MessagePolicyFacts(ctx, snap, nil, false, admittedTarget.needUnmentioned)
	if factsErr != nil || (admittedTarget.needUnmentioned && unmentionedErr != nil) {
		return errCopyAdmissionFactsRead
	}
	fp := computeCopyFactsFingerprint(snap.surface, input.Msg, convKind, convRef, input.copyBoundAttachmentCount, builtFacts)
	return s.verifyCopyFingerprintMatches(ctx, input.copyAdmission, admittedTarget, fp, builtFacts, input.SenderAgent)
}
