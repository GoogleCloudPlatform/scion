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

package hubclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// AdminSettingsService covers the hub's administrative settings: the
// server configuration (/api/v1/admin/server-config). Write refusals are
// returned as *SettingsError.
type AdminSettingsService interface {
	// GetServerConfig returns the hub's server configuration.
	GetServerConfig(ctx context.Context) (*ServerConfig, error)

	// UpdateServerConfig saves a server configuration change. body holds
	// only the keys to change, nested by their dotted path; an explicit
	// null clears a key. expectedRevisions maps a section name to the
	// revision the change was based on (compare-and-set); nil sends none.
	UpdateServerConfig(ctx context.Context, body map[string]interface{}, expectedRevisions map[string]int64) (*ServerConfigSaveResult, error)

	// ResetServerConfigSection deletes the stored copy of a section, so
	// the hub falls back to its startup configuration for it.
	ResetServerConfigSection(ctx context.Context, section string) error

	// GetServerConfigSchema returns the settings sections with their keys
	// and JSON schema.
	GetServerConfigSchema(ctx context.Context) (map[string]ServerConfigSectionSchema, error)
}

// ServerConfigSectionMetadata is a section's provenance in the server
// configuration.
type ServerConfigSectionMetadata struct {
	Source    string     `json:"source"`
	Revision  int64      `json:"revision,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	UpdatedBy string     `json:"updated_by,omitempty"`
	Origin    string     `json:"origin,omitempty"`
}

// ServerConfig is the hub's server configuration. Raw holds every
// top-level member of the response, so keys this client has no field for
// stay readable through Lookup.
type ServerConfig struct {
	Raw map[string]json.RawMessage `json:"-"`

	SettingsTier    string                                 `json:"settings_tier,omitempty"`
	SectionMetadata map[string]ServerConfigSectionMetadata `json:"section_metadata,omitempty"`
	Layer0Editable  bool                                   `json:"layer0_editable"`
}

// UnmarshalJSON keeps the raw members alongside the typed fields.
func (c *ServerConfig) UnmarshalJSON(data []byte) error {
	type plain ServerConfig
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*c = ServerConfig(p)
	c.Raw = raw
	return nil
}

// MarshalJSON returns the response as received.
func (c ServerConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(c.Raw)
}

// Lookup returns the value at a dotted key (for example
// "server.hub.public_url"), and whether it is present.
func (c *ServerConfig) Lookup(key string) (json.RawMessage, bool) {
	return LookupDotted(c.Raw, key)
}

// LookupDotted walks a JSON object by a dotted key.
func LookupDotted(obj map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	parts := strings.Split(key, ".")
	cur := obj
	for i, p := range parts {
		v, ok := cur[p]
		if !ok {
			return nil, false
		}
		if i == len(parts)-1 {
			return v, true
		}
		var next map[string]json.RawMessage
		if err := json.Unmarshal(v, &next); err != nil || next == nil {
			return nil, false
		}
		cur = next
	}
	return nil, false
}

// ServerConfigSaveResult is the hub's report of a saved server
// configuration change.
type ServerConfigSaveResult struct {
	Status string `json:"status"`
	Reload struct {
		// Applied lists the sections written.
		Applied         []string `json:"applied"`
		RequiresRestart []string `json:"requires_restart"`
		// AppliedKeys lists the changed keys in effect now; PendingRestart
		// the changed keys that apply after a hub restart; Skipped the
		// changed keys saved but not in use.
		AppliedKeys    []string             `json:"applied_keys"`
		PendingRestart []string             `json:"pending_restart"`
		Skipped        []SettingsSkippedKey `json:"skipped"`
		FileApplied    []string             `json:"file_applied"`
	} `json:"reload"`
	// Unchanged lists the sections the change left as they were.
	Unchanged []string `json:"unchanged,omitempty"`
	// FileKeys lists the keys written to the hub's settings file.
	FileKeys []string `json:"file_keys,omitempty"`
	// Warnings are advisory messages about the saved values.
	Warnings []string `json:"warnings,omitempty"`
}

// ServerConfigSectionSchema describes one settings section.
type ServerConfigSectionSchema struct {
	Schema          json.RawMessage `json:"schema"`
	KoanfPaths      []string        `json:"koanf_paths"`
	RestartRequired []string        `json:"restart_required,omitempty"`
}

type adminSettingsService struct {
	c *client
}

const serverConfigPath = "/api/v1/admin/server-config"

func (s *adminSettingsService) GetServerConfig(ctx context.Context) (*ServerConfig, error) {
	resp, err := s.c.get(ctx, serverConfigPath, nil)
	if err != nil {
		return nil, err
	}
	return decodeSettingsResponse[ServerConfig](resp)
}

func (s *adminSettingsService) UpdateServerConfig(ctx context.Context, body map[string]interface{}, expectedRevisions map[string]int64) (*ServerConfigSaveResult, error) {
	if _, ok := body["expected_revisions"]; ok {
		return nil, fmt.Errorf("expected_revisions is not a settings key; pass it as expectedRevisions")
	}
	req := make(map[string]interface{}, len(body)+1)
	for k, v := range body {
		req[k] = v
	}
	if len(expectedRevisions) > 0 {
		req["expected_revisions"] = expectedRevisions
	}
	resp, err := s.c.put(ctx, serverConfigPath, req, nil)
	if err != nil {
		return nil, err
	}
	return decodeSettingsResponse[ServerConfigSaveResult](resp)
}

func (s *adminSettingsService) ResetServerConfigSection(ctx context.Context, section string) error {
	if section == "" {
		return fmt.Errorf("section name is required")
	}
	resp, err := s.c.delete(ctx, serverConfigPath+"/sections/"+url.PathEscape(section), nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseSettingsError(resp)
	}
	return nil
}

func (s *adminSettingsService) GetServerConfigSchema(ctx context.Context) (map[string]ServerConfigSectionSchema, error) {
	resp, err := s.c.get(ctx, serverConfigPath+"/schema", nil)
	if err != nil {
		return nil, err
	}
	out, err := decodeSettingsResponse[struct {
		Sections map[string]ServerConfigSectionSchema `json:"sections"`
	}](resp)
	if err != nil {
		return nil, err
	}
	return out.Sections, nil
}
