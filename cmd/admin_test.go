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

package cmd

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPromoteUserToAdmin_RefusesTestIdentity: the `scion admin promote` DB
// path never makes a hub test identity admin.
func TestPromoteUserToAdmin_RefusesTestIdentity(t *testing.T) {
	ctx := context.Background()
	s := newRecoveryTestStore(t)

	exp := time.Now().Add(time.Hour)
	issuer := uuid.NewString()
	fixture := &store.User{
		ID:        uuid.NewString(),
		Email:     "test-identity-abc@" + store.TestFixtureEmailDomain,
		Role:      store.UserRoleMember,
		Status:    store.UserStatusActive,
		Kind:      store.UserKindTestFixture,
		ExpiresAt: &exp,
		IssuedBy:  &issuer,
	}
	require.NoError(t, s.CreateTestFixtureUser(ctx, fixture))

	var out bytes.Buffer
	err := promoteUserToAdmin(ctx, s, fixture.Email, &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "test identities cannot be promoted")

	got, err := s.GetUser(ctx, fixture.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserRoleMember, got.Role)
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, fixture.ID)
	require.NoError(t, err)
	assert.Empty(t, bindings)
}
