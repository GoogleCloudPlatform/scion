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
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Broker on-behalf-of: compatibility predicate and middleware wiring
// (ptone/scion#2123; ruling-identity-fail-closed.md, 08:04Z)
// =============================================================================

// capturingAuditEmitter records every DecisionAuditRecord it receives, for
// tests that need to inspect audit content rather than merely count calls.
type capturingAuditEmitter struct {
	mu      sync.Mutex
	records []*store.DecisionAuditRecord
}

func (e *capturingAuditEmitter) EmitDecisionAudit(_ context.Context, record *store.DecisionAuditRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.records = append(e.records, record)
}

func (e *capturingAuditEmitter) last() *store.DecisionAuditRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.records) == 0 {
		return nil
	}
	return e.records[len(e.records)-1]
}

// oboMiddlewareVariant names the two broker-auth middleware constructors
// under test, so every OBO wiring assertion runs against both: both must
// install an identical broker CredentialContext for the identical request,
// through the one shared applyOnBehalfOf helper.
func oboMiddlewareVariants(auditLogger AuditLogger) []struct {
	name string
	wrap func(svc *BrokerAuthService, next http.Handler) http.Handler
} {
	return []struct {
		name string
		wrap func(svc *BrokerAuthService, next http.Handler) http.Handler
	}{
		{"BrokerAuthMiddleware", func(svc *BrokerAuthService, next http.Handler) http.Handler {
			return BrokerAuthMiddleware(svc)(next)
		}},
		{"AuditableBrokerAuthMiddleware", func(svc *BrokerAuthService, next http.Handler) http.Handler {
			return AuditableBrokerAuthMiddleware(svc, auditLogger)(next)
		}},
	}
}

// TestBrokerOnBehalfOf_BothMiddlewareVariantsGrantEffectiveUserAccess drives
// both middleware variants with a valid HMAC signature and a resolving
// X-Scion-On-Behalf-Of header, through DecideFromContext and through
// AuthorizeReadBatch (the list-filtering path). Both must proceed as the
// effective on-behalf-of user, carrying the broker credential — the pair the
// old option-A implementation denied under rule B until the compatibility
// predicate's broker exception (08:04Z) admitted it.
func TestBrokerOnBehalfOf_BothMiddlewareVariantsGrantEffectiveUserAccess(t *testing.T) {
	for _, variant := range oboMiddlewareVariants(nil) {
		t.Run(variant.name, func(t *testing.T) {
			srv, s := testServer(t)
			projectID := tid("obo-grant-project")
			ownerID := tid("obo-grant-owner")
			rs4Project(t, s, projectID, ownerID)

			brokerID, signReq := setupSignedBrokerRequest(t, srv.brokerAuthService, s)

			var decision Decision
			var readBatch []bool
			var readErr error
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := r.Context()
				decision = srv.authzService.DecideFromContext(ctx, Resource{
					Type: "project", ID: projectID, OwnerID: ownerID,
				}, ActionRead)

				identity := GetIdentityFromContext(ctx)
				readBatch, readErr = srv.authzService.AuthorizeReadBatch(ctx, identity, []Resource{
					{Type: "project", ID: projectID, OwnerID: ownerID},
				})
				w.WriteHeader(http.StatusOK)
			})

			wrapped := variant.wrap(srv.brokerAuthService, handler)
			req := signReq(http.MethodGet, "/api/v1/projects/"+projectID, map[string]string{
				HeaderOnBehalfOf: "user:" + ownerID + "@test.com",
			})
			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, req)

			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.True(t, decision.Allowed, "the effective user (project owner) must be allowed through DecideFromContext")
			assert.Equal(t, PrincipalKindUser, decision.PrincipalKind)
			assert.Equal(t, string(CredentialKindBroker), decision.CredentialKind, "the broker credential must remain effective")
			assert.Equal(t, brokerID, decision.CredentialID)

			require.NoError(t, readErr)
			require.Len(t, readBatch, 1)
			assert.True(t, readBatch[0], "AuthorizeReadBatch (list filtering) must also admit the effective user")
		})
	}
}

// TestBrokerOnBehalfOf_BrokerWithoutOBOStaysBrokerBroker: a broker request
// with no X-Scion-On-Behalf-Of header installs the broker itself as the
// request identity, with no OBO marker, and Decide denies it exactly as it
// denies any broker principal today (the unsupported-principal-kind switch),
// under BOTH middleware variants.
func TestBrokerOnBehalfOf_BrokerWithoutOBOStaysBrokerBroker(t *testing.T) {
	for _, variant := range oboMiddlewareVariants(nil) {
		t.Run(variant.name, func(t *testing.T) {
			srv, s := testServer(t)
			_, signReq := setupSignedBrokerRequest(t, srv.brokerAuthService, s)

			var decision Decision
			var gotOBO bool
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := r.Context()
				_, gotOBO = BrokerOnBehalfOfFromContext(ctx)
				decision = srv.authzService.DecideFromContext(ctx, Resource{Type: "agent", ID: tid("obo-no-header-target")}, ActionRead)
				w.WriteHeader(http.StatusOK)
			})

			wrapped := variant.wrap(srv.brokerAuthService, handler)
			req := signReq(http.MethodGet, "/api/v1/test", nil) // no OBO header
			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, req)

			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.False(t, gotOBO, "no OBO marker without the header")
			assert.False(t, decision.Allowed)
			assert.Equal(t, PrincipalKindBroker, decision.PrincipalKind)
			assert.Equal(t, "broker identities are not supported by authorization", decision.Reason)
		})
	}
}

// TestBrokerOnBehalfOf_PlainUserWithBrokerCredentialNoMarkerDenies covers the
// ruling's "a plain user + broker credential without the OBO marker denies"
// case directly at the Decide level: a context carrying an ordinary user
// identity and a broker CredentialContext, but with no broker identity and no
// BrokerOnBehalfOf marker at all (as if a caller fabricated the credential
// without ever going through broker auth middleware), must deny. This is also
// exactly what the existing RS4 T-C8 fabricated-broker-credential test proves
// through the HTTP token-management routes; this pins the same rule directly
// against Decide.
func TestBrokerOnBehalfOf_PlainUserWithBrokerCredentialNoMarkerDenies(t *testing.T) {
	user := NewAuthenticatedUser(tid("obo-no-marker-user"), "u@example.com", "U", "member", "cli")
	ctx := contextWithIdentity(context.Background(), user)
	ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindBroker, ID: tid("obo-no-marker-broker"), Type: "broker"})

	authz := &AuthzService{}
	decision := authz.Decide(ctx, AuthzRequestFromContext(ctx, Resource{Type: "agent", ID: tid("obo-no-marker-target")}, ActionRead))

	assert.False(t, decision.Allowed)
	assert.Equal(t, "credential kind does not match identity", decision.Reason)
}

// TestBrokerOnBehalfOf_MarkerMismatchDenies covers "a marker with a broker-ID
// or type mismatch denies": a context that does carry a genuine broker
// identity and a genuine BrokerOnBehalfOf marker, but where the supplied
// Credential.ID names a different broker than the one the marker and ctx
// broker identity agree on, must still deny — the predicate must actually
// bind Credential.ID, not merely check that a marker exists somewhere.
func TestBrokerOnBehalfOf_MarkerMismatchDenies(t *testing.T) {
	realBroker := NewBrokerIdentity(tid("obo-mismatch-real-broker"))
	otherBroker := tid("obo-mismatch-other-broker")
	user := NewAuthenticatedUser(tid("obo-mismatch-user"), "u@example.com", "U", "member", "cli")

	cases := []struct {
		name       string
		ctxBuilder func() context.Context
	}{
		{
			name: "Credential.ID names a different broker than the marker/ctx broker identity",
			ctxBuilder: func() context.Context {
				ctx := contextWithIdentity(context.Background(), user)
				ctx = contextWithBrokerIdentity(ctx, realBroker)
				ctx = contextWithBrokerOnBehalfOf(ctx, BrokerOnBehalfOf{Broker: realBroker, BrokerID: realBroker.ID()})
				return contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindBroker, ID: otherBroker, Type: "broker"})
			},
		},
		{
			name: "marker BrokerID disagrees with the ctx broker identity's own ID",
			ctxBuilder: func() context.Context {
				ctx := contextWithIdentity(context.Background(), user)
				ctx = contextWithBrokerIdentity(ctx, realBroker)
				ctx = contextWithBrokerOnBehalfOf(ctx, BrokerOnBehalfOf{Broker: realBroker, BrokerID: otherBroker})
				return contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindBroker, ID: realBroker.ID(), Type: "broker"})
			},
		},
		{
			name: "no ctx broker identity at all, only the marker and credential",
			ctxBuilder: func() context.Context {
				ctx := contextWithIdentity(context.Background(), user)
				ctx = contextWithBrokerOnBehalfOf(ctx, BrokerOnBehalfOf{Broker: realBroker, BrokerID: realBroker.ID()})
				return contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindBroker, ID: realBroker.ID(), Type: "broker"})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := tc.ctxBuilder()
			authz := &AuthzService{}
			decision := authz.Decide(ctx, AuthzRequestFromContext(ctx, Resource{Type: "agent", ID: tid("obo-mismatch-target")}, ActionRead))
			assert.False(t, decision.Allowed)
			assert.Equal(t, "credential kind does not match identity", decision.Reason)
		})
	}
}

// TestBrokerOnBehalfOf_InvalidHMACNeverInstallsAnything: a request with a
// broken HMAC signature (but a well-formed OBO header) is rejected before
// applyOnBehalfOf ever runs, under both middleware variants. The handler is
// never invoked, so neither the identity, the marker, nor the credential is
// ever installed for a downstream caller to observe.
func TestBrokerOnBehalfOf_InvalidHMACNeverInstallsAnything(t *testing.T) {
	for _, variant := range oboMiddlewareVariants(nil) {
		t.Run(variant.name, func(t *testing.T) {
			srv, s := testServer(t)
			_, signReq := setupSignedBrokerRequest(t, srv.brokerAuthService, s)

			called := false
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			})

			wrapped := variant.wrap(srv.brokerAuthService, handler)
			req := signReq(http.MethodPost, "/api/v1/test", map[string]string{
				HeaderOnBehalfOf: "user:someone@example.com",
			})
			// Tamper with the signature after signing so HMAC verification fails.
			req.Header.Set(HeaderSignature, "0000000000000000000000000000000000000000000000000000000000000000")

			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, req)

			assert.Equal(t, http.StatusUnauthorized, w.Code)
			assert.False(t, called, "the next handler must never run on an invalid HMAC, so nothing downstream can observe an installed identity/marker/credential")
		})
	}
}

// TestBrokerOnBehalfOf_TokenIssuanceAndAdminRoutesStayDenied: under a real
// OBO context (valid HMAC, resolving header), the session-only gates that
// guard token issuance and user-mutation admin routes still deny — they
// read the raw ctx CredentialContext.Kind directly (never through Decide's
// compatibility predicate), so the broker credential remaining "effective"
// for Decide's own evaluation has no bearing on routes that require a full
// interactive session.
func TestBrokerOnBehalfOf_TokenIssuanceAndAdminRoutesStayDenied(t *testing.T) {
	for _, variant := range oboMiddlewareVariants(nil) {
		t.Run(variant.name, func(t *testing.T) {
			srv, s := testServer(t)
			ownerID := tid("obo-admin-deny-user")
			require.NoError(t, s.CreateUser(context.Background(), &store.User{
				ID: ownerID, Email: ownerID + "@test.com", DisplayName: "Owner", Role: "member", Status: "active",
			}))
			_, signReq := setupSignedBrokerRequest(t, srv.brokerAuthService, s)

			var tokenErr, adminErr error
			var adminOK bool
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := r.Context()
				tokenErr = requireSessionCredential(ctx)
				rec := httptest.NewRecorder()
				_, adminOK = srv.requireSessionCredential(rec, ctx)
				if !adminOK {
					adminErr = fmt.Errorf("denied: %s", rec.Body.String())
				}
				w.WriteHeader(http.StatusOK)
			})

			wrapped := variant.wrap(srv.brokerAuthService, handler)
			req := signReq(http.MethodPost, "/api/v1/auth/tokens", map[string]string{
				HeaderOnBehalfOf: "user:" + ownerID + "@test.com",
			})
			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, req)

			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Error(t, tokenErr, "token issuance must stay denied under a broker-OBO credential")
			assert.False(t, adminOK, "user-mutation admin routes must stay denied under a broker-OBO credential")
			assert.Error(t, adminErr)
		})
	}
}

// TestBrokerOnBehalfOf_AuditRecordsEffectiveUserAndBrokerCredential: the
// decision audit record for an allowed broker-OBO request names the
// effective user as the principal and the broker as the credential/actor —
// under both middleware variants, closing the gap where the audited
// middleware previously recorded an empty credential.
func TestBrokerOnBehalfOf_AuditRecordsEffectiveUserAndBrokerCredential(t *testing.T) {
	for _, variant := range oboMiddlewareVariants(nil) {
		t.Run(variant.name, func(t *testing.T) {
			srv, s := testServer(t)
			projectID := tid("obo-audit-project")
			ownerID := tid("obo-audit-owner")
			rs4Project(t, s, projectID, ownerID)

			emitter := &capturingAuditEmitter{}
			srv.authzService.SetDecisionAuditEmitter(emitter)

			brokerID, signReq := setupSignedBrokerRequest(t, srv.brokerAuthService, s)

			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := r.Context()
				_ = srv.authzService.DecideFromContext(ctx, Resource{Type: "project", ID: projectID, OwnerID: ownerID}, ActionRead)
				w.WriteHeader(http.StatusOK)
			})

			wrapped := variant.wrap(srv.brokerAuthService, handler)
			req := signReq(http.MethodGet, "/api/v1/projects/"+projectID, map[string]string{
				HeaderOnBehalfOf: "user:" + ownerID + "@test.com",
			})
			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())

			record := emitter.last()
			require.NotNil(t, record, "a decision audit record must be emitted")
			assert.Equal(t, ownerID, record.PrincipalID, "audit must name the effective user, not the broker")
			assert.Equal(t, string(PrincipalKindUser), record.PrincipalKind)
			assert.Equal(t, brokerID, record.CredentialID, "audit must record the broker as the credential/actor")
			assert.Equal(t, string(CredentialKindBroker), record.CredentialType)
		})
	}
}
