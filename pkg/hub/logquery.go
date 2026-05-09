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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"

	gcplog "cloud.google.com/go/logging"
	logv2 "cloud.google.com/go/logging/apiv2"
	loggingpb "cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/logging/logadmin"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
)

// LogQueryService queries Google Cloud Logging for structured log entries.
type LogQueryService struct {
	client     *logadmin.Client
	tailClient *logv2.Client
	projectID  string
}

// CloudLogEntry represents a structured log entry from Cloud Logging.
type CloudLogEntry struct {
	Timestamp      time.Time              `json:"timestamp"`
	Severity       string                 `json:"severity"`
	Message        string                 `json:"message"`
	Labels         map[string]string      `json:"labels,omitempty"`
	Resource       map[string]interface{} `json:"resource,omitempty"`
	JSONPayload    map[string]interface{} `json:"jsonPayload,omitempty"`
	InsertID       string                 `json:"insertId"`
	SourceLocation *LogSourceLocation     `json:"sourceLocation,omitempty"`
}

// LogSourceLocation identifies the source code location of a log entry.
type LogSourceLocation struct {
	File     string `json:"file,omitempty"`
	Line     string `json:"line,omitempty"`
	Function string `json:"function,omitempty"`
}

// LogQueryOptions configures a Cloud Logging query.
type LogQueryOptions struct {
	AgentID   string
	ProjectID   string
	BrokerID  string
	LogID     string // Cloud Logging log ID (e.g. "scion-messages"); empty = default log
	Tail      int
	Since     time.Time
	Until     time.Time
	Severity  string
	PageToken string
}

// LogQueryResult contains the result of a log query.
type LogQueryResult struct {
	Entries       []CloudLogEntry `json:"entries"`
	NextPageToken string          `json:"nextPageToken,omitempty"`
	HasMore       bool            `json:"hasMore"`
}

// NewLogQueryService creates a LogQueryService. Returns an error if the
// logadmin client cannot be created.
func NewLogQueryService(ctx context.Context, projectID string) (*LogQueryService, error) {
	if projectID == "" {
		return nil, fmt.Errorf("GCP project ID is required")
	}

	client, err := logadmin.NewClient(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("creating logadmin client: %w", err)
	}

	// Create the apiv2 client for TailLogEntries streaming.
	tailClient, err := logv2.NewClient(ctx)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("creating logging apiv2 client: %w", err)
	}

	return &LogQueryService{
		client:     client,
		tailClient: tailClient,
		projectID:  projectID,
	}, nil
}

// Close releases resources held by the LogQueryService.
func (s *LogQueryService) Close() error {
	var firstErr error
	if err := s.tailClient.Close(); err != nil {
		firstErr = err
	}
	if err := s.client.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// Query returns log entries matching the given options.
func (s *LogQueryService) Query(ctx context.Context, opts LogQueryOptions) (*LogQueryResult, error) {
	filter := BuildLogFilter(opts, s.projectID)

	tail := opts.Tail
	if tail <= 0 {
		tail = 200
	}
	if tail > 1000 {
		tail = 1000
	}

	slog.Debug("querying cloud logs", "filter", filter, "tail", tail)

	it := s.client.Entries(ctx,
		logadmin.Filter(filter),
		logadmin.NewestFirst(),
	)

	var entries []CloudLogEntry
	for len(entries) < tail {
		entry, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading log entries: %w", err)
		}
		entries = append(entries, ConvertLogEntry(entry))
	}

	return &LogQueryResult{
		Entries: entries,
	}, nil
}

// BuildLogFilter constructs a Cloud Logging filter string from the options.
// When projectID is provided and opts.LogID is set, a logName filter is added
// to restrict results to the specific Cloud Logging log.
func BuildLogFilter(opts LogQueryOptions, projectID ...string) string {
	var parts []string

	if opts.LogID != "" && len(projectID) > 0 && projectID[0] != "" {
		parts = append(parts, fmt.Sprintf(`logName = "projects/%s/logs/%s"`, projectID[0], opts.LogID))
	} else if len(projectID) > 0 && projectID[0] != "" {
		// Exclude request logs from general log queries — they are high-volume
		// server infrastructure logs that are not relevant to agent activity.
		parts = append(parts, fmt.Sprintf(`logName != "projects/%s/logs/%s"`, projectID[0], logging.RequestLogID))
	}
	if opts.AgentID != "" && opts.LogID == logging.MessageLogID {
		// For message logs, match where this agent is either the recipient
		// (recipient_id) or the sender (sender_id). Uses unique IDs rather
		// than slugs to prevent cross-agent leakage when agents share a name.
		parts = append(parts, fmt.Sprintf(
			`(labels.recipient_id = %q OR labels.sender_id = %q)`,
			opts.AgentID, opts.AgentID))
	} else if opts.AgentID != "" {
		parts = append(parts, fmt.Sprintf(`labels.agent_id = %q`, opts.AgentID))
	}
	if opts.ProjectID != "" {
		parts = append(parts, fmt.Sprintf(`labels.project_id = %q`, opts.ProjectID))
	}
	if opts.BrokerID != "" {
		parts = append(parts, fmt.Sprintf(`labels.broker_id = %q`, opts.BrokerID))
	}
	if !opts.Since.IsZero() {
		parts = append(parts, fmt.Sprintf(`timestamp >= %q`, opts.Since.Format(time.RFC3339Nano)))
	}
	if !opts.Until.IsZero() {
		parts = append(parts, fmt.Sprintf(`timestamp < %q`, opts.Until.Format(time.RFC3339Nano)))
	}
	if opts.Severity != "" {
		parts = append(parts, fmt.Sprintf(`severity >= %s`, strings.ToUpper(opts.Severity)))
	}

	return strings.Join(parts, " AND ")
}

// ConvertLogEntry converts a Cloud Logging Entry to a CloudLogEntry.
func ConvertLogEntry(entry *gcplog.Entry) CloudLogEntry {
	e := CloudLogEntry{
		Timestamp: entry.Timestamp,
		Severity:  entry.Severity.String(),
		Labels:    entry.Labels,
		InsertID:  entry.InsertID,
	}

	// Extract payload
	switch p := entry.Payload.(type) {
	case string:
		e.Message = p
	case map[string]interface{}:
		e.JSONPayload = p
		if msg, ok := p["message"].(string); ok {
			e.Message = msg
		}
	default:
		// For other types, try JSON marshaling to extract a map
		if p != nil {
			data, err := json.Marshal(p)
			if err == nil {
				payload := make(map[string]interface{})
				if json.Unmarshal(data, &payload) == nil {
					e.JSONPayload = payload
					if msg, ok := payload["message"].(string); ok {
						e.Message = msg
					}
				}
			}
		}
	}

	// Extract resource info
	if res := entry.Resource; res != nil {
		resMap := map[string]interface{}{
			"type": res.GetType(),
		}
		if labels := res.GetLabels(); len(labels) > 0 {
			labelsMap := make(map[string]interface{}, len(labels))
			for k, v := range labels {
				labelsMap[k] = v
			}
			resMap["labels"] = labelsMap
		}
		e.Resource = resMap
	}

	// Extract source location
	if sl := entry.SourceLocation; sl != nil {
		e.SourceLocation = &LogSourceLocation{
			File:     sl.GetFile(),
			Line:     fmt.Sprintf("%d", sl.GetLine()),
			Function: sl.GetFunction(),
		}
	}

	return e
}

// Tail opens a streaming session using the Cloud Logging TailLogEntries API.
// It sends matching log entries to the returned channel. The caller must call
// the returned cancel function to stop the stream and release resources.
func (s *LogQueryService) Tail(ctx context.Context, opts LogQueryOptions) (<-chan CloudLogEntry, func(), error) {
	filter := BuildLogFilter(opts, s.projectID)

	stream, err := s.tailClient.TailLogEntries(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("opening tail stream: %w", err)
	}

	req := &loggingpb.TailLogEntriesRequest{
		ResourceNames: []string{fmt.Sprintf("projects/%s", s.projectID)},
		Filter:        filter,
		BufferWindow:  durationpb.New(2 * time.Second),
	}
	if err := stream.Send(req); err != nil {
		stream.CloseSend()
		return nil, nil, fmt.Errorf("sending tail request: %w", err)
	}

	ch := make(chan CloudLogEntry, 64)
	cancel := func() {
		stream.CloseSend()
	}

	go func() {
		defer close(ch)
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				return
			}
			if err != nil {
				if ctx.Err() != nil {
					return // context cancelled
				}
				slog.Error("tail stream recv error", "error", err)
				return
			}
			for _, entry := range resp.GetEntries() {
				converted := ConvertProtoLogEntry(entry)
				select {
				case ch <- converted:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return ch, cancel, nil
}

// ConvertProtoLogEntry converts a proto LogEntry (from TailLogEntries) to a CloudLogEntry.
func ConvertProtoLogEntry(entry *loggingpb.LogEntry) CloudLogEntry {
	e := CloudLogEntry{
		Severity: entry.GetSeverity().String(),
		Labels:   entry.GetLabels(),
		InsertID: entry.GetInsertId(),
	}

	if ts := entry.GetTimestamp(); ts != nil {
		e.Timestamp = ts.AsTime()
	}

	// Extract payload
	switch p := entry.GetPayload().(type) {
	case *loggingpb.LogEntry_TextPayload:
		e.Message = p.TextPayload
	case *loggingpb.LogEntry_JsonPayload:
		// Convert structpb to map via JSON round-trip
		data, err := protojson.Marshal(p.JsonPayload)
		if err == nil {
			payload := make(map[string]interface{})
			if json.Unmarshal(data, &payload) == nil {
				e.JSONPayload = payload
				if msg, ok := payload["message"].(string); ok {
					e.Message = msg
				}
			}
		}
	}

	// Extract resource info
	if res := entry.GetResource(); res != nil {
		resMap := map[string]interface{}{
			"type": res.GetType(),
		}
		if labels := res.GetLabels(); len(labels) > 0 {
			labelsMap := make(map[string]interface{}, len(labels))
			for k, v := range labels {
				labelsMap[k] = v
			}
			resMap["labels"] = labelsMap
		}
		e.Resource = resMap
	}

	// Extract source location
	if sl := entry.GetSourceLocation(); sl != nil {
		e.SourceLocation = &LogSourceLocation{
			File:     sl.GetFile(),
			Line:     fmt.Sprintf("%d", sl.GetLine()),
			Function: sl.GetFunction(),
		}
	}

	return e
}
