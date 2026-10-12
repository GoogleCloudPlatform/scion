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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// mdCountingStore counts the store reads the agent GET's messageability
// detail makes outside the authorization service's own counted store:
// sender rows, holds, delegation edges and root users. Only calls made
// under a perf trace (the measured request) are counted, so background
// work never adds to a count.
type mdCountingStore struct {
	store.Store
	mu     sync.Mutex
	counts map[string]int64
	// failGetAgent, when set, makes GetAgent for that ID fail with a
	// lookup error (not ErrNotFound).
	failGetAgent string
}

func (c *mdCountingStore) setFailGetAgent(id string) {
	c.mu.Lock()
	c.failGetAgent = id
	c.mu.Unlock()
}

// DB exposes the wrapped store's pool, which the server's startup
// migrations and the perf trace need.
func (c *mdCountingStore) DB() *sql.DB {
	if d, ok := c.Store.(interface{ DB() *sql.DB }); ok {
		return d.DB()
	}
	return nil
}

func (c *mdCountingStore) note(ctx context.Context, op string) {
	if perfTraceFrom(ctx) == nil {
		return
	}
	c.mu.Lock()
	c.counts[op]++
	c.mu.Unlock()
}

func (c *mdCountingStore) take() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.counts
	c.counts = map[string]int64{}
	return out
}

func (c *mdCountingStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	c.note(ctx, "GetAgent")
	c.mu.Lock()
	fail := c.failGetAgent != "" && c.failGetAgent == id
	c.mu.Unlock()
	if fail {
		return nil, errors.New("injected agent lookup failure")
	}
	return c.Store.GetAgent(ctx, id)
}

func (c *mdCountingStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	c.note(ctx, "GetUser")
	return c.Store.GetUser(ctx, id)
}

func (c *mdCountingStore) HasActiveAgentHold(ctx context.Context, agentID string) (bool, error) {
	c.note(ctx, "HasActiveAgentHold")
	return c.Store.HasActiveAgentHold(ctx, agentID)
}

func (c *mdCountingStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	c.note(ctx, "GetDelegationEdgesForDelegate")
	return c.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
}

// mdFixture is a hub with PerfTrace on, n agents of alice's in her
// project, and one agent of carol's, a plain project member.
type mdFixture struct {
	srv        *Server
	raw        store.Store
	counter    *mdCountingStore
	alice      *store.User
	carol      *store.User
	admin      *store.User
	project    *store.Project
	aliceAgent *store.Agent
	carolAgent *store.Agent
	agentToken string
}

func newMDFixture(t *testing.T, n int) *mdFixture {
	t.Helper()
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("sqlite driver not registered")
		}
		t.Fatalf("test store: %v", err)
	}
	ctx := context.Background()
	_ = s.DeleteHubSetting(ctx, "migration_delegation_edge_backfill_v1")

	f := &mdFixture{raw: s, counter: &mdCountingStore{Store: s, counts: map[string]int64{}}}
	f.srv = newPerfServer(t, f.counter, true)
	// Tests drive membership-loss processing themselves.
	f.srv.membershipService.onMembershipLoss = nil
	f.alice, _, f.project = setupDemoPolicyOn(t, f.srv, s)

	mkUser := func(name, role string) *store.User {
		u := &store.User{
			ID: tid("user-md-" + name), Email: name + "@test.com", DisplayName: name,
			Role: role, Status: store.UserStatusActive, Created: time.Now(),
		}
		require.NoError(t, s.CreateUser(ctx, u))
		ensureHubMembership(ctx, s, u.ID)
		return u
	}
	f.carol = mkUser("carol", store.UserRoleMember)
	f.admin = mkUser("admin", store.UserRoleAdmin)
	msgAuthzAddProjectMember(t, s, f.carol.ID, f.project.ID, f.project.Slug, store.GroupMemberRoleMember)

	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mk := func(name string, owner *store.User, mode string, i int) *store.Agent {
		a := &store.Agent{
			ID: tid(name), Slug: name, Name: name, ProjectID: f.project.ID,
			Phase: "running", CreatedBy: owner.ID, OwnerID: owner.ID, MessageMode: mode,
			Created: base.Add(time.Duration(i) * time.Minute), Updated: base,
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}
	// Mostly project mode (reachable), with lineage and none agents mixed
	// in so some decisions deny on mode before any standing check.
	modes := []string{store.MessageModeProject, store.MessageModeProject, store.MessageModeProject,
		store.MessageModeLineage, store.MessageModeNone}
	for i := 0; i < n; i++ {
		a := mk(fmt.Sprintf("md-alice-%d", i), f.alice, modes[i%len(modes)], i)
		if i == 0 {
			f.aliceAgent = a
		}
	}
	f.carolAgent = mk("md-carol-0", f.carol, store.MessageModeProject, n)

	f.agentToken, err = f.srv.GetAgentTokenService().GenerateAgentToken(
		f.aliceAgent.ID, f.project.ID, []AgentTokenScope{ScopeProjectRead}, nil)
	require.NoError(t, err)
	return f
}

type mdPrincipal string

const (
	mdMember mdPrincipal = "member"
	mdAdmin  mdPrincipal = "admin"
	mdAgent  mdPrincipal = "agent"
)

var mdPrincipals = []mdPrincipal{mdMember, mdAdmin, mdAgent}

// get serves GET /api/v1/agents/{id} as who under a perf trace and returns
// the decoded _messageability, the trace and the counted reads.
func (f *mdFixture) get(t *testing.T, who mdPrincipal, agentID string) (AgentMessageabilityDetail, PerfTraceSnapshot, map[string]int64) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agentID, nil)
	switch who {
	case mdAgent:
		req.Header.Set("X-Scion-Agent-Token", f.agentToken)
	case mdAdmin:
		req.Header.Set("Authorization", "Bearer "+testDevToken)
	default:
		u := f.alice
		token, _, _, err := f.srv.userTokenService.GenerateTokenPair(u.ID, u.Email, u.DisplayName, u.Role, ClientTypeWeb)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
	}
	f.counter.take()
	rec, snap := perfTracedRequest(t, f.srv, req)
	counts := f.counter.take()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Messageability *AgentMessageabilityDetail `json:"_messageability"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotNil(t, body.Messageability, "the agent GET carries _messageability")
	return *body.Messageability, snap, counts
}

// identity returns a viewer identity for who: alice, a hub admin user, or
// alice's first agent. Over HTTP the admin principal is the dev token.
func (f *mdFixture) identity(who mdPrincipal) Identity {
	switch who {
	case mdAdmin:
		return NewAuthenticatedUser(f.admin.ID, f.admin.Email, f.admin.DisplayName, f.admin.Role, string(ClientTypeWeb))
	case mdAgent:
		return agentIdentityFromAgent(f.aliceAgent)
	default:
		return NewAuthenticatedUser(f.alice.ID, f.alice.Email, f.alice.DisplayName, f.alice.Role, string(ClientTypeWeb))
	}
}

// referenceDetail is the messageability detail as computed before the
// sender's row and standing were read once per request: every decision
// goes through authorizeAgentMessage on a context with no standing memo,
// so each one reads the sender row and evaluates standing afresh.
func (f *mdFixture) referenceDetail(t *testing.T, viewer Identity, target *store.Agent) AgentMessageabilityDetail {
	t.Helper()
	ctx := context.Background()
	require.Nil(t, standingMemoFrom(ctx))
	res, err := f.raw.ListAgents(ctx, store.AgentFilter{ProjectID: target.ProjectID}, store.ListOptions{})
	require.NoError(t, err)
	sender := agentIdentityFromAgent(target)
	reachable := 0
	for i := range res.Items {
		other := &res.Items[i]
		if other.ID == target.ID {
			continue
		}
		if allowed, _, _ := f.srv.authorizeAgentMessage(ctx, sender, other, false); allowed {
			reachable++
		}
	}
	return AgentMessageabilityDetail{
		AgentMessageability: *f.srv.ComputeMessageability(ctx, viewer, target),
		ReachableAgentCount: reachable,
		ReachableUserCount:  countReachableUsers(target),
	}
}

// suspendCarol removes carol from the project and processes the
// membership loss, which holds her agents.
func (f *mdFixture) suspendCarol(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	rbs, err := f.raw.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.carol.ID)
	require.NoError(t, err)
	for _, rb := range rbs {
		if rb.ScopeType == store.RoleScopeProject && rb.ScopeID == f.project.ID {
			require.NoError(t, f.raw.DeleteRoleBinding(ctx, rb.ID))
		}
	}
	group, err := f.raw.GetGroupBySlug(ctx, projectMembersGroupSlug(f.project.Slug))
	require.NoError(t, err)
	require.NoError(t, f.raw.RemoveGroupMember(ctx, group.ID, store.GroupMemberTypeUser, f.carol.ID))
	require.NoError(t, enqueueMembershipLossTx(ctx, f.raw, f.carol.ID, f.project.ID, store.MembershipLossTriggerMemberRemove, AuditActor{}))
	f.srv.drainMembershipLossChecks(ctx)
	err = f.srv.agentStanding(ctx, f.carolAgent.ID)
	require.ErrorIs(t, err, errAgentNotInStanding, "carol's agent is out of standing after her removal")
}

// The agent GET's store reads and decisions do not grow with the number
// of agents in the project, for every principal (ptone/scion#3968).
func TestAgentGet_MessageabilityReadsFlatAcrossProjectSize(t *testing.T) {
	type sample struct {
		authzStoreCalls, decisions int64
		reads                      map[string]int64
		messageabilityPhases       int64
		reachable                  int
	}
	sizes := []int{25, 100, 500}
	got := map[mdPrincipal][]sample{}
	for _, n := range sizes {
		f := newMDFixture(t, n)
		for _, who := range mdPrincipals {
			// Warm once, then measure: the first request may populate
			// process-wide caches.
			f.get(t, who, f.aliceAgent.ID)
			md, snap, reads := f.get(t, who, f.aliceAgent.ID)
			got[who] = append(got[who], sample{
				authzStoreCalls:      snap.AuthzStoreCalls,
				decisions:            snap.AuditRecords,
				reads:                reads,
				messageabilityPhases: snap.Phases["messageability"].Count,
				reachable:            md.ReachableAgentCount,
			})
		}
	}
	for _, who := range mdPrincipals {
		t.Run(string(who), func(t *testing.T) {
			s := got[who]
			for i, n := range sizes {
				t.Logf("n=%d authz store calls=%d decisions=%d reads=%v reachable=%d",
					n, s[i].authzStoreCalls, s[i].decisions, s[i].reads, s[i].reachable)
				assert.Equal(t, int64(1), s[i].messageabilityPhases, "n=%d: one messageability phase covers the detail", n)
				// Alice's agent can reach every other project-mode agent.
				assert.Positive(t, s[i].reachable, "n=%d", n)
				// The counters are wired in: equal counts below cannot
				// come from every count being zero.
				assert.Positive(t, s[i].reads["GetAgent"], "n=%d: GetAgent reads are counted", n)
				assert.Positive(t, s[i].authzStoreCalls, "n=%d: authz store calls are counted", n)
			}
			for i := 1; i < len(sizes); i++ {
				assert.Equal(t, s[0].authzStoreCalls, s[i].authzStoreCalls, "authz store calls at n=%d vs n=%d", sizes[i], sizes[0])
				assert.Equal(t, s[0].decisions, s[i].decisions, "decisions at n=%d vs n=%d", sizes[i], sizes[0])
				assert.Equal(t, s[0].reads, s[i].reads, "store reads at n=%d vs n=%d", sizes[i], sizes[0])
			}
		})
	}
}

// The detail on the agent GET equals the reference computation for member,
// admin and agent principals, for a target in good standing and for a
// removed member's suspended agent.
func TestAgentGet_MessageabilityMatchesReference(t *testing.T) {
	const n = 12
	f := newMDFixture(t, n)
	check := func(t *testing.T, target *store.Agent) {
		for _, who := range mdPrincipals {
			t.Run(string(who), func(t *testing.T) {
				want := f.referenceDetail(t, f.identity(who), target)
				// Over HTTP: the reachable counts are viewer-independent.
				// The agent principal's token reads only its own agent.
				if who != mdAgent || target.ID == f.aliceAgent.ID {
					gotHTTP, _, _ := f.get(t, who, target.ID)
					assert.Equal(t, want.ReachableAgentCount, gotHTTP.ReachableAgentCount)
					assert.Equal(t, want.ReachableUserCount, gotHTTP.ReachableUserCount)
				}
				// Directly, with the same viewer identity: every field.
				gotDirect := f.srv.ComputeMessageabilityDetail(context.Background(), f.identity(who), target, f.listProject(t))
				assert.Equal(t, want, *gotDirect)
			})
		}
	}

	t.Run("in standing", func(t *testing.T) {
		// Every other agent is a candidate; project-mode ones are reached.
		want := f.referenceDetail(t, f.identity(mdMember), f.carolAgent)
		require.Positive(t, want.ReachableAgentCount)
		check(t, f.carolAgent)
		check(t, f.aliceAgent)
	})

	t.Run("removed member's suspended agent", func(t *testing.T) {
		f.suspendCarol(t)
		want := f.referenceDetail(t, f.identity(mdMember), f.carolAgent)
		require.Zero(t, want.ReachableAgentCount, "a suspended sender reaches no agent")
		check(t, f.carolAgent)
		// Alice's agents keep their standing; carol's held agent is a
		// target, which standing does not gate.
		check(t, f.aliceAgent)
	})
}

func (f *mdFixture) listProject(t *testing.T) []store.Agent {
	t.Helper()
	res, err := f.raw.ListAgents(context.Background(), store.AgentFilter{ProjectID: f.project.ID}, store.ListOptions{})
	require.NoError(t, err)
	return res.Items
}

// The standing memo the detail installs never carries a result from one
// request to the next: carol's agent reaches agents, then after her
// removal the next request sees the suspension. Member and admin requests
// go over HTTP; the agent principal's token cannot read carol's agent, so
// its request contexts (which carry their own per-request memo from
// auth) are reproduced directly.
func TestAgentGet_MessageabilityMemoDoesNotCrossRequests(t *testing.T) {
	const n = 6
	f := newMDFixture(t, n)
	httpPrincipals := []mdPrincipal{mdMember, mdAdmin}
	agents := f.listProject(t)
	before := map[mdPrincipal]int{}
	for _, who := range httpPrincipals {
		md, _, _ := f.get(t, who, f.carolAgent.ID)
		require.Positive(t, md.ReachableAgentCount, "%s before removal", who)
		before[who] = md.ReachableAgentCount
	}
	req1 := withStandingMemo(context.Background())
	agentBefore := f.srv.ComputeMessageabilityDetail(req1, f.identity(mdAgent), f.carolAgent, agents)
	require.Positive(t, agentBefore.ReachableAgentCount, "agent request before removal")

	f.suspendCarol(t)
	for _, who := range httpPrincipals {
		md, _, _ := f.get(t, who, f.carolAgent.ID)
		assert.Zero(t, md.ReachableAgentCount, "%s after removal (was %d)", who, before[who])
	}
	req2 := withStandingMemo(context.Background())
	agentAfter := f.srv.ComputeMessageabilityDetail(req2, f.identity(mdAgent), f.carolAgent, agents)
	assert.Zero(t, agentAfter.ReachableAgentCount, "agent request after removal (was %d)", agentBefore.ReachableAgentCount)

	// The memo is scoped to the call: a caller's context never gains one.
	ctx := context.Background()
	f.srv.ComputeMessageabilityDetail(ctx, f.identity(mdMember), f.aliceAgent, agents)
	assert.Nil(t, standingMemoFrom(ctx))
}

// A sender with no stored row reaches no agent: the decision uses the
// caller's row, and the sender's standing check, which reads by ID, refuses
// it (agent_missing) on every target the modes allow.
func TestCountReachableAgents_SenderWithoutStoredRowReachesNone(t *testing.T) {
	f := newMDFixture(t, 4)
	agents := f.listProject(t)
	ghost := *f.aliceAgent
	ghost.ID = tid("md-ghost")
	assert.Zero(t, f.srv.countReachableAgents(context.Background(), &ghost, agents))
	assert.Zero(t, f.referenceDetail(t, f.identity(mdMember), &ghost).ReachableAgentCount)
	assert.Zero(t, f.srv.countReachableAgents(context.Background(), nil, agents))
}

// A store error during the sender's standing check denies every target,
// and the fault is not memoised: the same request context recomputes it.
func TestCountReachableAgents_StandingLookupFaultDeniesAll(t *testing.T) {
	f := newMDFixture(t, 6)
	agents := f.listProject(t)
	ctx := withStandingMemo(context.Background())
	want := f.srv.countReachableAgents(context.Background(), f.carolAgent, agents)
	require.Positive(t, want, "carol's agent reaches agents while in standing")

	f.counter.setFailGetAgent(f.carolAgent.ID)
	assert.Zero(t, f.srv.countReachableAgents(ctx, f.carolAgent, agents), "a standing lookup fault denies every target")
	assert.Zero(t, f.referenceDetail(t, f.identity(mdMember), f.carolAgent).ReachableAgentCount,
		"the per-target reference denies too")

	f.counter.setFailGetAgent("")
	assert.Equal(t, want, f.srv.countReachableAgents(ctx, f.carolAgent, agents), "the fault was not memoised")
}
