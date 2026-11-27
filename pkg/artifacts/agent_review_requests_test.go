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

package artifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type agentReviewRequestTestHost struct {
	*reviewRequestTestHost
	delegators       map[string]string
	delegatorErr     error
	onDelegatingUser func()
}

var _ SponsorRecorder = (*agentReviewRequestTestHost)(nil)

func (h *agentReviewRequestTestHost) DelegatingUser(_ context.Context, agentID string) (string, error) {
	if h.onDelegatingUser != nil {
		h.onDelegatingUser()
	}
	if h.delegatorErr != nil {
		return "", h.delegatorErr
	}
	return h.delegators[agentID], nil
}

func doAgentJSON(svc *Service, method, target, kind, ref, homeScope string, body any) *httptest.ResponseRecorder {
	var r *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, r)
	if kind != "" {
		req = withPrincipal(req, principal{kind: kind, ref: ref, home: homeScope})
	}
	rec := httptest.NewRecorder()
	svc.ServeHTTP(rec, req)
	return rec
}

func newAgentReviewFixture(t *testing.T) (*Service, *agentReviewRequestTestHost, Store, string, string) {
	t.Helper()
	f := newFixture(t, false)
	st := f.store
	blobs := f.blobs
	fh := newFakeHost()
	base := &reviewRequestTestHost{
		fakeHost:     fh,
		gateOn:       true,
		inboxHintsOn: true,
		resolveMap: map[string]RecipientSubject{
			"alice": &staticRecipientSubject{scopes: map[string]bool{"proj-1": true}},
			"bob":   &staticRecipientSubject{scopes: map[string]bool{"proj-1": true}},
			"dave":  &staticRecipientSubject{scopes: map[string]bool{"proj-2": true}},
		},
		names: map[string]string{
			"user:alice":    "Alice",
			"user:bob":      "Bob",
			"user:dave":     "Dave",
			"agent:agent-1": "Agent One",
		},
	}
	h := &agentReviewRequestTestHost{
		reviewRequestTestHost: base,
		delegators:            map[string]string{"agent-1": "alice"},
	}
	svc := NewService(h)
	svc.SetStore(st)
	svc.SetBlobStorage(blobs, "hub-test")

	pAlice := principal{PrincipalKindUser, "alice", ""}
	pBob := principal{PrincipalKindUser, "bob", ""}
	pDave := principal{PrincipalKindUser, "dave", ""}
	pAgent1 := principal{PrincipalKindAgent, "agent-1", "proj-1"}
	pAgent2 := principal{PrincipalKindAgent, "agent-2", "proj-1"}

	fh.allow(pAlice, "proj-1", PermissionRead, PermissionCreate, PermissionManage, PermissionRequestReview)
	fh.allow(pBob, "proj-1", PermissionRead, PermissionCreate, PermissionRequestReview)
	fh.allow(pDave, "proj-2", PermissionRead, PermissionCreate, PermissionRequestReview)
	fh.allow(pAgent1, "proj-1", PermissionRead, PermissionCreate, PermissionRequestReview)
	fh.allow(pAgent2, "proj-1", PermissionRead, PermissionCreate, PermissionRequestReview)

	// Publish an artifact owned by agent-1 in proj-1.
	pubReq := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts?name=plan.md&title=Agent+Plan&scope=proj-1", strings.NewReader("# Agent Plan\n"))
	pubReq = withPrincipal(pubReq, pAgent1)
	pubRec := httptest.NewRecorder()
	svc.ServeHTTP(pubRec, pubReq)
	if pubRec.Code != http.StatusCreated {
		t.Fatalf("agent publish: %d %s", pubRec.Code, pubRec.Body.String())
	}
	var pubResp ArtifactResponse
	if err := json.Unmarshal(pubRec.Body.Bytes(), &pubResp); err != nil {
		t.Fatalf("decode publish: %v", err)
	}
	agentArtID := pubResp.Artifact.ID

	// Publish an artifact owned by Alice in proj-1.
	userPubReq := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts?name=user.md&title=User+Doc&scope=proj-1", strings.NewReader("# User Doc\n"))
	userPubReq = withPrincipal(userPubReq, pAlice)
	userPubRec := httptest.NewRecorder()
	svc.ServeHTTP(userPubRec, userPubReq)
	if userPubRec.Code != http.StatusCreated {
		t.Fatalf("user publish: %d %s", userPubRec.Code, userPubRec.Body.String())
	}
	var userPubResp ArtifactResponse
	if err := json.Unmarshal(userPubRec.Body.Bytes(), &userPubResp); err != nil {
		t.Fatalf("decode user publish: %v", err)
	}
	return svc, h, st, agentArtID, userPubResp.Artifact.ID
}

// TestAgentReviewRequestsMatrix covers the Phase 4 agent requester matrix
// (design §4.3, §13 item 2):
//   - non-owning agent -> 403;
//   - owning agent whose reviewer lacks a home role -> reviewer_cannot_review;
//   - owning agent whose reviewer holds a home role -> bounded grant;
//   - --delegator resolution (agent-only);
//   - after a move -> 403;
//   - on a store fault -> 503.
func TestAgentReviewRequestsMatrix(t *testing.T) {
	svc, h, st, agentArtID, userArtID := newAgentReviewFixture(t)
	ctx := context.Background()

	// 1. Non-owning agent (agent-2 on agent-1's artifact, and agent-1 on Alice's artifact) -> 403.
	rec := doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+agentArtID+"/review-requests",
		PrincipalKindAgent, "agent-2", "proj-1", CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owning agent on agent artifact = %d %s, want 403", rec.Code, rec.Body.String())
	}
	rec = doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+userArtID+"/review-requests",
		PrincipalKindAgent, "agent-1", "proj-1", CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owning agent on user artifact = %d %s, want 403", rec.Code, rec.Body.String())
	}

	// 2. Owning agent requesting reviewer without home role (Dave in proj-2) -> reviewer_cannot_review,
	//    and reviewer with home role (Bob in proj-1) -> bounded.
	//    Note: nonInteractive=true proves agents do NOT go through the user interactive-session gate.
	h.nonInteractive = true
	rec = doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+agentArtID+"/review-requests",
		PrincipalKindAgent, "agent-1", "proj-1", CreateReviewRequestsBody{
			Reviewers: []string{"user:dave", "user:bob"},
			Note:      "Please review",
		})
	h.nonInteractive = false
	if rec.Code != http.StatusOK {
		t.Fatalf("owning agent request = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var resp CreateReviewRequestsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Requests) != 2 {
		t.Fatalf("got %d results, want 2", len(resp.Requests))
	}
	if resp.Requests[0].Outcome != ReviewRequestOutcomeRefused || resp.Requests[0].Code != CodeReviewerCannotReview {
		t.Errorf("dave result = %+v, want refused(%s)", resp.Requests[0], CodeReviewerCannotReview)
	}
	if resp.Requests[1].Outcome != ReviewRequestOutcomeCreated || resp.Requests[1].Access != ReviewRequestAccessBounded || !resp.Requests[1].Notified {
		t.Errorf("bob result = %+v, want created(bounded, notified)", resp.Requests[1])
	}

	// Verify the bounded grant has CreatedByRef == BoundedReviewCreator.
	gs, err := st.ListGrants(ctx, agentArtID)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	var foundBounded bool
	for _, g := range gs {
		if g.SubjectKind == SubjectPrincipal && g.SubjectRef == "user:bob" && g.Permission == GrantReview {
			if g.CreatedByRef != BoundedReviewCreator {
				t.Errorf("bob grant CreatedByRef = %q, want %q", g.CreatedByRef, BoundedReviewCreator)
			}
			foundBounded = true
		}
	}
	if !foundBounded {
		t.Fatalf("expected bounded review grant for user:bob, grants=%+v", gs)
	}

	// 3. --delegator resolves agent-1's delegator (alice) -> bounded; non-agent using delegator -> 400.
	rec = doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+agentArtID+"/review-requests",
		PrincipalKindAgent, "agent-1", "proj-1", CreateReviewRequestsBody{Delegator: true, Note: "For sponsor"})
	if rec.Code != http.StatusOK {
		t.Fatalf("delegator request = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var delResp CreateReviewRequestsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &delResp); err != nil {
		t.Fatalf("decode delegator: %v", err)
	}
	if len(delResp.Requests) != 1 || delResp.Requests[0].Reviewer != "user:alice" || delResp.Requests[0].Access != ReviewRequestAccessBounded {
		t.Errorf("delegator result = %+v, want user:alice bounded", delResp.Requests)
	}
	rec = doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+userArtID+"/review-requests",
		PrincipalKindUser, "alice", "", CreateReviewRequestsBody{Delegator: true})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("user with delegator=true = %d %s, want 400", rec.Code, rec.Body.String())
	}

	// 4. After a move to proj-2 -> 403 for the owning agent.
	h.allow(principal{PrincipalKindUser, "alice", ""}, "proj-2", PermissionRead, PermissionCreate, PermissionManage)
	_, err = st.UpdateArtifact(ctx, agentArtID, ArtifactUpdate{
		HomeGrant: &Grant{
			ID: "move-home-grant", ArtifactID: agentArtID, SubjectKind: SubjectScope,
			SubjectRef: "proj-2", Permission: GrantRead, CreatedAt: time.Now().UTC(),
		},
		MaxGrants: MaxGrantsPerArtifact,
		Actor:     &Actor{Kind: PrincipalKindUser, Ref: "alice", Authority: AuthorityOwner},
	})
	if err != nil {
		t.Fatalf("move artifact: %v", err)
	}
	rec = doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+agentArtID+"/review-requests",
		PrincipalKindAgent, "agent-1", "proj-1", CreateReviewRequestsBody{Reviewers: []string{"user:dave"}})
	if rec.Code != http.StatusForbidden {
		t.Errorf("agent request after move = %d %s, want 403", rec.Code, rec.Body.String())
	}

	// 5. Store fault during permission check -> 503.
	faulty := &listGrantsFaultStore{Store: st, err: errors.New("db down")}
	svcFault := NewService(h)
	svcFault.SetStore(faulty)
	svcFault.SetBlobStorage(newFixture(t, false).blobs, "hub-test")
	// User owner requesting on userArtID triggers canWriteErr -> ListGrants fault -> 503.
	// Use a non-owner publisher (Bob with write grant) or wait: owner short-circuits canWriteWith
	// before ListGrants, so check with an agent that isn't owner? No, owner short-circuits canReadWith
	// and canWriteWith; a user with home read (not owner) hits ListGrants in canWriteErr -> 503!
	rec = doAgentJSON(svcFault, http.MethodPost, "/api/v1/artifacts/"+userArtID+"/review-requests",
		PrincipalKindUser, "bob", "", CreateReviewRequestsBody{Reviewers: []string{"user:alice"}})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("store fault on canWriteErr = %d %s, want 503", rec.Code, rec.Body.String())
	}
}

type listGrantsFaultStore struct {
	Store
	err error
}

func (f *listGrantsFaultStore) ListGrants(_ context.Context, _ string) ([]Grant, error) {
	return nil, f.err
}

// TestBoundedGrantNeverConfersRead verifies acceptance §13 item 2:
// a bounded grant never confers read on its own across:
//   - canReadWith without a home role;
//   - ListCandidates (readGrantFilter SQL pushdown);
//   - ResolveLink / LinkActive;
//   - canWriteWith without a home role.
func TestBoundedGrantNeverConfersRead(t *testing.T) {
	svc, h, st, agentArtID, _ := newAgentReviewFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Agent-1 creates a bounded grant for Bob (who currently holds a proj-1 role).
	rec := doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+agentArtID+"/review-requests",
		PrincipalKindAgent, "agent-1", "proj-1", CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("create bounded request: %d %s", rec.Code, rec.Body.String())
	}

	a, err := st.GetArtifact(ctx, agentArtID)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	gs, err := st.ListGrants(ctx, agentArtID)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}

	// With proj-1 role, Bob can read and review.
	bobCtx := withPrincipal(httptest.NewRequest(http.MethodGet, "/", nil), principal{PrincipalKindUser, "bob", ""}).Context()
	if !canReadWithAt(bobCtx, h, a, now, func() ([]Grant, error) { return gs, nil }) {
		t.Fatalf("bob with proj-1 role should read")
	}
	wa, err := canWriteWith(bobCtx, h, a, now, true, func() ([]Grant, error) { return gs, nil })
	if err != nil || wa != accessReviewGrant {
		t.Fatalf("bob with proj-1 role canWriteWith = %v, %v; want accessReviewGrant", wa, err)
	}

	// Remove Bob's proj-1 role (Authorize returns false for PermissionRead in proj-1,
	// while Permits remains true).
	delete(h.perms[PrincipalRef(PrincipalKindUser, "bob")]["proj-1"], PermissionRead)

	// 1. canReadWith without a role -> false.
	if canReadWithAt(bobCtx, h, a, now, func() ([]Grant, error) { return gs, nil }) {
		t.Errorf("bounded grant must NOT confer read without a home-project role")
	}

	// 2. canWriteWith without a role -> accessNone.
	wa, err = canWriteWith(bobCtx, h, a, now, true, func() ([]Grant, error) { return gs, nil })
	if err != nil || wa != accessNone {
		t.Errorf("canWriteWith without home role = %v, %v; want accessNone", wa, err)
	}

	// 3. ListCandidates (readGrantFilter pushdown) without proj-1 in ScopeRefs -> empty.
	cands, err := st.ListCandidates(ctx, CandidateQuery{
		PrincipalKind: PrincipalKindUser,
		PrincipalRef:  "bob",
		ScopeRefs:     nil,
		Now:           now,
		Limit:         10,
	})
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("ListCandidates returned %d rows for bounded-only grantee, want 0", len(cands))
	}

	// 4. Share-link resolution excludes role-gated created_by_ref rows.
	sqlSt := st.(*sqlStore)
	exp := now.Add(time.Hour)
	for _, creator := range []string{SponsorReviewCreator, BoundedReviewCreator} {
		hash := "hash-" + creator
		_, err := sqlSt.db.ExecContext(ctx, sqlSt.rebind(`INSERT INTO artifact_grant
			(id, artifact_id, subject_kind, subject_ref, permission, expires_at, created_by_ref, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			"link-"+creator, agentArtID, SubjectLink, hash, GrantRead, sqlSt.timeArg(exp), creator, sqlSt.timeArg(now))
		if err != nil {
			t.Fatalf("insert synthetic link (%s): %v", creator, err)
		}
		if _, _, err := st.ResolveLink(ctx, hash, now); !errors.Is(err, ErrNotFound) {
			t.Errorf("ResolveLink(%s) = %v, want ErrNotFound", creator, err)
		}
		active, err := st.LinkActive(ctx, agentArtID, "link-"+creator, now)
		if err != nil || active {
			t.Errorf("LinkActive(%s) = %v, %v; want false, nil", creator, active, err)
		}
	}
}

// TestAgentBoundedGrantCapAndRateLimit verifies:
//   - at most 10 bounded grants per artifact (11th returns CodeTooManyGrants);
//   - agent rate limit is 10/h per requester (11th request returns 429).
func TestAgentBoundedGrantCapAndRateLimit(t *testing.T) {
	svc, h, _, agentArtID, _ := newAgentReviewFixture(t)

	// Add 11 reviewers with proj-1 read roles.
	reviewers := make([]string, 0, 11)
	for i := 1; i <= 11; i++ {
		uid := fmt.Sprintf("u-%02d", i)
		h.resolveMap[uid] = &staticRecipientSubject{scopes: map[string]bool{"proj-1": true}}
		reviewers = append(reviewers, "user:"+uid)
	}

	// First batch of 10 fills the bounded-grant cap.
	rec := doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+agentArtID+"/review-requests",
		PrincipalKindAgent, "agent-1", "proj-1", CreateReviewRequestsBody{Reviewers: reviewers[:10]})
	if rec.Code != http.StatusOK {
		t.Fatalf("batch 1..10 = %d %s", rec.Code, rec.Body.String())
	}
	var resp1 CreateReviewRequestsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp1)
	for i, r := range resp1.Requests {
		if r.Outcome != ReviewRequestOutcomeCreated || r.Access != ReviewRequestAccessBounded {
			t.Fatalf("reviewer[%d] = %+v, want created(bounded)", i, r)
		}
	}

	// 11th bounded grant is refused with too_many_grants.
	rec = doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+agentArtID+"/review-requests",
		PrincipalKindAgent, "agent-1", "proj-1", CreateReviewRequestsBody{Reviewers: reviewers[10:]})
	if rec.Code != http.StatusOK {
		t.Fatalf("11th reviewer = %d %s", rec.Code, rec.Body.String())
	}
	var resp2 CreateReviewRequestsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp2)
	if len(resp2.Requests) != 1 || resp2.Requests[0].Outcome != ReviewRequestOutcomeRefused || resp2.Requests[0].Code != CodeTooManyGrants {
		t.Errorf("11th bounded grant = %+v, want refused(%s)", resp2.Requests, CodeTooManyGrants)
	}

	// We already sent 2 requests as agent-1; send 8 more (total 10), then the 11th hits 429.
	for i := 3; i <= 10; i++ {
		rec = doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+agentArtID+"/review-requests",
			PrincipalKindAgent, "agent-1", "proj-1", CreateReviewRequestsBody{Reviewers: []string{"user:u-01"}})
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d %s, want 200", i, rec.Code, rec.Body.String())
		}
	}
	rec = doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+agentArtID+"/review-requests",
		PrincipalKindAgent, "agent-1", "proj-1", CreateReviewRequestsBody{Reviewers: []string{"user:u-01"}})
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("11th agent request in 1h = %d %s, want 429", rec.Code, rec.Body.String())
	}
}

// TestWithdrawBoundedGrantCleanup verifies design §4.4 / §4.6 writer 3:
// withdrawing a bounded request deletes the bounded grant only while it is
// still role-gated; if a human took ownership of the grant, withdraw leaves it.
func TestWithdrawBoundedGrantCleanup(t *testing.T) {
	svc, _, st, agentArtID, _ := newAgentReviewFixture(t)
	ctx := context.Background()

	// 1. Agent creates bounded request for Bob, then withdraws it -> bounded grant deleted.
	rec := doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+agentArtID+"/review-requests",
		PrincipalKindAgent, "agent-1", "proj-1", CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var createResp CreateReviewRequestsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &createResp)
	reqID := createResp.Requests[0].Request.ID

	wRec := doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+agentArtID+"/review-requests/"+reqID+"/withdraw",
		PrincipalKindAgent, "agent-1", "proj-1", nil)
	if wRec.Code != http.StatusOK {
		t.Fatalf("withdraw: %d %s", wRec.Code, wRec.Body.String())
	}
	gs, err := st.ListGrants(ctx, agentArtID)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	for _, g := range gs {
		if g.SubjectKind == SubjectPrincipal && g.SubjectRef == "user:bob" {
			t.Errorf("expected bounded grant for bob to be deleted on withdraw, still found %+v", g)
		}
	}

	// 2. Agent creates another bounded request for Bob; human (Alice) edits the
	//    grant (taking ownership, changing created_by_ref to user:alice); agent
	//    withdraws -> grant MUST remain in place.
	rec = doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+agentArtID+"/review-requests",
		PrincipalKindAgent, "agent-1", "proj-1", CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("create 2: %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &createResp)
	reqID2 := createResp.Requests[0].Request.ID

	// Human admin takes ownership of the bounded grant by changing its level to write.
	_, err = st.PutGrant(ctx, &Grant{
		ID: "takeover", ArtifactID: agentArtID, SubjectKind: SubjectPrincipal,
		SubjectRef: "user:bob", Permission: GrantWrite, CreatedByRef: "user:alice", CreatedAt: time.Now().UTC(),
	}, MaxGrantsPerArtifact, false, &Actor{Kind: PrincipalKindUser, Ref: "alice", Authority: AuthorityOwner})
	if err != nil {
		t.Fatalf("human PutGrant takeover: %v", err)
	}

	wRec = doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+agentArtID+"/review-requests/"+reqID2+"/withdraw",
		PrincipalKindAgent, "agent-1", "proj-1", nil)
	if wRec.Code != http.StatusOK {
		t.Fatalf("withdraw 2: %d %s", wRec.Code, wRec.Body.String())
	}
	gs, err = st.ListGrants(ctx, agentArtID)
	if err != nil {
		t.Fatalf("ListGrants 2: %v", err)
	}
	var stillPresent bool
	for _, g := range gs {
		if g.SubjectKind == SubjectPrincipal && g.SubjectRef == "user:bob" && g.CreatedByRef == "user:alice" {
			stillPresent = true
		}
	}
	if !stillPresent {
		t.Errorf("expected human-owned grant to survive withdraw, grants=%+v", gs)
	}
}

// TestRaiseOverRoleGatedOrExpiredGrant verifies carried-over finding #8:
// raising over a sponsor-form review grant whose reviewer lost their home role
// (or over an expired grant) rewrites created_by_ref to the requester and clears
// expired expires_at so principalCanReview is true afterwards.
func TestRaiseOverRoleGatedOrExpiredGrant(t *testing.T) {
	svc, h, st, _, userArtID := newAgentReviewFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	sqlSt := st.(*sqlStore)

	// 1. Insert a sponsor-form review grant for Dave, who has NO role in proj-1.
	_, err := sqlSt.db.ExecContext(ctx, sqlSt.rebind(`INSERT INTO artifact_grant
		(id, artifact_id, subject_kind, subject_ref, permission, expires_at, created_by_ref, created_at)
		VALUES (?, ?, ?, ?, ?, NULL, ?, ?)`),
		"sponsor-dave", userArtID, SubjectPrincipal, "user:dave", GrantReview, SponsorReviewCreator, sqlSt.timeArg(now))
	if err != nil {
		t.Fatalf("insert sponsor grant: %v", err)
	}
	a, err := st.GetArtifact(ctx, userArtID)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	daveSubj := h.resolveMap["dave"]
	canBefore, err := principalCanReview(ctx, daveSubj, a, PrincipalKindUser, "dave", now, func() ([]Grant, error) {
		return st.ListGrants(ctx, userArtID)
	})
	if err != nil || canBefore {
		t.Fatalf("before raise: principalCanReview(dave) = %v, %v; want false, nil", canBefore, err)
	}

	// User admin (Alice) raises Dave via POST /review-requests.
	h.visibleUsers = map[string]bool{"dave": true, "bob": true}
	rec := doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+userArtID+"/review-requests",
		PrincipalKindUser, "alice", "", CreateReviewRequestsBody{Reviewers: []string{"user:dave"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("raise over sponsor grant: %d %s", rec.Code, rec.Body.String())
	}
	var resp CreateReviewRequestsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Requests) != 1 || resp.Requests[0].Outcome != ReviewRequestOutcomeCreated || resp.Requests[0].Access != ReviewRequestAccessRaised {
		t.Fatalf("raise over sponsor grant result = %+v, want created(raised)", resp.Requests)
	}

	// Assert created_by_ref was rewritten to user:alice and principalCanReview is now true!
	gs, err := st.ListGrants(ctx, userArtID)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	var daveGrant *Grant
	for i := range gs {
		if gs[i].SubjectKind == SubjectPrincipal && gs[i].SubjectRef == "user:dave" {
			daveGrant = &gs[i]
		}
	}
	if daveGrant == nil || daveGrant.CreatedByRef != "user:alice" {
		t.Fatalf("dave grant after raise = %+v, want CreatedByRef=user:alice", daveGrant)
	}
	canAfter, err := principalCanReview(ctx, daveSubj, a, PrincipalKindUser, "dave", now, func() ([]Grant, error) {
		return st.ListGrants(ctx, userArtID)
	})
	if err != nil || !canAfter {
		t.Fatalf("after raise: principalCanReview(dave) = %v, %v; want true, nil", canAfter, err)
	}

	// 2. Insert an expired review grant for Bob, then raise via Alice -> expires_at cleared, principalCanReview true.
	past := now.Add(-time.Hour)
	_, err = sqlSt.db.ExecContext(ctx, sqlSt.rebind(`INSERT INTO artifact_grant
		(id, artifact_id, subject_kind, subject_ref, permission, expires_at, created_by_ref, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		"expired-bob", userArtID, SubjectPrincipal, "user:bob", GrantReview, sqlSt.timeArg(past), "user:alice", sqlSt.timeArg(past))
	if err != nil {
		t.Fatalf("insert expired grant: %v", err)
	}
	rec = doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+userArtID+"/review-requests",
		PrincipalKindUser, "alice", "", CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("raise over expired grant: %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Requests) != 1 || resp.Requests[0].Outcome != ReviewRequestOutcomeCreated || resp.Requests[0].Access != ReviewRequestAccessRaised {
		t.Fatalf("raise over expired grant result = %+v, want created(raised)", resp.Requests)
	}
	canBob, err := principalCanReview(ctx, h.resolveMap["bob"], a, PrincipalKindUser, "bob", now, func() ([]Grant, error) {
		return st.ListGrants(ctx, userArtID)
	})
	if err != nil || !canBob {
		t.Fatalf("after raise over expired grant: principalCanReview(bob) = %v, %v; want true, nil", canBob, err)
	}
}

// TestAgentReviewRequests_BoundedConflictExpiredAndNonOwnerWriteGrant covers R2
// (review 1 mutations m8, m12, and m6):
//  1. Unexpired human read grant + home role -> refused reviewer_cannot_review,
//     grant row unchanged (catches m8).
//  2. Expired ordinary grant + home role -> bounded, grant row rewritten to
//     review / system:review_request / expires_at NULL, principalCanReview true,
//     and the rewrite counts against the cap of 10 (catches m12).
//  3. Agent holding a write grant on another principal's artifact -> 403, and
//     no review request row is created (catches m6).
func TestAgentReviewRequests_BoundedConflictExpiredAndNonOwnerWriteGrant(t *testing.T) {
	t.Run("unexpired human read grant is never overwritten by bounded request", func(t *testing.T) {
		svc, _, st, agentArtID, _ := newAgentReviewFixture(t)
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Microsecond)
		futureExp := now.Add(24 * time.Hour)
		sqlSt := st.(*sqlStore)

		_, err := sqlSt.db.ExecContext(ctx, sqlSt.rebind(`INSERT INTO artifact_grant
			(id, artifact_id, subject_kind, subject_ref, permission, expires_at, created_by_ref, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			"human-read-bob", agentArtID, SubjectPrincipal, "user:bob", GrantRead, sqlSt.timeArg(futureExp), "user:alice", sqlSt.timeArg(now))
		if err != nil {
			t.Fatalf("insert human read grant: %v", err)
		}

		// Bob has a proj-1 home role AND an unexpired human read grant.
		rec := doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+agentArtID+"/review-requests",
			PrincipalKindAgent, "agent-1", "proj-1", CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
		if rec.Code != http.StatusOK {
			t.Fatalf("bounded over unexpired read grant = %d %s, want 200", rec.Code, rec.Body.String())
		}
		var resp CreateReviewRequestsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Requests) != 1 || resp.Requests[0].Outcome != ReviewRequestOutcomeRefused || resp.Requests[0].Code != CodeReviewerCannotReview {
			t.Fatalf("result = %+v, want refused(%s)", resp.Requests, CodeReviewerCannotReview)
		}

		// Verify the human grant row is completely unchanged.
		gs, err := st.ListGrants(ctx, agentArtID)
		if err != nil {
			t.Fatalf("ListGrants: %v", err)
		}
		var bobGrant *Grant
		for i := range gs {
			if gs[i].SubjectKind == SubjectPrincipal && gs[i].SubjectRef == "user:bob" {
				bobGrant = &gs[i]
			}
		}
		if bobGrant == nil {
			t.Fatalf("expected bob grant to remain, grants=%+v", gs)
		}
		if bobGrant.Permission != GrantRead || bobGrant.CreatedByRef != "user:alice" || bobGrant.ExpiresAt == nil || !bobGrant.ExpiresAt.Equal(futureExp) {
			t.Errorf("human grant mutated: got %+v, want permission=%s created_by_ref=user:alice expires_at=%v",
				bobGrant, GrantRead, futureExp)
		}
	})

	t.Run("expired ordinary grant rewritten to live bounded grant and counted against cap", func(t *testing.T) {
		svc, h, st, agentArtID, _ := newAgentReviewFixture(t)
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Microsecond)
		pastExp := now.Add(-time.Hour)
		sqlSt := st.(*sqlStore)

		_, err := sqlSt.db.ExecContext(ctx, sqlSt.rebind(`INSERT INTO artifact_grant
			(id, artifact_id, subject_kind, subject_ref, permission, expires_at, created_by_ref, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			"expired-read-bob", agentArtID, SubjectPrincipal, "user:bob", GrantRead, sqlSt.timeArg(pastExp), "user:alice", sqlSt.timeArg(pastExp))
		if err != nil {
			t.Fatalf("insert expired read grant: %v", err)
		}

		rec := doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+agentArtID+"/review-requests",
			PrincipalKindAgent, "agent-1", "proj-1", CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
		if rec.Code != http.StatusOK {
			t.Fatalf("bounded over expired read grant = %d %s, want 200", rec.Code, rec.Body.String())
		}
		var resp CreateReviewRequestsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Requests) != 1 || resp.Requests[0].Outcome != ReviewRequestOutcomeCreated || resp.Requests[0].Access != ReviewRequestAccessBounded {
			t.Fatalf("result = %+v, want created(bounded)", resp.Requests)
		}

		// Verify the grant row was rewritten to review / BoundedReviewCreator / expires_at NULL.
		gs, err := st.ListGrants(ctx, agentArtID)
		if err != nil {
			t.Fatalf("ListGrants: %v", err)
		}
		var bobGrant *Grant
		for i := range gs {
			if gs[i].SubjectKind == SubjectPrincipal && gs[i].SubjectRef == "user:bob" {
				bobGrant = &gs[i]
			}
		}
		if bobGrant == nil || bobGrant.Permission != GrantReview || bobGrant.CreatedByRef != BoundedReviewCreator || bobGrant.ExpiresAt != nil {
			t.Fatalf("rewritten grant = %+v, want permission=review created_by_ref=%s expires_at=nil", bobGrant, BoundedReviewCreator)
		}

		// principalCanReview must now be true for Bob.
		a, err := st.GetArtifact(ctx, agentArtID)
		if err != nil {
			t.Fatalf("GetArtifact: %v", err)
		}
		canBob, err := principalCanReview(ctx, h.resolveMap["bob"], a, PrincipalKindUser, "bob", now, func() ([]Grant, error) {
			return st.ListGrants(ctx, agentArtID)
		})
		if err != nil || !canBob {
			t.Fatalf("principalCanReview(bob) after rewrite = %v, %v; want true, nil", canBob, err)
		}

		// Fill the remaining 9 bounded grant slots (total 10 including Bob's rewrite).
		extra := make([]string, 0, 10)
		for i := 1; i <= 10; i++ {
			uid := fmt.Sprintf("cap-u-%02d", i)
			h.resolveMap[uid] = &staticRecipientSubject{scopes: map[string]bool{"proj-1": true}}
			extra = append(extra, "user:"+uid)
		}
		rec = doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+agentArtID+"/review-requests",
			PrincipalKindAgent, "agent-1", "proj-1", CreateReviewRequestsBody{Reviewers: extra[:9]})
		if rec.Code != http.StatusOK {
			t.Fatalf("fill remaining 9 slots = %d %s", rec.Code, rec.Body.String())
		}
		var fillResp CreateReviewRequestsResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &fillResp)
		for i, r := range fillResp.Requests {
			if r.Outcome != ReviewRequestOutcomeCreated || r.Access != ReviewRequestAccessBounded {
				t.Fatalf("slot[%d] = %+v, want created(bounded)", i, r)
			}
		}

		// 11th bounded grant (even over an expired ordinary grant) must hit too_many_grants.
		_, err = sqlSt.db.ExecContext(ctx, sqlSt.rebind(`INSERT INTO artifact_grant
			(id, artifact_id, subject_kind, subject_ref, permission, expires_at, created_by_ref, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			"expired-read-11", agentArtID, SubjectPrincipal, extra[9], GrantRead, sqlSt.timeArg(pastExp), "user:alice", sqlSt.timeArg(pastExp))
		if err != nil {
			t.Fatalf("insert 11th expired grant: %v", err)
		}
		rec = doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+agentArtID+"/review-requests",
			PrincipalKindAgent, "agent-1", "proj-1", CreateReviewRequestsBody{Reviewers: []string{extra[9]}})
		if rec.Code != http.StatusOK {
			t.Fatalf("11th bounded request = %d %s", rec.Code, rec.Body.String())
		}
		var capResp CreateReviewRequestsResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &capResp)
		if len(capResp.Requests) != 1 || capResp.Requests[0].Outcome != ReviewRequestOutcomeRefused || capResp.Requests[0].Code != CodeTooManyGrants {
			t.Errorf("11th bounded rewrite = %+v, want refused(%s)", capResp.Requests, CodeTooManyGrants)
		}
	})

	t.Run("agent with write grant on another principal's artifact is refused 403", func(t *testing.T) {
		svc, _, st, _, userArtID := newAgentReviewFixture(t)
		ctx := context.Background()
		now := time.Now().UTC()

		// Give Bob review access (so access=existing would succeed) and agent-1 a
		// write grant (so agent-1 passes canPublish!) on Alice's artifact.
		_, err := st.PutGrant(ctx, &Grant{
			ID: "bob-review", ArtifactID: userArtID, SubjectKind: SubjectPrincipal,
			SubjectRef: "user:bob", Permission: GrantReview, CreatedByRef: "user:alice", CreatedAt: now,
		}, MaxGrantsPerArtifact, false, &Actor{Kind: PrincipalKindUser, Ref: "alice", Authority: AuthorityOwner})
		if err != nil {
			t.Fatalf("grant bob review: %v", err)
		}
		_, err = st.PutGrant(ctx, &Grant{
			ID: "agent-write", ArtifactID: userArtID, SubjectKind: SubjectPrincipal,
			SubjectRef: "agent:agent-1", Permission: GrantWrite, CreatedByRef: "user:alice", CreatedAt: now,
		}, MaxGrantsPerArtifact, false, &Actor{Kind: PrincipalKindUser, Ref: "alice", Authority: AuthorityOwner})
		if err != nil {
			t.Fatalf("grant agent-1 write: %v", err)
		}

		// agent-1 holds write access on Alice's artifact but does NOT own it -> 403.
		rec := doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+userArtID+"/review-requests",
			PrincipalKindAgent, "agent-1", "proj-1", CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("non-owner agent with write grant = %d %s, want 403", rec.Code, rec.Body.String())
		}

		// Verify no review request row was created.
		reqs, err := st.ListReviewRequests(ctx, userArtID, 10)
		if err != nil {
			t.Fatalf("ListReviewRequests: %v", err)
		}
		if len(reqs) != 0 {
			t.Errorf("expected 0 review request rows, got %+v", reqs)
		}
	})
}

// TestPlainPutGrantLevelChangePreservesExpiryWhileRaiseClearsIt verifies
// review round 2 finding 3: a plain PutGrant level change (the Share route,
// fromReviewRequest=false) over an expired grant preserves expires_at, while
// the review-request raise path (fromReviewRequest=true) clears expires_at.
func TestPlainPutGrantLevelChangePreservesExpiryWhileRaiseClearsIt(t *testing.T) {
	svc, h, st, _, userArtID := newAgentReviewFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	past := now.Add(-time.Hour)
	sqlSt := st.(*sqlStore)

	_, err := sqlSt.db.ExecContext(ctx, sqlSt.rebind(`INSERT INTO artifact_grant
		(id, artifact_id, subject_kind, subject_ref, permission, expires_at, created_by_ref, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		"expired-bob-plain", userArtID, SubjectPrincipal, "user:bob", GrantRead, sqlSt.timeArg(past), "user:alice", sqlSt.timeArg(past))
	if err != nil {
		t.Fatalf("insert expired grant: %v", err)
	}

	// Plain PutGrant level change (read -> write) must leave expires_at intact.
	put, err := st.PutGrant(ctx, &Grant{
		ID: "ignored-id", ArtifactID: userArtID, SubjectKind: SubjectPrincipal,
		SubjectRef: "user:bob", Permission: GrantWrite, CreatedByRef: "user:alice", CreatedAt: now,
	}, MaxGrantsPerArtifact, false, &Actor{Kind: PrincipalKindUser, Ref: "alice", Authority: AuthorityOwner})
	if err != nil || put != GrantPutChanged {
		t.Fatalf("plain PutGrant over expired = %v, %v; want GrantPutChanged, nil", put, err)
	}
	findBob := func() *Grant {
		gs, lerr := st.ListGrants(ctx, userArtID)
		if lerr != nil {
			t.Fatalf("ListGrants: %v", lerr)
		}
		for i := range gs {
			if gs[i].SubjectKind == SubjectPrincipal && gs[i].SubjectRef == "user:bob" {
				return &gs[i]
			}
		}
		return nil
	}
	afterPlain := findBob()
	if afterPlain == nil || afterPlain.Permission != GrantWrite || afterPlain.ExpiresAt == nil || !afterPlain.ExpiresAt.Equal(past) {
		t.Fatalf("after plain PutGrant: got %+v, want permission=write expires_at=%v", afterPlain, past)
	}

	// Review-request raise over the still-expired grant must clear expires_at.
	h.visibleUsers = map[string]bool{"bob": true}
	rec := doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+userArtID+"/review-requests",
		PrincipalKindUser, "alice", "", CreateReviewRequestsBody{Reviewers: []string{"user:bob"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("raise over expired grant: %d %s", rec.Code, rec.Body.String())
	}
	var resp CreateReviewRequestsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Requests) != 1 || resp.Requests[0].Outcome != ReviewRequestOutcomeCreated || resp.Requests[0].Access != ReviewRequestAccessRaised {
		t.Fatalf("raise result = %+v, want created(raised)", resp.Requests)
	}
	afterRaise := findBob()
	if afterRaise == nil || afterRaise.Permission != GrantReview || afterRaise.ExpiresAt != nil {
		t.Fatalf("after raise: got %+v, want permission=review expires_at=nil", afterRaise)
	}
}

// TestDelegatorMissingWithConcurrentMoveReturns403 verifies review round 2
// finding 5: when --delegator resolves to nothing and a concurrent move is
// detected on the first real reviewer via checkExpect, the route returns 403
// instead of a per-reviewer reviewer_cannot_review refusal.
func TestDelegatorMissingWithConcurrentMoveReturns403(t *testing.T) {
	svc, h, st, agentArtID, _ := newAgentReviewFixture(t)
	ctx := context.Background()
	sqlSt := st.(*sqlStore)

	h.delegators["agent-1"] = ""
	h.onDelegatingUser = func() {
		// Simulate the artifact moving to proj-2 after readableArtifact passed
		// the route-level home check and before CreateOrRemindReviewRequest runs.
		_, _ = sqlSt.db.ExecContext(ctx, sqlSt.rebind(`UPDATE artifact SET scope_ref = ? WHERE id = ?`), "proj-2", agentArtID)
	}

	rec := doAgentJSON(svc, http.MethodPost, "/api/v1/artifacts/"+agentArtID+"/review-requests",
		PrincipalKindAgent, "agent-1", "proj-1", CreateReviewRequestsBody{
			Delegator: true,
			Reviewers: []string{"user:bob"},
		})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("delegator missing + concurrent move = %d %s, want 403", rec.Code, rec.Body.String())
	}
	reqs, err := st.ListReviewRequests(ctx, agentArtID, 10)
	if err != nil {
		t.Fatalf("ListReviewRequests: %v", err)
	}
	if len(reqs) != 0 {
		t.Errorf("expected 0 review requests after 403, got %+v", reqs)
	}
}
