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
	"github.com/GoogleCloudPlatform/scion/pkg/hostedguard"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// checkHostedRequestConfig refuses member-written config in a create or
// start request (a create's config volumes and its inline config, a start's
// inline config) on a hosted Runtime Broker. A create's config env is not
// checked: its values are used literally (never expanded or passed through
// from the broker's environment), and the broker drops or refuses the
// reserved names it must not take from it where it reads them. runtimeName
// returns the runtime the agent will run in; it is called only when there
// are volumes to check. A workstation broker checks nothing.
func (s *Server) checkHostedRequestConfig(runtimeName func() string, cfgs ...*api.ScionConfig) error {
	if s.config.WorkstationMode || !anyMemberContent(cfgs) {
		return nil
	}
	kind := ""
	if anyVolumes(cfgs) {
		kind = runtimeName()
		if isKubernetesRuntimeName(kind) {
			kind = hostedguard.RuntimeKubernetes
		}
	}
	for _, cfg := range cfgs {
		if err := hostedguard.CheckMemberConfig(cfg, hostedguard.Origin{Kind: hostedguard.OriginAgentConfig}, kind); err != nil {
			return err
		}
	}
	return nil
}

// anyMemberContent reports whether any of cfgs holds content hostedguard
// checks: volumes, env or a hub block.
func anyMemberContent(cfgs []*api.ScionConfig) bool {
	for _, cfg := range cfgs {
		if cfg != nil && (len(cfg.Volumes) > 0 || len(cfg.Env) > 0 || cfg.Hub != nil) {
			return true
		}
	}
	return false
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

// hostedConfigRefusalFrom reports whether err is (or wraps) refused member
// config raised inside Manager.Preflight, Provision or Start (a harness
// config's volumes on a hosted broker).
func hostedConfigRefusalFrom(err error) (*hostedguard.RefusalError, bool) {
	var re *hostedguard.RefusalError
	if errors.As(err, &re) {
		return re, true
	}
	return nil, false
}

// writeHostedConfigRefusalDetails writes re as writeHostedConfigRefusal
// does, with the operation's error details. Only re's own text is sent:
// what wraps it can name broker paths.
func writeHostedConfigRefusalDetails(w http.ResponseWriter, re *hostedguard.RefusalError, details map[string]interface{}) {
	writeError(w, http.StatusBadRequest, hostedguard.ErrorCode, re.Error(), details)
}

// workspaceNotPermittedFrom reports whether err is (or wraps) a typed
// workspace_not_permitted LaunchFailure raised on a hosted broker.
func workspaceNotPermittedFrom(err error) bool {
	var lf *scionrt.LaunchFailure
	return errors.As(err, &lf) && lf.Code == api.LaunchCodeWorkspaceNotPermitted
}

// writeWorkspaceNotPermitted writes a hosted explicit-workspace refusal as a
// 400 with the workspace_not_permitted code and fixed public text (never
// leaking broker host paths).
func writeWorkspaceNotPermitted(w http.ResponseWriter, details map[string]interface{}) {
	writeError(w, http.StatusBadRequest, api.LaunchCodeWorkspaceNotPermitted, launchFailureText[api.LaunchCodeWorkspaceNotPermitted], details)
}
