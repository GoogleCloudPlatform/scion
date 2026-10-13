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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withMockCommand replaces runCommand for the duration of a test.
func withMockCommand(t *testing.T, fn func(ctx context.Context, name string, args []string, dir string, env []string) (string, error)) {
	t.Helper()
	orig := runCommand
	runCommand = fn
	t.Cleanup(func() { runCommand = orig })
}

func TestHandleHealth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp healthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("expected status ok, got %s", resp.Status)
	}
}

func TestHandleHealthMethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/health", nil)
	w := httptest.NewRecorder()
	handleHealth(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
	if got, want := w.Header().Get("Allow"), http.MethodGet; got != want {
		t.Errorf("Allow = %q, want %q", got, want)
	}
}

func TestHandleAskSuccess(t *testing.T) {
	withMockCommand(t, func(ctx context.Context, name string, args []string, dir string, env []string) (string, error) {
		if name != "gemini" {
			t.Errorf("expected command 'gemini', got %q", name)
		}
		// Verify args contain --prompt with the query
		found := false
		for i, a := range args {
			if a == "--prompt" && i+1 < len(args) && args[i+1] == "what is scion?" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected --prompt 'what is scion?' in args: %v", args)
		}
		return "Scion is a \x1b[1mcontainer orchestration\x1b[0m platform.", nil
	})

	body := `{"query": "what is scion?"}`
	req := httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleAsk(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp askResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	// ANSI codes should be stripped
	if strings.Contains(resp.Answer, "\x1b") {
		t.Errorf("response still contains ANSI codes: %q", resp.Answer)
	}
	if !strings.Contains(resp.Answer, "container orchestration") {
		t.Errorf("expected answer to contain 'container orchestration', got %q", resp.Answer)
	}
}

func TestHandleAskEmptyQuery(t *testing.T) {
	body := `{"query": ""}`
	req := httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(body))
	w := httptest.NewRecorder()
	handleAsk(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleAskWhitespaceQuery(t *testing.T) {
	body := `{"query": "   "}`
	req := httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(body))
	w := httptest.NewRecorder()
	handleAsk(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleAskQueryTooLong(t *testing.T) {
	long := strings.Repeat("a", maxQueryLength+1)
	body := fmt.Sprintf(`{"query": "%s"}`, long)
	req := httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(body))
	w := httptest.NewRecorder()
	handleAsk(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleAskInvalidJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	handleAsk(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleAskWrongMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/ask", nil)
	w := httptest.NewRecorder()
	handleAsk(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
	if got, want := w.Header().Get("Allow"), http.MethodPost+", "+http.MethodOptions; got != want {
		t.Errorf("Allow = %q, want %q", got, want)
	}
}

func TestHandleAskCommandError(t *testing.T) {
	withMockCommand(t, func(ctx context.Context, name string, args []string, dir string, env []string) (string, error) {
		return "", fmt.Errorf("command failed")
	})

	body := `{"query": "test"}`
	req := httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(body))
	w := httptest.NewRecorder()
	handleAsk(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestHandleRefreshSuccess(t *testing.T) {
	withMockCommand(t, func(ctx context.Context, name string, args []string, dir string, env []string) (string, error) {
		if name != "git" {
			t.Errorf("expected command 'git', got %q", name)
		}
		return "Already up to date.\n", nil
	})

	req := httptest.NewRequest(http.MethodPost, "/refresh", nil)
	w := httptest.NewRecorder()
	handleRefresh(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp refreshResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("expected status ok, got %s", resp.Status)
	}
}

func TestHandleRefreshWrongMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/refresh", nil)
	w := httptest.NewRecorder()
	handleRefresh(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
	if got, want := w.Header().Get("Allow"), http.MethodPost+", "+http.MethodOptions; got != want {
		t.Errorf("Allow = %q, want %q", got, want)
	}
}

func TestHandleRefreshError(t *testing.T) {
	withMockCommand(t, func(ctx context.Context, name string, args []string, dir string, env []string) (string, error) {
		return "fatal: not a git repository", fmt.Errorf("exit status 128")
	})

	req := httptest.NewRequest(http.MethodPost, "/refresh", nil)
	w := httptest.NewRecorder()
	handleRefresh(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestHandleChat(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/chat", nil)
	w := httptest.NewRecorder()
	handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("expected text/html content type, got %s", ct)
	}
	if !strings.Contains(w.Body.String(), "<!DOCTYPE html>") {
		t.Errorf("expected HTML content in response body")
	}
}

func TestHandleChatWrongMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/chat", nil)
	w := httptest.NewRecorder()
	handleChat(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
	if got, want := w.Header().Get("Allow"), http.MethodGet; got != want {
		t.Errorf("Allow = %q, want %q", got, want)
	}
}

func TestCORSMiddleware(t *testing.T) {
	handler := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("expected CORS origin *, got %q", got)
	}
}

func TestCORSPreflightOptions(t *testing.T) {
	handler := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("next handler should not be called for OPTIONS preflight")
	}))

	req := httptest.NewRequest(http.MethodOptions, "/ask", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
}

func TestGetRepoDirDefaults(t *testing.T) {
	t.Setenv("DOCS_AGENT_WORKSPACE", "")
	t.Setenv("DOCS_AGENT_REPO_DIR", "")
	if got := getRepoDir(); got != "/workspace/scion" {
		t.Errorf("expected /workspace/scion, got %s", got)
	}
}

func TestGetRepoDirFromWorkspace(t *testing.T) {
	t.Setenv("DOCS_AGENT_WORKSPACE", "/home/me/projects")
	t.Setenv("DOCS_AGENT_REPO_DIR", "")
	if got := getRepoDir(); got != "/home/me/projects/scion" {
		t.Errorf("expected /home/me/projects/scion, got %s", got)
	}
}

func TestGetRepoDirExplicitOverride(t *testing.T) {
	t.Setenv("DOCS_AGENT_WORKSPACE", "/ignored")
	t.Setenv("DOCS_AGENT_REPO_DIR", "/custom/path")
	if got := getRepoDir(); got != "/custom/path" {
		t.Errorf("expected /custom/path, got %s", got)
	}
}

func TestDebugEnabled(t *testing.T) {
	tests := []struct {
		value    string
		expected bool
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"True", true},
		{"0", false},
		{"false", false},
		{"", false},
		{"yes", false},
	}
	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("DEBUG", tc.value)
			if got := debugEnabled(); got != tc.expected {
				t.Errorf("DEBUG=%q: expected %v, got %v", tc.value, tc.expected, got)
			}
		})
	}
}

func TestHandleAskCommandErrorDebugLogs(t *testing.T) {
	t.Setenv("DEBUG", "1")
	withMockCommand(t, func(ctx context.Context, name string, args []string, dir string, env []string) (string, error) {
		return "partial output", fmt.Errorf("command failed")
	})

	body := `{"query": "test"}`
	req := httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(body))
	w := httptest.NewRecorder()
	handleAsk(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
	// Verify the error response is still generic (no leak of internal details)
	var resp errorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Error != "failed to get response from Gemini" {
		t.Errorf("unexpected error message: %q", resp.Error)
	}
}

func TestTruncateTail(t *testing.T) {
	short := "hello"
	if got := truncateTail(short, 100); got != short {
		t.Errorf("expected %q, got %q", short, got)
	}
	long := strings.Repeat("x", 50) + "IMPORTANT"
	got := truncateTail(long, 20)
	if !strings.HasPrefix(got, "...[truncated] ") {
		t.Errorf("expected truncation prefix, got %q", got)
	}
	if !strings.HasSuffix(got, "IMPORTANT") {
		t.Errorf("expected tail preserved, got %q", got)
	}
}

func TestCleanGeminiOutput(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "strips ANSI codes",
			input:    "Hello \x1b[31mred\x1b[0m world \x1b[1;32mgreen\x1b[0m",
			expected: "Hello red world green",
		},
		{
			name:     "strips MCP warning",
			input:    "MCP issues detected. Run /mcp list for status.\nThe answer is 42.",
			expected: "The answer is 42.",
		},
		{
			name:     "strips MCP warning without trailing content",
			input:    "MCP issues detected. Run /mcp list for status.\n",
			expected: "",
		},
		{
			name:     "preserves clean markdown",
			input:    "# Hello\n\n**bold** and *italic*\n",
			expected: "# Hello\n\n**bold** and *italic*",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanGeminiOutput(tc.input); got != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, got)
			}
		})
	}
}
