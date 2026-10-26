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

package projectfs

import (
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// homesOrLocal is a Homes value with the empty default spelled out.
func homesOrLocal(b Backend) Backend {
	if b == "" {
		return BackendLocal
	}
	return b
}

// DescriptorDiff compares this Layout (a hub's) with the storage layout a
// runtime broker reports in its heartbeat descriptor, and returns one line
// per disagreement, or nil when the broker places the project tree where
// the hub does (design §P2: "broker and hub compare Layout identity").
//
// Only a shared Layout, or a local Layout with a shared-dir split, has an
// identity to compare: on a local Layout without a split every broker keeps
// its own trees and nil is returned. Compared: the backend;
// for nfs the share's server and export (trailing slashes ignored) and the
// subpath root; the export identity marker when both sides know it
// (hubExportID, d.NFS.ExportID); and homes. A broker that reports no
// descriptor, or (for nfs) no usable share, disagrees. A broker that
// predates the homes field disagrees only when this Layout keeps homes on
// nfs. For the volume backends: the subpath root and, for
// gke-shared-volume, the claim. Share IDs, mount roots and volume names are
// host-local naming and are not compared; nor is share health.
func (l Layout) DescriptorDiff(d *api.BrokerWorkspaceStorage, hubExportID string) []string {
	if !l.Backend.Shared() && l.SharedDirs == nil {
		return nil
	}
	if d == nil {
		return []string{"broker reports no storage layout (upgrade it)"}
	}
	var diff []string
	if Backend(d.Backend) != l.Backend {
		diff = append(diff, fmt.Sprintf("backend: hub %q, broker %q", l.Backend, d.Backend))
	} else if l.Backend == BackendCloudRunVolume || l.Backend == BackendGKESharedVolume {
		root, _ := l.subPathRoot()
		if d.Volume == nil {
			diff = append(diff, "volume: broker reports no volume identity (upgrade it)")
		} else {
			if d.Volume.SubPathRoot != root {
				diff = append(diff, fmt.Sprintf("subpath_root: hub %q, broker %q", root, d.Volume.SubPathRoot))
			}
			if l.Share.PVName != "" && d.Volume.ClaimName != l.Share.PVName {
				diff = append(diff, fmt.Sprintf("volume claim: hub %q, broker %q", l.Share.PVName, d.Volume.ClaimName))
			}
		}
	} else if l.Backend == BackendNFS {
		root, _ := l.subPathRoot()
		if d.NFS == nil {
			diff = append(diff, "nfs: broker reports no usable share")
		} else {
			if d.NFS.Server != l.Share.Server {
				diff = append(diff, fmt.Sprintf("nfs server: hub %q, broker %q", l.Share.Server, d.NFS.Server))
			}
			if api.NormalizeExportPath(d.NFS.Export) != api.NormalizeExportPath(l.Share.Export) {
				diff = append(diff, fmt.Sprintf("nfs export: hub %q, broker %q", l.Share.Export, d.NFS.Export))
			}
			if d.NFS.SubPathRoot != root {
				diff = append(diff, fmt.Sprintf("subpath_root: hub %q, broker %q", root, d.NFS.SubPathRoot))
			}
			if hubExportID != "" && d.NFS.ExportID != "" && hubExportID != d.NFS.ExportID {
				diff = append(diff, fmt.Sprintf("export identity marker: hub %q, broker %q", hubExportID, d.NFS.ExportID))
			}
		}
	}
	if hubSD := SharedDirsPlacement(l); hubSD != d.SharedDirs {
		diff = append(diff, fmt.Sprintf("shared dirs: hub %s, broker %s", placementLabel(hubSD), placementLabel(d.SharedDirs)))
	}
	hubHomes := homesOrLocal(l.Homes)
	switch {
	case d.Homes == "":
		if hubHomes != BackendLocal {
			diff = append(diff, fmt.Sprintf("homes: hub %q, broker unknown (upgrade it)", hubHomes))
		}
	case Backend(d.Homes) != hubHomes:
		diff = append(diff, fmt.Sprintf("homes: hub %q, broker %q", hubHomes, d.Homes))
	}
	return diff
}

// SharedDirsPlacement is the shared-dir placement a broker reports for l
// (api.BrokerWorkspaceStorage.SharedDirs): the transitional split's backend,
// or "" when shared dirs are with the workspace.
func SharedDirsPlacement(l Layout) string {
	if l.SharedDirs == nil {
		return ""
	}
	return string(l.SharedDirs.Backend)
}

func placementLabel(p string) string {
	if p == "" {
		return "with the workspace"
	}
	return fmt.Sprintf("%q", p)
}
