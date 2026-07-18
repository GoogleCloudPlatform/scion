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

package wsclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTokenSource implements transportauth.TokenSource for testing.
type fakeTokenSource struct {
	token  string
	expiry time.Time
}

func (f *fakeTokenSource) Token() (string, error) {
	if f.token == "" {
		return "", fmt.Errorf("no token")
	}
	return f.token, nil
}
func (f *fakeTokenSource) SetToken(token string, expiry time.Time) {
	f.token = token
	f.expiry = expiry
}
func (f *fakeTokenSource) Expiry() time.Time { return f.expiry }

func TestConnect_WithTransportAuth_IAP(t *testing.T) {
	var receivedAuth, receivedProxy string
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		receivedProxy = r.Header.Get("Proxy-Authorization")
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	src := &fakeTokenSource{token: "oidc-transport-token"}
	client := NewPTYClient(PTYClientConfig{
		Endpoint:        srv.URL,
		Token:           "scion-user-token",
		Slug:            "test-agent",
		TransportSource: src,
		TransportMode:   transportauth.HeaderProxyAuthorization,
	})

	err := client.Connect(context.Background())
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	assert.Equal(t, "Bearer scion-user-token", receivedAuth, "scion token should be in Authorization")
	assert.Equal(t, "Bearer oidc-transport-token", receivedProxy, "OIDC token should be in Proxy-Authorization")
}

func TestConnect_WithTransportAuth_CloudRunInvoker(t *testing.T) {
	var receivedAuth, receivedServerless string
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		receivedServerless = r.Header.Get("X-Serverless-Authorization")
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	src := &fakeTokenSource{token: "oidc-transport-token"}
	client := NewPTYClient(PTYClientConfig{
		Endpoint:        srv.URL,
		Token:           "scion-user-token",
		Slug:            "test-agent",
		TransportSource: src,
		TransportMode:   transportauth.HeaderServerlessAuthorization,
	})

	err := client.Connect(context.Background())
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	assert.Equal(t, "Bearer scion-user-token", receivedAuth, "scion token in Authorization")
	assert.Equal(t, "Bearer oidc-transport-token", receivedServerless, "OIDC in X-Serverless-Authorization")
}

func TestConnect_WithoutTransportAuth(t *testing.T) {
	var receivedAuth, receivedProxy string
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		receivedProxy = r.Header.Get("Proxy-Authorization")
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	client := NewPTYClient(PTYClientConfig{
		Endpoint: srv.URL,
		Token:    "scion-user-token",
		Slug:     "test-agent",
	})

	err := client.Connect(context.Background())
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	assert.Equal(t, "Bearer scion-user-token", receivedAuth, "scion token should be in Authorization")
	assert.Empty(t, receivedProxy, "no Proxy-Authorization without transport auth")
}

func TestWithTransport_AttachOption(t *testing.T) {
	src := &fakeTokenSource{token: "test"}
	cfg := PTYClientConfig{}
	opt := WithTransport(src, transportauth.HeaderProxyAuthorization)
	opt(&cfg)

	assert.Equal(t, src, cfg.TransportSource)
	assert.Equal(t, transportauth.HeaderProxyAuthorization, cfg.TransportMode)
}
