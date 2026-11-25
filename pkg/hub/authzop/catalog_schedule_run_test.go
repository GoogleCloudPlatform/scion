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

package authzop

import (
	"slices"
	"strings"
	"testing"
)

// TestCatalog_ScheduleEventRunContract pins the catalog contract for
// POST /api/v1/projects/{projectId}/schedules/{id}/run (ptone/scion#9173).
// Unlike schedule.event.update (PATCH / resume), /run inserts a manual
// scheduled event carrying the caller's live attribution and ceiling without
// altering the schedule row's stored attribution or cron timing, and fires it
// immediately (delivering a message, starting an exec run, or dispatching an
// agent). Revert/mutation checks: moving /run back to schedule.event.update,
// dropping EffectCreateResource / EffectGrantAuthority / EffectEmitExternal,
// dropping ExternalPolicy or AuthBeforeEmit, or regressing the description /
// delegation / audit text to update/resume semantics fails this test.
func TestCatalog_ScheduleEventRunContract(t *testing.T) {
	const runPattern = "/api/v1/projects/{projectId}/schedules/{id}/run"

	var runSpec, updateSpec *OperationSpec
	for i := range Catalog {
		switch Catalog[i].ID {
		case "schedule.event.run":
			runSpec = &Catalog[i]
		case "schedule.event.update":
			updateSpec = &Catalog[i]
		}
		for _, ep := range Catalog[i].EntryPoints {
			if ep.Kind == EntryPointHTTPRoute && ep.Method == "POST" && ep.Pattern == runPattern {
				if Catalog[i].ID != "schedule.event.run" {
					t.Errorf("POST %s is catalogued under %q, want %q", runPattern, Catalog[i].ID, "schedule.event.run")
				}
			}
		}
	}
	if runSpec == nil {
		t.Fatal("schedule.event.run is not catalogued")
	}
	if updateSpec == nil {
		t.Fatal("schedule.event.update is not catalogued")
	}

	// Entry point ownership: /run belongs solely to schedule.event.run.
	if len(runSpec.EntryPoints) != 1 ||
		runSpec.EntryPoints[0].Kind != EntryPointHTTPRoute ||
		runSpec.EntryPoints[0].Method != "POST" ||
		runSpec.EntryPoints[0].Pattern != runPattern {
		t.Errorf("schedule.event.run entry points = %+v, want only POST %s", runSpec.EntryPoints, runPattern)
	}

	// Authorization semantics match schedule.event.update.
	if runSpec.BasePermission != updateSpec.BasePermission {
		t.Errorf("BasePermission = %q, want %q", runSpec.BasePermission, updateSpec.BasePermission)
	}
	if !slices.Equal(runSpec.Principals, updateSpec.Principals) {
		t.Errorf("Principals = %v, want %v", runSpec.Principals, updateSpec.Principals)
	}
	if !slices.Equal(runSpec.Credentials, updateSpec.Credentials) {
		t.Errorf("Credentials = %v, want %v", runSpec.Credentials, updateSpec.Credentials)
	}
	if runSpec.ResourceResolver != updateSpec.ResourceResolver {
		t.Errorf("ResourceResolver = %q, want %q", runSpec.ResourceResolver, updateSpec.ResourceResolver)
	}
	if runSpec.Bearer.Kind != updateSpec.Bearer.Kind ||
		runSpec.Bearer.Target != updateSpec.Bearer.Target ||
		!slices.Equal(runSpec.Bearer.Boundaries, updateSpec.Bearer.Boundaries) {
		t.Errorf("Bearer = %+v, want %+v", runSpec.Bearer, updateSpec.Bearer)
	}
	if runSpec.DelegationKind != DelegationNonAmplification {
		t.Errorf("DelegationKind = %q, want %q", runSpec.DelegationKind, DelegationNonAmplification)
	}

	// Required effects for manual event creation, dispatch_agent delegation,
	// and synchronous/detached external fire (message, exec, agent dispatch).
	for _, want := range []SecurityEffect{EffectCreateResource, EffectGrantAuthority, EffectEmitExternal} {
		if !slices.Contains(runSpec.Effects, want) {
			t.Errorf("schedule.event.run missing required effect %q (have %v)", want, runSpec.Effects)
		}
	}
	if slices.Contains(runSpec.Effects, EffectUpdateResource) {
		t.Errorf("schedule.event.run must not declare %q (it inserts a manual event and leaves the schedule definition untouched)", EffectUpdateResource)
	}

	// ExternalPolicy must be populated and enforce AuthBeforeEmit.
	if runSpec.ExternalPolicy == nil {
		t.Fatal("schedule.event.run missing ExternalPolicy")
	}
	if !runSpec.ExternalPolicy.AuthBeforeEmit {
		t.Error("schedule.event.run ExternalPolicy.AuthBeforeEmit must be true")
	}
	if strings.TrimSpace(runSpec.ExternalPolicy.IdempotencyKey) == "" {
		t.Error("schedule.event.run ExternalPolicy.IdempotencyKey must be non-empty")
	}
	if strings.TrimSpace(runSpec.ExternalPolicy.RetryPolicy) == "" {
		t.Error("schedule.event.run ExternalPolicy.RetryPolicy must be non-empty")
	}

	// Description, DelegationDescription, and AuditObligation must describe
	// manual-run semantics rather than update/resume schedule-row rewrites.
	descLower := strings.ToLower(runSpec.Description)
	if !strings.Contains(descLower, "manual") {
		t.Errorf("Description should mention manual run semantics: %q", runSpec.Description)
	}
	if strings.Contains(descLower, "resume") {
		t.Errorf("Description should not describe resume semantics: %q", runSpec.Description)
	}

	delegLower := strings.ToLower(runSpec.DelegationDescription)
	if !strings.Contains(delegLower, "manual") || !strings.Contains(delegLower, "untouched") {
		t.Errorf("DelegationDescription should state that attribution/ceiling is recorded on the manual event leaving the schedule row untouched: %q", runSpec.DelegationDescription)
	}
	if strings.Contains(delegLower, "in place of the stored ones") {
		t.Errorf("DelegationDescription still describes overwriting stored schedule attribution: %q", runSpec.DelegationDescription)
	}

	if runSpec.AuditObligation == nil {
		t.Fatal("schedule.event.run missing AuditObligation")
	}
	auditLower := strings.ToLower(runSpec.AuditObligation.NonAtomicJustification)
	if !strings.Contains(auditLower, "manual") || !strings.Contains(auditLower, "untouched") {
		t.Errorf("AuditObligation.NonAtomicJustification should describe manual event insert leaving the schedule row untouched: %q", runSpec.AuditObligation.NonAtomicJustification)
	}
	if strings.Contains(auditLower, "bumps its authorization revision") {
		t.Errorf("AuditObligation.NonAtomicJustification still describes bumping schedule authorization revision: %q", runSpec.AuditObligation.NonAtomicJustification)
	}
}
