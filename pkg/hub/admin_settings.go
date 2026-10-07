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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
	yamlv3 "gopkg.in/yaml.v3"
)

// ServerConfigResponse is the API representation of the server settings file.
// It mirrors the on-disk settings.yaml structure, omitting sensitive fields.
type ServerConfigResponse struct {
	// Read-only server build info (not persisted in settings.yaml).
	ScionVersion   string `json:"scion_version,omitempty"`
	ScionCommit    string `json:"scion_commit,omitempty"`
	ScionBuildTime string `json:"scion_build_time,omitempty"`

	// SettingsTier indicates the settings backend: "db" or "file".
	SettingsTier string `json:"settings_tier,omitempty"`

	SchemaVersion        string                               `json:"schema_version"`
	ActiveProfile        string                               `json:"active_profile,omitempty"`
	DefaultTemplate      string                               `json:"default_template,omitempty"`
	DefaultHarnessConfig string                               `json:"default_harness_config,omitempty"`
	ImageRegistry        string                               `json:"image_registry,omitempty"`
	WorkspacePath        string                               `json:"workspace_path,omitempty"`
	Server               *config.V1ServerConfig               `json:"server,omitempty"`
	Telemetry            *config.V1TelemetryConfig            `json:"telemetry,omitempty"`
	Runtimes             map[string]config.V1RuntimeConfig    `json:"runtimes,omitempty"`
	HarnessConfigs       map[string]config.HarnessConfigEntry `json:"harness_configs,omitempty"`
	Profiles             map[string]config.V1ProfileConfig    `json:"profiles,omitempty"`

	// Default agent limits
	DefaultMaxTurns      int               `json:"default_max_turns,omitempty"`
	DefaultMaxModelCalls int               `json:"default_max_model_calls,omitempty"`
	DefaultMaxDuration   string            `json:"default_max_duration,omitempty"`
	DefaultResources     *api.ResourceSpec `json:"default_resources,omitempty"`

	// Default agent model settings
	DefaultModel         string `json:"default_model,omitempty"`
	DefaultThinkingLevel *int   `json:"default_thinking_level,omitempty"`

	// Default agent authorization
	DefaultMaxAgentRole string `json:"default_max_agent_role,omitempty"`
	DefaultAgentRole    string `json:"default_agent_role,omitempty"`

	// Default runtime broker (hub-level)
	DefaultRuntimeBroker string `json:"default_runtime_broker,omitempty"`

	// DefaultTimezone is the hub-level IANA timezone fallback.
	DefaultTimezone string `json:"default_timezone,omitempty"`

	// DefaultGCPIdentityMode is the hub-wide fallback GCP metadata mode
	// ("block", "passthrough", or "assign"), applied when neither the agent
	// create request nor the project's default GCP identity setting names one.
	DefaultGCPIdentityMode string `json:"default_gcp_identity_mode,omitempty"`
	// DefaultGCPIdentityServiceAccountID is the service account used when
	// DefaultGCPIdentityMode is "assign".
	DefaultGCPIdentityServiceAccountID string `json:"default_gcp_identity_service_account_id,omitempty"`

	// AutoInjectGcloudADC controls whether gcloud ADC is injected into agent containers.
	AutoInjectGcloudADC bool `json:"auto_inject_gcloud_adc,omitempty"`

	// AutoExposePorts controls whether ports are automatically exposed in agent containers.
	AutoExposePorts *config.AutoExposePortsSettings `json:"auto_expose_ports,omitempty"`

	// Quotas controls hub-level quota enforcement toggles.
	Quotas *config.QuotaSettings `json:"quotas,omitempty"`

	// AgentSecrets controls hub-level policy for secrets written by agents.
	AgentSecrets *config.AgentSecretsSettings `json:"agent_secrets,omitempty"`

	// Federation holds the federation authentication config for the admin API.
	Federation *config.V1FederationConfig `json:"federation,omitempty"`

	// EnvOverrides lists koanf keys overridden by SCION_SERVER_* env vars
	// on this node. Present in both file-mode and DB-mode responses so the
	// admin UI can show env-pinned fields regardless of settings tier.
	EnvOverrides []string `json:"env_overrides,omitempty"`
}

// ServerConfigUpdateRequest is the payload for updating settings.
type ServerConfigUpdateRequest struct {
	SchemaVersion        *string                              `json:"schema_version,omitempty"`
	ActiveProfile        *string                              `json:"active_profile,omitempty"`
	DefaultTemplate      *string                              `json:"default_template,omitempty"`
	DefaultHarnessConfig *string                              `json:"default_harness_config,omitempty"`
	ImageRegistry        *string                              `json:"image_registry,omitempty"`
	WorkspacePath        *string                              `json:"workspace_path,omitempty"`
	Server               *config.V1ServerConfig               `json:"server,omitempty"`
	Telemetry            *config.V1TelemetryConfig            `json:"telemetry,omitempty"`
	Runtimes             map[string]config.V1RuntimeConfig    `json:"runtimes,omitempty"`
	HarnessConfigs       map[string]config.HarnessConfigEntry `json:"harness_configs,omitempty"`
	Profiles             map[string]config.V1ProfileConfig    `json:"profiles,omitempty"`

	// Default agent limits
	DefaultMaxTurns      *int              `json:"default_max_turns,omitempty"`
	DefaultMaxModelCalls *int              `json:"default_max_model_calls,omitempty"`
	DefaultMaxDuration   *string           `json:"default_max_duration,omitempty"`
	DefaultResources     *api.ResourceSpec `json:"default_resources,omitempty"`

	// Default agent model settings
	DefaultModel         *string `json:"default_model,omitempty"`
	DefaultThinkingLevel *int    `json:"default_thinking_level,omitempty"`

	// Default agent authorization
	DefaultMaxAgentRole *string `json:"default_max_agent_role,omitempty"`
	DefaultAgentRole    *string `json:"default_agent_role,omitempty"`

	// Default runtime broker (hub-level)
	DefaultRuntimeBroker *string `json:"default_runtime_broker,omitempty"`

	// DefaultTimezone is the hub-level IANA timezone fallback.
	DefaultTimezone *string `json:"default_timezone,omitempty"`

	// DefaultGCPIdentityMode is the hub-wide fallback GCP metadata mode
	// ("block", "passthrough", or "assign").
	DefaultGCPIdentityMode *string `json:"default_gcp_identity_mode,omitempty"`
	// DefaultGCPIdentityServiceAccountID is the service account used when
	// DefaultGCPIdentityMode is "assign".
	DefaultGCPIdentityServiceAccountID *string `json:"default_gcp_identity_service_account_id,omitempty"`

	// AutoInjectGcloudADC controls whether gcloud ADC is injected into agent containers.
	AutoInjectGcloudADC *bool `json:"auto_inject_gcloud_adc,omitempty"`

	// AutoExposePorts controls whether ports are automatically exposed in agent containers.
	AutoExposePorts *config.AutoExposePortsSettings `json:"auto_expose_ports,omitempty"`

	// Quotas controls hub-level quota enforcement toggles.
	Quotas *config.QuotaSettings `json:"quotas,omitempty"`

	// AgentSecrets controls hub-level policy for secrets written by agents.
	AgentSecrets *config.AgentSecretsSettings `json:"agent_secrets,omitempty"`

	// Federation holds the federation authentication config update.
	Federation *config.V1FederationConfig `json:"federation,omitempty"`
}

// handleAdminServerConfig handles GET/PUT /api/v1/admin/server-config.
// GET: Returns the current global settings.yaml contents (sensitive fields masked).
// PUT: Updates global settings.yaml and optionally reloads applicable runtime settings.
func (s *Server) handleAdminServerConfig(w http.ResponseWriter, r *http.Request) {
	// Whenever OperationalSettings is wired (every DB driver, SQLite
	// included, since #1432) delegate to the DB-backed handlers: Layer-1
	// reads/writes go through the DB (design §3.8) and Layer-0 keys are
	// rejected with 422 exactly as on postgres. Writing settings.yaml on a
	// DB-backed SQLite hub let the next ops.Update re-apply the stale DB rows
	// and silently revert the write (ptone/scion#1091). Only a hub with no
	// OperationalSettings service keeps the file read/write path.
	if ops := s.GetOperationalSettings(); ops != nil {
		switch r.Method {
		case http.MethodGet:
			s.handleGetServerConfigDB(w, r, ops)
		case http.MethodPut, http.MethodPatch, http.MethodPost:
			// Require write permission for mutating operations.
			// The route guard already verified read access; this elevates to update.
			if s.authzService != nil {
				identity := GetIdentityFromContext(r.Context())
				if user, ok := identity.(UserIdentity); ok {
					decision := s.authzService.Decide(r.Context(), AuthzRequest{
						Principal:  principalContextForIdentity(user),
						Credential: credentialContextForIdentity(user),
						Resource:   Resource{Type: "hub", ID: "hub"},
						Action:     Action("update"),
						Permission: "hub.config.update",
					})
					if !decision.Allowed {
						Forbidden(w)
						return
					}
				}
			}
			s.handlePutServerConfigDB(w, r, ops)
		default:
			MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodPost)
		}
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGetServerConfig(w)
	case http.MethodPut, http.MethodPatch, http.MethodPost:
		// Require write permission for mutating operations.
		// The route guard already verified read access; this elevates to update.
		if s.authzService != nil {
			identity := GetIdentityFromContext(r.Context())
			if user, ok := identity.(UserIdentity); ok {
				decision := s.authzService.Decide(r.Context(), AuthzRequest{
					Principal:  principalContextForIdentity(user),
					Credential: credentialContextForIdentity(user),
					Resource:   Resource{Type: "hub", ID: "hub"},
					Action:     Action("update"),
					Permission: "hub.config.update",
				})
				if !decision.Allowed {
					Forbidden(w)
					return
				}
			}
		}
		s.handlePutServerConfig(w, r)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodPost)
	}
}

// handleAdminServerConfigSectionReset handles
// DELETE /api/v1/admin/server-config/sections/{name}
// Resets a managed section back to bootstrap material by deleting the DB row.
// Available whenever OperationalSettings is wired (any DB driver); admin-gated.
// Design §3.2.4.
func (s *Server) handleAdminServerConfigSectionReset(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())

	if r.Method != http.MethodDelete {
		MethodNotAllowed(w, http.MethodDelete)
		return
	}

	ops := s.GetOperationalSettings()
	if ops == nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"Section reset requires DB-backed operational settings", nil)
		return
	}

	sectionName := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/server-config/sections/")
	if sectionName == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"Section name is required", nil)
		return
	}

	// The "experiments" section has its own compare-and-set reset with a
	// per-name audit log (DELETE /api/v1/admin/experiments), gated on
	// hub.experiments.update. This generic route has no compare-and-set and
	// is gated on hub.config.update, so it must not be a second way to clear
	// every experiment override (ptone/scion#2217). Rejected before any
	// store call.
	if sectionName == "experiments" {
		writeError(w, http.StatusBadRequest, "validation_failed",
			"use DELETE /api/v1/admin/experiments", nil)
		return
	}

	sec := opsettings.SectionByName(sectionName)
	if sec == nil {
		writeError(w, http.StatusNotFound, ErrCodeNotFound,
			"Unknown section: "+sectionName, nil)
		return
	}

	if err := ops.DeleteSection(r.Context(), sectionName); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, ErrCodeNotFound,
				"Section not found in database: "+sectionName, nil)
			return
		}
		slog.Error("Failed to reset section", "section", sectionName, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to reset section", nil)
		return
	}

	slog.Info("Section reset to bootstrap", "section", sectionName, "by", user.Email())
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"reset":   true,
		"section": sectionName,
	})
}

// handleGetServerConfig reads and returns the global settings.yaml.
func (s *Server) handleGetServerConfig(w http.ResponseWriter) {
	resp, err := buildServerConfigFileResponse()
	if err != nil {
		var ue *serverConfigReadError
		if errors.As(err, &ue) {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, ue.userMsg, nil)
			return
		}
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to read settings", nil)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// buildServerConfigFileResponse builds the file-mode GET
// /api/v1/admin/server-config body (sensitive fields masked). The file-mode
// PUT also uses it as the reference view for echo detection.
func buildServerConfigFileResponse() (*ServerConfigResponse, error) {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return nil, &serverConfigReadError{"Failed to resolve settings directory", err}
	}

	settingsPath := filepath.Join(globalDir, "settings.yaml")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			resp := ServerConfigResponse{
				ScionVersion:   version.Short(),
				ScionCommit:    version.GetCommit(),
				ScionBuildTime: version.GetBuildTime(),
				SettingsTier:   "file",
				SchemaVersion:  "1",
			}
			envK := config.LoadEnvKoanf()
			if envOverrides := opsettings.DetectEnvOverrides(envK); len(envOverrides) > 0 {
				sort.Strings(envOverrides)
				resp.EnvOverrides = envOverrides
			}
			return &resp, nil
		}
		return nil, &serverConfigReadError{"Failed to read settings file", err}
	}

	var vs config.VersionedSettings
	if err := yamlv3.Unmarshal(data, &vs); err != nil {
		return nil, &serverConfigReadError{"Failed to parse settings file", err}
	}

	// Mask sensitive fields before sending to the client
	resp := ServerConfigResponse{
		ScionVersion:         version.Short(),
		ScionCommit:          version.GetCommit(),
		ScionBuildTime:       version.GetBuildTime(),
		SettingsTier:         "file",
		SchemaVersion:        vs.SchemaVersion,
		ActiveProfile:        vs.ActiveProfile,
		DefaultTemplate:      vs.DefaultTemplate,
		DefaultHarnessConfig: vs.DefaultHarnessConfig,
		ImageRegistry:        vs.ImageRegistry,
		WorkspacePath:        vs.WorkspacePath,
		Server:               vs.Server,
		Telemetry:            vs.Telemetry,
		Runtimes:             vs.Runtimes,
		HarnessConfigs:       vs.HarnessConfigs,
		Profiles:             vs.Profiles,
		DefaultMaxTurns:      vs.DefaultMaxTurns,
		DefaultMaxModelCalls: vs.DefaultMaxModelCalls,
		DefaultMaxDuration:   vs.DefaultMaxDuration,
		DefaultResources:     vs.DefaultResources,
		DefaultModel:         vs.DefaultModel,
		DefaultThinkingLevel: vs.DefaultThinkingLevel,
		DefaultMaxAgentRole:  vs.DefaultMaxAgentRole,
		DefaultAgentRole:     vs.DefaultAgentRole,
		DefaultRuntimeBroker: vs.DefaultRuntimeBroker,
		DefaultTimezone:      vs.DefaultTimezone,
		AutoInjectGcloudADC:  vs.AutoInjectGcloudADC,
		AutoExposePorts:      vs.AutoExposePorts,
		Quotas:               vs.Quotas,
		AgentSecrets:         vs.AgentSecrets,

		DefaultGCPIdentityMode:             vs.DefaultGCPIdentityMode,
		DefaultGCPIdentityServiceAccountID: vs.DefaultGCPIdentityServiceAccountID,
	}

	// Populate top-level federation field from the server config.
	if vs.Server != nil && vs.Server.Federation != nil {
		resp.Federation = vs.Server.Federation
	}

	// Env overrides — detect SCION_SERVER_* env vars so the admin UI can
	// show env-pinned fields in file mode too (H1).
	envK := config.LoadEnvKoanf()
	if envOverrides := opsettings.DetectEnvOverrides(envK); len(envOverrides) > 0 {
		sort.Strings(envOverrides)
		resp.EnvOverrides = envOverrides
	}

	maskSensitiveFields(&resp)
	return &resp, nil
}

// validateDefaultTimezone checks an agent_defaults.default_timezone
// candidate against the rule design.md §3 A (d) also uses for the per-user
// display-timezone preference: it must be a real IANA time zone name, and
// nonPortableTimezoneNames is rejected even though time.LoadLocation accepts
// those names. An empty string means UTC and is always valid.
//
// Delegates to validateIANATimezone (timezone_validate.go), shared with the
// per-user display-timezone preference validator (handlers_users_core.go's
// validateUserTimezone), so the two can't drift. Unlike validateUserTimezone,
// this one adds no wrapping of its own: errNonPortableTimezone's own text
// ("not an IANA time zone name") already says everything "default_timezone"
// needs — there is no "Auto" concept to mention here, which is the only
// reason validateUserTimezone's wording has to differ from the sentinel's.
func validateDefaultTimezone(tz string) error {
	if tz == "" {
		return nil
	}
	return validateIANATimezone(tz)
}

// handlePutServerConfig updates the global settings.yaml.
func (s *Server) handlePutServerConfig(w http.ResponseWriter, r *http.Request) {
	rawBody, err := readRawBody(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
		return
	}
	var req ServerConfigUpdateRequest
	if err := json.NewDecoder(bytes.NewReader(rawBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
		return
	}
	// The typed decode above silently drops a removed profiles.<name>.timezone
	// key, so check the raw body before settings.yaml is touched.
	if rejectRemovedProfileTimezone(w, rawBody) {
		return
	}
	// Any other key the typed decode drops (unknown, misspelt, or a flat
	// dotted "server.hub.x" key) is rejected with 422 before anything is
	// written, unless it echoes the GET view (ptone/scion#3463).
	if rejectUnknownFileConfigKeys(w, rawBody) {
		return
	}

	// server.hub.agent_endpoint has no live-reload path (like public_url, it
	// only takes effect at the next restart), so a malformed value written
	// here would otherwise only surface as a startup failure later. Reject it
	// at write time with the same validator the Hub uses at startup, and
	// persist the normalized form so the written value never diverges from
	// what the Hub will actually stamp into agents once it restarts.
	if req.Server != nil && req.Server.Hub != nil && req.Server.Hub.AgentEndpoint != "" {
		normalized, err := config.ValidateAgentEndpoint(req.Server.Hub.AgentEndpoint)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, err.Error(), nil)
			return
		}
		req.Server.Hub.AgentEndpoint = normalized
	}

	// server.auth.default_user_role must be one of the schema enum values
	// (design D6). The DB path validates section docs against the schema;
	// file mode has no schema pass, so validate this key against the same
	// access-section schema here rather than writing garbage to settings.yaml.
	if req.Server != nil && req.Server.Auth != nil && req.Server.Auth.DefaultUserRole != "" {
		doc, err := json.Marshal(opsettings.AccessSettings{DefaultUserRole: req.Server.Auth.DefaultUserRole})
		if err == nil {
			if errs := opsettings.Validate("access", doc); len(errs) > 0 {
				writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
					fmt.Sprintf("invalid server.auth.default_user_role %q: must be \"member\" or \"viewer\"", req.Server.Auth.DefaultUserRole), nil)
				return
			}
		}
	}

	// shared_dir_size on runtime and profile entries must be a Kubernetes
	// quantity; reject a bad value here, naming its key, rather than writing
	// it to settings.yaml where it would fail every agent start.
	if errs := config.ValidateSharedDirSizes(req.Runtimes, req.Profiles); len(errs) > 0 {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, errs[0].Error(), nil)
		return
	}

	// home_storage_backend and home_storage_leaf on runtime and profile
	// entries, and server.home_storage, must hold known values.
	if errs := config.ValidateHomeStorageOverrides(req.Runtimes, req.Profiles); len(errs) > 0 {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, errs[0].Error(), nil)
		return
	}
	if req.Server != nil {
		if err := req.Server.HomeStorage.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, err.Error(), nil)
			return
		}
	}

	// shared_dir_storage_backend on runtime and profile entries must be
	// "local" or "nfs", and "nfs" needs a complete
	// server.shared_dir_storage.nfs block (from this request, else the
	// current global settings). When the request changes
	// server.shared_dir_storage, it is also checked against the runtimes
	// and profiles already stored, so removing or emptying the nfs block
	// cannot strand an existing nfs override. Configuration only; no mount
	// is checked.
	sdInRequest := req.Server != nil && req.Server.SharedDirStorage != nil
	// Values and per-dir names do not depend on the current settings, so
	// they are checked even when those cannot be read below.
	if errs := config.ValidateSharedDirStorageBackendValues(req.Runtimes, req.Profiles); len(errs) > 0 {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, errs[0].Error(), nil)
		return
	}
	if req.Runtimes != nil || req.Profiles != nil || sdInRequest {
		runtimes, profiles := req.Runtimes, req.Profiles
		var sdGlobal *config.V1SharedDirStorageConfig
		if sdInRequest {
			sdGlobal = req.Server.SharedDirStorage
		}
		sdKnown := true
		if !sdInRequest || runtimes == nil || profiles == nil {
			gs, _, gErr := config.LoadGlobalSettings()
			switch {
			case gErr != nil:
				// The current settings cannot be read, so the merged
				// result is unknown; validation at agent start still
				// applies.
				sdKnown = false
			case gs != nil:
				if sdInRequest {
					if runtimes == nil {
						runtimes = gs.Runtimes
					}
					if profiles == nil {
						profiles = gs.Profiles
					}
				} else if gs.Server != nil {
					sdGlobal = gs.Server.SharedDirStorage
				}
			}
		}
		if sdKnown {
			if errs := config.ValidateSharedDirStorageBackends(runtimes, profiles, sdGlobal); len(errs) > 0 {
				writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, errs[0].Error(), nil)
				return
			}
		}
	}

	globalDir, err := config.GetGlobalDir()
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to resolve settings directory", nil)
		return
	}

	settingsPath := filepath.Join(globalDir, "settings.yaml")

	// Load existing settings to merge with updates
	var raw map[string]interface{}
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := yamlv3.Unmarshal(data, &raw); err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to parse existing settings", nil)
			return
		}
	}
	if raw == nil {
		raw = make(map[string]interface{})
	}

	// GET masks secrets and clients send the GET body back on save: restore
	// every still-masked field before anything is written. The stored view
	// is decoded from the same read that is merged and written below, so the
	// restore and the write see the same file contents.
	if req.Server != nil {
		stored, err := serverConfigFromRaw(raw)
		if err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to parse existing settings", nil)
			return
		}
		if err := restoreMaskedServerSecrets(req.Server, stored); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, err.Error(), nil)
			return
		}
	}

	// Apply updates by marshaling the request fields and merging. The raw
	// server object tells the merge which server fields were sent.
	rawServer := rawServerObject(rawBody)
	applySettingsUpdatesFromBody(raw, &req, rawServer)

	// The server section is deep-merged, so a section the request changes
	// only in part is validated again as merged with the stored fields.
	if req.Server != nil {
		if err := validateMergedServerSections(raw, rawServer); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, err.Error(), nil)
			return
		}
	}

	// Validate the effective hub default GCP identity (the merged result, so
	// a PUT that changes only one of the pair is checked against the other's
	// stored value). Same checks as the DB-mode handler.
	if req.DefaultGCPIdentityMode != nil || req.DefaultGCPIdentityServiceAccountID != nil {
		mode, _ := raw["default_gcp_identity_mode"].(string)
		saID, _ := raw["default_gcp_identity_service_account_id"].(string)
		if !s.validateHubDefaultGCPIdentity(w, r.Context(), opsettings.AgentDefaultsSettings{
			DefaultGCPIdentityMode:             mode,
			DefaultGCPIdentityServiceAccountID: saID,
		}) {
			return
		}
	}

	// Validate the hub default timezone (IANA name check) before writing.
	// Same rule, same 422, as the DB-mode handler (admin_settings_db.go) —
	// without this, an invalid name is written to settings.yaml silently and
	// never rejected in file mode.
	if req.DefaultTimezone != nil {
		tz := *req.DefaultTimezone
		if err := validateDefaultTimezone(tz); err != nil {
			writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationError,
				fmt.Sprintf("invalid default_timezone %q: %v", tz, err), nil)
			return
		}
	}

	// Ensure schema_version is set
	if _, ok := raw["schema_version"]; !ok {
		raw["schema_version"] = "1"
	}

	newData, err := yamlv3.Marshal(raw)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to marshal settings", nil)
		return
	}

	// safe_to_evict on a non-Kubernetes runtime is saved and ignored, with
	// the same warning as config validate. Checked on the merged file so a
	// profile is matched against a runtime saved earlier.
	var saveWarnings []string
	if req.Runtimes != nil || req.Profiles != nil {
		var merged struct {
			Runtimes map[string]config.V1RuntimeConfig `yaml:"runtimes"`
			Profiles map[string]config.V1ProfileConfig `yaml:"profiles"`
		}
		if yamlv3.Unmarshal(newData, &merged) == nil {
			saveWarnings = safeToEvictSaveWarnings(merged.Runtimes, merged.Profiles)
		}
	}

	if err := os.WriteFile(settingsPath, newData, 0644); err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to write settings file", nil)
		return
	}

	slog.Info("Server config updated via admin API",
		"user", user(GetUserIdentityFromContext(r.Context())),
	)

	// Attempt to reload applicable runtime settings
	reloadResults := s.reloadSettings()

	resp := map[string]interface{}{
		"status": "saved",
		"reload": reloadResults,
	}
	if len(saveWarnings) > 0 {
		resp["warnings"] = saveWarnings
	}
	writeJSON(w, http.StatusOK, resp)
}

// safeToEvictSaveWarnings returns, and logs, a warning for each runtime or
// profile that sets safe_to_evict, or home_storage_backend "nfs", on a
// non-Kubernetes runtime. The value is saved and ignored at agent start;
// this is the same rule as config validate. Used by both the file-mode and
// DB-mode PUT handlers.
func safeToEvictSaveWarnings(runtimes map[string]config.V1RuntimeConfig, profiles map[string]config.V1ProfileConfig) []string {
	warnings := config.SafeToEvictIgnoredWarnings(runtimes, profiles)
	warnings = append(warnings, config.HomeStorageIgnoredWarnings(runtimes, profiles)...)
	for _, msg := range warnings {
		slog.Warn("Server config saved with an ignored setting", "warning", msg)
	}
	return warnings
}

// reloadSettings re-reads the settings file and applies runtime-changeable values.
// Returns a summary of what was reloaded and what requires a restart.
//
// This is the file-mode path: it loads GlobalConfig from settings.yaml,
// builds a Layer1Snapshot, and delegates to applySnapshot. It is used only by
// a hub without OperationalSettings; with it (any DB driver) the service
// provides the snapshot instead.
func (s *Server) reloadSettings() map[string]interface{} {
	results := map[string]interface{}{
		"applied":          []string{},
		"requires_restart": []string{},
	}

	gc, err := config.LoadGlobalConfig("")
	if err != nil {
		slog.Error("Failed to reload global config", "error", err)
		results["error"] = err.Error()
		return results
	}

	snap := BuildLayer1SnapshotFromFile(gc)
	results = ApplySnapshot(s, snap)

	// Log level is a Layer-0 setting (per design §3.1) — only applied in
	// file mode via reloadSettings, not through OperationalSettings.
	if gc.LogLevel != "" {
		applySnapshotLogLevel(gc.LogLevel)
		applied := results["applied"].([]string)
		applied = append(applied, "log_level")
		results["applied"] = applied
	}

	// Detect container runtime changes and reload the co-located broker.
	// This covers the onboarding wizard flow where the user selects a
	// different runtime (e.g. podman) after the server auto-detected one
	// at startup. Without this, the broker continues using the stale
	// runtime until a manual server restart.
	s.mu.RLock()
	reloadFn := s.runtimeReloadFunc
	s.mu.RUnlock()
	if reloadFn != nil {
		if reloaded := reloadFn(); reloaded {
			applied := results["applied"].([]string)
			applied = append(applied, "broker_runtime")
			results["applied"] = applied
		}
	}

	return results
}

// setOrDeleteString applies an optional string update to the raw settings
// map: nil leaves the key untouched, "" deletes it, anything else sets it.
func setOrDeleteString(raw map[string]interface{}, key string, v *string) {
	if v == nil {
		return
	}
	if *v == "" {
		delete(raw, key)
		return
	}
	raw[key] = *v
}

// applySettingsUpdates merges the update request into the raw settings map.
// The server section is deep-merged from the typed request alone, so a
// zero value in req.Server cannot be told apart from an omitted field and
// keeps the stored value. The PUT handler uses applySettingsUpdatesFromBody
// so that an explicit "" or null in the body clears a server field.
func applySettingsUpdates(raw map[string]interface{}, req *ServerConfigUpdateRequest) {
	applySettingsUpdatesFromBody(raw, req, nil)
}

// applySettingsUpdatesFromBody is applySettingsUpdates with the request's
// raw "server" JSON object, which tells mergeServerSettings which server
// fields the client sent. A nil rawServer derives that from req.Server.
func applySettingsUpdatesFromBody(raw map[string]interface{}, req *ServerConfigUpdateRequest, rawServer json.RawMessage) {
	if req.SchemaVersion != nil {
		raw["schema_version"] = *req.SchemaVersion
	}
	// Top-level string settings: an explicit "" deletes the key from
	// settings.yaml; a nil pointer (key omitted) means "no change".
	setOrDeleteString(raw, "active_profile", req.ActiveProfile)
	setOrDeleteString(raw, "default_template", req.DefaultTemplate)
	setOrDeleteString(raw, "default_harness_config", req.DefaultHarnessConfig)
	setOrDeleteString(raw, "image_registry", req.ImageRegistry)
	setOrDeleteString(raw, "workspace_path", req.WorkspacePath)

	if req.Server != nil {
		mergeServerSettings(raw, req.Server, rawServer)
	}
	if req.Telemetry != nil {
		raw["telemetry"] = marshalToMap(req.Telemetry)
	}
	if req.Runtimes != nil {
		raw["runtimes"] = marshalToMap(req.Runtimes)
	}
	if req.HarnessConfigs != nil {
		raw["harness_configs"] = marshalToMap(req.HarnessConfigs)
	}
	if req.Profiles != nil {
		raw["profiles"] = marshalToMap(req.Profiles)
	}

	if req.DefaultMaxTurns != nil {
		if *req.DefaultMaxTurns > 0 {
			raw["default_max_turns"] = *req.DefaultMaxTurns
		} else {
			delete(raw, "default_max_turns")
		}
	}
	if req.DefaultMaxModelCalls != nil {
		if *req.DefaultMaxModelCalls > 0 {
			raw["default_max_model_calls"] = *req.DefaultMaxModelCalls
		} else {
			delete(raw, "default_max_model_calls")
		}
	}
	if req.DefaultMaxDuration != nil {
		if *req.DefaultMaxDuration != "" {
			raw["default_max_duration"] = *req.DefaultMaxDuration
		} else {
			delete(raw, "default_max_duration")
		}
	}
	if req.DefaultResources != nil {
		raw["default_resources"] = marshalToMap(req.DefaultResources)
	}
	if req.DefaultModel != nil {
		if *req.DefaultModel != "" {
			raw["default_model"] = *req.DefaultModel
		} else {
			delete(raw, "default_model")
		}
	}
	if req.DefaultThinkingLevel != nil {
		if *req.DefaultThinkingLevel > 0 {
			raw["default_thinking_level"] = *req.DefaultThinkingLevel
		} else {
			delete(raw, "default_thinking_level")
		}
	}
	setOrDeleteString(raw, "default_max_agent_role", req.DefaultMaxAgentRole)
	setOrDeleteString(raw, "default_agent_role", req.DefaultAgentRole)
	setOrDeleteString(raw, "default_runtime_broker", req.DefaultRuntimeBroker)
	if req.DefaultTimezone != nil {
		if *req.DefaultTimezone != "" {
			raw["default_timezone"] = *req.DefaultTimezone
		} else {
			delete(raw, "default_timezone")
		}
	}
	if req.DefaultGCPIdentityMode != nil {
		if *req.DefaultGCPIdentityMode != "" {
			raw["default_gcp_identity_mode"] = *req.DefaultGCPIdentityMode
		} else {
			delete(raw, "default_gcp_identity_mode")
		}
	}
	if req.DefaultGCPIdentityServiceAccountID != nil {
		if *req.DefaultGCPIdentityServiceAccountID != "" {
			raw["default_gcp_identity_service_account_id"] = *req.DefaultGCPIdentityServiceAccountID
		} else {
			delete(raw, "default_gcp_identity_service_account_id")
		}
	}
	if req.AutoInjectGcloudADC != nil {
		if *req.AutoInjectGcloudADC {
			raw["auto_inject_gcloud_adc"] = true
		} else {
			delete(raw, "auto_inject_gcloud_adc")
		}
	}
	if req.AutoExposePorts != nil {
		// Section-generic zero check; see the Quotas block below.
		if !isZeroStruct(req.AutoExposePorts) {
			raw["auto_expose_ports"] = marshalToMap(req.AutoExposePorts)
		} else {
			delete(raw, "auto_expose_ports")
		}
	}
	if req.Quotas != nil {
		// Section-generic zero check (matches isZeroStruct's use elsewhere,
		// admin_settings_db.go): checking a single named field (e.g.
		// EnforceBrokerQuotas != nil) would silently stop deleting empty
		// documents the moment QuotaSettings gains a second field, since a
		// request with only the new field set would then wrongly delete the
		// whole section. Delete only when every field is nil/zero.
		if !isZeroStruct(req.Quotas) {
			raw["quotas"] = marshalToMap(req.Quotas)
		} else {
			delete(raw, "quotas")
		}
	}
	if req.AgentSecrets != nil {
		// Section-generic zero check; see the Quotas block above.
		if !isZeroStruct(req.AgentSecrets) {
			raw["agent_secrets"] = marshalToMap(req.AgentSecrets)
		} else {
			delete(raw, "agent_secrets")
		}
	}
	if req.Federation != nil {
		serverMap, ok := raw["server"].(map[string]interface{})
		if !ok {
			serverMap = make(map[string]interface{})
			raw["server"] = serverMap
		}
		serverMap["federation"] = marshalToMap(req.Federation)
	}
}

// marshalToMap converts a struct to a map[string]interface{} via YAML round-trip.
func marshalToMap(v interface{}) interface{} {
	data, err := yamlv3.Marshal(v)
	if err != nil {
		return v
	}
	var m interface{}
	if err := yamlv3.Unmarshal(data, &m); err != nil {
		return v
	}
	return m
}

// user returns the email or ID string for logging purposes.
func user(u UserIdentity) string {
	if u == nil {
		return "unknown"
	}
	return u.Email()
}
