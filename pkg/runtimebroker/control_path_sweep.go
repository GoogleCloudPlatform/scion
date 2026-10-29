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
	"log/slog"
	"reflect"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercontrol"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

const (
	// controlPathSweepTick is how often the sweep checks whether the
	// Runtime Broker control paths resolve differently, and retries an
	// inspect that failed on the previous tick.
	controlPathSweepTick = 10 * time.Second
	// controlPathSweepInterval is the longest time between two full sweeps.
	controlPathSweepInterval = 60 * time.Second
)

// controlPathSweepCallTimeout bounds each runtime call the sweep makes
// (List, Stop), so one hung container CLI cannot stall the sweep for every
// runtime. Inspect and locality calls are bounded inside the runtime
// (runtime.runStdoutCommand). A variable so tests can shorten it.
var controlPathSweepCallTimeout = 60 * time.Second

// controlPathSweeper re-checks the binds of the agents already running on
// the broker's runtimes (the primary runtime and every auxiliary runtime,
// collected afresh on each tick) against the Runtime Broker control paths,
// with runtime.VerifyRunningContainerBinds. It covers agents created before the
// check existed and agents whose binds came to reach a control path after
// they started (the control paths resolving somewhere else). A confirmed
// violation or mismatch stops the agent at once; a container whose binds
// cannot be inspected or verified is retried on the next tick and stopped
// if that fails again. Runtimes that bind no broker-host paths (no
// scionrt.MountInspector) are not swept.
type controlPathSweeper struct {
	// runtimes returns the runtimes to sweep.
	runtimes func() []scionrt.Runtime
	log      *slog.Logger
	// markError records a stopped agent's error phase; a seam for tests.
	markError func(a agentRef)

	lastFingerprint string
	lastSweep       time.Time
	// failures counts consecutive inspect or verify failures per runtime
	// and container.
	failures map[string]int
}

// agentRef names a swept agent for markError.
type agentRef struct {
	name, projectPath, runtimeName, profile string
}

func newControlPathSweeper(runtimes func() []scionrt.Runtime, log *slog.Logger) *controlPathSweeper {
	return &controlPathSweeper{
		runtimes: runtimes,
		log:      log,
		failures: map[string]int{},
		markError: func(a agentRef) {
			if a.projectPath == "" {
				return
			}
			if err := agent.UpdateAgentConfig(a.name, a.projectPath, "error", a.runtimeName, a.profile); err != nil {
				log.Debug("control path sweep: failed to mark the stopped agent's error phase", "agent", a.name, "error", err)
			}
		},
	}
}

// run sweeps at once, then on every tick that is due, until ctx is done.
func (sw *controlPathSweeper) run(ctx context.Context) {
	sw.tick(ctx, time.Now())
	t := time.NewTicker(controlPathSweepTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			sw.tick(ctx, now)
		}
	}
}

// tick sweeps when a full sweep is due, the control paths resolve
// differently than at the last sweep (or cannot be resolved), or an inspect
// failed on the previous tick.
func (sw *controlPathSweeper) tick(ctx context.Context, now time.Time) {
	fp := ""
	if set, err := brokercontrol.Resolve(); err == nil {
		fp = set.Fingerprint()
	}
	due := sw.lastSweep.IsZero() || now.Sub(sw.lastSweep) >= controlPathSweepInterval ||
		fp == "" || fp != sw.lastFingerprint || len(sw.failures) > 0
	if !due {
		return
	}
	sw.lastFingerprint = fp
	sw.lastSweep = now
	sw.sweep(ctx)
}

// inspectableRuntimes returns the distinct runtimes that implement
// scionrt.MountInspector.
func (sw *controlPathSweeper) inspectableRuntimes() []scionrt.Runtime {
	var out []scionrt.Runtime
	seen := map[any]bool{}
	for _, rt := range sw.runtimes() {
		if rt == nil {
			continue
		}
		if _, ok := rt.(scionrt.MountInspector); !ok {
			continue
		}
		if reflect.TypeOf(rt).Comparable() {
			if seen[rt] {
				continue
			}
			seen[rt] = true
		}
		out = append(out, rt)
	}
	return out
}

// sweep checks every live agent container on every inspectable runtime once.
func (sw *controlPathSweeper) sweep(ctx context.Context) {
	seen := map[string]bool{}
	// unlisted are the runtimes whose listing failed: their containers'
	// retry counts are kept, so a container whose inspect and listing fail
	// in turn is still stopped.
	unlisted := map[string]bool{}
	for _, rt := range sw.inspectableRuntimes() {
		if !sw.sweepRuntime(ctx, rt, seen) {
			unlisted[rt.Name()] = true
		}
	}
	for key := range sw.failures {
		if seen[key] || unlisted[runtimeOfKey(key)] {
			continue
		}
		delete(sw.failures, key)
	}
}

// failureKey identifies a container of a runtime in failures.
func failureKey(runtimeName, containerID string) string {
	return runtimeName + "\x00" + containerID
}

// runtimeOfKey is the runtime name of a failureKey.
func runtimeOfKey(key string) string {
	name, _, _ := strings.Cut(key, "\x00")
	return name
}

// sweepRuntime checks one runtime's live agent containers. It reports
// false when the runtime's containers could not be listed.
func (sw *controlPathSweeper) sweepRuntime(ctx context.Context, rt scionrt.Runtime, seen map[string]bool) bool {
	insp := rt.(scionrt.MountInspector)
	listCtx, cancel := context.WithTimeout(ctx, controlPathSweepCallTimeout)
	agents, err := rt.List(listCtx, nil)
	cancel()
	if err != nil {
		// Without a listing no container can be identified, so nothing is
		// stopped; the next sweep tries again.
		sw.log.Warn("control path sweep: could not list agents; retrying on the next sweep", "runtime", rt.Name(), "error", err)
		return false
	}
	for _, a := range agents {
		// Only Scion agent containers; never another workload on the same
		// daemon.
		if a.ContainerID == "" || a.Phase == "stopped" || a.Labels["scion.name"] == "" {
			continue
		}
		key := failureKey(rt.Name(), a.ContainerID)
		seen[key] = true
		ib, err := insp.InspectBindMounts(ctx, a.ContainerID)
		if err == nil {
			err = scionrt.VerifyRunningContainerBinds(ib)
		}
		switch {
		case err == nil:
			delete(sw.failures, key)
			continue
		case errors.Is(err, brokercontrol.ErrControlPathMount):
			// A confirmed violation or mismatch: stop now.
		default:
			sw.failures[key]++
			if sw.failures[key] < 2 {
				sw.log.Warn("control path sweep: could not verify an agent's mounts; retrying on the next tick",
					"agent", a.Name, "runtime", rt.Name(), "container", a.ContainerID, "error", err)
				continue
			}
		}
		sw.stop(ctx, rt, key, a.Name, a.ContainerID, a.RunID, err, agentRef{
			name: a.Name, projectPath: a.ProjectPath, runtimeName: rt.Name(), profile: a.Profile,
		})
	}
	return true
}

func (sw *controlPathSweeper) stop(ctx context.Context, rt scionrt.Runtime, key, name, containerID, runID string, cause error, ref agentRef) {
	sw.log.Warn("control path sweep: stopping an agent whose mounts reach, or could not be verified clear of, Runtime Broker control paths",
		"agent", name, "runtime", rt.Name(), "container", containerID, "code", brokercontrol.ErrorCode, "error", cause)
	stopCtx, cancel := context.WithTimeout(ctx, controlPathSweepCallTimeout)
	err := rt.Stop(stopCtx, scionrt.RunRef{ID: containerID, RunID: runID})
	cancel()
	if err != nil {
		// Keep the container due, so the stop is retried on the next tick
		// rather than at the next full sweep.
		sw.failures[key] = max(sw.failures[key], 2)
		sw.log.Error("control path sweep: failed to stop the agent; retrying on the next tick", "agent", name, "container", containerID, "error", err)
		return
	}
	delete(sw.failures, key)
	sw.markError(ref)
}

// sweptRuntimes is the primary runtime plus every auxiliary runtime
// registered now.
func (s *Server) sweptRuntimes() []scionrt.Runtime {
	s.mu.RLock()
	rts := []scionrt.Runtime{s.runtime}
	s.mu.RUnlock()
	for _, aux := range s.sortedAuxiliaryRuntimes() {
		rts = append(rts, aux.Runtime)
	}
	return rts
}

// startControlPathSweep starts the running-agent control-path sweep, at
// most once per Server. It stops when ctx is cancelled or Shutdown is
// called.
func (s *Server) startControlPathSweep(ctx context.Context) {
	s.controlPathSweepOnce.Do(func() {
		loopCtx, cancel := context.WithCancel(ctx)
		s.mu.Lock()
		s.controlPathSweepCancel = cancel
		s.mu.Unlock()
		go newControlPathSweeper(s.sweptRuntimes, s.agentLifecycleLog).run(loopCtx)
	})
}
