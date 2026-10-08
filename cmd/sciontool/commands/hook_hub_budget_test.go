/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hookBudgetSlack is the CI allowance on top of hookHubBudget: local file
// work plus telemetry's own bound (about 0.75s).
const hookBudgetSlack = 1500 * time.Millisecond

// blackholeHub returns the URL of a listener that accepts connections and
// reads requests but never answers, like a Hub whose packets are dropped.
func blackholeHub(t *testing.T) string {
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

// Regression for ptone/scion#3610: with max_model_calls / max_turns set, an
// antigravity PostInvocation (model-end) or Stop (agent-end) made two Hub
// calls before answering, each capped at 5s, so a black-holed Hub held the
// answer for 10s, agy's hook timeout. All Hub calls now share
// hookHubBudget.
func TestProcessHookData_AntigravityBlackholedHubWithinBudget(t *testing.T) {
	for _, event := range []string{"PostInvocation", "Stop"} {
		t.Run(event, func(t *testing.T) {
			out := setupAntigravityHook(t, closedLoopbackPort(t))
			t.Setenv("SCION_HUB_ENDPOINT", blackholeHub(t))
			t.Setenv("SCION_AUTH_TOKEN", "test-token")
			t.Setenv("SCION_AGENT_ID", "test-agent")
			t.Setenv("SCION_MAX_TURNS", "1000")
			t.Setenv("SCION_MAX_MODEL_CALLS", "1000")
			home := os.Getenv("HOME")
			limitsPath := filepath.Join(home, "agent-limits.json")
			require.NoError(t, handlers.InitLimitsFile(limitsPath, 1000, 1000, 0, 0))

			start := time.Now()
			err := processHookData(antigravityPayload(t, event))
			elapsed := time.Since(start)

			require.NoError(t, err)
			assert.JSONEq(t, `{}`, out.String())
			assert.Less(t, elapsed, hookHubBudget+hookBudgetSlack, "%s with a black-holed Hub took %s", event, elapsed)

			// The counter still advanced locally.
			var ls handlers.LimitsState
			data, err := os.ReadFile(limitsPath)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(data, &ls))
			assert.Equal(t, 1, ls.TurnCount+ls.ModelCallCount)
		})
	}
}

// The session-end metrics report runs after the response but before the
// hook exits, so it shares the same budget: a Hub that stops answering made
// session-end take 5s (status) + 5s (metrics) before.
func TestProcessHookData_SessionEndMetricsShareHubBudget(t *testing.T) {
	home := t.TempDir()
	scrubScionEnv(t)
	t.Setenv("HOME", home)
	setTestLogPath(t, filepath.Join(home, "agent.log"))

	var hang atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if hang.Load() {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
	t.Setenv("SCION_AUTH_TOKEN", "test-token")
	t.Setenv("SCION_AGENT_ID", "hook-test-agent")
	t.Setenv("SCION_TELEMETRY_ENABLED", "true")
	t.Setenv("SCION_TELEMETRY_CLOUD_ENABLED", "false")
	t.Setenv("SCION_OTEL_GRPC_PORT", strconv.Itoa(startDiscardingReceiver(t)))

	oldDialect := hookDialect
	hookDialect = "claude"
	t.Cleanup(func() { hookDialect = oldDialect })

	for _, ev := range []map[string]interface{}{
		{"hook_event_name": "SessionStart", "session_id": "sess-budget", "source": "startup"},
		{"hook_event_name": "PostToolUse", "session_id": "sess-budget", "tool_name": "Bash"},
	} {
		data, err := json.Marshal(ev)
		require.NoError(t, err)
		require.NoError(t, processHookData(data))
	}

	hang.Store(true)
	data, err := json.Marshal(map[string]interface{}{"hook_event_name": "SessionEnd", "session_id": "sess-budget", "reason": "logout"})
	require.NoError(t, err)

	start := time.Now()
	require.NoError(t, processHookData(data))
	elapsed := time.Since(start)

	assert.Less(t, elapsed, hookHubBudget+hookBudgetSlack, "session-end with a hung Hub took %s", elapsed)
}

// init reports a hook-detected limit to the Hub with the message the hook
// wrote to the trigger file (the hook no longer reports it itself).
func TestReportHookLimitsExceeded(t *testing.T) {
	scrubScionEnv(t)
	var mu sync.Mutex
	var bodies []map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
	t.Setenv("SCION_AUTH_TOKEN", "test-token")
	t.Setenv("SCION_AGENT_ID", "test-agent")

	trigger := filepath.Join(t.TempDir(), "scion-limits-exceeded")
	require.NoError(t, os.WriteFile(trigger, []byte("max_model_calls of 3 exceeded (completed 3)"), 0600))

	reportHookLimitsExceeded(nil, trigger) // no Hub configured: no-op
	reportHookLimitsExceeded(handlers.NewHubHandler(), trigger)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, bodies, 1)
	assert.Equal(t, "limits_exceeded", bodies[0]["activity"])
	assert.Equal(t, "max_model_calls of 3 exceeded (completed 3)", bodies[0]["message"])
}
