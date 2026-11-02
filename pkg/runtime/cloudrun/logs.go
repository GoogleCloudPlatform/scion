package cloudrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	logging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultLogPollInterval = 2 * time.Second
	maxLogPollBackoff      = 30 * time.Second
)

var (
	logPollInterval = defaultLogPollInterval
	logPollBackoff  = maxLogPollBackoff

	// logLookback is how far back GetLogs and the first StreamLogs poll
	// read. Without a lower timestamp bound a Cloud Logging read covers the
	// whole retention period, and for an instance with few matching entries
	// it keeps paging (short or empty pages that still carry a page token)
	// until the caller gives up (ptone/scion#7101).
	logLookback = 24 * time.Hour

	// logQueryTimeout bounds one GetLogs read. It stays below the hub's
	// 60s HTTP write timeout, and below the 30s timeout common HTTP
	// clients use, so a slow read ends with an answer (the newest entries
	// read so far, or an error) instead of a request the caller has given
	// up on or the hub can no longer reply to (ptone/scion#7101).
	logQueryTimeout = 20 * time.Second

	// logNow is the clock for the lookback bound; tests replace it.
	logNow = time.Now
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

// GetLogs retrieves the newest log lines (at most opts.Lines when set) for
// a Cloud Run Instance, in chronological order. Uses the Cloud Logging API
// filtering by resource labels, within logLookback.
//
// The read runs under logQueryTimeout. If that runs out after some entries
// were read, GetLogs returns them: the query is newest first, so they are
// the latest tail. If it runs out before any entry was read, GetLogs
// returns an error saying so.
func (c *LogClient) GetLogs(ctx context.Context, instanceName string, opts LogOptions) ([]LogEntry, error) {
	queryCtx, cancel := context.WithTimeout(ctx, logQueryTimeout)
	defer cancel()

	req := &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{"projects/" + c.projectID},
		Filter:        cloudRunInstanceLogFilter(instanceName, c.projectID, time.Time{}, logNow().Add(-logLookback)),
		OrderBy:       "timestamp desc",
	}
	if opts.Lines > 0 {
		req.PageSize = int32(opts.Lines)
	}

	it := c.client.ListLogEntries(queryCtx, req)
	var entries []LogEntry
	for {
		resp, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			// Only this read's own deadline; a cancelled or expired
			// caller context stays an error.
			if ctx.Err() == nil && errors.Is(queryCtx.Err(), context.DeadlineExceeded) {
				if len(entries) > 0 {
					// The tail is cut short by time, not by Lines; the
					// response cannot say so, the log does.
					slog.Warn("Cloud Run log read stopped at its deadline; returning the newest entries read",
						"instance", instanceName, "entries", len(entries), "deadline", logQueryTimeout.String())
					break
				}
				return nil, fmt.Errorf("listing log entries: the read for instance %s returned no entries before its %s deadline (searching the last %s)",
					instanceName, logQueryTimeout, logLookback)
			}
			if status.Code(err) == codes.PermissionDenied {
				return nil, fmt.Errorf("listing log entries: permission denied: reading Cloud Logging needs the logging.logEntries.list permission: %w", err)
			}
			return nil, fmt.Errorf("listing log entries: %w", err)
		}

		entries = append(entries, logEntryFromResponse(resp))

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

// StreamLogs tails log output in real time.
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
	// Until an entry has been seen, read only within logLookback, as
	// GetLogs does; afterwards lastSeen is the bound.
	var since time.Time
	if lastSeen.IsZero() {
		since = logNow().Add(-logLookback)
	}
	req := &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{"projects/" + c.projectID},
		Filter:        cloudRunInstanceLogFilter(instanceName, c.projectID, *lastSeen, since),
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

// cloudRunInstanceLogFilter selects an instance's entries, after lastSeen
// (exclusive) and from since (inclusive) when either is set.
func cloudRunInstanceLogFilter(instanceName string, projectID string, lastSeen, since time.Time) string {
	filter := fmt.Sprintf(
		`resource.type="cloud_run_instance" AND resource.labels.instance_name=%s AND resource.labels.project_id=%s`,
		logFilterString(instanceName),
		logFilterString(projectID),
	)
	if !lastSeen.IsZero() {
		filter += fmt.Sprintf(` AND timestamp > %s`, logFilterString(lastSeen.UTC().Format(time.RFC3339Nano)))
	}
	if !since.IsZero() {
		filter += fmt.Sprintf(` AND timestamp >= %s`, logFilterString(since.UTC().Format(time.RFC3339Nano)))
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
