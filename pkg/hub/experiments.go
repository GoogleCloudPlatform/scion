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
)

// experimentRegistry returns the registry this server resolves experiments
// against. Production never sets ServerConfig.Experiments, so this falls
// back to experiments.Default(). Every reader below goes through this
// accessor, never the s.experiments field directly, because many pkg/hub
// tests build &Server{...} by struct literal without New() (ptone/scion#2217).
func (s *Server) experimentRegistry() *experiments.Registry {
	if s.experiments == nil {
		return experiments.Default()
	}
	return s.experiments
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

// experimentEnabledIn is the single resolution function. Callers that report
// several values (resolvedExperiments, the admin responses in 1a-ii) take one
// snapshot and resolve every name from it, so a concurrent refresh cannot
// produce a mixed answer (ptone/scion#2217).
func (s *Server) experimentEnabledIn(snap ExperimentsSnapshot, name string) bool {
	exp, ok := s.experimentRegistry().Lookup(name)
	if !ok {
		return false // unknown or retired → off, never panic
	}
	if snap.Malformed && exp.HasLayer(experiments.LayerServer) {
		return false // fail closed for server behaviour
	}
	if v, set := snap.Overrides[name]; set {
		return v
	}
	return exp.Default
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
