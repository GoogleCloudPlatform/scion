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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// FeatureReviewRequests is the server-side experiment that gates the
// review-request routes, response fields, and filters. It equals
// experiments.ArtifactsReviewRequests.
const FeatureReviewRequests = "hub.artifacts_review_requests"

const (
	reviewRequestsSegment = "review-requests"
	reviewRequestSegment  = "review-request"

	maxReviewRequestReviewers = 10
	maxReviewRequestNoteRunes = 1000
	maxReviewRequestBodyBytes = 16 << 10

	reviewRequestRemindCooldown = 24 * time.Hour
)

// Error and per-reviewer refusal codes for review requests (design §4.3, §4.4).
const (
	CodeReviewerUnavailable   = "reviewer_unavailable"
	CodeReviewerCannotReview  = "reviewer_cannot_review"
	CodeRequestClosed         = "request_closed"
	CodeAgentCannotRequestRev = "agent_cannot_request_review"
)

// CreateReviewRequestsBody is the JSON body of POST /api/v1/artifacts/{id}/review-requests.
type CreateReviewRequestsBody struct {
	Reviewers []string `json:"reviewers"`
	Note      string   `json:"note,omitempty"`
}

// CreateReviewRequestsResponse is the JSON response of POST /api/v1/artifacts/{id}/review-requests.
type CreateReviewRequestsResponse struct {
	Requests []ReviewRequestResult `json:"requests"`
}

// ReviewRequestResult is the per-reviewer result inside CreateReviewRequestsResponse.
type ReviewRequestResult struct {
	Reviewer      string             `json:"reviewer"`
	Outcome       string             `json:"outcome"`
	Code          string             `json:"code,omitempty"`
	Access        string             `json:"access,omitempty"`
	Request       *ReviewRequestInfo `json:"request,omitempty"`
	Notified      bool               `json:"notified"`
	NotifySkipped string             `json:"notifySkipped,omitempty"`
}

// DismissReviewRequestBody is the optional JSON body of POST /api/v1/artifacts/{id}/review-request/dismiss.
type DismissReviewRequestBody struct {
	Note string `json:"note,omitempty"`
}

// DismissReviewRequestResponse is the JSON response of POST /api/v1/artifacts/{id}/review-request/dismiss.
type DismissReviewRequestResponse struct {
	Request        ReviewRequestInfo `json:"request"`
	AuthorNotified bool              `json:"authorNotified"`
}

func (s *Service) reviewRequestInfo(ctx context.Context, rr ReviewRequest, names map[string]string) ReviewRequestInfo {
	reviewerRef := PrincipalRef(rr.ReviewerKind, rr.ReviewerRef)
	requesterRef := PrincipalRef(rr.RequesterKind, rr.RequesterRef)
	if names == nil {
		names = s.principalDisplayNames(ctx, []string{reviewerRef, requesterRef})
	}
	return ReviewRequestInfo{
		ID: rr.ID,
		Reviewer: ReviewRequestPrincipal{
			Ref:         rr.ReviewerRef,
			DisplayName: names[reviewerRef],
		},
		Requester: ReviewRequestPrincipal{
			Kind:        rr.RequesterKind,
			Ref:         rr.RequesterRef,
			DisplayName: names[requesterRef],
		},
		RequestedSeq: rr.RequestedSeq,
		Note:         rr.Note,
		Access:       rr.Access,
		State:        rr.State,
		ResponseNote: rr.ResponseNote,
		ResolvedSeq:  rr.ResolvedSeq,
		CreatedAt:    rr.CreatedAt,
		RemindedAt:   rr.RemindedAt,
		ResolvedAt:   rr.ResolvedAt,
	}
}

func (s *Service) principalDisplayNames(ctx context.Context, refs []string) map[string]string {
	namer, ok := s.host.(PrincipalNamer)
	if !ok || len(refs) == 0 {
		return nil
	}
	return namer.PrincipalDisplayNames(ctx, refs)
}

// populateReviewState sets resp.ReviewState on GET artifact / version responses
// while FeatureReviewRequests is enabled. While the experiment is off,
// resp.ReviewState stays nil and is omitted from JSON.
func (s *Service) populateReviewState(ctx context.Context, b backend, a *Artifact, resp *ArtifactResponse) {
	if !s.featureEnabled(ctx, FeatureReviewRequests) {
		return
	}
	state := &ReviewStateInfo{}
	kind, ref, _, ok := s.host.Principal(ctx)
	if ok && kind == PrincipalKindUser {
		rr, err := b.store.GetOpenReviewRequest(ctx, a.ID, kind, ref)
		if err == nil && rr != nil {
			info := s.reviewRequestInfo(ctx, *rr, nil)
			state.RequestedOfMe = &info
		} else if err != nil && !errors.Is(err, ErrNotFound) {
			slog.WarnContext(ctx, "artifacts: get open review request failed", "artifact", a.ID, "error", err)
		}
	}
	resp.ReviewState = state
}

// handleCreateReviewRequests serves POST /api/v1/artifacts/{id}/review-requests.
// In Phase 1 (ptone/scion#9306), only user requesters and reviewers who already
// have review access (access=existing) are admitted; agents and reviewers who
// cannot already review are refused.
func (s *Service) handleCreateReviewRequests(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	if !s.featureEnabled(ctx, FeatureReviewRequests) {
		writeNotFound(w)
		return
	}
	kind, ref, _, ok := s.host.Principal(ctx)
	if !ok {
		writeNotFound(w)
		return
	}
	b, a, ok := s.readableArtifact(w, r, id)
	if !ok {
		return
	}
	if kind != PrincipalKindUser {
		writeError(w, http.StatusForbidden, CodeAgentCannotRequestRev, "only users can request reviews")
		return
	}
	access, err := s.canWriteErr(ctx, b, a)
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: review request write check failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "could not verify artifact permissions")
		return
	}
	if !access.canPublish() {
		writeError(w, http.StatusForbidden, "forbidden", "only publishers of this artifact can request reviews")
		return
	}
	if !s.host.Permits(ctx, a.ScopeRef, PermissionRequestReview) {
		if se, ok := s.host.(ScopeExplainer); ok {
			if missing := se.MissingScope(ctx, PermissionRequestReview); missing != "" {
				writeErrorDetails(w, http.StatusForbidden, "missing_scope",
					"your credential does not include the "+missing+" scope",
					map[string]any{"scope": missing})
				return
			}
		}
		writeError(w, http.StatusForbidden, "forbidden", "your credential does not permit requesting reviews")
		return
	}
	if !s.chargePrincipal(w, LimitReviewRequest, kind, ref) {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxReviewRequestBodyBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "could not read the request body")
		return
	}
	if len(body) > maxReviewRequestBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "the request body is too large")
		return
	}
	var req CreateReviewRequestsBody
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		writeError(w, http.StatusBadRequest, "bad_request", `the body must be a JSON object such as {"reviewers": ["user:<id>"], "note": "..."}`)
		return
	}
	if len(req.Reviewers) == 0 || len(req.Reviewers) > maxReviewRequestReviewers {
		writeError(w, http.StatusBadRequest, "bad_request", "reviewers must contain between 1 and 10 user principals")
		return
	}
	if !utf8.ValidString(req.Note) || strings.ContainsRune(req.Note, 0) || utf8.RuneCountInString(req.Note) > maxReviewRequestNoteRunes {
		writeError(w, http.StatusBadRequest, "bad_request", "note must be valid text of at most 1000 characters")
		return
	}

	// Validate and deduplicate reviewer principals while preserving order.
	type reviewerTarget struct {
		principal string
		userID    string
	}
	seen := make(map[string]bool, len(req.Reviewers))
	targets := make([]reviewerTarget, 0, len(req.Reviewers))
	for _, raw := range req.Reviewers {
		rk, rid, ok := strings.Cut(strings.TrimSpace(raw), ":")
		if !ok || rk != PrincipalKindUser || !validSubjectID(rid) {
			writeError(w, http.StatusBadRequest, "bad_request", `each reviewer must be a user principal ("user:<id>")`)
			return
		}
		norm := PrincipalRef(PrincipalKindUser, rid)
		if seen[norm] {
			continue
		}
		seen[norm] = true
		targets = append(targets, reviewerTarget{principal: norm, userID: rid})
	}

	if a.CurrentSeq == 0 {
		writeError(w, http.StatusConflict, CodeNothingToReview, "a review request needs an artifact with a published version")
		return
	}

	auth, hasAuth := s.host.(RecipientAuthority)
	if !hasAuth {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "reviewer resolution is unavailable")
		return
	}
	notifier, hasNotifier := s.host.(ReviewRequestNotifier)

	// Load grants lazily once for the existing-access checks.
	var (
		grantsLoaded bool
		grants       []Grant
		grantsErr    error
	)
	loadGrants := func() ([]Grant, error) {
		if !grantsLoaded {
			grants, grantsErr = b.store.ListGrants(ctx, a.ID)
			grantsLoaded = true
		}
		return grants, grantsErr
	}

	now := s.now()
	actor := &Actor{Kind: kind, Ref: ref, Authority: AuthorityPublisher}
	nameRefs := make([]string, 0, len(targets)+1)
	nameRefs = append(nameRefs, PrincipalRef(kind, ref))
	for _, t := range targets {
		nameRefs = append(nameRefs, t.principal)
	}
	names := s.principalDisplayNames(ctx, nameRefs)

	results := make([]ReviewRequestResult, 0, len(targets))
	for _, t := range targets {
		if t.userID == ref {
			results = append(results, ReviewRequestResult{
				Reviewer: t.principal,
				Outcome:  ReviewRequestOutcomeRefused,
				Code:     CodeReviewerUnavailable,
			})
			continue
		}

		// Step 3: resolve active user via RecipientAuthority.
		subject, err := auth.ResolveRecipient(ctx, PrincipalKindUser, t.userID)
		if err != nil {
			slog.ErrorContext(ctx, "artifacts: resolve reviewer failed", "error", err)
			writeError(w, http.StatusServiceUnavailable, "unavailable", "could not resolve reviewer")
			return
		}
		if subject == nil {
			results = append(results, ReviewRequestResult{
				Reviewer: t.principal,
				Outcome:  ReviewRequestOutcomeRefused,
				Code:     CodeReviewerUnavailable,
			})
			continue
		}

		// Step 4: anti-spam visibility for user requesters.
		if hasNotifier {
			visible, err := notifier.MayNotifyReviewer(ctx, t.userID)
			if err != nil {
				slog.ErrorContext(ctx, "artifacts: reviewer visibility check failed", "error", err)
				writeError(w, http.StatusServiceUnavailable, "unavailable", "could not verify reviewer visibility")
				return
			}
			if !visible {
				results = append(results, ReviewRequestResult{
					Reviewer: t.principal,
					Outcome:  ReviewRequestOutcomeRefused,
					Code:     CodeReviewerUnavailable,
				})
				continue
			}
		}

		// Step 5a (P1): reviewer must already be able to review.
		canReview, err := principalCanReview(ctx, subject, a, PrincipalKindUser, t.userID, now, loadGrants)
		if err != nil {
			slog.ErrorContext(ctx, "artifacts: reviewer access check failed", "error", err)
			writeError(w, http.StatusServiceUnavailable, "unavailable", "could not verify reviewer access")
			return
		}
		if !canReview {
			results = append(results, ReviewRequestResult{
				Reviewer: t.principal,
				Outcome:  ReviewRequestOutcomeRefused,
				Code:     CodeReviewerCannotReview,
			})
			continue
		}

		row, outcome, err := b.store.CreateOrRemindReviewRequest(ctx, ReviewRequest{
			ArtifactID:    a.ID,
			ReviewerKind:  PrincipalKindUser,
			ReviewerRef:   t.userID,
			RequesterKind: kind,
			RequesterRef:  ref,
			Note:          req.Note,
			Access:        ReviewRequestAccessExisting,
		}, reviewRequestRemindCooldown, now, actor)
		if errors.Is(err, ErrNotFound) {
			writeNotFound(w)
			return
		}
		if errors.Is(err, ErrNothingToReview) {
			writeError(w, http.StatusConflict, CodeNothingToReview, "a review request needs an artifact with a published version")
			return
		}
		if err != nil {
			slog.ErrorContext(ctx, "artifacts: create review request failed", "error", err)
			writeError(w, http.StatusServiceUnavailable, "unavailable", "could not save review request")
			return
		}

		if outcome == ReviewRequestOutcomeCreated {
			s.notifyInboxChanged(ctx, t.userID)
		}

		var notified bool
		var notifySkipped string
		if outcome == ReviewRequestOutcomeCreated || outcome == ReviewRequestOutcomeReminded {
			if _, allowed := s.notifyLimiterFor().allow(PrincipalRef(kind, ref)); !allowed {
				notifySkipped = NotifySkippedRateLimited
			} else if !hasNotifier {
				notifySkipped = NotifySkippedUnavailable
			} else {
				nerr := notifier.NotifyReviewRequest(ctx, ReviewRequestNotice{
					ArtifactID:     a.ID,
					ArtifactTitle:  a.Title,
					HomeScope:      a.ScopeRef,
					RequestedSeq:   row.RequestedSeq,
					ReviewerUserID: t.userID,
					RequesterKind:  kind,
					RequesterRef:   ref,
					Note:           row.Note,
				})
				if nerr != nil {
					slog.WarnContext(ctx, "artifacts: review request notification failed", "artifact", a.ID, "error", nerr)
					notifySkipped = NotifySkippedUnavailable
				} else {
					notified = true
				}
			}
		}

		info := s.reviewRequestInfo(ctx, row, names)
		results = append(results, ReviewRequestResult{
			Reviewer:      t.principal,
			Outcome:       outcome,
			Access:        row.Access,
			Request:       &info,
			Notified:      notified,
			NotifySkipped: notifySkipped,
		})
	}

	writeJSON(w, http.StatusOK, CreateReviewRequestsResponse{Requests: results})
}

// handleDismissReviewRequest serves POST /api/v1/artifacts/{id}/review-request/dismiss.
// Works even after the caller has lost read access to the artifact, and returns
// only the request row (never artifact content).
func (s *Service) handleDismissReviewRequest(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	if !s.featureEnabled(ctx, FeatureReviewRequests) {
		writeNotFound(w)
		return
	}
	kind, ref, _, ok := s.host.Principal(ctx)
	if !ok {
		writeNotFound(w)
		return
	}
	if kind != PrincipalKindUser {
		writeNotFound(w)
		return
	}
	if !s.chargePrincipal(w, LimitReviewRequestDismiss, kind, ref) {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxReviewRequestBodyBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "could not read the request body")
		return
	}
	if len(body) > maxReviewRequestBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "the request body is too large")
		return
	}
	var req DismissReviewRequestBody
	if len(bytes.TrimSpace(body)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil || dec.More() {
			writeError(w, http.StatusBadRequest, "bad_request", `the body must be a JSON object such as {"note": "..."}`)
			return
		}
	}
	if !utf8.ValidString(req.Note) || strings.ContainsRune(req.Note, 0) || utf8.RuneCountInString(req.Note) > maxReviewRequestNoteRunes {
		writeError(w, http.StatusBadRequest, "bad_request", "note must be valid text of at most 1000 characters")
		return
	}

	b, ok := s.backend()
	if !ok {
		writeBackendUnavailable(w, b, "artifact storage is not configured")
		return
	}

	actor := &Actor{Kind: kind, Ref: ref, Authority: AuthorityReviewer}
	row, err := b.store.DismissReviewRequest(ctx, id, kind, ref, req.Note, s.now(), actor)
	if errors.Is(err, ErrNotFound) {
		writeNotFound(w)
		return
	}
	if errors.Is(err, ErrRequestClosed) {
		writeError(w, http.StatusConflict, CodeRequestClosed, "the review request is no longer open")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: dismiss review request failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "could not dismiss the review request")
		return
	}

	s.notifyInboxChanged(ctx, ref)

	authorNotified := false
	if notifier, ok := s.host.(ReviewRequestNotifier); ok {
		var title, homeScope, ownerKind, ownerRef string
		if a, aerr := b.store.GetArtifact(ctx, id); aerr == nil && a != nil {
			title = a.Title
			homeScope = a.ScopeRef
			ownerKind = a.OwnerKind
			ownerRef = a.OwnerRef
		}
		if nerr := notifier.NotifyRequestDismissed(ctx, ReviewRequestDismissNotice{
			ArtifactID:    id,
			ArtifactTitle: title,
			HomeScope:     homeScope,
			OwnerKind:     ownerKind,
			OwnerRef:      ownerRef,
			RequesterKind: row.RequesterKind,
			RequesterRef:  row.RequesterRef,
			ReviewerKind:  kind,
			ReviewerRef:   ref,
			ResponseNote:  row.ResponseNote,
		}); nerr != nil {
			slog.WarnContext(ctx, "artifacts: dismiss notification failed", "artifact", id, "error", nerr)
		} else {
			authorNotified = true
		}
	}

	info := s.reviewRequestInfo(ctx, row, nil)
	writeJSON(w, http.StatusOK, DismissReviewRequestResponse{
		Request:        info,
		AuthorNotified: authorNotified,
	})
}
