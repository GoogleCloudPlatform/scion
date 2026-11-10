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
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The delegating user recorded on agent-owned artifacts (ptone/scion#6028):
// artifactHost.DelegatingUser, the recorded path of the steward and review
// clauses, and the routes after the agent is deleted and purged.

var errSponsorStoreFault = errors.New("injected store fault")

// sponsorFaultStore fails the reads the recorded-delegator and
// delegating-user lookups make, for the ids listed.
type sponsorFaultStore struct {
	store.Store
	failEdgesFor string
	failUser     string
	failProject  string
	failBindings bool
}

func (s *sponsorFaultStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes, scopeIDs []string) ([]*store.RoleBinding, error) {
	if s.failBindings {
		return nil, errSponsorStoreFault
	}
	return s.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

func (s *sponsorFaultStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	if id == s.failProject {
		return nil, errSponsorStoreFault
	}
	return s.Store.GetProject(ctx, id)
}

func (s *sponsorFaultStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	if delegateID == s.failEdgesFor {
		return nil, errSponsorStoreFault
	}
	return s.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
}

// GetUser fails for failUser, returning the real, active user together
// with the error, so code that ignored the error would go on with an
// active user and be caught by an assertion rather than a nil dereference.
func (s *sponsorFaultStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if id == s.failUser {
		u, err := s.Store.GetUser(ctx, id)
		if err != nil {
			return nil, err
		}
		return u, errSponsorStoreFault
	}
	return s.Store.GetUser(ctx, id)
}

// hostOverStore is an artifact host whose authorization service reads s;
// only the methods that need nothing else of a server may be called on it
// (DelegatingUser, recordedDelegator).
func hostOverStore(s store.Store, devLocal bool) *artifactHost {
	authz := NewAuthzService(s, slog.Default())
	authz.setDevLocalAuthorityEnabled(devLocal)
	return newArtifactHost(&Server{authzService: authz})
}

func (f reviewGrantFixture) stewardRecorded(host *artifactHost, identity Identity, agentID, home, sponsor string) string {
	return host.StewardClause(contextWithIdentity(context.Background(), identity), agentID, home, sponsor, false)
}

func (f reviewGrantFixture) reviewRecorded(host *artifactHost, identity Identity, agentID, home, sponsor string) string {
	return host.ReviewGrantClause(contextWithIdentity(context.Background(), identity), agentID, home, sponsor)
}

// softDeleteAgentForSponsor soft-deletes agentID as the delete engine's
// transaction does for its edges: the row is marked deleted and its
// delegation edges are deactivated with cause agent_soft_delete.
func softDeleteAgentForSponsor(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	markAgentSoftDeleted(t, s, agentID)
	_, err := s.DeactivateDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, agentID,
		store.Deactivation{Cause: store.EdgeDeactivationAgentSoftDelete, OpID: "sponsor-test-" + uuid.NewString()})
	require.NoError(t, err)
}

// purgeAgentForSponsor purges the soft-deleted agentID: its row is
// deleted and its edges deactivated with cause agent_hard_delete.
func purgeAgentForSponsor(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	n, err := s.PurgeDeletedAgentsByID(context.Background(), []string{agentID}, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, n, "purged")
	_, err = s.GetAgent(context.Background(), agentID)
	require.ErrorIs(t, err, store.ErrNotFound, "the agent row is gone")
}

// TestArtifactHostDelegatingUser: DelegatingUser names the root user of
// the agent's recorded chain (through an agent-spawned agent too);
// answers ("", nil) for a provenance denial (no edge, unknown or deleted
// agent); and returns a lookup fault as an error, so the sponsor backfill
// stops rather than recording that it finished.
func TestArtifactHostDelegatingUser(t *testing.T) {
	f := newReviewGrantFixture(t)
	host := newArtifactHost(f.srv)
	ctx := context.Background()

	for name, c := range map[string]struct{ agent, want string }{
		"agent":                   {f.agent.ID, f.delegator},
		"agent-spawned agent":     {f.child.ID, f.delegator},
		"another user's delegate": {f.sibling.ID, f.member},
		"agent with no edge":      {f.noEdge.ID, ""},
		"unknown agent":           {tid("sp-missing"), ""},
	} {
		got, err := host.DelegatingUser(ctx, c.agent)
		require.NoError(t, err, name)
		assert.Equal(t, c.want, got, name)
	}

	t.Run("lookup fault is an error", func(t *testing.T) {
		faulty := hostOverStore(&sponsorFaultStore{Store: f.s, failEdgesFor: f.agent.ID}, false)
		got, err := faulty.DelegatingUser(ctx, f.agent.ID)
		require.ErrorIs(t, err, errSponsorStoreFault)
		assert.Equal(t, "", got)
		// An agent whose chain passes through the faulted one fails too.
		_, err = faulty.DelegatingUser(ctx, f.child.ID)
		require.ErrorIs(t, err, errSponsorStoreFault)
		// The same host resolves an agent whose lookups succeed.
		got, err = faulty.DelegatingUser(ctx, f.sibling.ID)
		require.NoError(t, err)
		assert.Equal(t, f.member, got)
	})
	t.Run("soft-deleted agent is a denial", func(t *testing.T) {
		softDeleteAgentForSponsor(t, f.s, f.sibling.ID)
		got, err := host.DelegatingUser(ctx, f.sibling.ID)
		require.NoError(t, err)
		assert.Equal(t, "", got)
	})
	t.Run("no authz service", func(t *testing.T) {
		_, err := newArtifactHost(&Server{}).DelegatingUser(ctx, f.agent.ID)
		require.Error(t, err)
	})
}

// TestArtifactHostRecordedDelegator: with a recorded delegating user, both
// clauses decide from it alone: it holds for that user while active and
// admitted to the current home (and, for the steward clause, reading
// there through a role), whatever the live chain says, and for no one
// else.
func TestArtifactHostRecordedDelegator(t *testing.T) {
	f := newReviewGrantFixture(t)
	host := newArtifactHost(f.srv)
	type clauses struct{ steward, review string }
	both := func(identity Identity, agentID, home, sponsor string) clauses {
		return clauses{f.stewardRecorded(host, identity, agentID, home, sponsor), f.reviewRecorded(host, identity, agentID, home, sponsor)}
	}
	yes := clauses{artifacts.StewardClauseDelegator, artifacts.ReviewClauseDelegatingUser}
	no := clauses{"", ""}

	cases := []struct {
		name     string
		identity Identity
		agentID  string
		sponsor  string
		want     clauses
	}{
		{"recorded user", f.user(f.delegator), f.agent.ID, f.delegator, yes},
		{"recorded user, agent with no live edge", f.user(f.delegator), f.noEdge.ID, f.delegator, yes},
		{"recorded user, agent unknown", f.user(f.delegator), tid("sp-gone"), f.delegator, yes},
		// The creation-time value stands: the live root of sibling is
		// member, but the recorded user is delegator.
		{"live root that is not the recorded user", f.user(f.member), f.sibling.ID, f.delegator, no},
		{"recorded user on an agent whose live root is someone else", f.user(f.delegator), f.sibling.ID, f.delegator, yes},
		{"other member", f.user(f.member), f.agent.ID, f.delegator, no},
		{"user from another project", f.user(f.outside), f.agent.ID, f.outside, no},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, both(tc.identity, tc.agentID, f.home, tc.sponsor))
		})
	}

	t.Run("token without artifact:read is not a steward", func(t *testing.T) {
		delegator := NewAuthenticatedUser(f.delegator, "rg-delegator@test.com", "delegator", "member", "api")
		assert.Equal(t, "", f.stewardRecorded(host, artifactTestUAT(t, delegator, f.home, "artifact:manage"), f.agent.ID, f.home, f.delegator))
		assert.Equal(t, artifacts.StewardClauseDelegator,
			f.stewardRecorded(host, artifactTestUAT(t, delegator, f.home, "artifact:read", "artifact:manage"), f.agent.ID, f.home, f.delegator))
	})
	// After a move the recorded user's clauses follow the current home:
	// none in a home where it holds no role (not admitted), both once it
	// holds one there, whatever its standing in the agent's project.
	t.Run("moved home", func(t *testing.T) {
		assert.Equal(t, no, both(f.user(f.delegator), f.agent.ID, f.other, f.delegator), "no role in the new home")
		createDCUser(t, f.s, f.delegator, "rg-delegator@test.com", f.other, store.ProjectRoleMember)
		assert.Equal(t, yes, both(f.user(f.delegator), f.agent.ID, f.other, f.delegator), "member of the new home")
		assert.Equal(t, no, both(f.user(f.outside), f.agent.ID, f.other, f.delegator), "member of the new home who is not the recorded user")
	})
	// Each check is made on every request: a suspension takes effect on the
	// next one.
	t.Run("suspended between requests", func(t *testing.T) {
		require.Equal(t, yes, both(f.user(f.delegator), f.agent.ID, f.home, f.delegator))
		setUserStatus(t, f.s, f.delegator, "suspended")
		assert.Equal(t, no, both(f.user(f.delegator), f.agent.ID, f.home, f.delegator))
		setUserStatus(t, f.s, f.delegator, "active")
		assert.Equal(t, yes, both(f.user(f.delegator), f.agent.ID, f.home, f.delegator))
	})
	t.Run("left the home project", func(t *testing.T) {
		g := newReviewGrantFixture(t)
		h := newArtifactHost(g.srv)
		bindings, err := g.s.ListRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalUser, g.delegator)
		require.NoError(t, err)
		for _, b := range bindings {
			if b.ScopeType == store.RoleScopeProject && b.ScopeID == g.home {
				require.NoError(t, g.s.DeleteRoleBinding(context.Background(), b.ID))
			}
		}
		assert.Equal(t, "", g.stewardRecorded(h, g.user(g.delegator), g.agent.ID, g.home, g.delegator))
		assert.Equal(t, "", g.reviewRecorded(h, g.user(g.delegator), g.agent.ID, g.home, g.delegator))
	})
	// Deleting the home project ends both clauses. The store delete removes
	// only the project row, so the recorded user's role binding in the home
	// is left behind and would still admit it: the home-exists checks end
	// the clauses (each clause's own check, and recordedDelegator's, which
	// the direct call below pins).
	t.Run("home project deleted", func(t *testing.T) {
		g := newReviewGrantFixture(t)
		h := newArtifactHost(g.srv)
		require.Equal(t, artifacts.ReviewClauseDelegatingUser, g.reviewRecorded(h, g.user(g.delegator), g.agent.ID, g.home, g.delegator))
		require.NoError(t, g.s.DeleteProject(context.Background(), g.home))
		assert.Equal(t, "", g.stewardRecorded(h, g.user(g.delegator), g.agent.ID, g.home, g.delegator), "steward clause")
		assert.Equal(t, "", g.reviewRecorded(h, g.user(g.delegator), g.agent.ID, g.home, g.delegator), "review clause")
		assert.False(t, h.recordedDelegator(context.Background(), g.delegator, g.delegator, g.home), "recordedDelegator")
	})
	// A recorded user with a hub-wide system role: a super-admin passes the
	// admin clause and is admitted anywhere, so only the home-exists check
	// at the top of the review clause ends it once the home is deleted.
	t.Run("home project deleted, system-role sponsor", func(t *testing.T) {
		g := newReviewGrantFixture(t)
		h := newArtifactHost(g.srv)
		super := tid("sp-super")
		createRS3SuperAdmin(t, g.s, super)
		// The precondition the top check is pinned against: before the
		// delete the super-admin answers through the admin clause.
		require.Equal(t, artifacts.ReviewClauseProjectAdmin, g.reviewRecorded(h, g.user(super), g.agent.ID, g.home, super), "before the delete")
		require.NoError(t, g.s.DeleteProject(context.Background(), g.home))
		assert.Equal(t, "", g.reviewRecorded(h, g.user(super), g.agent.ID, g.home, super), "review clause")
		assert.Equal(t, "", g.stewardRecorded(h, g.user(super), g.agent.ID, g.home, super), "steward clause")
	})
}

// TestArtifactHostRecordedDelegatorChecks drives recordedDelegator
// directly: the local development user counts only while local-development
// authority is enabled, and a failed user, home project or admission
// lookup answers false.
func TestArtifactHostRecordedDelegatorChecks(t *testing.T) {
	f := newReviewGrantFixture(t)
	ctx := context.Background()

	t.Run("dev user needs dev-local authority", func(t *testing.T) {
		createDCUser(t, f.s, DevUserID, "dev@test.com", f.home, store.ProjectRoleMember)
		assert.False(t, hostOverStore(f.s, false).recordedDelegator(ctx, DevUserID, DevUserID, f.home), "dev-local authority off")
		assert.True(t, hostOverStore(f.s, true).recordedDelegator(ctx, DevUserID, DevUserID, f.home), "dev-local authority on")
		// Any other user is unaffected by the setting.
		assert.True(t, hostOverStore(f.s, false).recordedDelegator(ctx, f.delegator, f.delegator, f.home), "other user, dev-local authority off")
	})
	t.Run("user lookup fault fails closed", func(t *testing.T) {
		require.True(t, hostOverStore(f.s, false).recordedDelegator(ctx, f.delegator, f.delegator, f.home), "without the fault")
		faulty := hostOverStore(&sponsorFaultStore{Store: f.s, failUser: f.delegator}, false)
		assert.False(t, faulty.recordedDelegator(ctx, f.delegator, f.delegator, f.home))
	})
	t.Run("admission lookup fault fails closed", func(t *testing.T) {
		faulty := hostOverStore(&sponsorFaultStore{Store: f.s, failBindings: true}, false)
		assert.False(t, faulty.recordedDelegator(ctx, f.delegator, f.delegator, f.home))
	})
	t.Run("home project lookup fault fails closed", func(t *testing.T) {
		faulty := hostOverStore(&sponsorFaultStore{Store: f.s, failProject: f.home}, false)
		assert.False(t, faulty.recordedDelegator(ctx, f.delegator, f.delegator, f.home))
	})
	t.Run("missing user", func(t *testing.T) {
		assert.False(t, newArtifactHost(f.srv).recordedDelegator(ctx, tid("sp-nobody"), tid("sp-nobody"), f.home))
	})
	t.Run("caller is not the recorded user", func(t *testing.T) {
		assert.False(t, newArtifactHost(f.srv).recordedDelegator(ctx, f.member, f.delegator, f.home))
		assert.False(t, newArtifactHost(f.srv).recordedDelegator(ctx, "", "", f.home))
	})
}

// sponsorArtifactResponse GETs the artifact as identity and decodes it.
func sponsorArtifactResponse(t *testing.T, srv *Server, identity Identity, id string) (int, artifacts.ArtifactResponse) {
	t.Helper()
	rec := identityArtifactRequest(t, srv, identity, http.MethodGet, "/api/v1/artifacts/"+id, nil)
	var resp artifacts.ArtifactResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	}
	return rec.Code, resp
}

// TestArtifactRecordedDelegatorSurvivesAgentDeletion: an agent publishes;
// its delegating user is recorded and keeps steward sharing and Grant
// review access after the agent is soft-deleted and after it is purged,
// within the steward bounds (no admin grant, no move); another member does
// not gain them; after a move the clauses follow the new home.
func TestArtifactRecordedDelegatorSurvivesAgentDeletion(t *testing.T) {
	f := newReviewGrantFixture(t)
	st, _ := enableArtifactsForTest(t, f.srv)
	ctx := context.Background()
	agent := artifactTestAgent(f.agent.ID, f.home, ScopeProjectArtifactRead, ScopeProjectArtifactWrite)
	rec := identityArtifactRequest(t, f.srv, agent, http.MethodPost, "/api/v1/artifacts?name=report.md&scope="+f.home, []byte("# report\n"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var pub artifacts.ArtifactResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pub))
	id := pub.Artifact.ID
	stored, err := st.GetArtifact(ctx, id)
	require.NoError(t, err)
	require.Equal(t, f.delegator, stored.SponsorRef, "the delegating user is recorded at creation")

	delegator, member := f.user(f.delegator), f.user(f.member)
	path := "/api/v1/artifacts/" + id
	assertDelegator := func(stage string) {
		t.Helper()
		code, resp := sponsorArtifactResponse(t, f.srv, delegator, id)
		require.Equal(t, http.StatusOK, code, stage)
		assert.True(t, resp.CanShare, "%s: canShare", stage)
		assert.Equal(t, artifacts.StewardClauseDelegator, resp.ShareAuthority, "%s: shareAuthority", stage)
		assert.True(t, resp.CanGrantReview, "%s: canGrantReview", stage)
		assert.False(t, resp.CanManage, "%s: canManage", stage)
		code, resp = sponsorArtifactResponse(t, f.srv, member, id)
		require.Equal(t, http.StatusOK, code, stage)
		assert.False(t, resp.CanShare, "%s: member canShare", stage)
		assert.False(t, resp.CanGrantReview, "%s: member canGrantReview", stage)
	}
	assertDelegator("agent live")

	softDeleteAgentForSponsor(t, f.s, f.agent.ID)
	assertDelegator("agent soft-deleted")
	rec = identityArtifactRequest(t, f.srv, delegator, http.MethodPost, path+"/grants",
		[]byte(`{"subjectKind":"principal","subjectRef":"user:`+f.outside+`","permission":"read"}`))
	require.Equal(t, http.StatusCreated, rec.Code, "read grant after soft delete: %s", rec.Body.String())

	purgeAgentForSponsor(t, f.s, f.agent.ID)
	assertDelegator("agent purged")
	rec = identityArtifactRequest(t, f.srv, delegator, http.MethodPost, path+"/grants",
		[]byte(`{"subjectKind":"principal","subjectRef":"user:`+f.outside+`","permission":"write"}`))
	require.Equal(t, http.StatusOK, rec.Code, "write grant after purge: %s", rec.Body.String())
	rec = identityArtifactRequest(t, f.srv, delegator, http.MethodPost, path+"/grants",
		[]byte(`{"subjectKind":"principal","subjectRef":"user:`+f.admin+`","permission":"review"}`))
	require.Equal(t, http.StatusCreated, rec.Code, "review grant after purge: %s", rec.Body.String())
	for _, req := range []struct{ method, target, body string }{
		{http.MethodPost, path + "/grants", `{"subjectKind":"principal","subjectRef":"user:` + f.owner + `","permission":"admin"}`},
		{http.MethodPatch, path, `{"scopeRef":"` + f.other + `"}`},
	} {
		rec = identityArtifactRequest(t, f.srv, delegator, req.method, req.target, []byte(req.body))
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", req.method, req.body, rec.Body.String())
		assert.Contains(t, rec.Body.String(), artifacts.CodeStewardActionNotAllowed, "%s %s", req.method, req.body)
	}
	rec = identityArtifactRequest(t, f.srv, member, http.MethodPost, path+"/grants",
		[]byte(`{"subjectKind":"principal","subjectRef":"user:`+f.owner+`","permission":"read"}`))
	assert.Equal(t, http.StatusForbidden, rec.Code, "member share after purge: %s", rec.Body.String())
	evs, err := st.ListEvents(ctx, id)
	require.NoError(t, err)
	var stewardEvents int
	for _, e := range evs {
		if e.Action == artifacts.EventGrantCreated && e.ActorRef == f.delegator {
			assert.Equal(t, artifacts.AuthorityStewardDelegator, e.Authority)
			stewardEvents++
		}
	}
	assert.Positive(t, stewardEvents, "the delegator's grants are recorded with its steward authority")

	// An admin moves the artifact to other. The delegating user reads it
	// there through a principal read grant but holds no role in other: no
	// clause. Once it is a member of other, both clauses again.
	_, err = st.PutGrant(ctx, &artifacts.Grant{ID: tid("sp-delegator-read"), ArtifactID: id, SubjectKind: artifacts.SubjectPrincipal,
		SubjectRef: artifacts.PrincipalRef(artifacts.PrincipalKindUser, f.delegator), Permission: artifacts.GrantRead, CreatedAt: time.Now()},
		artifacts.MaxGrantsPerArtifact, true, &artifacts.Actor{Kind: artifacts.PrincipalKindUser, Ref: "test-setup", Authority: artifacts.AuthorityOwner})
	require.NoError(t, err)
	_, err = st.UpdateArtifact(ctx, id, artifacts.ArtifactUpdate{
		HomeGrant: &artifacts.Grant{ID: tid("sp-move-home"), ArtifactID: id, SubjectKind: artifacts.SubjectScope, SubjectRef: f.other,
			Permission: artifacts.GrantRead, CreatedAt: time.Now()},
		MaxGrants: artifacts.MaxGrantsPerArtifact,
		Actor:     &artifacts.Actor{Kind: artifacts.PrincipalKindUser, Ref: "test-setup", Authority: artifacts.AuthorityOwner},
	})
	require.NoError(t, err)
	code, resp := sponsorArtifactResponse(t, f.srv, delegator, id)
	require.Equal(t, http.StatusOK, code)
	assert.False(t, resp.CanShare, "moved, no role in the new home: canShare")
	assert.False(t, resp.CanGrantReview, "moved, no role in the new home: canGrantReview")
	createDCUser(t, f.s, f.delegator, "rg-delegator@test.com", f.other, store.ProjectRoleMember)
	code, resp = sponsorArtifactResponse(t, f.srv, delegator, id)
	require.Equal(t, http.StatusOK, code)
	assert.True(t, resp.CanShare, "moved, member of the new home: canShare")
	assert.Equal(t, artifacts.StewardClauseDelegator, resp.ShareAuthority)
	assert.True(t, resp.CanGrantReview, "moved, member of the new home: canGrantReview")
}

// TestArtifactUnrecordedDelegatorFallsBackToLive: an artifact with nothing
// recorded (created before sponsors were recorded) is decided by the live
// chain: the delegating user holds both clauses while the agent is live
// and loses them when it is deleted.
func TestArtifactUnrecordedDelegatorFallsBackToLive(t *testing.T) {
	f := newReviewGrantFixture(t)
	st, blobs := enableArtifactsForTest(t, f.srv)
	id := reviewGrantArtifact(t, f, st, blobs)
	stored, err := st.GetArtifact(context.Background(), id)
	require.NoError(t, err)
	require.Empty(t, stored.SponsorRef)
	delegator := f.user(f.delegator)

	code, resp := sponsorArtifactResponse(t, f.srv, delegator, id)
	require.Equal(t, http.StatusOK, code)
	assert.True(t, resp.CanShare)
	assert.Equal(t, artifacts.StewardClauseDelegator, resp.ShareAuthority)
	assert.True(t, resp.CanGrantReview)

	softDeleteAgentForSponsor(t, f.s, f.agent.ID)
	code, resp = sponsorArtifactResponse(t, f.srv, delegator, id)
	require.Equal(t, http.StatusOK, code)
	assert.False(t, resp.CanShare, "live fallback after delete")
	assert.False(t, resp.CanGrantReview, "live fallback after delete")
}

// sponsorSeedArtifact stores a published artifact owned by agentID in home
// with nothing recorded, as an artifact created before sponsors were.
func sponsorSeedArtifact(t *testing.T, st artifacts.Store, agentID, home string) string {
	t.Helper()
	now := time.Now()
	a := &artifacts.Artifact{ID: uuid.NewString(), ScopeKind: artifacts.ScopeKindProject, ScopeRef: home,
		OwnerKind: artifacts.PrincipalKindAgent, OwnerRef: agentID, Title: "old", CreatedAt: now, UpdatedAt: now}
	v := &artifacts.Version{ID: uuid.NewString(), ArtifactID: a.ID, Seq: 1, Kind: artifacts.VersionKindPublish,
		EntryPath: "old.md", TotalBytes: 1, FileCount: 1, CreatedAt: now, State: artifacts.VersionStateReady}
	require.NoError(t, st.CreatePublished(context.Background(), a, v,
		[]artifacts.File{{VersionID: v.ID, Path: "old.md", Size: 1, SHA256: uuid.NewString(), MediaType: "text/markdown"}}, nil))
	return a.ID
}

func sponsorOf(t *testing.T, st artifacts.Store, id string) string {
	t.Helper()
	a, err := st.GetArtifact(context.Background(), id)
	require.NoError(t, err)
	return a.SponsorRef
}

// TestArtifactSponsorBackfillOnHub: the maintenance pass records the
// delegating user on the older artifacts of live agents, leaves those of a
// deleted agent and of an agent with no delegation unrecorded, and records
// the backfill as done.
func TestArtifactSponsorBackfillOnHub(t *testing.T) {
	f := newReviewGrantFixture(t)
	st, _ := enableArtifactsForTest(t, f.srv)
	ctx := context.Background()
	live := sponsorSeedArtifact(t, st, f.agent.ID, f.home)
	child := sponsorSeedArtifact(t, st, f.child.ID, f.home)
	deleted := sponsorSeedArtifact(t, st, f.sibling.ID, f.home)
	noEdge := sponsorSeedArtifact(t, st, f.noEdge.ID, f.home)
	softDeleteAgentForSponsor(t, f.s, f.sibling.ID)

	f.srv.reapArtifactVersions(ctx)
	assert.Equal(t, f.delegator, sponsorOf(t, st, live), "live agent")
	assert.Equal(t, f.delegator, sponsorOf(t, st, child), "agent-spawned agent")
	assert.Equal(t, "", sponsorOf(t, st, deleted), "soft-deleted agent")
	assert.Equal(t, "", sponsorOf(t, st, noEdge), "agent with no delegation")
	done, err := st.SponsorBackfillDone(ctx)
	require.NoError(t, err)
	assert.True(t, done)

	// Backfilled, the delegating user keeps the clauses after a purge.
	softDeleteAgentForSponsor(t, f.s, f.agent.ID)
	purgeAgentForSponsor(t, f.s, f.agent.ID)
	code, resp := sponsorArtifactResponse(t, f.srv, f.user(f.delegator), live)
	require.Equal(t, http.StatusOK, code)
	assert.True(t, resp.CanShare)
	assert.True(t, resp.CanGrantReview)
}

// TestArtifactSponsorBackfillLookupFault: a store fault while resolving an
// agent stops the backfill without recording it done; an agent that
// resolves to no one (a provenance denial) does not, and a later pass
// completes.
func TestArtifactSponsorBackfillLookupFault(t *testing.T) {
	f := newReviewGrantFixture(t)
	st, _ := enableArtifactsForTest(t, f.srv)
	ctx := context.Background()
	live := sponsorSeedArtifact(t, st, f.agent.ID, f.home)
	noEdge := sponsorSeedArtifact(t, st, f.noEdge.ID, f.home)

	faulty := hostOverStore(&sponsorFaultStore{Store: f.s, failEdgesFor: f.agent.ID}, false)
	res, err := artifacts.BackfillSponsors(ctx, st, faulty.DelegatingUser, 10, time.Now())
	require.ErrorIs(t, err, errSponsorStoreFault)
	assert.False(t, res.Done)
	done, err := st.SponsorBackfillDone(ctx)
	require.NoError(t, err)
	assert.False(t, done, "a lookup fault must not record the backfill done")
	assert.Equal(t, "", sponsorOf(t, st, live))

	res, err = artifacts.BackfillSponsors(ctx, st, hostOverStore(f.s, false).DelegatingUser, 10, time.Now())
	require.NoError(t, err)
	assert.True(t, res.Done)
	assert.Equal(t, f.delegator, sponsorOf(t, st, live))
	assert.Equal(t, "", sponsorOf(t, st, noEdge), "a denial records nothing and does not block completion")
}

// TestArtifactHostStorelessFailsClosed: a host whose server has an
// authorization service but no store answers no clause from either clause
// and an error from ScopesExist, and never panics (a panic is recovered and
// reported as a failure, so a missing nil guard fails this test rather
// than crashing the binary).
func TestArtifactHostStorelessFailsClosed(t *testing.T) {
	f := newReviewGrantFixture(t)
	host := newArtifactHost(&Server{authzService: NewAuthzService(f.s, slog.Default())})
	ctx := contextWithIdentity(context.Background(), f.user(f.delegator))
	call := func(name string, fn func() string) string {
		t.Helper()
		var got string
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s panicked on a store-less host: %v", name, r)
					got = "panic"
				}
			}()
			got = fn()
		}()
		return got
	}
	assert.Equal(t, "", call("StewardClause", func() string {
		return host.StewardClause(ctx, f.agent.ID, f.home, f.delegator, false)
	}), "StewardClause")
	assert.Equal(t, "", call("ReviewGrantClause", func() string {
		return host.ReviewGrantClause(ctx, f.agent.ID, f.home, f.delegator)
	}), "ReviewGrantClause")
	assert.Equal(t, "", call("ScopesExist", func() string {
		exists, err := host.ScopesExist(ctx, []string{f.home})
		if err == nil || exists[f.home] {
			return "no error"
		}
		return ""
	}), "ScopesExist answers an error and no project")
	exists, err := host.ScopesExist(ctx, nil)
	require.NoError(t, err, "no refs needs no store")
	assert.Empty(t, exists)
}
