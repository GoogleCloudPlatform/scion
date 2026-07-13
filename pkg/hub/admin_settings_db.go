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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
	"github.com/knadh/koanf/v2"
	yamlv3 "gopkg.in/yaml.v3"
)

const maxSettingsBodySize = 1 << 20 // 1 MB — matches integrations/harness-config convention

// SectionMetadata carries per-section provenance metadata in the GET response
// (design §3.8, additive shape). The key "section_metadata" is chosen to not
// collide with any existing ServerConfigResponse field.
type SectionMetadata struct {
	Source    string     `json:"source"`               // "db", "file", or "default"
	Revision  int64      `json:"revision,omitempty"`   // DB revision (0 for file/default)
	UpdatedAt *time.Time `json:"updated_at,omitempty"` // last update time (DB only)
	UpdatedBy string     `json:"updated_by,omitempty"` // admin email (DB only)
	Origin    string     `json:"origin,omitempty"`     // "seeded" or "managed" (DB mode only)
}

// ServerConfigDBResponse extends the file-mode response with metadata for the
// postgres-mode GET endpoint. It embeds the original ServerConfigResponse and
// adds section_metadata and env_overrides.
type ServerConfigDBResponse struct {
	ServerConfigResponse

	// SectionMetadata maps section name to its provenance metadata.
	SectionMeta map[string]SectionMetadata `json:"section_metadata,omitempty"`

	// SupersededKeys maps section name to bootstrap-material keys whose
	// merged value differs from the DB value (managed sections only).
	SupersededKeys map[string][]SupersededKey `json:"superseded_keys,omitempty"`

	// DeprecatedEnvKeys lists SCION_SERVER_* env vars that target Layer-1
	// keys and should be migrated to SCION_SEED_* equivalents.
	DeprecatedEnvKeys []DeprecatedEnvKeyInfo `json:"deprecated_env_keys,omitempty"`
}

// SupersededKey represents a bootstrap-material key whose value in the
// bootstrap merge differs from the admin-set DB value in a managed section.
type SupersededKey struct {
	Key    string `json:"key"`
	Source string `json:"source"` // "seed_env", "yaml", or "server_env"
}

// DeprecatedEnvKeyInfo represents a SCION_SERVER_* env var that targets a
// Layer-1 key and should be migrated to the SCION_SEED_* equivalent.
type DeprecatedEnvKeyInfo struct {
	EnvVar         string `json:"env_var"`
	KoanfKey       string `json:"koanf_key"`
	SeedEquivalent string `json:"seed_equivalent"`
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
func (s *Server) handleGetServerConfigDB(w http.ResponseWriter, r *http.Request, ops *OperationalSettings) {
	// Build the base response from the file (same as file mode) for Layer-0.
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		// N3: log the full error server-side for observability.
		slog.Error("GET server-config: failed to resolve settings directory", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to resolve settings directory", nil)
		return
	}

	settingsPath := filepath.Join(globalDir, "settings.yaml")
	data, err := os.ReadFile(settingsPath)
	if err != nil && !os.IsNotExist(err) {
		// N3: log the full error server-side for observability.
		slog.Error("GET server-config: failed to read settings file", "path", settingsPath, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to read settings file", nil)
		return
	}

	var vs config.VersionedSettings
	if data != nil {
		if err := yamlv3.Unmarshal(data, &vs); err != nil {
			// N3: log the full error server-side for observability.
			slog.Error("GET server-config: failed to parse settings file", "path", settingsPath, "error", err)
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

	if resp.SchemaVersion == "" {
		resp.SchemaVersion = "1"
	}

	resp.SettingsTier = "db"

	// Overlay Layer-1 fields from the operational settings snapshot.
	snap := ops.Snapshot()
	applySnapshotToResponse(&resp.ServerConfigResponse, snap)

	// Build section metadata from the cache.
	resp.SectionMeta = s.buildSectionMetadata(r.Context(), ops)

	// Env overrides.
	overrides := ops.EnvOverriddenKeys()
	sort.Strings(overrides)
	resp.EnvOverrides = overrides

	// Superseded keys (managed sections whose DB value diverges from bootstrap).
	resp.SupersededKeys = s.computeSupersededKeys(ops)

	// Deprecated env keys (SCION_SERVER_* targeting Layer-1 keys).
	resp.DeprecatedEnvKeys = s.computeDeprecatedEnvKeys(ops)

	// Mask sensitive fields — same logic as file mode.
	maskSensitiveFields(&resp.ServerConfigResponse)

	writeJSON(w, http.StatusOK, resp)
}

// applySnapshotToResponse writes Layer-1 snapshot values into the
// ServerConfigResponse, ensuring the response reflects the merged
// (DB > bootstrap merge) view exactly. The snapshot is the
// authoritative merged result (DB > bootstrap merge); every
// field MUST be written unconditionally so that false booleans, empty
// slices, and zero-value strings from the snapshot override any
// stale file-loaded values in the response.
func applySnapshotToResponse(resp *ServerConfigResponse, snap Layer1Snapshot) {
	// Agent defaults
	resp.DefaultTemplate = snap.DefaultTemplate
	resp.DefaultHarnessConfig = snap.DefaultHarnessConfig
	resp.ImageRegistry = snap.ImageRegistry
	resp.DefaultMaxTurns = snap.DefaultMaxTurns
	resp.DefaultMaxModelCalls = snap.DefaultMaxModelCalls
	resp.DefaultMaxDuration = snap.DefaultMaxDuration
	resp.DefaultResources = snap.DefaultResources

	// Telemetry — always set from snapshot (nil = no telemetry configured).
	resp.Telemetry = snap.TelemetryConfig

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

	// Lifecycle — always set booleans from the snapshot, regardless of
	// true/false, so that a DB-explicit false overrides a file-loaded true.
	b := snap.AutoSuspendStalled
	resp.Server.Hub.AutoSuspendStalled = &b
	resp.Server.Hub.SoftDeleteRetention = snap.SoftDeleteRetention
	b2 := snap.SoftDeleteRetainFiles
	resp.Server.Hub.SoftDeleteRetainFiles = &b2

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

	// Notifications — always set from snapshot so an explicit empty DB
	// value overrides file-loaded channels.
	resp.Server.NotificationChannels = snap.NotificationChannels
}

// buildSectionMetadata reads the OperationalSettings cache to determine
// per-section provenance: "db" (present in cache), "file" (section absent from
// cache but present in settings.yaml fallback), or "default" (neither).
//
// N4: metadata is served entirely from the enriched cache (sectionState carries
// UpdatedAt/UpdatedBy since Refresh), eliminating the extra per-GET DB query
// and the metadata/value consistency window.
func (s *Server) buildSectionMetadata(_ context.Context, ops *OperationalSettings) map[string]SectionMetadata {
	meta := make(map[string]SectionMetadata, len(opsettings.Registry))

	// Read cache snapshot under lock.
	ops.mu.RLock()
	cacheSnap := make(map[string]sectionState, len(ops.cache))
	for name, ss := range ops.cache {
		cacheSnap[name] = ss
	}
	ops.mu.RUnlock()

	for _, sec := range opsettings.Registry {
		if ss, ok := cacheSnap[sec.Name]; ok {
			t := ss.UpdatedAt
			meta[sec.Name] = SectionMetadata{
				Source:    "db",
				Revision:  ss.Revision,
				UpdatedAt: &t,
				UpdatedBy: ss.UpdatedBy,
				Origin:    ss.Origin,
			}
		} else if s.sectionHasBootstrapValues(ops, sec.Name) {
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

// sectionHasBootstrapValues checks whether the bootstrap koanf has any non-zero
// values for the given section's koanf paths.
func (s *Server) sectionHasBootstrapValues(ops *OperationalSettings, sectionName string) bool {
	sec := opsettings.SectionByName(sectionName)
	if sec == nil || len(sec.KoanfPaths) == 0 {
		return false
	}
	for _, kp := range sec.KoanfPaths {
		if ops.bootstrapKoanf != nil && ops.bootstrapKoanf.Exists(kp) {
			return true
		}
	}
	return false
}

// computeSupersededKeys returns, for each managed section, the bootstrap-material
// keys whose merged value differs from the DB value. This tells the admin which
// deployment-config values their explicit DB writes are overriding.
func (s *Server) computeSupersededKeys(ops *OperationalSettings) map[string][]SupersededKey {
	ops.mu.RLock()
	cacheSnap := make(map[string]sectionState, len(ops.cache))
	for name, ss := range ops.cache {
		cacheSnap[name] = ss
	}
	ops.mu.RUnlock()

	if ops.bootstrapKoanf == nil {
		return nil
	}

	// Load individual layers for source attribution.
	seedEnvK := config.LoadSeedEnvKoanf()
	serverEnvK := config.LoadEnvKoanf()

	result := make(map[string][]SupersededKey)
	for _, sec := range opsettings.Registry {
		ss, ok := cacheSnap[sec.Name]
		if !ok || ss.Origin != "managed" {
			continue
		}

		bootstrapDoc, err := opsettings.ExtractSectionFromKoanf(ops.bootstrapKoanf, sec.Name)
		if err != nil || bootstrapDoc == nil {
			continue
		}

		var dbMap, bootstrapMap map[string]interface{}
		if err := json.Unmarshal(ss.Value, &dbMap); err != nil {
			continue
		}
		if err := json.Unmarshal(bootstrapDoc, &bootstrapMap); err != nil {
			continue
		}

		var superseded []SupersededKey
		for key, bootstrapVal := range bootstrapMap {
			dbVal, exists := dbMap[key]
			if !exists || !reflect.DeepEqual(dbVal, bootstrapVal) {
				koanfPath := opsettings.KoanfPathFromSectionKey(sec.Name, key)
				if koanfPath == "" {
					koanfPath = key
				}
				superseded = append(superseded, SupersededKey{
					Key:    koanfPath,
					Source: detectKeySource(seedEnvK, serverEnvK, sec.Name, key),
				})
			}
		}
		if len(superseded) > 0 {
			sort.Slice(superseded, func(i, j int) bool { return superseded[i].Key < superseded[j].Key })
			result[sec.Name] = superseded
		}
	}

	if len(result) == 0 {
		return nil
	}
	return result
}

// detectKeySource determines which bootstrap layer provides a given section key.
// It checks the individual layers in reverse precedence order (server_env first,
// then seed_env, then yaml) and returns the first match.
func detectKeySource(seedEnvK, serverEnvK *koanf.Koanf, sectionName, key string) string {
	sec := opsettings.SectionByName(sectionName)
	if sec == nil {
		return "yaml"
	}

	// Map the section-level key (e.g. "admin_emails") to koanf paths and check
	// which layer provides the value.
	for _, kp := range sec.KoanfPaths {
		if opsettings.SectionKeyFromKoanfPath(kp) != key {
			continue
		}
		if serverEnvK != nil && serverEnvK.Exists(kp) {
			return "server_env"
		}
		if seedEnvK != nil && seedEnvK.Exists(kp) {
			return "seed_env"
		}
		return "yaml"
	}

	return "yaml"
}

// computeDeprecatedEnvKeys returns SCION_SERVER_* env vars that target Layer-1
// keys and should be migrated to SCION_SEED_* equivalents.
func (s *Server) computeDeprecatedEnvKeys(ops *OperationalSettings) []DeprecatedEnvKeyInfo {
	if ops.envKoanf == nil {
		return nil
	}
	deprecated := opsettings.DetectDeprecatedServerEnv(ops.envKoanf)
	if len(deprecated) == 0 {
		return nil
	}
	result := make([]DeprecatedEnvKeyInfo, len(deprecated))
	for i, d := range deprecated {
		result[i] = DeprecatedEnvKeyInfo{
			EnvVar:         d.EnvVar,
			KoanfKey:       d.KoanfKey,
			SeedEquivalent: d.SeedEquivalent,
		}
	}
	return result
}

// handlePutServerConfigDB handles PUT /api/v1/admin/server-config in postgres mode.
//
// It partitions incoming fields via the opsettings registry:
//   - Layer-1 fields → per-section docs → validate → OperationalSettings.Update
//   - Layer-0 fields → 422 rejection with offending key list
//
// Supports optional CAS via expected_revisions in the request body.
func (s *Server) handlePutServerConfigDB(w http.ResponseWriter, r *http.Request, ops *OperationalSettings) {
	// N6/N7: Read raw body first for presence-aware field clearing,
	// then decode into the typed struct. This lets us distinguish
	// OMITTED fields (keep current value) from EXPLICITLY-SENT empty
	// values ("", [], null) which CLEAR the field in the section doc.
	// File-mode behavior is untouched — this is postgres-path only.
	rawBody, err := readRawBody(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
		return
	}
	var req ServerConfigUpdateDBRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
		return
	}

	caller := GetUserIdentityFromContext(r.Context())
	updatedBy := ""
	if caller != nil {
		updatedBy = caller.Email()
	}

	// Convert the update request into koanf keys to classify Layer-0 vs Layer-1
	// vs unclassified. Three-way classification (design §3.1):
	//   - Layer-1 → write to DB sections
	//   - Layer-0 (explicit bootstrap set) → 422 rejection
	//   - Unclassified (e.g. runtimes, profiles, schema_version) → ignored with warning
	//
	// N6: Also extract keys for explicitly-sent empty values (for presence-aware
	// clearing) by walking the raw request body.
	koanfKeys := extractKoanfKeysFromRequest(&req.ServerConfigUpdateRequest)
	koanfKeys = appendPresenceAwareKeys(koanfKeys, rawBody)

	// Classify keys.
	layer1BySec, layer0Keys, unclassifiedKeys := opsettings.ClassifyKeys(koanfKeys)

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

	// Warn about unclassified keys — accepted for shape compatibility with
	// file mode but not written to DB. Clients can see what was skipped via
	// the ignored_keys field in the response.
	if len(unclassifiedKeys) > 0 {
		sort.Strings(unclassifiedKeys)
		slog.Warn("PUT server-config: ignoring unclassified keys (not Layer-0, not Layer-1)",
			"keys", unclassifiedKeys,
			"user", updatedBy,
		)
	}

	// Build per-section documents from the request.
	sectionDocs, err := buildSectionDocsFromRequest(&req.ServerConfigUpdateRequest, layer1BySec, rawBody)
	if err != nil {
		// N3: log the full error server-side for observability.
		slog.Error("PUT server-config: failed to build section documents", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to build section documents", nil)
		return
	}

	// Validate ALL sections before writing ANY (atomic: all-or-nothing).
	// Collect errors from every section so the client sees all invalid
	// sections in one response, not just the first one (N6).
	allValidationErrors := make(map[string][]config.ValidationError)
	for secName, doc := range sectionDocs {
		if errs := opsettings.Validate(secName, doc); len(errs) > 0 {
			allValidationErrors[secName] = errs
		}
	}
	if len(allValidationErrors) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":  "validation_failed",
			"errors": allValidationErrors,
		})
		return
	}

	// Write sections in sorted order for deterministic partial-apply and CAS
	// behavior: if a conflict occurs partway, exactly the alphabetically-first
	// sections are applied, giving clients predictable retry semantics.
	applied := make(map[string]int64)
	var conflicted []map[string]interface{}

	sortedSections := make([]string, 0, len(sectionDocs))
	for secName := range sectionDocs {
		sortedSections = append(sortedSections, secName)
	}
	sort.Strings(sortedSections)

	for _, secName := range sortedSections {
		doc := sectionDocs[secName]
		expectedRev := int64(-1) // last-writer-wins by default
		if rev, ok := req.ExpectedRevisions[secName]; ok {
			expectedRev = rev
		}

		newRev, err := ops.Update(r.Context(), secName, doc, updatedBy, expectedRev, "managed")
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
			slog.Error("Failed to update section", "section", secName, "error", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				fmt.Sprintf("Failed to update section %q", secName), nil)
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

	resp := map[string]interface{}{
		"status": "saved",
		"reload": map[string]interface{}{
			"applied":          appliedKeys,
			"requires_restart": []string{},
		},
	}

	// Report ignored unclassified keys so clients/UI can see what was skipped.
	if len(unclassifiedKeys) > 0 {
		resp["ignored_keys"] = unclassifiedKeys
	}

	writeJSON(w, http.StatusOK, resp)
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

// isZeroStruct reports whether a non-nil struct pointer is entirely zero-valued.
// Used by B1 fix: the web UI's buildPayload() always sends certain Layer-0
// objects (database, broker, storage, secrets, message_broker) as empty JSON
// objects ({}). Go unmarshals {} into a non-nil pointer with all zero fields.
// We treat these "UI artifacts" as not-present rather than rejecting them as
// Layer-0 writes, while a struct with ANY meaningful (non-zero) value still
// triggers the Layer-0 422 rejection as designed.
func isZeroStruct(v interface{}) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer && rv.IsNil() {
		return true
	}
	return reflect.DeepEqual(v, reflect.New(reflect.TypeOf(v).Elem()).Interface())
}

// extractKoanfKeysFromRequest converts a ServerConfigUpdateRequest into a list
// of koanf keys representing the fields that are being updated. This enables
// Layer-0 vs Layer-1 classification via the opsettings registry.
//
// B1 fix: nested Layer-0 struct pointers that are non-nil but entirely
// zero-valued emit nothing (treated as not-present). The web UI's buildPayload()
// always sends database, broker, storage, secrets, and message_broker as empty
// objects — these are UI artifacts, not intentional Layer-0 writes. A Layer-0
// object with ANY meaningful (non-zero) value still triggers 422 rejection.
//
// N2 fix: server.env is now extracted (Layer-0). auth.DevMode bool cannot
// distinguish an explicit false from omission due to Go's zero-value semantics —
// documented as a known limitation consistent with B1's zero-struct logic.
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
		// N2: extract server.env for Layer-0 422 per design §3.1.
		if srv.Env != "" {
			keys = append(keys, "server.env")
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
			// N2: auth.DevMode is a bool (not *bool), so Go's zero value (false)
			// is indistinguishable from an explicit false in the JSON payload.
			// This means a PUT with "dev_mode": false won't emit the key and
			// won't trigger Layer-0 rejection. Practical impact is nil (setting
			// dev_mode to false is a no-op). This is consistent with B1's
			// zero-struct logic: zero-valued fields are treated as not-present.
			if auth.DevMode {
				keys = append(keys, "server.auth.dev_mode")
			}
			if auth.DevToken != "" {
				keys = append(keys, "server.auth.dev_token")
			}
			if auth.DevTokenFile != "" {
				keys = append(keys, "server.auth.dev_token_file")
			}
			if auth.Proxy != nil {
				keys = append(keys, "server.auth.proxy")
			}
			if auth.Transport != nil {
				keys = append(keys, "server.auth.transport")
			}
		}
		// B1 fix: Layer-0 struct pointers that are non-nil but entirely
		// zero-valued are treated as UI artifacts (not-present). The web UI's
		// buildPayload() always sends these as empty objects. Only emit the
		// key when the struct has at least one meaningful (non-zero) field.
		//
		// Always-sent UI keys verified from web/src/components/pages/
		// admin-server-config.ts buildPayload() lines 884-924:
		//   - database (line 889: server.database = database)
		//   - broker   (line 882: server.broker = broker)
		//   - storage  (line 912: server.storage = storage)
		//   - secrets  (line 918: server.secrets = secrets)
		//   - message_broker (line 921-924: server.message_broker = {...})
		if srv.Database != nil && !isZeroStruct(srv.Database) {
			keys = append(keys, "server.database")
		}
		if srv.Broker != nil && !isZeroStruct(srv.Broker) {
			keys = append(keys, "server.broker")
		}
		if srv.OAuth != nil {
			keys = append(keys, "server.oauth")
		}
		if srv.Storage != nil && !isZeroStruct(srv.Storage) {
			keys = append(keys, "server.storage")
		}
		if srv.Secrets != nil && !isZeroStruct(srv.Secrets) {
			keys = append(keys, "server.secrets")
		}
		if srv.WorkspaceStorage != nil && !isZeroStruct(srv.WorkspaceStorage) {
			keys = append(keys, "server.workspace_storage")
		}
		if srv.MessageBroker != nil && !isZeroStruct(srv.MessageBroker) {
			keys = append(keys, "server.message_broker")
		}
		if srv.Plugins != nil && !isZeroStruct(srv.Plugins) {
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

// appendPresenceAwareKeys adds koanf keys for Layer-1 fields that are
// explicitly present in the raw JSON but zero-valued in the Go struct.
// This enables presence-aware clearing (N6): an explicit empty value
// ("", [], null) in the PUT body should clear the field, but
// extractKoanfKeysFromRequest can't detect these because Go's zero values
// make them invisible.
//
// Only the clearable Layer-1 fields are checked here:
// admin_emails, user_access_mode, notification_channels, public_url.
func appendPresenceAwareKeys(keys []string, rawBody []byte) []string {
	fp, err := parseFieldPresence(rawBody)
	if err != nil {
		// Presence detection falls back to omitted-semantics on malformed body;
		// the typed decode will 400 anyway.
		slog.Warn("parseFieldPresence failed, falling back to omitted-semantics", "error", err)
		return keys
	}

	keySet := make(map[string]bool, len(keys))
	for _, k := range keys {
		keySet[k] = true
	}

	serverFP := fp.nestedPresence("server")
	hubFP := serverFP.nestedPresence("hub")
	authFP := serverFP.nestedPresence("auth")

	// admin_emails: present in hub but empty → add the key.
	if !keySet["server.hub.admin_emails"] && hubFP.has("admin_emails") {
		keys = append(keys, "server.hub.admin_emails")
	}
	// user_access_mode: present in auth but empty → add the key.
	if !keySet["server.auth.user_access_mode"] && authFP.has("user_access_mode") {
		keys = append(keys, "server.auth.user_access_mode")
	}
	// authorized_domains: present in auth but empty → add the key.
	if !keySet["server.auth.authorized_domains"] && authFP.has("authorized_domains") {
		keys = append(keys, "server.auth.authorized_domains")
	}
	// notification_channels: present in server but empty → add the key.
	if !keySet["server.notification_channels"] && serverFP.has("notification_channels") {
		keys = append(keys, "server.notification_channels")
	}
	// public_url: present in hub but empty → add the key.
	if !keySet["server.hub.public_url"] && hubFP.has("public_url") {
		keys = append(keys, "server.hub.public_url")
	}

	return keys
}

// buildSectionDocsFromRequest constructs per-section JSON documents from the
// update request, grouped by the Layer-1 sections they belong to.
// rawBody is used for presence-aware field clearing (N6/N7).
func buildSectionDocsFromRequest(req *ServerConfigUpdateRequest, layer1BySec map[string][]string, rawBody []byte) (map[string]json.RawMessage, error) {
	// Parse top-level presence for N6/N7. On error, fall back to nil
	// (omitted-semantics for all fields); the typed decode will 400 anyway.
	fp, err := parseFieldPresence(rawBody)
	if err != nil {
		slog.Warn("parseFieldPresence failed in buildSectionDocs, falling back to omitted-semantics", "error", err)
	}

	docs := make(map[string]json.RawMessage)

	for secName := range layer1BySec {
		doc, err := buildSingleSectionDoc(req, secName, fp)
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
//
// N6/N7 presence-aware clearing (postgres-path only):
//
// The fp (fieldPresence) parameter carries the raw JSON structure so we can
// distinguish OMITTED fields from EXPLICITLY-SENT empty values:
//   - OMITTED → field not in raw JSON → do NOT include in section doc
//     (the current DB value is preserved on Refresh)
//   - EXPLICIT empty ("", [], null) → field IS in raw JSON → include the
//     zero value in the section doc, which CLEARS it in the DB
//
// This applies to: admin_emails, user_access_mode, notification_channels,
// public_url. File-mode behavior is untouched.
func buildSingleSectionDoc(req *ServerConfigUpdateRequest, secName string, fp *fieldPresence) (json.RawMessage, error) {
	var doc interface{}

	// N6/N7: Derive nested presence maps for the server, hub, and auth sub-objects.
	serverFP := fp.nestedPresence("server")
	hubFP := serverFP.nestedPresence("hub")
	authFP := serverFP.nestedPresence("auth")

	switch secName {
	case "access":
		d := &opsettings.AccessSettings{}
		if req.Server != nil && req.Server.Hub != nil {
			// N6: presence-aware — explicit empty [] clears admin_emails.
			if len(req.Server.Hub.AdminEmails) > 0 {
				d.AdminEmails = req.Server.Hub.AdminEmails
			} else if hubFP.has("admin_emails") {
				// Explicitly sent as [] or null → clear to empty slice.
				d.AdminEmails = []string{}
			}
		}
		if req.Server != nil && req.Server.Auth != nil {
			// N6: presence-aware — explicit empty "" clears user_access_mode.
			if req.Server.Auth.UserAccessMode != "" {
				d.UserAccessMode = req.Server.Auth.UserAccessMode
			} else if authFP.has("user_access_mode") {
				d.UserAccessMode = "" // explicitly cleared
			}
			if len(req.Server.Auth.AuthorizedDomains) > 0 {
				d.AuthorizedDomains = req.Server.Auth.AuthorizedDomains
			} else if authFP.has("authorized_domains") {
				d.AuthorizedDomains = []string{}
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
		if req.Server != nil && req.Server.Hub != nil {
			// N6: presence-aware — explicit empty "" clears public_url.
			if req.Server.Hub.PublicURL != "" {
				d.PublicURL = req.Server.Hub.PublicURL
			} else if hubFP.has("public_url") {
				d.PublicURL = "" // explicitly cleared
			}
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
			// #391: webhooks_enabled is a plain bool in the request; use
			// fieldPresence to distinguish explicit false from omitted.
			githubFP := serverFP.nestedPresence("github_app")
			if ga.WebhooksEnabled || githubFP.has("webhooks_enabled") {
				d.WebhooksEnabled = &ga.WebhooksEnabled
			}
			d.InstallationURL = ga.InstallationURL
			d.PrivateKeyPath = ga.PrivateKeyPath
		}
		doc = d

	case "notifications":
		d := &opsettings.NotificationsSettings{}
		if req.Server != nil {
			// N6: presence-aware — explicit empty [] or null clears channels.
			if len(req.Server.NotificationChannels) > 0 {
				d.NotificationChannels = req.Server.NotificationChannels
			} else if serverFP.has("notification_channels") {
				// Explicitly sent as [] or null → clear to empty slice.
				d.NotificationChannels = []config.V1NotificationChannelConfig{}
			}
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
	// N7: Read raw body for presence-aware message clearing.
	rawBody, err := readRawBody(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
		return
	}
	var body struct {
		Enabled *bool  `json:"enabled"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rawBody, &body); err != nil {
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

	// N7: presence-aware message clearing. An explicit empty message ("")
	// clears the maintenance message; an omitted message field preserves it.
	fp, fpErr := parseFieldPresence(rawBody)
	if fpErr != nil {
		slog.Warn("parseFieldPresence failed in maintenance handler, falling back to omitted-semantics", "error", fpErr)
	}
	if body.Message != "" {
		ms.MaintenanceMessage = body.Message
	} else if fp.has("message") {
		// Explicitly sent as "" → clear the message.
		ms.MaintenanceMessage = ""
	}

	doc, err := json.Marshal(ms)
	if err != nil {
		// N3: log the full error server-side for observability.
		slog.Error("PUT maintenance: failed to marshal maintenance settings", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to marshal maintenance settings", nil)
		return
	}

	// last-writer-wins (-1) for maintenance — no CAS needed for this endpoint.
	if _, err := ops.Update(r.Context(), "maintenance", doc, updatedBy, -1, "managed"); err != nil {
		slog.Error("Failed to update maintenance settings", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to update maintenance settings", nil)
		return
	}

	// The Update call already self-applies via ApplySnapshot + ApplyMaintenanceFromSnapshot,
	// but read the final state from the server's MaintenanceState to reflect env overrides.
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled": s.maintenance.IsEnabled(),
		"message": s.maintenance.Message(),
	})
}

// handleAdminServerConfigSchema handles GET /api/v1/admin/server-config/schema.
// Returns JSON-schema fragments per section from the opsettings registry,
// intended for UI form generation and CLI validation. Static metadata — no DB access.
func (s *Server) handleAdminServerConfigSchema(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	info := opsettings.SchemaInfo()
	if info == nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Schema information unavailable", nil)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sections": info,
	})
}

// readRawBody reads and returns the full request body as raw bytes, enforcing
// maxSettingsBodySize via http.MaxBytesReader.
func readRawBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, fmt.Errorf("empty request body")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSettingsBodySize)
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// fieldPresence extracts which JSON fields are explicitly present (including
// when set to "", [], null) in the raw request body by walking nested
// map[string]json.RawMessage paths. This powers N6/N7: presence-aware
// clearing in the postgres PUT path.
//
// Semantics:
//   - OMITTED field → not in the returned set → keep current DB value
//   - EXPLICITLY-SENT empty ("", [], null) → in the returned set → CLEAR the field
//   - EXPLICITLY-SENT non-empty → in the returned set → normal update
//
// File-mode behavior is untouched — this is postgres-path only.
type fieldPresence struct {
	raw map[string]json.RawMessage
}

// parseFieldPresence parses the top-level raw JSON into a fieldPresence.
func parseFieldPresence(rawBody []byte) (*fieldPresence, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &raw); err != nil {
		return nil, err
	}
	return &fieldPresence{raw: raw}, nil
}

// nestedPresence returns a fieldPresence for a nested object key.
func (fp *fieldPresence) nestedPresence(key string) *fieldPresence {
	if fp == nil {
		return nil
	}
	val, ok := fp.raw[key]
	if !ok {
		return nil
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(val, &nested); err != nil {
		return nil
	}
	return &fieldPresence{raw: nested}
}

// has reports whether the given field key is explicitly present in the JSON.
func (fp *fieldPresence) has(key string) bool {
	if fp == nil {
		return false
	}
	_, ok := fp.raw[key]
	return ok
}

// maintenanceMessageOrDefault returns the message or the default if empty.
func maintenanceMessageOrDefault(msg string) string {
	if msg == "" {
		return defaultMaintenanceMessage
	}
	return msg
}
