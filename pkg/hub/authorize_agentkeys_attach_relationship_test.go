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

// Tests for ptone/scion#2460: the agent-credential branch of
// authorizeAgentKeys (authorize_agentkeys.go) now also requires live attach
// authority on the target via authorizeAgentTargetAction(ctx, identity,
// target, ActionAttach) -- B.2's single relationship-evaluation rule for
// lifecycle, attach and delete, merged forward onto this branch from
// origin/pat/2120-b2-parent-ceilings. Same-project equality plus lifecycle
// scope is no longer sufficient on its own: the caller's delegation chain
// must also reach attach authority on the specific target, with no
// permissive fallback on an invalid/revoked delegation or an evaluator
// failure. See .design/agent-keys-contract.md's Agent-credential row,
// AK-20/AK-20a, and the Phase 5 note (this supersedes that deferral for this
// one gate only). User callers are unchanged by this task.
//
// TestExecuteAgentKeys_AgentCallerRequiresAttachRelationship's subtests mark
// the delegation-edge backfill complete (markEdgeBackfillComplete,
// agent_target_authz_test.go), the same precondition B.2's own tests use --
// the state production will be in once the backfill migration has run.
// TestExecuteAgentKeys_AgentCallerPreBackfillParity instead names, directly,
// the pre-backfill legacy allowance every other ExecuteAgentKeys
// agent-caller test in this package relies on only implicitly (none of them
// marks the backfill complete or seeds a delegation edge): a caller with no
// edge at all is grandfathered by authz_delegation_ceiling.go's legacy
// allowance (protecting already-deployed callers), which is deliberately
// untouched by this task and is attach parity, not a keys-only bypass --
// every other attach-gated agent action that calls authorizeAgentTargetAction
// gets the identical pre-backfill treatment.

import (
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestExecuteAgentKeys_AgentCallerRequiresAttachRelationship covers the
// acceptance criteria ptone/scion#2460's binding ruling lists: deny with no
// live delegation, deny when a live delegation's delegator itself lacks
// attach authority on the target (AK-20a's first-named case), allow with a
// valid one, fail closed on a revoked delegation and on an evaluator
// failure, and preserve the cross-project ordering -- on both route shapes,
// with zero dispatch and zero budget effect on every denial, and
// response/audit operation-ID equality.
func TestExecuteAgentKeys_AgentCallerRequiresAttachRelationship(t *testing.T) {
	t.Run("deny: same-project caller with no delegation edge", func(t *testing.T) {
		for _, shape := range keysRouteShapes {
			t.Run(shape.name, func(t *testing.T) {
				f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
				markEdgeBackfillComplete(t, f.store)
				callerID := tid("execkeys-attach-deny-" + shape.name)
				token := f.agentToken(t, callerID, f.projectA.ID, ScopeAgentLifecycle)
				log := installSentinelLogCapture(t)

				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
				assertKeysDenialOutcome(t, shape.name+"/no delegation edge", rec, http.StatusForbidden, "keys_denied")
				assertKeysDenialOutcomeAuditMatches(t, shape.name+"/no delegation edge", rec, log)
				assertNoKeysSideEffects(t, storeSpy, events)
				if got := d.callCount(); got != 0 {
					t.Errorf("%s: dispatcher called %d times, want 0 (denied before admission)", shape.name, got)
				}
			})
		}
	})

	t.Run("deny: same-project caller whose delegator lacks attach", func(t *testing.T) {
		// Distinct from the "no edge at all" subtest above: here a live edge
		// exists, so the evaluator runs the full ceiling walk against a real
		// delegator (f.nonOwner, who has no ownership/role on either fixture
		// agent and so no attach authority to delegate) rather than stopping
		// at the post-backfill missing-edge branch. This is the case AK-20a
		// names first ("delegator lacks attach").
		for _, shape := range keysRouteShapes {
			t.Run(shape.name, func(t *testing.T) {
				f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
				markEdgeBackfillComplete(t, f.store)
				callerID := tid("execkeys-attach-delegatorlacks-" + shape.name)
				addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.nonOwner.ID, callerID, f.projectA.ID)
				token := f.agentToken(t, callerID, f.projectA.ID, ScopeAgentLifecycle)
				log := installSentinelLogCapture(t)

				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
				assertKeysDenialOutcome(t, shape.name+"/delegator lacks attach", rec, http.StatusForbidden, "keys_denied")
				assertKeysDenialOutcomeAuditMatches(t, shape.name+"/delegator lacks attach", rec, log)
				assertNoKeysSideEffects(t, storeSpy, events)
				if got := d.callCount(); got != 0 {
					t.Errorf("%s: dispatcher called %d times, want 0 (denied before admission)", shape.name, got)
				}
			})
		}
	})

	t.Run("allow: same-project caller with a live delegation edge", func(t *testing.T) {
		for _, shape := range keysRouteShapes {
			t.Run(shape.name, func(t *testing.T) {
				f, d, _, _ := newExecuteAgentKeysFixture(t)
				markEdgeBackfillComplete(t, f.store)
				callerID := tid("execkeys-attach-allow-" + shape.name)
				// f.owner owns agentInA (and agentInB): delegating from the
				// owner to the caller, at a role covering ScopeAgentLifecycle
				// (the scope agent.attach's registry entry maps to), grants
				// the caller attach authority up to that ceiling.
				addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, callerID, f.projectA.ID)
				token := f.agentToken(t, callerID, f.projectA.ID, ScopeAgentLifecycle)

				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s: expected 200 with a live delegation edge, got %d: %s", shape.name, rec.Code, rec.Body.String())
				}
				if got := d.callCount(); got != 1 {
					t.Errorf("%s: dispatcher called %d times, want 1", shape.name, got)
				}
			})
		}
	})

	t.Run("fail closed: revoked delegation edge", func(t *testing.T) {
		for _, shape := range keysRouteShapes {
			t.Run(shape.name, func(t *testing.T) {
				f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
				markEdgeBackfillComplete(t, f.store)
				callerID := tid("execkeys-attach-revoked-" + shape.name)
				addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, callerID, f.projectA.ID)
				revokeDelegateEdges(t, f.store, callerID)
				token := f.agentToken(t, callerID, f.projectA.ID, ScopeAgentLifecycle)
				log := installSentinelLogCapture(t)

				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
				assertKeysDenialOutcome(t, shape.name+"/revoked edge", rec, http.StatusForbidden, "keys_denied")
				assertKeysDenialOutcomeAuditMatches(t, shape.name+"/revoked edge", rec, log)
				assertNoKeysSideEffects(t, storeSpy, events)
				if got := d.callCount(); got != 0 {
					t.Errorf("%s: dispatcher called %d times, want 0", shape.name, got)
				}
			})
		}
	})

	t.Run("fail closed: evaluator failure", func(t *testing.T) {
		for _, shape := range keysRouteShapes {
			t.Run(shape.name, func(t *testing.T) {
				f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
				markEdgeBackfillComplete(t, f.store)
				callerID := tid("execkeys-attach-evalfail-" + shape.name)
				// A live edge exists, but the evaluator's own delegation-edge
				// lookup fails for this caller (edgeLookupErrStore,
				// authz_ceiling_store_error_test.go) -- the evaluator must
				// still deny, never fall back to "allowed" on its own error.
				addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, callerID, f.projectA.ID)
				f.srv.authzService.store = &edgeLookupErrStore{Store: f.store, failID: callerID}
				token := f.agentToken(t, callerID, f.projectA.ID, ScopeAgentLifecycle)
				log := installSentinelLogCapture(t)

				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
				assertKeysDenialOutcome(t, shape.name+"/evaluator failure", rec, http.StatusForbidden, "keys_denied")
				assertKeysDenialOutcomeAuditMatches(t, shape.name+"/evaluator failure", rec, log)
				assertNoKeysSideEffects(t, storeSpy, events)
				if got := d.callCount(); got != 0 {
					t.Errorf("%s: dispatcher called %d times, want 0", shape.name, got)
				}
			})
		}
	})

	t.Run("cross-project ordering is preserved", func(t *testing.T) {
		// The cross-project refusal must still take precedence over the new
		// attach-relationship check: a cross-project agent caller is denied
		// as cross_project_keys_unsupported (422), never keys_denied (403),
		// even when it holds a live delegation edge in the *target's*
		// project -- proving the attach check is reached only after the
		// project check has already passed, never before it.
		for _, shape := range keysRouteShapes {
			t.Run(shape.name, func(t *testing.T) {
				f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
				markEdgeBackfillComplete(t, f.store)
				callerID := tid("execkeys-attach-crossproj-" + shape.name)
				addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, callerID, f.projectB.ID)
				token := f.agentToken(t, callerID, f.projectA.ID, ScopeAgentLifecycle)

				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInB), validKeysBody, token)
				assertKeysDenialOutcome(t, shape.name+"/cross-project", rec, http.StatusUnprocessableEntity, "cross_project_keys_unsupported")
				assertNoKeysSideEffects(t, storeSpy, events)
				if got := d.callCount(); got != 0 {
					t.Errorf("%s: dispatcher called %d times, want 0", shape.name, got)
				}
			})
		}
	})
}

// TestExecuteAgentKeys_AgentCallerPreBackfillParity names, explicitly, the
// pre-backfill legacy allowance every other ExecuteAgentKeys agent-caller
// test in this package relies on implicitly (none of them marks the edge
// backfill complete or seeds a delegation edge, so AK-20 and similar tests
// pass only because of this behaviour). The allowance lives in the shared
// evaluator (AuthzService's delegation ceiling, reached through Decide), not
// in anything keys-specific: every attach-gated agent action that calls
// authorizeAgentTargetAction gets the identical pre-backfill treatment (for
// example handleAgentAction's generic gate), so this is attach parity, not a
// keys-only bypass. Without a test that names it directly, a change to the
// backfill flag or to a fixture that starts seeding it would silently change
// what AK-20 and similar tests actually exercise.
func TestExecuteAgentKeys_AgentCallerPreBackfillParity(t *testing.T) {
	t.Run("no edge, pre-backfill: legacy allowance admits", func(t *testing.T) {
		for _, shape := range keysRouteShapes {
			t.Run(shape.name, func(t *testing.T) {
				f, d, _, _ := newExecuteAgentKeysFixture(t)
				// Deliberately no markEdgeBackfillComplete call: this is the
				// pre-backfill state every other agent-caller test in this
				// package already runs in.
				callerID := tid("execkeys-attach-prebackfill-noedge-" + shape.name)
				token := f.agentToken(t, callerID, f.projectA.ID, ScopeAgentLifecycle)

				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s: expected 200 under the pre-backfill legacy allowance, got %d: %s", shape.name, rec.Code, rec.Body.String())
				}
				if got := d.callCount(); got != 1 {
					t.Errorf("%s: dispatcher called %d times, want 1", shape.name, got)
				}
			})
		}
	})

	t.Run("delegator-lacks-attach edge, pre-backfill: still denied", func(t *testing.T) {
		// A seeded edge is evaluated on its own merits even before the
		// backfill: the legacy allowance only ever covers the "no edge at
		// all" case above, never a live edge whose delegator lacks attach.
		for _, shape := range keysRouteShapes {
			t.Run(shape.name, func(t *testing.T) {
				f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
				callerID := tid("execkeys-attach-prebackfill-badedge-" + shape.name)
				addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.nonOwner.ID, callerID, f.projectA.ID)
				token := f.agentToken(t, callerID, f.projectA.ID, ScopeAgentLifecycle)
				log := installSentinelLogCapture(t)

				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
				assertKeysDenialOutcome(t, shape.name+"/pre-backfill delegator lacks attach", rec, http.StatusForbidden, "keys_denied")
				assertKeysDenialOutcomeAuditMatches(t, shape.name+"/pre-backfill delegator lacks attach", rec, log)
				assertNoKeysSideEffects(t, storeSpy, events)
				if got := d.callCount(); got != 0 {
					t.Errorf("%s: dispatcher called %d times, want 0", shape.name, got)
				}
			})
		}
	})
}
