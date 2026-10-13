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

package brokerhost

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
)

// Instance-qualified routing (ptone/scion#3273).

// secretFor gives each instance its own HMAC secret.
func secretFor(key string) []byte {
	b := make([]byte, 32)
	copy(b, "secret-for-"+key)
	return b
}

// authFixture builds hosts whose instances enforce broker auth with their
// own secrets.
func authConfig(t *testing.T, f *hostFixture, instances ...config.V1RuntimeBrokerInstanceConfig) Config {
	t.Helper()
	cfg := f.config(t, instances...)
	cfg.BuildServer = func(ic InstanceContext) (*runtimebroker.Server, error) {
		f.built = append(f.built, ic.Instance.Key)
		sc := runtimebroker.DefaultServerConfig()
		sc.BrokerID = ic.Identity.RuntimeBrokerID
		sc.BrokerName = ic.Instance.Name
		sc.StateDir = t.TempDir()
		sc.HubEnabled = true
		sc.HubEndpoint = "http://127.0.0.1:1"
		sc.InMemoryCredentials = &brokercredentials.BrokerCredentials{
			BrokerID: ic.Identity.RuntimeBrokerID, HubEndpoint: "http://127.0.0.1:1",
			SecretKey: base64.StdEncoding.EncodeToString(secretFor(ic.Instance.Key)),
		}
		sc.FlatInstance = &runtimebroker.FlatInstanceConfig{Identity: ic.Identity, Instance: ic.Instance, HubInProcess: true}
		return runtimebroker.New(sc, ic.Manager, ic.Runtime), nil
	}
	return cfg
}

// signedGet signs a GET for path with brokerID's secret, as the Hub does for
// the row's endpoint.
func signedGet(t *testing.T, h http.Handler, path, brokerID string, secret []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if brokerID != "" {
		require.NoError(t, (&apiclient.HMACAuth{BrokerID: brokerID, SecretKey: secret}).ApplyAuth(req))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func preparedAuthHost(t *testing.T, refuse map[string]error, instances ...config.V1RuntimeBrokerInstanceConfig) (*Host, map[string]string) {
	t.Helper()
	f := newFixture(t)
	f.activator.refuse = refuse
	h, err := New(authConfig(t, f, instances...))
	require.NoError(t, err)
	require.NoError(t, h.Prepare(context.Background()))
	ids := map[string]string{}
	for _, st := range h.Status() {
		ids[st.Key] = st.RuntimeBrokerID
	}
	return h, ids
}

func TestHostRouting_PrefixedRouteReachesOnlyThatInstance(t *testing.T) {
	h, ids := preparedAuthHost(t, map[string]error{"docker-c": errors.New("refused")},
		dockerInstance("docker-a", "a"), dockerInstance("docker-b", "b"), dockerInstance("docker-c", "c"))
	handler := h.Handler()

	for _, key := range []string{"docker-a", "docker-b"} {
		id := ids[key]
		rec := signedGet(t, handler, InstancePrefix(id)+"/api/v1/info", id, secretFor(key))
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", key, rec.Body.String())
		assert.Contains(t, rec.Body.String(), id, "served by %s", key)
		for other, otherID := range ids {
			if other != key && otherID != "" {
				assert.NotContains(t, rec.Body.String(), otherID)
			}
		}
	}

	// A refused instance's ID and an unknown ID are 404: never served
	// elsewhere.
	assert.Equal(t, http.StatusNotFound, signedGet(t, handler, InstancePrefix(ids["docker-c"])+"/api/v1/info", ids["docker-c"], secretFor("docker-c")).Code)
	assert.Equal(t, http.StatusNotFound, signedGet(t, handler, InstancePrefix("unknown-id")+"/api/v1/info", "unknown-id", secretFor("x")).Code)
	assert.Equal(t, http.StatusNotFound, signedGet(t, handler, InstancePathPrefix, "", nil).Code)
	// The root is not an instance route when several are configured.
	assert.Equal(t, http.StatusNotFound, signedGet(t, handler, "/api/v1/info", ids["docker-a"], secretFor("docker-a")).Code)
}

func TestHostRouting_MismatchedSignedIdentityRefused(t *testing.T) {
	h, ids := preparedAuthHost(t, nil, dockerInstance("docker-a", "a"), dockerInstance("docker-b", "b"))
	handler := h.Handler()

	// B's identity (and secret) on A's route: refused before any instance.
	rec := signedGet(t, handler, InstancePrefix(ids["docker-a"])+"/api/v1/info", ids["docker-b"], secretFor("docker-b"))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	// A's identity with B's secret: A's own signature check refuses it.
	rec = signedGet(t, handler, InstancePrefix(ids["docker-a"])+"/api/v1/info", ids["docker-a"], secretFor("docker-b"))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	// Unsigned: refused by the instance's strict broker auth.
	rec = signedGet(t, handler, InstancePrefix(ids["docker-a"])+"/api/v1/info", "", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestHostRouting_SignatureCoversPrefixedPath: the Hub signs the full path
// of the row's endpoint; a signature over the unprefixed path does not
// authenticate a prefixed request.
func TestHostRouting_SignatureCoversPrefixedPath(t *testing.T) {
	h, ids := preparedAuthHost(t, nil, dockerInstance("docker-a", "a"), dockerInstance("docker-b", "b"))
	handler := h.Handler()
	id := ids["docker-a"]

	req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	require.NoError(t, (&apiclient.HMACAuth{BrokerID: id, SecretKey: secretFor("docker-a")}).ApplyAuth(req))
	req.URL.Path = InstancePrefix(id) + "/api/v1/info"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestHostRouting_PrefixedHealthUnauthenticated(t *testing.T) {
	h, ids := preparedAuthHost(t, nil, dockerInstance("docker-a", "a"), dockerInstance("docker-b", "b"))
	handler := h.Handler()

	rec := signedGet(t, handler, InstancePrefix(ids["docker-a"])+"/healthz", "", nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusOK, signedGet(t, handler, "/healthz", "", nil).Code, "host liveness")
}

func TestHostRouting_SingletonRootRouteBounded(t *testing.T) {
	h, ids := preparedAuthHost(t, nil, dockerInstance("docker-a", "a"))
	handler := h.Handler()
	id := ids["docker-a"]

	// One configured instance: the root route serves it (P1 compatibility)
	// and so does its prefixed route.
	assert.Equal(t, http.StatusOK, signedGet(t, handler, "/api/v1/info", id, secretFor("docker-a")).Code)
	assert.Equal(t, http.StatusOK, signedGet(t, handler, InstancePrefix(id)+"/api/v1/info", id, secretFor("docker-a")).Code)

	// Several configured with only one active: no root instance route.
	multi, multiIDs := preparedAuthHost(t, map[string]error{"docker-b": errors.New("refused")}, dockerInstance("docker-a", "a"), dockerInstance("docker-b", "b"))
	mh := multi.Handler()
	assert.Equal(t, http.StatusNotFound, signedGet(t, mh, "/api/v1/info", multiIDs["docker-a"], secretFor("docker-a")).Code)
	assert.Equal(t, http.StatusOK, signedGet(t, mh, InstancePrefix(multiIDs["docker-a"])+"/api/v1/info", multiIDs["docker-a"], secretFor("docker-a")).Code)
}

// TestHostRouting_StaleSingletonAliasIdentityRefused: after the configured
// singleton changes from A to B, a caller still using A's identity on the
// root alias never executes on B.
func TestHostRouting_StaleSingletonAliasIdentityRefused(t *testing.T) {
	h, ids := preparedAuthHost(t, nil, dockerInstance("docker-b", "b"))
	handler := h.Handler()

	rec := signedGet(t, handler, "/api/v1/info", "old-instance-a-id", secretFor("docker-a"))
	assert.Equal(t, http.StatusNotFound, rec.Code, "another broker's identity on the root alias")
	assert.Equal(t, http.StatusOK, signedGet(t, handler, "/api/v1/info", ids["docker-b"], secretFor("docker-b")).Code)
	// Missing authentication on the matching route keeps the auth answer.
	assert.Equal(t, http.StatusUnauthorized, signedGet(t, handler, "/api/v1/info", "", nil).Code)
}

// TestHostRouting_PathVariantsNeverSelectAnotherInstance: non-canonical
// paths under the namespace are 404, never cleaned or redirected.
func TestHostRouting_PathVariantsNeverSelectAnotherInstance(t *testing.T) {
	h, ids := preparedAuthHost(t, nil, dockerInstance("docker-a", "a"), dockerInstance("docker-b", "b"))
	handler := h.Handler()
	a, b := ids["docker-a"], ids["docker-b"]

	for _, raw := range []string{
		"/instances/" + a + "/../" + b + "/api/v1/info",
		"/instances/" + a + "/./api/v1/info",
		"/instances//" + a + "/api/v1/info",
		"/instances/" + a + "//api/v1/info",
		"/instances/" + a + "%2F..%2F" + b + "/api/v1/info",
		"/instances/" + a + "/api/%2E%2E/v1/info",
		"/instances%2F" + a + "/api/v1/info",
		"/instances/" + fmt.Sprintf("%%%02X", a[0]) + a[1:] + "/api/v1/info",
		"/instances/" + strings.ToUpper(a) + "/api/v1/info",
		"/instances/" + a + "x/api/v1/info",
		"/instances",
	} {
		t.Run(raw, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://broker"+raw, nil)
			require.NoError(t, (&apiclient.HMACAuth{BrokerID: a, SecretKey: secretFor("docker-a")}).ApplyAuth(req))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusNotFound, rec.Code)
			assert.Empty(t, rec.Header().Get("Location"), "no redirect")
		})
	}

	// A legitimate suffix and query survive the strip.
	rec := signedGet(t, handler, InstancePrefix(a)+"/api/v1/agents?projectId=p1", a, secretFor("docker-a"))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Escaped data in the route suffix is the instance's own route contract:
	// the request reaches the instance and gets the instance's own JSON
	// answer (as over the root route), not the host's plain 404.
	for _, suffix := range []string{"/api/v1/agents/a%20b/logs", "/api/v1/agents/a%2Fb/logs", "/api/v1/agents/%E2%9C%93/logs"} {
		t.Run("escaped suffix "+suffix, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://broker"+InstancePrefix(a)+suffix, nil)
			require.NoError(t, (&apiclient.HMACAuth{BrokerID: a, SecretKey: secretFor("docker-a")}).ApplyAuth(req))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assert.Contains(t, rec.Body.String(), `"error":{"code":`, "served by the instance: %d %s", rec.Code, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "404 page not found", "not refused by the host")
		})
	}
}

// TestHostRouting_SingleAuthenticationPassOverFullPath: the signature is
// made over the full prefixed path and verified once; a second check over
// the stripped path would refuse this request.
func TestHostRouting_SingleAuthenticationPassOverFullPath(t *testing.T) {
	h, ids := preparedAuthHost(t, nil, dockerInstance("docker-a", "a"), dockerInstance("docker-b", "b"))
	id := ids["docker-a"]
	rec := signedGet(t, h.Handler(), InstancePrefix(id)+"/api/v1/info", id, secretFor("docker-a"))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestHostRouting_EscapedSuffixSameAnswerAsSingletonRoot: on a singleton
// host, a route suffix carrying escaped data gets exactly the answer the
// root alias gives for the same suffix (status and body), so the prefixed
// route keeps the instance's existing contract.
func TestHostRouting_EscapedSuffixSameAnswerAsSingletonRoot(t *testing.T) {
	h, ids := preparedAuthHost(t, nil, dockerInstance("docker-a", "a"))
	handler := h.Handler()
	a := ids["docker-a"]
	serve := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://broker"+path, nil)
		require.NoError(t, (&apiclient.HMACAuth{BrokerID: a, SecretKey: secretFor("docker-a")}).ApplyAuth(req))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	for _, suffix := range []string{"/api/v1/agents/a%20b/logs", "/api/v1/agents/a%2Fb/logs", "/api/v1/agents/%E2%9C%93/logs", "/api/v1/agents?projectId=p%2F1"} {
		t.Run(suffix, func(t *testing.T) {
			root, prefixed := serve(suffix), serve(InstancePrefix(a)+suffix)
			assert.Equal(t, root.Code, prefixed.Code, "root %s / prefixed %s", root.Body.String(), prefixed.Body.String())
			assert.Equal(t, root.Body.String(), prefixed.Body.String())
		})
	}
}

// TestHostRouting_DecodedDotSegmentRefused: a suffix whose decoded and
// cleaned form would be a valid route (here /api/v1/info) is refused by the
// host, not cleaned and served.
func TestHostRouting_DecodedDotSegmentRefused(t *testing.T) {
	h, ids := preparedAuthHost(t, nil, dockerInstance("docker-a", "a"), dockerInstance("docker-b", "b"))
	handler := h.Handler()
	a := ids["docker-a"]
	for _, suffix := range []string{"/api/v1/agents/%2E%2E/info", "/api/v1/agents/%2e%2e/info", "/api/v1/%2E/info"} {
		t.Run(suffix, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://broker"+InstancePrefix(a)+suffix, nil)
			require.NoError(t, (&apiclient.HMACAuth{BrokerID: a, SecretKey: secretFor("docker-a")}).ApplyAuth(req))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
			assert.Empty(t, rec.Header().Get("Location"), "no redirect")
		})
	}
}
