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

package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

type stubReviewRequestArtifactService struct {
	hubclient.ArtifactService
	lastID       string
	lastReq      *hubclient.ArtifactReviewRequestsInput
	lastDismiss  string
	lastListOpts *hubclient.ListArtifactsOptions
	reqResp      *hubclient.ArtifactReviewRequestsResponse
	dismissResp  *hubclient.ArtifactDismissReviewRequestResponse
	listResp     *hubclient.ArtifactListResponse
}

func (s *stubReviewRequestArtifactService) RequestReview(_ context.Context, id string, req *hubclient.ArtifactReviewRequestsInput) (*hubclient.ArtifactReviewRequestsResponse, error) {
	s.lastID = id
	s.lastReq = req
	return s.reqResp, nil
}

func (s *stubReviewRequestArtifactService) DismissReviewRequest(_ context.Context, id, note string) (*hubclient.ArtifactDismissReviewRequestResponse, error) {
	s.lastID = id
	s.lastDismiss = note
	return s.dismissResp, nil
}

func (s *stubReviewRequestArtifactService) List(_ context.Context, opts *hubclient.ListArtifactsOptions) (*hubclient.ArtifactListResponse, error) {
	s.lastListOpts = opts
	return s.listResp, nil
}

type stubUserLookupService struct {
	hubclient.UserService
	users []hubclient.User
}

func (s *stubUserLookupService) List(_ context.Context, _ *hubclient.ListUsersOptions) (*hubclient.ListUsersResponse, error) {
	return &hubclient.ListUsersResponse{Users: s.users}, nil
}

func TestRequestArtifactReviewCLI(t *testing.T) {
	const artID = "5f1c2d3e-1111-2222-3333-444455556666"
	svc := &stubReviewRequestArtifactService{
		reqResp: &hubclient.ArtifactReviewRequestsResponse{
			Requests: []hubclient.ArtifactReviewRequestResult{
				{Reviewer: "user:u-alice", Outcome: artifacts.ReviewRequestOutcomeCreated, Access: artifacts.ReviewRequestAccessExisting, Notified: true},
				{Reviewer: "user:u-bob", Outcome: artifacts.ReviewRequestOutcomeReminded, Access: artifacts.ReviewRequestAccessExisting, Notified: false, NotifySkipped: "rate_limited"},
				{Reviewer: "user:u-carol", Outcome: artifacts.ReviewRequestOutcomeUnchanged, Access: artifacts.ReviewRequestAccessExisting},
				{Reviewer: "user:u-dave", Outcome: artifacts.ReviewRequestOutcomeRefused, Code: artifacts.CodeReviewerCannotReview},
			},
		},
	}
	users := &stubUserLookupService{
		users: []hubclient.User{{ID: "u-alice", Email: "alice@example.com"}},
	}

	var out bytes.Buffer
	err := requestArtifactReview(context.Background(), svc, users, &out, "scion://artifact/"+artID, requestReviewOptions{
		Users: []string{"alice@example.com", "user:u-bob", "u-carol", "u-dave"},
		Note:  "Please check section 2",
	})
	if err != nil {
		t.Fatalf("requestArtifactReview: %v", err)
	}
	if svc.lastID != artID {
		t.Errorf("lastID = %q, want %q", svc.lastID, artID)
	}
	if len(svc.lastReq.Reviewers) != 4 || svc.lastReq.Reviewers[0] != "user:u-alice" || svc.lastReq.Note != "Please check section 2" {
		t.Errorf("unexpected request input: %+v", svc.lastReq)
	}
	got := out.String()
	for _, want := range []string{
		"user:u-alice: requested",
		"user:u-bob: reminded (not notified: rate_limited)",
		"user:u-carol: already requested",
		"user:u-dave: refused (reviewer_cannot_review)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestDismissArtifactReviewCLI(t *testing.T) {
	const artID = "5f1c2d3e-1111-2222-3333-444455556666"
	svc := &stubReviewRequestArtifactService{
		dismissResp: &hubclient.ArtifactDismissReviewRequestResponse{
			Request:        hubclient.ArtifactReviewRequest{ID: "req-1", State: artifacts.ReviewRequestStateDismissed},
			AuthorNotified: true,
		},
	}
	var out bytes.Buffer
	err := dismissArtifactReview(context.Background(), svc, &out, artID, dismissReviewOptions{Note: "Busy"})
	if err != nil {
		t.Fatalf("dismissArtifactReview: %v", err)
	}
	if svc.lastID != artID || svc.lastDismiss != "Busy" {
		t.Errorf("lastID=%q lastDismiss=%q", svc.lastID, svc.lastDismiss)
	}
	if !strings.Contains(out.String(), "Dismissed review request on scion://artifact/"+artID+" (author notified)") {
		t.Errorf("unexpected output: %q", out.String())
	}
}

func TestListArtifactsRequestedFlag(t *testing.T) {
	const artID = "5f1c2d3e-1111-2222-3333-444455556666"
	svc := &stubReviewRequestArtifactService{
		listResp: &hubclient.ArtifactListResponse{
			Artifacts: []hubclient.ArtifactListItem{
				{
					Artifact: hubclient.Artifact{
						ID:         artID,
						Title:      "Design Doc",
						CurrentSeq: 2,
						UpdatedAt:  time.Date(2026, 11, 25, 20, 0, 0, 0, time.UTC),
					},
					Access:          "project",
					ReviewRequested: true,
				},
			},
		},
	}
	var out bytes.Buffer
	err := listArtifacts(context.Background(), svc, &out, listArtifactsParams{
		Options: hubclient.ListArtifactsOptions{Requested: true},
	})
	if err != nil {
		t.Fatalf("listArtifacts: %v", err)
	}
	if svc.lastListOpts == nil || !svc.lastListOpts.Requested {
		t.Fatalf("expected Requested=true, got %+v", svc.lastListOpts)
	}
	got := out.String()
	if !strings.Contains(got, "STATE") || !strings.Contains(got, "review requested") {
		t.Errorf("expected STATE column with 'review requested' in output:\n%s", got)
	}
}

func TestArtifactReviewRequestModeAndWindowsGates(t *testing.T) {
	if agentAllowed["artifact.request-review"] {
		t.Error("artifact.request-review must NOT be in agentAllowed in Phase 1")
	}
	if agentAllowed["artifact.dismiss-review"] {
		t.Error("artifact.dismiss-review must NOT be in agentAllowed")
	}
	if !windowsAllowed["artifact.request-review"] {
		t.Error("artifact.request-review must be in windowsAllowed")
	}
	if !windowsAllowed["artifact.dismiss-review"] {
		t.Error("artifact.dismiss-review must be in windowsAllowed")
	}
}
