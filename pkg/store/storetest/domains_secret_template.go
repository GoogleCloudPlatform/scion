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

package storetest

import (
	"context"
	"fmt"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// secretTestScopeID is a fixed scope identifier shared by all entities a single
// Secret/EnvVar domain run creates, so the harness's (key, scope, scope_id)
// lookups resolve consistently within one store.
const secretTestScopeID = "00000000-0000-0000-0000-0000000000aa"

// listResultFrom wraps a plain slice from a non-paginated list method into a
// ListResult so it can satisfy a FilterCase. TotalCount mirrors the slice
// length, which is the contract the filter oracle checks.
func listResultFrom[T any](items []T, err error) (*store.ListResult[T], error) {
	if err != nil {
		return nil, err
	}
	return &store.ListResult[T]{Items: items, TotalCount: len(items)}, nil
}

// TemplateDomain describes the template entity for the CRUD-parity oracle.
func TemplateDomain() Domain[store.Template] {
	return Domain[store.Template]{
		Name: "template",
		Make: func(seq int) *store.Template {
			id := uuid.NewString()
			return &store.Template{
				ID:          id,
				Name:        fmt.Sprintf("Template %d", seq),
				Slug:        fmt.Sprintf("template-%d-%s", seq, id[:8]),
				Harness:     "claude",
				Image:       "img:latest",
				Scope:       store.TemplateScopeGlobal,
				Visibility:  "private",
				Status:      store.TemplateStatusActive,
				ContentHash: fmt.Sprintf("hash-%d", seq),
			}
		},
		GetID: func(e *store.Template) string { return e.ID },
		Create: func(ctx context.Context, s store.Store, e *store.Template) error {
			return s.CreateTemplate(ctx, e)
		},
		Get: func(ctx context.Context, s store.Store, id string) (*store.Template, error) {
			return s.GetTemplate(ctx, id)
		},
		List: func(ctx context.Context, s store.Store, opts store.ListOptions) (*store.ListResult[store.Template], error) {
			return s.ListTemplates(ctx, store.TemplateFilter{}, opts)
		},
		VerifyEqual: func(t *testing.T, want, got *store.Template) {
			assert.Equal(t, want.ID, got.ID)
			assert.Equal(t, want.Name, got.Name)
			assert.Equal(t, want.Slug, got.Slug)
			assert.Equal(t, want.Harness, got.Harness)
			assert.Equal(t, want.Scope, got.Scope)
			assert.False(t, got.Created.IsZero(), "Created timestamp should be set")
		},
		Mutate: func(e *store.Template) {
			e.Name = "Renamed " + e.Name
			e.Status = store.TemplateStatusArchived
		},
		Update: func(ctx context.Context, s store.Store, e *store.Template) error {
			return s.UpdateTemplate(ctx, e)
		},
		VerifyMutated: func(t *testing.T, got *store.Template) {
			assert.Contains(t, got.Name, "Renamed ")
			assert.Equal(t, store.TemplateStatusArchived, got.Status)
		},
		Delete: func(ctx context.Context, s store.Store, id string) error {
			return s.DeleteTemplate(ctx, id)
		},
		Filters: []FilterCase[store.Template]{
			{
				Name: "ByHarness",
				Seed: func(t *testing.T, ctx context.Context, s store.Store) {
					require.NoError(t, s.CreateTemplate(ctx, &store.Template{
						ID: uuid.NewString(), Name: "Claude", Slug: "claude-" + uuid.NewString()[:8],
						Harness: "claude", Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive,
					}))
					require.NoError(t, s.CreateTemplate(ctx, &store.Template{
						ID: uuid.NewString(), Name: "Gemini", Slug: "gemini-" + uuid.NewString()[:8],
						Harness: "gemini", Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive,
					}))
				},
				List: func(ctx context.Context, s store.Store) (*store.ListResult[store.Template], error) {
					return s.ListTemplates(ctx, store.TemplateFilter{Harness: "gemini"}, store.ListOptions{})
				},
				WantCount: 1,
			},
		},
	}
}

// HarnessConfigDomain describes the harness config entity for the CRUD-parity oracle.
func HarnessConfigDomain() Domain[store.HarnessConfig] {
	return Domain[store.HarnessConfig]{
		Name: "harness_config",
		Make: func(seq int) *store.HarnessConfig {
			id := uuid.NewString()
			return &store.HarnessConfig{
				ID:          id,
				Name:        fmt.Sprintf("Harness %d", seq),
				Slug:        fmt.Sprintf("harness-%d-%s", seq, id[:8]),
				Harness:     "claude",
				Scope:       store.HarnessConfigScopeGlobal,
				Visibility:  "private",
				Status:      store.HarnessConfigStatusActive,
				ContentHash: fmt.Sprintf("hash-%d", seq),
			}
		},
		GetID: func(e *store.HarnessConfig) string { return e.ID },
		Create: func(ctx context.Context, s store.Store, e *store.HarnessConfig) error {
			return s.CreateHarnessConfig(ctx, e)
		},
		Get: func(ctx context.Context, s store.Store, id string) (*store.HarnessConfig, error) {
			return s.GetHarnessConfig(ctx, id)
		},
		List: func(ctx context.Context, s store.Store, opts store.ListOptions) (*store.ListResult[store.HarnessConfig], error) {
			return s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, opts)
		},
		VerifyEqual: func(t *testing.T, want, got *store.HarnessConfig) {
			assert.Equal(t, want.ID, got.ID)
			assert.Equal(t, want.Name, got.Name)
			assert.Equal(t, want.Slug, got.Slug)
			assert.Equal(t, want.Harness, got.Harness)
			assert.Equal(t, want.Scope, got.Scope)
			assert.False(t, got.Created.IsZero(), "Created timestamp should be set")
		},
		Mutate: func(e *store.HarnessConfig) {
			e.Name = "Renamed " + e.Name
			e.Status = store.HarnessConfigStatusArchived
		},
		Update: func(ctx context.Context, s store.Store, e *store.HarnessConfig) error {
			return s.UpdateHarnessConfig(ctx, e)
		},
		VerifyMutated: func(t *testing.T, got *store.HarnessConfig) {
			assert.Contains(t, got.Name, "Renamed ")
			assert.Equal(t, store.HarnessConfigStatusArchived, got.Status)
		},
		Delete: func(ctx context.Context, s store.Store, id string) error {
			return s.DeleteHarnessConfig(ctx, id)
		},
		Filters: []FilterCase[store.HarnessConfig]{
			{
				Name: "ByHarness",
				Seed: func(t *testing.T, ctx context.Context, s store.Store) {
					require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
						ID: uuid.NewString(), Name: "Claude", Slug: "claude-" + uuid.NewString()[:8],
						Harness: "claude", Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
					}))
					require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
						ID: uuid.NewString(), Name: "Gemini", Slug: "gemini-" + uuid.NewString()[:8],
						Harness: "gemini", Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
					}))
				},
				List: func(ctx context.Context, s store.Store) (*store.ListResult[store.HarnessConfig], error) {
					return s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{Harness: "gemini"}, store.ListOptions{})
				},
				WantCount: 1,
			},
		},
	}
}

// SecretDomain describes the secret entity for the CRUD-parity oracle. Secrets
// are addressed by (key, scope, scope_id) rather than a surrogate ID, so the
// harness's id parameter carries the key and a fixed scope/scope_id pair is used
// throughout a run. Listing is non-paginated, so only the filter category (not
// pagination) is exercised here.
func SecretDomain() Domain[store.Secret] {
	return Domain[store.Secret]{
		Name: "secret",
		Make: func(seq int) *store.Secret {
			return &store.Secret{
				ID:             uuid.NewString(),
				Key:            fmt.Sprintf("SECRET_%d", seq),
				EncryptedValue: fmt.Sprintf("enc-%d", seq),
				Scope:          store.ScopeUser,
				ScopeID:        secretTestScopeID,
				Description:    fmt.Sprintf("secret %d", seq),
			}
		},
		GetID: func(e *store.Secret) string { return e.Key },
		Create: func(ctx context.Context, s store.Store, e *store.Secret) error {
			return s.CreateSecret(ctx, e)
		},
		Get: func(ctx context.Context, s store.Store, id string) (*store.Secret, error) {
			return s.GetSecret(ctx, id, store.ScopeUser, secretTestScopeID)
		},
		VerifyEqual: func(t *testing.T, want, got *store.Secret) {
			assert.Equal(t, want.Key, got.Key)
			assert.Equal(t, want.EncryptedValue, got.EncryptedValue)
			assert.Equal(t, want.Scope, got.Scope)
			assert.Equal(t, 1, got.Version, "new secret starts at version 1")
			assert.False(t, got.Created.IsZero(), "Created timestamp should be set")
		},
		Mutate: func(e *store.Secret) {
			e.EncryptedValue = "rotated"
			e.Description = "changed"
		},
		Update: func(ctx context.Context, s store.Store, e *store.Secret) error {
			return s.UpdateSecret(ctx, e)
		},
		VerifyMutated: func(t *testing.T, got *store.Secret) {
			assert.Equal(t, "rotated", got.EncryptedValue)
			assert.GreaterOrEqual(t, got.Version, 2, "update should bump version")
		},
		Delete: func(ctx context.Context, s store.Store, id string) error {
			return s.DeleteSecret(ctx, id, store.ScopeUser, secretTestScopeID)
		},
		Filters: []FilterCase[store.Secret]{
			{
				Name: "ByType",
				Seed: func(t *testing.T, ctx context.Context, s store.Store) {
					require.NoError(t, s.CreateSecret(ctx, &store.Secret{
						ID: uuid.NewString(), Key: "ENV_SECRET", EncryptedValue: "v",
						SecretType: store.SecretTypeEnvironment, Scope: store.ScopeUser, ScopeID: secretTestScopeID,
					}))
					require.NoError(t, s.CreateSecret(ctx, &store.Secret{
						ID: uuid.NewString(), Key: "FILE_SECRET", EncryptedValue: "v",
						SecretType: store.SecretTypeFile, Target: "/etc/x", Scope: store.ScopeUser, ScopeID: secretTestScopeID,
					}))
				},
				List: func(ctx context.Context, s store.Store) (*store.ListResult[store.Secret], error) {
					return listResultFrom(s.ListSecrets(ctx, store.SecretFilter{
						Scope: store.ScopeUser, ScopeID: secretTestScopeID, Type: store.SecretTypeFile,
					}))
				},
				WantCount: 1,
			},
		},
	}
}

// EnvVarDomain describes the env var entity for the CRUD-parity oracle. Like
// secrets, env vars are addressed by (key, scope, scope_id); see SecretDomain.
func EnvVarDomain() Domain[store.EnvVar] {
	return Domain[store.EnvVar]{
		Name: "env_var",
		Make: func(seq int) *store.EnvVar {
			return &store.EnvVar{
				ID:            uuid.NewString(),
				Key:           fmt.Sprintf("ENV_%d", seq),
				Value:         fmt.Sprintf("val-%d", seq),
				Scope:         store.ScopeUser,
				ScopeID:       secretTestScopeID,
				InjectionMode: store.InjectionModeAsNeeded,
				Description:   fmt.Sprintf("env %d", seq),
			}
		},
		GetID: func(e *store.EnvVar) string { return e.Key },
		Create: func(ctx context.Context, s store.Store, e *store.EnvVar) error {
			return s.CreateEnvVar(ctx, e)
		},
		Get: func(ctx context.Context, s store.Store, id string) (*store.EnvVar, error) {
			return s.GetEnvVar(ctx, id, store.ScopeUser, secretTestScopeID)
		},
		VerifyEqual: func(t *testing.T, want, got *store.EnvVar) {
			assert.Equal(t, want.Key, got.Key)
			assert.Equal(t, want.Value, got.Value)
			assert.Equal(t, want.Scope, got.Scope)
			assert.False(t, got.Created.IsZero(), "Created timestamp should be set")
		},
		Mutate: func(e *store.EnvVar) {
			e.Value = "updated"
		},
		Update: func(ctx context.Context, s store.Store, e *store.EnvVar) error {
			return s.UpdateEnvVar(ctx, e)
		},
		VerifyMutated: func(t *testing.T, got *store.EnvVar) {
			assert.Equal(t, "updated", got.Value)
		},
		Delete: func(ctx context.Context, s store.Store, id string) error {
			return s.DeleteEnvVar(ctx, id, store.ScopeUser, secretTestScopeID)
		},
		Filters: []FilterCase[store.EnvVar]{
			{
				Name: "ByKey",
				Seed: func(t *testing.T, ctx context.Context, s store.Store) {
					require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
						ID: uuid.NewString(), Key: "KEEP", Value: "v", Scope: store.ScopeUser, ScopeID: secretTestScopeID,
					}))
					require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
						ID: uuid.NewString(), Key: "OTHER", Value: "v", Scope: store.ScopeUser, ScopeID: secretTestScopeID,
					}))
				},
				List: func(ctx context.Context, s store.Store) (*store.ListResult[store.EnvVar], error) {
					return listResultFrom(s.ListEnvVars(ctx, store.EnvVarFilter{
						Scope: store.ScopeUser, ScopeID: secretTestScopeID, Key: "KEEP",
					}))
				},
				WantCount: 1,
			},
		},
	}
}
