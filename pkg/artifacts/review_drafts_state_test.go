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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// draftStateTestHost supports independent toggling of FeatureReviewDrafts and
// FeatureReviewRequests, plus inbox hint recording and recipient resolution.
type draftStateTestHost struct {
	*fakeHost
	mu          sync.Mutex
	draftsOn    bool
	requestsOn  bool
	inboxOn     bool
	inboxHints  []string
	resolveMap  map[string]RecipientSubject
	visibleUser map[string]bool
}

func newDraftStateTestHost(h *fakeHost) *draftStateTestHost {
	return &draftStateTestHost{
		fakeHost:    h,
		draftsOn:    true,
		requestsOn:  true,
		inboxOn:     true,
		resolveMap:  map[string]RecipientSubject{},
		visibleUser: map[string]bool{},
	}
}

func (h *draftStateTestHost) FeatureEnabled(_ context.Context, name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch name {
	case FeatureReviewDrafts:
		return h.draftsOn
	case FeatureReviewRequests:
		return h.requestsOn
	default:
		return false
	}
}

func (h *draftStateTestHost) setGates(drafts, requests bool) {
	h.mu.Lock()
	h.draftsOn = drafts
	h.requestsOn = requests
	h.mu.Unlock()
}

func (h *draftStateTestHost) setInboxHints(on bool) {
	h.mu.Lock()
	h.inboxOn = on
	h.mu.Unlock()
}

func (h *draftStateTestHost) InboxHintsActive(context.Context) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.inboxOn
}

func (h *draftStateTestHost) InboxChanged(_ context.Context, userID string) {
	h.mu.Lock()
	h.inboxHints = append(h.inboxHints, userID)
	h.mu.Unlock()
}

func (h *draftStateTestHost) takeHints() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := append([]string(nil), h.inboxHints...)
	h.inboxHints = nil
	return out
}

func (h *draftStateTestHost) ResolveRecipient(_ context.Context, kind, ref string) (RecipientSubject, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.resolveMap[PrincipalRef(kind, ref)], nil
}

func (h *draftStateTestHost) MayNotifyReviewer(_ context.Context, userID string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.visibleUser[userID], nil
}

func (h *draftStateTestHost) NotifyReviewRequest(_ context.Context, _ ReviewRequestNotice) error {
	return nil
}

func (h *draftStateTestHost) NotifyRequestDismissed(_ context.Context, _ ReviewRequestDismissNotice) error {
	return nil
}

func doDraftReq(svc *Service, p principal, method, target string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req = withPrincipal(req, p)
	rec := httptest.NewRecorder()
	svc.ServeHTTP(rec, req)
	return rec
}

// TestReviewDraftState_MyDraftAndDraftsInProgress verifies §13 item 13 and
// reviewState.myDraft / reviewState.draftsInProgress semantics:
//   - myDraft {base, savedAt} appears for the draft author when the drafts gate
//     is on (even if requests gate is off) and is omitted when the drafts gate
//     is off.
//   - draftsInProgress is visible to publishers only, counts ONLY drafts held
//     by open-request reviewers (never unsolicited drafters, never dismissed
//     requests), and never exposes draft content.
func TestReviewDraftState_MyDraftAndDraftsInProgress(t *testing.T) {
	f := newFixture(t, false)
	fh := newFakeHost()
	h := newDraftStateTestHost(fh)
	svc := NewService(h)
	svc.SetStore(f.store)
	svc.SetBlobStorage(f.blobs, "hub-test")

	owner := principal{kind: PrincipalKindUser, ref: "u-owner"}
	reqRev1 := principal{kind: PrincipalKindUser, ref: "u-req1"}
	reqRev2 := principal{kind: PrincipalKindUser, ref: "u-req2"}
	unsolicited := principal{kind: PrincipalKindUser, ref: "u-unsolicited"}
	reader := principal{kind: PrincipalKindUser, ref: "u-reader"}
	const scope = "proj-1"

	for _, p := range []principal{owner, reqRev1, reqRev2, unsolicited, reader} {
		fh.allow(p, scope, PermissionRead)
	}
	fh.allow(owner, scope, PermissionCreate, PermissionRequestReview)
	for _, rev := range []principal{reqRev1, reqRev2, unsolicited} {
		fh.allow(rev, scope, PermissionCreate)
		h.resolveMap[PrincipalRef(rev.kind, rev.ref)] = &staticRecipientSubject{scopes: map[string]bool{scope: true}}
		h.visibleUser[rev.ref] = true
	}

	// Owner publishes an artifact (v1).
	rec := doDraftReq(svc, owner, http.MethodPost, "/api/v1/artifacts?name=doc.md&scope="+scope, []byte("# Plan\n"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	artID := decodeInto[ArtifactResponse](t, rec).Artifact.ID

	// Grant review access to reqRev1, reqRev2, and unsolicited.
	ctx := context.Background()
	actor := &Actor{Kind: owner.kind, Ref: owner.ref, Authority: AuthorityOwner}
	for i, rev := range []principal{reqRev1, reqRev2, unsolicited} {
		if _, err := f.store.PutGrant(ctx, &Grant{
			ID: "g-" + rev.ref, ArtifactID: artID,
			SubjectKind: SubjectPrincipal, SubjectRef: PrincipalRef(rev.kind, rev.ref),
			Permission: GrantReview,
		}, 100, true, actor); err != nil {
			t.Fatalf("PutGrant %d: %v", i, err)
		}
	}

	// Owner requests review from reqRev1 and reqRev2 (NOT unsolicited).
	rec = doDraftReq(svc, owner, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests",
		[]byte(`{"reviewers":["user:u-req1","user:u-req2"],"note":"please check"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("request-review = %d: %s", rec.Code, rec.Body.String())
	}

	const secretBody = "TOP_SECRET_DRAFT_CONTENT_XYZ"
	const secretNote = "TOP_SECRET_NOTE_XYZ"
	putDraft := func(p principal) {
		body := `{"rev":0,"base":1,"entry":"doc.md","body":"` + secretBody + `","note":"` + secretNote + `"}`
		r := doDraftReq(svc, p, http.MethodPut, draftPath(artID), []byte(body))
		if r.Code != http.StatusOK {
			t.Fatalf("PUT draft as %s = %d: %s", p.ref, r.Code, r.Body.String())
		}
	}

	// 1. Unsolicited drafter saves a draft: publisher MUST see draftsInProgress == 0.
	putDraft(unsolicited)
	rec = doDraftReq(svc, owner, http.MethodGet, "/api/v1/artifacts/"+artID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET as owner = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secretBody) || strings.Contains(rec.Body.String(), secretNote) {
		t.Fatalf("publisher GET leaked draft content: %s", rec.Body.String())
	}
	ownerResp := decodeInto[ArtifactResponse](t, rec)
	if ownerResp.ReviewState == nil || ownerResp.ReviewState.DraftsInProgress != 0 {
		t.Fatalf("unsolicited drafter counted in draftsInProgress: %+v", ownerResp.ReviewState)
	}

	// Unsolicited drafter sees their own myDraft on both GET /{id} and GET /{id}/versions/1.
	for _, path := range []string{"/api/v1/artifacts/" + artID, "/api/v1/artifacts/" + artID + "/versions/1"} {
		rec = doDraftReq(svc, unsolicited, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s as unsolicited = %d", path, rec.Code)
		}
		got := decodeInto[ArtifactResponse](t, rec)
		if got.ReviewState == nil || got.ReviewState.MyDraft == nil || got.ReviewState.MyDraft.Base != 1 || got.ReviewState.MyDraft.SavedAt.IsZero() {
			t.Fatalf("GET %s myDraft = %+v, want base=1 and savedAt", path, got.ReviewState)
		}
		if got.ReviewState.DraftsInProgress != 0 {
			t.Fatalf("non-publisher got draftsInProgress=%d", got.ReviewState.DraftsInProgress)
		}
	}

	// 2. Open-request reviewers save drafts -> publisher sees draftsInProgress == 1, then 2.
	putDraft(reqRev1)
	rec = doDraftReq(svc, owner, http.MethodGet, "/api/v1/artifacts/"+artID, nil)
	if strings.Contains(rec.Body.String(), secretBody) || strings.Contains(rec.Body.String(), secretNote) {
		t.Fatalf("publisher GET leaked draft content: %s", rec.Body.String())
	}
	if got := decodeInto[ArtifactResponse](t, rec); got.ReviewState == nil || got.ReviewState.DraftsInProgress != 1 {
		t.Fatalf("draftsInProgress after 1 requested draft = %+v, want 1", got.ReviewState)
	}

	putDraft(reqRev2)
	rec = doDraftReq(svc, owner, http.MethodGet, "/api/v1/artifacts/"+artID, nil)
	if got := decodeInto[ArtifactResponse](t, rec); got.ReviewState == nil || got.ReviewState.DraftsInProgress != 2 {
		t.Fatalf("draftsInProgress after 2 requested drafts = %+v, want 2", got.ReviewState)
	}

	// Non-publishers (reader and reqRev1) never see draftsInProgress.
	for _, p := range []principal{reader, reqRev1} {
		rec = doDraftReq(svc, p, http.MethodGet, "/api/v1/artifacts/"+artID, nil)
		if got := decodeInto[ArtifactResponse](t, rec); got.ReviewState == nil || got.ReviewState.DraftsInProgress != 0 {
			t.Fatalf("non-publisher %s saw draftsInProgress=%+v", p.ref, got.ReviewState)
		}
	}

	// 3. reqRev1 dismisses their request while still holding their draft -> draftsInProgress drops to 1.
	rec = doDraftReq(svc, reqRev1, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-request/dismiss", []byte(`{"note":"busy"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss = %d: %s", rec.Code, rec.Body.String())
	}
	rec = doDraftReq(svc, owner, http.MethodGet, "/api/v1/artifacts/"+artID, nil)
	if got := decodeInto[ArtifactResponse](t, rec); got.ReviewState == nil || got.ReviewState.DraftsInProgress != 1 {
		t.Fatalf("draftsInProgress after dismiss = %+v, want 1", got.ReviewState)
	}

	// 4. Drafts gate ON, requests gate OFF -> myDraft is still populated.
	h.setGates(true, false)
	rec = doDraftReq(svc, reqRev2, http.MethodGet, "/api/v1/artifacts/"+artID, nil)
	if got := decodeInto[ArtifactResponse](t, rec); got.ReviewState == nil || got.ReviewState.MyDraft == nil || got.ReviewState.MyDraft.Base != 1 {
		t.Fatalf("drafts-only gate myDraft = %+v, want base=1", got.ReviewState)
	}

	// 5. Drafts gate OFF, requests gate ON -> myDraft and draftsInProgress are omitted.
	h.setGates(false, true)
	rec = doDraftReq(svc, reqRev2, http.MethodGet, "/api/v1/artifacts/"+artID, nil)
	if got := decodeInto[ArtifactResponse](t, rec); got.ReviewState == nil || got.ReviewState.MyDraft != nil {
		t.Fatalf("drafts gate off myDraft = %+v, want nil", got.ReviewState)
	}
	rec = doDraftReq(svc, owner, http.MethodGet, "/api/v1/artifacts/"+artID, nil)
	if got := decodeInto[ArtifactResponse](t, rec); got.ReviewState == nil || got.ReviewState.DraftsInProgress != 0 {
		t.Fatalf("drafts gate off draftsInProgress = %+v, want 0", got.ReviewState)
	}

	// 6. Both gates OFF -> reviewState still carries ungated reviewOnLatest, while gated fields are omitted.
	h.setGates(false, false)
	rec = doDraftReq(svc, owner, http.MethodGet, "/api/v1/artifacts/"+artID, nil)
	if got := decodeInto[ArtifactResponse](t, rec); got.ReviewState == nil || got.ReviewState.ReviewOnLatest || got.ReviewState.RequestedOfMe != nil || got.ReviewState.MyDraft != nil || got.ReviewState.DraftsInProgress != 0 {
		t.Fatalf("both gates off reviewState = %+v, want only reviewOnLatest=false", got.ReviewState)
	}
}

// TestReviewDraftListFiltersAndCursor verifies §13 item 5 for drafts=1 and
// is:draft / -is:draft in both global and project schemas, gate-off behavior,
// subset-of-authorized enforcement, and cursor binding.
func TestReviewDraftListFiltersAndCursor(t *testing.T) {
	f := newFixture(t, false)
	fh := newFakeHost()
	h := newDraftStateTestHost(fh)
	svc := NewService(h)
	svc.SetStore(f.store)
	svc.SetBlobStorage(f.blobs, "hub-test")

	owner := principal{kind: PrincipalKindUser, ref: "u-owner"}
	rev := principal{kind: PrincipalKindUser, ref: "u-rev"}
	const p1 = "proj-1"
	const p2 = "proj-2"

	for _, sc := range []string{p1, p2} {
		fh.allow(owner, sc, PermissionRead, PermissionCreate)
		fh.allow(rev, sc, PermissionRead, PermissionCreate)
	}

	publish := func(scope, name string) string {
		rec := doDraftReq(svc, owner, http.MethodPost, "/api/v1/artifacts?name="+name+"&scope="+scope, []byte("# "+name+"\n"))
		if rec.Code != http.StatusCreated {
			t.Fatalf("publish %s = %d: %s", name, rec.Code, rec.Body.String())
		}
		id := decodeInto[ArtifactResponse](t, rec).Artifact.ID
		_, err := f.store.PutGrant(context.Background(), &Grant{
			ID: "g-" + id, ArtifactID: id,
			SubjectKind: SubjectPrincipal, SubjectRef: PrincipalRef(rev.kind, rev.ref),
			Permission: GrantReview,
		}, 100, true, &Actor{Kind: owner.kind, Ref: owner.ref, Authority: AuthorityOwner})
		if err != nil {
			t.Fatalf("PutGrant %s: %v", id, err)
		}
		return id
	}

	a1 := publish(p1, "a1.md") // drafted
	a2 := publish(p1, "a2.md") // not drafted
	a3 := publish(p2, "a3.md") // drafted, later loses read access

	// Save drafts on a1 and a3 for rev.
	for _, id := range []string{a1, a3} {
		entry := "a1.md"
		if id == a3 {
			entry = "a3.md"
		}
		r := doDraftReq(svc, rev, http.MethodPut, draftPath(id),
			[]byte(`{"rev":0,"base":1,"entry":"`+entry+`","body":"draft","note":""}`))
		if r.Code != http.StatusOK {
			t.Fatalf("PUT draft %s = %d: %s", id, r.Code, r.Body.String())
		}
	}

	listIDs := func(path string) (map[string]ArtifactListItem, string) {
		r := doDraftReq(svc, rev, http.MethodGet, path, nil)
		if r.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, r.Code, r.Body.String())
		}
		resp := decodeInto[ArtifactListResponse](t, r)
		out := make(map[string]ArtifactListItem, len(resp.Artifacts))
		for _, it := range resp.Artifacts {
			out[it.ID] = it
		}
		return out, resp.NextCursor
	}

	// Global unfiltered: a1 & a3 have reviewInDraft=true; a2 has reviewInDraft=false.
	all, _ := listIDs("/api/v1/artifacts?mine=1")
	if len(all) != 3 || !all[a1].ReviewInDraft || all[a2].ReviewInDraft || !all[a3].ReviewInDraft {
		t.Fatalf("global unfiltered = %+v", all)
	}

	// Global drafts=1 and is:draft return {a1, a3}.
	for _, path := range []string{"/api/v1/artifacts?mine=1&drafts=1", "/api/v1/artifacts?mine=1&q=is:draft"} {
		got, _ := listIDs(path)
		if len(got) != 2 || !got[a1].ReviewInDraft || !got[a3].ReviewInDraft {
			t.Fatalf("GET %s = %v, want {a1, a3}", path, got)
		}
	}

	// Global -is:draft returns {a2}.
	neg, _ := listIDs("/api/v1/artifacts?mine=1&q=-is:draft")
	if len(neg) != 1 || neg[a2].ID != a2 {
		t.Fatalf("global -is:draft = %v, want {a2}", neg)
	}

	// Project-scoped is:draft and -is:draft.
	projDraft, _ := listIDs("/api/v1/artifacts?mine=1&scope=" + p1 + "&q=is:draft")
	if len(projDraft) != 1 || projDraft[a1].ID != a1 {
		t.Fatalf("project is:draft = %v, want {a1}", projDraft)
	}
	projNeg, _ := listIDs("/api/v1/artifacts?mine=1&scope=" + p1 + "&q=-is:draft")
	if len(projNeg) != 1 || projNeg[a2].ID != a2 {
		t.Fatalf("project -is:draft = %v, want {a2}", projNeg)
	}

	// Cursor binding includes drafts=1: cursor from drafts=1 is rejected without drafts=1.
	_, cursor := listIDs("/api/v1/artifacts?mine=1&drafts=1&limit=1")
	if cursor == "" {
		t.Fatal("expected non-empty nextCursor for limit=1 with 2 drafted artifacts")
	}
	badCur := doDraftReq(svc, rev, http.MethodGet, "/api/v1/artifacts?mine=1&limit=1&cursor="+cursor, nil)
	if badCur.Code != http.StatusBadRequest {
		t.Fatalf("reusing drafts=1 cursor without drafts=1 = %d, want 400", badCur.Code)
	}
	// Invalid drafts param returns 400.
	if r := doDraftReq(svc, rev, http.MethodGet, "/api/v1/artifacts?mine=1&drafts=maybe", nil); r.Code != http.StatusBadRequest {
		t.Fatalf("invalid drafts param = %d, want 400", r.Code)
	}

	// Subset-of-authorized: revoke rev's read/grant on a3 -> a3 disappears from drafts=1 & is:draft
	// even while its draft row still exists in the store.
	if err := f.store.DeleteGrant(context.Background(), a3, "g-"+a3, &Actor{Kind: owner.kind, Ref: owner.ref, Authority: AuthorityOwner}); err != nil {
		t.Fatalf("DeleteGrant a3: %v", err)
	}
	fh.deny(rev, p2, PermissionRead)
	for _, path := range []string{"/api/v1/artifacts?mine=1&drafts=1", "/api/v1/artifacts?mine=1&q=is:draft"} {
		got, _ := listIDs(path)
		if len(got) != 1 || got[a1].ID != a1 {
			t.Fatalf("after revoking a3 read, %s = %v, want {a1} only", path, got)
		}
	}

	// Gate off: drafts=1 and is:draft match nothing, -is:draft matches all authorized,
	// and reviewInDraft is false on every row.
	h.setGates(false, true)
	for _, path := range []string{"/api/v1/artifacts?mine=1&drafts=1", "/api/v1/artifacts?mine=1&q=is:draft", "/api/v1/artifacts?mine=1&scope=" + p1 + "&q=is:draft"} {
		got, _ := listIDs(path)
		if len(got) != 0 {
			t.Fatalf("gate off %s = %v, want empty", path, got)
		}
	}
	allOff, _ := listIDs("/api/v1/artifacts?mine=1")
	if len(allOff) != 2 || allOff[a1].ReviewInDraft || allOff[a2].ReviewInDraft {
		t.Fatalf("gate off unfiltered = %+v, want reviewInDraft=false on all rows", allOff)
	}
	negOff, _ := listIDs("/api/v1/artifacts?mine=1&q=-is:draft")
	if len(negOff) != 2 {
		t.Fatalf("gate off -is:draft = %d rows, want 2", len(negOff))
	}
}

// TestReviewDraftInboxHints verifies §13 item 7 for draft transitions:
//   - PUT creating a draft -> hints draft owner (user only);
//   - PUT updating (autosave) -> NO hint;
//   - DELETE removing a draft -> hints draft owner (user only);
//   - DELETE when no draft exists -> NO hint;
//   - TTL reaper -> hints each distinct reaped user owner;
//   - lost-access reaper -> hints each distinct reaped user owner;
//   - InboxHintsActive=false -> NO hints anywhere.
func TestReviewDraftInboxHints(t *testing.T) {
	f := newFixture(t, false)
	fh := newFakeHost()
	h := newDraftStateTestHost(fh)
	svc := NewService(h)
	svc.SetStore(f.store)
	svc.SetBlobStorage(f.blobs, "hub-test")

	userA := principal{kind: PrincipalKindUser, ref: "u-a"}
	userB := principal{kind: PrincipalKindUser, ref: "u-b"}
	agentC := principal{kind: PrincipalKindAgent, ref: "ag-c", home: "proj-1"}
	const scope = "proj-1"

	for _, p := range []principal{userA, userB, agentC} {
		fh.allow(p, scope, PermissionRead, PermissionCreate)
	}

	publish := func(name string) string {
		r := doDraftReq(svc, userA, http.MethodPost, "/api/v1/artifacts?name="+name+"&scope="+scope, []byte("# "+name+"\n"))
		if r.Code != http.StatusCreated {
			t.Fatalf("publish %s = %d: %s", name, r.Code, r.Body.String())
		}
		id := decodeInto[ArtifactResponse](t, r).Artifact.ID
		actor := &Actor{Kind: userA.kind, Ref: userA.ref, Authority: AuthorityOwner}
		for _, p := range []principal{userB, agentC} {
			if _, err := f.store.PutGrant(context.Background(), &Grant{
				ID: "g-" + id + "-" + p.ref, ArtifactID: id,
				SubjectKind: SubjectPrincipal, SubjectRef: PrincipalRef(p.kind, p.ref),
				Permission: GrantWrite,
			}, 100, true, actor); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}

	a1 := publish("d1.md")
	a2 := publish("d2.md")
	h.takeHints() // clear any publish hints

	// 1. Create draft (rev=0) -> hints u-a once.
	rec := doDraftReq(svc, userA, http.MethodPut, draftPath(a1),
		[]byte(`{"rev":0,"base":1,"entry":"d1.md","body":"v1","note":""}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("create draft = %d: %s", rec.Code, rec.Body.String())
	}
	if hints := h.takeHints(); len(hints) != 1 || hints[0] != "u-a" {
		t.Fatalf("create draft hints = %v, want [u-a]", hints)
	}

	// 2. Autosave update (rev=1) -> NO hint.
	rec = doDraftReq(svc, userA, http.MethodPut, draftPath(a1),
		[]byte(`{"rev":1,"base":1,"entry":"d1.md","body":"v2 autosave","note":""}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("update draft = %d: %s", rec.Code, rec.Body.String())
	}
	if hints := h.takeHints(); len(hints) != 0 {
		t.Fatalf("autosave update hints = %v, want empty", hints)
	}

	// 3. DELETE existing draft -> hints u-a once.
	rec = doDraftReq(svc, userA, http.MethodDelete, draftPath(a1), nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete draft = %d", rec.Code)
	}
	if hints := h.takeHints(); len(hints) != 1 || hints[0] != "u-a" {
		t.Fatalf("delete draft hints = %v, want [u-a]", hints)
	}

	// 4. DELETE when absent -> NO hint.
	rec = doDraftReq(svc, userA, http.MethodDelete, draftPath(a1), nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("repeat delete draft = %d", rec.Code)
	}
	if hints := h.takeHints(); len(hints) != 0 {
		t.Fatalf("absent delete hints = %v, want empty", hints)
	}

	// 5. Agent create/delete -> NO hint (user principals only).
	rec = doDraftReq(svc, agentC, http.MethodPut, draftPath(a1),
		[]byte(`{"rev":0,"base":1,"entry":"d1.md","body":"agent draft","note":""}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("agent create draft = %d", rec.Code)
	}
	rec = doDraftReq(svc, agentC, http.MethodDelete, draftPath(a1), nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("agent delete draft = %d", rec.Code)
	}
	if hints := h.takeHints(); len(hints) != 0 {
		t.Fatalf("agent draft hints = %v, want empty", hints)
	}

	// 6. TTL reaper -> hints each distinct reaped user once, never agents.
	ctx := context.Background()
	now := time.Now().UTC()
	oldTime := now.Add(-DraftTTL - time.Hour)
	for _, tc := range []struct {
		art  string
		p    principal
		file string
	}{
		{a1, userA, "d1.md"},
		{a2, userA, "d2.md"}, // second draft for same user -> deduplicated to 1 hint
		{a1, agentC, "d1.md"},
	} {
		if _, _, err := f.store.PutReviewDraft(ctx, ReviewDraft{
			ArtifactID: tc.art, PrincipalKind: tc.p.kind, PrincipalRef: tc.p.ref,
			BaseSeq: 1, EntryPath: tc.file, Body: "stale", Note: "",
		}, 0, oldTime, MaxDraftsPerPrincipal); err != nil {
			t.Fatal(err)
		}
	}
	reaped, err := ReapReviewDrafts(ctx, f.store, h, now, 100, nil)
	if err != nil || reaped != 3 {
		t.Fatalf("ReapReviewDrafts = %d, %v; want 3, nil", reaped, err)
	}
	if hints := h.takeHints(); len(hints) != 1 || hints[0] != "u-a" {
		t.Fatalf("TTL reaper hints = %v, want [u-a]", hints)
	}

	// 7. Lost-access reaper -> hints user whose draft was reaped, never agents.
	for _, tc := range []struct {
		art  string
		p    principal
		file string
	}{
		{a1, userB, "d1.md"},
		{a2, userB, "d2.md"},
		{a1, agentC, "d1.md"},
	} {
		if _, _, err := f.store.PutReviewDraft(ctx, ReviewDraft{
			ArtifactID: tc.art, PrincipalKind: tc.p.kind, PrincipalRef: tc.p.ref,
			BaseSeq: 1, EntryPath: tc.file, Body: "lost-access", Note: "",
		}, 0, now, MaxDraftsPerPrincipal); err != nil {
			t.Fatal(err)
		}
	}
	// resolveMap does not include userB or agentC, so ResolveRecipient returns nil (gone/no access).
	res, err := ReapLostAccessReviewDrafts(ctx, f.store, h, h, now, 100, nil)
	if err != nil || res.Deleted != 3 {
		t.Fatalf("ReapLostAccessReviewDrafts = %+v, %v; want Deleted=3", res, err)
	}
	if hints := h.takeHints(); len(hints) != 1 || hints[0] != "u-b" {
		t.Fatalf("lost-access reaper hints = %v, want [u-b]", hints)
	}

	// 8. InboxHintsActive=false -> NO hints on create, delete, or reapers.
	h.setInboxHints(false)
	rec = doDraftReq(svc, userA, http.MethodPut, draftPath(a1),
		[]byte(`{"rev":0,"base":1,"entry":"d1.md","body":"quiet","note":""}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("quiet create = %d", rec.Code)
	}
	rec = doDraftReq(svc, userA, http.MethodDelete, draftPath(a1), nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("quiet delete = %d", rec.Code)
	}
	if hints := h.takeHints(); len(hints) != 0 {
		t.Fatalf("hints when inactive = %v, want empty", hints)
	}
	// Ensure JSON marshaling of ReviewStateInfo omits myDraft/draftsInProgress when zero.
	raw, _ := json.Marshal(ReviewStateInfo{})
	if strings.Contains(string(raw), "myDraft") || strings.Contains(string(raw), "draftsInProgress") {
		t.Fatalf("zero ReviewStateInfo serialized draft fields: %s", raw)
	}
}

// TestReviewDraftPerCallerIsolation pins the per-caller principal predicate at
// every SQL site (my_draft select column, drafts=1 filter, positive/negative
// is:draft filter on global and project-scoped lists, and GetReviewDraftSummary
// on GET /{id} and GET /{id}/versions/{seq}). When one user saves a draft, a
// second principal who can read the artifact must never see reviewInDraft=true,
// never match drafts=1 or is:draft, still see the artifact under -is:draft, and
// always receive reviewState.myDraft == nil.
func TestReviewDraftPerCallerIsolation(t *testing.T) {
	f := newFixture(t, false)
	fh := newFakeHost()
	h := newDraftStateTestHost(fh)
	svc := NewService(h)
	svc.SetStore(f.store)
	svc.SetBlobStorage(f.blobs, "hub-test")

	owner := principal{kind: PrincipalKindUser, ref: "u-owner"}
	drafter := principal{kind: PrincipalKindUser, ref: "u-drafter"}
	const scope = "proj-1"

	fh.allow(owner, scope, PermissionRead, PermissionCreate)
	fh.allow(drafter, scope, PermissionRead, PermissionCreate)

	rec := doDraftReq(svc, owner, http.MethodPost, "/api/v1/artifacts?name=doc.md&scope="+scope, []byte("# Plan\n"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	artID := decodeInto[ArtifactResponse](t, rec).Artifact.ID

	if _, err := f.store.PutGrant(context.Background(), &Grant{
		ID: "g-drafter", ArtifactID: artID,
		SubjectKind: SubjectPrincipal, SubjectRef: PrincipalRef(drafter.kind, drafter.ref),
		Permission: GrantReview,
	}, 100, true, &Actor{Kind: owner.kind, Ref: owner.ref, Authority: AuthorityOwner}); err != nil {
		t.Fatalf("PutGrant: %v", err)
	}

	// drafter saves a draft; owner holds no draft.
	rec = doDraftReq(svc, drafter, http.MethodPut, draftPath(artID),
		[]byte(`{"rev":0,"base":1,"entry":"doc.md","body":"drafter private text","note":"drafter note"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT draft as drafter = %d: %s", rec.Code, rec.Body.String())
	}

	// 1. Unfiltered global list as owner: reviewInDraft must be false on every row.
	rec = doDraftReq(svc, owner, http.MethodGet, "/api/v1/artifacts?mine=1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unfiltered list as owner = %d: %s", rec.Code, rec.Body.String())
	}
	unfiltered := decodeInto[ArtifactListResponse](t, rec)
	if len(unfiltered.Artifacts) != 1 {
		t.Fatalf("unfiltered list len = %d, want 1", len(unfiltered.Artifacts))
	}
	for _, it := range unfiltered.Artifacts {
		if it.ReviewInDraft {
			t.Fatalf("owner saw reviewInDraft=true on %s drafted by another user", it.ID)
		}
	}

	// 2. drafts=1, global is:draft, and project-scoped is:draft all return 0 rows for owner.
	for _, path := range []string{
		"/api/v1/artifacts?mine=1&drafts=1",
		"/api/v1/artifacts?mine=1&q=is:draft",
		"/api/v1/artifacts?mine=1&scope=" + scope + "&q=is:draft",
	} {
		rec = doDraftReq(svc, owner, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s as owner = %d: %s", path, rec.Code, rec.Body.String())
		}
		got := decodeInto[ArtifactListResponse](t, rec)
		if len(got.Artifacts) != 0 {
			t.Fatalf("GET %s as owner returned %d rows (%+v), want 0", path, len(got.Artifacts), got.Artifacts)
		}
	}

	// 3. Project-scoped -is:draft still returns the artifact for owner.
	rec = doDraftReq(svc, owner, http.MethodGet, "/api/v1/artifacts?mine=1&scope="+scope+"&q=-is:draft", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("project -is:draft as owner = %d: %s", rec.Code, rec.Body.String())
	}
	negGot := decodeInto[ArtifactListResponse](t, rec)
	if len(negGot.Artifacts) != 1 || negGot.Artifacts[0].ID != artID {
		t.Fatalf("project -is:draft as owner = %+v, want [%s]", negGot.Artifacts, artID)
	}

	// 4. GET /{id} and GET /{id}/versions/1 as owner have reviewState.myDraft == nil.
	for _, path := range []string{
		"/api/v1/artifacts/" + artID,
		"/api/v1/artifacts/" + artID + "/versions/1",
	} {
		rec = doDraftReq(svc, owner, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s as owner = %d: %s", path, rec.Code, rec.Body.String())
		}
		got := decodeInto[ArtifactResponse](t, rec)
		if got.ReviewState == nil {
			t.Fatalf("GET %s as owner: reviewState is nil, want non-nil with myDraft=nil", path)
		}
		if got.ReviewState.MyDraft != nil {
			t.Fatalf("GET %s as owner leaked other user's myDraft: %+v", path, got.ReviewState.MyDraft)
		}
	}
}
