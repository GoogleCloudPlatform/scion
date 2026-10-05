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

package agent

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// expire marks the failures held for keys (all of them when keys is empty)
// as expired.
func (m *FailureMemo) expire(keys ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	past := time.Now().Add(-time.Second)
	for k, e := range m.entries {
		match := len(keys) == 0
		for _, want := range keys {
			match = match || k == want
		}
		if match {
			e.expiresAt = past
			m.entries[k] = e
		}
	}
}

func TestFailureMemo_ZeroValue(t *testing.T) {
	var m FailureMemo
	if err := m.Recent("k"); err != nil {
		t.Fatalf("Recent on an empty memo = %v, want nil", err)
	}
	m.Clear("k")
	if n := m.Len(); n != 0 {
		t.Fatalf("Len = %d, want 0", n)
	}
}

func TestFailureMemo_RecordSetsTTL(t *testing.T) {
	// A remembered failure lasts one minute: long enough to spare repeated
	// lookups close together, short enough that a fix shows up quickly.
	if FailureMemoTTL != time.Minute {
		t.Fatalf("FailureMemoTTL = %v, want %v", FailureMemoTTL, time.Minute)
	}
	var m FailureMemo
	before := time.Now()
	m.Record("k", errors.New("not found"))
	after := time.Now()

	m.mu.Lock()
	got := m.entries["k"].expiresAt
	m.mu.Unlock()
	if got.Before(before.Add(FailureMemoTTL)) || got.After(after.Add(FailureMemoTTL)) {
		t.Fatalf("expiresAt = %v, want within [%v, %v]", got, before.Add(FailureMemoTTL), after.Add(FailureMemoTTL))
	}
	if err := m.Recent("k"); err == nil || err.Error() != "not found" {
		t.Fatalf("Recent = %v, want the recorded error", err)
	}
}

func TestFailureMemo_ExpiredFailureIsNotServed(t *testing.T) {
	var m FailureMemo
	m.Record("k", errors.New("not found"))
	m.expire("k")
	if err := m.Recent("k"); err != nil {
		t.Fatalf("Recent after expiry = %v, want nil", err)
	}
	if n := m.Len(); n != 0 {
		t.Fatalf("an expired entry must be dropped when looked up; Len = %d", n)
	}
}

func TestFailureMemo_RecordBelowCapKeepsExpired(t *testing.T) {
	var m FailureMemo
	m.Record("old", errors.New("old"))
	m.expire("old")
	m.Record("new", errors.New("new"))
	// Below the limit, Record does not sweep: the expired entry is still
	// held but is never served.
	if n := m.Len(); n != 2 {
		t.Fatalf("Len = %d, want 2 (no sweep below the limit)", n)
	}
	if m.Recent("new") == nil {
		t.Fatal("the new failure must be remembered")
	}
	if err := m.Recent("old"); err != nil {
		t.Fatalf("Recent(old) = %v, want nil for an expired entry", err)
	}
	if n := m.Len(); n != 1 {
		t.Fatalf("Len = %d, want 1 (the expired entry is dropped on lookup)", n)
	}
}

func TestFailureMemo_RecordAtCapSweepsExpired(t *testing.T) {
	var m FailureMemo
	for i := 0; i < FailureMemoMaxEntries; i++ {
		m.Record(fmt.Sprintf("k%d", i), errors.New("not found"))
	}
	m.expire("k1", "k2", "k3")
	m.Record("new", errors.New("not found"))
	if m.Recent("new") == nil {
		t.Fatal("a new key must be remembered once expired entries make room at the limit")
	}
	if n := m.Len(); n != FailureMemoMaxEntries-2 {
		t.Fatalf("Len = %d, want %d (three expired entries dropped, one added)", n, FailureMemoMaxEntries-2)
	}
	if m.Recent("k0") == nil {
		t.Fatal("an unexpired entry must survive the sweep")
	}
}

func TestFailureMemo_Clear(t *testing.T) {
	var m FailureMemo
	m.Record("k", errors.New("not found"))
	m.Clear("k")
	if err := m.Recent("k"); err != nil {
		t.Fatalf("Recent after Clear = %v, want nil", err)
	}
}

func TestFailureMemo_Capped(t *testing.T) {
	var m FailureMemo
	for i := 0; i < FailureMemoMaxEntries; i++ {
		m.Record(fmt.Sprintf("k%d", i), errors.New("not found"))
	}
	m.Record("one-more", errors.New("not found"))
	if n := m.Len(); n != FailureMemoMaxEntries {
		t.Fatalf("Len = %d, want %d", n, FailureMemoMaxEntries)
	}
	if m.Recent("one-more") != nil {
		t.Fatal("a new key must not be remembered at the cap")
	}

	// An existing key can still be refreshed at the cap.
	m.Record("k0", errors.New("again"))
	if err := m.Recent("k0"); err == nil || err.Error() != "again" {
		t.Fatalf("Recent(k0) = %v, want again", err)
	}

	// Once entries expire, there is room again.
	m.expire()
	m.Record("after-expiry", errors.New("not found"))
	if m.Recent("after-expiry") == nil {
		t.Fatal("a failure must be remembered once expired entries are dropped")
	}
	if n := m.Len(); n != 1 {
		t.Fatalf("Len = %d, want 1", n)
	}
}
