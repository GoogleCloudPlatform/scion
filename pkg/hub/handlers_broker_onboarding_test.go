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
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Broker registration, re-registration and secret rotation admit only user
// session and dev credentials (and, for the broker self-rotate arm, the
// broker's own HMAC credential for its own ID). Broker on-behalf-of
// credentials are not admitted for the user arms. Every request below runs
// through srv.Handler() with a real HMAC signature, so the credential kind
// is the one BrokerAuthMiddleware records.
// ============================================================================

// newOnboardingSigningBroker inserts a broker with an active HMAC secret
// and returns it with the secret key, for signing requests as that broker.
func newOnboardingSigningBroker(t *testing.T, s store.Store, name string) (*store.RuntimeBroker, []byte) {
	t.Helper()
	b := &store.RuntimeBroker{ID: tid("onboarding-signer-" + name), Name: "Onboarding Signer " + name, Slug: "onboarding-signer-" + name}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), b))
	return b, seedBrokerSecret(t, s, b.ID)
}

// doBrokerSignedRequest sends an HMAC-signed request as brokerID through
// the full handler chain. When onBehalfOfEmail is non-empty the request
// carries an X-Scion-On-Behalf-Of header naming that user.
func doBrokerSignedRequest(t *testing.T, srv *Server, brokerID string, key []byte, onBehalfOfEmail, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if onBehalfOfEmail != "" {
		req.Header.Set(HeaderOnBehalfOf, "user:"+onBehalfOfEmail)
	}
	require.NoError(t, srv.brokerAuthService.SignRequest(req, brokerID, key))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// assertBrokerSecretKey confirms the stored active secret for brokerID
// equals want.
func assertBrokerSecretKey(t *testing.T, s store.Store, brokerID string, want []byte, msg string) {
	t.Helper()
	stored, err := s.GetBrokerSecret(context.Background(), brokerID)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(want, stored.SecretKey), msg)
}

// TestBrokerOnboarding_BrokerOnBehalfOfNotAdmittedForReregistration:
// re-registration of another user's broker does not admit a broker
// on-behalf-of credential, whatever user the request names.
func TestBrokerOnboarding_BrokerOnBehalfOfNotAdmittedForReregistration(t *testing.T) {
	srv, s := testServer(t)
	admin := newSuperAdminUser(t, s, "onboarding-ck1-admin")
	owner := newHubMemberUser(t, s, "onboarding-ck1-owner")
	target := createReregistrationTestBroker(t, s, "onboarding-ck1-target", owner.ID)
	signer, key := newOnboardingSigningBroker(t, s, "ck1")

	rec := doBrokerSignedRequest(t, srv, signer.ID, key, admin.Email, http.MethodPost, "/api/v1/brokers",
		CreateBrokerRegistrationRequest{
			Name:        target.Name,
			AutoProvide: true,
			Labels:      map[string]string{"env": "requested"},
		})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"a broker on-behalf-of credential must not re-register a broker; got: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "joinToken", "a denied response must not carry a join token")
	assertBrokerUnchanged(t, s, target.ID)
}

// TestBrokerOnboarding_BrokerOnBehalfOfNotAdmittedForRotation: secret
// rotation of another broker does not admit a broker on-behalf-of
// credential, whatever user the request names.
func TestBrokerOnboarding_BrokerOnBehalfOfNotAdmittedForRotation(t *testing.T) {
	srv, s := testServer(t)
	admin := newSuperAdminUser(t, s, "onboarding-ck2-admin")
	owner := newHubMemberUser(t, s, "onboarding-ck2-owner")
	target := createReregistrationTestBroker(t, s, "onboarding-ck2-target", owner.ID)
	targetKey := seedBrokerSecret(t, s, target.ID)
	signer, key := newOnboardingSigningBroker(t, s, "ck2")

	rec := doBrokerSignedRequest(t, srv, signer.ID, key, admin.Email, http.MethodPost,
		"/api/v1/brokers/"+target.ID+"/rotate-secret", nil)

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"a broker on-behalf-of credential must not rotate another broker's secret; got: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "secretKey", "a denied response must not carry a secret")
	assertBrokerSecretKey(t, s, target.ID, targetKey, "the target broker's secret must not change on a denied rotation")
}

// TestBrokerOnboarding_BrokerOnBehalfOfNotAdmittedForRegistration: first
// registration does not admit a broker on-behalf-of credential.
func TestBrokerOnboarding_BrokerOnBehalfOfNotAdmittedForRegistration(t *testing.T) {
	srv, s := testServer(t)
	member := newHubMemberUser(t, s, "onboarding-ck3-member")
	signer, key := newOnboardingSigningBroker(t, s, "ck3")

	const brokerName = "onboarding-ck3-new-broker"
	rec := doBrokerSignedRequest(t, srv, signer.ID, key, member.Email, http.MethodPost, "/api/v1/brokers",
		CreateBrokerRegistrationRequest{Name: brokerName})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"a broker on-behalf-of credential must not register a broker; got: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "joinToken", "a denied response must not carry a join token")
	_, err := s.GetRuntimeBrokerByName(context.Background(), brokerName)
	assert.ErrorIs(t, err, store.ErrNotFound, "a denied registration must not create a broker record")
}

// TestBrokerOnboarding_BrokerOnBehalfOfNotAdmittedForEmbeddedRegistration:
// the embedded-broker path of POST /projects/register does not admit a
// broker on-behalf-of credential, and leaves no project or broker behind.
func TestBrokerOnboarding_BrokerOnBehalfOfNotAdmittedForEmbeddedRegistration(t *testing.T) {
	srv, s := testServer(t)
	member := newHubMemberUser(t, s, "onboarding-ck4-member")
	signer, key := newOnboardingSigningBroker(t, s, "ck4")

	const brokerName = "onboarding-ck4-embedded-broker"
	gitRemote := "https://github.com/onboarding-test/ck4"
	rec := doBrokerSignedRequest(t, srv, signer.ID, key, member.Email, http.MethodPost, "/api/v1/projects/register",
		RegisterProjectRequest{
			Name:      "onboarding-ck4-project",
			GitRemote: gitRemote,
			Broker:    &RegisterProjectBrokerInfo{Name: brokerName, Version: "1.0.0"},
		})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"a broker on-behalf-of credential must not register an embedded broker; got: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "secretKey", "a denied response must not carry a secret")

	projects, err := s.GetProjectsByGitRemote(context.Background(), util.NormalizeGitRemote(gitRemote))
	require.NoError(t, err)
	assert.Empty(t, projects, "a denied embedded-broker register must not persist a project")
	_, err = s.GetRuntimeBrokerByName(context.Background(), brokerName)
	assert.ErrorIs(t, err, store.ErrNotFound, "a denied embedded-broker register must not create a broker record")
}

// TestBrokerOnboarding_BrokerSelfRotateAllowed: a broker's own HMAC
// credential rotates its own secret.
func TestBrokerOnboarding_BrokerSelfRotateAllowed(t *testing.T) {
	srv, s := testServer(t)
	self, key := newOnboardingSigningBroker(t, s, "ck5")

	rec := doBrokerSignedRequest(t, srv, self.ID, key, "", http.MethodPost,
		"/api/v1/brokers/"+self.ID+"/rotate-secret", nil)

	require.Equal(t, http.StatusOK, rec.Code,
		"a broker must be able to rotate its own secret; got: %s", rec.Body.String())
	var resp RotateSecretResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.NotEmpty(t, resp.SecretKey, "a successful rotation returns the new secret")

	stored, err := s.GetBrokerSecret(context.Background(), self.ID)
	require.NoError(t, err)
	assert.False(t, bytes.Equal(key, stored.SecretKey), "the stored secret must change on a successful rotation")
}

// TestBrokerOnboarding_BrokerSelfRotateWithOnBehalfOfHeader: the broker
// self-rotate arm depends only on the HMAC-verified broker identity. A
// broker naming another user in X-Scion-On-Behalf-Of still rotates its own
// secret, and still cannot rotate another broker's secret, even one owned
// by the named user.
func TestBrokerOnboarding_BrokerSelfRotateWithOnBehalfOfHeader(t *testing.T) {
	srv, s := testServer(t)
	other := newHubMemberUser(t, s, "onboarding-ck6-other")
	target := createReregistrationTestBroker(t, s, "onboarding-ck6-target", other.ID)
	targetKey := seedBrokerSecret(t, s, target.ID)
	self, key := newOnboardingSigningBroker(t, s, "ck6")

	rec := doBrokerSignedRequest(t, srv, self.ID, key, other.Email, http.MethodPost,
		"/api/v1/brokers/"+self.ID+"/rotate-secret", nil)
	require.Equal(t, http.StatusOK, rec.Code,
		"a broker must be able to rotate its own secret with an on-behalf-of header; got: %s", rec.Body.String())
	var resp RotateSecretResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotEmpty(t, resp.SecretKey, "a successful rotation returns the new secret")
	newKey, err := base64.StdEncoding.DecodeString(resp.SecretKey)
	require.NoError(t, err)

	rec = doBrokerSignedRequest(t, srv, self.ID, newKey, other.Email, http.MethodPost,
		"/api/v1/brokers/"+target.ID+"/rotate-secret", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"a broker must not rotate another broker's secret, whatever user it names; got: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "secretKey", "a denied response must not carry a secret")
	assertBrokerSecretKey(t, s, target.ID, targetKey, "the target broker's secret must not change on a denied rotation")
}
