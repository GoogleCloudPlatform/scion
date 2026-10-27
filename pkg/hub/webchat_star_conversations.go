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
	"fmt"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The Starred rail's store query: the distinct conversations in which a
// user has at least one live star.
//
// A star counts for a conversation when its message still exists, is a
// message of the conversation's native routing key (messages.thread_id
// equals the star's conversation key, which a promotion moves in the same
// transaction as the star), is in the history channel, and is not
// soft-deleted; a thread must also be a live topic. Stars are grouped by
// that key before the page is cut, so a conversation appears once
// whatever its number of stars, and a page boundary never splits one.
//
// The order is deterministic and keyset-paged on values the query itself
// computes, so the position a page ends at is exactly the position the
// next page resumes after:
//   - activity: the conversation's last activity (the thread's topic row,
//     or the caller's own DM row, the sources the chat rail reads) as
//     fixed-width UTC text, newest first; no activity sorts last. Then the
//     conversation key, ascending.
//   - alpha: the case-folded conversation name (the topic name, or a DM
//     peer's label as the star routes label it), then the key, both
//     ascending, by byte order.
//
// Read access is not applied here: the handler authorizes every candidate
// before it serializes anything.

// Starred rail sort orders (the sort query parameter).
const (
	StarredConversationsByActivity = "activity"
	StarredConversationsByName     = "alpha"
)

// starActivityKeyLayout is the fixed-width text both dialects return as an
// activity sort value, so a lexical comparison is chronological.
const starActivityKeyLayout = "2006-01-02T15:04:05.000000000Z"

// StarredConversationPosition is a keyset position in a starred rail
// order: the order's sort value (the activity key, or the folded name)
// and the conversation key.
type StarredConversationPosition struct {
	Value string
	Key   string
}

// StarredConversationFilter selects one page of the Starred rail.
type StarredConversationFilter struct {
	UserID string
	// Channel limits stars to messages of one channel ("web" for native
	// chat); empty matches every channel.
	Channel string
	// ProjectID, when set, keeps the threads of that project and the DMs
	// whose stars are stored under it (its agents' DMs); never a DM
	// between users.
	ProjectID string
	// Sort is StarredConversationsByActivity or StarredConversationsByName.
	Sort string
	// After resumes strictly after this position; nil starts at the top.
	After *StarredConversationPosition
	// Limit is the page size (at least 1).
	Limit int
}

// StarredConversationCandidate is one conversation of the Starred rail
// before authorization.
type StarredConversationCandidate struct {
	ConversationKey string
	IsDM            bool
	// ProjectID is a thread's topic project, or the project an agent DM's
	// stars are stored under ("" for a DM between users).
	ProjectID string
	// TopicName is a thread's current name ("" for a DM).
	TopicName string
	// LastActivityAt is the conversation's last activity; zero when it has
	// none recorded.
	LastActivityAt time.Time
	// Position is the candidate's place in the requested order.
	Position StarredConversationPosition
}

// StarredConversationPage is one page of candidates; Next is the position
// to resume after when more may remain, nil at the end.
type StarredConversationPage struct {
	Items []StarredConversationCandidate
	Next  *StarredConversationPosition
}

// validStarredConversationFilter reports whether f names a user, a known
// order and a page size.
func validStarredConversationFilter(f StarredConversationFilter) bool {
	if f.UserID == "" || f.Limit < 1 {
		return false
	}
	return f.Sort == StarredConversationsByActivity || f.Sort == StarredConversationsByName
}

// trimStarredConversationPage cuts a page fetched with limit+1 rows to
// limit and sets Next when the extra row showed that more remain.
func trimStarredConversationPage(page StarredConversationPage, limit int) StarredConversationPage {
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1].Position
		page.Next = &last
	}
	return page
}

// parseStarActivityKey returns the instant of an activity sort value, the
// zero time for "" (no activity).
func parseStarActivityKey(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(starActivityKeyLayout, v)
	if err != nil || len(v) != len(starActivityKeyLayout) {
		return time.Time{}, fmt.Errorf("webchat store: activity key %q: %w", v, store.ErrInvalidInput)
	}
	return t.UTC(), nil
}

// sqliteStarActivityKey is the activity sort value of a webchat_* TEXT
// time column on SQLite: an RFC3339 UTC text (what current writers store)
// padded to nine fractional digits, "" for NULL or any other layout (a
// legacy row, which then sorts as having no activity).
func sqliteStarActivityKey(col string) string {
	return `(CASE WHEN ` + col + ` GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
	           AND length(` + col + `) <= 30
	      THEN substr(` + col + `, 1, 19) || '.' ||
	           substr((CASE WHEN substr(` + col + `, 20, 1) = '.' THEN substr(` + col + `, 21, length(` + col + `) - 21) ELSE '' END) || '000000000', 1, 9) || 'Z'
	      ELSE '' END)`
}

// starPeerNameSQL is the case-folded label of a DM row's peer, as
// starUserLabel and starAgentLabel label it, over the users row u and the
// agents row a joined for the peer (fold is the dialect's lower-casing
// function, trim its trim).
func starPeerNameSQL(fold, trim string) string {
	return fold + `(CASE
	    WHEN d.peer_kind = 'user' THEN COALESCE(NULLIF(` + trim + `(u.display_name), ''),
	         CASE WHEN u.id IS NULL THEN '` + starLabelDeletedUser + `' ELSE '` + starLabelUser + `' END)
	    WHEN d.peer_kind = 'agent' THEN COALESCE(NULLIF(` + trim + `(a.name), ''), NULLIF(` + trim + `(a.slug), ''),
	         CASE WHEN a.id IS NULL THEN '` + starLabelDeletedAgent + `' ELSE '` + starLabelAgent + `' END)
	    ELSE '` + starLabelDM + `' END)`
}

// sqliteStarredConversationsQuery returns the Starred rail statement and
// its arguments for a valid filter.
func sqliteStarredConversationsQuery(f StarredConversationFilter) (string, []any) {
	const isDM = `substr(el.ckey, 1, 3) = 'dm:'`
	activity := `(CASE WHEN ` + isDM + ` THEN d.last_activity_at ELSE t.last_activity_at END)`
	sortValue := sqliteStarActivityKey(activity)
	peerJoins := ""
	if f.Sort == StarredConversationsByName {
		sortValue = `(CASE WHEN ` + isDM + ` THEN ` + starPeerNameSQL("lower", "trim") + ` ELSE lower(t.name) END)`
		peerJoins = `
  LEFT JOIN users u ON d.peer_kind = 'user' AND u.id = d.peer_id
  LEFT JOIN agents a ON d.peer_kind = 'agent' AND a.id = d.peer_id`
	}
	args := []any{f.UserID, f.Channel, f.Channel, f.UserID}
	var conds []string
	if f.ProjectID != "" {
		conds = append(conds, "c.project_id = ?")
		args = append(args, f.ProjectID)
	}
	cmp, dir := ">", "ASC"
	if f.Sort == StarredConversationsByActivity {
		cmp, dir = "<", "DESC"
	}
	if f.After != nil {
		conds = append(conds, "(c.sort_value "+cmp+" ? OR (c.sort_value = ? AND c.ckey > ?))")
		args = append(args, f.After.Value, f.After.Value, f.After.Key)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, f.Limit+1)
	// The inner query groups the user's live stars by conversation key
	// (driven by the user's star index); the outer one joins the current
	// topic or DM row for the name, activity and project.
	return `
SELECT c.ckey, c.is_dm, c.project_id, c.topic_name, c.activity_raw, c.sort_value FROM (
SELECT el.ckey AS ckey,
       (CASE WHEN ` + isDM + ` THEN 1 ELSE 0 END) AS is_dm,
       (CASE WHEN ` + isDM + ` THEN el.star_project ELSE t.project_id END) AS project_id,
       COALESCE(t.name, '') AS topic_name,
       COALESCE(` + activity + `, '') AS activity_raw,
       ` + sortValue + ` AS sort_value
  FROM (SELECT st.conversation_key AS ckey, MAX(COALESCE(st.project_id, '')) AS star_project
          FROM webchat_message_star st
          JOIN messages m ON m.id = st.message_id
         WHERE st.user_id = ?
           AND m.thread_id = st.conversation_key
           AND (? = '' OR m.channel = ?)
           AND NOT EXISTS (SELECT 1 FROM webchat_message_ext e WHERE e.message_id = st.message_id AND e.deleted_at IS NOT NULL)
         GROUP BY st.conversation_key) el
  LEFT JOIN webchat_topic t ON t.id = el.ckey
  LEFT JOIN webchat_dm d ON d.participant_id = ? AND d.conversation_key = el.ckey` + peerJoins + `
 WHERE ` + isDM + ` OR (t.id IS NOT NULL AND t.deleted_at IS NULL)
) c` + where + `
 ORDER BY c.sort_value ` + dir + `, c.ckey ASC
 LIMIT ?`, args
}

func (s *sqliteWebChatStore) ListStarredConversationCandidates(ctx context.Context, f StarredConversationFilter) (StarredConversationPage, error) {
	if !validStarredConversationFilter(f) {
		return StarredConversationPage{}, fmt.Errorf("webchat store: list starred conversations: %w", store.ErrInvalidInput)
	}
	query, args := sqliteStarredConversationsQuery(f)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return StarredConversationPage{}, fmt.Errorf("webchat store: list starred conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	page := StarredConversationPage{Items: []StarredConversationCandidate{}}
	for rows.Next() {
		var c StarredConversationCandidate
		var isDMInt int
		var activityRaw string
		if err := rows.Scan(&c.ConversationKey, &isDMInt, &c.ProjectID, &c.TopicName, &activityRaw, &c.Position.Value); err != nil {
			return StarredConversationPage{}, fmt.Errorf("webchat store: list starred conversations: %w", err)
		}
		c.IsDM = isDMInt != 0
		c.Position.Key = c.ConversationKey
		c.LastActivityAt = parseSQLiteTime(activityRaw)
		page.Items = append(page.Items, c)
	}
	if err := rows.Err(); err != nil {
		return StarredConversationPage{}, fmt.Errorf("webchat store: list starred conversations: %w", err)
	}
	return trimStarredConversationPage(page, f.Limit), nil
}
