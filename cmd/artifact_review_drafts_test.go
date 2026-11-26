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
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

func TestListArtifactsDraftsFlagAndOutput(t *testing.T) {
	const artID = "5f1c2d3e-1111-2222-3333-444455556666"
	svc := &stubReviewRequestArtifactService{
		listResp: &hubclient.ArtifactListResponse{
			Artifacts: []hubclient.ArtifactListItem{
				{
					Artifact: hubclient.Artifact{
						ID:         artID,
						Title:      "Drafted Spec",
						CurrentSeq: 2,
						UpdatedAt:  time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC),
					},
					ReviewInDraft: true,
					Access:        "project",
				},
			},
		},
	}

	var out bytes.Buffer
	err := listArtifacts(context.Background(), svc, &out, listArtifactsParams{
		Options: hubclient.ListArtifactsOptions{Drafts: true},
	})
	if err != nil {
		t.Fatalf("listArtifacts(--drafts): %v", err)
	}
	if svc.lastListOpts == nil || !svc.lastListOpts.Drafts {
		t.Fatalf("List options = %+v, want Drafts=true", svc.lastListOpts)
	}
	if !strings.Contains(out.String(), "project, review in draft") {
		t.Errorf("table output missing review in draft:\n%s", out.String())
	}

	// JSON output preserves reviewInDraft.
	out.Reset()
	err = listArtifacts(context.Background(), svc, &out, listArtifactsParams{
		Options: hubclient.ListArtifactsOptions{Drafts: true},
		JSON:    true,
	})
	if err != nil {
		t.Fatalf("listArtifacts(--drafts --json): %v", err)
	}
	var decoded struct {
		Artifacts []hubclient.ArtifactListItem `json:"artifacts"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if len(decoded.Artifacts) != 1 || !decoded.Artifacts[0].ReviewInDraft {
		t.Fatalf("decoded JSON artifacts = %+v, want ReviewInDraft=true", decoded.Artifacts)
	}

	// Flag is registered on artifactListCmd and command is allowed in both modes.
	if f := artifactListCmd.Flags().Lookup("drafts"); f == nil {
		t.Fatal("artifact list missing --drafts flag")
	}
	if !agentAllowed["artifact.list"] {
		t.Fatal("artifact.list must remain in agentAllowed")
	}
}
