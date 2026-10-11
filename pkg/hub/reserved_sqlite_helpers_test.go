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

// stripSiteKey identifies one call site by (file, function), matching
// effectCallSiteClassifications' own key shape.
type stripSiteKey struct{ file, function string }

// strippedSites lists every (file, function) that calls
// messaging.StripReservedMetadata (directly, or via ExecuteAgentDM/a shared
// helper) before rendering or dispatching to the site's recipient(s). This
// is the P1+P2 site list from design §4.2 item 1 and §10 P1/P2.
var strippedSites = map[stripSiteKey]bool{
	// P1: the two ExecuteAgentDM-routed entry points, covering the outbound
	// endpoint, the handleAgentMessage agent fork, and #2083's agent mention
	// fan-out (fanOutAgentMentions reaches dispatch through ExecuteAgentDM,
	// so it shares this AST entry rather than having its own).
	{"agent_dm_operation.go", "ExecuteAgentDM"}: true,
	// P1: the handleAgentMessage human/broker-sender branch.
	{"handlers_agent_messaging.go", "handleAgentMessage"}: true,

	// P2 (commit 2a): strip-only, no offload yet.
	{"handlers_agent_messaging.go", "handleGroupMessage"}:            true,
	{"handlers_agent_messaging.go", "broadcastDirect"}:               true,
	{"handlers_agent_messaging.go", "processMentions"}:               true,
	{"handlers_broker_inbound.go", "handleBrokerInbound"}:            true,
	{"handlers_broker_inbound_routed.go", "dispatchRoutedRecipient"}: true,
	// sendAgentRouted covers both the primary send and the mention
	// fan-out loop in the same function — both call StripReservedMetadata
	// explicitly (defence in depth for the mention copy, which already
	// aliases the primary's stripped map).
	{"handlers_chat_v2.go", "sendAgentRouted"}: true,
	{"messagebroker.go", "deliverToAgent"}:     true,
}
