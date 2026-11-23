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
	"slices"
	"strings"
	"time"

	"github.com/knadh/koanf/v2"
)

const (
	// SystemOneSecretNamesKey is the Layer-0 operator-only settings key mapping
	// System One adapter IDs to extra permitted named-secret keys
	// (server.hub.system_one.secret_names). Read once at Hub startup; refused
	// unconditionally by the server-config API in every mode.
	SystemOneSecretNamesKey = "server.hub.system_one.secret_names"

	// SystemOneDefaultMaxEvalTime is the default wall-clock budget for
	// evaluating System One rules on a single message
	// (server.hub.system_one.max_eval_time / hub.systemOne.maxEvalTime).
	SystemOneDefaultMaxEvalTime = 3 * time.Second

	// SystemOneMaxEvalTimeLimit is the hard upper bound on
	// server.hub.system_one.max_eval_time.
	SystemOneMaxEvalTimeLimit = 30 * time.Second

	// SystemOneDefaultMaxInFlightPerProject is the default per-replica
	// concurrency semaphore limit per project
	// (server.hub.system_one.max_in_flight_per_project; runtime-design §§4.2, 7.2).
	SystemOneDefaultMaxInFlightPerProject = 4

	// SystemOneDefaultAdmissionWait is the default maximum duration an
	// evaluation waits for per-project / per-instance admission permits before
	// failing fast with capacity (server.hub.system_one.admission_wait;
	// runtime-design §7.2).
	SystemOneDefaultAdmissionWait = 100 * time.Millisecond
)

// HubSystemOneConfig holds Hub-wide System One rule evaluation settings
// (server.hub.system_one). Concurrency and rate limits are per Hub replica.
// Values are kept as configured and checked by Validate.
type HubSystemOneConfig struct {
	// MaxEvalTime bounds the wall-clock time spent evaluating System One
	// rules for a single message ("" = SystemOneDefaultMaxEvalTime, maximum
	// SystemOneMaxEvalTimeLimit).
	MaxEvalTime string `json:"maxEvalTime,omitempty" yaml:"maxEvalTime,omitempty" koanf:"maxEvalTime"`

	// MaxInFlightPerProject caps concurrent in-flight System One evaluations
	// per project per Hub replica (nil = SystemOneDefaultMaxInFlightPerProject = 4,
	// 0 = unlimited, negative values are rejected).
	MaxInFlightPerProject *int `json:"maxInFlightPerProject,omitempty" yaml:"maxInFlightPerProject,omitempty" koanf:"maxInFlightPerProject"`

	// AdmissionWait bounds how long an evaluation waits to acquire per-project
	// and per-instance admission permits before failing fast with capacity
	// ("" = SystemOneDefaultAdmissionWait = 100ms, maximum SystemOneMaxEvalTimeLimit).
	AdmissionWait string `json:"admissionWait,omitempty" yaml:"admissionWait,omitempty" koanf:"admissionWait"`

	// SecretNames maps System One adapter IDs to extra permitted named-secret
	// keys (server.hub.system_one.secret_names; PV3 Addendum F §F10). Read once
	// at Hub startup from settings.yaml or SCION_SERVER_* env; never writable
	// or echoed through the server-config API.
	SecretNames map[string][]string `json:"secretNames,omitempty" yaml:"secretNames,omitempty" koanf:"secretNames"`
}

// IsZero reports whether no System One hub setting is configured.
func (c HubSystemOneConfig) IsZero() bool {
	return c.MaxEvalTime == "" && c.MaxInFlightPerProject == nil && c.AdmissionWait == "" && len(c.SecretNames) == 0
}

// CloneSecretNames returns a deep copy of c.SecretNames, or nil when empty.
func (c HubSystemOneConfig) CloneSecretNames() map[string][]string {
	return cloneStringSliceMap(c.SecretNames)
}

func cloneStringSliceMap(in map[string][]string) map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]string, len(in))
	for k, v := range in {
		out[k] = slices.Clone(v)
	}
	return out
}

// parseSystemOneSecretNamesEnvRest recognizes SCION_SERVER_* suffixes (without
// the leading "SCION_SERVER_") targeting server.hub.system_one.secret_names
// (either as a whole JSON map or for a specific adapter key, e.g.
// HUB_SYSTEM_ONE_SECRET_NAMES_GEMINI_DECISIONS or
// HUB_SYSTEMONE_SECRETNAMES_GEMINI_DECISIONS).
func parseSystemOneSecretNamesEnvRest(rest string) (adapter string, matched bool) {
	up := strings.ToUpper(strings.TrimSpace(rest))
	for _, p := range []string{
		"HUB_SYSTEM_ONE_SECRET_NAMES",
		"HUB_SYSTEMONE_SECRETNAMES",
		"HUB_SYSTEM_ONE_SECRETNAMES",
		"HUB_SYSTEMONE_SECRET_NAMES",
	} {
		if up == p {
			return "", true
		}
		if suf, ok := strings.CutPrefix(up, p+"_"); ok && suf != "" {
			return strings.ToLower(suf), true
		}
	}
	return "", false
}

// applySystemOneSecretNamesEnv overlays SCION_SERVER_HUB_SYSTEM_ONE_SECRET_NAMES*
// (and collapsed-underscore equivalents) onto gc.Hub.SystemOne.SecretNames,
// accepting either a JSON object mapping adapter -> []string on the root key or
// a JSON array / comma-separated list on a per-adapter suffix key.
func applySystemOneSecretNamesEnv(gc *GlobalConfig) error {
	if gc == nil {
		return nil
	}
	k := koanf.New("|")
	_ = k.Load(nonEmptyEnvProvider(serverEnvPrefix, func(rest string) string {
		if _, ok := parseSystemOneSecretNamesEnvRest(rest); ok {
			return rest
		}
		return ""
	}), nil)
	keys := k.Keys()
	if len(keys) == 0 {
		return nil
	}
	slices.Sort(keys)
	// Apply root JSON map keys first, then per-adapter overrides so specific
	// adapter env vars win deterministically over a root JSON map.
	for _, passPerAdapter := range []bool{false, true} {
		for _, rest := range keys {
			adapter, ok := parseSystemOneSecretNamesEnvRest(rest)
			if !ok || (adapter != "") != passPerAdapter {
				continue
			}
			raw := strings.TrimSpace(k.String(rest))
			if raw == "" {
				continue
			}
			if adapter == "" {
				var parsed map[string][]string
				if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
					return fmt.Errorf("invalid %s%s: expected JSON map of adapter to secret name list: %w", serverEnvPrefix, rest, err)
				}
				if gc.Hub.SystemOne.SecretNames == nil {
					gc.Hub.SystemOne.SecretNames = make(map[string][]string, len(parsed))
				}
				for a, names := range parsed {
					gc.Hub.SystemOne.SecretNames[a] = slices.Clone(names)
				}
				continue
			}
			var list []string
			if strings.HasPrefix(raw, "[") {
				if err := json.Unmarshal([]byte(raw), &list); err != nil {
					return fmt.Errorf("invalid %s%s: %w", serverEnvPrefix, rest, err)
				}
			} else {
				list = parseCommaSeparatedList(raw)
			}
			if gc.Hub.SystemOne.SecretNames == nil {
				gc.Hub.SystemOne.SecretNames = make(map[string][]string)
			}
			gc.Hub.SystemOne.SecretNames[adapter] = list
		}
	}
	return nil
}

// MaxEvalTimeDuration parses MaxEvalTime ("" = SystemOneDefaultMaxEvalTime).
// Values <= 0 or > SystemOneMaxEvalTimeLimit are rejected.
func (c HubSystemOneConfig) MaxEvalTimeDuration() (time.Duration, error) {
	if c.MaxEvalTime == "" {
		return SystemOneDefaultMaxEvalTime, nil
	}
	d, err := time.ParseDuration(c.MaxEvalTime)
	if err != nil {
		return 0, fmt.Errorf("invalid server.hub.system_one.max_eval_time %q: %w", c.MaxEvalTime, err)
	}
	if d <= 0 || d > SystemOneMaxEvalTimeLimit {
		return 0, fmt.Errorf("invalid server.hub.system_one.max_eval_time %q: must be > 0 and <= %s", c.MaxEvalTime, SystemOneMaxEvalTimeLimit)
	}
	return d, nil
}

// EffectiveMaxInFlightPerProject returns the per-replica per-project concurrency
// limit (nil = SystemOneDefaultMaxInFlightPerProject = 4, 0 = unlimited).
// Negative values are rejected.
func (c HubSystemOneConfig) EffectiveMaxInFlightPerProject() (int, error) {
	if c.MaxInFlightPerProject == nil {
		return SystemOneDefaultMaxInFlightPerProject, nil
	}
	if *c.MaxInFlightPerProject < 0 {
		return 0, fmt.Errorf("invalid server.hub.system_one.max_in_flight_per_project %d: must be >= 0", *c.MaxInFlightPerProject)
	}
	return *c.MaxInFlightPerProject, nil
}

// AdmissionWaitDuration parses AdmissionWait ("" = SystemOneDefaultAdmissionWait).
// Values <= 0 or > SystemOneMaxEvalTimeLimit are rejected.
func (c HubSystemOneConfig) AdmissionWaitDuration() (time.Duration, error) {
	if c.AdmissionWait == "" {
		return SystemOneDefaultAdmissionWait, nil
	}
	d, err := time.ParseDuration(c.AdmissionWait)
	if err != nil {
		return 0, fmt.Errorf("invalid server.hub.system_one.admission_wait %q: %w", c.AdmissionWait, err)
	}
	if d <= 0 || d > SystemOneMaxEvalTimeLimit {
		return 0, fmt.Errorf("invalid server.hub.system_one.admission_wait %q: must be > 0 and <= %s", c.AdmissionWait, SystemOneMaxEvalTimeLimit)
	}
	return d, nil
}

// Validate checks every System One hub setting.
func (c HubSystemOneConfig) Validate() error {
	if _, err := c.MaxEvalTimeDuration(); err != nil {
		return err
	}
	if _, err := c.EffectiveMaxInFlightPerProject(); err != nil {
		return err
	}
	if _, err := c.AdmissionWaitDuration(); err != nil {
		return err
	}
	return nil
}
