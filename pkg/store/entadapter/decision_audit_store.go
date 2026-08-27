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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionaudit"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// DecisionAuditStore implements store.DecisionAuditStore using Ent ORM.
type DecisionAuditStore struct {
	client *ent.Client
}

// NewDecisionAuditStore creates a new Ent-backed DecisionAuditStore.
func NewDecisionAuditStore(client *ent.Client) *DecisionAuditStore {
	return &DecisionAuditStore{client: client}
}

// entDecisionAuditToStore converts an Ent DecisionAudit entity to a store model.
func entDecisionAuditToStore(da *ent.DecisionAudit) *store.DecisionAuditRecord {
	return &store.DecisionAuditRecord{
		ID:             da.ID.String(),
		Timestamp:      da.Timestamp,
		PrincipalKind:  da.PrincipalKind,
		PrincipalID:    da.PrincipalID,
		CredentialID:   da.CredentialID,
		CredentialType: da.CredentialType,
		Route:          da.Route,
		ResourceType:   da.ResourceType,
		ResourceID:     da.ResourceID,
		Permission:     da.Permission,
		Result:         da.Result,
		Reason:         da.Reason,
		MatchedPolicy:  da.MatchedPolicy,
		MatchedGrant:   da.MatchedGrant,
		PolicyID:       da.PolicyID,
		CorrelationID:  da.CorrelationID,
		Sampled:        da.Sampled,
	}
}

// CreateDecisionAudit stores a new decision audit record.
func (s *DecisionAuditStore) CreateDecisionAudit(ctx context.Context, record *store.DecisionAuditRecord) error {
	builder := s.client.DecisionAudit.Create().
		SetPrincipalKind(record.PrincipalKind).
		SetPrincipalID(record.PrincipalID).
		SetResourceType(record.ResourceType).
		SetPermission(record.Permission).
		SetResult(record.Result).
		SetReason(record.Reason).
		SetSampled(record.Sampled)

	if record.ID != "" {
		uid, err := parseUUID(record.ID)
		if err != nil {
			return err
		}
		builder.SetID(uid)
	}
	if !record.Timestamp.IsZero() {
		builder.SetTimestamp(record.Timestamp)
	}
	if record.CredentialID != "" {
		builder.SetCredentialID(record.CredentialID)
	}
	if record.CredentialType != "" {
		builder.SetCredentialType(record.CredentialType)
	}
	if record.Route != "" {
		builder.SetRoute(record.Route)
	}
	if record.ResourceID != "" {
		builder.SetResourceID(record.ResourceID)
	}
	if record.MatchedPolicy != "" {
		builder.SetMatchedPolicy(record.MatchedPolicy)
	}
	if record.MatchedGrant != "" {
		builder.SetMatchedGrant(record.MatchedGrant)
	}
	if record.PolicyID != "" {
		builder.SetPolicyID(record.PolicyID)
	}
	if record.CorrelationID != "" {
		builder.SetCorrelationID(record.CorrelationID)
	}

	created, err := builder.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	record.ID = created.ID.String()
	if record.Timestamp.IsZero() {
		record.Timestamp = created.Timestamp
	}
	return nil
}

// ListDecisionAudits returns decision audit records matching the filter.
func (s *DecisionAuditStore) ListDecisionAudits(ctx context.Context, filter store.DecisionAuditFilter) ([]*store.DecisionAuditRecord, int, error) {
	query := s.client.DecisionAudit.Query()

	if filter.PrincipalID != "" {
		query = query.Where(decisionaudit.PrincipalIDEQ(filter.PrincipalID))
	}
	if filter.PrincipalKind != "" {
		query = query.Where(decisionaudit.PrincipalKindEQ(filter.PrincipalKind))
	}
	if filter.CredentialID != "" {
		query = query.Where(decisionaudit.CredentialIDEQ(filter.CredentialID))
	}
	if filter.Route != "" {
		query = query.Where(decisionaudit.RouteEQ(filter.Route))
	}
	if filter.ResourceType != "" {
		query = query.Where(decisionaudit.ResourceTypeEQ(filter.ResourceType))
	}
	if filter.ResourceID != "" {
		query = query.Where(decisionaudit.ResourceIDEQ(filter.ResourceID))
	}
	if filter.Result != "" {
		query = query.Where(decisionaudit.ResultEQ(filter.Result))
	}
	if !filter.Since.IsZero() {
		query = query.Where(decisionaudit.TimestampGTE(filter.Since))
	}
	if !filter.Until.IsZero() {
		query = query.Where(decisionaudit.TimestampLTE(filter.Until))
	}
	if filter.CorrelationID != "" {
		query = query.Where(decisionaudit.CorrelationIDEQ(filter.CorrelationID))
	}

	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, 0, mapError(err)
	}

	query = query.Order(decisionaudit.ByTimestamp())
	if filter.Limit > 0 {
		query = query.Limit(filter.Limit)
	}
	if filter.Offset > 0 {
		query = query.Offset(filter.Offset)
	}

	entities, err := query.All(ctx)
	if err != nil {
		return nil, 0, mapError(err)
	}

	records := make([]*store.DecisionAuditRecord, len(entities))
	for i, e := range entities {
		records[i] = entDecisionAuditToStore(e)
	}
	return records, total, nil
}

// DeleteDecisionAuditsBefore removes decision audit records older than the given time.
func (s *DecisionAuditStore) DeleteDecisionAuditsBefore(ctx context.Context, before time.Time) (int, error) {
	n, err := s.client.DecisionAudit.Delete().
		Where(decisionaudit.TimestampLT(before)).
		Exec(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}
