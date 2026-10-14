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

package runtimebroker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// journalAllowedKeys is the complete set of top-level keys a journal record
// may hold (design wave 3 §1 C-a: plain identifiers only). A new key must
// be added here deliberately.
var journalAllowedKeys = map[string]bool{
	"version": true, "launchId": true, "agentId": true, "kind": true, "runId": true,
	"instanceId": true, "hubName": true, "ownerHub": true, "deadline": true, "seq": true,
	"projectId": true, "slug": true, "agentName": true, "projectPath": true,
	"hubProjectId": true, "sharedWorkspace": true, "runtimeName": true, "profile": true,
	"ownedRun": true, "handles": true, "lastStep": true, "state": true, "terminal": true,
}

func newTestJournal(t *testing.T, instanceID string) (*launchJournal, string) {
	t.Helper()
	stateDir := t.TempDir()
	j := newLaunchJournal(stateDir, instanceID, nil)
	if j == nil {
		t.Fatal("newLaunchJournal returned nil for a usable state dir")
	}
	t.Cleanup(j.Close)
	return j, stateDir
}

func readJournalFileT(t *testing.T, path string) *journalRecord {
	t.Helper()
	jr, err := readJournalRecord(path)
	if err != nil {
		t.Fatalf("read journal record %s: %v", path, err)
	}
	return jr
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

// TestLaunchJournal_RoundTripAllowlistModes is C-T1: a record round-trips,
// holds only allowlisted keys, and the journal's directories are 0700 and
// its files 0600; concurrent writers leave one complete record and no temp
// files.
func TestLaunchJournal_RoundTripAllowlistModes(t *testing.T) {
	j, stateDir := newTestJournal(t, "inst-a")
	deadline := time.Now().Add(5 * time.Minute).UTC().Truncate(time.Second)
	rec := newLaunchRecord("L-rt", "agent-id-rt", store.LaunchKindCreate, "hub-a", deadline, func() {})
	rec.RunID = "run-rt"
	e := j.admit(rec, journalRecord{ProjectID: "proj-1", Slug: "agent-rt", AgentName: "agent-rt",
		ProjectPath: "/p", HubProjectID: "hp-1", SharedWorkspace: true, RuntimeName: "kubernetes", Profile: "prof"})
	if e == nil {
		t.Fatal("admit returned nil")
	}

	// Concurrent handle writers, each followed by a write.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec.AddHandle(api.ResourceHandle{Kind: "Pod", Namespace: "ns", Name: fmt.Sprintf("p%d", i), UID: fmt.Sprintf("u%d", i)})
			e.update()
		}(i)
	}
	wg.Wait()
	e.update()

	path := filepath.Join(stateDir, journalDirName, "inst-a", "L-rt.json")
	jr := readJournalFileT(t, path)
	if jr.LaunchID != "L-rt" || jr.AgentID != "agent-id-rt" || jr.Kind != store.LaunchKindCreate ||
		jr.RunID != "run-rt" || jr.InstanceID != "inst-a" || jr.HubName != "hub-a" ||
		!jr.Deadline.Equal(deadline) || jr.ProjectID != "proj-1" || jr.Slug != "agent-rt" ||
		jr.AgentName != "agent-rt" || jr.ProjectPath != "/p" || jr.HubProjectID != "hp-1" ||
		!jr.SharedWorkspace || jr.RuntimeName != "kubernetes" || jr.Profile != "prof" ||
		jr.State != "launching" {
		t.Fatalf("record did not round-trip: %+v", jr)
	}
	if len(jr.Handles) != 8 {
		t.Fatalf("expected all 8 concurrently added handles, got %d", len(jr.Handles))
	}

	assertMode(t, filepath.Join(stateDir, journalDirName), 0o700)
	assertMode(t, filepath.Join(stateDir, journalDirName, "inst-a"), 0o700)
	assertMode(t, path, 0o600)
	assertMode(t, filepath.Join(stateDir, journalDirName, "inst-a.lock"), 0o600)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	assertJournalKeysAllowed(t, raw)

	entries, err := os.ReadDir(filepath.Join(stateDir, journalDirName, "inst-a"))
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range entries {
		if strings.HasSuffix(de.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", de.Name())
		}
	}

	e.remove()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("record not removed: %v", err)
	}
}

func assertJournalKeysAllowed(t *testing.T, raw []byte) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("journal record is not a JSON object: %v", err)
	}
	var bad []string
	for k := range m {
		if !journalAllowedKeys[k] {
			bad = append(bad, k)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("journal record holds keys outside the allowlist: %v", bad)
	}
}

// TestWriteJournalFile_UniqueTempNames covers the writer's unique temp
// names: concurrent writes of one path never collide on a fixed ".tmp"
// name, all succeed, and leave no temp file.
func TestWriteJournalFile_UniqueTempNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "L.json")
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- writeJournalFile(path, []byte(fmt.Sprintf(`{"n":%d}`, i)))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent writeJournalFile failed: %v", err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "L.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected only L.json, got %v", names)
	}
	assertMode(t, path, 0o600)
}

// TestAsyncCreate_JournalHoldsNoSecrets is C-AC6 end to end: an async create
// whose request carries env values and secrets journals none of them, and
// its record holds only allowlisted keys.
func TestAsyncCreate_JournalHoldsNoSecrets(t *testing.T) {
	mgr := newAsyncManager()
	mgr.startBlock = make(chan struct{})
	srv, _ := newAsyncTestServer(t, mgr)
	t.Cleanup(func() {
		// Let the launch finish before the temp dirs go.
		close(mgr.startBlock)
		waitUntil(t, 5*time.Second, func() bool { return registryEmpty(srv) })
	})
	if srv.launchJournal == nil {
		t.Fatal("test server has no launch journal")
	}

	const secret = "s3cr3t-journal-canary"
	w := postCreate(t, srv, map[string]any{
		"name": "agent-sec", "asyncLaunch": true, "launchId": "L-sec",
		"launchTimeoutSeconds": 300,
		"config":               map[string]any{"template": "claude", "env": []string{"PLAIN_KEY=" + secret + "-env"}},
		"resolvedEnv":          map[string]string{"API_KEY": secret + "-resolved"},
		"resolvedSecrets":      []map[string]string{{"name": "tok", "type": "environment", "target": "TOK", "value": secret + "-secret"}},
	})
	if w.Code != 201 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	path := filepath.Join(srv.launchJournal.dir(), "L-sec.json")
	var raw []byte
	if !waitUntil(t, 2*time.Second, func() bool {
		b, err := os.ReadFile(path)
		raw = b
		return err == nil
	}) {
		t.Fatalf("no journal record at %s", path)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("journal record contains request secret material: %s", raw)
	}
	assertJournalKeysAllowed(t, raw)
	assertMode(t, path, 0o600)
}

// TestLaunchJournal_Ordering is C-T1b (design wave 3 §1 C-a, R4, Nit-3).
func TestLaunchJournal_Ordering(t *testing.T) {
	newEntry := func(t *testing.T, id string) (*launchJournal, *journalEntry, *launchRecord, string) {
		t.Helper()
		j, _ := newTestJournal(t, "inst-o")
		rec := newLaunchRecord(id, "agent-"+id, store.LaunchKindCreate, "", time.Now().Add(time.Hour), func() {})
		e := j.admit(rec, journalRecord{Slug: "s-" + id, AgentName: "s-" + id})
		if e == nil {
			t.Fatal("admit returned nil")
		}
		return j, e, rec, e.path
	}

	// (a) A launching write arriving after the terminal (a late coalesced
	// step write) never replaces terminal_pending on disk.
	t.Run("late launching write after terminal", func(t *testing.T) {
		_, e, rec, path := newEntry(t, "La")
		e.terminal(journalTerminal{ErrorCode: "runtime_error", Message: "m"})
		rec.AddHandle(api.ResourceHandle{Kind: "Pod", Name: "late", UID: "u-late"})
		e.update()
		jr := readJournalFileT(t, path)
		if jr.State != "terminal_pending" || jr.Terminal == nil || jr.Terminal.ErrorCode != "runtime_error" {
			t.Fatalf("terminal_pending was replaced by a later launching write: %+v", jr)
		}
	})

	// (b) A write released after removal does not recreate the file.
	t.Run("write after removal", func(t *testing.T) {
		_, e, _, path := newEntry(t, "Lb")
		e.remove()
		e.update()
		e.terminal(journalTerminal{Succeeded: true})
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("a write after removal recreated the record: %v", err)
		}
	})

	// (c) A write whose snapshot is held while a handle is added, and a
	// second write, leave every handle in the final launching file: each
	// snapshot is taken under the entry lock, after the previous write.
	t.Run("handles accumulate across interleaved writes", func(t *testing.T) {
		j, e, rec, path := newEntry(t, "Lc")
		held := make(chan struct{})
		release := make(chan struct{})
		var calls atomic.Int32
		// Only the first write is held after its snapshot; later ones pass.
		j.testHookAfterSnapshot = func(_ *journalEntry, state journalState) {
			if calls.Add(1) == 1 {
				close(held)
				<-release
			}
		}
		rec.AddHandle(api.ResourceHandle{Kind: "Pod", Name: "h1", UID: "u1"})
		first := make(chan struct{})
		go func() { e.update(); close(first) }()
		<-held
		rec.AddHandle(api.ResourceHandle{Kind: "Secret", Name: "h2", UID: "u2"})
		second := make(chan struct{})
		go func() { e.update(); close(second) }()
		// The second write must wait for the held one (it needs the entry
		// lock); give a writer that does not wait time to finish first.
		select {
		case <-second:
		case <-time.After(200 * time.Millisecond):
		}
		close(release)
		<-first
		<-second
		jr := readJournalFileT(t, path)
		if len(jr.Handles) != 2 {
			t.Fatalf("expected both handles in the final record, got %+v", jr.Handles)
		}
	})

	// (d) A second terminal after a pending succeeded is dropped; removal
	// still proceeds.
	t.Run("second terminal dropped", func(t *testing.T) {
		_, e, _, path := newEntry(t, "Ld")
		e.terminal(journalTerminal{Succeeded: true})
		e.terminal(journalTerminal{ErrorCode: "broker_restarted"})
		jr := readJournalFileT(t, path)
		if jr.Terminal == nil || !jr.Terminal.Succeeded || jr.Terminal.ErrorCode != "" {
			t.Fatalf("a second terminal replaced the pending succeeded: %+v", jr.Terminal)
		}
		e.remove()
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("removal after a terminal did not remove the record: %v", err)
		}
	})
}

// TestLaunchJournal_LockContention is C-T7 (C-AC8): while instance A holds
// its lock, B's recovery never touches A's records; once A is gone, B
// recovers them.
func TestLaunchJournal_LockContention(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	stateDir := srv.stateDir

	a := newLaunchJournal(stateDir, "inst-live", nil)
	rec := newLaunchRecord("L-live", "agent-live", store.LaunchKindCreate, "", time.Now().Add(time.Hour), func() {})
	if a.admit(rec, journalRecord{Slug: "agent-live", AgentName: "agent-live"}) == nil {
		t.Fatal("admit returned nil")
	}
	path := filepath.Join(stateDir, journalDirName, "inst-live", "L-live.json")

	srv.recoverLaunches()
	srv.launchDrain.recoveryWG.Wait()
	if n := len(rtb.getLaunchReports()); n != 0 {
		t.Fatalf("recovery reported %d launches of a live instance", n)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("recovery touched a live instance's record: %v", err)
	}

	a.Close() // instance A is gone
	rtb.launchReportFunc = appliedReports
	srv.recoverLaunches()
	srv.launchDrain.recoveryWG.Wait()
	if n := len(rtb.getLaunchReports()); n != 1 {
		t.Fatalf("expected the dead instance's launch to be reported once, got %d", n)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("recovered record not removed: %v", err)
	}
}
