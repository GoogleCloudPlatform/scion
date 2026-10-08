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

// ---------------------------------------------------------------------------
// U5 (ptone/scion#2257, design auto-offload-large-dm §4.2 item 4): every
// agent-recipient dispatch site must strip client- or plugin-supplied
// body_offloaded / body_chars / body_sha256 before render/dispatch, or have
// a written exemption reason. TestReservedKeyStripCoverage is the durable
// guard — it reads msg_containment_callsite_test.go's own AST-classified
// call sites (the authoritative site inventory) and requires every
// dispatchWithBrokerRetry/DispatchAgentMessage entry to be accounted for
// here, the same pattern TestExternalEffectCallSiteClassification already
// uses for authorization guarding.
// ---------------------------------------------------------------------------

import "testing"

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

// stripExemptions lists every (file, function) dispatch/render site that is
// intentionally NOT in strippedSites, with the reason it is safe: the site
// never forwards client- or plugin-supplied metadata at all.
var stripExemptions = map[stripSiteKey]string{
	{"broker_routing.go", "dispatchWithBrokerRetry"}:                  "primitive wrapper: the retry loop implementation itself, not a call site",
	{"handlers_agent_messaging.go", "publishBroadcastDeliveryFailed"}: "hub-built delivery-failure notice to the original sender; no client metadata",
	{"messagebroker.go", "publishDeliveryFailed"}:                     "hub-built delivery-failure notice to the original sender; no client metadata",
	{"messagebroker.go", "publishDeliveryDeferred"}:                   "hub-built delivery-deferred notice to the original sender; no client metadata",
	{"artifacts_review.go", "deliverArtifactReview"}:                  "hub-built review notice to the artifact's owner; messages.NewSystemMessage carries no client metadata",
	{"notifications.go", "dispatchToAgent"}:                           "hub-built notification fan-out; no client metadata",
	{"server.go", "messageEventHandler"}:                              "scheduled message; messages.NewSystemMessage carries no client metadata",
	{"reconcile.go", "deliverMessage"}:                                "dead code: assigned at server.go but never invoked in production (msg_containment_callsite_test.go)",
	{"notification_sweep.go", "RetryDispatch"}:                        "guarded retry of a previously authorized, hub-built notification dispatch; no client metadata",
}

// TestReservedKeyStripCoverage requires every dispatchWithBrokerRetry /
// DispatchAgentMessage call site classified in
// effectCallSiteClassifications (msg_containment_callsite_test.go) to be
// either a strippedSites entry or a stripExemptions entry with a reason. A
// new dispatch site fails this test until it is classified into one of the
// two, closing the same gap TestExternalEffectCallSiteClassification closes
// for authorization.
func TestReservedKeyStripCoverage(t *testing.T) {
	seen := make(map[stripSiteKey]bool)
	for _, e := range effectCallSiteClassifications {
		if e.symbol != "dispatchWithBrokerRetry" && e.symbol != "DispatchAgentMessage" {
			continue
		}
		key := stripSiteKey{e.file, e.function}
		seen[key] = true

		_, stripped := strippedSites[key]
		reason, exempt := stripExemptions[key]
		if !stripped && !exempt {
			t.Errorf("UNCLASSIFIED reserved-metadata strip site: %s:%s (symbol %s)\n"+
				"  Every dispatch site must be in strippedSites (it calls "+
				"messaging.StripReservedMetadata before render/dispatch) or in "+
				"stripExemptions with a reason.", e.file, e.function, e.symbol)
		}
		if stripped && exempt {
			t.Errorf("%s:%s is in both strippedSites and stripExemptions (reason %q) — pick one", e.file, e.function, reason)
		}
	}

	if len(seen) == 0 {
		t.Fatal("found zero dispatchWithBrokerRetry/DispatchAgentMessage entries in effectCallSiteClassifications — scanner or classification table is broken")
	}

	// Stale entries: every strippedSites/stripExemptions key must correspond
	// to a real, currently-classified site.
	for key := range strippedSites {
		if !seen[key] {
			t.Errorf("STALE strippedSites entry: %s:%s — no longer a dispatchWithBrokerRetry/DispatchAgentMessage site in effectCallSiteClassifications", key.file, key.function)
		}
	}
	for key := range stripExemptions {
		if !seen[key] {
			t.Errorf("STALE stripExemptions entry: %s:%s — no longer a dispatchWithBrokerRetry/DispatchAgentMessage site in effectCallSiteClassifications", key.file, key.function)
		}
	}
}
