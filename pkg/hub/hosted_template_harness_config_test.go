//go:build !no_sqlite

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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hostedguard"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	hostedTplName   = "member-tpl"
	hostedTplHCPath = "harness-configs/hc-tpl/config.yaml"
	// hostedTplHCEnvYAML has an env reference; hostedHCVolYAML (P2) a host
	// bind. Neither value may appear in a refusal.
	hostedTplHCEnvYAML = "harness: claude\nenv:\n  K: ${" + hostedEnvSecret + "}\n"
)

// createHostedTestTemplate stores a template of scope with files, the files'
// content in stor at the legacy path layout.
func createHostedTestTemplate(t *testing.T, s store.Store, stor *contentMockStorage, scope string, files map[string]string) *store.Template {
	t.Helper()
	tmpl := &store.Template{
		ID: tid("tmpl-hosted"), Name: hostedTplName, Slug: hostedTplName, Harness: "claude",
		Scope: scope, Status: store.TemplateStatusActive,
		StoragePath: "templates/" + scope + "/" + hostedTplName, StorageBucket: "test-bucket",
		Updated: time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC),
	}
	if scope == store.TemplateScopeProject {
		tmpl.ScopeID = tid("project-1")
	}
	for p, content := range files {
		obj := tmpl.StoragePath + "/" + p
		stor.content[obj] = []byte(content)
		stor.objects[obj] = &storage.Object{Name: obj, Size: int64(len(content))}
		tmpl.Files = append(tmpl.Files, store.TemplateFile{Path: p, Size: int64(len(content)), Hash: "sha256:placeholder"})
	}
	tmpl.ContentHash = defaultContentHash(tmpl.Files)
	require.NoError(t, s.CreateTemplate(context.Background(), tmpl))
	return tmpl
}

func putTemplateFile(t *testing.T, srv *Server, tmplID, p, content string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"content": content})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/templates/"+tmplID+"/files/"+p, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func assertHostedTemplateRefusal(t *testing.T, rec *httptest.ResponseRecorder, wantParts ...string) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	code, msg := decodeHostedRefusal(t, rec)
	assert.Equal(t, hostedguard.ErrorCode, code)
	assert.Contains(t, msg, `template "`+hostedTplName+`"`)
	for _, p := range wantParts {
		assert.Contains(t, msg, p)
	}
	for _, leak := range []string{"member-secret-src", hostedEnvSecret} {
		assert.NotContains(t, rec.Body.String(), leak, "the refusal never echoes a source or value")
	}
}

// TestTemplateFileWrite_HostedProjectBundledHarnessConfig: on a hosted hub,
// writing a bundled harness config into a project-scope template is refused
// before any object is written when it names a host volume or a host env
// reference, or when its path spells harness-configs or config.yaml in
// another case. A global template, a workstation hub, clean content and a
// path outside harness-configs/ are accepted.
func TestTemplateFileWrite_HostedProjectBundledHarnessConfig(t *testing.T) {
	for name, tc := range map[string]struct {
		path, content string
		want          []string
	}{
		"host volume":        {hostedTplHCPath, hostedHCVolYAML, []string{`volume "/hc-data"`, `harness config "hc-tpl"`}},
		"host env reference": {hostedTplHCPath, hostedTplHCEnvYAML, []string{`env "K"`, `harness config "hc-tpl"`}},
		"folded directory":   {"Harness-Configs/hc-tpl/config.yaml", hostedHCOKYAML, []string{"must be spelled in lower case"}},
		"folded config.yaml": {"harness-configs/hc-tpl/Config.YAML", hostedHCOKYAML, []string{"must be spelled in lower case"}},
	} {
		t.Run(name+" refused", func(t *testing.T) {
			srv, s, stor := testTemplateFileServer(t)
			tmpl := createHostedTestTemplate(t, s, stor, store.TemplateScopeProject, map[string]string{"scion-agent.yaml": "harness: claude\n"})
			objects := len(stor.content)
			rec := putTemplateFile(t, srv, tmpl.ID, tc.path, tc.content)
			assertHostedTemplateRefusal(t, rec, tc.want...)
			assert.Len(t, stor.content, objects, "no object written for refused content")
			got, err := s.GetTemplate(context.Background(), tmpl.ID)
			require.NoError(t, err)
			assert.Equal(t, tmpl.ContentHash, got.ContentHash, "row unchanged")
		})
	}
	for name, tc := range map[string]struct {
		scope       string
		workstation bool
		path        string
		content     string
	}{
		"global template":         {store.TemplateScopeGlobal, false, hostedTplHCPath, hostedHCVolYAML},
		"workstation hub":         {store.TemplateScopeProject, true, hostedTplHCPath, hostedHCVolYAML},
		"clean bundled config":    {store.TemplateScopeProject, false, hostedTplHCPath, hostedHCOKYAML},
		"outside harness-configs": {store.TemplateScopeProject, false, "home/config.yaml", hostedHCVolYAML},
	} {
		t.Run(name+" accepted", func(t *testing.T) {
			srv, s, stor := testTemplateFileServer(t)
			srv.workstation = tc.workstation
			tmpl := createHostedTestTemplate(t, s, stor, tc.scope, map[string]string{"scion-agent.yaml": "harness: claude\n"})
			rec := putTemplateFile(t, srv, tmpl.ID, tc.path, tc.content)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
}

// TestTemplateFileUpload_HostedProjectBundledHarnessConfig: the multipart
// upload refuses the same content before writing.
func TestTemplateFileUpload_HostedProjectBundledHarnessConfig(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)
	tmpl := createHostedTestTemplate(t, s, stor, store.TemplateScopeProject, map[string]string{"scion-agent.yaml": "harness: claude\n"})
	objects := len(stor.content)
	req := templateMultipartRequest(t, tmpl.ID, map[string][]byte{hostedTplHCPath: []byte(hostedHCVolYAML)})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assertHostedTemplateRefusal(t, rec, `volume "/hc-data"`)
	assert.Len(t, stor.content, objects, "no object written for refused content")
}

// TestTemplateCommit_HostedProjectChecksEveryBundledConfig: the commit
// checks every bundled harness config of a project-scope template, not
// only the ones a write introduces, so a template stored before the check
// is refused at its next commit until the bundled config is fixed; the
// same commit is accepted on a workstation hub and for a global template.
func TestTemplateCommit_HostedProjectChecksEveryBundledConfig(t *testing.T) {
	for name, tc := range map[string]struct {
		scope       string
		workstation bool
		refused     bool
	}{
		"hosted project template": {store.TemplateScopeProject, false, true},
		"hosted global template":  {store.TemplateScopeGlobal, false, false},
		"workstation hub":         {store.TemplateScopeProject, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			srv, s, stor := testTemplateFileServer(t)
			srv.workstation = tc.workstation
			tmpl := createHostedTestTemplate(t, s, stor, tc.scope, map[string]string{
				"scion-agent.yaml": "harness: claude\n",
				hostedTplHCPath:    hostedTplHCEnvYAML,
			})
			rec := putTemplateFile(t, srv, tmpl.ID, "README.md", "# readme")
			if !tc.refused {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				return
			}
			assertHostedTemplateRefusal(t, rec, `env "K"`, `harness config "hc-tpl"`)
		})
	}
}

// TestCheckHostedTemplateDirs: import and reimport into project scope on a
// hosted hub are refused as a whole when a template directory bundles a
// refused harness config or a case-folded path, naming every refused
// template in name order; global scope and a workstation hub are not
// checked.
func TestCheckHostedTemplateDirs(t *testing.T) {
	parent := t.TempDir()
	mk := func(name, rel, content string) resourceDir {
		dir := filepath.Join(parent, name)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(rel)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte("harness: claude\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644))
		return resourceDir{name: name, path: dir}
	}
	dirs := []resourceDir{
		mk("zz-env", hostedTplHCPath, hostedTplHCEnvYAML),
		mk("aa-vol", hostedTplHCPath, hostedHCVolYAML),
		mk("mm-folded", "HARNESS-CONFIGS/hc/config.yaml", hostedHCOKYAML),
		mk("ok", hostedTplHCPath, hostedHCOKYAML),
	}
	srv, _ := testServer(t)
	err := srv.checkHostedTemplateDirs(dirs, store.TemplateScopeProject)
	var ie *hostedHarnessConfigImportError
	require.True(t, errors.As(err, &ie), "refused as an import: %v", err)
	require.Len(t, ie.errs, 3)
	assert.Contains(t, ie.errs[0].Error(), `template "aa-vol"`)
	assert.Contains(t, ie.errs[1].Error(), `template "mm-folded"`)
	assert.Contains(t, ie.errs[2].Error(), `template "zz-env"`)
	assert.NotContains(t, err.Error(), hostedEnvSecret)
	assert.NotContains(t, err.Error(), "member-secret-src")

	rec := httptest.NewRecorder()
	assert.True(t, writeImportContentRefusal(rec, err))
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	assert.NoError(t, srv.checkHostedTemplateDirs(dirs, store.TemplateScopeGlobal))
	srv.workstation = true
	assert.NoError(t, srv.checkHostedTemplateDirs(dirs, store.TemplateScopeProject))
}

// hostedTemplateDispatcher is a dispatcher (hosted or not) over srv's store
// with one stored agent that uses tmpl.
func hostedTemplateDispatcher(t *testing.T, srv *Server, s store.Store, tmpl *store.Template, hosted bool) (*HTTPAgentDispatcher, *mockRuntimeBrokerClient, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: tid("tpl-broker"), Name: "tpl-broker", Slug: "tpl-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}))
	project := &store.Project{ID: tid("project-1"), Name: "tpl-project", Slug: "tpl-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("tpl-agent"), Name: "tpl-agent", Slug: "tpl-agent",
		ProjectID: project.ID, RuntimeBrokerID: tid("tpl-broker"),
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude", TemplateID: tmpl.ID, TemplateHash: tmpl.ContentHash,
			InlineConfig: &api.ScionConfig{Harness: "claude"},
		},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	srv.workstation = !hosted
	m := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(s, m, false, slog.Default())
	d.SetHostedMode(hosted)
	d.SetHostedTemplateCheck(srv.checkStoredHostedTemplate)
	return d, m, agent
}

// TestDispatch_HostedTemplateHarnessConfigPrecheck: on a hosted hub every
// dispatch path refuses an agent that uses a project-scope template whose
// stored bundled harness config names host resources (stored before the
// check existed), with no broker call. A global template, a workstation hub
// and a clean project template dispatch.
func TestDispatch_HostedTemplateHarnessConfigPrecheck(t *testing.T) {
	ctx := context.Background()
	for _, op := range precheckOps(ctx) {
		t.Run(op.name+" refuses", func(t *testing.T) {
			srv, s, stor := testTemplateFileServer(t)
			tmpl := createHostedTestTemplate(t, s, stor, store.TemplateScopeProject, map[string]string{hostedTplHCPath: hostedHCVolYAML})
			d, m, a := hostedTemplateDispatcher(t, srv, s, tmpl, true)
			err := op.run(d, a)
			var refusal *RuntimeTargetRefusal
			require.True(t, errors.As(err, &refusal), "refused with a typed refusal: %v", err)
			assert.Equal(t, http.StatusBadRequest, refusal.Status)
			assert.Equal(t, hostedguard.ErrorCode, refusal.Code)
			assert.Contains(t, refusal.Message, `template "`+hostedTplName+`"`)
			assert.Contains(t, refusal.Message, `volume "/hc-data"`)
			assert.NotContains(t, refusal.Message, "member-secret-src")
			assert.False(t, op.called(m), "no broker call")
		})
	}
	t.Run("folded path refuses", func(t *testing.T) {
		srv, s, stor := testTemplateFileServer(t)
		tmpl := createHostedTestTemplate(t, s, stor, store.TemplateScopeProject, map[string]string{"harness-configs/hc/CONFIG.yaml": hostedHCOKYAML})
		d, m, a := hostedTemplateDispatcher(t, srv, s, tmpl, true)
		err := d.DispatchAgentStart(ctx, a, "task", false)
		var refusal *RuntimeTargetRefusal
		require.True(t, errors.As(err, &refusal) && refusal.Code == hostedguard.ErrorCode, "refused: %v", err)
		assert.False(t, m.startCalled, "no broker call")
	})
	for name, tc := range map[string]struct {
		scope   string
		hosted  bool
		content string
	}{
		"global template":        {store.TemplateScopeGlobal, true, hostedHCVolYAML},
		"workstation hub":        {store.TemplateScopeProject, false, hostedHCVolYAML},
		"clean project template": {store.TemplateScopeProject, true, hostedHCOKYAML},
	} {
		t.Run(name+" dispatches", func(t *testing.T) {
			srv, s, stor := testTemplateFileServer(t)
			tmpl := createHostedTestTemplate(t, s, stor, tc.scope, map[string]string{hostedTplHCPath: tc.content})
			d, m, a := hostedTemplateDispatcher(t, srv, s, tmpl, tc.hosted)
			err := d.DispatchAgentStart(ctx, a, "task", false)
			var refusal *RuntimeTargetRefusal
			assert.False(t, errors.As(err, &refusal) && refusal.Code == hostedguard.ErrorCode, "no hosted refusal: %v", err)
			assert.True(t, m.startCalled, "broker called")
		})
	}
}

// TestCreateAuthenticatedDispatcher_WiresHostedTemplateCheck: the production
// dispatcher carries the stored template check.
func TestCreateAuthenticatedDispatcher_WiresHostedTemplateCheck(t *testing.T) {
	srv, _, _ := testTemplateFileServer(t)
	d := srv.CreateAuthenticatedDispatcher()
	require.NotNil(t, d.hostedTemplateCheck)
}
