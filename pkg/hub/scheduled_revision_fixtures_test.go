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
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The revision fixtures in this file need no store, so they build under
// every tag set; the ones that read a stored row are in
// scheduled_authority_fixtures_test.go.

// withSessionRevision returns evt carrying the recorded authorization
// revision a session create or resume by userID writes: session attribution
// for the user and the principal ceiling.
func withSessionRevision(evt store.ScheduledEvent, userID string) store.ScheduledEvent {
	evt.InitiatorAttribution = store.InitiatorAttribution{
		InitiatorPrincipalKind:  store.DelegationPrincipalUser,
		InitiatorPrincipalID:    userID,
		InitiatorCredentialKind: store.InitiatorCredentialKindSession,
		AttributionVersion:      1,
		AuthorizationRevision:   1,
	}
	evt.AuthorityCeiling = store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
	return evt
}

// withMockAgentRevision returns evt carrying an agent revision for agentID
// whose recorded ceiling allows every registry permission, for stores where
// the author's write ceiling is not computed. A fire intersects it with the
// agent's write ceiling at fire time, so the result is that ceiling.
func withMockAgentRevision(evt store.ScheduledEvent, agentID string) store.ScheduledEvent {
	ids := make([]string, 0, len(permissions.Registry))
	for _, p := range permissions.Registry {
		ids = append(ids, p.ID)
	}
	evt.InitiatorAttribution = store.InitiatorAttribution{
		InitiatorPrincipalKind:  store.DelegationPrincipalAgent,
		InitiatorPrincipalID:    agentID,
		InitiatorCredentialKind: store.InitiatorCredentialKindAgent,
		InitiatorCredentialID:   "jti-" + agentID,
		AttributionVersion:      1,
		AuthorizationRevision:   1,
	}
	evt.AuthorityCeiling = store.EffectCeiling{
		Kind:          store.EffectCeilingBounded,
		Version:       permissions.CeilingVersionV1,
		PermissionIDs: sortedUniqueIDs(ids),
	}
	return evt
}
