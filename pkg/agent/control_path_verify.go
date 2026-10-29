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

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// verifyStopTimeout bounds the stop of a container whose binds failed
// verification.
const verifyStopTimeout = 60 * time.Second

// verifyCreatedMounts re-checks the binds of a container that Run has just
// created, before the start goes any further (runtime.VerifyContainerBinds):
// the runtime's own view of the binds must match the binds the broker
// requested and recorded on the container, and every source is checked
// against the Runtime Broker control paths again. An inspect failure is
// retried once at once. A refused or mismatching bind, a missing record, or
// a second inspect failure stops the container and fails the start; the
// workspace is kept. Runtimes that bind no broker-host paths (no
// runtime.MountInspector) have nothing to verify.
func (m *AgentManager) verifyCreatedMounts(ctx context.Context, ref runtime.RunRef, agentName string) error {
	insp, ok := m.Runtime.(runtime.MountInspector)
	if !ok {
		return nil
	}
	ib, err := insp.InspectBindMounts(ctx, ref.ID)
	if err != nil {
		ib, err = insp.InspectBindMounts(ctx, ref.ID)
	}
	if err == nil {
		err = runtime.VerifyContainerBinds(ib)
		if err == nil {
			return nil
		}
	}
	slog.Warn("Start: stopping an agent whose container mounts could not be verified clear of Runtime Broker control paths",
		"agent", agentName, "code", brokercontrol.ErrorCode, "error", err)
	// The stop is bounded, so a hung runtime CLI cannot hold the start. It
	// does not inherit the request's cancellation: a client that goes away
	// or times out must not cut the stop short.
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), verifyStopTimeout)
	stopErr := m.Runtime.Stop(stopCtx, ref)
	cancel()
	if !errors.Is(err, brokercontrol.ErrControlPathMount) {
		err = fmt.Errorf("%w: %v", brokercontrol.ErrControlPathMount, err)
	}
	if stopErr != nil {
		slog.Error("Start: failed to stop the agent after a refused container mount",
			"agent", agentName, "error", stopErr)
		return fmt.Errorf("agent %q could not be stopped (%v) and may still be running; the Runtime Broker's running-agent check re-checks it and stops it if its mounts still fail the check: %w", agentName, stopErr, err)
	}
	return fmt.Errorf("agent %q stopped: %w", agentName, err)
}
