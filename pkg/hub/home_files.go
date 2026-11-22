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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ErrBrokerLacksHomeFiles is returned when a create carrying non-empty
// HomeFiles targets a Runtime Broker that does not advertise the homeFiles
// capability (triggers Stage 2 §5.4, §14, §17 OQ3, ptone/scion#8058).
var ErrBrokerLacksHomeFiles = errors.New("broker does not support home files")

// ErrHomeFilesEnvGatherDeferred is returned when a create or provision carrying
// non-empty HomeFiles cannot complete in a single dispatch because required
// environment keys remain unresolved after as_needed resolution. HomeFiles
// exist only on the single dispatch's ctx and are never persisted on the agent
// record (cross-node hops serialize them only into broker_dispatch.args), so
// deferring to a separate HTTP env submission would drop them.
var ErrHomeFilesEnvGatherDeferred = errors.New("homeFiles create cannot defer to a later env submission")

// ErrHomeFilesCreateNotDeferrable is returned when a create carrying non-empty
// HomeFiles hits a Hub create path that would defer execution to a separate
// follow-up request (such as workspace bootstrap upload finalize), where the
// request-scoped HomeFiles are no longer present.
var ErrHomeFilesCreateNotDeferrable = errors.New("homeFiles create cannot be deferred")

// errHomeFilesNotDelivered is returned by start paths when an agent's
// create-time HomeFiles have not yet been delivered by its create dispatch
// (CreateInputs.HomeFilesPending == true). Wraps errStartRequestNotSent because
// the refusal happens before any broker request is sent.
var errHomeFilesNotDelivered = fmt.Errorf("create-time home files have not been delivered yet (%w)", errStartRequestNotSent)

// agentHomeFilesPending reports whether agent requires create-time HomeFiles
// that have not yet been delivered.
func agentHomeFilesPending(agent *store.Agent) bool {
	return agent != nil &&
		agent.AppliedConfig != nil &&
		agent.AppliedConfig.CreateInputs != nil &&
		agent.AppliedConfig.CreateInputs.HomeFilesPending
}

// clearAgentHomeFilesPending clears CreateInputs.HomeFilesPending on the
// persisted agent row (and the in-memory copy, if non-nil) once its create
// dispatch has delivered the HomeFiles. Best-effort: logs a warning on store
// error so a delivered create is not falsely rolled back.
func (s *Server) clearAgentHomeFilesPending(ctx context.Context, agent *store.Agent) {
	if s == nil || s.store == nil || agent == nil || agent.ID == "" {
		return
	}
	if !agentHomeFilesPending(agent) {
		return
	}
	fresh, err := s.store.GetAgent(ctx, agent.ID)
	if err != nil {
		slog.Warn("HomeFiles: failed to reload agent to clear HomeFilesPending",
			"agent_id", agent.ID, "error", err)
		return
	}
	if !agentHomeFilesPending(fresh) {
		agent.AppliedConfig.CreateInputs.HomeFilesPending = false
		return
	}
	fresh.AppliedConfig.CreateInputs.HomeFilesPending = false
	if err := s.store.UpdateAgent(ctx, fresh); err != nil {
		slog.Warn("HomeFiles: failed to clear HomeFilesPending on agent",
			"agent_id", agent.ID, "error", err)
		return
	}
	agent.AppliedConfig.CreateInputs.HomeFilesPending = false
}

type brokerLacksHomeFilesError struct {
	broker string
}

func (e *brokerLacksHomeFilesError) Error() string {
	if e.broker == "" {
		return ErrBrokerLacksHomeFiles.Error()
	}
	return fmt.Sprintf("broker %s does not support home files", e.broker)
}

func (e *brokerLacksHomeFilesError) Unwrap() []error {
	return []error{ErrBrokerLacksHomeFiles, errStartRequestNotSent}
}

type homeFilesEnvGatherDeferredError struct {
	needs []string
}

func (e *homeFilesEnvGatherDeferredError) Error() string {
	if len(e.needs) == 0 {
		return ErrHomeFilesEnvGatherDeferred.Error()
	}
	return fmt.Sprintf("%s: missing env %v", ErrHomeFilesEnvGatherDeferred.Error(), e.needs)
}

func (e *homeFilesEnvGatherDeferredError) Unwrap() error {
	return ErrHomeFilesEnvGatherDeferred
}

type createHomeFilesCtxKey struct{}

// WithCreateHomeFiles attaches transient create-only HomeFiles to ctx for a
// single create dispatch (DispatchAgentCreate, single-pass or
// as_needed-satisfied DispatchAgentCreateWithGather, DispatchAgentProvision,
// and cross-node durable create/finalize_env hops derived from them).
// HomeFiles exist only within that one dispatch's ctx and are never persisted
// on the agent record (cross-node durable hops serialize them into
// broker_dispatch.args); a separate HTTP POST .../env or workspace finalize
// request has no access to them. The slice and file byte contents are
// defensively cloned.
func WithCreateHomeFiles(ctx context.Context, files []api.HomeFile) context.Context {
	if len(files) == 0 {
		return context.WithValue(ctx, createHomeFilesCtxKey{}, ([]api.HomeFile)(nil))
	}
	return context.WithValue(ctx, createHomeFilesCtxKey{}, api.CloneHomeFiles(files))
}

func createHomeFilesFromContext(ctx context.Context) []api.HomeFile {
	if ctx == nil {
		return nil
	}
	files, _ := ctx.Value(createHomeFilesCtxKey{}).([]api.HomeFile)
	if len(files) == 0 {
		return nil
	}
	return api.CloneHomeFiles(files)
}

// checkBrokerHomeFilesCapability fails closed when broker does not advertise
// the homeFiles capability.
//
// Stale capability window (P1b security L4 / review-2 F2): Capabilities are
// replaced (not OR-merged) on every broker registration and heartbeat. If a
// broker is downgraded to a pre-P1b build between heartbeats, the Hub still
// sees HomeFiles=true until the next heartbeat/re-registration and the older
// broker's lenient JSON decoder ignores the unknown homeFiles field. This is
// the standard capability-heartbeat window shared by all broker capabilities
// and is accepted as documented behavior (no protocol echo added).
func checkBrokerHomeFilesCapability(broker *store.RuntimeBroker) error {
	if broker != nil && broker.Capabilities != nil && broker.Capabilities.HomeFiles {
		return nil
	}
	name := ""
	if broker != nil {
		name = brokerDisplayName(broker)
	}
	return &brokerLacksHomeFilesError{broker: name}
}

// gateCreateHomeFiles is the single Hub validation and broker capability gate
// for create-only HomeFiles, shared by the pre-token/run precheck and the
// sendCreate / sendCreateWithGather boundary check.
func (d *HTTPAgentDispatcher) gateCreateHomeFiles(ctx context.Context, agent *store.Agent, files []api.HomeFile, reprovision bool) error {
	if len(files) == 0 {
		return nil
	}
	if reprovision {
		return fmt.Errorf("homeFiles is not valid on a reprovision dispatch (%w)", errStartRequestNotSent)
	}
	if err := api.ValidateHomeFiles(files); err != nil {
		return fmt.Errorf("invalid homeFiles: %w (%w)", err, errStartRequestNotSent)
	}
	if d.store == nil || agent == nil || agent.RuntimeBrokerID == "" {
		return &brokerLacksHomeFilesError{}
	}
	broker, err := d.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return fmt.Errorf("failed to load runtime broker for homeFiles capability check: %w (%w)", err, errStartRequestNotSent)
	}
	return checkBrokerHomeFilesCapability(broker)
}

// createHomeFilesPrecheck extracts any create-only HomeFiles attached to ctx
// and runs gateCreateHomeFiles before any agent token is authorized/signed, run
// recorded, or broker request sent.
func (d *HTTPAgentDispatcher) createHomeFilesPrecheck(ctx context.Context, agent *store.Agent, reprovision bool) error {
	return d.gateCreateHomeFiles(ctx, agent, createHomeFilesFromContext(ctx), reprovision)
}
