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

package hub

import (
	"context"
	"fmt"
	"strings"
	"testing"

	policytroubleshooterpb "cloud.google.com/go/policytroubleshooter/iam/apiv3/iampb"
	gax "github.com/googleapis/gax-go/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// fakePTClient is a scriptable PTClient for tests.
type fakePTClient struct {
	resp *policytroubleshooterpb.TroubleshootIamPolicyResponse
	err  error
	// captured records the last request for assertions.
	captured *policytroubleshooterpb.TroubleshootIamPolicyRequest
}

func (f *fakePTClient) TroubleshootIamPolicy(
	_ context.Context,
	req *policytroubleshooterpb.TroubleshootIamPolicyRequest,
	_ ...gax.CallOption,
) (*policytroubleshooterpb.TroubleshootIamPolicyResponse, error) {
	f.captured = req
	return f.resp, f.err
}

var (
	testCaller = store.Principal{
		Kind:                store.PrincipalAgent,
		ID:                  "agent-1",
		ServiceAccountEmail: "agent@my-project.iam.gserviceaccount.com",
	}

	testHumanCaller = store.Principal{
		Kind:  store.PrincipalUser,
		ID:    "user-1",
		Email: "alice@example.com",
	}

	testTargetSA = &store.GCPServiceAccount{
		ID:        "sa-1",
		Email:     "target@target-project.iam.gserviceaccount.com",
		ProjectID: "target-project",
	}

	testHubSAEmail = "hub-sa@hub-project.iam.gserviceaccount.com"
)

func TestPolicyTroubleshooterChecker_CallerHasActAsDirectly(t *testing.T) {
	fake := &fakePTClient{
		resp: &policytroubleshooterpb.TroubleshootIamPolicyResponse{
			OverallAccessState: policytroubleshooterpb.TroubleshootIamPolicyResponse_CAN_ACCESS,
		},
	}
	checker := NewPolicyTroubleshooterChecker(fake, testHubSAEmail)

	result, err := checker.CanActAs(context.Background(), testCaller, testTargetSA)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != store.ActAsAllowed {
		t.Errorf("expected ActAsAllowed, got %v", result.Outcome)
	}
	if result.Mechanism != MechanismPolicyTroubleshooter {
		t.Errorf("expected mechanism %q, got %q", MechanismPolicyTroubleshooter, result.Mechanism)
	}
	if !strings.Contains(result.Reason, store.PermissionActAs) {
		t.Errorf("reason should mention the permission: %q", result.Reason)
	}

	// Verify the request was constructed correctly.
	if fake.captured == nil {
		t.Fatal("PT client was not called")
	}
	tuple := fake.captured.GetAccessTuple()
	if tuple.GetPrincipal() != "serviceAccount:agent@my-project.iam.gserviceaccount.com" {
		t.Errorf("unexpected principal: %q", tuple.GetPrincipal())
	}
	if tuple.GetPermission() != store.PermissionActAs {
		t.Errorf("unexpected permission: %q", tuple.GetPermission())
	}
	expectedResource := "//iam.googleapis.com/projects/target-project/serviceAccounts/target@target-project.iam.gserviceaccount.com"
	if tuple.GetFullResourceName() != expectedResource {
		t.Errorf("unexpected resource name: %q, want %q", tuple.GetFullResourceName(), expectedResource)
	}
}

func TestPolicyTroubleshooterChecker_CallerHasActAsViaProjectBinding(t *testing.T) {
	// From PT's perspective, a project-level binding is the same CAN_ACCESS
	// result; the difference is only in the explained policies. This test
	// verifies the checker maps it the same way.
	fake := &fakePTClient{
		resp: &policytroubleshooterpb.TroubleshootIamPolicyResponse{
			OverallAccessState: policytroubleshooterpb.TroubleshootIamPolicyResponse_CAN_ACCESS,
		},
	}
	checker := NewPolicyTroubleshooterChecker(fake, testHubSAEmail)

	result, err := checker.CanActAs(context.Background(), testCaller, testTargetSA)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != store.ActAsAllowed {
		t.Errorf("expected ActAsAllowed, got %v", result.Outcome)
	}
}

func TestPolicyTroubleshooterChecker_CallerDoesNotHaveActAs(t *testing.T) {
	fake := &fakePTClient{
		resp: &policytroubleshooterpb.TroubleshootIamPolicyResponse{
			OverallAccessState: policytroubleshooterpb.TroubleshootIamPolicyResponse_CANNOT_ACCESS,
		},
	}
	checker := NewPolicyTroubleshooterChecker(fake, testHubSAEmail)

	result, err := checker.CanActAs(context.Background(), testCaller, testTargetSA)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != store.ActAsDenied {
		t.Errorf("expected ActAsDenied, got %v", result.Outcome)
	}
	if result.Mechanism != MechanismPolicyTroubleshooter {
		t.Errorf("expected mechanism %q, got %q", MechanismPolicyTroubleshooter, result.Mechanism)
	}
	if !strings.Contains(result.Reason, "does not have") {
		t.Errorf("reason should say 'does not have': %q", result.Reason)
	}
}

func TestPolicyTroubleshooterChecker_DenyPolicyOverridesAllow(t *testing.T) {
	// CANNOT_ACCESS with a deny policy explanation that says DENIED.
	fake := &fakePTClient{
		resp: &policytroubleshooterpb.TroubleshootIamPolicyResponse{
			OverallAccessState: policytroubleshooterpb.TroubleshootIamPolicyResponse_CANNOT_ACCESS,
			DenyPolicyExplanation: &policytroubleshooterpb.DenyPolicyExplanation{
				DenyAccessState: policytroubleshooterpb.DenyAccessState_DENY_ACCESS_STATE_DENIED,
			},
		},
	}
	checker := NewPolicyTroubleshooterChecker(fake, testHubSAEmail)

	result, err := checker.CanActAs(context.Background(), testCaller, testTargetSA)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != store.ActAsDenied {
		t.Errorf("expected ActAsDenied, got %v", result.Outcome)
	}
	if !strings.Contains(result.Reason, "deny policy") {
		t.Errorf("reason should mention deny policy: %q", result.Reason)
	}
}

func TestPolicyTroubleshooterChecker_GroupBindingUnresolvable(t *testing.T) {
	// UNKNOWN_INFO with MEMBERSHIP_UNKNOWN_INFO on a group binding.
	fake := &fakePTClient{
		resp: &policytroubleshooterpb.TroubleshootIamPolicyResponse{
			OverallAccessState: policytroubleshooterpb.TroubleshootIamPolicyResponse_UNKNOWN_INFO,
			AllowPolicyExplanation: &policytroubleshooterpb.AllowPolicyExplanation{
				AllowAccessState: policytroubleshooterpb.AllowAccessState_ALLOW_ACCESS_STATE_UNKNOWN_INFO,
				ExplainedPolicies: []*policytroubleshooterpb.ExplainedAllowPolicy{
					{
						BindingExplanations: []*policytroubleshooterpb.AllowBindingExplanation{
							{
								Memberships: map[string]*policytroubleshooterpb.AllowBindingExplanation_AnnotatedAllowMembership{
									"group:eng@example.com": {
										Membership: policytroubleshooterpb.MembershipMatchingState_MEMBERSHIP_UNKNOWN_INFO,
									},
								},
							},
						},
					},
				},
			},
		},
	}
	checker := NewPolicyTroubleshooterChecker(fake, testHubSAEmail)

	result, err := checker.CanActAs(context.Background(), testCaller, testTargetSA)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != store.ActAsIndeterminate {
		t.Errorf("expected ActAsIndeterminate, got %v", result.Outcome)
	}
	if !strings.Contains(result.Reason, "group:eng@example.com") {
		t.Errorf("reason should name the unresolvable group: %q", result.Reason)
	}
	if !strings.Contains(result.Reason, "groups.read") {
		t.Errorf("reason should mention groups.read access: %q", result.Reason)
	}
}

func TestPolicyTroubleshooterChecker_ConditionalBinding(t *testing.T) {
	fake := &fakePTClient{
		resp: &policytroubleshooterpb.TroubleshootIamPolicyResponse{
			OverallAccessState: policytroubleshooterpb.TroubleshootIamPolicyResponse_UNKNOWN_CONDITIONAL,
		},
	}
	checker := NewPolicyTroubleshooterChecker(fake, testHubSAEmail)

	result, err := checker.CanActAs(context.Background(), testCaller, testTargetSA)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != store.ActAsIndeterminate {
		t.Errorf("expected ActAsIndeterminate, got %v", result.Outcome)
	}
	if !strings.Contains(result.Reason, "runtime condition") {
		t.Errorf("reason should mention runtime condition: %q", result.Reason)
	}
}

func TestPolicyTroubleshooterChecker_GRPCUnavailable(t *testing.T) {
	fake := &fakePTClient{
		err: status.Error(codes.Unavailable, "service unavailable"),
	}
	checker := NewPolicyTroubleshooterChecker(fake, testHubSAEmail)

	result, err := checker.CanActAs(context.Background(), testCaller, testTargetSA)

	if err == nil {
		t.Fatal("expected an error")
	}
	if result.Outcome != store.ActAsIndeterminate {
		t.Errorf("expected ActAsIndeterminate on gRPC error, got %v", result.Outcome)
	}
	if result.Mechanism != MechanismPolicyTroubleshooter {
		t.Errorf("expected mechanism %q, got %q", MechanismPolicyTroubleshooter, result.Mechanism)
	}
}

func TestPolicyTroubleshooterChecker_GRPCPermissionDenied(t *testing.T) {
	fake := &fakePTClient{
		err: status.Error(codes.PermissionDenied, "caller does not have permission"),
	}
	checker := NewPolicyTroubleshooterChecker(fake, testHubSAEmail)

	result, err := checker.CanActAs(context.Background(), testCaller, testTargetSA)

	if err == nil {
		t.Fatal("expected an error")
	}
	if result.Outcome != store.ActAsIndeterminate {
		t.Errorf("expected ActAsIndeterminate on gRPC error, got %v", result.Outcome)
	}
}

func TestPolicyTroubleshooterChecker_EmptyProjectID(t *testing.T) {
	fake := &fakePTClient{
		resp: &policytroubleshooterpb.TroubleshootIamPolicyResponse{
			OverallAccessState: policytroubleshooterpb.TroubleshootIamPolicyResponse_CAN_ACCESS,
		},
	}
	checker := NewPolicyTroubleshooterChecker(fake, testHubSAEmail)

	noProjectSA := &store.GCPServiceAccount{
		ID:    "sa-no-project",
		Email: "target@missing.iam.gserviceaccount.com",
		// ProjectID intentionally empty
	}

	result, err := checker.CanActAs(context.Background(), testCaller, noProjectSA)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != store.ActAsIndeterminate {
		t.Errorf("expected ActAsIndeterminate for empty ProjectID, got %v", result.Outcome)
	}
	if fake.captured != nil {
		t.Error("PT client should NOT have been called when ProjectID is empty")
	}
}

func TestPolicyTroubleshooterChecker_AgentCaller(t *testing.T) {
	fake := &fakePTClient{
		resp: &policytroubleshooterpb.TroubleshootIamPolicyResponse{
			OverallAccessState: policytroubleshooterpb.TroubleshootIamPolicyResponse_CAN_ACCESS,
		},
	}
	checker := NewPolicyTroubleshooterChecker(fake, testHubSAEmail)

	result, err := checker.CanActAs(context.Background(), testCaller, testTargetSA)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != store.ActAsAllowed {
		t.Errorf("expected ActAsAllowed for agent caller, got %v", result.Outcome)
	}
	// Verify the principal was formatted as serviceAccount:
	tuple := fake.captured.GetAccessTuple()
	if !strings.HasPrefix(tuple.GetPrincipal(), "serviceAccount:") {
		t.Errorf("agent caller should produce serviceAccount: principal, got %q", tuple.GetPrincipal())
	}
}

func TestPolicyTroubleshooterChecker_HumanCaller(t *testing.T) {
	fake := &fakePTClient{
		resp: &policytroubleshooterpb.TroubleshootIamPolicyResponse{
			OverallAccessState: policytroubleshooterpb.TroubleshootIamPolicyResponse_CAN_ACCESS,
		},
	}
	checker := NewPolicyTroubleshooterChecker(fake, testHubSAEmail)

	result, err := checker.CanActAs(context.Background(), testHumanCaller, testTargetSA)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != store.ActAsAllowed {
		t.Errorf("expected ActAsAllowed for human caller, got %v", result.Outcome)
	}
	// Verify the principal was formatted as user:
	tuple := fake.captured.GetAccessTuple()
	if !strings.HasPrefix(tuple.GetPrincipal(), "user:") {
		t.Errorf("human caller should produce user: principal, got %q", tuple.GetPrincipal())
	}
	if tuple.GetPrincipal() != "user:alice@example.com" {
		t.Errorf("unexpected principal: %q", tuple.GetPrincipal())
	}
}

func TestPolicyTroubleshooterChecker_UnrecognisedState(t *testing.T) {
	// Simulate an unknown/future enum value.
	fake := &fakePTClient{
		resp: &policytroubleshooterpb.TroubleshootIamPolicyResponse{
			OverallAccessState: policytroubleshooterpb.TroubleshootIamPolicyResponse_OverallAccessState(999),
		},
	}
	checker := NewPolicyTroubleshooterChecker(fake, testHubSAEmail)

	result, err := checker.CanActAs(context.Background(), testCaller, testTargetSA)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != store.ActAsIndeterminate {
		t.Errorf("expected ActAsIndeterminate for unrecognised state, got %v", result.Outcome)
	}
	if !strings.Contains(result.Reason, "unrecognised") {
		t.Errorf("reason should mention unrecognised state: %q", result.Reason)
	}
}

func TestPolicyTroubleshooterChecker_UnknownInfoWithoutGroupDetails(t *testing.T) {
	// UNKNOWN_INFO but no group-specific binding detail — should still produce
	// a useful reason mentioning securityReviewer.
	fake := &fakePTClient{
		resp: &policytroubleshooterpb.TroubleshootIamPolicyResponse{
			OverallAccessState: policytroubleshooterpb.TroubleshootIamPolicyResponse_UNKNOWN_INFO,
		},
	}
	checker := NewPolicyTroubleshooterChecker(fake, testHubSAEmail)

	result, err := checker.CanActAs(context.Background(), testCaller, testTargetSA)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != store.ActAsIndeterminate {
		t.Errorf("expected ActAsIndeterminate, got %v", result.Outcome)
	}
	if !strings.Contains(result.Reason, "securityReviewer") {
		t.Errorf("reason should mention securityReviewer when no group detail: %q", result.Reason)
	}
}

func TestPolicyTroubleshooterChecker_NoHubSAEmail(t *testing.T) {
	// When hubSAEmail is empty, reason should still be useful.
	fake := &fakePTClient{
		resp: &policytroubleshooterpb.TroubleshootIamPolicyResponse{
			OverallAccessState: policytroubleshooterpb.TroubleshootIamPolicyResponse_UNKNOWN_INFO,
		},
	}
	checker := NewPolicyTroubleshooterChecker(fake, "") // no hub SA email

	result, err := checker.CanActAs(context.Background(), testCaller, testTargetSA)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != store.ActAsIndeterminate {
		t.Errorf("expected ActAsIndeterminate, got %v", result.Outcome)
	}
	// Should not panic or produce empty reason even without hub SA email.
	if result.Reason == "" {
		t.Error("reason should not be empty")
	}
}

func TestPolicyTroubleshooterChecker_AllMappingsAreAttributed(t *testing.T) {
	// Every possible overall state must produce a non-empty mechanism.
	states := []policytroubleshooterpb.TroubleshootIamPolicyResponse_OverallAccessState{
		policytroubleshooterpb.TroubleshootIamPolicyResponse_CAN_ACCESS,
		policytroubleshooterpb.TroubleshootIamPolicyResponse_CANNOT_ACCESS,
		policytroubleshooterpb.TroubleshootIamPolicyResponse_UNKNOWN_INFO,
		policytroubleshooterpb.TroubleshootIamPolicyResponse_UNKNOWN_CONDITIONAL,
		policytroubleshooterpb.TroubleshootIamPolicyResponse_OVERALL_ACCESS_STATE_UNSPECIFIED,
	}

	for _, s := range states {
		t.Run(fmt.Sprintf("state_%v", s), func(t *testing.T) {
			fake := &fakePTClient{
				resp: &policytroubleshooterpb.TroubleshootIamPolicyResponse{
					OverallAccessState: s,
				},
			}
			checker := NewPolicyTroubleshooterChecker(fake, testHubSAEmail)

			result, err := checker.CanActAs(context.Background(), testCaller, testTargetSA)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Mechanism == "" {
				t.Errorf("mechanism must never be empty for state %v", s)
			}
			if result.Reason == "" {
				t.Errorf("reason must never be empty for state %v", s)
			}
		})
	}
}
