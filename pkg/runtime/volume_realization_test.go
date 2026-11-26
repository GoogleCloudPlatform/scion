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
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercontrol"
)

func TestRealizeVolume_Matrix(t *testing.T) {
	bindRuntimes := []string{"docker", "podman", "container", "apple-container", "", "DOCKER"}
	for _, rt := range bindRuntimes {
		t.Run("bind/"+rt, func(t *testing.T) {
			cases := []struct {
				name string
				vol  api.VolumeMount
				want VolumeRealization
			}{
				{"empty host path", api.VolumeMount{Source: "/tmp/data", Target: "/mnt/d"}, RealizeHostBind},
				{"local host path", api.VolumeMount{Type: "local", Source: "/tmp/data", Target: "/mnt/d"}, RealizeHostBind},
				{"LOCAL uppercase", api.VolumeMount{Type: "LOCAL", Source: "/tmp/data", Target: "/mnt/d"}, RealizeHostBind},
				{"local padded", api.VolumeMount{Type: " Local ", Source: "/tmp/data", Target: "/mnt/d"}, RealizeHostBind},
				{"empty named vol", api.VolumeMount{Source: "cachevol", Target: "/mnt/c"}, RealizeNamed},
				{"local named vol", api.VolumeMount{Type: "local", Source: "cachevol", Target: "/mnt/c"}, RealizeNamed},
				{"LOCAL named vol", api.VolumeMount{Type: "LOCAL", Source: "cachevol", Target: "/mnt/c"}, RealizeNamed},
				{"nfs host path", api.VolumeMount{Type: "nfs", Source: "/mnt/nfs/share", Target: "/mnt/n"}, RealizeHostBind},
				{"NFS uppercase", api.VolumeMount{Type: "NFS", Source: "/mnt/nfs/share", Target: "/mnt/n"}, RealizeHostBind},
				{"nfs bare name is still host bind", api.VolumeMount{Type: "nfs", Source: "sharename", Target: "/mnt/n"}, RealizeHostBind},
				{"gcs", api.VolumeMount{Type: "gcs", Bucket: "b", Target: "/mnt/g"}, RealizeFUSE},
				{"GCS uppercase", api.VolumeMount{Type: "GCS", Bucket: "b", Target: "/mnt/g"}, RealizeFUSE},
				{"Gcs padded", api.VolumeMount{Type: " Gcs ", Bucket: "b", Target: "/mnt/g"}, RealizeFUSE},
				{"cloudrun-volume", api.VolumeMount{Type: "cloudrun-volume", VolumeName: "v", Target: "/mnt/cr"}, RealizeUnsupported},
				{"CLOUDRUN-VOLUME", api.VolumeMount{Type: "CLOUDRUN-VOLUME", VolumeName: "v", Target: "/mnt/cr"}, RealizeUnsupported},
				{"gke-shared-volume", api.VolumeMount{Type: "gke-shared-volume", VolumeName: "v", Target: "/mnt/gke"}, RealizeUnsupported},
				{"GKE-SHARED-VOLUME", api.VolumeMount{Type: "GKE-SHARED-VOLUME", VolumeName: "v", Target: "/mnt/gke"}, RealizeUnsupported},
				{"unknown type", api.VolumeMount{Type: "tmpfs", Target: "/mnt/t"}, RealizeUnsupported},
			}
			for _, tc := range cases {
				if got := RealizeVolume(tc.vol, rt); got != tc.want {
					t.Errorf("%s: RealizeVolume(%+v, %q) = %s, want %s", tc.name, tc.vol, rt, got, tc.want)
				}
			}
		})
	}

	for _, rt := range []string{"kubernetes", "k8s", "Kubernetes"} {
		t.Run("k8s/"+rt, func(t *testing.T) {
			cases := []struct {
				vol  api.VolumeMount
				want VolumeRealization
			}{
				{api.VolumeMount{Type: "gcs", Bucket: "b", Target: "/mnt/g"}, RealizeCSI},
				{api.VolumeMount{Type: "GCS", Bucket: "b", Target: "/mnt/g"}, RealizeCSI},
				{api.VolumeMount{Type: " Gcs ", Bucket: "b", Target: "/mnt/g"}, RealizeCSI},
				{api.VolumeMount{Type: "", Source: "/tmp/d", Target: "/mnt/d"}, RealizeUnsupported},
				{api.VolumeMount{Type: "local", Source: "/tmp/d", Target: "/mnt/d"}, RealizeUnsupported},
				{api.VolumeMount{Type: "LOCAL", Source: "/tmp/d", Target: "/mnt/d"}, RealizeUnsupported},
				{api.VolumeMount{Type: "nfs", Source: "/export", Target: "/mnt/n"}, RealizeUnsupported},
				{api.VolumeMount{Type: "cloudrun-volume", VolumeName: "v", Target: "/mnt/cr"}, RealizeUnsupported},
				{api.VolumeMount{Type: "gke-shared-volume", VolumeName: "v", Target: "/mnt/gke"}, RealizeUnsupported},
			}
			for _, tc := range cases {
				if got := RealizeVolume(tc.vol, rt); got != tc.want {
					t.Errorf("RealizeVolume(%+v, %q) = %s, want %s", tc.vol, rt, got, tc.want)
				}
			}
		})
	}

	for _, rt := range []string{"cloudrun", "cloudrun-instances", "cloudrun-sandbox", "substrate"} {
		t.Run("other/"+rt, func(t *testing.T) {
			for _, volType := range []string{"", "local", "LOCAL", "nfs", "gcs", "GCS", "cloudrun-volume", "gke-shared-volume"} {
				v := api.VolumeMount{Type: volType, Source: "/tmp/d", Bucket: "b", VolumeName: "v", Target: "/mnt/x"}
				if got := RealizeVolume(v, rt); got != RealizeUnsupported {
					t.Errorf("RealizeVolume(type=%q, %q) = %s, want %s", volType, rt, got, RealizeUnsupported)
				}
			}
		})
	}
}

// TestVolumeRealization_BindRuntimes_ConfigAndLaunch verifies the §8 matrix
// across every bind runtime at both config time (CheckVolumeSourcesForRuntime,
// CheckWorkspaceVolumeSourcesForRuntime) and launch time (buildCommonRunArgs).
func TestVolumeRealization_BindRuntimes_ConfigAndLaunch(t *testing.T) {
	_, global, outside := controlPathFixture(t)
	controlSrc := filepath.Join(global, "settings.yaml")

	bindRuntimes := []string{"docker", "podman", "container", "apple-container"}
	for _, rt := range bindRuntimes {
		t.Run(rt, func(t *testing.T) {
			// 1. Host-bind types: local, "", nfs, LOCAL must be guarded as host
			// binds at both config time and launch time.
			for _, volType := range []string{"local", "", "nfs", "LOCAL"} {
				t.Run("host_bind/"+volType, func(t *testing.T) {
					cleanVol := api.VolumeMount{Type: volType, Source: outside, Target: "/mnt/ok"}
					badVol := api.VolumeMount{Type: volType, Source: controlSrc, Target: "/mnt/bad"}
					badWSVol := api.VolumeMount{Type: volType, Source: controlSrc, Target: "/workspace"}

					// Config time: clean allowed, control path refused.
					if err := CheckVolumeSourcesForRuntime([]api.VolumeMount{cleanVol}, rt); err != nil {
						t.Fatalf("CheckVolumeSourcesForRuntime(clean %q) = %v, want nil", volType, err)
					}
					if err := CheckVolumeSourcesForRuntime([]api.VolumeMount{badVol}, rt); !errors.Is(err, brokercontrol.ErrControlPathMount) {
						t.Fatalf("CheckVolumeSourcesForRuntime(bad %q) = %v, want ErrControlPathMount", volType, err)
					}
					if err := CheckWorkspaceVolumeSourcesForRuntime([]api.VolumeMount{badWSVol}, rt); !errors.Is(err, brokercontrol.ErrControlPathMount) {
						t.Fatalf("CheckWorkspaceVolumeSourcesForRuntime(bad %q) = %v, want ErrControlPathMount", volType, err)
					}

					// Launch time: clean emits bind mount, control path refused.
					cfgClean := controlPathRunConfig(cleanVol)
					cfgClean.RuntimeName = rt
					args, err := buildCommonRunArgs(cfgClean)
					if err != nil {
						t.Fatalf("buildCommonRunArgs(clean %q) = %v, want nil", volType, err)
					}
					if !strings.Contains(strings.Join(args, "\n"), outside+":/mnt/ok") {
						t.Fatalf("buildCommonRunArgs(clean %q) missing bind %s:/mnt/ok", volType, outside)
					}

					cfgBad := controlPathRunConfig(badVol)
					cfgBad.RuntimeName = rt
					if _, err := buildCommonRunArgs(cfgBad); !errors.Is(err, brokercontrol.ErrControlPathMount) {
						t.Fatalf("buildCommonRunArgs(bad %q) = %v, want ErrControlPathMount", volType, err)
					}
				})
			}

			// 2. gcs / GCS: FUSE at launch time, allowed at config time.
			for _, volType := range []string{"gcs", "GCS"} {
				t.Run("fuse/"+volType, func(t *testing.T) {
					v := api.VolumeMount{Type: volType, Bucket: "my-bucket", Source: controlSrc, Target: "/mnt/gcs"}
					if err := CheckVolumeSourcesForRuntime([]api.VolumeMount{v}, rt); err != nil {
						t.Fatalf("CheckVolumeSourcesForRuntime(%q) = %v, want nil", volType, err)
					}
					if err := CheckWorkspaceVolumeSourcesForRuntime([]api.VolumeMount{{Type: volType, Bucket: "my-bucket", Target: "/workspace"}}, rt); err != nil {
						t.Fatalf("CheckWorkspaceVolumeSourcesForRuntime(%q) = %v, want nil", volType, err)
					}

					cfg := controlPathRunConfig(v)
					cfg.RuntimeName = rt
					args, err := buildCommonRunArgs(cfg)
					if err != nil {
						t.Fatalf("buildCommonRunArgs(%q) = %v, want nil", volType, err)
					}
					joined := strings.Join(args, " ")
					if !strings.Contains(joined, "gcsfuse") || !strings.Contains(joined, "my-bucket") {
						t.Fatalf("buildCommonRunArgs(%q) did not emit gcsfuse mount: %s", volType, joined)
					}
					if strings.Contains(joined, controlSrc+":/mnt/gcs") {
						t.Fatalf("buildCommonRunArgs(%q) unexpectedly emitted bind mount: %s", volType, joined)
					}
				})
			}

			// 3. cloudrun-volume / gke-shared-volume: refused with typed error
			// at config time AND launch time (wrapped in *LaunchFailure).
			for _, volType := range []string{"cloudrun-volume", "gke-shared-volume"} {
				t.Run("unsupported/"+volType, func(t *testing.T) {
					v := api.VolumeMount{Type: volType, VolumeName: "shared-vol", Source: outside, Target: "/mnt/u"}
					wsVol := api.VolumeMount{Type: volType, VolumeName: "shared-vol", Source: outside, Target: "/workspace"}

					// Config time
					err := CheckVolumeSourcesForRuntime([]api.VolumeMount{v}, rt)
					var uve *UnsupportedVolumeTypeError
					if !errors.As(err, &uve) || !errors.Is(err, ErrUnsupportedVolumeType) {
						t.Fatalf("CheckVolumeSourcesForRuntime(%q) = %v, want *UnsupportedVolumeTypeError", volType, err)
					}
					if uve.Type != volType || uve.Target != "/mnt/u" {
						t.Errorf("UnsupportedVolumeTypeError = %+v, want type=%q target=/mnt/u", uve, volType)
					}
					if strings.Contains(err.Error(), outside) {
						t.Errorf("error %q leaked host path %q", err.Error(), outside)
					}

					if err := CheckWorkspaceVolumeSourcesForRuntime([]api.VolumeMount{wsVol}, rt); !errors.Is(err, ErrUnsupportedVolumeType) {
						t.Fatalf("CheckWorkspaceVolumeSourcesForRuntime(%q) = %v, want ErrUnsupportedVolumeType", volType, err)
					}

					// Launch time
					cfg := controlPathRunConfig(v)
					cfg.RuntimeName = rt
					_, err = buildCommonRunArgs(cfg)
					if !errors.Is(err, ErrUnsupportedVolumeType) {
						t.Fatalf("buildCommonRunArgs(%q) = %v, want ErrUnsupportedVolumeType", volType, err)
					}
					var lf *LaunchFailure
					if !errors.As(err, &lf) {
						t.Fatalf("buildCommonRunArgs(%q) = %T (%v), want *LaunchFailure", volType, err, err)
					}
					if lf.Code != api.LaunchCodeUnsupportedVolumeType {
						t.Errorf("LaunchFailure.Code = %q, want %q", lf.Code, api.LaunchCodeUnsupportedVolumeType)
					}
				})
			}
		})
	}
}

func TestVolumeRealization_KubernetesAndCloudRun(t *testing.T) {
	_, global, outside := controlPathFixture(t)
	controlSrc := filepath.Join(global, "settings.yaml")

	t.Run("kubernetes config and launch", func(t *testing.T) {
		vols := []api.VolumeMount{
			{Type: "gcs", Bucket: "b1", Target: "/mnt/g1"},
			{Type: "GCS", Bucket: "b2", Target: "/mnt/g2"},
			{Type: "gke-shared-volume", VolumeName: "v", Target: "/mnt/gke"},
			{Type: "cloudrun-volume", VolumeName: "v", Target: "/mnt/cr"},
			{Type: "local", Source: outside, Target: "/mnt/local"},
			{Type: "nfs", Source: "/export", Target: "/mnt/nfs"},
		}
		if err := CheckVolumeSourcesForRuntime(vols, "kubernetes"); err != nil {
			t.Fatalf("CheckVolumeSourcesForRuntime(kubernetes) = %v, want nil", err)
		}
		// Workspace volume on kubernetes with a control-path host Source is still refused.
		if err := CheckWorkspaceVolumeSourcesForRuntime([]api.VolumeMount{{Source: controlSrc, Target: "/workspace"}}, "kubernetes"); !errors.Is(err, brokercontrol.ErrControlPathMount) {
			t.Fatalf("CheckWorkspaceVolumeSourcesForRuntime(kubernetes control path) = %v, want ErrControlPathMount", err)
		}
		// Workspace gke-shared-volume with no host Source is allowed on kubernetes.
		if err := CheckWorkspaceVolumeSourcesForRuntime([]api.VolumeMount{{Type: "gke-shared-volume", VolumeName: "v", Target: "/workspace"}}, "kubernetes"); err != nil {
			t.Fatalf("CheckWorkspaceVolumeSourcesForRuntime(kubernetes gke-shared-volume) = %v, want nil", err)
		}

		// Launch time: buildPod realizes both "gcs" and "GCS" as CSI volumes and skips others.
		rt, _, _ := newTestK8sRuntime()
		pod, err := rt.buildPod("default", RunConfig{
			Name:         "test-agent",
			Image:        "test:latest",
			UnixUsername: "scion",
			Volumes:      vols,
		})
		if err != nil {
			t.Fatalf("buildPod: %v", err)
		}
		var csiBuckets []string
		for _, pv := range pod.Spec.Volumes {
			if pv.CSI != nil && pv.CSI.Driver == "gcsfuse.csi.storage.gke.io" {
				csiBuckets = append(csiBuckets, pv.CSI.VolumeAttributes["bucketName"])
			}
		}
		if len(csiBuckets) != 2 || csiBuckets[0] != "b1" || csiBuckets[1] != "b2" {
			t.Fatalf("CSI buckets = %v, want [b1 b2]", csiBuckets)
		}
	})

	t.Run("cloudrun config", func(t *testing.T) {
		vols := []api.VolumeMount{
			{Type: "cloudrun-volume", VolumeName: "v", Target: "/mnt/cr"},
			{Type: "gcs", Bucket: "b", Target: "/mnt/g"},
		}
		if err := CheckVolumeSourcesForRuntime(vols, "cloudrun"); err != nil {
			t.Fatalf("CheckVolumeSourcesForRuntime(cloudrun) = %v, want nil", err)
		}
	})
}
