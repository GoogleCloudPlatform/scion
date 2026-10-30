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
// descriptor, reports its storage settings invalid, or (for nfs) has no
// usable share, disagrees. A broker that
// predates the homes field disagrees only when this Layout keeps homes on
// nfs. For the volume backends: the subpath root and, for
// gke-shared-volume, the claim. Share IDs, mount roots and volume names are
// host-local naming and are not compared; nor is share health. The lines
// carry the values (the log view): a response a non-admin can read renders
// DescriptorIssues with ViewPublic instead.
func (l Layout) DescriptorDiff(d *api.BrokerWorkspaceStorage, hubExportID string) []string {
	return RenderLayoutIssues(l.DescriptorIssues(d, hubExportID), ViewLog)
}

// DescriptorIssues is DescriptorDiff as structured issues, for callers that
// render them for a reader (RenderLayoutIssues): a response a non-admin can
// read uses ViewPublic, which names the fields only.
func (l Layout) DescriptorIssues(d *api.BrokerWorkspaceStorage, hubExportID string) []LayoutIssue {
	if !l.Backend.Shared() && l.SharedDirs == nil {
		return nil
	}
	if d == nil {
		return []LayoutIssue{reasonIssue("storage layout", "broker reports none (upgrade it)", "broker reports no storage layout (upgrade it)")}
	}
	if d.Invalid {
		return []LayoutIssue{reasonIssue("storage settings", "broker reports them invalid (fix them on the broker and restart it)",
			"broker reports invalid project storage settings (fix them on the broker and restart it)")}
	}
	var diff []LayoutIssue
	q := func(v string) string { return fmt.Sprintf("%q", v) }
	if Backend(d.Backend) != l.Backend {
		diff = append(diff, mismatch("backend", q(string(l.Backend)), q(d.Backend), string(l.Backend), d.Backend))
	} else if l.Backend == BackendCloudRunVolume || l.Backend == BackendGKESharedVolume {
		root, _ := l.subPathRoot()
		if d.Volume == nil {
			diff = append(diff, reasonIssue("volume", "broker reports no volume identity (upgrade it)", "volume: broker reports no volume identity (upgrade it)"))
		} else {
			if d.Volume.SubPathRoot != root {
				diff = append(diff, mismatch("subpath_root", q(root), q(d.Volume.SubPathRoot), root, d.Volume.SubPathRoot))
			}
			if l.Share.PVName != "" && d.Volume.ClaimName != l.Share.PVName {
				diff = append(diff, mismatch("volume claim", q(l.Share.PVName), q(d.Volume.ClaimName), l.Share.PVName, d.Volume.ClaimName))
			}
		}
	} else if l.Backend == BackendNFS {
		root, _ := l.subPathRoot()
		if d.NFS == nil {
			diff = append(diff, reasonIssue("nfs", "broker reports no usable share", "nfs: broker reports no usable share"))
		} else {
			if d.NFS.Server != l.Share.Server {
				diff = append(diff, mismatch("nfs server", q(l.Share.Server), q(d.NFS.Server), l.Share.Server, d.NFS.Server))
			}
			if api.NormalizeExportPath(d.NFS.Export) != api.NormalizeExportPath(l.Share.Export) {
				diff = append(diff, mismatch("nfs export", q(l.Share.Export), q(d.NFS.Export), l.Share.Export, d.NFS.Export))
			}
			if d.NFS.SubPathRoot != root {
				diff = append(diff, mismatch("subpath_root", q(root), q(d.NFS.SubPathRoot), root, d.NFS.SubPathRoot))
			}
			if hubExportID != "" && d.NFS.ExportID != "" && hubExportID != d.NFS.ExportID {
				diff = append(diff, mismatch("export identity marker", q(hubExportID), q(d.NFS.ExportID), hubExportID, d.NFS.ExportID))
			}
		}
	}
	if hubSD := SharedDirsPlacement(l); hubSD != d.SharedDirs {
		diff = append(diff, mismatch("shared dirs", placementLabel(hubSD), placementLabel(d.SharedDirs), hubSD, d.SharedDirs))
	}
	hubHomes := homesOrLocal(l.Homes)
	switch {
	case d.Homes == "":
		if hubHomes != BackendLocal {
			diff = append(diff, mismatch("homes", q(string(hubHomes)), "unknown (upgrade it)", string(hubHomes), ""))
		}
	case Backend(d.Homes) != hubHomes:
		diff = append(diff, mismatch("homes", q(string(hubHomes)), q(d.Homes), string(hubHomes), d.Homes))
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
