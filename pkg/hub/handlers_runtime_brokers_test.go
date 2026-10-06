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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Regression tests for the authorization gates on runtime broker handlers
// (getRuntimeBroker, handleBrokerHeartbeat, getBrokerProjects).
//
// These tests cover the four identity scenarios per handler:
//   1. Broker self-access (matching BrokerID) → 200
//   2. No identity → 401
//   3. User with denied CheckAccess →
//        404 for the read handlers (getRuntimeBroker, getBrokerProjects),
//        matching getProject/getAgent elsewhere in this package: a caller
//        who may not read the resource must not be able to tell "exists but
//        denied" from "does not exist" by probing IDs.
//        403 for handleBrokerHeartbeat, a mutation, where that concern
//        doesn't apply.
//   4. Non-user, non-broker identity (agent) → 403 (unchanged; this gate
//      runs before the broker is even fetched, so it can't leak existence)
// ============================================================================

// brokerAuthFixture holds the test world for broker auth gate tests.
type brokerAuthFixture struct {
	srv          *Server
	store        store.Store
	broker       *store.RuntimeBroker
	brokerSecret []byte
	deniedUser   *store.User
}

// brokerAuthSetup creates a server with broker auth enabled, a runtime broker,
// and a non-admin user who has no access policies for the broker.
func brokerAuthSetup(t *testing.T) *brokerAuthFixture {
	t.Helper()

	// Use bypassAgentsServer which configures broker auth (HMAC).
	srv, s := bypassAgentsServer(t)
	ctx := context.Background()
	f := &brokerAuthFixture{srv: srv, store: s}

	// Create a runtime broker with HMAC secret.
	f.brokerSecret = []byte("broker-auth-test-secret-32bytes!")
	f.broker = &store.RuntimeBroker{
		ID:      uuid.New().String(),
		Name:    "auth-test-broker",
		Slug:    "auth-test-broker",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, f.broker))
	require.NoError(t, s.CreateBrokerSecret(ctx, &store.BrokerSecret{
		BrokerID:  f.broker.ID,
		SecretKey: f.brokerSecret,
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		Status:    store.BrokerSecretStatusActive,
	}))

	// Create a regular member user with no policies granting broker access.
	f.deniedUser = &store.User{
		ID:          tid("broker-auth-denied-user"),
		Email:       "denied@example.com",
		DisplayName: "Denied User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, f.deniedUser))

	return f
}

// asBrokerSelf sends an HMAC-signed request as the test broker.
func (f *brokerAuthFixture) asBrokerSelf(t *testing.T, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "broker-auth-nonce-" + uuid.New().String()
	req.Header.Set(HeaderBrokerID, f.broker.ID)
	req.Header.Set(HeaderTimestamp, timestamp)
	req.Header.Set(HeaderNonce, nonce)

	svc := f.srv.brokerAuthService
	require.NotNil(t, svc, "broker auth service must be configured")
	mac := hmac.New(sha256.New, f.brokerSecret)
	mac.Write(svc.buildCanonicalString(req, timestamp, nonce))
	req.Header.Set(HeaderSignature, base64.StdEncoding.EncodeToString(mac.Sum(nil)))

	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// asAgent sends a request carrying an agent JWT (non-user, non-broker identity).
func (f *brokerAuthFixture) asAgent(t *testing.T, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()

	// Create a project and agent so we can mint a valid agent token.
	ctx := context.Background()

	owner := &store.User{
		ID:          tid("broker-auth-agent-owner"),
		Email:       "agent-owner@example.com",
		DisplayName: "Agent Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	// Ignore error if already exists from a previous subtest.
	_ = f.store.CreateUser(ctx, owner)

	proj := &store.Project{
		ID:      tid("broker-auth-agent-proj"),
		Name:    "Agent Project",
		Slug:    "broker-auth-agent-proj",
		OwnerID: owner.ID,
	}
	_ = f.store.CreateProject(ctx, proj)

	agent := &store.Agent{
		ID:        tid("broker-auth-agent"),
		Slug:      "broker-auth-agent",
		Name:      "broker-auth-agent",
		ProjectID: proj.ID,
		Phase:     "running",
		CreatedBy: owner.ID,
		OwnerID:   owner.ID,
	}
	_ = f.store.CreateAgent(ctx, agent)

	// Mint an agent token.
	svc := f.srv.GetAgentTokenService()
	require.NotNil(t, svc)
	tok, err := svc.GenerateAgentToken(agent.ID, agent.ProjectID,
		[]AgentTokenScope{ScopeProjectRead}, nil)
	require.NoError(t, err)

	return doRequestWithAgentToken(t, f.srv, method, path, body, tok)
}

// TestBrokerAuthGates is the regression suite for the authorization gates on
// the three runtime broker handlers. Each handler is tested with 4 scenarios:
// broker-self (200), no-identity (401), denied-user (404 for the two read
// handlers, 403 for the heartbeat mutation), agent (403).
func TestBrokerAuthGates(t *testing.T) {
	type testCase struct {
		name string
		// wantStatus maps a handler name to its expected status for this
		// scenario. Every scenario used below applies uniformly across
		// handlers except "denied-user", which splits by read vs write.
		wantStatus map[string]int
		request    func(t *testing.T, f *brokerAuthFixture, handler string) *httptest.ResponseRecorder
	}

	readHandlers := []string{"getRuntimeBroker", "getBrokerProjects"}
	allHandlers := []string{"getRuntimeBroker", "handleBrokerHeartbeat", "getBrokerProjects"}

	// uniformStatus builds a wantStatus map assigning the same status to
	// every handler in allHandlers.
	uniformStatus := func(status int) map[string]int {
		m := make(map[string]int, len(allHandlers))
		for _, h := range allHandlers {
			m[h] = status
		}
		return m
	}

	scenarios := []testCase{
		{
			name:       "broker-self=200",
			wantStatus: uniformStatus(http.StatusOK),
			request: func(t *testing.T, f *brokerAuthFixture, handler string) *httptest.ResponseRecorder {
				t.Helper()
				switch handler {
				case "getRuntimeBroker":
					return f.asBrokerSelf(t, http.MethodGet,
						"/api/v1/runtime-brokers/"+f.broker.ID, nil)
				case "handleBrokerHeartbeat":
					return f.asBrokerSelf(t, http.MethodPost,
						"/api/v1/runtime-brokers/"+f.broker.ID+"/heartbeat",
						brokerHeartbeatRequest{
							Status: string(store.BrokerStatusOnline),
						})
				case "getBrokerProjects":
					return f.asBrokerSelf(t, http.MethodGet,
						"/api/v1/runtime-brokers/"+f.broker.ID+"/projects", nil)
				default:
					t.Fatalf("unknown handler: %s", handler)
					return nil
				}
			},
		},
		{
			name:       "no-identity=401",
			wantStatus: uniformStatus(http.StatusUnauthorized),
			request: func(t *testing.T, f *brokerAuthFixture, handler string) *httptest.ResponseRecorder {
				t.Helper()
				switch handler {
				case "getRuntimeBroker":
					return doRequestNoAuth(t, f.srv, http.MethodGet,
						"/api/v1/runtime-brokers/"+f.broker.ID, nil)
				case "handleBrokerHeartbeat":
					return doRequestNoAuth(t, f.srv, http.MethodPost,
						"/api/v1/runtime-brokers/"+f.broker.ID+"/heartbeat",
						brokerHeartbeatRequest{Status: string(store.BrokerStatusOnline)})
				case "getBrokerProjects":
					return doRequestNoAuth(t, f.srv, http.MethodGet,
						"/api/v1/runtime-brokers/"+f.broker.ID+"/projects", nil)
				default:
					t.Fatalf("unknown handler: %s", handler)
					return nil
				}
			},
		},
		{
			// The read handlers report a CheckAccess denial as 404, so it is
			// indistinguishable from the broker not existing; the heartbeat
			// mutation keeps reporting 403.
			name: "denied-user",
			wantStatus: func() map[string]int {
				m := uniformStatus(http.StatusForbidden)
				for _, h := range readHandlers {
					m[h] = http.StatusNotFound
				}
				return m
			}(),
			request: func(t *testing.T, f *brokerAuthFixture, handler string) *httptest.ResponseRecorder {
				t.Helper()
				switch handler {
				case "getRuntimeBroker":
					return doRequestAsUser(t, f.srv, f.deniedUser, http.MethodGet,
						"/api/v1/runtime-brokers/"+f.broker.ID, nil)
				case "handleBrokerHeartbeat":
					return doRequestAsUser(t, f.srv, f.deniedUser, http.MethodPost,
						"/api/v1/runtime-brokers/"+f.broker.ID+"/heartbeat",
						brokerHeartbeatRequest{Status: string(store.BrokerStatusOnline)})
				case "getBrokerProjects":
					return doRequestAsUser(t, f.srv, f.deniedUser, http.MethodGet,
						"/api/v1/runtime-brokers/"+f.broker.ID+"/projects", nil)
				default:
					t.Fatalf("unknown handler: %s", handler)
					return nil
				}
			},
		},
		{
			name:       "agent=403",
			wantStatus: uniformStatus(http.StatusForbidden),
			request: func(t *testing.T, f *brokerAuthFixture, handler string) *httptest.ResponseRecorder {
				t.Helper()
				switch handler {
				case "getRuntimeBroker":
					return f.asAgent(t, http.MethodGet,
						"/api/v1/runtime-brokers/"+f.broker.ID, nil)
				case "handleBrokerHeartbeat":
					return f.asAgent(t, http.MethodPost,
						"/api/v1/runtime-brokers/"+f.broker.ID+"/heartbeat",
						brokerHeartbeatRequest{Status: string(store.BrokerStatusOnline)})
				case "getBrokerProjects":
					return f.asAgent(t, http.MethodGet,
						"/api/v1/runtime-brokers/"+f.broker.ID+"/projects", nil)
				default:
					t.Fatalf("unknown handler: %s", handler)
					return nil
				}
			},
		},
	}

	handlers := []struct {
		name   string
		action Action
	}{
		{"getRuntimeBroker", ActionRead},
		{"handleBrokerHeartbeat", ActionUpdate},
		{"getBrokerProjects", ActionRead},
	}

	for _, h := range handlers {
		t.Run(h.name, func(t *testing.T) {
			for _, sc := range scenarios {
				t.Run(sc.name, func(t *testing.T) {
					f := brokerAuthSetup(t)
					rec := sc.request(t, f, h.name)
					want := sc.wantStatus[h.name]

					if want == http.StatusOK {
						// For the broker-self happy path, the auth gate must
						// pass — any non-401/403/404 proves the gate allowed it.
						assert.NotEqual(t, http.StatusUnauthorized, rec.Code,
							"broker self-access must not be rejected as 401; got: %s", rec.Body.String())
						assert.NotEqual(t, http.StatusForbidden, rec.Code,
							"broker self-access must not be rejected as 403; got: %s", rec.Body.String())
						assert.NotEqual(t, http.StatusNotFound, rec.Code,
							"broker self-access must not be rejected as 404; got: %s", rec.Body.String())
					} else {
						assert.Equal(t, want, rec.Code,
							"expected %d; got %d: %s", want, rec.Code, rec.Body.String())
					}
				})
			}
		})
	}
}

// TestBrokerAuthGates_DeniedReadMatchesNotFound proves that a denied
// broker.read is not just status-compatible with a nonexistent broker (that
// alone is what TestBrokerAuthGates checks), but genuinely indistinguishable
// on the wire: same status AND same JSON body, for both
// GET /runtime-brokers/{id} and GET /runtime-brokers/{id}/projects. Before
// getRuntimeBroker/getBrokerProjects's not-found branch used writeStoreErr
// instead of a bare writeErrorFromErr, a denied read returned
// {"message":"RuntimeBroker not found"} while a nonexistent ID returned the
// generic {"message":"Resource not found"} -- same 404 status, different
// body, so a caller could still tell "exists but denied" from "does not
// exist" by probing IDs.
func TestBrokerAuthGates_DeniedReadMatchesNotFound(t *testing.T) {
	scenarios := []struct {
		name   string
		suffix string
	}{
		{"getRuntimeBroker", ""},
		{"getBrokerProjects", "/projects"},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			f := brokerAuthSetup(t)

			deniedRec := doRequestAsUser(t, f.srv, f.deniedUser, http.MethodGet,
				"/api/v1/runtime-brokers/"+f.broker.ID+sc.suffix, nil)
			missingRec := doRequestAsUser(t, f.srv, f.deniedUser, http.MethodGet,
				"/api/v1/runtime-brokers/does-not-exist-xyz"+sc.suffix, nil)

			require.Equal(t, http.StatusNotFound, deniedRec.Code,
				"a denied read must be reported as 404: %s", deniedRec.Body.String())
			require.Equal(t, http.StatusNotFound, missingRec.Code,
				"a lookup against a nonexistent broker ID must be reported as 404: %s", missingRec.Body.String())
			assert.JSONEq(t, missingRec.Body.String(), deniedRec.Body.String(),
				"a denied read and a lookup against a nonexistent ID must return the identical body, "+
					"or the response still discloses that the broker exists")
		})
	}
}

// ============================================================================
// Broker heartbeat request decoding.
// ============================================================================

func TestBrokerHeartbeatRequest_UnmarshalJSON(t *testing.T) {
	data := `{"status":"online","projects":[{"projectId":"p1","agentCount":1}]}`
	var hb brokerHeartbeatRequest
	err := json.Unmarshal([]byte(data), &hb)
	require.NoError(t, err)
	assert.Equal(t, "online", hb.Status)
	require.Len(t, hb.Projects, 1)
	assert.Equal(t, "p1", hb.Projects[0].ProjectID)
}

func TestBrokerProjectHeartbeat_UnmarshalJSON(t *testing.T) {
	data := `{"projectId":"p1","agentCount":1}`
	var p brokerProjectHeartbeat
	err := json.Unmarshal([]byte(data), &p)
	require.NoError(t, err)
	assert.Equal(t, "p1", p.ProjectID)
}

func TestBrokerProjectHeartbeat_UnmarshalJSON_GroveIdKeyIgnored(t *testing.T) {
	// The removed "groveId" name must not populate ProjectID: only
	// "projectId" is decoded.
	data := `{"groveId":"p1","agentCount":1}`
	var p brokerProjectHeartbeat
	err := json.Unmarshal([]byte(data), &p)
	require.NoError(t, err)
	assert.Empty(t, p.ProjectID)
}

func TestBrokerHeartbeatRequest_UnmarshalJSON_GrovesKeyIgnored(t *testing.T) {
	// The removed "groves" name must not populate Projects: only "projects"
	// is decoded. The project entry uses the canonical "projectId" key so
	// this test isolates the outer "groves" decoder from the inner one.
	data := `{"status":"online","groves":[{"projectId":"p1","agentCount":1}]}`
	var hb brokerHeartbeatRequest
	err := json.Unmarshal([]byte(data), &hb)
	require.NoError(t, err)
	assert.Equal(t, "online", hb.Status)
	assert.Empty(t, hb.Projects)
}

// TestBrokerHeartbeat_GroveOnlyPayloadDoesNotRegisterProjects proves that a
// heartbeat body keyed by the removed outer "groves" name no longer updates
// any agent: brokerHeartbeatRequest decodes it into zero projects, so the
// per-project agent loop never runs. The project entry uses the canonical
// "projectId" key so this test isolates the outer "groves" decoder from the
// inner "groveId" decoder (covered separately by
// TestBrokerHeartbeat_ProjectEntryGroveIdFieldIgnored).
func TestBrokerHeartbeat_GroveOnlyPayloadDoesNotRegisterProjects(t *testing.T) {
	srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

	before := getAgentState(t, s, agentSlug, projectID)

	body := []byte(`{"status":"online","groves":[{"projectId":"` + projectID + `","agentCount":1,"agents":[{"slug":"` + agentSlug + `","status":"WORKING","phase":"stopped","activity":"crashed"}]}]}`)
	rec := doRequestRaw(t, srv, http.MethodPost,
		"/api/v1/runtime-brokers/"+brokerID+"/heartbeat", body, "application/json")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	after := getAgentState(t, s, agentSlug, projectID)
	assert.Equal(t, before.Phase, after.Phase, "a grove-only heartbeat must not change agent phase")
	assert.Equal(t, before.Activity, after.Activity, "a grove-only heartbeat must not change agent activity")
}

// TestBrokerHeartbeat_ProjectEntryGroveIdFieldIgnored proves the same for a
// project entry that uses the canonical outer "projects" key but the removed
// "groveId" name on the entry itself: it decodes to an empty ProjectID, so
// the agent lookup inside the per-project loop fails silently and no agent
// state changes.
func TestBrokerHeartbeat_ProjectEntryGroveIdFieldIgnored(t *testing.T) {
	srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

	before := getAgentState(t, s, agentSlug, projectID)

	body := []byte(`{"status":"online","projects":[{"groveId":"` + projectID + `","agentCount":1,"agents":[{"slug":"` + agentSlug + `","status":"WORKING","phase":"stopped","activity":"crashed"}]}]}`)
	rec := doRequestRaw(t, srv, http.MethodPost,
		"/api/v1/runtime-brokers/"+brokerID+"/heartbeat", body, "application/json")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	after := getAgentState(t, s, agentSlug, projectID)
	assert.Equal(t, before.Phase, after.Phase, "a project entry keyed by the removed groveId name must not change agent phase")
	assert.Equal(t, before.Activity, after.Activity, "a project entry keyed by the removed groveId name must not change agent activity")
}

// ============================================================================
// A plain hub member who registers and auto-provides a broker must be able
// to read the broker record and its provider list back immediately, through
// the same user-authenticated path `scion runtime-broker status` uses.
// ============================================================================
func TestBrokerAuthz_AutoProvideRegistration_StatusSeesProviderImmediately(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// A plain hub member — not a super-admin, not the broker's HMAC self —
	// standing in for the operator who ran `scion runtime-broker register
	// --auto-provide` and then `scion runtime-broker status`.
	operator := &store.User{
		ID:          tid("user-status-operator"),
		Email:       "status-operator@test.com",
		DisplayName: "Operator",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, operator))
	ensureHubMembership(ctx, s, operator.ID)

	// Phase 1: POST /api/v1/brokers — create the broker registration with
	// auto-provide enabled, exactly as `scion runtime-broker register
	// --auto-provide` does.
	createRec := doRequestAsUser(t, srv, operator, http.MethodPost, "/api/v1/brokers",
		CreateBrokerRegistrationRequest{
			Name:        "status-broker",
			AutoProvide: true,
		})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var createResp CreateBrokerRegistrationResponse
	require.NoError(t, json.NewDecoder(createRec.Body).Decode(&createResp))
	require.NotEmpty(t, createResp.BrokerID)
	require.NotEmpty(t, createResp.JoinToken)

	// Phase 2: POST /api/v1/brokers/join — unauthenticated, the join token is
	// the credential.
	joinRec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/brokers/join",
		BrokerJoinRequest{
			BrokerID:  createResp.BrokerID,
			JoinToken: createResp.JoinToken,
			Hostname:  "status-broker",
			Version:   "0.1.0",
		})
	require.Equal(t, http.StatusOK, joinRec.Code, joinRec.Body.String())

	// Link the broker to a project, mirroring the CLI's "If project is
	// linked, offer to add this broker as a provider" step that prints
	// "Broker added as provider to project 'X'".
	registerRec := doRequestAsUser(t, srv, operator, http.MethodPost, "/api/v1/projects/register",
		RegisterProjectRequest{
			Name:     "Global",
			BrokerID: createResp.BrokerID,
		})
	require.Equal(t, http.StatusOK, registerRec.Code, registerRec.Body.String())
	var registerResp RegisterProjectResponse
	require.NoError(t, json.NewDecoder(registerRec.Body).Decode(&registerResp))
	require.NotNil(t, registerResp.Project)

	// Read path: the same operator immediately runs `scion runtime-broker
	// status`, which fetches the broker record and its provider list as a
	// user (never as the broker's own HMAC identity).
	getRec := doRequestAsUser(t, srv, operator, http.MethodGet,
		"/api/v1/runtime-brokers/"+createResp.BrokerID, nil)
	assert.Equal(t, http.StatusOK, getRec.Code,
		"the broker's own registering user must be able to read it back; got: %s", getRec.Body.String())

	projectsRec := doRequestAsUser(t, srv, operator, http.MethodGet,
		"/api/v1/runtime-brokers/"+createResp.BrokerID+"/projects", nil)
	require.Equal(t, http.StatusOK, projectsRec.Code,
		"the broker's own registering user must be able to list its providers; got: %s", projectsRec.Body.String())

	var projectsResp ListBrokerProjectsResponse
	require.NoError(t, json.NewDecoder(projectsRec.Body).Decode(&projectsResp))
	require.Len(t, projectsResp.Projects, 1,
		"the just-linked project must show up immediately, not '(none)'")
	assert.Equal(t, registerResp.Project.ID, projectsResp.Projects[0].ProjectID)
}

// autoProvideBrokerWithProject registers an auto-provide broker as owner and
// links it to a new, owner-created project via the two-phase register flow
// (mirroring the CLI's `register --auto-provide` + project-link step). It
// returns the broker ID and the created project (with its real name and git
// remote, for cross-project-disclosure checks).
func autoProvideBrokerWithProject(t *testing.T, srv *Server, owner *store.User, brokerName, projectName, gitRemote string) (brokerID string, project *store.Project) {
	t.Helper()

	createRec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/brokers",
		CreateBrokerRegistrationRequest{Name: brokerName, AutoProvide: true})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var createResp CreateBrokerRegistrationResponse
	require.NoError(t, json.NewDecoder(createRec.Body).Decode(&createResp))

	joinRec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/brokers/join",
		BrokerJoinRequest{
			BrokerID:  createResp.BrokerID,
			JoinToken: createResp.JoinToken,
			Hostname:  brokerName,
			Version:   "0.1.0",
		})
	require.Equal(t, http.StatusOK, joinRec.Code, joinRec.Body.String())

	registerRec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/register",
		RegisterProjectRequest{
			Name:      projectName,
			GitRemote: gitRemote,
			BrokerID:  createResp.BrokerID,
		})
	require.Equal(t, http.StatusOK, registerRec.Code, registerRec.Body.String())
	var registerResp RegisterProjectResponse
	require.NoError(t, json.NewDecoder(registerRec.Body).Decode(&registerResp))
	require.NotNil(t, registerResp.Project)

	return createResp.BrokerID, registerResp.Project
}

// TestBrokerAuthz_GetBrokerProjects_HidesUnreadableProjects proves that
// broker.read must not double as project.read for every project an
// auto-provide broker happens to serve. A hub member with no access to
// "SecretProj" must not learn its name, slug, or git remote through GET
// /runtime-brokers/{id}/projects, even though they can read the broker
// record itself. The owner, who created both the broker and the project,
// must still see it — that immediate-visibility behavior must keep working
// under the filter.
func TestBrokerAuthz_GetBrokerProjects_HidesUnreadableProjects(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID:          tid("user-provider-filter-owner"),
		Email:       "provider-filter-owner@test.com",
		DisplayName: "Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	outsider := &store.User{
		ID:          tid("user-provider-filter-outsider"),
		Email:       "provider-filter-outsider@test.com",
		DisplayName: "Outsider",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, outsider))
	ensureHubMembership(ctx, s, outsider.ID) // hub member, but not a project member

	brokerID, project := autoProvideBrokerWithProject(t, srv, owner,
		"provider-filter-broker", "SecretProj", "https://github.com/acme/private-repo.git")

	// The owner must still see their own auto-provided project immediately
	// after registration — the filter must not regress that.
	ownerRec := doRequestAsUser(t, srv, owner, http.MethodGet,
		"/api/v1/runtime-brokers/"+brokerID+"/projects", nil)
	require.Equal(t, http.StatusOK, ownerRec.Code, ownerRec.Body.String())
	var ownerResp ListBrokerProjectsResponse
	require.NoError(t, json.NewDecoder(ownerRec.Body).Decode(&ownerResp))
	require.Len(t, ownerResp.Projects, 1, "the owner must still see their own project")
	assert.Equal(t, project.ID, ownerResp.Projects[0].ProjectID)
	assert.Equal(t, "SecretProj", ownerResp.Projects[0].ProjectName)

	// A hub member with no access to the project must get a 200 (broker.read
	// still allows reading the broker's provider list as a concept), but the
	// project itself — including its name and git remote — must not appear.
	outsiderRec := doRequestAsUser(t, srv, outsider, http.MethodGet,
		"/api/v1/runtime-brokers/"+brokerID+"/projects", nil)
	require.Equal(t, http.StatusOK, outsiderRec.Code, outsiderRec.Body.String())
	assert.NotContains(t, outsiderRec.Body.String(), "SecretProj",
		"an outsider must never see the project name through the broker's provider list")
	assert.NotContains(t, outsiderRec.Body.String(), "private-repo",
		"an outsider must never see the project's git remote through the broker's provider list")
	var outsiderResp ListBrokerProjectsResponse
	require.NoError(t, json.NewDecoder(outsiderRec.Body).Decode(&outsiderResp))
	assert.Empty(t, outsiderResp.Projects, "an outsider must not see a project they cannot read")
}

// TestBrokerAuthz_GetBrokerProjects_AdminSeesAll checks that the
// project-read filter goes through the normal authz path: a super-admin
// sees every project a broker serves through the ordinary project.read
// grant, including projects it never joined, the same way it would through
// any other project listing.
func TestBrokerAuthz_GetBrokerProjects_AdminSeesAll(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID:          tid("user-provider-filter-admin-owner"),
		Email:       "provider-filter-admin-owner@test.com",
		DisplayName: "Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	adminID := tid("user-provider-filter-admin")
	createTestUserWithRole(t, s, adminID, "provider-filter-admin@test.com", "admin", store.SystemRoleSuperAdmin)
	admin, err := s.GetUser(ctx, adminID)
	require.NoError(t, err)
	ensureHubMembership(ctx, s, admin.ID)

	brokerID, project := autoProvideBrokerWithProject(t, srv, owner,
		"provider-filter-admin-broker", "AdminVisibleProj", "https://github.com/acme/admin-repo.git")

	adminRec := doRequestAsUser(t, srv, admin, http.MethodGet,
		"/api/v1/runtime-brokers/"+brokerID+"/projects", nil)
	require.Equal(t, http.StatusOK, adminRec.Code, adminRec.Body.String())
	var adminResp ListBrokerProjectsResponse
	require.NoError(t, json.NewDecoder(adminRec.Body).Decode(&adminResp))
	require.Len(t, adminResp.Projects, 1, "a super-admin must keep the usual bypass and see every project")
	assert.Equal(t, project.ID, adminResp.Projects[0].ProjectID)
}

// getProjectErrStore wraps a store and forces GetProject to fail for one
// specific project ID with a caller-supplied error, leaving every other
// method (including GetProject for any other ID) untouched. Used to exercise
// getBrokerProjects' handling of a provider record whose project lookup
// fails, without needing a real deleted-row or connection-failure fixture.
type getProjectErrStore struct {
	store.Store
	projectID string
	err       error
}

func (g *getProjectErrStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	if id == g.projectID {
		return nil, g.err
	}
	return g.Store.GetProject(ctx, id)
}

// TestBrokerAuthz_GetBrokerProjects_ToleratesNotFoundProject proves that a
// provider record whose project has since been deleted (GetProject returning
// store.ErrNotFound — the row was removed but the provider record wasn't yet
// cleaned up) does not fail the whole request: getBrokerProjects must not
// treat that lookup failure as an error. The provider entry itself is still
// returned (to a caller who can read it) with no name or git remote, since
// only the enrichment step — not the entry — is skipped.
func TestBrokerAuthz_GetBrokerProjects_ToleratesNotFoundProject(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID:          tid("user-getproject-notfound-owner"),
		Email:       "getproject-notfound-owner@test.com",
		DisplayName: "Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	brokerID, project := autoProvideBrokerWithProject(t, srv, owner,
		"notfound-project-broker", "StaleProj", "https://github.com/acme/stale-repo.git")

	srv.store = &getProjectErrStore{Store: s, projectID: project.ID, err: store.ErrNotFound}
	defer func() { srv.store = s }()

	rec := doRequestAsUser(t, srv, owner, http.MethodGet,
		"/api/v1/runtime-brokers/"+brokerID+"/projects", nil)

	require.Equal(t, http.StatusOK, rec.Code,
		"a provider record whose project lookup returns not-found must not fail the whole request: %s", rec.Body.String())

	// The owner's project-owner role binding is scoped to the project ID
	// itself (created at registration), independent of the store.Project
	// record the read filter's authz check would otherwise attach as
	// OwnerID -- so the entry stays in the list even though its project
	// lookup failed. What the not-found skip actually buys is narrower:
	// GetProject's failure never reaches the client as an error (asserted
	// above via the 200), and the entry it could not enrich carries no name
	// or git remote, rather than a stale or fabricated one.
	var resp ListBrokerProjectsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Projects, 1)
	assert.Equal(t, project.ID, resp.Projects[0].ProjectID,
		"the provider entry itself must still be listed; only its project details are unavailable")
	assert.Empty(t, resp.Projects[0].ProjectName,
		"a project whose lookup returned not-found must not carry a stale or fabricated name")
	assert.Empty(t, resp.Projects[0].GitRemote,
		"a project whose lookup returned not-found must not carry a stale or fabricated git remote")
}

// TestBrokerAuthz_GetBrokerProjects_PropagatesOtherProjectErrors is the other
// side of the same fix: a GetProject failure that is NOT store.ErrNotFound
// (a genuine store error, e.g. a connection failure) must be reported as an
// error rather than silently treated the same as a not-found and dropped
// from the list.
func TestBrokerAuthz_GetBrokerProjects_PropagatesOtherProjectErrors(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID:          tid("user-getproject-error-owner"),
		Email:       "getproject-error-owner@test.com",
		DisplayName: "Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	brokerID, project := autoProvideBrokerWithProject(t, srv, owner,
		"getproject-error-broker", "ErrProj", "https://github.com/acme/err-repo.git")

	srv.store = &getProjectErrStore{Store: s, projectID: project.ID, err: errors.New("connection reset by peer")}
	defer func() { srv.store = s }()

	rec := doRequestAsUser(t, srv, owner, http.MethodGet,
		"/api/v1/runtime-brokers/"+brokerID+"/projects", nil)

	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"a genuine store error from GetProject must not be silently swallowed into a 200 with an incomplete list, and must map through writeErrorFromErr's default (unrecognized-error) branch: %s", rec.Body.String())
}

// TestBrokerHeartbeat_BackfillsRuntimeFromResolvedProfile verifies the
// narrow heartbeat-time companion to the display-time enrichment fix
// (ptone/scion#2262): once a heartbeat reports the agent's applied profile,
// the hub resolves it by name against the broker's advertised profiles and
// backfills agent.Runtime alongside the existing Profile backfill — without
// guessing from whichever profile happens to be listed first.
func TestBrokerHeartbeat_BackfillsRuntimeFromResolvedProfile(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     tid("hb-runtime-broker"),
		Name:   "HB Runtime Broker",
		Slug:   "hb-runtime-broker",
		Status: store.BrokerStatusOnline,
		Profiles: []store.BrokerProfile{
			{Name: "docker-default", Type: "docker", Available: true},
			{Name: "k8s-prod", Type: "kubernetes", Available: true},
		},
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	project := &store.Project{
		ID:      tid("hb-runtime-project"),
		Slug:    "hb-runtime-project",
		Name:    "HB Runtime Project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     broker.Status,
	}))

	agent := &store.Agent{
		ID:              tid("hb-runtime-agent"),
		Slug:            "hb-runtime-agent",
		Name:            "HB Runtime Agent",
		Template:        "default",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           "running",
		Activity:        "working",
		Labels:          map[string]string{},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	code := sendHeartbeat(t, srv, broker.ID, project.ID, brokerAgentHeartbeat{
		Slug:    agent.Slug,
		Status:  "WORKING",
		Phase:   "running",
		Profile: "k8s-prod",
	})
	require.Equal(t, http.StatusOK, code)

	updated := getAgentState(t, s, agent.Slug, project.ID)
	assert.Equal(t, "kubernetes", updated.Runtime,
		"Runtime should be backfilled from the agent's own resolved profile, not the broker's first profile")
	require.NotNil(t, updated.AppliedConfig)
	assert.Equal(t, "k8s-prod", updated.AppliedConfig.Profile)
}

// TestBrokerHeartbeat_UnresolvedProfileDoesNotBackfillRuntime is the
// unresolved-profile counterpart: if the heartbeat's reported profile name
// doesn't match any profile the broker advertises, Runtime must stay empty
// rather than falling back to the broker's docker profile.
func TestBrokerHeartbeat_UnresolvedProfileDoesNotBackfillRuntime(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     tid("hb-runtime-broker-unresolved"),
		Name:   "HB Runtime Broker Unresolved",
		Slug:   "hb-runtime-broker-unresolved",
		Status: store.BrokerStatusOnline,
		Profiles: []store.BrokerProfile{
			{Name: "docker-default", Type: "docker", Available: true},
			{Name: "k8s-prod", Type: "kubernetes", Available: true},
		},
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	project := &store.Project{
		ID:      tid("hb-runtime-project-unresolved"),
		Slug:    "hb-runtime-project-unresolved",
		Name:    "HB Runtime Project Unresolved",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     broker.Status,
	}))

	agent := &store.Agent{
		ID:              tid("hb-runtime-agent-unresolved"),
		Slug:            "hb-runtime-agent-unresolved",
		Name:            "HB Runtime Agent Unresolved",
		Template:        "default",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           "running",
		Activity:        "working",
		Labels:          map[string]string{},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	code := sendHeartbeat(t, srv, broker.ID, project.ID, brokerAgentHeartbeat{
		Slug:    agent.Slug,
		Status:  "WORKING",
		Phase:   "running",
		Profile: "some-other-profile",
	})
	require.Equal(t, http.StatusOK, code)

	updated := getAgentState(t, s, agent.Slug, project.ID)
	assert.Empty(t, updated.Runtime,
		"Runtime must stay empty when the reported profile can't be resolved against the broker's profiles, not fall back to docker")
}

// TestBrokerHeartbeat_NeverOverwritesExplicitRuntime verifies the heartbeat
// backfill never overwrites an already-set Runtime: an agent that already
// has a Runtime and an already-known applied profile is never overwritten,
// even when the broker's profile list would resolve the current profile to
// a different runtime type. The backfill only ever fills in a value that's
// missing.
func TestBrokerHeartbeat_NeverOverwritesExplicitRuntime(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     tid("hb-runtime-broker-no-overwrite"),
		Name:   "HB Runtime Broker No Overwrite",
		Slug:   "hb-runtime-broker-no-overwrite",
		Status: store.BrokerStatusOnline,
		Profiles: []store.BrokerProfile{
			{Name: "docker-default", Type: "docker", Available: true},
			{Name: "k8s-prod", Type: "kubernetes", Available: true},
		},
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	project := &store.Project{
		ID:      tid("hb-runtime-project-no-overwrite"),
		Slug:    "hb-runtime-project-no-overwrite",
		Name:    "HB Runtime Project No Overwrite",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     broker.Status,
	}))

	agent := &store.Agent{
		ID:              tid("hb-runtime-agent-no-overwrite"),
		Slug:            "hb-runtime-agent-no-overwrite",
		Name:            "HB Runtime Agent No Overwrite",
		Template:        "default",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           "running",
		Activity:        "working",
		Labels:          map[string]string{},
		Runtime:         "docker",
		AppliedConfig:   &store.AgentAppliedConfig{Profile: "k8s-prod"},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// The heartbeat reports the same profile the agent already has on
	// record, so this is not a first-time Profile backfill — Runtime must
	// stay untouched even though "k8s-prod" resolves to kubernetes.
	code := sendHeartbeat(t, srv, broker.ID, project.ID, brokerAgentHeartbeat{
		Slug:    agent.Slug,
		Status:  "WORKING",
		Phase:   "running",
		Profile: "k8s-prod",
	})
	require.Equal(t, http.StatusOK, code)

	updated := getAgentState(t, s, agent.Slug, project.ID)
	assert.Equal(t, "docker", updated.Runtime,
		"an explicitly-set Runtime must not be silently overwritten by the heartbeat backfill")
}

// TestBrokerHeartbeat_RuntimeNotOverwrittenWhenProfileFirstBackfilledSamePass
// proves the backfill is a pure fill-in, not a re-derivation: even when a
// heartbeat backfills AppliedConfig.Profile for the first time (it was
// previously unknown) in the same pass, an already-set Runtime is left
// alone, even though the newly-known profile would resolve to a different
// runtime type. The only writer of a non-empty Runtime elsewhere in the hub
// for a broker-hosted agent (the dispatch response path) sets Runtime from
// the broker's own AgentInfo for the agent it actually started (recording
// Profile too when AppliedConfig exists), so "Runtime set, Profile still
// unknown" is exactly the authoritative case this backfill must not touch.
func TestBrokerHeartbeat_RuntimeNotOverwrittenWhenProfileFirstBackfilledSamePass(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     tid("hb-runtime-broker-refresh"),
		Name:   "HB Runtime Broker Refresh",
		Slug:   "hb-runtime-broker-refresh",
		Status: store.BrokerStatusOnline,
		Profiles: []store.BrokerProfile{
			{Name: "docker-default", Type: "docker", Available: true},
			{Name: "k8s-prod", Type: "kubernetes", Available: true},
		},
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	project := &store.Project{
		ID:      tid("hb-runtime-project-refresh"),
		Slug:    "hb-runtime-project-refresh",
		Name:    "HB Runtime Project Refresh",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     broker.Status,
	}))

	// Runtime is already authoritatively set to "kubernetes" (e.g. from the
	// dispatch response path), and AppliedConfig is nil so Profile is still
	// unknown to the hub.
	agent := &store.Agent{
		ID:              tid("hb-runtime-agent-refresh"),
		Slug:            "hb-runtime-agent-refresh",
		Name:            "HB Runtime Agent Refresh",
		Template:        "default",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           "running",
		Activity:        "working",
		Labels:          map[string]string{},
		Runtime:         "kubernetes",
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// The heartbeat backfills Profile for the first time, and it resolves to
	// a *different* type (docker) than the Runtime already on record.
	code := sendHeartbeat(t, srv, broker.ID, project.ID, brokerAgentHeartbeat{
		Slug:    agent.Slug,
		Status:  "WORKING",
		Phase:   "running",
		Profile: "docker-default",
	})
	require.Equal(t, http.StatusOK, code)

	updated := getAgentState(t, s, agent.Slug, project.ID)
	require.NotNil(t, updated.AppliedConfig)
	assert.Equal(t, "docker-default", updated.AppliedConfig.Profile)
	assert.Equal(t, "kubernetes", updated.Runtime,
		"an already-set Runtime must be kept even when this same heartbeat pass first backfills a Profile that would resolve to a different type")
}

// countingBrokerLoadStore wraps a store.Store and counts calls to
// GetRuntimeBroker, optionally injecting an error, so a test can prove the
// heartbeat handler's lazily-loaded, memoised broker read (ptone/scion#2262,
// loadHeartbeatBroker) behaves as described: at most one read per heartbeat
// regardless of how many callers need it, no read at all when nothing needs
// it, and a failed read that the handler recovers from without crashing.
//
// getRuntimeBrokerErrBroker, when set alongside getRuntimeBrokerErr, is
// returned together with the error, simulating a store call that returns a
// (non-nil but unreliable) value in the same breath as an error. This proves
// a caller actually gates on the error rather than trusting whatever value
// came back whenever one happens to be present.
//
// updateRuntimeBrokerCalls counts broker row writes, so a test can prove the
// heartbeat handler writes the row only when the refreshed state changed.
type countingBrokerLoadStore struct {
	store.Store
	getRuntimeBrokerCalls     int
	getRuntimeBrokerErr       error
	getRuntimeBrokerErrBroker *store.RuntimeBroker
	updateRuntimeBrokerCalls  int
}

func (s *countingBrokerLoadStore) UpdateRuntimeBroker(ctx context.Context, broker *store.RuntimeBroker) error {
	s.updateRuntimeBrokerCalls++
	return s.Store.UpdateRuntimeBroker(ctx, broker)
}

func (s *countingBrokerLoadStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	s.getRuntimeBrokerCalls++
	if s.getRuntimeBrokerErr != nil {
		return s.getRuntimeBrokerErrBroker, s.getRuntimeBrokerErr
	}
	return s.Store.GetRuntimeBroker(ctx, id)
}

// TestBrokerHeartbeat_BrokerLoadedOnceForCapabilitiesAndBackfill proves the
// lazy, memoised broker load shared by the Capabilities refresh and the
// Runtime backfill (loadHeartbeatBroker) reads the broker row at most once
// per heartbeat, even when the Capabilities block and two agents needing a
// Runtime backfill all call it. Without the memoisation
// (heartbeatBrokerLoaded), this would be three separate reads.
func TestBrokerHeartbeat_BrokerLoadedOnceForCapabilitiesAndBackfill(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     tid("hb-load-once-broker"),
		Name:   "HB Load Once Broker",
		Slug:   "hb-load-once-broker",
		Status: store.BrokerStatusOnline,
		Profiles: []store.BrokerProfile{
			{Name: "k8s-prod", Type: "kubernetes", Available: true},
		},
		Capabilities: &store.BrokerCapabilities{Sync: true},
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	project := &store.Project{
		ID:      tid("hb-load-once-project"),
		Slug:    "hb-load-once-project",
		Name:    "HB Load Once Project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     broker.Status,
	}))

	agent1 := &store.Agent{
		ID:              tid("hb-load-once-agent-1"),
		Slug:            "hb-load-once-agent-1",
		Name:            "HB Load Once Agent 1",
		Template:        "default",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           "running",
		Activity:        "working",
		Labels:          map[string]string{},
	}
	require.NoError(t, s.CreateAgent(ctx, agent1))

	agent2 := &store.Agent{
		ID:              tid("hb-load-once-agent-2"),
		Slug:            "hb-load-once-agent-2",
		Name:            "HB Load Once Agent 2",
		Template:        "default",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           "running",
		Activity:        "working",
		Labels:          map[string]string{},
	}
	require.NoError(t, s.CreateAgent(ctx, agent2))

	counting := &countingBrokerLoadStore{Store: s}
	srv.store = counting
	defer func() { srv.store = s }()

	hb := brokerHeartbeatRequest{
		Status:       "online",
		Capabilities: &store.BrokerCapabilities{Sync: true, Attach: true},
		Projects: []brokerProjectHeartbeat{
			{
				ProjectID: project.ID,
				Agents: []brokerAgentHeartbeat{
					{Slug: agent1.Slug, Status: "WORKING", Phase: "running", Profile: "k8s-prod"},
					{Slug: agent2.Slug, Status: "WORKING", Phase: "running", Profile: "k8s-prod"},
				},
			},
		},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", hb)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.Equal(t, 1, counting.getRuntimeBrokerCalls,
		"the broker row backing both the Capabilities refresh and the Runtime backfill for two agents must be read exactly once per heartbeat")

	updated1 := getAgentState(t, s, agent1.Slug, project.ID)
	updated2 := getAgentState(t, s, agent2.Slug, project.ID)
	assert.Equal(t, "kubernetes", updated1.Runtime, "agent 1's Runtime should be backfilled")
	assert.Equal(t, "kubernetes", updated2.Runtime, "agent 2's Runtime should be backfilled")

	updatedBroker, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	require.NotNil(t, updatedBroker.Capabilities)
	assert.True(t, updatedBroker.Capabilities.Attach, "capabilities should be refreshed from the heartbeat")
}

// TestBrokerHeartbeat_NoBrokerLoadWhenNothingNeedsIt proves the broker row is
// never read when a heartbeat carries no Capabilities and every agent
// already has a Runtime: the common-case path the lazy load exists to avoid
// regressing back into an unconditional read on every heartbeat.
func TestBrokerHeartbeat_NoBrokerLoadWhenNothingNeedsIt(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     tid("hb-no-load-broker"),
		Name:   "HB No Load Broker",
		Slug:   "hb-no-load-broker",
		Status: store.BrokerStatusOnline,
		Profiles: []store.BrokerProfile{
			{Name: "k8s-prod", Type: "kubernetes", Available: true},
		},
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	project := &store.Project{
		ID:      tid("hb-no-load-project"),
		Slug:    "hb-no-load-project",
		Name:    "HB No Load Project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     broker.Status,
	}))

	agent := &store.Agent{
		ID:              tid("hb-no-load-agent"),
		Slug:            "hb-no-load-agent",
		Name:            "HB No Load Agent",
		Template:        "default",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           "running",
		Activity:        "working",
		Labels:          map[string]string{},
		Runtime:         "kubernetes",
		AppliedConfig:   &store.AgentAppliedConfig{Profile: "k8s-prod"},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	counting := &countingBrokerLoadStore{Store: s}
	srv.store = counting
	defer func() { srv.store = s }()

	code := sendHeartbeat(t, srv, broker.ID, project.ID, brokerAgentHeartbeat{
		Slug:    agent.Slug,
		Status:  "WORKING",
		Phase:   "running",
		Profile: "k8s-prod",
	})
	require.Equal(t, http.StatusOK, code)

	assert.Equal(t, 0, counting.getRuntimeBrokerCalls,
		"a heartbeat with no Capabilities and no agent needing a Runtime backfill must not read the broker row at all")
}

// TestBrokerHeartbeat_BrokerLoadErrorDoesNotCrashOrBlockStatusUpdate proves
// that a GetRuntimeBroker failure does not crash the handler: the
// Capabilities refresh logs and skips instead of dereferencing a nil broker,
// the Runtime backfill is skipped (Runtime stays empty), and the agent's
// status update — which does not depend on the
// broker row — is still applied.
func TestBrokerHeartbeat_BrokerLoadErrorDoesNotCrashOrBlockStatusUpdate(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     tid("hb-load-err-broker"),
		Name:   "HB Load Err Broker",
		Slug:   "hb-load-err-broker",
		Status: store.BrokerStatusOnline,
		Profiles: []store.BrokerProfile{
			{Name: "k8s-prod", Type: "kubernetes", Available: true},
		},
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	project := &store.Project{
		ID:      tid("hb-load-err-project"),
		Slug:    "hb-load-err-project",
		Name:    "HB Load Err Project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     broker.Status,
	}))

	agent := &store.Agent{
		ID:              tid("hb-load-err-agent"),
		Slug:            "hb-load-err-agent",
		Name:            "HB Load Err Agent",
		Template:        "default",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           "provisioning",
		Activity:        "starting",
		Labels:          map[string]string{},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	counting := &countingBrokerLoadStore{Store: s, getRuntimeBrokerErr: errors.New("connection reset by peer")}
	srv.store = counting
	defer func() { srv.store = s }()

	hb := brokerHeartbeatRequest{
		Status:       "online",
		Capabilities: &store.BrokerCapabilities{Sync: true},
		Projects: []brokerProjectHeartbeat{
			{
				ProjectID: project.ID,
				Agents: []brokerAgentHeartbeat{
					{Slug: agent.Slug, Status: "WORKING", Phase: "running", Activity: "working", Profile: "k8s-prod"},
				},
			},
		},
	}

	var rec *httptest.ResponseRecorder
	require.NotPanics(t, func() {
		rec = doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", hb)
	}, "a GetRuntimeBroker failure must not panic the heartbeat handler")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated := getAgentState(t, s, agent.Slug, project.ID)
	assert.Empty(t, updated.Runtime, "Runtime must stay empty when the broker load needed to resolve it fails")
	assert.Equal(t, "running", updated.Phase, "the agent's status update must still apply even though the broker load failed")
	assert.Equal(t, "working", updated.Activity)
}

// TestBrokerHeartbeat_BrokerLoadErrorSkipsBackfillEvenWithStaleBrokerValue
// isolates the Runtime-backfill branch's own error gate from the Capabilities
// branch: no Capabilities are sent, so this exercises only
// "if err != nil { skip }" at the backfill call site. The injected error
// comes back together with a non-nil, resolvable broker value, so a gate
// that ignored the error (rather than skipping whenever err != nil) would
// still produce a non-empty, wrong Runtime from that stale value.
func TestBrokerHeartbeat_BrokerLoadErrorSkipsBackfillEvenWithStaleBrokerValue(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     tid("hb-load-err-stale-broker"),
		Name:   "HB Load Err Stale Broker",
		Slug:   "hb-load-err-stale-broker",
		Status: store.BrokerStatusOnline,
		Profiles: []store.BrokerProfile{
			{Name: "k8s-prod", Type: "kubernetes", Available: true},
		},
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	project := &store.Project{
		ID:      tid("hb-load-err-stale-project"),
		Slug:    "hb-load-err-stale-project",
		Name:    "HB Load Err Stale Project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     broker.Status,
	}))

	agent := &store.Agent{
		ID:              tid("hb-load-err-stale-agent"),
		Slug:            "hb-load-err-stale-agent",
		Name:            "HB Load Err Stale Agent",
		Template:        "default",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           "running",
		Activity:        "working",
		Labels:          map[string]string{},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// A resolvable, single-type broker value is returned alongside the
	// error: if the backfill used it regardless of the error, Runtime would
	// come back "kubernetes" instead of staying empty.
	counting := &countingBrokerLoadStore{
		Store:               s,
		getRuntimeBrokerErr: errors.New("connection reset by peer"),
		getRuntimeBrokerErrBroker: &store.RuntimeBroker{
			ID: broker.ID,
			Profiles: []store.BrokerProfile{
				{Name: "k8s-prod", Type: "kubernetes", Available: true},
			},
		},
	}
	srv.store = counting
	defer func() { srv.store = s }()

	code := sendHeartbeat(t, srv, broker.ID, project.ID, brokerAgentHeartbeat{
		Slug:    agent.Slug,
		Status:  "WORKING",
		Phase:   "running",
		Profile: "k8s-prod",
	})
	require.Equal(t, http.StatusOK, code)

	updated := getAgentState(t, s, agent.Slug, project.ID)
	assert.Empty(t, updated.Runtime,
		"the backfill must skip on a GetRuntimeBroker error rather than resolving from whatever broker value came back with it")
}
