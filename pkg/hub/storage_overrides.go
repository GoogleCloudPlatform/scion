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

package hub

import (
	"errors"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// storageOverridesCheck is the health check key of a hub whose settings
// still carry a removed per-runtime or per-profile storage override
// (ptone/scion#6371). It is not in criticalHealthChecks: the hub keeps
// running and only agents of the affected profiles are refused at start,
// so it makes the hub degraded (the diagnostics banner shows it), never
// unhealthy.
const storageOverridesCheck = "storage_overrides"

// removedStorageOverrideErrors returns the removed storage overrides this
// hub's settings carry, one message per source: the global settings file
// as read at startup (ServerConfig.RemovedStorageOverridesFileError), and
// the stored runtimes and profiles of the operational settings, read now so
// the entry clears as soon as an admin save removes them. Messages are the
// public form (no claim or subpath root). Nil when there are none.
func (s *Server) removedStorageOverrideErrors() []string {
	var out []string
	if msg := s.config.RemovedStorageOverridesFileError; msg != "" {
		out = append(out, msg)
	}
	if ops := s.GetOperationalSettings(); ops != nil {
		snap := ops.Snapshot()
		err := config.RemovedStorageOverridesError(config.HubStoredSettingsSource, snap.Runtimes, snap.Profiles, nil)
		var se *config.StorageMigrationRequiredError
		if errors.As(err, &se) {
			out = append(out, se.PublicMessage())
		}
	}
	return out
}

// checkRemovedStorageOverrides reports a hub whose settings carry a
// removed storage override. The text is fixed (no profile or runtime
// names); GET /api/v1/admin/server-config lists them in
// storage_override_errors, and the hub startup log has the full message.
func (s *Server) checkRemovedStorageOverrides(checks map[string]string) {
	if len(s.removedStorageOverrideErrors()) == 0 {
		return
	}
	checks[storageOverridesCheck] = "degraded: removed storage overrides are set; agents of the affected profiles are refused at start " +
		"(see storage_override_errors in the admin server config, or the hub startup log; " +
		"stored settings clear on save, a settings-file fix clears when the hub restarts)"
}
