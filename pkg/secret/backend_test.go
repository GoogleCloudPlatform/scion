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

//go:build !no_sqlite

package secret

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ============================================================================
// Resolve: nil AuthzCheck excludes progeny secrets (fail closed by default)
// ============================================================================

// TestResolve_NilAuthzCheckExcludesSharedSecrets verifies that when Resolve
// is called with a non-empty AgentAncestry and no AuthzCheck callback,
// progeny secrets are excluded rather than included. A caller must supply an
// explicit policy decision before any progeny value is read; a missing
// checker is not an implicit allow. This holds for both backends.
func TestResolve_NilAuthzCheckExcludesSharedSecrets(t *testing.T) {
	t.Run("local", func(t *testing.T) {
		backend, s := createTestBackend(t)
		ctx := context.Background()

		seedSecret(t, s, &store.Secret{
			ID:             tid("resolve-nil-authz-local"),
			Key:            "NO_AUTHZ_KEY",
			EncryptedValue: "no-authz-value",
			SecretType:     store.SecretTypeEnvironment,
			Target:         "NO_AUTHZ_KEY",
			Scope:          store.ScopeUser,
			ScopeID:        "alice-123",
			AllowProgeny:   true,
			CreatedBy:      "alice-123",
		})

		opts := &ResolveOpts{
			AgentAncestry: []string{"alice-123", "agent-a"},
			AuthzCheck:    nil,
		}
		resolved, err := backend.Resolve(ctx, "", "", "", opts)
		if err != nil {
			t.Fatalf("Resolve failed: %v", err)
		}
		for _, sv := range resolved {
			if sv.Name == "NO_AUTHZ_KEY" {
				t.Error("progeny secret should be excluded when AuthzCheck is nil")
			}
		}
	})

	t.Run("gcp", func(t *testing.T) {
		backend, _ := createTestGCPBackend(t)
		ctx := context.Background()

		_, _, err := backend.Set(ctx, &SetSecretInput{
			Name:         "NO_AUTHZ_KEY",
			Value:        "no-authz-value",
			SecretType:   TypeEnvironment,
			Scope:        ScopeUser,
			ScopeID:      "alice-123",
			AllowProgeny: true,
			CreatedBy:    "alice-123",
		})
		if err != nil {
			t.Fatalf("Set failed: %v", err)
		}

		opts := &ResolveOpts{
			AgentAncestry: []string{"alice-123", "agent-a"},
			AuthzCheck:    nil,
		}
		resolved, err := backend.Resolve(ctx, "", "", "", opts)
		if err != nil {
			t.Fatalf("Resolve failed: %v", err)
		}
		for _, sv := range resolved {
			if sv.Name == "NO_AUTHZ_KEY" {
				t.Error("progeny secret should be excluded when AuthzCheck is nil")
			}
		}
	})
}
