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
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"net/http"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Cached placeholder PNGs — generated once, reused on every request.
var (
	colorPNGOnce     sync.Once
	outlinePNGOnce   sync.Once
	cachedColorPNG   []byte
	cachedOutlinePNG []byte
	colorPNGErr      error
	outlinePNGErr    error
)

// teamsManifest mirrors the Azure Teams app manifest schema (v1.16).
type teamsManifest struct {
	Schema          string           `json:"$schema"`
	ManifestVersion string           `json:"manifestVersion"`
	Version         string           `json:"version"`
	ID              string           `json:"id"`
	Developer       teamsDevInfo     `json:"developer"`
	Name            teamsName        `json:"name"`
	Description     teamsDescription `json:"description"`
	Icons           teamsIcons       `json:"icons"`
	AccentColor     string           `json:"accentColor"`
	Bots            []teamsBot       `json:"bots"`
	Permissions     []string         `json:"permissions"`
	ValidDomains    []string         `json:"validDomains"`
}

type teamsDevInfo struct {
	Name          string `json:"name"`
	WebsiteURL    string `json:"websiteUrl"`
	PrivacyURL    string `json:"privacyUrl"`
	TermsOfUseURL string `json:"termsOfUseUrl"`
}

type teamsName struct {
	Short string `json:"short"`
	Full  string `json:"full"`
}

type teamsDescription struct {
	Short string `json:"short"`
	Full  string `json:"full"`
}

type teamsIcons struct {
	Color   string `json:"color"`
	Outline string `json:"outline"`
}

type teamsBot struct {
	BotID              string             `json:"botId"`
	Scopes             []string           `json:"scopes"`
	SupportsFiles      bool               `json:"supportsFiles"`
	IsNotificationOnly bool               `json:"isNotificationOnly"`
	CommandLists       []teamsBotCommands `json:"commandLists"`
}

type teamsBotCommands struct {
	Scopes   []string          `json:"scopes"`
	Commands []teamsBotCommand `json:"commands"`
}

type teamsBotCommand struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

// handleTeamsManifestDownload generates and returns a Teams app manifest .zip.
// GET /api/v1/admin/integrations/teams/manifest
func (s *Server) handleTeamsManifestDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

	// Read current Teams config to get app_id and tenant_id.
	cfg, err := s.loadTeamsConfig()
	if err != nil {
		slog.Error("failed to load Teams config for manifest", "error", err)
		http.Error(w, "Teams integration is not configured", http.StatusBadRequest)
		return
	}

	appID := cfg["app_id"]
	if appID == "" {
		http.Error(w, "app_id is not configured — set it in the Teams integration settings first", http.StatusBadRequest)
		return
	}

	manifest := buildTeamsManifest(appID)

	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		slog.Error("failed to marshal Teams manifest", "error", err)
		http.Error(w, "internal error generating manifest", http.StatusInternalServerError)
		return
	}

	// Build zip archive in memory.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	// Add manifest.json.
	mf, err := zw.Create("manifest.json")
	if err != nil {
		slog.Error("failed to create manifest.json in zip", "error", err)
		http.Error(w, "internal error generating zip", http.StatusInternalServerError)
		return
	}
	if _, err := mf.Write(manifestJSON); err != nil {
		slog.Error("failed to write manifest.json", "error", err)
		http.Error(w, "internal error generating zip", http.StatusInternalServerError)
		return
	}

	// Add color.png (192x192) — uses cached, pre-generated image.
	colorPNGOnce.Do(func() {
		cachedColorPNG, colorPNGErr = generatePlaceholderPNG(192, color.RGBA{R: 74, G: 144, B: 217, A: 255})
	})
	if colorPNGErr != nil {
		slog.Error("failed to generate color.png", "error", colorPNGErr)
		http.Error(w, "internal error generating manifest", http.StatusInternalServerError)
		return
	}
	cf, err := zw.Create("color.png")
	if err != nil {
		slog.Error("failed to create color.png in zip", "error", err)
		http.Error(w, "internal error generating zip", http.StatusInternalServerError)
		return
	}
	if _, err := cf.Write(cachedColorPNG); err != nil {
		slog.Error("failed to write color.png", "error", err)
		http.Error(w, "internal error generating zip", http.StatusInternalServerError)
		return
	}

	// Add outline.png (32x32) — uses cached, pre-generated image.
	outlinePNGOnce.Do(func() {
		cachedOutlinePNG, outlinePNGErr = generatePlaceholderPNG(32, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	})
	if outlinePNGErr != nil {
		slog.Error("failed to generate outline.png", "error", outlinePNGErr)
		http.Error(w, "internal error generating manifest", http.StatusInternalServerError)
		return
	}
	of, err := zw.Create("outline.png")
	if err != nil {
		slog.Error("failed to create outline.png in zip", "error", err)
		http.Error(w, "internal error generating zip", http.StatusInternalServerError)
		return
	}
	if _, err := of.Write(cachedOutlinePNG); err != nil {
		slog.Error("failed to write outline.png", "error", err)
		http.Error(w, "internal error generating zip", http.StatusInternalServerError)
		return
	}

	if err := zw.Close(); err != nil {
		slog.Error("failed to finalize zip", "error", err)
		http.Error(w, "internal error generating zip", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="teams-app.zip"`)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", buf.Len()))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(buf.Bytes()); err != nil {
		slog.Error("failed to write manifest zip response", "error", err)
	}
}

// loadTeamsConfig reads the current Teams integration config.
func (s *Server) loadTeamsConfig() (map[string]string, error) {
	s.mu.RLock()
	mgr := s.pluginManager
	s.mu.RUnlock()

	if mgr == nil {
		return nil, fmt.Errorf("plugin manager not available")
	}

	configFile := mgr.GetPluginConfigFile("broker", "teams")
	cfg, err := config.ResolvePluginConfig(configFile, nil)
	if err != nil {
		return nil, fmt.Errorf("resolve plugin config: %w", err)
	}
	return cfg, nil
}

// buildTeamsManifest constructs a Teams app manifest from config values.
func buildTeamsManifest(appID string) teamsManifest {
	return teamsManifest{
		Schema:          "https://developer.microsoft.com/json-schemas/teams/v1.16/MicrosoftTeams.schema.json",
		ManifestVersion: "1.16",
		Version:         "1.0.0",
		ID:              appID,
		Developer: teamsDevInfo{
			Name:          "Scion",
			WebsiteURL:    "https://github.com/GoogleCloudPlatform/scion",
			PrivacyURL:    "https://github.com/GoogleCloudPlatform/scion",
			TermsOfUseURL: "https://github.com/GoogleCloudPlatform/scion",
		},
		Name: teamsName{
			Short: "Scion",
			Full:  "Scion Agent Orchestration",
		},
		Description: teamsDescription{
			Short: "Chat with Scion agents from Microsoft Teams",
			Full:  "Scion Teams integration provides bidirectional messaging between Microsoft Teams channels and Scion agents. Link channels to projects, message agents via @-mention, receive status updates and interactive prompts via Adaptive Cards, and manage your agent workflows directly from Teams.",
		},
		Icons: teamsIcons{
			Color:   "color.png",
			Outline: "outline.png",
		},
		AccentColor: "#4A90D9",
		Bots: []teamsBot{
			{
				BotID:              appID,
				Scopes:             []string{"personal", "team", "groupChat"},
				SupportsFiles:      false,
				IsNotificationOnly: false,
				CommandLists: []teamsBotCommands{
					{
						Scopes: []string{"personal", "team", "groupChat"},
						Commands: []teamsBotCommand{
							{Title: "setup", Description: "Link this channel to a Scion project"},
							{Title: "unlink", Description: "Unlink this channel from its project"},
							{Title: "agents", Description: "List agents in the linked project"},
							{Title: "status", Description: "Show project or agent status"},
							{Title: "register", Description: "Link your Teams account to your Scion identity"},
							{Title: "unregister", Description: "Unlink your Teams account from Scion"},
							{Title: "help", Description: "Show available commands"},
						},
					},
				},
			},
		},
		Permissions:  []string{"identity", "messageTeamMembers"},
		ValidDomains: []string{},
	}
}

// generatePlaceholderPNG creates a minimal solid-color PNG of the given size.
func generatePlaceholderPNG(size int, c color.Color) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encode PNG: %w", err)
	}
	return buf.Bytes(), nil
}
