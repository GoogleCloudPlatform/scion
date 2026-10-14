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

// A denied sign-in reports why it was denied: unauthorized_domain when the
// email domain is not allowed, invite_only when the hub is invite-only and
// the email has no invite or active account (ptone/scion#3330).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckSignInDenial_Reasons(t *testing.T) {
	st := newInviteFlowStore()
	st.users["u1"] = &store.User{ID: "u1", Email: "invited@allowed.com", Status: store.UserStatusInvited}

	tests := []struct {
		name    string
		email   string
		domains []string
		admins  []string
		mode    string
		want    string
	}{
		{"open mode allows", "anyone@other.com", nil, nil, "open", ""},
		{"admin always allowed", "boss@other.com", []string{"allowed.com"}, []string{"boss@other.com"}, "invite_only", ""},
		{"domain_restricted outside domain", "user@other.com", []string{"allowed.com"}, nil, "domain_restricted", signInDeniedDomain},
		{"domain_restricted inside domain", "user@allowed.com", []string{"allowed.com"}, nil, "domain_restricted", ""},
		{"domain_restricted without domains", "user@allowed.com", nil, nil, "domain_restricted", signInDeniedDomain},
		{"invite_only without invite", "uninvited@allowed.com", nil, nil, "invite_only", signInDeniedInviteOnly},
		{"invite_only with invite", "invited@allowed.com", nil, nil, "invite_only", ""},
		{"invite_only domain checked first", "user@other.com", []string{"allowed.com"}, nil, "invite_only", signInDeniedDomain},
		{"invite_only inside domain without invite", "uninvited@allowed.com", []string{"allowed.com"}, nil, "invite_only", signInDeniedInviteOnly},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := checkSignInDenial(context.Background(), tc.email, tc.domains, tc.admins, tc.mode, st)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.want == "",
				checkUserAuthorized(context.Background(), tc.email, tc.domains, tc.admins, tc.mode, st),
				"checkUserAuthorized must agree with checkSignInDenial")
		})
	}
}

func TestOAuthCallback_DenialReasonCode(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		domains []string
		email   string
		want    string
	}{
		{"invite_only without invite", "invite_only", nil, "uninvited@other.example", "/login?error=invite_only"},
		{"domain_restricted outside domain", "domain_restricted", []string{"allowed.example"}, "user@other.example", "/login?error=unauthorized_domain"},
		{"invite_only outside domain", "invite_only", []string{"allowed.example"}, "user@other.example", "/login?error=unauthorized_domain"},
		{"invite_only inside domain without invite", "invite_only", []string{"allowed.example"}, "user@allowed.example", "/login?error=invite_only"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newLiveModeStore()
			srv := newLiveModeServer(st, tc.mode)
			srv.config.AuthorizedDomains = tc.domains

			ws := newTestWebServer(t, WebServerConfig{
				SessionSecret: "test-session-secret-for-denial-reason-1234567890",
				BaseURL:       "http://localhost:8080",
			})
			ws.oauthService = NewOAuthService(OAuthConfig{
				Web: OAuthClientConfig{
					Google: OAuthProviderConfig{ClientID: "test-client-id", ClientSecret: "test-client-secret"},
				},
			}, nil)
			ws.store = st
			ws.SetAccessSettingsProvider(srv)

			assert.Equal(t, tc.want, oauthCallbackLogin(t, ws, tc.email))
			_, err := st.GetUserByEmail(context.Background(), tc.email)
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}

func TestProvisionUser_DenialReason(t *testing.T) {
	ctx := context.Background()

	inviteSrv := newLiveModeServer(newInviteFlowStore(), "invite_only")
	_, err := inviteSrv.provisionUser(ctx, &ExternalUserInfo{Email: "uninvited@other.example"})
	require.ErrorIs(t, err, ErrAccessDenied)
	require.ErrorIs(t, err, ErrInviteRequired)

	domainSrv := newLiveModeServer(newInviteFlowStore(), "domain_restricted")
	domainSrv.config.AuthorizedDomains = []string{"allowed.example"}
	_, err = domainSrv.provisionUser(ctx, &ExternalUserInfo{Email: "user@other.example"})
	require.ErrorIs(t, err, ErrAccessDenied)
	require.NotErrorIs(t, err, ErrInviteRequired)
}

func TestWriteSignInDenied_Code(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"invite required", ErrInviteRequired, "invite_only"},
		{"domain denied", ErrAccessDenied, "unauthorized_domain"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeSignInDenied(rec, tc.err)
			require.Equal(t, http.StatusForbidden, rec.Code)
			var resp ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, tc.want, resp.Error.Code)
		})
	}
}

// inviteAuditRecorder records invite audit events. Only
// LogInviteAuditEvent is implemented; other methods are not called.
type inviteAuditRecorder struct {
	AuditLogger
	events []*InviteAuditEvent
}

func (r *inviteAuditRecorder) LogInviteAuditEvent(_ context.Context, e *InviteAuditEvent) error {
	r.events = append(r.events, e)
	return nil
}

// signInDenialAuditCases covers each refusal reason in each access mode
// that can refuse it. The audit reason follows the refusal, not the mode
// (ptone/scion#4446).
var signInDenialAuditCases = []struct {
	name       string
	mode       string
	domains    []string
	email      string
	wantReason string
	wantInvite bool
}{
	{"open outside domain", "open", []string{"allowed.example"}, "user@other.example", "domain_not_authorized", false},
	{"domain_restricted outside domain", "domain_restricted", []string{"allowed.example"}, "user@other.example", "domain_not_authorized", false},
	{"domain_restricted without domains", "domain_restricted", nil, "user@allowed.example", "domain_not_authorized", false},
	{"invite_only without invite", "invite_only", nil, "uninvited@other.example", "not_on_allow_list", true},
	{"invite_only outside domain", "invite_only", []string{"allowed.example"}, "user@other.example", "domain_not_authorized", false},
	{"invite_only inside domain without invite", "invite_only", []string{"allowed.example"}, "user@allowed.example", "not_on_allow_list", true},
}

func TestSignInDenialAuditReason(t *testing.T) {
	tests := []struct {
		denial, mode, want string
	}{
		{signInDeniedDomain, "open", "domain_not_authorized"},
		{signInDeniedDomain, "domain_restricted", "domain_not_authorized"},
		{signInDeniedDomain, "invite_only", "domain_not_authorized"},
		{signInDeniedInviteOnly, "invite_only", "not_on_allow_list"},
		{signInDeniedInviteOnly, "domain_restricted", "not_on_allow_list"},
		{"", "invite_only", "not_on_allow_list"},
		{"", "domain_restricted", "domain_not_authorized"},
		{"", "open", "domain_not_authorized"},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, signInDenialAuditReason(tc.denial, tc.mode),
			"denial=%q mode=%q", tc.denial, tc.mode)
	}
}

// A denial code without a mapped audit reason is recorded as itself,
// not replaced by a reason guessed from the access mode.
func TestSignInDenialAuditReason_UnknownCode(t *testing.T) {
	for _, mode := range []string{"open", "domain_restricted", "invite_only"} {
		assert.Equal(t, "some_new_code", signInDenialAuditReason("some_new_code", mode),
			"mode=%q", mode)
	}
}

func TestProvisionUser_DenialAuditReason(t *testing.T) {
	for _, tc := range signInDenialAuditCases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newLiveModeServer(newInviteFlowStore(), tc.mode)
			srv.config.AuthorizedDomains = tc.domains
			rec := &inviteAuditRecorder{}
			srv.auditLogger = rec

			_, err := srv.provisionUser(context.Background(), &ExternalUserInfo{Email: tc.email})
			require.ErrorIs(t, err, ErrAccessDenied)
			assert.Equal(t, tc.wantInvite, errors.Is(err, ErrInviteRequired))

			require.Len(t, rec.events, 1)
			assert.Equal(t, InviteAuditLoginDenied, rec.events[0].EventType)
			assert.False(t, rec.events[0].Success)
			assert.Equal(t, tc.email, rec.events[0].Email)
			assert.Equal(t, tc.wantReason, rec.events[0].FailReason)
		})
	}
}

// The auditDenied callback fires when applyLiveSignInPolicy refuses an
// existing record (the identity resolver path, preAuthorized=false).
func TestSignInPolicyDeps_AuditDeniedReason(t *testing.T) {
	for _, tc := range signInDenialAuditCases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newLiveModeServer(newInviteFlowStore(), tc.mode)
			srv.config.AuthorizedDomains = tc.domains
			rec := &inviteAuditRecorder{}
			srv.auditLogger = rec

			user := &store.User{ID: "u1", Email: tc.email, Status: store.UserStatusActive}
			_, err := applyLiveSignInPolicy(context.Background(), srv.signInPolicyDeps(),
				user, "", "", false, signInPolicyPersistOpts{})
			require.ErrorIs(t, err, ErrAccessDenied)

			require.Len(t, rec.events, 1)
			assert.Equal(t, InviteAuditLoginDenied, rec.events[0].EventType)
			assert.False(t, rec.events[0].Success)
			assert.Equal(t, tc.wantReason, rec.events[0].FailReason)
		})
	}
}

// countingLookupStore counts invited-or-active lookups. When results is
// non-empty, call n returns results[n] (the last entry repeats), so a
// test can change the user's state between two lookups.
type countingLookupStore struct {
	*inviteFlowStore
	lookups int
	results []bool
}

func (s *countingLookupStore) IsUserInvitedOrActive(ctx context.Context, email string) (bool, error) {
	s.lookups++
	if len(s.results) == 0 {
		return s.inviteFlowStore.IsUserInvitedOrActive(ctx, email)
	}
	i := s.lookups - 1
	if i >= len(s.results) {
		i = len(s.results) - 1
	}
	return s.results[i], nil
}

// A refused invite-only sign-in through provisionUser makes one user
// lookup and records the decision's reason (ptone/scion#4519).
func TestProvisionUser_InviteOnlyRefusalSingleLookup(t *testing.T) {
	st := &countingLookupStore{inviteFlowStore: newInviteFlowStore()}
	srv := newLiveModeServer(st, "invite_only")
	rec := &inviteAuditRecorder{}
	srv.auditLogger = rec

	_, err := srv.provisionUser(context.Background(), &ExternalUserInfo{Email: "uninvited@other.example"})
	require.ErrorIs(t, err, ErrInviteRequired)
	assert.Equal(t, 1, st.lookups)
	require.Len(t, rec.events, 1)
	assert.Equal(t, signInAuditReasonInviteOnly, rec.events[0].FailReason)
}

// applyLiveSignInPolicy with the server's deps evaluates the policy once
// for a refusal: the audit entry uses the decision's code rather than a
// second lookup (ptone/scion#4519).
func TestSignInPolicyDeps_InviteOnlyRefusalSingleLookup(t *testing.T) {
	st := &countingLookupStore{inviteFlowStore: newInviteFlowStore()}
	srv := newLiveModeServer(st, "invite_only")
	rec := &inviteAuditRecorder{}
	srv.auditLogger = rec

	user := &store.User{ID: "u1", Email: "user@other.example", Status: store.UserStatusActive}
	_, err := applyLiveSignInPolicy(context.Background(), srv.signInPolicyDeps(),
		user, "", "", false, signInPolicyPersistOpts{})
	require.ErrorIs(t, err, ErrAccessDenied)
	assert.Equal(t, 1, st.lookups)
	require.Len(t, rec.events, 1)
	assert.Equal(t, signInAuditReasonInviteOnly, rec.events[0].FailReason)
}

// The audit entry records the reason the decision used even when the
// policy inputs change right after the decision: here the domain list
// changes so a second evaluation would report domain_not_authorized
// (ptone/scion#4519).
func TestSignInPolicyDeps_AuditReasonMatchesDecisionAfterChange(t *testing.T) {
	st := &countingLookupStore{inviteFlowStore: newInviteFlowStore(), results: []bool{false, true}}
	srv := newLiveModeServer(st, "invite_only")
	rec := &inviteAuditRecorder{}
	srv.auditLogger = rec

	deps := srv.signInPolicyDeps()
	decide := deps.signInDenial
	require.NotNil(t, decide)
	deps.signInDenial = func(ctx context.Context, email string) string {
		denial := decide(ctx, email)
		srv.config.AuthorizedDomains = []string{"elsewhere.example"}
		return denial
	}

	user := &store.User{ID: "u1", Email: "user@other.example", Status: store.UserStatusActive}
	_, err := applyLiveSignInPolicy(context.Background(), deps,
		user, "", "", false, signInPolicyPersistOpts{})
	require.ErrorIs(t, err, ErrAccessDenied)
	assert.Equal(t, 1, st.lookups)
	require.Len(t, rec.events, 1)
	assert.Equal(t, signInAuditReasonInviteOnly, rec.events[0].FailReason)
}

// The user's state changes between the decision and the audit step: the
// first lookup refuses and any later lookup would admit. The audit entry
// still records the refusal reason (ptone/scion#4519).
func TestSignInPolicyDeps_AuditReasonMatchesDecisionAfterStateChange(t *testing.T) {
	st := &countingLookupStore{inviteFlowStore: newInviteFlowStore(), results: []bool{false, true}}
	srv := newLiveModeServer(st, "invite_only")
	srv.config.AuthorizedDomains = []string{"other.example"}
	rec := &inviteAuditRecorder{}
	srv.auditLogger = rec

	user := &store.User{ID: "u1", Email: "user@other.example", Status: store.UserStatusActive}
	_, err := applyLiveSignInPolicy(context.Background(), srv.signInPolicyDeps(),
		user, "", "", false, signInPolicyPersistOpts{})
	require.ErrorIs(t, err, ErrAccessDenied)
	assert.Equal(t, 1, st.lookups)
	require.Len(t, rec.events, 1)
	assert.Equal(t, signInAuditReasonInviteOnly, rec.events[0].FailReason)
}

// The identity resolver (GE exchange and external-bearer paths), wired as
// server.go wires it, refuses an existing invite-only record with one
// lookup and records the decision's reason (ptone/scion#4519).
func TestGoogleIdentityResolver_InviteOnlyRefusalSingleLookup(t *testing.T) {
	st := &countingLookupStore{inviteFlowStore: newInviteFlowStore(), results: []bool{false, true}}
	st.users["u1"] = &store.User{ID: "u1", Email: "user@gmail.com", Status: store.UserStatusActive}
	srv := newLiveModeServer(st, "invite_only")
	rec := &inviteAuditRecorder{}
	srv.auditLogger = rec

	resolver := NewGoogleIdentityResolver(st, newMemExtIDStore(), srv.isUserAuthorized, nil, nil)
	resolver.SetSignInDenial(srv.signInDenial)
	resolver.SetSignInPolicyDeps(srv.signInPolicyDeps())

	_, err := resolver.Resolve(context.Background(), &ValidatedGoogleIdentity{
		Subject:       "sub-4519",
		Email:         "user@gmail.com",
		EmailVerified: true,
		Issuer:        googleIssuerHTTPS,
	}, ResolvePolicy{})
	require.ErrorIs(t, err, ErrAccessDenied)
	assert.Equal(t, 1, st.lookups)
	require.Len(t, rec.events, 1)
	assert.Equal(t, InviteAuditLoginDenied, rec.events[0].EventType)
	assert.Equal(t, signInAuditReasonInviteOnly, rec.events[0].FailReason)
}

// SetSignInDenial makes the resolver's authorize check agree with the
// code-returning check it was given.
func TestGoogleIdentityResolver_SetSignInDenialDerivesAuthorize(t *testing.T) {
	resolver := NewGoogleIdentityResolver(newFakeUserStore(), newMemExtIDStore(), alwaysAuthorized, nil, nil)
	resolver.SetSignInDenial(func(_ context.Context, email string) string {
		if email == "ok@example.com" {
			return ""
		}
		return signInDeniedDomain
	})
	ctx := context.Background()
	assert.True(t, resolver.authorize(ctx, "ok@example.com"))
	assert.False(t, resolver.authorize(ctx, "no@example.com"))
	deps := resolver.liveSignInPolicyDeps()
	require.NotNil(t, deps.signInDenial)
	assert.Equal(t, signInDeniedDomain, deps.signInDenial(ctx, "no@example.com"))

	resolver.SetSignInDenial(nil)
	assert.False(t, resolver.authorize(ctx, "no@example.com"), "nil keeps the current check")
}
