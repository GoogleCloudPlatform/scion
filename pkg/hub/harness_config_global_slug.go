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

// ErrCodeHarnessConfigSlugReserved answers a hosted-hub request that would
// give a project-scope harness config the slug of a global one (409).
const ErrCodeHarnessConfigSlugReserved = "harness_config_slug_reserved"

// harnessConfigSlugReservedError refuses a project-scope harness config
// whose slug is taken by a global harness config on a hosted hub. A hosted
// hub resolves global scope first (harnessConfigLookupScopes), but a
// same-slug project row would still be ambiguous with the global one, would
// replace it wherever a workstation-order lookup or a broker-side resolution
// applies, and would shadow it if the global row were later deleted, so the
// slug is refused at write time as well.
type harnessConfigSlugReservedError struct {
	slug string
}

func (e *harnessConfigSlugReservedError) Error() string {
	return fmt.Sprintf("harness config slug %q is reserved by a global harness config; choose a different name", e.slug)
}

// checkHarnessConfigSlugNotGlobal returns a *harnessConfigSlugReservedError
// when, on a hosted hub, a harness config in scope with this slug would take
// the slug of a global harness config: a hub global harness config, a
// built-in harness config name (reserved even after its global row was
// deleted, since brokers still resolve it), or an entry of the global
// settings harness_configs. It returns nil on a
// workstation hub and for every scope other than project. A store failure
// other than not-found is returned as is, so callers fail closed.
func (s *Server) checkHarnessConfigSlugNotGlobal(ctx context.Context, scope, slug string) error {
	if s.workstation || scope != store.HarnessConfigScopeProject || slug == "" {
		return nil
	}
	if isBuiltinName(storage.ResourceKindHarnessConfig, slug) || s.settingsHarnessConfigSlugTaken(slug) {
		return &harnessConfigSlugReservedError{slug: slug}
	}
	hc, err := s.store.GetHarnessConfigBySlug(ctx, slug, store.HarnessConfigScopeGlobal, "")
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if hc != nil {
		return &harnessConfigSlugReservedError{slug: slug}
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
// row stored before checkHarnessConfigSlugNotGlobal existed, nor one whose
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

// writeHarnessConfigSlugReserved writes the 409 for a
// *harnessConfigSlugReservedError in err and reports whether it did.
func writeHarnessConfigSlugReserved(w http.ResponseWriter, err error) bool {
	var reserved *harnessConfigSlugReservedError
	if !errors.As(err, &reserved) {
		return false
	}
	writeError(w, http.StatusConflict, ErrCodeHarnessConfigSlugReserved, reserved.Error(),
		map[string]interface{}{"field": "slug"})
	return true
}

// refuseGlobalHarnessConfigSlug runs checkHarnessConfigSlugNotGlobal and
// writes the refusal (409) or the store failure. It reports whether a
// response was written.
func (s *Server) refuseGlobalHarnessConfigSlug(ctx context.Context, w http.ResponseWriter, scope, slug string) bool {
	err := s.checkHarnessConfigSlugNotGlobal(ctx, scope, slug)
	if err == nil {
		return false
	}
	if !writeHarnessConfigSlugReserved(w, err) {
		writeErrorFromErr(w, err, "")
	}
	return true
}
