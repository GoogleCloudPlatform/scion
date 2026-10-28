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

package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// legacyProfileNFSSettings is a settings file as written before
// server.project_storage: the shared-dir nfs block is global with backend
// local, and one Kubernetes profile selects nfs through the per-profile
// shared_dir_storage_backend override, which is no longer applied
// (ptone/scion#6371). The workspace is not on nfs. A second profile has
// no override.
const legacyProfileNFSSettings = `schema_version: "1"
active_profile: plain
runtimes:
  cluster:
    type: kubernetes
profiles:
  shared-nfs:
    runtime: cluster
    shared_dir_storage_backend: nfs
  plain:
    runtime: cluster
server:
  shared_dir_storage:
    backend: local
    nfs:
      mount_root: /mnt/share
      subpath_root: tree/projects
      shares:
        - id: share-a
          server: 192.0.2.10
          export: /export/a
          pv_name: share-a-pv
`

// legacyProfileOverlayFileSettings is the file of the DB-overlay variant:
// the same server block, with the profiles stored in the hub instead.
const legacyProfileOverlayFileSettings = `schema_version: "1"
active_profile: plain
server:
  shared_dir_storage:
    backend: local
    nfs:
      mount_root: /mnt/share
      subpath_root: tree/projects
      shares:
        - id: share-a
          server: 192.0.2.10
          export: /export/a
          pv_name: share-a-pv
`

// writeUpgradeGlobalSettings points HOME at a temp dir holding content as
// the global settings file, and installs overlay (nil: none) as the hub
// settings overlay a co-located broker reads.
func writeUpgradeGlobalSettings(t *testing.T, content string, overlay *config.SettingsOverlay) {
	t.Helper()
	old := config.GetGlobalSettingsOverlay()
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(old) })
	config.SetGlobalSettingsOverlay(overlay)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0o644))
}

// TestUpgrade_LegacyProfileNFSSharedDirs_NeverDefaultClassRWXClaim is the
// upgrade regression of ptone/scion#6371. A legacy settings file whose
// Kubernetes profile put shared dirs on nfs through the per-profile
// override is loaded through the real global settings loader (from the
// file, and from the hub's stored profiles through the settings overlay),
// resolved as an agent start resolves shared-dir storage, and handed to
// the Kubernetes runtime's claim creation on a fake clientset.
//
// Before the fix the override was ignored: the layout was local and the
// runtime created one ReadWriteMany claim per shared dir with no storage
// class, which the cluster's default class captures (Pending forever on a
// class without RWX). The start must instead be refused naming the key,
// with no claim created; it must never produce a default-class RWX claim.
func TestUpgrade_LegacyProfileNFSSharedDirs_NeverDefaultClassRWXClaim(t *testing.T) {
	overlayWithProfiles := func() *config.SettingsOverlay {
		o := config.NewSettingsOverlay()
		o.Update(
			map[string]config.V1RuntimeConfig{"cluster": {Type: "kubernetes"}},
			map[string]config.V1ProfileConfig{
				"shared-nfs": {Runtime: "cluster", SharedDirStorageBackend: "nfs"},
				"plain":      {Runtime: "cluster"},
			},
			nil)
		return o
	}
	variants := []struct {
		name    string
		file    string
		overlay func() *config.SettingsOverlay
	}{
		{name: "settings file", file: legacyProfileNFSSettings, overlay: func() *config.SettingsOverlay { return nil }},
		{name: "hub stored profiles", file: legacyProfileOverlayFileSettings, overlay: overlayWithProfiles},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			writeUpgradeGlobalSettings(t, v.file, v.overlay())
			vs, _, err := config.LoadGlobalSettingsWithOverlay()
			require.NoError(t, err)

			rt, clientset, _ := newTestK8sRuntime()
			ctx := t.Context()
			cfg := RunConfig{
				Name:  "agent-1",
				Image: "test:latest",
				Labels: map[string]string{
					"scion.project":    "proj",
					"scion.project_id": "pid-1",
				},
				SharedDirs: []api.SharedDir{{Name: "scratchpad"}, {Name: "cache"}},
			}

			refusal := upgradeAgentStartSharedDirStorage(t, vs, "shared-nfs", &cfg)
			if refusal == nil {
				require.NoError(t, rt.createSharedDirPVCs(ctx, "default", cfg))
			}

			pvcs, err := clientset.CoreV1().PersistentVolumeClaims("default").List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			for _, pvc := range pvcs.Items {
				for _, mode := range pvc.Spec.AccessModes {
					if mode == corev1.ReadWriteMany && pvc.Spec.StorageClassName == nil {
						t.Errorf("claim %s requests ReadWriteMany with no storage class (the cluster default class captures it): "+
							"the legacy per-profile nfs override was ignored (ptone/scion#6371)", pvc.Name)
					}
				}
			}
			require.Error(t, refusal, "a start of the profile carrying the removed override must be refused")
			require.Empty(t, pvcs.Items, "a refused start must create no claim")
			var se *config.StorageMigrationRequiredError
			require.ErrorAs(t, refusal, &se)
			require.Contains(t, se.Error(), "profiles.shared-nfs.shared_dir_storage_backend")
			require.Contains(t, se.Error(), "server.shared_dir_storage.backend: \"nfs\"")
			require.Contains(t, se.Error(), `pv_name "share-a-pv"`)
			require.NotContains(t, se.PublicMessage(), "share-a-pv")

			// The profile without the override is admitted, and keeps the
			// local layout's per-dir claims.
			plain := RunConfig{Name: "agent-2", Image: "test:latest", Labels: cfg.Labels, SharedDirs: cfg.SharedDirs}
			require.NoError(t, upgradeAgentStartSharedDirStorage(t, vs, "plain", &plain))
		})
	}
}

// upgradeAgentStartSharedDirStorage resolves shared-dir storage for an
// agent start of profile from vs (the loaded global settings) as the agent
// manager does, and fills cfg's realization when the layout is shared. It
// returns the start's refusal, if any. It mirrors the agent manager's call
// sequence (pkg/runtime cannot import pkg/agent); that Manager.Start itself
// makes the call is proven in pkg/agent by TestStart_RefusedStorageCreatesNothing
// and TestStartSharedDirLayout_ProfileOverrideRefused, and at create
// admission by pkg/runtimebroker TestCreateAgent_StorageRefusalBeforeAnyPreparation.
func upgradeAgentStartSharedDirStorage(t *testing.T, vs *config.VersionedSettings, profile string, cfg *RunConfig) error {
	t.Helper()
	if err := vs.RemovedStorageOverridesForProfile(profile); err != nil {
		return err
	}
	layout, _, err := config.SharedDirStorageLayout(vs.Server)
	require.NoError(t, err)
	if layout.Backend.Shared() {
		cfg.SharedDirStorage = &SharedDirRealization{Backend: string(layout.Backend), PVClaimName: layout.Share.PVName}
	}
	return nil
}
