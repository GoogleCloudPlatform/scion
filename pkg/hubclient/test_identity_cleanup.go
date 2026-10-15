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

package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// Issuer-scoped cleanup and provenance of test identity resources
// (ptone/scion#4240, Phase 3c).

// TestIdentityCleanupTarget is one resource a cleanup run selected.
type TestIdentityCleanupTarget struct {
	// Type is agent, schedule, group, project or test_identity.
	Type          string `json:"type"`
	ID            string `json:"id"`
	FixtureRootID string `json:"fixtureRootId"`
	ProjectID     string `json:"projectId,omitempty"`
	// Status is target (dry run), deleted, not_done or kept.
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// TestIdentityCleanupIdentity is one test identity a cleanup run reached.
type TestIdentityCleanupIdentity struct {
	ID string `json:"id"`
	// State is expired or absent.
	State  string                   `json:"state"`
	Report *TestIdentityPurgeReport `json:"report,omitempty"`
	Error  string                   `json:"error,omitempty"`
}

// TestIdentityCleanupReport is the result of a cleanup run.
type TestIdentityCleanupReport struct {
	RunID             string                        `json:"runId,omitempty"`
	DryRun            bool                          `json:"dryRun"`
	Scope             string                        `json:"scope"`
	Complete          bool                          `json:"complete"`
	Identities        []TestIdentityCleanupIdentity `json:"identities"`
	Targets           []TestIdentityCleanupTarget   `json:"targets"`
	Deleted           TestIdentityPurgeCounts       `json:"deleted"`
	IdentitiesDeleted int                           `json:"identitiesDeleted"`
	LiveSkipped       int                           `json:"liveSkipped"`
	NotDone           []TestIdentityPurgeItem       `json:"notDone,omitempty"`
	Unproven          []TestIdentityPurgeItem       `json:"unproven,omitempty"`
	Truncated         bool                          `json:"truncated,omitempty"`
}

// TestIdentityProvenance is the hub's answer for one resource.
type TestIdentityProvenance struct {
	Resource        string `json:"resource"`
	Proven          bool   `json:"proven"`
	FixtureRootID   string `json:"fixtureRootId,omitempty"`
	FixtureIssuerID string `json:"fixtureIssuerId,omitempty"`
	// FixtureState is live, expired or absent.
	FixtureState string `json:"fixtureState,omitempty"`
	// Deleted is true for a resource that is gone, proven by its deletion
	// record (whose correlation ID is DeletionRecord).
	Deleted        bool   `json:"deleted,omitempty"`
	DeletionRecord string `json:"deletionRecord,omitempty"`
	Reason         string `json:"reason"`
}

// Cleanup removes the resources of the caller's expired or deleted test
// identities. It sends no body and names no target: the hub selects them.
// With dryRun the hub lists the targets and writes nothing. It is sent
// once, never retried by the transport; running it again is safe. An
// incomplete run returns its report together with the error.
func (s *testIdentityService) Cleanup(ctx context.Context, dryRun bool) (*TestIdentityCleanupReport, error) {
	path := "/api/v1/test-identities/cleanup"
	if dryRun {
		path += "?dryRun=true"
	}
	resp, err := s.c.postNoRetry(ctx, path, nil, nil)
	if err != nil {
		return nil, err
	}
	report, err := apiclient.DecodeRequired[TestIdentityCleanupReport](resp)
	if err != nil {
		if r, ok := TestIdentityCleanupIncomplete(err); ok {
			return r, err
		}
		return nil, err
	}
	return report, nil
}

// Provenance asks whether the hub recorded resource ("<type>:<id>", type
// one of agent, project, schedule, group) as created under a test
// identity.
func (s *testIdentityService) Provenance(ctx context.Context, resource string) (*TestIdentityProvenance, error) {
	if resource == "" {
		return nil, fmt.Errorf("resource is required: <type>:<id>")
	}
	resp, err := s.c.getWithQuery(ctx, "/api/v1/test-identities/provenance", url.Values{"resource": {resource}}, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[TestIdentityProvenance](resp)
}

// TestIdentityCleanupIncomplete returns the report carried by a cleanup
// answer that did not complete (details.reason purge_incomplete). ok is
// false when err is not such an answer.
func TestIdentityCleanupIncomplete(err error) (*TestIdentityCleanupReport, bool) {
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) || apiErr.Details == nil {
		return nil, false
	}
	if reason, _ := apiErr.Details["reason"].(string); reason != "purge_incomplete" {
		return nil, false
	}
	raw, err2 := json.Marshal(apiErr.Details["report"])
	if err2 != nil {
		return nil, false
	}
	var report TestIdentityCleanupReport
	if json.Unmarshal(raw, &report) != nil {
		return nil, false
	}
	return &report, true
}
