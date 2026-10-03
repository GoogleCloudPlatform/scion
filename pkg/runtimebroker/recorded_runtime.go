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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Recorded runtime type for operations on an existing agent
// (ptone/scion#2748).
//
// The hub sends the runtime type it recorded for an agent (store
// Agent.Runtime, the broker-reported runtime.Runtime.Name() of the runtime
// that started it) as the api.RecordedRuntimeQueryParam query parameter on
// every existing-agent request. The broker then looks for the agent only in
// runtimes of that type, and when none is registered answers with a
// retryable 503 rather than looking in its default runtime — which could
// otherwise report "not found", or an idempotent success, while the agent is
// still running in a runtime this broker cannot reach right now.
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
// accepted alongside "kubernetes").
var knownRuntimeNames = map[string]bool{
	"docker":           true,
	"podman":           true,
	"container":        true,
	"cloudrun":         true,
	"cloudrun-sandbox": true,
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
	if knownRuntimeNames[name] {
		return name, true
	}
	return "", false
}

type recordedRuntimeKey struct{}

// withRecordedRuntime returns ctx restricted to runtimes whose canonical
// name is canonical (as returned by canonicalRuntimeName).
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
	got, ok := canonicalRuntimeName(rt.Name())
	return ok && got == want
}

// defaultRuntimeAllowed reports whether the broker's default runtime may hold
// the agent of a request carrying ctx.
func (s *Server) defaultRuntimeAllowed(ctx context.Context) bool {
	return runtimeAllowed(ctx, s.runtime)
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
// context existing-agent lookups must use:
//
//   - no recorded type: ctx unchanged (every runtime is searched);
//   - an unrecognised type: ctx unchanged, logged — the broker cannot tell
//     which runtime it names, so it keeps the previous behaviour rather than
//     refusing every operation on the agent;
//   - a recognised type with at least one registered runtime of that type
//     (default or auxiliary): ctx restricted to those runtimes;
//   - a recognised type with no registered runtime: errRuntimeNotRegistered.
func (s *Server) applyRecordedRuntime(r *http.Request, id string) (context.Context, string, error) {
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
	restricted := withRecordedRuntime(ctx, canonical)
	if s.defaultRuntimeAllowed(restricted) || len(s.sortedAuxiliaryRuntimesFor(restricted)) > 0 {
		return restricted, recorded, nil
	}
	s.agentLifecycleLog.Warn("Recorded runtime has no registered manager on this broker",
		"agent_id", id, "runtime", recorded)
	return ctx, recorded, fmt.Errorf("%w: %s", errRuntimeNotRegistered, recorded)
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
// resolves its runtime from settings, ptone/scion#2709) and keys (which
// applies the check itself so it can answer in its own result shape).
// Unknown actions are left to handleAgentAction's 404.
func isExistingAgentRequest(action string) bool {
	if action == "" {
		return true
	}
	if action == api.AgentActionStart || action == api.AgentActionKeys {
		return false
	}
	_, ok := api.RuntimeBrokerAgentActionMethod(action)
	return ok
}
