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
	"log/slog"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// heartbeatAgentKey returns a key that uniquely identifies an agent within the
// broker for deduplication purposes. It combines the agent's slug (name) with
// its project ID so agents that share a slug across different projects are not
// collapsed into one entry. This matches the dedup key used by the agent-list
// handler (see runtimebroker/handlers.go).
func heartbeatAgentKey(a api.AgentInfo) string {
	pid := a.ProjectID
	if pid == "" {
		pid = a.Labels["scion.project_id"]
	}
	if pid == "" {
		pid = a.Labels["scion.grove_id"]
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

	mu     sync.Mutex
	stopCh chan struct{}
	doneCh chan struct{}
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
		client:      client,
		brokerID:    brokerID,
		interval:    interval,
		manager:     manager,
		projectFilter: projectFilter,
		log:         log,
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
	heartbeat := s.buildHeartbeat()
	return s.client.Heartbeat(ctx, s.brokerID, heartbeat)
}

// buildHeartbeat constructs the heartbeat payload from current state.
func (s *HeartbeatService) buildHeartbeat() *hubclient.BrokerHeartbeat {
	status := "online"

	heartbeat := &hubclient.BrokerHeartbeat{
		Status: status,
	}

	// If we have a manager, gather per-project agent counts
	if s.manager != nil {
		projectAgents := s.gatherProjectAgents()
		if len(projectAgents) > 0 {
			heartbeat.Projects = projectAgents
		}
	}

	return heartbeat
}

// gatherProjectAgents collects agent information grouped by project.
func (s *HeartbeatService) gatherProjectAgents() []hubclient.ProjectHeartbeat {
	if s.manager == nil {
		return nil
	}

	// List all agents managed by this broker (default runtime)
	agents, err := s.manager.List(context.Background(), nil)
	if err != nil {
		s.log.Error("Failed to list agents for heartbeat", "error", err)
		return nil
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
		for _, auxMgr := range s.auxiliaryManagers() {
			auxAgents, auxErr := auxMgr.List(context.Background(), nil)
			if auxErr != nil {
				continue
			}
			for _, ag := range auxAgents {
				k := heartbeatAgentKey(ag)
				if !seen[k] {
					seen[k] = true
					agents = append(agents, ag)
				}
			}
		}
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

	return projects
}

// ForceHeartbeat sends an immediate heartbeat, bypassing the interval.
// This can be used when significant state changes occur.
func (s *HeartbeatService) ForceHeartbeat(ctx context.Context) error {
	return s.sendHeartbeat(ctx)
}
