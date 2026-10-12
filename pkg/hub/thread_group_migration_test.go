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

//go:build !no_sqlite && (!hubshard || hubshard_1)

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the automatic migration of personal thread groups
// (ptone/scion#4424, design section 5a and the phase 5 acceptance list).

// tgmEventSpy records chat.thread_group events.
type tgmEventSpy struct {
	noopEventPublisher
	mu     sync.Mutex
	events []tgmEvent
}

type tgmEvent struct {
	projectID string
	action    string
	group     ThreadGroupResponse
}

func (e *tgmEventSpy) PublishChatThreadGroupEvent(_ context.Context, projectID, action string, group ThreadGroupResponse) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, tgmEvent{projectID: projectID, action: action, group: group})
}

func (e *tgmEventSpy) take() []tgmEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.events
	e.events = nil
	return out
}

func tgmSetExperiment(t *testing.T, srv *Server, enabled bool) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("experiments", json.RawMessage(fmt.Sprintf(`{"overrides":{%q:%t}}`, experiments.ChatThreadGroups, enabled)))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	require.Equal(t, enabled, srv.experimentEnabled(experiments.ChatThreadGroups))
}

type tgmFixture struct {
	t    *testing.T
	srv  *Server
	s    store.Store
	wcs  WebChatStore
	proj *store.Project
	spy  *tgmEventSpy
}

func newTGMFixture(t *testing.T) *tgmFixture {
	t.Helper()
	srv, s, wcs, proj := setupSharedChatTest(t)
	spy := &tgmEventSpy{}
	srv.SetEventPublisher(spy)
	tgmSetExperiment(t, srv, true)
	return &tgmFixture{t: t, srv: srv, s: s, wcs: wcs, proj: proj, spy: spy}
}

// project creates another project.
func (f *tgmFixture) project(slug string) *store.Project {
	f.t.Helper()
	p := &store.Project{ID: api.NewUUID(), Name: slug, Slug: slug, Created: time.Now(), Updated: time.Now()}
	require.NoError(f.t, f.s.CreateProject(context.Background(), p))
	return p
}

// topic creates a live thread in projectID and returns its topic ID.
func (f *tgmFixture) topic(projectID, name string) string {
	f.t.Helper()
	id := uuid.NewString()
	require.NoError(f.t, f.wcs.CreateTopic(context.Background(), WebChatTopic{
		ID: id, ProjectID: projectID, Name: name, CreatedBy: "u-setup", CreatedAt: time.Now().UTC(),
	}))
	return id
}

func (f *tgmFixture) conv(topicID string) string {
	f.t.Helper()
	return topicConversationID(f.t, f.wcs, topicID)
}

// tgmPG builds one personal group.
func tgmPG(id, name string, threadIDs ...string) personalThreadGroup {
	if threadIDs == nil {
		threadIDs = []string{}
	}
	return personalThreadGroup{ID: id, Name: name, ThreadIDs: threadIDs}
}

// groupsDoc builds a thread_groups document.
func groupsDoc(t *testing.T, byProject map[string][]personalThreadGroup) string {
	t.Helper()
	type wire struct {
		ID        string   `json:"id"`
		Name      string   `json:"name"`
		ThreadIDs []string `json:"threadIds"`
	}
	doc := map[string][]wire{}
	for p, gs := range byProject {
		for _, g := range gs {
			doc[p] = append(doc[p], wire{ID: g.ID, Name: g.Name, ThreadIDs: g.ThreadIDs})
		}
	}
	b, err := json.Marshal(doc)
	require.NoError(t, err)
	return string(b)
}

func (f *tgmFixture) setPrefs(userID, threadGroups, threadOrder string) {
	f.t.Helper()
	require.NoError(f.t, f.wcs.SetUserPrefs(context.Background(), userID, WebChatUserPrefs{
		SpaceSortMode: "activity", ThreadSortMode: "custom",
		ThreadGroups: threadGroups, ThreadOrder: threadOrder,
	}))
}

func (f *tgmFixture) prefs(userID string) *WebChatUserPrefs {
	f.t.Helper()
	return getPrefs(f.t, f.wcs, userID)
}

func (f *tgmFixture) pass() threadGroupMigrationStats {
	f.t.Helper()
	return f.srv.migrateThreadGroupsPass(context.Background())
}

func (f *tgmFixture) groups(projectID string) []store.ThreadGroup {
	f.t.Helper()
	gs, err := f.s.ListThreadGroups(context.Background(), projectID)
	require.NoError(f.t, err)
	return gs
}

// group returns the project group with the given name key, or nil.
func (f *tgmFixture) group(projectID, name string) *store.ThreadGroup {
	f.t.Helper()
	for _, g := range f.groups(projectID) {
		if g.NameKey == store.ThreadGroupNameKey(name) {
			g := g
			return &g
		}
	}
	return nil
}

// tgmMembers returns a group's member conversation IDs, sorted.
func tgmMembers(g *store.ThreadGroup) []string {
	out := []string{}
	if g == nil {
		return out
	}
	for _, m := range g.Members {
		out = append(out, m.ConversationID)
	}
	sort.Strings(out)
	return out
}

func tgmSorted(ids ...string) []string {
	out := append([]string{}, ids...)
	sort.Strings(out)
	return out
}

func orderOf(t *testing.T, threadOrder, projectID string) []string {
	t.Helper()
	var doc map[string][]string
	require.NoError(t, json.Unmarshal([]byte(threadOrder), &doc))
	return doc[projectID]
}

// --- Flag gate ---------------------------------------------------------------

func TestThreadGroupMigration_FlagOffDoesNothing(t *testing.T) {
	f := newTGMFixture(t)
	t1 := f.topic(f.proj.ID, "t1")
	doc := groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("g1", "Release", t1)}})
	f.setPrefs("u-a", doc, "")

	tgmSetExperiment(t, f.srv, false)
	st := f.pass()
	assert.Zero(t, st.Users)
	assert.Empty(t, f.groups(f.proj.ID))
	assert.False(t, f.prefs("u-a").ThreadGroupsMigrated)
	assert.Empty(t, f.spy.take())
}

// The sweeper idles while the flag is off and migrates within one tick of
// it being turned on, with no restart.
func TestThreadGroupMigration_SweeperStartsWhenFlagTurnsOn(t *testing.T) {
	f := newTGMFixture(t)
	t1 := f.topic(f.proj.ID, "t1")
	f.setPrefs("u-a", groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("g1", "Release", t1)}}), "")
	tgmSetExperiment(t, f.srv, false)

	prev := threadGroupMigrationTick
	threadGroupMigrationTick = 20 * time.Millisecond
	t.Cleanup(func() { threadGroupMigrationTick = prev })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.srv.startThreadGroupMigrationSweeper(ctx)

	time.Sleep(100 * time.Millisecond)
	assert.Empty(t, f.groups(f.proj.ID), "nothing happens while the flag is off")
	assert.False(t, f.prefs("u-a").ThreadGroupsMigrated)

	tgmSetExperiment(t, f.srv, true)
	require.Eventually(t, func() bool { return f.prefs("u-a").ThreadGroupsMigrated }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{f.conv(t1)}, tgmMembers(f.group(f.proj.ID, "Release")))
}

// The first pass runs at once on start, before the first tick.
func TestThreadGroupMigration_SweeperRunsAPassAtStart(t *testing.T) {
	f := newTGMFixture(t)
	t1 := f.topic(f.proj.ID, "t1")
	f.setPrefs("u-a", groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("g1", "Release", t1)}}), "")

	prev := threadGroupMigrationTick
	threadGroupMigrationTick = time.Hour
	t.Cleanup(func() { threadGroupMigrationTick = prev })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.srv.startThreadGroupMigrationSweeper(ctx)
	require.Eventually(t, func() bool { return f.prefs("u-a").ThreadGroupsMigrated }, 5*time.Second, 10*time.Millisecond)
}

// --- Copy rules ---------------------------------------------------------------

// Every user's personal groups become project groups, and personal data is
// kept.
func TestThreadGroupMigration_CopiesEveryUsersGroups(t *testing.T) {
	f := newTGMFixture(t)
	other := f.project("other")
	t1, t2, t3 := f.topic(f.proj.ID, "t1"), f.topic(f.proj.ID, "t2"), f.topic(other.ID, "o1")
	docA := groupsDoc(t, map[string][]personalThreadGroup{
		f.proj.ID: {tgmPG("ga1", "Release", t1)},
		other.ID:  {tgmPG("ga2", "Ops", t3)},
	})
	docB := groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("gb1", "Design", t2)}})
	f.setPrefs("u-a", docA, "")
	f.setPrefs("u-b", docB, "")

	st := f.pass()
	assert.Equal(t, 2, st.Users)
	assert.Equal(t, 2, st.UsersMarked)
	assert.Equal(t, 3, st.GroupsCreated)
	assert.Equal(t, 3, st.ThreadsFiled)

	rel := f.group(f.proj.ID, "Release")
	require.NotNil(t, rel)
	assert.Equal(t, "Release", rel.Name)
	assert.Equal(t, "u-a", rel.CreatedBy)
	assert.Equal(t, store.ThreadGroupCreatorUser, rel.CreatedByKind)
	assert.Equal(t, []string{f.conv(t1)}, tgmMembers(rel))
	assert.Equal(t, "u-a", rel.Members[0].AddedBy)
	assert.Equal(t, []string{f.conv(t2)}, tgmMembers(f.group(f.proj.ID, "Design")))
	assert.Equal(t, []string{f.conv(t3)}, tgmMembers(f.group(other.ID, "Ops")))

	for user, doc := range map[string]string{"u-a": docA, "u-b": docB} {
		p := f.prefs(user)
		assert.True(t, p.ThreadGroupsMigrated)
		assert.Equal(t, doc, p.ThreadGroups, "personal groups are kept unchanged")
		assert.Equal(t, "custom", p.ThreadSortMode)
	}
}

// Rule 2: groups merge by name key; the first user by user ID creates and
// names the group; an existing project group with the key is reused.
func TestThreadGroupMigration_SameNamedGroupsMerge(t *testing.T) {
	f := newTGMFixture(t)
	t1, t2, t3, t4 := f.topic(f.proj.ID, "t1"), f.topic(f.proj.ID, "t2"), f.topic(f.proj.ID, "t3"), f.topic(f.proj.ID, "t4")
	f.setPrefs("u-b", groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("gb", "  RELEASE ", t2)}}), "")
	f.setPrefs("u-a", groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("ga", "Release", t1)}}), "")
	f.setPrefs("u-c", groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("gc", "Design", t3), tgmPG("gc2", "design", t4)}}), "")

	// "Design" already exists as a project group made by someone else.
	existing := &store.ThreadGroup{ProjectID: f.proj.ID, Name: "DESIGN", CreatedBy: "agent-1", CreatedByKind: store.ThreadGroupCreatorAgent}
	require.NoError(t, f.s.CreateThreadGroup(context.Background(), existing))

	st := f.pass()
	assert.Equal(t, 1, st.GroupsCreated)
	assert.Equal(t, 1, st.GroupsReused)
	gs := f.groups(f.proj.ID)
	require.Len(t, gs, 2, "one Release group and the existing Design group")

	rel := f.group(f.proj.ID, "release")
	require.NotNil(t, rel)
	assert.Equal(t, "Release", rel.Name, "named by the first user by user ID")
	assert.Equal(t, "u-a", rel.CreatedBy)
	assert.Equal(t, tgmSorted(f.conv(t1), f.conv(t2)), tgmMembers(rel))

	des := f.group(f.proj.ID, "design")
	require.NotNil(t, des)
	assert.Equal(t, existing.ID, des.ID, "the existing group is reused")
	assert.Equal(t, "DESIGN", des.Name, "the existing name is kept")
	assert.Equal(t, "agent-1", des.CreatedBy)
	assert.Equal(t, tgmSorted(f.conv(t3), f.conv(t4)), tgmMembers(des))
}

// Rule 2: names are sanitized; an empty name is skipped.
func TestThreadGroupMigration_NamesAreSanitized(t *testing.T) {
	f := newTGMFixture(t)
	t1, t2 := f.topic(f.proj.ID, "t1"), f.topic(f.proj.ID, "t2")
	long := strings.Repeat("é", 120)
	f.setPrefs("u-a", groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {
		tgmPG("g1", "\tRe\x00lease\n", t1),
		tgmPG("g2", " \x07 ", t2),
		tgmPG("g3", long, t2),
	}}), "")

	st := f.pass()
	assert.Equal(t, 1, st.Skipped[tgmSkipEmptyName])
	gs := f.groups(f.proj.ID)
	require.Len(t, gs, 2)
	assert.Equal(t, []string{f.conv(t1)}, tgmMembers(f.group(f.proj.ID, "Release")))
	lg := f.group(f.proj.ID, strings.Repeat("é", 100))
	require.NotNil(t, lg, "a long name is truncated to 100 runes")
	assert.Equal(t, strings.Repeat("é", 100), lg.Name)
	assert.Equal(t, []string{f.conv(t2)}, tgmMembers(lg))
}

// Rule 3: a disputed thread goes to the group most users put it in, a tie
// to the alphabetically first name key, and a thread already in a project
// group is never moved.
func TestThreadGroupMigration_VoteTieAndAlreadyFiled(t *testing.T) {
	f := newTGMFixture(t)
	ctx := context.Background()
	t1, t2, t3 := f.topic(f.proj.ID, "t1"), f.topic(f.proj.ID, "t2"), f.topic(f.proj.ID, "t3")

	manual := &store.ThreadGroup{ProjectID: f.proj.ID, Name: "Manual", CreatedBy: "u-z", CreatedByKind: store.ThreadGroupCreatorUser}
	require.NoError(t, f.s.CreateThreadGroup(ctx, manual))
	require.NoError(t, f.s.SetThreadGroupMember(ctx, manual.ID, f.conv(t3), "u-z"))

	// t1: Beta has two votes, Alpha one. t2: one vote each, tie -> alpha.
	// t3: already in Manual.
	f.setPrefs("u-a", groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("a1", "Alpha", t1, t3), tgmPG("a2", "Beta", t2)}}), "")
	f.setPrefs("u-b", groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("b1", "Beta", t1), tgmPG("b2", "Alpha", t2)}}), "")
	f.setPrefs("u-c", groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("c1", "beta", t1)}}), "")

	st := f.pass()
	assert.Equal(t, 1, st.Skipped[tgmSkipAlreadyFiled])
	assert.Equal(t, []string{f.conv(t2)}, tgmMembers(f.group(f.proj.ID, "alpha")), "tie goes to the first name key")
	assert.Equal(t, []string{f.conv(t1)}, tgmMembers(f.group(f.proj.ID, "beta")), "most votes wins")
	assert.Equal(t, []string{f.conv(t3)}, tgmMembers(f.group(f.proj.ID, "manual")), "an already filed thread is never moved")
	b := f.group(f.proj.ID, "beta")
	assert.Equal(t, "u-b", b.Members[0].AddedBy, "added by the first user by ID who put it there")
}

// Rule 1 and 4: deleted, unknown, foreign-project and #general threads and
// projects that no longer exist are skipped, and a group with nothing left
// to file is not created.
func TestThreadGroupMigration_SkipsDeadEntriesAndEmptyGroups(t *testing.T) {
	f := newTGMFixture(t)
	ctx := context.Background()
	other := f.project("other")
	live := f.topic(f.proj.ID, "live")
	deleted := f.topic(f.proj.ID, "deleted")
	require.NoError(t, f.wcs.DeleteTopic(ctx, deleted))
	foreign := f.topic(other.ID, "foreign")
	general, _, err := f.wcs.EnsureGeneralTopic(ctx, f.proj.ID, "u-setup")
	require.NoError(t, err)
	gone := uuid.NewString()

	doc := groupsDoc(t, map[string][]personalThreadGroup{
		f.proj.ID: {
			tgmPG("g1", "Live", live, deleted, foreign, general, "unknown-topic"),
			tgmPG("g2", "Dead", deleted, foreign, general),
			tgmPG("g3", "Empty"),
		},
		gone:       {tgmPG("g4", "Gone", live)},
		"not-uuid": {tgmPG("g5", "Bad", live)},
		other.ID:   {tgmPG("g6", "Elsewhere", live)},
	})
	f.setPrefs("u-a", doc, "")

	st := f.pass()
	assert.Equal(t, 1, st.UsersMarked)
	assert.Equal(t, 1, st.GroupsCreated)
	assert.Equal(t, 1, st.ThreadsFiled)
	assert.Equal(t, 2, st.Skipped[tgmSkipMissingProject])
	assert.Equal(t, 2, st.Skipped[tgmSkipGeneralThread])
	// deleted x2, foreign x2, unknown x1, and "live" filed under the other project
	assert.Equal(t, 6, st.Skipped[tgmSkipUnresolvedThread])
	assert.Equal(t, 3, st.Skipped[tgmSkipNothingToFile], "Dead, Empty and Elsewhere are not created")

	gs := f.groups(f.proj.ID)
	require.Len(t, gs, 1)
	assert.Equal(t, "Live", gs[0].Name)
	assert.Equal(t, []string{f.conv(live)}, tgmMembers(&gs[0]))
	assert.Empty(t, f.groups(other.ID))
	assert.True(t, f.prefs("u-a").ThreadGroupsMigrated)
}

// A topic whose conversation is deleted is skipped.
func TestThreadGroupMigration_SkipsDeletedConversation(t *testing.T) {
	f := newTGMFixture(t)
	ctx := context.Background()
	t1 := f.topic(f.proj.ID, "t1")
	conv, err := f.s.GetConversation(ctx, f.conv(t1))
	require.NoError(t, err)
	now := time.Now()
	conv.DeletedAt = &now
	require.NoError(t, f.s.UpdateConversation(ctx, conv))
	f.setPrefs("u-a", groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("g1", "Release", t1)}}), "")

	st := f.pass()
	assert.Equal(t, 1, st.Skipped[tgmSkipDeadConversation])
	assert.Empty(t, f.groups(f.proj.ID))
	assert.True(t, f.prefs("u-a").ThreadGroupsMigrated)
}

// Rule 5: the migration may exceed the per-project limit; nothing is
// truncated.
func TestThreadGroupMigration_LimitIsNotEnforced(t *testing.T) {
	f := newTGMFixture(t)
	ctx := context.Background()
	for i := 0; i < store.MaxThreadGroupsPerProject; i++ {
		require.NoError(t, f.s.CreateThreadGroup(ctx, &store.ThreadGroup{
			ProjectID: f.proj.ID, Name: fmt.Sprintf("existing-%03d", i), CreatedBy: "u-z", CreatedByKind: store.ThreadGroupCreatorUser,
		}))
	}
	t1 := f.topic(f.proj.ID, "t1")
	f.setPrefs("u-a", groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("g1", "One more", t1)}}), "")

	st := f.pass()
	assert.Equal(t, 1, st.GroupsCreated)
	assert.Len(t, f.groups(f.proj.ID), store.MaxThreadGroupsPerProject+1)
}

// Rule 6: each user's threadOrder gains the project group ID directly after
// each personal group ID it replaces; everything else is kept.
func TestThreadGroupMigration_ThreadOrderGainsProjectIDs(t *testing.T) {
	f := newTGMFixture(t)
	other := f.project("other")
	t1, t2, t5 := f.topic(f.proj.ID, "t1"), f.topic(f.proj.ID, "t2"), f.topic(f.proj.ID, "t5")
	doc := groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {
		tgmPG("pg-rel", "Release", t1),
		tgmPG("pg-empty", "Empty"),
		tgmPG("pg-des", "Design", t2),
	}})
	order := fmt.Sprintf(`{%q:[%q,"pg-rel",%q,"pg-empty","pg-des"],%q:["x1","x2"]}`, f.proj.ID, t5, t1, other.ID)
	f.setPrefs("u-a", doc, order)
	// u-b has no order for this project: it is not given one.
	f.setPrefs("u-b", groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("pg-b", "Release", t1)}}), fmt.Sprintf(`{%q:["y"]}`, other.ID))

	f.pass()
	rel := f.group(f.proj.ID, "Release")
	des := f.group(f.proj.ID, "Design")
	require.NotNil(t, rel)
	require.NotNil(t, des)

	pa := f.prefs("u-a")
	assert.Equal(t, []string{t5, "pg-rel", rel.ID, t1, "pg-empty", "pg-des", des.ID}, orderOf(t, pa.ThreadOrder, f.proj.ID))
	assert.Equal(t, []string{"x1", "x2"}, orderOf(t, pa.ThreadOrder, other.ID))
	assert.Equal(t, doc, pa.ThreadGroups)

	pb := f.prefs("u-b")
	assert.Equal(t, fmt.Sprintf(`{%q:["y"]}`, other.ID), pb.ThreadOrder, "unchanged byte for byte")
	assert.True(t, pb.ThreadGroupsMigrated)
}

// Rule 6: a personal group whose name matches an existing project group
// takes that group's position even when it files nothing new.
func TestThreadGroupMigration_ThreadOrderUsesExistingGroup(t *testing.T) {
	f := newTGMFixture(t)
	existing := &store.ThreadGroup{ProjectID: f.proj.ID, Name: "Release", CreatedBy: "u-z", CreatedByKind: store.ThreadGroupCreatorUser}
	require.NoError(t, f.s.CreateThreadGroup(context.Background(), existing))
	f.setPrefs("u-a", groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("pg-rel", "release")}}),
		fmt.Sprintf(`{%q:["pg-rel"]}`, f.proj.ID))

	f.pass()
	assert.Equal(t, []string{"pg-rel", existing.ID}, orderOf(t, f.prefs("u-a").ThreadOrder, f.proj.ID))
}

// Rule 8: one event per group created or changed, none for a group left
// unchanged.
func TestThreadGroupMigration_PublishesOneEventPerGroup(t *testing.T) {
	f := newTGMFixture(t)
	ctx := context.Background()
	t1, t2, t3 := f.topic(f.proj.ID, "t1"), f.topic(f.proj.ID, "t2"), f.topic(f.proj.ID, "t3")
	reused := &store.ThreadGroup{ProjectID: f.proj.ID, Name: "Reused", CreatedBy: "u-z", CreatedByKind: store.ThreadGroupCreatorUser}
	require.NoError(t, f.s.CreateThreadGroup(ctx, reused))
	same := &store.ThreadGroup{ProjectID: f.proj.ID, Name: "Same", CreatedBy: "u-z", CreatedByKind: store.ThreadGroupCreatorUser}
	require.NoError(t, f.s.CreateThreadGroup(ctx, same))
	require.NoError(t, f.s.SetThreadGroupMember(ctx, same.ID, f.conv(t3), "u-z"))

	f.setPrefs("u-a", groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("a1", "New", t1), tgmPG("a2", "Reused", t2), tgmPG("a3", "Same", t3)}}), "")
	f.setPrefs("u-b", groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("b1", "new", t1)}}), "")
	f.spy.take()
	f.pass()

	events := f.spy.take()
	require.Len(t, events, 2)
	byName := map[string]tgmEvent{}
	for _, e := range events {
		assert.Equal(t, f.proj.ID, e.projectID)
		byName[e.group.Name] = e
	}
	require.Contains(t, byName, "New")
	require.Contains(t, byName, "Reused")
	assert.Equal(t, ThreadGroupEventCreated, byName["New"].action)
	assert.Equal(t, ThreadGroupEventUpdated, byName["Reused"].action)
	require.Len(t, byName["New"].group.Threads, 1)
	assert.Equal(t, t1, byName["New"].group.Threads[0].ThreadID)
	assert.Equal(t, f.conv(t1), byName["New"].group.Threads[0].ConversationID)
	assert.Equal(t, "t1", byName["New"].group.Threads[0].Name)

	f.pass()
	assert.Empty(t, f.spy.take(), "a second pass publishes nothing")
}

// --- Idempotency and concurrency ---------------------------------------------

// state is the outcome of a migration with IDs replaced by names, so two
// fixtures can be compared.
func (f *tgmFixture) state(users ...string) string {
	f.t.Helper()
	topics, err := f.wcs.ListTopics(context.Background(), f.proj.ID)
	require.NoError(f.t, err)
	topicName := map[string]string{}
	for _, tp := range topics {
		topicName[tp.ConversationID] = tp.Name
		topicName[tp.ID] = tp.Name
	}
	groupName := map[string]string{}
	var lines []string
	for _, g := range f.groups(f.proj.ID) {
		groupName[g.ID] = "PG:" + g.Name
		var ms []string
		for _, m := range g.Members {
			ms = append(ms, topicName[m.ConversationID]+"@"+m.AddedBy)
		}
		sort.Strings(ms)
		lines = append(lines, fmt.Sprintf("group %s by %s: %v", g.Name, g.CreatedBy, ms))
	}
	for _, u := range users {
		p := f.prefs(u)
		var order []string
		if p.ThreadOrder != "" {
			for _, id := range orderOf(f.t, p.ThreadOrder, f.proj.ID) {
				switch {
				case groupName[id] != "":
					order = append(order, groupName[id])
				case topicName[id] != "":
					order = append(order, topicName[id])
				default:
					order = append(order, id)
				}
			}
		}
		lines = append(lines, fmt.Sprintf("user %s migrated=%t order=%v", u, p.ThreadGroupsMigrated, order))
	}
	return strings.Join(lines, "\n")
}

// seedScenario gives f a mix of merges, votes, a tie and orders. Topic and
// personal group IDs are per fixture; names are the same.
func seedScenario(t *testing.T, f *tgmFixture) []string {
	t.Helper()
	tp := map[string]string{}
	for _, n := range []string{"t1", "t2", "t3", "t4", "t5"} {
		tp[n] = f.topic(f.proj.ID, n)
	}
	users := []string{"u-a", "u-b", "u-c", "u-d"}
	f.setPrefs("u-a", groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("a1", "Alpha", tp["t1"], tp["t2"]), tgmPG("a2", "Beta", tp["t3"])}}),
		fmt.Sprintf(`{%q:["a2",%q,"a1"]}`, f.proj.ID, tp["t4"]))
	f.setPrefs("u-b", groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("b1", "beta", tp["t1"], tp["t4"]), tgmPG("b2", "Gamma", tp["t5"])}}),
		fmt.Sprintf(`{%q:["b1","b2"]}`, f.proj.ID))
	f.setPrefs("u-c", groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("c1", "ALPHA", tp["t3"]), tgmPG("c2", "Beta", tp["t1"])}}), "")
	f.setPrefs("u-d", groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("d1", "Gamma", tp["t2"])}}),
		fmt.Sprintf(`{%q:["d1"]}`, f.proj.ID))
	return users
}

// A second pass changes nothing, including after the marker is cleared by a
// write that changes thread_groups without changing its meaning.
func TestThreadGroupMigration_SecondPassChangesNothing(t *testing.T) {
	f := newTGMFixture(t)
	users := seedScenario(t, f)
	f.pass()
	want := f.state(users...)
	require.Contains(t, want, "migrated=true")
	f.spy.take()

	st := f.pass()
	assert.Zero(t, st.Users)
	assert.Equal(t, want, f.state(users...))

	// Re-serialize every user's thread_groups: the marker is cleared, the
	// next pass re-processes them and still changes nothing.
	for _, u := range users {
		p := f.prefs(u)
		f.setPrefs(u, " "+p.ThreadGroups, p.ThreadOrder)
		require.False(t, f.prefs(u).ThreadGroupsMigrated)
	}
	st = f.pass()
	assert.Equal(t, len(users), st.UsersMarked)
	assert.Zero(t, st.GroupsCreated)
	assert.Zero(t, st.ThreadsFiled)
	assert.Equal(t, want, f.state(users...))
	assert.Empty(t, f.spy.take(), "nothing changed, so nothing is published")
}

// Two passes running at once produce the same result as one.
func TestThreadGroupMigration_ConcurrentPassesMatchOnePass(t *testing.T) {
	ref := newTGMFixture(t)
	users := seedScenario(t, ref)
	ref.pass()
	want := ref.state(users...)

	f := newTGMFixture(t)
	seedScenario(t, f)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.srv.migrateThreadGroupsPass(context.Background())
		}()
	}
	wg.Wait()
	// A pass that lost a race on a busy database leaves its users for the
	// next pass; let it run, as the next tick would.
	f.pass()
	assert.Equal(t, want, f.state(users...))
	filed := map[string]int{}
	for _, g := range f.groups(f.proj.ID) {
		for _, m := range g.Members {
			filed[m.ConversationID]++
		}
	}
	assert.Len(t, filed, 5)
	for conv, n := range filed {
		assert.Equal(t, 1, n, "thread %s is filed once", conv)
	}
}

// --- Marker reset and retries ---------------------------------------------------

// The user-prefs PUT clears the marker when it changes thread_groups, and
// only then.
func TestThreadGroupMigration_UserPrefsPutResetsMarker(t *testing.T) {
	f := newTGMFixture(t)
	t1 := f.topic(f.proj.ID, "t1")
	doc := groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("g1", "Release", t1)}})
	put := func(threadGroups, threadOrder string) {
		t.Helper()
		rec := doRequest(t, f.srv, http.MethodPut, "/api/v1/chat/user-prefs", map[string]string{
			"threadSortMode": "custom", "threadGroups": threadGroups, "threadOrder": threadOrder,
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}

	put(doc, "")
	assert.False(t, f.prefs(DevUserID).ThreadGroupsMigrated)
	f.pass()
	require.True(t, f.prefs(DevUserID).ThreadGroupsMigrated)

	put(doc, fmt.Sprintf(`{%q:["g1"]}`, f.proj.ID))
	assert.True(t, f.prefs(DevUserID).ThreadGroupsMigrated, "a PUT that keeps thread_groups keeps the marker")

	doc2 := groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("g1", "Release", t1), tgmPG("g2", "New")}})
	put(doc2, "")
	assert.False(t, f.prefs(DevUserID).ThreadGroupsMigrated, "a PUT that changes thread_groups clears the marker")

	rec := doRequest(t, f.srv, http.MethodGet, "/api/v1/chat/user-prefs", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "igrated", "the marker is not part of the API")
}

// A personal edit made while the flag is off is migrated on the next pass
// after it is turned on.
func TestThreadGroupMigration_EditWhileFlagOffMigratedLater(t *testing.T) {
	f := newTGMFixture(t)
	t1, t2 := f.topic(f.proj.ID, "t1"), f.topic(f.proj.ID, "t2")
	f.setPrefs("u-a", groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("g1", "Release", t1)}}), "")
	f.pass()
	require.True(t, f.prefs("u-a").ThreadGroupsMigrated)

	tgmSetExperiment(t, f.srv, false)
	f.setPrefs("u-a", groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("g1", "Release", t1), tgmPG("g2", "Later", t2)}}), "")
	f.pass()
	assert.Nil(t, f.group(f.proj.ID, "Later"))
	assert.False(t, f.prefs("u-a").ThreadGroupsMigrated)

	tgmSetExperiment(t, f.srv, true)
	f.pass()
	assert.Equal(t, []string{f.conv(t2)}, tgmMembers(f.group(f.proj.ID, "Later")))
	assert.Equal(t, []string{f.conv(t1)}, tgmMembers(f.group(f.proj.ID, "Release")))
	assert.True(t, f.prefs("u-a").ThreadGroupsMigrated)
}

// failingTopicsStore fails ListTopics for one project while fail is set.
type failingTopicsStore struct {
	WebChatStore
	projectID string
	fail      atomic.Bool
}

func (s *failingTopicsStore) ListTopics(ctx context.Context, projectID string) ([]WebChatTopic, error) {
	if s.fail.Load() && projectID == s.projectID {
		return nil, errors.New("injected list topics failure")
	}
	return s.WebChatStore.ListTopics(ctx, projectID)
}

// A failed project is retried on the next pass, and its users stay unmarked
// until then; other projects' users are not held back.
func TestThreadGroupMigration_FailedProjectIsRetried(t *testing.T) {
	f := newTGMFixture(t)
	other := f.project("other")
	t1, o1 := f.topic(f.proj.ID, "t1"), f.topic(other.ID, "o1")
	wrapped := &failingTopicsStore{WebChatStore: f.wcs, projectID: f.proj.ID}
	wrapped.fail.Store(true)
	f.srv.SetWebChatStore(wrapped)

	f.setPrefs("u-a", groupsDoc(t, map[string][]personalThreadGroup{f.proj.ID: {tgmPG("g1", "Release", t1)}, other.ID: {tgmPG("g2", "Ops", o1)}}), "")
	f.setPrefs("u-b", groupsDoc(t, map[string][]personalThreadGroup{other.ID: {tgmPG("g3", "Ops", o1)}}), "")

	st := f.pass()
	assert.Equal(t, 1, st.FailedProjects)
	assert.False(t, f.prefs("u-a").ThreadGroupsMigrated, "a user with a failed project stays unmarked")
	assert.True(t, f.prefs("u-b").ThreadGroupsMigrated)
	assert.Empty(t, f.groups(f.proj.ID))
	assert.Equal(t, []string{f.conv(o1)}, tgmMembers(f.group(other.ID, "Ops")))

	wrapped.fail.Store(false)
	st = f.pass()
	assert.Zero(t, st.FailedProjects)
	assert.True(t, f.prefs("u-a").ThreadGroupsMigrated)
	assert.Equal(t, []string{f.conv(t1)}, tgmMembers(f.group(f.proj.ID, "Release")))
	assert.Len(t, f.groups(other.ID), 1)
}

// A thread_groups document that is not a JSON object is marked done and
// skipped, so it is not re-read every pass.
func TestThreadGroupMigration_InvalidPrefsAreMarked(t *testing.T) {
	f := newTGMFixture(t)
	f.setPrefs("u-a", `[1,2]`, "")
	f.setPrefs("u-b", `{"p":"not-an-array"}`, "")
	st := f.pass()
	assert.Equal(t, 1, st.Skipped[tgmSkipInvalidPrefs])
	assert.Equal(t, 2, st.UsersMarked)
	assert.True(t, f.prefs("u-a").ThreadGroupsMigrated)
	assert.Equal(t, `[1,2]`, f.prefs("u-a").ThreadGroups)
}

// --- Unit tests -----------------------------------------------------------------

func TestParsePersonalThreadGroups_MatchesTheRail(t *testing.T) {
	got, err := parsePersonalThreadGroups(`{
		"p1": [
			{"id":"g1","name":"A","threadIds":["t1", 2, "t2", null]},
			{"id":"g2","name":"B"},
			{"id":3,"name":"C","threadIds":[]},
			{"id":"g4","name":"D","threadIds":[]},
			"junk"
		],
		"p2": {"not":"array"},
		"p3": []
	}`)
	require.NoError(t, err)
	assert.Equal(t, map[string][]personalThreadGroup{
		"p1": {
			{ID: "g1", Name: "A", ThreadIDs: []string{"t1", "t2"}},
			{ID: "g4", Name: "D"},
		},
	}, got)

	for _, bad := range []string{`[]`, `null`, `"x"`, `{`} {
		_, err := parsePersonalThreadGroups(bad)
		assert.Error(t, err, bad)
	}
}

func TestInsertProjectGroupsInThreadOrder(t *testing.T) {
	personal := map[string][]personalThreadGroup{"p": {
		{ID: "a", Name: "Rel"},
		{ID: "b", Name: " rel "}, // same key as a: inserted once, after a
		{ID: "c", Name: "Other"},
	}}
	ids := map[string]map[string]string{"p": {"rel": "PG1", "other": "PG2"}}

	assert.Equal(t, `{"p":["x","a","PG1","b","c","PG2",7]}`,
		insertProjectGroupsInThreadOrder(`{"p":["x","a","b","c",7]}`, personal, ids))
	// Idempotent: IDs already present are not added again.
	once := insertProjectGroupsInThreadOrder(`{"p":["a","c"]}`, personal, ids)
	assert.Equal(t, `{"p":["a","PG1","c","PG2"]}`, once)
	assert.Equal(t, once, insertProjectGroupsInThreadOrder(once, personal, ids))
	// Nothing to do: returned byte for byte.
	for _, in := range []string{``, `{ "q" : ["a"] }`, `not json`, `{"p":"x"}`, `{"p":["z"]}`} {
		assert.Equal(t, in, insertProjectGroupsInThreadOrder(in, personal, ids), in)
	}
}
