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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestBuildStartContext_KubernetesAssignWithMapping covers the happy path: a
// Kubernetes "assign" dispatch whose GSA has a mapped KSA gets the KSA
// resolved (for pkg/agent/run.go to apply to the pod's ServiceAccountName),
// keeps the informational SA email/project ID env, and gets no
// emulator-specific env (mode, redirect, or port).
func TestBuildStartContext_KubernetesAssignWithMapping(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-mapped",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
				ProjectID:    "my-project",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected a mapped GSA to be accepted, got %v", err)
	}
	if sc.Opts.Env["SCION_METADATA_MODE"] != "passthrough" {
		t.Errorf("expected SCION_METADATA_MODE='passthrough' (no emulator), got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["SCION_METADATA_SA_EMAIL"] != "agent-worker@my-project.iam.gserviceaccount.com" {
		t.Errorf("expected SCION_METADATA_SA_EMAIL to be kept as informational env, got %q", sc.Opts.Env["SCION_METADATA_SA_EMAIL"])
	}
	if sc.Opts.Env["SCION_METADATA_PROJECT_ID"] != "my-project" {
		t.Errorf("expected SCION_METADATA_PROJECT_ID to be kept as informational env, got %q", sc.Opts.Env["SCION_METADATA_PROJECT_ID"])
	}
	for _, key := range []string{"GCE_METADATA_HOST", "GCE_METADATA_ROOT", "SCION_METADATA_PORT"} {
		if v, ok := sc.Opts.Env[key]; ok {
			t.Errorf("expected no %s for Kubernetes assign (no emulator), got %q", key, v)
		}
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
		t.Errorf("expected ResolvedKubernetesServiceAccountName='agent-worker-ksa', got %q", got)
	}
	if sc.Opts.InlineConfig != nil {
		t.Errorf("expected InlineConfig to be left nil (the resolved KSA is not carried there), got %+v", sc.Opts.InlineConfig)
	}
}

// testKubernetesMappingWithNamespaceGlobalSettingsYAML extends
// testKubernetesMappingGlobalSettingsYAML with an operator-pinned namespace
// on the "kubernetes" runtime entry, for the namespace-conflict tests below.
const testKubernetesMappingWithNamespaceGlobalSettingsYAML = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes:
        type: kubernetes
        namespace: scion-agents
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: agent-worker-ksa
`

// TestBuildStartContext_KubernetesAssignConflictingNamespaceRejected: the
// Workload Identity principal is (namespace, KSA)
// together, so an explicit request-level namespace that differs from the
// operator's pinned namespace must be rejected the same authoritative way a
// differing explicit KSA already is.
func TestBuildStartContext_KubernetesAssignConflictingNamespaceRejected(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingWithNamespaceGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-namespace-conflict",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
			},
			Kubernetes: &api.KubernetesConfig{Namespace: "some-other-namespace"},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatalf("expected an error for a conflicting explicit namespace, got nil (env: %v)", sc.Opts.Env)
	}
	if sc != nil {
		t.Errorf("expected nil startContext alongside the error, got %+v", sc)
	}
	if !strings.Contains(err.Error(), "some-other-namespace") || !strings.Contains(err.Error(), "scion-agents") {
		t.Errorf("expected the error to name both the explicit and operator-configured namespaces, got %q", err.Error())
	}
}

// TestBuildStartContext_KubernetesAssignMatchingNamespaceAccepted covers the
// non-conflicting half: an explicit namespace that already equals the
// operator's pinned namespace proceeds normally.
func TestBuildStartContext_KubernetesAssignMatchingNamespaceAccepted(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingWithNamespaceGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-namespace-match",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
			},
			Kubernetes: &api.KubernetesConfig{Namespace: "scion-agents"},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected a matching explicit namespace to be accepted, got %v", err)
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
		t.Errorf("expected ResolvedKubernetesServiceAccountName='agent-worker-ksa', got %q", got)
	}
}

// TestBuildStartContext_KubernetesAssignNoOperatorNamespace covers a broker
// whose runtime entry sets no namespace (testKubernetesMappingGlobalSettingsYAML,
// unlike the *WithNamespace variant above). The namespace then falls back to
// the Kubernetes runtime's default (SCION_K8S_NAMESPACE here), so an
// explicit request namespace is accepted only when it equals that default.
func TestBuildStartContext_KubernetesAssignNoOperatorNamespace(t *testing.T) {
	for _, tc := range []struct {
		name      string
		namespace string
		wantErr   bool
	}{
		{name: "explicit namespace other than the runtime default refused", namespace: "whatever-namespace", wantErr: true},
		{name: "explicit namespace equal to the runtime default accepted", namespace: "ns-runtime-default", wantErr: false},
		{name: "no explicit namespace accepted", namespace: "", wantErr: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCION_K8S_NAMESPACE", "ns-runtime-default")
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
			projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
			newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

			createCfg := &CreateAgentConfig{
				GCPIdentity: &GCPIdentityConfig{
					MetadataMode: "assign",
					SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
				},
			}
			if tc.namespace != "" {
				createCfg.Kubernetes = &api.KubernetesConfig{Namespace: tc.namespace}
			}
			r := httptest.NewRequest("POST", "/api/v1/agents", nil)
			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:        "agent-k8s-assign-no-operator-namespace",
				ProjectPath: projectDir,
				Config:      createCfg,
				HTTPRequest: r,
				Operation:   opCreate,
			})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for an explicit namespace other than the runtime default, got nil")
				}
				var sce *startContextError
				if !errors.As(err, &sce) || sce.Status != http.StatusBadRequest {
					t.Fatalf("expected a 400 startContextError, got %v", err)
				}
				if !strings.Contains(err.Error(), tc.namespace) || !strings.Contains(err.Error(), "ns-runtime-default") {
					t.Errorf("expected the error to name the explicit and the resolved namespace, got %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("expected acceptance, got %v", err)
			}
			if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
				t.Errorf("expected ResolvedKubernetesServiceAccountName='agent-worker-ksa', got %q", got)
			}
		})
	}
}

// TestBuildStartContext_KubernetesAssignMixedCaseSAEmailStillResolves proves
// the case-normalization fix directly: a mixed-case GSA email (as a caller
// might send it, even though canonical GCP service account emails are
// lowercase) must still resolve the mapping and must not then fail the
// resolved-entry validation, which requires a lowercase email — the mapping
// lookup and the validation call must agree on the same lower-cased value.
func TestBuildStartContext_KubernetesAssignMixedCaseSAEmailStillResolves(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-mixed-case",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "Agent-Worker@My-Project.IAM.GServiceAccount.com",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected a mixed-case GSA email to still resolve the mapping, got %v", err)
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
		t.Errorf("expected ResolvedKubernetesServiceAccountName='agent-worker-ksa', got %q", got)
	}
}

// TestBuildStartContext_KubernetesAssignWithoutMapping covers the required
// rejection: a Kubernetes "assign" dispatch whose GSA has no mapped KSA
// fails before any pod or env is built, with an actionable error naming the
// setting to add. The message must be the specific "no ... mapped" text, not
// merely any error — ValidateKubernetesServiceAccountMappings's own error
// text also happens to mention kubernetes_service_account_mappings and the
// GSA email, so a looser assertion would not catch the reject check itself
// being disabled.
func TestBuildStartContext_KubernetesAssignWithoutMapping(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-unmapped",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "unmapped@my-project.iam.gserviceaccount.com",
				ProjectID:    "my-project",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatalf("expected an error for an unmapped GSA, got nil (env: %v)", sc.Opts.Env)
	}
	if sc != nil {
		t.Errorf("expected nil startContext alongside the error, got %+v", sc)
	}
	const wantSubstr = `has no Kubernetes ServiceAccount mapped for "unmapped@my-project.iam.gserviceaccount.com"`
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("expected the error to contain %q, got %q", wantSubstr, err.Error())
	}
}

// TestBuildStartContext_KubernetesAssignEmptyGSAEmail covers the distinct
// "no GSA at all" case: it must not be reported as "no mapping found", since
// there is nothing to look up and the fix is different (supply a GSA, not
// add a mapping).
func TestBuildStartContext_KubernetesAssignEmptyGSAEmail(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-no-gsa",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{MetadataMode: "assign"},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatalf("expected an error for assign with no GSA email, got nil (env: %v)", sc.Opts.Env)
	}
	if strings.Contains(err.Error(), "no Kubernetes ServiceAccount mapped") {
		t.Errorf("expected a distinct 'requires a service account email' error, not the no-mapping error, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "requires a service account email") {
		t.Errorf("expected the error to say a service account email is required, got %q", err.Error())
	}
}

// TestBuildStartContext_KubernetesAssignGlobalSettingsLoadFailure covers the
// other distinct error: the broker's global settings.yaml itself failing to
// load (malformed YAML) must not be reported as "no mapping found" either —
// the fix is different (repair settings.yaml, not add a mapping).
func TestBuildStartContext_KubernetesAssignGlobalSettingsLoadFailure(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, "schema_version: \"1\"\nprofiles: [this is not valid yaml for a map")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-bad-global-settings",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatalf("expected an error for a malformed global settings file, got nil (env: %v)", sc.Opts.Env)
	}
	if strings.Contains(err.Error(), "no Kubernetes ServiceAccount mapped") {
		t.Errorf("expected a distinct settings-load error, not the no-mapping error, got %q", err.Error())
	}
}

// TestBuildStartContext_KubernetesAssignConflictingInlineServiceAccountName
// covers the conflict rule via InlineConfig.Kubernetes: the mapping is
// authoritative, so an explicit inline ServiceAccountName naming a
// *different* KSA than the mapping is refused rather than silently honored
// or silently overridden — either direction would run the pod as an
// identity other than the one the mapping says this GSA should get.
func TestBuildStartContext_KubernetesAssignConflictingInlineServiceAccountName(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-conflict-inline",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
				ProjectID:    "my-project",
			},
		},
		InlineConfig: &api.ScionConfig{
			Kubernetes: &api.KubernetesConfig{ServiceAccountName: "some-other-ksa"},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatalf("expected an error for a conflicting explicit ServiceAccountName, got nil (env: %v)", sc.Opts.Env)
	}
	if sc != nil {
		t.Errorf("expected nil startContext alongside the error, got %+v", sc)
	}
	if !strings.Contains(err.Error(), "some-other-ksa") || !strings.Contains(err.Error(), "agent-worker-ksa") {
		t.Errorf("expected the error to name both the explicit and mapped ServiceAccount names, got %q", err.Error())
	}
}

// TestBuildStartContext_KubernetesAssignConflictingConfigServiceAccountName
// covers the same conflict rule via the create request's Config.Kubernetes
// field (not InlineConfig) — a separate source the conflict check also
// reads, and one that must independently reject a mismatch.
func TestBuildStartContext_KubernetesAssignConflictingConfigServiceAccountName(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-conflict-config",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
				ProjectID:    "my-project",
			},
			Kubernetes: &api.KubernetesConfig{ServiceAccountName: "config-level-other-ksa"},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatalf("expected an error for a conflicting Config.Kubernetes ServiceAccountName, got nil (env: %v)", sc.Opts.Env)
	}
	if sc != nil {
		t.Errorf("expected nil startContext alongside the error, got %+v", sc)
	}
	if !strings.Contains(err.Error(), "config-level-other-ksa") || !strings.Contains(err.Error(), "agent-worker-ksa") {
		t.Errorf("expected the error to name both the explicit and mapped ServiceAccount names, got %q", err.Error())
	}
}

// TestBuildStartContext_KubernetesAssignExplicitServiceAccountNameMatchesMapping
// covers the non-conflicting half of the same rule: an explicit inline
// ServiceAccountName that already equals the mapped KSA proceeds normally.
func TestBuildStartContext_KubernetesAssignExplicitServiceAccountNameMatchesMapping(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-match",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
				ProjectID:    "my-project",
			},
		},
		InlineConfig: &api.ScionConfig{
			Kubernetes: &api.KubernetesConfig{ServiceAccountName: "agent-worker-ksa"},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected a matching explicit ServiceAccountName to be accepted, got %v", err)
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
		t.Errorf("expected ResolvedKubernetesServiceAccountName='agent-worker-ksa', got %q", got)
	}
	// The original explicit InlineConfig is untouched: the resolved value is
	// carried on the dedicated opts field, not written back into it.
	if got := sc.Opts.InlineConfig.Kubernetes.ServiceAccountName; got != "agent-worker-ksa" {
		t.Errorf("expected the original InlineConfig ServiceAccountName to remain 'agent-worker-ksa', got %q", got)
	}
}

// TestBuildStartContext_KubernetesAssignResolvedEnvSAEmailSource covers the
// start/restart shape: no Config at all, the GSA email and project ID arrive
// only via a hub-injected resolvedEnv (as httpdispatcher.go actually sends
// them), and the mapping must still resolve from that source.
func TestBuildStartContext_KubernetesAssignResolvedEnvSAEmailSource(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-resolvedenv",
		ProjectPath: projectDir,
		ResolvedEnv: map[string]string{
			"SCION_METADATA_MODE":        "assign",
			"SCION_METADATA_MODE_SOURCE": "hub",
			"SCION_METADATA_SA_EMAIL":    "agent-worker@my-project.iam.gserviceaccount.com",
			"SCION_METADATA_PROJECT_ID":  "my-project",
		},
		HTTPRequest: r,
		Operation:   opHTTPStart,
	})
	if err != nil {
		t.Fatalf("expected the resolvedEnv-sourced GSA to resolve, got %v", err)
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
		t.Errorf("expected ResolvedKubernetesServiceAccountName='agent-worker-ksa', got %q", got)
	}
	if sc.Opts.Env["SCION_METADATA_MODE"] != "passthrough" {
		t.Errorf("expected SCION_METADATA_MODE='passthrough', got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
}

// TestBuildStartContext_KubernetesAssignCreateAndStartProduceSameEnv pins the
// requirement directly: a create-shaped dispatch (Config.GCPIdentity) and a
// start-shaped dispatch (ResolvedEnv, as the hub actually sends on
// start/restart) for the same GSA must produce byte-identical
// SCION_METADATA_* env, not differ by dispatch path.
func TestBuildStartContext_KubernetesAssignCreateAndStartProduceSameEnv(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	metadataKeys := []string{"SCION_METADATA_MODE", "SCION_METADATA_SA_EMAIL", "SCION_METADATA_PROJECT_ID", "GCE_METADATA_HOST", "GCE_METADATA_ROOT", "SCION_METADATA_PORT"}

	srvCreate := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDirCreate := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)
	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	scCreate, err := srvCreate.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-create",
		ProjectPath: projectDirCreate,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
				ProjectID:    "my-project",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("create path: expected acceptance, got %v", err)
	}

	cfg2 := DefaultServerConfig()
	cfg2.StateDir = t.TempDir()
	srvStart := newTestServerForStartContextRuntime(t, cfg2, "kubernetes")
	projectDirStart := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)
	scStart, err := srvStart.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-start",
		ProjectPath: projectDirStart,
		ResolvedEnv: map[string]string{
			"SCION_METADATA_MODE":        "assign",
			"SCION_METADATA_MODE_SOURCE": "hub",
			"SCION_METADATA_SA_EMAIL":    "agent-worker@my-project.iam.gserviceaccount.com",
			"SCION_METADATA_PROJECT_ID":  "my-project",
		},
		HTTPRequest: r,
		Operation:   opHTTPStart,
	})
	if err != nil {
		t.Fatalf("start path: expected acceptance, got %v", err)
	}

	for _, key := range metadataKeys {
		createVal, createOK := scCreate.Opts.Env[key]
		startVal, startOK := scStart.Opts.Env[key]
		if createOK != startOK || createVal != startVal {
			t.Errorf("%s differs between create and start: create=(%q,%v) start=(%q,%v)", key, createVal, createOK, startVal, startOK)
		}
	}
	if scCreate.Opts.ResolvedKubernetesServiceAccountName != scStart.Opts.ResolvedKubernetesServiceAccountName {
		t.Errorf("ResolvedKubernetesServiceAccountName differs between create (%q) and start (%q)",
			scCreate.Opts.ResolvedKubernetesServiceAccountName, scStart.Opts.ResolvedKubernetesServiceAccountName)
	}
}

// TestBuildStartContext_KubernetesAssignSavedProfileFallback covers the
// start/restart profile source: with no Config at all, the profile must come
// from the agent's own saved profile (agent.GetSavedProfile), not silently
// fall back to the project's active profile. Uses
// newTestServerForStartContextMultiProfile (not
// newTestServerForStartContextRuntime): ForceRuntime short-circuits
// resolveDispatchProfileSelection before any profile is resolved at all, which
// would defeat this test's entire point. The broker's default profile
// ("local", pinned to a non-Kubernetes runtime) and the agent's saved
// profile ("other-profile", pinned to Kubernetes with its own KSA mapping)
// are deliberately different, so only reading the correct one passes.
func TestBuildStartContext_KubernetesAssignSavedProfileFallback(t *testing.T) {
	const otherProfile = "other-profile"
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv, dotScion := newTestServerForStartContextMultiProfile(t, cfg, "docker", otherProfile, "kubernetes")
	newTestGlobalSettings(t, fmt.Sprintf(`schema_version: "1"
profiles:
    %s:
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: saved-profile-ksa
runtimes:
    kubernetes:
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: runtime-entry-ksa
`, otherProfile))

	// Persist a saved profile for this agent, distinct from the broker's
	// default, the same way agent.GetSavedProfile reads it (agent-info.json
	// under the agent's home directory).
	const agentName = "agent-k8s-assign-saved-profile"
	writeSavedAgentProfile(t, dotScion, agentName, otherProfile)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: agentName,
		ResolvedEnv: map[string]string{
			"SCION_METADATA_MODE":        "assign",
			"SCION_METADATA_MODE_SOURCE": "hub",
			"SCION_METADATA_SA_EMAIL":    "agent-worker@my-project.iam.gserviceaccount.com",
		},
		HTTPRequest: r,
		Operation:   opHTTPStart,
	})
	if err != nil {
		t.Fatalf("expected the dispatch to be accepted, got %v", err)
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "saved-profile-ksa" {
		t.Errorf("expected the agent's saved profile mapping 'saved-profile-ksa' to be used, got %q (runtime-entry-ksa would mean the profile-level entry was skipped)", got)
	}
}

// TestBuildStartContext_KubernetesAssignProjectLevelMappingLogsWarning covers
// the operator-experience side of the "project settings cannot override this
// mapping" rule: a project that sets kubernetes_service_account_mappings on
// its own runtime entry does not just get ignored silently — the broker logs
// a warning naming the setting, so the mistake is visible instead of only
// showing up later as a confusing "no mapping" error for some other GSA.
// This dispatch itself still succeeds, using the global mapping.
func TestBuildStartContext_KubernetesAssignProjectLevelMappingLogsWarning(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")

	var buf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	projectDir := newTestProjectSettings(t, `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes:
        type: kubernetes
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: project-level-ksa-ignored
`)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-project-level-warning",
		ProjectPath: projectDir,
		ResolvedEnv: map[string]string{
			"SCION_METADATA_MODE":        "assign",
			"SCION_METADATA_MODE_SOURCE": "hub",
			"SCION_METADATA_SA_EMAIL":    "agent-worker@my-project.iam.gserviceaccount.com",
		},
		HTTPRequest: r,
		Operation:   opHTTPStart,
	})
	if err != nil {
		t.Fatalf("expected the dispatch to be accepted (using the global mapping), got %v", err)
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
		t.Errorf("expected the global mapping 'agent-worker-ksa' to be used (not the project-level entry), got %q", got)
	}

	logged := buf.String()
	if !strings.Contains(logged, "kubernetes_service_account_mappings") || !strings.Contains(logged, "never consulted") {
		t.Errorf("expected a warning naming kubernetes_service_account_mappings as never consulted from project settings, got log: %s", logged)
	}
}

// TestBuildStartContext_KubernetesAssignNonActiveProfileMapping covers an
// explicit, non-active profile named on the create request: the mapping must
// come from that named profile, not silently from the broker's default
// profile (pinned to a non-Kubernetes runtime here, so the two are also
// distinguishable by isKubernetes, not just by which KSA resolves).
func TestBuildStartContext_KubernetesAssignNonActiveProfileMapping(t *testing.T) {
	const otherProfile = "other-profile"
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv, _ := newTestServerForStartContextMultiProfile(t, cfg, "docker", otherProfile, "kubernetes")
	newTestGlobalSettings(t, fmt.Sprintf(`schema_version: "1"
profiles:
    %s:
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: other-profile-ksa
runtimes:
    kubernetes:
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: runtime-entry-ksa
`, otherProfile))

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-k8s-assign-non-active-profile",
		Config: &CreateAgentConfig{
			Profile: otherProfile,
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected the dispatch to be accepted, got %v", err)
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "other-profile-ksa" {
		t.Errorf("expected the named profile's mapping 'other-profile-ksa' to be used, got %q (runtime-entry-ksa would mean the requested profile was ignored)", got)
	}
}

// TestBuildStartContext_KubernetesAssignInvalidResolvedEntryRejected covers
// the broker-side re-validation of the resolved mapping entry: settings.yaml
// can be hand-edited (or DB-overlaid) without going through the schema
// validator, so an invalid KSA name that nonetheless resolves via
// ResolveKubernetesServiceAccountMapping must still be rejected here.
func TestBuildStartContext_KubernetesAssignInvalidResolvedEntryRejected(t *testing.T) {
	const settingsYAML = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes:
        type: kubernetes
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: Invalid_KSA_Name
`
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, settingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-invalid-entry",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatalf("expected an error for an invalid resolved KSA name, got nil (env: %v)", sc.Opts.Env)
	}
	if !strings.Contains(err.Error(), "not a valid Kubernetes ServiceAccount name") {
		t.Errorf("expected the error to say the resolved KSA name is invalid, got %q", err.Error())
	}
}

// TestBuildStartContext_KubernetesAssignProjectSettingsCannotOverrideMapping
// is the operator-only-source requirement's own test: a project's
// settings.yaml defines a *conflicting* mapping for the same GSA, but the
// broker's global mapping must still be the one that wins — a project
// (potentially a repository any contributor can edit) must not be able to
// redirect a Kubernetes assign dispatch to a different identity than the one
// the operator configured.
func TestBuildStartContext_KubernetesAssignProjectSettingsCannotOverrideMapping(t *testing.T) {
	const projectSettingsYAML = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes:
        type: kubernetes
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: project-sourced-ksa
`
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, projectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-project-override-attempt",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected the dispatch to be accepted, got %v", err)
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
		t.Errorf("expected the operator's global mapping 'agent-worker-ksa' to win, got %q (project-sourced-ksa would mean the project settings.yaml overrode it)", got)
	}
}

// TestBuildStartContext_KubernetesAssignProfileMappingOverridesRuntimeMapping
// exercises the settings precedence end-to-end through buildStartContext (the
// unit-level precedence itself is covered directly by
// TestResolveKubernetesServiceAccountMapping in pkg/config): a profile-level
// mapping for the requested GSA wins over the runtime-level mapping for the
// same GSA, both read from the broker's global settings. Uses
// newTestServerForStartContextMultiProfile (not
// newTestServerForStartContextRuntime): ForceRuntime short-circuits
// resolveDispatchProfileSelection before any profile is resolved at all,
// which would always leave ProfileName empty and defeat this test's point —
// the broker's default profile ("local") is the one under test here, pinned
// to Kubernetes; the unused "other-profile"/"docker" values just satisfy the
// helper's signature.
func TestBuildStartContext_KubernetesAssignProfileMappingOverridesRuntimeMapping(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv, _ := newTestServerForStartContextMultiProfile(t, cfg, "kubernetes", "other-profile", "docker")
	newTestGlobalSettings(t, `schema_version: "1"
profiles:
    local:
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: profile-ksa
runtimes:
    kubernetes:
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: runtime-ksa
`)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-k8s-assign-precedence",
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
				ProjectID:    "my-project",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected the dispatch to be accepted, got %v", err)
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "profile-ksa" {
		t.Errorf("expected the profile-level mapping 'profile-ksa' to win, got %q", got)
	}
}

// TestStartAgentEndpoint_KubernetesAssignResolvedKSAReachesOpts and
// TestRestartAgentEndpoint_KubernetesAssignResolvedKSAReachesOpts exercise the
// full HTTP handler (not just buildStartContext directly), confirming
// startAgent/restartAgent pass the broker-resolved KSA through to
// mgr.Start's opts unmodified — the "reaches the pod spec" requirement, one
// level up from pkg/agent's own TestStart_ResolvedKubernetesServiceAccountNameOverridesTemplate
// and TestStart_RestartOfExistingAgent_ResolvedKubernetesServiceAccountNameOverridesPersistedValue,
// which cover run.go's application of the field once it reaches Start.

func TestStartAgentEndpoint_KubernetesAssignResolvedKSAReachesOpts(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	mgr, ok := srv.manager.(*envCapturingManager)
	if !ok {
		t.Fatalf("expected *envCapturingManager, got %T", srv.manager)
	}

	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	bodyJSON := `{"projectPath": ` + strconv.Quote(projectDir) + `, "resolvedEnv": {"SCION_METADATA_MODE": "assign", "SCION_METADATA_MODE_SOURCE": "hub", "SCION_METADATA_SA_EMAIL": "agent-worker@my-project.iam.gserviceaccount.com", "SCION_METADATA_PROJECT_ID": "my-project"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-k8s-assign-start/start", strings.NewReader(bodyJSON))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted && w.Code != http.StatusOK {
		t.Fatalf("expected a success status, got %d: %s", w.Code, w.Body.String())
	}
	if got := mgr.lastStartOpts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
		t.Errorf("expected opts.ResolvedKubernetesServiceAccountName='agent-worker-ksa' to reach mgr.Start, got %q", got)
	}
}

func TestRestartAgentEndpoint_KubernetesAssignResolvedKSAReachesOpts(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	mgr, ok := srv.manager.(*envCapturingManager)
	if !ok {
		t.Fatalf("expected *envCapturingManager, got %T", srv.manager)
	}

	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	// restartAgent looks the agent up by name via manager.List before calling
	// buildStartContext, to resolve its ProjectPath — register it here the
	// same way an already-running agent would appear.
	mgr.agents = []api.AgentInfo{{Name: "agent-k8s-assign-restart", ProjectPath: projectDir}}

	bodyJSON := `{"resolvedEnv": {"SCION_METADATA_MODE": "assign", "SCION_METADATA_MODE_SOURCE": "hub", "SCION_METADATA_SA_EMAIL": "agent-worker@my-project.iam.gserviceaccount.com", "SCION_METADATA_PROJECT_ID": "my-project"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-k8s-assign-restart/restart", strings.NewReader(bodyJSON))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK && w.Code != http.StatusAccepted {
		t.Fatalf("expected a success status, got %d: %s", w.Code, w.Body.String())
	}
	if got := mgr.lastStartOpts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
		t.Errorf("expected opts.ResolvedKubernetesServiceAccountName='agent-worker-ksa' to reach mgr.Start, got %q", got)
	}
}

// TestStartAgentEndpoint_KubernetesAssignConflictingInlineServiceAccountNameRejected
// confirms the explicit-conflict check fires through the actual start HTTP
// path (a JSON inlineConfig field in the request body), not only when
// buildStartContext is called directly — mgr.Start must never be reached.
func TestStartAgentEndpoint_KubernetesAssignConflictingInlineServiceAccountNameRejected(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	mgr, ok := srv.manager.(*envCapturingManager)
	if !ok {
		t.Fatalf("expected *envCapturingManager, got %T", srv.manager)
	}

	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	bodyJSON := `{"projectPath": ` + strconv.Quote(projectDir) + `, "resolvedEnv": {"SCION_METADATA_MODE": "assign", "SCION_METADATA_MODE_SOURCE": "hub", "SCION_METADATA_SA_EMAIL": "agent-worker@my-project.iam.gserviceaccount.com"}, "inlineConfig": {"kubernetes": {"serviceAccountName": "some-other-ksa"}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-k8s-assign-start-conflict/start", strings.NewReader(bodyJSON))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	// Pins the 400-status-preservation fix: buildStartContext's
	// startContextError carries Status: http.StatusBadRequest for this
	// rejection, and the handler must surface that status, not collapse it
	// to a generic 500.
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 for a conflicting inline ServiceAccountName (not collapsed to a 500), got %d: %s", w.Code, w.Body.String())
	}
	if mgr.startCalls != 0 {
		t.Errorf("expected mgr.Start to never be called, got %d calls", mgr.startCalls)
	}
	if !strings.Contains(w.Body.String(), "some-other-ksa") {
		t.Errorf("expected the error response to name the conflicting ServiceAccountName, got %s", w.Body.String())
	}
}

// TestCreateAgentEndpoint_BuildStartContext400RecordedAsAttemptStatus guards
// handlers.go's createAgent: when buildStartContext rejects the request with
// a 400 (the same Kubernetes assign conflict as the start-path test above),
// the recorded dispatch-attempt HTTPStatus must be that 400, not a reversion
// to an unconditional 500 (M19) — a Hub polling the attempt status by
// requestId needs the real status to distinguish a client-correctable
// rejection from a broker-side failure.
func TestCreateAgentEndpoint_BuildStartContext400RecordedAsAttemptStatus(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")

	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	const requestID = "req-m19-attempt-status"
	bodyJSON := `{"requestId": "` + requestID + `", "name": "agent-k8s-assign-create-conflict", "projectPath": ` + strconv.Quote(projectDir) + `, "resolvedEnv": {"SCION_METADATA_MODE": "assign", "SCION_METADATA_MODE_SOURCE": "hub", "SCION_METADATA_SA_EMAIL": "agent-worker@my-project.iam.gserviceaccount.com"}, "inlineConfig": {"kubernetes": {"serviceAccountName": "some-other-ksa"}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 for a conflicting inline ServiceAccountName, got %d: %s", w.Code, w.Body.String())
	}

	attempt, ok := srv.dispatchAttempts[requestID]
	if !ok {
		t.Fatalf("expected a recorded dispatch attempt for requestId %q", requestID)
	}
	if attempt.HTTPStatus != http.StatusBadRequest {
		t.Errorf("expected recorded attempt HTTPStatus=400, got %d", attempt.HTTPStatus)
	}
}

// TestStartAgentEndpoint_NoStrayProjectMarkerFromResolvedProjectPath is the
// regression anchor for the stray-".scion"-marker bug (M17): startAgent's
// fallback resolves opts.ProjectPath from an existing container's recorded
// ProjectPath only when the start request's own body carries neither
// projectPath nor projectSlug. That resolved value must be used only for the
// rest of this handler and the dispatch below, never fed back into
// buildStartContext's own in.ProjectPath — buildStartContext's
// project-marker block (start_context.go, guarded on
// in.ProjectPath != "" && (in.ProjectSlug != "" || in.ProjectID != "")) would
// otherwise create a ".scion" marker nested inside what is typically already
// a split-storage external directory, not a project root. ProjectID is
// supplied here (via the projectId query parameter, independent of the
// request body) so the marker block's guard is satisfied as soon as
// in.ProjectPath is non-empty, isolating exactly the behavior this test
// guards.
func TestStartAgentEndpoint_NoStrayProjectMarkerFromResolvedProjectPath(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "docker")

	// externalPath simulates an existing agent's recorded ProjectPath in
	// split-storage mode: a directory that is not itself a project root and
	// has no ".scion" of its own yet.
	externalPath := filepath.Join(t.TempDir(), "external")
	if err := os.MkdirAll(externalPath, 0755); err != nil {
		t.Fatal(err)
	}

	mgr, ok := srv.manager.(*envCapturingManager)
	if !ok {
		t.Fatalf("expected *envCapturingManager, got %T", srv.manager)
	}
	mgr.agents = []api.AgentInfo{{Name: "existing-agent", ProjectPath: externalPath, ProjectID: "proj-123"}}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/existing-agent/start?projectId=proj-123", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK && w.Code != http.StatusAccepted {
		t.Fatalf("expected a success status, got %d: %s", w.Code, w.Body.String())
	}

	strayMarker := filepath.Join(externalPath, config.DotScion)
	if _, err := os.Stat(strayMarker); err == nil {
		t.Errorf("expected no stray marker at %s, but one was created", strayMarker)
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected error checking for stray marker: %v", err)
	}
}
