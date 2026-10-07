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

//go:build integration

package hub

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// TestScheduledSend_TwoHubReplicasPostgres_OneDelivery runs two complete hub
// servers on one Postgres database (hub store and webchat store), schedules
// a message through one of them, and lets both sweep at the same moment,
// repeatedly: the message is delivered exactly once.
func TestScheduledSend_TwoHubReplicasPostgres_OneDelivery(t *testing.T) {
	requirePG(t)
	ctx := context.Background()
	dsn := enttest.NewSchemaURL(t)

	newReplica := func() (*Server, store.Store, WebChatStore, *sql.DB, *brokerMockDispatcher) {
		client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 5, MaxIdleConns: 2})
		require.NoError(t, err)
		srv, s := testServerWithStore(t, entadapter.NewCompositeStore(client))
		db, err := sql.Open("pgx", dsn)
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		wcs := NewWebChatStore(db, "postgres")
		require.NoError(t, wcs.Init())
		srv.SetWebChatStore(wcs)
		d := &brokerMockDispatcher{}
		srv.SetDispatcher(d)
		setScheduledSendExperiment(t, srv, true)
		return srv, s, wcs, db, d
	}
	_ = stdlib.GetDefaultDriver() // registers the "pgx" driver

	srvA, storeA, wcsA, dbA, dispA := newReplica()
	srvB, _, _, _, dispB := newReplica()

	_, bob, project := setupDemoPolicyOn(t, srvA, storeA)
	addProjectMemberWithRole(t, storeA, project, bob.ID, store.GroupMemberRoleMember)
	agent := &store.Agent{
		ID: tid("ha-sched-agent"), ProjectID: project.ID, Name: "ha-sched-agent", Slug: "ha-sched-agent",
		Phase: "running", OwnerID: bob.ID, CreatedBy: bob.ID,
	}
	require.NoError(t, storeA.CreateAgent(ctx, agent))

	topicID := tid("ha-sched-topic")
	require.NoError(t, wcsA.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: project.ID, Name: "ha-sched-topic",
		CreatedBy: bob.ID, CreatedAt: time.Now().UTC(), DefaultAgent: agent.Slug,
	}))
	pid := project.ID
	conv, err := storeA.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind: "group", Surface: "native", ExternalRef: "thread:" + project.ID + ":" + topicID,
		DriftState: "active", ProjectID: &pid,
	})
	require.NoError(t, err)
	_, err = dbA.ExecContext(ctx, `UPDATE webchat_topic SET conversation_id = $1 WHERE id = $2`, conv.ID, topicID)
	require.NoError(t, err)

	const rounds = 5
	for i := 0; i < rounds; i++ {
		fireAt := time.Now().Add(2 * time.Second)
		rec := doRequestAsUser(t, srvA, bob, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/scheduled",
			map[string]interface{}{
				"content":         "ha round " + string(rune('a'+i)),
				"fire_at":         fireAt.UTC().Format(time.RFC3339Nano),
				"idempotency_key": "ha-" + string(rune('a'+i)),
			})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		start := make(chan struct{})
		results := make(chan int, 2)
		for _, srv := range []*Server{srvA, srvB} {
			go func(srv *Server) {
				<-start
				results <- srv.sweepScheduledMessages(ctx, fireAt)
			}(srv)
		}
		close(start)
		assert.Equal(t, 1, <-results+<-results, "round %d: exactly one replica claims the message", i)
	}

	rows, err := storeA.ListMessages(ctx, store.MessageFilter{ThreadID: topicID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Len(t, rows.Items, rounds, "one delivered message per scheduled message")
	assert.Equal(t, rounds, len(dispA.getMessages())+len(dispB.getMessages()), "one dispatch per scheduled message")
}
