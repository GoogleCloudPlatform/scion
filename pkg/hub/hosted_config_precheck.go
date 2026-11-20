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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hostedguard"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// SetHostedMode makes the dispatcher check member-written agent config
// before every create, provision, start and restart dispatch
// (hostedConfigPrecheck). Set on a hosted hub (not workstation mode),
// whatever mode the receiving broker runs in.
func (d *HTTPAgentDispatcher) SetHostedMode(hosted bool) {
	d.hostedMode = hosted
}

// hostedConfigPrecheck refuses a dispatch on a hosted hub when the agent's
// stored inline config (written by the member who created or updated the
// agent) names host resources, so config stored before the check existed
// is refused at its next dispatch. The runtime is not known here: rules
// that depend on it are left to the broker.
func (d *HTTPAgentDispatcher) hostedConfigPrecheck(agent *store.Agent) error {
	if !d.hostedMode || agent == nil || agent.AppliedConfig == nil {
		return nil
	}
	return hostedConfigRefusal(agent.AppliedConfig.InlineConfig)
}

// hostedConfigRefusal checks member-written agent config with
// hostedguard and returns its refusal as a 400 hosted_config_refused
// *RuntimeTargetRefusal, which create, start and restart relay as is.
func hostedConfigRefusal(cfg *api.ScionConfig) error {
	if err := hostedguard.CheckMemberConfig(cfg, hostedguard.Origin{Kind: hostedguard.OriginAgentConfig}, ""); err != nil {
		return &RuntimeTargetRefusal{Code: hostedguard.ErrorCode, Status: http.StatusBadRequest, Message: err.Error()}
	}
	return nil
}
