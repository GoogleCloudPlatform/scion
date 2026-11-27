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
	"fmt"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
)

// experimentRegistry returns the registry this server resolves experiments
// against. Production never sets ServerConfig.Experiments, so this falls
// back to experiments.Default(). Every reader below goes through this
// accessor, never the s.experiments field directly, because many pkg/hub
// tests build &Server{...} by struct literal without New() (ptone/scion#2217).
func (s *Server) experimentRegistry() *experiments.Registry {
	s.experimentsMu.RLock()
	reg := s.experiments
	s.experimentsMu.RUnlock()
	if reg == nil {
		return experiments.Default()
	}
	return reg
}

// setExperimentRegistry swaps the server's compiled experiment registry
// safely with respect to concurrent experimentRegistry() readers.
func (s *Server) setExperimentRegistry(reg *experiments.Registry) {
	s.experimentsMu.Lock()
	s.experiments = reg
	s.experimentsMu.Unlock()
}

// experimentsSnapshot returns the cached experiments settings view, or an
// empty (no-overrides, not malformed) snapshot when OperationalSettings is
// unavailable.
func (s *Server) experimentsSnapshot() ExperimentsSnapshot {
	ops := s.GetOperationalSettings()
	if ops == nil {
		return ExperimentsSnapshot{Overrides: map[string]bool{}}
	}
	return ops.ExperimentsSnapshot()
}

// experimentEnabled reports whether the named experiment is enabled right
// now. Non-route code calls this at its decision point; the value is never
// cached at startup.
func (s *Server) experimentEnabled(name string) bool {
	return s.experimentEnabledIn(s.experimentsSnapshot(), name)
}

// conduitDeploymentDefault holds a startup-computed deployment default for
// hub.conduit (SetConduitDeploymentDefault).
type conduitDeploymentDefault struct {
	on     bool
	reason string
}

// SetConduitDeploymentDefault sets the deployment default for hub.conduit
// (and its diagnostic reason when off). Call once at startup after
// newCommandBus and before startConduit. Stored overrides and a malformed
// experiments document still take precedence.
func (s *Server) SetConduitDeploymentDefault(on bool, reason string) {
	s.conduitDeploymentDefault.Store(&conduitDeploymentDefault{
		on:     on,
		reason: reason,
	})
}

// ConduitDeploymentDefaultReason returns the diagnostic reason recorded by
// SetConduitDeploymentDefault, or "" when no deployment default was set.
func (s *Server) ConduitDeploymentDefaultReason() string {
	if def := s.conduitDeploymentDefault.Load(); def != nil {
		return def.reason
	}
	return ""
}

// SetConduitEarlyResolveGuardForTest arms or disarms a test-only guard that
// panics if hub.conduit is resolved before SetConduitDeploymentDefault runs.
func (s *Server) SetConduitEarlyResolveGuardForTest(on bool) {
	s.conduitGuardEarlyResolve.Store(on)
}

// ConduitOverrideState reports the stored override state of hub.conduit
// without resolving its effective value or reading the deployment default:
//   - val, true, false when a non-malformed experiments document has an
//     explicit override for hub.conduit;
//   - false, false, true when the experiments document is malformed;
//   - false, false, false when no override is stored.
//
// Startup code that runs before SetConduitDeploymentDefault (such as
// newCommandBus) uses this to detect an explicit operator opt-in without
// resolving hub.conduit early.
func (s *Server) ConduitOverrideState() (val, explicit, malformed bool) {
	snap := s.experimentsSnapshot()
	if snap.Malformed {
		return false, false, true
	}
	v, ok := snap.Overrides[conduitExperiment]
	return v, ok, false
}

// experimentEnabledIn is the single resolution function. Callers that report
// several values (resolvedExperiments, the admin responses in 1a-ii) take one
// snapshot and resolve every name from it, so a concurrent refresh cannot
// produce a mixed answer (ptone/scion#2217).
func (s *Server) experimentEnabledIn(snap ExperimentsSnapshot, name string) bool {
	if name == conduitExperiment && s.conduitGuardEarlyResolve.Load() && s.conduitDeploymentDefault.Load() == nil {
		panic("hub.conduit resolved before SetConduitDeploymentDefault")
	}
	overrides := snap.Overrides
	if _, set := overrides[name]; !set {
		if def, ok := s.deploymentExperimentDefault(name); ok {
			// A deployment default stands in for the compiled default
			// only; a stored override and a malformed document still win
			// (Resolve's own precedence).
			overrides = map[string]bool{name: def}
		}
	}
	return s.experimentRegistry().Resolve(name, overrides, snap.Malformed)
}

// deploymentExperimentDefault returns the default of experiment name on
// this deployment when it differs by deployment from the compiled default,
// with ok true; otherwise ok is false and the compiled default applies.
//
// hub.attachment_artifacts defaults on wherever the artifact service is
// ready (attachmentArtifactsReady), local blob storage included
// (ptone/scion#5828), and on every hub whose resource storage is an object
// store (ptone/scion#5827): uploads then go to the artifact service and
// reach each recipient agent through inbox delivery, so a workstation or
// single-node hub needs no object storage for attachments.
//
// hub.conduit uses the startup deployment default recorded via
// SetConduitDeploymentDefault when set (e.g. off in hosted HA stage 1).
func (s *Server) deploymentExperimentDefault(name string) (bool, bool) {
	if name == conduitExperiment {
		if def := s.conduitDeploymentDefault.Load(); def != nil {
			return def.on, true
		}
		return false, false
	}
	if name == experiments.AttachmentArtifacts && (s.objectStorageConfigured() || s.attachmentArtifactsReady()) {
		return true, true
	}
	return false, false
}

// experimentDefault returns the default experiment exp resolves to on this
// deployment when no override is stored.
func (s *Server) experimentDefault(exp experiments.Experiment) bool {
	if def, ok := s.deploymentExperimentDefault(exp.Name); ok {
		return def
	}
	return exp.Default
}

// objectStorageConfigured reports whether the hub's resource storage is an
// object store (not local disk).
func (s *Server) objectStorageConfigured() bool {
	s.mu.RLock()
	st := s.storage
	s.mu.RUnlock()
	return st != nil && st.Provider() != storage.ProviderLocal
}

// resolvedExperiments returns the effective value of every registered
// web-layer experiment, resolved from a single snapshot.
func (s *Server) resolvedExperiments() map[string]bool {
	snap := s.experimentsSnapshot()
	out := make(map[string]bool)
	for _, exp := range s.experimentRegistry().All() {
		if exp.HasLayer(experiments.LayerWeb) {
			out[exp.Name] = s.experimentEnabledIn(snap, exp.Name)
		}
	}
	return out
}

// requireExperiment wraps next with a request-time gate for a server-layer
// experiment. Routes stay registered so toggling needs no restart.
//
// It validates at registration time: it panics (and so fails hub startup and
// every test that builds the mux) if name is not a registered server-layer
// experiment. A typo therefore fails at boot, not as a silent permanent 404.
func (s *Server) requireExperiment(name string, next http.HandlerFunc) http.HandlerFunc {
	exp, ok := s.experimentRegistry().Lookup(name)
	if !ok || !exp.HasLayer(experiments.LayerServer) {
		panic(fmt.Sprintf("requireExperiment: %q is not a registered server-layer experiment", name))
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.experimentEnabled(name) {
			NotFound(w, "route")
			return
		}
		next(w, r)
	}
}

// dispatchExperimentNames are the server-layer experiments whose state the
// hub sends with each agent dispatch, because they change what the broker
// does. hub.conduit, hub.conduit_deliver, hub.exec_schedules and
// hub.command_requests are added separately (see dispatchExperiments).
//
// It also lists every experiment a platform skill gates on (experiment:
// frontmatter or a scion:experiment block, ptone/scion#7830): the broker
// evaluates those gates from the dispatched state. A test fails when a
// skill names an experiment that is missing here.
var dispatchExperimentNames = []string{
	experiments.K8sNFSHome,
	experiments.K8sWorkloadIdentitySetup,
	experiments.AgentPlugins,
	experiments.CloudRunNativeIdentity,
	experiments.CloudRunEphemeralAgents,
	experiments.Decisions,
	experiments.Artifacts,
	experiments.MessagePolicies,
	experiments.CodeSpanMentions,
	experiments.AgentSecrets,
	experiments.AgentPolicyDurationBackstop,
}

// dispatchExperiments returns the dispatch experiments that are enabled
// now, resolved from one snapshot, or nil when none is. hub.conduit is a
// derived entry: it is included only while a relay can admit an agent that
// dials this hub, with the experiment on in the same snapshot: the
// in-process relay is running (conduitServingIn), or, in external relay
// mode, the ingress serves and a relay is eligible (conduitIngressServingIn).
func (s *Server) dispatchExperiments() []string {
	snap := s.experimentsSnapshot()
	var out []string
	for _, name := range dispatchExperimentNames {
		if s.experimentEnabledIn(snap, name) {
			out = append(out, name)
		}
	}
	if s.conduitServingIn(snap) || s.conduitIngressServingIn(snap) {
		out = append(out, conduitExperiment)
		// hub.conduit_deliver needs hub.conduit (gap G6 of the v2.22
		// freeze): it reaches the broker and the agent only while conduit
		// is served. The agent registers deliver-message from it at
		// launch; the broker routes the agent's deliveries over conduit
		// only for agents launched with it.
		if s.experimentEnabledIn(snap, experiments.ConduitDeliver) {
			out = append(out, experiments.ConduitDeliver)
		}
		// hub.exec_schedules needs hub.conduit too (design D8): the agent
		// registers exec-start and exec-status from it at launch.
		if s.experimentEnabledIn(snap, experiments.ExecSchedules) {
			out = append(out, experiments.ExecSchedules)
		}
		// hub.command_requests needs hub.conduit too: shell execution
		// containers register exec-start and exec-status from it at launch
		// (LLM agents ignore it).
		if s.experimentEnabledIn(snap, experiments.CommandRequests) {
			out = append(out, experiments.CommandRequests)
		}
	}
	return out
}
