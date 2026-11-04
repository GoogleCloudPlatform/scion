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
	"encoding/json"
	"reflect"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/knadh/koanf/v2"
)

// RunningValueSourceStartup is the source of a running_values entry: the
// value start-up resolved, from the workstation defaults or a command-line
// flag (or the coded default when neither the settings file nor the
// environment sets the key). The hub does not record which of them set it.
const RunningValueSourceStartup = "startup"

// RunningKeyValue is one entry of the server-config GET's running_values
// (ptone/scion#3722): the value this node runs with for a server key that
// start-up sets after the settings load (startupOverriddenServerKeys), where
// it differs from the value the response shows.
type RunningKeyValue struct {
	// Value is the running value, as the GET view would show it.
	Value any `json:"value"`
	// Source is RunningValueSourceStartup.
	Source string `json:"source"`
}

// runningValuesNeverShown are the start-up-set keys that never get a
// running_values entry, whatever their value, empty included: the GET masks
// them (secrets), so even an entry for an empty value would tell a caller
// that the node runs without the secret while one is stored, a
// running-versus-stored oracle. The masked-equals-unmasked check alone does
// not catch an empty value (masking leaves "" unchanged), and the field's
// omitempty tag is not a rule. TestRunningValuesNeverShown_CoversMaskedKeys
// requires every start-up-set string key that the GET masking masks to be
// listed here.
var runningValuesNeverShown = map[string]bool{
	"server.database.url": true,
}

// startupRunningValues returns running_values for a server-config GET.
// view is the response being built, after the never-shown keys are hidden
// and BEFORE the secrets are masked; startup is
// ServerConfig.StartupServerSettings. Nil when startup is nil or no key
// differs.
//
// Only the keys of startupOverriddenServerKeys are reported. They are all
// restart-required, so the value start-up resolved is the value the node
// runs with until the next start. A key gets an entry only when its running
// value differs from the value in view, and not when the environment sets
// it to the running value (env_values already reports that value as in
// effect). Keys whose file value comes from a --config file are not
// covered (ptone/scion#3845).
//
// Masking fails closed, as for env_values: the running value is put through
// the same view, hiding and masking as the GET body. A key listed in
// runningValuesNeverShown (a secret, whatever its value, empty included),
// a key whose value the masking changes, that the view leaves out, or that
// envValueMayBeShown or the never-API-writable filter refuses, gets no entry
// at all (not even a masked one): whether an entry exists compares the
// running value with the stored one, so for a secret it would let a
// settings writer confirm a guessed value.
func startupRunningValues(envK *koanf.Koanf, startup *config.V1ServerConfig, view *ServerConfigResponse) map[string]RunningKeyValue {
	if startup == nil || view == nil {
		return nil
	}
	plain, shown, ok := startupValueViews(startup)
	if !ok {
		return nil
	}
	current, err := config.FlattenSettingsJSON(view)
	if err != nil {
		return nil
	}
	keys := make([]string, 0, len(startupOverriddenServerKeys))
	for k := range startupOverriddenServerKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out map[string]RunningKeyValue
	for _, key := range keys {
		if runningValuesNeverShown[key] || !envValueMayBeShown(key) || len(withoutNeverAPIWritableKeys([]string{key})) == 0 {
			continue
		}
		rv, inPlain := plain[key]
		sv, inShown := shown[key]
		if !inPlain || !inShown || !reflect.DeepEqual(rv, sv) {
			// Unset at start-up (nothing to show), or hidden or masked.
			continue
		}
		if cv, inCurrent := current[key]; inCurrent && reflect.DeepEqual(rv, cv) {
			continue
		}
		if envK != nil && envK.Exists(key) && envValueEqualsShown(envK, key, plain) {
			continue
		}
		if out == nil {
			out = make(map[string]RunningKeyValue)
		}
		out[key] = RunningKeyValue{Value: sv, Source: RunningValueSourceStartup}
	}
	return out
}

// startupValueViews returns the start-up server settings flattened by
// settings key twice: as they are (plain), and as the GET view shows them
// after hiding and masking (shown). Each view decodes its own copy, because
// the masking writes through the settings' pointers.
func startupValueViews(startup *config.V1ServerConfig) (plain, shown map[string]any, ok bool) {
	data, err := json.Marshal(startup)
	if err != nil {
		return nil, nil, false
	}
	var plainSrv, viewSrv config.V1ServerConfig
	if json.Unmarshal(data, &plainSrv) != nil || json.Unmarshal(data, &viewSrv) != nil {
		return nil, nil, false
	}
	plain, err = config.FlattenSettingsJSON(map[string]any{"server": &plainSrv})
	if err != nil {
		return nil, nil, false
	}
	resp := serverConfigResponseFromSettings(&config.VersionedSettings{Server: &viewSrv})
	hideNeverShownServerKeys(resp.Server)
	maskSensitiveFields(&resp)
	shown, err = config.FlattenSettingsJSON(resp)
	if err != nil {
		return nil, nil, false
	}
	return plain, shown, true
}
