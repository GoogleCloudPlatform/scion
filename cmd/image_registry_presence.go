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
	"errors"
	"log/slog"
	"os"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// maintenanceRegistryOnlyNote is appended to a missing-registry message when
// SCION_MAINTENANCE_IMAGE_REGISTRY is set: it no longer supplies the agent
// image registry (ptone/scion#3916).
const maintenanceRegistryOnlyNote = config.EnvMaintenanceImageRegistry + " now applies only to maintenance builds; set " +
	config.EnvImageRegistry + " or image_registry in settings.yaml"

// imageRegistryConfiguredEnvOrFile reports whether the broker has an image
// registry from its node env (SCION_IMAGE_REGISTRY) or its settings file
// (resolved for projectPath, see config.LoadEffectiveSettings, for the
// active profile). SCION_MAINTENANCE_IMAGE_REGISTRY is not a source.
func imageRegistryConfiguredEnvOrFile(projectPath string) bool {
	if config.ImageRegistryFromEnv() != "" {
		return true
	}
	vs, _, err := config.LoadEffectiveSettings(projectPath)
	if err != nil {
		slog.Debug("Failed to load settings while checking image registry", "error", err)
		return false
	}
	return vs != nil && vs.SettingsImageRegistry("") != ""
}

// maintenanceOnlyRegistrySet reports whether SCION_MAINTENANCE_IMAGE_REGISTRY
// is set, so a missing-registry message names its replacement.
func maintenanceOnlyRegistrySet() bool {
	return strings.TrimSpace(os.Getenv(config.EnvMaintenanceImageRegistry)) != ""
}

// brokerRegistryMissingError is the error of a broker start with no image
// registry. It is used by the standalone broker and by hosted servers, which
// have no setup wizard, so it does not mention one. It names both the
// setting and the env var, and how to set the env var for an interactive
// run (an export) and for a service (its environment file), where an export
// in a shell has no effect (ptone/scion#7183).
func brokerRegistryMissingError() error {
	msg := "image_registry is not configured, but the runtime broker requires it.\n\n" +
		"The runtime broker pulls container images to run agents. Without a registry,\n" +
		"image pulls will fail. To fix this, set one of:\n\n" +
		"  Option 1: image_registry in settings.yaml, for example with\n" +
		"            scion config set --global image_registry <your-registry>\n" +
		"  Option 2: the " + config.EnvImageRegistry + " environment variable: in a shell,\n" +
		"            export " + config.EnvImageRegistry + "=<your-registry>; for a service, add\n" +
		"            " + config.EnvImageRegistry + "=<your-registry> to its environment file\n" +
		"            (such as hub.env for a hub run by systemd)\n\n" +
		"For images in this host's local image store, use localhost/scion.\n" +
		"Then start or restart the server or broker.\n\n"
	if maintenanceOnlyRegistrySet() {
		msg += maintenanceRegistryOnlyNote + ".\n\n"
	}
	msg += "See image-build/README.md for instructions on building and pushing images.\n" +
		"For images that match a release, run image-build/scripts/build-release-images.sh <version>\n" +
		"(image-build/README.md#build-images-for-a-release); a release build of scion pulls the images\n" +
		"tagged with its own version unless image_tag is set (image_tag: latest opts out)"
	return errors.New(msg)
}

// requireImageRegistryForBrokerAt is requireImageRegistryForBroker with the
// settings resolved for projectPath (see config.LoadEffectiveSettings)
// instead of the project found from the working directory.
func requireImageRegistryForBrokerAt(projectPath string) error {
	if imageRegistryConfiguredEnvOrFile(projectPath) {
		return nil
	}
	return brokerRegistryMissingError()
}

// flatInstanceImageRegistries returns the agent_defaults.image_registry of
// each flat Runtime Broker instance configured on this host, keyed by
// instance, for the startup warning about values a saved hub value beats.
func flatInstanceImageRegistries() map[string]string {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return nil
	}
	vs, err := config.LoadVersionedSettings(globalDir)
	if err != nil || vs == nil || vs.Server == nil || vs.Server.Broker == nil {
		return nil
	}
	out := map[string]string{}
	for _, inst := range vs.Server.Broker.Instances {
		if inst.AgentDefaults != nil && strings.TrimSpace(inst.AgentDefaults.ImageRegistry) != "" {
			out[inst.Key] = inst.AgentDefaults.ImageRegistry
		}
	}
	return out
}
