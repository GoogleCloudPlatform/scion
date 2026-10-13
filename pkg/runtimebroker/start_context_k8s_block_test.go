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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestBuildStartContext_KubernetesBlockUnconfiguredUsesNamespaceDefault
// covers ptone/scion#4034 with no kubernetes_block_service_account
// configured: "block" on Kubernetes is accepted, not refused, and the pod
// runs as the namespace's default ServiceAccount (an empty block
// ServiceAccountName). It is never downgraded to passthrough. Both spellings
// the codebase accepts for the Kubernetes runtime name are covered.
func TestBuildStartContext_KubernetesBlockUnconfiguredUsesNamespaceDefault(t *testing.T) {
	for _, runtimeName := range []string{"kubernetes", "k8s"} {
		t.Run(runtimeName, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContextRuntime(t, cfg, runtimeName)

			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name: "agent-k8s-block",
				Config: &CreateAgentConfig{
					GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"},
				},
				HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
				Operation:   opCreate,
			})
			if err != nil {
				t.Fatalf("expected block on runtime %q to be accepted, got %v", runtimeName, err)
			}
			assertKubernetesBlockContext(t, sc, "")
			want := dispatchProfileSelection{RuntimeEntryName: runtimeName}
			if sc.BlockSelection == nil || *sc.BlockSelection != want {
				t.Errorf("BlockSelection = %+v, want %+v", sc.BlockSelection, want)
			}
		})
	}
}

// assertKubernetesBlockContext checks the start context of a Kubernetes
// "block" dispatch: the block env (never passthrough), the block identity
// with wantKSA (empty for the namespace default), no "assign" ServiceAccount
// and no assign identity env.
func assertKubernetesBlockContext(t *testing.T, sc *startContext, wantKSA string) {
	t.Helper()
	if got := sc.Opts.Env["SCION_METADATA_MODE"]; got != "block" {
		t.Errorf("SCION_METADATA_MODE = %q, want block", got)
	}
	if sc.Opts.KubernetesBlockIdentity == nil {
		t.Fatalf("expected KubernetesBlockIdentity to be set")
	}
	if got := sc.Opts.KubernetesBlockIdentity.ServiceAccountName; got != wantKSA {
		t.Errorf("block ServiceAccountName = %q, want %q", got, wantKSA)
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "" {
		t.Errorf("expected no assign ServiceAccount, got %q", got)
	}
	if sc.AssignSelection != nil {
		t.Errorf("expected no AssignSelection, got %+v", sc.AssignSelection)
	}
	for _, key := range []string{"SCION_METADATA_SA_EMAIL", "SCION_METADATA_PROJECT_ID"} {
		if v, ok := sc.Opts.Env[key]; ok {
			t.Errorf("expected no %s for block, got %q", key, v)
		}
	}
}

// testKubernetesBlockGlobalSettingsYAML is operator global settings with a
// block ServiceAccount on the "kubernetes" runtime entry (the entry a
// ForceRuntime "kubernetes" test server resolves to).
const testKubernetesBlockGlobalSettingsYAML = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes:
        type: kubernetes
        kubernetes_block_service_account: scion-block
`

// TestBuildStartContext_KubernetesBlockConfiguredServiceAccount covers
// "block" from the request, from a project default and from a hub default
// (the latter two arrive as hub-supplied resolvedEnv) with a configured
// kubernetes_block_service_account: each runs as the block ServiceAccount.
func TestBuildStartContext_KubernetesBlockConfiguredServiceAccount(t *testing.T) {
	cases := []struct {
		name string
		in   startContextInputs
	}{
		{"request", startContextInputs{
			Config:    &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"}},
			Operation: opCreate,
		}},
		{"project_default_create", startContextInputs{
			Config:      &CreateAgentConfig{},
			ResolvedEnv: map[string]string{"SCION_METADATA_MODE": "block", "SCION_METADATA_MODE_SOURCE": "hub"},
			Operation:   opCreate,
		}},
		{"hub_default_start", startContextInputs{
			ResolvedEnv: map[string]string{"SCION_METADATA_MODE": "block", "SCION_METADATA_MODE_SOURCE": "hub"},
			Operation:   opHTTPStart,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
			newTestGlobalSettings(t, testKubernetesBlockGlobalSettingsYAML)

			in := tc.in
			in.Name = "agent-k8s-block-" + strings.ReplaceAll(tc.name, "_", "-")
			in.HTTPRequest = httptest.NewRequest("POST", "/api/v1/agents", nil)
			sc, err := srv.buildStartContext(context.Background(), in)
			if err != nil {
				t.Fatalf("buildStartContext: %v", err)
			}
			assertKubernetesBlockContext(t, sc, "scion-block")
		})
	}
}

// TestBuildStartContext_KubernetesBlockProfileOverridesRuntimeEntry covers
// the precedence: a profile's kubernetes_block_service_account wins over its
// runtime entry's. See TestBuildStartContext_KubernetesBlockProjectSettingIgnored
// for a project's own settings.
func TestBuildStartContext_KubernetesBlockProfileOverridesRuntimeEntry(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv, _ := newTestServerForStartContextMultiProfile(t, cfg, "docker", "team", "kubernetes")
	newTestGlobalSettings(t, `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: docker
    team:
        runtime: kubernetes
        kubernetes_block_service_account: team-block
runtimes:
    docker:
        type: docker
    kubernetes:
        type: kubernetes
        kubernetes_block_service_account: entry-block
`)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-k8s-block-profile",
		Config: &CreateAgentConfig{
			Profile:     "team",
			GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"},
		},
		HTTPRequest: httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("buildStartContext: %v", err)
	}
	assertKubernetesBlockContext(t, sc, "team-block")
	want := dispatchProfileSelection{ProfileName: "team", RuntimeEntryName: "kubernetes"}
	if sc.BlockSelection == nil || *sc.BlockSelection != want {
		t.Errorf("BlockSelection = %+v, want %+v", sc.BlockSelection, want)
	}
}

// TestBuildStartContext_KubernetesBlockProjectSettingIgnored pins that a
// kubernetes_block_service_account set only in a project's own
// settings.yaml is never used: the setting decides the pod's identity, so it
// is read only from the broker's global settings. With none there, the pod
// runs as the namespace's default ServiceAccount.
func TestBuildStartContext_KubernetesBlockProjectSettingIgnored(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesBlockGlobalSettingsYAML)
	newTestGlobalSettings(t, testKubernetesProjectSettingsYAML)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-block-project-setting",
		ProjectPath: projectDir,
		Config:      &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"}},
		HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("buildStartContext: %v", err)
	}
	assertKubernetesBlockContext(t, sc, "")
}

// TestBuildStartContext_KubernetesBlockInvalidServiceAccountRefused covers a
// hand-edited, malformed kubernetes_block_service_account: refused at the
// point of use rather than reaching the pod spec.
func TestBuildStartContext_KubernetesBlockInvalidServiceAccountRefused(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	newTestGlobalSettings(t, strings.Replace(testKubernetesBlockGlobalSettingsYAML, "scion-block", "Not_Valid", 1))

	_, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-block-invalid",
		Config:      &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"}},
		HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	if err == nil || !strings.Contains(err.Error(), "kubernetes_block_service_account") {
		t.Fatalf("expected the malformed block ServiceAccount to be refused, got %v", err)
	}
}

// TestBuildStartContext_KubernetesBlockAssignAndPassthroughUnaffected pins
// that the block ServiceAccount setting does not change "passthrough" or
// "assign" on Kubernetes, or "block" on docker.
func TestBuildStartContext_KubernetesBlockAssignAndPassthroughUnaffected(t *testing.T) {
	const globalYAML = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes:
        type: kubernetes
        kubernetes_block_service_account: scion-block
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: agent-worker-ksa
`
	t.Run("passthrough", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
		newTestGlobalSettings(t, globalYAML)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:        "agent-k8s-pt",
			Config:      &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{MetadataMode: "passthrough"}},
			HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
			Operation:   opCreate,
		})
		if err != nil {
			t.Fatal(err)
		}
		if sc.Opts.KubernetesBlockIdentity != nil || sc.BlockSelection != nil {
			t.Errorf("expected no block identity for passthrough, got %+v", sc.Opts.KubernetesBlockIdentity)
		}
		if got := sc.Opts.Env["SCION_METADATA_MODE"]; got != "passthrough" {
			t.Errorf("SCION_METADATA_MODE = %q, want passthrough", got)
		}
	})
	t.Run("assign", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
		projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
		newTestGlobalSettings(t, globalYAML)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:        "agent-k8s-assign",
			ProjectPath: projectDir,
			Config: &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
			}},
			HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
			Operation:   opCreate,
		})
		if err != nil {
			t.Fatal(err)
		}
		if sc.Opts.KubernetesBlockIdentity != nil {
			t.Errorf("expected no block identity for assign, got %+v", sc.Opts.KubernetesBlockIdentity)
		}
		if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
			t.Errorf("assign ServiceAccount = %q, want agent-worker-ksa", got)
		}
	})
	t.Run("docker_block", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContextRuntime(t, cfg, "docker")
		newTestGlobalSettings(t, globalYAML)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:        "agent-docker-block",
			Config:      &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"}},
			HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
			Operation:   opCreate,
		})
		if err != nil {
			t.Fatal(err)
		}
		if sc.Opts.KubernetesBlockIdentity != nil || sc.BlockSelection != nil {
			t.Errorf("expected no Kubernetes block identity on docker, got %+v", sc.Opts.KubernetesBlockIdentity)
		}
	})
}

// TestBuildStartContext_KubernetesBlockRefusedAfterProjectDirResolution
// pins the ordering of the Kubernetes "block" resolution relative to
// buildStartContext's own project-directory resolution (WriteProjectMarker,
// MkdirAll): it runs after that resolution, so settings and the saved
// profile it depends on are read from the final, post-update location. A
// request-level serviceAccountName that conflicts with block is refused at
// that point, and the project directory must already exist by then.
func TestBuildStartContext_KubernetesBlockRefusedAfterProjectDirResolution(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")

	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatalf("GetGlobalDir: %v", err)
	}
	const slug = "hub-managed-project-block-test"
	projectDir := filepath.Join(globalDir, "projects", slug)
	t.Cleanup(func() { _ = os.RemoveAll(projectDir) })
	if _, statErr := os.Stat(projectDir); !os.IsNotExist(statErr) {
		t.Fatalf("precondition failed: %s already exists", projectDir)
	}

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-block-no-side-effects",
		ProjectSlug: slug,
		ProjectID:   "project-id-for-side-effect-check",
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"},
			Kubernetes:  &api.KubernetesConfig{ServiceAccountName: "some-other-ksa"},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatalf("expected the dispatch to be rejected, got nil (env: %v)", sc.Opts.Env)
	}
	if !strings.Contains(err.Error(), "conflicts with GCP identity mode") {
		t.Errorf("expected the explicit ServiceAccount conflict, got %q", err.Error())
	}
	if _, statErr := os.Stat(projectDir); os.IsNotExist(statErr) {
		t.Errorf("expected the project directory/marker to already exist by the time the rejection runs, but %s does not exist", projectDir)
	}
}
