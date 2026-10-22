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
	"github.com/stretchr/testify/require"
)

// authzSeesUserActive makes srv's authorization service, and the
// account-status check of the auth middleware (AuthConfig.UserStore), read
// userID's account as active. Every other reader, including the handlers'
// transaction guards (which read users through the transaction), still
// reads the stored row. Tests use it to reach a guard that runs after
// authorization with an actor whose stored row is not active; the
// account-status check itself is pinned by tests that do not use it.
// The wrapper hides the concrete clamp type from the grant-clamp type
// checks (authz_candelegate.go:225, agent_standing.go:311), which then
// fall back to uncached reads; the tests that use it do not depend on that.
func authzSeesUserActive(t *testing.T, srv *Server, userID string) {
	t.Helper()
	require.NotNil(t, srv.authzService)
	srv.authzService.store = &authzActiveUserView{Store: srv.authzService.store, userID: userID}
	if srv.authConfig.UserStore != nil {
		srv.authConfig.UserStore = &authActiveUserView{UserStore: srv.authConfig.UserStore, userID: userID}
	}
}

// authActiveUserView is authzActiveUserView for the auth middleware's
// store.UserStore.
type authActiveUserView struct {
	store.UserStore
	userID string
}

func (v *authActiveUserView) GetUser(ctx context.Context, id string) (*store.User, error) {
	u, err := v.UserStore.GetUser(ctx, id)
	if err != nil || u == nil || id != v.userID {
		return u, err
	}
	active := *u
	active.Status = store.UserStatusActive
	return &active, nil
}

// authzActiveUserView reports userID's users row with an active status
// and passes every other call through unchanged.
type authzActiveUserView struct {
	store.Store
	userID string
}

func (v *authzActiveUserView) GetUser(ctx context.Context, id string) (*store.User, error) {
	u, err := v.Store.GetUser(ctx, id)
	if err != nil || u == nil || id != v.userID {
		return u, err
	}
	active := *u
	active.Status = store.UserStatusActive
	return &active, nil
}
