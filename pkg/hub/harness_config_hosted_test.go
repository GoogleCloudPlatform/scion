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
	body := `{"content": "` + strings.ReplaceAll(yaml, "\n", `\n`) + `"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/harness-configs/"+hcID+"/files/config.yaml", strings.NewReader(body))
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

	tooLarge := hostedHarnessConfigYAMLRefusal("big-hc", nil, true)
	require.True(t, errors.As(tooLarge, &he), "too large is refused: %v", tooLarge)
	assert.Contains(t, tooLarge.Error(), `harness config "big-hc"`)
	assert.NoError(t, hostedHarnessConfigYAMLRefusal("no-yaml", nil, false), "no config.yaml")
	assert.NoError(t, hostedHarnessConfigYAMLRefusal("broken", []byte(":\n\t- ["), true), "unparseable: no broker can load it")
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
	assert.False(t, srv.validateAgentConfigPatch(ctx, rec, agent, patch, false))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	code, msg := decodeHostedRefusal(t, rec)
	assert.Equal(t, hostedguard.ErrorCode, code)
	assert.Contains(t, msg, `volume "/data": host volumes in agent config`)
	assert.NotContains(t, msg, "member-secret-src")

	rec = httptest.NewRecorder()
	assert.True(t, srv.validateAgentConfigPatch(ctx, rec, agent, &api.ScionConfig{Volumes: []api.VolumeMount{{Type: "gcs", Bucket: "b", Target: "/b"}}}, false), rec.Body.String())

	srv.workstation = true
	rec = httptest.NewRecorder()
	assert.True(t, srv.validateAgentConfigPatch(ctx, rec, agent, patch, false), rec.Body.String())
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
