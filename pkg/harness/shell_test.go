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

package harness

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestShellHarness(t *testing.T) {
	h := New(config.ShellHarnessConfigName)
	shellH, ok := h.(*Shell)
	if !ok {
		t.Fatalf("harness.New(%q) returned %T, want *Shell", config.ShellHarnessConfigName, h)
	}
	if shellH.Name() != config.ShellHarnessConfigName {
		t.Errorf("Name() = %q, want %q", shellH.Name(), config.ShellHarnessConfigName)
	}

	// GetCommand always runs the respawning login shell, ignoring task/args.
	cmd := shellH.GetCommand("do something evil", false, []string{"--flag"})
	wantCmd := []string{"sh", "-c", "while :; do bash -l; done"}
	if len(cmd) != len(wantCmd) || cmd[0] != wantCmd[0] || cmd[1] != wantCmd[1] || cmd[2] != wantCmd[2] {
		t.Errorf("GetCommand() = %v, want %v", cmd, wantCmd)
	}

	// ReadySpec uses ReadyStrategyIdle.
	rs := shellH.ReadySpec()
	if rs.Strategy != config.ReadyStrategyIdle {
		t.Errorf("ReadySpec().Strategy = %q, want %q", rs.Strategy, config.ReadyStrategyIdle)
	}
	if rs.Timeout != 0 {
		t.Errorf("ReadySpec().Timeout = %v, want 0", rs.Timeout)
	}

	// DeliverySpec is disabled (Delivery: none).
	ds := shellH.DeliverySpec()
	if !ds.Disabled {
		t.Error("DeliverySpec().Disabled = false, want true")
	}

	// Capabilities: Resume=yes, LLM capabilities=no.
	caps := shellH.AdvancedCapabilities()
	if caps.Resume.Support != api.SupportYes {
		t.Errorf("Resume.Support = %q, want %q", caps.Resume.Support, api.SupportYes)
	}
	if caps.Auth.APIKey.Support != api.SupportNo || caps.Auth.AuthFile.Support != api.SupportNo ||
		caps.Auth.OAuthToken.Support != api.SupportNo || caps.Auth.VertexAI.Support != api.SupportNo {
		t.Errorf("expected all Auth capabilities SupportNo, got %+v", caps.Auth)
	}
	if caps.Limits.MaxTurns.Support != api.SupportNo || caps.Limits.MaxModelCalls.Support != api.SupportNo {
		t.Errorf("expected MaxTurns/MaxModelCalls SupportNo, got %+v", caps.Limits)
	}

	// ResolveAuth uses NoAuth semantics: no env vars or files injected even when credentials are supplied.
	resolved, err := shellH.ResolveAuth(api.AuthConfig{
		GoogleCloudProject: "my-proj",
		EnvVars: map[string]string{
			"ANTHROPIC_API_KEY": "sk-ant-secret",
			"GEMINI_API_KEY":    "gemini-secret",
			"OPENAI_API_KEY":    "sk-openai-secret",
		},
	})
	if err != nil {
		t.Fatalf("ResolveAuth: %v", err)
	}
	if resolved.Method != "none" {
		t.Errorf("ResolveAuth.Method = %q, want %q", resolved.Method, "none")
	}
	if len(resolved.EnvVars) != 0 || len(resolved.Files) != 0 {
		t.Errorf("ResolveAuth must not inject credentials, got env=%v files=%v", resolved.EnvVars, resolved.Files)
	}

	// Resolve always returns Shell for "shell" and ValidateEntryCommand succeeds with empty task/args.
	res, err := Resolve(context.Background(), ResolveOptions{Name: config.ShellHarnessConfigName})
	if err != nil {
		t.Fatalf("Resolve(shell): %v", err)
	}
	if _, ok := res.Harness.(*Shell); !ok {
		t.Errorf("Resolve(shell).Harness = %T, want *Shell", res.Harness)
	}
	if err := ValidateEntryCommand(config.HarnessConfigSourceBuiltin, res.Config, "", false, nil); err != nil {
		t.Errorf("ValidateEntryCommand(shell) = %v, want nil", err)
	}
}
