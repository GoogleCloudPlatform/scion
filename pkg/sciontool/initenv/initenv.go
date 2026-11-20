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

// Package initenv loads and normalizes container startup environment policy
// for sciontool init before hooks or startup child processes run.
package initenv

import (
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/envpolicy"
)

// Policy holds the startup environment filtering configuration loaded by
// sciontool init.
type Policy struct {
	// Mode is the active hook environment filtering mode (currently always
	// envpolicy.HookEnvModeDenylist).
	Mode string

	// Withheld is the effective set of environment variable names withheld
	// from lifecycle hooks (static denylist union per-start marker keys).
	Withheld envpolicy.Set
}

// Load reads and unsets the runtime-provided withheld-key marker from the
// process environment, returning the effective Policy. When the marker is
// absent or empty, only the static denylist applies. Unknown SCION_HOOK_*
// variables are ignored.
func Load() Policy {
	raw := os.Getenv(envpolicy.HookWithheldEnvVar)
	_ = os.Unsetenv(envpolicy.HookWithheldEnvVar)

	received := envpolicy.ParseWithheldMarker(raw)
	return Policy{
		Mode:     envpolicy.HookEnvModeDenylist,
		Withheld: envpolicy.NewSet(received...),
	}
}
