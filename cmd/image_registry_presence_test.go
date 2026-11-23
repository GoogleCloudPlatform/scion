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
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

// TestResolveInitImageRegistry: 'scion init --global' counts the node env as
// configured (no prompt) but never saves it.
func TestResolveInitImageRegistry(t *testing.T) {
	prompted := false
	prompt := func() string { prompted = true; return "typed.example" }

	save, env := resolveInitImageRegistry("", "", "env.example", prompt)
	assert.Equal(t, "", save, "the env value is never saved")
	assert.Equal(t, "env.example", env)
	assert.False(t, prompted, "an env value skips the prompt")

	save, _ = resolveInitImageRegistry("flag.example", "file.example", "env.example", prompt)
	assert.Equal(t, "flag.example", save)
	save, _ = resolveInitImageRegistry("", "file.example", "env.example", prompt)
	assert.Equal(t, "file.example", save)

	save, env = resolveInitImageRegistry("", "", "", prompt)
	assert.Equal(t, "typed.example", save)
	assert.Equal(t, "", env)
	assert.True(t, prompted)

	save, env = resolveInitImageRegistry("", "", "", nil)
	assert.Equal(t, "", save)
	assert.Equal(t, "", env)
}

// TestResolveMaintenanceConfig_ImageRegistryOverride: the maintenance
// registry is an override for maintenance builds only, and the settings
// file's registry is not copied into the maintenance config.
func TestResolveMaintenanceConfig_ImageRegistryOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)
	writeHomeSettingsYAML(t, home, "schema_version: \"1\"\nimage_registry: file.example/r\n")

	t.Setenv("SCION_MAINTENANCE_IMAGE_REGISTRY", "")
	assert.Equal(t, "", resolveMaintenanceConfig(&config.GlobalConfig{}).ImageRegistryOverride)

	t.Setenv("SCION_MAINTENANCE_IMAGE_REGISTRY", "maint.example/r")
	assert.Equal(t, "maint.example/r", resolveMaintenanceConfig(&config.GlobalConfig{}).ImageRegistryOverride)
}

// TestFlatInstanceImageRegistries lists this host's flat instances that set
// agent_defaults.image_registry.
func TestFlatInstanceImageRegistries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeHomeSettingsYAML(t, home, `schema_version: "1"
server:
  broker:
    instances:
      - key: docker-1
        runtime_target:
          type: docker
        agent_defaults:
          image_registry: inst.example/r
      - key: docker-2
        runtime_target:
          type: docker
`)
	assert.Equal(t, map[string]string{"docker-1": "inst.example/r"}, flatInstanceImageRegistries())
}

// writeHomeSettingsYAML writes ~/.scion/settings.yaml under home.
func writeHomeSettingsYAML(t *testing.T, home, content string) {
	t.Helper()
	dir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestImageRegistryPresenceHint_LinksReleaseBuild: the missing-registry
// hint names the release image driver and the README section on building
// images that match a release (ptone/scion#6625), next to the general
// build instructions.
func TestImageRegistryPresenceHint_LinksReleaseBuild(t *testing.T) {
	t.Setenv(config.EnvMaintenanceImageRegistry, "")
	msg := brokerRegistryMissingError().Error()
	assert.Contains(t, msg, defaultImageRegistrySuggestion)
	assert.Contains(t, msg, "See image-build/README.md")
	assert.Contains(t, msg, "image-build/README.md#build-images-for-a-release")
	assert.Contains(t, msg, "run image-build/scripts/build-release-images.sh <version>")
	assert.Contains(t, msg, "image_tag")
}

func TestPromptImageRegistryFrom(t *testing.T) {
	t.Run("bare enter accepts default public registry", func(t *testing.T) {
		var out bytes.Buffer
		got := promptImageRegistryFrom(strings.NewReader("\n"), &out, true)
		assert.Equal(t, defaultImageRegistrySuggestion, got)
		assert.Contains(t, out.String(), defaultImageRegistrySuggestion)
		assert.Contains(t, out.String(), "image-build/README.md")
		assert.Contains(t, out.String(), "localhost/scion")
	})

	t.Run("custom registry overrides default", func(t *testing.T) {
		var out bytes.Buffer
		got := promptImageRegistryFrom(strings.NewReader("us-central1-docker.pkg.dev/my-proj/scion\n"), &out, true)
		assert.Equal(t, "us-central1-docker.pkg.dev/my-proj/scion", got)
	})

	t.Run("skip leaves registry unset", func(t *testing.T) {
		for _, word := range []string{"skip\n", "SKIP\n", "  Skip  \n"} {
			var out bytes.Buffer
			got := promptImageRegistryFrom(strings.NewReader(word), &out, true)
			assert.Equal(t, "", got, "input=%q", word)
		}
	})

	t.Run("EOF without newline leaves registry unset", func(t *testing.T) {
		var out bytes.Buffer
		got := promptImageRegistryFrom(strings.NewReader(""), &out, true)
		assert.Equal(t, "", got)
	})

	t.Run("EOF with non-empty input uses entered registry", func(t *testing.T) {
		var out bytes.Buffer
		got := promptImageRegistryFrom(strings.NewReader("ghcr.io/myorg"), &out, true)
		assert.Equal(t, "ghcr.io/myorg", got)
	})

	t.Run("non-EOF read error discards partial input", func(t *testing.T) {
		var out bytes.Buffer
		got := promptImageRegistryFrom(partialErrReader{data: "ghcr.io/partial", err: errors.New("terminal read failed")}, &out, true)
		assert.Equal(t, "", got)
	})

	t.Run("non-TTY prints hints and leaves registry unset", func(t *testing.T) {
		var out bytes.Buffer
		got := promptImageRegistryFrom(strings.NewReader("\n"), &out, false)
		assert.Equal(t, "", got)
		assert.Contains(t, out.String(), defaultImageRegistrySuggestion)
		assert.NotContains(t, out.String(), "Image registry path [")
	})
}

func TestInitImageRegistryFlagHelpMentionsDefaultSuggestion(t *testing.T) {
	for _, flag := range []*cobra.Command{initCmd, projectInitCmd} {
		f := flag.Flags().Lookup("image-registry")
		if assert.NotNil(t, f, "command %s missing --image-registry flag", flag.Name()) {
			assert.Contains(t, f.Usage, defaultImageRegistrySuggestion)
		}
	}
}

type partialErrReader struct {
	data string
	err  error
}

func (r partialErrReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	return n, r.err
}
