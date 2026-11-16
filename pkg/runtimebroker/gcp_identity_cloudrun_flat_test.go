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

package runtimebroker

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

// The token probe: a dispatch carries a hub-issued agent token
// (flatProbeHubToken) and a different SCION_AUTH_TOKEN in its env
// (flatProbeOtherToken). buildStartContext handles both in its agent token
// step, which runs after the Cloud Run identity resolution and before the
// metadata env, ADC and host credential steps, and writes one lifecycle log
// line for each (flatProbeTokenLines; the second only with Debug on). A
// refusal that leaves both lines out of the log returned before that step.
const (
	flatProbeHubToken   = "probe-token"
	flatProbeOtherToken = "probe-other"
)

var flatProbeTokenLines = []string{
	"hub-issued agent token present",
	"SCION_AUTH_TOKEN in the start env is not the hub-issued token",
}

// probeFlatTokenStep points srv's lifecycle log at the returned buffer at
// Debug level, with Debug on.
func probeFlatTokenStep(srv *Server) *bytes.Buffer {
	var buf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv.config.Debug = true
	return &buf
}

// withFlatTokenProbe returns in with the probe tokens: the hub-issued token
// in AgentToken, and the other token in the config env of a create or the
// resolved env of a start.
func withFlatTokenProbe(in startContextInputs) startContextInputs {
	in.AgentToken = flatProbeHubToken
	if in.Operation == opCreate {
		if in.Config == nil {
			in.Config = &CreateAgentConfig{}
		}
		in.Config.Env = append(in.Config.Env, "SCION_AUTH_TOKEN="+flatProbeOtherToken)
		return in
	}
	env := make(map[string]string, len(in.ResolvedEnv)+1)
	for k, v := range in.ResolvedEnv {
		env[k] = v
	}
	env["SCION_AUTH_TOKEN"] = flatProbeOtherToken
	in.ResolvedEnv = env
	return in
}

// requireNoFlatTokenStep fails when logs hold a line of the agent token
// step.
func requireNoFlatTokenStep(t *testing.T, logs *bytes.Buffer) {
	t.Helper()
	for _, line := range flatProbeTokenLines {
		if strings.Contains(logs.String(), line) {
			t.Errorf("the agent token step ran before the refusal: the log has %q", line)
		}
	}
}

// The probe's control: with the experiment off, a flat Cloud Run dispatch
// with no GCP identity request passes the identity resolution and reaches the
// agent token step, which writes both probe lines, so their absence in the
// refusal tests shows a return before that step.
func TestBuildStartContext_CloudRunFlatTokenProbeControl(t *testing.T) {
	srv := newFlatCloudRunStartServer(t)
	logs := probeFlatTokenStep(srv)
	newTestGlobalSettings(t, testCloudRunAllowListGlobalSettingsYAML)
	projectDir := newTestProjectSettings(t, testCloudRunProjectSettingsYAML)
	in := withFlatTokenProbe(startContextInputs{
		Name:        "agent-cr-flat-probe",
		ProjectPath: projectDir,
		Config:      &CreateAgentConfig{},
		HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	// Only the log is checked: whatever the later steps return, the token
	// step has run by then.
	_, _ = srv.buildStartContext(context.Background(), in)
	for _, line := range flatProbeTokenLines {
		if !strings.Contains(logs.String(), line) {
			t.Errorf("the log has no %q, so the probe shows nothing: %s", line, logs.String())
		}
	}
	for _, tok := range []string{flatProbeHubToken, flatProbeOtherToken} {
		if strings.Contains(logs.String(), tok) {
			t.Errorf("the log names a token value %q", tok)
		}
	}
}

// On a flat Cloud Run instance, confined passthrough (RequireLocalRuntime /
// hub-default passthrough) downgrades to block and therefore still requires the
// hub's native marker (refusing unmarked dispatches before the agent token
// step), whereas unconfined passthrough (explicit passthrough on create or
// hub-resolved passthrough on start) runs as runtime_target.service_account
// with SCION_METADATA_MODE=passthrough and requires no native marker.
func TestBuildStartContext_CloudRunFlatInstancePassthroughForms(t *testing.T) {
	refusedConfined := []struct {
		name string
		in   func(projectDir string) startContextInputs
	}{
		{
			// An unset mode the hub resolved to its hub-default passthrough
			// grant (marked RequireLocalRuntime) without native marker.
			name: "unmarked hub-default passthrough on create",
			in: func(projectDir string) startContextInputs {
				return startContextInputs{
					Name:        "agent-cr-flat-pt",
					ProjectPath: projectDir,
					Config: &CreateAgentConfig{
						GCPIdentity:      &GCPIdentityConfig{MetadataMode: "passthrough", RequireLocalRuntime: true},
						HubAgentDefaults: crGateDefaults(),
					},
					HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
					Operation:   opCreate,
				}
			},
		},
		{
			// The hub-default passthrough grant reaches a start as the
			// hub's resolved mode with SCION_METADATA_MODE_SOURCE=hub and
			// SCION_METADATA_REQUIRE_LOCAL_RUNTIME=true without native marker.
			name: "unmarked hub-default passthrough on start",
			in: func(projectDir string) startContextInputs {
				return startContextInputs{
					Name:        "agent-cr-flat-pt",
					ProjectPath: projectDir,
					ResolvedEnv: map[string]string{
						"SCION_METADATA_MODE":                  "passthrough",
						"SCION_METADATA_MODE_SOURCE":           "hub",
						"SCION_METADATA_REQUIRE_LOCAL_RUNTIME": "true",
					},
					HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents/x/start", nil),
					Operation:   opHTTPStart,
				}
			},
		},
	}
	for _, tc := range refusedConfined {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFlatCloudRunStartServer(t)
			logs := probeFlatTokenStep(srv)
			newTestGlobalSettings(t, testCloudRunAllowListGlobalSettingsYAML)
			projectDir := newTestProjectSettings(t, testCloudRunProjectSettingsYAML)
			sc, err := srv.buildStartContext(crGateCtx(), withFlatTokenProbe(tc.in(projectDir)))
			if sc != nil {
				t.Fatalf("expected a refusal, got a start context with CloudRunIdentity %+v", sc.Opts.CloudRunIdentity)
			}
			sce := requireStartContextError(t, err)
			if sce.Status < 400 || sce.Status > 499 {
				t.Errorf("status = %d, want a 4xx", sce.Status)
			}
			if !strings.Contains(sce.Message, "agent-cr-flat-pt") || !strings.Contains(sce.Message, "Cloud Run") {
				t.Errorf("message %q must name the agent and Cloud Run", sce.Message)
			}
			if strings.Contains(sce.Message, crAssignedSA) || strings.Contains(sce.Message, "@") {
				t.Errorf("message %q names an account", sce.Message)
			}
			requireNoFlatTokenStep(t, logs)
		})
	}

	allowedUnconfined := []struct {
		name string
		in   func(projectDir string) startContextInputs
	}{
		{
			name: "explicit passthrough on create without marker",
			in: func(projectDir string) startContextInputs {
				return startContextInputs{
					Name:        "agent-cr-flat-pt",
					ProjectPath: projectDir,
					Config: &CreateAgentConfig{
						GCPIdentity:      &GCPIdentityConfig{MetadataMode: "passthrough"},
						HubAgentDefaults: crGateDefaults(),
					},
					HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
					Operation:   opCreate,
				}
			},
		},
		{
			name: "project-default passthrough on start without marker",
			in: func(projectDir string) startContextInputs {
				return startContextInputs{
					Name:        "agent-cr-flat-pt",
					ProjectPath: projectDir,
					ResolvedEnv: map[string]string{
						"SCION_METADATA_MODE":        "passthrough",
						"SCION_METADATA_MODE_SOURCE": "hub",
					},
					HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents/x/start", nil),
					Operation:   opHTTPStart,
				}
			},
		},
	}
	for _, tc := range allowedUnconfined {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFlatCloudRunStartServer(t)
			newTestGlobalSettings(t, testCloudRunAllowListGlobalSettingsYAML)
			projectDir := newTestProjectSettings(t, testCloudRunProjectSettingsYAML)
			sc, err := srv.buildStartContext(crGateCtx(), tc.in(projectDir))
			if err != nil {
				t.Fatalf("buildStartContext: %v", err)
			}
			if sc.Opts.CloudRunIdentity == nil || sc.Opts.CloudRunIdentity.ServiceAccount != flatCRTestInstanceSA || sc.Opts.CloudRunIdentity.Mode != "passthrough" {
				t.Fatalf("CloudRunIdentity = %+v, want {%q passthrough}", sc.Opts.CloudRunIdentity, flatCRTestInstanceSA)
			}
			if got := sc.Opts.Env["SCION_METADATA_MODE"]; got != "passthrough" {
				t.Errorf("SCION_METADATA_MODE = %q, want passthrough", got)
			}
		})
	}
}
