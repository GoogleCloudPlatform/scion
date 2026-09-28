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
//   3. User with denied CheckAccess → 403
//   4. Non-user, non-broker identity (agent) → 403
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
// broker-self (200), no-identity (401), denied-user (403), agent (403).
func TestBrokerAuthGates(t *testing.T) {
	type testCase struct {
		name       string
		wantStatus int
		request    func(t *testing.T, f *brokerAuthFixture, handler string) *httptest.ResponseRecorder
	}

	scenarios := []testCase{
		{
			name:       "broker-self=200",
			wantStatus: http.StatusOK,
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
			wantStatus: http.StatusUnauthorized,
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
			name:       "denied-user=403",
			wantStatus: http.StatusForbidden,
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
			wantStatus: http.StatusForbidden,
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

					if sc.wantStatus == http.StatusOK {
						// For the broker-self happy path, the auth gate must
						// pass — any non-401/403 proves the gate allowed it.
						assert.NotEqual(t, http.StatusUnauthorized, rec.Code,
							"broker self-access must not be rejected as 401; got: %s", rec.Body.String())
						assert.NotEqual(t, http.StatusForbidden, rec.Code,
							"broker self-access must not be rejected as 403; got: %s", rec.Body.String())
					} else {
						assert.Equal(t, sc.wantStatus, rec.Code,
							"expected %d; got %d: %s", sc.wantStatus, rec.Code, rec.Body.String())
					}
				})
			}
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
