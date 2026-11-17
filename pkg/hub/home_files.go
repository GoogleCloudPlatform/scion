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
// exist only on the single dispatch's ctx and are never persisted, so deferring
// to a separate HTTP env submission would drop them.
var ErrHomeFilesEnvGatherDeferred = errors.New("homeFiles create cannot defer to a later env submission")

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

func (e *homeFilesEnvGatherDeferredError) Unwrap() []error {
	return []error{ErrHomeFilesEnvGatherDeferred, errStartRequestNotSent}
}

type createHomeFilesCtxKey struct{}

// WithCreateHomeFiles attaches transient create-only HomeFiles to ctx for a
// single create dispatch (DispatchAgentCreate, single-pass or
// as_needed-satisfied DispatchAgentCreateWithGather, DispatchAgentProvision,
// and cross-node durable create/finalize_env hops derived from them).
// HomeFiles exist only within that one dispatch's ctx and are never persisted
// on the agent store record across a separate HTTP POST .../env or workspace
// finalize request; the slice and file byte contents are defensively cloned.
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

// createHomeFilesPrecheck extracts any create-only HomeFiles attached to ctx,
// records that the agent's create requires HomeFiles, and runs
// gateCreateHomeFiles before any agent token is authorized/signed, run
// recorded, or broker request sent.
func (d *HTTPAgentDispatcher) createHomeFilesPrecheck(ctx context.Context, agent *store.Agent, reprovision bool) error {
	files := createHomeFilesFromContext(ctx)
	if !reprovision && len(files) > 0 && agent != nil && agent.ID != "" {
		d.homeFilesAgents.Store(agent.ID, struct{}{})
	}
	return d.gateCreateHomeFiles(ctx, agent, files, reprovision)
}
