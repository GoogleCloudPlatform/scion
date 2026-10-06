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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// KeysAuthzDecision is the shared evaluation result authorizeAgentKeys
// produces (.design/agent-keys-contract.md §3, ptone/scion#2191 / task 2.1,
// ptone/scion#2195). ExecuteAgentKeys consumes this result directly rather
// than reimplementing the policy: this file evaluates the policy once and
// returns a decision, and the caller turns it into the keys response
// envelope.
//
// Allowed is true exactly when the caller may proceed to the next admission
// step (rate limiting, runtime/phase checks); it says nothing about whether
// dispatch will ultimately succeed. When Allowed is false, Outcome is always
// one of agentkeys.OutcomeKeysDenied or agentkeys.OutcomeCrossProjectKeysUnsupported
// — the only two outcomes this policy layer can produce — and is the zero
// value ("") when Allowed is true, since a caller of this function decides
// the eventual agentkeys.OutcomeDispatched (or a later failure outcome)
// after admission steps this function does not gate. Reason is a short,
// content-free string intended for audit logging (it is also passed to
// logAuthzDenial internally, so it is already in the structured "authz
// denial" log line by the time a caller sees it here). It is exported so
// 2.2/2.3 can fold it into their own content-free audit records without a
// second lookup, but it is policy-internal detail, not response content:
// callers MUST NOT copy it into an HTTP error body verbatim. Both 2.2 and
// 2.3's error envelopes must carry only the sanitized, fixed messages their
// own outcome tables define (contract §2.4a) — enforce this with a test at
// whichever call site renders the HTTP response, not by relying on this
// comment alone.
type KeysAuthzDecision struct {
	Allowed bool
	Outcome agentkeys.Outcome
	Reason  string
}

// authorizeAgentKeys is the single authorization gate for the agent-keys
// operation (contract §3 "Authorization"). The route action is not a new
// independently granted permission: every caller kind is evaluated by the
// shared relationship evaluator, authorizeAgentTargetAction with
// ActionAttach (agent.attach), the same rule every other attach-gated
// action on an existing agent uses (ptone/scion#3517). Results match the
// earlier attach-based mapping for every caller kind:
//
//   - Human session / user access token: agent.attach on the target through
//     Decide. Owner/privacy/cross-member restrictions, UAT scope and
//     project-boundary caveats (contract "User access token" row) and any
//     explicit deny all live there; this function special-cases none of
//     them. There is deliberately no ancestry/owner/super-admin shortcut of
//     the kind authorizeAgentMessage's user branch has (contract "no
//     self/parent/ancestor shortcut"; issue #2195 AC3).
//   - Agent credential: ScopeAgentLifecycle, the target's exact project,
//     and agent.attach on the target through Decide, including the
//     delegation ceiling of every live ancestor (ptone/scion#2460). The one
//     keys-specific mapping lives here, ahead of the shared evaluator: a
//     caller that holds the scope but is outside the target's project gets
//     agentkeys.OutcomeCrossProjectKeysUnsupported (422). A caller without
//     the scope gets keys_denied whatever its project, as before.
//   - Broker credential and every other principal kind (including a
//     cross-Hub federated agent identity): denied by the shared evaluator.
//     A broker credential authenticates Hub-to-broker execution under the
//     internal contract (§4), never direct public /keys authority.
//
// Every denial from the shared evaluator becomes the ordinary
// agentkeys.OutcomeKeysDenied (403) through denyAgentKeys, whatever status
// or detail the evaluator attached: never a new outcome, a different
// status, or any denial detail beyond the fixed message (contract §2.4a:
// KeysAuthzDecision.Reason is content-free, audit-only, and must never
// reach the HTTP response). There is no permissive fallback: an evaluator
// failure denies.
//
// Message modes (open/closed/none/etc.) are never read here: contract
// decision 5 and issue #2195 AC2 both require that message-plane
// configuration neither grants nor denies keys, so this function has no
// dependency on store.Agent.MessageMode at all.
//
// target must already be resolved by the caller (ExecuteAgentKeys, from
// handleAgentAction and handleProjectAgentAction). The project-scoped
// route's agent-credential cross-project refusal must be decided *before*
// that resolution happens at all (contract §3.1 invariant 4, AK-21c): see
// authorizeAgentKeysCrossProject below, which runs ahead of resolution.
func (s *Server) authorizeAgentKeys(r *http.Request, target *store.Agent) KeysAuthzDecision {
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		// Unreachable in production: the shared auth middleware answers an
		// unauthenticated request with 401 before any handler runs, so a nil
		// identity never reaches this function in practice (contract's
		// OperationID policy, §2.5). Fail closed anyway rather than panic,
		// mirroring authorizeAgentLifecycle's own defensive nil check.
		return s.denyAgentKeys(r, Resource{Type: "agent"}, "no authenticated identity")
	}
	if target == nil {
		return s.denyAgentKeys(r, Resource{Type: "agent"}, "nil target agent")
	}
	resource := agentResource(target)

	// The one keys-specific outcome: a scoped agent caller outside the
	// target's project gets cross_project_keys_unsupported instead of the
	// generic keys_denied the shared evaluator's denial maps to below.
	if keysAgentCrossProject(identity, target.ProjectID) {
		return s.denyAgentKeysCrossProject(r, resource, "agent project mismatch")
	}
	if denial := s.authorizeAgentTargetAction(r.Context(), identity, target, ActionAttach); denial != nil {
		return s.denyAgentKeys(r, resource, "attach denied: "+denial.reason)
	}
	return KeysAuthzDecision{Allowed: true, Reason: "attach granted"}
}

// keysAgentCrossProject reports whether identity is an agent credential
// holding ScopeAgentLifecycle whose project differs from targetProjectID.
// A caller without the scope is not reported here, so its denial stays the
// generic keys_denied from the shared evaluator.
func keysAgentCrossProject(identity Identity, targetProjectID string) bool {
	if identity.Type() != "agent" {
		return false
	}
	agentIdent, ok := identity.(AgentIdentity)
	if !ok || !agentIdent.HasScope(ScopeAgentLifecycle) {
		return false
	}
	return agentIdent.ProjectID() != targetProjectID
}

// authorizeAgentKeysCrossProject evaluates only the agent-credential
// cross-project refusal (contract §3.1), using the caller's identity and an
// already-resolved target project ID, without resolving or requiring any
// specific target agent record. It exists because the project-scoped route
// must decide this refusal *before* resolving the agent-by-slug-or-id target
// at all (contract §3.1 invariant 4, AK-21c: "no target-agent lookup of any
// kind, successful or not, may precede or be required by this comparison") —
// the full authorizeAgentKeys above cannot satisfy that ordering on its own
// because it requires an already-resolved *store.Agent.
//
// Returns nil when this function reaches no verdict: either the caller is
// not an authenticated agent identity (a human caller has no blanket
// cross-project ban — contract "Human cross-project use is allowed"), or the
// agent identity's own project already matches targetProjectID. nil does
// NOT mean "allowed": the caller must still resolve the target and call
// authorizeAgentKeys for the complete evaluation (scope, and — for the
// top-level route, which resolves its target first and so never calls this
// function at all — the equivalent project comparison folded into that
// single call).
//
// Returns a non-nil, always-denied decision (Outcome ==
// agentkeys.OutcomeCrossProjectKeysUnsupported) exactly when the caller is
// an authenticated agent identity whose own project differs from
// targetProjectID.
//
// Gated on identity.Type() == "agent", not merely on the identity
// implementing the AgentIdentity interface: FederatedAgentIdentity also
// implements AgentIdentity (Type() == "federated_agent", ProjectID() ==
// "") but is not one of the two caller kinds contract §3's table gives a
// project-boundary rule to. Gating on the interface alone would give a
// federated caller 422 cross_project_keys_unsupported here while the main
// authorizeAgentKeys gate's default branch denies that same caller with
// generic keys_denied on the top-level route (which never calls this
// pre-check and instead folds the equivalent comparison into one call) —
// two route shapes disagreeing on the outcome for an identical caller and
// target (AC4). Every principal kind other than "agent" returns nil here,
// so the caller falls through to the full authorizeAgentKeys gate, whose
// default branch is the single place that denies them.
func (s *Server) authorizeAgentKeysCrossProject(r *http.Request, targetProjectID string) *KeysAuthzDecision {
	identity := GetIdentityFromContext(r.Context())
	if identity == nil || identity.Type() != "agent" {
		return nil
	}
	agentIdent, ok := identity.(AgentIdentity)
	if !ok {
		return nil
	}
	if agentIdent.ProjectID() == targetProjectID {
		return nil
	}
	resource := Resource{Type: "agent", ParentType: "project", ParentID: targetProjectID}
	d := s.denyAgentKeysCrossProject(r, resource, "agent project mismatch")
	return &d
}

// denyAgentKeys logs the structured authorization-denial record every other
// denial path in this package produces (logAuthzDenial, #591) and returns
// the generic keys_denied decision. The "keys: " reason prefix (content-free
// — it tags the operation, not the request) lets an operator distinguish a
// keys denial from a PTY attach denial in shared audit logs, since both
// currently log under the same ActionAttach action.
func (s *Server) denyAgentKeys(r *http.Request, resource Resource, reason string) KeysAuthzDecision {
	reason = "keys: " + reason
	logAuthzDenial(r, GetIdentityFromContext(r.Context()), resource, ActionAttach, reason)
	return KeysAuthzDecision{Allowed: false, Outcome: agentkeys.OutcomeKeysDenied, Reason: reason}
}

// denyAgentKeysCrossProject is denyAgentKeys' sibling for the one denial
// reason that maps to a different wire outcome
// (agentkeys.OutcomeCrossProjectKeysUnsupported, 422) rather than the
// generic agentkeys.OutcomeKeysDenied (403).
func (s *Server) denyAgentKeysCrossProject(r *http.Request, resource Resource, reason string) KeysAuthzDecision {
	reason = "keys: " + reason
	logAuthzDenial(r, GetIdentityFromContext(r.Context()), resource, ActionAttach, reason)
	return KeysAuthzDecision{Allowed: false, Outcome: agentkeys.OutcomeCrossProjectKeysUnsupported, Reason: reason}
}
