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
	"fmt"
	"log"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
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

	brokerLabels := map[string]string{
		"scion.io/broker-role": "embedded",
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
			Labels:   brokerLabels,
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
		// Ensure deployment-type labels are set on re-registration
		if broker.Labels == nil {
			broker.Labels = brokerLabels
		} else {
			for k, v := range brokerLabels {
				broker.Labels[k] = v
			}
		}
		if err := s.UpdateRuntimeBroker(ctx, broker); err != nil {
			return brokerID, fmt.Errorf("failed to update runtime broker: %w", err)
		}
	}

	// Re-assign agents orphaned by broker ID changes.
	// An agent is orphaned when its RuntimeBrokerID points to a broker that
	// is offline/missing AND the agent is not in a terminal state.
	// Guard: agents under active remote brokers are NOT reassigned.
	orphaned, orphanErr := s.FindOrphanedAgents(ctx, brokerID)
	if orphanErr != nil {
		log.Printf("Warning: failed to query orphaned agents: %v", orphanErr)
	} else if len(orphaned) > 0 {
		// Collect distinct old broker IDs for downstream repair.
		oldBrokerIDs := make(map[string]struct{})
		for _, a := range orphaned {
			if a.RuntimeBrokerID != "" && a.RuntimeBrokerID != brokerID {
				oldBrokerIDs[a.RuntimeBrokerID] = struct{}{}
			}
		}

		reassigned, reassignErr := s.ReassignAgentsToBroker(ctx, orphaned, brokerID)
		if reassignErr != nil {
			log.Printf("Warning: failed to re-assign %d orphaned agent(s): %v", len(orphaned), reassignErr)
		} else {
			log.Printf("NOTICE: re-assigned %d orphaned agent(s) to broker %s", reassigned, brokerID)
		}

		// Update projects whose default broker points to an offline/missing broker.
		for oldID := range oldBrokerIDs {
			updated, err := s.ReassignProjectBroker(ctx, oldID, brokerID)
			if err != nil {
				log.Printf("Warning: failed to reassign projects from broker %s: %v", oldID, err)
			} else if updated > 0 {
				log.Printf("NOTICE: updated %d project(s) default broker from %s to %s", updated, oldID, brokerID)
			}
		}

		// Mark stale broker records as offline so they don't appear active.
		for oldID := range oldBrokerIDs {
			if err := s.MarkBrokerOffline(ctx, oldID); err != nil {
				if err != store.ErrNotFound {
					log.Printf("Warning: failed to mark broker %s offline: %v", oldID, err)
				}
			} else {
				log.Printf("NOTICE: marked stale broker %s as offline", oldID)
			}
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

// isLocalOnlyRuntime returns true for runtime types that require a local daemon
// or hardware and cannot function in a hosted cloud environment.
func isLocalOnlyRuntime(runtimeType string) bool {
	switch runtimeType {
	case "docker", "podman", "container":
		return true
	}
	return false
}

// buildStoreBrokerProfiles builds store.BrokerProfile objects from settings.Profiles.
// If no profiles are defined in settings, returns a default profile with the detected runtime type.
// When the detected default runtime is not local-only (e.g. cloudrun, kubernetes),
// profiles referencing local-only runtimes (docker, podman, container) are
// filtered out because no local daemon is available in those environments.
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

		if !isLocalOnlyRuntime(defaultRuntimeType) && isLocalOnlyRuntime(runtimeType) {
			continue
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

	if len(profiles) == 0 {
		profiles = []store.BrokerProfile{
			{Name: "default", Type: defaultRuntimeType, Available: true},
		}
	}

	return profiles
}
