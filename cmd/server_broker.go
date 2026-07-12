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

package cmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// registerGlobalProjectAndBroker creates the global project and registers this
// runtime broker as a provider. This enables automatic agent handoff.
// Returns the effective broker ID, which may differ from the input if an
// existing broker was found by name (deduplication).
func registerGlobalProjectAndBroker(ctx context.Context, s store.Store, brokerID, brokerName, endpoint string, rt runtime.Runtime, autoProvide bool, settings *config.Settings) (string, error) {
	// Check if global project already exists
	globalProject, err := s.GetProjectBySlug(ctx, GlobalProjectName)
	if err != nil && err != store.ErrNotFound {
		return brokerID, fmt.Errorf("failed to check for global project: %w", err)
	}

	// Create global project if it doesn't exist (without DefaultRuntimeBrokerID yet)
	projectNeedsDefaultBroker := false
	if globalProject == nil {
		globalProject = &store.Project{
			ID:         api.NewUUID(),
			Name:       "Global",
			Slug:       GlobalProjectName,
			Visibility: store.VisibilityPrivate,
			Labels: map[string]string{
				"scion.io/system": "true",
				"scion.io/global": "true",
			},
		}

		if err := s.CreateProject(ctx, globalProject); err != nil {
			return brokerID, fmt.Errorf("failed to create global project: %w", err)
		}
		projectNeedsDefaultBroker = true
	} else if globalProject.DefaultRuntimeBrokerID == "" {
		projectNeedsDefaultBroker = true
	}

	// Create or update the runtime broker record (must happen before setting as default)
	runtimeType := "docker"
	if rt != nil {
		runtimeType = rt.Name()
	}

	// Build profiles from settings, falling back to a default profile if none defined
	profiles := buildStoreBrokerProfiles(settings, runtimeType)

	broker, err := s.GetRuntimeBroker(ctx, brokerID)
	if err != nil && err != store.ErrNotFound {
		return brokerID, fmt.Errorf("failed to check for runtime broker: %w", err)
	}

	// If not found by ID, try to find an existing broker with the same name
	// to prevent duplicate registrations when the broker ID changes (e.g.,
	// settings file recreated, format migration, or database reset).
	if broker == nil && brokerName != "" {
		existingByName, nameErr := s.GetRuntimeBrokerByName(ctx, brokerName)
		if nameErr != nil && nameErr != store.ErrNotFound {
			return brokerID, fmt.Errorf("failed to check for runtime broker by name: %w", nameErr)
		}
		if existingByName != nil {
			log.Printf("Found existing broker by name %q (ID: %s), reusing instead of creating duplicate", brokerName, existingByName.ID)
			broker = existingByName
			brokerID = existingByName.ID
		}
	}

	if broker == nil {
		broker = &store.RuntimeBroker{
			ID:              brokerID,
			Name:            brokerName,
			Slug:            api.Slugify(brokerName),
			Version:         "0.1.0",
			Status:          store.BrokerStatusOnline,
			ConnectionState: "connected",
			Endpoint:        endpoint,
			AutoProvide:     autoProvide,
			Capabilities: &store.BrokerCapabilities{
				WebPTY: false,
				Sync:   true,
				Attach: true,
			},
			Profiles: profiles,
			Labels: map[string]string{
				"scion.io/broker-type": "hosted",
			},
		}

		if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
			return brokerID, fmt.Errorf("failed to create runtime broker: %w", err)
		}
	} else {
		// Update existing broker status, endpoint, auto-provide setting, and profiles
		broker.Status = store.BrokerStatusOnline
		broker.ConnectionState = "connected"
		broker.Endpoint = endpoint
		broker.AutoProvide = autoProvide
		broker.LastHeartbeat = time.Now()
		// Update profiles from settings (may have changed)
		broker.Profiles = profiles
		// Unconditionally set the hosted label. This is safe because
		// registerGlobalProjectAndBroker is only called for co-located
		// (hub-embedded) brokers; external brokers register through
		// CreateBrokerRegistration in brokerauth.go instead.
		if broker.Labels == nil {
			broker.Labels = make(map[string]string)
		}
		broker.Labels["scion.io/broker-type"] = "hosted"
		if err := s.UpdateRuntimeBroker(ctx, broker); err != nil {
			return brokerID, fmt.Errorf("failed to update runtime broker: %w", err)
		}
	}

	// Now that the runtime broker exists, set it as the default for the project
	if projectNeedsDefaultBroker {
		globalProject.DefaultRuntimeBrokerID = brokerID
		if err := s.UpdateProject(ctx, globalProject); err != nil {
			log.Printf("Warning: failed to set default runtime broker for global project: %v", err)
		}
	}

	// Get the global project path (~/.scion)
	globalPath, err := config.GetGlobalDir()
	if err != nil {
		log.Printf("Warning: failed to get global project path: %v", err)
		globalPath = "" // Will work but agents may not find the right path
	}

	// Add runtime broker as provider to global project
	provider := &store.ProjectProvider{
		ProjectID:  globalProject.ID,
		BrokerID:   brokerID,
		BrokerName: brokerName,
		LocalPath:  globalPath, // ~/.scion for the global project
		Status:     store.BrokerStatusOnline,
		LastSeen:   time.Now(),
	}

	if err := s.AddProjectProvider(ctx, provider); err != nil {
		// Ignore duplicate provider errors
		if err != store.ErrAlreadyExists {
			return brokerID, fmt.Errorf("failed to add project provider: %w", err)
		}
		// Update provider status
		if err := s.UpdateProviderStatus(ctx, globalProject.ID, brokerID, store.BrokerStatusOnline); err != nil {
			log.Printf("Warning: failed to update provider status: %v", err)
		}
	}

	return brokerID, nil
}

func registerSystemProject(ctx context.Context, s store.Store, brokerID, brokerName string, cfg config.SystemProjectConfig) error {
	if !cfg.Enabled {
		return nil
	}

	workspacePath, err := config.GetSystemProjectDir(cfg.WorkspacePath)
	if err != nil {
		return fmt.Errorf("failed to resolve system project path: %w", err)
	}
	if err := provisionSystemProjectWorkspace(workspacePath); err != nil {
		return err
	}

	project, err := s.GetProjectBySlug(ctx, SystemProjectName)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("failed to check for system project: %w", err)
	}

	if project == nil {
		project = &store.Project{
			ID:         api.NewUUID(),
			Name:       "System",
			Slug:       SystemProjectName,
			Visibility: store.VisibilityPrivate,
			Labels: map[string]string{
				projectcompat.LabelScionSystem:   "true",
				projectcompat.LabelSystemProject: "true",
			},
			DefaultRuntimeBrokerID: brokerID,
			SharedDirs: []api.SharedDir{
				{Name: "shared", ReadOnly: false, InWorkspace: false},
			},
		}
		if err := s.CreateProject(ctx, project); err != nil {
			return fmt.Errorf("failed to create system project: %w", err)
		}
	} else if backfillSystemProject(project, brokerID) {
		if err := s.UpdateProject(ctx, project); err != nil {
			return fmt.Errorf("failed to update system project: %w", err)
		}
	}

	provider := &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   brokerID,
		BrokerName: brokerName,
		LocalPath:  workspacePath,
		Status:     store.BrokerStatusOnline,
		LastSeen:   time.Now(),
	}
	if err := s.AddProjectProvider(ctx, provider); err != nil {
		if !errors.Is(err, store.ErrAlreadyExists) {
			return fmt.Errorf("failed to add system project provider: %w", err)
		}
		if existing, getErr := s.GetProjectProvider(ctx, project.ID, brokerID); getErr == nil && existing.LocalPath != workspacePath {
			log.Printf("Error: system project workspace path differs: stored=%q current=%q; restart with correct path or update provider manually", existing.LocalPath, workspacePath)
		}
		if err := s.UpdateProviderStatus(ctx, project.ID, brokerID, store.BrokerStatusOnline); err != nil {
			log.Printf("Warning: failed to update system project provider status: %v", err)
		}
	}

	ensureProjectMembersGroupAndPolicy(ctx, s, project)
	return nil
}

func provisionSystemProjectWorkspace(workspacePath string) error {
	for _, dir := range []string{
		filepath.Join(workspacePath, "shared"),
		filepath.Join(workspacePath, "shared", "notes"),
		filepath.Join(workspacePath, "shared", "runbooks"),
		filepath.Join(workspacePath, "agents"),
		filepath.Join(workspacePath, "config"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create system project directory %s: %w", dir, err)
		}
	}

	journalPath := filepath.Join(workspacePath, "shared", "journal.md")
	if _, err := os.Stat(journalPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to stat system project journal: %w", err)
	}
	if err := os.WriteFile(journalPath, []byte("# System Project Journal\n\n"), 0644); err != nil {
		return fmt.Errorf("failed to seed system project journal: %w", err)
	}
	return nil
}

func backfillSystemProject(project *store.Project, brokerID string) bool {
	changed := false
	if project.Labels == nil {
		project.Labels = map[string]string{}
	}
	if project.Labels[projectcompat.LabelScionSystem] != "true" {
		project.Labels[projectcompat.LabelScionSystem] = "true"
		changed = true
	}
	if project.Labels[projectcompat.LabelSystemProject] != "true" {
		project.Labels[projectcompat.LabelSystemProject] = "true"
		changed = true
	}
	if project.DefaultRuntimeBrokerID == "" {
		project.DefaultRuntimeBrokerID = brokerID
		changed = true
	}
	if !hasSharedDir(project.SharedDirs, "shared") {
		project.SharedDirs = append(project.SharedDirs, api.SharedDir{Name: "shared", ReadOnly: false, InWorkspace: false})
		changed = true
	}
	return changed
}

func hasSharedDir(sharedDirs []api.SharedDir, name string) bool {
	for _, dir := range sharedDirs {
		if dir.Name == name {
			return true
		}
	}
	return false
}

func ensureProjectMembersGroupAndPolicy(ctx context.Context, s store.Store, project *store.Project) {
	membersSlug := "project:" + project.Slug + ":members"
	membersGroup := &store.Group{
		ID:        api.NewUUID(),
		Name:      project.Name + " Members",
		Slug:      membersSlug,
		GroupType: store.GroupTypeExplicit,
		ProjectID: project.ID,
		OwnerID:   project.OwnerID,
		CreatedBy: project.CreatedBy,
	}
	if err := s.CreateGroup(ctx, membersGroup); err != nil {
		if !errors.Is(err, store.ErrAlreadyExists) {
			log.Printf("Warning: failed to create project members group for %s: %v", project.Slug, err)
			return
		}
		existing, lookupErr := s.GetGroupBySlug(ctx, membersSlug)
		if lookupErr != nil {
			log.Printf("Warning: failed to look up project members group for %s: %v", project.Slug, lookupErr)
			return
		}
		membersGroup = existing
		needsUpdate := false
		if membersGroup.ProjectID != project.ID {
			membersGroup.ProjectID = project.ID
			needsUpdate = true
		}
		if membersGroup.OwnerID == "" && project.OwnerID != "" {
			membersGroup.OwnerID = project.OwnerID
			needsUpdate = true
		}
		if needsUpdate {
			if updateErr := s.UpdateGroup(ctx, membersGroup); updateErr != nil {
				log.Printf("Warning: failed to update project members group for %s: %v", project.Slug, updateErr)
			}
		}
	}

	policyName := "project:" + project.Slug + ":member-create-agents"
	policy := &store.Policy{
		ID:           api.NewUUID(),
		Name:         policyName,
		Description:  "Allow project members to create and stop agents",
		ScopeType:    store.PolicyScopeProject,
		ScopeID:      project.ID,
		ResourceType: "agent",
		Actions:      []string{"create", "stop_all"},
		Effect:       store.PolicyEffectAllow,
	}
	if err := s.CreatePolicy(ctx, policy); err != nil {
		if !errors.Is(err, store.ErrAlreadyExists) {
			log.Printf("Warning: failed to create project members policy for %s: %v", project.Slug, err)
			return
		}
		existing, lookupErr := s.ListPolicies(ctx, store.PolicyFilter{Name: policyName}, store.ListOptions{Limit: 1})
		if lookupErr != nil || len(existing.Items) == 0 {
			log.Printf("Warning: failed to look up project members policy for %s: %v", project.Slug, lookupErr)
			return
		}
		policy = &existing.Items[0]
		needsUpdate := false
		if policy.ScopeID != project.ID {
			policy.ScopeID = project.ID
			needsUpdate = true
		}
		if !stringSliceContains(policy.Actions, "stop_all") {
			policy.Actions = append(policy.Actions, "stop_all")
			needsUpdate = true
		}
		if needsUpdate {
			if updateErr := s.UpdatePolicy(ctx, policy); updateErr != nil {
				log.Printf("Warning: failed to update project members policy for %s: %v", project.Slug, updateErr)
			}
		}
	}

	if err := s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID:      policy.ID,
		PrincipalType: store.PolicyPrincipalTypeGroup,
		PrincipalID:   membersGroup.ID,
	}); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		log.Printf("Warning: failed to bind project members policy for %s: %v", project.Slug, err)
	}
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// buildStoreBrokerProfiles builds store.BrokerProfile objects from settings.Profiles.
// If no profiles are defined in settings, returns a default profile with the detected runtime type.
func buildStoreBrokerProfiles(settings *config.Settings, defaultRuntimeType string) []store.BrokerProfile {
	// If no settings or no profiles defined, return a default profile
	if settings == nil || len(settings.Profiles) == 0 {
		return []store.BrokerProfile{
			{Name: "default", Type: defaultRuntimeType, Available: true},
		}
	}

	var profiles []store.BrokerProfile
	for name, profileCfg := range settings.Profiles {
		// Determine runtime type from the profile's runtime reference
		runtimeType := profileCfg.Runtime
		if runtimeType == "" {
			runtimeType = defaultRuntimeType
		}

		// Look up runtime config to get additional info (context, namespace for K8s)
		var context, namespace string
		if settings.Runtimes != nil {
			if rtCfg, ok := settings.Runtimes[profileCfg.Runtime]; ok {
				context = rtCfg.Context
				namespace = rtCfg.Namespace
			}
		}

		profiles = append(profiles, store.BrokerProfile{
			Name:      name,
			Type:      runtimeType,
			Available: true,
			Context:   context,
			Namespace: namespace,
		})
	}

	return profiles
}
