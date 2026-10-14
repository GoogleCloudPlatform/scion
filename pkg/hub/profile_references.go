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
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Built-in profile references for the co-located broker's settings overlay
// (ptone/scion#5113). The overlay retains a built-in default profile that is
// still referenced when the hub's stored profiles replace the catalog; the
// hub counts the references here and hands them to the overlay
// (config.SettingsOverlay.SetProfileReferences).
//
// The count runs once at install (EnableProfileReferences) and then in one
// worker goroutine, started with settings propagation, on every poll tick,
// after stored-settings applies, project activeProfile saves and provider
// links to the co-located broker. Runs are serialized; triggers coalesce
// and never block. Because retention only grows within a process, a stale
// count can lower counts but never remove a retained profile.

// profileReferenceRunTimeout bounds one reference count. A variable so
// tests can shorten it.
var profileReferenceRunTimeout = 30 * time.Second

// profileReferencePageSize is the page size of the agent and project scans.
const profileReferencePageSize = 500

// profileReferences is the Server's reference-count state. The zero value
// is ready to use.
type profileReferences struct {
	once    sync.Once
	trigger chan struct{}

	// runMu serializes counts: the install compute and worker runs.
	runMu sync.Mutex

	// mu guards the target, published by EnableProfileReferences.
	mu        sync.Mutex
	overlay   *config.SettingsOverlay
	brokerID  string
	globalDir string

	// lastErr is the text of the last count failure logged (guarded by
	// runMu); a failure with the same text is not logged again until a
	// count succeeds.
	lastErr string
}

func (p *profileReferences) triggerChan() chan struct{} {
	p.once.Do(func() { p.trigger = make(chan struct{}, 1) })
	return p.trigger
}

// target returns the published overlay, co-located broker ID and global
// directory; overlay is nil until EnableProfileReferences ran.
func (p *profileReferences) target() (*config.SettingsOverlay, string, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.overlay, p.brokerID, p.globalDir
}

// LegacyColocatedBrokerID returns the ID of the legacy co-located Runtime
// Broker this process hosts (stateful or stateless), never a flat instance
// ID; "" when there is none (a flat host, or a failed registration).
func (s *Server) LegacyColocatedBrokerID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.embeddedBrokerID
}

// EnableProfileReferences counts the references for the co-located broker
// brokerID and enables built-in retention on overlay. It runs the count
// synchronously; on failure it enables conservative retention instead (every
// built-in profile except those on the unscoped kubernetes runtime) and the
// worker retries. globalDir is the broker's global settings directory
// (hub-managed projects live under it). A no-op when overlay is nil or
// brokerID is empty: then the overlay behaves as before ptone/scion#5113.
func (s *Server) EnableProfileReferences(ctx context.Context, overlay *config.SettingsOverlay, brokerID, globalDir string) {
	if overlay == nil || brokerID == "" {
		return
	}
	p := &s.profileRefs
	p.runMu.Lock()
	runCtx, cancel := context.WithTimeout(ctx, profileReferenceRunTimeout)
	refs, err := s.countProfileReferences(runCtx, brokerID, globalDir)
	cancel()
	if err != nil {
		p.lastErr = err.Error()
		slog.Error("Built-in profile references could not be counted at start; retaining built-in profiles conservatively until they can be", "error", err)
		overlay.EnableConservativeRetention()
	} else {
		overlay.SetProfileReferences(refs)
	}
	p.mu.Lock()
	p.overlay, p.brokerID, p.globalDir = overlay, brokerID, globalDir
	p.mu.Unlock()
	p.runMu.Unlock()
}

// triggerProfileReferences asks the worker for a count. It never blocks: a
// count already queued absorbs it, and with no worker it is a no-op.
func (s *Server) triggerProfileReferences() {
	if s == nil {
		return
	}
	select {
	case s.profileRefs.triggerChan() <- struct{}{}:
	default:
	}
}

// runProfileReferenceWorker runs counts on trigger until ctx ends.
// StartPropagation runs it under the propagation lifecycle.
func (s *Server) runProfileReferenceWorker(ctx context.Context) {
	if s == nil {
		return
	}
	ch := s.profileRefs.triggerChan()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			s.runProfileReferenceCount(ctx)
		}
	}
}

// runProfileReferenceCount runs one count and hands it to the overlay. A
// failure (or a panic) keeps the previous references. A no-op until
// EnableProfileReferences published a target.
func (s *Server) runProfileReferenceCount(ctx context.Context) {
	p := &s.profileRefs
	p.runMu.Lock()
	defer p.runMu.Unlock()
	overlay, brokerID, globalDir := p.target()
	if overlay == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Built-in profile reference count panicked; keeping the previous references", "panic", r)
		}
	}()
	runCtx, cancel := context.WithTimeout(ctx, profileReferenceRunTimeout)
	defer cancel()
	refs, err := s.countProfileReferences(runCtx, brokerID, globalDir)
	if err != nil {
		if ctx.Err() == nil && err.Error() != p.lastErr {
			p.lastErr = err.Error()
			slog.Error("Built-in profile references could not be counted; keeping the previous references (logged again only when the error changes)", "error", err)
		}
		return
	}
	if p.lastErr != "" {
		p.lastErr = ""
		slog.Info("Built-in profile references counted again")
	}
	overlay.SetProfileReferences(refs)
}

// linkProjectProvider is the one way pkg/hub links a broker to a project: it
// stores the provider row and then records the placement for built-in
// profile retention (onColocatedPlacement), so no link site can forget it.
func (s *Server) linkProjectProvider(ctx context.Context, provider *store.ProjectProvider) error {
	if err := s.store.AddProjectProvider(ctx, provider); err != nil {
		return err
	}
	s.onColocatedPlacement(ctx, provider.ProjectID, provider.BrokerID, "")
	return nil
}

// onColocatedPlacement records, at once, the references a placement on the
// co-located broker adds: called after every pkg/hub provider link (with
// agentProfile "") and on an agent move (with the moved agent's saved
// profile, whether or not the move linked the project). For brokerID the
// co-located broker it adds the project's activeProfile and agentProfile to
// the overlay's references (they become retained before the caller
// dispatches) and triggers a count. A no-op for any other broker.
func (s *Server) onColocatedPlacement(ctx context.Context, projectID, brokerID, agentProfile string) {
	overlay, coID, _ := s.profileRefs.target()
	if overlay == nil || brokerID == "" || brokerID != coID {
		return
	}
	if projectID != "" {
		if project, err := s.store.GetProject(ctx, projectID); err == nil {
			overlay.AddProjectReference(project.Annotations[projectSettingActiveProfile])
		}
	}
	overlay.AddAgentReference(agentProfile)
	s.triggerProfileReferences()
}

// addColocatedAgentReference records, at once, an agent reference to
// profile for an agent placed on brokerID, when that is the co-located
// broker, and triggers a count.
func (s *Server) addColocatedAgentReference(brokerID, profile string) {
	overlay, coID, _ := s.profileRefs.target()
	if overlay == nil || brokerID == "" || brokerID != coID {
		return
	}
	overlay.AddAgentReference(profile)
	s.triggerProfileReferences()
}

// onProjectActiveProfileSaved records a saved project's activeProfile at
// once when the co-located broker provides the project, and triggers a
// count.
func (s *Server) onProjectActiveProfileSaved(ctx context.Context, project *store.Project) {
	overlay, coID, _ := s.profileRefs.target()
	if overlay == nil || project == nil {
		return
	}
	if _, err := s.store.GetProjectProvider(ctx, project.ID, coID); err == nil {
		overlay.AddProjectReference(project.Annotations[projectSettingActiveProfile])
	}
	s.triggerProfileReferences()
}

// countProfileReferences reads the store and the co-located broker's agent
// directories and counts the references (config.CountProfileReferences).
//
// An agent's saved profile is read from its agent-info.json on this host
// (agent.SavedProfileOnDisk), at the project path dispatch would send and
// the broker would resolve (config.ProjectDispatchLocation, then
// config.BrokerProjectPath). agent-info.json, not the image provenance
// record, is the source: both are written from the same profile at
// provision, and agent-info.json only gains a profile later when it had
// none, so it is never less specific. When the file is missing (never
// provisioned here), or the agent is unplaced, the hub's
// AppliedConfig.Profile is counted: that is what the first start sends.
// Agents on flat Runtime Brokers carry their instance's broker ID, never
// the legacy co-located one, so the broker filter excludes them.
func (s *Server) countProfileReferences(ctx context.Context, brokerID, globalDir string) (config.ProfileReferences, error) {
	providers, err := s.store.GetBrokerProjects(ctx, brokerID)
	if err != nil {
		return config.ProfileReferences{}, fmt.Errorf("list the co-located broker's projects: %w", err)
	}
	localPaths := make(map[string]string, len(providers))
	for _, pv := range providers {
		localPaths[pv.ProjectID] = pv.LocalPath
	}

	projects := make(map[string]*store.Project)
	var projectRows []config.ProfileRefProject
	cursor := ""
	for {
		page, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: profileReferencePageSize, Cursor: cursor, SkipTotalCount: true})
		if err != nil {
			return config.ProfileReferences{}, fmt.Errorf("list projects: %w", err)
		}
		for i := range page.Items {
			pr := &page.Items[i]
			projects[pr.ID] = pr
			_, provided := localPaths[pr.ID]
			projectRows = append(projectRows, config.ProfileRefProject{
				ID:                  pr.ID,
				ActiveProfile:       pr.Annotations[projectSettingActiveProfile],
				ProvidedByColocated: provided,
			})
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	var agentRows []config.ProfileRefAgent
	missing, unreadable := 0, 0
	cursor = ""
	for {
		page, err := s.store.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{Limit: profileReferencePageSize, Cursor: cursor, SkipTotalCount: true})
		if err != nil {
			return config.ProfileReferences{}, fmt.Errorf("list agents: %w", err)
		}
		for i := range page.Items {
			a := &page.Items[i]
			if a.RuntimeBrokerID != "" && a.RuntimeBrokerID != brokerID {
				continue // counted nowhere; skip before any disk read
			}
			hubProfile := ""
			if a.AppliedConfig != nil {
				hubProfile = a.AppliedConfig.Profile
			}
			row := config.ProfileRefAgent{SavedProfile: hubProfile, RuntimeBrokerID: a.RuntimeBrokerID}
			if a.RuntimeBrokerID == brokerID {
				if pr := projects[a.ProjectID]; pr != nil {
					path, slug := config.ProjectDispatchLocation(localPaths[pr.ID], pr.Slug, isGlobalHubProject(pr.Slug))
					projectPath := config.BrokerProjectPath(globalDir, path, slug)
					if projectPath != "" {
						profile, state := agent.SavedProfileOnDisk(projectPath, a.Slug, pr.IsSharedWorkspace(), pr.ID)
						switch state {
						case agent.SavedProfileFound:
							row.SavedProfile = profile
						case agent.SavedProfileUnreadable:
							// What start's runtime selection uses: the
							// provisioned profile, else "" (GetSavedProfile).
							unreadable++
							row.SavedProfile = profile
						default:
							missing++
						}
					}
				}
			}
			agentRows = append(agentRows, row)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	refs, ok := config.CountProfileReferences(brokerID, agentRows, projectRows)
	if !ok {
		return config.ProfileReferences{}, fmt.Errorf("no co-located broker ID")
	}
	refs.AgentInfoMissing = missing
	refs.AgentInfoUnreadable = unreadable
	slog.Debug("Built-in profile references counted",
		"agents", len(agentRows), "agent_info_missing", missing, "agent_info_unreadable", unreadable)
	return refs, nil
}
