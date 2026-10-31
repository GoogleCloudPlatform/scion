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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Peers used by the transport policy tests. httptest.NewRequest gives a
// request the network peer 192.0.2.1, which the hub-broker transport policy
// refuses for broker registration (join-token minting), broker join, secret
// rotation, the control channel and the broker conduit session
// (config.ConfidentialArrival). Tests of those endpoints
// that are not about the transport send from tpLoopbackPeer, a client on
// the hub's host, through the *Loopback helpers below.
const (
	tpLoopbackPeer = "127.0.0.1:41000"
	tpNetworkPeer  = "192.0.2.1:41000" // httptest.NewRequest's default host
	tpProxyPeer    = "10.20.3.4:41000"
	tpProxyCIDR    = "10.20.0.0/16"
)

// newLoopbackJSONRequest is httptest.NewRequest with body marshalled as
// JSON (when not nil) and the peer tpLoopbackPeer.
func newLoopbackJSONRequest(t *testing.T, method, path string, body interface{}) *http.Request {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("failed to marshal body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = tpLoopbackPeer
	return req
}

func serveTest(srv *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// doRequestLoopback is doRequest (dev token) from tpLoopbackPeer.
func doRequestLoopback(t *testing.T, srv *Server, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	req := newLoopbackJSONRequest(t, method, path, body)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	return serveTest(srv, req)
}

// doRequestNoAuthLoopback is doRequestNoAuth from tpLoopbackPeer.
func doRequestNoAuthLoopback(t *testing.T, srv *Server, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return serveTest(srv, newLoopbackJSONRequest(t, method, path, body))
}

// doRequestWithTokenLoopback is doRequestWithToken from tpLoopbackPeer.
func doRequestWithTokenLoopback(t *testing.T, srv *Server, token, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	req := newLoopbackJSONRequest(t, method, path, body)
	req.Header.Set("Authorization", "Bearer "+token)
	return serveTest(srv, req)
}

// doRequestAsUserLoopback is doRequestAsUser from tpLoopbackPeer.
func doRequestAsUserLoopback(t *testing.T, srv *Server, user *store.User, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	token, _, _, err := srv.userTokenService.GenerateTokenPair(user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb)
	if err != nil {
		t.Fatal(err)
	}
	return doRequestWithTokenLoopback(t, srv, token, method, path, body)
}

// doBrokerSignedRequestLoopback is doBrokerSignedRequest from tpLoopbackPeer.
func doBrokerSignedRequestLoopback(t *testing.T, srv *Server, brokerID string, key []byte, onBehalfOfEmail, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	req := newLoopbackJSONRequest(t, method, path, body)
	if onBehalfOfEmail != "" {
		req.Header.Set(HeaderOnBehalfOf, "user:"+onBehalfOfEmail)
	}
	if err := (&apiclient.HMACAuth{BrokerID: brokerID, SecretKey: key}).ApplyAuth(req); err != nil {
		t.Fatal(err)
	}
	return serveTest(srv, req)
}
