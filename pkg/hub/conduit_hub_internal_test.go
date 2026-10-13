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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// hubInternalTestPath is an operation-free path: a request that passes
// every transport check gets 404 there.
const hubInternalTestPath = relay.HubInternalPathPrefix + "no-such-op"

// failingReplayStore is a replay store that cannot be reached.
type failingReplayStore struct{}

func (failingReplayStore) CheckAndStore(context.Context, string, time.Duration) (bool, error) {
	return false, errors.New("store down")
}

// newExternalHub starts a hub in external relay mode with replay as its
// shared replay store (nil: the default) and returns its internal API.
func newExternalHub(t *testing.T, replay relayPeerReplayStore) *httptest.Server {
	t.Helper()
	f := &externalFixture{conduitFixture: newConduitFixture(t)}
	f.regStore = entadapter.NewConduitRegistryStore(enttest.NewClient(t))
	f.reg = registry.New(f.regStore, registry.Config{})
	opts := externalOptions(t, f)
	opts.replayStore = replay
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, f.srv.StartConduitExternal(ctx, opts))
	srv := httptest.NewServer(f.srv.ConduitInternalHandler())
	t.Cleanup(srv.Close)
	return srv
}

// signedHubRequest returns a relay-peer request for base+path with body,
// signed by auth for target (instance id) and generation.
func signedHubRequest(t *testing.T, auth relay.PeerAuth, base, path, target string, gen int64, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
	require.NoError(t, err)
	sum := sha256.Sum256(body)
	req.Header.Set(relay.HeaderBodySHA256, hex.EncodeToString(sum[:]))
	relay.SetPeerTarget(req, target, gen)
	require.NoError(t, auth.Sign(req))
	return req
}

// resend sends a copy of req (same headers and body) to base.
func resend(t *testing.T, req *http.Request, base string, body []byte) *http.Response {
	t.Helper()
	cp, err := http.NewRequest(req.Method, base+req.URL.Path, bytes.NewReader(body))
	require.NoError(t, err)
	cp.Header = req.Header.Clone()
	resp, err := http.DefaultClient.Do(cp)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func doRaw(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestHubInternal_TransportChecks (design §3.9, criterion 30): the hub
// internal API authenticates the relay, requires the hub target, checks the
// body digest and the method, and serves only its operations.
func TestHubInternal_TransportChecks(t *testing.T) {
	hub := newExternalHub(t, nil)
	auth := externalRelayAuth(t, "relay-0")
	body := []byte(`{}`)

	unsigned, err := http.NewRequest(http.MethodPost, hub.URL+hubInternalTestPath, bytes.NewReader(body))
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, doRaw(t, unsigned).StatusCode, "no relay-peer identity")

	sessionAuth, err := relay.NewHMACPeerAuthFromSecret(relay.HMACPeerAuthConfig{Secret: []byte("session-secret-0123456789abcdefghij"), SelfID: "relay-0"})
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized,
		doRaw(t, signedHubRequest(t, sessionAuth, hub.URL, hubInternalTestPath, relay.HubPeerTarget, 0, body)).StatusCode,
		"signed with a key not derived from the peer secret")

	wrong := doRaw(t, signedHubRequest(t, auth, hub.URL, hubInternalTestPath, "relay-1", 3, body))
	assert.Equal(t, http.StatusConflict, wrong.StatusCode, "signed for another receiver")
	assert.Equal(t, "relay_target_mismatch", wrong.Header.Get(relay.HeaderStaleReason))

	bad := signedHubRequest(t, auth, hub.URL, hubInternalTestPath, relay.HubPeerTarget, 0, body)
	bad.Body = http.NoBody
	bad.ContentLength = 0
	assert.Equal(t, http.StatusBadRequest, doRaw(t, bad).StatusCode, "body does not match its digest")

	get := signedHubRequest(t, auth, hub.URL, hubInternalTestPath, relay.HubPeerTarget, 0, nil)
	get.Method = http.MethodGet
	// Re-sign: the method is covered by the signature.
	require.NoError(t, auth.Sign(get))
	resp := doRaw(t, get)
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	assert.Equal(t, http.MethodPost, resp.Header.Get("Allow"))

	assert.Equal(t, http.StatusNotFound,
		doRaw(t, signedHubRequest(t, auth, hub.URL, hubInternalTestPath, relay.HubPeerTarget, 0, body)).StatusCode,
		"a request that passes every check reaches the operation table")
}

// TestHubInternal_ReplayAcrossHubReplicas (criterion 30): a relay-peer
// request replayed to the same hub, or to a second hub sharing the
// database, is refused with 401.
func TestHubInternal_ReplayAcrossHubReplicas(t *testing.T) {
	shared := NewNonceCacheStore(enttest.NewClient(t))
	hubA := newExternalHub(t, shared)
	hubB := newExternalHub(t, shared)
	auth := externalRelayAuth(t, "relay-0")
	body := []byte(`{}`)

	req := signedHubRequest(t, auth, hubA.URL, hubInternalTestPath, relay.HubPeerTarget, 0, body)
	assert.Equal(t, http.StatusNotFound, resend(t, req, hubA.URL, body).StatusCode, "first delivery passes the transport checks")
	assert.Equal(t, http.StatusUnauthorized, resend(t, req, hubB.URL, body).StatusCode, "replayed to another replica")
	assert.Equal(t, http.StatusUnauthorized, resend(t, req, hubA.URL, body).StatusCode, "replayed to the same replica")
}

// TestHubInternal_ReplayStoreUnavailable (criterion 30): with the shared
// replay store unreachable, the hub answers 503 and never accepts.
func TestHubInternal_ReplayStoreUnavailable(t *testing.T) {
	hub := newExternalHub(t, failingReplayStore{})
	auth := externalRelayAuth(t, "relay-0")
	resp := doRaw(t, signedHubRequest(t, auth, hub.URL, hubInternalTestPath, relay.HubPeerTarget, 0, []byte(`{}`)))
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// TestHubClient_OutcomeClasses: the relay's hub client reports a 4xx as a
// refusal and anything that leaves the outcome unknown as ErrHubUnavailable.
func TestHubClient_OutcomeClasses(t *testing.T) {
	hub := newExternalHub(t, failingReplayStore{})
	c := &relay.HubClient{BaseURL: hub.URL, Auth: externalRelayAuth(t, "relay-0")}
	err := c.Call(context.Background(), hubInternalTestPath, map[string]string{}, nil)
	assert.ErrorIs(t, err, relay.ErrHubUnavailable, "503")

	hub.Close()
	err = c.Call(context.Background(), hubInternalTestPath, map[string]string{}, nil)
	assert.ErrorIs(t, err, relay.ErrHubUnavailable, "unreachable")

	ok := newExternalHub(t, nil)
	c = &relay.HubClient{BaseURL: ok.URL, Auth: externalRelayAuth(t, "relay-0")}
	err = c.Call(context.Background(), hubInternalTestPath, map[string]string{}, nil)
	var refusal *relay.HubRefusal
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, http.StatusNotFound, refusal.Status)
}
