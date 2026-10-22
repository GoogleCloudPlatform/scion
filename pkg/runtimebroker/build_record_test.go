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
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Build records (Kubernetes placement defaults, ptone/scion#5085): every
// holder carries the record of the runtime it holds, and a resolver returns
// the manager, runtime and record of one holder. The records are carried,
// not consulted.

const brProjectSettings = `schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
  k8s-a:
    runtime: k8s-a
  alias:
    runtime: alias-rt
  broken:
    runtime: broken-rt
runtimes:
  docker:
    type: docker
  k8s-a:
    type: kubernetes
    context: cluster-a
    namespace: ns-a
  alias-rt:
    type: podman
  broken-rt:
    type: kubernetes
    context: broken
`

var (
	brDefaultRecord = config.RuntimeBuildRecord{Present: true, EntryName: "docker"}
	brAuxRecord     = config.RuntimeBuildRecord{Present: true, EntryName: "k8s-a", KubeconfigContext: "cluster-a"}
)

// brBuild is what the fixture's resolver builds for a profile.
type brBuild struct {
	rt  scionrt.Runtime
	rec config.RuntimeBuildRecord
}

type brFixture struct {
	srv        *Server
	defMgr     *filteringMockManager
	defRT      *scionrt.MockRuntime
	projectDir string
	builds     map[string]brBuild
}

// newBRFixture builds a server whose default holder is a "docker" runtime
// with brDefaultRecord, and whose resolver builds f.builds[profile].
func newBRFixture(t *testing.T) *brFixture {
	t.Helper()
	clearSCIONEnv(t)
	// Keep the embedded default settings on the workstation tier, whose
	// profiles (local, remote) the tests account for.
	t.Setenv("K_SERVICE", "")
	t.Setenv("CLOUD_RUN_INSTANCE", "")
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	prev := config.GetGlobalSettingsOverlay()
	config.SetGlobalSettingsOverlay(nil)
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(prev) })

	projectDir := filepath.Join(t.TempDir(), "proj", ".scion")
	writeProjectSettings(t, projectDir, brProjectSettings)

	f := &brFixture{
		defMgr:     &filteringMockManager{},
		defRT:      &scionrt.MockRuntime{NameFunc: func() string { return "docker" }},
		projectDir: projectDir,
		builds: map[string]brBuild{
			"k8s-a": {rt: fakeKubernetesRuntime("cluster-a", "ns-a"), rec: brAuxRecord},
			// Resolves to the default's identity (type and name "docker").
			"alias": {rt: &scionrt.MockRuntime{NameFunc: func() string { return "docker" }}, rec: config.RuntimeBuildRecord{Present: true, EntryName: "alias-rt"}},
			"broken": {rt: &scionrt.ErrorRuntime{Err: errors.New("cluster unreachable")},
				rec: config.RuntimeBuildRecord{Present: true, EntryName: "broken-rt"}},
		},
	}
	cfg := DefaultServerConfig()
	cfg.ForceRuntime = ""
	f.srv = New(cfg, f.defMgr, f.defRT, brDefaultRecord)
	f.srv.resolveAuxiliaryRuntime = func(_, _, profile string) (scionrt.Runtime, config.RuntimeBuildRecord) {
		b, ok := f.builds[profile]
		if !ok {
			t.Errorf("resolver called for unexpected profile %q", profile)
			return &scionrt.ErrorRuntime{Err: errors.New("unexpected profile")}, config.RuntimeBuildRecord{}
		}
		return b.rt, b.rec
	}
	return f
}

func (f *brFixture) opts(profile string) api.StartOptions {
	return api.StartOptions{Name: "agent", ProjectPath: f.projectDir, Profile: profile}
}

// registerAux registers an auxiliary holder for rt with rec, as discovery
// or full resolution does.
func (f *brFixture) registerAux(rt scionrt.Runtime, mgr agent.Manager, rec config.RuntimeBuildRecord) auxiliaryRuntime {
	aux := auxiliaryRuntime{Runtime: rt, Manager: mgr, Record: rec}
	f.srv.auxiliaryRuntimesMu.Lock()
	f.srv.auxiliaryRuntimes[auxiliaryRuntimeIdentity(rt)] = aux
	f.srv.auxiliaryRuntimesMu.Unlock()
	return aux
}

// storedAux returns the auxiliary holder stored for rt's identity.
func (f *brFixture) storedAux(t *testing.T, rt scionrt.Runtime) auxiliaryRuntime {
	t.Helper()
	f.srv.auxiliaryRuntimesMu.RLock()
	defer f.srv.auxiliaryRuntimesMu.RUnlock()
	aux, ok := f.srv.auxiliaryRuntimes[auxiliaryRuntimeIdentity(rt)]
	if !ok {
		t.Fatalf("no auxiliary runtime stored for %q", auxiliaryRuntimeIdentity(rt))
	}
	return aux
}

// assertPinnedResolution checks that a pinned restart for match resolves to
// the match's own manager, runtime and record, classified unrecorded.
func assertPinnedResolution(t *testing.T, srv *Server, opts api.StartOptions, match agentMatch) {
	t.Helper()
	pin := srv.restartPin(match)
	if pin.name == "" || pin.mgr == nil {
		t.Fatalf("restart pin not taken: %+v", pin)
	}
	opts.Profile = ""
	res, err := srv.resolveExistingAgentManager(opts, profileLenient, slog.LevelDebug, pin)
	if err != nil {
		t.Fatalf("resolveExistingAgentManager: %v", err)
	}
	if res.How != resolutionUnrecorded {
		t.Errorf("How = %v, want unrecorded", res.How)
	}
	if res.Manager != match.manager {
		t.Errorf("Manager is not the match's manager")
	}
	if res.Runtime != match.runtime {
		t.Errorf("Runtime = %v, want the match's runtime %v", res.Runtime, match.runtime)
	}
	if res.Record != match.record {
		t.Errorf("Record = %+v, want the match's record %+v", res.Record, match.record)
	}
	if res.RuntimeType != match.runtime.Name() {
		t.Errorf("RuntimeType = %q, want %q", res.RuntimeType, match.runtime.Name())
	}
}

// BR1: after SwapRuntime, defaultPair returns the new runtime's record, and
// the default-holder returns of the resolver use it. A reload swap made
// while the overlay stores runtimes records the overlay's entry, while the
// startup record was resolved against the settings file.
func TestBuildRecord_BR1_SwapRuntimeReplacesDefaultRecord(t *testing.T) {
	t.Run("swap", func(t *testing.T) {
		f := newBRFixture(t)
		if _, rt, rec := f.srv.defaultPair(); rt != scionrt.Runtime(f.defRT) || rec != brDefaultRecord {
			t.Fatalf("before swap: runtime %v record %+v, want the startup runtime and %+v", rt, rec, brDefaultRecord)
		}
		newRT := &scionrt.MockRuntime{NameFunc: func() string { return "docker" }}
		newRec := config.RuntimeBuildRecord{Present: true, EntryName: "swapped"}
		f.srv.SwapRuntime(newRT, newRec)

		mgr, rt, rec := f.srv.defaultPair()
		if rt != scionrt.Runtime(newRT) {
			t.Fatalf("defaultPair runtime = %v, want the swapped runtime", rt)
		}
		if rec != newRec {
			t.Errorf("defaultPair record = %+v, want %+v", rec, newRec)
		}
		res, err := f.srv.resolveManagerForOptsStrict(f.opts("local"), profileLenient, slog.LevelWarn)
		if err != nil {
			t.Fatal(err)
		}
		if res.Manager != mgr || res.Runtime != rt || res.Record != newRec {
			t.Errorf("default-holder resolution = {%v %v %+v}, want the swapped holder with %+v", res.Manager, res.Runtime, res.Record, newRec)
		}
	})

	t.Run("reload record from the overlay, startup record from the file", func(t *testing.T) {
		clearSCIONEnv(t)
		t.Setenv("PATH", "")
		t.Setenv("K_SERVICE", "")
		t.Setenv("CLOUD_RUN_INSTANCE", "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Chdir(t.TempDir())
		prev := config.GetGlobalSettingsOverlay()
		config.SetGlobalSettingsOverlay(nil)
		t.Cleanup(func() { config.SetGlobalSettingsOverlay(prev) })
		if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o755); err != nil {
			t.Fatal(err)
		}
		fileSettings := "schema_version: \"1\"\nactive_profile: main\nruntimes:\n  file-rt:\n    type: docker\nprofiles:\n  main:\n    runtime: file-rt\n"
		if err := os.WriteFile(filepath.Join(home, ".scion", "settings.yaml"), []byte(fileSettings), 0o644); err != nil {
			t.Fatal(err)
		}

		// Startup: resolved before the overlay is installed, with the global
		// project path the server passes (cmd serverRuntimeProjectPath).
		startRT, startRec := scionrt.GetRuntimeWithRecord("global", "")
		if startRec.EntryName != "file-rt" {
			t.Fatalf("startup record EntryName = %q, want file-rt (file view)", startRec.EntryName)
		}
		srv := New(DefaultServerConfig(), agent.NewManager(startRT), startRT, startRec)

		// The co-located overlay is installed with stored runtimes, then the
		// reload resolves and swaps.
		o := config.NewSettingsOverlay()
		o.Update(map[string]config.V1RuntimeConfig{"stored-rt": {Type: "docker"}},
			map[string]config.V1ProfileConfig{"main": {Runtime: "stored-rt"}}, nil)
		config.SetGlobalSettingsOverlay(o)
		newRT, rec := scionrt.GetRuntimeWithRecord("global", "")
		srv.SwapRuntime(newRT, rec)

		_, rt, got := srv.defaultPair()
		if rt != newRT {
			t.Fatalf("defaultPair runtime is not the reloaded runtime")
		}
		if got.EntryName != "stored-rt" || !got.Present {
			t.Errorf("record after reload = %+v, want Present with EntryName stored-rt (overlay view)", got)
		}
	})
}

// BR2: the forced branch returns the record stored with the auxiliary
// holder it returns.
func TestBuildRecord_BR2_ForcedBranchReturnsHolderRecord(t *testing.T) {
	f := newBRFixture(t)
	rtA := fakeKubernetesRuntime("cluster-a", "ns-a")
	rtB := fakeKubernetesRuntime("cluster-b", "ns-b")
	recB := config.RuntimeBuildRecord{Present: true, EntryName: "k8s-b"}
	want := f.registerAux(rtA, agent.NewManager(rtA), brAuxRecord)
	f.registerAux(rtB, agent.NewManager(rtB), recB)
	f.srv.config.ForceRuntime = "kubernetes"

	res, err := f.srv.resolveManagerForOptsStrict(f.opts("local"), profileLenient, slog.LevelWarn)
	if err != nil {
		t.Fatal(err)
	}
	// findAuxiliaryRuntimeByType picks the smallest identity: cluster-a.
	if res.Runtime != want.Runtime || res.Manager != want.Manager {
		t.Fatalf("forced resolution returned %v, want the cluster-a holder", res.Runtime)
	}
	if res.Record != want.Record {
		t.Errorf("Record = %+v, want the holder's %+v", res.Record, want.Record)
	}
	if res.How != resolutionForced {
		t.Errorf("How = %v, want forced", res.How)
	}
}

// BR3: a pinned restart's resolution carries the match's runtime and record
// for matches from (a) a defaultPair snapshot, (b) the own-runtime memo and
// (c) the auxiliary list; (d1) a match without a runtime still pins, with
// the runtime and name from runtimeOfManager and a zero record; (d2) with
// neither, the pin is zero and the pin branch is not taken. The match a
// restart builds in its walk of the other runtimes carries a zero record.
func TestBuildRecord_BR3_PinnedRestartResolution(t *testing.T) {
	t.Run("a: defaultPair snapshot", func(t *testing.T) {
		f := newBRFixture(t)
		f.defMgr.agents = []api.AgentInfo{lifecycleAgent("agent", f.projectDir, "")}
		match, err := f.srv.lookupAgentMatch(context.Background(), "agent", "")
		if err != nil {
			t.Fatal(err)
		}
		if match.runtime != scionrt.Runtime(f.defRT) || match.record != brDefaultRecord {
			t.Fatalf("match = {%v %+v}, want the default runtime with %+v", match.runtime, match.record, brDefaultRecord)
		}
		assertPinnedResolution(t, f.srv, f.opts(""), match)
	})

	t.Run("b: own-runtime memo", func(t *testing.T) {
		f := newOwnRTFixture(t, "agents", true)
		ownRec := config.RuntimeBuildRecord{Present: true, EntryName: "agents-rt", KubeconfigContext: "agents-ctx"}
		inner := runtimeOnly(f.srv.resolveAuxiliaryRuntime)
		f.srv.resolveAuxiliaryRuntime = func(projectPath, agentName, profile string) (scionrt.Runtime, config.RuntimeBuildRecord) {
			return inner(projectPath, agentName, profile), ownRec
		}
		ctx := f.srv.ensureAgentOwnRuntime(context.Background(), ownRTAgent, ownRTProjectID, "")
		own := agentOwnRuntimeFrom(ctx)
		if own == nil {
			t.Fatal("own runtime not resolved")
		}
		match, _ := f.srv.lookupAgentMatch(ctx, ownRTAgent, ownRTProjectID)
		if !match.matched {
			t.Fatal("agent not matched in its own runtime")
		}
		if match.runtime != own.rt || match.record != ownRec {
			t.Fatalf("match = {%v %+v}, want the own runtime with %+v", match.runtime, match.record, ownRec)
		}
		assertPinnedResolution(t, f.srv, api.StartOptions{Name: ownRTAgent, ProjectPath: f.projectDir}, match)
	})

	t.Run("c: auxiliary list", func(t *testing.T) {
		f := newBRFixture(t)
		auxRT := fakeKubernetesRuntime("cluster-a", "ns-a")
		auxMgr := &filteringMockManager{}
		auxMgr.agents = []api.AgentInfo{lifecycleAgent("agent", f.projectDir, "")}
		f.registerAux(auxRT, auxMgr, brAuxRecord)
		match, err := f.srv.lookupAgentMatch(context.Background(), "agent", "")
		if err != nil {
			t.Fatal(err)
		}
		if match.runtime != scionrt.Runtime(auxRT) || match.manager != agent.Manager(auxMgr) || match.record != brAuxRecord {
			t.Fatalf("match = {%v %+v}, want the auxiliary holder with %+v", match.runtime, match.record, brAuxRecord)
		}
		assertPinnedResolution(t, f.srv, f.opts(""), match)
	})

	t.Run("d1: no match runtime, runtimeOfManager fallback with a zero record", func(t *testing.T) {
		f := newBRFixture(t)
		fallbackRT := &scionrt.MockRuntime{NameFunc: func() string { return "podman" }}
		mgr := agent.NewManager(fallbackRT)
		match := agentMatch{containerID: "ctr-agent", manager: mgr, record: brAuxRecord, matched: true}
		pin := f.srv.restartPin(match)
		if pin.mgr != mgr || pin.rt != scionrt.Runtime(fallbackRT) || pin.name != "podman" {
			t.Fatalf("pin = %+v, want the fallback runtime and its name", pin)
		}
		if pin.record != (config.RuntimeBuildRecord{}) {
			t.Fatalf("pin record = %+v, want zero (never paired through runtimeOfManager)", pin.record)
		}
		res, err := f.srv.resolveExistingAgentManager(f.opts(""), profileLenient, slog.LevelDebug, pin)
		if err != nil {
			t.Fatal(err)
		}
		if res.How != resolutionUnrecorded || res.Runtime != scionrt.Runtime(fallbackRT) || res.RuntimeType != "podman" || res.Record != (config.RuntimeBuildRecord{}) {
			t.Errorf("resolution = {How %v Runtime %v Type %q Record %+v}, want the pin with a zero record", res.How, res.Runtime, res.RuntimeType, res.Record)
		}
	})

	t.Run("d2: neither runtime: zero pin, pin branch not taken", func(t *testing.T) {
		f := newBRFixture(t)
		match := agentMatch{containerID: "ctr-agent", manager: &mockManager{}, record: brAuxRecord, matched: true}
		pin := f.srv.restartPin(match)
		if pin != (pinnedRuntime{}) {
			t.Fatalf("pin = %+v, want the zero pin", pin)
		}
		res, err := f.srv.resolveExistingAgentManager(f.opts(""), profileLenient, slog.LevelDebug, pin)
		if err != nil {
			t.Fatal(err)
		}
		if res.How == resolutionUnrecorded {
			t.Errorf("How = unrecorded: the pin branch was taken with a zero pin")
		}
		if res.Manager != agent.Manager(f.defMgr) || res.Record != brDefaultRecord {
			t.Errorf("resolution = {%v %+v}, want the default holder from resolution", res.Manager, res.Record)
		}
	})

	t.Run("other-runtime walk: zero record", func(t *testing.T) {
		f := newBRFixture(t)
		walkRT := fakeKubernetesRuntime("cluster-a", "ns-a")
		walkMgr := agent.NewManager(walkRT)
		f.registerAux(walkRT, walkMgr, brAuxRecord)
		c := agentCandidate{mgr: walkMgr, entry: lifecycleAgent("agent", f.projectDir, "")}
		match, err := f.srv.otherRuntimeMatch("agent", c)
		if err != nil {
			t.Fatal(err)
		}
		if match.runtime != scionrt.Runtime(walkRT) {
			t.Fatalf("match runtime = %v, want the candidate's runtime", match.runtime)
		}
		if match.record != (config.RuntimeBuildRecord{}) {
			t.Fatalf("match record = %+v, want zero (paired through runtimeOfManager)", match.record)
		}
		assertPinnedResolution(t, f.srv, f.opts(""), match)
	})
}

// BR4 (discovery half): discovery stores each auxiliary holder with the
// record its resolver returned, together with that build's runtime and a
// manager over it.
func TestBuildRecord_BR4_DiscoveryStoresRecord(t *testing.T) {
	f := newBRFixture(t)
	// Discovery resolves every profile in the effective settings: the
	// project's own, plus "remote" (runtime "kubernetes"), which the
	// embedded default settings always add. "local" and "alias" resolve to
	// the default's identity, and "broken" and "remote" to error runtimes,
	// so they are all skipped and only "k8s-a" is registered.
	f.builds["local"] = brBuild{rt: &scionrt.MockRuntime{NameFunc: func() string { return "docker" }}, rec: brDefaultRecord}
	f.builds["remote"] = brBuild{rt: &scionrt.ErrorRuntime{Err: errors.New("no cluster in this test")},
		rec: config.RuntimeBuildRecord{Present: true, EntryName: "kubernetes"}}
	auxRT := f.builds["k8s-a"].rt

	f.srv.discoverAuxiliaryRuntimesForProjects([]string{f.projectDir})

	if keys := auxKeys(f.srv); len(keys) != 1 || keys[0] != auxiliaryRuntimeIdentity(auxRT) {
		t.Fatalf("registered auxiliary runtimes = %v, want only %q", keys, auxiliaryRuntimeIdentity(auxRT))
	}
	stored := f.storedAux(t, auxRT)
	if stored.Record != brAuxRecord {
		t.Errorf("stored record = %+v, want %+v", stored.Record, brAuxRecord)
	}
	if stored.Runtime != auxRT {
		t.Errorf("stored runtime = %v, want the discovered build %v", stored.Runtime, auxRT)
	}
	am, ok := stored.Manager.(*agent.AgentManager)
	if !ok || am.Runtime != auxRT {
		t.Errorf("stored manager = %#v, want a manager over the discovered build", stored.Manager)
	}
}

// BR5: a holder built without a record has the zero value.
func TestBuildRecord_BR5_HolderWithoutRecordIsZero(t *testing.T) {
	f := newBRFixture(t)
	zero := config.RuntimeBuildRecord{}

	srv := New(DefaultServerConfig(), f.defMgr, f.defRT, zero)
	if _, _, rec := srv.defaultPair(); rec != zero {
		t.Errorf("default holder record = %+v, want zero", rec)
	}

	auxRT := fakeKubernetesRuntime("cluster-a", "ns-a")
	f.srv.auxiliaryRuntimesMu.Lock()
	f.srv.auxiliaryRuntimes[auxiliaryRuntimeIdentity(auxRT)] = auxiliaryRuntime{Runtime: auxRT, Manager: agent.NewManager(auxRT)}
	f.srv.auxiliaryRuntimesMu.Unlock()
	f.srv.config.ForceRuntime = "kubernetes"
	res, err := f.srv.resolveManagerForOptsStrict(f.opts("local"), profileLenient, slog.LevelWarn)
	if err != nil {
		t.Fatal(err)
	}
	if res.Runtime != scionrt.Runtime(auxRT) || res.Record != zero {
		t.Errorf("forced holder registered without a record: {%v %+v}, want zero record", res.Runtime, res.Record)
	}

	f2 := newBRFixture(t)
	f2.srv.resolveAuxiliaryRuntime = withoutRecord(func(_, _, _ string) scionrt.Runtime { return fakeKubernetesRuntime("cluster-a", "ns-a") })
	res, err = f2.srv.resolveManagerForOptsStrict(f2.opts("k8s-a"), profileLenient, slog.LevelWarn)
	if err != nil {
		t.Fatal(err)
	}
	if res.Record != zero || f2.storedAux(t, res.Runtime).Record != zero {
		t.Errorf("runtime built without a record: returned %+v, stored %+v, want zero", res.Record, f2.storedAux(t, res.Runtime).Record)
	}
}

// BR6: a full-resolution cache overwrite returns, and stores, the new
// build's record.
func TestBuildRecord_BR6_FullResolutionOverwriteReturnsNewRecord(t *testing.T) {
	f := newBRFixture(t)
	first, err := f.srv.resolveManagerForOptsStrict(f.opts("k8s-a"), profileLenient, slog.LevelWarn)
	if err != nil {
		t.Fatal(err)
	}
	if first.Record != brAuxRecord {
		t.Fatalf("first build record = %+v, want %+v", first.Record, brAuxRecord)
	}

	rebuilt := fakeKubernetesRuntime("cluster-a", "ns-a") // same identity, new build
	rebuiltRec := config.RuntimeBuildRecord{Present: true, EntryName: "k8s-a", KubeconfigContext: "cluster-a-rebuilt"}
	f.builds["k8s-a"] = brBuild{rt: rebuilt, rec: rebuiltRec}
	second, err := f.srv.resolveManagerForOptsStrict(f.opts("k8s-a"), profileLenient, slog.LevelWarn)
	if err != nil {
		t.Fatal(err)
	}
	if second.Runtime != scionrt.Runtime(rebuilt) || second.Record != rebuiltRec {
		t.Errorf("second resolution = {%v %+v}, want the rebuilt runtime with %+v", second.Runtime, second.Record, rebuiltRec)
	}
	stored := f.storedAux(t, rebuilt)
	if stored.Runtime != scionrt.Runtime(rebuilt) || stored.Manager != second.Manager || stored.Record != rebuiltRec {
		t.Errorf("stored holder = {%v %+v}, want the rebuilt holder with %+v", stored.Runtime, stored.Record, rebuiltRec)
	}
}

// BR7: an own-runtime memo entry filled before a SwapRuntime still returns
// the record of the runtime it holds.
func TestBuildRecord_BR7_OwnRuntimeMemoKeepsItsRecordAcrossSwap(t *testing.T) {
	f := newOwnRTFixture(t, "agents", true)
	ownRec := config.RuntimeBuildRecord{Present: true, EntryName: "agents-rt", KubeconfigContext: "agents-ctx"}
	inner := runtimeOnly(f.srv.resolveAuxiliaryRuntime)
	f.srv.resolveAuxiliaryRuntime = func(projectPath, agentName, profile string) (scionrt.Runtime, config.RuntimeBuildRecord) {
		return inner(projectPath, agentName, profile), ownRec
	}
	own := agentOwnRuntimeFrom(f.srv.ensureAgentOwnRuntime(context.Background(), ownRTAgent, ownRTProjectID, ""))
	if own == nil {
		t.Fatal("own runtime not resolved")
	}
	if own.record != ownRec {
		t.Fatalf("memo record = %+v, want %+v", own.record, ownRec)
	}

	swapRec := config.RuntimeBuildRecord{Present: true, EntryName: "swapped"}
	f.srv.SwapRuntime(&scionrt.MockRuntime{NameFunc: func() string { return "docker" }}, swapRec)

	again := agentOwnRuntimeFrom(f.srv.ensureAgentOwnRuntime(context.Background(), ownRTAgent, ownRTProjectID, ""))
	if again != own {
		t.Fatalf("memo entry replaced after the swap: %p, want %p", again, own)
	}
	if again.rt != own.rt || again.record != ownRec {
		t.Errorf("memo after swap = {%v %+v}, want its own runtime with %+v", again.rt, again.record, ownRec)
	}
	if _, _, rec := f.srv.defaultPair(); rec != swapRec {
		t.Errorf("default record = %+v, want %+v", rec, swapRec)
	}
}

// CT1: How, and the holder, for every branch the resolvers return.
func TestBuildRecord_CT1_ResolutionKinds(t *testing.T) {
	zero := config.RuntimeBuildRecord{}
	type want struct {
		how            resolutionKind
		defaultHolder  bool // manager, runtime and record are the default holder's
		record         config.RuntimeBuildRecord
		profile, entry string
		err            error
	}
	cases := []struct {
		name    string
		setup   func(t *testing.T, f *brFixture)
		profile string
		mode    profileResolution
		want    want
	}{
		{
			name:    "forced equals the default",
			setup:   func(t *testing.T, f *brFixture) { f.srv.config.ForceRuntime = "docker" },
			profile: "k8s-a",
			want:    want{how: resolutionForced, defaultHolder: true, record: brDefaultRecord},
		},
		{
			name: "forced matches nothing: falls through to resolution",
			setup: func(t *testing.T, f *brFixture) {
				f.srv.config.ForceRuntime = "no-such-runtime"
			},
			profile: "local",
			want:    want{how: resolutionProfile, defaultHolder: true, record: brDefaultRecord, profile: "local", entry: "docker"},
		},
		{
			name: "no project settings, lenient",
			setup: func(t *testing.T, f *brFixture) {
				f.srv.loadSettings = func(string) (*config.VersionedSettings, []string, error) { return nil, nil, nil }
			},
			profile: "local",
			want:    want{how: resolutionDefaultFallback, defaultHolder: true, record: brDefaultRecord},
		},
		{
			name:    "default profile unresolved",
			setup:   func(t *testing.T, f *brFixture) { installStoredProfilesOverlay(t) },
			profile: "",
			want:    want{how: resolutionDefaultFallback, defaultHolder: true, record: brDefaultRecord, err: errDefaultProfileUnresolved},
		},
		{
			name:    "requested profile not stored",
			setup:   func(t *testing.T, f *brFixture) { installStoredProfilesOverlay(t) },
			profile: "absent",
			want:    want{how: resolutionDefaultFallback, defaultHolder: true, record: brDefaultRecord, err: errProfileUnresolved},
		},
		{
			name:    "lenient fallback for a profile not in settings",
			profile: "absent",
			want:    want{how: resolutionDefaultFallback, defaultHolder: true, record: brDefaultRecord},
		},
		{
			name:    "pre-check match with a requested profile",
			profile: "local",
			want:    want{how: resolutionProfile, defaultHolder: true, record: brDefaultRecord, profile: "local", entry: "docker"},
		},
		{
			// Unset until the existing-agent signal is chosen: a create and
			// an existing agent with no recorded profile look the same here.
			name:    "pre-check match with an empty profile (active profile)",
			profile: "",
			want:    want{how: resolutionUnset, defaultHolder: true, record: brDefaultRecord, profile: "local", entry: "docker"},
		},
		{
			name:    "identity equal to the default after full resolution",
			profile: "alias",
			want:    want{how: resolutionProfile, defaultHolder: true, record: brDefaultRecord, profile: "alias", entry: "alias-rt"},
		},
		{
			name:    "full resolution to an auxiliary runtime",
			profile: "k8s-a",
			want:    want{how: resolutionProfile, record: brAuxRecord, profile: "k8s-a", entry: "k8s-a"},
		},
		{
			name:    "error runtime",
			profile: "broken",
			want:    want{how: resolutionErrorRuntime, record: zero, profile: "broken", entry: "broken-rt"},
		},
		{
			name: "strict recorded-runtime fallback",
			setup: func(t *testing.T, f *brFixture) {
				writeSavedAgentInfo(t, f.projectDir, "agent", "vanished", "docker")
			},
			profile: "vanished",
			mode:    profileStrict,
			want:    want{how: resolutionStrictFallback, defaultHolder: true, record: brDefaultRecord},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newBRFixture(t)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			res, err := f.srv.resolveManagerForOptsStrict(f.opts(tc.profile), tc.mode, slog.LevelWarn)
			if tc.want.err != nil {
				if !errors.Is(err, tc.want.err) {
					t.Fatalf("err = %v, want %v", err, tc.want.err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.How != tc.want.how {
				t.Errorf("How = %v, want %v", res.How, tc.want.how)
			}
			if res.Record != tc.want.record {
				t.Errorf("Record = %+v, want %+v", res.Record, tc.want.record)
			}
			if res.Profile != tc.want.profile || res.DispatchEntry != tc.want.entry {
				t.Errorf("Profile, DispatchEntry = %q, %q; want %q, %q", res.Profile, res.DispatchEntry, tc.want.profile, tc.want.entry)
			}
			isDefault := res.Manager == agent.Manager(f.defMgr) && res.Runtime == scionrt.Runtime(f.defRT)
			if isDefault != tc.want.defaultHolder {
				t.Errorf("default holder = %v, want %v (manager %v, runtime %v)", isDefault, tc.want.defaultHolder, res.Manager, res.Runtime)
			}
			if res.Runtime != nil && res.RuntimeType != res.Runtime.Name() {
				t.Errorf("RuntimeType = %q, want %q", res.RuntimeType, res.Runtime.Name())
			}
			if tc.want.how == resolutionErrorRuntime && len(auxKeys(f.srv)) != 0 {
				t.Errorf("error runtime stored in a holder: %v", auxKeys(f.srv))
			}
		})
	}

	t.Run("strict without fallback returns no holder", func(t *testing.T) {
		f := newBRFixture(t)
		res, err := f.srv.resolveManagerForOptsStrict(f.opts("vanished"), profileStrictProvisioned, slog.LevelWarn)
		if !errors.Is(err, errSavedProfileUnresolved) {
			t.Fatalf("err = %v, want errSavedProfileUnresolved", err)
		}
		if res != (runtimeResolution{}) {
			t.Errorf("resolution = %+v, want zero", res)
		}
	})

	t.Run("flat", func(t *testing.T) {
		ff := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
		res, err := ff.srv.resolveManagerForOptsStrict(api.StartOptions{Name: "test-agent-1"}, profileStrict, slog.LevelWarn)
		if err != nil {
			t.Fatal(err)
		}
		mgr, rt, _ := ff.srv.defaultPair()
		if res.How != resolutionFlat || res.Manager != mgr || res.Runtime != rt || res.Record != zero {
			t.Errorf("flat resolution = {How %v Record %+v}, want flat on the instance holder with a zero record", res.How, res.Record)
		}
	})

	t.Run("pin is unrecorded; forced still wins over the pin", func(t *testing.T) {
		f := newBRFixture(t)
		pinRT := fakeKubernetesRuntime("cluster-a", "ns-a")
		pin := pinnedRuntimeOf(agent.NewManager(pinRT), pinRT, brAuxRecord)
		res, err := f.srv.resolveExistingAgentManager(f.opts(""), profileLenient, slog.LevelDebug, pin)
		if err != nil {
			t.Fatal(err)
		}
		if res.How != resolutionUnrecorded || res.Record != brAuxRecord || res.Runtime != scionrt.Runtime(pinRT) {
			t.Errorf("pin resolution = {How %v Record %+v}, want unrecorded with the pin's record", res.How, res.Record)
		}

		f.srv.config.ForceRuntime = "docker"
		res, err = f.srv.resolveExistingAgentManager(f.opts(""), profileLenient, slog.LevelDebug, pin)
		if err != nil {
			t.Fatal(err)
		}
		if res.How != resolutionForced || res.Record != brDefaultRecord {
			t.Errorf("forced over a pin = {How %v Record %+v}, want forced on the default holder", res.How, res.Record)
		}
	})
}
