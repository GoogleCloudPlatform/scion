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

// Association relationships for launch delivery (ptone/scion#2129).
//
// A project-, hub- or broker-scope secret or env var reaches an agent at
// launch through the association between the agent's stored record and the
// material's scope: the material is in the agent's project, on this hub, or
// on the agent's runtime broker. The association is a reason to authorize,
// not authority on its own. It is a relationship candidate like progeny, so
// the deliver request still passes every relationship stage, the delivery
// credential restriction, and the step-10 admission walk
// (hubDeliveryAdmission), which requires the target's recorded effect
// ceiling to allow the exact deliver permission and the provenance root to
// be live and admitted to the target's project.
//
// The candidate is built only for the internal delivery credential
// (hubDeliveryIdentity) with ActionDeliver, for secret.deliver or
// env_var.deliver. Its fact reads only stored records: the material record
// the resource names (re-read here by key and scope, and matched by ID), the
// credential's stored agent, and the agent's project or broker record. It
// never reads request input. It has no sharing source, so relationship
// stage 4 does not apply to it.

import (
	"context"
	"errors"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// materialRecordKind names the store a delivery item's record lives in.
type materialRecordKind string

const (
	materialRecordSecret materialRecordKind = "secret"
	materialRecordEnvVar materialRecordKind = "env_var"
)

// materialDeliveryItem names one stored material record a deliver request
// is for. It is carried on Resource.deliveryItem, which only hub code can
// set (materialDeliveryResource).
type materialDeliveryItem struct {
	// Record is the store the record lives in. A legacy secret-flagged env
	// var row is requested as kind secret with Record env_var.
	Record materialRecordKind
	// Key, Scope and ScopeID locate the record. ScopeID is the scope ID the
	// caller derived for this agent (the agent's project, the hub's own
	// ID, or the agent's broker), never the record's own field.
	Key     string
	Scope   string
	ScopeID string
	// TargetAgentID is the agent the item is delivered to. It must equal
	// the delivery credential's bound agent.
	TargetAgentID string
}

// materialAssociationPermissions are the permissions an association
// candidate may carry, with the resource type each is registered on.
var materialAssociationPermissions = map[string]string{
	"secret.deliver":  "secret",
	"env_var.deliver": "env_var",
}

// materialAssociationRule maps a delivery item's scope to its association
// rule. Any other scope (user scope included) has no association rule.
func materialAssociationRule(scope string) (RelationshipRuleID, bool) {
	switch scope {
	case store.ScopeProject:
		return RelationshipRuleProjectAssociation, true
	case store.ScopeHub:
		return RelationshipRuleHubAssociation, true
	case store.ScopeRuntimeBroker:
		return RelationshipRuleBrokerAssociation, true
	default:
		return "", false
	}
}

// materialAssociationCandidate returns the association candidate for a
// deliver request of a hub_delivery principal, when the request names a
// delivery item in an associated scope. No other principal, action,
// permission or resource gets one.
func (a *AuthzService) materialAssociationCandidate(principal PrincipalContext, resource Resource, action Action, permissionID string) (relationshipCandidate, bool) {
	h, ok := principal.Identity.(*hubDeliveryIdentity)
	if !ok || h == nil || action != ActionDeliver {
		return relationshipCandidate{}, false
	}
	if want, ok := materialAssociationPermissions[permissionID]; !ok || resource.Type != want {
		return relationshipCandidate{}, false
	}
	item := resource.deliveryItem
	if item == nil {
		return relationshipCandidate{}, false
	}
	rule, ok := materialAssociationRule(item.Scope)
	if !ok {
		return relationshipCandidate{}, false
	}
	return relationshipCandidate{
		rule: rule,
		fact: func(ctx context.Context) (*SharingSource, bool, string) {
			holds, detail := a.materialAssociationFact(ctx, h, rule, resource)
			return nil, holds, detail
		},
		decision: Decision{
			Allowed:      true,
			Reason:       "relationship grant: " + string(rule),
			Scope:        ScopeTypeRelationship,
			MatchedGrant: string(rule),
		},
	}, true
}

// materialAssociationFact decides whether the delivery item on resource is
// associated with the credential's stored agent under rule. It returns the
// candidate detail on failure. Every lookup error fails closed.
//
//   - A1: the credential is bound to its own agent and carries the stored
//     agent evidence; the item names that agent as its target.
//   - A2: the item's scope matches the rule, its record kind matches the
//     resource type, and the resource's parent is the item's scope.
//   - A3: the association: project items are in the agent's stored
//     project and broker items are on the agent's stored runtime broker,
//     both checked against the stored agent. For hub items the hub ID is
//     trusted caller input, not checked here: AuthzService does not hold
//     the hub's ID, so the fact requires only that the caller named one.
//     Only hub code can name a delivery item (materialDeliveryResource),
//     and launch selection names its own hub ID (the dispatcher's hub ID
//     for env vars, the secret backend's for secrets). A4 then requires
//     the record to exist under that ID.
//   - A4: the stored record exists under that key and scope, has the
//     resource's ID, and is deliverable (a secret is not internal; an env
//     var's secret flag matches the requested kind).
//   - A5: the live record behind the association exists: the agent's
//     project for project and hub items, the agent's broker for broker
//     items.
func (a *AuthzService) materialAssociationFact(ctx context.Context, h *hubDeliveryIdentity, rule RelationshipRuleID, resource Resource) (bool, string) {
	// A1.
	if h == nil || h.evidence == nil || h.evidence.agent == nil {
		return false, "delivery credential has no stored-agent evidence"
	}
	if h.boundAgentID == "" || h.boundAgentID != h.agentID {
		return false, "delivery credential is bound to a different agent"
	}
	agent := h.evidence.agent
	if agent.ID != h.agentID {
		return false, "delivery credential is bound to a different agent"
	}
	item := resource.deliveryItem
	if item == nil {
		return false, "delivery item is not named"
	}
	if item.TargetAgentID == "" || item.TargetAgentID != h.boundAgentID {
		return false, "delivery item is for a different agent"
	}

	// A2.
	if want, ok := materialAssociationRule(item.Scope); !ok || want != rule {
		return false, "delivery item scope does not match the association"
	}
	switch {
	case resource.Type == "env_var" && item.Record != materialRecordEnvVar:
		return false, "delivery item record does not match its kind"
	case resource.Type == "secret" && item.Record != materialRecordSecret && item.Record != materialRecordEnvVar:
		return false, "delivery item record does not match its kind"
	case resource.Type != "secret" && resource.Type != "env_var":
		return false, "delivery item kind is not deliverable by association"
	}
	if resource.ID == "" || item.Key == "" {
		return false, "delivery item is not named"
	}

	// A3.
	var scopeID string
	switch rule {
	case RelationshipRuleProjectAssociation:
		scopeID = agent.ProjectID
	case RelationshipRuleBrokerAssociation:
		scopeID = agent.RuntimeBrokerID
	case RelationshipRuleHubAssociation:
		// Trusted caller input (see A3 above): the hub ID the hub-internal
		// caller named. There is no stored agent field to compare it with.
		scopeID = item.ScopeID
	}
	if scopeID == "" {
		return false, "delivery item scope does not apply to the target agent"
	}
	if rule != RelationshipRuleHubAssociation && item.ScopeID != scopeID {
		return false, "delivery item is not associated with the target agent"
	}
	if resource.ParentType != scopeToResourceType(item.Scope) || resource.ParentID != scopeID {
		return false, "delivery item parent does not match its scope"
	}

	if a.store == nil {
		return false, "delivery association check failed (fail-closed): store not available"
	}

	// A4.
	if holds, detail := a.materialAssociationRecord(ctx, resource, item, scopeID); !holds {
		return false, detail
	}

	// A5.
	switch rule {
	case RelationshipRuleProjectAssociation, RelationshipRuleHubAssociation:
		p, err := a.store.GetProject(ctx, agent.ProjectID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && p == nil) {
			return false, "target agent's project does not exist"
		}
		if err != nil {
			return false, "delivery association check failed (fail-closed): project lookup failed"
		}
	case RelationshipRuleBrokerAssociation:
		b, err := a.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && b == nil) {
			return false, "target agent's runtime broker does not exist"
		}
		if err != nil {
			return false, "delivery association check failed (fail-closed): broker lookup failed"
		}
	}
	return true, ""
}

// materialAssociationRecord re-reads the delivery item's record from the
// store and checks it is the record the resource names (A4).
func (a *AuthzService) materialAssociationRecord(ctx context.Context, resource Resource, item *materialDeliveryItem, scopeID string) (bool, string) {
	switch item.Record {
	case materialRecordSecret:
		rec, err := a.store.GetSecret(ctx, item.Key, item.Scope, scopeID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && rec == nil) {
			return false, "material record not found"
		}
		if err != nil {
			return false, "delivery association check failed (fail-closed): material lookup failed"
		}
		if rec.ID != resource.ID || rec.Scope != item.Scope || rec.ScopeID != scopeID {
			return false, "material record does not match the delivery item"
		}
		if rec.SecretType == store.SecretTypeInternal {
			return false, "material record is not deliverable"
		}
		return true, ""
	case materialRecordEnvVar:
		rec, err := a.store.GetEnvVar(ctx, item.Key, item.Scope, scopeID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && rec == nil) {
			return false, "material record not found"
		}
		if err != nil {
			return false, "delivery association check failed (fail-closed): material lookup failed"
		}
		if rec.ID != resource.ID || rec.Scope != item.Scope || rec.ScopeID != scopeID {
			return false, "material record does not match the delivery item"
		}
		// A legacy secret-flagged env var row is kind secret; a plain row
		// is kind env_var. Either mismatch denies.
		if rec.Secret != (resource.Type == "secret") {
			return false, "material record does not match its kind"
		}
		return true, ""
	default:
		return false, fmt.Sprintf("delivery item record kind %q is not deliverable", item.Record)
	}
}
