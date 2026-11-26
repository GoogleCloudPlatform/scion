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

package artifacts

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// FeatureTransfer is the server-side experiment that gates the ownership
// transfer routes and ArtifactResponse.CanTransfer. It equals
// experiments.ArtifactsTransfer.
const FeatureTransfer = "hub.artifacts_transfer"

const (
	transferSegment        = "transfer"
	transferTargetsSegment = "transfer-targets"

	// maxTransferRequestBytes caps the JSON body of POST /{id}/transfer.
	maxTransferRequestBytes int64 = 64 << 10
)

// Target ineligibility internal reason codes (operator-only decision log;
// the HTTP response always answers uniform 403 target_ineligible).
const (
	TargetReasonNotFound      = "target_not_found"
	TargetReasonOtherProject  = "other_project"
	TargetReasonNotControlled = "not_controlled"
	TargetReasonStanding      = "standing"
	TargetReasonNoRoot        = "no_root"
)

// TransferCandidate is one eligible target agent returned by
// GET /api/v1/artifacts/{id}/transfer-targets.
type TransferCandidate struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	Phase string `json:"phase"`
}

// OwnershipTransfer is the optional host extension for
// POST /api/v1/artifacts/{id}/transfer and GET /api/v1/artifacts/{id}/transfer-targets.
// It uses strings and bools only (design D13).
type OwnershipTransfer interface {
	// SessionCredentialAllowed reports whether the caller's credential is
	// an interactive session or dev credential (UATs are refused).
	SessionCredentialAllowed(ctx context.Context) bool
	// OwnerLive reports whether agent ownerAgentID exists and is not
	// soft-deleted. A lookup error answers (true, err): callers treat it as live.
	OwnerLive(ctx context.Context, ownerAgentID string) (bool, error)
	// ControlsAgent reports whether the caller of ctx controls agentID
	// (issuerControlsAgent on a live row). False on any lookup failure.
	ControlsAgent(ctx context.Context, agentID string) bool
	// TransferTarget checks that agentID may become the owner of an artifact
	// homed in homeScope on behalf of the caller of ctx, and returns the
	// user the new owner's provenance root resolves to (the new sponsor_ref).
	// ok=false for every ineligibility reason, with reason naming the internal
	// operator-log code (TargetReason*); err is a lookup failure.
	TransferTarget(ctx context.Context, agentID, homeScope string) (sponsor string, ok bool, reason string, err error)
	// TransferTargets lists the agents TransferTarget would admit (for the
	// picker and CLI name resolution), capped at 200, plus whether the list
	// was truncated.
	TransferTargets(ctx context.Context, homeScope string) (agents []TransferCandidate, truncated bool, err error)
}

// TransferAuditor is an optional host extension that mirrors committed
// ownership transfers into the hub's mutation audit log. Best-effort after
// commit; artifact_event is the source of truth.
type TransferAuditor interface {
	RecordTransfer(ctx context.Context, artifactID, fromOwner, toOwner, authority string)
}

// TransferNotice describes a committed ownership transfer for best-effort
// agent notifications.
type TransferNotice struct {
	ArtifactID      string
	Title           string
	FromAgentID     string
	ToAgentID       string
	ActorUserID     string
	FormerOwnerLive bool
}

// TransferNoticeResult reports whether best-effort notices were sent.
type TransferNoticeResult struct {
	NewOwner    bool `json:"newOwner"`
	FormerOwner bool `json:"formerOwner"`
}

// TransferRequest is the body of POST /api/v1/artifacts/{id}/transfer.
type TransferRequest struct {
	To          string `json:"to"`
	ExpectOwner string `json:"expectOwner"`
	ClearKey    bool   `json:"clearKey,omitempty"`
	// ScopeRef is reserved for future re-homing transfers; refused with
	// 400 invalid_target in this phase.
	ScopeRef string `json:"scopeRef,omitempty"`
}

// TransferResponse is the body of POST /api/v1/artifacts/{id}/transfer.
type TransferResponse struct {
	Artifact ArtifactResponse     `json:"artifact"`
	Notified TransferNoticeResult `json:"notified"`
}

// TransferTargetsResponse is the body of GET /api/v1/artifacts/{id}/transfer-targets.
type TransferTargetsResponse struct {
	Agents    []TransferCandidate `json:"agents"`
	Truncated bool                `json:"truncated"`
}

// SetTransferNotifier sets the callback notified after a committed ownership
// transfer. Best-effort; never undoes the transfer.
func (s *Service) SetTransferNotifier(fn func(context.Context, TransferNotice) TransferNoticeResult) {
	s.mu.Lock()
	s.transferNotifier = fn
	s.mu.Unlock()
}

func (s *Service) transferNotifierFn() func(context.Context, TransferNotice) TransferNoticeResult {
	s.mu.RLock()
	fn := s.transferNotifier
	s.mu.RUnlock()
	return fn
}

const (
	transferNotAllowedMessage = "only a steward of an agent-owned artifact may transfer its ownership"
	transferOwnerLiveMessage  = "cannot transfer ownership from a live agent you do not control"
	transferTargetIneligible  = "target agent is not eligible to take ownership of this artifact"
)

// canTransfer computes ArtifactResponse.CanTransfer for a readable artifact.
// It is advisory only; POST /{id}/transfer re-decides. It performs no extra
// host calls unless the caller already holds a steward authority (or admin
// grant on an agent-owned artifact) and the transfer experiment is on.
func (s *Service) canTransfer(ctx context.Context, a *Artifact, authority string) bool {
	if a == nil || a.OwnerKind != PrincipalKindAgent {
		return false
	}
	if !s.featureEnabled(ctx, FeatureTransfer) {
		return false
	}
	stewardAuth := authority
	if !isSteward(stewardAuth) {
		// A caller who holds both an admin grant and a steward clause gets
		// AuthorityAdminGrant from adminAuthorityWith; check stewardAuthority
		// only in that narrow case. Non-administrators (authority == "")
		// make zero extra host calls.
		if authority != AuthorityAdminGrant {
			return false
		}
		stewardAuth = s.stewardAuthority(ctx, a)
		if !isSteward(stewardAuth) {
			return false
		}
	}
	ot, ok := s.host.(OwnershipTransfer)
	if !ok || !ot.SessionCredentialAllowed(ctx) {
		return false
	}
	live, err := ot.OwnerLive(ctx, a.OwnerRef)
	if err != nil {
		return false
	}
	if live && !ot.ControlsAgent(ctx, a.OwnerRef) {
		return false
	}
	return true
}

// transferGate runs the shared pre-checks for POST /{id}/transfer and
// GET /{id}/transfer-targets: feature gate, read check, user/session/steward
// authority, and (optionally) live-owner check.
func (s *Service) transferCallerGate(w http.ResponseWriter, r *http.Request, id, action string) (b backend, a *Artifact, ot OwnershipTransfer, userID, authority string, ok bool) {
	ctx := r.Context()
	if !s.featureEnabled(ctx, FeatureTransfer) {
		writeNotFound(w)
		return b, nil, nil, "", "", false
	}
	b, a, ok = s.readableArtifact(w, r, id)
	if !ok {
		return b, nil, nil, "", "", false
	}
	kind, ref, _, hasPrincipal := s.host.Principal(ctx)
	if !hasPrincipal || kind != PrincipalKindUser {
		s.logRefusal(r, action, logReasonTransferNotAllowed, a)
		writeError(w, http.StatusForbidden, "transfer_not_allowed", transferNotAllowedMessage)
		return b, nil, nil, "", "", false
	}
	ot, ok = s.host.(OwnershipTransfer)
	if !ok {
		s.logDecision(r, slog.LevelWarn, logEntry{action: action, outcome: logOutcomeError, reason: logReasonUnavailable, artifact: a})
		writeError(w, http.StatusServiceUnavailable, "unavailable", "ownership transfer is not available on this hub")
		return b, nil, nil, "", "", false
	}
	if !ot.SessionCredentialAllowed(ctx) {
		s.logRefusal(r, action, logReasonSessionRequired, a)
		writeErrorDetails(w, http.StatusForbidden, "forbidden",
			"this operation requires an interactive session",
			map[string]any{"reason": "GOV_PENDING", "credential": "session_required"})
		return b, nil, nil, "", "", false
	}
	authority = s.stewardAuthority(ctx, a)
	if !isSteward(authority) {
		s.logRefusal(r, action, logReasonTransferNotAllowed, a)
		writeError(w, http.StatusForbidden, "transfer_not_allowed", transferNotAllowedMessage)
		return b, nil, nil, "", "", false
	}
	return b, a, ot, ref, authority, true
}

func (s *Service) handleTransferTargets(w http.ResponseWriter, r *http.Request, id string) {
	b, a, ot, _, authority, ok := s.transferCallerGate(w, r, id, logActionTransferTargets)
	if !ok {
		return
	}
	_ = b
	ctx := r.Context()
	ownerLive, err := ot.OwnerLive(ctx, a.OwnerRef)
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: transfer owner liveness check failed", "artifact_id", a.ID, "error", err)
		s.logDecision(r, slog.LevelWarn, logEntry{action: logActionTransferTargets, outcome: logOutcomeError, reason: logReasonUnavailable, artifact: a})
		writeError(w, http.StatusServiceUnavailable, "unavailable", "could not verify current owner status")
		return
	}
	orphaned := !ownerLive
	if ownerLive && !ot.ControlsAgent(ctx, a.OwnerRef) {
		s.logDecision(r, slog.LevelWarn, logEntry{
			action: logActionTransferTargets, outcome: logOutcomeRefused, reason: logReasonOwnerLive,
			artifact: a, authority: authority, orphanedOverride: &orphaned,
		})
		writeError(w, http.StatusForbidden, "owner_live", transferOwnerLiveMessage)
		return
	}
	agents, truncated, err := ot.TransferTargets(ctx, a.ScopeRef)
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: list transfer targets failed", "artifact_id", a.ID, "error", err)
		s.logDecision(r, slog.LevelWarn, logEntry{action: logActionTransferTargets, outcome: logOutcomeError, reason: logReasonUnavailable, artifact: a})
		writeError(w, http.StatusServiceUnavailable, "unavailable", "could not list eligible transfer targets")
		return
	}
	filtered := make([]TransferCandidate, 0, len(agents))
	for _, c := range agents {
		if c.ID == a.OwnerRef {
			continue
		}
		filtered = append(filtered, c)
	}
	s.logAllowed(r, logEntry{
		action: logActionTransferTargets, artifact: a, authority: authority,
		count: len(filtered), orphanedOverride: &orphaned,
	})
	writeJSON(w, http.StatusOK, TransferTargetsResponse{
		Agents:    filtered,
		Truncated: truncated,
	})
}

func (s *Service) handleTransfer(w http.ResponseWriter, r *http.Request, id string) {
	b, a, ot, userID, authority, ok := s.transferCallerGate(w, r, id, logActionTransfer)
	if !ok {
		return
	}
	ctx := r.Context()

	// Rate limit applies to attempts that pass caller authorization (§4.2).
	if !s.chargePrincipal(w, LimitTransfer, PrincipalKindUser, userID) {
		s.logRefusal(r, logActionTransfer, logReasonRateLimited, a)
		return
	}

	var req TransferRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxTransferRequestBytes+1))
	if err := dec.Decode(&req); err != nil {
		s.logRefusal(r, logActionTransfer, "invalid_request", a)
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON request body")
		return
	}
	if req.ScopeRef != "" {
		s.logRefusal(r, logActionTransfer, logReasonInvalidTarget, a)
		writeError(w, http.StatusBadRequest, "invalid_target", "moving an artifact's home project is not supported in ownership transfer")
		return
	}
	toKind, toID, toOK := parseAgentPrincipalRef(req.To)
	if !toOK || toKind != PrincipalKindAgent {
		s.logRefusal(r, logActionTransfer, logReasonInvalidTarget, a)
		writeError(w, http.StatusBadRequest, "invalid_target", "to must be an agent reference of the form agent:<uuid>")
		return
	}
	expectKind, expectID, expectOK := parseAgentPrincipalRef(req.ExpectOwner)
	if !expectOK || expectKind != PrincipalKindAgent {
		s.logRefusal(r, logActionTransfer, logReasonInvalidTarget, a)
		writeError(w, http.StatusBadRequest, "invalid_target", "expectOwner must be an agent reference of the form agent:<uuid>")
		return
	}
	if toKind == a.OwnerKind && toID == a.OwnerRef {
		s.logRefusal(r, logActionTransfer, logReasonInvalidTarget, a)
		writeError(w, http.StatusBadRequest, "invalid_target", "target agent already owns this artifact")
		return
	}

	// Pre-check expectOwner before live-owner / target checks.
	if expectKind != a.OwnerKind || expectID != a.OwnerRef {
		s.logRefusal(r, logActionTransfer, logReasonOwnerChanged, a)
		writeError(w, http.StatusConflict, "owner_changed", "the artifact's owner has changed; reload and try again")
		return
	}

	// Live-owner rule (§4.3).
	ownerLive, err := ot.OwnerLive(ctx, a.OwnerRef)
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: transfer owner liveness check failed", "artifact_id", a.ID, "error", err)
		s.logDecision(r, slog.LevelWarn, logEntry{action: logActionTransfer, outcome: logOutcomeError, reason: logReasonUnavailable, artifact: a})
		writeError(w, http.StatusServiceUnavailable, "unavailable", "could not verify current owner status")
		return
	}
	orphaned := !ownerLive
	if ownerLive && !ot.ControlsAgent(ctx, a.OwnerRef) {
		s.logDecision(r, slog.LevelWarn, logEntry{
			action: logActionTransfer, outcome: logOutcomeRefused, reason: logReasonOwnerLive,
			artifact: a, authority: authority, subjectKind: PrincipalKindAgent, subjectRef: toID,
			orphanedOverride: &orphaned,
		})
		writeError(w, http.StatusForbidden, "owner_live", transferOwnerLiveMessage)
		return
	}

	// Target eligibility (§4.3).
	sponsor, eligible, reason, err := ot.TransferTarget(ctx, toID, a.ScopeRef)
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: transfer target check failed", "artifact_id", a.ID, "error", err)
		s.logDecision(r, slog.LevelWarn, logEntry{
			action: logActionTransfer, outcome: logOutcomeError, reason: logReasonUnavailable,
			artifact: a, authority: authority, subjectKind: PrincipalKindAgent, subjectRef: toID,
			orphanedOverride: &orphaned,
		})
		writeError(w, http.StatusServiceUnavailable, "unavailable", "could not verify target agent eligibility")
		return
	}
	if !eligible || sponsor == "" {
		if reason == "" {
			reason = TargetReasonNoRoot
		}
		s.logDecision(r, slog.LevelWarn, logEntry{
			action: logActionTransfer, outcome: logOutcomeRefused, reason: reason,
			artifact: a, authority: authority, subjectKind: PrincipalKindAgent, subjectRef: toID,
			orphanedOverride: &orphaned,
		})
		writeError(w, http.StatusForbidden, "target_ineligible", transferTargetIneligible)
		return
	}

	// Check automatic sponsor review grant policy for the new sponsor.
	sponsorReviewGrant := false
	if sponsor != a.SponsorRef {
		if policy, ok := s.host.(SponsorReviewPolicy); ok {
			if err := policy.SponsorReviewGrant(ctx, a.ScopeRef, sponsor); err == nil {
				sponsorReviewGrant = true
			}
		}
	}

	actor := s.callerActor(ctx, a, authority)
	now := time.Now()
	if s.nowFn != nil {
		now = s.nowFn()
	}
	updated, res, err := b.store.TransferOwner(ctx, a.ID, TransferOptions{
		ExpectOwner:        Owner{Kind: expectKind, Ref: expectID},
		To:                 Owner{Kind: PrincipalKindAgent, Ref: toID},
		Sponsor:            sponsor,
		ClearKey:           req.ClearKey,
		SponsorReviewGrant: sponsorReviewGrant,
		FormerOwnerLive:    ownerLive,
	}, actor, now)
	if err != nil {
		switch {
		case errors.Is(err, ErrOwnerChanged):
			s.logDecision(r, slog.LevelWarn, logEntry{
				action: logActionTransfer, outcome: logOutcomeRefused, reason: logReasonOwnerChanged,
				artifact: a, authority: authority, subjectKind: PrincipalKindAgent, subjectRef: toID,
				orphanedOverride: &orphaned,
			})
			writeError(w, http.StatusConflict, "owner_changed", "the artifact's owner has changed; reload and try again")
		case errors.Is(err, ErrStewardPrecondition):
			s.logStewardPrecondition(r, logActionTransfer, a, err)
			writeNotFound(w)
		case errors.Is(err, ErrKeyConflict):
			s.logDecision(r, slog.LevelWarn, logEntry{
				action: logActionTransfer, outcome: logOutcomeRefused, reason: logReasonKeyConflict,
				artifact: a, authority: authority, subjectKind: PrincipalKindAgent, subjectRef: toID,
				orphanedOverride: &orphaned,
			})
			writeError(w, http.StatusConflict, "key_conflict", "target agent already owns an artifact with this key in the same project; pass clearKey to remove the key and transfer")
		case errors.Is(err, ErrNotFound), errors.Is(err, ErrAttachmentNotShareable):
			writeNotFound(w)
		default:
			slog.ErrorContext(ctx, "artifacts: transfer owner store failed", "artifact_id", a.ID, "error", err)
			s.logDecision(r, slog.LevelWarn, logEntry{
				action: logActionTransfer, outcome: logOutcomeError, reason: logReasonStoreError,
				artifact: a, authority: authority, subjectKind: PrincipalKindAgent, subjectRef: toID,
				orphanedOverride: &orphaned,
			})
			writeError(w, http.StatusServiceUnavailable, "unavailable", "could not complete ownership transfer")
		}
		return
	}

	// Decision log (§4.7).
	keyCleared := res.KeyCleared
	s.logAllowed(r, logEntry{
		action:           logActionTransfer,
		artifact:         updated,
		authority:        authority,
		subjectKind:      PrincipalKindAgent,
		subjectRef:       toID,
		formerOwnerRef:   a.OwnerRef,
		keyCleared:       &keyCleared,
		orphanedOverride: &orphaned,
	})

	// Hub mutation audit (§4.7, best-effort).
	if aud, ok := s.host.(TransferAuditor); ok {
		aud.RecordTransfer(ctx, updated.ID,
			PrincipalRef(a.OwnerKind, a.OwnerRef),
			PrincipalRef(PrincipalKindAgent, toID),
			authority)
	}

	// Best-effort agent notices (§4.6).
	var notified TransferNoticeResult
	if fn := s.transferNotifierFn(); fn != nil {
		notified = fn(ctx, TransferNotice{
			ArtifactID:      updated.ID,
			Title:           updated.Title,
			FromAgentID:     a.OwnerRef,
			ToAgentID:       toID,
			ActorUserID:     userID,
			FormerOwnerLive: ownerLive,
		})
	}

	// Recompute ArtifactResponse for the caller (§4.4).
	artResp := ArtifactResponse{Artifact: artifactInfo(updated)}
	if updated.CurrentSeq > 0 {
		if v, err := b.store.GetVersion(ctx, updated.ID, updated.CurrentSeq); err == nil {
			if files, err := b.store.ListFiles(ctx, v.ID); err == nil {
				artResp.Version = versionInfo(v, files)
			}
		}
	}
	if !s.capabilities(w, r, b, updated, &artResp) {
		return
	}
	writeJSON(w, http.StatusOK, TransferResponse{
		Artifact: artResp,
		Notified: notified,
	})
}

// parseAgentPrincipalRef parses "agent:<canonical-uuid>".
func parseAgentPrincipalRef(raw string) (kind, id string, ok bool) {
	k, ref, found := strings.Cut(strings.TrimSpace(raw), ":")
	if !found || k != PrincipalKindAgent {
		return "", "", false
	}
	if !canonicalID(ref) {
		return "", "", false
	}
	return k, strings.ToLower(ref), true
}
