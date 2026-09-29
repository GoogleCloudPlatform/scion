/*
Copyright 2025 The Scion Authors.
*/

package handlers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitLimitsFile(t *testing.T) {
	scrubHubEnv(t)
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")

	err := InitLimitsFile(limitsPath, 50, 200, 0, 0)
	require.NoError(t, err)

	data, err := os.ReadFile(limitsPath)
	require.NoError(t, err)

	var ls LimitsState
	err = json.Unmarshal(data, &ls)
	require.NoError(t, err)

	assert.Equal(t, 0, ls.TurnCount)
	assert.Equal(t, 0, ls.ModelCallCount)
	assert.Equal(t, 50, ls.MaxTurns)
	assert.Equal(t, 200, ls.MaxModelCalls)
	assert.NotEmpty(t, ls.StartedAt)
}

func TestInitLimitsFile_ZeroValues(t *testing.T) {
	scrubHubEnv(t)
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")

	err := InitLimitsFile(limitsPath, 0, 0, 0, 0)
	require.NoError(t, err)

	data, err := os.ReadFile(limitsPath)
	require.NoError(t, err)

	var ls LimitsState
	err = json.Unmarshal(data, &ls)
	require.NoError(t, err)

	assert.Equal(t, 0, ls.MaxTurns)
	assert.Equal(t, 0, ls.MaxModelCalls)
}

func TestLimitsHandler_TurnCounting(t *testing.T) {
	scrubHubEnv(t)
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")

	// Initialize the limits file
	err := InitLimitsFile(limitsPath, 5, 0, 0, 0)
	require.NoError(t, err)

	h := &LimitsHandler{
		maxTurns:      5,
		maxModelCalls: 0,
		limitsPath:    limitsPath,
		statusHandler: &StatusHandler{StatusPath: filepath.Join(tmpDir, "agent-info.json")},
	}

	// Simulate 3 turns
	for i := 0; i < 3; i++ {
		err := h.Handle(&hooks.Event{Name: hooks.EventAgentEnd})
		require.NoError(t, err)
	}

	// Check the count
	ls := readLimitsFile(t, limitsPath)
	assert.Equal(t, 3, ls.TurnCount)
	assert.Equal(t, 0, ls.ModelCallCount)
}

func TestLimitsHandler_ModelCallCounting(t *testing.T) {
	scrubHubEnv(t)
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")

	err := InitLimitsFile(limitsPath, 0, 10, 0, 0)
	require.NoError(t, err)

	h := &LimitsHandler{
		maxTurns:      0,
		maxModelCalls: 10,
		limitsPath:    limitsPath,
		statusHandler: &StatusHandler{StatusPath: filepath.Join(tmpDir, "agent-info.json")},
	}

	// Simulate 5 model calls
	for i := 0; i < 5; i++ {
		err := h.Handle(&hooks.Event{Name: hooks.EventModelEnd})
		require.NoError(t, err)
	}

	ls := readLimitsFile(t, limitsPath)
	assert.Equal(t, 0, ls.TurnCount)
	assert.Equal(t, 5, ls.ModelCallCount)
}

func TestLimitsHandler_IgnoresIrrelevantEvents(t *testing.T) {
	scrubHubEnv(t)
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")

	err := InitLimitsFile(limitsPath, 10, 10, 0, 0)
	require.NoError(t, err)

	h := &LimitsHandler{
		maxTurns:      10,
		maxModelCalls: 10,
		limitsPath:    limitsPath,
		statusHandler: &StatusHandler{StatusPath: filepath.Join(tmpDir, "agent-info.json")},
	}

	// These events should not change any counters
	events := []string{
		hooks.EventToolStart,
		hooks.EventToolEnd,
		hooks.EventSessionStart,
		hooks.EventPromptSubmit,
		hooks.EventModelStart,
	}

	for _, eventName := range events {
		err := h.Handle(&hooks.Event{Name: eventName})
		require.NoError(t, err)
	}

	ls := readLimitsFile(t, limitsPath)
	assert.Equal(t, 0, ls.TurnCount)
	assert.Equal(t, 0, ls.ModelCallCount)
}

func TestLimitsHandler_NilHandler(t *testing.T) {
	scrubHubEnv(t)
	var h *LimitsHandler
	err := h.Handle(&hooks.Event{Name: hooks.EventAgentEnd})
	assert.NoError(t, err)
}

func TestLimitsHandler_NoLimitsConfigured(t *testing.T) {
	scrubHubEnv(t)
	// When maxTurns=0 and maxModelCalls=0, events are silently ignored
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")

	err := InitLimitsFile(limitsPath, 0, 0, 0, 0)
	require.NoError(t, err)

	h := &LimitsHandler{
		maxTurns:      0,
		maxModelCalls: 0,
		limitsPath:    limitsPath,
		statusHandler: &StatusHandler{StatusPath: filepath.Join(tmpDir, "agent-info.json")},
	}

	// agent-end with no max_turns should be ignored
	err = h.Handle(&hooks.Event{Name: hooks.EventAgentEnd})
	require.NoError(t, err)

	// model-end with no max_model_calls should be ignored
	err = h.Handle(&hooks.Event{Name: hooks.EventModelEnd})
	require.NoError(t, err)

	ls := readLimitsFile(t, limitsPath)
	assert.Equal(t, 0, ls.TurnCount)
	assert.Equal(t, 0, ls.ModelCallCount)
}

func TestLimitsHandler_TurnLimitDetection(t *testing.T) {
	scrubHubEnv(t)
	// Test that the handler detects when the turn limit is reached.
	// We can't test the SIGUSR1 signal in unit tests (it would kill the test process),
	// so we verify the status file is updated and the limits file has the right count.
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	triggerPath := filepath.Join(tmpDir, "scion-limits-exceeded")

	err := InitLimitsFile(limitsPath, 3, 0, 0, 0)
	require.NoError(t, err)

	h := &LimitsHandler{
		maxTurns:        3,
		maxModelCalls:   0,
		limitsPath:      limitsPath,
		triggerFilePath: triggerPath,
		statusHandler:   &StatusHandler{StatusPath: statusPath},
	}

	// Simulate 2 turns (under limit)
	for i := 0; i < 2; i++ {
		err := h.Handle(&hooks.Event{Name: hooks.EventAgentEnd})
		require.NoError(t, err)
	}

	ls := readLimitsFile(t, limitsPath)
	assert.Equal(t, 2, ls.TurnCount)

	// The 3rd turn hits the limit - this will attempt SIGUSR1 to PID 1
	// which won't work in tests (PID 1 is the test runner's init), but
	// the status update and file write should succeed.
	_ = h.Handle(&hooks.Event{Name: hooks.EventAgentEnd})

	ls = readLimitsFile(t, limitsPath)
	assert.Equal(t, 3, ls.TurnCount)

	// Verify the agent-info.json was updated to limits_exceeded
	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "limits_exceeded", info.Activity)
}

func TestLimitsHandler_ModelCallLimitDetection(t *testing.T) {
	scrubHubEnv(t)
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	triggerPath := filepath.Join(tmpDir, "scion-limits-exceeded")

	err := InitLimitsFile(limitsPath, 0, 2, 0, 0)
	require.NoError(t, err)

	h := &LimitsHandler{
		maxTurns:        0,
		maxModelCalls:   2,
		limitsPath:      limitsPath,
		triggerFilePath: triggerPath,
		statusHandler:   &StatusHandler{StatusPath: statusPath},
	}

	// First model call - under limit
	err = h.Handle(&hooks.Event{Name: hooks.EventModelEnd})
	require.NoError(t, err)

	ls := readLimitsFile(t, limitsPath)
	assert.Equal(t, 1, ls.ModelCallCount)

	// Second model call hits the limit
	_ = h.Handle(&hooks.Event{Name: hooks.EventModelEnd})

	ls = readLimitsFile(t, limitsPath)
	assert.Equal(t, 2, ls.ModelCallCount)

	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "limits_exceeded", info.Activity)
}

func TestLimitsHandler_BothLimitsIndependent(t *testing.T) {
	scrubHubEnv(t)
	// Verify both counters are tracked independently
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")

	err := InitLimitsFile(limitsPath, 100, 100, 0, 0)
	require.NoError(t, err)

	h := &LimitsHandler{
		maxTurns:      100,
		maxModelCalls: 100,
		limitsPath:    limitsPath,
		statusHandler: &StatusHandler{StatusPath: filepath.Join(tmpDir, "agent-info.json")},
	}

	// Simulate 5 model calls
	for i := 0; i < 5; i++ {
		err := h.Handle(&hooks.Event{Name: hooks.EventModelEnd})
		require.NoError(t, err)
	}

	// Simulate 2 turns
	for i := 0; i < 2; i++ {
		err := h.Handle(&hooks.Event{Name: hooks.EventAgentEnd})
		require.NoError(t, err)
	}

	ls := readLimitsFile(t, limitsPath)
	assert.Equal(t, 2, ls.TurnCount)
	assert.Equal(t, 5, ls.ModelCallCount)
}

func TestLimitsHandler_MissingLimitsFile(t *testing.T) {
	scrubHubEnv(t)
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "nonexistent", "agent-limits.json")

	h := &LimitsHandler{
		maxTurns:      10,
		maxModelCalls: 0,
		limitsPath:    limitsPath,
		statusHandler: &StatusHandler{StatusPath: filepath.Join(tmpDir, "agent-info.json")},
	}

	// Should not error - just logs and returns nil
	err := h.Handle(&hooks.Event{Name: hooks.EventAgentEnd})
	assert.NoError(t, err)
}

func TestNewLimitsHandler_NoLimits(t *testing.T) {
	// Ensure NewLimitsHandlerWithPath returns nil when no limits are configured
	h := NewLimitsHandlerWithPath(0, 0, "/tmp/test-limits.json")
	assert.Nil(t, h)
}

func TestNewLimitsHandler_WithTurnsOnly(t *testing.T) {
	h := NewLimitsHandlerWithPath(10, 0, "/tmp/test-limits.json")
	assert.NotNil(t, h)
	assert.Equal(t, 10, h.maxTurns)
	assert.Equal(t, 0, h.maxModelCalls)
}

func TestNewLimitsHandler_WithModelCallsOnly(t *testing.T) {
	h := NewLimitsHandlerWithPath(0, 50, "/tmp/test-limits.json")
	assert.NotNil(t, h)
	assert.Equal(t, 0, h.maxTurns)
	assert.Equal(t, 50, h.maxModelCalls)
}

func TestParseEnvInt(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  string
		want int
	}{
		{"valid integer", "TEST_INT_1", "42", 42},
		{"zero", "TEST_INT_2", "0", 0},
		{"empty string", "TEST_INT_3", "", 0},
		{"invalid string", "TEST_INT_4", "abc", 0},
		{"negative", "TEST_INT_5", "-1", -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.val != "" {
				t.Setenv(tt.key, tt.val)
			}
			got := ParseEnvInt(tt.key)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestExitCodeLimitsExceeded(t *testing.T) {
	assert.Equal(t, 10, ExitCodeLimitsExceeded)
}

func TestWriteLimitsState_AtomicWrite(t *testing.T) {
	scrubHubEnv(t)
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")

	ls := &LimitsState{
		TurnCount:      5,
		ModelCallCount: 12,
		MaxTurns:       50,
		MaxModelCalls:  200,
		StartedAt:      "2026-02-22T10:30:00Z",
	}

	err := writeLimitsState(limitsPath, ls, 0, 0)
	require.NoError(t, err)

	// Read and verify
	data, err := os.ReadFile(limitsPath)
	require.NoError(t, err)

	var read LimitsState
	err = json.Unmarshal(data, &read)
	require.NoError(t, err)

	assert.Equal(t, 5, read.TurnCount)
	assert.Equal(t, 12, read.ModelCallCount)
	assert.Equal(t, 50, read.MaxTurns)
	assert.Equal(t, 200, read.MaxModelCalls)
	assert.Equal(t, "2026-02-22T10:30:00Z", read.StartedAt)
}

// TestWriteLimitsState_ChownsTempFdBeforeRename proves the uid>0 path
// chowns the temp file via its open fd before the rename rather than the
// final path afterwards. A non-root test can't chown to an arbitrary uid,
// but chowning to its own current uid/gid is always permitted, which is
// enough to prove the fd-based call succeeds and doesn't error.
func TestWriteLimitsState_ChownsTempFdBeforeRename(t *testing.T) {
	scrubHubEnv(t)
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")

	ls := &LimitsState{MaxTurns: 1, StartedAt: "2026-02-22T10:30:00Z"}
	err := writeLimitsState(limitsPath, ls, os.Getuid(), os.Getgid())
	require.NoError(t, err)

	entries, err := os.ReadDir(tmpDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temp file should remain after a successful write")
	assert.Equal(t, "agent-limits.json", entries[0].Name())
}

func TestLimitsTriggerFileConstant(t *testing.T) {
	assert.Equal(t, "/tmp/scion-limits-exceeded", LimitsTriggerFile)
}

func TestSignalLimitsExceeded_CreatesTriggerFile(t *testing.T) {
	scrubHubEnv(t)
	tmpDir := t.TempDir()
	triggerPath := filepath.Join(tmpDir, "scion-limits-exceeded")

	h := &LimitsHandler{
		triggerFilePath: triggerPath,
	}

	err := h.signalLimitsExceeded()
	assert.NoError(t, err)

	// Verify the trigger file was created
	_, err = os.Stat(triggerPath)
	assert.NoError(t, err, "trigger file should exist after signalLimitsExceeded")
}

// TestLimitsHandler_ReadLimitsStateNormalRead proves the hardened read
// still returns ordinary limits state for a plain, legitimate file.
func TestLimitsHandler_ReadLimitsStateNormalRead(t *testing.T) {
	scrubHubEnv(t)
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")
	require.NoError(t, InitLimitsFile(limitsPath, 7, 9, 0, 0))

	h := &LimitsHandler{limitsPath: limitsPath}
	ls, err := h.readLimitsState()
	require.NoError(t, err)
	assert.Equal(t, 7, ls.MaxTurns)
	assert.Equal(t, 9, ls.MaxModelCalls)
}

// TestLimitsHandler_SymlinkAtLimitsPathIsRefused proves a symlink swapped
// in at agent-limits.json (the workload owns the containing directory and
// can always do this) is refused rather than followed. This fails if
// readLimitsState is ever reverted to a plain os.ReadFile, which follows
// symlinks unconditionally.
func TestLimitsHandler_SymlinkAtLimitsPathIsRefused(t *testing.T) {
	scrubHubEnv(t)
	tmpDir := t.TempDir()
	secret := filepath.Join(tmpDir, "root-only-secret.json")
	require.NoError(t, os.WriteFile(secret, []byte(`{"turn_count":999}`), 0o600))

	limitsPath := filepath.Join(tmpDir, "agent-limits.json")
	require.NoError(t, os.Symlink(secret, limitsPath))

	h := &LimitsHandler{limitsPath: limitsPath}
	_, err := h.readLimitsState()
	assert.Error(t, err, "expected a symlink at limitsPath to be refused, not followed")
}

// wantAgentLimitsMaxBytes is this test's OWN, independently hardcoded copy
// of the size bound limits.go documents for agentLimitsMaxBytes (1 MiB) —
// not a reference to that constant. See status_test.go's
// wantAgentInfoMaxBytes for why a test that instead sized its fixture as
// agentLimitsMaxBytes+1 could never notice a regression that widens that
// constant: the fixture and the cap would drift together.
const wantAgentLimitsMaxBytes = 1 << 20

// limitsStateJSONOfSize returns a syntactically valid, LimitsState-shaped
// JSON document of exactly n bytes, by padding the started_at field. n must
// be at least the length of the zero-padding LimitsState marshals to.
func limitsStateJSONOfSize(t *testing.T, n int) []byte {
	t.Helper()
	base, err := json.Marshal(LimitsState{StartedAt: ""})
	if err != nil {
		t.Fatalf("marshal base LimitsState: %v", err)
	}
	pad := n - len(base)
	if pad < 0 {
		t.Fatalf("limitsStateJSONOfSize: n=%d is smaller than the unpadded encoding (%d bytes)", n, len(base))
	}
	// 'a' padding, not NUL bytes: json.Marshal escapes a control character
	// like NUL as \u0000 (6 bytes of output per 1 byte of input), which would
	// make the padding math below wrong. 'a' round-trips as a single byte.
	padded, err := json.Marshal(LimitsState{StartedAt: strings.Repeat("a", pad)})
	if err != nil {
		t.Fatalf("marshal padded LimitsState: %v", err)
	}
	if len(padded) != n {
		t.Fatalf("limitsStateJSONOfSize(%d) produced %d bytes", n, len(padded))
	}
	return padded
}

// TestLimitsHandler_OversizeRegularLimitsPathIsRefused proves the size bound
// on readLimitsState is enforced against an actual regular file, not just a
// symlink or a fixture that would fail to parse regardless of the cap: the
// fixture here is syntactically valid, LimitsState-shaped JSON one byte over
// wantAgentLimitsMaxBytes, which would unmarshal successfully if the cap did not
// refuse it first. This fails if the cap is removed or widened.
func TestLimitsHandler_OversizeRegularLimitsPathIsRefused(t *testing.T) {
	scrubHubEnv(t)
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")
	require.NoError(t, os.WriteFile(limitsPath, limitsStateJSONOfSize(t, wantAgentLimitsMaxBytes+1), 0o600))

	h := &LimitsHandler{limitsPath: limitsPath}
	_, err := h.readLimitsState()
	assert.Error(t, err, "expected an oversize regular agent-limits.json to be refused")
}

// TestLimitsHandler_AtCapRegularLimitsPathIsRead is
// OversizeRegularLimitsPathIsRefused's companion: the same valid-JSON shape
// at exactly wantAgentLimitsMaxBytes (not one byte over) is read and parsed
// successfully, pinning the boundary at the documented cap.
func TestLimitsHandler_AtCapRegularLimitsPathIsRead(t *testing.T) {
	scrubHubEnv(t)
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")
	require.NoError(t, os.WriteFile(limitsPath, limitsStateJSONOfSize(t, wantAgentLimitsMaxBytes), 0o600))

	h := &LimitsHandler{limitsPath: limitsPath}
	ls, err := h.readLimitsState()
	require.NoError(t, err)
	assert.NotNil(t, ls)
}

// readLimitsFile reads and parses an agent-limits.json file for test assertions.
func readLimitsFile(t *testing.T, path string) LimitsState {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var ls LimitsState
	err = json.Unmarshal(data, &ls)
	require.NoError(t, err)
	return ls
}
