//go:build unix

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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// readOrCreateExportOwner returns the canonical owner uid:gid stored in
// <dir>/.scion-export-owner (beside .scion-export-id), creating the marker
// with uid:gid when it does not exist. dir itself is never created, so nothing
// is written to local disk under an unmounted mountpoint. Creation is
// race-free across brokers sharing the export: a temp file is hard-linked to
// the marker name so one broker wins and all others read the winner's ids. A
// malformed marker is an error and is never overwritten.
func readOrCreateExportOwner(dir string, uid, gid int) (int, int, error) {
	marker := filepath.Join(dir, api.ExportOwnerMarkerName)
	ownerUID, ownerGID, err := readExportOwnerMarker(marker)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return ownerUID, ownerGID, err
	}
	tmp, err := os.CreateTemp(dir, api.ExportOwnerMarkerName+".tmp-*")
	if err != nil {
		return 0, 0, fmt.Errorf("create export owner marker in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	_, werr := fmt.Fprintf(tmp, "%d:%d\n", uid, gid)
	if serr := tmp.Sync(); werr == nil {
		werr = serr
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return 0, 0, fmt.Errorf("write export owner marker %s: %w", tmpName, werr)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return 0, 0, fmt.Errorf("chmod export owner marker %s: %w", tmpName, err)
	}
	// Link fails with EEXIST when another broker created the marker first;
	// either way the marker now exists and is read back.
	if err := os.Link(tmpName, marker); err != nil && !errors.Is(err, fs.ErrExist) {
		return 0, 0, fmt.Errorf("create export owner marker %s: %w", marker, err)
	}
	return readExportOwnerMarker(marker)
}

// effectiveAgentOwnerIDs returns the effective uid and gid agents started by
// this broker run as on a shared project-storage layout.
//
//   - Kubernetes: pods always run as K8sContainerUID (1000, ptone/scion#2608),
//     with fsGroup set to the resolved layout GID (default 1000).
//   - Cloud Run sandbox: sandbox containers run as 0:0.
//   - Docker, Podman, Apple, Cloud Run and other container runtimes: use the
//     advertised NFS owner ids (resolved layout UID/GID, defaulting to 1000).
func effectiveAgentOwnerIDs(runtimeName string, layout projectfs.Layout) (uid, gid int, ok bool) {
	if !layout.Backend.Shared() {
		return 0, 0, false
	}
	if isKubernetesRuntimeName(runtimeName) {
		return int(scionrt.K8sContainerUID), provision.DefaultOwnerID(layout.GID), true
	}
	if runtimeName == "cloudrun-sandbox" {
		return 0, 0, true
	}
	u, g := scionrt.AdvertisedHostOwnerIDs("nfs", layout.UID, layout.GID)
	return u, g, true
}
