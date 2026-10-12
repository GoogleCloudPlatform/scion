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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/credentialmeta"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Workload-bound issuer delegation (ptone/scion#4240, phase X2).
//
// A human issuer can delegate member-tier test identity issuance to one
// pinned workload identity (a Google service account). The delegation
// (store.TestIdentityDelegation, created by the human on
// /api/v1/test-identity-delegations) is the credential. A request proves it
// with:
//
//   - Authorization: Bearer <Google ID token>, whose aud is exactly the
//     hub's issuer audience "scion-test-issuer:<hub id>" and whose sub is
//     the delegation's pinned subject, which must also be in the startup
//     allowlist server.hub.test_identities.workload_issuers;
//   - X-Scion-Issuer-Delegation: <delegation id>.
//
// The token is accepted only on the /api/v1/test-identities issuance routes.
// It is handled by its own arm of UnifiedAuthMiddleware that runs before
// every other credential arm, so it never reaches the external-bearer path
// (GoogleResolver), the federation path, or user provisioning, and no user
// row is ever created for the workload. The arm builds a context-only
// identity for the grantor: a hub-boundary *ScopedUserIdentity whose single
// scope is the delegation's scope and whose CredentialID is the delegation
// id. Authorization then runs as for an issuer PAT: the grantor's live
// bindings intersected with the delegation's frozen scope, at every request.
// The request credential context carries CredentialKindWorkloadDelegation,
// which testIdentityCaller admits and the mutation audit records.

const (
	// testIssuerDelegationHeader names the delegation a workload request
	// presents. The value is an id, not a secret.
	testIssuerDelegationHeader = "X-Scion-Issuer-Delegation"
	// testIssuerRunLabelHeader is an optional, self-asserted run label
	// recorded in the issuance audit for attribution only.
	testIssuerRunLabelHeader = "X-Scion-Run-Label"
	// testIssuerAudiencePrefix prefixes the hub id in the issuer audience.
	testIssuerAudiencePrefix = "scion-test-issuer:"
	// testIssuerRunLabelMaxRunes bounds a run label.
	testIssuerRunLabelMaxRunes = 64
	// testIdentityIssuanceRoutePath is the only route namespace a workload
	// token is accepted on.
	testIdentityIssuanceRoutePath = "/api/v1/test-identities"
)

// CredentialKindWorkloadDelegation is the request credential kind of a
// workload delegation request. Only serveWorkloadDelegation sets it.
const CredentialKindWorkloadDelegation = credentialmeta.KindWorkloadDelegation

// AuthTypeWorkloadDelegation labels a request authenticated by the workload
// delegation arm.
const AuthTypeWorkloadDelegation = "workload-delegation"

// testIdentityDelegationMaxAge is the longest a member-tier delegation may
// live, measured server-side from its creation.
const testIdentityDelegationMaxAge = 7 * 24 * time.Hour

// testIdentityDelegationScope is the only delegation scope in this release:
// member and viewer issuance.
const testIdentityDelegationScope = "test_identity:issue"

// testIdentityDelegationScopeTiers maps each delegation scope this hub knows
// to the workload issuer tier it needs. Admin scopes are recognised only to
// refuse them with a clear reason; they map to no tier.
var testIdentityDelegationScopeTiers = map[string]string{
	testIdentityDelegationScope: config.WorkloadIssuerTierMember,
}

// testIdentityDelegationAdminScopes are the admin-tier delegation scopes. A
// delegation create naming one gets 422 until the admin tiers support
// workload delegation.
var testIdentityDelegationAdminScopes = map[string]bool{
	"test_identity:issue_admin":       true,
	"test_identity:issue_super_admin": true,
}

// testIssuerAudience returns the issuer audience of the hub with id hubID.
// The binary computes it; it is never configured.
func testIssuerAudience(hubID string) string {
	if hubID == "" {
		return ""
	}
	return testIssuerAudiencePrefix + hubID
}

var (
	errWorkloadIssuerNoHubID          = errors.New("test identity workload issuers need a resolved hub id to compute the issuer audience")
	errWorkloadIssuerAudienceConflict = errors.New("the test identity issuer audience must differ from every other configured token audience")
)

// validateTestIdentityWorkloadIssuers checks the workload issuer startup
// settings in cfg: the allowlist itself (config.ValidateTestIdentityWorkloadIssuers),
// a resolved hub id, and that the issuer audience equals none of the other
// token audiences this hub accepts (the external-bearer expected_audience of
// every trusted issuer, the IAP audience, and the transport audience).
// New refuses to build a Server when it fails.
func validateTestIdentityWorkloadIssuers(cfg ServerConfig) error {
	if len(cfg.TestIdentityWorkloadIssuers) == 0 {
		return nil
	}
	if err := config.ValidateTestIdentityWorkloadIssuers(cfg.TestIdentityWorkloadIssuers); err != nil {
		return err
	}
	aud := testIssuerAudience(cfg.HubID)
	if aud == "" {
		return errWorkloadIssuerNoHubID
	}
	for _, other := range otherAcceptedAudiences(cfg) {
		if other.audience == aud {
			return fmt.Errorf("%w: it equals the %s", errWorkloadIssuerAudienceConflict, other.source)
		}
	}
	return nil
}

type namedAudience struct {
	source   string
	audience string
}

// otherAcceptedAudiences lists the token audiences, other than the issuer
// audience, that this hub's configuration accepts.
func otherAcceptedAudiences(cfg ServerConfig) []namedAudience {
	var out []namedAudience
	for i, ti := range cfg.Federation.TrustedIssuers {
		if ti.ExpectedAudience != "" {
			out = append(out, namedAudience{fmt.Sprintf("external-bearer expected_audience of trusted_issuers[%d]", i), ti.ExpectedAudience})
		}
	}
	if iap, ok := cfg.ProxyAuth.(*IAPAuthenticator); ok && iap != nil && iap.Audience != "" {
		out = append(out, namedAudience{"IAP audience (auth.proxy.iap.audience)", iap.Audience})
	}
	if cfg.TransportAudience != "" {
		out = append(out, namedAudience{"transport audience (auth.transport.oidcAudience)", cfg.TransportAudience})
	}
	return out
}

// workloadIssuerAuthenticator is the startup-fixed state of the workload
// delegation arm. It is immutable after New builds it.
type workloadIssuerAuthenticator struct {
	audience  string
	allowlist map[string]string // sub -> max_tier
	validator GoogleCredentialValidator
	store     store.Store
	now       func() time.Time
}

// newWorkloadIssuerAuthenticator builds the arm's state from validated
// config. It returns nil when no workload issuer is configured.
func newWorkloadIssuerAuthenticator(cfg ServerConfig, st store.Store, now func() time.Time) *workloadIssuerAuthenticator {
	if len(cfg.TestIdentityWorkloadIssuers) == 0 || st == nil {
		return nil
	}
	allow := make(map[string]string, len(cfg.TestIdentityWorkloadIssuers))
	for _, e := range cfg.TestIdentityWorkloadIssuers {
		allow[e.Sub] = e.MaxTier
	}
	v := cfg.TestIdentityWorkloadValidator
	if v == nil {
		v = NewGoogleCredentialValidator(nil)
	}
	return &workloadIssuerAuthenticator{
		audience:  testIssuerAudience(cfg.HubID),
		allowlist: allow,
		validator: v,
		store:     st,
		now:       now,
	}
}

func (a *workloadIssuerAuthenticator) clock() time.Time {
	if a.now == nil {
		return time.Now()
	}
	return a.now()
}

// subCovers reports whether sub is allow-listed with a max_tier that covers
// the tier scope needs. The admin tier covers the member tier; no admin
// entry is accepted at startup in this release.
func (a *workloadIssuerAuthenticator) subCovers(sub, scope string) bool {
	if a == nil {
		return false
	}
	maxTier, ok := a.allowlist[sub]
	if !ok {
		return false
	}
	need, ok := testIdentityDelegationScopeTiers[scope]
	if !ok {
		return false
	}
	switch need {
	case config.WorkloadIssuerTierMember:
		return maxTier == config.WorkloadIssuerTierMember || maxTier == config.WorkloadIssuerTierAdmin
	default:
		return false
	}
}

// unverifiedJWTClaims is the subset of an unverified JWT payload the arm
// reads before verification: only to route the request (aud) and, after the
// signature is verified, to read iat for the audit.
type unverifiedJWTClaims struct {
	Aud json.RawMessage `json:"aud"`
	Iat *int64          `json:"iat"`
}

// unverifiedJWTPayload returns the payload claims and aud values of a JWT
// without verifying it, or nils when token is not a decodable JWT. Callers
// use it only to route a request, or to read iat after the same token's
// signature has been verified.
func unverifiedJWTPayload(token string) (*unverifiedJWTClaims, []string) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, nil
	}
	var c unverifiedJWTClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, nil
	}
	var auds []string
	var one string
	if err := json.Unmarshal(c.Aud, &one); err == nil {
		auds = []string{one}
	} else if err := json.Unmarshal(c.Aud, &auds); err != nil {
		auds = nil
	}
	return &c, auds
}

// isWorkloadDelegationRequest reports whether r must be handled by the
// workload delegation arm: it names a delegation, or its bearer token is a
// JWT whose audience includes the issuer audience. The second case routes
// a workload token sent without the header to the arm (which refuses it),
// so such a token can never reach the external-bearer path.
//
// Cost model: UnifiedAuthMiddleware calls this for every request outside
// the unauthenticated endpoints. When no workload issuer is configured (a is nil, as on every
// hub without server.hub.test_identities.workload_issuers) it does one
// header lookup and nothing else: no bearer is read or decoded. Only on a
// hub with workload issuers does a JWT-shaped bearer get one base64 decode
// and one small JSON decode of its unverified payload, bounded by the
// header size and cheaper than the verification the request runs next.
// The audience check deliberately runs on every route, not only the
// issuance routes: a header-less workload token on any other route must
// reach the arm (and be refused there) instead of the external-bearer
// path.
func isWorkloadDelegationRequest(r *http.Request, a *workloadIssuerAuthenticator) bool {
	if r.Header.Get(testIssuerDelegationHeader) != "" {
		return true
	}
	if a == nil || a.audience == "" {
		return false
	}
	token := extractBearerToken(r)
	if token == "" || !looksLikeJWT(token) {
		return false
	}
	_, auds := unverifiedJWTPayload(token)
	for _, aud := range auds {
		if aud == a.audience {
			return true
		}
	}
	return false
}

// isTestIdentityIssuanceRoute reports whether r targets one of the four
// test identity issuance routes, the only routes a workload token is
// accepted on. The match is exact on the decoded path; an escaped path is
// refused.
func isTestIdentityIssuanceRoute(r *http.Request) bool {
	if r.URL.RawPath != "" {
		return false
	}
	p := r.URL.Path
	switch {
	case p == testIdentityIssuanceRoutePath:
		return r.Method == http.MethodPost || r.Method == http.MethodGet
	case strings.HasPrefix(p, testIdentityIssuanceRoutePath+"/"):
		rest := strings.TrimPrefix(p, testIdentityIssuanceRoutePath+"/")
		if id, ok := strings.CutSuffix(rest, "/token"); ok {
			return r.Method == http.MethodPost && validPathID(id)
		}
		return r.Method == http.MethodDelete && validPathID(rest)
	}
	return false
}

func validPathID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.Contains(id, "/")
}

// workloadDelegationState is the per-request state the arm attaches for
// the handlers: the delegation loaded for this request and the verified
// token facts the audit records. It never holds the token.
type workloadDelegationState struct {
	delegation *store.TestIdentityDelegation
	sub        string
	tokenIAT   time.Time
	runLabel   string
}

type workloadDelegationStateKey struct{}

func contextWithWorkloadDelegationState(ctx context.Context, st *workloadDelegationState) context.Context {
	return context.WithValue(ctx, workloadDelegationStateKey{}, st)
}

// workloadDelegationFromContext returns the request's workload delegation
// state, or nil when the request was not authenticated by the arm.
func workloadDelegationFromContext(ctx context.Context) *workloadDelegationState {
	st, _ := ctx.Value(workloadDelegationStateKey{}).(*workloadDelegationState)
	return st
}

// workloadDelegationCaller returns the request's workload delegation state
// when, and only when, the request credential is a workload delegation
// whose credential id matches the attached delegation. Every other request
// gets nil.
func workloadDelegationCaller(ctx context.Context) *workloadDelegationState {
	cred := GetCredentialContextFromContext(ctx)
	if cred.Kind != CredentialKindWorkloadDelegation {
		return nil
	}
	st := workloadDelegationFromContext(ctx)
	if st == nil || st.delegation == nil || st.delegation.ID == "" || st.delegation.ID != cred.ID {
		return nil
	}
	return st
}

// workloadDelegationRefusal is a refusal of the arm: an HTTP status and a
// log reason. The client sees a generic message only.
type workloadDelegationRefusal struct {
	status int
	reason string
}

func (r *workloadDelegationRefusal) Error() string { return r.reason }

func refuse401(reason string) *workloadDelegationRefusal {
	return &workloadDelegationRefusal{status: http.StatusUnauthorized, reason: reason}
}

func refuse403(reason string) *workloadDelegationRefusal {
	return &workloadDelegationRefusal{status: http.StatusForbidden, reason: reason}
}

// verifyWorkloadToken verifies a workload Google ID token: Google's
// signature, issuer and expiry (GoogleCredentialValidator.ValidateIDToken),
// an aud that is exactly the issuer audience (one value, nothing else), a
// service account subject, and returns the subject and iat.
func (a *workloadIssuerAuthenticator) verifyWorkloadToken(ctx context.Context, token string) (string, time.Time, *workloadDelegationRefusal) {
	if !looksLikeJWT(token) {
		return "", time.Time{}, refuse401("workload token is not a JWT")
	}
	claims, auds := unverifiedJWTPayload(token)
	if claims == nil || len(auds) != 1 || auds[0] != a.audience {
		return "", time.Time{}, refuse401("workload token audience is not exactly the issuer audience")
	}
	ident, err := a.validator.ValidateIDToken(ctx, token, []string{a.audience})
	if err != nil {
		if errors.Is(err, ErrGoogleUpstreamError) {
			return "", time.Time{}, &workloadDelegationRefusal{status: http.StatusServiceUnavailable, reason: "workload token verification unavailable"}
		}
		return "", time.Time{}, refuse401("workload token verification failed")
	}
	if ident == nil || ident.Audience != a.audience || ident.Subject == "" {
		return "", time.Time{}, refuse401("workload token audience or subject mismatch")
	}
	if !ident.IsServiceAccount {
		return "", time.Time{}, refuse401("workload token is not a service account token")
	}
	var iat time.Time
	if claims.Iat != nil {
		iat = time.Unix(*claims.Iat, 0).UTC()
	}
	return ident.Subject, iat, nil
}

// workloadDelegationAuth is a successful authentication by the workload
// delegation arm: the context-only grantor identity, its request
// credential context, and the state the handlers read. The auth middleware
// (serveWorkloadDelegation in auth.go) installs them on the request.
type workloadDelegationAuth struct {
	identity   *ScopedUserIdentity
	credential CredentialContext
	state      *workloadDelegationState
}

// authenticate runs the whole arm for one request and returns what to
// serve it as, or a refusal. Refusal statuses: 401 for any failure to prove
// the credential (route, token, unknown delegation, subject); 403 when the
// credential is proved but no longer grants anything (revoked, past its
// age, out of the allowlist ceiling, grantor not a live human); 503 when a
// dependency cannot answer.
func (a *workloadIssuerAuthenticator) authenticate(ctx context.Context, r *http.Request, enabled bool) (*workloadDelegationAuth, *workloadDelegationRefusal) {
	if a == nil || !enabled {
		return nil, refuse401("workload issuer delegation is not available on this hub")
	}
	if !isTestIdentityIssuanceRoute(r) {
		return nil, refuse401("workload token presented outside the test identity issuance routes")
	}
	for _, h := range []string{"X-Scion-Agent-Token", FederationTokenHeader, "X-Scion-Broker-ID"} {
		if r.Header.Get(h) != "" {
			return nil, refuse401("workload delegation request carries another credential")
		}
	}
	token := extractBearerToken(r)
	if token == "" {
		return nil, refuse401("workload delegation request has no bearer token")
	}
	delegationID := strings.TrimSpace(r.Header.Get(testIssuerDelegationHeader))
	if delegationID == "" {
		return nil, refuse401("workload token presented without a delegation id")
	}
	runLabel := r.Header.Get(testIssuerRunLabelHeader)
	if utf8.RuneCountInString(runLabel) > testIssuerRunLabelMaxRunes || hasControlChar(runLabel, false) || !utf8.ValidString(runLabel) {
		return nil, refuse401("invalid run label")
	}

	sub, iat, refusal := a.verifyWorkloadToken(ctx, token)
	if refusal != nil {
		return nil, refusal
	}

	d, err := a.store.GetTestIdentityDelegation(ctx, delegationID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, refuse401("unknown delegation")
	}
	if err != nil {
		return nil, &workloadDelegationRefusal{status: http.StatusServiceUnavailable, reason: "delegation lookup failed"}
	}
	if d.WorkloadSub != sub {
		return nil, refuse401("workload subject does not match the delegation")
	}
	now := a.clock()
	if refusal := a.checkDelegationLive(d, now); refusal != nil {
		return nil, refusal
	}

	grantor, refusal := a.liveHumanGrantor(ctx, d.GrantorID)
	if refusal != nil {
		return nil, refusal
	}

	ceiling, ok := permissions.BuildCeilingFromSelectors([]string{d.Scope})
	if !ok || len(ceiling.PermissionIDs) != 1 || ceiling.PermissionIDs[0] != permissionTestIdentityIssue {
		return nil, refuse403("delegation scope does not resolve to test_identity.issue")
	}
	base := NewAuthenticatedUser(grantor.ID, grantor.Email, grantor.DisplayName, grantor.Role, string(ClientTypeAPI))
	identity := NewScopedUserIdentityWithBoundary(base, TokenBoundary{Kind: BoundaryKindHub}, []string{d.Scope}, d.ID, ceiling)
	cred := credentialContextForIdentity(identity)
	cred.Kind = CredentialKindWorkloadDelegation
	cred.Type = string(CredentialKindWorkloadDelegation)
	return &workloadDelegationAuth{
		identity:   identity,
		credential: cred,
		state:      &workloadDelegationState{delegation: d, sub: sub, tokenIAT: iat, runLabel: runLabel},
	}, nil
}

// checkDelegationLive applies DEC-3 C2/C3 to a loaded delegation: not
// revoked, within its server-side age (its stored expiry and the hard
// maximum age from its creation time), exactly one known scope, and a
// pinned subject the startup allowlist still covers for that scope.
func (a *workloadIssuerAuthenticator) checkDelegationLive(d *store.TestIdentityDelegation, now time.Time) *workloadDelegationRefusal {
	switch {
	case d.RevokedAt != nil:
		return refuse403("delegation revoked")
	case !now.Before(d.ExpiresAt):
		return refuse403("delegation expired")
	case !now.Before(d.Created.Add(testIdentityDelegationMaxAge)):
		return refuse403("delegation past its maximum age")
	case d.Scope != testIdentityDelegationScope:
		return refuse403("delegation scope not accepted")
	case !a.subCovers(d.WorkloadSub, d.Scope):
		return refuse403("workload subject not allow-listed for the delegation's tier")
	}
	return nil
}

// liveHumanGrantor reads the grantor's row from the store on every
// request and fails closed unless it is an active human user outside the
// reserved test-identity domain.
func (a *workloadIssuerAuthenticator) liveHumanGrantor(ctx context.Context, grantorID string) (*store.User, *workloadDelegationRefusal) {
	u, err := a.store.GetUser(ctx, grantorID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, refuse403("delegation grantor no longer exists")
	case err != nil:
		return nil, refuse403("delegation grantor could not be read")
	case u.Kind != store.UserKindHuman:
		return nil, refuse403("delegation grantor is not a human user")
	case u.Status != store.UserStatusActive:
		return nil, refuse403("delegation grantor is not active")
	case isReservedTestIdentityEmail(u.Email):
		return nil, refuse403("delegation grantor is in the reserved test identity domain")
	}
	return u, nil
}

// writeWorkloadDelegationRefusal writes the client response of a
// refusal: a generic message per status, never the reason.
func writeWorkloadDelegationRefusal(w http.ResponseWriter, refusal *workloadDelegationRefusal) {
	switch refusal.status {
	case http.StatusForbidden:
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "workload delegation does not grant this request", nil)
	case http.StatusServiceUnavailable:
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, "unable to verify workload delegation", nil)
	default:
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "invalid workload delegation credential", nil)
	}
}
