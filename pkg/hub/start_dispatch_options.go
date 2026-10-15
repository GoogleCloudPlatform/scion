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
	// every eligibility rule holds; otherwise the start is synchronous.
	// This release carries it but no dispatcher acts on it yet: every start
	// stays synchronous.
	Async bool
}

// StartDispatchResult is what a start dispatch returns besides its error.
// Launch is set when the broker accepted the start as an asynchronous
// launch; the launch's end then settles the start. Nil (or a nil Launch)
// means the start ran synchronously. This release never sets it.
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
// (test fakes, other implementations) keep working: they get the
// synchronous start (dispatchAgentStartWithOptions).
type startOptionsDispatcher interface {
	DispatchAgentStartWithOptions(ctx context.Context, agent *store.Agent, task string, resume bool, opts StartDispatchOptions) (*StartDispatchResult, error)
}

// dispatchAgentStartWithOptions dispatches a start through d with opts when
// d takes start options, and otherwise through d.DispatchAgentStart, which
// is always synchronous (a nil result).
func dispatchAgentStartWithOptions(ctx context.Context, d AgentDispatcher, agent *store.Agent, task string, resume bool, opts StartDispatchOptions) (*StartDispatchResult, error) {
	if od, ok := d.(startOptionsDispatcher); ok {
		return od.DispatchAgentStartWithOptions(ctx, agent, task, resume, opts)
	}
	return nil, d.DispatchAgentStart(ctx, agent, task, resume)
}

// HTTPAgentDispatcher takes start options.
var _ startOptionsDispatcher = (*HTTPAgentDispatcher)(nil)
