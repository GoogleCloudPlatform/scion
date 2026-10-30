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
	"errors"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// reportRemovedStorageOverrides logs, as an ERROR through logf, the removed
// per-runtime or per-profile storage overrides that runtimes and profiles
// (from source) still carry, with the migration text, and returns the
// error (nil when there are none). Process start reports them and keeps
// running: agent start refuses the affected profiles
// (ptone/scion#6371). sd is server.shared_dir_storage, for the migration
// text's claim and subpath root.
func reportRemovedStorageOverrides(logf func(format string, args ...interface{}), source string,
	runtimes map[string]config.V1RuntimeConfig, profiles map[string]config.V1ProfileConfig, sd *config.V1SharedDirStorageConfig) error {
	err := config.RemovedStorageOverridesError(source, runtimes, profiles, sd)
	if err != nil {
		logf("ERROR: %v", err)
	}
	return err
}

// reportRemovedStorageOverridesIn is reportRemovedStorageOverrides over a
// loaded settings file. A nil vs reports nothing.
func reportRemovedStorageOverridesIn(logf func(format string, args ...interface{}), source string, vs *config.VersionedSettings) error {
	if vs == nil {
		return nil
	}
	var sd *config.V1SharedDirStorageConfig
	if vs.Server != nil {
		sd = vs.Server.SharedDirStorage
	}
	return reportRemovedStorageOverrides(logf, source, vs.Runtimes, vs.Profiles, sd)
}

// brokerStorageOverridesCheck is the runtime broker's
// StorageOverridesCheck: the removed storage overrides of the global
// settings as agent starts read them (the file with any hub settings
// overlay). Settings that cannot be loaded report nothing here; the start
// path handles them.
func brokerStorageOverridesCheck() error {
	vs, _, err := config.LoadGlobalSettingsWithOverlay()
	if err != nil {
		return nil
	}
	return vs.RemovedStorageOverridesError(config.GlobalSettingsSource)
}

// hubRemovedStorageOverridesFileError is the public message of the removed
// storage overrides in the global settings file the hub starts with, after
// logging the full error, or "" when there are none or the file cannot be
// loaded (startup reports those problems elsewhere).
func hubRemovedStorageOverridesFileError(logf func(format string, args ...interface{})) string {
	vs, _, err := config.LoadGlobalSettings()
	if err != nil {
		return ""
	}
	var se *config.StorageMigrationRequiredError
	if errors.As(reportRemovedStorageOverridesIn(logf, config.GlobalSettingsSource, vs), &se) {
		return se.PublicMessage()
	}
	return ""
}
