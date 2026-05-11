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

// registerGlobalGroveAndBroker creates the global project and registers this
// runtime broker as a provider. This enables automatic agent handoff.
// Returns the effective broker ID, which may differ from the input if an
// existing broker was found by name (deduplication).
func registerGlobalProjectAndBroker(ctx context.Context, s store.Store, brokerID, brokerName, endpoint string, rt runtime.Runtime, autoProvide bool, settings *config.Settings) (string, error) {
	// Check if global project already exists
	globalGrove, err := s.GetProjectBySlug(ctx, GlobalGroveName)
	if err != nil && err != store.ErrNotFound {
		return brokerID, fmt.Errorf("failed to check for global project: %w", err)
	}

	// Create global project if it doesn't exist (without DefaultRuntimeBrokerID yet)
	projectNeedsDefaultBroker := false
	if globalGrove == nil {
		globalGrove = &store.Project{
			ID:         api.NewUUID(),
			Name:       "Global",
			Slug:       GlobalGroveName,
			Visibility: store.VisibilityPrivate,
			Labels: map[string]string{
				"scion.io/system": "true",
				"scion.io/global": "true",
			},
		}

		if err := s.CreateProject(ctx, globalGrove); err != nil {
			return brokerID, fmt.Errorf("failed to create global project: %w", err)
		}
		projectNeedsDefaultBroker = true
	} else if globalGrove.DefaultRuntimeBrokerID == "" {
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
		if err := s.UpdateRuntimeBroker(ctx, broker); err != nil {
			return brokerID, fmt.Errorf("failed to update runtime broker: %w", err)
		}
	}

	// Now that the runtime broker exists, set it as the default for the project
	if projectNeedsDefaultBroker {
		globalGrove.DefaultRuntimeBrokerID = brokerID
		if err := s.UpdateProject(ctx, globalGrove); err != nil {
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
		ProjectID:    globalGrove.ID,
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
		if err := s.UpdateProviderStatus(ctx, globalGrove.ID, brokerID, store.BrokerStatusOnline); err != nil {
			log.Printf("Warning: failed to update provider status: %v", err)
		}
	}

	return brokerID, nil
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
