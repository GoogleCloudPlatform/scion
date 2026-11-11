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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUserDeleteToken_InTransactionBindingRefused_Postgres is
// TestUserDeleteToken_BindingAddedAfterPreCheckRefused on PostgreSQL: a
// system-scope binding invisible to the pre-check and present in the
// transaction refuses a token's plain and forced delete with 409 conflict
// and details.reason binding_state_drift, keeping the user and the binding,
// with no deletion recorded.
func TestUserDeleteToken_InTransactionBindingRefused_Postgres(t *testing.T) {
	f := newForceDelPostgresFixture(t, "urd-late")
	// The fixture's setup writes through the raw store only, so both
	// wrappers are installed before anything reads the server's store in
	// the background.
	hidePlain := urdInstallHidden(t, f.srv)
	hideDave := urdInstallHidden(t, f.srv)
	ctx := context.Background()
	admin := hubConfigTokenUser(t, f.s, "urd-late-pg-super", store.SystemRoleSuperAdmin)
	igProjectBinding(t, f.s, admin, f.project.ID, store.ProjectRoleOwner)
	customRole := urdCustomSystemRole(t, f.s, "urd-late-pg-role")
	key := mintHubConfigToken(t, f.srv, admin, hubBoundary(), "user:delete", "agent:delete")

	plain := urdMember(t, f.s, "urd-late-pg-plain")
	urdSystemBinding(t, f.s, plain, customRole, nil)
	hidePlain.hide(plain, nil)
	rec := doRequestWithToken(t, f.srv, key, http.MethodDelete, "/api/v1/users/"+plain, nil)
	requireBindingStateDrift(t, rec, "plain delete")
	assert.True(t, urdUserExists(t, f.s, plain))
	assert.True(t, urdHoldsBinding(t, f.s, plain, customRole))

	f.agent(t, "urd-late-pg-a", nil)
	urdSystemBinding(t, f.s, f.dave.ID, customRole, nil)
	hidePlain.stop()
	hideDave.hide(f.dave.ID, nil)
	rec = doRequestWithToken(t, f.srv, key, http.MethodDelete, "/api/v1/users/"+f.dave.ID+"?force=true", nil)
	requireBindingStateDrift(t, rec, "forced delete")
	u, err := f.s.GetUser(ctx, f.dave.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusActive, u.Status)
	assert.True(t, urdHoldsBinding(t, f.s, f.dave.ID, customRole))
	_, err = f.s.GetUserDeletionByUser(ctx, f.dave.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "no deletion recorded")
}
