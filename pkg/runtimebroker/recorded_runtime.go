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
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Recorded runtime type for operations on an existing agent
// (ptone/scion#2748).
//
// The hub sends the runtime type it recorded for an agent (store
// Agent.Runtime) as the api.RecordedRuntimeQueryParam query parameter on
// every existing-agent request. The broker uses it to pick the runtime that
// holds the agent:
//
//   - runtimes of the recorded type are searched first;
//   - if the agent is not there but another registered runtime lists it, that
//     runtime is used: a positive match wins over the recorded type, which can
//     be a guess (for example a runtime backfilled from a broker profile);
//   - if no registered runtime lists it and the recorded type has no
//     registered runtime, the broker answers with a retryable 503 rather than
//     acting through its default runtime, which could otherwise report "not
//     found", or an idempotent success, while the agent is still running in a
//     runtime this broker cannot reach right now.
//
// An empty parameter (an older hub, or an agent with no recorded runtime)
// keeps the previous behaviour: every registered runtime is searched,
// default first.

// errRuntimeNotRegistered marks a request whose recorded runtime type is
// recognised but has no registered manager on this broker.
var errRuntimeNotRegistered = errors.New("runtime not registered on this broker")

// recordedRuntimeRetryAfterSeconds is the Retry-After sent with the 503 for
// an unregistered recorded runtime. Auxiliary runtimes are registered at
// startup and on demand, so the condition can clear without operator action.
const recordedRuntimeRetryAfterSeconds = "30"

// knownRuntimeNames are the names runtime.Runtime.Name() reports for the
// runtimes pkg/runtime.GetRuntime (factory.go) constructs. Kubernetes
// spellings are classified separately by isKubernetesRuntimeName, which
// mirrors GetRuntime's own normalization ("remote" → "kubernetes", "k8s"
// accepted alongside "kubernetes"); "cloudrun-instances" is likewise mapped
// to "cloudrun" by canonicalRuntimeName.
var knownRuntimeNames = map[string]bool{
	"docker":           true,
	"podman":           true,
	"container":        true,
	"cloudrun":         true,
	"cloudrun-sandbox": true,
	"substrate":        true,
}

// canonicalRuntimeName returns the runtime name a recorded runtime type
// matches, and whether it is a recognised runtime name at all. It applies
// only the normalization pkg/runtime already performs; any other value
// (including the "local"/"auto" auto-detect requests, which name no concrete
// runtime) is unrecognised.
func canonicalRuntimeName(name string) (string, bool) {
	if isKubernetesRuntimeName(name) {
		return "kubernetes", true
	}
	if name == "cloudrun-instances" {
		// GetRuntime builds a CloudRunRuntime for "cloudrun-instances",
		// and that runtime reports its name as "cloudrun".
		return "cloudrun", true
	}
	if knownRuntimeNames[name] {
		return name, true
	}
	return "", false
}

// runtimeMatchName is the name runtimes are matched by: the canonical name
// for a recognised runtime name, otherwise the name itself.
func runtimeMatchName(name string) string {
	if canonical, ok := canonicalRuntimeName(name); ok {
		return canonical
	}
	return name
}

type recordedRuntimeKey struct{}

// withRecordedRuntime returns ctx restricted to runtimes whose match name
// (runtimeMatchName) is canonical.
func withRecordedRuntime(ctx context.Context, canonical string) context.Context {
	return context.WithValue(ctx, recordedRuntimeKey{}, canonical)
}

// recordedRuntimeFrom returns the canonical recorded runtime name attached to
// ctx, or "" when lookups are unrestricted.
func recordedRuntimeFrom(ctx context.Context) string {
	v, _ := ctx.Value(recordedRuntimeKey{}).(string)
	return v
}

// runtimeAllowed reports whether rt may hold the agent of a request carrying
// ctx: always when ctx carries no recorded runtime, otherwise only when rt's
// canonical name matches it.
func runtimeAllowed(ctx context.Context, rt scionrt.Runtime) bool {
	want := recordedRuntimeFrom(ctx)
	if want == "" {
		return true
	}
	if rt == nil {
		return false
	}
	return runtimeMatchName(rt.Name()) == want
}

// defaultRuntimeAllowed reports whether the broker's default runtime may hold
// the agent of a request carrying ctx.
func (s *Server) defaultRuntimeAllowed(ctx context.Context) bool {
	defRT := s.currentRuntime()
	return runtimeAllowed(ctx, defRT)
}

// sortedAuxiliaryRuntimesFor is sortedAuxiliaryRuntimes restricted to the
// auxiliary runtimes a request carrying ctx may target.
func (s *Server) sortedAuxiliaryRuntimesFor(ctx context.Context) []namedAuxiliaryRuntime {
	all := s.sortedAuxiliaryRuntimes()
	if recordedRuntimeFrom(ctx) == "" {
		return all
	}
	out := make([]namedAuxiliaryRuntime, 0, len(all))
	for _, aux := range all {
		if runtimeAllowed(ctx, aux.Runtime) {
			out = append(out, aux)
		}
	}
	return out
}

// applyRecordedRuntime reads the recorded runtime type from r and returns the
// context existing-agent lookups for agent id in projectID must use:
//
//   - no recorded type: ctx unchanged (every runtime is searched);
//   - an unrecognised type: ctx unchanged, logged — the broker cannot tell
//     which runtime it names, so it keeps the previous behaviour rather than
//     refusing every operation on the agent;
//   - a recognised type whose runtimes list the agent: ctx restricted to
//     runtimes of that type;
//   - otherwise, if any registered runtime lists the agent: ctx restricted to
//     runtimes of that runtime's type (logged, since the recorded type is
//     then wrong);
//   - otherwise, a recognised type with a registered runtime: ctx restricted
//     to runtimes of that type (the agent is gone; the operation reports it
//     as it would there);
//   - otherwise: errRuntimeNotRegistered.
func (s *Server) applyRecordedRuntime(r *http.Request, id, projectID string) (context.Context, string, error) {
	ctx := r.Context()
	recorded := r.URL.Query().Get(api.RecordedRuntimeQueryParam)
	if recorded == "" {
		return ctx, "", nil
	}
	canonical, ok := canonicalRuntimeName(recorded)
	if !ok {
		s.agentLifecycleLog.Warn("Unrecognised recorded runtime type; searching all registered runtimes",
			"agent_id", id, "runtime", recorded)
		return ctx, recorded, nil
	}
	// Selection rule. "Lists the agent" means a runtime's List returned it;
	// a List that fails counts as not listing it. So a recorded-type runtime
	// whose List errors does not hand the request to another runtime, and
	// the request stays in the recorded type unless another runtime
	// positively lists the agent.
	restricted := withRecordedRuntime(ctx, canonical)
	registered := s.defaultRuntimeAllowed(restricted) || len(s.sortedAuxiliaryRuntimesFor(restricted)) > 0
	if registered {
		if _, _, found := s.findAgentRuntimeTarget(restricted, id, projectID); found {
			return restricted, recorded, nil
		}
	}
	if _, rt, found := s.findAgentRuntimeTarget(ctx, id, projectID); found && rt != nil {
		actual := runtimeMatchName(rt.Name())
		s.logRecordedRuntimeMismatch(id, projectID, recorded, actual)
		return withRecordedRuntime(ctx, actual), recorded, nil
	}
	if registered {
		return restricted, recorded, nil
	}
	s.agentLifecycleLog.Warn("Recorded runtime has no registered manager on this broker and no runtime lists the agent",
		"agent_id", id, "runtime", recorded)
	return ctx, recorded, fmt.Errorf("%w: %s", errRuntimeNotRegistered, recorded)
}

// recordedRuntimeMismatchLogged holds the agent/recorded/actual combinations
// whose mismatch has already been logged at warn level in this process. One
// entry per agent whose recorded type is wrong, so it stays small.
var recordedRuntimeMismatchLogged sync.Map

// logRecordedRuntimeMismatch logs that an agent was found in a runtime other
// than its recorded type: at warn level the first time for that agent and
// pair of types in this process, at debug level after that, since every
// gated request for the agent repeats the same finding.
func (s *Server) logRecordedRuntimeMismatch(id, projectID, recorded, actual string) {
	key := projectID + "\x00" + id + "\x00" + recorded + "\x00" + actual
	log := s.agentLifecycleLog.Debug
	if _, seen := recordedRuntimeMismatchLogged.LoadOrStore(key, struct{}{}); !seen {
		log = s.agentLifecycleLog.Warn
	}
	log("Agent found in a runtime other than its recorded type; using that runtime",
		"agent_id", id, "project_id", projectID, "recorded_runtime", recorded, "runtime", actual)
}

// runtimeNotRegisteredMessage is the client-facing text for
// errRuntimeNotRegistered.
func runtimeNotRegisteredMessage(recorded string) string {
	return fmt.Sprintf("runtime %q is not available on this broker; retry later or check the broker's runtime configuration", recorded)
}

// writeRuntimeNotRegistered writes the retryable 503 for a recorded runtime
// with no registered manager.
func writeRuntimeNotRegistered(w http.ResponseWriter, recorded string) {
	w.Header().Set("Retry-After", recordedRuntimeRetryAfterSeconds)
	RuntimeUnavailable(w, runtimeNotRegisteredMessage(recorded))
}

// isExistingAgentRequest reports whether handleAgentByID must apply the
// recorded-runtime check to action before dispatching it: the bare GET and
// DELETE (action ""), and every handleAgentAction action except start (which
// resolves its runtime from settings, ptone/scion#2709), keys (which applies
// the check itself so it can answer in its own result shape) and stats (a
// placeholder that reads no runtime).
// Unknown actions are left to handleAgentAction's 404.
func isExistingAgentRequest(action string) bool {
	if action == "" {
		return true
	}
	// Stats is a placeholder that reads no runtime, so the check would only
	// add List calls.
	if action == api.AgentActionStart || action == api.AgentActionKeys || action == api.AgentActionStats {
		return false
	}
	_, ok := api.RuntimeBrokerAgentActionMethod(action)
	return ok
}
