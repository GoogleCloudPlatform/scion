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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Shared fixtures for the review level on agent-owned artifacts. The
// review-only grant path for non-administrators (design D24) is removed
// (ptone/scion#5145): only the owner, admin grantees and stewards give
// grants, review included.

var (
	delegator  = principal{PrincipalKindUser, "user-delegator", ""}
	projAdmin  = principal{PrincipalKindUser, "user-admin", ""}
	member     = principal{PrincipalKindUser, "user-member", ""}
	reviewer   = principal{PrincipalKindUser, "user-reviewer", ""}
	otherProj  = principal{PrincipalKindUser, "user-other", ""}
	reviewerID = PrincipalRef(reviewer.kind, reviewer.ref)
)

// newReviewGrantFixture publishes an artifact owned by agentA homed in
// project-1. delegator, projAdmin, member and reviewer read project-1;
// otherProj reads only project-2. None of them administers the artifact
// unless a test makes it so (a steward clause, an admin grant).
func newReviewGrantFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f, _ := newLinkFixture(t)
	id := f.publish(agentA, "review.md", []byte("# draft"), "scope=project-1").Artifact.ID
	for _, p := range []principal{delegator, projAdmin, member, reviewer} {
		f.host.allow(p, "project-1", PermissionRead)
	}
	f.host.allow(otherProj, "project-2", PermissionRead)
	return f, id
}

func reviewBody(kind, ref, perm string) string {
	return `{"subjectKind":"` + kind + `","subjectRef":"` + ref + `","permission":"` + perm + `"}`
}

func (f *fixture) postGrantBody(p principal, id, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(&p, http.MethodPost, grantsPath(id), []byte(body), map[string]string{"Content-Type": "application/json"})
}

func (f *fixture) grantOf(id, subject string) *Grant {
	f.t.Helper()
	gs, err := f.store.ListGrants(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	for i := range gs {
		if gs[i].SubjectKind == SubjectPrincipal && gs[i].SubjectRef == subject {
			return &gs[i]
		}
	}
	return nil
}

// TestPutGrantNonAdministratorUniform403 (K4, ptone/scion#5145): with the
// review-only path removed, a reader who may not administer an agent-owned
// artifact gets the uniform 403 for every body, a well-formed review grant
// to a reader included, even when it is an admin of the home project or the
// agent's delegating user without a steward clause; nothing is written. A
// caller that cannot read gets the 404 of a missing artifact.
func TestPutGrantNonAdministratorUniform403(t *testing.T) {
	f, id := newReviewGrantFixture(t)
	f.host.allowAdmin(projAdmin, "project-1")
	userOwned := f.publish(userU, "mine.md", []byte("# mine"), "scope=project-1").Artifact.ID
	uniform := response(f.postGrantBody(member, userOwned, "junk"))
	c := captureLog(f, slog.LevelInfo)
	for _, tc := range []struct {
		name string
		p    principal
		body string
	}{
		{"project admin, review to a reader", projAdmin, reviewBody(SubjectPrincipal, reviewerID, GrantReview)},
		{"delegating user without a clause, review", delegator, reviewBody(SubjectPrincipal, reviewerID, GrantReview)},
		{"member, read", member, reviewBody(SubjectPrincipal, reviewerID, GrantRead)},
		{"member, malformed", member, "junk"},
		{"member, review to an agent with notify", member, `{"subjectKind":"principal","subjectRef":"agent:x","permission":"review","notify":true}`},
	} {
		c.take(t)
		rec := f.postGrantBody(tc.p, id, tc.body)
		if rec.Code != http.StatusForbidden || response(rec) != uniform {
			t.Errorf("%s: %d %s, want the uniform 403 %s", tc.name, rec.Code, rec.Body.String(), uniform)
		}
		// Every refusal is logged once by logAdminRefusal, before the body
		// is read, with the reason adminAuthorityWith refused it for.
		entries, _ := c.take(t)
		e := one(t, tc.name, entries)
		checkEntry(t, tc.name, e, "WARN", map[string]any{
			logKeyAction: logActionAdminister, logKeyOutcome: logOutcomeRefused,
			logKeyRoute: "POST /api/v1/artifacts/{id}/grants", logKeyArtifactID: id,
		})
		if r := e[logKeyReason]; r != logReasonNotAdministrator && r != logReasonCredentialNotPermit {
			t.Errorf("%s: reason %v, want %s or %s", tc.name, r, logReasonNotAdministrator, logReasonCredentialNotPermit)
		}
	}
	if g := f.grantOf(id, reviewerID); g != nil {
		t.Errorf("a grant was written: %+v", g)
	}
	// A caller that cannot read the artifact gets the 404 of a missing one,
	// body included.
	body := reviewBody(SubjectPrincipal, reviewerID, GrantReview)
	hidden := f.postGrantBody(otherProj, id, body)
	missing := f.postGrantBody(otherProj, uuid.NewString(), body)
	if hidden.Code != http.StatusNotFound || missing.Code != http.StatusNotFound || response(hidden) != response(missing) {
		t.Errorf("non-reader: %d %s; a missing artifact answers %d %s", hidden.Code, hidden.Body.String(), missing.Code, missing.Body.String())
	}
}

// TestAdminForbiddenCopy: the 403 text is the same for agent- and
// user-owned artifacts, states who may share, and no longer offers review
// access to home-project admins.
func TestAdminForbiddenCopy(t *testing.T) {
	f, agentOwned := newReviewGrantFixture(t)
	userOwned := f.publish(userU, "mine.md", []byte("# mine"), "scope=project-1").Artifact.ID
	a := f.postGrantBody(member, agentOwned, "junk")
	u := f.postGrantBody(member, userOwned, "junk")
	if response(a) != response(u) {
		t.Errorf("403 differs by owner kind:\n%s\n%s", response(a), response(u))
	}
	var e errorResponse
	if err := json.Unmarshal(a.Body.Bytes(), &e); err != nil || a.Code != http.StatusForbidden {
		t.Fatalf("%d %v", a.Code, err)
	}
	for _, want := range []string{"owner", "admin grant", "delegating user", "owners of its home project"} {
		if !strings.Contains(e.Error.Message, want) {
			t.Errorf("message %q lacks %q", e.Error.Message, want)
		}
	}
	if strings.Contains(e.Error.Message, "review access") {
		t.Errorf("message %q still offers review access", e.Error.Message)
	}
}
