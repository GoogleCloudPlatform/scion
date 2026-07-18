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

package runtimebroker

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// fakeTokenSource returns a configurable sequence of tokens for testing.
type fakeTokenSource struct {
	tokens  []string
	expiry  time.Time
	callIdx atomic.Int64
}

func newFakeTokenSource(tokens ...string) *fakeTokenSource {
	return &fakeTokenSource{
		tokens: tokens,
		expiry: time.Now().Add(time.Hour),
	}
}

func (f *fakeTokenSource) Token() (string, error) {
	idx := f.callIdx.Add(1) - 1
	if int(idx) < len(f.tokens) {
		return f.tokens[idx], nil
	}
	return f.tokens[len(f.tokens)-1], nil
}

func (f *fakeTokenSource) SetToken(token string, expiry time.Time) {
	f.expiry = expiry
}

func (f *fakeTokenSource) Expiry() time.Time {
	return f.expiry
}

func (f *fakeTokenSource) CallCount() int64 {
	return f.callIdx.Load()
}

func TestBuildAuthHeaders_WithTransportAuth(t *testing.T) {
	cc := &ControlChannelClient{
		config: ControlChannelConfig{
			HubEndpoint:     "https://hub.example.com",
			BrokerID:        "broker-1",
			SecretKey:       []byte("test-secret-key-for-hmac-signing"),
			TransportSource: newFakeTokenSource("oidc-token-1"),
			TransportMode:   transportauth.HeaderProxyAuthorization,
		},
	}

	headers, err := cc.buildAuthHeaders()
	if err != nil {
		t.Fatalf("buildAuthHeaders failed: %v", err)
	}

	// HMAC headers should be present (X-Scion-* headers, not Authorization)
	if bid := headers.Get("X-Scion-Broker-ID"); bid != "broker-1" {
		t.Errorf("expected X-Scion-Broker-ID=%q, got %q", "broker-1", bid)
	}
	if sig := headers.Get("X-Scion-Signature"); sig == "" {
		t.Error("expected X-Scion-Signature header")
	}

	// Transport OIDC token should be in Proxy-Authorization
	if pa := headers.Get("Proxy-Authorization"); pa != "Bearer oidc-token-1" {
		t.Errorf("Proxy-Authorization: expected %q, got %q", "Bearer oidc-token-1", pa)
	}
}

func TestBuildAuthHeaders_WithoutTransportAuth(t *testing.T) {
	cc := &ControlChannelClient{
		config: ControlChannelConfig{
			HubEndpoint: "https://hub.example.com",
			BrokerID:    "broker-1",
			SecretKey:   []byte("test-secret-key-for-hmac-signing"),
		},
	}

	headers, err := cc.buildAuthHeaders()
	if err != nil {
		t.Fatalf("buildAuthHeaders failed: %v", err)
	}

	// HMAC headers should be present
	if bid := headers.Get("X-Scion-Broker-ID"); bid != "broker-1" {
		t.Errorf("expected X-Scion-Broker-ID=%q, got %q", "broker-1", bid)
	}

	// No Proxy-Authorization when transport auth is nil
	if pa := headers.Get("Proxy-Authorization"); pa != "" {
		t.Errorf("expected no Proxy-Authorization, got %q", pa)
	}
}

func TestBuildAuthHeaders_ReconnectFreshToken(t *testing.T) {
	src := newFakeTokenSource("token-attempt-1", "token-attempt-2", "token-attempt-3")
	cc := &ControlChannelClient{
		config: ControlChannelConfig{
			HubEndpoint:     "https://hub.example.com",
			BrokerID:        "broker-1",
			SecretKey:       []byte("test-secret-key-for-hmac-signing"),
			TransportSource: src,
			TransportMode:   transportauth.HeaderProxyAuthorization,
		},
	}

	// First dial attempt
	headers1, err := cc.buildAuthHeaders()
	if err != nil {
		t.Fatalf("first buildAuthHeaders failed: %v", err)
	}
	if pa := headers1.Get("Proxy-Authorization"); pa != "Bearer token-attempt-1" {
		t.Errorf("first attempt: expected token-attempt-1, got %q", pa)
	}

	// Simulate reconnect — second dial attempt
	headers2, err := cc.buildAuthHeaders()
	if err != nil {
		t.Fatalf("second buildAuthHeaders failed: %v", err)
	}
	if pa := headers2.Get("Proxy-Authorization"); pa != "Bearer token-attempt-2" {
		t.Errorf("second attempt: expected token-attempt-2, got %q", pa)
	}

	// Third reconnect
	headers3, err := cc.buildAuthHeaders()
	if err != nil {
		t.Fatalf("third buildAuthHeaders failed: %v", err)
	}
	if pa := headers3.Get("Proxy-Authorization"); pa != "Bearer token-attempt-3" {
		t.Errorf("third attempt: expected token-attempt-3, got %q", pa)
	}

	// Verify Token() was called once per buildAuthHeaders call
	if src.CallCount() != 3 {
		t.Errorf("expected 3 Token() calls, got %d", src.CallCount())
	}
}

func TestBuildAuthHeaders_ServerlessMode(t *testing.T) {
	cc := &ControlChannelClient{
		config: ControlChannelConfig{
			HubEndpoint:     "https://hub.example.com",
			BrokerID:        "broker-1",
			SecretKey:       []byte("test-secret-key-for-hmac-signing"),
			TransportSource: newFakeTokenSource("serverless-token"),
			TransportMode:   transportauth.HeaderServerlessAuthorization,
		},
	}

	headers, err := cc.buildAuthHeaders()
	if err != nil {
		t.Fatalf("buildAuthHeaders failed: %v", err)
	}

	if sa := headers.Get("X-Serverless-Authorization"); sa != "Bearer serverless-token" {
		t.Errorf("X-Serverless-Authorization: expected %q, got %q", "Bearer serverless-token", sa)
	}
}

func TestBuildAuthHeaders_NoSecretKey_WithTransport(t *testing.T) {
	cc := &ControlChannelClient{
		config: ControlChannelConfig{
			HubEndpoint:     "https://hub.example.com",
			BrokerID:        "broker-1",
			TransportSource: newFakeTokenSource("no-hmac-token"),
			TransportMode:   transportauth.HeaderProxyAuthorization,
		},
	}

	headers, err := cc.buildAuthHeaders()
	if err != nil {
		t.Fatalf("buildAuthHeaders failed: %v", err)
	}

	// Should have X-Scion-Broker-ID (dev-auth path)
	if bid := headers.Get("X-Scion-Broker-ID"); bid != "broker-1" {
		t.Errorf("expected X-Scion-Broker-ID, got %q", bid)
	}

	// Should still have transport auth
	if pa := headers.Get("Proxy-Authorization"); pa != "Bearer no-hmac-token" {
		t.Errorf("Proxy-Authorization: expected %q, got %q", "Bearer no-hmac-token", pa)
	}
}
