/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// hookResponseDeadline is the generous CI bound for a hook run whose
// telemetry receiver is missing. Antigravity kills a hook after 10s; before
// the fix such a run took ~15s.
const hookResponseDeadline = 2 * time.Second

// antigravityPayload returns the raw agy 1.2.12 hook payload for the given
// event from the dialect fixtures.
func antigravityPayload(t *testing.T, event string) []byte {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "..", "pkg", "sciontool", "hooks", "dialects", "testdata", "antigravity", "hook-payloads-1.2.12.jsonl"))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	needle := []byte(`"hook_event_name": "` + event + `"`)
	for scanner.Scan() {
		if bytes.Contains(scanner.Bytes(), needle) {
			return append([]byte(nil), scanner.Bytes()...)
		}
	}
	require.NoError(t, scanner.Err())
	t.Fatalf("no %s payload in antigravity fixture", event)
	return nil
}

// setupAntigravityHook installs the bundled antigravity dialect in a temp
// HOME, enables telemetry pointed at grpcPort, and returns a buffer that
// captures the hook response.
func setupAntigravityHook(t *testing.T, grpcPort int) *bytes.Buffer {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	scrubScionEnv(t)
	setTestLogPath(t, filepath.Join(tmpDir, "agent.log"))
	t.Setenv("SCION_TELEMETRY_ENABLED", "true")
	t.Setenv(telemetry.EnvGRPCPort, strconv.Itoa(grpcPort))

	dialectYAML, err := os.ReadFile(filepath.Join("..", "..", "..", "harnesses", "antigravity", "dialect.yaml"))
	require.NoError(t, err)
	bundleDir := filepath.Join(tmpDir, ".scion", "harness")
	require.NoError(t, os.MkdirAll(bundleDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(bundleDir, "dialect.yaml"), dialectYAML, 0644))

	oldDialect := hookDialect
	hookDialect = "antigravity"
	t.Cleanup(func() { hookDialect = oldDialect })

	var out bytes.Buffer
	oldStdout := hookStdout
	hookStdout = &out
	t.Cleanup(func() { hookStdout = oldStdout })
	return &out
}

// closedLoopbackPort returns a loopback port with nothing listening on it.
func closedLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

// Regression for antigravity PreToolUse timeouts: with telemetry enabled and
// no receiver, the hook used to block ~10s on the span/log export before
// answering and ~5s more in shutdown, past agy's 10s hook timeout.
func TestProcessHookData_AntigravityNoReceiverAnswersPromptly(t *testing.T) {
	out := setupAntigravityHook(t, closedLoopbackPort(t))

	start := time.Now()
	err := processHookData(antigravityPayload(t, "PreToolUse"))
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.JSONEq(t, `{"decision":"allow"}`, out.String())
	assert.Less(t, elapsed, hookResponseDeadline, "hook with no telemetry receiver took %s", elapsed)
}

// A receiver that accepts TCP connections but never speaks gRPC cannot fail
// fast; the export and shutdown bounds must still cap the hook.
func TestProcessHookData_AntigravityUnresponsiveReceiverIsBounded(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var conns []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
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

	out := setupAntigravityHook(t, l.Addr().(*net.TCPAddr).Port)

	start := time.Now()
	err = processHookData(antigravityPayload(t, "PostToolUse"))
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.JSONEq(t, `{}`, out.String())
	assert.Less(t, elapsed, hookResponseDeadline, "hook with an unresponsive receiver took %s", elapsed)
}

// orderedWriter records how much telemetry the receiver had seen at the
// moment the hook response was written.
type orderedWriter struct {
	buf           bytes.Buffer
	seenAtWrite   int64
	writes        int
	receivedSoFar func() int64
}

func (w *orderedWriter) Write(p []byte) (int, error) {
	if w.writes == 0 {
		w.seenAtWrite = w.receivedSoFar()
	}
	w.writes++
	return w.buf.Write(p)
}

// With a live receiver, the hook's span, log and metric exports still
// arrive, and only after the response has been written.
func TestProcessHookData_TelemetryExportedAfterResponse(t *testing.T) {
	var spans, logs, metrics atomic.Int64
	receiver := telemetry.NewReceiver(&telemetry.Config{Enabled: true},
		func(_ context.Context, batches []*tracepb.ResourceSpans) error {
			spans.Add(int64(len(batches)))
			return nil
		},
		telemetry.WithLogHandler(func(_ context.Context, batches []*logspb.ResourceLogs) error {
			logs.Add(int64(len(batches)))
			return nil
		}),
		telemetry.WithMetricHandler(func(_ context.Context, batches []*metricpb.ResourceMetrics) error {
			metrics.Add(int64(len(batches)))
			return nil
		}),
	)
	require.NoError(t, receiver.Start(context.Background()))
	t.Cleanup(func() { _ = receiver.Stop(context.Background()) })
	grpcPort, _ := receiver.BoundPorts()

	setupAntigravityHook(t, grpcPort)
	w := &orderedWriter{receivedSoFar: func() int64 { return spans.Load() + logs.Load() + metrics.Load() }}
	hookStdout = w

	require.NoError(t, processHookData(antigravityPayload(t, "PostToolUse")))

	assert.JSONEq(t, `{}`, w.buf.String())
	assert.Equal(t, 1, w.writes)
	assert.Zero(t, w.seenAtWrite, "telemetry reached the receiver before the hook response was written")
	assert.Positive(t, spans.Load(), "span export did not reach the receiver")
	assert.Positive(t, logs.Load(), "log export did not reach the receiver")
	assert.Positive(t, metrics.Load(), "metric export did not reach the receiver")
}
