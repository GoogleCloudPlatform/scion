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

//go:build !no_sqlite

package artifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type reviewRequestTestHost struct {
	*fakeHost
	gateOn         bool
	inboxHintsOn   bool
	inboxHints     []string
	resolveErr     error
	resolveMap     map[string]RecipientSubject
	visibleUsers   map[string]bool
	visErr         error
	sentRequests   []ReviewRequestNotice
	sentDismissals []ReviewRequestDismissNotice
	notifyErr      error
	names          map[string]string
}

var (
	_ FeatureGate           = (*reviewRequestTestHost)(nil)
	_ RecipientAuthority    = (*reviewRequestTestHost)(nil)
	_ ReviewRequestNotifier = (*reviewRequestTestHost)(nil)
	_ InboxNotifier         = (*reviewRequestTestHost)(nil)
	_ PrincipalNamer        = (*reviewRequestTestHost)(nil)
)

func (h *reviewRequestTestHost) FeatureEnabled(_ context.Context, name string) bool {
	return h.gateOn && name == FeatureReviewRequests
}

func (h *reviewRequestTestHost) ResolveRecipient(_ context.Context, kind, ref string) (RecipientSubject, error) {
	if h.resolveErr != nil {
		return nil, h.resolveErr
	}
	if kind != PrincipalKindUser {
		return nil, nil
	}
	return h.resolveMap[ref], nil
}

func (h *reviewRequestTestHost) MayNotifyReviewer(_ context.Context, reviewerUserID string) (bool, error) {
	if h.visErr != nil {
		return false, h.visErr
	}
	if h.visibleUsers == nil {
		return true, nil
	}
	return h.visibleUsers[reviewerUserID], nil
}

func (h *reviewRequestTestHost) NotifyReviewRequest(_ context.Context, n ReviewRequestNotice) error {
	if h.notifyErr != nil {
		return h.notifyErr
	}
	h.sentRequests = append(h.sentRequests, n)
	return nil
}

func (h *reviewRequestTestHost) NotifyRequestDismissed(_ context.Context, n ReviewRequestDismissNotice) error {
	if h.notifyErr != nil {
		return h.notifyErr
	}
	h.sentDismissals = append(h.sentDismissals, n)
	return nil
}

func (h *reviewRequestTestHost) InboxHintsActive(_ context.Context) bool { return h.inboxHintsOn }
func (h *reviewRequestTestHost) InboxChanged(_ context.Context, userID string) {
	h.inboxHints = append(h.inboxHints, userID)
}

func (h *reviewRequestTestHost) PrincipalDisplayNames(_ context.Context, refs []string) map[string]string {
	out := make(map[string]string, len(refs))
	for _, r := range refs {
		if n, ok := h.names[r]; ok {
			out[r] = n
		}
	}
	return out
}

type staticRecipientSubject struct {
	scopes map[string]bool
}

func (s *staticRecipientSubject) Permits(_ context.Context, scopeRef, _ string) (bool, error) {
	return scopeRef != "", nil
}

func (s *staticRecipientSubject) Authorize(_ context.Context, scopeRef, _ string) (bool, error) {
	return s.scopes[scopeRef], nil
}

func newReviewRequestFixture(t *testing.T) (*Service, *reviewRequestTestHost, Store, string) {
	t.Helper()
	f := newFixture(t, false)
	st := f.store
	blobs := f.blobs
	fh := newFakeHost()
	h := &reviewRequestTestHost{
		fakeHost:     fh,
		gateOn:       true,
		inboxHintsOn: true,
		resolveMap: map[string]RecipientSubject{
			"alice": &staticRecipientSubject{scopes: map[string]bool{"proj-1": true}},
			"bob":   &staticRecipientSubject{scopes: map[string]bool{"proj-1": true}},
			"carol": &staticRecipientSubject{scopes: map[string]bool{"proj-1": true}},
			"dave":  &staticRecipientSubject{scopes: map[string]bool{"proj-2": true}},
		},
		names: map[string]string{
			"user:alice": "Alice",
			"user:bob":   "Bob",
			"user:carol": "Carol",
		},
	}
	svc := NewService(h)
	svc.SetStore(st)
	svc.SetBlobStorage(blobs, "hub-test")
	svc.SetFilterQueryDialectEnabled(func(context.Context) bool { return true })

	pAlice := principal{PrincipalKindUser, "alice", ""}
	pBob := principal{PrincipalKindUser, "bob", ""}
	pCarol := principal{PrincipalKindUser, "carol", ""}
	pDave := principal{PrincipalKindUser, "dave", ""}
	pAgent := principal{PrincipalKindAgent, "agent-1", "proj-1"}

	// Alice owns artifact in proj-1; Bob has a review grant; Carol has only read (via proj-1).
	fh.allow(pAlice, "proj-1", PermissionRead, PermissionCreate, PermissionManage, PermissionRequestReview)
	fh.allow(pBob, "proj-1", PermissionRead, PermissionCreate, PermissionRequestReview)
	fh.allow(pCarol, "proj-1", PermissionRead, PermissionCreate, PermissionRequestReview)
	fh.allow(pDave, "proj-2", PermissionRead, PermissionCreate, PermissionRequestReview)
	fh.allow(pAgent, "proj-1", PermissionRead, PermissionCreate, PermissionRequestReview)

	// Publish artifact as Alice.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts?name=doc.md&title=Design+Doc&scope=proj-1", strings.NewReader("# Hello\n"))
	req = withPrincipal(req, pAlice)
	rec := httptest.NewRecorder()
	svc.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body.String())
	}
	var pubResp ArtifactResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &pubResp); err != nil {
		t.Fatalf("decode publish: %v", err)
	}
	artID := pubResp.Artifact.ID

	// Grant Bob review access.
	grantReq := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/"+artID+"/grants",
		strings.NewReader(`{"subjectKind":"principal","subjectRef":"user:bob","permission":"review"}`))
	grantReq = withPrincipal(grantReq, pAlice)
	grantRec := httptest.NewRecorder()
	svc.ServeHTTP(grantRec, grantReq)
	if grantRec.Code != http.StatusCreated {
		t.Fatalf("grant bob review: %d %s", grantRec.Code, grantRec.Body.String())
	}
	return svc, h, st, artID
}

func doJSON(svc *Service, method, target, kind, ref string, body any) *httptest.ResponseRecorder {
	var r *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, r)
	if kind != "" {
		req = withPrincipal(req, principal{kind: kind, ref: ref})
	}
	rec := httptest.NewRecorder()
	svc.ServeHTTP(rec, req)
	return rec
}

func TestReviewRequestsGateOff(t *testing.T) {
	svc, h, _, artID := newReviewRequestFixture(t)
	h.gateOn = false

	// Routes return 404 while gate is off.
	rec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST review-requests gate off = %d, want 404", rec.Code)
	}
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-request/dismiss", "user", "bob",
		DismissReviewRequestBody{Note: "no"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST dismiss gate off = %d, want 404", rec.Code)
	}

	// GET artifact omits reviewState.
	rec = doJSON(svc, http.MethodGet, "/api/v1/artifacts/"+artID, "user", "bob", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET artifact = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"reviewState"`) {
		t.Errorf("reviewState should be absent while gate is off: %s", rec.Body.String())
	}

	// List omits reviewRequested and requested=1 / is:requested match nothing.
	rec = doJSON(svc, http.MethodGet, "/api/v1/artifacts?mine=1", "user", "bob", nil)
	if strings.Contains(rec.Body.String(), `"reviewRequested"`) {
		t.Errorf("reviewRequested should be absent while gate is off: %s", rec.Body.String())
	}
	for _, url := range []string{
		"/api/v1/artifacts?mine=1&requested=1",
		"/api/v1/artifacts?mine=1&q=is:requested",
		"/api/v1/artifacts?mine=1&scope=proj-1&q=is:requested",
	} {
		rec = doJSON(svc, http.MethodGet, url, "user", "bob", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", url, rec.Code, rec.Body.String())
		}
		var lr ArtifactListResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &lr)
		if len(lr.Artifacts) != 0 {
			t.Errorf("GET %s gate off returned %d items, want 0", url, len(lr.Artifacts))
		}
	}
}

func TestReviewRequestsAuthorization(t *testing.T) {
	svc, h, _, artID := newReviewRequestFixture(t)

	// Unreadable artifact (Dave is in proj-2) -> 404.
	rec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "dave",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("outsider = %d, want 404", rec.Code)
	}

	// Reader without publish (Carol) -> 403.
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "carol",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-publisher = %d, want 403", rec.Code)
	}

	// Review grantee (Bob) cannot publish -> 403.
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "bob",
		CreateReviewRequestsBody{Reviewers: []string{"user:carol"}})
	if rec.Code != http.StatusForbidden {
		t.Errorf("review grantee = %d, want 403", rec.Code)
	}

	// Agent requester refused in Phase 1 -> 403.
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "agent", "agent-1",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
	if rec.Code != http.StatusForbidden {
		t.Errorf("agent requester = %d, want 403", rec.Code)
	}

	// Per-reviewer outcomes: Bob (can review) -> created; Carol (cannot review) -> reviewer_cannot_review;
	// Alice (self) & unknown & hidden -> reviewer_unavailable.
	h.visibleUsers = map[string]bool{"bob": true, "carol": true}
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{
			Reviewers: []string{"user:bob", "user:carol", "user:alice", "user:ghost", "user:dave"},
			Note:      "Please check section 2",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST review-requests = %d %s", rec.Code, rec.Body.String())
	}
	var resp CreateReviewRequestsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Requests) != 5 {
		t.Fatalf("got %d results, want 5", len(resp.Requests))
	}
	want := []struct {
		reviewer, outcome, code, access string
		notified                        bool
	}{
		{"user:bob", ReviewRequestOutcomeCreated, "", ReviewRequestAccessExisting, true},
		{"user:carol", ReviewRequestOutcomeRefused, CodeReviewerCannotReview, "", false},
		{"user:alice", ReviewRequestOutcomeRefused, CodeReviewerUnavailable, "", false},
		{"user:ghost", ReviewRequestOutcomeRefused, CodeReviewerUnavailable, "", false},
		{"user:dave", ReviewRequestOutcomeRefused, CodeReviewerUnavailable, "", false},
	}
	for i, w := range want {
		got := resp.Requests[i]
		if got.Reviewer != w.reviewer || got.Outcome != w.outcome || got.Code != w.code || got.Access != w.access || got.Notified != w.notified {
			t.Errorf("result[%d] = %+v, want %+v", i, got, w)
		}
	}

	// Verify inbox hint sent for Bob only.
	if len(h.inboxHints) != 1 || h.inboxHints[0] != "bob" {
		t.Errorf("inboxHints = %v, want [bob]", h.inboxHints)
	}

	// Store/lookup fault -> 503.
	h.resolveErr = errors.New("db down")
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
	h.resolveErr = nil
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("resolve fault = %d, want 503", rec.Code)
	}
}

func TestReviewRequestDismissAndLostReadAndFilters(t *testing.T) {
	svc, h, _, artID := newReviewRequestFixture(t)

	// Alice requests Bob.
	rec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}, Note: "Check intro"})
	if rec.Code != http.StatusOK {
		t.Fatalf("request: %d %s", rec.Code, rec.Body.String())
	}

	// Bob's GET artifact and GET version show reviewState.requestedOfMe.
	for _, target := range []string{"/api/v1/artifacts/" + artID, "/api/v1/artifacts/" + artID + "/versions/1"} {
		rec = doJSON(svc, http.MethodGet, target, "user", "bob", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", target, rec.Code)
		}
		var ar ArtifactResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &ar)
		if ar.ReviewState == nil || ar.ReviewState.RequestedOfMe == nil {
			t.Fatalf("GET %s missing reviewState.requestedOfMe: %s", target, rec.Body.String())
		}
		if ar.ReviewState.RequestedOfMe.Note != "Check intro" || ar.ReviewState.RequestedOfMe.Requester.DisplayName != "Alice" {
			t.Errorf("unexpected requestedOfMe: %+v", ar.ReviewState.RequestedOfMe)
		}
	}

	// Bob's list filters: requested=1, is:requested, -is:requested (global & project).
	for _, url := range []string{
		"/api/v1/artifacts?mine=1&requested=1",
		"/api/v1/artifacts?mine=1&q=is:requested",
		"/api/v1/artifacts?mine=1&scope=proj-1&q=is:requested",
	} {
		rec = doJSON(svc, http.MethodGet, url, "user", "bob", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", url, rec.Code, rec.Body.String())
		}
		var lr ArtifactListResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &lr)
		if len(lr.Artifacts) != 1 || lr.Artifacts[0].ID != artID || !lr.Artifacts[0].ReviewRequested {
			t.Errorf("GET %s = %+v, want 1 requested artifact", url, lr.Artifacts)
		}
	}
	for _, url := range []string{
		"/api/v1/artifacts?mine=1&q=-is:requested",
		"/api/v1/artifacts?mine=1&scope=proj-1&q=-is:requested",
	} {
		rec = doJSON(svc, http.MethodGet, url, "user", "bob", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", url, rec.Code, rec.Body.String())
		}
		var lr ArtifactListResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &lr)
		if len(lr.Artifacts) != 0 {
			t.Errorf("GET %s = %d items, want 0", url, len(lr.Artifacts))
		}
	}

	// Revoke Bob's read access: requested=1 must NOT show the unreadable artifact,
	// yet POST /review-request/dismiss STILL succeeds and returns only the request row.
	h.fakeHost.deny(principal{PrincipalKindUser, "bob", ""}, "proj-1", PermissionRead)
	// Also delete Bob's review grant so Bob truly cannot read.
	grantsRec := doJSON(svc, http.MethodGet, "/api/v1/artifacts/"+artID+"/grants", "user", "alice", nil)
	var gl GrantListResponse
	_ = json.Unmarshal(grantsRec.Body.Bytes(), &gl)
	for _, g := range gl.Grants {
		if g.SubjectRef == "user:bob" {
			delRec := doJSON(svc, http.MethodDelete, "/api/v1/artifacts/"+artID+"/grants/"+g.ID, "user", "alice", nil)
			if delRec.Code != http.StatusNoContent {
				t.Fatalf("delete bob grant: %d", delRec.Code)
			}
		}
	}
	rec = doJSON(svc, http.MethodGet, "/api/v1/artifacts?mine=1&requested=1", "user", "bob", nil)
	var lr ArtifactListResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &lr)
	if len(lr.Artifacts) != 0 {
		t.Fatalf("unreadable artifact leaked in requested=1: %+v", lr.Artifacts)
	}

	h.inboxHints = nil
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-request/dismiss", "user", "bob",
		DismissReviewRequestBody{Note: "Cannot review this week"})
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss after lost read = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Design Doc") {
		t.Errorf("dismiss response leaked artifact title: %s", rec.Body.String())
	}
	var dr DismissReviewRequestResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &dr)
	if dr.Request.State != ReviewRequestStateDismissed || dr.Request.ResponseNote != "Cannot review this week" || !dr.AuthorNotified {
		t.Errorf("unexpected dismiss response: %+v", dr)
	}
	if len(h.inboxHints) != 1 || h.inboxHints[0] != "bob" {
		t.Errorf("dismiss inboxHints = %v, want [bob]", h.inboxHints)
	}
	if len(h.sentDismissals) != 1 || h.sentDismissals[0].ResponseNote != "Cannot review this week" {
		t.Errorf("sentDismissals = %+v", h.sentDismissals)
	}

	// Second dismiss -> 404.
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-request/dismiss", "user", "bob", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("second dismiss = %d, want 404", rec.Code)
	}
}

func TestReviewRequestNotifyRateLimitedDoesNotBlockState(t *testing.T) {
	svc, _, _, artID := newReviewRequestFixture(t)
	// Replace notifyLimiter with a 0-burst limiter so notification is rate-limited.
	svc.notifyLimiterOnce.Do(func() {})
	svc.notifyLimiter = newPeriodLimiter(time.Now, 1, time.Hour, 1, 1, time.Hour, 1, 100)
	// Exhaust the 1 token.
	svc.notifyLimiter.allow("user:alice")

	rec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST review-requests = %d %s", rec.Code, rec.Body.String())
	}
	var resp CreateReviewRequestsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Requests) != 1 {
		t.Fatalf("got %d results, want 1", len(resp.Requests))
	}
	r := resp.Requests[0]
	if r.Outcome != ReviewRequestOutcomeCreated || r.Notified || r.NotifySkipped != NotifySkippedRateLimited {
		t.Errorf("got %+v, want created + notified=false + notifySkipped=rate_limited", r)
	}
}
