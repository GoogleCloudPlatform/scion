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
	"go/ast"
)

// effectCallSiteClassifications is the authoritative classification map.
// Every production call site of the four external-effect primitives must
// appear here. "guarded" means messaging/dispatch authorization occurs
// before the call; "exempt" means the call is intentionally unguarded
// with a documented reason.
var effectCallSiteClassifications = []effectCallSiteEntry{
	// ---- dispatchWithBrokerRetry ----

	// broker_routing.go: the primitive itself — calls DispatchAgentMessage
	// in its retry loop. This is the wrapper, not a call site.
	{file: "broker_routing.go", function: "dispatchWithBrokerRetry", symbol: "DispatchAgentMessage",
		class: "exempt", reason: "primitive wrapper: the retry loop implementation itself"},

	// handlers_agent_messaging.go: handleAgentMessage — guarded by routers
	// at handlers_agents_core.go:2700 and handlers_projects_core.go:2529.
	{file: "handlers_agent_messaging.go", function: "handleAgentMessage", symbol: "dispatchWithBrokerRetry",
		class: "guarded", reason: "authorizeAgentMessage called in both routers before this handler"},

	// agent_dm_operation.go: ExecuteAgentDM — the shared agent DM operation
	// (#1688). Authorization is the first admission check inside the operation
	// (authorizeAgentMessage called before any side effects).
	{file: "agent_dm_operation.go", function: "ExecuteAgentDM", symbol: "dispatchWithBrokerRetry",
		class: "guarded", reason: "authorizeAgentMessage called inside ExecuteAgentDM before persistence/dispatch/observer (#1688)"},

	// handlers_agent_messaging.go: handleGroupMessage — guarded at :1286.
	{file: "handlers_agent_messaging.go", function: "handleGroupMessage", symbol: "dispatchWithBrokerRetry",
		class: "guarded", reason: "authorizeAgentMessage at handlers_agent_messaging.go:1286"},

	// handlers_agent_messaging.go: broadcastDirect — guarded per-recipient
	// at handlers_agent_messaging.go:1644.
	{file: "handlers_agent_messaging.go", function: "broadcastDirect", symbol: "dispatchWithBrokerRetry",
		class: "guarded", reason: "authorizeAgentMessage per-recipient at handlers_agent_messaging.go:1644"},

	// handlers_agent_messaging.go: publishBroadcastDeliveryFailed — derivative
	// notice back to the original sender agent.
	{file: "handlers_agent_messaging.go", function: "publishBroadcastDeliveryFailed", symbol: "DispatchAgentMessage",
		class: "exempt", reason: "derivative: delivery-failure notice to original sender, not attacker-chosen target"},

	// handlers_agent_messaging.go: processMentions — guarded at :1852.
	{file: "handlers_agent_messaging.go", function: "processMentions", symbol: "dispatchWithBrokerRetry",
		class: "guarded", reason: "authorizeAgentMessage at handlers_agent_messaging.go:1852"},

	// handlers_broker_inbound.go: guarded at :164 (authorizeAgentMessage).
	{file: "handlers_broker_inbound.go", function: "handleBrokerInbound", symbol: "dispatchWithBrokerRetry",
		class: "guarded", reason: "authorizeAgentMessage at handlers_broker_inbound.go:164"},

	// handlers_broker_inbound_routed.go: dispatchRoutedRecipient — guarded per-recipient.
	{file: "handlers_broker_inbound_routed.go", function: "dispatchRoutedRecipient", symbol: "dispatchWithBrokerRetry",
		class: "guarded", reason: "authorizeAgentMessage called per-recipient before dispatch"},

	// handlers_chat_v2.go: sendAgentRouted primary — guarded at :1125.
	{file: "handlers_chat_v2.go", function: "sendAgentRouted", symbol: "dispatchWithBrokerRetry",
		class: "guarded", reason: "authorizeAgentMessage at handlers_chat_v2.go:1125"},

	// messagebroker.go: deliverToAgent — UNGUARDED internal surface.
	// No authorizeAgentMessage call anywhere in this function.
	// Classified as exempt pending full RS6 remediation.
	{file: "messagebroker.go", function: "deliverToAgent", symbol: "dispatchWithBrokerRetry",
		class: "exempt", reason: "UNGUARDED internal surface (F-RS6-07); no external reach identified; deferred to RS6/AH"},

	// messagebroker.go: publishDeliveryFailed — derivative notice.
	{file: "messagebroker.go", function: "publishDeliveryFailed", symbol: "DispatchAgentMessage",
		class: "exempt", reason: "derivative: delivery-failure notice to original sender"},

	// messagebroker.go: publishDeliveryDeferred — derivative notice (design
	// agent-reincarnate §3.7, O2 p2a-r1 review). Same shape as
	// publishDeliveryFailed above: tells the original sender their message
	// was deferred, not dropped, while the recipient is mid-migration.
	{file: "messagebroker.go", function: "publishDeliveryDeferred", symbol: "DispatchAgentMessage",
		class: "exempt", reason: "derivative: delivery-deferred notice to original sender"},

	// artifacts_review.go: deliverArtifactReview — derivative notice
	// (ptone/scion#3229). Sent after the artifact service finalized a review
	// version for a request-authenticated reviewer who could write the
	// artifact; it goes to the artifact's owning agent, as "system", with
	// a body built from the reference alone.
	{file: "artifacts_review.go", function: "deliverArtifactReview", symbol: "dispatchWithBrokerRetry",
		class: "exempt", reason: "derivative: review notice to the artifact's owning agent after an authorized finalize"},

	// notifications.go: dispatchToAgent — UNGUARDED notification fan-out.
	// Subscription-only authorization; revocation not re-evaluated.
	{file: "notifications.go", function: "dispatchToAgent", symbol: "dispatchWithBrokerRetry",
		class: "exempt", reason: "UNGUARDED notification fan-out (F-RS6-08); subscription-only authz; deferred to RS6/AH"},

	// server.go: messageEventHandler — GUARDED after C1 containment.
	// authorizeScheduledMessageFire calls authorizeAgentMessage before dispatch.
	{file: "server.go", function: "messageEventHandler", symbol: "dispatchWithBrokerRetry",
		class: "guarded", reason: "C1 containment: authorizeScheduledMessageFire before dispatch"},

	// server.go: dispatchAgentEventHandler — guarded at server.go:3103
	// (authorizeScheduledAgentCreate).
	{file: "server.go", function: "dispatchAgentEventHandler", symbol: "DispatchAgentCreate",
		class: "guarded", reason: "authorizeScheduledAgentCreate at server.go fire-time authorization"},

	// reconcile.go: deliverMessage — dead code. The seam is assigned but
	// never invoked in production.
	{file: "reconcile.go", function: "deliverMessage", symbol: "DispatchAgentMessage",
		class: "exempt", reason: "dead code: seam assigned at server.go but never invoked (F-RS6-18)"},

	// reconcile.go: execDispatchStart — durable-intent replay. Replays
	// a previously authorized dispatch.
	{file: "reconcile.go", function: "execDispatchStart", symbol: "DispatchAgentStart",
		class: "exempt", reason: "durable-intent replay of previously authorized dispatch"},

	// reconcile.go: execDispatchCreate (via DispatchAgentCreateWithGather) —
	// durable-intent replay.
	{file: "reconcile.go", function: "execDispatchCreate", symbol: "DispatchAgentCreateWithGather",
		class: "exempt", reason: "durable-intent replay of previously authorized dispatch"},

	// start_claim.go: startAgentCore — the shared start path that runs a
	// start under its start claim. Its callers authorize before calling it:
	// handleAgentLifecycle and handleExistingAgent (guarded by their routers'
	// authz), and wakeAgentForDM (called after admission checks in
	// ExecuteAgentDM, #1691 AC-2, or guarded by the calling routers for
	// user→agent).
	{file: "start_claim.go", function: "startAgentCore", symbol: "DispatchAgentStart",
		class: "guarded", reason: "every caller authorizes before calling: lifecycle and create-on-existing routers' authz; wake after DM admission checks"},

	// reincarnate_worker.go: DispatchAgentStart in runReincarnationWorker —
	// the detached background worker for `scion reincarnate` (design §3.1).
	// Started only from handleReincarnateAgent, after authorizeAgentReincarnate
	// (design §3.8, decision D2) has already authorized the request; the
	// worker itself does not re-check authorization.
	{file: "reincarnate_worker.go", function: "runReincarnationWorker", symbol: "DispatchAgentStart",
		class: "guarded", reason: "authorizeAgentReincarnate in handleReincarnateAgent runs before the worker is started"},

	// handlers_agents_core.go: DispatchAgentCreateWithGather in createAgentInProject.
	{file: "handlers_agents_core.go", function: "createAgentInProject", symbol: "DispatchAgentCreateWithGather",
		class: "guarded", reason: "authorizeAgentCreate at handlers_agents_core.go"},

	// workspace_handlers.go: DispatchAgentCreate in handleWorkspaceSyncToFinalize.
	{file: "workspace_handlers.go", function: "handleWorkspaceSyncToFinalize", symbol: "DispatchAgentCreate",
		class: "guarded", reason: "workspace agent creation with project authorization"},

	// notification_sweep.go: RetryDispatch — guarded retry of previously
	// authorized notification dispatch.
	{file: "notification_sweep.go", function: "RetryDispatch", symbol: "dispatchWithBrokerRetry",
		class: "guarded", reason: "retry of previously authorized notification dispatch"},
}

type effectCallSite struct {
	file     string // base filename
	function string // enclosing function name
	symbol   string // called function/method name
	line     int
}

// extractCallSymbol returns the function/method name from a call expression.
func extractCallSymbol(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fn.Sel.Name
	case *ast.Ident:
		return fn.Name
	}
	return ""
}

// effectCallSiteEntry describes one classified external-effect call site.
type effectCallSiteEntry struct {
	file     string // base filename (e.g. "server.go")
	function string // enclosing function name
	symbol   string // called function/method name
	class    string // "guarded" or "exempt"
	reason   string // why this classification is correct
}
