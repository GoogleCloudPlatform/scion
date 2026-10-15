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

package config

import (
	"os"
	"strings"

	"github.com/knadh/koanf/v2"
)

// EnvImageRegistry is the node environment variable that overrides every
// other image_registry source for the process that runs an agent
// (ptone/scion#3916). It is not a settings override: it is excluded from
// the settings env mappers (settingsExcludedEnvVars) and read only by
// ImageRegistryFromEnv.
const EnvImageRegistry = "SCION_IMAGE_REGISTRY"

// EnvMaintenanceImageRegistry overrides the registry of maintenance image
// builds only. It is not an image_registry source for agents.
const EnvMaintenanceImageRegistry = "SCION_MAINTENANCE_IMAGE_REGISTRY"

// ImageRegistryFromEnv returns this process's SCION_IMAGE_REGISTRY. It is
// the only reader of that variable; process entry points call it once and
// pass the value on as the node env layer.
func ImageRegistryFromEnv() string {
	return strings.TrimSpace(os.Getenv(EnvImageRegistry))
}

// ImageRegistrySource names the layer an image_registry value came from.
type ImageRegistrySource string

// The image_registry layers, highest precedence first.
const (
	ImageRegistrySourceNodeEnv      ImageRegistrySource = "node_env"
	ImageRegistrySourceHubDBProfile ImageRegistrySource = "hub_db_profile"
	ImageRegistrySourceHubDB        ImageRegistrySource = "hub_db"
	ImageRegistrySourceFileProfile  ImageRegistrySource = "file_profile"
	ImageRegistrySourceFile         ImageRegistrySource = "file"
	ImageRegistrySourceDefault      ImageRegistrySource = "default"
)

// ImageRegistryLayers holds one value per image_registry layer. The order
// is by source first (node env, hub database, settings file, default) and,
// within a source, by scope (profile or flat instance before the top
// level):
//
//	node env > hub DB profile > hub DB top > file profile/instance > file top > default ("")
//
// A hub DB value is a value of a managed settings row only; a seeded row
// mirrors the startup values and counts as file.
type ImageRegistryLayers struct {
	NodeEnv      string
	HubDBProfile string
	HubDB        string
	FileProfile  string
	File         string
}

// ResolvedImageRegistry is the winning image_registry value and its layer.
type ResolvedImageRegistry struct {
	Value  string
	Source ImageRegistrySource
}

// ResolveImageRegistryLayers returns the first non-blank layer in precedence
// order, or an empty value from the default layer when all are blank.
func ResolveImageRegistryLayers(l ImageRegistryLayers) ResolvedImageRegistry {
	for _, c := range []struct {
		v string
		s ImageRegistrySource
	}{
		{l.NodeEnv, ImageRegistrySourceNodeEnv},
		{l.HubDBProfile, ImageRegistrySourceHubDBProfile},
		{l.HubDB, ImageRegistrySourceHubDB},
		{l.FileProfile, ImageRegistrySourceFileProfile},
		{l.File, ImageRegistrySourceFile},
	} {
		if v := strings.TrimSpace(c.v); v != "" {
			return ResolvedImageRegistry{Value: v, Source: c.s}
		}
	}
	return ResolvedImageRegistry{Source: ImageRegistrySourceDefault}
}

// ImageRegistryFileTier is the settings-file tier of image_registry as a
// hub replica starts with it: its bootstrap merge (coded defaults <
// SCION_SEED_* < settings.yaml < SCION_SERVER_*). Top is never resolved
// through a profile.
type ImageRegistryFileTier struct {
	Top      string
	Profiles map[string]string
}

// ImageRegistryFileTierFromKoanf reads the file tier from a bootstrap koanf
// (top-level image_registry and profiles.<name>.image_registry). A nil
// koanf gives an empty tier.
func ImageRegistryFileTierFromKoanf(k *koanf.Koanf) ImageRegistryFileTier {
	t := ImageRegistryFileTier{}
	if k == nil {
		return t
	}
	t.Top = strings.TrimSpace(k.String("image_registry"))
	if profiles, ok := k.Get("profiles").(map[string]interface{}); ok {
		for name, raw := range profiles {
			p, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if v, ok := p["image_registry"].(string); ok && strings.TrimSpace(v) != "" {
				if t.Profiles == nil {
					t.Profiles = map[string]string{}
				}
				t.Profiles[name] = strings.TrimSpace(v)
			}
		}
	}
	return t
}

// ProfileValue returns the file-tier image_registry of profile within the
// profile catalog in effect (catalog: the names the profiles map resolves
// against). A profile outside the catalog (a retained built-in profile, or
// a missing one) has none: built-in definitions carry no image_registry. A
// profile in the catalog takes the file value even when a managed
// profiles row holds its definition: such a row carries image_registry
// only when an admin set it, so a missing key inherits the file value.
func (t ImageRegistryFileTier) ProfileValue(profile string, catalog []string) string {
	if profile == "" || !containsString(catalog, profile) {
		return ""
	}
	return t.Profiles[profile]
}

// ImageRegistryHubInputs are the hub-side image_registry layers a
// co-located broker resolves with: the managed hub database values, the
// hub's file tier and the profile catalog in effect. The hub computes them
// with the same accessor it resolves dispatches with, so both sides agree.
type ImageRegistryHubInputs struct {
	// ManagedTop is the image_registry of a managed endpoints row.
	ManagedTop string
	// ManagedProfiles maps a profile to the image_registry of a managed
	// profiles row (non-empty values only).
	ManagedProfiles map[string]string
	// FileTier is the hub's settings-file tier.
	FileTier ImageRegistryFileTier
	// Catalog is the sorted list of profile names the profiles map
	// resolves against (the stored catalog, or the file's when none is
	// stored).
	Catalog []string
	// ActiveProfile is active_profile as the broker loads it. Only the hub
	// reads it, filled for each dispatch; the broker resolves an empty
	// profile through its own live settings, never this copy.
	ActiveProfile string
}

// Clone returns a deep copy of in.
func (in ImageRegistryHubInputs) Clone() ImageRegistryHubInputs {
	out := in
	out.ManagedProfiles = cloneStringMap(in.ManagedProfiles)
	out.FileTier.Profiles = cloneStringMap(in.FileTier.Profiles)
	out.Catalog = append([]string(nil), in.Catalog...)
	return out
}

// Layers returns the image_registry layers for profile (already resolved
// from "" by the caller) on a profile-based target.
func (in ImageRegistryHubInputs) Layers(profile, nodeEnv string) ImageRegistryLayers {
	return ImageRegistryLayers{
		NodeEnv:      nodeEnv,
		HubDBProfile: in.ManagedProfiles[profile],
		HubDB:        in.ManagedTop,
		FileProfile:  in.FileTier.ProfileValue(profile, in.Catalog),
		File:         in.FileTier.Top,
	}
}

// TopLayers returns the image_registry layers of a read with no profile
// (the hub scope): the profile layers are skipped.
func (in ImageRegistryHubInputs) TopLayers(nodeEnv string) ImageRegistryLayers {
	return ImageRegistryLayers{
		NodeEnv: nodeEnv,
		HubDB:   in.ManagedTop,
		File:    in.FileTier.Top,
	}
}

// EffectiveDefaultProfile returns the profile an agent with no profile
// resolves to against catalog: configured, unless the catalog is a single
// profile that is not configured, in which case that profile. It is the
// part of the settings overlay's default-profile rule that decides the
// image_registry profile (resolveOverlaidActiveProfile), shared so the hub
// and the co-located broker agree.
func EffectiveDefaultProfile(configured string, catalog []string) string {
	if len(catalog) == 1 && catalog[0] != configured {
		return catalog[0]
	}
	return configured
}

// ImageRegistryLayers returns the image_registry layers for profileName
// ("" = the active profile) with nodeEnv as the node env layer.
//
//   - A flat instance view has no profile layers: its
//     agent_defaults.image_registry is the file profile/instance layer.
//   - With hub inputs from the co-located hub's settings overlay, the hub
//     database and file layers come from those inputs.
//   - Otherwise (a remote broker, the CLI) there is no hub database layer
//     and the file layers are this settings' own values.
func (vs *VersionedSettings) ImageRegistryLayers(profileName, nodeEnv string) ImageRegistryLayers {
	l := ImageRegistryLayers{NodeEnv: nodeEnv}
	if vs == nil {
		return l
	}
	hub := vs.imageRegistryHub
	if vs.flat != nil {
		l.FileProfile = vs.flat.Defaults.ImageRegistry
		l.File = vs.ImageRegistry
		if hub != nil {
			l.HubDB = hub.ManagedTop
			l.File = hub.FileTier.Top
		}
		return l
	}
	if profileName == "" {
		profileName = vs.ActiveProfile
	}
	if hub != nil {
		return hub.Layers(profileName, nodeEnv)
	}
	if p, ok := vs.Profiles[profileName]; ok {
		l.FileProfile = p.ImageRegistry
	}
	l.File = vs.ImageRegistry
	return l
}

// ResolveImageRegistry returns the image_registry for profileName ("" =
// the active profile) from the hub database and settings-file layers only,
// without a node env. A caller that runs or builds agent images resolves
// ImageRegistryLayers with ImageRegistryFromEnv instead.
func (vs *VersionedSettings) ResolveImageRegistry(profileName string) string {
	return ResolveImageRegistryLayers(vs.ImageRegistryLayers(profileName, "")).Value
}

// cloneStringMap returns a copy of m (nil for an empty map).
func cloneStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
