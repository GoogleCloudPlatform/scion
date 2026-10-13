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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

type mockTokenGenerator struct {
	mu             sync.Mutex
	accessToken    string
	accessTokenErr error
	email          string
	calls          int
}

func newCapturingAuditLogger() *capturingAuditLogger {
	return &capturingAuditLogger{
		LogAuditLogger: NewLogAuditLogger("[Test]", true),
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

const landedRunRemovedWarning = "agent was deleted while it was starting; its container was removed"

// newLandedDeleteServer returns a server whose real HTTP dispatcher talks to
// a landingClient, and a stopped agent assigned to that broker.
func newLandedDeleteServer(t *testing.T) (*Server, store.Store, *store.Agent, *landingClient) {
	t.Helper()
	ctx := context.Background()
	srv, s := testServer(t)

	broker := &store.RuntimeBroker{
		ID:       tid("landed-del-broker-" + t.Name()),
		Name:     "landed-del-broker",
		Slug:     "landed-del-broker-" + tidSlugSafe(t.Name()),
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{
		ID:                     tid("landed-del-project-" + t.Name()),
		Name:                   "landed-del-project",
		Slug:                   "landed-del-project-" + tidSlugSafe(t.Name()),
		DefaultRuntimeBrokerID: broker.ID,
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: broker.Status,
	}))
	// The owner is a project member, so the agent is in good standing
	// (ptone/scion#3433).
	ensureStandingRoot(t, s, project.ID, tid("landed-del-user"))
	agent := &store.Agent{
		ID:              tid("landed-del-agent-" + t.Name()),
		Name:            "landed-del-agent",
		Slug:            "landed-del-agent",
		ProjectID:       project.ID,
		OwnerID:         tid("landed-del-user"),
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseStopped),
		AppliedConfig:   &store.AgentAppliedConfig{HarnessConfig: "claude"},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	client := &landingClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}, reportRunID: true}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
	return srv, s, agent, client
}

// calledFrom reports whether the store call in progress was made, directly
// or not, by the function whose qualified name ends in suffix (for example
// ".(*Server).deleteWonAfterLanding"). The tests use it to place a fault at
// one specific read: several reads of the row run between the started write
// and the reload (the compensating-stop check among them), so counting calls
// would be fragile. A rename that stops it matching is not silent: the tests
// that inject this way require that the fault was applied (p.applied, or
// failedReload), and fail otherwise.
//
// It does not skip a fixed number of frames: which frames exist depends on
// what the compiler inlines, so a skip count could drop the frame being
// looked for. It skips only runtime.Callers itself, collects the whole
// stack (growing the buffer until it is not filled), and scans every frame.
// Inlining cannot hide the caller: runtime.CallersFrames expands inlined
// calls into their own frames, with their own function names.
func calledFrom(suffix string) bool {
	pcs := make([]uintptr, 64)
	for {
		n := runtime.Callers(1, pcs)
		if n < len(pcs) {
			pcs = pcs[:n]
			break
		}
		pcs = make([]uintptr, 2*len(pcs))
	}
	frames := runtime.CallersFrames(pcs)
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, suffix) {
			return true
		}
		if !more {
			return false
		}
	}
}

// failReloadStore fails settleLifecycleWrite's reload of the row with a
// database error that is not store.ErrNotFound.
type failReloadStore struct {
	store.Store
	failedReload atomic.Bool
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

func (p *failReloadStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if calledFrom(".(*Server).settleLifecycleWrite") {
		p.failedReload.Store(true)
		return nil, errors.New("db unavailable")
	}
	return p.Store.GetAgent(ctx, id)
}

type capturingAuditLogger struct {
	mu     sync.Mutex
	events []*LifecycleHookExecutionEvent
	// Embed the real logger so we satisfy the full interface without
	// implementing every method from scratch.
	*LogAuditLogger
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
