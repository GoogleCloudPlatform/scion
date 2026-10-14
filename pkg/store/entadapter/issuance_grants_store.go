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

	"github.com/GoogleCloudPlatform/scion/pkg/ent/brokerjointoken"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/groupmembership"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/invitecode"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/rolebinding"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/roledefinition"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ListIssuanceStampedGrants returns the grant rows stamped with issuanceID
// (admin tier A3); see store.Store.ListIssuanceStampedGrants.
func (c *CompositeStore) ListIssuanceStampedGrants(ctx context.Context, issuanceID string, limit int) ([]store.IssuanceStampedGrant, bool, error) {
	if issuanceID == "" {
		return nil, false, fmt.Errorf("%w: issuance ID is required", store.ErrInvalidInput)
	}
	fetch := 0
	if limit > 0 {
		fetch = limit + 1
	}
	truncated := false
	trim := func(n int) int {
		if limit > 0 && n > limit {
			truncated = true
			return limit
		}
		return n
	}
	var out []store.IssuanceStampedGrant

	rbq := c.client.RoleBinding.Query().Where(rolebinding.IssuanceIDEQ(issuanceID)).Order(rolebinding.ByID())
	if fetch > 0 {
		rbq = rbq.Limit(fetch)
	}
	rbs, err := rbq.All(ctx)
	if err != nil {
		return nil, false, mapError(err)
	}
	for _, rb := range rbs[:trim(len(rbs))] {
		g := store.IssuanceStampedGrant{
			Kind: store.IssuanceGrantRoleBinding, ID: rb.ID.String(),
			PrincipalType: string(rb.PrincipalType), PrincipalID: rb.PrincipalID,
			ScopeType: string(rb.ScopeType), ScopeID: rb.ScopeID, ExpiresAt: rb.ExpiresAt,
		}
		if rb.RoleDefinitionID != nil {
			g.RoleDefinitionID = rb.RoleDefinitionID.String()
		}
		out = append(out, g)
	}

	gmq := c.client.GroupMembership.Query().Where(groupmembership.IssuanceIDEQ(issuanceID)).Order(groupmembership.ByID())
	if fetch > 0 {
		gmq = gmq.Limit(fetch)
	}
	gms, err := gmq.All(ctx)
	if err != nil {
		return nil, false, mapError(err)
	}
	for _, m := range gms[:trim(len(gms))] {
		g := store.IssuanceStampedGrant{
			Kind: store.IssuanceGrantGroupMembership, ID: m.ID.String(),
			GroupID: m.GroupID.String(), ExpiresAt: m.ExpiresAt,
		}
		switch {
		case m.UserID != nil:
			g.PrincipalType, g.PrincipalID = store.GroupMemberTypeUser, m.UserID.String()
		case m.AgentID != nil:
			g.PrincipalType, g.PrincipalID = store.GroupMemberTypeAgent, m.AgentID.String()
		}
		out = append(out, g)
	}

	rdq := c.client.RoleDefinition.Query().Where(roledefinition.IssuanceIDEQ(issuanceID)).Order(roledefinition.ByID())
	if fetch > 0 {
		rdq = rdq.Limit(fetch)
	}
	rds, err := rdq.All(ctx)
	if err != nil {
		return nil, false, mapError(err)
	}
	for _, rd := range rds[:trim(len(rds))] {
		out = append(out, store.IssuanceStampedGrant{
			Kind: store.IssuanceGrantRoleDefinition, ID: rd.ID.String(),
			Name: rd.Name, ScopeType: string(rd.ScopeType),
		})
	}

	icq := c.client.InviteCode.Query().Where(invitecode.IssuanceIDEQ(issuanceID)).Order(invitecode.ByID())
	if fetch > 0 {
		icq = icq.Limit(fetch)
	}
	ics, err := icq.All(ctx)
	if err != nil {
		return nil, false, mapError(err)
	}
	for _, ic := range ics[:trim(len(ics))] {
		exp := ic.ExpiresAt
		out = append(out, store.IssuanceStampedGrant{
			Kind: store.IssuanceGrantInviteCode, ID: ic.ID.String(), Name: ic.CodePrefix, ExpiresAt: &exp,
		})
	}

	jtq := c.client.BrokerJoinToken.Query().Where(brokerjointoken.IssuanceIDEQ(issuanceID)).Order(brokerjointoken.ByID())
	if fetch > 0 {
		jtq = jtq.Limit(fetch)
	}
	jts, err := jtq.All(ctx)
	if err != nil {
		return nil, false, mapError(err)
	}
	for _, jt := range jts[:trim(len(jts))] {
		exp := jt.ExpiresAt
		out = append(out, store.IssuanceStampedGrant{
			Kind: store.IssuanceGrantBrokerJoinToken, ID: jt.ID.String(), ExpiresAt: &exp,
		})
	}
	return out, truncated, nil
}

// DeleteIssuanceStampedGrant deletes the row g names while it carries
// issuanceID; see store.Store.DeleteIssuanceStampedGrant. Each delete is one
// statement conditioned on the stamp, so a row that no longer carries it is
// never removed.
func (c *CompositeStore) DeleteIssuanceStampedGrant(ctx context.Context, g store.IssuanceStampedGrant, issuanceID string) (bool, error) {
	if issuanceID == "" {
		return false, fmt.Errorf("%w: issuance ID is required", store.ErrInvalidInput)
	}
	id, err := parseUUID(g.ID)
	if err != nil {
		return false, err
	}
	var n int
	switch g.Kind {
	case store.IssuanceGrantRoleBinding:
		n, err = c.client.RoleBinding.Delete().
			Where(rolebinding.IDEQ(id), rolebinding.IssuanceIDEQ(issuanceID)).Exec(ctx)
	case store.IssuanceGrantGroupMembership:
		n, err = c.client.GroupMembership.Delete().
			Where(groupmembership.IDEQ(id), groupmembership.IssuanceIDEQ(issuanceID)).Exec(ctx)
	case store.IssuanceGrantRoleDefinition:
		bound, berr := c.client.RoleBinding.Query().Where(rolebinding.RoleDefinitionIDEQ(id)).Exist(ctx)
		if berr != nil {
			return false, mapError(berr)
		}
		if bound {
			return false, store.ErrStampedRoleInUse
		}
		n, err = c.client.RoleDefinition.Delete().
			Where(roledefinition.IDEQ(id), roledefinition.IssuanceIDEQ(issuanceID), roledefinition.SystemEQ(false)).Exec(ctx)
	case store.IssuanceGrantInviteCode:
		n, err = c.client.InviteCode.Delete().
			Where(invitecode.IDEQ(id), invitecode.IssuanceIDEQ(issuanceID)).Exec(ctx)
	case store.IssuanceGrantBrokerJoinToken:
		n, err = c.client.BrokerJoinToken.Delete().
			Where(brokerjointoken.IDEQ(id), brokerjointoken.IssuanceIDEQ(issuanceID)).Exec(ctx)
	default:
		return false, fmt.Errorf("%w: unknown grant kind %q", store.ErrInvalidInput, g.Kind)
	}
	if err != nil {
		return false, mapError(err)
	}
	return n > 0, nil
}
