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

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListSecretsWithScope_Narrow400Matching(t *testing.T) {
	t.Run("experiment-off scope=all maps to ErrAgentScopeUnavailable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{
					"code":    "validation_error",
					"message": `scope must be "project" or "user"`,
				},
			})
		}))
		defer srv.Close()

		c := &Client{hubURL: srv.URL, agentID: "agent-1", token: "tok", client: srv.Client()}
		_, err := c.ListSecretsWithScope(context.Background(), "all")
		if !errors.Is(err, ErrAgentScopeUnavailable) {
			t.Fatalf("expected ErrAgentScopeUnavailable, got %v", err)
		}
	})

	t.Run("experiment-off scope=agent maps to ErrAgentScopeUnavailable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{
					"code":    "invalid_request",
					"message": "invalid scope: agent",
				},
			})
		}))
		defer srv.Close()

		c := &Client{hubURL: srv.URL, agentID: "agent-1", token: "tok", client: srv.Client()}
		_, err := c.ListSecretsWithScope(context.Background(), "agent")
		if !errors.Is(err, ErrAgentScopeUnavailable) {
			t.Fatalf("expected ErrAgentScopeUnavailable, got %v", err)
		}
	})

	t.Run("unrelated 400 does not map to ErrAgentScopeUnavailable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{
					"code":    "validation_error",
					"message": "malformed query parameter",
				},
			})
		}))
		defer srv.Close()

		c := &Client{hubURL: srv.URL, agentID: "agent-1", token: "tok", client: srv.Client()}
		_, err := c.ListSecretsWithScope(context.Background(), "all")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if errors.Is(err, ErrAgentScopeUnavailable) {
			t.Fatalf("unrelated 400 must not map to ErrAgentScopeUnavailable: %v", err)
		}
	})
}
