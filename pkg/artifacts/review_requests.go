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

	"github.com/google/uuid"
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
	maxClosedReviewRequests   = 50

	// MaxBoundedGrantsPerArtifact caps the number of bounded review grants
	// (CreatedByRef == BoundedReviewCreator) on one artifact (design §4.3,
	// ptone/scion#5548).
	MaxBoundedGrantsPerArtifact = 10

	reviewRequestRemindCooldown = 24 * time.Hour
)

// BoundedReviewCreator is the CreatedByRef of a bounded review grant written
// by an agent review request (design §4.3, ptone/scion#5548). Like
// SponsorReviewCreator, a grant carrying this marker counts only while its
// subject holds a role that reads in the artifact's home project, so it never
// confers read on its own.
const BoundedReviewCreator = ActorKindSystem + ":review_request"

// Error and per-reviewer refusal codes for review requests (design §4.3, §4.4).
const (
	CodeReviewerUnavailable        = "reviewer_unavailable"
	CodeReviewerCannotReview       = "reviewer_cannot_review"
	CodeRequestClosed              = "request_closed"
	CodeInteractiveSessionRequired = "interactive_session_required"
	CodeUseReviewRequests          = "use_review_requests"
	CodeUnavailable                = "unavailable"
	CodeTooManyGrants              = "too_many_grants"
)

// reviewerClass is the non-access-granting classification returned by
// classifyReviewer for one candidate reviewer in POST /review-requests.
type reviewerClass int

const (
	reviewerClassUnavailable reviewerClass = iota
	reviewerClassCanReview
	reviewerClassNeedsRaise
	reviewerClassNeedsBounded
	reviewerClassCannotReview
)

// classifyReviewer runs steps 3 (ResolveRecipient), 4 (MayNotifyReviewer for
// user requesters), 5a (principalCanReview), and 5c (principalReadsHomeByRole
// for agent requesters) for one candidate user reviewer and returns only a
// classification. The resolved RecipientSubject never leaves this function and
// is used only for a nil check, principalCanReview, and
// principalReadsHomeByRole.
func classifyReviewer(
	ctx context.Context,
	auth RecipientAuthority,
	notifier ReviewRequestNotifier,
	a *Artifact,
	requesterKind string,
	reviewerUserID string,
	now time.Time,
	loadGrants func() ([]Grant, error),
) (reviewerClass, error) {
	subject, err := auth.ResolveRecipient(ctx, PrincipalKindUser, reviewerUserID)
	if err != nil {
		return reviewerClassUnavailable, err
	}
	if subject == nil {
		return reviewerClassUnavailable, nil
	}
	if requesterKind == PrincipalKindUser {
		visible, err := notifier.MayNotifyReviewer(ctx, reviewerUserID)
		if err != nil {
			return reviewerClassUnavailable, err
		}
		if !visible {
			return reviewerClassUnavailable, nil
		}
	}
	canReview, err := principalCanReview(ctx, subject, a, PrincipalKindUser, reviewerUserID, now, loadGrants)
	if err != nil {
		return reviewerClassUnavailable, err
	}
	if canReview {
		return reviewerClassCanReview, nil
	}
	if requesterKind == PrincipalKindAgent {
		readsHome, err := principalReadsHomeByRole(ctx, subject, a)
		if err != nil {
			return reviewerClassUnavailable, err
		}
		if readsHome {
			return reviewerClassNeedsBounded, nil
		}
		return reviewerClassCannotReview, nil
	}
	return reviewerClassNeedsRaise, nil
}

// CreateReviewRequestsBody is the JSON body of POST /api/v1/artifacts/{id}/review-requests.
type CreateReviewRequestsBody struct {
	Reviewers []string `json:"reviewers"`
	Delegator bool     `json:"delegator,omitempty"`
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

// ListReviewRequestsResponse is the JSON response of GET /api/v1/artifacts/{id}/review-requests.
type ListReviewRequestsResponse struct {
	Requests []ReviewRequestInfo `json:"requests"`
}

// WithdrawReviewRequestResponse is the JSON response of POST /api/v1/artifacts/{id}/review-requests/{rid}/withdraw.
type WithdrawReviewRequestResponse struct {
	Request ReviewRequestInfo `json:"request"`
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

// populateReviewState sets resp.ReviewState on GET artifact / version responses.
// ReviewOnLatest is always populated; RequestedOfMe is populated only while
// FeatureReviewRequests is enabled; MyDraft and DraftsInProgress are populated
// only while FeatureReviewDrafts is enabled.
func (s *Service) populateReviewState(ctx context.Context, b backend, a *Artifact, currentIsReview bool, resp *ArtifactResponse) {
	requestsEnabled := s.featureEnabled(ctx, FeatureReviewRequests)
	draftsEnabled := s.featureEnabled(ctx, FeatureReviewDrafts)
	state := &ReviewStateInfo{
		ReviewOnLatest: currentIsReview,
	}
	kind, ref, _, ok := s.host.Principal(ctx)
	if requestsEnabled && ok && kind == PrincipalKindUser {
		rr, err := b.store.GetOpenReviewRequest(ctx, a.ID, kind, ref)
		if err == nil && rr != nil {
			info := s.reviewRequestInfo(ctx, *rr, nil)
			state.RequestedOfMe = &info
		} else if err != nil && !errors.Is(err, ErrNotFound) {
			slog.WarnContext(ctx, "artifacts: get open review request failed", "artifact", a.ID, "error", err)
		}
	}
	if draftsEnabled {
		if ok {
			summary, err := b.store.GetReviewDraftSummary(ctx, a.ID, kind, ref)
			if err == nil && summary != nil {
				state.MyDraft = summary
			} else if err != nil && !errors.Is(err, ErrNotFound) {
				slog.WarnContext(ctx, "artifacts: get review draft summary failed", "artifact", a.ID, "error", err)
			}
		}
		if resp.CanPublish {
			n, err := b.store.CountOpenRequestReviewDrafts(ctx, a.ID)
			if err == nil {
				state.DraftsInProgress = n
			} else {
				slog.WarnContext(ctx, "artifacts: count open-request review drafts failed", "artifact", a.ID, "error", err)
			}
		}
	}
	resp.ReviewState = state
}

// handleCreateReviewRequests serves POST /api/v1/artifacts/{id}/review-requests.
// User requesters must hold an interactive session and publish access; when
// they may also administer the artifact, reviewers who cannot yet review are
// raised to a principal review grant in the same transaction (access=raised).
// Agent requesters must own the artifact in their own home project and hold
// artifact.request_review; reviewers who read the artifact via a home-project
// role receive a role-gated bounded review grant in the same transaction
// (access=bounded, at most MaxBoundedGrantsPerArtifact per artifact).
func (s *Service) handleCreateReviewRequests(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	if !s.featureEnabled(ctx, FeatureReviewRequests) {
		writeNotFound(w)
		return
	}
	kind, ref, homeScope, ok := s.host.Principal(ctx)
	if !ok {
		writeNotFound(w)
		return
	}
	b, a, ok := s.readableArtifact(w, r, id)
	if !ok {
		return
	}
	if kind != PrincipalKindUser && kind != PrincipalKindAgent {
		writeError(w, http.StatusForbidden, "forbidden", "your principal cannot request reviews")
		return
	}
	if kind == PrincipalKindAgent {
		if a.OwnerKind != PrincipalKindAgent || a.OwnerRef != ref {
			writeError(w, http.StatusForbidden, "forbidden", "agents can only request reviews on artifacts they own")
			return
		}
		if homeScope == "" || a.ScopeKind != ScopeKindProject || a.ScopeRef != homeScope {
			writeError(w, http.StatusForbidden, "forbidden", "agents cannot request reviews on artifacts outside their home project")
			return
		}
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
	auth, hasAuth := s.host.(RecipientAuthority)
	if !hasAuth {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "reviewer resolution is unavailable")
		return
	}
	notifier, hasNotifier := s.host.(ReviewRequestNotifier)
	if !hasNotifier {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "reviewer visibility check is unavailable")
		return
	}
	// Route-level refusal for non-interactive user credentials before charging
	// the limiter or entering the per-reviewer loop. Agent requesters use the
	// owning-agent + home-role bounded path instead (design §4.3 step 5c).
	if kind == PrincipalKindUser && !notifier.IsInteractiveSession(ctx) {
		writeError(w, http.StatusForbidden, CodeInteractiveSessionRequired, "requesting reviews requires an interactive user session")
		return
	}
	limitFamily := LimitReviewRequest
	if kind == PrincipalKindAgent {
		limitFamily = LimitReviewRequestAgent
	}
	if !s.chargePrincipal(w, limitFamily, kind, ref) {
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
	if req.Delegator && kind != PrincipalKindAgent {
		writeError(w, http.StatusBadRequest, "bad_request", "delegator can only be used by an agent")
		return
	}
	if (!req.Delegator && len(req.Reviewers) == 0) || len(req.Reviewers) > maxReviewRequestReviewers {
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
	seen := make(map[string]bool, len(req.Reviewers)+1)
	targets := make([]reviewerTarget, 0, len(req.Reviewers)+1)
	var delegatorMissing bool
	if req.Delegator {
		rec, ok := s.host.(SponsorRecorder)
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "delegating user resolution is unavailable")
			return
		}
		delegatorID, derr := rec.DelegatingUser(ctx, ref)
		if derr != nil {
			slog.ErrorContext(ctx, "artifacts: delegating user resolution failed", "error", derr)
			writeError(w, http.StatusServiceUnavailable, "unavailable", "could not resolve delegating user")
			return
		}
		if delegatorID == "" {
			delegatorMissing = true
		} else {
			if u, err := uuid.Parse(delegatorID); err == nil {
				delegatorID = u.String()
			}
			norm := PrincipalRef(PrincipalKindUser, delegatorID)
			seen[norm] = true
			targets = append(targets, reviewerTarget{principal: norm, userID: delegatorID})
		}
	}
	for _, raw := range req.Reviewers {
		rk, rid, ok := strings.Cut(strings.TrimSpace(raw), ":")
		if !ok || rk != PrincipalKindUser || !validSubjectID(rid) {
			writeError(w, http.StatusBadRequest, "bad_request", `each reviewer must be a user principal ("user:<id>")`)
			return
		}
		if u, err := uuid.Parse(rid); err == nil {
			rid = u.String()
		}
		norm := PrincipalRef(PrincipalKindUser, rid)
		if seen[norm] {
			continue
		}
		seen[norm] = true
		targets = append(targets, reviewerTarget{principal: norm, userID: rid})
	}
	if len(targets) > maxReviewRequestReviewers {
		writeError(w, http.StatusBadRequest, "bad_request", "reviewers must contain between 1 and 10 user principals")
		return
	}

	if a.CurrentSeq == 0 {
		writeError(w, http.StatusConflict, CodeNothingToReview, "a review request needs an artifact with a published version")
		return
	}

	// Load grants lazily once for existing-access and admin-authority checks.
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

	var (
		adminChecked   bool
		adminAuthority string
		adminErr       error
	)
	checkAdmin := func() (string, error) {
		if !adminChecked {
			adminAuthority, adminErr = s.adminAuthorityWith(ctx, a, loadGrants)
			adminChecked = true
		}
		return adminAuthority, adminErr
	}

	now := s.now()
	actor := &Actor{Kind: kind, Ref: ref, Authority: AuthorityPublisher}
	if kind == PrincipalKindAgent {
		actor.Expect = Expect{ScopeRef: a.ScopeRef, OwnerKind: a.OwnerKind, OwnerRef: a.OwnerRef}
	}
	nameRefs := make([]string, 0, len(targets)+1)
	nameRefs = append(nameRefs, PrincipalRef(kind, ref))
	for _, t := range targets {
		nameRefs = append(nameRefs, t.principal)
	}
	names := s.principalDisplayNames(ctx, nameRefs)

	results := make([]ReviewRequestResult, 0, len(targets)+1)
	if delegatorMissing {
		results = append(results, ReviewRequestResult{
			Reviewer: "delegator",
			Outcome:  ReviewRequestOutcomeRefused,
			Code:     CodeReviewerUnavailable,
		})
	}
	for _, t := range targets {
		if kind == PrincipalKindUser && t.userID == ref {
			results = append(results, ReviewRequestResult{
				Reviewer: t.principal,
				Outcome:  ReviewRequestOutcomeRefused,
				Code:     CodeReviewerUnavailable,
			})
			continue
		}

		// Steps 3, 4, 5a, and 5c via classifyReviewer (non-access-granting).
		class, err := classifyReviewer(ctx, auth, notifier, a, kind, t.userID, now, loadGrants)
		if err != nil {
			slog.ErrorContext(ctx, "artifacts: classify reviewer failed", "error", err)
			results = append(results, ReviewRequestResult{
				Reviewer: t.principal,
				Outcome:  ReviewRequestOutcomeRefused,
				Code:     CodeUnavailable,
			})
			continue
		}
		if class == reviewerClassUnavailable {
			results = append(results, ReviewRequestResult{
				Reviewer: t.principal,
				Outcome:  ReviewRequestOutcomeRefused,
				Code:     CodeReviewerUnavailable,
			})
			continue
		}
		if class == reviewerClassCannotReview {
			results = append(results, ReviewRequestResult{
				Reviewer: t.principal,
				Outcome:  ReviewRequestOutcomeRefused,
				Code:     CodeReviewerCannotReview,
			})
			continue
		}

		reqAccess := ReviewRequestAccessExisting
		var grantOpt *ReviewRequestGrantOption
		switch class {
		case reviewerClassNeedsRaise:
			// Step 5b: user requester who may administer raises the reviewer's
			// principal grant to review in the same transaction.
			admAuth, aerr := checkAdmin()
			if aerr != nil {
				slog.ErrorContext(ctx, "artifacts: reviewer admin check failed", "error", aerr)
				results = append(results, ReviewRequestResult{
					Reviewer: t.principal,
					Outcome:  ReviewRequestOutcomeRefused,
					Code:     CodeUnavailable,
				})
				continue
			}
			if admAuth == "" {
				results = append(results, ReviewRequestResult{
					Reviewer: t.principal,
					Outcome:  ReviewRequestOutcomeRefused,
					Code:     CodeReviewerCannotReview,
				})
				continue
			}
			reqAccess = ReviewRequestAccessRaised
			grantOpt = &ReviewRequestGrantOption{
				Grant: &Grant{
					ID:           uuid.NewString(),
					ArtifactID:   a.ID,
					SubjectKind:  SubjectPrincipal,
					SubjectRef:   t.principal,
					Permission:   GrantReview,
					CreatedByRef: PrincipalRef(kind, ref),
					CreatedAt:    now,
				},
				MaxGrants: MaxGrantsPerArtifact,
				Actor:     s.callerActor(ctx, a, admAuth),
			}
		case reviewerClassNeedsBounded:
			// Step 5c: owning agent writes a role-gated bounded review grant
			// in the same transaction.
			if kind != PrincipalKindAgent || a.OwnerKind != PrincipalKindAgent || a.OwnerRef != ref || a.ScopeRef != homeScope {
				results = append(results, ReviewRequestResult{
					Reviewer: t.principal,
					Outcome:  ReviewRequestOutcomeRefused,
					Code:     CodeReviewerCannotReview,
				})
				continue
			}
			reqAccess = ReviewRequestAccessBounded
			grantOpt = &ReviewRequestGrantOption{
				Grant: &Grant{
					ID:           uuid.NewString(),
					ArtifactID:   a.ID,
					SubjectKind:  SubjectPrincipal,
					SubjectRef:   t.principal,
					Permission:   GrantReview,
					CreatedByRef: BoundedReviewCreator,
					CreatedAt:    now,
				},
				MaxGrants:        MaxGrantsPerArtifact,
				Bounded:          true,
				MaxBoundedGrants: MaxBoundedGrantsPerArtifact,
				Actor:            actor,
			}
		}

		row, outcome, err := b.store.CreateOrRemindReviewRequest(ctx, ReviewRequest{
			ArtifactID:    a.ID,
			ReviewerKind:  PrincipalKindUser,
			ReviewerRef:   t.userID,
			RequesterKind: kind,
			RequesterRef:  ref,
			Note:          req.Note,
			Access:        reqAccess,
		}, reviewRequestRemindCooldown, now, actor, grantOpt)
		if errors.Is(err, ErrStewardPrecondition) && kind == PrincipalKindAgent {
			if len(results) == 0 {
				writeError(w, http.StatusForbidden, "forbidden", "the artifact's home project or owner changed")
				return
			}
			results = append(results, ReviewRequestResult{
				Reviewer: t.principal,
				Outcome:  ReviewRequestOutcomeRefused,
				Code:     CodeReviewerCannotReview,
			})
			continue
		}
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrStewardPrecondition) {
			if len(results) == 0 {
				writeNotFound(w)
				return
			}
			results = append(results, ReviewRequestResult{
				Reviewer: t.principal,
				Outcome:  ReviewRequestOutcomeRefused,
				Code:     CodeUnavailable,
			})
			continue
		}
		if errors.Is(err, ErrBoundedGrantConflict) {
			results = append(results, ReviewRequestResult{
				Reviewer: t.principal,
				Outcome:  ReviewRequestOutcomeRefused,
				Code:     CodeReviewerCannotReview,
			})
			continue
		}
		if errors.Is(err, ErrNothingToReview) {
			if len(results) == 0 {
				writeError(w, http.StatusConflict, CodeNothingToReview, "a review request needs an artifact with a published version")
				return
			}
			results = append(results, ReviewRequestResult{
				Reviewer: t.principal,
				Outcome:  ReviewRequestOutcomeRefused,
				Code:     CodeNothingToReview,
			})
			continue
		}
		if errors.Is(err, ErrTooManyGrants) {
			results = append(results, ReviewRequestResult{
				Reviewer: t.principal,
				Outcome:  ReviewRequestOutcomeRefused,
				Code:     CodeTooManyGrants,
			})
			continue
		}
		if err != nil {
			slog.ErrorContext(ctx, "artifacts: create review request failed", "error", err)
			results = append(results, ReviewRequestResult{
				Reviewer: t.principal,
				Outcome:  ReviewRequestOutcomeRefused,
				Code:     CodeUnavailable,
			})
			continue
		}
		if grantOpt != nil {
			// Invalidate cached grants so subsequent reviewers in the same batch
			// see the updated grant list, and emit the grant decision log.
			grantsLoaded = false
			s.logGrantPut(r, a, grantOpt.Grant, grantOpt.Put, grantOpt.Actor.Authority)
		}

		if outcome == ReviewRequestOutcomeCreated {
			s.notifyInboxChanged(ctx, t.userID)
		}

		var notified bool
		var notifySkipped string
		if outcome == ReviewRequestOutcomeCreated || outcome == ReviewRequestOutcomeReminded {
			if _, allowed := s.notifyLimiterFor().allow(PrincipalRef(kind, ref)); !allowed {
				notifySkipped = NotifySkippedRateLimited
			} else {
				noticeNote := row.Note
				if outcome == ReviewRequestOutcomeReminded && (row.RequesterKind != kind || row.RequesterRef != ref) {
					noticeNote = req.Note
				}
				nerr := notifier.NotifyReviewRequest(ctx, ReviewRequestNotice{
					ArtifactID:     a.ID,
					ArtifactTitle:  a.Title,
					HomeScope:      a.ScopeRef,
					RequestedSeq:   row.RequestedSeq,
					ReviewerUserID: t.userID,
					RequesterKind:  kind,
					RequesterRef:   ref,
					Note:           noticeNote,
				})
				if nerr != nil {
					slog.WarnContext(ctx, "artifacts: review request notification failed", "artifact", a.ID, "error", nerr)
					notifySkipped = NotifySkippedUnavailable
				} else {
					notified = true
				}
			}
		}

		if row.RequesterRef != "" {
			reqPrincipal := PrincipalRef(row.RequesterKind, row.RequesterRef)
			if _, known := names[reqPrincipal]; !known {
				for k, v := range s.principalDisplayNames(ctx, []string{reqPrincipal}) {
					if names == nil {
						names = make(map[string]string, 1)
					}
					names[k] = v
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

// handleListReviewRequests serves GET /api/v1/artifacts/{id}/review-requests.
// Callers who can publish receive every open request and up to 50 most recent
// closed ones, with reviewerCanReview and inDraft (open requests only, when
// FeatureReviewDrafts is enabled). Other readable callers receive only
// requests addressed to themselves, without reviewerCanReview or inDraft.
func (s *Service) handleListReviewRequests(w http.ResponseWriter, r *http.Request, id string) {
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
	access, err := s.canWriteErr(ctx, b, a)
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: list review requests write check failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "could not verify artifact permissions")
		return
	}

	isPublisher := access.canPublish()
	var rows []ReviewRequest
	if isPublisher {
		rows, err = b.store.ListReviewRequests(ctx, a.ID, maxClosedReviewRequests)
	} else {
		rows, err = b.store.ListReviewRequestsForReviewer(ctx, a.ID, kind, ref, maxClosedReviewRequests)
	}
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: list review requests failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not list review requests")
		return
	}

	// Collect principal refs for one batch display-name lookup.
	nameSeen := make(map[string]bool, len(rows)*2)
	nameRefs := make([]string, 0, len(rows)*2)
	hasOpen := false
	for _, rr := range rows {
		if rr.State == ReviewRequestStateOpen {
			hasOpen = true
		}
		for _, pr := range []string{PrincipalRef(rr.ReviewerKind, rr.ReviewerRef), PrincipalRef(rr.RequesterKind, rr.RequesterRef)} {
			if pr != "" && !nameSeen[pr] {
				nameSeen[pr] = true
				nameRefs = append(nameRefs, pr)
			}
		}
	}
	names := s.principalDisplayNames(ctx, nameRefs)

	// Publisher view: check inDraft (open requests only, when drafts gate is on)
	// and reviewerCanReview for open requests.
	var draftHolders map[string]bool
	if isPublisher && hasOpen && s.featureEnabled(ctx, FeatureReviewDrafts) {
		dkeys, derr := b.store.ListReviewDraftPrincipals(ctx, a.ID)
		if derr != nil {
			slog.WarnContext(ctx, "artifacts: list draft principals for review requests failed", "artifact", a.ID, "error", derr)
		} else if len(dkeys) > 0 {
			draftHolders = make(map[string]bool, len(dkeys))
			for _, dk := range dkeys {
				draftHolders[PrincipalRef(dk.PrincipalKind, dk.PrincipalRef)] = true
			}
		}
	}

	var (
		auth, _      = s.host.(RecipientAuthority)
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

	infos := make([]ReviewRequestInfo, 0, len(rows))
	for _, rr := range rows {
		info := s.reviewRequestInfo(ctx, rr, names)
		if isPublisher && rr.State == ReviewRequestStateOpen {
			if auth != nil {
				subject, rerr := auth.ResolveRecipient(ctx, rr.ReviewerKind, rr.ReviewerRef)
				if rerr != nil {
					slog.WarnContext(ctx, "artifacts: resolve reviewer for list failed", "artifact", a.ID, "error", rerr)
				} else {
					canRev, cerr := principalCanReview(ctx, subject, a, rr.ReviewerKind, rr.ReviewerRef, now, loadGrants)
					if cerr != nil {
						slog.WarnContext(ctx, "artifacts: reviewer access check for list failed", "artifact", a.ID, "error", cerr)
					} else {
						info.ReviewerCanReview = &canRev
					}
				}
			}
			if draftHolders[PrincipalRef(rr.ReviewerKind, rr.ReviewerRef)] {
				info.InDraft = true
			}
		}
		infos = append(infos, info)
	}

	writeJSON(w, http.StatusOK, ListReviewRequestsResponse{Requests: infos})
}

// handleWithdrawReviewRequest serves POST /api/v1/artifacts/{id}/review-requests/{rid}/withdraw.
// Allowed for the request's requester, the artifact owner, and anyone who may
// administer the artifact. An agent may withdraw only requests it made.
func (s *Service) handleWithdrawReviewRequest(w http.ResponseWriter, r *http.Request, id, rid string) {
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
	rr, err := b.store.GetReviewRequest(ctx, a.ID, rid)
	if errors.Is(err, ErrNotFound) {
		writeNotFound(w)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: get review request for withdraw failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "could not read the review request")
		return
	}

	actor, allowed, err := s.withdrawAuthority(ctx, b, a, rr, kind, ref)
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: withdraw authority check failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "could not verify artifact permissions")
		return
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "forbidden", "you are not allowed to withdraw this review request")
		return
	}

	row, err := b.store.WithdrawReviewRequest(ctx, a.ID, rid, s.now(), actor)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrStewardPrecondition) {
		writeNotFound(w)
		return
	}
	if errors.Is(err, ErrRequestClosed) {
		writeError(w, http.StatusConflict, CodeRequestClosed, "the review request is no longer open")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: withdraw review request failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "could not withdraw the review request")
		return
	}

	if row.ReviewerKind == PrincipalKindUser && row.ReviewerRef != "" {
		if notifier, ok := s.host.(ReviewRequestNotifier); ok {
			notifier.RequestClosed(ctx, a.ID, row.ReviewerRef)
		}
		s.notifyInboxChanged(ctx, row.ReviewerRef)
	}

	info := s.reviewRequestInfo(ctx, row, nil)
	writeJSON(w, http.StatusOK, WithdrawReviewRequestResponse{Request: info})
}

// withdrawAuthority decides whether principal (kind, ref) may withdraw rr on
// artifact a (design §4.4), enforcing the caller's credential ceiling:
//   - An agent (Phase 4) may withdraw only requests it made, and only when its
//     credential permits artifact.request_review in a's home scope.
//   - A user requester may withdraw their own request when their credential
//     permits artifact.request_review in a's home scope.
//   - Otherwise, a user (including the artifact owner) must hold admin or
//     steward authority via adminAuthorityWith (which checks artifact.manage).
func (s *Service) withdrawAuthority(ctx context.Context, b backend, a *Artifact, rr *ReviewRequest, kind, ref string) (*Actor, bool, error) {
	if kind == PrincipalKindAgent {
		// Phase 4 agent branch: agents may withdraw only requests they made,
		// subject to the same artifact.request_review credential check.
		if rr.RequesterKind == PrincipalKindAgent && rr.RequesterRef == ref &&
			s.host.Permits(ctx, a.ScopeRef, PermissionRequestReview) {
			return &Actor{Kind: kind, Ref: ref, Authority: AuthorityPublisher}, true, nil
		}
		return nil, false, nil
	}
	if rr.RequesterKind == kind && rr.RequesterRef == ref &&
		s.host.Permits(ctx, a.ScopeRef, PermissionRequestReview) {
		return &Actor{Kind: kind, Ref: ref, Authority: AuthorityPublisher}, true, nil
	}
	admAuth, err := s.adminAuthorityWith(ctx, a, func() ([]Grant, error) {
		return b.store.ListGrants(ctx, a.ID)
	})
	if err != nil {
		return nil, false, err
	}
	if admAuth != "" {
		return s.callerActor(ctx, a, admAuth), true, nil
	}
	return nil, false, nil
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

	// Enforce the caller's credential ceiling and project boundary (Permits is
	// credential-only and always true for session users, so reviewers who lost
	// read through role or grant removal can still dismiss).
	a, err := b.store.GetArtifact(ctx, id)
	if errors.Is(err, ErrNotFound) || a == nil {
		writeNotFound(w)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: load artifact for dismiss failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "could not verify artifact scope")
		return
	}
	if !s.host.Permits(ctx, a.ScopeRef, PermissionRead) {
		writeNotFound(w)
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

	authorNotified := false
	if notifier, ok := s.host.(ReviewRequestNotifier); ok {
		if nerr := notifier.NotifyRequestDismissed(ctx, ReviewRequestDismissNotice{
			ArtifactID:    id,
			ArtifactTitle: a.Title,
			HomeScope:     a.ScopeRef,
			OwnerKind:     a.OwnerKind,
			OwnerRef:      a.OwnerRef,
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

	s.notifyInboxChanged(ctx, ref)

	info := s.reviewRequestInfo(ctx, row, nil)
	writeJSON(w, http.StatusOK, DismissReviewRequestResponse{
		Request:        info,
		AuthorNotified: authorNotified,
	})
}
