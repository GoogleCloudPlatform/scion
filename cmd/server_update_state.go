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
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
)

// maintenanceInstallRoot resolves only the maintenance install root
// (SCION_MAINTENANCE_INSTALL_ROOT > server.maintenance.install_root >
// /opt/scion), the same value resolveMaintenanceConfig sets, without the
// rest of the maintenance config.
func maintenanceInstallRoot() string {
	var maint *config.V1MaintenanceConfig
	if vs, err := loadServerVersionedSettings(); err == nil && vs.Server != nil {
		maint = vs.Server.Maintenance
	}
	return config.MaintenanceInstallRoot(maint)
}

// markUpdateStateBooting records, in <install_root>/update-state.json, that
// this binary is about to touch the database of an update that targets it:
// migrated=true and previous_safe against this binary's DowngradeFloor. It
// runs before the store is opened (initStore), so before the first
// migration statement; a crash anywhere after it counts as migrated for
// the boot-failure guard. Best-effort: a failure is logged and startup
// goes on.
func markUpdateStateBooting(installRoot string) {
	changed, st, err := maintenance.MarkUpdateStateBooting(installRoot, version.Version, version.DowngradeFloor)
	switch {
	case err != nil:
		slog.Warn("Failed to update the update state before migrations", "install_root", installRoot, "error", err)
	case changed:
		slog.Info("Update state: booting the update target", "target", st.Target, "from", st.From,
			"run_id", st.RunID, "previous_safe", st.PreviousSafe)
	}
}
