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

package apiclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewTransport(t *testing.T) {
	tr := NewTransport("https://example.com")
	if tr.BaseURL != "https://example.com" {
		t.Errorf("expected base URL 'https://example.com', got %q", tr.BaseURL)
	}
	if tr.HTTPClient == nil {
		t.Error("expected HTTP client to be initialized")
	}
	if tr.UserAgent != "scion-client/1.0" {
		t.Errorf("expected user agent 'scion-client/1.0', got %q", tr.UserAgent)
	}
}

func TestNewTransportWithOptions(t *testing.T) {
	customClient := &http.Client{Timeout: 60 * time.Second}
	tr := NewTransport("https://example.com",
		WithHTTPClient(customClient),
		WithUserAgent("test-client/2.0"),
		WithRetry(3, 2*time.Second),
	)

	if tr.HTTPClient != customClient {
		t.Error("expected custom HTTP client")
	}
	if tr.UserAgent != "test-client/2.0" {
		t.Errorf("expected user agent 'test-client/2.0', got %q", tr.UserAgent)
	}
	if tr.MaxRetries != 3 {
		t.Errorf("expected max retries 3, got %d", tr.MaxRetries)
	}
	if tr.RetryWait != 2*time.Second {
		t.Errorf("expected retry wait 2s, got %v", tr.RetryWait)
	}
}

func TestTransportGet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/test" {
			t.Errorf("expected path /api/v1/test, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer server.Close()

	tr := NewTransport(server.URL)
	resp, err := tr.Get(context.Background(), "/api/v1/test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
}

func TestTransportPost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", r.Header.Get("Content-Type"))
		}

		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}
		if body["name"] != "test" {
			t.Errorf("expected name 'test', got %q", body["name"])
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"id": "123"})
	}))
	defer server.Close()

	tr := NewTransport(server.URL)
	resp, err := tr.Post(context.Background(), "/api/v1/resources", map[string]string{"name": "test"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("expected status 201, got %d", resp.StatusCode)
	}
}

func TestTransportWithAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-token" {
			t.Errorf("expected Authorization 'Bearer test-token', got %q", auth)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tr := NewTransport(server.URL, WithAuth(&BearerAuth{Token: "test-token"}))
	resp, err := tr.Get(context.Background(), "/api/v1/protected", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()
}

func TestDecodeResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":   "123",
			"name": "test",
		})
	}))
	defer server.Close()

	tr := NewTransport(server.URL)
	resp, err := tr.Get(context.Background(), "/api/v1/resource", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	type Resource struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}

	result, err := DecodeResponse[Resource](resp)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}

	if result.ID != "123" {
		t.Errorf("expected id '123', got %q", result.ID)
	}
	if result.Name != "test" {
		t.Errorf("expected name 'test', got %q", result.Name)
	}
}

func TestDecodeResponseError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":    "not_found",
				"message": "Resource not found",
			},
		})
	}))
	defer server.Close()

	tr := NewTransport(server.URL)
	resp, err := tr.Get(context.Background(), "/api/v1/missing", nil)
	if err != nil {
		t.Fatalf("unexpected network error: %v", err)
	}

	type Resource struct {
		ID string `json:"id"`
	}

	_, err = DecodeResponse[Resource](resp)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}

	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected APIError, got %T", err)
	}

	if !apiErr.IsNotFound() {
		t.Errorf("expected not found error, got status %d", apiErr.StatusCode)
	}
	if apiErr.Code != "not_found" {
		t.Errorf("expected code 'not_found', got %q", apiErr.Code)
	}
}
