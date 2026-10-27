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

package runtimebroker

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Tests for releasing the reservation of a create that failed before its
// agent existed (ptone/scion#5768). Interleavings are forced in a fixed
// order; none depends on timing or on the race detector.

const abandonProjectID = "7c1d2e3f-4a5b-4c6d-8e9f-0a1b2c3d4e5f"

func soleRelease() func() bool { return func() bool { return true } }
func otherHolder() func() bool { return func() bool { return false } }
func begin(t *testing.T, s *OwnershipStore, agentID, slug, runID string) beganRun {
	t.Helper()
	b, err := s.beginRun(abandonProjectID, agentID, slug, runID)
	require.NoError(t, err)
	return b
}

func holderOf(t *testing.T, s *OwnershipStore, slug string) string {
	t.Helper()
	h, err := s.SlugHolder(abandonProjectID, slug)
	require.NoError(t, err)
	return h
}

func recordOf(t *testing.T, s *OwnershipStore, agentID string) (*OwnershipRecord, bool) {
	t.Helper()
	rec, ok, err := s.Get(abandonProjectID, agentID)
	require.NoError(t, err)
	return rec, ok
}

// TestOwnershipStore_AbandonCreateRun covers each rule of AbandonCreateRun.
func TestOwnershipStore_AbandonCreateRun(t *testing.T) {
	type setup func(t *testing.T, s *OwnershipStore) (b beganRun)
	fresh := func(t *testing.T, s *OwnershipStore) beganRun { return begin(t, s, "agent-a", "worker", "run-1") }
	cases := []struct {
		name         string
		setup        setup
		wroteNothing bool
		files        agent.FilesPresence
		release      func() bool
		want         abandonOutcome
		wantRecord   bool
		wantHolder   string
	}{
		{name: "created, wrote nothing", setup: fresh, wroteNothing: true, release: soleRelease(), want: abandonRecordDeleted},
		{name: "files proven absent", setup: fresh, files: agent.FilesAbsent, release: soleRelease(), want: abandonRecordDeleted},
		{name: "files present", setup: fresh, files: agent.FilesPresent, release: soleRelease(), want: abandonRunRemoved, wantRecord: true, wantHolder: "agent-a"},
		{name: "files unknown", setup: fresh, files: agent.FilesUnknown, release: soleRelease(), want: abandonRunRemoved, wantRecord: true, wantHolder: "agent-a"},
		{name: "another holder of the run", setup: fresh, wroteNothing: true, release: otherHolder(), want: abandonNone, wantRecord: true, wantHolder: "agent-a"},
		{name: "another run exists", setup: func(t *testing.T, s *OwnershipStore) beganRun {
			begin(t, s, "agent-a", "worker", "run-0")
			require.NoError(t, s.SetRunState(abandonProjectID, "agent-a", "run-0", OwnershipStateCreated))
			return begin(t, s, "agent-a", "worker", "run-1")
		}, files: agent.FilesAbsent, release: soleRelease(), want: abandonRunRemoved, wantRecord: true, wantHolder: "agent-a"},
		{name: "run recorded an object", setup: func(t *testing.T, s *OwnershipStore) beganRun {
			b := fresh(t, s)
			require.NoError(t, s.AddResource(abandonProjectID, "agent-a", "run-1", handle("uid-1")))
			b.revision++
			return b
		}, wroteNothing: true, release: soleRelease(), want: abandonNone, wantRecord: true, wantHolder: "agent-a"},
		{name: "run not provisioning", setup: func(t *testing.T, s *OwnershipStore) beganRun {
			b := fresh(t, s)
			require.NoError(t, s.SetRunState(abandonProjectID, "agent-a", "run-1", OwnershipStateCreated))
			b.revision++
			return b
		}, wroteNothing: true, release: soleRelease(), want: abandonNone, wantRecord: true, wantHolder: "agent-a"},
		{name: "record not active", setup: func(t *testing.T, s *OwnershipStore) beganRun {
			b := fresh(t, s)
			require.NoError(t, s.SetRecordState(abandonProjectID, "agent-a", OwnershipStateDeleting))
			b.revision++
			return b
		}, wroteNothing: true, release: soleRelease(), want: abandonNone, wantRecord: true, wantHolder: "agent-a"},
		{name: "revision changed since the reservation", setup: func(t *testing.T, s *OwnershipStore) beganRun {
			b := fresh(t, s)
			// Another run of the agent came and went (its abandon removed
			// its entry): the record has one run again, but another
			// request may have written files.
			begin(t, s, "agent-a", "worker", "run-2")
			_, err := s.AbandonCreateRun(abandonProjectID, "agent-a", "run-2", false, 0, agent.FilesPresent, soleRelease())
			require.NoError(t, err)
			return b
		}, wroteNothing: true, release: soleRelease(), want: abandonRunRemoved, wantRecord: true, wantHolder: "agent-a"},
		{name: "index names another agent", setup: func(t *testing.T, s *OwnershipStore) beganRun {
			b := fresh(t, s)
			sp, err := s.slugPath(abandonProjectID, "worker")
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(sp, []byte("agent-other\n"), 0o600))
			return b
		}, wroteNothing: true, release: soleRelease(), want: abandonRecordDeleted, wantHolder: "agent-other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewOwnershipStore(t.TempDir(), "broker-a")
			b := tc.setup(t, s)
			got, err := s.AbandonCreateRun(abandonProjectID, "agent-a", "run-1", tc.wroteNothing, b.revision, tc.files, tc.release)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			rec, ok := recordOf(t, s, "agent-a")
			assert.Equal(t, tc.wantRecord, ok, "record kept")
			if ok && tc.want == abandonRunRemoved {
				assert.Nil(t, rec.Run("run-1"), "the failed run's entry is removed, not marked deleted")
			}
			assert.Equal(t, tc.wantHolder, holderOf(t, s, "worker"))
		})
	}
}

// TestOwnershipStore_AbandonedRecordReservedAgainBySameRun: after either
// abandon outcome the Hub's identical resend (same agent ID and run ID)
// can reserve the slug again.
func TestOwnershipStore_AbandonedRecordReservedAgainBySameRun(t *testing.T) {
	s := NewOwnershipStore(t.TempDir(), "broker-a")
	b := begin(t, s, "agent-a", "worker", "run-1")
	got, err := s.AbandonCreateRun(abandonProjectID, "agent-a", "run-1", true, b.revision, agent.FilesUnknown, soleRelease())
	require.NoError(t, err)
	require.Equal(t, abandonRecordDeleted, got)
	b = begin(t, s, "agent-a", "worker", "run-1")
	assert.True(t, b.created, "the resend creates a fresh record")

	begin(t, s, "agent-a", "worker", "run-0")
	got, err = s.AbandonCreateRun(abandonProjectID, "agent-a", "run-1", false, b.revision, agent.FilesPresent, soleRelease())
	require.NoError(t, err)
	require.Equal(t, abandonRunRemoved, got)
	b = begin(t, s, "agent-a", "worker", "run-1")
	assert.False(t, b.created)
	rec, _ := recordOf(t, s, "agent-a")
	assert.Equal(t, OwnershipStateProvisioning, rec.Run("run-1").State, "the resend adds its run again")
}

// TestOwnershipStore_AbandonHoldsSlugMuAcrossBothRemovals: no other agent
// can reserve the slug between the index removal and the record removal.
func TestOwnershipStore_AbandonHoldsSlugMuAcrossBothRemovals(t *testing.T) {
	s := NewOwnershipStore(t.TempDir(), "broker-a")
	b := begin(t, s, "agent-a", "worker", "run-1")
	reached := false
	testHookBetweenSlugAndRecordRemoval = func() {
		reached = true
		if s.slugMu.TryLock() {
			s.slugMu.Unlock()
			t.Error("slugMu is free between the index removal and the record removal")
		}
		// The key lock is held here: read the file directly.
		_, ok, err := s.readFile(abandonProjectID, "agent-a")
		assert.NoError(t, err)
		assert.True(t, ok, "the index goes first")
	}
	t.Cleanup(func() { testHookBetweenSlugAndRecordRemoval = nil })
	_, err := s.AbandonCreateRun(abandonProjectID, "agent-a", "run-1", true, b.revision, agent.FilesUnknown, soleRelease())
	require.NoError(t, err)
	require.True(t, reached)
}

// TestOwnershipStore_AbandonVersusOtherAgent: an abandon and a different
// agent's reservation of the same slug, in both orders.
func TestOwnershipStore_AbandonVersusOtherAgent(t *testing.T) {
	t.Run("abandon first", func(t *testing.T) {
		s := NewOwnershipStore(t.TempDir(), "broker-a")
		b := begin(t, s, "agent-a", "worker", "run-1")
		_, err := s.AbandonCreateRun(abandonProjectID, "agent-a", "run-1", true, b.revision, agent.FilesUnknown, soleRelease())
		require.NoError(t, err)
		begin(t, s, "agent-b", "worker", "run-b")
		assert.Equal(t, "agent-b", holderOf(t, s, "worker"))
	})
	t.Run("other agent first", func(t *testing.T) {
		s := NewOwnershipStore(t.TempDir(), "broker-a")
		b := begin(t, s, "agent-a", "worker", "run-1")
		_, err := s.beginRun(abandonProjectID, "agent-b", "worker", "run-b")
		require.ErrorIs(t, err, ErrOwnershipSlugHeld)
		_, err = s.AbandonCreateRun(abandonProjectID, "agent-a", "run-1", true, b.revision, agent.FilesUnknown, soleRelease())
		require.NoError(t, err)
		begin(t, s, "agent-b", "worker", "run-b")
		assert.Equal(t, "agent-b", holderOf(t, s, "worker"))
	})
}

// endRun moves a run to deleted, its objects confirmed absent, as a failed
// start's cleanup does.
func endRun(t *testing.T, s *OwnershipStore, agentID, runID string, uids ...string) {
	t.Helper()
	for _, u := range uids {
		require.NoError(t, s.AddResource(abandonProjectID, agentID, runID, handle(u)))
	}
	require.NoError(t, s.SetRunState(abandonProjectID, agentID, runID, OwnershipStateDeleting))
	for _, u := range uids {
		require.NoError(t, s.MarkAbsent(abandonProjectID, agentID, u))
	}
	require.NoError(t, s.SetRunState(abandonProjectID, agentID, runID, OwnershipStateDeleted))
}

// TestOwnershipStore_RetireEndedCreate covers each rule of
// RetireEndedCreate.
func TestOwnershipStore_RetireEndedCreate(t *testing.T) {
	t.Run("never-objected run: record deleted", func(t *testing.T) {
		s := NewOwnershipStore(t.TempDir(), "broker-a")
		begin(t, s, "agent-a", "worker", "run-1")
		endRun(t, s, "agent-a", "run-1")
		ok, err := s.RetireEndedCreate(abandonProjectID, "agent-a", agent.FilesAbsent, soleRelease())
		require.NoError(t, err)
		assert.True(t, ok)
		_, exists := recordOf(t, s, "agent-a")
		assert.False(t, exists)
		assert.Empty(t, holderOf(t, s, "worker"))
	})
	t.Run("objects confirmed absent: tombstone", func(t *testing.T) {
		s := NewOwnershipStore(t.TempDir(), "broker-a")
		begin(t, s, "agent-a", "worker", "run-1")
		endRun(t, s, "agent-a", "run-1", "uid-1")
		ok, err := s.RetireEndedCreate(abandonProjectID, "agent-a", agent.FilesAbsent, soleRelease())
		require.NoError(t, err)
		assert.True(t, ok)
		rec, exists := recordOf(t, s, "agent-a")
		require.True(t, exists)
		assert.Equal(t, OwnershipStateDeleted, rec.State)
		assert.Empty(t, holderOf(t, s, "worker"))
	})
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, s *OwnershipStore)
		files agent.FilesPresence
		rel   func() bool
	}{
		{"files not proven absent", func(t *testing.T, s *OwnershipStore) { endRun(t, s, "agent-a", "run-1") }, agent.FilesUnknown, soleRelease()},
		{"another holder", func(t *testing.T, s *OwnershipStore) { endRun(t, s, "agent-a", "run-1") }, agent.FilesAbsent, otherHolder()},
		{"run not ended", func(t *testing.T, s *OwnershipStore) {}, agent.FilesAbsent, soleRelease()},
		{"record already deleted by the Hub's delete", func(t *testing.T, s *OwnershipStore) {
			endRun(t, s, "agent-a", "run-1")
			require.NoError(t, s.SetRecordState(abandonProjectID, "agent-a", OwnershipStateDeleted))
		}, agent.FilesAbsent, soleRelease()},
	} {
		t.Run(tc.name+": kept", func(t *testing.T) {
			s := NewOwnershipStore(t.TempDir(), "broker-a")
			begin(t, s, "agent-a", "worker", "run-1")
			tc.setup(t, s)
			before, _ := recordOf(t, s, "agent-a")
			ok, err := s.RetireEndedCreate(abandonProjectID, "agent-a", tc.files, tc.rel)
			require.NoError(t, err)
			assert.False(t, ok)
			after, exists := recordOf(t, s, "agent-a")
			require.True(t, exists)
			assert.Equal(t, before.Revision, after.Revision, "unchanged")
		})
	}
}

// TestOwnershipStore_RetireAbandonedRecords: the start-up retire.
func TestOwnershipStore_RetireAbandonedRecords(t *testing.T) {
	s := NewOwnershipStore(t.TempDir(), "broker-a")
	begin(t, s, "agent-s1", "s1", "run-1") // one provisioning run, no object
	begin(t, s, "agent-s2", "s2", "run-1") // every run ended
	endRun(t, s, "agent-s2", "run-1", "uid-2")
	begin(t, s, "agent-zero", "zero", "run-1") // no run left
	_, err := s.AbandonCreateRun(abandonProjectID, "agent-zero", "run-1", false, 0, agent.FilesPresent, soleRelease())
	require.NoError(t, err)
	begin(t, s, "agent-files", "files", "run-1") // proof keeps it
	begin(t, s, "agent-live", "live", "run-1")   // a live object
	require.NoError(t, s.AddResource(abandonProjectID, "agent-live", "run-1", handle("uid-live")))
	begin(t, s, "agent-new", "newer", "run-1") // a state this build does not know
	rec, _ := recordOf(t, s, "agent-new")
	rec.Runs[0].State = "suspended"
	p, _ := s.recordPath(abandonProjectID, "agent-new")
	require.NoError(t, writeOwnershipFile(p, mustJSON(rec)))
	begin(t, s, "agent-moved", "moved", "run-1") // index names another agent
	sp, _ := s.slugPath(abandonProjectID, "moved")
	require.NoError(t, os.WriteFile(sp, []byte("agent-elsewhere\n"), 0o600))
	begin(t, s, "agent-unindexed", "unindexed", "run-1") // crash left no index
	sp, _ = s.slugPath(abandonProjectID, "unindexed")
	require.NoError(t, os.Remove(sp))

	var warned []string
	n, err := s.RetireAbandonedRecords(func(r OwnershipRecord) agent.FilesPresence {
		if r.AgentSlug == "files" {
			return agent.FilesPresent
		}
		return agent.FilesAbsent
	}, func(msg string, args ...any) { warned = append(warned, args[1].(string)) })
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	for _, id := range []string{"agent-s1", "agent-zero", "agent-moved", "agent-unindexed"} {
		_, ok := recordOf(t, s, id)
		assert.False(t, ok, "%s retired", id)
	}
	rec, ok := recordOf(t, s, "agent-s2")
	require.True(t, ok)
	assert.Equal(t, OwnershipStateDeleted, rec.State, "a record whose objects existed becomes a tombstone")
	for _, id := range []string{"agent-files", "agent-live", "agent-new"} {
		rec, ok := recordOf(t, s, id)
		require.True(t, ok, "%s kept", id)
		assert.Equal(t, OwnershipStateActive, rec.State)
	}
	assert.Equal(t, []string{"agent-files"}, warned, "a candidate whose files are not proven absent is reported")
	assert.Equal(t, "agent-elsewhere", holderOf(t, s, "moved"), "an index entry naming another agent is never removed")
	for _, slug := range []string{"s1", "s2", "zero", "unindexed"} {
		assert.Empty(t, holderOf(t, s, slug))
	}
}

// abandonServer is a flat-instance server with only what the release path
// uses, for an instance whose runtime target type is targetType.
func abandonServer(t *testing.T, targetType string) *Server {
	t.Helper()
	return &Server{
		ownership:         NewOwnershipStore(t.TempDir(), "broker-a"),
		agentLifecycleLog: slog.New(slog.DiscardHandler),
		config: ServerConfig{FlatInstance: &FlatInstanceConfig{Instance: config.V1RuntimeBrokerInstanceConfig{
			RuntimeTarget: &config.V1RuntimeTargetConfig{Type: targetType}}}},
	}
}

// abandonProject creates, under a fresh HOME, a hub-managed project that
// identifies as abandonProjectID, and returns its directory.
func abandonProject(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	globalDir, err := config.GetGlobalDir()
	require.NoError(t, err)
	root := filepath.Join(globalDir, "projects", "abandon")
	scionDir := filepath.Join(root, config.DotScion)
	require.NoError(t, os.MkdirAll(filepath.Join(scionDir, "agents"), 0o755))
	require.NoError(t, config.WriteProjectID(scionDir, abandonProjectID))
	return root
}

// reserve reserves a create's run the way buildStartContext does and
// returns its release state.
func reserve(t *testing.T, s *Server, agentID, slug, runID, projectPath string) *ownedCreateAbandon {
	t.Helper()
	k := ownedRunKey{projectID: abandonProjectID, agentID: agentID, runID: runID}
	hold := s.holdOwnedRun(k)
	b, err := s.beginOwnedRunInfo(abandonProjectID, agentID, slug, runID, true)
	require.NoError(t, err)
	return &ownedCreateAbandon{armed: true, key: k, slug: slug, name: slug, projectPath: projectPath,
		hubProjectID: abandonProjectID, began: b, hold: hold}
}

// TestOwnedFilesProof covers the single files proof every release uses.
func TestOwnedFilesProof(t *testing.T) {
	root := abandonProject(t)
	docker, k8s := abandonServer(t, "docker"), abandonServer(t, "kubernetes")
	assert.Equal(t, agent.FilesAbsent, docker.ownedFilesProof(abandonProjectID, "worker", root, abandonProjectID))
	assert.Equal(t, agent.FilesAbsent, docker.ownedFilesProof(abandonProjectID, "worker", "", abandonProjectID), "an empty path resolves to the one hub-managed project")
	assert.Equal(t, agent.FilesUnknown, k8s.ownedFilesProof(abandonProjectID, "worker", root, abandonProjectID), "never absent on Kubernetes")
	assert.Equal(t, agent.FilesUnknown, docker.ownedFilesProof("0e9d8c7b-6a5f-4e3d-8c2b-1a0f9e8d7c6b", "worker", root, ""), "the path must identify as the project")
	assert.Equal(t, agent.FilesUnknown, docker.ownedFilesProof(abandonProjectID, "worker", t.TempDir(), abandonProjectID))
	require.NoError(t, os.MkdirAll(filepath.Join(root, config.DotScion, "agents", "worker"), 0o755))
	assert.Equal(t, agent.FilesPresent, docker.ownedFilesProof(abandonProjectID, "worker", root, abandonProjectID))
	// Storage blocks are global-only (Layer-0, ptone/scion#5821): a project's
	// own settings.yaml cannot place storage, so the nfs backend is set in the
	// broker's global settings.
	newTestGlobalSettings(t, "schema_version: \"1\"\nserver:\n  project_storage:\n    backend: nfs\n")
	require.NoError(t, os.RemoveAll(filepath.Join(root, config.DotScion, "agents", "worker")))
	assert.Equal(t, agent.FilesUnknown, docker.ownedFilesProof(abandonProjectID, "worker", root, abandonProjectID), "an nfs-backed project is never proven absent")
}

// TestAbandonOwnedCreate_RuntimeAwareProof: a request that did not create
// the record releases the slug only through the files proof, which is
// never absent on a Kubernetes instance.
func TestAbandonOwnedCreate_RuntimeAwareProof(t *testing.T) {
	for _, tc := range []struct {
		target  string
		deleted bool
	}{{"docker", true}, {"kubernetes", false}} {
		t.Run(tc.target, func(t *testing.T) {
			root := abandonProject(t)
			s := abandonServer(t, tc.target)
			first := reserve(t, s, "agent-a", "worker", "run-1", root)
			first.hold.release()
			resend := reserve(t, s, "agent-a", "worker", "run-1", root) // the same run again: created=false
			require.False(t, resend.began.created)
			s.abandonOwnedCreate(context.Background(), resend)
			rec, ok := recordOf(t, s.ownership, "agent-a")
			if tc.deleted {
				assert.False(t, ok, "files proven absent: record deleted")
				assert.Empty(t, holderOf(t, s.ownership, "worker"))
				return
			}
			require.True(t, ok, "files not provable on Kubernetes: record kept")
			assert.Nil(t, rec.Run("run-1"), "only the run is removed")
			assert.Equal(t, "agent-a", holderOf(t, s.ownership, "worker"))
		})
	}
}

// TestAbandonOwnedCreate_Holds: with two in-flight holders of one run,
// exactly the last one to fail releases the reservation.
func TestAbandonOwnedCreate_Holds(t *testing.T) {
	s := abandonServer(t, "docker")
	a := reserve(t, s, "agent-a", "worker", "run-1", "")
	// A second in-flight request of the same run (its reservation is the
	// same record, unchanged).
	b := *a
	b.hold = s.holdOwnedRun(a.key)
	s.abandonOwnedCreate(context.Background(), a)
	_, ok := recordOf(t, s.ownership, "agent-a")
	require.True(t, ok, "the other request still holds the run")
	s.abandonOwnedCreate(context.Background(), &b)
	_, ok = recordOf(t, s.ownership, "agent-a")
	assert.False(t, ok, "the last holder releases it")
	assert.Empty(t, holderOf(t, s.ownership, "worker"))
}

// TestAbandonOwnedCreate_ResendFailsBeforeFirstRequestEnds: the first
// request drops its hold at its abandon, not when it ends, so an identical
// resend failing in between still releases the slug.
func TestAbandonOwnedCreate_ResendFailsBeforeFirstRequestEnds(t *testing.T) {
	s := abandonServer(t, "docker")
	first := reserve(t, s, "agent-a", "worker", "run-1", "")
	s.abandonOwnedCreate(context.Background(), first)
	// first's request has not returned (finishOwnedCreate not run).
	resend := reserve(t, s, "agent-a", "worker", "run-1", "")
	s.abandonOwnedCreate(context.Background(), resend)
	_, ok := recordOf(t, s.ownership, "agent-a")
	assert.False(t, ok)
	assert.Empty(t, holderOf(t, s.ownership, "worker"))
	s.finishOwnedCreate(context.Background(), nil, first)
	assert.Empty(t, s.ownedHolds, "no hold left behind")
}

// TestAbandonOwnedCreate_RevisionGuard: a concurrent request of the same
// agent that wrote files and removed its run changes the record revision,
// so the request that created the record cannot use "wrote nothing".
func TestAbandonOwnedCreate_RevisionGuard(t *testing.T) {
	root := abandonProject(t)
	s := abandonServer(t, "docker")
	a := reserve(t, s, "agent-a", "worker", "run-a", root)
	b := reserve(t, s, "agent-a", "worker", "run-b", root)
	b.markWriting()
	require.NoError(t, os.MkdirAll(filepath.Join(root, config.DotScion, "agents", "worker"), 0o755))
	s.abandonOwnedCreate(context.Background(), b)
	s.abandonOwnedCreate(context.Background(), a)
	_, ok := recordOf(t, s.ownership, "agent-a")
	assert.True(t, ok, "kept: b's files exist")
	assert.Equal(t, "agent-a", holderOf(t, s.ownership, "worker"))
}

// TestAbandonOwnedCreate_UnderReservationLock: the commit runs under the
// host's slug reservation lock, so no other instance reserves the slug
// between the index removal and the record removal.
func TestAbandonOwnedCreate_UnderReservationLock(t *testing.T) {
	s := abandonServer(t, "docker")
	a := reserve(t, s, "agent-a", "worker", "run-1", "")
	reached := false
	testHookBetweenSlugAndRecordRemoval = func() {
		reached = true
		if s.locks().reserveMu.TryLock() {
			s.locks().reserveMu.Unlock()
			t.Error("reserveMu is free during the commit")
		}
	}
	t.Cleanup(func() { testHookBetweenSlugAndRecordRemoval = nil })
	s.abandonOwnedCreate(context.Background(), a)
	require.True(t, reached)
}

// TestRetireEndedCreate_RuntimeAware: a failed create's cleanup releases
// the slug only when the files proof finds nothing, which never happens on
// a Kubernetes instance.
func TestRetireEndedCreate_RuntimeAware(t *testing.T) {
	for _, tc := range []struct {
		target  string
		retired bool
	}{{"docker", true}, {"kubernetes", false}} {
		t.Run(tc.target, func(t *testing.T) {
			root := abandonProject(t)
			s := abandonServer(t, tc.target)
			a := reserve(t, s, "agent-a", "worker", "run-1", root)
			endRun(t, s.ownership, "agent-a", "run-1")
			removed := s.filesRemovedAfterCleanup(abandonProjectID, "worker", root, abandonProjectID)
			assert.Equal(t, tc.retired, removed)
			s.retireEndedCreate(a.key, removed, a.hold)
			_, ok := recordOf(t, s.ownership, "agent-a")
			assert.Equal(t, !tc.retired, ok)
		})
	}
}

// Keep api imported for handle's kinds.
var _ = api.ResourceKindContainer
