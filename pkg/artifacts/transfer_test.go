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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type transferTestHost struct {
	*fakeHost
	featureOn      bool
	sessionAllowed map[string]bool // principal ref -> session allowed
	ownerLive      map[string]bool
	ownerLiveErr   error
	controls       map[string]map[string]bool // userRef -> agentID -> bool
	targets        map[string]transferTargetStub
	targetErr      error
	candidates     []TransferCandidate
	truncated      bool
	auditRecords   []string
}

type transferTargetStub struct {
	sponsor string
	ok      bool
	reason  string
}

func (h *transferTestHost) FeatureEnabled(_ context.Context, name string) bool {
	return h.featureOn && name == FeatureTransfer
}

func (h *transferTestHost) SessionCredentialAllowed(ctx context.Context) bool {
	_, ref, _, ok := h.Principal(ctx)
	if !ok {
		return false
	}
	if h.sessionAllowed == nil {
		return true
	}
	allowed, set := h.sessionAllowed[ref]
	if !set {
		return true
	}
	return allowed
}

func (h *transferTestHost) OwnerLive(_ context.Context, ownerAgentID string) (bool, error) {
	if h.ownerLiveErr != nil {
		return true, h.ownerLiveErr
	}
	return h.ownerLive[ownerAgentID], nil
}

func (h *transferTestHost) ControlsAgent(ctx context.Context, agentID string) bool {
	_, ref, _, ok := h.Principal(ctx)
	if !ok {
		return false
	}
	return h.controls[ref][agentID]
}

func (h *transferTestHost) TransferTarget(_ context.Context, agentID, _ string) (string, bool, string, error) {
	if h.targetErr != nil {
		return "", false, "", h.targetErr
	}
	st, ok := h.targets[agentID]
	if !ok {
		return "", false, TargetReasonNotFound, nil
	}
	return st.sponsor, st.ok, st.reason, nil
}

func (h *transferTestHost) TransferTargets(_ context.Context, _ string) ([]TransferCandidate, bool, error) {
	if h.targetErr != nil {
		return nil, false, h.targetErr
	}
	return h.candidates, h.truncated, nil
}

func (h *transferTestHost) RecordTransfer(_ context.Context, artifactID, fromOwner, toOwner, authority string) {
	h.auditRecords = append(h.auditRecords, fmt.Sprintf("%s:%s->%s:%s", artifactID, fromOwner, toOwner, authority))
}

func newTransferFixture(t *testing.T) (*fixture, *transferTestHost, string, string, string, *bytes.Buffer) {
	t.Helper()
	f, _ := newLinkFixture(t)
	th := &transferTestHost{
		fakeHost:       f.host,
		featureOn:      true,
		sessionAllowed: map[string]bool{},
		ownerLive:      map[string]bool{},
		controls:       map[string]map[string]bool{},
		targets:        map[string]transferTargetStub{},
	}
	f.svc = NewService(th)
	f.svc.SetStore(f.store)
	f.svc.SetBlobStorage(f.blobs, "hub-1")
	f.svc.SetViewKey([]byte("0123456789abcdef0123456789abcdef"))

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.svc.SetLogger(func() *slog.Logger { return logger })

	oldAgentID := uuid.NewString()
	newAgentID := uuid.NewString()
	oldAgent := principal{PrincipalKindAgent, oldAgentID, "project-1"}
	newAgent := principal{PrincipalKindAgent, newAgentID, "project-1"}
	th.allow(oldAgent, "project-1", PermissionCreate)
	th.allow(oldAgent, "project-1", PermissionRead)
	th.allow(newAgent, "project-1", PermissionCreate)
	th.allow(newAgent, "project-1", PermissionRead)

	for _, p := range []principal{ownerSteward, delegSteward, stAdminGrantee, custodianP, plainReader} {
		th.allow(p, "project-1", PermissionRead)
	}
	th.allowSteward(ownerSteward, oldAgentID, "project-1", StewardClauseProjectOwner)
	th.allowSteward(delegSteward, oldAgentID, "project-1", StewardClauseDelegator)
	th.allowAdmin(custodianP, "project-1")

	pub := f.publish(oldAgent, "doc.md", []byte("# v1"), "scope=project-1")
	artID := pub.Artifact.ID
	if _, err := f.store.PutGrant(context.Background(), &Grant{
		ID: uuid.NewString(), ArtifactID: artID,
		SubjectKind: SubjectPrincipal, SubjectRef: PrincipalRef(stAdminGrantee.kind, stAdminGrantee.ref),
		Permission: GrantAdmin, CreatedAt: time.Now(),
	}, MaxGrantsPerArtifact, true, testActor); err != nil {
		t.Fatal(err)
	}

	th.targets[newAgentID] = transferTargetStub{sponsor: delegSteward.ref, ok: true}
	th.candidates = []TransferCandidate{
		{ID: oldAgentID, Slug: "old-agent", Name: "Old", Phase: "running"},
		{ID: newAgentID, Slug: "new-agent", Name: "New", Phase: "running"},
	}
	return f, th, artID, oldAgentID, newAgentID, &logBuf
}

func doTransferReq(f *fixture, p principal, method, path, body string) *httptest.ResponseRecorder {
	var b []byte
	if body != "" {
		b = []byte(body)
	}
	return f.do(&p, method, path, b, map[string]string{"Content-Type": "application/json"})
}

func TestTransferAuthorizationMatrix(t *testing.T) {
	f, th, artID, oldAgentID, newAgentID, logBuf := newTransferFixture(t)
	body := fmt.Sprintf(`{"to":"agent:%s","expectOwner":"agent:%s"}`, newAgentID, oldAgentID)
	transferPath := RouteCollection + "/" + artID + "/transfer"
	targetsPath := RouteCollection + "/" + artID + "/transfer-targets"

	// 1. Experiment off -> 404 on both routes and canTransfer=false.
	th.featureOn = false
	if rec := doTransferReq(f, ownerSteward, http.MethodPost, transferPath, body); rec.Code != http.StatusNotFound {
		t.Fatalf("gate off POST: got %d, want 404", rec.Code)
	}
	if rec := doTransferReq(f, ownerSteward, http.MethodGet, targetsPath, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("gate off GET targets: got %d, want 404", rec.Code)
	}
	getRec := f.do(&ownerSteward, http.MethodGet, RouteCollection+"/"+artID, nil, nil)
	var getResp ArtifactResponse
	_ = json.Unmarshal(getRec.Body.Bytes(), &getResp)
	if getResp.CanTransfer {
		t.Fatalf("gate off: CanTransfer = true, want false")
	}
	th.featureOn = true

	// 2. Non-reader gets byte-identical 404 to missing artifact (S2).
	outsider := principal{PrincipalKindUser, "user-outsider", ""}
	r1 := doTransferReq(f, outsider, http.MethodPost, transferPath, body)
	r2 := doTransferReq(f, outsider, http.MethodPost, RouteCollection+"/"+uuid.NewString()+"/transfer", body)
	if r1.Code != http.StatusNotFound || r2.Code != http.StatusNotFound || r1.Body.String() != r2.Body.String() {
		t.Fatalf("read-first 404 mismatch: %d %q vs %d %q", r1.Code, r1.Body.String(), r2.Code, r2.Body.String())
	}

	// 3. Refused callers (S1): agent, UAT, admin grantee, custodian, plain reader.
	oldAgent := principal{PrincipalKindAgent, oldAgentID, "project-1"}
	if rec := doTransferReq(f, oldAgent, http.MethodPost, transferPath, body); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "transfer_not_allowed") {
		t.Fatalf("agent caller: got %d %s", rec.Code, rec.Body.String())
	}
	th.sessionAllowed[ownerSteward.ref] = false
	recUAT := doTransferReq(f, ownerSteward, http.MethodPost, transferPath, body)
	if recUAT.Code != http.StatusForbidden || !strings.Contains(recUAT.Body.String(), "session_required") {
		t.Fatalf("UAT caller: got %d %s", recUAT.Code, recUAT.Body.String())
	}
	th.sessionAllowed[ownerSteward.ref] = true

	for _, tc := range []struct {
		name string
		p    principal
	}{
		{"admin grantee", stAdminGrantee},
		{"custodian", custodianP},
		{"plain reader", plainReader},
	} {
		rec := doTransferReq(f, tc.p, http.MethodPost, transferPath, body)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "transfer_not_allowed") {
			t.Errorf("%s: got %d %s", tc.name, rec.Code, rec.Body.String())
		}
	}

	// 4. Live-owner rule (S3): live owner not controlled -> 403 owner_live.
	th.ownerLive[oldAgentID] = true
	if rec := doTransferReq(f, ownerSteward, http.MethodPost, transferPath, body); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "owner_live") {
		t.Fatalf("live uncontrolled owner: got %d %s", rec.Code, rec.Body.String())
	}
	// OwnerLive lookup error -> 503 unavailable (fail closed).
	th.ownerLiveErr = errors.New("db down")
	if rec := doTransferReq(f, ownerSteward, http.MethodPost, transferPath, body); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("OwnerLive error: got %d, want 503", rec.Code)
	}
	th.ownerLiveErr = nil

	// 5. Uniform 403 target_ineligible across all 5 reasons (S4) + precise log reason.
	var bodies []string
	for _, reason := range []string{TargetReasonNotFound, TargetReasonOtherProject, TargetReasonNotControlled, TargetReasonStanding, TargetReasonNoRoot} {
		badTarget := uuid.NewString()
		th.targets[badTarget] = transferTargetStub{ok: false, reason: reason}
		th.ownerLive[oldAgentID] = false
		rec := doTransferReq(f, ownerSteward, http.MethodPost, transferPath, fmt.Sprintf(`{"to":"agent:%s","expectOwner":"agent:%s"}`, badTarget, oldAgentID))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "target_ineligible") {
			t.Fatalf("reason %s: got %d %s", reason, rec.Code, rec.Body.String())
		}
		bodies = append(bodies, rec.Body.String())
		if !strings.Contains(logBuf.String(), `"reason":"`+reason+`"`) {
			t.Errorf("decision log missing internal reason %q", reason)
		}
	}
	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Fatalf("target_ineligible bodies differ: %q vs %q", bodies[0], bodies[i])
		}
	}

	// 6. Invalid targets: same owner, user target, scopeRef.
	for _, badBody := range []string{
		fmt.Sprintf(`{"to":"agent:%s","expectOwner":"agent:%s"}`, oldAgentID, oldAgentID),
		fmt.Sprintf(`{"to":"user:%s","expectOwner":"agent:%s"}`, uuid.NewString(), oldAgentID),
		fmt.Sprintf(`{"to":"agent:%s","expectOwner":"agent:%s","scopeRef":"project-2"}`, newAgentID, oldAgentID),
	} {
		if rec := doTransferReq(f, ownerSteward, http.MethodPost, transferPath, badBody); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_target") {
			t.Errorf("badBody %s: got %d %s", badBody, rec.Code, rec.Body.String())
		}
	}

	// 7. Successful transfer by delegator steward when owner is live and controlled,
	// with notices, audit, and new owner publishing v2 + receiving review notice.
	th.ownerLive[oldAgentID] = true
	th.controls[delegSteward.ref] = map[string]bool{oldAgentID: true}
	var gotNotice TransferNotice
	f.svc.SetTransferNotifier(func(_ context.Context, n TransferNotice) TransferNoticeResult {
		gotNotice = n
		return TransferNoticeResult{NewOwner: true, FormerOwner: true}
	})
	rec := doTransferReq(f, delegSteward, http.MethodPost, transferPath, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("transfer failed: %d %s", rec.Code, rec.Body.String())
	}
	var tr TransferResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Artifact.Artifact.OwnerRef != newAgentID || tr.Artifact.Artifact.FormerOwnerRef != oldAgentID || tr.Artifact.Artifact.TransferredAt == nil {
		t.Errorf("response artifact = %+v", tr.Artifact.Artifact)
	}
	if !tr.Notified.NewOwner || !tr.Notified.FormerOwner || gotNotice.ToAgentID != newAgentID || !gotNotice.FormerOwnerLive {
		t.Errorf("notices = %+v / %+v", tr.Notified, gotNotice)
	}
	if len(th.auditRecords) != 1 {
		t.Errorf("auditRecords = %v, want 1", th.auditRecords)
	}
	if !strings.Contains(logBuf.String(), `"route":"POST /api/v1/artifacts/{id}/transfer"`) {
		t.Errorf("decision log missing transfer routeTemplate; got %s", logBuf.String())
	}

	// New owner publishes seq=2; old owner is refused.
	newAgent := principal{PrincipalKindAgent, newAgentID, "project-1"}
	v2Body := `{"entry":"doc.md","files":[{"path":"doc.md","size":2,"sha256":"` + sha([]byte("v2")) + `"}]}`
	if rOld := doTransferReq(f, oldAgent, http.MethodPost, RouteCollection+"/"+artID+"/versions", v2Body); rOld.Code != http.StatusForbidden {
		t.Errorf("old owner publish: got %d, want 403", rOld.Code)
	}
	rNew := doTransferReq(f, newAgent, http.MethodPost, RouteCollection+"/"+artID+"/versions", v2Body)
	if rNew.Code != http.StatusCreated {
		t.Fatalf("new owner create version: got %d %s", rNew.Code, rNew.Body.String())
	}
}

func TestTransferTargetsRoute(t *testing.T) {
	f, th, artID, oldAgentID, newAgentID, _ := newTransferFixture(t)
	th.ownerLive[oldAgentID] = false
	rec := doTransferReq(f, ownerSteward, http.MethodGet, RouteCollection+"/"+artID+"/transfer-targets", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET transfer-targets: %d %s", rec.Code, rec.Body.String())
	}
	var resp TransferTargetsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// oldAgentID must be filtered out even if host returned it.
	if len(resp.Agents) != 1 || resp.Agents[0].ID != newAgentID {
		t.Fatalf("targets = %+v, want only %s", resp.Agents, newAgentID)
	}
}
