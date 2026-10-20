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
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// The server reads its versioned settings at startup from the global
// settings directory only (ptone/scion#5362). An empty path would resolve a
// project from the server's working directory and merge that project's
// settings over the global ones, so where the server was started would
// change its plugins, notification channels, maintenance settings, broker
// identity and profiles. These loaders pass the global directory instead;
// each is otherwise the loader the call site used before, so the settings
// overlay applies exactly where it applied before (LoadEffectiveSettings
// applies it, LoadVersionedSettings does not).

// loadServerVersionedSettings is config.LoadVersionedSettings for the
// global settings directory.
func loadServerVersionedSettings() (*config.VersionedSettings, error) {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return nil, fmt.Errorf("resolving the global settings directory: %w", err)
	}
	return config.LoadVersionedSettings(globalDir)
}

// loadServerEffectiveSettings is config.LoadEffectiveSettings for the
// global settings directory, with the installed settings overlay applied.
func loadServerEffectiveSettings() (*config.VersionedSettings, []string, error) {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return nil, nil, fmt.Errorf("resolving the global settings directory: %w", err)
	}
	return config.LoadEffectiveSettings(globalDir)
}

// serverRuntimeProjectPath is the project path the server passes to
// runtime.GetRuntime for the co-located broker's default runtime, at
// startup and when the settings reload swaps the runtime: the global
// project, so the default runtime never comes from a project found from the
// working directory. It is the "global" alias rather than the resolved
// global directory because runtime.GetRuntime takes a project path and
// resolves it itself (config.ResolveProjectPath maps "global" to the
// global directory); the settings loaders above take the directory.
const serverRuntimeProjectPath = "global"
