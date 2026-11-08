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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Automatic review access for the delegating user (ptone/scion#7134):
// artifactHost.SponsorReviewGrant and sponsorReadsHomeThroughRole.

// enableSponsorReviewForTest turns hub.artifact_sponsor_review_grant on (on
// top of whatever registry the server has, hub.artifacts included when
// enableArtifactsForTest ran first).
func enableSponsorReviewForTest(t *testing.T, srv *Server) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.Artifacts || e.Name == experiments.ArtifactSponsorReviewGrant {
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.True(t, srv.experimentEnabled(experiments.ArtifactSponsorReviewGrant))
}

// requireRoleError asserts that err is the role refusal for scope, naming
// the scope and artifact.read only.
func requireRoleError(t *testing.T, err error, scope string, msgAndArgs ...any) {
	t.Helper()
	var roleErr *artifacts.SponsorReviewRoleError
	require.True(t, errors.As(err, &roleErr), append([]any{"want a role error, got %v"}, err)...)
	assert.Equal(t, scope, roleErr.Scope, msgAndArgs...)
	assert.Equal(t, artifacts.PermissionRead, roleErr.Permission, msgAndArgs...)
}

// TestArtifactHostSponsorReviewGrant: the policy answers off while the
// experiment is off; with it on, a grant only for an active user who reads
// the asked scope through a role. The caller's own rights never carry over.
func TestArtifactHostSponsorReviewGrant(t *testing.T) {
	f := newReviewGrantFixture(t)
	host := newArtifactHost(f.srv)
	ctx := contextWithIdentity(context.Background(), f.user(f.admin))

	require.ErrorIs(t, host.SponsorReviewGrant(ctx, f.home, f.delegator), artifacts.ErrSponsorReviewOff)

	enableSponsorReviewForTest(t, f.srv)
	inactive := tid("sr-inactive")
	createDCUser(t, f.s, inactive, "sr-inactive@test.com", f.home, store.ProjectRoleMember)
	u, err := f.s.GetUser(context.Background(), inactive)
	require.NoError(t, err)
	u.Status = store.UserStatusSuspended
	require.NoError(t, f.s.UpdateUser(context.Background(), u))

	assert.NoError(t, host.SponsorReviewGrant(ctx, f.home, f.delegator), "member of home")
	assert.NoError(t, host.SponsorReviewGrant(ctx, f.home, f.owner), "owner of home")
	for name, tc := range map[string]struct{ scope, user string }{
		"member of another project only": {f.home, f.outside},
		"inactive member":                {f.home, inactive},
		"unknown user":                   {f.home, tid("sr-missing-user")},
		"deleted home project":           {tid("sr-missing-project"), f.delegator},
	} {
		requireRoleError(t, host.SponsorReviewGrant(ctx, tc.scope, tc.user), tc.scope, name)
	}
	assert.Error(t, host.SponsorReviewGrant(ctx, f.home, ""), "empty user")
}

// TestArtifactHostSponsorReviewHomeScopeOnly: the role check is made
// against the artifact's home, not the agent's project. The delegating
// user is admitted to the agent's project (DelegatingUser resolves them)
// and reads there, but holds no role in another project: an artifact homed
// there gets no grant, and the refusal names that scope and artifact.read
// only.
func TestArtifactHostSponsorReviewHomeScopeOnly(t *testing.T) {
	f := newReviewGrantFixture(t)
	enableSponsorReviewForTest(t, f.srv)
	host := newArtifactHost(f.srv)
	ctx := context.Background()

	sponsor, err := host.DelegatingUser(ctx, f.agent.ID)
	require.NoError(t, err)
	require.Equal(t, f.delegator, sponsor, "the delegating user is admitted to the agent's project")
	require.NoError(t, host.SponsorReviewGrant(ctx, f.home, sponsor), "role in the agent's project, artifact homed there")

	err = host.SponsorReviewGrant(ctx, f.other, sponsor)
	requireRoleError(t, err, f.other)
	assert.Equal(t, "artifacts: no active role with artifact.read in project "+f.other, err.Error())
	assert.NotContains(t, err.Error(), sponsor)
	assert.NotContains(t, err.Error(), f.home)
}

// TestArtifactHostSponsorReadsHomeChecks: each condition the local check
// keeps answers no: a test identity that can no longer sign in, the local
// development user while local-development authority is off, and every
// lookup failure (an error, never a grant).
func TestArtifactHostSponsorReadsHomeChecks(t *testing.T) {
	f := newReviewGrantFixture(t)
	host := newArtifactHost(f.srv)
	ctx := context.Background()

	// A test identity past its window, holding a member role in home.
	expired := gateAFixtureUser(t, f.s, "sr-expired", time.Now().Add(-time.Hour))
	createDCUser(t, f.s, expired.ID, expired.Email, f.home, store.ProjectRoleMember)
	ok, err := host.sponsorReadsHomeThroughRole(ctx, expired.ID, f.home)
	require.NoError(t, err)
	assert.False(t, ok, "a test identity past its window")

	// The dev user, which the test server seeds with a system role, with
	// local-development authority on (positive control) and off.
	_, err = f.s.GetUser(ctx, DevUserID)
	require.NoError(t, err, "the test server seeds the dev user")
	f.srv.authzService.setDevLocalAuthorityEnabled(true)
	ok, err = host.sponsorReadsHomeThroughRole(ctx, DevUserID, f.home)
	require.NoError(t, err)
	require.True(t, ok, "control: the dev user with local-development authority on reads home")
	f.srv.authzService.setDevLocalAuthorityEnabled(false)
	ok, err = host.sponsorReadsHomeThroughRole(ctx, DevUserID, f.home)
	require.NoError(t, err)
	assert.False(t, ok, "the dev user with local-development authority off")

	for name, fault := range map[string]*sponsorFaultStore{
		"user lookup":    {Store: f.s, failUser: f.delegator},
		"project lookup": {Store: f.s, failProject: f.home},
	} {
		faulty := hostOverStore(fault, false)
		ok, err := faulty.sponsorReadsHomeThroughRole(ctx, f.delegator, f.home)
		assert.False(t, ok, name)
		// Fired control: only the injected fault returns errSponsorStoreFault.
		assert.ErrorIs(t, err, errSponsorStoreFault, name)
	}
	// A role-binding fault makes CheckAccess's decision indeterminate: it is
	// a lookup failure, not "no role".
	ok, err = hostOverStore(&sponsorFaultStore{Store: f.s, failBindings: true}, false).sponsorReadsHomeThroughRole(ctx, f.delegator, f.home)
	assert.False(t, ok, "role bindings")
	// Fired control: with the project and user lookups unfaulted, the helper
	// returns an error only for an indeterminate CheckAccess decision, which
	// only the injected binding fault produces here.
	assert.ErrorContains(t, err, "role lookup failed", "role bindings: reported as a lookup failure")
	ok, err = hostOverStore(f.s, false).sponsorReadsHomeThroughRole(ctx, f.delegator, f.home)
	require.NoError(t, err)
	assert.True(t, ok, "control: the same host without faults")
}

// TestSponsorReviewIdentityOnlyASubject pins the check-only identity: in
// sponsorReadsHomeThroughRole nothing puts an identity into a context, the
// identity built from the user record is passed only as CheckAccess's
// subject, and CheckAccess runs under maskAllAuthzMemo.
func TestSponsorReviewIdentityOnlyASubject(t *testing.T) {
	forbidden := map[string]bool{
		"WithValue": true, "contextWithIdentity": true, "contextWithCredentialContext": true,
		"WithIdentity": true, "withIdentity": true, "ContextWithIdentity": true,
	}
	found := false
	forEachHubFile(t, func(name string, _ *token.FileSet, f *ast.File) {
		if name != "artifacts_host.go" {
			return
		}
		ast.Inspect(f, func(n ast.Node) bool {
			fd, ok := n.(*ast.FuncDecl)
			if !ok || fd.Name.Name != "sponsorReadsHomeThroughRole" || fd.Body == nil {
				return true
			}
			found = true
			uses, checks := 0, 0
			ast.Inspect(fd.Body, func(m ast.Node) bool {
				call, ok := m.(*ast.CallExpr)
				if !ok {
					return true
				}
				sym := extractCallSymbol(call)
				if forbidden[sym] {
					t.Errorf("sponsorReadsHomeThroughRole calls %s: the identity must never enter a context", sym)
				}
				if sym == "CheckAccess" {
					checks++
					if len(call.Args) < 2 {
						t.Errorf("CheckAccess with %d arguments", len(call.Args))
						return true
					}
					mask, ok := call.Args[0].(*ast.CallExpr)
					if !ok || extractCallSymbol(mask) != "maskAllAuthzMemo" {
						t.Error("CheckAccess's context must be maskAllAuthzMemo(ctx)")
					}
				}
				for i, arg := range call.Args {
					if id, ok := arg.(*ast.Ident); ok && id.Name == "identity" {
						uses++
						if sym != "CheckAccess" || i != 1 {
							t.Errorf("identity passed to %s (arg %d); only CheckAccess's subject argument is allowed", sym, i)
						}
					}
				}
				return true
			})
			if uses != 1 || checks != 1 {
				t.Errorf("identity used %d times and CheckAccess called %d times, want 1 and 1", uses, checks)
			}
			return false
		})
	})
	require.True(t, found, "sponsorReadsHomeThroughRole not found in artifacts_host.go: scanner broken")
}

// sponsorReviewManifest is a one-file manifest body for the versions route.
func sponsorReviewManifest(t *testing.T, kind string, body []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(body)
	b, err := json.Marshal(artifacts.CreateVersionRequest{Kind: kind, Entry: "report.md",
		Files: []artifacts.ManifestFile{{Path: "report.md", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}}})
	require.NoError(t, err)
	return b
}

// TestArtifactSponsorReviewGrantOnHub: with the experiment on, an agent's
// publish gives its delegating user a review grant with its event, so the
// user starts a review version with no manual grant and is refused an
// ordinary one (review_only); a steward's removal is final. With the
// experiment off, the publish adds nothing.
func TestArtifactSponsorReviewGrantOnHub(t *testing.T) {
	f := newReviewGrantFixture(t)
	st, _ := enableArtifactsForTest(t, f.srv)
	ctx := context.Background()
	agent := artifactTestAgent(f.agent.ID, f.home, ScopeProjectArtifactRead, ScopeProjectArtifactWrite)
	publish := func(name string) string {
		t.Helper()
		rec := identityArtifactRequest(t, f.srv, agent, http.MethodPost, "/api/v1/artifacts?name="+name+"&scope="+f.home, []byte("# report\n"))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var pub artifacts.ArtifactResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pub))
		return pub.Artifact.ID
	}
	sponsorRef := artifacts.PrincipalRef(artifacts.PrincipalKindUser, f.delegator)
	sponsorGrant := func(id string) *artifacts.Grant {
		t.Helper()
		gs, err := st.ListGrants(ctx, id)
		require.NoError(t, err)
		for i := range gs {
			if gs[i].SubjectKind == artifacts.SubjectPrincipal && gs[i].SubjectRef == sponsorRef {
				return &gs[i]
			}
		}
		return nil
	}

	off := publish("off.md")
	assert.Nil(t, sponsorGrant(off), "experiment off: no grant")

	enableSponsorReviewForTest(t, f.srv)
	id := publish("on.md")
	g := sponsorGrant(id)
	require.NotNil(t, g, "experiment on: the automatic grant")
	assert.Equal(t, artifacts.GrantReview, g.Permission)
	assert.Equal(t, artifacts.SponsorReviewCreator, g.CreatedByRef)
	evs, err := st.ListEvents(ctx, id)
	require.NoError(t, err)
	var created int
	for _, e := range evs {
		if e.Action == artifacts.EventGrantCreated {
			created++
			assert.Equal(t, artifacts.ActorKindSystem, e.ActorKind)
			assert.Equal(t, artifacts.AuthoritySponsorReview, e.Authority)
		}
	}
	assert.Equal(t, 1, created)

	delegator := f.user(f.delegator)
	path := "/api/v1/artifacts/" + id + "/versions"
	rec := identityArtifactRequest(t, f.srv, delegator, http.MethodPost, path, sponsorReviewManifest(t, artifacts.VersionKindReview, []byte("# report {>>ok?<<}\n")))
	assert.Equal(t, http.StatusCreated, rec.Code, "review by the delegating user: %s", rec.Body.String())
	rec = identityArtifactRequest(t, f.srv, delegator, http.MethodPost, path, sponsorReviewManifest(t, "", []byte("# rewritten\n")))
	assert.Equal(t, http.StatusForbidden, rec.Code, "ordinary version: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), artifacts.CodeReviewOnly)

	owner := f.user(f.owner)
	rec = identityArtifactRequest(t, f.srv, owner, http.MethodDelete, "/api/v1/artifacts/"+id+"/grants/"+g.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, "steward removes the grant: %s", rec.Body.String())
	rec = identityArtifactRequest(t, f.srv, delegator, http.MethodPost, path, sponsorReviewManifest(t, artifacts.VersionKindReview, []byte("# report {>>again?<<}\n")))
	assert.Equal(t, http.StatusForbidden, rec.Code, "review after the grant was removed: %s", rec.Body.String())
	rec = identityArtifactRequest(t, f.srv, agent, http.MethodPost, "/api/v1/artifacts/"+id+"/versions", sponsorReviewManifest(t, "", []byte("# v3\n")))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Nil(t, sponsorGrant(id), "a removed grant is never re-created")
}

// TestArtifactHostSponsorHomeMustExist: a sponsor whose system role would
// pass CheckAccess for any project id (the dev user, seeded with a system
// role, with local-development authority on) gets no grant for a home
// project id that does not exist, and gets one for an existing home. Only
// the helper's project-exists check refuses the missing id.
func TestArtifactHostSponsorHomeMustExist(t *testing.T) {
	f := newReviewGrantFixture(t)
	host := newArtifactHost(f.srv)
	ctx := context.Background()
	_, err := f.s.GetUser(ctx, DevUserID)
	require.NoError(t, err, "the test server seeds the dev user")
	f.srv.authzService.setDevLocalAuthorityEnabled(true)
	missing := tid("sr-never-created")

	// The system role alone admits the missing id: CheckAccess allows it.
	dev, err := f.s.GetUser(ctx, DevUserID)
	require.NoError(t, err)
	readPerm, ok := artifactPermission(artifacts.PermissionRead)
	require.True(t, ok)
	decision := f.srv.authzService.CheckAccess(maskAllAuthzMemo(ctx),
		NewAuthenticatedUser(dev.ID, dev.Email, dev.DisplayName, dev.Role, ""),
		Resource{Type: permissions.ResourceArtifact, ParentType: permissions.ResourceProject, ParentID: missing},
		Action(readPerm.Action))
	require.True(t, decision.Allowed, "precondition: a system role passes CheckAccess for a missing project id")

	ok, err = host.sponsorReadsHomeThroughRole(ctx, DevUserID, f.home)
	require.NoError(t, err)
	require.True(t, ok, "control: existing home")
	ok, err = host.sponsorReadsHomeThroughRole(ctx, DevUserID, missing)
	require.NoError(t, err)
	assert.False(t, ok, "missing home project")

	enableSponsorReviewForTest(t, f.srv)
	requireRoleError(t, host.SponsorReviewGrant(ctx, missing, DevUserID), missing)
	assert.NoError(t, host.SponsorReviewGrant(ctx, f.home, DevUserID))
}

// userReadCountStore counts GetUser calls per user id.
type userReadCountStore struct {
	store.Store
	mu    sync.Mutex
	reads map[string]int
}

func (s *userReadCountStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	s.mu.Lock()
	s.reads[id]++
	s.mu.Unlock()
	return s.Store.GetUser(ctx, id)
}

func (s *userReadCountStore) count(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads[id]
}

// TestArtifactHostSponsorInactiveRefusedByHelper: the helper itself refuses
// an inactive sponsor before any policy decision. CheckAccess reads the
// user again for its own account-status gate, so an active sponsor costs
// more than one user read; a suspended sponsor holding a home binding costs
// exactly the helper's one read and gets no grant.
func TestArtifactHostSponsorInactiveRefusedByHelper(t *testing.T) {
	f := newReviewGrantFixture(t)
	ctx := context.Background()
	suspended := tid("sr-suspended")
	createDCUser(t, f.s, suspended, "sr-suspended@test.com", f.home, store.ProjectRoleMember)
	u, err := f.s.GetUser(ctx, suspended)
	require.NoError(t, err)
	u.Status = store.UserStatusSuspended
	require.NoError(t, f.s.UpdateUser(ctx, u))

	counting := &userReadCountStore{Store: f.s, reads: map[string]int{}}
	host := hostOverStore(counting, false)

	ok, err := host.sponsorReadsHomeThroughRole(ctx, f.delegator, f.home)
	require.NoError(t, err)
	require.True(t, ok, "control: active member")
	// Fired control: the count is on the production path (the helper's read
	// plus CheckAccess's account-status read).
	require.Greater(t, counting.count(f.delegator), 1, "control: CheckAccess reads the user too")

	ok, err = host.sponsorReadsHomeThroughRole(ctx, suspended, f.home)
	require.NoError(t, err)
	assert.False(t, ok, "suspended member")
	assert.Equal(t, 1, counting.count(suspended), "the helper refuses before CheckAccess")
}

// principalBindingFaultStore fails every role-binding load whose principal
// list names the user userID. Production loads a principal's bindings for
// every scope at once (ListRoleBindingsForPrincipals with nil scope
// arguments; authz_request_inputs.go), so the fault is keyed on the
// principal, the argument production passes, and fired counts each fault.
type principalBindingFaultStore struct {
	store.Store
	userID string
	mu     sync.Mutex
	fired  int
}

func (s *principalBindingFaultStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes, scopeIDs []string) ([]*store.RoleBinding, error) {
	for _, p := range principals {
		if p.Type == store.RoleBindingPrincipalUser && p.ID == s.userID {
			s.mu.Lock()
			s.fired++
			s.mu.Unlock()
			return nil, errSponsorStoreFault
		}
	}
	return s.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

func (s *principalBindingFaultStore) firedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fired
}

// TestArtifactSponsorReviewAutomaticFormOnHub: through the hub's own
// Authorize, an automatic grant counts only while its holder reads the home
// project through a role. The delegating user also reads the artifact
// through a grant to another project they belong to, so only the
// automatic-form rule decides: leaving the home project refuses the review
// and re-joining restores it. A role-lookup fault for the delegating user
// is shown to fire and to make the hub's decision deny (CheckAccess with a
// resolution error, Authorize false), and the review is refused under it:
// every read path of a user loads the same bindings, so the artifact
// answers 404. Finally the automatic-form clause itself is driven under a
// fault confined to the home project (homeScopeHost): the artifact still
// reads through the other project's grant, and the review is refused with
// 403; the same service with an unfaulted home host accepts it.
func TestArtifactSponsorReviewAutomaticFormOnHub(t *testing.T) {
	f := newReviewGrantFixture(t)
	st, blobs := enableArtifactsForTest(t, f.srv)
	enableSponsorReviewForTest(t, f.srv)
	ctx := context.Background()
	agent := artifactTestAgent(f.agent.ID, f.home, ScopeProjectArtifactRead, ScopeProjectArtifactWrite)
	rec := identityArtifactRequest(t, f.srv, agent, http.MethodPost, "/api/v1/artifacts?name=auto.md&scope="+f.home, []byte("# report\n"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var pub artifacts.ArtifactResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pub))
	id := pub.Artifact.ID

	createDCUser(t, f.s, f.delegator, "rg-delegator@test.com", f.other, store.ProjectRoleMember)
	_, err := st.PutGrant(ctx, &artifacts.Grant{ID: tid("sr-other-read"), ArtifactID: id, SubjectKind: artifacts.SubjectScope,
		SubjectRef: f.other, Permission: artifacts.GrantRead, CreatedAt: time.Now()},
		artifacts.MaxGrantsPerArtifact, true, &artifacts.Actor{Kind: artifacts.PrincipalKindUser, Ref: "test-setup", Authority: artifacts.AuthorityOwner})
	require.NoError(t, err)

	delegator := f.user(f.delegator)
	path := "/api/v1/artifacts/" + id
	n := 0
	review := func() int {
		t.Helper()
		n++
		body := sponsorReviewManifest(t, artifacts.VersionKindReview, []byte(fmt.Sprintf("# report {>>ok %d?<<}\n", n)))
		return identityArtifactRequest(t, f.srv, delegator, http.MethodPost, path+"/versions", body).Code
	}
	read := func() int {
		t.Helper()
		return identityArtifactRequest(t, f.srv, delegator, http.MethodGet, path, nil).Code
	}
	require.Equal(t, http.StatusCreated, review(), "control: review with a home role")

	bindings, err := f.s.ListRoleBindingsForPrincipals(ctx, []store.PrincipalRef{{Type: store.RoleBindingPrincipalUser, ID: f.delegator}},
		[]string{store.RoleScopeProject}, []string{f.home})
	require.NoError(t, err)
	require.NotEmpty(t, bindings)
	for _, b := range bindings {
		require.NoError(t, f.s.DeleteRoleBinding(ctx, b.ID))
	}
	require.Equal(t, http.StatusOK, read(), "still reads through the other project's grant")
	assert.Equal(t, http.StatusForbidden, review(), "left the home project: the automatic grant gives nothing")

	createDCUser(t, f.s, f.delegator, "rg-delegator@test.com", f.home, store.ProjectRoleMember)
	assert.Equal(t, http.StatusCreated, review(), "re-joined the home project")

	readPerm, ok := artifactPermission(artifacts.PermissionRead)
	require.True(t, ok)
	homeRead := func() Decision {
		t.Helper()
		return f.srv.authzService.CheckAccess(context.Background(), delegator, Resource{
			Type: permissions.ResourceArtifact, ParentType: permissions.ResourceProject, ParentID: f.home,
		}, Action(readPerm.Action))
	}
	host := newArtifactHost(f.srv)
	asDelegator := contextWithIdentity(context.Background(), delegator)
	require.True(t, homeRead().Allowed, "control without the fault: CheckAccess allows the home read")
	require.True(t, host.Authorize(asDelegator, f.home, artifacts.PermissionRead), "control without the fault: Authorize allows")

	orig := f.srv.authzService.store
	fault := &principalBindingFaultStore{Store: orig, userID: f.delegator}
	f.srv.authzService.store = fault
	t.Cleanup(func() { f.srv.authzService.store = orig })
	decision := homeRead()
	require.Positive(t, fault.firedCount(), "the fault fired")
	require.False(t, decision.Allowed, "under the fault: CheckAccess denies")
	require.Equal(t, DenyCauseResolutionError, decision.DenyCause, "under the fault: the deny is a resolution error")
	assert.False(t, host.Authorize(asDelegator, f.home, artifacts.PermissionRead), "under the fault: Authorize answers false")
	// 404, not 403: the read through the other project's grant needs the same
	// faulted binding load, so the artifact is unreadable and answers as a
	// missing one before any write decision.
	assert.Equal(t, http.StatusNotFound, review(), "role lookup fault: the review is refused (the read fails closed)")

	f.srv.authzService.store = orig
	assert.True(t, host.Authorize(asDelegator, f.home, artifacts.PermissionRead), "control: fault cleared, Authorize allows")
	assert.Equal(t, http.StatusCreated, review(), "control: fault cleared")

	// The automatic-form refusal itself under a real fault: a service whose
	// host is the hub's own artifactHost, except that Authorize for the home
	// project goes through an artifactHost whose store faults the delegating
	// user's role bindings. The other project's grant still reads (clean
	// Authorize), so the review reaches the automatic-form clause in
	// grantAllows, which must refuse it (403).
	homeFault := &principalBindingFaultStore{Store: orig, userID: f.delegator}
	reviewVia := func(homeHost *artifactHost) (int, int) {
		t.Helper()
		svc := artifacts.NewService(homeScopeHost{artifactHost: host, home: f.home, homeHost: homeHost})
		svc.SetStore(st)
		svc.SetBlobStorage(blobs, "hub-sponsor-review-test")
		serve := func(method, target string, body []byte) int {
			req := httptest.NewRequest(method, target, bytes.NewReader(body))
			req = req.WithContext(contextWithIdentity(req.Context(), delegator))
			rec := httptest.NewRecorder()
			svc.ServeHTTP(rec, req)
			return rec.Code
		}
		n++
		body := sponsorReviewManifest(t, artifacts.VersionKindReview, []byte(fmt.Sprintf("# report {>>ok %d?<<}\n", n)))
		return serve(http.MethodGet, path, nil), serve(http.MethodPost, path+"/versions", body)
	}
	readCode, reviewCode := reviewVia(hostOverStore(homeFault, false))
	require.Positive(t, homeFault.firedCount(), "the home-project fault fired")
	require.Equal(t, http.StatusOK, readCode, "under the home fault: still reads through the other project's grant")
	assert.Equal(t, http.StatusForbidden, reviewCode, "under the home fault: the automatic grant is refused (403)")
	_, reviewCode = reviewVia(hostOverStore(orig, false))
	assert.Equal(t, http.StatusCreated, reviewCode, "control: the same service without the fault")
}

// homeScopeHost is the hub's artifactHost with Authorize for one project
// answered by another artifactHost (one whose store injects a fault).
type homeScopeHost struct {
	*artifactHost
	home     string
	homeHost *artifactHost
}

func (h homeScopeHost) Authorize(ctx context.Context, scopeRef, permission string) bool {
	if scopeRef == h.home {
		return h.homeHost.Authorize(ctx, scopeRef, permission)
	}
	return h.artifactHost.Authorize(ctx, scopeRef, permission)
}

// TestArtifactHostSponsorHomeDeletedBindingRemains: a home project deleted
// while the sponsor's member binding on its id remains gives no grant; the
// same user is granted on the live home.
func TestArtifactHostSponsorHomeDeletedBindingRemains(t *testing.T) {
	f := newReviewGrantFixture(t)
	host := newArtifactHost(f.srv)
	ctx := context.Background()
	gone := tid("sr-deleted-home")
	createDCProject(t, f.s, gone, "sr-deleted-home")
	createDCUser(t, f.s, f.delegator, "rg-delegator@test.com", gone, store.ProjectRoleMember)
	ok, err := host.sponsorReadsHomeThroughRole(ctx, f.delegator, gone)
	require.NoError(t, err)
	require.True(t, ok, "control: the member reads the project before it is deleted")

	require.NoError(t, f.s.DeleteProject(ctx, gone))
	bindings, err := f.s.ListRoleBindingsForPrincipals(ctx, []store.PrincipalRef{{Type: store.RoleBindingPrincipalUser, ID: f.delegator}},
		[]string{store.RoleScopeProject}, []string{gone})
	require.NoError(t, err)
	require.NotEmpty(t, bindings, "shape under test: the member binding remains on the deleted project's id")

	ok, err = host.sponsorReadsHomeThroughRole(ctx, f.delegator, gone)
	require.NoError(t, err)
	assert.False(t, ok, "deleted home project")
	enableSponsorReviewForTest(t, f.srv)
	requireRoleError(t, host.SponsorReviewGrant(ctx, gone, f.delegator), gone)
	assert.NoError(t, host.SponsorReviewGrant(ctx, f.home, f.delegator), "control: live home")
}
