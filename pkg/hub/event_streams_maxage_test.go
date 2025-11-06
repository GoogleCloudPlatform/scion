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
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// ptone/scion#5230: each stream's maximum age is the configured age with a
// +/-20% jitter, recorded on its registry entry.
func TestEventStreamMaxAge_JitterRange(t *testing.T) {
	for _, tc := range []struct {
		u    float64
		want time.Duration
	}{
		{0, 48 * time.Minute},
		{0.5, time.Hour},
		{0.999, time.Duration(float64(time.Hour) * (0.8 + 0.999*0.4))},
	} {
		u := tc.u
		ws, _, ts, _ := newStreamTestServer(t, func(cfg *WebServerConfig) {
			cfg.SSEMaxConnectionAge = time.Hour
			cfg.SSEMaxAgeJitter = func() float64 { return u }
		})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		resp := openEventStream(t, ctx, http.DefaultClient, ts.URL, "")
		snap := ws.EventStreams().Snapshot()
		require.Len(t, snap, 1)
		assert.Equal(t, tc.want, snap[0].MaxAge, "u=%v", u)
		_ = resp.Body.Close()
		cancel()
	}
}

// ptone/scion#5230: with no configured age the default is 10 minutes, and
// unpinned jitter keeps every stream within 8 to 12 minutes.
func TestEventStreamMaxAge_DefaultAndRandomJitter(t *testing.T) {
	ws := newDevAuthWebServer(t)
	assert.Equal(t, 10*time.Minute, config.SSEDefaultMaxConnectionAge)
	for i := 0; i < 200; i++ {
		d := ws.sseMaxAge()
		require.GreaterOrEqual(t, d, 8*time.Minute)
		require.LessOrEqual(t, d, 12*time.Minute)
	}
}

// ptone/scion#5230: a stream ends with the server's reconnect event at its
// jittered age (here the low end, 0.8 x 1s), recorded on the close line.
func TestEventStreamMaxAge_ReconnectAtJitteredAge(t *testing.T) {
	_, _, ts, logs := newStreamTestServer(t, func(cfg *WebServerConfig) {
		cfg.SSEHeartbeatInterval = time.Minute
		cfg.SSEMaxConnectionAge = time.Second
		cfg.SSEMaxAgeJitter = func() float64 { return 0 }
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events?sub=project.test.>", nil)
	require.NoError(t, err)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(bufio.NewReader(resp.Body))
	elapsed := time.Since(start)
	assert.True(t, strings.HasSuffix(string(body), "event: reconnect\ndata: {}\n\n"), "body %q", body)
	assert.GreaterOrEqual(t, elapsed, 700*time.Millisecond)
	require.Eventually(t, func() bool { return len(logs.closeLines(t)) == 1 }, 5*time.Second, 10*time.Millisecond)
	line := logs.closeLines(t)[0]
	assert.Equal(t, "max_age", line["reason"])
	assert.Equal(t, float64(800), line["max_age_ms"], "max_age_ms is the jittered age, 0.8 x 1s")
}
