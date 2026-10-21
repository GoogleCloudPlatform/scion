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
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/conversationparticipant"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// groupCleanupFixture holds one participant row per shape the cleanup has
// to remove or keep.
type groupCleanupFixture struct {
	srv    *Server
	s      store.Store
	client *ent.Client

	group   *store.Conversation // the group conversation holding most rows
	group2  *store.Conversation // a second group conversation
	direct  *store.Conversation // a direct conversation
	deleted *store.Conversation // a soft-deleted group conversation

	// Agents whose rows in group are removed.
	none, lineage, branch, softDeleted, missing string
	// Agents whose rows in group are kept.
	project, hub string
	// A user row in group (kept).
	user string
	// A none agent whose row in group was already left before the run.
	leftNone      string
	leftNoneLeftA time.Time
}

// groupCleanupRow identifies one participant row.
type groupCleanupRow struct{ conv, agent string }

func newGroupCleanupFixture(t *testing.T) *groupCleanupFixture {
	t.Helper()
	ctx := context.Background()
	cs, client, err := newTestStoreWithClient(t)
	require.NoError(t, err)
	srv, s := testServerOnMigratedStore(t, cs)
	f := &groupCleanupFixture{srv: srv, s: s, client: client}

	projectID := uuid.NewString()
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Name: "gpc", Slug: "gpc-" + projectID[:8]}))

	agent := func(mode string, deleted bool) string {
		if !deleted {
			return groupModeAgent(t, s, projectID, "", mode).ID
		}
		id := uuid.NewString()
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{
			ID: id, Name: "gpc-deleted-" + id[:8], Slug: "gpc-deleted-" + id[:8],
			ProjectID: projectID, MessageMode: mode, DeletedAt: time.Now(),
		}))
		got, err := s.GetAgent(ctx, id)
		require.NoError(t, err)
		require.False(t, got.DeletedAt.IsZero(), "fixture agent must be soft-deleted")
		return id
	}
	f.none = agent(store.MessageModeNone, false)
	f.lineage = agent(store.MessageModeLineage, false)
	f.branch = agent(store.MessageModeBranch, false)
	f.project = agent(store.MessageModeProject, false)
	f.hub = agent(store.MessageModeHub, false)
	f.softDeleted = agent(store.MessageModeProject, true)
	f.leftNone = agent(store.MessageModeNone, false)
	f.missing = uuid.NewString()
	f.user = uuid.NewString()

	f.group = groupModeConversation(t, s, projectID, "")
	f.group2 = groupModeConversation(t, s, projectID, "")
	f.deleted = groupModeConversation(t, s, projectID, "")

	add := func(convID, kind, id string) {
		require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
			ConversationID: convID, PrincipalKind: kind, PrincipalID: id, Role: "member",
		}))
	}
	for _, id := range []string{f.none, f.lineage, f.branch, f.project, f.hub, f.softDeleted, f.missing, f.leftNone} {
		add(f.group.ID, "agent", id)
	}
	add(f.group.ID, "user", f.user)
	add(f.group2.ID, "agent", f.lineage)
	add(f.group2.ID, "agent", f.hub)
	add(f.deleted.ID, "agent", f.none)

	require.NoError(t, s.RemoveParticipant(ctx, f.group.ID, "agent", f.leftNone))
	leftAt, active := f.rowState(t, f.group.ID, "agent", f.leftNone)
	require.False(t, active)
	f.leftNoneLeftA = leftAt

	require.NoError(t, s.DeleteConversation(ctx, f.deleted.ID))

	// A direct conversation between a user and the none agent.
	key, err := messages.DMConversationKey("user", f.user, "agent", f.none)
	require.NoError(t, err)
	f.direct, err = s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind: "direct", Surface: "native", ExternalRef: key, DriftState: "active",
	})
	require.NoError(t, err)
	add(f.direct.ID, "agent", f.none)
	add(f.direct.ID, "user", f.user)
	return f
}

// rowState reads one participant row past the store, which lists only
// active rows: its left_at (zero when active) and whether it is active.
func (f *groupCleanupFixture) rowState(t *testing.T, convID, kind, id string) (time.Time, bool) {
	t.Helper()
	row, err := f.client.ConversationParticipant.Query().
		Where(
			conversationparticipant.ConversationIDEQ(uuid.MustParse(convID)),
			conversationparticipant.PrincipalKindEQ(conversationparticipant.PrincipalKind(kind)),
			conversationparticipant.PrincipalIDEQ(id),
		).
		Only(context.Background())
	require.NoError(t, err, "row %s/%s/%s must exist", convID, kind, id)
	if row.LeftAt == nil {
		return time.Time{}, true
	}
	return *row.LeftAt, false
}

// removedRows are the rows a run removes, with their reasons.
func (f *groupCleanupFixture) removedRows() map[groupCleanupRow]string {
	return map[groupCleanupRow]string{
		{f.group.ID, f.none}:        groupCleanupReasonNone,
		{f.group.ID, f.lineage}:     groupCleanupReasonLineage,
		{f.group.ID, f.branch}:      groupCleanupReasonBranch,
		{f.group.ID, f.softDeleted}: groupCleanupReasonDeleted,
		{f.group.ID, f.missing}:     groupCleanupReasonMissing,
		{f.group2.ID, f.lineage}:    groupCleanupReasonLineage,
	}
}

// keptActiveRows are the active rows a run never touches.
func (f *groupCleanupFixture) keptActiveRows() []struct{ conv, kind, id, why string } {
	return []struct{ conv, kind, id, why string }{
		{f.group.ID, "agent", f.project, "project mode agent"},
		{f.group.ID, "agent", f.hub, "hub mode agent"},
		{f.group2.ID, "agent", f.hub, "hub mode agent"},
		{f.group.ID, "user", f.user, "user row"},
		{f.direct.ID, "agent", f.none, "direct conversation"},
		{f.direct.ID, "user", f.user, "direct conversation"},
		{f.deleted.ID, "agent", f.none, "soft-deleted conversation"},
	}
}

func (f *groupCleanupFixture) run(t *testing.T, params map[string]string) (groupParticipantCleanupResult, string) {
	t.Helper()
	var buf bytes.Buffer
	res, err := (&GroupParticipantModeCleanupExecutor{Store: f.s}).run(context.Background(), &buf, params)
	require.NoError(t, err, "log:\n%s", buf.String())
	return res, buf.String()
}

// listedRows parses the "conversation=<id> agent=<id> reason=<r>" lines of
// a run log.
func listedRows(t *testing.T, log, verb string) map[groupCleanupRow]string {
	t.Helper()
	got := map[groupCleanupRow]string{}
	for _, line := range strings.Split(log, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, verb+" conversation=") {
			continue
		}
		var row groupCleanupRow
		var reason string
		for _, field := range strings.Fields(strings.TrimPrefix(line, verb)) {
			k, v, _ := strings.Cut(field, "=")
			switch k {
			case "conversation":
				row.conv = v
			case "agent":
				row.agent = v
			case "reason":
				reason = v
			}
		}
		_, dup := got[row]
		require.False(t, dup, "row listed twice: %s", line)
		got[row] = reason
	}
	return got
}

// assertGroupCleanupUntouched checks that no row changed: every row the
// fixture created is still active, except the already-left row, whose
// left_at is unchanged.
func assertGroupCleanupUntouched(t *testing.T, f *groupCleanupFixture) {
	t.Helper()
	for row := range f.removedRows() {
		_, active := f.rowState(t, row.conv, "agent", row.agent)
		assert.True(t, active, "row %v must stay active", row)
	}
	for _, k := range f.keptActiveRows() {
		_, active := f.rowState(t, k.conv, k.kind, k.id)
		assert.True(t, active, "%s row must stay active", k.why)
	}
	assertGroupCleanupLeftRowUnchanged(t, f)
}

func assertGroupCleanupLeftRowUnchanged(t *testing.T, f *groupCleanupFixture) {
	t.Helper()
	leftAt, active := f.rowState(t, f.group.ID, "agent", f.leftNone)
	require.False(t, active, "already-left row must stay left")
	assert.True(t, leftAt.Equal(f.leftNoneLeftA), "already-left row must keep its left_at: was %v, now %v", f.leftNoneLeftA, leftAt)
}

// TestGroupParticipantModeCleanup_ReportOnlyByDefault runs without apply,
// with and without dryRun, and with apply together with dryRun: each run
// lists exactly the removable rows and changes nothing.
func TestGroupParticipantModeCleanup_ReportOnlyByDefault(t *testing.T) {
	for name, params := range map[string]map[string]string{
		"no params":        nil,
		"dryRun":           {"dryRun": "true"},
		"apply and dryRun": {"apply": "true", "dryRun": "true"},
		"apply not true":   {"apply": "yes"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newGroupCleanupFixture(t)
			res, log := f.run(t, params)

			assert.False(t, res.Applied)
			assert.Contains(t, log, "REPORT ONLY")
			assert.Equal(t, f.removedRows(), listedRows(t, log, "WOULD REMOVE"))
			assert.Empty(t, listedRows(t, log, "REMOVE"))
			assert.Equal(t, len(f.removedRows()), res.Matched)
			assert.Equal(t, 0, res.Removed)
			assert.Equal(t, 0, res.AlreadyLeft)
			assert.Equal(t, map[string]int{
				groupCleanupReasonNone:    1,
				groupCleanupReasonLineage: 2,
				groupCleanupReasonBranch:  1,
				groupCleanupReasonDeleted: 1,
				groupCleanupReasonMissing: 1,
			}, res.ByReason)
			assert.Contains(t, log, "Would remove 6 row(s) (agent_deleted=1, agent_missing=1, branch=1, lineage=2, none=1).")
			assertGroupCleanupUntouched(t, f)
		})
	}
}

// TestGroupParticipantModeCleanup_ApplyRemovesOnlyMatchingRows applies the
// cleanup: each removable row is marked left, every other row is kept.
func TestGroupParticipantModeCleanup_ApplyRemovesOnlyMatchingRows(t *testing.T) {
	f := newGroupCleanupFixture(t)
	before := time.Now()
	res, log := f.run(t, map[string]string{"apply": "true"})

	assert.True(t, res.Applied)
	assert.NotContains(t, log, "REPORT ONLY")
	assert.Equal(t, f.removedRows(), listedRows(t, log, "REMOVE"))
	assert.Equal(t, len(f.removedRows()), res.Removed)
	assert.Equal(t, 0, res.Failed)
	assert.Equal(t, 0, res.AlreadyLeft)
	assert.Equal(t, 2, res.ConversationsScanned, "group and group2; the direct and the soft-deleted conversations are not scanned")
	assert.Equal(t, 3, res.Kept)
	assert.Contains(t, log, "Removed 6 row(s) (agent_deleted=1, agent_missing=1, branch=1, lineage=2, none=1).")

	for row := range f.removedRows() {
		leftAt, active := f.rowState(t, row.conv, "agent", row.agent)
		assert.False(t, active, "row %v must be marked left", row)
		assert.False(t, leftAt.Before(before.Add(-time.Second)), "row %v left_at %v must be set by this run", row, leftAt)
	}
	for _, k := range f.keptActiveRows() {
		_, active := f.rowState(t, k.conv, k.kind, k.id)
		assert.True(t, active, "%s row must be kept", k.why)
	}
}

// TestGroupParticipantModeCleanup_KeepsAlreadyLeftRow checks that a row left
// before the run is neither listed nor written, in a report-only and in an
// applied run.
func TestGroupParticipantModeCleanup_KeepsAlreadyLeftRow(t *testing.T) {
	f := newGroupCleanupFixture(t)
	leftRow := groupCleanupRow{f.group.ID, f.leftNone}

	_, log := f.run(t, nil)
	_, listed := listedRows(t, log, "WOULD REMOVE")[leftRow]
	assert.False(t, listed, "already-left row must not be listed by a report-only run")

	res, log := f.run(t, map[string]string{"apply": "true"})
	_, listed = listedRows(t, log, "REMOVE")[leftRow]
	assert.False(t, listed, "already-left row must not be listed by an applied run")
	assert.Equal(t, 0, res.AlreadyLeft, "already-left row must not be attempted")
	assertGroupCleanupLeftRowUnchanged(t, f)
}

// TestGroupParticipantModeCleanup_KeepsHubModeAgent checks the hub mode
// agent's rows on their own, so a change to the kept modes fails here with
// a named cause.
func TestGroupParticipantModeCleanup_KeepsHubModeAgent(t *testing.T) {
	f := newGroupCleanupFixture(t)
	_, log := f.run(t, map[string]string{"apply": "true"})
	for _, conv := range []string{f.group.ID, f.group2.ID} {
		_, active := f.rowState(t, conv, "agent", f.hub)
		assert.True(t, active, "hub mode agent row was removed in conversation %s", conv)
		_, listed := listedRows(t, log, "REMOVE")[groupCleanupRow{conv, f.hub}]
		assert.False(t, listed, "hub mode agent row was listed in conversation %s", conv)
	}
}

// TestGroupParticipantModeCleanup_RerunRemovesNothing applies the cleanup
// twice: the second run removes 0 and changes no row.
func TestGroupParticipantModeCleanup_RerunRemovesNothing(t *testing.T) {
	f := newGroupCleanupFixture(t)
	f.run(t, map[string]string{"apply": "true"})

	leftAts := map[groupCleanupRow]time.Time{}
	for row := range f.removedRows() {
		leftAt, active := f.rowState(t, row.conv, "agent", row.agent)
		require.False(t, active)
		leftAts[row] = leftAt
	}

	res, log := f.run(t, map[string]string{"apply": "true"})
	assert.Equal(t, 0, res.Removed)
	assert.Equal(t, 0, res.Matched)
	assert.Equal(t, 0, res.AlreadyLeft)
	assert.Empty(t, listedRows(t, log, "REMOVE"))
	assert.Contains(t, log, "Removed 0 row(s).")
	for row, want := range leftAts {
		leftAt, active := f.rowState(t, row.conv, "agent", row.agent)
		assert.False(t, active)
		assert.True(t, leftAt.Equal(want), "row %v left_at changed on re-run", row)
	}
	for _, k := range f.keptActiveRows() {
		_, active := f.rowState(t, k.conv, k.kind, k.id)
		assert.True(t, active, "%s row must be kept", k.why)
	}
	assertGroupCleanupLeftRowUnchanged(t, f)
}

// TestGroupParticipantModeCleanup_ReAddAfterCleanup checks that an agent
// removed by the cleanup can be added back, which re-activates its row.
func TestGroupParticipantModeCleanup_ReAddAfterCleanup(t *testing.T) {
	f := newGroupCleanupFixture(t)
	f.run(t, map[string]string{"apply": "true"})
	require.NoError(t, f.s.AddParticipant(context.Background(), &store.ConversationParticipant{
		ConversationID: f.group.ID, PrincipalKind: "agent", PrincipalID: f.lineage, Role: "member",
	}))
	_, active := f.rowState(t, f.group.ID, "agent", f.lineage)
	assert.True(t, active)
}

// TestGroupParticipantModeCleanup_LogHasNoUserIDs checks that the run log
// names agents and conversations only.
func TestGroupParticipantModeCleanup_LogHasNoUserIDs(t *testing.T) {
	f := newGroupCleanupFixture(t)
	for _, params := range []map[string]string{nil, {"apply": "true"}} {
		_, log := f.run(t, params)
		assert.NotContains(t, log, f.user)
		assert.NotContains(t, log, "@")
	}
}

// TestGroupParticipantModeCleanup_ModeMatchesJoinGate checks that the
// cleanup keeps exactly the rows of agents whose stored mode
// groupAgentJoinAllowed lets any adder add, including mode values in a
// different case or with whitespace. The gate does not normalize case or
// whitespace, so neither does the cleanup: such a value is removed.
func TestGroupParticipantModeCleanup_ModeMatchesJoinGate(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()
	wantKept := map[string]bool{"": true, "project": true, "hub": true}
	for _, mode := range []string{
		"", "project", "hub", "none", "lineage", "branch",
		"Hub", "HUB", " hub", "hub ", "Project", " project", "project\n",
		"None", " none", "Lineage", "BRANCH", "bogus",
	} {
		gate, _ := srv.groupAgentJoinAllowed(ctx, nil, &store.Agent{ID: uuid.NewString(), MessageMode: mode})
		keep := groupModeKeepsParticipant(mode)
		assert.Equal(t, gate, keep, "mode %q: cleanup keep must equal the join gate", mode)
		assert.Equal(t, wantKept[mode], keep, "mode %q", mode)

		reason := groupParticipantCleanupReason(&store.Agent{MessageMode: mode})
		assert.Equal(t, keep, reason == "", "mode %q: reason %q", mode, reason)
		if _, known := map[string]bool{"none": true, "lineage": true, "branch": true}[mode]; !keep && !known {
			assert.Equal(t, groupCleanupReasonUnknown, reason, "mode %q", mode)
		}
	}
}

// TestGroupParticipantModeCleanupRegistered checks the seeded migration and
// its executor.
func TestGroupParticipantModeCleanupRegistered(t *testing.T) {
	srv, s := newTestServerWithStore(t)

	op, err := s.GetMaintenanceOperation(context.Background(), entadapter.GroupParticipantModeCleanupKey)
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceCategoryMigration, op.Category)
	assert.Equal(t, store.MaintenanceStatusPending, op.Status)
	assert.Contains(t, op.Description, `{"params":{"apply":"true"}}`)
	assert.Contains(t, op.Description, "scion conversation join")
	assert.True(t, rerunnableMigrations[entadapter.GroupParticipantModeCleanupKey], "a report-only run completes the migration; the applied run must still be allowed")

	exec, err := srv.resolveMaintenanceExecutor(entadapter.GroupParticipantModeCleanupKey)
	require.NoError(t, err)
	assert.IsType(t, &GroupParticipantModeCleanupExecutor{}, exec)
}
