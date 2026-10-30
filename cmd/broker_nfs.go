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
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
)

// brokerNFSConfig returns the NFS settings for the runtime broker's
// ServerConfig.NFSConfig: the nfs block of the broker's resolved project
// storage (config.ResolveProjectStorage: server.project_storage, or the
// block synthesized from server.workspace_storage), with defaults applied
// and the field rules of config.ValidateProjectStorage passed. It returns
// nil (NFS handling off, the broker behaves exactly as without NFS) unless
// the backend is "nfs" and the block is valid. The returned value is a
// copy; vs is not modified.
//
// The second return value is a warning for the caller to log, set when the
// backend is "nfs" but the block is unusable. The broker's agent starts are
// then refused as well (brokerStorageLayout).
func brokerNFSConfig(vs *config.VersionedSettings) (*config.V1NFSConfig, string) {
	if vs == nil || vs.Server == nil || vs.Server.EffectiveWorkspaceStorage() == nil {
		return nil, ""
	}
	if vs.Server.EffectiveWorkspaceStorage().Backend != config.WorkspaceStorageBackendNFS {
		return nil, ""
	}
	ps, _, err := config.ResolveProjectStorage(vs.Server)
	if err != nil {
		return nil, "NFS mount checks disabled: " + err.Error()
	}
	if ps.EffectiveBackend() != config.WorkspaceStorageBackendNFS || ps.NFS == nil {
		return nil, ""
	}
	return ps.NFS, ""
}

// brokerWorkspaceStorageWarning returns a startup warning when the
// subpath_root of the selected server.workspace_storage backend is invalid,
// or "" when it is fine. The broker does not refuse to start, and NFS mount
// checks (brokerNFSConfig) do not depend on subpath_root, but the runtime
// workspace backends reject the value, so every agent start on this backend
// would fail; this says so once, at startup, instead of only per agent.
func brokerWorkspaceStorageWarning(vs *config.VersionedSettings) string {
	if vs == nil || vs.Server == nil {
		return ""
	}
	if err := vs.Server.EffectiveWorkspaceStorage().ValidateSelectedSubPathRoot(); err != nil {
		return fmt.Sprintf("server.%v; agent starts that use the %q workspace backend will fail until it is fixed",
			err, vs.Server.EffectiveWorkspaceStorage().Backend)
	}
	return ""
}

// brokerWorkspaceStorageBackend returns the server.workspace_storage backend
// name from the broker's global settings, or "" (local) when unset.
func brokerWorkspaceStorageBackend(vs *config.VersionedSettings) string {
	if vs == nil || vs.Server == nil || vs.Server.EffectiveWorkspaceStorage() == nil {
		return ""
	}
	return vs.Server.EffectiveWorkspaceStorage().Backend
}

// brokerRegistrationWorkspaceStorage returns the workspace storage
// descriptor a broker sends when it registers with a hub, built from the
// global settings the same way the running broker builds the one it sends
// on every heartbeat. Share health is reported false: registration happens
// before the broker has checked its mounts, and the next heartbeat carries
// the real health.
func brokerRegistrationWorkspaceStorage(vs *config.VersionedSettings) *api.BrokerWorkspaceStorage {
	nfs, _ := brokerNFSConfig(vs)
	desc := runtimebroker.BuildWorkspaceStorageDescriptor(brokerWorkspaceStorageBackend(vs), nfs, nil)
	layout, _, storageErr := brokerStorageLayout(vs)
	if storageErr != nil {
		return runtimebroker.InvalidStorageDescriptor(desc)
	}
	desc.Homes = runtimebroker.StorageHomes(layout)
	desc.Volume = runtimebroker.StorageVolume(layout)
	desc.SharedDirs = projectfs.SharedDirsPlacement(layout)
	return desc
}

// brokerStorageLayout builds the broker's projectfs Layout from its global
// settings: the same server.project_storage struct the hub builds its Layout
// from (config.ResolveProjectStorage: the block as written, or synthesized
// from the deprecated blocks, with the field rules of
// config.ValidateProjectStorage), translated by config.ProjectStorageLayout.
// The returned warnings are for the caller to log once at startup.
//
// When the block is unusable, storageErr says why and the Layout is the
// zero Layout: the broker still starts, so its existing agents can be
// stopped and deleted, but it reports its storage as invalid, refuses every
// agent start with storageErr and reports degraded health
// (runtimebroker.ServerConfig.StorageError). It never falls back to the
// local layout, whose placement would differ from the configured one.
func brokerStorageLayout(vs *config.VersionedSettings) (layout projectfs.Layout, warnings []string, storageErr error) {
	local := projectfs.Layout{Backend: projectfs.BackendLocal, Homes: projectfs.BackendLocal}
	if vs == nil {
		return local, nil, nil
	}
	ps, warnings, err := config.ResolveProjectStorage(vs.Server)
	if err != nil {
		return projectfs.Layout{}, nil, fmt.Errorf("invalid project storage settings: %w", err)
	}
	layout, err = config.ProjectStorageLayout(ps, config.DefaultVolumeMountBase)
	if err != nil {
		return projectfs.Layout{}, warnings, fmt.Errorf("invalid project storage settings: %w", err)
	}
	return layout, append(warnings, brokerSharedDirLayoutWarnings(layout, os.Stat)...), nil
}

// brokerStorageUnavailableWarning is the startup warning for a broker whose
// storage settings are unusable.
func brokerStorageUnavailableWarning(err error) string {
	return fmt.Sprintf("%v; this broker refuses agent starts until it is fixed and restarted (stop, delete and logs keep working)", err)
}

// brokerSharedDirLayoutWarnings returns startup warnings for a shared-dir
// Layout that would fail Kubernetes agent starts with shared dirs (every
// project has the default scratchpad): a share without a claim (the legacy
// split's nfs shares[0].pv_name, gke_shared_volume.pv_claim_name) or a gke-shared-volume
// this broker has not mounted. Agent starts still refuse; this says so once,
// at startup, instead of only per start.
func brokerSharedDirLayoutWarnings(layout projectfs.Layout, stat func(string) (os.FileInfo, error)) []string {
	sd := layout.SharedDirLayout()
	var out []string
	switch sd.Backend {
	case projectfs.BackendNFS:
		// Without a pv_name, Kubernetes pods mount the Layout's own share
		// inline (logged once at startup); only the legacy split still
		// needs its claim.
		if sd.Share.PVName == "" && layout.SharedDirs != nil {
			out = append(out, "project storage: the nfs share has no pv_name; Kubernetes agent starts with shared dirs will fail until nfs.shares[0].pv_name is set")
		}
	case projectfs.BackendGKESharedVolume:
		if sd.Share.PVName == "" {
			out = append(out, "project storage: gke_shared_volume.pv_claim_name is not set; Kubernetes agent starts with shared dirs will fail until it is")
		}
		if _, err := stat(sd.HostBase); err != nil {
			out = append(out, fmt.Sprintf("project storage: the shared volume is not mounted on this broker at %s; agent starts with shared dirs will fail until it is", sd.HostBase))
		}
	}
	return out
}

// loadBrokerRegistrationWorkspaceStorage loads the global settings and
// returns brokerRegistrationWorkspaceStorage for them, or nil (descriptor
// not reported; the next heartbeat reports it) when they cannot be loaded.
func loadBrokerRegistrationWorkspaceStorage() *api.BrokerWorkspaceStorage {
	vs, _, err := config.LoadGlobalSettings()
	if err != nil {
		return nil
	}
	return brokerRegistrationWorkspaceStorage(vs)
}

// nfsDoctorProbe is the read-only system access the doctor NFS check needs.
// Tests replace it with fakes.
type nfsDoctorProbe struct {
	// mountSource returns the source device (server:export for NFS) mounted
	// at path, and whether path is a mountpoint at all.
	mountSource func(path string) (source string, mounted bool, err error)
	// dial checks TCP reachability of addr (host:port).
	dial func(addr string) error
}

// nfsServerPort is the NFS server port the doctor reachability check dials.
const nfsServerPort = "2049"

func defaultNFSDoctorProbe() nfsDoctorProbe {
	return nfsDoctorProbe{
		mountSource: runtimebroker.ProcMountSource,
		dial: func(addr string) error {
			conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
			if err != nil {
				return err
			}
			return conn.Close()
		},
	}
}

// checkDoctorNFSMounts performs D5: NFS mount status, checked locally from
// this host's global settings (server.workspace_storage) and mount table.
// It reports whether NFS workspace storage is configured, and for each
// share whether it is mounted at <mount_root>/<share id> from the expected
// server:export and whether the server's NFS port is reachable. It never
// mounts anything.
func checkDoctorNFSMounts(vs *config.VersionedSettings, probe nfsDoctorProbe) scionruntime.CheckResult {
	const name = "nfs-mounts"
	backend := "local"
	if vs != nil && vs.Server != nil && vs.Server.EffectiveWorkspaceStorage() != nil && vs.Server.EffectiveWorkspaceStorage().Backend != "" {
		backend = vs.Server.EffectiveWorkspaceStorage().Backend
	}
	if backend != "nfs" {
		return scionruntime.CheckResult{
			Name:    name,
			Status:  "skip",
			Message: fmt.Sprintf("NFS workspace storage not configured (server.workspace_storage.backend: %s)", backend),
		}
	}
	nfsCfg, warning := brokerNFSConfig(vs)
	if nfsCfg == nil {
		return scionruntime.CheckResult{
			Name:        name,
			Status:      "fail",
			Message:     warning,
			Remediation: "Fix server.workspace_storage.nfs in the global settings: an absolute mount_root and at least one share with id, server and export",
		}
	}

	// The broker never mounts when its default runtime is Kubernetes or
	// Cloud Run (the platform mounts the export into the agent); it only
	// verifies. Resolved from settings without constructing a runtime; an
	// unresolvable profile is treated as a local-container runtime.
	verifyOnlyRuntime := ""
	if _, rtType, err := vs.ResolveRuntime(""); err == nil && runtimebroker.NFSWarnOnlyRuntime(rtType) {
		verifyOnlyRuntime = rtType
	}

	mode := "auto_mount off"
	remediation := "Mount each export at <mount_root>/<share id> on this host, or set server.workspace_storage.nfs.auto_mount: true on a broker that runs as root"
	switch {
	case nfsCfg.AutoMount && verifyOnlyRuntime != "":
		mode = "auto_mount on, verify only"
		remediation = "The default runtime mounts the export into each agent; mount it on this host only if host-side tools need it"
	case nfsCfg.AutoMount:
		mode = "auto_mount on"
		remediation = "The broker mounts the shares itself: check the broker log (broker.nfs-mount) and that the broker runs as root"
	}

	// Inline pod volumes are selected by shares[0] alone, as in the
	// project-storage Layout and the runtime: no shares[0].pv_name on a
	// Kubernetes default runtime.
	inline := verifyOnlyRuntime != "" && kubernetesRuntimeType(verifyOnlyRuntime) &&
		len(nfsCfg.Shares) > 0 && nfsCfg.Shares[0].PVName == ""

	// A missing mount is a failure only where the broker is expected to
	// mount it: with auto_mount on, no pv_name, and a local-container
	// default runtime; and, in inline mode, for shares[0], which agent
	// starts need (the broker creates each directory before the pod).
	// With auto_mount off the operator may mount it elsewhere; a pv_name
	// share is mounted into pods by the kubelet; and on a Kubernetes or
	// Cloud Run default runtime the broker does not mount. Those are
	// warnings, as in the broker's health. A wrong source, an unreachable
	// server, or an unusable block are failures.
	var ok, problems []string
	failed := false
	for i, share := range nfsCfg.Shares {
		target := filepath.Join(nfsCfg.MountRoot, share.ID)
		want := share.Server + ":" + share.Export
		var issues []string

		source, mounted, err := probe.mountSource(target)
		switch {
		case err != nil:
			issues = append(issues, fmt.Sprintf("mount state unknown (%v)", err))
			failed = true
		case !mounted && share.PVName != "":
			issues = append(issues, fmt.Sprintf("not mounted at %s (pv_name is set, so the export is mounted into pods and this host need not mount it)", target))
		case !mounted && inline && i == 0:
			// No pv_name on Kubernetes: pods mount each directory inline,
			// and the broker must create it first through this mount.
			issues = append(issues, fmt.Sprintf("not mounted at %s (no pv_name: Kubernetes pods mount the share inline, and this broker must mount the export, writable, to create each directory first)", target))
			failed = true
		case !mounted && !nfsCfg.AutoMount:
			issues = append(issues, fmt.Sprintf("not mounted at %s (auto_mount is off, so the broker does not mount it)", target))
		case !mounted && verifyOnlyRuntime != "":
			issues = append(issues, fmt.Sprintf("not mounted at %s (the default runtime is %s, so the broker does not mount it)", target, verifyOnlyRuntime))
		case !mounted:
			issues = append(issues, fmt.Sprintf("not mounted at %s", target))
			failed = true
		case source != want:
			issues = append(issues, fmt.Sprintf("%s is mounted from %s, expected %s", target, source, want))
			failed = true
		}

		if err := probe.dial(net.JoinHostPort(share.Server, nfsServerPort)); err != nil {
			issues = append(issues, fmt.Sprintf("server %s unreachable on port %s", share.Server, nfsServerPort))
			failed = true
		}

		if len(issues) == 0 {
			ok = append(ok, share.ID)
			continue
		}
		if inline && i == 0 {
			// Agent starts need the broker's mount of shares[0], so every
			// failure of it points there, whatever the cause. Pods do not
			// mount the other shares, which keep the mode's remediation.
			remediation = fmt.Sprintf("Mount %s at %s on this host, writable by the broker (no pv_name: agent pods mount the share inline and the broker creates each directory first), or set nfs.shares[0].pv_name", want, target)
		}
		problems = append(problems, share.ID+": "+strings.Join(issues, ", "))
	}

	if len(problems) == 0 {
		return scionruntime.CheckResult{
			Name:    name,
			Status:  "pass",
			Message: fmt.Sprintf("%d share(s) mounted and reachable: %s (%s)", len(ok), strings.Join(ok, ", "), mode),
		}
	}
	status := "warn"
	if failed {
		status = "fail"
	}
	return scionruntime.CheckResult{
		Name:        name,
		Status:      status,
		Message:     fmt.Sprintf("%s (%s)", strings.Join(problems, "; "), mode),
		Remediation: remediation,
	}
}

// kubernetesRuntimeType reports whether a resolved runtime type is the
// Kubernetes runtime ("kubernetes", "k8s" or "remote"), using the settings
// package's runtime-entry predicate.
func kubernetesRuntimeType(rtType string) bool {
	return config.IsKubernetesRuntimeEntry(rtType, config.V1RuntimeConfig{})
}
