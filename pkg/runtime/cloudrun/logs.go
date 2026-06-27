package cloudrun

import (
	"context"
	"fmt"
	"time"

	logging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/iterator"
)

type LogOptions struct {
	Lines int
}

type LogEntry struct {
	Timestamp time.Time
	Message   string
	Severity  string
}

type LogClient struct {
	client    *logging.Client
	projectID string
}

func NewLogClient(ctx context.Context, projectID string) (*LogClient, error) {
	client, err := logging.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating logging client: %w", err)
	}
	return &LogClient{
		client:    client,
		projectID: projectID,
	}, nil
}

func (c *LogClient) Close() error {
	return c.client.Close()
}

// GetLogs retrieves recent log lines for a Cloud Run Instance.
// Uses the Cloud Logging API filtering by resource labels.
func (c *LogClient) GetLogs(ctx context.Context, instanceName string, opts LogOptions) ([]LogEntry, error) {
	filter := fmt.Sprintf(`resource.type="cloud_run_instance" AND resource.labels.instance_name="%s" AND resource.labels.project_id="%s"`, instanceName, c.projectID)
	
	req := &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{"projects/" + c.projectID},
		Filter:        filter,
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
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				filter := fmt.Sprintf(`resource.type="cloud_run_instance" AND resource.labels.instance_name="%s" AND resource.labels.project_id="%s"`, instanceName, c.projectID)
				
				if !lastSeen.IsZero() {
					filter += fmt.Sprintf(` AND timestamp > "%s"`, lastSeen.Format(time.RFC3339Nano))
				}
				
				req := &loggingpb.ListLogEntriesRequest{
					ResourceNames: []string{"projects/" + c.projectID},
					Filter:        filter,
					OrderBy:       "timestamp asc",
				}
				
				it := c.client.ListLogEntries(ctx, req)
				for {
					resp, err := it.Next()
					if err == iterator.Done {
						break
					}
					if err != nil {
						// On transient error, break and retry next tick
						break
					}
					
					ts := resp.GetTimestamp().AsTime()
					if ts.After(lastSeen) {
						lastSeen = ts
					}
					
					msg := resp.GetTextPayload()
					if msg == "" && resp.GetJsonPayload() != nil {
						msg = resp.GetJsonPayload().String()
					}
					
					entry := LogEntry{
						Timestamp: ts,
						Message:   msg,
						Severity:  resp.GetSeverity().String(),
					}
					
					select {
					case ch <- entry:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	
	return ch, nil
}
