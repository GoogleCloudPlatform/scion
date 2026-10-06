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

package hub

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// --- resourceEqual must be a true whole-Resource compare -------------

// TestResourceEqual_MutationCoversEveryField is the required proof:
// reflection-fill a Resource, mutate each exported field one at a
// time, and assert resourceEqual returns false every time. A future
// Resource field is then covered automatically, because this test iterates
// reflect.TypeOf(Resource{}).NumField() rather than naming fields by hand --
// exactly the property the old hand-written field list lacked (it already
// silently omitted ScopeUserID).
func TestResourceEqual_MutationCoversEveryField(t *testing.T) {
	base := Resource{
		Type:        "agent",
		ID:          "id-1",
		OwnerID:     "owner-1",
		ParentType:  "project",
		ParentID:    "project-1",
		Labels:      map[string]string{"k": "v"},
		Ancestry:    []string{"a1", "a2"},
		ScopeKind:   "project",
		ScopeUserID: "user-1",
	}

	// Sanity: the base compares equal to an identical copy.
	require.True(t, resourceEqual(base, base))

	typ := reflect.TypeOf(base)
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		t.Run(field.Name, func(t *testing.T) {
			mutated := base
			v := reflect.ValueOf(&mutated).Elem().Field(i)
			switch v.Kind() {
			case reflect.String:
				v.SetString(v.String() + "-mutated")
			case reflect.Map:
				m := reflect.MakeMap(v.Type())
				m.SetMapIndex(reflect.ValueOf("different-key"), reflect.ValueOf("different-value"))
				v.Set(m)
			case reflect.Slice:
				v.Set(reflect.AppendSlice(reflect.MakeSlice(v.Type(), 0, 0), v))
				v.Set(reflect.Append(v, reflect.ValueOf("different-element")))
			default:
				t.Fatalf("unhandled Resource field kind %s for field %s; extend this test", v.Kind(), field.Name)
			}
			require.NotEqual(t, base, mutated, "mutation must actually change the struct (test bug if not)")
			require.False(t, resourceEqual(base, mutated),
				"resourceEqual must detect a change to Resource.%s", field.Name)
			require.False(t, resourceEqual(mutated, base),
				"resourceEqual must detect a change to Resource.%s (argument order reversed)", field.Name)
		})
	}
}

func TestResourceEqual_NilVsEmptyStillNormalizes(t *testing.T) {
	a := Resource{Type: "agent", ID: "x", Labels: nil, Ancestry: nil}
	b := Resource{Type: "agent", ID: "x", Labels: map[string]string{}, Ancestry: []string{}}
	require.True(t, resourceEqual(a, b), "nil and empty Labels/Ancestry must still compare equal after switching to reflect.DeepEqual")
}

// --- the member/full equality gate, reflection-filled, real round trip ---

// reflectFillStoreAgent returns a *store.Agent with every exported field set
// to a distinguishable non-zero value (the fixture is filled by reflection
// so that every exported store.Agent field is non-zero), via generic
// reflection plus a short list of special-cased fields that must hold a
// specific shape to round-trip through the real store (valid UUIDs, a real
// MessageMode enum value, etc.) rather than an arbitrary string.
//
// The fields below are deliberately left at their zero value, each for a
// documented, store-enforced reason rather than an oversight:
//   - Project, RuntimeBrokerName, HarnessConfig, HarnessAuth: "Enriched
//     fields (populated by Hub when returning data, not persisted)" per
//     store.Agent's own field comment (pkg/store/models.go) -- no store
//     write path can ever make these non-zero after a read-back, and
//     agentResource never reads them either. (HarnessConfig was once
//     missing from this list and from the skip map below, even though
//     it sits in the exact same "enriched, not persisted" block as the
//     other three -- the reflection fill set it, but it reads back zero,
//     same as its neighbors. assertNonSkippedFieldsNonZero below proves this
//     skip list is exhaustive rather than trusting the comment.)
//   - DeletedAt: GetAgentsByIDs (deliberately exercised here) hard-codes
//     agent.DeletedAtIsNil() -- a soft-deleted
//     fixture could never be read back through it at all. The dropped-row
//     behavior for a deleted agent is covered separately by
//     TestListProjectAgentsSorted_Race_MissingRow.
//   - The Launch* bookkeeping fields (LaunchAsyncOptIn, LaunchID,
//     LaunchState, LaunchEndReason, LaunchKind, LaunchDeadline,
//     LaunchLastReportAt, LaunchOwner, LaunchSeq, LaunchStep, LaunchError):
//     per agent_store.go's own doc comment, "UpdateAgent never sets any of
//     these from the caller's struct: they are absent from its Ent builder
//     chain entirely" -- only BeginLaunch/MarkLaunchAccepted/EndLaunch/
//     ApplyLaunchReport/RunLaunchReaperTick write them. They are also json:"-"
//     and never reach agentResource.
//   - Launch: the computed, client-facing view of the Launch* columns. No
//     store method persists or populates it; only the hub's enrichAgents
//     (via ComputeAgentLaunch) sets it on a response copy, and agentResource
//     never reads it.
//   - The Deletion* marker columns (DeletionState, DeletionClaim,
//     DeletionLeaseAt, DeletionStartedAt, DeletionFailedAt, DeletionCode,
//     DeletionError, DeletionPrior, DeletionRequest): written only through
//     UpdateAgentDeletion, never by CreateAgent/UpdateAgent, and json:"-".
//     Deletion is their computed view (ComputeAgentDeletion), set only on
//     response copies.
//   - SoftDeleteOpID: written only through SetAgentSoftDeleteOpID, and only
//     meaningful on a soft-deleted row (see DeletedAt above).
//
// Every other exported field, including Slug (which a mutation of
// agentResource to read ScopeUserID would depend on), is filled and
// persisted through CreateAgent followed by one UpdateAgent call (which
// covers the handful of fields
// CreateAgent itself does not set, e.g. ExitCode/ExitReason/
// ReincarnationState/ReincarnationUpdatedAt).
// reflectFillStoreAgentSkipFields is the single source of truth for which
// store.Agent fields cannot round-trip through the real store and so are
// deliberately left at their zero value by reflectFillStoreAgent (see its
// doc comment for why each one is here). Sharing this map with
// assertNonSkippedFieldsNonZero (rather than each keeping its own copy)
// means the skip list cannot drift out of sync with what the fill/verify
// pair actually checks -- a gap like HarnessConfig once missing from an
// independently-stated list cannot recur silently, because
// assertNonSkippedFieldsNonZero fails closed on every field not in this map.
var reflectFillStoreAgentSkipFields = map[string]bool{
	"Project": true, "RuntimeBrokerName": true, "HarnessConfig": true, "HarnessAuth": true,
	"DeletedAt":        true,
	"LaunchAsyncOptIn": true, "LaunchID": true, "LaunchState": true,
	"LaunchEndReason": true, "LaunchKind": true, "LaunchDeadline": true,
	"LaunchLastReportAt": true, "LaunchOwner": true, "LaunchSeq": true,
	"LaunchStep": true, "LaunchError": true,
	"Launch": true,
	// Deletion marker columns are written only through UpdateAgentDeletion
	// (never by CreateAgent/UpdateAgent), and Deletion is computed.
	"DeletionState": true, "DeletionClaim": true, "DeletionLeaseAt": true,
	"DeletionStartedAt": true, "DeletionFailedAt": true, "DeletionCode": true,
	"DeletionError": true, "DeletionPrior": true, "DeletionRequest": true,
	"Deletion": true,
	// run_id is written only through SetAgentRunID and the run-ID swaps
	// (ptone/scion#2550): CreateAgent and UpdateAgent never write it, so it
	// cannot round-trip here; that is the only reason it is skipped. List
	// reads do carry it (ListAgents maps full rows), and project deletion
	// dispatches deletes from one. It is not an authz input.
	"RunID": true,
	// previous_run_ids: the same writers and the same reasons
	// (ptone/scion#3097).
	"PreviousRunIDs": true,
	// workspace_placement is written only through SetAgentWorkspacePlacement
	// (ptone/scion#2727): CreateAgent and UpdateAgent never write it, so it
	// cannot round-trip here. It is not an authz input.
	"WorkspacePlacement": true,
	// Run intent columns are written only through SetRunIntent and
	// RevertRunIntent (never by CreateAgent/UpdateAgent).
	"RunIntent": true, "RunIntentAt": true, "RunIntentMarkedAt": true,
	// ProvisionedOnly is computed by the hub at response time from Phase,
	// RunIntent and the launch/deletion columns (ptone/scion#2929); it is
	// not stored.
	"ProvisionedOnly": true,
	// Start claim columns are written only through the start-claim store
	// methods (never by CreateAgent/UpdateAgent).
	"StartClaimID": true, "StartClaimKind": true, "StartClaimState": true,
	"StartClaimOwner": true, "StartClaimTarget": true, "StartClaimAt": true,
	"StartClaimLeaseUntil": true, "StartClaimUnconfirmedAt": true,
	"StartClaimHoldUntil": true, "StartClaimLaunchID": true,
	// soft_delete_op_id is written only through SetAgentSoftDeleteOpID
	// (never by CreateAgent/UpdateAgent) and is set only alongside
	// DeletedAt, which GetAgentsByIDs filters out. It is json:"-" and not
	// an authz input.
	"SoftDeleteOpID": true,
}

func reflectFillStoreAgent(t *testing.T, projectID string) *store.Agent {
	t.Helper()
	a := &store.Agent{}
	v := reflect.ValueOf(a).Elem()
	typ := v.Type()

	skip := reflectFillStoreAgentSkipFields
	special := map[string]func(reflect.Value){
		"ID":          func(f reflect.Value) { f.SetString(uuid.New().String()) },
		"ProjectID":   func(f reflect.Value) { f.SetString(projectID) },
		"OwnerID":     func(f reflect.Value) { f.SetString(uuid.New().String()) },
		"CreatedBy":   func(f reflect.Value) { f.SetString(uuid.New().String()) },
		"MessageMode": func(f reflect.Value) { f.SetString("project") },
		"AppliedConfig": func(f reflect.Value) {
			f.Set(reflect.ValueOf(&store.AgentAppliedConfig{Image: "reflect-image", Task: "reflect-task"}))
		},
		"ExposedPorts": func(f reflect.Value) {
			f.Set(reflect.ValueOf([]store.ExposedPort{{
				Port: 9999, Label: "reflect-port", Host: "reflect-host", Mode: "tcp",
				ExposedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ExposedBy: "reflect",
			}}))
		},
	}

	seq := 0
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		f := v.Field(i)
		if skip[name] {
			continue
		}
		if fn, ok := special[name]; ok {
			fn(f)
			continue
		}
		seq++
		fillGenericNonZero(t, f, name, seq)
	}
	return a
}

// fillGenericNonZero sets f (an exported, addressable store.Agent field) to
// a non-zero value appropriate to its kind. seq makes each field's value
// distinguishable from every other field's, in case a future bug swaps two
// same-typed fields.
func fillGenericNonZero(t *testing.T, f reflect.Value, name string, seq int) {
	t.Helper()
	switch f.Kind() {
	case reflect.String:
		f.SetString(fmt.Sprintf("reflect-%s-%d", name, seq))
	case reflect.Bool:
		f.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		f.SetInt(int64(seq))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		f.SetUint(uint64(seq))
	case reflect.Map:
		m := reflect.MakeMap(f.Type())
		m.SetMapIndex(reflect.ValueOf(fmt.Sprintf("k%d", seq)), reflect.ValueOf(fmt.Sprintf("v%d", seq)))
		f.Set(m)
	case reflect.Slice:
		switch f.Type().Elem().Kind() {
		case reflect.String:
			f.Set(reflect.ValueOf([]string{fmt.Sprintf("anc-%d", seq)}))
		default:
			t.Fatalf("fillGenericNonZero: unhandled slice element kind for field %s; extend this helper", name)
		}
	case reflect.Struct:
		if f.Type() == reflect.TypeOf(time.Time{}) {
			f.Set(reflect.ValueOf(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Hour)))
			return
		}
		t.Fatalf("fillGenericNonZero: unhandled struct type for field %s; extend this helper", name)
	case reflect.Pointer:
		elemType := f.Type().Elem()
		switch {
		case elemType.Kind() == reflect.Int:
			p := new(int)
			*p = seq
			f.Set(reflect.ValueOf(p))
		case elemType == reflect.TypeOf(time.Time{}):
			p := new(time.Time)
			*p = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Hour)
			f.Set(reflect.ValueOf(p))
		default:
			t.Fatalf("fillGenericNonZero: unhandled pointer element type for field %s; extend this helper", name)
		}
	default:
		t.Fatalf("fillGenericNonZero: unhandled kind %s for field %s; extend this helper", f.Kind(), name)
	}
}

// assertNonSkippedFieldsNonZero asserts every exported field of a NOT in
// reflectFillStoreAgentSkipFields is non-zero ("the test compares
// memberResource against agentResource(re-read row), so any field that does
// not round-trip is invisible to it ... assert that every non-skipped field
// of the re-read store.Agent is non-zero. That makes the fixture prove its
// own coverage."). Without this, a field silently falling out of round-trip
// (like HarnessConfig once did) would just quietly stop being exercised by
// the decision-count gate rather than failing loudly.
func assertNonSkippedFieldsNonZero(t *testing.T, a *store.Agent) {
	t.Helper()
	v := reflect.ValueOf(a).Elem()
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if reflectFillStoreAgentSkipFields[name] {
			continue
		}
		f := v.Field(i)
		if f.IsZero() {
			t.Errorf("store.Agent.%s read back as the zero value after the reflection-filled round trip; "+
				"either reflectFillStoreAgent needs to fill it, or it belongs in reflectFillStoreAgentSkipFields "+
				"with a documented reason (it is currently neither)", name)
		}
	}
}

// TestListProjectAgentsSorted_MemberProjectionEquality is the non-waivable
// member/full equality gate: a reflection-filled store.Agent, written
// through the real store and read back through the real ListAgentMembers and
// GetAgentsByIDs (not a hand-built member struct and not resourceEqual),
// must satisfy reflect.DeepEqual(memberResource(m), agentResource(full)).
// This is the test a ScopeUserID mutation probe is checked against: adding
// an agentResource input that AgentMember/ToAgent does not carry must make
// this test fail, because memberResource(m) can never reflect a field
// ToAgent() never copies.
func TestListProjectAgentsSorted_MemberProjectionEquality(t *testing.T) {
	f := sortedListSetup(t)
	ctx := context.Background()

	full := reflectFillStoreAgent(t, f.project.ID)
	require.NoError(t, f.store.CreateAgent(ctx, full))
	// CreateAgent does not persist ExitCode/ExitReason/ReincarnationState/
	// ReincarnationUpdatedAt; one UpdateAgent call (same struct, now with
	// Create's assigned StateVersion) applies those too.
	require.NoError(t, f.store.UpdateAgent(ctx, full))

	members, err := f.store.ListAgentMembers(ctx, store.AgentFilter{ProjectID: f.project.ID, IDs: []string{full.ID}}, "updated", "desc", 10)
	require.NoError(t, err)
	require.Len(t, members, 1)
	member := members[0]

	fullRows, err := f.store.GetAgentsByIDs(ctx, []string{full.ID})
	require.NoError(t, err)
	require.Contains(t, fullRows, full.ID)
	rereadFull := fullRows[full.ID]

	// Prove the fixture's own coverage rather than trusting the skip-list
	// comment -- every field NOT in reflectFillStoreAgentSkipFields
	// must actually have round-tripped non-zero, or this test's deep-equal
	// below would be silently blind to it.
	assertNonSkippedFieldsNonZero(t, rereadFull)

	require.True(t, reflect.DeepEqual(memberResource(member), agentResource(rereadFull)),
		"memberResource(ListAgentMembers row) must deep-equal agentResource(GetAgentsByIDs row) for a fully reflection-filled agent;\nmember resource: %#v\nfull resource:   %#v",
		memberResource(member), agentResource(rereadFull))
}

// TestListProjectAgentsSorted_CapsDeepEqualLegacy proves: the sorted-mode
// page's merged _capabilities must deep-equal what the legacy path computes
// for the same resource via a direct
// ComputeCapabilitiesBatch call, including action order -- not just
// "contains read". The legacy project-list user path
// (listProjectAgents, handlers_projects_core.go) calls
// ComputeCapabilitiesBatch directly, so hitting it for the same single
// agent gives an independently-computed reference to compare against,
// without needing to reconstruct a UserIdentity by hand in the test.
func TestListProjectAgentsSorted_CapsDeepEqualLegacy(t *testing.T) {
	f := sortedListSetup(t)
	f.createAgent(t, "caps-eq-1", string(state.PhaseStopped), map[string]string{"k": "v"})

	legacyRec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath(""), nil)
	require.Equal(t, http.StatusOK, legacyRec.Code, legacyRec.Body.String())
	legacy := mustDecodeListAgentsResponse(t, legacyRec.Body)
	require.Len(t, legacy.Agents, 1)

	sortedRec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&fit=500"), nil)
	require.Equal(t, http.StatusOK, sortedRec.Code, sortedRec.Body.String())
	sorted := mustDecodeListAgentsResponse(t, sortedRec.Body)
	require.Len(t, sorted.Agents, 1)

	require.Equal(t, legacy.Agents[0].Cap.Actions, sorted.Agents[0].Cap.Actions,
		"sorted mode's merged capabilities must deep-equal the legacy path's ComputeCapabilitiesBatch output, including action order")
}
