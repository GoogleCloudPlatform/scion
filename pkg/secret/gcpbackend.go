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

package secret

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"

	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// GCPBackend implements SecretBackend using a hybrid approach:
// metadata is stored in the Hub database, values are stored in GCP Secret Manager.
type GCPBackend struct {
	store                store.SecretStore
	smClient             SMClient
	projectID            string
	hubID                string
	replicationLocations []string
	mu                   sync.RWMutex
	hubName              string
}

// NewGCPBackend creates a GCPBackend with a real GCP Secret Manager client.
func NewGCPBackend(ctx context.Context, s store.SecretStore, cfg GCPBackendConfig, hubID string) (*GCPBackend, error) {
	if cfg.ProjectID == "" {
		return nil, fmt.Errorf("gcpsm backend requires a GCP project ID")
	}
	if hubID == "" {
		// secretNamePrefix() still produces a valid, deterministic prefix
		// (sha256("")[:12]) so this is not fatal, but every hub instance that
		// hits this ends up sharing the same meaningless prefix, defeating
		// the point of this feature (ptone/scion#2152 review finding 15).
		slog.Warn("GCP secret backend configured with an empty hub ID; secrets will use a shared, non-hub-specific name prefix")
	}
	smClient, err := newGCPSMClient(ctx, cfg.CredentialsJSON)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCP SM client: %w", err)
	}
	return &GCPBackend{
		store:                s,
		smClient:             smClient,
		projectID:            cfg.ProjectID,
		hubID:                hubID,
		replicationLocations: cfg.ReplicationLocations,
	}, nil
}

// NewGCPBackendWithClient creates a GCPBackend with a provided SMClient (for testing).
func NewGCPBackendWithClient(s store.SecretStore, client SMClient, projectID, hubID string) *GCPBackend {
	return &GCPBackend{
		store:     s,
		smClient:  client,
		projectID: projectID,
		hubID:     hubID,
	}
}

// HubID returns the hub instance ID used for hub-scoped secret namespacing.
func (b *GCPBackend) HubID() string {
	return b.hubID
}

// SetHubName sets the human-readable hub display name.
func (b *GCPBackend) SetHubName(name string) {
	b.mu.Lock()
	b.hubName = name
	b.mu.Unlock()
}

// HubName returns the hub display name last set with SetHubName ("" if
// none; labels then fall back to the hostname).
func (b *GCPBackend) HubName() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.hubName
}

func (b *GCPBackend) Get(ctx context.Context, name, scope, scopeID string) (*SecretWithValue, error) {
	// Get metadata from DB
	s, err := b.store.GetSecret(ctx, name, scope, scopeID)
	if err != nil && err != store.ErrNotFound {
		return nil, err
	}

	// Prefer the stored SecretRef for GCP SM lookup (handles secrets created under
	// a previous naming scheme). Fall back to computing the name if no ref is stored.
	// If no DB record exists at all, try GCP SM directly by computed name — this
	// handles database resets where the secret still exists in GCP SM.
	var value string
	if s != nil {
		if smPath, ok := extractGCPSMPath(s.SecretRef); ok {
			value, err = b.accessLatestVersionByPath(ctx, smPath)
		} else {
			value, _, err = b.accessSecretByComputedName(ctx, name, scope, scopeID)
		}
		// Convert gRPC NotFound to store.ErrNotFound so callers such as
		// MigratePluginSecrets handle missing GCP SM resources correctly.
		if err != nil && status.Code(err) == codes.NotFound {
			return nil, store.ErrNotFound
		}
	} else {
		// No DB record; try GCP SM directly by computed name (hub-prefixed,
		// falling back to the legacy pre-prefix name) to handle database
		// resets where the secret still exists in GCP SM.
		value, _, err = b.accessSecretByComputedName(ctx, name, scope, scopeID)
		if err != nil {
			if status.Code(err) == codes.NotFound {
				return nil, store.ErrNotFound
			}
		} else {
			slog.Info("Recovered secret from GCP SM without DB record", "name", name, "scope", scope)
		}
	}
	if err != nil {
		if permErr := b.wrapGCPError(err, "access secret"); permErr != nil {
			return nil, permErr
		}
		return nil, fmt.Errorf("failed to access secret value from GCP SM: %w", err)
	}

	var meta *SecretMeta
	if s != nil {
		meta = fromStoreSecretMeta(s)
	} else {
		meta = &SecretMeta{
			Name:       name,
			Scope:      scope,
			ScopeID:    scopeID,
			SecretType: store.SecretTypeInternal,
		}
	}
	return &SecretWithValue{
		SecretMeta: *meta,
		Value:      value,
	}, nil
}

// FetchValues returns values for exactly the given metadata records, matched
// by ID, Version, AllowProgeny, CreatedBy and SecretType, keyed by each
// record's ID in the returned map. A decrypt or backend-access failure is
// reported as a per-item error, never as an empty value delivered in place
// of an error. A record whose current SecretType is internal is refused with
// store.ErrNotFound, since internal secrets are never candidates for
// delivery. The returned outer error reports only a failure of the whole
// call, not a per-item failure.
//
// Unlike Get, it never falls back to a Secret Manager lookup by computed
// name when the Hub database record is missing entirely: a missing or
// mismatched record is reported as store.ErrNotFound for that item. A record
// that does exist but has no stored ref yet (not yet touched by
// `migrate-names` or hub-boot copy-forward) resolves the same way Get does
// in that case: by the hub-prefixed computed name, falling back to the
// legacy pre-prefix name with a WARN log (ptone/scion#2152).
//
// store.SecretStore has no primary-key lookup (see LocalBackend.FetchValues
// for the reasoning this mirrors), so each record is located by its
// Name/Scope/ScopeID triple and then verified against the recorded metadata
// (see recordGenerationChanged) before its value is read from Secret
// Manager, and again immediately after: the DB record and the Secret
// Manager value are two separate reads with no shared transaction, so a Set
// or delete-and-recreate can land in between them. Re-checking after the
// Secret Manager read narrows that window instead of returning a new
// generation's value under the old generation's metadata.
func (b *GCPBackend) FetchValues(ctx context.Context, metas []SecretMeta) (map[string]FetchResult, error) {
	results := make(map[string]FetchResult, len(metas))
	for _, meta := range metas {
		results[meta.ID] = b.fetchValue(ctx, meta)
	}
	return results, nil
}

func (b *GCPBackend) fetchValue(ctx context.Context, meta SecretMeta) FetchResult {
	s, err := b.store.GetSecret(ctx, meta.Name, meta.Scope, meta.ScopeID)
	if err != nil {
		return FetchResult{Err: err}
	}
	if recordGenerationChanged(s, meta) {
		// The record has been replaced, rotated or reclassified since the
		// caller's metadata was recorded; treat it the same as not found
		// rather than accessing Secret Manager for a different generation
		// of the record, or falling back to a computed name.
		return FetchResult{Err: store.ErrNotFound}
	}

	var value string
	if smPath, ok := extractGCPSMPath(s.SecretRef); ok {
		value, err = b.AccessSecretValueByRef(ctx, smPath)
	} else {
		// No stored ref: mirror Get's DB-less recovery path (tries the
		// hub-prefixed name, then falls back to the legacy pre-prefix name
		// with a WARN log) rather than looking up the prefixed name alone,
		// so a record that predates ref-tracking and hasn't yet been
		// touched by `migrate-names` or hub-boot copy-forward still
		// resolves (ptone/scion#2152 / GoogleCloudPlatform/scion#2085 interaction).
		value, _, err = b.accessSecretByComputedName(ctx, s.Key, s.Scope, s.ScopeID)
	}
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return FetchResult{Err: store.ErrNotFound}
		}
		if permErr := b.wrapGCPError(err, "access secret"); permErr != nil {
			return FetchResult{Err: permErr}
		}
		return FetchResult{Err: err}
	}

	// Re-read the DB record and repeat the comparison. A metadata update,
	// or a Set whose database write lands between the Secret Manager read
	// and this re-read, is reported as not found instead of returning a
	// value under metadata that no longer matches. This narrows the race
	// but does not close it: Set adds the Secret Manager version before it
	// writes the database record, so a fetch that completes both reads
	// between those two writes still returns the new value under the old
	// metadata. Closing it needs the Secret Manager version recorded in
	// the database record.
	after, err := b.store.GetSecret(ctx, meta.Name, meta.Scope, meta.ScopeID)
	if err != nil {
		return FetchResult{Err: err}
	}
	if recordGenerationChanged(after, meta) {
		return FetchResult{Err: store.ErrNotFound}
	}
	return FetchResult{Value: value}
}

// extractGCPSMPath extracts the full GCP SM resource path from a stored SecretRef.
// Returns the path and true if the ref is a gcpsm ref, empty string and false otherwise.
func extractGCPSMPath(ref string) (string, bool) {
	if strings.HasPrefix(ref, "gcpsm:") {
		return strings.TrimPrefix(ref, "gcpsm:"), true
	}
	return "", false
}

// accessLatestVersionByPath retrieves the latest version of a secret using a full GCP SM path.
func (b *GCPBackend) accessLatestVersionByPath(ctx context.Context, smPath string) (string, error) {
	resp, err := b.smClient.AccessSecretVersion(ctx, &smpb.AccessSecretVersionRequest{
		Name: smPath + "/versions/latest",
	})
	if err != nil {
		return "", err
	}
	return string(resp.Payload.Data), nil
}

// buildReplication returns the replication policy for new GCP SM secrets.
// When replicationLocations is non-empty, user-managed replication with the
// specified regions is used; otherwise automatic (global) replication is returned.
func (b *GCPBackend) buildReplication() *smpb.Replication {
	if len(b.replicationLocations) > 0 {
		replicas := make([]*smpb.Replication_UserManaged_Replica, len(b.replicationLocations))
		for i, loc := range b.replicationLocations {
			replicas[i] = &smpb.Replication_UserManaged_Replica{Location: loc}
		}
		return &smpb.Replication{
			Replication: &smpb.Replication_UserManaged_{
				UserManaged: &smpb.Replication_UserManaged{Replicas: replicas},
			},
		}
	}
	return &smpb.Replication{
		Replication: &smpb.Replication_Automatic_{
			Automatic: &smpb.Replication_Automatic{},
		},
	}
}

func (b *GCPBackend) Set(ctx context.Context, input *SetSecretInput) (bool, *SecretMeta, error) {
	smName := b.gcpSecretName(input.Name, input.Scope, input.ScopeID)
	fullName := fmt.Sprintf("projects/%s/secrets/%s", b.projectID, smName)

	target := input.Target
	if target == "" {
		target = input.Name
	}

	if err := b.ensureSecretAndAddVersion(ctx, smName, []byte(input.Value), buildLabels(input, target, b.resolveHubName())); err != nil {
		return false, nil, err
	}

	// Store metadata in Hub DB (with a reference instead of the value)
	secret := toStoreSecret(input)
	secret.EncryptedValue = "" // Don't store value in DB
	secret.SecretRef = "gcpsm:" + fullName

	created, err := b.store.UpsertSecret(ctx, secret)
	if err != nil {
		return false, nil, fmt.Errorf("failed to store secret metadata: %w", err)
	}

	meta := fromStoreSecretMeta(secret)
	return created, meta, nil
}

// ensureSecretAndAddVersion creates the named GCP SM secret container if it
// does not already exist (using the backend's replication policy and the
// given labels), then adds a new version carrying value. Shared by Set() and
// the hub-prefixed name migration helpers below so container creation,
// replication policy, and permission-error wrapping stay consistent.
func (b *GCPBackend) ensureSecretAndAddVersion(ctx context.Context, smName string, value []byte, labels map[string]string) error {
	fullName := fmt.Sprintf("projects/%s/secrets/%s", b.projectID, smName)

	// Ensure the GCP SM secret exists (create if needed)
	_, err := b.smClient.GetSecret(ctx, &smpb.GetSecretRequest{
		Name: fullName,
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			// Create the secret
			_, err = b.smClient.CreateSecret(ctx, &smpb.CreateSecretRequest{
				Parent:   fmt.Sprintf("projects/%s", b.projectID),
				SecretId: smName,
				Secret: &smpb.Secret{
					Replication: b.buildReplication(),
					Labels:      labels,
				},
			})
			if err != nil && status.Code(err) != codes.AlreadyExists {
				if permErr := b.wrapGCPError(err, "create secret"); permErr != nil {
					return permErr
				}
				return fmt.Errorf("failed to create GCP SM secret: %w", err)
			}
			// AlreadyExists: a concurrent replica of this same hub created the
			// container first (both saw NotFound on GetSecret above and raced
			// to CreateSecret). The container now exists either way, so fall
			// through to add the version — no data is lost, and the key
			// material both replicas are writing is identical (review
			// finding 11, ptone/scion#2152).
		} else {
			if permErr := b.wrapGCPError(err, "check secret"); permErr != nil {
				return permErr
			}
			return fmt.Errorf("failed to check GCP SM secret: %w", err)
		}
	}

	// Add a new version with the secret value
	_, err = b.smClient.AddSecretVersion(ctx, &smpb.AddSecretVersionRequest{
		Parent: fullName,
		Payload: &smpb.SecretPayload{
			Data: value,
		},
	})
	if err != nil {
		if permErr := b.wrapGCPError(err, "add secret version"); permErr != nil {
			return permErr
		}
		return fmt.Errorf("failed to add GCP SM secret version: %w", err)
	}
	return nil
}

func (b *GCPBackend) UpdateMeta(ctx context.Context, input *UpdateMetaInput) (*SecretMeta, error) {
	// Metadata-only update: only modify the Hub DB record.
	// Do NOT create a new GCP SM version — the secret value is unchanged.
	meta := &store.SecretMetaUpdate{
		Description:   input.Description,
		InjectionMode: input.InjectionMode,
		SecretType:    input.SecretType,
		Target:        input.Target,
		AllowProgeny:  input.AllowProgeny,
		UpdatedBy:     input.UpdatedBy,
	}
	updated, err := b.store.UpdateSecretMeta(ctx, input.Name, input.Scope, input.ScopeID, meta)
	if err != nil {
		return nil, err
	}
	return fromStoreSecretMeta(updated), nil
}

func (b *GCPBackend) Delete(ctx context.Context, name, scope, scopeID string) error {
	// Delete the current hub-prefixed name. A failure here (other than
	// NotFound) is always fatal: this is the name new writes actually use.
	prefixedName := b.gcpSecretName(name, scope, scopeID)
	prefixedFull := fmt.Sprintf("projects/%s/secrets/%s", b.projectID, prefixedName)
	prefixedErr := b.smClient.DeleteSecret(ctx, &smpb.DeleteSecretRequest{Name: prefixedFull})
	prefixedWasNotFound := status.Code(prefixedErr) == codes.NotFound
	if prefixedErr != nil && !prefixedWasNotFound {
		if permErr := b.wrapGCPError(prefixedErr, "delete secret"); permErr != nil {
			return permErr
		}
		return fmt.Errorf("failed to delete GCP SM secret %s: %w", prefixedFull, prefixedErr)
	}

	// Whether the legacy delete may be treated as best-effort depends on
	// whether the prefixed copy is confirmed authoritative (ptone/scion#2152
	// round-2 review finding 2). The legacy name might be the ONLY existing
	// copy of the secret in two cases: the prefixed delete just reported
	// NotFound (nothing was ever migrated), or a DB record exists whose
	// SecretRef is not yet the prefixed name (migrated in GCP SM terms only
	// once the ref says so — see RepairRefToPrefixed). In either case, a
	// legacy-delete failure must be fatal: silently swallowing it would let
	// Delete report success while the secret's only copy survives, ready to
	// be "resurrected" by the DB-less computed-name read fallback the next
	// time it's looked up.
	hasRecord, refIsPrefixed, err := b.RefPointsAtPrefixed(ctx, name, scope, scopeID)
	if err != nil {
		return fmt.Errorf("failed to check DB record before deleting %s: %w", name, err)
	}
	// A DB record whose ref already points at the prefixed name is confirmed
	// authoritative regardless of what happened to the prefixed delete above
	// (ptone/scion#2152 round-3 review finding 8): if a prior, partially
	// completed migrate-names/Delete run already removed the prefixed copy,
	// re-running Delete would otherwise see prefixedWasNotFound=true and
	// wedge forever on a PermissionDenied legacy delete that can never
	// succeed once IAM has been narrowed. Only the no-DB-record path still
	// leans on prefixedWasNotFound, since it has no ref to consult.
	legacyMightBeOnlyCopy := (!hasRecord && prefixedWasNotFound) || (hasRecord && !refIsPrefixed)

	legacyName := b.legacyGCPSecretName(name, scope, scopeID)
	legacyFull := fmt.Sprintf("projects/%s/secrets/%s", b.projectID, legacyName)
	if legacyErr := b.smClient.DeleteSecret(ctx, &smpb.DeleteSecretRequest{Name: legacyFull}); legacyErr != nil && status.Code(legacyErr) != codes.NotFound {
		if !legacyMightBeOnlyCopy && status.Code(legacyErr) == codes.PermissionDenied {
			// Best-effort cleanup of a copy already superseded by the
			// confirmed-authoritative prefixed one: once an operator has
			// narrowed IAM to only the new prefix, the Hub's service account
			// may no longer have permission to even query the legacy name
			// (GCP evaluates IAM before existence). Any other error code, or
			// PermissionDenied while the legacy copy might still be the only
			// one, is NOT swallowed here — see below.
			slog.Warn("failed to delete legacy GCP SM secret (non-fatal: the hub-prefixed copy is already the authoritative one)",
				"name", name, "scope", scope, "legacy_name", legacyFull, "error", legacyErr)
		} else {
			if permErr := b.wrapGCPError(legacyErr, "delete secret"); permErr != nil {
				return permErr
			}
			return fmt.Errorf("failed to delete GCP SM secret %s: %w", legacyFull, legacyErr)
		}
	}

	// Delete from Hub DB
	return b.store.DeleteSecret(ctx, name, scope, scopeID)
}

func (b *GCPBackend) List(ctx context.Context, filter Filter) ([]SecretMeta, error) {
	// List from DB only (metadata, no values)
	secrets, err := b.store.ListSecrets(ctx, toStoreFilter(filter))
	if err != nil {
		return nil, err
	}
	result := make([]SecretMeta, len(secrets))
	for i, s := range secrets {
		result[i] = *fromStoreSecretMeta(&s)
	}
	return result, nil
}

func (b *GCPBackend) GetMeta(ctx context.Context, name, scope, scopeID string) (*SecretMeta, error) {
	s, err := b.store.GetSecret(ctx, name, scope, scopeID)
	if err != nil {
		return nil, err
	}
	return fromStoreSecretMeta(s), nil
}

func (b *GCPBackend) Resolve(ctx context.Context, userID, projectID, brokerID string, opts *ResolveOpts) ([]SecretWithValue, error) {
	merged := make(map[string]SecretWithValue)

	type scopeEntry struct {
		scope   string
		scopeID string
	}

	// Scope precedence, lowest first: runtime_broker < hub < project < user.
	// Later entries in this slice overwrite earlier ones in the merge loop
	// below, so this order must match envScopePrecedence in
	// pkg/hub/httpdispatcher.go — broker is the most infrastructural and
	// least specific of the four scopes, so it is intentionally the
	// weakest, not an override nobody can escape. (Previously this listed
	// hub, user, project, broker, which put broker last and therefore
	// strongest — the opposite of every other precedence-ordered resolver
	// in this codebase, and the reason a stale broker-scoped secret could
	// silently shadow a project-scoped one regardless of which was meant
	// to win.)
	scopes := make([]scopeEntry, 0, 4)
	if brokerID != "" {
		scopes = append(scopes, scopeEntry{scope: store.ScopeRuntimeBroker, scopeID: brokerID})
	}
	scopes = append(scopes, scopeEntry{scope: store.ScopeHub, scopeID: b.hubID})
	if projectID != "" {
		scopes = append(scopes, scopeEntry{scope: store.ScopeProject, scopeID: projectID})
	}
	if userID != "" {
		scopes = append(scopes, scopeEntry{scope: store.ScopeUser, scopeID: userID})
	}

	for _, sc := range scopes {
		secrets, err := b.store.ListSecrets(ctx, store.SecretFilter{
			Scope:   sc.scope,
			ScopeID: sc.scopeID,
		})
		if err != nil {
			return nil, err
		}

		for _, s := range secrets {
			// Never project hub-internal infrastructure secrets (e.g. signing keys)
			// into agent environments.
			if s.SecretType == store.SecretTypeInternal {
				continue
			}

			var value string
			var secretRef string
			if smPath, ok := extractGCPSMPath(s.SecretRef); ok {
				value, err = b.accessLatestVersionByPath(ctx, smPath)
				secretRef = smPath
			} else {
				var smName string
				value, smName, err = b.accessSecretByComputedName(ctx, s.Key, sc.scope, sc.scopeID)
				secretRef = fmt.Sprintf("projects/%s/secrets/%s", b.projectID, smName)
			}
			if err != nil {
				continue
			}

			secretType := s.SecretType
			if secretType == "" {
				secretType = store.SecretTypeEnvironment
			}
			target := s.Target
			if target == "" {
				target = s.Key
			}

			merged[s.Key] = SecretWithValue{
				SecretMeta: SecretMeta{
					ID:            s.ID,
					Name:          s.Key,
					SecretType:    secretType,
					Target:        target,
					Scope:         sc.scope,
					ScopeID:       sc.scopeID,
					Description:   s.Description,
					InjectionMode: s.InjectionMode,
					SecretRef:     secretRef,
					AllowProgeny:  s.AllowProgeny,
					Version:       s.Version,
					Created:       s.Created,
					Updated:       s.Updated,
					CreatedBy:     s.CreatedBy,
					UpdatedBy:     s.UpdatedBy,
				},
				Value: value,
			}
		}
	}

	// Progeny secret resolution: when the caller is an agent with ancestry,
	// include user-scoped secrets marked allowProgeny whose creator is in the
	// ancestry chain. These are added at user-scope precedence.
	if opts != nil && len(opts.AgentAncestry) > 0 {
		progenySecrets, err := b.store.ListProgenySecrets(ctx, opts.AgentAncestry)
		if err != nil {
			return nil, err
		}
		for _, s := range progenySecrets {
			if _, exists := merged[s.Key]; exists {
				continue
			}
			if s.SecretType == store.SecretTypeInternal {
				continue
			}

			meta := fromStoreSecretMeta(&s)

			// Verify access via the policy engine. With no checker configured,
			// a progeny secret is excluded rather than included by default:
			// the caller must supply an explicit policy decision before any
			// progeny value is read.
			if opts.AuthzCheck == nil || !opts.AuthzCheck(*meta) {
				continue
			}

			// Read value from GCP SM, preferring stored SecretRef
			var value string
			var secretRef string
			if smPath, ok := extractGCPSMPath(s.SecretRef); ok {
				value, err = b.accessLatestVersionByPath(ctx, smPath)
				secretRef = smPath
			} else {
				var smName string
				value, smName, err = b.accessSecretByComputedName(ctx, s.Key, s.Scope, s.ScopeID)
				secretRef = fmt.Sprintf("projects/%s/secrets/%s", b.projectID, smName)
			}
			if err != nil {
				continue
			}

			secretType := s.SecretType
			if secretType == "" {
				secretType = store.SecretTypeEnvironment
			}
			target := s.Target
			if target == "" {
				target = s.Key
			}

			merged[s.Key] = SecretWithValue{
				SecretMeta: SecretMeta{
					ID:            s.ID,
					Name:          s.Key,
					SecretType:    secretType,
					Target:        target,
					Scope:         s.Scope,
					ScopeID:       s.ScopeID,
					Description:   s.Description,
					InjectionMode: s.InjectionMode,
					SecretRef:     secretRef,
					AllowProgeny:  s.AllowProgeny,
					Version:       s.Version,
					Created:       s.Created,
					Updated:       s.Updated,
					CreatedBy:     s.CreatedBy,
					UpdatedBy:     s.UpdatedBy,
				},
				Value: value,
			}
		}
	}

	result := make([]SecretWithValue, 0, len(merged))
	for _, sv := range merged {
		result = append(result, sv)
	}
	return DeduplicateByTarget(result), nil
}

// accessLatestVersion retrieves the latest version of a secret from GCP SM.
func (b *GCPBackend) accessLatestVersion(ctx context.Context, smSecretName string) (string, error) {
	resp, err := b.smClient.AccessSecretVersion(ctx, &smpb.AccessSecretVersionRequest{
		Name: fmt.Sprintf("projects/%s/secrets/%s/versions/latest", b.projectID, smSecretName),
	})
	if err != nil {
		return "", err
	}
	return string(resp.Payload.Data), nil
}

// AccessSecretValueByRef retrieves a secret value using a full GCP SM resource path.
// The path should be in the form "projects/{project}/secrets/{name}".
// This is used during migration to read values from old GCP SM secrets.
func (b *GCPBackend) AccessSecretValueByRef(ctx context.Context, smPath string) (string, error) {
	resp, err := b.smClient.AccessSecretVersion(ctx, &smpb.AccessSecretVersionRequest{
		Name: smPath + "/versions/latest",
	})
	if err != nil {
		return "", fmt.Errorf("failed to access secret at %s: %w", smPath, err)
	}
	return string(resp.Payload.Data), nil
}

// secretNamePrefix returns the hub-scoped prefix prepended to every GCP SM
// secret ID this backend writes (all scopes, including hub). It is the single
// place the prefix formula is computed (ptone/scion#2152): every caller that
// needs the prefix — naming, migration, IAM-grant hints — goes through
// gcpSecretName/legacyGCPSecretName rather than recomputing it.
//
// Format: "scion-" + first 12 hex chars of sha256(raw hubID bytes) + "-".
// The hubID is hashed as-is (no lowercasing, sanitizing, or trimming) so that
// distinct hub IDs never collide and the fixed-length hex digest can never
// become a prefix of another hub's. The result always ends in "-", so an
// operator's IAM condition of the form
// resource.name.startsWith("projects/<NUMBER>/secrets/scion-<h12>-") only ever
// matches this hub's secrets.
//
// There is intentionally no override or enable/disable setting: new
// hub-scoped names are on by default for every hub, and deployment tooling
// (Terraform) computes the identical formula from hub_id to build the
// matching IAM condition.
func (b *GCPBackend) secretNamePrefix() string {
	return SecretNamePrefixForHubID(b.hubID)
}

// SecretNamePrefixForHubID computes the hub-prefix formula
// (ptone/scion#2152) — "scion-" + first 12 hex chars of sha256(hubID) + "-" —
// without requiring a constructed GCPBackend. Exported so callers that only
// need to report the prefix a resolved hub ID would produce (e.g. `hub
// secret migrate-names` printing it before acting, per round-3 review
// finding 1) don't have to duplicate the formula.
func SecretNamePrefixForHubID(hubID string) string {
	hash := sha256.Sum256([]byte(hubID))
	return "scion-" + hex.EncodeToString(hash[:6]) + "-" // 6 bytes = 12 hex chars
}

// gcpSecretName builds a sanitized GCP SM secret ID from the scion secret
// identity, using the current (hub-prefixed) naming scheme.
// Format: {secretNamePrefix()}{scope}-{sha256(hubID:scopeID)[:12]}-{name}
// The hubID is combined with the scopeID before hashing to ensure uniqueness
// across hub instances sharing the same GCP project, independent of the
// hub-prefix segment (which only depends on hubID).
//
// If the raw name would exceed GCP SM's 255-character secret ID limit even
// after sanitization, a short content-hash suffix is appended at the
// truncation point instead of plain-truncating (ptone/scion#2152 review
// finding 16): the longer hub prefix moves the cut point earlier than before
// this feature, so plain truncation makes it more likely that two distinct,
// very long names collide after being cut to the same 255 bytes. This is
// safe to change here (unlike legacyGCPSecretName below, which must keep
// reproducing the exact pre-ptone/scion#2152 truncated bytes to find already-existing
// secrets): every gcpSecretName output is a brand-new name under this PR
// regardless of length, so there is no existing secret whose ID this could
// stop matching.
func (b *GCPBackend) gcpSecretName(name, scope, scopeID string) string {
	combined := b.hubID + ":" + scopeID
	hash := sha256.Sum256([]byte(combined))
	shortHash := hex.EncodeToString(hash[:6]) // 6 bytes = 12 hex chars
	raw := fmt.Sprintf("%s%s-%s-%s", b.secretNamePrefix(), scope, shortHash, name)
	sanitized := invalidSecretIDChars.ReplaceAllString(raw, "-")
	if len(sanitized) <= 255 {
		return sanitized
	}
	contentHash := sha256.Sum256([]byte(sanitized))
	suffix := "-" + hex.EncodeToString(contentHash[:4]) // 4 bytes = 8 hex chars, plus "-" = 9
	return sanitized[:255-len(suffix)] + suffix
}

// legacyGCPSecretName builds the pre-ptone/scion#2152 GCP SM secret ID (no hub
// prefix) for the scion secret identity.
// Format: scion-{scope}-{sha256(hubID:scopeID)[:12]}-{name}
// This is used only as a read/delete fallback for secrets created before
// hub-prefixed names existed; all new writes use gcpSecretName's prefixed
// form.
func (b *GCPBackend) legacyGCPSecretName(name, scope, scopeID string) string {
	combined := b.hubID + ":" + scopeID
	hash := sha256.Sum256([]byte(combined))
	shortHash := hex.EncodeToString(hash[:6])
	return sanitizeSecretID(fmt.Sprintf("scion-%s-%s-%s", scope, shortHash, name))
}

// accessSecretByComputedName tries the current hub-prefixed GCP SM name first
// and, only if that is not found, falls back to the legacy (pre-prefix) name,
// logging a WARN on a legacy hit. It is used whenever a secret must be looked
// up without a stored DB SecretRef to guide the lookup (a DB-less recovery
// read, or a Resolve() pass over a record that predates SecretRef being
// persisted). Returns the value and the GCP SM secret ID that resolved it.
func (b *GCPBackend) accessSecretByComputedName(ctx context.Context, name, scope, scopeID string) (value, smName string, err error) {
	prefixedName := b.gcpSecretName(name, scope, scopeID)
	value, err = b.accessLatestVersion(ctx, prefixedName)
	if err == nil {
		return value, prefixedName, nil
	}
	if status.Code(err) != codes.NotFound {
		return "", "", err
	}

	legacyName := b.legacyGCPSecretName(name, scope, scopeID)
	value, legacyErr := b.accessLatestVersion(ctx, legacyName)
	if legacyErr != nil {
		// Neither name resolved; surface the prefixed-name error since that is
		// the current/expected name.
		return "", "", err
	}
	slog.Warn("resolved secret via legacy (pre hub-prefix) GCP SM name; run `scion hub secret migrate-names` to migrate",
		"name", name, "scope", scope, "legacy_name", legacyName)
	return value, legacyName, nil
}

// hasAccessibleVersion reports whether the given GCP SM secret ID currently
// has an accessible "latest" version. NotFound (no such secret) and
// FailedPrecondition (the secret container exists but its latest version is
// disabled or destroyed — "exists with no enabled version" in
// .design/secret-id-hub-refactor.md §7) both mean "not accessible" for
// migration purposes, not an error (ptone/scion#2152 review finding 12).
func (b *GCPBackend) hasAccessibleVersion(ctx context.Context, smName string) (bool, error) {
	_, err := b.accessLatestVersion(ctx, smName)
	if err == nil {
		return true, nil
	}
	switch status.Code(err) {
	case codes.NotFound, codes.FailedPrecondition:
		return false, nil
	default:
		return false, err
	}
}

// legacyReadErrIsFatal classifies an error from reading/checking the legacy
// GCP SM name into "nothing there, safe to treat as absent" (false) or a
// genuine failure that must be surfaced (true), given whether a DB record's
// ref still makes a live claim on the legacy name specifically
// (hasRecord && !refIsPrefixed — round-2 review finding 3: that one case must
// keep surfacing an unreadable legacy name as a real problem, since ref
// repair genuinely depends on reading it). Shared by migrationCheck,
// canDeleteLegacyName, and LegacyStillPresent so the three can't
// independently drift on how each GCP error code is interpreted
// (ptone/scion#2152 round-4 review finding 2): before this, canDeleteLegacyName
// treated a PermissionDenied/FailedPrecondition legacy read as fatal even for
// a no-DB-record candidate or one already fully migrated by ref, which left
// `migrate-names --delete-legacy` permanently failing for the known
// hub-scope signing keys once an operator narrowed IAM as ptone/scion#2180's
// rollout describes doing — while `--dry-run` (via migrationCheck) already
// treated the identical signal as "absent". Both now agree: NotFound and
// FailedPrecondition (an existing container with no enabled version) are
// never fatal, since nothing accessible exists under the name either way.
// PermissionDenied is fatal only when hasRecord && !refIsPrefixed; once a
// stored ref already designates the prefixed name (already migrated), or
// there's no ref making any claim about the legacy name at all, an
// unreadable legacy name is simply nothing further to do.
func legacyReadErrIsFatal(err error, hasRecord, refIsPrefixed bool) bool {
	switch status.Code(err) {
	case codes.NotFound, codes.FailedPrecondition:
		return false
	case codes.PermissionDenied:
		return hasRecord && !refIsPrefixed
	default:
		return true
	}
}

// migrationCheck classifies a secret identity's name-migration state in a
// single pass over GCP SM: whether the prefixed name is already accessible,
// and — only if it is not — whether the legacy name is accessible and, if so,
// its current value. NeedsNameMigration and MigrateNameForward both build on
// this so the legacy name is read at most once per call instead of twice
// (ptone/scion#2152 review nit 19). Both callers only ever run on the
// no-DB-record path, so legacyReadErrIsFatal is consulted with
// hasRecord=false.
//
// A PermissionDenied on the legacy name (distinct from NotFound: GCP
// evaluates IAM before existence) is treated the same as "legacy absent" —
// on a fresh hub whose service account only holds the new-prefix conditioned
// grant, there is no legacy secret this call could ever migrate, and
// treating the denial as a hard error would log a spurious warning on every
// boot (ptone/scion#2152 review finding 13).
func (b *GCPBackend) migrationCheck(ctx context.Context, name, scope, scopeID string) (prefixedOK bool, legacyValue string, legacyOK bool, err error) {
	prefixedName := b.gcpSecretName(name, scope, scopeID)
	prefixedOK, err = b.hasAccessibleVersion(ctx, prefixedName)
	if err != nil {
		return false, "", false, fmt.Errorf("failed to check prefixed secret %s: %w", prefixedName, err)
	}
	if prefixedOK {
		return true, "", false, nil
	}

	legacyName := b.legacyGCPSecretName(name, scope, scopeID)
	value, accessErr := b.accessLatestVersion(ctx, legacyName)
	if accessErr != nil {
		if legacyReadErrIsFatal(accessErr, false, false) {
			return false, "", false, fmt.Errorf("failed to check legacy secret %s: %w", legacyName, accessErr)
		}
		if status.Code(accessErr) == codes.PermissionDenied {
			slog.Debug("legacy GCP SM secret not accessible (permission denied); treating as absent",
				"name", name, "scope", scope, "legacy_name", fmt.Sprintf("projects/%s/secrets/%s", b.projectID, legacyName))
		}
		return false, "", false, nil
	}
	return false, value, true, nil
}

// NeedsNameMigration reports whether a secret identity's GCP SM value is only
// reachable under the legacy (pre-prefix) name — i.e. the hub-prefixed name
// has no accessible version yet, but the legacy name does. It performs reads
// only (no writes), so it is safe to use for --dry-run planning.
// Returns store.ErrNotFound if the secret exists under neither name (or the
// legacy name is not accessible to this hub's service account at all).
func (b *GCPBackend) NeedsNameMigration(ctx context.Context, name, scope, scopeID string) (bool, error) {
	prefixedOK, _, legacyOK, err := b.migrationCheck(ctx, name, scope, scopeID)
	if err != nil {
		return false, err
	}
	if prefixedOK {
		return false, nil
	}
	if !legacyOK {
		return false, store.ErrNotFound
	}
	return true, nil
}

// MigrateNameForward copies a secret identity's value from its legacy
// (pre-prefix) GCP SM name to the current hub-prefixed name, preserving GCP SM
// labels from the legacy secret. It is idempotent: if the prefixed name
// already has an accessible version, it returns (false, nil) without reading
// or modifying the legacy secret. If neither name has an accessible version,
// it returns store.ErrNotFound. The legacy secret is left in place — deleting
// it is the separate, explicit job of DeleteLegacySecretName (--delete-legacy).
func (b *GCPBackend) MigrateNameForward(ctx context.Context, name, scope, scopeID string) (migrated bool, err error) {
	prefixedOK, legacyValue, legacyOK, err := b.migrationCheck(ctx, name, scope, scopeID)
	if err != nil {
		return false, err
	}
	if prefixedOK {
		return false, nil
	}
	if !legacyOK {
		return false, store.ErrNotFound
	}

	legacyName := b.legacyGCPSecretName(name, scope, scopeID)
	legacyFull := fmt.Sprintf("projects/%s/secrets/%s", b.projectID, legacyName)
	labels, err := b.legacyLabels(ctx, legacyFull)
	if err != nil {
		return false, fmt.Errorf("failed to copy %s to prefixed GCP SM name: %w", name, err)
	}

	prefixedName := b.gcpSecretName(name, scope, scopeID)
	if err := b.ensureSecretAndAddVersion(ctx, prefixedName, []byte(legacyValue), labels); err != nil {
		return false, fmt.Errorf("failed to copy %s to prefixed GCP SM name: %w", name, err)
	}
	return true, nil
}

// legacyLabels fetches the GCP SM labels of the secret at legacyFull, so a
// copy-forward/resync preserves them instead of silently producing an
// unlabeled prefixed copy. Any error fetching them fails the caller (the
// copy is retried on the next boot/run) rather than proceeding with empty
// labels: external consumers that discover hub-scope secrets (e.g. signing
// keys) by label would otherwise be unable to find an unlabeled prefixed
// copy once the legacy one is deleted (ptone/scion#2152 round-2 review
// finding 12).
func (b *GCPBackend) legacyLabels(ctx context.Context, legacyFull string) (map[string]string, error) {
	legacySecret, err := b.smClient.GetSecret(ctx, &smpb.GetSecretRequest{Name: legacyFull})
	if err != nil {
		return nil, fmt.Errorf("failed to read labels at %s: %w", legacyFull, err)
	}
	return legacySecret.Labels, nil
}

// LegacyStillPresent reports whether a secret identity's legacy (pre-prefix)
// GCP SM name still has an accessible version. Used by migrate-names's
// --dry-run to simulate presence *after* a MIGRATE/RESYNC/REPAIR REF action
// that would already have run by this point in a real invocation (see
// planMigrateNamesCandidate's actionPlanned handling) — a purely
// presence-only question that never needs the ref-authority nuance
// canDeleteLegacyName applies, since the hypothetical action it's simulating
// would already have made the ref authoritative by then. legacyReadErrIsFatal
// is consulted with hasRecord=false so NotFound, FailedPrecondition, and
// PermissionDenied are all "nothing here" (ptone/scion#2152 round-4 review
// finding 2 — this used to surface PermissionDenied as a hard error via
// hasAccessibleVersion, inconsistently with canDeleteLegacyName's post-fix
// treatment of the same signal).
func (b *GCPBackend) LegacyStillPresent(ctx context.Context, name, scope, scopeID string) (bool, error) {
	legacyName := b.legacyGCPSecretName(name, scope, scopeID)
	_, err := b.accessLatestVersion(ctx, legacyName)
	if err == nil {
		return true, nil
	}
	if legacyReadErrIsFatal(err, false, false) {
		return false, fmt.Errorf("failed to check legacy secret %s: %w", legacyName, err)
	}
	return false, nil
}

// RefPointsAtPrefixed reports whether a DB record exists for the given secret
// identity and, if so, whether its SecretRef already points at the current
// hub-prefixed GCP SM name. hasRecord is false (with refIsPrefixed
// meaningless) when there is no DB row at all — e.g. a hub-scope signing key
// recovered directly from GCP SM without ever gaining one. Used to decide
// whether RepairRefToPrefixed has anything to do, and by DeleteLegacySecretName
// to refuse deleting the legacy secret while a DB record still depends on it.
func (b *GCPBackend) RefPointsAtPrefixed(ctx context.Context, name, scope, scopeID string) (hasRecord, refIsPrefixed bool, err error) {
	rec, err := b.store.GetSecret(ctx, name, scope, scopeID)
	if err != nil {
		if err == store.ErrNotFound {
			return false, false, nil
		}
		return false, false, err
	}
	prefixedName := b.gcpSecretName(name, scope, scopeID)
	prefixedRef := "gcpsm:" + fmt.Sprintf("projects/%s/secrets/%s", b.projectID, prefixedName)
	return true, rec.SecretRef == prefixedRef, nil
}

// DeleteLegacySecretName decides whether the legacy (pre-prefix) secret for
// the given identity is safe to delete, then deletes it. canDeleteLegacyName
// is the shared decision function: --dry-run (PlanLegacyDeletion) and a real
// run (this function) must reach the identical conclusion from the identical
// check (ptone/scion#2152 round-3 review finding 2), so a dry-run's report of
// what a real run would do is never a guess.
//
// The safety condition is *not* value equality with the legacy secret. Once
// a DB record's ref already points at the prefixed name, the prefixed copy
// is authoritative by definition — that is exactly what the ref means — so
// all that's required is that it have an accessible version; it is not a
// bug for a rotation performed after migration to have left the legacy copy
// stale, and refusing to delete a stale-but-superseded legacy copy would
// make --delete-legacy permanently unable to finish for any secret that was
// ever rotated post-migration. Value equality is still required for the
// no-DB-record path (a hub-scope signing key recovered directly from GCP SM
// with no ref to establish which copy is authoritative), where matching
// values is the only available evidence the prefixed copy is safe to treat
// as a full replacement. NotFound on the legacy secret is treated as success
// (already migrated/deleted).
//
// deleted reports whether this call actually performed the delete (true) or
// found nothing that needed deleting -- already gone, or not yet safe to
// remove (false, nil in both cases; the latter is a refusal only when err is
// also non-nil). This lets a caller that has already committed to deleting
// (the non-dry-run migrate-names path) act on the outcome without a second,
// redundant canDeleteLegacyName check via PlanLegacyDeletion (GoogleCloudPlatform/scion#2123
// review discussion_r4144099464 / discussion_r4144099474): every existing
// caller that only needs the error can keep ignoring the bool.
func (b *GCPBackend) DeleteLegacySecretName(ctx context.Context, name, scope, scopeID string) (deleted bool, err error) {
	legacyName := b.legacyGCPSecretName(name, scope, scopeID)
	legacyFull := fmt.Sprintf("projects/%s/secrets/%s", b.projectID, legacyName)

	ok, err := b.canDeleteLegacyName(ctx, name, scope, scopeID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}

	if err := b.smClient.DeleteSecret(ctx, &smpb.DeleteSecretRequest{Name: legacyFull}); err != nil && status.Code(err) != codes.NotFound {
		return false, fmt.Errorf("failed to delete legacy secret %s: %w", legacyFull, err)
	}
	return true, nil
}

// PlanLegacyDeletion is the read-only counterpart of DeleteLegacySecretName,
// used for --delete-legacy --dry-run: it runs the identical decision
// (canDeleteLegacyName) but never deletes anything.
func (b *GCPBackend) PlanLegacyDeletion(ctx context.Context, name, scope, scopeID string) (bool, error) {
	return b.canDeleteLegacyName(ctx, name, scope, scopeID)
}

// canDeleteLegacyName is the shared decision behind DeleteLegacySecretName
// and PlanLegacyDeletion (ptone/scion#2152 round-3 review finding 2). It
// returns (true, nil) when the legacy secret exists and is safe to delete,
// (false, nil) when there is nothing to delete (the legacy secret is already
// gone, or unreadable in a way legacyReadErrIsFatal says isn't a genuine
// problem for this record), and a non-nil error when deletion would be
// unsafe or a check failed for a reason that must be surfaced.
//
// The legacy read runs *before* the "not yet repaired" refusal, and its
// error (if any) is classified by legacyReadErrIsFatal, which already knows
// how to weigh hasRecord/refIsPrefixed (ptone/scion#2152 round-5 review
// finding 1, a regression from round 4's reordering): an ORPHAN record (a
// stored ref whose target is gone) or one with no ref and no legacy value
// either has nothing accessible under the legacy name at all, so
// legacyReadErrIsFatal(NotFound, ...) correctly says "nothing to delete" —
// but only if that NotFound is actually observed, which requires reading the
// legacy name first. Refusing "not yet repaired" before checking whether the
// legacy name even has anything to protect made that refusal permanent and
// unconditional for exactly the records ptone/scion#2152 round-3/4 review
// findings 4 and the ORPHAN classification exist to let through. The refusal
// still fires — now correctly gated on the legacy read having found a real,
// live value only a repair could safely supersede — for a genuinely
// unmigrated record whose only copy is the legacy one, and
// legacyReadErrIsFatal's PermissionDenied-when-unrepaired arm (round-2
// finding 3) is what makes an unreadable legacy name fatal in that specific
// case.
func (b *GCPBackend) canDeleteLegacyName(ctx context.Context, name, scope, scopeID string) (bool, error) {
	legacyName := b.legacyGCPSecretName(name, scope, scopeID)
	legacyFull := fmt.Sprintf("projects/%s/secrets/%s", b.projectID, legacyName)
	prefixedName := b.gcpSecretName(name, scope, scopeID)

	hasRecord, refIsPrefixed, err := b.RefPointsAtPrefixed(ctx, name, scope, scopeID)
	if err != nil {
		return false, fmt.Errorf("failed to check DB record before deleting legacy secret %s: %w", legacyFull, err)
	}

	legacyValue, err := b.accessLatestVersion(ctx, legacyName)
	if err != nil {
		if legacyReadErrIsFatal(err, hasRecord, refIsPrefixed) {
			return false, fmt.Errorf("failed to read legacy secret %s: %w", legacyFull, err)
		}
		return false, nil
	}

	// The legacy name is readable, so there is a real value that deleting it
	// would destroy. Only now does an unrepaired ref matter: an ORPHAN or
	// no-ref record never reaches this line, since its legacy read above
	// returned NotFound.
	if hasRecord && !refIsPrefixed {
		return false, fmt.Errorf("refusing to delete legacy secret %s: its DB record's SecretRef does not yet designate the prefixed name %s; run migrate-names without --delete-legacy first, or inspect the record directly if it is reported as ORPHAN", legacyFull, prefixedName)
	}

	if hasRecord && refIsPrefixed {
		// The DB ref already designates the prefixed name as authoritative:
		// all that's required is that it actually be readable. Its value is
		// not compared against the legacy value — a rotation performed after
		// migration legitimately leaves the legacy copy stale, and that is
		// exactly the case --delete-legacy exists to clean up.
		if _, err := b.accessLatestVersion(ctx, prefixedName); err != nil {
			return false, fmt.Errorf("refusing to delete legacy secret %s: prefixed copy %s is not accessible: %w", legacyFull, prefixedName, err)
		}
		return true, nil
	}

	// No DB record at all: fall back to value equality as the only available
	// evidence that the prefixed copy is a safe, complete replacement.
	prefixedValue, err := b.accessLatestVersion(ctx, prefixedName)
	if err != nil {
		return false, fmt.Errorf("refusing to delete legacy secret %s: prefixed copy %s is not accessible: %w", legacyFull, prefixedName, err)
	}
	if prefixedValue != legacyValue {
		return false, fmt.Errorf("refusing to delete legacy secret %s: prefixed copy %s value does not match", legacyFull, prefixedName)
	}
	return true, nil
}

// ErrOrphanedRef indicates a secret's DB record has a stored SecretRef, but
// the value it designates has no accessible version (ptone/scion#2152
// round-3 review finding 4; wording corrected in round-4 nit 9 — this is
// returned based on the ref-designated path alone, without also checking the
// prefixed name). This is distinct from
// store.ErrNotFound (no DB record and no computed-legacy-name value either —
// truly nothing to migrate): here the record itself exists and makes a
// specific authority claim about where its value lives, and that claim can no
// longer be honored. migrate-names reports this as a skipped ORPHAN, visible
// to an operator, rather than a silent no-op or a hard failure — there is
// nothing for it to copy.
var ErrOrphanedRef = errors.New("secret record's SecretRef designates no accessible GCP SM value")

// ErrConflictingWrite indicates that planOrRepairRef wrote a version to the
// prefixed name during this attempt, but the subsequent ref-update CAS then
// found the record's (SecretRef, Version) had already changed — a
// concurrent writer's ref update won the race after this attempt's write
// landed (ptone/scion#2152 round-5 review finding 2). The prefixed name's
// latest version may now be the value this attempt just wrote rather than
// the concurrent writer's, with no cheap, mechanism-free way to tell which;
// this is surfaced rather than retried so an operator can verify or re-set
// the secret. See planOrRepairRefAttempt's CAS-refused-after-write handling
// and .design/secret-id-hub-refactor.md §7 for the full explanation.
var ErrConflictingWrite = errors.New("a concurrent write to the prefixed secret was detected after this attempt already wrote a version to it; verify or re-set the secret")

// maxPlanOrRepairRefAttempts bounds the retry loop in planOrRepairRef (see
// its doc comment for why a retry is needed at all).
const maxPlanOrRepairRefAttempts = 3

// RefRepairAction identifies which action planOrRepairRef determined was
// needed (or took) for one secret identity (ptone/scion#2152 round-3 review
// nit 13). Typed rather than a bare string so a typo in a comparison or a
// switch case fails to compile instead of silently matching nothing.
type RefRepairAction string

const (
	// RefRepairNone means there was nothing to do.
	RefRepairNone RefRepairAction = ""
	// RefRepairCopied means the prefixed name didn't exist and was created
	// with the ref-designated value.
	RefRepairCopied RefRepairAction = "copied"
	// RefRepairResynced means the prefixed name existed with a value
	// different from the ref-designated one and was overwritten.
	RefRepairResynced RefRepairAction = "resynced"
	// RefRepairRepaired means the prefixed name already held the correct
	// value and only the DB ref itself needed to move.
	RefRepairRepaired RefRepairAction = "repaired"
)

// planOrRepairRef is the shared implementation behind RepairRefToPrefixed
// (write=true) and PlanRefRepair (write=false).
//
// A DB record's SecretRef is the source of truth for which GCP SM copy is
// currently authoritative (ptone/scion#2152 round-2 review finding 1): if the
// ref is not the prefixed name, whatever it does point at — the legacy name,
// in every case this codebase produces today — holds the value that must
// win. This reads that value and makes sure the prefixed name carries it
// exactly, adding a new version if the prefixed name is missing *or* if it
// already exists with a *different* value, before ever repointing the ref.
// This is what makes it safe to call unconditionally any time the prefixed
// copy might be stale relative to the ref — a mixed-version rolling deploy
// where an old replica rotates the secret through the legacy name after a
// new replica already created a prefixed copy, or a rollback-then-
// roll-forward window — not just the first-ever migration.
//
// Concurrency (ptone/scion#2152 round-3 review finding 3; hardened further by
// round-4 finding 1): GCP Secret Manager has no conditional AddSecretVersion,
// so a write racing between this reading the authoritative value and writing
// it to the prefixed name cannot be prevented by the GCP SM API alone.
// Immediately before acting — including on the RefRepairRepaired path, which
// performs no GCP write at all — this re-reads the DB record and compares
// (SecretRef, Version) against what it read at the start of the attempt; if
// either changed, something else — a concurrent Set(), or an old-binary
// rotation that re-upserts the exact same ref string but still bumps Version
// — has already moved the authoritative value out from under this attempt.
// The ref-update CAS (store.UpdateSecretRefIfMatches) is ALSO keyed on that
// same (SecretRef, Version) pair, not the ref alone, so a rotation landing
// during the labels/AddSecretVersion RPCs between the recheck and the CAS is
// caught too: the version predicate fails to match and the CAS reports
// applied=false. When that happens and this attempt had NOT yet written to
// the prefixed name (the RefRepairRepaired path), it's cheap to just retry
// with fresh reads. When this attempt HAD already written a version to the
// prefixed name before the CAS refused, a plain retry would instead read the
// concurrent writer's now-current ref and conclude "nothing to do" — see the
// residual-window paragraph below for why that's the wrong thing to do
// silently. That specific case is reported as ErrConflictingWrite (logged at
// WARN, ptone/scion#2152 round-5 review finding 2) instead of retried, up to
// maxPlanOrRepairRefAttempts times for every other case.
//
// What this does and does not guarantee, precisely (ptone/scion#2152 round-6
// review findings 1 and 2 — round 5 overclaimed this as a single, uniform
// guarantee, and never actually updated .design/secret-id-hub-refactor.md §7
// to say so): whether a concurrent writer's own AddSecretVersion to the
// *prefixed* name (GCP Secret Manager has no compare-and-swap on which
// version becomes latest) is caught depends entirely on when that writer's
// own DB upsert lands relative to THIS attempt's recheck and CAS above, not
// on when its GCP write landed:
//   - If the concurrent writer's DB upsert lands between this attempt's
//     recheck and its CAS (or is otherwise visible in the DB row at CAS
//     time), the CAS's version predicate fails to match, applied=false, and
//     — if this attempt had already written a version to the prefixed name —
//     that is reported as ErrConflictingWrite (logged at WARN) rather than
//     silently retried. This is the case ErrConflictingWrite exists for.
//   - If the concurrent writer's DB upsert lands AFTER this attempt's CAS
//     has already applied, nothing detects it: the CAS already succeeded
//     against the (now stale) values this attempt read, no recheck or CAS
//     runs again, and this attempt's stale value can be left as the
//     prefixed name's latest version — silently, with no error and no log —
//     until the key is next written. This is not "brief": nothing about
//     this design repairs it on its own, and it persists indefinitely until
//     some other write touches the same key. TestSPREV6_
//     SetUpsertAfterCASIsUndetectedLostUpdate pins this exact interleaving.
//
// In this codebase only a new-binary Set() writes the prefixed name
// directly, so either interleaving requires a concurrent Set() (or another
// replica's boot copy-forward) racing with this call on the same key; see
// .design/secret-id-hub-refactor.md §7 for the same statement in operator
// terms, including what to do about it (re-set the secret directly —
// re-running migrate-names does not detect or repair either case, since by
// then the ref already matches whatever this attempt or the concurrent
// writer left behind).
//
// Returns:
//   - ("", nil) if there is nothing to do: no DB record, the ref already
//     points at the prefixed name, or a concurrent write raced ahead of the
//     ref update.
//   - ("copied", nil) if the prefixed name didn't exist and was created with
//     the ref-designated value.
//   - ("resynced", nil) if the prefixed name existed with a different value
//     than the ref-designated one and was overwritten with a new version.
//   - ("repaired", nil) if the prefixed name already held the correct value
//     and only the ref itself needed to move.
//   - ("", store.ErrNotFound) if there is a DB record but no ref, and the
//     computed legacy name has no accessible value either — nothing to
//     migrate for this identity (round-3 finding 4).
//   - ("", ErrOrphanedRef) if there is a DB record with a *stored* ref, but
//     the value it designates is gone (round-3 finding 4) — a distinct,
//     reportable condition, not a failure.
//   - an error, with no side effects, if the ref-designated value could not
//     be read for any other reason (including PermissionDenied on a stored
//     ref — round-2 finding 3 deliberately does *not* soften this the way
//     the no-DB-record path does, since a record whose ref depends on an
//     unreadable secret is a real problem to surface, not something to
//     silently skip), or if concurrent writes kept invalidating every
//     attempt.
func (b *GCPBackend) planOrRepairRef(ctx context.Context, name, scope, scopeID string, write bool) (action RefRepairAction, err error) {
	for attempt := 0; attempt < maxPlanOrRepairRefAttempts; attempt++ {
		action, retry, err := b.planOrRepairRefAttempt(ctx, name, scope, scopeID, write)
		if !retry {
			return action, err
		}
	}
	return "", fmt.Errorf("failed to migrate/resync %s (scope %s/%s): concurrent writes kept changing the authoritative value across %d attempts; re-run later", name, scope, scopeID, maxPlanOrRepairRefAttempts)
}

func (b *GCPBackend) planOrRepairRefAttempt(ctx context.Context, name, scope, scopeID string, write bool) (action RefRepairAction, retry bool, err error) {
	rec, err := b.store.GetSecret(ctx, name, scope, scopeID)
	if err != nil {
		if err == store.ErrNotFound {
			return "", false, nil
		}
		return "", false, err
	}

	prefixedName := b.gcpSecretName(name, scope, scopeID)
	prefixedFull := fmt.Sprintf("projects/%s/secrets/%s", b.projectID, prefixedName)
	prefixedRef := "gcpsm:" + prefixedFull
	if rec.SecretRef == prefixedRef {
		return "", false, nil
	}

	// A stored ref designates the authoritative source directly. A record
	// with no ref at all — it predates SecretRef being persisted, or was
	// never migrated under any prior naming scheme — carries no more
	// authority than having no DB record at all, so this falls back to the
	// computed legacy name, mirroring Get()'s existing DB-less-recovery
	// convention.
	refPath, hasStoredRef := extractGCPSMPath(rec.SecretRef)
	if !hasStoredRef {
		legacyName := b.legacyGCPSecretName(name, scope, scopeID)
		refPath = fmt.Sprintf("projects/%s/secrets/%s", b.projectID, legacyName)
	}
	authoritativeValue, err := b.accessLatestVersionByPath(ctx, refPath)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			if hasStoredRef {
				// The record makes a specific authority claim (its stored
				// ref) that no longer resolves to anything. Distinct from
				// "nothing to migrate" — surface it (round-3 finding 4).
				return "", false, ErrOrphanedRef
			}
			return "", false, store.ErrNotFound
		}
		if !hasStoredRef && status.Code(err) == codes.PermissionDenied {
			// No stored ref to begin with, so this claims no more than the
			// no-DB-record path does: treat identically (round-2 review
			// finding 3's rule only applies once a ref makes a real
			// authority claim about where the value lives).
			return "", false, store.ErrNotFound
		}
		return "", false, fmt.Errorf("failed to read authoritative value at %s: %w", refPath, err)
	}

	prefixedValue, prefixedErr := b.accessLatestVersion(ctx, prefixedName)
	prefixedOK := prefixedErr == nil
	if prefixedErr != nil && status.Code(prefixedErr) != codes.NotFound && status.Code(prefixedErr) != codes.FailedPrecondition {
		return "", false, fmt.Errorf("failed to check prefixed secret %s: %w", prefixedFull, prefixedErr)
	}

	switch {
	case !prefixedOK:
		action = RefRepairCopied
	case prefixedValue != authoritativeValue:
		action = RefRepairResynced
	default:
		action = RefRepairRepaired
	}

	if !write {
		return action, false, nil
	}

	// Recheck immediately before the ref update: if the record's ref or
	// Version has moved since the read above, a concurrent Set() or an
	// old-binary rotation already changed the authoritative value (or
	// re-asserted the same ref with a new value — Version still bumps on
	// every UpsertSecret, so this catches that ABA case too). This applies
	// even when action is RefRepairRepaired and no GCP write happens this
	// attempt at all: repointing the ref past a rotation this attempt never
	// saw would silently make the rotation invisible just the same as
	// writing a stale value would (ptone/scion#2152 round-4 review finding
	// 1(a) — the repaired path previously skipped this check entirely).
	// Discard the attempt and retry with fresh reads rather than act on
	// stale information.
	current, err := b.store.GetSecret(ctx, name, scope, scopeID)
	if err != nil {
		return "", false, err
	}
	if current.SecretRef != rec.SecretRef || current.Version != rec.Version {
		return "", true, nil
	}

	if action != RefRepairRepaired {
		labels, err := b.legacyLabels(ctx, refPath)
		if err != nil {
			return "", false, fmt.Errorf("failed to sync %s to prefixed GCP SM name: %w", name, err)
		}
		if err := b.ensureSecretAndAddVersion(ctx, prefixedName, []byte(authoritativeValue), labels); err != nil {
			return "", false, fmt.Errorf("failed to sync %s to prefixed GCP SM name: %w", name, err)
		}
	}

	// The CAS itself is also version-aware (matching both SecretRef and the
	// Version captured at the start of THIS attempt, not just the recheck
	// above): any write that lands between the recheck and this call —
	// including during the legacyLabels/ensureSecretAndAddVersion RPCs above
	// — bumps Version and makes the predicate fail to match, so applied=false
	// and this attempt is retried rather than repointing the ref past a
	// change it never accounted for (ptone/scion#2152 round-4 review finding
	// 1(b)).
	applied, err := b.store.UpdateSecretRefIfMatches(ctx, name, scope, scopeID, rec.SecretRef, rec.Version, prefixedRef)
	if err != nil {
		return "", false, fmt.Errorf("failed to update DB ref: %w", err)
	}
	if !applied {
		if action != RefRepairRepaired {
			// We already wrote a version to the prefixed name this attempt,
			// and the CAS then found the ref/Version had moved: a concurrent
			// writer's ref update won the race after our write landed. A
			// plain retry would now read rec.SecretRef == prefixedRef and
			// return "nothing to do", silently accepting whatever the
			// prefixed name's latest version happens to be — which may be
			// the stale value we just wrote, permanently, with nobody told
			// (ptone/scion#2152 round-5 review finding 2). Report it instead
			// of retrying: this can't be resolved automatically without a
			// mechanism this fix deliberately doesn't add, so it's surfaced
			// for a human to verify or re-set the secret. No secret value or
			// full GCP resource path is logged, only the scion identity.
			slog.Warn("secret ref update lost a race after this attempt already wrote a version to the prefixed name; its latest version may be stale and needs manual verification",
				"name", name, "scope", scope, "scope_id", scopeID)
			return "", false, ErrConflictingWrite
		}
		// The repaired path performs no GCP write, so a lost race here just
		// means someone else already made some change first: retry with
		// fresh reads rather than claiming an action that may already be
		// stale.
		return "", true, nil
	}
	return action, false, nil
}

// RepairRefToPrefixed makes the DB record's SecretRef authoritative-safe (see
// planOrRepairRef) and points it at the prefixed name. See planOrRepairRef
// for the full contract and the RefRepairAction values it can return.
func (b *GCPBackend) RepairRefToPrefixed(ctx context.Context, name, scope, scopeID string) (action RefRepairAction, err error) {
	return b.planOrRepairRef(ctx, name, scope, scopeID, true)
}

// PlanRefRepair is the read-only counterpart of RepairRefToPrefixed, used for
// --dry-run planning: it performs the identical checks (including reading
// both the ref-designated and prefixed values to determine whether a resync
// would be needed) but never writes to GCP SM or the DB.
func (b *GCPBackend) PlanRefRepair(ctx context.Context, name, scope, scopeID string) (action RefRepairAction, err error) {
	return b.planOrRepairRef(ctx, name, scope, scopeID, false)
}

// CopyHubSecretForward implements the hub-startup copy-forward for hub-scope
// infrastructure secrets (signing keys). Per .design/secret-id-hub-refactor.md
// §7: "read the legacy name and write the same value to the prefixed name,
// then use it."
//
// If a DB record already exists for this identity, its SecretRef is treated
// as authoritative (RepairRefToPrefixed — see there for why this must never
// blindly repoint at a stale prefixed copy, ptone/scion#2152 round-2 review
// finding 1). If no DB record exists at all — e.g. a hub-scope signing key
// recovered directly from GCP SM without ever gaining one — there is no ref
// to consult, so this falls back to the presence-based MigrateNameForward
// (copy only if the prefixed name doesn't exist yet); that residual gap (no
// authority signal to detect a stale prefixed copy in this case) is
// documented, not fixed, in .design/secret-id-hub-refactor.md §7.
//
// Called at ensureSigningKey/OIDC-key-manager time, without waiting for an
// operator to run `migrate-names`. This matters specifically for signing
// keys (unlike ordinary secrets) because losing them invalidates every live
// session/agent token; ordinary secrets are already backward compatible via
// the stored SecretRef and are migrated on the operator's schedule via
// `migrate-names`.
// Returns store.ErrNotFound when there is nothing to copy (neither name
// exists yet, e.g. first boot, or the legacy name is not accessible to this
// hub's service account at all, and there is no DB record) — callers should
// treat that as "proceed with normal key resolution", not a failure.
func (b *GCPBackend) CopyHubSecretForward(ctx context.Context, name string) error {
	hasRecord, refIsPrefixed, err := b.RefPointsAtPrefixed(ctx, name, store.ScopeHub, b.hubID)
	if err != nil {
		return err
	}
	if hasRecord {
		if refIsPrefixed {
			return nil
		}
		_, err := b.RepairRefToPrefixed(ctx, name, store.ScopeHub, b.hubID)
		return err
	}
	_, err = b.MigrateNameForward(ctx, name, store.ScopeHub, b.hubID)
	return err
}

// sanitizeSecretID ensures the string is a valid GCP SM secret ID.
// Secret IDs must match [a-zA-Z0-9_-] and be 1-255 chars.
var invalidSecretIDChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func sanitizeSecretID(s string) string {
	s = invalidSecretIDChars.ReplaceAllString(s, "-")
	if len(s) > 255 {
		s = s[:255]
	}
	return s
}

// sanitizeLabel ensures a GCP label value is valid.
// Label values must match [a-z0-9_-] and be at most 63 chars.
func sanitizeLabel(s string) string {
	s = strings.ToLower(s)
	s = invalidSecretIDChars.ReplaceAllString(s, "-")
	if len(s) > 63 {
		s = s[:63]
	}
	return s
}

// buildLabels constructs the GCP SM labels map for a secret.
// For user-scoped secrets with a known email, a scion-userid label is added.
// The scion-hub-name label allows filtering secrets by hub in the GCP console.
func buildLabels(input *SetSecretInput, target, hubName string) map[string]string {
	labels := map[string]string{
		"scion-scope":    sanitizeLabel(input.Scope),
		"scion-scope-id": sanitizeLabel(input.ScopeID),
		"scion-type":     sanitizeLabel(input.SecretType),
		"scion-name":     sanitizeLabel(input.Name),
		"scion-target":   sanitizeLabel(target),
		"scion-hub-name": sanitizeLabel(hubName),
	}
	if input.Scope == ScopeUser && input.UserEmail != "" {
		labels["scion-userid"] = sanitizeLabel(input.UserEmail)
	}
	return labels
}

// wrapGCPError checks whether err is a gRPC PermissionDenied error and returns
// a *PermissionError so that HTTP handlers can return 403 instead of 500. The
// returned error carries this backend's hub prefix and project ID so
// PermissionError.Error() can suggest a least-privilege, hub-scoped
// conditioned IAM grant (ptone/scion#2152) instead of only the broad
// project-wide role. Returns nil for all other error codes, enabling the
// idiomatic "if permErr := b.wrapGCPError(...); permErr != nil" pattern.
func (b *GCPBackend) wrapGCPError(err error, operation string) error {
	if status.Code(err) == codes.PermissionDenied {
		return &PermissionError{
			Operation: operation,
			Err:       err,
			HubPrefix: b.secretNamePrefix(),
			ProjectID: b.projectID,
		}
	}
	return nil
}

// resolveHubName returns the hub display name if set, falling back to the machine hostname.
func (b *GCPBackend) resolveHubName() string {
	b.mu.RLock()
	name := b.hubName
	b.mu.RUnlock()
	if name != "" {
		return name
	}
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
