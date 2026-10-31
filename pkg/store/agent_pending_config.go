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
	"strings"
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

	// LimitsFormat is the storage format of the count limits (max_turns,
	// max_model_calls) in Config (ptone/scion#5605). Rows written before
	// the count limits were typed have none (0): their 0 meant "clear",
	// because an int field encoded a null clear as 0. Format 2 rows keep
	// a clear as null and Unlimited as 0. Storage always writes
	// PendingLimitsFormatTyped (MarshalPendingConfigForStorage), and
	// loading normalizes an older row (UnmarshalPendingConfigFromStorage),
	// so every in-memory value is format 2. Persisted, never returned
	// (MarshalJSON drops it).
	LimitsFormat int `json:"limitsFormat,omitempty"`

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
	out.LimitsFormat = 0
	out.Config = redactPendingConfigForResponse(p.Config, p.envResponseVisible)
	return json.Marshal(out)
}

// PendingLimitsFormatTyped is the AgentPendingConfig.LimitsFormat of a row
// whose count limits are typed: a null clears, 0 is Unlimited.
const PendingLimitsFormatTyped = 2

// pendingCountLimitKeys are the config keys of the count limits, lower-cased.
var pendingCountLimitKeys = map[string]bool{"max_turns": true, "max_model_calls": true}

// normalizePendingLimits brings a row of an older format to
// PendingLimitsFormatTyped, in place: a count-limit value of 0, which then
// could only mean "clear", becomes null. Keys are matched
// case-insensitively, as encoding/json decodes them. Any other value is
// left as it is.
func normalizePendingLimits(p *AgentPendingConfig) {
	if p == nil || p.LimitsFormat >= PendingLimitsFormatTyped {
		return
	}
	for k, v := range p.Config {
		if pendingCountLimitKeys[strings.ToLower(k)] && strings.TrimSpace(string(v)) == "0" {
			p.Config[k] = json.RawMessage("null")
		}
	}
	p.LimitsFormat = PendingLimitsFormatTyped
}

// MarshalPendingConfigForStorage encodes p for the agents.pending_config
// column, bypassing the response redaction: every key round-trips as held.
// It returns "" for an empty p, so the column is cleared.
func MarshalPendingConfigForStorage(p *AgentPendingConfig) (string, error) {
	if p.IsEmpty() {
		return "", nil
	}
	// Every row written is typed: a fresh row built in memory carries no
	// format, and all in-memory rows hold typed values (loading
	// normalizes older ones), so the format is stamped here.
	out := *p
	out.LimitsFormat = PendingLimitsFormatTyped
	type alias AgentPendingConfig
	data, err := json.Marshal((*alias)(&out))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// UnmarshalPendingConfigFromStorage decodes a stored agents.pending_config
// value; "" decodes to nil. It is the one load path of a stored row, so a
// row of an older format is normalized here (normalizePendingLimits): every
// reader and writer of a loaded row sees typed count limits.
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
	normalizePendingLimits(&p)
	return &p, nil
}

// redactPendingConfigForResponse returns a copy of cfg with config.env, the
// telemetry cloud-export headers, every MCP server's env and headers and
// every service's env removed unless canAttach (as
// AgentAppliedConfig.ResponseView redacts the inline config), and with
// GITHUB_TOKEN removed from env always. The hub stores the pending config
// in canonical form, but keys are matched case-insensitively here too, as
// encoding/json decodes them, so a stored value of another spelling cannot
// escape redaction. A value that is not the expected JSON shape is dropped
// when it could hold a redacted value, so a malformed stored value fails
// closed.
func redactPendingConfigForResponse(cfg map[string]json.RawMessage, canAttach bool) map[string]json.RawMessage {
	if cfg == nil {
		return nil
	}
	out := maps.Clone(cfg)
	for k, raw := range out {
		switch {
		case strings.EqualFold(k, "env"):
			if !canAttach {
				delete(out, k)
				continue
			}
			if string(raw) == "null" {
				continue
			}
			var env map[string]json.RawMessage
			if err := json.Unmarshal(raw, &env); err != nil {
				delete(out, k)
				continue
			}
			delete(env, "GITHUB_TOKEN")
			b, err := json.Marshal(env)
			if err != nil {
				delete(out, k)
				continue
			}
			out[k] = b
		case strings.EqualFold(k, "telemetry") && !canAttach && string(raw) != "null":
			b, ok := redactTelemetryHeaders(raw)
			if !ok {
				delete(out, k)
				continue
			}
			out[k] = b
		case strings.EqualFold(k, "mcp_servers") && !canAttach && string(raw) != "null":
			b, ok := redactMCPServersRaw(raw)
			if !ok {
				delete(out, k)
				continue
			}
			out[k] = b
		case strings.EqualFold(k, "services") && !canAttach && string(raw) != "null":
			b, ok := redactServicesRaw(raw)
			if !ok {
				delete(out, k)
				continue
			}
			out[k] = b
		}
	}
	return out
}

// redactTelemetryHeaders returns raw (a telemetry object) with every
// cloud.headers entry removed, matching both keys case-insensitively. ok is
// false when raw is not the expected shape.
func redactTelemetryHeaders(raw json.RawMessage) (json.RawMessage, bool) {
	var tel map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tel); err != nil {
		return nil, false
	}
	for tk, cloudRaw := range tel {
		if !strings.EqualFold(tk, "cloud") || string(cloudRaw) == "null" {
			continue
		}
		var cloud map[string]json.RawMessage
		if err := json.Unmarshal(cloudRaw, &cloud); err != nil {
			delete(tel, tk)
			continue
		}
		for ck := range cloud {
			if strings.EqualFold(ck, "headers") {
				delete(cloud, ck)
			}
		}
		b, err := json.Marshal(cloud)
		if err != nil {
			delete(tel, tk)
			continue
		}
		tel[tk] = b
	}
	b, err := json.Marshal(tel)
	if err != nil {
		return nil, false
	}
	return b, true
}

// withoutKeys re-encodes the JSON object raw without the keys names matches
// (case-insensitively). ok is false when raw is not an object.
func withoutKeys(raw json.RawMessage, names ...string) (json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, false
	}
	for k := range obj {
		for _, n := range names {
			if strings.EqualFold(k, n) {
				delete(obj, k)
			}
		}
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return b, true
}

// redactMCPServersRaw returns raw (an MCP server map) with every server's
// env and headers removed; the servers themselves stay. ok is false when
// raw is not the expected shape.
func redactMCPServersRaw(raw json.RawMessage) (json.RawMessage, bool) {
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(raw, &servers); err != nil {
		return nil, false
	}
	for name, srv := range servers {
		if string(srv) == "null" {
			continue
		}
		b, ok := withoutKeys(srv, "env", "headers")
		if !ok {
			return nil, false
		}
		servers[name] = b
	}
	b, err := json.Marshal(servers)
	if err != nil {
		return nil, false
	}
	return b, true
}

// redactServicesRaw returns raw (a service list) with every service's env
// removed; the services themselves stay. ok is false when raw is not the
// expected shape.
func redactServicesRaw(raw json.RawMessage) (json.RawMessage, bool) {
	var services []json.RawMessage
	if err := json.Unmarshal(raw, &services); err != nil {
		return nil, false
	}
	for i, svc := range services {
		if string(svc) == "null" {
			continue
		}
		b, ok := withoutKeys(svc, "env")
		if !ok {
			return nil, false
		}
		services[i] = b
	}
	b, err := json.Marshal(services)
	if err != nil {
		return nil, false
	}
	return b, true
}
