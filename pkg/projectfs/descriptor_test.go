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
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestLayoutDescriptorDiff(t *testing.T) {
	hub := Layout{
		Backend: BackendNFS, HostBase: "/mnt/nfs/a", SubPathRoot: "projects",
		Share: Share{ID: "a", Server: "10.0.0.2", Export: "/share/a"},
	}
	match := func() *api.BrokerWorkspaceStorage {
		return &api.BrokerWorkspaceStorage{Backend: "nfs", Homes: "local", NFS: &api.BrokerNFSWorkspaceStorage{
			Server: "10.0.0.2", Export: "/share/a/", SubPathRoot: "projects", ExportID: "e1"}}
	}
	homesNFS := hub
	homesNFS.Homes = BackendNFS
	gke := Layout{Backend: BackendGKESharedVolume, HostBase: "/mnt/v", SubPathRoot: "projects"}

	tests := []struct {
		name     string
		l        Layout
		d        func() *api.BrokerWorkspaceStorage
		exportID string
		want     []string
	}{
		{"same tree", hub, match, "e1", nil},
		{"same tree, hub marker unknown", hub, match, "", nil},
		{"local hub never compares", Layout{Backend: BackendLocal}, func() *api.BrokerWorkspaceStorage { return nil }, "", nil},
		{"no descriptor", hub, func() *api.BrokerWorkspaceStorage { return nil }, "", []string{"broker reports no storage layout (upgrade it)"}},
		{"broker local", hub, func() *api.BrokerWorkspaceStorage {
			return &api.BrokerWorkspaceStorage{Backend: "local", Homes: "local"}
		}, "", []string{`backend: hub "nfs", broker "local"`}},
		{"no usable share", hub, func() *api.BrokerWorkspaceStorage { d := match(); d.NFS = nil; return d }, "", []string{"nfs: broker reports no usable share"}},
		{"other server", hub, func() *api.BrokerWorkspaceStorage { d := match(); d.NFS.Server = "10.0.0.3"; return d }, "", []string{`nfs server: hub "10.0.0.2", broker "10.0.0.3"`}},
		{"other export", hub, func() *api.BrokerWorkspaceStorage { d := match(); d.NFS.Export = "/share/b"; return d }, "", []string{`nfs export: hub "/share/a", broker "/share/b"`}},
		{"other subpath root", hub, func() *api.BrokerWorkspaceStorage { d := match(); d.NFS.SubPathRoot = "team"; return d }, "", []string{`subpath_root: hub "projects", broker "team"`}},
		{"other export marker", hub, match, "e2", []string{`export identity marker: hub "e2", broker "e1"`}},
		{"broker marker unknown", hub, func() *api.BrokerWorkspaceStorage { d := match(); d.NFS.ExportID = ""; return d }, "e2", nil},
		{"homes differ", homesNFS, match, "", []string{`homes: hub "nfs", broker "local"`}},
		{"homes unknown, hub local", hub, func() *api.BrokerWorkspaceStorage { d := match(); d.Homes = ""; return d }, "", nil},
		{"homes unknown, hub nfs", homesNFS, func() *api.BrokerWorkspaceStorage { d := match(); d.Homes = ""; return d }, "", []string{`homes: hub "nfs", broker unknown (upgrade it)`}},
		{"volume backend same", gke, func() *api.BrokerWorkspaceStorage {
			return &api.BrokerWorkspaceStorage{Backend: "gke-shared-volume", Homes: "local"}
		}, "", nil},
		{"volume backend differs", gke, func() *api.BrokerWorkspaceStorage { return &api.BrokerWorkspaceStorage{Backend: "nfs", Homes: "local"} }, "", []string{`backend: hub "gke-shared-volume", broker "nfs"`}},
		{"several", hub, func() *api.BrokerWorkspaceStorage {
			d := match()
			d.NFS.Server, d.Homes = "x", "nfs"
			return d
		}, "", []string{`nfs server: hub "10.0.0.2", broker "x"`, `homes: hub "local", broker "nfs"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.l.DescriptorDiff(tt.d(), tt.exportID)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DescriptorDiff = %q, want %q", got, tt.want)
			}
		})
	}
}
