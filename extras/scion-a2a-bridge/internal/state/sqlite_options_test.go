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

package state

import (
	"context"
	"testing"
	"time"
)

// TestSQLiteConnectionOptions verifies that every connection option in the
// data source name takes effect. The driver silently ignores options it does
// not recognize, so a DSN written in another driver's syntax would open fine
// and leave every setting at its default.
func TestSQLiteConnectionOptions(t *testing.T) {
	s := newTestSQLiteStore(t).(*SQLiteStore)

	tests := []struct {
		pragma string
		want   string
	}{
		{pragma: "journal_mode", want: "wal"},
		{pragma: "busy_timeout", want: "5000"},
		{pragma: "foreign_keys", want: "1"},
		{pragma: "synchronous", want: "1"}, // 1 = NORMAL
	}
	for _, tt := range tests {
		t.Run(tt.pragma, func(t *testing.T) {
			var got string
			if err := s.db.QueryRow("PRAGMA " + tt.pragma).Scan(&got); err != nil {
				t.Fatalf("PRAGMA %s: %v", tt.pragma, err)
			}
			if got != tt.want {
				t.Errorf("PRAGMA %s = %q, want %q", tt.pragma, got, tt.want)
			}
		})
	}
}

// TestSQLiteTimeStorageFormat verifies that time.Time values are written in
// the "2006-01-02 15:04:05.999999999-07:00" text form that earlier releases
// stored, so rows written before and after the driver change compare and sort
// consistently (for example in ListStaleActiveTasks).
func TestSQLiteTimeStorageFormat(t *testing.T) {
	s := newTestSQLiteStore(t).(*SQLiteStore)
	ctx := context.Background()

	ts := time.Date(2026, 1, 2, 3, 4, 5, 600000000, time.UTC)
	task := &Task{
		ID:        "task-time-format",
		ContextID: "ctx-time-format",
		ProjectID: "project-1",
		AgentSlug: "agent-1",
		State:     "working",
		CreatedAt: ts,
		UpdatedAt: ts,
		Metadata:  "{}",
	}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	var raw string
	if err := s.db.QueryRow(`SELECT CAST(updated_at AS TEXT) FROM tasks WHERE id = ?`, task.ID).Scan(&raw); err != nil {
		t.Fatalf("select raw updated_at: %v", err)
	}
	if want := "2026-01-02 03:04:05.6+00:00"; raw != want {
		t.Errorf("stored updated_at = %q, want %q", raw, want)
	}

	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got == nil {
		t.Fatal("GetTask returned nil")
	}
	if !got.UpdatedAt.Equal(ts) {
		t.Errorf("GetTask UpdatedAt = %v, want %v", got.UpdatedAt, ts)
	}

	// The driver may return a "+00:00" value in time.Local or in an unnamed
	// zero-offset zone rather than time.UTC. Only the instant and the stored
	// text matter here, so writing a time read back from the store must store
	// the same text as the original.
	copied := *got
	copied.ID = "task-time-format-copy"
	if err := s.CreateTask(ctx, &copied); err != nil {
		t.Fatalf("CreateTask (copy): %v", err)
	}
	var rawCopy string
	if err := s.db.QueryRow(`SELECT CAST(updated_at AS TEXT) FROM tasks WHERE id = ?`, copied.ID).Scan(&rawCopy); err != nil {
		t.Fatalf("select raw updated_at (copy): %v", err)
	}
	if rawCopy != raw {
		t.Errorf("re-written updated_at = %q, want %q", rawCopy, raw)
	}
}
