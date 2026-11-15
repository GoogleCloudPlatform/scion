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
	"testing"
	"time"

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
