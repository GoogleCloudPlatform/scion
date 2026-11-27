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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type reviewRequestTestHost struct {
	*fakeHost
	gateOn         bool
	draftsGateOn   bool
	nonInteractive bool
	inboxHintsOn   bool
	inboxHints     []string
	resolveErr     error
	resolveErrFor  map[string]error
	resolveMap     map[string]RecipientSubject
	visibleUsers   map[string]bool
	visErr         error
	sentRequests   []ReviewRequestNotice
	sentDismissals []ReviewRequestDismissNotice
	closedAcks     []string
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
	switch name {
	case FeatureReviewRequests:
		return h.gateOn
	case FeatureReviewDrafts:
		return h.draftsGateOn
	default:
		return false
	}
}

func (h *reviewRequestTestHost) IsInteractiveSession(_ context.Context) bool {
	return !h.nonInteractive
}

func (h *reviewRequestTestHost) ResolveRecipient(_ context.Context, kind, ref string) (RecipientSubject, error) {
	if h.resolveErr != nil {
		return nil, h.resolveErr
	}
	if err := h.resolveErrFor[ref]; err != nil {
		return nil, err
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

func (h *reviewRequestTestHost) RequestClosed(_ context.Context, artifactID, reviewerUserID string) {
	h.closedAcks = append(h.closedAcks, artifactID+":"+reviewerUserID)
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

	// GET artifact omits requestedOfMe while gate is off (reviewState.reviewOnLatest is ungated).
	rec = doJSON(svc, http.MethodGet, "/api/v1/artifacts/"+artID, "user", "bob", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET artifact = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"requestedOfMe"`) {
		t.Errorf("requestedOfMe should be absent while gate is off: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"reviewOnLatest":false`) {
		t.Errorf("reviewOnLatest should be present while gate is off: %s", rec.Body.String())
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

func TestReviewRequestsGateOffWithExistingRow(t *testing.T) {
	svc, h, _, artID := newReviewRequestFixture(t)
	h.gateOn = true

	// Create an open review request while the gate is on.
	rec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}, Note: "please review"})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST review-requests gate on = %d %s", rec.Code, rec.Body.String())
	}

	// Turn the gate off with the open request row still in the database.
	h.gateOn = false

	// Positive requested filters must return 0 rows.
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
		if err := json.Unmarshal(rec.Body.Bytes(), &lr); err != nil {
			t.Fatalf("unmarshal %s: %v", url, err)
		}
		if len(lr.Artifacts) != 0 {
			t.Errorf("GET %s gate off with existing row returned %d items, want 0", url, len(lr.Artifacts))
		}
	}

	// Negated requested filters must still return the artifact.
	for _, url := range []string{
		"/api/v1/artifacts?mine=1&q=-is:requested",
		"/api/v1/artifacts?mine=1&scope=proj-1&q=-is:requested",
	} {
		rec = doJSON(svc, http.MethodGet, url, "user", "bob", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", url, rec.Code, rec.Body.String())
		}
		var lr ArtifactListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &lr); err != nil {
			t.Fatalf("unmarshal %s: %v", url, err)
		}
		if len(lr.Artifacts) != 1 || lr.Artifacts[0].ID != artID {
			t.Errorf("GET %s gate off with existing row returned %d items, want 1 (%s)", url, len(lr.Artifacts), artID)
		}
	}

	// List must omit reviewRequested even though the row exists.
	rec = doJSON(svc, http.MethodGet, "/api/v1/artifacts?mine=1", "user", "bob", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET list = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"reviewRequested"`) {
		t.Errorf("reviewRequested should be absent while gate is off with existing row: %s", rec.Body.String())
	}

	// GET artifact must omit requestedOfMe even though the row exists (reviewOnLatest is ungated).
	rec = doJSON(svc, http.MethodGet, "/api/v1/artifacts/"+artID, "user", "bob", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET artifact = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"requestedOfMe"`) {
		t.Errorf("requestedOfMe should be absent while gate is off with existing row: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"reviewOnLatest":false`) {
		t.Errorf("reviewOnLatest should be present while gate is off with existing row: %s", rec.Body.String())
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

	// Non-interactive credential is refused at the route level with 403 interactive_session_required
	// before charging the limiter or entering the per-reviewer loop.
	h.nonInteractive = true
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
	h.nonInteractive = false
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), CodeInteractiveSessionRequired) {
		t.Errorf("non-interactive = %d %s, want 403 %s", rec.Code, rec.Body.String(), CodeInteractiveSessionRequired)
	}

	// Grant Eve review access so she CAN review, but omit her from visibleUsers
	// to prove step 4 (anti-spam visibility) runs before step 5a (access check).
	h.resolveMap["eve"] = &staticRecipientSubject{scopes: map[string]bool{"proj-1": true}}
	h.names["user:eve"] = "Eve"
	eveGrantRec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/grants", "user", "alice",
		GrantRequest{SubjectKind: "principal", SubjectRef: "user:eve", Permission: GrantReview})
	if eveGrantRec.Code != http.StatusCreated {
		t.Fatalf("grant eve review = %d %s", eveGrantRec.Code, eveGrantRec.Body.String())
	}

	// Per-reviewer outcomes when requester is owner/admin (Alice):
	// Bob (can review) -> created (access=existing);
	// Carol (cannot review yet, Alice is admin) -> created (access=raised);
	// Eve (can review, but invisible to requester) -> reviewer_unavailable;
	// Alice (self) & unknown & hidden -> reviewer_unavailable.
	h.visibleUsers = map[string]bool{"bob": true, "carol": true}
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{
			Reviewers: []string{"user:bob", "user:carol", "user:eve", "user:alice", "user:ghost", "user:dave"},
			Note:      "Please check section 2",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST review-requests = %d %s", rec.Code, rec.Body.String())
	}
	var resp CreateReviewRequestsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Requests) != 6 {
		t.Fatalf("got %d results, want 6", len(resp.Requests))
	}
	want := []struct {
		reviewer, outcome, code, access string
		notified                        bool
	}{
		{"user:bob", ReviewRequestOutcomeCreated, "", ReviewRequestAccessExisting, true},
		{"user:carol", ReviewRequestOutcomeCreated, "", ReviewRequestAccessRaised, true},
		{"user:eve", ReviewRequestOutcomeRefused, CodeReviewerUnavailable, "", false},
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

	// Verify inbox hints sent for Bob and Carol.
	if len(h.inboxHints) != 2 || h.inboxHints[0] != "bob" || h.inboxHints[1] != "carol" {
		t.Errorf("inboxHints = %v, want [bob carol]", h.inboxHints)
	}

	// Non-admin publisher (write grantee) requesting a user who cannot review -> reviewer_cannot_review.
	h.resolveMap["frank"] = &staticRecipientSubject{scopes: map[string]bool{"proj-1": true}}
	h.visibleUsers["frank"] = true
	grantRec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/grants", "user", "alice",
		GrantRequest{SubjectKind: "principal", SubjectRef: "user:carol", Permission: GrantWrite})
	if grantRec.Code != http.StatusOK {
		t.Fatalf("upgrade carol to write = %d %s", grantRec.Code, grantRec.Body.String())
	}
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "carol",
		CreateReviewRequestsBody{Reviewers: []string{"user:frank"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("carol request frank = %d %s", rec.Code, rec.Body.String())
	}
	var frankResp CreateReviewRequestsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &frankResp)
	if len(frankResp.Requests) != 1 || frankResp.Requests[0].Outcome != ReviewRequestOutcomeRefused || frankResp.Requests[0].Code != CodeReviewerCannotReview {
		t.Errorf("non-admin publisher request = %+v, want refused reviewer_cannot_review", frankResp.Requests)
	}

	// Lookup fault records per-reviewer refused (unavailable) instead of 503.
	h.resolveErr = errors.New("db down")
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
	h.resolveErr = nil
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve fault = %d, want 200 with per-reviewer refused", rec.Code)
	}
	var faultResp CreateReviewRequestsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &faultResp)
	if len(faultResp.Requests) != 1 || faultResp.Requests[0].Outcome != ReviewRequestOutcomeRefused || faultResp.Requests[0].Code != "unavailable" {
		t.Errorf("resolve fault result = %+v, want refused unavailable", faultResp.Requests)
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

	// Credential refusal (Permits false, modeling a scoped UAT lacking artifact:read
	// or bounded to another project) must answer 404 and leave the open row untouched.
	h.deny(principal{PrincipalKindUser, "bob", ""}, "proj-1", PermissionRead)
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-request/dismiss", "user", "bob",
		DismissReviewRequestBody{Note: "Should be refused"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("dismiss with denied credential = %d, want 404", rec.Code)
	}
	delete(h.denied, PrincipalRef(PrincipalKindUser, "bob")+" proj-1 "+PermissionRead)

	// Revoke Bob's read access via role (Authorize) and grant removal — NOT via Permits,
	// because session credentials still pass Permits: requested=1 must NOT show the
	// unreadable artifact, yet POST /review-request/dismiss STILL succeeds and returns
	// only the request row.
	delete(h.perms[PrincipalRef(PrincipalKindUser, "bob")]["proj-1"], PermissionRead)
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

type noNotifierReviewRequestHost struct {
	*fakeHost
	resolveMap map[string]RecipientSubject
}

func (h *noNotifierReviewRequestHost) FeatureEnabled(_ context.Context, name string) bool {
	return name == FeatureReviewRequests
}

func (h *noNotifierReviewRequestHost) ResolveRecipient(_ context.Context, kind, ref string) (RecipientSubject, error) {
	if kind != PrincipalKindUser {
		return nil, nil
	}
	return h.resolveMap[ref], nil
}

func TestReviewRequestsFailClosedWithoutNotifier(t *testing.T) {
	f := newFixture(t, false)
	fh := newFakeHost()
	fh.allow(principal{PrincipalKindUser, "alice", ""}, "proj-1", PermissionRead, PermissionCreate, PermissionManage, PermissionRequestReview)
	fh.allow(principal{PrincipalKindUser, "bob", ""}, "proj-1", PermissionRead)
	h := &noNotifierReviewRequestHost{
		fakeHost: fh,
		resolveMap: map[string]RecipientSubject{
			"bob": &staticRecipientSubject{scopes: map[string]bool{"proj-1": true}},
		},
	}
	svc := NewService(h)
	svc.SetStore(f.store)
	svc.SetBlobStorage(f.blobs, "hub-test")

	pubReq := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts?name=doc.md&title=Design+Doc&scope=proj-1", strings.NewReader("# Hello\n"))
	pubReq = withPrincipal(pubReq, principal{PrincipalKindUser, "alice", ""})
	pubRec := httptest.NewRecorder()
	svc.ServeHTTP(pubRec, pubReq)
	if pubRec.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", pubRec.Code, pubRec.Body.String())
	}
	var pubResp ArtifactResponse
	if err := json.Unmarshal(pubRec.Body.Bytes(), &pubResp); err != nil {
		t.Fatalf("decode publish: %v", err)
	}

	rec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+pubResp.Artifact.ID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST review-requests without ReviewRequestNotifier = %d %s, want 503", rec.Code, rec.Body.String())
	}
}

func TestReviewRequestRemindUpdatesNoteAndCooldownUnchanged(t *testing.T) {
	svc, h, _, artID := newReviewRequestFixture(t)

	// Initial create.
	rec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}, Note: "initial note"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}

	// First remind with a new note updates the stored note and sends it.
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}, Note: "please prioritize"})
	if rec.Code != http.StatusOK {
		t.Fatalf("remind = %d %s", rec.Code, rec.Body.String())
	}
	var remResp CreateReviewRequestsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &remResp)
	if len(remResp.Requests) != 1 || remResp.Requests[0].Outcome != ReviewRequestOutcomeReminded || !remResp.Requests[0].Notified {
		t.Fatalf("remind response = %+v, want reminded + notified", remResp.Requests)
	}
	if remResp.Requests[0].Request.Note != "please prioritize" {
		t.Errorf("remind stored note = %q, want %q", remResp.Requests[0].Request.Note, "please prioritize")
	}
	if len(h.sentRequests) != 2 || h.sentRequests[1].Note != "please prioritize" {
		t.Errorf("sentRequests = %+v, want second notice with updated note", h.sentRequests)
	}

	// Second remind within cooldown returns unchanged, keeps stored note, sends no notice.
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}, Note: "should be ignored"})
	if rec.Code != http.StatusOK {
		t.Fatalf("cooldown remind = %d %s", rec.Code, rec.Body.String())
	}
	var cdResp CreateReviewRequestsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &cdResp)
	if len(cdResp.Requests) != 1 || cdResp.Requests[0].Outcome != ReviewRequestOutcomeUnchanged || cdResp.Requests[0].Notified {
		t.Fatalf("cooldown response = %+v, want unchanged + not notified", cdResp.Requests)
	}
	if cdResp.Requests[0].Request.Note != "please prioritize" {
		t.Errorf("cooldown note = %q, want %q", cdResp.Requests[0].Request.Note, "please prioritize")
	}
	if len(h.sentRequests) != 2 {
		t.Errorf("sentRequests count = %d, want 2 (no notice during cooldown)", len(h.sentRequests))
	}
}

func TestReviewRequestRemindByOtherPublisherPreservesNoteAndPopulatesRequesterName(t *testing.T) {
	svc, h, _, artID := newReviewRequestFixture(t)

	// Grant Carol write access so she can publish and remind.
	grantRec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/grants", "user", "alice",
		GrantRequest{SubjectKind: "principal", SubjectRef: "user:carol", Permission: GrantWrite})
	if grantRec.Code != http.StatusCreated {
		t.Fatalf("grant carol write = %d %s", grantRec.Code, grantRec.Body.String())
	}

	// Alice requests review from Bob with note A.
	rec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}, Note: "note A"})
	if rec.Code != http.StatusOK {
		t.Fatalf("alice create = %d %s", rec.Code, rec.Body.String())
	}

	// Carol (a different publisher) reminds Bob with note C.
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "carol",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}, Note: "note C"})
	if rec.Code != http.StatusOK {
		t.Fatalf("carol remind = %d %s", rec.Code, rec.Body.String())
	}
	var remResp CreateReviewRequestsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &remResp); err != nil {
		t.Fatalf("decode carol remind: %v", err)
	}
	if len(remResp.Requests) != 1 || remResp.Requests[0].Outcome != ReviewRequestOutcomeReminded || !remResp.Requests[0].Notified {
		t.Fatalf("carol remind = %+v, want reminded + notified", remResp.Requests)
	}
	rr := remResp.Requests[0].Request
	if rr.Requester.Ref != "alice" || rr.Requester.DisplayName != "Alice" {
		t.Errorf("reminded requester = %+v, want ref=alice displayName=Alice", rr.Requester)
	}
	if rr.Note != "note A" {
		t.Errorf("reminded stored note = %q, want %q (must not overwrite original requester's note)", rr.Note, "note A")
	}

	// Bob's GET artifact still shows requester Alice and note A.
	getRec := doJSON(svc, http.MethodGet, "/api/v1/artifacts/"+artID, "user", "bob", nil)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET artifact = %d %s", getRec.Code, getRec.Body.String())
	}
	var ar ArtifactResponse
	if err := json.Unmarshal(getRec.Body.Bytes(), &ar); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if ar.ReviewState == nil || ar.ReviewState.RequestedOfMe == nil {
		t.Fatalf("missing requestedOfMe: %s", getRec.Body.String())
	}
	if ar.ReviewState.RequestedOfMe.Requester.Ref != "alice" || ar.ReviewState.RequestedOfMe.Requester.DisplayName != "Alice" || ar.ReviewState.RequestedOfMe.Note != "note A" {
		t.Errorf("requestedOfMe = %+v, want requester Alice with note A", ar.ReviewState.RequestedOfMe)
	}

	// The reminder notice Bob received attributes note C to Carol.
	if len(h.sentRequests) != 2 {
		t.Fatalf("sentRequests count = %d, want 2", len(h.sentRequests))
	}
	if h.sentRequests[1].RequesterRef != "carol" || h.sentRequests[1].Note != "note C" {
		t.Errorf("reminder notice = %+v, want RequesterRef=carol Note=%q", h.sentRequests[1], "note C")
	}
}

func TestReviewRequestCanonicalisesReviewerUUIDs(t *testing.T) {
	f := newFixture(t, false)
	fh := newFakeHost()
	aliceUUID := "11111111-1111-4111-8111-111111111111"
	bobUUID := "22222222-2222-4222-8222-222222222222"
	h := &reviewRequestTestHost{
		fakeHost:     fh,
		gateOn:       true,
		inboxHintsOn: true,
		resolveMap: map[string]RecipientSubject{
			aliceUUID: &staticRecipientSubject{scopes: map[string]bool{"proj-1": true}},
			bobUUID:   &staticRecipientSubject{scopes: map[string]bool{"proj-1": true}},
		},
		names: map[string]string{
			"user:" + aliceUUID: "Alice",
			"user:" + bobUUID:   "Bob",
		},
	}
	svc := NewService(h)
	svc.SetStore(f.store)
	svc.SetBlobStorage(f.blobs, "hub-test")

	pAlice := principal{PrincipalKindUser, aliceUUID, ""}
	pBob := principal{PrincipalKindUser, bobUUID, ""}
	fh.allow(pAlice, "proj-1", PermissionRead, PermissionCreate, PermissionManage, PermissionRequestReview)
	fh.allow(pBob, "proj-1", PermissionRead)

	pubReq := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts?name=doc.md&title=UUID+Doc&scope=proj-1", strings.NewReader("# UUID\n"))
	pubReq = withPrincipal(pubReq, pAlice)
	pubRec := httptest.NewRecorder()
	svc.ServeHTTP(pubRec, pubReq)
	if pubRec.Code != http.StatusCreated {
		t.Fatalf("publish = %d %s", pubRec.Code, pubRec.Body.String())
	}
	var pubResp ArtifactResponse
	_ = json.Unmarshal(pubRec.Body.Bytes(), &pubResp)
	artID := pubResp.Artifact.ID

	grantRec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/grants", "user", aliceUUID,
		GrantRequest{SubjectKind: "principal", SubjectRef: "user:" + bobUUID, Permission: GrantReview})
	if grantRec.Code != http.StatusCreated {
		t.Fatalf("grant bob = %d %s", grantRec.Code, grantRec.Body.String())
	}

	// Pass uppercase Bob UUID, duplicate lowercase Bob UUID, and uppercase self UUID.
	rec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", aliceUUID,
		CreateReviewRequestsBody{
			Reviewers: []string{
				"user:" + strings.ToUpper(bobUUID),
				"user:" + bobUUID,
				"user:" + strings.ToUpper(aliceUUID),
			},
			Note: "Check UUID",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST review-requests = %d %s", rec.Code, rec.Body.String())
	}
	var resp CreateReviewRequestsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Requests) != 2 {
		t.Fatalf("got %d deduplicated results, want 2: %+v", len(resp.Requests), resp.Requests)
	}
	if resp.Requests[0].Reviewer != "user:"+bobUUID || resp.Requests[0].Outcome != ReviewRequestOutcomeCreated || resp.Requests[0].Request.Reviewer.Ref != bobUUID {
		t.Errorf("result[0] = %+v, want canonical lowercase bobUUID created", resp.Requests[0])
	}
	if resp.Requests[1].Reviewer != "user:"+aliceUUID || resp.Requests[1].Outcome != ReviewRequestOutcomeRefused || resp.Requests[1].Code != CodeReviewerUnavailable {
		t.Errorf("result[1] = %+v, want canonical self refused reviewer_unavailable", resp.Requests[1])
	}

	// Bob sees requestedOfMe and can dismiss with his canonical lowercase UUID.
	getRec := doJSON(svc, http.MethodGet, "/api/v1/artifacts/"+artID, "user", bobUUID, nil)
	var ar ArtifactResponse
	_ = json.Unmarshal(getRec.Body.Bytes(), &ar)
	if ar.ReviewState == nil || ar.ReviewState.RequestedOfMe == nil {
		t.Fatalf("bob cannot see requestedOfMe after uppercase UUID request: %s", getRec.Body.String())
	}
	disRec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-request/dismiss", "user", bobUUID, nil)
	if disRec.Code != http.StatusOK {
		t.Fatalf("bob dismiss = %d %s, want 200", disRec.Code, disRec.Body.String())
	}
}

func TestListReviewRequestsPublisherAndSelfView(t *testing.T) {
	svc, h, st, artID := newReviewRequestFixture(t)
	h.draftsGateOn = true

	// Request Bob (existing) and Carol (raised by owner Alice).
	rec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob", "user:carol"}, Note: "Check v1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}

	// Bob saves a draft; Eve (unsolicited reviewer with review grant) also saves a draft.
	_, _, err := st.PutReviewDraft(context.Background(), ReviewDraft{
		ArtifactID: artID, PrincipalKind: PrincipalKindUser, PrincipalRef: "bob",
		BaseSeq: 1, EntryPath: "doc.md", Body: "SECRET BOB DRAFT CONTENT",
	}, 0, time.Now().UTC(), MaxDraftsPerPrincipal)
	if err != nil {
		t.Fatalf("PutReviewDraft bob: %v", err)
	}
	_, _, err = st.PutReviewDraft(context.Background(), ReviewDraft{
		ArtifactID: artID, PrincipalKind: PrincipalKindUser, PrincipalRef: "eve",
		BaseSeq: 1, EntryPath: "doc.md", Body: "SECRET EVE DRAFT CONTENT",
	}, 0, time.Now().UTC(), MaxDraftsPerPrincipal)
	if err != nil {
		t.Fatalf("PutReviewDraft eve: %v", err)
	}

	// Revoke Carol's review grant so Carol has reviewerCanReview=false.
	grants, _ := st.ListGrants(context.Background(), artID)
	for _, g := range grants {
		if g.SubjectRef == "user:carol" {
			_ = st.DeleteGrant(context.Background(), artID, g.ID, &Actor{Kind: PrincipalKindUser, Ref: "alice", Authority: AuthorityOwner})
		}
	}

	// Publisher view (Alice): sees both Bob and Carol, never Eve, never draft content.
	rec = doJSON(svc, http.MethodGet, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET review-requests (publisher) = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "SECRET") || strings.Contains(rec.Body.String(), "eve") {
		t.Fatalf("publisher response leaked draft content or unsolicited drafter: %s", rec.Body.String())
	}
	var pubList ListReviewRequestsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &pubList); err != nil {
		t.Fatalf("decode pubList: %v", err)
	}
	if len(pubList.Requests) != 2 {
		t.Fatalf("pubList len = %d, want 2", len(pubList.Requests))
	}
	byReviewer := map[string]ReviewRequestInfo{}
	for _, r := range pubList.Requests {
		byReviewer[r.Reviewer.Ref] = r
	}
	if bobR := byReviewer["bob"]; bobR.ReviewerCanReview == nil || !*bobR.ReviewerCanReview || !bobR.InDraft {
		t.Errorf("bob publisher view = %+v, want reviewerCanReview=true inDraft=true", bobR)
	}
	if carolR := byReviewer["carol"]; carolR.ReviewerCanReview == nil || *carolR.ReviewerCanReview || carolR.InDraft {
		t.Errorf("carol publisher view = %+v, want reviewerCanReview=false inDraft=false", carolR)
	}

	// With drafts gate OFF: inDraft is false for Bob.
	h.draftsGateOn = false
	rec = doJSON(svc, http.MethodGet, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice", nil)
	var pubNoDrafts ListReviewRequestsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &pubNoDrafts)
	for _, r := range pubNoDrafts.Requests {
		if r.InDraft {
			t.Errorf("inDraft must be false when drafts gate is off: %+v", r)
		}
	}
	h.draftsGateOn = true

	// Self-only view (Bob): sees ONLY Bob's request, without reviewerCanReview or inDraft.
	rec = doJSON(svc, http.MethodGet, "/api/v1/artifacts/"+artID+"/review-requests", "user", "bob", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET review-requests (self) = %d %s", rec.Code, rec.Body.String())
	}
	var selfList ListReviewRequestsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &selfList)
	if len(selfList.Requests) != 1 || selfList.Requests[0].Reviewer.Ref != "bob" {
		t.Fatalf("selfList = %+v, want only bob", selfList.Requests)
	}
	if selfList.Requests[0].ReviewerCanReview != nil || selfList.Requests[0].InDraft {
		t.Errorf("self view must omit reviewerCanReview and inDraft: %+v", selfList.Requests[0])
	}
}

func TestWithdrawReviewRequestPermissionsAndLifecycle(t *testing.T) {
	svc, h, st, artID := newReviewRequestFixture(t)

	// Alice requests Carol (access=raised).
	rec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:carol"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var createResp CreateReviewRequestsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &createResp)
	reqID := createResp.Requests[0].Request.ID

	// Non-requester reviewer (Bob) and requested reviewer (Carol) cannot withdraw -> 403.
	for _, who := range []string{"bob", "carol"} {
		wrec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests/"+reqID+"/withdraw", "user", who, nil)
		if wrec.Code != http.StatusForbidden {
			t.Errorf("withdraw by %s = %d, want 403", who, wrec.Code)
		}
	}

	// Agent guard unit test: an agent may withdraw ONLY requests it made,
	// even if it owns the artifact.
	agentArt, _, _, _ := seedArtifact(t, st, "")
	// Give agent-1 read/publish on a fresh agent-owned artifact by inserting a request
	// created by user:alice and another created by agent:agent-1.
	aRow, _ := st.GetArtifact(context.Background(), artID)
	fakeOwnedByAgent := *aRow
	fakeOwnedByAgent.OwnerKind = PrincipalKindAgent
	fakeOwnedByAgent.OwnerRef = "agent-1"
	b, _ := svc.backend()
	pAgent1 := principal{PrincipalKindAgent, "agent-1", "proj-1"}
	agentCtx := withPrincipal(httptest.NewRequest(http.MethodPost, "/", nil), pAgent1).Context()
	_, allowedOther, err := svc.withdrawAuthority(agentCtx, b, &fakeOwnedByAgent, &ReviewRequest{
		RequesterKind: PrincipalKindUser, RequesterRef: "alice",
	}, PrincipalKindAgent, "agent-1")
	if err != nil || allowedOther {
		t.Errorf("agent withdrawing user's request on agent-owned artifact = %v, %v; want false, nil", allowedOther, err)
	}
	actorOwn, allowedOwn, err := svc.withdrawAuthority(agentCtx, b, &fakeOwnedByAgent, &ReviewRequest{
		RequesterKind: PrincipalKindAgent, RequesterRef: "agent-1",
	}, PrincipalKindAgent, "agent-1")
	if err != nil || !allowedOwn || actorOwn == nil {
		t.Errorf("agent withdrawing its own request = %v, %v; want true, nil", allowedOwn, err)
	}
	// Agent whose credential lacks artifact.request_review cannot withdraw even its own request.
	h.deny(pAgent1, "proj-1", PermissionRequestReview)
	_, allowedAgentDenied, err := svc.withdrawAuthority(agentCtx, b, &fakeOwnedByAgent, &ReviewRequest{
		RequesterKind: PrincipalKindAgent, RequesterRef: "agent-1",
	}, PrincipalKindAgent, "agent-1")
	delete(h.denied, PrincipalRef(PrincipalKindAgent, "agent-1")+" proj-1 "+PermissionRequestReview)
	if err != nil || allowedAgentDenied {
		t.Errorf("agent lacking artifact.request_review withdrawing own request = %v, %v; want false, nil", allowedAgentDenied, err)
	}
	_ = agentArt

	// R1 credential-ceiling tests:
	// Give Carol write access so she can publish and create a review request for Bob.
	if gRec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/grants", "user", "alice",
		GrantRequest{SubjectKind: SubjectPrincipal, SubjectRef: "user:carol", Permission: GrantWrite}); gRec.Code != http.StatusOK {
		t.Fatalf("upgrade carol to write = %d %s", gRec.Code, gRec.Body.String())
	}
	carolReqRec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "carol",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}, Note: "carol asks bob"})
	if carolReqRec.Code != http.StatusOK {
		t.Fatalf("carol create request = %d %s", carolReqRec.Code, carolReqRec.Body.String())
	}
	var carolCreateResp CreateReviewRequestsResponse
	_ = json.Unmarshal(carolReqRec.Body.Bytes(), &carolCreateResp)
	carolReqID := carolCreateResp.Requests[0].Request.ID

	// (a) Owner (Alice) with a credential lacking artifact.manage withdrawing
	// another publisher's (Carol's) request -> 403.
	pAlice := principal{PrincipalKindUser, "alice", ""}
	h.deny(pAlice, "proj-1", PermissionManage)
	wAliceDenied := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests/"+carolReqID+"/withdraw", "user", "alice", nil)
	delete(h.denied, PrincipalRef(PrincipalKindUser, "alice")+" proj-1 "+PermissionManage)
	if wAliceDenied.Code != http.StatusForbidden {
		t.Errorf("owner lacking artifact.manage withdrawing other's request = %d %s, want 403", wAliceDenied.Code, wAliceDenied.Body.String())
	}

	// (b) Requester (Carol) with a credential lacking artifact.request_review
	// withdrawing her own request -> 403; full session -> 200.
	pCarol := principal{PrincipalKindUser, "carol", ""}
	h.deny(pCarol, "proj-1", PermissionRequestReview)
	wCarolDenied := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests/"+carolReqID+"/withdraw", "user", "carol", nil)
	delete(h.denied, PrincipalRef(PrincipalKindUser, "carol")+" proj-1 "+PermissionRequestReview)
	if wCarolDenied.Code != http.StatusForbidden {
		t.Errorf("requester lacking artifact.request_review withdrawing own request = %d %s, want 403", wCarolDenied.Code, wCarolDenied.Body.String())
	}
	wCarolOK := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests/"+carolReqID+"/withdraw", "user", "carol", nil)
	if wCarolOK.Code != http.StatusOK {
		t.Errorf("requester with full session withdrawing own request = %d %s, want 200", wCarolOK.Code, wCarolOK.Body.String())
	}

	// Owner (Alice) with full session withdraws Carol's request -> 200, RequestClosed + inbox hint fired, raised grant kept.
	h.inboxHints = nil
	h.closedAcks = nil
	wrec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests/"+reqID+"/withdraw", "user", "alice", nil)
	if wrec.Code != http.StatusOK {
		t.Fatalf("withdraw by alice = %d %s", wrec.Code, wrec.Body.String())
	}
	var wResp WithdrawReviewRequestResponse
	_ = json.Unmarshal(wrec.Body.Bytes(), &wResp)
	if wResp.Request.State != ReviewRequestStateWithdrawn {
		t.Errorf("withdrawn state = %q, want withdrawn", wResp.Request.State)
	}
	if len(h.closedAcks) != 1 || h.closedAcks[0] != artID+":carol" {
		t.Errorf("closedAcks = %v, want [%s:carol]", h.closedAcks, artID)
	}
	if len(h.inboxHints) != 1 || h.inboxHints[0] != "carol" {
		t.Errorf("inboxHints = %v, want [carol]", h.inboxHints)
	}

	// Second withdraw returns 409 request_closed.
	wrec2 := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests/"+reqID+"/withdraw", "user", "alice", nil)
	if wrec2.Code != http.StatusConflict || !strings.Contains(wrec2.Body.String(), CodeRequestClosed) {
		t.Errorf("second withdraw = %d %s, want 409 %s", wrec2.Code, wrec2.Body.String(), CodeRequestClosed)
	}
}

func TestMidBatchFaultContinuesWithoutAbortingBatch(t *testing.T) {
	svc, h, _, artID := newReviewRequestFixture(t)
	// Grant Carol and Eve review access; fail resolver on Carol (second reviewer).
	h.resolveMap["eve"] = &staticRecipientSubject{scopes: map[string]bool{"proj-1": true}}
	h.names["user:eve"] = "Eve"
	for _, u := range []string{"carol", "eve"} {
		doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/grants", "user", "alice",
			GrantRequest{SubjectKind: "principal", SubjectRef: "user:" + u, Permission: GrantReview})
	}
	h.resolveErrFor = map[string]error{"carol": errors.New("transient ldap timeout")}

	rec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob", "user:carol", "user:eve"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("mid-batch fault = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var resp CreateReviewRequestsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Requests) != 3 {
		t.Fatalf("len(requests) = %d, want 3", len(resp.Requests))
	}
	if resp.Requests[0].Outcome != ReviewRequestOutcomeCreated || !resp.Requests[0].Notified {
		t.Errorf("reviewer 1 (bob) = %+v, want created+notified", resp.Requests[0])
	}
	if resp.Requests[1].Outcome != ReviewRequestOutcomeRefused || resp.Requests[1].Code != "unavailable" {
		t.Errorf("reviewer 2 (carol) = %+v, want refused unavailable", resp.Requests[1])
	}
	if resp.Requests[2].Outcome != ReviewRequestOutcomeCreated || !resp.Requests[2].Notified {
		t.Errorf("reviewer 3 (eve) = %+v, want created+notified", resp.Requests[2])
	}

	// Retry with resolver healthy: reviewer 1 (bob) -> reminded (1 extra notice),
	// reviewer 2 (carol) -> created (notified), reviewer 3 (eve) -> reminded.
	h.resolveErrFor = nil
	noticesBeforeRetry := len(h.sentRequests)
	rec2 := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob", "user:carol", "user:eve"}})
	if rec2.Code != http.StatusOK {
		t.Fatalf("retry 1 = %d %s, want 200", rec2.Code, rec2.Body.String())
	}
	var resp2 CreateReviewRequestsResponse
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp2)
	if len(resp2.Requests) != 3 {
		t.Fatalf("retry 1 len = %d, want 3", len(resp2.Requests))
	}
	if resp2.Requests[0].Outcome != ReviewRequestOutcomeReminded || !resp2.Requests[0].Notified {
		t.Errorf("retry 1 bob = %+v, want reminded+notified", resp2.Requests[0])
	}
	if resp2.Requests[1].Outcome != ReviewRequestOutcomeCreated || !resp2.Requests[1].Notified {
		t.Errorf("retry 1 carol = %+v, want created+notified", resp2.Requests[1])
	}
	if resp2.Requests[2].Outcome != ReviewRequestOutcomeReminded || !resp2.Requests[2].Notified {
		t.Errorf("retry 1 eve = %+v, want reminded+notified", resp2.Requests[2])
	}
	if len(h.sentRequests) != noticesBeforeRetry+3 {
		t.Errorf("notices sent on retry 1 = %d, want 3", len(h.sentRequests)-noticesBeforeRetry)
	}

	// Retry once more within 24h cooldown: reviewer 1 (bob) and reviewer 3 (eve)
	// answer unchanged with no notice; reviewer 2 (carol) answers reminded.
	noticesBeforeRetry2 := len(h.sentRequests)
	rec3 := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob", "user:carol", "user:eve"}})
	if rec3.Code != http.StatusOK {
		t.Fatalf("retry 2 = %d %s, want 200", rec3.Code, rec3.Body.String())
	}
	var resp3 CreateReviewRequestsResponse
	_ = json.Unmarshal(rec3.Body.Bytes(), &resp3)
	if len(resp3.Requests) != 3 {
		t.Fatalf("retry 2 len = %d, want 3", len(resp3.Requests))
	}
	if resp3.Requests[0].Outcome != ReviewRequestOutcomeUnchanged || resp3.Requests[0].Notified {
		t.Errorf("retry 2 bob = %+v, want unchanged+not notified", resp3.Requests[0])
	}
	if resp3.Requests[1].Outcome != ReviewRequestOutcomeReminded || !resp3.Requests[1].Notified {
		t.Errorf("retry 2 carol = %+v, want reminded+notified", resp3.Requests[1])
	}
	if resp3.Requests[2].Outcome != ReviewRequestOutcomeUnchanged || resp3.Requests[2].Notified {
		t.Errorf("retry 2 eve = %+v, want unchanged+not notified", resp3.Requests[2])
	}
	if len(h.sentRequests) != noticesBeforeRetry2+1 {
		t.Errorf("notices sent on retry 2 = %d, want 1 (carol only)", len(h.sentRequests)-noticesBeforeRetry2)
	}
}

func TestGrantsNotifyReviewRefusedWhileGateOn(t *testing.T) {
	svc, h, _, artID := newReviewRequestFixture(t)

	// Gate ON: POST /grants with notify:true and permission:"review" -> 400 use_review_requests.
	rec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/grants", "user", "alice",
		GrantRequest{SubjectKind: SubjectPrincipal, SubjectRef: "user:carol", Permission: GrantReview, Notify: true})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), CodeUseReviewRequests) {
		t.Fatalf("grants notify+review (gate on) = %d %s, want 400 %s", rec.Code, rec.Body.String(), CodeUseReviewRequests)
	}

	// Gate ON: POST /grants with notify:true and permission:"read" still works.
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/grants", "user", "alice",
		GrantRequest{SubjectKind: SubjectPrincipal, SubjectRef: "user:carol", Permission: GrantRead, Notify: true})
	if rec.Code != http.StatusCreated {
		t.Fatalf("grants notify+read (gate on) = %d %s, want 201", rec.Code, rec.Body.String())
	}

	// Gate OFF: POST /grants with notify:true and permission:"review" works as before.
	h.gateOn = false
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/grants", "user", "alice",
		GrantRequest{SubjectKind: SubjectPrincipal, SubjectRef: "user:carol", Permission: GrantReview, Notify: true})
	if rec.Code != http.StatusOK {
		t.Fatalf("grants notify+review (gate off) = %d %s, want 200", rec.Code, rec.Body.String())
	}
}

func TestDeleteArtifactNotifiesOpenRequestReviewers(t *testing.T) {
	svc, h, _, artID := newReviewRequestFixture(t)

	rec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	h.inboxHints = nil

	delRec := doJSON(svc, http.MethodDelete, "/api/v1/artifacts/"+artID, "user", "alice", nil)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", delRec.Code, delRec.Body.String())
	}
	// Both owner (alice) and open-request reviewer (bob) receive inbox hints.
	got := map[string]bool{}
	for _, u := range h.inboxHints {
		got[u] = true
	}
	if !got["alice"] || !got["bob"] {
		t.Errorf("inboxHints on delete = %v, want alice and bob", h.inboxHints)
	}
}

func TestReapReviewRequestsTTLAndLostAccessAndLookupErrorFailSafe(t *testing.T) {
	_, h, st, artID := newReviewRequestFixture(t)
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ownerActor := &Actor{Kind: PrincipalKindUser, Ref: "alice", Authority: AuthorityOwner}

	// 1. Bob: open request created at t0; Carol: open request created at t0 (no review grant -> lost access).
	rrBob, _, err := st.CreateOrRemindReviewRequest(ctx, ReviewRequest{
		ArtifactID: artID, ReviewerKind: PrincipalKindUser, ReviewerRef: "bob",
		RequesterKind: PrincipalKindUser, RequesterRef: "alice", Access: ReviewRequestAccessExisting,
	}, 24*time.Hour, t0, ownerActor, nil)
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	rrCarol, _, err := st.CreateOrRemindReviewRequest(ctx, ReviewRequest{
		ArtifactID: artID, ReviewerKind: PrincipalKindUser, ReviewerRef: "carol",
		RequesterKind: PrincipalKindUser, RequesterRef: "alice", Access: ReviewRequestAccessExisting,
	}, 24*time.Hour, t0, ownerActor, nil)
	if err != nil {
		t.Fatalf("create carol: %v", err)
	}

	// Lookup error on Carol must NEVER close Carol's request (fail safe).
	h.resolveErrFor = map[string]error{"carol": errors.New("directory unavailable")}
	res, err := ReapReviewRequests(ctx, st, h, t0.Add(24*time.Hour), 50, nil)
	if err == nil || res.Errors == 0 || res.Expired != 0 {
		t.Fatalf("lookup-error reap = %+v, %v; want Errors>0 Expired=0", res, err)
	}
	if got, err := st.GetOpenReviewRequest(ctx, artID, PrincipalKindUser, "carol"); err != nil || got.ID != rrCarol.ID {
		t.Fatalf("carol must remain open after lookup error: got=%+v err=%v", got, err)
	}

	// Clear lookup error -> Carol (no review grant) expires via lost_access; Bob stays open.
	h.resolveErrFor = nil
	h.closedAcks = nil
	h.inboxHints = nil
	res, err = ReapReviewRequests(ctx, st, h, t0.Add(25*time.Hour), 50, nil)
	if err != nil || res.Expired != 1 {
		t.Fatalf("lost-access reap = %+v, %v; want Expired=1", res, err)
	}
	if got, _ := st.GetReviewRequest(ctx, artID, rrCarol.ID); got.State != ReviewRequestStateExpired {
		t.Errorf("carol state = %q, want expired", got.State)
	}
	if len(h.closedAcks) != 1 || h.closedAcks[0] != artID+":carol" {
		t.Errorf("closedAcks = %v, want [%s:carol]", h.closedAcks, artID)
	}
	if len(h.inboxHints) != 1 || h.inboxHints[0] != "carol" {
		t.Errorf("inboxHints = %v, want [carol]", h.inboxHints)
	}

	// Remind Bob at day 20 -> at day 35 (15 days after remind), Bob must NOT expire!
	if _, _, err := st.CreateOrRemindReviewRequest(ctx, ReviewRequest{
		ArtifactID: artID, ReviewerKind: PrincipalKindUser, ReviewerRef: "bob",
		RequesterKind: PrincipalKindUser, RequesterRef: "alice", Access: ReviewRequestAccessExisting,
	}, 24*time.Hour, t0.Add(20*24*time.Hour), ownerActor, nil); err != nil {
		t.Fatalf("remind bob: %v", err)
	}
	res, err = ReapReviewRequests(ctx, st, h, t0.Add(35*24*time.Hour), 50, nil)
	if err != nil || res.Expired != 0 {
		t.Fatalf("day 35 reap (15d after remind) = %+v, %v; want Expired=0", res, err)
	}

	// At day 51 (31 days after remind), Bob expires via TTL!
	res, err = ReapReviewRequests(ctx, st, h, t0.Add(51*24*time.Hour), 50, nil)
	if err != nil || res.Expired != 1 {
		t.Fatalf("day 51 reap = %+v, %v; want Expired=1", res, err)
	}
	if got, _ := st.GetReviewRequest(ctx, artID, rrBob.ID); got.State != ReviewRequestStateExpired {
		t.Errorf("bob state = %q, want expired", got.State)
	}

	// At day 150 (>90d after both resolved_at), both closed rows are purged.
	res, err = ReapReviewRequests(ctx, st, h, t0.Add(150*24*time.Hour), 50, nil)
	if err != nil || res.Purged != 2 {
		t.Fatalf("day 150 purge = %+v, %v; want Purged=2", res, err)
	}
}

func TestRaiseGrantOnRemindAndUnchanged(t *testing.T) {
	svc, _, st, artID := newReviewRequestFixture(t)
	ctx := context.Background()
	ownerActor := &Actor{Kind: PrincipalKindUser, Ref: "alice", Authority: AuthorityOwner}

	// 1. Create request for Bob (already has review grant -> access=existing, grant_id="").
	rec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}, Note: "initial"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var cResp CreateReviewRequestsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &cResp)
	if cResp.Requests[0].Access != ReviewRequestAccessExisting {
		t.Fatalf("initial access = %q, want existing", cResp.Requests[0].Access)
	}
	reqID := cResp.Requests[0].Request.ID

	revokeBob := func() {
		t.Helper()
		grants, err := st.ListGrants(ctx, artID)
		if err != nil {
			t.Fatalf("ListGrants: %v", err)
		}
		for _, g := range grants {
			if g.SubjectRef == "user:bob" {
				if err := st.DeleteGrant(ctx, artID, g.ID, ownerActor); err != nil {
					t.Fatalf("DeleteGrant: %v", err)
				}
			}
		}
	}

	// 2. Revoke Bob's review grant; admin Alice re-requests (reminded) ->
	// response and row both report access=raised and grant_id set.
	revokeBob()
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}, Note: "re-request"})
	if rec.Code != http.StatusOK {
		t.Fatalf("remind raise = %d %s", rec.Code, rec.Body.String())
	}
	var remResp CreateReviewRequestsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &remResp)
	if remResp.Requests[0].Outcome != ReviewRequestOutcomeReminded || remResp.Requests[0].Access != ReviewRequestAccessRaised {
		t.Fatalf("remind raise result = %+v, want reminded + access=raised", remResp.Requests[0])
	}
	row1, err := st.GetReviewRequest(ctx, artID, reqID)
	if err != nil || row1.Access != ReviewRequestAccessRaised || row1.GrantID == "" {
		t.Fatalf("stored row after remind raise = %+v, %v; want access=raised + non-empty GrantID", row1, err)
	}

	// 3. Revoke Bob's review grant again; admin Alice re-requests within 24h cooldown (unchanged) ->
	// response and row both report access=raised and new grant_id set.
	revokeBob()
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("unchanged raise = %d %s", rec.Code, rec.Body.String())
	}
	var unchResp CreateReviewRequestsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &unchResp)
	if unchResp.Requests[0].Outcome != ReviewRequestOutcomeUnchanged || unchResp.Requests[0].Access != ReviewRequestAccessRaised {
		t.Fatalf("unchanged raise result = %+v, want unchanged + access=raised", unchResp.Requests[0])
	}
	row2, err := st.GetReviewRequest(ctx, artID, reqID)
	if err != nil || row2.Access != ReviewRequestAccessRaised || row2.GrantID == "" || row2.GrantID == row1.GrantID {
		t.Fatalf("stored row after unchanged raise = %+v, %v; want access=raised + new GrantID", row2, err)
	}
}

type failOnNthCreateStore struct {
	Store
	failAt int
	calls  int
	err    error
}

func (s *failOnNthCreateStore) CreateOrRemindReviewRequest(ctx context.Context, req ReviewRequest, remindCooldown time.Duration, now time.Time, actor *Actor, grantOpt *ReviewRequestGrantOption) (ReviewRequest, string, error) {
	s.calls++
	if s.calls >= s.failAt {
		return ReviewRequest{}, "", s.err
	}
	return s.Store.CreateOrRemindReviewRequest(ctx, req, remindCooldown, now, actor, grantOpt)
}

func TestMidBatchArtifactErrorPreservesEarlierResults(t *testing.T) {
	svc, _, st, artID := newReviewRequestFixture(t)

	// Artifact-level ErrNotFound on the 2nd reviewer returns 200 with reviewer 1
	// created and reviewer 2 refused (unavailable).
	svc.SetStore(&failOnNthCreateStore{Store: st, failAt: 2, err: ErrNotFound})
	rec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob", "user:carol"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("mid-batch ErrNotFound = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var resp CreateReviewRequestsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Requests) != 2 ||
		resp.Requests[0].Outcome != ReviewRequestOutcomeCreated ||
		resp.Requests[1].Outcome != ReviewRequestOutcomeRefused || resp.Requests[1].Code != CodeUnavailable {
		t.Fatalf("mid-batch ErrNotFound results = %+v", resp.Requests)
	}

	// Artifact-level ErrNotFound on the 1st reviewer still returns 404.
	svc.SetStore(&failOnNthCreateStore{Store: st, failAt: 1, err: ErrNotFound})
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("first-reviewer ErrNotFound = %d %s, want 404", rec.Code, rec.Body.String())
	}
}

func TestDecisionLogReviewRequestRaisedGrant(t *testing.T) {
	svc, h, st, artID := newReviewRequestFixture(t)
	h.resolveMap["eve"] = &staticRecipientSubject{scopes: map[string]bool{"proj-1": true}}
	h.names["user:eve"] = "Eve"
	h.resolveMap["frank"] = &staticRecipientSubject{scopes: map[string]bool{"proj-1": true}}
	h.names["user:frank"] = "Frank"

	// Give Frank an existing read grant so raising him emits grant_changed.
	if gRec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/grants", "user", "alice",
		GrantRequest{SubjectKind: SubjectPrincipal, SubjectRef: "user:frank", Permission: GrantRead}); gRec.Code != http.StatusCreated {
		t.Fatalf("grant frank read = %d %s", gRec.Code, gRec.Body.String())
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	svc.SetLogger(func() *slog.Logger { return logger })
	takeGrantLogs := func() []map[string]any {
		t.Helper()
		raw := buf.String()
		buf.Reset()
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
			if line == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("decode log %q: %v", line, err)
			}
			if act, _ := m[logKeyAction].(string); act == logActionGrantCreated || act == logActionGrantChanged {
				out = append(out, m)
			}
		}
		return out
	}

	// 1. Owner raise (no existing grant for Carol) -> logs grant_created with authority=owner.
	rec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:carol"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("owner raise carol = %d %s", rec.Code, rec.Body.String())
	}
	entries := takeGrantLogs()
	checkEntry(t, "owner raise", one(t, "owner raise", entries), "INFO", map[string]any{
		logKeyAction:      logActionGrantCreated,
		logKeyAuthority:   AuthorityOwner,
		logKeyPrincipalID: "alice",
		logKeySubjectKind: SubjectPrincipal,
		logKeySubjectRef:  "user:carol",
		logKeyPermission:  GrantReview,
	})

	// 2. Raise over an existing read grant (Frank) -> logs grant_changed with authority=owner.
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:frank"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("owner raise frank = %d %s", rec.Code, rec.Body.String())
	}
	entries = takeGrantLogs()
	checkEntry(t, "raise over read", one(t, "raise over read", entries), "INFO", map[string]any{
		logKeyAction:      logActionGrantChanged,
		logKeyAuthority:   AuthorityOwner,
		logKeyPrincipalID: "alice",
		logKeySubjectKind: SubjectPrincipal,
		logKeySubjectRef:  "user:frank",
		logKeyPermission:  GrantReview,
	})

	// 3. Reviewer who already has review grant (Bob) -> logs nothing.
	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("existing review bob = %d %s", rec.Code, rec.Body.String())
	}
	if entries = takeGrantLogs(); len(entries) != 0 {
		t.Errorf("existing review grant should log nothing, got %+v", entries)
	}

	// 4. Steward raise on an agent-owned artifact -> logs grant_created with steward authority.
	pubReq := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts?name=agent.md&title=Agent+Doc&scope=proj-1", strings.NewReader("# Agent\n"))
	pubReq = withPrincipal(pubReq, principal{PrincipalKindAgent, "agent-1", "proj-1"})
	pubRec := httptest.NewRecorder()
	svc.ServeHTTP(pubRec, pubReq)
	if pubRec.Code != http.StatusCreated {
		t.Fatalf("agent publish = %d %s", pubRec.Code, pubRec.Body.String())
	}
	var agentPub ArtifactResponse
	_ = json.Unmarshal(pubRec.Body.Bytes(), &agentPub)
	agentArtID := agentPub.Artifact.ID

	pAlice := principal{PrincipalKindUser, "alice", ""}
	h.allowSteward(pAlice, "agent-1", "proj-1", StewardClauseDelegator)
	// Give Alice write access so she can publish (and request review) as steward.
	if gRec := doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+agentArtID+"/grants", "user", "alice",
		GrantRequest{SubjectKind: SubjectPrincipal, SubjectRef: "user:alice", Permission: GrantWrite}); gRec.Code != http.StatusCreated {
		t.Fatalf("grant alice write on agent art = %d %s", gRec.Code, gRec.Body.String())
	}
	_ = st
	takeGrantLogs()

	rec = doJSON(svc, http.MethodPost, "/api/v1/artifacts/"+agentArtID+"/review-requests", "user", "alice",
		CreateReviewRequestsBody{Reviewers: []string{"user:eve"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("steward raise eve = %d %s", rec.Code, rec.Body.String())
	}
	entries = takeGrantLogs()
	checkEntry(t, "steward raise", one(t, "steward raise", entries), "INFO", map[string]any{
		logKeyAction:      logActionGrantCreated,
		logKeyAuthority:   AuthorityStewardDelegator,
		logKeyPrincipalID: "alice",
		logKeySubjectKind: SubjectPrincipal,
		logKeySubjectRef:  "user:eve",
		logKeyPermission:  GrantReview,
		logKeyOwnerKind:   PrincipalKindAgent,
	})
}

type failingAuthorizeSubject struct {
	err error
}

func (s *failingAuthorizeSubject) Permits(_ context.Context, _ string, _ string) (bool, error) {
	return true, nil
}

func (s *failingAuthorizeSubject) Authorize(_ context.Context, _ string, _ string) (bool, error) {
	return false, s.err
}

func TestReapReviewRequestsPrincipalCanReviewErrorFailSafe(t *testing.T) {
	_, h, st, artID := newReviewRequestFixture(t)
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ownerActor := &Actor{Kind: PrincipalKindUser, Ref: "alice", Authority: AuthorityOwner}

	// Create an open request for Carol (no review grant, so a working Authorize
	// would report canReview=false and expire it).
	rr, _, err := st.CreateOrRemindReviewRequest(ctx, ReviewRequest{
		ArtifactID: artID, ReviewerKind: PrincipalKindUser, ReviewerRef: "carol",
		RequesterKind: PrincipalKindUser, RequesterRef: "alice", Access: ReviewRequestAccessExisting,
	}, 24*time.Hour, t0, ownerActor, nil)
	if err != nil {
		t.Fatalf("create carol: %v", err)
	}

	// ResolveRecipient succeeds and returns a subject whose Authorize errors,
	// so principalCanReview itself returns rerr != nil.
	h.resolveMap["carol"] = &failingAuthorizeSubject{err: errors.New("policy backend unavailable")}
	res, err := ReapReviewRequests(ctx, st, h, t0.Add(24*time.Hour), 50, nil)
	if err == nil {
		t.Fatalf("expected error from ReapReviewRequests when principalCanReview fails, got nil (res=%+v)", res)
	}
	if res.Errors == 0 || res.Expired != 0 {
		t.Fatalf("reap with principalCanReview error = %+v, want Errors>0 Expired=0", res)
	}
	got, gerr := st.GetOpenReviewRequest(ctx, artID, PrincipalKindUser, "carol")
	if gerr != nil || got.ID != rr.ID {
		t.Fatalf("request must stay open when principalCanReview errors: got=%+v err=%v", got, gerr)
	}
}
