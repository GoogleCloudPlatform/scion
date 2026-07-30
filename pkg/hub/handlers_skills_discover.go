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
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// skillDiscoverKind is the resourceImportKind used to scan a fetched remote
// directory for skills. A directory is a skill when it contains a SKILL.md.
//
// newStore is nil because skill discovery never persists anything: skills are
// referenced by URI, not imported into hub object storage. Only
// discoverResourceDirs is ever called with this kind, and that function does
// not touch newStore.
var skillDiscoverKind = resourceImportKind{
	noun:   "skills",
	marker: "SKILL.md",
	isResourceDir: func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, "SKILL.md"))
		return err == nil
	},
	newStore: nil,
}

// DiscoverSkillsRequest is the request body for
// POST /api/v1/skills/discover-directory.
//
// ProjectID is optional; when set it is used to resolve GitHub credentials (a
// GitHub App installation token, falling back to the project's GITHUB_TOKEN
// secret) so private repositories can be scanned.
type DiscoverSkillsRequest struct {
	SourceURL string `json:"sourceUrl"`
	ProjectID string `json:"projectId,omitempty"`
}

// DiscoveredSkill is one skill found at the discovered directory. URI is the
// canonical skill URI (as produced by api.NormalizeSkillURI) and Name is the
// bare directory name, used as the display label.
type DiscoveredSkill struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

// DiscoverSkillsResponse is the response body for
// POST /api/v1/skills/discover-directory.
type DiscoverSkillsResponse struct {
	Skills []DiscoveredSkill `json:"skills"`
	Count  int               `json:"count"`
}

// handleSkillsDiscoverDirectory handles POST /api/v1/skills/discover-directory:
// it fetches a remote GitHub directory, scans it for subdirectories containing
// a SKILL.md, and returns the canonical skill URI plus display name for each.
//
// Nothing is persisted — this is a read-only probe used by the UI (and, in a
// later phase, the CLI) to offer a batch-add selection list. Unlike template
// and harness-config discovery, no hub object storage is required.
func (s *Server) handleSkillsDiscoverDirectory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	var req DiscoverSkillsRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
		return
	}

	// Auth: agents need project:agent:create and may only discover for their own
	// project (mirrors handleProjectDiscoverTemplates); users need only to be
	// authenticated, since discovery reads a public-or-project-credentialed
	// GitHub URL and persists nothing.
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if !agentIdent.HasScope(ScopeAgentCreate) {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope: project:agent:create", nil)
			return
		}
		if req.ProjectID != "" && req.ProjectID != agentIdent.ProjectID() {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only discover skills within their own project", nil)
			return
		}
		// Scope the fetch to the agent's own project even when the caller
		// omitted projectId, so project GitHub credentials still apply.
		req.ProjectID = agentIdent.ProjectID()
	} else if userIdent := GetUserIdentityFromContext(ctx); userIdent == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "Authentication required", nil)
		return
	}

	if req.SourceURL == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "sourceUrl is required", nil)
		return
	}
	if !config.IsRemoteURI(req.SourceURL) {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"sourceUrl must be a remote URI (http://, https://, or rclone)", nil)
		return
	}

	cachePath, err := s.fetchRemoteForImport(ctx, req.ProjectID, req.SourceURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "discover_failed", "Failed to fetch remote skills: "+err.Error(), nil)
		return
	}
	defer func() { _ = os.RemoveAll(cachePath) }()

	dirs, _, err := discoverResourceDirs(cachePath, req.SourceURL, skillDiscoverKind)
	if err != nil {
		writeError(w, http.StatusBadRequest, "discover_failed", err.Error(), nil)
		return
	}

	skills := make([]DiscoveredSkill, 0, len(dirs))
	for _, d := range dirs {
		uri, normErr := api.NormalizeSkillURI(d.sourceURL)
		if normErr != nil {
			// A directory whose URL cannot be expressed as a skill URI is not
			// addressable, so it cannot be added. Drop it rather than failing
			// the whole discovery.
			s.resourceLog.Warn("skipping discovered skill with unnormalizable URI",
				"sourceURL", d.sourceURL, "error", normErr)
			continue
		}
		skills = append(skills, DiscoveredSkill{URI: uri, Name: d.name})
	}

	if len(skills) == 0 {
		writeError(w, http.StatusBadRequest, "discover_failed",
			"no skills found at "+req.SourceURL, nil)
		return
	}

	writeJSON(w, http.StatusOK, DiscoverSkillsResponse{Skills: skills, Count: len(skills)})
}
