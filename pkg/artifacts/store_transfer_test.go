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
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func seedAgentArtifact(t *testing.T, st Store, agentID, scope, key, sponsor string) *Artifact {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	a := &Artifact{
		ID:         uuid.NewString(),
		ScopeKind:  ScopeKindProject,
		ScopeRef:   scope,
		OwnerKind:  PrincipalKindAgent,
		OwnerRef:   agentID,
		Key:        key,
		Title:      "Agent Work",
		CreatedAt:  now,
		UpdatedAt:  now,
		SponsorRef: sponsor,
	}
	v := &Version{
		ID:            uuid.NewString(),
		ArtifactID:    a.ID,
		Seq:           1,
		Kind:          VersionKindPublish,
		EntryPath:     "doc.md",
		TotalBytes:    5,
		FileCount:     1,
		CreatedByKind: PrincipalKindAgent,
		CreatedByRef:  agentID,
		CreatedAt:     now,
		State:         VersionStateReady,
	}
	f := File{VersionID: v.ID, Path: "doc.md", Size: 5, SHA256: sha([]byte("hello")), MediaType: "text/markdown"}
	g := Grant{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectScope, SubjectRef: scope, Permission: GrantRead, CreatedAt: now}
	if err := st.CreatePublished(context.Background(), a, v, []File{f}, []Grant{g}); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestStoreTransferOwner(t *testing.T) {
	for _, b := range testBackends() {
		t.Run(b.name, func(t *testing.T) {
			db, _ := b.open(t)
			st := NewStore(db, b.driver)
			ctx := context.Background()
			if err := st.Init(ctx); err != nil {
				t.Fatal(err)
			}

			oldAgent := uuid.NewString()
			newAgent := uuid.NewString()
			a := seedAgentArtifact(t, st, oldAgent, "project-1", "report-key", "user-old-sponsor")

			// Mark orphaned first so we verify transfer clears orphaned_at.
			orphanAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
			if _, err := st.SetOwnerOrphaned(ctx, Owner{Kind: PrincipalKindAgent, Ref: oldAgent}, true, orphanAt, 10); err != nil {
				t.Fatal(err)
			}
			// Give newAgent a redundant principal grant and user-other a surviving grant.
			redundantGrant := &Grant{
				ID: uuid.NewString(), ArtifactID: a.ID,
				SubjectKind: SubjectPrincipal, SubjectRef: PrincipalRef(PrincipalKindAgent, newAgent),
				Permission: GrantWrite, CreatedAt: orphanAt,
			}
			otherGrant := &Grant{
				ID: uuid.NewString(), ArtifactID: a.ID,
				SubjectKind: SubjectPrincipal, SubjectRef: "user:user-reviewer",
				Permission: GrantReview, CreatedAt: orphanAt,
			}
			for _, g := range []*Grant{redundantGrant, otherGrant} {
				if _, err := st.PutGrant(ctx, g, MaxGrantsPerArtifact, true, testActor); err != nil {
					t.Fatal(err)
				}
			}

			stewardActor := &Actor{
				Kind:      PrincipalKindUser,
				Ref:       "user-steward",
				Authority: AuthorityStewardProjectOwner,
				Expect:    Expect{ScopeRef: "project-1", OwnerKind: PrincipalKindAgent, OwnerRef: oldAgent},
			}

			// Steward actor without Expect is refused.
			noExpectActor := &Actor{Kind: PrincipalKindUser, Ref: "user-steward", Authority: AuthorityStewardProjectOwner}
			if _, _, err := st.TransferOwner(ctx, a.ID, TransferOptions{
				ExpectOwner: Owner{Kind: PrincipalKindAgent, Ref: oldAgent},
				To:          Owner{Kind: PrincipalKindAgent, Ref: newAgent},
				Sponsor:     "user-new-sponsor",
			}, noExpectActor, time.Now()); !errors.Is(err, errNoExpect) {
				t.Fatalf("steward without Expect: got %v, want errNoExpect", err)
			}

			// ExpectOwner mismatch -> ErrOwnerChanged.
			if _, _, err := st.TransferOwner(ctx, a.ID, TransferOptions{
				ExpectOwner: Owner{Kind: PrincipalKindAgent, Ref: uuid.NewString()},
				To:          Owner{Kind: PrincipalKindAgent, Ref: newAgent},
				Sponsor:     "user-new-sponsor",
			}, stewardActor, time.Now()); !errors.Is(err, ErrOwnerChanged) {
				t.Fatalf("ExpectOwner mismatch: got %v, want ErrOwnerChanged", err)
			}

			// Home mismatch -> ErrStewardPrecondition.
			wrongScopeActor := *stewardActor
			wrongScopeActor.Expect.ScopeRef = "project-other"
			if _, _, err := st.TransferOwner(ctx, a.ID, TransferOptions{
				ExpectOwner: Owner{Kind: PrincipalKindAgent, Ref: oldAgent},
				To:          Owner{Kind: PrincipalKindAgent, Ref: newAgent},
				Sponsor:     "user-new-sponsor",
			}, &wrongScopeActor, time.Now()); !errors.Is(err, ErrStewardPrecondition) {
				t.Fatalf("home mismatch: got %v, want ErrStewardPrecondition", err)
			}

			// Successful transfer with SponsorReviewGrant=true.
			now := time.Now().UTC().Truncate(time.Microsecond)
			updated, res, err := st.TransferOwner(ctx, a.ID, TransferOptions{
				ExpectOwner:        Owner{Kind: PrincipalKindAgent, Ref: oldAgent},
				To:                 Owner{Kind: PrincipalKindAgent, Ref: newAgent},
				Sponsor:            "user-new-sponsor",
				SponsorReviewGrant: true,
				FormerOwnerLive:    false,
			}, stewardActor, now)
			if err != nil {
				t.Fatalf("TransferOwner: %v", err)
			}
			if updated.OwnerKind != PrincipalKindAgent || updated.OwnerRef != newAgent {
				t.Errorf("owner = %s:%s, want agent:%s", updated.OwnerKind, updated.OwnerRef, newAgent)
			}
			if updated.FormerOwnerKind != PrincipalKindAgent || updated.FormerOwnerRef != oldAgent {
				t.Errorf("former owner = %s:%s, want agent:%s", updated.FormerOwnerKind, updated.FormerOwnerRef, oldAgent)
			}
			if updated.TransferredAt == nil || !updated.TransferredAt.Equal(now) {
				t.Errorf("transferredAt = %v, want %v", updated.TransferredAt, now)
			}
			if updated.OrphanedAt != nil {
				t.Errorf("orphanedAt = %v, want nil", updated.OrphanedAt)
			}
			if updated.SponsorRef != "user-new-sponsor" {
				t.Errorf("sponsorRef = %q, want user-new-sponsor", updated.SponsorRef)
			}
			if updated.Key != "report-key" || res.KeyCleared {
				t.Errorf("key = %q, keyCleared = %v; want report-key, false", updated.Key, res.KeyCleared)
			}
			if res.RemovedGrantID != redundantGrant.ID {
				t.Errorf("removedGrantID = %q, want %q", res.RemovedGrantID, redundantGrant.ID)
			}
			if res.SponsorReviewGrantID == "" {
				t.Errorf("expected automatic sponsor review grant for new sponsor")
			}

			// Version authorship is untouched (I3).
			v1, err := st.GetVersion(ctx, a.ID, 1)
			if err != nil {
				t.Fatal(err)
			}
			if v1.CreatedByKind != PrincipalKindAgent || v1.CreatedByRef != oldAgent {
				t.Errorf("v1 creator = %s:%s, want agent:%s", v1.CreatedByKind, v1.CreatedByRef, oldAgent)
			}

			// Grants: redundant grant removed, other grant kept, sponsor review grant added.
			grants, err := st.ListGrants(ctx, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			var sawOther, sawSponsor, sawRedundant bool
			for _, g := range grants {
				if g.ID == redundantGrant.ID {
					sawRedundant = true
				}
				if g.ID == otherGrant.ID {
					sawOther = true
				}
				if g.SubjectRef == "user:user-new-sponsor" && g.CreatedByRef == SponsorReviewCreator && g.Permission == GrantReview {
					sawSponsor = true
				}
			}
			if sawRedundant || !sawOther || !sawSponsor {
				t.Errorf("grants mismatch: redundant=%v other=%v sponsor=%v", sawRedundant, sawOther, sawSponsor)
			}

			// Events: owner_transferred, grant_removed, grant_created (sponsor_review).
			ev := onlyEvent(t, st, a.ID, EventOwnerTransferred, stewardActor)
			if ev.Detail["from"] != PrincipalRef(PrincipalKindAgent, oldAgent) || ev.Detail["to"] != PrincipalRef(PrincipalKindAgent, newAgent) {
				t.Errorf("event detail = %+v", ev.Detail)
			}
			if ev.Detail["formerSponsor"] != "user-old-sponsor" || ev.Detail["sponsor"] != "user-new-sponsor" {
				t.Errorf("event sponsors = %+v", ev.Detail)
			}
			onlyEvent(t, st, a.ID, EventGrantRemoved, stewardActor)
			var sawSponsorEvent bool
			for _, e := range eventsOf(t, st, a.ID) {
				if e.Action == EventGrantCreated && e.Authority == AuthoritySponsorReview && e.ActorKind == ActorKindSystem && e.ActorRef == AuthoritySponsorReview {
					sawSponsorEvent = true
				}
			}
			if !sawSponsorEvent {
				t.Errorf("missing sponsor_review grant_created event")
			}
		})
	}
}

func TestStoreTransferOwner_KeyCollisionAndOrphanExpiry(t *testing.T) {
	for _, b := range testBackends() {
		t.Run(b.name, func(t *testing.T) {
			db, _ := b.open(t)
			st := NewStore(db, b.driver)
			ctx := context.Background()
			if err := st.Init(ctx); err != nil {
				t.Fatal(err)
			}

			oldAgent := uuid.NewString()
			newAgent := uuid.NewString()
			a := seedAgentArtifact(t, st, oldAgent, "project-1", "dup-key", "user-s1")
			_ = seedAgentArtifact(t, st, newAgent, "project-1", "dup-key", "user-s2")

			// Set orphan_policy expiry directly to verify transfer clears it.
			exp := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Microsecond)
			if _, err := db.ExecContext(ctx, st.(*sqlStore).rebind(`UPDATE artifact SET expires_at = ?, expiry_source = ? WHERE id = ?`),
				st.(*sqlStore).timeArg(exp), expirySourceOrphanPolicy, a.ID); err != nil {
				t.Fatal(err)
			}

			actor := &Actor{
				Kind: PrincipalKindUser, Ref: "user-s1", Authority: AuthorityStewardDelegator,
				Expect: Expect{ScopeRef: "project-1", OwnerKind: PrincipalKindAgent, OwnerRef: oldAgent},
			}
			// Without ClearKey -> ErrKeyConflict.
			if _, _, err := st.TransferOwner(ctx, a.ID, TransferOptions{
				ExpectOwner: Owner{Kind: PrincipalKindAgent, Ref: oldAgent},
				To:          Owner{Kind: PrincipalKindAgent, Ref: newAgent},
				Sponsor:     "user-s2",
				ClearKey:    false,
			}, actor, time.Now()); !errors.Is(err, ErrKeyConflict) {
				t.Fatalf("key collision without ClearKey: got %v, want ErrKeyConflict", err)
			}

			// With ClearKey -> succeeds, key cleared, former_key set, orphan_policy expiry cleared.
			updated, res, err := st.TransferOwner(ctx, a.ID, TransferOptions{
				ExpectOwner: Owner{Kind: PrincipalKindAgent, Ref: oldAgent},
				To:          Owner{Kind: PrincipalKindAgent, Ref: newAgent},
				Sponsor:     "user-s2",
				ClearKey:    true,
			}, actor, time.Now())
			if err != nil {
				t.Fatalf("TransferOwner with ClearKey: %v", err)
			}
			if !res.KeyCleared || updated.Key != "" || updated.FormerKey != "dup-key" {
				t.Errorf("key=%q formerKey=%q cleared=%v", updated.Key, updated.FormerKey, res.KeyCleared)
			}
			if updated.ExpiresAt != nil {
				t.Errorf("orphan_policy expiry not cleared: %v", updated.ExpiresAt)
			}

			// Manual expiry is preserved on transfer.
			thirdAgent := uuid.NewString()
			manualExp := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Microsecond)
			if _, err := st.SetExpiry(ctx, a.ID, &manualExp, testActor); err != nil {
				t.Fatal(err)
			}
			actor2 := &Actor{
				Kind: PrincipalKindUser, Ref: "user-s2", Authority: AuthorityStewardDelegator,
				Expect: Expect{ScopeRef: "project-1", OwnerKind: PrincipalKindAgent, OwnerRef: newAgent},
			}
			updated2, _, err := st.TransferOwner(ctx, a.ID, TransferOptions{
				ExpectOwner: Owner{Kind: PrincipalKindAgent, Ref: newAgent},
				To:          Owner{Kind: PrincipalKindAgent, Ref: thirdAgent},
				Sponsor:     "user-s3",
			}, actor2, time.Now())
			if err != nil {
				t.Fatalf("second transfer: %v", err)
			}
			if updated2.ExpiresAt == nil || !updated2.ExpiresAt.Equal(manualExp) {
				t.Errorf("manual expiry = %v, want %v", updated2.ExpiresAt, manualExp)
			}
		})
	}
}

func TestStoreTransferOwner_ConcurrentAndReconcilerRace(t *testing.T) {
	for _, b := range testBackends() {
		t.Run(b.name, func(t *testing.T) {
			db, _ := b.open(t)
			st := NewStore(db, b.driver)
			ctx := context.Background()
			if err := st.Init(ctx); err != nil {
				t.Fatal(err)
			}

			oldAgent := uuid.NewString()
			target1 := uuid.NewString()
			target2 := uuid.NewString()
			a := seedAgentArtifact(t, st, oldAgent, "project-1", "", "user-s1")

			// Concurrent transfers: exactly one wins, the other gets ErrOwnerChanged.
			var wg sync.WaitGroup
			errs := make([]error, 2)
			targets := []string{target1, target2}
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func(idx int) {
					defer wg.Done()
					actor := &Actor{
						Kind: PrincipalKindUser, Ref: "user-steward", Authority: AuthorityStewardProjectOwner,
						Expect: Expect{ScopeRef: "project-1", OwnerKind: PrincipalKindAgent, OwnerRef: oldAgent},
					}
					_, _, errs[idx] = st.TransferOwner(ctx, a.ID, TransferOptions{
						ExpectOwner: Owner{Kind: PrincipalKindAgent, Ref: oldAgent},
						To:          Owner{Kind: PrincipalKindAgent, Ref: targets[idx]},
						Sponsor:     "user-s2",
					}, actor, time.Now())
				}(i)
			}
			wg.Wait()
			wins, ownerChanged := 0, 0
			for _, err := range errs {
				if err == nil {
					wins++
				} else if errors.Is(err, ErrOwnerChanged) {
					ownerChanged++
				} else {
					t.Errorf("unexpected concurrent error: %v", err)
				}
			}
			if wins != 1 || ownerChanged != 1 {
				t.Fatalf("concurrent transfers: wins=%d ownerChanged=%d errs=%v", wins, ownerChanged, errs)
			}

			// Reconciler race (S11): simulate an ownership transfer landing in the
			// gap between setOwnerOrphaned's SELECT by old owner and its per-row
			// UPDATE. The UPDATE's AND owner_kind = ? AND owner_ref = ? guard must
			// prevent re-marking the transferred row orphaned or writing a false event.
			raceOld := uuid.NewString()
			raceNew := uuid.NewString()
			raceArt := seedAgentArtifact(t, st, raceOld, "project-1", "", "user-s1")
			sqlSt := st.(*sqlStore)
			sqlSt.beforeOrphanUpdate = func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, sqlSt.rebind(`UPDATE artifact SET owner_ref = ? WHERE id = ?`), raceNew, raceArt.ID)
				return err
			}
			changed, err := st.SetOwnerOrphaned(ctx, Owner{Kind: PrincipalKindAgent, Ref: raceOld}, true, time.Now(), 10)
			sqlSt.beforeOrphanUpdate = nil
			if err != nil {
				t.Fatalf("SetOwnerOrphaned: %v", err)
			}
			if len(changed) != 0 {
				t.Errorf("SetOwnerOrphaned(raceOld, true) changed %v after in-gap transfer; want empty", changed)
			}
			got, err := st.GetArtifact(ctx, raceArt.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.OrphanedAt != nil {
				t.Errorf("artifact was falsely marked orphaned after in-gap transfer")
			}
			if got.OwnerRef != raceNew {
				t.Errorf("owner = %q, want %q", got.OwnerRef, raceNew)
			}
			for _, ev := range eventsOf(t, st, raceArt.ID) {
				if ev.Action == EventOrphaned {
					t.Errorf("unexpected orphaned event written after in-gap transfer: %+v", ev)
				}
			}

			// Restore invariant (I5): restoring raceOld does not return ownership.
			if _, err := st.SetOwnerOrphaned(ctx, Owner{Kind: PrincipalKindAgent, Ref: raceOld}, false, time.Now(), 10); err != nil {
				t.Fatal(err)
			}
			got, err = st.GetArtifact(ctx, raceArt.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.OwnerRef != raceNew {
				t.Errorf("owner after restore = %q, want %q", got.OwnerRef, raceNew)
			}
		})
	}
}
