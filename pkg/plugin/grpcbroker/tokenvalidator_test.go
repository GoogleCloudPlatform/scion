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

package grpcbroker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin/refbroker"
	brokerv1 "github.com/GoogleCloudPlatform/scion/proto/broker/v1"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// testJWKSServer creates a test JWKS HTTP server with the given key.
func testJWKSServer(t *testing.T, key *ecdsa.PrivateKey, kid string) *httptest.Server {
	t.Helper()
	jwk := jose.JSONWebKey{
		Key:       &key.PublicKey,
		KeyID:     kid,
		Algorithm: string(jose.ES256),
		Use:       "sig",
	}
	jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk}}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	}))
}

// signTestIDToken creates a signed JWT with the given claims.
func signTestIDToken(t *testing.T, key *ecdsa.PrivateKey, kid string, claims interface{}) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: key},
		(&jose.SignerOptions{}).WithHeader("kid", kid),
	)
	require.NoError(t, err)
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	require.NoError(t, err)
	return raw
}

// --- GoogleIDTokenValidator tests ---

func TestGoogleIDTokenValidator_ValidToken(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	kid := "test-key-1"
	jwksServer := testJWKSServer(t, key, kid)
	defer jwksServer.Close()

	validator, err := NewGoogleIDTokenValidator(GoogleIDTokenValidatorConfig{
		Audience:           "https://bridge.example.com",
		AuthorizedSubjects: []string{"hub-sa@project.iam.gserviceaccount.com"},
		JWKSURL:            jwksServer.URL,
	})
	require.NoError(t, err)

	claims := googleIDTokenClaims{
		Claims: jwt.Claims{
			Issuer:   GoogleIssuerV2,
			Audience: jwt.Audience{"https://bridge.example.com"},
			Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Email:         "hub-sa@project.iam.gserviceaccount.com",
		EmailVerified: true,
	}
	token := signTestIDToken(t, key, kid, claims)

	err = validator.ValidateToken(context.Background(), token)
	require.NoError(t, err)
}

func TestGoogleIDTokenValidator_WrongAudience(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	kid := "test-key-2"
	jwksServer := testJWKSServer(t, key, kid)
	defer jwksServer.Close()

	validator, err := NewGoogleIDTokenValidator(GoogleIDTokenValidatorConfig{
		Audience:           "https://bridge.example.com",
		AuthorizedSubjects: []string{"hub-sa@project.iam.gserviceaccount.com"},
		JWKSURL:            jwksServer.URL,
	})
	require.NoError(t, err)

	claims := googleIDTokenClaims{
		Claims: jwt.Claims{
			Issuer:   GoogleIssuerV2,
			Audience: jwt.Audience{"https://wrong-service.example.com"},
			Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Email:         "hub-sa@project.iam.gserviceaccount.com",
		EmailVerified: true,
	}
	token := signTestIDToken(t, key, kid, claims)

	err = validator.ValidateToken(context.Background(), token)
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.Unauthenticated, st.Code())
	assert.Contains(t, st.Message(), "audience mismatch")
}

func TestGoogleIDTokenValidator_ExpiredToken(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	kid := "test-key-3"
	jwksServer := testJWKSServer(t, key, kid)
	defer jwksServer.Close()

	validator, err := NewGoogleIDTokenValidator(GoogleIDTokenValidatorConfig{
		Audience:           "https://bridge.example.com",
		AuthorizedSubjects: []string{"hub-sa@project.iam.gserviceaccount.com"},
		JWKSURL:            jwksServer.URL,
	})
	require.NoError(t, err)

	claims := googleIDTokenClaims{
		Claims: jwt.Claims{
			Issuer:   GoogleIssuerV2,
			Audience: jwt.Audience{"https://bridge.example.com"},
			Expiry:   jwt.NewNumericDate(time.Now().Add(-time.Hour)), // expired
		},
		Email:         "hub-sa@project.iam.gserviceaccount.com",
		EmailVerified: true,
	}
	token := signTestIDToken(t, key, kid, claims)

	err = validator.ValidateToken(context.Background(), token)
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.Unauthenticated, st.Code())
	assert.Contains(t, st.Message(), "expired")
}

func TestGoogleIDTokenValidator_WrongIssuer(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	kid := "test-key-4"
	jwksServer := testJWKSServer(t, key, kid)
	defer jwksServer.Close()

	validator, err := NewGoogleIDTokenValidator(GoogleIDTokenValidatorConfig{
		Audience:           "https://bridge.example.com",
		AuthorizedSubjects: []string{"hub-sa@project.iam.gserviceaccount.com"},
		JWKSURL:            jwksServer.URL,
	})
	require.NoError(t, err)

	claims := googleIDTokenClaims{
		Claims: jwt.Claims{
			Issuer:   "https://evil-issuer.example.com",
			Audience: jwt.Audience{"https://bridge.example.com"},
			Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Email:         "hub-sa@project.iam.gserviceaccount.com",
		EmailVerified: true,
	}
	token := signTestIDToken(t, key, kid, claims)

	err = validator.ValidateToken(context.Background(), token)
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.Unauthenticated, st.Code())
	assert.Contains(t, st.Message(), "invalid issuer")
}

// --- Critical negative test: GE invoker cannot call control RPCs ---

func TestGoogleIDTokenValidator_GEInvoker_CannotCallControlRPCs(t *testing.T) {
	// This test proves that a valid Google ID token from the GE Discovery
	// Engine (or any other service with Cloud Run invoker permission) is
	// rejected by the bridge's application-level auth, because its email
	// is not in the authorized_subjects list.
	//
	// Cloud Run invocation permission alone does NOT authorize broker RPCs.

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	kid := "test-key-ge"
	jwksServer := testJWKSServer(t, key, kid)
	defer jwksServer.Close()

	// Bridge validator authorizes ONLY the Hub's service account.
	validator, err := NewGoogleIDTokenValidator(GoogleIDTokenValidatorConfig{
		Audience:           "https://bridge-abc123.run.app",
		AuthorizedSubjects: []string{"hub-sa@hub-project.iam.gserviceaccount.com"},
		JWKSURL:            jwksServer.URL,
	})
	require.NoError(t, err)

	// GE Discovery Engine uses a different service account.
	geInvokerClaims := googleIDTokenClaims{
		Claims: jwt.Claims{
			Issuer:   GoogleIssuerV2,
			Audience: jwt.Audience{"https://bridge-abc123.run.app"},
			Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
			Subject:  "112233445566778899", // GE SA numeric ID
		},
		Email:         "ge-discovery@ge-project.iam.gserviceaccount.com",
		EmailVerified: true,
	}
	geToken := signTestIDToken(t, key, kid, geInvokerClaims)

	// GE invoker's token has valid signature, correct audience, valid expiry,
	// and valid issuer — but the wrong email. Application-level auth rejects it.
	err = validator.ValidateToken(context.Background(), geToken)
	require.Error(t, err, "GE invoker must be denied control RPC access")
	st, _ := status.FromError(err)
	assert.Equal(t, codes.PermissionDenied, st.Code())
	assert.Contains(t, st.Message(), "not authorized")

	// Now test this end-to-end through the gRPC interceptor.
	broker := refbroker.New(slog.Default())
	defer func() { _ = broker.Close() }()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	s := grpc.NewServer(
		grpc.UnaryInterceptor(UnaryAuthInterceptor(validator)),
		grpc.StreamInterceptor(StreamAuthInterceptor(validator)),
	)
	brokerv1.RegisterBrokerServiceServer(s, NewServer(broker))
	go func() { _ = s.Serve(lis) }()
	defer s.GracefulStop()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(NewTokenSourceCredentials(
			newMockTokenSource(geToken), false)),
	)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	client := brokerv1.NewBrokerServiceClient(conn)
	ctx := context.Background()

	// Every control method must be denied.
	_, err = client.Configure(ctx, &brokerv1.ConfigureRequest{Config: map[string]string{}})
	require.Error(t, err, "GE invoker must not call Configure")

	_, err = client.Publish(ctx, &brokerv1.PublishRequest{
		Topic:   "test",
		Message: &brokerv1.StructuredMessage{Version: 1, Msg: "exploit"},
	})
	require.Error(t, err, "GE invoker must not call Publish")

	_, err = client.Subscribe(ctx, &brokerv1.SubscribeRequest{Pattern: "test.>"})
	require.Error(t, err, "GE invoker must not call Subscribe")

	_, err = client.GetInfo(ctx, &brokerv1.GetInfoRequest{})
	require.Error(t, err, "GE invoker must not call GetInfo")

	_, err = client.HealthCheck(ctx, &brokerv1.HealthCheckRequest{})
	require.Error(t, err, "GE invoker must not call HealthCheck")

	_, err = client.Unsubscribe(ctx, &brokerv1.UnsubscribeRequest{Pattern: "test.>"})
	require.Error(t, err, "GE invoker must not call Unsubscribe")
}

func TestGoogleIDTokenValidator_NoEmailClaim(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	kid := "test-key-noemail"
	jwksServer := testJWKSServer(t, key, kid)
	defer jwksServer.Close()

	validator, err := NewGoogleIDTokenValidator(GoogleIDTokenValidatorConfig{
		Audience:           "https://bridge.example.com",
		AuthorizedSubjects: []string{"hub-sa@project.iam.gserviceaccount.com"},
		JWKSURL:            jwksServer.URL,
	})
	require.NoError(t, err)

	// Token without email claim.
	claims := googleIDTokenClaims{
		Claims: jwt.Claims{
			Issuer:   GoogleIssuerV2,
			Audience: jwt.Audience{"https://bridge.example.com"},
			Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		// No Email field set.
	}
	token := signTestIDToken(t, key, kid, claims)

	err = validator.ValidateToken(context.Background(), token)
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.PermissionDenied, st.Code())
	assert.Contains(t, st.Message(), "no email claim")
}

func TestGoogleIDTokenValidator_UnverifiedEmail(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	kid := "test-key-unverified"
	jwksServer := testJWKSServer(t, key, kid)
	defer jwksServer.Close()

	validator, err := NewGoogleIDTokenValidator(GoogleIDTokenValidatorConfig{
		Audience:           "https://bridge.example.com",
		AuthorizedSubjects: []string{"hub-sa@project.iam.gserviceaccount.com"},
		JWKSURL:            jwksServer.URL,
	})
	require.NoError(t, err)

	claims := googleIDTokenClaims{
		Claims: jwt.Claims{
			Issuer:   GoogleIssuerV2,
			Audience: jwt.Audience{"https://bridge.example.com"},
			Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Email:         "hub-sa@project.iam.gserviceaccount.com",
		EmailVerified: false, // not verified
	}
	token := signTestIDToken(t, key, kid, claims)

	err = validator.ValidateToken(context.Background(), token)
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.PermissionDenied, st.Code())
	assert.Contains(t, st.Message(), "not verified")
}

func TestGoogleIDTokenValidator_WrongSigningKey(t *testing.T) {
	// Token signed with a different key than what's in JWKS.
	signingKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	jwksKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	kid := "test-key-wrong"
	jwksServer := testJWKSServer(t, jwksKey, kid)
	defer jwksServer.Close()

	validator, err := NewGoogleIDTokenValidator(GoogleIDTokenValidatorConfig{
		Audience:           "https://bridge.example.com",
		AuthorizedSubjects: []string{"hub-sa@project.iam.gserviceaccount.com"},
		JWKSURL:            jwksServer.URL,
	})
	require.NoError(t, err)

	claims := googleIDTokenClaims{
		Claims: jwt.Claims{
			Issuer:   GoogleIssuerV2,
			Audience: jwt.Audience{"https://bridge.example.com"},
			Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Email:         "hub-sa@project.iam.gserviceaccount.com",
		EmailVerified: true,
	}
	token := signTestIDToken(t, signingKey, kid, claims)

	err = validator.ValidateToken(context.Background(), token)
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.Unauthenticated, st.Code())
	assert.Contains(t, st.Message(), "signature verification failed")
}

func TestGoogleIDTokenValidator_MissingAudience(t *testing.T) {
	_, err := NewGoogleIDTokenValidator(GoogleIDTokenValidatorConfig{
		AuthorizedSubjects: []string{"sa@project.iam.gserviceaccount.com"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "audience is required")
}

func TestGoogleIDTokenValidator_BothGoogleIssuers(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	kid := "test-key-issuers"
	jwksServer := testJWKSServer(t, key, kid)
	defer jwksServer.Close()

	validator, err := NewGoogleIDTokenValidator(GoogleIDTokenValidatorConfig{
		Audience: "https://bridge.example.com",
		JWKSURL:  jwksServer.URL,
	})
	require.NoError(t, err)

	for _, issuer := range []string{GoogleIssuerV1, GoogleIssuerV2} {
		t.Run(issuer, func(t *testing.T) {
			claims := googleIDTokenClaims{
				Claims: jwt.Claims{
					Issuer:   issuer,
					Audience: jwt.Audience{"https://bridge.example.com"},
					Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
				},
			}
			token := signTestIDToken(t, key, kid, claims)
			err := validator.ValidateToken(context.Background(), token)
			require.NoError(t, err)
		})
	}
}

// --- HMAC validator tests ---

func TestHMACTokenValidator_ValidToken(t *testing.T) {
	hmacKey := []byte("test-hmac-signing-key-32-bytes!!")

	validator, err := NewHMACTokenValidator(HMACTokenValidatorConfig{
		Key:                hmacKey,
		Audience:           "bridge-service",
		AuthorizedSubjects: []string{"hub-server"},
	})
	require.NoError(t, err)

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: hmacKey},
		nil,
	)
	require.NoError(t, err)

	claims := jwt.Claims{
		Issuer:   "hub",
		Subject:  "hub-server",
		Audience: jwt.Audience{"bridge-service"},
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}
	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	require.NoError(t, err)

	err = validator.ValidateToken(context.Background(), token)
	require.NoError(t, err)
}

func TestHMACTokenValidator_WrongKey(t *testing.T) {
	signingKey := []byte("signing-key-32-bytes-long!!!!!!!!")
	wrongKey := []byte("wrong-key-32-bytes-long!!!!!!!!!!")

	validator, err := NewHMACTokenValidator(HMACTokenValidatorConfig{
		Key:      wrongKey,
		Audience: "bridge",
	})
	require.NoError(t, err)

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: signingKey},
		nil,
	)
	require.NoError(t, err)

	claims := jwt.Claims{
		Audience: jwt.Audience{"bridge"},
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}
	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	require.NoError(t, err)

	err = validator.ValidateToken(context.Background(), token)
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.Unauthenticated, st.Code())
}

func TestHMACTokenValidator_UnauthorizedSubject(t *testing.T) {
	hmacKey := []byte("test-hmac-key-32-bytes-long!!!!!")

	validator, err := NewHMACTokenValidator(HMACTokenValidatorConfig{
		Key:                hmacKey,
		Audience:           "bridge",
		AuthorizedSubjects: []string{"hub-server"},
	})
	require.NoError(t, err)

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: hmacKey},
		nil,
	)
	require.NoError(t, err)

	claims := jwt.Claims{
		Subject:  "attacker-service",
		Audience: jwt.Audience{"bridge"},
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}
	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	require.NoError(t, err)

	err = validator.ValidateToken(context.Background(), token)
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.PermissionDenied, st.Code())
	assert.Contains(t, st.Message(), "not authorized")
}

// --- Config validation tests ---

func TestValidateStandaloneServerConfig_GoogleIDToken_Valid(t *testing.T) {
	err := ValidateStandaloneServerConfig(StandaloneServerConfig{
		AuthMode:           AuthModeGoogleIDToken,
		Audience:           "https://bridge.example.com",
		AuthorizedSubjects: []string{"hub-sa@project.iam.gserviceaccount.com"},
		ListenAddress:      ":9090",
	})
	require.NoError(t, err)
}

func TestValidateStandaloneServerConfig_GoogleIDToken_MissingAudience(t *testing.T) {
	err := ValidateStandaloneServerConfig(StandaloneServerConfig{
		AuthMode:           AuthModeGoogleIDToken,
		AuthorizedSubjects: []string{"hub-sa@project.iam.gserviceaccount.com"},
		ListenAddress:      ":9090",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "audience is required")
}

func TestValidateStandaloneServerConfig_GoogleIDToken_MissingSubjects(t *testing.T) {
	err := ValidateStandaloneServerConfig(StandaloneServerConfig{
		AuthMode:      AuthModeGoogleIDToken,
		Audience:      "https://bridge.example.com",
		ListenAddress: ":9090",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "authorized_subjects is required")
}

func TestValidateStandaloneServerConfig_HMAC_Valid(t *testing.T) {
	err := ValidateStandaloneServerConfig(StandaloneServerConfig{
		AuthMode:      AuthModeHMAC,
		Audience:      "bridge-service",
		HMACKey:       []byte("secret-key"),
		ListenAddress: "localhost:9090",
	})
	require.NoError(t, err)
}

func TestValidateStandaloneServerConfig_HMAC_MissingKey(t *testing.T) {
	err := ValidateStandaloneServerConfig(StandaloneServerConfig{
		AuthMode:      AuthModeHMAC,
		Audience:      "bridge-service",
		ListenAddress: "localhost:9090",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hmac_key is required")
}

func TestValidateStandaloneServerConfig_LocalDev_LocalAddress(t *testing.T) {
	err := ValidateStandaloneServerConfig(StandaloneServerConfig{
		AuthMode:      AuthModeLocalDev,
		ListenAddress: "localhost:9090",
	})
	require.NoError(t, err)
}

func TestValidateStandaloneServerConfig_LocalDev_RemoteAddress_FailsClosed(t *testing.T) {
	err := ValidateStandaloneServerConfig(StandaloneServerConfig{
		AuthMode:      AuthModeLocalDev,
		ListenAddress: "0.0.0.0:9090",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "local_dev auth mode is only allowed for local addresses")
}

func TestValidateStandaloneServerConfig_NoAuth_RemoteAddress_FailsClosed(t *testing.T) {
	err := ValidateStandaloneServerConfig(StandaloneServerConfig{
		AuthMode:      "",
		ListenAddress: "0.0.0.0:9090",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth_mode is required for non-local")
}

func TestValidateStandaloneServerConfig_NoAuth_LocalAddress_Allowed(t *testing.T) {
	err := ValidateStandaloneServerConfig(StandaloneServerConfig{
		AuthMode:      "",
		ListenAddress: "localhost:9090",
	})
	require.NoError(t, err)
}

func TestValidateStandaloneServerConfig_UnsupportedAuthMode(t *testing.T) {
	err := ValidateStandaloneServerConfig(StandaloneServerConfig{
		AuthMode:      "kerberos",
		ListenAddress: "localhost:9090",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported auth_mode")
}

// --- TLS config validation tests ---

func TestValidateStandaloneServerConfig_TLS_CertWithoutKey(t *testing.T) {
	err := ValidateStandaloneServerConfig(StandaloneServerConfig{
		AuthMode:      AuthModeLocalDev,
		ListenAddress: "localhost:9090",
		TLSCertFile:   "/path/to/cert.pem",
		// TLSKeyFile intentionally omitted.
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tls_cert_file requires tls_key_file")
}

func TestValidateStandaloneServerConfig_TLS_KeyWithoutCert(t *testing.T) {
	err := ValidateStandaloneServerConfig(StandaloneServerConfig{
		AuthMode:      AuthModeLocalDev,
		ListenAddress: "localhost:9090",
		TLSKeyFile:    "/path/to/key.pem",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tls_key_file requires tls_cert_file")
}

func TestValidateStandaloneServerConfig_TLS_ClientCAWithoutServerCert(t *testing.T) {
	err := ValidateStandaloneServerConfig(StandaloneServerConfig{
		AuthMode:        AuthModeLocalDev,
		ListenAddress:   "localhost:9090",
		TLSClientCAFile: "/path/to/ca.pem",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tls_client_ca_file requires tls_cert_file")
}

func TestValidateStandaloneServerConfig_TLS_ValidCertAndKey(t *testing.T) {
	err := ValidateStandaloneServerConfig(StandaloneServerConfig{
		AuthMode:      AuthModeLocalDev,
		ListenAddress: "localhost:9090",
		TLSCertFile:   "/path/to/cert.pem",
		TLSKeyFile:    "/path/to/key.pem",
	})
	require.NoError(t, err)
}

// --- BuildStandaloneServerOptions tests ---

func TestBuildStandaloneServerOptions_LocalDev(t *testing.T) {
	opts, err := BuildStandaloneServerOptions(StandaloneServerConfig{
		AuthMode:      AuthModeLocalDev,
		ListenAddress: "localhost:9090",
	})
	require.NoError(t, err)
	assert.Empty(t, opts, "local_dev should produce no server options")
}

func TestBuildStandaloneServerOptions_HMAC(t *testing.T) {
	opts, err := BuildStandaloneServerOptions(StandaloneServerConfig{
		AuthMode:      AuthModeHMAC,
		Audience:      "bridge",
		HMACKey:       []byte("test-key-32-bytes-long!!!!!!!!!"),
		ListenAddress: "localhost:9090",
	})
	require.NoError(t, err)
	// Should have unary + stream interceptors.
	assert.Len(t, opts, 2)
}

func TestBuildStandaloneServerOptions_GoogleIDToken(t *testing.T) {
	opts, err := BuildStandaloneServerOptions(StandaloneServerConfig{
		AuthMode:           AuthModeGoogleIDToken,
		Audience:           "https://bridge.example.com",
		AuthorizedSubjects: []string{"hub-sa@project.iam.gserviceaccount.com"},
		ListenAddress:      ":9090",
	})
	require.NoError(t, err)
	assert.Len(t, opts, 2) // unary + stream interceptors
}

// --- Factory fail-closed tests ---

func TestNewAdapterFromEntry_RemoteAddress_NoAuth_FailsClosed(t *testing.T) {
	// Remote address without auth must fail closed (not warn and continue).
	entry := plugin.PluginEntry{
		Address: "bridge.example.com:443",
		Mode:    "grpc",
	}
	_, err := NewAdapterFromEntry(entry, slog.Default())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth_type is required for remote")
}

func TestNewAdapterFromEntry_RemoteAddress_ExplicitNone_FailsClosed(t *testing.T) {
	// Explicit "none" for remote address must fail closed.
	entry := plugin.PluginEntry{
		Address:  "bridge.example.com:443",
		Mode:     "grpc",
		AuthType: "none",
	}
	_, err := NewAdapterFromEntry(entry, slog.Default())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not allowed for remote")
}

func TestNewAdapterFromEntry_LocalAddress_NoAuth_Allowed(t *testing.T) {
	// Local address without auth is allowed (backward compatible).
	broker := refbroker.New(slog.Default())
	defer func() { _ = broker.Close() }()

	addr, stop := startTestServer(t, broker)
	defer stop()

	entry := plugin.PluginEntry{
		Address: addr,
		Mode:    "grpc",
	}
	client, err := NewAdapterFromEntry(entry, slog.Default())
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	info, err := client.GetInfo()
	require.NoError(t, err)
	assert.Equal(t, "refbroker", info.Name)
}

func TestNewAdapterFromEntry_LocalAddress_ExplicitNone_Allowed(t *testing.T) {
	// Explicit "none" for local address is allowed.
	broker := refbroker.New(slog.Default())
	defer func() { _ = broker.Close() }()

	addr, stop := startTestServer(t, broker)
	defer stop()

	entry := plugin.PluginEntry{
		Address:  addr,
		Mode:     "grpc",
		AuthType: "none",
	}
	client, err := NewAdapterFromEntry(entry, slog.Default())
	require.NoError(t, err)
	_ = client.Close()
}
