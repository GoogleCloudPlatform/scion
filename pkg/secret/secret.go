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

// Package secret provides the SecretBackend abstraction layer for secret storage.
// It sits between Hub handlers/dispatcher and the underlying secret storage,
// enabling pluggable backends (local SQLite, GCP Secret Manager, etc.).
package secret

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// ErrNoSecretBackend is returned when a secret operation is attempted but no
// secret backend has been initialized (e.g., the hub was started without a
// database or secret storage). Handlers translate this to HTTP 501.
var ErrNoSecretBackend = errors.New("no secret backend is configured; set SCION_SERVER_SECRETS_BACKEND to \"local\" or \"gcpsm\"")

// PermissionError indicates that a secret backend operation failed due to
// insufficient permissions (e.g., the Hub service account lacks a required
// Secret Manager permission). Handlers should translate this to HTTP 403.
type PermissionError struct {
	Operation string // e.g., "create secret", "access secret version"
	Err       error  // the underlying error
}

func (e *PermissionError) Error() string {
	return fmt.Sprintf("failed to %s: the Hub service account lacks the required Secret Manager permission. "+
		"Grant roles/secretmanager.admin to the Hub Runner service account", e.Operation)
}

func (e *PermissionError) Unwrap() error {
	return e.Err
}

// Secret type constants define how a secret is projected into the agent container.
const (
	TypeEnvironment = "environment" // Injected as environment variable (default)
	TypeVariable    = "variable"    // Written to ~/.scion/secrets.json for programmatic access
	TypeFile        = "file"        // Written to a file at the specified Target path
)

// Scope constants define the visibility of a secret.
const (
	ScopeUser          = "user"
	ScopeProject       = "project"
	ScopeRuntimeBroker = "runtime_broker"
)

// Filter specifies criteria for listing secrets.
type Filter struct {
	Scope   string // Required: user, project, runtime_broker
	ScopeID string // Required: ID of the scoped entity
	Type    string // Optional: filter by secret type (environment, variable, file)
	Name    string // Optional: filter by specific key name
}

// SecretMeta holds secret metadata without the secret value.
type SecretMeta struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`    // Secret key name (e.g., "API_KEY")
	SecretType    string    `json:"type"`    // environment, variable, file
	Target        string    `json:"target"`  // Projection target
	Scope         string    `json:"scope"`   // user, project, runtime_broker
	ScopeID       string    `json:"scopeId"` // ID of the scoped entity
	Description   string    `json:"description,omitempty"`
	InjectionMode string    `json:"injectionMode,omitempty"` // "always" or "as_needed"
	SecretRef     string    `json:"secretRef,omitempty"`     // External reference (e.g., GCP SM resource path)
	AllowProgeny  bool      `json:"allowProgeny,omitempty"`  // Allow creator's progeny agents to access (user scope only)
	Version       int       `json:"version"`
	Created       time.Time `json:"created"`
	Updated       time.Time `json:"updated"`
	CreatedBy     string    `json:"createdBy,omitempty"`
	UpdatedBy     string    `json:"updatedBy,omitempty"`
}

// SecretWithValue embeds SecretMeta and adds the plaintext secret value.
type SecretWithValue struct {
	SecretMeta
	Value string `json:"-"` // Never serialized
}

// UpdateMetaInput contains fields for a metadata-only secret update.
// Only non-zero fields are updated. The secret value is never touched.
type UpdateMetaInput struct {
	Name          string
	Scope         string
	ScopeID       string
	Description   *string // pointer to distinguish "" (clear) from nil (no change)
	InjectionMode string  // "" means no change
	SecretType    string  // "" means no change
	Target        string  // "" means no change
	AllowProgeny  *bool   // pointer to distinguish false from nil
	UpdatedBy     string
}

// SetSecretInput provides the data needed to create or update a secret.
type SetSecretInput struct {
	Name          string // Secret key name
	Value         string // Plaintext secret value
	SecretType    string // environment, variable, file
	Target        string // Projection target
	Scope         string // user, project, runtime_broker
	ScopeID       string // ID of the scoped entity
	Description   string // Optional description
	InjectionMode string // "always" or "as_needed"
	AllowProgeny  bool   // Allow creator's progeny agents to access (user scope only)
	CreatedBy     string // User ID of creator (for new secrets)
	UpdatedBy     string // User ID of updater
	UserEmail     string // Email of the user (for labeling user-scoped secrets)
}

// ResolveOpts provides additional context for secret resolution.
type ResolveOpts struct {
	// AgentAncestry is the ordered ancestor chain from the agent's token.
	// When present, secrets marked allowProgeny whose creator appears
	// in this chain are included in the result.
	AgentAncestry []string

	// AuthzCheck is called for each progeny-candidate secret to verify
	// access via the policy engine. If nil, progeny secrets are not included.
	// Returns true if access is allowed.
	AuthzCheck func(secret SecretMeta) bool
}

// FetchResult holds the outcome of fetching one secret's value by its
// recorded metadata. Value is meaningful only when Err is nil; a failed item
// always carries an empty Value, never a partially-successful one. It is
// returned by LocalBackend.FetchValues and GCPBackend.FetchValues (see their
// doc comments for the shared contract).
type FetchResult struct {
	Value string
	Err   error
}

// SecretBackend defines the interface for secret storage operations.
// Implementations include local (wrapping store.SecretStore) and GCP Secret Manager.
type SecretBackend interface {
	// Get retrieves a secret including its value, keyed by name/scope/scopeId
	// rather than a specific recorded metadata version. It exists for the
	// fixed set of hub-internal callers that read a value directly, outside
	// the material-selection flow: hub infrastructure secrets (signing keys,
	// OIDC keys, telemetry credentials), hub-side git clone credentials that
	// are never delivered to an agent, and the agent material paths that
	// have not yet switched to FetchValues. On the GCP backend, when no Hub
	// database record exists, Get still falls back to a Secret Manager
	// lookup by a computed name, to recover from a database reset; that
	// fallback is exactly what FetchValues does not do. Anything selected
	// for delivery to an agent should use FetchValues, which resolves a
	// specific recorded metadata version and never falls back by name.
	//
	// A new caller must be written using one of the receiver names
	// TestSecretBackendGet_CallersAreHubInternal (backend_test.go) matches —
	// secretBackend, sb or Backend — or that drift guard must be updated to
	// see it; it is a name-based regex scan, not a type-aware one.
	Get(ctx context.Context, name, scope, scopeID string) (*SecretWithValue, error)

	// FetchValues returns values for exactly the given metadata records,
	// matched by ID, Version, AllowProgeny, CreatedBy and SecretType, keyed
	// by each record's ID in the returned map. There is no name-based
	// fallback: a record that is no longer in the store, or whose current
	// ID, Version, AllowProgeny, CreatedBy or SecretType no longer matches
	// the recorded metadata, is reported as store.ErrNotFound for that item.
	// The extra AllowProgeny/CreatedBy/SecretType comparison catches a
	// same-Version metadata race that ID+Version alone would miss, since
	// UpdateSecretMeta is a read-modify-write with no version predicate (see
	// recordGenerationChanged in backend.go). A decrypt or backend-access
	// failure is also reported as a per-item error; a failed item's value is
	// always empty, never delivered as an empty string in place of an
	// error. Records whose current SecretType is internal are refused with
	// store.ErrNotFound, since internal secrets are never candidates for
	// delivery. The returned outer error reports only a failure of the
	// whole call, not a per-item failure.
	FetchValues(ctx context.Context, metas []SecretMeta) (map[string]FetchResult, error)

	// Set creates or updates a secret. Returns whether a new secret was created.
	Set(ctx context.Context, input *SetSecretInput) (created bool, meta *SecretMeta, err error)

	// Delete removes a secret.
	Delete(ctx context.Context, name, scope, scopeID string) error

	// List returns secret metadata matching the filter. Values are not included.
	List(ctx context.Context, filter Filter) ([]SecretMeta, error)

	// GetMeta retrieves secret metadata without the value.
	GetMeta(ctx context.Context, name, scope, scopeID string) (*SecretMeta, error)

	// UpdateMeta updates only the metadata of an existing secret.
	// The secret value is not modified. Returns ErrNotFound if the secret doesn't exist.
	UpdateMeta(ctx context.Context, input *UpdateMetaInput) (*SecretMeta, error)

	// Resolve collects and merges secrets from all applicable scopes for an agent.
	// Scopes are resolved in order, lowest first: runtime_broker < hub < project < user.
	// The opts parameter is optional; pass nil for the current behavior.
	// When opts.AgentAncestry is present, user-scoped secrets marked allowProgeny
	// whose creator appears in the ancestry chain are included in the result.
	Resolve(ctx context.Context, userID, projectID, brokerID string, opts *ResolveOpts) ([]SecretWithValue, error)

	// HubID returns the hub instance ID used for hub-scoped secret namespacing.
	HubID() string
}

// scopePrecedence returns a numeric rank for the given scope string.
// Higher values indicate higher precedence. Unknown scopes get 0.
//
// Order must match envScopePrecedence in pkg/hub/httpdispatcher.go and the
// scope ordering in LocalBackend.Resolve / GCPBackend.Resolve: runtime_broker
// is the most infrastructural and least specific scope, so it is the
// weakest, not an override nobody can escape. (This function previously
// ranked runtime_broker highest — the opposite of every other
// precedence-ordered resolver in this codebase — which let a stale
// broker-scoped secret silently shadow a project- or user-scoped one for
// any target two differently-named secrets happened to share.)
func scopePrecedence(scope string) int {
	switch scope {
	case ScopeRuntimeBroker:
		return 1
	case ScopeHub:
		return 2
	case ScopeProject:
		return 3
	case ScopeUser:
		return 4
	default:
		return 0
	}
}

// ScopeHub is the hub-level scope constant. It is defined in the store package
// but duplicated here to avoid a circular import in the precedence helper.
const ScopeHub = "hub"

// DeduplicateByTarget resolves conflicts where multiple secrets (with different
// names) map to the same injection target. For each target, the secret from the
// highest-precedence scope wins. If two secrets at the same scope share a
// target, the last one encountered is kept (non-deterministic from map
// iteration, but this is a misconfiguration).
func DeduplicateByTarget(secrets []SecretWithValue) []SecretWithValue {
	// Index: target → best secret seen so far
	type winner struct {
		index      int
		precedence int
	}
	targetWinners := make(map[string]winner)

	for i, s := range secrets {
		key := s.SecretType + ":" + s.Target
		prec := scopePrecedence(s.Scope)
		w, exists := targetWinners[key]
		if !exists || prec >= w.precedence {
			if exists {
				loser := secrets[w.index]
				slog.Warn("duplicate secret target: higher-scope secret takes precedence",
					"target", s.Target,
					"type", s.SecretType,
					"winner_name", s.Name,
					"winner_scope", s.Scope,
					"replaced_name", loser.Name,
					"replaced_scope", loser.Scope,
				)
			}
			targetWinners[key] = winner{index: i, precedence: prec}
		} else {
			slog.Warn("duplicate secret target: higher-scope secret takes precedence",
				"target", s.Target,
				"type", s.SecretType,
				"winner_name", secrets[w.index].Name,
				"winner_scope", secrets[w.index].Scope,
				"replaced_name", s.Name,
				"replaced_scope", s.Scope,
			)
		}
	}

	result := make([]SecretWithValue, 0, len(targetWinners))
	// Preserve original ordering of winners
	for i, s := range secrets {
		key := s.SecretType + ":" + s.Target
		if w, ok := targetWinners[key]; ok && w.index == i {
			result = append(result, s)
		}
	}
	return result
}
