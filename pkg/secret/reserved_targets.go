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
	"path"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// reservedEnvTargetPrefixes lists the environment-variable name prefixes that
// are reserved for scion's own control-plane variables. These names are
// always set by the runtime itself, so an environment-type secret may not
// target one of them. This mirrors the existing SCION_* persistence
// allowlist in the hub dispatcher (shouldPersistResolvedEnvKey), which
// already treats SCION_* as a reserved system namespace. The list is
// api.ReservedEnvPrefixes, shared with pkg/hostedguard.
var reservedEnvTargetPrefixes = api.ReservedEnvPrefixes()

// IsReservedEnvTarget reports whether target falls under a prefix reserved
// for scion's own control-plane environment variables (see
// reservedEnvTargetPrefixes). It is meaningful only for environment-type
// secret targets; other secret types are not projected into the container
// environment.
func IsReservedEnvTarget(target string) bool {
	for _, prefix := range reservedEnvTargetPrefixes {
		if strings.HasPrefix(target, prefix) {
			return true
		}
	}
	return false
}

// IsReservedTelemetryCredentialFileTarget reports whether target resolves to
// the well-known telemetry GCP credentials path (.scion/telemetry-gcp-credentials.json)
// at a path-segment boundary, after slash normalization, path cleaning, and
// Unicode simple case folding (strings.EqualFold, covering case-insensitive
// broker filesystems including U+017F long-s folding).
func IsReservedTelemetryCredentialFileTarget(target string) bool {
	if target == "" {
		return false
	}
	cleaned := path.Clean(strings.ReplaceAll(target, "\\", "/"))
	dir, file := path.Split(cleaned)
	if dir == "" {
		return false
	}
	return strings.EqualFold(file, "telemetry-gcp-credentials.json") &&
		strings.EqualFold(path.Base(path.Clean(dir)), ".scion")
}

// IsReservedTelemetryCredentialSecretName reports whether name is the
// well-known telemetry credential secret name (scion-telemetry-gcp-credentials),
// which is reserved for hub scope in hosted mode.
func IsReservedTelemetryCredentialSecretName(name string) bool {
	return name == telemetryCredentialFileName
}

// IsReservedFileTarget reports whether target is reserved for hub-only file
// secrets and therefore prohibited for non-hub scopes in hosted mode.
// Hub-scope secrets (ScopeHub) are permitted to target it (for the well-known
// scion-telemetry-gcp-credentials secret).
func IsReservedFileTarget(scope, target string) bool {
	if scope == ScopeHub {
		return false
	}
	return IsReservedTelemetryCredentialFileTarget(target)
}

// IsReservedNonHubSecret reports whether a secret with the given scope, name,
// type, and target is reserved for hub scope in hosted mode and therefore
// prohibited for non-hub scopes. A non-hub secret is reserved if either:
//   - its name is scion-telemetry-gcp-credentials (of any type, including
//     environment, file, and on_demand, because non-hub rows of that name can
//     replace or shadow the hub-scoped telemetry credential); or
//   - it is a file-type secret whose normalized effective target (target, or
//     name when empty) matches .scion/telemetry-gcp-credentials.json
//     case-insensitively at a path-segment boundary.
//
// Hub-scope secrets (ScopeHub) are never reserved by this check.
func IsReservedNonHubSecret(scope, name, secretType, target string) bool {
	if scope == ScopeHub {
		return false
	}
	if IsReservedTelemetryCredentialSecretName(name) {
		return true
	}
	if secretType == store.SecretTypeFile {
		effTarget := target
		if effTarget == "" {
			effTarget = name
		}
		return IsReservedTelemetryCredentialFileTarget(effTarget)
	}
	return false
}
