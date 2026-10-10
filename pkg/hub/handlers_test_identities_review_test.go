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
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- GSA resolver existing-binding branch (F3) ------------------------------

// An existing external binding to a test-fixture row is refused even when
// the presented email is outside the reserved domain, so only the
// bound-user check can refuse it.
func TestTestIdentity_GSAResolverExistingBindingRefused(t *testing.T) {
	ctx := context.Background()
	users := newFakeUserStore()
	exp := time.Now().Add(time.Hour)
	fixture := &store.User{ID: generateID(), Email: "bound@" + store.TestFixtureEmailDomain, Role: "member", Status: "active",
		Kind: store.UserKindTestFixture, ExpiresAt: &exp}
	require.NoError(t, users.CreateUser(ctx, fixture))
	ext := newMemExtIDStore()
	require.NoError(t, ext.CreateExternalIdentity(ctx, &store.ExternalIdentityBinding{
		ID: generateID(), Provider: "google", Issuer: canonicalizeGoogleIssuer(googleIssuerHTTPS), Subject: "999",
		UserID: fixture.ID, Email: "sa@example.iam.gserviceaccount.com", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}))
	resolver := NewGoogleIdentityResolver(users, ext, alwaysAuthorized, nil, nil)
	user, err := resolver.Resolve(ctx, &ValidatedGoogleIdentity{
		Subject: "999", Email: "sa@example.iam.gserviceaccount.com", EmailVerified: true, Issuer: googleIssuerHTTPS,
		IsServiceAccount: true, UpstreamExpiry: time.Now().Add(time.Hour),
	}, ResolvePolicy{PreAuthorized: true})
	assert.True(t, errors.Is(err, ErrAccessDenied), "got %v", err)
	assert.Nil(t, user)
}

// --- Admin-visible indicator (F4) -------------------------------------------

func TestTestIdentity_HealthSummaryIndicator(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		srv, _ := newTestIdentityServer(t, enabled)
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp HealthSummaryResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, enabled, resp.Hub.TestIdentitiesEnabled)
		assert.Contains(t, rec.Body.String(), `"test_identities_enabled"`)
	}
}

// --- List bounds (F5) -------------------------------------------------------

func TestTestIdentity_ListLiveByDefaultAndBounded(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	issuerID, issuerTok := tiIssuer(t, srv, s, "ti-lb-issuer")
	for i := 0; i < 3; i++ {
		tiStoreFixture(t, s, issuerID, time.Now().Add(time.Hour))
	}
	tiStoreFixture(t, s, issuerID, time.Now().Add(-time.Minute))

	get := func(query string) ListTestIdentitiesResponse {
		rec := doRequestWithToken(t, srv, issuerTok, http.MethodGet, "/api/v1/test-identities"+query, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp ListTestIdentitiesResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp
	}
	def := get("")
	assert.Len(t, def.Items, 3, "expired identities are left out by default")
	for _, it := range def.Items {
		assert.True(t, it.Live)
	}
	assert.False(t, def.Truncated)
	assert.Len(t, get("?includeExpired=true").Items, 4)
	capped := get("?limit=2")
	assert.Len(t, capped.Items, 2)
	assert.True(t, capped.Truncated)
	for _, q := range []string{"?limit=0", "?limit=501", "?limit=x", "?includeExpired=yes"} {
		rec := doRequestWithToken(t, srv, issuerTok, http.MethodGet, "/api/v1/test-identities"+q, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, q)
	}
}

// --- Transient store conflicts (F7) -----------------------------------------

func TestTestIdentity_TransientConflictIs429(t *testing.T) {
	for _, msg := range []string{
		"database is locked", "sqlite3: SQLITE_BUSY", "ERROR: could not serialize access due to concurrent update (SQLSTATE 40001)",
		"ERROR: deadlock detected (SQLSTATE 40P01)",
	} {
		assert.True(t, isTransientIssuanceConflict(errors.New("create test identity: "+msg)), msg)
	}
	assert.False(t, isTransientIssuanceConflict(errors.New("constraint failed")))
	assert.False(t, isTransientIssuanceConflict(nil))

	srv, s := newTestIdentityServer(t, true)
	_, issuerTok := tiIssuer(t, srv, s, "ti-busy-issuer")
	srv.testIdentities.hooks.writeAudit = func(context.Context, store.Store, *store.MutationAuditRecord) error {
		return errors.New("database is locked")
	}
	rec := tiPost(t, srv, issuerTok, "/api/v1/test-identities", nil)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	assert.Contains(t, rec.Body.String(), testIdentityReasonBusy)
	list, err := s.ListTestFixtureUsers(context.Background(), "", time.Time{}, 0)
	require.NoError(t, err)
	assert.Empty(t, list)
}
