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
	"database/sql"
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Postgres twin of the Starred rail query in webchat_star_conversations.go.
// Sort values and keys compare under COLLATE "C" (byte order, as SQLite
// compares them), so the order does not depend on the database collation.
// The activity sort value is the same fixed-width UTC text SQLite returns.

func (s *pgWebChatStore) ListStarredConversationCandidates(ctx context.Context, f StarredConversationFilter) (StarredConversationPage, error) {
	if !validStarredConversationFilter(f) {
		return StarredConversationPage{}, fmt.Errorf("webchat store: list starred conversations: %w", store.ErrInvalidInput)
	}
	messagesTable, err := s.table(ctx, "messages")
	if err != nil {
		return StarredConversationPage{}, err
	}
	conversationsTable, err := s.table(ctx, "conversations")
	if err != nil {
		return StarredConversationPage{}, err
	}
	const isDM = `left(el.ckey, 3) = 'dm:'`
	activity := `(CASE WHEN ` + isDM + ` THEN d.last_activity_at ELSE t.last_activity_at END)`
	// TIMESTAMPTZ keeps microseconds: the nine-digit form ends in 000.
	sortValue := `COALESCE(to_char(` + activity + ` AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US') || '000Z', '')`
	peerJoins := ""
	a := &pgArgs{}
	if f.Sort == StarredConversationsByName {
		usersTable, err := s.table(ctx, "users")
		if err != nil {
			return StarredConversationPage{}, err
		}
		agentsTable, err := s.table(ctx, "agents")
		if err != nil {
			return StarredConversationPage{}, err
		}
		trim := func(col string) string { return `btrim(` + col + `, ` + a.add(starLabelSpace) + `::text)` }
		// Folding under "C" lower-cases ASCII letters only, as SQLite's lower
		// does, whatever the database's ctype.
		sortValue = `(CASE WHEN ` + isDM + ` THEN ` + starPeerNameSQL(trim, ` COLLATE "C"`) + ` ELSE lower(t.name COLLATE "C") END)`
		peerJoins = `
  LEFT JOIN ` + usersTable + ` u ON d.peer_kind = 'user' AND u.id = ` + pgUUIDText("d.peer_id") + `
  LEFT JOIN ` + agentsTable + ` a ON d.peer_kind = 'agent' AND a.id = ` + pgUUIDText("d.peer_id")
	}
	user := a.add(f.UserID)
	ch := a.add(f.Channel)
	var conds []string
	if f.ProjectID != "" {
		conds = append(conds, "c.project_id = "+a.add(f.ProjectID)+"::text")
	}
	cmp, dir := ">", "ASC"
	if f.Sort == StarredConversationsByActivity {
		cmp, dir = "<", "DESC"
	}
	if f.After != nil {
		v := a.add(f.After.Value)
		k := a.add(f.After.Key)
		conds = append(conds, `(c.sort_value COLLATE "C" `+cmp+` `+v+`::text COLLATE "C"
     OR (c.sort_value COLLATE "C" = `+v+`::text COLLATE "C" AND c.ckey COLLATE "C" > `+k+`::text COLLATE "C"))`)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	// The read switch's one selector, as the other star paths apply it.
	selector := `m.thread_id = st.conversation_key`
	if f.ByConversationID {
		selector = `m.conversation_id IN (
                     SELECT ` + pgUUIDText("lower(t2.conversation_id)") + ` FROM webchat_topic t2
                      WHERE t2.id = st.conversation_key
                     UNION ALL
                     SELECT c.id FROM ` + conversationsTable + ` c
                      WHERE left(st.conversation_key, 3) = 'dm:' AND c.surface = 'native'
                        AND c.external_ref = st.conversation_key
                        -- These two terms mirror the predicate of the partial unique index on (surface, external_ref).
                        AND c.external_ref <> '' AND c.deleted_at IS NULL)`
	}
	limit := a.add(f.Limit + 1)
	rows, err := s.db.QueryContext(ctx, `
SELECT c.ckey, c.is_dm, c.project_id, c.topic_name, c.activity, c.sort_value FROM (
SELECT el.ckey AS ckey,
       `+isDM+` AS is_dm,
       (CASE WHEN `+isDM+` THEN el.star_project ELSE t.project_id END) AS project_id,
       COALESCE(t.name, '') AS topic_name,
       `+activity+` AS activity,
       `+sortValue+` AS sort_value
  FROM (SELECT st.conversation_key AS ckey, MAX(COALESCE(st.project_id, '')) AS star_project
          FROM webchat_message_star st
          JOIN `+messagesTable+` m ON m.id = `+pgUUIDText("st.message_id")+`
         WHERE st.user_id = `+user+`::text
           AND `+selector+`
           AND (`+ch+`::text = '' OR m.channel = `+ch+`::text)
           AND NOT EXISTS (SELECT 1 FROM webchat_message_ext e WHERE e.message_id = st.message_id AND e.deleted_at IS NOT NULL)
         GROUP BY st.conversation_key) el
  LEFT JOIN webchat_topic t ON t.id = el.ckey
  LEFT JOIN webchat_dm d ON d.participant_id = `+user+`::text AND d.conversation_key = el.ckey`+peerJoins+`
 WHERE `+isDM+` OR (t.id IS NOT NULL AND t.deleted_at IS NULL)
) c`+where+`
 ORDER BY c.sort_value COLLATE "C" `+dir+`, c.ckey COLLATE "C" ASC
 LIMIT `+limit, a.vals...)
	if err != nil {
		return StarredConversationPage{}, fmt.Errorf("webchat store: list starred conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	page := StarredConversationPage{Items: []StarredConversationCandidate{}}
	for rows.Next() {
		var c StarredConversationCandidate
		var activity sql.NullTime
		if err := rows.Scan(&c.ConversationKey, &c.IsDM, &c.ProjectID, &c.TopicName, &activity, &c.Position.Value); err != nil {
			return StarredConversationPage{}, fmt.Errorf("webchat store: list starred conversations: %w", err)
		}
		if activity.Valid {
			c.LastActivityAt = activity.Time.UTC()
		}
		c.Position.Key = c.ConversationKey
		page.Items = append(page.Items, c)
	}
	if err := rows.Err(); err != nil {
		return StarredConversationPage{}, fmt.Errorf("webchat store: list starred conversations: %w", err)
	}
	return trimStarredConversationPage(page, f.Limit), nil
}
