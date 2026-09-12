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
	"reflect"
	"testing"
)

// DEF-58 NEGATIVE GATE
//
// brokerIdentityImpl must NEVER satisfy UserIdentity or AgentIdentity.
//
// Broker traffic enters via a distinct authentication path (HMAC-signed
// headers) and uses BrokerIdentity for authorization decisions. If
// brokerIdentityImpl were to accidentally implement UserIdentity or
// AgentIdentity — for example by adding an Email() or ProjectID()
// method during a refactor — the context-extraction helpers
// (GetUserIdentityFromContext, GetAgentIdentityFromContext) would
// begin returning it as a user or agent principal. That would route
// broker-authenticated requests through principal-authorization paths
// the broker was never meant to enter, silently bypassing the
// broker-specific authz checks and creating a privilege-escalation
// vector.
//
// This gate makes that accident a loud test failure instead of a
// silent regression.

var (
	brokerType     = reflect.TypeOf((*brokerIdentityImpl)(nil))
	userIfaceType  = reflect.TypeOf((*UserIdentity)(nil)).Elem()
	agentIfaceType = reflect.TypeOf((*AgentIdentity)(nil)).Elem()
)

func TestBrokerIdentityImpl_MustNotSatisfyUserIdentity(t *testing.T) {
	if brokerType.Implements(userIfaceType) || reflect.PointerTo(brokerType.Elem()).Implements(userIfaceType) {
		t.Fatal("DEF-58 VIOLATION: *brokerIdentityImpl satisfies UserIdentity — " +
			"this would route broker traffic through the user principal-authorization " +
			"path, bypassing broker-specific authz checks")
	}
}

func TestBrokerIdentityImpl_MustNotSatisfyAgentIdentity(t *testing.T) {
	if brokerType.Implements(agentIfaceType) || reflect.PointerTo(brokerType.Elem()).Implements(agentIfaceType) {
		t.Fatal("DEF-58 VIOLATION: *brokerIdentityImpl satisfies AgentIdentity — " +
			"this would route broker traffic through the agent principal-authorization " +
			"path, bypassing broker-specific authz checks")
	}
}
