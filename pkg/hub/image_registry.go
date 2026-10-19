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

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// image_registry resolution on the hub (ptone/scion#3916). Every hub read
// resolves through config.ResolveImageRegistryLayers with the layers built
// here:
//
//	node env > hub DB profile > hub DB top > file profile > file top > default
//
// The hub DB layers are values of managed rows only; the file layers are
// this replica's bootstrap merge (config.ImageRegistryFileTier).

// managedImageRegistries returns the image_registry values of the managed
// endpoints and profiles rows in the cache: the top-level value and each
// profile's non-empty value. A seeded row, or no row, contributes nothing.
func (o *OperationalSettings) managedImageRegistries() (top string, profiles map[string]string) {
	o.mu.RLock()
	endpoints, hasEndpoints := o.cache["endpoints"]
	prof, hasProfiles := o.cache["profiles"]
	o.mu.RUnlock()

	if hasEndpoints && endpoints.Origin == opsettings.OriginManaged && !endpoints.Malformed {
		var doc struct {
			ImageRegistry string `json:"image_registry"`
		}
		if json.Unmarshal(endpoints.Value, &doc) == nil {
			top = strings.TrimSpace(doc.ImageRegistry)
		}
	}
	if hasProfiles && prof.Origin == opsettings.OriginManaged && !prof.Malformed {
		var doc map[string]struct {
			ImageRegistry string `json:"image_registry"`
		}
		if json.Unmarshal(prof.Value, &doc) == nil {
			for name, p := range doc {
				if v := strings.TrimSpace(p.ImageRegistry); v != "" {
					if profiles == nil {
						profiles = map[string]string{}
					}
					profiles[name] = v
				}
			}
		}
	}
	return top, profiles
}

// imageRegistryFileTier returns this replica's file tier of image_registry
// (its bootstrap merge: defaults < SCION_SEED_* < settings.yaml <
// SCION_SERVER_*).
func (o *OperationalSettings) imageRegistryFileTier() config.ImageRegistryFileTier {
	return config.ImageRegistryFileTierFromKoanf(o.bootstrapKoanf)
}

// ImageRegistryHubInputs returns the hub-side image_registry layers: the
// managed values, the file tier and the profile catalog in effect (the
// names the co-located broker's profiles map resolves against). The hub
// resolves with these and feeds the same values to the co-located broker's
// settings overlay, so both sides agree.
func (o *OperationalSettings) ImageRegistryHubInputs() config.ImageRegistryHubInputs {
	top, profiles := o.managedImageRegistries()
	catalog := make([]string, 0)
	for name := range o.Snapshot().Profiles {
		catalog = append(catalog, name)
	}
	sort.Strings(catalog)
	return config.ImageRegistryHubInputs{
		ManagedTop:      top,
		ManagedProfiles: profiles,
		FileTier:        o.imageRegistryFileTier(),
		Catalog:         catalog,
	}
}

// shownImageRegistry is the top-level image_registry the admin settings
// GET shows, and the value a save compares a request with: the managed
// endpoints value when the row is managed and holds one, else this
// replica's file tier, always. It never shows a node env, and never the
// value of a seeded row, which under HA holds the bootstrap of whichever
// replica re-synced it last.
func shownImageRegistry(ops *OperationalSettings) string {
	if ops == nil {
		return ""
	}
	if top, _ := ops.managedImageRegistries(); top != "" {
		return top
	}
	return ops.imageRegistryFileTier().Top
}

// fileImageRegistryWithoutOps is the file top-level image_registry for a
// server without operational settings (tests and embedded servers): the
// global settings file.
func fileImageRegistryWithoutOps() string {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return ""
	}
	vs, err := config.LoadVersionedSettings(globalDir)
	if err != nil || vs == nil {
		return ""
	}
	return vs.ImageRegistry
}

// hubScopeImageRegistry resolves image_registry for a read with no target
// broker (setup status, image pull, harness image status, maintenance
// builds): the hub process's own SCION_IMAGE_REGISTRY as the node env, the
// managed top value, then the file top. Profile layers do not apply.
func (s *Server) hubScopeImageRegistry() config.ResolvedImageRegistry {
	s.mu.RLock()
	nodeEnv := s.config.NodeImageRegistry
	s.mu.RUnlock()
	ops := s.GetOperationalSettings()
	if ops == nil {
		return config.ResolveImageRegistryLayers(config.ImageRegistryLayers{NodeEnv: nodeEnv, File: fileImageRegistryWithoutOps()})
	}
	return config.ResolveImageRegistryLayers(ops.ImageRegistryHubInputs().TopLayers(nodeEnv))
}

// resolveImageRegistry is the hub-scope image_registry value.
func (s *Server) resolveImageRegistry() string {
	return s.hubScopeImageRegistry().Value
}

// imageRegistryLookupTimeout bounds the broker read of a dispatch-time
// image_registry resolution.
const imageRegistryLookupTimeout = 5 * time.Second

// dispatchImageRegistryFor returns the registry the hub rewrites an
// agent's explicit image to when dispatching it to brokerID with profile
// ("" = no saved profile), or "" for no rewrite. The node env is only that
// broker's reported value; the hub's own env never applies to a broker.
//
//   - A flat target is not rewritten: the broker rewrites the bare image
//     with its own layers, so explicit and template images agree.
//   - Nor is an agent with no saved profile on the co-located broker: the
//     broker selects its default profile from its own live settings
//     (SCION_ACTIVE_PROFILE, project settings, the overlay), which the hub
//     does not copy, so the broker rewrites.
//   - For any other broker, an empty profile resolves to the default
//     profile it registered.
//   - A broker row that cannot be read (other than not found) is not
//     rewritten either: the broker rewrites with its own layers.
func (s *Server) dispatchImageRegistryFor(brokerID, profile string) string {
	r, ok := s.dispatchImageRegistryResolved(brokerID, profile)
	if !ok {
		return ""
	}
	return r.Value
}

// dispatchImageRegistryResolved is dispatchImageRegistryFor with the
// source; ok is false when the hub does not rewrite.
func (s *Server) dispatchImageRegistryResolved(brokerID, profile string) (config.ResolvedImageRegistry, bool) {
	if profile == "" && brokerID != "" && brokerID == s.LegacyColocatedBrokerID() {
		return config.ResolvedImageRegistry{}, false
	}
	nodeEnv := ""
	var broker *store.RuntimeBroker
	if brokerID != "" && s.store != nil {
		// The dispatcher's provider carries no context; the read is bounded.
		ctx, cancel := context.WithTimeout(context.Background(), imageRegistryLookupTimeout)
		b, err := s.store.GetRuntimeBroker(ctx, brokerID)
		cancel()
		switch {
		case err == nil && b != nil:
			broker = b
			if b.Capabilities != nil {
				nodeEnv = b.Capabilities.NodeImageRegistry
			}
		case errors.Is(err, store.ErrNotFound):
		default:
			slog.Warn("image_registry: cannot read the target broker; the broker rewrites the image", "broker_id", brokerID, "error", err)
			return config.ResolvedImageRegistry{}, false
		}
	}
	if broker != nil && broker.RuntimeTarget != nil {
		return config.ResolvedImageRegistry{}, false
	}

	ops := s.GetOperationalSettings()
	if ops == nil {
		return config.ResolveImageRegistryLayers(config.ImageRegistryLayers{NodeEnv: nodeEnv, File: fileImageRegistryWithoutOps()}), true
	}
	in := ops.ImageRegistryHubInputs()
	if profile == "" && broker != nil {
		profile = broker.DefaultProfile
	}
	return config.ResolveImageRegistryLayers(in.Layers(profile, nodeEnv)), true
}

// feedImageRegistryOverlay hands the hub's image_registry layers to the
// co-located broker's settings overlay, when one is installed.
func feedImageRegistryOverlay(ops *OperationalSettings) {
	if ops == nil {
		return
	}
	if overlay := config.GetGlobalSettingsOverlay(); overlay != nil {
		overlay.UpdateImageRegistryInputs(ops.ImageRegistryHubInputs())
	}
}

// maintenanceImageRegistry is the registry of a maintenance image build:
// SCION_MAINTENANCE_IMAGE_REGISTRY when set, else the hub-scope value,
// resolved at build time.
func (s *Server) maintenanceImageRegistry() string {
	s.mu.RLock()
	override := s.config.MaintenanceConfig.ImageRegistryOverride
	s.mu.RUnlock()
	if override != "" {
		return override
	}
	return s.resolveImageRegistry()
}

// WarnMaintenanceImageRegistryDrift logs, once at startup, that
// SCION_MAINTENANCE_IMAGE_REGISTRY now applies only to maintenance builds
// when it is set and differs from the registry agent images resolve to at
// hub scope.
func (s *Server) WarnMaintenanceImageRegistryDrift() {
	s.mu.RLock()
	override := s.config.MaintenanceConfig.ImageRegistryOverride
	s.mu.RUnlock()
	if override == "" {
		return
	}
	if resolved := s.resolveImageRegistry(); resolved != override {
		slog.Warn(config.EnvMaintenanceImageRegistry+" now applies only to maintenance image builds; agent images use the resolved image_registry. Set "+config.EnvImageRegistry+" or the hub image_registry setting for agent images.",
			"maintenance_image_registry", override, "agent_image_registry", resolved)
	}
}

// WarnShadowedImageRegistries logs, once at startup, the settings-file
// profile and flat-instance image_registry values that a managed hub
// top-level value now beats (ptone/scion#3916): file profiles without a
// managed profile value, and the agent_defaults.image_registry of this
// host's flat instances (instanceRegistries, keyed by instance).
func (s *Server) WarnShadowedImageRegistries(instanceRegistries map[string]string) {
	ops := s.GetOperationalSettings()
	if ops == nil {
		return
	}
	top, managedProfiles := ops.managedImageRegistries()
	if top == "" {
		return
	}
	var profiles []string
	for name, v := range ops.imageRegistryFileTier().Profiles {
		if v != "" && managedProfiles[name] == "" {
			profiles = append(profiles, name)
		}
	}
	var instances []string
	for name, v := range instanceRegistries {
		if strings.TrimSpace(v) != "" {
			instances = append(instances, name)
		}
	}
	if len(profiles) == 0 && len(instances) == 0 {
		return
	}
	sort.Strings(profiles)
	sort.Strings(instances)
	slog.Warn("image_registry: the hub's saved top-level value now beats these settings-file values; clear the hub value, or move each value into the hub profile",
		"hub_image_registry", top, "profiles", profiles, "flat_instances", instances)
}
