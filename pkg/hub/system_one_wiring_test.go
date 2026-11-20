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
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/compute/metadata"
	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	systemoneprovider "github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider/geminidecisions"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider/jevwire"
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

func TestSystemOneProviderRegistry_RegistersBuiltInAdapters(t *testing.T) {
	srv := &Server{}
	reg := srv.SystemOneProviderRegistry()
	assert.Contains(t, reg.Names(), jevwire.AdapterNameTypeSafe)
	assert.Contains(t, reg.Names(), geminidecisions.AdapterName)

	gem, ok := reg.Lookup(geminidecisions.AdapterName)
	require.True(t, ok)
	assert.Equal(t, geminidecisions.DefaultEndpoint, defaultSystemOneAdapterEndpoint(gem))
	_, hasValidator := gem.(systemoneprovider.InstanceValidator)
	assert.True(t, hasValidator)

	ts, ok := reg.Lookup(jevwire.AdapterNameTypeSafe)
	require.True(t, ok)
	assert.Equal(t, jevwire.DefaultTypeSafeEndpoint, defaultSystemOneAdapterEndpoint(ts))
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

type funcTokenSource func() (*oauth2.Token, error)

func (f funcTokenSource) Token() (*oauth2.Token, error) {
	return f()
}

func TestSystemOneADC_CredentialJSONInspectionAndAllowedTypes(t *testing.T) {
	t.Run("allowed_credential_categories", func(t *testing.T) {
		credType, email, err := inspectGoogleCredentialsJSON([]byte(`{
			"type": "service_account",
			"client_email": "gemini-runner@paid-proj-123.iam.gserviceaccount.com",
			"private_key": "SYNTHETIC-NOT-A-KEY"
		}`))
		require.NoError(t, err)
		assert.Equal(t, SystemOneADCTypeServiceAccount, credType)
		assert.Equal(t, "gemini-runner@paid-proj-123.iam.gserviceaccount.com", email)

		credType, email, err = inspectGoogleCredentialsJSON([]byte(`{
			"type": "external_account",
			"service_account_impersonation_url": "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/wif-sa@paid-proj-123.iam.gserviceaccount.com:generateAccessToken"
		}`))
		require.NoError(t, err)
		assert.Equal(t, SystemOneADCTypeExternalAccount, credType)
		assert.Equal(t, "wif-sa@paid-proj-123.iam.gserviceaccount.com", email)

		credType, email, err = inspectGoogleCredentialsJSON([]byte(`{
			"type": "external_account"
		}`))
		require.NoError(t, err)
		assert.Equal(t, SystemOneADCTypeExternalAccount, credType)
		assert.Empty(t, email)

		credType, email, err = inspectGoogleCredentialsJSON([]byte(`{
			"type": "impersonated_service_account",
			"service_account_impersonation_url": "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/imp-sa%40paid-proj-123.iam.gserviceaccount.com:generateAccessToken"
		}`))
		require.NoError(t, err)
		assert.Equal(t, SystemOneADCTypeImpersonatedServiceAccount, credType)
		assert.Equal(t, "imp-sa@paid-proj-123.iam.gserviceaccount.com", email)

		credType, email, err = inspectGoogleCredentialsJSON([]byte(`{
			"type": "authorized_user",
			"client_id": "synthetic-id",
			"client_secret": "synthetic-secret",
			"refresh_token": "synthetic-refresh"
		}`))
		require.NoError(t, err)
		assert.Equal(t, SystemOneADCTypeAuthorizedUser, credType)
		assert.Empty(t, email)
	})

	t.Run("disallowed_or_malformed_types_map_to_ErrAuth", func(t *testing.T) {
		for _, badJSON := range []string{
			`not-json`,
			`{}`,
			`{"web":{"client_id":"id"}}`,
			`{"type":"gdch_service_account"}`,
			`{"type":"external_account_authorized_user"}`,
			`{"type":"unknown_custom_type"}`,
		} {
			_, _, err := inspectGoogleCredentialsJSON([]byte(badJSON))
			require.ErrorIs(t, err, egress.ErrAuth, "json=%s", badJSON)
		}
	})
}

func TestSystemOneADC_ReuseTokenSourceCacheAndErrorMapping(t *testing.T) {
	const (
		canaryToken = "fake-adc-token-CANARY-unit"
		testScopeA  = "https://www.googleapis.com/auth/synthetic.scope.a"
		testScopeB  = "https://www.googleapis.com/auth/synthetic.scope.b"
	)

	t.Run("caches_per_canonical_scope_set_and_refreshes_within_5m_expiry", func(t *testing.T) {
		var providerCalls atomic.Int32
		var mintCalls atomic.Int32
		var nextExpiry time.Time

		srv := &Server{}
		srv.SetSystemOneADCForTesting(t, func(_ context.Context, scopes []string) (*SystemOneADCCredentials, error) {
			n := providerCalls.Add(1)
			return &SystemOneADCCredentials{
				CredentialType:      SystemOneADCTypeServiceAccount,
				ServiceAccountEmail: "sa@paid-proj-123.iam.gserviceaccount.com",
				TokenSource: funcTokenSource(func() (*oauth2.Token, error) {
					m := mintCalls.Add(1)
					return &oauth2.Token{
						AccessToken: canaryToken + "-" + string(rune('0'+n)) + "-" + string(rune('0'+m)),
						Expiry:      nextExpiry,
					}, nil
				}),
			}, nil
		})
		src := &hubSystemOneCredentialSource{srv: srv}

		// Empty scopes are rejected (Addendum E: no default scope).
		noScopeTok, noScopeErr := src.BearerToken(context.Background(), systemoneruntime.Instance{
			Auth: systemoneruntime.InstanceAuth{Kind: store.SystemOneAuthKindGoogleAccessToken},
		})
		assert.Empty(t, noScopeTok)
		require.ErrorIs(t, noScopeErr, egress.ErrAuth)
		assert.Zero(t, providerCalls.Load())

		instA := systemoneruntime.Instance{
			Name:    "gem-adc-a",
			Adapter: geminidecisions.AdapterName,
			Auth: systemoneruntime.InstanceAuth{
				Kind:   store.SystemOneAuthKindGoogleAccessToken,
				Scopes: []string{testScopeA, testScopeB},
			},
			Options: map[string]string{systemoneprovider.OptionQuotaProject: "project-alpha-1"},
		}
		// Reversed scope order + different quota_project -> SAME canonical scope key!
		instB := systemoneruntime.Instance{
			Name:    "gem-adc-b",
			Adapter: geminidecisions.AdapterName,
			Auth: systemoneruntime.InstanceAuth{
				Kind:   store.SystemOneAuthKindGoogleAccessToken,
				Scopes: []string{testScopeB, testScopeA},
			},
			Options: map[string]string{systemoneprovider.OptionQuotaProject: "project-bravo-2"},
		}

		// Token valid for 30 minutes (> 5m reuse window): minted once, reused on instB.
		nextExpiry = time.Now().Add(30 * time.Minute)
		tok1, err := src.BearerToken(context.Background(), instA)
		require.NoError(t, err)
		assert.Equal(t, canaryToken+"-1-1", tok1)

		tok2, err := src.BearerToken(context.Background(), instB)
		require.NoError(t, err)
		assert.Equal(t, canaryToken+"-1-1", tok2)
		assert.Equal(t, int32(1), providerCalls.Load(), "quota_project and scope ordering must not bust ADC cache")
		assert.Equal(t, int32(1), mintCalls.Load(), "token valid for >5m must be reused from memory")

		// Distinct scope set -> new provider lookup.
		instSingleScope := instA
		instSingleScope.Auth.Scopes = []string{testScopeA}
		// Set expiry within 2 minutes (< 5m early-refresh window) so subsequent call refreshes!
		nextExpiry = time.Now().Add(2 * time.Minute)
		tok3, err := src.BearerToken(context.Background(), instSingleScope)
		require.NoError(t, err)
		assert.Equal(t, canaryToken+"-2-2", tok3)
		assert.Equal(t, int32(2), providerCalls.Load())

		nextExpiry = time.Now().Add(30 * time.Minute)
		tok4, err := src.BearerToken(context.Background(), instSingleScope)
		require.NoError(t, err)
		assert.Equal(t, canaryToken+"-2-3", tok4, "token within 5m of expiry must refresh automatically")
		assert.Equal(t, int32(2), providerCalls.Load(), "refresh must reuse the cached TokenSource")
	})

	t.Run("authorized_user_requires_quota_project_at_runtime", func(t *testing.T) {
		var mintCalls atomic.Int32
		srv := &Server{}
		srv.SetSystemOneADCForTesting(t, func(_ context.Context, _ []string) (*SystemOneADCCredentials, error) {
			return &SystemOneADCCredentials{
				CredentialType: SystemOneADCTypeAuthorizedUser,
				TokenSource: funcTokenSource(func() (*oauth2.Token, error) {
					mintCalls.Add(1)
					return &oauth2.Token{AccessToken: canaryToken}, nil
				}),
			}, nil
		})
		src := &hubSystemOneCredentialSource{srv: srv}

		noQP := systemoneruntime.Instance{
			Auth: systemoneruntime.InstanceAuth{
				Kind:   store.SystemOneAuthKindGoogleAccessToken,
				Scopes: []string{testScopeA},
			},
		}
		tok, err := src.BearerToken(context.Background(), noQP)
		assert.Empty(t, tok)
		require.ErrorIs(t, err, egress.ErrAuth)
		assert.Zero(t, mintCalls.Load(), "must reject authorized_user without quota_project before minting token")

		withQP := noQP
		withQP.Options = map[string]string{systemoneprovider.OptionQuotaProject: "paid-proj-123"}
		tok, err = src.BearerToken(context.Background(), withQP)
		require.NoError(t, err)
		assert.Equal(t, canaryToken, tok)
		assert.Equal(t, int32(1), mintCalls.Load())
	})

	t.Run("error_mapping_matches_B7_and_never_echoes_canary", func(t *testing.T) {
		cases := []struct {
			name    string
			credTyp string
			provErr error
			tokErr  error
			wantErr error
		}{
			{
				name:    "no_adc_found",
				provErr: errors.New("google: could not find default credentials; secret=" + canaryToken),
				wantErr: egress.ErrAuth,
			},
			{
				name:    "unreadable_credentials_file",
				provErr: &os.PathError{Op: "open", Path: "/tmp/" + canaryToken, Err: os.ErrNotExist},
				wantErr: egress.ErrAuth,
			},
			{
				name:    "disallowed_credential_type",
				credTyp: "gdch_service_account",
				wantErr: egress.ErrAuth,
			},
			{
				name:    "refresh_invalid_grant",
				credTyp: SystemOneADCTypeServiceAccount,
				tokErr: &oauth2.RetrieveError{
					Response:         &http.Response{StatusCode: http.StatusBadRequest},
					ErrorCode:        "invalid_grant",
					ErrorDescription: "revoked " + canaryToken,
				},
				wantErr: egress.ErrAuth,
			},
			{
				name:    "refresh_http_401",
				credTyp: SystemOneADCTypeServiceAccount,
				tokErr: &oauth2.RetrieveError{
					Response: &http.Response{StatusCode: http.StatusUnauthorized},
					Body:     []byte(canaryToken),
				},
				wantErr: egress.ErrAuth,
			},
			{
				name:    "metadata_not_defined_404",
				credTyp: SystemOneADCTypeMetadata,
				tokErr:  metadata.NotDefinedError("instance/service-accounts/default/token"),
				wantErr: egress.ErrAuth,
			},
			{
				name:    "token_endpoint_503",
				credTyp: SystemOneADCTypeServiceAccount,
				tokErr: &oauth2.RetrieveError{
					Response: &http.Response{StatusCode: http.StatusServiceUnavailable},
					Body:     []byte("upstream outage " + canaryToken),
				},
				wantErr: egress.ErrCredentialUnavailable,
			},
			{
				name:    "metadata_500",
				credTyp: SystemOneADCTypeMetadata,
				tokErr:  &metadata.Error{Code: http.StatusInternalServerError, Message: canaryToken},
				wantErr: egress.ErrCredentialUnavailable,
			},
			{
				name:    "sts_timeout",
				credTyp: SystemOneADCTypeExternalAccount,
				tokErr:  &url.Error{Op: "Post", URL: "https://sts.googleapis.com/v1/token", Err: context.DeadlineExceeded},
				wantErr: egress.ErrCredentialUnavailable,
			},
			{
				name:    "connection_reset",
				credTyp: SystemOneADCTypeServiceAccount,
				tokErr:  io.ErrUnexpectedEOF,
				wantErr: egress.ErrCredentialUnavailable,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				srv := &Server{}
				srv.SetSystemOneADCForTesting(t, func(_ context.Context, _ []string) (*SystemOneADCCredentials, error) {
					if tc.provErr != nil {
						return nil, tc.provErr
					}
					return &SystemOneADCCredentials{
						CredentialType: tc.credTyp,
						TokenSource: funcTokenSource(func() (*oauth2.Token, error) {
							return nil, tc.tokErr
						}),
					}, nil
				})
				src := &hubSystemOneCredentialSource{srv: srv}
				tok, err := src.BearerToken(context.Background(), systemoneruntime.Instance{
					Auth: systemoneruntime.InstanceAuth{
						Kind:   store.SystemOneAuthKindGoogleAccessToken,
						Scopes: []string{testScopeA},
					},
				})
				assert.Empty(t, tok)
				require.ErrorIs(t, err, tc.wantErr)
				assert.NotContains(t, err.Error(), canaryToken)
			})
		}
	})
}

func TestSystemOneADC_DefaultProviderSyntheticInvalidCredentialsFile(t *testing.T) {
	// Point GOOGLE_APPLICATION_CREDENTIALS at synthetic invalid/disallowed temp
	// files so google.FindDefaultCredentials returns immediately from step 1
	// without ever consulting ~/.config/gcloud or GCE metadata.
	const (
		fileSecretCanary = "fake-key-file-secret-CANARY-never-echo"
		testScope        = "https://www.googleapis.com/auth/synthetic.scope.a"
	)
	dir := t.TempDir()

	t.Run("missing_file_maps_to_ErrAuth", func(t *testing.T) {
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(dir, "does-not-exist.json"))
		creds, err := defaultSystemOneADCProvider(context.Background(), []string{testScope})
		assert.Nil(t, creds)
		require.ErrorIs(t, err, egress.ErrAuth)
	})

	t.Run("malformed_json_file_maps_to_ErrAuth_without_echoing_contents", func(t *testing.T) {
		badFile := filepath.Join(dir, "malformed.json")
		require.NoError(t, os.WriteFile(badFile, []byte("not-json-"+fileSecretCanary), 0o600))
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", badFile)

		creds, err := defaultSystemOneADCProvider(context.Background(), []string{testScope})
		assert.Nil(t, creds)
		require.ErrorIs(t, err, egress.ErrAuth)
		assert.NotContains(t, err.Error(), fileSecretCanary)
	})

	t.Run("disallowed_type_json_file_maps_to_ErrAuth", func(t *testing.T) {
		disallowedFile := filepath.Join(dir, "disallowed.json")
		require.NoError(t, os.WriteFile(disallowedFile, []byte(`{"type":"gdch_service_account","private_key":"`+fileSecretCanary+`"}`), 0o600))
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", disallowedFile)

		creds, err := defaultSystemOneADCProvider(context.Background(), []string{testScope})
		assert.Nil(t, creds)
		require.ErrorIs(t, err, egress.ErrAuth)
		assert.NotContains(t, err.Error(), fileSecretCanary)
	})
}

func TestSystemOneNamedSecret_FallbackMatrixTypeChecksAndScopeIsolation(t *testing.T) {
	const (
		hubID        = "hub-g4d-unit"
		projID       = "proj-alpha-id"
		envCanary    = "CANARY_PROCESS_ENV_NEVER_READ_8414"
		hubCanary    = "CANARY_HUB_GEMINI_KEY_8414"
		projCanary   = "CANARY_PROJ_ALPHA_KEY_8414"
		otherCanary  = "CANARY_OTHER_PROJ_NEVER_READ_8414"
		userCanary   = "CANARY_USER_SCOPE_NEVER_READ_8414"
		brokerCanary = "CANARY_BROKER_SCOPE_NEVER_READ_8414"
	)

	// Guard: setting GEMINI_API_KEY in the Hub process environment never has any effect.
	t.Setenv("GEMINI_API_KEY", envCanary)

	makeInst := func(scope string) systemoneruntime.Instance {
		return systemoneruntime.Instance{
			ID:      "inst-named-1",
			Name:    "gemini-named",
			Adapter: geminidecisions.AdapterName,
			Model:   "gemini-decision-1",
			Auth: systemoneruntime.InstanceAuth{
				Kind:       store.SystemOneAuthKindAPIKey,
				SecretName: systemoneprovider.DefaultGeminiAPIKeySecretName,
			},
			Options: map[string]string{
				systemoneprovider.OptionSecretScope: scope,
			},
			Enabled: true,
		}
	}

	t.Run("allowed_types_environment_variable_legacy_empty_succeed_and_internal_and_file_refused", func(t *testing.T) {
		for _, secType := range []string{
			"",
			store.SecretTypeEnvironment,
			store.SecretTypeVariable,
		} {
			srv := &Server{
				hubID: hubID,
				secretBackend: &stubSystemOneSecretBackend{
					getFn: func(_ context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
						require.Equal(t, "GEMINI_API_KEY", name)
						require.Equal(t, store.ScopeHub, scope)
						require.Equal(t, hubID, scopeID)
						return &secret.SecretWithValue{
							SecretMeta: secret.SecretMeta{
								Name:       name,
								SecretType: secType,
								Scope:      scope,
								ScopeID:    scopeID,
								Version:    4,
							},
							Value: hubCanary,
						}, nil
					},
				},
			}
			src := &hubSystemOneCredentialSource{srv: srv}
			tok, err := src.APIKeyToken(context.Background(), makeInst(systemoneprovider.SecretScopeHub))
			require.NoError(t, err, "expected secretType=%q to succeed", secType)
			assert.Equal(t, hubCanary, tok)
		}

		// Both internal and file secrets are refused with ErrAuth.
		for _, disallowedType := range []string{
			store.SecretTypeInternal,
			store.SecretTypeFile,
		} {
			srvDisallowed := &Server{
				hubID: hubID,
				secretBackend: &stubSystemOneSecretBackend{
					getFn: func(_ context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
						return &secret.SecretWithValue{
							SecretMeta: secret.SecretMeta{
								Name:       name,
								SecretType: disallowedType,
								Scope:      scope,
								ScopeID:    scopeID,
								Version:    1,
							},
							Value: hubCanary,
						}, nil
					},
				},
			}
			srcDisallowed := &hubSystemOneCredentialSource{srv: srvDisallowed}
			tok, err := srcDisallowed.APIKeyToken(context.Background(), makeInst(systemoneprovider.SecretScopeHub))
			assert.Empty(t, tok, "expected secretType=%q to be refused", disallowedType)
			require.ErrorIs(t, err, egress.ErrAuth, "expected secretType=%q to fail with ErrAuth", disallowedType)
		}

		// Hub infrastructure keys are refused BEFORE consulting SecretBackend even if a legacy row had SecretType=="".
		for _, infraKey := range []string{
			SecretKeyUserSigningKey,
			SecretKeyAgentSigningKey,
			SecretKeyDownloadSigningKey,
			SecretKeyArtifactViewKey,
			SecretKeyAttachmentDeliveryKey,
			SecretKeyPortViewKey,
			SecretKeyListCursorKey,
			"oidc_signing_key",
			"oidc_signing_keyset",
			conduitGrantKeySecretName,
		} {
			var backendCalls atomic.Int32
			srvInfra := &Server{
				hubID: hubID,
				secretBackend: &stubSystemOneSecretBackend{
					getFn: func(_ context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
						backendCalls.Add(1)
						return &secret.SecretWithValue{
							SecretMeta: secret.SecretMeta{Name: name, SecretType: "", Scope: scope, ScopeID: scopeID, Version: 1},
							Value:      hubCanary,
						}, nil
					},
				},
			}
			instInfra := makeInst(systemoneprovider.SecretScopeHub)
			instInfra.Auth.SecretName = infraKey
			tok, err := (&hubSystemOneCredentialSource{srv: srvInfra}).APIKeyToken(context.Background(), instInfra)
			assert.Empty(t, tok, infraKey)
			require.ErrorIs(t, err, egress.ErrAuth, infraKey)
			assert.Zero(t, backendCalls.Load(), "SecretBackend.Get must never be called for Hub infrastructure key %q", infraKey)
		}
	})

	t.Run("project_or_hub_fallback_matrix_only_falls_back_on_definite_not_found", func(t *testing.T) {
		ctxProj := systemoneruntime.WithEvalProjectID(context.Background(), projID)

		// 1. Definite store.ErrNotFound and codes.NotFound fall back to hub.
		for _, nfErr := range []error{
			store.ErrNotFound,
			status.Error(codes.NotFound, "secret not found"),
		} {
			var hubLookups atomic.Int32
			srv := &Server{
				hubID: hubID,
				secretBackend: &stubSystemOneSecretBackend{
					getFn: func(_ context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
						switch scope {
						case store.ScopeProject:
							require.Equal(t, projID, scopeID)
							return nil, nfErr
						case store.ScopeHub:
							hubLookups.Add(1)
							require.Equal(t, hubID, scopeID)
							return &secret.SecretWithValue{
								SecretMeta: secret.SecretMeta{Name: name, SecretType: store.SecretTypeVariable, Version: 7},
								Value:      hubCanary,
							}, nil
						default:
							t.Fatalf("forbidden secret scope consulted: %s/%s", scope, scopeID)
							return nil, egress.ErrAuth
						}
					},
				},
			}
			src := &hubSystemOneCredentialSource{srv: srv}
			tok, err := src.APIKeyToken(ctxProj, makeInst(systemoneprovider.SecretScopeProjectOrHub))
			require.NoError(t, err)
			assert.Equal(t, hubCanary, tok)
			assert.Equal(t, int32(1), hubLookups.Load())
		}

		// 2. Non-fallback faults on project lookup NEVER fall back to hub even when hub secret exists!
		for _, tc := range []struct {
			name     string
			projResp *secret.SecretWithValue
			projErr  error
			wantErr  error
		}{
			{
				name: "project secret has disallowed internal type",
				projResp: &secret.SecretWithValue{
					SecretMeta: secret.SecretMeta{SecretType: store.SecretTypeInternal, Version: 1},
					Value:      projCanary,
				},
				wantErr: egress.ErrAuth,
			},
			{
				name: "project secret has disallowed file type",
				projResp: &secret.SecretWithValue{
					SecretMeta: secret.SecretMeta{SecretType: store.SecretTypeFile, Version: 1},
					Value:      projCanary,
				},
				wantErr: egress.ErrAuth,
			},
			{
				name: "project secret has empty/whitespace value",
				projResp: &secret.SecretWithValue{
					SecretMeta: secret.SecretMeta{SecretType: store.SecretTypeVariable, Version: 1},
					Value:      "   ",
				},
				wantErr: egress.ErrAuth,
			},
			{
				name: "project secret has control-char value",
				projResp: &secret.SecretWithValue{
					SecretMeta: secret.SecretMeta{SecretType: store.SecretTypeVariable, Version: 1},
					Value:      projCanary + "\nline2",
				},
				wantErr: egress.ErrAuth,
			},
			{
				name:    "project lookup returns PermissionError",
				projErr: &secret.PermissionError{Err: errors.New("denied " + projCanary)},
				wantErr: egress.ErrAuth,
			},
			{
				name:    "project lookup returns gRPC PermissionDenied",
				projErr: status.Error(codes.PermissionDenied, "iam denied"),
				wantErr: egress.ErrAuth,
			},
			{
				name:    "project lookup returns backend fault",
				projErr: status.Error(codes.Unavailable, "secret manager unavailable"),
				wantErr: egress.ErrCredentialUnavailable,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var hubLookups atomic.Int32
				srv := &Server{
					hubID: hubID,
					secretBackend: &stubSystemOneSecretBackend{
						getFn: func(_ context.Context, _ string, scope, scopeID string) (*secret.SecretWithValue, error) {
							if scope == store.ScopeHub {
								hubLookups.Add(1)
								return &secret.SecretWithValue{
									SecretMeta: secret.SecretMeta{SecretType: store.SecretTypeVariable, Version: 9},
									Value:      hubCanary,
								}, nil
							}
							require.Equal(t, store.ScopeProject, scope)
							require.Equal(t, projID, scopeID)
							return tc.projResp, tc.projErr
						},
					},
				}
				src := &hubSystemOneCredentialSource{srv: srv}
				tok, err := src.APIKeyToken(ctxProj, makeInst(systemoneprovider.SecretScopeProjectOrHub))
				assert.Empty(t, tok)
				require.ErrorIs(t, err, tc.wantErr)
				assert.Zero(t, hubLookups.Load(), "must NEVER fall back to hub on %s", tc.name)
				assert.NotContains(t, err.Error(), projCanary)
				assert.NotContains(t, err.Error(), hubCanary)
			})
		}
	})

	t.Run("never_reads_env_user_broker_or_other_project_secrets", func(t *testing.T) {
		ctxProj := systemoneruntime.WithEvalProjectID(context.Background(), projID)
		var seenScopes []string
		srv := &Server{
			hubID: hubID,
			secretBackend: &stubSystemOneSecretBackend{
				getFn: func(_ context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
					seenScopes = append(seenScopes, scope+":"+scopeID)
					switch {
					case scope == store.ScopeUser:
						return &secret.SecretWithValue{Value: userCanary}, nil
					case scope == store.ScopeRuntimeBroker:
						return &secret.SecretWithValue{Value: brokerCanary}, nil
					case scope == store.ScopeProject && scopeID != projID:
						return &secret.SecretWithValue{Value: otherCanary}, nil
					default:
						return nil, store.ErrNotFound
					}
				},
			},
		}
		src := &hubSystemOneCredentialSource{srv: srv}
		tok, err := src.APIKeyToken(ctxProj, makeInst(systemoneprovider.SecretScopeProjectOrHub))
		assert.Empty(t, tok)
		require.ErrorIs(t, err, egress.ErrAuth)
		assert.Equal(t, []string{
			store.ScopeProject + ":" + projID,
			store.ScopeHub + ":" + hubID,
		}, seenScopes, "must consult ONLY evalProject then hub, never env/user/broker/other-project")

		// Reserved prefix smuggling is refused before consulting SecretBackend.
		reservedInst := makeInst(systemoneprovider.SecretScopeHub)
		reservedInst.Auth.SecretName = "systemone_instance_other"
		seenScopes = nil
		tok2, err2 := src.APIKeyToken(ctxProj, reservedInst)
		assert.Empty(t, tok2)
		require.ErrorIs(t, err2, egress.ErrAuth)
		assert.Empty(t, seenScopes)
	})
}

func TestHubSystemOneRuntime_ProductionInitHasNoTestingOptions(t *testing.T) {
	srv := &Server{}
	srv.initSystemOneRuntime()
	srv.mu.RLock()
	gotOpts := len(srv.systemOneRuntimeOpts)
	rt := srv.systemOneRuntime
	srv.mu.RUnlock()
	assert.Zero(t, gotOpts, "production Hub initSystemOneRuntime must pass zero testing options")
	require.NotNil(t, rt)
}

func TestSystemOneNamedSecret_ProviderPinnedSecretNamePolicyBeforeLookup(t *testing.T) {
	const (
		hubID     = "hub-item8-unit"
		projID    = "proj-item8-id"
		secretVal = "CANARY_ITEM8_NEVER_READ_8414"
	)
	ctx := systemoneruntime.WithEvalProjectID(context.Background(), projID)

	var backendGetCalls atomic.Int32
	srv := &Server{
		hubID: hubID,
		secretBackend: &stubSystemOneSecretBackend{
			getFn: func(_ context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
				backendGetCalls.Add(1)
				return &secret.SecretWithValue{
					SecretMeta: secret.SecretMeta{
						Name:       name,
						SecretType: store.SecretTypeVariable,
						Scope:      scope,
						ScopeID:    scopeID,
						Version:    1,
					},
					Value: secretVal,
				}, nil
			},
		},
	}
	src := &hubSystemOneCredentialSource{srv: srv}

	// 1. Absent SecretNamePolicy (typesafe) -> refused BEFORE SecretBackend.Get across all scopes.
	for _, sc := range []string{
		systemoneprovider.SecretScopeHub,
		systemoneprovider.SecretScopeProject,
		systemoneprovider.SecretScopeProjectOrHub,
	} {
		backendGetCalls.Store(0)
		tok, err := src.APIKeyToken(ctx, systemoneruntime.Instance{
			ID:       "inst-ts-ref",
			Name:     "ts-ref",
			Adapter:  "typesafe",
			Model:    "jev-1.14.0",
			Endpoint: "https://api.typesafe.ai",
			Auth: systemoneruntime.InstanceAuth{
				Kind:       store.SystemOneAuthKindAPIKey,
				SecretName: "GEMINI_API_KEY",
			},
			Options: map[string]string{systemoneprovider.OptionSecretScope: sc},
			Enabled: true,
		})
		assert.Empty(t, tok)
		require.ErrorIs(t, err, egress.ErrAuth)
		assert.Zero(t, backendGetCalls.Load(), "SecretBackend.Get must not be called for adapter without SecretNamePolicy (scope=%s)", sc)
	}

	// 2. Disallowed / case-folded / suffixed / unrelated / infrastructure names on gemini_decisions -> 0 SecretBackend.Get calls across all scopes (including project_or_hub).
	for _, badName := range []string{
		"GITHUB_TOKEN",
		"ANTHROPIC_API_KEY",
		"gemini_api_key",
		"GEMINI_API_KEY_2",
		"MY_GEMINI_API_KEY",
		SecretKeyUserSigningKey,
		SecretKeyAgentSigningKey,
		"systemone.instance.00000000-0000-4000-8000-000000000099",
		"systemone_instance_other",
	} {
		for _, sc := range []string{
			systemoneprovider.SecretScopeHub,
			systemoneprovider.SecretScopeProject,
			systemoneprovider.SecretScopeProjectOrHub,
		} {
			backendGetCalls.Store(0)
			tok, err := src.APIKeyToken(ctx, systemoneruntime.Instance{
				ID:       "inst-gem-bad",
				Name:     "gem-bad",
				Adapter:  geminidecisions.AdapterName,
				Model:    "gemini-decision-1",
				Endpoint: geminidecisions.DefaultEndpoint,
				Auth: systemoneruntime.InstanceAuth{
					Kind:       store.SystemOneAuthKindAPIKey,
					SecretName: badName,
				},
				Options: map[string]string{systemoneprovider.OptionSecretScope: sc},
				Enabled: true,
			})
			assert.Empty(t, tok, "name=%q scope=%s", badName, sc)
			require.ErrorIs(t, err, egress.ErrAuth, "name=%q scope=%s", badName, sc)
			assert.Zero(t, backendGetCalls.Load(), "SecretBackend.Get must never be called for disallowed name %q (scope=%s)", badName, sc)
		}
	}

	// 3. Exact allowed name ("GEMINI_API_KEY") on gemini_decisions succeeds.
	backendGetCalls.Store(0)
	tok, err := src.APIKeyToken(ctx, systemoneruntime.Instance{
		ID:       "inst-gem-ok",
		Name:     "gem-ok",
		Adapter:  geminidecisions.AdapterName,
		Model:    "gemini-decision-1",
		Endpoint: geminidecisions.DefaultEndpoint,
		Auth: systemoneruntime.InstanceAuth{
			Kind:       store.SystemOneAuthKindAPIKey,
			SecretName: "GEMINI_API_KEY",
		},
		Options: map[string]string{systemoneprovider.OptionSecretScope: systemoneprovider.SecretScopeHub},
		Enabled: true,
	})
	require.NoError(t, err)
	assert.Equal(t, secretVal, tok)
	assert.Equal(t, int32(1), backendGetCalls.Load())
}
