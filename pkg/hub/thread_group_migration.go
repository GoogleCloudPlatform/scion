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
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// Automatic migration of personal thread groups into project thread groups
// (ptone/scion#4424, design section 5a).
//
// Personal groups live in webchat_user_prefs.thread_groups, a JSON document
// {"<projectId>": [{"id", "name", "threadIds": [<topic ID>...]}]}. A
// background sweeper copies them into project thread groups while the
// hub.chat_thread_groups experiment is on. A pass reads every row whose
// thread_groups is non-empty and whose thread_groups_migrated_at is NULL,
// handles the rows one project at a time, then marks each user done.
//
// Every pass is deterministic and idempotent, so several hubs may run passes
// at once: groups are matched on the unique (project_id, name_key) index,
// threads are filed with insert-if-unfiled on the unique conversation_id
// index, and a user is marked with a compare-and-set on the row it read.

// threadGroupMigrationTick is how often the sweeper runs a pass. It is a
// variable so tests can shorten it.
var threadGroupMigrationTick = 5 * time.Minute

// Reasons an entry is skipped, logged as counts (design 5a, Observability).
const (
	tgmSkipInvalidPrefs     = "invalid_prefs"     // thread_groups is not a JSON object
	tgmSkipMissingProject   = "missing_project"   // the project no longer exists
	tgmSkipEmptyName        = "empty_name"        // nothing left after sanitizing the name
	tgmSkipUnresolvedThread = "unresolved_thread" // unknown or deleted topic, or another project's
	tgmSkipGeneralThread    = "general_thread"    // the #general topic
	tgmSkipDeadConversation = "dead_conversation" // the topic's conversation is missing, deleted, not a group or in another project
	tgmSkipAlreadyFiled     = "already_filed"     // already in a project group; never moved
	tgmSkipNothingToFile    = "nothing_to_file"   // a group with no thread left to file is not created
)

// personalThreadGroup is one personal group as the web stores it.
type personalThreadGroup struct {
	ID        string
	Name      string
	ThreadIDs []string
}

// parsePersonalThreadGroups parses a thread_groups document the way the web
// rail does (parseRailPrefs in chat-space-rail.ts): the top level must be an
// object; a project value that is not an array, a group without a string id
// and name and a threadIds array, and a thread ID that is not a string are
// ignored.
func parsePersonalThreadGroups(doc string) (map[string][]personalThreadGroup, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(doc), &raw); err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, errors.New("thread_groups is not an object")
	}
	out := make(map[string][]personalThreadGroup, len(raw))
	for projectKey, val := range raw {
		var items []json.RawMessage
		if err := json.Unmarshal(val, &items); err != nil {
			continue
		}
		var groups []personalThreadGroup
		for _, item := range items {
			var g struct {
				ID        *string           `json:"id"`
				Name      *string           `json:"name"`
				ThreadIDs []json.RawMessage `json:"threadIds"`
			}
			if err := json.Unmarshal(item, &g); err != nil || g.ID == nil || g.Name == nil || g.ThreadIDs == nil {
				continue
			}
			pg := personalThreadGroup{ID: *g.ID, Name: *g.Name}
			for _, t := range g.ThreadIDs {
				var id *string // a pointer, so null is told apart from ""
				if json.Unmarshal(t, &id) == nil && id != nil {
					pg.ThreadIDs = append(pg.ThreadIDs, *id)
				}
			}
			groups = append(groups, pg)
		}
		if len(groups) > 0 {
			out[projectKey] = groups
		}
	}
	return out, nil
}

// threadGroupMigrationStats are the counts one pass logs. They hold no names
// or IDs.
type threadGroupMigrationStats struct {
	Users          int
	UsersMarked    int
	GroupsCreated  int
	GroupsReused   int
	ThreadsFiled   int
	FailedProjects int
	Skipped        map[string]int
}

func (st *threadGroupMigrationStats) skip(reason string, n int) {
	if n <= 0 {
		return
	}
	if st.Skipped == nil {
		st.Skipped = map[string]int{}
	}
	st.Skipped[reason] += n
}

// tgmUserGroup is one user's personal group in one project.
type tgmUserGroup struct {
	userID  string
	group   personalThreadGroup
	name    string // sanitized
	nameKey string
}

// startThreadGroupMigrationSweeper starts the migration sweeper: a pass at
// once, then one every threadGroupMigrationTick, until ctx is cancelled.
// Each pass checks the experiment, so turning it on starts the migration
// within one tick with no restart.
func (s *Server) startThreadGroupMigrationSweeper(ctx context.Context) {
	if !s.nativeChatEnabled() {
		return
	}
	go func() {
		s.migrateThreadGroupsPass(ctx)
		ticker := time.NewTicker(threadGroupMigrationTick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.migrateThreadGroupsPass(ctx)
			}
		}
	}()
}

// migrateThreadGroupsPass runs one migration pass and returns its counts. It
// does nothing while hub.chat_thread_groups is off.
func (s *Server) migrateThreadGroupsPass(ctx context.Context) threadGroupMigrationStats {
	var st threadGroupMigrationStats
	if !s.experimentEnabled(experiments.ChatThreadGroups) {
		return st
	}
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil || s.store == nil {
		return st
	}
	rows, err := wcs.ListUserPrefsPendingThreadGroupMigration(ctx)
	if err != nil {
		slog.WarnContext(ctx, "thread group migration: listing pending users failed", "error", err)
		return st
	}
	if len(rows) == 0 {
		return st
	}
	st.Users = len(rows)

	// Group every user's personal groups by project. Users are in user ID
	// order (the store sorts them), and each user's groups keep their order.
	byProject := map[string][]tgmUserGroup{}
	parsed := make([]map[string][]personalThreadGroup, len(rows))
	for i, row := range rows {
		groups, err := parsePersonalThreadGroups(row.ThreadGroups)
		if err != nil {
			st.skip(tgmSkipInvalidPrefs, 1)
			continue
		}
		parsed[i] = groups
		for projectKey, list := range groups {
			for _, g := range list {
				name := store.SanitizeThreadGroupName(g.Name)
				byProject[projectKey] = append(byProject[projectKey], tgmUserGroup{
					userID:  row.UserID,
					group:   g,
					name:    name,
					nameKey: store.ThreadGroupNameKey(name),
				})
			}
		}
	}

	projectKeys := make([]string, 0, len(byProject))
	for k := range byProject {
		projectKeys = append(projectKeys, k)
	}
	sort.Strings(projectKeys)

	// groupIDs[project][nameKey] is the project group each name maps to
	// after the project's batch; failed holds the projects to retry.
	groupIDs := map[string]map[string]string{}
	failed := map[string]bool{}
	for _, projectKey := range projectKeys {
		ids, err := s.migrateProjectThreadGroups(ctx, wcs, projectKey, byProject[projectKey], &st)
		if err != nil {
			failed[projectKey] = true
			st.FailedProjects++
			slog.WarnContext(ctx, "thread group migration: a project failed; its users are retried next pass", "error", err)
			continue
		}
		groupIDs[projectKey] = ids
	}

	for i, row := range rows {
		retry := false
		for projectKey := range parsed[i] {
			if failed[projectKey] {
				retry = true
				break
			}
		}
		if retry {
			continue
		}
		newOrder := insertProjectGroupsInThreadOrder(row.ThreadOrder, parsed[i], groupIDs)
		ok, err := wcs.MarkUserPrefsThreadGroupsMigrated(ctx, row.UserID, row.ThreadGroups, row.ThreadOrder, newOrder)
		if err != nil {
			slog.WarnContext(ctx, "thread group migration: marking a user done failed", "error", err)
			continue
		}
		if ok {
			st.UsersMarked++
		}
	}

	attrs := []any{
		"users", st.Users,
		"users_marked", st.UsersMarked,
		"groups_created", st.GroupsCreated,
		"groups_reused", st.GroupsReused,
		"threads_filed", st.ThreadsFiled,
		"failed_projects", st.FailedProjects,
	}
	reasons := make([]string, 0, len(st.Skipped))
	for r := range st.Skipped {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	for _, r := range reasons {
		attrs = append(attrs, "skipped_"+r, st.Skipped[r])
	}
	slog.InfoContext(ctx, "thread group migration: pass complete", attrs...)
	return st
}

// migrateProjectThreadGroups copies one project's personal groups into
// project groups (design 5a, rules 1-5 and 8) and returns, for every name
// key in the batch that has a project group afterwards, that group's ID. An
// error means the project should be retried on the next pass.
func (s *Server) migrateProjectThreadGroups(ctx context.Context, wcs WebChatStore, projectID string, entries []tgmUserGroup, st *threadGroupMigrationStats) (map[string]string, error) {
	totalThreads := 0
	for _, e := range entries {
		totalThreads += len(e.group.ThreadIDs)
	}

	// Rule 1: a project that no longer exists is skipped as a whole.
	if _, err := uuid.Parse(projectID); err != nil {
		st.skip(tgmSkipMissingProject, totalThreads)
		return map[string]string{}, nil
	}
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			st.skip(tgmSkipMissingProject, totalThreads)
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("get project: %w", err)
	}

	// Rule 1: resolve topic IDs within this project only. ListTopics returns
	// the project's live topics, the same rows GetTopicConversationIDInProject
	// resolves, in one query, and also tells #general apart.
	topics, err := wcs.ListTopics(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list topics: %w", err)
	}
	topicByID := make(map[string]WebChatTopic, len(topics))
	for _, t := range topics {
		topicByID[t.ID] = t
	}
	liveConv := map[string]bool{}
	resolve := func(topicID string) (string, string, error) {
		t, ok := topicByID[topicID]
		if !ok || t.ConversationID == "" {
			return "", tgmSkipUnresolvedThread, nil
		}
		if t.IsGeneral {
			return "", tgmSkipGeneralThread, nil
		}
		live, seen := liveConv[t.ConversationID]
		if !seen {
			conv, err := s.store.GetConversation(ctx, t.ConversationID)
			switch {
			case errors.Is(err, store.ErrNotFound):
				live = false
			case err != nil:
				return "", "", fmt.Errorf("get conversation: %w", err)
			default:
				live = conv.DeletedAt == nil && conv.Kind == "group" &&
					conv.ProjectID != nil && *conv.ProjectID == projectID
			}
			liveConv[t.ConversationID] = live
		}
		if !live {
			return "", tgmSkipDeadConversation, nil
		}
		return t.ConversationID, "", nil
	}

	// Rule 2: merge by name key. For each key: the users who had it (the
	// first by user ID creates it and names it) and, per conversation, the
	// users who put it there (rule 3's vote).
	type keyInfo struct {
		name    string
		creator string
		voters  map[string]map[string]bool // conversation ID -> user IDs
	}
	keys := map[string]*keyInfo{}
	for _, e := range entries {
		if e.name == "" {
			st.skip(tgmSkipEmptyName, 1)
			continue
		}
		ki := keys[e.nameKey]
		if ki == nil {
			ki = &keyInfo{name: e.name, creator: e.userID, voters: map[string]map[string]bool{}}
			keys[e.nameKey] = ki
		} else if e.userID < ki.creator {
			ki.name, ki.creator = e.name, e.userID
		}
		for _, topicID := range e.group.ThreadIDs {
			convID, reason, err := resolve(topicID)
			if err != nil {
				return nil, err
			}
			if reason != "" {
				st.skip(reason, 1)
				continue
			}
			if ki.voters[convID] == nil {
				ki.voters[convID] = map[string]bool{}
			}
			ki.voters[convID][e.userID] = true
		}
	}

	existing, err := s.store.ListThreadGroups(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list thread groups: %w", err)
	}
	byKey := make(map[string]store.ThreadGroup, len(existing))
	filed := map[string]bool{}
	for _, g := range existing {
		byKey[g.NameKey] = g
		for _, m := range g.Members {
			filed[m.ConversationID] = true
		}
	}

	// Rule 3: each conversation goes to the key with the most voters; a tie
	// goes to the alphabetically first key. A conversation already in a
	// project group is never moved.
	sortedKeys := make([]string, 0, len(keys))
	for k := range keys {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)
	type placement struct {
		key   string
		votes int
	}
	winner := map[string]placement{}
	for _, k := range sortedKeys {
		for convID, voters := range keys[k].voters {
			if w, ok := winner[convID]; !ok || len(voters) > w.votes {
				winner[convID] = placement{key: k, votes: len(voters)}
			}
		}
	}
	toFile := map[string][]string{} // key -> conversation IDs
	for convID, w := range winner {
		if filed[convID] {
			st.skip(tgmSkipAlreadyFiled, 1)
			continue
		}
		toFile[w.key] = append(toFile[w.key], convID)
	}

	result := map[string]string{}
	createdHere := 0
	for _, k := range sortedKeys {
		convs := toFile[k]
		g, reused := byKey[k]
		if len(convs) == 0 {
			// Rule 4: nothing to file, so no group is created. A group that
			// already exists still takes the users' positions (rule 6).
			if reused {
				result[k] = g.ID
			} else {
				st.skip(tgmSkipNothingToFile, 1)
			}
			continue
		}
		created := false
		if !reused {
			ng := &store.ThreadGroup{
				ProjectID:     projectID,
				Name:          keys[k].name,
				CreatedBy:     keys[k].creator,
				CreatedByKind: store.ThreadGroupCreatorUser,
			}
			err := s.store.CreateThreadGroup(ctx, ng)
			switch {
			case err == nil:
				g, created = *ng, true
			case errors.Is(err, store.ErrAlreadyExists):
				// Another hub (or a person) created it meanwhile: reuse it.
				found, ferr := s.findThreadGroupByKey(ctx, projectID, k)
				if ferr != nil {
					return nil, ferr
				}
				g = found
			default:
				return nil, fmt.Errorf("create thread group: %w", err)
			}
		}
		if created {
			createdHere++
			st.GroupsCreated++
		} else {
			st.GroupsReused++
		}
		result[k] = g.ID

		sort.Strings(convs)
		changed := false
		for _, convID := range convs {
			addedBy := firstVoter(keys[k].voters[convID])
			ok, err := s.store.AddThreadGroupMemberIfUnfiled(ctx, g.ID, convID, addedBy)
			if err != nil {
				return nil, fmt.Errorf("file thread: %w", err)
			}
			if ok {
				changed = true
				st.ThreadsFiled++
			} else {
				st.skip(tgmSkipAlreadyFiled, 1)
			}
		}

		// Rule 8: one event per group created or changed.
		if created || changed {
			action := ThreadGroupEventUpdated
			if created {
				action = ThreadGroupEventCreated
			}
			s.publishMigratedThreadGroup(ctx, g.ID, action)
		}
	}

	// Rule 5: over the limit is a warning, not a truncation.
	if n := len(existing) + createdHere; n > store.MaxThreadGroupsPerProject {
		slog.WarnContext(ctx, "thread group migration: a project is over the thread group limit",
			"groups", n, "limit", store.MaxThreadGroupsPerProject)
	}
	return result, nil
}

// findThreadGroupByKey returns the project's group with name key k.
func (s *Server) findThreadGroupByKey(ctx context.Context, projectID, k string) (store.ThreadGroup, error) {
	groups, err := s.store.ListThreadGroups(ctx, projectID)
	if err != nil {
		return store.ThreadGroup{}, fmt.Errorf("list thread groups: %w", err)
	}
	for _, g := range groups {
		if g.NameKey == k {
			return g, nil
		}
	}
	return store.ThreadGroup{}, fmt.Errorf("thread group vanished after a name conflict: %w", store.ErrNotFound)
}

// publishMigratedThreadGroup publishes the chat.thread_group event for a
// group the migration created or changed. A failure is logged; the group
// itself is already saved.
func (s *Server) publishMigratedThreadGroup(ctx context.Context, groupID, action string) {
	g, err := s.store.GetThreadGroup(ctx, groupID)
	if err != nil {
		slog.WarnContext(ctx, "thread group migration: reading a group for its event failed", "error", err)
		return
	}
	resp, err := s.threadGroupResponse(ctx, g)
	if err != nil {
		slog.WarnContext(ctx, "thread group migration: building a group event failed", "error", err)
		return
	}
	// Read under s.mu: the sweeper runs in the background and may race a
	// late SetEventPublisher.
	s.mu.RLock()
	events := s.events
	s.mu.RUnlock()
	events.PublishChatThreadGroupEvent(ctx, g.ProjectID, action, resp)
}

// firstVoter returns the smallest user ID in voters.
func firstVoter(voters map[string]bool) string {
	first := ""
	for u := range voters {
		if first == "" || u < first {
			first = u
		}
	}
	return first
}

// insertProjectGroupsInThreadOrder is design 5a rule 6. In each project's
// list in the threadOrder document, the project group a personal group maps
// to (by name key) is inserted directly after that personal group's ID. The
// personal IDs stay. A project group ID already in the list is not added
// again, a project with no list is left without one (it is not custom
// ordered), and other projects and entries are kept as they are. It returns
// the document unchanged when nothing is inserted or it cannot be parsed.
func insertProjectGroupsInThreadOrder(threadOrder string, personal map[string][]personalThreadGroup, groupIDs map[string]map[string]string) string {
	if threadOrder == "" || len(personal) == 0 {
		return threadOrder
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(threadOrder), &doc); err != nil || doc == nil {
		return threadOrder
	}
	changed := false
	for projectKey, groups := range personal {
		ids := groupIDs[projectKey]
		raw, ok := doc[projectKey]
		if !ok || len(ids) == 0 {
			continue
		}
		var list []json.RawMessage
		if err := json.Unmarshal(raw, &list); err != nil {
			continue
		}
		replace := map[string]string{} // personal group ID -> project group ID
		for _, g := range groups {
			name := store.SanitizeThreadGroupName(g.Name)
			if name == "" {
				continue
			}
			if pid, ok := ids[store.ThreadGroupNameKey(name)]; ok {
				if _, dup := replace[g.ID]; !dup {
					replace[g.ID] = pid
				}
			}
		}
		present := map[string]bool{}
		for _, item := range list {
			var id string
			if json.Unmarshal(item, &id) == nil {
				present[id] = true
			}
		}
		out := make([]json.RawMessage, 0, len(list))
		inserted := false
		for _, item := range list {
			out = append(out, item)
			var id string
			if json.Unmarshal(item, &id) != nil {
				continue
			}
			pid, ok := replace[id]
			if !ok || present[pid] {
				continue
			}
			b, _ := json.Marshal(pid)
			out = append(out, b)
			present[pid] = true
			inserted = true
		}
		if inserted {
			b, err := json.Marshal(out)
			if err != nil {
				continue
			}
			doc[projectKey] = b
			changed = true
		}
	}
	if !changed {
		return threadOrder
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return threadOrder
	}
	return string(b)
}
