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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer/transfertest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pagingWorkspaceStorage is a GCS-like storage fake for the workspace
// download: List pages like GCSStorage.List (sorted names, StartOffset
// inclusive, MaxResults, NextOffset = the first name of the next page), and
// GenerateSignedURL issues recognisable V4-shaped URLs, which it records.
type pagingWorkspaceStorage struct {
	*mockStorage
	mu     sync.Mutex
	sizes  map[string]int64
	opts   []storage.SignedURLOptions
	issued []string
}

func newPagingWorkspaceStorage() *pagingWorkspaceStorage {
	m := newMockStorage("ws-bucket")
	m.provider = storage.ProviderGCS
	return &pagingWorkspaceStorage{mockStorage: m, sizes: map[string]int64{}}
}

func (p *pagingWorkspaceStorage) put(name string, size int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sizes[name] = size
}

func (p *pagingWorkspaceStorage) List(_ context.Context, opts storage.ListOptions) (*storage.ListResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var names []string
	for n := range p.sizes {
		if strings.HasPrefix(n, opts.Prefix) && n >= opts.StartOffset {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	res := &storage.ListResult{}
	for i, n := range names {
		if opts.MaxResults > 0 && i >= opts.MaxResults {
			res.NextOffset = n
			break
		}
		res.Objects = append(res.Objects, storage.Object{Name: n, Size: p.sizes[n]})
	}
	return res, nil
}

func (p *pagingWorkspaceStorage) GenerateSignedURL(_ context.Context, objectPath string, opts storage.SignedURLOptions) (*storage.SignedURL, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	u := "https://storage.googleapis.com/ws-bucket/" + url.PathEscape(objectPath) +
		"?X-Goog-Algorithm=GOOG4-RSA-SHA256&X-Goog-Credential=hub%40example.iam&X-Goog-Expires=900&X-Goog-Signature=sig" + fmt.Sprint(len(p.issued))
	p.opts = append(p.opts, opts)
	p.issued = append(p.issued, u)
	return &storage.SignedURL{URL: u, Method: opts.Method, Expires: time.Now().Add(opts.Expires)}, nil
}

// workspaceDownloadFixture is a hub with a project, a broker that provides
// for it, and an agent of that broker with a workspace upload.
type workspaceDownloadFixture struct {
	srv     *Server
	s       store.Store
	stor    *pagingWorkspaceStorage
	project *store.Project
	agent   *store.Agent
	broker  string
	prefix  string
}

func newWorkspaceDownloadFixture(t *testing.T) *workspaceDownloadFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	f := &workspaceDownloadFixture{srv: srv, s: s, stor: newPagingWorkspaceStorage(), broker: tid("wd-broker")}
	srv.SetStorage(f.stor)
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{ID: f.broker, Slug: "wd-broker", Name: "wd-broker", Status: store.BrokerStatusOnline}))
	f.project = &store.Project{ID: tid("wd-project"), Slug: "wd-project", Name: "WD Project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, f.project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{ProjectID: f.project.ID, BrokerID: f.broker, BrokerName: "wd-broker", Status: store.BrokerStatusOnline}))
	agentID := tid("wd-agent")
	f.prefix = storage.WorkspaceStoragePath(srv.HubID(), f.project.ID, agentID)
	f.agent = &store.Agent{
		ID: agentID, Slug: "wd-agent", Name: "wd-agent", ProjectID: f.project.ID,
		Phase: string(state.PhaseProvisioning), RuntimeBrokerID: f.broker, StateVersion: 1,
		AppliedConfig: &store.AgentAppliedConfig{WorkspaceStoragePath: f.prefix, WorkspaceStorageBucket: "ws-bucket"},
		Created:       time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, f.agent))
	return f
}

// post sends a workspace download page request as the broker identity
// brokerIdent ("" for none) to the route for brokerID and agentID.
func (f *workspaceDownloadFixture) post(t *testing.T, method, brokerID, agentPath, brokerIdent string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, "/api/v1/runtime-brokers/"+brokerID+"/agents/"+agentPath, bytes.NewReader(raw))
	if brokerIdent != "" {
		req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity(brokerIdent)))
	}
	rr := httptest.NewRecorder()
	f.srv.handleRuntimeBrokerRoutes(rr, req)
	return rr
}

func (f *workspaceDownloadFixture) page(t *testing.T, cursor string) api.WorkspaceDownloadResponse {
	t.Helper()
	rr := f.post(t, http.MethodPost, f.broker, f.agent.ID+"/workspace-download", f.broker, api.WorkspaceDownloadRequest{Cursor: cursor})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var out api.WorkspaceDownloadResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	return out
}

func TestWorkspaceDownload_EndpointAuthzMatrix(t *testing.T) {
	f := newWorkspaceDownloadFixture(t)
	f.stor.put(f.prefix+"/files/main.go", 3)
	ctx := context.Background()
	path := f.agent.ID + "/workspace-download"

	t.Run("the broker itself gets its page", func(t *testing.T) {
		out := f.page(t, "")
		require.Len(t, out.Files, 1)
		assert.Equal(t, "main.go", out.Files[0].Path)
	})
	t.Run("no broker identity gets 403", func(t *testing.T) {
		assert.Equal(t, http.StatusForbidden, f.post(t, http.MethodPost, f.broker, path, "", nil).Code)
	})
	t.Run("another broker's HMAC identity gets 403", func(t *testing.T) {
		assert.Equal(t, http.StatusForbidden, f.post(t, http.MethodPost, f.broker, path, tid("wd-other"), nil).Code)
	})
	t.Run("a user principal gets 403", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-brokers/"+f.broker+"/agents/"+path, nil)
		user := NewAuthenticatedUser(tid("wd-user"), "user@example.com", "User", "admin", "cli")
		req = req.WithContext(contextWithIdentity(req.Context(), user))
		rr := httptest.NewRecorder()
		f.srv.handleRuntimeBrokerRoutes(rr, req)
		assert.Equal(t, http.StatusForbidden, rr.Code)
	})
	t.Run("a broker the agent is not assigned to gets 404", func(t *testing.T) {
		other := tid("wd-unassigned")
		assert.Equal(t, http.StatusNotFound, f.post(t, http.MethodPost, other, path, other, nil).Code)
	})
	t.Run("an unknown agent gets 404", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, f.post(t, http.MethodPost, f.broker, tid("wd-nope")+"/workspace-download", f.broker, nil).Code)
	})
	t.Run("a non-POST gets 405", func(t *testing.T) {
		assert.Equal(t, http.StatusMethodNotAllowed, f.post(t, http.MethodGet, f.broker, path, f.broker, nil).Code)
	})
	t.Run("an extra path segment gets 404", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, f.post(t, http.MethodPost, f.broker, f.agent.ID+"/x/workspace-download", f.broker, nil).Code)
	})
	t.Run("a cursor outside the prefix gets 400", func(t *testing.T) {
		rr := f.post(t, http.MethodPost, f.broker, path, f.broker, api.WorkspaceDownloadRequest{Cursor: "workspaces/other/agent/files/a"})
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})
	t.Run("an oversized body gets 400", func(t *testing.T) {
		rr := f.post(t, http.MethodPost, f.broker, path, f.broker, api.WorkspaceDownloadRequest{Cursor: f.prefix + "/files/" + strings.Repeat("a", 5000)})
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})
	t.Run("local hub storage gets 409 workspace_download_unsupported", func(t *testing.T) {
		f.srv.SetStorage(newMockStorage("local"))
		defer f.srv.SetStorage(f.stor)
		rr := f.post(t, http.MethodPost, f.broker, path, f.broker, nil)
		require.Equal(t, http.StatusConflict, rr.Code)
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
		assert.Equal(t, ErrCodeWorkspaceDownloadUnsupported, resp.Error.Code)
	})
	t.Run("no storage path gets 409", func(t *testing.T) {
		a := *f.agent
		a.ID, a.Slug, a.Name = tid("wd-nopath"), "wd-nopath", "wd-nopath"
		a.AppliedConfig = &store.AgentAppliedConfig{}
		require.NoError(t, f.s.CreateAgent(ctx, &a))
		assert.Equal(t, http.StatusConflict, f.post(t, http.MethodPost, f.broker, a.ID+"/workspace-download", f.broker, nil).Code)
	})
	t.Run("a storage path other than the derived prefix gets 409", func(t *testing.T) {
		a := *f.agent
		a.ID, a.Slug, a.Name = tid("wd-otherpath"), "wd-otherpath", "wd-otherpath"
		a.AppliedConfig = &store.AgentAppliedConfig{WorkspaceStoragePath: f.prefix}
		require.NoError(t, f.s.CreateAgent(ctx, &a))
		assert.Equal(t, http.StatusConflict, f.post(t, http.MethodPost, f.broker, a.ID+"/workspace-download", f.broker, nil).Code)
	})
	t.Run("an agent being deleted gets 409", func(t *testing.T) {
		assert.True(t, workspaceDownloadAgentDeleting(&store.Agent{DeletionState: store.DeletionStateDeleting}))
		assert.True(t, workspaceDownloadAgentDeleting(&store.Agent{DeletedAt: time.Now()}))
		assert.False(t, workspaceDownloadAgentDeleting(&store.Agent{}))
	})
	t.Run("a storage failure is logged as a fixed class", checkWorkspaceDownloadStorageErrorsLoggedAsClasses)
	t.Run("a broker that no longer provides for the project gets 403", func(t *testing.T) {
		require.NoError(t, f.s.RemoveProjectProvider(ctx, f.project.ID, f.broker))
		assert.Equal(t, http.StatusForbidden, f.post(t, http.MethodPost, f.broker, path, f.broker, nil).Code)
	})
}

// TestWorkspaceDownload_PagesThroughNextOffset: 1,200 objects come back in
// three pages chained by NextOffset, each object once, as GET-only URLs
// with the 15 minute expiry.
func TestWorkspaceDownload_PagesThroughNextOffset(t *testing.T) {
	f := newWorkspaceDownloadFixture(t)
	want := map[string]bool{}
	for i := 0; i < 1200; i++ {
		name := fmt.Sprintf("dir/file-%04d.txt", i)
		f.stor.put(f.prefix+"/files/"+name, int64(i))
		want[name] = true
	}
	got := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		out := f.page(t, cursor)
		pages++
		require.LessOrEqual(t, len(out.Files), api.WorkspaceDownloadPageSize)
		assert.WithinDuration(t, time.Now().Add(SignedURLExpiry), out.Expires, time.Minute)
		assert.Equal(t, 900, out.ExpiresInSeconds, "the relative TTL lets the broker measure expiry on its own clock")
		for _, file := range out.Files {
			require.False(t, got[file.Path], "object %s repeated", file.Path)
			got[file.Path] = true
		}
		if out.NextCursor == "" {
			break
		}
		cursor = out.NextCursor
		require.Less(t, pages, 10)
	}
	assert.Equal(t, 3, pages)
	assert.Equal(t, want, got)
	for _, o := range f.stor.opts {
		assert.Equal(t, http.MethodGet, o.Method)
		assert.Equal(t, 15*time.Minute, o.Expires)
	}
}

// TestWorkspaceDownload_AllFilteredPageKeepsCursor: a page whose listed
// objects are all excluded comes back with no files and a next cursor.
func TestWorkspaceDownload_AllFilteredPageKeepsCursor(t *testing.T) {
	f := newWorkspaceDownloadFixture(t)
	for i := 0; i < api.WorkspaceDownloadPageSize; i++ {
		f.stor.put(fmt.Sprintf("%s/files/.scion/f-%04d", f.prefix, i), 1)
	}
	f.stor.put(f.prefix+"/files/zz.txt", 2)
	first := f.page(t, "")
	assert.Empty(t, first.Files)
	require.NotEmpty(t, first.NextCursor)
	second := f.page(t, first.NextCursor)
	require.Len(t, second.Files, 1)
	assert.Equal(t, "zz.txt", second.Files[0].Path)
	assert.Empty(t, second.NextCursor)
}

// TestWorkspaceDownload_FilterMatchesSharedTable is the hub side of the
// shared exclusion table: the endpoint issues URLs for exactly the names
// the table keeps.
func TestWorkspaceDownload_FilterMatchesSharedTable(t *testing.T) {
	f := newWorkspaceDownloadFixture(t)
	cases := transfertest.WorkspaceObjectCases()
	require.NotEmpty(t, cases)
	want := map[string]bool{}
	for _, c := range cases {
		f.stor.put(f.prefix+"/files/"+c.Name, 1)
		if c.Excluded == "" {
			want[c.Name] = true
		}
	}
	require.NotEmpty(t, want)
	got := map[string]bool{}
	for _, file := range f.page(t, "").Files {
		got[file.Path] = true
	}
	assert.Equal(t, want, got)
}

// TestWorkspaceDownload_ResponseUncompressed: the route is in the
// compression bypass, so its signed URLs are never compressed together with
// object names.
func TestWorkspaceDownload_ResponseUncompressed(t *testing.T) {
	path := "/api/v1/runtime-brokers/b-1/agents/a-1/workspace-download"
	assert.True(t, isCompressBypassPath(path))
	assert.True(t, compressRequestBypassed(http.MethodPost, path))
	assert.False(t, isCompressBypassPath("/api/v1/runtime-brokers/b-1/agents/a-1/launch"), "control: the launch report is still compressed")
}

// createDispatchFns are the five create-shaped dispatches, all built by
// buildCreateRequestFor.
func createDispatchFns() map[string]func(context.Context, *HTTPAgentDispatcher, *store.Agent) error {
	return map[string]func(context.Context, *HTTPAgentDispatcher, *store.Agent) error{
		"create": func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
			_, err := d.DispatchAgentCreate(ctx, a)
			return err
		},
		"provision": func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
			return d.DispatchAgentProvision(ctx, a)
		},
		"reprovision": func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
			return d.DispatchAgentReprovision(ctx, a)
		},
		"create with gather": func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
			_, err := d.DispatchAgentCreateWithGather(ctx, a)
			return err
		},
		"finalize env": func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
			_, err := d.DispatchFinalizeEnv(ctx, a, map[string]string{"K": "v"})
			return err
		},
	}
}

// TestBuildCreateRequest_WorkspaceDownloadModeOnEveryCreateDispatch: every
// create-shaped dispatch sends workspaceDownloadMode=signed_urls when the
// agent has a workspace upload and the hub's storage is GCS, and no mode for
// local storage, no storage, or no upload. The bucket is still sent.
func TestBuildCreateRequest_WorkspaceDownloadModeOnEveryCreateDispatch(t *testing.T) {
	gcs := newMockStorage("hub-bucket")
	gcs.provider = storage.ProviderGCS
	for name, dispatch := range createDispatchFns() {
		for _, tc := range []struct {
			label string
			stor  storage.Storage
			path  string
			want  string
		}{
			{"gcs", gcs, "workspaces/p/a", api.WorkspaceDownloadModeSignedURLs},
			{"local", newMockStorage("local"), "workspaces/p/a", ""},
			{"no storage", nil, "workspaces/p/a", ""},
			{"no upload", gcs, "", ""},
		} {
			t.Run(name+"/"+tc.label, func(t *testing.T) {
				f := newTZDispatchFixture(t, "")
				stor := tc.stor
				f.d.SetWorkspaceStorage(func() storage.Storage { return stor })
				f.agent.AppliedConfig.WorkspaceStoragePath = tc.path
				if tc.path != "" {
					f.agent.AppliedConfig.WorkspaceStorageBucket = "hub-bucket"
				}
				require.NoError(t, dispatch(context.Background(), f.d, f.agent))
				req := f.client.lastCreateReq
				require.NotNil(t, req)
				assert.Equal(t, tc.want, req.WorkspaceDownloadMode)
				if tc.path != "" {
					assert.Equal(t, "hub-bucket", req.WorkspaceStorageBucket, "older brokers still get the bucket")
				}
			})
		}
	}
}

// signedURLLeaks returns the signed-URL material found in text: an
// X-Goog- query parameter or any of the issued URLs.
func signedURLLeaks(text string, issued []string) []string {
	var found []string
	if strings.Contains(text, "X-Goog-") {
		found = append(found, "X-Goog-")
	}
	for _, u := range issued {
		if strings.Contains(text, u) {
			found = append(found, u)
		}
	}
	return found
}

// TestCreateRequest_CarriesNoSignedURL is the hub half of the redaction
// check: after the endpoint has issued URLs for the agent, none of them (and
// no X-Goog- parameter) is in the create request of any create-shaped
// dispatch, nor in the dispatcher's debug lines (including "config sent to
// broker"), with debug logging on. The detector itself is checked on a
// planted line first.
func TestCreateRequest_CarriesNoSignedURL(t *testing.T) {
	wf := newWorkspaceDownloadFixture(t)
	wf.stor.put(wf.prefix+"/files/main.go", 3)
	_ = wf.page(t, "")
	require.NotEmpty(t, wf.stor.issued)
	require.NotEmpty(t, signedURLLeaks("planted ?X-Goog-Signature=planted", nil), "detector control: a planted parameter must be found")
	require.NotEmpty(t, signedURLLeaks("planted "+wf.stor.issued[0], wf.stor.issued), "detector control: a planted URL must be found")

	for name, dispatch := range createDispatchFns() {
		t.Run(name, func(t *testing.T) {
			f := newTZDispatchFixture(t, "")
			var logs bytes.Buffer
			f.d = NewHTTPAgentDispatcherWithClient(f.store, f.client, true, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			f.d.SetHubID(tzDispatchHubID)
			f.d.SetWorkspaceStorage(func() storage.Storage { return wf.stor })
			f.agent.AppliedConfig.WorkspaceStoragePath = wf.prefix
			f.agent.AppliedConfig.WorkspaceStorageBucket = "ws-bucket"
			require.NoError(t, dispatch(context.Background(), f.d, f.agent))
			require.NotNil(t, f.client.lastCreateReq)
			require.Equal(t, api.WorkspaceDownloadModeSignedURLs, f.client.lastCreateReq.WorkspaceDownloadMode)
			raw, err := json.Marshal(f.client.lastCreateReq)
			require.NoError(t, err)
			assert.Empty(t, signedURLLeaks(string(raw), wf.stor.issued), "create request")
			require.Contains(t, logs.String(), "buildCreateRequest", "debug lines were written")
			assert.Empty(t, signedURLLeaks(logs.String(), wf.stor.issued), "dispatcher debug lines")
		})
	}
}

// failingListStorage fails List with an error whose text names the bucket
// and the object prefix.
type failingListStorage struct{ *pagingWorkspaceStorage }

func (f *failingListStorage) List(_ context.Context, opts storage.ListOptions) (*storage.ListResult, error) {
	return nil, fmt.Errorf("googleapi: listing gs://ws-bucket/%s: permission denied", opts.Prefix)
}

// checkWorkspaceDownloadStorageErrorsLoggedAsClasses: a storage failure is
// logged as a fixed class, never with the storage error's text (which names
// the bucket and object path). Control: the error text does carry the path.
// It runs as a subtest of TestWorkspaceDownload_EndpointAuthzMatrix.
func checkWorkspaceDownloadStorageErrorsLoggedAsClasses(t *testing.T) {
	f := newWorkspaceDownloadFixture(t)
	f.srv.SetStorage(&failingListStorage{f.stor})
	var logs bytes.Buffer
	f.srv.workspaceLog = slog.New(slog.NewTextHandler(&logs, nil))
	rr := f.post(t, http.MethodPost, f.broker, f.agent.ID+"/workspace-download", f.broker, nil)
	require.Equal(t, http.StatusBadGateway, rr.Code, rr.Body.String())
	_, raw := (&failingListStorage{}).List(context.Background(), storage.ListOptions{Prefix: f.prefix + "/files/"})
	require.Contains(t, raw.Error(), "gs://ws-bucket/", "control: the raw error names the bucket")
	assert.Contains(t, logs.String(), "error_class=storage_error")
	assert.NotContains(t, logs.String(), "gs://")
	assert.NotContains(t, logs.String(), f.prefix)
	assert.NotContains(t, rr.Body.String(), f.prefix)
	assert.Equal(t, "not_found", workspaceStorageErrorClass(storage.ErrNotFound))
	assert.Equal(t, "timeout", workspaceStorageErrorClass(context.DeadlineExceeded))
}
