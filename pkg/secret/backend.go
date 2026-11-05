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
	"fmt"
	"path"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Backend type constants.
const (
	BackendLocal = "local"
	BackendGCPSM = "gcpsm"
)

// GCPBackendConfig holds configuration for the GCP Secret Manager backend.
type GCPBackendConfig struct {
	ProjectID       string
	CredentialsJSON string
	// ReplicationLocations, if non-empty, switches Secret Manager from automatic
	// (global) to user-managed regional replication. Use when org policy
	// constraints/gcp.resourceLocations prohibits global secrets.
	// Example: []string{"northamerica-northeast1"}
	ReplicationLocations []string
}

// recordGenerationChanged reports whether the current store record no longer
// matches the caller's recorded metadata closely enough for FetchValues to
// deliver its value. This is the shared record-generation check used by both
// LocalBackend.fetchValue and GCPBackend.fetchValue; s is always the record
// found by looking up meta's own Name/Scope/ScopeID triple, so those three
// fields are already known to be equal and are not compared again here.
//
// A record fails the check when:
//   - its ID or Version differs from meta's (the record was deleted and
//     recreated under the same triple, or rotated to a new version);
//   - its AllowProgeny, CreatedBy or SecretType differs from meta's, even at
//     the same Version. UpdateSecretMeta is a read-modify-write with no
//     version predicate, so two concurrent metadata updates can each bump
//     Version from the same baseline and land on the same new Version with
//     different field values; Version alone would not catch that.
//   - its SecretType is (still) internal, since internal secrets are never
//     candidates for delivery regardless of whether meta already recorded
//     that.
//
// A nil s reports changed rather than dereferencing it. Every current caller
// only reaches this check after SecretStore.GetSecret has returned a nil
// error, and that interface's contract promises a non-nil record in that
// case, so s is not expected to be nil in practice; this is defense in depth
// against a future SecretStore implementation that returns (nil, nil).
func recordGenerationChanged(s *store.Secret, meta SecretMeta) bool {
	if s == nil {
		return true
	}
	return s.ID != meta.ID ||
		s.Version != meta.Version ||
		s.AllowProgeny != meta.AllowProgeny ||
		s.CreatedBy != meta.CreatedBy ||
		s.SecretType != meta.SecretType ||
		s.SecretType == store.SecretTypeInternal
}

// Well-known name and in-home path of the telemetry credential file secret.
const (
	telemetryCredentialFileName = "scion-telemetry-gcp-credentials"
	telemetryCredentialFilePath = ".scion/telemetry-gcp-credentials.json"
)

// HubSecretDeliverable reports whether a hub-scope secret may reach an agent.
// Environment-type secrets qualify with injection mode "always" or
// "as_needed", plus the telemetry credential file (isTelemetryCredentialFile).
// Variable, file and internal hub-scope secrets, and environment secrets with
// any other mode, never reach an agent. Callers apply it to hub-scope rows
// only, before reading values and before merging scopes, so an excluded row is
// absent and never shadows a lower-scope row of the same name.
//
// An "as_needed" environment secret is admitted only for the harness
// credential pass: every direct-share path drops as_needed environment
// secrets on its own (resolveSecrets and selectDeliverySecrets in pkg/hub), so
// such a secret is supplied only when the agent's harness requests its key
// during credential gathering (resolveAsNeededForKeys in pkg/hub).
func HubSecretDeliverable(meta SecretMeta) bool {
	if isHubEnvSecret(meta) && (meta.InjectionMode == store.InjectionModeAlways || meta.InjectionMode == store.InjectionModeAsNeeded) {
		return true
	}
	return isTelemetryCredentialFile(meta)
}

// HubSecretAgentVisible reports whether an agent may see a hub-scope
// secret's metadata through the agent-readable list and get paths. Only
// environment-type secrets with injection mode "always" qualify: as_needed
// secrets and the telemetry credential file stay hidden.
func HubSecretAgentVisible(meta SecretMeta) bool {
	return isHubEnvSecret(meta) && meta.InjectionMode == store.InjectionModeAlways
}

// isHubEnvSecret reports whether a secret is environment-typed (an empty type
// means environment).
func isHubEnvSecret(meta SecretMeta) bool {
	return meta.SecretType == store.SecretTypeEnvironment || meta.SecretType == ""
}

// isTelemetryCredentialFile is the only exception that delivers a hub-scope
// file secret to agents. It covers delivery only: the secret stays hidden
// from agent-readable list and get paths. It requires hub scope, file type,
// the well-known name, and a target that names the well-known path at a
// path-segment boundary.
func isTelemetryCredentialFile(meta SecretMeta) bool {
	if meta.Scope != ScopeHub || meta.SecretType != store.SecretTypeFile || meta.Name != telemetryCredentialFileName {
		return false
	}
	target := path.Clean(meta.Target)
	return target == telemetryCredentialFilePath || strings.HasSuffix(target, "/"+telemetryCredentialFilePath)
}

// NewBackend creates a SecretBackend of the specified type.
// The "local" backend wraps the given SecretStore directly and encrypts values
// at rest with cipher, the server's one shared ValueCipher (ptone/scion#1530
// §5). A nil cipher fails closed: the local backend then refuses every
// value operation (the disabled plaintext mode is NewLegacyValueCipher("")).
// The "gcpsm" backend uses a hybrid approach: metadata in the Hub DB, values in GCP SM.
// The hubID parameter is the unique hub instance identifier used for secret namespacing.
func NewBackend(ctx context.Context, backendType string, s store.SecretStore, gcpCfg GCPBackendConfig, hubID string, cipher *ValueCipher) (SecretBackend, error) {
	switch backendType {
	case BackendLocal, "":
		return NewLocalBackendWithCipher(s, hubID, cipher), nil
	case BackendGCPSM:
		return NewGCPBackend(ctx, s, gcpCfg, hubID)
	default:
		return nil, fmt.Errorf("unknown secrets backend type: %q (supported: %q, %q)", backendType, BackendLocal, BackendGCPSM)
	}
}

// hubRowDeliverable applies HubSecretDeliverable to a stored hub-scope row.
func hubRowDeliverable(s *store.Secret) bool {
	meta := fromStoreSecretMeta(s)
	meta.Scope = ScopeHub
	return HubSecretDeliverable(*meta)
}
