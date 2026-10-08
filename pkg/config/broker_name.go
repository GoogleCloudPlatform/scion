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
	"os"
	"strings"
)

// BrokerNameSettingKey is the settings key that holds this host's runtime
// broker name. In versioned settings it is stored as
// server.broker.broker_nickname. 'runtime-broker register --broker-name'
// persists it here, and 'server start' reads it when naming the broker.
const BrokerNameSettingKey = "hub.brokerNickname"

// ConfiguredBrokerName returns the runtime broker name set in the global
// settings in globalDir: server.broker.broker_nickname (hub.brokerNickname),
// else server.broker.broker_name. It returns "" when neither is set, in
// which case the broker is named after the OS hostname. The order matches
// how 'server start' names the broker.
func ConfiguredBrokerName(globalDir string) string {
	if globalDir != "" {
		if s, err := LoadSettings(globalDir); err == nil && s != nil && s.Hub != nil {
			if n := strings.TrimSpace(s.Hub.BrokerNickname); n != "" {
				return n
			}
		}
	}
	if cfg, err := LoadGlobalConfig(""); err == nil && cfg != nil {
		if n := strings.TrimSpace(cfg.RuntimeBroker.BrokerName); n != "" {
			return n
		}
	}
	return ""
}

// LocalBrokerName returns the name of this host's runtime broker: the name
// configured in the global settings (see ConfiguredBrokerName), else the OS
// hostname, else fallback. Callers that send the broker name to the hub must
// use this rather than os.Hostname(), because the hub matches an existing
// broker by name: re-deriving the hostname would merge a broker registered
// under a custom name into another broker on the same host.
func LocalBrokerName(fallback string) string {
	globalDir, _ := GetGlobalDir()
	if n := ConfiguredBrokerName(globalDir); n != "" {
		return n
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return fallback
}
