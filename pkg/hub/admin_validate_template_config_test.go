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

//go:build !no_sqlite && (!hubshard || hubshard_5)

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Tests for template environment keys on the hub (ptone/scion#4524).

const envKeysTemplateYAML = "default_harness_config: claude\nmodel: opus\nimage: tpl-image:1\ntelemetry:\n  enabled: true\nvolumes:\n  - source: /a\n    target: /b\nenv:\n  SCION_HUB_URL: http://x\n  MY_FLAG: \"1\"\n"

var wantEnvKeysTemplateKeys = []string{"env.SCION_HUB_URL", "image", "telemetry", "volumes"}

// A committed template that sets environment keys is stored as is (the
// AgentConfig snapshot stays a faithful parse) and the commit logs a WARN
// listing the keys.
func TestTemplateCommit_EnvironmentKeysWarnAndSnapshotFaithful(t *testing.T) {
	srv, _ := newCommitTestServer(t, newCommitTestStorage(t))
	logs := captureSlogDefault(t)

	tmpl := seedCommittedTemplate(t, srv, "env-keys-tpl", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": envKeysTemplateYAML})

	cfg := tmpl.AgentConfig
	if cfg == nil {
		t.Fatal("AgentConfig is nil")
	}
	if cfg.Image != "tpl-image:1" || cfg.Telemetry == nil || len(cfg.Volumes) != 1 || cfg.Env["SCION_HUB_URL"] != "http://x" {
		t.Errorf("AgentConfig snapshot is not a faithful parse: %+v", cfg)
	}
	out := logs.String()
	if !strings.Contains(out, "template sets environment keys") || !strings.Contains(out, "env-keys-tpl") {
		t.Fatalf("no environment-keys WARN in commit logs:\n%s", out)
	}
	for _, k := range wantEnvKeysTemplateKeys {
		if !strings.Contains(out, k) {
			t.Errorf("commit WARN does not list %s:\n%s", k, out)
		}
	}
}

// A template without environment keys commits without the WARN.
func TestTemplateCommit_NoEnvironmentKeysNoWarn(t *testing.T) {
	srv, _ := newCommitTestServer(t, newCommitTestStorage(t))
	logs := captureSlogDefault(t)
	seedCommittedTemplate(t, srv, "job-only-tpl", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgBoth})
	if strings.Contains(logs.String(), "template sets environment keys") {
		t.Errorf("unexpected environment-keys WARN:\n%s", logs.String())
	}
}

// The validate report lists the template and its keys, in every scope, and
// not a template that sets none.
func TestAdminValidateResources_ListsTemplateEnvironmentKeys(t *testing.T) {
	srv, s := newCommitTestServer(t, newCommitTestStorage(t))
	project := &store.Project{
		ID: api.NewUUID(), Name: "Env Keys Project", Slug: "env-keys-project",
		OwnerID: DevUserID, CreatedBy: DevUserID,
	}
	if err := s.CreateProject(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	global := seedCommittedTemplate(t, srv, "env-keys-tpl", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": envKeysTemplateYAML})
	proj := seedCommittedTemplate(t, srv, "proj-env-tpl", store.TemplateScopeProject, project.ID, map[string]string{"scion-agent.yaml": "image: proj:1\n"})
	seedCommittedTemplate(t, srv, "job-only-tpl", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgBoth})

	w := doTemplateRequest(t, srv, http.MethodGet, "/api/v1/admin/validate-resources", "", nil)
	mustStatus(t, w, http.StatusOK)
	var resp struct {
		TemplateConfigFindings []TemplateConfigFinding `json:"templateConfigFindings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	byName := map[string]TemplateConfigFinding{}
	for _, f := range resp.TemplateConfigFindings {
		byName[f.Template] = f
	}
	if len(byName) != 2 {
		t.Fatalf("findings = %+v, want exactly env-keys-tpl and proj-env-tpl", resp.TemplateConfigFindings)
	}
	g := byName["env-keys-tpl"]
	if g.Kind != TemplateFindingEnvironmentKeys || g.TemplateID != global.ID || g.Scope != store.TemplateScopeGlobal {
		t.Errorf("global finding = %+v", g)
	}
	if !reflect.DeepEqual(g.Keys, wantEnvKeysTemplateKeys) {
		t.Errorf("global finding keys = %v, want %v", g.Keys, wantEnvKeysTemplateKeys)
	}
	wantMsg := "template env-keys-tpl sets environment key(s) env.SCION_HUB_URL, image, telemetry, volumes; ignored (environment comes from settings)"
	if g.Message != wantMsg {
		t.Errorf("message = %q, want %q", g.Message, wantMsg)
	}
	p := byName["proj-env-tpl"]
	if p.TemplateID != proj.ID || p.Scope != store.TemplateScopeProject || p.ScopeID != project.ID || !reflect.DeepEqual(p.Keys, []string{"image"}) {
		t.Errorf("project finding = %+v", p)
	}
}
