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
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// normalizeBearerPattern replaces every "{name}" parameter with "{}", so
// patterns that name their parameters differently compare equal.
func normalizeBearerPattern(p string) string {
	return bearerPlaceholder.ReplaceAllString(p, "{}")
}

// splitRouteKey splits a route metadata key into its method prefix (empty
// when the key has none) and its path.
func splitRouteKey(key string) (method, path string) {
	if i := strings.Index(key, " /"); i >= 0 {
		return key[:i], key[i+1:]
	}
	return "", key
}

func newBearerMatrixFixture(t *testing.T) *bearerMatrixFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	ids := seedLiveInventoryFixtures(t, ctx, srv, s)
	// Project secret routes read and write through the secret backend.
	backend := secret.NewLocalBackend(s, "bdm-hub-id", "bdm-secret")
	srv.SetSecretBackend(backend)
	_, _, err := backend.Set(ctx, &secret.SetSecretInput{
		Name: ids.projectSecretKey, Value: "1", SecretType: secret.TypeEnvironment,
		Scope: secret.ScopeProject, ScopeID: ids.project,
	})
	require.NoError(t, err)

	adminID := tid("bdm-super-admin")
	createTestUserWithRole(t, s, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)
	ensureHubMembership(ctx, s, adminID)
	// Broker material reads need the owner relationship or broker.update,
	// and a token carries no broker.update, so the super-admin owns the
	// fixture broker; its secret sits in the same backend.
	owned, err := s.SetRuntimeBrokerCreatedByIfEmpty(ctx, ids.runtimeBroker, adminID)
	require.NoError(t, err)
	require.True(t, owned, "the fixture broker has no owner before the matrix assigns one")
	_, _, err = backend.Set(ctx, &secret.SetSecretInput{
		Name: ids.brokerSecretKey, Value: "1", SecretType: secret.TypeEnvironment,
		Scope: secret.ScopeRuntimeBroker, ScopeID: ids.runtimeBroker,
	})
	require.NoError(t, err)

	other := tid("bdm-other-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: other, Name: "BDM Other", Slug: "bdm-other"}))
	// Membership in the other project lets the super-admin mint tokens
	// bound to it.
	createTestUserWithProjectRole(t, s, adminID, adminID+"@test.com", other, store.ProjectRoleOwner)
	// The inbox routes act on the caller's own records, so the matrix
	// addresses the super-admin's records in the fixture project.
	ids.inbox = seedInboxRecords(t, ctx, s, adminID, ids.project, ids.agent)
	ids.userSkillInjection = seedUserSkillInjection(t, ctx, s, adminID)
	ids.userTemplate, ids.userTemplateDel = seedUserTemplates(t, ctx, s, adminID)
	// The chat routes likewise address the super-admin's own chat records.
	ids.chat = seedChatRecords(t, ctx, s, srv.webChatStore, adminID, ids.project, ids.chatTopic, ids.agent, ids.user)
	// The fixture agent takes messages from any user holding agent.message
	// on it, so the message action's admitting token reaches delivery.
	agent, err := s.GetAgent(ctx, ids.agent)
	require.NoError(t, err)
	agent.MessageMode = store.MessageModeProject
	require.NoError(t, s.UpdateAgent(ctx, agent))

	// The user the user delete row removes: a member with no role binding,
	// so a token's delete passes the system-scope binding rule.
	userDelete := tid("bdm-user-delete")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userDelete, Email: userDelete + "@test.com", DisplayName: "BDM User Delete", Role: "member", Status: "active"}))

	return &bearerMatrixFixture{srv: srv, store: s, ids: ids, adminID: adminID, otherProject: other, userDelete: userDelete, tokens: map[string]string{}}
}

// sessionOnlyDetailsOf returns details.reason and details.credential of an
// error response, or "" for each when absent.
func sessionOnlyDetailsOf(rec *httptest.ResponseRecorder) (reason, credential string) {
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return "", ""
	}
	reason, _ = resp.Error.Details["reason"].(string)
	credential, _ = resp.Error.Details["credential"].(string)
	return reason, credential
}

// bearerPlaceholder matches one "{name}" pattern parameter.
var bearerPlaceholder = regexp.MustCompile(`\{[^}/]*\}`)

// bearerMatrixFixture is a live server seeded with the live-inventory
// fixtures, a super-admin who holds live authority on every fixture, and a
// second project the super-admin's project tokens can be bound to.
type bearerMatrixFixture struct {
	srv          *Server
	store        store.Store
	ids          idFixtures
	adminID      string
	otherProject string
	// userDelete is the user the user delete row removes.
	userDelete string
	tokens     map[string]string
	// tokenIDs are the tokens mint and tryMint created since the last
	// releaseTokens.
	tokenIDs []string
}

// mint returns a real token for the super-admin, minted through
// UserAccessTokenService over a session, for the boundary and selectors.
// Tokens are cached per boundary and selector set.
func (m *bearerMatrixFixture) mint(t *testing.T, boundary TokenBoundary, scopes []string) string {
	t.Helper()
	cacheKey := string(boundary.Kind) + "|" + boundary.ProjectID + "|" + strings.Join(scopes, ",")
	if key, ok := m.tokens[cacheKey]; ok {
		return key
	}
	key, tok, err := m.srv.uatService.CreateTokenWithParams(rs4MintContext(m.adminID), CreateTokenParams{
		UserID: m.adminID, Name: "bdm-" + tid("tok"), Boundary: boundary, Scopes: scopes,
	})
	require.NoError(t, err, "mint %s token with %v", boundary.Kind, scopes)
	m.tokens[cacheKey] = key
	m.tokenIDs = append(m.tokenIDs, tok.ID)
	return key
}

// tryMint is mint for a selector set that may not be mintable; it returns
// "" when minting fails. Results are cached like mint's.
func (m *bearerMatrixFixture) tryMint(boundary TokenBoundary, scopes []string) string {
	cacheKey := "try|" + string(boundary.Kind) + "|" + boundary.ProjectID + "|" + strings.Join(scopes, ",")
	if key, ok := m.tokens[cacheKey]; ok {
		return key
	}
	key, tok, err := m.srv.uatService.CreateTokenWithParams(rs4MintContext(m.adminID), CreateTokenParams{
		UserID: m.adminID, Name: "bdm-" + tid("try"), Boundary: boundary, Scopes: scopes,
	})
	if err != nil {
		key = ""
	} else {
		m.tokenIDs = append(m.tokenIDs, tok.ID)
	}
	m.tokens[cacheKey] = key
	return key
}

// releaseTokens deletes the tokens mint and tryMint created and empties
// their cache. The matrix calls it before each admit row, so the tokens
// one row holds stay under the per-user token limit
// (store.UATMaxPerUser) however many rows the catalog has.
func (m *bearerMatrixFixture) releaseTokens(t *testing.T) {
	t.Helper()
	ctx := rs4MintContext(m.adminID)
	for _, id := range m.tokenIDs {
		require.NoError(t, m.srv.uatService.DeleteToken(ctx, m.adminID, id))
	}
	m.tokenIDs = nil
	clear(m.tokens)
}

// canMint reports whether the super-admin can mint a token for the
// boundary and selectors. The trial token is deleted, so probing does not
// count against the per-user token limit.
func (m *bearerMatrixFixture) canMint(t *testing.T, boundary TokenBoundary, scopes []string) bool {
	t.Helper()
	ctx := rs4MintContext(m.adminID)
	_, tok, err := m.srv.uatService.CreateTokenWithParams(ctx, CreateTokenParams{
		UserID: m.adminID, Name: "bdm-" + tid("probe"), Boundary: boundary, Scopes: scopes,
	})
	if err != nil {
		require.NotErrorIs(t, err, ErrUATLimitExceeded, "probing must not exhaust the token limit")
		return false
	}
	require.NoError(t, m.srv.uatService.DeleteToken(ctx, m.adminID, tok.ID))
	return true
}

// everySelectorHubToken mints a hub token for the super-admin that carries
// every selector the super-admin can mint on the hub boundary.
func (m *bearerMatrixFixture) everySelectorHubToken(t *testing.T) (string, []string) {
	t.Helper()
	var selectors []string
	seen := map[string]bool{}
	for _, p := range permissions.Registry {
		if p.UATScope == "" || seen[p.UATScope] {
			continue
		}
		seen[p.UATScope] = true
		if m.canMint(t, hubBoundary(), []string{p.UATScope}) {
			selectors = append(selectors, p.UATScope)
		}
	}
	sort.Strings(selectors)
	require.NotEmpty(t, selectors, "the super-admin can mint at least one hub selector")
	// Minted outside the mint cache, so releaseTokens keeps it.
	key, _, err := m.srv.uatService.CreateTokenWithParams(rs4MintContext(m.adminID), CreateTokenParams{
		UserID: m.adminID, Name: "bdm-" + tid("all"), Boundary: hubBoundary(), Scopes: selectors,
	})
	require.NoError(t, err, "mint hub token with every selector")
	return key, selectors
}

// request sends method to the entry point's live path with a real token.
// A stream is ended by the request deadline.
func (m *bearerMatrixFixture) request(t *testing.T, e bearerMatrixEntry, key string) *httptest.ResponseRecorder {
	t.Helper()
	ep := e.EntryPoint
	id := string(e.Spec.ID)
	params := m.callerPatternOverrides()[overrideKey{id, ep.Pattern}]
	if params == nil {
		params = opPatternOverrides(m.ids)[overrideKey{id, ep.Pattern}]
	}
	if params == nil {
		params = bearerMatrixPatternOverrides(m.ids)[ep.Pattern]
	}
	if params == nil {
		params = patternOverrides(m.ids)[ep.Pattern]
	}
	path := substituteLiveInventoryParams(ep.Pattern, params)
	if q, ok := bearerMatrixQueryOverrides(m.ids)[overrideKey{id, ep.Pattern}]; ok {
		path += "?" + q
	} else if q, ok := queryOverrides(m.ids)[ep.Pattern]; ok {
		path += "?" + q
	}
	var body []byte
	switch ep.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		var payload interface{} = map[string]interface{}{}
		if override, ok := bearerMatrixBodyOverrides(m.ids)[overrideKey{id, ep.Pattern}]; ok {
			payload = override
		} else if override, ok := bodyOverrides(m.ids)[overrideKey{id, ep.Pattern}]; ok {
			payload = override
		}
		var err error
		body, err = json.Marshal(payload)
		require.NoError(t, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest(ep.Method, path, bytes.NewReader(body)).WithContext(ctx)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	m.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// bearerMatrixBodyOverrides holds request bodies the matrix sends in place
// of the live-inventory bodies, for handlers that validate the body before
// they reach the credential check the matrix observes.
func bearerMatrixBodyOverrides(f idFixtures) map[overrideKey]map[string]interface{} {
	return map[overrideKey]map[string]interface{}{
		{"project.membership.add", "/api/v1/projects/{id}/members"}: {
			"roleDefinitionId": f.projectMemberRoleID, "principalType": "user", "principalId": f.user,
		},
		{"project.membership.transfer", "/api/v1/projects/{id}/transfer-ownership"}: {"newOwnerId": f.member},
		// Env and secret writes validate the body (a value is required)
		// before the scope's credential check: user scope is the default,
		// and hub-level writes name the scope in the body.
		{"env.user.write", "/api/v1/env/{key}"}:        {"value": "bdm"},
		{"secret.user.write", "/api/v1/secrets/{key}"}: {"value": "YmRt"},
		{"env.hub.write", "/api/v1/env/{key}"}:         {"scope": "hub", "value": "bdm"},
		{"secret.hub.write", "/api/v1/secrets/{key}"}:  {"scope": "hub", "value": "YmRt"},
		// The delegation ceiling applies to the role an agent is granted:
		// a token carrying only agent:create covers no usable role, so the
		// probe asks for agentRole "none", which every creator may grant.
		{"agent.lifecycle.create", "/api/v1/agents"}:                      {"name": "bdm-created", "projectId": f.project, "agentRole": "none"},
		{"agent.lifecycle.create", "/api/v1/projects/{projectId}/agents"}: {"name": "bdm-created-project", "agentRole": "none"},
		// handleSetMessageMode validates the mode before the authorization
		// check; the fixture agent is already in project mode.
		{"agent.setmessagemode", "/api/v1/agents/{id}/set_message_mode"}:                      {"mode": "project"},
		{"agent.setmessagemode", "/api/v1/projects/{projectId}/agents/{id}/set_message_mode"}: {"mode": "project"},
		// Inbox writes name the fixture project and agent.
		{"inbox.conversation.create", "/api/v1/conversations"}:                              {"displayName": f.inbox.createConversations + "-a", "projectId": f.project},
		{"inbox.conversation.create", "/api/v1/conversations/"}:                             {"displayName": f.inbox.createConversations + "-b", "projectId": f.project},
		{"inbox.conversation.defaultagent.set", "/api/v1/conversations/{id}/default-agent"}: {"agentId": f.agent},
		{"inbox.conversation.participant.add", "/api/v1/conversations/{id}/participants"}:   {"principalKind": "agent", "principalId": f.agent},
		{"inbox.notification.subscription.create", "/api/v1/notifications/subscriptions"}: {
			"projectId": f.project, "scope": "project", "triggerActivities": []string{"COMPLETED"},
		},
		{"inbox.notification.subscription.write", "/api/v1/notifications/subscriptions/{id}"}: {"triggerActivities": []string{"FAILED"}},
		// Thread group create names the fixture project; adding a thread
		// files the fixture group conversation (removed again by the
		// following DELETE .../threads/{conversationId} row).
		{"inbox.threadgroup.create", "/api/v1/thread-groups"}:              {"name": f.inbox.createConversations + "-thread-group", "projectId": f.project},
		{"inbox.threadgroup.update", "/api/v1/thread-groups/{id}/threads"}: {"conversationId": f.inbox.groupConversation},
		{"inbox.notification.template.create", "/api/v1/notifications/templates"}: {
			"name": f.inbox.createConversations + "-template", "triggerActivities": []string{"COMPLETED"}, "projectId": f.project,
		},
		// Explain names a fixture project target; the cross-principal
		// variant explains for the fixture user.
		{"user.self.explain", "/api/v1/authz/explain"}: {
			"resource": map[string]interface{}{"type": "project", "id": f.project}, "action": "read",
		},
		{"hub.audit.explain", "/api/v1/authz/explain"}: {
			"resource": map[string]interface{}{"type": "project", "id": f.project}, "action": "read", "principalId": f.user,
		},
		// The user PATCH variants change a display name: the fixture user's
		// for the cross-user variant, the caller's own for the self variant.
		{"user.update", "/api/v1/users/{id}"}:      {"displayName": "BDM User"},
		{"user.self.update", "/api/v1/users/{id}"}: {"displayName": "BDM Self"},
		// Chat writes with bodies the handlers accept.
		{"chat.prefs.update", "/api/v1/chat/prefs"}:                                    {"visibility_mode": "conversation"},
		{"chat.thread.create", "/api/v1/chat/spaces/{id}/threads"}:                     {"name": "bdm thread"},
		{"chat.space.emoji.update", "/api/v1/chat/spaces/{id}/emoji"}:                  {"emoji": "x"},
		{"chat.conversation.send.topic", "/api/v1/chat/conversations/{id}/messages"}:   {"content": "bdm thread message"},
		{"chat.conversation.send.dmagent", "/api/v1/chat/conversations/{id}/messages"}: {"content": "bdm agent message"},
		{"chat.conversation.send.dmuser", "/api/v1/chat/conversations/{id}/messages"}:  {"content": "bdm user message"},
		{"chat.message.edit", "/api/v1/chat/conversations/{id}/messages/{messageId}"}:  {"content": "bdm edited"},
		{"chat.conversation.markread.topic", "/api/v1/chat/conversations/{id}/read"}:   {"messageId": f.chat.topicMessage},
		{"chat.conversation.markread.dm", "/api/v1/chat/conversations/{id}/read"}:      {"messageId": f.chat.dmMessage},
		{"chat.conversation.mute.topic", "/api/v1/chat/conversations/{id}/mute"}:       {"muted": true},
		{"chat.conversation.mute.dm", "/api/v1/chat/conversations/{id}/mute"}:          {"muted": true},
		{"chat.conversation.pin.topic", "/api/v1/chat/conversations/{id}/pin"}:         {"pinned": true},
		{"chat.conversation.pin.dm", "/api/v1/chat/conversations/{id}/pin"}:            {"pinned": true},
		{"chat.presence.update", "/api/v1/chat/presence"}:                              {"projectIds": []string{f.project}},
		{"chat.userprefs.update", "/api/v1/chat/user-prefs"}:                           {"spaceSortMode": "alpha"},
		// Group create validates the name before the authorization check.
		{"group.create", "/api/v1/groups"}: {"name": "bdm group create"},
	}
}

// callerPatternOverrides holds per-operation path parameters that name
// records the matrix fixture seeds itself: the matrix's own super-admin,
// for entry points told apart by whether the path names the caller's own
// record, and the user the user delete row removes.
func (m *bearerMatrixFixture) callerPatternOverrides() map[overrideKey]map[string]string {
	return map[overrideKey]map[string]string{
		{"user.self.update", "/api/v1/users/{id}"}:  {"id": m.adminID},
		{"user.admin.delete", "/api/v1/users/{id}"}: {"id": m.userDelete},
	}
}

// bearerMatrixQueryOverrides holds per-operation query strings, for
// entry points that share a pattern and are told apart by the query (the
// chat search variants).
func bearerMatrixQueryOverrides(f idFixtures) map[overrideKey]string {
	return map[overrideKey]string{
		{"chat.search.topic", "/api/v1/chat/search"}:   "q=chat&key=" + f.chatTopic,
		{"chat.search.project", "/api/v1/chat/search"}: "q=chat&projectId=" + f.project,
		{"chat.search.dm", "/api/v1/chat/search"}:      "q=chat&key=" + f.chat.dmAgent,
		// The starred-only history variant.
		{"chat.conversation.history.starred", "/api/v1/chat/conversations/{id}/messages"}: "starred=true",
		// Hub-level env and secret variants (user scope is the default).
		{"env.hub.list", "/api/v1/env"}:               "scope=hub",
		{"env.hub.read", "/api/v1/env/{key}"}:         "scope=hub",
		{"env.hub.write", "/api/v1/env/{key}"}:        "scope=hub",
		{"secret.hub.read", "/api/v1/secrets"}:        "scope=hub",
		{"secret.hub.read", "/api/v1/secrets/{key}"}:  "scope=hub",
		{"secret.hub.write", "/api/v1/secrets/{key}"}: "scope=hub",
	}
}

// bearerMatrixPatternOverrides holds path parameters for entry points the
// live method inventory does not probe (it covers HTTP routes only), so
// that the matrix addresses a seeded record.
func bearerMatrixPatternOverrides(f idFixtures) map[string]map[string]string {
	return map[string]map[string]string{
		"/api/v1/agents/{id}/pty": {"id": f.agent},
		// Streams of the fixture agent and project.
		"/api/v1/agents/{id}/cloud-logs/stream":                        {"id": f.agent},
		"/api/v1/projects/{projectId}/agents/{id}/cloud-logs/stream":   {"projectId": f.project, "id": f.agent},
		"/api/v1/agents/{id}/messages/stream":                          {"id": f.agent},
		"/api/v1/agents/{id}/message-logs/stream":                      {"id": f.agent},
		"/api/v1/projects/{projectId}/agents/{id}/message-logs/stream": {"projectId": f.project, "id": f.agent},
		"/api/v1/projects/{id}/message-logs/stream":                    {"id": f.project},
	}
}

// bearerMatrixEntry is one catalog entry point with a request surface.
type bearerMatrixEntry struct {
	Spec       authzop.OperationSpec
	EntryPoint authzop.EntryPoint
}

func (e bearerMatrixEntry) key() liveInventoryKey {
	return liveInventoryKey{OperationID: string(e.Spec.ID), Method: e.EntryPoint.Method, Pattern: e.EntryPoint.Pattern}
}
