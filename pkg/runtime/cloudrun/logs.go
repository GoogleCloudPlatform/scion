package cloudrun

import (
	"context"
	"fmt"
	"strconv"
	"time"

	logging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/iterator"
)

const (
	defaultLogPollInterval = 2 * time.Second
	maxLogPollBackoff      = 30 * time.Second
)

var (
	logPollInterval = defaultLogPollInterval
	logPollBackoff  = maxLogPollBackoff
)

type LogOptions struct {
	Lines int
}

type LogEntry struct {
	Timestamp time.Time
	Message   string
	Severity  string
	Error     string
}

type LogClient struct {
	client    logEntryClient
	projectID string
}

type logEntryClient interface {
	ListLogEntries(context.Context, *loggingpb.ListLogEntriesRequest) logEntryIterator
	Close() error
}

type logEntryIterator interface {
	Next() (*loggingpb.LogEntry, error)
}

type googleLogEntryClient struct {
	client *logging.Client
}

func (c *googleLogEntryClient) ListLogEntries(ctx context.Context, req *loggingpb.ListLogEntriesRequest) logEntryIterator {
	return c.client.ListLogEntries(ctx, req)
}

func (c *googleLogEntryClient) Close() error {
	return c.client.Close()
}

func NewLogClient(ctx context.Context, projectID string) (*LogClient, error) {
	client, err := logging.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating logging client: %w", err)
	}
	return &LogClient{
		client:    &googleLogEntryClient{client: client},
		projectID: projectID,
	}, nil
}

func (c *LogClient) Close() error {
	return c.client.Close()
}

// GetLogs retrieves recent log lines for a Cloud Run Instance.
// Uses the Cloud Logging API filtering by resource labels.
func (c *LogClient) GetLogs(ctx context.Context, instanceName string, opts LogOptions) ([]LogEntry, error) {
	req := &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{"projects/" + c.projectID},
		Filter:        cloudRunInstanceLogFilter(instanceName, c.projectID, time.Time{}),
		OrderBy:       "timestamp desc",
	}
	if opts.Lines > 0 {
		req.PageSize = int32(opts.Lines)
	}

	it := c.client.ListLogEntries(ctx, req)
	var entries []LogEntry
	for {
		resp, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("listing log entries: %w", err)
		}

		msg := resp.GetTextPayload()
		if msg == "" && resp.GetJsonPayload() != nil {
			msg = resp.GetJsonPayload().String()
		}

		entries = append(entries, LogEntry{
			Timestamp: resp.GetTimestamp().AsTime(),
			Message:   msg,
			Severity:  resp.GetSeverity().String(),
		})

		if opts.Lines > 0 && len(entries) >= opts.Lines {
			break
		}
	}

	// Reverse to chronological order
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}

	return entries, nil
}

// StreamLogs tails log output in real time (for scion look / scion logs -f).
func (c *LogClient) StreamLogs(ctx context.Context, instanceName string, opts LogOptions) (<-chan LogEntry, error) {
	ch := make(chan LogEntry, 100)

	go func() {
		defer close(ch)

		var lastSeen time.Time
		consecutiveErrors := 0
		nextPoll := logPollInterval
		timer := time.NewTimer(nextPoll)
		defer timer.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				err := c.pollLogs(ctx, ch, instanceName, opts, &lastSeen)
				if err != nil {
					consecutiveErrors++
					msg := fmt.Sprintf("listing log entries: %v", err)
					if !sendLogEntry(ctx, ch, LogEntry{Timestamp: time.Now(), Message: msg, Severity: "ERROR", Error: msg}) {
						return
					}
					nextPoll = logBackoff(consecutiveErrors)
				} else {
					consecutiveErrors = 0
					nextPoll = logPollInterval
				}
				timer.Reset(nextPoll)
			}
		}
	}()

	return ch, nil
}

func (c *LogClient) pollLogs(ctx context.Context, ch chan<- LogEntry, instanceName string, opts LogOptions, lastSeen *time.Time) error {
	req := &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{"projects/" + c.projectID},
		Filter:        cloudRunInstanceLogFilter(instanceName, c.projectID, *lastSeen),
		OrderBy:       "timestamp asc",
	}
	if opts.Lines > 0 {
		req.PageSize = int32(opts.Lines)
	}

	it := c.client.ListLogEntries(ctx, req)
	for {
		resp, err := it.Next()
		if err == iterator.Done {
			return nil
		}
		if err != nil {
			return err
		}

		entry := logEntryFromResponse(resp)
		if entry.Timestamp.After(*lastSeen) {
			*lastSeen = entry.Timestamp
		}

		if !sendLogEntry(ctx, ch, entry) {
			return ctx.Err()
		}
	}
}

func logEntryFromResponse(resp *loggingpb.LogEntry) LogEntry {
	msg := resp.GetTextPayload()
	if msg == "" && resp.GetJsonPayload() != nil {
		msg = resp.GetJsonPayload().String()
	}

	return LogEntry{
		Timestamp: resp.GetTimestamp().AsTime(),
		Message:   msg,
		Severity:  resp.GetSeverity().String(),
	}
}

func sendLogEntry(ctx context.Context, ch chan<- LogEntry, entry LogEntry) bool {
	select {
	case ch <- entry:
		return true
	case <-ctx.Done():
		return false
	}
}

func cloudRunInstanceLogFilter(instanceName string, projectID string, lastSeen time.Time) string {
	filter := fmt.Sprintf(
		`resource.type="cloud_run_instance" AND resource.labels.instance_name=%s AND resource.labels.project_id=%s`,
		logFilterString(instanceName),
		logFilterString(projectID),
	)
	if !lastSeen.IsZero() {
		filter += fmt.Sprintf(` AND timestamp > %s`, logFilterString(lastSeen.Format(time.RFC3339Nano)))
	}
	return filter
}

func logFilterString(value string) string {
	return strconv.Quote(value)
}

func logBackoff(consecutiveErrors int) time.Duration {
	if consecutiveErrors <= 0 {
		return logPollInterval
	}

	backoff := logPollInterval
	for i := 1; i < consecutiveErrors; i++ {
		backoff *= 2
		if backoff >= logPollBackoff {
			return logPollBackoff
		}
	}
	return backoff
}
