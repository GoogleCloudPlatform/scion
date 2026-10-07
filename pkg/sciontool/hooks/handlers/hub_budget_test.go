/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blackholeHubURL returns the URL of a listener that accepts connections and
// reads requests but never answers, like a Hub whose responses are dropped.
// Only the caller's context can end a request to it.
func blackholeHubURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			go func() { _, _ = io.Copy(io.Discard, c) }()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return "http://" + l.Addr().String()
}

func setTestHubEnv(t *testing.T, url string) {
	t.Helper()
	scrubHubEnv(t)
	t.Setenv("SCION_HUB_ENDPOINT", url)
	t.Setenv("SCION_AUTH_TOKEN", "test-token")
	t.Setenv("SCION_AGENT_ID", "test-agent-id")
}

// Every call on a budgeted HubHandler shares the budget's deadline, so
// several calls against an unresponsive Hub end when the budget does instead
// of each taking the full per-call cap (5s each before the fix).
func TestHubHandler_BudgetBoundsAllCalls(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setTestHubEnv(t, blackholeHubURL(t))

	budget, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	h := NewHubHandler().WithBudget(budget)
	require.NotNil(t, h)

	start := time.Now()
	require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventToolStart, Data: hooks.EventData{ToolName: "Bash"}}))
	assert.Error(t, h.ReportCounts(1, 1))
	assert.Error(t, h.ReportLimitsExceeded("m"))
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 2*time.Second, "three calls on a 300ms budget took %s", elapsed)
}

// Without a budget a call keeps its own per-call cap, so callers outside the
// hook (init, ask_user) are unaffected.
func TestHubHandler_WithBudgetNilSafe(t *testing.T) {
	var h *HubHandler
	assert.Nil(t, h.WithBudget(context.Background()))
	assert.Nil(t, NewHubHandlerForClient(nil))
}

type recordedHubRequest struct {
	body          map[string]interface{}
	triggerExists bool
}

// A tripped limit signals init before any Hub call, and the hook no longer
// reports limits_exceeded itself (init does). The trigger file carries the
// limit message for init to report.
func TestLimitsHandler_TripSignalsBeforeHubAndLeavesReportToInit(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	triggerPath := filepath.Join(tmpDir, "scion-limits-exceeded")

	var mu sync.Mutex
	var reqs []recordedHubRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, statErr := os.Stat(triggerPath)
		mu.Lock()
		reqs = append(reqs, recordedHubRequest{body: body, triggerExists: statErr == nil})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	setTestHubEnv(t, server.URL)

	limitsPath := filepath.Join(tmpDir, "agent-limits.json")
	require.NoError(t, InitLimitsFile(limitsPath, 1, 0, 0, 0))
	h := &LimitsHandler{
		maxTurns:        1,
		limitsPath:      limitsPath,
		triggerFilePath: triggerPath,
		statusHandler:   &StatusHandler{StatusPath: filepath.Join(tmpDir, "agent-info.json")},
		hub:             NewHubHandler(),
	}
	require.NotNil(t, h.hub)

	require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventAgentEnd}))

	content, err := os.ReadFile(triggerPath)
	require.NoError(t, err)
	assert.Equal(t, "max_turns of 1 exceeded (completed 1)", string(content))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, reqs, 1, "expected only the counts report")
	assert.True(t, reqs[0].triggerExists, "trigger file must be written before any Hub call")
	assert.Equal(t, float64(1), reqs[0].body["currentTurns"])
	for _, r := range reqs {
		assert.NotEqual(t, "limits_exceeded", r.body["activity"], "hook must not report limits_exceeded")
	}
}

// NewLimitsHandler reports counts through the HubHandler it is given (the
// hook's shared one) and makes no Hub call when given none.
func TestNewLimitsHandler_UsesGivenHubHandler(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SCION_MAX_TURNS", "100")
	t.Setenv("SCION_MAX_MODEL_CALLS", "")

	var mu sync.Mutex
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	setTestHubEnv(t, server.URL)
	require.NoError(t, InitLimitsFile(filepath.Join(tmpDir, "agent-limits.json"), 100, 0, 0, 0))

	require.NoError(t, NewLimitsHandler(nil).Handle(&hooks.Event{Name: hooks.EventAgentEnd}))
	mu.Lock()
	assert.Equal(t, 0, calls, "no HubHandler given: no Hub call")
	mu.Unlock()

	require.NoError(t, NewLimitsHandler(NewHubHandler()).Handle(&hooks.Event{Name: hooks.EventAgentEnd}))
	mu.Lock()
	assert.Equal(t, 1, calls)
	mu.Unlock()
}

func TestReadLimitsTriggerMessage(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte(content), 0600))
		return p
	}

	assert.Equal(t, "max_turns of 5 exceeded (completed 5)",
		ReadLimitsTriggerMessage(write("msg", "max_turns of 5 exceeded (completed 5)\n")))
	assert.Equal(t, DefaultLimitsExceededMessage, ReadLimitsTriggerMessage(filepath.Join(dir, "missing")))
	assert.Equal(t, DefaultLimitsExceededMessage, ReadLimitsTriggerMessage(write("empty", "")))
	assert.Equal(t, DefaultLimitsExceededMessage, ReadLimitsTriggerMessage(write("legacy", "exceeded")),
		"the fixed marker older hooks wrote carries no message")
	assert.Equal(t, "ab", ReadLimitsTriggerMessage(write("ctrl", "a\x1b\x00b")))

	long := ReadLimitsTriggerMessage(write("long", strings.Repeat("x", 500)))
	assert.Len(t, long, limitsTriggerMessageMaxLen)

	assert.Equal(t, DefaultLimitsExceededMessage,
		ReadLimitsTriggerMessage(write("huge", strings.Repeat("x", limitsTriggerMaxBytes+1))),
		"oversize file is refused")

	secret := write("secret", "do not leak")
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(secret, link))
	assert.Equal(t, DefaultLimitsExceededMessage, ReadLimitsTriggerMessage(link), "symlink is refused")
}
