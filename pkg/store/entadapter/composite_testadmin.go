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

package entadapter

import (
	"context"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// CreateTestAdminUser creates an admin-kind test identity and its system
// role binding in one transaction (see store.Store.CreateTestAdminUser).
// Inside a caller's WithTx the nested WithTx passes through, so the row,
// the binding and whatever else the caller writes (its audit record)
// commit or roll back together.
func (c *CompositeStore) CreateTestAdminUser(ctx context.Context, u *store.User, roleDefinitionID string) (*store.RoleBinding, error) {
	if u == nil {
		return nil, fmt.Errorf("%w: user is required", store.ErrInvalidInput)
	}
	if err := validateTestAdminUser(u); err != nil {
		return nil, err
	}
	if roleDefinitionID == "" {
		return nil, fmt.Errorf("%w: a role definition is required", store.ErrInvalidInput)
	}
	var created *store.RoleBinding
	err := c.WithTx(ctx, func(tx store.Store) error {
		txc, ok := tx.(*CompositeStore)
		if !ok {
			return fmt.Errorf("create test admin user: unexpected transactional store %T", tx)
		}
		rd, err := txc.GetRoleDefinition(ctx, roleDefinitionID)
		if err != nil {
			return err
		}
		if rd.ScopeType != store.RoleScopeSystem {
			return fmt.Errorf("%w: an admin-kind test identity binding must be system-scoped", store.ErrInvalidInput)
		}
		if err := txc.createTestAdminRow(ctx, u); err != nil {
			return err
		}
		// The row's expiry at its stored precision (createTestAdminRow).
		expiresAt := testAdminExpiry(*u.ExpiresAt)
		created, err = txc.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: roleDefinitionID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      u.ID,
			ScopeType:        store.RoleScopeSystem,
			ExpiresAt:        &expiresAt,
			CreatedBy:        store.TestIdentityIssuanceCreatedBy,
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}
