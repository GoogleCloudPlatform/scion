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
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#4568: an agent created by another agent has a
// bounded delegation edge whose frozen ceiling never holds agent.read
// (agent.read has no agent scope). The delegation ceiling then denied the
// launcher status read (ptone/scion#3409) for every such launcher, so a
// nested launcher could not follow its own child. An agent may now read
// the status of any descendant, in its project, through the single-agent
// GET routes only.

// descendantChain is a real delegation chain built through the create API:
// the creator's session creates L (principal edge); L creates M and S
// (bounded edges, delegator L); M creates C (bounded, delegator M); C
// creates G (bounded, delegator C). X is an unrelated agent of the
// creator.
type descendantChain struct {
	*chainFixture
	l, m, c, g, s, x *store.Agent
	mEdge, cEdge     *store.DelegationEdge
}

func newDescendantChain(t *testing.T, name string) *descendantChain {
	t.Helper()
	f := newChainFixture(t, name)
	d := &descendantChain{chainFixture: f}
	d.l, _ = f.sessionParent(t, name+"-l")
	d.m, d.mEdge = f.childOf(t, d.l, name+"-m")
	d.c, d.cEdge = f.childOf(t, d.m, name+"-c")
	d.g, _ = f.childOf(t, d.c, name+"-g")
	d.s, _ = f.childOf(t, d.l, name+"-s")
	d.x, _ = f.sessionParent(t, name+"-x")
	return d
}

// getAs performs GET path with the production token of agent.
func (d *descendantChain) getAs(t *testing.T, agent *store.Agent, path string) (int, string) {
	t.Helper()
	rec := doRequestWithAgentToken(t, d.srv, http.MethodGet, path, nil, d.agentToken(t, agent.ID))
	return rec.Code, rec.Body.String()
}

// requireBoundedWithoutAgentRead checks that edge is bounded and that its
// frozen ceiling does not hold agent.read: the case the exception is for.
func requireBoundedWithoutAgentRead(t *testing.T, edge *store.DelegationEdge) {
	t.Helper()
	require.NotNil(t, edge)
	require.Equal(t, store.EffectCeilingBounded, edge.Kind)
	require.False(t, slices.Contains(edge.PermissionIDs, permissionAgentRead), "frozen ceiling: %v", edge.PermissionIDs)
}

// TestDescendantRead_BoundedLauncherReadsChild checks that an agent whose
// own edge is bounded reads its direct child on both single-agent GET
// routes. Before ptone/scion#4568 the delegation ceiling answered 404.
func TestDescendantRead_BoundedLauncherReadsChild(t *testing.T) {
	d := newDescendantChain(t, "desc-child")
	requireBoundedWithoutAgentRead(t, d.mEdge)

	for name, path := range launcherReadPaths(d.proj.ID, d.c.ID) {
		code, body := d.getAs(t, d.m, path)
		require.Equal(t, http.StatusOK, code, "%s: %s", name, body)
		assert.Contains(t, body, d.c.ID, name)
	}
}

// TestDescendantRead_GrandchildAllowed checks that a descendant below the
// direct child is readable too.
func TestDescendantRead_GrandchildAllowed(t *testing.T) {
	d := newDescendantChain(t, "desc-grand")
	require.Equal(t, []string{d.creator.ID, d.l.ID, d.m.ID, d.c.ID}, d.g.Ancestry, "G's stored ancestry")

	for name, path := range launcherReadPaths(d.proj.ID, d.g.ID) {
		code, body := d.getAs(t, d.m, path)
		require.Equal(t, http.StatusOK, code, "%s: %s", name, body)
		assert.Contains(t, body, d.g.ID, name)
	}
}

// TestDescendantRead_TwoBoundedHops checks the exception at every bounded
// hop: C's own edge (delegator M) and M's edge (delegator L) are both
// bounded, as in a coordinator -> lead -> dev chain.
func TestDescendantRead_TwoBoundedHops(t *testing.T) {
	d := newDescendantChain(t, "desc-hops")
	requireBoundedWithoutAgentRead(t, d.cEdge)
	requireBoundedWithoutAgentRead(t, d.mEdge)
	require.Equal(t, d.m.ID, d.cEdge.DelegatorID)

	for name, path := range launcherReadPaths(d.proj.ID, d.g.ID) {
		code, body := d.getAs(t, d.c, path)
		require.Equal(t, http.StatusOK, code, "%s: %s", name, body)
	}
}

// TestDescendantRead_NonDescendantsMatchMissing checks that every agent
// that is not a descendant of the caller in its project gets exactly the
// answer a missing agent gets: a sibling, an unrelated agent, the caller's
// own parent, and an agent in another project whose stored ancestry names
// the caller.
func TestDescendantRead_NonDescendantsMatchMissing(t *testing.T) {
	d := newDescendantChain(t, "desc-deny")
	forged := launcherReadAgent(t, d.bypassAgentsFixture, "desc-deny-forged", d.other.ID,
		[]string{d.creator.ID, d.l.ID, d.m.ID})

	targets := map[string]*store.Agent{
		"sibling":                       d.s,
		"unrelated agent":               d.x,
		"own parent":                    d.l,
		"descendant in another project": forged,
	}
	for name := range launcherReadPaths(d.proj.ID, "") {
		missingCode, missingBody := d.getAs(t, d.m, launcherReadPaths(d.proj.ID, tid("desc-deny-missing"))[name])
		require.Equal(t, http.StatusNotFound, missingCode, missingBody)
		for targetName, target := range targets {
			code, body := d.getAs(t, d.m, launcherReadPaths(target.ProjectID, target.ID)[name])
			assert.Equal(t, missingCode, code, "%s %s: %s", name, targetName, body)
			assert.Equal(t, missingBody, body, "%s %s", name, targetName)
		}
	}
}

// TestDescendantRead_LogsAndStreamsStayClosed checks that the exception is
// limited to the single-agent GET: every other agent.read decision on a
// descendant (logs, message logs, streams, events, workspace) builds the
// resource without the stored target and is still denied.
func TestDescendantRead_LogsAndStreamsStayClosed(t *testing.T) {
	d := newDescendantChain(t, "desc-closed")
	identity := d.agentIdentityFor(t, d.agentToken(t, d.m.ID))

	decision := d.srv.authzService.CheckAccess(context.Background(), identity, agentResource(d.c), ActionRead)
	assert.False(t, decision.Allowed, "agent.read on a descendant outside the single-agent GET: %q", decision.Reason)

	single := d.srv.authzService.CheckAccess(context.Background(), identity, agentStatusReadResource(d.c), ActionRead)
	assert.True(t, single.Allowed, "control: the single-agent GET resource is allowed: %q", single.Reason)
	assert.Contains(t, single.Reason, descendantReadReasonMarker, "the decision records the descendant status read")

	// The HTTP routes that decide agent.read on agentResource answer the
	// descendant as they answer an unrelated agent.
	// (The SSE agent.<id>.* gate is on the web server and decides with
	// CheckAccess on agentResource, asserted above.)
	for _, sub := range []string{"logs", "cloud-logs", "cloud-logs/stream", "message-logs", "message-logs/stream"} {
		childCode, childBody := d.getAs(t, d.m, "/api/v1/agents/"+d.c.ID+"/"+sub)
		otherCode, _ := d.getAs(t, d.m, "/api/v1/agents/"+d.x.ID+"/"+sub)
		assert.NotEqual(t, http.StatusOK, childCode, "%s: %s", sub, childBody)
		assert.Equal(t, otherCode, childCode, "%s: %s", sub, childBody)
	}
}

// TestDescendantRead_OtherPermissionsUnchanged checks that the bounded-hop
// exception admits agent.read only, and only when the caller reads a
// descendant.
func TestDescendantRead_OtherPermissionsUnchanged(t *testing.T) {
	d := newDescendantChain(t, "desc-perms")
	requireBoundedWithoutAgentRead(t, d.mEdge)
	res := agentStatusReadResource(d.c)

	c, _ := hopEffectCeilingDenyFor(d.mEdge, permissionAgentRead, res, d.m.ID, false, true)
	assert.Empty(t, c, "agent.read for a descendant passes the bounded hop")
	c, _ = hopEffectCeilingDenyFor(d.mEdge, permissionAgentRead, res, d.m.ID, false, false)
	assert.Equal(t, DenyCauseCeilingEffectExceeded, c, "agent.read without the descendant read")

	for _, perm := range []string{"agent.update", "agent.list", "agent.message", "agent.port_access"} {
		require.False(t, slices.Contains(d.mEdge.PermissionIDs, perm), "%s is not in the frozen ceiling", perm)
		c, _ := hopEffectCeilingDenyFor(d.mEdge, perm, res, d.m.ID, false, true)
		assert.Equal(t, DenyCauseCeilingEffectExceeded, c, perm)
	}
}

// TestDescendantRead_DelegatorChecksStillApply checks that the exception
// does not skip the delegator checks: when the chain's root user is no
// longer active, the ceiling denies the descendant read at the delegator
// check, before any effect ceiling is consulted.
func TestDescendantRead_DelegatorChecksStillApply(t *testing.T) {
	d := newDescendantChain(t, "desc-root")
	ctx := context.Background()
	m := d.m
	identity := d.agentIdentityFor(t, d.agentToken(t, m.ID))
	req := AuthzRequest{
		Principal:  PrincipalContext{Kind: PrincipalKindAgent, ID: m.ID, Identity: identity},
		Resource:   agentStatusReadResource(d.c),
		Action:     ActionRead,
		Permission: permissionAgentRead,
	}

	var explain []DecisionStep
	allowed, reason, err := d.srv.authzService.checkDelegationCeiling(ctx, req, permissionAgentRead, m.ID, &explain, nil)
	require.NoError(t, err)
	require.True(t, allowed, "control: the descendant read passes while the root is active: %q", reason)
	assert.True(t, hasDecisionStep(explain, "delegation_ceiling_descendant_read"), "explain: %+v", explain)

	root, err := d.store.GetUser(ctx, d.creator.ID)
	require.NoError(t, err)
	root.Status = store.UserStatusSuspended
	require.NoError(t, d.store.UpdateUser(ctx, root))

	var cause DenyCause
	explain = nil
	allowed, reason, err = d.srv.authzService.checkDelegationCeiling(ctx, req, permissionAgentRead, m.ID, &explain, &cause)
	require.NoError(t, err)
	assert.False(t, allowed, "a suspended root user supplies no authority: %q", reason)
	assert.Equal(t, DenyCauseCeilingDelegatorLacksPermission, cause, "denied at the delegator check: %q", reason)
}

// TestDescendantRead_UnknownKindOrVersionFailsClosed checks that the
// bounded-hop exception applies only to a bounded edge with a known
// ceiling version: an unknown kind or version still denies agent.read.
func TestDescendantRead_UnknownKindOrVersionFailsClosed(t *testing.T) {
	d := newDescendantChain(t, "desc-kind")
	requireBoundedWithoutAgentRead(t, d.mEdge)
	res := agentStatusReadResource(d.c)

	future := *d.mEdge
	future.Kind = store.EffectCeilingKind("future")
	c, _ := hopEffectCeilingDenyFor(&future, permissionAgentRead, res, d.m.ID, false, true)
	assert.Equal(t, DenyCauseCeilingEffectExceeded, c, "unknown edge kind")

	newer := *d.mEdge
	newer.Version = permissions.CeilingVersionV1 + 1
	c, _ = hopEffectCeilingDenyFor(&newer, permissionAgentRead, res, d.m.ID, false, true)
	assert.Equal(t, DenyCauseCeilingEffectExceeded, c, "unknown ceiling version")
}

// TestDescendantRead_CeilingFlagIsPerTarget checks that the ceiling admits
// agent.read only for the status read of a descendant: the caller's own
// sibling on the single-agent GET resource, and its child outside it, are
// denied at the effect ceiling with no descendant-read step.
func TestDescendantRead_CeilingFlagIsPerTarget(t *testing.T) {
	d := newDescendantChain(t, "desc-flag")
	ctx := context.Background()
	identity := d.agentIdentityFor(t, d.agentToken(t, d.m.ID))
	for name, res := range map[string]Resource{
		"sibling status read":   agentStatusReadResource(d.s),
		"child outside the GET": agentResource(d.c),
	} {
		req := AuthzRequest{
			Principal:  PrincipalContext{Kind: PrincipalKindAgent, ID: d.m.ID, Identity: identity},
			Resource:   res,
			Action:     ActionRead,
			Permission: permissionAgentRead,
		}
		var explain []DecisionStep
		var cause DenyCause
		allowed, reason, err := d.srv.authzService.checkDelegationCeiling(ctx, req, permissionAgentRead, d.m.ID, &explain, &cause)
		require.NoError(t, err, name)
		assert.False(t, allowed, "%s: %q", name, reason)
		assert.Equal(t, DenyCauseCeilingEffectExceeded, cause, name)
		assert.False(t, hasDecisionStep(explain, "delegation_ceiling_descendant_read"), "%s: %+v", name, explain)
	}
}

// TestDescendantRead_AgentDelegatorChecksStillApply checks the agent
// delegator arm: when the caller's own delegator agent is no longer live,
// the descendant read is denied at that delegator, before any effect
// ceiling.
func TestDescendantRead_AgentDelegatorChecksStillApply(t *testing.T) {
	d := newDescendantChain(t, "desc-agentdel")
	ctx := context.Background()
	require.Equal(t, d.l.ID, d.mEdge.DelegatorID, "M's delegator is the agent L")
	identity := d.agentIdentityFor(t, d.agentToken(t, d.m.ID))
	req := AuthzRequest{
		Principal:  PrincipalContext{Kind: PrincipalKindAgent, ID: d.m.ID, Identity: identity},
		Resource:   agentStatusReadResource(d.c),
		Action:     ActionRead,
		Permission: permissionAgentRead,
	}

	l, err := d.store.GetAgent(ctx, d.l.ID)
	require.NoError(t, err)
	l.DeletedAt = time.Now()
	require.NoError(t, d.store.UpdateAgent(ctx, l))

	var explain []DecisionStep
	var cause DenyCause
	allowed, reason, err := d.srv.authzService.checkDelegationCeiling(ctx, req, permissionAgentRead, d.m.ID, &explain, &cause)
	require.NoError(t, err)
	assert.False(t, allowed, "a deleted delegator agent supplies no authority: %q", reason)
	assert.Equal(t, DenyCauseCeilingOrphaned, cause, "denied at the delegator check: %q", reason)
	assert.False(t, hasDecisionStep(explain, "delegation_ceiling_descendant_read"), "explain: %+v", explain)
}

// TestDescendantRead_CeilingHoldingAgentReadNeedsNoException checks that a
// bounded ceiling that already holds agent.read (for example one copied
// from a user access token) allows the read on the normal path: the
// descendant-read exception, and its audit marker, do not apply.
func TestDescendantRead_CeilingHoldingAgentReadNeedsNoException(t *testing.T) {
	d := newDescendantChain(t, "desc-holds")
	requireBoundedWithoutAgentRead(t, d.mEdge)
	res := agentStatusReadResource(d.c)

	holds := *d.mEdge
	holds.PermissionIDs = append(slices.Clone(d.mEdge.PermissionIDs), permissionAgentRead)
	c, _ := hopEffectCeilingDenyFor(&holds, permissionAgentRead, res, d.m.ID, false, true)
	assert.Empty(t, c, "a ceiling holding agent.read allows it")
	assert.False(t, descendantReadPassesHop(&holds, permissionAgentRead, true), "no descendant-read exception, so no marker")
	assert.True(t, descendantReadPassesHop(d.mEdge, permissionAgentRead, true), "control: a ceiling without agent.read uses the exception")
}
