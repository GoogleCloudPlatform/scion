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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
)

// nilEntryListStore returns listings that contain nil entries. Methods not
// overridden here panic through the nil embedded store.Store.
type nilEntryListStore struct {
	store.Store
	bindings    []*store.RoleBinding
	constraints []*store.AccessConstraint
	deletedIDs  []string
}

func (s *nilEntryListStore) ListRoleBindingsForPrincipal(_ context.Context, _, _ string) ([]*store.RoleBinding, error) {
	return s.bindings, nil
}

func (s *nilEntryListStore) ListAccessConstraintsFiltered(_ context.Context, _ store.AccessConstraintListOptions) ([]*store.AccessConstraint, string, int, error) {
	return s.constraints, "", len(s.constraints), nil
}

func (s *nilEntryListStore) GetRoleDefinition(_ context.Context, _ string) (*store.RoleDefinition, error) {
	return nil, store.ErrNotFound
}

func (s *nilEntryListStore) WithTx(_ context.Context, fn func(tx store.Store) error) error {
	return fn(s)
}

func (s *nilEntryListStore) DeleteRoleBinding(_ context.Context, id string) error {
	s.deletedIDs = append(s.deletedIDs, id)
	return nil
}

func (s *nilEntryListStore) CreateMutationAudit(_ context.Context, _ *store.MutationAuditRecord) error {
	return nil
}

func TestRemoveProjectMembersGroupRoleBindings_SkipsNilEntries(t *testing.T) {
	s := &nilEntryListStore{bindings: []*store.RoleBinding{
		nil,
		{ID: "rb-1", RoleDefinitionID: "rd-1"},
		nil,
	}}
	removed := removeProjectMembersGroupRoleBindings(context.Background(), s, "group-1")
	assert.Equal(t, 1, removed)
	assert.Equal(t, []string{"rb-1"}, s.deletedIDs)
}

func TestCountAccessConstraintsByGroup_SkipsNilEntries(t *testing.T) {
	groupID := "group-1"
	s := &nilEntryListStore{constraints: []*store.AccessConstraint{
		nil,
		{SubjectKind: store.ConstraintSubjectGroupClosure, SubjectGroupID: &groupID},
		nil,
	}}
	counts := countAccessConstraintsByGroup(context.Background(), s)
	assert.Equal(t, map[string]int{groupID: 1}, counts)
}
