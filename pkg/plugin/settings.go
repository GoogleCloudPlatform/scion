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

package plugin

// V1PluginsConfigAdapter converts versioned settings plugin entries to PluginsConfig.
// This avoids a circular dependency between pkg/config and pkg/plugin.
//
// Usage:
//
//	if vs.Server != nil && vs.Server.Plugins != nil {
//	    cfg := plugin.PluginsConfigFromEntries(vs.Server.Plugins.Broker)
//	}
type V1PluginEntryLike struct {
	Path        string
	Config      map[string]string
	ConfigFile  string
	SelfManaged bool
	Address     string
}

// PluginsConfigFromEntries builds a PluginsConfig from a broker entry map.
func PluginsConfigFromEntries(brokerEntries map[string]V1PluginEntryLike) PluginsConfig {
	cfg := PluginsConfig{
		Broker: make(map[string]PluginEntry),
	}
	for name, entry := range brokerEntries {
		cfg.Broker[name] = PluginEntry{
			Path:        entry.Path,
			Config:      entry.Config,
			ConfigFile:  entry.ConfigFile,
			SelfManaged: entry.SelfManaged,
			Address:     entry.Address,
		}
	}
	return cfg
}
