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
	// value; false when another value (a saved database value, or a value
	// start-up sets from a flag or the workstation defaults) wins. Nil
	// (omitted) when Masked: see maskedEnvKeyValue.
	InEffect *bool `json:"in_effect,omitempty"`
	// SavedDiffers is true when a stored value of the key exists (in
	// settings.yaml, or in a managed database row for a Layer-1 key on a
	// DB-backed hub) and differs from the environment value. Only then does
	// the page label the stored value as "saved, not in effect"; a default,
	// or a stored value equal to the environment value, is never so
	// labelled. Always false (omitted) when Masked: see maskedEnvKeyValue.
	SavedDiffers bool `json:"saved_differs,omitempty"`
}

// maskedEnvKeyValue is the env_values entry of a masked key: the masked
// placeholder only, with neither InEffect nor SavedDiffers. Both bits
// compare the environment value with a stored or running value, so for a
// secret they would let a caller who can write settings confirm a guessed
// environment value (write the guess, read the bit). They are therefore
// neither computed nor returned for any key shownEnvValue masks: the GET
// body masking (viewMaskedEnvValue) plus the keys envValueMayBeShown or the
// never-API-writable filter refuse, and any key that cannot be decoded.
func maskedEnvKeyValue() EnvKeyValue {
	return EnvKeyValue{Value: maskedValue, Masked: true}
}

// envSavedDiffers reports whether stored (flattened stored values, keyed by
// settings key) holds key with a value other than the environment value.
func envSavedDiffers(envK *koanf.Koanf, key string, stored map[string]any) bool {
	v, ok := stored[key]
	if !ok {
		return false
	}
	return !envValueEqualsShown(envK, key, map[string]any{key: v})
}

// fileStoredValues returns the settings.yaml values (vs) flattened by
// settings key, for envSavedDiffers. Internal only: never returned.
func fileStoredValues(vs *config.VersionedSettings) map[string]any {
	if vs == nil {
		return nil
	}
	flat, err := config.FlattenSettingsJSON(vs)
	if err != nil {
		return nil
	}
	return flat
}

// dbStoredValues returns, for envSavedDiffers on a DB-backed hub, the
// settings.yaml values of the keys outside the Layer-1 sections (file) and
// the values the managed database rows hold for Layer-1 keys. A seeded row
// is not a saved value (start-up rewrites it from the bootstrap merge), and
// a Layer-1 key that a managed row does not hold is not saved either.
func dbStoredValues(file map[string]any, ops *OperationalSettings) map[string]any {
	out := make(map[string]any, len(file))
	for k, v := range file {
		if !opsettings.IsLayer1Key(k) {
			out[k] = v
		}
	}
	if ops == nil {
		return out
	}
	ops.mu.RLock()
	rows := make(map[string]json.RawMessage, len(ops.cache))
	for name, ss := range ops.cache {
		if ss.Origin == "managed" && !ss.Malformed {
			rows[name] = ss.Value
		}
	}
	ops.mu.RUnlock()
	for section, doc := range rows {
		var m map[string]any
		if err := json.Unmarshal(doc, &m); err != nil {
			continue
		}
		for field, v := range m {
			if p := opsettings.KoanfPathFromSectionKey(section, field); p != "" {
				out[p] = v
			}
		}
	}
	return out
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
// never returns: server.hub.class, server.hub.test_identities and
// server.hub.gcp_project_id (the hub's GCP project, read once at startup;
// ptone/scion#4942) are never API-writable, so a client that echoes the body
// back must not send them;
// nor is the relay-peer secret ever returned, nor the proxy driver settings
// (WS2), nor the server keyring file path and its world-readable override
// (ptone/scion#1530). Both tiers' GET builders and the env_values view use
// it, so the three cannot drift.
func hideNeverShownServerKeys(srv *config.V1ServerConfig) {
	if srv == nil {
		return
	}
	if srv.Hub != nil {
		srv.Hub.HubClass = ""
		srv.Hub.TestIdentities = nil
		srv.Hub.GCPProjectID = ""
		if c := srv.Hub.Conduit; c != nil {
			c.PeerSecret = ""
		}
	}
	hideNeverAPIWritableProxyKeys(srv)
	srv.KeyringFile = ""
	srv.KeyringFileAllowWorldReadable = false
}

// shownEnvValue returns the value the GET may show for the environment
// value of key in envK, and whether it is masked. It fails closed: the
// value is decoded into the settings struct alone, put through the same
// view, hiding and masking as the GET body, and taken from that masked view
// only. A key that does not decode into a known field, that the view leaves
// out, that the masking changed, or that envValueMayBeShown refuses, is
// shown as the masked placeholder (maskedValue, true).
func shownEnvValue(envK *koanf.Koanf, key string) (any, bool) {
	if !envValueMayBeShown(key) || len(withoutNeverAPIWritableKeys([]string{key})) == 0 {
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

// startupOverriddenServerKeys are the server keys that start-up sets after
// the settings load, so an environment value of one may not be the value
// the hub runs with: the workstation defaults (hub host, broker enabled,
// dev auth, storage provider, secrets backend: cmd applyWorkstationDefaults
// and applyServerListenHosts) and the command-line flags (--port, --host,
// --db, --enable-runtime-broker, --runtime-broker-port, --dev-auth,
// --storage-bucket, --storage-dir). Each holds a plain string, number or
// bool, so its value compares exactly. Their in_effect compares with
// ServerConfig.StartupServerSettings. The conduit flags also override
// settings, but their values are durations whose text form start-up
// normalizes, so they are left to the loader's precedence (a documented
// limitation).
var startupOverriddenServerKeys = map[string]bool{
	"server.hub.host":           true,
	"server.hub.port":           true,
	"server.broker.enabled":     true,
	"server.broker.port":        true,
	"server.broker.host":        true,
	"server.auth.dev_mode":      true,
	"server.storage.provider":   true,
	"server.storage.bucket":     true,
	"server.storage.local_path": true,
	"server.secrets.backend":    true,
	"server.database.url":       true,
}

// startupEnvInEffect reports, for a key in startupOverriddenServerKeys,
// whether its environment value equals the value start-up resolved
// (startup, ServerConfig.StartupServerSettings). ok is false for any other
// key, or when startup is nil; the caller then applies its own rule.
func startupEnvInEffect(envK *koanf.Koanf, key string, startup *config.V1ServerConfig) (inEffect, ok bool) {
	if startup == nil || !startupOverriddenServerKeys[key] {
		return false, false
	}
	running, err := config.FlattenSettingsJSON(map[string]any{"server": startup})
	if err != nil {
		return false, true
	}
	return envValueEqualsShown(envK, key, running), true
}

// fileTierEnvValues returns env_values for the file-tier GET, one entry per
// key of keys, which must be the response's env_overrides (already without
// the never-API-writable keys), so the two lists never differ. The settings
// loader layers SCION_SERVER_* above settings.yaml (LoadGlobalConfig), so an
// environment value is in effect, except for a key start-up overrides
// after the load, which compares with startup (startupEnvInEffect).
//
// stored is fileStoredValues of settings.yaml (nil without a file).
func fileTierEnvValues(envK *koanf.Koanf, keys []string, startup *config.V1ServerConfig, stored map[string]any) map[string]EnvKeyValue {
	if len(keys) == 0 {
		return nil
	}
	out := make(map[string]EnvKeyValue, len(keys))
	for _, key := range keys {
		v, masked := shownEnvValue(envK, key)
		if masked {
			out[key] = maskedEnvKeyValue()
			continue
		}
		inEffect, ok := startupEnvInEffect(envK, key, startup)
		if !ok {
			inEffect = true
		}
		out[key] = EnvKeyValue{Value: v, InEffect: boolPtr(inEffect), SavedDiffers: envSavedDiffers(envK, key, stored)}
	}
	return out
}

// dbTierEnvValues returns env_values for the DB-tier GET. view is the GET
// body before secrets are masked: for Layer-1 keys it holds the value the
// hub runs with (the snapshot, a saved database value winning over the
// bootstrap merge). A key start-up overrides compares with the start-up
// value (startupEnvInEffect). A Layer-1 environment value is in effect when
// it equals the value in view, or for startupOnlyLayer1Keys when it equals
// the running value; a pinned public URL is in effect. Other Layer-0 keys
// are read at startup from the bootstrap merge, where SCION_SERVER_* is the
// top layer, so they are in effect.
//
// keys must be the response's env_overrides (already without the
// never-API-writable keys), so the two lists never differ. stored is
// dbStoredValues.
func (s *Server) dbTierEnvValues(envK *koanf.Koanf, keys []string, view *ServerConfigResponse, stored map[string]any) map[string]EnvKeyValue {
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
		if masked {
			out[key] = maskedEnvKeyValue()
			continue
		}
		out[key] = EnvKeyValue{
			Value:        v,
			InEffect:     boolPtr(s.dbTierEnvInEffect(envK, key, running)),
			SavedDiffers: envSavedDiffers(envK, key, stored),
		}
	}
	return out
}

// dbTierEnvInEffect reports, for dbTierEnvValues, whether the environment
// value of key is the one the hub runs with.
func (s *Server) dbTierEnvInEffect(envK *koanf.Koanf, key string, running map[string]any) bool {
	if inEffect, ok := startupEnvInEffect(envK, key, s.config.StartupServerSettings); ok {
		return inEffect
	}
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
