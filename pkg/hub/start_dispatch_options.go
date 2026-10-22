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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// StartDispatchOptions are the per-call options of a start dispatch
// (ptone/scion#4669).
type StartDispatchOptions struct {
	// Async asks for the start to be sent as an asynchronous launch when
	// the dispatcher's own rules also hold (hub.async_agent_launch on, a
	// launch timeout, a broker not known to lack AsyncStart); otherwise
	// the start is synchronous. The caller has already checked the
	// hub.async_agent_start experiment, the all-live-hubs rule
	// (asyncStartAllowed), the request's opt-in and the prior phase.
	Async bool
	// Kind is the launch kind to begin: store.LaunchKindStart, or
	// store.LaunchKindRestart for a restart's start leg. Empty means
	// start.
	Kind string
	// PriorPhase is the effective resting phase the start was taken from
	// (startDispatch.restingPhase), recorded as the launch's prior phase.
	PriorPhase string
}

// launchKind returns o.Kind, defaulting to store.LaunchKindStart.
func (o StartDispatchOptions) launchKind() string {
	if o.Kind == store.LaunchKindRestart {
		return store.LaunchKindRestart
	}
	return store.LaunchKindStart
}

// StartDispatchResult is what a start dispatch returns besides its error.
// Launch is set when the broker accepted the start as an asynchronous
// launch; the launch's end then settles the start. Nil (or a nil Launch)
// means the start ran synchronously.
type StartDispatchResult struct {
	Launch *LaunchAccepted `json:"launch,omitempty"`
}

// AcceptedLaunch returns r.Launch, or nil when r is nil.
func (r *StartDispatchResult) AcceptedLaunch() *LaunchAccepted {
	if r == nil {
		return nil
	}
	return r.Launch
}

// startOptionsDispatcher is an AgentDispatcher that takes start options.
// HTTPAgentDispatcher implements it. It is a separate interface, not a
// change to AgentDispatcher.DispatchAgentStart, so dispatchers without it
// (test fakes, other implementations) keep working: a caller type-asserts
// and falls back to the synchronous DispatchAgentStart (execDispatchStart).
// Every call of DispatchAgentStartWithOptions is a guarded external-effect
// and lifecycle dispatch call site (msg_helpers_test.go,
// run_intent_guard_test.go).
type startOptionsDispatcher interface {
	DispatchAgentStartWithOptions(ctx context.Context, agent *store.Agent, task string, resume bool, opts StartDispatchOptions) (*StartDispatchResult, error)
}

// HTTPAgentDispatcher takes start options.
var _ startOptionsDispatcher = (*HTTPAgentDispatcher)(nil)
