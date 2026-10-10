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

package authzop

// brokerOperations lists the catalog operations for runtime brokers.
var brokerOperations = []OperationSpec{
	// =====================================================================
	// Domain: broker — runtime broker read
	// =====================================================================
	{
		ID:          "broker.read",
		Domain:      "broker",
		Description: "Read runtime broker status or list brokers",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}", Method: "GET"},
			// ptone/scion#2061 P2, ptone/scion#2177: per-broker settings GET
			// (design.md §5.4) is dispatched inside handleRuntimeBrokerRoutes,
			// not a separate mux pattern; broker.read gates the read.
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/settings", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "broker.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "broker.agent.launchreport",
		Domain:      "broker",
		Description: "Record a broker's launch report for an agent it runs",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/agents/{agentId}/launch", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalBroker},
		Credentials:      []CredentialKind{CredentialBrokerToken},
		ResourceResolver: "broker-self",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub", Function: "TestBearerDisposition_EveryRoutePatternCovered"}},
		Exemptions: []Exemption{{
			Kind:   ExemptionInternalOnly,
			Reason: "The broker authenticates with its own HMAC credential for its own record; no user permission applies",
			Scope:  "broker self access",
			Waives: []WaivedObligation{WaiveBasePermission},
		}},
		Bearer: NonUser(),
	},
	{
		ID:          "broker.messagefailures.report",
		Domain:      "broker",
		Description: "Record buffered message delivery failures reported by a broker",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/message-failures", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalBroker},
		Credentials:      []CredentialKind{CredentialBrokerToken},
		ResourceResolver: "broker-self",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub", Function: "TestBearerDisposition_EveryRoutePatternCovered"}},
		Exemptions: []Exemption{{
			Kind:   ExemptionInternalOnly,
			Reason: "The broker authenticates with its own HMAC credential for its own record; no user permission applies",
			Scope:  "broker self access",
			Waives: []WaivedObligation{WaiveBasePermission},
		}},
		Bearer: NonUser(),
	},
	{
		ID:          "broker.controlchannel.call",
		Domain:      "broker",
		Description: "Carry a call between the hub and a connected broker over the control channel; each call runs under the operation that initiated it",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointBrokerCall, Pattern: "TunnelRequest:controlchannel"},
			{Kind: EntryPointBrokerCall, Pattern: "OpenStream:controlchannel"},
			{Kind: EntryPointBrokerCall, Pattern: "SendStreamData:controlchannel"},
			{Kind: EntryPointBrokerCall, Pattern: "ResizeStream:controlchannel"},
			{Kind: EntryPointBrokerCall, Pattern: "CloseStream:controlchannel"},
			{Kind: EntryPointBrokerCall, Pattern: "handleResponse:controlchannel"},
			{Kind: EntryPointBrokerCall, Pattern: "handleStreamData:controlchannel"},
			{Kind: EntryPointBrokerCall, Pattern: "handleStreamClose:controlchannel"},
			{Kind: EntryPointBrokerCall, Pattern: "handleEvent:controlchannel"},
		},
		Principals:       []PrincipalKind{PrincipalBroker, PrincipalSystem},
		Credentials:      []CredentialKind{CredentialBrokerToken, CredentialSystemInternal},
		ResourceResolver: "initiating-operation",
		Effects:          []SecurityEffect{EffectReadOne, EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub", Function: "TestBearerDisposition_EveryRoutePatternCovered"}},
		Exemptions: []Exemption{{
			Kind:   ExemptionInternalOnly,
			Reason: "Transport between the hub and an authenticated broker; authorization belongs to the initiating operation",
			Scope:  "control channel transport",
			Waives: []WaivedObligation{WaiveBasePermission},
		}},
		Bearer: NonUser(),
	},
}
