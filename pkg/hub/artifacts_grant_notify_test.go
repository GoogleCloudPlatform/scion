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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ask for review (ptone/scion#5145 phase 2), the hub side: the send gate
// and subject checks (MayNotifyGrant), the notice label (grantNoticeLabel),
// the notice row and its writer (NotifyGrant, writeUserNotification), and
// the named-user subject builder's C1 pin. The service's notify flow is
// tested in pkg/artifacts (grant_notify_test.go).

// grantNoticeStore wraps the hub store to force lookup and write faults and
// to report a user with another status (O4) or a stored email the store's
// own writes would normalise, without a store write.
type grantNoticeStore struct {
	store.Store
	mu         sync.Mutex
	getUserErr map[string]error
	status     map[string]string
	email      map[string]string
	createErr  error
}

func (s *grantNoticeStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	s.mu.Lock()
	err, status := s.getUserErr[id], s.status[id]
	email, hasEmail := s.email[id]
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	u, gerr := s.Store.GetUser(ctx, id)
	if gerr == nil && (status != "" || hasEmail) {
		c := *u
		if status != "" {
			c.Status = status
		}
		if hasEmail {
			c.Email = email
		}
		return &c, nil
	}
	return u, gerr
}

func (s *grantNoticeStore) CreateNotification(ctx context.Context, n *store.Notification) error {
	s.mu.Lock()
	err := s.createErr
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.Store.CreateNotification(ctx, n)
}

// grantNoticeEventSpy records the user and broadcast notification publishes.
type grantNoticeEventSpy struct {
	noopEventPublisher
	mu        sync.Mutex
	user      []*store.Notification
	broadcast int
}

func (e *grantNoticeEventSpy) PublishNotification(_ context.Context, _ *store.Notification) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.broadcast++
}

func (e *grantNoticeEventSpy) PublishUserNotification(_ context.Context, n *store.Notification) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.user = append(e.user, n)
}

func (e *grantNoticeEventSpy) counts() (int, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.user), e.broadcast
}

// grantNotifyFixture: project home with granter (a hub member who may read
// users), reader (a hub member of home) and nonMember (a home member who is
// not a hub member, so policy does not let it read other users); store is
// the wrapped hub store and events the publish spy.
type grantNotifyFixture struct {
	srv                        *Server
	s                          store.Store
	st                         *grantNoticeStore
	events                     *grantNoticeEventSpy
	host                       *artifactHost
	home                       string
	granter, reader, nonMember string
}

func newGrantNotifyFixture(t *testing.T) *grantNotifyFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	f := &grantNotifyFixture{
		srv: srv, s: s, home: tid("gn-home"),
		granter: tid("gn-granter"), reader: tid("gn-reader"), nonMember: tid("gn-non-member"),
	}
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: f.home, Name: "gn-home", Slug: "gn-home", Created: time.Now(), Updated: time.Now()}))
	createTestUserWithProjectRole(t, s, f.granter, "gn-granter@test.com", f.home, store.ProjectRoleOwner)
	createTestUserWithProjectRole(t, s, f.reader, "gn-reader@test.com", f.home, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, f.nonMember, "gn-non-member@test.com", f.home, store.ProjectRoleMember)
	ensureHubMembership(ctx, s, f.granter)
	ensureHubMembership(ctx, s, f.reader)
	f.st, _ = installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *grantNoticeStore {
		return &grantNoticeStore{Store: inner, getUserErr: map[string]error{}, status: map[string]string{}, email: map[string]string{}}
	})
	f.events = &grantNoticeEventSpy{}
	srv.SetEventPublisher(f.events)
	f.host = newArtifactHost(srv)
	return f
}

// session is ctx for user id on a credential of kind.
func (f *grantNotifyFixture) session(id string, kind CredentialKind) context.Context {
	u, err := f.s.GetUser(context.Background(), id)
	if err != nil {
		panic(err)
	}
	ctx := contextWithIdentity(context.Background(), NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb)))
	return contextWithCredentialContext(ctx, CredentialContext{Kind: kind})
}

// notices returns the notifications addressed to user id.
func (f *grantNotifyFixture) notices(t *testing.T, id string) []store.Notification {
	t.Helper()
	ns, err := f.s.GetNotifications(context.Background(), store.SubscriberTypeUser, id, false)
	require.NoError(t, err)
	return ns
}

func (f *grantNotifyFixture) notice(artifactID, subject, permission string) artifacts.GrantNotice {
	return artifacts.GrantNotice{ArtifactID: artifactID, HomeScope: f.home, SubjectUserID: subject, Permission: permission,
		ActorKind: artifacts.PrincipalKindUser, ActorRef: f.granter, Authority: artifacts.AuthorityOwner}
}

// TestGrantNotifyGate (condition B iii, K8): only an interactive or local
// dev session may send. Project- and hub-boundary access tokens with
// inbox:write, an artifact:manage-only token, a federated user, a broker
// acting on behalf of the user and an agent are refused with (false, nil),
// which the service answers not_permitted without charging the limiter or
// recording notified (pkg/artifacts TestGrantNotifyOrder). A CLI stored
// login is an interactive session and sends (route row below).
func TestGrantNotifyGate(t *testing.T) {
	f := newGrantNotifyFixture(t)
	u, err := f.s.GetUser(context.Background(), f.granter)
	require.NoError(t, err)
	user := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb))
	fed := NewFederatedUserIdentity("https://issuer.example", "sub-gn", u.Email, u.DisplayName, "member", nil)
	agent := artifactTestAgent(tid("gn-agent"), f.home, ScopesForRole(AgentRoleFull)...)
	cases := []struct {
		name     string
		identity Identity
		kind     CredentialKind
		want     bool
	}{
		{"interactive session", user, CredentialKindInteractive, true},
		{"dev session", user, CredentialKindDev, true},
		{"project-boundary token with inbox:write", selfToken(t, f.granter, projectBoundary(f.home), "inbox:write"), CredentialKindUAT, false},
		{"hub-boundary token with inbox:write", selfToken(t, f.granter, hubBoundary(), "inbox:write"), CredentialKindUAT, false},
		{"artifact:manage-only token", artifactTestUAT(t, user, f.home, "artifact:manage"), CredentialKindUAT, false},
		{"scoped token on an interactive credential", selfToken(t, f.granter, projectBoundary(f.home), "inbox:write"), CredentialKindInteractive, false},
		{"federated user", fed, CredentialKindFederation, false},
		{"federated user on an interactive credential", fed, CredentialKindInteractive, false},
		{"broker on behalf of the user", user, CredentialKindBroker, false},
		{"agent", agent, CredentialKindAgentJWT, false},
		{"no credential context", user, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := contextWithIdentity(context.Background(), tc.identity)
			if tc.kind != "" {
				ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: tc.kind})
			}
			ok, err := f.host.MayNotifyGrant(ctx, f.reader)
			require.NoError(t, err)
			assert.Equal(t, tc.want, ok)
		})
	}
	assert.Empty(t, f.notices(t, f.reader), "the send check writes nothing")

	t.Run("CLI stored login sends", func(t *testing.T) {
		enableArtifactsForTest(t, f.srv)
		srv := f.srv
		token, _, _, err := srv.userTokenService.GenerateTokenPair(u.ID, u.Email, u.DisplayName, u.Role, ClientTypeCLI)
		require.NoError(t, err)
		do := func(method, path string, body []byte) *httptest.ResponseRecorder {
			req := httptest.NewRequest(method, path, bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			return rec
		}
		rec := do(http.MethodPost, "/api/v1/artifacts?name=cli.md&scope="+f.home, []byte("# cli\n"))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		id := decodeArtifactID(t, rec)
		rec = do(http.MethodPost, "/api/v1/artifacts/"+id+"/grants",
			[]byte(`{"subjectKind":"principal","subjectRef":"user:`+f.reader+`","permission":"write","notify":true}`))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var resp artifacts.GrantResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.True(t, resp.Notified, "notifySkipped=%q", resp.NotifySkipped)
		assert.Len(t, f.notices(t, f.reader), 1)
	})
}

// TestGrantNotifyTokenOnRoute (D9, the bearer-matrix exclusion's reason):
// through the hub's mux, an artifact:read+manage access token of the owner
// posting a grant with notify gets the grant made, notifySkipped
// not_permitted, and no notice row: a token's notify is never a refusal of
// the grant, and never a notice.
func TestGrantNotifyTokenOnRoute(t *testing.T) {
	f := newGrantNotifyFixture(t)
	enableArtifactsForTest(t, f.srv)
	ctx := context.Background()
	g, err := f.s.GetUser(ctx, f.granter)
	require.NoError(t, err)
	rec := userArtifactRequest(t, f.srv, g, http.MethodPost, "/api/v1/artifacts?name=uat.md&scope="+f.home, []byte("# uat\n"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id := decodeArtifactID(t, rec)
	user := NewAuthenticatedUser(g.ID, g.Email, g.DisplayName, g.Role, string(ClientTypeWeb))
	token := artifactTestUAT(t, user, f.home, "artifact:read", "artifact:manage")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/"+id+"/grants",
		bytes.NewReader([]byte(`{"subjectKind":"principal","subjectRef":"user:`+f.reader+`","permission":"write","notify":true}`)))
	req = req.WithContext(contextWithCredentialContext(contextWithIdentity(req.Context(), token), CredentialContext{Kind: CredentialKindUAT}))
	rec = httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp artifacts.GrantResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.False(t, resp.Notified)
	assert.Equal(t, artifacts.NotifySkippedNotPermitted, resp.NotifySkipped)
	assert.Equal(t, artifacts.GrantWrite, resp.Grant.Permission)
	assert.Empty(t, f.notices(t, f.reader))
}

// TestInteractiveUserSessionMatchesScheduledSendCaller (condition F3, K33n):
// the F6 predicate and scheduledSendCaller decide the same way for every
// credential, through one shared check.
func TestInteractiveUserSessionMatchesScheduledSendCaller(t *testing.T) {
	user := NewAuthenticatedUser(tid("f6-user"), "f6@test.com", "F6", "member", string(ClientTypeWeb))
	scoped := NewScopedUserIdentity(user, tid("f6-project"), []string{"project:read"})
	fed := NewFederatedUserIdentity("https://issuer.example", "sub-f6", "f6@test.com", "F6", "member", nil)
	agent := artifactTestAgent(tid("f6-agent"), tid("f6-project"), ScopesForRole(AgentRoleFull)...)
	cases := []struct {
		name     string
		identity Identity
		kind     CredentialKind
		allowed  bool
	}{
		{"interactive session", user, CredentialKindInteractive, true},
		{"dev session", user, CredentialKindDev, true},
		{"broker on behalf of the user", user, CredentialKindBroker, false},
		{"no credential context", user, "", false},
		{"access token", scoped, CredentialKindUAT, false},
		{"scoped access token, interactive credential", scoped, CredentialKindInteractive, false},
		{"federated identity", fed, CredentialKindInteractive, false},
		{"federated identity, federation credential", fed, CredentialKindFederation, false},
		{"agent", agent, CredentialKindAgentJWT, false},
		{"no identity", nil, CredentialKindInteractive, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.identity != nil {
				ctx = contextWithIdentity(ctx, tc.identity)
			}
			if tc.kind != "" {
				ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: tc.kind})
			}
			_, viaPredicate := interactiveUserSession(ctx)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/conversations/x/scheduled", nil).WithContext(ctx)
			viaRoute := scheduledSendCaller(rec, req, ActionCreate) != nil
			assert.Equal(t, tc.allowed, viaPredicate, "interactiveUserSession")
			assert.Equal(t, tc.allowed, viaRoute, "scheduledSendCaller")
		})
	}
}

// TestScheduledSendCallerUnchanged: scheduledSendCaller's refusals are byte
// for byte what they were before it used the shared check: the plain 403
// without a user, and the scheduled-messages 403 for every other refusal.
func TestScheduledSendCallerUnchanged(t *testing.T) {
	user := NewAuthenticatedUser(tid("f6u-user"), "f6u@test.com", "F6U", "member", string(ClientTypeWeb))
	scoped := NewScopedUserIdentity(user, tid("f6u-project"), []string{"project:read"})
	plain := httptest.NewRecorder()
	Forbidden(plain)
	signedIn := httptest.NewRecorder()
	writeError(signedIn, http.StatusForbidden, ErrCodeForbidden, "scheduled messages require a signed-in session", nil)
	for _, tc := range []struct {
		name     string
		identity Identity
		kind     CredentialKind
		want     *httptest.ResponseRecorder
	}{
		{"no user", nil, CredentialKindInteractive, plain},
		{"broker", user, CredentialKindBroker, signedIn},
		{"scoped token", scoped, CredentialKindInteractive, signedIn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.identity != nil {
				ctx = contextWithIdentity(ctx, tc.identity)
			}
			ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: tc.kind})
			rec := httptest.NewRecorder()
			got := scheduledSendCaller(rec, httptest.NewRequest(http.MethodPost, "/x", nil).WithContext(ctx), ActionCreate)
			assert.Nil(t, got)
			assert.Equal(t, tc.want.Code, rec.Code)
			assert.Equal(t, tc.want.Body.String(), rec.Body.String())
		})
	}
}

// TestMayNotifyGrantSubject (v5 2.4, O4, K16, K26n): the subject must be an
// active user that can still sign in and that the caller may read; a
// missing, invited, pending or suspended user, an expired test identity and
// a user the caller may not read are refused; a failed lookup is a fault.
func TestMayNotifyGrantSubject(t *testing.T) {
	f := newGrantNotifyFixture(t)
	ctx := f.session(f.granter, CredentialKindInteractive)
	f.srv.testIdentities.enabled = true
	expired := tiStoreFixture(t, f.s, generateID(), time.Now().Add(-time.Minute))
	statused := func(status string) string {
		id := tid("gn-" + status)
		createTestUserWithProjectRole(t, f.s, id, "gn-"+status+"@test.com", f.home, store.ProjectRoleMember)
		ensureHubMembership(context.Background(), f.s, id)
		f.st.status[id] = status
		return id
	}
	cases := []struct {
		name    string
		subject string
		want    bool
	}{
		{"active reader", f.reader, true},
		{"missing", tid("gn-missing"), false},
		{"empty", "", false},
		{"invited", statused(store.UserStatusInvited), false},
		{"pending", statused(store.UserStatusPending), false},
		{"suspended", statused(store.UserStatusSuspended), false},
		{"expired test identity", expired.ID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := f.host.MayNotifyGrant(ctx, tc.subject)
			require.NoError(t, err)
			assert.Equal(t, tc.want, ok)
		})
	}

	t.Run("caller may not read the subject", func(t *testing.T) {
		caller := f.session(f.nonMember, CredentialKindInteractive)
		subject, err := f.s.GetUser(context.Background(), f.reader)
		require.NoError(t, err)
		granter := GetUserIdentityFromContext(caller)
		require.False(t, f.srv.authzService.CheckAccess(caller, granter, userResource(subject), ActionRead).Allowed,
			"fixture: a user outside hub-members may not read other users")
		ok, err := f.host.MayNotifyGrant(caller, f.reader)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("lookup fault", func(t *testing.T) {
		f.st.getUserErr[f.reader] = errors.New("store down")
		defer delete(f.st.getUserErr, f.reader)
		ok, err := f.host.MayNotifyGrant(ctx, f.reader)
		assert.Error(t, err)
		assert.False(t, ok)
	})
}

// TestGrantNoticeLabelSanitised (the label rule, K22): the label strips
// control and format runes and the line and paragraph separators, makes
// one line of at most 80 runes, and falls back to "a user" when nothing is
// left.
func TestGrantNoticeLabelSanitised(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Ada\x1b[2J Lovelace", "Ada[2J Lovelace"},
		{"Ada\r\nFake: line", "AdaFake: line"},
		{"Ada \u202eecilA", "Ada ecilA"},
		{"Ada\u2028Fake\u2029line", "AdaFakeline"},
		{"A\u200dda", "Ada"},
		{"  Ada \u00a0  Lovelace  ", "Ada Lovelace"},
		{"\x1b\r\n\u202e\u2028 ", ""},
	} {
		assert.Equal(t, tc.want, sanitizeNoticeLabel(tc.in), "%q", tc.in)
	}
	long := sanitizeNoticeLabel(strings.Repeat("é", 200))
	assert.Equal(t, 80, utf8.RuneCountInString(long))

	// Through grantNoticeLabel, for a subject that may not read the
	// granter (no email): a hostile stored display name.
	f := newGrantNotifyFixture(t)
	g, err := f.s.GetUser(context.Background(), f.granter)
	require.NoError(t, err)
	g.DisplayName = "Mallory\x1b[2J\r\nscion: system says\u202e" + strings.Repeat("x", 200)
	require.NoError(t, f.s.UpdateUser(context.Background(), g))
	label := f.srv.grantNoticeLabel(context.Background(), f.nonMember, f.granter)
	for _, bad := range []string{"\x1b", "\n", "\r", "\u202e"} {
		assert.NotContains(t, label, bad)
	}
	assert.Equal(t, 80, utf8.RuneCountInString(label))
	assert.True(t, strings.HasPrefix(label, "Mallory[2Jscion: system saysxxx"), label)

	g.DisplayName = "\x1b\r\n\u202e"
	require.NoError(t, f.s.UpdateUser(context.Background(), g))
	assert.Equal(t, "a user", f.srv.grantNoticeLabel(context.Background(), f.nonMember, f.granter))
}

// TestGrantNoticeLabelEmail (the email rule, K24n, K25n): the granter's email shows
// only when policy lets the subject read the granter; a lookup fault hides
// it and the notice is still written; with no display name a readable
// granter is named by email.
func TestGrantNoticeLabelEmail(t *testing.T) {
	f := newGrantNotifyFixture(t)
	ctx := context.Background()
	g, err := f.s.GetUser(ctx, f.granter)
	require.NoError(t, err)
	g.DisplayName = "Grace"
	require.NoError(t, f.s.UpdateUser(ctx, g))

	assert.Equal(t, "Grace (gn-granter@test.com)", f.srv.grantNoticeLabel(ctx, f.reader, f.granter), "shown: the subject may read the granter")
	assert.Equal(t, "Grace", f.srv.grantNoticeLabel(ctx, f.nonMember, f.granter), "hidden: the subject may not read the granter")

	t.Run("hidden on a lookup fault, notice still written", func(t *testing.T) {
		f.st.getUserErr[f.reader] = errors.New("store down")
		defer delete(f.st.getUserErr, f.reader)
		assert.Equal(t, "Grace", f.srv.grantNoticeLabel(ctx, f.reader, f.granter))
		id := uuid.NewString()
		require.NoError(t, f.host.NotifyGrant(ctx, f.notice(id, f.reader, artifacts.GrantRead)))
		ns := f.notices(t, f.reader)
		require.Len(t, ns, 1)
		assert.Equal(t, "Grace shared an artifact with you (Can view): scion://artifact/"+id, ns[0].Message)
	})

	t.Run("granter lookup fault", func(t *testing.T) {
		f.st.getUserErr[f.granter] = errors.New("store down")
		defer delete(f.st.getUserErr, f.granter)
		assert.Equal(t, "a user", f.srv.grantNoticeLabel(ctx, f.reader, f.granter))
		before := len(f.notices(t, f.reader))
		id := uuid.NewString()
		require.NoError(t, f.host.NotifyGrant(ctx, f.notice(id, f.reader, artifacts.GrantRead)), "the notice is never refused")
		ns := f.notices(t, f.reader)
		require.Len(t, ns, before+1)
		found := false
		for _, n := range ns {
			found = found || n.Message == "a user shared an artifact with you (Can view): scion://artifact/"+id
		}
		assert.True(t, found, "a notice naming \"a user\"")
	})

	g.DisplayName = ""
	require.NoError(t, f.s.UpdateUser(ctx, g))
	assert.Equal(t, "gn-granter@test.com", f.srv.grantNoticeLabel(ctx, f.reader, f.granter), "no name, readable: email only")
	assert.Equal(t, "a user", f.srv.grantNoticeLabel(ctx, f.nonMember, f.granter), "no name, not readable")

	// An email is shown whole or not at all: 81 runes -> "a user", 79 runes
	// -> the email; with a name, a combination over the cap -> the name.
	long := strings.Repeat("a", 81-len("@test.com")) + "@test.com"
	fits := strings.Repeat("b", 79-len("@test.com")) + "@test.com"
	require.Equal(t, 81, utf8.RuneCountInString(long))
	require.Equal(t, 79, utf8.RuneCountInString(fits))
	g.Email = long
	require.NoError(t, f.s.UpdateUser(ctx, g))
	assert.Equal(t, "a user", f.srv.grantNoticeLabel(ctx, f.reader, f.granter), "no name, 81-rune email")
	g.DisplayName = "Grace"
	require.NoError(t, f.s.UpdateUser(ctx, g))
	assert.Equal(t, "Grace", f.srv.grantNoticeLabel(ctx, f.reader, f.granter), "name, 81-rune email")
	g.DisplayName, g.Email = "", fits
	require.NoError(t, f.s.UpdateUser(ctx, g))
	assert.Equal(t, fits, f.srv.grantNoticeLabel(ctx, f.reader, f.granter), "no name, 79-rune email")

	// An email that cleaning would change is left out, so it never displays
	// as a different-looking address. The stored value is set through the
	// store override: UpdateUser normalises emails (lower case, trimmed),
	// but other write paths need not.
	for _, odd := range []string{"gn-gr\u200banter@test.com", "gn-granter@test\u202e.com", " gn-granter@test.com"} {
		f.st.mu.Lock()
		f.st.email[f.granter] = odd
		f.st.mu.Unlock()
		assert.Equal(t, "a user", f.srv.grantNoticeLabel(ctx, f.reader, f.granter), "email %q", odd)
	}
	f.st.mu.Lock()
	delete(f.st.email, f.granter)
	f.st.mu.Unlock()
}

// TestNamedUserSubjectNeverEntersAContext (carry-forward C1, K29n): the
// check-only identity namedUserSubject builds is called for only from the
// allow-listed functions; each binds it by an assignment to three plain
// identifiers, keeps at most one identity (not "_"), and passes it exactly
// once, as the subject argument of CheckAccess; no function of the files
// that build or use it puts any identity into a context.
func TestNamedUserSubjectNeverEntersAContext(t *testing.T) {
	allowed := map[string]bool{
		"artifacts_review_subject.go:ReviewSubjectAuthorize": true,
		"artifacts_grant_notify.go:MayNotifyGrant":           true,
		"artifacts_grant_notify.go:grantNoticeLabel":         true,
	}
	forbidden := map[string]bool{
		"WithValue": true, "contextWithIdentity": true, "contextWithCredentialContext": true,
		"WithIdentity": true, "withIdentity": true, "ContextWithIdentity": true,
	}
	files := map[string]bool{"artifacts_subject_user.go": true, "artifacts_review_subject.go": true, "artifacts_grant_notify.go": true}
	callers := map[string]bool{}
	scanned := 0
	forEachHubFile(t, func(name string, _ *token.FileSet, file *ast.File) {
		if files[name] {
			scanned++
			ast.Inspect(file, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && forbidden[extractCallSymbol(call)] {
					t.Errorf("%s calls %s: a named-user subject identity must never enter a context", name, extractCallSymbol(call))
				}
				return true
			})
		}
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			key := name + ":" + fd.Name.Name
			// The identity is the second result of namedUserSubject. Every
			// call must be the single right-hand side of a 3-name := or =
			// assignment whose three left-hand sides are plain identifiers,
			// so the scanner always knows the identity's name ("_" when it
			// is discarded); any other form fails, and a function may keep
			// at most one identity.
			var identity string
			bound := map[*ast.CallExpr]bool{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				st, ok := n.(*ast.AssignStmt)
				if !ok || len(st.Rhs) != 1 {
					return true
				}
				call, ok := st.Rhs[0].(*ast.CallExpr)
				if !ok || extractCallSymbol(call) != "namedUserSubject" {
					return true
				}
				if len(st.Lhs) != 3 {
					return true
				}
				for _, l := range st.Lhs {
					if _, ok := l.(*ast.Ident); !ok {
						t.Errorf("%s binds a namedUserSubject result to a non-identifier; the C1 scanner cannot follow it", key)
						return true
					}
				}
				bound[call] = true
				if id := st.Lhs[1].(*ast.Ident); id.Name != "_" {
					if identity != "" {
						t.Errorf("%s keeps more than one namedUserSubject identity (%s, %s)", key, identity, id.Name)
					}
					identity = id.Name
				}
				return true
			})
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && extractCallSymbol(call) == "namedUserSubject" {
					callers[key] = true
					if !bound[call] {
						t.Errorf("%s calls namedUserSubject outside a 3-identifier assignment; the C1 scanner cannot follow the identity", key)
					}
				}
				return true
			})
			if !callers[key] {
				continue
			}
			if !allowed[key] {
				t.Errorf("%s calls namedUserSubject; only %v may", key, allowed)
				continue
			}
			if identity == "" {
				continue
			}
			uses := 0
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				for i, arg := range call.Args {
					if id, ok := arg.(*ast.Ident); ok && id.Name == identity {
						uses++
						if extractCallSymbol(call) != "CheckAccess" || i != 1 {
							t.Errorf("%s passes the subject identity to %s (arg %d); only CheckAccess's subject argument is allowed",
								key, extractCallSymbol(call), i)
						}
					}
				}
				return true
			})
			if uses != 1 {
				t.Errorf("%s passes the subject identity to %d calls, want exactly 1 (CheckAccess)", key, uses)
			}
		}
	})
	if scanned != len(files) {
		t.Fatalf("scanned %d of %d files: scanner broken", scanned, len(files))
	}
	for key := range allowed {
		if !callers[key] {
			t.Errorf("allow-listed %s no longer calls namedUserSubject; update the list", key)
		}
	}
}

// TestGrantNoticeNoOwnerText (conditions C N1, K14/K19): a hostile artifact
// title never reaches the notice: the stored message is exactly the fixed
// template, with none of the title's bytes.
func TestGrantNoticeNoOwnerText(t *testing.T) {
	f := newGrantNotifyFixture(t)
	enableArtifactsForTest(t, f.srv)
	ctx := context.Background()
	g, err := f.s.GetUser(ctx, f.granter)
	require.NoError(t, err)
	g.DisplayName = "Grace"
	require.NoError(t, f.s.UpdateUser(ctx, g))
	title := "[click](https://evil.example) ![i](https://evil.example/i.png) @agent-slug\nscion: fake line"
	rec := userArtifactRequest(t, f.srv, g, http.MethodPost,
		"/api/v1/artifacts?"+url.Values{"name": {"h.md"}, "scope": {f.home}, "title": {title}}.Encode(), []byte("# h\n"))
	if rec.Code == http.StatusBadRequest {
		// A title with a line break is refused at publish; the rest of it
		// is still hostile.
		t.Log("publish refused a title with a newline; using it without")
		title = strings.ReplaceAll(title, "\n", " ")
		rec = userArtifactRequest(t, f.srv, g, http.MethodPost,
			"/api/v1/artifacts?"+url.Values{"name": {"h.md"}, "scope": {f.home}, "title": {title}}.Encode(), []byte("# h\n"))
	}
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id := decodeArtifactID(t, rec)
	for _, perm := range []string{artifacts.GrantReview, artifacts.GrantWrite} {
		require.NoError(t, f.host.NotifyGrant(ctx, f.notice(id, f.reader, perm)))
	}
	ns := f.notices(t, f.reader)
	require.Len(t, ns, 2)
	want := map[string]bool{
		"Grace (gn-granter@test.com) asked you to review an artifact (Can review): scion://artifact/" + id: true,
		"Grace (gn-granter@test.com) shared an artifact with you (Can edit): scion://artifact/" + id:       true,
	}
	for _, n := range ns {
		assert.True(t, want[n.Message], "message %q", n.Message)
		for _, frag := range []string{"click", "evil", "@agent-slug", "fake line", "\n"} {
			assert.NotContains(t, n.Message, frag)
		}
	}
}

// TestGrantNoticePublishesUserOnly (O3, K28n): a notice is published once,
// on the subject's own subject, and never as the broadcast notification
// event.
func TestGrantNoticePublishesUserOnly(t *testing.T) {
	f := newGrantNotifyFixture(t)
	require.NoError(t, f.host.NotifyGrant(context.Background(), f.notice(uuid.NewString(), f.reader, artifacts.GrantRead)))
	user, broadcast := f.counts()
	assert.Equal(t, 1, user)
	assert.Equal(t, 0, broadcast)
	f.events.mu.Lock()
	defer f.events.mu.Unlock()
	require.Len(t, f.events.user, 1)
	assert.Equal(t, f.reader, f.events.user[0].SubscriberID)
}

func (f *grantNotifyFixture) counts() (int, int) { return f.events.counts() }

// TestGrantNoticeNeverClaimed (carry-over F6): a grant notice is a user row:
// the agent dispatch claim does not return it and the stale expiry does not
// touch it.
func TestGrantNoticeNeverClaimed(t *testing.T) {
	f := newGrantNotifyFixture(t)
	ctx := context.Background()
	require.NoError(t, f.host.NotifyGrant(ctx, f.notice(uuid.NewString(), f.reader, artifacts.GrantRead)))
	row := f.notices(t, f.reader)
	require.Len(t, row, 1)
	for _, broker := range []string{"", tid("gn-broker")} {
		claimed, err := f.s.GetUndispatchedAgentNotifications(ctx, broker)
		require.NoError(t, err)
		for _, n := range claimed {
			assert.NotEqual(t, row[0].ID, n.ID, "grant notice claimed for agent dispatch")
		}
	}
	_, err := f.s.ExpireStaleAgentNotifications(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	after := f.notices(t, f.reader)
	require.Len(t, after, 1)
	assert.False(t, after[0].Dispatched)
	assert.Empty(t, after[0].DispatchOutcome)
	assert.Empty(t, after[0].DispatchReason)
}

// TestGrantNoticeRowShape (v7 section 3, N3): the exact row, for both
// statuses.
func TestGrantNoticeRowShape(t *testing.T) {
	f := newGrantNotifyFixture(t)
	ctx := context.Background()
	g, err := f.s.GetUser(ctx, f.granter)
	require.NoError(t, err)
	g.DisplayName = "Grace"
	require.NoError(t, f.s.UpdateUser(ctx, g))
	for _, tc := range []struct{ perm, status, text string }{
		{artifacts.GrantReview, store.NotificationStatusArtifactReviewRequested, "asked you to review an artifact (Can review)"},
		{artifacts.GrantRead, store.NotificationStatusArtifactShared, "shared an artifact with you (Can view)"},
		{artifacts.GrantWrite, store.NotificationStatusArtifactShared, "shared an artifact with you (Can edit)"},
		{artifacts.GrantAdmin, store.NotificationStatusArtifactShared, "shared an artifact with you (Can manage)"},
	} {
		t.Run(tc.perm, func(t *testing.T) {
			subject := tid("gn-shape-" + tc.perm)
			createTestUserWithProjectRole(t, f.s, subject, "gn-shape-"+tc.perm+"@test.com", f.home, store.ProjectRoleMember)
			ensureHubMembership(ctx, f.s, subject)
			id := uuid.NewString()
			require.NoError(t, f.host.NotifyGrant(ctx, f.notice(id, subject, tc.perm)))
			ns := f.notices(t, subject)
			require.Len(t, ns, 1)
			n := ns[0]
			assert.Equal(t, store.NotificationKindArtifactGrant, n.Kind)
			assert.Equal(t, store.NotificationResourceArtifact, n.ResourceType)
			assert.Equal(t, id, n.ResourceID)
			assert.Equal(t, tc.status, n.Status)
			assert.Equal(t, uuid.Nil.String(), n.SubscriptionID)
			assert.Equal(t, uuid.Nil.String(), n.AgentID)
			assert.Equal(t, uuidOrNil(f.home), n.ProjectID)
			assert.Equal(t, store.SubscriberTypeUser, n.SubscriberType)
			assert.Equal(t, subject, n.SubscriberID)
			assert.Equal(t, "Grace (gn-granter@test.com) "+tc.text+": scion://artifact/"+id, n.Message)
			assert.False(t, n.Dispatched)
			assert.False(t, n.Acknowledged)
		})
	}

	t.Run("refusals", func(t *testing.T) {
		agentActor := f.notice(uuid.NewString(), f.reader, artifacts.GrantRead)
		agentActor.ActorKind = artifacts.PrincipalKindAgent
		assert.Error(t, f.host.NotifyGrant(ctx, agentActor), "a non-user actor (ptone/scion#5548)")
		badPerm := f.notice(uuid.NewString(), f.reader, "owner")
		assert.Error(t, f.host.NotifyGrant(ctx, badPerm))
		badID := f.notice("not-a-uuid", f.reader, artifacts.GrantRead)
		assert.Error(t, f.host.NotifyGrant(ctx, badID))
		assert.ErrorIs(t, f.srv.writeUserNotification(ctx, userNotice{Kind: "other", ResourceType: store.NotificationResourceArtifact,
			ResourceID: uuid.NewString(), Status: "X", SubscriberUserID: f.reader, Message: "m"}), errUserNoticeInvalid)
		assert.Empty(t, f.notices(t, f.reader))
	})
}

// TestNoNotificationStatusLiterals (N3): the grant notice statuses are
// written only through the pkg/store constants: no other non-test Go file
// spells them.
func TestNoNotificationStatusLiterals(t *testing.T) {
	root := filepath.Join("..", "..")
	literals := []string{`"` + store.NotificationStatusArtifactReviewRequested + `"`, `"` + store.NotificationStatusArtifactShared + `"`}
	scanned := 0
	// pkgmove:scan-covers pkg/hub/...
	// recursive walk of the whole module: SkipDir only for module-level directories (node_modules, .git, web, docs-site, .scion), none under pkg/hub, so it covers every directory below pkg/hub.
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", "web", "docs-site", ".scion":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		scanned++
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, lit := range literals {
			if bytes.Contains(src, []byte(lit)) && filepath.ToSlash(filepath.Dir(rel)) != "pkg/store" {
				t.Errorf("%s spells %s; use the pkg/store constant", rel, lit)
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.Greater(t, scanned, 100, "scanner broken")
}

// TestGrantNoticeVisibleToSubjectOnly: the subject lists the notice; another
// user does not, and a project-boundary token of the subject for another
// project does not see it either.
func TestGrantNoticeVisibleToSubjectOnly(t *testing.T) {
	f := newGrantNotifyFixture(t)
	ctx := context.Background()
	require.NoError(t, f.host.NotifyGrant(ctx, f.notice(uuid.NewString(), f.reader, artifacts.GrantRead)))
	list := func(id string) []store.Notification {
		u, err := f.s.GetUser(ctx, id)
		require.NoError(t, err)
		rec := userArtifactRequest(t, f.srv, u, http.MethodGet, "/api/v1/notifications", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var ns []store.Notification
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ns))
		return ns
	}
	assert.Len(t, list(f.reader), 1)
	assert.Empty(t, list(f.granter))
	assert.Empty(t, list(f.nonMember))

	other := tid("gn-other-project")
	seedSelfScopeMember(t, f.s, f.reader, other)
	rows := f.notices(t, f.reader)
	require.Len(t, rows, 1)
	token := selfToken(t, f.reader, projectBoundary(other), "inbox:read")
	assert.Empty(t, scopeNotificationsForToken(f.srv.newSelfScopeCheck(ctx, token, "inbox.read"), rows))
	home := selfToken(t, f.reader, projectBoundary(f.home), "inbox:read")
	assert.Len(t, scopeNotificationsForToken(f.srv.newSelfScopeCheck(ctx, home, "inbox.read"), rows), 1)
}

// TestGrantNotifyInsertFailure (O5, K27n): when the notice cannot be
// written after the grant committed, the response says notified false and
// notifySkipped unavailable, the committed event keeps notified true, the
// decision is logged as notify_failed, and no row is stored or published.
func TestGrantNotifyInsertFailure(t *testing.T) {
	f := newGrantNotifyFixture(t)
	st, _ := enableArtifactsForTest(t, f.srv)
	g, err := f.s.GetUser(context.Background(), f.granter)
	require.NoError(t, err)
	rec := userArtifactRequest(t, f.srv, g, http.MethodPost, "/api/v1/artifacts?name=o5.md&scope="+f.home, []byte("# o5\n"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id := decodeArtifactID(t, rec)
	f.st.createErr = errors.New("disk full")
	rec = userArtifactRequest(t, f.srv, g, http.MethodPost, "/api/v1/artifacts/"+id+"/grants",
		[]byte(`{"subjectKind":"principal","subjectRef":"user:`+f.reader+`","permission":"read","notify":true}`))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp artifacts.GrantResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.False(t, resp.Notified)
	assert.Equal(t, artifacts.NotifySkippedUnavailable, resp.NotifySkipped)
	f.st.createErr = nil
	assert.Empty(t, f.notices(t, f.reader))
	user, broadcast := f.counts()
	assert.Equal(t, 0, user)
	assert.Equal(t, 0, broadcast)
	evs, err := st.ListEvents(context.Background(), id)
	require.NoError(t, err)
	found := false
	for _, e := range evs {
		if e.Action == artifacts.EventGrantCreated && e.Detail["subjectRef"] == "user:"+f.reader {
			found = true
			assert.Equal(t, true, e.Detail["notified"], "the append-only event keeps notified")
		}
	}
	assert.True(t, found, "grant_created event")
}

// TestGrantNotifyAgentToken (ptone/scion#7362 overlap): an agent token
// posting a notify body gets today's refusal and no notice is written.
func TestGrantNotifyAgentToken(t *testing.T) {
	f := newGrantNotifyFixture(t)
	enableArtifactsForTest(t, f.srv)
	_, tok := artifactAgent(t, f.srv, f.s, f.home, "gn-agent", AgentRoleFull)
	rec := doRawAgentRequest(t, f.srv, http.MethodPost, "/api/v1/artifacts?name=a.md&scope="+f.home, []byte("# a\n"), tok)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id := decodeArtifactID(t, rec)
	body := []byte(`{"subjectKind":"principal","subjectRef":"user:` + f.reader + `","permission":"review","notify":true}`)
	plain := []byte(`{"subjectKind":"principal","subjectRef":"user:` + f.reader + `","permission":"review"}`)
	want := doRawAgentRequest(t, f.srv, http.MethodPost, "/api/v1/artifacts/"+id+"/grants", plain, tok)
	got := doRawAgentRequest(t, f.srv, http.MethodPost, "/api/v1/artifacts/"+id+"/grants", body, tok)
	assert.Equal(t, http.StatusForbidden, got.Code, got.Body.String())
	assert.Equal(t, want.Code, got.Code)
	assert.Equal(t, want.Body.String(), got.Body.String())
	assert.Empty(t, f.notices(t, f.reader))
}

// TestArtifactsGrantNotifyOnRoutes: through the real hub, the steward of an
// agent's artifact (the owner of its home project) asks a home-project
// reader for review with notify: one notice is written to the reader, who
// can then open the artifact; the review grant is in place.
func TestArtifactsGrantNotifyOnRoutes(t *testing.T) {
	f := newGrantNotifyFixture(t)
	enableArtifactsForTest(t, f.srv)
	ctx := context.Background()
	_, tok := artifactAgent(t, f.srv, f.s, f.home, "gn-route-agent", AgentRoleFull)
	steward := tid("art-delegator-" + f.home)
	ensureHubMembership(ctx, f.s, steward)
	rec := doRawAgentRequest(t, f.srv, http.MethodPost, "/api/v1/artifacts?name=r.md&scope="+f.home, []byte("# r\n"), tok)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id := decodeArtifactID(t, rec)
	su, err := f.s.GetUser(ctx, steward)
	require.NoError(t, err)
	rec = userArtifactRequest(t, f.srv, su, http.MethodPost, "/api/v1/artifacts/"+id+"/grants",
		[]byte(`{"subjectKind":"principal","subjectRef":"user:`+f.reader+`","permission":"review","notify":true}`))
	require.Contains(t, []int{http.StatusCreated, http.StatusOK}, rec.Code, rec.Body.String())
	var resp artifacts.GrantResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Notified, "notifySkipped=%q", resp.NotifySkipped)
	ns := f.notices(t, f.reader)
	require.Len(t, ns, 1)
	assert.Equal(t, store.NotificationStatusArtifactReviewRequested, ns[0].Status)
	assert.Equal(t, id, ns[0].ResourceID)
	ru, err := f.s.GetUser(ctx, f.reader)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, userArtifactRequest(t, f.srv, ru, http.MethodGet, "/api/v1/artifacts/"+id, nil).Code)
}

// TestArtifactGrantNotifyCatalogEntry (conditions A5, F5): the catalog
// records artifact.grant.notify as a session-only variant of the grant
// route for user principals.
func TestArtifactGrantNotifyCatalogEntry(t *testing.T) {
	var spec *authzop.OperationSpec
	for i := range authzop.Catalog {
		if authzop.Catalog[i].ID == "artifact.grant.notify" {
			spec = &authzop.Catalog[i]
		}
	}
	require.NotNil(t, spec, "artifact.grant.notify missing from the catalog")
	assert.Equal(t, authzop.SessionOnly(authzop.ReasonInteractiveState), spec.Bearer)
	assert.Equal(t, []authzop.CredentialKind{authzop.CredentialSessionJWT}, spec.Credentials)
	assert.Equal(t, []authzop.PrincipalKind{authzop.PrincipalUser}, spec.Principals)
	require.Len(t, spec.EntryPoints, 1)
	ep := spec.EntryPoints[0]
	assert.Equal(t, "/api/v1/artifacts/{id}/grants", ep.Pattern)
	assert.Equal(t, "POST", ep.Method)
	assert.Equal(t, "notify", ep.Variant)
}
