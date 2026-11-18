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
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
	"google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
)

// GCPServiceAccountAdmin provides operations for creating GCP service accounts
// and managing their IAM policies. Used by the Hub to mint new SAs in the Hub's
// own GCP project.
type GCPServiceAccountAdmin interface {
	// CreateServiceAccount creates a new service account in the given GCP project.
	// Returns the SA email and unique ID.
	CreateServiceAccount(ctx context.Context, projectID, accountID, displayName, description string) (email string, uniqueID string, err error)

	// DeleteServiceAccount deletes a service account by email. Used for
	// best-effort cleanup when a mint operation partially succeeds (SA
	// created but a required IAM mutation fails).
	DeleteServiceAccount(ctx context.Context, saEmail string) error

	// SetIAMPolicy grants a role to a member on a service account.
	// Used to grant roles/iam.serviceAccountTokenCreator to the Hub SA on minted SAs.
	// IAMAdminClient implements it as AddRoleMembers with one member.
	SetIAMPolicy(ctx context.Context, saEmail string, member string, role string) error

	// AddRoleMembers grants role to every member on a service account in
	// one get-modify-set of the account's IAM policy. The write carries the
	// etag of the policy read, so a concurrent change fails it with a
	// conflict (HTTP 409) rather than being overwritten. Members already
	// granted the role are left as they are. Used for the Workload Identity
	// bindings of a minted account (ptone/scion#3329 phase 5).
	AddRoleMembers(ctx context.Context, saEmail, role string, members []string) error

	// RoleMembers returns the members on unconditional bindings of role in
	// saEmail's resource-level IAM policy (projects/-/serviceAccounts/<email>).
	// Conditional bindings are excluded, matching AddRoleMembers. Used by the
	// per-account status view for minted accounts (ptone/scion#3329 phase 5, P4).
	RoleMembers(ctx context.Context, saEmail, role string) ([]string, error)
}

// IAMAdminClient implements GCPServiceAccountAdmin using the GCP IAM Admin API.
type IAMAdminClient struct {
	service *iam.Service
}

// NewIAMAdminClient creates a new IAMAdminClient. It uses Application Default
// Credentials to authenticate with the IAM Admin API.
func NewIAMAdminClient(ctx context.Context, opts ...option.ClientOption) (*IAMAdminClient, error) {
	svc, err := iam.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating IAM admin service: %w", err)
	}
	return &IAMAdminClient{service: svc}, nil
}

func (c *IAMAdminClient) CreateServiceAccount(ctx context.Context, projectID, accountID, displayName, description string) (string, string, error) {
	req := &iam.CreateServiceAccountRequest{
		AccountId: accountID,
		ServiceAccount: &iam.ServiceAccount{
			DisplayName: displayName,
			Description: description,
		},
	}

	sa, err := c.service.Projects.ServiceAccounts.Create("projects/"+projectID, req).Context(ctx).Do()
	if err != nil {
		return "", "", fmt.Errorf("creating service account %s in project %s: %w", accountID, projectID, err)
	}

	return sa.Email, sa.UniqueId, nil
}

func (c *IAMAdminClient) DeleteServiceAccount(ctx context.Context, saEmail string) error {
	resource := "projects/-/serviceAccounts/" + saEmail
	_, err := c.service.Projects.ServiceAccounts.Delete(resource).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("deleting service account %s: %w", saEmail, err)
	}
	return nil
}

func (c *IAMAdminClient) SetIAMPolicy(ctx context.Context, saEmail string, member string, role string) error {
	return c.AddRoleMembers(ctx, saEmail, role, []string{member})
}

func (c *IAMAdminClient) AddRoleMembers(ctx context.Context, saEmail, role string, members []string) error {
	resource := "projects/-/serviceAccounts/" + saEmail

	// Get current policy (with its etag, which the write below sends back).
	policy, err := c.service.Projects.ServiceAccounts.GetIamPolicy(resource).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("getting IAM policy for %s: %w", saEmail, err)
	}

	if !addPolicyRoleMembers(policy, role, members) {
		return nil
	}

	// Set the updated policy
	_, err = c.service.Projects.ServiceAccounts.SetIamPolicy(resource, &iam.SetIamPolicyRequest{
		Policy: policy,
	}).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("setting IAM policy for %s: %w", saEmail, err)
	}

	return nil
}

func (c *IAMAdminClient) RoleMembers(ctx context.Context, saEmail, role string) ([]string, error) {
	resource := "projects/-/serviceAccounts/" + saEmail
	policy, err := c.service.Projects.ServiceAccounts.GetIamPolicy(resource).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("getting IAM policy for %s: %w", saEmail, err)
	}
	return policyRoleMembers(policy, role), nil
}

// policyRoleMembers returns the members on unconditional bindings of role in
// policy. Conditional bindings are ignored, matching addPolicyRoleMembers.
func policyRoleMembers(policy *iam.Policy, role string) []string {
	if policy == nil {
		return nil
	}
	var out []string
	for _, b := range policy.Bindings {
		if b.Role == role && b.Condition == nil {
			out = append(out, b.Members...)
		}
	}
	return out
}

// addPolicyRoleMembers adds each of members to the unconditional binding of
// role in policy, creating the binding when there is none, and reports
// whether the policy changed. A conditional binding of the same role is
// never extended.
func addPolicyRoleMembers(policy *iam.Policy, role string, members []string) bool {
	var binding *iam.Binding
	for _, b := range policy.Bindings {
		if b.Role == role && b.Condition == nil {
			binding = b
			break
		}
	}
	changed := false
	for _, member := range members {
		if binding == nil {
			binding = &iam.Binding{Role: role}
			policy.Bindings = append(policy.Bindings, binding)
		}
		if slices.Contains(binding.Members, member) {
			continue
		}
		binding.Members = append(binding.Members, member)
		changed = true
	}
	return changed
}

// retryIAMGrant retries an IAM operation with exponential backoff to handle
// GCP eventual consistency after SA creation. Retries only on "does not exist"
// errors (400 badRequest), which indicate the SA has not propagated yet.
func retryIAMGrant(ctx context.Context, op func() error) error {
	delays := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	var lastErr error
	// Try once without delay first.
	if err := op(); err == nil {
		return nil
	} else {
		lastErr = err
	}
	for _, delay := range delays {
		// Only retry on "does not exist" / badRequest errors.
		if !isEventualConsistencyError(lastErr) {
			return lastErr
		}
		slog.Warn("IAM grant hit eventual consistency delay, retrying",
			"delay", delay, "error", lastErr)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := op(); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

// isEventualConsistencyError checks if the error is a GCP eventual consistency
// error (400 badRequest with "does not exist" message).
func isEventualConsistencyError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// A Workload Identity pool that does not exist is a permanent error
	// with the same status and wording; retrying it only adds delay.
	if isWorkloadIdentityPoolNotFoundError(err) {
		return false
	}
	return strings.Contains(errStr, "does not exist") &&
		strings.Contains(errStr, "400")
}

// onGCE is metadata.OnGCE; a variable so tests can replace it.
var onGCE = metadata.OnGCE

// ResolveGCPProjectID returns the project ID from config or, on GCP, auto-detects
// it from the metadata server. Off GCP the lookup would retry dial timeouts for
// about 13s, so it is skipped.
func ResolveGCPProjectID(configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	if !onGCE() {
		return "", errors.New("not running on GCP; set server.hub.gcp_project_id to use a GCP project")
	}
	projectID, err := metadata.ProjectIDWithContext(context.Background())
	if err != nil {
		return "", fmt.Errorf("auto-detecting GCP project ID from metadata server: %w", err)
	}
	return projectID, nil
}
