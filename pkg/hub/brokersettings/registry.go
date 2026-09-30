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

// Package brokersettings is the key registry for the general per-broker
// settings mechanism (ptone/scion#2061 P2, ptone/scion#2177, design.md
// §5.1). Each field of store.BrokerSettings corresponds to exactly one
// KeyDef here, keyed by its JSON name. Adding a new per-broker setting is
// one new struct field on store.BrokerSettings plus one new KeyDef: the GET
// and PUT handlers (pkg/hub/broker_settings_handlers.go) work generically
// off this registry, rejecting any key not listed here with 400.
package brokersettings

import "fmt"

// KeyDef describes one key in the broker settings document.
type KeyDef struct {
	// Name is the JSON key, exactly as it appears in the settings document
	// (design.md §5.4: `{"settings": {"<Name>": ...}}`).
	Name string
	// Description is shown in the web UI and API docs.
	Description string
	// Permission is the permission required to write this key (design.md
	// §5.3). Declared per key, rather than once for the whole endpoint, so a
	// future low-stakes setting could use a narrower permission (e.g.
	// broker.update, editable by the broker's own owner) than the hub-admin
	// -only quota.update that maxAgents requires.
	Permission string
}

// MaxAgents is the first, and for P2.1 the only, registered key: a
// per-broker override of the max_agents_per_broker quota. nil means
// "inherit" (fall through to the entitlement engine / hub-wide default); 0
// means unlimited; a positive value may be lower than the hub-wide default
// or any system-scoped entitlement binding (design.md §5.2, P2-D3).
var MaxAgents = KeyDef{
	Name:        "maxAgents",
	Description: "Maximum concurrently live agents on this broker. 0 means unlimited; unset inherits the hub-wide default.",
	Permission:  "quota.update",
}

// registry indexes every known key by name, for the "unknown key -> 400"
// check in the PUT handler.
var registry = map[string]KeyDef{
	MaxAgents.Name: MaxAgents,
}

// Lookup returns the KeyDef for name, or ok=false if name is not a known
// broker-settings key.
func Lookup(name string) (def KeyDef, ok bool) {
	def, ok = registry[name]
	return def, ok
}

// ValidateMaxAgents validates a decoded maxAgents value. nil (inherit) is
// always valid; otherwise it must be >= 0 (0 means unlimited).
func ValidateMaxAgents(value *int64) error {
	if value != nil && *value < 0 {
		return fmt.Errorf("maxAgents must be >= 0 (0 means unlimited)")
	}
	return nil
}
