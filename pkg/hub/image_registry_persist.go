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
	"bytes"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// image_registry persistence (ptone/scion#3916): the value is stored only
// when an admin changes it. The value GET shows is the value a save
// compares the request with:
//
//   - a key a managed row holds is compared with its stored value: a
//     different non-empty value is stored (even one equal to the file
//     tier), "" clears it, an equal value keeps it;
//   - any other key (seeded row, no row, managed row without the key) is
//     compared with the value GET shows for it: at the top level this
//     replica's file tier, at profile scope "" (GET shows managed profile
//     values only). An equal value or "" stores nothing.
//
// A seeded or no-row base never carries image_registry into a managed row
// (endpointsSeededBase, endpointsNoRowBase, stripProfileImageRegistries),
// and a save whose filtered document equals the filtered base from the
// same read is not written (sameAsFilteredBase), so an echo neither adopts
// a seeded row nor bumps its revision.

// endpointsImageRegistryKey is the endpoints section key of image_registry.
const endpointsImageRegistryKey = "image_registry"

// filterEndpointsImageRegistry returns the endpoints request filter of an
// image_registry save. base is the endpoints base from the merge's row
// read: it holds image_registry only when that row is managed and holds
// it. Otherwise a request equal to fileTop (the value GET shows) is an
// echo and is dropped from the request.
func filterEndpointsImageRegistry(fileTop string) func(base, next map[string]json.RawMessage, fp *fieldPresence) {
	return func(base, next map[string]json.RawMessage, fp *fieldPresence) {
		if rawJSONString(base[endpointsImageRegistryKey]) != "" {
			return // a managed key: the request applies as sent
		}
		sent, ok := next[endpointsImageRegistryKey]
		if !ok && fp != nil {
			sent, ok = fp.raw[endpointsImageRegistryKey]
		}
		if !ok {
			return
		}
		if v := rawJSONString(sent); v == "" || v == strings.TrimSpace(fileTop) {
			delete(next, endpointsImageRegistryKey)
			if fp != nil {
				delete(fp.raw, endpointsImageRegistryKey)
			}
		}
	}
}

// rawJSONString decodes a JSON string value, trimmed; "" for anything
// else (absent, null, not a string).
func rawJSONString(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// endpointsMergeOptions is bodyMergeOptions("endpoints") with the
// image_registry filter for this replica, and baseOut set to receive the
// filtered base for the no-op check.
func endpointsMergeOptions(ops *OperationalSettings, baseOut *json.RawMessage) sectionMergeOptions {
	opts := bodyMergeOptions("endpoints")
	opts.requestFilter = filterEndpointsImageRegistry(ops.imageRegistryFileTier().Top)
	opts.baseOut = baseOut
	return opts
}

// sameAsFilteredBase reports whether a section document equals the
// filtered base from the same read (decoded-JSON compare), that is,
// whether writing it would change nothing but the row's origin and
// revision.
func sameAsFilteredBase(doc, base json.RawMessage) bool {
	if len(base) == 0 {
		return false
	}
	var a, b any
	if json.Unmarshal(doc, &a) != nil || json.Unmarshal(base, &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

// stripProfileImageRegistries removes image_registry from every profile.
func stripProfileImageRegistries(profiles map[string]config.V1ProfileConfig) {
	for name, p := range profiles {
		if p.ImageRegistry != "" {
			p.ImageRegistry = ""
			profiles[name] = p
		}
	}
}

// profilesFilteredBase returns the profiles document a save that sends
// back what GET showed would write, from row (nil = no row): a managed
// row as stored; a seeded row, or with no row the effective (bootstrap)
// catalog, without image_registry values, which are file copies there.
func profilesFilteredBase(ops *OperationalSettings, row *store.HubSetting) (json.RawMessage, error) {
	profiles := map[string]config.V1ProfileConfig{}
	switch {
	case row != nil:
		if len(bytes.TrimSpace(row.Value)) > 0 {
			if err := json.Unmarshal(row.Value, &profiles); err != nil {
				return nil, err
			}
		}
		if row.Origin != opsettings.OriginManaged {
			stripProfileImageRegistries(profiles)
		}
	default:
		for name, p := range ops.Snapshot().Profiles {
			profiles[name] = p
		}
		stripProfileImageRegistries(profiles)
	}
	if profiles == nil {
		profiles = map[string]config.V1ProfileConfig{}
	}
	return json.Marshal(profiles)
}

// showManagedProfileImageRegistries returns profiles (a GET view) with each
// image_registry replaced by the managed profiles row's value: blank for a
// seeded row, for no row, and for a profile of a managed row without one.
// The map is copied; profiles is not modified.
func showManagedProfileImageRegistries(profiles map[string]config.V1ProfileConfig, managed map[string]string) map[string]config.V1ProfileConfig {
	if profiles == nil {
		return nil
	}
	out := make(map[string]config.V1ProfileConfig, len(profiles))
	for name, p := range profiles {
		p.ImageRegistry = managed[name]
		out[name] = p
	}
	return out
}
