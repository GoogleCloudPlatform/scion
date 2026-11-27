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

//go:build unix

package telemetry

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func TestDetectAgyVersionManaged_Sec_HardenedEnv(t *testing.T) {
	t.Setenv("PATH", "/tmp/agent-writable:/usr/bin:/bin")
	t.Setenv("HOME", "/home/untrusted-agent")
	t.Setenv("LD_PRELOAD", "/tmp/evil.so")
	t.Setenv("NODE_OPTIONS", "--require=/tmp/evil.js")

	var captured *exec.Cmd
	prev := agyVersionOutput
	agyVersionOutput = func(cmd *exec.Cmd) ([]byte, error) {
		captured = cmd
		return []byte("1.5.18\n"), nil
	}
	t.Cleanup(func() { agyVersionOutput = prev })

	// Resolve a standard trusted binary ("sh") so the real rootexec.Resolve +
	// exec.CommandContext + rootexec.Env path executes end-to-end.
	ver, err := detectAgyVersionBinary(context.Background(), "sh")
	if err != nil {
		t.Fatalf("detectAgyVersionBinary(sh): %v", err)
	}
	if ver != "1.5.18" {
		t.Fatalf("ver = %q, want 1.5.18", ver)
	}
	if captured == nil {
		t.Fatal("expected agyVersionOutput to be invoked")
	}
	wantEnv := rootexec.Env("HOME=/nonexistent")
	if len(captured.Env) != len(wantEnv) {
		t.Fatalf("cmd.Env = %v, want %v", captured.Env, wantEnv)
	}
	for i := range wantEnv {
		if captured.Env[i] != wantEnv[i] {
			t.Fatalf("cmd.Env[%d] = %q, want %q", i, captured.Env[i], wantEnv[i])
		}
	}
	for _, entry := range captured.Env {
		if strings.HasPrefix(entry, "LD_PRELOAD=") || strings.HasPrefix(entry, "NODE_OPTIONS=") {
			t.Fatalf("ambient hazardous env leaked into cmd.Env: %q", entry)
		}
	}
}

func TestAntigravitySQLiteTailer_Sec_SkipsFIFOAndSymlinkWithoutHanging(t *testing.T) {
	setTestAgyEnv(t)
	home := t.TempDir()
	convDir := AgyConversationsDir(home)
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatalf("mkdir convDir: %v", err)
	}

	// 1. FIFO named a-fifo.db with no writer — opening with SQLite would hang forever.
	fifoPath := filepath.Join(convDir, "a-fifo.db")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	// 2. Symlink named b-link.db pointing to a real DB outside conversations/.
	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "secret.db")
	outsideDB := createTestAgyDB(t, outsidePath)
	defer func() { _ = outsideDB.Close() }()
	secretRow := encodeTestGenMeta(1466, "gemini-4-neon", 1795720000, testAgyUsage{
		modelID: 1466, input: 1234, output: 56, id11: "secret-link-call",
	})
	if _, err := outsideDB.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, secretRow, len(secretRow)); err != nil {
		t.Fatalf("insert secretRow: %v", err)
	}
	linkPath := filepath.Join(convDir, "b-link.db")
	if err := os.Symlink(outsidePath, linkPath); err != nil {
		t.Fatalf("symlink b-link.db: %v", err)
	}

	// 3. Regular conversation DB c-good.db with 1 historical row.
	goodPath := filepath.Join(convDir, "c-good.db")
	goodDB := createTestAgyDB(t, goodPath)
	defer func() { _ = goodDB.Close() }()
	histRow := encodeTestGenMeta(1466, "gemini-4-neon", 1795700000, testAgyUsage{
		modelID: 1466, input: 9000, output: 900, id11: "hist-good-call",
	})
	if _, err := goodDB.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, histRow, len(histRow)); err != nil {
		t.Fatalf("insert histRow: %v", err)
	}

	outbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
	if err != nil {
		t.Fatalf("open outbox: %v", err)
	}
	tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
		AgentHome:         home,
		DisableWakeSocket: true,
		Analytics:         NewUsageAnalytics(outbox, telemetrycontract.UsageSourceSQLiteNative),
		DetectVersion:     func(context.Context) (string, error) { return "1.5.18", nil },
	})

	runBounded := func(label string, fn func() error) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- fn() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s failed: %v", label, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s hung (likely blocked opening FIFO)", label)
		}
	}

	// Start with no state file: must not hang on FIFO, must skip symlink, and
	// must baseline c-good.db cleanly.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runBounded("Start", func() error { return tailer.Start(ctx) })
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer stopCancel()
		_ = tailer.Stop(stopCtx)
	}()

	runBounded("PollOnce(baseline)", func() error { return tailer.PollOnce(ctx) })
	if got := drainOutboxEvents(t, outbox); len(got) != 0 {
		t.Fatalf("after baseline got %d events, want 0", len(got))
	}

	// Insert new rows into BOTH the symlink target and c-good.db:
	// only c-good.db's new row must be emitted; symlinked DB must remain skipped.
	newSecret := encodeTestGenMeta(1466, "gemini-4-neon", 1795720100, testAgyUsage{
		modelID: 1466, input: 7777, output: 77, id11: "secret-link-call-2",
	})
	if _, err := outsideDB.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (1, ?, ?)`, newSecret, len(newSecret)); err != nil {
		t.Fatalf("insert newSecret: %v", err)
	}
	newGood := encodeTestGenMeta(1466, "gemini-4-neon", 1795720200, testAgyUsage{
		modelID: 1466, input: 345, output: 67, id11: "good-call-1",
	})
	if _, err := goodDB.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (1, ?, ?)`, newGood, len(newGood)); err != nil {
		t.Fatalf("insert newGood: %v", err)
	}

	runBounded("PollOnce(new row)", func() error { return tailer.PollOnce(ctx) })
	events := drainOutboxEvents(t, outbox)
	if len(events) != 1 || events[0].Input == nil || *events[0].Input != 345 {
		t.Fatalf("got events %+v, want 1 event from c-good.db with input=345", events)
	}
}

func TestStartAgySQLiteWakeListener_Sec_StageSwapAfterBindDoesNotTouchTarget(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("/proc/self/fd unavailable on this host")
	}
	// Use a short temp dir so sockPath stays under Linux's 108-byte sun_path limit.
	home, err := os.MkdirTemp("", "agyw-")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	scionDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(scionDir, 0o700); err != nil {
		t.Fatalf("mkdir .scion: %v", err)
	}

	// Attacker target directory containing a file named "s" with mode 0644.
	targetDir := t.TempDir()
	victimPath := filepath.Join(targetDir, "s")
	if err := os.WriteFile(victimPath, []byte("victim-untouched"), 0o644); err != nil {
		t.Fatalf("write victim s: %v", err)
	}
	victimBefore, err := os.Lstat(victimPath)
	if err != nil {
		t.Fatalf("lstat victim before: %v", err)
	}
	beforeStat := victimBefore.Sys().(*syscall.Stat_t)

	// Immediately after ListenUnixgram binds inside stageFd, swap the stage
	// directory name in .scion to a symlink pointing at targetDir.
	prevHook := agyWakeAfterBindHook
	swapped := false
	movedAside := ""
	agyWakeAfterBindHook = func(parentDir, stageName string) {
		orig := filepath.Join(parentDir, stageName)
		movedAside = filepath.Join(parentDir, stageName+".moved")
		if err := os.Rename(orig, movedAside); err != nil {
			t.Errorf("hook rename stage aside: %v", err)
			return
		}
		if err := os.Symlink(targetDir, orig); err != nil {
			t.Errorf("hook symlink stage -> targetDir: %v", err)
			return
		}
		swapped = true
	}
	t.Cleanup(func() {
		agyWakeAfterBindHook = prevHook
		if movedAside != "" {
			_ = os.RemoveAll(movedAside)
		}
	})

	woke := make(chan struct{}, 1)
	sockPath := AgySQLiteWakePath(home)
	closer, err := startAgySQLiteWakeListener(sockPath, nil, func() {
		select {
		case woke <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatalf("startAgySQLiteWakeListener: %v", err)
	}
	defer func() { _ = closer.Close() }()

	if !swapped {
		t.Fatal("expected agyWakeAfterBindHook to execute")
	}

	// Verify attacker's targetDir/s was NOT moved, NOT chmodded, and kept its owner/inode.
	victimAfter, err := os.Lstat(victimPath)
	if err != nil {
		t.Fatalf("victim targetDir/s disappeared or moved: %v", err)
	}
	if victimAfter.Mode().Perm() != 0o644 {
		t.Fatalf("victim mode = %#o, want 0644 (must not be chmodded)", victimAfter.Mode().Perm())
	}
	afterStat := victimAfter.Sys().(*syscall.Stat_t)
	if afterStat.Ino != beforeStat.Ino || afterStat.Uid != beforeStat.Uid || afterStat.Gid != beforeStat.Gid {
		t.Fatalf("victim inode/owner changed: before=(ino=%d uid=%d gid=%d) after=(ino=%d uid=%d gid=%d)",
			beforeStat.Ino, beforeStat.Uid, beforeStat.Gid, afterStat.Ino, afterStat.Uid, afterStat.Gid)
	}

	// Verify the real wake socket was placed at sockPath and delivers wakes.
	NotifyAgySQLiteWakePath(sockPath)
	select {
	case <-woke:
	case <-time.After(2 * time.Second):
		t.Fatal("expected wake notification on staged socket")
	}

	// Close must unlink sockPath via pinned scionFd.
	if err := closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Lstat(sockPath); !os.IsNotExist(err) {
		t.Fatalf("expected wake socket to be unlinked on Close, got err=%v", err)
	}
}

func TestAntigravitySQLiteTailer_Sec_StateParentSymlinkRefused(t *testing.T) {
	setTestAgyEnv(t)
	home := t.TempDir()
	targetDir := t.TempDir()
	// Replace $HOME/.scion with a symlink to targetDir.
	if err := os.Symlink(targetDir, filepath.Join(home, ".scion")); err != nil {
		t.Fatalf("symlink .scion: %v", err)
	}

	dbPath := filepath.Join(AgyConversationsDir(home), "conv-state.db")
	db := createTestAgyDB(t, dbPath)
	defer func() { _ = db.Close() }()

	outbox, err := OpenAnalyticsOutbox(filepath.Join(t.TempDir(), "hub-outbox"))
	if err != nil {
		t.Fatalf("open outbox: %v", err)
	}
	tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
		AgentHome:         home,
		DisableWakeSocket: true,
		Analytics:         NewUsageAnalytics(outbox, telemetrycontract.UsageSourceSQLiteNative),
		DetectVersion:     func(context.Context) (string, error) { return "1.5.18", nil },
	})

	if err := tailer.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	// Verify targetDir received NO state file and NO temp files.
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		t.Fatalf("readdir targetDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 files written into symlinked .scion target, got %d (%v)", len(entries), entries)
	}
}

func TestAntigravitySQLiteTailer_Sec_SkipsFIFOJournalAndSHMWithoutHanging(t *testing.T) {
	setTestAgyEnv(t)
	home := t.TempDir()
	convDir := AgyConversationsDir(home)
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatalf("mkdir convDir: %v", err)
	}

	// 1. Rollback-mode DB with a FIFO <x>.db-journal companion (no writer).
	journalDBPath := filepath.Join(convDir, "a-rollback.db")
	journalDB := createTestAgyDB(t, journalDBPath)
	if _, err := journalDB.Exec(`PRAGMA journal_mode=DELETE`); err != nil {
		t.Fatalf("set rollback journal_mode: %v", err)
	}
	_ = journalDB.Close()
	if err := syscall.Mkfifo(journalDBPath+"-journal", 0o600); err != nil {
		t.Fatalf("mkfifo -journal: %v", err)
	}

	// 2. DB with a FIFO <y>.db-shm companion (no writer).
	shmDBPath := filepath.Join(convDir, "b-shm.db")
	shmDB := createTestAgyDB(t, shmDBPath)
	_ = shmDB.Close()
	if err := syscall.Mkfifo(shmDBPath+"-shm", 0o600); err != nil {
		t.Fatalf("mkfifo -shm: %v", err)
	}

	// 3. Clean regular DB c-good.db with 1 historical row.
	goodPath := filepath.Join(convDir, "c-good.db")
	goodDB := createTestAgyDB(t, goodPath)
	defer func() { _ = goodDB.Close() }()
	histRow := encodeTestGenMeta(1466, "gemini-4-neon", 1795700000, testAgyUsage{
		modelID: 1466, input: 5000, output: 500, id11: "hist-journal-good",
	})
	if _, err := goodDB.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, histRow, len(histRow)); err != nil {
		t.Fatalf("insert histRow: %v", err)
	}

	outbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
	if err != nil {
		t.Fatalf("open outbox: %v", err)
	}
	tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
		AgentHome:         home,
		DisableWakeSocket: true,
		Analytics:         NewUsageAnalytics(outbox, telemetrycontract.UsageSourceSQLiteNative),
		DetectVersion:     func(context.Context) (string, error) { return "1.5.18", nil },
	})

	runBounded := func(label string, fn func() error) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- fn() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s failed: %v", label, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s hung on FIFO -journal/-shm companion", label)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runBounded("Start", func() error { return tailer.Start(ctx) })
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer stopCancel()
		_ = tailer.Stop(stopCtx)
	}()

	runBounded("PollOnce(baseline)", func() error { return tailer.PollOnce(ctx) })
	if got := drainOutboxEvents(t, outbox); len(got) != 0 {
		t.Fatalf("after baseline got %d events, want 0", len(got))
	}

	// Insert a new row into c-good.db and confirm it is still metered cleanly.
	newGood := encodeTestGenMeta(1466, "gemini-4-neon", 1795721000, testAgyUsage{
		modelID: 1466, input: 612, output: 84, id11: "good-after-fifo-journal",
	})
	if _, err := goodDB.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (1, ?, ?)`, newGood, len(newGood)); err != nil {
		t.Fatalf("insert newGood: %v", err)
	}
	runBounded("PollOnce(new row)", func() error { return tailer.PollOnce(ctx) })
	events := drainOutboxEvents(t, outbox)
	if len(events) != 1 || events[0].Input == nil || *events[0].Input != 612 {
		t.Fatalf("got events %+v, want 1 event from c-good.db with input=612", events)
	}
}
