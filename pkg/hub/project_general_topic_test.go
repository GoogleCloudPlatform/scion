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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// generalTopicServer returns a test server with a SQLite webchat store.
func generalTopicServer(t *testing.T) (*Server, store.Store, WebChatStore) {
	t.Helper()
	srv, s := testServer(t)
	db := openTestMemorySQLite(t, "sqlite3")
	wcs := NewWebChatStore(db, "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)
	return srv, s, wcs
}

// countGeneral returns the number of active topics and of #general topics.
func countGeneral(t *testing.T, wcs WebChatStore, projectID string) (total, general int) {
	t.Helper()
	topics, err := wcs.ListTopics(context.Background(), projectID)
	require.NoError(t, err)
	for _, tp := range topics {
		if tp.IsGeneral {
			general++
		}
	}
	return len(topics), general
}

func decodeProject(t *testing.T, body []byte) store.Project {
	t.Helper()
	var p store.Project
	require.NoError(t, json.Unmarshal(body, &p))
	require.NotEmpty(t, p.ID)
	return p
}

func TestGeneralTopic_PlainCreate(t *testing.T) {
	srv, _, wcs := generalTopicServer(t)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
		Name:      "General Plain",
		GitRemote: "github.com/org/general-plain",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	p := decodeProject(t, rec.Body.Bytes())

	total, general := countGeneral(t, wcs, p.ID)
	assert.Equal(t, 1, total)
	assert.Equal(t, 1, general)
}

func TestGeneralTopic_Register_RetryNoDuplicate(t *testing.T) {
	srv, _, wcs := generalTopicServer(t)
	req := RegisterProjectRequest{
		ID:        api.NewUUID(),
		Name:      "General Register",
		GitRemote: "github.com/org/general-register",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", req)
	require.Less(t, rec.Code, 300, rec.Body.String())
	// A retried register resolves the existing project.
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", req)
	require.Less(t, rec.Code, 300, rec.Body.String())

	total, general := countGeneral(t, wcs, req.ID)
	assert.Equal(t, 1, total)
	assert.Equal(t, 1, general)
}

func TestGeneralTopic_Clone(t *testing.T) {
	srv, s, wcs := generalTopicServer(t)
	src := createSourceProject(t, srv, s)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
		map[string]string{"name": "General Clone"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	clone := decodeProject(t, rec.Body.Bytes())

	total, general := countGeneral(t, wcs, clone.ID)
	assert.Equal(t, 1, total)
	assert.Equal(t, 1, general)
}

func TestGeneralTopic_CreateFromTemplate(t *testing.T) {
	srv, s, wcs := generalTopicServer(t)
	ctx := context.Background()
	tmpl := &store.Project{
		ID:        api.NewUUID(),
		Name:      "Some Template",
		Slug:      "some-template",
		GitRemote: "github.com/org/some-template",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
		Labels:    map[string]string{store.LabelTemplate: "true"},
	}
	require.NoError(t, s.CreateProject(ctx, tmpl))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+tmpl.ID+"/clone",
		map[string]string{"name": "From Template"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	p := decodeProject(t, rec.Body.Bytes())
	require.False(t, p.IsTemplate())

	total, general := countGeneral(t, wcs, p.ID)
	assert.Equal(t, 1, total)
	assert.Equal(t, 1, general)
	// The template itself stays without threads.
	total, _ = countGeneral(t, wcs, tmpl.ID)
	assert.Equal(t, 0, total)
}

func TestGeneralTopic_CloneAsTemplate_NoTopic(t *testing.T) {
	srv, s, wcs := generalTopicServer(t)
	src := createSourceProject(t, srv, s)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
		map[string]interface{}{"name": "New Template", "asTemplate": true})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	p := decodeProject(t, rec.Body.Bytes())
	require.True(t, p.IsTemplate())

	total, _ := countGeneral(t, wcs, p.ID)
	assert.Equal(t, 0, total)
}

func TestGeneralTopic_EnsureIdempotent_SkipsTemplate(t *testing.T) {
	srv, _, wcs := generalTopicServer(t)
	ctx := context.Background()

	p := &store.Project{ID: api.NewUUID(), Name: "Idem", Slug: "idem"}
	srv.ensureProjectGeneralTopic(ctx, p)
	srv.ensureProjectGeneralTopic(ctx, p)
	total, general := countGeneral(t, wcs, p.ID)
	assert.Equal(t, 1, total)
	assert.Equal(t, 1, general)

	tmpl := &store.Project{ID: api.NewUUID(), Name: "T", Slug: "t",
		Labels: map[string]string{store.LabelTemplate: "true"}}
	srv.ensureProjectGeneralTopic(ctx, tmpl)
	total, _ = countGeneral(t, wcs, tmpl.ID)
	assert.Equal(t, 0, total)
}

func TestGeneralTopic_ListThreads_BackfillsMissing(t *testing.T) {
	srv, s, wcs := generalTopicServer(t)
	ctx := context.Background()
	// A project created before every path ensured #general.
	p := &store.Project{ID: api.NewUUID(), Name: "Legacy", Slug: "legacy",
		OwnerID: DevUserID, CreatedBy: DevUserID}
	require.NoError(t, s.CreateProject(ctx, p))

	for range 2 {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces/"+p.ID+"/threads", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp chatTopicListResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.Len(t, resp.Threads, 1)
		assert.True(t, resp.Threads[0].IsGeneral)
	}
	total, general := countGeneral(t, wcs, p.ID)
	assert.Equal(t, 1, total)
	assert.Equal(t, 1, general)
}

func TestGeneralTopic_ListThreads_DoesNotResurrectDeletedGeneral(t *testing.T) {
	srv, s, wcs := generalTopicServer(t)
	ctx := context.Background()
	p := &store.Project{ID: api.NewUUID(), Name: "Deleted", Slug: "deleted-general",
		OwnerID: DevUserID, CreatedBy: DevUserID}
	require.NoError(t, s.CreateProject(ctx, p))
	genID, _, err := wcs.EnsureGeneralTopic(ctx, p.ID, DevUserID)
	require.NoError(t, err)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/spaces/"+p.ID+"/threads",
		map[string]string{"name": "other"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.NoError(t, wcs.DeleteTopic(ctx, genID))

	rec = doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces/"+p.ID+"/threads", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	total, general := countGeneral(t, wcs, p.ID)
	assert.Equal(t, 1, total)
	assert.Equal(t, 0, general)
}

func TestGeneralTopic_Template_NoBackfillNoThreads(t *testing.T) {
	srv, s, wcs := generalTopicServer(t)
	ctx := context.Background()
	tmpl := &store.Project{ID: api.NewUUID(), Name: "Tmpl", Slug: "tmpl-nothreads",
		OwnerID: DevUserID, CreatedBy: DevUserID,
		Labels: map[string]string{store.LabelTemplate: "true"}}
	require.NoError(t, s.CreateProject(ctx, tmpl))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces/"+tmpl.ID+"/threads", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/chat/spaces/"+tmpl.ID+"/threads",
		map[string]string{"name": "nope"})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	total, _ := countGeneral(t, wcs, tmpl.ID)
	assert.Equal(t, 0, total)
}

func TestChatSpaces_ExcludesTemplates(t *testing.T) {
	srv, s, _ := generalTopicServer(t)
	ctx := context.Background()
	regular := &store.Project{ID: api.NewUUID(), Name: "Regular", Slug: "regular-space",
		OwnerID: DevUserID, CreatedBy: DevUserID}
	tmpl := &store.Project{ID: api.NewUUID(), Name: "Git Template", Slug: "git-template",
		OwnerID: DevUserID, CreatedBy: DevUserID,
		Labels: map[string]string{store.LabelTemplate: "true"}}
	require.NoError(t, s.CreateProject(ctx, regular))
	require.NoError(t, s.CreateProject(ctx, tmpl))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp chatSpacesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	ids := map[string]bool{}
	for _, sp := range resp.Spaces {
		ids[sp.ProjectID] = true
	}
	assert.True(t, ids[regular.ID], "regular project must be a space")
	assert.False(t, ids[tmpl.ID], "template must not be a space")
}

// failingGeneralStore fails EnsureGeneralTopic and, when failRelist is set,
// every ListTopics call after the first.
type failingGeneralStore struct {
	WebChatStore
	failEnsure bool
	failRelist bool
	lists      atomic.Int32
}

func (f *failingGeneralStore) EnsureGeneralTopic(ctx context.Context, projectID, createdBy string) (string, bool, error) {
	if f.failEnsure {
		return "", false, errors.New("injected ensure failure")
	}
	return f.WebChatStore.EnsureGeneralTopic(ctx, projectID, createdBy)
}

func (f *failingGeneralStore) ListTopics(ctx context.Context, projectID string) ([]WebChatTopic, error) {
	if f.lists.Add(1) > 1 && f.failRelist {
		return nil, errors.New("injected list failure")
	}
	return f.WebChatStore.ListTopics(ctx, projectID)
}

func getThreadsExpectEmpty(t *testing.T, srv *Server, projectID string) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces/"+projectID+"/threads", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp chatTopicListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Threads)
	assert.Empty(t, resp.Threads)
}

func TestGeneralTopic_ListThreads_EnsureFailure_EmptyAndWarnsOnce(t *testing.T) {
	srv, s, wcs := generalTopicServer(t)
	ctx := context.Background()
	srv.SetWebChatStore(&failingGeneralStore{WebChatStore: wcs, failEnsure: true})
	var logBuf bytes.Buffer
	srv.projectsLog = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	p := &store.Project{ID: api.NewUUID(), Name: "Failing", Slug: "failing-ensure",
		OwnerID: DevUserID, CreatedBy: DevUserID}
	require.NoError(t, s.CreateProject(ctx, p))

	for range 3 {
		getThreadsExpectEmpty(t, srv, p.ID)
	}
	logs := logBuf.String()
	assert.Equal(t, 1, strings.Count(logs, "level=WARN"), logs)
	assert.Equal(t, 2, strings.Count(logs, "level=DEBUG"), logs)
}

func TestGeneralTopic_ListThreads_RelistFailure_ReturnsEmpty(t *testing.T) {
	srv, s, wcs := generalTopicServer(t)
	ctx := context.Background()
	srv.SetWebChatStore(&failingGeneralStore{WebChatStore: wcs, failRelist: true})

	p := &store.Project{ID: api.NewUUID(), Name: "Relist", Slug: "relist-fail",
		OwnerID: DevUserID, CreatedBy: DevUserID}
	require.NoError(t, s.CreateProject(ctx, p))

	getThreadsExpectEmpty(t, srv, p.ID)
	// The backfill itself still happened.
	_, general := countGeneral(t, wcs, p.ID)
	assert.Equal(t, 1, general)
}

// generalTopicEventSpy counts "created" chat topic events per project.
type generalTopicEventSpy struct {
	noopEventPublisher
	mu      sync.Mutex
	created map[string]int
}

func (e *generalTopicEventSpy) PublishChatTopicEvent(_ context.Context, projectID, action string, _ WebChatTopic) {
	if action != "created" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.created[projectID]++
}

func TestGeneralTopic_ListThreads_ParallelFirstOpen(t *testing.T) {
	srv, s, wcs := generalTopicServer(t)
	ctx := context.Background()
	spy := &generalTopicEventSpy{created: map[string]int{}}
	srv.SetEventPublisher(spy)

	p := &store.Project{ID: api.NewUUID(), Name: "Parallel", Slug: "parallel-open",
		OwnerID: DevUserID, CreatedBy: DevUserID}
	require.NoError(t, s.CreateProject(ctx, p))

	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces/"+p.ID+"/threads", nil)
			codes[i] = rec.Code
		}()
	}
	close(start)
	wg.Wait()

	for i, c := range codes {
		assert.Equal(t, http.StatusOK, c, "request %d", i)
	}
	total, general := countGeneral(t, wcs, p.ID)
	assert.Equal(t, 1, total)
	assert.Equal(t, 1, general)
	spy.mu.Lock()
	defer spy.mu.Unlock()
	assert.Equal(t, 1, spy.created[p.ID])
}
