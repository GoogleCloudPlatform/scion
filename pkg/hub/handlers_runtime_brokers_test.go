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
