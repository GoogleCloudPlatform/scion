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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Project messaging policy tests (D3)
// ---------------------------------------------------------------------------

func TestCrossProjectInboundConstants(t *testing.T) {
	// Valid values.
	for _, v := range []string{"none", "members", "any"} {
		if !store.IsValidCrossProjectInbound(v) {
			t.Errorf("expected %q to be valid", v)
		}
	}
	// Invalid values.
	for _, v := range []string{"", "all", "invalid", "None"} {
		if store.IsValidCrossProjectInbound(v) {
			t.Errorf("expected %q to be invalid", v)
		}
	}
}

func TestProjectMessagingPolicy_DefaultsToNone(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:        tid("pmp-default"),
		Name:      "pmp-default",
		Slug:      "pmp-default",
		CreatedBy: "test",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require_NoError(t, s.CreateProject(ctx, project))

	// Read back.
	p, err := s.GetProject(ctx, project.ID)
	require_NoError(t, err)

	if p.CrossProjectInbound != store.CrossProjectInboundNone {
		t.Errorf("expected default CrossProjectInbound=%q, got %q",
			store.CrossProjectInboundNone, p.CrossProjectInbound)
	}
	if p.CrossProjectInboundRevision != 1 {
		t.Errorf("expected default revision=1, got %d", p.CrossProjectInboundRevision)
	}
}

func TestProjectMessagingPolicy_UpdateWithCAS(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:        tid("pmp-update"),
		Name:      "pmp-update",
		Slug:      "pmp-update",
		CreatedBy: "test",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require_NoError(t, s.CreateProject(ctx, project))

	// Update to "members".
	updated, err := s.UpdateProjectMessagingPolicy(ctx, project.ID, "members", 1)
	require_NoError(t, err)

	if updated.CrossProjectInbound != "members" {
		t.Errorf("expected policy=members, got %q", updated.CrossProjectInbound)
	}
	if updated.CrossProjectInboundRevision != 2 {
		t.Errorf("expected revision=2, got %d", updated.CrossProjectInboundRevision)
	}

	// Update to "any" with correct revision.
	updated, err = s.UpdateProjectMessagingPolicy(ctx, project.ID, "any", 2)
	require_NoError(t, err)
	if updated.CrossProjectInbound != "any" {
		t.Errorf("expected policy=any, got %q", updated.CrossProjectInbound)
	}
	if updated.CrossProjectInboundRevision != 3 {
		t.Errorf("expected revision=3, got %d", updated.CrossProjectInboundRevision)
	}
}

func TestProjectMessagingPolicy_RevisionConflict(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:        tid("pmp-conflict"),
		Name:      "pmp-conflict",
		Slug:      "pmp-conflict",
		CreatedBy: "test",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require_NoError(t, s.CreateProject(ctx, project))

	// Update with wrong revision should fail.
	_, err := s.UpdateProjectMessagingPolicy(ctx, project.ID, "members", 999)
	if err != store.ErrRevisionConflict {
		t.Errorf("expected ErrRevisionConflict, got %v", err)
	}
}

func TestProjectMessagingPolicy_NotFound(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	_, err := s.UpdateProjectMessagingPolicy(ctx, tid("pmp-nonexistent"), "members", 1)
	if err != store.ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestProjectMessagingPolicy_GenericUpdateDoesNotChangePolicy(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:        tid("pmp-bypass"),
		Name:      "pmp-bypass",
		Slug:      "pmp-bypass",
		CreatedBy: "test",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require_NoError(t, s.CreateProject(ctx, project))

	// Set policy to "members" via dedicated method.
	_, err := s.UpdateProjectMessagingPolicy(ctx, project.ID, "members", 1)
	require_NoError(t, err)

	// Generic UpdateProject should not reset the policy.
	p, err := s.GetProject(ctx, project.ID)
	require_NoError(t, err)
	p.Name = "pmp-bypass-updated"
	require_NoError(t, s.UpdateProject(ctx, p))

	// Read back and verify policy is unchanged.
	p, err = s.GetProject(ctx, project.ID)
	require_NoError(t, err)
	if p.CrossProjectInbound != "members" {
		t.Errorf("generic UpdateProject should not change policy; got %q", p.CrossProjectInbound)
	}
}

func TestProjectMessagingPolicy_InvalidValuesRejected(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:        tid("pmp-invalid"),
		Name:      "pmp-invalid",
		Slug:      "pmp-invalid",
		CreatedBy: "test",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require_NoError(t, s.CreateProject(ctx, project))

	// Attempt to set invalid inbound values.
	for _, invalid := range []string{"all", "public", "None", ""} {
		if store.IsValidCrossProjectInbound(invalid) {
			t.Errorf("expected %q to be invalid for cross_project_inbound", invalid)
		}
	}
}

func TestProjectMessagingPolicy_ModeStagedWhileDisabled(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:        tid("pmp-staged"),
		Name:      "pmp-staged",
		Slug:      "pmp-staged",
		CreatedBy: "test",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require_NoError(t, s.CreateProject(ctx, project))

	// Hub switch is off by default (no OperationalSettings in test env).
	// Set policy to "members" — should succeed even though effective will be "none".
	updated, err := s.UpdateProjectMessagingPolicy(ctx, project.ID, "members", 1)
	require_NoError(t, err)

	if updated.CrossProjectInbound != "members" {
		t.Errorf("expected configured=members even when hub disabled, got %q", updated.CrossProjectInbound)
	}

	// Change to "any" — should also succeed.
	updated, err = s.UpdateProjectMessagingPolicy(ctx, project.ID, "any", 2)
	require_NoError(t, err)
	if updated.CrossProjectInbound != "any" {
		t.Errorf("expected configured=any even when hub disabled, got %q", updated.CrossProjectInbound)
	}
}

func TestHubDisableLeavesStoredModesIntact(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	projectID := tid("hdl-project")
	project := &store.Project{
		ID:        projectID,
		Name:      "hdl-project",
		Slug:      "hdl-project",
		CreatedBy: "test",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require_NoError(t, s.CreateProject(ctx, project))

	// Create an agent with hub mode.
	agent := &store.Agent{
		ID:          tid("hdl-agent"),
		Name:        "hdl-agent",
		Slug:        "hdl-agent",
		ProjectID:   projectID,
		MessageMode: store.MessageModeHub,
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require_NoError(t, s.CreateAgent(ctx, agent))

	// Set project policy to "members".
	_, err := s.UpdateProjectMessagingPolicy(ctx, projectID, "members", 1)
	require_NoError(t, err)

	// Hub switch is OFF by default. Verify agent mode is still hub in the DB.
	a, err := s.GetAgent(ctx, agent.ID)
	require_NoError(t, err)
	if a.MessageMode != store.MessageModeHub {
		t.Errorf("hub disable should not change stored agent mode; got %q", a.MessageMode)
	}

	// Verify project policy is still "members" in the DB.
	p, err := s.GetProject(ctx, projectID)
	require_NoError(t, err)
	if p.CrossProjectInbound != "members" {
		t.Errorf("hub disable should not change stored policy; got %q", p.CrossProjectInbound)
	}
}

func TestMessageModeHub_ValidConstant(t *testing.T) {
	// Hub must be a valid message mode.
	if !store.IsValidMessageMode(store.MessageModeHub) {
		t.Error("MessageModeHub should be a valid message mode")
	}
	if store.MessageModeHub != "hub" {
		t.Errorf("MessageModeHub = %q, want %q", store.MessageModeHub, "hub")
	}
}

func TestBuildMessagingPolicyResponse_EffectiveState(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:        tid("pmp-effective"),
		Name:      "pmp-effective",
		Slug:      "pmp-effective",
		CreatedBy: "test",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require_NoError(t, s.CreateProject(ctx, project))

	// Set policy to "members".
	updated, err := s.UpdateProjectMessagingPolicy(ctx, project.ID, "members", 1)
	require_NoError(t, err)

	// Hub switch is off by default, so effective should be "none".
	resp := srv.buildMessagingPolicyResponse(updated)
	if resp.CrossProjectInbound != "members" {
		t.Errorf("configured should be 'members', got %q", resp.CrossProjectInbound)
	}
	if resp.EffectiveCrossProjectInbound != "none" {
		t.Errorf("effective should be 'none' when hub is off, got %q", resp.EffectiveCrossProjectInbound)
	}
	if resp.HubCrossProjectEnabled {
		t.Error("hub cross-project should be disabled by default")
	}
	if resp.Capabilities == nil || len(resp.Capabilities.CrossProjectConversationKinds) == 0 {
		t.Error("capabilities should list supported conversation kinds")
	}
}
