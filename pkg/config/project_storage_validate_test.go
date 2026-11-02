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

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// validNFSBlock is a valid nfs server.project_storage block for both
// origins once mount_root and auto_mount are cleared for OriginManaged.
func validNFSBlock() *V1ProjectStorageConfig {
	ps := &V1ProjectStorageConfig{
		Backend: WorkspaceStorageBackendNFS,
		NFS: &V1NFSConfig{
			MountRoot: "/mnt/scion-nfs",
			Shares:    []V1NFSShare{{ID: "share-1", Server: "10.0.0.2", Export: "/vol1", PVName: "scion-pv"}},
		},
	}
	ps.ApplyDefaults()
	return ps
}

func managedBlock(ps *V1ProjectStorageConfig) *V1ProjectStorageConfig {
	ps.NFS.MountRoot = ""
	ps.NFS.AutoMount = false
	return ps
}

func TestValidateProjectStorage_ValidBlocks(t *testing.T) {
	if errs := ValidateProjectStorage(nil, OriginFile); errs != nil {
		t.Errorf("nil block: %v", errs)
	}
	if errs := ValidateProjectStorage(&V1ProjectStorageConfig{Backend: "local"}, OriginFile); errs != nil {
		t.Errorf("local block: %v", errs)
	}
	if errs := ValidateProjectStorage(validNFSBlock(), OriginFile); errs != nil {
		t.Errorf("valid nfs block (file): %v", errs)
	}
	if errs := ValidateProjectStorage(managedBlock(validNFSBlock()), OriginManaged); errs != nil {
		t.Errorf("valid nfs block (managed): %v", errs)
	}
	// An nfs block under a non-nfs backend is not used and not checked.
	unused := &V1ProjectStorageConfig{Backend: "local", NFS: &V1NFSConfig{Shares: []V1NFSShare{{ID: "../x"}}}}
	if errs := ValidateProjectStorage(unused, OriginFile); errs != nil {
		t.Errorf("unselected nfs block: %v", errs)
	}
}

// TestValidateProjectStorage_FieldRules is the per-field table for both
// origins: each case mutates a valid block and names the field it expects
// to be reported, or "" when the value is valid for that origin.
func TestValidateProjectStorage_FieldRules(t *testing.T) {
	const (
		idPath     = "server.project_storage.nfs.shares[0].id"
		serverPath = "server.project_storage.nfs.shares[0].server"
		exportPath = "server.project_storage.nfs.shares[0].export"
		pvPath     = "server.project_storage.nfs.shares[0].pv_name"
		optsPath   = "server.project_storage.nfs.mount_options"
		rootPath   = "server.project_storage.nfs.mount_root"
		autoPath   = "server.project_storage.nfs.auto_mount"
		scPath     = "server.project_storage.nfs.storage_class"
		uidPath    = "server.project_storage.nfs.uid"
	)
	share := func(f func(s *V1NFSShare)) func(*V1ProjectStorageConfig) {
		return func(ps *V1ProjectStorageConfig) { f(&ps.NFS.Shares[0]) }
	}
	cases := []struct {
		name        string
		mutate      func(*V1ProjectStorageConfig)
		fileWant    string // field reported for OriginFile, "" = valid
		managedWant string // field reported for OriginManaged, "" = valid
	}{
		// share id
		{"id dotdot traversal", share(func(s *V1NFSShare) { s.ID = "../x" }), idPath, idPath},
		{"id nested", share(func(s *V1NFSShare) { s.ID = "a/b" }), idPath, idPath},
		{"id dot", share(func(s *V1NFSShare) { s.ID = "." }), idPath, idPath},
		{"id dotdot", share(func(s *V1NFSShare) { s.ID = ".." }), idPath, idPath},
		{"id backslash", share(func(s *V1NFSShare) { s.ID = `a\b` }), idPath, idPath},
		{"id empty", share(func(s *V1NFSShare) { s.ID = "" }), idPath, idPath},
		{"id NUL", share(func(s *V1NFSShare) { s.ID = "a\x00b" }), idPath, idPath},
		{"id space", share(func(s *V1NFSShare) { s.ID = "a b" }), idPath, idPath},
		{"id newline", share(func(s *V1NFSShare) { s.ID = "a\nb" }), idPath, idPath},
		{"id control", share(func(s *V1NFSShare) { s.ID = "a\x07b" }), idPath, idPath},
		{"id 64 bytes", share(func(s *V1NFSShare) { s.ID = strings.Repeat("a", 64) }), idPath, idPath},
		{"id 63 bytes", share(func(s *V1NFSShare) { s.ID = strings.Repeat("a", 63) }), "", ""},
		{"id existing hub name", share(func(s *V1NFSShare) { s.ID = "scion-hub1" }), "", ""},
		{"id upper case", share(func(s *V1NFSShare) { s.ID = "Scion_Hub" }), "", idPath},
		{"id leading dash", share(func(s *V1NFSShare) { s.ID = "-x" }), "", idPath},
		{"id duplicated", func(ps *V1ProjectStorageConfig) {
			ps.NFS.Shares = append(ps.NFS.Shares, V1NFSShare{ID: "share-1", Server: "10.0.0.3", Export: "/vol2"})
		}, "server.project_storage.nfs.shares[1].id", "server.project_storage.nfs.shares[1].id"},
		// server
		{"server leading dash", share(func(s *V1NFSShare) { s.Server = "-oexec" }), serverPath, serverPath},
		{"server empty", share(func(s *V1NFSShare) { s.Server = "" }), serverPath, serverPath},
		{"server space", share(func(s *V1NFSShare) { s.Server = "10.0.0.2 -o x" }), serverPath, serverPath},
		{"server tab", share(func(s *V1NFSShare) { s.Server = "host\tname" }), serverPath, serverPath},
		{"server comma", share(func(s *V1NFSShare) { s.Server = "a,b" }), serverPath, serverPath},
		{"server slash", share(func(s *V1NFSShare) { s.Server = "a/b" }), serverPath, serverPath},
		{"server bracketed ipv6", share(func(s *V1NFSShare) { s.Server = "[fd00::2]" }), serverPath, serverPath},
		{"server underscore", share(func(s *V1NFSShare) { s.Server = "nfs_host" }), serverPath, serverPath},
		{"server label leading dash", share(func(s *V1NFSShare) { s.Server = "nfs.-x" }), serverPath, serverPath},
		{"server ipv4", share(func(s *V1NFSShare) { s.Server = "192.168.1.10" }), "", ""},
		{"server ipv6", share(func(s *V1NFSShare) { s.Server = "fd00::2" }), serverPath, serverPath},
		{"server ipv4-mapped ipv6", share(func(s *V1NFSShare) { s.Server = "::ffff:10.0.0.2" }), serverPath, serverPath},
		{"server hostname", share(func(s *V1NFSShare) { s.Server = "nfs.example.internal" }), "", ""},
		{"server short hostname", share(func(s *V1NFSShare) { s.Server = "filestore-1" }), "", ""},
		// export
		{"export leading dash", share(func(s *V1NFSShare) { s.Export = "-oexec" }), exportPath, exportPath},
		{"export relative", share(func(s *V1NFSShare) { s.Export = "vol1" }), exportPath, exportPath},
		{"export empty", share(func(s *V1NFSShare) { s.Export = "" }), exportPath, exportPath},
		{"export dotdot", share(func(s *V1NFSShare) { s.Export = "/vol/../etc" }), exportPath, exportPath},
		{"export unclean", share(func(s *V1NFSShare) { s.Export = "/vol1/" }), exportPath, exportPath},
		{"export space", share(func(s *V1NFSShare) { s.Export = "/a b" }), exportPath, exportPath},
		{"export comma", share(func(s *V1NFSShare) { s.Export = "/a,b" }), exportPath, exportPath},
		{"export NUL", share(func(s *V1NFSShare) { s.Export = "/a\x00b" }), exportPath, exportPath},
		{"export too long", share(func(s *V1NFSShare) { s.Export = "/" + strings.Repeat("a", 1024) }), exportPath, exportPath},
		{"export nested", share(func(s *V1NFSShare) { s.Export = "/share/scion-workspaces" }), "", ""},
		// pv_name
		{"pv_name upper case", share(func(s *V1NFSShare) { s.PVName = "PV" }), pvPath, pvPath},
		{"pv_name slash", share(func(s *V1NFSShare) { s.PVName = "a/b" }), pvPath, pvPath},
		{"pv_name empty", share(func(s *V1NFSShare) { s.PVName = "" }), "", ""},
		{"pv_name dotted", share(func(s *V1NFSShare) { s.PVName = "scion.pv-1" }), "", ""},
		// storage_class
		{"storage_class invalid", func(ps *V1ProjectStorageConfig) { ps.NFS.StorageClass = "Bad Class" }, scPath, scPath},
		// mount_options
		{"mount option leading dash", func(ps *V1ProjectStorageConfig) { ps.NFS.MountOptions = "-oexec" }, optsPath, optsPath},
		{"mount option empty token", func(ps *V1ProjectStorageConfig) { ps.NFS.MountOptions = "vers=3,,hard" }, optsPath, optsPath},
		{"mount option space", func(ps *V1ProjectStorageConfig) { ps.NFS.MountOptions = "vers=3, hard" }, optsPath, optsPath},
		{"mount option value space", func(ps *V1ProjectStorageConfig) { ps.NFS.MountOptions = "sec=a b" }, optsPath, optsPath},
		{"mount option upper name", func(ps *V1ProjectStorageConfig) { ps.NFS.MountOptions = "VERS=3" }, optsPath, optsPath},
		{"mount option suid", func(ps *V1ProjectStorageConfig) { ps.NFS.MountOptions = "vers=3,suid" }, optsPath, optsPath},
		{"mount option dev", func(ps *V1ProjectStorageConfig) { ps.NFS.MountOptions = "dev,vers=3" }, optsPath, optsPath},
		{"mount option exec", func(ps *V1ProjectStorageConfig) { ps.NFS.MountOptions = "exec" }, "", optsPath},
		{"mount option defaults", func(ps *V1ProjectStorageConfig) { ps.NFS.MountOptions = "defaults,vers=3" }, "", optsPath},
		{"mount option rw valued", func(ps *V1ProjectStorageConfig) { ps.NFS.MountOptions = "rw=x" }, optsPath, optsPath},
		{"mount option suid valued", func(ps *V1ProjectStorageConfig) { ps.NFS.MountOptions = "suid=1" }, optsPath, optsPath},
		{"mount option nosuid valued", func(ps *V1ProjectStorageConfig) { ps.NFS.MountOptions = "nosuid=0" }, optsPath, optsPath},
		{"mount option nosuid nodev rw", func(ps *V1ProjectStorageConfig) { ps.NFS.MountOptions = "vers=3,nosuid,nodev,rw" }, "", ""},
		{"mount option soft", func(ps *V1ProjectStorageConfig) { ps.NFS.MountOptions = "soft" }, "", optsPath},
		{"mount option actimeo too high", func(ps *V1ProjectStorageConfig) { ps.NFS.MountOptions = "actimeo=61" }, "", optsPath},
		{"mount option sec", func(ps *V1ProjectStorageConfig) { ps.NFS.MountOptions = "sec=sys" }, "", optsPath},
		{"mount option port", func(ps *V1ProjectStorageConfig) { ps.NFS.MountOptions = "port=2049" }, "", optsPath},
		{"mount option addr", func(ps *V1ProjectStorageConfig) { ps.NFS.MountOptions = "addr=10.0.0.9" }, "", optsPath},
		{"mount option nconnect too high", func(ps *V1ProjectStorageConfig) { ps.NFS.MountOptions = "nconnect=17" }, "", optsPath},
		{"mount option flag with value", func(ps *V1ProjectStorageConfig) { ps.NFS.MountOptions = "hard=1" }, "", optsPath},
		{"mount option vers without value", func(ps *V1ProjectStorageConfig) { ps.NFS.MountOptions = "vers" }, "", optsPath},
		{"mount options default", func(ps *V1ProjectStorageConfig) { ps.NFS.MountOptions = "vers=3,hard,nconnect=4,_netdev" }, "", ""},
		{"mount options allowlisted", func(ps *V1ProjectStorageConfig) {
			ps.NFS.MountOptions = "nfsvers=4.1,proto=tcp,hard,rsize=1048576,wsize=4096,timeo=600,retrans=0,actimeo=60,noatime,nodiratime,lookupcache=pos,nolock,local_lock=none,nosuid,nodev,rw"
		}, "", ""},
		// owner ids
		{"uid negative", func(ps *V1ProjectStorageConfig) { ps.NFS.UID = -1 }, "server.project_storage", "server.project_storage"},
	}
	for _, tc := range cases {
		for _, origin := range []Origin{OriginFile, OriginManaged} {
			want := tc.fileWant
			ps := validNFSBlock()
			if origin == OriginManaged {
				want = tc.managedWant
				ps = managedBlock(ps)
			}
			tc.mutate(ps)
			errs := ValidateProjectStorage(ps, origin)
			if want == "" {
				if len(errs) != 0 {
					t.Errorf("%s (%s): unexpected errors %v", tc.name, origin, errs)
				}
				continue
			}
			if !hasFieldError(errs, want) {
				t.Errorf("%s (%s): errors %v, want one for %s", tc.name, origin, errs, want)
			}
		}
	}

	// File-only rules: mount_root.
	for name, root := range map[string]string{"empty": "", "relative": "mnt/nfs", "unclean": "/mnt/nfs/", "dotdot": "/mnt/../etc"} {
		ps := validNFSBlock()
		ps.NFS.MountRoot = root
		if errs := ValidateProjectStorage(ps, OriginFile); !hasFieldError(errs, rootPath) {
			t.Errorf("mount_root %s (%q): errors %v, want one for %s", name, root, errs, rootPath)
		}
	}
	// Managed-only rules: mount_root and auto_mount are not deliverable,
	// and uid and gid must not be 0.
	ps := managedBlock(validNFSBlock())
	ps.NFS.MountRoot = "/mnt/scion-nfs"
	if errs := ValidateProjectStorage(ps, OriginManaged); !hasFieldError(errs, rootPath) {
		t.Errorf("managed mount_root: errors %v, want one for %s", errs, rootPath)
	}
	ps = managedBlock(validNFSBlock())
	ps.NFS.AutoMount = true
	if errs := ValidateProjectStorage(ps, OriginManaged); !hasFieldError(errs, autoPath) {
		t.Errorf("managed auto_mount: errors %v, want one for %s", errs, autoPath)
	}
	ps = managedBlock(validNFSBlock())
	ps.NFS.UID = 0
	if errs := ValidateProjectStorage(ps, OriginManaged); !hasFieldError(errs, uidPath) {
		t.Errorf("managed uid 0: errors %v, want one for %s", errs, uidPath)
	}
}

// TestValidateProjectStorage_FileMountOptionsNameTheOption: a file block's
// suid or dev is refused with an error naming the option; exec and
// defaults are accepted.
func TestValidateProjectStorage_FileMountOptionsNameTheOption(t *testing.T) {
	for _, opt := range []string{"suid", "dev"} {
		ps := validNFSBlock()
		ps.NFS.MountOptions = "vers=3," + opt
		errs := ValidateProjectStorage(ps, OriginFile)
		if len(errs) != 1 || !strings.Contains(errs[0].Error(), fmt.Sprintf("%q", opt)) {
			t.Errorf("%s: errors %v, want one naming the option", opt, errs)
		}
	}
	for _, opts := range []string{"defaults,vers=3", "exec", "vers=3,exec,hard"} {
		ps := validNFSBlock()
		ps.NFS.MountOptions = opts
		if errs := ValidateProjectStorage(ps, OriginFile); errs != nil {
			t.Errorf("%q: unexpected errors %v", opts, errs)
		}
	}
}

// TestValidateProjectStorage_NeverAllowsPathSeparators: for every origin,
// a share id that is not one path element is refused.
func TestValidateProjectStorage_NeverAllowsPathSeparators(t *testing.T) {
	for _, origin := range []Origin{OriginFile, OriginManaged} {
		for _, id := range []string{"/", "a/b", `a\b`, ".", "..", "../x", "x/..", "/abs"} {
			ps := validNFSBlock()
			if origin == OriginManaged {
				ps = managedBlock(ps)
			}
			ps.NFS.Shares[0].ID = id
			if errs := ValidateProjectStorage(ps, origin); len(errs) == 0 {
				t.Errorf("%s: share id %q accepted", origin, id)
			}
		}
	}
}

func TestValidateProjectStorage_VolumeBackends(t *testing.T) {
	gke := func(name, claim string) *V1ProjectStorageConfig {
		ps := &V1ProjectStorageConfig{Backend: WorkspaceStorageBackendGKESharedVolume,
			GKESharedVolume: &V1GKESharedVolumeConfig{VolumeName: name, PVClaimName: claim}}
		ps.ApplyDefaults()
		return ps
	}
	if errs := ValidateProjectStorage(gke("scion-shared", "scion-claim"), OriginFile); errs != nil {
		t.Errorf("valid gke block: %v", errs)
	}
	if errs := ValidateProjectStorage(gke("..", ""), OriginFile); !hasFieldError(errs, "server.project_storage.gke_shared_volume.volume_name") {
		t.Errorf("gke volume_name ..: %v", errs)
	}
	if errs := ValidateProjectStorage(gke("a/b", ""), OriginManaged); !hasFieldError(errs, "server.project_storage.gke_shared_volume.volume_name") {
		t.Errorf("gke volume_name a/b: %v", errs)
	}
	if errs := ValidateProjectStorage(gke("v", "Bad_Claim"), OriginFile); !hasFieldError(errs, "server.project_storage.gke_shared_volume.pv_claim_name") {
		t.Errorf("gke pv_claim_name: %v", errs)
	}
	cr := &V1ProjectStorageConfig{Backend: WorkspaceStorageBackendCloudRunVolume,
		CloudRunVolume: &V1CloudRunVolumeConfig{VolumeName: `a\b`}}
	cr.ApplyDefaults()
	if errs := ValidateProjectStorage(cr, OriginFile); !hasFieldError(errs, "server.project_storage.cloudrun_volume.volume_name") {
		t.Errorf("cloudrun volume_name: %v", errs)
	}
}

// TestValidateProjectStorage_StructuralError: a block Validate refuses is
// reported as one error under server.project_storage, with Validate's
// message unchanged.
func TestValidateProjectStorage_StructuralError(t *testing.T) {
	ps := &V1ProjectStorageConfig{Backend: "nfs", NFS: &V1NFSConfig{MountRoot: "/mnt"}}
	ps.ApplyDefaults()
	errs := ValidateProjectStorage(ps, OriginFile)
	if len(errs) != 1 || errs[0].Path != ProjectStorageKey {
		t.Fatalf("errors = %v, want one under %s", errs, ProjectStorageKey)
	}
	if err := ps.Validate(); err == nil || errs[0].Error() != err.Error() {
		t.Errorf("message = %q, want Validate's %v", errs[0].Error(), err)
	}
}

// TestValidateProjectStorage_Pure: the validator does not modify its input.
func TestValidateProjectStorage_Pure(t *testing.T) {
	ps := validNFSBlock()
	ps.NFS.Shares[0].ID = "../x"
	before := *ps.NFS
	beforeShare := ps.NFS.Shares[0]
	_ = ValidateProjectStorage(ps, OriginManaged)
	if ps.NFS.MountRoot != before.MountRoot || ps.NFS.MountOptions != before.MountOptions || ps.NFS.Shares[0] != beforeShare {
		t.Error("ValidateProjectStorage modified its input")
	}
}

// TestResolveProjectStorage_RefusesInvalidFields: every resolution of the
// block applies the field rules and names the field; a block synthesized
// from the deprecated blocks is reported under the key it was read from.
func TestResolveProjectStorage_RefusesInvalidFields(t *testing.T) {
	ps := validNFSBlock()
	ps.NFS.Shares[0].ID = "../x"
	_, _, err := ResolveProjectStorage(&V1ServerConfig{ProjectStorage: ps})
	var fields FieldErrorList
	if !errors.As(err, &fields) || !hasFieldError(fields, "server.project_storage.nfs.shares[0].id") {
		t.Fatalf("project_storage: err = %v, want a FieldErrorList naming shares[0].id", err)
	}
	if !strings.Contains(err.Error(), `server.project_storage.nfs.shares[0].id`) || !strings.Contains(err.Error(), `"../x"`) {
		t.Errorf("message %q does not name the field and value", err)
	}

	ws := &V1WorkspaceStorageConfig{Backend: "nfs", NFS: &V1NFSConfig{
		MountRoot: "/mnt/nfs",
		Shares:    []V1NFSShare{{ID: "ws1", Server: "-oexec", Export: "/vol"}},
	}}
	_, _, err = ResolveProjectStorage(&V1ServerConfig{WorkspaceStorage: ws})
	if !errors.As(err, &fields) || !hasFieldError(fields, "server.workspace_storage.nfs.shares[0].server") {
		t.Fatalf("workspace_storage: err = %v, want a FieldErrorList naming the server", err)
	}

	// The shared-dir-only block needs no server or export, but its id is
	// checked.
	sd := &V1SharedDirStorageConfig{Backend: "nfs", NFS: &V1NFSConfig{MountRoot: "/mnt/sd", Shares: []V1NFSShare{{ID: "sd1"}}}}
	if _, _, err := ResolveProjectStorage(&V1ServerConfig{SharedDirStorage: sd}); err != nil {
		t.Errorf("shared_dir_storage without server: %v", err)
	}
	sd.NFS.Shares[0].ID = "a/b"
	_, _, err = ResolveProjectStorage(&V1ServerConfig{SharedDirStorage: sd})
	if err == nil || !strings.Contains(err.Error(), "shares[0].id") {
		t.Errorf("shared_dir_storage id a/b: err = %v", err)
	}
}

// TestValidateProjectStorage_DeploymentRenders: the blocks the Terraform
// module and the Helm chart render (share id = the hub name) pass
// OriginFile.
func TestValidateProjectStorage_DeploymentRenders(t *testing.T) {
	for _, hubName := range []string{"scion-hub1", "scion", "my-team-hub"} {
		ps := &V1ProjectStorageConfig{Backend: "nfs", NFS: &V1NFSConfig{
			MountRoot:   "/mnt/scion-nfs",
			UID:         1000,
			GID:         1000,
			SubPathRoot: "projects",
			Shares:      []V1NFSShare{{ID: hubName, Server: "10.12.0.2", Export: "/scion", PVName: hubName + "-nfs"}},
		}}
		ps.ApplyDefaults()
		if errs := ValidateProjectStorage(ps, OriginFile); errs != nil {
			t.Errorf("rendered block for %q: %v", hubName, errs)
		}
	}

	// The rendered settings themselves: the Terraform template and the Helm
	// chart's rendered value for the share id is the hub name, and their
	// mount_root is a plain absolute path.
	_, file, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(file), "..", "..")
	tf, err := os.ReadFile(filepath.Join(repo, "deploy", "terraform", "modules", "hub-cloudrun", "templates", "settings.yaml.tftpl"))
	if err != nil {
		t.Fatalf("reading the Terraform settings template: %v", err)
	}
	if !regexp.MustCompile(`id = hub_name`).Match(tf) {
		t.Error("the Terraform template no longer renders id = hub_name; update this test's rendered block")
	}
}

// TestValidateProjectStorage_SupersetOfBrokerRules: every value the
// broker's former share checks refused is still refused, for both
// origins. Tightenings beyond them are covered in
// TestValidateProjectStorage_FieldRules.
func TestValidateProjectStorage_SupersetOfBrokerRules(t *testing.T) {
	formerlyRefused := map[string]func(ps *V1ProjectStorageConfig){
		"empty mount_root":    func(ps *V1ProjectStorageConfig) { ps.NFS.MountRoot = "" },
		"relative mount_root": func(ps *V1ProjectStorageConfig) { ps.NFS.MountRoot = "mnt/nfs" },
		"empty id":            func(ps *V1ProjectStorageConfig) { ps.NFS.Shares[0].ID = "" },
		"dot id":              func(ps *V1ProjectStorageConfig) { ps.NFS.Shares[0].ID = "." },
		"dotdot id":           func(ps *V1ProjectStorageConfig) { ps.NFS.Shares[0].ID = ".." },
		"nested id":           func(ps *V1ProjectStorageConfig) { ps.NFS.Shares[0].ID = "a/b" },
		"backslash id":        func(ps *V1ProjectStorageConfig) { ps.NFS.Shares[0].ID = `a\b` },
		"duplicate id": func(ps *V1ProjectStorageConfig) {
			ps.NFS.Shares = append(ps.NFS.Shares, ps.NFS.Shares[0])
		},
		"empty server":        func(ps *V1ProjectStorageConfig) { ps.NFS.Shares[0].Server = "" },
		"server with space":   func(ps *V1ProjectStorageConfig) { ps.NFS.Shares[0].Server = "10.0.0.2 -o x" },
		"server with tab":     func(ps *V1ProjectStorageConfig) { ps.NFS.Shares[0].Server = "host\tname" },
		"server leading dash": func(ps *V1ProjectStorageConfig) { ps.NFS.Shares[0].Server = "-oexec" },
		"ipv6 server":         func(ps *V1ProjectStorageConfig) { ps.NFS.Shares[0].Server = "fd00::2" },
		"bracketed ipv6":      func(ps *V1ProjectStorageConfig) { ps.NFS.Shares[0].Server = "[fd00::2]" },
		"brackets":            func(ps *V1ProjectStorageConfig) { ps.NFS.Shares[0].Server = "[nfs-host]" },
		"relative export":     func(ps *V1ProjectStorageConfig) { ps.NFS.Shares[0].Export = "export" },
		"export with space":   func(ps *V1ProjectStorageConfig) { ps.NFS.Shares[0].Export = "/a b" },
	}
	for name, mutate := range formerlyRefused {
		for _, origin := range []Origin{OriginFile, OriginManaged} {
			ps := validNFSBlock()
			if origin == OriginManaged {
				ps = managedBlock(ps)
				if strings.Contains(name, "mount_root") {
					continue // mount_root is not deliverable in a managed block
				}
			}
			mutate(ps)
			if errs := ValidateProjectStorage(ps, origin); len(errs) == 0 {
				t.Errorf("%s (%s): accepted", name, origin)
			}
		}
	}
}

func hasFieldError(errs []FieldError, path string) bool {
	for _, e := range errs {
		if e.Path == path {
			return true
		}
	}
	return false
}

// TestMigrateSettingsFile_StorageFieldWarns: migrating a legacy settings
// file whose storage block breaks a field rule still converts the file and
// reports the field as a warning; validation still refuses the result.
func TestMigrateSettingsFile_StorageFieldWarns(t *testing.T) {
	dir := t.TempDir()
	legacy := `
active_profile: local
runtimes:
  docker: {}
profiles:
  local:
    runtime: docker
server:
  workspace_storage:
    backend: nfs
    nfs:
      shares:
        - id: ws1
          server: 10.0.0.2
          export: /scion
`
	path := filepath.Join(dir, "settings.yaml")
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := MigrateSettingsFile(dir, false)
	if err != nil {
		t.Fatalf("MigrateSettingsFile: %v", err)
	}
	warned := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "server.workspace_storage.nfs.mount_root") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("warnings %q do not name server.workspace_storage.nfs.mount_root", result.Warnings)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := DetectSettingsFormat(data); v != "1" || !strings.Contains(string(data), "workspace_storage") {
		t.Fatalf("the file was not converted with its storage block:\n%s", data)
	}
	errs, err := ValidateSettings(data, "1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range errs {
		if e.StorageField && e.Path == "server.workspace_storage.nfs.mount_root" {
			found = true
		}
	}
	if !found {
		t.Errorf("validation errors %v do not report the mount_root field", errs)
	}
}
