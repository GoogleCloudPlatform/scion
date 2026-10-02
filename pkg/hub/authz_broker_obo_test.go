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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Broker on-behalf-of: compatibility predicate and middleware wiring
// (ptone/scion#2123)
// =============================================================================

// capturingAuditEmitter is defined in decisionaudit_test_helpers_test.go,
// which carries no no_sqlite build constraint.

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
// effective on-behalf-of user, carrying the broker credential.
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
// request identity, with no OBO marker, and Decide denies it through the
// unsupported-principal-kind switch, under BOTH middleware variants. It also
// installs the broker
// CredentialContext, identically under both variants: the broker-only branch
// of applyOnBehalfOf must not skip the credential just because there is no
// on-behalf-of user to substitute.
func TestBrokerOnBehalfOf_BrokerWithoutOBOStaysBrokerBroker(t *testing.T) {
	for _, variant := range oboMiddlewareVariants(nil) {
		t.Run(variant.name, func(t *testing.T) {
			srv, s := testServer(t)
			brokerID, signReq := setupSignedBrokerRequest(t, srv.brokerAuthService, s)

			var decision Decision
			var gotOBO bool
			var gotCredential CredentialContext
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := r.Context()
				_, gotOBO = BrokerOnBehalfOfFromContext(ctx)
				gotCredential = GetCredentialContextFromContext(ctx)
				decision = srv.authzService.DecideFromContext(ctx, Resource{Type: "agent", ID: tid("obo-no-header-target")}, ActionRead)
				w.WriteHeader(http.StatusOK)
			})

			wrapped := variant.wrap(srv.brokerAuthService, handler)
			req := signReq(http.MethodGet, "/api/v1/test", nil) // no OBO header
			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, req)

			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.False(t, gotOBO, "no OBO marker without the header")
			assert.Equal(t, CredentialKindBroker, gotCredential.Kind, "the broker-only branch must still install the broker credential")
			assert.Equal(t, brokerID, gotCredential.ID)
			assert.False(t, decision.Allowed)
			assert.Equal(t, PrincipalKindBroker, decision.PrincipalKind)
			assert.Equal(t, "broker identities are not supported by authorization", decision.Reason)
		})
	}
}

// TestBrokerOnBehalfOf_PlainUserWithBrokerCredentialNoMarkerDenies covers a
// plain user plus a broker credential with no OBO marker directly at the
// Decide level: a context carrying an ordinary user
// identity and a broker CredentialContext, but with no broker identity and no
// BrokerOnBehalfOf marker at all (as if a caller supplied the credential
// without ever going through broker auth middleware), must deny. This is also
// exactly what the existing RS4 fabricated-broker-credential test proves
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

// fakeNonBrokerTypeIdentity implements BrokerIdentity but reports a Type()
// other than "broker", so a test can install it as the ctx broker identity
// and assert that brokerOnBehalfOfAuthorizes still checks Type() rather than
// trusting that GetBrokerIdentityFromContext only ever holds a genuine
// broker.
type fakeNonBrokerTypeIdentity struct {
	id string
}

func (f *fakeNonBrokerTypeIdentity) ID() string       { return f.id }
func (f *fakeNonBrokerTypeIdentity) Type() string     { return "agent" }
func (f *fakeNonBrokerTypeIdentity) BrokerID() string { return f.id }

// TestBrokerOnBehalfOf_BrokerIdentityWithNoMarkerDenies covers the case where
// ctx carries a genuine broker identity, but no BrokerOnBehalfOf marker at
// all: a broker identity in ctx alone must not qualify the on-behalf-of
// exception.
func TestBrokerOnBehalfOf_BrokerIdentityWithNoMarkerDenies(t *testing.T) {
	broker := NewBrokerIdentity(tid("obo-no-marker-only-broker"))
	user := NewAuthenticatedUser(tid("obo-no-marker-only-user"), "u@example.com", "U", "member", "cli")

	ctx := contextWithIdentity(context.Background(), user)
	ctx = contextWithBrokerIdentity(ctx, broker)
	ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindBroker, ID: broker.ID(), Type: "broker"})

	authz := &AuthzService{}
	decision := authz.Decide(ctx, AuthzRequestFromContext(ctx, Resource{Type: "agent", ID: tid("obo-no-marker-only-target")}, ActionRead))

	assert.False(t, decision.Allowed)
	assert.Equal(t, "credential kind does not match identity", decision.Reason)
}

// TestBrokerOnBehalfOf_NonBrokerTypeIdentityDenies covers "the ctx broker
// identity's Type() must equal the expected broker credential type": a ctx
// identity that satisfies BrokerIdentity structurally but reports a
// different Type(), paired with an otherwise-valid marker and matching IDs,
// must still deny.
func TestBrokerOnBehalfOf_NonBrokerTypeIdentityDenies(t *testing.T) {
	fakeBroker := &fakeNonBrokerTypeIdentity{id: tid("obo-wrong-type-broker")}
	user := NewAuthenticatedUser(tid("obo-wrong-type-user"), "u@example.com", "U", "member", "cli")

	ctx := contextWithIdentity(context.Background(), user)
	ctx = contextWithBrokerIdentity(ctx, fakeBroker)
	ctx = contextWithBrokerOnBehalfOf(ctx, BrokerOnBehalfOf{Broker: fakeBroker, BrokerID: fakeBroker.ID()})
	ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindBroker, ID: fakeBroker.ID(), Type: "broker"})

	authz := &AuthzService{}
	decision := authz.Decide(ctx, AuthzRequestFromContext(ctx, Resource{Type: "agent", ID: tid("obo-wrong-type-target")}, ActionRead))

	assert.False(t, decision.Allowed)
	assert.Equal(t, "credential kind does not match identity", decision.Reason)
}

// TestBrokerOnBehalfOf_SuppliedTypeMismatchDenies covers the supplied.Type
// half of the broker on-behalf-of check directly: ctx carries a genuine
// broker identity and a genuine, fully-matching BrokerOnBehalfOf marker, but
// the supplied CredentialContext names a Type other than "broker" (as if a
// caller built the credential by hand rather than reading it from ctx). The
// predicate must check supplied.Type, not merely IDs.
func TestBrokerOnBehalfOf_SuppliedTypeMismatchDenies(t *testing.T) {
	broker := NewBrokerIdentity(tid("obo-supplied-type-broker"))
	user := NewAuthenticatedUser(tid("obo-supplied-type-user"), "u@example.com", "U", "member", "cli")

	ctx := contextWithIdentity(context.Background(), user)
	ctx = contextWithBrokerIdentity(ctx, broker)
	ctx = contextWithBrokerOnBehalfOf(ctx, BrokerOnBehalfOf{Broker: broker, BrokerID: broker.ID()})

	request := AuthzRequestFromContext(ctx, Resource{Type: "agent", ID: tid("obo-supplied-type-target")}, ActionRead)
	request.Credential = CredentialContext{Kind: CredentialKindBroker, ID: broker.ID(), Type: "user"}

	authz := &AuthzService{}
	decision := authz.Decide(ctx, request)

	assert.False(t, decision.Allowed)
	assert.Equal(t, "credential kind does not match identity", decision.Reason)
}

// TestBrokerOnBehalfOf_DifferentPrincipalDenies covers binding the effective
// user: ctx carries a genuine broker identity, a genuine BrokerOnBehalfOf
// marker naming it, and a matching broker Credential.ID — all exactly as a
// real on-behalf-of substitution for one user would set them — but the
// AuthzRequest's Principal names a DIFFERENT local user than the ctx
// effective identity (GetIdentityFromContext). The on-behalf-of exception
// must name exactly the principal being evaluated, not merely require that
// SOME user was substituted somewhere in ctx.
func TestBrokerOnBehalfOf_DifferentPrincipalDenies(t *testing.T) {
	broker := NewBrokerIdentity(tid("obo-diffprincipal-broker"))
	effectiveUser := NewAuthenticatedUser(tid("obo-diffprincipal-effective"), "e@example.com", "E", "member", "cli")
	otherUser := NewAuthenticatedUser(tid("obo-diffprincipal-other"), "o@example.com", "O", "member", "cli")

	ctx := contextWithIdentity(context.Background(), effectiveUser)
	ctx = contextWithBrokerIdentity(ctx, broker)
	ctx = contextWithBrokerOnBehalfOf(ctx, BrokerOnBehalfOf{Broker: broker, BrokerID: broker.ID()})
	ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindBroker, ID: broker.ID(), Type: "broker"})

	authz := &AuthzService{}
	request := AuthzRequest{
		Principal:  PrincipalContext{Identity: otherUser},
		Credential: GetCredentialContextFromContext(ctx),
		Resource:   Resource{Type: "agent", ID: tid("obo-diffprincipal-target")},
		Action:     ActionRead,
	}
	decision := authz.Decide(ctx, request)

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
// effective user as the principal and the broker as the credential/actor,
// identically under both middleware variants.
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

// TestBrokerOnBehalfOf_BareHeaderNeverSetsMarker: an X-Scion-On-Behalf-Of
// header on a request with none of the broker HMAC headers is not a
// broker-authenticated request at all — both middleware variants skip broker
// auth entirely and call the next handler unchanged, so neither the broker
// identity, the OBO marker, nor a broker credential is ever installed. A bare
// header can never substitute for the middleware resolving it.
func TestBrokerOnBehalfOf_BareHeaderNeverSetsMarker(t *testing.T) {
	for _, variant := range oboMiddlewareVariants(nil) {
		t.Run(variant.name, func(t *testing.T) {
			srv, _ := testServer(t)

			var gotOBO, gotBroker bool
			var gotCredential CredentialContext
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := r.Context()
				_, gotOBO = BrokerOnBehalfOfFromContext(ctx)
				gotBroker = GetBrokerIdentityFromContext(ctx) != nil
				gotCredential = GetCredentialContextFromContext(ctx)
				w.WriteHeader(http.StatusOK)
			})

			wrapped := variant.wrap(srv.brokerAuthService, handler)
			req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
			req.Header.Set(HeaderOnBehalfOf, "user:someone@example.com") // no HeaderBrokerID, no HMAC
			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, req)

			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.False(t, gotOBO, "a bare header must never set the OBO marker")
			assert.False(t, gotBroker, "a bare header must never install a broker identity")
			assert.Equal(t, CredentialKind(""), gotCredential.Kind, "a bare header must never install a broker credential")
		})
	}
}

// TestBrokerOnBehalfOf_AuditableUnknownUserDenies covers the audited
// middleware's "valid HMAC + OBO, unknown user" case: production wires
// AuditableBrokerAuthMiddleware whenever an audit logger is configured. An
// unresolvable on-behalf-of principal must return 403 without ever calling
// the next handler, identically to the non-audited variant.
func TestBrokerOnBehalfOf_AuditableUnknownUserDenies(t *testing.T) {
	srv, s := testServer(t)
	_, signReq := setupSignedBrokerRequest(t, srv.brokerAuthService, s)

	called := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	wrapped := AuditableBrokerAuthMiddleware(srv.brokerAuthService, nil)(handler)
	req := signReq(http.MethodPost, "/api/v1/test", map[string]string{
		HeaderOnBehalfOf: "user:unknown@example.com",
	})
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.False(t, called, "the handler must not be called for an unresolvable on-behalf-of principal")
}

// TestSuppliedCredentialCompatible_NonUserPrincipalNeverAdmitsInteractiveNarrowing
// pins the principal-kind half of the UAT/broker exception. Production never
// pairs a non-user PrincipalKind with a CredentialKindInteractive derivation
// (Dev derives CredentialKindDev, Agent derives CredentialKindAgentJWT, and
// so on), but the predicate takes principal and derived credential as
// independent arguments, so this pins the principal-kind guard directly
// rather than relying on that pairing never occurring.
func TestSuppliedCredentialCompatible_NonUserPrincipalNeverAdmitsInteractiveNarrowing(t *testing.T) {
	broker := NewBrokerIdentity(tid("obo-nonuser-broker"))
	user := NewAuthenticatedUser(tid("obo-nonuser-user"), "u@example.com", "U", "member", "cli")

	validOBOCtx := func() context.Context {
		ctx := contextWithIdentity(context.Background(), user)
		ctx = contextWithBrokerIdentity(ctx, broker)
		return contextWithBrokerOnBehalfOf(ctx, BrokerOnBehalfOf{Broker: broker, BrokerID: broker.ID()})
	}

	principals := []struct {
		name string
		kind PrincipalKind
	}{
		{"dev", PrincipalKindDev},
		{"agent", PrincipalKindAgent},
		{"federated_user", PrincipalKindFederatedUser},
		{"unknown (empty)", PrincipalKind("")},
	}

	for _, p := range principals {
		t.Run(p.name, func(t *testing.T) {
			principal := PrincipalContext{Kind: p.kind, ID: user.ID()}
			derived := CredentialContext{Kind: CredentialKindInteractive}

			assert.False(t, suppliedCredentialCompatible(context.Background(), principal, derived, CredentialContext{Kind: CredentialKindUAT}),
				"the narrowing overlay to UAT must require PrincipalKindUser")

			ctx := validOBOCtx()
			assert.False(t, suppliedCredentialCompatible(ctx, principal, derived, CredentialContext{Kind: CredentialKindBroker, ID: broker.ID(), Type: "broker"}),
				"the broker on-behalf-of exception must require PrincipalKindUser even with a fully valid OBO context")
		})
	}

	// The broker on-behalf-of exception also requires the derived credential
	// kind to be CredentialKindInteractive, independent of the PrincipalKind
	// check above: a user principal whose own derived credential is
	// CredentialKindUAT (a real *ScopedUserIdentity) must not admit a
	// supplied broker credential through this predicate, even with a fully
	// valid OBO context for that same user, because that would replace the
	// UAT's scope/project caveats with the broker credential for the rest of
	// Decide.
	t.Run("user principal with a UAT derivation", func(t *testing.T) {
		principal := PrincipalContext{Kind: PrincipalKindUser, ID: user.ID()}
		derived := CredentialContext{Kind: CredentialKindUAT}

		ctx := validOBOCtx()
		assert.False(t, suppliedCredentialCompatible(ctx, principal, derived, CredentialContext{Kind: CredentialKindBroker, ID: broker.ID(), Type: "broker"}),
			"the broker on-behalf-of exception must require an interactive derivation, not a UAT one, even with a fully valid OBO context")
	})
}
