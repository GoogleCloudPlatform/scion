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
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// WHY THESE TESTS EXIST
// ---------------------------------------------------------------------------
//
// The actAs gate is INERT in the current tree. saAssignCheckMode and
// hookIdentityCheckMode are assigned SAAssignCheckOff at server.go:980 and
// nothing else ever writes them; SAAssignCheckEnforce is defined and, outside
// this file, never referenced. Every existing test of the decision logic calls
// store.EvaluateActAs directly.
//
// The consequence is the thing being fixed here: with the checker disabled,
// EVERY assignment is allowed with mechanism "check-disabled", so the observed
// behaviour of a hub whose gate is correctly wired and inert is IDENTICAL to
// the behaviour of a hub where the gate was never connected to the HTTP path at
// all. An invariant outcome across a component's entire behaviour range tells
// you nothing about whether the component is on the path (aid-em rule 15). If
// somebody deleted the authorizeSAAssignment call from the create handler
// tomorrow, the whole suite would still pass.
//
// So each test below drives a real HTTP request end to end, twice, with the
// mode flipped to enforce and a scripted checker that DENIES in one case and
// ALLOWS in the other, and asserts the two produce DIFFERENT results. That pair
// is what makes the gate falsifiable:
//
//   - the deny case proves the gate can refuse, i.e. it is reachable;
//   - the allow case proves the refusal came from the gate and not from some
//     unrelated precondition further up the handler;
//   - CallCount proves the request actually reached the checker, rather than
//     being refused by Hub policy or by a validation step before it.
//
// Without the allow case a broken handler that refuses everything would pass.
// Without CallCount a handler that refuses for an unrelated reason would pass.
//
// ⚠️ These tests are NOT a statement that enforce mode is supported. Nothing in
// the tree can turn it on, and turning it on is out of scope here. They exist
// so that when someone does wire the toggle, they inherit evidence that the
// gate is connected, rather than having to establish it after the fact.
//
// The fields are unexported and these tests are in package hub, so they are set
// directly. No production setter is added for a test's benefit: an exported
// "SetSAAssignCheckMode" would be a way to turn enforcement OFF from outside the
// package, which is a worse thing to have in the tree than a test that reaches
// into a struct.

// enforceSAAssign switches the agent-assignment surface into enforce mode with
// the given checker, and wires a token generator.
//
// The generator is load-bearing rather than incidental: in enforce mode
// saAssignCheckerFor SUBSTITUTES the unavailable checker when
// gcpTokenGenerator is nil (sa_assign_gate.go:173). That substitute also
// denies — so a test that only asserted "the request was refused" would pass
// while the scripted checker was never consulted at all, which is the same
// class of false pass these tests exist to close. With the generator present
// the configured checker is used, and CallCount can prove it.
func enforceSAAssign(srv *Server, checker store.CallerPermissionChecker) {
	srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub@test.iam.gserviceaccount.com"})
	srv.mu.Lock()
	defer srv.mu.Unlock()
	srv.saAssignCheckMode = SAAssignCheckEnforce
	srv.saAssignChecker = checker
}

// enforceHookIdentity is the same for the lifecycle-hook execution-identity
// surface, which has its own mode and its own checker by design.
func enforceHookIdentity(srv *Server, checker store.CallerPermissionChecker) {
	srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub@test.iam.gserviceaccount.com"})
	srv.mu.Lock()
	defer srv.mu.Unlock()
	srv.hookIdentityCheckMode = SAAssignCheckEnforce
	srv.hookIdentityChecker = checker
}

// wiringSA seeds a service account for these tests.
//
// ⚠️ CreatedBy is a stranger on purpose. gcpServiceAccountResource maps
// CreatedBy to Resource.OwnerID and authz short-circuits on resource owner, so
// seeding the account under the caller would let the request pass the Hub
// policy layer for the wrong reason — and, worse for a DENY test, would leave
// the reader unable to tell which layer produced the refusal.
func wiringSA(t *testing.T, s store.Store, scope, scopeID, email string) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID:                 tid("wiring-sa-" + email),
		Scope:              scope,
		ScopeID:            scopeID,
		Email:              email,
		ProjectID:          tid("gcp-project"),
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: store.GCPVerificationVerified,
		CreatedBy:          tid("some-other-user"),
		CreatedAt:          time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), sa))
	return sa
}

// ---------------------------------------------------------------------------
// Surface 1: agent create
// ---------------------------------------------------------------------------

func TestSAAssignGate_AgentCreate_IsOnTheHTTPPath(t *testing.T) {
	// A denial must stop the assignment AND stop the agent: an agent that
	// exists without the identity its creator asked for is a half-applied
	// request, and the caller has no way to tell it apart from success.
	t.Run("deny refuses the request and creates no agent", func(t *testing.T) {
		disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
		srv, s, project := setupCreateAgentServer(t, disp)
		sa := wiringSA(t, s, store.ScopeProject, project.ID, "deny-create@p.iam.gserviceaccount.com")

		checker := store.NewFakeCallerPermissionChecker().
			DenyTarget(sa.Email, "caller lacks iam.serviceAccounts.actAs")
		enforceSAAssign(srv, checker)

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:      "wiring-deny",
			ProjectID: project.ID,
			Task:      "do something",
			GCPIdentity: &GCPIdentityAssignment{
				MetadataMode:     store.GCPMetadataModeAssign,
				ServiceAccountID: sa.ID,
			},
		})

		assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())

		// The assertion that distinguishes "the gate refused" from "something
		// else refused": the checker was actually consulted over HTTP.
		require.Equal(t, 1, checker.CallCount(),
			"the caller-permission checker was never reached; the refusal came from somewhere else "+
				"and this test would pass even if the gate were disconnected")
		assert.Equal(t, sa.ID, checker.Calls()[0].TargetSAID, "checker was asked about the wrong account")

		_, err := s.GetAgentBySlug(context.Background(), project.ID, "wiring-deny")
		assert.ErrorIs(t, err, store.ErrNotFound, "agent must not be created when the assignment is refused")
	})

	// The other half of the range. Same server, same mode, same route — only
	// the checker's answer differs, so a difference in outcome can only have
	// come from the checker.
	t.Run("allow permits the same request", func(t *testing.T) {
		disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
		srv, s, project := setupCreateAgentServer(t, disp)
		sa := wiringSA(t, s, store.ScopeProject, project.ID, "allow-create@p.iam.gserviceaccount.com")

		checker := store.NewFakeCallerPermissionChecker().AllowTarget(sa.Email)
		enforceSAAssign(srv, checker)

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:      "wiring-allow",
			ProjectID: project.ID,
			Task:      "do something",
			GCPIdentity: &GCPIdentityAssignment{
				MetadataMode:     store.GCPMetadataModeAssign,
				ServiceAccountID: sa.ID,
			},
		})

		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
		require.Equal(t, 1, checker.CallCount(), "checker not consulted on the allow path either")

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotNil(t, resp.Agent)
		require.NotNil(t, resp.Agent.AppliedConfig)
		require.NotNil(t, resp.Agent.AppliedConfig.GCPIdentity)
		assert.Equal(t, sa.ID, resp.Agent.AppliedConfig.GCPIdentity.ServiceAccountID)
	})
}

// ---------------------------------------------------------------------------
// Surface 2: agent PATCH
// ---------------------------------------------------------------------------
//
// PATCH is a separate surface with its own call site, so create passing says
// nothing about it. It is also the more dangerous of the two: it attaches an
// identity to an agent that already exists and may already be known to other
// systems.

func TestSAAssignGate_AgentPatch_IsOnTheHTTPPath(t *testing.T) {
	// patchTarget builds an agent in the phase the PATCH handler requires.
	patchTarget := func(t *testing.T, s store.Store, projectID, name string) *store.Agent {
		t.Helper()
		a := &store.Agent{
			ID:        tid("wiring-patch-" + name),
			Slug:      name,
			Name:      name,
			ProjectID: projectID,
			Phase:     string(state.PhaseCreated),
			CreatedBy: tid("some-other-user"),
			OwnerID:   tid("some-other-user"),
		}
		require.NoError(t, s.CreateAgent(context.Background(), a))
		return a
	}

	patch := func(t *testing.T, srv *Server, agentID, saID string) int {
		t.Helper()
		rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agentID,
			map[string]interface{}{
				"gcp_identity": map[string]interface{}{
					"metadata_mode":      store.GCPMetadataModeAssign,
					"service_account_id": saID,
				},
			})
		return rec.Code
	}

	t.Run("deny leaves the agent without the identity", func(t *testing.T) {
		disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
		srv, s, project := setupCreateAgentServer(t, disp)
		sa := wiringSA(t, s, store.ScopeProject, project.ID, "deny-patch@p.iam.gserviceaccount.com")
		agent := patchTarget(t, s, project.ID, "patch-deny")

		checker := store.NewFakeCallerPermissionChecker().
			DenyTarget(sa.Email, "caller lacks iam.serviceAccounts.actAs")
		enforceSAAssign(srv, checker)

		assert.Equal(t, http.StatusForbidden, patch(t, srv, agent.ID, sa.ID))
		require.Equal(t, 1, checker.CallCount(),
			"the caller-permission checker was never reached on the PATCH path")

		// A 403 with the identity attached anyway would be the worst outcome of
		// all: refused on the wire, granted in the database.
		got, err := s.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		if got.AppliedConfig != nil && got.AppliedConfig.GCPIdentity != nil {
			assert.NotEqual(t, store.GCPMetadataModeAssign, got.AppliedConfig.GCPIdentity.MetadataMode,
				"the service account was attached despite the refusal")
		}
	})

	t.Run("allow attaches the identity", func(t *testing.T) {
		disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
		srv, s, project := setupCreateAgentServer(t, disp)
		sa := wiringSA(t, s, store.ScopeProject, project.ID, "allow-patch@p.iam.gserviceaccount.com")
		agent := patchTarget(t, s, project.ID, "patch-allow")

		checker := store.NewFakeCallerPermissionChecker().AllowTarget(sa.Email)
		enforceSAAssign(srv, checker)

		require.Equal(t, http.StatusOK, patch(t, srv, agent.ID, sa.ID))
		require.Equal(t, 1, checker.CallCount(), "checker not consulted on the allow path either")

		got, err := s.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		require.NotNil(t, got.AppliedConfig)
		require.NotNil(t, got.AppliedConfig.GCPIdentity)
		assert.Equal(t, sa.ID, got.AppliedConfig.GCPIdentity.ServiceAccountID)
	})
}

// ---------------------------------------------------------------------------
// Surface 3: lifecycle-hook execution identity
// ---------------------------------------------------------------------------
//
// This surface lives in pkg/lifecyclehooks and is reached through a different
// handler, a different checker field and a different mode field. Its denial is
// also reported differently — as a 400 field error rather than a 403 — because
// hook validation collects field errors rather than short-circuiting. That
// difference is exactly why the surface needs its own wiring test: nothing
// about the agent path generalises to it.

func TestHookIdentityGate_IsOnTheHTTPPath(t *testing.T) {
	hookRequest := func(saID string) createLifecycleHookRequest {
		req := validCreateRequest()
		req.ExecutionIdentity = saID
		return req
	}

	t.Run("deny refuses the hook", func(t *testing.T) {
		srv, s := testServer(t)
		sa := wiringSA(t, s, store.ScopeHub, "test-hub-id", "deny-hook@p.iam.gserviceaccount.com")

		checker := store.NewFakeCallerPermissionChecker().
			DenyTarget(sa.Email, "caller lacks iam.serviceAccounts.actAs")
		enforceHookIdentity(srv, checker)

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/lifecycle-hooks", hookRequest(sa.ID))

		require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
		require.Equal(t, 1, checker.CallCount(),
			"the caller-permission checker was never reached on the lifecycle-hook path")

		var body ErrorResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
		// Pinned because the remedy is not guessable from "invalid": the caller
		// needs to know it is a permission on a named account, not a malformed
		// field.
		assert.Contains(t, body.Error.Message, store.PermissionActAs)

		hooks, err := s.ListLifecycleHooks(context.Background(), store.LifecycleHookFilter{}, store.ListOptions{})
		require.NoError(t, err)
		assert.Empty(t, hooks.Items, "hook must not be created when the execution identity is refused")
	})

	t.Run("allow creates the hook", func(t *testing.T) {
		srv, s := testServer(t)
		sa := wiringSA(t, s, store.ScopeHub, "test-hub-id", "allow-hook@p.iam.gserviceaccount.com")

		checker := store.NewFakeCallerPermissionChecker().AllowTarget(sa.Email)
		enforceHookIdentity(srv, checker)

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/lifecycle-hooks", hookRequest(sa.ID))

		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
		require.Equal(t, 1, checker.CallCount(), "checker not consulted on the allow path either")

		var hook store.LifecycleHook
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&hook))
		assert.Equal(t, sa.ID, hook.ExecutionIdentity)
	})
}

// ---------------------------------------------------------------------------
// The toggle itself
// ---------------------------------------------------------------------------

// TestModeOffIgnoresSetSAAssignChecker proves the INV-4 fix: when
// saAssignCheckMode is off, saAssignCheckerFor returns the disabled checker
// even after SetSAAssignChecker has overwritten the field with a real checker.
// Without the fix, server_foreground.go's unconditional SetSAAssignChecker
// call replaces the disabled checker with the PT checker, causing every
// mode=off assignment to hit GCP and potentially be denied.
func TestModeOffIgnoresSetSAAssignChecker(t *testing.T) {
	t.Run("saAssignCheckerFor returns disabled checker despite SetSAAssignChecker", func(t *testing.T) {
		srv, _ := testServer(t)

		// Precondition: mode is off (the default set by NewServer).
		require.Equal(t, SAAssignCheckOff, srv.saAssignCheckMode,
			"test assumes mode defaults to off")

		// Simulate what server_foreground.go does: overwrite the checker.
		realChecker := store.NewFakeCallerPermissionChecker().
			DenyTarget("any@test.iam.gserviceaccount.com", "should never be consulted")
		srv.SetSAAssignChecker(realChecker)

		// The resolver must return the disabled checker, not the one just set.
		got := srv.saAssignCheckerFor()
		result, err := got.CanActAs(context.Background(), store.Principal{
			Kind: store.PrincipalUser, ID: "u1", Email: "u1@test.com",
		}, &store.GCPServiceAccount{Email: "any@test.iam.gserviceaccount.com"})

		require.NoError(t, err)
		assert.Equal(t, store.ActAsAllowed, result.Outcome,
			"mode=off must allow; the overwritten checker would have denied")
		assert.Equal(t, store.MechanismCheckDisabled, result.Mechanism,
			"mechanism must be check-disabled, proving the disabled checker was used")
		assert.Equal(t, 0, realChecker.CallCount(),
			"the real checker must not be consulted when mode=off")
	})

	t.Run("hookIdentityCheckerFor returns disabled checker despite SetHookIdentityChecker", func(t *testing.T) {
		srv, _ := testServer(t)

		require.Equal(t, SAAssignCheckOff, srv.hookIdentityCheckMode,
			"test assumes hook identity mode defaults to off")

		realChecker := store.NewFakeCallerPermissionChecker().
			DenyTarget("any@test.iam.gserviceaccount.com", "should never be consulted")
		srv.SetHookIdentityChecker(realChecker)

		got := srv.hookIdentityCheckerFor()
		result, err := got.CanActAs(context.Background(), store.Principal{
			Kind: store.PrincipalUser, ID: "u1", Email: "u1@test.com",
		}, &store.GCPServiceAccount{Email: "any@test.iam.gserviceaccount.com"})

		require.NoError(t, err)
		assert.Equal(t, store.ActAsAllowed, result.Outcome,
			"mode=off must allow; the overwritten checker would have denied")
		assert.Equal(t, store.MechanismCheckDisabled, result.Mechanism,
			"mechanism must be check-disabled, proving the disabled checker was used")
		assert.Equal(t, 0, realChecker.CallCount(),
			"the real checker must not be consulted when mode=off")
	})
}

// TestSAAssignGate_EnforceWithoutGeneratorRefuses pins the other thing the mode
// field controls, and the one with the sharpest failure mode if it regresses.
//
// In enforce mode with no GCP token generator, saAssignCheckerFor SUBSTITUTES
// the unavailable checker and the request is refused — even though the
// configured checker, had it been asked, would have allowed. That is a
// deliberate downgrade on a missing capability: a hub that cannot check must
// not silently behave like a hub that checked and approved. It is the defect
// tracked as #29 (verifyGCPServiceAccount, where a nil generator marks accounts
// verified having contacted nothing) not being repeated here.
//
// The allow-scripted checker is what gives the test teeth. If the substitution
// were ever dropped, the configured checker would allow and the request would
// succeed, and this is the only test that would notice.
func TestSAAssignGate_EnforceWithoutGeneratorRefuses(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	sa := wiringSA(t, s, store.ScopeProject, project.ID, "nogen@p.iam.gserviceaccount.com")

	checker := store.NewFakeCallerPermissionChecker().AllowTarget(sa.Email)
	srv.mu.Lock()
	srv.saAssignCheckMode = SAAssignCheckEnforce
	srv.saAssignChecker = checker
	srv.mu.Unlock()
	// Deliberately NO SetGCPTokenGenerator. testServer leaves it nil.

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "no-generator",
		ProjectID: project.ID,
		Task:      "do something",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: sa.ID,
		},
	})

	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 0, checker.CallCount(),
		"the configured checker must not be consulted when the hub cannot support enforcement")

	_, err := s.GetAgentBySlug(context.Background(), project.ID, "no-generator")
	assert.ErrorIs(t, err, store.ErrNotFound)
}
