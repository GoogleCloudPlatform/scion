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
	"testing"
)

// TestResolvedSettingsGuard_InterveningIsWiredToTheResponseTypes closes the
// residual documented on assertResponseKeySetIsTypeDetermined.
//
// THE RESIDUAL. TestResolvedSettingsGuard_InstrumentControls proves the three
// interveners WORK. Nothing proved they are INVOKED. Deleting the three-line
// loop in TestResolvedSettingsResponseShape_NoEffectiveValue removed every
// shape guard in that file and left the whole package green, because the
// control table drives the interveners directly against its own fixtures and
// never observes the production call site.
//
// HOW THIS CLOSES IT. Rather than inspect the source, this poisons the input.
// resolvedResponseTypes is swapped for one that returns a type the interveners
// are known to reject — known because the SAME fixture is a REJECT row in the
// control table, so if the fixture ever stops being rejected, that table goes
// red first and this test cannot silently invert. The subject test is then run
// against a throwaway *testing.T. If the loop is present, the subject fails and
// this passes. If the loop is gone, the subject passes on the production types
// alone and this fails.
//
// It therefore asserts the one thing the control table cannot: that something
// on the production path calls the interveners, on whatever
// resolvedResponseTypes returns.
//
// WHAT IT DOES NOT ASSERT. It does not check which of the three interveners
// fired, and it must not: that is the control table's job, and duplicating it
// here would make one change red two tables and invite someone to delete one.
// The three poisons below are one per intervener so that deleting any single
// call inside the composite is visible from this direction too, but the
// diagnosis lives in the table.
//
// ORDERING AND PARALLELISM. The swap is a package-level variable. Nothing in
// this package's guard tests calls t.Parallel, and this test restores the
// original in a Cleanup. If these tests are ever made parallel, this swap
// becomes a data race and `go test -race` will say so; do not "fix" that by
// removing the swap.
func TestResolvedSettingsGuard_InterveningIsWiredToTheResponseTypes(t *testing.T) {
	const why = "\n\n" +
		"The resolved-settings shape guard is no longer applying its interveners to\n" +
		"the response types. The most likely cause is that this loop was removed from\n" +
		"TestResolvedSettingsResponseShape_NoEffectiveValue:\n\n" +
		"    for _, v := range resolvedResponseTypes() {\n" +
		"        assertResponseKeySetIsTypeDetermined(t, v)\n" +
		"    }\n\n" +
		"Those three lines are the ONLY thing that points the interveners at the real\n" +
		"types. TestResolvedSettingsGuard_InstrumentControls stays green without them:\n" +
		"it proves the instruments work, never that anything calls them. Without the\n" +
		"loop, a field carrying `omitzero`, an embedded struct pointer, or a custom\n" +
		"MarshalJSON can hide a key from the wire while the whole package is green.\n" +
		"Restore the loop. Do not delete this test."

	orig := resolvedResponseTypes
	t.Cleanup(func() { resolvedResponseTypes = orig })

	// One poison per intervener. Each fixture is also a REJECT row in
	// TestResolvedSettingsGuard_InstrumentControls, which is what licenses the
	// inference "the subject did not fail" => "the interveners did not run".
	poisons := []struct {
		name string
		v    any
	}{
		{"tags: a type carrying omitzero", ctlOmitZero{}},
		{"structure: a type with an embedded struct pointer", ctlNestedPtr{}},
		{"behaviour: a type with a custom MarshalJSON", marshalerCtrlPointer{}},
	}

	for _, p := range poisons {
		t.Run(p.name, func(t *testing.T) {
			resolvedResponseTypes = func() []any { return []any{p.v} }

			if !subjectTestFails(t, TestResolvedSettingsResponseShape_NoEffectiveValue) {
				t.Errorf("the shape guard PASSED a type the interveners reject"+
					" (%T)%s", p.v, why)
			}
		})
	}

	// ACCEPT row, and it is not decoration. Every row above concludes "the
	// interveners ran" from "the subject failed". That inference is only sound
	// if the subject does not fail for some OTHER reason once the list is
	// swapped — a broken swap, a require tripping on a malformed input, a
	// throwaway *testing.T misbehaving. Any of those would turn all three
	// REJECT rows green-for-the-wrong-reason and this test would report the
	// loop as wired while it was not. The clean substitute distinguishes them:
	// it exercises the identical machinery with the only difference being that
	// nothing should fire.
	t.Run("control: a clean substitute leaves the subject passing", func(t *testing.T) {
		resolvedResponseTypes = func() []any { return []any{ctlClean{}} }

		if subjectTestFails(t, TestResolvedSettingsResponseShape_NoEffectiveValue) {
			t.Error("the subject failed on a CLEAN substitute.\n\n" +
				"The three REJECT rows above cannot be trusted while this is red: " +
				"they conclude \"the interveners ran\" from \"the subject failed\", " +
				"and this row exists to show the subject does not fail merely " +
				"because the type list was swapped. Diagnose this before reading " +
				"anything else in this test.")
		}
	})
}

// subjectTestFails runs a whole Test function against a throwaway *testing.T
// and reports whether it failed.
//
// Same construction as instrumentFires, and for the same two reasons: a real
// *testing.T would mark this test failed when the subject correctly fails, and
// the subject's require.* calls end in runtime.Goexit, which would tear down
// the caller's goroutine if run inline.
func subjectTestFails(t *testing.T, subject func(*testing.T)) bool {
	t.Helper()

	sub := &testing.T{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		subject(sub)
	}()
	<-done

	return sub.Failed()
}
