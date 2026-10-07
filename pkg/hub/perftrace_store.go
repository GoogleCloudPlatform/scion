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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// perfAuthzStore wraps the store held by the authorization service when
// server.hub.perf_trace is on. It counts and times the reads listed in
// perfStoreOp against the request's trace, then forwards every call
// unchanged: same arguments, same context, same results and errors. All
// other store.Store methods are promoted from the embedded store untouched.
//
// It wraps only the store passed to NewAuthzService (server.go), never
// Server.store. The decorator exposes only the store.Store interface, not
// the optional interfaces a concrete store also implements (DB(),
// store.AdvisoryLocker, store.ScheduledEventClaimer), which other server
// code type-asserts for on Server.store. The authorization service does not
// type-assert its store.
//
// Because it sits below the request-local input memo, the counts are real
// store round trips after memoization.
type perfAuthzStore struct {
	store.Store
}

// wrapAuthzStoreForPerfTrace returns s unchanged when enabled is false, so
// with the setting off the authorization service holds the original store.
func wrapAuthzStoreForPerfTrace(s store.Store, enabled bool) store.Store {
	if !enabled || s == nil {
		return s
	}
	return perfAuthzStore{Store: s}
}

// perfStoreCallStart returns the function that records one call of op.
// With no trace in ctx it returns the shared no-op.
func perfStoreCallStart(ctx context.Context, op perfStoreOp) func() {
	t := perfTraceFrom(ctx)
	if t == nil {
		return perfNoop
	}
	start := time.Now()
	return func() { t.addStoreCall(op, time.Since(start)) }
}

func (c perfAuthzStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	done := perfStoreCallStart(ctx, perfStoreGetEffectiveGroups)
	v, err := c.Store.GetEffectiveGroups(ctx, userID)
	done()
	return v, err
}

func (c perfAuthzStore) GetEffectiveGroupsForAgent(ctx context.Context, agentID string) ([]string, error) {
	done := perfStoreCallStart(ctx, perfStoreGetEffectiveGroupsForAgent)
	v, err := c.Store.GetEffectiveGroupsForAgent(ctx, agentID)
	done()
	return v, err
}

func (c perfAuthzStore) GetParentGroups(ctx context.Context, groupID string) ([]string, error) {
	done := perfStoreCallStart(ctx, perfStoreGetParentGroups)
	v, err := c.Store.GetParentGroups(ctx, groupID)
	done()
	return v, err
}

func (c perfAuthzStore) GetUserGroups(ctx context.Context, userID string) ([]store.GroupMember, error) {
	done := perfStoreCallStart(ctx, perfStoreGetUserGroups)
	v, err := c.Store.GetUserGroups(ctx, userID)
	done()
	return v, err
}

func (c perfAuthzStore) GetGroupMembership(ctx context.Context, groupID, memberType, memberID string) (*store.GroupMember, error) {
	done := perfStoreCallStart(ctx, perfStoreGetGroupMembership)
	v, err := c.Store.GetGroupMembership(ctx, groupID, memberType, memberID)
	done()
	return v, err
}

func (c perfAuthzStore) GetGroupBySlug(ctx context.Context, slug string) (*store.Group, error) {
	done := perfStoreCallStart(ctx, perfStoreGetGroupBySlug)
	v, err := c.Store.GetGroupBySlug(ctx, slug)
	done()
	return v, err
}

func (c perfAuthzStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	done := perfStoreCallStart(ctx, perfStoreListRoleBindingsForPrincipals)
	v, err := c.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
	done()
	return v, err
}

func (c perfAuthzStore) ListRoleBindingsForPrincipal(ctx context.Context, principalType, principalID string) ([]*store.RoleBinding, error) {
	done := perfStoreCallStart(ctx, perfStoreListRoleBindingsForPrincipal)
	v, err := c.Store.ListRoleBindingsForPrincipal(ctx, principalType, principalID)
	done()
	return v, err
}

func (c perfAuthzStore) GetRoleDefinition(ctx context.Context, id string) (*store.RoleDefinition, error) {
	done := perfStoreCallStart(ctx, perfStoreGetRoleDefinition)
	v, err := c.Store.GetRoleDefinition(ctx, id)
	done()
	return v, err
}

func (c perfAuthzStore) GetRoleDefinitionsByIDs(ctx context.Context, ids []string) (map[string]*store.RoleDefinition, error) {
	done := perfStoreCallStart(ctx, perfStoreGetRoleDefinitionsByIDs)
	v, err := c.Store.GetRoleDefinitionsByIDs(ctx, ids)
	done()
	return v, err
}

func (c perfAuthzStore) GetRoleDefinitionByName(ctx context.Context, name string, scopeType string) (*store.RoleDefinition, error) {
	done := perfStoreCallStart(ctx, perfStoreGetRoleDefinitionByName)
	v, err := c.Store.GetRoleDefinitionByName(ctx, name, scopeType)
	done()
	return v, err
}

func (c perfAuthzStore) ListAccessConstraints(ctx context.Context, limit, offset int) ([]*store.AccessConstraint, error) {
	done := perfStoreCallStart(ctx, perfStoreListAccessConstraints)
	v, err := c.Store.ListAccessConstraints(ctx, limit, offset)
	done()
	return v, err
}

func (c perfAuthzStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	done := perfStoreCallStart(ctx, perfStoreGetDelegationEdgesForDelegate)
	v, err := c.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
	done()
	return v, err
}

func (c perfAuthzStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	done := perfStoreCallStart(ctx, perfStoreGetUser)
	v, err := c.Store.GetUser(ctx, id)
	done()
	return v, err
}

func (c perfAuthzStore) GetUserAccessToken(ctx context.Context, id string) (*store.UserAccessToken, error) {
	done := perfStoreCallStart(ctx, perfStoreGetUserAccessToken)
	v, err := c.Store.GetUserAccessToken(ctx, id)
	done()
	return v, err
}

func (c perfAuthzStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	done := perfStoreCallStart(ctx, perfStoreGetAgent)
	v, err := c.Store.GetAgent(ctx, id)
	done()
	return v, err
}

func (c perfAuthzStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	done := perfStoreCallStart(ctx, perfStoreGetProject)
	v, err := c.Store.GetProject(ctx, id)
	done()
	return v, err
}

func (c perfAuthzStore) GetProjectMembership(ctx context.Context, projectID, userID string) (*store.ProjectMembership, error) {
	done := perfStoreCallStart(ctx, perfStoreGetProjectMembership)
	v, err := c.Store.GetProjectMembership(ctx, projectID, userID)
	done()
	return v, err
}

func (c perfAuthzStore) GetHubSetting(ctx context.Context, section string) (*store.HubSetting, error) {
	done := perfStoreCallStart(ctx, perfStoreGetHubSetting)
	v, err := c.Store.GetHubSetting(ctx, section)
	done()
	return v, err
}
