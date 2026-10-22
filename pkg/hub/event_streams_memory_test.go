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

package hub

import (
	"bufio"
	"context"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	// memoryBaselineStreams is how many events streams the baseline opens.
	memoryBaselineStreams = 500
	// memoryBaselineCeiling is the per-stream heap ceiling. The design
	// estimates 25-50 KiB per stream; the ceiling is generous so only real
	// growth fails the test.
	memoryBaselineCeiling = 128 << 10
)

// heapInUse returns the live heap after a full collection.
func heapInUse() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// TestEventStreamMemoryBaseline opens 500 events streams against an
// in-process web server, waits for the first heartbeat on each, and reports
// the live-heap growth per stream (ptone/scion#5229). The figure includes
// the client side of each connection (the test's HTTP transport and
// readers), so it is an upper bound on the server's cost. It fails above
// memoryBaselineCeiling.
func TestEventStreamMemoryBaseline(t *testing.T) {
	if testing.Short() {
		t.Skip("memory baseline opens 500 streams; skipped in short mode")
	}
	ws, _, ts, _ := newStreamTestServer(t, func(cfg *WebServerConfig) {
		cfg.SSEHeartbeatInterval = 200 * time.Millisecond
	})
	transport := &http.Transport{}
	client := &http.Client{Transport: transport}
	t.Cleanup(transport.CloseIdleConnections)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	before := heapInUse()
	type open struct {
		resp *http.Response
		r    *bufio.Reader
	}
	streams := make([]open, 0, memoryBaselineStreams)
	defer func() {
		cancel()
		for _, s := range streams {
			_ = s.resp.Body.Close()
		}
	}()
	for i := 0; i < memoryBaselineStreams; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events?sub=project.test.>", nil)
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		streams = append(streams, open{resp: resp, r: bufio.NewReader(resp.Body)})
	}
	for _, s := range streams {
		waitForHeartbeat(t, s.r)
	}
	waitForStreamCount(t, ws.EventStreams(), memoryBaselineStreams)

	after := heapInUse()
	var delta uint64
	if after > before {
		delta = after - before
	}
	perStream := delta / memoryBaselineStreams
	t.Logf("events stream memory baseline: %d streams, heap %d -> %d bytes, %.1f KiB per stream (client side included)",
		memoryBaselineStreams, before, after, float64(perStream)/1024)
	require.LessOrEqual(t, perStream, uint64(memoryBaselineCeiling),
		"per-stream heap %d bytes exceeds the %d byte ceiling", perStream, memoryBaselineCeiling)

	cancel()
	for _, s := range streams {
		_ = s.resp.Body.Close()
	}
	streams = nil
	waitForStreamCount(t, ws.EventStreams(), 0)
}
