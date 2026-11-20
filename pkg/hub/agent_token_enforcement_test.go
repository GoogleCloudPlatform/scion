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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type refusalLogCapture struct {
	mu     sync.Mutex
	causes []string
}

func (c *refusalLogCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *refusalLogCapture) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *refusalLogCapture) WithGroup(string) slog.Handler            { return c }
func (c *refusalLogCapture) Handle(_ context.Context, r slog.Record) error {
	if r.Message != agentTokenRefusedLogMessage {
		return nil
	}
	var cause string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "cause" {
			cause = a.Value.String()
		}
		return true
	})
	c.mu.Lock()
	c.causes = append(c.causes, cause)
	c.mu.Unlock()
	return nil
}

func (c *refusalLogCapture) recordedCauses() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.causes))
	copy(out, c.causes)
	return out
}

// signAgentTokenWithoutJTI signs a valid HS256 agent JWT whose jti (ID) claim
// is empty, using svc's signing key.
func signAgentTokenWithoutJTI(t *testing.T, svc *AgentTokenService, agentID, projectID string) string {
	t.Helper()
	now := time.Now().UTC()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: svc.config.SigningKey},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	require.NoError(t, err)
	claims := AgentTokenClaims{
		Claims: jwt.Claims{
			Issuer:    AgentTokenIssuer,
			Subject:   agentID,
			Audience:  jwt.Audience{AgentTokenAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			Expiry:    jwt.NewNumericDate(now.Add(time.Hour)),
			ID:        "",
		},
		ProjectID:   projectID,
		Scopes:      []AgentTokenScope{ScopeAgentStatusUpdate},
		ScopeSchema: CurrentAgentScopeSchema,
	}
	tok, err := jwt.Signed(signer).Claims(claims).Serialize()
	require.NoError(t, err)
	return tok
}

func TestValidateAgentToken_RejectsEmptyJTI(t *testing.T) {
	svc, err := NewAgentTokenService(AgentTokenConfig{
		SigningKey:    []byte("test-signing-key-for-empty-jti-check-32b"),
		TokenDuration: time.Hour,
	})
	require.NoError(t, err)

	emptyJTITok := signAgentTokenWithoutJTI(t, svc, "agent-1", "project-1")
	claims, err := svc.ValidateAgentToken(emptyJTITok)
	require.Error(t, err)
	assert.ErrorIs(t, err, errAgentCredentialEmptyJTI)
	assert.Nil(t, claims)
}

func TestVerifyAgentTokenCredential_DefenseInDepth(t *testing.T) {
	credStore := newMemAgentCredentialStore()
	logs := &refusalLogCapture{}
	log := slog.New(logs)

	t.Run("empty jti rejected directly", func(t *testing.T) {
		claims := &AgentTokenClaims{
			Claims: jwt.Claims{Subject: "agent-1", ID: ""},
		}
		_, refusal := verifyAgentTokenCredential(context.Background(), AuthConfig{
			CredentialStore: credStore,
		}, log, claims, runScopeRequest{}, runScopeSourceHTTP)
		assert.Equal(t, agentTokenMissingJTI, refusal)

		rec := httptest.NewRecorder()
		writeAgentTokenRefusal(context.Background(), rec, log, refusal, claims.Subject)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Equal(t, agentTokenRefusedBody, rec.Body.String())
		causes := logs.recordedCauses()
		require.NotEmpty(t, causes)
		assert.Equal(t, agentTokenCauseMissingJTI, causes[len(causes)-1])
	})

	t.Run("nil CredentialStore fails closed", func(t *testing.T) {
		claims := &AgentTokenClaims{
			Claims: jwt.Claims{Subject: "agent-1", ID: "jti-1"},
		}
		_, refusal := verifyAgentTokenCredential(context.Background(), AuthConfig{
			CredentialStore: nil,
		}, log, claims, runScopeRequest{}, runScopeSourceHTTP)
		assert.Equal(t, agentTokenUnevaluated, refusal)
	})
}

func TestAgentAuthMiddleware_CredentialAndJTIEnforcement(t *testing.T) {
	svc, err := NewAgentTokenService(AgentTokenConfig{
		SigningKey:    []byte("test-signing-key-for-middleware-enforce"),
		TokenDuration: time.Hour,
	})
	require.NoError(t, err)

	okHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("unrecorded token rejected with unrecorded cause", func(t *testing.T) {
		credStore := newMemAgentCredentialStore()
		logs := &refusalLogCapture{}
		mw := UnifiedAuthMiddleware(AuthConfig{
			AgentTokenSvc:   svc,
			CredentialStore: credStore,
			Logger:          slog.New(logs),
		})(okHandler)

		unrecordedTok, _, err := svc.SignAgentToken(AgentTokenGrant{
			AgentID:   "agent-unrecorded",
			ProjectID: "proj-1",
			Scopes:    []AgentTokenScope{ScopeAgentStatusUpdate},
		}, "")
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/agent-unrecorded", nil)
		req.Header.Set("X-Scion-Agent-Token", unrecordedTok)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)

		require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		assert.Equal(t, agentTokenRefusedBody, rec.Body.String())
		assert.Equal(t, []string{agentTokenCauseUnrecorded}, logs.recordedCauses())
	})

	t.Run("empty jti token rejected on header and bearer", func(t *testing.T) {
		credStore := newMemAgentCredentialStore()
		logs := &refusalLogCapture{}
		mw := UnifiedAuthMiddleware(AuthConfig{
			AgentTokenSvc:   svc,
			CredentialStore: credStore,
			Logger:          slog.New(logs),
		})(okHandler)

		emptyJTITok := signAgentTokenWithoutJTI(t, svc, "agent-no-jti", "proj-1")

		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/agent-no-jti", nil)
		req.Header.Set("X-Scion-Agent-Token", emptyJTITok)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		assert.Equal(t, agentTokenRefusedBody, rec.Body.String())
		assert.Equal(t, []string{agentTokenCauseInvalidToken}, logs.recordedCauses())

		bearerReq := httptest.NewRequest(http.MethodGet, "/api/v1/agents/agent-no-jti", nil)
		bearerReq.Header.Set("Authorization", "Bearer "+emptyJTITok)
		bearerRec := httptest.NewRecorder()
		mw.ServeHTTP(bearerRec, bearerReq)
		require.Equal(t, http.StatusUnauthorized, bearerRec.Code, bearerRec.Body.String())
	})

	t.Run("recorded and revoked rejected with revoked cause", func(t *testing.T) {
		credStore := newMemAgentCredentialStore()
		logs := &refusalLogCapture{}
		mw := UnifiedAuthMiddleware(AuthConfig{
			AgentTokenSvc:   svc,
			CredentialStore: credStore,
			Logger:          slog.New(logs),
		})(okHandler)

		tok := generateRecordedAgentToken(t, svc, credStore, "agent-revoked", "proj-1", []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
		claims, err := svc.ValidateAgentToken(tok)
		require.NoError(t, err)
		cred, err := credStore.GetAgentCredentialByJTIHash(context.Background(), hashJTI(claims.ID))
		require.NoError(t, err)
		require.NoError(t, credStore.RevokeAgentCredential(context.Background(), cred.ID, "test", "explicit"))

		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/agent-revoked", nil)
		req.Header.Set("X-Scion-Agent-Token", tok)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)

		require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		assert.Equal(t, agentTokenRefusedBody, rec.Body.String())
		assert.Equal(t, []string{agentTokenCauseRevoked}, logs.recordedCauses())
	})

	t.Run("recorded and active passes", func(t *testing.T) {
		credStore := newMemAgentCredentialStore()
		mw := UnifiedAuthMiddleware(AuthConfig{
			AgentTokenSvc:   svc,
			CredentialStore: credStore,
		})(okHandler)

		tok := generateRecordedAgentToken(t, svc, credStore, "agent-active", "proj-1", []AgentTokenScope{ScopeAgentStatusUpdate}, nil)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/agent-active", nil)
		req.Header.Set("X-Scion-Agent-Token", tok)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("store error returns 503", func(t *testing.T) {
		errStore := &fixedErrAgentCredentialStore{err: errors.New("db down")}
		mw := UnifiedAuthMiddleware(AuthConfig{
			AgentTokenSvc:   svc,
			CredentialStore: errStore,
		})(okHandler)

		tok, _, err := svc.SignAgentToken(AgentTokenGrant{
			AgentID:   "agent-err",
			ProjectID: "proj-1",
			Scopes:    []AgentTokenScope{ScopeAgentStatusUpdate},
		}, "")
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/agent-err", nil)
		req.Header.Set("X-Scion-Agent-Token", tok)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)

		require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	})

	t.Run("nil CredentialStore fails closed with 503", func(t *testing.T) {
		mw := UnifiedAuthMiddleware(AuthConfig{
			AgentTokenSvc:   svc,
			CredentialStore: nil,
		})(okHandler)

		tok, _, err := svc.SignAgentToken(AgentTokenGrant{
			AgentID:   "agent-nil-store",
			ProjectID: "proj-1",
			Scopes:    []AgentTokenScope{ScopeAgentStatusUpdate},
		}, "")
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/agent-nil-store", nil)
		req.Header.Set("X-Scion-Agent-Token", tok)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)

		require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	})
}

type fixedErrAgentCredentialStore struct {
	store.AgentCredentialStore
	err error
}

func (f *fixedErrAgentCredentialStore) GetAgentCredentialByJTIHash(_ context.Context, _ string) (*store.AgentCredential, error) {
	return nil, f.err
}
