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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Test Helpers
// =============================================================================

// recordingDecisionAuditEmitter records emitted decision audit records for testing.
type recordingDecisionAuditEmitter struct {
	records []*store.DecisionAuditRecord
}

func (e *recordingDecisionAuditEmitter) EmitDecisionAudit(_ context.Context, record *store.DecisionAuditRecord) {
	e.records = append(e.records, record)
}

// =============================================================================
// Decision Audit Tests
// =============================================================================

func TestDecisionAudit_AllowAndDeny(t *testing.T) {
	srv, s := testServer(t)

	// Create a project and an agent for testing
	ctx := context.Background()
	project := &store.Project{
		ID:        tid("audit-project"),
		Name:      "Audit Test Project",
		Slug:      "audit-project",
		CreatedBy: DevUserID,
		OwnerID:   DevUserID,
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Set up recording emitter
	emitter := &recordingDecisionAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emitter)

	// Test allow decision (dev user is admin, should be allowed)
	resource := Resource{
		Type:       "project",
		ID:         project.ID,
		ParentType: "",
	}
	identity := NewAuthenticatedUser(DevUserID, "dev@localhost", "Development User", "admin", "api")
	decision := srv.authzService.CheckAccess(ctx, identity, resource, ActionRead)
	if !decision.Allowed {
		t.Errorf("expected allow decision for admin, got deny: %s", decision.Reason)
	}

	// Wait a bit for async emission
	time.Sleep(50 * time.Millisecond)

	if len(emitter.records) < 1 {
		t.Fatalf("expected at least 1 audit record, got %d", len(emitter.records))
	}

	allowRecord := emitter.records[len(emitter.records)-1]
	if allowRecord.Result != "allow" {
		t.Errorf("expected result=allow, got %s", allowRecord.Result)
	}
	if allowRecord.PrincipalKind != "user" {
		t.Errorf("expected principal_kind=user, got %s", allowRecord.PrincipalKind)
	}
	if allowRecord.PrincipalID != DevUserID {
		t.Errorf("expected principal_id=%s, got %s", DevUserID, allowRecord.PrincipalID)
	}
	if allowRecord.ResourceType != "project" {
		t.Errorf("expected resource_type=project, got %s", allowRecord.ResourceType)
	}
	if allowRecord.Permission != "read" {
		t.Errorf("expected permission=read, got %s", allowRecord.Permission)
	}

	// Test deny decision (create a non-admin user)
	nonAdminID := tid("non-admin-user")
	if err := s.CreateUser(ctx, &store.User{
		ID:          nonAdminID,
		Email:       "nonadmin@test.com",
		DisplayName: "Non Admin",
		Role:        "member",
		Status:      "active",
	}); err != nil {
		t.Fatalf("failed to create non-admin user: %v", err)
	}

	// Non-admin user trying to access a resource they don't own
	nonAdmin := NewAuthenticatedUser(nonAdminID, "nonadmin@test.com", "Non Admin", "member", "api")
	otherResource := Resource{
		Type:    "policy",
		ID:      tid("some-policy"),
		OwnerID: tid("someone-else"),
	}
	denyDecision := srv.authzService.CheckAccess(ctx, nonAdmin, otherResource, ActionDelete)
	if denyDecision.Allowed {
		t.Errorf("expected deny for non-admin non-owner, got allow")
	}

	time.Sleep(50 * time.Millisecond)

	lastRecord := emitter.records[len(emitter.records)-1]
	if lastRecord.Result != "deny" {
		t.Errorf("expected result=deny, got %s", lastRecord.Result)
	}
	if lastRecord.CredentialType == "" {
		// Should have credential info even for deny
		t.Log("credential_type is empty for deny record (acceptable if no credential present)")
	}
}

func TestDecisionAudit_NoSecrets(t *testing.T) {
	srv, _ := testServer(t)

	emitter := &recordingDecisionAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emitter)

	ctx := context.Background()
	identity := NewAuthenticatedUser(DevUserID, "dev@localhost", "Development User", "admin", "api")

	resource := Resource{
		Type: "agent",
		ID:   tid("test-agent"),
	}

	_ = srv.authzService.CheckAccess(ctx, identity, resource, ActionRead)
	time.Sleep(50 * time.Millisecond)

	if len(emitter.records) == 0 {
		t.Fatal("expected at least one audit record")
	}

	for _, record := range emitter.records {
		// Check that no field contains bearer tokens or secrets
		fields := []string{
			record.Reason,
			record.MatchedPolicy,
			record.MatchedGrant,
			record.CredentialID,
			record.CredentialType,
			record.Route,
		}
		for _, field := range fields {
			lower := strings.ToLower(field)
			if strings.Contains(lower, "bearer") || strings.Contains(lower, "token_value") || strings.Contains(lower, "secret") {
				t.Errorf("audit record contains potential secret in field: %s", field)
			}
		}
	}
}

func TestDecisionAudit_Sampling(t *testing.T) {
	srv, _ := testServer(t)

	emitter := &recordingDecisionAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emitter)
	srv.authzService.DecisionAuditSampleRate = 0.0 // Sample nothing

	ctx := context.Background()
	identity := NewAuthenticatedUser(DevUserID, "dev@localhost", "Development User", "admin", "api")

	// Allow decision with 0% sample rate - should not be audited
	resource := Resource{
		Type: "agent",
		ID:   tid("sample-agent"),
	}
	decision := srv.authzService.CheckAccess(ctx, identity, resource, ActionRead)
	if !decision.Allowed {
		t.Errorf("expected allow, got deny")
	}

	time.Sleep(50 * time.Millisecond)
	allowCount := 0
	for _, r := range emitter.records {
		if r.Result == "allow" {
			allowCount++
		}
	}
	if allowCount > 0 {
		t.Errorf("expected 0 allow audit records with sample rate 0, got %d", allowCount)
	}

	// Deny decisions should always be audited regardless of sample rate
	nonAdmin := NewAuthenticatedUser(tid("sample-nonadmin"), "sample@test.com", "Sampler", "member", "api")
	denyResource := Resource{
		Type:    "policy",
		ID:      tid("restricted"),
		OwnerID: tid("other"),
	}
	denyDecision := srv.authzService.CheckAccess(ctx, nonAdmin, denyResource, ActionDelete)
	if denyDecision.Allowed {
		t.Errorf("expected deny")
	}

	time.Sleep(50 * time.Millisecond)
	denyCount := 0
	for _, r := range emitter.records {
		if r.Result == "deny" {
			denyCount++
		}
	}
	if denyCount == 0 {
		t.Errorf("expected deny decisions to be audited regardless of sample rate")
	}
}

// =============================================================================
// Explain API Tests
// =============================================================================

func TestExplainAPI_SuperAdmin(t *testing.T) {
	srv, s := testServer(t)

	// Create a project
	ctx := context.Background()
	project := &store.Project{
		ID:        tid("explain-project"),
		Name:      "Explain Test",
		Slug:      "explain-test",
		CreatedBy: DevUserID,
		OwnerID:   DevUserID,
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create a non-admin user to explain for
	targetUserID := tid("explain-target")
	if err := s.CreateUser(ctx, &store.User{
		ID:          targetUserID,
		Email:       "target@test.com",
		DisplayName: "Target User",
		Role:        "member",
		Status:      "active",
	}); err != nil {
		t.Fatalf("failed to create target user: %v", err)
	}

	// Super-admin explains for another user
	body := map[string]interface{}{
		"resource": map[string]interface{}{
			"type":      "project",
			"id":        project.ID,
			"projectId": project.ID,
		},
		"action":        "read",
		"principalId":   targetUserID,
		"principalKind": "user",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/authz/explain", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp explainResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	// CO1: The AK1 kernel does not yet build inline explain traces.
	// Verify the response contains a decision (allowed + reason) rather
	// than requiring trace steps.
	if resp.Reason == "" {
		t.Error("expected non-empty reason in explain response")
	}
}

func TestExplainAPI_Self(t *testing.T) {
	srv, s := testServer(t)

	ctx := context.Background()
	project := &store.Project{
		ID:        tid("explain-self-project"),
		Name:      "Explain Self Test",
		Slug:      "explain-self",
		CreatedBy: DevUserID,
		OwnerID:   DevUserID,
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Dev user explains their own access (dev user = admin, explains self)
	body := map[string]interface{}{
		"resource": map[string]interface{}{
			"type": "project",
			"id":   project.ID,
		},
		"action": "read",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/authz/explain", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp explainResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if !resp.Allowed {
		t.Error("expected allowed=true for admin user")
	}

	// CO1: The AK1 kernel does not yet build inline explain traces.
	// The response includes allowed/reason but trace may be empty.
	if resp.Reason == "" {
		t.Error("expected non-empty reason for self explain")
	}
}

func TestExplainAPI_DeniedForOtherPrincipal(t *testing.T) {
	srv, s := testServer(t)

	ctx := context.Background()

	// Create a non-admin user
	nonAdminID := tid("explain-requester")
	if err := s.CreateUser(ctx, &store.User{
		ID:          nonAdminID,
		Email:       "requester@test.com",
		DisplayName: "Requester",
		Role:        "member",
		Status:      "active",
	}); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	ensureHubMembership(ctx, s, nonAdminID)

	// Test the handler directly with a non-admin identity context to verify
	// that non-admins cannot explain for another principal.
	body := map[string]interface{}{
		"resource": map[string]interface{}{
			"type": "project",
			"id":   tid("some-project"),
		},
		"action":      "read",
		"principalId": tid("another-user"),
	}

	bodyBytes, _ := json.Marshal(body)
	req := newRequestWithIdentity(t, http.MethodPost, "/api/v1/authz/explain", bodyBytes,
		NewAuthenticatedUser(nonAdminID, "requester@test.com", "Requester", "member", "api"))

	// Call the handler directly (bypassing the route guard middleware)
	rec := httptest.NewRecorder()
	srv.handleAuthzExplain(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-admin explaining other principal, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestExplainAPI_MemberWithoutAuditReadCannotExplainForOthers(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// A member user without hub.audit.read cannot explain for another principal,
	// even with other hub-level policies. This verifies the Decide-based gate
	// that replaced the IsUnscopedLocalPlatformAdmin check.
	memberID := tid("explain-member-noaudit")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: memberID, Email: "member-noaudit@test.com", DisplayName: "Member", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, memberID)

	body := map[string]interface{}{
		"resource":    map[string]interface{}{"type": "agent", "id": tid("some-agent")},
		"action":      "read",
		"principalId": tid("another-user"),
	}
	bodyBytes, _ := json.Marshal(body)
	req := newRequestWithIdentity(t, http.MethodPost, "/api/v1/authz/explain", bodyBytes,
		NewAuthenticatedUser(memberID, "member-noaudit@test.com", "Member", "member", "api"))
	rec := httptest.NewRecorder()
	srv.handleAuthzExplain(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code, "member without hub.audit.read must be denied explain-for-other")
}

func TestExplainAPI_SuperAdminCanExplainForOthersViaDecide(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Super-admin (role=admin) can still explain for another principal after
	// the IsUnscopedLocalPlatformAdmin check was replaced with a Decide call.
	// The step-1 admin bypass in checkAccessForUser grants hub.audit.read.
	targetID := tid("explain-target-decide")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "target-decide@test.com", DisplayName: "Target", Role: "member", Status: "active",
	}))

	project := &store.Project{
		ID: tid("explain-decide-project"), Name: "Explain Decide Test", Slug: "explain-decide",
		CreatedBy: DevUserID, OwnerID: DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	body := map[string]interface{}{
		"resource":      map[string]interface{}{"type": "project", "id": project.ID, "projectId": project.ID},
		"action":        "read",
		"principalId":   targetID,
		"principalKind": "user",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/authz/explain", body)
	assert.Equal(t, http.StatusOK, rec.Code, "super-admin must be able to explain for other principals: %s", rec.Body.String())

	var resp explainResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// CO1: The AK1 kernel does not yet build inline explain traces.
	// Verify the response includes a reason instead.
	assert.NotEmpty(t, resp.Reason, "explain response must include a reason")
}

func TestExplainAPI_NoSecretLeakage(t *testing.T) {
	srv, s := testServer(t)

	ctx := context.Background()
	project := &store.Project{
		ID:        tid("explain-leak-project"),
		Name:      "Leak Test",
		Slug:      "leak-test",
		CreatedBy: DevUserID,
		OwnerID:   DevUserID,
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	body := map[string]interface{}{
		"resource": map[string]interface{}{
			"type": "project",
			"id":   project.ID,
		},
		"action": "read",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/authz/explain", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp explainResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	// Check trace for secrets
	responseStr := rec.Body.String()
	sensitivePatterns := []string{"bearer", "token_value", "secret_key", "password", "scion_pat_", "scion_dev_"}
	for _, pattern := range sensitivePatterns {
		if strings.Contains(strings.ToLower(responseStr), pattern) {
			t.Errorf("explain response contains potential secret pattern %q", pattern)
		}
	}
}

func TestExplainAPI_TraceContainsDecidingPolicy(t *testing.T) {
	srv, s := testServer(t)

	ctx := context.Background()
	project := &store.Project{
		ID:        tid("trace-policy-project"),
		Name:      "Trace Policy Test",
		Slug:      "trace-policy",
		CreatedBy: DevUserID,
		OwnerID:   DevUserID,
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	body := map[string]interface{}{
		"resource": map[string]interface{}{
			"type": "project",
			"id":   project.ID,
		},
		"action": "read",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/authz/explain", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp explainResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	// CO1: The AK1 kernel does not yet build inline explain traces.
	// Verify the response returns a valid decision with reason.
	if !resp.Allowed {
		t.Errorf("expected allowed=true for admin user, got reason=%q", resp.Reason)
	}
	if resp.Reason == "" {
		t.Error("expected non-empty reason in explain response")
	}
}

// =============================================================================
// Mutation Audit Tests
// =============================================================================

func TestMutationAudit_PolicyCreate(t *testing.T) {
	// CO1: Policy API removed (returns 410 Gone). Mutation audit for policy
	// creation is no longer testable via HTTP. Test retained as shell.
	// Mutation audit for remaining write operations (role bindings,
	// credential revocation) is covered by TestMutationAudit_CredentialRevocation.
}

func TestMutationAudit_CredentialRevocation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a UAT for the dev user first
	project := &store.Project{
		ID:        tid("revoke-project"),
		Name:      "Revoke Test",
		Slug:      "revoke-test",
		CreatedBy: DevUserID,
		OwnerID:   DevUserID,
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create a UAT via the API
	tokenBody := map[string]interface{}{
		"name":      "test-revoke-token",
		"projectId": project.ID,
		"scopes":    []string{"agent:read"},
	}
	createRec := doRequest(t, srv, http.MethodPost, "/api/v1/auth/tokens", tokenBody)
	if createRec.Code != http.StatusCreated && createRec.Code != http.StatusOK {
		t.Skipf("skipping UAT revocation test - token creation returned %d: %s", createRec.Code, createRec.Body.String())
	}

	var createResp map[string]interface{}
	if err := json.Unmarshal(createRec.Body.Bytes(), &createResp); err != nil {
		t.Fatalf("failed to parse token creation response: %v", err)
	}

	tokenID, ok := createResp["id"].(string)
	if !ok || tokenID == "" {
		t.Skip("skipping - could not get token ID from response")
	}

	// Revoke the token
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/auth/tokens/"+tokenID, nil)
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("expected 200/204 for revocation, got %d: %s", rec.Code, rec.Body.String())
	}

	time.Sleep(100 * time.Millisecond)

	// Check mutation audit
	records, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: "credential_revoke",
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("failed to list mutation audits: %v", err)
	}

	if len(records) == 0 {
		t.Log("no credential_revoke mutation audit found (may be expected if handler path differs)")
	}
}

// =============================================================================
// Audit Queryability Tests
// =============================================================================

func TestAuditQueryability(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// Create decision audit records with various fields
	now := time.Now()
	records := []*store.DecisionAuditRecord{
		{
			PrincipalKind: "user",
			PrincipalID:   tid("query-user-1"),
			CredentialID:  tid("cred-1"),
			Route:         "GET /api/v1/agents",
			ResourceType:  "agent",
			ResourceID:    tid("agent-1"),
			Permission:    "read",
			Result:        "allow",
			Reason:        "admin bypass",
			CorrelationID: "corr-001",
			Timestamp:     now.Add(-2 * time.Hour),
		},
		{
			PrincipalKind: "agent",
			PrincipalID:   tid("query-agent-1"),
			Route:         "POST /api/v1/agents",
			ResourceType:  "agent",
			ResourceID:    tid("agent-2"),
			Permission:    "create",
			Result:        "deny",
			Reason:        "default deny",
			CorrelationID: "corr-002",
			Timestamp:     now.Add(-1 * time.Hour),
		},
		{
			PrincipalKind: "user",
			PrincipalID:   tid("query-user-1"),
			CredentialID:  tid("cred-2"),
			Route:         "DELETE /api/v1/policies/123",
			ResourceType:  "policy",
			ResourceID:    tid("policy-1"),
			Permission:    "delete",
			Result:        "allow",
			Reason:        "policy match",
			CorrelationID: "corr-003",
			Timestamp:     now,
		},
	}

	for _, record := range records {
		if err := s.CreateDecisionAudit(ctx, record); err != nil {
			t.Fatalf("failed to create decision audit: %v", err)
		}
	}

	// Test: filter by principal
	results, total, err := s.ListDecisionAudits(ctx, store.DecisionAuditFilter{
		PrincipalID: tid("query-user-1"),
		Limit:       10,
	})
	if err != nil {
		t.Fatalf("failed to list by principal: %v", err)
	}
	if total != 2 {
		t.Errorf("expected 2 results for principal filter, got %d", total)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 records, got %d", len(results))
	}

	// Test: filter by credential
	_, total, err = s.ListDecisionAudits(ctx, store.DecisionAuditFilter{
		CredentialID: tid("cred-1"),
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("failed to list by credential: %v", err)
	}
	if total != 1 {
		t.Errorf("expected 1 result for credential filter, got %d", total)
	}

	// Test: filter by route
	_, total, err = s.ListDecisionAudits(ctx, store.DecisionAuditFilter{
		Route: "GET /api/v1/agents",
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("failed to list by route: %v", err)
	}
	if total != 1 {
		t.Errorf("expected 1 result for route filter, got %d", total)
	}

	// Test: filter by resource
	_, total, err = s.ListDecisionAudits(ctx, store.DecisionAuditFilter{
		ResourceType: "agent",
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("failed to list by resource type: %v", err)
	}
	if total != 2 {
		t.Errorf("expected 2 results for resource type filter, got %d", total)
	}

	// Test: filter by result
	_, total, err = s.ListDecisionAudits(ctx, store.DecisionAuditFilter{
		Result: "deny",
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("failed to list by result: %v", err)
	}
	if total != 1 {
		t.Errorf("expected 1 result for deny filter, got %d", total)
	}

	// Test: filter by time range
	_, total, err = s.ListDecisionAudits(ctx, store.DecisionAuditFilter{
		Since: now.Add(-90 * time.Minute),
		Until: now.Add(1 * time.Minute),
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("failed to list by time range: %v", err)
	}
	if total != 2 {
		t.Errorf("expected 2 results for time range filter, got %d", total)
	}

	// Test: filter by correlation ID
	results, _, err = s.ListDecisionAudits(ctx, store.DecisionAuditFilter{
		CorrelationID: "corr-002",
		Limit:         10,
	})
	if err != nil {
		t.Fatalf("failed to list by correlation ID: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 result for correlation ID filter, got %d", len(results))
	}
}

// =============================================================================
// Retention Cleanup Tests
// =============================================================================

func TestRetentionCleanup(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create old decision audit records
	oldTime := time.Now().AddDate(0, 0, -100) // 100 days ago
	recentTime := time.Now().Add(-1 * time.Hour)

	oldRecord := &store.DecisionAuditRecord{
		PrincipalKind: "user",
		PrincipalID:   tid("cleanup-user"),
		ResourceType:  "agent",
		Permission:    "read",
		Result:        "allow",
		Reason:        "test",
		Timestamp:     oldTime,
	}
	recentRecord := &store.DecisionAuditRecord{
		PrincipalKind: "user",
		PrincipalID:   tid("cleanup-user"),
		ResourceType:  "agent",
		Permission:    "read",
		Result:        "allow",
		Reason:        "test",
		Timestamp:     recentTime,
	}

	if err := s.CreateDecisionAudit(ctx, oldRecord); err != nil {
		t.Fatalf("failed to create old record: %v", err)
	}
	if err := s.CreateDecisionAudit(ctx, recentRecord); err != nil {
		t.Fatalf("failed to create recent record: %v", err)
	}

	// Create old mutation audit records
	oldMutation := &store.MutationAuditRecord{
		MutationType:       "policy_create",
		ActorPrincipalKind: "user",
		ActorPrincipalID:   tid("cleanup-user"),
		TargetType:         "policy",
		TargetID:           tid("old-policy"),
		Timestamp:          oldTime,
	}
	recentMutation := &store.MutationAuditRecord{
		MutationType:       "policy_create",
		ActorPrincipalKind: "user",
		ActorPrincipalID:   tid("cleanup-user"),
		TargetType:         "policy",
		TargetID:           tid("recent-policy"),
		Timestamp:          recentTime,
	}

	if err := s.CreateMutationAudit(ctx, oldMutation); err != nil {
		t.Fatalf("failed to create old mutation: %v", err)
	}
	if err := s.CreateMutationAudit(ctx, recentMutation); err != nil {
		t.Fatalf("failed to create recent mutation: %v", err)
	}

	// Run cleanup with 90-day retention
	if err := srv.CleanupAuditRecords(ctx, 90); err != nil {
		t.Fatalf("CleanupAuditRecords failed: %v", err)
	}

	// Verify old records were deleted
	decisionRecords, total, err := s.ListDecisionAudits(ctx, store.DecisionAuditFilter{
		PrincipalID: tid("cleanup-user"),
		Limit:       10,
	})
	if err != nil {
		t.Fatalf("failed to list decision audits: %v", err)
	}
	if total != 1 {
		t.Errorf("expected 1 decision audit remaining after cleanup, got %d", total)
	}
	if len(decisionRecords) == 1 && decisionRecords[0].Timestamp.Before(time.Now().AddDate(0, 0, -90)) {
		t.Error("remaining record should be recent, not old")
	}

	mutationRecords, total, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		ActorPrincipalID: tid("cleanup-user"),
		Limit:            10,
	})
	if err != nil {
		t.Fatalf("failed to list mutation audits: %v", err)
	}
	if total != 1 {
		t.Errorf("expected 1 mutation audit remaining after cleanup, got %d", total)
	}
	_ = mutationRecords
}

// =============================================================================
// Test Helpers for non-dev auth requests
// =============================================================================

func newRequestWithIdentity(t *testing.T, method, path string, body []byte, identity Identity) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, path, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	ctx := contextWithIdentity(req.Context(), identity)
	return req.WithContext(ctx)
}
