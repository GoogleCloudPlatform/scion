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

	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// ErrCodeHarnessConfigUnusable is the error code (and async launch error
// code) for a harness-config whose provisioner cannot run
// (harness.ErrUnusableProvisioner, ptone/scion#611). It is a configuration
// error the caller must fix; retrying does not help.
const ErrCodeHarnessConfigUnusable = "harness_config_unusable"

// unusableProvisionerFrom reports whether err is (or wraps) an
// *harness.UnusableProvisionerError.
func unusableProvisionerFrom(err error) (*harness.UnusableProvisionerError, bool) {
	var ue *harness.UnusableProvisionerError
	if errors.As(err, &ue) {
		return ue, true
	}
	return nil, false
}

// writeUnusableProvisioner answers an unusable harness-config provisioner
// with 422 harness_config_unusable and the error's public message (name,
// reason, fix; no broker filesystem paths). The full error, with the
// harness-config directory, goes to the broker log only.
func (s *Server) writeUnusableProvisioner(w http.ResponseWriter, ue *harness.UnusableProvisionerError, op, agentID string, details map[string]interface{}) {
	s.agentLifecycleLog.Warn("Harness-config provisioner is not usable",
		"op", op, "agent_id", agentID, "harness_config", ue.Name, "error", ue.Error())
	writeError(w, http.StatusUnprocessableEntity, ErrCodeHarnessConfigUnusable, "Failed to "+op+": "+ue.PublicMessage(), details)
}
