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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// PublicSettingsResponse contains non-sensitive server settings for the web UI.
type PublicSettingsResponse struct {
	TelemetryEnabled       bool `json:"telemetryEnabled"`
	AutoExposePortsEnabled bool `json:"autoExposePortsEnabled"`
	// NativeChatEnabled mirrors the server.native_chat.enabled toggle so the
	// web UI can hide chat without needing admin rights to read the config.
	NativeChatEnabled bool `json:"nativeChatEnabled"`
	// DefaultRuntimeBroker is the hub-level default broker ID/slug/name.
	// The agent-create UI uses it as a fallback when no project default is set.
	DefaultRuntimeBroker string `json:"defaultRuntimeBroker,omitempty"`
	// DefaultHarnessConfig is the hub-level default harness config name.
	// The agent-create UI uses it as a fallback when no project default is set,
	// so client-side resolution matches the server's applyHubAgentDefaults chain.
	DefaultHarnessConfig string `json:"defaultHarnessConfig,omitempty"`
	// DefaultTemplate is the hub-level default template name/slug.
	DefaultTemplate string `json:"defaultTemplate,omitempty"`
	// DefaultModel is the hub-level default model identifier.
	DefaultModel string `json:"defaultModel,omitempty"`
	// AgentSecretsUserScopeOnly mirrors agent_secrets.user_scope_only so the
	// web terminal's capture dialog can disable Project and preselect
	// Profile without needing admin rights to read the config (design
	// ptone/scion#2291 §7). Exposing this policy boolean on an
	// unauthenticated endpoint reveals nothing beyond what a 403 would
	// reveal anyway.
	AgentSecretsUserScopeOnly bool `json:"agentSecretsUserScopeOnly"`
	// Branding carries Hub-wide UI branding (non-boolean fields only) when
	// server.hub.branding is customized; omitted when stock defaults apply.
	Branding *config.PublicBranding `json:"branding,omitempty"`
	// Uploads are the web upload limits (server.hub.uploads), so the web UI
	// can check a file before sending it.
	Uploads PublicUploadLimits `json:"uploads"`
}

// PublicUploadLimits are the upload limits the web UI reads, in bytes.
type PublicUploadLimits struct {
	// MaxAttachmentSize is the per-file limit of a chat attachment
	// uploaded from the web UI (the artifact service's file limit while
	// attachments are stored there).
	MaxAttachmentSize int64 `json:"maxAttachmentSize"`
	// MaxWorkspaceFileSize is the per-file limit of a project workspace or
	// shared directory upload.
	MaxWorkspaceFileSize int64 `json:"maxWorkspaceFileSize"`
}

// nativeChatEnabled reports whether the built-in chat feature is active.
// Chat shipped default-on, so an absent config means enabled; only an
// explicit server.native_chat.enabled: false turns it off.
func (s *Server) nativeChatEnabled() bool {
	if s.config.NativeChatEnabled == nil {
		return true
	}
	return *s.config.NativeChatEnabled
}

func (s *Server) handlePublicSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	telemetryEnabled := false
	if s.config.TelemetryDefault != nil {
		telemetryEnabled = *s.config.TelemetryDefault
	}

	autoExposePortsEnabled := false
	if s.config.AutoExposePortsDefault != nil {
		autoExposePortsEnabled = *s.config.AutoExposePortsDefault
	}

	defaults := s.hubAgentDefaults()

	writeJSON(w, http.StatusOK, PublicSettingsResponse{
		TelemetryEnabled:          telemetryEnabled,
		AutoExposePortsEnabled:    autoExposePortsEnabled,
		NativeChatEnabled:         s.nativeChatEnabled(),
		DefaultRuntimeBroker:      defaults.DefaultRuntimeBroker,
		DefaultHarnessConfig:      defaults.DefaultHarnessConfig,
		DefaultTemplate:           defaults.DefaultTemplate,
		DefaultModel:              defaults.DefaultModel,
		AgentSecretsUserScopeOnly: s.agentSecretsUserScopeOnly(),
		Branding:                  s.ResolvedBranding().Public(),
		Uploads: PublicUploadLimits{
			MaxAttachmentSize:    s.webAttachmentMaxBytes(r.Context()),
			MaxWorkspaceFileSize: s.ResolvedUploads().EffectiveMaxWorkspaceFileSize(),
		},
	})
}
