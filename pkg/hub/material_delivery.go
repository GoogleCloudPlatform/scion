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

// Launch material selection (ptone/scion#2129).
//
// The start and restart paths (buildStartEnv) select the stored env vars and
// secrets an agent receives through a deliverySelector. Every project-, hub-
// and broker-scope item is authorized, one by one, by
// Decide(secret.deliver | env_var.deliver, ActionDeliver) under the internal
// delivery credential (hubDeliveryIdentity) bound to the agent, before any
// secret value is read:
//
//  1. enumerate: env var rows from the store, secret metadata from the
//     secret backend's List (no values);
//  2. mint: the delivery credential is built from the stored agent the
//     first time an item needs authorizing (newHubDeliveryIdentity); a
//     failure fails the whole start;
//  3. authorize: each associated item through Decide; a denied item is
//     not delivered and does not mask a lower-scope item for the same key
//     or target (precedence runs after authorization);
//  4. fetch: secret values only for authorized items, by recorded metadata
//     (FetchValues), so no value is read before the credential is minted
//     and the item admitted;
//  5. precedence: runtime_broker < hub < project < user, as before.
//
// User-scope rows and progeny rows keep their existing selection here
// (owner-scope rows, and progeny rows through the agent's own progeny
// decision), unchanged by this file.

import (
	"context"
	"errors"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// errDeliveryAuthzUnavailable is returned when an associated item needs
// authorizing and the dispatcher has no authorization service. The start
// fails closed.
var errDeliveryAuthzUnavailable = errors.New("material delivery: authorization service not available")

// deliverySelector authorizes launch material for one agent and one
// purpose under the delivery credential. It is created per start or
// restart and never reused, so every selection re-reads the agent, the
// material records and the delegation chain.
type deliverySelector struct {
	authz   *AuthzService
	agent   *store.Agent
	purpose MaterialPurpose
	// ident is minted on the first authorize call (mint).
	ident *hubDeliveryIdentity
	// denied counts items Decide refused, for the debug log only.
	denied int
}

func newDeliverySelector(authz *AuthzService, agent *store.Agent, purpose MaterialPurpose) *deliverySelector {
	return &deliverySelector{authz: authz, agent: agent, purpose: purpose}
}

// mint builds the delivery credential from the stored agent, once. It fails
// when there is no authorization service, when the stored agent is missing,
// deleted or has no project, or when the stored agent's project differs
// from the copy being started.
func (s *deliverySelector) mint(ctx context.Context) error {
	if s.ident != nil {
		return nil
	}
	if s.authz == nil {
		return errDeliveryAuthzUnavailable
	}
	if s.agent == nil || s.agent.ID == "" {
		return errors.New("material delivery: no target agent")
	}
	h, err := s.authz.newHubDeliveryIdentity(ctx, s.agent.ID)
	if err != nil {
		return fmt.Errorf("material delivery: %w", err)
	}
	if h.projectID != s.agent.ProjectID {
		return fmt.Errorf("material delivery: agent %s project changed", s.agent.ID)
	}
	s.ident = h
	return nil
}

// authorize decides one associated item. It returns an error only when the
// credential cannot be minted; a denial is (false, nil).
func (s *deliverySelector) authorize(ctx context.Context, kind MaterialKind, record materialRecordKind, id, key, scope, scopeID string) (bool, error) {
	grant, ok := grantForDeliveryScope(scope)
	if !ok {
		return false, nil
	}
	if err := s.mint(ctx); err != nil {
		return false, err
	}
	res := materialDeliveryResource(kind, record, id, key, scope, scopeID, s.agent.ID, grant, s.purpose)
	d := s.authz.Decide(ctx, deliveryAuthzRequest(s.ident, kind, res))
	if !d.Allowed {
		s.denied++
	}
	return d.Allowed, nil
}

// deliveryAuthzRequest is the Decide request for one launch item. Explain
// is never set: the hub_delivery step-10 trace names delegation edge,
// delegator and root user IDs, and no launch caller needs or returns it.
// The decision's reason is not returned to any caller either (see
// deliverySelector's callers).
func deliveryAuthzRequest(h *hubDeliveryIdentity, kind MaterialKind, res Resource) AuthzRequest {
	return AuthzRequest{
		Principal:  principalContextForIdentity(h),
		Credential: credentialContextForIdentity(h),
		Resource:   res,
		Action:     ActionDeliver,
		Permission: deliverPermissionForKind(kind),
	}
}
