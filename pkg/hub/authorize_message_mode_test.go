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

// Characterization tests for the existing messaging mode matrix.
//
// Purpose: pin the current behavior of authorizeAgentMessage as a
// characterization test suite before adding the "hub" mode in Phase 1.
// These tests serve as a regression safety net — any change to the mode
// matrix must update this file explicitly.
//
// Coverage:
//   - All 16 same-project agent-to-agent mode pairs (§1)
//   - Self-message exception (§2)
//   - System-plane bypass (§3)
//   - Human piercing rules: ancestry, project owner, super-admin (§4)
//   - Full-agent mutation authority note (§5)
//   - Project confinement: cross-project sends denied (§6)
//   - Mode creation ceiling gap (§7)

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// §1: All 16 same-project agent-to-agent mode pairs
//
// The current mode matrix for distinct agents in the same project:
//
//   sender\target | none  | lineage | branch  | project
//   ------------- | ----- | ------- | ------- | -------
//   none          | DENY  | DENY    | DENY    | DENY
//   lineage       | DENY  | DENY    | DENY    | DENY
//   branch        | DENY  | DENY    | *       | DENY
//   project       | DENY  | DENY    | DENY    | ALLOW
//
// (*) branch-to-branch: ALLOW only for direct parent/child; DENY otherwise.
//
// Lineage-mode agents have ZERO agent-to-agent edges (D4). A lineage agent
// cannot message any other agent regardless of the target's mode.
// Branch-to-branch requires direct parent/child relationship.
// Only project-to-project allows unrestricted same-project agent messaging.
// ---------------------------------------------------------------------------

func TestCharacterization_ModeMatrix_AllPairs(t *testing.T) {
	srv, s, owner, _, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	modes := []string{
		store.MessageModeNone,
		store.MessageModeLineage,
		store.MessageModeBranch,
		store.MessageModeProject,
	}

	// Create TWO agents per mode (sender and target). All share the same
	// project and have owner as their ancestry root — no parent/child
	// relationship between them (except where we test branch specifically).
	senderAgents := make(map[string]*store.Agent)
	targetAgents := make(map[string]*store.Agent)
	for _, mode := range modes {
		senderAgents[mode] = msgAuthzAgent(t, s, "matrix-sender-"+mode, projectID, mode, []string{owner.ID})
		targetAgents[mode] = msgAuthzAgent(t, s, "matrix-target-"+mode, projectID, mode, []string{owner.ID})
	}

	// Define expected outcomes for all 16 pairs.
	type pair struct {
		sender, target string
		allowed        bool
	}
	tests := []pair{
		// none sender → all targets: DENY
		{"none", "none", false},
		{"none", "lineage", false},
		{"none", "branch", false},
		{"none", "project", false},

		// lineage sender → all targets: DENY (D4: zero agent edges)
		{"lineage", "none", false},
		{"lineage", "lineage", false},
		{"lineage", "branch", false},
		{"lineage", "project", false},

		// branch sender → all targets: DENY (no parent/child here)
		{"branch", "none", false},
		{"branch", "lineage", false},
		{"branch", "branch", false}, // same mode but no parent/child
		{"branch", "project", false},

		// project sender → targets: only project→project ALLOW
		{"project", "none", false},
		{"project", "lineage", false},
		{"project", "branch", false},
		{"project", "project", true},
	}

	for _, tt := range tests {
		name := tt.sender + "→" + tt.target
		t.Run(name, func(t *testing.T) {
			senderAgent := senderAgents[tt.sender]
			targetAgent := targetAgents[tt.target]
			senderIdent := msgAuthzAgentIdentity(senderAgent.ID, projectID, senderAgent.Ancestry)

			allowed, reason, _ := srv.authorizeAgentMessage(ctx, senderIdent, targetAgent, false)
			if allowed != tt.allowed {
				t.Fatalf("expected allowed=%v for %s, got allowed=%v (reason: %s)",
					tt.allowed, name, allowed, reason)
			}
		})
	}
}

// TestCharacterization_BranchParentChild verifies that branch-to-branch
// messaging is allowed ONLY for direct parent/child relationships.
func TestCharacterization_BranchParentChild(t *testing.T) {
	srv, s, owner, _, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	parent := msgAuthzAgent(t, s, "char-branch-parent", projectID, store.MessageModeBranch,
		[]string{owner.ID})
	child := msgAuthzAgent(t, s, "char-branch-child", projectID, store.MessageModeBranch,
		[]string{owner.ID, parent.ID})
	grandchild := msgAuthzAgent(t, s, "char-branch-grandchild", projectID, store.MessageModeBranch,
		[]string{owner.ID, parent.ID, child.ID})
	sibling := msgAuthzAgent(t, s, "char-branch-sibling", projectID, store.MessageModeBranch,
		[]string{owner.ID, parent.ID})

	tests := []struct {
		name    string
		sender  *store.Agent
		target  *store.Agent
		allowed bool
	}{
		{"parent→child", parent, child, true},
		{"child→parent", child, parent, true},
		{"child→grandchild", child, grandchild, true},
		{"grandchild→child", grandchild, child, true},
		{"parent→grandchild (not direct)", parent, grandchild, false},
		{"grandchild→parent (not direct)", grandchild, parent, false},
		{"sibling→child", sibling, child, false},
		{"child→sibling", child, sibling, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			senderIdent := msgAuthzAgentIdentity(tt.sender.ID, projectID, tt.sender.Ancestry)
			allowed, reason, _ := srv.authorizeAgentMessage(ctx, senderIdent, tt.target, false)
			if allowed != tt.allowed {
				t.Fatalf("expected allowed=%v, got allowed=%v (reason: %s)",
					tt.allowed, allowed, reason)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// §2: Self-message exception
//
// An agent can deliver to itself regardless of mode. This is NOT system-plane;
// it is a self-access exemption for harness integration.
// ---------------------------------------------------------------------------

func TestCharacterization_SelfMessage(t *testing.T) {
	srv, s, owner, _, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	modes := []string{
		store.MessageModeNone,
		store.MessageModeLineage,
		store.MessageModeBranch,
		store.MessageModeProject,
	}

	for _, mode := range modes {
		t.Run("self-message-"+mode, func(t *testing.T) {
			agent := msgAuthzAgent(t, s, "self-"+mode, projectID, mode, []string{owner.ID})
			selfIdent := msgAuthzAgentIdentity(agent.ID, projectID, agent.Ancestry)

			allowed, reason, _ := srv.authorizeAgentMessage(ctx, selfIdent, agent, false)
			if !allowed {
				t.Fatalf("self-message should be allowed for mode %s: %s", mode, reason)
			}
			if reason != "agent self-message" {
				t.Fatalf("expected reason 'agent self-message', got %q", reason)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// §3: System-plane bypass (D8)
//
// System-plane messages bypass all mode checks. This is for hub-internal
// system messages (sciontool self-messages, state-change notices).
// ---------------------------------------------------------------------------

func TestCharacterization_SystemPlaneBypass(t *testing.T) {
	srv, s, owner, _, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	modes := []string{
		store.MessageModeNone,
		store.MessageModeLineage,
		store.MessageModeBranch,
		store.MessageModeProject,
	}

	for _, mode := range modes {
		t.Run("system-plane-"+mode, func(t *testing.T) {
			target := msgAuthzAgent(t, s, "sys-target-"+mode, projectID, mode, []string{owner.ID})
			sender := msgAuthzAgent(t, s, "sys-sender-"+mode, projectID, store.MessageModeProject, []string{owner.ID})
			senderIdent := msgAuthzAgentIdentity(sender.ID, projectID, sender.Ancestry)

			allowed, reason, _ := srv.authorizeAgentMessage(ctx, senderIdent, target, true)
			if !allowed {
				t.Fatalf("system-plane should bypass mode check for %s: %s", mode, reason)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// §4: Human piercing rules
//
// - Super-admin (D6): pierces everything including none.
// - Ancestry user: allowed for lineage, branch, project; denied for none.
// - Project owner: pierces lineage, branch; denied for none.
// - Non-lineage project member: denied for lineage, branch; depends on
//   agent.message permission for project.
// - UAT without agent:message: denied piercing even if user is in ancestry.
// - Agents NEVER inherit piercing from their origin user (D6 pinning).
// ---------------------------------------------------------------------------

func TestCharacterization_HumanPiercing(t *testing.T) {
	srv, s, owner, member, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	noneAgent := msgAuthzAgent(t, s, "hp-none", projectID, store.MessageModeNone, []string{owner.ID})
	lineageAgent := msgAuthzAgent(t, s, "hp-lineage", projectID, store.MessageModeLineage, []string{owner.ID})
	branchAgent := msgAuthzAgent(t, s, "hp-branch", projectID, store.MessageModeBranch, []string{owner.ID})
	projectAgent := msgAuthzAgent(t, s, "hp-project", projectID, store.MessageModeProject, []string{owner.ID})

	ownerIdent := msgAuthzUserIdentity(owner.ID)
	memberIdent := msgAuthzUserIdentity(member.ID)
	adminIdent := msgAuthzAdminIdentity()

	tests := []struct {
		name    string
		sender  Identity
		target  *store.Agent
		allowed bool
	}{
		// Super-admin pierces everything, including none
		{"admin→none", adminIdent, noneAgent, true},
		{"admin→lineage", adminIdent, lineageAgent, true},
		{"admin→branch", adminIdent, branchAgent, true},
		{"admin→project", adminIdent, projectAgent, true},

		// Ancestry user (owner is in ancestry)
		{"ancestry-owner→none", ownerIdent, noneAgent, false},      // none blocks even ancestry
		{"ancestry-owner→lineage", ownerIdent, lineageAgent, true}, // ancestry pierces lineage
		{"ancestry-owner→branch", ownerIdent, branchAgent, true},   // ancestry pierces branch
		{"ancestry-owner→project", ownerIdent, projectAgent, true}, // ancestry for project

		// Non-lineage project member
		{"member→none", memberIdent, noneAgent, false},
		{"member→lineage", memberIdent, lineageAgent, false},
		{"member→branch", memberIdent, branchAgent, false},
		{"member→project", memberIdent, projectAgent, false}, // no agent.message permission
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed, reason, _ := srv.authorizeAgentMessage(ctx, tt.sender, tt.target, false)
			if allowed != tt.allowed {
				t.Fatalf("expected allowed=%v, got allowed=%v (reason: %s)",
					tt.allowed, allowed, reason)
			}
		})
	}
}

// TestCharacterization_AgentNeverPiercesMode verifies that agents NEVER
// inherit piercing from their origin user, even if the origin user is a
// super-admin or project owner (D6 pinning rule).
func TestCharacterization_AgentNeverPiercesMode(t *testing.T) {
	srv, s, owner, _, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	lineageTarget := msgAuthzAgent(t, s, "pierce-lineage", projectID, store.MessageModeLineage, []string{owner.ID})
	branchTarget := msgAuthzAgent(t, s, "pierce-branch", projectID, store.MessageModeBranch, []string{owner.ID})

	// Owner's project-mode agent — should NOT inherit owner's piercing ability
	ownerAgent := msgAuthzAgent(t, s, "owner-agent-pierce", projectID, store.MessageModeProject, []string{owner.ID})
	agentIdent := msgAuthzAgentIdentity(ownerAgent.ID, projectID, ownerAgent.Ancestry)

	t.Run("owner agent cannot pierce lineage", func(t *testing.T) {
		allowed, _, _ := srv.authorizeAgentMessage(ctx, agentIdent, lineageTarget, false)
		if allowed {
			t.Fatal("owner's agent should NOT pierce lineage mode")
		}
	})

	t.Run("owner agent cannot pierce branch", func(t *testing.T) {
		allowed, _, _ := srv.authorizeAgentMessage(ctx, agentIdent, branchTarget, false)
		if allowed {
			t.Fatal("owner's agent should NOT pierce branch mode")
		}
	})
}

// ---------------------------------------------------------------------------
// §5: Full-agent mutation authority
//
// An agent with full role and ScopeAgentLifecycle can change the message mode
// of other agents via set_message_mode. However, the message mode itself
// has no creation/mutation ceiling — this is tested to document the current
// behavior and the gap that the hub-mode grant guard will fill in Phase 1.
//
// Note: this is an assertion about authorization behavior, not a functional
// test of the handler. The handler tests are in authorize_message_test.go.
// ---------------------------------------------------------------------------

func TestCharacterization_FullAgentMutationAuthority(t *testing.T) {
	srv, s, owner, _, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	// A full-role agent can message project-mode agents in its project.
	fullAgent := msgAuthzAgent(t, s, "full-agent", projectID, store.MessageModeProject, []string{owner.ID})
	target := msgAuthzAgent(t, s, "full-target", projectID, store.MessageModeProject, []string{owner.ID})

	fullIdent := msgAuthzAgentIdentity(fullAgent.ID, projectID, fullAgent.Ancestry, ScopeAgentLifecycle)

	allowed, reason, _ := srv.authorizeAgentMessage(ctx, fullIdent, target, false)
	if !allowed {
		t.Fatalf("full-role agent should message same-project project-mode target: %s", reason)
	}
}

// ---------------------------------------------------------------------------
// §6: Project confinement
//
// Cross-project agent-to-agent messaging is unconditionally denied.
// This is the existing invariant that Phase 1+ will selectively relax.
// ---------------------------------------------------------------------------

func TestCharacterization_ProjectConfinement(t *testing.T) {
	srv, s, owner, _, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	otherProjectID := tid("char-other-project")
	otherProject := &store.Project{
		ID:        otherProjectID,
		Name:      "other-project",
		Slug:      "other-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require_NoError(t, s.CreateProject(ctx, otherProject))

	modes := []string{
		store.MessageModeNone,
		store.MessageModeLineage,
		store.MessageModeBranch,
		store.MessageModeProject,
	}

	// Test all 16 cross-project mode pairs — all must be denied.
	for _, senderMode := range modes {
		for _, targetMode := range modes {
			name := senderMode + "→" + targetMode + "_cross"
			t.Run(name, func(t *testing.T) {
				sender := msgAuthzAgent(t, s, "xp-sender-"+senderMode+"-"+targetMode,
					projectID, senderMode, []string{owner.ID})
				target := msgAuthzAgent(t, s, "xp-target-"+senderMode+"-"+targetMode,
					otherProjectID, targetMode, []string{owner.ID})

				senderIdent := msgAuthzAgentIdentity(sender.ID, projectID, sender.Ancestry)
				allowed, _, _ := srv.authorizeAgentMessage(ctx, senderIdent, target, false)
				if allowed {
					t.Fatalf("cross-project messaging should be denied for %s", name)
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// §7: Mode creation/mutation ceiling gap
//
// IMPORTANT: Role creation has a ceiling (CompareRoles/minRole in
// createAgentInProject), but messaging-mode creation and mutation currently
// do NOT have such a ceiling.
//
// Specifically:
//   - An agent creator in branch mode can select project mode for a child
//     (there is no mode ceiling blocking it).
//   - handleSetMessageMode checks full-role scope and same-project target,
//     but does NOT compare the caller's own message mode against the
//     requested mode.
//   - The cascade helper assigns the requested mode to descendants without
//     a caller-mode comparison.
//
// This gap is documented here as a characterization. Phase 1 will introduce
// a hub-mode grant guard (AuthorizeMessageModeGrant) to enforce that only
// full-role, already-hub-mode agents can grant hub mode.
// ---------------------------------------------------------------------------

func TestCharacterization_ModeCeilingGap_Documented(t *testing.T) {
	// This test is intentionally a documentation marker, not a functional test.
	// It records the finding from the investigation (Section 4, follow-up):
	//
	// 1. createAgentInProject (lines ~642-685) reads the parent's stored ROLE
	//    and rejects an explicitly requested higher role (CompareRoles/minRole).
	//    There is NO analogous parent-versus-requested message-mode comparison.
	//
	// 2. handleSetMessageMode (lines ~127-151) checks full-role scope and
	//    same-project target, then authorizes the agent branch. It does NOT
	//    load/compare the caller's own message mode.
	//
	// 3. The cascade helper assigns the requested mode to descendants without
	//    a caller-mode comparison.
	//
	// 4. authorize.go:authorizeAgentCreate checks create scope and caller
	//    project; its signature does not receive the resolved child mode.
	//
	// Therefore: an authorized agent creator in branch mode CAN select a valid
	// project mode explicitly or through a template; the observed code contains
	// no mode ceiling blocking it.
	//
	// The new hub privilege MUST introduce a shared non-escalation check
	// (AuthorizeMessageModeGrant) rather than assuming one exists.
	t.Log("DOCUMENTED: messaging-mode creation/mutation has no ceiling. " +
		"Role creation has CompareRoles/minRole; messaging modes do not. " +
		"Phase 1 must introduce AuthorizeMessageModeGrant for the hub privilege.")
}
