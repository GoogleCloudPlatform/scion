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
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Mock GCP token generator for tests (no real GCP calls)
// ---------------------------------------------------------------------------

type mockTokenGenerator struct {
	mu             sync.Mutex
	accessToken    string
	accessTokenErr error
	email          string
	calls          int
}

func (m *mockTokenGenerator) GenerateAccessToken(_ context.Context, _ string, _ []string) (*GCPAccessToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.accessTokenErr != nil {
		return nil, m.accessTokenErr
	}
	return &GCPAccessToken{
		AccessToken: m.accessToken,
		ExpiresIn:   3600,
		TokenType:   "Bearer",
	}, nil
}

func (m *mockTokenGenerator) GenerateIDToken(_ context.Context, _ string, _ string) (*GCPIDToken, error) {
	return &GCPIDToken{Token: "mock-id-token"}, nil
}

func (m *mockTokenGenerator) VerifyImpersonation(_ context.Context, _ string) error {
	return nil
}

func (m *mockTokenGenerator) ServiceAccountEmail() string {
	return m.email
}

// ---------------------------------------------------------------------------
// Audit logger that captures events for inspection
// ---------------------------------------------------------------------------

type capturingAuditLogger struct {
	mu     sync.Mutex
	events []*LifecycleHookExecutionEvent
	// Embed the real logger so we satisfy the full interface without
	// implementing every method from scratch.
	*LogAuditLogger
}

func newCapturingAuditLogger() *capturingAuditLogger {
	return &capturingAuditLogger{
		LogAuditLogger: NewLogAuditLogger("[Test]", true),
	}
}

func (l *capturingAuditLogger) LogLifecycleHookExecutionEvent(_ context.Context, event *LifecycleHookExecutionEvent) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
	return nil
}

func (l *capturingAuditLogger) getEvents() []*LifecycleHookExecutionEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*LifecycleHookExecutionEvent, len(l.events))
	copy(out, l.events)
	return out
}

// ---------------------------------------------------------------------------
// Test store setup
// ---------------------------------------------------------------------------

func executorTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := newTestStore(":memory:")
	require.NoError(t, err)
	return s
}

func seedExecutorProject(t *testing.T, s store.Store, name string) string {
	t.Helper()
	id := uuid.New().String()
	require.NoError(t, s.CreateProject(context.Background(), &store.Project{
		ID:         id,
		Name:       name,
		Slug:       name,
		Visibility: "private",
		Created:    time.Now(),
		Updated:    time.Now(),
	}))
	return id
}

func seedExecutorSA(t *testing.T, s store.Store, projectID, email string) string {
	t.Helper()
	id := uuid.New().String()
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), &store.GCPServiceAccount{
		ID:                 id,
		Scope:              store.ScopeProject,
		ScopeID:            projectID,
		Email:              email,
		ProjectID:          "gcp-project",
		DisplayName:        "Test SA",
		DefaultScopes:      []string{"https://www.googleapis.com/auth/cloud-platform"},
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: "verified",
		CreatedBy:          "test-user",
		CreatedAt:          time.Now(),
	}))
	return id
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func makeTestHook(saID string, action *store.LifecycleHookAction) *store.LifecycleHook {
	return &store.LifecycleHook{
		ID:                uuid.New().String(),
		Name:              "test-hook",
		ScopeType:         store.LifecycleHookScopeHub,
		Trigger:           store.LifecycleHookTriggerRunning,
		Action:            action,
		ExecutionIdentity: saID,
		Enabled:           true,
		Created:           time.Now(),
		Updated:           time.Now(),
	}
}

func makeTestAgent(projectID string) *store.Agent {
	return &store.Agent{
		ID:          uuid.New().String(),
		Slug:        "test-agent",
		Name:        "Test Agent <with special chars>",
		Template:    "test-template",
		ProjectID:   projectID,
		Phase:       "running",
		TaskSummary: "doing some work\nwith newlines",
		Message:     "agent error message",
		Created:     time.Now(),
		Updated:     time.Now(),
		Visibility:  "private",
	}
}

// newTestExecutor creates an HTTPExecutor with a test-friendly HTTP client
// that allows loopback connections (httptest servers bind to 127.0.0.1).
// The client still blocks ALL redirects, matching production behavior.
func newTestExecutor(s store.Store, tokenGen GCPTokenGenerator, auditLog AuditLogger, log *slog.Logger) *HTTPExecutor {
	executor := NewHTTPExecutor(s, tokenGen, auditLog, log)
	executor.newHTTPClient = func() *http.Client {
		return &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return fmt.Errorf("redirects are blocked for lifecycle hook requests (SSRF protection)")
			},
		}
	}
	return executor
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestLifecycleHookExecutor_Success2xx(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")
	saID := seedExecutorSA(t, s, projID, "test-sa@project.iam.gserviceaccount.com")

	var receivedAuth string
	var receivedBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer ts.Close()

	tokenGen := &mockTokenGenerator{accessToken: "mock-access-token-123", email: "hub@sa.com"}
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	hook := makeTestHook(saID, &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionHTTP,
		Method:         "POST",
		URL:            ts.URL + "/register/${AGENT_ID}",
		Headers:        map[string]string{"Content-Type": "application/json"},
		Body:           `{"agent":"${AGENT_ID}","trigger":"${TRIGGER}"}`,
		OnError:        store.LifecycleHookOnErrorLog,
		TimeoutSeconds: 10,
	})
	agent := makeTestAgent(projID)

	err := executor.Execute(context.Background(), hook, agent, "running")
	require.NoError(t, err)

	// Verify the Authorization header was attached (http type).
	assert.Equal(t, "Bearer mock-access-token-123", receivedAuth)

	// Verify the body was rendered with trusted substitution.
	assert.Contains(t, receivedBody, agent.ID)
	assert.Contains(t, receivedBody, `"trigger":"running"`)

	// Verify audit event.
	events := auditLog.getEvents()
	require.Len(t, events, 1)
	assert.True(t, events[0].Success)
	assert.Equal(t, 200, events[0].HTTPStatusCode)
	assert.Equal(t, "test-sa@project.iam.gserviceaccount.com", events[0].ExecutionIdentity)
	assert.Equal(t, 1, events[0].Attempt)
	assert.Greater(t, events[0].LatencyMs, int64(-1))
}

func TestLifecycleHookExecutor_Failure4xx(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")
	saID := seedExecutorSA(t, s, projID, "sa@p.iam.gserviceaccount.com")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer ts.Close()

	tokenGen := &mockTokenGenerator{accessToken: "tok", email: "hub@sa.com"}
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	hook := makeTestHook(saID, &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionHTTP,
		Method:         "POST",
		URL:            ts.URL + "/register",
		Body:           `{}`,
		OnError:        store.LifecycleHookOnErrorLog,
		TimeoutSeconds: 5,
	})
	agent := makeTestAgent(projID)

	err := executor.Execute(context.Background(), hook, agent, "running")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 400")

	// Verify failure audit event — no response body persisted.
	events := auditLog.getEvents()
	require.Len(t, events, 1)
	assert.False(t, events[0].Success)
	assert.Equal(t, 400, events[0].HTTPStatusCode)
	assert.Equal(t, "HTTP 400", events[0].FailReason)
	// Ensure no response body in any field.
	assert.NotContains(t, events[0].FailReason, "bad request")
}

func TestLifecycleHookExecutor_Failure5xx(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")
	saID := seedExecutorSA(t, s, projID, "sa@p.iam.gserviceaccount.com")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`internal error details`))
	}))
	defer ts.Close()

	tokenGen := &mockTokenGenerator{accessToken: "tok", email: "hub@sa.com"}
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	hook := makeTestHook(saID, &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionHTTP,
		Method:         "POST",
		URL:            ts.URL + "/register",
		Body:           `{}`,
		OnError:        store.LifecycleHookOnErrorLog,
		TimeoutSeconds: 5,
	})

	err := executor.Execute(context.Background(), hook, makeTestAgent(projID), "stopped")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 500")

	events := auditLog.getEvents()
	require.Len(t, events, 1)
	assert.False(t, events[0].Success)
	assert.Equal(t, 500, events[0].HTTPStatusCode)
	// Response body MUST NOT be in audit.
	assert.NotContains(t, events[0].FailReason, "internal error details")
}

func TestLifecycleHookExecutor_Timeout(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")
	saID := seedExecutorSA(t, s, projID, "sa@p.iam.gserviceaccount.com")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep longer than the timeout.
		time.Sleep(5 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	tokenGen := &mockTokenGenerator{accessToken: "tok", email: "hub@sa.com"}
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	hook := makeTestHook(saID, &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionHTTP,
		Method:         "GET",
		URL:            ts.URL + "/slow",
		OnError:        store.LifecycleHookOnErrorLog,
		TimeoutSeconds: 1, // 1-second timeout
	})

	start := time.Now()
	err := executor.Execute(context.Background(), hook, makeTestAgent(projID), "running")
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "request failed")
	// Should have timed out in roughly 1 second, not 5.
	assert.Less(t, elapsed, 3*time.Second)

	events := auditLog.getEvents()
	require.Len(t, events, 1)
	assert.False(t, events[0].Success)
	assert.Equal(t, 0, events[0].HTTPStatusCode) // no response received
}

func TestLifecycleHookExecutor_RetryWithBackoff(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")
	saID := seedExecutorSA(t, s, projID, "sa@p.iam.gserviceaccount.com")

	var attemptCount atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attemptCount.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	tokenGen := &mockTokenGenerator{accessToken: "tok", email: "hub@sa.com"}
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	hook := makeTestHook(saID, &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionHTTP,
		Method:         "POST",
		URL:            ts.URL + "/register",
		Body:           `{}`,
		OnError:        store.LifecycleHookOnErrorRetry, // retry policy
		TimeoutSeconds: 5,
	})

	err := executor.Execute(context.Background(), hook, makeTestAgent(projID), "running")
	require.NoError(t, err)

	// Should have made 3 attempts.
	assert.Equal(t, int32(3), attemptCount.Load())

	// Should have 3 audit events (one per attempt).
	events := auditLog.getEvents()
	require.Len(t, events, 3)
	assert.False(t, events[0].Success)
	assert.Equal(t, 1, events[0].Attempt)
	assert.False(t, events[1].Success)
	assert.Equal(t, 2, events[1].Attempt)
	assert.True(t, events[2].Success)
	assert.Equal(t, 3, events[2].Attempt)
}

func TestLifecycleHookExecutor_RetryExhausted(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")
	saID := seedExecutorSA(t, s, projID, "sa@p.iam.gserviceaccount.com")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ts.Close()

	tokenGen := &mockTokenGenerator{accessToken: "tok", email: "hub@sa.com"}
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	hook := makeTestHook(saID, &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionHTTP,
		Method:         "POST",
		URL:            ts.URL + "/register",
		Body:           `{}`,
		OnError:        store.LifecycleHookOnErrorRetry,
		TimeoutSeconds: 5,
	})

	err := executor.Execute(context.Background(), hook, makeTestAgent(projID), "running")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "all 3 attempts failed")

	// Should have 3 audit events, all failures.
	events := auditLog.getEvents()
	require.Len(t, events, maxRetryAttempts)
	for i, e := range events {
		assert.False(t, e.Success, "attempt %d should be failure", i+1)
		assert.Equal(t, i+1, e.Attempt)
		assert.Equal(t, 502, e.HTTPStatusCode)
	}
}

func TestLifecycleHookExecutor_HTTPTypeAttachesBearerToken(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")
	saID := seedExecutorSA(t, s, projID, "sa@p.iam.gserviceaccount.com")

	var receivedAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	tokenGen := &mockTokenGenerator{accessToken: "secret-bearer-token-xyz", email: "hub@sa.com"}
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	hook := makeTestHook(saID, &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionHTTP,
		Method:         "POST",
		URL:            ts.URL + "/api",
		Body:           `{}`,
		TimeoutSeconds: 5,
	})

	err := executor.Execute(context.Background(), hook, makeTestAgent(projID), "running")
	require.NoError(t, err)

	assert.Equal(t, "Bearer secret-bearer-token-xyz", receivedAuth)

	// CRITICAL: Verify that the auth header value does NOT appear in audit.
	events := auditLog.getEvents()
	require.Len(t, events, 1)
	assert.NotContains(t, events[0].FailReason, "secret-bearer-token-xyz")
	assert.NotContains(t, events[0].ExecutionIdentity, "secret-bearer-token-xyz")
	assert.NotContains(t, events[0].Host, "secret-bearer-token-xyz")
}

func TestLifecycleHookExecutor_WebhookSendsNoAuth(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")
	saID := seedExecutorSA(t, s, projID, "sa@p.iam.gserviceaccount.com")

	var receivedAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	tokenGen := &mockTokenGenerator{accessToken: "should-not-appear", email: "hub@sa.com"}
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	hook := makeTestHook(saID, &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionWebhook,
		Method:         "POST",
		URL:            ts.URL + "/webhook?token=webhook-secret",
		Body:           `{"event":"agent_started"}`,
		TimeoutSeconds: 5,
	})

	err := executor.Execute(context.Background(), hook, makeTestAgent(projID), "running")
	require.NoError(t, err)

	// Webhook MUST NOT have an Authorization header.
	assert.Empty(t, receivedAuth, "webhook must not send Authorization header")

	// Verify audit doesn't contain the bearer token either.
	events := auditLog.getEvents()
	require.Len(t, events, 1)
	assert.NotContains(t, events[0].FailReason, "should-not-appear")
}

func TestLifecycleHookExecutor_UntrustedVariableEncoding(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")
	saID := seedExecutorSA(t, s, projID, "sa@p.iam.gserviceaccount.com")

	var receivedBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	tokenGen := &mockTokenGenerator{accessToken: "tok", email: "hub@sa.com"}
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	hook := makeTestHook(saID, &store.LifecycleHookAction{
		Type:                 store.LifecycleHookActionHTTP,
		Method:               "POST",
		URL:                  ts.URL + "/register",
		Headers:              map[string]string{"Content-Type": "application/json"},
		Body:                 `{"agent_id":"${AGENT_ID}","name":"${AGENT_NAME}","summary":"${TASK_SUMMARY}"}`,
		AllowedUntrustedVars: []string{"AGENT_NAME", "TASK_SUMMARY"},
		TimeoutSeconds:       5,
	})

	agent := makeTestAgent(projID)
	// Set agent name with special characters that need JSON encoding.
	agent.Name = `Evil Agent "with quotes" and \backslash`
	agent.TaskSummary = "line1\nline2\ttab"

	err := executor.Execute(context.Background(), hook, agent, "running")
	require.NoError(t, err)

	// The untrusted values should be JSON-encoded (via RenderAction).
	// Quotes and backslashes should be escaped.
	assert.Contains(t, receivedBody, `Evil Agent \"with quotes\" and \\backslash`)
	assert.Contains(t, receivedBody, `line1\nline2\ttab`)

	// Trusted vars (AGENT_ID) should be substituted verbatim.
	assert.Contains(t, receivedBody, agent.ID)
}

func TestLifecycleHookExecutor_RedirectBlocked(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")
	saID := seedExecutorSA(t, s, projID, "sa@p.iam.gserviceaccount.com")

	// Server that redirects to localhost (simulating SSRF via redirect).
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1234/internal", http.StatusFound)
	}))
	defer ts.Close()

	tokenGen := &mockTokenGenerator{accessToken: "tok", email: "hub@sa.com"}
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	hook := makeTestHook(saID, &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionHTTP,
		Method:         "GET",
		URL:            ts.URL + "/redirect-me",
		TimeoutSeconds: 5,
	})

	err := executor.Execute(context.Background(), hook, makeTestAgent(projID), "running")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redirect")

	events := auditLog.getEvents()
	require.Len(t, events, 1)
	assert.False(t, events[0].Success)
	assert.Contains(t, events[0].FailReason, "redirect")
}

func TestLifecycleHookExecutor_NoResponseBodyInAudit(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")
	saID := seedExecutorSA(t, s, projID, "sa@p.iam.gserviceaccount.com")

	sensitiveResponseBody := "SUPER_SECRET_RESPONSE_DATA_12345"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sensitiveResponseBody))
	}))
	defer ts.Close()

	tokenGen := &mockTokenGenerator{accessToken: "tok", email: "hub@sa.com"}
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	hook := makeTestHook(saID, &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionHTTP,
		Method:         "POST",
		URL:            ts.URL + "/api",
		Body:           `{}`,
		TimeoutSeconds: 5,
	})

	err := executor.Execute(context.Background(), hook, makeTestAgent(projID), "running")
	require.NoError(t, err)

	// The response body MUST NOT appear anywhere in audit events.
	events := auditLog.getEvents()
	require.Len(t, events, 1)
	eventStr := fmt.Sprintf("%+v", events[0])
	assert.NotContains(t, eventStr, sensitiveResponseBody)
}

func TestLifecycleHookExecutor_NoAuthHeaderInAudit(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")
	saID := seedExecutorSA(t, s, projID, "sa@p.iam.gserviceaccount.com")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden) // Trigger a failure to see fail_reason
		_, _ = w.Write([]byte("forbidden"))
	}))
	defer ts.Close()

	secretToken := "ULTRA_SECRET_TOKEN_ABCDEF"
	tokenGen := &mockTokenGenerator{accessToken: secretToken, email: "hub@sa.com"}
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	hook := makeTestHook(saID, &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionHTTP,
		Method:         "POST",
		URL:            ts.URL + "/api",
		Body:           `{}`,
		TimeoutSeconds: 5,
	})

	err := executor.Execute(context.Background(), hook, makeTestAgent(projID), "running")
	require.Error(t, err)

	events := auditLog.getEvents()
	require.Len(t, events, 1)
	// The bearer token MUST NOT appear anywhere in the audit event.
	eventStr := fmt.Sprintf("%+v", events[0])
	assert.NotContains(t, eventStr, secretToken)
}

func TestLifecycleHookExecutor_WebhookNoExecutionIdentity(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")

	var receivedAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	tokenGen := &mockTokenGenerator{accessToken: "should-never-be-used", email: "hub@sa.com"}
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	// Webhook with no execution identity — valid for webhooks.
	hook := makeTestHook("", &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionWebhook,
		Method:         "POST",
		URL:            ts.URL + "/webhook",
		Body:           `{"event":"test"}`,
		TimeoutSeconds: 5,
	})
	hook.ExecutionIdentity = "" // explicitly no identity

	err := executor.Execute(context.Background(), hook, makeTestAgent(projID), "running")
	require.NoError(t, err)

	assert.Empty(t, receivedAuth)
}

func TestLifecycleHookExecutor_HTTPRequiresExecutionIdentity(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")

	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, nil, auditLog, slog.Default())

	hook := makeTestHook("", &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionHTTP,
		Method:         "POST",
		URL:            "https://example.com/api",
		Body:           `{}`,
		TimeoutSeconds: 5,
	})
	hook.ExecutionIdentity = ""

	err := executor.Execute(context.Background(), hook, makeTestAgent(projID), "running")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "execution identity")

	// Should still get an audit event for the failure.
	events := auditLog.getEvents()
	require.Len(t, events, 1)
	assert.False(t, events[0].Success)
}

func TestLifecycleHookExecutor_RenderVarsCorrectTrustClasses(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")

	executor := newTestExecutor(s, nil, nil, slog.Default())
	agent := makeTestAgent(projID)
	agent.Name = "Evil Agent"
	agent.TaskSummary = "task summary"
	agent.Phase = "running"
	agent.Message = "error msg"
	agent.Slug = "my-agent"

	hook := &store.LifecycleHook{
		ID:   "hook-123",
		Name: "test-hook",
	}

	vars := executor.buildRenderVars(context.Background(), hook, agent, "running", "sa@test.com")

	// Verify trusted variables are present.
	assert.Equal(t, "hook-123", vars["HOOK_ID"])
	assert.Equal(t, "test-hook", vars["HOOK_NAME"])
	assert.Equal(t, "running", vars["TRIGGER"])
	assert.Equal(t, agent.ID, vars["AGENT_ID"])
	assert.Equal(t, "my-agent", vars["AGENT_SLUG"])
	assert.Equal(t, "sa@test.com", vars["SA_EMAIL"])
	assert.Equal(t, projID, vars["PROJECT_ID"])
	assert.Equal(t, "test-project", vars["PROJECT_NAME"])

	// Verify untrusted variables are present (will be encoded by RenderAction).
	assert.Equal(t, "Evil Agent", vars["AGENT_NAME"])
	assert.Equal(t, "task summary", vars["TASK_SUMMARY"])
	assert.Equal(t, "running", vars["AGENT_STATUS"])
	assert.Equal(t, "error msg", vars["ERROR_MSG"])
}

func TestLifecycleHookExecutor_NoAction(t *testing.T) {
	s := executorTestStore(t)
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, nil, auditLog, slog.Default())

	hook := &store.LifecycleHook{
		ID:     "hook-no-action",
		Name:   "no-action",
		Action: nil,
	}

	err := executor.Execute(context.Background(), hook, makeTestAgent("proj"), "running")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no action")
}

func TestLifecycleHookExecutor_AuditHostOnly(t *testing.T) {
	// Verify that audit records only the host, not the full URL (which may
	// contain path-based tokens for webhooks).
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")
	saID := seedExecutorSA(t, s, projID, "sa@p.iam.gserviceaccount.com")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	tokenGen := &mockTokenGenerator{accessToken: "tok", email: "hub@sa.com"}
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	hook := makeTestHook(saID, &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionHTTP,
		Method:         "POST",
		URL:            ts.URL + "/secret-path/with-token",
		Body:           `{}`,
		TimeoutSeconds: 5,
	})

	err := executor.Execute(context.Background(), hook, makeTestAgent(projID), "running")
	require.NoError(t, err)

	events := auditLog.getEvents()
	require.Len(t, events, 1)
	// Host should be just the host:port, not the full URL.
	assert.True(t, strings.HasPrefix(events[0].Host, "127.0.0.1:"))
	assert.NotContains(t, events[0].Host, "/secret-path")
}

func TestLifecycleHookExecutor_TokenGeneratorError(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")
	saID := seedExecutorSA(t, s, projID, "sa@p.iam.gserviceaccount.com")

	tokenGen := &mockTokenGenerator{
		accessTokenErr: fmt.Errorf("IAM permission denied"),
		email:          "hub@sa.com",
	}
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	hook := makeTestHook(saID, &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionHTTP,
		Method:         "POST",
		URL:            "https://example.com/api",
		Body:           `{}`,
		TimeoutSeconds: 5,
	})

	err := executor.Execute(context.Background(), hook, makeTestAgent(projID), "running")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "generate access token")

	events := auditLog.getEvents()
	require.Len(t, events, 1)
	assert.False(t, events[0].Success)
	assert.Contains(t, events[0].FailReason, "IAM permission denied")
}

func TestLifecycleHookExecutor_DefaultTimeout(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")
	saID := seedExecutorSA(t, s, projID, "sa@p.iam.gserviceaccount.com")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	tokenGen := &mockTokenGenerator{accessToken: "tok", email: "hub@sa.com"}
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	hook := makeTestHook(saID, &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionHTTP,
		Method:         "GET",
		URL:            ts.URL + "/api",
		TimeoutSeconds: 0, // no timeout specified -> default should apply
	})

	err := executor.Execute(context.Background(), hook, makeTestAgent(projID), "running")
	require.NoError(t, err)

	events := auditLog.getEvents()
	require.Len(t, events, 1)
	assert.True(t, events[0].Success)
}

// ---------------------------------------------------------------------------
// SSRF Protection — isBlockedSSRFTarget unit tests
// ---------------------------------------------------------------------------

func TestIsBlockedSSRFTarget(t *testing.T) {
	tests := []struct {
		name    string
		ip      net.IP
		blocked bool
	}{
		// Loopback — MUST block
		{"loopback IPv4", net.ParseIP("127.0.0.1"), true},
		{"loopback IPv4 other", net.ParseIP("127.0.0.2"), true},
		{"loopback IPv6", net.ParseIP("::1"), true},

		// Link-local — MUST block (includes metadata service 169.254.169.254)
		{"link-local IPv4 metadata", net.ParseIP("169.254.169.254"), true},
		{"link-local IPv4 base", net.ParseIP("169.254.0.1"), true},
		{"link-local IPv6", net.ParseIP("fe80::1"), true},

		// Link-local multicast — MUST block
		{"link-local multicast IPv4", net.ParseIP("224.0.0.1"), true},
		{"link-local multicast IPv6", net.ParseIP("ff02::1"), true},

		// Unspecified/any — MUST block (routes to loopback on many platforms)
		{"unspecified IPv4", net.ParseIP("0.0.0.0"), true},
		{"unspecified IPv6", net.ParseIP("::"), true},

		// RFC1918 — MUST ALLOW (architect decision: internal service registries)
		{"RFC1918 10.x", net.ParseIP("10.0.0.1"), false},
		{"RFC1918 172.16.x", net.ParseIP("172.16.0.1"), false},
		{"RFC1918 192.168.x", net.ParseIP("192.168.1.1"), false},

		// Public IPs — MUST ALLOW
		{"public IPv4", net.ParseIP("8.8.8.8"), false},
		{"public IPv4 other", net.ParseIP("203.0.113.1"), false},
		{"public IPv6", net.ParseIP("2001:4860:4860::8888"), false},

		// nil IP
		{"nil IP", nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isBlockedSSRFTarget(tc.ip)
			assert.Equal(t, tc.blocked, got, "isBlockedSSRFTarget(%s)", tc.ip)
		})
	}
}

// Integration test — the SSRF-safe transport REFUSES a loopback dial.
func TestLifecycleHookExecutor_SSRFBlocksLoopback(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")
	saID := seedExecutorSA(t, s, projID, "sa@p.iam.gserviceaccount.com")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // should never reach here
	}))
	defer ts.Close()

	tokenGen := &mockTokenGenerator{accessToken: "tok", email: "hub@sa.com"}
	auditLog := newCapturingAuditLogger()
	// Use the REAL SSRF-safe client (NOT newTestExecutor) to verify blocking.
	executor := NewHTTPExecutor(s, tokenGen, auditLog, slog.Default())

	hook := makeTestHook(saID, &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionHTTP,
		Method:         "GET",
		URL:            ts.URL + "/api", // httptest URL is 127.0.0.1
		TimeoutSeconds: 5,
	})

	err := executor.Execute(context.Background(), hook, makeTestAgent(projID), "running")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SSRF protection")
	assert.Contains(t, err.Error(), "all resolved IPs")
}

// ---------------------------------------------------------------------------
// 4xx is non-retryable
// ---------------------------------------------------------------------------

func TestLifecycleHookExecutor_4xxNonRetryable(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")
	saID := seedExecutorSA(t, s, projID, "sa@p.iam.gserviceaccount.com")

	var attemptCount atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attemptCount.Add(1)
		w.WriteHeader(http.StatusForbidden) // 4xx
	}))
	defer ts.Close()

	tokenGen := &mockTokenGenerator{accessToken: "tok", email: "hub@sa.com"}
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	hook := makeTestHook(saID, &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionHTTP,
		Method:         "POST",
		URL:            ts.URL + "/register",
		Body:           `{}`,
		OnError:        store.LifecycleHookOnErrorRetry, // retry policy set...
		TimeoutSeconds: 5,
	})

	err := executor.Execute(context.Background(), hook, makeTestAgent(projID), "running")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-retryable HTTP 403")

	// Should have made only 1 attempt (4xx is non-retryable).
	assert.Equal(t, int32(1), attemptCount.Load())

	// Should have exactly 1 audit event.
	events := auditLog.getEvents()
	require.Len(t, events, 1)
	assert.False(t, events[0].Success)
	assert.Equal(t, 403, events[0].HTTPStatusCode)
}

// ---------------------------------------------------------------------------
// on_error="" defaults to single attempt
// ---------------------------------------------------------------------------

func TestLifecycleHookExecutor_EmptyOnErrorSingleAttempt(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")
	saID := seedExecutorSA(t, s, projID, "sa@p.iam.gserviceaccount.com")

	var attemptCount atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attemptCount.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	tokenGen := &mockTokenGenerator{accessToken: "tok", email: "hub@sa.com"}
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	hook := makeTestHook(saID, &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionHTTP,
		Method:         "POST",
		URL:            ts.URL + "/api",
		Body:           `{}`,
		OnError:        "", // empty -> defaults to "log" -> single attempt
		TimeoutSeconds: 5,
	})

	err := executor.Execute(context.Background(), hook, makeTestAgent(projID), "running")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 500")

	// Only 1 attempt (empty on_error defaults to "log" = single attempt).
	assert.Equal(t, int32(1), attemptCount.Load())
}

// ---------------------------------------------------------------------------
// Empty body sends nil body (GET with no body)
// ---------------------------------------------------------------------------

func TestLifecycleHookExecutor_EmptyBodySendsNil(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")
	saID := seedExecutorSA(t, s, projID, "sa@p.iam.gserviceaccount.com")

	var receivedContentLength int64
	var receivedBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedContentLength = r.ContentLength
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	tokenGen := &mockTokenGenerator{accessToken: "tok", email: "hub@sa.com"}
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	hook := makeTestHook(saID, &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionHTTP,
		Method:         "GET",
		URL:            ts.URL + "/check",
		Body:           "", // empty body -> nil request body
		TimeoutSeconds: 5,
	})

	err := executor.Execute(context.Background(), hook, makeTestAgent(projID), "running")
	require.NoError(t, err)

	assert.Empty(t, receivedBody)
	// With nil body, Content-Length should be 0 or -1 (no body).
	assert.LessOrEqual(t, receivedContentLength, int64(0))
}

// ---------------------------------------------------------------------------
// ctx cancellation during retry backoff aborts further attempts
// ---------------------------------------------------------------------------

func TestLifecycleHookExecutor_CtxCancelDuringBackoff(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "test-project")
	saID := seedExecutorSA(t, s, projID, "sa@p.iam.gserviceaccount.com")

	var attemptCount atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attemptCount.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable) // 5xx -> retryable
	}))
	defer ts.Close()

	tokenGen := &mockTokenGenerator{accessToken: "tok", email: "hub@sa.com"}
	auditLog := newCapturingAuditLogger()
	executor := newTestExecutor(s, tokenGen, auditLog, slog.Default())

	hook := makeTestHook(saID, &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionHTTP,
		Method:         "POST",
		URL:            ts.URL + "/register",
		Body:           `{}`,
		OnError:        store.LifecycleHookOnErrorRetry,
		TimeoutSeconds: 5,
	})

	// Cancel context shortly after first attempt to abort during backoff.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := executor.Execute(ctx, hook, makeTestAgent(projID), "running")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	// Should have made only 1 attempt (cancelled during backoff before 2nd).
	assert.Equal(t, int32(1), attemptCount.Load())
}

// ---------------------------------------------------------------------------
// SSRF dialer hardening — DNS-rebinding TOCTOU closure
// ---------------------------------------------------------------------------

// fakeResolver returns a fixed set of IPs for any hostname lookup.
type fakeResolver struct {
	ips []net.IPAddr
	err error
}

func (r *fakeResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.ips, nil
}

// capturingDialer records the addr passed to DialContext, then delegates to a
// real dialer. This lets us verify the dialer is called with an IP, not a host.
type capturingDialer struct {
	mu       sync.Mutex
	addrs    []string
	delegate ssrfDialer
}

func (d *capturingDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, addr)
	d.mu.Unlock()
	return d.delegate.DialContext(ctx, network, addr)
}

func (d *capturingDialer) getAddrs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.addrs))
	copy(out, d.addrs)
	return out
}

func TestSSRFDialer_DialsByValidatedIP(t *testing.T) {
	// Start a test server to accept the connection.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	// Parse the httptest server's address to get its actual IP and port.
	_, tsPort, err := net.SplitHostPort(ts.Listener.Addr().String())
	require.NoError(t, err)

	// The "allowed" IP is the actual httptest server IP (127.0.0.1 in practice,
	// but we want the SSRF-safe dialer to see it as an allowed IP for this test).
	// We use 10.0.0.1 as the "resolved" IP (RFC1918, allowed) and route the
	// actual dial back to the httptest server via capturingDialer.
	allowedIP := net.ParseIP("10.0.0.1")

	resolver := &fakeResolver{
		ips: []net.IPAddr{{IP: allowedIP}},
	}

	// The capturing dialer wraps a real dialer but rewrites the addr to the
	// actual httptest server address so the connection succeeds.
	realDialer := &net.Dialer{Timeout: 5 * time.Second}
	capturing := &capturingDialer{
		delegate: &rewritingDialer{
			target: ts.Listener.Addr().String(),
			inner:  realDialer,
		},
	}

	client := newSSRFSafeClientWith(resolver, capturing)

	// Make a request to a "hostname" URL. The SSRF dialer should resolve
	// via fakeResolver, find 10.0.0.1 (allowed), and dial "10.0.0.1:<port>".
	resp, err := client.Get(fmt.Sprintf("http://some-host.example.com:%s/api", tsPort))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Verify the dialer was called with the IP address, not the hostname.
	addrs := capturing.getAddrs()
	require.Len(t, addrs, 1)
	assert.Equal(t, net.JoinHostPort(allowedIP.String(), tsPort), addrs[0],
		"dialer must be called with the validated IP, not the hostname")
}

func TestSSRFDialer_AllBlockedIPsRefused(t *testing.T) {
	// A resolver that returns only blocked IPs (loopback + link-local).
	resolver := &fakeResolver{
		ips: []net.IPAddr{
			{IP: net.ParseIP("127.0.0.1")},
			{IP: net.ParseIP("::1")},
			{IP: net.ParseIP("169.254.169.254")},
		},
	}

	realDialer := &net.Dialer{Timeout: 5 * time.Second}
	client := newSSRFSafeClientWith(resolver, realDialer)

	_, err := client.Get("http://evil-host.example.com:8080/steal")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SSRF protection")
	assert.Contains(t, err.Error(), "all resolved IPs")
}

func TestSSRFDialer_MixedIPsDialsFirstAllowed(t *testing.T) {
	// Resolver returns a blocked IP first, then an allowed IP.
	allowedIP := net.ParseIP("10.0.0.5")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	_, tsPort, err := net.SplitHostPort(ts.Listener.Addr().String())
	require.NoError(t, err)

	resolver := &fakeResolver{
		ips: []net.IPAddr{
			{IP: net.ParseIP("127.0.0.1")}, // blocked
			{IP: allowedIP},                  // allowed — should be dialed
		},
	}

	realDialer := &net.Dialer{Timeout: 5 * time.Second}
	capturing := &capturingDialer{
		delegate: &rewritingDialer{
			target: ts.Listener.Addr().String(),
			inner:  realDialer,
		},
	}

	client := newSSRFSafeClientWith(resolver, capturing)
	resp, err := client.Get(fmt.Sprintf("http://mixed-host.example.com:%s/api", tsPort))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Verify the dialer was called with the allowed IP, skipping the blocked one.
	addrs := capturing.getAddrs()
	require.Len(t, addrs, 1)
	assert.Equal(t, net.JoinHostPort(allowedIP.String(), tsPort), addrs[0])
}

// rewritingDialer always dials a fixed target address, regardless of the
// addr argument. This lets tests verify what address the SSRF transport
// INTENDED to dial while still reaching an actual httptest server.
type rewritingDialer struct {
	target string
	inner  ssrfDialer
}

func (d *rewritingDialer) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	return d.inner.DialContext(ctx, network, d.target)
}
