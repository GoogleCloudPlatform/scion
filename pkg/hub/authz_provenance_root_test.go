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
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fixtures ---

// recordedProv returns version-1 provenance whose source is the edge's
// delegator, as every recording write produces.
func recordedProv(kind string, id string, cred store.SourceCredentialKind) store.AuthorityProvenance {
	return store.AuthorityProvenance{
		ProvenanceVersion:    store.ProvenanceVersionV1,
		SourcePrincipalKind:  kind,
		SourcePrincipalID:    id,
		SourceCredentialKind: cred,
	}
}

// schedulerProv returns consistent scheduler provenance for a delegator.
func schedulerProv(kind, id, initiatorCred string) store.AuthorityProvenance {
	p := store.AuthorityProvenance{
		ProvenanceVersion:           store.ProvenanceVersionV1,
		SourcePrincipalKind:         kind,
		SourcePrincipalID:           id,
		SourceCredentialKind:        store.SourceCredentialScheduler,
		SourceEventID:               "evt-" + id,
		SourceScheduleID:            "sch-" + id,
		SourceAuthorizationRevision: 1,
		InitiatorPrincipalKind:      kind,
		InitiatorPrincipalID:        id,
		InitiatorCredentialKind:     initiatorCred,
		InitiatorCredentialID:       "",
	}
	if initiatorCred == store.InitiatorCredentialKindDevLocal {
		p.InitiatorPrincipalKind = string(PrincipalKindDev)
	}
	return p
}

// provenanceFixture is a project with an admitted owner user, and an
// AuthzService with local development authority as requested.
type provenanceFixture struct {
	ceilingFixture
	a *AuthzService
}

func newProvenanceFixture(t *testing.T, name string, devLocal bool) provenanceFixture {
	t.Helper()
	f := newCeilingFixture(t, "pr-"+name)
	return provenanceFixture{ceilingFixture: f, a: f.authz(f.store, devLocal, false)}
}

// userChild creates an agent whose single edge is a recorded session edge
// from the fixture user.
func (f provenanceFixture) userChild(t *testing.T, name string, c store.EffectCeiling) *store.Agent {
	t.Helper()
	ag := f.agent(t, name, AgentRoleFull)
	f.edge(t, store.DelegationPrincipalUser, f.userID, ag.ID, c, recordedProv(store.DelegationPrincipalUser, f.userID, store.SourceCredentialSession))
	return ag
}

// agentChild creates an agent whose single edge is a recorded agent edge
// from parent.
func (f provenanceFixture) agentChild(t *testing.T, name string, parent *store.Agent, c store.EffectCeiling) *store.Agent {
	t.Helper()
	ag := f.agent(t, name, AgentRoleFull)
	f.edge(t, store.DelegationPrincipalAgent, parent.ID, ag.ID, c, recordedProv(store.DelegationPrincipalAgent, parent.ID, store.SourceCredentialAgent))
	return ag
}

func (f provenanceFixture) resolve(t *testing.T, agentID string) (RecordedProvenanceRoot, error) {
	t.Helper()
	return f.a.ResolveProvenanceRoot(context.Background(), agentID, ResolveProvenanceOptions{PermissionID: "agent.create"})
}

// devUserInProject creates the local development user as an owner of the
// fixture project with the given status.
func (f provenanceFixture) devUserInProject(t *testing.T, status string) {
	t.Helper()
	createDCUser(t, f.store, DevUserID, "dev@localhost", f.projectID, store.ProjectRoleOwner)
	setUserStatus(t, f.store, DevUserID, status)
}

// rewriteEdgeStore rewrites edges read for one delegate.
type rewriteEdgeStore struct {
	store.Store
	delegateID string
	rewrite    func(e *store.DelegationEdge)
}

func (s *rewriteEdgeStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	edges, err := s.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
	if err != nil || delegateID != s.delegateID {
		return edges, err
	}
	out := make([]*store.DelegationEdge, 0, len(edges))
	for _, e := range edges {
		cp := *e
		s.rewrite(&cp)
		out = append(out, &cp)
	}
	return out, nil
}

var errProvenanceStoreFault = errors.New("store fault")

// faultStore fails the named lookups.
type faultStore struct {
	store.Store
	edges, users, agents bool
	userID               string // when set, only GetUser(userID) fails
}

func (s *faultStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	if s.edges {
		return nil, errProvenanceStoreFault
	}
	return s.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
}

func (s *faultStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if s.users && (s.userID == "" || s.userID == id) {
		return nil, errProvenanceStoreFault
	}
	return s.Store.GetUser(ctx, id)
}

func (s *faultStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if s.agents {
		return nil, errProvenanceStoreFault
	}
	return s.Store.GetAgent(ctx, id)
}

// --- resolver: admit paths ---

func TestProvenanceRoot_UserRoot(t *testing.T) {
	f := newProvenanceFixture(t, "user-root", false)
	ag := f.userChild(t, "user-root", ceilPrincip)
	root, err := f.resolve(t, ag.ID)
	require.NoError(t, err)
	assert.Equal(t, ProvenancePrincipal{Kind: store.DelegationPrincipalUser, ID: f.userID}, root.Principal)
	require.NotNil(t, root.RootUser)
	assert.Equal(t, f.userID, root.RootUser.ID)
	assert.Equal(t, store.EffectCeilingPrincipal, root.Ceiling.Kind)
	assert.Equal(t, store.SourceCredentialSession, root.Provenance.SourceCredentialKind)
	assert.Equal(t, ag.ID, root.Edge.DelegateID)
	assert.Zero(t, root.Revision)
}

func TestProvenanceRoot_AgentChain(t *testing.T) {
	f := newProvenanceFixture(t, "agent-chain", false)
	p := f.userChild(t, "agent-chain-p", ceilPrincip)
	c := f.agentChild(t, "agent-chain-c", p, boundedCeiling("agent.create", "project.read"))
	root, err := f.resolve(t, c.ID)
	require.NoError(t, err)
	assert.Equal(t, ProvenancePrincipal{Kind: store.DelegationPrincipalAgent, ID: p.ID}, root.Principal)
	assert.Equal(t, f.userID, root.RootUser.ID)
	assert.Equal(t, store.EffectCeilingBounded, root.Ceiling.Kind)
}

func TestProvenanceRoot_SchedulerEdgeConsistent(t *testing.T) {
	for _, cred := range []string{store.InitiatorCredentialKindSession, store.InitiatorCredentialKindUAT} {
		t.Run(cred, func(t *testing.T) {
			f := newProvenanceFixture(t, "sched-ok-"+cred, false)
			ag := f.agent(t, "sched-ok-"+cred, AgentRoleFull)
			c := ceilPrincip
			if cred == store.InitiatorCredentialKindUAT {
				c = boundedCeiling("agent.create")
			}
			prov := schedulerProv(store.DelegationPrincipalUser, f.userID, cred)
			f.edge(t, store.DelegationPrincipalUser, f.userID, ag.ID, c, prov)
			root, err := f.resolve(t, ag.ID)
			require.NoError(t, err)
			assert.Equal(t, ProvenanceRevision{ScheduleID: prov.SourceScheduleID, EventID: prov.SourceEventID, AuthorizationRevision: 1}, root.Revision)
		})
	}
	t.Run("agent", func(t *testing.T) {
		f := newProvenanceFixture(t, "sched-ok-agent", false)
		p := f.userChild(t, "sched-ok-agent-p", ceilPrincip)
		ag := f.agent(t, "sched-ok-agent-c", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalAgent, p.ID, ag.ID, boundedCeiling("agent.create"), schedulerProv(store.DelegationPrincipalAgent, p.ID, store.InitiatorCredentialKindAgent))
		root, err := f.resolve(t, ag.ID)
		require.NoError(t, err)
		assert.Equal(t, p.ID, root.Principal.ID)
	})
	t.Run("dev_local", func(t *testing.T) {
		f := newProvenanceFixture(t, "sched-ok-dev", true)
		f.devUserInProject(t, store.UserStatusActive)
		ag := f.agent(t, "sched-ok-dev", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, DevUserID, ag.ID, ceilPrincip, schedulerProv(store.DelegationPrincipalUser, DevUserID, store.InitiatorCredentialKindDevLocal))
		root, err := f.resolve(t, ag.ID)
		require.NoError(t, err)
		assert.Equal(t, DevUserID, root.RootUser.ID)
	})
}

// An edge whose ceiling and provenance come from the scheduler writer
// (scheduledEffectCeiling) resolves through resolveProvenanceChain, for
// every principal and initiator credential the writer records.
func TestProvenanceRoot_SchedulerWriterRoundTrip(t *testing.T) {
	ctx := context.Background()
	scheduledEvent := func(kind, id, cred string) store.ScheduledEvent {
		evt := store.ScheduledEvent{ID: "evt-rt-" + cred, ScheduleID: "sch-rt-" + cred}
		evt.InitiatorPrincipalKind = kind
		evt.InitiatorPrincipalID = id
		evt.InitiatorCredentialKind = cred
		evt.InitiatorCredentialID = "cred-rt-" + cred
		evt.AuthorizationRevision = 3
		return evt
	}
	writeAndResolve := func(t *testing.T, f provenanceFixture, srv *Server, auth ScheduledAuthority, evt store.ScheduledEvent) RecordedProvenanceRoot {
		t.Helper()
		c, prov, err := srv.scheduledEffectCeiling(ctx, auth, evt)
		require.NoError(t, err)
		ag := f.agent(t, "rt-"+auth.CredentialKind, AgentRoleFull)
		f.edge(t, auth.PrincipalKind, auth.PrincipalID, ag.ID, c, prov)
		root, err := f.a.resolveProvenanceChain(ctx, ag, false)
		require.NoError(t, err)
		assert.Equal(t, ProvenanceRevision{ScheduleID: evt.ScheduleID, EventID: evt.ID, AuthorizationRevision: evt.AuthorizationRevision}, root.Revision)
		assert.Equal(t, ProvenancePrincipal{Kind: auth.PrincipalKind, ID: auth.PrincipalID}, root.Principal)
		return root
	}

	for _, cred := range []string{store.InitiatorCredentialKindSession, store.InitiatorCredentialKindUAT} {
		t.Run(cred, func(t *testing.T) {
			f := newProvenanceFixture(t, "sched-rt-"+cred, false)
			c := ceilPrincip
			if cred == store.InitiatorCredentialKindUAT {
				c = boundedCeiling("agent.create")
			}
			auth := ScheduledAuthority{PrincipalKind: store.DelegationPrincipalUser, PrincipalID: f.userID, CredentialKind: cred, Ceiling: c}
			root := writeAndResolve(t, f, &Server{authzService: f.a}, auth, scheduledEvent(store.DelegationPrincipalUser, f.userID, cred))
			require.NotNil(t, root.RootUser)
			assert.Equal(t, f.userID, root.RootUser.ID)
		})
	}
	t.Run("agent", func(t *testing.T) {
		f := newProvenanceFixture(t, "sched-rt-agent", false)
		p := f.userChild(t, "sched-rt-agent-p", ceilPrincip)
		auth := ScheduledAuthority{PrincipalKind: store.DelegationPrincipalAgent, PrincipalID: p.ID, CredentialKind: store.InitiatorCredentialKindAgent, Ceiling: boundedCeiling("agent.create"), agent: p}
		root := writeAndResolve(t, f, &Server{authzService: f.a}, auth, scheduledEvent(store.DelegationPrincipalAgent, p.ID, store.InitiatorCredentialKindAgent))
		require.NotNil(t, root.RootUser)
		assert.Equal(t, f.userID, root.RootUser.ID)
	})
	t.Run("dev_local", func(t *testing.T) {
		f := newProvenanceFixture(t, "sched-rt-dev", true)
		f.devUserInProject(t, store.UserStatusActive)
		auth := ScheduledAuthority{PrincipalKind: store.DelegationPrincipalUser, PrincipalID: DevUserID, CredentialKind: store.InitiatorCredentialKindDevLocal, Ceiling: ceilPrincip}
		root := writeAndResolve(t, f, &Server{authzService: f.a}, auth, scheduledEvent(string(PrincipalKindDev), DevUserID, store.InitiatorCredentialKindDevLocal))
		require.NotNil(t, root.RootUser)
		assert.Equal(t, DevUserID, root.RootUser.ID)
	})
}

// The seeded local development user row is active and resolves as the
// provenance root of a dev_local edge.
func TestDevUserSeededRowResolvesAsRoot(t *testing.T) {
	f := newProvenanceFixture(t, "dev-seeded", true)
	ctx := context.Background()
	seedDevUser(ctx, f.store, DevUserConfig{})
	u, err := f.store.GetUser(ctx, DevUserID)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusActive, u.Status)

	ag := f.agent(t, "dev-seeded", AgentRoleFull)
	f.edge(t, store.DelegationPrincipalUser, DevUserID, ag.ID, ceilPrincip, recordedProv(store.DelegationPrincipalUser, DevUserID, store.SourceCredentialDevLocal))
	root, err := f.resolve(t, ag.ID)
	require.NoError(t, err)
	assert.Equal(t, DevUserID, root.RootUser.ID)
	assert.Equal(t, ProvenancePrincipal{Kind: store.DelegationPrincipalUser, ID: DevUserID}, root.Principal)
}

// --- resolver: deny matrix ---

func TestProvenanceRoot_EmptyPermissionID(t *testing.T) {
	f := newProvenanceFixture(t, "empty-perm", false)
	ag := f.userChild(t, "empty-perm", ceilPrincip)
	ctx := context.Background()
	for _, perm := range []string{"", "not.a.permission"} {
		_, err := f.a.ResolveProvenanceRoot(ctx, ag.ID, ResolveProvenanceOptions{PermissionID: perm})
		assert.ErrorIs(t, err, ErrProvenanceRequest, "permission %q", perm)
		assert.Equal(t, DenyCauseCeilingError, DenyCauseForProvenanceError(err))
	}
	_, err := f.a.ResolveProvenanceRoot(ctx, "", ResolveProvenanceOptions{PermissionID: "agent.create"})
	assert.ErrorIs(t, err, ErrProvenanceRequest)
}

func TestProvenanceRoot_ZeroEdges(t *testing.T) {
	f := newProvenanceFixture(t, "zero-edges", false)
	ag := f.agent(t, "zero-edges", AgentRoleFull)
	_, err := f.resolve(t, ag.ID)
	assert.ErrorIs(t, err, ErrProvenanceMissing)
	assert.Equal(t, DenyCauseCeilingOrphaned, DenyCauseForProvenanceError(err))

	t.Run("edge in another project only", func(t *testing.T) {
		f := newProvenanceFixture(t, "zero-edges-xp", false)
		ag := f.agent(t, "zero-edges-xp", AgentRoleFull)
		other := tid("pr-other-proj")
		createDCProject(t, f.store, other, "pr-other")
		require.NoError(t, f.store.CreateDelegationEdge(context.Background(), &store.DelegationEdge{
			DelegatorType: store.DelegationPrincipalUser, DelegatorID: f.userID,
			DelegateType: store.DelegationPrincipalAgent, DelegateID: ag.ID,
			ScopeType: store.RoleScopeProject, ScopeID: other, Role: string(AgentRoleFull), Active: true,
			AuthorityProvenance: recordedProv(store.DelegationPrincipalUser, f.userID, store.SourceCredentialSession),
			EffectCeiling:       ceilPrincip,
		}))
		_, err := f.resolve(t, ag.ID)
		assert.ErrorIs(t, err, ErrProvenanceMissing)
	})
}

func TestProvenanceRoot_TwoEdges(t *testing.T) {
	f := newProvenanceFixture(t, "two-edges", false)
	ag := f.userChild(t, "two-edges", ceilPrincip)
	a := f.authz(&ambiguousEdgeStore{Store: f.store, dupID: ag.ID}, false, false)
	_, err := a.ResolveProvenanceRoot(context.Background(), ag.ID, ResolveProvenanceOptions{PermissionID: "agent.create"})
	assert.ErrorIs(t, err, ErrProvenanceAmbiguous)
	assert.Equal(t, DenyCauseCeilingOrphaned, DenyCauseForProvenanceError(err))
}

func TestProvenanceRoot_TypeMismatch(t *testing.T) {
	f := newProvenanceFixture(t, "type-mismatch", false)
	ag := f.agent(t, "type-mismatch", AgentRoleFull)
	f.edge(t, store.DelegationPrincipalUser, f.userID, ag.ID, ceilPrincip, recordedProv(store.DelegationPrincipalAgent, f.userID, store.SourceCredentialSession))
	_, err := f.resolve(t, ag.ID)
	assert.ErrorIs(t, err, ErrProvenanceMismatch)
	assert.Equal(t, DenyCauseCeilingOrphaned, DenyCauseForProvenanceError(err))
}

// A delegator type outside {user, agent} is a shape no writer produces; it
// is seeded by rewriting the stored edge on read.
func TestProvenanceRoot_UnsupportedDelegatorType(t *testing.T) {
	f := newProvenanceFixture(t, "bad-type", false)
	ag := f.userChild(t, "bad-type", ceilPrincip)
	s := &rewriteEdgeStore{Store: f.store, delegateID: ag.ID, rewrite: func(e *store.DelegationEdge) {
		e.DelegatorType = "group"
		e.SourcePrincipalKind = "group"
	}}
	_, err := f.authz(s, false, false).ResolveProvenanceRoot(context.Background(), ag.ID, ResolveProvenanceOptions{PermissionID: "agent.create"})
	assert.ErrorIs(t, err, ErrProvenanceMismatch)
}

func TestProvenanceRoot_SchedulerEdgeInconsistent(t *testing.T) {
	cases := map[string]func(p *store.AuthorityProvenance, c *store.EffectCeiling){
		"no event":      func(p *store.AuthorityProvenance, _ *store.EffectCeiling) { p.SourceEventID = "" },
		"revision zero": func(p *store.AuthorityProvenance, _ *store.EffectCeiling) { p.SourceAuthorizationRevision = 0 },
		"legacy initiator": func(p *store.AuthorityProvenance, _ *store.EffectCeiling) {
			p.InitiatorCredentialKind = store.InitiatorCredentialKindLegacyUnknown
		},
		"empty initiator":    func(p *store.AuthorityProvenance, _ *store.EffectCeiling) { p.InitiatorCredentialKind = "" },
		"unrecorded ceiling": func(_ *store.AuthorityProvenance, c *store.EffectCeiling) { *c = store.EffectCeiling{} },
		"scheduler initiator": func(p *store.AuthorityProvenance, _ *store.EffectCeiling) {
			p.InitiatorCredentialKind = string(store.SourceCredentialScheduler)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newProvenanceFixture(t, "sched-bad-"+strings.ReplaceAll(name, " ", "-"), false)
			ag := f.agent(t, "sched-bad-"+strings.ReplaceAll(name, " ", "-"), AgentRoleFull)
			p := schedulerProv(store.DelegationPrincipalUser, f.userID, store.InitiatorCredentialKindSession)
			c := ceilPrincip
			mutate(&p, &c)
			f.edge(t, store.DelegationPrincipalUser, f.userID, ag.ID, c, p)
			// Accepting unrecorded hops does not accept an inconsistent
			// scheduler edge.
			_, err := f.a.ResolveProvenanceRoot(context.Background(), ag.ID, ResolveProvenanceOptions{PermissionID: "agent.create", AllowUnrecordedLegacy: true})
			assert.ErrorIs(t, err, ErrProvenanceMismatch)
		})
	}
}

func TestProvenanceRoot_InitiatorMismatch(t *testing.T) {
	cases := map[string]func(p *store.AuthorityProvenance){
		"initiator id":   func(p *store.AuthorityProvenance) { p.InitiatorPrincipalID = "someone-else" },
		"initiator kind": func(p *store.AuthorityProvenance) { p.InitiatorPrincipalKind = store.DelegationPrincipalAgent },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newProvenanceFixture(t, "init-mm-"+strings.ReplaceAll(name, " ", "-"), false)
			ag := f.agent(t, "init-mm-"+strings.ReplaceAll(name, " ", "-"), AgentRoleFull)
			p := schedulerProv(store.DelegationPrincipalUser, f.userID, store.InitiatorCredentialKindSession)
			mutate(&p)
			f.edge(t, store.DelegationPrincipalUser, f.userID, ag.ID, ceilPrincip, p)
			_, err := f.resolve(t, ag.ID)
			assert.ErrorIs(t, err, ErrProvenanceMismatch)
		})
	}
}

func TestProvenanceRoot_UserInactive(t *testing.T) {
	f := newProvenanceFixture(t, "user-inactive", false)
	ag := f.userChild(t, "user-inactive", ceilPrincip)
	setUserStatus(t, f.store, f.userID, "suspended")
	_, err := f.resolve(t, ag.ID)
	assert.ErrorIs(t, err, ErrProvenanceInactive)
	assert.Equal(t, DenyCauseCeilingOrphaned, DenyCauseForProvenanceError(err))
}

func TestProvenanceRoot_UserNotAdmittedForPermission(t *testing.T) {
	for _, perm := range []string{"agent.create", "secret.deliver"} {
		t.Run(perm, func(t *testing.T) {
			slug := strings.ReplaceAll(perm, ".", "-")
			f := newProvenanceFixture(t, "not-admitted-"+slug, false)
			outsider := tid("pr-outsider-" + slug)
			other := tid("pr-outsider-proj-" + slug)
			createDCProject(t, f.store, other, "pr-op-"+slug)
			createDCUser(t, f.store, outsider, "outsider-"+slug+"@test.com", other, store.ProjectRoleOwner)
			ag := f.agent(t, "not-admitted-"+slug, AgentRoleFull)
			f.edge(t, store.DelegationPrincipalUser, outsider, ag.ID, ceilPrincip, recordedProv(store.DelegationPrincipalUser, outsider, store.SourceCredentialSession))
			_, err := f.a.ResolveProvenanceRoot(context.Background(), ag.ID, ResolveProvenanceOptions{PermissionID: perm})
			assert.ErrorIs(t, err, ErrProvenanceNotAdmitted)
			assert.Equal(t, DenyCauseCeilingDelegatorLacksPermission, DenyCauseForProvenanceError(err))

			// The project owner of the agent's project is admitted.
			owned := f.userChild(t, "admitted-"+slug, ceilPrincip)
			_, err = f.a.ResolveProvenanceRoot(context.Background(), owned.ID, ResolveProvenanceOptions{PermissionID: perm})
			assert.NoError(t, err)
		})
	}
}

func TestProvenanceRoot_AgentDeleted(t *testing.T) {
	t.Run("target", func(t *testing.T) {
		f := newProvenanceFixture(t, "deleted-target", false)
		ag := f.userChild(t, "deleted-target", ceilPrincip)
		softDeleteStoredAgent(t, f.store, ag.ID)
		_, err := f.resolve(t, ag.ID)
		assert.ErrorIs(t, err, ErrProvenanceInactive)
	})
	t.Run("intermediate", func(t *testing.T) {
		f := newProvenanceFixture(t, "deleted-mid", false)
		p := f.userChild(t, "deleted-mid-p", ceilPrincip)
		c := f.agentChild(t, "deleted-mid-c", p, boundedCeiling("agent.create"))
		softDeleteStoredAgent(t, f.store, p.ID)
		_, err := f.resolve(t, c.ID)
		assert.ErrorIs(t, err, ErrProvenanceInactive)
	})
	t.Run("unknown agent", func(t *testing.T) {
		f := newProvenanceFixture(t, "deleted-unknown", false)
		_, err := f.resolve(t, tid("pr-no-such-agent"))
		assert.ErrorIs(t, err, ErrProvenanceInactive)
	})
}

func TestProvenanceRoot_Sentinel(t *testing.T) {
	f := newProvenanceFixture(t, "sentinel", false)
	ag := f.agent(t, "sentinel", AgentRoleFull)
	f.edge(t, store.DelegationPrincipalUser, migrationDelegatorID, ag.ID, store.EffectCeiling{}, store.AuthorityProvenance{})
	for _, allow := range []bool{false, true} {
		_, err := f.a.ResolveProvenanceRoot(context.Background(), ag.ID, ResolveProvenanceOptions{PermissionID: "agent.create", AllowUnrecordedLegacy: allow})
		assert.ErrorIs(t, err, ErrProvenanceChain, "allow=%v", allow)
	}
}

func TestProvenanceRoot_Cycle(t *testing.T) {
	f := newProvenanceFixture(t, "cycle", false)
	x := f.agent(t, "cycle-x", AgentRoleFull)
	y := f.agent(t, "cycle-y", AgentRoleFull)
	f.edge(t, store.DelegationPrincipalAgent, y.ID, x.ID, boundedCeiling("agent.create"), recordedProv(store.DelegationPrincipalAgent, y.ID, store.SourceCredentialAgent))
	f.edge(t, store.DelegationPrincipalAgent, x.ID, y.ID, boundedCeiling("agent.create"), recordedProv(store.DelegationPrincipalAgent, x.ID, store.SourceCredentialAgent))
	_, err := f.resolve(t, x.ID)
	assert.ErrorIs(t, err, ErrProvenanceChain)
}

func TestProvenanceRoot_DepthExceeded(t *testing.T) {
	f := newProvenanceFixture(t, "depth", false)
	prev := f.userChild(t, "depth-0", ceilPrincip)
	// The chain of maxDelegationDepth+1 edges resolves; one more does not.
	for i := 1; i <= maxDelegationDepth; i++ {
		prev = f.agentChild(t, fmt.Sprintf("depth-%d", i), prev, boundedCeiling("agent.create"))
	}
	_, err := f.resolve(t, prev.ID)
	require.NoError(t, err)
	deepest := f.agentChild(t, "depth-over", prev, boundedCeiling("agent.create"))
	_, err = f.resolve(t, deepest.ID)
	assert.ErrorIs(t, err, ErrProvenanceChain)
}

func TestProvenanceRoot_LookupError(t *testing.T) {
	cases := map[string]*faultStore{
		"edges":  {edges: true},
		"users":  {users: true},
		"agents": {agents: true},
	}
	for name, fs := range cases {
		t.Run(name, func(t *testing.T) {
			f := newProvenanceFixture(t, "lookup-"+name, false)
			p := f.userChild(t, "lookup-"+name+"-p", ceilPrincip)
			c := f.agentChild(t, "lookup-"+name+"-c", p, boundedCeiling("agent.create"))
			// The target agent row is read before the fault for "agents",
			// so resolve the chain directly from the loaded row.
			fs.Store = f.store
			a := f.authz(fs, false, false)
			_, err := a.resolveProvenanceChain(context.Background(), c, false)
			require.Error(t, err)
			assert.ErrorIs(t, err, errProvenanceStoreFault)
			assert.False(t, isStructuralProvenanceError(err))
			assert.Equal(t, DenyCauseCeilingError, DenyCauseForProvenanceError(err))
		})
	}
}

func TestProvenanceRoot_UnrecordedDeniedByDefault(t *testing.T) {
	cases := map[string]struct {
		c store.EffectCeiling
		p store.AuthorityProvenance
	}{
		"unrecorded edge":           {store.EffectCeiling{}, store.AuthorityProvenance{}},
		"provenance version 0":      {ceilPrincip, store.AuthorityProvenance{}},
		"provenance version 2":      {ceilPrincip, store.AuthorityProvenance{ProvenanceVersion: 2}},
		"bounded under version 2":   {boundedCeiling("agent.create"), store.AuthorityProvenance{ProvenanceVersion: 2}},
		"recorded kind, unrecorded": {store.EffectCeiling{}, store.AuthorityProvenance{ProvenanceVersion: 1, SourcePrincipalKind: store.DelegationPrincipalUser, SourceCredentialKind: store.SourceCredentialSession}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			slug := strings.ReplaceAll(strings.ReplaceAll(name, " ", "-"), ",", "")
			f := newProvenanceFixture(t, "unrec-"+slug, false)
			ag := f.agent(t, "unrec-"+slug, AgentRoleFull)
			f.edge(t, store.DelegationPrincipalUser, f.userID, ag.ID, tc.c, tc.p)
			_, err := f.resolve(t, ag.ID)
			assert.ErrorIs(t, err, ErrProvenanceUnrecorded)
			assert.Equal(t, DenyCauseCeilingUnrecorded, DenyCauseForProvenanceError(err))

			root, err := f.a.ResolveProvenanceRoot(context.Background(), ag.ID, ResolveProvenanceOptions{PermissionID: "agent.create", AllowUnrecordedLegacy: true})
			require.NoError(t, err)
			assert.Equal(t, f.userID, root.RootUser.ID)
		})
	}
	t.Run("unrecorded intermediate hop", func(t *testing.T) {
		f := newProvenanceFixture(t, "unrec-mid", false)
		p := f.agent(t, "unrec-mid-p", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, f.userID, p.ID, store.EffectCeiling{}, store.AuthorityProvenance{})
		c := f.agentChild(t, "unrec-mid-c", p, boundedCeiling("agent.create"))
		_, err := f.resolve(t, c.ID)
		assert.ErrorIs(t, err, ErrProvenanceUnrecorded)
	})
}

func TestProvenanceRoot_DevLocalDevAuthDisabled(t *testing.T) {
	for name, prov := range map[string]store.AuthorityProvenance{
		"direct":    recordedProv(store.DelegationPrincipalUser, DevUserID, store.SourceCredentialDevLocal),
		"scheduler": schedulerProv(store.DelegationPrincipalUser, DevUserID, store.InitiatorCredentialKindDevLocal),
	} {
		t.Run(name, func(t *testing.T) {
			f := newProvenanceFixture(t, "dev-off-"+name, false)
			f.devUserInProject(t, store.UserStatusActive)
			ag := f.agent(t, "dev-off-"+name, AgentRoleFull)
			f.edge(t, store.DelegationPrincipalUser, DevUserID, ag.ID, ceilPrincip, prov)
			_, err := f.resolve(t, ag.ID)
			assert.ErrorIs(t, err, ErrProvenanceSourceNotAllowed)
			assert.Equal(t, DenyCauseCeilingSourceNotAllowed, DenyCauseForProvenanceError(err))
		})
	}
}

func TestProvenanceRoot_DevLocalPrincipalMismatch(t *testing.T) {
	t.Run("delegator is another user", func(t *testing.T) {
		f := newProvenanceFixture(t, "dev-mm-deleg", true)
		ag := f.agent(t, "dev-mm-deleg", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, f.userID, ag.ID, ceilPrincip, recordedProv(store.DelegationPrincipalUser, f.userID, store.SourceCredentialDevLocal))
		_, err := f.resolve(t, ag.ID)
		assert.ErrorIs(t, err, ErrProvenanceMismatch)
	})
	t.Run("source principal is another user", func(t *testing.T) {
		f := newProvenanceFixture(t, "dev-mm-src", true)
		f.devUserInProject(t, store.UserStatusActive)
		ag := f.agent(t, "dev-mm-src", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, DevUserID, ag.ID, ceilPrincip, recordedProv(store.DelegationPrincipalUser, f.userID, store.SourceCredentialDevLocal))
		_, err := f.resolve(t, ag.ID)
		assert.ErrorIs(t, err, ErrProvenanceMismatch)
	})
	t.Run("scheduler revision with another principal", func(t *testing.T) {
		f := newProvenanceFixture(t, "dev-mm-sched", true)
		ag := f.agent(t, "dev-mm-sched", AgentRoleFull)
		p := schedulerProv(store.DelegationPrincipalUser, f.userID, store.InitiatorCredentialKindDevLocal)
		f.edge(t, store.DelegationPrincipalUser, f.userID, ag.ID, ceilPrincip, p)
		_, err := f.resolve(t, ag.ID)
		assert.ErrorIs(t, err, ErrProvenanceMismatch)
	})
}

func TestProvenanceRoot_DevLocalUserInactive(t *testing.T) {
	f := newProvenanceFixture(t, "dev-inactive", true)
	f.devUserInProject(t, "suspended")
	ag := f.agent(t, "dev-inactive", AgentRoleFull)
	f.edge(t, store.DelegationPrincipalUser, DevUserID, ag.ID, ceilPrincip, recordedProv(store.DelegationPrincipalUser, DevUserID, store.SourceCredentialDevLocal))
	_, err := f.resolve(t, ag.ID)
	assert.ErrorIs(t, err, ErrProvenanceInactive)
}

// The resolver is authority evidence: it never fills in the agent's
// ancestry.
func TestProvenanceRoot_NotAncestry(t *testing.T) {
	f := newProvenanceFixture(t, "not-ancestry", false)
	id := tid("pr-not-ancestry")
	require.NoError(t, f.store.CreateAgent(context.Background(), &store.Agent{
		ID: id, Slug: "pr-not-ancestry", Name: "pr-not-ancestry", ProjectID: f.projectID,
		CreatedBy: f.userID, AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
	}))
	ag, err := f.store.GetAgent(context.Background(), id)
	require.NoError(t, err)
	require.Empty(t, ag.Ancestry)
	f.edge(t, store.DelegationPrincipalUser, f.userID, ag.ID, ceilPrincip, recordedProv(store.DelegationPrincipalUser, f.userID, store.SourceCredentialSession))

	root, err := f.resolve(t, ag.ID)
	require.NoError(t, err)
	reread, err := f.store.GetAgent(context.Background(), ag.ID)
	require.NoError(t, err)
	assert.Empty(t, reread.Ancestry)
	require.NotNil(t, root.RootUser)
	assert.Equal(t, f.userID, root.RootUser.ID)
}

func TestProvenanceOptionsZeroValueDenies(t *testing.T) {
	assert.False(t, ResolveProvenanceOptions{}.AllowUnrecordedLegacy)
	assert.False(t, AgentAuthorityOptions{}.AllowUnrecordedLegacy)

	f := newProvenanceFixture(t, "zero-opts", false)
	ag := f.agent(t, "zero-opts", AgentRoleFull)
	f.edge(t, store.DelegationPrincipalUser, f.userID, ag.ID, store.EffectCeiling{}, store.AuthorityProvenance{})
	ctx := context.Background()
	_, err := f.a.ResolveProvenanceRoot(ctx, ag.ID, ResolveProvenanceOptions{PermissionID: "agent.create"})
	assert.ErrorIs(t, err, ErrProvenanceUnrecorded)
	_, err = f.a.AgentEffectCeiling(ctx, ag.ID, AgentAuthorityOptions{})
	assert.ErrorIs(t, err, ErrProvenanceUnrecorded)
	_, _, err = f.a.EffectiveAgentAuthority(ctx, ag, AgentAuthorityOptions{})
	assert.ErrorIs(t, err, ErrProvenanceUnrecorded)
}

// --- AgentEffectCeiling and EffectiveAgentAuthority: local development ---

func TestAgentEffectCeiling_DevLocal(t *testing.T) {
	type shape struct {
		devLocal bool
		status   string
		deleg    func(f provenanceFixture) string
	}
	cases := map[string]shape{
		"DevAuthDisabled":   {false, store.UserStatusActive, func(provenanceFixture) string { return DevUserID }},
		"PrincipalMismatch": {true, store.UserStatusActive, func(f provenanceFixture) string { return f.userID }},
		"UserInactive":      {true, "suspended", func(provenanceFixture) string { return DevUserID }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newProvenanceFixture(t, "aec-dev-"+name, tc.devLocal)
			f.devUserInProject(t, tc.status)
			ag := f.agent(t, "aec-dev-"+name, AgentRoleFull)
			deleg := tc.deleg(f)
			f.edge(t, store.DelegationPrincipalUser, deleg, ag.ID, ceilPrincip, recordedProv(store.DelegationPrincipalUser, deleg, store.SourceCredentialDevLocal))
			ctx := context.Background()

			_, err := f.a.AgentEffectCeiling(ctx, ag.ID, AgentAuthorityOptions{})
			assert.ErrorIs(t, err, ErrProvenanceSourceNotAllowed, "AgentEffectCeiling")
			assert.Equal(t, DenyCauseCeilingSourceNotAllowed, DenyCauseForProvenanceError(err))

			_, _, err = f.a.EffectiveAgentAuthority(ctx, ag, AgentAuthorityOptions{})
			assert.ErrorIs(t, err, ErrProvenanceSourceNotAllowed, "EffectiveAgentAuthority")
			assert.Equal(t, DenyCauseCeilingSourceNotAllowed, DenyCauseForProvenanceError(err))
		})
	}
	t.Run("dev user lookup error", func(t *testing.T) {
		f := newProvenanceFixture(t, "aec-dev-fault", true)
		f.devUserInProject(t, store.UserStatusActive)
		ag := f.agent(t, "aec-dev-fault", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, DevUserID, ag.ID, ceilPrincip, recordedProv(store.DelegationPrincipalUser, DevUserID, store.SourceCredentialDevLocal))
		a := f.authz(&faultStore{Store: f.store, users: true, userID: DevUserID}, true, false)
		ctx := context.Background()
		_, err := a.AgentEffectCeiling(ctx, ag.ID, AgentAuthorityOptions{})
		assert.ErrorIs(t, err, errProvenanceStoreFault)
		assert.NotErrorIs(t, err, ErrProvenanceSourceNotAllowed)
		_, _, err = a.EffectiveAgentAuthority(ctx, ag, AgentAuthorityOptions{})
		assert.ErrorIs(t, err, errProvenanceStoreFault)
	})
	t.Run("enabled and active", func(t *testing.T) {
		f := newProvenanceFixture(t, "aec-dev-ok", true)
		f.devUserInProject(t, store.UserStatusActive)
		ag := f.agent(t, "aec-dev-ok", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, DevUserID, ag.ID, ceilPrincip, recordedProv(store.DelegationPrincipalUser, DevUserID, store.SourceCredentialDevLocal))
		c, err := f.a.AgentEffectCeiling(context.Background(), ag.ID, AgentAuthorityOptions{})
		require.NoError(t, err)
		assert.Equal(t, store.EffectCeilingPrincipal, c.Kind)
	})
}

// For every registry permission, AgentEffectCeiling allows p exactly when
// every hop's own ceiling allows it, including an Unspecified-version hop.
func TestAgentEffectCeilingMatchesPerHopAllows(t *testing.T) {
	f := newProvenanceFixture(t, "per-hop", false)
	unspecified := store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionUnspecified, PermissionIDs: []string{"agent.create", "project.read", "agent.delete"}}
	hop2 := boundedCeiling("project.read", "agent.delete", "agent.lifecycle", "secret.deliver")
	p := f.userChild(t, "per-hop-p", unspecified)
	c := f.agentChild(t, "per-hop-c", p, hop2)

	got, err := f.a.AgentEffectCeiling(context.Background(), c.ID, AgentAuthorityOptions{})
	require.NoError(t, err)
	require.Equal(t, store.EffectCeilingBounded, got.Kind)
	assert.Equal(t, permissions.CeilingVersionV1, got.Version)
	f1, _ := unspecified.Frozen()
	f2, _ := hop2.Frozen()
	gf, _ := got.Frozen()
	for _, perm := range permissions.Registry {
		want := f1.Allows(perm.ID) && f2.Allows(perm.ID)
		assert.Equal(t, want, gf.Allows(perm.ID), perm.ID)
	}
}

// --- F contract ---

// mintScopes returns what mint issues for ag: the candidate scopes filtered
// by the ceiling inputs, the computation GenerateAgentTokenForAgent runs.
func mintScopes(t *testing.T, a *AuthzService, ag *store.Agent) []AgentTokenScope {
	t.Helper()
	s, err := a.ceilingFilteredAgentScopes(context.Background(), ag, a.mintCandidateScopes(ag))
	require.NoError(t, err)
	return s
}

func TestFContract_EffectiveAgentAuthority(t *testing.T) {
	ctx := context.Background()

	t.Run("session child gets the full stored scopes and principal", func(t *testing.T) {
		f := newProvenanceFixture(t, "fc-session", false)
		ag := f.userChild(t, "fc-session", ceilPrincip)
		scopes, c, err := f.a.EffectiveAgentAuthority(ctx, ag, AgentAuthorityOptions{})
		require.NoError(t, err)
		assert.Equal(t, store.EffectCeilingPrincipal, c.Kind)
		assert.Equal(t, f.a.mintCandidateScopes(ag), scopes)
		assert.Equal(t, mintScopes(t, f.a, ag), scopes)
	})

	t.Run("UAT child gets ceiled scopes and bounded", func(t *testing.T) {
		f := newProvenanceFixture(t, "fc-uat", false)
		ag := f.agent(t, "fc-uat", AgentRoleFull)
		c := boundedCeiling(append(agentScopeCoverage([]AgentTokenScope{ScopeProjectRead}), "agent.status_update")...)
		f.edge(t, store.DelegationPrincipalUser, f.userID, ag.ID, c, recordedProv(store.DelegationPrincipalUser, f.userID, store.SourceCredentialUAT))
		scopes, got, err := f.a.EffectiveAgentAuthority(ctx, ag, AgentAuthorityOptions{})
		require.NoError(t, err)
		assert.Equal(t, store.EffectCeilingBounded, got.Kind)
		assert.Equal(t, mintScopes(t, f.a, ag), scopes)
		assert.Contains(t, scopes, ScopeProjectRead)
		assert.NotContains(t, scopes, ScopeAgentCreate)
		assert.NotContains(t, scopes, ScopeProjectSecretRead)
	})

	t.Run("missing edge", func(t *testing.T) {
		f := newProvenanceFixture(t, "fc-missing", false)
		ag := f.agent(t, "fc-missing", AgentRoleFull)
		f.a.backfillDone.Store(true)
		_, _, err := f.a.EffectiveAgentAuthority(ctx, ag, AgentAuthorityOptions{})
		assert.ErrorIs(t, err, ErrProvenanceMissing)
		_, err = f.a.AgentEffectCeiling(ctx, ag.ID, AgentAuthorityOptions{})
		assert.ErrorIs(t, err, ErrProvenanceMissing)
	})

	t.Run("unrecorded edge with zero options", func(t *testing.T) {
		f := newProvenanceFixture(t, "fc-unrec", false)
		ag := f.agent(t, "fc-unrec", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, f.userID, ag.ID, store.EffectCeiling{}, store.AuthorityProvenance{})
		_, _, err := f.a.EffectiveAgentAuthority(ctx, ag, AgentAuthorityOptions{})
		assert.ErrorIs(t, err, ErrProvenanceUnrecorded)
	})

	t.Run("bounded parent above an unrecorded hop keeps the narrowing", func(t *testing.T) {
		f := newProvenanceFixture(t, "fc-mixed", false)
		p := f.userChild(t, "fc-mixed-p", boundedCeiling("project.read", "agent.create", "project.secret_read", "secret.deliver"))
		c := f.agent(t, "fc-mixed-c", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalAgent, p.ID, c.ID, store.EffectCeiling{}, store.AuthorityProvenance{})
		got, err := f.a.AgentEffectCeiling(ctx, c.ID, AgentAuthorityOptions{AllowUnrecordedLegacy: true})
		require.NoError(t, err)
		assert.Equal(t, store.EffectCeilingBounded, got.Kind)
		assert.Equal(t, []string{"agent.create", "project.read"}, got.PermissionIDs)
		scopes, ceil2, err := f.a.EffectiveAgentAuthority(ctx, c, AgentAuthorityOptions{AllowUnrecordedLegacy: true})
		require.NoError(t, err)
		assert.Equal(t, got, ceil2)
		assert.Subset(t, mintScopes(t, f.a, c), scopes)
		assert.NotContains(t, scopes, ScopeProjectSecretRead)
	})

	t.Run("principal above an unrecorded hop is unrecorded", func(t *testing.T) {
		f := newProvenanceFixture(t, "fc-pu", false)
		p := f.userChild(t, "fc-pu-p", ceilPrincip)
		c := f.agent(t, "fc-pu-c", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalAgent, p.ID, c.ID, store.EffectCeiling{}, store.AuthorityProvenance{})
		got, err := f.a.AgentEffectCeiling(ctx, c.ID, AgentAuthorityOptions{AllowUnrecordedLegacy: true})
		require.NoError(t, err)
		assert.Equal(t, store.EffectCeilingUnrecorded, got.Kind)
		for _, perm := range recordedProvenanceRequiredIDs {
			assert.False(t, EffectCeilingAllows(got, perm, false), perm)
		}
	})

	t.Run("lookup error is wrapped", func(t *testing.T) {
		f := newProvenanceFixture(t, "fc-fault", false)
		ag := f.userChild(t, "fc-fault", ceilPrincip)
		a := f.authz(&faultStore{Store: f.store, edges: true}, false, false)
		_, _, err := a.EffectiveAgentAuthority(ctx, ag, AgentAuthorityOptions{})
		assert.ErrorIs(t, err, errProvenanceStoreFault)
		assert.Equal(t, DenyCauseCeilingError, DenyCauseForProvenanceError(err))
	})

	t.Run("deliver permissions", func(t *testing.T) {
		for _, perm := range []string{"secret.deliver", "env_var.deliver", "skill_injection.deliver"} {
			assert.False(t, EffectCeilingAllows(store.EffectCeiling{}, perm, false), perm)
			assert.False(t, EffectCeilingAllows(boundedCeiling("project.read"), perm, false), perm)
			assert.True(t, EffectCeilingAllows(boundedCeiling(perm), perm, false), perm)
		}
	})
}

// Every chain shape up to depth 3: mint, the chain fold and the contract
// agree. With zero options and a fully recorded chain the contract's scopes
// equal mint's; with AllowUnrecordedLegacy they are a subset, and for every
// recordedProvenanceRequired permission the contract allows p only when the
// walk's per-hop rule allows it on every hop.
func TestMintAndContractShareChainFold(t *testing.T) {
	type hop struct {
		name string
		c    store.EffectCeiling
		p    func(kind, id string) store.AuthorityProvenance
	}
	session := func(kind, id string) store.AuthorityProvenance {
		cred := store.SourceCredentialSession
		if kind == store.DelegationPrincipalAgent {
			cred = store.SourceCredentialAgent
		}
		return recordedProv(kind, id, cred)
	}
	none := func(string, string) store.AuthorityProvenance { return store.AuthorityProvenance{} }
	v2 := func(kind, id string) store.AuthorityProvenance {
		p := session(kind, id)
		p.ProvenanceVersion = 2
		return p
	}
	kinds := []hop{
		{"principal", ceilPrincip, session},
		{"bounded", boundedCeiling("project.read", "agent.create", "agent.status_update", "project.secret_read"), session},
		{"unrecorded", store.EffectCeiling{}, none},
		{"boundedv2", boundedCeiling("project.read", "agent.lifecycle", "project.secret_read"), v2},
	}
	var shapes [][]hop
	for _, a := range kinds {
		shapes = append(shapes, []hop{a})
		for _, b := range kinds {
			shapes = append(shapes, []hop{a, b})
			for _, c := range kinds {
				shapes = append(shapes, []hop{a, b, c})
			}
		}
	}
	ctx := context.Background()
	for i, shape := range shapes {
		var names []string
		for _, h := range shape {
			names = append(names, h.name)
		}
		t.Run(strings.Join(names, "/"), func(t *testing.T) {
			f := newProvenanceFixture(t, fmt.Sprintf("fold-%d", i), false)
			var prev *store.Agent
			var edges []store.EffectCeiling
			var versions []int
			for j, h := range shape {
				ag := f.agent(t, fmt.Sprintf("fold-%d-%d", i, j), AgentRoleFull)
				if prev == nil {
					p := h.p(store.DelegationPrincipalUser, f.userID)
					f.edge(t, store.DelegationPrincipalUser, f.userID, ag.ID, h.c, p)
					versions = append(versions, p.ProvenanceVersion)
				} else {
					p := h.p(store.DelegationPrincipalAgent, prev.ID)
					f.edge(t, store.DelegationPrincipalAgent, prev.ID, ag.ID, h.c, p)
					versions = append(versions, p.ProvenanceVersion)
				}
				edges = append(edges, h.c)
				prev = ag
			}
			leaf := prev
			chain, err := f.a.chainEffectCeiling(ctx, leaf)
			require.NoError(t, err)
			minted := mintScopes(t, f.a, leaf)

			zeroScopes, zeroCeil, zeroErr := f.a.EffectiveAgentAuthority(ctx, leaf, AgentAuthorityOptions{})
			aec, aecErr := f.a.AgentEffectCeiling(ctx, leaf.ID, AgentAuthorityOptions{})
			if chain.UnrecordedHops == 0 {
				require.NoError(t, zeroErr)
				require.NoError(t, aecErr)
				assert.Equal(t, minted, zeroScopes, "zero options: contract scopes equal mint")
				assert.Equal(t, chain.Ceiling, zeroCeil)
				assert.Equal(t, chain.Ceiling, aec)
			} else {
				assert.ErrorIs(t, zeroErr, ErrProvenanceUnrecorded)
				assert.ErrorIs(t, aecErr, ErrProvenanceUnrecorded)
			}

			legacyOpts := AgentAuthorityOptions{AllowUnrecordedLegacy: true}
			scopes, ceil, err := f.a.EffectiveAgentAuthority(ctx, leaf, legacyOpts)
			require.NoError(t, err)
			aecLegacy, err := f.a.AgentEffectCeiling(ctx, leaf.ID, legacyOpts)
			require.NoError(t, err)
			assert.Equal(t, aecLegacy, ceil)
			assert.Subset(t, minted, scopes, "contract never wider than mint")

			for _, perm := range recordedProvenanceRequiredIDs {
				walk := true
				for j, c := range edges {
					e := &store.DelegationEdge{EffectCeiling: c, AuthorityProvenance: store.AuthorityProvenance{ProvenanceVersion: versions[j]}}
					if cause, _ := hopEffectCeilingDeny(e, perm, Resource{Type: "secret"}, leaf.ID, false); cause != "" {
						walk = false
					}
				}
				if EffectCeilingAllows(ceil, perm, false) {
					assert.True(t, walk, "contract allows %s where the walk denies", perm)
				}
				if chain.UnrecordedHops > 0 {
					assert.False(t, EffectCeilingAllows(ceil, perm, false), "unrecorded chain allows %s", perm)
				}
			}
		})
	}
}

// --- frozen characterization (§ consumer policy table) ---

func TestUnrecordedConsumerPolicyTable(t *testing.T) {
	ctx := context.Background()
	type row struct {
		consumer   string
		unrecorded error // nil: allowed
		missing    error
		run        func(f provenanceFixture, ag *store.Agent) error
	}
	rows := []row{
		{"resolveProvenanceChain(allow)", nil, ErrProvenanceMissing, func(f provenanceFixture, ag *store.Agent) error {
			_, err := f.a.resolveProvenanceChain(ctx, ag, true)
			return err
		}},
		{"ResolveProvenanceRoot zero options", ErrProvenanceUnrecorded, ErrProvenanceMissing, func(f provenanceFixture, ag *store.Agent) error {
			_, err := f.a.ResolveProvenanceRoot(ctx, ag.ID, ResolveProvenanceOptions{PermissionID: "agent.create"})
			return err
		}},
		{"AgentEffectCeiling zero options", ErrProvenanceUnrecorded, ErrProvenanceMissing, func(f provenanceFixture, ag *store.Agent) error {
			_, err := f.a.AgentEffectCeiling(ctx, ag.ID, AgentAuthorityOptions{})
			return err
		}},
		{"EffectiveAgentAuthority zero options", ErrProvenanceUnrecorded, ErrProvenanceMissing, func(f provenanceFixture, ag *store.Agent) error {
			_, _, err := f.a.EffectiveAgentAuthority(ctx, ag, AgentAuthorityOptions{})
			return err
		}},
		{"ResolveProvenanceRoot allow", nil, ErrProvenanceMissing, func(f provenanceFixture, ag *store.Agent) error {
			_, err := f.a.ResolveProvenanceRoot(ctx, ag.ID, ResolveProvenanceOptions{PermissionID: "agent.create", AllowUnrecordedLegacy: true})
			return err
		}},
		{"AgentEffectCeiling allow", nil, ErrProvenanceMissing, func(f provenanceFixture, ag *store.Agent) error {
			c, err := f.a.AgentEffectCeiling(ctx, ag.ID, AgentAuthorityOptions{AllowUnrecordedLegacy: true})
			if err == nil {
				for _, p := range recordedProvenanceRequiredIDs {
					if EffectCeilingAllows(c, p, false) {
						return fmt.Errorf("allows %s", p)
					}
				}
			}
			return err
		}},
	}
	for i, r := range rows {
		t.Run(r.consumer, func(t *testing.T) {
			f := newProvenanceFixture(t, fmt.Sprintf("policy-%d", i), false)
			f.a.backfillDone.Store(true)
			unrec := f.agent(t, fmt.Sprintf("policy-%d-u", i), AgentRoleFull)
			f.edge(t, store.DelegationPrincipalUser, f.userID, unrec.ID, store.EffectCeiling{}, store.AuthorityProvenance{})
			missing := f.agent(t, fmt.Sprintf("policy-%d-m", i), AgentRoleFull)

			err := r.run(f, unrec)
			if r.unrecorded == nil {
				assert.NoError(t, err, "unrecorded column")
			} else {
				assert.ErrorIs(t, err, r.unrecorded, "unrecorded column")
			}
			assert.ErrorIs(t, r.run(f, missing), r.missing, "missing column")
		})
	}

	t.Run("dev arm", func(t *testing.T) {
		for _, enabled := range []bool{true, false} {
			f := newProvenanceFixture(t, fmt.Sprintf("policy-dev-%v", enabled), enabled)
			f.devUserInProject(t, store.UserStatusActive)
			ag := f.agent(t, fmt.Sprintf("policy-dev-%v", enabled), AgentRoleFull)
			f.edge(t, store.DelegationPrincipalUser, DevUserID, ag.ID, ceilPrincip, recordedProv(store.DelegationPrincipalUser, DevUserID, store.SourceCredentialDevLocal))

			// The structural chain resolves to the seeded dev user either way.
			root, err := f.a.resolveProvenanceChain(ctx, ag, true)
			require.NoError(t, err)
			assert.Equal(t, DevUserID, root.RootUser.ID)

			_, rerr := f.a.ResolveProvenanceRoot(ctx, ag.ID, ResolveProvenanceOptions{PermissionID: "agent.create"})
			_, aerr := f.a.AgentEffectCeiling(ctx, ag.ID, AgentAuthorityOptions{})
			if enabled {
				assert.NoError(t, rerr)
				assert.NoError(t, aerr)
			} else {
				assert.ErrorIs(t, rerr, ErrProvenanceSourceNotAllowed)
				assert.ErrorIs(t, aerr, ErrProvenanceSourceNotAllowed)
			}
		}
	})
}

// --- pins over production sources ---

// parseHubProduction parses the non-test Go files of this package.
func parseHubProduction(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	require.NoError(t, err)
	var files []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		src, err := os.ReadFile(p)
		require.NoError(t, err)
		f, err := parser.ParseFile(fset, p, src, 0)
		require.NoError(t, err)
		files = append(files, f)
	}
	return fset, files
}

// declReceiverName returns "Recv.Name" or "Name" for decl.
func declReceiverName(decl *ast.FuncDecl) string {
	if decl.Recv == nil || len(decl.Recv.List) == 0 {
		return decl.Name.Name
	}
	typ := decl.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + decl.Name.Name
	}
	return decl.Name.Name
}

// callsIn returns, per enclosing function, the calls to a function or
// method named name.
func callsIn(files []*ast.File, name string) map[string][]*ast.CallExpr {
	out := map[string][]*ast.CallExpr{}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					if fun.Name == name {
						out[declReceiverName(fn)] = append(out[declReceiverName(fn)], call)
					}
				case *ast.SelectorExpr:
					if fun.Sel.Name == name {
						out[declReceiverName(fn)] = append(out[declReceiverName(fn)], call)
					}
				}
				return true
			})
		}
	}
	return out
}

// The only production caller that passes allowUnrecordedLegacy=true to
// resolveProvenanceChain is provenanceRootSourceResolver, and no production
// code sets AllowUnrecordedLegacy.
func TestAllowUnrecordedLegacyCallersPinned(t *testing.T) {
	fset, files := parseHubProduction(t)
	allowedTrue := map[string]bool{"provenanceRootSourceResolver.ResolveExecutionSource": true}
	for fn, calls := range callsIn(files, "resolveProvenanceChain") {
		for _, call := range calls {
			require.Len(t, call.Args, 3, fn)
			id, ok := call.Args[2].(*ast.Ident)
			if ok && id.Name == "false" {
				continue
			}
			if ok && id.Name == "true" && allowedTrue[fn] {
				continue
			}
			// ResolveProvenanceRoot forwards the caller's option.
			if fn == "AuthzService.ResolveProvenanceRoot" {
				continue
			}
			t.Errorf("%s calls resolveProvenanceChain with a non-literal-false legacy flag", fn)
		}
	}
	for _, f := range files {
		for _, pos := range allowUnrecordedLegacySetters(f) {
			t.Errorf("production code sets AllowUnrecordedLegacy at %s", fset.Position(pos))
		}
	}
}

// allowUnrecordedLegacySetters returns the positions in f that set an
// AllowUnrecordedLegacy field: a composite-literal key, or the left side of
// an assignment (x.AllowUnrecordedLegacy = …).
func allowUnrecordedLegacySetters(f *ast.File) []token.Pos {
	var out []token.Pos
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.KeyValueExpr:
			if key, ok := n.Key.(*ast.Ident); ok && key.Name == "AllowUnrecordedLegacy" {
				out = append(out, n.Pos())
			}
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "AllowUnrecordedLegacy" {
					out = append(out, lhs.Pos())
				}
			}
		}
		return true
	})
	return out
}

// The setter matcher finds both the composite-literal and the assignment
// form, and ignores reads.
func TestAllowUnrecordedLegacySetterMatcher(t *testing.T) {
	for src, want := range map[string]int{
		"o := ResolveProvenanceOptions{AllowUnrecordedLegacy: true}; _ = o": 1,
		"var o ResolveProvenanceOptions; o.AllowUnrecordedLegacy = true":    1,
		"p.opts.AllowUnrecordedLegacy, x = true, 1":                         1,
		"var o ResolveProvenanceOptions; _ = o.AllowUnrecordedLegacy":       0,
		"var o ResolveProvenanceOptions; o.PermissionID = \"x\"":            0,
	} {
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package x\nfunc f() {\n"+src+"\n}\n", 0)
		require.NoError(t, err, src)
		assert.Len(t, allowUnrecordedLegacySetters(f), want, src)
	}
}

// filterScopes is the only production function that removes scopes from a
// candidate list by a ceiling. It is called only from
// ceilingFilteredAgentScopes and EffectiveAgentAuthority, and both obtain
// their inputs from loadScopeCeilings.
func TestScopeFilterSingleSource(t *testing.T) {
	_, files := parseHubProduction(t)
	callers := callsIn(files, "filterScopes")
	want := map[string]bool{
		"AuthzService.ceilingFilteredAgentScopes": true,
		"AuthzService.EffectiveAgentAuthority":    true,
	}
	for fn := range callers {
		assert.True(t, want[fn], "unexpected filterScopes caller %s", fn)
	}
	loaders := callsIn(files, "loadScopeCeilings")
	for fn := range want {
		assert.NotEmpty(t, callers[fn], "%s must call filterScopes", fn)
		assert.NotEmpty(t, loaders[fn], "%s must obtain its inputs from loadScopeCeilings", fn)
	}
	// ceilingAllowsScope, the per-scope rule, is applied only inside
	// filterScopes and the role-fit check that selects a child role.
	for fn := range callsIn(files, "ceilingAllowsScope") {
		assert.Contains(t, []string{"filterScopes", "roleFitsCeiling", "childRoleWithinCeiling"}, fn, "unexpected ceilingAllowsScope caller %s", fn)
	}
}

// --- purge ---

// A scheduled child's provenance is the edge's frozen copy of the revision:
// purging the fired event does not change resolution, execution admission
// or the contract.
func TestProvenanceRootSurvivesEventPurge(t *testing.T) {
	f := newProvenanceFixture(t, "purge", false)
	ctx := context.Background()
	evtID := tid("pr-purge-evt")
	old := time.Now().Add(-30 * 24 * time.Hour)
	require.NoError(t, f.store.CreateScheduledEvent(ctx, &store.ScheduledEvent{
		ID: evtID, ProjectID: f.projectID, EventType: "dispatch_agent", FireAt: old,
		Payload: "{}", Status: "fired", CreatedAt: old, CreatedBy: f.userID, FiredAt: &old,
	}))
	ag := f.agent(t, "purge", AgentRoleFull)
	prov := schedulerProv(store.DelegationPrincipalUser, f.userID, store.InitiatorCredentialKindSession)
	prov.SourceEventID = evtID
	f.edge(t, store.DelegationPrincipalUser, f.userID, ag.ID, ceilPrincip, prov)

	n, err := f.store.PurgeOldScheduledEvents(ctx, time.Now())
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, 1)
	_, err = f.store.GetScheduledEvent(ctx, evtID)
	require.ErrorIs(t, err, store.ErrNotFound)

	root, err := f.resolve(t, ag.ID)
	require.NoError(t, err)
	assert.Equal(t, evtID, root.Revision.EventID)

	_, _, err = f.a.EffectiveAgentAuthority(ctx, ag, AgentAuthorityOptions{})
	require.NoError(t, err)

	ok, reason := f.a.executionProjectAdmission(ctx, PrincipalContext{
		Kind:     PrincipalKindAgent,
		ID:       ag.ID,
		Identity: dcAgentIdentity(ag.ID, f.projectID, AgentRoleFull),
	}, "skill.read")
	assert.True(t, ok, reason)
}
