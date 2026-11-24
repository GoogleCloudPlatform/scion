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
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type inboxRecordingHost struct {
	*fakeHost
	inboxMu     sync.Mutex
	hintsActive bool
	inboxUsers  []string
}

var _ InboxNotifier = (*inboxRecordingHost)(nil)

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

func TestReviewPendingSummary_BasicOrderingAndCapping(t *testing.T) {
	f := newFixture(t, false)
	f.host.allow(userU, "project-2", PermissionRead, PermissionCreate)

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	idOld := seedReviewArtifact(t, f.store, "project-1", PrincipalKindUser, userU.ref, "old-review", VersionKindReview, base.Add(1*time.Minute), nil)
	idMid := seedReviewArtifact(t, f.store, "project-2", PrincipalKindUser, userU.ref, "mid-review", VersionKindReview, base.Add(2*time.Minute), nil)
	idNew := seedReviewArtifact(t, f.store, "project-1", PrincipalKindUser, userU.ref, "new-review", VersionKindReview, base.Add(3*time.Minute), nil)
	// Non-review owned artifact (excluded).
	seedReviewArtifact(t, f.store, "project-1", PrincipalKindUser, userU.ref, "publish-only", VersionKindPublish, base.Add(4*time.Minute), nil)
	// Expired review owned artifact (excluded).
	past := base.Add(-time.Minute)
	seedReviewArtifact(t, f.store, "project-1", PrincipalKindUser, userU.ref, "expired-review", VersionKindReview, base.Add(5*time.Minute), &past)
	// Another user's review artifact in a project userU can read (excluded because OwnedOnly).
	seedReviewArtifact(t, f.store, "project-1", PrincipalKindUser, outside.ref, "other-user-review", VersionKindReview, base.Add(6*time.Minute), nil)

	ctx := context.WithValue(context.Background(), principalKey{}, userU)

	// previews=2 returns count=3, capped=false, newest 2 items.
	count, capped, items, err := f.svc.ReviewPendingSummary(ctx, 2)
	if err != nil {
		t.Fatalf("ReviewPendingSummary: %v", err)
	}
	if count != 3 || capped {
		t.Fatalf("got count=%d capped=%v, want 3 false", count, capped)
	}
	if len(items) != 2 || items[0].ID != idNew || items[1].ID != idMid {
		t.Fatalf("got items %v, want [%s, %s]", items, idNew, idMid)
	}

	// previews=0 returns count=3 and empty items.
	count, capped, items, err = f.svc.ReviewPendingSummary(ctx, 0)
	if err != nil {
		t.Fatalf("ReviewPendingSummary(0): %v", err)
	}
	if count != 3 || capped || len(items) != 0 {
		t.Fatalf("got count=%d capped=%v len=%d, want 3 false 0", count, capped, len(items))
	}

	// Token boundary denial on project-2 excludes idMid.
	f.host.deny(userU, "project-2", PermissionRead)
	count, capped, items, err = f.svc.ReviewPendingSummary(ctx, 5)
	if err != nil {
		t.Fatalf("ReviewPendingSummary(denied): %v", err)
	}
	if count != 2 || capped {
		t.Fatalf("got count=%d capped=%v, want 2 false", count, capped)
	}
	if len(items) != 2 || items[0].ID != idNew || items[1].ID != idOld {
		t.Fatalf("got items %v, want [%s, %s]", items, idNew, idOld)
	}

	// Lower maxListScan to 2 to verify capped=true when scan budget runs out.
	origScan := maxListScan
	maxListScan = 2
	t.Cleanup(func() { maxListScan = origScan })
	count, capped, items, err = f.svc.ReviewPendingSummary(ctx, 5)
	if err != nil {
		t.Fatalf("ReviewPendingSummary(capped): %v", err)
	}
	if !capped {
		t.Fatalf("expected capped=true when scan budget reached; got count=%d items=%d", count, len(items))
	}

	// Unauthenticated caller returns empty without error.
	count, capped, items, err = f.svc.ReviewPendingSummary(context.Background(), 3)
	if err != nil || count != 0 || capped || len(items) != 0 {
		t.Fatalf("unauthenticated: got (%d, %v, %d, %v)", count, capped, len(items), err)
	}

	// Unconfigured backend returns ErrBackendUnavailable.
	unconfigured := NewService(f.host)
	if _, _, _, err := unconfigured.ReviewPendingSummary(ctx, 3); err != ErrBackendUnavailable {
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

	// 4b. When InboxHintsActive is false, revoking a grant skips the grant read and does not fire InboxChanged.
	gRec2 := f.do(&userU, http.MethodPost, "/api/v1/artifacts/"+art.Artifact.ID+"/grants", grantBody, map[string]string{"Content-Type": "application/json"})
	if gRec2.Code != http.StatusCreated {
		t.Fatalf("re-grant review: %d %s", gRec2.Code, gRec2.Body.String())
	}
	var gResp2 GrantResponse
	if err := json.Unmarshal(gRec2.Body.Bytes(), &gResp2); err != nil {
		t.Fatal(err)
	}
	recHost.hintsActive = false
	delGrantRec2 := f.do(&userU, http.MethodDelete, "/api/v1/artifacts/"+art.Artifact.ID+"/grants/"+gResp2.Grant.ID, nil, nil)
	if delGrantRec2.Code != http.StatusNoContent {
		t.Fatalf("delete grant (hints off): %d %s", delGrantRec2.Code, delGrantRec2.Body.String())
	}
	if got := recHost.takeInboxUsers(); len(got) != 0 {
		t.Fatalf("hintsActive=false grant revocation should not fire InboxChanged, got %v", got)
	}
	recHost.hintsActive = true

	// 5. Delete user-owned artifact -> fires InboxChanged for owner only!
	delRec := f.do(&userU, http.MethodDelete, "/api/v1/artifacts/"+art.Artifact.ID, nil, nil)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("delete artifact: %d %s", delRec.Code, delRec.Body.String())
	}
	if got := recHost.takeInboxUsers(); len(got) != 1 || got[0] != userU.ref {
		t.Fatalf("delete user artifact: got %v, want [%s]", got, userU.ref)
	}

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
