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

// Package runtimebroker provides the Scion Runtime Broker API server.
package runtimebroker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// heartbeatAgentKey returns a key that uniquely identifies an agent within the
// broker for deduplication purposes. It combines the agent's slug (name) with
// its project ID so agents that share a slug across different projects are not
// collapsed into one entry. This matches the dedup key used by the agent-list
// handler (see runtimebroker/handlers.go).
func heartbeatAgentKey(a api.AgentInfo) string {
	pid := a.ProjectID
	if pid == "" {
		pid = projectkeys.ProjectIDFromLabels(a.Labels)
	}
	return a.Name + "\x00" + pid
}

const (
	// DefaultHeartbeatInterval is the default interval between heartbeats.
	DefaultHeartbeatInterval = 30 * time.Second
	// MinHeartbeatInterval is the minimum allowed heartbeat interval.
	MinHeartbeatInterval = 5 * time.Second
)

// HeartbeatConfig configures the heartbeat service.
type HeartbeatConfig struct {
	// Interval is the time between heartbeats.
	Interval time.Duration
	// Enabled controls whether heartbeats are sent.
	Enabled bool
}

// DefaultHeartbeatConfig returns the default heartbeat configuration.
func DefaultHeartbeatConfig() HeartbeatConfig {
	return HeartbeatConfig{
		Interval: DefaultHeartbeatInterval,
		Enabled:  true,
	}
}

// HeartbeatService sends periodic heartbeats to the Hub.
type HeartbeatService struct {
	client            hubclient.RuntimeBrokerService
	brokerID          string
	interval          time.Duration
	manager           agent.Manager
	auxiliaryManagers func() []agent.Manager // optional: returns managers for non-default runtimes
	version           string
	projectFilter     func(projectID string) bool // returns true if this project belongs to this hub
	log               *slog.Logger

	// defaultRuntime is the broker's own default runtime instance, set once
	// by the caller that constructs this service (which already holds it)
	// and kept in sync by SwapRuntime alongside SwapManager. Used only to
	// answer the reported Capabilities.Attach via scionrt.HasAttachSupport —
	// never constructed here.
	defaultRuntime scionrt.Runtime

	mu          sync.Mutex
	listFailing map[string]bool // target key -> last listing failed (guarded by mu)
	stopCh      chan struct{}
	doneCh      chan struct{}
}

// SwapManager replaces the agent manager used by the heartbeat service.
// This is called when the broker's container runtime changes (e.g. via
// Server.SwapRuntime during onboarding) so the heartbeat picks up the
// new runtime without being restarted.
func (s *HeartbeatService) SwapManager(m agent.Manager) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.manager = m
}

// SetDefaultRuntime records the broker's default runtime instance for the
// Capabilities.Attach field reported on every heartbeat.
func (s *HeartbeatService) SetDefaultRuntime(rt scionrt.Runtime) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.defaultRuntime = rt
}

// NewHeartbeatService creates a new heartbeat service.
// The client must be an authenticated hubclient.RuntimeBrokerService.
// The manager is used to gather agent status information.
// The projectFilter, if non-nil, restricts which projects are included in heartbeats.
func NewHeartbeatService(client hubclient.RuntimeBrokerService, brokerID string, interval time.Duration, manager agent.Manager, projectFilter func(string) bool, log *slog.Logger) *HeartbeatService {
	if interval < MinHeartbeatInterval {
		interval = MinHeartbeatInterval
	}

	return &HeartbeatService{
		client:        client,
		brokerID:      brokerID,
		interval:      interval,
		manager:       manager,
		projectFilter: projectFilter,
		log:           log,
	}
}

// SetVersion sets the broker version reported in heartbeats.
func (s *HeartbeatService) SetVersion(version string) {
	s.version = version
}

// Start begins sending heartbeats in the background.
// It blocks until Stop is called or the context is cancelled.
// If already started, this is a no-op.
func (s *HeartbeatService) Start(ctx context.Context) {
	s.mu.Lock()
	if s.stopCh != nil {
		s.mu.Unlock()
		return // Already running
	}
	s.stopCh = make(chan struct{})
	s.doneCh = make(chan struct{})
	s.mu.Unlock()

	go s.run(ctx)
}

// Stop signals the heartbeat service to stop and waits for it to finish.
func (s *HeartbeatService) Stop() {
	s.mu.Lock()
	if s.stopCh == nil {
		s.mu.Unlock()
		return // Not running
	}
	close(s.stopCh)
	doneCh := s.doneCh
	s.mu.Unlock()

	// Wait for the run goroutine to finish
	<-doneCh

	s.mu.Lock()
	s.stopCh = nil
	s.doneCh = nil
	s.mu.Unlock()
}

// IsRunning returns true if the heartbeat service is currently running.
func (s *HeartbeatService) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopCh != nil
}

// run is the main heartbeat loop.
func (s *HeartbeatService) run(ctx context.Context) {
	defer close(s.doneCh)

	// Send initial heartbeat immediately
	if err := s.sendHeartbeat(ctx); err != nil {
		s.log.Error("Initial heartbeat failed", "error", err)
	} else {
		s.log.Info("Initial heartbeat sent to Hub")
	}

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := s.sendHeartbeat(ctx); err != nil {
				s.log.Error("Failed to send heartbeat", "error", err)
			}
		case <-s.stopCh:
			s.log.Info("Heartbeat service stopping")
			return
		case <-ctx.Done():
			s.log.Info("Heartbeat service context cancelled")
			return
		}
	}
}

// sendHeartbeat sends a single heartbeat to the Hub.
func (s *HeartbeatService) sendHeartbeat(ctx context.Context) error {
	heartbeat := s.buildHeartbeat(ctx)
	return s.client.Heartbeat(ctx, s.brokerID, heartbeat)
}

// buildHeartbeat constructs the heartbeat payload from current state.
func (s *HeartbeatService) buildHeartbeat(ctx context.Context) *hubclient.BrokerHeartbeat {
	status := "online"

	s.mu.Lock()
	defaultRuntime := s.defaultRuntime
	s.mu.Unlock()

	heartbeat := &hubclient.BrokerHeartbeat{
		Status: status,
		// Design §3.4 Amendment A2.2(b): report capabilities on every heartbeat so the hub's
		// `scion reincarnate` gate is never stuck on a stale join-time
		// snapshot for an already-registered broker. Sync and Reprovision
		// are a fixed property of this broker binary; Attach reflects the
		// default runtime's own optional capability
		// (scionrt.HasAttachSupport), same as handleInfo's Capabilities.Attach.
		Capabilities: &hubclient.BrokerCapabilities{
			WebPTY:                 false,
			Sync:                   true,
			Attach:                 scionrt.HasAttachSupport(defaultRuntime),
			Reprovision:            true,
			AsyncLaunch:            true,
			EmptyPerAgentWorkspace: scionrt.HasEmptyPerAgentSupport(defaultRuntime),
		},
	}

	// Gather per-project agent counts. gatherProjectAgents snapshots the
	// current manager under its own lock and handles nil, so no separate
	// nil check is needed here.
	projectAgents, inventory := s.gatherProjectAgents(ctx)
	if len(projectAgents) > 0 {
		heartbeat.Projects = projectAgents
	}
	heartbeat.Inventory = inventory

	return heartbeat
}

// runtimeNamer is implemented by agent managers that can name the runtime
// they list (agent.AgentManager does).
type runtimeNamer interface {
	RuntimeName() string
}

// runtimeTargetNamer lets a manager other than agent.AgentManager (tests)
// supply its inventory target ID directly.
type runtimeTargetNamer interface {
	runtimeTargetID() string
}

// heartbeatTargetOf returns the inventory target ID and runtime name for a
// manager, or empty strings when the manager cannot be identified. The ID is
// the same identity the broker keys its auxiliary runtimes by
// (auxiliaryRuntimeIdentity), so a Kubernetes target includes its cluster
// context and namespace.
func heartbeatTargetOf(m agent.Manager) (id, runtimeName string) {
	if am, ok := m.(*agent.AgentManager); ok {
		if am.Runtime == nil {
			return "", ""
		}
		return auxiliaryRuntimeIdentity(am.Runtime), am.Runtime.Name()
	}
	if n, ok := m.(runtimeNamer); ok {
		runtimeName = n.RuntimeName()
	}
	if t, ok := m.(runtimeTargetNamer); ok && t.runtimeTargetID() != "" {
		return t.runtimeTargetID(), runtimeName
	}
	return runtimeName, runtimeName
}

// noteListResult records whether listing a target failed and logs the
// failure at Warn only when the target's state changes (first failure, or
// failure after success); repeated failures are logged at Debug, and a
// recovery is logged once at Info.
func (s *HeartbeatService) noteListResult(key string, err error) {
	s.mu.Lock()
	if s.listFailing == nil {
		s.listFailing = make(map[string]bool)
	}
	wasFailing := s.listFailing[key]
	s.listFailing[key] = err != nil
	s.mu.Unlock()

	switch {
	case err != nil && !wasFailing:
		s.log.Warn("Runtime target agent listing failed for heartbeat; target reported incomplete",
			"target", key, "error", err)
	case err != nil:
		s.log.Debug("Runtime target agent listing still failing for heartbeat",
			"target", key, "error", err)
	case wasFailing:
		s.log.Info("Runtime target agent listing recovered for heartbeat", "target", key)
	}
}

// gatherProjectAgents collects agent information grouped by project, and
// reports per runtime target whether that information is the target's
// complete inventory. A target is complete only when its listing succeeded
// and it can be identified; a failed listing (for example one forbidden by
// the cluster) marks only that target incomplete. Each reported agent
// carries the ID of the target whose listing reported it.
func (s *HeartbeatService) gatherProjectAgents(ctx context.Context) ([]hubclient.ProjectHeartbeat, *hubclient.BrokerInventory) {
	// Snapshot the current manager under the lock so that a concurrent
	// SwapManager call (triggered by Server.SwapRuntime) is picked up
	// on the next heartbeat tick rather than racing with this one.
	s.mu.Lock()
	mgr := s.manager
	s.mu.Unlock()

	inventory := &hubclient.BrokerInventory{}
	if mgr == nil {
		return nil, inventory
	}

	targetIndex := make(map[string]int)
	addTarget := func(id, runtimeName string, complete bool) {
		if id == "" {
			return
		}
		if i, ok := targetIndex[id]; ok {
			// The same target listed twice is complete only if both
			// listings succeeded.
			inventory.Targets[i].Complete = inventory.Targets[i].Complete && complete
			return
		}
		targetIndex[id] = len(inventory.Targets)
		inventory.Targets = append(inventory.Targets, hubclient.InventoryTarget{ID: id, Runtime: runtimeName, Complete: complete})
	}

	// agentTargets maps heartbeatAgentKey to the target that reported it.
	agentTargets := make(map[string]string)

	// List all agents managed by this broker (default runtime).
	// If the default manager fails (e.g. its runtime binary is missing),
	// continue — auxiliary managers may still work.
	defaultID, defaultRuntime := heartbeatTargetOf(mgr)
	defaultKey := defaultID
	if defaultKey == "" {
		defaultKey = "default"
	}
	agents, err := mgr.List(ctx, nil)
	s.noteListResult(defaultKey, err)
	if err != nil {
		agents = nil
	}
	addTarget(defaultID, defaultRuntime, err == nil)
	for _, ag := range agents {
		agentTargets[heartbeatAgentKey(ag)] = defaultID
	}

	// Also include agents from auxiliary runtimes (e.g. Kubernetes).
	// Dedup by name+projectID (not name alone) to prevent collision across
	// projects while still deduplicating the same agent found on multiple
	// runtimes. Keying by name alone would drop an auxiliary-runtime agent
	// whenever a different project has a default-runtime agent with the same
	// slug — that agent would then never be reported in heartbeats and its
	// status on the Hub would go stale (e.g. stuck at "starting"). This
	// mirrors the dedup key used by the agent-list handler.
	if s.auxiliaryManagers != nil {
		seen := make(map[string]bool)
		for _, ag := range agents {
			seen[heartbeatAgentKey(ag)] = true
		}
		for i, auxMgr := range s.auxiliaryManagers() {
			auxID, auxRuntime := heartbeatTargetOf(auxMgr)
			key := auxID
			if key == "" {
				key = fmt.Sprintf("auxiliary-%d", i)
			}
			auxAgents, auxErr := auxMgr.List(ctx, nil)
			s.noteListResult(key, auxErr)
			addTarget(auxID, auxRuntime, auxErr == nil)
			if auxErr != nil {
				continue
			}
			for _, ag := range auxAgents {
				k := heartbeatAgentKey(ag)
				if !seen[k] {
					seen[k] = true
					agents = append(agents, ag)
					agentTargets[k] = auxID
				}
			}
		}
	}

	// A project filter (multi-hub mode) drops projects whose ownership is
	// inferred from local settings, so the reported list is not a reliable
	// complete inventory of any target for any one hub: claim nothing.
	if s.projectFilter != nil {
		inventory.Targets = nil
	}

	// Group agents by project
	projectMap := make(map[string][]hubclient.AgentHeartbeat)
	for _, ag := range agents {
		projectID := ag.ProjectID
		if projectID == "" {
			projectID = ag.Project
		}
		if projectID == "" {
			projectID = "default"
		}

		// Compute legacy Status using DisplayStatus logic:
		// if running with an activity, show the activity; otherwise show the phase.
		as := state.AgentState{Phase: state.Phase(ag.Phase), Activity: state.Activity(ag.Activity)}
		agentHB := hubclient.AgentHeartbeat{
			Slug:            ag.Name, // Use Name as the slug identifier
			Status:          as.DisplayStatus(),
			Phase:           ag.Phase,
			Activity:        ag.Activity,
			ContainerStatus: ag.ContainerStatus,
			HarnessAuth:     ag.HarnessAuth,
			Profile:         ag.Profile,
			ExitCode:        ag.ExitCode,
			ExitReason:      ag.ExitReason,
			RuntimeTarget:   agentTargets[heartbeatAgentKey(ag)],
		}
		if ag.Detail != nil && ag.Detail.Message != "" {
			agentHB.Message = ag.Detail.Message
		}
		projectMap[projectID] = append(projectMap[projectID], agentHB)
	}

	// Convert to slice, applying project filter
	var projects []hubclient.ProjectHeartbeat
	for projectID, agentList := range projectMap {
		if s.projectFilter != nil && !s.projectFilter(projectID) {
			continue
		}
		projects = append(projects, hubclient.ProjectHeartbeat{
			ProjectID:  projectID,
			AgentCount: len(agentList),
			Agents:     agentList,
		})
	}

	return projects, inventory
}

// ForceHeartbeat sends an immediate heartbeat, bypassing the interval.
// This can be used when significant state changes occur.
func (s *HeartbeatService) ForceHeartbeat(ctx context.Context) error {
	return s.sendHeartbeat(ctx)
}
