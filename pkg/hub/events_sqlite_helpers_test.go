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
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/jackc/pgx/v5/pgconn"
)

// noopDispatcher is a minimal AgentDispatcher that does nothing.
type noopDispatcher struct{}

// recExec records Exec calls so publish-path tests can assert the SQL and
// arguments without a real database.
type recExec struct {
	mu    sync.Mutex
	calls []recCall
}

func requirePostgres(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("SCION_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set SCION_TEST_POSTGRES_DSN to run Postgres LISTEN/NOTIFY integration tests")
	}
	return dsn
}

func (noopDispatcher) DispatchAgentCreate(_ context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	agent.Phase = string(state.PhaseRunning)
	return nil, nil
}
func (noopDispatcher) DispatchAgentProvision(_ context.Context, _ *store.Agent) error   { return nil }
func (noopDispatcher) DispatchAgentReprovision(_ context.Context, _ *store.Agent) error { return nil }
func (noopDispatcher) DispatchAgentStart(_ context.Context, _ *store.Agent, _ string, _ bool) error {
	return nil
}
func (noopDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error      { return nil }
func (noopDispatcher) DispatchAgentRestart(_ context.Context, _ *store.Agent) error   { return nil }
func (noopDispatcher) DispatchAgentResetAuth(_ context.Context, _ *store.Agent) error { return nil }
func (noopDispatcher) DispatchAgentDelete(_ context.Context, _ *store.Agent, _, _, _ bool, _ time.Time) error {
	return nil
}
func (noopDispatcher) DispatchAgentMessage(_ context.Context, _ *store.Agent, _ string, _ bool, _ *messages.StructuredMessage) error {
	return nil
}
func (noopDispatcher) DispatchCheckAgentPrompt(_ context.Context, _ *store.Agent) (bool, error) {
	return false, nil
}
func (noopDispatcher) DispatchAgentCreateWithGather(_ context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	agent.Phase = string(state.PhaseRunning)
	return nil, nil
}
func (noopDispatcher) DispatchAgentLogs(_ context.Context, _ *store.Agent, _ int) (string, error) {
	return "", nil
}
func (noopDispatcher) DispatchAgentExec(_ context.Context, _ *store.Agent, _ []string, _ int) (string, int, error) {
	return "", 0, nil
}
func (noopDispatcher) DispatchFinalizeEnv(_ context.Context, _ *store.Agent, _ map[string]string) (*CreateDispatchResult, error) {
	return nil, nil
}

func (e *recExec) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, recCall{sql: sql, args: args})
	return pgconn.CommandTag{}, nil
}

func (e *recExec) notifyCalls() []recCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []recCall
	for _, c := range e.calls {
		if strings.Contains(c.sql, "pg_notify") {
			out = append(out, c)
		}
	}
	return out
}

func (e *recExec) inserts() []recCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []recCall
	for _, c := range e.calls {
		if strings.Contains(c.sql, "INSERT INTO scion_event_payloads") {
			out = append(out, c)
		}
	}
	return out
}

type recCall struct {
	sql  string
	args []any
}
