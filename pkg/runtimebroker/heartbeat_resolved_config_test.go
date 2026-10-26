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
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// TestHeartbeatService_ReportsResolvedAgentConfig: each listed entry carries
// the agent config the runtime resolved (template, harness config and its
// source, image, runtime), so the Hub can record it for a create whose
// dispatch outcome was unknown (ptone/scion#4987). The values come from the
// same source as the synchronous start answer: the start answer is the
// runtime's listing of the started entry (AgentManager.Start), converted by
// AgentInfoToResponse, so for one entry the two must agree, a non-default
// runtime included. An entry with no runtime reports none.
func TestHeartbeatService_ReportsResolvedAgentConfig(t *testing.T) {
	listed := api.AgentInfo{
		Name: "resolved", ProjectID: "project-1", Phase: "running", RunID: "run-1",
		Template: "resolved-template", HarnessConfig: "resolved-hc", HarnessConfigSource: "hub-hydrated",
		Image: "registry.example/resolved:v1", Runtime: "kubernetes",
	}
	client := &mockRuntimeBrokerService{}
	manager := &heartbeatMockManager{
		agents: []api.AgentInfo{listed, {Name: "no-runtime", ProjectID: "project-1", Phase: "running", RunID: "run-2"}},
	}
	svc := NewHeartbeatService(client, "test-host", time.Hour, manager, nil, slog.Default())
	if err := svc.ForceHeartbeat(context.Background()); err != nil {
		t.Fatalf("ForceHeartbeat failed: %v", err)
	}
	calls := client.getHeartbeatCalls()
	if len(calls) != 1 {
		t.Fatalf("Expected 1 heartbeat call, got %d", len(calls))
	}
	var got, noRuntime *hubclient.AgentHeartbeat
	for _, p := range calls[0].Heartbeat.Projects {
		for i := range p.Agents {
			switch p.Agents[i].Slug {
			case "resolved":
				got = &p.Agents[i]
			case "no-runtime":
				noRuntime = &p.Agents[i]
			}
		}
	}
	if got == nil || noRuntime == nil {
		t.Fatal("a listed agent was not reported")
	}
	want := hubclient.AgentHeartbeat{
		Template: "resolved-template", HarnessConfig: "resolved-hc", HarnessConfigSource: "hub-hydrated",
		Image: "registry.example/resolved:v1", Runtime: "kubernetes",
	}
	if got.Template != want.Template || got.HarnessConfig != want.HarnessConfig ||
		got.HarnessConfigSource != want.HarnessConfigSource || got.Image != want.Image || got.Runtime != want.Runtime {
		t.Errorf("reported resolved config = {template %q, harnessConfig %q, source %q, image %q, runtime %q}, want {%q, %q, %q, %q, %q}",
			got.Template, got.HarnessConfig, got.HarnessConfigSource, got.Image, got.Runtime,
			want.Template, want.HarnessConfig, want.HarnessConfigSource, want.Image, want.Runtime)
	}
	// The synchronous start answer for the same entry.
	answer := AgentInfoToResponse(listed)
	if answer.RuntimeType != got.Runtime || answer.Template != got.Template || answer.HarnessConfig != got.HarnessConfig ||
		answer.HarnessConfigSource != got.HarnessConfigSource || answer.Image != got.Image {
		t.Errorf("heartbeat and start answer disagree: answer {runtime %q, template %q, harnessConfig %q, source %q, image %q}, heartbeat {%q, %q, %q, %q, %q}",
			answer.RuntimeType, answer.Template, answer.HarnessConfig, answer.HarnessConfigSource, answer.Image,
			got.Runtime, got.Template, got.HarnessConfig, got.HarnessConfigSource, got.Image)
	}
	if noRuntime.Runtime != "" {
		t.Errorf("an entry with no runtime reported runtime %q, want empty", noRuntime.Runtime)
	}
}
