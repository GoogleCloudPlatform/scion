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

	modes := store.MessageModes()

	// Create TWO agents per mode (sender and target). All share the same
	// project and have owner as their ancestry root — distinct trees and no
	// parent/child relationship between them.
	senderAgents := make(map[string]*store.Agent)
	targetAgents := make(map[string]*store.Agent)
	for _, mode := range modes {
		senderAgents[mode] = msgAuthzAgent(t, s, "matrix-sender-"+mode, projectID, mode, []string{owner.ID})
		targetAgents[mode] = msgAuthzAgent(t, s, "matrix-target-"+mode, projectID, mode, []string{owner.ID})
	}

	// Define expected outcomes and denial codes for all 36 unrelated pairs.
	type pair struct {
		sender, target string
		allowed        bool
		wantCode       string
	}
	tests := []pair{
		// none sender → all targets: DENY (mode_none_sender)
		{"none", "none", false, ReasonModeNoneSender},
		{"none", "owner", false, ReasonModeNoneSender},
		{"none", "direct", false, ReasonModeNoneSender},
		{"none", "tree", false, ReasonModeNoneSender},
		{"none", "project", false, ReasonModeNoneSender},
		{"none", "hub", false, ReasonModeNoneSender},

		// owner sender → all targets: DENY
		{"owner", "none", false, ReasonModeNone},
		{"owner", "owner", false, ReasonModeIncompatible},
		{"owner", "direct", false, ReasonModeIncompatible},
		{"owner", "tree", false, ReasonModeIncompatible},
		{"owner", "project", false, ReasonModeIncompatible},
		{"owner", "hub", false, ReasonModeIncompatible},

		// direct sender → all targets: DENY (no parent/child, distinct trees)
		{"direct", "none", false, ReasonModeNone},
		{"direct", "owner", false, ReasonModeIncompatible},
		{"direct", "direct", false, ReasonModeDirectNoEdge},
		{"direct", "tree", false, ReasonModeTreeOutside},
		{"direct", "project", false, ReasonModeIncompatible},
		{"direct", "hub", false, ReasonModeIncompatible},

		// tree sender → all targets: DENY (distinct trees)
		{"tree", "none", false, ReasonModeNone},
		{"tree", "owner", false, ReasonModeIncompatible},
		{"tree", "direct", false, ReasonModeTreeOutside},
		{"tree", "tree", false, ReasonModeTreeOutside},
		{"tree", "project", false, ReasonModeTreeOutside},
		{"tree", "hub", false, ReasonModeTreeOutside},

		// project sender → targets: only project/hub ALLOW (distinct trees)
		{"project", "none", false, ReasonModeNone},
		{"project", "owner", false, ReasonModeIncompatible},
		{"project", "direct", false, ReasonModeIncompatible},
		{"project", "tree", false, ReasonModeTreeOutside},
		{"project", "project", true, ""},
		{"project", "hub", true, ""},

		// hub sender → targets: only project/hub ALLOW (distinct trees)
		{"hub", "none", false, ReasonModeNone},
		{"hub", "owner", false, ReasonModeIncompatible},
		{"hub", "direct", false, ReasonModeIncompatible},
		{"hub", "tree", false, ReasonModeTreeOutside},
		{"hub", "project", true, ""},
		{"hub", "hub", true, ""},
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
			if !tt.allowed {
				if got := mapReasonToCode(reason); got != tt.wantCode {
					t.Fatalf("expected code %q for %s, got %q (reason: %s)", tt.wantCode, name, got, reason)
				}
			}
		})
	}
}

// TestCharacterization_DirectParentChild verifies that direct-to-direct
// messaging is allowed ONLY for direct parent/child relationships.
func TestCharacterization_DirectParentChild(t *testing.T) {
	srv, s, owner, _, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	parent := msgAuthzAgent(t, s, "char-direct-parent", projectID, store.MessageModeDirect,
		[]string{owner.ID})
	child := msgAuthzAgent(t, s, "char-direct-child", projectID, store.MessageModeDirect,
		[]string{owner.ID, parent.ID})
	grandchild := msgAuthzAgent(t, s, "char-direct-grandchild", projectID, store.MessageModeDirect,
		[]string{owner.ID, parent.ID, child.ID})
	sibling := msgAuthzAgent(t, s, "char-direct-sibling", projectID, store.MessageModeDirect,
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
			if !tt.allowed {
				if got := mapReasonToCode(reason); got != ReasonModeDirectNoEdge {
					t.Fatalf("expected code %q, got %q (reason: %s)", ReasonModeDirectNoEdge, got, reason)
				}
			}
		})
	}
}

// TestTreeMode_MatrixAndSymmetry covers tree membership, symmetric tree bond,
// deleted intermediates, empty ancestry, separate trees under the same owner,
// and the full symmetry property allow(S→T) == allow(T→S).
func TestTreeMode_MatrixAndSymmetry(t *testing.T) {
	srv, s, owner, member, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	// Tree 1: U -> Foo -> Bar -> Baz; Foo -> Bar2
	foo := msgAuthzAgent(t, s, "tree-foo", projectID, store.MessageModeTree, []string{owner.ID})
	bar := msgAuthzAgent(t, s, "tree-bar", projectID, store.MessageModeTree, []string{owner.ID, foo.ID})
	baz := msgAuthzAgent(t, s, "tree-baz", projectID, store.MessageModeTree, []string{owner.ID, foo.ID, bar.ID})
	bar2 := msgAuthzAgent(t, s, "tree-bar2", projectID, store.MessageModeTree, []string{owner.ID, foo.ID})

	// Tree 2 (same owner, different root): U -> Foo2 -> Qux
	foo2 := msgAuthzAgent(t, s, "tree-foo2", projectID, store.MessageModeTree, []string{owner.ID})
	qux := msgAuthzAgent(t, s, "tree-qux", projectID, store.MessageModeTree, []string{owner.ID, foo2.ID})

	// Legacy / empty ancestry agent
	emptyAnc := msgAuthzAgent(t, s, "tree-empty", projectID, store.MessageModeTree, nil)

	check := func(t *testing.T, sAgent, tAgent *store.Agent, want bool, wantCode string) {
		t.Helper()
		sIdent := msgAuthzAgentIdentity(sAgent.ID, projectID, sAgent.Ancestry)
		got, reason, _ := srv.authorizeAgentMessage(ctx, sIdent, tAgent, false)
		if got != want {
			t.Fatalf("%s(%s)→%s(%s): got allowed=%v, want %v (reason: %s)",
				sAgent.Name, sAgent.MessageMode, tAgent.Name, tAgent.MessageMode, got, want, reason)
		}
		if !want && wantCode != "" {
			if code := mapReasonToCode(reason); code != wantCode {
				t.Fatalf("%s→%s: got code %q, want %q (reason: %s)", sAgent.Name, tAgent.Name, code, wantCode, reason)
			}
		}
	}

	t.Run("same tree descendants and cousins allowed", func(t *testing.T) {
		check(t, foo, baz, true, "")
		check(t, baz, foo, true, "")
		check(t, bar, baz, true, "")
		check(t, baz, bar, true, "")
		check(t, baz, bar2, true, "")
		check(t, bar2, baz, true, "")
	})

	t.Run("separate trees under same owner and empty ancestry denied", func(t *testing.T) {
		check(t, foo, foo2, false, ReasonModeTreeOutside)
		check(t, foo2, foo, false, ReasonModeTreeOutside)
		check(t, baz, qux, false, ReasonModeTreeOutside)
		check(t, qux, baz, false, ReasonModeTreeOutside)
		check(t, emptyAnc, foo, false, ReasonModeTreeOutside)
		check(t, foo, emptyAnc, false, ReasonModeTreeOutside)
	})

	t.Run("symmetric tree bond with project, hub, direct, owner", func(t *testing.T) {
		// project root Foo ↔ tree Baz: allowed both directions
		foo.MessageMode = store.MessageModeProject
		require_NoError(t, s.UpdateAgent(ctx, foo))
		check(t, foo, baz, true, "")
		check(t, baz, foo, true, "")

		// project agent outside tree (Foo2) ↔ tree Baz: denied (mode_tree_outside)
		foo2.MessageMode = store.MessageModeProject
		require_NoError(t, s.UpdateAgent(ctx, foo2))
		check(t, foo2, baz, false, ReasonModeTreeOutside)
		check(t, baz, foo2, false, ReasonModeTreeOutside)

		// hub root Foo ↔ tree Baz: allowed both directions
		foo.MessageMode = store.MessageModeHub
		require_NoError(t, s.UpdateAgent(ctx, foo))
		check(t, foo, baz, true, "")
		check(t, baz, foo, true, "")

		// direct Bar ↔ tree Baz (parent/child): allowed; direct Bar ↔ tree Bar2 (siblings): denied (mode_direct_no_edge)
		bar.MessageMode = store.MessageModeDirect
		require_NoError(t, s.UpdateAgent(ctx, bar))
		check(t, bar, baz, true, "")
		check(t, baz, bar, true, "")
		check(t, bar, bar2, false, ReasonModeDirectNoEdge)
		check(t, bar2, bar, false, ReasonModeDirectNoEdge)
		// direct Bar ↔ tree Qux (different tree): denied (mode_tree_outside)
		check(t, bar, qux, false, ReasonModeTreeOutside)
		check(t, qux, bar, false, ReasonModeTreeOutside)

		// owner Foo ↔ tree Baz: denied both directions (mode_incompatible)
		foo.MessageMode = store.MessageModeOwner
		require_NoError(t, s.UpdateAgent(ctx, foo))
		check(t, foo, baz, false, ReasonModeIncompatible)
		check(t, baz, foo, false, ReasonModeIncompatible)

		// Restore modes for subsequent subtests
		foo.MessageMode = store.MessageModeTree
		bar.MessageMode = store.MessageModeTree
		foo2.MessageMode = store.MessageModeTree
		require_NoError(t, s.UpdateAgent(ctx, foo))
		require_NoError(t, s.UpdateAgent(ctx, bar))
		require_NoError(t, s.UpdateAgent(ctx, foo2))
	})

	t.Run("user path for tree mode", func(t *testing.T) {
		ownerIdent := msgAuthzUserIdentity(owner.ID)
		memberIdent := msgAuthzUserIdentity(member.ID)
		adminIdent := msgAuthzAdminIdentity()

		allowed, reason, _ := srv.authorizeAgentMessage(ctx, ownerIdent, baz, false)
		if !allowed {
			t.Fatalf("owner→tree should be allowed: %s", reason)
		}
		allowed, reason, _ = srv.authorizeAgentMessage(ctx, memberIdent, baz, false)
		if allowed {
			t.Fatalf("non-owner member→tree should be denied")
		}
		if code := mapReasonToCode(reason); code != ReasonModeOwnerOnly {
			t.Fatalf("member→tree: got code %q, want %q (reason: %s)", code, ReasonModeOwnerOnly, reason)
		}
		allowed, reason, _ = srv.authorizeAgentMessage(ctx, adminIdent, baz, false)
		if !allowed {
			t.Fatalf("super-admin→tree should be allowed: %s", reason)
		}

		// Isolated project-owner piercing of tree: target's Ancestry[0] is member,
		// not owner, so only project-owner piercing (not ancestry allow) admits owner.
		memberTree := msgAuthzAgent(t, s, "tree-member-owned", projectID, store.MessageModeTree, []string{member.ID})
		allowed, reason, _ = srv.authorizeAgentMessage(ctx, ownerIdent, memberTree, false)
		if !allowed || reason != "project owner piercing" {
			t.Fatalf("project owner→member-owned tree: got allowed=%v reason=%q, want allowed=true reason='project owner piercing'", allowed, reason)
		}
	})

	t.Run("symmetry property over all modes and pairs", func(t *testing.T) {
		agents := []*store.Agent{foo, bar, baz, bar2, foo2, qux, emptyAnc}
		for _, mA := range store.MessageModes() {
			for _, mB := range store.MessageModes() {
				for i := 0; i < len(agents); i++ {
					for j := i + 1; j < len(agents); j++ {
						a := *agents[i]
						b := *agents[j]
						a.MessageMode = mA
						b.MessageMode = mB
						dAB := srv.evaluateSameProjectModes(&a, &b)
						dBA := srv.evaluateSameProjectModes(&b, &a)
						if dAB.Allowed != dBA.Allowed {
							t.Fatalf("asymmetry: %s(%s)→%s(%s)=%v vs reverse=%v",
								a.Name, mA, b.Name, mB, dAB.Allowed, dBA.Allowed)
						}
					}
				}
			}
		}
	})

	t.Run("soft-deleted intermediate keeps descendants connected, sender denied by standing", func(t *testing.T) {
		bar.DeletedAt = time.Now().UTC()
		require_NoError(t, s.UpdateAgent(ctx, bar))

		// Baz ↔ Bar2 still allowed via immutable Ancestry
		check(t, baz, bar2, true, "")
		check(t, bar2, baz, true, "")

		// Soft-deleted Bar cannot send (standing check fails)
		check(t, bar, baz, false, "")
	})
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

	modes := store.MessageModes()

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

	modes := store.MessageModes()

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
	ownerAgent := msgAuthzAgent(t, s, "hp-owner", projectID, store.MessageModeOwner, []string{owner.ID})
	directAgent := msgAuthzAgent(t, s, "hp-direct", projectID, store.MessageModeDirect, []string{owner.ID})
	treeAgent := msgAuthzAgent(t, s, "hp-tree", projectID, store.MessageModeTree, []string{owner.ID})
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
		{"admin→owner", adminIdent, ownerAgent, true},
		{"admin→direct", adminIdent, directAgent, true},
		{"admin→tree", adminIdent, treeAgent, true},
		{"admin→project", adminIdent, projectAgent, true},

		// Ancestry user (owner is in ancestry)
		{"ancestry-owner→none", ownerIdent, noneAgent, false},      // none blocks even ancestry
		{"ancestry-owner→owner", ownerIdent, ownerAgent, true},     // ancestry pierces owner
		{"ancestry-owner→direct", ownerIdent, directAgent, true},   // ancestry pierces direct
		{"ancestry-owner→tree", ownerIdent, treeAgent, true},       // ancestry pierces tree
		{"ancestry-owner→project", ownerIdent, projectAgent, true}, // ancestry for project

		// Non-owner project member
		{"member→none", memberIdent, noneAgent, false},
		{"member→owner", memberIdent, ownerAgent, false},
		{"member→direct", memberIdent, directAgent, false},
		{"member→tree", memberIdent, treeAgent, false},
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

	ownerTarget := msgAuthzAgent(t, s, "pierce-owner", projectID, store.MessageModeOwner, []string{owner.ID})
	directTarget := msgAuthzAgent(t, s, "pierce-direct", projectID, store.MessageModeDirect, []string{owner.ID})
	treeTarget := msgAuthzAgent(t, s, "pierce-tree", projectID, store.MessageModeTree, []string{owner.ID})

	// Owner's project-mode agent — should NOT inherit owner's piercing ability
	ownerAgent := msgAuthzAgent(t, s, "owner-agent-pierce", projectID, store.MessageModeProject, []string{owner.ID})
	agentIdent := msgAuthzAgentIdentity(ownerAgent.ID, projectID, ownerAgent.Ancestry)

	t.Run("owner agent cannot pierce owner", func(t *testing.T) {
		allowed, _, _ := srv.authorizeAgentMessage(ctx, agentIdent, ownerTarget, false)
		if allowed {
			t.Fatal("owner's agent should NOT pierce owner mode")
		}
	})

	t.Run("owner agent cannot pierce direct", func(t *testing.T) {
		allowed, _, _ := srv.authorizeAgentMessage(ctx, agentIdent, directTarget, false)
		if allowed {
			t.Fatal("owner's agent should NOT pierce direct mode")
		}
	})

	t.Run("owner agent cannot pierce separate tree", func(t *testing.T) {
		allowed, _, _ := srv.authorizeAgentMessage(ctx, agentIdent, treeTarget, false)
		if allowed {
			t.Fatal("owner's agent should NOT pierce separate tree mode agent")
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
		store.MessageModeOwner,
		store.MessageModeDirect,
		store.MessageModeTree,
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
