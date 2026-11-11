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

package cmd

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateServerPreflight_AgentRunScope: the hub refuses to start with
// an agent run-scope value other than off or observe, including enforce,
// or with a malformed legacy cut-off.
func TestValidateServerPreflight_AgentRunScope(t *testing.T) {
	t.Cleanup(resetServerFlags)
	tests := []struct {
		name        string
		hub         bool
		mode        string
		legacyUntil string
		wantErr     string
		wantMode    string
	}{
		{name: "unset", hub: true, wantMode: "off"},
		{name: "off", hub: true, mode: "off", wantMode: "off"},
		{name: "observe with cut-off", hub: true, mode: "observe", legacyUntil: "2026-11-01T00:00:00Z", wantMode: "observe"},
		{name: "enforce is rejected", hub: true, mode: "enforce", wantErr: "server.auth.agent_run_scope"},
		{name: "unknown value is rejected", hub: true, mode: "strict", wantErr: "server.auth.agent_run_scope"},
		{name: "case variant is rejected", hub: true, mode: "Observe", wantErr: "server.auth.agent_run_scope"},
		{name: "malformed cut-off", hub: true, mode: "observe", legacyUntil: "next week", wantErr: "server.auth.agent_run_scope_legacy_until"},
		{name: "broker-only process skips the check", mode: "enforce"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetServerFlags()
			enableHub = tt.hub
			cfg := &config.GlobalConfig{}
			cfg.Auth.AgentRunScope = tt.mode
			cfg.Auth.AgentRunScopeLegacyUntil = tt.legacyUntil
			err := validateServerPreflight(cfg)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			if tt.wantMode != "" {
				assert.Equal(t, tt.wantMode, agentRunScopeSetting(cfg).String())
			}
		})
	}
}
