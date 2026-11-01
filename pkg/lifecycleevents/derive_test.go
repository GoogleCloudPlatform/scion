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

package lifecycleevents

import (
	"fmt"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// planString renders a plan compactly: "gap" for a skipped revision, then
// each transition with its distinguishing fields, then "rmcursor".
func planString(p Plan) string {
	var parts []string
	if p.SkipRevision {
		parts = append(parts, "gap")
	}
	for _, e := range p.Events {
		s := e.Transition
		var attrs []string
		for _, a := range []string{e.Cause, e.EndReason, e.ErrorCode, e.DeleteMode} {
			if a != "" {
				attrs = append(attrs, a)
			}
		}
		if e.Row == RowBefore {
			attrs = append(attrs, "before")
		} else {
			attrs = append(attrs, "after")
		}
		if len(attrs) > 0 {
			s += "(" + strings.Join(attrs, ",") + ")"
		}
		parts = append(parts, s)
	}
	if p.DeleteCursor {
		parts = append(parts, "rmcursor")
	}
	return strings.Join(parts, " ")
}

var (
	noCursor      = CursorFacts{}
	cursorMatch   = CursorFacts{Exists: true, FingerprintMatches: true}
	cursorDiffers = CursorFacts{Exists: true}
	cursorDeleted = CursorFacts{Exists: true, LastDeleted: true, FingerprintMatches: true}
	// cursorLiveDiffers: last observed live, before row now deleted.
	cursorLiveDiffers = CursorFacts{Exists: true}
)

// TestDeriveSliceOneTable is the hand-written expectation table for the
// slice 1 rules (created, step 1 deletion, resync).
func TestDeriveSliceOneTable(t *testing.T) {
	rolledBack := Cause{Kind: CauseCreateRolledBack, Stage: "dispatch"}
	cases := []struct {
		name string
		f    Facts
		want string
	}{
		{"create", Facts{Op: store.AgentWriteCreate, HasAfter: true}, "created(after)"},
		{"create ignores cursor facts", Facts{Op: store.AgentWriteCreate, HasAfter: true, Cursor: cursorDiffers}, "created(after)"},

		{"hard delete live, cursor current", Facts{Op: store.AgentWriteHardDelete, HasBefore: true, BeforeLive: true, Cursor: cursorMatch},
			"deleted(hard,before) rmcursor"},
		{"hard delete live, unobserved change", Facts{Op: store.AgentWriteHardDelete, HasBefore: true, BeforeLive: true, Cursor: cursorDiffers},
			"gap resynced(unobserved_change,before) deleted(hard,before) rmcursor"},
		{"hard delete live, no cursor", Facts{Op: store.AgentWriteHardDelete, HasBefore: true, BeforeLive: true, Cursor: noCursor},
			"resynced(no_cursor,before) deleted(hard,before) rmcursor"},
		{"soft delete live", Facts{Op: store.AgentWriteSoftDelete, HasBefore: true, BeforeLive: true, HasAfter: true, AfterDeleted: true, Cursor: cursorMatch},
			"deleted(soft,after)"},
		{"soft delete live, no cursor", Facts{Op: store.AgentWriteSoftDelete, HasBefore: true, BeforeLive: true, HasAfter: true, AfterDeleted: true, Cursor: noCursor},
			"resynced(no_cursor,before) deleted(soft,after)"},
		{"project delete live", Facts{Op: store.AgentWriteProjectDelete, HasBefore: true, BeforeLive: true, Cursor: cursorMatch},
			"deleted(project_deleted,before) rmcursor"},
		{"create rollback", Facts{Op: store.AgentWriteHardDelete, HasBefore: true, BeforeLive: true, Cursor: cursorMatch, Cause: rolledBack},
			"launch_failed(create_rolled_back,dispatch,before) deleted(create_rolled_back,before) rmcursor"},
		{"create rollback after unobserved dispatch write", Facts{Op: store.AgentWriteHardDelete, HasBefore: true, BeforeLive: true, Cursor: cursorDiffers, Cause: rolledBack},
			"gap resynced(unobserved_change,before) launch_failed(create_rolled_back,dispatch,before) deleted(create_rolled_back,before) rmcursor"},

		// N4: removal of an already soft-deleted row emits nothing.
		{"purge of observed soft delete", Facts{Op: store.AgentWritePurge, HasBefore: true, Cursor: cursorDeleted}, "rmcursor"},
		{"hard finalize of observed soft delete", Facts{Op: store.AgentWriteHardDelete, HasBefore: true, Cursor: cursorDeleted}, "rmcursor"},
		{"project delete of observed soft delete", Facts{Op: store.AgentWriteProjectDelete, HasBefore: true, Cursor: cursorDeleted}, "rmcursor"},
		{"purge, no cursor (pre-rollout)", Facts{Op: store.AgentWritePurge, HasBefore: true, Cursor: noCursor}, "rmcursor"},
		{"cause ignored on removal of deleted row", Facts{Op: store.AgentWriteHardDelete, HasBefore: true, Cursor: cursorDeleted, Cause: rolledBack}, "rmcursor"},

		// Review r6 N2: the soft delete ran unobserved.
		{"purge after unobserved soft delete", Facts{Op: store.AgentWritePurge, HasBefore: true, Cursor: cursorLiveDiffers},
			"gap resynced(unobserved_change,before) deleted(purged,before) rmcursor"},
		{"project delete after unobserved soft delete", Facts{Op: store.AgentWriteProjectDelete, HasBefore: true, Cursor: cursorLiveDiffers},
			"gap resynced(unobserved_change,before) deleted(project_deleted,before) rmcursor"},
		{"hard finalize after unobserved soft delete", Facts{Op: store.AgentWriteHardDelete, HasBefore: true, Cursor: cursorLiveDiffers},
			"gap resynced(unobserved_change,before) deleted(hard,before) rmcursor"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := planString(Derive(c.f)); got != c.want {
				t.Errorf("Derive = %q\n          want %q", got, c.want)
			}
		})
	}
}

// TestDeriveInvariantsExhaustive enumerates every combination of the slice
// 1 inputs and checks the invariants that hold for all of them.
func TestDeriveInvariantsExhaustive(t *testing.T) {
	ops := []store.AgentWriteOp{store.AgentWriteCreate, store.AgentWriteSoftDelete, store.AgentWriteHardDelete,
		store.AgentWriteProjectDelete, store.AgentWritePurge}
	cursors := []CursorFacts{noCursor, cursorMatch, cursorDiffers, cursorDeleted,
		{Exists: true, LastDeleted: true}, {Exists: true, OpenRun: "r1", FingerprintMatches: true}}
	causes := []Cause{{}, {Kind: CauseCreateRolledBack, Stage: "s"}, {Kind: CauseCreateFailed, Stage: "s"}}
	bools := []bool{false, true}
	n := 0
	for _, op := range ops {
		for _, hasBefore := range bools {
			for _, beforeLive := range bools {
				for _, hasAfter := range bools {
					for _, afterDeleted := range bools {
						for _, cur := range cursors {
							for _, cause := range causes {
								if (!hasBefore && beforeLive) || (!hasAfter && afterDeleted) || (!hasBefore && !hasAfter) {
									continue
								}
								f := Facts{Op: op, HasBefore: hasBefore, BeforeLive: beforeLive, HasAfter: hasAfter,
									AfterDeleted: afterDeleted, Cursor: cur, Cause: cause}
								n++
								checkPlanInvariants(t, f, Derive(f))
							}
						}
					}
				}
			}
		}
	}
	t.Logf("checked %d input combinations", n)
}

func checkPlanInvariants(t *testing.T, f Facts, p Plan) {
	t.Helper()
	desc := fmt.Sprintf("%+v -> %s", f, planString(p))
	last := -1
	deleted, resynced := 0, 0
	for _, e := range p.Events {
		if k := orderKey(e); k < last {
			t.Errorf("%s: transitions out of fixed order", desc)
		} else {
			last = k
		}
		switch e.Transition {
		case tc.TransitionDeleted:
			deleted++
			if !f.HasBefore || (f.HasAfter && (!f.BeforeLive || !f.AfterDeleted)) {
				t.Errorf("%s: deleted from a write that neither removes nor deletes the row", desc)
			}
			if e.ClosesOpenRun != (f.Cursor.OpenRun != "") {
				t.Errorf("%s: ClosesOpenRun does not follow the cursor's open run", desc)
			}
		case tc.TransitionResynced:
			resynced++
			if e.Row != RowBefore {
				t.Errorf("%s: resynced must snapshot the before row", desc)
			}
			if e.Gap != (e.Cause == tc.CauseUnobservedChange) || e.Gap != p.SkipRevision {
				t.Errorf("%s: gap marker inconsistent", desc)
			}
		case tc.TransitionLaunchFailed:
			if f.Cause.Kind == "" || e.EndReason != f.Cause.Kind {
				t.Errorf("%s: launch_failed without its create cause", desc)
			}
		case tc.TransitionCreated:
			if f.HasBefore && f.Op != store.AgentWriteCreate {
				t.Errorf("%s: created from a non-create write", desc)
			}
		}
		if e.Row == RowAfter && !f.HasAfter || e.Row == RowBefore && !f.HasBefore {
			t.Errorf("%s: event describes a row the write does not have", desc)
		}
	}
	if deleted > 1 || resynced > 1 {
		t.Errorf("%s: more than one deleted or resynced", desc)
	}
	if p.DeleteCursor != (f.HasBefore && !f.HasAfter && f.Op != store.AgentWriteCreate) {
		t.Errorf("%s: cursor removal must follow row removal", desc)
	}
	// Removal of an already-deleted row emits only on the N2 evidence.
	if f.HasBefore && !f.BeforeLive && !f.HasAfter && f.Op != store.AgentWriteCreate {
		want := f.Cursor.Exists && !f.Cursor.LastDeleted
		if (len(p.Events) > 0) != want {
			t.Errorf("%s: removal of a deleted row must emit only when the cursor saw it live", desc)
		}
	}
	// No cursor and not a create: the first event is resynced(no_cursor),
	// unless the write removes an already-deleted row.
	if f.HasBefore && f.Op != store.AgentWriteCreate && !f.Cursor.Exists && (f.BeforeLive || f.HasAfter) {
		if len(p.Events) == 0 || p.Events[0].Cause != tc.CauseNoCursor {
			t.Errorf("%s: missing resynced(no_cursor)", desc)
		}
	}
}
