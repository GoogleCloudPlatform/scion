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

package runtimebroker

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// ErrCodeStorageMigrationRequired is the error code (and async launch
// error code) of an agent start refused because its storage would be
// placed differently than configured before server.project_storage
// (config.StorageMigrationRequiredError, ptone/scion#6371). Like
// ErrCodeHarnessConfigUnusable it is a configuration error an operator
// must fix; retrying does not help.
const ErrCodeStorageMigrationRequired = api.BrokerErrCodeStorageMigrationRequired

// storageMigrationRequiredFrom reports whether err is (or wraps) a
// *config.StorageMigrationRequiredError.
func storageMigrationRequiredFrom(err error) (*config.StorageMigrationRequiredError, bool) {
	var se *config.StorageMigrationRequiredError
	if errors.As(err, &se) {
		return se, true
	}
	return nil, false
}

// writeStorageMigrationRequired answers a refused start with 422
// storage_migration_required and the refusal's public message (the keys
// and the migration, without the share's claim or subpath root). The full
// error goes to the broker log only.
func (s *Server) writeStorageMigrationRequired(w http.ResponseWriter, se *config.StorageMigrationRequiredError, op, agentID string, details map[string]interface{}) {
	s.agentLifecycleLog.Error("Agent start refused: storage migration required",
		"op", op, "agent_id", agentID, "error", se.Error())
	writeError(w, http.StatusUnprocessableEntity, ErrCodeStorageMigrationRequired, "Failed to "+op+": "+se.PublicMessage(), details)
}

// storageOverridesCheck is the /healthz check key of a broker whose
// settings carry a removed storage override; storageOverridesCheckDegraded
// is its fixed value (no profile names: the broker log has them).
const (
	storageOverridesCheck         = "storage_overrides"
	storageOverridesCheckDegraded = "degraded: removed storage overrides are set; agents of the affected profiles are refused at start (see the broker startup log)"
)

// storageOverridesCacheTTL bounds how often /healthz re-reads the settings
// for the storage overrides check. A variable so tests can disable it.
var storageOverridesCacheTTL = 30 * time.Second

// storageOverridesCache holds the last StorageOverridesCheck result.
type storageOverridesCache struct {
	mu  sync.Mutex
	at  time.Time
	err error
}

// storageOverridesError returns the result of ServerConfig.StorageOverridesCheck,
// re-evaluated at most every storageOverridesCacheTTL (health probes are
// frequent; the check reads the settings file), or nil when no check is set.
func (s *Server) storageOverridesError() error {
	check := s.config.StorageOverridesCheck
	if check == nil {
		return nil
	}
	c := &s.storageOverrides
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < storageOverridesCacheTTL {
		return c.err
	}
	c.err, c.at = check(), time.Now()
	return c.err
}
