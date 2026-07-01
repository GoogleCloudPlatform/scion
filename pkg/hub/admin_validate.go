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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// handleAdminValidateResources validates storage consistency for all global
// resources (templates and harness-configs). Requires admin role.
func (s *Server) handleAdminValidateResources(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()
	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage backend is not configured")
		return
	}

	var reports []ValidationReport

	templates, err := s.store.ListTemplates(ctx, store.TemplateFilter{
		Scope: "global",
	}, store.ListOptions{Limit: 1000})
	if err != nil {
		RuntimeError(w, "failed to list templates: "+err.Error())
		return
	}

	for i := range templates.Items {
		t := &templates.Items[i]
		rec := templateToRecord(t)
		rs := s.templateStore()
		report, err := rs.ValidateStorage(ctx, rec)
		if err != nil {
			s.templateLog.Warn("admin validate: template validation error",
				"name", t.Name, "error", err)
			continue
		}
		reports = append(reports, *report)
	}

	configs, err := s.store.ListHarnessConfigs(ctx, store.HarnessConfigFilter{
		Scope: "global",
	}, store.ListOptions{Limit: 1000})
	if err != nil {
		RuntimeError(w, "failed to list harness-configs: "+err.Error())
		return
	}

	for i := range configs.Items {
		hc := &configs.Items[i]
		rec := harnessConfigToRecord(hc)
		rs := s.harnessConfigStore(hc.Harness)
		report, err := rs.ValidateStorage(ctx, rec)
		if err != nil {
			s.templateLog.Warn("admin validate: harness-config validation error",
				"name", hc.Name, "error", err)
			continue
		}
		reports = append(reports, *report)
	}

	var passed, failed int
	for _, r := range reports {
		if len(r.Issues) == 0 {
			passed++
		} else {
			failed++
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"reports": reports,
		"summary": map[string]int{
			"total":  len(reports),
			"passed": passed,
			"failed": failed,
		},
	})
}
