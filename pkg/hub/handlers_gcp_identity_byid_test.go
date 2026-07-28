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

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

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
// Nothing here touches live GCP. Verification runs with a nil token generator,
// which is the same path every existing verify test uses.

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
	require.Equal(t, http.StatusForbidden, hubRec.Code,
		"hub-scoped: any authenticated caller can already list these, so a 404 would conceal "+
			"nothing and only make the refusal harder to diagnose; got: %s", hubRec.Body.String())

	userRec := doRequestAsUser(t, srv, owner, http.MethodDelete, flatSAPath+userSA.ID, nil)
	require.Equal(t, http.StatusNotFound, userRec.Code,
		"user-scoped: no other surface exposes these, so a 403 would be an existence oracle "+
			"this route invented; got: %s", userRec.Body.String())

	require.True(t, saExists(t, s, hubSA.ID))
	require.True(t, saExists(t, s, userSA.ID))
}

// The verdict/renderer split must not have changed what the NESTED routes
// answer. The nested caller has already named the project, so a 403 discloses
// nothing they did not supply themselves, and every nested refusal stays a 403.
//
// This is the regression guard on the refactor, not a new requirement. If it
// fails, the split leaked the flat route's disclosure policy into the nested
// one -- which would turn existing 403s into 404s and silently change what
// every current client sees.
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
