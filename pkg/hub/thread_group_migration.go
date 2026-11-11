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
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// Automatic migration of personal thread groups into project thread groups
// (ptone/scion#4424, design section 5a).
//
// Personal groups live in webchat_user_prefs.thread_groups, a JSON document
// {"<projectId>": [{"id", "name", "threadIds": [<topic ID>...]}]}. A
// background sweeper copies them into project thread groups. A pass reads
// every row whose thread_groups is non-empty and whose
// thread_groups_migrated_at is NULL, handles the rows one project at a time,
// then marks each user done. A user's entries for a project count only if
// the user may read that project now (rule 0, design section 3).
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
	tgmSkipNoAccess         = "no_access"         // the user may not read the project (rule 0); counted per group
	tgmSkipInactiveUser     = "inactive_user"     // the user is missing or inactive (rule 0); counted per group
)

// threadGroupMigrationClientType is the client type of the identity the
// migration checks a user's project access with (rule 0), next to
// scheduledSendClientType.
const threadGroupMigrationClientType = "thread-group-migration"

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
	UsersSkipped   int // missing or inactive users, left unmarked (rule 0)
	UsersDeferred  int // a store error during the access check; retried next pass
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

// tgmStageError is a pass error tagged with the step that failed, so the
// warning can name the step and the error class without logging the error
// text, which may carry IDs (design 5a, Observability).
type tgmStageError struct {
	stage string
	err   error
}

func (e *tgmStageError) Error() string { return e.stage + ": " + e.err.Error() }
func (e *tgmStageError) Unwrap() error { return e.err }

func tgmErr(stage string, err error) error { return &tgmStageError{stage: stage, err: err} }

// tgmLogAttrs returns the stage and error class of err for a warning: no
// error text, names or IDs.
func tgmLogAttrs(err error) []any {
	stage := "unknown"
	var se *tgmStageError
	if errors.As(err, &se) {
		stage = se.stage
	}
	class := "other"
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		class = "canceled"
	case errors.Is(err, store.ErrTransient):
		class = "transient"
	case errors.Is(err, store.ErrNotFound):
		class = "not_found"
	case errors.Is(err, store.ErrAlreadyExists):
		class = "already_exists"
	case errors.Is(err, store.ErrInvalidInput):
		class = "invalid_input"
	}
	return []any{"stage", stage, "error_class", class}
}

// threadGroupMigrationRuntime tracks the sweeper goroutine so that stopping
// the hub waits for a pass in progress.
type threadGroupMigrationRuntime struct {
	mu      sync.Mutex
	stopped bool
	cancel  context.CancelFunc
	running sync.WaitGroup
}

// startThreadGroupMigrationSweeper starts the migration sweeper: a pass at
// once, then one every threadGroupMigrationTick, until ctx is cancelled or
// stopThreadGroupMigrationSweeper runs. A user is marked done once copied,
// so later passes find nothing to do.
func (s *Server) startThreadGroupMigrationSweeper(ctx context.Context) {
	if !s.nativeChatEnabled() {
		return
	}
	rt := &s.threadGroupMigration
	tick := threadGroupMigrationTick // read once, before the goroutine starts
	loopCtx, cancel := context.WithCancel(ctx)
	rt.mu.Lock()
	if rt.stopped || rt.cancel != nil {
		rt.mu.Unlock()
		cancel()
		return
	}
	rt.cancel = cancel
	rt.running.Add(1)
	rt.mu.Unlock()
	go func() {
		defer rt.running.Done()
		s.migrateThreadGroupsPass(loopCtx)
		ticker := time.NewTicker(tick)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				s.migrateThreadGroupsPass(loopCtx)
			}
		}
	}()
}

// stopThreadGroupMigrationSweeper stops the sweeper and waits for a pass in
// progress to return; its store calls see the cancelled context. It is safe
// to call more than once, or without a start, and the sweeper cannot be
// started again afterwards.
func (s *Server) stopThreadGroupMigrationSweeper() {
	rt := &s.threadGroupMigration
	rt.mu.Lock()
	rt.stopped = true
	cancel := rt.cancel
	rt.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	rt.running.Wait()
}

// tgmUserAccess is the rule 0 outcome for one user in one pass.
type tgmUserAccess int

const (
	tgmUserOK       tgmUserAccess = iota
	tgmUserSkipped                // missing or inactive: entries skipped, left unmarked
	tgmUserDeferred               // a store error: entries skipped, left unmarked, retried
)

// migrateThreadGroupsPass runs one migration pass and returns its counts.
func (s *Server) migrateThreadGroupsPass(ctx context.Context) threadGroupMigrationStats {
	var st threadGroupMigrationStats
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil || s.store == nil {
		return st
	}
	rows, err := wcs.ListUserPrefsPendingThreadGroupMigration(ctx)
	if err != nil {
		slog.WarnContext(ctx, "thread group migration: listing pending users failed", tgmLogAttrs(tgmErr("list_pending", err))...)
		return st
	}
	if len(rows) == 0 {
		return st
	}
	st.Users = len(rows)

	// Rule 0: a user's entries for a project are copied only if the user
	// could read the project through the API now (design section 3). The
	// identity is rebuilt from the current user record, and the check is the
	// one the scheduled-send sweeper makes (checkScheduledTopicFire), not
	// canReadProject, which would turn a store error into a denial and mark
	// the user done. Projects and decisions are cached for the pass.
	projects := map[string]tgmProjectLookup{}
	lookupProject := func(projectKey string) tgmProjectLookup {
		if pl, ok := projects[projectKey]; ok {
			return pl
		}
		var pl tgmProjectLookup
		if _, perr := uuid.Parse(projectKey); perr == nil {
			p, err := s.store.GetProject(ctx, projectKey)
			switch {
			case err == nil:
				pl.project = p
			case !errors.Is(err, store.ErrNotFound):
				pl.err = tgmErr("get_project", err)
			}
		}
		projects[projectKey] = pl
		return pl
	}

	// allowed[i] holds row i's personal groups for the projects its user may
	// read; nil when the row is not marked this pass.
	allowed := make([]map[string][]personalThreadGroup, len(rows))
	byProject := map[string][]tgmUserGroup{}
	for i, row := range rows {
		groups, err := parsePersonalThreadGroups(row.ThreadGroups)
		if err != nil {
			st.skip(tgmSkipInvalidPrefs, 1)
			allowed[i] = map[string][]personalThreadGroup{}
			continue
		}
		mine, access := s.tgmAllowedGroups(ctx, row.UserID, groups, lookupProject, &st)
		switch access {
		case tgmUserSkipped:
			st.UsersSkipped++
			continue
		case tgmUserDeferred:
			st.UsersDeferred++
			continue
		}
		allowed[i] = mine
		for projectKey, list := range mine {
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

	// One batch per project, in sorted order. Rows are in user ID order (the
	// store sorts them) and each user's groups keep their order, so the
	// batch is deterministic.
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
			slog.WarnContext(ctx, "thread group migration: a project failed; its users are retried next pass", tgmLogAttrs(err)...)
			continue
		}
		groupIDs[projectKey] = ids
	}

	for i, row := range rows {
		mine := allowed[i]
		if mine == nil {
			continue // skipped or deferred user: left unmarked
		}
		retry := false
		for projectKey := range mine {
			if failed[projectKey] {
				retry = true
				break
			}
		}
		if retry {
			continue
		}
		// Rule 6 uses only the projects the user may read, so a denied
		// project's group IDs never enter the user's order.
		newOrder := insertProjectGroupsInThreadOrder(row.ThreadOrder, mine, groupIDs)
		ok, err := wcs.MarkUserPrefsThreadGroupsMigrated(ctx, row.UserID, row.ThreadGroups, row.ThreadOrder, newOrder)
		if err != nil {
			slog.WarnContext(ctx, "thread group migration: marking a user done failed", tgmLogAttrs(tgmErr("mark_user", err))...)
			continue
		}
		if ok {
			st.UsersMarked++
		}
	}

	attrs := []any{
		"users", st.Users,
		"users_marked", st.UsersMarked,
		"users_skipped", st.UsersSkipped,
		"users_deferred", st.UsersDeferred,
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

// tgmProjectLookup is a project read once per pass for rule 0.
type tgmProjectLookup struct {
	project *store.Project // nil: the project does not exist (or the key is not a UUID)
	err     error          // a store error; the user is deferred
}

// tgmAllowedGroups applies rule 0 to one user. It returns the user's
// personal groups for the projects the user may read, and the user's
// outcome: a missing or inactive user is skipped, and a store error or an
// indeterminate access decision (Decision.IsIndeterminate) defers the whole
// user. Entries for a missing project (rule 1) or a project the
// user may not read are dropped and counted; the user can still be marked.
func (s *Server) tgmAllowedGroups(ctx context.Context, userID string, groups map[string][]personalThreadGroup,
	lookupProject func(string) tgmProjectLookup, st *threadGroupMigrationStats) (map[string][]personalThreadGroup, tgmUserAccess) {
	if len(groups) == 0 {
		return map[string][]personalThreadGroup{}, tgmUserOK
	}

	u, err := s.store.GetUser(ctx, userID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		for _, list := range groups {
			st.skip(tgmSkipInactiveUser, len(list))
		}
		return nil, tgmUserSkipped
	case err != nil:
		slog.WarnContext(ctx, "thread group migration: user lookup failed; the user is retried next pass", tgmLogAttrs(tgmErr("get_user", err))...)
		return nil, tgmUserDeferred
	case u.Status != store.UserStatusActive:
		for _, list := range groups {
			st.skip(tgmSkipInactiveUser, len(list))
		}
		return nil, tgmUserSkipped
	}
	identity := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, threadGroupMigrationClientType)

	// Sorted, so the outcome does not depend on map order.
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string][]personalThreadGroup, len(groups))
	denied := map[string]int{}
	missing := map[string]int{}
	for _, projectKey := range keys {
		list := groups[projectKey]
		pl := lookupProject(projectKey)
		if pl.err != nil {
			slog.WarnContext(ctx, "thread group migration: project lookup failed; the user is retried next pass", tgmLogAttrs(pl.err)...)
			return nil, tgmUserDeferred
		}
		if pl.project == nil {
			threads := 0
			for _, g := range list {
				threads += len(g.ThreadIDs)
			}
			missing[projectKey] = threads
			continue
		}
		d := s.authzService.CheckAccess(ctx, identity, projectResource(pl.project), ActionRead)
		switch {
		case d.Allowed:
			out[projectKey] = list
		case d.IsIndeterminate():
			// The check could not be decided (a store or resolution fault
			// in the authz layer): defer the whole user, unmarked, as for a
			// store error here, so a fault never marks the user done.
			slog.WarnContext(ctx, "thread group migration: access check was indeterminate; the user is retried next pass",
				"stage", "check_access", "error_class", "indeterminate")
			return nil, tgmUserDeferred
		default:
			// A policy deny: skip this project's entries; the user can
			// still be marked done.
			denied[projectKey] = len(list)
		}
	}
	// Counted only once the user is known not to be deferred, so a retried
	// user is not counted twice.
	for _, n := range missing {
		st.skip(tgmSkipMissingProject, n)
	}
	for _, n := range denied {
		st.skip(tgmSkipNoAccess, n)
	}
	return out, tgmUserOK
}

// migrateProjectThreadGroups copies one project's personal groups into
// project groups (design 5a, rules 1-5 and 8) and returns, for every name
// key in the batch that has a project group afterwards, that group's ID. An
// error means the project should be retried on the next pass.
func (s *Server) migrateProjectThreadGroups(ctx context.Context, wcs WebChatStore, projectID string, entries []tgmUserGroup, st *threadGroupMigrationStats) (map[string]string, error) {
	// The project exists and every entry's user may read it (rule 0).

	// Rule 1: resolve topic IDs within this project only. This is the batched
	// equivalent of calling GetTopicConversationIDInProject per entry, as
	// design 5a rule 1 names: ListTopics(projectID) returns exactly the rows
	// that lookup resolves (this project, deleted_at IS NULL), in one query
	// per project, and also tells #general apart, which that lookup cannot.
	// The unscoped lookups are never used.
	topics, err := wcs.ListTopics(ctx, projectID)
	if err != nil {
		return nil, tgmErr("list_topics", err)
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
				return "", "", tgmErr("get_conversation", err)
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
		return nil, tgmErr("list_thread_groups", err)
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
	changedGroups := map[string]string{} // group ID -> event action
	// Rule 8: one event per group created or changed, built in one batch.
	// Deferred, so groups saved before a failure later in this project still
	// get their event: a retry reuses them unchanged and would publish none.
	defer func() { s.publishMigratedThreadGroups(ctx, projectID, changedGroups) }()
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
				return nil, tgmErr("create_thread_group", err)
			}
		}
		if created {
			createdHere++
			st.GroupsCreated++
			changedGroups[g.ID] = ThreadGroupEventCreated
		} else {
			st.GroupsReused++
		}
		result[k] = g.ID

		sort.Strings(convs)
		for _, convID := range convs {
			addedBy := firstVoter(keys[k].voters[convID])
			ok, err := s.store.AddThreadGroupMemberIfUnfiled(ctx, g.ID, convID, addedBy)
			if err != nil {
				return nil, tgmErr("file_thread", err)
			}
			if ok {
				if _, seen := changedGroups[g.ID]; !seen {
					changedGroups[g.ID] = ThreadGroupEventUpdated
				}
				st.ThreadsFiled++
			} else {
				st.skip(tgmSkipAlreadyFiled, 1)
			}
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
		return store.ThreadGroup{}, tgmErr("list_thread_groups", err)
	}
	for _, g := range groups {
		if g.NameKey == k {
			return g, nil
		}
	}
	return store.ThreadGroup{}, tgmErr("reuse_thread_group", fmt.Errorf("thread group vanished after a name conflict: %w", store.ErrNotFound))
}

// publishMigratedThreadGroups publishes the chat.thread_group events for
// the groups of one project that the migration created or changed (rule 8),
// with one group read and one payload build (so one topic listing in
// threadGroupResponses) for the whole batch, not one per group. Failures
// are logged; the groups themselves are already saved.
func (s *Server) publishMigratedThreadGroups(ctx context.Context, projectID string, actions map[string]string) {
	if len(actions) == 0 {
		return
	}
	all, err := s.store.ListThreadGroups(ctx, projectID)
	if err != nil {
		slog.WarnContext(ctx, "thread group migration: reading groups for events failed", tgmLogAttrs(tgmErr("event_read", err))...)
		return
	}
	changed := make([]store.ThreadGroup, 0, len(actions))
	for _, g := range all {
		if _, ok := actions[g.ID]; ok {
			changed = append(changed, g)
		}
	}
	resps, err := s.threadGroupResponses(ctx, projectID, changed)
	if err != nil {
		slog.WarnContext(ctx, "thread group migration: building group events failed", tgmLogAttrs(tgmErr("event_build", err))...)
		return
	}
	// Read under s.mu: the sweeper runs in the background and may race a
	// late SetEventPublisher.
	s.mu.RLock()
	events := s.events
	s.mu.RUnlock()
	for _, r := range resps {
		events.PublishChatThreadGroupEvent(ctx, projectID, actions[r.ID], r)
	}
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
