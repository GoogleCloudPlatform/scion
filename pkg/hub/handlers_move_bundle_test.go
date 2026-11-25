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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer/bundle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMoveBundleStorage is an HTTP-signing GCS-like storage fake that records
// signed URL options and serves uploaded objects (such as manifest.json) on Download.
type fakeMoveBundleStorage struct {
	*mockStorage
	mu        sync.Mutex
	signed    []signedRecord
	issuedURL []string
}

type signedRecord struct {
	path string
	opts storage.SignedURLOptions
}

func newFakeMoveBundleStorage() *fakeMoveBundleStorage {
	m := newMockStorage("mb-bucket")
	m.provider = storage.ProviderGCS
	return &fakeMoveBundleStorage{mockStorage: m}
}

func (f *fakeMoveBundleStorage) GenerateSignedURL(_ context.Context, objectPath string, opts storage.SignedURLOptions) (*storage.SignedURL, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := "https://storage.googleapis.com/mb-bucket/" + url.PathEscape(objectPath) +
		"?X-Goog-Algorithm=GOOG4-RSA-SHA256&X-Goog-Expires=900&X-Goog-Signature=mbsig" + fmt.Sprint(len(f.issuedURL))
	f.signed = append(f.signed, signedRecord{path: objectPath, opts: opts})
	f.issuedURL = append(f.issuedURL, u)
	headers := map[string]string{}
	if opts.Method == http.MethodPut {
		if opts.ContentType != "" {
			headers["Content-Type"] = opts.ContentType
		}
		if opts.ContentLengthRange != nil {
			headers["x-goog-content-length-range"] = fmt.Sprintf("%d,%d", opts.ContentLengthRange.Min, opts.ContentLengthRange.Max)
		}
	}
	return &storage.SignedURL{
		URL:     u,
		Method:  opts.Method,
		Expires: time.Now().Add(opts.Expires),
		Headers: headers,
	}, nil
}

type moveBundleEndpointFixture struct {
	srv     *Server
	s       store.Store
	stor    *fakeMoveBundleStorage
	srcID   string
	dstID   string
	otherID string
	project *store.Project
	agent   *store.Agent
	rec     *store.AgentReincarnation
}

func newMoveBundleEndpointFixture(t *testing.T, state, mode string) *moveBundleEndpointFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	f := &moveBundleEndpointFixture{
		srv:     srv,
		s:       s,
		stor:    newFakeMoveBundleStorage(),
		srcID:   tid("mb-src"),
		dstID:   tid("mb-dst"),
		otherID: tid("mb-other"),
	}
	srv.SetStorage(f.stor)

	for _, id := range []string{f.srcID, f.dstID, f.otherID} {
		require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
			ID:     id,
			Slug:   id,
			Name:   id,
			Status: store.BrokerStatusOnline,
		}))
	}
	f.project = &store.Project{
		ID:      tid("mb-project"),
		Slug:    "mb-project",
		Name:    "MB Project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, f.project))

	assignedBroker := f.srcID
	if state == store.AgentReincarnationStateProvisioning {
		assignedBroker = f.dstID
	}
	f.agent = &store.Agent{
		ID:              tid("mb-agent"),
		Slug:            "mb-agent",
		Name:            "mb-agent",
		ProjectID:       f.project.ID,
		RuntimeBrokerID: assignedBroker,
	}
	require.NoError(t, s.CreateAgent(ctx, f.agent))

	f.rec = &store.AgentReincarnation{
		ID:             tid("rid-bundle-" + state + "-" + mode),
		AgentID:        f.agent.ID,
		FromGeneration: 1,
		ToGeneration:   2,
		State:          state,
		SourceBrokerID: f.srcID,
		TargetBrokerID: f.dstID,
		TransferMode:   mode,
		RequestedAt:    time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}
	require.NoError(t, s.CreateAgentReincarnation(ctx, f.rec))
	return f
}

func (f *moveBundleEndpointFixture) doAsBroker(method, path, callerBrokerID string, body any) *httptest.ResponseRecorder {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if callerBrokerID != "" {
		req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity(callerBrokerID)))
	}
	rec := httptest.NewRecorder()
	f.srv.handleRuntimeBrokerRoutes(rec, req)
	return rec
}

func (f *moveBundleEndpointFixture) uploadURLPath(brokerID, agentID, rid string) string {
	return fmt.Sprintf("/api/v1/runtime-brokers/%s/agents/%s/move-bundle/%s/upload-url", brokerID, agentID, rid)
}

func (f *moveBundleEndpointFixture) downloadURLsPath(brokerID, agentID, rid string) string {
	return fmt.Sprintf("/api/v1/runtime-brokers/%s/agents/%s/move-bundle/%s/download-urls", brokerID, agentID, rid)
}

// T9: upload-url happy path, size binding, Hub-derived keys, no URL in logs, and full authz matrix.
func TestMoveBundleUploadURL_AuthzMatrix(t *testing.T) {
	f := newMoveBundleEndpointFixture(t, store.AgentReincarnationStateStopping, store.TransferModeBundle)
	// Capture logs to ensure signed URLs never appear in logs.
	var logBuf bytes.Buffer
	f.srv.workspaceLog = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	validReq := api.MoveBundleUploadURLRequest{
		Root:      storage.MoveBundleRootWorkspace,
		PartIndex: 0,
		Size:      1024,
		Hash:      "sha256:abcdef",
	}

	// Happy path.
	rec := f.doAsBroker(http.MethodPost, f.uploadURLPath(f.srcID, f.agent.ID, f.rec.ID), f.srcID, validReq)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp api.MoveBundleUploadURLResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, http.MethodPut, resp.Method)
	assert.Equal(t, int64(SignedURLExpiry/time.Second), resp.ExpiresInSeconds)
	assert.Equal(t, "application/octet-stream", resp.Headers["Content-Type"])
	assert.Equal(t, "1024,1024", resp.Headers["x-goog-content-length-range"])
	assert.NotEmpty(t, resp.URL)
	assert.NotContains(t, logBuf.String(), resp.URL, "signed URL must never appear in logs")
	assert.NotContains(t, logBuf.String(), "X-Goog-Signature", "signature must never appear in logs")

	require.Len(t, f.stor.signed, 1)
	wantKey, err := storage.MoveBundlePartPath(f.srv.HubID(), f.rec.ID, storage.MoveBundleRootWorkspace, 0)
	require.NoError(t, err)
	assert.Equal(t, wantKey, f.stor.signed[0].path)
	require.NotNil(t, f.stor.signed[0].opts.ContentLengthRange)
	assert.Equal(t, int64(1024), f.stor.signed[0].opts.ContentLengthRange.Min)
	assert.Equal(t, int64(1024), f.stor.signed[0].opts.ContentLengthRange.Max)

	// Method not allowed.
	rec = f.doAsBroker(http.MethodGet, f.uploadURLPath(f.srcID, f.agent.ID, f.rec.ID), f.srcID, nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)

	// Unauthenticated / broker ID mismatch -> 403.
	rec = f.doAsBroker(http.MethodPost, f.uploadURLPath(f.srcID, f.agent.ID, f.rec.ID), "", validReq)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	rec = f.doAsBroker(http.MethodPost, f.uploadURLPath(f.srcID, f.agent.ID, f.rec.ID), f.dstID, validReq)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// Target broker calling under its own brokerID -> 403 (not rec.SourceBrokerID).
	rec = f.doAsBroker(http.MethodPost, f.uploadURLPath(f.dstID, f.agent.ID, f.rec.ID), f.dstID, validReq)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// Unknown rid -> 404.
	rec = f.doAsBroker(http.MethodPost, f.uploadURLPath(f.srcID, f.agent.ID, tid("rid-unknown")), f.srcID, validReq)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// Wrong agentId -> 404.
	rec = f.doAsBroker(http.MethodPost, f.uploadURLPath(f.srcID, "other-agent", f.rec.ID), f.srcID, validReq)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// Invalid root -> 400.
	badRoot := validReq
	badRoot.Root = "secrets"
	rec = f.doAsBroker(http.MethodPost, f.uploadURLPath(f.srcID, f.agent.ID, f.rec.ID), f.srcID, badRoot)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// Index beyond maxParts -> 400.
	lim := f.srv.MoveBundleExportLimits().WorkspaceLimits()
	overIdx := validReq
	overIdx.PartIndex = lim.MaxParts()
	rec = f.doAsBroker(http.MethodPost, f.uploadURLPath(f.srcID, f.agent.ID, f.rec.ID), f.srcID, overIdx)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// Negative part index -> 400.
	negIdx := validReq
	negIdx.PartIndex = -1
	rec = f.doAsBroker(http.MethodPost, f.uploadURLPath(f.srcID, f.agent.ID, f.rec.ID), f.srcID, negIdx)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// Oversized part -> 400.
	overSize := validReq
	overSize.Size = lim.PartSize + 1
	rec = f.doAsBroker(http.MethodPost, f.uploadURLPath(f.srcID, f.agent.ID, f.rec.ID), f.srcID, overSize)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// Zero size -> 400.
	zeroSize := validReq
	zeroSize.Size = 0
	rec = f.doAsBroker(http.MethodPost, f.uploadURLPath(f.srcID, f.agent.ID, f.rec.ID), f.srcID, zeroSize)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// Wrong state -> 409.
	for _, st := range []string{
		store.AgentReincarnationStatePending,
		store.AgentReincarnationStateProvisioning,
		store.AgentReincarnationStateStarting,
		store.AgentReincarnationStateCompleted,
		store.AgentReincarnationStateFailed,
	} {
		f2 := newMoveBundleEndpointFixture(t, st, store.TransferModeBundle)
		rec = f2.doAsBroker(http.MethodPost, f2.uploadURLPath(f2.srcID, f2.agent.ID, f2.rec.ID), f2.srcID, validReq)
		assert.Equal(t, http.StatusConflict, rec.Code, "state=%s", st)
	}

	// Wrong mode -> 409.
	for _, mode := range []string{store.TransferModeExport, store.TransferModeRecreate, ""} {
		f2 := newMoveBundleEndpointFixture(t, store.AgentReincarnationStateStopping, mode)
		rec = f2.doAsBroker(http.MethodPost, f2.uploadURLPath(f2.srcID, f2.agent.ID, f2.rec.ID), f2.srcID, validReq)
		assert.Equal(t, http.StatusConflict, rec.Code, "mode=%q", mode)
	}

	// Non-GCS (local) and nil storage -> 409 move_bundle_unsupported.
	for _, stor := range []storage.Storage{nil, &mockStorage{provider: storage.ProviderLocal}} {
		f.srv.SetStorage(stor)
		rec = f.doAsBroker(http.MethodPost, f.uploadURLPath(f.srcID, f.agent.ID, f.rec.ID), f.srcID, validReq)
		assert.Equal(t, http.StatusConflict, rec.Code)
		assert.Contains(t, rec.Body.String(), api.BrokerErrCodeMoveBundleUnsupported)
	}
}

// T9: download-urls happy path, manifest-driven parts, no URL in logs, and full authz matrix.
func TestMoveBundleDownloadURLs_AuthzMatrix(t *testing.T) {
	f := newMoveBundleEndpointFixture(t, store.AgentReincarnationStateProvisioning, store.TransferModeBundle)
	var logBuf bytes.Buffer
	f.srv.workspaceLog = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Persist a valid manifest.json at the Hub-derived manifest key.
	manifest := bundle.Manifest{
		Version:         bundle.ManifestVersion,
		ReincarnationID: f.rec.ID,
		Roots: map[string]bundle.RootManifest{
			storage.MoveBundleRootWorkspace: {
				PartSize: 64 << 20,
				Entries:  10,
				Bytes:    2048,
				Parts: []bundle.Part{
					{Index: 0, Size: 512, Hash: "sha256:0000000000000000000000000000000000000000000000000000000000000001"},
					{Index: 1, Size: 256, Hash: "sha256:0000000000000000000000000000000000000000000000000000000000000002"},
				},
			},
			storage.MoveBundleRootHome: {
				PartSize: 64 << 20,
				Entries:  3,
				Bytes:    512,
				Parts: []bundle.Part{
					{Index: 0, Size: 128, Hash: "sha256:0000000000000000000000000000000000000000000000000000000000000003"},
				},
			},
		},
	}
	manifestBytes, _, err := bundle.CanonicalManifest(manifest)
	require.NoError(t, err)
	manifestKey, err := storage.MoveBundleManifestPath(f.srv.HubID(), f.rec.ID)
	require.NoError(t, err)
	_, err = f.stor.Upload(context.Background(), manifestKey, bytes.NewReader(manifestBytes), storage.UploadOptions{ContentType: "application/json"})
	require.NoError(t, err)

	// Happy path.
	rec := f.doAsBroker(http.MethodPost, f.downloadURLsPath(f.dstID, f.agent.ID, f.rec.ID), f.dstID, api.MoveBundleDownloadURLsRequest{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp api.MoveBundleDownloadURLsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, int64(SignedURLExpiry/time.Second), resp.ExpiresInSeconds)
	require.Len(t, resp.Parts, 3)
	assert.Equal(t, storage.MoveBundleRootWorkspace, resp.Parts[0].Root)
	assert.Equal(t, 0, resp.Parts[0].Index)
	assert.Equal(t, storage.MoveBundleRootWorkspace, resp.Parts[1].Root)
	assert.Equal(t, 1, resp.Parts[1].Index)
	assert.Equal(t, storage.MoveBundleRootHome, resp.Parts[2].Root)
	assert.Equal(t, 0, resp.Parts[2].Index)
	for _, p := range resp.Parts {
		assert.NotEmpty(t, p.URL)
		assert.NotContains(t, logBuf.String(), p.URL, "signed URL must never appear in logs")
	}
	assert.NotContains(t, logBuf.String(), "X-Goog-Signature")
	for _, srec := range f.stor.signed {
		assert.Equal(t, http.MethodGet, srec.opts.Method)
		assert.Equal(t, SignedURLExpiry, srec.opts.Expires)
	}

	// Method not allowed.
	rec = f.doAsBroker(http.MethodGet, f.downloadURLsPath(f.dstID, f.agent.ID, f.rec.ID), f.dstID, nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)

	// Unauthenticated / source broker -> 403.
	rec = f.doAsBroker(http.MethodPost, f.downloadURLsPath(f.dstID, f.agent.ID, f.rec.ID), "", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	rec = f.doAsBroker(http.MethodPost, f.downloadURLsPath(f.srcID, f.agent.ID, f.rec.ID), f.srcID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// Wrong agentId -> 404.
	rec = f.doAsBroker(http.MethodPost, f.downloadURLsPath(f.dstID, "other-agent", f.rec.ID), f.dstID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// Wrong state -> 409.
	for _, st := range []string{
		store.AgentReincarnationStatePending,
		store.AgentReincarnationStateStopping,
		store.AgentReincarnationStateStarting,
		store.AgentReincarnationStateCompleted,
		store.AgentReincarnationStateFailed,
	} {
		f2 := newMoveBundleEndpointFixture(t, st, store.TransferModeBundle)
		rec = f2.doAsBroker(http.MethodPost, f2.downloadURLsPath(f2.dstID, f2.agent.ID, f2.rec.ID), f2.dstID, nil)
		assert.Equal(t, http.StatusConflict, rec.Code, "state=%s", st)
	}

	// Wrong mode -> 409.
	fExport := newMoveBundleEndpointFixture(t, store.AgentReincarnationStateProvisioning, store.TransferModeExport)
	rec = fExport.doAsBroker(http.MethodPost, fExport.downloadURLsPath(fExport.dstID, fExport.agent.ID, fExport.rec.ID), fExport.dstID, nil)
	assert.Equal(t, http.StatusConflict, rec.Code)

	// Agent not assigned to target broker -> 404.
	fUnassigned := newMoveBundleEndpointFixture(t, store.AgentReincarnationStateProvisioning, store.TransferModeBundle)
	fUnassigned.agent.RuntimeBrokerID = fUnassigned.srcID
	require.NoError(t, fUnassigned.s.UpdateAgent(context.Background(), fUnassigned.agent))
	rec = fUnassigned.doAsBroker(http.MethodPost, fUnassigned.downloadURLsPath(fUnassigned.dstID, fUnassigned.agent.ID, fUnassigned.rec.ID), fUnassigned.dstID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// Missing manifest.json -> 409.
	fNoManifest := newMoveBundleEndpointFixture(t, store.AgentReincarnationStateProvisioning, store.TransferModeBundle)
	rec = fNoManifest.doAsBroker(http.MethodPost, fNoManifest.downloadURLsPath(fNoManifest.dstID, fNoManifest.agent.ID, fNoManifest.rec.ID), fNoManifest.dstID, nil)
	assert.Equal(t, http.StatusConflict, rec.Code)

	// Mismatched reincarnationId in manifest -> 502 (RuntimeError).
	fBadRID := newMoveBundleEndpointFixture(t, store.AgentReincarnationStateProvisioning, store.TransferModeBundle)
	badManifest := manifest
	badManifest.ReincarnationID = tid("other-rid")
	badBytes, _, err := bundle.CanonicalManifest(badManifest)
	require.NoError(t, err)
	badKey, err := storage.MoveBundleManifestPath(fBadRID.srv.HubID(), fBadRID.rec.ID)
	require.NoError(t, err)
	_, err = fBadRID.stor.Upload(context.Background(), badKey, bytes.NewReader(badBytes), storage.UploadOptions{ContentType: "application/json"})
	require.NoError(t, err)
	rec = fBadRID.doAsBroker(http.MethodPost, fBadRID.downloadURLsPath(fBadRID.dstID, fBadRID.agent.ID, fBadRID.rec.ID), fBadRID.dstID, nil)
	assert.Equal(t, http.StatusBadGateway, rec.Code)

	// Corrupt manifest JSON -> 502 (RuntimeError).
	fCorrupt := newMoveBundleEndpointFixture(t, store.AgentReincarnationStateProvisioning, store.TransferModeBundle)
	corruptKey, err := storage.MoveBundleManifestPath(fCorrupt.srv.HubID(), fCorrupt.rec.ID)
	require.NoError(t, err)
	_, err = fCorrupt.stor.Upload(context.Background(), corruptKey, strings.NewReader("{not valid json"), storage.UploadOptions{ContentType: "application/json"})
	require.NoError(t, err)
	rec = fCorrupt.doAsBroker(http.MethodPost, fCorrupt.downloadURLsPath(fCorrupt.dstID, fCorrupt.agent.ID, fCorrupt.rec.ID), fCorrupt.dstID, nil)
	assert.Equal(t, http.StatusBadGateway, rec.Code)

	// Oversized manifest (> moveBundleManifestMaxBytes) -> 502 (RuntimeError).
	fOversize := newMoveBundleEndpointFixture(t, store.AgentReincarnationStateProvisioning, store.TransferModeBundle)
	oversizeKey, err := storage.MoveBundleManifestPath(fOversize.srv.HubID(), fOversize.rec.ID)
	require.NoError(t, err)
	oversizeData := bytes.Repeat([]byte(" "), moveBundleManifestMaxBytes+10)
	_, err = fOversize.stor.Upload(context.Background(), oversizeKey, bytes.NewReader(oversizeData), storage.UploadOptions{ContentType: "application/json"})
	require.NoError(t, err)
	rec = fOversize.doAsBroker(http.MethodPost, fOversize.downloadURLsPath(fOversize.dstID, fOversize.agent.ID, fOversize.rec.ID), fOversize.dstID, nil)
	assert.Equal(t, http.StatusBadGateway, rec.Code)

	// Non-GCS (local) and nil storage -> 409 move_bundle_unsupported.
	for _, stor := range []storage.Storage{nil, &mockStorage{provider: storage.ProviderLocal}} {
		f.srv.SetStorage(stor)
		rec = f.doAsBroker(http.MethodPost, f.downloadURLsPath(f.dstID, f.agent.ID, f.rec.ID), f.dstID, nil)
		assert.Equal(t, http.StatusConflict, rec.Code)
		assert.Contains(t, rec.Body.String(), api.BrokerErrCodeMoveBundleUnsupported)
	}
}

func TestResolveMoveBundleSecretFileTargets_ForwardedOnPrecheckAndExport(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Configure local secret backend with hub-scoped and project-scoped file
	// secrets plus an env secret.
	proj := &store.Project{ID: tid("sec-proj"), Slug: "sec-proj", Name: "Sec Proj", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, proj))

	hubID := "test-hub-secret-targets"
	secBackend := secret.NewLocalBackend(s, hubID, "test-secret-key-0123456789abcdef")
	_, _, err := secBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "HUB_CREDS",
		Value:      "hub-creds-data",
		SecretType: store.SecretTypeFile,
		Target:     ".config/x/creds",
		Scope:      secret.ScopeHub,
		ScopeID:    hubID,
	})
	require.NoError(t, err)
	_, _, err = secBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "PROJ_CERT",
		Value:      "pem-data",
		SecretType: store.SecretTypeFile,
		Target:     "~/.config/corp/cert.pem",
		Scope:      secret.ScopeProject,
		ScopeID:    proj.ID,
	})
	require.NoError(t, err)
	_, _, err = secBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "API_KEY",
		Value:      "secret-val",
		SecretType: store.SecretTypeEnvironment,
		Target:     "API_KEY",
		Scope:      secret.ScopeProject,
		ScopeID:    proj.ID,
	})
	require.NoError(t, err)

	var gotPrecheckTargets []string
	var gotExportTargets []string
	brokerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/move-bundle/precheck"):
			var req api.MoveBundlePrecheckRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			gotPrecheckTargets = req.SecretFileTargets
			writeJSON(w, http.StatusOK, api.MoveBundlePrecheckResponse{Durable: true})
		case strings.HasSuffix(r.URL.Path, "/move-bundle/export"):
			var req api.MoveBundleExportRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			gotExportTargets = req.SecretFileTargets
			writeJSON(w, http.StatusAccepted, api.MoveBundleJobStatus{
				ReincarnationID: req.ReincarnationID,
				Kind:            api.MoveBundleJobKindExport,
				State:           api.MoveBundleJobStateRunning,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer brokerSrv.Close()

	broker := &store.RuntimeBroker{
		ID:       tid("sec-broker"),
		Slug:     "sec-broker",
		Name:     "sec-broker",
		Status:   store.BrokerStatusOnline,
		Endpoint: brokerSrv.URL,
		Capabilities: &store.BrokerCapabilities{
			AgentMove:       true,
			AgentMoveBundle: true,
		},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	agent := &store.Agent{ID: tid("sec-agent"), Slug: "sec-agent", Name: "sec-agent", ProjectID: proj.ID, RuntimeBrokerID: broker.ID}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// O2: ResolveMoveBundleSecretFileTargets uses metadata-only store.ListSecrets
	// (including store.ScopeHub) and does not require or call secretBackend.Resolve.
	disp := NewHTTPAgentDispatcher(s, false, slog.Default())
	disp.SetHubID(hubID)
	srv.SetDispatcher(disp)

	wantTargets := []string{".config/x/creds", "~/.config/corp/cert.pem"}
	targets, err := disp.ResolveMoveBundleSecretFileTargets(ctx, agent)
	require.NoError(t, err)
	assert.Equal(t, wantTargets, targets)

	// O6: Verify moveBundleSourceProbe itself resolves and forwards secret targets.
	probe := srv.moveBundleSourceProbe(ctx)
	require.NotNil(t, probe)
	_, err = probe(broker, agent)
	require.NoError(t, err)
	assert.Equal(t, wantTargets, gotPrecheckTargets)

	limits := config.DefaultMoveBundleConfig().ExportLimits()
	_, err = disp.DispatchMoveBundleExport(ctx, agent, "rid-sec-1", limits, targets)
	require.NoError(t, err)
	assert.Equal(t, wantTargets, gotExportTargets)
}

// O6: Handler-level HTTP dry-run tests covering planReincarnateMove wiring of
// experimentEnabled(AgentMoveBundle), moveBundleStorageCheck, and moveBundleSourceProbe.
func TestReincarnateMove_BundleDryRun_HandlerWiring(t *testing.T) {
	setup := func(t *testing.T, enableExp bool) *moveFixture {
		t.Helper()
		f := setupForceMoveFixture(t, false, func(dst *store.RuntimeBroker) {
			dst.AutoProvide = true
			dst.Capabilities = &store.BrokerCapabilities{
				Reprovision:       true,
				AgentMove:         true,
				AgentMoveRecreate: true,
				AgentMoveBundle:   true,
			}
		})
		ctx := context.Background()
		f.src.Capabilities = &store.BrokerCapabilities{
			Reprovision:       true,
			AgentMove:         true,
			AgentMoveRecreate: true,
			AgentMoveBundle:   true,
		}
		require.NoError(t, f.s.UpdateRuntimeBroker(ctx, f.src))
		f.srv.SetStorage(newFakeMoveBundleStorage())
		if enableExp {
			setExperimentOverrides(t, f.srv, map[string]bool{experiments.AgentMoveBundle: true})
		}
		return f
	}

	t.Run("experiment off returns refusal with bundleUnavailable naming experiment", func(t *testing.T) {
		f := setup(t, false)
		rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/reincarnate",
			ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		var body struct {
			Error struct {
				Details struct {
					Verdict MoveVerdict `json:"verdict"`
				} `json:"details"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.False(t, body.Error.Details.Verdict.Eligible)
		assert.Contains(t, body.Error.Details.Verdict.BundleUnavailable, experiments.AgentMoveBundle)
	})

	t.Run("experiment on with GCS and AgentMoveBundle returns transferMode bundle", func(t *testing.T) {
		f := setup(t, true)
		f.disp.moveBundlePrecheckFn = func(_ context.Context, _ *store.Agent, lim api.MoveBundleLimits, targets []string) (*api.MoveBundlePrecheckResponse, error) {
			assert.NoError(t, lim.Validate())
			assert.NotNil(t, targets)
			return &api.MoveBundlePrecheckResponse{
				Durable:   true,
				Workspace: api.MoveBundleRootEstimate{Bytes: 8 << 20, Entries: 120},
				Home:      api.MoveBundleRootEstimate{Bytes: 2 << 20, Entries: 40},
			}, nil
		}
		rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/reincarnate",
			ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp ReincarnateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotNil(t, resp.MoveVerdict)
		assert.True(t, resp.MoveVerdict.Eligible)
		assert.Equal(t, MoveTransferModeBundle, resp.MoveVerdict.TransferMode)
		require.NotNil(t, resp.MoveVerdict.BundleEstimate)
		assert.Equal(t, int64(8<<20), resp.MoveVerdict.BundleEstimate.WorkspaceBytes)
		assert.Equal(t, int64(2<<20), resp.MoveVerdict.BundleEstimate.HomeBytes)
	})
}
