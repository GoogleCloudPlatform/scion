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

package entadapter

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SetAgentNativeCloudRun writes only the nativeCloudRun key, keeps
// state_version and every other applied-config key, and removes the key
// when cleared.
func TestAgentStore_SetAgentNativeCloudRun(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "native-cr")
	a.AppliedConfig = &store.AgentAppliedConfig{
		Image:   "example/image:1",
		Profile: "cr",
		Env:     map[string]string{"A": "1"},
		GCPIdentity: &store.GCPIdentityConfig{
			MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: "sa-1",
		},
	}
	require.NoError(t, s.CreateAgent(ctx, a))
	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "running", Activity: "working"}))

	require.NoError(t, s.SetAgentNativeCloudRun(ctx, a.ID, true))
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	assert.True(t, got.AppliedConfig.NativeCloudRun)
	assert.Equal(t, before.StateVersion, got.StateVersion, "the flag write does not bump state_version")
	assert.Equal(t, "running", got.Phase)
	assert.Equal(t, "working", got.Activity)
	assert.Equal(t, "example/image:1", got.AppliedConfig.Image)
	assert.Equal(t, "cr", got.AppliedConfig.Profile)
	assert.Equal(t, map[string]string{"A": "1"}, got.AppliedConfig.Env)
	require.NotNil(t, got.AppliedConfig.GCPIdentity)
	assert.Equal(t, "sa-1", got.AppliedConfig.GCPIdentity.ServiceAccountID)

	// Setting it again is a no-op.
	require.NoError(t, s.SetAgentNativeCloudRun(ctx, a.ID, true))

	// A later full write of a copy that carries the flag keeps it.
	got.AppliedConfig.Model = "m"
	require.NoError(t, s.UpdateAgent(ctx, got))
	again, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.True(t, again.AppliedConfig.NativeCloudRun)

	require.NoError(t, s.SetAgentNativeCloudRun(ctx, a.ID, false))
	cleared, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.False(t, cleared.AppliedConfig.NativeCloudRun)
	assert.Equal(t, again.StateVersion, cleared.StateVersion)
	raw, err := s.client.Agent.Get(ctx, uuid.MustParse(a.ID))
	require.NoError(t, err)
	assert.False(t, strings.Contains(raw.AppliedConfig, "nativeCloudRun"), "the cleared key is removed")
	require.NoError(t, s.SetAgentNativeCloudRun(ctx, a.ID, false), "clearing again is a no-op")
}

func TestAgentStore_SetAgentNativeCloudRun_NotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestAgentStore(t)
	err := s.SetAgentNativeCloudRun(ctx, uuid.NewString(), true)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// newNativeAgent creates an agent for a test that then records the native
// decision on it.
func newNativeAgent(t *testing.T, s *AgentStore, projectID, slug string) *store.Agent {
	t.Helper()
	ctx := context.Background()
	a := makeAgent(projectID, slug)
	a.AppliedConfig = &store.AgentAppliedConfig{Profile: "cr", Env: map[string]string{"A": "1"}}
	require.NoError(t, s.CreateAgent(ctx, a))
	return a
}

func storedNativeCloudRun(t *testing.T, s *AgentStore, id string) bool {
	t.Helper()
	got, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return got.AppliedConfig != nil && got.AppliedConfig.NativeCloudRun
}

// A whole-row write from a copy read before the decision was recorded
// keeps it, on the plain and the running path, and the caller's copy is
// brought in line.
func TestAgentStore_UpdateAgentKeepsNativeCloudRun(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	for _, phase := range []string{"stopped", "running"} {
		t.Run(phase, func(t *testing.T) {
			a := newNativeAgent(t, s, projectID, "keep-"+phase)
			stale, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			require.NoError(t, s.SetAgentNativeCloudRun(ctx, a.ID, true))

			stale.Phase = phase
			stale.AppliedConfig.Model = "m"
			require.NoError(t, s.UpdateAgent(ctx, stale))
			assert.True(t, storedNativeCloudRun(t, s, a.ID), "a stale copy cannot drop it")
			assert.True(t, stale.AppliedConfig.NativeCloudRun, "the caller's copy is brought in line")

			// A caller that passes false cannot lower it either.
			stale.AppliedConfig.NativeCloudRun = false
			require.NoError(t, s.UpdateAgent(ctx, stale))
			assert.True(t, storedNativeCloudRun(t, s, a.ID))

			// A whole-row write with no applied config keeps the key alone.
			stale.AppliedConfig = nil
			require.NoError(t, s.UpdateAgent(ctx, stale))
			assert.True(t, storedNativeCloudRun(t, s, a.ID))
		})
	}
}

// UpdateAgentConfig keeps the decision the same way, and a caller's true
// does not raise it.
func TestAgentStore_UpdateAgentConfigKeepsNativeCloudRun(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := newNativeAgent(t, s, projectID, "keep-config")
	stale, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetAgentNativeCloudRun(ctx, a.ID, true))

	applied := *stale.AppliedConfig
	applied.Model = "m"
	applied.NativeCloudRun = false
	_, err = s.UpdateAgentConfig(ctx, a.ID, stale.StateVersion, &applied, nil)
	require.NoError(t, err)
	assert.True(t, storedNativeCloudRun(t, s, a.ID))

	b := newNativeAgent(t, s, projectID, "raise-config")
	bb, err := s.GetAgent(ctx, b.ID)
	require.NoError(t, err)
	raised := *bb.AppliedConfig
	raised.NativeCloudRun = true
	_, err = s.UpdateAgentConfig(ctx, b.ID, bb.StateVersion, &raised, nil)
	require.NoError(t, err)
	assert.False(t, storedNativeCloudRun(t, s, b.ID), "only SetAgentNativeCloudRun records it")
}

// CreateAgent never records the decision, whatever the caller passes.
func TestAgentStore_CreateAgentNeverRecordsNativeCloudRun(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "create-native")
	a.AppliedConfig = &store.AgentAppliedConfig{Profile: "cr", NativeCloudRun: true}
	require.NoError(t, s.CreateAgent(ctx, a))
	assert.False(t, storedNativeCloudRun(t, s, a.ID))
}

// The write predicate matches a row only while it holds the value the
// write was built for, so a write built before SetAgentNativeCloudRun
// matches nothing after it.
func TestAppliedNativeCloudRunIs(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := newNativeAgent(t, s, projectID, "predicate")
	uid := uuid.MustParse(a.ID)
	n, err := s.client.Agent.Update().Where(agent.IDEQ(uid), appliedNativeCloudRunIs(true)).SetMessage("x").Save(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "not recorded yet")
	n, err = s.client.Agent.Update().Where(agent.IDEQ(uid), appliedNativeCloudRunIs(false)).SetMessage("x").Save(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	require.NoError(t, s.SetAgentNativeCloudRun(ctx, a.ID, true))
	n, err = s.client.Agent.Update().Where(agent.IDEQ(uid), appliedNativeCloudRunIs(false)).SetMessage("y").Save(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "a write built for the unrecorded row matches nothing")
	n, err = s.client.Agent.Update().Where(agent.IDEQ(uid), appliedNativeCloudRunIs(true)).SetMessage("y").Save(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// The keep predicate a whole-document write adds agrees with the Go
// reading of the row (rowNativeCloudRun) for every kind of stored document:
// it matches the row for the value Go reads and, for an empty or valid
// JSON document, not for the other value. A document that is not valid
// JSON is matched by its exact text, so no JSON parse runs in SQL. This
// runs on SQLite here and on Postgres under the enttest Postgres backend
// (-tags integration with SCION_TEST_POSTGRES_URL), so the two dialects
// agree with Go and with each other.
func TestAppliedConfigKeepAgreesWithGo(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	docs := map[string]string{
		"null column":        "<NULL>",
		"empty":              "",
		"JSON null":          "null",
		"not JSON":           "{not json",
		"array":              "[]",
		"string document":    `"x"`,
		"recorded":           `{"nativeCloudRun":true,"profile":"cr"}`,
		"false":              `{"nativeCloudRun":false}`,
		"string true":        `{"nativeCloudRun":"true"}`,
		"number one":         `{"nativeCloudRun":1}`,
		"nested key only":    `{"inline":{"nativeCloudRun":true}}`,
		"absent":             `{"profile":"cr"}`,
		"spaced recorded":    `{ "nativeCloudRun" : true }`,
		"recorded among env": `{"env":{"A":"1"},"nativeCloudRun":true}`,
		"nul escape":         `{"task":"a\u0000b","nativeCloudRun":true}`,
		"lone surrogate":     `{"task":"\ud800","nativeCloudRun":true}`,
		"html escape":        `{"task":"a\u003cb","nativeCloudRun":true}`,
		"mixed-case key":     `{"NativeCloudRun":true}`,
		"all-caps key":       `{"NATIVECLOUDRUN":true}`,
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			a := makeAgent(projectID, "keep-"+strings.ToLower(strings.ReplaceAll(name, " ", "-")))
			require.NoError(t, s.CreateAgent(ctx, a))
			uid := uuid.MustParse(a.ID)
			upd := s.client.Agent.UpdateOneID(uid)
			stored := doc
			if doc == "<NULL>" {
				upd.ClearAppliedConfig()
				stored = ""
			} else {
				upd.SetAppliedConfig(doc)
			}
			require.NoError(t, upd.Exec(ctx))

			goNative := rowNativeCloudRun(stored)
			n, err := s.client.Agent.Query().Where(agent.IDEQ(uid), appliedConfigKeep(stored, goNative)).Count(ctx)
			require.NoError(t, err)
			assert.Equal(t, 1, n, "the keep predicate matches the value Go reads")
			if appliedConfigKeepUsesJSON(stored) {
				n, err = s.client.Agent.Query().Where(agent.IDEQ(uid), appliedNativeCloudRunIs(!goNative)).Count(ctx)
				require.NoError(t, err)
				assert.Zero(t, n, "and not the other value")
			}

			// GetAgent reads the decision exactly as the row reading and
			// the SQL predicate do.
			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, goNative, got.AppliedConfig != nil && got.AppliedConfig.NativeCloudRun, "GetAgent agrees")

			// A whole-row write on the row succeeds and keeps the value.
			got.Message = "touched"
			require.NoError(t, s.UpdateAgent(ctx, got))
			assert.Equal(t, goNative, storedNativeCloudRun(t, s, a.ID))
		})
	}
	assert.True(t, rowNativeCloudRun(docs["recorded"]))
	assert.True(t, rowNativeCloudRun(docs["spaced recorded"]))
	assert.False(t, rowNativeCloudRun(docs["string true"]))
	assert.False(t, rowNativeCloudRun(docs["not JSON"]))
	assert.True(t, rowNativeCloudRun(docs["nul escape"]))
	assert.False(t, appliedConfigKeepUsesJSON(docs["nul escape"]), "matched by its text, never cast")
	assert.False(t, appliedConfigKeepUsesJSON(docs["lone surrogate"]), "matched by its text, never cast")
	assert.False(t, appliedConfigKeepUsesJSON(docs["html escape"]), "matched by its text, never cast")
	assert.True(t, rowNativeCloudRun(docs["lone surrogate"]))
	marshalled := marshalAppliedConfig(&store.AgentAppliedConfig{Task: "a<b"})
	assert.Contains(t, marshalled, `\u003c`, "the hub's marshal escapes <")
	assert.False(t, appliedConfigKeepUsesJSON(marshalled))
	assert.False(t, rowNativeCloudRun(docs["mixed-case key"]), "the key is exact")
	assert.False(t, rowNativeCloudRun(docs["all-caps key"]), "the key is exact")
}

// A decision recorded between a whole-row write's read of the row and its
// save makes the save match no row; the store call then reports a version
// conflict, and a retry from the same copy keeps the decision.
func TestAgentStore_UpdateAgentRacesNativeRecord(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := newNativeAgent(t, s, projectID, "race-record")
	cur, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	cur.Phase = "stopped"
	uid := uuid.MustParse(a.ID)

	update, err := buildAgentUpdate(ctx, s.client.Agent, uid, cur, cur.StateVersion, cur.StateVersion+1, time.Now(), false)
	require.NoError(t, err)
	require.NoError(t, s.SetAgentNativeCloudRun(ctx, a.ID, true))
	n, err := update.Save(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "the write built before the record matches nothing")
	assert.ErrorIs(t, agentUpdateConflictError(ctx, s.client.Agent, uid), store.ErrVersionConflict)
	assert.True(t, storedNativeCloudRun(t, s, a.ID))

	require.NoError(t, s.UpdateAgent(ctx, cur), "a retry reads the row again")
	assert.True(t, storedNativeCloudRun(t, s, a.ID))
}

// The recorded decision survives every kind of applied_config writer and
// is read the same way by the exact-key reading, the SQL predicate and
// GetAgent; after a confirmed clear all three read false, and later
// writers do not raise it.
func TestNativeCloudRunRoundTripThroughWriters(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	cs := NewCompositeStore(s.client)
	a := makeAgent(projectID, "round-trip")
	a.AppliedConfig = &store.AgentAppliedConfig{Profile: "cr"}
	require.NoError(t, s.CreateAgent(ctx, a))
	uid := uuid.MustParse(a.ID)
	readers := func(t *testing.T, want bool, step string) {
		t.Helper()
		row, err := s.client.Agent.Get(ctx, uid)
		require.NoError(t, err)
		assert.Equal(t, want, rowNativeCloudRun(row.AppliedConfig), "%s: exact-key reading", step)
		n, err := s.client.Agent.Query().Where(agent.IDEQ(uid), appliedNativeCloudRunIs(true)).Count(ctx)
		require.NoError(t, err)
		assert.Equal(t, want, n == 1, "%s: SQL predicate", step)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, want, got.AppliedConfig != nil && got.AppliedConfig.NativeCloudRun, "%s: GetAgent", step)
	}
	writeAll := func(t *testing.T, carry bool) {
		t.Helper()
		cur, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		cur.AppliedConfig.NativeCloudRun = carry
		cur.AppliedConfig.Model = "m"
		require.NoError(t, s.UpdateAgent(ctx, cur))

		cur, err = s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		applied := *cur.AppliedConfig
		applied.NativeCloudRun = carry
		applied.Task = "t"
		_, err = s.UpdateAgentConfig(ctx, a.ID, cur.StateVersion, &applied, nil)
		require.NoError(t, err)

		current, err := s.client.Agent.Get(ctx, uid)
		require.NoError(t, err)
		harness := "hc-false"
		if carry {
			harness = "hc-true"
		}
		upd := s.client.Agent.UpdateOneID(uid)
		applySucceededAgentInfo(upd, current, store.LaunchReport{AgentConfig: store.BrokerAgentConfig{HarnessConfig: harness}})
		require.NoError(t, upd.Exec(ctx))
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.Equal(t, harness, got.AppliedConfig.HarnessConfig, "the launch report write landed")
	}

	require.NoError(t, s.SetAgentNativeCloudRun(ctx, a.ID, true))
	readers(t, true, "recorded")
	writeAll(t, false)
	readers(t, true, "after UpdateAgent, UpdateAgentConfig and the launch report write, each carrying false")

	row, err := s.client.Agent.Get(ctx, uid)
	require.NoError(t, err)
	done, err := cs.backfillAgentRole(ctx, row)
	require.NoError(t, err)
	require.True(t, done, "the agent has no role, so the backfill writes")
	readers(t, true, "after the role backfill")

	require.NoError(t, s.SetAgentNativeCloudRun(ctx, a.ID, false))
	readers(t, false, "after a confirmed clear")
	writeAll(t, true)
	readers(t, false, "writers carrying true do not raise it")
}

// A document SetAgentNativeCloudRun cannot edit as an object (invalid
// JSON, an array, a string) is refused with an error, so the dispatch that
// asked is refused, not served without a record.
func TestAgentStore_SetAgentNativeCloudRun_NonObjectDocument(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	for name, doc := range map[string]string{"not JSON": "{not json", "array": "[]", "string": `"x"`} {
		t.Run(name, func(t *testing.T) {
			a := makeAgent(projectID, "non-object-"+strings.ReplaceAll(name, " ", "-"))
			require.NoError(t, s.CreateAgent(ctx, a))
			require.NoError(t, s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).SetAppliedConfig(doc).Exec(ctx))
			assert.Error(t, s.SetAgentNativeCloudRun(ctx, a.ID, true))
		})
	}
}

// backfillTestAgent creates an agent with no role and no recorded native
// decision, and returns its stored row as the backfill would read it.
func backfillTestAgent(t *testing.T, s *AgentStore, projectID, slug string) (*store.Agent, *ent.Agent) {
	t.Helper()
	ctx := context.Background()
	a := makeAgent(projectID, slug)
	a.AppliedConfig = &store.AgentAppliedConfig{Profile: "p", Model: "m"}
	require.NoError(t, s.CreateAgent(ctx, a))
	row, err := s.client.Agent.Get(ctx, uuid.MustParse(a.ID))
	require.NoError(t, err)
	return a, row
}

// concurrentAppliedEdit changes the agent's applied config through
// UpdateAgent, as a writer racing the backfill would.
func concurrentAppliedEdit(t *testing.T, s *AgentStore, id string, edit func(c *store.AgentAppliedConfig)) {
	t.Helper()
	cur, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	edit(cur.AppliedConfig)
	require.NoError(t, s.UpdateAgent(context.Background(), cur))
}

// The role backfill writes, for an agent with no role and no recorded
// decision, exactly the document the unconditional write produced: the
// row's config with the grandfathered full role.
func TestBackfillAgentRole_PlainAgentAsBefore(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	cs := NewCompositeStore(s.client)
	_, row := backfillTestAgent(t, s, projectID, "backfill-plain")
	want, err := parseAppliedConfig(row.AppliedConfig)
	require.NoError(t, err)
	want.AgentRole = "full"
	want.AgentRoleGrandfathered = true

	done, err := cs.backfillAgentRole(ctx, row)
	require.NoError(t, err)
	assert.True(t, done)
	after, err := s.client.Agent.Get(ctx, row.ID)
	require.NoError(t, err)
	assert.Equal(t, marshalAppliedConfig(want), after.AppliedConfig, "the same bytes the unconditional write produced")
	assert.NotContains(t, after.AppliedConfig, nativeCloudRunKey)
}

// A concurrent edit between the backfill's read and its write makes the
// write match no row; the backfill reads the row again and fills the role
// on the edited document, keeping the edit.
func TestBackfillAgentRole_ReReadsAfterConcurrentEdit(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	cs := NewCompositeStore(s.client)
	a, stale := backfillTestAgent(t, s, projectID, "backfill-reread")
	concurrentAppliedEdit(t, s, a.ID, func(c *store.AgentAppliedConfig) { c.Task = "concurrent" })

	done, err := cs.backfillAgentRole(ctx, stale)
	require.NoError(t, err)
	assert.True(t, done)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "full", got.AppliedConfig.AgentRole)
	assert.True(t, got.AppliedConfig.AgentRoleGrandfathered)
	assert.Equal(t, "concurrent", got.AppliedConfig.Task, "the concurrent edit is kept")
	row, err := s.client.Agent.Get(ctx, stale.ID)
	require.NoError(t, err)
	assert.NotContains(t, row.AppliedConfig, nativeCloudRunKey)
}

// A role filled concurrently is left alone.
func TestBackfillAgentRole_LeavesConcurrentlyFilledRole(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	cs := NewCompositeStore(s.client)
	a, stale := backfillTestAgent(t, s, projectID, "backfill-filled")
	concurrentAppliedEdit(t, s, a.ID, func(c *store.AgentAppliedConfig) { c.AgentRole = "readonly" })

	done, err := cs.backfillAgentRole(ctx, stale)
	require.NoError(t, err)
	assert.False(t, done)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "readonly", got.AppliedConfig.AgentRole)
	assert.False(t, got.AppliedConfig.AgentRoleGrandfathered)
}

// When the document keeps changing past the bounded attempts, the backfill
// gives up with an error naming only the agent, and writes nothing: the
// row is byte-identical to the last concurrent write.
func TestBackfillAgentRole_GivesUpWithoutWriting(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	cs := NewCompositeStore(s.client)
	saved := backfillAgentRoleAttempts
	backfillAgentRoleAttempts = 1
	t.Cleanup(func() { backfillAgentRoleAttempts = saved })
	a, stale := backfillTestAgent(t, s, projectID, "backfill-giveup")
	concurrentAppliedEdit(t, s, a.ID, func(c *store.AgentAppliedConfig) { c.Task = "concurrent" })
	before, err := s.client.Agent.Get(ctx, stale.ID)
	require.NoError(t, err)

	done, err := cs.backfillAgentRole(ctx, stale)
	require.Error(t, err)
	assert.False(t, done)
	assert.Contains(t, err.Error(), stale.ID.String())
	assert.NotContains(t, err.Error(), "concurrent", "the document is not named")
	after, err := s.client.Agent.Get(ctx, stale.ID)
	require.NoError(t, err)
	assert.Equal(t, before.AppliedConfig, after.AppliedConfig, "nothing was written")
}
