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
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Stewards (design D25, ptone/scion#5144).

var (
	ownerSteward   = principal{PrincipalKindUser, "user-proj-owner", ""}
	delegSteward   = principal{PrincipalKindUser, "user-deleg", ""}
	stAdminGrantee = principal{PrincipalKindUser, "user-st-admin-grantee", ""}
	custodianP     = principal{PrincipalKindUser, "user-proj-admin", ""}
	plainReader    = principal{PrincipalKindUser, "user-reader", ""}
)

// stewardFixture holds an artifact owned by agentA and homed in project-1,
// shareable across projects, with one principal per authority:
// ownerSteward and delegSteward are its stewards (project_owner,
// delegator), stAdminGrantee holds an admin grant, custodianP administers
// project-1 (custodian only), and plainReader only reads. Each of them
// reads project-1 and may publish in project-2 (the move target). A
// principal grant to user:grantee (admin) and one share link exist, so the
// removal and lowering actions have a target.
type stewardFixture struct {
	*fixture
	id     string
	linkID string
}

func newStewardFixture(t *testing.T) *stewardFixture {
	t.Helper()
	f, _ := newLinkFixture(t)
	f.host.crossScope = true
	id := f.publish(agentA, "steward.md", []byte("# agent work"), "scope=project-1").Artifact.ID
	for _, p := range []principal{ownerSteward, delegSteward, stAdminGrantee, custodianP, plainReader} {
		f.host.allow(p, "project-1", PermissionRead)
		f.host.allow(p, "project-2", PermissionCreate)
	}
	f.host.allowSteward(ownerSteward, agentA.ref, "project-1", StewardClauseProjectOwner)
	f.host.allowSteward(delegSteward, agentA.ref, "project-1", StewardClauseDelegator)
	f.host.allowAdmin(custodianP, "project-1")
	ctx := context.Background()
	for _, g := range []*Grant{
		{ID: uuid.NewString(), ArtifactID: id, SubjectKind: SubjectPrincipal, SubjectRef: PrincipalRef(stAdminGrantee.kind, stAdminGrantee.ref), Permission: GrantAdmin, CreatedAt: time.Now()},
		{ID: uuid.NewString(), ArtifactID: id, SubjectKind: SubjectPrincipal, SubjectRef: "user:grantee", Permission: GrantAdmin, CreatedAt: time.Now()},
	} {
		if _, err := f.store.PutGrant(ctx, g, MaxGrantsPerArtifact, true, testActor); err != nil {
			t.Fatal(err)
		}
	}
	link := linkGrant(id, LinkTokenHash("steward-fixture"), time.Now().Add(time.Hour))
	if err := f.store.CreateLink(ctx, link, MaxLinksPerArtifact, time.Now(), testActor); err != nil {
		t.Fatal(err)
	}
	return &stewardFixture{fixture: f, id: id, linkID: link.ID}
}

func (f *stewardFixture) jsonDo(p principal, method, target, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	var b []byte
	if body != "" {
		b = []byte(body)
	}
	return f.do(&p, method, target, b, map[string]string{"Content-Type": "application/json"})
}

func (f *stewardFixture) grantID(subject string) string {
	f.t.Helper()
	g := f.grantOf(f.id, subject)
	if g == nil {
		return uuid.NewString() // none (the parity test asks about a missing artifact)
	}
	return g.ID
}

// stewardAction is one management request on the fixture's artifact.
type stewardAction struct {
	name string
	do   func(f *stewardFixture, p principal) *httptest.ResponseRecorder
}

func stewardActions() []stewardAction {
	art := func(f *stewardFixture) string { return "/api/v1/artifacts/" + f.id }
	put := func(kind, ref, perm string) func(f *stewardFixture, p principal) *httptest.ResponseRecorder {
		return func(f *stewardFixture, p principal) *httptest.ResponseRecorder {
			return f.jsonDo(p, http.MethodPost, grantsPath(f.id), reviewBody(kind, ref, perm))
		}
	}
	return []stewardAction{
		{"grant user read", put(SubjectPrincipal, "user:outsider", GrantRead)},
		{"grant user review", put(SubjectPrincipal, "user:outsider", GrantReview)},
		{"grant user write", put(SubjectPrincipal, "user:outsider", GrantWrite)},
		{"grant user admin", put(SubjectPrincipal, "user:outsider", GrantAdmin)},
		{"grant agent read", put(SubjectPrincipal, "agent:other-agent", GrantRead)},
		{"grant agent admin", put(SubjectPrincipal, "agent:other-agent", GrantAdmin)},
		{"grant project read", put(SubjectScope, "project-3", GrantRead)},
		{"grant project write", put(SubjectScope, "project-3", GrantWrite)},
		{"grant project admin", put(SubjectScope, "project-3", GrantAdmin)},
		{"raise home to write", put(SubjectScope, "project-1", GrantWrite)},
		{"lower admin grant to write", put(SubjectPrincipal, "user:grantee", GrantWrite)},
		{"raise to admin", put(SubjectPrincipal, "user:grantee", GrantAdmin)},
		{"list grants", func(f *stewardFixture, p principal) *httptest.ResponseRecorder {
			return f.jsonDo(p, http.MethodGet, grantsPath(f.id), "")
		}},
		{"remove grant", func(f *stewardFixture, p principal) *httptest.ResponseRecorder {
			return f.jsonDo(p, http.MethodDelete, grantsPath(f.id)+"/"+f.grantID("user:grantee"), "")
		}},
		{"create link", func(f *stewardFixture, p principal) *httptest.ResponseRecorder {
			return f.jsonDo(p, http.MethodPost, linksPath(f.id), `{"ttlHours": 24}`)
		}},
		{"list links", func(f *stewardFixture, p principal) *httptest.ResponseRecorder {
			return f.jsonDo(p, http.MethodGet, linksPath(f.id), "")
		}},
		{"revoke link", func(f *stewardFixture, p principal) *httptest.ResponseRecorder {
			return f.jsonDo(p, http.MethodDelete, linksPath(f.id)+"/"+f.linkID, "")
		}},
		{"set expiry", func(f *stewardFixture, p principal) *httptest.ResponseRecorder {
			return f.jsonDo(p, http.MethodPatch, art(f), `{"expiresAt": "`+time.Now().Add(48*time.Hour).UTC().Format(time.RFC3339)+`"}`)
		}},
		{"clear expiry", func(f *stewardFixture, p principal) *httptest.ResponseRecorder {
			return f.jsonDo(p, http.MethodPatch, art(f), `{"expiresAt": null}`)
		}},
		{"move", func(f *stewardFixture, p principal) *httptest.ResponseRecorder {
			return f.jsonDo(p, http.MethodPatch, art(f), `{"scopeRef": "project-2"}`)
		}},
		{"move with expiry", func(f *stewardFixture, p principal) *httptest.ResponseRecorder {
			return f.jsonDo(p, http.MethodPatch, art(f), `{"scopeRef": "project-2", "expiresAt": null}`)
		}},
		{"delete", func(f *stewardFixture, p principal) *httptest.ResponseRecorder {
			return f.jsonDo(p, http.MethodDelete, art(f), "")
		}},
	}
}

// outcome is an expected status and error code ("" for a success).
type outcome struct {
	status int
	code   string
}

var (
	okCreated     = outcome{http.StatusCreated, ""}
	okOK          = outcome{http.StatusOK, ""}
	okNoContent   = outcome{http.StatusNoContent, ""}
	refused       = outcome{http.StatusForbidden, "forbidden"}
	stewardRefuse = outcome{http.StatusForbidden, CodeStewardActionNotAllowed}
)

// TestStewardBoundedTable: for each management action and each authority
// on an artifact owned by an agent, the status and code (design 4.2). The
// stewards share (view, review, edit; principals and projects), list,
// remove, link, change the expiry and delete, but every admin grant and
// every move answers 403 steward_action_not_allowed; an admin grantee may
// do everything; a home-project admin who is not a steward may only
// delete (custodian); a plain reader may do nothing.
func TestStewardBoundedTable(t *testing.T) {
	manager := map[string]outcome{
		"grant user read": okCreated, "grant user review": okCreated, "grant user write": okCreated,
		"grant user admin": okCreated, "grant agent read": okCreated, "grant agent admin": okCreated,
		"grant project read": okCreated, "grant project write": okCreated, "grant project admin": okCreated,
		"raise home to write": okOK, "lower admin grant to write": okOK, "raise to admin": okOK,
		"list grants": okOK, "remove grant": okNoContent, "create link": okCreated, "list links": okOK,
		"revoke link": okNoContent, "set expiry": okOK, "clear expiry": okOK, "move": okOK,
		"move with expiry": okOK, "delete": okNoContent,
	}
	steward := map[string]outcome{}
	for k, v := range manager {
		steward[k] = v
	}
	for _, k := range []string{"grant user admin", "grant agent admin", "grant project admin", "move", "move with expiry"} {
		steward[k] = stewardRefuse
	}
	// raise to admin re-puts the existing admin grant at admin: still an
	// admin grant from a steward, so refused.
	steward["raise to admin"] = stewardRefuse
	none := map[string]outcome{}
	for k := range manager {
		none[k] = refused
	}
	custodian := map[string]outcome{}
	for k := range manager {
		custodian[k] = refused
	}
	custodian["delete"] = okNoContent

	authorities := []struct {
		name string
		p    principal
		want map[string]outcome
	}{
		{"admin_grant", stAdminGrantee, manager},
		{"steward_project_owner", ownerSteward, steward},
		{"steward_delegator", delegSteward, steward},
		{"custodian_only", custodianP, custodian},
		{"plain_reader", plainReader, none},
	}
	for _, auth := range authorities {
		for _, act := range stewardActions() {
			t.Run(auth.name+"/"+act.name, func(t *testing.T) {
				f := newStewardFixture(t)
				want := auth.want[act.name]
				rec := act.do(f, auth.p)
				if rec.Code != want.status {
					t.Fatalf("status %d, want %d: %s", rec.Code, want.status, rec.Body.String())
				}
				if want.code != "" {
					if got := errCode(t, rec); got != want.code {
						t.Fatalf("code %q, want %q", got, want.code)
					}
				}
				if want == stewardRefuse {
					if !strings.Contains(rec.Body.String(), "publish your own copy") {
						t.Errorf("refusal text: %s", rec.Body.String())
					}
				}
			})
		}
	}
}

// TestStewardRefusalWritesNothing: a steward's refused admin grant or move
// changes nothing and records no event.
func TestStewardRefusalWritesNothing(t *testing.T) {
	for _, act := range stewardActions() {
		switch act.name {
		case "grant user admin", "grant project admin", "raise to admin", "move", "move with expiry":
		default:
			continue
		}
		t.Run(act.name, func(t *testing.T) {
			f := newStewardFixture(t)
			grants, _ := f.store.ListGrants(context.Background(), f.id)
			events := eventsOf(t, f.store, f.id)
			if rec := act.do(f, delegSteward); rec.Code != http.StatusForbidden {
				t.Fatalf("status %d", rec.Code)
			}
			after, _ := f.store.ListGrants(context.Background(), f.id)
			a, err := f.store.GetArtifact(context.Background(), f.id)
			if err != nil || a.ScopeRef != "project-1" || a.ExpiresAt != nil || len(after) != len(grants) {
				t.Fatalf("refusal changed the artifact: %+v %v grants %d -> %d", a, err, len(grants), len(after))
			}
			if got := eventsOf(t, f.store, f.id); len(got) != len(events) {
				t.Fatalf("refusal recorded events: %+v", got[len(events):])
			}
		})
	}
}

// TestStewardStoreRefusesBoundedActions: the store refuses an admin grant
// and a move from a steward actor even if a route did not.
func TestStewardStoreRefusesBoundedActions(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, reopen func() *sql.DB) {
		ctx := context.Background()
		a := seedOwned(t, st, PrincipalKindAgent, "agent-1")
		actor := &Actor{Kind: PrincipalKindUser, Ref: "u", Authority: AuthorityStewardDelegator, Expect: expectFor(a)}
		g := &Grant{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectPrincipal, SubjectRef: "user:x", Permission: GrantAdmin, CreatedAt: time.Now()}
		if _, err := st.PutGrant(ctx, g, 10, true, actor); err != ErrStewardActionNotAllowed {
			t.Errorf("steward admin grant: %v", err)
		}
		if _, err := st.UpdateArtifact(ctx, a.ID, ArtifactUpdate{HomeGrant: scopeGrant(a.ID, "project-2", GrantRead), MaxGrants: 10, Actor: actor}); err != ErrStewardActionNotAllowed {
			t.Errorf("steward move: %v", err)
		}
		got, _ := st.GetArtifact(ctx, a.ID)
		if got.ScopeRef != "project-1" || len(eventsOf(t, st, a.ID)) != 0 {
			t.Errorf("refused steward writes changed the artifact: %+v", got)
		}
	})
}

// TestStewardUserOwnedNeverSteward: on an artifact owned by a user, no
// steward clause applies, even with a host that would make every caller a
// steward; the host is not even asked. Only the owner (and admin grantees)
// administer it.
func TestStewardUserOwnedNeverSteward(t *testing.T) {
	f := newStewardFixture(t)
	f.host.stewardAll = StewardClauseProjectOwner
	userOwned := f.publish(userU, "mine.md", []byte("# mine"), "scope=project-1").Artifact.ID
	f.host.mu.Lock()
	f.host.calls = nil
	f.host.mu.Unlock()
	for _, p := range []principal{ownerSteward, delegSteward, plainReader} {
		if rec := f.jsonDo(p, http.MethodPost, grantsPath(userOwned), reviewBody(SubjectPrincipal, "user:outsider", GrantRead)); rec.Code != http.StatusForbidden || errCode(t, rec) != "forbidden" {
			t.Errorf("%s grant on a user-owned artifact: %d %s", p.ref, rec.Code, rec.Body.String())
		}
		if rec := f.jsonDo(p, http.MethodPost, linksPath(userOwned), ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s link on a user-owned artifact: %d", p.ref, rec.Code)
		}
		resp := decodeInto[ArtifactResponse](t, f.jsonDo(p, http.MethodGet, "/api/v1/artifacts/"+userOwned, ""))
		if resp.CanShare || resp.CanManage || resp.ShareAuthority != "" {
			t.Errorf("%s capabilities on a user-owned artifact: %+v", p.ref, resp)
		}
	}
	f.host.mu.Lock()
	for _, c := range f.host.calls {
		if strings.HasPrefix(c, "steward ") {
			t.Errorf("host asked %q for a user-owned artifact", c)
		}
	}
	f.host.mu.Unlock()
	// The owner still administers it.
	if rec := f.jsonDo(userU, http.MethodPost, grantsPath(userOwned), reviewBody(SubjectPrincipal, "user:outsider", GrantAdmin)); rec.Code != http.StatusCreated {
		t.Errorf("owner admin grant: %d %s", rec.Code, rec.Body.String())
	}
}

// TestStewardAgentsNeverSteward: an agent is never a steward, its own
// artifact included, even with a host that would answer a clause.
func TestStewardAgentsNeverSteward(t *testing.T) {
	f := newStewardFixture(t)
	f.host.stewardAll = StewardClauseDelegator
	for _, p := range []principal{agentA, agentB} {
		if rec := f.jsonDo(p, http.MethodPost, grantsPath(f.id), reviewBody(SubjectPrincipal, "user:outsider", GrantRead)); rec.Code != http.StatusForbidden {
			t.Errorf("%s grant: %d %s", p.ref, rec.Code, rec.Body.String())
		}
		if rec := f.jsonDo(p, http.MethodPost, linksPath(f.id), ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s link: %d", p.ref, rec.Code)
		}
		if rec := f.jsonDo(p, http.MethodPatch, "/api/v1/artifacts/"+f.id, `{"expiresAt": null}`); rec.Code != http.StatusForbidden {
			t.Errorf("%s patch: %d", p.ref, rec.Code)
		}
		if rec := f.jsonDo(p, http.MethodDelete, "/api/v1/artifacts/"+f.id, ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s delete: %d", p.ref, rec.Code)
		}
	}
}

// TestStewardNeedsCredential: a steward whose credential does not permit
// artifact.manage in the home project is not one.
func TestStewardNeedsCredential(t *testing.T) {
	f := newStewardFixture(t)
	f.host.deny(delegSteward, "project-1", PermissionManage)
	if rec := f.jsonDo(delegSteward, http.MethodPost, grantsPath(f.id), reviewBody(SubjectPrincipal, "user:outsider", GrantRead)); rec.Code != http.StatusForbidden {
		t.Fatalf("grant without the credential: %d %s", rec.Code, rec.Body.String())
	}
}

// TestStewardCapabilities: canShare and shareAuthority on a GET: a steward
// may share (canShare) but not manage, and learns its clause; an admin
// grantee may manage and share, with no clause; a custodian or reader has
// neither.
func TestStewardCapabilities(t *testing.T) {
	f := newStewardFixture(t)
	for _, tc := range []struct {
		p                   principal
		canManage, canShare bool
		authority           string
	}{
		{ownerSteward, false, true, StewardClauseProjectOwner},
		{delegSteward, false, true, StewardClauseDelegator},
		{stAdminGrantee, true, true, ""},
		{custodianP, false, false, ""},
		{plainReader, false, false, ""},
	} {
		resp := decodeInto[ArtifactResponse](t, f.jsonDo(tc.p, http.MethodGet, "/api/v1/artifacts/"+f.id, ""))
		if resp.CanManage != tc.canManage || resp.CanShare != tc.canShare || resp.ShareAuthority != tc.authority {
			t.Errorf("%s: canManage %v canShare %v shareAuthority %q; want %v %v %q", tc.p.ref,
				resp.CanManage, resp.CanShare, resp.ShareAuthority, tc.canManage, tc.canShare, tc.authority)
		}
	}
	// A version GET answers the same.
	resp := decodeInto[ArtifactResponse](t, f.jsonDo(delegSteward, http.MethodGet, "/api/v1/artifacts/"+f.id+"/versions/1", ""))
	if !resp.CanShare || resp.CanManage || resp.ShareAuthority != StewardClauseDelegator {
		t.Errorf("version GET: %+v", resp)
	}
}

// TestStewardEvents: every grant creation, level change and link creation
// by a steward writes an event naming the actor and the steward clause,
// and neither the event nor the decision log carries the link token or
// its hash.
func TestStewardEvents(t *testing.T) {
	f := newStewardFixture(t)
	c := captureLog(f.fixture, slog.LevelDebug)
	before := len(eventsOf(t, f.store, f.id))

	if rec := f.jsonDo(delegSteward, http.MethodPost, grantsPath(f.id), reviewBody(SubjectPrincipal, "user:outsider", GrantRead)); rec.Code != http.StatusCreated {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.jsonDo(ownerSteward, http.MethodPost, grantsPath(f.id), reviewBody(SubjectPrincipal, "user:outsider", GrantWrite)); rec.Code != http.StatusOK {
		t.Fatalf("change: %d %s", rec.Code, rec.Body.String())
	}
	// The same level again is a no-op: no event and no decision log entry.
	c.take(t)
	if rec := f.jsonDo(ownerSteward, http.MethodPost, grantsPath(f.id), reviewBody(SubjectPrincipal, "user:outsider", GrantWrite)); rec.Code != http.StatusOK {
		t.Fatalf("repeat: %d %s", rec.Code, rec.Body.String())
	}
	if entries, _ := c.take(t); len(entries) != 0 {
		t.Errorf("a no-op put was logged: %v", entries)
	}
	rec := f.jsonDo(delegSteward, http.MethodPost, linksPath(f.id), `{"ttlHours": 2}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("link: %d %s", rec.Code, rec.Body.String())
	}
	link := decodeInto[CreateLinkResponse](t, rec)
	token := strings.TrimPrefix(link.URL, RouteShared)

	evs := eventsOf(t, f.store, f.id)[before:]
	if len(evs) != 3 {
		t.Fatalf("events = %+v; want grant_created, grant_changed, link_created", evs)
	}
	want := []struct{ action, ref, authority string }{
		{EventGrantCreated, delegSteward.ref, AuthorityStewardDelegator},
		{EventGrantChanged, ownerSteward.ref, AuthorityStewardProjectOwner},
		{EventLinkCreated, delegSteward.ref, AuthorityStewardDelegator},
	}
	for i, w := range want {
		e := evs[i]
		if e.Action != w.action || e.ActorKind != PrincipalKindUser || e.ActorRef != w.ref || e.Authority != w.authority {
			t.Errorf("event %d = %+v; want %+v", i, e, w)
		}
	}
	if d := evs[0].Detail; d["subjectRef"] != "user:outsider" || d["permission"] != GrantRead || d["grantId"] == "" {
		t.Errorf("grant_created detail = %+v", d)
	}
	if d := evs[1].Detail; d["oldPermission"] != GrantRead || d["newPermission"] != GrantWrite {
		t.Errorf("grant_changed detail = %+v", d)
	}
	if d := evs[2].Detail; d["linkId"] != link.Link.ID || d["expiresAt"] == nil || len(d) != 2 {
		t.Errorf("link_created detail = %+v", d)
	}
	raw, _ := json.Marshal(evs)
	_, logged := c.take(t)
	for _, secret := range []string{token, LinkTokenHash(token)} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("an event carries the link token or its hash: %s", raw)
		}
		if strings.Contains(logged, secret) {
			t.Errorf("the decision log carries the link token or its hash: %s", logged)
		}
	}
}

// TestStewardDecisionLog: an allowed steward change is logged with its
// steward authority, and a refused admin grant or move with the reason
// steward_action_not_allowed.
func TestStewardDecisionLog(t *testing.T) {
	f := newStewardFixture(t)
	c := captureLog(f.fixture, slog.LevelDebug)
	if rec := f.jsonDo(ownerSteward, http.MethodPost, grantsPath(f.id), reviewBody(SubjectScope, "project-3", GrantRead)); rec.Code != http.StatusCreated {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body.String())
	}
	entries, _ := c.take(t)
	checkEntry(t, "steward grant", one(t, "steward grant", entries), "INFO", map[string]any{
		logKeyAction: logActionGrantCreated, logKeyAuthority: AuthorityStewardProjectOwner, logKeyPrincipalID: ownerSteward.ref,
	})
	for _, tc := range []struct {
		method, path, body, action string
	}{
		{http.MethodPost, grantsPath(f.id), reviewBody(SubjectPrincipal, "user:outsider", GrantAdmin), logActionShare},
		{http.MethodPatch, "/api/v1/artifacts/" + f.id, `{"scopeRef": "project-2"}`, logActionMove},
	} {
		if rec := f.jsonDo(delegSteward, tc.method, tc.path, tc.body); rec.Code != http.StatusForbidden {
			t.Fatalf("%s: %d", tc.action, rec.Code)
		}
		entries, _ := c.take(t)
		checkEntry(t, "refused "+tc.action, one(t, tc.action, entries), "WARN", map[string]any{
			logKeyAction: tc.action, logKeyOutcome: logOutcomeRefused, logKeyReason: logReasonStewardActionNotAllowed,
		})
	}
}

// TestStewardShareReachesOutsider: a steward gives a user outside the home
// project Can view, and that user can then open the artifact; the share
// link a steward creates works signed out.
func TestStewardShareReachesOutsider(t *testing.T) {
	for _, p := range []principal{ownerSteward, delegSteward} {
		t.Run(p.ref, func(t *testing.T) {
			f := newStewardFixture(t)
			if rec := f.jsonDo(outside, http.MethodGet, "/api/v1/artifacts/"+f.id, ""); rec.Code != http.StatusNotFound {
				t.Fatalf("outsider before the share: %d", rec.Code)
			}
			if rec := f.jsonDo(p, http.MethodPost, grantsPath(f.id), reviewBody(SubjectPrincipal, PrincipalRef(outside.kind, outside.ref), GrantRead)); rec.Code != http.StatusCreated {
				t.Fatalf("share: %d %s", rec.Code, rec.Body.String())
			}
			if rec := f.jsonDo(outside, http.MethodGet, "/api/v1/artifacts/"+f.id, ""); rec.Code != http.StatusOK {
				t.Fatalf("outsider after the share: %d", rec.Code)
			}
			rec := f.jsonDo(p, http.MethodPost, linksPath(f.id), "")
			if rec.Code != http.StatusCreated {
				t.Fatalf("link: %d %s", rec.Code, rec.Body.String())
			}
			link := decodeInto[CreateLinkResponse](t, rec)
			if rec := f.do(nil, http.MethodGet, link.URL, nil, nil); rec.Code != http.StatusSeeOther {
				t.Fatalf("signed-out link: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestStewardCrossProjectSwitch: a steward's grant to another project
// follows the cross-project sharing switch exactly as an admin's does.
func TestStewardCrossProjectSwitch(t *testing.T) {
	f := newStewardFixture(t)
	f.host.mu.Lock()
	f.host.crossScope = false
	f.host.mu.Unlock()
	rec := f.jsonDo(delegSteward, http.MethodPost, grantsPath(f.id), reviewBody(SubjectScope, "project-3", GrantWrite))
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "cross_project_sharing_disabled" {
		t.Fatalf("cross-project off: %d %s", rec.Code, rec.Body.String())
	}
	f.host.mu.Lock()
	f.host.crossScope = true
	f.host.mu.Unlock()
	if rec := f.jsonDo(delegSteward, http.MethodPost, grantsPath(f.id), reviewBody(SubjectScope, "project-3", GrantWrite)); rec.Code != http.StatusCreated {
		t.Fatalf("cross-project on: %d %s", rec.Code, rec.Body.String())
	}
}

// TestStewardReviewGrantTakesManagerPath pins a phase 1 side effect: a
// steward's review grant is served by the steward (manager) path, not the
// review-grant path (design D24), so it does not check that the user can
// already read the artifact, and replacing a read grant answers the
// manager path's 400 review_replaces_access.
func TestStewardReviewGrantTakesManagerPath(t *testing.T) {
	f := newStewardFixture(t)
	f.host.allowReview(delegSteward, agentA.ref, "project-1")
	f.host.mu.Lock()
	f.host.calls = nil
	f.host.mu.Unlock()
	rec := f.jsonDo(delegSteward, http.MethodPost, grantsPath(f.id), reviewBody(SubjectPrincipal, "user:cannot-read", GrantReview))
	if rec.Code != http.StatusCreated {
		t.Fatalf("steward review grant to a non-reader: %d %s", rec.Code, rec.Body.String())
	}
	f.host.mu.Lock()
	for _, c := range f.host.calls {
		if strings.HasPrefix(c, "review ") || strings.HasPrefix(c, "subject ") {
			t.Errorf("the review-grant path was asked: %q", c)
		}
	}
	f.host.mu.Unlock()
	if e := eventsOf(t, f.store, f.id); len(e) == 0 || e[len(e)-1].Action != EventGrantCreated || e[len(e)-1].Authority != AuthorityStewardDelegator {
		t.Errorf("last event = %+v; want grant_created by steward_delegator", e)
	}
	if rec := f.jsonDo(delegSteward, http.MethodPost, grantsPath(f.id), reviewBody(SubjectPrincipal, "user:outsider", GrantRead)); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	rec = f.jsonDo(delegSteward, http.MethodPost, grantsPath(f.id), reviewBody(SubjectPrincipal, "user:outsider", GrantReview))
	if rec.Code != http.StatusBadRequest || errCode(t, rec) != CodeReviewReplacesAccess {
		t.Fatalf("steward review over a read grant: %d %s", rec.Code, rec.Body.String())
	}
}

// TestStewardNotFoundParity: a caller who cannot read the artifact gets
// the missing artifact's 404 on every management route, even when the
// host would make it a steward.
func TestStewardNotFoundParity(t *testing.T) {
	f := newStewardFixture(t)
	stranger := principal{PrincipalKindUser, "user-stranger", ""}
	f.host.allow(stranger, "project-2", PermissionCreate)
	f.host.allowSteward(stranger, agentA.ref, "project-1", StewardClauseDelegator)
	missing := uuid.NewString()
	for _, act := range stewardActions() {
		got := act.do(f, stranger)
		real := f.id
		f.id = missing
		want := act.do(f, stranger)
		f.id = real
		if got.Code != http.StatusNotFound || got.Body.String() != want.Body.String() {
			t.Errorf("%s: %d %s; missing artifact answers %d %s", act.name, got.Code, got.Body.String(), want.Code, want.Body.String())
		}
	}
}

// TestReviewGrantEventAuthority: a review grant on the review-grant path
// (design D24, which serves callers who are not stewards) records a
// grant_created event: a home-project admin as
// reviewGrantProjectAdminAuthority, the delegating user as
// reviewGrantDelegatorAuthority (never a steward authority: the path serves
// only callers who are not stewards).
func TestReviewGrantEventAuthority(t *testing.T) {
	f, id := newReviewGrantFixture(t)
	f.host.allowAdmin(projAdmin, "project-1")
	memberID := PrincipalRef(member.kind, member.ref)
	for _, tc := range []struct {
		caller    principal
		subject   string
		authority string
	}{
		{projAdmin, memberID, reviewGrantProjectAdminAuthority},
		{delegator, reviewerID, reviewGrantDelegatorAuthority},
	} {
		if rec := f.postGrantBody(tc.caller, id, reviewBody(SubjectPrincipal, tc.subject, GrantReview)); rec.Code != http.StatusCreated {
			t.Fatalf("review grant by %s: %d %s", tc.caller.ref, rec.Code, rec.Body.String())
		}
		evs := eventsOf(t, f.store, id)
		if len(evs) == 0 {
			t.Fatalf("review grant by %s recorded no event", tc.caller.ref)
		}
		e := evs[len(evs)-1]
		if e.Action != EventGrantCreated || e.ActorRef != tc.caller.ref || e.Authority != tc.authority || e.Detail["subjectRef"] != tc.subject {
			t.Errorf("review grant by %s: event %+v, want authority %s", tc.caller.ref, e, tc.authority)
		}
	}
}

// laxStore stands for a store without the steward bounds: it records a
// steward's admin grant or move as if an owner made it. With it, only the
// routes' own gates stand between a steward and those actions.
type laxStore struct{ Store }

func laxActor(actor *Actor) *Actor {
	if actor == nil || !isSteward(actor.Authority) {
		return actor
	}
	return &Actor{Kind: actor.Kind, Ref: actor.Ref, Authority: AuthorityAdminGrant}
}

func (s laxStore) PutGrant(ctx context.Context, g *Grant, max int, cross bool, actor *Actor) (GrantPut, error) {
	return s.Store.PutGrant(ctx, g, max, cross, laxActor(actor))
}

func (s laxStore) UpdateArtifact(ctx context.Context, id string, u ArtifactUpdate) (*Artifact, error) {
	u.Actor = laxActor(u.Actor)
	return s.Store.UpdateArtifact(ctx, id, u)
}

// TestStewardHandlerGates: the routes refuse a steward's admin grant and
// move themselves, before the store is asked (the store's own refusal is
// TestStewardStoreRefusesBoundedActions).
func TestStewardHandlerGates(t *testing.T) {
	for _, act := range stewardActions() {
		switch act.name {
		case "grant user admin", "grant agent admin", "grant project admin", "raise to admin", "move", "move with expiry":
		default:
			continue
		}
		t.Run(act.name, func(t *testing.T) {
			f := newStewardFixture(t)
			f.svc.SetStore(laxStore{f.store})
			rec := act.do(f, ownerSteward)
			if rec.Code != http.StatusForbidden || errCode(t, rec) != CodeStewardActionNotAllowed {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}
