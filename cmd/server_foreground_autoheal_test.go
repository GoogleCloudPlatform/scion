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

package cmd

import (
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// autoPopulateBrokerTypes replicates the auto-populate logic from
// server_foreground.go so it can be tested in isolation.
func autoPopulateBrokerTypes(vs *config.VersionedSettings) {
	if vs.Server == nil || vs.Server.Plugins == nil || vs.Server.MessageBroker == nil {
		return
	}
	if vs.Server.MessageBroker.Enabled && len(vs.Server.MessageBroker.Types) == 0 && len(vs.Server.Plugins.Broker) > 0 {
		for name := range vs.Server.Plugins.Broker {
			vs.Server.MessageBroker.Types = append(vs.Server.MessageBroker.Types, name)
		}
	}
}

// pluginsNotInTypes returns the names of broker plugins that are loaded
// but not listed in message_broker.types.
func pluginsNotInTypes(vs *config.VersionedSettings) []string {
	if vs.Server == nil || vs.Server.Plugins == nil || vs.Server.MessageBroker == nil {
		return nil
	}
	if !vs.Server.MessageBroker.Enabled || len(vs.Server.MessageBroker.Types) == 0 {
		return nil
	}
	typesSet := make(map[string]bool, len(vs.Server.MessageBroker.Types))
	for _, t := range vs.Server.MessageBroker.Types {
		typesSet[t] = true
	}
	var missing []string
	for name := range vs.Server.Plugins.Broker {
		if !typesSet[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

func TestAutoPopulateBrokerTypes_EmptyTypesWithPlugins(t *testing.T) {
	vs := &config.VersionedSettings{
		Server: &config.V1ServerConfig{
			MessageBroker: &config.V1MessageBrokerConfig{
				Enabled: true,
				Types:   nil, // empty
			},
			Plugins: &config.V1PluginsConfig{
				Broker: map[string]config.V1PluginEntry{
					"discord": {Path: "/usr/bin/scion-plugin-discord"},
				},
			},
		},
	}

	autoPopulateBrokerTypes(vs)

	if len(vs.Server.MessageBroker.Types) != 1 {
		t.Fatalf("expected 1 type, got %d", len(vs.Server.MessageBroker.Types))
	}
	if vs.Server.MessageBroker.Types[0] != "discord" {
		t.Errorf("expected type 'discord', got %q", vs.Server.MessageBroker.Types[0])
	}
}

func TestAutoPopulateBrokerTypes_MultiplePlugins(t *testing.T) {
	vs := &config.VersionedSettings{
		Server: &config.V1ServerConfig{
			MessageBroker: &config.V1MessageBrokerConfig{
				Enabled: true,
				Types:   []string{}, // explicitly empty
			},
			Plugins: &config.V1PluginsConfig{
				Broker: map[string]config.V1PluginEntry{
					"discord":  {Path: "/usr/bin/scion-plugin-discord"},
					"telegram": {Path: "/usr/bin/scion-plugin-telegram"},
				},
			},
		},
	}

	autoPopulateBrokerTypes(vs)

	if len(vs.Server.MessageBroker.Types) != 2 {
		t.Fatalf("expected 2 types, got %d", len(vs.Server.MessageBroker.Types))
	}

	sort.Strings(vs.Server.MessageBroker.Types)
	if vs.Server.MessageBroker.Types[0] != "discord" || vs.Server.MessageBroker.Types[1] != "telegram" {
		t.Errorf("unexpected types: %v", vs.Server.MessageBroker.Types)
	}
}

func TestAutoPopulateBrokerTypes_NoOpWhenTypesAlreadySet(t *testing.T) {
	vs := &config.VersionedSettings{
		Server: &config.V1ServerConfig{
			MessageBroker: &config.V1MessageBrokerConfig{
				Enabled: true,
				Types:   []string{"discord"},
			},
			Plugins: &config.V1PluginsConfig{
				Broker: map[string]config.V1PluginEntry{
					"discord":  {Path: "/usr/bin/scion-plugin-discord"},
					"telegram": {Path: "/usr/bin/scion-plugin-telegram"},
				},
			},
		},
	}

	autoPopulateBrokerTypes(vs)

	// Should not modify — types was already populated.
	if len(vs.Server.MessageBroker.Types) != 1 {
		t.Fatalf("expected 1 type (unchanged), got %d", len(vs.Server.MessageBroker.Types))
	}
	if vs.Server.MessageBroker.Types[0] != "discord" {
		t.Errorf("expected type 'discord', got %q", vs.Server.MessageBroker.Types[0])
	}
}

func TestAutoPopulateBrokerTypes_NoOpWhenNotEnabled(t *testing.T) {
	vs := &config.VersionedSettings{
		Server: &config.V1ServerConfig{
			MessageBroker: &config.V1MessageBrokerConfig{
				Enabled: false,
				Types:   nil,
			},
			Plugins: &config.V1PluginsConfig{
				Broker: map[string]config.V1PluginEntry{
					"discord": {Path: "/usr/bin/scion-plugin-discord"},
				},
			},
		},
	}

	autoPopulateBrokerTypes(vs)

	if len(vs.Server.MessageBroker.Types) != 0 {
		t.Fatalf("expected 0 types when broker not enabled, got %d", len(vs.Server.MessageBroker.Types))
	}
}

func TestAutoPopulateBrokerTypes_NoOpWhenNoPlugins(t *testing.T) {
	vs := &config.VersionedSettings{
		Server: &config.V1ServerConfig{
			MessageBroker: &config.V1MessageBrokerConfig{
				Enabled: true,
				Types:   nil,
			},
			Plugins: &config.V1PluginsConfig{
				Broker: map[string]config.V1PluginEntry{},
			},
		},
	}

	autoPopulateBrokerTypes(vs)

	if len(vs.Server.MessageBroker.Types) != 0 {
		t.Fatalf("expected 0 types when no plugins, got %d", len(vs.Server.MessageBroker.Types))
	}
}

func TestPluginsNotInTypes_WarnsOnMissingPlugin(t *testing.T) {
	vs := &config.VersionedSettings{
		Server: &config.V1ServerConfig{
			MessageBroker: &config.V1MessageBrokerConfig{
				Enabled: true,
				Types:   []string{"discord"},
			},
			Plugins: &config.V1PluginsConfig{
				Broker: map[string]config.V1PluginEntry{
					"discord":  {Path: "/usr/bin/scion-plugin-discord"},
					"telegram": {Path: "/usr/bin/scion-plugin-telegram"},
				},
			},
		},
	}

	missing := pluginsNotInTypes(vs)
	if len(missing) != 1 {
		t.Fatalf("expected 1 missing plugin, got %d: %v", len(missing), missing)
	}
	if missing[0] != "telegram" {
		t.Errorf("expected 'telegram', got %q", missing[0])
	}
}

func TestPluginsNotInTypes_AllPresent(t *testing.T) {
	vs := &config.VersionedSettings{
		Server: &config.V1ServerConfig{
			MessageBroker: &config.V1MessageBrokerConfig{
				Enabled: true,
				Types:   []string{"discord", "telegram"},
			},
			Plugins: &config.V1PluginsConfig{
				Broker: map[string]config.V1PluginEntry{
					"discord":  {},
					"telegram": {},
				},
			},
		},
	}

	missing := pluginsNotInTypes(vs)
	if len(missing) != 0 {
		t.Errorf("expected no missing plugins, got %v", missing)
	}
}
