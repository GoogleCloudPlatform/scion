package cloudrun

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/iterator"
	logtype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCloudRunInstanceLogFilterEscapesValues(t *testing.T) {
	filter := cloudRunInstanceLogFilter(`agent" OR severity>=ERROR \ tail`, `project" OR true \ id`, time.Time{}, time.Time{})

	want := `resource.type="cloud_run_instance" AND resource.labels.instance_name="agent\" OR severity>=ERROR \\ tail" AND resource.labels.project_id="project\" OR true \\ id"`
	if filter != want {
		t.Fatalf("filter = %q, want %q", filter, want)
	}

	if strings.Contains(filter, `instance_name="agent" OR`) {
		t.Fatalf("instance name was not escaped in filter: %s", filter)
	}
	if strings.Contains(filter, `project_id="project" OR`) {
		t.Fatalf("project ID was not escaped in filter: %s", filter)
	}
}

func TestCloudRunInstanceLogFilterIncludesTimestamp(t *testing.T) {
	lastSeen := time.Date(2026, 6, 28, 1, 2, 3, 4, time.UTC)
	filter := cloudRunInstanceLogFilter("agent", "project", lastSeen, time.Time{})

	wantTimestamp := `timestamp > "2026-06-28T01:02:03.000000004Z"`
	if !strings.Contains(filter, wantTimestamp) {
		t.Fatalf("filter %q does not include %q", filter, wantTimestamp)
	}
}

func TestGetLogsReturnsEmptyResults(t *testing.T) {
	now := fixLogClock(t)
	client := &fakeLogEntryClient{
		iterators: []logEntryIterator{
			&fakeLogEntryIterator{err: iterator.Done},
		},
	}
	logClient := &LogClient{client: client, projectID: "project"}

	entries, err := logClient.GetLogs(context.Background(), "agent", LogOptions{Lines: 10})
	if err != nil {
		t.Fatalf("GetLogs returned error: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("GetLogs returned %d entries, want 0", len(entries))
	}

	if got := client.requests[0].Filter; got != cloudRunInstanceLogFilter("agent", "project", time.Time{}, now.Add(-logLookback)) {
		t.Fatalf("filter = %q", got)
	}
}

func TestGetLogsMapsEntriesChronologically(t *testing.T) {
	first := time.Date(2026, 6, 28, 1, 0, 0, 0, time.UTC)
	second := first.Add(time.Minute)
	client := &fakeLogEntryClient{
		iterators: []logEntryIterator{
			&fakeLogEntryIterator{
				entries: []*loggingpb.LogEntry{
					{
						Timestamp: timestamppb.New(second),
						Payload:   &loggingpb.LogEntry_TextPayload{TextPayload: "newer"},
						Severity:  logtype.LogSeverity_ERROR,
					},
					{
						Timestamp: timestamppb.New(first),
						Payload:   &loggingpb.LogEntry_TextPayload{TextPayload: "older"},
						Severity:  logtype.LogSeverity_INFO,
					},
				},
			},
		},
	}
	logClient := &LogClient{client: client, projectID: "project"}

	entries, err := logClient.GetLogs(context.Background(), "agent", LogOptions{})
	if err != nil {
		t.Fatalf("GetLogs returned error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("GetLogs returned %d entries, want 2", len(entries))
	}
	if entries[0].Message != "older" || entries[1].Message != "newer" {
		t.Fatalf("entries are not chronological: %#v", entries)
	}
}

func TestStreamLogsPropagatesListingErrors(t *testing.T) {
	now := fixLogClock(t)
	oldPollInterval := logPollInterval
	oldPollBackoff := logPollBackoff
	logPollInterval = time.Millisecond
	logPollBackoff = 5 * time.Millisecond
	t.Cleanup(func() {
		logPollInterval = oldPollInterval
		logPollBackoff = oldPollBackoff
	})

	listErr := errors.New("permission denied")
	client := &fakeLogEntryClient{
		iterators: []logEntryIterator{
			&fakeLogEntryIterator{err: listErr},
		},
	}
	logClient := &LogClient{client: client, projectID: "project"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := logClient.StreamLogs(ctx, "agent", LogOptions{})
	if err != nil {
		t.Fatalf("StreamLogs returned error: %v", err)
	}
	// The StreamLogs goroutine reads logPollInterval/logPollBackoff and
	// appends to client.requests. Wait for it to exit (it closes ch) before
	// reading requests and before the cleanup above restores the globals.
	// Cleanups run LIFO, so this one runs before the restore.
	stopStream := func() {
		cancel()
		for range ch {
		}
	}
	t.Cleanup(stopStream)

	select {
	case entry := <-ch:
		if entry.Error == "" {
			t.Fatalf("stream entry Error is empty: %#v", entry)
		}
		if !strings.Contains(entry.Error, listErr.Error()) {
			t.Fatalf("stream entry Error = %q, want it to contain %q", entry.Error, listErr.Error())
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream error")
	}
	stopStream()

	// The first poll, with nothing seen yet, is bounded by logLookback.
	if got := client.requests[0].Filter; got != cloudRunInstanceLogFilter("agent", "project", time.Time{}, now.Add(-logLookback)) {
		t.Fatalf("filter = %q", got)
	}
}

type fakeLogEntryClient struct {
	requests  []*loggingpb.ListLogEntriesRequest
	iterators []logEntryIterator
}

func (c *fakeLogEntryClient) ListLogEntries(_ context.Context, req *loggingpb.ListLogEntriesRequest) logEntryIterator {
	c.requests = append(c.requests, req)
	if len(c.iterators) == 0 {
		return &fakeLogEntryIterator{err: iterator.Done}
	}
	it := c.iterators[0]
	c.iterators = c.iterators[1:]
	return it
}

func (c *fakeLogEntryClient) Close() error {
	return nil
}

type fakeLogEntryIterator struct {
	entries []*loggingpb.LogEntry
	err     error
}

func (it *fakeLogEntryIterator) Next() (*loggingpb.LogEntry, error) {
	if it.err != nil {
		err := it.err
		it.err = nil
		return nil, err
	}
	if len(it.entries) == 0 {
		return nil, iterator.Done
	}
	entry := it.entries[0]
	it.entries = it.entries[1:]
	return entry, nil
}

// fixLogClock pins logNow to a fixed instant for the test and returns it.
func fixLogClock(t *testing.T) time.Time {
	t.Helper()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	old := logNow
	logNow = func() time.Time { return now }
	t.Cleanup(func() { logNow = old })
	return now
}

// setLogQueryTimeout sets logQueryTimeout for the test.
func setLogQueryTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := logQueryTimeout
	logQueryTimeout = d
	t.Cleanup(func() { logQueryTimeout = old })
}

func logEntryAt(ts time.Time, msg string) *loggingpb.LogEntry {
	return &loggingpb.LogEntry{
		Timestamp: timestamppb.New(ts),
		Payload:   &loggingpb.LogEntry_TextPayload{TextPayload: msg},
	}
}

// ctxLogEntryClient hands out ctxLogEntryIterators bound to the context
// ListLogEntries was called with, like the real client.
type ctxLogEntryClient struct {
	requests []*loggingpb.ListLogEntriesRequest
	entries  []*loggingpb.LogEntry
}

func (c *ctxLogEntryClient) ListLogEntries(ctx context.Context, req *loggingpb.ListLogEntriesRequest) logEntryIterator {
	c.requests = append(c.requests, req)
	return &ctxLogEntryIterator{ctx: ctx, entries: c.entries}
}

func (c *ctxLogEntryClient) Close() error { return nil }

// ctxLogEntryIterator yields its entries, then blocks until its context
// ends, as a read still paging through empty pages does, and returns the
// context's error.
type ctxLogEntryIterator struct {
	ctx     context.Context
	entries []*loggingpb.LogEntry
}

func (it *ctxLogEntryIterator) Next() (*loggingpb.LogEntry, error) {
	if len(it.entries) > 0 {
		e := it.entries[0]
		it.entries = it.entries[1:]
		return e, nil
	}
	<-it.ctx.Done()
	return nil, it.ctx.Err()
}

func TestGetLogsFilterHasLookbackBound(t *testing.T) {
	now := fixLogClock(t)
	client := &fakeLogEntryClient{}
	logClient := &LogClient{client: client, projectID: "project"}

	if _, err := logClient.GetLogs(context.Background(), "agent", LogOptions{Lines: 10}); err != nil {
		t.Fatalf("GetLogs returned error: %v", err)
	}

	req := client.requests[0]
	want := `timestamp >= "` + now.Add(-logLookback).Format(time.RFC3339Nano) + `"`
	if !strings.Contains(req.Filter, want) {
		t.Fatalf("filter %q does not include %q", req.Filter, want)
	}
	if req.OrderBy != "timestamp desc" {
		t.Fatalf("OrderBy = %q, want newest first", req.OrderBy)
	}
}

func TestGetLogsReturnsNewestTailChronologically(t *testing.T) {
	base := time.Date(2026, 11, 2, 11, 0, 0, 0, time.UTC)
	// Newest first, as the timestamp desc query returns them.
	client := &fakeLogEntryClient{
		iterators: []logEntryIterator{
			&fakeLogEntryIterator{entries: []*loggingpb.LogEntry{
				logEntryAt(base.Add(4*time.Minute), "line4"),
				logEntryAt(base.Add(3*time.Minute), "line3"),
				logEntryAt(base.Add(2*time.Minute), "line2"),
				logEntryAt(base.Add(1*time.Minute), "line1"),
			}},
		},
	}
	logClient := &LogClient{client: client, projectID: "project"}

	entries, err := logClient.GetLogs(context.Background(), "agent", LogOptions{Lines: 2})
	if err != nil {
		t.Fatalf("GetLogs returned error: %v", err)
	}
	if len(entries) != 2 || entries[0].Message != "line3" || entries[1].Message != "line4" {
		t.Fatalf("entries = %#v, want the newest two (line3, line4) in order", entries)
	}
}

func TestGetLogsDeadlineReturnsPartialEntries(t *testing.T) {
	setLogQueryTimeout(t, 20*time.Millisecond)
	base := time.Date(2026, 11, 2, 11, 0, 0, 0, time.UTC)
	client := &ctxLogEntryClient{entries: []*loggingpb.LogEntry{
		logEntryAt(base.Add(2*time.Minute), "newest"),
		logEntryAt(base.Add(1*time.Minute), "older"),
	}}
	logClient := &LogClient{client: client, projectID: "project"}

	entries, err := logClient.GetLogs(context.Background(), "agent", LogOptions{Lines: 100})
	if err != nil {
		t.Fatalf("GetLogs returned error: %v", err)
	}
	if len(entries) != 2 || entries[0].Message != "older" || entries[1].Message != "newest" {
		t.Fatalf("entries = %#v, want the newest entries read before the deadline, in order", entries)
	}
}

func TestGetLogsDeadlineNoEntriesClearError(t *testing.T) {
	setLogQueryTimeout(t, 20*time.Millisecond)
	logClient := &LogClient{client: &ctxLogEntryClient{}, projectID: "project"}

	start := time.Now()
	_, err := logClient.GetLogs(context.Background(), "agent", LogOptions{Lines: 100})
	if err == nil {
		t.Fatal("GetLogs returned no error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("GetLogs took %s, want it bounded by logQueryTimeout", time.Since(start))
	}
	for _, want := range []string{"the read for instance agent returned no entries", "before its 20ms deadline", "last 24h0m0s"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

func TestGetLogsCallerCancelStaysError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &ctxLogEntryClient{entries: []*loggingpb.LogEntry{
		logEntryAt(time.Date(2026, 11, 2, 11, 0, 0, 0, time.UTC), "line"),
	}}
	logClient := &LogClient{client: client, projectID: "project"}

	_, err := logClient.GetLogs(ctx, "agent", LogOptions{Lines: 100})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the caller's context.Canceled", err)
	}
}

func TestGetLogsPermissionDeniedNamesPermission(t *testing.T) {
	denied := status.Error(codes.PermissionDenied, "denied")
	client := &fakeLogEntryClient{
		iterators: []logEntryIterator{&fakeLogEntryIterator{err: denied}},
	}
	logClient := &LogClient{client: client, projectID: "project"}

	_, err := logClient.GetLogs(context.Background(), "agent", LogOptions{Lines: 10})
	if err == nil || !strings.Contains(err.Error(), "logging.logEntries.list") {
		t.Fatalf("err = %v, want it to name logging.logEntries.list", err)
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("status code = %v, want the wrapped PermissionDenied", status.Code(err))
	}
}

// A caller deadline shorter than logQueryTimeout still wins: the read ends
// with the caller's error, not a partial result.
func TestGetLogsShorterCallerDeadlineStaysError(t *testing.T) {
	setLogQueryTimeout(t, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	client := &ctxLogEntryClient{entries: []*loggingpb.LogEntry{
		logEntryAt(time.Date(2026, 11, 2, 11, 0, 0, 0, time.UTC), "line"),
	}}
	logClient := &LogClient{client: client, projectID: "project"}

	entries, err := logClient.GetLogs(ctx, "agent", LogOptions{Lines: 100})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("entries = %#v, err = %v; want the caller's context.DeadlineExceeded", entries, err)
	}
}

// After an entry has been seen, a poll is bounded by lastSeen only, with
// no lookback term.
func TestStreamLogsLaterPollUsesLastSeen(t *testing.T) {
	fixLogClock(t)
	oldPollInterval := logPollInterval
	logPollInterval = time.Millisecond
	t.Cleanup(func() { logPollInterval = oldPollInterval })

	seen := time.Date(2026, 11, 2, 11, 30, 0, 0, time.UTC)
	client := &notifyingLogEntryClient{
		polls: make(chan *loggingpb.ListLogEntriesRequest, 16),
		first: []*loggingpb.LogEntry{logEntryAt(seen, "first")},
	}
	logClient := &LogClient{client: client, projectID: "project"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := logClient.StreamLogs(ctx, "agent", LogOptions{})
	if err != nil {
		t.Fatalf("StreamLogs returned error: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		for range ch {
		}
	})

	var reqs []*loggingpb.ListLogEntriesRequest
	for len(reqs) < 2 {
		select {
		case req := <-client.polls:
			reqs = append(reqs, req)
		case entry := <-ch:
			if entry.Message != "first" {
				t.Fatalf("unexpected stream entry %#v", entry)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("got %d polls, want 2", len(reqs))
		}
	}
	want := cloudRunInstanceLogFilter("agent", "project", seen, time.Time{})
	if got := reqs[1].Filter; got != want {
		t.Fatalf("second poll filter = %q, want %q", got, want)
	}
}

// notifyingLogEntryClient sends each request on polls (the stream's
// goroutine calls it), answering the first with first and later ones with
// no entries.
type notifyingLogEntryClient struct {
	polls chan *loggingpb.ListLogEntriesRequest
	first []*loggingpb.LogEntry
	calls int
}

func (c *notifyingLogEntryClient) ListLogEntries(_ context.Context, req *loggingpb.ListLogEntriesRequest) logEntryIterator {
	c.calls++
	entries := c.first
	if c.calls > 1 {
		entries = nil
	}
	select {
	case c.polls <- req:
	default:
	}
	return &fakeLogEntryIterator{entries: entries}
}

func (c *notifyingLogEntryClient) Close() error { return nil }
