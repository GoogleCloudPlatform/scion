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
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCloudRunInstanceLogFilterEscapesValues(t *testing.T) {
	filter := cloudRunInstanceLogFilter(`agent" OR severity>=ERROR \ tail`, `project" OR true \ id`, time.Time{})

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
	filter := cloudRunInstanceLogFilter("agent", "project", lastSeen)

	wantTimestamp := `timestamp > "2026-06-28T01:02:03.000000004Z"`
	if !strings.Contains(filter, wantTimestamp) {
		t.Fatalf("filter %q does not include %q", filter, wantTimestamp)
	}
}

func TestGetLogsReturnsEmptyResults(t *testing.T) {
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

	if got := client.requests[0].Filter; got != cloudRunInstanceLogFilter("agent", "project", time.Time{}) {
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

	if got := client.requests[0].Filter; got != cloudRunInstanceLogFilter("agent", "project", time.Time{}) {
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
