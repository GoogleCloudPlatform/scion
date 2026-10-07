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
	"database/sql"
	"runtime"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// perfIndependentCounter is a test store placed UNDER the server (and so
// under the perf decorator). It counts, by the method actually invoked, the
// calls whose immediate caller is a perfAuthzStore method, independently of
// the decorator's own labels. A call that arrives from a decorator method
// of a different name is counted under "mismatch:<caller>-><method>". It
// forwards everything unchanged and keeps DB() so the server still finds
// its connection pool.
type perfIndependentCounter struct {
	store.Store
	mu     sync.Mutex
	counts map[string]int64
}

func newPerfIndependentCounter(s store.Store) *perfIndependentCounter {
	return &perfIndependentCounter{Store: s, counts: map[string]int64{}}
}

func (c *perfIndependentCounter) DB() *sql.DB {
	if d, ok := c.Store.(interface{ DB() *sql.DB }); ok {
		return d.DB()
	}
	return nil
}

func (c *perfIndependentCounter) note(method string) {
	pcs := make([]uintptr, 4)
	n := runtime.Callers(3, pcs) // skip Callers, note, the counter method
	frames := runtime.CallersFrames(pcs[:n])
	f, _ := frames.Next()
	const marker = ".perfAuthzStore."
	i := strings.LastIndex(f.Function, marker)
	if i < 0 {
		return // not a call through the decorator
	}
	caller := f.Function[i+len(marker):]
	key := method
	if caller != method {
		key = "mismatch:" + caller + "->" + method
	}
	c.mu.Lock()
	c.counts[key]++
	c.mu.Unlock()
}

func (c *perfIndependentCounter) reset() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.counts
	c.counts = map[string]int64{}
	return out
}

func (c *perfIndependentCounter) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	c.note("GetEffectiveGroups")
	return c.Store.GetEffectiveGroups(ctx, userID)
}

func (c *perfIndependentCounter) GetEffectiveGroupsForAgent(ctx context.Context, agentID string) ([]string, error) {
	c.note("GetEffectiveGroupsForAgent")
	return c.Store.GetEffectiveGroupsForAgent(ctx, agentID)
}

func (c *perfIndependentCounter) GetParentGroups(ctx context.Context, groupID string) ([]string, error) {
	c.note("GetParentGroups")
	return c.Store.GetParentGroups(ctx, groupID)
}

func (c *perfIndependentCounter) GetUserGroups(ctx context.Context, userID string) ([]store.GroupMember, error) {
	c.note("GetUserGroups")
	return c.Store.GetUserGroups(ctx, userID)
}

func (c *perfIndependentCounter) GetGroupMembership(ctx context.Context, groupID, memberType, memberID string) (*store.GroupMember, error) {
	c.note("GetGroupMembership")
	return c.Store.GetGroupMembership(ctx, groupID, memberType, memberID)
}

func (c *perfIndependentCounter) GetGroupBySlug(ctx context.Context, slug string) (*store.Group, error) {
	c.note("GetGroupBySlug")
	return c.Store.GetGroupBySlug(ctx, slug)
}

func (c *perfIndependentCounter) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	c.note("ListRoleBindingsForPrincipals")
	return c.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

func (c *perfIndependentCounter) ListRoleBindingsForPrincipal(ctx context.Context, principalType, principalID string) ([]*store.RoleBinding, error) {
	c.note("ListRoleBindingsForPrincipal")
	return c.Store.ListRoleBindingsForPrincipal(ctx, principalType, principalID)
}

func (c *perfIndependentCounter) GetRoleDefinition(ctx context.Context, id string) (*store.RoleDefinition, error) {
	c.note("GetRoleDefinition")
	return c.Store.GetRoleDefinition(ctx, id)
}

func (c *perfIndependentCounter) GetRoleDefinitionsByIDs(ctx context.Context, ids []string) (map[string]*store.RoleDefinition, error) {
	c.note("GetRoleDefinitionsByIDs")
	return c.Store.GetRoleDefinitionsByIDs(ctx, ids)
}

func (c *perfIndependentCounter) GetRoleDefinitionByName(ctx context.Context, name string, scopeType string) (*store.RoleDefinition, error) {
	c.note("GetRoleDefinitionByName")
	return c.Store.GetRoleDefinitionByName(ctx, name, scopeType)
}

func (c *perfIndependentCounter) ListAccessConstraints(ctx context.Context, limit, offset int) ([]*store.AccessConstraint, error) {
	c.note("ListAccessConstraints")
	return c.Store.ListAccessConstraints(ctx, limit, offset)
}

func (c *perfIndependentCounter) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	c.note("GetDelegationEdgesForDelegate")
	return c.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
}

func (c *perfIndependentCounter) GetUser(ctx context.Context, id string) (*store.User, error) {
	c.note("GetUser")
	return c.Store.GetUser(ctx, id)
}

func (c *perfIndependentCounter) GetUserAccessToken(ctx context.Context, id string) (*store.UserAccessToken, error) {
	c.note("GetUserAccessToken")
	return c.Store.GetUserAccessToken(ctx, id)
}

func (c *perfIndependentCounter) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	c.note("GetAgent")
	return c.Store.GetAgent(ctx, id)
}

func (c *perfIndependentCounter) GetProject(ctx context.Context, id string) (*store.Project, error) {
	c.note("GetProject")
	return c.Store.GetProject(ctx, id)
}

func (c *perfIndependentCounter) GetProjectMembership(ctx context.Context, projectID, userID string) (*store.ProjectMembership, error) {
	c.note("GetProjectMembership")
	return c.Store.GetProjectMembership(ctx, projectID, userID)
}

func (c *perfIndependentCounter) GetHubSetting(ctx context.Context, section string) (*store.HubSetting, error) {
	c.note("GetHubSetting")
	return c.Store.GetHubSetting(ctx, section)
}
