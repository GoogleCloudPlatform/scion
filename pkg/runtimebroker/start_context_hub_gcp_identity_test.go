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
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// hubGCPIdentityDispatchCase mirrors the shape pkg/hub's
// TestDispatch_GCPIdentityRuntimeDefault_MatchesBrokerFixture writes: the
// GCP identity slice of the hub's real create and start dispatch output.
type hubGCPIdentityDispatchCase struct {
	Create struct {
		GCPIdentityMode string            `json:"gcpIdentityMode"`
		ResolvedEnv     map[string]string `json:"resolvedEnv"`
	} `json:"create"`
	Start struct {
		ResolvedEnv map[string]string `json:"resolvedEnv"`
	} `json:"start"`
}

// TestBuildStartContext_HubGCPIdentityDispatchFixture feeds the hub's own
// create and start dispatch output (testdata/hub_gcp_identity_dispatch.json,
// kept in sync with the hub by a pkg/hub test) into buildStartContext on the
// Kubernetes and docker runtimes:
//
//   - no identity configured: Kubernetes gets "passthrough" (no metadata
//     redirect); docker keeps "block".
//   - an explicit "block" (agent, project default or hub default), and a
//     stored project default of "assign" with no service account: refused
//     on Kubernetes with the existing message; "block" on docker.
func TestBuildStartContext_HubGCPIdentityDispatchFixture(t *testing.T) {
	path, err := filepath.Abs(filepath.Join("testdata", "hub_gcp_identity_dispatch.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]hubGCPIdentityDispatchCase
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}

	// Every fixture case must have an expectation here, and vice versa, so a
	// case added on the hub side cannot go unchecked on this side.
	wantK8s := map[string]string{
		"no_identity":                       store.GCPMetadataModePassthrough,
		"agent_block":                       "",
		"project_default_block":             "",
		"hub_default_block":                 "",
		"project_default_assign_without_sa": "",
	}
	names := make([]string, 0, len(fixture))
	for n := range fixture {
		if _, ok := wantK8s[n]; !ok {
			t.Errorf("fixture case %q has no expectation in this test", n)
		}
		names = append(names, n)
	}
	for n := range wantK8s {
		if _, ok := fixture[n]; !ok {
			t.Errorf("expected fixture case %q is missing", n)
		}
	}
	sort.Strings(names)

	for _, name := range names {
		c := fixture[name]
		paths := []struct {
			label string
			in    startContextInputs
		}{
			{"create", startContextInputs{
				Name:        "agent-" + strings.ReplaceAll(name, "_", "-"),
				Config:      createConfigFromFixture(c.Create.GCPIdentityMode),
				ResolvedEnv: c.Create.ResolvedEnv,
				Operation:   opCreate,
			}},
			{"start", startContextInputs{
				Name:        "agent-" + strings.ReplaceAll(name, "_", "-"),
				ResolvedEnv: c.Start.ResolvedEnv,
				Operation:   opHTTPStart,
			}},
		}
		for _, p := range paths {
			for _, runtimeName := range []string{"kubernetes", "docker"} {
				t.Run(name+"/"+p.label+"/"+runtimeName, func(t *testing.T) {
					cfg := DefaultServerConfig()
					cfg.StateDir = t.TempDir()
					srv := newTestServerForStartContextRuntime(t, cfg, runtimeName)
					in := p.in
					in.HTTPRequest = httptest.NewRequest("POST", "/api/v1/agents", nil)

					sc, err := srv.buildStartContext(context.Background(), in)

					want := store.GCPMetadataModeBlock
					if runtimeName == "kubernetes" {
						want = wantK8s[name]
					}
					if want == "" {
						if err == nil {
							t.Fatalf("expected an explicit block to be refused on Kubernetes, got env %v", sc.Opts.Env)
						}
						if !strings.Contains(err.Error(), "not supported on the Kubernetes runtime") ||
							!strings.Contains(err.Error(), "project or hub default") {
							t.Errorf("expected the existing Kubernetes block message, got %q", err.Error())
						}
						return
					}
					if err != nil {
						t.Fatalf("buildStartContext: %v", err)
					}
					if got := sc.Opts.Env["SCION_METADATA_MODE"]; got != want {
						t.Errorf("SCION_METADATA_MODE = %q, want %q", got, want)
					}
					_, redirected := sc.Opts.Env["GCE_METADATA_HOST"]
					if wantRedirect := want == store.GCPMetadataModeBlock; redirected != wantRedirect {
						t.Errorf("GCE_METADATA_HOST present = %v, want %v (env %v)", redirected, wantRedirect, sc.Opts.Env)
					}
				})
			}
		}
	}
}

func createConfigFromFixture(mode string) *CreateAgentConfig {
	if mode == "" {
		return &CreateAgentConfig{}
	}
	return &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{MetadataMode: mode}}
}
