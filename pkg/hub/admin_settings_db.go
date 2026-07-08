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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
	yamlv3 "gopkg.in/yaml.v3"
)

// SectionMetadata carries per-section provenance metadata in the GET response
// (design §3.8, additive shape). The key "section_metadata" is chosen to not
// collide with any existing ServerConfigResponse field.
type SectionMetadata struct {
	Source    string     `json:"source"`               // "db", "file", or "default"
	Revision  int64      `json:"revision,omitempty"`   // DB revision (0 for file/default)
	UpdatedAt *time.Time `json:"updated_at,omitempty"` // last update time (DB only)
	UpdatedBy string     `json:"updated_by,omitempty"` // admin email (DB only)
}

// ServerConfigDBResponse extends the file-mode response with metadata for the
// postgres-mode GET endpoint. It embeds the original ServerConfigResponse and
// adds section_metadata and env_overrides.
type ServerConfigDBResponse struct {
	ServerConfigResponse

	// SectionMetadata maps section name to its provenance metadata.
	SectionMeta map[string]SectionMetadata `json:"section_metadata,omitempty"`

	// EnvOverrides lists Layer-1 koanf keys that are overridden by env vars
	// on this node — a drift warning for the admin UI.
	EnvOverrides []string `json:"env_overrides,omitempty"`
}

// ServerConfigUpdateDBRequest extends the update request with optional CAS
// support via expected_revisions. The body shape is additive — the web UI
// sends ServerConfigUpdateRequest today, and expected_revisions is optional
// (omitted = last-writer-wins, preserving current UI behavior).
//
// We chose an in-body map over If-Match headers because:
//   - A single PUT can touch multiple sections, each with its own revision.
//   - If-Match holds a single ETag, which doesn't map well to per-section CAS.
//   - The existing API has no ETag convention; adding one would be a breaking change.
type ServerConfigUpdateDBRequest struct {
	ServerConfigUpdateRequest

	// ExpectedRevisions maps section name → expected revision for CAS.
	// Omitted sections use last-writer-wins semantics.
	ExpectedRevisions map[string]int64 `json:"expected_revisions,omitempty"`
}

// handleGetServerConfigDB handles GET /api/v1/admin/server-config in postgres mode.
//
// Layer-1 sections come from OperationalSettings.Snapshot(); Layer-0 comes from
// the local GlobalConfig (settings.yaml). Section metadata shows provenance.
func (s *Server) handleGetServerConfigDB(w http.ResponseWriter, ops *OperationalSettings) {
	// Build the base response from the file (same as file mode) for Layer-0.
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to resolve settings directory", nil)
		return
	}

	settingsPath := filepath.Join(globalDir, "settings.yaml")
	data, err := os.ReadFile(settingsPath)
	if err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to read settings file", nil)
		return
	}

	var vs config.VersionedSettings
	if data != nil {
		if err := yamlv3.Unmarshal(data, &vs); err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to parse settings file", nil)
			return
		}
	}

	// Start with file-based response for Layer-0 fields.
	resp := ServerConfigDBResponse{
		ServerConfigResponse: ServerConfigResponse{
			ScionVersion:   version.Short(),
			ScionCommit:    version.GetCommit(),
			ScionBuildTime: version.GetBuildTime(),
			SchemaVersion:  vs.SchemaVersion,
			ActiveProfile:  vs.ActiveProfile,
			WorkspacePath:  vs.WorkspacePath,
			Server:         vs.Server,
			Runtimes:       vs.Runtimes,
			HarnessConfigs: vs.HarnessConfigs,
			Profiles:       vs.Profiles,
		},
	}

	if resp.ServerConfigResponse.SchemaVersion == "" {
		resp.ServerConfigResponse.SchemaVersion = "1"
	}

	// Overlay Layer-1 fields from the operational settings snapshot.
	snap := ops.Snapshot()
	applySnapshotToResponse(&resp.ServerConfigResponse, snap)

	// Build section metadata from the cache.
	resp.SectionMeta = s.buildSectionMetadata(ops)

	// Env overrides.
	overrides := ops.EnvOverriddenKeys()
	sort.Strings(overrides)
	resp.EnvOverrides = overrides

	// Mask sensitive fields — same logic as file mode.
	maskSensitiveFields(&resp.ServerConfigResponse)

	writeJSON(w, http.StatusOK, resp)
}

// applySnapshotToResponse writes Layer-1 snapshot values into the
// ServerConfigResponse, ensuring the response reflects the merged
// (env > DB > file > defaults) view.
func applySnapshotToResponse(resp *ServerConfigResponse, snap Layer1Snapshot) {
	// Access
	resp.DefaultTemplate = snap.DefaultTemplate
	resp.DefaultHarnessConfig = snap.DefaultHarnessConfig
	resp.ImageRegistry = snap.ImageRegistry
	resp.DefaultMaxTurns = snap.DefaultMaxTurns
	resp.DefaultMaxModelCalls = snap.DefaultMaxModelCalls
	resp.DefaultMaxDuration = snap.DefaultMaxDuration
	resp.DefaultResources = snap.DefaultResources

	// Telemetry
	if snap.TelemetryConfig != nil {
		resp.Telemetry = snap.TelemetryConfig
	}

	// Ensure server sub-structs exist.
	if resp.Server == nil {
		resp.Server = &config.V1ServerConfig{}
	}
	if resp.Server.Hub == nil {
		resp.Server.Hub = &config.V1ServerHubConfig{}
	}
	if resp.Server.Auth == nil {
		resp.Server.Auth = &config.V1AuthConfig{}
	}

	// Access fields
	resp.Server.Hub.AdminEmails = snap.AdminEmails
	resp.Server.Auth.UserAccessMode = snap.UserAccessMode
	resp.Server.Auth.AuthorizedDomains = snap.AuthorizedDomains

	// Lifecycle
	if snap.AutoSuspendStalled {
		b := true
		resp.Server.Hub.AutoSuspendStalled = &b
	}
	resp.Server.Hub.SoftDeleteRetention = snap.SoftDeleteRetention
	if snap.SoftDeleteRetainFiles {
		b := true
		resp.Server.Hub.SoftDeleteRetainFiles = &b
	}

	// Endpoints
	resp.Server.Hub.PublicURL = snap.PublicURL

	// GitHub App
	if resp.Server.GitHubApp == nil {
		resp.Server.GitHubApp = &config.V1GitHubAppConfig{}
	}
	resp.Server.GitHubApp.AppID = snap.GitHubAppID
	resp.Server.GitHubApp.APIBaseURL = snap.GitHubAPIBaseURL
	resp.Server.GitHubApp.WebhooksEnabled = snap.GitHubWebhooksEnabled
	resp.Server.GitHubApp.InstallationURL = snap.GitHubInstallationURL
	resp.Server.GitHubApp.PrivateKeyPath = snap.GitHubPrivateKeyPath

	// Notifications
	if len(snap.NotificationChannels) > 0 {
		resp.Server.NotificationChannels = snap.NotificationChannels
	}
}

// buildSectionMetadata queries the OperationalSettings cache to determine
// per-section provenance: "db" (present in DB), "file" (section absent from
// DB but present in settings.yaml fallback), or "default" (neither).
func (s *Server) buildSectionMetadata(ops *OperationalSettings) map[string]SectionMetadata {
	meta := make(map[string]SectionMetadata, len(opsettings.Registry))

	// Get DB rows for metadata.
	rows, err := ops.store.ListHubSettings(s.requestContext())
	if err != nil {
		slog.Error("Failed to list hub settings for metadata", "error", err)
		// Return empty metadata rather than failing the GET.
		return meta
	}

	rowsBySection := make(map[string]*store.HubSetting, len(rows))
	for i := range rows {
		if rows[i].Section != "_meta" {
			rowsBySection[rows[i].Section] = &rows[i]
		}
	}

	for _, sec := range opsettings.Registry {
		if row, ok := rowsBySection[sec.Name]; ok {
			t := row.UpdatedAt
			meta[sec.Name] = SectionMetadata{
				Source:    "db",
				Revision:  row.Revision,
				UpdatedAt: &t,
				UpdatedBy: row.UpdatedBy,
			}
		} else if s.sectionHasFileValues(ops, sec.Name) {
			meta[sec.Name] = SectionMetadata{
				Source: "file",
			}
		} else {
			meta[sec.Name] = SectionMetadata{
				Source: "default",
			}
		}
	}

	return meta
}

// sectionHasFileValues checks whether the file fallback koanf has any non-zero
// values for the given section's koanf paths.
func (s *Server) sectionHasFileValues(ops *OperationalSettings, sectionName string) bool {
	sec := opsettings.SectionByName(sectionName)
	if sec == nil || len(sec.KoanfPaths) == 0 {
		return false
	}
	for _, kp := range sec.KoanfPaths {
		if ops.fileFallback != nil && ops.fileFallback.Exists(kp) {
			return true
		}
	}
	return false
}

// requestContext returns a background context for internal queries. This is
// used for metadata queries that don't originate from an HTTP request.
func (s *Server) requestContext() context.Context {
	return context.Background()
}

// handlePutServerConfigDB handles PUT /api/v1/admin/server-config in postgres mode.
//
// It partitions incoming fields via the opsettings registry:
//   - Layer-1 fields → per-section docs → validate → OperationalSettings.Update
//   - Layer-0 fields → 422 rejection with offending key list
//
// Supports optional CAS via expected_revisions in the request body.
func (s *Server) handlePutServerConfigDB(w http.ResponseWriter, r *http.Request, ops *OperationalSettings) {
	var req ServerConfigUpdateDBRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
		return
	}

	caller := GetUserIdentityFromContext(r.Context())
	updatedBy := ""
	if caller != nil {
		updatedBy = caller.Email()
	}

	// Convert the update request into koanf keys to classify Layer-0 vs Layer-1.
	koanfKeys := extractKoanfKeysFromRequest(&req.ServerConfigUpdateRequest)

	// Classify keys.
	layer1BySec, layer0Keys := opsettings.ClassifyKeys(koanfKeys)

	// Reject if any Layer-0 keys are present — 422 before any write.
	if len(layer0Keys) > 0 {
		sort.Strings(layer0Keys)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]interface{}{
			"error":   "layer0_rejected",
			"message": "Bootstrap settings are managed via settings.yaml / deployment tooling; restart required.",
			"keys":    layer0Keys,
		})
		return
	}

	// Build per-section documents from the request.
	sectionDocs, err := buildSectionDocsFromRequest(&req.ServerConfigUpdateRequest, layer1BySec)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to build section documents", nil)
		return
	}

	// Validate ALL sections before writing ANY (atomic: all-or-nothing).
	for secName, doc := range sectionDocs {
		if errs := opsettings.Validate(secName, doc); len(errs) > 0 {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"error":   "validation_failed",
				"section": secName,
				"errors":  errs,
			})
			return
		}
	}

	// Write sections. If a CAS conflict occurs partway, we stop and report
	// which sections were applied vs conflicted. This is documented behavior:
	// the store guarantees per-section atomicity, and multi-section PUTs may
	// partially apply if a CAS conflict occurs after some sections succeed.
	applied := make(map[string]int64)
	var conflicted []map[string]interface{}

	for secName, doc := range sectionDocs {
		expectedRev := int64(-1) // last-writer-wins by default
		if rev, ok := req.ExpectedRevisions[secName]; ok {
			expectedRev = rev
		}

		newRev, err := ops.Update(r.Context(), secName, doc, updatedBy, expectedRev)
		if err != nil {
			if errors.Is(err, store.ErrRevisionConflict) {
				// Report the conflict with current revision.
				currentRev := s.getCurrentRevision(ops, secName)
				conflicted = append(conflicted, map[string]interface{}{
					"section":           secName,
					"expected_revision": expectedRev,
					"current_revision":  currentRev,
				})
				// Stop writing further sections on CAS conflict.
				break
			}
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				fmt.Sprintf("Failed to update section %q: %v", secName, err), nil)
			return
		}
		applied[secName] = newRev
	}

	if len(conflicted) > 0 {
		writeJSON(w, http.StatusConflict, map[string]interface{}{
			"error":      "revision_conflict",
			"message":    "One or more sections have been modified since the expected revision.",
			"applied":    applied,
			"conflicted": conflicted,
		})
		return
	}

	slog.Info("Server config updated via admin API (postgres mode)",
		"user", updatedBy,
		"sections", mapKeys(applied),
	)

	appliedKeys := mapKeys(applied)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "saved",
		"reload": map[string]interface{}{
			"applied":          appliedKeys,
			"requires_restart": []string{},
		},
	})
}

// getCurrentRevision reads the current revision for a section from the cache.
func (s *Server) getCurrentRevision(ops *OperationalSettings, section string) int64 {
	ops.mu.RLock()
	defer ops.mu.RUnlock()
	if ss, ok := ops.cache[section]; ok {
		return ss.Revision
	}
	return 0
}

// extractKoanfKeysFromRequest converts a ServerConfigUpdateRequest into a list
// of koanf keys representing the fields that are being updated. This enables
// Layer-0 vs Layer-1 classification via the opsettings registry.
func extractKoanfKeysFromRequest(req *ServerConfigUpdateRequest) []string {
	var keys []string

	// Top-level fields
	if req.SchemaVersion != nil {
		keys = append(keys, "schema_version")
	}
	if req.ActiveProfile != nil {
		keys = append(keys, "active_profile")
	}
	if req.DefaultTemplate != nil {
		keys = append(keys, "default_template")
	}
	if req.DefaultHarnessConfig != nil {
		keys = append(keys, "default_harness_config")
	}
	if req.ImageRegistry != nil {
		keys = append(keys, "image_registry")
	}
	if req.WorkspacePath != nil {
		keys = append(keys, "workspace_path")
	}
	if req.DefaultMaxTurns != nil {
		keys = append(keys, "default_max_turns")
	}
	if req.DefaultMaxModelCalls != nil {
		keys = append(keys, "default_max_model_calls")
	}
	if req.DefaultMaxDuration != nil {
		keys = append(keys, "default_max_duration")
	}
	if req.DefaultResources != nil {
		keys = append(keys, "default_resources")
	}

	if req.Telemetry != nil {
		keys = append(keys, "telemetry.enabled")
	}

	if req.Runtimes != nil {
		keys = append(keys, "runtimes")
	}
	if req.HarnessConfigs != nil {
		keys = append(keys, "harness_configs")
	}
	if req.Profiles != nil {
		keys = append(keys, "profiles")
	}

	// Server sub-fields
	if req.Server != nil {
		srv := req.Server
		if srv.Mode != "" {
			keys = append(keys, "server.mode")
		}
		if srv.LogLevel != "" {
			keys = append(keys, "server.log_level")
		}
		if srv.LogFormat != "" {
			keys = append(keys, "server.log_format")
		}
		if srv.Hub != nil {
			hub := srv.Hub
			if hub.Port != 0 {
				keys = append(keys, "server.hub.port")
			}
			if hub.Host != "" {
				keys = append(keys, "server.hub.host")
			}
			if hub.PublicURL != "" {
				keys = append(keys, "server.hub.public_url")
			}
			if len(hub.AdminEmails) > 0 {
				keys = append(keys, "server.hub.admin_emails")
			}
			if hub.AutoSuspendStalled != nil {
				keys = append(keys, "server.hub.auto_suspend_stalled")
			}
			if hub.SoftDeleteRetention != "" {
				keys = append(keys, "server.hub.soft_delete_retention")
			}
			if hub.SoftDeleteRetainFiles != nil {
				keys = append(keys, "server.hub.soft_delete_retain_files")
			}
			if hub.ReadTimeout != "" {
				keys = append(keys, "server.hub.read_timeout")
			}
			if hub.WriteTimeout != "" {
				keys = append(keys, "server.hub.write_timeout")
			}
			if hub.HubID != "" {
				keys = append(keys, "server.hub.hub_id")
			}
			if hub.CORS != nil {
				keys = append(keys, "server.hub.cors")
			}
		}
		if srv.Auth != nil {
			auth := srv.Auth
			if auth.UserAccessMode != "" {
				keys = append(keys, "server.auth.user_access_mode")
			}
			if len(auth.AuthorizedDomains) > 0 {
				keys = append(keys, "server.auth.authorized_domains")
			}
			if auth.Mode != "" {
				keys = append(keys, "server.auth.mode")
			}
			if auth.DevMode {
				keys = append(keys, "server.auth.dev_mode")
			}
			if auth.DevToken != "" {
				keys = append(keys, "server.auth.dev_token")
			}
			if auth.Proxy != nil {
				keys = append(keys, "server.auth.proxy")
			}
			if auth.Transport != nil {
				keys = append(keys, "server.auth.transport")
			}
		}
		if srv.Database != nil {
			keys = append(keys, "server.database")
		}
		if srv.Broker != nil {
			keys = append(keys, "server.broker")
		}
		if srv.OAuth != nil {
			keys = append(keys, "server.oauth")
		}
		if srv.Storage != nil {
			keys = append(keys, "server.storage")
		}
		if srv.Secrets != nil {
			keys = append(keys, "server.secrets")
		}
		if srv.WorkspaceStorage != nil {
			keys = append(keys, "server.workspace_storage")
		}
		if srv.MessageBroker != nil {
			keys = append(keys, "server.message_broker")
		}
		if srv.Plugins != nil {
			keys = append(keys, "server.plugins")
		}
		if srv.GitHubApp != nil {
			keys = append(keys, "server.github_app")
		}
		if len(srv.NotificationChannels) > 0 {
			keys = append(keys, "server.notification_channels")
		}
	}

	return keys
}

// buildSectionDocsFromRequest constructs per-section JSON documents from the
// update request, grouped by the Layer-1 sections they belong to.
func buildSectionDocsFromRequest(req *ServerConfigUpdateRequest, layer1BySec map[string][]string) (map[string]json.RawMessage, error) {
	docs := make(map[string]json.RawMessage)

	for secName := range layer1BySec {
		doc, err := buildSingleSectionDoc(req, secName)
		if err != nil {
			return nil, fmt.Errorf("building doc for section %q: %w", secName, err)
		}
		if doc != nil {
			docs[secName] = doc
		}
	}

	return docs, nil
}

// buildSingleSectionDoc extracts the fields for a single section from the
// update request and marshals them into a section document.
func buildSingleSectionDoc(req *ServerConfigUpdateRequest, secName string) (json.RawMessage, error) {
	var doc interface{}

	switch secName {
	case "access":
		d := &opsettings.AccessSettings{}
		if req.Server != nil && req.Server.Hub != nil && len(req.Server.Hub.AdminEmails) > 0 {
			d.AdminEmails = req.Server.Hub.AdminEmails
		}
		if req.Server != nil && req.Server.Auth != nil {
			if req.Server.Auth.UserAccessMode != "" {
				d.UserAccessMode = req.Server.Auth.UserAccessMode
			}
			if len(req.Server.Auth.AuthorizedDomains) > 0 {
				d.AuthorizedDomains = req.Server.Auth.AuthorizedDomains
			}
		}
		doc = d

	case "lifecycle":
		d := &opsettings.LifecycleSettings{}
		if req.Server != nil && req.Server.Hub != nil {
			d.AutoSuspendStalled = req.Server.Hub.AutoSuspendStalled
			if req.Server.Hub.SoftDeleteRetention != "" {
				d.SoftDeleteRetention = req.Server.Hub.SoftDeleteRetention
			}
			d.SoftDeleteRetainFiles = req.Server.Hub.SoftDeleteRetainFiles
		}
		doc = d

	case "telemetry":
		if req.Telemetry != nil {
			doc = req.Telemetry
		} else {
			return nil, nil
		}

	case "agent_defaults":
		d := &opsettings.AgentDefaultsSettings{}
		if req.DefaultTemplate != nil {
			d.DefaultTemplate = *req.DefaultTemplate
		}
		if req.DefaultHarnessConfig != nil {
			d.DefaultHarnessConfig = *req.DefaultHarnessConfig
		}
		if req.DefaultMaxTurns != nil {
			d.DefaultMaxTurns = *req.DefaultMaxTurns
		}
		if req.DefaultMaxModelCalls != nil {
			d.DefaultMaxModelCalls = *req.DefaultMaxModelCalls
		}
		if req.DefaultMaxDuration != nil {
			d.DefaultMaxDuration = *req.DefaultMaxDuration
		}
		if req.DefaultResources != nil {
			d.DefaultResources = req.DefaultResources
		}
		doc = d

	case "endpoints":
		d := &opsettings.EndpointsSettings{}
		if req.Server != nil && req.Server.Hub != nil && req.Server.Hub.PublicURL != "" {
			d.PublicURL = req.Server.Hub.PublicURL
		}
		if req.ImageRegistry != nil {
			d.ImageRegistry = *req.ImageRegistry
		}
		doc = d

	case "github_app":
		d := &opsettings.GitHubAppSettings{}
		if req.Server != nil && req.Server.GitHubApp != nil {
			ga := req.Server.GitHubApp
			d.AppID = ga.AppID
			d.APIBaseURL = ga.APIBaseURL
			d.WebhooksEnabled = ga.WebhooksEnabled
			d.InstallationURL = ga.InstallationURL
			d.PrivateKeyPath = ga.PrivateKeyPath
		}
		doc = d

	case "notifications":
		d := &opsettings.NotificationsSettings{}
		if req.Server != nil && len(req.Server.NotificationChannels) > 0 {
			d.NotificationChannels = req.Server.NotificationChannels
		}
		doc = d

	default:
		return nil, nil
	}

	return json.Marshal(doc)
}

// mapKeys returns the keys of a map as a sorted slice.
func mapKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// handleGetMaintenanceDB handles GET /api/v1/admin/maintenance in postgres mode.
// Reads maintenance state from the operational settings snapshot.
func (s *Server) handleGetMaintenanceDB(w http.ResponseWriter, ops *OperationalSettings) {
	snap := ops.Snapshot()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled": snap.AdminMode,
		"message": maintenanceMessageOrDefault(snap.MaintenanceMessage),
	})
}

// handlePutMaintenanceDB handles PUT /api/v1/admin/maintenance in postgres mode.
// Writes the maintenance section via OperationalSettings.Update (durable +
// propagated), then applies locally via ApplyMaintenanceFromSnapshot.
func (s *Server) handlePutMaintenanceDB(w http.ResponseWriter, r *http.Request, ops *OperationalSettings) {
	var body struct {
		Enabled *bool  `json:"enabled"`
		Message string `json:"message"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
		return
	}

	caller := GetUserIdentityFromContext(r.Context())
	updatedBy := ""
	if caller != nil {
		updatedBy = caller.Email()
	}

	// Build the maintenance section doc. Start from the current snapshot values
	// to preserve fields not being updated (partial update semantics).
	snap := ops.Snapshot()
	ms := opsettings.MaintenanceSettings{
		AdminMode:          snap.AdminMode,
		MaintenanceMessage: snap.MaintenanceMessage,
	}
	if body.Enabled != nil {
		ms.AdminMode = *body.Enabled
	}
	if body.Message != "" {
		ms.MaintenanceMessage = body.Message
	}

	doc, err := json.Marshal(ms)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to marshal maintenance settings", nil)
		return
	}

	// last-writer-wins (-1) for maintenance — no CAS needed for this endpoint.
	if _, err := ops.Update(r.Context(), "maintenance", doc, updatedBy, -1); err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			fmt.Sprintf("Failed to update maintenance settings: %v", err), nil)
		return
	}

	// The Update call already self-applies via ApplySnapshot + ApplyMaintenanceFromSnapshot,
	// but read the final state from the server's MaintenanceState to reflect env overrides.
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled": s.maintenance.IsEnabled(),
		"message": s.maintenance.Message(),
	})
}

// maintenanceMessageOrDefault returns the message or the default if empty.
func maintenanceMessageOrDefault(msg string) string {
	if msg == "" {
		return defaultMaintenanceMessage
	}
	return msg
}
