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

package storetest

import (
	"context"
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GroupDirectParentsConformance exercises store.GroupStore's
// GetDirectParentGroupIDs across backends: it returns exactly the groups that
// hold the given group as a direct child group, never a transitive ancestor,
// and includes the group itself when it has a self-edge.
func GroupDirectParentsConformance(t *testing.T, factory Factory) {
	t.Helper()
	ctx := context.Background()
	s := factory(t)

	newGroup := func(name string) string {
		t.Helper()
		id := uuid.NewString()
		require.NoError(t, s.CreateGroup(ctx, &store.Group{
			ID: id, Name: name, Slug: name + "-" + id[:8], GroupType: store.GroupTypeExplicit,
		}))
		return id
	}
	addChild := func(parentID, childID string) {
		t.Helper()
		require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
			GroupID: parentID, MemberType: store.GroupMemberTypeGroup, MemberID: childID,
			Role: store.GroupMemberRoleMember,
		}))
	}

	child := newGroup("direct-parents-child")
	parentA := newGroup("direct-parents-a")
	parentB := newGroup("direct-parents-b")
	ancestor := newGroup("direct-parents-ancestor")
	lone := newGroup("direct-parents-lone")
	selfNested := newGroup("direct-parents-self")

	addChild(parentA, child)
	addChild(parentB, child)
	addChild(ancestor, parentA) // ancestor of child, not a direct parent
	addChild(selfNested, selfNested)

	got, err := s.GetDirectParentGroupIDs(ctx, child)
	require.NoError(t, err)
	want := []string{parentA, parentB}
	sort.Strings(want)
	assert.Equal(t, want, got, "only direct parents, sorted")

	got, err = s.GetDirectParentGroupIDs(ctx, parentA)
	require.NoError(t, err)
	assert.Equal(t, []string{ancestor}, got)

	got, err = s.GetDirectParentGroupIDs(ctx, lone)
	require.NoError(t, err)
	assert.Empty(t, got, "no parents is an empty result, not an error")

	got, err = s.GetDirectParentGroupIDs(ctx, selfNested)
	require.NoError(t, err)
	assert.Equal(t, []string{selfNested}, got, "a self-edge is reported")

	got, err = s.GetDirectParentGroupIDs(ctx, uuid.NewString())
	require.NoError(t, err)
	assert.Empty(t, got, "unknown group has no parents")
}
