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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// skillProgenyAdapter is the ProgenyFactAdapter for personal (user-scoped)
// skills (ptone/scion#2128). It consolidates the former dedicated
// creator-user-skill grant (agentCreatorUserSkillGrant, retired) into the
// common progeny evaluator (authz_relationship_rules.go).
//
// A personal skill's sharing source is the bucket itself, not a per-record
// fact: skillScopeResource sets Resource.ScopeUserID to the owning user for
// both a concrete skill (skillResource, which also sets Resource.ID) and the
// ID-less bucket probe agentSkillAccessScope uses
// (skillScopeResource(store.SkillScopeUser, origin), which never sets an
// ID). FactResourceID below is what makes both shapes resolve to the same
// query ID; Sources itself never reads the store or inspects a Resource — it
// only echoes back the ID it is asked about as a synthetic sharing source
// owned by that same ID.
//
// This adapter never consults Resource.OwnerID or a skill's CreatedBy: a
// skill's OwnerID/CreatedBy can differ from its ScopeUserID, and the sharing
// source is deliberately keyed on ScopeUserID alone — never union both keys
// to broaden recipients.
type skillProgenyAdapter struct{}

// Kind implements ProgenyFactAdapter.
func (skillProgenyAdapter) Kind() string { return "skill" }

// ReadPermissions implements ProgenyFactAdapter. skill.read is the only
// read-class permission a personal-skill progeny candidate may carry.
func (skillProgenyAdapter) ReadPermissions() []string { return []string{"skill.read"} }

// Sources implements ProgenyFactAdapter. q.ResourceID carries the owning
// user's ID (see FactResourceID/relationshipCandidates), not a skill record
// ID. Sources never reads the store or a Resource — it only synthesizes the
// sharing source from the ID it is asked about. A personal skill bucket is
// available to its owning user's descendants by default, never opt-in
// (SharingPolicyOriginDescendants) — no mandatory opt-in flag is introduced
// by this consolidation.
func (skillProgenyAdapter) Sources(_ context.Context, q ProgenyQuery) ([]SharingSource, error) {
	if q.ResourceID == "" {
		return nil, nil
	}
	return []SharingSource{{
		Kind:    "skill",
		ID:      q.ResourceID,
		OwnerID: q.ResourceID,
		Policy:  SharingPolicyOriginDescendants,
	}}, nil
}

// FactResourceID implements ProgenyFactResourceIDer (authz_relationship_rules.go).
// A personal skill's sharing source is its owning user's bucket
// (Resource.ScopeUserID), not resource.ID, so this is what makes an ID-less
// bucket probe (skillScopeResource(store.SkillScopeUser, origin), used by
// agentSkillAccessScope) and a concrete skill read (skillResource, which
// additionally sets Resource.ID) evaluate through the exact same fact.
//
// resource is ineligible — no progeny candidate is built at all — unless it
// is an explicit user-scoped skill with a non-empty owning user: a skill of
// any other scope (global/core/project), or a hand-built Resource that
// carries ScopeUserID without ScopeKind == store.SkillScopeUser, must not
// pick up a sharing source by accident. This mirrors the shape check the
// retired agentCreatorUserSkillGrant used to make.
func (skillProgenyAdapter) FactResourceID(resource Resource) (string, bool) {
	if resource.ScopeKind != store.SkillScopeUser || resource.ScopeUserID == "" {
		return "", false
	}
	return resource.ScopeUserID, true
}
