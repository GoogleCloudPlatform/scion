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

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/accesspolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/policybinding"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	entschema "github.com/GoogleCloudPlatform/scion/pkg/ent/schema"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// PolicyStore implements store.PolicyStore using Ent ORM.
type PolicyStore struct {
	client *ent.Client
}

// NewPolicyStore creates a new Ent-backed PolicyStore.
func NewPolicyStore(client *ent.Client) *PolicyStore {
	return &PolicyStore{client: client}
}

// entPolicyToStore converts an Ent AccessPolicy entity to a store.Policy model.
func entPolicyToStore(p *ent.AccessPolicy) *store.Policy {
	sp := &store.Policy{
		ID:           p.ID.String(),
		Name:         p.Name,
		Description:  p.Description,
		ScopeType:    string(p.ScopeType),
		ScopeID:      p.ScopeID,
		ResourceType: p.ResourceType,
		ResourceID:   p.ResourceID,
		Actions:      p.Actions,
		Effect:       string(p.Effect),
		Priority:     p.Priority,
		Labels:       p.Labels,
		Annotations:  p.Annotations,
		Created:      p.Created,
		Updated:      p.Updated,
		CreatedBy:    p.CreatedBy,
	}
	if p.Conditions != nil {
		sp.Conditions = entConditionsToStore(p.Conditions)
	}
	return sp
}

// entConditionsToStore converts Ent schema PolicyConditions to store PolicyConditions.
func entConditionsToStore(c *entschema.PolicyConditions) *store.PolicyConditions {
	if c == nil {
		return nil
	}
	sc := &store.PolicyConditions{
		Labels:             c.Labels,
		ValidFrom:          c.ValidFrom,
		ValidUntil:         c.ValidUntil,
		SourceIPs:          c.SourceIPs,
		DelegatedFromGroup: c.DelegatedFromGroup,
	}
	if c.DelegatedFrom != nil {
		sc.DelegatedFrom = &store.DelegatedFromCondition{
			PrincipalType: c.DelegatedFrom.PrincipalType,
			PrincipalID:   c.DelegatedFrom.PrincipalID,
		}
	}
	return sc
}

// storeConditionsToEnt converts store PolicyConditions to Ent schema PolicyConditions.
func storeConditionsToEnt(c *store.PolicyConditions) *entschema.PolicyConditions {
	if c == nil {
		return nil
	}
	ec := &entschema.PolicyConditions{
		Labels:             c.Labels,
		ValidFrom:          c.ValidFrom,
		ValidUntil:         c.ValidUntil,
		SourceIPs:          c.SourceIPs,
		DelegatedFromGroup: c.DelegatedFromGroup,
	}
	if c.DelegatedFrom != nil {
		ec.DelegatedFrom = &entschema.DelegatedFromCondition{
			PrincipalType: c.DelegatedFrom.PrincipalType,
			PrincipalID:   c.DelegatedFrom.PrincipalID,
		}
	}
	return ec
}

// CreatePolicy creates a new policy record.
func (s *PolicyStore) CreatePolicy(ctx context.Context, p *store.Policy) error {
	uid, err := parseUUID(p.ID)
	if err != nil {
		return err
	}

	create := s.client.AccessPolicy.Create().
		SetID(uid).
		SetName(p.Name).
		SetDescription(p.Description).
		SetScopeType(accesspolicy.ScopeType(p.ScopeType)).
		SetResourceType(p.ResourceType).
		SetEffect(accesspolicy.Effect(p.Effect)).
		SetPriority(p.Priority)

	if p.ScopeID != "" {
		create.SetScopeID(p.ScopeID)
	}
	if p.ResourceID != "" {
		create.SetResourceID(p.ResourceID)
	}
	if p.Actions != nil {
		create.SetActions(p.Actions)
	}
	if p.Conditions != nil {
		create.SetConditions(storeConditionsToEnt(p.Conditions))
	}
	if p.Labels != nil {
		create.SetLabels(p.Labels)
	}
	if p.Annotations != nil {
		create.SetAnnotations(p.Annotations)
	}
	if p.CreatedBy != "" {
		create.SetCreatedBy(p.CreatedBy)
	}

	created, err := create.Save(ctx)
	if err != nil {
		return mapError(err)
	}

	p.Created = created.Created
	p.Updated = created.Updated
	return nil
}

// GetPolicy retrieves a policy by ID.
func (s *PolicyStore) GetPolicy(ctx context.Context, id string) (*store.Policy, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return nil, err
	}

	p, err := s.client.AccessPolicy.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}

	return entPolicyToStore(p), nil
}

// UpdatePolicy updates an existing policy.
func (s *PolicyStore) UpdatePolicy(ctx context.Context, p *store.Policy) error {
	uid, err := parseUUID(p.ID)
	if err != nil {
		return err
	}

	update := s.client.AccessPolicy.UpdateOneID(uid).
		SetName(p.Name).
		SetDescription(p.Description).
		SetScopeType(accesspolicy.ScopeType(p.ScopeType)).
		SetResourceType(p.ResourceType).
		SetEffect(accesspolicy.Effect(p.Effect)).
		SetPriority(p.Priority)

	if p.ScopeID != "" {
		update.SetScopeID(p.ScopeID)
	} else {
		update.ClearScopeID()
	}
	if p.ResourceID != "" {
		update.SetResourceID(p.ResourceID)
	} else {
		update.ClearResourceID()
	}
	if p.Actions != nil {
		update.SetActions(p.Actions)
	} else {
		update.ClearActions()
	}
	if p.Conditions != nil {
		update.SetConditions(storeConditionsToEnt(p.Conditions))
	} else {
		update.ClearConditions()
	}
	if p.Labels != nil {
		update.SetLabels(p.Labels)
	} else {
		update.ClearLabels()
	}
	if p.Annotations != nil {
		update.SetAnnotations(p.Annotations)
	} else {
		update.ClearAnnotations()
	}
	if p.CreatedBy != "" {
		update.SetCreatedBy(p.CreatedBy)
	}

	updated, err := update.Save(ctx)
	if err != nil {
		return mapError(err)
	}

	p.Updated = updated.Updated
	return nil
}

// DeletePolicy removes a policy by ID. Also removes all policy bindings.
func (s *PolicyStore) DeletePolicy(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}

	// Delete bindings first
	_, _ = s.client.PolicyBinding.Delete().
		Where(policybinding.PolicyIDEQ(uid)).
		Exec(ctx)

	err = s.client.AccessPolicy.DeleteOneID(uid).Exec(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// ListPolicies returns policies matching the filter criteria.
func (s *PolicyStore) ListPolicies(ctx context.Context, filter store.PolicyFilter, opts store.ListOptions) (*store.ListResult[store.Policy], error) {
	query := s.client.AccessPolicy.Query()

	if filter.Name != "" {
		query.Where(accesspolicy.NameEQ(filter.Name))
	}
	if filter.ScopeType != "" {
		query.Where(accesspolicy.ScopeTypeEQ(accesspolicy.ScopeType(filter.ScopeType)))
	}
	if filter.ScopeID != "" {
		query.Where(accesspolicy.ScopeIDEQ(filter.ScopeID))
	}
	if filter.ResourceType != "" {
		query.Where(accesspolicy.ResourceTypeEQ(filter.ResourceType))
	}
	if filter.Effect != "" {
		query.Where(accesspolicy.EffectEQ(accesspolicy.Effect(filter.Effect)))
	}

	totalCount, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	policies, err := query.
		Order(accesspolicy.ByCreated()).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]store.Policy, 0, len(policies))
	for _, p := range policies {
		items = append(items, *entPolicyToStore(p))
	}

	return &store.ListResult[store.Policy]{
		Items:      items,
		TotalCount: totalCount,
	}, nil
}

// AddPolicyBinding binds a principal (user, group, or agent) to a policy.
func (s *PolicyStore) AddPolicyBinding(ctx context.Context, binding *store.PolicyBinding) error {
	policyUID, err := parseUUID(binding.PolicyID)
	if err != nil {
		return err
	}

	principalUID, err := parseUUID(binding.PrincipalID)
	if err != nil {
		return err
	}

	create := s.client.PolicyBinding.Create().
		SetPolicyID(policyUID).
		SetPrincipalType(policybinding.PrincipalType(binding.PrincipalType))

	switch binding.PrincipalType {
	case store.PolicyPrincipalTypeUser:
		create.SetUserID(principalUID)
	case store.PolicyPrincipalTypeGroup:
		create.SetGroupID(principalUID)
	case store.PolicyPrincipalTypeAgent:
		create.SetAgentID(principalUID)
	default:
		return fmt.Errorf("%w: unsupported principal type %q", store.ErrInvalidInput, binding.PrincipalType)
	}

	_, err = create.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// RemovePolicyBinding removes a binding from a policy.
func (s *PolicyStore) RemovePolicyBinding(ctx context.Context, policyID, principalType, principalID string) error {
	policyUID, err := parseUUID(policyID)
	if err != nil {
		return err
	}

	principalUID, err := parseUUID(principalID)
	if err != nil {
		return err
	}

	preds := []predicate.PolicyBinding{
		policybinding.PolicyIDEQ(policyUID),
		policybinding.PrincipalTypeEQ(policybinding.PrincipalType(principalType)),
	}

	switch principalType {
	case store.PolicyPrincipalTypeUser:
		preds = append(preds, policybinding.UserIDEQ(principalUID))
	case store.PolicyPrincipalTypeGroup:
		preds = append(preds, policybinding.GroupIDEQ(principalUID))
	case store.PolicyPrincipalTypeAgent:
		preds = append(preds, policybinding.AgentIDEQ(principalUID))
	default:
		return fmt.Errorf("%w: unsupported principal type %q", store.ErrInvalidInput, principalType)
	}

	count, err := s.client.PolicyBinding.Delete().
		Where(preds...).
		Exec(ctx)
	if err != nil {
		return err
	}
	if count == 0 {
		return store.ErrNotFound
	}
	return nil
}

// GetPolicyBindings returns all bindings for a policy.
func (s *PolicyStore) GetPolicyBindings(ctx context.Context, policyID string) ([]store.PolicyBinding, error) {
	policyUID, err := parseUUID(policyID)
	if err != nil {
		return nil, err
	}

	bindings, err := s.client.PolicyBinding.Query().
		Where(policybinding.PolicyIDEQ(policyUID)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]store.PolicyBinding, 0, len(bindings))
	for _, b := range bindings {
		result = append(result, entBindingToStore(b))
	}

	return result, nil
}

// entBindingToStore converts an Ent PolicyBinding to a store.PolicyBinding.
func entBindingToStore(b *ent.PolicyBinding) store.PolicyBinding {
	sb := store.PolicyBinding{
		PrincipalType: string(b.PrincipalType),
	}
	if b.PolicyID != nil {
		sb.PolicyID = b.PolicyID.String()
	}
	switch b.PrincipalType {
	case policybinding.PrincipalTypeUser:
		if b.UserID != nil {
			sb.PrincipalID = b.UserID.String()
		}
	case policybinding.PrincipalTypeGroup:
		if b.GroupID != nil {
			sb.PrincipalID = b.GroupID.String()
		}
	case policybinding.PrincipalTypeAgent:
		if b.AgentID != nil {
			sb.PrincipalID = b.AgentID.String()
		}
	}
	return sb
}

// GetPoliciesForPrincipal returns all policies bound to a specific principal.
func (s *PolicyStore) GetPoliciesForPrincipal(ctx context.Context, principalType, principalID string) ([]store.Policy, error) {
	return s.GetPoliciesForPrincipals(ctx, []store.PrincipalRef{{Type: principalType, ID: principalID}})
}

// GetPoliciesForPrincipals returns all policies bound to any of the given principals.
// Results are ordered by scope_type (hub < project < resource) then priority ASC.
func (s *PolicyStore) GetPoliciesForPrincipals(ctx context.Context, principals []store.PrincipalRef) ([]store.Policy, error) {
	if len(principals) == 0 {
		return nil, nil
	}

	// Build OR predicates across the binding edges
	var bindingPreds []predicate.PolicyBinding
	for _, p := range principals {
		uid, err := parseUUID(p.ID)
		if err != nil {
			continue
		}
		switch p.Type {
		case store.PolicyPrincipalTypeUser:
			bindingPreds = append(bindingPreds, policybinding.And(
				policybinding.PrincipalTypeEQ(policybinding.PrincipalTypeUser),
				policybinding.UserIDEQ(uid),
			))
		case store.PolicyPrincipalTypeGroup:
			bindingPreds = append(bindingPreds, policybinding.And(
				policybinding.PrincipalTypeEQ(policybinding.PrincipalTypeGroup),
				policybinding.GroupIDEQ(uid),
			))
		case store.PolicyPrincipalTypeAgent:
			bindingPreds = append(bindingPreds, policybinding.And(
				policybinding.PrincipalTypeEQ(policybinding.PrincipalTypeAgent),
				policybinding.AgentIDEQ(uid),
			))
		}
	}

	if len(bindingPreds) == 0 {
		return nil, nil
	}

	// Query policies that have bindings matching any of the principals
	policies, err := s.client.AccessPolicy.Query().
		Where(accesspolicy.HasBindingsWith(
			policybinding.Or(bindingPreds...),
		)).
		Order(
			accesspolicy.ByScopeType(),
			accesspolicy.ByPriority(),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]store.Policy, 0, len(policies))
	for _, p := range policies {
		result = append(result, *entPolicyToStore(p))
	}

	return result, nil
}
