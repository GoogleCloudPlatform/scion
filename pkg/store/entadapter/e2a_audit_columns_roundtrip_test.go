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

package entadapter

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// A round-trip test for the decision/mutation audit columns and the
// mutation audit correlation_id filter, plus backward compatibility for
// rows written without them.
// ---------------------------------------------------------------------------

func TestDecisionAuditStore_NewFieldsRoundTrip(t *testing.T) {
	client := enttest.NewClient(t)
	s := NewDecisionAuditStore(client)
	ctx := context.Background()

	record := &store.DecisionAuditRecord{
		PrincipalKind:               "user",
		PrincipalID:                 "e2a-f8-user",
		ResourceType:                "project",
		Permission:                  "read",
		Result:                      "allow",
		Reason:                      "role binding grant",
		PermissionID:                "project.read",
		CredentialName:              "e2a-f8-token-name",
		CredentialBoundaryKind:      "project",
		CredentialBoundaryProjectID: "e2a-f8-project",
		CredentialLabels:            `{"env":"prod"}`,
		ExecutorKind:                "scheduler",
		ExecutorID:                  "scheduled_event:e2a-f8",
		DeniedBy:                    "delegation_ceiling",
		CorrelationID:               "e2a-f8-correlation",
	}
	require.NoError(t, s.CreateDecisionAudit(ctx, record))
	require.NotEmpty(t, record.ID)

	got, total, err := s.ListDecisionAudits(ctx, store.DecisionAuditFilter{PrincipalID: "e2a-f8-user", Limit: 10})
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, got, 1)

	require.Equal(t, "project.read", got[0].PermissionID)
	require.Equal(t, "e2a-f8-token-name", got[0].CredentialName)
	require.Equal(t, "project", got[0].CredentialBoundaryKind)
	require.Equal(t, "e2a-f8-project", got[0].CredentialBoundaryProjectID)
	require.Equal(t, `{"env":"prod"}`, got[0].CredentialLabels)
	require.Equal(t, "scheduler", got[0].ExecutorKind)
	require.Equal(t, "scheduled_event:e2a-f8", got[0].ExecutorID)
	require.Equal(t, "delegation_ceiling", got[0].DeniedBy)
	require.Equal(t, "e2a-f8-correlation", got[0].CorrelationID)
}

func TestDecisionAuditStore_NewFieldsDefaultEmpty(t *testing.T) {
	client := enttest.NewClient(t)
	s := NewDecisionAuditStore(client)
	ctx := context.Background()

	record := &store.DecisionAuditRecord{
		PrincipalKind: "user",
		PrincipalID:   "e2a-f8-default-user",
		ResourceType:  "project",
		Permission:    "read",
		Result:        "deny",
		Reason:        "no grant",
	}
	require.NoError(t, s.CreateDecisionAudit(ctx, record))

	got, _, err := s.ListDecisionAudits(ctx, store.DecisionAuditFilter{PrincipalID: "e2a-f8-default-user", Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1)

	require.Empty(t, got[0].PermissionID)
	require.Empty(t, got[0].CredentialName)
	require.Empty(t, got[0].CredentialBoundaryKind)
	require.Empty(t, got[0].CredentialBoundaryProjectID)
	require.Empty(t, got[0].CredentialLabels)
	require.Empty(t, got[0].ExecutorKind)
	require.Empty(t, got[0].ExecutorID)
	require.Empty(t, got[0].DeniedBy)
}

func TestMutationAuditStore_NewFieldsRoundTrip(t *testing.T) {
	client := enttest.NewClient(t)
	s := NewMutationAuditStore(client)
	ctx := context.Background()

	record := &store.MutationAuditRecord{
		MutationType:                "test_mutation",
		ActorPrincipalKind:          "user",
		ActorPrincipalID:            "e2a-f8-mut-user",
		TargetType:                  "project",
		TargetID:                    "e2a-f8-mut-project",
		CredentialName:              "e2a-f8-mut-token-name",
		CredentialBoundaryKind:      "project",
		CredentialBoundaryProjectID: "e2a-f8-mut-project",
		CredentialLabels:            `{"env":"staging"}`,
		CorrelationID:               "e2a-f8-mut-correlation",
		ExecutorKind:                "schedule_evaluator",
		ExecutorID:                  "schedule:e2a-f8",
	}
	require.NoError(t, s.CreateMutationAudit(ctx, record))
	require.NotEmpty(t, record.ID)

	got, total, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{CorrelationID: "e2a-f8-mut-correlation", Limit: 10})
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, got, 1)

	require.Equal(t, "e2a-f8-mut-token-name", got[0].CredentialName)
	require.Equal(t, "project", got[0].CredentialBoundaryKind)
	require.Equal(t, "e2a-f8-mut-project", got[0].CredentialBoundaryProjectID)
	require.Equal(t, `{"env":"staging"}`, got[0].CredentialLabels)
	require.Equal(t, "e2a-f8-mut-correlation", got[0].CorrelationID)
	require.Equal(t, "schedule_evaluator", got[0].ExecutorKind)
	require.Equal(t, "schedule:e2a-f8", got[0].ExecutorID)
}

func TestMutationAuditStore_NewFieldsDefaultEmpty(t *testing.T) {
	client := enttest.NewClient(t)
	s := NewMutationAuditStore(client)
	ctx := context.Background()

	record := &store.MutationAuditRecord{
		MutationType:       "test_mutation_default",
		ActorPrincipalKind: "user",
		ActorPrincipalID:   "e2a-f8-mut-default-user",
		TargetType:         "project",
		TargetID:           "e2a-f8-mut-default-project",
	}
	require.NoError(t, s.CreateMutationAudit(ctx, record))

	got, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{ActorPrincipalID: "e2a-f8-mut-default-user", Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1)

	require.Empty(t, got[0].CredentialName)
	require.Empty(t, got[0].CredentialBoundaryKind)
	require.Empty(t, got[0].CredentialBoundaryProjectID)
	require.Empty(t, got[0].CredentialLabels)
	require.Empty(t, got[0].CorrelationID)
	require.Empty(t, got[0].ExecutorKind)
	require.Empty(t, got[0].ExecutorID)
}
