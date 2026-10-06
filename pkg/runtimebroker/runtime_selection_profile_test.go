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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// runtimeProfileFixture builds a docker-default broker whose project settings
// map profile "prov" to docker and profile "saved" to Kubernetes, plus an
// agent dir whose agent-info.json names "saved".
// provenance, when non-empty, is written as the agent's broker-side
// image-provenance.json.
func runtimeProfileFixture(t *testing.T, provenance string) (*Server, string, string) {
	t.Helper()
	srv := newDiscoveryTestServer(t, "docker")
	srv.config.ForceRuntime = ""
	srv.resolveAuxiliaryRuntime = func(_, _, _ string) runtime.Runtime {
		return fakeKubernetesRuntime("cluster-saved", "ns-saved")
	}
	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, `schema_version: "1"
active_profile: prov
profiles:
  prov:
    runtime: local-docker
  saved:
    runtime: k8s-saved
runtimes:
  local-docker:
    type: docker
  k8s-saved:
    type: kubernetes
    context: cluster-saved
    namespace: ns-saved
`)
	const id = "saved-profile-agent"
	agentDir := filepath.Join(projectDir, "agents", id)
	if err := os.MkdirAll(filepath.Join(agentDir, "home"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{"harness_config": "x"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "home", "agent-info.json"), []byte(`{"name": "saved-profile-agent", "profile": "saved"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if provenance != "" {
		if err := os.WriteFile(filepath.Join(agentDir, "image-provenance.json"), []byte(provenance), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return srv, projectDir, id
}

// TestRuntimeSelectionOpts_ProvenanceProfileSelectsRuntime pins
// ptone/scion#1799: rewriting agent-info.json's profile to one that selects
// a different runtime must not change the runtime a provenance-recorded
// agent starts on. That runtime decides whether the bare-image local-exists
// check runs, and so whether the image_registry prefix is applied.
func TestRuntimeSelectionOpts_ProvenanceProfileSelectsRuntime(t *testing.T) {
	srv, projectDir, id := runtimeProfileFixture(t, `{"version": 1, "profile": "prov"}`)

	saved := agent.GetSavedProfile(id, projectDir)
	if saved != "saved" {
		t.Fatalf("precondition: saved profile = %q, want saved", saved)
	}
	opts := api.StartOptions{Name: id, ProjectPath: projectDir, Profile: saved}
	runtimeOpts, _, err := runtimeSelectionOpts(opts, id, false)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeOpts.Profile != "prov" {
		t.Fatalf("runtime-selection profile = %q, want the provisioned profile prov", runtimeOpts.Profile)
	}
	if opts.Profile != "saved" {
		t.Fatalf("runtimeSelectionOpts must not mutate opts; Profile = %q", opts.Profile)
	}
	if _, rtType := srv.resolveManagerForOpts(runtimeOpts); rtType != "docker" {
		t.Fatalf("runtime = %q, want the provisioned docker runtime", rtType)
	}
}

// TestRuntimeSelectionOpts_LegacyAgentKeepsSavedProfile: with no provenance
// file the saved profile still selects the runtime (pre-existing
// behaviour), which also shows the saved profile would otherwise apply.
func TestRuntimeSelectionOpts_LegacyAgentKeepsSavedProfile(t *testing.T) {
	srv, projectDir, id := runtimeProfileFixture(t, "")
	opts := api.StartOptions{Name: id, ProjectPath: projectDir, Profile: agent.GetSavedProfile(id, projectDir)}
	runtimeOpts, _, err := runtimeSelectionOpts(opts, id, false)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeOpts.Profile != "saved" {
		t.Fatalf("legacy runtime-selection profile = %q, want the saved profile", runtimeOpts.Profile)
	}
	if _, rtType := srv.resolveManagerForOpts(runtimeOpts); rtType != "kubernetes" {
		t.Fatalf("legacy runtime = %q, want kubernetes", rtType)
	}
}

// TestRuntimeSelectionOpts_UnusableProvenanceFailsClosed: an existing but
// unusable provenance file is an error, never a fallback to the saved
// profile.
func TestRuntimeSelectionOpts_UnusableProvenanceFailsClosed(t *testing.T) {
	for _, body := range []string{"{not json", `{"profile": "prov"}`} {
		_, projectDir, id := runtimeProfileFixture(t, body)
		_, _, err := runtimeSelectionOpts(api.StartOptions{Name: id, ProjectPath: projectDir, Profile: "saved"}, id, false)
		if err == nil || !strings.Contains(err.Error(), "re-provision the agent") {
			t.Fatalf("body %q: expected an actionable provenance error, got %v", body, err)
		}
	}
}

// TestStartAgent_UnusableImageProvenanceIsConflict: the start handler fails
// closed with 409 (re-provision) on an unusable provenance file and never
// calls Manager.Start.
func TestStartAgent_UnusableImageProvenanceIsConflict(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	projectDir := t.TempDir()
	agentDir := filepath.Join(projectDir, "agents", "test-agent-1")
	if err := os.MkdirAll(filepath.Join(agentDir, "home"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "image-provenance.json"), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}

	body := `{"projectPath": ` + strconvQuote(projectDir) + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "re-provision the agent") {
		t.Fatalf("expected 409 with a re-provision hint, got %d: %s", w.Code, w.Body.String())
	}
	if mgr.startCalls != 0 {
		t.Fatalf("Start must not be called, got %d calls", mgr.startCalls)
	}
}

func strconvQuote(s string) string { return strconv.Quote(s) }

// classificationFixture is a docker-default broker whose project maps
// profile "local" to docker and "kube" to Kubernetes; the agent's
// agent-info.json saves "kube". provenance, when
// non-empty, is written as the agent's broker-side image-provenance.json.
func classificationFixture(t *testing.T, provenance string) (*Server, string, string) {
	t.Helper()
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv, dotScion := newTestServerForStartContextMultiProfile(t, cfg, "docker", "kube", "kubernetes")
	const name = "classified-agent"
	writeSavedAgentProfile(t, dotScion, name, "kube")
	agentDir := filepath.Join(dotScion, "agents", name)
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if provenance != "" {
		if err := os.WriteFile(filepath.Join(agentDir, "image-provenance.json"), []byte(provenance), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return srv, dotScion, name
}

func buildClassifiedStart(t *testing.T, srv *Server, dotScion, name string) (*startContext, error) {
	t.Helper()
	return srv.buildStartContext(context.Background(), startContextInputs{
		Name:        name,
		ProjectPath: dotScion,
		// No SCION_METADATA_MODE: the runtime-dependent default applies, the
		// path an older hub that omits the mode would take.
		ResolvedEnv: map[string]string{},
		HTTPRequest: httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+name+"/start", nil),
		Operation:   opHTTPStart,
	})
}

// TestBuildStartContext_ClassifiesRuntimeWithProvisionedProfile: start/restart
// classify the runtime with the provisioned profile. With the provenance
// profile on docker and agent-info.json's profile on Kubernetes, the default
// GCP metadata mode is block with the metadata redirect, not the Kubernetes
// passthrough default.
func TestBuildStartContext_ClassifiesRuntimeWithProvisionedProfile(t *testing.T) {
	srv, dotScion, name := classificationFixture(t, `{"version": 1, "profile": "local"}`)
	sc, err := buildClassifiedStart(t, srv, dotScion, name)
	if err != nil {
		t.Fatal(err)
	}
	if sc.RuntimeType != "docker" {
		t.Errorf("classified runtime = %q, want docker (the provisioned profile's)", sc.RuntimeType)
	}
	env := sc.Opts.Env
	if env["SCION_METADATA_MODE"] != "block" || env["GCE_METADATA_HOST"] != "localhost:18380" || env["GCE_METADATA_ROOT"] != "localhost:18380" {
		t.Errorf("metadata env = mode %q host %q root %q, want block with the localhost:18380 redirect",
			env["SCION_METADATA_MODE"], env["GCE_METADATA_HOST"], env["GCE_METADATA_ROOT"])
	}
}

// TestBuildStartContext_CorruptProvenanceIsConflict: an unusable provenance
// file fails buildStartContext with 409 (re-provision), never a fallback to
// the saved profile.
func TestBuildStartContext_CorruptProvenanceIsConflict(t *testing.T) {
	srv, dotScion, name := classificationFixture(t, "{not json")
	_, err := buildClassifiedStart(t, srv, dotScion, name)
	sce, ok := err.(*startContextError)
	if !ok || sce.Status != http.StatusConflict || !strings.Contains(sce.Message, "re-provision the agent") {
		t.Fatalf("expected a 409 re-provision startContextError, got %v", err)
	}
}

// TestBuildStartContext_LegacyAgentClassifiesWithSavedProfile: an agent with
// no provenance file keeps the saved-profile classification (pre-existing
// behaviour): Kubernetes, with its passthrough default.
func TestBuildStartContext_LegacyAgentClassifiesWithSavedProfile(t *testing.T) {
	srv, dotScion, name := classificationFixture(t, "")
	sc, err := buildClassifiedStart(t, srv, dotScion, name)
	if err != nil {
		t.Fatal(err)
	}
	if sc.RuntimeType != "kubernetes" || sc.Opts.Env["SCION_METADATA_MODE"] != "passthrough" {
		t.Errorf("legacy classification = runtime %q mode %q, want kubernetes / passthrough", sc.RuntimeType, sc.Opts.Env["SCION_METADATA_MODE"])
	}
}

// TestRejectRuntimeClassificationMismatch: the late check refuses a start
// whose preliminary classification differs from the authoritative runtime,
// treating Kubernetes spellings as one runtime.
func TestRejectRuntimeClassificationMismatch(t *testing.T) {
	for _, tc := range []struct {
		early, late string
		reject      bool
	}{
		{"docker", "docker", false},
		{"kubernetes", "k8s", false},
		{"docker", "kubernetes", true},
		{"kubernetes", "docker", true},
		{"podman", "docker", true},
	} {
		sce := rejectRuntimeClassificationMismatch(tc.early, tc.late)
		if (sce != nil) != tc.reject {
			t.Errorf("%s vs %s: rejected=%v, want %v", tc.early, tc.late, sce != nil, tc.reject)
		}
		if sce != nil && sce.Status != http.StatusConflict {
			t.Errorf("%s vs %s: status %d, want 409", tc.early, tc.late, sce.Status)
		}
	}
}

// TestRestartAgent_ClassificationMismatchIsConflictBeforeStop: when the
// preliminary classification (buildStartContext) and the authoritative
// runtime disagree — here the fixture's early lookup sees docker and the late
// one Kubernetes — and no more specific rejection applies, the restart is
// refused with 409 before the agent is stopped.
func TestRestartAgent_ClassificationMismatchIsConflictBeforeStop(t *testing.T) {
	srv, mgr, remapRuntime := newTestServerForLateCheckOrdering(t, "actual-agent-name", "url-id", "kubernetes")
	remapRuntime.RunFunc = func(ctx context.Context, config runtime.RunConfig) (string, error) {
		t.Fatal("Start must not run on a classification mismatch")
		return "", nil
	}
	body := `{"resolvedEnv": {"SCION_METADATA_MODE": "passthrough", "SCION_METADATA_MODE_SOURCE": "hub"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/url-id/restart", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "differs from the runtime it resolves to") {
		t.Fatalf("expected 409 classification mismatch, got %d: %s", w.Code, w.Body.String())
	}
	if mgr.stopCalls != 0 {
		t.Errorf("expected no stop before the mismatch rejection, got %d", mgr.stopCalls)
	}
}

// handlerProfileFixture builds a broker whose default runtime ("docker") is
// backed by a mock manager, with profile "prov" -> docker and "saved" ->
// Kubernetes (an auxiliary runtime whose construction is counted). The agent
// "saved-profile-agent" is listed with its project path (so restart reaches the
// runtime selection) and its agent-info.json names
// "saved". provenance, when non-empty, is its broker-side
// image-provenance.json.
func handlerProfileFixture(t *testing.T, provenance string) (*Server, *mockManager, *int, string) {
	t.Helper()
	clearSCIONEnv(t)
	t.Setenv("HOME", t.TempDir())
	dotScion := filepath.Join(t.TempDir(), ".scion")
	writeProjectSettings(t, dotScion, `schema_version: "1"
active_profile: prov
profiles:
  prov:
    runtime: local-docker
  saved:
    runtime: k8s-saved
runtimes:
  local-docker:
    type: docker
  k8s-saved:
    type: kubernetes
`)
	const id = "saved-profile-agent"
	agentDir := filepath.Join(dotScion, "agents", id)
	if err := os.MkdirAll(filepath.Join(agentDir, "home"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "home", "agent-info.json"), []byte(`{"name": "saved-profile-agent", "profile": "saved"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if provenance != "" {
		if err := os.WriteFile(filepath.Join(agentDir, "image-provenance.json"), []byte(provenance), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	mgr := &mockManager{agents: []api.AgentInfo{{
		ID: id, Name: id, ContainerID: id, ProjectPath: dotScion, Phase: "running",
		Labels: map[string]string{"scion.name": id},
	}}}
	srv := New(cfg, mgr, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
	auxCalls := 0
	srv.resolveAuxiliaryRuntime = func(_, _, _ string) runtime.Runtime {
		auxCalls++
		return &runtime.ErrorRuntime{Err: errors.New("auxiliary runtime must not be used in this test")}
	}
	return srv, mgr, &auxCalls, dotScion
}

func postAgentOp(t *testing.T, srv *Server, op, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/saved-profile-agent/"+op, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// TestStartRestart_ProvenanceProfileSelectsDefaultRuntime pins the handler
// wiring (round-6 finding 2): for a provenance agent whose agent-info.json
// names the Kubernetes profile, start and restart run on the provisioned
// profile's docker runtime: the default mock manager starts it and the
// auxiliary (Kubernetes) runtime is never constructed.
func TestStartRestart_ProvenanceProfileSelectsDefaultRuntime(t *testing.T) {
	for _, op := range []string{"start", "restart"} {
		t.Run(op, func(t *testing.T) {
			srv, mgr, auxCalls, dotScion := handlerProfileFixture(t, `{"version": 1, "profile": "prov"}`)
			w := postAgentOp(t, srv, op, `{"projectPath": `+strconvQuote(dotScion)+`}`)
			if w.Code != http.StatusAccepted {
				t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
			}
			if mgr.startCalls != 1 {
				t.Errorf("expected the default (docker) manager to start the agent, got %d calls", mgr.startCalls)
			}
			if *auxCalls != 0 {
				t.Errorf("the Kubernetes runtime named by agent-info.json must not be resolved, got %d resolutions", *auxCalls)
			}
		})
	}
}

// TestStartRestart_LegacyAgentFollowsSavedProfile: without a provenance file
// the saved (agent-info.json) profile still selects the runtime, so the
// Kubernetes auxiliary runtime is resolved (pre-existing behaviour).
func TestStartRestart_LegacyAgentFollowsSavedProfile(t *testing.T) {
	for _, op := range []string{"start", "restart"} {
		t.Run(op, func(t *testing.T) {
			srv, _, auxCalls, dotScion := handlerProfileFixture(t, "")
			postAgentOp(t, srv, op, `{"projectPath": `+strconvQuote(dotScion)+`}`)
			if *auxCalls == 0 {
				t.Errorf("expected the saved profile's Kubernetes runtime to be resolved for a legacy agent")
			}
		})
	}
}

// TestRestartAgent_UnusableImageProvenanceIsConflictBeforeStop: restart with
// an unusable provenance file returns 409 before any Stop or Start, and the
// response does not reveal the broker host path.
func TestRestartAgent_UnusableImageProvenanceIsConflictBeforeStop(t *testing.T) {
	srv, mgr, _, dotScion := handlerProfileFixture(t, "{not json")
	w := postAgentOp(t, srv, "restart", `{}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "re-provision the agent") {
		t.Fatalf("expected 409 with a re-provision hint, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), dotScion) {
		t.Errorf("the response must not contain the broker host path: %s", w.Body.String())
	}
	if mgr.stopCalls != 0 || mgr.startCalls != 0 {
		t.Errorf("expected no Stop/Start, got stop=%d start=%d", mgr.stopCalls, mgr.startCalls)
	}
}

const sharedHubProjectID = "44444444-4444-4444-4444-444444444444"

// sharedRestartFixture is handlerProfileFixture for a shared-workspace project
// with Hub project ID sharedHubProjectID: the in-project agents root
// (<project>/.scion/agents, inside the shared workspace mount in that mode)
// holds an in-project copy of the agent dir whose provenance names "saved", and, when
// external is set, the broker-side external agent dir (located from the Hub
// project ID) holds the real provenance naming "prov".
func sharedRestartFixture(t *testing.T, external bool) (*Server, *mockManager, *int) {
	t.Helper()
	srv, mgr, auxCalls, dotScion := handlerProfileFixture(t, `{"version": 1, "profile": "saved"}`)
	// (No project-id marker is written here: the broker locates the external
	// root from the Hub project ID alone. That a marker which disagrees does not
	// move it is pinned by config's TestAgentsRootForProject_HubProjectIDWinsOverMarker.)
	if external {
		extDir, err := config.AgentDirForProject(dotScion, "saved-profile-agent", true, sharedHubProjectID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(extDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(extDir, "scion-agent.json"), []byte(`{}`), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(extDir, "image-provenance.json"), []byte(`{"version": 1, "profile": "prov"}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return srv, mgr, auxCalls
}

func postSharedRestart(t *testing.T, srv *Server) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/saved-profile-agent/restart?projectId="+sharedHubProjectID,
		strings.NewReader(`{"sharedWorkspace": true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// TestRestartAgent_SharedWorkspaceUsesExternalRoot pins the shared-workspace
// restart path (C-ROOT-1/3/5): the restart carries sharedWorkspace, Start
// runs with it and with the Hub project ID, and runtime selection reads the
// external (Hub-project-ID-located) record, ignoring the in-project
// copy.
func TestRestartAgent_SharedWorkspaceUsesExternalRoot(t *testing.T) {
	srv, mgr, auxCalls := sharedRestartFixture(t, true)
	w := postSharedRestart(t, srv)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	if *auxCalls != 0 {
		t.Errorf("the in-project copy's profile must not select the runtime, got %d auxiliary resolutions", *auxCalls)
	}
	if mgr.startCalls != 1 || !mgr.lastStartOpts.SharedWorkspace || mgr.lastStartOpts.HubProjectID != sharedHubProjectID {
		t.Errorf("Start must run with SharedWorkspace and the Hub project ID, got calls=%d shared=%v hubProjectID=%q",
			mgr.startCalls, mgr.lastStartOpts.SharedWorkspace, mgr.lastStartOpts.HubProjectID)
	}
}

// TestRestartAgent_SharedWorkspaceMissingExternalDirIsConflict: with the
// shared flag set and no external agent dir, the restart fails closed with
// 409 before any Stop/Start; the in-project copy is never used.
func TestRestartAgent_SharedWorkspaceMissingExternalDirIsConflict(t *testing.T) {
	srv, mgr, auxCalls := sharedRestartFixture(t, false)
	w := postSharedRestart(t, srv)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "re-provision the agent") {
		t.Fatalf("expected 409 with a re-provision hint, got %d: %s", w.Code, w.Body.String())
	}
	if mgr.stopCalls != 0 || mgr.startCalls != 0 || *auxCalls != 0 {
		t.Errorf("expected no Stop/Start/runtime resolution, got stop=%d start=%d aux=%d", mgr.stopCalls, mgr.startCalls, *auxCalls)
	}
}

// TestStartAgent_ClassificationMismatchIsConflictBeforeStart is the start twin
// of TestRestartAgent_ClassificationMismatchIsConflictBeforeStop. The legacy
// agent's saved profile ("saved") maps to Kubernetes when buildStartContext
// classifies the runtime; the auxiliary-runtime resolution that classification
// triggers then remaps "saved" to docker in the project settings, so the
// handler's authoritative resolution sees docker. The two disagree, so the
// start is refused with 409 and Start never runs.
func TestStartAgent_ClassificationMismatchIsConflictBeforeStart(t *testing.T) {
	srv, mgr, _, dotScion := handlerProfileFixture(t, "")
	srv.resolveAuxiliaryRuntime = func(_, _, _ string) runtime.Runtime {
		writeProjectSettings(t, dotScion, `schema_version: "1"
active_profile: prov
profiles:
  prov:
    runtime: local-docker
  saved:
    runtime: local-docker
runtimes:
  local-docker:
    type: docker
`)
		return &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	}
	body := `{"projectPath": ` + strconvQuote(dotScion) + `, "resolvedEnv": {"SCION_METADATA_MODE": "passthrough", "SCION_METADATA_MODE_SOURCE": "hub"}}`
	w := postAgentOp(t, srv, "start", body)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "differs from the runtime it resolves to") {
		t.Fatalf("expected 409 classification mismatch, got %d: %s", w.Code, w.Body.String())
	}
	if mgr.startCalls != 0 {
		t.Errorf("expected no Start, got %d", mgr.startCalls)
	}
}

// TestRuntimeSelectionOpts_ExternalStateWithoutSharedFlag pins final-broker
// #1: a request that does not carry sharedWorkspace for an agent whose state
// is stored in the external (Hub-project-ID) agents root classifies with that
// external record, the same directory GetAgent resolves, not with the
// in-project dir.
func TestRuntimeSelectionOpts_ExternalStateWithoutSharedFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dotScion := filepath.Join(t.TempDir(), "proj", ".scion")
	if err := os.MkdirAll(dotScion, 0755); err != nil {
		t.Fatal(err)
	}
	const name = "external-agent"
	extDir, err := config.AgentDirForProject(dotScion, name, true, sharedHubProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(extDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "scion-agent.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "image-provenance.json"), []byte(`{"version": 1, "profile": "prov"}`), 0600); err != nil {
		t.Fatal(err)
	}
	opts := api.StartOptions{Name: name, ProjectPath: dotScion, Profile: "saved", HubProjectID: sharedHubProjectID}
	runtimeOpts, _, err := runtimeSelectionOpts(opts, name, true)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeOpts.Profile != "prov" {
		t.Fatalf("runtime-selection profile = %q, want the external record's prov", runtimeOpts.Profile)
	}
}
