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

// identityInventoryExpectation is the classified inventory of every non-test
// pkg/hub concrete type that implements Identity, keyed by the type's
// declared name. attested records AncestryIsHubAttested's planned outcome. A
// new identity type added to non-test pkg/hub source without a row here — or
// without a localAncestryProvenance() implementation matching its row —
// fails TestIdentityClassification_EveryTypeHasExplicitOutcome/SourceScan.
// That is the point: it forces classification to be a deliberate, explicitly
// classified edit. See the AST-scan limitations noted on scanIdentitySource
// below.
var identityInventoryExpectation = map[string]bool{
	"AuthenticatedUser":        true,
	"ScopedUserIdentity":       true,
	"DevUser":                  true,
	"agentIdentityWrapper":     true,
	"storedAgentIdentity":      true,
	"peerAgentIdentity":        true,
	"explainAgentIdentity":     true,
	"brokerIdentityImpl":       false,
	"FederatedUserIdentity":    false,
	"FederatedAgentIdentity":   false,
	"FederatedServiceIdentity": false,
	"hubDeliveryIdentity":      false,
}
