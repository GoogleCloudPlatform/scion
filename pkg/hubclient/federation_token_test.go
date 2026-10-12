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

package hubclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The token value below is a synthetic placeholder, not a credential.
const fakeFederationToken = "synthetic.federation.fixture"

func TestAuthFederationToken_PostsAudienceOnce(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/auth/federation-token" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("body: %v", err)
		}
		if len(req) != 1 || req["audience"] != "https://peer.example.com" {
			t.Errorf("unexpected body %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"` + fakeFederationToken + `","expires_at":"2026-10-12T17:00:00Z"}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, WithRetry(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Auth().FederationToken(context.Background(), &FederationTokenRequest{Audience: "https://peer.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Token != fakeFederationToken {
		t.Errorf("token = %q", resp.Token)
	}
	if want := time.Date(2026, 10, 12, 17, 0, 0, 0, time.UTC); !resp.ExpiresAt.Equal(want) {
		t.Errorf("expires_at = %v, want %v", resp.ExpiresAt, want)
	}
	if calls.Load() != 1 {
		t.Errorf("request sent %d times, want 1", calls.Load())
	}
}

func TestAuthFederationToken_NotRetriedOnServerError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"try later"}}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, WithRetry(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Auth().FederationToken(context.Background(), &FederationTokenRequest{Audience: "https://peer.example.com"}); err == nil {
		t.Fatal("expected an error for a 503 response")
	}
	if calls.Load() != 1 {
		t.Errorf("request sent %d times, want 1 (no retry)", calls.Load())
	}
}
