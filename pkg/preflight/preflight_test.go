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

package preflight

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func check(name string, run func(context.Context, Env) ([]Finding, error)) Check {
	return Check{Name: name, Timeout: time.Second, Run: run}
}

func findings(fs ...Finding) func(context.Context, Env) ([]Finding, error) {
	return func(context.Context, Env) ([]Finding, error) { return fs, nil }
}

// Checks run in name order; findings are sorted; info counts in neither
// Blocking nor Warnings.
func TestRunAll_OrderAndCounts(t *testing.T) {
	r := NewRegistry()
	r.Register(check("z.last", findings(Finding{Code: "z.last.w", Severity: SeverityWarn})))
	r.Register(check("a.first", findings(
		Finding{Code: "a.first.i", Severity: SeverityInfo},
		Finding{Code: "a.first.b", Severity: SeverityBlock},
	)))
	rep := r.RunAll(context.Background(), Env{})
	if rep.Version != 1 || strings.Join(rep.Checks, ",") != "a.first,z.last" {
		t.Fatalf("version %d checks %v, want 1 and [a.first z.last]", rep.Version, rep.Checks)
	}
	var codes []string
	for _, f := range rep.Findings {
		codes = append(codes, f.Check+":"+f.Code)
	}
	if got := strings.Join(codes, ","); got != "a.first:a.first.b,a.first:a.first.i,z.last:z.last.w" {
		t.Fatalf("findings = %s", got)
	}
	if rep.Blocking != 1 || rep.Warnings != 1 {
		t.Fatalf("blocking %d warnings %d, want 1 and 1 (info is not counted)", rep.Blocking, rep.Warnings)
	}
}

// An error, a panic and a timeout each give one could_not_run block finding:
// findings returned together with an error are dropped, and a timed-out
// check's late result is discarded.
func TestRunAll_CouldNotRun(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()
	slowCtxErr := make(chan error, 1)
	r := NewRegistry()
	r.Register(check("c.err", func(context.Context, Env) ([]Finding, error) {
		return []Finding{{Code: "c.err.partial", Severity: SeverityInfo}}, errors.New("boom")
	}))
	r.Register(check("c.panic", func(context.Context, Env) ([]Finding, error) { panic("bad") }))
	r.Register(Check{Name: "c.slow", Timeout: 10 * time.Millisecond, Run: func(ctx context.Context, _ Env) ([]Finding, error) {
		<-release // returns only after RunAll is done: its result is late
		slowCtxErr <- ctx.Err()
		return []Finding{{Code: "c.slow.late", Severity: SeverityInfo}}, nil
	}})
	rep := r.RunAll(context.Background(), Env{})
	// The timed-out check's context is cancelled by the time RunAll returns.
	releaseAll()
	select {
	case err := <-slowCtxErr:
		if err == nil {
			t.Fatal("the timed-out check's context was not cancelled after RunAll returned")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the released slow check never reported its context")
	}
	want := map[string]string{"c.err": "boom", "c.panic": "panic: bad", "c.slow": "timed out"}
	if len(rep.Findings) != 3 || rep.Blocking != 3 {
		t.Fatalf("findings %+v, want 3 blocking could_not_run", rep.Findings)
	}
	for _, f := range rep.Findings {
		if f.Code != f.Check+".could_not_run" || f.Severity != SeverityBlock || !strings.Contains(f.Message, want[f.Check]) {
			t.Fatalf("finding %+v, want %s.could_not_run (block) mentioning %q", f, f.Check, want[f.Check])
		}
	}
}

// A finding with an unknown severity, or a code outside the check's name,
// becomes an invalid_finding block.
func TestRunAll_InvalidFinding(t *testing.T) {
	r := NewRegistry()
	r.Register(check("c.bad", findings(Finding{Code: "c.bad.x", Severity: "fatal"})))
	r.Register(check("c.other", findings(Finding{Code: "elsewhere.x", Severity: SeverityInfo})))
	rep := r.RunAll(context.Background(), Env{})
	if len(rep.Findings) != 2 || rep.Blocking != 2 ||
		rep.Findings[0].Code != "c.bad.invalid_finding" || rep.Findings[1].Code != "c.other.invalid_finding" {
		t.Fatalf("findings %+v, want c.bad.invalid_finding and c.other.invalid_finding, both block", rep.Findings)
	}
}

// A cancelled parent context reads as cancelled, not as a timeout.
func TestRunAll_ParentCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	r := NewRegistry()
	r.Register(Check{Name: "c.wait", Timeout: time.Hour, Run: func(ctx context.Context, _ Env) ([]Finding, error) {
		<-ctx.Done()
		<-done // returns only after the test: only the select in RunAll decides
		return nil, nil
	}})
	rep := r.RunAll(ctx, Env{})
	if len(rep.Findings) != 1 || !strings.Contains(rep.Findings[0].Message, "cancelled") {
		t.Fatalf("findings %+v, want one could_not_run saying cancelled", rep.Findings)
	}
}

// A parent deadline that ends a check reads as the preflight deadline, not
// as the check's own (much longer) timeout.
func TestRunAll_ParentDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	r := NewRegistry()
	r.Register(Check{Name: "c.wait", Timeout: time.Hour, Run: func(ctx context.Context, _ Env) ([]Finding, error) {
		<-ctx.Done()
		<-done // returns only after the test: only the select in RunAll decides
		return nil, nil
	}})
	rep := r.RunAll(ctx, Env{})
	if len(rep.Findings) != 1 || !strings.Contains(rep.Findings[0].Message, "the preflight deadline passed") ||
		strings.Contains(rep.Findings[0].Message, "timed out after") {
		t.Fatalf("findings %+v, want one could_not_run saying the preflight deadline passed", rep.Findings)
	}
}

// Findings with the same check and code are ordered by their subjects.
func TestRunAll_SubjectTieBreak(t *testing.T) {
	r := NewRegistry()
	r.Register(check("c.tie", findings(
		Finding{Code: "c.tie.x", Severity: SeverityWarn, Subjects: []string{"p/b"}},
		Finding{Code: "c.tie.x", Severity: SeverityWarn, Subjects: []string{"p/a"}},
	)))
	rep := r.RunAll(context.Background(), Env{})
	if got := rep.Findings[0].Subjects[0] + "," + rep.Findings[1].Subjects[0]; got != "p/a,p/b" {
		t.Fatalf("subject order %s, want p/a,p/b", got)
	}
}

// Subjects are capped; SubjectCount keeps the full count.
func TestRunAll_SubjectsCapped(t *testing.T) {
	var subjects []string
	for i := 0; i < MaxSubjects+5; i++ {
		subjects = append(subjects, fmt.Sprintf("p/a%02d", i))
	}
	r := NewRegistry()
	r.Register(check("c.many", findings(Finding{Code: "c.many.x", Severity: SeverityWarn, Subjects: subjects})))
	f := r.RunAll(context.Background(), Env{}).Findings[0]
	if len(f.Subjects) != MaxSubjects || f.SubjectCount != MaxSubjects+5 {
		t.Fatalf("subjects %d, count %d; want %d and %d", len(f.Subjects), f.SubjectCount, MaxSubjects, MaxSubjects+5)
	}
}

// Register refuses bad names, duplicates, a missing timeout and a nil Run.
func TestRegister_Validation(t *testing.T) {
	ok := func(context.Context, Env) ([]Finding, error) { return nil, nil }
	for _, c := range []struct {
		name  string
		check Check
	}{
		{"undotted", Check{Name: "profiles", Timeout: time.Second, Run: ok}},
		{"upper case", Check{Name: "Settings.Profiles", Timeout: time.Second, Run: ok}},
		{"no timeout", Check{Name: "a.b", Run: ok}},
		{"nil run", Check{Name: "a.b", Timeout: time.Second}},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: Register did not panic", c.name)
				}
			}()
			NewRegistry().Register(c.check)
		}()
	}
	r := NewRegistry()
	r.Register(Check{Name: "a.b", Timeout: time.Second, Run: ok})
	defer func() {
		if recover() == nil {
			t.Errorf("duplicate: Register did not panic")
		}
	}()
	r.Register(Check{Name: "a.b", Timeout: time.Second, Run: ok})
}

// The profile catalog mapping, kind by kind.
func TestProfileFindingSeverity(t *testing.T) {
	cases := []struct {
		kind        config.ProfileCatalogFindingKind
		preExisting bool
		sev         Severity
		code        string
	}{
		{config.ProfileFindingActiveProfile, false, SeverityBlock, "settings.profiles.active_profile"},
		{config.ProfileFindingActiveProfile, true, SeverityWarn, "settings.profiles.active_profile.pre_existing"},
		{config.ProfileFindingProjectActiveProfile, false, SeverityBlock, "settings.profiles.project_active_profile"},
		{config.ProfileFindingProjectActiveProfile, true, SeverityWarn, "settings.profiles.project_active_profile.pre_existing"},
		{config.ProfileFindingAgentSavedProfile, false, SeverityBlock, "settings.profiles.agent_saved_profile"},
		{config.ProfileFindingAgentSavedProfile, true, SeverityWarn, "settings.profiles.agent_saved_profile.pre_existing"},
		{config.ProfileFindingProfileRuntimeMissing, false, SeverityBlock, "settings.profiles.profile_runtime_missing"},
		{config.ProfileFindingProfileRuntimeMissing, true, SeverityWarn, "settings.profiles.profile_runtime_missing.pre_existing"},
		{config.ProfileFindingDroppedUnused, false, SeverityWarn, "settings.profiles.dropped_unused_profile"},
		{config.ProfileFindingActiveProfileFallback, false, SeverityInfo, "settings.profiles.active_profile_fallback"},
		{config.ProfileFindingActiveProfileChanged, false, SeverityInfo, "settings.profiles.active_profile_changed"},
		{config.ProfileFindingRetainedBuiltinProfile, false, SeverityInfo, "settings.profiles.retained_builtin_profile"},
		{config.ProfileFindingRetainedBuiltinRuntime, false, SeverityInfo, "settings.profiles.retained_builtin_runtime"},
		{config.ProfileCatalogFindingKind("future_kind"), false, SeverityBlock, "settings.profiles.future_kind"},
	}
	for _, c := range cases {
		sev, code := ProfileFindingSeverity(config.ProfileCatalogFinding{Kind: c.kind, PreExisting: c.preExisting})
		if sev != c.sev || code != c.code {
			t.Errorf("%s (pre_existing %v) = %s %s, want %s %s", c.kind, c.preExisting, sev, code, c.sev, c.code)
		}
	}
}

// TestNewDefaultRegistry pins the built-in check list: exactly the
// settings.profiles check runs.
func TestNewDefaultRegistry(t *testing.T) {
	rep := NewDefaultRegistry().RunAll(context.Background(), Env{})
	if len(rep.Checks) != 1 || rep.Checks[0] != ProfilesCheckName {
		t.Fatalf("checks = %v, want exactly [%s]", rep.Checks, ProfilesCheckName)
	}
}
