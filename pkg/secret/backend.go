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
}

// NewBackend creates a SecretBackend of the specified type.
// The "local" backend wraps the given SecretStore directly and encrypts values
// at rest using a key derived from sharedSecret.
// The "gcpsm" backend uses a hybrid approach: metadata in the Hub DB, values in GCP SM.
// The hubID parameter is the unique hub instance identifier used for secret namespacing.
func NewBackend(ctx context.Context, backendType string, s store.SecretStore, gcpCfg GCPBackendConfig, hubID, sharedSecret string) (SecretBackend, error) {
	switch backendType {
	case BackendLocal, "":
		return NewLocalBackend(s, hubID, sharedSecret), nil
	case BackendGCPSM:
		return NewGCPBackend(ctx, s, gcpCfg, hubID)
	default:
		return nil, fmt.Errorf("unknown secrets backend type: %q (supported: %q, %q)", backendType, BackendLocal, BackendGCPSM)
	}
}
