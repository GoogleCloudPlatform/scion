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

package hub

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentRefresh_RequiresRecordedCredentialAndJTI(t *testing.T) {
	t.Run("unrecorded token rejected 401 with refresh_unrecorded cause", func(t *testing.T) {
		srv, s, _, project := setupCredentialTestServer(t)
		logs := &captureHandler{}
		srv.authConfig.Logger = slog.New(logs)

		agentID := tid("agent-refresh-unrec")
		createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

		claims := newUnrecordedAgentToken(t, srv, agentID, project.ID)
		req := buildAgentRefreshRequest(agentID, claims, "", false)
		rec := httptest.NewRecorder()
		srv.handleAgentTokenRefresh(rec, req, agentID)

		require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		assert.Equal(t, agentTokenRefusedBody, rec.Body.String())
		recs := logs.records(agentTokenRefusedLogMessage)
		require.Len(t, recs, 1)
		assert.Equal(t, agentTokenCauseRefreshUnrecorded, recs[0].attrs["cause"])
	})

	t.Run("empty jti rejected 401 with refresh_missing_jti cause", func(t *testing.T) {
		srv, s, _, project := setupCredentialTestServer(t)
		logs := &captureHandler{}
		srv.authConfig.Logger = slog.New(logs)

		agentID := tid("agent-refresh-empty-jti")
		createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

		now := time.Now().UTC()
		claims := &AgentTokenClaims{
			Claims: jwt.Claims{
				Issuer:    AgentTokenIssuer,
				Subject:   agentID,
				Audience:  jwt.Audience{AgentTokenAudience},
				IssuedAt:  jwt.NewNumericDate(now),
				NotBefore: jwt.NewNumericDate(now),
				Expiry:    jwt.NewNumericDate(now.Add(time.Hour)),
				ID:        "",
			},
			ProjectID:   project.ID,
			Scopes:      ScopesForRole(AgentRoleFull),
			ScopeSchema: CurrentAgentScopeSchema,
		}
		req := buildAgentRefreshRequest(agentID, claims, "", false)
		rec := httptest.NewRecorder()
		srv.handleAgentTokenRefresh(rec, req, agentID)

		require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		assert.Equal(t, agentTokenRefusedBody, rec.Body.String())
		recs := logs.records(agentTokenRefusedLogMessage)
		require.Len(t, recs, 1)
		assert.Equal(t, agentTokenCauseRefreshMissingJTI, recs[0].attrs["cause"])
	})

	t.Run("ErrNotFound on re-check with credential marker present returns 401 not 503", func(t *testing.T) {
		srv, s, _, project, credFault := setupCredentialTestServerWithCredFault(t, store.ErrNotFound)
		logs := &captureHandler{}
		srv.authConfig.Logger = slog.New(logs)

		agentID := tid("agent-refresh-notfound-recheck")
		createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

		token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
		require.NoError(t, err)
		claims, err := srv.agentTokenService.ValidateAgentToken(token)
		require.NoError(t, err)
		cred, err := s.GetAgentCredentialByJTIHash(context.Background(), hashJTI(claims.ID))
		require.NoError(t, err)

		credFault.Arm()

		req := buildAgentRefreshRequest(agentID, claims, cred.ID, false)
		rec := httptest.NewRecorder()
		srv.handleAgentTokenRefresh(rec, req, agentID)

		require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		assert.Equal(t, agentTokenRefusedBody, rec.Body.String())
		recs := logs.records(agentTokenRefusedLogMessage)
		require.Len(t, recs, 1)
		assert.Equal(t, agentTokenCauseRefreshUnrecorded, recs[0].attrs["cause"])
	})
}

func TestConduitRecheckRelayAdmission_AgentCredentialEnforcement(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)
	ctx := context.Background()

	agentID := tid("agent-conduit-recheck")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

	recordedTok, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	recordedClaims, err := srv.agentTokenService.ValidateAgentToken(recordedTok)
	require.NoError(t, err)

	baseClaims := conduitAdmissionClaims{
		Kind:      registry.PrincipalAgent,
		ID:        agentID,
		ProjectID: project.ID,
	}

	t.Run("empty CredentialID denied", func(t *testing.T) {
		c := baseClaims
		c.CredentialID = ""
		v := srv.recheckRelayAdmission(ctx, c)
		assert.Equal(t, conduitAuthzDenied, v.verdict)
		assert.Equal(t, uint32(conduit.CloseUnauthenticated), v.closeCode)
	})

	t.Run("unrecorded CredentialID denied", func(t *testing.T) {
		c := baseClaims
		c.CredentialID = "unrecorded-jti-value"
		v := srv.recheckRelayAdmission(ctx, c)
		assert.Equal(t, conduitAuthzDenied, v.verdict)
		assert.Equal(t, uint32(conduit.CloseUnauthenticated), v.closeCode)
	})

	t.Run("recorded active CredentialID allowed", func(t *testing.T) {
		c := baseClaims
		c.CredentialID = recordedClaims.ID
		v := srv.recheckRelayAdmission(ctx, c)
		assert.Equal(t, conduitAuthzAllowed, v.verdict)
	})

	t.Run("store error returns unavailable", func(t *testing.T) {
		orig := srv.authConfig.CredentialStore
		t.Cleanup(func() { srv.authConfig.CredentialStore = orig })
		srv.authConfig.CredentialStore = &fixedErrAgentCredentialStore{err: errors.New("db unavailable")}

		c := baseClaims
		c.CredentialID = recordedClaims.ID
		v := srv.recheckRelayAdmission(ctx, c)
		assert.Equal(t, conduitAuthzUnavailable, v.verdict)
	})

	t.Run("nil CredentialStore returns unavailable", func(t *testing.T) {
		orig := srv.authConfig.CredentialStore
		t.Cleanup(func() { srv.authConfig.CredentialStore = orig })
		srv.authConfig.CredentialStore = nil

		c := baseClaims
		c.CredentialID = recordedClaims.ID
		v := srv.recheckRelayAdmission(ctx, c)
		assert.Equal(t, conduitAuthzUnavailable, v.verdict)
	})
}

func TestConduitAgentRefreshCheck_RejectsUnrecordedAndEmptyJTI(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)
	ctx := context.Background()

	agentID := tid("agent-conduit-inband")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

	p := relay.Principal{
		Kind:      registry.PrincipalAgent,
		ID:        agentID,
		ProjectID: project.ID,
	}
	sess := relay.RefreshSession{SessionID: "sess-1"}

	t.Run("unrecorded token refused", func(t *testing.T) {
		unrecordedTok, _, err := srv.agentTokenService.SignAgentToken(AgentTokenGrant{
			AgentID:   agentID,
			ProjectID: project.ID,
			Scopes:    ScopesForRole(AgentRoleFull),
		}, "")
		require.NoError(t, err)

		outcome, exp := srv.conduitAgentRefreshCheck(ctx, p, sess, &conduitv1.AuthRefresh{
			Credential: []byte(unrecordedTok),
		})
		assert.Equal(t, conduitRefreshRefused, outcome)
		assert.True(t, exp.IsZero())
	})

	t.Run("empty jti token refused", func(t *testing.T) {
		emptyJTITok := signAgentTokenWithoutJTI(t, srv.agentTokenService, agentID, project.ID)
		outcome, exp := srv.conduitAgentRefreshCheck(ctx, p, sess, &conduitv1.AuthRefresh{
			Credential: []byte(emptyJTITok),
		})
		assert.Equal(t, conduitRefreshRefused, outcome)
		assert.True(t, exp.IsZero())
	})
}
