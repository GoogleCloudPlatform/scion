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
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Template config findings: the "templateConfigFindings" section of
// GET /api/v1/admin/validate-resources. Each finding names a template whose
// configuration holds values agents ignore. The section is shared by every
// such check; each check has its own Kind:
//   - environment_keys (ptone/scion#4524): the template's AgentConfig
//     snapshot sets environment keys (see config.StripTemplateEnvironmentKeys).
//
// Findings are reported only; nothing is rewritten.

// Template config finding kinds.
const (
	TemplateFindingEnvironmentKeys = "environment_keys"
)

// templateFindingsPageSize is the page size of the template scan.
const templateFindingsPageSize = 500

// TemplateConfigFinding is one template config finding.
type TemplateConfigFinding struct {
	Kind       string   `json:"kind"`
	Template   string   `json:"template"`
	TemplateID string   `json:"templateId"`
	Scope      string   `json:"scope"`
	ScopeID    string   `json:"scopeId,omitempty"`
	Keys       []string `json:"keys"`
	Message    string   `json:"message"`
}

// templateEnvironmentKeyFinding returns the environment_keys finding for t,
// or nil when its snapshot sets none. The harness-config's credential env
// keys are not known to the hub here, so only the fixed reserved env keys
// (SCION_*, OTEL_*, hub-only) are checked; agents also ignore the
// credential keys of the harness-config they resolve.
func templateEnvironmentKeyFinding(t *store.Template) *TemplateConfigFinding {
	keys := config.TemplateEnvironmentKeys(t.AgentConfig, nil)
	if len(keys) == 0 {
		return nil
	}
	return &TemplateConfigFinding{
		Kind:       TemplateFindingEnvironmentKeys,
		Template:   t.Name,
		TemplateID: t.ID,
		Scope:      t.Scope,
		ScopeID:    t.ScopeID,
		Keys:       keys,
		Message: fmt.Sprintf("template %s sets environment key(s) %s; ignored (environment comes from settings)",
			t.Name, strings.Join(keys, ", ")),
	}
}

// templateConfigFindings scans every template, in every scope, and returns
// its config findings.
func (s *Server) templateConfigFindings(ctx context.Context) ([]TemplateConfigFinding, error) {
	findings := []TemplateConfigFinding{}
	opts := store.ListOptions{Limit: templateFindingsPageSize, SkipTotalCount: true}
	for {
		page, err := s.store.ListTemplates(ctx, store.TemplateFilter{}, opts)
		if err != nil {
			return nil, err
		}
		for i := range page.Items {
			if f := templateEnvironmentKeyFinding(&page.Items[i]); f != nil {
				findings = append(findings, *f)
			}
		}
		if page.NextCursor == "" || page.NextCursor == opts.Cursor {
			return findings, nil
		}
		opts.Cursor = page.NextCursor
	}
}
