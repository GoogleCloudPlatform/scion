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
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/knadh/koanf/v2"
)

// OverlaySections is the input the hub gives its settings overlay for the
// map-of-objects sections (runtimes, profiles, harness_configs).
type OverlaySections struct {
	Runtimes       map[string]V1RuntimeConfig
	Profiles       map[string]V1ProfileConfig
	HarnessConfigs map[string]HarnessConfigEntry
}

// SectionDecodeError reports a stored section document that does not decode.
type SectionDecodeError struct {
	Section string
	Err     error
}

func (e *SectionDecodeError) Error() string {
	return fmt.Sprintf("%s: failed to unmarshal DB doc: %v", e.Section, e.Err)
}

func (e *SectionDecodeError) Unwrap() error { return e.Err }

// OverlayMapSections computes the overlay input for runtimes, profiles and
// harness_configs from the stored section documents (dbSections, by section
// name) and the bootstrap koanf. For each section, a stored document is
// decoded directly, so a stored empty map stays an empty map (a nil document
// decodes to an empty map); with no stored document the bootstrap koanf is
// the fallback (nil when it has no such section). A document that does not
// decode leaves that section nil and is reported in the returned errors;
// the caller decides what an undecodable section means.
//
// The hub's operational settings snapshot and the rebuild preflight both use
// this, so they compute the same input (ptone/scion#5057).
func OverlayMapSections(dbSections map[string]json.RawMessage, bootstrap *koanf.Koanf) (OverlaySections, []*SectionDecodeError) {
	var out OverlaySections
	var errs []*SectionDecodeError

	if doc, ok := dbSections["runtimes"]; ok {
		var v map[string]V1RuntimeConfig
		if err := json.Unmarshal(doc, &v); err != nil {
			errs = append(errs, &SectionDecodeError{Section: "runtimes", Err: err})
		} else {
			if v == nil {
				v = map[string]V1RuntimeConfig{}
			}
			out.Runtimes = v
		}
	} else {
		out.Runtimes = RuntimesFromBootstrapKoanf(bootstrap)
	}

	if doc, ok := dbSections["profiles"]; ok {
		var v map[string]V1ProfileConfig
		if err := json.Unmarshal(doc, &v); err != nil {
			errs = append(errs, &SectionDecodeError{Section: "profiles", Err: err})
		} else {
			if v == nil {
				v = map[string]V1ProfileConfig{}
			}
			out.Profiles = v
		}
	} else {
		out.Profiles = ProfilesFromBootstrapKoanf(bootstrap)
	}

	if doc, ok := dbSections["harness_configs"]; ok {
		var v map[string]HarnessConfigEntry
		if err := json.Unmarshal(doc, &v); err != nil {
			errs = append(errs, &SectionDecodeError{Section: "harness_configs", Err: err})
		} else {
			if v == nil {
				v = map[string]HarnessConfigEntry{}
			}
			out.HarnessConfigs = v
		}
	} else {
		out.HarnessConfigs = HarnessConfigsFromBootstrapKoanf(bootstrap)
	}

	return out, errs
}

// RuntimesFromBootstrapKoanf extracts runtimes from a bootstrap koanf
// instance (the file fallback); nil when it has none.
func RuntimesFromBootstrapKoanf(k *koanf.Koanf) map[string]V1RuntimeConfig {
	var v map[string]V1RuntimeConfig
	if !bootstrapSection(k, "runtimes", &v) {
		return nil
	}
	return v
}

// ProfilesFromBootstrapKoanf extracts profiles from a bootstrap koanf
// instance (the file fallback); nil when it has none.
func ProfilesFromBootstrapKoanf(k *koanf.Koanf) map[string]V1ProfileConfig {
	var v map[string]V1ProfileConfig
	if !bootstrapSection(k, "profiles", &v) {
		return nil
	}
	return v
}

// HarnessConfigsFromBootstrapKoanf extracts harness_configs from a bootstrap
// koanf instance (the file fallback); nil when it has none.
func HarnessConfigsFromBootstrapKoanf(k *koanf.Koanf) map[string]HarnessConfigEntry {
	var v map[string]HarnessConfigEntry
	if !bootstrapSection(k, "harness_configs", &v) {
		return nil
	}
	return v
}

// bootstrapSection decodes section of k into out through JSON, as the
// operational settings snapshot always has. It reports false, leaving out
// unset, when k has no such section or it is empty; a section that does not
// convert is logged and reported false.
func bootstrapSection(k *koanf.Koanf, section string, out any) bool {
	if k == nil || !k.Exists(section) {
		return false
	}
	sub := k.Cut(section)
	if sub == nil || len(sub.Keys()) == 0 {
		return false
	}
	data, err := json.Marshal(sub.Raw())
	if err != nil {
		slog.Warn(section+": failed to marshal from bootstrap koanf", "error", err)
		return false
	}
	if err := json.Unmarshal(data, out); err != nil {
		slog.Warn(section+": failed to unmarshal from bootstrap koanf", "error", err)
		return false
	}
	return true
}

// LoadServerBootstrapKoanf builds the hub server's bootstrap koanf: the
// layers of LoadBootstrapKoanfWithConfigPath for serverConfigPath, without
// the removed profiles.<name>.timezone keys found in the settings file
// (profileTimezones, from ScanSettingsFileProfileTimezones), so seed material
// never carries them. The server and the rebuild preflight both use it.
func LoadServerBootstrapKoanf(serverConfigPath string, profileTimezones []LegacyProfileTimezone) *koanf.Koanf {
	k := LoadBootstrapKoanfWithConfigPath(serverConfigPath)
	DeleteLegacyProfileTimezones(k, profileTimezones)
	return k
}
