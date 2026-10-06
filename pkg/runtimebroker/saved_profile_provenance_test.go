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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// writeProvisionedProfile writes the agent's broker-side image provenance
// with profile as its provisioned profile.
func writeProvisionedProfile(t *testing.T, dotScionDir, agentName, profile string) {
	t.Helper()
	agentDir := config.GetAgentDir(dotScionDir, agentName, false)
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"version":1,"profile":"` + profile + `"}`)
	if err := os.WriteFile(filepath.Join(agentDir, config.ImageProvenanceFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// assertProvisionedProfileUnresolved checks the 503 for an agent whose
// provisioned profile does not resolve: the usual unresolved-profile
// response, naming the profile as the provisioned one, with no
// recorded-runtime fallback logged.
func assertProvisionedProfileUnresolved(t *testing.T, tc unresolvedCase, w *httptest.ResponseRecorder, logs *lockedBuffer, projectPath string) {
	t.Helper()
	assertSavedProfileUnresolved(t, tc, w, logs, projectPath)
	body := w.Body.String()
	for _, want := range []string{"provisioned with", "re-provision the agent"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
	if strings.Contains(logs.String(), fallbackLogMsg) {
		t.Errorf("recorded-runtime fallback logged for an agent with image provenance:\n%s", logs.String())
	}
}

// TestStartAgent_UnresolvableProvisionedProfileNoRecordedRuntimeFallback: an
// agent with image provenance whose provisioned profile does not resolve
// gets the 503 even when agent-info.json records the broker's default
// runtime; agent-info.json is not an input to its runtime selection. (The
// legacy-agent fallback is covered by
// TestStartAgent_UnresolvableSavedProfileRecordedRuntime.)
func TestStartAgent_UnresolvableProvisionedProfileNoRecordedRuntimeFallback(t *testing.T) {
	for _, tc := range unresolvedCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			logs := captureLifecycleLog(f.srv)
			const name = "provisioned-profile-agent"
			writeSavedAgentInfo(t, f.projectPath, name, "vanished", "docker")
			writeProvisionedProfile(t, f.projectPath, name, "vanished")
			tc.apply(t, f)

			w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/start", map[string]any{
				"projectPath": f.projectPath,
			})
			assertProvisionedProfileUnresolved(t, tc, w, logs, f.projectPath)
			if f.defaultMgr.StartCalls() != 0 {
				t.Errorf("default runtime Start calls = %d, want 0", f.defaultMgr.StartCalls())
			}
			if runs, _ := f.k8sRun(); runs != 0 {
				t.Errorf("kubernetes runtime runs = %d, want 0", runs)
			}
		})
	}
}

// TestRestartAgent_UnresolvableProvisionedProfileNoRecordedRuntimeFallback is
// the restart counterpart: the 503 is returned before the agent is stopped
// or started.
func TestRestartAgent_UnresolvableProvisionedProfileNoRecordedRuntimeFallback(t *testing.T) {
	f := newLifecycleFixture(t)
	logs := captureLifecycleLog(f.srv)
	const name = "restart-provisioned-profile"
	writeSavedAgentInfo(t, f.projectPath, name, "vanished", "docker")
	writeProvisionedProfile(t, f.projectPath, name, "vanished")
	f.defaultMgr.agents = append(f.defaultMgr.agents, lifecycleAgent(name, f.projectPath, ""))

	w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/restart", map[string]any{})
	assertProvisionedProfileUnresolved(t, unresolvedCases()[0], w, logs, f.projectPath)
	if f.defaultMgr.StartCalls() != 0 {
		t.Errorf("default runtime Start calls = %d, want 0", f.defaultMgr.StartCalls())
	}
}

func TestExistingAgentProfileResolution(t *testing.T) {
	for _, tt := range []struct {
		profile     string
		provisioned bool
		want        profileResolution
	}{
		{"", false, profileLenient},
		{"", true, profileLenient},
		{"p", false, profileStrict},
		{"p", true, profileStrictProvisioned},
	} {
		if got := existingAgentProfileResolution(tt.profile, tt.provisioned); got != tt.want {
			t.Errorf("existingAgentProfileResolution(%q, %v) = %v, want %v", tt.profile, tt.provisioned, got, tt.want)
		}
	}
}
