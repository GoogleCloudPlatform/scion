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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
// have no setup wizard, so it does not mention one.
func brokerRegistryMissingError() error {
	msg := "image_registry is not configured, but the runtime broker requires it.\n\n" +
		"The runtime broker pulls container images to run agents. Without a registry,\n" +
		"image pulls will fail. To fix this:\n\n" +
		"  Option 1: scion config set --global image_registry <your-registry>\n" +
		"  Option 2: export SCION_IMAGE_REGISTRY=<your-registry>\n\n"
	if maintenanceOnlyRegistrySet() {
		msg += maintenanceRegistryOnlyNote + ".\n\n"
	}
	msg += "See image-build/README.md for instructions on building and pushing images"
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

// workstationRegistryDegradeWarning is the warning a workstation server logs
// when it starts without its co-located broker for lack of a registry.
func workstationRegistryDegradeWarning() string {
	msg := "image_registry is not configured; starting without the runtime broker. Set the registry in the setup wizard or with 'scion config set --global image_registry <registry>', then run 'scion server restart'."
	if maintenanceOnlyRegistrySet() {
		msg += " " + maintenanceRegistryOnlyNote + "."
	}
	return msg
}

// disableWorkstationBrokerForRegistry starts a workstation server without
// its co-located broker for lack of an image registry, and says so.
func disableWorkstationBrokerForRegistry(cfg *config.GlobalConfig) {
	slog.Warn(workstationRegistryDegradeWarning())
	fmt.Fprintln(os.Stderr, "Warning: "+brokerSkippedNotice)
	cfg.RuntimeBroker.Enabled = false
}

// managedImageRegistryConfigured reports whether the hub database holds a
// saved image_registry the co-located broker resolves with: a managed
// endpoints value, or a managed value of the broker's default profile
// (config.EffectiveDefaultProfile over activeProfile and the stored
// profile names). It runs before the settings sync that backfills row
// origins, so managed-ness uses opsettings.EffectiveOrigin. A read error
// counts as not configured.
func managedImageRegistryConfigured(ctx context.Context, s store.HubSettingStore, activeProfile string) bool {
	if s == nil {
		return false
	}
	managed := func(section string) (json.RawMessage, bool) {
		row, err := s.GetHubSetting(ctx, section)
		if err != nil || row == nil {
			return nil, false
		}
		updatedBy := row.UpdatedBy
		if opsettings.EffectiveOrigin(section, row.Origin, &updatedBy) != opsettings.OriginManaged {
			return nil, false
		}
		return row.Value, true
	}
	if doc, ok := managed("endpoints"); ok {
		var ep struct {
			ImageRegistry string `json:"image_registry"`
		}
		if json.Unmarshal(doc, &ep) == nil && strings.TrimSpace(ep.ImageRegistry) != "" {
			return true
		}
	}
	if doc, ok := managed("profiles"); ok {
		var profiles map[string]struct {
			ImageRegistry string `json:"image_registry"`
		}
		if json.Unmarshal(doc, &profiles) == nil && len(profiles) > 0 {
			names := make([]string, 0, len(profiles))
			for name := range profiles {
				names = append(names, name)
			}
			sort.Strings(names)
			p := config.EffectiveDefaultProfile(activeProfile, names)
			if strings.TrimSpace(profiles[p].ImageRegistry) != "" {
				return true
			}
		}
	}
	return false
}

// bootstrapImageRegistryConfigured reports whether the hub's file tier
// (its bootstrap merge for configPath: defaults < SCION_SEED_* <
// settings.yaml < SCION_SERVER_*) gives the co-located broker a registry:
// the top-level value, or the value of activeProfile. The co-located
// broker resolves with this tier through the settings overlay, so a
// registry set only through SCION_SERVER_IMAGEREGISTRY or
// SCION_SEED_IMAGEREGISTRY counts.
func bootstrapImageRegistryConfigured(configPath, activeProfile string) bool {
	tier := config.ImageRegistryFileTierFromKoanf(config.LoadBootstrapKoanfWithConfigPath(configPath))
	return tier.Top != "" || (activeProfile != "" && tier.Profiles[activeProfile] != "")
}

// colocatedActiveProfile returns active_profile as the co-located broker
// loads it for an agent with no project (the global settings file plus
// SCION_ACTIVE_PROFILE; "" when the settings cannot be loaded). Like the
// server's other startup loads it never merges a project found from the
// working directory (ptone/scion#5362).
func colocatedActiveProfile() string {
	vs, _, err := loadServerEffectiveSettings()
	if err != nil || vs == nil {
		return ""
	}
	return vs.ActiveProfile
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
