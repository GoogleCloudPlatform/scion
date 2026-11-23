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

//go:build !no_sqlite

package hub

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hostedguard"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostedHCConfigYAMLVariants are root-level file names a hosted hub
// refuses in a project harness config because a broker's filesystem could
// read them as another name (config.yaml): other letter case, non-ASCII
// (the U+FB01 ligature, a zero-width non-joiner), a trailing dot or space,
// and a short-name alias.
var hostedHCConfigYAMLVariants = []string{
	"CONFIG.YAML", "con\ufb01g.yaml", "CON\ufb01G.YAML", "con\u200cfig.yaml",
	"config.yaml.", "config.yaml ", "CONFIG~1.YAM",
}

const (
	// hostedHCVolYAML is a harness config with one host bind. Its source
	// must never appear in a refusal.
	hostedHCVolYAML = "harness: claude\nvolumes:\n  - source: /srv/member-secret-src\n    target: /hc-data\n"
	hostedHCNFSYAML = "harness: claude\nvolumes:\n  - type: nfs\n    server: 10.9.8.7\n    source: /export\n    target: /hc-data\n"
	hostedHCGCSYAML = "harness: claude\nvolumes:\n  - type: gcs\n    bucket: member-bucket\n    target: /hc-data\n"
	hostedHCOKYAML  = "harness: claude\nimage: scion-claude:test\n"
)

// newHostedTestHC creates a harness config of scope and stores yaml as its
// config.yaml through the commit path, with the check off (content stored
// before the check existed, or by a workstation hub).
func newHostedTestHC(t *testing.T, srv *Server, s store.Store, id, name, scope, scopeID, yaml string) *store.HarnessConfig {
	t.Helper()
	ctx := context.Background()
	hc := &store.HarnessConfig{
		ID: tid(id), Name: name, Slug: name, Harness: "claude",
		Scope: scope, ScopeID: scopeID, Status: store.HarnessConfigStatusActive,
		Layout: store.HarnessConfigLayoutBlobs, StorageBucket: "test-bucket",
	}
	hc.StoragePath = srv.harnessConfigBlobStoragePath(hc)
	require.NoError(t, s.CreateHarnessConfig(ctx, hc))
	if yaml == "" {
		return hc
	}
	data := []byte(yaml)
	hash, blobPath, err := srv.writeHarnessConfigFileBlob(ctx, srv.GetStorage(), hc, data)
	require.NoError(t, err)
	was := srv.workstation
	srv.workstation = true
	defer func() { srv.workstation = was }()
	require.NoError(t, srv.commitHarnessConfigFiles(ctx, hc,
		[]store.TemplateFile{{Path: "config.yaml", Size: int64(len(data)), Hash: hash}},
		harnessConfigCommitOpts{content: map[string][]byte{"config.yaml": data}, written: map[string]bool{blobPath: true}}))
	return hc
}

func assertHostedHCRefusalBody(t *testing.T, rec *httptest.ResponseRecorder, name string) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	code, msg := decodeHostedRefusal(t, rec)
	assert.Equal(t, hostedguard.ErrorCode, code)
	assert.Contains(t, msg, `volume "/hc-data"`)
	assert.Contains(t, msg, `harness config "`+name+`"`)
	for _, leak := range []string{"member-secret-src", "10.9.8.7", "/export"} {
		assert.NotContains(t, rec.Body.String(), leak, "the refusal never echoes a source or server")
	}
}

func putHarnessConfigYAML(t *testing.T, srv *Server, hcID, yaml string) *httptest.ResponseRecorder {
	t.Helper()
	return putHarnessConfigFile(t, srv, hcID, "config.yaml", yaml)
}

func putHarnessConfigFile(t *testing.T, srv *Server, hcID, filePath, content string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"content": "` + strings.ReplaceAll(content, "\n", `\n`) + `"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/harness-configs/"+hcID+"/files/"+url.PathEscape(filePath), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestHarnessConfigFileWrite_HostedProjectScope: on a hosted hub, writing a
// project-scope harness config's config.yaml with a host, nfs or other
// non-gcs volume is refused (400 hosted_config_refused naming the target and
// the harness config) before any object is written, and the row is
// unchanged. A global harness config, a workstation hub and a gcs volume
// (the runtime is not known on the hub) are not refused.
func TestHarnessConfigFileWrite_HostedProjectScope(t *testing.T) {
	ctx := context.Background()
	for name, yaml := range map[string]string{"host bind": hostedHCVolYAML, "nfs": hostedHCNFSYAML} {
		t.Run(name+" refused", func(t *testing.T) {
			srv, s, stor := testHarnessConfigFileServer(t)
			hc := newHostedTestHC(t, srv, s, "hc-member", "member-hc", store.HarnessConfigScopeProject, tid("project-1"), hostedHCOKYAML)
			objects := len(stor.content)
			rec := putHarnessConfigYAML(t, srv, hc.ID, yaml)
			assertHostedHCRefusalBody(t, rec, "member-hc")
			assert.Len(t, stor.content, objects, "no object written for refused content")
			got, err := s.GetHarnessConfig(ctx, hc.ID)
			require.NoError(t, err)
			assert.Equal(t, hc.ContentHash, got.ContentHash, "row unchanged")
		})
	}
	// A root-level file name a broker's filesystem could read as
	// config.yaml is refused on a hosted hub, and accepted on a workstation
	// hub.
	for _, variant := range hostedHCConfigYAMLVariants {
		for _, workstation := range []bool{false, true} {
			t.Run(map[bool]string{false: "hosted", true: "workstation"}[workstation]+" "+variant, func(t *testing.T) {
				srv, s, _ := testHarnessConfigFileServer(t)
				hc := newHostedTestHC(t, srv, s, "hc-case", "case-hc", store.HarnessConfigScopeProject, tid("project-1"), hostedHCOKYAML)
				srv.workstation = workstation
				rec := putHarnessConfigFile(t, srv, hc.ID, variant, hostedHCVolYAML)
				if workstation {
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					return
				}
				require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				code, msg := decodeHostedRefusal(t, rec)
				assert.Equal(t, hostedguard.ErrorCode, code)
				assert.Contains(t, msg, `harness config "case-hc"`)
				assert.Contains(t, msg, "on a hosted server")
				assert.NotContains(t, rec.Body.String(), "member-secret-src")
				got, err := s.GetHarnessConfig(ctx, hc.ID)
				require.NoError(t, err)
				assert.Equal(t, hc.ContentHash, got.ContentHash, "row unchanged")
			})
		}
	}
	// Files in subdirectories, and plain ASCII root files, are not
	// restricted.
	for _, other := range []string{"sub/CONFIG.YAML", "home/con\ufb01g.yaml", "home/n\u00f6tes~.md.", "notes.md", "provision.py"} {
		t.Run("hosted accepts "+other, func(t *testing.T) {
			srv, s, _ := testHarnessConfigFileServer(t)
			hc := newHostedTestHC(t, srv, s, "hc-other", "other-hc", store.HarnessConfigScopeProject, tid("project-1"), hostedHCOKYAML)
			rec := putHarnessConfigFile(t, srv, hc.ID, other, "notes")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
	for name, tc := range map[string]struct {
		scope       string
		workstation bool
		yaml        string
	}{
		"global harness config":   {store.HarnessConfigScopeGlobal, false, hostedHCVolYAML},
		"workstation hub":         {store.HarnessConfigScopeProject, true, hostedHCVolYAML},
		"gcs volume, runtime TBD": {store.HarnessConfigScopeProject, false, hostedHCGCSYAML},
		"no volumes":              {store.HarnessConfigScopeProject, false, hostedHCOKYAML},
	} {
		t.Run(name+" accepted", func(t *testing.T) {
			srv, s, _ := testHarnessConfigFileServer(t)
			srv.workstation = tc.workstation
			scopeID := ""
			if tc.scope == store.HarnessConfigScopeProject {
				scopeID = tid("project-1")
			}
			hc := newHostedTestHC(t, srv, s, "hc-accepted", "accepted-hc", tc.scope, scopeID, "")
			rec := putHarnessConfigYAML(t, srv, hc.ID, tc.yaml)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
}

// TestHarnessConfigFileUpload_HostedProjectScope: the multipart upload
// refuses the same content before writing any part.
func TestHarnessConfigFileUpload_HostedProjectScope(t *testing.T) {
	srv, s, stor := testHarnessConfigFileServer(t)
	hc := newHostedTestHC(t, srv, s, "hc-upload", "upload-hc", store.HarnessConfigScopeProject, tid("project-1"), hostedHCOKYAML)
	objects := len(stor.content)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, harnessConfigMultipartRequest(t, hc.ID, map[string][]byte{
		"config.yaml": []byte(hostedHCVolYAML), "home/notes.md": []byte("x"),
	}))
	assertHostedHCRefusalBody(t, rec, "upload-hc")
	assert.Len(t, stor.content, objects, "no part written")
}

// TestHarnessConfigCommit_HostedProjectScope: the single commit path (used
// by finalize, the file APIs, clone, project clone, import and repair)
// refuses on its own, without the file APIs' early check, and leaves the
// row unchanged; writeHarnessConfigCommitError answers 400
// hosted_config_refused. A config.yaml too large to read is refused because
// it cannot be checked.
func TestHarnessConfigCommit_HostedProjectScope(t *testing.T) {
	ctx := context.Background()
	srv, s, _ := testHarnessConfigFileServer(t)
	hc := newHostedTestHC(t, srv, s, "hc-commit", "commit-hc", store.HarnessConfigScopeProject, tid("project-1"), hostedHCOKYAML)
	before := *hc

	data := []byte(hostedHCVolYAML)
	hash, blobPath, err := srv.writeHarnessConfigFileBlob(ctx, srv.GetStorage(), hc, data)
	require.NoError(t, err)
	err = srv.commitHarnessConfigFiles(ctx, hc, []store.TemplateFile{{Path: "config.yaml", Size: int64(len(data)), Hash: hash}},
		harnessConfigCommitOpts{content: map[string][]byte{"config.yaml": data}, written: map[string]bool{blobPath: true}})
	var he *hostedHarnessConfigError
	require.True(t, errors.As(err, &he), "refused: %v", err)
	assert.Equal(t, before.ContentHash, hc.ContentHash, "hc left unchanged")
	got, err := s.GetHarnessConfig(ctx, hc.ID)
	require.NoError(t, err)
	assert.Equal(t, before.ContentHash, got.ContentHash, "row unchanged")

	rec := httptest.NewRecorder()
	writeHarnessConfigCommitError(rec, he)
	assertHostedHCRefusalBody(t, rec, "commit-hc")

	tooLarge := hostedHarnessConfigYAMLRefusal("big-hc", nil, true, "")
	require.True(t, errors.As(tooLarge, &he), "too large is refused: %v", tooLarge)
	assert.Contains(t, tooLarge.Error(), `harness config "big-hc"`)
	assert.NoError(t, hostedHarnessConfigYAMLRefusal("no-yaml", nil, false, ""), "no config.yaml")
	assert.NoError(t, hostedHarnessConfigYAMLRefusal("broken", []byte(":\n\t- ["), true, ""), "unparseable: no broker can load it")
}

// TestHarnessConfigClone_HostedIntoProjectScope: cloning a global harness
// config (operator content, not checked) into project scope makes it member
// content, and the clone commit refuses its volumes.
func TestHarnessConfigClone_HostedIntoProjectScope(t *testing.T) {
	ctx := context.Background()
	srv, s, _ := testHarnessConfigFileServer(t)
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: tid("project-clone"), Name: "clone", Slug: "clone"}))
	src := newHostedTestHC(t, srv, s, "hc-global-src", "global-src", store.HarnessConfigScopeGlobal, "", hostedHCVolYAML)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+src.ID+"/clone", map[string]interface{}{
		"name": "cloned-hc", "scope": store.HarnessConfigScopeProject, "projectId": tid("project-clone"),
	})
	assertHostedHCRefusalBody(t, rec, "cloned-hc")
	_, err := s.GetHarnessConfigBySlug(ctx, "cloned-hc", store.HarnessConfigScopeProject, tid("project-clone"))
	assert.ErrorIs(t, err, store.ErrNotFound, "no clone stored")
}

func writeImportHCDir(t *testing.T, parent, name, yaml string) resourceDir {
	t.Helper()
	dir := filepath.Join(parent, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o644))
	return resourceDir{name: name, path: dir}
}

// TestHarnessConfigImport_HostedProjectScope: the import's content check
// refuses an import into project scope on a hosted hub naming every refused
// harness config, ordered by name, before anything is persisted; the
// non-streaming answer is 400 hosted_config_refused and the streaming error
// event carries the same code. Global scope and a workstation hub are not
// refused.
func TestHarnessConfigImport_HostedProjectScope(t *testing.T) {
	parent := t.TempDir()
	dirs := []resourceDir{
		writeImportHCDir(t, parent, "zeta-hc", hostedHCNFSYAML),
		writeImportHCDir(t, parent, "alpha-hc", hostedHCVolYAML),
		writeImportHCDir(t, parent, "ok-hc", hostedHCOKYAML),
	}
	srv := &Server{}
	check := srv.harnessConfigImportKind().checkContent

	err := check(dirs, store.HarnessConfigScopeProject)
	var ie *hostedHarnessConfigImportError
	require.True(t, errors.As(err, &ie), "refused: %v", err)
	require.Len(t, ie.errs, 2)
	assert.Contains(t, ie.errs[0].Error(), `harness config "alpha-hc"`)
	assert.Contains(t, ie.errs[1].Error(), `harness config "zeta-hc"`)

	rec := httptest.NewRecorder()
	require.True(t, writeImportContentRefusal(rec, err))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	code, msg := decodeHostedRefusal(t, rec)
	assert.Equal(t, hostedguard.ErrorCode, code)
	assert.NotContains(t, msg, "member-secret-src")
	ev := importErrorEvent(err)
	assert.Equal(t, hostedguard.ErrorCode, ev.Code)
	assert.Equal(t, msg, ev.Reason)

	assert.NoError(t, check(dirs, store.HarnessConfigScopeGlobal), "global scope is operator content")
	srv.workstation = true
	assert.NoError(t, check(dirs, store.HarnessConfigScopeProject), "workstation hub")
}

// addHostedTestHCFile adds file p with content to hc through the commit
// path with the check off (a row stored before the check existed).
func addHostedTestHCFile(t *testing.T, srv *Server, hc *store.HarnessConfig, p, content string) {
	t.Helper()
	ctx := context.Background()
	data := []byte(content)
	hash, blobPath, err := srv.writeHarnessConfigFileBlob(ctx, srv.GetStorage(), hc, data)
	require.NoError(t, err)
	was := srv.workstation
	srv.workstation = true
	defer func() { srv.workstation = was }()
	next := upsertTemplateFile(hc.Files, store.TemplateFile{Path: p, Size: int64(len(data)), Hash: hash})
	require.NoError(t, srv.commitHarnessConfigFiles(ctx, hc, next,
		harnessConfigCommitOpts{content: map[string][]byte{p: data}, written: map[string]bool{blobPath: true}}))
}

// hostedHCDispatcher is a hosted (or workstation) dispatcher wired like
// CreateAuthenticatedDispatcher, over srv's store, with one stored agent
// dispatched with harness config hc and no inline volumes.
func hostedHCDispatcher(t *testing.T, srv *Server, s store.Store, hc *store.HarnessConfig, hosted bool) (*HTTPAgentDispatcher, *mockRuntimeBrokerClient, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: tid("hc-broker"), Name: "hc-broker", Slug: "hc-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}))
	project := &store.Project{ID: tid("project-1"), Name: "hc-project", Slug: "hc-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("hc-agent"), Name: "hc-agent", Slug: "hc-agent",
		ProjectID: project.ID, RuntimeBrokerID: tid("hc-broker"),
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: hc.Name, HarnessConfigID: hc.ID, HarnessConfigHash: hc.ContentHash,
			InlineConfig: &api.ScionConfig{Harness: "claude"},
		},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	srv.workstation = !hosted
	m := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(s, m, false, slog.Default())
	d.SetHostedMode(hosted)
	d.SetHostedHarnessConfigCheck(srv.checkStoredHostedHarnessConfig)
	return d, m, agent
}

// TestDispatch_HostedHarnessConfigPrecheck: on a hosted hub every dispatch
// path refuses an agent dispatched with a project-scope harness config whose
// stored config.yaml holds a host volume (stored before the check existed),
// with a 400 hosted_config_refused and no broker call. A global harness
// config with the same content, a workstation hub, and a project harness
// config without volumes still dispatch.
func TestDispatch_HostedHarnessConfigPrecheck(t *testing.T) {
	ctx := context.Background()
	for _, op := range precheckOps(ctx) {
		t.Run(op.name+" refuses a stored project harness config", func(t *testing.T) {
			srv, s, _ := testHarnessConfigFileServer(t)
			hc := newHostedTestHC(t, srv, s, "hc-stored", "stored-hc", store.HarnessConfigScopeProject, tid("project-1"), hostedHCVolYAML)
			d, m, a := hostedHCDispatcher(t, srv, s, hc, true)
			err := op.run(d, a)
			var refusal *RuntimeTargetRefusal
			require.True(t, errors.As(err, &refusal), "refused with a typed refusal: %v", err)
			assert.Equal(t, http.StatusBadRequest, refusal.Status)
			assert.Equal(t, hostedguard.ErrorCode, refusal.Code)
			assert.Contains(t, refusal.Message, `volume "/hc-data"`)
			assert.Contains(t, refusal.Message, `harness config "stored-hc"`)
			assert.NotContains(t, refusal.Message, "member-secret-src")
			assert.False(t, op.called(m), "no broker call")
		})
	}
	for _, variant := range hostedHCConfigYAMLVariants {
		t.Run("start refuses a stored "+variant, func(t *testing.T) {
			srv, s, _ := testHarnessConfigFileServer(t)
			hc := newHostedTestHC(t, srv, s, "hc-case-stored", "case-stored-hc", store.HarnessConfigScopeProject, tid("project-1"), hostedHCOKYAML)
			addHostedTestHCFile(t, srv, hc, variant, hostedHCVolYAML)
			d, m, a := hostedHCDispatcher(t, srv, s, hc, true)
			err := d.DispatchAgentStart(ctx, a, "task", false)
			var refusal *RuntimeTargetRefusal
			require.True(t, errors.As(err, &refusal), "refused with a typed refusal: %v", err)
			assert.Equal(t, hostedguard.ErrorCode, refusal.Code)
			assert.Contains(t, refusal.Message, `harness config "case-stored-hc"`)
			assert.Contains(t, refusal.Message, "on a hosted server")
			assert.False(t, m.startCalled, "no broker call")
		})
	}
	t.Run("workstation hub dispatches a stored CONFIG.YAML", func(t *testing.T) {
		srv, s, _ := testHarnessConfigFileServer(t)
		hc := newHostedTestHC(t, srv, s, "hc-case-ws", "case-ws-hc", store.HarnessConfigScopeProject, tid("project-1"), hostedHCOKYAML)
		addHostedTestHCFile(t, srv, hc, "CONFIG.YAML", hostedHCVolYAML)
		d, m, a := hostedHCDispatcher(t, srv, s, hc, false)
		err := d.DispatchAgentStart(ctx, a, "task", false)
		var refusal *RuntimeTargetRefusal
		assert.False(t, errors.As(err, &refusal) && refusal.Code == hostedguard.ErrorCode, "no hosted refusal: %v", err)
		assert.True(t, m.startCalled, "broker called")
	})
	for name, tc := range map[string]struct {
		scope  string
		hosted bool
		yaml   string
	}{
		"global harness config with a host volume":    {store.HarnessConfigScopeGlobal, true, hostedHCVolYAML},
		"workstation hub with a host volume":          {store.HarnessConfigScopeProject, false, hostedHCVolYAML},
		"hosted hub, project harness config, no vols": {store.HarnessConfigScopeProject, true, hostedHCOKYAML},
	} {
		t.Run(name+" dispatches", func(t *testing.T) {
			srv, s, _ := testHarnessConfigFileServer(t)
			scopeID := ""
			if tc.scope == store.HarnessConfigScopeProject {
				scopeID = tid("project-1")
			}
			hc := newHostedTestHC(t, srv, s, "hc-ok", "ok-hc", tc.scope, scopeID, tc.yaml)
			d, m, a := hostedHCDispatcher(t, srv, s, hc, tc.hosted)
			err := d.DispatchAgentStart(ctx, a, "task", false)
			var refusal *RuntimeTargetRefusal
			assert.False(t, errors.As(err, &refusal) && refusal.Code == hostedguard.ErrorCode, "no hosted refusal: %v", err)
			assert.True(t, m.startCalled, "broker called")
		})
	}
}

// TestCreateAuthenticatedDispatcher_WiresHostedHarnessConfigCheck: the
// production dispatcher carries the stored harness-config check.
func TestCreateAuthenticatedDispatcher_WiresHostedHarnessConfigCheck(t *testing.T) {
	srv, _, _ := testHarnessConfigFileServer(t)
	d := srv.CreateAuthenticatedDispatcher()
	require.NotNil(t, d.hostedHarnessConfigCheck)
	assert.Equal(t, !srv.config.Workstation, d.hostedMode)
}

// TestValidateAgentConfigPatch_HostedRefusesMemberVolumes: on a hosted hub
// a config PATCH (and a held edit, which is validated by the same call)
// naming a host volume is refused when written; a workstation hub and a
// gcs volume are not refused.
func TestValidateAgentConfigPatch_HostedRefusesMemberVolumes(t *testing.T) {
	ctx := context.Background()
	srv, _ := testServer(t)
	agent := &store.Agent{ID: tid("agent-patch-vol"), ProjectID: tid("project-patch-vol"), AppliedConfig: &store.AgentAppliedConfig{}}
	patch := &api.ScionConfig{Volumes: []api.VolumeMount{{Source: "/srv/member-secret-src", Target: "/data"}}}

	rec := httptest.NewRecorder()
	assert.False(t, srv.validateAgentConfigPatch(ctx, rec, agent, patch, nil))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	code, msg := decodeHostedRefusal(t, rec)
	assert.Equal(t, hostedguard.ErrorCode, code)
	assert.Contains(t, msg, `volume "/data": host volumes in agent config`)
	assert.NotContains(t, msg, "member-secret-src")

	rec = httptest.NewRecorder()
	assert.True(t, srv.validateAgentConfigPatch(ctx, rec, agent, &api.ScionConfig{Volumes: []api.VolumeMount{{Type: "gcs", Bucket: "b", Target: "/b"}}}, nil), rec.Body.String())

	srv.workstation = true
	rec = httptest.NewRecorder()
	assert.True(t, srv.validateAgentConfigPatch(ctx, rec, agent, patch, nil), rec.Body.String())
}

// TestAgentPendingConfig_HostedRefusesMemberVolumes: through the HTTP
// PATCH, a held edit naming a host volume is refused on a hosted hub and
// nothing is held.
func TestAgentPendingConfig_HostedRefusesMemberVolumes(t *testing.T) {
	srv, s, _, agent := heldEditsFixture(t, state.PhaseRunning)
	require.False(t, srv.workstation, "fixture is a hosted hub")
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"volumes": []map[string]interface{}{{"source": "/srv/member-secret-src", "target": "/data"}}},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	code, _ := decodeHostedRefusal(t, rec)
	assert.Equal(t, hostedguard.ErrorCode, code)
	unchanged := pendingReloadAgent(t, s, agent.ID)
	assert.Nil(t, unchanged.PendingConfig)
	assert.Equal(t, agent.StateVersion, unchanged.StateVersion)
}

// hostedKindDispatcher is a hosted dispatcher over srv's store whose broker
// reports profiles (name -> type), with one stored agent on profile whose
// inline config holds inlineVols and which is dispatched with hc (nil for
// none).
func hostedKindDispatcher(t *testing.T, srv *Server, s store.Store, profiles map[string]string, profile string, inlineVols []api.VolumeMount, hc *store.HarnessConfig) (*HTTPAgentDispatcher, *mockRuntimeBrokerClient, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	broker := &store.RuntimeBroker{
		ID: tid("kind-broker"), Name: "kind-broker", Slug: "kind-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	for name, typ := range profiles {
		broker.Profiles = append(broker.Profiles, store.BrokerProfile{Name: name, Type: typ, Available: true})
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{ID: tid("project-1"), Name: "kind-project", Slug: "kind-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	applied := &store.AgentAppliedConfig{Profile: profile, InlineConfig: &api.ScionConfig{Harness: "claude", Volumes: inlineVols}}
	if hc != nil {
		applied.HarnessConfig, applied.HarnessConfigID, applied.HarnessConfigHash = hc.Name, hc.ID, hc.ContentHash
	}
	agent := &store.Agent{
		ID: tid("kind-agent"), Name: "kind-agent", Slug: "kind-agent",
		ProjectID: project.ID, RuntimeBrokerID: broker.ID, AppliedConfig: applied,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	srv.workstation = false
	m := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(s, m, false, slog.Default())
	d.SetHostedMode(true)
	d.SetHostedHarnessConfigCheck(srv.checkStoredHostedHarnessConfig)
	return d, m, agent
}

// TestDispatch_HostedGCSByBrokerProfileType: at dispatch a hosted hub
// decides gcs from the runtime type the target broker reports for the
// agent's profile: refused unless it is Kubernetes (kubernetes, k8s,
// remote), and let through to the broker only when the profile or its type
// is not known. It applies to the agent's own config and to a project
// harness config alike.
func TestDispatch_HostedGCSByBrokerProfileType(t *testing.T) {
	ctx := context.Background()
	gcs := []api.VolumeMount{{Type: "gcs", Bucket: "member-bucket", Target: "/hc-data"}}
	cases := []struct {
		name     string
		profiles map[string]string
		profile  string
		refused  bool
		// unknown: the hub cannot tell the runtime. gcs in agent config is
		// left to the broker, which checks it again with its real runtime;
		// a project harness config's hydrated copy is not checked by the
		// broker, so the hub refuses it.
		unknown bool
	}{
		{"docker profile", map[string]string{"local": "docker"}, "local", true, false},
		{"podman default profile", map[string]string{"only": "podman"}, "", true, false},
		{"kubernetes profile", map[string]string{"k8s-prod": "kubernetes"}, "k8s-prod", false, false},
		{"k8s alias", map[string]string{"gke": "k8s"}, "gke", false, false},
		{"remote alias", map[string]string{"r": "remote"}, "r", false, false},
		{"profile type not reported", map[string]string{"blank": ""}, "blank", false, true},
		{"profile not on the broker", map[string]string{"local": "docker"}, "missing", false, true},
		{"broker reports no profiles", nil, "", false, true},
		{"several profiles, none named", map[string]string{"a": "docker", "b": "kubernetes"}, "", false, true},
	}
	for _, src := range []string{"agent config", "harness config"} {
		for _, tc := range cases {
			t.Run(src+"/"+tc.name, func(t *testing.T) {
				srv, s, _ := testHarnessConfigFileServer(t)
				var hc *store.HarnessConfig
				inline := gcs
				if src == "harness config" {
					hc = newHostedTestHC(t, srv, s, "hc-gcs", "gcs-hc", store.HarnessConfigScopeProject, tid("project-1"), hostedHCGCSYAML)
					inline = nil
				}
				d, m, a := hostedKindDispatcher(t, srv, s, tc.profiles, tc.profile, inline, hc)
				err := d.DispatchAgentStart(ctx, a, "task", false)
				var refusal *RuntimeTargetRefusal
				isRefusal := errors.As(err, &refusal) && refusal.Code == hostedguard.ErrorCode
				if tc.refused || (tc.unknown && src == "harness config") {
					require.True(t, isRefusal, "refused: %v", err)
					assert.Contains(t, refusal.Message, "only for agents run on Kubernetes")
					if tc.unknown {
						assert.Contains(t, refusal.Message, "runtime could not be determined")
					}
					assert.Contains(t, refusal.Message, src)
					assert.NotContains(t, refusal.Message, "member-bucket")
					assert.False(t, m.startCalled, "no broker call")
					return
				}
				assert.False(t, isRefusal, "not refused: %v", err)
				assert.True(t, m.startCalled, "broker called")
			})
		}
	}
}

// TestValidateAgentConfigPatch_HostedGCSByBrokerProfileType: a config
// PATCH is judged with the same broker-reported profile type.
func TestValidateAgentConfigPatch_HostedGCSByBrokerProfileType(t *testing.T) {
	ctx := context.Background()
	gcsPatch := &api.ScionConfig{Volumes: []api.VolumeMount{{Type: "gcs", Bucket: "b", Target: "/b"}}}
	for name, tc := range map[string]struct {
		typ     string
		refused bool
	}{
		"docker":     {"docker", true},
		"kubernetes": {"kubernetes", false},
		"unknown":    {"", false},
	} {
		t.Run(name, func(t *testing.T) {
			srv, s := testServer(t)
			require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
				ID: tid("patch-broker"), Name: "patch-broker", Slug: "patch-broker", Status: store.BrokerStatusOnline,
				Profiles: []store.BrokerProfile{{Name: "p", Type: tc.typ, Available: true}},
			}))
			agent := &store.Agent{ID: tid("agent-patch-gcs"), ProjectID: tid("project-patch-gcs"), RuntimeBrokerID: tid("patch-broker"),
				AppliedConfig: &store.AgentAppliedConfig{Profile: "p"}}
			rec := httptest.NewRecorder()
			ok := srv.validateAgentConfigPatch(ctx, rec, agent, gcsPatch, nil)
			if !tc.refused {
				assert.True(t, ok, rec.Body.String())
				return
			}
			assert.False(t, ok)
			code, _ := decodeHostedRefusal(t, rec)
			assert.Equal(t, hostedguard.ErrorCode, code)
		})
	}
}
