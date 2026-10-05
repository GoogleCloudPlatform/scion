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

// messagingOperations lists the catalog operations for agent messaging.
var messagingOperations = []OperationSpec{
	// =====================================================================
	// Domain: agent.message — agent messaging (external effect)
	// =====================================================================
	{
		ID:          "agent.message.send",
		Domain:      "agent.message",
		Description: "Send a message to an agent",
		EntryPoints: []EntryPoint{
			// "/api/v1/chat/threads/{id}/messages" was never a registered
			// route; there is no live HTTP entry point for this operation
			// today, only the broker-call path below.
			{Kind: EntryPointBrokerCall, Pattern: "broker.inbound"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent, PrincipalBroker},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialAgentJWT, CredentialBrokerToken},
		ResourceResolver: "agent-from-thread",
		BasePermission:   "agent.message",
		Effects:          []SecurityEffect{EffectEmitExternal},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		ExternalPolicy: &ExternalEffectPolicy{
			DeliveryMode:   DeliveryFireAndForget,
			FailureMode:    FailureLogAndContinue,
			IdempotencyKey: "message ID",
			RetryPolicy:    "no retry for user-sent messages",
			AuthBeforeEmit: true,
		},
		AuditObligation: &AuditObligation{
			EventType:              "agent.message.send",
			ContextFields:          []string{"actor_id", "project_id"},
			AfterFields:            []string{"message_id", "target_agent_id"},
			Atomic:                 false,
			NonAtomicJustification: "Message dispatch is fire-and-forget; audit recorded before dispatch",
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
}
