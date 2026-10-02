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
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// brokerNFSConfig returns the NFS settings for the runtime broker's
// ServerConfig.NFSConfig, taken from server.workspace_storage in the
// broker's global settings. It returns nil (NFS handling off, the broker
// behaves exactly as without NFS) unless the backend is "nfs" and the block
// is valid (see validateBrokerNFS). The returned value is a copy with the workspace
// storage defaults applied; vs is not modified.
//
// The second return value is a warning for the caller to log, set when the
// backend is "nfs" but the block is unusable.
func brokerNFSConfig(vs *config.VersionedSettings) (*config.V1NFSConfig, string) {
	if vs == nil || vs.Server == nil || vs.Server.WorkspaceStorage == nil {
		return nil, ""
	}
	ws := *vs.Server.WorkspaceStorage
	if ws.Backend != "nfs" {
		return nil, ""
	}
	if ws.NFS != nil {
		nfsCopy := *ws.NFS
		nfsCopy.Shares = append([]config.V1NFSShare(nil), ws.NFS.Shares...)
		ws.NFS = &nfsCopy
	}
	ws.ApplyNFSDefaults()
	if err := ws.ValidateNFS(); err != nil {
		return nil, "NFS mount checks disabled: " + err.Error()
	}
	if err := validateBrokerNFS(ws.NFS); err != nil {
		return nil, "NFS mount checks disabled: " + err.Error()
	}
	return ws.NFS, ""
}

// validateBrokerNFS checks the fields the broker uses to build each mount:
// an absolute mount_root, and per share a unique id that is a single path
// element, a server, and an absolute export.
func validateBrokerNFS(nfs *config.V1NFSConfig) error {
	if nfs.MountRoot == "" || !filepath.IsAbs(nfs.MountRoot) {
		return fmt.Errorf("server.workspace_storage.nfs.mount_root must be an absolute path (got %q)", nfs.MountRoot)
	}
	seen := make(map[string]bool, len(nfs.Shares))
	for i, share := range nfs.Shares {
		switch {
		case share.ID == "" || share.ID == "." || share.ID == ".." || strings.ContainsAny(share.ID, `/\`):
			return fmt.Errorf("server.workspace_storage.nfs.shares[%d].id must be a single path element (got %q)", i, share.ID)
		case seen[share.ID]:
			return fmt.Errorf("server.workspace_storage.nfs.shares[%d].id %q is duplicated", i, share.ID)
		case share.Server == "":
			return fmt.Errorf("server.workspace_storage.nfs.shares[%d].server is empty", i)
		case !strings.HasPrefix(share.Export, "/"):
			return fmt.Errorf("server.workspace_storage.nfs.shares[%d].export must be an absolute path (got %q)", i, share.Export)
		}
		seen[share.ID] = true
	}
	return nil
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
		mountSource: procMountSource,
		dial: func(addr string) error {
			conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
			if err != nil {
				return err
			}
			return conn.Close()
		},
	}
}

// procMountSource looks path up in /proc/mounts (Linux). It reads the mount
// table only; it never stats the mountpoint, so a hung NFS mount cannot
// block it.
func procMountSource(path string) (string, bool, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return "", false, fmt.Errorf("reading mount table: %w", err)
	}
	defer func() { _ = f.Close() }()
	return findMountSource(f, path)
}

// findMountSource scans a /proc/mounts-format table for path. When path is
// mounted more than once, the last (topmost) entry wins.
func findMountSource(r io.Reader, path string) (string, bool, error) {
	clean := filepath.Clean(path)
	var source string
	var found bool
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		if filepath.Clean(unescapeMountField(fields[1])) == clean {
			source, found = unescapeMountField(fields[0]), true
		}
	}
	if err := scanner.Err(); err != nil {
		return "", false, fmt.Errorf("reading mount table: %w", err)
	}
	return source, found, nil
}

// unescapeMountField decodes the octal escapes (\040 for space, etc.) the
// kernel uses in /proc/mounts fields.
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
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
	if vs != nil && vs.Server != nil && vs.Server.WorkspaceStorage != nil && vs.Server.WorkspaceStorage.Backend != "" {
		backend = vs.Server.WorkspaceStorage.Backend
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

	mode := "auto_mount off"
	remediation := "Mount each export at <mount_root>/<share id> on this host, or set server.workspace_storage.nfs.auto_mount: true on a broker that runs as root"
	if nfsCfg.AutoMount {
		mode = "auto_mount on"
		remediation = "The broker mounts the shares itself: check the broker log (broker.nfs-mount) and that the broker runs as root"
	}

	var ok, problems []string
	for _, share := range nfsCfg.Shares {
		target := filepath.Join(nfsCfg.MountRoot, share.ID)
		want := share.Server + ":" + share.Export
		var issues []string

		source, mounted, err := probe.mountSource(target)
		switch {
		case err != nil:
			issues = append(issues, fmt.Sprintf("mount state unknown (%v)", err))
		case !mounted:
			issues = append(issues, fmt.Sprintf("not mounted at %s", target))
		case source != want:
			issues = append(issues, fmt.Sprintf("%s is mounted from %s, expected %s", target, source, want))
		}

		if share.Server == "" {
			issues = append(issues, "no server configured")
		} else if err := probe.dial(net.JoinHostPort(share.Server, nfsServerPort)); err != nil {
			issues = append(issues, fmt.Sprintf("server %s unreachable on port %s", share.Server, nfsServerPort))
		}

		if len(issues) == 0 {
			ok = append(ok, share.ID)
			continue
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
	return scionruntime.CheckResult{
		Name:        name,
		Status:      "fail",
		Message:     fmt.Sprintf("%s (%s)", strings.Join(problems, "; "), mode),
		Remediation: remediation,
	}
}
