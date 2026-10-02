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

// Tests for task 2.3 (ptone/scion#2197): the temporary message-raw bridge
// (agent_keys_message_bridge.go). Reuses the fixtures from
// authorize_agentkeys_route_test.go / execute_agent_keys_test.go
// (newAgentKeysRouteFixture, newExecuteAgentKeysFixture, keysRouteShapes,
// doRequestWithAgentToken, doRequestAsUser, assertSameOutcomeModuloOperationID,
// assertNoKeysSideEffects) so the bridge is proven against the identical
// fixtures and assertions the direct /keys routes already use -- not a
// parallel, potentially-diverging test harness.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// messageRouteShape mirrors keysRouteShape (authorize_agentkeys_route_test.go)
// for the legacy /message endpoint the bridge intercepts.
type messageRouteShape struct {
	name string
	path func(agent *store.Agent) string
}

var messageRouteShapes = []messageRouteShape{
	{name: "top-level", path: func(agent *store.Agent) string { return "/api/v1/agents/" + agent.ID + "/message" }},
	{name: "project-scoped", path: func(agent *store.Agent) string {
		return "/api/v1/projects/" + agent.ProjectID + "/agents/" + agent.Slug + "/message"
	}},
}

// rawTopLevelBody builds a legacy top-level-only raw request:
// {"raw":true,"message":<msg>} -- cmd/message.go's plain-message shape
// (no structured_message at all).
func rawTopLevelBody(msg string) map[string]interface{} {
	return map[string]interface{}{"raw": true, "message": msg}
}

// rawNestedBody builds a legacy nested-structured-message raw request
// addressed at recipient (a bare value to be joined onto "agent:" --
// pass "" to omit the recipient field entirely).
func rawNestedBody(msg, recipient string) map[string]interface{} {
	sm := map[string]interface{}{"raw": true, "msg": msg, "type": "instruction"}
	if recipient != "" {
		sm["recipient"] = recipient
	}
	return map[string]interface{}{"structured_message": sm}
}

// TestAgentKeysMessageBridge_ParityWithDirectKeys drives the identical
// caller/target pair through both a direct /keys call and the equivalent
// raw /message call, on both route shapes, for three decisive outcomes
// (success, keys_denied, cross_project_keys_unsupported) and asserts they
// produce the same status/code/message (operation IDs necessarily differ
// per request) -- the binding "identical auth, quota, dispatch and
// content-free audit outcomes" obligation (issue #2197), AK-24/AK-25.
func TestAgentKeysMessageBridge_ParityWithDirectKeys(t *testing.T) {
	for _, shape := range keysRouteShapes {
		msgShape := messageRouteShapes[indexOfShape(shape.name)]

		t.Run(shape.name+"/success", func(t *testing.T) {
			f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
			token := f.agentToken(t, tid("bridge-parity-ok-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)

			keysRec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
			require.Equal(t, http.StatusOK, keysRec.Code, "direct /keys: %s", keysRec.Body.String())
			require.Equal(t, 1, d.callCount())

			msgRec := doRequestWithAgentToken(t, f.srv, http.MethodPost, msgShape.path(f.agentInA), rawTopLevelBody("C-c"), token)
			require.Equal(t, http.StatusOK, msgRec.Code, "raw bridge: %s", msgRec.Body.String())
			require.Equal(t, 2, d.callCount(), "bridge must dispatch through the identical ExecuteAgentKeys path")

			var keysResp, bridgeResp agentkeys.Response
			require.NoError(t, json.Unmarshal(keysRec.Body.Bytes(), &keysResp))
			require.NoError(t, json.Unmarshal(msgRec.Body.Bytes(), &bridgeResp))
			require.Equal(t, keysResp.Status, bridgeResp.Status)
			require.Equal(t, keysResp.AgentID, bridgeResp.AgentID)
			require.NotEmpty(t, bridgeResp.OperationID)

			assertNoKeysSideEffects(t, storeSpy, events)
		})

		t.Run(shape.name+"/keys_denied (no lifecycle scope)", func(t *testing.T) {
			f := newAgentKeysRouteFixture(t)
			token := f.agentToken(t, tid("bridge-parity-denied-"+shape.name), f.projectA.ID)

			keysRec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
			msgRec := doRequestWithAgentToken(t, f.srv, http.MethodPost, msgShape.path(f.agentInA), rawTopLevelBody("C-c"), token)

			assertKeysDenialOutcome(t, shape.name+"/keys direct", keysRec, http.StatusForbidden, "keys_denied")
			assertKeysDenialOutcome(t, shape.name+"/bridge", msgRec, http.StatusForbidden, "keys_denied")
			assertSameOutcomeModuloOperationID(t, shape.name+"/denied parity", keysRec, msgRec)
		})

		t.Run(shape.name+"/cross_project_keys_unsupported", func(t *testing.T) {
			f := newAgentKeysRouteFixture(t)
			token := f.agentToken(t, tid("bridge-parity-xproj-"+shape.name), f.projectB.ID, ScopeAgentLifecycle)

			keysRec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
			msgRec := doRequestWithAgentToken(t, f.srv, http.MethodPost, msgShape.path(f.agentInA), rawTopLevelBody("C-c"), token)

			assertKeysDenialOutcome(t, shape.name+"/keys direct", keysRec, http.StatusUnprocessableEntity, "cross_project_keys_unsupported")
			assertKeysDenialOutcome(t, shape.name+"/bridge", msgRec, http.StatusUnprocessableEntity, "cross_project_keys_unsupported")
			assertSameOutcomeModuloOperationID(t, shape.name+"/cross-project parity", keysRec, msgRec)
		})

		t.Run(shape.name+"/success (user)", func(t *testing.T) {
			f, d, storeSpy, events := newExecuteAgentKeysFixture(t)

			keysRec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, shape.path(f.agentInA), validKeysBody)
			require.Equal(t, http.StatusOK, keysRec.Code, "direct /keys: %s", keysRec.Body.String())
			require.Equal(t, 1, d.callCount())

			msgRec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, msgShape.path(f.agentInA), rawTopLevelBody("C-c"))
			require.Equal(t, http.StatusOK, msgRec.Code, "raw bridge: %s", msgRec.Body.String())
			require.Equal(t, 2, d.callCount())

			assertNoKeysSideEffects(t, storeSpy, events)
		})

		t.Run(shape.name+"/keys_denied (user, message-only)", func(t *testing.T) {
			f := newAgentKeysRouteFixture(t)
			ctx := context.Background()
			open := *f.agentInA
			open.MessageMode = store.MessageModeHub
			require.NoError(t, f.store.UpdateAgent(ctx, &open))
			user := newMessageOnlyUser(t, f, &open)

			keysRec := doRequestAsUser(t, f.srv, user, http.MethodPost, shape.path(f.agentInA), validKeysBody)
			msgRec := doRequestAsUser(t, f.srv, user, http.MethodPost, msgShape.path(f.agentInA), rawTopLevelBody("C-c"))

			assertKeysDenialOutcome(t, shape.name+"/keys direct (user)", keysRec, http.StatusForbidden, "keys_denied")
			assertKeysDenialOutcome(t, shape.name+"/bridge (user)", msgRec, http.StatusForbidden, "keys_denied")
			assertSameOutcomeModuloOperationID(t, shape.name+"/denied parity (user)", keysRec, msgRec)
		})
	}
}

// TestAgentKeysMessageBridge_ShapeDependentCrossProjectOrdering pins review
// r1 finding 7 (lead ruling, ptone/scion#2469): the bridge matches direct
// /keys on the SAME route shape, even though the two shapes do not decide a
// missing-scope, cross-project agent caller the same way. The divergence
// comes from the already-approved 2.1/2.2 ordering -- top-level /keys
// checks ScopeAgentLifecycle before project equality inside
// authorizeAgentKeys itself (authorize_agentkeys.go), while the
// project-scoped /keys route decides the cross-project refusal before any
// target-agent lookup, and so before any scope check, via its own
// pre-resolution seam (contract §3.1 invariant 4, AK-21c) -- task 2.3 must
// mirror that difference, not collapse it (see
// agentKeysBridgeCrossProjectDecision).
func TestAgentKeysMessageBridge_ShapeDependentCrossProjectOrdering(t *testing.T) {
	for _, shape := range keysRouteShapes {
		msgShape := messageRouteShapes[indexOfShape(shape.name)]
		t.Run(shape.name, func(t *testing.T) {
			f := newAgentKeysRouteFixture(t)
			// Cross-project, and deliberately missing ScopeAgentLifecycle.
			token := f.agentToken(t, tid("bridge-shape-xproj-noscope-"+shape.name), f.projectB.ID)

			keysRec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
			msgRec := doRequestWithAgentToken(t, f.srv, http.MethodPost, msgShape.path(f.agentInA), rawTopLevelBody("C-c"), token)

			keysEnv := decodeKeysError(t, keysRec.Body.Bytes())
			bridgeEnv := decodeKeysError(t, msgRec.Body.Bytes())
			require.Equal(t, keysRec.Code, msgRec.Code, "bridge must match direct /keys's status on the SAME shape")
			require.Equal(t, keysEnv.Code, bridgeEnv.Code, "bridge must match direct /keys's code on the SAME shape")

			switch shape.name {
			case "top-level":
				require.Equal(t, "keys_denied", keysEnv.Code, "top-level /keys checks scope before project")
			case "project-scoped":
				require.Equal(t, "cross_project_keys_unsupported", keysEnv.Code, "project-scoped /keys decides cross-project before any scope check")
			default:
				t.Fatalf("unhandled shape %q", shape.name)
			}
		})
	}
}

// TestAgentKeysMessageBridge_BudgetParityWithDirectKeys pins AC2's "quota"
// clause directly: the bridge and direct /keys draw from the same
// principal+project and target token buckets, in either direction.
func TestAgentKeysMessageBridge_BudgetParityWithDirectKeys(t *testing.T) {
	t.Run("exhausted via /keys blocks the bridge", func(t *testing.T) {
		f, d, _, _ := newExecuteAgentKeysFixture(t)
		freezeKeysRateLimiters(f)
		token := f.agentToken(t, tid("bridge-budget-via-keys"), f.projectA.ID, ScopeAgentLifecycle)

		for i := 0; i < agentkeys.PrincipalProjectBurst; i++ {
			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody, token)
			require.Equal(t, http.StatusOK, rec.Code, "burst call %d: %s", i, rec.Body.String())
		}
		require.Equal(t, agentkeys.PrincipalProjectBurst, d.callCount())

		rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/message", rawTopLevelBody("C-c"), token)
		assertKeysDenialOutcome(t, "bridge after /keys burst", rec, http.StatusTooManyRequests, "keys_rate_limited")
		require.Equal(t, agentkeys.PrincipalProjectBurst, d.callCount(), "the rate-limited bridge call must never dispatch")
	})

	t.Run("exhausted via the bridge blocks /keys", func(t *testing.T) {
		f, d, _, _ := newExecuteAgentKeysFixture(t)
		freezeKeysRateLimiters(f)
		token := f.agentToken(t, tid("bridge-budget-via-bridge"), f.projectA.ID, ScopeAgentLifecycle)

		for i := 0; i < agentkeys.PrincipalProjectBurst; i++ {
			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/message", rawTopLevelBody("C-c"), token)
			require.Equal(t, http.StatusOK, rec.Code, "burst call %d: %s", i, rec.Body.String())
		}
		require.Equal(t, agentkeys.PrincipalProjectBurst, d.callCount())

		rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody, token)
		assertKeysDenialOutcome(t, "/keys after bridge burst", rec, http.StatusTooManyRequests, "keys_rate_limited")
		require.Equal(t, agentkeys.PrincipalProjectBurst, d.callCount(), "the rate-limited /keys call must never dispatch")
	})
}

// TestAgentKeysMessageBridge_AuditFieldParity compares the full outcome
// audit record (not just status/code) between a direct /keys success and
// the equivalent bridge success, for the same caller/target: every field
// must match except route and operation_id (contract §5).
func TestAgentKeysMessageBridge_AuditFieldParity(t *testing.T) {
	for _, shape := range keysRouteShapes {
		msgShape := messageRouteShapes[indexOfShape(shape.name)]
		t.Run(shape.name, func(t *testing.T) {
			f, _, _, _ := newExecuteAgentKeysFixture(t)
			token := f.agentToken(t, tid("bridge-audit-parity-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)

			keysLog := installSentinelLogCapture(t)
			keysRec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
			require.Equal(t, http.StatusOK, keysRec.Code, "body: %s", keysRec.Body.String())
			keysOutcome := lastOutcomeAuditRecord(t, keysLog)

			bridgeLog := installSentinelLogCapture(t)
			msgRec := doRequestWithAgentToken(t, f.srv, http.MethodPost, msgShape.path(f.agentInA), rawTopLevelBody("C-c"), token)
			require.Equal(t, http.StatusOK, msgRec.Code, "body: %s", msgRec.Body.String())
			bridgeOutcome := lastOutcomeAuditRecord(t, bridgeLog)

			for _, field := range []string{
				"actor_type", "actor_id", "source_project_id", "target_agent_id",
				"target_project_id", "credential_kind", "credential_id", "decision", "input_bytes",
			} {
				require.Equal(t, keysOutcome[field], bridgeOutcome[field], "field %q must match between /keys and the bridge", field)
			}
			require.Equal(t, "keys", keysOutcome["route"])
			require.Equal(t, "message_raw_bridge", bridgeOutcome["route"])
			require.NotEqual(t, keysOutcome["operation_id"], bridgeOutcome["operation_id"], "each request mints its own operation ID")
		})
	}
}

// TestAgentKeysMessageBridge_ContentFreeOnEveryOutcome proves a distinctive
// keys-content sentinel never appears in the HTTP response body or captured
// logs, across a success and every rejection class the bridge produces
// (validation/malformed, unsupported-combination, and authorization
// denial) -- this also proves "count bridge usage without payload content"
// (issue #2197 scope), since the only way to count is through these same
// audit records.
func TestAgentKeysMessageBridge_ContentFreeOnEveryOutcome(t *testing.T) {
	cases := []struct {
		name       string
		body       map[string]interface{}
		scopes     []AgentTokenScope
		wantStatus int
	}{
		{"success", rawTopLevelBody(keysContentSentinel), []AgentTokenScope{ScopeAgentLifecycle}, http.StatusOK},
		{"keys_denied", rawTopLevelBody(keysContentSentinel), nil, http.StatusForbidden},
		{"raw_combination_unsupported", map[string]interface{}{"raw": true, "message": keysContentSentinel, "wake": true}, []AgentTokenScope{ScopeAgentLifecycle}, http.StatusUnprocessableEntity},
		{"invalid_request (raw+plain)", map[string]interface{}{"raw": true, "plain": true, "message": keysContentSentinel}, []AgentTokenScope{ScopeAgentLifecycle}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _, _, _ := newExecuteAgentKeysFixture(t)
			token := f.agentToken(t, tid("bridge-contentfree-"+sanitizeTestName(tc.name)), f.projectA.ID, tc.scopes...)
			log := installSentinelLogCapture(t)

			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/message", tc.body, token)
			require.Equal(t, tc.wantStatus, rec.Code, "body: %s", rec.Body.String())

			require.NotContains(t, rec.Body.String(), keysContentSentinel, "response body must never contain key content")
			require.NotContains(t, log.String(), keysContentSentinel, "captured logs must never contain key content")
		})
	}
}

func indexOfShape(name string) int {
	for i, s := range messageRouteShapes {
		if s.name == name {
			return i
		}
	}
	panic("no message route shape named " + name)
}

// TestAgentKeysMessageBridge_ClosedModeAttachVsMessageOnly pins AK-23/24/25:
// message mode never gates keys. A target sealed to MessageModeNone still
// accepts raw delivery through the bridge from an attach-authorized caller,
// and still denies a message-only caller (one with no ScopeAgentLifecycle,
// hence no keys authority) exactly as a direct /keys call would.
func TestAgentKeysMessageBridge_ClosedModeAttachVsMessageOnly(t *testing.T) {
	for _, shape := range messageRouteShapes {
		t.Run(shape.name, func(t *testing.T) {
			f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
			ctx := context.Background()

			sealed := *f.agentInA
			sealed.MessageMode = store.MessageModeNone
			require.NoError(t, f.store.UpdateAgent(ctx, &sealed))

			t.Run("attach-authorized caller can use the bridge", func(t *testing.T) {
				token := f.agentToken(t, tid("bridge-closed-attach-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)
				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(&sealed), rawTopLevelBody("C-c"), token)
				require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
				require.Equal(t, 1, d.callCount())
				assertNoKeysSideEffects(t, storeSpy, events)
			})

			t.Run("message-only caller (no lifecycle scope) is denied", func(t *testing.T) {
				token := f.agentToken(t, tid("bridge-closed-messageonly-"+shape.name), f.projectA.ID)
				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(&sealed), rawTopLevelBody("C-c"), token)
				assertKeysDenialOutcome(t, "message-only", rec, http.StatusForbidden, "keys_denied")
				require.Equal(t, 1, d.callCount(), "no additional dispatch for the denied caller")
			})

			t.Run("attach-authorized human caller (owner) can use the bridge", func(t *testing.T) {
				rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, shape.path(&sealed), rawTopLevelBody("C-c"))
				require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
				require.Equal(t, 2, d.callCount())
				assertNoKeysSideEffects(t, storeSpy, events)
			})
		})
	}
}

// newMessageOnlyUser creates a human user with agent.message authority on
// target (via a one-off role granting only that permission, reusing
// authorize_message_test.go's msgAuthzGrantAgentMessage) but not
// agent.attach -- the contract §6.3 "human with message authority but not
// ActionAttach" case. Verified directly against authzService.CheckAccess so
// this fixture cannot silently stop proving what its name promises. target
// must be in an open/hub message mode (MessageModeProject or
// MessageModeHub) for agent.message to actually be reachable through
// authorizeUserToAgent -- MessageModeNone denies every non-super-admin
// caller outright, which would make this fixture indistinguishable from
// any other denied caller.
func newMessageOnlyUser(t *testing.T, f *agentKeysRouteFixture, target *store.Agent) *store.User {
	t.Helper()
	ctx := context.Background()

	user := &store.User{
		ID: tid("bridge-message-only"), Email: "bridge-message-only@test.example",
		DisplayName: "Message Only", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, f.store.CreateUser(ctx, user))
	ensureHubMembership(ctx, f.store, user.ID)
	msgAuthzGrantAgentMessage(t, f.store, user.ID, target.ProjectID)

	identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "api")
	resource := agentResource(target)
	msgDecision := f.srv.authzService.CheckAccess(ctx, identity, resource, ActionMessage)
	require.True(t, msgDecision.Allowed, "fixture setup: expected agent.message to be granted: %s", msgDecision.Reason)
	attachDecision := f.srv.authzService.CheckAccess(ctx, identity, resource, ActionAttach)
	require.False(t, attachDecision.Allowed, "fixture setup: expected agent.attach to NOT be granted")
	return user
}

// TestAgentKeysMessageBridge_HumanMessageOnlyDenied pins the contract §6.3
// "Humans" paragraph directly: a human with message authority but not
// ActionAttach on the target is allowed to send an ordinary message
// (authorizeAgentMessage) but denied by the bridge (authorizeAgentKeys),
// exactly as a direct /keys call from the same human would be.
func TestAgentKeysMessageBridge_HumanMessageOnlyDenied(t *testing.T) {
	for _, shape := range messageRouteShapes {
		t.Run(shape.name, func(t *testing.T) {
			f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
			ctx := context.Background()
			open := *f.agentInA
			open.MessageMode = store.MessageModeHub
			require.NoError(t, f.store.UpdateAgent(ctx, &open))

			user := newMessageOnlyUser(t, f, &open)

			rec := doRequestAsUser(t, f.srv, user, http.MethodPost, shape.path(&open), rawTopLevelBody("C-c"))
			assertKeysDenialOutcome(t, shape.name, rec, http.StatusForbidden, "keys_denied")
			require.Equal(t, 0, d.callCount())
			assertNoKeysSideEffects(t, storeSpy, events)
		})
	}
}

// TestAgentKeysMessageBridge_RawSelectionORSemantics pins the OR-of-
// top-level-and-nested-raw rule (contract §8/§6.1): either spelling alone,
// or both together, selects the bridge; neither selects the unchanged
// message path (Plain/normal unaffected, AK-37).
func TestAgentKeysMessageBridge_RawSelectionORSemantics(t *testing.T) {
	cases := []struct {
		name string
		body map[string]interface{}
	}{
		{"top-level raw only", map[string]interface{}{"raw": true, "message": "C-c"}},
		{"nested raw only", map[string]interface{}{"structured_message": map[string]interface{}{"raw": true, "msg": "C-c", "type": "instruction"}}},
		{"both true", map[string]interface{}{"raw": true, "structured_message": map[string]interface{}{"raw": true, "msg": "C-c", "type": "instruction"}}},
	}
	for _, shape := range messageRouteShapes {
		t.Run(shape.name, func(t *testing.T) {
			f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
			token := f.agentToken(t, tid("bridge-or-semantics-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)

			for i, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), tc.body, token)
					require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
					require.Equal(t, i+1, d.callCount())
				})
			}
			assertNoKeysSideEffects(t, storeSpy, events)
		})
	}
}

// TestAgentKeysMessageBridge_PlainAndNormalUnaffected is the bridge's own
// regression proof (issue #2197: "No message-path behavior changes for
// normal or Plain requests"): a request with no raw flag anywhere still
// creates exactly one ordinary message row, never reaches the bridge.
func TestAgentKeysMessageBridge_PlainAndNormalUnaffected(t *testing.T) {
	// A human sender with project-owner piercing (authorizeUserToAgent),
	// not an agent credential: the agent-to-agent message path separately
	// requires the sender to have its own store.Agent row
	// (EvaluateAgentMessage's GetAgent lookup), which this fixture's
	// synthetic agentToken callers deliberately don't have (see
	// agentKeysRouteFixture.agentToken's doc comment) -- using the owner
	// isolates this regression test to exactly what it means to prove: the
	// bridge does not change ordinary/Plain message behavior, independent
	// of that unrelated agent-sender-record requirement.
	for _, shape := range messageRouteShapes {
		t.Run(shape.name, func(t *testing.T) {
			f := newAgentKeysRouteFixture(t)
			ctx := context.Background()
			f.srv.SetDispatcher(&brokerMockDispatcher{})

			for _, tc := range []struct {
				name string
				body map[string]interface{}
			}{
				{"normal message", map[string]interface{}{"message": "hello there"}},
				{"plain message", map[string]interface{}{"plain": true, "message": "hello there"}},
				{"structured plain", map[string]interface{}{"structured_message": map[string]interface{}{"plain": true, "msg": "hello there", "type": "instruction"}}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					before, err := f.store.ListMessages(ctx, store.MessageFilter{RecipientID: f.agentInA.ID}, store.ListOptions{Limit: 100})
					require.NoError(t, err)

					rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, shape.path(f.agentInA), tc.body)
					require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

					after, err := f.store.ListMessages(ctx, store.MessageFilter{RecipientID: f.agentInA.ID}, store.ListOptions{Limit: 100})
					require.NoError(t, err)
					require.Equal(t, len(before.Items)+1, len(after.Items), "exactly one ordinary message row must still be created")

					// Never the agentkeys envelope.
					var probe map[string]interface{}
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &probe))
					require.NotContains(t, probe, "operation_id", "an unaffected message response must not carry a keys operation_id")
				})
			}
		})
	}
}

// TestAgentKeysMessageBridge_PositiveControlWritesAndEventsOnMessageRoute is
// the write/event spy's positive control specifically for the two message
// routers this PR touches (review r1 finding 4): 2.2's own
// StoreSpyPositiveControl calls s.store directly, proving the spy wiring
// works but not that a regression in handleAgentAction/handleProjectAgentAction
// itself would be visible through it. An ordinary (non-raw) message sent
// through each router must register at least one write and one event.
func TestAgentKeysMessageBridge_PositiveControlWritesAndEventsOnMessageRoute(t *testing.T) {
	for _, shape := range messageRouteShapes {
		t.Run(shape.name, func(t *testing.T) {
			f, _, storeSpy, events := newExecuteAgentKeysFixture(t)
			rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, shape.path(f.agentInA), map[string]interface{}{"message": "hello there"})
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			require.Greater(t, storeSpy.totalWrites(), 0, "expected at least one messaging-model write for a non-raw message through this router")
			require.Greater(t, events.count(), 0, "expected at least one published event for a non-raw message through this router")
		})
	}
}

// TestAgentKeysMessageBridge_FieldTableRejections pins a representative
// sample of contract §6.1's "Rejected if set/true/non-empty" legacy field
// table rows (AK-43): each affects zero targets, produces
// raw_combination_unsupported with an operation ID, and never reaches
// messaging side effects.
func TestAgentKeysMessageBridge_FieldTableRejections(t *testing.T) {
	cases := []struct {
		name string
		body map[string]interface{}
	}{
		{"interrupt", map[string]interface{}{"raw": true, "message": "C-c", "interrupt": true}},
		{"notify", map[string]interface{}{"raw": true, "message": "C-c", "notify": true}},
		{"wake", map[string]interface{}{"raw": true, "message": "C-c", "wake": true}},
		{"mentions", map[string]interface{}{"raw": true, "message": "C-c", "mentions": []string{"agent:someone"}}},
		{"surface", map[string]interface{}{"raw": true, "message": "C-c", "surface": "native"}},
		{"external_ref", map[string]interface{}{"raw": true, "message": "C-c", "external_ref": "ext-1"}},
		{"parent_ref", map[string]interface{}{"raw": true, "message": "C-c", "parent_ref": "parent-1"}},
		{"recipients (group)", map[string]interface{}{"structured_message": map[string]interface{}{"raw": true, "msg": "C-c", "type": "instruction", "recipients": "a,b"}}},
		{"attachments", map[string]interface{}{"structured_message": map[string]interface{}{"raw": true, "msg": "C-c", "type": "instruction", "attachments": []string{"att-1"}}}},
		{"metadata", map[string]interface{}{"structured_message": map[string]interface{}{"raw": true, "msg": "C-c", "type": "instruction", "metadata": map[string]string{"k": "v"}}}},
		{"channel", map[string]interface{}{"structured_message": map[string]interface{}{"raw": true, "msg": "C-c", "type": "instruction", "channel": "general"}}},
		{"thread_id", map[string]interface{}{"structured_message": map[string]interface{}{"raw": true, "msg": "C-c", "type": "instruction", "thread_id": "t-1"}}},
		{"conversation_id", map[string]interface{}{"structured_message": map[string]interface{}{"raw": true, "msg": "C-c", "type": "instruction", "conversation_id": "c-1"}}},
		{"delivery_text", map[string]interface{}{"structured_message": map[string]interface{}{"raw": true, "msg": "C-c", "type": "instruction", "delivery_text": "d"}}},
		{"broadcasted", map[string]interface{}{"structured_message": map[string]interface{}{"raw": true, "msg": "C-c", "type": "instruction", "broadcasted": true}}},
		{"observer_only", map[string]interface{}{"structured_message": map[string]interface{}{"raw": true, "msg": "C-c", "type": "instruction", "observer_only": true}}},
		{"recipient mismatch", map[string]interface{}{"structured_message": map[string]interface{}{"raw": true, "msg": "C-c", "type": "instruction", "recipient": "agent:someone-else"}}},
		{"recipient_id mismatch", map[string]interface{}{"structured_message": map[string]interface{}{"raw": true, "msg": "C-c", "type": "instruction", "recipient_id": "not-the-target"}}},
		{"recipient is a user: form", map[string]interface{}{"structured_message": map[string]interface{}{"raw": true, "msg": "C-c", "type": "instruction", "recipient": "user:someone"}}},
		{"recipient is a group form", map[string]interface{}{"structured_message": map[string]interface{}{"raw": true, "msg": "C-c", "type": "instruction", "recipient": "group[agent:a,agent:b]"}}},
	}

	for _, shape := range messageRouteShapes {
		for _, tc := range cases {
			t.Run(shape.name+"/"+tc.name, func(t *testing.T) {
				f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
				token := f.agentToken(t, tid("bridge-field-"+shape.name+"-"+sanitizeTestName(tc.name)), f.projectA.ID, ScopeAgentLifecycle)

				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), tc.body, token)
				assertKeysDenialOutcome(t, shape.name+"/"+tc.name, rec, http.StatusUnprocessableEntity, "raw_combination_unsupported")
				require.Equal(t, 0, d.callCount(), "%s: must never dispatch", tc.name)
				assertNoKeysSideEffects(t, storeSpy, events)
			})
		}
	}
}

func sanitizeTestName(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		if r == ' ' || r == '(' || r == ')' || r == ',' {
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

// TestAgentKeysMessageBridge_RecipientMatchAllowed pins AK-48/AK-49, on both
// route shapes: a recipient value addressing the target by its raw UUID, by
// the route's own URL segment (the slug, on the project-scoped shape), or
// by a non-canonical display name that slugifies to the target's slug, must
// not be rejected.
func TestAgentKeysMessageBridge_RecipientMatchAllowed(t *testing.T) {
	// recipient is parameterized by shape name because "the URL segment" is
	// a different value per route shape (the canonical ID on top-level, the
	// slug on project-scoped, per messageRouteShapes) -- on the top-level
	// shape it is byte-identical to "matches resolved UUID", so keeping it
	// a plain, shape-blind function would make that sub-test a silent
	// duplicate there (review r2 finding 5). Making it shape-aware instead
	// keeps it a distinct, meaningful case on both shapes.
	cases := []struct {
		name      string
		recipient func(agent *store.Agent, shapeName string) string
	}{
		{"matches resolved UUID", func(a *store.Agent, _ string) string { return a.ID }},
		{"matches resolved slug", func(a *store.Agent, _ string) string { return "agent:" + a.Slug }},
		{"matches URL segment", func(a *store.Agent, shapeName string) string {
			if shapeName == "project-scoped" {
				return a.Slug
			}
			return a.ID
		}},
		{"empty (ignored)", func(a *store.Agent, _ string) string { return "" }},
		{"AK-49: non-canonical display name slugifying to the target's slug", func(a *store.Agent, _ string) string {
			return strings.ReplaceAll(a.Slug, "-", " ")
		}},
	}
	for _, shape := range messageRouteShapes {
		for _, tc := range cases {
			t.Run(shape.name+"/"+tc.name, func(t *testing.T) {
				f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
				token := f.agentToken(t, tid("bridge-recipient-ok-"+shape.name+"-"+sanitizeTestName(tc.name)), f.projectA.ID, ScopeAgentLifecycle)

				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA),
					rawNestedBody("C-c", tc.recipient(f.agentInA, shape.name)), token)
				require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
				require.Equal(t, 1, d.callCount())
				assertNoKeysSideEffects(t, storeSpy, events)
			})
		}
	}

	t.Run("project-scoped: matches the URL's slug segment specifically", func(t *testing.T) {
		f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
		token := f.agentToken(t, tid("bridge-recipient-ok-p-urlslug"), f.projectA.ID, ScopeAgentLifecycle)

		rec := doRequestWithAgentToken(t, f.srv, http.MethodPost,
			"/api/v1/projects/"+f.agentInA.ProjectID+"/agents/"+f.agentInA.Slug+"/message",
			rawNestedBody("C-c", f.agentInA.Slug), token)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		require.Equal(t, 1, d.callCount())
		assertNoKeysSideEffects(t, storeSpy, events)
	})
}

// assertKeysMalformedOutcome asserts a pre-operation-ID malformed-input
// rejection (contract §3 invariant 2/§2.5: OutcomeInvalidRequest and
// OutcomePayloadTooLarge are decided during validation itself, before an
// operation is recognized to exist, and so carry no operation_id at all --
// unlike every outcome assertKeysDenialOutcome checks, which are all
// post-validation).
func assertKeysMalformedOutcome(t *testing.T, label string, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("%s: status = %d, want %d: %s", label, rec.Code, wantStatus, rec.Body.String())
	}
	env := decodeKeysError(t, rec.Body.Bytes())
	if env.Code != wantCode {
		t.Errorf("%s: code = %q, want %q", label, env.Code, wantCode)
	}
	if _, ok := env.Details["operation_id"]; ok {
		t.Errorf("%s: expected no operation_id (malformed-input outcomes precede the mint), got %v", label, env.Details)
	}
}

// TestAgentKeysMessageBridge_RawPlainConflict pins AK-42: raw and plain
// together (either field, either location, including a mixed
// top-level/nested spelling) is 400 invalid_request, not 422.
func TestAgentKeysMessageBridge_RawPlainConflict(t *testing.T) {
	cases := []struct {
		name string
		body map[string]interface{}
	}{
		{"both top-level", map[string]interface{}{"raw": true, "plain": true, "message": "C-c"}},
		{"both nested", map[string]interface{}{"structured_message": map[string]interface{}{"raw": true, "plain": true, "msg": "C-c", "type": "instruction"}}},
		{"raw top-level, plain nested", map[string]interface{}{"raw": true, "structured_message": map[string]interface{}{"plain": true, "msg": "C-c", "type": "instruction"}}},
		{"raw nested, plain top-level", map[string]interface{}{"plain": true, "structured_message": map[string]interface{}{"raw": true, "msg": "C-c", "type": "instruction"}}},
	}
	for _, shape := range messageRouteShapes {
		for _, tc := range cases {
			t.Run(shape.name+"/"+tc.name, func(t *testing.T) {
				f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
				token := f.agentToken(t, tid("bridge-raw-plain-"+shape.name+"-"+sanitizeTestName(tc.name)), f.projectA.ID, ScopeAgentLifecycle)

				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), tc.body, token)
				assertKeysMalformedOutcome(t, shape.name+"/"+tc.name, rec, http.StatusBadRequest, "invalid_request")
				require.Equal(t, 0, d.callCount())
				assertNoKeysSideEffects(t, storeSpy, events)
			})
		}
	}
}

// TestAgentKeysMessageBridge_MessageMsgConflict pins AK-41: a nonempty
// top-level message and a disagreeing nested structured_message.msg is a
// 400, not a silent pick of one or the other. raw:true selects the bridge
// in the first place -- without it this body would simply be an ordinary
// (non-raw) message, which the bridge must leave alone entirely (AK-37).
func TestAgentKeysMessageBridge_MessageMsgConflict(t *testing.T) {
	body := map[string]interface{}{
		"raw":                true,
		"message":            "C-c",
		"structured_message": map[string]interface{}{"msg": "C-v", "type": "instruction"},
	}
	for _, shape := range messageRouteShapes {
		t.Run(shape.name, func(t *testing.T) {
			f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
			token := f.agentToken(t, tid("bridge-msg-conflict-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)

			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), body, token)
			assertKeysMalformedOutcome(t, "message vs structured_message.msg", rec, http.StatusBadRequest, "invalid_request")
			require.Equal(t, 0, d.callCount())
			assertNoKeysSideEffects(t, storeSpy, events)
		})
	}
}

// TestAgentKeysMessageBridge_NeitherCandidatePresent pins the bridge's
// "no keys content at all" case: raw selected via the nested flag, but
// neither message nor structured_message.msg is present.
func TestAgentKeysMessageBridge_NeitherCandidatePresent(t *testing.T) {
	f, d, _, _ := newExecuteAgentKeysFixture(t)
	token := f.agentToken(t, tid("bridge-no-content"), f.projectA.ID, ScopeAgentLifecycle)

	body := map[string]interface{}{"structured_message": map[string]interface{}{"raw": true, "type": "instruction"}}
	rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/message", body, token)
	assertKeysMalformedOutcome(t, "no content", rec, http.StatusBadRequest, "invalid_request")
	require.Equal(t, 0, d.callCount())
}

// TestAgentKeysMessageBridge_SizeBoundary pins AK-40: the extracted keys
// content is bound by agentkeys.MaxBytes (4096), not the legacy 16000-char/
// 64 KiB message ceiling -- the §6.2 "bridge size behaviour change".
func TestAgentKeysMessageBridge_SizeBoundary(t *testing.T) {
	atLimit := make([]byte, agentkeys.MaxBytes)
	for i := range atLimit {
		atLimit[i] = 'a'
	}
	overLimit := append(append([]byte{}, atLimit...), 'a')

	for _, shape := range messageRouteShapes {
		t.Run(shape.name, func(t *testing.T) {
			f, d, _, _ := newExecuteAgentKeysFixture(t)
			token := f.agentToken(t, tid("bridge-size-boundary-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)

			t.Run("at limit dispatches", func(t *testing.T) {
				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA),
					rawTopLevelBody(string(atLimit)), token)
				require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			})
			t.Run("over limit is 413", func(t *testing.T) {
				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA),
					rawTopLevelBody(string(overLimit)), token)
				assertKeysMalformedOutcome(t, "over limit", rec, http.StatusRequestEntityTooLarge, "payload_too_large")
			})
			require.Equal(t, 1, d.callCount(), "exactly the at-limit case dispatches")
		})
	}
}

// TestAgentKeysMessageBridge_PreAuthBodyCap pins AK-52/§6.4: a body (raw or
// not) exceeding the bridge's own 2 MiB pre-authorization read bound gets a
// generic, non-keys 413 with no operation_id -- distinct from
// agentkeys.OutcomePayloadTooLarge, which governs only the already-selected
// "keys" content (4096 bytes).
func TestAgentKeysMessageBridge_PreAuthBodyCap(t *testing.T) {
	oversized := make([]byte, agentKeysBridgePreAuthMaxBodyBytes+1)
	for i := range oversized {
		oversized[i] = 'x'
	}
	body := map[string]interface{}{"message": string(oversized)}

	for _, shape := range messageRouteShapes {
		t.Run(shape.name, func(t *testing.T) {
			f := newAgentKeysRouteFixture(t)
			token := f.agentToken(t, tid("bridge-precap-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)

			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), body, token)
			require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, "body: %s", rec.Body.String())
			env := decodeKeysError(t, rec.Body.Bytes())
			require.Equal(t, "payload_too_large", env.Code)
			require.NotContains(t, env.Details, "operation_id", "pre-classification 413 carries no operation ID")
		})
	}
}

// brokenChunkReadCloser delivers data in full (possibly across several Read
// calls, each returning whatever the destination buffer can hold) and then,
// together with its final chunk of data, returns a non-EOF error -- the
// shape io.ReadAll sees from a body whose underlying connection breaks
// immediately after the client has otherwise finished sending a complete
// payload (review r1 finding 1's reproduction).
type brokenChunkReadCloser struct {
	data []byte
	err  error
}

func (r *brokenChunkReadCloser) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

func (r *brokenChunkReadCloser) Close() error { return nil }

// TestAgentKeysMessageBridge_BrokenBodyReadFailsClosed pins review r1
// finding 1: a body read that errors (not via http.MaxBytesReader) after
// already delivering a complete, raw-selected JSON document must not let
// that content reach the legacy message path as if the read had succeeded
// -- the bridge must fail closed instead, with zero dispatch, zero writes
// and zero events, on both route shapes.
func TestAgentKeysMessageBridge_BrokenBodyReadFailsClosed(t *testing.T) {
	for _, shape := range messageRouteShapes {
		t.Run(shape.name, func(t *testing.T) {
			f, d, storeSpy, events := newExecuteAgentKeysFixture(t)

			// A human (owner) bearer-token caller, not an agent credential
			// (review r2 finding 4): authorizeAgentMessage would ALLOW this
			// caller via project-owner piercing, unlike the fixture's
			// synthetic agentToken callers (no store.Agent row, denied by
			// EvaluateAgentMessage's GetAgent lookup regardless of this
			// fix). Using the owner means a regression here would show up
			// as a real persisted message and a real dispatch, not merely
			// a status-code mismatch.
			userToken, _, _, err := f.srv.userTokenService.GenerateTokenPair(
				f.owner.ID, f.owner.Email, f.owner.DisplayName, f.owner.Role, ClientTypeWeb,
			)
			require.NoError(t, err)

			fullBody, err := json.Marshal(rawTopLevelBody("C-c"))
			require.NoError(t, err)

			req := httptest.NewRequest(http.MethodPost, shape.path(f.agentInA), nil)
			req.Body = &brokenChunkReadCloser{data: fullBody, err: errors.New("simulated broken chunk")}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+userToken)

			rec := httptest.NewRecorder()
			f.srv.Handler().ServeHTTP(rec, req)

			assertKeysMalformedOutcome(t, shape.name, rec, http.StatusBadRequest, "invalid_request")
			require.Equal(t, 0, d.callCount(), "a broken read must never reach dispatch")
			assertNoKeysSideEffects(t, storeSpy, events)

			// This rejection is a bridge response like any other raw-selected
			// outcome, so it must carry the same migration headers too.
			require.Equal(t, "true", rec.Header().Get("Deprecation"))
			require.NotEmpty(t, rec.Header().Get("Link"))
			require.Contains(t, rec.Header().Get("Link"), "/keys")
			require.NotEmpty(t, rec.Header().Get("X-Scion-Keys-Migration"))
		})
	}
}

// TestAgentKeysMessageBridge_MigrationHeaders pins the issue #2197 scope
// item "return keys results plus deprecation/migration guidance" and the
// binding brief obligation that the legacy-client retry limitation be
// stated explicitly.
func TestAgentKeysMessageBridge_MigrationHeaders(t *testing.T) {
	for _, shape := range messageRouteShapes {
		t.Run(shape.name, func(t *testing.T) {
			f, _, _, _ := newExecuteAgentKeysFixture(t)
			token := f.agentToken(t, tid("bridge-migration-headers-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)

			var wantPath string
			switch shape.name {
			case "top-level":
				wantPath = "/api/v1/agents/" + f.agentInA.ID + "/keys"
			case "project-scoped":
				wantPath = "/api/v1/projects/" + f.agentInA.ProjectID + "/agents/" + f.agentInA.ID + "/keys"
			default:
				t.Fatalf("unhandled shape %q", shape.name)
			}

			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), rawTopLevelBody("C-c"), token)
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			require.Equal(t, "true", rec.Header().Get("Deprecation"))
			require.Equal(t, "<"+wantPath+">; rel=\"successor-version\"", rec.Header().Get("Link"),
				"the Link header must name this exact route shape's own /keys path")
			guidance := rec.Header().Get("X-Scion-Keys-Migration")
			require.Contains(t, guidance, wantPath)
			require.Contains(t, guidance, "retr", "migration guidance must state the legacy-client retry limitation explicitly")
		})
	}
}

// TestAgentKeysMessageBridge_AuditRouteTag pins contract §5's "route (\"keys\"
// ... or \"message_raw_bridge\" for the temporary raw bridge)" requirement: a
// bridge-handled request's audit record is tagged distinctly from a direct
// /keys request's, even though decision/outcome are otherwise identical.
func TestAgentKeysMessageBridge_AuditRouteTag(t *testing.T) {
	for _, shape := range messageRouteShapes {
		t.Run(shape.name, func(t *testing.T) {
			f, _, _, _ := newExecuteAgentKeysFixture(t)
			token := f.agentToken(t, tid("bridge-audit-route-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)

			log := installSentinelLogCapture(t)
			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), rawTopLevelBody("C-c"), token)
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

			outcome := lastOutcomeAuditRecord(t, log)
			require.Equal(t, "message_raw_bridge", outcome["route"])
			require.Equal(t, "dispatched", outcome["decision"])
		})
	}
}

// TestAgentKeysMessageBridge_ZeroMessagingSideEffectsOnRejection proves the
// zero-write/zero-event invariant for a rejected bridge request too, not
// only for a successful dispatch (TestAgentKeysMessageBridge_ParityWithDirectKeys
// already covers success): every accepted or rejected raw request creates
// zero conversations/messages/attachments/events/observers/notifications
// (issue #2197 acceptance criteria).
func TestAgentKeysMessageBridge_ZeroMessagingSideEffectsOnRejection(t *testing.T) {
	for _, shape := range messageRouteShapes {
		t.Run(shape.name, func(t *testing.T) {
			f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
			token := f.agentToken(t, tid("bridge-zero-effects-denied-"+shape.name), f.projectA.ID)

			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), rawTopLevelBody("C-c"), token)
			assertKeysDenialOutcome(t, "denied", rec, http.StatusForbidden, "keys_denied")
			require.Equal(t, 0, d.callCount())
			assertNoKeysSideEffects(t, storeSpy, events)
		})
	}
}

// TestAgentKeysMessageBridge_NeverParsesAtTextForMentions pins "never
// parses literal @text for mentions" (issue #2197 acceptance criteria,
// AK-15): literal '@' content in the keys body must reach dispatch
// unchanged, not be rewritten or trigger mention fan-out.
func TestAgentKeysMessageBridge_NeverParsesAtTextForMentions(t *testing.T) {
	f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
	token := f.agentToken(t, tid("bridge-no-mention-parse"), f.projectA.ID, ScopeAgentLifecycle)

	const literal = "@builder do the thing"
	rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/message", rawTopLevelBody(literal), token)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, 1, d.callCount())
	require.Equal(t, literal, d.lastReq.keys, "literal @text must reach dispatch byte-for-byte")
	assertNoKeysSideEffects(t, storeSpy, events)
}

// TestAgentKeysMessageBridge_RunningPhaseAndManagedRuntimeDelegated proves
// the bridge delegates admission checks (running-phase, managed-runtime
// unsupported) to the identical ExecuteAgentKeys path, rather than
// reimplementing or skipping them.
func TestAgentKeysMessageBridge_RunningPhaseAndManagedRuntimeDelegated(t *testing.T) {
	f, d, _, _ := newExecuteAgentKeysFixture(t)
	ctx := context.Background()

	notRunning := *f.agentInA
	notRunning.Phase = string(state.PhaseStopped)
	require.NoError(t, f.store.UpdateAgent(ctx, &notRunning))

	token := f.agentToken(t, tid("bridge-not-running"), f.projectA.ID, ScopeAgentLifecycle)
	rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+notRunning.ID+"/message", rawTopLevelBody("C-c"), token)
	assertKeysDenialOutcome(t, "not running", rec, http.StatusConflict, "agent_not_running")
	require.Equal(t, 0, d.callCount())
}

// TestAgentKeysMessageBridge_MutationCheck_ReachesAndRespectsAuthorizeAgentKeys
// is a mutation-style negative control for the two key properties the
// branch-point invariant depends on, using a caller for which the claim is
// actually true (review r1 finding 6: the original version of this test
// used a caller authorizeAgentMessage would ALSO have denied, for an
// unrelated reason -- synthetic agentToken callers have no store.Agent row,
// so EvaluateAgentMessage's GetAgent lookup fails them too -- making the
// mutation claim false even though the assertion still happened to pass).
// newMessageOnlyUser's caller genuinely passes authorizeAgentMessage (open/
// hub mode, agent.message granted) and genuinely fails authorizeAgentKeys
// (no agent.attach): observing keys_denied with zero dispatch therefore
// proves both properties at once --
//
//   - the bridge reaches authorizeAgentKeys, not authorizeAgentMessage: if
//     the raw-selection check were deleted or inverted so this request fell
//     through instead, authorizeAgentMessage would allow it, producing a
//     persisted message (not keys_denied).
//   - the bridge respects (never bypasses) authorizeAgentKeys's decision:
//     if admitAndDispatchAgentKeys were ever called without checking that
//     decision first, this denied caller would dispatch (not zero calls).
func TestAgentKeysMessageBridge_MutationCheck_ReachesAndRespectsAuthorizeAgentKeys(t *testing.T) {
	f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
	ctx := context.Background()
	open := *f.agentInA
	open.MessageMode = store.MessageModeHub
	require.NoError(t, f.store.UpdateAgent(ctx, &open))

	user := newMessageOnlyUser(t, f, &open)

	rec := doRequestAsUser(t, f.srv, user, http.MethodPost, "/api/v1/agents/"+open.ID+"/message", rawTopLevelBody("C-c"))
	assertKeysDenialOutcome(t, "must reach and respect authorizeAgentKeys", rec, http.StatusForbidden, "keys_denied")
	require.Equal(t, 0, d.callCount())
	assertNoKeysSideEffects(t, storeSpy, events)
}

// TestAgentKeysMessageBridge_RawBodyNeverReachesHandleAgentMessage covers:
// handleAgentMessage's own internal raw guards
// (handlers_agent_messaging.go, now unreachable from the routers) are dead
// code reached only when a test calls srv.handleAgentMessage directly, as
// raw_guard_test.go's TestHandleAgentMessage_RawGuard_* tests do. This test
// instead drives a raw request through the real, full router
// (srv.Handler().ServeHTTP, the production entry point for both T and P)
// and proves the response is the bridge's, never handleAgentMessage's own:
// handleAgentMessage has no knowledge of the Deprecation/Link/
// X-Scion-Keys-Migration headers (grep confirms zero occurrences in
// handlers_agent_messaging.go) or of agentkeys.Response's shape, so their
// presence here is proof the request never fell through to it.
func TestAgentKeysMessageBridge_RawBodyNeverReachesHandleAgentMessage(t *testing.T) {
	for _, shape := range messageRouteShapes {
		t.Run(shape.name, func(t *testing.T) {
			f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
			token := f.agentToken(t, tid("router-dead-code-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)

			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), rawTopLevelBody("C-c"), token)
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			require.Equal(t, 1, d.callCount(), "must dispatch exactly once, through ExecuteAgentKeys")

			// handleAgentMessage never sets these; only the bridge does.
			require.Equal(t, "true", rec.Header().Get("Deprecation"))
			require.Contains(t, rec.Header().Get("Link"), "/keys")
			require.NotEmpty(t, rec.Header().Get("X-Scion-Keys-Migration"))

			var resp agentkeys.Response
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.NotEmpty(t, resp.OperationID, "body must be the keys/bridge response shape, not handleAgentMessage's own MessageDeliveryResponse")

			assertNoKeysSideEffects(t, storeSpy, events)
		})
	}
}
