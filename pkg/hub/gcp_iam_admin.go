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
	"log/slog"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
	"google.golang.org/api/cloudresourcemanager/v1"
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
	SetIAMPolicy(ctx context.Context, saEmail string, member string, role string) error

	// AddProjectIAMBinding grants a role to a member at the GCP project level.
	// This is distinct from SetIAMPolicy because it operates on the PROJECT IAM
	// policy (different blast radius than SA-level IAM). Used to grant minted SAs
	// project-level roles like roles/aiplatform.user.
	AddProjectIAMBinding(ctx context.Context, projectID, member, role string) error
}

// IAMAdminClient implements GCPServiceAccountAdmin using the GCP IAM Admin API
// and the Cloud Resource Manager API (for project-level IAM bindings).
type IAMAdminClient struct {
	service    *iam.Service
	crmService *cloudresourcemanager.Service
}

// NewIAMAdminClient creates a new IAMAdminClient. It uses Application Default
// Credentials to authenticate with the IAM Admin API and Cloud Resource Manager API.
func NewIAMAdminClient(ctx context.Context, opts ...option.ClientOption) (*IAMAdminClient, error) {
	svc, err := iam.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating IAM admin service: %w", err)
	}
	crmSvc, err := cloudresourcemanager.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating Cloud Resource Manager service: %w", err)
	}
	return &IAMAdminClient{service: svc, crmService: crmSvc}, nil
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
	resource := "projects/-/serviceAccounts/" + saEmail

	// Get current policy
	policy, err := c.service.Projects.ServiceAccounts.GetIamPolicy(resource).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("getting IAM policy for %s: %w", saEmail, err)
	}

	// Add the binding
	found := false
	for _, binding := range policy.Bindings {
		if binding.Role == role {
			for _, m := range binding.Members {
				if m == member {
					found = true
					break
				}
			}
			if !found {
				binding.Members = append(binding.Members, member)
				found = true
			}
			break
		}
	}
	if !found {
		policy.Bindings = append(policy.Bindings, &iam.Binding{
			Role:    role,
			Members: []string{member},
		})
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

func (c *IAMAdminClient) AddProjectIAMBinding(ctx context.Context, projectID, member, role string) error {
	const maxRetries = 3

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Short backoff before retry: 250ms, 500ms.
			time.Sleep(time.Duration(attempt) * 250 * time.Millisecond)
		}

		err := c.addProjectIAMBindingOnce(ctx, projectID, member, role)
		if err == nil {
			return nil
		}
		// Retry on 409 Conflict (etag mismatch from concurrent modification).
		if strings.Contains(err.Error(), "409") || strings.Contains(err.Error(), "conflict") {
			lastErr = err
			continue
		}
		// Non-retryable error — return immediately.
		return err
	}
	return fmt.Errorf("AddProjectIAMBinding failed after %d attempts: %w", maxRetries, lastErr)
}

// addProjectIAMBindingOnce performs a single read-modify-write of the project
// IAM policy. Extracted for retry by AddProjectIAMBinding.
func (c *IAMAdminClient) addProjectIAMBindingOnce(ctx context.Context, projectID, member, role string) error {
	// Read-modify-write the project IAM policy.
	policy, err := c.crmService.Projects.GetIamPolicy(projectID,
		&cloudresourcemanager.GetIamPolicyRequest{}).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("getting project IAM policy for %s: %w", projectID, err)
	}

	// Add the binding — check if the role already exists.
	found := false
	for _, binding := range policy.Bindings {
		if binding.Role == role {
			for _, m := range binding.Members {
				if m == member {
					// Already bound — nothing to do.
					return nil
				}
			}
			binding.Members = append(binding.Members, member)
			found = true
			break
		}
	}
	if !found {
		policy.Bindings = append(policy.Bindings, &cloudresourcemanager.Binding{
			Role:    role,
			Members: []string{member},
		})
	}

	_, err = c.crmService.Projects.SetIamPolicy(projectID,
		&cloudresourcemanager.SetIamPolicyRequest{
			Policy: policy,
		}).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("setting project IAM policy for %s: %w", projectID, err)
	}
	return nil
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
	return strings.Contains(errStr, "does not exist") &&
		strings.Contains(errStr, "400")
}

// ResolveGCPProjectID returns the project ID from config or auto-detects it
// from the GCE metadata server.
func ResolveGCPProjectID(configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	projectID, err := metadata.ProjectIDWithContext(context.Background())
	if err != nil {
		return "", fmt.Errorf("auto-detecting GCP project ID from metadata server: %w", err)
	}
	return projectID, nil
}
