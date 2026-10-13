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

package logging

import "sync/atomic"

// appliedHubName is the hub name the hub applied from its operational
// settings, or nil when none has been set. Log handlers are built during
// early logging init, before stored settings are loaded, from a startup
// value; once set, this value replaces it in the "hub" label of every record
// (ptone/scion#4903).
var appliedHubName atomic.Pointer[string]

// SetHubName records the hub name the hub applied from its operational
// settings, so every log handler labels its records with it instead of the
// name it was built with. An empty name clears it, and handlers return to
// their own name.
func SetHubName(name string) {
	if name == "" {
		appliedHubName.Store(nil)
		return
	}
	appliedHubName.Store(&name)
}

// addHubLabel sets the "hub" label to the applied hub name, or when none is
// set to startup, the name the handler was built with. No label is set when
// both are empty.
func addHubLabel(labels map[string]string, startup string) {
	name := startup
	if p := appliedHubName.Load(); p != nil {
		name = *p
	}
	if name != "" {
		labels["hub"] = name
	}
}
