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
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requireStandingReason(t *testing.T, err error, reason string) {
	t.Helper()
	require.Error(t, err)
	require.ErrorIs(t, err, errAgentNotInStanding)
	assert.Equal(t, reason, standingReason(err))
}

func TestStanding_MemberAgentsInStanding(t *testing.T) {
	f := newMSFixture(t, "member")
	ctx := context.Background()
	require.NoError(t, f.srv.agentStanding(ctx, f.agentA.ID))
	require.NoError(t, f.srv.agentStanding(ctx, f.childC.ID))
}

// The live predicate refuses as soon as access ends, with no hold present.
func TestStanding_RemovedRootDenied_NoHold(t *testing.T) {
	f := newMSFixture(t, "removed-nohold")
	ctx := context.Background()
	f.dropBindings(f.userID)
	require.False(t, f.held(f.agentA.ID))
	requireStandingReason(t, f.srv.agentStanding(ctx, f.agentA.ID), standingReasonRootNotAdmited)
	requireStandingReason(t, f.srv.agentStanding(ctx, f.childC.ID), standingReasonRootNotAdmited)
}

func TestStanding_HeldAndChainHeld(t *testing.T) {
	f := newMSFixture(t, "held-chain")
	ctx := context.Background()
	f.hold(f.agentA.ID, f.userID)
	requireStandingReason(t, f.srv.agentStanding(ctx, f.agentA.ID), standingReasonAgentHeld)
	requireStandingReason(t, f.srv.agentStanding(ctx, f.childC.ID), standingReasonChainHeld)
}

func TestStanding_InactiveRootDenied(t *testing.T) {
	f := newMSFixture(t, "inactive-root")
	ctx := context.Background()
	u, err := f.s.GetUser(ctx, f.userID)
	require.NoError(t, err)
	u.Status = "suspended"
	require.NoError(t, f.s.UpdateUser(ctx, u))
	requireStandingReason(t, f.srv.agentStanding(ctx, f.agentA.ID), standingReasonRootInactive)
}

// An agent with no edge, no owner, no ancestry and no creator has no
// resolvable root and is refused.
func TestStanding_NoResolvableRootDenied(t *testing.T) {
	f := newMSFixture(t, "noroot")
	a := f.newAgentRow("orphan", "", "", nil)
	requireStandingReason(t, f.srv.agentStanding(context.Background(), a.ID), standingReasonNoRoot)
}

// An agent with no edge resolves through its stored links (owner user).
func TestStanding_LegacyOwnerFallback(t *testing.T) {
	f := newMSFixture(t, "legacy-owner")
	ctx := context.Background()
	legacy := f.newAgentRow("legacy", f.userID, f.userID, nil)
	require.NoError(t, f.srv.agentStanding(ctx, legacy.ID))
	// A scheduled child without an edge: no owner, creator is the agent.
	sched := f.newAgentRow("sched", "", legacy.ID, nil)
	require.NoError(t, f.srv.agentStanding(ctx, sched.ID))
	f.dropBindings(f.userID)
	requireStandingReason(t, f.srv.agentStanding(ctx, legacy.ID), standingReasonRootNotAdmited)
	requireStandingReason(t, f.srv.agentStanding(ctx, sched.ID), standingReasonRootNotAdmited)
}

// An agent reached through an edge must carry its own edge: a missing edge
// above the first link breaks the chain.
func TestStanding_BrokenChainDenied(t *testing.T) {
	f := newMSFixture(t, "broken")
	ctx := context.Background()
	mid := f.newAgentRow("mid-noedge", f.userID, f.userID, []string{f.userID})
	child := f.newAgentRow("child-of-noedge", mid.ID, mid.ID, []string{f.userID, mid.ID})
	f.edge(store.DelegationPrincipalAgent, mid.ID, child)
	requireStandingReason(t, f.srv.agentStanding(ctx, child.ID), standingReasonChainBroken)
}

// The standing resolver follows chains up to the delegation ceiling's
// bound; deeper agents are refused with chain_too_deep (the descendant walk
// still reaches and holds them).
func TestStanding_ChainBeyondResolverBound(t *testing.T) {
	f := newMSFixture(t, "deep")
	ctx := context.Background()
	parent := f.agentA
	var chain []*store.Agent
	for i := 0; i < standingMaxChainDepth+1; i++ {
		parent = f.childAgent(fmt.Sprintf("deep-%d", i), parent)
		chain = append(chain, parent)
	}
	require.NoError(t, f.srv.agentStanding(ctx, chain[standingMaxChainDepth-2].ID))
	requireStandingReason(t, f.srv.agentStanding(ctx, chain[len(chain)-1].ID), standingReasonChainTooDeep)
}

func TestStanding_DeletedAgentDenied(t *testing.T) {
	f := newMSFixture(t, "deleted")
	ctx := context.Background()
	a, err := f.s.GetAgent(ctx, f.agentA.ID)
	require.NoError(t, err)
	a.DeletedAt = time.Now()
	require.NoError(t, f.s.UpdateAgent(ctx, a))
	requireStandingReason(t, f.srv.agentStanding(ctx, f.agentA.ID), standingReasonAgentDeleted)
	requireStandingReason(t, f.srv.agentStanding(ctx, f.childC.ID), standingReasonChainDeleted)
}

// standingFaultStore injects a fault into one named lookup.
type standingFaultStore struct {
	store.Store
	mu   sync.Mutex
	fail string
}

var errStandingInjected = errors.New("injected standing fault")

func (s *standingFaultStore) should(m string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fail == m
}

func (s *standingFaultStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if s.should("GetAgent") {
		return nil, errStandingInjected
	}
	return s.Store.GetAgent(ctx, id)
}

func (s *standingFaultStore) HasActiveAgentHold(ctx context.Context, id string) (bool, error) {
	if s.should("HasActiveAgentHold") {
		return false, errStandingInjected
	}
	return s.Store.HasActiveAgentHold(ctx, id)
}

func (s *standingFaultStore) GetDelegationEdgesForDelegate(ctx context.Context, t, id string) ([]*store.DelegationEdge, error) {
	if s.should("GetDelegationEdgesForDelegate") {
		return nil, errStandingInjected
	}
	return s.Store.GetDelegationEdgesForDelegate(ctx, t, id)
}

func (s *standingFaultStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if s.should("GetUser") {
		return nil, errStandingInjected
	}
	return s.Store.GetUser(ctx, id)
}

func (s *standingFaultStore) ListRoleBindingsForPrincipals(ctx context.Context, p []store.PrincipalRef, st []string, sid []string) ([]*store.RoleBinding, error) {
	if s.should("ListRoleBindingsForPrincipals") {
		return nil, errStandingInjected
	}
	return s.Store.ListRoleBindingsForPrincipals(ctx, p, st, sid)
}

// installStandingFaults routes the server's and the authz service's reads
// through a fault store for the test.
func installStandingFaults(t *testing.T, f *msFixture) *standingFaultStore {
	t.Helper()
	fs := &standingFaultStore{Store: f.s}
	origSrv, origAuthz := f.srv.store, f.srv.authzService.store
	f.srv.store = fs
	f.srv.authzService.store = fs
	t.Cleanup(func() {
		f.srv.store = origSrv
		f.srv.authzService.store = origAuthz
	})
	return fs
}

// Every failed lookup refuses, and a fault is not reported as
// good standing.
func TestStanding_NilAndFaults(t *testing.T) {
	f := newMSFixture(t, "faults")
	ctx := context.Background()
	fs := installStandingFaults(t, f)
	for _, m := range []string{"GetAgent", "HasActiveAgentHold", "GetDelegationEdgesForDelegate", "GetUser", "ListRoleBindingsForPrincipals"} {
		t.Run(m, func(t *testing.T) {
			fs.mu.Lock()
			fs.fail = m
			fs.mu.Unlock()
			defer func() { fs.mu.Lock(); fs.fail = ""; fs.mu.Unlock() }()
			err := f.srv.agentStanding(ctx, f.childC.ID)
			require.Error(t, err, "a %s fault must refuse", m)
		})
	}
	require.NoError(t, f.srv.agentStanding(ctx, f.childC.ID))
	requireStandingReason(t, f.srv.agentStanding(ctx, ""), standingReasonAgentMissing)
	requireStandingReason(t, f.srv.agentStanding(ctx, tid("ms-no-such-agent")), standingReasonAgentMissing)
}

// Results are memoised per request only.
func TestStanding_NoCrossRequestCache(t *testing.T) {
	f := newMSFixture(t, "nocache")
	req1 := withStandingMemo(context.Background())
	require.NoError(t, f.srv.agentStanding(req1, f.agentA.ID))
	f.dropBindings(f.userID)
	req2 := withStandingMemo(context.Background())
	requireStandingReason(t, f.srv.agentStanding(req2, f.agentA.ID), standingReasonRootNotAdmited)
	// Without a memo every call reads live state.
	requireStandingReason(t, f.srv.agentStanding(context.Background(), f.agentA.ID), standingReasonRootNotAdmited)
}

// The check reads no setting.
func TestFallback_IgnoresAutoSuspendSetting(t *testing.T) {
	f := newMSFixture(t, "setting")
	f.dropBindings(f.userID)
	for _, on := range []bool{false, true} {
		f.srv.config.AutoSuspendStalled = on
		requireStandingReason(t, f.srv.agentStanding(context.Background(), f.childC.ID), standingReasonRootNotAdmited)
	}
}

// A child created after the removal, before any sweep, is refused live.
func TestStanding_StragglerChildDenied(t *testing.T) {
	f := newMSFixture(t, "straggler")
	ctx := context.Background()
	f.removeAndProcess(f.userID)
	require.True(t, f.held(f.agentA.ID))
	straggler := f.childAgent("straggler", f.agentA)
	require.False(t, f.held(straggler.ID))
	requireStandingReason(t, f.srv.agentStanding(ctx, straggler.ID), standingReasonChainHeld)
}

// ---------------------------------------------------------------------------
// Refusal table. One fixture: U removed from P with no hold
// written, so each row exercises the live predicate at its own gate. Revert
// proof: removing a gate's line makes its row fail.
// ---------------------------------------------------------------------------

func TestStandingGates_RemovedRootDenied(t *testing.T) {
	f := newMSFixture(t, "gates")
	ctx := context.Background()
	tokA := f.agentToken(f.agentA)
	f.dropBindings(f.userID)
	require.False(t, f.held(f.agentA.ID))

	isForbidden := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	}

	t.Run("startGate", func(t *testing.T) {
		rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentA.ID+"/start", nil)
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), agentSuspendedConflictMessage)
	})
	t.Run("startGate_direct", func(t *testing.T) {
		for _, entry := range []startEntry{startEntryStart, startEntryRestart, startEntryWake, startEntryReincarnate, startEntryCreateExisting} {
			refusal := f.srv.startGate(ctx, f.agentA, entry)
			require.NotNil(t, refusal, string(entry))
			assert.Equal(t, http.StatusConflict, refusal.HTTPStatus, string(entry))
			assert.Equal(t, agentSuspendedConflictMessage, refusal.Message, string(entry))
		}
		assert.Nil(t, f.srv.startGate(ctx, f.agentA, startEntryRestore), "restore is not refused by the gate")
	})
	t.Run("dispatcher", func(t *testing.T) {
		d := NewHTTPAgentDispatcherWithClient(f.s, nil, false, nil)
		f.srv.SetDispatcher(d)
		err := d.DispatchAgentStart(ctx, f.agentA, "", false)
		require.ErrorIs(t, err, ErrAgentNotInStanding)
		err = d.DispatchAgentRestart(ctx, f.agentA)
		require.ErrorIs(t, err, ErrAgentNotInStanding)
	})
	t.Run("managed", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.agentA.ID+"/start", nil)
		f.srv.handleManagedAgentLifecycle(rec, req, f.agentA, "start")
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	})
	t.Run("tokenMint", func(t *testing.T) {
		_, err := f.srv.GenerateAgentTokenForAgent(ctx, f.agentA)
		require.ErrorIs(t, err, errAgentNotInStanding)
	})
	t.Run("selfMessage", func(t *testing.T) {
		ok, reason, _ := f.srv.authorizeAgentMessage(ctx, f.agentIdentity(f.agentA), f.agentA, false)
		assert.False(t, ok)
		assert.Equal(t, messageReasonSenderNotPermitted, reason)
	})
	t.Run("evaluateAgentMessage", func(t *testing.T) {
		d := f.srv.EvaluateAgentMessage(ctx, f.agentIdentity(f.agentA), f.childC)
		assert.False(t, d.Allowed)
		assert.Equal(t, messageReasonSenderNotPermitted, d.Reason)
	})
	t.Run("scheduledAuthorAgent", func(t *testing.T) {
		evt := store.ScheduledEvent{ID: "evt", EventType: "message", ProjectID: f.projectID, CreatedBy: f.agentA.ID}
		require.Error(t, f.srv.scheduledFireStanding(ctx, evt, f.childC))
	})
	t.Run("scheduledDispatchAuthor", func(t *testing.T) {
		evt := store.ScheduledEvent{ID: "evt2", EventType: "dispatch_agent", ProjectID: f.projectID, CreatedBy: f.agentA.ID}
		require.Error(t, f.srv.scheduledFireStanding(ctx, evt, nil))
	})
	t.Run("agentCreatesChild", func(t *testing.T) {
		rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents",
			map[string]interface{}{"name": "gate-child", "projectId": f.projectID}, tokA)
		isForbidden(t, rec)
	})
	t.Run("ceilingUserHop", func(t *testing.T) {
		u, err := f.s.GetUser(ctx, f.userID)
		require.NoError(t, err)
		ok, _, err := f.srv.authzService.userRelationshipAuthority(ctx, u, agentResource(f.agentA), ActionRead, "agent.read")
		require.NoError(t, err)
		assert.False(t, ok, "the removed user's owner relationship must not grant authority")
	})
	t.Run("projectEnv", func(t *testing.T) {
		isForbidden(t, doRequestWithAgentToken(t, f.srv, http.MethodGet, "/api/v1/projects/"+f.projectID+"/env", nil, tokA))
	})
	t.Run("projectSecrets", func(t *testing.T) {
		isForbidden(t, doRequestWithAgentToken(t, f.srv, http.MethodGet, "/api/v1/projects/"+f.projectID+"/secrets", nil, tokA))
	})
	t.Run("scopedEnv", func(t *testing.T) {
		isForbidden(t, doRequestWithAgentToken(t, f.srv, http.MethodGet, "/api/v1/env?scope=project&scopeId="+f.projectID, nil, tokA))
	})
	t.Run("hubEnv", func(t *testing.T) {
		isForbidden(t, doRequestWithAgentToken(t, f.srv, http.MethodGet, "/api/v1/env?scope=hub", nil, tokA))
	})
	t.Run("agentSecretWrite", func(t *testing.T) {
		rec := httptest.NewRecorder()
		assert.True(t, f.srv.agentStandingForbidden(ctx, rec, f.agentA.ID))
		isForbidden(t, rec)
	})
	t.Run("materialPrecheck", func(t *testing.T) {
		// The precheck's own ancestry-root membership check refuses first
		// here; the standing check's own row is materialAfterHold.
		_, _, status := f.srv.materialRuntimePrecheck(ctx, f.agentIdentity(f.agentA))
		assert.Equal(t, http.StatusForbidden, status)
	})
	t.Run("githubToken", func(t *testing.T) {
		isForbidden(t, doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentA.ID+"/refresh-token", nil, tokA))
	})
	t.Run("gcpToken", func(t *testing.T) {
		rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agent/gcp-token", map[string]interface{}{}, tokA)
		isForbidden(t, rec)
		var body struct {
			Error struct{ Message string } `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		assert.NotContains(t, body.Error.Message, "service account", "refused before the assignment lookup")
	})
}

// Gates that read the hold itself: one fixture with A held, U still a member.
func TestStandingGates_HeldAgent(t *testing.T) {
	f := newMSFixture(t, "gates-held")
	ctx := context.Background()
	tokA := f.agentToken(f.agentA)
	f.hold(f.agentA.ID, f.userID)

	t.Run("agentTokenAuth", func(t *testing.T) {
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, "/api/v1/agents/"+f.agentA.ID, nil, tokA)
		require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "token has been revoked")
	})
	t.Run("legacyToken", func(t *testing.T) {
		legacyService, err := NewAgentTokenService(AgentTokenConfig{
			SigningKey:    f.srv.agentTokenService.config.SigningKey,
			TokenDuration: time.Hour,
		})
		require.NoError(t, err)
		legacy, err := legacyService.GenerateAgentToken(f.agentA.ID, f.projectID, ScopesForRole(AgentRoleFull), nil)
		require.NoError(t, err)
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, "/api/v1/agents/"+f.agentA.ID, nil, legacy)
		require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	})
	// No new external token for a held agent.
	t.Run("noGitHubTokenAfterHold", func(t *testing.T) {
		rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentA.ID+"/refresh-token", nil, tokA)
		require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	})
	t.Run("noGCPTokenAfterHold", func(t *testing.T) {
		for _, path := range []string{"/api/v1/agent/gcp-token", "/api/v1/agent/gcp-identity-token"} {
			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, path, map[string]interface{}{}, tokA)
			require.Equal(t, http.StatusUnauthorized, rec.Code, path+": "+rec.Body.String())
		}
	})
	t.Run("standingRefusesHeldBeforeAuth", func(t *testing.T) {
		rec := httptest.NewRecorder()
		assert.True(t, f.srv.agentStandingForbidden(ctx, rec, f.agentA.ID))
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})
	t.Run("materialAfterHold", func(t *testing.T) {
		_, reason, status := f.srv.materialRuntimePrecheck(ctx, f.agentIdentity(f.agentA))
		assert.Equal(t, http.StatusForbidden, status)
		assert.Equal(t, ReasonDeniedByPolicy, reason)
	})
	t.Run("reconcileDelivery", func(t *testing.T) {
		err := f.srv.deliverMessage(ctx, &store.Message{AgentID: f.agentA.ID, Msg: "hi"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "suspended")
	})
	t.Run("dmToHeldTarget", func(t *testing.T) {
		assert.NotNil(t, heldTargetDMError(f.agentA))
	})
	t.Run("suspendedPrimaryWakeable", func(t *testing.T) {
		a := *f.agentA
		a.Phase = string(state.PhaseSuspended)
		owner := NewAuthenticatedUser(f.ownerID, f.ownerID+"@test.com", "Owner", "member", "")
		assert.False(t, f.srv.suspendedPrimaryWakeable(ctx, owner, &a))
	})
	t.Run("scheduledTargetHeld", func(t *testing.T) {
		evt := store.ScheduledEvent{ID: "evt3", EventType: "message", ProjectID: f.projectID, CreatedBy: f.ownerID}
		require.ErrorIs(t, f.srv.scheduledFireStanding(ctx, evt, f.agentA), errScheduledMessageRefused)
	})
	t.Run("ceilingAgentHop", func(t *testing.T) {
		_, _, err := f.srv.authzService.checkAgentHoldsPermission(ctx, f.agentA.ID, "agent.read", store.RoleScopeProject, f.projectID)
		require.ErrorIs(t, err, store.ErrNotFound)
	})
	t.Run("heldMemberStillRefused", func(t *testing.T) {
		rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentA.ID+"/start", nil)
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), agentSuspendedConflictMessage)
	})
}

// With no dispatcher standing hook at all, a held agent is
// still refused at token mint, auth and messaging.
func TestHeldAgent_RefusedWithoutDispatcherHook(t *testing.T) {
	f := newMSFixture(t, "nohook")
	ctx := context.Background()
	d := NewHTTPAgentDispatcherWithClient(f.s, nil, false, nil)
	f.srv.mu.Lock()
	f.srv.dispatcher = d // attached without SetDispatcher: no hook
	f.srv.mu.Unlock()
	require.Nil(t, d.requiredStandingCheck)
	tokA := f.agentToken(f.agentA)
	f.hold(f.agentA.ID, f.userID)

	_, err := f.srv.GenerateAgentTokenForAgent(ctx, f.agentA)
	require.ErrorIs(t, err, errAgentNotInStanding)
	rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, "/api/v1/agents/"+f.agentA.ID, nil, tokA)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	ok, _, _ := f.srv.authorizeAgentMessage(ctx, f.agentIdentity(f.agentA), f.childC, false)
	assert.False(t, ok)
	assert.NotNil(t, f.srv.startGate(ctx, f.agentA, startEntryStart))
}

// Every production path that attaches a dispatcher
// installs the standing check. Revert proof: drop the install in
// SetDispatcher (or CreateAuthenticatedDispatcher) and this fails.
func TestDispatcherStandingHook_ProductionWiring(t *testing.T) {
	srv, _ := testServer(t)
	d := srv.CreateAuthenticatedDispatcher()
	require.NotNil(t, d.requiredStandingCheck, "CreateAuthenticatedDispatcher must install the standing check")

	raw := NewHTTPAgentDispatcherWithClient(srv.store, nil, false, nil)
	require.Nil(t, raw.requiredStandingCheck)
	srv.SetDispatcher(raw)
	require.NotNil(t, raw.requiredStandingCheck, "SetDispatcher must install the standing check")
}

// A removed root's agent and a never-member user's agent
// get the same answer at the agent-facing gates.
func TestRemovedVsNeverMember_Indistinguishable(t *testing.T) {
	f := newMSFixture(t, "oracle")
	never := tid("ms-oracle-never")
	f.addUser(never)
	neverAgent := f.userAgent("never-agent", never)
	tokA := f.agentToken(f.agentA)
	tokN := f.agentToken(neverAgent)
	f.dropBindings(f.userID)

	for _, path := range []string{
		"/api/v1/projects/" + f.projectID + "/env",
		"/api/v1/projects/" + f.projectID + "/secrets",
	} {
		a := doRequestWithAgentToken(t, f.srv, http.MethodGet, path, nil, tokA)
		n := doRequestWithAgentToken(t, f.srv, http.MethodGet, path, nil, tokN)
		assert.Equal(t, n.Code, a.Code, path)
		assert.Equal(t, n.Body.String(), a.Body.String(), path)
	}
	ra := f.srv.startGate(context.Background(), f.agentA, startEntryStart)
	rn := f.srv.startGate(context.Background(), neverAgent, startEntryStart)
	require.NotNil(t, ra)
	require.NotNil(t, rn)
	assert.Equal(t, rn.Message, ra.Message)
	assert.Equal(t, rn.HTTPStatus, ra.HTTPStatus)
}
