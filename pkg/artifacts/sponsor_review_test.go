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
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Automatic review access for the delegating user (ptone/scion#7134).

var (
	srSponsor   = principal{PrincipalKindUser, "user-sponsor", ""}
	srSteward   = principal{PrincipalKindUser, "user-sr-steward", ""}
	srSponsorID = PrincipalRef(srSponsor.kind, srSponsor.ref)
)

// sponsorReviewHost is sponsorHost plus SponsorReviewPolicy. The policy
// answers from the fake host's permission table, for the named sponsor:
// a grant when the sponsor reads in the asked scope, the role error
// otherwise. off, inactive and err stand for the experiment, the user's
// status and a lookup failure.
type sponsorReviewHost struct {
	*sponsorHost
	off      bool
	inactive bool
	err      error
	asked    []string
}

func (h *sponsorReviewHost) SponsorReviewGrant(_ context.Context, scopeRef, sponsorUserID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.asked = append(h.asked, scopeRef+" "+sponsorUserID)
	switch {
	case h.off:
		return ErrSponsorReviewOff
	case h.err != nil:
		return h.err
	case h.inactive || !h.perms[PrincipalRef(PrincipalKindUser, sponsorUserID)][scopeRef][PermissionRead]:
		return &SponsorReviewRoleError{Scope: scopeRef, Permission: PermissionRead}
	}
	return nil
}

// newSponsorReviewFixture returns a fixture whose host records srSponsor as
// agentA's delegating user and has a SponsorReviewPolicy; srSponsor reads
// project-1 (agentA's project) through a role.
func newSponsorReviewFixture(t *testing.T) (*fixture, *sponsorReviewHost) {
	t.Helper()
	f := newFixture(t, false)
	h := &sponsorReviewHost{sponsorHost: &sponsorHost{fakeHost: f.host, users: map[string]string{}}}
	h.setUser(agentA.ref, srSponsor.ref)
	f.host.allow(srSponsor, "project-1", PermissionRead)
	svc := NewService(h)
	svc.SetStore(f.store)
	svc.SetBlobStorage(f.blobs, "hub-1")
	f.svc = svc
	return f, h
}

// createPaths creates one artifact on each creation path as p, in
// project-1, and returns their ids by path name.
func createPaths(f *fixture, p principal) map[string]string {
	f.t.Helper()
	req := site.manifest("index.html")
	req.Scope = "project-1"
	return map[string]string{
		"single-file": f.publish(p, "a.md", []byte("# a"), "scope=project-1").Artifact.ID,
		"two-step":    f.createPending(p, "/api/v1/artifacts", req).Artifact.ID,
	}
}

// sponsorGrants returns the principal grants of artifact id.
func sponsorGrants(t *testing.T, f *fixture, id string) []Grant {
	t.Helper()
	gs, err := f.store.ListGrants(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []Grant
	for _, g := range gs {
		if g.SubjectKind == SubjectPrincipal {
			out = append(out, g)
		}
	}
	return out
}

// checkNoSponsorGrant asserts that id has its home grant only and no
// grant_created event.
func checkNoSponsorGrant(t *testing.T, f *fixture, id string) {
	t.Helper()
	gs, err := f.store.ListGrants(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 1 || gs[0].SubjectKind != SubjectScope {
		t.Errorf("grants of %s = %+v, want the home grant only", id, gs)
	}
	for _, e := range eventsOf(t, f.store, id) {
		if e.Action == EventGrantCreated {
			t.Errorf("grant_created event without an automatic grant: %+v", e)
		}
	}
}

// checkSponsorGrant asserts that id has exactly the home grant and one
// automatic review grant for srSponsor, with its one event, and returns
// the review grant.
func checkSponsorGrant(t *testing.T, f *fixture, id string) Grant {
	t.Helper()
	gs, err := f.store.ListGrants(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 2 {
		t.Fatalf("grants of %s = %+v, want the home grant and the automatic review grant", id, gs)
	}
	var home, review *Grant
	for i := range gs {
		switch gs[i].SubjectKind {
		case SubjectScope:
			home = &gs[i]
		case SubjectPrincipal:
			review = &gs[i]
		}
	}
	if home == nil || home.SubjectRef != "project-1" || home.Permission != GrantRead {
		t.Errorf("home grant = %+v", home)
	}
	if review == nil {
		t.Fatalf("no principal grant among %+v", gs)
	}
	if review.SubjectRef != srSponsorID || review.Permission != GrantReview ||
		review.CreatedByRef != SponsorReviewCreator || review.ExpiresAt != nil {
		t.Errorf("automatic grant = %+v, want review for %s by %s", *review, srSponsorID, SponsorReviewCreator)
	}
	e := onlyEvent(t, f.store, id, EventGrantCreated, &sponsorReviewActor)
	if e.Detail["subjectKind"] != SubjectPrincipal || e.Detail["subjectRef"] != srSponsorID ||
		e.Detail["permission"] != GrantReview || e.Detail["grantId"] != review.ID {
		t.Errorf("grant_created detail = %v", e.Detail)
	}
	return *review
}

// TestSponsorReviewGrantOnCreate: on both creation paths, an agent with a
// recorded delegating user who reads the home project through a role
// creates its artifact with exactly two grants (home read, review for the
// delegating user) and one grant_created event by the system actor
// sponsor_review. The policy is asked for the artifact's home and the
// recorded user.
func TestSponsorReviewGrantOnCreate(t *testing.T) {
	f, h := newSponsorReviewFixture(t)
	for path, id := range createPaths(f, agentA) {
		t.Run(path, func(t *testing.T) { checkSponsorGrant(t, f, id) })
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.asked) != 2 || h.asked[0] != "project-1 "+srSponsor.ref || h.asked[1] != "project-1 "+srSponsor.ref {
		t.Errorf("policy asked %v, want project-1 and the sponsor twice", h.asked)
	}
	if n := f.svc.SponsorReviewSkips(); n != 0 {
		t.Errorf("skips = %d, want 0", n)
	}
}

// TestSponsorReviewGrantSkipped: no automatic grant when the delegating
// user holds no role that reads in the home project, is inactive, or the
// policy's lookup fails (each counted as a skip), when the policy is
// off (not counted), and for no sponsor, a user owner, an attachment or a
// host without the policy. The artifact is created regardless.
func TestSponsorReviewGrantSkipped(t *testing.T) {
	counted := []struct {
		name string
		set  func(f *fixture, h *sponsorReviewHost)
	}{
		{"no role in home", func(f *fixture, h *sponsorReviewHost) {
			h.mu.Lock()
			delete(h.perms, srSponsorID)
			h.mu.Unlock()
		}},
		{"inactive", func(_ *fixture, h *sponsorReviewHost) { h.inactive = true }},
		{"lookup failure", func(_ *fixture, h *sponsorReviewHost) { h.err = errors.New("store down") }},
	}
	for _, tc := range counted {
		t.Run(tc.name, func(t *testing.T) {
			f, h := newSponsorReviewFixture(t)
			tc.set(f, h)
			for _, id := range createPaths(f, agentA) {
				checkNoSponsorGrant(t, f, id)
				if got := f.sponsorOf(id); got != srSponsor.ref {
					t.Errorf("sponsor recorded %q, want %q (the skip must not touch it)", got, srSponsor.ref)
				}
			}
			if n := f.svc.SponsorReviewSkips(); n != 2 {
				t.Errorf("skips = %d, want 2", n)
			}
		})
	}
	t.Run("policy off", func(t *testing.T) {
		f, h := newSponsorReviewFixture(t)
		h.off = true
		for _, id := range createPaths(f, agentA) {
			checkNoSponsorGrant(t, f, id)
		}
		if n := f.svc.SponsorReviewSkips(); n != 0 {
			t.Errorf("skips = %d, want 0 while off", n)
		}
	})
	t.Run("no sponsor", func(t *testing.T) {
		f, h := newSponsorReviewFixture(t)
		h.setUser(agentA.ref, "")
		for _, id := range createPaths(f, agentA) {
			checkNoSponsorGrant(t, f, id)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if len(h.asked) != 0 {
			t.Errorf("policy asked %v without a sponsor", h.asked)
		}
	})
	t.Run("user owner", func(t *testing.T) {
		f, h := newSponsorReviewFixture(t)
		h.setUser(userU.ref, srSponsor.ref)
		for _, id := range createPaths(f, userU) {
			checkNoSponsorGrant(t, f, id)
		}
	})
	t.Run("attachment", func(t *testing.T) {
		f, _ := newSponsorReviewFixture(t)
		info, err := f.svc.CreateAttachment(ctxAs(agentA), "a.txt", "text/plain", strings.NewReader("x"))
		if err != nil {
			t.Fatal(err)
		}
		if gs := sponsorGrants(t, f, info.ID); len(gs) != 0 {
			t.Errorf("attachment grants = %+v", gs)
		}
	})
	t.Run("host without policy", func(t *testing.T) {
		f := newFixture(t, false)
		withSponsorHost(f).setUser(agentA.ref, srSponsor.ref)
		f.host.allow(srSponsor, "project-1", PermissionRead)
		for _, id := range createPaths(f, agentA) {
			checkNoSponsorGrant(t, f, id)
		}
	})
}

// TestSponsorReviewGrantHomeScopeOnly: the role check is made against the
// artifact's own home, not the agent's project. The delegating user reads
// agentA's project (project-1) through a role but holds none in project-2;
// an artifact agentA creates in project-2 gets no grant on either path,
// the skip is logged with the reason only, and the policy's error names
// the scope and the permission only. The same agent creating in project-1
// still gets it.
func TestSponsorReviewGrantHomeScopeOnly(t *testing.T) {
	f, h := newSponsorReviewFixture(t)
	f.host.allow(agentA, "project-2", PermissionRead, PermissionCreate)
	c := captureLog(f, slog.LevelInfo)
	ids := []string{
		f.publish(agentA, "a.md", []byte("# a"), "scope=project-2").Artifact.ID,
	}
	req := site.manifest("index.html")
	req.Scope = "project-2"
	ids = append(ids, f.createPending(agentA, "/api/v1/artifacts", req).Artifact.ID)
	for _, id := range ids {
		checkNoSponsorGrant(t, f, id)
	}
	if n := f.svc.SponsorReviewSkips(); n != 2 {
		t.Errorf("skips = %d, want 2", n)
	}
	entries, raw := c.take(t)
	var skips int
	for _, e := range entries {
		if e[logKeyOutcome] == logOutcomeRefused {
			skips++
			checkEntry(t, "skip", e, "INFO", map[string]any{
				logKeyAction: logActionGrantCreated, logKeyReason: logReasonSponsorNoRole,
				logKeyAuthority: AuthoritySponsorReview, logKeyProjectID: "project-2", logKeySubjectRef: nil,
			})
		}
	}
	if skips != 2 {
		t.Errorf("%d skip entries, want 2: %v", skips, entries)
	}
	if strings.Contains(raw, srSponsor.ref) {
		t.Errorf("decision log names the delegating user: %s", raw)
	}
	h.mu.Lock()
	asked := append([]string(nil), h.asked...)
	h.mu.Unlock()
	for _, a := range asked {
		if a != "project-2 "+srSponsor.ref {
			t.Errorf("policy asked %q, want the artifact's home project-2", a)
		}
	}
	err := h.SponsorReviewGrant(context.Background(), "project-2", srSponsor.ref)
	var roleErr *SponsorReviewRoleError
	if !errors.As(err, &roleErr) {
		t.Fatalf("policy answer %v, want a role error", err)
	}
	if got, want := err.Error(), "artifacts: no active role with artifact.read in project project-2"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
	if strings.Contains(err.Error(), srSponsor.ref) || strings.Contains(err.Error(), "project-1") {
		t.Errorf("error names more than the scope and role: %q", err.Error())
	}
	checkSponsorGrant(t, f, f.publish(agentA, "b.md", []byte("# b"), "scope=project-1").Artifact.ID)
}

// TestSponsorReviewGrantNotOnAppend: appending under an existing key, a new
// version and a re-publish never add a grant, and a removed automatic grant
// is never re-created.
func TestSponsorReviewGrantNotOnAppend(t *testing.T) {
	f, _ := newSponsorReviewFixture(t)
	req := site.manifest("index.html")
	req.Key = "plan"
	first := f.createPending(agentA, "/api/v1/artifacts", req)
	g := checkSponsorGrant(t, f, first.Artifact.ID)

	again := f.createPending(agentA, "/api/v1/artifacts", req)
	if again.Artifact.ID != first.Artifact.ID {
		t.Fatalf("keyed create made a new artifact %s", again.Artifact.ID)
	}
	checkSponsorGrant(t, f, first.Artifact.ID)

	if err := f.store.DeleteGrant(context.Background(), first.Artifact.ID, g.ID, testActor); err != nil {
		t.Fatal(err)
	}
	f.createPending(agentA, "/api/v1/artifacts", req)
	f.createPending(agentA, "/api/v1/artifacts/"+first.Artifact.ID+"/versions", site.manifest("index.html"))
	if gs := sponsorGrants(t, f, first.Artifact.ID); len(gs) != 0 {
		t.Errorf("removed automatic grant came back: %+v", gs)
	}
	var created int
	for _, e := range eventsOf(t, f.store, first.Artifact.ID) {
		if e.Action == EventGrantCreated {
			created++
		}
	}
	if created != 1 {
		t.Errorf("%d grant_created events, want 1", created)
	}
}

// TestSponsorReviewSubjectIsRecordedSponsor: the grant's subject is the
// recorded delegating user, whatever the request names, and never the
// owning agent.
func TestSponsorReviewSubjectIsRecordedSponsor(t *testing.T) {
	f, _ := newSponsorReviewFixture(t)
	forged := "user-forged"
	f.host.allow(principal{PrincipalKindUser, forged, ""}, "project-1", PermissionRead)
	rec := f.do(&agentA, http.MethodPost, "/api/v1/artifacts?name=a.md&sponsor_ref="+forged+"&subjectRef=user:"+forged,
		[]byte("a"), map[string]string{"Content-Type": "application/octet-stream", "X-Sponsor-Ref": forged})
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body.String())
	}
	g := checkSponsorGrant(t, f, decodeInto[ArtifactResponse](t, rec).Artifact.ID)
	if g.SubjectRef == PrincipalRef(agentA.kind, agentA.ref) || strings.Contains(g.SubjectRef, forged) {
		t.Errorf("subject = %s", g.SubjectRef)
	}
}

// reviewStatus and publishStatus start a review version and an ordinary
// version of artifact id as p, and return the status.
func reviewStatus(f *fixture, p principal, id string) int {
	f.t.Helper()
	req := bundle{"a.md": []byte("# a {>>ok?<<}")}.manifest("a.md")
	req.Kind = VersionKindReview
	return f.postJSON(&p, "/api/v1/artifacts/"+id+"/versions", req).Code
}

func publishStatus(f *fixture, p principal, id string) (int, string) {
	f.t.Helper()
	rec := f.postJSON(&p, "/api/v1/artifacts/"+id+"/versions", bundle{"a.md": []byte("# b")}.manifest("a.md"))
	if rec.Code == http.StatusCreated {
		return rec.Code, ""
	}
	return rec.Code, errCode(f.t, rec)
}

// TestSponsorReviewGrantUse: with the automatic grant, the delegating user
// starts a review version with no manual grant and is refused an ordinary
// version (review_only).
func TestSponsorReviewGrantUse(t *testing.T) {
	f, _ := newSponsorReviewFixture(t)
	id := f.publish(agentA, "a.md", []byte("# a"), "").Artifact.ID
	checkSponsorGrant(t, f, id)
	if code := reviewStatus(f, srSponsor, id); code != http.StatusCreated {
		t.Errorf("review by the delegating user: %d", code)
	}
	if code, ec := publishStatus(f, srSponsor, id); code != http.StatusForbidden || ec != CodeReviewOnly {
		t.Errorf("ordinary version by the delegating user: %d %s, want 403 %s", code, ec, CodeReviewOnly)
	}
}

// TestSponsorReviewAutomaticForm: while the grant is in its automatic
// form it counts only while the delegating user reads the home project
// through a role. The user also reads the artifact through a grant to
// project-2, so only the automatic-form rule decides: without a role in
// project-1 the review is refused, and re-adding the role restores it. A
// steward's level change makes it an ordinary grant created by the
// steward, which then survives the role's loss; a re-put at the same
// level leaves it automatic.
func TestSponsorReviewAutomaticForm(t *testing.T) {
	f, _ := newSponsorReviewFixture(t)
	f.host.crossScope = true
	f.host.allow(srSponsor, "project-2", PermissionRead)
	id := f.publish(agentA, "a.md", []byte("# a"), "").Artifact.ID
	checkSponsorGrant(t, f, id)
	ctx := context.Background()
	if _, err := f.store.PutGrant(ctx, &Grant{ID: uuid.NewString(), ArtifactID: id, SubjectKind: SubjectScope,
		SubjectRef: "project-2", Permission: GrantRead, CreatedAt: time.Now()}, MaxGrantsPerArtifact, true, testActor); err != nil {
		t.Fatal(err)
	}
	leave := func() {
		f.host.mu.Lock()
		delete(f.host.perms[srSponsorID], "project-1")
		f.host.mu.Unlock()
	}

	if code := reviewStatus(f, srSponsor, id); code != http.StatusCreated {
		t.Fatalf("review with a role: %d", code)
	}
	leave()
	if rec := f.do(&srSponsor, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("the project-2 grant no longer reads: %d", rec.Code)
	}
	if code := reviewStatus(f, srSponsor, id); code != http.StatusForbidden {
		t.Errorf("review after leaving the home project: %d, want 403", code)
	}
	f.host.allow(srSponsor, "project-1", PermissionRead)
	if code := reviewStatus(f, srSponsor, id); code != http.StatusCreated {
		t.Errorf("review after being re-added: %d", code)
	}

	// A re-put at the same level by a steward changes nothing.
	f.host.allow(srSteward, "project-1", PermissionRead)
	f.host.allowSteward(srSteward, agentA.ref, "project-1", StewardClauseProjectOwner)
	if rec := jsonDo(f, srSteward, http.MethodPost, grantsPath(id), reviewBody(SubjectPrincipal, srSponsorID, GrantReview)); rec.Code != http.StatusOK {
		t.Fatalf("same-level re-put: %d %s", rec.Code, rec.Body.String())
	}
	if g := f.grantOf(id, srSponsorID); g == nil || g.CreatedByRef != SponsorReviewCreator {
		t.Errorf("after a same-level re-put: %+v, want still automatic", g)
	}

	// A level change by the steward ends the automatic form.
	if rec := jsonDo(f, srSteward, http.MethodPost, grantsPath(id), reviewBody(SubjectPrincipal, srSponsorID, GrantWrite)); rec.Code != http.StatusOK {
		t.Fatalf("level change: %d %s", rec.Code, rec.Body.String())
	}
	g := f.grantOf(id, srSponsorID)
	if g == nil || g.Permission != GrantWrite || g.CreatedByRef != PrincipalRef(srSteward.kind, srSteward.ref) {
		t.Fatalf("after a level change: %+v, want write created by the steward", g)
	}
	f.host.mu.Lock()
	delete(f.host.perms, srSponsorID)
	f.host.mu.Unlock()
	if code, _ := publishStatus(f, srSponsor, id); code != http.StatusCreated {
		t.Errorf("deliberate grant after leaving every project: %d, want 201", code)
	}
}

// TestGrantAllowsAutomaticForm pins the automatic-form rule in the one place grants
// are matched: an automatic-form grant counts only when the host
// authorizes the caller to read in the home scope (a failed lookup answers
// no), and a grant created by a person counts regardless.
func TestGrantAllowsAutomaticForm(t *testing.T) {
	h := newFakeHost()
	a := &Artifact{ID: "a1", ScopeKind: ScopeKindProject, ScopeRef: "project-1", OwnerKind: PrincipalKindAgent, OwnerRef: agentA.ref}
	auto := Grant{SubjectKind: SubjectPrincipal, SubjectRef: srSponsorID, Permission: GrantReview, CreatedByRef: SponsorReviewCreator}
	deliberate := auto
	deliberate.CreatedByRef = PrincipalRef(srSteward.kind, srSteward.ref)
	ctx := ctxAs(srSponsor)
	now := time.Now()
	if grantAllows(ctx, h, a, []Grant{auto}, now, srSponsor.kind, srSponsor.ref, grantsForReview, PermissionCreate, false) {
		t.Error("automatic grant counted without a role in the home project")
	}
	if !grantAllows(ctx, h, a, []Grant{deliberate}, now, srSponsor.kind, srSponsor.ref, grantsForReview, PermissionCreate, false) {
		t.Error("deliberate grant did not count")
	}
	h.allow(srSponsor, "project-2", PermissionRead)
	if grantAllows(ctx, h, a, []Grant{auto}, now, srSponsor.kind, srSponsor.ref, grantsForReview, PermissionCreate, false) {
		t.Error("automatic grant counted with a role in another project only")
	}
	h.allow(srSponsor, "project-1", PermissionRead)
	if !grantAllows(ctx, h, a, []Grant{auto}, now, srSponsor.kind, srSponsor.ref, grantsForReview, PermissionCreate, false) {
		t.Error("automatic grant did not count with a role in the home project")
	}
	// The credential (Permits) is part of the host's decision: refused, the
	// automatic grant does not count.
	h.deny(srSponsor, "project-1", PermissionRead)
	if grantAllows(ctx, h, a, []Grant{auto}, now, srSponsor.kind, srSponsor.ref, grantsForReview, PermissionCreate, false) {
		t.Error("automatic grant counted when the host refused the read")
	}
}

// TestSponsorReviewCreatorReserved: the automatic form is reachable
// only from creation. A grant request carrying a creator is refused (400),
// and the store refuses a put that carries the creator or the authority.
func TestSponsorReviewCreatorReserved(t *testing.T) {
	f, _ := newSponsorReviewFixture(t)
	id := f.publish(agentA, "a.md", []byte("# a"), "").Artifact.ID
	f.host.allow(srSteward, "project-1", PermissionRead)
	f.host.allowSteward(srSteward, agentA.ref, "project-1", StewardClauseProjectOwner)
	other := principal{PrincipalKindUser, "user-other-reviewer", ""}
	otherID := PrincipalRef(other.kind, other.ref)
	for _, field := range []string{"createdBy", "createdByRef", "created_by_ref"} {
		body := `{"subjectKind":"principal","subjectRef":"` + otherID + `","permission":"review","` + field + `":"` + SponsorReviewCreator + `"}`
		if rec := jsonDo(f, srSteward, http.MethodPost, grantsPath(id), body); rec.Code != http.StatusBadRequest {
			t.Errorf("grant with %s: %d %s, want 400", field, rec.Code, rec.Body.String())
		}
	}
	if g := f.grantOf(id, otherID); g != nil {
		t.Errorf("grant written: %+v", g)
	}
	ctx := context.Background()
	g := &Grant{ID: uuid.NewString(), ArtifactID: id, SubjectKind: SubjectPrincipal, SubjectRef: otherID,
		Permission: GrantReview, CreatedByRef: SponsorReviewCreator, CreatedAt: time.Now()}
	if _, err := f.store.PutGrant(ctx, g, MaxGrantsPerArtifact, true, testActor); !errors.Is(err, errSponsorReviewReserved) {
		t.Errorf("PutGrant with the creator: %v", err)
	}
	g.CreatedByRef = "user:x"
	if _, err := f.store.PutGrant(ctx, g, MaxGrantsPerArtifact, true, &sponsorReviewActor); !errors.Is(err, errSponsorReviewReserved) {
		t.Errorf("PutGrant as the sponsor_review actor: %v", err)
	}
	if g := f.grantOf(id, otherID); g != nil {
		t.Errorf("grant written: %+v", g)
	}
}

// TestDecisionLogSponsorReviewGrant: the automatic grant is logged as a
// grant_created entry with authority sponsor_review and its subject.
func TestDecisionLogSponsorReviewGrant(t *testing.T) {
	f, _ := newSponsorReviewFixture(t)
	c := captureLog(f, slog.LevelInfo)
	f.publish(agentA, "a.md", []byte("# a"), "")
	entries, _ := c.take(t)
	var grants []map[string]any
	for _, e := range entries {
		if e[logKeyAction] == logActionGrantCreated {
			grants = append(grants, e)
		}
	}
	checkEntry(t, "automatic grant", one(t, "automatic grant", grants), "INFO", map[string]any{
		logKeyAuthority: AuthoritySponsorReview, logKeySubjectKind: SubjectPrincipal,
		logKeySubjectRef: srSponsorID, logKeyPermission: GrantReview, logKeyOwnerKind: PrincipalKindAgent,
	})
}

// TestSponsorBackfillAddsNoGrant: the sponsor backfill of ptone/scion#6028
// records the delegating user on older artifacts and adds no grant and no
// grant_created event: only new artifacts get the automatic grant.
func TestSponsorBackfillAddsNoGrant(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		id := seedBackfillRow(t, db, st, backfillRow{scopeKind: ScopeKindProject, ownerKind: PrincipalKindAgent, ownerRef: "agent-old"})
		res := newCountingResolver(map[string]string{"agent-old": srSponsor.ref})
		got, err := BackfillSponsors(ctx, st, res.resolve, 10, time.Now())
		if err != nil || got.Artifacts != 1 {
			t.Fatalf("BackfillSponsors = %+v, %v", got, err)
		}
		a, err := st.GetArtifact(ctx, id)
		if err != nil || a.SponsorRef != srSponsor.ref {
			t.Fatalf("sponsor after backfill = %+v, %v", a, err)
		}
		if gs, err := st.ListGrants(ctx, id); err != nil || len(gs) != 0 {
			t.Errorf("grants after backfill = %+v, %v; want none", gs, err)
		}
		for _, e := range eventsOf(t, st, id) {
			if e.Action == EventGrantCreated {
				t.Errorf("grant_created event after backfill: %+v", e)
			}
		}
	})
}

// sponsorReviewCreate builds a published artifact in project-1 owned by an
// agent, with its home grant and, when withReview, an automatic review
// grant shaped by shape.
func sponsorReviewCreate(shape func(*Grant)) (*Artifact, *Version, []File, []Grant) {
	now := time.Now()
	a := &Artifact{ID: uuid.NewString(), ScopeKind: ScopeKindProject, ScopeRef: "project-1", OwnerKind: PrincipalKindAgent,
		OwnerRef: "agent-sr", Title: "T", CreatedAt: now, UpdatedAt: now, SponsorRef: srSponsor.ref}
	v := &Version{ID: uuid.NewString(), ArtifactID: a.ID, Seq: 1, Kind: VersionKindPublish, EntryPath: "a.md",
		TotalBytes: 1, FileCount: 1, CreatedAt: now, State: VersionStateReady}
	fl := File{VersionID: v.ID, Path: "a.md", Size: 1, SHA256: sha([]byte(a.ID)), MediaType: "text/markdown"}
	home := Grant{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectScope, SubjectRef: "project-1", Permission: GrantRead, CreatedAt: now}
	review := Grant{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectPrincipal, SubjectRef: srSponsorID,
		Permission: GrantReview, CreatedByRef: SponsorReviewCreator, CreatedAt: now}
	if shape != nil {
		shape(&review)
	}
	return a, v, []File{fl}, []Grant{home, review}
}

// TestStoreSponsorReviewGrantCommitsWithArtifact: on both dialects and both
// create calls, the automatic grant and its event commit with the
// artifact; a grant of another shape carrying the creator is refused and
// nothing is written; and a failing event insert rolls the artifact back.
func TestStoreSponsorReviewGrantCommitsWithArtifact(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		creates := map[string]func(a *Artifact, v *Version, fs []File, gs []Grant) error{
			"published": func(a *Artifact, v *Version, fs []File, gs []Grant) error {
				return st.CreatePublished(ctx, a, v, fs, gs)
			},
			"pending": func(a *Artifact, v *Version, fs []File, gs []Grant) error {
				v.State = VersionStatePending
				return st.CreatePending(ctx, a, v, fs, gs)
			},
		}
		for name, create := range creates {
			a, v, fs, gs := sponsorReviewCreate(nil)
			if err := create(a, v, fs, gs); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if got, err := st.ListGrants(ctx, a.ID); err != nil || len(got) != 2 {
				t.Errorf("%s: grants = %+v, %v", name, got, err)
			}
			e := onlyEvent(t, st, a.ID, EventGrantCreated, &sponsorReviewActor)
			if e.Detail["subjectRef"] != srSponsorID || e.Detail["permission"] != GrantReview {
				t.Errorf("%s: detail = %v", name, e.Detail)
			}
			for shapeName, shape := range map[string]func(*Grant){
				"write":   func(g *Grant) { g.Permission = GrantWrite },
				"agent":   func(g *Grant) { g.SubjectRef = "agent:x" },
				"scope":   func(g *Grant) { g.SubjectKind, g.SubjectRef = SubjectScope, "project-2" },
				"expires": func(g *Grant) { exp := time.Now().Add(time.Hour); g.ExpiresAt = &exp },
			} {
				a, v, fs, gs := sponsorReviewCreate(shape)
				if err := create(a, v, fs, gs); err == nil {
					t.Errorf("%s/%s: created", name, shapeName)
				}
				if _, err := st.GetArtifact(ctx, a.ID); !errors.Is(err, ErrNotFound) {
					t.Errorf("%s/%s: artifact left behind: %v", name, shapeName, err)
				}
			}
		}
		// Without the event table every automatic grant's event fails, and
		// the creation rolls back; a creation without one is unaffected.
		if _, err := db.Exec("DROP TABLE artifact_event"); err != nil {
			t.Fatal(err)
		}
		for name, create := range creates {
			a, v, fs, gs := sponsorReviewCreate(nil)
			if err := create(a, v, fs, gs); err == nil {
				t.Errorf("%s: created without its event", name)
			}
			if _, err := st.GetArtifact(ctx, a.ID); !errors.Is(err, ErrNotFound) {
				t.Errorf("%s: artifact committed without its event: %v", name, err)
			}
			a, v, fs, gs = sponsorReviewCreate(nil)
			if err := create(a, v, fs, gs[:1]); err != nil {
				t.Errorf("%s: plain creation failed: %v", name, err)
			}
		}
	})
}

// TestSponsorReviewSkipLogCarriesNoIdentifiers: a policy lookup failure whose error
// text names the delegating user and a store error is logged in the general
// log with the fixed reason principal_check_failed and the artifact id, and
// in the decision log with that reason and its usual fields (route, the
// calling agent, the home project, owner_kind); neither carries the user id
// or the error text.
func TestSponsorReviewSkipLogCarriesNoIdentifiers(t *testing.T) {
	f, h := newSponsorReviewFixture(t)
	h.err = errors.New("lookup of " + srSponsor.ref + " failed: pq: connection refused at db-host-7")
	c := captureLog(f, slog.LevelDebug)
	var general bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&general, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	id := f.publish(agentA, "a.md", []byte("# a"), "scope=project-1").Artifact.ID
	checkNoSponsorGrant(t, f, id)
	entries, raw := c.take(t)
	for name, text := range map[string]string{"general log": general.String(), "decision log": raw} {
		for _, banned := range []string{srSponsor.ref, "connection refused", "db-host-7", "pq:"} {
			if strings.Contains(text, banned) {
				t.Errorf("%s carries %q: %s", name, banned, text)
			}
		}
	}
	if !strings.Contains(general.String(), `"reason":"`+logReasonCheckerError+`"`) || !strings.Contains(general.String(), id) {
		t.Errorf("general log line lacks the reason code or the artifact id: %s", general.String())
	}
	var skips int
	for _, e := range entries {
		if e[logKeyOutcome] == logOutcomeRefused {
			skips++
			checkEntry(t, "skip", e, "INFO", map[string]any{
				logKeyAction: logActionGrantCreated, logKeyReason: logReasonCheckerError,
				logKeyArtifactID: id, logKeySubjectRef: nil,
			})
		}
	}
	if skips != 1 {
		t.Errorf("%d skip entries, want 1: %v", skips, entries)
	}
}
