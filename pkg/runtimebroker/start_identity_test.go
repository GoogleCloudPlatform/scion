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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// The profile, harness-config and runtime an existing agent starts with come
// from the broker's own records (image provenance, scion-agent.json) and the
// Hub's start request, never from agent-info.json in the agent home. These
// tests rewrite agent-info.json and check that nothing the broker uses
// changes.

// rewriteHomeAgentInfo replaces agentName's agent-info.json with one naming
// profile, harnessConfig and runtimeName.
func rewriteHomeAgentInfo(t *testing.T, dotScion, agentName, profile, harnessConfig, runtimeName string) {
	t.Helper()
	home := config.GetAgentHomePath(dotScion, agentName)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"name": "` + agentName + `", "profile": "` + profile + `", "harnessConfig": "` + harnessConfig + `", "runtime": "` + runtimeName + `"}`
	if err := os.WriteFile(filepath.Join(home, "agent-info.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestStartIdentity_RewrittenAgentInfoChangesNothing: for a provisioned agent
// (profile "prov" -> docker), an agent-info.json naming the Kubernetes
// profile, another harness-config and the Kubernetes runtime changes
// neither the runtime nor the profile of a start, a restart, or a wake (a
// resumed start); the provisioned profile also outranks a Hub-sent one.
func TestStartIdentity_RewrittenAgentInfoChangesNothing(t *testing.T) {
	for _, tc := range []struct{ name, op, body string }{
		{name: "start", op: "start", body: `{"projectPath": %q}`},
		{name: "restart", op: "restart", body: `{"projectPath": %q}`},
		{name: "wake", op: "start", body: `{"projectPath": %q, "resume": true}`},
		{name: "start with a hub profile", op: "start", body: `{"projectPath": %q, "profile": "saved"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, mgr, auxCalls, dotScion := handlerProfileFixture(t, `{"version": 1, "profile": "prov"}`)
			rewriteHomeAgentInfo(t, dotScion, "saved-profile-agent", "saved", "rewritten-hc", "kubernetes")
			w := postAgentOp(t, srv, tc.op, fmt.Sprintf(tc.body, dotScion))
			if w.Code != http.StatusAccepted {
				t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
			}
			if *auxCalls != 0 || mgr.startCalls != 1 {
				t.Errorf("expected the provisioned docker runtime to start the agent, got aux=%d start=%d", *auxCalls, mgr.startCalls)
			}
			if got := mgr.LastStartOpts().Profile; got != "prov" {
				t.Errorf("start profile = %q, want the provisioned profile prov", got)
			}
		})
	}
}

// TestStartIdentity_OlderHubLegacyAgentRefused: an agent provisioned before
// image provenance was recorded, started or restarted by a Hub that sends no
// profile, is refused with a 409 naming the remedy. agent-info.json, which
// names a profile, is not a substitute; nothing is stopped or started.
func TestStartIdentity_OlderHubLegacyAgentRefused(t *testing.T) {
	for _, op := range []string{"start", "restart"} {
		t.Run(op, func(t *testing.T) {
			srv, mgr, auxCalls, dotScion := handlerProfileFixture(t, "")
			rewriteHomeAgentInfo(t, dotScion, "saved-profile-agent", "prov", "rewritten-hc", "docker")
			w := postAgentOp(t, srv, op, fmt.Sprintf(`{"projectPath": %q}`, dotScion))
			want := `agent \"saved-profile-agent\" has no broker-side record of its settings profile (it was provisioned by an older version) and the hub sent no profile: re-provision the agent (scion reincarnate, or delete and re-create it)`
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), want) {
				t.Fatalf("expected 409 %s, got %d: %s", want, w.Code, w.Body.String())
			}
			if mgr.stopCalls != 0 || mgr.startCalls != 0 || *auxCalls != 0 {
				t.Errorf("expected no Stop/Start/runtime resolution, got stop=%d start=%d aux=%d", mgr.stopCalls, mgr.startCalls, *auxCalls)
			}
		})
	}
}

// TestStartIdentity_PolicyCheckUsesRecordedHarnessConfig: the early
// harness-config policy check on restart evaluates the harness-config in the
// agent's broker-side scion-agent.json. An agent-info.json naming another
// harness-config neither gets a container-script harness-config past the
// check nor gets a declarative one refused.
func TestStartIdentity_PolicyCheckUsesRecordedHarnessConfig(t *testing.T) {
	t.Run("recorded container-script is refused", func(t *testing.T) {
		srv, mgr, projectScion := restartGateEnv(t, false, scriptedHarnessYAML)
		writeHarnessConfig(t, projectScion, "nice-hc", declarativeHarnessYAML)
		rewriteHomeAgentInfo(t, projectScion, "rs-agent", "", "nice-hc", "")
		code, body := postAgentAction(t, srv, "rs-agent", "restart", `{}`)
		assertPolicyRefusal(t, code, body, "rhc")
		if mgr.StopCalls() != 0 || mgr.StartCalls() != 0 {
			t.Errorf("restart refusal must precede stop and start (stops=%d starts=%d)", mgr.StopCalls(), mgr.StartCalls())
		}
	})
	t.Run("recorded declarative passes", func(t *testing.T) {
		srv, mgr, projectScion := restartGateEnv(t, false, declarativeHarnessYAML)
		writeHarnessConfig(t, projectScion, "evil-hc", scriptedHarnessYAML)
		rewriteHomeAgentInfo(t, projectScion, "rs-agent", "", "evil-hc", "")
		code, body := postAgentAction(t, srv, "rs-agent", "restart", `{}`)
		if code != http.StatusAccepted || mgr.StartCalls() != 1 {
			t.Fatalf("expected 202 and one Start, got %d (starts=%d): %s", code, mgr.StartCalls(), body)
		}
	})
}

// TestStartIdentity_OwnRuntimeUsesProvisionedProfile: the runtime operations
// on an existing agent (stop, delete, status, logs, exec) select is resolved
// from the provisioned profile. An agent-info.json naming another profile
// does not move them, and an agent without image provenance gets no own
// runtime (every registered runtime is searched) even when its
// agent-info.json names a profile.
func TestStartIdentity_OwnRuntimeUsesProvisionedProfile(t *testing.T) {
	t.Run("provisioned", func(t *testing.T) {
		f := newOwnRTFixture(t, "agents", true)
		rewriteHomeAgentInfo(t, f.projectDir, ownRTAgent, "base", "rewritten-hc", "docker")
		own := agentOwnRuntimeFrom(f.srv.ensureAgentOwnRuntime(context.Background(), ownRTAgent, ownRTProjectID, ""))
		if own == nil || own.profile != "agents" {
			t.Fatalf("own runtime = %+v, want the provisioned profile agents", own)
		}
	})
	t.Run("no image provenance", func(t *testing.T) {
		f := newOwnRTFixture(t, "", true)
		rewriteHomeAgentInfo(t, f.projectDir, ownRTAgent, "agents", "rewritten-hc", "kubernetes")
		if own := agentOwnRuntimeFrom(f.srv.ensureAgentOwnRuntime(context.Background(), ownRTAgent, ownRTProjectID, "")); own != nil {
			t.Fatalf("own runtime = %+v, want none for an agent without image provenance", own)
		}
	})
}
