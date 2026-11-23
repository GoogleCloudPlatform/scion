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
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type artifactReviewWakeFixture struct {
	srv        *Server
	store      store.Store
	db         *sql.DB
	disp       *wakeTrackingDispatcher
	project    *store.Project
	owner      *store.Agent
	ownerTok   string
	adminUser  *store.User
	reviewUser *store.User
	artifactID string
	currentSeq int
	parentText string
}

func newArtifactReviewWakeFixture(t *testing.T, idleSuspendOn bool) *artifactReviewWakeFixture {
	t.Helper()
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	setExperimentOverrides(t, srv, map[string]bool{
		experiments.Artifacts:   true,
		experiments.IdleSuspend: idleSuspendOn,
	})
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "review-wake.db")+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	st := artifacts.NewStore(db, "sqlite")
	require.NoError(t, st.Init(context.Background()))
	srv.SetArtifactStore(st)

	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)

	ctx := context.Background()
	p := artifactProject(t, s, "rev-wake-p1")
	broker := &store.RuntimeBroker{
		ID:     tid("rev-wake-broker"),
		Name:   "Review Wake Broker",
		Slug:   "rev-wake-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  p.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	owner, ownerTok := artifactAgent(t, srv, s, p.ID, "review-owner", AgentRoleBaseline)
	owner.RuntimeBrokerID = broker.ID
	owner.Phase = string(state.PhaseRunning)
	require.NoError(t, s.UpdateAgent(ctx, owner))

	// Project owner has agent.lifecycle on the agent and can wake it.
	createTestUserWithProjectRole(t, s, tid("rev-wake-admin"), "rev-wake-admin@test.com", p.ID, store.ProjectRoleOwner)
	adminUser, err := s.GetUser(ctx, tid("rev-wake-admin"))
	require.NoError(t, err)

	// Project viewer with an explicit review grant: can submit reviews,
	// but lacks agent.lifecycle so cannot wake the agent.
	createTestUserWithProjectRole(t, s, tid("rev-wake-viewer"), "rev-wake-viewer@test.com", p.ID, store.ProjectRoleViewer)
	reviewUser, err := s.GetUser(ctx, tid("rev-wake-viewer"))
	require.NoError(t, err)

	const parent = "# Plan\n\nWe ship in Q3.\n"
	sum := sha256.Sum256([]byte(parent))
	createBody, err := json.Marshal(artifacts.CreateVersionRequest{
		Key:   "plan",
		Entry: "plan.md",
		Files: []artifacts.ManifestFile{{Path: "plan.md", Size: int64(len(parent)), SHA256: hex.EncodeToString(sum[:])}},
	})
	require.NoError(t, err)
	rec := doRawAgentRequest(t, srv, http.MethodPost, "/api/v1/artifacts", createBody, ownerTok)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var pend artifacts.PendingVersionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pend))
	id := pend.Artifact.ID
	require.Equal(t, http.StatusNoContent, doRawAgentRequest(t, srv, http.MethodPut, "/api/v1/artifacts/"+id+"/versions/1/files/plan.md", []byte(parent), ownerTok).Code)
	require.Equal(t, http.StatusOK, doRawAgentRequest(t, srv, http.MethodPost, "/api/v1/artifacts/"+id+"/versions/1/finalize", nil, ownerTok).Code)

	for i, u := range []*store.User{adminUser, reviewUser} {
		_, err = db.Exec(`INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("g-rev-%d", i), id, artifacts.SubjectPrincipal, artifacts.PrincipalRef(artifacts.PrincipalKindUser, u.ID), artifacts.GrantReview,
			time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00"))
		require.NoError(t, err)
	}

	t.Cleanup(func() { srv.autoWakeWork.wg.Wait() })
	return &artifactReviewWakeFixture{
		srv:        srv,
		store:      s,
		db:         db,
		disp:       disp,
		project:    p,
		owner:      owner,
		ownerTok:   ownerTok,
		adminUser:  adminUser,
		reviewUser: reviewUser,
		artifactID: id,
		currentSeq: 1,
		parentText: parent,
	}
}

func (f *artifactReviewWakeFixture) suspendOwner(t *testing.T, reason state.SuspendReason) {
	t.Helper()
	require.NoError(t, f.store.UpdateAgentStatus(context.Background(), f.owner.ID, store.AgentStatusUpdate{
		Phase:         string(state.PhaseSuspended),
		SuspendReason: string(reason),
	}))
	got, err := f.store.GetAgent(context.Background(), f.owner.ID)
	require.NoError(t, err)
	*f.owner = *got
}

func (f *artifactReviewWakeFixture) readyOnStart(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	t.Cleanup(func() { close(done); wg.Wait() })
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			case <-time.After(15 * time.Millisecond):
			}
			a, err := f.store.GetAgent(context.Background(), f.owner.ID)
			if err == nil && a.Phase == string(state.PhaseStarting) {
				_ = f.store.UpdateAgentStatus(context.Background(), f.owner.ID, store.AgentStatusUpdate{
					Phase:    string(state.PhaseRunning),
					Activity: "idle",
				})
			}
		}
	}()
}

// stageReview uploads a valid CriticMarkup review and returns its pending seq.
func (f *artifactReviewWakeFixture) stageReview(t *testing.T, user *store.User) int {
	t.Helper()
	body := "# Plan\n\nWe {~~ship~>launch~~} in Q3.\n"
	sum := sha256.Sum256([]byte(body))
	reqBytes, err := json.Marshal(artifacts.CreateVersionRequest{
		Kind:  artifacts.VersionKindReview,
		Entry: "plan.md",
		Files: []artifacts.ManifestFile{{Path: "plan.md", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}},
	})
	require.NoError(t, err)
	rec := userArtifactRequest(t, f.srv, user, http.MethodPost, "/api/v1/artifacts/"+f.artifactID+"/versions", reqBytes)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var p artifacts.PendingVersionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	putURL := fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/files/plan.md", f.artifactID, p.Version.Seq)
	require.Equal(t, http.StatusNoContent, userArtifactRequest(t, f.srv, user, http.MethodPut, putURL, []byte(body)).Code)
	return p.Version.Seq
}

func (f *artifactReviewWakeFixture) finalizeReview(t *testing.T, user *store.User, seq int, freq artifacts.FinalizeRequest) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(freq)
	require.NoError(t, err)
	url := fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/finalize", f.artifactID, seq)
	return userArtifactRequest(t, f.srv, user, http.MethodPost, url, b)
}

func TestArtifactReviewWake_SystemSuspendedAutoWakes(t *testing.T) {
	f := newArtifactReviewWakeFixture(t, true)
	f.suspendOwner(t, state.SuspendReasonIdle)
	f.readyOnStart(t)

	// Even a viewer without agent.lifecycle auto-wakes a system-suspended owner.
	seq := f.stageReview(t, f.reviewUser)
	rec := f.finalizeReview(t, f.reviewUser, seq, artifacts.FinalizeRequest{
		Base:      f.currentSeq,
		OfferWake: true,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp artifacts.ArtifactResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.ReviewDelivery)
	assert.Equal(t, artifacts.ReviewDeliveryAutoWaking, resp.ReviewDelivery.Status)
	assert.Equal(t, f.owner.ID, resp.ReviewDelivery.AgentID)
	assert.Equal(t, f.owner.Slug, resp.ReviewDelivery.AgentSlug)
	assert.Equal(t, seq, resp.Artifact.CurrentSeq)

	require.Eventually(t, func() bool {
		return len(f.disp.getStartCalls()) == 1 && len(f.disp.getMessageCalls()) == 1
	}, 5*time.Second, 20*time.Millisecond, "agent should auto-wake and receive the review notice")
	assert.True(t, f.disp.getStartCalls()[0].Continue, "wake should resume with continue=true")
	assert.Contains(t, f.disp.getMessageCalls()[0].Message, "A review (v2, by a user) was published")
}

func TestArtifactReviewWake_UserSuspendedOfferAndConfirm(t *testing.T) {
	f := newArtifactReviewWakeFixture(t, true)
	f.suspendOwner(t, state.SuspendReasonUser)
	f.readyOnStart(t)

	seq := f.stageReview(t, f.adminUser)

	// Step 1: offer_wake=true returns 409 agent_not_running with canWake=true
	// without finalizing the pending version.
	offerRec := f.finalizeReview(t, f.adminUser, seq, artifacts.FinalizeRequest{
		Base:      f.currentSeq,
		OfferWake: true,
	})
	require.Equal(t, http.StatusConflict, offerRec.Code, offerRec.Body.String())
	var errBody struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(offerRec.Body.Bytes(), &errBody))
	assert.Equal(t, ErrCodeAgentNotRunning, errBody.Error.Code)
	assert.Equal(t, true, errBody.Error.Details["canWake"])
	assert.Equal(t, f.owner.ID, errBody.Error.Details["agentId"])
	assert.Equal(t, f.owner.Slug, errBody.Error.Details["agentSlug"])
	assert.Empty(t, f.disp.getStartCalls(), "offer must not wake the agent")

	// Step 2: user confirms -> retry finalize on the same pending seq with wake=true.
	finRec := f.finalizeReview(t, f.adminUser, seq, artifacts.FinalizeRequest{
		Base: f.currentSeq,
		Wake: true,
	})
	require.Equal(t, http.StatusOK, finRec.Code, finRec.Body.String())

	var resp artifacts.ArtifactResponse
	require.NoError(t, json.Unmarshal(finRec.Body.Bytes(), &resp))
	assert.Equal(t, seq, resp.Artifact.CurrentSeq)
	require.NotNil(t, resp.ReviewDelivery)
	assert.Equal(t, artifacts.ReviewDeliveryWoken, resp.ReviewDelivery.Status)
	assert.Equal(t, f.owner.Slug, resp.ReviewDelivery.AgentSlug)

	require.Len(t, f.disp.getStartCalls(), 1)
	assert.True(t, f.disp.getStartCalls()[0].Continue)
	require.Len(t, f.disp.getMessageCalls(), 1)
	assert.Contains(t, f.disp.getMessageCalls()[0].Message, "A review (v2, by a user) was published")
}

func TestArtifactReviewWake_UserSuspendedDeclineStillSaves(t *testing.T) {
	f := newArtifactReviewWakeFixture(t, true)
	f.suspendOwner(t, state.SuspendReasonUser)

	seq := f.stageReview(t, f.adminUser)

	// Offer first, then decline by finalizing without offer_wake or wake.
	offerRec := f.finalizeReview(t, f.adminUser, seq, artifacts.FinalizeRequest{
		Base:      f.currentSeq,
		OfferWake: true,
	})
	require.Equal(t, http.StatusConflict, offerRec.Code)

	finRec := f.finalizeReview(t, f.adminUser, seq, artifacts.FinalizeRequest{
		Base: f.currentSeq,
	})
	require.Equal(t, http.StatusOK, finRec.Code, finRec.Body.String())

	var resp artifacts.ArtifactResponse
	require.NoError(t, json.Unmarshal(finRec.Body.Bytes(), &resp))
	assert.Equal(t, seq, resp.Artifact.CurrentSeq)
	require.NotNil(t, resp.ReviewDelivery)
	assert.Equal(t, artifacts.ReviewDeliverySuspended, resp.ReviewDelivery.Status)
	assert.Equal(t, f.owner.Slug, resp.ReviewDelivery.AgentSlug)
	assert.Empty(t, f.disp.getStartCalls(), "declined wake must not start the agent")
	assert.Empty(t, f.disp.getMessageCalls())
}

func TestArtifactReviewWake_UnauthorizedCallerNoOfferAndRefusedWakeStillSaves(t *testing.T) {
	t.Run("offer_wake skips dialog and saves directly", func(t *testing.T) {
		f := newArtifactReviewWakeFixture(t, true)
		f.suspendOwner(t, state.SuspendReasonUser)

		seq := f.stageReview(t, f.reviewUser)
		rec := f.finalizeReview(t, f.reviewUser, seq, artifacts.FinalizeRequest{
			Base:      f.currentSeq,
			OfferWake: true,
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp artifacts.ArtifactResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, seq, resp.Artifact.CurrentSeq)
		require.NotNil(t, resp.ReviewDelivery)
		assert.Equal(t, artifacts.ReviewDeliverySuspended, resp.ReviewDelivery.Status)
		assert.Empty(t, f.disp.getStartCalls())
	})

	t.Run("explicit wake=true refused but review still saves", func(t *testing.T) {
		f := newArtifactReviewWakeFixture(t, true)
		f.suspendOwner(t, state.SuspendReasonUser)

		seq := f.stageReview(t, f.reviewUser)
		rec := f.finalizeReview(t, f.reviewUser, seq, artifacts.FinalizeRequest{
			Base: f.currentSeq,
			Wake: true,
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp artifacts.ArtifactResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, seq, resp.Artifact.CurrentSeq, "review must still be saved when wake is refused")
		require.NotNil(t, resp.ReviewDelivery)
		assert.Equal(t, artifacts.ReviewDeliveryWakeRefused, resp.ReviewDelivery.Status)
		assert.NotEmpty(t, resp.ReviewDelivery.Message)
		assert.Empty(t, f.disp.getStartCalls(), "unauthorized caller must not wake agent")
	})
}

func TestArtifactReviewWake_WakeFailureStillSavesReview(t *testing.T) {
	f := newArtifactReviewWakeFixture(t, true)
	f.suspendOwner(t, state.SuspendReasonUser)
	f.disp.startReturnErr = errors.New("broker unavailable")

	seq := f.stageReview(t, f.adminUser)
	rec := f.finalizeReview(t, f.adminUser, seq, artifacts.FinalizeRequest{
		Base: f.currentSeq,
		Wake: true,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp artifacts.ArtifactResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, seq, resp.Artifact.CurrentSeq, "review must still be saved when wake fails")
	require.NotNil(t, resp.ReviewDelivery)
	assert.Equal(t, artifacts.ReviewDeliveryWakeFailed, resp.ReviewDelivery.Status)
	assert.NotEmpty(t, resp.ReviewDelivery.Message)
	assert.Len(t, f.disp.getStartCalls(), 1)
	assert.Empty(t, f.disp.getMessageCalls(), "notice must not be dispatched when wake failed")
}

func TestArtifactReviewWake_RunningAgentUnchanged(t *testing.T) {
	f := newArtifactReviewWakeFixture(t, true)

	seq := f.stageReview(t, f.adminUser)
	rec := f.finalizeReview(t, f.adminUser, seq, artifacts.FinalizeRequest{
		Base:      f.currentSeq,
		OfferWake: true,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp artifacts.ArtifactResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, seq, resp.Artifact.CurrentSeq)
	assert.Nil(t, resp.ReviewDelivery, "running agent must omit reviewDelivery")

	require.Eventually(t, func() bool {
		return len(f.disp.getMessageCalls()) == 1
	}, 5*time.Second, 20*time.Millisecond)
	assert.Empty(t, f.disp.getStartCalls(), "running agent must not be woken")
}

// TestArtifactReviewWake_OldShapeRequestBackwardCompatible verifies that an
// old-shape finalize request (`{"base":1}` without offer_wake or wake) stays
// backward compatible: system-suspended agents auto-wake, user-suspended agents
// save without waking or returning 409, and running agents are unchanged.
func TestArtifactReviewWake_OldShapeRequestBackwardCompatible(t *testing.T) {
	t.Run("user-suspended saves without 409 or wake", func(t *testing.T) {
		f := newArtifactReviewWakeFixture(t, true)
		f.suspendOwner(t, state.SuspendReasonUser)

		seq := f.stageReview(t, f.adminUser)
		url := fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/finalize", f.artifactID, seq)
		rec := userArtifactRequest(t, f.srv, f.adminUser, http.MethodPost, url, []byte(`{"base":1}`))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp artifacts.ArtifactResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, seq, resp.Artifact.CurrentSeq)
		require.NotNil(t, resp.ReviewDelivery)
		assert.Equal(t, artifacts.ReviewDeliverySuspended, resp.ReviewDelivery.Status)
		assert.Empty(t, f.disp.getStartCalls())
	})

	t.Run("system-suspended auto-wakes", func(t *testing.T) {
		f := newArtifactReviewWakeFixture(t, true)
		f.suspendOwner(t, state.SuspendReasonIdle)
		f.readyOnStart(t)

		seq := f.stageReview(t, f.adminUser)
		url := fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/finalize", f.artifactID, seq)
		rec := userArtifactRequest(t, f.srv, f.adminUser, http.MethodPost, url, []byte(`{"base":1}`))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		require.Eventually(t, func() bool {
			return len(f.disp.getStartCalls()) == 1 && len(f.disp.getMessageCalls()) == 1
		}, 5*time.Second, 20*time.Millisecond)
	})
}

// TestArtifactReviewWake_RetryPendingVersionNoDoubleCommitOrDoubleNotify
// verifies that retrying finalize on the same pending version after commit
// returns 409 conflict and never double-wakes or double-notifies.
func TestArtifactReviewWake_RetryPendingVersionNoDoubleCommitOrDoubleNotify(t *testing.T) {
	f := newArtifactReviewWakeFixture(t, true)
	f.suspendOwner(t, state.SuspendReasonUser)
	f.readyOnStart(t)

	seq := f.stageReview(t, f.adminUser)

	// Offer -> 409 (no commit, no wake).
	offerRec := f.finalizeReview(t, f.adminUser, seq, artifacts.FinalizeRequest{
		Base:      f.currentSeq,
		OfferWake: true,
	})
	require.Equal(t, http.StatusConflict, offerRec.Code)

	// Confirm -> 200 (commits once, wakes once, notifies once).
	finRec := f.finalizeReview(t, f.adminUser, seq, artifacts.FinalizeRequest{
		Base: f.currentSeq,
		Wake: true,
	})
	require.Equal(t, http.StatusOK, finRec.Code, finRec.Body.String())
	require.Len(t, f.disp.getStartCalls(), 1)
	require.Len(t, f.disp.getMessageCalls(), 1)

	// Duplicate retry on the same seq -> 409 conflict, no extra wake or notice.
	retryRec := f.finalizeReview(t, f.adminUser, seq, artifacts.FinalizeRequest{
		Base: f.currentSeq,
		Wake: true,
	})
	require.Equal(t, http.StatusConflict, retryRec.Code, retryRec.Body.String())
	assert.Len(t, f.disp.getStartCalls(), 1, "retry must not wake again")
	assert.Len(t, f.disp.getMessageCalls(), 1, "retry must not notify again")
}
