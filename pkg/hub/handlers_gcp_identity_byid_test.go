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

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// Coverage for /api/v1/gcp-service-accounts/{id} -- the flat by-id route (P5).
//
// The route exists because a hub-scoped account has no project, so the nested
// address can only reach one by borrowing an unrelated project's ID. What most
// of this file actually tests is the OTHER half of that: that adding a
// project-free address did not accidentally create a project-free way to reach
// accounts that already have a project, or a first-ever way to reach accounts
// that were never meant to be addressable at all.
//
// Nothing here touches live GCP. Verification uses a mock token generator
// whose VerifyImpersonation always succeeds.

const flatSAPath = "/api/v1/gcp-service-accounts/"

func mkSA(t *testing.T, s store.Store, idName, email, scope, scopeID, createdBy string) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID:        tid(idName),
		Scope:     scope,
		ScopeID:   scopeID,
		Email:     email,
		ProjectID: "gcp-proj",
		CreatedBy: createdBy,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), sa))
	return sa
}

func decodeSAWithCaps(t *testing.T, body []byte) GCPServiceAccountWithCapabilities {
	t.Helper()
	var got GCPServiceAccountWithCapabilities
	require.NoError(t, json.Unmarshal(body, &got))
	return got
}

func saExists(t *testing.T, s store.Store, id string) bool {
	t.Helper()
	_, err := s.GetGCPServiceAccount(context.Background(), id)
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	require.NoError(t, err)
	return true
}

// The route's reason for existing: an ordinary hub member can read a hub-scoped
// account without naming a project. Read by a plain member rather than the
// creator on purpose -- the creator would pass through the owner short-circuit
// in CheckAccess and prove nothing about hub scope.
func TestGCPSA_FlatByID_Get_HubScoped(t *testing.T) {
	srv, s, _, member, _, _ := setupGCPAuthzTest(t)

	sa := mkSA(t, s, "sa-flat-hub", "hubwide@p.iam.gserviceaccount.com",
		store.ScopeHub, "hub-instance-1", tid("user-stranger"))

	rec := doRequestAsUser(t, srv, member, http.MethodGet, flatSAPath+sa.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code,
		"a hub member must be able to read a hub-scoped SA without naming a project; got: %s",
		rec.Body.String())

	got := decodeSAWithCaps(t, rec.Body.Bytes())
	require.Equal(t, sa.ID, got.ID)
	require.Equal(t, store.ScopeHub, got.Scope,
		"scope must survive the response; every client that distinguishes hub-scoped from "+
			"project-scoped reads this field")
}

// THE CONDITION-1 TEST. A project-scoped account is not found here, and the
// assertion is paired with a nested read by the SAME user to show what is being
// measured: the account exists and this caller may read it, so the 404 is the
// route refusing an address, not the authorization layer refusing a caller.
//
// If someone later "fixes" this to a 403 for consistency with the nested
// route's error style, they will have turned the flat route into an existence
// oracle: it takes no project, so a caller who knows nothing can probe any ID
// and learn which ones are real.
func TestGCPSA_FlatByID_Get_ProjectScopedIs404NotForbidden(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)

	sa := mkSA(t, s, "sa-flat-project", "proj@p.iam.gserviceaccount.com",
		store.ScopeProject, project.ID, owner.ID)

	rec := doRequestAsUser(t, srv, owner, http.MethodGet, flatSAPath+sa.ID, nil)
	require.Equal(t, http.StatusNotFound, rec.Code,
		"a project-scoped SA has a nested address and must not gain a second, project-free "+
			"one; got: %s", rec.Body.String())

	rec = doRequestAsUser(t, srv, owner, http.MethodGet,
		"/api/v1/projects/"+project.ID+"/gcp-service-accounts/"+sa.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code,
		"the same user reading the same account nested must still succeed -- otherwise the "+
			"404 above is measuring permission, not routing; got: %s", rec.Body.String())
}

// A 404 must not be reachable by way of having deleted the thing first.
func TestGCPSA_FlatByID_Delete_ProjectScopedIs404AndDoesNotDelete(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)

	sa := mkSA(t, s, "sa-flat-project-del", "projdel@p.iam.gserviceaccount.com",
		store.ScopeProject, project.ID, owner.ID)

	rec := doRequestAsUser(t, srv, owner, http.MethodDelete, flatSAPath+sa.ID, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.True(t, saExists(t, s, sa.ID),
		"the refusal must happen before the delete, not be reported after it")
}

// USER SCOPE, the case sa-arch asked for explicitly. The creator can read their
// own account through this route -- which is the first HTTP address a
// user-scoped account has ever had, since one is not reachable from any
// project and the nested route therefore always 404'd it.
func TestGCPSA_FlatByID_Get_UserScoped_CreatorCanRead(t *testing.T) {
	srv, s, _, member, _, _ := setupGCPAuthzTest(t)

	sa := mkSA(t, s, "sa-flat-user", "mine@p.iam.gserviceaccount.com",
		store.ScopeUser, member.ID, member.ID)

	rec := doRequestAsUser(t, srv, member, http.MethodGet, flatSAPath+sa.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code,
		"the creator of a user-scoped SA must be able to read it; got: %s", rec.Body.String())
	require.Equal(t, store.ScopeUser, decodeSAWithCaps(t, rec.Body.Bytes()).Scope)
}

// The other half, and the one that matters. 404 rather than 403 is deliberate
// and is an extension of sa-arch's condition 1 rather than the condition
// itself: the user arm of authorizeGCPServiceAccount would answer 403, and
// since this route is the first address a user-scoped account has ever had,
// that 403 would be a NEW existence oracle rather than an inherited one.
//
// It costs nothing in authority. That arm already denies every caller but the
// creator, so this changes the status seen by callers who were refused either
// way. If the ruling comes back the other way, only the status changes -- do
// not also relax who may read.
func TestGCPSA_FlatByID_Get_UserScoped_OtherUserIs404NotForbidden(t *testing.T) {
	srv, s, owner, member, _, _ := setupGCPAuthzTest(t)

	sa := mkSA(t, s, "sa-flat-user-other", "theirs@p.iam.gserviceaccount.com",
		store.ScopeUser, member.ID, member.ID)

	rec := doRequestAsUser(t, srv, owner, http.MethodGet, flatSAPath+sa.ID, nil)
	require.Equal(t, http.StatusNotFound, rec.Code,
		"another user's user-scoped SA must not be confirmed to exist; got: %s", rec.Body.String())
}

// THE DISCLOSURE RULE, ASSERTED AS A DIVERGENCE RATHER THAN AS TWO FACTS.
//
// The rule (sa-arch) is: render 404 when the caller could not otherwise have
// established that the account exists, 403 when they could. Hub and user scope
// come out on opposite sides of it, and this test exists so that the difference
// reads as deliberate. Written as one test on purpose -- as two, a later
// "consistency" cleanup could align them without anything looking wrong.
//
// HUB -> 403, because existence is already establishable: every user is joined
// to hub-members on login and hub-member-read-all grants read+list at hub
// scope, so any authenticated caller can enumerate hub-scoped accounts. There
// is nothing left to protect and a 404 would only cost debuggability.
//
// USER -> 404, because nothing makes existence establishable: user-scoped
// accounts are reachable from no project, so this route is the first address
// one has ever had.
//
// IF THE HUB ARM STARTS FAILING WITH A 404, do not "fix" it here. Check whether
// hub-member-read-all still grants list at hub scope -- if #19's admin-gating
// of hub-scoped creation narrowed it, then 404 is the newly correct answer and
// this test should be updated to say so, with the reason.
func TestGCPSA_FlatByID_DisclosureRule_HubIs403_UserIs404(t *testing.T) {
	srv, s, owner, member, _, _ := setupGCPAuthzTest(t)

	hubSA := mkSA(t, s, "sa-rule-hub", "rulehub@p.iam.gserviceaccount.com",
		store.ScopeHub, "hub-instance-1", tid("user-stranger-rule"))
	userSA := mkSA(t, s, "sa-rule-user", "ruleuser@p.iam.gserviceaccount.com",
		store.ScopeUser, member.ID, member.ID)

	// Same caller, same action, both denied by policy -- only the disclosure
	// answer differs.
	hubRec := doRequestAsUser(t, srv, owner, http.MethodDelete, flatSAPath+hubSA.ID, nil)
	userRec := doRequestAsUser(t, srv, owner, http.MethodDelete, flatSAPath+userSA.ID, nil)

	// THE DIVERGENCE IS THE SUBJECT, asserted before either specific code. For
	// a REASONED refusal the disclosure answer tracks establishability, so these
	// two must not converge -- in either direction. (Contrast the noIdentity
	// refusal, where the subject is the SAMENESS: see the #45 tests below. Same
	// idiom, opposite requirement, and the difference is who is refused.)
	require.NotEqual(t, hubRec.Code, userRec.Code,
		"hub-scoped and user-scoped refusals of an IDENTIFIED caller must disclose differently; "+
			"both gave %d", hubRec.Code)

	require.Equal(t, http.StatusForbidden, hubRec.Code,
		"hub-scoped: any authenticated caller can already list these, so a 404 would conceal "+
			"nothing and only make the refusal harder to diagnose; got: %s", hubRec.Body.String())

	require.Equal(t, http.StatusNotFound, userRec.Code,
		"user-scoped: no other surface exposes these, so a 403 would be an existence oracle "+
			"this route invented; got: %s", userRec.Body.String())

	require.True(t, saExists(t, s, hubSA.ID))
	require.True(t, saExists(t, s, userSA.ID))
}

// The verdict/renderer split must not have changed what the NESTED routes
// answer to a REASONED refusal -- an identified caller who was evaluated
// against a policy and lost. Such a caller has already named the project, so a
// 403 discloses nothing they did not supply themselves.
//
// This is the regression guard on the refactor, not a new requirement. If it
// fails, the split leaked the flat route's disclosure policy into the nested
// one -- which would turn existing 403s into 404s and silently change what
// every current client sees.
//
// "REASONED" IS LOAD-BEARING AND WAS ADDED BY #45. Design §8.4 asked for a
// guard that "the nested routes still render 403", without the qualifier, and
// this test was written to that sentence. But an identity-less caller is also
// refused by these routes, and for that caller 403 is an existence-and-scope
// oracle -- see TestGCPSA_NestedByID_NoIdentity_TheFourRowsAreIndistinguishable
// below, and §8.5, where sa-arch records the missing qualifier as a spec
// defect. Both callers below are identified, which is why this test is
// unaffected by that fix and still means what it meant.
func TestGCPSA_Nested_StillRenders403AfterVerdictSplit(t *testing.T) {
	srv, s, owner, member, _, project := setupGCPAuthzTest(t)

	projSA := mkSA(t, s, "sa-nested-403", "n403@p.iam.gserviceaccount.com",
		store.ScopeProject, project.ID, owner.ID)
	hubSA := mkSA(t, s, "sa-nested-403-hub", "n403hub@p.iam.gserviceaccount.com",
		store.ScopeHub, "hub-instance-1", tid("user-stranger-nested"))

	rec := doRequestAsUser(t, srv, member, http.MethodDelete,
		"/api/v1/projects/"+project.ID+"/gcp-service-accounts/"+projSA.ID, nil)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"project-scoped nested delete by a non-owner member must still be 403; got: %s",
		rec.Body.String())

	rec = doRequestAsUser(t, srv, member, http.MethodDelete,
		"/api/v1/projects/"+project.ID+"/gcp-service-accounts/"+hubSA.ID, nil)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"hub-scoped nested delete by a plain member must still be 403; got: %s", rec.Body.String())

	require.True(t, saExists(t, s, projSA.ID))
	require.True(t, saExists(t, s, hubSA.ID))
}

// Capabilities are computed, not implied by the response having a body. A plain
// hub member can read a hub-scoped account but cannot delete it, and the
// response has to say so -- a client that renders Delete from the account being
// visible produces a button that 403s on click, and for hub-scoped accounts
// that caller is the common case rather than the edge one.
func TestGCPSA_FlatByID_Get_CapabilitiesReflectAuthorityNotExistence(t *testing.T) {
	srv, s, _, member, _, _ := setupGCPAuthzTest(t)

	sa := mkSA(t, s, "sa-flat-caps", "caps@p.iam.gserviceaccount.com",
		store.ScopeHub, "hub-instance-1", tid("user-stranger-caps"))

	rec := doRequestAsUser(t, srv, member, http.MethodGet, flatSAPath+sa.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got := decodeSAWithCaps(t, rec.Body.Bytes())
	require.NotNil(t, got.Cap,
		"the detail response must carry capabilities; without them a client has nothing to "+
			"render affordances from except existence")
	require.Contains(t, got.Cap.Actions, string(ActionRead))
	require.NotContains(t, got.Cap.Actions, string(ActionDelete),
		"a plain hub member can read a hub-scoped SA but must not be told they can delete it")

	// And the capability is not merely advisory: the operation it withholds is
	// actually refused. A capabilities list that disagrees with the endpoint is
	// worse than none, because clients trust it.
	rec = doRequestAsUser(t, srv, member, http.MethodDelete, flatSAPath+sa.ID, nil)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"the withheld capability must match the endpoint's answer; got: %s", rec.Body.String())
	require.True(t, saExists(t, s, sa.ID))
}

// THE CREATOR GRANT, recorded as behaviour rather than left as a footnote
// (sa-arch condition 3). A non-admin who created a hub-wide credential can
// destroy it, because gcpServiceAccountResource sets OwnerID from CreatedBy and
// CheckAccess short-circuits for an owner. The nested DELETE has the same reach
// today; this route does not widen it.
//
// If hub-scoped creation is ever restricted to admins, this stops being a live
// exposure and becomes history -- but it should still pass, and the reason it
// passes should still be the owner grant.
func TestGCPSA_FlatByID_Delete_HubScoped_CreatorCan(t *testing.T) {
	srv, s, _, member, _, _ := setupGCPAuthzTest(t)

	sa := mkSA(t, s, "sa-flat-hub-mine", "minehub@p.iam.gserviceaccount.com",
		store.ScopeHub, "hub-instance-1", member.ID)

	rec := doRequestAsUser(t, srv, member, http.MethodDelete, flatSAPath+sa.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code,
		"the creator of a hub-scoped SA can delete it -- non-admin, hub-wide credential; got: %s",
		rec.Body.String())
	require.False(t, saExists(t, s, sa.ID))
}

// Verify re-runs through the same body as the nested route. Asserted on the
// persisted record, not just the response, because the assign gate reads the
// stored verification state and not what an HTTP response once said.
func TestGCPSA_FlatByID_Verify_HubScoped(t *testing.T) {
	srv, s, _, member, _, _ := setupGCPAuthzTest(t)

	// The verify handler fails closed when no token generator is configured
	// (503 "GCP token generation is not configured on this Hub"). Supply a
	// mock so the handler reaches the verification path and succeeds.
	srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub-sa@test.iam.gserviceaccount.com"})

	sa := mkSA(t, s, "sa-flat-verify", "verify@p.iam.gserviceaccount.com",
		store.ScopeHub, "hub-instance-1", member.ID)
	require.False(t, sa.Verified)

	rec := doRequestAsUser(t, srv, member, http.MethodPost, flatSAPath+sa.ID+"/verify", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	stored, err := s.GetGCPServiceAccount(context.Background(), sa.ID)
	require.NoError(t, err)
	require.True(t, stored.Verified,
		"verification must be persisted, not only reported -- the assign gate reads the store")
	require.Equal(t, store.GCPVerificationVerified, stored.VerificationStatus)
}

// Verify on a project-scoped account is 404 here too. Worth its own case
// because verify is the one flat operation with a sub-path, so it is the one
// most likely to be wired up bypassing the shared loader.
func TestGCPSA_FlatByID_Verify_ProjectScopedIs404(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)

	sa := mkSA(t, s, "sa-flat-verify-proj", "vproj@p.iam.gserviceaccount.com",
		store.ScopeProject, project.ID, owner.ID)

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, flatSAPath+sa.ID+"/verify", nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// sa-arch's condition 4: no mint and no create on the by-id route.
//
// The mint case is not hypothetical pedantry. The nested dispatcher treats
// "mint" as a COLLECTION-level action on the same path segment an ID occupies,
// so the obvious way to write this handler -- by copying that one -- gives the
// flat route a mint endpoint at hub scope, where the per-project mint quota it
// is built around does not exist. This pins that the copy did not happen.
func TestGCPSA_FlatByID_NoMintNoCreate(t *testing.T) {
	srv, s, _, member, _, _ := setupGCPAuthzTest(t)

	sa := mkSA(t, s, "sa-flat-nomint", "nomint@p.iam.gserviceaccount.com",
		store.ScopeHub, "hub-instance-1", member.ID)

	before, err := s.CountGCPServiceAccounts(context.Background(), store.GCPServiceAccountFilter{})
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, member, http.MethodPost, flatSAPath+"mint",
		map[string]any{"account_id": "should-not-exist"})
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code,
		"on the flat by-id route \"mint\" parses as an account ID with no action, not as the "+
			"collection-level action the NESTED dispatcher makes it, so POST to it is simply a "+
			"method that members do not accept; got: %s", rec.Body.String())

	rec = doRequestAsUser(t, srv, member, http.MethodPost, flatSAPath+sa.ID, map[string]any{})
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code,
		"creation belongs to the collection route; got: %s", rec.Body.String())

	after, err := s.CountGCPServiceAccounts(context.Background(), store.GCPServiceAccountFilter{})
	require.NoError(t, err)
	require.Equal(t, before, after,
		"neither refusal may have created an account on the way to being refused")
}

// An unknown action is a 404 rather than a 405. The distinction is that the
// method is fine and the sub-path is not, and a 405 would advertise which
// methods a nonexistent endpoint accepts.
func TestGCPSA_FlatByID_UnknownAction(t *testing.T) {
	srv, s, _, member, _, _ := setupGCPAuthzTest(t)

	sa := mkSA(t, s, "sa-flat-unknown", "unknown@p.iam.gserviceaccount.com",
		store.ScopeHub, "hub-instance-1", member.ID)

	rec := doRequestAsUser(t, srv, member, http.MethodPost, flatSAPath+sa.ID+"/impersonate", nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// A nonexistent ID and a project-scoped ID must be indistinguishable. Asserted
// on the body, not just the status: a message naming the scope would leak
// exactly what the 404 is there to withhold.
func TestGCPSA_FlatByID_RefusalsAreIndistinguishable(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)

	real := mkSA(t, s, "sa-flat-indist", "indist@p.iam.gserviceaccount.com",
		store.ScopeProject, project.ID, owner.ID)

	realRec := doRequestAsUser(t, srv, owner, http.MethodGet, flatSAPath+real.ID, nil)
	fakeRec := doRequestAsUser(t, srv, owner, http.MethodGet, flatSAPath+tid("sa-does-not-exist"), nil)

	require.Equal(t, http.StatusNotFound, realRec.Code)
	require.Equal(t, http.StatusNotFound, fakeRec.Code)
	require.Equal(t, fakeRec.Body.String(), realRec.Body.String(),
		"an existing-but-out-of-scope account must be indistinguishable from one that does not "+
			"exist; a differing body is an existence oracle spelled slightly more quietly")
}

// The flat COLLECTION route keeps its exact-match registration. Registering a
// subtree next to it is the kind of change that silently swallows the
// collection, and the failure would look like the list route vanishing.
func TestGCPSA_FlatByID_CollectionRouteStillReachable(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	_, hub := seedListMix(t, ctx, s, owner, project)

	emails := topLevelSAEmails(t, srv, owner, "scope=hub")
	require.Contains(t, emails, hub,
		"adding /api/v1/gcp-service-accounts/ must not shadow /api/v1/gcp-service-accounts")
}

// #42. THE FLAT RENDERER LEAKED WHICH ARM IT WOULD HAVE HIT, to a caller that
// had established nothing at all. Found by sa-arch reviewing acc4285d.
//
// gcpSAVerdict.noIdentity exists, in its own doc's words, "so a renderer can
// answer it generically instead of leaking which arm it would have hit". The
// nested renderer honoured that; the flat one checked allowed and fell straight
// into the scope switch, which answers 403 for hub scope and 404 for user
// scope. An identity-less caller could therefore separate the two by status.
//
// AND THAT CALLER IS ORDINARY. GetUserIdentityFromContext returns nil for an
// AGENT, and agents authenticate perfectly well, so every authenticated agent
// reaching this route took the leaking path. The hub arm's 403 rests on "every
// user is joined to hub-members on login" -- which is FALSE FOR AGENTS, whose
// principals never include hub-members. The disclosure was being granted on a
// premise explicitly false for the caller receiving it.
//
// ONE TEST ASSERTING THE TWO STATUSES ARE NOW IDENTICAL, not two tests each
// pinning 404. Two such tests can both be made green by a change that reopens
// the divergence in the other direction, or by a cleanup that harmonises one to
// whatever the code happens to do; a test whose subject IS the sameness cannot.
func TestGCPSA_FlatByID_NoIdentity_HubAndUserAreIndistinguishable(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	agent := &store.Agent{
		ID:           tid("agent-flat-sa-probe"),
		Slug:         "flat-sa-probe",
		Name:         "Flat SA Probe",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	agentToken, err := srv.agentTokenService.GenerateAgentToken(agent.ID, project.ID, nil, nil)
	require.NoError(t, err)

	hubSA := mkSA(t, s, "sa-flat-noid-hub", "hubwide@p.iam.gserviceaccount.com",
		store.ScopeHub, "hub-instance-1", tid("user-stranger"))
	userSA := mkSA(t, s, "sa-flat-noid-user", "personal@p.iam.gserviceaccount.com",
		store.ScopeUser, tid("user-stranger"), tid("user-stranger"))

	hubRec := doRequestWithAgentToken(t, srv, http.MethodGet, flatSAPath+hubSA.ID, nil, agentToken)
	userRec := doRequestWithAgentToken(t, srv, http.MethodGet, flatSAPath+userSA.ID, nil, agentToken)

	require.Equal(t, hubRec.Code, userRec.Code,
		"an identity-less caller must not be able to tell a hub-scoped account from a user-scoped "+
			"one by status: hub gave %d, user gave %d", hubRec.Code, userRec.Code)

	// And the shared answer is the one that reveals least. Asserted after the
	// sameness, not instead of it -- sameness at 403 would be a different bug.
	require.Equal(t, http.StatusNotFound, hubRec.Code,
		"a caller with no identity has established nothing anywhere; got: %s", hubRec.Body.String())

	// AND THE ROUTES AGREE. This block first pinned the nested route's 403, so
	// that "make the two renderers agree" would read as a behaviour change --
	// right instinct, wrong target, and sa-arch filed it as #45: the renderers
	// SHOULD agree on this arm, so the pin was freezing a defect as if it were
	// specified. What cannot be made green by reopening the divergence in either
	// direction is an assertion whose subject is the sameness itself.
	nested := doRequestWithAgentToken(t, srv, http.MethodGet,
		"/api/v1/projects/"+project.ID+"/gcp-service-accounts/"+hubSA.ID, nil, agentToken)
	require.Equal(t, hubRec.Code, nested.Code,
		"the two renderers must answer an identity-less caller alike: flat gave %d, nested gave %d (%s)",
		hubRec.Code, nested.Code, nested.Body.String())

	// POSITIVE CONTROL, and it is what makes the parity above mean anything.
	// sa-arch's note on the sameness idiom: a pin whose shared value is 404 is
	// the one case where the expected answer and the DEAD-PATH answer coincide.
	// Unregister the route, misspell the path, delete the handler -- ServeMux
	// answers 404 to all three requests and every assertion above still passes.
	// One request that must NOT 404 fixes that, on the same route, in the same
	// test, so the instrument cannot pass by measuring nothing.
	live := doRequestAsUser(t, srv, owner, http.MethodGet, flatSAPath+hubSA.ID, nil)
	require.Equal(t, http.StatusOK, live.Code,
		"flat GET must be reachable for an identified reader, or the 404s above prove nothing; got: %s",
		live.Body.String())
}

// #45, found by sa-arch. THE NESTED ROUTE WAS THE SAME ORACLE WITH A WIDER
// SURFACE, and the #42 commit pinned its 403 as intended behaviour.
//
// The four rows below are the whole finding. To an identity-less caller naming
// a project P, the old nested DELETE answered 403 for "project-scoped in P" and
// for "hub-scoped", 404 for "unreachable" and for "nonexistent". That status
// separated existence from non-existence; it separated hub scope from project
// scope, since ReachableFromProject is unconditionally true for hub scope; and
// iterating P over the projects an agent can name turned the third row into the
// owning project of any account id.
//
// ONE TEST OVER ALL FOUR ROWS, asserting they are indistinguishable from each
// other, rather than four assertions of 404. Four such assertions can be
// harmonised one at a time by a future change; the sameness cannot be restored
// piecemeal once broken.
func TestGCPSA_NestedByID_NoIdentity_TheFourRowsAreIndistinguishable(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	other := &store.Project{
		ID:        tid("proj-elsewhere-45"),
		Name:      "Elsewhere",
		Slug:      "elsewhere-45",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, other))

	agent := &store.Agent{
		ID:           tid("agent-nested-sa-probe"),
		Slug:         "nested-sa-probe",
		Name:         "Nested SA Probe",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	agentToken, err := srv.agentTokenService.GenerateAgentToken(agent.ID, project.ID, nil, nil)
	require.NoError(t, err)

	hubSA := mkSA(t, s, "sa-nested-45-hub", "hub45@p.iam.gserviceaccount.com",
		store.ScopeHub, "hub-instance-1", tid("user-stranger"))
	hereSA := mkSA(t, s, "sa-nested-45-here", "here45@p.iam.gserviceaccount.com",
		store.ScopeProject, project.ID, tid("user-stranger"))
	elsewhereSA := mkSA(t, s, "sa-nested-45-away", "away45@p.iam.gserviceaccount.com",
		store.ScopeProject, other.ID, tid("user-stranger"))

	rows := []struct {
		what string
		id   string
	}{
		{"nonexistent", tid("sa-nested-45-ghost")},
		{"project-scoped in another project", elsewhereSA.ID},
		{"project-scoped in the named project", hereSA.ID},
		{"hub-scoped", hubSA.ID},
	}

	// DELETE, not GET: delete authorizes unconditionally, while the read path
	// authorizes only hub scope (an older gap, #598). Delete is therefore where
	// all four rows pass through the renderer under test.
	codes := make(map[string]int, len(rows))
	for _, row := range rows {
		rec := doRequestWithAgentToken(t, srv, http.MethodDelete,
			"/api/v1/projects/"+project.ID+"/gcp-service-accounts/"+row.id, nil, agentToken)
		codes[row.what] = rec.Code
	}

	base := codes[rows[0].what]
	for _, row := range rows[1:] {
		require.Equal(t, base, codes[row.what],
			"an identity-less caller must not tell %q from %q by status: %d vs %d (all four rows: %v)",
			rows[0].what, row.what, base, codes[row.what], codes)
	}
	require.Equal(t, http.StatusNotFound, base,
		"and the shared answer is the one a nonexistent id already gave; got %v", codes)

	// Status parity would also be satisfied by four successful deletes. It was
	// a refusal, and the accounts prove it.
	require.True(t, saExists(t, s, hubSA.ID), "hub-scoped account must survive")
	require.True(t, saExists(t, s, hereSA.ID), "project-scoped account must survive")
	require.True(t, saExists(t, s, elsewhereSA.ID), "the other project's account must survive")

	// POSITIVE CONTROL. sa-arch's note on the idiom they gave me, and it is the
	// hole in every sameness pin whose shared value is 404: THE EXPECTED ANSWER
	// AND THE DEAD-PATH ANSWER ARE THE SAME VALUE HERE. Drop the route, rename
	// the path, delete the handler -- ServeMux answers 404 four times, the
	// parity holds, and the survival checks hold too, because a request that
	// never reached a handler deletes exactly as little as a refused one.
	//
	// A divergence pin does not need this: two different codes cannot both be
	// the harness default. A sameness pin at 404 needs one request that must
	// succeed. The project owner deleting their own project-scoped account is
	// that request, on the same route, in the same test.
	ok := doRequestAsUser(t, srv, owner, http.MethodDelete,
		"/api/v1/projects/"+project.ID+"/gcp-service-accounts/"+hereSA.ID, nil)
	require.Equal(t, http.StatusNoContent, ok.Code,
		"nested DELETE must work for an authorized caller, or the four 404s above prove nothing; got: %s",
		ok.Body.String())
	require.False(t, saExists(t, s, hereSA.ID),
		"and the authorized delete must actually delete -- a 204 from a dead path would not")
}

// The same leak on the write verb, because the fix lives in the renderer and a
// renderer is reached by every verb. DELETE also gives an observable a status
// cannot forge: the account is still there afterwards.
func TestGCPSA_FlatByID_NoIdentity_DeleteIsAlsoIndistinguishable(t *testing.T) {
	srv, s, _, member, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	agent := &store.Agent{
		ID:           tid("agent-flat-sa-del"),
		Slug:         "flat-sa-del",
		Name:         "Flat SA Del",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	agentToken, err := srv.agentTokenService.GenerateAgentToken(agent.ID, project.ID, nil, nil)
	require.NoError(t, err)

	hubSA := mkSA(t, s, "sa-flat-del-hub", "hubdel@p.iam.gserviceaccount.com",
		store.ScopeHub, "hub-instance-1", tid("user-stranger"))
	// Created BY member, so member can delete it below as the positive control.
	// The identity-less assertions do not care who created it.
	userSA := mkSA(t, s, "sa-flat-del-user", "userdel@p.iam.gserviceaccount.com",
		store.ScopeUser, member.ID, member.ID)

	hubRec := doRequestWithAgentToken(t, srv, http.MethodDelete, flatSAPath+hubSA.ID, nil, agentToken)
	userRec := doRequestWithAgentToken(t, srv, http.MethodDelete, flatSAPath+userSA.ID, nil, agentToken)

	require.Equal(t, hubRec.Code, userRec.Code,
		"delete must not distinguish the scopes for an identity-less caller either: hub gave %d, user gave %d",
		hubRec.Code, userRec.Code)
	require.Equal(t, http.StatusNotFound, hubRec.Code)

	require.True(t, saExists(t, s, hubSA.ID), "the hub-scoped account must survive a refused delete")
	require.True(t, saExists(t, s, userSA.ID), "the user-scoped account must survive a refused delete")

	// POSITIVE CONTROL, same reasoning as the GET test above: 404 is what a
	// dead route returns, so a sameness pin at 404 needs one request on the same
	// route that must succeed. The creator deleting their own user-scoped
	// account is it, and it is the strongest available here -- a 204 is not
	// something ServeMux can produce by accident, and the account is gone after.
	ok := doRequestAsUser(t, srv, member, http.MethodDelete, flatSAPath+userSA.ID, nil)
	require.Equal(t, http.StatusNoContent, ok.Code,
		"flat DELETE must work for the creator, or the 404s above prove nothing; got: %s",
		ok.Body.String())
	require.False(t, saExists(t, s, userSA.ID),
		"and the authorized delete must actually delete")
}

// HIGH-1: The flat delete route must invalidate the actAs cache for the
// deleted SA's email, matching what the project-nested delete already does.
// Without this, a cached "allowed" verdict for a deleted SA would survive the
// delete and let a subsequent actAs check succeed against a credential that no
// longer exists.
func TestGCPSA_FlatByID_Delete_InvalidatesActAsCache(t *testing.T) {
	srv, s, _, member, _, _ := setupGCPAuthzTest(t)
	ctx := context.Background()

	saEmail := "cacheflat@p.iam.gserviceaccount.com"
	sa := mkSA(t, s, "sa-flat-cache-inv", saEmail,
		store.ScopeHub, "hub-instance-1", member.ID)

	// Install a cached checker and populate it with a cached decision for the
	// SA we are about to delete.
	inner := newCountingChecker()
	inner.inner.AllowTarget(saEmail)
	cached := NewCachedCallerPermissionChecker(inner, 60*time.Second, 10*time.Second)
	srv.SetSAAssignChecker(cached)

	// Prime the cache — inner should be called once.
	targetSA := &store.GCPServiceAccount{ID: sa.ID, Email: saEmail, ProjectID: "gcp-proj"}
	_, _ = cached.CanActAs(ctx, cacheCaller, targetSA)
	require.Equal(t, 1, inner.callCount(), "setup: inner should have been called once to prime the cache")

	// Verify the cache is warm — inner should NOT be called again.
	_, _ = cached.CanActAs(ctx, cacheCaller, targetSA)
	require.Equal(t, 1, inner.callCount(), "cache should be warm; inner must not be called a second time")

	// Delete the SA via the flat route.
	rec := doRequestAsUser(t, srv, member, http.MethodDelete, flatSAPath+sa.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code,
		"creator should be able to delete their hub-scoped SA; got: %s", rec.Body.String())
	require.False(t, saExists(t, s, sa.ID), "SA should be deleted")

	// The cache for this email must have been invalidated — inner should be
	// called again on the next CanActAs.
	_, _ = cached.CanActAs(ctx, cacheCaller, targetSA)
	require.Equal(t, 2, inner.callCount(),
		"flat delete must invalidate the actAs cache; inner was not called after delete, "+
			"meaning the stale cached decision survived")
}
