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
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeContainerCmd struct {
	available map[string]bool
	listOut   string
	listErr   error
	inspect   map[string]struct {
		out string
		err error
	}
	calls [][]string
}

func (f *fakeContainerCmd) deps(localVersion, localCommit string) containerImagesDoctorDeps {
	return containerImagesDoctorDeps{
		lookPath: func(file string) (string, error) {
			if f.available[file] {
				return "/usr/bin/" + file, nil
			}
			return "", errors.New("not found")
		},
		runCmd: func(_ context.Context, name string, args ...string) ([]byte, error) {
			call := append([]string{name}, args...)
			f.calls = append(f.calls, call)
			if len(args) > 0 && args[0] == "images" {
				return []byte(f.listOut), f.listErr
			}
			if len(args) >= 5 && args[0] == "image" && args[1] == "inspect" {
				img := args[len(args)-1]
				if resp, ok := f.inspect[img]; ok {
					return []byte(resp.out), resp.err
				}
				return nil, errors.New("Error: No such image: " + img)
			}
			return nil, errors.New("unexpected command: " + strings.Join(call, " "))
		},
		localVersion: localVersion,
		localCommit:  localCommit,
	}
}

func TestCheckDoctorContainerImages_AppleContainerSkipped(t *testing.T) {
	for _, rt := range []string{"container", "apple-container"} {
		t.Run(rt, func(t *testing.T) {
			fake := &fakeContainerCmd{available: map[string]bool{"docker": true}}
			res := checkDoctorContainerImages(rt, []string{"scion-claude:latest"}, fake.deps("v1.0.0", "abcdef123456"))
			require.Len(t, res, 1)
			assert.Equal(t, "container-images", res[0].Name)
			assert.Equal(t, "skip", res[0].Status)
			assert.Contains(t, res[0].Message, "Apple Container")
			assert.Empty(t, fake.calls, "must not invoke docker/podman for Apple Container")
		})
	}
}

func TestCheckDoctorContainerImages_EngineMissing(t *testing.T) {
	t.Run("no runtime specified and neither docker nor podman in PATH", func(t *testing.T) {
		fake := &fakeContainerCmd{available: map[string]bool{}}
		res := checkDoctorContainerImages("", []string{"scion-claude:latest"}, fake.deps("v1.0.0", "abcdef123456"))
		require.Len(t, res, 1)
		assert.Equal(t, "container-images", res[0].Name)
		assert.Equal(t, "skip", res[0].Status)
		assert.Contains(t, res[0].Message, "No container runtime CLI found")
	})

	t.Run("configured docker missing even if podman exists", func(t *testing.T) {
		fake := &fakeContainerCmd{available: map[string]bool{"podman": true}}
		res := checkDoctorContainerImages("docker", []string{"scion-claude:latest"}, fake.deps("v1.0.0", "abcdef123456"))
		require.Len(t, res, 1)
		assert.Equal(t, "container-images", res[0].Name)
		assert.Equal(t, "skip", res[0].Status)
		assert.Contains(t, res[0].Message, `"docker" not found`)
	})
}

func TestCheckDoctorContainerImages_ListErrorAndNoScionImages(t *testing.T) {
	t.Run("list error skips", func(t *testing.T) {
		fake := &fakeContainerCmd{
			available: map[string]bool{"docker": true},
			listErr:   errors.New("daemon not running"),
		}
		res := checkDoctorContainerImages("docker", []string{"scion-claude:latest"}, fake.deps("v1.0.0", "abcdef123456"))
		require.Len(t, res, 1)
		assert.Equal(t, "skip", res[0].Status)
		assert.Contains(t, res[0].Message, "Could not list docker images")
	})

	t.Run("no scion images warns", func(t *testing.T) {
		fake := &fakeContainerCmd{
			available: map[string]bool{"docker": true},
			listOut:   "alpine:latest\nubuntu:24.04\n",
		}
		res := checkDoctorContainerImages("docker", []string{"scion-claude:latest"}, fake.deps("v1.0.0", "abcdef123456"))
		require.Len(t, res, 1)
		assert.Equal(t, "warn", res[0].Status)
		assert.Contains(t, res[0].Message, "No scion container images found via docker")
	})
}

func TestCheckDoctorContainerImages_MatchingImageUpToDate(t *testing.T) {
	fake := &fakeContainerCmd{
		available: map[string]bool{"docker": true},
		listOut:   "scion-claude:latest\nalpine:latest\n",
		inspect: map[string]struct {
			out string
			err error
		}{
			"scion-claude:latest": {
				out: `{"scion.sciontool.capabilities":"netns-setup","org.opencontainers.image.version":"v0.5.0","org.opencontainers.image.revision":"501fea05e44caa831cc60da07c9ec08ad39769b7"}`,
			},
		},
	}

	res := checkDoctorContainerImages("docker", []string{"scion-claude:latest", "scion-gemini:latest"}, fake.deps("v0.5.0", "501fea05"))
	require.Len(t, res, 1, "up-to-date local image produces only the pass summary; absent scion-gemini is skipped")
	assert.Equal(t, "container-images", res[0].Name)
	assert.Equal(t, "pass", res[0].Status)
	assert.Contains(t, res[0].Message, "1 scion image(s) found via docker")
	// Only scion-claude:latest was inspected, not absent scion-gemini:latest.
	require.Len(t, fake.calls, 2)
	assert.Equal(t, []string{"docker", "image", "inspect", "--format", "{{json .Config.Labels}}", "scion-claude:latest"}, fake.calls[1])
}

func TestCheckDoctorContainerImages_CapabilityAndVersionSkew(t *testing.T) {
	fake := &fakeContainerCmd{
		available: map[string]bool{"docker": true},
		listOut:   "scion-claude:latest\nscion-gemini:latest\nscion-codex:latest\n",
		inspect: map[string]struct {
			out string
			err error
		}{
			// Missing labels altogether (Docker prints null).
			"scion-claude:latest": {
				out: "null\n",
			},
			// Capabilities label present but lacks netns-setup; version and revision differ.
			"scion-gemini:latest": {
				out: `{"scion.sciontool.capabilities":"other-cap","org.opencontainers.image.version":"v0.4.0","org.opencontainers.image.revision":"1111111111111111111111111111111111111111"}`,
			},
			// Has netns-setup in a comma-separated list, but version differs.
			"scion-codex:latest": {
				out: `{"scion.sciontool.capabilities":"foo, netns-setup, bar","org.opencontainers.image.version":"v0.4.0","org.opencontainers.image.revision":"2222222222222222222222222222222222222222"}`,
			},
		},
	}

	res := checkDoctorContainerImages(
		"docker",
		[]string{"scion-claude:latest", "scion-gemini:latest", "scion-codex:latest"},
		fake.deps("v0.5.0", "2222222222222222222222222222222222222222"),
	)

	// Expected order (sorted matched images):
	// 0: container-images (pass)
	// 1: container-image-capabilities[scion-claude:latest] (warn, missing label; no version info because OCI labels missing)
	// 2: container-image-version[scion-codex:latest] (info, version differs, revision matches)
	// 3: container-image-capabilities[scion-gemini:latest] (warn, label lacks netns-setup)
	// 4: container-image-version[scion-gemini:latest] (info, both version and revision differ)
	require.Len(t, res, 5)

	assert.Equal(t, "container-images", res[0].Name)
	assert.Equal(t, "pass", res[0].Status)

	assert.Equal(t, "container-image-capabilities[scion-claude:latest]", res[1].Name)
	assert.Equal(t, "warn", res[1].Status)
	assert.Contains(t, res[1].Message, "missing the scion.sciontool.capabilities label")
	assert.Contains(t, res[1].Message, "unlabeled-image fallback")
	assert.NotEmpty(t, res[1].Remediation)

	assert.Equal(t, "container-image-version[scion-codex:latest]", res[2].Name)
	assert.Equal(t, "info", res[2].Status)
	assert.Contains(t, res[2].Message, "version v0.4.0 != local v0.5.0")
	assert.NotContains(t, res[2].Message, "revision")

	assert.Equal(t, "container-image-capabilities[scion-gemini:latest]", res[3].Name)
	assert.Equal(t, "warn", res[3].Status)
	assert.Contains(t, res[3].Message, `scion.sciontool.capabilities="other-cap" lacks "netns-setup"`)

	assert.Equal(t, "container-image-version[scion-gemini:latest]", res[4].Name)
	assert.Equal(t, "info", res[4].Status)
	assert.Contains(t, res[4].Message, "version v0.4.0 != local v0.5.0")
	assert.Contains(t, res[4].Message, "revision 111111111111 != local 222222222222")
}

func TestCheckDoctorContainerImages_DevBuildSkipsVersionInfo(t *testing.T) {
	tests := []struct {
		name         string
		localVersion string
		localCommit  string
	}{
		{"empty version and empty commit", "", ""},
		{"empty version and unknown commit", "", "unknown"},
		{"dev version with commit", "dev", "abcdef123456"},
		{"unknown version with commit", "unknown", "abcdef123456"},
		{"devel version with commit", "(devel)", "abcdef123456"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeContainerCmd{
				available: map[string]bool{"docker": true},
				listOut:   "scion-claude:latest\n",
				inspect: map[string]struct {
					out string
					err error
				}{
					"scion-claude:latest": {
						out: `{"scion.sciontool.capabilities":"netns-setup","org.opencontainers.image.version":"v0.5.0","org.opencontainers.image.revision":"111111111111"}`,
					},
				},
			}
			res := checkDoctorContainerImages("docker", []string{"scion-claude:latest"}, fake.deps(tt.localVersion, tt.localCommit))
			require.Len(t, res, 1, "dev build must skip version/revision info check")
			assert.Equal(t, "pass", res[0].Status)
		})
	}
}

func TestCheckDoctorContainerImages_PodmanLocalhostMatchingAndInspectFailure(t *testing.T) {
	fake := &fakeContainerCmd{
		available: map[string]bool{"docker": true, "podman": true},
		listOut:   "localhost/scion-claude:latest\nlocalhost/scion-gemini:latest\n",
		inspect: map[string]struct {
			out string
			err error
		}{
			"localhost/scion-claude:latest": {
				out: `{}`,
			},
			"localhost/scion-gemini:latest": {
				err: errors.New("Error: no such image"),
			},
		},
	}

	res := checkDoctorContainerImages("podman", []string{"scion-claude", "scion-gemini:latest"}, fake.deps("v0.5.0", "abcdef123456"))
	require.Len(t, res, 2)
	assert.Equal(t, "pass", res[0].Status)
	assert.Contains(t, res[0].Message, "via podman")
	assert.Equal(t, "container-image-capabilities[localhost/scion-claude:latest]", res[1].Name)
	assert.Equal(t, "warn", res[1].Status)
	// Confirm podman was invoked rather than docker.
	assert.Equal(t, "podman", fake.calls[0][0])
}

func TestDoctorConfiguredHarnessImages_RewritesAndOverrides(t *testing.T) {
	t.Setenv("SCION_IMAGE_REGISTRY", "")
	t.Setenv("SCION_IMAGE_TAG", "")

	vs := &config.VersionedSettings{
		ActiveProfile: "local",
		ImageRegistry: "ghcr.io/example",
		HarnessConfigs: map[string]config.HarnessConfigEntry{
			"claude": {Image: "scion-claude:latest"},
			"gemini": {Image: "scion-gemini:latest"},
		},
		Profiles: map[string]config.V1ProfileConfig{
			"local": {
				Runtime: "docker",
				HarnessOverrides: map[string]config.V1HarnessOverride{
					"gemini": {Image: "custom.io/team/scion-gemini:v2"},
				},
			},
		},
	}

	t.Setenv("HOME", t.TempDir())
	got := doctorConfiguredHarnessImages("", vs, "", "v1.2.3")
	assert.Contains(t, got, "scion-claude:latest")
	assert.Contains(t, got, "scion-claude:v1.2.3")
	assert.Contains(t, got, "ghcr.io/example/scion-claude:latest")
	assert.Contains(t, got, "ghcr.io/example/scion-claude:v1.2.3")
	assert.Contains(t, got, "custom.io/team/scion-gemini:v2")
}

func TestDoctorConfiguredHarnessImages_OnDiskBaseDirAndInspection(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("SCION_IMAGE_REGISTRY", "")
	t.Setenv("SCION_IMAGE_TAG", "")

	claudeDir := filepath.Join(tmpHome, ".scion", "harness-configs", "claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(claudeDir, "config.yaml"),
		[]byte("harness: claude\nimage: scion-claude:latest\n"),
		0o644,
	))

	// Default settings have empty harness_configs and harness_overrides.
	vs := &config.VersionedSettings{ActiveProfile: "local"}
	configured := doctorConfiguredHarnessImages("", vs, "", "v1.0.0")
	require.Contains(t, configured, "scion-claude:latest")

	fake := &fakeContainerCmd{
		available: map[string]bool{"docker": true},
		listOut:   "scion-claude:latest\n",
		inspect: map[string]struct {
			out string
			err error
		}{
			"scion-claude:latest": {
				out: `{"org.opencontainers.image.version":"v0.9.0"}`,
			},
		},
	}

	res := checkDoctorContainerImages("docker", configured, fake.deps("v1.0.0", "abcdef123456"))
	require.Len(t, res, 3)
	assert.Equal(t, "container-images", res[0].Name)
	assert.Equal(t, "pass", res[0].Status)
	assert.Equal(t, "container-image-capabilities[scion-claude:latest]", res[1].Name)
	assert.Equal(t, "warn", res[1].Status)
	assert.Equal(t, "container-image-version[scion-claude:latest]", res[2].Name)
	assert.Equal(t, "info", res[2].Status)
	require.Len(t, fake.calls, 2)
	assert.Equal(t, []string{"docker", "image", "inspect", "--format", "{{json .Config.Labels}}", "scion-claude:latest"}, fake.calls[1])
}

func TestCheckDoctorContainerImages_NonLocalRuntimesSkipSkewChecks(t *testing.T) {
	for _, rt := range []string{"kubernetes", "k8s", "cloudrun", "cloudrun-flat"} {
		t.Run(rt, func(t *testing.T) {
			fake := &fakeContainerCmd{
				available: map[string]bool{"docker": true},
				listOut:   "scion-claude:latest\n",
				inspect: map[string]struct {
					out string
					err error
				}{
					"scion-claude:latest": {
						out: `{}`,
					},
				},
			}
			res := checkDoctorContainerImages(rt, []string{"scion-claude:latest"}, fake.deps("v1.0.0", "abcdef123456"))
			require.Len(t, res, 1, "non-local runtime must skip per-image skew checks")
			assert.Equal(t, "container-images", res[0].Name)
			assert.Equal(t, "pass", res[0].Status)
			assert.Len(t, fake.calls, 1, "must not call image inspect for non-local runtime")
		})
	}
}

func TestCheckDoctorContainerImages_OffTagBuildComparesRevision(t *testing.T) {
	fake := &fakeContainerCmd{
		available: map[string]bool{"docker": true},
		listOut:   "scion-claude:latest\n",
		inspect: map[string]struct {
			out string
			err error
		}{
			"scion-claude:latest": {
				out: `{"scion.sciontool.capabilities":"netns-setup","org.opencontainers.image.version":"","org.opencontainers.image.revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
			},
		},
	}

	res := checkDoctorContainerImages("docker", []string{"scion-claude:latest"}, fake.deps("", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
	require.Len(t, res, 2)
	assert.Equal(t, "pass", res[0].Status)
	assert.Equal(t, "container-image-version[scion-claude:latest]", res[1].Name)
	assert.Equal(t, "info", res[1].Status)
	assert.Contains(t, res[1].Message, "revision aaaaaaaaaaaa != local bbbbbbbbbbbb")
}
