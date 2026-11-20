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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hostedguard"
)

// checkHostedRequestConfig refuses member-written config in a create or
// start request (a create's config and inline config, a start's inline
// config) on a hosted Runtime Broker. runtimeName returns the runtime the
// agent will run in; it is called only when there is config to check. A
// workstation broker checks nothing.
func (s *Server) checkHostedRequestConfig(runtimeName func() string, cfgs ...*api.ScionConfig) error {
	if s.config.WorkstationMode || !anyVolumes(cfgs) {
		return nil
	}
	kind := runtimeName()
	if isKubernetesRuntimeName(kind) {
		kind = hostedguard.RuntimeKubernetes
	}
	for _, cfg := range cfgs {
		if err := hostedguard.CheckMemberConfig(cfg, hostedguard.Origin{Kind: hostedguard.OriginAgentConfig}, kind); err != nil {
			return err
		}
	}
	return nil
}

func anyVolumes(cfgs []*api.ScionConfig) bool {
	for _, cfg := range cfgs {
		if cfg != nil && len(cfg.Volumes) > 0 {
			return true
		}
	}
	return false
}

// writeHostedConfigRefusal writes refused member config as a 400 with the
// hosted_config_refused code. The message names the field and the entry
// as requested, never a host path or a value.
func writeHostedConfigRefusal(w http.ResponseWriter, err error) {
	writeError(w, http.StatusBadRequest, hostedguard.ErrorCode, err.Error(), nil)
}
