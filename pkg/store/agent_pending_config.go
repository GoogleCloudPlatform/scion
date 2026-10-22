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

package store

import (
	"encoding/json"
	"maps"
	"time"
)

// AgentPendingConfig is an agent's pending config: config edits the hub
// stored instead of applying them (ptone/scion#3976). Container-creation
// keys saved while the agent has a live or transitional container are
// applied at its next start; provision-rendered keys stay pending until a
// reincarnation. It is persisted in its own column (agents.pending_config),
// so a reincarnation's rebuild of AgentAppliedConfig, or a broker echo of
// it, never touches it, and it is written with the agent's state_version
// compare-and-set.
//
// A pending edit is a stored agent PATCH body: applying it replays the same
// code the live PATCH runs, so held and immediate edits have one meaning.
type AgentPendingConfig struct {
	// Config is the raw config object, keyed by the config's json key (the
	// hub canonicalizes key case), holding only the keys an edit named:
	// presence survives storage, and a stored JSON null means "clear this
	// key" when it is applied. Later saves overlay their keys on it.
	Config map[string]json.RawMessage `json:"config,omitempty"`

	// SavedBy is the principal of the most recent save ("user:<id>" or
	// "agent:<id>"), and SavedByName its display name or email when known.
	SavedBy     string    `json:"savedBy,omitempty"`
	SavedByName string    `json:"savedByName,omitempty"`
	SavedAt     time.Time `json:"savedAt"`
	// BaseGeneration and BaseStateVersion are the agent's generation and
	// state version the most recent save was made against.
	BaseGeneration   int   `json:"baseGeneration"`
	BaseStateVersion int64 `json:"baseStateVersion"`

	// EnvSaverCanViewEnv records whether the caller who last saved
	// config.env could read the agent's env. Applying the edit passes it to
	// the env-removal rules the live PATCH gates on the caller's env access.
	// Persisted, never returned (MarshalJSON drops it).
	EnvSaverCanViewEnv bool `json:"envSaverCanViewEnv,omitempty"`

	// envResponseVisible gates whether MarshalJSON includes config.env and
	// the telemetry cloud-export headers, exactly like
	// AgentAppliedConfig.envResponseVisible: false unless ResponseView set
	// it, so a response path that forgets ResponseView fails closed.
	// Persistence bypasses MarshalJSON (see MarshalPendingConfigForStorage).
	envResponseVisible bool
}

// IsEmpty reports whether p holds no pending edit.
func (p *AgentPendingConfig) IsEmpty() bool {
	return p == nil || len(p.Config) == 0
}

// Clone returns a deep copy of p (nil for nil). The raw values are copied
// too, so the copy can be edited without touching p.
func (p *AgentPendingConfig) Clone() *AgentPendingConfig {
	if p == nil {
		return nil
	}
	out := *p
	if p.Config != nil {
		out.Config = make(map[string]json.RawMessage, len(p.Config))
		for k, v := range p.Config {
			out.Config[k] = append(json.RawMessage(nil), v...)
		}
	}
	return &out
}

// ResponseView returns a copy of p for an API response. canAttach must be
// the caller's attach-equivalent access to the agent, the same decision
// AgentAppliedConfig.ResponseView takes: without it config.env and the
// telemetry cloud-export headers are left out; with it, GITHUB_TOKEN is
// still never returned.
func (p *AgentPendingConfig) ResponseView(canAttach bool) *AgentPendingConfig {
	if p == nil {
		return nil
	}
	view := p.Clone()
	view.envResponseVisible = canAttach
	return view
}

// MarshalJSON is the response encoding: it drops EnvSaverCanViewEnv, and
// redacts config.env and config.telemetry.cloud.headers unless ResponseView
// allowed them (GITHUB_TOKEN is always dropped from env).
func (p AgentPendingConfig) MarshalJSON() ([]byte, error) {
	type alias AgentPendingConfig
	out := alias(p)
	out.EnvSaverCanViewEnv = false
	out.Config = redactPendingConfigForResponse(p.Config, p.envResponseVisible)
	return json.Marshal(out)
}

// MarshalPendingConfigForStorage encodes p for the agents.pending_config
// column, bypassing the response redaction: every key round-trips as held.
// It returns "" for an empty p, so the column is cleared.
func MarshalPendingConfigForStorage(p *AgentPendingConfig) (string, error) {
	if p.IsEmpty() {
		return "", nil
	}
	type alias AgentPendingConfig
	data, err := json.Marshal((*alias)(p))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// UnmarshalPendingConfigFromStorage decodes a stored agents.pending_config
// value; "" decodes to nil.
func UnmarshalPendingConfigFromStorage(raw string) (*AgentPendingConfig, error) {
	if raw == "" {
		return nil, nil
	}
	var p AgentPendingConfig
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, err
	}
	if p.IsEmpty() {
		return nil, nil
	}
	return &p, nil
}

// redactPendingConfigForResponse returns a copy of cfg with config.env and
// the telemetry cloud-export headers removed unless canAttach, and with
// GITHUB_TOKEN removed from env always. A value that is not the expected
// JSON shape is dropped when it could hold a redacted value, so a malformed
// stored value fails closed.
func redactPendingConfigForResponse(cfg map[string]json.RawMessage, canAttach bool) map[string]json.RawMessage {
	if cfg == nil {
		return nil
	}
	out := maps.Clone(cfg)
	if raw, ok := out["env"]; ok {
		switch {
		case !canAttach:
			delete(out, "env")
		case string(raw) != "null":
			var env map[string]json.RawMessage
			if err := json.Unmarshal(raw, &env); err != nil {
				delete(out, "env")
				break
			}
			delete(env, "GITHUB_TOKEN")
			b, err := json.Marshal(env)
			if err != nil {
				delete(out, "env")
				break
			}
			out["env"] = b
		}
	}
	if raw, ok := out["telemetry"]; ok && !canAttach && string(raw) != "null" {
		var tel map[string]json.RawMessage
		if err := json.Unmarshal(raw, &tel); err != nil {
			delete(out, "telemetry")
		} else if cloudRaw, ok := tel["cloud"]; ok && string(cloudRaw) != "null" {
			var cloud map[string]json.RawMessage
			if err := json.Unmarshal(cloudRaw, &cloud); err != nil {
				delete(tel, "cloud")
			} else {
				delete(cloud, "headers")
				if b, err := json.Marshal(cloud); err == nil {
					tel["cloud"] = b
				} else {
					delete(tel, "cloud")
				}
			}
			if b, err := json.Marshal(tel); err == nil {
				out["telemetry"] = b
			} else {
				delete(out, "telemetry")
			}
		}
	}
	return out
}
