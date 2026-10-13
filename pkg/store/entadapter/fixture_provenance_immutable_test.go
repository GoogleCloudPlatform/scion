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
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/google/uuid"
)

// The create builders do have the stamp setters. These assertions fail to
// compile if the columns are renamed or removed, which would make the
// negative checks below vacuous.
var (
	_ interface {
		SetFixtureRootID(uuid.UUID) *ent.AgentCreate
		SetFixtureIssuerID(uuid.UUID) *ent.AgentCreate
	} = (*ent.AgentCreate)(nil)
	_ interface {
		SetFixtureRootID(uuid.UUID) *ent.ProjectCreate
		SetFixtureIssuerID(uuid.UUID) *ent.ProjectCreate
	} = (*ent.ProjectCreate)(nil)
	_ interface {
		SetFixtureRootID(uuid.UUID) *ent.ScheduleCreate
		SetFixtureIssuerID(uuid.UUID) *ent.ScheduleCreate
	} = (*ent.ScheduleCreate)(nil)
	_ interface {
		SetFixtureRootID(uuid.UUID) *ent.GroupCreate
		SetFixtureIssuerID(uuid.UUID) *ent.GroupCreate
	} = (*ent.GroupCreate)(nil)
	_ interface {
		SetCreatedBy(uuid.UUID) *ent.AgentCreate
		SetAncestry([]string) *ent.AgentCreate
	} = (*ent.AgentCreate)(nil)
	_ interface {
		SetCreatedBy(string) *ent.ProjectCreate
	} = (*ent.ProjectCreate)(nil)
	_ interface {
		SetProvenanceVersion(int) *ent.DelegationEdgeCreate
	} = (*ent.DelegationEdgeCreate)(nil)
)

// TestImmutableProvenanceColumns_NoUpdateWriters: ent generates no writer
// for the fixture provenance stamp on any update or upsert builder of
// agents, projects, schedules and groups, nor for agent created_by and
// ancestry, project created_by, or the authority provenance of delegation
// edges and service-account assignments. Go cannot assert a method's
// absence at compile time, so this checks the generated builders' method
// sets.
func TestImmutableProvenanceColumns_NoUpdateWriters(t *testing.T) {
	stamp := []string{"FixtureRootID", "FixtureIssuerID"}
	edgeProvenance := []string{
		"ProvenanceVersion", "SourcePrincipalKind", "SourcePrincipalID", "SourceCredentialKind",
		"SourceCredentialID", "SourceEventID", "SourceScheduleID", "SourceAuthorizationRevision",
		"InitiatorPrincipalKind", "InitiatorPrincipalID", "InitiatorCredentialKind", "InitiatorCredentialID",
	}
	cases := []struct {
		name     string
		builders []any
		fields   []string
	}{
		{"agent", []any{&ent.AgentUpdate{}, &ent.AgentUpdateOne{}, &ent.AgentUpsert{}, &ent.AgentUpsertOne{}, &ent.AgentUpsertBulk{}},
			append([]string{"CreatedBy", "Ancestry"}, stamp...)},
		{"project", []any{&ent.ProjectUpdate{}, &ent.ProjectUpdateOne{}, &ent.ProjectUpsert{}, &ent.ProjectUpsertOne{}, &ent.ProjectUpsertBulk{}},
			append([]string{"CreatedBy"}, stamp...)},
		{"schedule", []any{&ent.ScheduleUpdate{}, &ent.ScheduleUpdateOne{}, &ent.ScheduleUpsert{}, &ent.ScheduleUpsertOne{}, &ent.ScheduleUpsertBulk{}},
			stamp},
		{"group", []any{&ent.GroupUpdate{}, &ent.GroupUpdateOne{}, &ent.GroupUpsert{}, &ent.GroupUpsertOne{}, &ent.GroupUpsertBulk{}},
			stamp},
		{"delegation edge", []any{&ent.DelegationEdgeUpdate{}, &ent.DelegationEdgeUpdateOne{}, &ent.DelegationEdgeUpsert{}, &ent.DelegationEdgeUpsertOne{}, &ent.DelegationEdgeUpsertBulk{}},
			edgeProvenance},
		// The service-account assignment shares the authority provenance mixin.
		{"service account assignment", []any{&ent.AgentServiceAccountAssignmentUpdate{}, &ent.AgentServiceAccountAssignmentUpdateOne{},
			&ent.AgentServiceAccountAssignmentUpsert{}, &ent.AgentServiceAccountAssignmentUpsertOne{}, &ent.AgentServiceAccountAssignmentUpsertBulk{}},
			edgeProvenance},
	}
	prefixes := []string{"Set", "SetNillable", "Clear", "Add", "Append", "Update", "Reset"}
	for _, tc := range cases {
		for _, b := range tc.builders {
			typ := reflect.TypeOf(b)
			for _, f := range tc.fields {
				for _, p := range prefixes {
					if _, ok := typ.MethodByName(p + f); ok {
						t.Errorf("%s: %s has %s%s; the column must be Immutable in the ent schema", tc.name, typ, p, f)
					}
				}
			}
		}
	}
}
