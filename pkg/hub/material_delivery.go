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
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
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

// envVarDeliveryKind is the material kind of a stored env var row: a legacy
// row flagged secret is a secret, any other row a plain env var.
func envVarDeliveryKind(v store.EnvVar) MaterialKind {
	if v.Secret {
		return MaterialKindSecret
	}
	return MaterialKindEnvVar
}

// selectDeliveryEnv is resolveEnvFromStorage for the start and restart
// paths, with every project-, hub- and broker-scope row authorized under
// the delivery credential before it is used. The scopes, their order, the
// as_needed and reserved-target skips, the progeny pass and the error
// contract are resolveEnvFromStorage's; TZ rows, which buildStartEnv
// discards, are skipped. A credential that cannot be minted is an error,
// like a store read error.
func (d *HTTPAgentDispatcher) selectDeliveryEnv(ctx context.Context, sel *deliverySelector, agent *store.Agent) (map[string]string, map[string]bool, error) {
	result := make(map[string]string)
	plain := make(map[string]bool)
	if agent == nil {
		return result, plain, nil
	}

	for _, filter := range d.envScopesForDispatch(ctx, agent) {
		vars, err := d.store.ListEnvVars(ctx, filter)
		if err != nil {
			return nil, nil, fmt.Errorf("list %s env vars: %w", filter.Scope, err)
		}
		for _, v := range vars {
			if v.InjectionMode == store.InjectionModeAsNeeded {
				continue
			}
			if secret.IsReservedEnvTarget(v.Key) {
				continue
			}
			// The agent TZ resolver owns TZ: buildStartEnv never takes it
			// from stored env, so a TZ row is neither authorized nor
			// returned.
			if v.Key == agentTZEnvKey {
				continue
			}
			if filter.Scope != store.ScopeUser {
				allowed, err := sel.authorize(ctx, envVarDeliveryKind(v), materialRecordEnvVar, v.ID, v.Key, filter.Scope, filter.ScopeID)
				if err != nil {
					return nil, nil, err
				}
				if !allowed {
					continue
				}
			}
			result[v.Key] = v.Value
			plain[v.Key] = !v.Secret
		}
	}

	// Progeny env vars: unchanged from resolveEnvFromStorage.
	if len(agent.Ancestry) > 1 {
		progenyVars, err := d.store.ListProgenyEnvVars(ctx, agent.Ancestry)
		if err != nil {
			return nil, nil, fmt.Errorf("list progeny env vars: %w", err)
		}
		for _, v := range progenyVars {
			if secret.IsReservedEnvTarget(v.Key) || v.Key == agentTZEnvKey {
				continue
			}
			if _, exists := result[v.Key]; exists {
				continue
			}
			result[v.Key] = v.Value
			plain[v.Key] = !v.Secret
		}
	}
	return result, plain, nil
}

// secretDeliveryCandidate is one secret selected for fetch.
type secretDeliveryCandidate struct {
	meta    secret.SecretMeta // as listed; FetchValues matches on it
	progeny bool
}

// selectDeliverySecrets is resolveSecrets for the start and restart paths.
// It returns the same list resolveSecrets returned (as_needed environment
// secrets and reserved targets dropped, test-admin scope restriction
// applied), built as follows:
//
//   - broker, hub and project secrets are listed by metadata and each one
//     is authorized under the delivery credential;
//   - owner user-scope secrets are listed by metadata and kept, and progeny
//     secrets keep the agent's own progeny decision, as resolveSecrets;
//   - values are fetched only for the kept secrets, by recorded metadata,
//     after every authorization; an item whose fetch fails is skipped, as
//     Resolve skipped it;
//   - precedence: a higher scope's secret replaces a lower scope's secret
//     of the same name, a progeny secret fills only names no scope set,
//     then the highest scope wins per type and target.
//
// A list error or a credential that cannot be minted is an error.
func (d *HTTPAgentDispatcher) selectDeliverySecrets(ctx context.Context, sel *deliverySelector, agent *store.Agent) ([]ResolvedSecret, error) {
	if d.secretBackend == nil || agent == nil {
		return nil, nil
	}

	type scopeEntry struct{ scope, scopeID string }
	scopes := make([]scopeEntry, 0, 4)
	if agent.RuntimeBrokerID != "" {
		scopes = append(scopes, scopeEntry{store.ScopeRuntimeBroker, agent.RuntimeBrokerID})
	}
	scopes = append(scopes, scopeEntry{store.ScopeHub, d.secretBackend.HubID()})
	if agent.ProjectID != "" {
		scopes = append(scopes, scopeEntry{store.ScopeProject, agent.ProjectID})
	}
	if agent.OwnerID != "" {
		scopes = append(scopes, scopeEntry{store.ScopeUser, agent.OwnerID})
	}

	var kept []secretDeliveryCandidate
	for _, sc := range scopes {
		metas, err := d.secretBackend.List(ctx, secret.Filter{Scope: sc.scope, ScopeID: sc.scopeID})
		if err != nil {
			return nil, err
		}
		for _, m := range metas {
			if m.SecretType == store.SecretTypeInternal {
				continue
			}
			if sc.scope != store.ScopeUser {
				allowed, err := sel.authorize(ctx, MaterialKindSecret, materialRecordSecret, m.ID, m.Name, sc.scope, sc.scopeID)
				if err != nil {
					return nil, err
				}
				if !allowed {
					continue
				}
			}
			kept = append(kept, secretDeliveryCandidate{meta: m})
		}
	}

	// Progeny secrets: the agent's own progeny decision, as resolveSecrets.
	if len(agent.Ancestry) > 1 && d.authzService != nil {
		progeny, err := d.store.ListProgenySecrets(ctx, agent.Ancestry)
		if err != nil {
			return nil, err
		}
		check := d.progenySecretCheck(ctx, agent)
		for i := range progeny {
			m := *secretMetaFromStore(&progeny[i])
			if m.SecretType == store.SecretTypeInternal {
				continue
			}
			if !check(m) {
				continue
			}
			kept = append(kept, secretDeliveryCandidate{meta: m, progeny: true})
		}
	}

	if d.debug && sel.denied > 0 {
		d.log.Debug("material delivery: items not authorized", "agent_id", agent.ID, "purpose", string(sel.purpose), "count", sel.denied)
	}
	if len(kept) == 0 {
		return nil, nil
	}

	metas := make([]secret.SecretMeta, len(kept))
	for i, c := range kept {
		metas[i] = c.meta
	}
	values, err := d.secretBackend.FetchValues(ctx, metas)
	if err != nil {
		return nil, err
	}

	merged := make(map[string]secret.SecretWithValue)
	for _, c := range kept {
		r, ok := values[c.meta.ID]
		if !ok || r.Err != nil {
			continue
		}
		if c.progeny {
			if _, exists := merged[c.meta.Name]; exists {
				continue
			}
		}
		sv := secret.SecretWithValue{SecretMeta: c.meta, Value: r.Value}
		if sv.SecretType == "" {
			sv.SecretType = store.SecretTypeEnvironment
		}
		if sv.Target == "" {
			sv.Target = sv.Name
		}
		merged[c.meta.Name] = sv
	}
	names := make([]string, 0, len(merged))
	for n := range merged {
		names = append(names, n)
	}
	sort.Strings(names)
	resolved := make([]secret.SecretWithValue, 0, len(merged))
	for _, n := range names {
		resolved = append(resolved, merged[n])
	}
	resolved = secret.DeduplicateByTarget(resolved)
	resolved = d.restrictTestAdminRootedSecrets(ctx, agent, resolved)

	result := make([]ResolvedSecret, 0, len(resolved))
	for _, sv := range resolved {
		if (sv.SecretType == store.SecretTypeEnvironment || sv.SecretType == "") && secret.IsReservedEnvTarget(sv.Target) {
			continue
		}
		if sv.InjectionMode == store.InjectionModeAsNeeded && (sv.SecretType == store.SecretTypeEnvironment || sv.SecretType == "") {
			continue
		}
		result = append(result, ResolvedSecret{
			Name:   sv.Name,
			Type:   sv.SecretType,
			Target: sv.Target,
			Value:  sv.Value,
			Source: sv.Scope,
			Ref:    sv.SecretRef,
		})
	}
	return result, nil
}

// secretMetaFromStore converts a stored secret row to backend metadata,
// field for field as the local backend does.
func secretMetaFromStore(s *store.Secret) *secret.SecretMeta {
	return &secret.SecretMeta{
		ID:            s.ID,
		Name:          s.Key,
		SecretType:    s.SecretType,
		Target:        s.Target,
		Scope:         s.Scope,
		ScopeID:       s.ScopeID,
		Description:   s.Description,
		InjectionMode: s.InjectionMode,
		SecretRef:     s.SecretRef,
		AllowProgeny:  s.AllowProgeny,
		Version:       s.Version,
		Created:       s.Created,
		Updated:       s.Updated,
		CreatedBy:     s.CreatedBy,
		UpdatedBy:     s.UpdatedBy,
	}
}

// probe reports whether a project- or hub-scope record the auth credential
// probe found would be delivered to the agent at start: Decide(*.deliver)
// under the delivery credential, with no value read.
//
// A nil selector is the create-time probe, which runs before the agent row
// and its delegation edge are written, so there is no target to authorize
// against: the record counts by existence (true), and every item is still
// authorized at start before any value is read. A credential that cannot
// be minted for an existing agent is an error.
func (s *deliverySelector) probe(ctx context.Context, kind MaterialKind, record materialRecordKind, id, key, scope, scopeID string) (bool, error) {
	if s == nil {
		return true, nil
	}
	return s.authorize(ctx, kind, record, id, key, scope, scopeID)
}

// newProbeDeliverySelector returns the selector the auth credential probe
// uses for agent, or nil when the agent's record does not exist yet (the
// create-time probe). A lookup error other than not-found is returned.
func (s *Server) newProbeDeliverySelector(ctx context.Context, agent *store.Agent) (*deliverySelector, error) {
	if agent == nil || agent.ID == "" || s.store == nil {
		return nil, nil
	}
	stored, err := s.store.GetAgent(ctx, agent.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, nil
	}
	return newDeliverySelector(s.authzService, stored, PurposeAvailabilityProbe), nil
}
