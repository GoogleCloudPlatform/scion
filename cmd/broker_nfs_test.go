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

package cmd

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func nfsSettings(autoMount bool, shares ...config.V1NFSShare) *config.VersionedSettings {
	return &config.VersionedSettings{
		Server: &config.V1ServerConfig{
			WorkspaceStorage: &config.V1WorkspaceStorageConfig{
				Backend: "nfs",
				NFS: &config.V1NFSConfig{
					MountRoot: "/mnt/nfs",
					AutoMount: autoMount,
					Shares:    shares,
				},
			},
		},
	}
}

// withDefaultRuntime sets the active profile's runtime, as the global
// settings name it (key) and its type ("" leaves the type to the key).
func withDefaultRuntime(vs *config.VersionedSettings, key, rtType string) *config.VersionedSettings {
	vs.ActiveProfile = "default"
	vs.Profiles = map[string]config.V1ProfileConfig{"default": {Runtime: key}}
	vs.Runtimes = map[string]config.V1RuntimeConfig{key: {Type: rtType}}
	return vs
}

var share1 = config.V1NFSShare{ID: "ws1", Server: "10.0.0.2", Export: "/scion-workspaces"}

// pvShare is share1 with a Kubernetes PV name.
var pvShare = config.V1NFSShare{ID: "ws1", Server: "10.0.0.2", Export: "/scion-workspaces", PVName: "scion-ws1"}

func TestBrokerNFSConfig(t *testing.T) {
	t.Run("nil settings", func(t *testing.T) {
		if cfg, warn := brokerNFSConfig(nil); cfg != nil || warn != "" {
			t.Fatalf("got %v, %q; want nil, empty", cfg, warn)
		}
	})
	t.Run("no workspace storage", func(t *testing.T) {
		vs := &config.VersionedSettings{Server: &config.V1ServerConfig{}}
		if cfg, warn := brokerNFSConfig(vs); cfg != nil || warn != "" {
			t.Fatalf("got %v, %q; want nil, empty", cfg, warn)
		}
	})
	t.Run("local backend ignores nfs block", func(t *testing.T) {
		vs := nfsSettings(true, share1)
		vs.Server.WorkspaceStorage.Backend = "local"
		if cfg, warn := brokerNFSConfig(vs); cfg != nil || warn != "" {
			t.Fatalf("got %v, %q; want nil, empty", cfg, warn)
		}
	})
	t.Run("nfs without shares warns", func(t *testing.T) {
		cfg, warn := brokerNFSConfig(nfsSettings(false))
		if cfg != nil || !strings.Contains(warn, "no NFS shares") {
			t.Fatalf("got %v, %q; want nil and a no-shares warning", cfg, warn)
		}
	})
	t.Run("nfs with shares", func(t *testing.T) {
		for _, auto := range []bool{false, true} {
			vs := nfsSettings(auto, share1)
			cfg, warn := brokerNFSConfig(vs)
			if cfg == nil || warn != "" {
				t.Fatalf("auto=%v: got %v, %q; want config", auto, cfg, warn)
			}
			if cfg.AutoMount != auto {
				t.Errorf("AutoMount = %v, want %v", cfg.AutoMount, auto)
			}
			if cfg.MountOptions == "" || cfg.UID == 0 {
				t.Errorf("defaults not applied: %+v", cfg)
			}
			if len(cfg.Shares) != 1 || cfg.Shares[0] != share1 {
				t.Errorf("Shares = %+v", cfg.Shares)
			}
			// The input is not modified.
			in := vs.Server.WorkspaceStorage.NFS
			if in.MountOptions != "" || in.UID != 0 {
				t.Errorf("input settings were modified: %+v", in)
			}
			if cfg == in || &cfg.Shares[0] == &in.Shares[0] {
				t.Error("returned config aliases the input")
			}
		}
	})
}

func TestBrokerNFSConfig_RejectsInvalidShares(t *testing.T) {
	cases := map[string]func(vs *config.VersionedSettings){
		"empty mount_root":    func(vs *config.VersionedSettings) { vs.Server.WorkspaceStorage.NFS.MountRoot = "" },
		"relative mount_root": func(vs *config.VersionedSettings) { vs.Server.WorkspaceStorage.NFS.MountRoot = "mnt/nfs" },
		"empty id":            func(vs *config.VersionedSettings) { vs.Server.WorkspaceStorage.NFS.Shares[0].ID = "" },
		"dotdot id":           func(vs *config.VersionedSettings) { vs.Server.WorkspaceStorage.NFS.Shares[0].ID = ".." },
		"nested id":           func(vs *config.VersionedSettings) { vs.Server.WorkspaceStorage.NFS.Shares[0].ID = "a/b" },
		"empty server":        func(vs *config.VersionedSettings) { vs.Server.WorkspaceStorage.NFS.Shares[0].Server = "" },
		"relative export":     func(vs *config.VersionedSettings) { vs.Server.WorkspaceStorage.NFS.Shares[0].Export = "export" },
		"export with space":   func(vs *config.VersionedSettings) { vs.Server.WorkspaceStorage.NFS.Shares[0].Export = "/a b" },
		"server with space":   func(vs *config.VersionedSettings) { vs.Server.WorkspaceStorage.NFS.Shares[0].Server = "10.0.0.2 -o x" },
		"server with tab":     func(vs *config.VersionedSettings) { vs.Server.WorkspaceStorage.NFS.Shares[0].Server = "host\tname" },
		"server leading dash": func(vs *config.VersionedSettings) { vs.Server.WorkspaceStorage.NFS.Shares[0].Server = "-oexec" },
		"ipv6 server":         func(vs *config.VersionedSettings) { vs.Server.WorkspaceStorage.NFS.Shares[0].Server = "fd00::2" },
		"bracketed ipv6":      func(vs *config.VersionedSettings) { vs.Server.WorkspaceStorage.NFS.Shares[0].Server = "[fd00::2]" },
		"brackets":            func(vs *config.VersionedSettings) { vs.Server.WorkspaceStorage.NFS.Shares[0].Server = "[nfs-host]" },
		"duplicate id": func(vs *config.VersionedSettings) {
			vs.Server.WorkspaceStorage.NFS.Shares = append(vs.Server.WorkspaceStorage.NFS.Shares, share1)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			vs := nfsSettings(true, share1)
			mutate(vs)
			cfg, warn := brokerNFSConfig(vs)
			if cfg != nil || !strings.HasPrefix(warn, "NFS mount checks disabled:") {
				t.Fatalf("got %v, %q; want nil and a warning", cfg, warn)
			}
		})
	}
}

func TestBrokerNFSConfig_AcceptsServerForms(t *testing.T) {
	for _, server := range []string{"10.0.0.2", "nfs.example.internal", "filestore-1"} {
		share := share1
		share.Server = server
		if cfg, warn := brokerNFSConfig(nfsSettings(false, share)); cfg == nil {
			t.Errorf("server %q rejected: %s", server, warn)
		}
	}
}

// TestServerForeground_WiresBrokerNFSConfig guards the broker wiring: the
// runtimebroker.ServerConfig literal in runServerForeground must set
// NFSConfig from brokerNFSConfig, fed from the global settings.
func TestServerForeground_WiresBrokerNFSConfig(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server_foreground.go", nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var nfsIdent string
	literals := 0
	assignedFrom := map[string]string{} // ident -> called function
	brokerNFSArg := ""
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CompositeLit:
			sel, ok := n.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "ServerConfig" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "runtimebroker" {
				return true
			}
			literals++
			for _, elt := range n.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "NFSConfig" {
					if v, ok := kv.Value.(*ast.Ident); ok {
						nfsIdent = v.Name
					}
				}
			}
		case *ast.AssignStmt:
			if len(n.Rhs) != 1 {
				return true
			}
			call, ok := n.Rhs[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			fn := ""
			switch f := call.Fun.(type) {
			case *ast.Ident:
				fn = f.Name
			case *ast.SelectorExpr:
				fn = f.Sel.Name
			}
			if len(n.Lhs) > 0 {
				if id, ok := n.Lhs[0].(*ast.Ident); ok {
					assignedFrom[id.Name] = fn
				}
			}
		case *ast.CallExpr:
			if fn, ok := n.Fun.(*ast.Ident); ok && fn.Name == "brokerNFSConfig" && len(n.Args) == 1 {
				if arg, ok := n.Args[0].(*ast.Ident); ok {
					brokerNFSArg = arg.Name
				}
			}
		}
		return true
	})

	if literals != 1 {
		t.Fatalf("found %d runtimebroker.ServerConfig literals, want 1", literals)
	}
	if nfsIdent == "" {
		t.Fatal("runtimebroker.ServerConfig literal does not set NFSConfig from a variable")
	}
	if got := assignedFrom[nfsIdent]; got != "brokerNFSConfig" {
		t.Fatalf("NFSConfig is set from %q, which is assigned from %q; want brokerNFSConfig", nfsIdent, got)
	}
	if brokerNFSArg == "" {
		t.Fatal("brokerNFSConfig is not called with a settings variable")
	}
	if got := assignedFrom[brokerNFSArg]; got != "LoadGlobalSettings" {
		t.Errorf("brokerNFSConfig(%s): %s is assigned from %q; want config.LoadGlobalSettings (global settings only)",
			brokerNFSArg, brokerNFSArg, got)
	}
}

type fakeNFSProbe struct {
	mounts  map[string]string
	mntErr  error
	dialErr map[string]error
	dialed  []string
}

func (f *fakeNFSProbe) probe() nfsDoctorProbe {
	return nfsDoctorProbe{
		mountSource: func(path string) (string, bool, error) {
			if f.mntErr != nil {
				return "", false, f.mntErr
			}
			src, ok := f.mounts[path]
			return src, ok, nil
		},
		dial: func(addr string) error {
			f.dialed = append(f.dialed, addr)
			return f.dialErr[addr]
		},
	}
}

func TestCheckDoctorNFSMounts(t *testing.T) {
	good := map[string]string{"/mnt/nfs/ws1": "10.0.0.2:/scion-workspaces"}
	cases := []struct {
		name        string
		vs          *config.VersionedSettings
		probe       fakeNFSProbe
		wantStatus  string
		wantMessage []string
		wantRemedy  string
	}{
		{name: "nil settings", vs: nil, wantStatus: "skip", wantMessage: []string{"not configured", "backend: local"}},
		{name: "local backend", vs: func() *config.VersionedSettings {
			vs := nfsSettings(false, share1)
			vs.Server.WorkspaceStorage.Backend = "local"
			return vs
		}(), wantStatus: "skip", wantMessage: []string{"not configured"}},
		{name: "nfs without shares", vs: nfsSettings(false), wantStatus: "fail", wantMessage: []string{"no NFS shares"}, wantRemedy: "at least one share"},
		{name: "healthy, auto_mount off", vs: nfsSettings(false, share1), probe: fakeNFSProbe{mounts: good},
			wantStatus: "pass", wantMessage: []string{"1 share(s) mounted and reachable: ws1", "auto_mount off"}},
		{name: "healthy, auto_mount on", vs: nfsSettings(true, share1), probe: fakeNFSProbe{mounts: good},
			wantStatus: "pass", wantMessage: []string{"auto_mount on"}},
		{name: "not mounted, auto_mount off: warn", vs: nfsSettings(false, share1),
			wantStatus: "warn", wantMessage: []string{"ws1: not mounted at /mnt/nfs/ws1", "auto_mount is off", "(auto_mount off)"}, wantRemedy: "Mount each export"},
		{name: "not mounted, auto_mount on: fail", vs: nfsSettings(true, share1),
			wantStatus: "fail", wantMessage: []string{"not mounted", "auto_mount on"}, wantRemedy: "broker.nfs-mount"},
		{name: "not mounted, auto_mount on, kubernetes default runtime: warn", vs: withDefaultRuntime(nfsSettings(true, share1), "gke", "kubernetes"),
			wantStatus: "warn", wantMessage: []string{"not mounted at /mnt/nfs/ws1", "default runtime is kubernetes", "verify only"}, wantRemedy: "mounts the export into each agent"},
		{name: "not mounted, auto_mount on, cloudrun default runtime: warn", vs: withDefaultRuntime(nfsSettings(true, share1), "cloudrun", ""),
			wantStatus: "warn", wantMessage: []string{"default runtime is cloudrun"}},
		{name: "not mounted, auto_mount on, docker default runtime: fail", vs: withDefaultRuntime(nfsSettings(true, share1), "docker", ""),
			wantStatus: "fail", wantMessage: []string{"not mounted", "(auto_mount on)"}, wantRemedy: "broker.nfs-mount"},
		{name: "not mounted, auto_mount on, unresolvable profile: fail", vs: func() *config.VersionedSettings {
			vs := withDefaultRuntime(nfsSettings(true, share1), "gke", "kubernetes")
			vs.ActiveProfile = "missing"
			return vs
		}(), wantStatus: "fail", wantMessage: []string{"not mounted"}},
		{name: "wrong source, auto_mount on, kubernetes default runtime: fail", vs: withDefaultRuntime(nfsSettings(true, share1), "gke", "kubernetes"),
			probe: fakeNFSProbe{mounts: map[string]string{"/mnt/nfs/ws1": "10.9.9.9:/x"}}, wantStatus: "fail", wantMessage: []string{"mounted from 10.9.9.9:/x"}},
		{name: "not mounted, pv_name share, auto_mount on: warn", vs: nfsSettings(true, pvShare),
			wantStatus: "warn", wantMessage: []string{"not mounted", "pv_name is set"}},
		{name: "not mounted with auto_mount off, server unreachable: fail", vs: nfsSettings(false, share1),
			probe:      fakeNFSProbe{dialErr: map[string]error{"10.0.0.2:2049": errors.New("refused")}},
			wantStatus: "fail", wantMessage: []string{"not mounted", "unreachable"}},
		{name: "wrong source, auto_mount on: fail", vs: nfsSettings(true, share1), probe: fakeNFSProbe{mounts: map[string]string{"/mnt/nfs/ws1": "10.9.9.9:/x"}},
			wantStatus: "fail", wantMessage: []string{"mounted from 10.9.9.9:/x"}},
		{name: "wrong source, pv_name share: fail", vs: nfsSettings(false, pvShare), probe: fakeNFSProbe{mounts: map[string]string{"/mnt/nfs/ws1": "10.9.9.9:/x"}},
			wantStatus: "fail", wantMessage: []string{"mounted from 10.9.9.9:/x"}},
		{name: "wrong source", vs: nfsSettings(false, share1), probe: fakeNFSProbe{mounts: map[string]string{"/mnt/nfs/ws1": "10.9.9.9:/x"}},
			wantStatus: "fail", wantMessage: []string{"mounted from 10.9.9.9:/x, expected 10.0.0.2:/scion-workspaces"}},
		{name: "server unreachable", vs: nfsSettings(false, share1),
			probe:      fakeNFSProbe{mounts: good, dialErr: map[string]error{"10.0.0.2:2049": errors.New("refused")}},
			wantStatus: "fail", wantMessage: []string{"server 10.0.0.2 unreachable on port 2049"}},
		{name: "mount table unreadable", vs: nfsSettings(false, share1), probe: fakeNFSProbe{mntErr: errors.New("no /proc")},
			wantStatus: "fail", wantMessage: []string{"mount state unknown (no /proc)"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.probe
			res := checkDoctorNFSMounts(tc.vs, p.probe())
			if res.Name != "nfs-mounts" || res.Status != tc.wantStatus {
				t.Fatalf("got %s/%s %q; want status %s", res.Name, res.Status, res.Message, tc.wantStatus)
			}
			for _, want := range tc.wantMessage {
				if !strings.Contains(res.Message, want) {
					t.Errorf("message %q missing %q", res.Message, want)
				}
			}
			if tc.wantRemedy != "" && !strings.Contains(res.Remediation, tc.wantRemedy) {
				t.Errorf("remediation %q missing %q", res.Remediation, tc.wantRemedy)
			}
		})
	}
}

// TestDefaultNFSDoctorProbe_ReadsMountTable verifies that doctor's default
// probe uses the shared mount-table lookup: "/" is always mounted.
func TestDefaultNFSDoctorProbe_ReadsMountTable(t *testing.T) {
	if _, err := os.Stat("/proc/mounts"); err != nil {
		t.Skip("no /proc/mounts")
	}
	_, mounted, err := defaultNFSDoctorProbe().mountSource("/")
	if err != nil || !mounted {
		t.Fatalf("mountSource(/) = %v, %v; want mounted", mounted, err)
	}
}

func TestCheckDoctorNFSMounts_DialsEachShareServer(t *testing.T) {
	share2 := config.V1NFSShare{ID: "ws2", Server: "nfs.example.internal", Export: "/b"}
	p := fakeNFSProbe{mounts: map[string]string{
		"/mnt/nfs/ws1": "10.0.0.2:/scion-workspaces",
		"/mnt/nfs/ws2": "nfs.example.internal:/b",
	}}
	res := checkDoctorNFSMounts(nfsSettings(false, share1, share2), p.probe())
	if res.Status != "pass" {
		t.Fatalf("status = %s %q", res.Status, res.Message)
	}
	if strings.Join(p.dialed, ",") != "10.0.0.2:2049,nfs.example.internal:2049" {
		t.Errorf("dialed %v", p.dialed)
	}
}
