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

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	systemoneruntime "github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
)

// setSystemOneRuntimeOptions replaces optional runtime construction options and
// rebuilds the Server's System One runtime for package-internal tests.
func (s *Server) setSystemOneRuntimeOptions(opts ...systemoneruntime.Option) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.systemOneRuntimeOpts = slices.Clone(opts)
	s.rebuildSystemOneRuntimeLocked()
}

type stubSystemOneSecretBackend struct {
	secret.SecretBackend
	getFn func(ctx context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error)
}

func (b *stubSystemOneSecretBackend) Get(ctx context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
	return b.getFn(ctx, name, scope, scopeID)
}

func TestSystemOneCredentialSource_ErrorClassification(t *testing.T) {
	validInst := systemoneruntime.Instance{
		ID:      "11111111-1111-1111-1111-111111111111",
		Name:    "typesafe-prod",
		Adapter: "typesafe",
		Auth: systemoneruntime.InstanceAuth{
			Kind:       store.SystemOneAuthKindAPIKey,
			SecretName: "systemone.instance.11111111-1111-1111-1111-111111111111",
		},
	}

	t.Run("nil secret backend returns ErrCredentialUnavailable", func(t *testing.T) {
		srv := &Server{}
		src := &hubSystemOneCredentialSource{srv: srv}
		tok, err := src.BearerToken(context.Background(), validInst)
		assert.Empty(t, tok)
		require.ErrorIs(t, err, egress.ErrCredentialUnavailable)
	})

	t.Run("non-api_key auth kind returns ErrAuth", func(t *testing.T) {
		srv := &Server{secretBackend: &stubSystemOneSecretBackend{
			getFn: func(context.Context, string, string, string) (*secret.SecretWithValue, error) {
				t.Fatal("backend must not be called for unsupported auth kind")
				return nil, nil
			},
		}}
		src := &hubSystemOneCredentialSource{srv: srv}
		inst := validInst
		inst.Auth.Kind = "google_access_token"
		tok, err := src.BearerToken(context.Background(), inst)
		assert.Empty(t, tok)
		require.ErrorIs(t, err, egress.ErrAuth)
	})

	t.Run("empty or unprefixed secret name returns ErrAuth", func(t *testing.T) {
		srv := &Server{secretBackend: &stubSystemOneSecretBackend{
			getFn: func(context.Context, string, string, string) (*secret.SecretWithValue, error) {
				t.Fatal("backend must not be called for invalid secret name")
				return nil, nil
			},
		}}
		src := &hubSystemOneCredentialSource{srv: srv}
		for _, badName := range []string{"", "OTHER_SECRET"} {
			inst := validInst
			inst.Auth.SecretName = badName
			tok, err := src.BearerToken(context.Background(), inst)
			assert.Empty(t, tok)
			require.ErrorIs(t, err, egress.ErrAuth)
		}
	})

	t.Run("missing or access-denied secret returns ErrAuth", func(t *testing.T) {
		authErrors := []error{
			store.ErrNotFound,
			status.Error(codes.NotFound, "secret version destroyed"),
			status.Error(codes.PermissionDenied, "iam denied"),
			&secret.PermissionError{Operation: "get", Err: errors.New("denied")},
		}
		for _, backendErr := range authErrors {
			srv := &Server{secretBackend: &stubSystemOneSecretBackend{
				getFn: func(_ context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
					assert.Equal(t, validInst.Auth.SecretName, name)
					assert.Equal(t, store.ScopeHub, scope)
					assert.Empty(t, scopeID)
					return nil, backendErr
				},
			}}
			src := &hubSystemOneCredentialSource{srv: srv}
			tok, err := src.BearerToken(context.Background(), validInst)
			assert.Empty(t, tok)
			require.ErrorIs(t, err, egress.ErrAuth, "backendErr=%v", backendErr)
		}
	})

	t.Run("non-internal secret type or invalid bearer value returns ErrAuth", func(t *testing.T) {
		cases := []*secret.SecretWithValue{
			nil,
			{SecretMeta: secret.SecretMeta{SecretType: store.SecretTypeEnvironment}, Value: "valid-token"},
			{SecretMeta: secret.SecretMeta{SecretType: store.SecretTypeInternal}, Value: "   "},
			{SecretMeta: secret.SecretMeta{SecretType: store.SecretTypeInternal}, Value: "bad\r\ntoken"},
		}
		for i, sv := range cases {
			srv := &Server{secretBackend: &stubSystemOneSecretBackend{
				getFn: func(context.Context, string, string, string) (*secret.SecretWithValue, error) {
					return sv, nil
				},
			}}
			src := &hubSystemOneCredentialSource{srv: srv}
			tok, err := src.BearerToken(context.Background(), validInst)
			assert.Empty(t, tok, "case %d", i)
			require.ErrorIs(t, err, egress.ErrAuth, "case %d", i)
		}
	})

	t.Run("transient backend fault returns ErrCredentialUnavailable", func(t *testing.T) {
		srv := &Server{secretBackend: &stubSystemOneSecretBackend{
			getFn: func(context.Context, string, string, string) (*secret.SecretWithValue, error) {
				return nil, errors.New("database connection reset")
			},
		}}
		src := &hubSystemOneCredentialSource{srv: srv}
		tok, err := src.BearerToken(context.Background(), validInst)
		assert.Empty(t, tok)
		require.ErrorIs(t, err, egress.ErrCredentialUnavailable)
	})

	t.Run("context cancellation passes through", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		srv := &Server{secretBackend: &stubSystemOneSecretBackend{
			getFn: func(c context.Context, _, _, _ string) (*secret.SecretWithValue, error) {
				return nil, c.Err()
			},
		}}
		src := &hubSystemOneCredentialSource{srv: srv}
		tok, err := src.BearerToken(ctx, validInst)
		assert.Empty(t, tok)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("valid internal secret returns bearer token", func(t *testing.T) {
		const want = "sentinel-unit-bearer-token-99"
		srv := &Server{secretBackend: &stubSystemOneSecretBackend{
			getFn: func(_ context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
				assert.Equal(t, validInst.Auth.SecretName, name)
				assert.Equal(t, store.ScopeHub, scope)
				assert.Empty(t, scopeID)
				return &secret.SecretWithValue{
					SecretMeta: secret.SecretMeta{
						Name:       name,
						Scope:      store.ScopeHub,
						SecretType: store.SecretTypeInternal,
					},
					Value: want,
				}, nil
			},
		}}
		src := &hubSystemOneCredentialSource{srv: srv}
		tok, err := src.BearerToken(context.Background(), validInst)
		require.NoError(t, err)
		assert.Equal(t, want, tok)
	})
}

func TestResolveSystemOneMaxEvalTime(t *testing.T) {
	assert.Equal(t, 3*time.Second, resolveSystemOneMaxEvalTime(ServerConfig{}))

	withStartup := ServerConfig{
		StartupServerSettings: &config.V1ServerConfig{
			Hub: &config.V1ServerHubConfig{
				SystemOne: &config.V1ServerHubSystemOneConfig{
					MaxEvalTime: "7s",
				},
			},
		},
	}
	assert.Equal(t, 7*time.Second, resolveSystemOneMaxEvalTime(withStartup))

	withOverride := withStartup
	withOverride.SystemOneMaxEvalTime = 4500 * time.Millisecond
	assert.Equal(t, 4500*time.Millisecond, resolveSystemOneMaxEvalTime(withOverride))

	capped := ServerConfig{SystemOneMaxEvalTime: 90 * time.Second}
	assert.Equal(t, systemoneruntime.MaxAllowedEvalTime, resolveSystemOneMaxEvalTime(capped))
}

func TestSystemOneStartupCheckedFileBlock(t *testing.T) {
	validMerged := &config.V1ServerConfig{
		Hub: &config.V1ServerHubConfig{
			SystemOne: &config.V1ServerHubSystemOneConfig{MaxEvalTime: "5s"},
		},
	}
	require.NoError(t, checkStartupFileBlocks(validMerged, []string{"server.hub.system_one.max_eval_time"}))

	for _, bad := range []string{"45s", "0s", "-1s", "not-a-duration"} {
		invalidMerged := &config.V1ServerConfig{
			Hub: &config.V1ServerHubConfig{
				SystemOne: &config.V1ServerHubSystemOneConfig{MaxEvalTime: bad},
			},
		}
		err := checkStartupFileBlocks(invalidMerged, []string{"server.hub.system_one.max_eval_time"})
		require.Error(t, err, "max_eval_time=%q", bad)
		var fieldErr *serverConfigFileFieldError
		require.ErrorAs(t, err, &fieldErr)
		assert.Equal(t, "server.hub.system_one.max_eval_time", fieldErr.field)
	}
}

// smNameCaptureClient captures the exact GCP Secret Manager secret resource
// name computed by secret.GCPBackend without making any network or store calls.
type smNameCaptureClient struct {
	lastDeletedName string
}

func (c *smNameCaptureClient) CreateSecret(_ context.Context, _ *smpb.CreateSecretRequest) (*smpb.Secret, error) {
	return nil, errors.New("not implemented")
}

func (c *smNameCaptureClient) AddSecretVersion(_ context.Context, _ *smpb.AddSecretVersionRequest) (*smpb.SecretVersion, error) {
	return nil, errors.New("not implemented")
}

func (c *smNameCaptureClient) AccessSecretVersion(_ context.Context, _ *smpb.AccessSecretVersionRequest) (*smpb.AccessSecretVersionResponse, error) {
	return nil, errors.New("not implemented")
}

func (c *smNameCaptureClient) DeleteSecret(_ context.Context, req *smpb.DeleteSecretRequest) error {
	c.lastDeletedName = req.GetName()
	return errors.New("stop-after-name-capture")
}

func (c *smNameCaptureClient) GetSecret(_ context.Context, _ *smpb.GetSecretRequest) (*smpb.Secret, error) {
	return nil, errors.New("not implemented")
}

func (c *smNameCaptureClient) Close() error { return nil }

func realGCPComputedSecretID(t *testing.T, gcp *secret.GCPBackend, capture *smNameCaptureClient, key string) string {
	t.Helper()
	capture.lastDeletedName = ""
	_ = gcp.Delete(context.Background(), key, store.ScopeHub, gcp.HubID())
	const resourcePrefix = "projects/test-proj/secrets/"
	require.True(t, strings.HasPrefix(capture.lastDeletedName, resourcePrefix),
		"unexpected SM resource path %q for key %q", capture.lastDeletedName, key)
	return strings.TrimPrefix(capture.lastDeletedName, resourcePrefix)
}

func TestIsSystemOneReservedSecretKey_SanitizedForms(t *testing.T) {
	const instID = "22222222-2222-2222-2222-222222222222"
	canonicalKey := "systemone.instance." + instID

	reserved := []string{
		canonicalKey,
		"systemone-instance-" + instID,
		"SystemOne_Instance_" + instID,
		// R1: U+0130 (İ) and U+212A (K Kelvin sign) fold to ASCII 'i'/'k' under
		// strings.ToLower, whereas GCP Secret Manager maps them to "-".
		"systemone\u0130instance\u0130" + instID,
		"systemone\u212ainstance." + instID,
		"systemone\u212ainstance\u212a" + instID,
		// Other non-ASCII letters (U+017F long s 'ſ', accented Latin 'é', Greek 'ο', Cyrillic 'о') in separator positions.
		"systemone\u017finstance\u00e9" + instID,
		"systemone\u03bfinstance\u043e" + instID,
		"  SYSTEMONE/INSTANCE/abc-123  ",
		"systemone:instance:abc-123",
		"systemone.instance.",
		"systemone-instance-",
		"systemone.instance ",
		"systemone.instance." + strings.Repeat("x", 300),
	}
	for _, k := range reserved {
		assert.True(t, isSystemOneReservedSecretKey(k), "expected reserved for %q", k)
	}

	allowed := []string{
		"",
		"OPENAI_API_KEY",
		"SYSTEMONE_API_KEY",
		"systemone",
		"systemone.instances.other",
		"my_systemone_instance_key",
		// Non-ASCII letters inside the word stem map to "-" on GCP SM and therefore do not collide.
		"\u212aELVIN_API_KEY",
		"\u0130STANBUL_API_KEY",
		"system\u212ane.instance." + instID,
		"sy\u017ftemone.instance." + instID,
		"system\u00f6ne.instance." + instID,
	}
	for _, k := range allowed {
		assert.False(t, isSystemOneReservedSecretKey(k), "expected non-reserved for %q", k)
	}

	// Parity check against pkg/secret's real GCPBackend secret-ID computation:
	// 1. Unicode separator collisions (U+0130, U+212A, U+017F/U+00E9, Greek/Cyrillic)
	//    produce the EXACT same GCP SM secret ID as canonical "systemone.instance.<id>"
	//    and MUST all be flagged as reserved.
	// 2. Across all reserved and allowed test vectors, any key whose real GCP SM
	//    secret ID falls under the reserved prefix (case-insensitively, or with
	//    "_" folded to "-") MUST be flagged as reserved (zero false negatives).
	capture := &smNameCaptureClient{}
	gcp := secret.NewGCPBackendWithClient(nil, capture, "test-proj", "test-hub")
	canonicalSMID := realGCPComputedSecretID(t, gcp, capture, canonicalKey)
	reservedSMPrefix := strings.TrimSuffix(
		realGCPComputedSecretID(t, gcp, capture, "systemone.instance.SENTINEL"),
		"SENTINEL",
	)
	require.NotEmpty(t, reservedSMPrefix)

	exactCollisionInputs := []string{
		"systemone-instance-" + instID,
		"systemone\u0130instance\u0130" + instID,
		"systemone\u212ainstance." + instID,
		"systemone\u017finstance\u00e9" + instID,
		"systemone\u03bfinstance\u043e" + instID,
	}
	for _, k := range exactCollisionInputs {
		gotSMID := realGCPComputedSecretID(t, gcp, capture, k)
		assert.Equal(t, canonicalSMID, gotSMID, "expected exact GCP SM collision for %q", k)
		assert.True(t, isSystemOneReservedSecretKey(k), "colliding key %q must be reserved", k)
	}

	for _, k := range slices.Concat(reserved, allowed) {
		gotSMID := realGCPComputedSecretID(t, gcp, capture, k)
		foldedSMID := strings.ReplaceAll(strings.ToLower(gotSMID), "_", "-")
		if strings.HasPrefix(foldedSMID, reservedSMPrefix) {
			assert.True(t, isSystemOneReservedSecretKey(k),
				"key %q produced GCP SM ID %q under reserved prefix %q but was not rejected",
				k, gotSMID, reservedSMPrefix)
		}
	}

	filtered := withoutSystemOneReservedSecrets([]secret.SecretMeta{
		{Name: "OPENAI_API_KEY"},
		{Name: "systemone.instance.1111"},
		{Name: "systemone-instance-2222"},
		{Name: "SystemOne_Instance_3333"},
		{Name: "systemone\u0130instance\u01304444"},
		{Name: "systemone\u212ainstance.5555"},
		{Name: "SYSTEMONE_API_KEY"},
	})
	require.Len(t, filtered, 2)
	assert.Equal(t, "OPENAI_API_KEY", filtered[0].Name)
	assert.Equal(t, "SYSTEMONE_API_KEY", filtered[1].Name)
}
