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
	"context"
	"errors"
	"net/http"
	"strings"

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

// SetHostedHarnessConfigCheck sets the check hostedConfigPrecheck applies
// to the hub harness config an agent is dispatched with
// (Server.checkStoredHostedHarnessConfig). Nil checks nothing.
func (d *HTTPAgentDispatcher) SetHostedHarnessConfigCheck(check func(ctx context.Context, harnessConfigID, runtimeKind string) error) {
	d.hostedHarnessConfigCheck = check
}

// hostedConfigPrecheck refuses a dispatch on a hosted hub when the agent's
// stored inline config (written by the member who created or updated the
// agent), or the project-scope hub harness config it is dispatched with,
// names host resources, so config stored before the check existed is
// refused at its next dispatch. The runtime kind is the one the target
// broker reports for the agent's profile (hostedRuntimeKind); when it is not
// known, rules that depend on it are left to the broker.
func (d *HTTPAgentDispatcher) hostedConfigPrecheck(ctx context.Context, agent *store.Agent) error {
	if !d.hostedMode || agent == nil || agent.AppliedConfig == nil {
		return nil
	}
	kind := hostedRuntimeKind(ctx, d.store, agent)
	if err := hostedConfigRefusal(agent.AppliedConfig.InlineConfig, kind); err != nil {
		return err
	}
	if d.hostedHarnessConfigCheck == nil || agent.AppliedConfig.HarnessConfigID == "" {
		return nil
	}
	err := d.hostedHarnessConfigCheck(ctx, agent.AppliedConfig.HarnessConfigID, kind)
	var he *hostedHarnessConfigError
	if errors.As(err, &he) {
		return &RuntimeTargetRefusal{Code: hostedguard.ErrorCode, Status: http.StatusBadRequest, Message: he.Error()}
	}
	return err
}

// hostedConfigRefusal checks member-written agent config with
// hostedguard for runtimeKind ("" when not known) and returns its refusal
// as a 400 hosted_config_refused *RuntimeTargetRefusal, which create, start
// and restart relay as is.
func hostedConfigRefusal(cfg *api.ScionConfig, runtimeKind string) error {
	if err := hostedguard.CheckMemberConfig(cfg, hostedguard.Origin{Kind: hostedguard.OriginAgentConfig}, runtimeKind); err != nil {
		return &RuntimeTargetRefusal{Code: hostedguard.ErrorCode, Status: http.StatusBadRequest, Message: err.Error()}
	}
	return nil
}

// hostedRuntimeKind is the hostedguard runtime kind of the profile agent
// runs on at its broker, from the profile type the broker reports
// (store.BrokerProfile.Type, resolved like the dispatch's other profile
// checks: resolveAgentRuntimeProfileType): hostedguard.RuntimeKubernetes for
// a Kubernetes type, the reported type otherwise, and "" when the broker,
// the profile or its type is not known, which leaves gcs to the broker.
func hostedRuntimeKind(ctx context.Context, st store.Store, agent *store.Agent) string {
	if agent == nil || agent.RuntimeBrokerID == "" {
		return ""
	}
	broker, err := st.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil || broker == nil {
		return ""
	}
	profile := ""
	if agent.AppliedConfig != nil {
		profile = agent.AppliedConfig.Profile
	}
	_, typ, ok := resolveAgentRuntimeProfileType(broker, profile)
	typ = strings.ToLower(strings.TrimSpace(typ))
	switch {
	case !ok || typ == "":
		return ""
	case isKubernetesRuntimeType(typ):
		return hostedguard.RuntimeKubernetes
	default:
		return typ
	}
}
