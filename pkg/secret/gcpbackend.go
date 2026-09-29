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
// name when the Hub database record is missing: a missing or mismatched
// record is reported as store.ErrNotFound for that item.
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
		smName := b.gcpSecretName(s.Key, s.Scope, s.ScopeID)
		value, err = b.accessLatestVersion(ctx, smName)
	}
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return FetchResult{Err: store.ErrNotFound}
		}
		if permErr := wrapGCPError(err, "access secret"); permErr != nil {
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
			if err != nil {
				if permErr := b.wrapGCPError(err, "create secret"); permErr != nil {
					return permErr
				}
				return fmt.Errorf("failed to create GCP SM secret: %w", err)
			}
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
	// Delete from GCP SM under both the current hub-prefixed name and the
	// legacy pre-prefix name (NotFound on either is fine — a secret created
	// before ptone/scion#2152, or one already migrated, will only exist under
	// one of the two).
	for _, smName := range []string{b.gcpSecretName(name, scope, scopeID), b.legacyGCPSecretName(name, scope, scopeID)} {
		fullName := fmt.Sprintf("projects/%s/secrets/%s", b.projectID, smName)
		err := b.smClient.DeleteSecret(ctx, &smpb.DeleteSecretRequest{
			Name: fullName,
		})
		if err != nil && status.Code(err) != codes.NotFound {
			if permErr := b.wrapGCPError(err, "delete secret"); permErr != nil {
				return permErr
			}
			return fmt.Errorf("failed to delete GCP SM secret %s: %w", smName, err)
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
	hash := sha256.Sum256([]byte(b.hubID))
	return "scion-" + hex.EncodeToString(hash[:6]) + "-" // 6 bytes = 12 hex chars
}

// gcpSecretName builds a sanitized GCP SM secret ID from the scion secret
// identity, using the current (hub-prefixed) naming scheme.
// Format: {secretNamePrefix()}{scope}-{sha256(hubID:scopeID)[:12]}-{name}
// The hubID is combined with the scopeID before hashing to ensure uniqueness
// across hub instances sharing the same GCP project, independent of the
// hub-prefix segment (which only depends on hubID).
func (b *GCPBackend) gcpSecretName(name, scope, scopeID string) string {
	combined := b.hubID + ":" + scopeID
	hash := sha256.Sum256([]byte(combined))
	shortHash := hex.EncodeToString(hash[:6]) // 6 bytes = 12 hex chars
	return sanitizeSecretID(fmt.Sprintf("%s%s-%s-%s", b.secretNamePrefix(), scope, shortHash, name))
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

// NeedsNameMigration reports whether a secret identity's GCP SM value is only
// reachable under the legacy (pre-prefix) name — i.e. the hub-prefixed name
// has no accessible version yet, but the legacy name does. It performs reads
// only (no writes), so it is safe to use for --dry-run planning.
// Returns store.ErrNotFound if the secret exists under neither name.
func (b *GCPBackend) NeedsNameMigration(ctx context.Context, name, scope, scopeID string) (bool, error) {
	prefixedName := b.gcpSecretName(name, scope, scopeID)
	if _, err := b.accessLatestVersion(ctx, prefixedName); err == nil {
		return false, nil
	} else if status.Code(err) != codes.NotFound {
		return false, fmt.Errorf("failed to check prefixed secret %s: %w", prefixedName, err)
	}

	legacyName := b.legacyGCPSecretName(name, scope, scopeID)
	if _, err := b.accessLatestVersion(ctx, legacyName); err != nil {
		if status.Code(err) == codes.NotFound {
			return false, store.ErrNotFound
		}
		return false, fmt.Errorf("failed to check legacy secret %s: %w", legacyName, err)
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
	needsMigration, err := b.NeedsNameMigration(ctx, name, scope, scopeID)
	if err != nil {
		return false, err
	}
	if !needsMigration {
		return false, nil
	}

	legacyName := b.legacyGCPSecretName(name, scope, scopeID)
	legacyFull := fmt.Sprintf("projects/%s/secrets/%s", b.projectID, legacyName)
	value, err := b.accessLatestVersion(ctx, legacyName)
	if err != nil {
		return false, fmt.Errorf("failed to read legacy secret %s: %w", legacyName, err)
	}

	var labels map[string]string
	if legacySecret, gerr := b.smClient.GetSecret(ctx, &smpb.GetSecretRequest{Name: legacyFull}); gerr == nil {
		labels = legacySecret.Labels
	}

	prefixedName := b.gcpSecretName(name, scope, scopeID)
	if err := b.ensureSecretAndAddVersion(ctx, prefixedName, []byte(value), labels); err != nil {
		return false, fmt.Errorf("failed to copy %s to prefixed GCP SM name: %w", name, err)
	}
	return true, nil
}

// DeleteLegacySecretName verifies that the hub-prefixed secret for the given
// identity has an accessible version whose value matches the legacy secret's
// current value, then deletes the legacy secret. It refuses to delete if the
// prefixed copy is missing or its value does not match, so --delete-legacy can
// never drop the only copy of a secret. NotFound on the legacy secret is
// treated as success (already migrated/deleted).
func (b *GCPBackend) DeleteLegacySecretName(ctx context.Context, name, scope, scopeID string) error {
	legacyName := b.legacyGCPSecretName(name, scope, scopeID)
	legacyFull := fmt.Sprintf("projects/%s/secrets/%s", b.projectID, legacyName)

	legacyValue, err := b.accessLatestVersion(ctx, legacyName)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil
		}
		return fmt.Errorf("failed to read legacy secret %s: %w", legacyName, err)
	}

	prefixedName := b.gcpSecretName(name, scope, scopeID)
	prefixedValue, err := b.accessLatestVersion(ctx, prefixedName)
	if err != nil {
		return fmt.Errorf("refusing to delete legacy secret %s: prefixed copy %s is not accessible: %w", legacyName, prefixedName, err)
	}
	if prefixedValue != legacyValue {
		return fmt.Errorf("refusing to delete legacy secret %s: prefixed copy %s value does not match", legacyName, prefixedName)
	}

	if err := b.smClient.DeleteSecret(ctx, &smpb.DeleteSecretRequest{Name: legacyFull}); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("failed to delete legacy secret %s: %w", legacyName, err)
	}
	return nil
}

// UpdateSecretRefToPrefixed points a secret's stored DB SecretRef at its
// current hub-prefixed GCP SM name. It is a no-op (returns nil) if no DB
// record exists for the identity — e.g. a hub-scope signing key recovered
// directly from GCP SM without ever gaining a DB row.
func (b *GCPBackend) UpdateSecretRefToPrefixed(ctx context.Context, name, scope, scopeID string) error {
	rec, err := b.store.GetSecret(ctx, name, scope, scopeID)
	if err != nil {
		if err == store.ErrNotFound {
			return nil
		}
		return err
	}
	prefixedName := b.gcpSecretName(name, scope, scopeID)
	rec.SecretRef = "gcpsm:" + fmt.Sprintf("projects/%s/secrets/%s", b.projectID, prefixedName)
	return b.store.UpdateSecret(ctx, rec)
}

// CopyHubSecretForward implements the hub-startup copy-forward for hub-scope
// infrastructure secrets (signing keys): it is a thin wrapper around
// MigrateNameForward fixing scope=hub, scopeID=b.hubID. Called at
// ensureSigningKey time so that a signing key created before ptone/scion#2152
// becomes available under the hub-prefixed name — with the same key
// material — without waiting for an operator to run `migrate-names`. This
// matters specifically for signing keys (unlike ordinary secrets) because
// losing them invalidates every live session/agent token; ordinary secrets
// are already backward compatible via the stored SecretRef and are migrated
// on the operator's schedule via `migrate-names`.
// Returns store.ErrNotFound when there is nothing to copy (neither name
// exists yet, e.g. first boot) — callers should treat that as "proceed with
// normal key resolution", not a failure.
func (b *GCPBackend) CopyHubSecretForward(ctx context.Context, name string) error {
	_, err := b.MigrateNameForward(ctx, name, store.ScopeHub, b.hubID)
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
