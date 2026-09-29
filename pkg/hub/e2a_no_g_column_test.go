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
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// E.2a (ptone/scion#2127, plan §3.7): G's separate verified-agent-actor
// columns are reserved and must never be written by an E code path. G adds
// its own columns/fields when it lands; this test only asserts that E.2a's
// two audit record types do not define them, so a future accidental reuse of
// these structs for G's fields (instead of G's own block/table) fails this
// test immediately, before it fails review.
// ---------------------------------------------------------------------------

// gReservedFieldNames are the column/field names plan §3.7 reserves for G's
// verified-agent actor extension, plus the fine denial code named in the
// rulings (G uses DeniedBy=agent_delegation with its own agent_delegation_code
// block). None of these may appear as a Go struct field on
// store.DecisionAuditRecord or store.MutationAuditRecord.
var gReservedFieldNames = []string{
	"ActorAgentID",
	"AuthorizingUserID",
	"SourceGrantID",
	"DelegationEdgeID",
	"AgentDelegationCode",
}

func assertNoReservedFields(t *testing.T, v any) {
	t.Helper()
	typ := reflect.TypeOf(v)
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		for _, reserved := range gReservedFieldNames {
			if name == reserved {
				t.Errorf("%s defines reserved G column %q; G owns this field in its own block/table, not E's audit records", typ.Name(), reserved)
			}
		}
	}
}

func TestDecisionAuditRecord_NoGColumns(t *testing.T) {
	assertNoReservedFields(t, store.DecisionAuditRecord{})
}

func TestMutationAuditRecord_NoGColumns(t *testing.T) {
	assertNoReservedFields(t, store.MutationAuditRecord{})
}
