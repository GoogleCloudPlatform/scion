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
