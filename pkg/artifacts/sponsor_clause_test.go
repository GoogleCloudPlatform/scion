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
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedOwned stores a published artifact owned by (ownerKind, ownerRef) in
// project-1, readable by the project, with sponsor recorded as given.
func (f *fixture) seedOwned(ownerKind, ownerRef, sponsor string) *Artifact {
	f.t.Helper()
	now := time.Now()
	a := &Artifact{ID: uuid.NewString(), ScopeKind: ScopeKindProject, ScopeRef: "project-1", OwnerKind: ownerKind,
		OwnerRef: ownerRef, Title: "T", CreatedAt: now, UpdatedAt: now, SponsorRef: sponsor}
	v := &Version{ID: uuid.NewString(), ArtifactID: a.ID, Seq: 1, Kind: VersionKindPublish, EntryPath: "a.md",
		TotalBytes: 1, FileCount: 1, CreatedAt: now, State: VersionStateReady}
	fl := File{VersionID: v.ID, Path: "a.md", Size: 1, SHA256: sha([]byte(a.ID)), MediaType: "text/markdown"}
	g := Grant{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectScope, SubjectRef: "project-1",
		Permission: GrantRead, CreatedAt: now}
	if err := f.store.CreatePublished(context.Background(), a, v, []File{fl}, []Grant{g}); err != nil {
		f.t.Fatal(err)
	}
	return a
}

func (h *fakeHost) sponsorQuestions() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.sponsorArgs)
}

// TestSponsorPassedToClauses: the service hands the recorded sponsor of
// the artifact to the steward clause (stewardAuthority), and "" for an
// artifact with nothing recorded (ptone/scion#6028).
func TestSponsorPassedToClauses(t *testing.T) {
	f := newFixture(t, false)
	recorded := f.seedOwned(PrincipalKindAgent, agentA.ref, "user-delegator")
	unrecorded := f.seedOwned(PrincipalKindAgent, agentB.ref, "")
	for _, id := range []string{recorded.ID, unrecorded.ID} {
		if rec := f.do(&userU, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil); rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", id, rec.Code, rec.Body.String())
		}
	}
	got := f.host.sponsorQuestions()
	for _, want := range []string{
		"steward " + agentA.ref + " user-delegator",
		"steward " + agentB.ref + " ",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("host was not asked %q; asked %q", want, got)
		}
	}
}

// TestSponsorIgnoredUnlessAgentOwned pins the owner_kind gate the sponsor
// relies on: an artifact not owned by an agent has no stewards even when it
// carries a sponsor matching the caller (as a row would after an ownership
// change), and the host is never asked the steward clause for it.
func TestSponsorIgnoredUnlessAgentOwned(t *testing.T) {
	f := newFixture(t, false)
	a := f.seedOwned(PrincipalKindUser, "user-9", userU.ref)
	// A host that would make anyone a steward.
	f.host.stewardAll = StewardClauseDelegator
	rec := f.do(&userU, http.MethodGet, "/api/v1/artifacts/"+a.ID, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	resp := decodeInto[ArtifactResponse](t, rec)
	if resp.CanShare || resp.ShareAuthority != "" {
		t.Errorf("user-owned artifact with a sponsor: canShare=%v shareAuthority=%q",
			resp.CanShare, resp.ShareAuthority)
	}
	share := f.postJSON(&userU, "/api/v1/artifacts/"+a.ID+"/grants",
		GrantRequest{SubjectKind: SubjectPrincipal, SubjectRef: PrincipalRef(PrincipalKindUser, "user-3"), Permission: GrantRead})
	if share.Code != http.StatusForbidden {
		t.Errorf("read grant by the sponsor on a user-owned artifact: %d, want 403", share.Code)
	}
	review := f.postJSON(&userU, "/api/v1/artifacts/"+a.ID+"/grants",
		GrantRequest{SubjectKind: SubjectPrincipal, SubjectRef: PrincipalRef(PrincipalKindUser, "user-3"), Permission: GrantReview})
	if review.Code != http.StatusForbidden {
		t.Errorf("review grant by the sponsor on a user-owned artifact: %d, want 403", review.Code)
	}
	if got := f.host.sponsorQuestions(); len(got) != 0 {
		t.Errorf("host asked a clause for a user-owned artifact: %q", got)
	}
}
