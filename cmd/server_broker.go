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
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// hostDetection is what detectHostGCPIdentity read from the metadata server.
type hostDetection struct {
	// Email is the host's default service account (lowercased), when
	// reported.
	Email string
	// ProjectID is the host's GCP project, when reported.
	ProjectID string
	// Outcome is the store.HostObservation* outcome; empty is derived from
	// Email (present when set, otherwise unavailable).
	Outcome string
	// ObservedAt is when the metadata server was read.
	ObservedAt time.Time
}

// Observation is the host identity observation for the broker record.
func (d hostDetection) Observation() store.HostIdentityObservation {
	outcome := d.Outcome
	if outcome == "" {
		outcome = store.HostObservationUnavailable
		if d.Email != "" {
			outcome = store.HostObservationPresent
		}
	}
	at := d.ObservedAt
	if at.IsZero() {
		at = time.Now()
	}
	return store.HostIdentityObservation{Outcome: outcome, Email: d.Email, ObservedAt: at}
}

// detectHostGCPIdentity reads the host's GCP identity from the GCE metadata
// server (GCE, Cloud Run and GKE). A variable so tests can replace it.
var detectHostGCPIdentity = func(ctx context.Context) hostDetection {
	d := hostDetection{Outcome: store.HostObservationUnavailable, ObservedAt: time.Now()}
	if !metadata.OnGCE() {
		return d
	}
	email, err := metadata.EmailWithContext(ctx, "default")
	switch {
	case err != nil:
		var notDefined metadata.NotDefinedError
		if errors.As(err, &notDefined) {
			d.Outcome = store.HostObservationAbsent
		}
	case email == "":
		d.Outcome = store.HostObservationAbsent
	default:
		d.Outcome = store.HostObservationPresent
		d.Email = strings.ToLower(email)
		log.Printf("Auto-detected GCP host service account: %s", d.Email)
	}
	if pid, err := metadata.ProjectIDWithContext(ctx); err == nil && pid != "" {
		d.ProjectID = pid
	}
	return d
}

// detectedSource is the GCP host source for a pair read from the metadata
// server: GCPHostSourceDetected when anything was read, otherwise "".
func detectedSource(email, project string) string {
	if email == "" && project == "" {
		return ""
	}
	return store.GCPHostSourceDetected
}

// refreshDetectedGCPHost refreshes the co-located broker's GCP host pair
// from the metadata server on every start, so the record follows a change
// of the host's service account. A value set or cleared through the update
// API is kept: the detected value is then only logged. Nothing detected
// leaves the stored pair unchanged.
func refreshDetectedGCPHost(ctx context.Context, s store.Store, broker *store.RuntimeBroker, email, project string) error {
	if email == "" {
		return nil
	}
	if project == "" {
		project = broker.GCPHostProjectID
	}
	applied, err := s.SetRuntimeBrokerGCPHostDetected(ctx, broker.ID, email, project)
	if err != nil {
		return fmt.Errorf("failed to update the runtime broker GCP host fields: %w", err)
	}
	if !applied && !strings.EqualFold(email, broker.GCPHostServiceAccountEmail) {
		log.Printf("Warning: broker %s (%s): the GCP host service account set through the update API (%q) differs from the host's detected service account (%q); keeping the configured value",
			broker.Name, broker.ID, broker.GCPHostServiceAccountEmail, email)
	}
	return nil
}

// registerGlobalProjectAndBroker creates the global project and registers this
// runtime broker as a provider. This enables automatic agent handoff.
// Returns the effective broker ID, which may differ from the input if an
// existing broker was found by name (deduplication). workspaceStorage is the
// broker's workspace storage descriptor; nil leaves an existing record's
// descriptor unchanged. profileTypeSettings resolves each profile's runtime
// type (loadBrokerProfileTypeSettings); nil registers runtime keys as types.
// It repairs orphaned agents on the store's guards alone; the co-located
// broker registers through registerGlobalProjectAndBrokerPresence.
func registerGlobalProjectAndBroker(ctx context.Context, s store.Store, brokerID, brokerName, endpoint string, rt runtime.Runtime, autoProvide bool, settings *config.Settings, workspaceStorage *api.BrokerWorkspaceStorage, profileTypeSettings *config.VersionedSettings) (string, error) {
	return registerGlobalProjectAndBrokerPresence(ctx, s, nil, brokerID, brokerName, endpoint, rt, autoProvide, settings, workspaceStorage, profileTypeSettings)
}

// brokerPresentFunc reports whether the broker with the given ID is present
// (hub.Server.BrokerPresent, design v2.21.50 §3.4). An error means presence
// is unknown, not absent; the func logs its own errors (BrokerPresent logs
// and counts them), so the caller does not log them again.
type brokerPresentFunc func(ctx context.Context, brokerID string) (bool, error)

// registerGlobalProjectAndBrokerPresence is registerGlobalProjectAndBroker
// whose orphan repair also skips every agent whose broker is present
// (present, nil: the store's guards alone). Project defaults are repointed
// only from brokers found not present, and those are the only brokers marked
// offline. At startup the conduit registry may not be open yet; the legacy
// presence signals then decide, as BrokerPresent documents.
func registerGlobalProjectAndBrokerPresence(ctx context.Context, s store.Store, present brokerPresentFunc, brokerID, brokerName, endpoint string, rt runtime.Runtime, autoProvide bool, settings *config.Settings, workspaceStorage *api.BrokerWorkspaceStorage, profileTypeSettings *config.VersionedSettings) (string, error) {
	// Check if global project already exists
	globalProject, err := s.GetProjectBySlug(ctx, GlobalProjectName)
	if err != nil && err != store.ErrNotFound {
		return brokerID, fmt.Errorf("failed to check for global project: %w", err)
	}

	// Create global project if it doesn't exist (without DefaultRuntimeBrokerID yet)
	projectNeedsDefaultBroker := false
	if globalProject == nil {
		globalProject = &store.Project{
			ID:   api.NewUUID(),
			Name: "Global",
			Slug: GlobalProjectName,
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
	profiles := buildStoreBrokerProfiles(settings, profileTypeSettings, runtimeType, rt)

	// The broker's own active/default profile name, so the hub can resolve
	// an agent dispatch with no explicit profile against this registration
	// data instead of guessing — see store.RuntimeBroker.DefaultProfile's
	// doc comment for why this is registration-time data, not a live view
	// of the broker's own dispatch-time settings. Empty when settings define
	// no active profile — buildStoreBrokerProfiles falls back to a single
	// "default" profile in that case, which the hub resolves without
	// needing this field (pkg/hub/default_gcp_identity.go).
	var defaultProfile string
	if settings != nil {
		defaultProfile = settings.ActiveProfile
	}

	broker, err := s.GetRuntimeBroker(ctx, brokerID)
	if err != nil && err != store.ErrNotFound {
		return brokerID, fmt.Errorf("failed to check for runtime broker: %w", err)
	}
	// A flat Runtime Broker row is never re-registered by this legacy path
	// (flat contract R4): its target, name and profiles stay as stored.
	if broker.IsFlat() {
		return brokerID, fmt.Errorf("%s: Runtime Broker %s is a flat Runtime Broker serving runtime target %s; the legacy embedded registration cannot re-register it",
			hub.ErrCodeRuntimeTargetChanged, brokerID, broker.RuntimeTarget.ID)
	}

	// If not found by ID, try to find an existing broker with the same name
	// to prevent duplicate registrations when the broker ID changes (e.g.,
	// settings file recreated, format migration, or database reset). Only
	// legacy rows are candidates: a flat row is never adopted by name, and
	// neither is a row with a recorded owner (only the Hub-managed row the
	// co-located server wrote itself).
	if broker == nil && brokerName != "" {
		existingByName, nameErr := s.GetLegacyRuntimeBrokerByName(ctx, brokerName)
		if nameErr != nil && nameErr != store.ErrNotFound {
			return brokerID, fmt.Errorf("failed to check for runtime broker by name: %w", nameErr)
		}
		if existingByName != nil && existingByName.CreatedBy != "" {
			log.Printf("Warning: broker %q (ID: %s) has a recorded owner and is not adopted as the co-located broker; registering a new record",
				brokerName, existingByName.ID)
			existingByName = nil
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

	// Auto-detect host service account from the GCE metadata server.
	// This works on GCE, Cloud Run, and GKE, and populates the fields
	// that the passthrough identity gate requires.
	detected := detectHostGCPIdentity(ctx)
	detectedSAEmail, detectedProjectID := detected.Email, detected.ProjectID

	if broker == nil {
		// Creating a legacy row next to a flat row with the same name or
		// slug is refused (flat contract R4): no duplicate is created.
		conflict, conflictErr := store.RuntimeBrokerNameConflict(ctx, s, brokerName, api.Slugify(brokerName), brokerID, true)
		if conflictErr != nil {
			return brokerID, fmt.Errorf("failed to check Runtime Broker name conflicts: %w", conflictErr)
		}
		if conflict != nil {
			return brokerID, fmt.Errorf("%s: Runtime Broker name %q (slug %q) is already used by a flat Runtime Broker; choose another name",
				hub.ErrCodeRuntimeBrokerNameConflict, brokerName, api.Slugify(brokerName))
		}
		broker = &store.RuntimeBroker{
			ID:                         brokerID,
			Name:                       brokerName,
			Slug:                       api.Slugify(brokerName),
			Version:                    hub.HubBuildToken(),
			Status:                     store.BrokerStatusOnline,
			ConnectionState:            "connected",
			Endpoint:                   endpoint,
			AutoProvide:                autoProvide,
			GCPHostServiceAccountEmail: detectedSAEmail,
			GCPHostProjectID:           detectedProjectID,
			GCPHostSource:              detectedSource(detectedSAEmail, detectedProjectID),
			Capabilities:               storeBrokerCapabilities(runtimebroker.StaticCapabilities(rt, config.ImageRegistryFromEnv())),
			Profiles:                   profiles,
			DefaultProfile:             defaultProfile,
			WorkspaceStorage:           workspaceStorage,
			Labels:                     brokerLabels,
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
		// The embedded broker runs in this hub binary, so its version is
		// the hub's build token, refreshed on every start (ptone/scion#5972).
		broker.Version = hub.HubBuildToken()
		// Update profiles from settings (may have changed)
		broker.Profiles = profiles
		broker.DefaultProfile = defaultProfile
		// Design §3.4 Amendment A2.2(b): refresh capabilities on every re-registration, not
		// just at create. The embedded broker's capability set is fixed by
		// the hub binary it runs in (not negotiated like a remote broker's),
		// so there is no reason for it to ever be stale — but a record
		// created before Reprovision existed would otherwise keep it unset
		// forever, since CompleteBrokerJoin (the remote-broker capability
		// path) never runs for the embedded broker. That produced a
		// permanent false 412 on `scion reincarnate` for every embedded
		// deployment.
		broker.Capabilities = storeBrokerCapabilities(runtimebroker.StaticCapabilities(rt, config.ImageRegistryFromEnv()))
		// A nil descriptor (not reported) keeps the stored one; the
		// broker's heartbeats refresh it either way.
		if workspaceStorage != nil {
			broker.WorkspaceStorage = workspaceStorage
		}
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
		if err := refreshDetectedGCPHost(ctx, s, broker, detectedSAEmail, detectedProjectID); err != nil {
			return brokerID, err
		}
	}
	hub.RecordHostObservation(ctx, s, brokerID, detected.Observation())

	// Re-assign agents orphaned by broker ID changes.
	// An agent is orphaned when its RuntimeBrokerID points to a broker that
	// is offline/missing AND the agent is not in a terminal state.
	// Guard: agents under active remote brokers are NOT reassigned, and
	// neither are agents whose broker is present.
	orphaned, orphanErr := s.FindOrphanedAgents(ctx, brokerID)
	if orphanErr == nil {
		orphaned = withoutPresentBrokers(ctx, orphaned, present)
	}
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

// loadBrokerProfileTypeSettings loads the global settings (plus the DB
// settings overlay, when installed) that buildStoreBrokerProfiles resolves
// profile runtime types from. On failure it logs a warning and returns nil:
// profiles then register their runtime key as their type, and registration
// goes ahead.
func loadBrokerProfileTypeSettings() *config.VersionedSettings {
	vs, _, err := config.LoadGlobalSettingsWithOverlay()
	if err != nil {
		log.Printf("Warning: could not load settings to resolve broker profile runtime types; registering each profile's runtime key as its type: %v", err)
		return nil
	}
	return vs
}

// buildStoreBrokerProfiles builds store.BrokerProfile objects from settings.Profiles.
// Each profile's Type is its resolved runtime type (the runtime entry's
// explicit type, else its key), so the local-only filter and the attach
// answer below also use the resolved type. Types resolve from
// profileTypeSettings (see loadBrokerProfileTypeSettings); nil keeps each
// profile's runtime key as its type.
// If no profiles are defined in settings, returns a default profile with the detected runtime type.
// When the detected default runtime is not local-only (e.g. cloudrun, kubernetes),
// profiles referencing local-only runtimes (docker, podman, container) are
// filtered out because no local daemon is available in those environments.
//
// defaultRuntime is the live instance this process already constructed for
// defaultRuntimeType — the embedded broker always has one, since it's about
// to serve agents with it. A profile resolving to that same type gets its
// real runtime.HasAttachSupport answer; any other profile gets nil
// (unknown, read as supported), since this function never constructs a
// runtime just to answer that field.
func buildStoreBrokerProfiles(settings *config.Settings, profileTypeSettings *config.VersionedSettings, defaultRuntimeType string, defaultRuntime runtime.Runtime) []store.BrokerProfile {
	attachForType := func(rtType string) *bool {
		if rtType != defaultRuntimeType || defaultRuntime == nil {
			return nil
		}
		v := runtime.HasAttachSupport(defaultRuntime)
		return &v
	}

	// If no settings or no profiles defined, return a default profile
	if settings == nil || len(settings.Profiles) == 0 {
		return []store.BrokerProfile{
			{Name: "default", Type: defaultRuntimeType, Available: true, Attach: attachForType(defaultRuntimeType)},
		}
	}

	var profiles []store.BrokerProfile
	for name, profileCfg := range settings.Profiles {
		// Determine runtime type from the profile's runtime reference:
		// the resolved type of the runtime entry it names, else the key.
		runtimeType := profileCfg.Runtime
		if runtimeType == "" {
			runtimeType = defaultRuntimeType
		} else {
			runtimeType = resolveProfileRuntimeType(profileTypeSettings, name, profileCfg.Runtime)
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
			Attach:    attachForType(runtimeType),
		})
	}

	if len(profiles) == 0 {
		profiles = []store.BrokerProfile{
			{Name: "default", Type: defaultRuntimeType, Available: true, Attach: attachForType(defaultRuntimeType)},
		}
	}

	return profiles
}

// withoutPresentBrokers drops from orphaned every agent whose broker is
// present. A broker whose presence cannot be read is kept as present: its
// agents are not moved, and the error is left to present's own log line.
// present nil returns orphaned unchanged.
func withoutPresentBrokers(ctx context.Context, orphaned []*store.Agent, present brokerPresentFunc) []*store.Agent {
	if present == nil || len(orphaned) == 0 {
		return orphaned
	}
	known := make(map[string]bool)
	kept := orphaned[:0:0]
	for _, a := range orphaned {
		isPresent, ok := known[a.RuntimeBrokerID]
		if !ok {
			var err error
			isPresent, err = present(ctx, a.RuntimeBrokerID)
			if err != nil {
				isPresent = true
			}
			known[a.RuntimeBrokerID] = isPresent
		}
		if !isPresent {
			kept = append(kept, a)
		}
	}
	return kept
}
