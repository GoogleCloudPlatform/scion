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

func TestSetAndDeleteSecret_AgentScope(t *testing.T) {
	t.Run("SetSecret sends PUT with scope=agent", func(t *testing.T) {
		var gotMethod, gotPath string
		var gotReq SetSecretRequest
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
				t.Errorf("decode body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(SetSecretResponse{
				Key:     "MY_AGENT_KEY",
				Scope:   "agent",
				ScopeID: "agent-1",
			})
		}))
		defer srv.Close()

		c := &Client{hubURL: srv.URL, agentID: "agent-1", token: "tok", client: srv.Client()}
		resp, err := c.SetSecret(context.Background(), "MY_AGENT_KEY", "dmFs", "environment", "", "agent", false, nil)
		if err != nil {
			t.Fatalf("SetSecret failed: %v", err)
		}
		if gotMethod != http.MethodPut || gotPath != "/api/v1/agents/agent-1/secrets/MY_AGENT_KEY" {
			t.Fatalf("unexpected request %s %s", gotMethod, gotPath)
		}
		if gotReq.Scope != "agent" || gotReq.Value != "dmFs" {
			t.Fatalf("unexpected request body: %+v", gotReq)
		}
		if resp.Scope != "agent" || resp.Key != "MY_AGENT_KEY" {
			t.Fatalf("unexpected response: %+v", resp)
		}
	})

	t.Run("SetSecret experiment-off maps to ErrAgentScopeUnavailable", func(t *testing.T) {
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
		_, err := c.SetSecret(context.Background(), "K", "dmFs", "", "", "agent", false, nil)
		if !errors.Is(err, ErrAgentScopeUnavailable) {
			t.Fatalf("expected ErrAgentScopeUnavailable, got %v", err)
		}
	})

	t.Run("DeleteSecret sends DELETE with scope=agent", func(t *testing.T) {
		var gotMethod, gotPath, gotQuery string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotQuery = r.URL.RawQuery
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()

		c := &Client{hubURL: srv.URL, agentID: "agent-1", token: "tok", client: srv.Client()}
		if err := c.DeleteSecret(context.Background(), "MY_AGENT_KEY", "agent"); err != nil {
			t.Fatalf("DeleteSecret failed: %v", err)
		}
		if gotMethod != http.MethodDelete || gotPath != "/api/v1/agents/agent-1/secrets/MY_AGENT_KEY" || gotQuery != "scope=agent" {
			t.Fatalf("unexpected request: %s %s?%s", gotMethod, gotPath, gotQuery)
		}
	})

	t.Run("DeleteSecret 404 returns not found error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		c := &Client{hubURL: srv.URL, agentID: "agent-1", token: "tok", client: srv.Client()}
		err := c.DeleteSecret(context.Background(), "MISSING_KEY", "agent")
		if err == nil {
			t.Fatal("expected error for 404, got nil")
		}
	})

	t.Run("DeleteSecret experiment-off maps to ErrAgentScopeUnavailable", func(t *testing.T) {
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
		err := c.DeleteSecret(context.Background(), "K", "agent")
		if !errors.Is(err, ErrAgentScopeUnavailable) {
			t.Fatalf("expected ErrAgentScopeUnavailable, got %v", err)
		}
	})
}
