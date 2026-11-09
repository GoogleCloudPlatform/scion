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
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// This file holds the hub's side of running a Cloud Run instance as its
// agent's own GCP identity (hub.cloudrun_native_identity). The hub alone
// decides whether a dispatch is "native": at each dispatch it decides from
// its stored broker row (cloudRunNativeLive), records the decision on the
// agent (AgentAppliedConfig.NativeCloudRun) before the broker call, and
// sends the broker a marker only when it decided native
// (RemoteGCPIdentityConfig.NativeCloudRun on create,
// SCION_METADATA_NATIVE_CLOUDRUN in the resolved env on start and restart).
// The broker runs an assign dispatch natively only with that marker.
//
// For a natively assigned agent the instance's metadata server holds its
// token, so the hub neither puts the GCP token scope on the agent's token
// nor mints a GCP token for it. The mint endpoints, the metadata emulator
// and the SCION_METADATA_* env stay in place for every other agent.

// cloudRunProfileType is the stored profile Type of a Cloud Run profile on
// the runtimes.<entry>.cloudrun form, the only form that can report native
// identity support.
const cloudRunProfileType = "cloudrun"

// envNativeCloudRun is the resolved-env marker of a start or restart the
// hub decided native. Like every SCION_* key it is reserved
// (secret.IsReservedEnvTarget) and never persisted from a resolved env
// (shouldPersistResolvedEnvKey).
const envNativeCloudRun = "SCION_METADATA_NATIVE_CLOUDRUN"

// msgGCPMintNativeIdentity is the refusal of a GCP token request from a
// natively assigned agent. It names no account.
const msgGCPMintNativeIdentity = "the hub does not issue GCP tokens for agents on this Cloud Run Instances profile; use the instance's metadata server"

// cloudRunProfileReportsNativeIdentity reports whether p is a Cloud Run
// profile (Type cloudrun) whose broker reported, at report version
// cloudRunSAReportMinVersion or later, that it runs instances as the
// identity in their dispatch. The report's age does not matter here: a
// stale report that says so still means the hub withholds and refuses
// GCP tokens.
func cloudRunProfileReportsNativeIdentity(p *store.BrokerProfile) bool {
	return p != nil &&
		p.Type == cloudRunProfileType &&
		p.MappingsReported &&
		p.MappingsReportVersion >= cloudRunSAReportMinVersion &&
		p.CloudRunNativeIdentity
}

// brokerRunsNativeCloudRun reports whether any of broker's profiles reports
// native identity support (cloudRunProfileReportsNativeIdentity).
func brokerRunsNativeCloudRun(broker *store.RuntimeBroker) bool {
	if broker == nil {
		return false
	}
	for i := range broker.Profiles {
		if cloudRunProfileReportsNativeIdentity(&broker.Profiles[i]) {
			return true
		}
	}
	return false
}

// brokerProfileNativelyAssigns reports whether an agent on profileName on
// broker is natively assigned (the caller checks the experiment):
//
//   - the profile resolves (resolveAgentRuntimeProfileType): its Type is
//     cloudrun and its report says the broker supports it;
//   - the profile does not resolve, for example because it has left the
//     broker row: the broker runs native Cloud Run for some profile
//     (brokerRunsNativeCloudRun), since it may resolve this one to it.
//     The dispatch precheck refuses such a dispatch
//     (cloudRunRefusalProfileUnknown); this keeps the GCP token path closed
//     for an agent already running when its profile stops resolving.
func brokerProfileNativelyAssigns(broker *store.RuntimeBroker, profileName string) bool {
	if broker == nil {
		return false
	}
	name, typ, ok := resolveAgentRuntimeProfileType(broker, profileName)
	if !ok {
		return brokerRunsNativeCloudRun(broker)
	}
	if typ != cloudRunProfileType {
		return false
	}
	for i := range broker.Profiles {
		if broker.Profiles[i].Name == name {
			return cloudRunProfileReportsNativeIdentity(&broker.Profiles[i])
		}
	}
	return false
}

// cloudRunNativeLive is the hub's decision, from its stored broker row,
// whether agent is natively assigned at this moment: gateOn (the
// hub.cloudrun_native_identity experiment) and brokerProfileNativelyAssigns
// for the agent's broker and profile. With gateOn false it reads nothing.
// An agent with no broker, or whose broker row is gone, is not natively
// assigned; any other lookup error is returned, and callers refuse on it.
func cloudRunNativeLive(ctx context.Context, st store.Store, gateOn bool, agent *store.Agent) (bool, error) {
	if !gateOn || agent == nil || agent.RuntimeBrokerID == "" || st == nil {
		return false, nil
	}
	broker, err := st.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	profileName := ""
	if agent.AppliedConfig != nil {
		profileName = agent.AppliedConfig.Profile
	}
	return brokerProfileNativelyAssigns(broker, profileName), nil
}

// agentRecordedNativeCloudRun reports whether agent's record carries the
// hub's recorded native decision (AgentAppliedConfig.NativeCloudRun).
func agentRecordedNativeCloudRun(agent *store.Agent) bool {
	return agent != nil && agent.AppliedConfig != nil && agent.AppliedConfig.NativeCloudRun
}

// agentCloudRunNativelyAssigned reports whether agent is natively assigned
// for the GCP token decisions: its record carries the hub's recorded
// native decision (kept until a confirmed stop or delete, whatever the
// experiment), or the live decision says so (cloudRunNativeLive). It never
// reads a token. A lookup error is returned, and callers refuse on it.
func (s *Server) agentCloudRunNativelyAssigned(ctx context.Context, agent *store.Agent) (bool, error) {
	if agentRecordedNativeCloudRun(agent) {
		return true, nil
	}
	return cloudRunNativeLive(ctx, s.store, s.experimentEnabled(experiments.CloudRunNativeIdentity), agent)
}

// cloudRunNativeKey carries a dispatch's native decision on its context
// (withCloudRunNative), from markCloudRunNative to the request builders.
type cloudRunNativeKey struct{}

func withCloudRunNative(ctx context.Context) context.Context {
	return context.WithValue(ctx, cloudRunNativeKey{}, true)
}

// cloudRunNativeFrom reports whether this dispatch was decided native. It
// is the only source of the marker the broker receives: never the stored
// flag, the agent's config, the create request or a template.
func cloudRunNativeFrom(ctx context.Context) bool {
	native, _ := ctx.Value(cloudRunNativeKey{}).(bool)
	return native
}

// markCloudRunNative decides, before any token, run or broker call,
// whether this dispatch of agent is native (cloudRunNativeLive with the
// experiment as the dispatch sends it) and returns ctx carrying the
// decision. A native decision is always written to the agent's row first
// (Store.SetAgentNativeCloudRun, whatever the caller's copy says, since the
// row may have been cleared since that copy was read), then to agent
// itself. A non-native one leaves any recorded flag as it is. A lookup or
// store failure refuses the dispatch, marked as not sent, with no marker:
// that includes a row that is missing or soft-deleted (ErrNotFound) and an
// agent ID the store cannot parse (ErrInvalidInput).
func (d *HTTPAgentDispatcher) markCloudRunNative(ctx context.Context, agent *store.Agent) (context.Context, error) {
	gateOn := slices.Contains(d.dispatchExperiments(), experiments.CloudRunNativeIdentity)
	if d.store == nil {
		// Never a nil store call mid-dispatch: with the experiment on, a
		// dispatcher without a store cannot decide or record, so it refuses.
		if gateOn {
			return ctx, fmt.Errorf("%w: deciding the Cloud Run identity of agent %s: no store", errStartRequestNotSent, agent.ID)
		}
		return ctx, nil
	}
	native, err := cloudRunNativeLive(ctx, d.store, gateOn, agent)
	if err != nil {
		return ctx, fmt.Errorf("%w: deciding the Cloud Run identity of agent %s: %w", errStartRequestNotSent, agent.ID, err)
	}
	if !native {
		return ctx, nil
	}
	if err := d.store.SetAgentNativeCloudRun(ctx, agent.ID, true); err != nil {
		return ctx, fmt.Errorf("%w: recording the Cloud Run identity of agent %s: %w", errStartRequestNotSent, agent.ID, err)
	}
	if agent.AppliedConfig != nil {
		agent.AppliedConfig.NativeCloudRun = true
	}
	return withCloudRunNative(ctx), nil
}

// clearCloudRunNative removes the recorded native decision after the hub
// confirmed a stop or delete of agent, when the caller's copy shows one (a
// stop or delete of an agent with no decision writes nothing). A copy read
// before the decision was recorded leaves it set, and so does a failure
// (logged): either way the GCP token path stays closed until a later
// confirmed stop or delete.
func (d *HTTPAgentDispatcher) clearCloudRunNative(ctx context.Context, agent *store.Agent) {
	if !agentRecordedNativeCloudRun(agent) || d.store == nil {
		return
	}
	if err := d.store.SetAgentNativeCloudRun(ctx, agent.ID, false); err != nil && !errors.Is(err, store.ErrNotFound) {
		d.log.Warn("clearing the recorded Cloud Run identity after a confirmed stop or delete failed; GCP tokens stay withheld",
			"agent_id", agent.ID, "error", err)
		return
	}
	agent.AppliedConfig.NativeCloudRun = false
}

// applyNativeCloudRunEnv sets envNativeCloudRun in a dispatch's resolved
// env when the dispatch was decided native, and deletes it otherwise, so
// no stored, secret or template value of that name reaches the broker. It
// runs in the same last-applied step as the hub's metadata mode env.
func applyNativeCloudRunEnv(env map[string]string, cls *map[string]api.EnvKind, native bool) {
	if env == nil {
		return
	}
	if !native {
		delete(env, envNativeCloudRun)
		if cls != nil && *cls != nil {
			delete(*cls, envNativeCloudRun)
		}
		return
	}
	env[envNativeCloudRun] = "1"
	if cls != nil {
		classifyEnv(cls, envNativeCloudRun, api.EnvKindPlain)
	}
}

// withoutNativeIdentityGCPScopes returns scopes without the GCP token
// scopes when agent is natively assigned, so its token never carries one.
// Scopes with no GCP token scope are returned as they are, with no lookup.
func (s *Server) withoutNativeIdentityGCPScopes(ctx context.Context, agent *store.Agent, scopes []AgentTokenScope) ([]AgentTokenScope, error) {
	hasGCP := false
	for _, scope := range scopes {
		if strings.HasPrefix(string(scope), ScopeGCPTokenPrefix) {
			hasGCP = true
			break
		}
	}
	if !hasGCP {
		return scopes, nil
	}
	native, err := s.agentCloudRunNativelyAssigned(ctx, agent)
	if err != nil {
		return nil, err
	}
	if !native {
		return scopes, nil
	}
	out := make([]AgentTokenScope, 0, len(scopes))
	for _, scope := range scopes {
		if !strings.HasPrefix(string(scope), ScopeGCPTokenPrefix) {
			out = append(out, scope)
		}
	}
	return out, nil
}

// refuseCloudRunNativeMint writes the refusal of a GCP token request from
// a natively assigned agent and returns true; it returns false, writing
// nothing, for any other agent. The check runs on the stored agent record
// (its recorded native decision) and the hub's live decision, so it
// refuses even when the presented token still carries the GCP token scope
// (a token issued while the experiment was off). A lookup error refuses
// too.
func (s *Server) refuseCloudRunNativeMint(ctx context.Context, w http.ResponseWriter, agent *store.Agent) bool {
	native, err := s.agentCloudRunNativelyAssigned(ctx, agent)
	if err != nil {
		slog.Error("GCP token request: checking the agent's Cloud Run identity failed", "agent_id", agent.ID, "error", err)
		InternalError(w)
		return true
	}
	if !native {
		return false
	}
	slog.Info("GCP token request refused: the agent is natively assigned on Cloud Run Instances", "agent_id", agent.ID)
	writeError(w, http.StatusForbidden, ErrCodeForbidden, msgGCPMintNativeIdentity, nil)
	return true
}
