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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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
	// startsInFlight returns the agent starts running on this broker
	// (Server.startsInFlightSnapshot). Optional: when nil, the heartbeat
	// neither lists starts nor advertises the capability.
	startsInFlight func() []launchKey
	log            *slog.Logger

	// defaultRuntime is the broker's own default runtime instance, set once
	// by the caller that constructs this service (which already holds it)
	// and kept in sync by SwapRuntime alongside SwapManager. Used only to
	// answer the reported Capabilities.Attach via scionrt.HasAttachSupport —
	// never constructed here.
	defaultRuntime scionrt.Runtime

	// listingDeadline bounds how long a heartbeat waits for the agent
	// listing of its runtime targets (default plus auxiliary). A target not
	// listed in time is reported incomplete, so liveness and the other
	// targets' data keep flowing every interval.
	listingDeadline time.Duration

	// workspaceStorage, when set, returns the broker's current workspace
	// storage descriptor, reported on every heartbeat so the hub sees share
	// health changes. Nil omits the field.
	workspaceStorage func() *api.BrokerWorkspaceStorage

	// profileAttach, when set, returns the attach capability of each
	// profile whose attach support the broker currently knows, reported on
	// every heartbeat so the hub's stored per-profile Attach follows
	// runtime changes without a re-registration. Nil omits the field.
	profileAttach func() []hubclient.ProfileAttachState

	// profileSAMappings, when set, returns each Kubernetes profile's GSA
	// mappings, or nil when they cannot be read. They are sent on the first
	// successful heartbeat, whenever they change, and every
	// saMappingsResendInterval, so a broker restart or a mapping edit
	// refreshes the hub without a re-registration.
	profileSAMappings func() []hubclient.ProfileSAMappingsState
	// sentSAMappingsKey is the fingerprint of the last profileSAMappings
	// the hub accepted, "" before the first, and sentSAMappingsAt when it
	// was accepted (both guarded by mu). Unchanged mappings are re-sent
	// once saMappingsResendInterval has passed, so a hub that lost or never
	// stored a report (an upgrade under a running broker, an overlapping
	// send) catches up; the hub persists only on change.
	sentSAMappingsKey string
	sentSAMappingsAt  time.Time

	// defaultProfile, when set, returns the broker's default (active)
	// profile name, reported on every heartbeat. A nil func, or a nil
	// result (unknown), omits the field.
	defaultProfile func() *string

	mu          sync.Mutex
	listFailing map[string]bool // target key -> last listing failed (guarded by mu)
	// listings holds the listing in progress for each target key (guarded
	// by mu). A heartbeat joins a listing that is still within its
	// deadline instead of starting another, and starts no new listing for
	// a target whose listing has run past its deadline, so a hung runtime
	// cannot pile up goroutines.
	listings map[string]*targetListing
	// joinedListing, if set, is called with each target key for which a
	// heartbeat joined a listing already in progress. Tests use it.
	joinedListing func(key string)
	stopCh        chan struct{}
	doneCh        chan struct{}
}

// targetListing is one runtime target's agent listing, shared by every
// heartbeat that waits for it. Its agents slice is shared by those
// heartbeats, so readers must not modify it or append to it. The listing
// runs under the context of the heartbeat that started it: if that context
// is cancelled, every heartbeat waiting on the listing reports the target
// incomplete.
type targetListing struct {
	done     chan struct{} // closed when agents and err are set
	deadline time.Time
	agents   []api.AgentInfo
	err      error
}

// errListingPending marks a target whose listing did not finish before
// the heartbeat's deadline.
var errListingPending = errors.New("agent listing did not finish before the heartbeat deadline")

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
		client:          client,
		brokerID:        brokerID,
		interval:        interval,
		listingDeadline: defaultListingDeadline(interval),
		manager:         manager,
		projectFilter:   projectFilter,
		log:             log,
	}
}

// listingDeadlineFloor is the listing deadline a heartbeat interval gets
// when half the interval would be shorter, as long as it stays below three
// quarters of the interval. It matches the Docker runtime's own bound on a
// listing (dockerListGroupTimeout), so a short interval does not discard
// Docker listings that would have succeeded.
const listingDeadlineFloor = 10 * time.Second

// defaultListingDeadline returns the listing deadline for a heartbeat
// interval: half the interval, raised towards listingDeadlineFloor for
// short intervals but never past three quarters of the interval, so the
// heartbeat is still sent within its interval. At the default 30s interval
// this is 15s; at the 5s minimum it is 3.75s, below the Docker listing
// bound, and such a slow listing is then reported incomplete.
func defaultListingDeadline(interval time.Duration) time.Duration {
	deadline := interval / 2
	if floor := min(listingDeadlineFloor, interval*3/4); deadline < floor {
		deadline = floor
	}
	return deadline
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

	// Cancel in-flight work (the agent listing and the heartbeat request)
	// as soon as Stop is called, so Stop never waits on a slow runtime.
	s.mu.Lock()
	stopCh := s.stopCh
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()

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
		case <-stopCh:
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
	saKey := s.addProfileSAMappings(heartbeat)
	if err := s.client.Heartbeat(ctx, s.brokerID, heartbeat); err != nil {
		return err
	}
	if saKey != "" {
		s.mu.Lock()
		s.sentSAMappingsKey = saKey
		s.sentSAMappingsAt = time.Now()
		s.mu.Unlock()
	}
	return nil
}

// saMappingsResendInterval is how often unchanged profile SA mappings are
// re-sent on the heartbeat.
const saMappingsResendInterval = 10 * time.Minute

// addProfileSAMappings sets heartbeat.ProfileSAMappings when the current
// mappings differ from the last ones the hub accepted (always on the first
// heartbeat) or saMappingsResendInterval has passed since then, and returns
// their fingerprint, or "" when nothing was added.
func (s *HeartbeatService) addProfileSAMappings(heartbeat *hubclient.BrokerHeartbeat) string {
	if s.profileSAMappings == nil {
		return ""
	}
	mappings := s.profileSAMappings()
	if mappings == nil {
		return ""
	}
	b, err := json.Marshal(mappings)
	if err != nil {
		return ""
	}
	key := string(b)
	s.mu.Lock()
	skip := key == s.sentSAMappingsKey && time.Since(s.sentSAMappingsAt) < saMappingsResendInterval
	s.mu.Unlock()
	if skip {
		return ""
	}
	heartbeat.ProfileSAMappings = mappings
	return key
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
			// This broker honours localOnly deletes and confirms a moved
			// agent's NFS workspace before provisioning it (agent move).
			AgentMove: true,
			// This broker's reprovision reuses an empty-per-agent
			// workspace in place (miller79/scion#167).
			ReprovisionEmptyPerAgent: true,
		},
	}
	if s.workspaceStorage != nil {
		heartbeat.WorkspaceStorage = s.workspaceStorage()
	}
	if s.profileAttach != nil {
		heartbeat.ProfileAttach = s.profileAttach()
	}
	if s.defaultProfile != nil {
		if name := s.defaultProfile(); name != nil {
			v := *name
			heartbeat.DefaultProfile = &v
		}
	}

	// Starts in flight are read BEFORE the agents are listed: a start that
	// finishes between the two reads is then either still listed here or
	// its container is in the agent list, so the hub never sees neither.
	if s.startsInFlight != nil {
		heartbeat.Capabilities.StartsInFlight = true
		for _, k := range s.startsInFlight() {
			if s.projectFilter != nil && !s.projectFilter(k.ProjectID) {
				continue
			}
			heartbeat.StartsInFlight = append(heartbeat.StartsInFlight, hubclient.StartInFlight{ProjectID: k.ProjectID, Slug: k.Slug})
		}
	}

	// Gather per-project agent counts. gatherProjectAgents snapshots the
	// current manager under its own lock and handles nil, so no separate
	// nil check is needed here. It returns within listingDeadline; a target
	// not listed by then is reported incomplete, so the Hub keeps the
	// broker online and draws no conclusion about that target's agents.
	projectAgents, inventory := s.gatherProjectAgents(ctx)
	if len(projectAgents) > 0 {
		heartbeat.Projects = projectAgents
	}
	heartbeat.Inventory = inventory

	return heartbeat
}

// listTarget is one runtime target a heartbeat lists.
type listTarget struct {
	key         string // listing and log key; unique within one heartbeat
	id          string // inventory target ID ("" when unidentified)
	runtimeName string
	mgr         agent.Manager
}

// listTargets lists every target concurrently and waits until all have
// finished, deadline passes, or ctx is done. It returns, per target, the
// agents and the listing error; a target not listed in time gets
// errListingPending.
//
// A target whose listing (started by this or an earlier heartbeat) is still
// within its own deadline is joined rather than listed again. A target
// whose listing has run past its deadline and not returned is reported
// pending without starting another. A listing that returns after its own
// deadline counts as failed, so its result is never reported.
func (s *HeartbeatService) listTargets(ctx context.Context, targets []listTarget, deadline time.Time) ([][]api.AgentInfo, []error) {
	now := time.Now()
	listings := make([]*targetListing, len(targets))
	var joined []string
	s.mu.Lock()
	onJoin := s.joinedListing
	if s.listings == nil {
		s.listings = make(map[string]*targetListing)
	}
	for i, t := range targets {
		if l, ok := s.listings[t.key]; ok {
			if now.Before(l.deadline) {
				listings[i] = l
				if onJoin != nil {
					joined = append(joined, t.key)
				}
			}
			continue
		}
		l := &targetListing{done: make(chan struct{}), deadline: deadline}
		s.listings[t.key] = l
		listings[i] = l
		go s.runListing(ctx, t.key, t.mgr, l)
	}
	s.mu.Unlock()
	for _, key := range joined {
		onJoin(key)
	}

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	expired := false
	agents := make([][]api.AgentInfo, len(targets))
	errs := make([]error, len(targets))
	for i, l := range listings {
		if l == nil {
			errs[i] = errListingPending
			continue
		}
		if !expired {
			select {
			case <-l.done:
			case <-timer.C:
				expired = true
			case <-ctx.Done():
				expired = true
			}
		}
		select {
		case <-l.done:
			agents[i], errs[i] = l.agents, l.err
		default:
			errs[i] = errListingPending
		}
	}
	return agents, errs
}

// runListing lists one target for l, bounded by l.deadline, then publishes
// the result and removes l from the listings in progress.
func (s *HeartbeatService) runListing(ctx context.Context, key string, mgr agent.Manager, l *targetListing) {
	listCtx, cancel := context.WithDeadline(ctx, l.deadline)
	agents, err := mgr.List(listCtx, nil)
	if err == nil {
		// Returned only after its deadline passed (or ctx ended): too old
		// to report. Check the clock too: listCtx's own timer may not have
		// fired yet when List returns just after the deadline.
		if err = listCtx.Err(); err == nil && !time.Now().Before(l.deadline) {
			err = context.DeadlineExceeded
		}
		if err != nil {
			agents = nil
		}
	}
	cancel()
	l.agents, l.err = agents, err
	s.mu.Lock()
	if s.listings[key] == l {
		delete(s.listings, key)
	}
	s.mu.Unlock()
	close(l.done)
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

	// Collect the targets: the default runtime first, then the auxiliary
	// runtimes (e.g. Kubernetes). Unidentified managers get a positional
	// key; a key seen twice in one heartbeat gets its position appended so
	// the two listings stay separate.
	var targets []listTarget
	seenKeys := make(map[string]bool)
	addListTarget := func(m agent.Manager, fallbackKey string) {
		id, runtimeName := heartbeatTargetOf(m)
		key := id
		if key == "" {
			key = fallbackKey
		}
		if seenKeys[key] {
			key = fmt.Sprintf("%s#%d", key, len(targets))
		}
		seenKeys[key] = true
		targets = append(targets, listTarget{key: key, id: id, runtimeName: runtimeName, mgr: m})
	}
	addListTarget(mgr, "default")
	if s.auxiliaryManagers != nil {
		for i, auxMgr := range s.auxiliaryManagers() {
			addListTarget(auxMgr, fmt.Sprintf("auxiliary-%d", i))
		}
	}

	s.mu.Lock()
	listingDeadline := s.listingDeadline
	s.mu.Unlock()
	if listingDeadline <= 0 {
		listingDeadline = defaultListingDeadline(s.interval)
	}
	listed, listErrs := s.listTargets(ctx, targets, time.Now().Add(listingDeadline))
	for i, t := range targets {
		s.noteListResult(t.key, listErrs[i])
		addTarget(t.id, t.runtimeName, listErrs[i] == nil)
	}

	// The default runtime's agents. If its listing failed (e.g. its runtime
	// binary is missing or it was too slow), continue — auxiliary runtimes
	// may still work.
	// Listing results may be shared with concurrent heartbeats and are
	// read-only: copy before appending the auxiliary agents.
	var agents []api.AgentInfo
	if listErrs[0] == nil {
		agents = slices.Clone(listed[0])
	}
	for _, ag := range agents {
		agentTargets[heartbeatAgentKey(ag)] = targets[0].id
	}

	// Also include agents from auxiliary runtimes.
	// Dedup by name+projectID (not name alone) to prevent collision across
	// projects while still deduplicating the same agent found on multiple
	// runtimes. Keying by name alone would drop an auxiliary-runtime agent
	// whenever a different project has a default-runtime agent with the same
	// slug — that agent would then never be reported in heartbeats and its
	// status on the Hub would go stale (e.g. stuck at "starting"). This
	// mirrors the dedup key used by the agent-list handler.
	seen := make(map[string]bool)
	for _, ag := range agents {
		seen[heartbeatAgentKey(ag)] = true
	}
	for i := 1; i < len(targets); i++ {
		if listErrs[i] != nil {
			continue
		}
		for _, ag := range listed[i] {
			k := heartbeatAgentKey(ag)
			if !seen[k] {
				seen[k] = true
				agents = append(agents, ag)
				agentTargets[k] = targets[i].id
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
