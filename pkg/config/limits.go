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

import "github.com/GoogleCloudPlatform/scion/pkg/api"

// FillDefaultLimits fills each limit cfg leaves unset from the default
// limits, highest tier first: hub (the hub agent_defaults, may be nil), then
// the broker settings defaults (settingsTurns, settingsCalls,
// settingsDuration; 0 or "" means no default). A count limit is unset only
// when nil: an explicit 0 ("no limit") is the agent's own value and is never
// filled (ptone/scion#5605). A negative count limit is invalid (the hub
// refuses it); if one reaches here it is treated as unset, so it inherits
// rather than lifting the limit. A max_duration is unset when "" ("0" is
// "no limit"). A nil cfg is left alone.
//
// It is the pure form of the broker's per-start default fill, exported so
// that the hub can check its PATCH dispositions against the same rule.
func FillDefaultLimits(cfg *api.ScionConfig, hub *api.HubAgentDefaults, settingsTurns, settingsCalls int, settingsDuration string) {
	if cfg == nil {
		return
	}
	fillCount := func(p **int, defaults ...int) {
		if *p != nil && **p >= 0 {
			return
		}
		*p = nil
		for _, d := range defaults {
			if d > 0 {
				*p = api.IntPtr(d)
				return
			}
		}
	}
	var hubTurns, hubCalls int
	var hubDuration string
	if hub != nil {
		hubTurns, hubCalls, hubDuration = hub.MaxTurns, hub.MaxModelCalls, hub.MaxDuration
	}
	fillCount(&cfg.MaxTurns, hubTurns, settingsTurns)
	fillCount(&cfg.MaxModelCalls, hubCalls, settingsCalls)
	if cfg.MaxDuration == "" {
		if hubDuration != "" {
			cfg.MaxDuration = hubDuration
		} else if settingsDuration != "" {
			cfg.MaxDuration = settingsDuration
		}
	}
}
