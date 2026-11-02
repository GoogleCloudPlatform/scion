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

package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// controlPathFixture points $HOME at a temporary directory holding a Scion
// global directory with control files, and returns the home, the global
// directory and an unrelated directory.
func controlPathFixture(t *testing.T) (home, global, outside string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home = filepath.Join(root, "home")
	global = filepath.Join(home, ".scion")
	outside = filepath.Join(root, "data")
	for _, d := range []string{outside, filepath.Join(global, "projects", "p", "workspace", "sub"), filepath.Join(global, "templates")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"broker-credentials.json", "settings.yaml"} {
		if err := os.WriteFile(filepath.Join(global, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home, global, outside
}

func controlPathRunConfig(vols ...api.VolumeMount) RunConfig {
	return RunConfig{
		Harness:      &harness.Generic{},
		Name:         "a1",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		Volumes:      vols,
	}
}

func TestBuildCommonRunArgs_RefusesVolumesReachingControlPaths(t *testing.T) {
	home, global, outside := controlPathFixture(t)
	link := filepath.Join(outside, "g")
	if err := os.Symlink(global, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for name, src := range map[string]string{
		"global dir":        global,
		"home":              home,
		"home via tilde":    "~",
		"credentials file":  filepath.Join(global, "broker-credentials.json"),
		"settings file":     filepath.Join(global, "settings.yaml"),
		"templates (floor)": filepath.Join(global, "templates"),
		"bare project dir":  filepath.Join(global, "projects", "p"),
		"symlink to global": link,
		"relative path":     "./data",
	} {
		for _, ro := range []bool{false, true} {
			t.Run(name, func(t *testing.T) {
				_, err := buildCommonRunArgs(controlPathRunConfig(api.VolumeMount{Source: src, Target: "/mnt/x", ReadOnly: ro}))
				if !errors.Is(err, brokercontrol.ErrControlPathMount) {
					t.Fatalf("read-only=%v: err = %v, want ErrControlPathMount", ro, err)
				}
			})
		}
	}
}

func TestBuildCommonRunArgs_AllowsVolumesClearOfControlPaths(t *testing.T) {
	_, global, outside := controlPathFixture(t)
	ws := filepath.Join(global, "projects", "p", "workspace", "sub")
	args, err := buildCommonRunArgs(controlPathRunConfig(
		api.VolumeMount{Source: outside, Target: "/mnt/data"},
		api.VolumeMount{Source: ws, Target: "/mnt/ws", ReadOnly: true},
		api.VolumeMount{Source: "cachevol", Target: "/mnt/cache"},
	))
	if err != nil {
		t.Fatalf("buildCommonRunArgs: %v", err)
	}
	joined := strings.Join(args, "\n")
	for _, want := range []string{outside + ":/mnt/data", ws + ":/mnt/ws:ro", "cachevol:/mnt/cache"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args lack bind %q:\n%s", want, joined)
		}
	}
	var label string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--label" && strings.HasPrefix(args[i+1], LabelBindSources+"=") {
			label = strings.TrimPrefix(args[i+1], LabelBindSources+"=")
		}
	}
	binds, err := decodeBindSourcesLabel(map[string]string{LabelBindSources: label})
	if err != nil {
		t.Fatalf("bind sources label: %v", err)
	}
	got := map[string]string{}
	for _, b := range binds {
		got[b.Destination] = b.Source
	}
	if got["/mnt/data"] != outside || got["/mnt/ws"] != ws {
		t.Errorf("label binds = %v", binds)
	}
	if _, ok := got["/mnt/cache"]; ok {
		t.Errorf("named volume recorded as a host bind: %v", binds)
	}
}

func TestBuildCommonRunArgs_BindUsesResolvedSource(t *testing.T) {
	_, _, outside := controlPathFixture(t)
	real := filepath.Join(outside, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(outside, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	args, err := buildCommonRunArgs(controlPathRunConfig(api.VolumeMount{Source: link, Target: "/mnt/x"}))
	if err != nil {
		t.Fatalf("buildCommonRunArgs: %v", err)
	}
	if !strings.Contains(strings.Join(args, "\n"), real+":/mnt/x") {
		t.Fatalf("bind does not use the resolved source %q: %v", real, args)
	}
}

func TestBuildCommonRunArgs_RefusesDevBinariesInGlobalDir(t *testing.T) {
	_, global, _ := controlPathFixture(t)
	t.Setenv("SCION_DEV_BINARIES", filepath.Join(global, "templates"))
	_, err := buildCommonRunArgs(controlPathRunConfig())
	if !errors.Is(err, brokercontrol.ErrControlPathMount) {
		t.Fatalf("err = %v, want ErrControlPathMount", err)
	}
}

func TestApplyResolvedAuth_RefusesControlPathSources(t *testing.T) {
	home, global, outside := controlPathFixture(t)
	agentHome := filepath.Join(outside, "agent-home")
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}
	okFile := filepath.Join(outside, "adc.json")
	if err := os.WriteFile(okFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	noop := func(string, string) {}
	noVol := func(api.VolumeMount) {}
	for _, homeDir := range []string{agentHome, ""} { // copy mode, bind mode
		for _, src := range []string{filepath.Join(global, "broker-credentials.json"), filepath.Join(global, "templates"), home} {
			var bound []string
			reg := func(s, _ string, _, _ bool) { bound = append(bound, s) }
			cfg := RunConfig{UnixUsername: "scion", HomeDir: homeDir, ResolvedAuth: &api.ResolvedAuth{
				Files: []api.FileMapping{{SourcePath: src, ContainerPath: "~/.config/x.json"}},
			}}
			err := applyResolvedAuth(cfg, noop, noVol, reg)
			if !errors.Is(err, brokercontrol.ErrControlPathMount) {
				t.Fatalf("home=%q src=%q: err = %v, want ErrControlPathMount", homeDir, src, err)
			}
			if len(bound) != 0 {
				t.Fatalf("a refused auth file was bound: %v", bound)
			}
		}
		cfg := RunConfig{UnixUsername: "scion", HomeDir: homeDir, ResolvedAuth: &api.ResolvedAuth{
			Files: []api.FileMapping{{SourcePath: okFile, ContainerPath: "~/.config/x.json"}},
		}}
		if err := applyResolvedAuth(cfg, noop, noVol, func(string, string, bool, bool) {}); err != nil {
			t.Fatalf("home=%q: an auth file outside the global directory: %v", homeDir, err)
		}
	}
}

func TestValidateWorkspaceSource_RefusesRegisteredControlPath(t *testing.T) {
	_, _, outside := controlPathFixture(t)
	state := filepath.Join(outside, "state")
	if err := os.MkdirAll(filepath.Join(state, "b1"), 0o755); err != nil {
		t.Fatal(err)
	}
	brokercontrol.SetExtraPaths("test", []brokercontrol.Entry{{Path: filepath.Join(state, "b1"), Kind: brokercontrol.Dir, Class: brokercontrol.ClassHostState}})
	t.Cleanup(func() { brokercontrol.SetExtraPaths("test", nil) })
	if _, err := ValidateWorkspaceSource(state, ""); !errors.Is(err, brokercontrol.ErrControlPathMount) {
		t.Fatalf("workspace containing a registered control path: err = %v", err)
	}
	if _, err := ValidateAgentHomeSource(filepath.Join(state, "b1", "home"), ""); !errors.Is(err, brokercontrol.ErrControlPathMount) {
		t.Fatalf("home inside a registered control path: err = %v", err)
	}
	if _, err := ValidateWorkspaceSource(filepath.Join(outside, "ws"), ""); err != nil {
		t.Fatalf("unrelated workspace: %v", err)
	}
}

func TestAgentHomeRepairArgs_RefusesGlobalDir(t *testing.T) {
	_, global, _ := controlPathFixture(t)
	_, err := agentHomeRepairArgs(agentHomeRepairHelper{req: AgentHomeOwnershipRepair{HomeDir: global, Image: "img"}})
	if !errors.Is(err, brokercontrol.ErrControlPathMount) {
		t.Fatalf("err = %v, want ErrControlPathMount", err)
	}
}

func TestCheckSandboxMountSources(t *testing.T) {
	_, global, outside := controlPathFixture(t)
	ok := scionPaths{agentHome: filepath.Join(outside, "agents", "a", "home"), workspace: filepath.Join(outside, "agents", "a", "workspace")}
	if err := checkSandboxMountSources(ok, []api.SharedDir{{Name: "s"}}); err != nil {
		t.Fatalf("sandbox paths clear of control files: %v", err)
	}
	bad := ok
	bad.workspace = global
	if err := checkSandboxMountSources(bad, nil); !errors.Is(err, brokercontrol.ErrControlPathMount) {
		t.Fatalf("err = %v, want ErrControlPathMount", err)
	}
}

func TestParseInspect_OnlyBindsAndLabels(t *testing.T) {
	out := `{"Mounts":[{"Type":"bind","Source":"/a","Destination":"/x"},{"Type":"volume","Source":"/var/lib/docker/volumes/v/_data","Destination":"/v"},{"Type":"tmpfs","Destination":"/t"}],"Labels":{"scion.name":"a"}}`
	ib, err := parseInspect([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if !ib.ReportedKnown || len(ib.Reported) != 1 || ib.Reported[0].Source != "/a" || ib.Reported[0].Destination != "/x" {
		t.Fatalf("inspected = %+v", ib)
	}
	if ib.Labels["scion.name"] != "a" {
		t.Fatalf("labels = %v", ib.Labels)
	}
	if _, err := parseInspect([]byte("not json")); !errors.Is(err, ErrMountsUnverifiable) {
		t.Fatalf("malformed inspect output: err = %v", err)
	}
}

func recordLabels(t *testing.T, binds ...BindMount) map[string]string {
	t.Helper()
	v, err := encodeBindSourcesLabel(binds)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{LabelBindSources: v}
}

func TestVerifyContainerBinds(t *testing.T) {
	_, global, outside := controlPathFixture(t)
	ws := filepath.Join(outside, "ws")
	gone := filepath.Join(outside, "not-on-this-host")
	rec := recordLabels(t, BindMount{Source: outside, Destination: "/x"}, BindMount{Source: ws, Destination: "/workspace"})
	cases := []struct {
		name string
		ib   InspectedBinds
		want error // nil, ErrControlPathMount or ErrMountsUnverifiable
	}{
		{"reported matches record", InspectedBinds{ReportedKnown: true, Labels: rec,
			Reported: []BindMount{{Source: ws, Destination: "/workspace"}, {Source: outside, Destination: "/x/"}}}, nil},
		{"docker desktop host prefix", InspectedBinds{ReportedKnown: true, Labels: rec,
			Reported: []BindMount{{Source: "/host_mnt" + outside, Destination: "/x"}, {Source: ws, Destination: "/workspace"}}}, nil},
		{"no record", InspectedBinds{ReportedKnown: true, Reported: []BindMount{{Source: outside, Destination: "/x"}}}, ErrMountsUnverifiable},
		{"no record, runtime cannot report", InspectedBinds{}, ErrMountsUnverifiable},
		{"source differs", InspectedBinds{ReportedKnown: true, Labels: rec,
			Reported: []BindMount{{Source: gone, Destination: "/x"}, {Source: ws, Destination: "/workspace"}}}, brokercontrol.ErrControlPathMount},
		{"bind not requested", InspectedBinds{ReportedKnown: true, Labels: rec,
			Reported: []BindMount{{Source: outside, Destination: "/x"}, {Source: ws, Destination: "/workspace"}, {Source: ws, Destination: "/extra"}}}, brokercontrol.ErrControlPathMount},
		{"requested bind missing", InspectedBinds{ReportedKnown: true, Labels: rec,
			Reported: []BindMount{{Source: outside, Destination: "/x"}}}, brokercontrol.ErrControlPathMount},
		{"reported source reaches control path", InspectedBinds{ReportedKnown: true, Labels: rec,
			Reported: []BindMount{{Source: global, Destination: "/x"}, {Source: ws, Destination: "/workspace"}}}, brokercontrol.ErrControlPathMount},
		{"recorded source reaches control path (label only)", InspectedBinds{Labels: recordLabels(t, BindMount{Source: global, Destination: "/g"})}, brokercontrol.ErrControlPathMount},
		{"label only, clean", InspectedBinds{Labels: rec}, nil},
		{"empty record, binds reported", InspectedBinds{ReportedKnown: true, Labels: recordLabels(t),
			Reported: []BindMount{{Source: outside, Destination: "/x"}}}, brokercontrol.ErrControlPathMount},
		{"null record, binds reported", InspectedBinds{ReportedKnown: true, Labels: map[string]string{LabelBindSources: "bnVsbA=="},
			Reported: []BindMount{{Source: outside, Destination: "/x"}}}, brokercontrol.ErrControlPathMount},
		{"malformed record", InspectedBinds{ReportedKnown: true, Labels: map[string]string{LabelBindSources: "%%%"},
			Reported: []BindMount{{Source: outside, Destination: "/x"}}}, ErrMountsUnverifiable},
		{"empty-string record", InspectedBinds{ReportedKnown: true, Labels: map[string]string{LabelBindSources: ""},
			Reported: []BindMount{{Source: outside, Destination: "/x"}}}, ErrMountsUnverifiable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyContainerBinds(tc.ib)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestBuildCommonRunArgs_BindRecordLabelIsTheBrokers(t *testing.T) {
	_, _, outside := controlPathFixture(t)
	cfg := controlPathRunConfig(api.VolumeMount{Source: outside, Target: "/mnt/data"})
	forged, err := encodeBindSourcesLabel(nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Labels = map[string]string{LabelBindSources: forged, "other": "x"}
	cfg.Annotations = map[string]string{LabelBindSources: forged}
	args, err := buildCommonRunArgs(cfg)
	if err != nil {
		t.Fatalf("buildCommonRunArgs: %v", err)
	}
	var values []string
	last := -1
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "--label" {
			continue
		}
		last = i
		if v, ok := strings.CutPrefix(args[i+1], LabelBindSources+"="); ok {
			values = append(values, v)
		}
	}
	if len(values) != 1 || values[0] == forged {
		t.Fatalf("bind record labels = %v, want exactly the broker's", values)
	}
	if !strings.HasPrefix(args[last+1], LabelBindSources+"=") {
		t.Fatalf("the bind record is not the last label: %v", args[last+1])
	}
}

func TestCheckWorkspaceVolumeSources(t *testing.T) {
	_, global, outside := controlPathFixture(t)
	for _, src := range []string{"~", global, filepath.Join(global, "settings.yaml")} {
		if err := CheckWorkspaceVolumeSources([]api.VolumeMount{{Source: src, Target: "/workspace"}}); !errors.Is(err, brokercontrol.ErrControlPathMount) {
			t.Errorf("workspace %q: err = %v", src, err)
		}
	}
	for _, v := range []api.VolumeMount{
		{Source: filepath.Join(global, "projects", "p"), Target: "/workspace"},
		{Source: outside, Target: "/workspace"},
		{Source: global, Target: "/mnt/not-a-workspace"}, // left to CheckVolumeSources
	} {
		if err := CheckWorkspaceVolumeSources([]api.VolumeMount{v}); err != nil {
			t.Errorf("%+v: %v", v, err)
		}
	}
}

func TestAgentHomeRepairArgs_AcceptsResolvedSymlinkedHome(t *testing.T) {
	_, _, outside := controlPathFixture(t)
	real := filepath.Join(outside, "real-home2")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(outside, "home-link2")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// repairAgentHomeOwnership resolves the home (ValidateAgentHomeSource)
	// before it builds the argv, so a home reached through a symlink is
	// repaired at its resolved path.
	resolved, err := ValidateAgentHomeSource(link, "")
	if err != nil {
		t.Fatalf("ValidateAgentHomeSource: %v", err)
	}
	args, err := agentHomeRepairArgs(agentHomeRepairHelper{req: AgentHomeOwnershipRepair{HomeDir: resolved, Image: "img"}, name: "scion-home-repair-0123456789abcdef", homeKey: "fedcba9876543210"})
	if err != nil {
		t.Fatalf("agentHomeRepairArgs with the resolved home: %v", err)
	}
	if !strings.Contains(strings.Join(args, "\n"), real+":"+agentHomeRepairMount) {
		t.Fatalf("argv does not bind the resolved home %q: %v", real, args)
	}
}

func TestAgentHomeRepairArgs_RequiresResolvedHome(t *testing.T) {
	_, _, outside := controlPathFixture(t)
	real := filepath.Join(outside, "real-home")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(outside, "home-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := agentHomeRepairArgs(agentHomeRepairHelper{req: AgentHomeOwnershipRepair{HomeDir: link, Image: "img"}}); err == nil {
		t.Fatal("a home path that is not resolved was accepted")
	}
}

func TestCheckBindMounts(t *testing.T) {
	_, global, outside := controlPathFixture(t)
	if err := CheckBindMounts([]BindMount{{Source: outside, Destination: "/x"}}); err != nil {
		t.Fatalf("clean binds: %v", err)
	}
	err := CheckBindMounts([]BindMount{{Source: outside, Destination: "/x"}, {Source: global, Destination: "/g"}})
	if !errors.Is(err, brokercontrol.ErrControlPathMount) {
		t.Fatalf("err = %v, want ErrControlPathMount", err)
	}
}

func TestDecodeBindSourcesLabel_MissingIsUnverifiable(t *testing.T) {
	if _, err := decodeBindSourcesLabel(nil); !errors.Is(err, ErrMountsUnverifiable) {
		t.Fatalf("missing label: err = %v", err)
	}
	if _, err := decodeBindSourcesLabel(map[string]string{LabelBindSources: "%%%"}); !errors.Is(err, ErrMountsUnverifiable) {
		t.Fatalf("malformed label: err = %v", err)
	}
}

func TestCheckVolumeSources(t *testing.T) {
	_, global, outside := controlPathFixture(t)
	for _, src := range []string{"~", "~/.scion", "${HOME}/.scion/settings.yaml", "$HOME"} {
		err := CheckVolumeSources([]api.VolumeMount{{Source: src, Target: "/x"}})
		if !errors.Is(err, brokercontrol.ErrControlPathMount) {
			t.Errorf("source %q: err = %v, want ErrControlPathMount", src, err)
		}
	}
	ok := []api.VolumeMount{
		{Source: outside, Target: "/a"},
		{Source: "cachevol", Target: "/b"},
		{Type: "gcs", Bucket: "b", Target: "/c"},
		{Type: "gke-shared-volume", VolumeName: "v", Target: "/d"},
		// The workspace is judged where it is decided, not here.
		{Source: filepath.Join(global, "projects", "p"), Target: "/workspace"},
	}
	if err := CheckVolumeSources(ok); err != nil {
		t.Fatalf("clean volumes: %v", err)
	}
	for _, v := range []api.VolumeMount{
		{Source: filepath.Join(global, "projects", "p"), Target: "/mnt/p"},
	} {
		if err := CheckVolumeSources([]api.VolumeMount{v}); err == nil {
			t.Errorf("volume %+v: want refusal", v)
		}
	}
}

func TestVerifyRunningContainerBinds_Unrecorded(t *testing.T) {
	_, global, outside := controlPathFixture(t)
	local := func() (bool, error) { return true, nil }
	remote := func() (bool, error) { return false, nil }
	unknown := func() (bool, error) { return false, errors.New("context inspect failed") }
	clean := []BindMount{{Source: outside, Destination: "/x"}}
	cases := []struct {
		name string
		ib   InspectedBinds
		want error // nil = kept
	}{
		{"local daemon, sources exist and are clean: kept", InspectedBinds{ReportedKnown: true, Reported: clean, DaemonLocal: local}, nil},
		{"docker desktop prefix on an existing source: kept", InspectedBinds{ReportedKnown: true, DaemonLocal: local,
			Reported: []BindMount{{Source: "/host_mnt" + outside, Destination: "/x"}}}, nil},
		{"source does not exist on this host", InspectedBinds{ReportedKnown: true, DaemonLocal: local,
			Reported: []BindMount{{Source: filepath.Join(outside, "absent"), Destination: "/x"}}}, ErrMountsUnverifiable},
		{"relative source", InspectedBinds{ReportedKnown: true, DaemonLocal: local,
			Reported: []BindMount{{Source: "rel/path", Destination: "/x"}}}, ErrMountsUnverifiable},
		{"source reaches a control path", InspectedBinds{ReportedKnown: true, DaemonLocal: local,
			Reported: []BindMount{{Source: global, Destination: "/g"}}}, brokercontrol.ErrControlPathMount},
		{"remote daemon (tcp, ssh or a remote docker context)", InspectedBinds{ReportedKnown: true, Reported: clean, DaemonLocal: remote}, ErrMountsUnverifiable},
		{"daemon locality cannot be determined", InspectedBinds{ReportedKnown: true, Reported: clean, DaemonLocal: unknown}, ErrMountsUnverifiable},
		{"no locality check available", InspectedBinds{ReportedKnown: true, Reported: clean}, ErrMountsUnverifiable},
		{"runtime cannot report binds (Apple)", InspectedBinds{DaemonLocal: local}, ErrMountsUnverifiable},
		{"malformed record is not legacy", InspectedBinds{ReportedKnown: true, Reported: clean, DaemonLocal: local,
			Labels: map[string]string{LabelBindSources: "%%%"}}, ErrMountsUnverifiable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyRunningContainerBinds(tc.ib)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("err = %v, want kept", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestVerifyContainerBinds_StrictWithoutRecord(t *testing.T) {
	// Right after creation (VerifyContainerBinds) a container without a
	// record is never accepted, even with a local daemon and clean sources.
	_, _, outside := controlPathFixture(t)
	ib := InspectedBinds{ReportedKnown: true, Reported: []BindMount{{Source: outside, Destination: "/x"}},
		DaemonLocal: func() (bool, error) { return true, nil }}
	if err := VerifyContainerBinds(ib); !errors.Is(err, ErrMountsUnverifiable) {
		t.Fatalf("err = %v, want ErrMountsUnverifiable", err)
	}
}

func TestVerifyRunningContainerBinds_RecordedIsUnchanged(t *testing.T) {
	_, _, outside := controlPathFixture(t)
	rec := recordLabels(t, BindMount{Source: outside, Destination: "/x"})
	// A recorded container is judged by the record comparison, whatever
	// the daemon's locality.
	ib := InspectedBinds{ReportedKnown: true, Labels: rec, Reported: []BindMount{{Source: outside, Destination: "/other"}},
		DaemonLocal: func() (bool, error) { return true, nil }}
	if err := VerifyRunningContainerBinds(ib); !errors.Is(err, brokercontrol.ErrControlPathMount) {
		t.Fatalf("err = %v, want a mismatch refusal", err)
	}
}

func TestVerifyRunningContainerBinds_HostMntPrefixedPathIsCheckedToo(t *testing.T) {
	_, global, outside := controlPathFixture(t)
	// A real directory standing in for /host_mnt on this host.
	prefix := filepath.Join(outside, "host_mnt")
	if err := os.MkdirAll(prefix, 0o755); err != nil {
		t.Fatal(err)
	}
	prev := dockerDesktopHostPrefix
	dockerDesktopHostPrefix = prefix
	t.Cleanup(func() { dockerDesktopHostPrefix = prev })
	clear := filepath.Join(outside, "clear")
	if err := os.MkdirAll(clear, 0o755); err != nil {
		t.Fatal(err)
	}
	// <prefix><clear> is a symlink to the global directory: the stripped
	// path is clear, the prefixed one is not.
	if err := os.MkdirAll(filepath.Dir(prefix+clear), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(global, prefix+clear); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	local := func() (bool, error) { return true, nil }
	ib := InspectedBinds{ReportedKnown: true, DaemonLocal: local, Reported: []BindMount{{Source: prefix + clear, Destination: "/x"}}}
	if err := VerifyRunningContainerBinds(ib); !errors.Is(err, brokercontrol.ErrControlPathMount) {
		t.Fatalf("err = %v, want the prefixed path refused", err)
	}
}

func TestVerifyRunningContainerBinds_ControlPathRefusedBeforeExistence(t *testing.T) {
	_, global, _ := controlPathFixture(t)
	// A source inside a control directory that does not exist is a
	// refusal (stop now), not merely unverifiable.
	ib := InspectedBinds{ReportedKnown: true, DaemonLocal: func() (bool, error) { return true, nil },
		Reported: []BindMount{{Source: filepath.Join(global, "hub-credentials", "absent"), Destination: "/x"}}}
	if err := VerifyRunningContainerBinds(ib); !errors.Is(err, brokercontrol.ErrControlPathMount) {
		t.Fatalf("err = %v, want ErrControlPathMount", err)
	}
}
