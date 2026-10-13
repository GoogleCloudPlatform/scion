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
	"github.com/GoogleCloudPlatform/scion/pkg/ent/mutationaudit"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// MutationAuditStore implements store.MutationAuditStore using Ent ORM.
type MutationAuditStore struct {
	client *ent.Client
}

// NewMutationAuditStore creates a new Ent-backed MutationAuditStore.
func NewMutationAuditStore(client *ent.Client) *MutationAuditStore {
	return &MutationAuditStore{client: client}
}

// entMutationAuditToStore converts an Ent MutationAudit entity to a store model.
func entMutationAuditToStore(ma *ent.MutationAudit) *store.MutationAuditRecord {
	return &store.MutationAuditRecord{
		ID:                  ma.ID.String(),
		Timestamp:           ma.Timestamp,
		MutationType:        ma.MutationType,
		ActorPrincipalKind:  ma.ActorPrincipalKind,
		ActorPrincipalID:    ma.ActorPrincipalID,
		ActorCredentialID:   ma.ActorCredentialID,
		ActorCredentialType: ma.ActorCredentialType,
		TargetType:          ma.TargetType,
		TargetID:            ma.TargetID,
		BeforeSummary:       ma.BeforeSummary,
		AfterSummary:        ma.AfterSummary,
		CanDelegateResult:   ma.CanDelegateResult,
		CanDelegateReason:   ma.CanDelegateReason,

		// E.2a additive fields.
		CredentialName:              ma.CredentialName,
		CredentialBoundaryKind:      ma.CredentialBoundaryKind,
		CredentialBoundaryProjectID: ma.CredentialBoundaryProjectID,
		CredentialLabels:            ma.CredentialLabels,
		CorrelationID:               ma.CorrelationID,
		ExecutorKind:                ma.ExecutorKind,
		ExecutorID:                  ma.ExecutorID,

		// Agent delegation block.
		ActorAgentID:              ma.ActorAgentID,
		AuthorizingUserID:         ma.AuthorizingUserID,
		SourceGrantID:             ma.SourceGrantID,
		ParentGrantID:             ma.ParentGrantID,
		DelegationEdgeID:          ma.DelegationEdgeID,
		ExchangeAgentCredentialID: ma.ExchangeAgentCredentialID,
		ActorKind:                 ma.ActorKind,
		AgentDelegationCode:       ma.AgentDelegationCode,
	}
}

// CreateMutationAudit stores a new mutation audit record.
func (s *MutationAuditStore) CreateMutationAudit(ctx context.Context, record *store.MutationAuditRecord) error {
	builder := s.client.MutationAudit.Create().
		SetMutationType(record.MutationType).
		SetActorPrincipalKind(record.ActorPrincipalKind).
		SetActorPrincipalID(record.ActorPrincipalID).
		SetTargetType(record.TargetType).
		SetTargetID(record.TargetID)

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
	if record.ActorCredentialID != "" {
		builder.SetActorCredentialID(record.ActorCredentialID)
	}
	if record.ActorCredentialType != "" {
		builder.SetActorCredentialType(record.ActorCredentialType)
	}
	if record.BeforeSummary != "" {
		builder.SetBeforeSummary(record.BeforeSummary)
	}
	if record.AfterSummary != "" {
		builder.SetAfterSummary(record.AfterSummary)
	}
	if record.CanDelegateResult != "" {
		builder.SetCanDelegateResult(record.CanDelegateResult)
	}
	if record.CanDelegateReason != "" {
		builder.SetCanDelegateReason(record.CanDelegateReason)
	}
	if record.CredentialName != "" {
		builder.SetCredentialName(record.CredentialName)
	}
	if record.CredentialBoundaryKind != "" {
		builder.SetCredentialBoundaryKind(record.CredentialBoundaryKind)
	}
	if record.CredentialBoundaryProjectID != "" {
		builder.SetCredentialBoundaryProjectID(record.CredentialBoundaryProjectID)
	}
	if record.CredentialLabels != "" {
		builder.SetCredentialLabels(record.CredentialLabels)
	}
	if record.CorrelationID != "" {
		builder.SetCorrelationID(record.CorrelationID)
	}
	if record.ExecutorKind != "" {
		builder.SetExecutorKind(record.ExecutorKind)
	}
	if record.ExecutorID != "" {
		builder.SetExecutorID(record.ExecutorID)
	}
	if record.ActorAgentID != "" {
		builder.SetActorAgentID(record.ActorAgentID)
	}
	if record.AuthorizingUserID != "" {
		builder.SetAuthorizingUserID(record.AuthorizingUserID)
	}
	if record.SourceGrantID != "" {
		builder.SetSourceGrantID(record.SourceGrantID)
	}
	if record.ParentGrantID != "" {
		builder.SetParentGrantID(record.ParentGrantID)
	}
	if record.DelegationEdgeID != "" {
		builder.SetDelegationEdgeID(record.DelegationEdgeID)
	}
	if record.ExchangeAgentCredentialID != "" {
		builder.SetExchangeAgentCredentialID(record.ExchangeAgentCredentialID)
	}
	if record.ActorKind != "" {
		builder.SetActorKind(record.ActorKind)
	}
	if record.AgentDelegationCode != "" {
		builder.SetAgentDelegationCode(record.AgentDelegationCode)
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

// ListMutationAudits returns mutation audit records matching the filter.
func (s *MutationAuditStore) ListMutationAudits(ctx context.Context, filter store.MutationAuditFilter) ([]*store.MutationAuditRecord, int, error) {
	query := s.client.MutationAudit.Query()

	if filter.MutationType != "" {
		query = query.Where(mutationaudit.MutationTypeEQ(filter.MutationType))
	}
	if filter.ActorPrincipalID != "" {
		query = query.Where(mutationaudit.ActorPrincipalIDEQ(filter.ActorPrincipalID))
	}
	if filter.ActorPrincipalKind != "" {
		query = query.Where(mutationaudit.ActorPrincipalKindEQ(filter.ActorPrincipalKind))
	}
	if filter.ActorCredentialID != "" {
		query = query.Where(mutationaudit.ActorCredentialIDEQ(filter.ActorCredentialID))
	}
	if filter.TargetType != "" {
		query = query.Where(mutationaudit.TargetTypeEQ(filter.TargetType))
	}
	if filter.TargetID != "" {
		query = query.Where(mutationaudit.TargetIDEQ(filter.TargetID))
	}
	if filter.CorrelationID != "" {
		query = query.Where(mutationaudit.CorrelationIDEQ(filter.CorrelationID))
	}
	if !filter.Since.IsZero() {
		query = query.Where(mutationaudit.TimestampGTE(filter.Since))
	}
	if !filter.Until.IsZero() {
		query = query.Where(mutationaudit.TimestampLTE(filter.Until))
	}

	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, 0, mapError(err)
	}

	query = query.Order(mutationaudit.ByTimestamp())
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

	records := make([]*store.MutationAuditRecord, len(entities))
	for i, e := range entities {
		records[i] = entMutationAuditToStore(e)
	}
	return records, total, nil
}

// DeleteMutationAuditsBefore removes mutation audit records older than the given time.
func (s *MutationAuditStore) DeleteMutationAuditsBefore(ctx context.Context, before time.Time) (int, error) {
	n, err := s.client.MutationAudit.Delete().
		Where(mutationaudit.TimestampLT(before)).
		Exec(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}
