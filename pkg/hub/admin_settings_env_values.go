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
	"reflect"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/knadh/koanf/v2"
)

// EnvKeyValue is one entry of the server-config GET's env_values: the value
// a SCION_SERVER_* variable gives a settings key on the node that served the
// request, and whether the hub runs with it.
type EnvKeyValue struct {
	// Value is the environment value as the hub parsed it (string, number,
	// bool or list), or the masked placeholder "********" when Masked.
	Value any `json:"value,omitempty"`
	// Masked is true when Value is the masked placeholder: the key is a
	// secret, or its value could not be shown safely.
	Masked bool `json:"masked,omitempty"`
	// InEffect is true when the hub on this node runs with the environment
	// value; false when another value (a saved database value) wins.
	InEffect bool `json:"in_effect"`
}

// startupOnlyLayer1Keys are Layer-1 keys whose saved value ApplySnapshot does
// not apply, so the hub runs with the startup value (the bootstrap merge,
// SCION_SERVER_* included) whatever is saved. Each maps to the running
// value. See layer1ExemptKeys in layer1_apply_coverage_test.go and
// ptone/scion#6000.
var startupOnlyLayer1Keys = map[string]func(s *Server) any{
	"server.auth.authorized_domains": func(s *Server) any { return s.AuthorizedDomains() },
}

// withheldEnvValueRoots are settings roots whose environment values are never
// shown: their maps (env, secrets, credentials of runtimes, profiles and
// harness configs) have no masking in the GET view.
var withheldEnvValueRoots = []string{"profiles.", "runtimes.", "harness_configs."}

// sensitiveEnvKeyWords withhold an environment value whose key names one of
// them, whatever the masking found: a second line of defence behind the GET
// masking, for keys it does not know.
var sensitiveEnvKeyWords = []string{"secret", "token", "password", "passwd", "credential", "private", "apikey", "api_key", "header", "dsn", "cookie"}

// envValueMayBeShown reports whether the environment value of key may be
// shown at all (before the GET masking is applied to it).
func envValueMayBeShown(key string) bool {
	lower := strings.ToLower(key)
	for _, root := range withheldEnvValueRoots {
		if strings.HasPrefix(lower, root) {
			return false
		}
	}
	for _, w := range sensitiveEnvKeyWords {
		if strings.Contains(lower, w) {
			return false
		}
	}
	return true
}

// serverConfigResponseFromSettings returns the GET view of vs before the
// never-shown keys are hidden and secrets are masked (see
// hideAndMaskServerConfig).
func serverConfigResponseFromSettings(vs *config.VersionedSettings) ServerConfigResponse {
	resp := ServerConfigResponse{
		SchemaVersion:        vs.SchemaVersion,
		ActiveProfile:        vs.ActiveProfile,
		DefaultTemplate:      vs.DefaultTemplate,
		DefaultHarnessConfig: vs.DefaultHarnessConfig,
		ImageRegistry:        vs.ImageRegistry,
		WorkspacePath:        vs.WorkspacePath,
		Server:               vs.Server,
		Telemetry:            vs.Telemetry,
		Runtimes:             vs.Runtimes,
		HarnessConfigs:       vs.HarnessConfigs,
		Profiles:             vs.Profiles,
		DefaultMaxTurns:      vs.DefaultMaxTurns,
		DefaultMaxModelCalls: vs.DefaultMaxModelCalls,
		DefaultMaxDuration:   vs.DefaultMaxDuration,
		DefaultResources:     vs.DefaultResources,
		DefaultModel:         vs.DefaultModel,
		DefaultThinkingLevel: vs.DefaultThinkingLevel,
		DefaultMaxAgentRole:  vs.DefaultMaxAgentRole,
		DefaultAgentRole:     vs.DefaultAgentRole,
		DefaultRuntimeBroker: vs.DefaultRuntimeBroker,
		DefaultTimezone:      vs.DefaultTimezone,
		AutoInjectGcloudADC:  vs.AutoInjectGcloudADC,
		AutoExposePorts:      vs.AutoExposePorts,
		Quotas:               vs.Quotas,
		AgentSecrets:         vs.AgentSecrets,

		DefaultGCPIdentityMode:             vs.DefaultGCPIdentityMode,
		DefaultGCPIdentityServiceAccountID: vs.DefaultGCPIdentityServiceAccountID,
	}
	// Populate top-level federation field from the server config.
	if vs.Server != nil && vs.Server.Federation != nil {
		resp.Federation = vs.Server.Federation
	}
	return resp
}

// hideNeverShownServerKeys clears, in a GET view, the server keys the GET
// never returns: server.hub.class and server.hub.test_identities are never
// API-writable, so a client that echoes the body back must not send them;
// nor is the relay-peer secret ever returned, nor the proxy driver settings
// (WS2).
func hideNeverShownServerKeys(srv *config.V1ServerConfig) {
	if srv == nil {
		return
	}
	if srv.Hub != nil {
		srv.Hub.HubClass = ""
		srv.Hub.TestIdentities = nil
		if c := srv.Hub.Conduit; c != nil {
			c.PeerSecret = ""
		}
	}
	hideNeverAPIWritableProxyKeys(srv)
}

// shownEnvValue returns the value the GET may show for the environment
// value of key in envK, and whether it is masked. It fails closed: the
// value is decoded into the settings struct alone, put through the same
// view, hiding and masking as the GET body, and taken from that masked view
// only. A key that does not decode into a known field, that the view leaves
// out, that the masking changed, or that envValueMayBeShown refuses, is
// shown as the masked placeholder (maskedValue, true).
func shownEnvValue(envK *koanf.Koanf, key string) (any, bool) {
	if !envValueMayBeShown(key) {
		return maskedValue, true
	}
	return viewMaskedEnvValue(envK, key)
}

// viewMaskedEnvValue is shownEnvValue without the envValueMayBeShown
// refusal: the value as the GET view and its masking show it. Separate so
// tests can prove the masking alone covers every masked field class.
func viewMaskedEnvValue(envK *koanf.Koanf, key string) (any, bool) {
	vs, err := config.DecodeSettingsKey(envK, key)
	if err != nil {
		return maskedValue, true
	}
	plain, err := config.FlattenSettingsJSON(vs)
	if err != nil {
		return maskedValue, true
	}
	// A fresh decode: the view shares pointers with vs, and the masking
	// writes through them.
	vsView, err := config.DecodeSettingsKey(envK, key)
	if err != nil {
		return maskedValue, true
	}
	view := serverConfigResponseFromSettings(vsView)
	hideNeverShownServerKeys(view.Server)
	maskSensitiveFields(&view)
	shown, err := config.FlattenSettingsJSON(view)
	if err != nil {
		return maskedValue, true
	}
	pv, inPlain := plain[key]
	sv, inShown := shown[key]
	switch {
	case inPlain && inShown:
		if !reflect.DeepEqual(pv, sv) {
			return maskedValue, true
		}
		return sv, false
	case !inPlain && !inShown:
		// The field encodes as absent: a zero value under omitempty can be
		// shown; anything else (a json:"-" field) is masked.
		if v := envK.Get(key); config.IsZeroSettingScalar(v) {
			return v, false
		}
		return maskedValue, true
	default:
		// Present in the settings but not in the GET view: hidden.
		return maskedValue, true
	}
}

// envValueEqualsShown reports whether the environment value of key equals
// the value at key in the flattened unmasked GET view running.
func envValueEqualsShown(envK *koanf.Koanf, key string, running map[string]any) bool {
	vs, err := config.DecodeSettingsKey(envK, key)
	if err != nil {
		return false
	}
	plain, err := config.FlattenSettingsJSON(vs)
	if err != nil {
		return false
	}
	ev, eok := plain[key]
	rv, rok := running[key]
	return eok == rok && (!eok || reflect.DeepEqual(ev, rv))
}

// fileTierEnvValues returns env_values for the file-tier GET. The settings
// loader layers SCION_SERVER_* above settings.yaml (LoadGlobalConfig), so
// every environment value is the one in effect.
func fileTierEnvValues(envK *koanf.Koanf) map[string]EnvKeyValue {
	keys := opsettings.DetectEnvOverrides(envK)
	if len(keys) == 0 {
		return nil
	}
	out := make(map[string]EnvKeyValue, len(keys))
	for _, key := range keys {
		v, masked := shownEnvValue(envK, key)
		out[key] = EnvKeyValue{Value: v, Masked: masked, InEffect: true}
	}
	return out
}

// dbTierEnvValues returns env_values for the DB-tier GET. view is the GET
// body before secrets are masked: for Layer-1 keys it holds the value the
// hub runs with (the snapshot, a saved database value winning over the
// bootstrap merge). A Layer-1 environment value is in effect when it equals
// that value, or for startupOnlyLayer1Keys when it equals the running value;
// a pinned public URL is in effect. Layer-0 and other keys are read at
// startup from the bootstrap merge, where SCION_SERVER_* is the top layer, so
// they are in effect.
func (s *Server) dbTierEnvValues(envK *koanf.Koanf, view *ServerConfigResponse) map[string]EnvKeyValue {
	keys := opsettings.DetectEnvOverrides(envK)
	if len(keys) == 0 {
		return nil
	}
	running, err := config.FlattenSettingsJSON(view)
	if err != nil {
		running = map[string]any{}
	}
	out := make(map[string]EnvKeyValue, len(keys))
	for _, key := range keys {
		v, masked := shownEnvValue(envK, key)
		out[key] = EnvKeyValue{Value: v, Masked: masked, InEffect: s.dbTierEnvInEffect(envK, key, running)}
	}
	return out
}

// dbTierEnvInEffect reports, for dbTierEnvValues, whether the environment
// value of key is the one the hub runs with.
func (s *Server) dbTierEnvInEffect(envK *koanf.Koanf, key string, running map[string]any) bool {
	if !opsettings.IsLayer1Key(key) {
		return true
	}
	if key == config.PublicURLKey && s.HubEndpoints().Pin != "" {
		return true
	}
	if runningValue, ok := startupOnlyLayer1Keys[key]; ok {
		cur, err := config.FlattenSettingsJSON(map[string]any{"v": runningValue(s)})
		if err != nil {
			return false
		}
		single := map[string]any{}
		if v, ok := cur["v"]; ok {
			single[key] = v
		}
		return envValueEqualsShown(envK, key, single)
	}
	return envValueEqualsShown(envK, key, running)
}
