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

package store

import (
	"encoding/json"
	"testing"
	"time"
)

func tp(t time.Time) *time.Time { return &t }

func TestComputeAgentDeletion(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	started := now.Add(-2 * time.Minute)

	t.Run("no marker is nil", func(t *testing.T) {
		if got := ComputeAgentDeletion(&Agent{}, now); got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})

	t.Run("soft-deleted row has no view", func(t *testing.T) {
		a := &Agent{DeletionState: DeletionStateDeleting, DeletionLeaseAt: tp(now.Add(time.Minute)), DeletedAt: now}
		if got := ComputeAgentDeletion(a, now); got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})

	t.Run("live deleting", func(t *testing.T) {
		lease := now.Add(time.Minute)
		a := &Agent{
			DeletionState: DeletionStateDeleting, DeletionClaim: 3,
			DeletionLeaseAt: tp(lease), DeletionStartedAt: tp(started),
			DeletionRequest: `{"soft":true}`,
		}
		got := ComputeAgentDeletion(a, now)
		if got == nil || got.State != DeletionStateDeleting || got.Code != "" {
			t.Fatalf("got %+v", got)
		}
		if got.LeaseExpiresAt == nil || !got.LeaseExpiresAt.Equal(lease) || got.ExpiresAt != nil {
			t.Fatalf("lease/expires = %v/%v", got.LeaseExpiresAt, got.ExpiresAt)
		}
		if !got.Soft || got.Claim != 3 || !got.StartedAt.Equal(started) {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("live finalizing reads deleting", func(t *testing.T) {
		a := &Agent{DeletionState: DeletionStateFinalizing, DeletionLeaseAt: tp(now.Add(time.Minute))}
		if got := ComputeAgentDeletion(a, now); got == nil || got.State != DeletionStateDeleting {
			t.Fatalf("got %+v", got)
		}
	})

	// Design note D4 (phase 2): the finalizing stage is visible on the
	// deleting view, and on the failed view of a lapsed finalizing row, and
	// absent for every other stored state.
	t.Run("stage marks finalizing rows only", func(t *testing.T) {
		cases := []struct {
			name  string
			a     *Agent
			state string
			stage string
		}{
			{"live finalizing", &Agent{DeletionState: DeletionStateFinalizing, DeletionLeaseAt: tp(now.Add(time.Minute))}, DeletionStateDeleting, DeletionStageFinalizing},
			{"lapsed finalizing", &Agent{DeletionState: DeletionStateFinalizing, DeletionLeaseAt: tp(now.Add(-time.Minute))}, DeletionStateFailed, DeletionStageFinalizing},
			{"revoke_failed finalizing", &Agent{DeletionState: DeletionStateFinalizing, DeletionLeaseAt: tp(now), DeletionCode: DeletionCodeRevokeFailed}, DeletionStateFailed, DeletionStageFinalizing},
			{"live deleting", &Agent{DeletionState: DeletionStateDeleting, DeletionLeaseAt: tp(now.Add(time.Minute))}, DeletionStateDeleting, ""},
			{"lapsed deleting", &Agent{DeletionState: DeletionStateDeleting, DeletionLeaseAt: tp(now.Add(-time.Minute))}, DeletionStateFailed, ""},
			{"failed", &Agent{DeletionState: DeletionStateFailed, DeletionCode: DeletionCodeInDoubt}, DeletionStateFailed, ""},
		}
		for _, tc := range cases {
			got := ComputeAgentDeletion(tc.a, now)
			if got == nil || got.State != tc.state || got.Stage != tc.stage {
				t.Errorf("%s: got %+v, want state %q stage %q", tc.name, got, tc.state, tc.stage)
			}
		}
	})

	t.Run("lease-expired deleting reads failed/abandoned with expiresAt", func(t *testing.T) {
		lease := now.Add(-time.Minute)
		a := &Agent{DeletionState: DeletionStateDeleting, DeletionLeaseAt: tp(lease), DeletionStartedAt: tp(started)}
		got := ComputeAgentDeletion(a, now)
		if got == nil || got.State != DeletionStateFailed || got.Code != DeletionCodeAbandoned {
			t.Fatalf("got %+v", got)
		}
		if got.LeaseExpiresAt != nil {
			t.Fatalf("expired row must not carry leaseExpiresAt")
		}
		if got.ExpiresAt == nil || !got.ExpiresAt.Equal(lease.Add(DeletionDisplayTTL)) {
			t.Fatalf("expiresAt = %v, want leaseAt+15m", got.ExpiresAt)
		}
	})

	t.Run("lease-expired deleting keeps its stored code", func(t *testing.T) {
		a := &Agent{DeletionState: DeletionStateDeleting, DeletionLeaseAt: tp(now.Add(-time.Minute)), DeletionCode: DeletionCodeRuntimeError}
		if got := ComputeAgentDeletion(a, now); got == nil || got.Code != DeletionCodeRuntimeError {
			t.Fatalf("got %+v", got)
		}
	})

	// Acceptance (u): a lease-expired finalizing row has no expiresAt.
	t.Run("lease-expired finalizing has no expiresAt", func(t *testing.T) {
		for _, code := range []string{"", DeletionCodeRevokeFailed} {
			a := &Agent{DeletionState: DeletionStateFinalizing, DeletionLeaseAt: tp(now.Add(-time.Hour)), DeletionCode: code}
			got := ComputeAgentDeletion(a, now)
			if got == nil || got.State != DeletionStateFailed || got.ExpiresAt != nil {
				t.Fatalf("code %q: got %+v", code, got)
			}
			want := code
			if want == "" {
				want = DeletionCodeAbandoned
			}
			if got.Code != want {
				t.Fatalf("code = %q, want %q", got.Code, want)
			}
		}
	})

	t.Run("failed uses failedAt+15m", func(t *testing.T) {
		failed := now.Add(-time.Minute)
		a := &Agent{
			DeletionState: DeletionStateFailed, DeletionCode: DeletionCodeConflict,
			DeletionError: "busy", DeletionFailedAt: tp(failed), DeletionLeaseAt: tp(now.Add(-10 * time.Minute)),
		}
		got := ComputeAgentDeletion(a, now)
		if got == nil || got.State != DeletionStateFailed || got.Code != DeletionCodeConflict || got.Error != "busy" {
			t.Fatalf("got %+v", got)
		}
		if got.ExpiresAt == nil || !got.ExpiresAt.Equal(failed.Add(DeletionDisplayTTL)) {
			t.Fatalf("expiresAt = %v, want failedAt+15m", got.ExpiresAt)
		}
	})

	// Review N5: with neither failedAt nor leaseAt, startedAt is the base.
	t.Run("failed with only startedAt uses startedAt+15m", func(t *testing.T) {
		a := &Agent{DeletionState: DeletionStateFailed, DeletionCode: DeletionCodeRuntimeError, DeletionStartedAt: tp(started)}
		got := ComputeAgentDeletion(a, now)
		if got == nil || got.ExpiresAt == nil || !got.ExpiresAt.Equal(started.Add(DeletionDisplayTTL)) {
			t.Fatalf("got %+v, want expiresAt = startedAt+15m", got)
		}
	})

	t.Run("failed with no timestamps has no expiresAt", func(t *testing.T) {
		a := &Agent{DeletionState: DeletionStateFailed, DeletionCode: DeletionCodeRuntimeError}
		if got := ComputeAgentDeletion(a, now); got == nil || got.ExpiresAt != nil {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("failed past expiresAt drops out of view", func(t *testing.T) {
		a := &Agent{DeletionState: DeletionStateFailed, DeletionCode: DeletionCodeRuntimeError, DeletionFailedAt: tp(now.Add(-DeletionDisplayTTL))}
		if got := ComputeAgentDeletion(a, now); got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})

	// Acceptance (z): in_doubt never expires from view.
	t.Run("in_doubt has no expiresAt", func(t *testing.T) {
		a := &Agent{DeletionState: DeletionStateFailed, DeletionCode: DeletionCodeInDoubt, DeletionFailedAt: tp(now.Add(-time.Hour))}
		got := ComputeAgentDeletion(a, now)
		if got == nil || got.Code != DeletionCodeInDoubt || got.ExpiresAt != nil {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestAgentDeletionActive(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		a    Agent
		want bool
	}{
		{"none", Agent{}, false},
		{"deleting live", Agent{DeletionState: DeletionStateDeleting, DeletionLeaseAt: tp(now.Add(time.Minute))}, true},
		{"finalizing live", Agent{DeletionState: DeletionStateFinalizing, DeletionLeaseAt: tp(now.Add(time.Minute))}, true},
		{"deleting expired", Agent{DeletionState: DeletionStateDeleting, DeletionLeaseAt: tp(now.Add(-time.Minute))}, false},
		{"deleting no lease", Agent{DeletionState: DeletionStateDeleting}, false},
		{"failed with future lease", Agent{DeletionState: DeletionStateFailed, DeletionLeaseAt: tp(now.Add(time.Minute))}, false},
	}
	for _, tc := range cases {
		if got := tc.a.DeletionActive(now); got != tc.want {
			t.Errorf("%s: DeletionActive = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDeletionPredicate_Matches(t *testing.T) {
	now := time.Now()
	one := int64(1)
	a := &Agent{DeletionState: DeletionStateDeleting, DeletionClaim: 1, DeletionLeaseAt: tp(now.Add(-time.Second))}
	cases := []struct {
		name string
		p    DeletionPredicate
		want bool
	}{
		{"empty matches", DeletionPredicate{}, true},
		{"claim match", DeletionPredicate{Claim: &one}, true},
		{"state match", DeletionPredicate{States: []string{DeletionStateFailed, DeletionStateDeleting}}, true},
		{"state miss", DeletionPredicate{States: []string{DeletionStateNone}}, false},
		{"lease expired before now", DeletionPredicate{LeaseExpiredBefore: &now}, true},
		{"deletedAt null", DeletionPredicate{DeletedAtNull: true}, true},
	}
	for _, tc := range cases {
		if got := tc.p.Matches(a); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	two := int64(2)
	if (DeletionPredicate{Claim: &two}).Matches(a) {
		t.Error("claim mismatch must not match")
	}
	if !(DeletionPredicate{States: []string{DeletionStateNone}}).Matches(&Agent{}) {
		t.Error(`"" must match a row with no marker`)
	}
	if (DeletionPredicate{DeletedAtNull: true}).Matches(&Agent{DeletedAt: now}) {
		t.Error("soft-deleted row must not match DeletedAtNull")
	}
}

func TestAgent_JSON_DeletionPopulated(t *testing.T) {
	a := Agent{ID: "a-1", DeletionState: DeletionStateDeleting, DeletionCode: "x"}
	a.Deletion = &DeletionInfo{State: DeletionStateDeleting, Claim: 2}
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	_ = json.Unmarshal(data, &m)
	d, ok := m["deletion"].(map[string]interface{})
	if !ok || d["state"] != "deleting" || d["claim"].(float64) != 2 {
		t.Fatalf("deletion = %v", m["deletion"])
	}
	for _, k := range []string{"deletionState", "DeletionState", "deletionCode", "DeletionCode"} {
		if _, ok := m[k]; ok {
			t.Errorf("raw column %q must not be serialized", k)
		}
	}
}

// The exact JSON key set of DeletionInfo (design §2.2 plus the additive
// phase-2 "stage"): stage is omitted unless set, and present as
// "finalizing" when set.
func TestDeletionInfo_JSONKeys(t *testing.T) {
	keys := func(d DeletionInfo) map[string]interface{} {
		data, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]interface{}
		_ = json.Unmarshal(data, &m)
		return m
	}
	lease := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	base := keys(DeletionInfo{State: DeletionStateDeleting, LeaseExpiresAt: &lease})
	want := []string{"state", "soft", "claim", "startedAt", "leaseExpiresAt"}
	if len(base) != len(want) {
		t.Fatalf("keys = %v, want exactly %v", base, want)
	}
	for _, k := range want {
		if _, ok := base[k]; !ok {
			t.Errorf("missing key %q in %v", k, base)
		}
	}
	full := keys(DeletionInfo{
		State: DeletionStateFailed, Code: DeletionCodeAbandoned, Error: "e",
		ExpiresAt: &lease, LeaseExpiresAt: &lease, Stage: DeletionStageFinalizing,
	})
	if len(full) != 9 || full["stage"] != "finalizing" {
		t.Fatalf("keys = %v, want 9 keys with stage=finalizing", full)
	}
}
