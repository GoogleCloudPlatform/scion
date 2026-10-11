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
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// logLinesWithMessage returns the text-handler log lines that contain message.
func logLinesWithMessage(logs *bytes.Buffer, message string) []string {
	var lines []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, message) {
			lines = append(lines, line)
		}
	}
	return lines
}

// assertSingleLogLine asserts that exactly one log line carries message, that
// it has every key=value in fields, and that it contains none of absent.
// It returns the line.
func assertSingleLogLine(t *testing.T, logs *bytes.Buffer, message string, fields map[string]string, absent ...string) string {
	t.Helper()
	lines := logLinesWithMessage(logs, message)
	require.Len(t, lines, 1, "expected one %q line in:\n%s", message, logs.String())
	line := lines[0]
	for key, value := range fields {
		assert.Contains(t, line, " "+key+"="+value, "line should carry %s=%s", key, value)
	}
	for _, s := range absent {
		require.NotEmpty(t, s, "absent strings must be non-empty to be meaningful")
		assert.NotContains(t, line, s)
	}
	return line
}

// runMismatchEnvelope is the broker's run-mismatch 404 for a delete of
// requested; current "" omits currentRunId; details false omits all details
// (as an older broker might).
func runMismatchEnvelope(t *testing.T, requested, current string, details bool) *brokerStatusError {
	t.Helper()
	if !details {
		return brokerEnvelope(t, http.StatusNotFound, api.BrokerErrorCodeRunMismatch, nil)
	}
	d := map[string]interface{}{api.BrokerErrorDetailRunID: requested}
	if current != "" {
		d[api.BrokerErrorDetailCurrentRunID] = current
	}
	return brokerEnvelope(t, http.StatusNotFound, api.BrokerErrorCodeRunMismatch, d)
}

// newRunMismatchFixture is an agent in phase on a broker answering through
// client, with the row's run run-a and the previous runs prev, oldest
// first (ptone/scion#3097).
func newRunMismatchFixture(t *testing.T, suffix string, phase state.Phase, prev ...string) *runMismatchFixture {
	t.Helper()
	srv, s := testServer(t)
	client := &runMismatchDeleteClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}, answer: func(string) error { return nil }}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
	agent := setupBrokerAgentInPhase(t, s, suffix, phase)
	ctx := context.Background()
	for _, p := range prev {
		_, err := s.SetAgentRunID(ctx, agent.ID, p, nil)
		require.NoError(t, err)
	}
	_, err := s.SetAgentRunID(ctx, agent.ID, "run-a", nil)
	require.NoError(t, err)
	got := mustGetAgent(t, s, agent.ID)
	require.Equal(t, "run-a", got.RunID)
	if len(prev) > 0 {
		require.Equal(t, prev, got.PreviousRunIDs)
	}
	return &runMismatchFixture{srv: srv, store: s, client: client, agent: got}
}

// runMismatchDeleteClient answers each delete with answer(runID), a
// broker error before the transport's handling, which it then applies
// (deleteAgentError), as both transports do.
type runMismatchDeleteClient struct {
	*mockRuntimeBrokerClient
	mu     sync.Mutex
	answer func(runID string) error
	runs   []string
}

type runMismatchFixture struct {
	srv    *Server
	store  store.Store
	client *runMismatchDeleteClient
	agent  *store.Agent
}

func (c *runMismatchDeleteClient) DeleteAgent(_ context.Context, _, _, _, _ string, opts DeleteAgentOptions) error {
	c.mu.Lock()
	c.runs = append(c.runs, opts.RunID)
	answer := c.answer
	c.mu.Unlock()
	return deleteAgentError(answer(opts.RunID), opts.RunID)
}

func (c *runMismatchDeleteClient) deleted() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.runs...)
}

func (f *runMismatchFixture) del(t *testing.T, query string) deleteResult {
	t.Helper()
	return waitDelete(t, deleteAsync(t, f.srv, "/api/v1/agents/"+f.agent.ID+query, nil), 10*time.Second)
}
