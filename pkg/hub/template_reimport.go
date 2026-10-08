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
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ReimportTemplateRequest is the optional request body for the template
// reimport endpoint. When SourceURL is set it replaces the stored source URL
// for this reimport and, once the import succeeds, is stored as the
// template's new source URL.
type ReimportTemplateRequest struct {
	SourceURL string `json:"sourceUrl,omitempty"`
}

// reimportSourceError describes why a source URL cannot be used for reimport.
type reimportSourceError struct {
	code    string
	message string
}

// resolveTemplateReimportSource picks the URL a template reimport fetches
// from and validates it with parseTemplateGitHubSource: an https URL for a
// folder on github.com, with no username or password. The stored URL is
// validated on every reimport, and an override is validated the same way
// before it is used or stored; an override may omit the scheme (it is
// normalized the same way import normalizes user input). Built-in templates
// (builtin://...) and templates with no stored source are refused. Error
// messages are fixed text and never echo the URL.
func resolveTemplateReimportSource(stored, override string) (*templateGitHubSource, string, *reimportSourceError) {
	raw := strings.TrimSpace(override)
	if raw != "" {
		// Refuse an explicit non-https scheme before normalization, which
		// would otherwise prepend "https://" to strings like "builtin://x".
		if i := strings.Index(raw, "://"); i > 0 {
			scheme := strings.ToLower(raw[:i])
			if scheme != "https" && scheme != "git+https" {
				return nil, "", &reimportSourceError{"unsupported_source", "Source URL must use https"}
			}
		}
		raw = config.NormalizeTemplateSourceURL(raw)
	} else {
		raw = strings.TrimSpace(stored)
		if raw == "" {
			return nil, "", &reimportSourceError{"no_source_url",
				"This template has no source URL to refresh from. Use the sourceUrl field to specify one."}
		}
	}

	src, err := parseTemplateGitHubSource(raw)
	if err != nil {
		return nil, "", &reimportSourceError{"unsupported_source", err.Error()}
	}
	return src, raw, nil
}

// handleTemplateReimport refreshes a template from its stored source URL (or
// an override URL), replacing its files with the current upstream content.
// POST /api/v1/templates/{id}/reimport
//
// Authorization mirrors template import: the caller must be able to read the
// template (an unreadable template reads as not found) and to create
// templates in the template's owning scope.
func (s *Server) handleTemplateReimport(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx := r.Context()

	tmpl, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		writeStoreErr(w, err, "Template")
		return
	}
	if !s.authorizeRead(w, r, templateResource(tmpl), "Template") {
		return
	}

	// The body is optional: an empty body means "use the stored source".
	var req ReimportTemplateRequest
	if r.Body != nil && r.Body != http.NoBody {
		if err := readJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
			BadRequest(w, "Invalid request body: "+err.Error())
			return
		}
	}

	// Authorize: same as import — template create on the owning scope.
	switch tmpl.Scope {
	case store.TemplateScopeGlobal:
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
		decision := s.authzService.CheckAccess(ctx, userIdent, templateScopeResource(store.TemplateScopeGlobal, ""), ActionCreate)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to reimport global resources", nil)
			return
		}
	case store.TemplateScopeProject:
		if !s.authorizeProjectImport(ctx, w, tmpl.ScopeID, "templates", "template") {
			return
		}
	case store.TemplateScopeUser:
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
		if tmpl.OwnerID != userIdent.ID() {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to reimport another user's template", nil)
			return
		}
	default:
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Reimport is not supported for this resource scope", nil)
		return
	}

	src, sourceURL, srcErr := resolveTemplateReimportSource(tmpl.SourceURL, req.SourceURL)
	if srcErr != nil {
		writeError(w, http.StatusBadRequest, srcErr.code, srcErr.message, nil)
		return
	}

	if s.GetStorage() == nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "Storage is not configured", nil)
		return
	}

	// Only this template is refreshed, even when the source holds several.
	// Import matches existing records by the slug of the discovered folder
	// name, so accept a folder named after either the template's name or slug.
	names := []string{tmpl.Name}
	if tmpl.Slug != "" && tmpl.Slug != tmpl.Name {
		names = append(names, tmpl.Slug)
	}
	run := func(progress importProgressFunc) ([]string, error) {
		return s.reimportTemplateSource(ctx, src, sourceURL, tmpl.Scope, tmpl.ScopeID, names, progress)
	}

	if importAcceptsNDJSON(r) {
		s.streamImport(w, run)
		return
	}

	var failures []ImportFailure
	imported, err := run(failureCollector(&failures))
	if err != nil {
		writeError(w, http.StatusBadRequest, "reimport_failed", err.Error(), nil)
		return
	}

	if len(imported) == 0 && len(failures) > 0 {
		reasons := make([]string, len(failures))
		for i, f := range failures {
			reasons[i] = f.Name + ": " + f.Reason
		}
		writeError(w, http.StatusBadRequest, "reimport_failed", strings.Join(reasons, "; "), nil)
		return
	}

	writeJSON(w, http.StatusOK, ImportTemplatesResponse{
		Templates: imported,
		Count:     len(imported),
		Failed:    failures,
	})
}
