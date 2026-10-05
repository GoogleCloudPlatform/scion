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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spacesCountingStore counts the project list calls the spaces handler
// makes.
type spacesCountingStore struct {
	store.Store
	mu        sync.Mutex
	list      int
	summaries int
}

func (s *spacesCountingStore) ListProjects(ctx context.Context, f store.ProjectFilter, o store.ListOptions) (*store.ListResult[store.Project], error) {
	s.mu.Lock()
	s.list++
	s.mu.Unlock()
	return s.Store.ListProjects(ctx, f, o)
}

func (s *spacesCountingStore) ListProjectSummaries(ctx context.Context, f store.ProjectFilter, o store.ListOptions) (*store.ListResult[store.Project], error) {
	s.mu.Lock()
	s.summaries++
	s.mu.Unlock()
	return s.Store.ListProjectSummaries(ctx, f, o)
}

// spacesCountingWCS counts the topic and read-state reads the spaces
// handler makes.
type spacesCountingWCS struct {
	WebChatStore
	mu            sync.Mutex
	listTopics    int
	topicsBatch   int
	getReadStates int
	// failTopicsBatch and failReadStates make the batched reads fail.
	failTopicsBatch bool
	failReadStates  bool
}

func (w *spacesCountingWCS) ListTopics(ctx context.Context, projectID string) ([]WebChatTopic, error) {
	w.mu.Lock()
	w.listTopics++
	w.mu.Unlock()
	return w.WebChatStore.ListTopics(ctx, projectID)
}

func (w *spacesCountingWCS) ListTopicsByProjects(ctx context.Context, ids []string) ([]WebChatTopic, error) {
	w.mu.Lock()
	w.topicsBatch++
	fail := w.failTopicsBatch
	w.mu.Unlock()
	if fail {
		return nil, errors.New("injected topics batch failure")
	}
	return w.WebChatStore.ListTopicsByProjects(ctx, ids)
}

func (w *spacesCountingWCS) GetReadStates(ctx context.Context, userID string, keys []string) ([]WebChatReadState, error) {
	w.mu.Lock()
	w.getReadStates++
	fail := w.failReadStates
	w.mu.Unlock()
	if fail {
		return nil, errors.New("injected read states failure")
	}
	return w.WebChatStore.GetReadStates(ctx, userID, keys)
}

func (w *spacesCountingWCS) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.listTopics, w.topicsBatch, w.getReadStates = 0, 0, 0
}

// legacyChatSpaces is the spaces rollup as handleChatSpaces computed it
// before the batched rewrite: the enriched ListProjects, the full project
// capability set, and per-project topic and read-state reads. It is the
// reference the new handler must match.
func legacyChatSpaces(t *testing.T, srv *Server, wcs WebChatStore, identity UserIdentity) []chatSpaceEntry {
	t.Helper()
	ctx := context.Background()
	all, err := srv.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 1000})
	require.NoError(t, err)
	resources := make([]Resource, len(all.Items))
	for i := range all.Items {
		resources[i] = projectResource(&all.Items[i])
	}
	caps := srv.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "project")

	spaces := []chatSpaceEntry{}
	for i, p := range all.Items {
		if !capabilityAllows(caps[i], ActionRead) {
			continue
		}
		topics, _ := wcs.ListTopics(ctx, p.ID)
		keys := make([]string, 0, len(topics))
		for _, tp := range topics {
			keys = append(keys, tp.ID)
		}
		unread := 0
		if len(keys) > 0 {
			states, _ := wcs.GetReadStates(ctx, identity.ID(), keys)
			readMap := make(map[string]WebChatReadState, len(states))
			for _, rs := range states {
				readMap[rs.ConversationKey] = rs
			}
			for _, tp := range topics {
				rs, ok := readMap[tp.ID]
				if ok && rs.Muted {
					continue
				}
				if !ok || rs.LastReadMessageID == "" || (tp.LastMessageID != "" && tp.LastMessageID != rs.LastReadMessageID) {
					if tp.LastMessageID != "" {
						unread++
					}
				}
			}
		}
		spaces = append(spaces, chatSpaceEntry{
			ProjectID:   p.ID,
			ProjectName: p.Name,
			ProjectSlug: p.Slug,
			Emoji:       p.Annotations[spaceEmojiAnnotationKey],
			ThreadCount: len(topics),
			UnreadCount: unread,
		})
	}
	return spaces
}

type spacesPerfFixture struct {
	srv      *Server
	s        store.Store
	wcs      *spacesCountingWCS
	member   *store.User
	admin    *store.User
	projects []string // every project the fixture created
}

// newSpacesPerfFixture builds a hub with several projects, only some of
// which the member can read, carrying threads in every read, unread,
// muted and empty combination, an emoji annotation, and an agent and a
// broker contributor so the enriched project list has work to do.
func newSpacesPerfFixture(t *testing.T) *spacesPerfFixture {
	t.Helper()
	srv, s, owner, member, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	base := NewWebChatStore(db, "sqlite3")
	require.NoError(t, base.Init())
	wcs := &spacesCountingWCS{WebChatStore: base}
	srv.SetWebChatStore(wcs)

	admin, err := s.GetUser(ctx, tid("msg-superadmin"))
	require.NoError(t, err)

	f := &spacesPerfFixture{srv: srv, s: s, wcs: wcs, member: member, admin: admin}
	f.projects = append(f.projects, projectID)

	// The member's project carries an emoji and an agent.
	p, err := s.GetProject(ctx, projectID)
	require.NoError(t, err)
	p.Annotations = map[string]string{spaceEmojiAnnotationKey: "🚀"}
	require.NoError(t, s.UpdateProject(ctx, p))
	createSpaceMembersAgents(t, s, projectID, member.ID, "spaces-perf", 2)

	// Projects the member is not a member of.
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("spaces-perf-other-%d", i)
		op := &store.Project{
			ID: tid(name), Name: name, Slug: name,
			OwnerID: owner.ID, CreatedBy: owner.ID,
			Created: time.Now(), Updated: time.Now(),
		}
		require.NoError(t, s.CreateProject(ctx, op))
		srv.seedProjectCreatorMembership(ctx, op)
		f.projects = append(f.projects, op.ID)
	}

	// Threads: f.projects[0] gets read, unread, muted-unread and never-read
	// threads; [1] gets one unread thread; [2] one thread with no messages;
	// [3] and [4] none.
	base0 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	mk := func(projectID, name string) string {
		id := tid(projectID + "-" + name)
		require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
			ID: id, ProjectID: projectID, Name: name,
			CreatedBy: member.ID, CreatedAt: base0,
		}))
		return id
	}
	touch := func(topicID, msg string) {
		require.NoError(t, wcs.TouchTopicActivity(ctx, topicID, tid(msg)))
	}
	read := mk(f.projects[0], "read")
	touch(read, "m-read")
	unread := mk(f.projects[0], "unread")
	touch(unread, "m-unread")
	muted := mk(f.projects[0], "muted")
	touch(muted, "m-muted")
	mk(f.projects[0], "quiet")
	other := mk(f.projects[1], "other")
	touch(other, "m-other")
	mk(f.projects[2], "empty")

	for _, u := range []*store.User{member, admin} {
		require.NoError(t, wcs.SetReadState(ctx, u.ID, read, tid("m-read")))
		require.NoError(t, wcs.SetReadState(ctx, u.ID, unread, tid("m-older")))
		require.NoError(t, wcs.SetMuted(ctx, u.ID, muted, true))
	}
	return f
}

func getSpaces(t *testing.T, srv *Server, u *store.User) chatSpacesResponse {
	t.Helper()
	rec := doRequestAsUser(t, srv, u, http.MethodGet, "/api/v1/chat/spaces", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp chatSpacesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func identityOf(u *store.User) UserIdentity {
	return NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb))
}

// The spaces list must be the same spaces, in the same order, with the same
// counts and emoji as before the batched rewrite, for a member who sees a
// subset of projects and for an admin who sees them all.
func TestChatSpaces_BatchedMatchesLegacy(t *testing.T) {
	f := newSpacesPerfFixture(t)
	for _, u := range []*store.User{f.member, f.admin} {
		t.Run(u.Email, func(t *testing.T) {
			got := getSpaces(t, f.srv, u)
			want := legacyChatSpaces(t, f.srv, f.wcs, identityOf(u))

			stripped := make([]chatSpaceEntry, len(got.Spaces))
			for i, sp := range got.Spaces {
				sp.LastActivityAt = nil
				stripped[i] = sp
			}
			assert.Equal(t, want, stripped)
		})
	}

	member := getSpaces(t, f.srv, f.member)
	admin := getSpaces(t, f.srv, f.admin)
	require.Less(t, len(member.Spaces), len(admin.Spaces),
		"fixture: the member must see fewer spaces than the admin")
	var mine *chatSpaceEntry
	for i := range member.Spaces {
		if member.Spaces[i].ProjectID == f.projects[0] {
			mine = &member.Spaces[i]
		}
	}
	require.NotNil(t, mine, "member's own project missing")
	assert.Equal(t, 4, mine.ThreadCount)
	assert.Equal(t, 1, mine.UnreadCount, "only the unread, unmuted thread with a message counts")
	assert.Equal(t, "🚀", mine.Emoji)
}

// getSpacesRaw returns GET /chat/spaces keyed by project ID, with each
// space's fields left as raw JSON so tests can check field presence.
func getSpacesRaw(t *testing.T, srv *Server, u *store.User) map[string]map[string]json.RawMessage {
	t.Helper()
	rec := doRequestAsUser(t, srv, u, http.MethodGet, "/api/v1/chat/spaces", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var raw struct {
		Spaces []map[string]json.RawMessage `json:"spaces"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	out := make(map[string]map[string]json.RawMessage, len(raw.Spaces))
	for _, sp := range raw.Spaces {
		var id string
		require.NoError(t, json.Unmarshal(sp["projectId"], &id))
		out[id] = sp
	}
	return out
}

// lastActivityAt is the newest lastActivityAt across the space's threads as
// GET .../threads reports them. It is omitted for a space with no threads
// and for a space whose threads have no messages yet, whose activity time
// is unset.
func TestChatSpaces_LastActivityAt(t *testing.T) {
	f := newSpacesPerfFixture(t)
	resp := getSpaces(t, f.srv, f.admin)
	require.NotEmpty(t, resp.Spaces)
	rawByID := getSpacesRaw(t, f.srv, f.admin)

	sawWith, sawWithout := false, false
	for _, sp := range resp.Spaces {
		trec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, "/api/v1/chat/spaces/"+sp.ProjectID+"/threads", nil)
		require.Equal(t, http.StatusOK, trec.Code, trec.Body.String())
		var threads struct {
			Threads []map[string]json.RawMessage `json:"threads"`
		}
		require.NoError(t, json.Unmarshal(trec.Body.Bytes(), &threads))

		var newest time.Time
		var newestRaw json.RawMessage
		for _, th := range threads.Threads {
			var ts time.Time
			require.NoError(t, json.Unmarshal(th["lastActivityAt"], &ts))
			if !ts.IsZero() && (newestRaw == nil || ts.After(newest)) {
				newest, newestRaw = ts, th["lastActivityAt"]
			}
		}

		if newestRaw == nil {
			sawWithout = true
			assert.Nil(t, sp.LastActivityAt, "space %s has no thread activity", sp.ProjectSlug)
			_, present := rawByID[sp.ProjectID]["lastActivityAt"]
			assert.False(t, present, "lastActivityAt must be omitted for %s", sp.ProjectSlug)
			continue
		}
		sawWith = true
		require.NotNil(t, sp.LastActivityAt, "space %s", sp.ProjectSlug)
		assert.True(t, newest.Equal(*sp.LastActivityAt), "space %s: got %v want %v", sp.ProjectSlug, *sp.LastActivityAt, newest)
		assert.JSONEq(t, string(newestRaw), string(rawByID[sp.ProjectID]["lastActivityAt"]),
			"space %s: same wire format as the thread's lastActivityAt", sp.ProjectSlug)
	}
	require.True(t, sawWith && sawWithout, "fixture: need spaces with and without thread activity")
}

// A space whose only threads have never had a message must omit
// lastActivityAt rather than report the zero time, and reports it as soon
// as one of those threads gets a message.
func TestChatSpaces_LastActivityAtOmittedForMessagelessThreads(t *testing.T) {
	f := newSpacesPerfFixture(t)
	ctx := context.Background()

	// f.projects[2] has one thread with no messages; add a second.
	quietProject := f.projects[2]
	second := tid(quietProject + "-second-empty")
	require.NoError(t, f.wcs.CreateTopic(ctx, WebChatTopic{
		ID: second, ProjectID: quietProject, Name: "second-empty",
		CreatedBy: f.admin.ID, CreatedAt: time.Now().UTC(),
	}))

	resp := getSpaces(t, f.srv, f.admin)
	var quiet *chatSpaceEntry
	for i := range resp.Spaces {
		if resp.Spaces[i].ProjectID == quietProject {
			quiet = &resp.Spaces[i]
		}
	}
	require.NotNil(t, quiet, "space missing")
	require.Equal(t, 2, quiet.ThreadCount, "fixture: two message-less threads")
	assert.Nil(t, quiet.LastActivityAt)
	raw := getSpacesRaw(t, f.srv, f.admin)[quietProject]
	_, present := raw["lastActivityAt"]
	assert.False(t, present, "lastActivityAt must be omitted, got %s", raw["lastActivityAt"])

	// The first message gives the space an activity time.
	require.NoError(t, f.wcs.TouchTopicActivity(ctx, second, tid("first-msg")))
	raw = getSpacesRaw(t, f.srv, f.admin)[quietProject]
	require.Contains(t, raw, "lastActivityAt")
	var ts time.Time
	require.NoError(t, json.Unmarshal(raw["lastActivityAt"], &ts))
	assert.False(t, ts.IsZero(), "lastActivityAt must be a real time once a message exists")
}

// The spaces list decides only ActionRead per project, lists projects
// without per-project enrichment, and reads topics and read states in one
// batch each instead of once per project.
func TestChatSpaces_FewerDecisionsAndStoreCalls(t *testing.T) {
	for _, who := range []string{"member", "admin"} {
		t.Run(who, func(t *testing.T) {
			f := newSpacesPerfFixture(t)
			u := f.member
			if who == "admin" {
				u = f.admin
			}
			counting := &spacesCountingStore{Store: f.s}
			f.srv.store = counting
			emitter := &parityRecordingAuditEmitter{}
			f.srv.authzService.SetDecisionAuditEmitter(emitter)
			f.srv.authzService.DecisionAuditSampleRate = 1.0

			nProjects := len(f.projects)
			all, err := f.s.ListProjects(context.Background(), store.ProjectFilter{}, store.ListOptions{Limit: 1000})
			require.NoError(t, err)
			require.Equal(t, nProjects, len(all.Items), "fixture: unexpected extra projects")

			resp := getSpaces(t, f.srv, u)
			newDecisions := countProjectDecisions(emitter.snapshot())
			newListTopics, newTopicsBatch, newReadStates := f.wcs.listTopics, f.wcs.topicsBatch, f.wcs.getReadStates

			f.wcs.reset()
			before := len(emitter.snapshot())
			legacy := legacyChatSpaces(t, f.srv, f.wcs, identityOf(u))
			legacyDecisions := countProjectDecisions(emitter.snapshot()[before:])

			k := len(ResourceActions["project"])
			require.Greater(t, k, 1)
			t.Logf("%s: visible=%d/%d project decisions legacy=%d new=%d; topic reads legacy=%d new=%d; read-state reads legacy=%d new=%d",
				who, len(legacy), nProjects, legacyDecisions, newDecisions,
				f.wcs.listTopics, newTopicsBatch, f.wcs.getReadStates, newReadStates)

			assert.Equal(t, nProjects*k, legacyDecisions, "legacy: every project action")
			assert.Equal(t, nProjects, newDecisions, "new: one read decision per project")

			assert.Equal(t, 0, newListTopics, "no per-project topic reads")
			assert.Equal(t, 1, newTopicsBatch, "one batched topic read")
			assert.Equal(t, 1, newReadStates, "one batched read-state read")
			assert.Equal(t, len(legacy), f.wcs.listTopics, "legacy: one topic read per visible project")

			assert.Equal(t, 1, counting.summaries, "one summary project list")
			assert.Equal(t, 1, counting.list, "only the legacy reference uses the enriched list")
			assert.Len(t, resp.Spaces, len(legacy))
		})
	}
}

func countProjectDecisions(records []*store.DecisionAuditRecord) int {
	n := 0
	for _, r := range records {
		if r.ResourceType == "project" {
			n++
		}
	}
	return n
}

// Rollups computed across several small batches equal the single-batch
// result, so batch boundaries cannot drop or double-count threads.
func TestChatSpaces_RollupBatchBoundaries(t *testing.T) {
	f := newSpacesPerfFixture(t)
	want := getSpaces(t, f.srv, f.admin)

	// Batch sizes live on this test's own server, not in shared state.
	f.srv.chatSpacesBatch = chatSpacesBatchSizes{topics: 2, readStates: 2}

	f.wcs.reset()
	got := getSpaces(t, f.srv, f.admin)
	assert.Equal(t, want, got)
	assert.Equal(t, (len(want.Spaces)+1)/2, f.wcs.topicsBatch, "topic reads chunked by project")
	assert.Equal(t, 3, f.wcs.getReadStates, "six threads in read-state chunks of two")
}

// ListProjectSummaries returns the same rows as ListProjects, in the same
// order, differing only in the computed fields it leaves zero.
func TestListProjectSummaries_MatchesListProjects(t *testing.T) {
	f := newSpacesPerfFixture(t)
	ctx := context.Background()
	full, err := f.s.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 1000})
	require.NoError(t, err)
	lean, err := f.s.ListProjectSummaries(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 1000})
	require.NoError(t, err)
	require.Equal(t, full.TotalCount, lean.TotalCount)
	require.Len(t, lean.Items, len(full.Items))
	sawAgents := false
	for i := range full.Items {
		fp := full.Items[i]
		sawAgents = sawAgents || fp.AgentCount > 0
		fp.AgentCount, fp.ActiveBrokerCount, fp.ProjectType = 0, 0, ""
		assert.Equal(t, fp, lean.Items[i])
	}
	assert.True(t, sawAgents, "fixture: enrichment must have had something to compute")
}

// A failed batch read is logged as a warning and degrades as the
// per-project reads did: a failed topic read leaves its spaces with no
// threads, and a failed read-state read leaves every messaged thread
// unread. The list itself still succeeds.
func TestChatSpaces_BatchReadFailureIsLogged(t *testing.T) {
	t.Run("topics", func(t *testing.T) {
		f := newSpacesPerfFixture(t)
		f.wcs.failTopicsBatch = true
		logs := captureSpaceMembersLogs(t)

		resp := getSpaces(t, f.srv, f.admin)
		require.Len(t, resp.Spaces, len(f.projects))
		for _, sp := range resp.Spaces {
			assert.Zero(t, sp.ThreadCount, "space %s", sp.ProjectSlug)
			assert.Nil(t, sp.LastActivityAt, "space %s", sp.ProjectSlug)
		}
		assert.Contains(t, logs.String(), "chat spaces: batched topic read failed")
		assert.Contains(t, logs.String(), "injected topics batch failure")
	})
	t.Run("read states", func(t *testing.T) {
		f := newSpacesPerfFixture(t)
		f.wcs.failReadStates = true
		logs := captureSpaceMembersLogs(t)

		resp := getSpaces(t, f.srv, f.admin)
		for _, sp := range resp.Spaces {
			if sp.ProjectID == f.projects[0] {
				assert.Equal(t, 3, sp.UnreadCount, "every messaged thread is unread without read state")
			}
		}
		assert.Contains(t, logs.String(), "chat spaces: batched read-state read failed")
		assert.Contains(t, logs.String(), "injected read states failure")
	})
}
