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
//     or target (precedence runs after authorization). An agent whose
//     delegation chain fails the admission walk only because of legacy
//     provenance is admitted by the legacy fallback, with a rate-limited
//     Warn per item (material_delivery_legacy.go);
//  4. fetch: secret values only for authorized items, by recorded metadata
//     (FetchValues), so no value is read before the credential is minted
//     and the item admitted;
//  5. precedence: runtime_broker < hub < project < user, as before.
//
// User-scope rows and progeny rows are not authorized under the delivery
// credential. They get the start-time checks resolveSecrets applies
// (startSecretResolveOpts): the owner path's user-scope check and the
// agent's own progeny decision.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

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
	// denied counts items Decide refused (the per-start Warn summary).
	denied int
}

// logger is the authorization service's logger, or the default logger.
func (s *deliverySelector) logger() *slog.Logger {
	if s.authz != nil && s.authz.logger != nil {
		return s.authz.logger
	}
	return slog.Default()
}

func newDeliverySelector(authz *AuthzService, agent *store.Agent, purpose MaterialPurpose) *deliverySelector {
	return &deliverySelector{authz: authz, agent: agent, purpose: purpose}
}

// mint builds the delivery credential from the stored agent, once, with an
// admission memo so the items of this selection share one admission walk
// per permission. It fails when there is no authorization service, when
// the stored agent is missing, deleted or has no project, or when the
// stored agent's project differs from the copy being started.
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
	h, err := s.authz.newLaunchDeliveryIdentity(ctx, s.agent.ID, legacyDeliveryFallbackPurpose(s.purpose))
	if err != nil {
		return fmt.Errorf("material delivery: %w", err)
	}
	if h.projectID != s.agent.ProjectID {
		return fmt.Errorf("material delivery: agent %s project changed", s.agent.ID)
	}
	s.ident = h
	return nil
}

// newLaunchDeliveryIdentity is the ONLY production caller of
// newHubDeliveryIdentity (TestHubDelivery_ConstructorCallSites pins it). It
// builds the delivery credential for agentID from the stored agent, with a
// fresh admission memo, so one selection judges all its items against one
// admission walk per permission and drops the memo with the credential, and
// with the legacy fallback when legacyFallback is set. Its callers are
// deliverySelector.mint and the read-only census (LaunchDeliveryCensus). The
// credential is an in-memory value only: neither path issues a token from
// it, persists it or puts it in a request context, and the census reads
// only the admission outcomes it yields.
func (a *AuthzService) newLaunchDeliveryIdentity(ctx context.Context, agentID string, legacyFallback bool) (*hubDeliveryIdentity, error) {
	h, err := a.newHubDeliveryIdentity(ctx, agentID)
	if err != nil {
		return nil, err
	}
	h.admission = newHubDeliveryAdmissionMemo()
	h.legacyFallback = legacyFallback
	return h, nil
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
		// A launch item the delivery decision refuses is withheld from the
		// start or restart (the start itself goes ahead). Say so at Warn,
		// with the item's name, scope and the deny cause, never its value, so
		// an operator can see why an agent came up without it. The create-time
		// availability probe decides nothing for delivery and stays quiet.
		// Not logged once the request has ended: a decision under a
		// cancelled or expired ctx is the caller's cancel, not a refusal
		// worth an operator's attention.
		if (s.purpose == PurposeDispatch || s.purpose == PurposeRestart) && ctx.Err() == nil {
			s.logger().WarnContext(ctx, "launch material withheld: not authorized for delivery",
				"agent_id", s.agent.ID, "purpose", string(s.purpose), "kind", string(kind),
				"key", key, "scope", scope, "scope_id", scopeID,
				"deny_cause", string(d.DenyCause), "reason", d.Reason)
		}
	}
	if d.Allowed {
		s.warnLegacyFallback(ctx, kind, key, scope, scopeID)
	}
	return d.Allowed, nil
}

// warnLegacyFallback logs, at Warn and rate-limited per agent, cohort and
// item (AuthzService.legacyDeliveryLog), an item delivered only through the legacy
// fallback (material_delivery_legacy.go): names and cohort, never a value.
// The admission memo holds the outcome step 10 used for this permission.
// The availability probe delivers nothing and stays quiet. Once the request
// has ended nothing is logged and the rate-limit slot is not used, so a
// later live delivery of the item is still logged.
func (s *deliverySelector) warnLegacyFallback(ctx context.Context, kind MaterialKind, key, scope, scopeID string) {
	if s.purpose == PurposeAvailabilityProbe || s.ident == nil || s.ident.admission == nil || ctx.Err() != nil {
		return
	}
	o, ok := s.ident.admission.load(deliverPermissionForKind(kind))
	if !ok || !o.ok || o.legacyCohort == "" {
		return
	}
	dedupKey := s.agent.ID + "\x00" + o.legacyCohort + "\x00" + string(kind) + "\x00" + scope + "\x00" + scopeID + "\x00" + key
	if dl := s.authz.legacyDeliveryLog; dl != nil && !dl.first(dedupKey, time.Now()) {
		return
	}
	s.logger().WarnContext(ctx, "launch material delivered through the legacy fallback",
		"agent_id", s.agent.ID, "purpose", string(s.purpose), "cohort", o.legacyCohort,
		"exception", "legacy",
		"kind", string(kind), "key", key, "scope", scope, "scope_id", scopeID,
		"reason", o.reason)
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
//   - owner user-scope secrets are listed by metadata and held to the
//     start-time owner-path check, and progeny secrets to the agent's own
//     progeny decision, both from startSecretResolveOpts, as resolveSecrets
//     applies them;
//   - values are fetched only for the kept secrets, by recorded metadata,
//     after every authorization, and not for a scope the test-admin
//     restriction withholds. An item whose record is gone since it was
//     listed (store.ErrNotFound), or whose Secret Manager read fails
//     (secret.ErrValueAccess), is skipped, as Resolve skips it. Any other
//     fetch failure fails the selection, as it fails Resolve: stored
//     values that cannot be decrypted are reported together as one
//     *secret.ResolveDecryptError (errors.Is secret.ErrResolveDecrypt),
//     so no agent starts with an apparently complete but partial set and
//     no lower-scope item stands in for the failed one;
//   - each delivered secret's Ref is the external reference the backend
//     read its value from (FetchResult.Ref), in the form Resolve reported:
//     for Secret Manager, the projects/<p>/secrets/<name> resource path
//     brokers mount, including the computed name for a record with no
//     stored ref;
//   - precedence: a higher scope's secret replaces a lower scope's secret
//     of the same name, a progeny secret fills only names no scope set,
//     then the highest scope wins per type and target.
//
// A list error, a credential that cannot be minted or a fetch failure other
// than the two skipped above is an error.
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

	// The start-time owner-path and progeny checks resolveSecrets applies
	// through the backend's Resolve. Nil without an authorization service.
	startOpts := d.startSecretResolveOpts(ctx, agent)

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
			} else if startOpts != nil && startOpts.UserScopeCheck != nil && !startOpts.UserScopeCheck(m) {
				// Skipped before any value is read, so a lower-scope
				// row with the same name stays in effect.
				continue
			}
			kept = append(kept, secretDeliveryCandidate{meta: m})
		}
	}

	// Progeny secrets: the agent's own progeny decision, as resolveSecrets.
	// startSecretResolveOpts sets AuthzCheck only for an agent with more
	// than one ancestry entry.
	if startOpts != nil && startOpts.AuthzCheck != nil {
		progeny, err := d.store.ListProgenySecrets(ctx, agent.Ancestry)
		if err != nil {
			return nil, err
		}
		check := startOpts.AuthzCheck
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

	// Not logged once the request has ended (see authorize).
	if sel.denied > 0 && ctx.Err() == nil {
		d.log.WarnContext(ctx, "launch material withheld: items not authorized for delivery", "agent_id", agent.ID, "purpose", string(sel.purpose), "count", sel.denied)
	}
	if len(kept) == 0 {
		return nil, nil
	}

	// The test-admin scope restriction is decided before the fetch, so no
	// value is read for a scope it withholds. A withheld item is still
	// placed in precedence below, without a value: as when it was fetched
	// and dropped afterwards, it outranks a lower-scope item of the same
	// name or target, and the restriction then drops it. The one
	// difference is fail-closed: a withheld item whose value could not
	// have been read still outranks that lower-scope item.
	restricted, allowedScopes, testAdminRoot := d.testAdminRootScopes(ctx, agent)
	withheld := func(scope string) bool { return restricted && !allowedScopes[scope] }

	metas := make([]secret.SecretMeta, 0, len(kept))
	for _, c := range kept {
		if withheld(c.meta.Scope) {
			continue
		}
		metas = append(metas, c.meta)
	}
	var values map[string]secret.FetchResult
	if len(metas) > 0 {
		var err error
		values, err = d.secretBackend.FetchValues(ctx, metas)
		if err != nil {
			return nil, err
		}
	}

	merged := make(map[string]secret.SecretWithValue)
	var decryptFailures []secret.ResolveFailure
	for _, c := range kept {
		// A progeny secret fills only names no scope set (kept lists every
		// scope secret before any progeny secret). It is checked before its
		// fetch result, as Resolve skips it before reading its value.
		if c.progeny {
			if _, exists := merged[c.meta.Name]; exists {
				continue
			}
		}
		sv := secret.SecretWithValue{SecretMeta: c.meta}
		if !withheld(c.meta.Scope) {
			r, ok := values[c.meta.ID]
			if !ok || errors.Is(r.Err, store.ErrNotFound) || errors.Is(r.Err, secret.ErrValueAccess) {
				continue
			}
			if r.Err != nil {
				var rde *secret.ResolveDecryptError
				if errors.As(r.Err, &rde) {
					decryptFailures = append(decryptFailures, rde.Failures...)
					continue
				}
				return nil, fmt.Errorf("fetching secret %s (%s scope): %w", c.meta.Name, c.meta.Scope, r.Err)
			}
			sv.Value = r.Value
			// The backend's external reference for the value it read, in
			// the form Resolve reported (for Secret Manager, the resource
			// path brokers mount, never the stored "gcpsm:" form).
			sv.SecretRef = r.Ref
		}
		if sv.SecretType == "" {
			sv.SecretType = store.SecretTypeEnvironment
		}
		if sv.Target == "" {
			sv.Target = sv.Name
		}
		merged[c.meta.Name] = sv
	}
	if len(decryptFailures) > 0 {
		return nil, &secret.ResolveDecryptError{Failures: decryptFailures}
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
	resolved = d.applyTestAdminSecretScopes(ctx, agent, resolved, restricted, allowedScopes, testAdminRoot)

	result := make([]ResolvedSecret, 0, len(resolved))
	for _, sv := range resolved {
		// applyTestAdminSecretScopes has dropped every withheld item; this
		// keeps an unfetched one from ever being delivered.
		if withheld(sv.Scope) {
			continue
		}
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
