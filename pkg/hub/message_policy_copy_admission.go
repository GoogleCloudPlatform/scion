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
// conversation kind, conversation ID (group) or DM key (direct), sorted
// participant ID lists, sorted unmentioned_agent_slugs used, and copy body +
// surface hash.
type copyFactsFingerprint struct {
	Surface               string
	BodyHash              string
	ConvKind              string
	ConvRef               string
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

func computeCopyFactsFingerprint(surface, body, convKind, convRef string, facts messagepolicy.Facts) copyFactsFingerprint {
	return copyFactsFingerprint{
		Surface:               surface,
		BodyHash:              hashCopyBody(surface, body),
		ConvKind:              convKind,
		ConvRef:               convRef,
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
type copyAdmissionRegistry struct {
	mu      sync.Mutex
	closed  bool
	entries map[*derivedAdmission]struct{}
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

// derivedAdmission is the trusted, unexported, pre-effect proof minted ONLY by
// mintCopyAdmission after the origin message policy gate admits a send.
type derivedAdmission struct {
	registry           *copyAdmissionRegistry
	sendID             string
	senderAgentID      string
	senderIdentityKind string
	evalProjectID      string
	copyBodyHash       string
	targets            map[string]copyAdmittedTarget
	policySnapshotIDs  []string

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
// derivedAdmission literal (enforced by TestCopyIntent_ASTGuard). It is called
// pre-effect by the origin gate immediately after the origin plan admits.
func mintCopyAdmission(in copyAdmissionMintInput) *derivedAdmission {
	reg := &copyAdmissionRegistry{}
	targets := make(map[string]copyAdmittedTarget, len(in.snapshots))
	for _, ts := range in.snapshots {
		if ts.target.Agent == nil {
			continue
		}
		targets[ts.target.Agent.ID] = copyAdmittedTarget{
			agentID:         ts.target.Agent.ID,
			fingerprint:     ts.fingerprint,
			needUnmentioned: ts.needUnmentioned,
		}
	}
	adm := &derivedAdmission{
		registry:           reg,
		sendID:             in.sendID,
		senderAgentID:      in.senderAgentID,
		senderIdentityKind: in.senderIdentityKind,
		evalProjectID:      in.evalProjectID,
		copyBodyHash:       hashCopyBody(messagepolicy.SurfaceMentionDelivery, in.copyBody),
		targets:            targets,
		policySnapshotIDs:  slices.Clone(in.policySnapshotIDs),
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

	fp := computeCopyFactsFingerprint(snap.surface, copyBody, snap.convKind, convRef, builtFacts)
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

// verifyCopyAdmissionPreflight runs inside deliverAgentMentionCopy BEFORE
// resolveMentionConversation and recordAttachmentAddressee so that a refused
// copy creates no DM conversation, no attachment addressee, no message row,
// and no dispatch (R5, Contract §8).
func (s *Server) verifyCopyAdmissionPreflight(
	ctx context.Context,
	in agentMentionFanoutInput,
	target *store.Agent,
	position string,
) error {
	admittedTarget, err := s.verifyCopyAdmissionCommon(ctx, in.CopyAdmission, in.Sender, in.SenderIdent, target, in.Msg)
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
	if !ts.fingerprint.Equal(admittedTarget.fingerprint) {
		return errCopyAdmissionFacts
	}
	return nil
}

// verifyCopyAdmissionAtDelivery runs inside ExecuteAgentDM in copy mode
// (input.copyIntent == true) after resolveMentionConversation and before
// migration, Phase 1b, and persistence (Contract §5A, §5B, §5C).
func (s *Server) verifyCopyAdmissionAtDelivery(ctx context.Context, input *AgentDMInput) error {
	admittedTarget, err := s.verifyCopyAdmissionCommon(ctx, input.copyAdmission, input.SenderAgent, input.SenderIdentity, input.TargetAgent, input.Msg)
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
		attachmentCount: len(input.Attachments) + len(input.AttachRefs) + len(input.Deliveries),
		convKind:        convKind,
		convID:          convID,
	}

	builtFacts, _, _, factsErr, unmentionedErr := s.buildH2MessagePolicyFacts(ctx, snap, nil, false, admittedTarget.needUnmentioned)
	if factsErr != nil || (admittedTarget.needUnmentioned && unmentionedErr != nil) {
		return errCopyAdmissionFactsRead
	}
	fp := computeCopyFactsFingerprint(snap.surface, input.Msg, convKind, convRef, builtFacts)
	if !fp.Equal(admittedTarget.fingerprint) {
		return errCopyAdmissionFacts
	}
	return nil
}
