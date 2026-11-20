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
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Template config findings: the "templateConfigFindings" section of
// GET /api/v1/admin/validate-resources. Each finding names a template whose
// configuration has an issue. The section is shared by every such check; each
// check has its own Kind:
//   - environment_keys (ptone/scion#4524): the template's AgentConfig
//     snapshot sets environment keys (see config.StripTemplateEnvironmentKeys).
//   - agent_schema (ptone/scion#7579): the template's effective agent config
//     file fails agent v1 schema validation or fails to parse.
//
// Findings are reported only; nothing is rewritten.

// Template config finding kinds.
const (
	TemplateFindingEnvironmentKeys = "environment_keys"
	TemplateFindingAgentSchema     = "agent_schema"
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
// or nil when its snapshot sets none: the six environment keys and any
// reserved SCION_* env key. Other env keys are a lowest-precedence fill for
// the job's tools, not findings.
func templateEnvironmentKeyFinding(t *store.Template) *TemplateConfigFinding {
	keys := config.TemplateEnvironmentKeys(t.AgentConfig)
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
		Message:    config.TemplateEnvironmentKeysWarning(t.Name, keys),
	}
}

// templateAgentSchemaFinding validates t's effective agent config file from
// storage against the agent v1 schema and returns an agent_schema finding when
// it fails validation or fails to parse. Returns nil when valid, absent, or
// when storage cannot be read (storage consistency checks report missing files).
func (s *Server) templateAgentSchemaFinding(ctx context.Context, t *store.Template) *TemplateConfigFinding {
	stor := s.GetStorage()
	if stor == nil || !templateHasAgentConfigFile(t) {
		return nil
	}
	read := func(ctx context.Context, p string) ([]byte, error) {
		f, ok := templateFileByPath(t.Files, p)
		if !ok {
			return nil, storage.ErrNotFound
		}
		return readDispatchTemplateFile(ctx, stor, templateObjectPath(t, f))
	}
	data, name, err := readTemplateAgentConfig(ctx, read, t.Files)
	if err != nil || name == "" {
		return nil
	}
	findings, err := config.ValidateTemplateAgentConfig(name, data)
	if err != nil {
		return &TemplateConfigFinding{
			Kind:       TemplateFindingAgentSchema,
			Template:   t.Name,
			TemplateID: t.ID,
			Scope:      t.Scope,
			ScopeID:    t.ScopeID,
			Keys:       []string{"(document)"},
			Message:    fmt.Sprintf("template %s: agent config %s is invalid: %s", t.Name, name, err.Error()),
		}
	}
	if len(findings) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(findings))
	keys := make([]string, 0, len(findings))
	for _, f := range findings {
		k := f.Path
		if k == "" {
			k = "(document)"
		}
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return &TemplateConfigFinding{
		Kind:       TemplateFindingAgentSchema,
		Template:   t.Name,
		TemplateID: t.ID,
		Scope:      t.Scope,
		ScopeID:    t.ScopeID,
		Keys:       keys,
		Message:    config.FormatAgentConfigValidationError(t.Name, name, findings),
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
			if f := s.templateAgentSchemaFinding(ctx, &page.Items[i]); f != nil {
				findings = append(findings, *f)
			}
		}
		if page.NextCursor == "" || page.NextCursor == opts.Cursor {
			return findings, nil
		}
		opts.Cursor = page.NextCursor
	}
}
