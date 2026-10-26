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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/eventstreams"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/dbmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/ssemetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// streamLogCapture collects the JSON records of a slog logger.
type streamLogCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *streamLogCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

// closeLines returns the "events stream closed" records.
func (c *streamLogCapture) closeLines(t *testing.T) []map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(c.buf.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "log line %q", line)
		if rec["msg"] == "events stream closed" {
			out = append(out, rec)
		}
	}
	return out
}

// newStreamTestServer is a dev-auth web server with an in-memory publisher
// and a capturing logger, serving on an httptest server.
func newStreamTestServer(t *testing.T, override func(*WebServerConfig)) (*WebServer, *ChannelEventPublisher, *httptest.Server, *streamLogCapture) {
	t.Helper()
	ws := newDevAuthWebServer(t, func(cfg *WebServerConfig) {
		cfg.SSEHeartbeatInterval = 50 * time.Millisecond
		cfg.SSEMaxConnectionAge = time.Minute
		if override != nil {
			override(cfg)
		}
	})
	logs := &streamLogCapture{}
	ws.log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	pub := NewChannelEventPublisher()
	ws.SetEventPublisher(pub)
	ws.SetAuthzService(NewAuthzService(mockSuperAdminStore(DevUserID), nil))
	t.Cleanup(pub.Close)
	ts := httptest.NewServer(devSignedIn(t, ws))
	t.Cleanup(ts.Close)
	return ws, pub, ts, logs
}

// openEventStream opens an events stream with the given extra query and waits
// for its first heartbeat, so the stream is registered when it returns.
func openEventStream(t *testing.T, ctx context.Context, client *http.Client, base, query string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events?sub=project.test.>"+query, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	waitForHeartbeat(t, bufio.NewReader(resp.Body))
	return resp
}

func waitForHeartbeat(t *testing.T, r *bufio.Reader) {
	t.Helper()
	for {
		line, err := r.ReadString('\n')
		require.NoError(t, err, "stream ended before its first heartbeat")
		if strings.TrimSpace(line) == "event: heartbeat" {
			return
		}
	}
}

func waitForStreamCount(t *testing.T, reg *eventstreams.Registry, want int) {
	t.Helper()
	require.Eventually(t, func() bool { return reg.Count() == want },
		5*time.Second, 10*time.Millisecond, "registry count did not reach %d (now %d)", want, reg.Count())
}

func closeReasons(lines []map[string]any) map[string]int {
	out := map[string]int{}
	for _, l := range lines {
		r, _ := l["reason"].(string)
		out[r]++
	}
	return out
}

// TestEventStreamRegistry_ClientDisconnect opens N streams, closes them
// from the client, and checks the registry empties and each close logs
// exactly one line with reason client (ptone/scion#5227).
func TestEventStreamRegistry_ClientDisconnect(t *testing.T) {
	ws, _, ts, logs := newStreamTestServer(t, nil)
	const n = 5
	var cancels []context.CancelFunc
	var bodies []io.Closer
	for i := 0; i < n; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		query := ""
		if i == 0 {
			query = "&browser=b-1&tab=t_1&kind=view"
		}
		resp := openEventStream(t, ctx, http.DefaultClient, ts.URL, query)
		bodies = append(bodies, resp.Body)
	}
	reg := ws.EventStreams()
	waitForStreamCount(t, reg, n)
	assert.Equal(t, 1, reg.Users(), "every stream belongs to the dev user")
	assert.Equal(t, map[eventstreams.Kind]int{eventstreams.KindView: 1, eventstreams.KindOther: n - 1}, reg.CountByKind())

	snap := reg.Snapshot()
	require.Len(t, snap, n)
	first := snap[0].Key
	assert.Equal(t, DevUserID, first.UserID)
	assert.Equal(t, "b-1", first.BrowserID)
	assert.Equal(t, "t_1", first.TabID)
	assert.Equal(t, eventstreams.KindView, first.Kind)
	assert.True(t, first.HasClientIDs())
	assert.Equal(t, 1, snap[0].Subjects)
	for _, s := range snap {
		assert.Len(t, s.Key.ConnID, 16)
		assert.Zero(t, s.EventsWritten, "a heartbeat is not an event")
	}
	// The handler counts a heartbeat's bytes after it flushes them, so the
	// count can trail the client's read.
	require.Eventually(t, func() bool {
		for _, s := range reg.Snapshot() {
			if s.BytesWritten <= 0 {
				return false
			}
		}
		return true
	}, 5*time.Second, 10*time.Millisecond, "the heartbeat counts as bytes written")
	assert.Empty(t, logs.closeLines(t), "no close line while the streams are open")

	for i := range cancels {
		cancels[i]()
		_ = bodies[i].Close()
	}
	waitForStreamCount(t, reg, 0)
	require.Eventually(t, func() bool { return len(logs.closeLines(t)) == n }, 5*time.Second, 10*time.Millisecond)
	lines := logs.closeLines(t)
	assert.Equal(t, map[string]int{"client": n}, closeReasons(lines))
	conns := map[string]bool{}
	for _, l := range lines {
		assert.Equal(t, "INFO", l["level"])
		assert.Equal(t, DevUserID, l["user_id"])
		conns[l["conn_id"].(string)] = true
		for _, k := range []string{"browser_id", "tab_id", "kind", "subjects", "duration_ms", "events_written", "bytes_written"} {
			assert.Contains(t, l, k)
		}
	}
	assert.Len(t, conns, n, "one close line per stream")
	assert.Equal(t, 0, reg.Users())
}

// TestEventStreamRegistry_MaxAge checks streams closed at max age log
// reason max_age and leave the registry (ptone/scion#5227).
func TestEventStreamRegistry_MaxAge(t *testing.T) {
	ws, _, ts, logs := newStreamTestServer(t, func(cfg *WebServerConfig) {
		cfg.SSEHeartbeatInterval = 20 * time.Millisecond
		cfg.SSEMaxConnectionAge = 300 * time.Millisecond
	})
	const n = 3
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		resp := openEventStream(t, ctx, http.DefaultClient, ts.URL, "&kind=agent-feed")
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.Copy(io.Discard, resp.Body) // until the server closes it
		}()
	}
	wg.Wait()
	waitForStreamCount(t, ws.EventStreams(), 0)
	require.Eventually(t, func() bool { return len(logs.closeLines(t)) == n }, 5*time.Second, 10*time.Millisecond)
	lines := logs.closeLines(t)
	assert.Equal(t, map[string]int{"max_age": n}, closeReasons(lines))
	for _, l := range lines {
		assert.Equal(t, "agent-feed", l["kind"])
		assert.GreaterOrEqual(t, l["duration_ms"].(float64), float64(250))
	}
}

// TestEventStreamRegistry_PublisherAndShutdownReasons covers the other
// exit paths of handleSSE: the publisher closing and the server shutting
// down.
func TestEventStreamRegistry_PublisherAndShutdownReasons(t *testing.T) {
	t.Run("publisher", func(t *testing.T) {
		ws, pub, ts, logs := newStreamTestServer(t, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resp := openEventStream(t, ctx, http.DefaultClient, ts.URL, "")
		defer func() { _ = resp.Body.Close() }()
		waitForStreamCount(t, ws.EventStreams(), 1)
		pub.Close()
		waitForStreamCount(t, ws.EventStreams(), 0)
		require.Eventually(t, func() bool { return len(logs.closeLines(t)) == 1 }, 5*time.Second, 10*time.Millisecond)
		assert.Equal(t, map[string]int{"publisher": 1}, closeReasons(logs.closeLines(t)))
	})
	t.Run("shutdown", func(t *testing.T) {
		ws, _, ts, logs := newStreamTestServer(t, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resp := openEventStream(t, ctx, http.DefaultClient, ts.URL, "")
		defer func() { _ = resp.Body.Close() }()
		waitForStreamCount(t, ws.EventStreams(), 1)
		ws.BeginShutdown()
		waitForStreamCount(t, ws.EventStreams(), 0)
		require.Eventually(t, func() bool { return len(logs.closeLines(t)) == 1 }, 5*time.Second, 10*time.Millisecond)
		assert.Equal(t, map[string]int{"shutdown": 1}, closeReasons(logs.closeLines(t)))
	})
}

// TestEventStreamRegistry_EventsCounted checks an update event written to
// a stream counts as an event and its bytes.
func TestEventStreamRegistry_EventsCounted(t *testing.T) {
	ws, pub, ts, _ := newStreamTestServer(t, func(cfg *WebServerConfig) {
		cfg.SSEHeartbeatInterval = time.Minute
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events?sub=project.test.>", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	waitForStreamCount(t, ws.EventStreams(), 1)

	pub.PublishRaw("project.test.thing", map[string]string{"k": "v"})
	r := bufio.NewReader(resp.Body)
	for {
		line, err := r.ReadString('\n')
		require.NoError(t, err)
		if strings.HasPrefix(line, "data: ") {
			break
		}
	}
	require.Eventually(t, func() bool {
		s := ws.EventStreams().Snapshot()
		return len(s) == 1 && s[0].EventsWritten == 1
	}, 5*time.Second, 10*time.Millisecond)
	s := ws.EventStreams().Snapshot()[0]
	assert.Greater(t, s.BytesWritten, int64(len("data: ")))
}

func TestEventStreamKeyFromRequest(t *testing.T) {
	req := sseAgentRequest(context.Background(), "user-7", "member", "project.>")
	q := req.URL.Query()
	q.Set("browser", "B1")
	q.Set("tab", "bad tab!")
	q.Set("kind", "terminal-metadata")
	req.URL.RawQuery = q.Encode()

	k := eventStreamKeyFromRequest(req)
	assert.Equal(t, "user-7", k.UserID)
	assert.Equal(t, "B1", k.BrowserID)
	assert.Empty(t, k.TabID, "a malformed tab id is dropped")
	assert.Equal(t, eventstreams.KindTerminalMetadata, k.Kind)
	assert.Len(t, k.ConnID, 16)
	assert.False(t, k.HasClientIDs())

	bare := eventStreamKeyFromRequest(httptest.NewRequest(http.MethodGet, "/events?sub=x&kind=nope", nil))
	assert.Empty(t, bare.UserID)
	assert.Equal(t, eventstreams.KindOther, bare.Kind)
	assert.NotEqual(t, k.ConnID, bare.ConnID)
}

// --- scion.hub.events.published (ptone/scion#5228) ---

func newPublishMetrics(t *testing.T) (ssemetrics.Recorder, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	rec, err := ssemetrics.New(mp)
	require.NoError(t, err)
	return rec, reader
}

// publishedCounts returns scion.hub.events.published keyed by
// "subject_class/trigger".
func publishedCounts(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != ssemetrics.MetricEventsPublished {
				continue
			}
			for _, p := range m.Data.(metricdata.Sum[int64]).DataPoints {
				cls, _ := p.Attributes.Value(attribute.Key(ssemetrics.AttrSubjectClass))
				trg, _ := p.Attributes.Value(attribute.Key(ssemetrics.AttrTrigger))
				out[cls.AsString()+"/"+trg.AsString()] = p.Value
			}
		}
	}
	return out
}

// publishSample publishes one heartbeat status event, one ordinary status
// event and one raw event on p.
func publishSample(p EventPublisher) {
	agent := &store.Agent{ID: "agent-1", ProjectID: "project-1", Phase: "running"}
	p.PublishAgentStatus(eventstreams.WithHeartbeatTrigger(context.Background()), agent)
	p.PublishAgentStatus(context.Background(), agent)
	p.PublishRaw("system.images.job-9", map[string]string{"k": "v"})
}

var wantPublishedSample = map[string]int64{
	"agent.status/heartbeat":         1,
	"project.agent.status/heartbeat": 1,
	"agent.status/change":            1,
	"project.agent.status/change":    1,
	"system.images/change":           1,
}

func TestEventsPublished_ChannelPublisher(t *testing.T) {
	rec, reader := newPublishMetrics(t)
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	pub.SetPublishMetrics(rec)

	// Delivery is unchanged by the trigger: a subscriber sees both status
	// events on each subject.
	ch, unsubscribe := pub.Subscribe("agent.agent-1.status")
	defer unsubscribe()
	publishSample(pub)
	assert.Equal(t, wantPublishedSample, publishedCounts(t, reader))
	for i := 0; i < 2; i++ {
		select {
		case evt := <-ch:
			assert.Equal(t, "agent.agent-1.status", evt.Subject)
		case <-time.After(5 * time.Second):
			t.Fatal("status event not delivered")
		}
	}

	pub.Close()
	pub.PublishRaw("system.images.job-9", nil)
	assert.Equal(t, wantPublishedSample, publishedCounts(t, reader), "a closed publisher publishes, and counts, nothing")
}

// fakeNotifyExec records NOTIFY statements, or fails them.
type fakeNotifyExec struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeNotifyExec) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return pgconn.CommandTag{}, f.err
}

func TestEventsPublished_PostgresPublisher(t *testing.T) {
	rec, reader := newPublishMetrics(t)
	// Built inline, without a database (newTestPostgresPublisher is in a
	// !no_sqlite file and this one builds under every tag set).
	exec := &fakeNotifyExec{}
	p := &PostgresEventPublisher{
		metrics:        dbmetrics.NewDisabled(),
		log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		ctx:            context.Background(),
		subs:           make(map[string]map[*pgSubscription][]string),
		desired:        make(map[string]int),
		testNotifyExec: exec,
	}
	p.wireSinks()
	p.SetPublishMetrics(rec)

	publishSample(p)
	assert.Equal(t, wantPublishedSample, publishedCounts(t, reader))
	assert.Positive(t, exec.calls, "every publish NOTIFYs")

	// A failed NOTIFY is not a published event.
	exec.err = errors.New("notify failed")
	p.PublishRaw("system.images.job-9", nil)
	assert.Equal(t, wantPublishedSample, publishedCounts(t, reader))
}

func TestServerSetEventStreamMetricsForwardsToPublisher(t *testing.T) {
	t.Run("metrics then publisher", func(t *testing.T) {
		rec, reader := newPublishMetrics(t)
		s := &Server{}
		s.SetEventStreamMetrics(rec)
		assert.Equal(t, rec, s.EventStreamMetrics())
		pub := NewChannelEventPublisher()
		t.Cleanup(pub.Close)
		s.SetEventPublisher(pub)
		pub.PublishRaw("admin.settings.updated", nil)
		assert.Equal(t, map[string]int64{"admin.settings.updated/change": 1}, publishedCounts(t, reader))
	})
	t.Run("publisher then metrics", func(t *testing.T) {
		rec, reader := newPublishMetrics(t)
		s := &Server{}
		pub := NewChannelEventPublisher()
		t.Cleanup(pub.Close)
		s.SetEventPublisher(pub)
		assert.Nil(t, s.EventStreamMetrics())
		s.SetEventStreamMetrics(rec)
		pub.PublishRaw("admin.settings.updated", nil)
		assert.Equal(t, map[string]int64{"admin.settings.updated/change": 1}, publishedCounts(t, reader))
	})
}

func TestStatusUpdateIsHeartbeatOnly(t *testing.T) {
	turns := 3
	cases := []struct {
		name string
		su   store.AgentStatusUpdate
		want bool
	}{
		{"heartbeat alone", store.AgentStatusUpdate{Heartbeat: true}, true},
		{"heartbeat with hub-set preconditions", store.AgentStatusUpdate{
			Heartbeat: true, GuardReincarnation: true,
			ReportSource: store.StatusReportSourceAgent, ReportRunID: "run-1",
		}, true},
		{"no heartbeat", store.AgentStatusUpdate{}, false},
		{"heartbeat with phase", store.AgentStatusUpdate{Heartbeat: true, Phase: "running"}, false},
		{"heartbeat with activity", store.AgentStatusUpdate{Heartbeat: true, Activity: "thinking"}, false},
		{"heartbeat with turns", store.AgentStatusUpdate{Heartbeat: true, CurrentTurns: &turns}, false},
		{"activity alone", store.AgentStatusUpdate{Activity: "thinking"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, statusUpdateIsHeartbeatOnly(c.su))
		})
	}
}

// --- project wildcard paging (ptone/scion#1741) ---

func newWildcardServer(st *mockAuthzStore) *WebServer {
	return &WebServer{store: st, authzService: NewAuthzService(st, nil)}
}

func wildcardRequest(userID string) *http.Request {
	return sseAgentRequest(context.Background(), userID, "admin", "project.>")
}

func manyProjects(n int) []store.Project {
	out := make([]store.Project, n)
	for i := range out {
		id := fmt.Sprintf("p-%05d", i)
		out[i] = store.Project{ID: id, Name: id, Slug: id}
	}
	return out
}

func TestExpandProjectWildcard_DrainsEveryPage(t *testing.T) {
	st := mockSuperAdminStore("admin-1")
	st.projects = manyProjects(2503)
	st.projectPageSize = 1000
	ws := newWildcardServer(st)

	got := ws.expandProjectWildcard(wildcardRequest("admin-1"), []string{"project", ">"})
	require.Len(t, got, 2503, "projects past the first page must be expanded")
	assert.Equal(t, "project.p-00000.>", got[0])
	assert.Equal(t, "project.p-02502.>", got[2502])
	assert.Equal(t, 3, st.listProjectsCalls, "three pages of at most 1000")
	assert.Equal(t, sseProjectWildcardPageSize, st.lastListProjectsLimit)
}

func TestExpandProjectWildcard_ErrorOnLaterPageFailsClosed(t *testing.T) {
	st := mockSuperAdminStore("admin-1")
	st.projects = manyProjects(1500)
	st.projectPageSize = 1000
	st.listProjectsErrAfter = 1
	ws := newWildcardServer(st)

	got := ws.expandProjectWildcard(wildcardRequest("admin-1"), []string{"project", ">"})
	assert.Empty(t, got, "a list error on any page fails the whole expansion closed")
}

func TestExpandProjectWildcard_StopsOnStuckCursor(t *testing.T) {
	st := mockSuperAdminStore("admin-1")
	st.projects = manyProjects(3)
	st.projectPageSize = 2
	st.stuckProjectCursor = true
	ws := newWildcardServer(st)

	got := ws.expandProjectWildcard(wildcardRequest("admin-1"), []string{"project", ">"})
	assert.Len(t, got, 4, "the first page and the repeated one, then stop")
	assert.Equal(t, 2, st.listProjectsCalls)
}
