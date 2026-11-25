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
		}, 24*time.Hour, now, actor)
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
				}, 24*time.Hour, now, actor)
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
		created, out, err := st.CreateOrRemindReviewRequest(ctx, req, 24*time.Hour, t0, pubActor)
		if err != nil || out != ReviewRequestOutcomeCreated {
			t.Fatalf("create: out=%q err=%v", out, err)
		}

		// 2. First remind succeeds (reminded_at was nil).
		rem1, out, err := st.CreateOrRemindReviewRequest(ctx, req, 24*time.Hour, t0.Add(time.Minute), pubActor)
		if err != nil || out != ReviewRequestOutcomeReminded || rem1.RemindedAt == nil {
			t.Fatalf("first remind: out=%q remindedAt=%v err=%v", out, rem1.RemindedAt, err)
		}

		// 3. Second remind within 24h is unchanged.
		_, out, err = st.CreateOrRemindReviewRequest(ctx, req, 24*time.Hour, t0.Add(2*time.Hour), pubActor)
		if err != nil || out != ReviewRequestOutcomeUnchanged {
			t.Fatalf("within cooldown: out=%q err=%v", out, err)
		}

		// 4. After 24h, remind succeeds again.
		_, out, err = st.CreateOrRemindReviewRequest(ctx, req, 24*time.Hour, t0.Add(25*time.Hour), pubActor)
		if err != nil || out != ReviewRequestOutcomeReminded {
			t.Fatalf("after cooldown: out=%q err=%v", out, err)
		}

		// 5. GetOpenReviewRequest finds it.
		got, err := st.GetOpenReviewRequest(ctx, a.ID, PrincipalKindUser, "rev-1")
		if err != nil || got.ID != created.ID {
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
		recreated, out, err := st.CreateOrRemindReviewRequest(ctx, req, 24*time.Hour, t0.Add(28*time.Hour), pubActor)
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
		}, 24*time.Hour, now, pubActor)
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
