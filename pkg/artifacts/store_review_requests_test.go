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
)

func TestReviewRequestMigrationIdempotentAndPartialUniqueIndex(t *testing.T) {
	forEachDraftBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		// Re-running Init must be a no-op.
		if err := st.Init(ctx); err != nil {
			t.Fatalf("second Init: %v", err)
		}

		a, _, _, _ := seedArtifact(t, st, "")
		now := time.Now().UTC().Truncate(time.Microsecond)
		actor := &Actor{Kind: PrincipalKindUser, Ref: "owner-1", Authority: AuthorityOwner}

		first, outcome, err := st.CreateOrRemindReviewRequest(ctx, ReviewRequest{
			ArtifactID:    a.ID,
			ReviewerKind:  PrincipalKindUser,
			ReviewerRef:   "rev-1",
			RequesterKind: PrincipalKindUser,
			RequesterRef:  "owner-1",
			Note:          "please review",
			Access:        ReviewRequestAccessExisting,
		}, 24*time.Hour, now, actor, nil)
		if err != nil {
			t.Fatalf("CreateOrRemindReviewRequest: %v", err)
		}
		if outcome != ReviewRequestOutcomeCreated {
			t.Fatalf("outcome = %q, want created", outcome)
		}
		if first.RequestedSeq != a.CurrentSeq {
			t.Errorf("requested_seq = %d, want %d", first.RequestedSeq, a.CurrentSeq)
		}
		if first.State != ReviewRequestStateOpen {
			t.Errorf("state = %q, want open", first.State)
		}

		// Direct SQL insert of a second open row for the same (artifact, reviewer) must fail
		// because of idx_artifact_review_request_open.
		ss := st.(*sqlStore)
		_, err = db.ExecContext(ctx, ss.rebind(`INSERT INTO artifact_review_request
			(id, artifact_id, reviewer_kind, reviewer_ref, requester_kind, requester_ref,
			 requested_seq, note, access, state, response_note, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?, '', ?, ?)`),
			"dup-open-id", a.ID, PrincipalKindUser, "rev-1", PrincipalKindUser, "owner-1",
			a.CurrentSeq, ReviewRequestAccessExisting, ReviewRequestStateOpen,
			ss.timeArg(now), ss.timeArg(now))
		if err == nil {
			t.Fatal("expected partial unique index to reject second open request")
		}
	})
}

func TestReviewRequestConcurrentCreateEndsWithOneRow(t *testing.T) {
	forEachDraftBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a, _, _, _ := seedArtifact(t, st, "")
		now := time.Now().UTC().Truncate(time.Microsecond)
		actor := &Actor{Kind: PrincipalKindUser, Ref: "owner-1", Authority: AuthorityOwner}

		const workers = 2
		start := make(chan struct{})
		outcomes := make([]string, workers)
		ids := make([]string, workers)
		errs := make([]error, workers)
		var wg sync.WaitGroup
		for i := range workers {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-start
				row, out, err := st.CreateOrRemindReviewRequest(ctx, ReviewRequest{
					ArtifactID:    a.ID,
					ReviewerKind:  PrincipalKindUser,
					ReviewerRef:   "rev-race",
					RequesterKind: PrincipalKindUser,
					RequesterRef:  "owner-1",
					Note:          "concurrent",
					Access:        ReviewRequestAccessExisting,
				}, 24*time.Hour, now, actor, nil)
				outcomes[idx] = out
				ids[idx] = row.ID
				errs[idx] = err
			}(i)
		}
		close(start)
		wg.Wait()

		var createdCount, remindedCount int
		for i := range workers {
			if errs[i] != nil {
				t.Fatalf("worker %d err: %v", i, errs[i])
			}
			switch outcomes[i] {
			case ReviewRequestOutcomeCreated:
				createdCount++
			case ReviewRequestOutcomeReminded:
				remindedCount++
			default:
				t.Errorf("worker %d unexpected outcome %q", i, outcomes[i])
			}
		}
		if createdCount != 1 || remindedCount != 1 {
			t.Fatalf("outcomes = %v, want 1 created + 1 reminded", outcomes)
		}
		if ids[0] != ids[1] {
			t.Errorf("concurrent calls produced different ids %q vs %q", ids[0], ids[1])
		}
	})
}

func TestReviewRequestRemindCooldownAndDismissLifecycle(t *testing.T) {
	forEachDraftBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a, _, _, _ := seedArtifact(t, st, "")
		t0 := time.Now().UTC().Truncate(time.Microsecond)
		pubActor := &Actor{Kind: PrincipalKindUser, Ref: "owner-1", Authority: AuthorityPublisher}
		revActor := &Actor{Kind: PrincipalKindUser, Ref: "rev-1", Authority: AuthorityReviewer}

		req := ReviewRequest{
			ArtifactID:    a.ID,
			ReviewerKind:  PrincipalKindUser,
			ReviewerRef:   "rev-1",
			RequesterKind: PrincipalKindUser,
			RequesterRef:  "owner-1",
			Note:          "please check",
			Access:        ReviewRequestAccessExisting,
		}

		// 1. Create.
		created, out, err := st.CreateOrRemindReviewRequest(ctx, req, 24*time.Hour, t0, pubActor, nil)
		if err != nil || out != ReviewRequestOutcomeCreated {
			t.Fatalf("create: out=%q err=%v", out, err)
		}

		// 2. First remind with a new note succeeds and updates the stored note (reminded_at was nil).
		reqRemind := req
		reqRemind.Note = "updated note"
		rem1, out, err := st.CreateOrRemindReviewRequest(ctx, reqRemind, 24*time.Hour, t0.Add(time.Minute), pubActor, nil)
		if err != nil || out != ReviewRequestOutcomeReminded || rem1.RemindedAt == nil {
			t.Fatalf("first remind: out=%q remindedAt=%v err=%v", out, rem1.RemindedAt, err)
		}
		if rem1.Note != "updated note" {
			t.Fatalf("first remind note = %q, want %q", rem1.Note, "updated note")
		}

		// 3. Second remind within 24h is unchanged and does NOT overwrite the stored note.
		reqCooldown := req
		reqCooldown.Note = "ignored during cooldown"
		unch, out, err := st.CreateOrRemindReviewRequest(ctx, reqCooldown, 24*time.Hour, t0.Add(2*time.Hour), pubActor, nil)
		if err != nil || out != ReviewRequestOutcomeUnchanged {
			t.Fatalf("within cooldown: out=%q err=%v", out, err)
		}
		if unch.Note != "updated note" {
			t.Fatalf("cooldown note = %q, want %q", unch.Note, "updated note")
		}

		// 4. After 24h, remind with empty note succeeds and preserves the stored note.
		reqEmpty := req
		reqEmpty.Note = ""
		rem2, out, err := st.CreateOrRemindReviewRequest(ctx, reqEmpty, 24*time.Hour, t0.Add(25*time.Hour), pubActor, nil)
		if err != nil || out != ReviewRequestOutcomeReminded {
			t.Fatalf("after cooldown: out=%q err=%v", out, err)
		}
		if rem2.Note != "updated note" {
			t.Fatalf("remind empty note = %q, want %q", rem2.Note, "updated note")
		}

		// 5. GetOpenReviewRequest finds it with the updated note.
		got, err := st.GetOpenReviewRequest(ctx, a.ID, PrincipalKindUser, "rev-1")
		if err != nil || got.ID != created.ID || got.Note != "updated note" {
			t.Fatalf("GetOpenReviewRequest: got=%+v err=%v", got, err)
		}

		// 6. Dismiss transitions open -> dismissed.
		dismissed, err := st.DismissReviewRequest(ctx, a.ID, PrincipalKindUser, "rev-1", "busy this week", t0.Add(26*time.Hour), revActor)
		if err != nil {
			t.Fatalf("DismissReviewRequest: %v", err)
		}
		if dismissed.State != ReviewRequestStateDismissed || dismissed.ResponseNote != "busy this week" || dismissed.ResolvedAt == nil {
			t.Fatalf("dismissed row mismatch: %+v", dismissed)
		}

		// 7. GetOpenReviewRequest and second dismiss return ErrNotFound.
		if _, err := st.GetOpenReviewRequest(ctx, a.ID, PrincipalKindUser, "rev-1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound after dismiss, got %v", err)
		}
		if _, err := st.DismissReviewRequest(ctx, a.ID, PrincipalKindUser, "rev-1", "again", t0.Add(27*time.Hour), revActor); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second dismiss: got %v, want ErrNotFound", err)
		}

		// 8. Requesting again after dismiss creates a new open row (closed row kept).
		recreated, out, err := st.CreateOrRemindReviewRequest(ctx, req, 24*time.Hour, t0.Add(28*time.Hour), pubActor, nil)
		if err != nil || out != ReviewRequestOutcomeCreated || recreated.ID == created.ID {
			t.Fatalf("recreate after dismiss: id=%q old=%q out=%q err=%v", recreated.ID, created.ID, out, err)
		}

		// 9. Verify audit events recorded.
		events, err := st.ListEvents(ctx, a.ID)
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		counts := map[string]int{}
		for _, ev := range events {
			counts[ev.Action]++
		}
		if counts[EventReviewRequestCreated] != 2 || counts[EventReviewRequestReminded] != 2 || counts[EventReviewRequestDismissed] != 1 {
			t.Errorf("unexpected event counts: %+v", counts)
		}
	})
}

func TestReviewRequestDismissLostCAS(t *testing.T) {
	forEachDraftBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a, _, _, _ := seedArtifact(t, st, "")
		now := time.Now().UTC().Truncate(time.Microsecond)
		pubActor := &Actor{Kind: PrincipalKindUser, Ref: "owner-1", Authority: AuthorityPublisher}
		revActor := &Actor{Kind: PrincipalKindUser, Ref: "rev-1", Authority: AuthorityReviewer}

		created, _, err := st.CreateOrRemindReviewRequest(ctx, ReviewRequest{
			ArtifactID:    a.ID,
			ReviewerKind:  PrincipalKindUser,
			ReviewerRef:   "rev-1",
			RequesterKind: PrincipalKindUser,
			RequesterRef:  "owner-1",
			Access:        ReviewRequestAccessExisting,
		}, 24*time.Hour, now, pubActor, nil)
		if err != nil {
			t.Fatalf("create: %v", err)
		}

		ss := st.(*sqlStore)
		prevHook := reviewRequestDismissHook
		reviewRequestDismissHook = func(hctx context.Context) {
			// Simulate concurrent close before our CAS UPDATE runs.
			_, _ = db.ExecContext(hctx, ss.rebind(`UPDATE artifact_review_request SET state = ? WHERE id = ?`),
				ReviewRequestStateFulfilled, created.ID)
		}
		defer func() { reviewRequestDismissHook = prevHook }()

		_, err = st.DismissReviewRequest(ctx, a.ID, PrincipalKindUser, "rev-1", "late dismiss", now.Add(time.Minute), revActor)
		if !errors.Is(err, ErrRequestClosed) {
			t.Fatalf("lost CAS: got %v, want ErrRequestClosed", err)
		}
	})
}

func TestReviewRequestListWithdrawAndRaisedGrant(t *testing.T) {
	forEachDraftBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a, _, _, _ := seedArtifact(t, st, "")
		t0 := time.Now().UTC().Truncate(time.Microsecond)
		ownerActor := &Actor{Kind: PrincipalKindUser, Ref: "owner-1", Authority: AuthorityOwner}

		// Create with raised grant in the same transaction.
		grant := &Grant{
			ArtifactID:   a.ID,
			SubjectKind:  SubjectPrincipal,
			SubjectRef:   PrincipalRef(PrincipalKindUser, "rev-raised"),
			Permission:   GrantReview,
			CreatedByRef: PrincipalRef(PrincipalKindUser, "owner-1"),
			CreatedAt:    t0,
		}
		created, out, err := st.CreateOrRemindReviewRequest(ctx, ReviewRequest{
			ArtifactID:    a.ID,
			ReviewerKind:  PrincipalKindUser,
			ReviewerRef:   "rev-raised",
			RequesterKind: PrincipalKindUser,
			RequesterRef:  "owner-1",
			Note:          "raised grant test",
			Access:        ReviewRequestAccessRaised,
		}, 24*time.Hour, t0, ownerActor, &ReviewRequestGrantOption{
			Grant:     grant,
			MaxGrants: MaxGrantsPerArtifact,
			Actor:     ownerActor,
		})
		if err != nil || out != ReviewRequestOutcomeCreated {
			t.Fatalf("create raised: out=%q err=%v", out, err)
		}
		if created.GrantID == "" || created.GrantID != grant.ID {
			t.Fatalf("created.GrantID = %q, want %q", created.GrantID, grant.ID)
		}

		// Verify the review grant was written.
		grants, err := st.ListGrants(ctx, a.ID)
		if err != nil {
			t.Fatalf("ListGrants: %v", err)
		}
		var foundGrant bool
		for _, g := range grants {
			if g.ID == created.GrantID && g.SubjectRef == "user:rev-raised" && g.Permission == GrantReview {
				foundGrant = true
			}
		}
		if !foundGrant {
			t.Fatalf("expected raised review grant in ListGrants, got %+v", grants)
		}

		// Enforcing maxGrants rolls back the request too.
		_, _, err = st.CreateOrRemindReviewRequest(ctx, ReviewRequest{
			ArtifactID:    a.ID,
			ReviewerKind:  PrincipalKindUser,
			ReviewerRef:   "rev-overcap",
			RequesterKind: PrincipalKindUser,
			RequesterRef:  "owner-1",
			Access:        ReviewRequestAccessRaised,
		}, 24*time.Hour, t0, ownerActor, &ReviewRequestGrantOption{
			Grant: &Grant{
				ArtifactID:   a.ID,
				SubjectKind:  SubjectPrincipal,
				SubjectRef:   PrincipalRef(PrincipalKindUser, "rev-overcap"),
				Permission:   GrantReview,
				CreatedByRef: PrincipalRef(PrincipalKindUser, "owner-1"),
				CreatedAt:    t0,
			},
			MaxGrants: 1,
			Actor:     ownerActor,
		})
		if !errors.Is(err, ErrTooManyGrants) {
			t.Fatalf("over-cap raised create: got %v, want ErrTooManyGrants", err)
		}
		if _, err := st.GetOpenReviewRequest(ctx, a.ID, PrincipalKindUser, "rev-overcap"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("over-cap rollback check: got %v, want ErrNotFound", err)
		}

		// ListReviewRequests and ListReviewRequestsForReviewer.
		all, err := st.ListReviewRequests(ctx, a.ID, 50)
		if err != nil || len(all) != 1 || all[0].ID != created.ID {
			t.Fatalf("ListReviewRequests = %+v, %v", all, err)
		}
		mine, err := st.ListReviewRequestsForReviewer(ctx, a.ID, PrincipalKindUser, "rev-raised", 50)
		if err != nil || len(mine) != 1 || mine[0].ID != created.ID {
			t.Fatalf("ListReviewRequestsForReviewer = %+v, %v", mine, err)
		}
		other, err := st.ListReviewRequestsForReviewer(ctx, a.ID, PrincipalKindUser, "someone-else", 50)
		if err != nil || len(other) != 0 {
			t.Fatalf("ListReviewRequestsForReviewer(other) = %+v, %v", other, err)
		}

		// Withdraw transitions open -> withdrawn and leaves the raised grant in place.
		withdrawn, err := st.WithdrawReviewRequest(ctx, a.ID, created.ID, t0.Add(time.Minute), ownerActor)
		if err != nil {
			t.Fatalf("WithdrawReviewRequest: %v", err)
		}
		if withdrawn.State != ReviewRequestStateWithdrawn || withdrawn.ResolvedAt == nil {
			t.Fatalf("withdrawn row mismatch: %+v", withdrawn)
		}
		if _, err := st.WithdrawReviewRequest(ctx, a.ID, created.ID, t0.Add(2*time.Minute), ownerActor); !errors.Is(err, ErrRequestClosed) {
			t.Fatalf("second withdraw = %v, want ErrRequestClosed", err)
		}
		grantsAfter, err := st.ListGrants(ctx, a.ID)
		if err != nil {
			t.Fatalf("ListGrants after withdraw: %v", err)
		}
		var stillThere bool
		for _, g := range grantsAfter {
			if g.ID == created.GrantID {
				stillThere = true
			}
		}
		if !stillThere {
			t.Errorf("withdrawing a raised request must leave its grant in place")
		}
		events, err := st.ListEvents(ctx, a.ID)
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		var hasWithdrawnEvent bool
		for _, ev := range events {
			if ev.Action == EventReviewRequestWithdrawn {
				hasWithdrawnEvent = true
			}
		}
		if !hasWithdrawnEvent {
			t.Errorf("expected %s event", EventReviewRequestWithdrawn)
		}
	})
}

func TestFinalizeVersionFulfilsRequestCarryForwardAndDeletesDraft(t *testing.T) {
	forEachDraftBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a, _, _, _ := seedArtifact(t, st, "")
		t0 := time.Now().UTC().Truncate(time.Microsecond)
		ownerActor := &Actor{Kind: PrincipalKindUser, Ref: "owner-1", Authority: AuthorityOwner}

		req1, _, err := st.CreateOrRemindReviewRequest(ctx, ReviewRequest{
			ArtifactID: a.ID, ReviewerKind: PrincipalKindUser, ReviewerRef: "rev-1",
			RequesterKind: PrincipalKindUser, RequesterRef: "owner-1", Access: ReviewRequestAccessExisting,
		}, 24*time.Hour, t0, ownerActor, nil)
		if err != nil {
			t.Fatalf("create req1: %v", err)
		}
		req2, _, err := st.CreateOrRemindReviewRequest(ctx, ReviewRequest{
			ArtifactID: a.ID, ReviewerKind: PrincipalKindUser, ReviewerRef: "rev-2",
			RequesterKind: PrincipalKindUser, RequesterRef: "owner-1", Access: ReviewRequestAccessExisting,
		}, 24*time.Hour, t0, ownerActor, nil)
		if err != nil {
			t.Fatalf("create req2: %v", err)
		}

		for _, rev := range []string{"rev-1", "rev-2"} {
			if _, _, err := st.PutReviewDraft(ctx, ReviewDraft{
				ArtifactID: a.ID, PrincipalKind: PrincipalKindUser, PrincipalRef: rev,
				BaseSeq: 1, EntryPath: "doc.md", Body: "draft by " + rev,
			}, 0, t0, MaxDraftsPerPrincipal); err != nil {
				t.Fatalf("PutReviewDraft(%s): %v", rev, err)
			}
		}

		// Publish v2 (kind: publish) -> carry forward: both requests stay open with requested_seq=1.
		v2 := &Version{
			ID: "v2-" + a.ID, ArtifactID: a.ID, Kind: VersionKindPublish,
			EntryPath: "doc.md", CreatedByKind: PrincipalKindUser, CreatedByRef: "owner-1",
			CreatedAt: t0.Add(time.Minute), State: VersionStatePending,
		}
		if err := st.CreateVersion(ctx, v2, []File{{VersionID: v2.ID, Path: "doc.md", Size: 5, SHA256: "aaaa", MediaType: "text/markdown", Pending: false}}, 10); err != nil {
			t.Fatalf("CreateVersion v2: %v", err)
		}
		claim2, err := st.ClaimFinalize(ctx, a.ID, v2.Seq, t0)
		if err != nil {
			t.Fatalf("ClaimFinalize v2: %v", err)
		}
		if _, effects, err := st.FinalizeVersionWithEffects(ctx, a.ID, v2.Seq, claim2, nil, 0); err != nil {
			t.Fatalf("FinalizeVersion v2: %v", err)
		} else if effects.FulfilledRequestID != "" || effects.DraftDeleted {
			t.Errorf("publish finalize had unexpected effects: %+v", effects)
		}
		stillOpen1, err := st.GetOpenReviewRequest(ctx, a.ID, PrincipalKindUser, "rev-1")
		if err != nil || stillOpen1.RequestedSeq != 1 || stillOpen1.State != ReviewRequestStateOpen {
			t.Fatalf("after publish v2, req1 = %+v, %v; want open with requestedSeq=1", stillOpen1, err)
		}

		// rev-1 finalizes a review against v2 -> fulfils ONLY rev-1's request (resolvedSeq=3)
		// and deletes ONLY rev-1's draft in the same transaction.
		v3 := &Version{
			ID: "v3-" + a.ID, ArtifactID: a.ID, Kind: VersionKindReview,
			EntryPath: "doc.md", CreatedByKind: PrincipalKindUser, CreatedByRef: "rev-1",
			CreatedAt: t0.Add(2 * time.Minute), State: VersionStatePending,
		}
		if err := st.CreateVersion(ctx, v3, []File{{VersionID: v3.ID, Path: "doc.md", Size: 6, SHA256: "bbbb", MediaType: "text/markdown", Pending: false}}, 10); err != nil {
			t.Fatalf("CreateVersion v3: %v", err)
		}
		claim3, err := st.ClaimFinalize(ctx, a.ID, v3.Seq, t0)
		if err != nil {
			t.Fatalf("ClaimFinalize v3: %v", err)
		}
		_, effects3, err := st.FinalizeVersionWithEffects(ctx, a.ID, v3.Seq, claim3, nil, v2.Seq)
		if err != nil {
			t.Fatalf("FinalizeVersion v3: %v", err)
		}
		if effects3.FulfilledRequestID != req1.ID || !effects3.DraftDeleted {
			t.Fatalf("finalize review effects = %+v, want FulfilledRequestID=%q DraftDeleted=true", effects3, req1.ID)
		}

		got1, err := st.GetReviewRequest(ctx, a.ID, req1.ID)
		if err != nil || got1.State != ReviewRequestStateFulfilled || got1.ResolvedSeq == nil || *got1.ResolvedSeq != v3.Seq || got1.RequestedSeq != 1 {
			t.Fatalf("req1 after fulfil = %+v, %v", got1, err)
		}
		got2, err := st.GetOpenReviewRequest(ctx, a.ID, PrincipalKindUser, "rev-2")
		if err != nil || got2.ID != req2.ID || got2.State != ReviewRequestStateOpen {
			t.Fatalf("req2 after rev-1 review = %+v, %v; want still open", got2, err)
		}

		if _, err := st.GetReviewDraft(ctx, a.ID, PrincipalKindUser, "rev-1"); !errors.Is(err, ErrNotFound) {
			t.Errorf("rev-1 draft after finalize = %v, want ErrNotFound", err)
		}
		if _, err := st.GetReviewDraft(ctx, a.ID, PrincipalKindUser, "rev-2"); err != nil {
			t.Errorf("rev-2 draft should remain intact, got %v", err)
		}

		events, err := st.ListEvents(ctx, a.ID)
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		var hasFulfilledEvent bool
		for _, ev := range events {
			if ev.Action == EventReviewRequestFulfilled {
				hasFulfilledEvent = true
			}
		}
		if !hasFulfilledEvent {
			t.Errorf("expected %s event", EventReviewRequestFulfilled)
		}
	})
}

func TestDismissVsFulfilRace(t *testing.T) {
	forEachDraftBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a, _, _, _ := seedArtifact(t, st, "")
		t0 := time.Now().UTC().Truncate(time.Microsecond)
		ownerActor := &Actor{Kind: PrincipalKindUser, Ref: "owner-1", Authority: AuthorityOwner}
		revActor := &Actor{Kind: PrincipalKindUser, Ref: "rev-1", Authority: AuthorityReviewer}

		req, _, err := st.CreateOrRemindReviewRequest(ctx, ReviewRequest{
			ArtifactID: a.ID, ReviewerKind: PrincipalKindUser, ReviewerRef: "rev-1",
			RequesterKind: PrincipalKindUser, RequesterRef: "owner-1", Access: ReviewRequestAccessExisting,
		}, 24*time.Hour, t0, ownerActor, nil)
		if err != nil {
			t.Fatalf("create: %v", err)
		}

		v2 := &Version{
			ID: "v2-race-" + a.ID, ArtifactID: a.ID, Kind: VersionKindReview,
			EntryPath: "doc.md", CreatedByKind: PrincipalKindUser, CreatedByRef: "rev-1",
			CreatedAt: t0.Add(time.Minute), State: VersionStatePending,
		}
		if err := st.CreateVersion(ctx, v2, []File{{VersionID: v2.ID, Path: "doc.md", Size: 4, SHA256: "cccc", MediaType: "text/markdown", Pending: false}}, 10); err != nil {
			t.Fatalf("CreateVersion: %v", err)
		}
		claim, err := st.ClaimFinalize(ctx, a.ID, v2.Seq, t0)
		if err != nil {
			t.Fatalf("ClaimFinalize: %v", err)
		}

		reviewRequestDismissHook = func(hctx context.Context) {
			if _, _, ferr := st.FinalizeVersionWithEffects(hctx, a.ID, v2.Seq, claim, nil, 1); ferr != nil {
				t.Errorf("hook FinalizeVersion: %v", ferr)
			}
		}
		t.Cleanup(func() { reviewRequestDismissHook = nil })

		_, err = st.DismissReviewRequest(ctx, a.ID, PrincipalKindUser, "rev-1", "too late", t0.Add(2*time.Minute), revActor)
		if !errors.Is(err, ErrRequestClosed) {
			t.Fatalf("DismissReviewRequest losing to FinalizeVersion = %v, want ErrRequestClosed", err)
		}
		got, err := st.GetReviewRequest(ctx, a.ID, req.ID)
		if err != nil || got.State != ReviewRequestStateFulfilled {
			t.Fatalf("final state = %+v, %v; want fulfilled", got, err)
		}
	})
}

func TestSoftDeleteCleansReviewRequestsAndReturnsOpenReviewers(t *testing.T) {
	forEachDraftBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a, _, _, _ := seedArtifact(t, st, "")
		t0 := time.Now().UTC().Truncate(time.Microsecond)
		ownerActor := &Actor{Kind: PrincipalKindUser, Ref: "owner-1", Authority: AuthorityOwner}
		revActor := &Actor{Kind: PrincipalKindUser, Ref: "rev-closed", Authority: AuthorityReviewer}

		for _, rev := range []string{"rev-open-a", "rev-open-b", "rev-closed"} {
			if _, _, err := st.CreateOrRemindReviewRequest(ctx, ReviewRequest{
				ArtifactID: a.ID, ReviewerKind: PrincipalKindUser, ReviewerRef: rev,
				RequesterKind: PrincipalKindUser, RequesterRef: "owner-1", Access: ReviewRequestAccessExisting,
			}, 24*time.Hour, t0, ownerActor, nil); err != nil {
				t.Fatalf("create %s: %v", rev, err)
			}
		}
		if _, err := st.DismissReviewRequest(ctx, a.ID, PrincipalKindUser, "rev-closed", "bye", t0.Add(time.Minute), revActor); err != nil {
			t.Fatalf("dismiss: %v", err)
		}

		delRes, err := st.DeleteArtifact(ctx, a.ID, DeleteOptions{Actor: ownerActor, ScopeRef: a.ScopeRef, Now: t0.Add(2 * time.Minute)})
		if err != nil {
			t.Fatalf("DeleteArtifact: %v", err)
		}
		if delRes.ReviewRequests != 3 {
			t.Errorf("DeleteResult.ReviewRequests = %d, want 3", delRes.ReviewRequests)
		}
		if len(delRes.OpenRequestReviewers) != 2 || delRes.OpenRequestReviewers[0] != "rev-open-a" || delRes.OpenRequestReviewers[1] != "rev-open-b" {
			t.Errorf("DeleteResult.OpenRequestReviewers = %v, want [rev-open-a rev-open-b]", delRes.OpenRequestReviewers)
		}
		rem, err := st.ListReviewRequests(ctx, a.ID, 50)
		if err != nil || len(rem) != 0 {
			t.Errorf("remaining requests after soft-delete = %+v, %v; want empty", rem, err)
		}
	})
}

func TestReviewRequestReaperStoreExpireAndPurge(t *testing.T) {
	forEachDraftBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a, _, _, _ := seedArtifact(t, st, "")
		t0 := time.Now().UTC().Truncate(time.Microsecond)
		ownerActor := &Actor{Kind: PrincipalKindUser, Ref: "owner-1", Authority: AuthorityOwner}

		rr, _, err := st.CreateOrRemindReviewRequest(ctx, ReviewRequest{
			ArtifactID: a.ID, ReviewerKind: PrincipalKindUser, ReviewerRef: "rev-exp",
			RequesterKind: PrincipalKindUser, RequesterRef: "owner-1", Access: ReviewRequestAccessExisting,
		}, 24*time.Hour, t0, ownerActor, nil)
		if err != nil {
			t.Fatalf("create: %v", err)
		}

		// CAS mismatch on updated_at does not expire.
		expired, err := st.ExpireReviewRequest(ctx, a.ID, rr.ID, t0.Add(time.Second), "ttl", t0.Add(31*24*time.Hour), &reconcilerActor)
		if err != nil || expired {
			t.Fatalf("wrong updated_at expire = %v, %v; want false, nil", expired, err)
		}

		// Matching updated_at expires and writes EventReviewRequestExpired.
		expiredAt := t0.Add(31 * 24 * time.Hour)
		expired, err = st.ExpireReviewRequest(ctx, a.ID, rr.ID, rr.UpdatedAt, "ttl", expiredAt, &reconcilerActor)
		if err != nil || !expired {
			t.Fatalf("expire = %v, %v; want true, nil", expired, err)
		}

		// Purge before 90 days keeps the row.
		purged, err := st.PurgeClosedReviewRequests(ctx, expiredAt.Add(-time.Hour), 50)
		if err != nil || purged != 0 {
			t.Fatalf("early purge = %d, %v; want 0, nil", purged, err)
		}
		// Purge at 90 days after resolved_at deletes the row.
		purged, err = st.PurgeClosedReviewRequests(ctx, expiredAt, 50)
		if err != nil || purged != 1 {
			t.Fatalf("90d purge = %d, %v; want 1, nil", purged, err)
		}
	})
}
