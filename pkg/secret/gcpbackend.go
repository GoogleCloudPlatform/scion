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

	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// GCPBackend implements SecretBackend using a hybrid approach:
// metadata is stored in the Hub database, values are stored in GCP Secret Manager.
type GCPBackend struct {
	store     store.SecretStore
	smClient  SMClient
	projectID string
	hubID     string
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
		store:     s,
		smClient:  smClient,
		projectID: cfg.ProjectID,
		hubID:     hubID,
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
			smName := b.gcpSecretName(name, scope, scopeID)
			value, err = b.accessLatestVersion(ctx, smName)
		}
	} else {
		// No DB record; try GCP SM directly by computed name to handle
		// database resets where the secret still exists in GCP SM.
		smName := b.gcpSecretName(name, scope, scopeID)
		value, err = b.accessLatestVersion(ctx, smName)
		if err != nil {
			if status.Code(err) == codes.NotFound {
				return nil, store.ErrNotFound
			}
		} else {
			slog.Info("Recovered secret from GCP SM without DB record", "name", name, "scope", scope)
		}
	}
	if err != nil {
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

func (b *GCPBackend) Set(ctx context.Context, input *SetSecretInput) (bool, *SecretMeta, error) {
	smName := b.gcpSecretName(input.Name, input.Scope, input.ScopeID)
	fullName := fmt.Sprintf("projects/%s/secrets/%s", b.projectID, smName)

	target := input.Target
	if target == "" {
		target = input.Name
	}

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
					Replication: &smpb.Replication{
						Replication: &smpb.Replication_Automatic_{
							Automatic: &smpb.Replication_Automatic{},
						},
					},
					Labels: buildLabels(input, target, hostname()),
				},
			})
			if err != nil {
				return false, nil, fmt.Errorf("failed to create GCP SM secret: %w", err)
			}
		} else {
			return false, nil, fmt.Errorf("failed to check GCP SM secret: %w", err)
		}
	}

	// Add a new version with the secret value
	_, err = b.smClient.AddSecretVersion(ctx, &smpb.AddSecretVersionRequest{
		Parent: fullName,
		Payload: &smpb.SecretPayload{
			Data: []byte(input.Value),
		},
	})
	if err != nil {
		return false, nil, fmt.Errorf("failed to add GCP SM secret version: %w", err)
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

func (b *GCPBackend) Delete(ctx context.Context, name, scope, scopeID string) error {
	smName := b.gcpSecretName(name, scope, scopeID)
	fullName := fmt.Sprintf("projects/%s/secrets/%s", b.projectID, smName)

	// Delete from GCP SM (ignore NotFound)
	err := b.smClient.DeleteSecret(ctx, &smpb.DeleteSecretRequest{
		Name: fullName,
	})
	if err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("failed to delete GCP SM secret: %w", err)
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

	var scopes []scopeEntry
	// Hub scope is always included as lowest precedence
	scopes = append(scopes, scopeEntry{scope: store.ScopeHub, scopeID: b.hubID})
	if userID != "" {
		scopes = append(scopes, scopeEntry{scope: store.ScopeUser, scopeID: userID})
	}
	if projectID != "" {
		scopes = append(scopes, scopeEntry{scope: store.ScopeProject, scopeID: projectID})
	}
	if brokerID != "" {
		scopes = append(scopes, scopeEntry{scope: store.ScopeRuntimeBroker, scopeID: brokerID})
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

			smName := b.gcpSecretName(s.Key, sc.scope, sc.scopeID)
			value, err := b.accessLatestVersion(ctx, smName)
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
					SecretRef:     fmt.Sprintf("projects/%s/secrets/%s", b.projectID, smName),
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

			if opts.AuthzCheck != nil && !opts.AuthzCheck(*meta) {
				continue
			}

			// Read value from GCP SM
			smName := b.gcpSecretName(s.Key, s.Scope, s.ScopeID)
			value, err := b.accessLatestVersion(ctx, smName)
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
					SecretRef:     fmt.Sprintf("projects/%s/secrets/%s", b.projectID, smName),
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

// gcpSecretName builds a sanitized GCP SM secret ID from the scion secret identity.
// Format: scion-{scope}-{sha256(hubID:scopeID)[:12]}-{name}
// The hubID is combined with the scopeID before hashing to ensure uniqueness
// across hub instances sharing the same GCP project.
func (b *GCPBackend) gcpSecretName(name, scope, scopeID string) string {
	combined := b.hubID + ":" + scopeID
	hash := sha256.Sum256([]byte(combined))
	shortHash := hex.EncodeToString(hash[:6]) // 6 bytes = 12 hex chars
	return sanitizeSecretID(fmt.Sprintf("scion-%s-%s-%s", scope, shortHash, name))
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
// The hubHostname label allows filtering secrets by hub in the GCP console.
func buildLabels(input *SetSecretInput, target, hubHostname string) map[string]string {
	labels := map[string]string{
		"scion-scope":        sanitizeLabel(input.Scope),
		"scion-scope-id":     sanitizeLabel(input.ScopeID),
		"scion-type":         sanitizeLabel(input.SecretType),
		"scion-name":         sanitizeLabel(input.Name),
		"scion-target":       sanitizeLabel(target),
		"scion-hub-hostname": sanitizeLabel(hubHostname),
	}
	if input.Scope == ScopeUser && input.UserEmail != "" {
		labels["scion-userid"] = sanitizeLabel(input.UserEmail)
	}
	return labels
}

// hostname returns the machine hostname for labeling, or "unknown" if unavailable.
func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
