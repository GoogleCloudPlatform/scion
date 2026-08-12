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

	policytroubleshooterpb "cloud.google.com/go/policytroubleshooter/iam/apiv3/iampb"
	gax "github.com/googleapis/gax-go/v2"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// MechanismPolicyTroubleshooter names the caller-permission check mechanism
// for audit records. Every allowed or denied result from PolicyTroubleshooterChecker
// carries this value as its Mechanism field.
const MechanismPolicyTroubleshooter = "policy-troubleshooter"

// PTClient is the subset of the Policy Troubleshooter v3 client that the
// checker actually calls. Exists for testability — the production
// implementation is *policytroubleshooteriam.PolicyTroubleshooterClient, and
// the test implementation is a fake.
type PTClient interface {
	TroubleshootIamPolicy(ctx context.Context, req *policytroubleshooterpb.TroubleshootIamPolicyRequest, opts ...gax.CallOption) (*policytroubleshooterpb.TroubleshootIamPolicyResponse, error)
}

// PolicyTroubleshooterChecker implements store.CallerPermissionChecker using
// the GCP Policy Troubleshooter v3 API. It is the ONLY CallerPermissionChecker
// implementation that contacts GCP. There is no fallback — PT answers, or the
// result is indeterminate.
//
// Why v3 and not v1: v3 is GA and includes IAM Deny / PAB evaluation. v1 does
// not evaluate deny policies. Since Q16 drops the getIamPolicy fallback partly
// because it is blind to deny, using a PT version that is equally blind would
// defeat the argument.
type PolicyTroubleshooterChecker struct {
	// client is the Policy Troubleshooter v3 gRPC client.
	client PTClient

	// hubSAEmail is the Hub's own service account email, used only in
	// diagnostic messages ("the Hub SA needs..."). Never sent as a
	// principal to PT — the principal is always the CALLER.
	hubSAEmail string

	// denyUnknownFailOpen controls behavior when PT v3 returns
	// UNKNOWN_INFO overall because deny policies could not be evaluated.
	// When true (default): if allow=GRANTED and deny=UNKNOWN (not DENIED),
	// treat as allowed. When false: treat as indeterminate (fail-closed).
	denyUnknownFailOpen bool
}

// NewPolicyTroubleshooterChecker creates a new PolicyTroubleshooterChecker.
// client must be a Policy Troubleshooter v3 client (or a fake for tests).
// hubSAEmail is used only in diagnostic reason strings.
// denyUnknownFailOpen controls the UNKNOWN_INFO fallback: true means
// allow-granted + deny-unknown is treated as allowed (fail-open).
func NewPolicyTroubleshooterChecker(client PTClient, hubSAEmail string, denyUnknownFailOpen bool) *PolicyTroubleshooterChecker {
	return &PolicyTroubleshooterChecker{
		client:              client,
		hubSAEmail:          hubSAEmail,
		denyUnknownFailOpen: denyUnknownFailOpen,
	}
}

// CanActAs implements store.CallerPermissionChecker.
func (c *PolicyTroubleshooterChecker) CanActAs(
	ctx context.Context,
	caller store.Principal,
	targetSA *store.GCPServiceAccount,
) (store.ActAsResult, error) {
	// 1. Build the full resource name. PT requires the real GCP project ID.
	if targetSA.ProjectID == "" {
		return store.ActAsResult{
			Outcome:   store.ActAsIndeterminate,
			Mechanism: MechanismPolicyTroubleshooter,
			Reason:    "target service account has no GCP project ID; cannot construct Policy Troubleshooter query",
		}, nil
	}
	fullResourceName := fmt.Sprintf(
		"//iam.googleapis.com/projects/%s/serviceAccounts/%s",
		targetSA.ProjectID, targetSA.Email,
	)

	// 2. Build the principal string.
	principalID := caller.GCPPrincipalID()
	if principalID == "" {
		// Should not happen — evaluateActAs checks HasGCPIdentity before
		// calling the checker — but must not panic.
		return store.ActAsResult{
			Outcome:   store.ActAsIndeterminate,
			Mechanism: MechanismPolicyTroubleshooter,
			Reason:    "caller has no GCP principal ID",
		}, nil
	}

	// 3. Call TroubleshootIamPolicy.
	// PT v3 requires bare email, not IAM "type:email" form.
	resp, err := c.client.TroubleshootIamPolicy(ctx, &policytroubleshooterpb.TroubleshootIamPolicyRequest{
		AccessTuple: &policytroubleshooterpb.AccessTuple{
			Principal:        ptPrincipal(principalID),
			FullResourceName: fullResourceName,
			Permission:       store.PermissionActAs,
		},
	})
	if err != nil {
		// gRPC error: return indeterminate + error per the interface contract.
		return store.ActAsResult{
			Outcome:   store.ActAsIndeterminate,
			Mechanism: MechanismPolicyTroubleshooter,
			Reason:    "Policy Troubleshooter API call failed",
		}, fmt.Errorf("policy troubleshooter call for %s on %s: %w", principalID, targetSA.Email, err)
	}

	// 4. Map the response.
	return c.mapResponse(resp, principalID, targetSA.Email), nil
}

// mapResponse translates a PT v3 TroubleshootIamPolicyResponse into an
// ActAsResult. The mapping is:
//
//	CAN_ACCESS       → ActAsAllowed
//	CANNOT_ACCESS    → ActAsDenied
//	UNKNOWN_INFO     → ActAsIndeterminate
//	UNKNOWN_CONDITIONAL → ActAsIndeterminate
//	anything else    → ActAsIndeterminate (defensive)
func (c *PolicyTroubleshooterChecker) mapResponse(
	resp *policytroubleshooterpb.TroubleshootIamPolicyResponse,
	principalID, targetSAEmail string,
) store.ActAsResult {
	overall := resp.GetOverallAccessState()

	switch overall {
	case policytroubleshooterpb.TroubleshootIamPolicyResponse_CAN_ACCESS:
		return store.ActAsResult{
			Outcome:   store.ActAsAllowed,
			Mechanism: MechanismPolicyTroubleshooter,
			Reason: fmt.Sprintf("%s has %s on %s",
				principalID, store.PermissionActAs, targetSAEmail),
		}

	case policytroubleshooterpb.TroubleshootIamPolicyResponse_CANNOT_ACCESS:
		reason := c.buildDenialReason(resp, principalID, targetSAEmail)
		return store.ActAsResult{
			Outcome:   store.ActAsDenied,
			Mechanism: MechanismPolicyTroubleshooter,
			Reason:    reason,
		}

	case policytroubleshooterpb.TroubleshootIamPolicyResponse_UNKNOWN_INFO:
		// Check if the allow sub-explanation shows GRANTED.
		// When deny is UNKNOWN (not DENIED) and allow is GRANTED,
		// the configurable fallback policy determines the outcome.
		if c.denyUnknownFailOpen {
			if allowGranted, denyNotDenied := c.checkSubExplanations(resp); allowGranted && denyNotDenied {
				return store.ActAsResult{
					Outcome:   store.ActAsAllowed,
					Mechanism: MechanismPolicyTroubleshooter,
					Reason: fmt.Sprintf("%s has %s on %s (allow policy granted; "+
						"deny policy evaluation inconclusive, fail-open policy applied)",
						principalID, store.PermissionActAs, targetSAEmail),
				}
			}
		}
		// Fall through to indeterminate if fail-closed or if allow is not granted.
		reason := c.buildUnknownReason(resp, principalID, targetSAEmail)
		return store.ActAsResult{
			Outcome:   store.ActAsIndeterminate,
			Mechanism: MechanismPolicyTroubleshooter,
			Reason:    reason,
		}

	case policytroubleshooterpb.TroubleshootIamPolicyResponse_UNKNOWN_CONDITIONAL:
		return store.ActAsResult{
			Outcome:   store.ActAsIndeterminate,
			Mechanism: MechanismPolicyTroubleshooter,
			Reason: fmt.Sprintf("access for %s to %s depends on a runtime condition "+
				"that Policy Troubleshooter cannot evaluate statically",
				principalID, targetSAEmail),
		}

	default:
		// Unrecognised or zero value — defensive indeterminate.
		return store.ActAsResult{
			Outcome:   store.ActAsIndeterminate,
			Mechanism: MechanismPolicyTroubleshooter,
			Reason: fmt.Sprintf("Policy Troubleshooter returned unrecognised state %v for %s on %s",
				overall, principalID, targetSAEmail),
		}
	}
}

// checkSubExplanations inspects the allow and deny sub-explanations in a PT v3
// response. Returns (allowGranted, denyNotDenied).
func (c *PolicyTroubleshooterChecker) checkSubExplanations(
	resp *policytroubleshooterpb.TroubleshootIamPolicyResponse,
) (bool, bool) {
	allowGranted := false
	if allowExpl := resp.GetAllowPolicyExplanation(); allowExpl != nil {
		allowGranted = allowExpl.GetAllowAccessState() == policytroubleshooterpb.AllowAccessState_ALLOW_ACCESS_STATE_GRANTED
	}

	denyNotDenied := true
	if denyExpl := resp.GetDenyPolicyExplanation(); denyExpl != nil {
		if denyExpl.GetDenyAccessState() == policytroubleshooterpb.DenyAccessState_DENY_ACCESS_STATE_DENIED {
			denyNotDenied = false
		}
	}

	return allowGranted, denyNotDenied
}

// buildDenialReason constructs a human-readable reason for a CANNOT_ACCESS result.
func (c *PolicyTroubleshooterChecker) buildDenialReason(
	resp *policytroubleshooterpb.TroubleshootIamPolicyResponse,
	principalID, targetSAEmail string,
) string {
	base := fmt.Sprintf("%s does not have %s on %s",
		principalID, store.PermissionActAs, targetSAEmail)

	// Check if a deny policy contributed.
	if denyExpl := resp.GetDenyPolicyExplanation(); denyExpl != nil {
		if denyExpl.GetDenyAccessState() == policytroubleshooterpb.DenyAccessState_DENY_ACCESS_STATE_DENIED {
			return base + "; an IAM deny policy overrides any allow binding"
		}
	}

	return base
}

// buildUnknownReason constructs a reason for UNKNOWN_INFO, scanning bindings
// for MEMBERSHIP_UNKNOWN_INFO to identify unresolvable groups.
func (c *PolicyTroubleshooterChecker) buildUnknownReason(
	resp *policytroubleshooterpb.TroubleshootIamPolicyResponse,
	principalID, targetSAEmail string,
) string {
	base := fmt.Sprintf("Policy Troubleshooter could not determine whether %s has %s on %s",
		principalID, store.PermissionActAs, targetSAEmail)

	// Scan allow-policy bindings for MEMBERSHIP_UNKNOWN_INFO, which typically
	// indicates an unresolvable group binding.
	var unknownGroups []string
	if allowExpl := resp.GetAllowPolicyExplanation(); allowExpl != nil {
		for _, policy := range allowExpl.GetExplainedPolicies() {
			for _, binding := range policy.GetBindingExplanations() {
				for member, annotation := range binding.GetMemberships() {
					if annotation == nil {
						continue
					}
					membership := annotation.GetMembership()
					if membership == policytroubleshooterpb.MembershipMatchingState_MEMBERSHIP_UNKNOWN_INFO ||
						membership == policytroubleshooterpb.MembershipMatchingState_MEMBERSHIP_UNKNOWN_UNSUPPORTED {
						if strings.HasPrefix(member, "group:") && len(unknownGroups) < 3 {
							unknownGroups = append(unknownGroups, member)
						}
					}
				}
			}
		}
	}

	if len(unknownGroups) > 0 {
		hint := fmt.Sprintf("; group binding for %s could not be resolved",
			strings.Join(unknownGroups, ", "))
		if c.hubSAEmail != "" {
			hint += fmt.Sprintf(" — the Hub SA (%s) may need Workspace groups.read access via domain-wide delegation",
				c.hubSAEmail)
		}
		return base + hint
	}

	if c.hubSAEmail != "" {
		return base + fmt.Sprintf("; the Hub SA (%s) may lack roles/iam.securityReviewer "+
			"on the project or organisation containing the target SA", c.hubSAEmail)
	}
	return base
}

// ptPrincipal returns the principal string for the PT v3 API.
// PT v3 expects bare email addresses, not IAM "type:email" form.
func ptPrincipal(principalID string) string {
	if idx := strings.Index(principalID, ":"); idx >= 0 {
		return principalID[idx+1:]
	}
	return principalID
}

// Compile-time assertion that PolicyTroubleshooterChecker satisfies the interface.
var _ store.CallerPermissionChecker = (*PolicyTroubleshooterChecker)(nil)
