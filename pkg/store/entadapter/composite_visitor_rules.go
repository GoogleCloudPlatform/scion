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
	"strings"
	"time"

	"entgo.io/ent/dialect"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/projectvisitorexclusion"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/projectvisitorrule"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/rolebinding"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// Project visitor rules and their exclusions (ptone/scion#4603): a rule
// gives visitor access to a project to the users of a trusted issuer with
// a verified email in a domain, through per-user project-visitor bindings
// tagged with the rule's ID (role_bindings.source_rule_id).

// entProjectVisitorRuleToStore converts an Ent ProjectVisitorRule to the
// store model.
func entProjectVisitorRuleToStore(r *ent.ProjectVisitorRule) *store.ProjectVisitorRule {
	return &store.ProjectVisitorRule{
		ID:        r.ID.String(),
		ProjectID: r.ProjectID,
		Issuer:    r.Issuer,
		Domain:    r.Domain,
		NotBefore: r.NotBefore,
		ExpiresAt: r.ExpiresAt,
		CreatedBy: r.CreatedBy,
		CreatedAt: r.CreatedAt,
	}
}

// lockProjectVisitorRules returns the rules matching where, read FOR
// UPDATE on Postgres, so that a rule binding being created in another
// transaction (which reads its rule FOR SHARE, see
// RoleStore.visitorBindingSourceRule) either commits first, and its
// binding is then visible here, or waits and then sees this transaction's
// change. SQLite serializes writes, so a plain read gives the same order.
// Call it inside a transaction, before touching the rules' bindings.
func (c *CompositeStore) lockProjectVisitorRules(ctx context.Context, where ...predicate.ProjectVisitorRule) ([]*ent.ProjectVisitorRule, error) {
	q := c.client.ProjectVisitorRule.Query().Where(where...)
	if c.client.Driver().Dialect() == dialect.Postgres {
		q = q.ForUpdate()
	}
	rules, err := q.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return rules, nil
}

// lockProjectVisitorRule is lockProjectVisitorRules for the rule id; a
// missing rule returns store.ErrNotFound.
func (c *CompositeStore) lockProjectVisitorRule(ctx context.Context, id uuid.UUID) error {
	rules, err := c.lockProjectVisitorRules(ctx, projectvisitorrule.IDEQ(id))
	if err != nil {
		return err
	}
	if len(rules) == 0 {
		return store.ErrNotFound
	}
	return nil
}

// CreateProjectVisitorRule creates a project visitor rule (see
// store.Store.CreateProjectVisitorRule).
func (c *CompositeStore) CreateProjectVisitorRule(ctx context.Context, rule *store.ProjectVisitorRule) error {
	if rule == nil {
		return fmt.Errorf("%w: rule is required", store.ErrInvalidInput)
	}
	projectID := strings.TrimSpace(rule.ProjectID)
	issuer := store.NormalizeFederatedIssuer(rule.Issuer)
	domain := store.NormalizeVisitorRuleDomain(rule.Domain)
	if projectID == "" || issuer == "" {
		return fmt.Errorf("%w: a visitor rule requires a project and an issuer", store.ErrInvalidInput)
	}
	if !store.ValidVisitorRuleDomain(domain) {
		return fmt.Errorf("%w: invalid visitor rule domain %q", store.ErrInvalidInput, rule.Domain)
	}
	if rule.NotBefore != nil && rule.ExpiresAt != nil && !rule.ExpiresAt.After(*rule.NotBefore) {
		return fmt.Errorf("%w: a visitor rule's expiry must be after its start", store.ErrInvalidInput)
	}
	create := c.client.ProjectVisitorRule.Create().
		SetProjectID(projectID).
		SetIssuer(issuer).
		SetDomain(domain).
		SetNillableNotBefore(rule.NotBefore).
		SetNillableExpiresAt(rule.ExpiresAt)
	if rule.ID != "" {
		uid, err := parseUUID(rule.ID)
		if err != nil {
			return err
		}
		create.SetID(uid)
	}
	if rule.CreatedBy != "" {
		create.SetCreatedBy(rule.CreatedBy)
	}
	created, err := create.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	*rule = *entProjectVisitorRuleToStore(created)
	return nil
}

// GetProjectVisitorRule returns a project visitor rule (see
// store.Store.GetProjectVisitorRule).
func (c *CompositeStore) GetProjectVisitorRule(ctx context.Context, id string) (*store.ProjectVisitorRule, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	r, err := c.client.ProjectVisitorRule.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entProjectVisitorRuleToStore(r), nil
}

// ListProjectVisitorRules returns a project's visitor rules (see
// store.Store.ListProjectVisitorRules).
func (c *CompositeStore) ListProjectVisitorRules(ctx context.Context, projectID string) ([]*store.ProjectVisitorRule, error) {
	if projectID == "" {
		return nil, fmt.Errorf("%w: a project ID is required", store.ErrInvalidInput)
	}
	rules, err := c.client.ProjectVisitorRule.Query().
		Where(projectvisitorrule.ProjectIDEQ(projectID)).
		Order(ent.Asc(projectvisitorrule.FieldCreatedAt), ent.Asc(projectvisitorrule.FieldID)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.ProjectVisitorRule, len(rules))
	for i, r := range rules {
		out[i] = entProjectVisitorRuleToStore(r)
	}
	return out, nil
}

// ListActiveProjectVisitorRules returns the active rules of every project
// for (issuer, domain) (see store.Store.ListActiveProjectVisitorRules). The
// window is evaluated here, with ProjectVisitorRule.ActiveAt, rather than
// in SQL, so the rule matches the one role bindings follow on every
// driver.
func (c *CompositeStore) ListActiveProjectVisitorRules(ctx context.Context, issuer, domain string, at time.Time) ([]*store.ProjectVisitorRule, error) {
	issuer = store.NormalizeFederatedIssuer(issuer)
	domain = store.NormalizeVisitorRuleDomain(domain)
	if issuer == "" || domain == "" {
		return nil, nil
	}
	rules, err := c.client.ProjectVisitorRule.Query().
		Where(
			projectvisitorrule.IssuerEQ(issuer),
			projectvisitorrule.DomainEQ(domain),
		).
		Order(ent.Asc(projectvisitorrule.FieldCreatedAt), ent.Asc(projectvisitorrule.FieldID)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	var out []*store.ProjectVisitorRule
	for _, r := range rules {
		sr := entProjectVisitorRuleToStore(r)
		if sr.ActiveAt(at) {
			out = append(out, sr)
		}
	}
	return out, nil
}

// UpdateProjectVisitorRuleWindow replaces the window of a rule and of every
// binding it created, in one transaction (see
// store.Store.UpdateProjectVisitorRuleWindow).
func (c *CompositeStore) UpdateProjectVisitorRuleWindow(ctx context.Context, id string, notBefore, expiresAt *time.Time) (*store.ProjectVisitorRule, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	if notBefore != nil && expiresAt != nil && !expiresAt.After(*notBefore) {
		return nil, fmt.Errorf("%w: a visitor rule's expiry must be after its start", store.ErrInvalidInput)
	}
	var updated *store.ProjectVisitorRule
	err = c.WithTx(ctx, func(tx store.Store) error {
		txc, ok := tx.(*CompositeStore)
		if !ok {
			return fmt.Errorf("update project visitor rule window: unexpected transactional store %T", tx)
		}
		if err := txc.lockProjectVisitorRule(ctx, uid); err != nil {
			return err
		}
		ruleUpdate := txc.client.ProjectVisitorRule.UpdateOneID(uid)
		bindingUpdate := txc.client.RoleBinding.Update().
			Where(rolebinding.SourceRuleIDEQ(uid))
		if notBefore != nil {
			ruleUpdate.SetNotBefore(*notBefore)
			bindingUpdate.SetNotBefore(*notBefore)
		} else {
			ruleUpdate.ClearNotBefore()
			bindingUpdate.ClearNotBefore()
		}
		if expiresAt != nil {
			ruleUpdate.SetExpiresAt(*expiresAt)
			bindingUpdate.SetExpiresAt(*expiresAt)
		} else {
			ruleUpdate.ClearExpiresAt()
			bindingUpdate.ClearExpiresAt()
		}
		r, err := ruleUpdate.Save(ctx)
		if err != nil {
			return mapError(err)
		}
		if _, err := bindingUpdate.Save(ctx); err != nil {
			return mapError(err)
		}
		updated = entProjectVisitorRuleToStore(r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// DeleteProjectVisitorRule deletes a rule, the bindings it created and its
// exclusions, in one transaction (see store.Store.DeleteProjectVisitorRule).
func (c *CompositeStore) DeleteProjectVisitorRule(ctx context.Context, id string) ([]*store.RoleBinding, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	var deleted []*store.RoleBinding
	err = c.WithTx(ctx, func(tx store.Store) error {
		txc, ok := tx.(*CompositeStore)
		if !ok {
			return fmt.Errorf("delete project visitor rule: unexpected transactional store %T", tx)
		}
		if err := txc.lockProjectVisitorRule(ctx, uid); err != nil {
			return err
		}
		rbs, err := txc.client.RoleBinding.Query().
			Where(rolebinding.SourceRuleIDEQ(uid)).
			Order(ent.Asc(rolebinding.FieldCreated), ent.Asc(rolebinding.FieldID)).
			All(ctx)
		if err != nil {
			return mapError(err)
		}
		if _, err := txc.client.RoleBinding.Delete().
			Where(rolebinding.SourceRuleIDEQ(uid)).
			Exec(ctx); err != nil {
			return mapError(err)
		}
		if _, err := txc.client.ProjectVisitorExclusion.Delete().
			Where(projectvisitorexclusion.RuleIDEQ(uid)).
			Exec(ctx); err != nil {
			return mapError(err)
		}
		if err := txc.client.ProjectVisitorRule.DeleteOneID(uid).Exec(ctx); err != nil {
			return mapError(err)
		}
		deleted = make([]*store.RoleBinding, len(rbs))
		for i, rb := range rbs {
			deleted[i] = entRoleBindingToStore(rb)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return deleted, nil
}

// DeleteProjectVisitorRulesByProject deletes every visitor rule of a
// project, the bindings they created and their exclusions, in one
// transaction (see store.Store.DeleteProjectVisitorRulesByProject).
func (c *CompositeStore) DeleteProjectVisitorRulesByProject(ctx context.Context, projectID string) (int, error) {
	if projectID == "" {
		return 0, fmt.Errorf("%w: a project ID is required", store.ErrInvalidInput)
	}
	deleted := 0
	err := c.WithTx(ctx, func(tx store.Store) error {
		txc, ok := tx.(*CompositeStore)
		if !ok {
			return fmt.Errorf("delete project visitor rules: unexpected transactional store %T", tx)
		}
		rules, err := txc.lockProjectVisitorRules(ctx, projectvisitorrule.ProjectIDEQ(projectID))
		if err != nil {
			return err
		}
		if len(rules) == 0 {
			return nil
		}
		ids := make([]uuid.UUID, len(rules))
		for i, r := range rules {
			ids[i] = r.ID
		}
		if _, err := txc.client.RoleBinding.Delete().
			Where(rolebinding.SourceRuleIDIn(ids...)).
			Exec(ctx); err != nil {
			return mapError(err)
		}
		if _, err := txc.client.ProjectVisitorExclusion.Delete().
			Where(projectvisitorexclusion.RuleIDIn(ids...)).
			Exec(ctx); err != nil {
			return mapError(err)
		}
		n, err := txc.client.ProjectVisitorRule.Delete().
			Where(projectvisitorrule.IDIn(ids...)).
			Exec(ctx)
		if err != nil {
			return mapError(err)
		}
		deleted = n
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// DeleteInactiveVisitorGrant deletes a visitor grant by email that is not
// active at a time (see store.Store.DeleteInactiveVisitorGrant).
func (c *CompositeStore) DeleteInactiveVisitorGrant(ctx context.Context, bindingID string, at time.Time) (bool, error) {
	uid, err := uuid.Parse(bindingID)
	if err != nil {
		return false, nil
	}
	rd, err := c.visitorRoleDefinition(ctx)
	if err != nil {
		return false, err
	}
	removed := false
	err = c.WithTx(ctx, func(tx store.Store) error {
		txc, ok := tx.(*CompositeStore)
		if !ok {
			return fmt.Errorf("delete inactive visitor grant: unexpected transactional store %T", tx)
		}
		// FOR UPDATE on Postgres: a concurrent window change of the grant
		// either commits first, and the check below sees its window, or
		// waits and then finds the grant removed.
		q := txc.client.RoleBinding.Query().Where(rolebinding.IDEQ(uid))
		if txc.client.Driver().Dialect() == dialect.Postgres {
			q = q.ForUpdate()
		}
		rb, err := q.Only(ctx)
		if ent.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return mapError(err)
		}
		if rb.RoleDefinitionID == nil || *rb.RoleDefinitionID != rd.ID || rb.SourceRuleID != nil {
			return nil
		}
		if roleBindingActiveAt(rb, at) {
			return nil
		}
		if err := txc.client.RoleBinding.DeleteOneID(uid).Exec(ctx); err != nil {
			return mapError(err)
		}
		removed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return removed, nil
}

// CreateProjectVisitorExclusion records an exclusion of a user from a rule
// (see store.Store.CreateProjectVisitorExclusion).
func (c *CompositeStore) CreateProjectVisitorExclusion(ctx context.Context, ruleID, userID, createdBy string) error {
	uid, err := parseGetID(ruleID)
	if err != nil {
		return err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return fmt.Errorf("%w: a user ID is required", store.ErrInvalidInput)
	}
	if _, err := c.client.ProjectVisitorRule.Get(ctx, uid); err != nil {
		return mapError(err)
	}
	create := c.client.ProjectVisitorExclusion.Create().
		SetRuleID(uid).
		SetUserID(userID)
	if createdBy != "" {
		create.SetCreatedBy(createdBy)
	}
	// CreateBulk with one builder, not Create: a single-row Create with
	// DoNothing reads the row back and fails when the insert is skipped
	// (see armLaunchReaper). An existing exclusion is kept as it is, and
	// ON CONFLICT keeps a Postgres transaction usable.
	if err := c.client.ProjectVisitorExclusion.CreateBulk(create).
		OnConflictColumns(projectvisitorexclusion.FieldRuleID, projectvisitorexclusion.FieldUserID).
		DoNothing().
		Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// IsProjectVisitorExcluded reports whether a rule has an exclusion for a
// user (see store.Store.IsProjectVisitorExcluded).
func (c *CompositeStore) IsProjectVisitorExcluded(ctx context.Context, ruleID, userID string) (bool, error) {
	uid, err := uuid.Parse(ruleID)
	if err != nil {
		return false, nil
	}
	ok, err := c.client.ProjectVisitorExclusion.Query().
		Where(
			projectvisitorexclusion.RuleIDEQ(uid),
			projectvisitorexclusion.UserIDEQ(userID),
		).
		Exist(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return ok, nil
}
