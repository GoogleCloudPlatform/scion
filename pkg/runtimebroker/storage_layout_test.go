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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

// TestWorkspaceStorageDescriptor_ReportsHomes: the heartbeat descriptor
// carries the broker Layout's homes (ptone/scion#5823).
func TestWorkspaceStorageDescriptor_ReportsHomes(t *testing.T) {
	for _, tc := range []struct {
		homes projectfs.Backend
		want  string
	}{{"", "local"}, {projectfs.BackendLocal, "local"}, {projectfs.BackendNFS, "nfs"}} {
		s := &Server{config: ServerConfig{StorageLayout: projectfs.Layout{Backend: projectfs.BackendLocal, Homes: tc.homes}}}
		if got := s.workspaceStorageDescriptor().Homes; got != tc.want {
			t.Errorf("homes %q: descriptor Homes = %q, want %q", tc.homes, got, tc.want)
		}
	}
}

// TestStorageVolume: the descriptor identifies a volume backend's tree.
func TestStorageVolume(t *testing.T) {
	if v := StorageVolume(projectfs.Layout{Backend: projectfs.BackendNFS}); v != nil {
		t.Errorf("nfs has no volume identity, got %+v", v)
	}
	v := StorageVolume(projectfs.Layout{Backend: projectfs.BackendGKESharedVolume, Share: projectfs.Share{PVName: "claim"}})
	if v == nil || v.ClaimName != "claim" || v.SubPathRoot != "projects" {
		t.Errorf("gke volume identity = %+v", v)
	}
	s := &Server{config: ServerConfig{StorageLayout: projectfs.Layout{Backend: projectfs.BackendCloudRunVolume, SubPathRoot: "team"}}}
	if got := s.workspaceStorageDescriptor().Volume; got == nil || got.SubPathRoot != "team" || got.ClaimName != "" {
		t.Errorf("cloudrun volume identity = %+v", got)
	}
}
