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

package entc

import (
	"context"
	"testing"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/stretchr/testify/require"
)

func TestNativeChatTailIndexes(t *testing.T) {
	client := newTestClient(t)
	driver, ok := client.Driver().(*entsql.Driver)
	require.True(t, ok)
	db := driver.DB()
	ctx := context.Background()
	for _, column := range []string{"conversation_id", "thread_id"} {
		t.Run(column, func(t *testing.T) {
			index := "message_" + column + "_channel_created_id"
			// Exercise an upgrade, not just initial creation. Existing databases
			// must acquire these indexes through the normal auto-migration path.
			_, err := db.ExecContext(ctx, "DROP INDEX "+index)
			require.NoError(t, err)
			require.NoError(t, AutoMigrate(ctx, client))
			rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN SELECT * FROM messages WHERE "+column+
				" = ? AND channel = ? AND type <> ? ORDER BY created DESC, id DESC LIMIT 2", "dm", "web", "mention")
			require.NoError(t, err)
			defer rows.Close()
			var plan string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
				plan += detail + "\n"
			}
			require.NoError(t, rows.Err())
			require.Contains(t, plan, index)
			require.NotContains(t, plan, "TEMP B-TREE", "tail lookup must not sort the entire history")
		})
	}
}
