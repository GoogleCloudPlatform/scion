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

package entadapter

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
	// Embedded zone data, so TZ=Asia/Kathmandu in runUTCTextHelper
	// resolves even on an image without /usr/share/zoneinfo; otherwise
	// time.Local would silently fall back to UTC there.
	_ "time/tzdata"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readRawAgentTimes fetches agent id's created, updated and
// last_activity_event columns as the literal TEXT SQLite stores. The query
// concatenates an empty string literal onto each column: the columns have
// decltype "datetime", and scanning a bare datetime column into a string
// makes this driver reparse the stored text and reformat it as RFC3339Nano.
// The concatenated expression has no decltype, so the Scan returns the
// stored bytes unchanged. SQL-level ORDER BY and keyset comparisons never go
// through this Scan path.
func readRawAgentTimes(t *testing.T, s *AgentStore, id string) (created, updated, lastActivity string) {
	t.Helper()
	ctx := context.Background()
	drv := s.client.Driver()
	var rows entsql.Rows
	require.NoError(t, drv.Query(ctx, "SELECT created || '', updated || '', COALESCE(last_activity_event, '') || '' FROM agents WHERE id = ?", []any{id}, &rows))
	defer func() { _ = rows.Close() }()
	require.True(t, rows.Next(), "expected one row for id %s", id)
	require.NoError(t, rows.Scan(&created, &updated, &lastActivity))
	return created, updated, lastActivity
}

// utcTextHelperEnv selects the subprocess body in TestUTCTextHelperProcess
// over the normal (immediate pass, no-op) behavior that function has when
// run as part of the ordinary test suite.
//
// A real subprocess, not an in-process time.Local override, is required
// here: time.Local is one process-wide variable, so mutating it in a test,
// even with a t.Cleanup restore, is a data race against any other test in
// the same binary that reads it concurrently. Re-executing the test binary
// with TZ set gives the child its own time.Local, so the parent test
// binary's global is never touched.
const utcTextHelperEnv = "SCION_UTC_TEXT_HELPER"

// TestUTCTextHelperProcess is not itself a test of this package's behavior;
// it is the subprocess body runUTCTextHelper re-execs. Run as part of the
// normal suite (utcTextHelperEnv unset) it does nothing and reports a pass
// immediately; all assertions live in the parent test, against this
// function's stdout. The child never pins time.Local: it writes through the
// store exactly as an unpinned process with a non-UTC TZ would.
func TestUTCTextHelperProcess(t *testing.T) {
	if os.Getenv(utcTextHelperEnv) == "" {
		return
	}
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "utc-text-helper")
	if err := s.CreateAgent(ctx, a); err != nil {
		fmt.Fprintf(os.Stderr, "CreateAgent: %v\n", err)
		os.Exit(1)
	}
	// A bare time.Now() in the child's non-UTC zone, carrying a monotonic
	// reading, written through the ordinary update path.
	a.LastActivityEvent = time.Now()
	if err := s.UpdateAgent(ctx, a); err != nil {
		fmt.Fprintf(os.Stderr, "UpdateAgent: %v\n", err)
		os.Exit(1)
	}
	created, updated, lastActivity := readRawAgentTimes(t, s, a.ID)
	_, offset := time.Now().Zone()
	// Plain stdout lines, not t.Log: the parent process reads this over a
	// pipe, and testing.T's own output is not a stable contract to parse.
	fmt.Printf("OFFSET=%d\n", offset)
	fmt.Printf("CREATED=%s\n", created)
	fmt.Printf("UPDATED=%s\n", updated)
	fmt.Printf("LASTACTIVITY=%s\n", lastActivity)
}

// runUTCTextHelper re-execs the current test binary as a subprocess with TZ
// set to a non-UTC, non-whole-hour zone and returns the child's stdout as
// key/value pairs. -test.run anchors on the exact helper test name, so
// nothing else in this package executes in that subprocess.
func runUTCTextHelper(t *testing.T) map[string]string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestUTCTextHelperProcess$", "-test.v=false")
	cmd.Env = append(os.Environ(), utcTextHelperEnv+"=1", "TZ=Asia/Kathmandu")

	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("UTC text helper subprocess failed: %v\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
	}
	got := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && k != "" && strings.ToUpper(k) == k {
			got[k] = v
		}
	}
	for _, k := range []string{"OFFSET", "CREATED", "UPDATED", "LASTACTIVITY"} {
		require.Contains(t, got, k, "helper subprocess printed no %s= line; full stdout:\n%s", k, out)
	}
	return got
}

// TestAgentTimes_StoredAsUTCTextUnderNonUTCLocal proves the stored text the
// sorted list SQL in agent_list_order.go is written against: in a process
// whose time.Local is a non-UTC zone and that never pins time.Local,
// created, updated and last_activity_event are still stored as the verbose
// "+0000 UTC" form, because the SQLite store boundary converts every
// written time to UTC. It checks the raw stored bytes (readRawAgentTimes).
func TestAgentTimes_StoredAsUTCTextUnderNonUTCLocal(t *testing.T) {
	got := runUTCTextHelper(t)
	// Guard against a vacuous pass: the child really ran in a non-UTC zone.
	assert.Equal(t, "20700", got["OFFSET"], "the child must run with time.Local = Asia/Kathmandu (+05:45)")
	for _, k := range []string{"CREATED", "UPDATED", "LASTACTIVITY"} {
		// Anchored on both ends: this rejects the RFC3339 ...Z form, any
		// non-UTC offset, and a monotonic-clock suffix.
		assert.Regexp(t, canonicalUTCTextPattern, got[k], "%s must be stored as canonical UTC text", k)
	}
}

// canonicalUTCTextPattern is the exact stored text form of a time written
// through the store boundary: Go's default time.Time text of a UTC value,
// with no monotonic-clock suffix.
const canonicalUTCTextPattern = `^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(\.\d{1,9})? \+0000 UTC$`
