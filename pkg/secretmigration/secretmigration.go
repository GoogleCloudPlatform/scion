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

// Package secretmigration performs the one-shot migration of plugin
// credentials found in raw configuration (settings.yaml inline config and
// per-plugin YAML config files) into the secret backend.
//
// It lives in its own package because two unrelated call paths need it: the
// server boot path in cmd/, which migrates every registered plugin before
// loading them, and the hub's runtime activation path, which migrates a single
// plugin when an operator activates it from the admin UI. Both must run the
// migration *before* config.ResolvePluginConfig strips secret keys, otherwise
// the credential is dropped and never reaches the backend.
package secretmigration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// MigratePluginSecrets performs a one-shot migration of the secret config keys
// declared in config.PluginSecretKeyMap for pluginName into the secret backend.
// Both sources of raw config are considered: the inline map from settings.yaml
// and, when configFile is set, the per-plugin YAML file it points at.
//
// config.ResolvePluginConfig strips secret config keys (bot_token,
// signing_key, ...) from *both* sources, so for both of them the credential
// only reaches the plugin if it has been migrated first — the backend is the
// authoritative store, and injectPluginSecretsIntoConfig fills the stripped key
// back in from there. Migrating is therefore not a hygiene improvement; it is
// what keeps a plugin able to start.
//
// When a secret key is present in both sources the config file value wins — see
// migrationSource.
//
// Existing backend values are never overwritten: this seeds the backend, it does
// not rotate it. That has a consequence worth knowing about. Once a key has been
// migrated, editing it in settings.yaml or the config file has no effect at all,
// because the raw value is stripped during resolution and the backend copy is
// what the plugin receives. An operator who rotates the credential in YAML and
// sees no change is hitting this; rotation must happen in the backend. The log
// line below names the file the value was taken from so it can be cleaned up.
//
// Calling this is safe without external locking. The semantics are seed-only: a
// key that already holds a value is left alone, and two writers racing an
// unseeded key both derive their value from the same config, so either ordering
// yields the same result. The Get-then-Set is a TOCTOU, but a benign one — the
// worst outcome is a redundant backend version holding the same value. The boot
// path still takes an advisory lock, to avoid replicas repeating the work rather
// than for correctness; the runtime activation path calls this without one.
//
// A nil backend, an unknown plugin, and an unreadable config file are all no-ops
// rather than errors — migration is best-effort and must never block plugin
// startup or activation.
func MigratePluginSecrets(ctx context.Context, sb secret.SecretBackend, pluginName string, inlineConfig map[string]string, configFile string) {
	if sb == nil {
		return
	}

	mappings, ok := config.PluginSecretKeyMap[pluginName]
	if !ok {
		return
	}

	fileConfig := loadConfigFile(pluginName, configFile)

	hubID := sb.HubID()
	for _, m := range mappings {
		val, source := migrationSource(m.ConfigKey, inlineConfig, fileConfig, configFile)
		if val == "" {
			continue
		}
		existing, err := sb.Get(ctx, m.SecretKey, store.ScopeHub, hubID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.Warn("failed to check secret backend, skipping migration",
				"config_key", m.ConfigKey, "plugin", pluginName, "error", err)
			continue
		}
		if existing != nil && existing.Value != "" {
			continue
		}
		if _, _, err := sb.Set(ctx, &secret.SetSecretInput{
			Name:          m.SecretKey,
			Value:         val,
			SecretType:    secret.TypeVariable,
			InjectionMode: "as_needed",
			Scope:         store.ScopeHub,
			ScopeID:       hubID,
			// Only the file name goes into backend metadata — a full host path
			// would leak the operator's username and directory layout.
			Description: fmt.Sprintf("Auto-migrated from %s for plugin %s", filepath.Base(source), pluginName),
		}); err != nil {
			slog.Warn("failed to migrate secret to backend",
				"config_key", m.ConfigKey, "plugin", pluginName, "error", err)
			continue
		}
		// The log carries the full source path, unlike the description: an
		// operator being told to clean up a file needs to know which one, and
		// this goes to their own logs rather than into backend metadata. The
		// path is printed as written in settings.yaml, so a relative or
		// "~/"-prefixed form is shown unresolved.
		slog.Info("migrated plugin secret to the secret backend — remove it from the source config",
			"config_key", m.ConfigKey, "plugin", pluginName, "source", source)
	}
}

// migrationSource picks which raw value to migrate for one secret config key,
// and returns a label naming where it came from.
//
// The per-plugin config file wins over inline config. That is the opposite of
// config.LoadPluginConfigFile's merge order, and deliberately so: when
// config_file is set, config.ResolvePluginConfig ignores non-wiring inline keys
// entirely and resolves the plugin from the file, so the file holds the
// credential the plugin is actually running on. Migrating the inline value
// instead would seed the backend with a different — possibly stale — credential
// that the plugin would then run on, since the backend copy is what gets
// injected once both raw copies are stripped.
func migrationSource(configKey string, inlineConfig, fileConfig map[string]string, configFile string) (value, source string) {
	if v := fileConfig[configKey]; v != "" {
		return v, configFile
	}
	return inlineConfig[configKey], "settings.yaml"
}

// loadConfigFile reads the raw per-plugin YAML config file so
// MigratePluginSecrets can see secrets that live only in that file. A missing
// file yields an empty map; any load failure is logged and treated as empty so
// migration still proceeds for inline config.
func loadConfigFile(pluginName, configFile string) map[string]string {
	if configFile == "" {
		return nil
	}
	fileConfig, err := config.LoadPluginConfigFile(configFile, nil)
	if err != nil {
		slog.Warn("failed to read config file during secret migration, migrating inline config only",
			"plugin", pluginName, "config_file", configFile, "error", err)
		return nil
	}
	return fileConfig
}
