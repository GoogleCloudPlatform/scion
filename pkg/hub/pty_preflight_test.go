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
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPTYPreflight_ReportsBrokerState checks that the non-upgrade PTY
// preflight runs the broker checks before returning 200 (ptone/scion#1970):
// 422 when the agent has no runtime broker, 503 when its broker is not
// connected, and 200 only when an attach could proceed.
func TestPTYPreflight_ReportsBrokerState(t *testing.T) {
	f := bypassAgentsSetup(t)
	path := "/api/v1/agents/" + f.sibling.ID + "/pty"

	rec := f.asAgent(t, http.MethodGet, path, nil, ScopeAgentLifecycle)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, "no runtime broker: %s", rec.Body.String())

	f.sibling.RuntimeBrokerID = f.broker.ID
	require.NoError(t, f.store.UpdateAgent(context.Background(), f.sibling))
	rec = f.asAgent(t, http.MethodGet, path, nil, ScopeAgentLifecycle)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "broker not connected: %s", rec.Body.String())

	require.NotNil(t, f.srv.controlChannel)
	f.srv.controlChannel.mu.Lock()
	f.srv.controlChannel.connections[f.broker.ID] = &BrokerConnection{brokerID: f.broker.ID, streams: map[string]*StreamProxy{}}
	f.srv.controlChannel.mu.Unlock()
	t.Cleanup(func() {
		f.srv.controlChannel.mu.Lock()
		delete(f.srv.controlChannel.connections, f.broker.ID)
		f.srv.controlChannel.mu.Unlock()
	})
	rec = f.asAgent(t, http.MethodGet, path, nil, ScopeAgentLifecycle)
	assert.Equal(t, http.StatusOK, rec.Code, "broker connected: %s", rec.Body.String())

	// Authorization still runs before the broker checks.
	rec = f.asAgent(t, http.MethodGet, path, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "unscoped agent: %s", rec.Body.String())
}
