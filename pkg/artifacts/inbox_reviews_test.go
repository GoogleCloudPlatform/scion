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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type inboxRecordingHost struct {
	*fakeHost
	inboxMu      sync.Mutex
	hintsActive  bool
	inboxUsers   []string
	requestsOn   bool
	draftsOn     bool
	displayNames map[string]string
}

var (
	_ InboxNotifier  = (*inboxRecordingHost)(nil)
	_ FeatureGate    = (*inboxRecordingHost)(nil)
	_ PrincipalNamer = (*inboxRecordingHost)(nil)
)

func (h *inboxRecordingHost) FeatureEnabled(_ context.Context, name string) bool {
	h.inboxMu.Lock()
	defer h.inboxMu.Unlock()
	switch name {
	case FeatureReviewRequests:
		return h.requestsOn
	case FeatureReviewDrafts:
		return h.draftsOn
	default:
		return false
	}
}

func (h *inboxRecordingHost) PrincipalDisplayNames(_ context.Context, refs []string) map[string]string {
	h.inboxMu.Lock()
	defer h.inboxMu.Unlock()
	out := make(map[string]string, len(refs))
	for _, ref := range refs {
		if name, ok := h.displayNames[ref]; ok {
			out[ref] = name
		}
	}
	return out
}

func (h *inboxRecordingHost) InboxHintsActive(context.Context) bool {
	h.inboxMu.Lock()
	defer h.inboxMu.Unlock()
	return h.hintsActive
}

func (h *inboxRecordingHost) InboxChanged(_ context.Context, userID string) {
	h.inboxMu.Lock()
	defer h.inboxMu.Unlock()
	h.inboxUsers = append(h.inboxUsers, userID)
}

func (h *inboxRecordingHost) takeInboxUsers() []string {
	h.inboxMu.Lock()
	defer h.inboxMu.Unlock()
	out := append([]string(nil), h.inboxUsers...)
	h.inboxUsers = nil
	return out
}

func seedReviewArtifact(t *testing.T, st Store, scope, ownerKind, ownerRef, title, currentKind string, updatedAt time.Time, expiresAt *time.Time) string {
	t.Helper()
	id := uuid.NewString()
	a := &Artifact{
		ID: id, ScopeKind: ScopeKindProject, ScopeRef: scope,
		OwnerKind: ownerKind, OwnerRef: ownerRef, Title: title,
		CurrentSeq: 1, CreatedAt: updatedAt, UpdatedAt: updatedAt, ExpiresAt: expiresAt,
	}
	v := &Version{
		ID: uuid.NewString(), ArtifactID: id, Seq: 1, Kind: currentKind,
		EntryPath: "doc.md", TotalBytes: 4, FileCount: 1,
		CreatedByKind: ownerKind, CreatedByRef: ownerRef,
		CreatedAt: updatedAt, State: VersionStateReady,
	}
	g := Grant{
		ID: uuid.NewString(), ArtifactID: id, SubjectKind: SubjectScope, SubjectRef: scope,
		Permission: GrantRead, CreatedByRef: PrincipalRef(ownerKind, ownerRef), CreatedAt: updatedAt,
	}
	if err := st.CreatePublished(context.Background(), a, v, nil, []Grant{g}); err != nil {
		t.Fatalf("CreatePublished(%s): %v", title, err)
	}
	return id
}

func TestReviewStateSummary_CountsParityMetadataGatesAndCapping(t *testing.T) {
	f := newFixture(t, false)
	recHost := &inboxRecordingHost{
		fakeHost:   f.host,
		requestsOn: true,
		draftsOn:   true,
		displayNames: map[string]string{
			PrincipalRef(PrincipalKindUser, outside.ref): "Outside Author",
			PrincipalRef(PrincipalKindUser, userU.ref):   "Reviewer U",
		},
	}
	f.svc.host = recHost
	f.host.allow(userU, "project-2", PermissionRead, PermissionCreate)

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	idOld := seedReviewArtifact(t, f.store, "project-1", PrincipalKindUser, userU.ref, "old-review", VersionKindReview, base.Add(1*time.Minute), nil)
	idMid := seedReviewArtifact(t, f.store, "project-2", PrincipalKindUser, userU.ref, "mid-review", VersionKindReview, base.Add(2*time.Minute), nil)
	idNew := seedReviewArtifact(t, f.store, "project-1", PrincipalKindUser, userU.ref, "new-review", VersionKindReview, base.Add(3*time.Minute), nil)
	// Non-review owned artifact (excluded from ToAddress).
	seedReviewArtifact(t, f.store, "project-1", PrincipalKindUser, userU.ref, "publish-only", VersionKindPublish, base.Add(4*time.Minute), nil)
	// Expired review owned artifact (excluded).
	past := base.Add(-time.Minute)
	seedReviewArtifact(t, f.store, "project-1", PrincipalKindUser, userU.ref, "expired-review", VersionKindReview, base.Add(5*time.Minute), &past)
	// Another user's review artifact in a project userU can read (excluded from ToAddress because OwnedOnly).
	seedReviewArtifact(t, f.store, "project-1", PrincipalKindUser, outside.ref, "other-user-review", VersionKindReview, base.Add(6*time.Minute), nil)

	// Open review requests addressed to userU:
	// req1 in project-1 (readable via project membership) with a long multiline note.
	req1 := seedReviewArtifact(t, f.store, "project-1", PrincipalKindUser, outside.ref, "requested-1", VersionKindPublish, base.Add(10*time.Minute), nil)
	longNote := "Please check\n  section 2 " + strings.Repeat("x", 300)
	_, _, err := f.store.CreateOrRemindReviewRequest(context.Background(), ReviewRequest{
		ArtifactID:    req1,
		ReviewerKind:  PrincipalKindUser,
		ReviewerRef:   userU.ref,
		RequesterKind: PrincipalKindUser,
		RequesterRef:  outside.ref,
		Note:          longNote,
		Access:        ReviewRequestAccessExisting,
	}, 24*time.Hour, base.Add(11*time.Minute), &Actor{Kind: PrincipalKindUser, Ref: outside.ref, Authority: AuthorityOwner}, nil)
	if err != nil {
		t.Fatalf("create request 1: %v", err)
	}
	// req2 in project-2 (readable via project-2 membership).
	req2 := seedReviewArtifact(t, f.store, "project-2", PrincipalKindUser, outside.ref, "requested-2", VersionKindPublish, base.Add(12*time.Minute), nil)
	_, _, err = f.store.CreateOrRemindReviewRequest(context.Background(), ReviewRequest{
		ArtifactID:    req2,
		ReviewerKind:  PrincipalKindUser,
		ReviewerRef:   userU.ref,
		RequesterKind: PrincipalKindUser,
		RequesterRef:  outside.ref,
		Note:          "quick check",
		Access:        ReviewRequestAccessExisting,
	}, 24*time.Hour, base.Add(13*time.Minute), &Actor{Kind: PrincipalKindUser, Ref: outside.ref, Authority: AuthorityOwner}, nil)
	if err != nil {
		t.Fatalf("create request 2: %v", err)
	}
	// reqUnreadable in project-3 (userU cannot read project-3) -> must be excluded by canReadWith.
	reqUnreadable := seedReviewArtifact(t, f.store, "project-3", PrincipalKindUser, outside.ref, "requested-hidden", VersionKindPublish, base.Add(14*time.Minute), nil)
	_, _, err = f.store.CreateOrRemindReviewRequest(context.Background(), ReviewRequest{
		ArtifactID:    reqUnreadable,
		ReviewerKind:  PrincipalKindUser,
		ReviewerRef:   userU.ref,
		RequesterKind: PrincipalKindUser,
		RequesterRef:  outside.ref,
		Note:          "secret",
		Access:        ReviewRequestAccessExisting,
	}, 24*time.Hour, base.Add(15*time.Minute), &Actor{Kind: PrincipalKindUser, Ref: outside.ref, Authority: AuthorityOwner}, nil)
	if err != nil {
		t.Fatalf("create unreadable request: %v", err)
	}

	// Saved drafts held by userU: one on req1 (readable) and one on reqUnreadable (unreadable).
	for _, artID := range []string{req1, reqUnreadable} {
		if _, _, err := f.store.PutReviewDraft(context.Background(), ReviewDraft{
			ArtifactID:    artID,
			PrincipalKind: PrincipalKindUser,
			PrincipalRef:  userU.ref,
			BaseSeq:       1,
			EntryPath:     "doc.md",
			Body:          "draft body",
		}, 0, base.Add(16*time.Minute), MaxDraftsPerPrincipal); err != nil {
			t.Fatalf("PutReviewDraft(%s): %v", artID, err)
		}
	}

	ctx := context.WithValue(context.Background(), principalKey{}, userU)

	// Full summary with Drafts: true and Previews: 2.
	sum, err := f.svc.ReviewStateSummary(ctx, ReviewSummaryOptions{Previews: 2, Drafts: true})
	if err != nil {
		t.Fatalf("ReviewStateSummary: %v", err)
	}
	if sum.ToAddress.Count != 3 || sum.ToAddress.Capped {
		t.Fatalf("ToAddress = (%d, capped=%v), want (3, false)", sum.ToAddress.Count, sum.ToAddress.Capped)
	}
	if len(sum.ToAddress.Items) != 2 || sum.ToAddress.Items[0].ID != idNew || sum.ToAddress.Items[1].ID != idMid {
		t.Fatalf("ToAddress.Items = %v, want [%s, %s]", sum.ToAddress.Items, idNew, idMid)
	}
	for _, it := range sum.ToAddress.Items {
		if it.Request != nil {
			t.Fatalf("ToAddress item unexpectedly has Request: %+v", it.Request)
		}
	}

	if sum.Requested.Count != 2 || sum.Requested.Capped {
		t.Fatalf("Requested = (%d, capped=%v), want (2, false)", sum.Requested.Count, sum.Requested.Capped)
	}
	if len(sum.Requested.Items) != 2 || sum.Requested.Items[0].ID != req2 || sum.Requested.Items[1].ID != req1 {
		t.Fatalf("Requested.Items = %v, want [%s, %s]", sum.Requested.Items, req2, req1)
	}
	if sum.Requested.Items[0].Request == nil || sum.Requested.Items[0].Request.Requester.DisplayName != "Outside Author" || sum.Requested.Items[0].Request.RequestedSeq != 1 || sum.Requested.Items[0].Request.Note != "quick check" {
		t.Fatalf("Requested.Items[0].Request = %+v", sum.Requested.Items[0].Request)
	}
	if sum.Requested.Items[1].Request == nil || strings.Contains(sum.Requested.Items[1].Request.Note, "\n") || len([]rune(sum.Requested.Items[1].Request.Note)) > reviewSummaryNoteMaxRunes {
		t.Fatalf("Requested.Items[1].Request.Note not cleaned/excerpted: %+v", sum.Requested.Items[1].Request)
	}

	if sum.Drafts == nil || sum.Drafts.Count != 1 || sum.Drafts.Capped || len(sum.Drafts.Items) != 1 || sum.Drafts.Items[0].ID != req1 {
		t.Fatalf("Drafts = %+v, want count=1 item=[%s]", sum.Drafts, req1)
	}
	if sum.Drafts.Items[0].Request != nil {
		t.Fatalf("Drafts item unexpectedly has Request: %+v", sum.Drafts.Items[0].Request)
	}

	// Parity with filtered HTTP lists, and handleList never sets ArtifactListItem.Request.
	listHTTP := func(path string) ArtifactListResponse {
		t.Helper()
		rec := f.do(&userU, http.MethodGet, path, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body.String())
		}
		var resp ArtifactListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	toAddrList := listHTTP("/api/v1/artifacts?mine=1&review_on_latest=1&owner=me")
	reqList := listHTTP("/api/v1/artifacts?mine=1&requested=1")
	draftList := listHTTP("/api/v1/artifacts?mine=1&drafts=1")
	if len(toAddrList.Artifacts) != sum.ToAddress.Count {
		t.Fatalf("ToAddress parity mismatch: list=%d summary=%d", len(toAddrList.Artifacts), sum.ToAddress.Count)
	}
	if len(reqList.Artifacts) != sum.Requested.Count {
		t.Fatalf("Requested parity mismatch: list=%d summary=%d", len(reqList.Artifacts), sum.Requested.Count)
	}
	for _, it := range reqList.Artifacts {
		if it.Request != nil {
			t.Fatalf("handleList must never populate ArtifactListItem.Request, got %+v", it.Request)
		}
	}
	if len(draftList.Artifacts) != sum.Drafts.Count {
		t.Fatalf("Drafts parity mismatch: list=%d summary=%d", len(draftList.Artifacts), sum.Drafts.Count)
	}

	// Drafts: false omits Drafts (nil); Previews: 0 returns counts with empty Items.
	sumNoDrafts, err := f.svc.ReviewStateSummary(ctx, ReviewSummaryOptions{Previews: 0, Drafts: false})
	if err != nil {
		t.Fatalf("ReviewStateSummary(0, false): %v", err)
	}
	if sumNoDrafts.Drafts != nil {
		t.Fatalf("expected Drafts=nil when opts.Drafts=false, got %+v", sumNoDrafts.Drafts)
	}
	if sumNoDrafts.ToAddress.Count != 3 || len(sumNoDrafts.ToAddress.Items) != 0 {
		t.Fatalf("ToAddress(previews=0) = %+v", sumNoDrafts.ToAddress)
	}
	if sumNoDrafts.Requested.Count != 2 || len(sumNoDrafts.Requested.Items) != 0 {
		t.Fatalf("Requested(previews=0) = %+v", sumNoDrafts.Requested)
	}

	// Requests gate off -> Requested is empty (0, false, []).
	recHost.requestsOn = false
	sumGateOff, err := f.svc.ReviewStateSummary(ctx, ReviewSummaryOptions{Previews: 5, Drafts: true})
	if err != nil {
		t.Fatalf("ReviewStateSummary(requests gate off): %v", err)
	}
	if sumGateOff.Requested.Count != 0 || sumGateOff.Requested.Capped || len(sumGateOff.Requested.Items) != 0 {
		t.Fatalf("Requested with gate off = %+v, want empty", sumGateOff.Requested)
	}
	recHost.requestsOn = true

	// Token boundary denial on project-2 excludes idMid and req2.
	f.host.deny(userU, "project-2", PermissionRead)
	sumDenied, err := f.svc.ReviewStateSummary(ctx, ReviewSummaryOptions{Previews: 5, Drafts: true})
	if err != nil {
		t.Fatalf("ReviewStateSummary(denied): %v", err)
	}
	if sumDenied.ToAddress.Count != 2 || sumDenied.ToAddress.Capped {
		t.Fatalf("ToAddress(denied) = (%d, %v), want (2, false)", sumDenied.ToAddress.Count, sumDenied.ToAddress.Capped)
	}
	if len(sumDenied.ToAddress.Items) != 2 || sumDenied.ToAddress.Items[0].ID != idNew || sumDenied.ToAddress.Items[1].ID != idOld {
		t.Fatalf("ToAddress(denied).Items = %v, want [%s, %s]", sumDenied.ToAddress.Items, idNew, idOld)
	}
	if sumDenied.Requested.Count != 1 || len(sumDenied.Requested.Items) != 1 || sumDenied.Requested.Items[0].ID != req1 {
		t.Fatalf("Requested(denied) = %+v, want count=1 [%s]", sumDenied.Requested, req1)
	}

	// Lower maxListScan to 2 to verify Capped=true when scan budget runs out.
	origScan := maxListScan
	maxListScan = 2
	t.Cleanup(func() { maxListScan = origScan })
	sumCap, err := f.svc.ReviewStateSummary(ctx, ReviewSummaryOptions{Previews: 5, Drafts: true})
	if err != nil {
		t.Fatalf("ReviewStateSummary(capped): %v", err)
	}
	if !sumCap.ToAddress.Capped {
		t.Fatalf("expected ToAddress.Capped=true when scan budget reached; got %+v", sumCap.ToAddress)
	}

	// Unauthenticated caller returns empty without error.
	sumAnon, err := f.svc.ReviewStateSummary(context.Background(), ReviewSummaryOptions{Previews: 3, Drafts: true})
	if err != nil || sumAnon.ToAddress.Count != 0 || sumAnon.Requested.Count != 0 || sumAnon.Drafts == nil || sumAnon.Drafts.Count != 0 {
		t.Fatalf("unauthenticated: got (%+v, %v)", sumAnon, err)
	}

	// Unconfigured backend returns ErrBackendUnavailable.
	unconfigured := NewService(f.host)
	if _, err := unconfigured.ReviewStateSummary(ctx, ReviewSummaryOptions{Previews: 3}); err != ErrBackendUnavailable {
		t.Fatalf("expected ErrBackendUnavailable, got %v", err)
	}
}

func TestStoreGetArtifacts(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	now := time.Now().UTC()

	// Empty input returns empty map without error.
	empty, err := f.store.GetArtifacts(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty GetArtifacts: got (%v, %v), want empty map, nil", empty, err)
	}

	// Exceeding MaxGrantsForIDs returns an error.
	if _, err := f.store.GetArtifacts(ctx, make([]string, MaxGrantsForIDs+1)); err == nil {
		t.Fatalf("expected error for >MaxGrantsForIDs ids")
	}

	live1 := seedReviewArtifact(t, f.store, "project-1", PrincipalKindUser, userU.ref, "live-1", VersionKindPublish, now, nil)
	live2 := seedReviewArtifact(t, f.store, "project-1", PrincipalKindUser, userU.ref, "live-2", VersionKindReview, now, nil)
	deleted := seedReviewArtifact(t, f.store, "project-1", PrincipalKindUser, userU.ref, "deleted-1", VersionKindPublish, now, nil)
	if _, err := f.store.DeleteArtifact(ctx, deleted, DeleteOptions{
		Actor:    &Actor{Kind: PrincipalKindUser, Ref: userU.ref, Authority: AuthorityOwner},
		ScopeRef: "project-1",
	}); err != nil {
		t.Fatalf("DeleteArtifact: %v", err)
	}

	got, err := f.store.GetArtifacts(ctx, []string{live1, live2, deleted, uuid.NewString()})
	if err != nil {
		t.Fatalf("GetArtifacts: %v", err)
	}
	if len(got) != 2 || got[live1] == nil || got[live2] == nil || got[deleted] != nil {
		t.Fatalf("GetArtifacts returned %v, want only %s and %s", got, live1, live2)
	}
	if got[live1].Title != "live-1" || got[live2].Title != "live-2" {
		t.Fatalf("unexpected titles: %q, %q", got[live1].Title, got[live2].Title)
	}
}

func TestReadableArtifacts(t *testing.T) {
	f := newFixture(t, false)
	now := time.Now().UTC()

	ownedID := seedReviewArtifact(t, f.store, "project-1", PrincipalKindUser, userU.ref, "owned-doc", VersionKindPublish, now, nil)
	projID := seedReviewArtifact(t, f.store, "project-1", PrincipalKindUser, outside.ref, "proj-doc", VersionKindPublish, now, nil)
	grantedID := seedReviewArtifact(t, f.store, "project-2", PrincipalKindUser, outside.ref, "granted-doc", VersionKindPublish, now, nil)
	if _, err := f.store.PutGrant(context.Background(), &Grant{
		ID: uuid.NewString(), ArtifactID: grantedID, SubjectKind: SubjectPrincipal,
		SubjectRef: PrincipalRef(PrincipalKindUser, userU.ref), Permission: GrantReview,
		CreatedByRef: PrincipalRef(PrincipalKindUser, outside.ref), CreatedAt: now,
	}, MaxGrantsPerArtifact, false, &Actor{Kind: PrincipalKindUser, Ref: outside.ref, Authority: AuthorityOwner}); err != nil {
		t.Fatalf("PutGrant: %v", err)
	}
	hiddenID := seedReviewArtifact(t, f.store, "project-2", PrincipalKindUser, outside.ref, "hidden-doc", VersionKindPublish, now, nil)
	past := now.Add(-time.Minute)
	expiredID := seedReviewArtifact(t, f.store, "project-1", PrincipalKindUser, outside.ref, "expired-doc", VersionKindPublish, now, &past)

	ctx := context.WithValue(context.Background(), principalKey{}, userU)
	got, capped, err := f.svc.ReadableArtifacts(ctx, []string{ownedID, projID, grantedID, hiddenID, expiredID, uuid.NewString(), "not-a-uuid"})
	if err != nil {
		t.Fatalf("ReadableArtifacts: %v", err)
	}
	if capped {
		t.Fatalf("expected capped=false")
	}
	if len(got) != 3 {
		t.Fatalf("got %d readable artifacts (%v), want 3", len(got), got)
	}
	for id, wantTitle := range map[string]string{ownedID: "owned-doc", projID: "proj-doc", grantedID: "granted-doc"} {
		info, ok := got[id]
		if !ok || info.Title != wantTitle {
			t.Fatalf("artifact %s: got (%v, %+v), want title %q", id, ok, info, wantTitle)
		}
	}

	// Lower maxListScan to 2 to verify truncation sets capped=true and keeps newest (first) 2.
	origScan := maxListScan
	maxListScan = 2
	t.Cleanup(func() { maxListScan = origScan })
	gotCap, capped, err := f.svc.ReadableArtifacts(ctx, []string{ownedID, projID, grantedID})
	if err != nil {
		t.Fatalf("ReadableArtifacts(capped): %v", err)
	}
	if !capped || len(gotCap) != 2 || gotCap[ownedID].ID != ownedID || gotCap[projID].ID != projID {
		t.Fatalf("got (%v, capped=%v), want 2 items (%s, %s) with capped=true", gotCap, capped, ownedID, projID)
	}
}

func TestInboxNotifier_PublishDeleteAndGrantRevoke(t *testing.T) {
	f := newFixture(t, false)
	recHost := &inboxRecordingHost{fakeHost: f.host, hintsActive: true}
	f.svc.host = recHost
	f.host.allow(outside, "project-1", PermissionRead)

	// Helper to append + upload + finalize a version on artifactID as caller.
	finalizeVersion := func(caller principal, artifactID, kind string, content []byte, base int) int {
		t.Helper()
		manifest, _ := json.Marshal(CreateVersionRequest{
			Kind:  kind,
			Entry: "doc.md",
			Files: []ManifestFile{{Path: "doc.md", Size: int64(len(content)), SHA256: sha(content)}},
		})
		rec := f.do(&caller, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/versions", manifest, map[string]string{"Content-Type": "application/json"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("create version (%s): %d %s", kind, rec.Code, rec.Body.String())
		}
		var vresp PendingVersionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &vresp); err != nil {
			t.Fatal(err)
		}
		seq := vresp.Version.Seq
		putRec := f.do(&caller, http.MethodPut, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/files/doc.md", artifactID, seq), content, map[string]string{"Content-Type": "text/markdown"})
		if putRec.Code != http.StatusNoContent {
			t.Fatalf("upload file: %d %s", putRec.Code, putRec.Body.String())
		}
		var finBody []byte
		if base > 0 {
			finBody, _ = json.Marshal(FinalizeRequest{Base: base})
		}
		finRec := f.do(&caller, http.MethodPost, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/finalize", artifactID, seq), finBody, map[string]string{"Content-Type": "application/json"})
		if finRec.Code != http.StatusOK {
			t.Fatalf("finalize (%s): %d %s", kind, finRec.Code, finRec.Body.String())
		}
		return seq
	}

	// 1. User publishes v1, then publishes v2 (prior was publish, NOT review) -> NO inbox hint.
	art := f.publish(userU, "doc.md", []byte("v1\n"), "scope=project-1")
	finalizeVersion(userU, art.Artifact.ID, VersionKindPublish, []byte("v2\n"), 0)
	if got := recHost.takeInboxUsers(); len(got) != 0 {
		t.Fatalf("publish after publish should not fire InboxChanged, got %v", got)
	}

	// 2. Grant review to outside, then outside submits a review (v3) -> NO InboxChanged (review submission uses notification).
	grantBody, _ := json.Marshal(GrantRequest{
		SubjectKind: SubjectPrincipal,
		SubjectRef:  PrincipalRef(PrincipalKindUser, outside.ref),
		Permission:  GrantReview,
	})
	gRec := f.do(&userU, http.MethodPost, "/api/v1/artifacts/"+art.Artifact.ID+"/grants", grantBody, map[string]string{"Content-Type": "application/json"})
	if gRec.Code != http.StatusCreated {
		t.Fatalf("grant review: %d %s", gRec.Code, gRec.Body.String())
	}
	var gResp GrantResponse
	if err := json.Unmarshal(gRec.Body.Bytes(), &gResp); err != nil {
		t.Fatal(err)
	}

	finalizeVersion(outside, art.Artifact.ID, VersionKindReview, []byte("v2{>>comment<<}\n"), 2)
	if got := recHost.takeInboxUsers(); len(got) != 0 {
		t.Fatalf("review finalize should not fire InboxChanged, got %v", got)
	}

	// 3. Owner publishes v4 (prior current version was review v3) -> fires InboxChanged for owner only!
	finalizeVersion(userU, art.Artifact.ID, VersionKindPublish, []byte("v4 addressed\n"), 0)
	if got := recHost.takeInboxUsers(); len(got) != 1 || got[0] != userU.ref {
		t.Fatalf("review-clearing publish: got %v, want [%s]", got, userU.ref)
	}

	// 3b. When InboxHintsActive is false, review-clearing publish skips the prior-version check and does not fire InboxChanged.
	finalizeVersion(outside, art.Artifact.ID, VersionKindReview, []byte("v4 addressed{>>again<<}\n"), 4)
	recHost.hintsActive = false
	finalizeVersion(userU, art.Artifact.ID, VersionKindPublish, []byte("v6 addressed\n"), 0)
	if got := recHost.takeInboxUsers(); len(got) != 0 {
		t.Fatalf("hintsActive=false review-clearing publish should not fire InboxChanged, got %v", got)
	}
	recHost.hintsActive = true

	// 4. Revoke outside's review grant -> fires InboxChanged for subject user (outside) only!
	delGrantRec := f.do(&userU, http.MethodDelete, "/api/v1/artifacts/"+art.Artifact.ID+"/grants/"+gResp.Grant.ID, nil, nil)
	if delGrantRec.Code != http.StatusNoContent {
		t.Fatalf("delete grant: %d %s", delGrantRec.Code, delGrantRec.Body.String())
	}
	if got := recHost.takeInboxUsers(); len(got) != 1 || got[0] != outside.ref {
		t.Fatalf("grant revocation: got %v, want [%s]", got, outside.ref)
	}

	// 4b. When InboxHintsActive is false, revoking a grant does not fire InboxChanged
	// even when decision logging is active (so grantForLog populates removed).
	gRec2 := f.do(&userU, http.MethodPost, "/api/v1/artifacts/"+art.Artifact.ID+"/grants", grantBody, map[string]string{"Content-Type": "application/json"})
	if gRec2.Code != http.StatusCreated {
		t.Fatalf("re-grant review: %d %s", gRec2.Code, gRec2.Body.String())
	}
	var gResp2 GrantResponse
	if err := json.Unmarshal(gRec2.Body.Bytes(), &gResp2); err != nil {
		t.Fatal(err)
	}
	infoLogger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
	f.svc.SetLogger(func() *slog.Logger { return infoLogger })
	recHost.hintsActive = false
	delGrantRec2 := f.do(&userU, http.MethodDelete, "/api/v1/artifacts/"+art.Artifact.ID+"/grants/"+gResp2.Grant.ID, nil, nil)
	if delGrantRec2.Code != http.StatusNoContent {
		t.Fatalf("delete grant (hints off): %d %s", delGrantRec2.Code, delGrantRec2.Body.String())
	}
	if got := recHost.takeInboxUsers(); len(got) != 0 {
		t.Fatalf("hintsActive=false grant revocation should not fire InboxChanged, got %v", got)
	}
	f.svc.SetLogger(nil)
	recHost.hintsActive = true

	// 5. Delete user-owned artifact -> fires InboxChanged for owner only!
	delRec := f.do(&userU, http.MethodDelete, "/api/v1/artifacts/"+art.Artifact.ID, nil, nil)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("delete artifact: %d %s", delRec.Code, delRec.Body.String())
	}
	if got := recHost.takeInboxUsers(); len(got) != 1 || got[0] != userU.ref {
		t.Fatalf("delete user artifact: got %v, want [%s]", got, userU.ref)
	}

	// 5b. When InboxHintsActive is false, deleting a user-owned artifact does not fire InboxChanged.
	art2 := f.publish(userU, "doc2.md", []byte("v1\n"), "scope=project-1")
	recHost.hintsActive = false
	delRec2 := f.do(&userU, http.MethodDelete, "/api/v1/artifacts/"+art2.Artifact.ID, nil, nil)
	if delRec2.Code != http.StatusNoContent {
		t.Fatalf("delete artifact (hints off): %d %s", delRec2.Code, delRec2.Body.String())
	}
	if got := recHost.takeInboxUsers(); len(got) != 0 {
		t.Fatalf("hintsActive=false delete user artifact should not fire InboxChanged, got %v", got)
	}
	recHost.hintsActive = true

	// 6. Delete agent-owned artifact -> must NOT fire InboxChanged (not a user owner).
	agentArt := f.publish(agentA, "agent.md", []byte("agent doc\n"), "")
	f.host.allowAdmin(userU, "project-1")
	delAgentRec := f.do(&userU, http.MethodDelete, "/api/v1/artifacts/"+agentArt.Artifact.ID, nil, nil)
	if delAgentRec.Code != http.StatusNoContent {
		t.Fatalf("delete agent artifact: %d %s", delAgentRec.Code, delAgentRec.Body.String())
	}
	if got := recHost.takeInboxUsers(); len(got) != 0 {
		t.Fatalf("delete agent artifact should not fire InboxChanged, got %v", got)
	}
}

func TestInboxNotifier_SoftDeleteHintsOwnerReviewersAndDraftHolders(t *testing.T) {
	f := newFixture(t, false)
	recHost := &inboxRecordingHost{
		fakeHost:    f.host,
		hintsActive: true,
		requestsOn:  true,
		draftsOn:    true,
	}
	f.svc.host = recHost

	drafterOnly := principal{kind: PrincipalKindUser, ref: "user-drafter-only"}
	both := principal{kind: PrincipalKindUser, ref: "user-both"}
	f.host.allow(outside, "project-1", PermissionRead)
	f.host.allow(drafterOnly, "project-1", PermissionRead)
	f.host.allow(both, "project-1", PermissionRead)

	art := f.publish(userU, "doc.md", []byte("v1\n"), "scope=project-1")
	now := time.Now().UTC()
	// Open requests for outside and both.
	for _, rev := range []principal{outside, both} {
		if _, _, err := f.store.CreateOrRemindReviewRequest(context.Background(), ReviewRequest{
			ArtifactID:    art.Artifact.ID,
			ReviewerKind:  PrincipalKindUser,
			ReviewerRef:   rev.ref,
			RequesterKind: PrincipalKindUser,
			RequesterRef:  userU.ref,
			Access:        ReviewRequestAccessExisting,
		}, 24*time.Hour, now, &Actor{Kind: PrincipalKindUser, Ref: userU.ref, Authority: AuthorityOwner}, nil); err != nil {
			t.Fatalf("create request for %s: %v", rev.ref, err)
		}
	}
	// Saved drafts for drafterOnly, both (overlap with open request), and agentA (non-user, must not be hinted).
	for _, dp := range []principal{drafterOnly, both, agentA} {
		if _, _, err := f.store.PutReviewDraft(context.Background(), ReviewDraft{
			ArtifactID:    art.Artifact.ID,
			PrincipalKind: dp.kind,
			PrincipalRef:  dp.ref,
			BaseSeq:       1,
			EntryPath:     "doc.md",
			Body:          "wip",
		}, 0, now, MaxDraftsPerPrincipal); err != nil {
			t.Fatalf("put draft for %s: %v", dp.ref, err)
		}
	}

	delRec := f.do(&userU, http.MethodDelete, "/api/v1/artifacts/"+art.Artifact.ID, nil, nil)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("delete artifact: %d %s", delRec.Code, delRec.Body.String())
	}

	got := recHost.takeInboxUsers()
	sort.Strings(got)
	want := []string{drafterOnly.ref, both.ref, outside.ref, userU.ref}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("soft-delete hints = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("soft-delete hints = %v, want %v", got, want)
		}
	}
}
