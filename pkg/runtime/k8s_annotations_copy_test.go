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
	"encoding/base64"
	"maps"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Tests for ptone/scion#5970: Run and buildPod add their annotations to a
// copy of RunConfig.Annotations, never to the caller's map, and the pod's
// annotations are the same as when they wrote into the caller's map.

// testGCSVolumesAnnotation is the scion.gcs_volumes value buildPod records
// for a single gcs volume of bucket b1 mounted at /data.
var testGCSVolumesAnnotation = base64.StdEncoding.EncodeToString(
	[]byte(`[{"source":"","target":"/data","bucket":"b1","prefix":""}]`))

func assertAnnotations(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	if !maps.Equal(got, want) {
		t.Errorf("%s:\n got  %v\n want %v", what, got, want)
	}
	if (got == nil) != (want == nil) {
		t.Errorf("%s: got nil=%v, want nil=%v", what, got == nil, want == nil)
	}
}

func TestK8sRun_DoesNotModifyCallerAnnotations(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	callerAnnotations := map[string]string{"user.key": "v"}
	config := hookTestConfig("copy-agent")
	config.Annotations = callerAnnotations
	config.GitClone = &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	config.Volumes = []api.VolumeMount{{Type: "gcs", Bucket: "b1", Target: "/data"}}

	pod := runUntilPodSubmitted(t, rt, clientset, config)

	assertAnnotations(t, "caller annotations after Run", callerAnnotations,
		map[string]string{"user.key": "v"})
	assertAnnotations(t, "pod annotations", pod.Annotations, map[string]string{
		"user.key":            "v",
		"scion.git_clone":     "true",
		"scion.git_clone_url": "https://example.com/repo.git",
		"scion.namespace":     "default",
		"gke-gcsfuse/volumes": "true",
		"scion.gcs_volumes":   testGCSVolumesAnnotation,
	})
}

func TestBuildPod_DoesNotModifyCallerAnnotations(t *testing.T) {
	gcs := []api.VolumeMount{{Type: "gcs", Bucket: "b1", Target: "/data"}}
	tests := []struct {
		name        string
		annotations map[string]string
		volumes     []api.VolumeMount
		wantPod     map[string]string
	}{
		{
			name:        "caller map with gcs volume",
			annotations: map[string]string{"user.key": "v"},
			volumes:     gcs,
			wantPod: map[string]string{
				"user.key":            "v",
				"gke-gcsfuse/volumes": "true",
				"scion.gcs_volumes":   testGCSVolumesAnnotation,
			},
		},
		{
			name:        "caller map without volumes",
			annotations: map[string]string{"user.key": "v"},
			wantPod:     map[string]string{"user.key": "v"},
		},
		{
			name:        "empty map with gcs volume",
			annotations: map[string]string{},
			volumes:     gcs,
			wantPod: map[string]string{
				"gke-gcsfuse/volumes": "true",
				"scion.gcs_volumes":   testGCSVolumesAnnotation,
			},
		},
		{
			name:    "nil map with gcs volume",
			volumes: gcs,
			wantPod: map[string]string{
				"gke-gcsfuse/volumes": "true",
				"scion.gcs_volumes":   testGCSVolumesAnnotation,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt, _, _ := newTestK8sRuntime()
			want := maps.Clone(tc.annotations)
			config := RunConfig{
				Name:         "copy-agent",
				Image:        "test:latest",
				UnixUsername: "scion",
				Annotations:  tc.annotations,
				Volumes:      tc.volumes,
			}

			pod, err := rt.buildPod("default", config)
			if err != nil {
				t.Fatalf("buildPod: %v", err)
			}

			assertAnnotations(t, "pod annotations", pod.Annotations, tc.wantPod)
			assertAnnotations(t, "caller annotations after buildPod", tc.annotations, want)

			// The pod's map must be its own: a later write to it must not
			// reach the caller's map.
			if pod.Annotations != nil {
				pod.Annotations["later.key"] = "x"
			}
			assertAnnotations(t, "caller annotations after a write to the pod map", tc.annotations, want)
		})
	}
}
