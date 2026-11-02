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
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

var updateVolumeArgsGolden = flag.Bool("update-volume-args-golden", false, "rewrite testdata/user_volumes_golden from the current run argv")

// userVolumeGoldenHome is $HOME for the golden runs, so a "~/" source
// expands the same on every machine. It does not exist.
const userVolumeGoldenHome = "/golden-home"

// userVolumeCases are the `volumes:` shapes whose docker, podman and apple
// run argv is pinned byte for byte (ptone/scion#5837): realizing the entries
// as Owner=user Mounts through projectfs.Layout.Realize must not change them.
// The testdata was captured before that change.
//
// A refused case pins the refusal instead: Run must fail, and its error text
// is the golden.
func userVolumeCases() []struct {
	name    string
	refused bool
	volumes []api.VolumeMount
} {
	return []struct {
		name    string
		refused bool
		volumes []api.VolumeMount
	}{
		{"local", false, []api.VolumeMount{
			{Source: "/host/data", Target: "/data"},
			{Source: "/host/ref", Target: "/ref", ReadOnly: true},
			{Type: "local", Source: "/host/typed", Target: "/typed"},
		}},
		{"expanded", false, []api.VolumeMount{
			{Source: "~/cache", Target: "~/.cache/tool"},
			{Source: "~/ro", Target: "/host-home", ReadOnly: true},
			{Source: "${GOLDEN_VOLUME_SRC}/sub", Target: "/env"},
		}},
		{"dedup", false, []api.VolumeMount{
			{Source: "/host/one", Target: "/same", ReadOnly: true},
			{Source: "/host/two", Target: "/same"},
			{Source: "/host/three", Target: "/other"},
		}},
		{"workdir", false, []api.VolumeMount{
			{Source: "/host/src", Target: "/workspace"},
		}},
		{"gcs-and-local", false, []api.VolumeMount{
			{Type: "gcs", Bucket: "bucket-a", Target: "/gcs-a"},
			{Type: "gcs", Bucket: "bucket-b", Prefix: "pre/fix", Mode: "ro", Target: "~/gcs-b", ReadOnly: true},
			{Source: "/host/data", Target: "/data"},
		}},
		{"other-types", false, []api.VolumeMount{
			{Type: "nfs", Server: "10.0.0.2", Source: "/export/x", Target: "/nfs"},
			{Type: "cloudrun-volume", VolumeName: "vol", Source: "/cr", Target: "/cr"},
		}},
		// The host home holds the Scion global directory: the source is
		// refused before any run.
		{"refused", true, []api.VolumeMount{
			{Source: "~", Target: "/host-home", ReadOnly: true},
		}},
	}
}

// hostIDEnv matches the host uid/gid env values, which come from the test
// process.
var hostIDEnv = regexp.MustCompile(`(?m)^(SCION_HOST_UID|SCION_HOST_GID|SCION_KEEPID_UID|SCION_KEEPID_GID)=\d+$`)

// TestUserVolumes_RunArgsGolden pins the full docker, podman and apple run
// argv of each user-volume case.
func TestUserVolumes_RunArgsGolden(t *testing.T) {
	t.Setenv("HOME", userVolumeGoldenHome)
	t.Setenv("GOLDEN_VOLUME_SRC", "/golden-env")
	t.Setenv("SCION_DEV_BINARIES", "")
	t.Setenv(ForceHostNetworkEnvVar, "")
	origPC := projectConfigsDirFunc
	projectConfigsDirFunc = func() (string, error) {
		return filepath.Join(userVolumeGoldenHome, ".scion", "project-configs"), nil
	}
	t.Cleanup(func() { projectConfigsDirFunc = origPC })

	// The mock CLI prints the run argv one argument per line; Run returns
	// it trimmed.
	mock := filepath.Join(t.TempDir(), "mock-cli")
	script := "#!/bin/sh\nif [ \"$1\" = \"run\" ]; then printf '%s\\n' \"$@\"; fi\n"
	if err := os.WriteFile(mock, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runtimes := []struct {
		name string
		rt   Runtime
	}{
		{"docker", &DockerRuntime{Command: mock}},
		{"podman", &PodmanRuntime{Command: mock}},
		{"apple", &AppleContainerRuntime{Command: mock}},
	}
	for _, rc := range runtimes {
		for _, tc := range userVolumeCases() {
			t.Run(rc.name+"/"+tc.name, func(t *testing.T) {
				cfg := RunConfig{
					Harness:      &harness.Generic{},
					Name:         "test-agent",
					UnixUsername: "scion",
					Image:        "scion-agent:latest",
					Task:         "hello",
					Volumes:      tc.volumes,
				}
				out, err := rc.rt.Run(context.Background(), cfg)
				var got []byte
				switch {
				case tc.refused && err == nil:
					t.Fatalf("Run succeeded, want a refusal:\n%s", out)
				case tc.refused:
					got = []byte("refused: " + err.Error() + "\n")
				case err != nil:
					t.Fatalf("Run: %v", err)
				default:
					got = append(hostIDEnv.ReplaceAll([]byte(out), []byte("${1}=HOST_ID")), '\n')
				}
				file := filepath.Join("testdata", "user_volumes_golden", rc.name+"-"+tc.name+".txt")
				if *updateVolumeArgsGolden {
					if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(file, got, 0o644); err != nil {
						t.Fatal(err)
					}
					return
				}
				want, err := os.ReadFile(file)
				if err != nil {
					t.Fatalf("read golden: %v", err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("run argv for %s differs from %s:\n%s", tc.name, file, got)
				}
			})
		}
	}
}
