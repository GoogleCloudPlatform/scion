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
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestDoctorSharedDirStorage: the doctor resolves the shared-dir class as an
// agent start does (the profile's value, else its runtime entry's) and reports
// a shared project storage backend, where no per-dir claim is used.
func TestDoctorSharedDirStorage(t *testing.T) {
	vs := &config.VersionedSettings{
		ActiveProfile: "gke",
		Runtimes:      map[string]config.V1RuntimeConfig{"k8s": {Type: "kubernetes", SharedDirStorageClass: "runtime-rwx"}},
		Profiles: map[string]config.V1ProfileConfig{
			"gke":   {Runtime: "k8s"},
			"fast":  {Runtime: "k8s", SharedDirStorageClass: "profile-rwx"},
			"other": {Runtime: "missing"},
		},
	}
	nfs := &config.V1ServerConfig{ProjectStorage: &config.V1ProjectStorageConfig{
		Backend: "nfs",
		NFS:     &config.V1NFSConfig{MountRoot: "/mnt/scion-nfs", Shares: []config.V1NFSShare{{ID: "a", Server: "10.0.0.2", Export: "/vol-a", PVName: "pv-a"}}},
	}}
	local := &config.V1ServerConfig{ProjectStorage: &config.V1ProjectStorageConfig{Backend: "local"}}
	gke := &config.V1ServerConfig{ProjectStorage: &config.V1ProjectStorageConfig{
		Backend:         "gke-shared-volume",
		GKESharedVolume: &config.V1GKESharedVolumeConfig{VolumeName: "v", PVClaimName: "c"},
	}}
	// A gke-shared-volume backend without its volume name: the Layout does
	// not resolve, so it is reported as not shared (the start refuses on the
	// settings error itself).
	invalid := &config.V1ServerConfig{ProjectStorage: &config.V1ProjectStorageConfig{Backend: "gke-shared-volume"}}

	for _, tc := range []struct {
		name       string
		vs         *config.VersionedSettings
		srv        *config.V1ServerConfig
		profile    string
		wantClass  string
		wantShared bool
	}{
		{"active profile takes its runtime entry's class", vs, nil, "", "runtime-rwx", false},
		{"profile class wins", vs, nil, "fast", "profile-rwx", false},
		{"profile without a class on any rung", vs, nil, "other", "", false},
		{"local project storage", vs, local, "", "runtime-rwx", false},
		{"nfs project storage is shared", vs, nfs, "", "runtime-rwx", true},
		{"gke-shared-volume project storage is shared", vs, gke, "", "runtime-rwx", true},
		{"invalid layout is not shared", vs, invalid, "", "runtime-rwx", false},
		{"no settings", nil, nil, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			class, shared := doctorSharedDirStorage(tc.vs, tc.srv, tc.profile)
			assert.Equal(t, tc.wantClass, class)
			assert.Equal(t, tc.wantShared, shared)
		})
	}
}
