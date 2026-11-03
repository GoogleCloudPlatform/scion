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
	"reflect"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Tests for the Kubernetes placement defaults resolution (ptone/scion#5085).
// Test IDs are from the placement design's catalogue.

// plGlobal is a global settings file with one Kubernetes runtime entry
// (gke: context ctx-a, namespace agents) carrying placement. extra is
// appended (more runtimes entries, or a profiles section).
func plGlobal(extra string) string {
	return `schema_version: "1"
runtimes:
  gke:
    type: kubernetes
    context: ctx-a
    namespace: agents
    node_selector:
      pool: agents
    tolerations:
      - key: dedicated
        operator: Equal
        value: agents
        effect: NoSchedule
` + extra
}

// plGPUProfile is a global profiles section with a placement profile on gke.
const plGPUProfile = `profiles:
  gpu:
    runtime: gke
    node_selector:
      accel: l4
`

func plGoodRecord() config.RuntimeBuildRecord {
	return config.RuntimeBuildRecord{Present: true, EntryName: "gke", KubeconfigContext: "ctx-a"}
}

type plFixture struct {
	srv      *Server
	rt       *scionrt.KubernetesRuntime
	mgr      *mockManager
	home     string
	dotScion string
	logs     *plLogBuffer
}

// plLogBuffer is a log sink safe for concurrent writes (an async launch
// logs from its own goroutine) and reads.
type plLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *plLogBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *plLogBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// newPlacementFixture builds an agent runtime server whose default runtime
// is a Kubernetes runtime on context ctx-a, namespace agents, connected as
// conn, with build record rec, and a global settings file global ("" for
// none). The kubeconfig's current context is ctx-a, and the namespace an
// entry without one uses is agents.
func newPlacementFixture(t *testing.T, global string, conn k8s.ConnectionMode, rec config.RuntimeBuildRecord) *plFixture {
	t.Helper()
	mgr := &mockManager{}
	f := newPlacementServer(t, global, conn, rec, mgr)
	f.mgr = mgr
	return f
}

// newPlacementServer is newPlacementFixture with the agent manager mgr.
func newPlacementServer(t *testing.T, global string, conn k8s.ConnectionMode, rec config.RuntimeBuildRecord, mgr agent.Manager) *plFixture {
	t.Helper()
	clearSCIONEnv(t)
	setupTestScionEnv(t)
	t.Setenv("POD_NAMESPACE", "agents")
	oldOverlay := config.GetGlobalSettingsOverlay()
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(oldOverlay) })
	config.SetGlobalSettingsOverlay(nil)

	home := os.Getenv("HOME")
	if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &plFixture{home: home, logs: &plLogBuffer{}}
	if global != "" {
		f.writeGlobal(t, global)
	}
	f.writeKubeconfig(t, "ctx-a")

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	f.dotScion = filepath.Join(wd, ".scion")

	f.rt = fakeKubernetesRuntime("ctx-a", "agents")
	f.rt.Client.Connection = conn
	cfg := testServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	f.srv = New(cfg, mgr, f.rt, rec)
	f.srv.agentLifecycleLog = slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	endAsyncLaunchesAtCleanup(t, f.srv)
	return f
}

func (f *plFixture) writeGlobal(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(f.home, ".scion", "settings.yaml")
	_ = os.RemoveAll(path)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// makeGlobalUnreadable replaces the global settings file with a directory.
func (f *plFixture) makeGlobalUnreadable(t *testing.T) {
	t.Helper()
	path := filepath.Join(f.home, ".scion", "settings.yaml")
	_ = os.RemoveAll(path)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func (f *plFixture) writeKubeconfig(t *testing.T, current string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	content := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: c
contexts:
- context:
    cluster: c
    user: u
  name: ctx-a
- context:
    cluster: c
    user: u
  name: ctx-b
current-context: ` + current + `
users:
- name: u
  user:
    token: t
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
}

// res is a resolution to the fixture's default holder.
func (f *plFixture) res(how resolutionKind, profile, dispatchEntry string) runtimeResolution {
	mgr, rt, rec := f.srv.defaultPair()
	return runtimeResolution{Manager: mgr, Runtime: rt, Record: rec, RuntimeType: rt.Name(), How: how,
		Profile: profile, DispatchEntry: dispatchEntry, Agent: "agent-1"}
}

// projectSettings makes the resolver read vs as the project settings.
func (f *plFixture) projectSettings(vs *config.VersionedSettings, err error) {
	f.srv.loadSettings = func(string) (*config.VersionedSettings, []string, error) { return vs, nil, err }
}

func plProjectVS(activeProfile string, profiles map[string]config.V1ProfileConfig) *config.VersionedSettings {
	return &config.VersionedSettings{
		ActiveProfile: activeProfile,
		Profiles:      profiles,
		Runtimes: map[string]config.V1RuntimeConfig{
			"gke": {Type: "kubernetes", Context: "ctx-a", Namespace: "agents"},
		},
	}
}

func wantEntryPlacement(t *testing.T, rp *api.ResolvedPlacement, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("want the entry placement, got error %v", err)
	}
	if rp == nil || rp.NodeSelector["pool"] != "agents" || len(rp.Tolerations) != 1 || rp.Tolerations[0].Key != "dedicated" {
		t.Fatalf("want the gke entry placement, got %+v", rp)
	}
	if _, ok := rp.NodeSelector["accel"]; ok {
		t.Fatalf("a profile rung was applied: %+v", rp)
	}
}

func wantRefusal(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want a refusal containing %q, got none", substr)
	}
	var sce *startContextError
	if !errors.As(err, &sce) || sce.Status != http.StatusBadRequest || sce.Code != ErrCodeValidationError {
		t.Fatalf("want a 400 validation_error refusal, got %T %v", err, err)
	}
	if !strings.Contains(sce.Message, substr) {
		t.Fatalf("refusal %q does not contain %q", sce.Message, substr)
	}
}

// createResolution runs the create path's resolution: the lenient resolver
// with opts.Profile = profile, settled for a create.
func (f *plFixture) createResolution(t *testing.T, profile string) runtimeResolution {
	t.Helper()
	res, err := f.srv.resolveManagerForOptsStrict(api.StartOptions{Name: "agent-1", ProjectPath: f.dotScion, Profile: profile}, profileLenient)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	return res.forCreate("agent-1")
}

// fallbackCase runs one default-fallback scenario twice: without a
// placement profile on the matched entry (entry placement applies), and with
// one (the start is refused, since the profile rung cannot be determined).
func fallbackCase(t *testing.T, setup func(f *plFixture), profile string) {
	t.Helper()
	for _, withProfile := range []bool{false, true} {
		extra := ""
		if withProfile {
			extra = plGPUProfile
		}
		f := newPlacementFixture(t, plGlobal(extra), k8s.ConnKubeconfig, plGoodRecord())
		setup(f)
		res := f.createResolution(t, profile)
		if res.How != resolutionDefaultFallback {
			t.Fatalf("How = %v, want defaultFallback", res.How)
		}
		rp, err := f.srv.resolvePlacement(context.Background(), res, nil)
		if withProfile {
			wantRefusal(t, err, "the agent's profile could not be determined")
		} else {
			wantEntryPlacement(t, rp, err)
		}
	}
}

// (1) project settings load error.
func TestResolvePlacement_ProjectSettingsLoadError_1(t *testing.T) {
	fallbackCase(t, func(f *plFixture) { f.projectSettings(nil, errors.New("project settings broken")) }, "")
}

// (2) default fallback, requested profile missing.
func TestResolvePlacement_MissingProfile_2(t *testing.T) {
	fallbackCase(t, func(f *plFixture) { f.projectSettings(plProjectVS("", nil), nil) }, "missing")
}

// (3) default fallback, the profile's runtime entry missing.
func TestResolvePlacement_MissingRuntimeEntry_3(t *testing.T) {
	fallbackCase(t, func(f *plFixture) {
		f.projectSettings(plProjectVS("", map[string]config.V1ProfileConfig{"p": {Runtime: "absent"}}), nil)
	}, "p")
}

// (6) the project settings name an active profile they do not define.
func TestResolvePlacement_MissingActiveProfile_6(t *testing.T) {
	fallbackCase(t, func(f *plFixture) { f.projectSettings(plProjectVS("ghost", nil), nil) }, "")
}

// (7) a malformed project settings file (the real loader).
func TestResolvePlacement_MalformedProjectSettings_7(t *testing.T) {
	fallbackCase(t, func(f *plFixture) {
		if err := os.WriteFile(filepath.Join(f.dotScion, "settings.yaml"), []byte("schema_version: \"1\"\nprofiles: [unclosed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}, "")
}

// (5) for an existing agent the provenance profile, not the saved one,
// decides the profile rung.
func TestResolvePlacement_ProvenanceProfileWins_5(t *testing.T) {
	f := newPlacementFixture(t, plGlobal(`profiles:
  gpu:
    runtime: gke
    node_selector:
      accel: l4
  saved:
    runtime: gke
    node_selector:
      accel: a100
`), k8s.ConnKubeconfig, plGoodRecord())
	f.projectSettings(plProjectVS("", map[string]config.V1ProfileConfig{"gpu": {Runtime: "gke"}, "saved": {Runtime: "gke"}}), nil)
	const id = "prov-agent"
	writePlacementAgent(t, f.dotScion, id, `{"version": 1, "profile": "gpu"}`, "saved", `{}`)

	opts := api.StartOptions{Name: id, ProjectPath: f.dotScion, Profile: "saved"}
	runtimeOpts, provisioned, err := runtimeSelectionOpts(opts, id, false, func() (string, error) { return opts.Profile, nil })
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.srv.resolveManagerForOptsStrict(runtimeOpts, existingAgentProfileResolution(runtimeOpts.Profile, provisioned))
	if err != nil {
		t.Fatal(err)
	}
	res = res.forExistingAgent(id)
	rp, err := f.srv.resolvePlacement(context.Background(), res, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rp.NodeSelector["accel"] != "l4" || rp.NodeSelector["pool"] != "agents" {
		t.Fatalf("want the provenance profile gpu over the entry, got %+v", rp)
	}
}

// writePlacementAgent writes an agent directory: provenance (when not
// empty), agent-info.json with savedProfile, and scion-agent.json.
func writePlacementAgent(t *testing.T, dotScion, id, provenance, savedProfile, scionAgent string) string {
	t.Helper()
	agentDir := filepath.Join(dotScion, "agents", id)
	if err := os.MkdirAll(filepath.Join(agentDir, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(scionAgent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "home", "agent-info.json"), []byte(`{"name": "`+id+`", "profile": "`+savedProfile+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if provenance != "" {
		if err := os.WriteFile(filepath.Join(agentDir, "image-provenance.json"), []byte(provenance), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return agentDir
}

// (8) the forced branch: entry rung only, even with a placement profile.
func TestResolvePlacement_ForcedEntryOnly_8(t *testing.T) {
	f := newPlacementFixture(t, plGlobal(plGPUProfile), k8s.ConnKubeconfig, plGoodRecord())
	f.srv.config.ForceRuntime = "kubernetes"
	f.projectSettings(plProjectVS("gpu", map[string]config.V1ProfileConfig{"gpu": {Runtime: "gke"}}), nil)
	res := f.createResolution(t, "gpu")
	if res.How != resolutionForced {
		t.Fatalf("How = %v, want forced", res.How)
	}
	rp, err := f.srv.resolvePlacement(context.Background(), res, nil)
	wantEntryPlacement(t, rp, err)
}

// (8b) a forced runtime that matches nothing falls through and is
// classified by what resolution did next.
func TestResolvePlacement_ForcedFallThrough_8b(t *testing.T) {
	f := newPlacementFixture(t, plGlobal(plGPUProfile), k8s.ConnKubeconfig, plGoodRecord())
	f.srv.config.ForceRuntime = "no-such-runtime"
	f.projectSettings(plProjectVS("gpu", map[string]config.V1ProfileConfig{"gpu": {Runtime: "gke"}}), nil)
	res := f.createResolution(t, "")
	if res.How != resolutionProfile || res.Profile != "gpu" {
		t.Fatalf("How = %v, Profile = %q; want profile gpu", res.How, res.Profile)
	}
	rp, err := f.srv.resolvePlacement(context.Background(), res, nil)
	if err != nil || rp.NodeSelector["accel"] != "l4" {
		t.Fatalf("want the gpu profile over the entry, got %+v, %v", rp, err)
	}
}

// (10) a global profile with placement whose entry does not match the
// runtime refuses the start.
func TestResolvePlacement_ProfileOnOtherEntry_10(t *testing.T) {
	f := newPlacementFixture(t, plGlobal(`  far:
    type: kubernetes
    context: ctx-b
    namespace: agents
profiles:
  farp:
    runtime: far
    runtime_class_name: gvisor
`), k8s.ConnKubeconfig, plGoodRecord())
	_, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionProfile, "farp", ""), nil)
	wantRefusal(t, err, "profile farp's placement targets a different runtime than the one chosen")
}

// (11) a project-only profile: refused while a placement profile exists for
// the runtime, entry placement otherwise.
func TestResolvePlacement_ProjectOnlyProfile_11(t *testing.T) {
	f := newPlacementFixture(t, plGlobal(plGPUProfile), k8s.ConnKubeconfig, plGoodRecord())
	_, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionProfile, "mine", "gke"), nil)
	wantRefusal(t, err, "profile mine is not defined in the agent runtime's settings")

	f.writeGlobal(t, plGlobal(""))
	rp, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionProfile, "mine", "gke"), nil)
	wantEntryPlacement(t, rp, err)
}

// (13) a restart after a profile change uses the profile the handler
// recomputed, not an earlier one.
func TestResolvePlacement_RestartUsesRecomputedProfile_13(t *testing.T) {
	f := newPlacementFixture(t, plGlobal(`profiles:
  gpu:
    runtime: gke
    node_selector:
      accel: l4
  gpu2:
    runtime: gke
    node_selector:
      accel: h100
`), k8s.ConnKubeconfig, plGoodRecord())
	f.projectSettings(plProjectVS("", map[string]config.V1ProfileConfig{"gpu": {Runtime: "gke"}, "gpu2": {Runtime: "gke"}}), nil)
	for _, profile := range []string{"gpu", "gpu2"} {
		res, err := f.srv.resolveExistingAgentManager(api.StartOptions{Name: "agent-1", ProjectPath: f.dotScion, Profile: profile},
			profileStrictProvisioned, pinnedRuntime{})
		if err != nil {
			t.Fatal(err)
		}
		rp, err := f.srv.resolvePlacement(context.Background(), res.forExistingAgent("agent-1"), nil)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"gpu": "l4", "gpu2": "h100"}[profile]
		if rp.NodeSelector["accel"] != want {
			t.Fatalf("profile %s: accel = %q, want %q", profile, rp.NodeSelector["accel"], want)
		}
	}
}

// (14) placement stored through the settings overlay does not help when
// the global settings file cannot be read: the start fails.
func TestResolvePlacement_OverlayWithUnreadableGlobal_14(t *testing.T) {
	f := newPlacementFixture(t, "", k8s.ConnKubeconfig, plGoodRecord())
	o := config.NewSettingsOverlay()
	o.Update(map[string]config.V1RuntimeConfig{"gke": {Type: "kubernetes", Context: "ctx-a", Namespace: "agents",
		NodeSelector: map[string]string{"pool": "agents"}}}, nil, nil)
	config.SetGlobalSettingsOverlay(o)
	f.makeGlobalUnreadable(t)
	_, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionForced, "", ""), nil)
	wantRefusal(t, err, placementGlobalUnreadableCause)
}

// (15) a Docker runtime is unaffected by an unreadable global file, and
// placement on a Docker entry has no effect beyond a warning.
func TestResolvePlacement_DockerUnaffected_15(t *testing.T) {
	f := newPlacementFixture(t, "", k8s.ConnKubeconfig, plGoodRecord())
	dock := &scionrt.MockRuntime{NameFunc: func() string { return "docker" }}
	res := runtimeResolution{Manager: f.mgr, Runtime: dock, RuntimeType: "docker", How: resolutionProfile, DispatchEntry: "dock", Agent: "agent-1"}

	f.makeGlobalUnreadable(t)
	if rp, err := f.srv.resolvePlacement(context.Background(), res, nil); rp != nil || err != nil {
		t.Fatalf("docker with an unreadable global file: got %+v, %v; want nil, nil", rp, err)
	}

	f.writeGlobal(t, "schema_version: \"1\"\nruntimes:\n  dock:\n    type: docker\n    node_selector:\n      pool: x\n")
	if rp, err := f.srv.resolvePlacement(context.Background(), res, nil); rp != nil || err != nil {
		t.Fatalf("docker entry with placement: got %+v, %v; want nil, nil", rp, err)
	}
	if !strings.Contains(f.logs.String(), "Kubernetes placement on a runtime entry is ignored") {
		t.Errorf("want a warning for placement on the docker entry; logs:\n%s", f.logs.String())
	}
}

// (16) a legacy-format global file that mentions a placement key, in any
// spelling, fails the start.
func TestResolvePlacement_LegacyGlobalMentionsKey_16(t *testing.T) {
	for _, key := range []string{"nodeSelector", "NODE_SELECTOR", "runtimeClassName"} {
		t.Run(key, func(t *testing.T) {
			f := newPlacementFixture(t, "harnesses:\n  claude:\n    image: x\n# "+key+": set here\n", k8s.ConnKubeconfig, plGoodRecord())
			_, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionForced, "", ""), nil)
			wantRefusal(t, err, placementLegacyGlobalCause)
		})
	}
}

// S1, S2: an existing agent with an empty selection profile is refused on
// start and on restart while a placement profile exists for its runtime.
// S3: without one, both get the entry placement.
func TestResolvePlacement_Unrecorded_S1_S2_S3(t *testing.T) {
	for _, withProfile := range []bool{true, false} {
		extra := ""
		if withProfile {
			extra = plGPUProfile
		}
		f := newPlacementFixture(t, plGlobal(extra), k8s.ConnKubeconfig, plGoodRecord())
		f.projectSettings(plProjectVS("gpu", map[string]config.V1ProfileConfig{"gpu": {Runtime: "gke"}}), nil)

		// Start: the strict resolver with the empty selection profile.
		start, err := f.srv.resolveManagerForOptsStrict(api.StartOptions{Name: "agent-1", ProjectPath: f.dotScion}, profileLenient)
		if err != nil {
			t.Fatal(err)
		}
		start = start.forExistingAgent("agent-1")
		// Restart: pinned to the runtime the container was found on.
		mgr, rt, rec := f.srv.defaultPair()
		restart, err := f.srv.resolveExistingAgentManager(api.StartOptions{Name: "agent-1", ProjectPath: f.dotScion}, profileLenient, pinnedRuntimeOf(mgr, rt, rec))
		if err != nil {
			t.Fatal(err)
		}
		restart = restart.forExistingAgent("agent-1")

		for leg, res := range map[string]runtimeResolution{"start": start, "restart": restart} {
			if res.How != resolutionUnrecorded {
				t.Fatalf("%s: How = %v, want unrecorded", leg, res.How)
			}
			rp, err := f.srv.resolvePlacement(context.Background(), res, nil)
			if withProfile {
				wantRefusal(t, err, "agent agent-1 has no recorded profile; profile-level placement is configured for its runtime (profiles gpu)")
				wantRefusal(t, err, "runtimes.gke")
			} else {
				wantEntryPlacement(t, rp, err)
			}
		}
	}
}

// S4: the agent is refused while the placement is on the profile, and starts
// with entry placement once it moves onto the runtime entry.
func TestResolvePlacement_UnrecordedTransition_S4(t *testing.T) {
	f := newPlacementFixture(t, plGlobal(plGPUProfile), k8s.ConnKubeconfig, plGoodRecord())
	res := f.res(resolutionUnrecorded, "", "")
	_, err := f.srv.resolvePlacement(context.Background(), res, nil)
	wantRefusal(t, err, "has no recorded profile")

	f.writeGlobal(t, `schema_version: "1"
runtimes:
  gke:
    type: kubernetes
    context: ctx-a
    namespace: agents
    node_selector:
      pool: agents
      accel: l4
profiles:
  gpu:
    runtime: gke
`)
	rp, err := f.srv.resolvePlacement(context.Background(), res, nil)
	if err != nil || rp.NodeSelector["accel"] != "l4" || rp.NodeSelector["pool"] != "agents" {
		t.Fatalf("after the move: got %+v, %v", rp, err)
	}
}

// S5: provenance with an empty profile and a non-empty saved profile, while
// a placement profile exists: refused as unrecorded on start and restart,
// and the saved profile's placement is never applied.
func TestResolvePlacement_EmptyProvenanceProfile_S5(t *testing.T) {
	f := newPlacementFixture(t, plGlobal(plGPUProfile), k8s.ConnKubeconfig, plGoodRecord())
	f.projectSettings(plProjectVS("gpu", map[string]config.V1ProfileConfig{"gpu": {Runtime: "gke"}}), nil)
	const id = "empty-prov"
	writePlacementAgent(t, f.dotScion, id, `{"version": 1, "profile": ""}`, "gpu", `{}`)
	for _, restart := range []bool{false, true} {
		runtimeOpts, provisioned, err := runtimeSelectionOpts(api.StartOptions{Name: id, ProjectPath: f.dotScion, Profile: "gpu"}, id, restart, func() (string, error) { return "gpu", nil })
		if err != nil {
			t.Fatal(err)
		}
		if runtimeOpts.Profile != "" {
			t.Fatalf("selection profile = %q, want empty (provenance)", runtimeOpts.Profile)
		}
		res, err := f.srv.resolveManagerForOptsStrict(runtimeOpts, existingAgentProfileResolution(runtimeOpts.Profile, provisioned))
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.srv.resolvePlacement(context.Background(), res.forExistingAgent(id), nil)
		wantRefusal(t, err, "agent empty-prov has no recorded profile")
	}
}

// U4: an Undetermined entry with no placement on it or its profiles is
// ignored. U7: with a placement profile on it, the start is refused.
func TestResolvePlacement_UndeterminedEntries_U4_U7(t *testing.T) {
	undetermined := `  ambiguous:
    type: kubernetes
    namespace: agents
`
	f := newPlacementFixture(t, plGlobal(undetermined), k8s.ConnKubeconfig, plGoodRecord())
	// The entry has no context, so it is matched through the kubeconfig,
	// which cannot be read: Undetermined.
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing", "kubeconfig"))
	rp, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionForced, "", ""), nil)
	wantEntryPlacement(t, rp, err) // U4

	f.writeGlobal(t, plGlobal(undetermined+`profiles:
  amb:
    runtime: ambiguous
    runtime_class_name: gvisor
`))
	_, err = f.srv.resolvePlacement(context.Background(), f.res(resolutionForced, "", ""), nil)
	wantRefusal(t, err, "cannot determine whether runtime entry ambiguous targets this agent's runtime (kubeconfig current-context could not be read)") // U7
}

// U5: the entry the runtime was built from was edited after the build (its
// context or namespace changed): refused.
func TestResolvePlacement_BuiltFromEdited_U5(t *testing.T) {
	for _, edit := range []string{"context: ctx-b\n    namespace: agents", "context: ctx-a\n    namespace: other"} {
		f := newPlacementFixture(t, `schema_version: "1"
runtimes:
  gke:
    type: kubernetes
    `+edit+`
    runtime_class_name: gvisor
`, k8s.ConnKubeconfig, plGoodRecord())
		_, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionForced, "", ""), nil)
		wantRefusal(t, err, placementBuiltFromCause)
	}
}

// U8: the dispatch entry (from the resolver's settings read) and the build
// record's entry differ, and one of them does not match: refused.
func TestResolvePlacement_DispatchEntryMismatch_U8(t *testing.T) {
	f := newPlacementFixture(t, plGlobal(`  gke-old:
    type: kubernetes
    context: ctx-b
    namespace: agents
`), k8s.ConnKubeconfig, plGoodRecord())
	_, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionProfile, "", "gke-old"), nil)
	wantRefusal(t, err, placementBuiltFromCause)
}

// G1b (start part): an entry without a context resolves to the same
// cluster as an explicit one, with different placement: refused, naming
// both. G2: with identical placement the start proceeds.
func TestResolvePlacement_ResolvedSiblings_G1b_G2(t *testing.T) {
	sibling := func(pool string) string {
		return `  gke-implicit:
    type: kubernetes
    namespace: agents
    node_selector:
      pool: ` + pool + `
    tolerations:
      - key: dedicated
        value: agents
        effect: NoSchedule
`
	}
	f := newPlacementFixture(t, plGlobal(sibling("other")), k8s.ConnKubeconfig, plGoodRecord())
	_, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionForced, "", ""), nil)
	wantRefusal(t, err, "runtime entries gke, gke-implicit target the same cluster and namespace with different placement")

	f.writeGlobal(t, plGlobal(sibling("agents")))
	rp, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionForced, "", ""), nil)
	wantEntryPlacement(t, rp, err)
	if got := rp.Sources["node_selector"]; len(got) != 2 {
		t.Errorf("sources = %v, want both matched entries", got)
	}
}

// G3: a profile on a sibling without placement cannot remove the other
// entry's placement: the start is refused, not started without placement.
func TestResolvePlacement_ProfileOnBareSibling_G3(t *testing.T) {
	f := newPlacementFixture(t, plGlobal(`  gke-bare:
    type: kubernetes
    context: ctx-a
    namespace: agents
profiles:
  bare:
    runtime: gke-bare
`), k8s.ConnKubeconfig, config.RuntimeBuildRecord{Present: true, EntryName: "gke-bare", KubeconfigContext: "ctx-a"})
	rp, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionProfile, "bare", "gke-bare"), nil)
	if rp != nil {
		t.Fatalf("started with placement %+v; want a refusal", rp)
	}
	wantRefusal(t, err, "runtime entries gke, gke-bare target the same cluster and namespace with different placement")
}

// G4: entries without any of the new fields are unaffected, whatever the
// runtime's record or connection mode.
func TestResolvePlacement_NoNewFields_G4(t *testing.T) {
	plain := `schema_version: "1"
runtimes:
  gke:
    type: kubernetes
    context: ctx-a
    namespace: agents
  implicit:
    type: kubernetes
profiles:
  p:
    runtime: gke
`
	for _, tc := range []struct {
		conn k8s.ConnectionMode
		rec  config.RuntimeBuildRecord
	}{
		{k8s.ConnKubeconfig, plGoodRecord()},
		{k8s.ConnUnknown, plGoodRecord()},
		{k8s.ConnKubeconfig, config.RuntimeBuildRecord{}},
	} {
		f := newPlacementFixture(t, plain, tc.conn, tc.rec)
		for _, how := range []resolutionKind{resolutionForced, resolutionUnrecorded, resolutionDefaultFallback, resolutionProfile} {
			if rp, err := f.srv.resolvePlacement(context.Background(), f.res(how, "p", "gke"), nil); rp != nil || err != nil {
				t.Errorf("conn %v, how %v: got %+v, %v; want nil, nil", tc.conn, how, rp, err)
			}
		}
	}
}

// F1 (client and log part): refusals carry the fixed cause only (no
// settings path, no parser text); the log carries the detail.
func TestResolvePlacement_FixedCauses_F1(t *testing.T) {
	f := newPlacementFixture(t, "", k8s.ConnKubeconfig, plGoodRecord())
	f.makeGlobalUnreadable(t)
	_, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionForced, "", ""), nil)
	wantRefusal(t, err, placementGlobalUnreadableCause)
	if err.Error() != placementGlobalUnreadableCause {
		t.Errorf("client text %q carries more than the fixed cause", err.Error())
	}
	if strings.Contains(err.Error(), f.home) || strings.Contains(err.Error(), "settings.yaml") {
		t.Errorf("client text carries a settings path: %q", err.Error())
	}
	if !strings.Contains(f.logs.String(), "global settings could not be read") || !strings.Contains(f.logs.String(), "error=") {
		t.Errorf("the log must carry the detail; logs:\n%s", f.logs.String())
	}

	// Through the start handler: the response body has the cause, not the path.
	f.writeGlobal(t, `schema_version: "1"
runtimes:
  gke:
    type: kubernetes
    context: ctx-a
    namespace: agents
    node_selector: {pool: agents}
`)
	f.srv.SwapRuntime(f.rt, config.RuntimeBuildRecord{})
	f.srv.config.ForceRuntime = "kubernetes"
	writePlacementAgent(t, f.dotScion, "agent-1", `{"version": 1, "profile": ""}`, "", `{}`)
	w := postAgentOpFor(t, f.srv, "agent-1", "start", `{"projectPath": `+strconvQuote(f.dotScion)+`}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "cannot determine whether runtime entry gke targets this agent's runtime (runtime has no build record or client)") {
		t.Fatalf("start: got %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), f.home) {
		t.Errorf("response carries a host path: %s", w.Body.String())
	}
}

func postAgentOpFor(t *testing.T, srv *Server, id, op, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+id+"/"+op, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// The project's own placement values are never applied, and are warned about.
func TestResolvePlacement_ProjectValuesIgnored(t *testing.T) {
	f := newPlacementFixture(t, plGlobal(""), k8s.ConnKubeconfig, plGoodRecord())
	project := plProjectVS("", nil)
	project.Runtimes["gke"] = config.V1RuntimeConfig{Type: "kubernetes", Context: "ctx-a", Namespace: "agents",
		NodeSelector: map[string]string{"pool": "project"}}
	rp, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionForced, "", ""), project)
	wantEntryPlacement(t, rp, err)
	if !strings.Contains(f.logs.String(), "Kubernetes placement set in project settings is ignored") {
		t.Errorf("want the project-values warning; logs:\n%s", f.logs.String())
	}
}

// A runtime that failed to build resolves no placement. A flat resolution
// on a server without an activated flat instance policy fails closed; it
// never falls back to the legacy runtime entries (see
// TestResolvePlacementFlatTarget for a flat instance).
func TestResolvePlacement_FlatAndErrorRuntime(t *testing.T) {
	f := newPlacementFixture(t, plGlobal(plGPUProfile), k8s.ConnUnknown, config.RuntimeBuildRecord{})
	if rp, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionErrorRuntime, "", ""), nil); rp != nil || err != nil {
		t.Errorf("error runtime: got %+v, %v", rp, err)
	}
	rp, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionFlat, "", ""), nil)
	if rp != nil || err == nil || !strings.Contains(err.Error(), placementFlatUnresolvedCause) {
		t.Errorf("flat without a policy: got %+v, %v", rp, err)
	}
}

// --- end to end through the agent manager: the submitted pod spec ---

// startPlacedPod builds a create's start context on f, resolves its
// placement as createAgent does, and starts the agent through the real
// agent manager, returning the submitted pod.
func startPlacedPod(t *testing.T, f *flatPodFixture, name string, gcp *GCPIdentityConfig) *corev1.Pod {
	t.Helper()
	sc, err := f.srv.buildStartContext(context.Background(), startContextInputs{
		Name: name, ProjectPath: f.projectDir, Operation: opCreate,
		Config:      &CreateAgentConfig{GCPIdentity: gcp},
		HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	opts := sc.Opts
	rp, err := f.srv.placementForStart(context.Background(), sc.Resolution, opts.ProjectPath)
	if err != nil {
		t.Fatal(err)
	}
	opts.ResolvedPlacement, opts.PlacementResolved = rp, true
	opts.BrokerMode, opts.NoAuth = true, true
	if _, err := f.mgr.Start(context.Background(), opts); !errors.Is(err, errPodCaptured) {
		t.Fatalf("start: want the captured pod create, got %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pods[len(f.pods)-1]
}

// newPlacedPodFixture is newPodFixture (a non-flat agent runtime with a
// fake Kubernetes client) whose global settings are global and whose
// runtime is connected as conn on context runtimeCtx with build record rec.
func newPlacedPodFixture(t *testing.T, global string, conn k8s.ConnectionMode, runtimeCtx string, rec config.RuntimeBuildRecord) *flatPodFixture {
	t.Helper()
	f := newPodFixture(t, "", config.V1RuntimeTargetConfig{}, false)
	oldOverlay := config.GetGlobalSettingsOverlay()
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(oldOverlay) })
	config.SetGlobalSettingsOverlay(nil)
	// SCION_K8S_NAMESPACE would win over POD_NAMESPACE.
	t.Setenv("SCION_K8S_NAMESPACE", "")
	t.Setenv("POD_NAMESPACE", "agents")
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".scion", "settings.yaml"), []byte(global), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := f.mgr.Runtime.(*scionrt.KubernetesRuntime)
	rt.DefaultNamespace = "agents"
	rt.Client.Connection = conn
	rt.Client.CurrentContext = runtimeCtx
	f.srv.SwapRuntime(rt, rec)
	return f
}

// U9e: an in-cluster runtime (empty current context) built from an entry
// with no context: the entry's placement reaches the pod spec.
func TestPlacementPod_InCluster_U9e(t *testing.T) {
	f := newPlacedPodFixture(t, `schema_version: "1"
runtimes:
  incluster:
    type: kubernetes
    node_selector:
      cloud.google.com/gke-nodepool: scion-agents
    tolerations:
      - key: dedicated
        operator: Equal
        value: scion-agents
        effect: NoSchedule
    runtime_class_name: gvisor
`, k8s.ConnInCluster, "", config.RuntimeBuildRecord{Present: true, EntryName: "incluster"})
	pod := startPlacedPod(t, f, "u9e-agent", nil)
	if pod.Spec.NodeSelector["cloud.google.com/gke-nodepool"] != "scion-agents" {
		t.Errorf("node selector %v lacks the entry's pool", pod.Spec.NodeSelector)
	}
	var tolerated bool
	for _, tol := range pod.Spec.Tolerations {
		if tol.Key == "dedicated" && tol.Value == "scion-agents" && tol.Effect == corev1.TaintEffectNoSchedule {
			tolerated = true
		}
	}
	if !tolerated {
		t.Errorf("tolerations %v lack the entry's toleration", pod.Spec.Tolerations)
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "gvisor" {
		t.Errorf("runtimeClassName = %v, want gvisor", pod.Spec.RuntimeClassName)
	}
}

// NK1: with none of the new keys anywhere, the pod's scheduling fields are
// the same as for a start without placement resolution; and the GCP
// identity "block" node label is still forced over a conflicting default.
func TestPlacementPod_NoNewKeysAndBlockLabel_NK1(t *testing.T) {
	plain := "schema_version: \"1\"\nruntimes:\n  gke:\n    type: kubernetes\n"
	f := newPlacedPodFixture(t, plain, k8s.ConnInCluster, "", config.RuntimeBuildRecord{Present: true, EntryName: "gke"})
	resolved := startPlacedPod(t, f, "nk1-agent", nil)
	if resolved.Spec.NodeSelector != nil || resolved.Spec.Tolerations != nil || resolved.Spec.RuntimeClassName != nil || resolved.Spec.Affinity != nil {
		t.Errorf("scheduling fields set without any placement: selector %v, tolerations %v, runtimeClass %v, affinity %v",
			resolved.Spec.NodeSelector, resolved.Spec.Tolerations, resolved.Spec.RuntimeClassName, resolved.Spec.Affinity)
	}
	// The same start without placement resolution (the nil hand-off, which
	// proceeds when nothing is configured) submits the same fields.
	g := newPlacedPodFixture(t, plain, k8s.ConnInCluster, "", config.RuntimeBuildRecord{Present: true, EntryName: "gke"})
	unresolved, err := g.start(t, "nk1-agent", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !podSchedulingEqual(resolved, unresolved) {
		t.Errorf("scheduling fields differ:\n resolved   %+v\n unresolved %+v", resolved.Spec, unresolved.Spec)
	}

	block := newPlacedPodFixture(t, `schema_version: "1"
runtimes:
  gke:
    type: kubernetes
    node_selector:
      `+scionrt.KubernetesWorkloadIdentityNodeLabel+`: "false"
      pool: agents
`, k8s.ConnInCluster, "", config.RuntimeBuildRecord{Present: true, EntryName: "gke"})
	pod := startPlacedPod(t, block, "nk1-block", &GCPIdentityConfig{MetadataMode: "block"})
	if pod.Spec.NodeSelector[scionrt.KubernetesWorkloadIdentityNodeLabel] != "true" {
		t.Errorf("block node label = %q, want true over the default's false", pod.Spec.NodeSelector[scionrt.KubernetesWorkloadIdentityNodeLabel])
	}
	if pod.Spec.NodeSelector["pool"] != "agents" {
		t.Errorf("the default's other keys must stay: %v", pod.Spec.NodeSelector)
	}
}

func podSchedulingEqual(a, b *corev1.Pod) bool {
	return reflect.DeepEqual(a.Spec.NodeSelector, b.Spec.NodeSelector) &&
		reflect.DeepEqual(a.Spec.Tolerations, b.Spec.Tolerations) &&
		reflect.DeepEqual(a.Spec.RuntimeClassName, b.Spec.RuntimeClassName) &&
		reflect.DeepEqual(a.Spec.Affinity, b.Spec.Affinity)
}

// --- configured runtime entries only (embedded default entries excluded) ---

// newInClusterPlacementFixture is newPlacementFixture with an in-cluster
// runtime (empty current context) built from entry recEntry.
func newInClusterPlacementFixture(t *testing.T, global, recEntry string) *plFixture {
	t.Helper()
	f := newPlacementFixture(t, global, k8s.ConnInCluster, config.RuntimeBuildRecord{Present: true, EntryName: recEntry})
	f.rt.Client.CurrentContext = ""
	return f
}

const plInClusterEntry = `schema_version: "1"
runtimes:
  incluster:
    type: kubernetes
    node_selector:
      pool: agents
`

// (i) an in-cluster runtime, a configured entry with placement for its
// identity, and the embedded default kubernetes entry (same identity, no
// placement): the placement applies and nothing is refused.
func TestResolvePlacement_DefaultsOnlyEntryIgnored(t *testing.T) {
	f := newInClusterPlacementFixture(t, plInClusterEntry, "incluster")
	vs, _, err := config.LoadGlobalSettingsWithOverlay()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := vs.Runtimes["kubernetes"]; !ok {
		t.Fatal("precondition: the embedded default kubernetes entry is in the global view")
	}
	rp, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionForced, "", ""), nil)
	if err != nil || rp == nil || rp.NodeSelector["pool"] != "agents" {
		t.Fatalf("got %+v, %v; want the incluster entry's placement", rp, err)
	}
}

// (ii) the same identity, but the operator's file defines kubernetes too,
// without placement: siblings disagree, so the start is refused (G3).
func TestResolvePlacement_ConfiguredBareSiblingStillRefuses(t *testing.T) {
	f := newInClusterPlacementFixture(t, plInClusterEntry+"  kubernetes:\n    type: kubernetes\n", "incluster")
	_, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionForced, "", ""), nil)
	wantRefusal(t, err, "runtime entries incluster, kubernetes target the same cluster and namespace with different placement")
}

// (iii) a global settings file that cannot be stat'd (the settings loader
// treats it as missing, so the settings load without it) fails closed with
// the global-unreadable cause, even when nothing in the remaining view
// carries placement.
func TestResolvePlacement_UnstatableGlobalFailsClosed(t *testing.T) {
	f := newInClusterPlacementFixture(t, "", "")
	path := filepath.Join(f.home, ".scion", "settings.yaml")
	_ = os.RemoveAll(path)
	if err := os.Symlink("settings.yaml", path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.LoadGlobalSettingsWithOverlay(); err != nil {
		t.Fatalf("precondition: the loader treats the file as missing, got %v", err)
	}
	rp, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionForced, "", ""), nil)
	if rp != nil {
		t.Fatalf("started with placement %+v", rp)
	}
	wantRefusal(t, err, placementGlobalUnreadableCause)
}

// (iv) DB-backed settings: runtimes stored in the overlay are configured, so
// a stored kubernetes entry without placement is a sibling of the stored
// entry with placement, and the start is refused.
func TestResolvePlacement_StoredRuntimesAreConfigured(t *testing.T) {
	f := newInClusterPlacementFixture(t, "schema_version: \"1\"\n", "incluster")
	o := config.NewSettingsOverlay()
	o.Update(map[string]config.V1RuntimeConfig{
		"incluster":  {Type: "kubernetes", NodeSelector: map[string]string{"pool": "agents"}},
		"kubernetes": {Type: "kubernetes"},
	}, nil, nil)
	config.SetGlobalSettingsOverlay(o)
	_, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionForced, "", ""), nil)
	wantRefusal(t, err, "runtime entries incluster, kubernetes target the same cluster and namespace with different placement")

	// Stored without the bare entry: the placement applies.
	o.Update(map[string]config.V1RuntimeConfig{"incluster": {Type: "kubernetes", NodeSelector: map[string]string{"pool": "agents"}}}, nil, nil)
	rp, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionForced, "", ""), nil)
	if err != nil || rp == nil || rp.NodeSelector["pool"] != "agents" {
		t.Fatalf("got %+v, %v", rp, err)
	}
}

// DB-backed settings whose overlay supplies the runtimes, a settings
// file that still defines kubernetes (not in effect), and built-in
// retention that keeps the embedded kubernetes entry for a referenced
// profile: only the stored runtimes are configured, so the retained entry is
// not a sibling and the stored entry's placement applies.
func TestResolvePlacement_StoredRuntimesWithRetainedBuiltin(t *testing.T) {
	f := newInClusterPlacementFixture(t, "schema_version: \"1\"\nruntimes:\n  kubernetes:\n    type: kubernetes\n", "incluster")
	o := config.NewSettingsOverlay()
	o.Update(map[string]config.V1RuntimeConfig{
		"incluster": {Type: "kubernetes", NodeSelector: map[string]string{"pool": "agents"}},
	}, map[string]config.V1ProfileConfig{
		"local":  {Runtime: "incluster"},
		"remote": {Runtime: "kubernetes"},
	}, nil)
	o.EnableConservativeRetention()
	o.AddAgentReference("remote")
	config.SetGlobalSettingsOverlay(o)
	vs, _, err := config.LoadGlobalSettingsWithOverlay()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := vs.Runtimes["kubernetes"]; !ok {
		t.Fatalf("precondition: the built-in kubernetes entry is retained; runtimes %v, retained %v", vs.Runtimes, vs.RetainedRuntimeNames())
	}
	rp, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionForced, "", ""), nil)
	if err != nil || rp == nil || rp.NodeSelector["pool"] != "agents" {
		t.Fatalf("got %+v, %v; want the stored entry's placement and no refusal", rp, err)
	}
}

// A global profile with placement whose runtime entry exists only
// in the embedded defaults (and would match this runtime) cannot apply: the
// start is refused with the dedicated cause, whether the profile was
// selected or the runtime choice fell back or recorded no profile.
func TestResolvePlacement_ProfileOnDefaultsOnlyEntry(t *testing.T) {
	const want = "profile gpu sets Kubernetes placement, but its runtime entry kubernetes is defined only by the built-in defaults; define runtimes.kubernetes in the agent runtime's settings so its placement can be matched"
	global := "schema_version: \"1\"\nprofiles:\n  gpu:\n    runtime: kubernetes\n    node_selector:\n      accel: l4\n"
	for _, tc := range []struct {
		name string
		how  resolutionKind
		prof string
	}{
		{"selected", resolutionProfile, "gpu"},
		{"fallback", resolutionDefaultFallback, ""},
		{"unrecorded", resolutionUnrecorded, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInClusterPlacementFixture(t, global, "")
			rp, err := f.srv.resolvePlacement(context.Background(), f.res(tc.how, tc.prof, "kubernetes"), nil)
			if rp != nil {
				t.Fatalf("started with placement %+v", rp)
			}
			wantRefusal(t, err, want)
		})
	}
	// The selected profile is settled: remote (no placement) on the same
	// defaults-only entry starts without placement, whatever other profiles
	// on that entry set.
	t.Run("selected profile without placement", func(t *testing.T) {
		f := newInClusterPlacementFixture(t, global+"  remote:\n    runtime: kubernetes\n", "")
		rp, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionProfile, "remote", "kubernetes"), nil)
		if rp != nil || err != nil {
			t.Fatalf("got %+v, %v; want nil, nil", rp, err)
		}
	})
	// A configured entry with the same identity (incluster, no placement)
	// whose own profile sets placement does not change that: the selected
	// remote profile on the defaults-only entry is still settled. An agent
	// with no recorded profile is still refused.
	t.Run("selected profile without placement beside a configured sibling", func(t *testing.T) {
		g := "schema_version: \"1\"\nruntimes:\n  incluster:\n    type: kubernetes\nprofiles:\n  gpu:\n    runtime: incluster\n    node_selector:\n      accel: l4\n  remote:\n    runtime: kubernetes\n"
		f := newInClusterPlacementFixture(t, g, "")
		rp, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionProfile, "remote", "kubernetes"), nil)
		if rp != nil || err != nil {
			t.Fatalf("remote selected: got %+v, %v; want nil, nil", rp, err)
		}
		_, err = f.srv.resolvePlacement(context.Background(), f.res(resolutionUnrecorded, "", ""), nil)
		wantRefusal(t, err, "agent agent-1 has no recorded profile; profile-level placement is configured for its runtime (profiles gpu)")
	})
	// Defining the entry makes it configured: the selected profile applies.
	f := newInClusterPlacementFixture(t, global+"runtimes:\n  kubernetes:\n    type: kubernetes\n", "kubernetes")
	rp, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionProfile, "gpu", "kubernetes"), nil)
	if err != nil || rp == nil || rp.NodeSelector["accel"] != "l4" {
		t.Fatalf("defined entry: got %+v, %v", rp, err)
	}
}

// Parity between a runtime entry that exists only in the built-in defaults
// and the same entry defined by the operator without placement. For every
// way the runtime was chosen, the selected profile's placement, and an
// identity-matched configured sibling (absent, present without a placement
// profile, present with one), case (x) has the runtime's matching entry
// "kubernetes" only in the built-in defaults and case (y) defines it in the
// settings file with no placement. (x) and (y) must give the same placement
// and the same error, except in the rows the defaults-only rule refuses on
// purpose (dedicated is true): there (x) must refuse with the dedicated
// cause. The error text is compared in full, including the entry an
// unrecorded refusal names: the sibling is called zsib so that it sorts
// after kubernetes, which makes that name depend on the defaults-only entry
// being considered in (x). The runtime is in-cluster, so every entry without a context in
// namespace "agents" (the default) matches it, and "far" (explicit context)
// does not.
func TestResolvePlacement_DefaultsOnlyParity(t *testing.T) {
	type kind struct {
		name    string
		how     resolutionKind
		profile string // selected profile
		farPl   bool   // profile far sets placement
	}
	kinds := []kind{
		{"profile-global", resolutionProfile, "kp", false},
		{"profile-not-global", resolutionProfile, "ghost", false},
		{"profile-elsewhere", resolutionProfile, "far", false},
		{"profile-elsewhere-with-placement", resolutionProfile, "far", true},
		{"unrecorded", resolutionUnrecorded, "", false},
		{"default-fallback", resolutionDefaultFallback, "", false},
		{"unset", resolutionUnset, "", false},
		{"forced", resolutionForced, "", false},
	}
	siblings := []string{"absent", "bare", "with-placement-profile"}
	settings := func(defineKubernetes, kpPl, farPl bool, sibling string) string {
		var b strings.Builder
		b.WriteString("schema_version: \"1\"\nruntimes:\n  far:\n    type: kubernetes\n    context: ctx-x\n")
		if defineKubernetes {
			b.WriteString("  kubernetes:\n    type: kubernetes\n")
		}
		if sibling != "absent" {
			b.WriteString("  zsib:\n    type: kubernetes\n")
		}
		b.WriteString("profiles:\n  kp:\n    runtime: kubernetes\n")
		if kpPl {
			b.WriteString("    node_selector:\n      accel: l4\n")
		}
		b.WriteString("  far:\n    runtime: far\n")
		if farPl {
			b.WriteString("    runtime_class_name: gvisor\n")
		}
		if sibling == "with-placement-profile" {
			b.WriteString("  sibp:\n    runtime: zsib\n    node_selector:\n      pool: sib\n")
		}
		return b.String()
	}
	type outcome struct {
		sel map[string]string
		rc  string
		err string
	}
	run := func(t *testing.T, global string, k kind) outcome {
		t.Helper()
		f := newInClusterPlacementFixture(t, global, "")
		rp, err := f.srv.resolvePlacement(context.Background(), f.res(k.how, k.profile, ""), nil)
		var o outcome
		if rp != nil {
			o.sel, o.rc = rp.NodeSelector, rp.RuntimeClassName
		}
		if err != nil {
			o.err = err.Error()
		}
		return o
	}
	for _, k := range kinds {
		for _, kpPl := range []bool{false, true} {
			for _, sib := range siblings {
				// The rows the defaults-only rule refuses on purpose in (x):
				// the selected global profile on the defaults-only entry sets
				// placement; or, for the kinds that do not settle the profile
				// rung (everything but a selected global profile on a matching
				// entry and the forced runtime), a profile with placement on
				// the defaults-only entry is the only one that could apply.
				dedicated := false
				switch k.name {
				case "profile-global":
					dedicated = kpPl
				case "forced":
				default:
					dedicated = kpPl && sib != "with-placement-profile" && !k.farPl
				}
				name := fmt.Sprintf("%s/kp-placement=%v/sibling=%s", k.name, kpPl, sib)
				t.Run(name, func(t *testing.T) {
					x := run(t, settings(false, kpPl, k.farPl, sib), k)
					if dedicated {
						if !strings.Contains(x.err, "profile kp sets Kubernetes placement, but its runtime entry kubernetes is defined only by the built-in defaults") {
							t.Fatalf("(x) want the dedicated cause, got %+v", x)
						}
						return
					}
					y := run(t, settings(true, kpPl, k.farPl, sib), k)
					if !reflect.DeepEqual(x, y) {
						t.Fatalf("(x) defaults-only and (y) configured differ:\n x: %+v\n y: %+v", x, y)
					}
				})
			}
		}
	}
}
