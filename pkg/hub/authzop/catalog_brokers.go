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
			// The projects a broker serves, filtered to the projects the
			// caller can read.
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/projects", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "broker.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestBearerDispositionMatrix_CatalogEntryPoints"},
			{Package: "pkg/hub", Function: "TestHubTokenBrokerOwner_ReadsOwnBrokerThroughOwnerGrant"},
		},
		Bearer: AdmitOn(BearerTargetHubInstance, BearerBoundaryHub),
	},
	{
		ID:          "broker.material.read",
		Domain:      "broker",
		Description: "Read a runtime broker's environment variables (sensitive values masked) and secret metadata. A user needs broker.read on the broker and, beyond it, the owner relationship or broker.update; the broker reads its own material",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/env", Method: "GET", Response: ResponseCredential},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/env/{key}", Method: "GET", Response: ResponseCredential},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/secrets", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/secrets/{key}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalBroker},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialBrokerToken},
		ResourceResolver: "hub-scoped",
		BasePermission:   "broker.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestBearerDispositionMatrix_CatalogEntryPoints"},
			{Package: "pkg/hub", Function: "TestBrokerMaterial_ReadsRequireBrokerRead"},
		},
		Bearer: AdmitOn(BearerTargetHubInstance, BearerBoundaryHub),
	},
	{
		ID:          "broker.material.write",
		Domain:      "broker",
		Description: "Set, patch or delete a runtime broker's environment variables and secrets. A user needs an interactive session and broker.update on the broker; the broker writes its own material",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/env/{key}", Method: "PUT", Response: ResponseCredential},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/env/{key}", Method: "DELETE"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/secrets/{key}", Method: "PUT"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/secrets/{key}", Method: "PATCH"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/secrets/{key}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalBroker},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialBrokerToken},
		ResourceResolver: "hub-scoped",
		BasePermission:   "broker.update",
		Effects:          []SecurityEffect{EffectCreateResource, EffectUpdateResource, EffectDeleteResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "broker.material.write",
			ContextFields: []string{"actor_id", "broker_id"},
			BeforeFields:  []string{"key"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestBearerDispositionMatrix_CatalogEntryPoints"},
			{Package: "pkg/hub", Function: "TestBrokerMaterial_WritesSessionOnly"},
		},
		Bearer: SessionOnly(ReasonGovernancePending),
	},
	{
		ID:          "broker.update",
		Domain:      "broker",
		Description: "Update a runtime broker's name, labels or host service account. A user needs an interactive session and broker.update on the broker",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}", Method: "PATCH"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "broker.update",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestBearerDispositionMatrix_CatalogEntryPoints"},
		},
		Bearer: SessionOnly(ReasonGovernancePending),
	},
	{
		ID:          "broker.delete",
		Domain:      "broker",
		Description: "Deregister a runtime broker: unlink it from every project, clear project defaults that name it, and remove its credentials. A user needs an interactive session and broker.delete on the broker",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "broker.delete",
		Effects:          []SecurityEffect{EffectDeleteResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "broker.deregister",
			ContextFields: []string{"actor_id", "broker_id"},
			BeforeFields:  []string{"broker_name"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestBearerDispositionMatrix_CatalogEntryPoints"},
		},
		Bearer: SessionOnly(ReasonGovernancePending),
	},
	{
		ID:          "broker.heartbeat",
		Domain:      "broker",
		Description: "Record a broker heartbeat: broker liveness and the state of the agents it runs. The broker sends its own; a user needs an interactive session and broker.update on the broker from a role binding (the owner relationship does not apply)",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/heartbeat", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalBroker, PrincipalUser},
		Credentials:      []CredentialKind{CredentialBrokerToken, CredentialSessionJWT},
		ResourceResolver: "broker-self",
		BasePermission:   "broker.update",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestBearerDispositionMatrix_CatalogEntryPoints"},
			{Package: "pkg/hub", Function: "TestBrokerHeartbeat_UserPathSessionOnly"},
		},
		Bearer: SessionOnly(ReasonHostOperations),
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
		ID:          "broker.agent.workspacedownload",
		Domain:      "broker",
		Description: "Issue signed download URLs for the uploaded workspace files of an agent the broker runs",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/agents/{agentId}/workspace-download", Method: "POST", Response: ResponseCredential},
		},
		Principals:       []PrincipalKind{PrincipalBroker},
		Credentials:      []CredentialKind{CredentialBrokerToken},
		ResourceResolver: "broker-self",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub", Function: "TestBearerDisposition_EveryRoutePatternCovered"}},
		Exemptions: []Exemption{{
			Kind:   ExemptionInternalOnly,
			Reason: "The broker authenticates with its own HMAC credential for an agent assigned to it in a project it still provides for; no user permission applies",
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
		ID:          "broker.attachment.redeem",
		Domain:      "broker",
		Description: "Redeem an attachment delivery capability (ptone/scion#4596): the broker the hub issued it to downloads one attachment, once, before it expires, to write it into the recipient agent's home",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/attachment-deliveries/{token}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalBroker},
		Credentials:      []CredentialKind{CredentialBrokerToken},
		ResourceResolver: "broker-self",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialResourceNotFound},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestDeliveryRedemption_WrongBrokerRefused"},
			{Package: "pkg/hub", Function: "TestDeliveryRedemption_UniformRefusal"},
		},
		Exemptions: []Exemption{{
			Kind:   ExemptionInternalOnly,
			Reason: "The broker authenticates with its own HMAC credential and presents a capability the hub signed for that broker and recipient under the attachment read decision; no user permission applies",
			Scope:  "broker capability redemption",
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
