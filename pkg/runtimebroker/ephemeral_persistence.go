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
	"context"
	"fmt"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
)

// ephemeralPersistenceRefusal refuses a dispatch carrying ephemeral
// persistence (ptone/scion#7780) unless the hub sent the
// hub.cloudrun_ephemeral_agents experiment with it: start and restart attach
// the hub defaults to ctx, create carries them in its request config. Any
// other persistence value than ephemeral or empty is refused too. The
// runtime itself refuses ephemeral when it cannot honour it
// (runtime.CheckPersistenceSupported).
func ephemeralPersistenceRefusal(ctx context.Context, in startContextInputs) *startContextError {
	if err := in.Persistence.Validate(); err != nil {
		return &startContextError{Status: http.StatusBadRequest, Message: fmt.Sprintf("agent %q: %v", in.Name, err)}
	}
	if !in.Persistence.IsEphemeral() {
		return nil
	}
	if api.HubAgentDefaultsFromContext(ctx).ExperimentEnabled(experiments.CloudRunEphemeralAgents) ||
		(in.Config != nil && in.Config.HubAgentDefaults.ExperimentEnabled(experiments.CloudRunEphemeralAgents)) {
		return nil
	}
	return &startContextError{
		Status:  http.StatusPreconditionFailed,
		Code:    "experiment_disabled",
		Message: fmt.Sprintf("agent %q is ephemeral, but the hub did not enable experiment %s for this dispatch; it cannot start until the experiment is on", in.Name, experiments.CloudRunEphemeralAgents),
		Details: map[string]interface{}{"experiment": experiments.CloudRunEphemeralAgents},
	}
}
