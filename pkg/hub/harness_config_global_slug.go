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
	"fmt"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ErrCodeGlobalNameReserved answers a hosted-hub request that would give a
// project-scope template or harness config the slug or name of a global one
// (409).
const ErrCodeGlobalNameReserved = "global_name_reserved"

// errGlobalNameReserved is the sentinel wrapped by globalNameReservedError so
// callers (such as project clone and tests) can match it with errors.Is.
var errGlobalNameReserved = errors.New("slug or name is reserved by a global resource")

// globalNameReservedError refuses a project-scope template or harness config
// whose slug or name (as written or slugified) is taken by a global resource
// on a hosted hub.
//
// Templates resolve project-first on both the Hub and the CLI (matching the
// CLI's lookup and explicit project:/global: prefixes), and the CLI matches
// templates and harness configs by name as well as slug. Project members can
// author project-scope templates and harness configs whose contents run inside
// agents started by other project members. Refusing global names and slugs
// (including built-in names whose global rows were deleted, and global
// settings harness_configs) at write time prevents shadowing for templates
// and keeps harness-config names unambiguous even if a global row is later
// deleted. User-scope records (resolved only for their own user) and
// workstation hubs (single user) are not checked.
type globalNameReservedError struct {
	Kind  string // "template" or "harness config"
	Field string // "slug" or "name"
	Value string
}

func (e *globalNameReservedError) Error() string {
	return fmt.Sprintf("%s %s %q is reserved by a global %s; choose a different name", e.Kind, e.Field, e.Value, e.Kind)
}

func (e *globalNameReservedError) Unwrap() error { return errGlobalNameReserved }

// shadowCandidate is one value a record could be found by.
type shadowCandidate struct {
	field, value string
}

// shadowCandidates returns the values a record with name and slug could be
// found by: each as stored (a PATCH stores a caller's slug as given) and
// slugified, slug first, without empty or repeated values.
func shadowCandidates(name, slug string) []shadowCandidate {
	var out []shadowCandidate
	seen := map[string]bool{}
	add := func(field, v string) {
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, shadowCandidate{field, v})
	}
	add("slug", slug)
	add("slug", api.Slugify(slug))
	add("name", name)
	add("name", api.Slugify(name))
	return out
}

// checkTemplateNotGlobal returns a *globalNameReservedError when, on a hosted
// hub, a template in scope with this name or slug would shadow a global
// template: a hub global template (matched by name or slug) or a built-in
// template name (reserved even after its global row was deleted). It returns
// nil on a workstation hub and for every scope other than project. A store
// failure is returned as is, so callers fail closed.
func (s *Server) checkTemplateNotGlobal(ctx context.Context, scope, name, slug string) error {
	if s.workstation || scope != store.TemplateScopeProject {
		return nil
	}
	for _, c := range shadowCandidates(name, slug) {
		if isBuiltinName(storage.ResourceKindTemplate, c.value) {
			return &globalNameReservedError{Kind: "template", Field: c.field, Value: c.value}
		}
		found, err := s.store.ListTemplates(ctx, store.TemplateFilter{Name: c.value, Scope: store.TemplateScopeGlobal},
			store.ListOptions{Limit: 1, SkipTotalCount: true})
		if err != nil {
			return err
		}
		if len(found.Items) > 0 {
			return &globalNameReservedError{Kind: "template", Field: c.field, Value: c.value}
		}
	}
	return nil
}

// checkHarnessConfigNotGlobal returns a *globalNameReservedError when, on a
// hosted hub, a harness config in scope with this name or slug would take the
// slug or name of a global harness config: a hub global harness config
// (matched by name or slug), a built-in harness config name (reserved even
// after its global row was deleted, since brokers still resolve it), or an
// entry of the global settings harness_configs. It returns nil on a
// workstation hub and for every scope other than project. A store failure is
// returned as is, so callers fail closed.
func (s *Server) checkHarnessConfigNotGlobal(ctx context.Context, scope, name, slug string) error {
	if s.workstation || scope != store.HarnessConfigScopeProject {
		return nil
	}
	for _, c := range shadowCandidates(name, slug) {
		if isBuiltinName(storage.ResourceKindHarnessConfig, c.value) || s.settingsHarnessConfigSlugTaken(c.value) {
			return &globalNameReservedError{Kind: "harness config", Field: c.field, Value: c.value}
		}
		found, err := s.store.ListHarnessConfigs(ctx, store.HarnessConfigFilter{Name: c.value, Scope: store.HarnessConfigScopeGlobal},
			store.ListOptions{Limit: 1, SkipTotalCount: true})
		if err != nil {
			return err
		}
		if len(found.Items) > 0 {
			return &globalNameReservedError{Kind: "harness config", Field: c.field, Value: c.value}
		}
	}
	return nil
}

// harnessConfigScope names one scope a harness-config slug is looked up in.
type harnessConfigScope struct {
	scope   string
	scopeID string
}

// harnessConfigLookupScopes returns the scopes, in order, a harness-config
// name resolves in for an agent of projectID. A workstation hub looks in the
// project scope first, then globally. A hosted hub looks globally first, so a
// project-scope row can never replace a global one of the same slug: not a
// row stored before checkHarnessConfigNotGlobal existed, nor one whose
// slug a global config (or a newly seeded built-in) took later. The project
// scope is omitted when projectID is empty.
func (s *Server) harnessConfigLookupScopes(projectID string) []harnessConfigScope {
	global := harnessConfigScope{scope: store.HarnessConfigScopeGlobal}
	if projectID == "" {
		return []harnessConfigScope{global}
	}
	project := harnessConfigScope{scope: store.HarnessConfigScopeProject, scopeID: projectID}
	if s.workstation {
		return []harnessConfigScope{project, global}
	}
	return []harnessConfigScope{global, project}
}

// resolveHarnessConfigBySlugWith walks harnessConfigLookupScopes(projectID) in
// order (project then global on a workstation hub, global then project on a
// hosted hub), invoking lookup for each scope and returning the first non-nil
// config found. It returns (nil, nil) when s is nil, name is empty, or no
// scope has a matching config. A store failure other than store.ErrNotFound is
// logged; on a hosted hub a failed global lookup returns immediately without
// consulting the project scope, while on a workstation hub the remaining scope
// is still tried and the failure is returned with a nil config if none matches.
func (s *Server) resolveHarnessConfigBySlugWith(
	projectID, name string,
	lookup func(slug, scope, scopeID string) (*store.HarnessConfig, error),
) (*store.HarnessConfig, error) {
	if s == nil || name == "" {
		return nil, nil
	}
	var lookupErr error
	for _, sc := range s.harnessConfigLookupScopes(projectID) {
		hc, err := lookup(name, sc.scope, sc.scopeID)
		if err == nil && hc != nil {
			return hc, nil
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			if s.agentLifecycleLog != nil {
				s.agentLifecycleLog.Warn("failed to get "+sc.scope+" harness config by slug",
					"slug", name, "scope_id", sc.scopeID, "error", err)
			}
			if !s.workstation && sc.scope == store.HarnessConfigScopeGlobal {
				return nil, err
			}
			lookupErr = err
		}
	}
	return nil, lookupErr
}

// settingsHarnessConfigSlugTaken reports whether the global settings
// harness_configs has an entry whose name, or its slug form, is slug.
func (s *Server) settingsHarnessConfigSlugTaken(slug string) bool {
	ops := s.GetOperationalSettings()
	if ops == nil {
		return false
	}
	for name := range ops.Snapshot().HarnessConfigs {
		if name == slug || api.Slugify(name) == slug {
			return true
		}
	}
	return false
}

// writeGlobalNameReserved writes the 409 for a *globalNameReservedError in err
// and reports whether it did.
func writeGlobalNameReserved(w http.ResponseWriter, err error) bool {
	var reserved *globalNameReservedError
	if !errors.As(err, &reserved) {
		return false
	}
	writeError(w, http.StatusConflict, ErrCodeGlobalNameReserved, reserved.Error(),
		map[string]interface{}{"field": reserved.Field, "reason": "shadows_global"})
	return true
}

// refuseGlobalTemplateName runs checkTemplateNotGlobal and writes the refusal
// (409) or the store failure. It reports whether a response was written.
func (s *Server) refuseGlobalTemplateName(ctx context.Context, w http.ResponseWriter, scope, name, slug string) bool {
	err := s.checkTemplateNotGlobal(ctx, scope, name, slug)
	if err == nil {
		return false
	}
	if !writeGlobalNameReserved(w, err) {
		writeErrorFromErr(w, err, "")
	}
	return true
}

// refuseGlobalHarnessConfigName runs checkHarnessConfigNotGlobal and writes
// the refusal (409) or the store failure. It reports whether a response was
// written.
func (s *Server) refuseGlobalHarnessConfigName(ctx context.Context, w http.ResponseWriter, scope, name, slug string) bool {
	err := s.checkHarnessConfigNotGlobal(ctx, scope, name, slug)
	if err == nil {
		return false
	}
	if !writeGlobalNameReserved(w, err) {
		writeErrorFromErr(w, err, "")
	}
	return true
}
