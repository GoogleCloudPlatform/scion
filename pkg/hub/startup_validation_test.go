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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// ptone/scion#1316 fault 3: nothing previously validated that a deployment's
// own agent_defaults.default_template / .default_harness_config resolved to
// a registered resource before the first agent-create. These tests pin
// ValidateStartupDefaults, which closes that gap by logging a clear warning
// at boot instead of leaving the operator to discover the misconfiguration
// via a create-time error naming a resource they never typed.

func TestValidateStartupDefaults_UnresolvedHarnessConfigWarns(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, _ := setupCreateAgentServer(t, disp)
	logs := captureHarnessLogs(srv)
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultHarnessConfig: "ghost-harness"})

	srv.ValidateStartupDefaults(context.Background())

	records := logs.recordsContaining("default_harness_config does not resolve")
	require.Len(t, records, 1, "an unresolvable default_harness_config must be a loud startup warning")
	name, ok := recordAttr(records[0], "harness_config")
	require.True(t, ok, "the log must name the harness-config so an operator can fix it")
	require.Equal(t, "ghost-harness", name.String())
}

func TestValidateStartupDefaults_ResolvedHarnessConfigNoWarn(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, _ := setupCreateAgentServer(t, disp)
	logs := captureHarnessLogs(srv)

	hc := &store.HarnessConfig{
		ID:          tid("hc-startup-validation-" + t.Name()),
		Name:        "claude",
		Slug:        "claude",
		Harness:     "claude",
		ContentHash: "beefcafe",
		Scope:       store.HarnessConfigScopeGlobal,
	}
	require.NoError(t, s.CreateHarnessConfig(context.Background(), hc))
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultHarnessConfig: "claude"})

	srv.ValidateStartupDefaults(context.Background())

	records := logs.recordsContaining("default_harness_config does not resolve")
	require.Empty(t, records, "a resolvable default_harness_config must not warn")
}

func TestValidateStartupDefaults_UnresolvedTemplateWarns(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, _ := setupCreateAgentServer(t, disp)
	logs := captureHarnessLogs(srv)
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultTemplate: "ghost-template"})

	srv.ValidateStartupDefaults(context.Background())

	records := logs.recordsContaining("default_template does not resolve")
	require.Len(t, records, 1, "an unresolvable default_template must be a loud startup warning")
	name, ok := recordAttr(records[0], "template")
	require.True(t, ok, "the log must name the template so an operator can fix it")
	require.Equal(t, "ghost-template", name.String())
}

func TestValidateStartupDefaults_ResolvedTemplateNoWarn(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, _ := setupCreateAgentServer(t, disp)
	logs := captureHarnessLogs(srv)

	tmpl := &store.Template{
		ID:      tid("template-startup-validation-" + t.Name()),
		Name:    "team-default",
		Slug:    "team-default",
		Harness: "claude",
		Scope:   store.TemplateScopeGlobal,
	}
	require.NoError(t, s.CreateTemplate(context.Background(), tmpl))
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultTemplate: "team-default"})

	srv.ValidateStartupDefaults(context.Background())

	records := logs.recordsContaining("default_template does not resolve")
	require.Empty(t, records, "a resolvable default_template must not warn")
}

// TestValidateStartupDefaults_EmptyDefaultsNoOp pins workstation/file-mode
// parity: hubAgentDefaults() is always the zero value there (design §3.2.4),
// so the validator must not fire any warning when both defaults are unset.
func TestValidateStartupDefaults_EmptyDefaultsNoOp(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, _ := setupCreateAgentServer(t, disp)
	logs := captureHarnessLogs(srv)

	srv.ValidateStartupDefaults(context.Background())

	require.Empty(t, logs.recordsContaining("does not resolve"),
		"no defaults configured must produce no startup warnings")
}
