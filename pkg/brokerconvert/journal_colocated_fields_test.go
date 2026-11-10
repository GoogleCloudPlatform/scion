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

package brokerconvert

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The co-located fields (StopRecheckAt, SingleHubReplica and
// RegisterItem.CredentialsCleanup) are additive within schema version 1:
// a journal without them encodes exactly as before, an older journal reads
// with them nil or false, and a journal with them round-trips.

func TestJournalColocatedFields_AbsentWhenUnset(t *testing.T) {
	j, err := NewJournal(strings.Repeat("a", 64), "legacy-id", time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	j.Register = []RegisterItem{{Key: "alpha"}}
	data, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"stopRecheckAt"`, `"singleHubReplica"`, `"credentialsCleanup"`} {
		if strings.Contains(string(data), key) {
			t.Fatalf("an unset co-located field is encoded: %s", key)
		}
	}
	if j.SchemaVersion != 1 || JournalSchemaVersion != 1 {
		t.Fatalf("schema version moved: %d / %d", j.SchemaVersion, JournalSchemaVersion)
	}

	// An older journal (the same bytes, which carry none of the fields)
	// reads with them nil or false.
	var old Journal
	if err := json.Unmarshal(data, &old); err != nil {
		t.Fatal(err)
	}
	if old.StopRecheckAt != nil || old.SingleHubReplica || old.Register[0].CredentialsCleanup != nil {
		t.Fatalf("old journal fields not zero: %+v", old)
	}
}

func TestJournalColocatedFields_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	j, err := NewJournal(strings.Repeat("b", 64), "legacy-id", now)
	if err != nil {
		t.Fatal(err)
	}
	at := now.Add(-time.Minute)
	j.StopRecheckAt = &at
	j.SingleHubReplica = true
	j.Register = []RegisterItem{
		{Key: "alpha", CredentialsCleanup: &ItemStatus{State: StepDone}},
		{Key: "beta", CredentialsCleanup: &ItemStatus{State: StepStarted, Error: "instance credentials could not be removed"}},
		{Key: "remote"},
	}
	if err := SaveJournal(dir, j, now); err != nil {
		t.Fatal(err)
	}
	got, err := LoadJournal(dir)
	if err != nil || got == nil {
		t.Fatalf("LoadJournal: %v %v", got, err)
	}
	if got.StopRecheckAt == nil || !got.StopRecheckAt.Equal(at) || !got.SingleHubReplica {
		t.Fatalf("journal fields lost: %+v", got)
	}
	if c := got.Register[0].CredentialsCleanup; c == nil || c.State != StepDone {
		t.Fatalf("alpha cleanup = %+v", c)
	}
	if c := got.Register[1].CredentialsCleanup; c == nil || c.State != StepStarted || c.Error == "" {
		t.Fatalf("beta cleanup = %+v", c)
	}
	if got.Register[2].CredentialsCleanup != nil {
		t.Fatal("a nil cleanup must stay nil (not applicable)")
	}
}
