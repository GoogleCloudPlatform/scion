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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Open reviews (ptone/scion#6849). A tab in Review mode sends a heartbeat
// (PUT review-sessions/{sid}) once a minute; the hub keeps one row per tab.
// A publish that would overtake an open review of the current version is
// refused with 409 review_open, naming the reviewers, unless it is forced.
// Presence is best effort: a suspended tab stops beating and stops
// counting, and the reviewer's text is protected on the reviewer's side.

// FeatureOpenReviewCheck is the experiment that gates the open-review
// check and the review-session routes. It equals the hub's
// experiments.ArtifactsOpenReviewCheck.
const FeatureOpenReviewCheck = "hub.artifacts_open_review_check"

const (
	// ReviewLiveWindow is how recently a session must have beaten to
	// count: the beat is every minute, and this tolerates the throttling
	// of background tabs.
	ReviewLiveWindow = 5 * time.Minute
	// ReviewIdleWindow is how recently a clean session (one without
	// unsaved changes) must have started or been edited to count.
	ReviewIdleWindow = 30 * time.Minute
	// ReviewSessionRetention is how long after its last beat the reaper
	// waits before it deletes a session row. The reaper runs periodically
	// and in batches, so a row can outlive it; the caller's DELETE or the
	// artifact's deletion removes the row sooner.
	ReviewSessionRetention = time.Hour
	// MaxReviewSessionsPerPrincipal is the number of live sessions at which
	// a principal can open no new session on one artifact. It is checked
	// only when a beat creates a session row, so a row that beats again
	// after leaving the live window is not counted again, and the live
	// count can then exceed it.
	MaxReviewSessionsPerPrincipal = 8
	// maxListedReviewSessions bounds the rows one check reads.
	maxListedReviewSessions = 200
	// maxReviewOpenListed caps the reviewers a refusal names.
	maxReviewOpenListed = 5
	// maxReviewBeatBytes caps the body of a heartbeat.
	maxReviewBeatBytes = 1 << 10
)

// Error codes of the open-review check and the review-session routes.
const (
	// CodeReviewOpen refuses a publish that would overtake an open review.
	// Its details carry "reviews" (ReviewOpenEntry), "truncated" and
	// "base".
	CodeReviewOpen = "review_open"
	// CodeTooManyReviewSessions refuses a heartbeat that would open a
	// session past MaxReviewSessionsPerPrincipal.
	CodeTooManyReviewSessions = "too_many_review_sessions"
)

// unnamedReviewer stands for a reviewer whose display name the host could
// not resolve. It is never replaced by an id.
const unnamedReviewer = "a reviewer"

// reviewSessionsSegment is the path segment of the review-session routes.
const reviewSessionsSegment = "review-sessions"

// sessionIDPattern is the form of a client-chosen session id: 128 random
// bits or more, base64url.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{22,64}$`)

// FeatureGate is an optional extension of Host. The service asks it
// whether a server-side experiment is on, so code in this package can be
// gated without knowing how the host stores experiments. A host without it
// has every gated feature off (fail closed).
type FeatureGate interface {
	// FeatureEnabled reports whether the experiment name is on now. It
	// answers false for a name the host does not know. Callers pass only
	// the Feature constants of this package, never a name from a request.
	FeatureEnabled(ctx context.Context, name string) bool
}

// PrincipalNamer is an optional extension of Host. The open-review check
// asks it for the display names of reviewers. A host without it names
// every reviewer "a reviewer".
type PrincipalNamer interface {
	// PrincipalDisplayNames returns the display name of each "kind:id"
	// in refs (PrincipalRef): a user's display name or an agent's name.
	// A ref it cannot resolve is left out of the answer. It must never
	// answer an id, an email address or a credential.
	PrincipalDisplayNames(ctx context.Context, refs []string) map[string]string
}

// featureEnabled asks the host whether name is on, and answers false when
// the host has no FeatureGate.
func (s *Service) featureEnabled(ctx context.Context, name string) bool {
	g, ok := s.host.(FeatureGate)
	return ok && g.FeatureEnabled(ctx, name)
}

// ReviewBeatRequest is the body of PUT
// /api/v1/artifacts/{id}/review-sessions/{sid}.
type ReviewBeatRequest struct {
	// Base is the version under review.
	Base int `json:"base"`
	// Edited is true when the review text changed since the last beat.
	Edited bool `json:"edited"`
	// Dirty is true when the review text differs from the text the
	// review started from.
	Dirty bool `json:"dirty"`
}

// ReviewBeatResponse answers a heartbeat with the artifact's current
// version, so the reviewer learns when a newer one is published. It
// carries nothing about other reviewers.
type ReviewBeatResponse struct {
	CurrentSeq int `json:"currentSeq"`
}

// ReviewOpenEntry is one open review in a review_open refusal: a display
// name, the last activity to the minute and whether there are unsaved
// changes. It never carries an id.
type ReviewOpenEntry struct {
	Reviewer       string    `json:"reviewer"`
	LastActivityAt time.Time `json:"lastActivityAt"`
	UnsavedChanges bool      `json:"unsavedChanges"`
}

// openReview is one principal's open review: its sessions collapsed.
type openReview struct {
	ref          string // PrincipalRef, never sent to a client
	lastActivity time.Time
	dirty        bool
}

// openReviews applies the active-window rule to sessions as of now and
// collapses them per principal, newest activity first. A session counts
// when it beat within ReviewLiveWindow; it has unsaved changes or started
// or was edited within ReviewIdleWindow; it is on currentSeq; and it is not
// the publisher's (publisherKind:publisherRef).
func openReviews(sessions []ReviewSession, currentSeq int, publisherKind, publisherRef string, now time.Time) []openReview {
	byRef := map[string]*openReview{}
	for _, rs := range sessions {
		if rs.LastSeenAt.Before(now.Add(-ReviewLiveWindow)) {
			continue
		}
		activity := rs.StartedAt
		if rs.LastEditAt != nil && rs.LastEditAt.After(activity) {
			activity = *rs.LastEditAt
		}
		if !rs.Dirty && activity.Before(now.Add(-ReviewIdleWindow)) {
			continue
		}
		if rs.BaseSeq != currentSeq {
			continue
		}
		if rs.PrincipalKind == publisherKind && rs.PrincipalRef == publisherRef {
			continue
		}
		ref := PrincipalRef(rs.PrincipalKind, rs.PrincipalRef)
		o := byRef[ref]
		if o == nil {
			o = &openReview{ref: ref}
			byRef[ref] = o
		}
		if activity.After(o.lastActivity) {
			o.lastActivity = activity
		}
		o.dirty = o.dirty || rs.Dirty
	}
	out := make([]openReview, 0, len(byRef))
	for _, o := range byRef {
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].lastActivity.Equal(out[j].lastActivity) {
			return out[i].lastActivity.After(out[j].lastActivity)
		}
		return out[i].ref < out[j].ref
	})
	return out
}

// reviewOpenEntries names the first maxReviewOpenListed reviews, and
// reports whether there were more.
func (s *Service) reviewOpenEntries(ctx context.Context, reviews []openReview) ([]ReviewOpenEntry, bool) {
	listed := reviews
	if len(listed) > maxReviewOpenListed {
		listed = listed[:maxReviewOpenListed]
	}
	var names map[string]string
	if n, ok := s.host.(PrincipalNamer); ok && len(listed) > 0 {
		refs := make([]string, len(listed))
		for i, o := range listed {
			refs[i] = o.ref
		}
		names = n.PrincipalDisplayNames(ctx, refs)
	}
	out := make([]ReviewOpenEntry, len(listed))
	for i, o := range listed {
		name := strings.TrimSpace(names[o.ref])
		if name == "" {
			name = unnamedReviewer
		}
		out[i] = ReviewOpenEntry{
			Reviewer: name, LastActivityAt: o.lastActivity.UTC().Truncate(time.Minute), UnsavedChanges: o.dirty,
		}
	}
	return out, len(reviews) > len(listed)
}

// reviewCount is "1 review" or "N reviews".
func reviewCount(n int) string {
	if n == 1 {
		return "1 review"
	}
	return fmt.Sprintf("%d reviews", n)
}

// checkOpenReviews runs the open-review check of a publish version of a by
// the caller, who passed every authorization check of the create. It
// answers 409 review_open and returns ok=false when reviews are open and
// the publish is not forced. A forced publish over open reviews passes this
// check, with a warning naming the reviewers and an audit entry with their
// count; later steps of the create can still refuse it.
// With the experiment off, before the artifact has a published version, or
// when nothing is open, it does nothing.
//
// It runs before the version number is taken, so a refusal leaves no
// pending version behind. A refusal has no side effects, so a client may
// always retry with force.
func (s *Service) checkOpenReviews(w http.ResponseWriter, r *http.Request, b backend, a *Artifact, force bool) ([]string, bool) {
	ctx := r.Context()
	if a.CurrentSeq == 0 || !s.featureEnabled(ctx, FeatureOpenReviewCheck) {
		return nil, true
	}
	kind, ref, _, _ := s.host.Principal(ctx)
	now := time.Now().UTC()
	sessions, err := b.store.ListOpenReviewSessions(ctx, a.ID, now.Add(-ReviewLiveWindow))
	if err != nil {
		// Presence is best effort; a failed read does not hold publishes.
		slog.WarnContext(ctx, "artifacts: reading open reviews failed", "error", err)
		return nil, true
	}
	reviews := openReviews(sessions, a.CurrentSeq, kind, ref, now)
	if len(reviews) == 0 {
		return nil, true
	}
	entries, truncated := s.reviewOpenEntries(ctx, reviews)
	if force {
		s.logAllowed(r, logEntry{action: logActionVersionForced, artifact: a, count: len(reviews)})
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Reviewer
		}
		list := strings.Join(names, ", ")
		if truncated {
			list += fmt.Sprintf(" and %d more", len(reviews)-len(entries))
		}
		verb := "was"
		if len(reviews) != 1 {
			verb = "were"
		}
		return []string{fmt.Sprintf("published while %s %s open (%s)", reviewCount(len(reviews)), verb, list)}, true
	}
	verb := "is"
	if len(reviews) != 1 {
		verb = "are"
	}
	writeJSON(w, http.StatusConflict, errorResponse{Error: errorBody{
		Code:    CodeReviewOpen,
		Message: fmt.Sprintf("%s of this artifact %s open; publish anyway with force, or wait for it", reviewCount(len(reviews)), verb),
		Details: map[string]any{"reviews": entries, "truncated": truncated, "base": a.CurrentSeq},
	}})
	return nil, false
}

// handleReviewSession implements PUT and DELETE
// /api/v1/artifacts/{id}/review-sessions/{sid}, in this order: the
// experiment (off: the route's 404), the session id's form (404), the
// caller (404 like any unreadable artifact), readableArtifact (404),
// canWriteErr (403; any access that may create a review version passes),
// for PUT a published version (409), then the per-principal limiter, then
// the body. The limiter is charged only after authorization, so only
// authorized principals create limiter entries, and no body byte is read
// before it. The row is always addressed by the caller's own principal.
func (s *Service) handleReviewSession(w http.ResponseWriter, r *http.Request, id, sid string) {
	ctx := r.Context()
	if !s.featureEnabled(ctx, FeatureOpenReviewCheck) || !sessionIDPattern.MatchString(sid) {
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
		writeGrantsReadFailed(w, r, err)
		return
	}
	if access == accessNone {
		s.logWriteRefusal(r, a)
		writeError(w, http.StatusForbidden, "forbidden", "not allowed to publish versions of this artifact")
		return
	}
	if r.Method == http.MethodPut && a.CurrentSeq == 0 {
		writeError(w, http.StatusConflict, CodeNothingToReview, "a review needs an artifact with a published version")
		return
	}
	if !s.chargePrincipal(w, LimitReviewBeat, kind, ref) {
		return
	}
	if r.Method == http.MethodDelete {
		ended, err := b.store.EndReviewSession(ctx, a.ID, kind, ref, sid)
		if err != nil {
			slog.ErrorContext(ctx, "artifacts: end review session failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not end the review session")
			return
		}
		if ended {
			s.logVersion(r, logActionReviewEnded, a, 0, access)
		}
		w.Header().Set("Cache-Control", "private, no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	req, ok := decodeReviewBeat(w, r)
	if !ok {
		return
	}
	if req.Base < 1 || req.Base > a.CurrentSeq {
		writeError(w, http.StatusBadRequest, "bad_request", "base must be a published version of this artifact")
		return
	}
	v, err := b.store.GetVersion(ctx, a.ID, req.Base)
	if errors.Is(err, ErrNotFound) || (err == nil && v.State != VersionStateReady) {
		writeError(w, http.StatusBadRequest, "bad_request", "base must be a published version of this artifact")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: get version failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the version")
		return
	}
	err = b.store.TouchReviewSession(ctx, ReviewSession{
		ArtifactID: a.ID, PrincipalKind: kind, PrincipalRef: ref, SessionID: sid, BaseSeq: req.Base, Dirty: req.Dirty,
	}, req.Edited, time.Now().UTC(), MaxReviewSessionsPerPrincipal)
	switch {
	case errors.Is(err, ErrTooManyReviewSessions):
		writeError(w, http.StatusTooManyRequests, CodeTooManyReviewSessions,
			fmt.Sprintf("you have at least %d live review sessions on this artifact; close some", MaxReviewSessionsPerPrincipal))
		return
	case err != nil:
		slog.ErrorContext(ctx, "artifacts: touch review session failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not record the review session")
		return
	}
	writeJSON(w, http.StatusOK, ReviewBeatResponse{CurrentSeq: a.CurrentSeq})
}

// decodeReviewBeat reads a heartbeat body through http.MaxBytesReader.
// Error responses never echo the body.
func decodeReviewBeat(w http.ResponseWriter, r *http.Request) (ReviewBeatRequest, bool) {
	var req ReviewBeatRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxReviewBeatBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "the request body is too large")
			return req, false
		}
		writeError(w, http.StatusBadRequest, "bad_request", "could not read the request body")
		return req, false
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		writeError(w, http.StatusBadRequest, "bad_request", `the body must be {"base": <version>, "edited": <bool>, "dirty": <bool>}`)
		return req, false
	}
	return req, true
}
