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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

// ErrCodeStorageInvalid marks an agent create, start or restart refused
// because the broker's project storage settings are invalid (422).
const ErrCodeStorageInvalid = api.BrokerErrCodeStorageInvalid

// healthCheckProjectStorage is the /healthz check that reports a broker
// whose project storage settings are invalid.
const healthCheckProjectStorage = "project_storage"

// storageInvalid reports whether the broker started with unusable project
// storage settings (ServerConfig.StorageError).
func (s *Server) storageInvalid() bool {
	return s.config.StorageError != nil
}

// refuseIfStorageInvalid answers 422 storage_invalid and reports true when
// the broker's project storage settings are invalid. op names the refused
// operation ("create", "start", "restart"). Callers run it before any
// tracker, registration or file work, so a refused request leaves nothing
// behind. Stop, delete, logs and status never call it.
func (s *Server) refuseIfStorageInvalid(w http.ResponseWriter, op, agentID string) bool {
	if !s.storageInvalid() {
		return false
	}
	msg := storageInvalidMessage(s.config.StorageError)
	s.agentLifecycleLog.Info("Agent "+op+" refused: project storage settings are invalid", "agent_id", agentID, "reason", msg)
	writeError(w, http.StatusUnprocessableEntity, ErrCodeStorageInvalid,
		"Failed to "+op+" agent: this runtime broker's project storage settings are invalid ("+msg+")", nil)
	return true
}

// storageInvalidMessage is the reason a refusal carries: the settings
// path of each invalid field, never the configured values (the requester
// may not be a hub admin; projectfs.ViewPublic). An error that is not a
// list of field errors is reported as the block being invalid; the startup
// log has the details.
func storageInvalidMessage(err error) string {
	var fields config.FieldErrorList
	if !errors.As(err, &fields) {
		return config.ProjectStorageKey + ": invalid; " + projectfs.LayoutIssuesRemediation
	}
	return projectfs.LayoutIssueSummary(fields.Issues(), projectfs.ViewPublic)
}

// InvalidStorageDescriptor returns the workspace storage descriptor of a
// broker whose project storage settings are invalid: the configured
// backend name, marked Invalid, and nothing else. No share, volume, homes
// or shared-dir placement is reported, since the broker places no files.
func InvalidStorageDescriptor(desc *api.BrokerWorkspaceStorage) *api.BrokerWorkspaceStorage {
	backend := api.WorkspaceStorageBackendLocal
	if desc != nil && desc.Backend != "" {
		backend = desc.Backend
	}
	return &api.BrokerWorkspaceStorage{Backend: backend, Invalid: true}
}
