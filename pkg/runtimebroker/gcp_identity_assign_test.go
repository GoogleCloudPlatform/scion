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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestRejectKubernetesAssignRuntimeChange(t *testing.T) {
	k8sEntry := dispatchProfileSelection{ProfileName: "prod", RuntimeEntryName: "k8s"}
	otherEntry := dispatchProfileSelection{ProfileName: "team", RuntimeEntryName: "k8s-team"}
	for _, tc := range []struct {
		name        string
		ksa         string
		mode        string
		early       *dispatchProfileSelection
		late        dispatchProfileSelection
		runtimeType string
		wantReject  bool
	}{
		{name: "no resolved ServiceAccount on docker", runtimeType: "docker"},
		{name: "passthrough on kubernetes", mode: "passthrough", runtimeType: "kubernetes"},
		{name: "resolved ServiceAccount on kubernetes", ksa: "agent-worker-ksa", mode: "passthrough", early: &k8sEntry, late: k8sEntry, runtimeType: "kubernetes"},
		{name: "resolved ServiceAccount on k8s alias", ksa: "agent-worker-ksa", mode: "passthrough", early: &k8sEntry, late: k8sEntry, runtimeType: "k8s"},
		{name: "resolved ServiceAccount on docker", ksa: "agent-worker-ksa", mode: "passthrough", early: &k8sEntry, late: k8sEntry, runtimeType: "docker", wantReject: true},
		{name: "resolved ServiceAccount with a different runtime entry", ksa: "agent-worker-ksa", mode: "passthrough", early: &k8sEntry, late: otherEntry, runtimeType: "kubernetes", wantReject: true},
		{name: "assign env on docker", mode: "assign", runtimeType: "docker"},
		{name: "assign env on kubernetes without a resolved ServiceAccount", mode: "assign", runtimeType: "kubernetes", wantReject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := api.StartOptions{
				ResolvedKubernetesServiceAccountName: tc.ksa,
				Env:                                  map[string]string{},
			}
			if tc.mode != "" {
				opts.Env["SCION_METADATA_MODE"] = tc.mode
			}
			sce := rejectKubernetesAssignRuntimeChange(opts, tc.early, tc.runtimeType, func() dispatchProfileSelection { return tc.late })
			if !tc.wantReject {
				if sce != nil {
					t.Fatalf("expected no rejection, got %v", sce)
				}
				return
			}
			if sce == nil {
				t.Fatal("expected a rejection, got nil")
			}
			if sce.Status != http.StatusConflict {
				t.Errorf("expected status %d, got %d", http.StatusConflict, sce.Status)
			}
		})
	}
}

// assignResolvedEnv is the env the hub sends on start/restart for an agent
// whose GCP identity mode is "assign".
var assignResolvedEnv = map[string]string{
	"SCION_METADATA_MODE":       "assign",
	"SCION_METADATA_SA_EMAIL":   "agent-worker@my-project.iam.gserviceaccount.com",
	"SCION_METADATA_PROJECT_ID": "my-project",
}

// testLateCheckMappingGlobalSettingsYAML maps the test GSA under the
// "kubernetes" runtime entry that newTestServerForLateCheckOrdering's
// "local" profile selects.
const testLateCheckMappingGlobalSettingsYAML = `schema_version: "1"
runtimes:
    kubernetes:
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: agent-worker-ksa
`

func postRestart(t *testing.T, srv *Server, urlID string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"resolvedEnv": assignResolvedEnv})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+urlID+"/restart", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// TestRestartAgent_LateKubernetesAssignRuntimeChangeRunsBeforeStop reverses
// newTestServerForLateCheckOrdering's saved profiles: the Kubernetes profile
// is saved under the agent's Name, which buildStartContext reads, so it
// resolves a ServiceAccount from the global mapping; nothing is saved under
// the URL id, which the handler's later resolution reads, so that one falls
// back to the active profile (docker). The restart must be refused with 409
// before the agent is stopped or started.
//
// On start, a request carries no profile of its own, and when the project
// path is resolved, startAgent passes the URL id to buildStartContext and
// reads the saved profile under the same id and project afterwards, so the
// two resolutions agree. When the project path is not resolved, startAgent
// leaves the profile unset for the later resolution; see
// TestStartAgent_LateKubernetesAssignRuntimeChangeWithoutProjectPath.
func TestRestartAgent_LateKubernetesAssignRuntimeChangeRunsBeforeStop(t *testing.T) {
	srv, mgr, remapRuntime := newTestServerForLateCheckOrdering(t, "actual-agent-name", "url-id", "kubernetes")
	remapRuntime.RunFunc = func(ctx context.Context, config runtime.RunConfig) (string, error) {
		t.Fatal("the agent must not run once the late runtime change is refused")
		return "", nil
	}
	dotScion := mgr.agents[0].ProjectPath
	writeSavedAgentProfile(t, dotScion, "actual-agent-name", "local")
	if err := os.RemoveAll(config.GetAgentHomePath(dotScion, "url-id")); err != nil {
		t.Fatal(err)
	}
	newTestGlobalSettings(t, testLateCheckMappingGlobalSettingsYAML)

	w := postRestart(t, srv, "url-id")

	if w.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, w.Code, w.Body.String())
	}
	if mgr.stopCalls != 0 {
		t.Errorf("expected the agent not to be stopped, got %d stop calls", mgr.stopCalls)
	}
}

// TestRestartAgent_LateKubernetesAssignRuntimeEntryChangeRunsBeforeStop
// saves two Kubernetes profiles on different runtime entries: "local"
// (runtime entry "kubernetes") under the agent's Name, which
// buildStartContext reads, and "team" (runtime entry "k8s-team") under the
// URL id, which the handler's later resolution reads. Both entries map the
// GSA, so both resolutions reach Kubernetes; only the runtime entry
// differs. The restart must be refused with 409 before the agent is stopped
// or started, and the message must name both selections.
func TestRestartAgent_LateKubernetesAssignRuntimeEntryChangeRunsBeforeStop(t *testing.T) {
	srv, mgr, remapRuntime := newTestServerForLateCheckOrdering(t, "actual-agent-name", "url-id", "kubernetes")
	remapRuntime.RunFunc = func(ctx context.Context, config runtime.RunConfig) (string, error) {
		t.Fatal("the agent must not run once the late runtime entry change is refused")
		return "", nil
	}
	dotScion := mgr.agents[0].ProjectPath
	const projectSettingsYAML = `schema_version: "1"
active_profile: other
profiles:
    other:
        runtime: docker
    local:
        runtime: kubernetes
    team:
        runtime: k8s-team
runtimes:
    docker:
        type: docker
    kubernetes:
        type: kubernetes
    k8s-team:
        type: kubernetes
`
	if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(projectSettingsYAML), 0644); err != nil {
		t.Fatal(err)
	}
	writeSavedAgentProfile(t, dotScion, "actual-agent-name", "local")
	writeSavedAgentProfile(t, dotScion, "url-id", "team")
	newTestGlobalSettings(t, `schema_version: "1"
runtimes:
    kubernetes:
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: agent-worker-ksa
    k8s-team:
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: team-ksa
`)

	w := postRestart(t, srv, "url-id")

	if w.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, w.Code, w.Body.String())
	}
	if mgr.stopCalls != 0 {
		t.Errorf("expected the agent not to be stopped, got %d stop calls", mgr.stopCalls)
	}
	body := w.Body.String()
	for _, want := range []string{`\"local\"`, `\"kubernetes\"`, `\"team\"`, `\"k8s-team\"`, "recreate the agent"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected the 409 message to contain %s, got %s", want, body)
		}
	}
	if strings.Contains(strings.ToLower(body), "retry") {
		t.Errorf("the 409 message must not suggest retrying, got %s", body)
	}
}

// TestRestartAgent_LateKubernetesAssignWithoutServiceAccountRunsBeforeStop
// uses newTestServerForLateCheckOrdering as is: buildStartContext resolves
// docker and builds the metadata emulator env for "assign", and only the
// handler's later resolution reaches Kubernetes, for which no ServiceAccount
// was resolved. The restart must be refused with 409 before the agent is
// stopped or started.
func TestRestartAgent_LateKubernetesAssignWithoutServiceAccountRunsBeforeStop(t *testing.T) {
	srv, mgr, remapRuntime := newTestServerForLateCheckOrdering(t, "actual-agent-name", "url-id", "kubernetes")
	remapRuntime.RunFunc = func(ctx context.Context, config runtime.RunConfig) (string, error) {
		t.Fatal("the agent must not run once the late runtime change is refused")
		return "", nil
	}
	newTestGlobalSettings(t, testLateCheckMappingGlobalSettingsYAML)

	w := postRestart(t, srv, "url-id")

	if w.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, w.Code, w.Body.String())
	}
	if mgr.stopCalls != 0 {
		t.Errorf("expected the agent not to be stopped, got %d stop calls", mgr.stopCalls)
	}
}

// TestBuildStartContext_KubernetesAssignRuntimeEntryMappingForNamedProfile
// covers a broker without ForceRuntime whose default profile is docker: a
// request naming the Kubernetes profile resolves the ServiceAccount from
// that profile's runtime entry, with no profile-level mapping configured.
func TestBuildStartContext_KubernetesAssignRuntimeEntryMappingForNamedProfile(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv, _ := newTestServerForStartContextMultiProfile(t, cfg, "docker", "team", "kubernetes")
	newTestGlobalSettings(t, testLateCheckMappingGlobalSettingsYAML)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-k8s-assign-runtime-entry",
		Config: &CreateAgentConfig{
			Profile: "team",
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
			},
		},
		HTTPRequest: httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected the dispatch to be accepted, got %v", err)
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
		t.Errorf("expected the runtime entry's mapping 'agent-worker-ksa', got %q", got)
	}
	want := dispatchProfileSelection{ProfileName: "team", RuntimeEntryName: "kubernetes"}
	if sc.AssignSelection == nil || *sc.AssignSelection != want {
		t.Errorf("expected AssignSelection %+v, got %+v", want, sc.AssignSelection)
	}
}

// TestBuildStartContext_KubernetesAssignProfileSelectsSecondRuntimeEntry
// covers a profile that selects its own Kubernetes runtime entry, whose
// namespace and mapping differ from the default entry's. Both the namespace
// and the mapping must come from the entry the profile selects.
func TestBuildStartContext_KubernetesAssignProfileSelectsSecondRuntimeEntry(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv, dotScion := newTestServerForStartContextMultiProfile(t, cfg, "kubernetes", "team", "kubernetes")
	const projectSettingsYAML = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: k8s-default
    team:
        runtime: k8s-team
runtimes:
    k8s-default:
        type: kubernetes
    k8s-team:
        type: kubernetes
`
	if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(projectSettingsYAML), 0644); err != nil {
		t.Fatal(err)
	}
	newTestGlobalSettings(t, `schema_version: "1"
runtimes:
    k8s-default:
        namespace: ns-default
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: default-ksa
    k8s-team:
        namespace: ns-team
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: team-ksa
`)

	for _, tc := range []struct {
		name      string
		profile   string
		namespace string
		wantKSA   string
		wantErr   bool
	}{
		{name: "team profile uses the team entry", profile: "team", namespace: "ns-team", wantKSA: "team-ksa"},
		{name: "team profile refuses the default entry's namespace", profile: "team", namespace: "ns-default", wantErr: true},
		{name: "default profile uses the default entry", profile: "", namespace: "ns-default", wantKSA: "default-ksa"},
		{name: "default profile refuses the team entry's namespace", profile: "", namespace: "ns-team", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name: "agent-k8s-assign-second-entry",
				Config: &CreateAgentConfig{
					Profile: tc.profile,
					GCPIdentity: &GCPIdentityConfig{
						MetadataMode: "assign",
						SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
					},
					Kubernetes: &api.KubernetesConfig{Namespace: tc.namespace},
				},
				HTTPRequest: httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil),
				Operation:   opCreate,
			})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected namespace %q to be refused, got nil", tc.namespace)
				}
				if !strings.Contains(err.Error(), "explicit Kubernetes namespace") {
					t.Errorf("expected a namespace refusal, got %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("expected the dispatch to be accepted, got %v", err)
			}
			if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != tc.wantKSA {
				t.Errorf("expected ServiceAccount %q, got %q", tc.wantKSA, got)
			}
		})
	}
}

// TestBuildStartContext_KubernetesAssignProjectRuntimeEntryOverrideRefused
// covers a project whose own settings.yaml overrides the namespace or
// context of the runtime entry the global mapping was read from. The pod
// would be placed with the project's value, so the dispatch is refused
// with 400 naming the entry and both values.
func TestBuildStartContext_KubernetesAssignProjectRuntimeEntryOverrideRefused(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override string
		want     []string
	}{
		{name: "namespace override", override: "namespace: ns-project", want: []string{`"k8s-team"`, `"ns-project"`, `"ns-team"`, "namespace"}},
		{name: "context override", override: "context: ctx-project", want: []string{`"k8s-team"`, `"ctx-project"`, `"ctx-team"`, "context"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv, dotScion := newTestServerForStartContextMultiProfile(t, cfg, "kubernetes", "team", "kubernetes")
			projectSettingsYAML := `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: k8s-default
    team:
        runtime: k8s-team
runtimes:
    k8s-default:
        type: kubernetes
    k8s-team:
        type: kubernetes
        ` + tc.override + "\n"
			if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(projectSettingsYAML), 0644); err != nil {
				t.Fatal(err)
			}
			newTestGlobalSettings(t, `schema_version: "1"
runtimes:
    k8s-team:
        namespace: ns-team
        context: ctx-team
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: team-ksa
`)

			_, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name: "agent-k8s-assign-project-override",
				Config: &CreateAgentConfig{
					Profile: "team",
					GCPIdentity: &GCPIdentityConfig{
						MetadataMode: "assign",
						SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
					},
				},
				HTTPRequest: httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil),
				Operation:   opCreate,
			})
			var sce *startContextError
			if !errors.As(err, &sce) || sce.Status != http.StatusBadRequest {
				t.Fatalf("expected a 400 startContextError, got %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(sce.Message, want) {
					t.Errorf("expected the message to contain %s, got %q", want, sce.Message)
				}
			}
		})
	}
}

// TestBuildStartContext_KubernetesAssignForceRuntimeNamespaceMismatchRefused
// covers a ForceRuntime broker: the pod is placed in the forced Kubernetes
// runtime's own namespace, so a global runtime entry that resolves another
// namespace is refused with 400, and a matching one is accepted.
func TestBuildStartContext_KubernetesAssignForceRuntimeNamespaceMismatchRefused(t *testing.T) {
	for _, tc := range []struct {
		name        string
		runtimeNS   string
		wantRefused bool
	}{
		{name: "runtime namespace differs", runtimeNS: "ns-actual", wantRefused: true},
		{name: "runtime namespace matches", runtimeNS: "ns-global", wantRefused: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
			srv.runtime = &runtime.KubernetesRuntime{DefaultNamespace: tc.runtimeNS}
			projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
			newTestGlobalSettings(t, `schema_version: "1"
runtimes:
    kubernetes:
        namespace: ns-global
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: agent-worker-ksa
`)

			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:        "agent-k8s-assign-force-runtime",
				ProjectPath: projectDir,
				Config: &CreateAgentConfig{
					GCPIdentity: &GCPIdentityConfig{
						MetadataMode: "assign",
						SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
					},
				},
				HTTPRequest: httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil),
				Operation:   opCreate,
			})
			if !tc.wantRefused {
				if err != nil {
					t.Fatalf("expected acceptance, got %v", err)
				}
				if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
					t.Errorf("expected ServiceAccount %q, got %q", "agent-worker-ksa", got)
				}
				return
			}
			var sce *startContextError
			if !errors.As(err, &sce) || sce.Status != http.StatusBadRequest {
				t.Fatalf("expected a 400 startContextError, got %v", err)
			}
			for _, want := range []string{`"kubernetes"`, `"ns-actual"`, `"ns-global"`} {
				if !strings.Contains(sce.Message, want) {
					t.Errorf("expected the message to contain %s, got %q", want, sce.Message)
				}
			}
		})
	}
}

// TestBuildStartContext_KubernetesAssignIgnoresGlobalDirProjectConfig puts
// a project-id file in the broker's global directory, plus that project's
// split-storage settings.yaml under project-configs with its own mapping
// and namespace. Only the global settings file is read for the mapping and
// the namespace, so neither value from the project-configs file is used.
func TestBuildStartContext_KubernetesAssignIgnoresGlobalDirProjectConfig(t *testing.T) {
	for _, tc := range []struct {
		name       string
		globalYAML string
		namespace  string
		wantKSA    string
		wantErr    string
	}{
		{
			name:       "global maps the GSA: the global ServiceAccount is used",
			globalYAML: testKubernetesMappingGlobalSettingsYAML,
			wantKSA:    "agent-worker-ksa",
		},
		{
			name:       "global has no mapping: the project-configs mapping is not used",
			globalYAML: "schema_version: \"1\"\n",
			wantErr:    "no Kubernetes ServiceAccount mapped",
		},
		{
			name:       "the project-configs namespace is not used",
			globalYAML: testKubernetesMappingGlobalSettingsYAML,
			namespace:  "ns-project-config",
			wantErr:    "explicit Kubernetes namespace",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCION_K8S_NAMESPACE", "ns-runtime-default")
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
			projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
			newTestGlobalSettings(t, tc.globalYAML)

			globalDir := filepath.Join(os.Getenv("HOME"), ".scion")
			if err := os.WriteFile(filepath.Join(globalDir, "project-id"), []byte("11111111-2222-3333-4444-555555555555\n"), 0644); err != nil {
				t.Fatal(err)
			}
			externalDir, err := config.GetGitProjectExternalConfigDir(globalDir)
			if err != nil || externalDir == "" {
				t.Fatalf("resolving the project-configs directory: %q, %v", externalDir, err)
			}
			if err := os.MkdirAll(externalDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(externalDir, "settings.yaml"), []byte(`schema_version: "1"
runtimes:
    kubernetes:
        namespace: ns-project-config
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: project-config-ksa
`), 0644); err != nil {
				t.Fatal(err)
			}

			createCfg := &CreateAgentConfig{
				GCPIdentity: &GCPIdentityConfig{
					MetadataMode: "assign",
					SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
				},
			}
			if tc.namespace != "" {
				createCfg.Kubernetes = &api.KubernetesConfig{Namespace: tc.namespace}
			}
			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:        "agent-k8s-assign-global-dir-project-config",
				ProjectPath: projectDir,
				Config:      createCfg,
				HTTPRequest: httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil),
				Operation:   opCreate,
			})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected an error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected acceptance, got %v", err)
			}
			if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != tc.wantKSA {
				t.Errorf("expected ServiceAccount %q, got %q", tc.wantKSA, got)
			}
		})
	}
}

// TestStartAgent_LateKubernetesAssignRuntimeChangeWithoutProjectPath covers
// a start whose project path is not resolved: the agent record carries no
// project path and the request names none, so the project is found from
// the working directory. buildStartContext still reads the saved profile
// under the URL id, while startAgent leaves the profile unset for its later
// resolution, which falls back to the active profile. When the two differ
// in runtime, or in runtime entry on Kubernetes, the start must be refused
// with 409 before the agent runs.
func TestStartAgent_LateKubernetesAssignRuntimeChangeWithoutProjectPath(t *testing.T) {
	const twoEntryProjectSettingsYAML = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
    team:
        runtime: k8s-team
runtimes:
    kubernetes:
        type: kubernetes
    k8s-team:
        type: kubernetes
`
	const twoEntryGlobalSettingsYAML = `schema_version: "1"
runtimes:
    kubernetes:
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: agent-worker-ksa
    k8s-team:
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: team-ksa
`
	for _, tc := range []struct {
		name            string
		projectSettings string
		savedProfile    string
		globalSettings  string
	}{
		{name: "active profile is docker", savedProfile: "local", globalSettings: testLateCheckMappingGlobalSettingsYAML},
		{name: "active profile is another Kubernetes entry", projectSettings: twoEntryProjectSettingsYAML, savedProfile: "team", globalSettings: twoEntryGlobalSettingsYAML},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, mgr, remapRuntime := newTestServerForLateCheckOrdering(t, "actual-agent-name", "url-id", "kubernetes")
			remapRuntime.RunFunc = func(ctx context.Context, config runtime.RunConfig) (string, error) {
				t.Fatal("the agent must not run once the late runtime change is refused")
				return "", nil
			}
			dotScion := mgr.agents[0].ProjectPath
			mgr.agents[0].ProjectPath = ""
			if tc.projectSettings != "" {
				if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(tc.projectSettings), 0644); err != nil {
					t.Fatal(err)
				}
			}
			writeSavedAgentProfile(t, dotScion, "url-id", tc.savedProfile)
			t.Chdir(filepath.Dir(dotScion))
			newTestGlobalSettings(t, tc.globalSettings)

			body, err := json.Marshal(map[string]any{"resolvedEnv": assignResolvedEnv})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/url-id/start", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if w.Code != http.StatusConflict {
				t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, w.Code, w.Body.String())
			}
		})
	}
}

// TestBuildStartContext_KubernetesAssignProjectSettingsLoadError covers a
// project settings file that fails to load: the dispatch reports the
// settings error rather than a missing ServiceAccount mapping.
func TestBuildStartContext_KubernetesAssignProjectSettingsLoadError(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	// The broker's default runtime is Kubernetes, so the dispatch still
	// reaches Kubernetes when the project settings fail to load.
	srv, dotScion := newTestServerForStartContextMultiProfile(t, cfg, "kubernetes", "team", "kubernetes")
	newTestGlobalSettings(t, `schema_version: "1"
runtimes:
    kubernetes:
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: agent-worker-ksa
`)
	if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte("schema_version: \"1\"\nactive_profile: local\nprofiles:\n    - not-a-map\n"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-k8s-assign-settings-load-error",
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
			},
		},
		HTTPRequest: httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatal("expected a settings load error, got nil")
	}
	if !strings.Contains(err.Error(), "loading the project's settings") {
		t.Errorf("expected the project settings load error, got %q", err.Error())
	}
	if strings.Contains(err.Error(), "no Kubernetes ServiceAccount mapped") {
		t.Errorf("expected no missing-mapping error, got %q", err.Error())
	}
}
