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

package artifacts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestListLiveOwnedArtifacts (S1): filters by owner and scope, excludes deleted
// and attachment-scoped rows, and paginates deterministically by id.
func TestListLiveOwnedArtifacts(t *testing.T) {
	for _, b := range testBackends() {
		t.Run(b.name, func(t *testing.T) {
			db, _ := b.open(t)
			st := NewStore(db, b.driver)
			ctx := context.Background()
			if err := st.Init(ctx); err != nil {
				t.Fatal(err)
			}

			child := uuid.NewString()
			otherAgent := uuid.NewString()
			now := time.Now().UTC().Truncate(time.Microsecond)

			// 3 live artifacts in proj-1 owned by child.
			a1 := seedAgentArtifact(t, st, child, "proj-1", "", "user-1")
			a2 := seedAgentArtifact(t, st, child, "proj-1", "", "user-1")
			a3 := seedAgentArtifact(t, st, child, "proj-1", "", "user-1")
			// Soft-deleted artifact in proj-1 owned by child -> excluded.
			deleted := seedAgentArtifact(t, st, child, "proj-1", "", "user-1")
			if _, err := st.DeleteArtifact(ctx, deleted.ID, DeleteOptions{Actor: testActor, Now: now}); err != nil {
				t.Fatal(err)
			}
			// Live artifact in proj-2 owned by child -> excluded.
			_ = seedAgentArtifact(t, st, child, "proj-2", "", "user-1")
			// Live artifact in proj-1 owned by otherAgent -> excluded.
			_ = seedAgentArtifact(t, st, otherAgent, "proj-1", "", "user-1")
			// Attachment-scoped row owned by child -> excluded.
			att := &Artifact{
				ID:        uuid.NewString(),
				ScopeKind: ScopeKindUpload,
				ScopeRef:  PrincipalRef(PrincipalKindAgent, child),
				OwnerKind: PrincipalKindAgent,
				OwnerRef:  child,
				Title:     "att.png",
				CreatedAt: now,
				UpdatedAt: now,
			}
			attV := &Version{
				ID:            uuid.NewString(),
				ArtifactID:    att.ID,
				Seq:           1,
				Kind:          VersionKindAttachment,
				EntryPath:     "att.png",
				TotalBytes:    4,
				FileCount:     1,
				CreatedByKind: PrincipalKindAgent,
				CreatedByRef:  child,
				CreatedAt:     now,
				State:         VersionStateReady,
			}
			attF := File{VersionID: attV.ID, Path: "att.png", Size: 4, SHA256: sha([]byte("data")), MediaType: "image/png"}
			if err := st.CreatePublished(ctx, att, attV, []File{attF}, nil); err != nil {
				t.Fatal(err)
			}

			wantIDs := []string{a1.ID, a2.ID, a3.ID}
			slices.Sort(wantIDs)

			owner := Owner{Kind: PrincipalKindAgent, Ref: child}
			// Paginate 2 at a time.
			page1, err := st.ListLiveOwnedArtifacts(ctx, owner, ScopeKindProject, "proj-1", "", 2)
			if err != nil {
				t.Fatal(err)
			}
			if len(page1) != 2 || page1[0].ID != wantIDs[0] || page1[1].ID != wantIDs[1] {
				t.Fatalf("page1 = %+v, want %v", page1, wantIDs[:2])
			}
			page2, err := st.ListLiveOwnedArtifacts(ctx, owner, ScopeKindProject, "proj-1", page1[1].ID, 2)
			if err != nil {
				t.Fatal(err)
			}
			if len(page2) != 1 || page2[0].ID != wantIDs[2] || page2[0].ScopeRef != "proj-1" {
				t.Fatalf("page2 = %+v, want [%s]", page2, wantIDs[2])
			}

			// Attachment scope is never returned even if explicitly queried.
			attRows, err := st.ListLiveOwnedArtifacts(ctx, owner, ScopeKindUpload, PrincipalRef(PrincipalKindAgent, child), "", 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(attRows) != 0 {
				t.Fatalf("attachment rows = %+v, want empty", attRows)
			}
		})
	}
}

// TestTransferToSuccessor_HappyPath (S2 & S6): owner moves to To,
// former_owner_ref=child, orphaned_at cleared, artifact_version.created_by_ref
// preserved as child, owner_transferred event and decision log recorded.
func TestTransferToSuccessor_HappyPath(t *testing.T) {
	for _, b := range testBackends() {
		t.Run(b.name, func(t *testing.T) {
			db, _ := b.open(t)
			st := NewStore(db, b.driver)
			ctx := context.Background()
			if err := st.Init(ctx); err != nil {
				t.Fatal(err)
			}

			child := uuid.NewString()
			parent := uuid.NewString()
			now := time.Now().UTC().Truncate(time.Microsecond)

			a := seedAgentArtifact(t, st, child, "proj-1", "report-k", "user-root")
			if _, err := st.SetOwnerOrphaned(ctx, Owner{Kind: PrincipalKindAgent, Ref: child}, true, now.Add(-time.Minute), 10); err != nil {
				t.Fatal(err)
			}

			capLog := &logCapture{}
			logger := slog.New(slog.NewJSONHandler(capLog, &slog.HandlerOptions{Level: slog.LevelDebug}))

			var callbacks []string
			res, err := TransferToSuccessor(ctx, st, Succession{
				From:      Owner{Kind: PrincipalKindAgent, Ref: child},
				ProjectID: "proj-1",
				ToAgentID: parent,
				Sponsor:   "user-root",
				Trigger:   SuccessionTriggerAgentDelete,
				OnTransferred: func(art *Artifact, _ TransferResult) {
					callbacks = append(callbacks, art.ID)
				},
			}, now, logger)
			if err != nil {
				t.Fatalf("TransferToSuccessor: %v", err)
			}
			if res != (SuccessionResult{Transferred: 1}) {
				t.Fatalf("res = %+v, want {Transferred:1}", res)
			}
			if len(callbacks) != 1 || callbacks[0] != a.ID {
				t.Fatalf("callbacks = %v, want [%s]", callbacks, a.ID)
			}

			got, err := st.GetArtifact(ctx, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.OwnerKind != PrincipalKindAgent || got.OwnerRef != parent {
				t.Fatalf("owner = %s:%s, want agent:%s", got.OwnerKind, got.OwnerRef, parent)
			}
			if got.FormerOwnerKind != PrincipalKindAgent || got.FormerOwnerRef != child {
				t.Fatalf("former owner = %s:%s, want agent:%s", got.FormerOwnerKind, got.FormerOwnerRef, child)
			}
			if got.OrphanedAt != nil {
				t.Fatalf("orphaned_at = %v, want nil", got.OrphanedAt)
			}
			if got.Key != "report-k" || got.FormerKey != "" {
				t.Fatalf("key=%q formerKey=%q, want report-k / empty", got.Key, got.FormerKey)
			}

			v, err := st.GetVersion(ctx, a.ID, 1)
			if err != nil {
				t.Fatal(err)
			}
			if v.CreatedByKind != PrincipalKindAgent || v.CreatedByRef != child {
				t.Fatalf("version created_by = %s:%s, want agent:%s", v.CreatedByKind, v.CreatedByRef, child)
			}

			events, err := st.ListEvents(ctx, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			var foundEv *Event
			for i := range events {
				if events[i].Action == EventOwnerTransferred {
					foundEv = &events[i]
				}
			}
			if foundEv == nil {
				t.Fatalf("missing %s event in %+v", EventOwnerTransferred, events)
			}
			if foundEv.ActorKind != ActorKindSystem || foundEv.ActorRef != SuccessionTriggerAgentDelete || foundEv.Authority != AuthorityAncestorSuccession {
				t.Fatalf("event actor = %s/%s/%s", foundEv.ActorKind, foundEv.ActorRef, foundEv.Authority)
			}
			if foundEv.Detail["formerOwnerLive"] != false || foundEv.Detail["keyCleared"] != false {
				t.Fatalf("event detail = %+v", foundEv.Detail)
			}

			entries, _ := capLog.take(t)
			entry := one(t, "allowed transfer", entries)
			checkEntry(t, "allowed transfer", entry, "INFO", map[string]any{
				logKeyAction:         logActionTransfer,
				logKeyOutcome:        logOutcomeAllowed,
				logKeyPrincipalKind:  ActorKindSystem,
				logKeyPrincipalID:    SuccessionTriggerAgentDelete,
				logKeyAuthority:      AuthorityAncestorSuccession,
				logKeyArtifactID:     a.ID,
				logKeyProjectID:      "proj-1",
				logKeySubjectKind:    PrincipalKindAgent,
				logKeySubjectRef:     parent,
				logKeyFormerOwnerRef: child,
				logKeyKeyCleared:     false,
			})
		})
	}
}

// storeFaultWrapper wraps a Store so tests can inject a single ErrKeyConflict,
// ErrStewardPrecondition, ErrOwnerChanged, ErrNotFound, or transient error on
// specific artifact IDs.
type storeFaultWrapper struct {
	Store
	onTransfer func(ctx context.Context, artifactID string, opts TransferOptions, actor *Actor, now time.Time, call int) (*Artifact, TransferResult, error)
	calls      map[string]int
}

func (w *storeFaultWrapper) TransferOwner(ctx context.Context, artifactID string, opts TransferOptions, actor *Actor, now time.Time) (*Artifact, TransferResult, error) {
	if w.calls == nil {
		w.calls = map[string]int{}
	}
	w.calls[artifactID]++
	if w.onTransfer != nil {
		return w.onTransfer(ctx, artifactID, opts, actor, now, w.calls[artifactID])
	}
	return w.Store.TransferOwner(ctx, artifactID, opts, actor, now)
}

// TestTransferToSuccessor_KeyCollisionAndRetry (S3 & S6): parent already owns
// key k -> child's key cleared into former_key and keyCleared=true; also tests
// the single retry on ErrKeyConflict from a concurrent same-key publish.
func TestTransferToSuccessor_KeyCollisionAndRetry(t *testing.T) {
	for _, b := range testBackends() {
		t.Run(b.name, func(t *testing.T) {
			db, _ := b.open(t)
			st := NewStore(db, b.driver)
			ctx := context.Background()
			if err := st.Init(ctx); err != nil {
				t.Fatal(err)
			}

			child := uuid.NewString()
			parent := uuid.NewString()
			now := time.Now().UTC().Truncate(time.Microsecond)

			// Parent already owns key "dup-key".
			parentArt := seedAgentArtifact(t, st, parent, "proj-1", "dup-key", "user-root")
			childArt := seedAgentArtifact(t, st, child, "proj-1", "dup-key", "user-root")
			// Another artifact that simulates a concurrent same-key publish on attempt 1.
			raceArt := seedAgentArtifact(t, st, child, "proj-1", "race-key", "user-root")

			wrapped := &storeFaultWrapper{
				Store: st,
				onTransfer: func(ctx context.Context, artifactID string, opts TransferOptions, actor *Actor, now time.Time, call int) (*Artifact, TransferResult, error) {
					if artifactID == raceArt.ID && call == 1 {
						// Simulate parent concurrently publishing "race-key" between clash count and UPDATE.
						_ = seedAgentArtifact(t, st, parent, "proj-1", "race-key", "user-root")
						return nil, TransferResult{}, ErrKeyConflict
					}
					return st.TransferOwner(ctx, artifactID, opts, actor, now)
				},
			}

			capLog := &logCapture{}
			logger := slog.New(slog.NewJSONHandler(capLog, &slog.HandlerOptions{Level: slog.LevelDebug}))

			res, err := TransferToSuccessor(ctx, wrapped, Succession{
				From:      Owner{Kind: PrincipalKindAgent, Ref: child},
				ProjectID: "proj-1",
				ToAgentID: parent,
				Sponsor:   "user-root",
				Trigger:   SuccessionTriggerAgentDelete,
			}, now, logger)
			if err != nil {
				t.Fatalf("TransferToSuccessor: %v", err)
			}
			if res != (SuccessionResult{Transferred: 2, KeyCleared: 2}) {
				t.Fatalf("res = %+v, want {Transferred:2, KeyCleared:2}", res)
			}
			if wrapped.calls[raceArt.ID] != 2 {
				t.Fatalf("raceArt calls = %d, want 2 (initial + 1 retry)", wrapped.calls[raceArt.ID])
			}

			// Parent's original artifact keeps its key.
			gotParent, err := st.GetArtifact(ctx, parentArt.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gotParent.Key != "dup-key" {
				t.Fatalf("parent key = %q, want dup-key", gotParent.Key)
			}

			// Both child artifacts moved to parent with keys cleared into former_key.
			for _, tc := range []struct {
				id, wantFormer string
			}{
				{childArt.ID, "dup-key"},
				{raceArt.ID, "race-key"},
			} {
				got, err := st.GetArtifact(ctx, tc.id)
				if err != nil {
					t.Fatal(err)
				}
				if got.OwnerRef != parent || got.Key != "" || got.FormerKey != tc.wantFormer {
					t.Fatalf("%s: owner=%s key=%q formerKey=%q", tc.id, got.OwnerRef, got.Key, got.FormerKey)
				}
			}

			entries, _ := capLog.take(t)
			if len(entries) != 2 {
				t.Fatalf("got %d log entries, want 2", len(entries))
			}
			for _, e := range entries {
				checkEntry(t, "key cleared", e, "INFO", map[string]any{
					logKeyAction:         logActionTransfer,
					logKeyOutcome:        logOutcomeAllowed,
					logKeyKeyCleared:     true,
					logKeyFormerOwnerRef: child,
				})
			}
		})
	}
}

// TestTransferToSuccessor_Skips (S4 & S6): moved_out_of_project, owner_changed,
// and not_found are skipped and logged while the loop continues past every skip.
func TestTransferToSuccessor_Skips(t *testing.T) {
	for _, b := range testBackends() {
		t.Run(b.name, func(t *testing.T) {
			db, _ := b.open(t)
			st := NewStore(db, b.driver)
			ctx := context.Background()
			if err := st.Init(ctx); err != nil {
				t.Fatal(err)
			}

			child := uuid.NewString()
			parent := uuid.NewString()
			now := time.Now().UTC().Truncate(time.Microsecond)

			aMoved := seedAgentArtifact(t, st, child, "proj-1", "", "user-root")
			aChanged := seedAgentArtifact(t, st, child, "proj-1", "", "user-root")
			aDeleted := seedAgentArtifact(t, st, child, "proj-1", "", "user-root")
			aOK := seedAgentArtifact(t, st, child, "proj-1", "", "user-root")

			wrapped := &storeFaultWrapper{
				Store: st,
				onTransfer: func(ctx context.Context, artifactID string, opts TransferOptions, actor *Actor, now time.Time, _ int) (*Artifact, TransferResult, error) {
					switch artifactID {
					case aMoved.ID:
						return nil, TransferResult{}, ErrStewardPrecondition
					case aChanged.ID:
						return nil, TransferResult{}, ErrOwnerChanged
					case aDeleted.ID:
						return nil, TransferResult{}, ErrNotFound
					default:
						return st.TransferOwner(ctx, artifactID, opts, actor, now)
					}
				},
			}

			capLog := &logCapture{}
			logger := slog.New(slog.NewJSONHandler(capLog, &slog.HandlerOptions{Level: slog.LevelDebug}))

			res, err := TransferToSuccessor(ctx, wrapped, Succession{
				From:      Owner{Kind: PrincipalKindAgent, Ref: child},
				ProjectID: "proj-1",
				ToAgentID: parent,
				Sponsor:   "user-root",
				Trigger:   SuccessionTriggerAgentDelete,
			}, now, logger)
			if err != nil {
				t.Fatalf("TransferToSuccessor: %v", err)
			}
			if res != (SuccessionResult{Transferred: 1, Skipped: 3}) {
				t.Fatalf("res = %+v, want {Transferred:1, Skipped:3}", res)
			}

			gotOK, err := st.GetArtifact(ctx, aOK.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gotOK.OwnerRef != parent {
				t.Fatalf("aOK owner = %s, want %s", gotOK.OwnerRef, parent)
			}

			entries, _ := capLog.take(t)
			if len(entries) != 4 {
				t.Fatalf("got %d log entries, want 4", len(entries))
			}
			byID := map[string]map[string]any{}
			for _, e := range entries {
				byID[fmt.Sprint(e[logKeyArtifactID])] = e
			}
			checkEntry(t, "moved", byID[aMoved.ID], "INFO", map[string]any{
				logKeyOutcome:        logOutcomeRefused,
				logKeyReason:         logReasonMovedOutOfProject,
				logKeyFormerOwnerRef: child,
			})
			checkEntry(t, "changed", byID[aChanged.ID], "INFO", map[string]any{
				logKeyOutcome:        logOutcomeRefused,
				logKeyReason:         logReasonOwnerChanged,
				logKeyFormerOwnerRef: child,
			})
			checkEntry(t, "deleted", byID[aDeleted.ID], "INFO", map[string]any{
				logKeyOutcome:        logOutcomeRefused,
				logKeyReason:         logReasonNotFound,
				logKeyFormerOwnerRef: child,
			})
			checkEntry(t, "ok", byID[aOK.ID], "INFO", map[string]any{
				logKeyOutcome:    logOutcomeAllowed,
				logKeyKeyCleared: false,
			})
		})
	}
}

// TestTransferToSuccessor_MidPageErrorIdempotent (S5): a store error mid-page
// returns an error while keeping earlier transfers committed, and a re-run
// transfers only the remaining artifacts.
func TestTransferToSuccessor_MidPageErrorIdempotent(t *testing.T) {
	for _, b := range testBackends() {
		t.Run(b.name, func(t *testing.T) {
			db, _ := b.open(t)
			st := NewStore(db, b.driver)
			ctx := context.Background()
			if err := st.Init(ctx); err != nil {
				t.Fatal(err)
			}

			child := uuid.NewString()
			parent := uuid.NewString()
			now := time.Now().UTC().Truncate(time.Microsecond)

			a1 := seedAgentArtifact(t, st, child, "proj-1", "", "user-root")
			a2 := seedAgentArtifact(t, st, child, "proj-1", "", "user-root")
			a3 := seedAgentArtifact(t, st, child, "proj-1", "", "user-root")
			ids := []string{a1.ID, a2.ID, a3.ID}
			slices.Sort(ids)

			errBoom := errors.New("transient db fault")
			failSecond := true
			wrapped := &storeFaultWrapper{
				Store: st,
				onTransfer: func(ctx context.Context, artifactID string, opts TransferOptions, actor *Actor, now time.Time, _ int) (*Artifact, TransferResult, error) {
					if failSecond && artifactID == ids[1] {
						return nil, TransferResult{}, errBoom
					}
					return st.TransferOwner(ctx, artifactID, opts, actor, now)
				},
			}

			succ := Succession{
				From:      Owner{Kind: PrincipalKindAgent, Ref: child},
				ProjectID: "proj-1",
				ToAgentID: parent,
				Sponsor:   "user-root",
				Trigger:   SuccessionTriggerAgentDelete,
			}

			res1, err := TransferToSuccessor(ctx, wrapped, succ, now, nil)
			if !errors.Is(err, errBoom) {
				t.Fatalf("first run err = %v, want errBoom", err)
			}
			if res1 != (SuccessionResult{Transferred: 1}) {
				t.Fatalf("first run res = %+v, want {Transferred:1}", res1)
			}

			// First artifact is committed; second and third still belong to child.
			got0, _ := st.GetArtifact(ctx, ids[0])
			got1, _ := st.GetArtifact(ctx, ids[1])
			if got0.OwnerRef != parent || got1.OwnerRef != child {
				t.Fatalf("mid-page state: got0=%s got1=%s", got0.OwnerRef, got1.OwnerRef)
			}

			// Re-run with fault cleared transfers only the remaining 2.
			failSecond = false
			res2, err := TransferToSuccessor(ctx, wrapped, succ, now.Add(time.Second), nil)
			if err != nil {
				t.Fatalf("second run err = %v", err)
			}
			if res2 != (SuccessionResult{Transferred: 2}) {
				t.Fatalf("second run res = %+v, want {Transferred:2}", res2)
			}
			for _, id := range ids {
				got, err := st.GetArtifact(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if got.OwnerRef != parent {
					t.Fatalf("artifact %s owner = %s, want %s", id, got.OwnerRef, parent)
				}
			}
		})
	}
}

// fakeSuccessorChecker extends fakeChecker with OwnerSuccessorResolver for R1/R2/C1.
type fakeSuccessorChecker struct {
	*fakeChecker
	mu        sync.Mutex
	succ      map[string]Succession
	errFor    map[string]error
	consulted []string
}

func (c *fakeSuccessorChecker) ResolveOwnerSuccessor(_ context.Context, agentID string) (Succession, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consulted = append(c.consulted, agentID)
	if err := c.errFor[agentID]; err != nil {
		return Succession{}, false, err
	}
	s, ok := c.succ[agentID]
	return s, ok, nil
}

// TestReconcileOwners_SuccessionBackstop (R1): found -> transferred & none marked;
// not found -> marked orphaned; resolver error -> unmarked & SuccessionErrors++,
// retried next pass; user owners never consult resolver.
func TestReconcileOwners_SuccessionBackstop(t *testing.T) {
	for _, b := range testBackends() {
		t.Run(b.name, func(t *testing.T) {
			db, _ := b.open(t)
			st := NewStore(db, b.driver)
			ctx := context.Background()
			if err := st.Init(ctx); err != nil {
				t.Fatal(err)
			}

			now := time.Now().UTC().Truncate(time.Microsecond)
			childFound := uuid.NewString()
			parent := uuid.NewString()
			childNone := uuid.NewString()
			childErr := uuid.NewString()
			userGone := uuid.NewString()

			aFound := seedAgentArtifact(t, st, childFound, "proj-1", "", "user-root")
			aNone := seedAgentArtifact(t, st, childNone, "proj-1", "", "user-root")
			aErr := seedAgentArtifact(t, st, childErr, "proj-1", "", "user-root")
			aUser := seedOwned(t, st, PrincipalKindUser, userGone)

			fc := newFakeChecker()
			fc.gone[PrincipalRef(PrincipalKindAgent, childFound)] = true
			fc.gone[PrincipalRef(PrincipalKindAgent, childNone)] = true
			fc.gone[PrincipalRef(PrincipalKindAgent, childErr)] = true
			fc.gone[PrincipalRef(PrincipalKindUser, userGone)] = true
			fc.live[PrincipalRef(PrincipalKindAgent, parent)] = true

			errTransient := errors.New("hub db unavailable")
			checker := &fakeSuccessorChecker{
				fakeChecker: fc,
				succ: map[string]Succession{
					childFound: {
						From:      Owner{Kind: PrincipalKindAgent, Ref: childFound},
						ProjectID: "proj-1",
						ToAgentID: parent,
						Sponsor:   "user-root",
					},
					childErr: {
						From:      Owner{Kind: PrincipalKindAgent, Ref: childErr},
						ProjectID: "proj-1",
						ToAgentID: parent,
						Sponsor:   "user-root",
					},
				},
				errFor: map[string]error{
					childErr: errTransient,
				},
			}

			capLog := &logCapture{}
			logger := slog.New(slog.NewJSONHandler(capLog, &slog.HandlerOptions{Level: slog.LevelDebug}))

			res1, err := ReconcileOwners(ctx, st, checker, now, logger)
			if err != nil {
				t.Fatalf("pass 1 err: %v", err)
			}
			if res1.Transferred != 1 || res1.Orphaned != 2 || res1.SuccessionErrors != 1 {
				t.Fatalf("pass 1 res = %+v, want Transferred=1 Orphaned=2 SuccessionErrors=1", res1)
			}

			// User owner never consulted resolver.
			if slices.Contains(checker.consulted, userGone) {
				t.Fatalf("resolver was consulted for user owner %s", userGone)
			}

			// aFound moved to parent with trigger=reconciler and not orphaned.
			gotFound, _ := st.GetArtifact(ctx, aFound.ID)
			if gotFound.OwnerRef != parent || gotFound.OrphanedAt != nil {
				t.Fatalf("aFound = owner:%s orphaned:%v", gotFound.OwnerRef, gotFound.OrphanedAt)
			}
			evs, _ := st.ListEvents(ctx, aFound.ID)
			var sawReconcilerEvent bool
			for _, ev := range evs {
				if ev.Action == EventOwnerTransferred && ev.ActorRef == SuccessionTriggerReconciler && ev.Authority == AuthorityAncestorSuccession {
					sawReconcilerEvent = true
				}
			}
			if !sawReconcilerEvent {
				t.Fatalf("expected reconciler owner_transferred event, got %+v", evs)
			}

			// aNone and aUser marked orphaned; aErr left unmarked for retry.
			if gotNone, _ := st.GetArtifact(ctx, aNone.ID); gotNone.OrphanedAt == nil {
				t.Fatalf("aNone should be orphaned")
			}
			if gotUser, _ := st.GetArtifact(ctx, aUser.ID); gotUser.OrphanedAt == nil {
				t.Fatalf("aUser should be orphaned")
			}
			if gotErr, _ := st.GetArtifact(ctx, aErr.ID); gotErr.OrphanedAt != nil || gotErr.OwnerRef != childErr {
				t.Fatalf("aErr should remain unmarked on childErr: owner=%s orphaned=%v", gotErr.OwnerRef, gotErr.OrphanedAt)
			}

			// Verify owner_pass summary carries transferred_count.
			entries, _ := capLog.take(t)
			var passEntry map[string]any
			for _, e := range entries {
				if e[logKeyAction] == logActionOwnerPass {
					passEntry = e
				}
			}
			if passEntry == nil {
				t.Fatalf("missing owner_pass log entry")
			}
			checkEntry(t, "owner_pass", passEntry, "INFO", map[string]any{
				logKeyTransferredCount: 1.0,
				logKeyOrphanedCount:    2.0,
			})

			// Second pass with transient error cleared transfers aErr.
			delete(checker.errFor, childErr)
			res2, err := ReconcileOwners(ctx, st, checker, now.Add(time.Minute), nil)
			if err != nil {
				t.Fatalf("pass 2 err: %v", err)
			}
			if res2.Transferred != 1 || res2.Orphaned != 0 || res2.SuccessionErrors != 0 {
				t.Fatalf("pass 2 res = %+v, want Transferred=1", res2)
			}
			if gotErr, _ := st.GetArtifact(ctx, aErr.ID); gotErr.OwnerRef != parent || gotErr.OrphanedAt != nil {
				t.Fatalf("aErr after retry = owner:%s orphaned:%v", gotErr.OwnerRef, gotErr.OrphanedAt)
			}
		})
	}
}

// TestReconcileOwners_SuccessionRunsOnce (R2): after a "not found" pass marks
// artifacts orphaned, a later pass where the resolver now reports a successor
// makes no transfer because Unmarked == 0.
func TestReconcileOwners_SuccessionRunsOnce(t *testing.T) {
	for _, b := range testBackends() {
		t.Run(b.name, func(t *testing.T) {
			db, _ := b.open(t)
			st := NewStore(db, b.driver)
			ctx := context.Background()
			if err := st.Init(ctx); err != nil {
				t.Fatal(err)
			}

			now := time.Now().UTC().Truncate(time.Microsecond)
			child := uuid.NewString()
			parent := uuid.NewString()
			a := seedAgentArtifact(t, st, child, "proj-1", "", "user-root")

			fc := newFakeChecker()
			fc.gone[PrincipalRef(PrincipalKindAgent, child)] = true
			checker := &fakeSuccessorChecker{fakeChecker: fc, succ: map[string]Succession{}}

			// Pass 1: no successor -> marked orphaned.
			res1, err := ReconcileOwners(ctx, st, checker, now, nil)
			if err != nil || res1.Orphaned != 1 || res1.Transferred != 0 {
				t.Fatalf("pass 1 = %+v, %v", res1, err)
			}

			// Pass 2: resolver now returns parent, but Unmarked == 0 -> no transfer.
			checker.succ[child] = Succession{
				From:      Owner{Kind: PrincipalKindAgent, Ref: child},
				ProjectID: "proj-1",
				ToAgentID: parent,
				Sponsor:   "user-root",
			}
			res2, err := ReconcileOwners(ctx, st, checker, now.Add(time.Minute), nil)
			if err != nil || res2.Transferred != 0 || res2.Orphaned != 0 {
				t.Fatalf("pass 2 = %+v, %v", res2, err)
			}
			got, _ := st.GetArtifact(ctx, a.ID)
			if got.OwnerRef != child || got.OrphanedAt == nil {
				t.Fatalf("artifact should stay orphaned on child: owner=%s orphaned=%v", got.OwnerRef, got.OrphanedAt)
			}
		})
	}
}

// TestTransferToSuccessor_Concurrency (C1, C2, C3, C4): exercises concurrent
// races under -race:
//   - C1: tail step vs reconciler backstop
//   - C2: two concurrent TransferToSuccessor calls (multi-replica)
//   - C3: succession vs steward manual TransferOwner to a different target
//   - C4: succession vs SetOwnerOrphaned
func TestTransferToSuccessor_Concurrency(t *testing.T) {
	for _, b := range testBackends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Microsecond)

			t.Run("C1_tail_vs_reconciler", func(t *testing.T) {
				db, _ := b.open(t)
				st := NewStore(db, b.driver)
				if err := st.Init(ctx); err != nil {
					t.Fatal(err)
				}
				child, parent := uuid.NewString(), uuid.NewString()
				var ids []string
				for range 6 {
					ids = append(ids, seedAgentArtifact(t, st, child, "proj-1", "", "user-root").ID)
				}
				fc := newFakeChecker()
				fc.gone[PrincipalRef(PrincipalKindAgent, child)] = true
				fc.live[PrincipalRef(PrincipalKindAgent, parent)] = true
				checker := &fakeSuccessorChecker{
					fakeChecker: fc,
					succ: map[string]Succession{
						child: {
							From:      Owner{Kind: PrincipalKindAgent, Ref: child},
							ProjectID: "proj-1",
							ToAgentID: parent,
							Sponsor:   "user-root",
						},
					},
				}

				var wg sync.WaitGroup
				var tailRes SuccessionResult
				var recRes ReconcileResult
				var tailErr, recErr error
				wg.Add(2)
				go func() {
					defer wg.Done()
					tailRes, tailErr = TransferToSuccessor(ctx, st, Succession{
						From:      Owner{Kind: PrincipalKindAgent, Ref: child},
						ProjectID: "proj-1",
						ToAgentID: parent,
						Sponsor:   "user-root",
						Trigger:   SuccessionTriggerAgentDelete,
					}, now, nil)
				}()
				go func() {
					defer wg.Done()
					recRes, recErr = ReconcileOwners(ctx, st, checker, now, nil)
				}()
				wg.Wait()
				if tailErr != nil || recErr != nil {
					t.Fatalf("errors: tail=%v rec=%v", tailErr, recErr)
				}
				if tailRes.Transferred+recRes.Transferred != len(ids) {
					t.Fatalf("total transferred = %d + %d, want %d", tailRes.Transferred, recRes.Transferred, len(ids))
				}
				for _, id := range ids {
					got, _ := st.GetArtifact(ctx, id)
					if got.OwnerRef != parent || got.OrphanedAt != nil {
						t.Fatalf("artifact %s: owner=%s orphaned=%v", id, got.OwnerRef, got.OrphanedAt)
					}
					evs, _ := st.ListEvents(ctx, id)
					count := 0
					for _, ev := range evs {
						if ev.Action == EventOwnerTransferred {
							count++
						}
					}
					if count != 1 {
						t.Fatalf("artifact %s: %d owner_transferred events, want 1", id, count)
					}
				}
			})

			t.Run("C2_two_replicas", func(t *testing.T) {
				db, _ := b.open(t)
				st := NewStore(db, b.driver)
				if err := st.Init(ctx); err != nil {
					t.Fatal(err)
				}
				child, parent := uuid.NewString(), uuid.NewString()
				var ids []string
				for range 6 {
					ids = append(ids, seedAgentArtifact(t, st, child, "proj-1", "", "user-root").ID)
				}
				succ := Succession{
					From:      Owner{Kind: PrincipalKindAgent, Ref: child},
					ProjectID: "proj-1",
					ToAgentID: parent,
					Sponsor:   "user-root",
					Trigger:   SuccessionTriggerAgentDelete,
				}
				var wg sync.WaitGroup
				var r1, r2 SuccessionResult
				var e1, e2 error
				wg.Add(2)
				go func() { defer wg.Done(); r1, e1 = TransferToSuccessor(ctx, st, succ, now, nil) }()
				go func() { defer wg.Done(); r2, e2 = TransferToSuccessor(ctx, st, succ, now, nil) }()
				wg.Wait()
				if e1 != nil || e2 != nil {
					t.Fatalf("errors: e1=%v e2=%v", e1, e2)
				}
				if r1.Transferred+r2.Transferred != len(ids) {
					t.Fatalf("total transferred = %d + %d, want %d", r1.Transferred, r2.Transferred, len(ids))
				}
				for _, id := range ids {
					evs, _ := st.ListEvents(ctx, id)
					count := 0
					for _, ev := range evs {
						if ev.Action == EventOwnerTransferred {
							count++
						}
					}
					if count != 1 {
						t.Fatalf("artifact %s: %d owner_transferred events, want 1", id, count)
					}
				}
			})

			t.Run("C3_succession_vs_manual_transfer", func(t *testing.T) {
				db, _ := b.open(t)
				st := NewStore(db, b.driver)
				if err := st.Init(ctx); err != nil {
					t.Fatal(err)
				}
				child, parent, other := uuid.NewString(), uuid.NewString(), uuid.NewString()
				a := seedAgentArtifact(t, st, child, "proj-1", "", "user-root")

				var wg sync.WaitGroup
				var succRes SuccessionResult
				var succErr, manualErr error
				wg.Add(2)
				go func() {
					defer wg.Done()
					succRes, succErr = TransferToSuccessor(ctx, st, Succession{
						From:      Owner{Kind: PrincipalKindAgent, Ref: child},
						ProjectID: "proj-1",
						ToAgentID: parent,
						Sponsor:   "user-root",
						Trigger:   SuccessionTriggerAgentDelete,
					}, now, nil)
				}()
				go func() {
					defer wg.Done()
					_, _, manualErr = st.TransferOwner(ctx, a.ID, TransferOptions{
						ExpectOwner: Owner{Kind: PrincipalKindAgent, Ref: child},
						To:          Owner{Kind: PrincipalKindAgent, Ref: other},
						Sponsor:     "user-root",
					}, &Actor{
						Kind:      PrincipalKindUser,
						Ref:       "user-root",
						Authority: AuthorityStewardDelegator,
						Expect:    Expect{ScopeRef: "proj-1", OwnerKind: PrincipalKindAgent, OwnerRef: child},
					}, now)
				}()
				wg.Wait()
				if succErr != nil {
					t.Fatalf("succErr: %v", succErr)
				}
				got, _ := st.GetArtifact(ctx, a.ID)
				if manualErr == nil {
					if succRes.Transferred != 0 || got.OwnerRef != other {
						t.Fatalf("manual won, but succRes=%+v owner=%s", succRes, got.OwnerRef)
					}
				} else {
					if !errors.Is(manualErr, ErrOwnerChanged) {
						t.Fatalf("manualErr = %v, want ErrOwnerChanged", manualErr)
					}
					if succRes.Transferred != 1 || got.OwnerRef != parent {
						t.Fatalf("succession won, but succRes=%+v owner=%s", succRes, got.OwnerRef)
					}
				}
			})

			t.Run("C4_succession_vs_set_owner_orphaned", func(t *testing.T) {
				db, _ := b.open(t)
				st := NewStore(db, b.driver)
				if err := st.Init(ctx); err != nil {
					t.Fatal(err)
				}
				child, parent := uuid.NewString(), uuid.NewString()
				a := seedAgentArtifact(t, st, child, "proj-1", "", "user-root")

				var wg sync.WaitGroup
				var succErr, orphErr error
				wg.Add(2)
				go func() {
					defer wg.Done()
					_, succErr = TransferToSuccessor(ctx, st, Succession{
						From:      Owner{Kind: PrincipalKindAgent, Ref: child},
						ProjectID: "proj-1",
						ToAgentID: parent,
						Sponsor:   "user-root",
						Trigger:   SuccessionTriggerAgentDelete,
					}, now, nil)
				}()
				go func() {
					defer wg.Done()
					_, orphErr = st.SetOwnerOrphaned(ctx, Owner{Kind: PrincipalKindAgent, Ref: child}, true, now, 10)
				}()
				wg.Wait()
				if succErr != nil || orphErr != nil {
					t.Fatalf("errors: succ=%v orph=%v", succErr, orphErr)
				}
				got, _ := st.GetArtifact(ctx, a.ID)
				if got.OwnerRef != parent || got.OrphanedAt != nil {
					t.Fatalf("final state: owner=%s orphaned=%v, want owner=%s orphaned=nil", got.OwnerRef, got.OrphanedAt, parent)
				}
			})
		})
	}
}
