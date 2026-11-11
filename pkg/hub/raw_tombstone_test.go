//go:build !no_sqlite

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

// Retirement tests for raw keystroke delivery through message requests
// (raw_tombstone.go). Every message ingress that used to accept a raw field
// must reject it with 422 raw_input_removed, for both spellings and every
// value shape, before any side effect: no authorization side effect, no
// sender synthesis, no persistence, no event, no dispatch of any kind.
// Scheduled payload coverage lives in scheduled_payload_raw_tombstone_test.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const rawTombstoneSecret = "RAW-TOMBSTONE-SECRET-4K2Q"

// rawValueShapes is every value shape the tombstone must reject. The
// malformed shapes are not valid JSON; the probe reads the member name
// before its value, so they are still reported as raw.
var rawValueShapes = []struct {
	name  string
	value string
}{
	{"true", `true`},
	{"false", `false`},
	{"null", `null`},
	{"wrong type string", `"true"`},
	{"wrong type number", `1`},
	{"wrong type object", `{}`},
	{"malformed literal", `tru`},
	{"malformed missing", ``},
}

// rawSpelling describes where the retired member is placed in a request
// body: at the top level, or as the first member of a nested object.
type rawSpelling struct {
	name   string
	nested string // "" for top level
	key    string // member name spelling
}

// injectRawMember inserts "<key>":<value> into body, at the top level or as
// the first member of the nested object named nested. It works on bytes so
// a malformed value can be injected, which json.Marshal would refuse.
func injectRawMember(t *testing.T, body []byte, sp rawSpelling, value string) []byte {
	t.Helper()
	member := `"` + sp.key + `":` + value + `,`
	if sp.nested == "" {
		require.True(t, bytes.HasPrefix(body, []byte("{")), "body must be an object")
		return append([]byte("{"+member), body[1:]...)
	}
	anchor := []byte(`"` + sp.nested + `":{`)
	idx := bytes.Index(body, anchor)
	require.GreaterOrEqual(t, idx, 0, "body must contain %s", anchor)
	cut := idx + len(anchor)
	out := append([]byte{}, body[:cut]...)
	out = append(out, member...)
	return append(out, body[cut:]...)
}

func postRawTombstoneBody(t *testing.T, srv *Server, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func userBearerToken(t *testing.T, srv *Server, user *store.User) string {
	t.Helper()
	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
	)
	require.NoError(t, err)
	return token
}

// assertRawInputRemoved checks the full rejection shape: 422, the
// raw_input_removed code, guidance naming `scion keys`, an operation ID,
// the ingress tag, a replacement route, and no echo of request content.
func assertRawInputRemoved(t *testing.T, rec *httptest.ResponseRecorder, wantIngress rawIngress, wantReplacement string) {
	t.Helper()
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body: %s", rec.Body.String())
	env := decodeKeysError(t, rec.Body.Bytes())
	assert.Equal(t, messages.RawInputRemovedCode, env.Code)
	assert.Equal(t, messages.RawInputRemovedMessage, env.Message)
	assert.Contains(t, env.Message, "scion keys")
	opID, _ := env.Details["operation_id"].(string)
	assert.NotEmpty(t, opID, "raw_input_removed carries an operation ID")
	assert.Equal(t, string(wantIngress), env.Details["ingress"])
	assert.Equal(t, wantReplacement, env.Details["replacement"])
	assert.NotContains(t, rec.Body.String(), rawTombstoneSecret, "the rejection must not echo request content")
}

// messageRouteCase is one of the two public /message routes.
type messageRouteCase struct {
	name        string
	ingress     rawIngress
	path        func(agent *store.Agent) string
	replacement func(agent *store.Agent) string
}

var messageRouteCases = []messageRouteCase{
	{
		name:        "top-level",
		ingress:     rawIngressAgentMessage,
		path:        func(a *store.Agent) string { return "/api/v1/agents/" + a.ID + "/message" },
		replacement: func(*store.Agent) string { return rawInputRemovedReplacement },
	},
	{
		name:    "project-scoped",
		ingress: rawIngressProjectAgentMessage,
		path: func(a *store.Agent) string {
			return "/api/v1/projects/" + a.ProjectID + "/agents/" + a.Slug + "/message"
		},
		replacement: func(*store.Agent) string { return rawInputRemovedProjectReplacement },
	},
}

// messageBodyBases are ordinary message bodies the raw member is injected
// into: the legacy text shape and the structured shape, with and without
// plain/interrupt, so no other field changes the outcome.
var messageBodyBases = []struct {
	name string
	body string
}{
	{"legacy text", `{"message":"` + rawTombstoneSecret + `"}`},
	{"structured", `{"structured_message":{"msg":"` + rawTombstoneSecret + `","type":"instruction"}}`},
	{"structured plain interrupt", `{"interrupt":true,"plain":true,"structured_message":{"msg":"` + rawTombstoneSecret + `","type":"instruction","plain":true}}`},
}

// TestMessageRoutes_RetiredRawRejectedWithoutSideEffects covers both public
// /message routes for a human owner (who would otherwise be allowed to
// message the agent), every spelling and every value shape.
func TestMessageRoutes_RetiredRawRejectedWithoutSideEffects(t *testing.T) {
	spellings := []rawSpelling{
		{"top-level", "", "raw"},
		{"top-level upper", "", "RAW"},
		{"nested", "structured_message", "raw"},
		{"nested mixed case", "structured_message", "Raw"},
	}
	for _, route := range messageRouteCases {
		t.Run(route.name, func(t *testing.T) {
			f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
			token := userBearerToken(t, f.srv, f.owner)
			for _, base := range messageBodyBases {
				for _, sp := range spellings {
					if sp.nested != "" && !strings.Contains(base.body, `"`+sp.nested+`"`) {
						continue
					}
					for _, v := range rawValueShapes {
						name := base.name + "/" + sp.name + "/" + v.name
						t.Run(name, func(t *testing.T) {
							body := injectRawMember(t, []byte(base.body), sp, v.value)
							rec := postRawTombstoneBody(t, f.srv, route.path(f.agentInA), token, body)
							assertRawInputRemoved(t, rec, route.ingress, route.replacement(f.agentInA))
						})
					}
				}
			}
			assert.Equal(t, 0, d.callCount(), "no keys dispatch")
			assert.Empty(t, d.messages, "no message dispatch")
			assertNoKeysSideEffects(t, storeSpy, events)
		})
	}
}

// TestMessageRoutes_RetiredRawRejectedForAgentCaller covers an agent
// credential caller, same project and across projects, with and without the
// lifecycle scope: the tombstone answers before authorization, so every
// caller sees the same raw_input_removed outcome and nothing is delivered.
func TestMessageRoutes_RetiredRawRejectedForAgentCaller(t *testing.T) {
	for _, route := range messageRouteCases {
		t.Run(route.name, func(t *testing.T) {
			f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
			callers := map[string]string{
				"same project lifecycle":  f.agentToken(t, tid("raw-tomb-same-"+route.name), f.projectA.ID, ScopeAgentLifecycle),
				"same project no scope":   f.agentToken(t, tid("raw-tomb-noscope-"+route.name), f.projectA.ID),
				"cross project lifecycle": f.agentToken(t, tid("raw-tomb-cross-"+route.name), f.projectB.ID, ScopeAgentLifecycle),
			}
			body := []byte(`{"raw":true,"message":"` + rawTombstoneSecret + `"}`)
			for name, token := range callers {
				t.Run(name, func(t *testing.T) {
					rec := postRawTombstoneBody(t, f.srv, route.path(f.agentInA), token, body)
					assertRawInputRemoved(t, rec, route.ingress, route.replacement(f.agentInA))
				})
			}
			assert.Equal(t, 0, d.callCount())
			assert.Empty(t, d.messages)
			assertNoKeysSideEffects(t, storeSpy, events)
		})
	}
}

// TestMessageRoutes_RetiredRawResponseNamesNoTarget pins that the 422 never
// discloses the resolved target: on either route (the project-scoped one
// addressed by slug), the response carries no UUID other than its own
// operation ID, and in particular not the agent's ID.
func TestMessageRoutes_RetiredRawResponseNamesNoTarget(t *testing.T) {
	uuidRE := regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	for _, route := range messageRouteCases {
		t.Run(route.name, func(t *testing.T) {
			f, _, _, _ := newExecuteAgentKeysFixture(t)
			token := userBearerToken(t, f.srv, f.owner)
			// The project-scoped route addresses the agent by slug; the
			// top-level route takes the ID, which must not be echoed either.
			rec := postRawTombstoneBody(t, f.srv, route.path(f.agentInA), token, []byte(`{"message":"x","raw":true}`))
			assertRawInputRemoved(t, rec, route.ingress, route.replacement(f.agentInA))
			env := decodeKeysError(t, rec.Body.Bytes())
			opID, _ := env.Details["operation_id"].(string)
			body := strings.ReplaceAll(rec.Body.String(), opID, "")
			assert.NotContains(t, body, f.agentInA.ID, "the response must not name the resolved agent ID")
			assert.Empty(t, uuidRE.FindAllString(body, -1), "no UUID other than the operation ID: %s", rec.Body.String())
		})
	}
}

// TestMessageRoutes_RetiredRawAuditIsContentFree pins the audit record: one
// "agent keys audit" line tagged with the removal route and ingress, and no
// request content in it.
func TestMessageRoutes_RetiredRawAuditIsContentFree(t *testing.T) {
	for _, route := range messageRouteCases {
		t.Run(route.name, func(t *testing.T) {
			f, _, _, _ := newExecuteAgentKeysFixture(t)
			log := installSentinelLogCapture(t)
			token := userBearerToken(t, f.srv, f.owner)

			rec := postRawTombstoneBody(t, f.srv, route.path(f.agentInA), token,
				[]byte(`{"structured_message":{"msg":"`+rawTombstoneSecret+`","raw":true}}`))
			assertRawInputRemoved(t, rec, route.ingress, route.replacement(f.agentInA))

			out := log.String()
			assert.Contains(t, out, "agent keys audit")
			assert.Contains(t, out, "route="+string(agentKeysRouteRawRemoved))
			assert.Contains(t, out, "ingress="+string(route.ingress))
			assert.Contains(t, out, "target_agent_id="+f.agentInA.ID)
			assert.NotContains(t, out, rawTombstoneSecret, "audit and logs must not contain request content")
		})
	}
}

// TestMessageRoutes_PlainNormalInterruptUnaffected is the positive control:
// requests without a raw member, including ones whose text or metadata
// merely mention raw, are still delivered as ordinary messages, and the
// response is never the raw_input_removed envelope.
func TestMessageRoutes_PlainNormalInterruptUnaffected(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		wantPlain     bool
		wantInterrupt bool
	}{
		{"normal", `{"message":"hello there"}`, false, false},
		{"plain", `{"plain":true,"message":"hello there"}`, true, false},
		{"interrupt", `{"interrupt":true,"message":"hello there"}`, false, true},
		{"structured plain", `{"structured_message":{"plain":true,"msg":"hello there","type":"instruction"}}`, true, false},
		{"raw only as text", `{"message":"send raw:true please"}`, false, false},
		{"raw deeper than probed", `{"structured_message":{"msg":"hi","type":"instruction","metadata":{"raw":"x"}}}`, false, false},
	}
	for _, route := range messageRouteCases {
		t.Run(route.name, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					f := newAgentKeysRouteFixture(t)
					d := &brokerMockDispatcher{}
					f.srv.SetDispatcher(d)
					token := userBearerToken(t, f.srv, f.owner)

					rec := postRawTombstoneBody(t, f.srv, route.path(f.agentInA), token, []byte(tc.body))
					require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
					assert.NotContains(t, rec.Body.String(), messages.RawInputRemovedCode)

					d.mu.Lock()
					defer d.mu.Unlock()
					require.Len(t, d.messages, 1, "exactly one ordinary dispatch")
					assert.Equal(t, tc.wantInterrupt, d.messages[0].interrupt)
					require.NotNil(t, d.messages[0].structured)
					assert.Equal(t, tc.wantPlain, d.messages[0].structured.Plain)

					rows, err := f.store.ListMessages(context.Background(), store.MessageFilter{RecipientID: f.agentInA.ID}, store.ListOptions{Limit: 10})
					require.NoError(t, err)
					assert.Len(t, rows.Items, 1, "exactly one ordinary message row")
				})
			}
		})
	}
}

// TestMessageRoutes_PreAuthBodyCap pins the 2 MiB bound on the body read
// that happens before authorization: a larger body gets a generic 413 with
// no operation ID and no side effects.
func TestMessageRoutes_PreAuthBodyCap(t *testing.T) {
	oversized := `{"message":"` + strings.Repeat("x", rawTombstonePreAuthMaxBodyBytes) + `"}`
	for _, route := range messageRouteCases {
		t.Run(route.name, func(t *testing.T) {
			f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
			token := userBearerToken(t, f.srv, f.owner)
			rec := postRawTombstoneBody(t, f.srv, route.path(f.agentInA), token, []byte(oversized))
			require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, "body: %s", rec.Body.String())
			env := decodeKeysError(t, rec.Body.Bytes())
			assert.Equal(t, "payload_too_large", env.Code)
			assert.NotContains(t, env.Details, "operation_id")
			assert.Equal(t, 0, d.callCount())
			assert.Empty(t, d.messages)
			assertNoKeysSideEffects(t, storeSpy, events)
		})
	}
}

// failingReadCloser returns all of data and then a non-EOF error, the shape
// io.ReadAll sees when a connection breaks after a complete payload.
type failingReadCloser struct {
	data []byte
	err  error
}

func (r *failingReadCloser) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

func (r *failingReadCloser) Close() error { return nil }

// TestMessageRoutes_BrokenReadWithRawFailsClosed pins that a body read that
// errors after delivering raw-bearing bytes is still rejected, never
// delivered as an ordinary message.
func TestMessageRoutes_BrokenReadWithRawFailsClosed(t *testing.T) {
	for _, route := range messageRouteCases {
		t.Run(route.name, func(t *testing.T) {
			f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
			token := userBearerToken(t, f.srv, f.owner)

			req := httptest.NewRequest(http.MethodPost, route.path(f.agentInA), nil)
			req.Body = &failingReadCloser{
				data: []byte(`{"raw":true,"message":"` + rawTombstoneSecret + `"}`),
				err:  errors.New("simulated broken read"),
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			f.srv.Handler().ServeHTTP(rec, req)

			assertRawInputRemoved(t, rec, route.ingress, route.replacement(f.agentInA))
			assert.Equal(t, 0, d.callCount())
			assert.Empty(t, d.messages)
			assertNoKeysSideEffects(t, storeSpy, events)
		})
	}
}

// TestProjectBroadcast_RetiredRawRejectedWithoutSideEffects covers
// structured_message.raw (and the top-level spelling) on broadcast for a
// project member who may broadcast: nothing is stamped, fanned out,
// persisted or published. A raw-free control from the same caller is
// delivered, so the fixture is known to be live.
func TestProjectBroadcast_RetiredRawRejectedWithoutSideEffects(t *testing.T) {
	f := newBrokerInboundFixture(t)
	token := userBearerToken(t, f.srv, f.user)
	path := "/api/v1/projects/" + f.project.ID + "/broadcast"
	base := `{"structured_message":{"msg":"` + rawTombstoneSecret + `","type":"instruction"}}`
	usersBefore := countAllUsers(t, f.store)
	convsBefore := countAllConversations(t, f.store)

	spellings := []rawSpelling{
		{"nested", "structured_message", "raw"},
		{"nested upper", "structured_message", "RAW"},
		{"top-level", "", "raw"},
	}
	for _, sp := range spellings {
		for _, v := range rawValueShapes {
			t.Run(sp.name+"/"+v.name, func(t *testing.T) {
				body := injectRawMember(t, []byte(base), sp, v.value)
				rec := postRawTombstoneBody(t, f.srv, path, token, body)
				assertRawInputRemoved(t, rec, rawIngressBroadcast, rawInputRemovedReplacement)
			})
		}
	}
	t.Run("duplicate structured_message", func(t *testing.T) {
		body := []byte(`{"structured_message":{"msg":"` + rawTombstoneSecret + `"},"structured_message":{"raw":false}}`)
		rec := postRawTombstoneBody(t, f.srv, path, token, body)
		assertRawInputRemoved(t, rec, rawIngressBroadcast, rawInputRemovedReplacement)
	})
	f.assertNoInboundSideEffects(t, usersBefore, convsBefore)

	// Control: the identical raw-free broadcast is accepted and delivered.
	rec := postRawTombstoneBody(t, f.srv, path, token, []byte(base))
	require.Less(t, rec.Code, 300, "control: a raw-free broadcast must be accepted; body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), messages.RawInputRemovedCode)
}

// brokerInboundFixture is a project with a running agent and an active,
// resolvable user, so a raw-free inbound message from that user would be
// delivered. That makes the rejection meaningful: it is not just a request
// that would fail later anyway.
type brokerInboundFixture struct {
	srv        *Server
	store      store.Store
	user       *store.User
	project    *store.Project
	agent      *store.Agent
	dispatcher *recordingDispatcher
	events     *spyEventPublisher
}

func newBrokerInboundFixture(t *testing.T) *brokerInboundFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("user-raw-tomb-inbound"),
		Email:       "raw-tomb-inbound@example.com",
		DisplayName: "Raw Tombstone Inbound",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("proj-raw-tomb-inbound"),
		Slug:      "raw-tomb-inbound",
		Name:      "Raw Tombstone Inbound",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)
	msgAuthzAddProjectMember(t, s, user.ID, project.ID, project.Slug, store.GroupMemberRoleMember)

	agent := &store.Agent{
		ID:           tid("agent-raw-tomb-inbound"),
		Slug:         "raw-tomb-inbound-agent",
		Name:         "Raw Tombstone Inbound Agent",
		ProjectID:    project.ID,
		Phase:        "running",
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)
	events := &spyEventPublisher{}
	srv.SetEventPublisher(events)

	return &brokerInboundFixture{srv: srv, store: s, user: user, project: project, agent: agent, dispatcher: dispatcher, events: events}
}

func (f *brokerInboundFixture) post(t *testing.T, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))
	rec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rec, req)
	return rec
}

// assertNoInboundSideEffects proves the request reached none of the plugin
// inbound work: no dispatch, no persisted message, no conversation, no user
// message event, and no sender identity created.
func (f *brokerInboundFixture) assertNoInboundSideEffects(t *testing.T, usersBefore, convsBefore int) {
	t.Helper()
	ctx := context.Background()
	assert.Empty(t, f.dispatcher.getCalls(), "no dispatch")
	rows, err := f.store.ListMessages(ctx, store.MessageFilter{}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, rows.Items, "no persisted message")
	assert.Equal(t, convsBefore, countAllConversations(t, f.store), "no conversation created")
	assert.Equal(t, usersBefore, countAllUsers(t, f.store), "no sender identity synthesized")
	assert.Empty(t, f.events.getUserMessages(), "no user message event")
}

func countAllConversations(t *testing.T, s store.Store) int {
	t.Helper()
	convs, err := s.ListConversations(context.Background(), store.ConversationFilter{}, store.ListOptions{Limit: 1000, SkipTotalCount: true})
	require.NoError(t, err)
	return len(convs.Items)
}

func countAllUsers(t *testing.T, s store.Store) int {
	t.Helper()
	users, err := s.ListUsers(context.Background(), store.UserFilter{}, store.ListOptions{Limit: 1000})
	require.NoError(t, err)
	return len(users.Items)
}

// TestBrokerInbound_RetiredRawRejectedBeforeSenderSynthesis covers both
// plugin inbound routes. Each base body carries a claimed sender that would
// otherwise be resolved (or, on the routed endpoint, mapped), so the
// rejection is proven to run before sender synthesis, routing, conversation
// resolution and dispatch. A raw-free control with the same fixture is
// delivered, so the fixture is known to be live.
func TestBrokerInbound_RetiredRawRejectedBeforeSenderSynthesis(t *testing.T) {
	type inboundRoute struct {
		name    string
		path    string
		ingress rawIngress
		body    func(f *brokerInboundFixture) []byte
	}
	routes := []inboundRoute{
		{
			name:    "inbound",
			path:    "/api/v1/broker/inbound",
			ingress: rawIngressBrokerInbound,
			body: func(f *brokerInboundFixture) []byte {
				b, _ := json.Marshal(inboundMessageRequest{
					Topic: "scion.project." + f.project.ID + ".agent." + f.agent.Slug + ".messages",
					Message: &messages.StructuredMessage{
						Version:   messages.Version,
						Timestamp: time.Now().UTC().Format(time.RFC3339),
						Sender:    "user:" + f.user.Email,
						Recipient: "agent:" + f.agent.Slug,
						Msg:       rawTombstoneSecret,
						Type:      messages.TypeInstruction,
					},
				})
				return b
			},
		},
		{
			name:    "routed",
			path:    "/api/v1/broker/inbound/routed",
			ingress: rawIngressBrokerInboundRouted,
			body: func(f *brokerInboundFixture) []byte {
				b, _ := json.Marshal(routedInboundRequest{
					ProjectID:    f.project.ID,
					DefaultAgent: f.agent.Slug,
					Message: &messages.StructuredMessage{
						Version:   messages.Version,
						Timestamp: time.Now().UTC().Format(time.RFC3339),
						Sender:    "user:" + f.user.Email,
						Recipient: "agent:" + f.agent.Slug,
						Msg:       rawTombstoneSecret,
						Type:      messages.TypeInstruction,
					},
				})
				return b
			},
		},
	}
	spellings := []rawSpelling{
		{"nested", "message", "raw"},
		{"nested upper", "message", "RAW"},
		{"top-level", "", "raw"},
	}
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			f := newBrokerInboundFixture(t)
			usersBefore := countAllUsers(t, f.store)
			convsBefore := countAllConversations(t, f.store)
			base := route.body(f)

			for _, sp := range spellings {
				for _, v := range rawValueShapes {
					t.Run(sp.name+"/"+v.name, func(t *testing.T) {
						rec := f.post(t, route.path, injectRawMember(t, base, sp, v.value))
						assertRawInputRemoved(t, rec, route.ingress, rawInputRemovedReplacement)
					})
				}
			}
			t.Run("duplicate message", func(t *testing.T) {
				dup := append(append([]byte{}, bytes.TrimSuffix(base, []byte("}"))...), []byte(`,"message":{"raw":false}}`)...)
				rec := f.post(t, route.path, dup)
				assertRawInputRemoved(t, rec, route.ingress, rawInputRemovedReplacement)
			})
			t.Run("before topic or sender validation", func(t *testing.T) {
				// An invalid topic, missing project and non-user sender
				// would each be a 400 if reached; the tombstone answers first.
				rec := f.post(t, route.path, []byte(`{"topic":"not-a-topic","message":{"sender":"nobody","msg":"`+rawTombstoneSecret+`","raw":true}}`))
				assertRawInputRemoved(t, rec, route.ingress, rawInputRemovedReplacement)
			})
			f.assertNoInboundSideEffects(t, usersBefore, convsBefore)

			// Control: the identical raw-free body is delivered.
			rec := f.post(t, route.path, base)
			require.Equal(t, http.StatusOK, rec.Code, "control: a raw-free inbound message must be delivered; body: %s", rec.Body.String())
			assert.Len(t, f.dispatcher.getCalls(), 1, "control: exactly one dispatch")
		})
	}
}

// TestBrokerInbound_RetiredRawLogsAreContentFree pins that a rejected
// plugin inbound request writes no request content to any log.
func TestBrokerInbound_RetiredRawLogsAreContentFree(t *testing.T) {
	buf := captureSlog(t)
	f := newBrokerInboundFixture(t)
	requireLogCaptureLive(t, buf, serverConstructionLogLine)

	body := []byte(`{"topic":"scion.project.` + f.project.ID + `.agent.` + f.agent.Slug + `.messages","message":{"sender":"user:` + f.user.Email + `","msg":"` + rawTombstoneSecret + `","raw":true}}`)
	rec := f.post(t, "/api/v1/broker/inbound", body)
	assertRawInputRemoved(t, rec, rawIngressBrokerInbound, rawInputRemovedReplacement)
	requireLogCaptureLive(t, buf, "agent keys audit")
	assert.NotContains(t, buf.String(), rawTombstoneSecret)
}

// TestRetiredRawIngress_BodyCap pins the 2 MiB bound on the buffered body
// read of project broadcast and both plugin inbound routes: an oversized
// body is refused with a generic 413 (no operation ID, not a keys outcome)
// before any decode, sender synthesis or dispatch.
func TestRetiredRawIngress_BodyCap(t *testing.T) {
	oversizedMsg := strings.Repeat("x", rawTombstonePreAuthMaxBodyBytes)
	cases := []struct {
		name string
		do   func(f *brokerInboundFixture) *httptest.ResponseRecorder
	}{
		{"broadcast", func(f *brokerInboundFixture) *httptest.ResponseRecorder {
			token := userBearerToken(t, f.srv, f.user)
			return postRawTombstoneBody(t, f.srv, "/api/v1/projects/"+f.project.ID+"/broadcast", token,
				[]byte(`{"structured_message":{"msg":"`+oversizedMsg+`","type":"instruction"}}`))
		}},
		{"broker inbound", func(f *brokerInboundFixture) *httptest.ResponseRecorder {
			return f.post(t, "/api/v1/broker/inbound",
				[]byte(`{"topic":"scion.project.`+f.project.ID+`.agent.`+f.agent.Slug+`.messages","message":{"sender":"user:`+f.user.Email+`","msg":"`+oversizedMsg+`"}}`))
		}},
		{"broker inbound routed", func(f *brokerInboundFixture) *httptest.ResponseRecorder {
			return f.post(t, "/api/v1/broker/inbound/routed",
				[]byte(`{"project_id":"`+f.project.ID+`","default_agent":"`+f.agent.Slug+`","message":{"sender":"user:`+f.user.Email+`","msg":"`+oversizedMsg+`"}}`))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newBrokerInboundFixture(t)
			usersBefore := countAllUsers(t, f.store)
			convsBefore := countAllConversations(t, f.store)
			rec := tc.do(f)
			require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, "body: %.300s", rec.Body.String())
			env := decodeKeysError(t, rec.Body.Bytes())
			assert.Equal(t, "payload_too_large", env.Code)
			assert.NotContains(t, env.Details, "operation_id")
			f.assertNoInboundSideEffects(t, usersBefore, convsBefore)
		})
	}
}
