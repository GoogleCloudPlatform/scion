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
	"strings"
	"testing"
)

// captureSlogDefault swaps slog's default logger for a text handler writing
// to the returned buffer, and restores the previous default on test cleanup.
func captureSlogDefault(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestCreateAuthenticatedDispatcher_AgentEndpoint pins the production wiring
// in CreateAuthenticatedDispatcher that installs the agent-endpoint override
// on the dispatcher it builds.
//
// WHY THIS EXISTS: every dispatcher test elsewhere in this package builds its
// own HTTPAgentDispatcher and calls SetAgentEndpoint directly, so deleting
// the wiring in CreateAuthenticatedDispatcher (or the field copy in
// cmd/server_foreground.go's buildHubServerConfig) would leave the whole
// suite green while a running Hub silently ignored the setting. This test
// goes through CreateAuthenticatedDispatcher, the same factory the running
// Hub uses, and checks the value actually reaches a built request — the same
// pattern used by TestDispatch_HubAgentDefaults_ProviderInstalledByServer for
// the hub agent_defaults wiring.
func TestCreateAuthenticatedDispatcher_AgentEndpoint(t *testing.T) {
	t.Run("override set", func(t *testing.T) {
		buf := captureSlogDefault(t)
		srv := &Server{
			store:       createTestStore(t),
			maintenance: NewMaintenanceState(false, ""),
			config: ServerConfig{
				HubEndpoint:   "https://hub.example.com",
				AgentEndpoint: "http://192.0.2.10:8080",
			},
		}
		d := srv.CreateAuthenticatedDispatcher()

		req, err := d.buildCreateRequest(context.Background(), hubDefaultsDispatchAgent(), "test")
		if err != nil {
			t.Fatalf("buildCreateRequest: %v", err)
		}
		if req.HubEndpoint != "http://192.0.2.10:8080" {
			t.Errorf("a dispatch from the production dispatcher carried HubEndpoint %q, want the agent-endpoint override %q",
				req.HubEndpoint, "http://192.0.2.10:8080")
		}

		logged := buf.String()
		if !strings.Contains(logged, "level=INFO") || !strings.Contains(logged, "agent_endpoint=http://192.0.2.10:8080") {
			t.Errorf("expected an Info line naming agent_endpoint when the override is configured, got log output: %s", logged)
		}
		if !strings.Contains(logged, "hub_endpoint=https://hub.example.com") {
			t.Errorf("expected the Info line to also name hub_endpoint (the value actually used), got log output: %s", logged)
		}
	})

	t.Run("override unset", func(t *testing.T) {
		buf := captureSlogDefault(t)
		srv := &Server{
			store:       createTestStore(t),
			maintenance: NewMaintenanceState(false, ""),
			config: ServerConfig{
				HubEndpoint: "https://hub.example.com",
			},
		}
		d := srv.CreateAuthenticatedDispatcher()

		req, err := d.buildCreateRequest(context.Background(), hubDefaultsDispatchAgent(), "test")
		if err != nil {
			t.Fatalf("buildCreateRequest: %v", err)
		}
		if req.HubEndpoint != "https://hub.example.com" {
			t.Errorf("a dispatch from the production dispatcher carried HubEndpoint %q, want the regular hub endpoint %q",
				req.HubEndpoint, "https://hub.example.com")
		}

		if logged := buf.String(); strings.Contains(logged, "agent_endpoint=") {
			t.Errorf("expected no agent_endpoint log line when the override is unset, got log output: %s", logged)
		}
	})
}
