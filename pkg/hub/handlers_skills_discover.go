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
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
// Skipped names the child directories that were passed over — either because
// they had no SKILL.md, or because their name could not be turned into a safe,
// addressable skill URI — so the UI can explain why a folder the user expected
// is missing.
type DiscoverSkillsResponse struct {
	Skills  []DiscoveredSkill `json:"skills"`
	Skipped []string          `json:"skipped,omitempty"`
	Count   int               `json:"count"`
}

// maxDiscoverBodyBytes caps the request body. The body is two short strings;
// anything larger is a mistake or an attempt to make the hub buffer garbage.
const maxDiscoverBodyBytes = 64 << 10

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

	r.Body = http.MaxBytesReader(w, r.Body, maxDiscoverBodyBytes)

	var req DiscoverSkillsRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
		return
	}

	if !s.authorizeSkillDiscover(ctx, w, &req) {
		return
	}

	if req.SourceURL == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "sourceUrl is required", nil)
		return
	}
	// Skills are only resolvable from GitHub, so anything else is a mistake or an
	// attack. config.IsRemoteURI is deliberately not used here: it also admits
	// rclone connection strings (":local:/" would have the hub copy its own
	// filesystem) and bare http:// URLs (an SSRF vector against hub-internal
	// hosts). Neither can ever yield a usable skill URI.
	u, parseErr := url.Parse(req.SourceURL)
	if parseErr != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "github.com") {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"sourceUrl must be an https://github.com/... URL", nil)
		return
	}

	// Canonicalize before anything downstream sees the URL:
	//
	//   - Lowercase the host. The gate above is case-insensitive (hostnames are),
	//     but config.DetectRemoteType matches "github.com" exactly, so a
	//     "https://GitHub.com/..." URL would otherwise sail past this check and
	//     die later with a generic fetch error.
	//   - Split off a ?token=SECRET_NAME suffix. A skill URI may name the secret
	//     used to resolve it, but discoverResourceDirs builds child URLs by plain
	//     concatenation ("<sourceURL>/<child>"), which would bury the query
	//     mid-path and make every child unnormalizable. It is re-attached per
	//     skill below. It is also irrelevant to the fetch itself, which
	//     authenticates from project credentials, so the fetch gets the bare URL.
	//   - Drop any #fragment, for the same concatenation reason: ".../skills#notes"
	//     would yield child URLs like ".../skills#notes/alpha-skill".
	//   - Drop any userinfo. "https://x-access-token:SECRET@github.com/..." parses
	//     with Host == "github.com", and base is both logged and echoed in the
	//     "no skills found at <base>" message — so leaving it in would write a
	//     pasted credential to the hub log and hand it back to the client.
	//     Discovery authenticates from project credentials, never from the URL.
	tokenSuffix := ""
	if u.RawQuery != "" {
		tokenSuffix = "?" + u.RawQuery
	}
	u.Host = "github.com"
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	base := u.String()

	// api.NormalizeSkillURI requires the /tree/<ref>/ path form. A bare-repo URL
	// ("https://github.com/acme/repo") would pass the host check, trigger a full
	// tarball fetch, and then fail normalization for every child — surfacing as a
	// misleading "no skills found" for a repo that plainly has skills. Reject it
	// up front with an actionable message instead.
	if !strings.Contains(u.Path, "/tree/") {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"sourceUrl must point to a directory in a GitHub repository "+
				"(e.g. https://github.com/org/repo/tree/main/skills)", nil)
		return
	}

	cachePath, err := s.fetchRemoteForImport(ctx, req.ProjectID, base)
	if err != nil {
		// Never echo err: the sparse-checkout fallback shells out to git with a
		// "https://x-access-token:<TOKEN>@github.com/..." remote, and git's stderr
		// embeds that URL verbatim on failure.
		s.resourceLog.Warn("skill discovery fetch failed",
			"sourceURL", base, "projectID", req.ProjectID, "error", err)
		writeError(w, http.StatusBadRequest, ErrCodeDiscoverFailed,
			"Failed to fetch remote skills; check the URL and repository access", nil)
		return
	}
	defer func() { _ = os.RemoveAll(cachePath) }()

	dirs, skippedDirs, err := discoverResourceDirs(cachePath, base, skillDiscoverKind)
	if err != nil {
		s.resourceLog.Warn("skill discovery scan failed", "sourceURL", base, "error", err)
		writeError(w, http.StatusBadRequest, ErrCodeDiscoverFailed,
			"Failed to scan skills at the given URL", nil)
		return
	}

	// Directories dropped inside the loop below. They are merged into the
	// response's Skipped list so a user whose folder vanished from the dialog
	// gets an explanation rather than a silent omission.
	var droppedNames []string

	skills := make([]DiscoveredSkill, 0, len(dirs))
	for _, d := range dirs {
		// Reject names that would inject URI syntax. A repo directory literally
		// named "helper?token=PROD_SECRET" would otherwise smuggle a token
		// parameter into the skill URI we hand back to the client.
		if strings.ContainsAny(d.name, "?#&=") || strings.Contains(d.name, "..") {
			s.resourceLog.Warn("skipping discovered skill with unsafe directory name", "name", d.name)
			droppedNames = append(droppedNames, d.name)
			continue
		}
		uri, normErr := api.NormalizeSkillURI(d.sourceURL + tokenSuffix)
		if normErr != nil {
			// Defence in depth. The /tree/ check above means a well-formed GitHub
			// URL should always normalize, so this branch is not reachable from
			// validated input — but a directory whose URL cannot be expressed as a
			// skill URI is not addressable and so cannot be added. Drop it rather
			// than failing the whole discovery.
			s.resourceLog.Warn("skipping discovered skill with unnormalizable URI",
				"sourceURL", d.sourceURL, "error", normErr)
			droppedNames = append(droppedNames, d.name)
			continue
		}
		skills = append(skills, DiscoveredSkill{URI: uri, Name: d.name})
	}

	if len(skills) == 0 {
		writeError(w, http.StatusBadRequest, ErrCodeDiscoverFailed,
			"no skills found at "+base, nil)
		return
	}

	skipped := make([]string, 0, len(skippedDirs)+len(droppedNames))
	for _, sd := range skippedDirs {
		skipped = append(skipped, sd.name)
	}
	skipped = append(skipped, droppedNames...)

	writeJSON(w, http.StatusOK, DiscoverSkillsResponse{
		Skills:  skills,
		Skipped: skipped,
		Count:   len(skills),
	})
}

// authorizeSkillDiscover authorizes the caller and, for agents, forces the
// request onto the agent's own project. It writes the error response and
// returns false when the caller is rejected.
//
// Discovery persists nothing, but it does *spend* a project's GitHub
// credentials: fetchRemoteForImport mints a GitHub App installation token or
// reads the project's GITHUB_TOKEN secret. Supplying a projectId therefore has
// to be authorized exactly like any other use of that project's credentials,
// which is why the user branch runs the same CheckAccess as
// authorizeProjectImport rather than settling for "is logged in".
func (s *Server) authorizeSkillDiscover(ctx context.Context, w http.ResponseWriter, req *DiscoverSkillsRequest) bool {
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if !agentIdent.HasScope(ScopeAgentCreate) {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope: project:agent:create", nil)
			return false
		}
		if req.ProjectID != "" && req.ProjectID != agentIdent.ProjectID() {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only discover skills within their own project", nil)
			return false
		}
		// Scope the fetch to the agent's own project even when the caller
		// omitted projectId, so project GitHub credentials still apply.
		req.ProjectID = agentIdent.ProjectID()
		return true
	}

	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "Authentication required", nil)
		return false
	}

	// No projectId means an unauthenticated fetch of a public repo — any
	// logged-in user may do that.
	if req.ProjectID == "" {
		return true
	}

	decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
		Type:       "agent",
		ParentType: "project",
		ParentID:   req.ProjectID,
	}, ActionCreate)
	if !decision.Allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"You don't have permission to discover skills in this project", nil)
		return false
	}

	// Confirm the project exists so a typo'd UUID fails loudly rather than
	// silently degrading to an unauthenticated fetch.
	if _, perr := s.store.GetProject(ctx, req.ProjectID); perr != nil {
		if errors.Is(perr, store.ErrNotFound) {
			NotFound(w, "Project")
			return false
		}
		writeErrorFromErr(w, perr, "")
		return false
	}
	return true
}
