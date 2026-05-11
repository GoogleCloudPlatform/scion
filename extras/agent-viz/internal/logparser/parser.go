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

package logparser

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// deleteAgentURLPattern matches DELETE /agents/<name> or /v1/agents/<name> etc.
var deleteAgentURLPattern = regexp.MustCompile(`/agents/([^/?]+)(?:\?.*)?$`)

// GCPLogEntry represents a single log entry from Google Cloud Logging export.
type GCPLogEntry struct {
	InsertID    string            `json:"insertId"`
	JSONPayload map[string]any    `json:"jsonPayload"`
	Timestamp   string            `json:"timestamp"`
	Severity    string            `json:"severity"`
	Labels      map[string]string `json:"labels"`
	LogName     string            `json:"logName"`
	HTTPRequest *HTTPRequestField `json:"httpRequest,omitempty"`
}

// HTTPRequestField represents the httpRequest field in GCP log entries.
type HTTPRequestField struct {
	RequestMethod string `json:"requestMethod"`
	RequestURL    string `json:"requestUrl"`
}

// deleteAgentRequest captures a DELETE /agents/<name> request from logs.
type deleteAgentRequest struct {
	agentName string
	timestamp string
}

// PlaybackManifest is sent once at connection start.
type PlaybackManifest struct {
	Type      string      `json:"type"`
	TimeRange TimeRange   `json:"timeRange"`
	Agents    []AgentInfo `json:"agents"`
	Files     []FileNode  `json:"files"`
	GroveID   string      `json:"groveId"`
	GroveName string      `json:"groveName"`
	MaxDepth  int         `json:"maxDepth"`
}

type TimeRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type AgentInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Harness string `json:"harness"`
	Color   string `json:"color"`
}

type FileNode struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Parent string `json:"parent"`
	IsDir  bool   `json:"isDir"`
}

// PlaybackEvent is streamed during playback.
type PlaybackEvent struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Data      any    `json:"data"`
}

type AgentStateEvent struct {
	AgentID  string `json:"agentId"`
	Phase    string `json:"phase,omitempty"`
	Activity string `json:"activity,omitempty"`
	ToolName string `json:"toolName,omitempty"`
}

type MessageEvent struct {
	Sender      string `json:"sender"`
	Recipient   string `json:"recipient"`
	MsgType     string `json:"msgType"`
	Content     string `json:"content,omitempty"`
	Broadcasted bool   `json:"broadcasted"`
}

type FileEditEvent struct {
	AgentID  string `json:"agentId"`
	FilePath string `json:"filePath"`
	Action   string `json:"action"`
}

type AgentLifecycleEvent struct {
	AgentID     string `json:"agentId"`
	Name        string `json:"name"`
	Action      string `json:"action"`
	RequestedBy string `json:"requestedBy,omitempty"` // agent name that requested the destroy
}

// Agent colors assigned in order.
var agentColors = []string{
	"#4e79a7", "#f28e2b", "#e15759", "#76b7b2",
	"#59a14f", "#edc948", "#b07aa1", "#ff9da7",
	"#9c755f", "#bab0ac",
}

// ParseResult contains all parsed data from log files.
type ParseResult struct {
	Manifest PlaybackManifest
	Events   []PlaybackEvent
}

// fsWatcherEvent mirrors the NDJSON output of the fs-watcher-tool.
type fsWatcherEvent struct {
	Timestamp time.Time `json:"ts"`
	AgentID   string    `json:"agent_id"`
	Action    string    `json:"action"`
	Path      string    `json:"path"`
	Size      *int64    `json:"size,omitempty"`
}

// ParseLogFile reads a GCP log JSON export and returns the manifest and events.
// If fsLogPath is non-empty, filesystem events are sourced from that NDJSON log
// instead of from tool calls in the primary log.
func ParseLogFile(path string, fsLogPath string, maxDepth int) (*ParseResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading log file: %w", err)
	}

	var entries []GCPLogEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parsing log JSON: %w", err)
	}

	// Sort entries by timestamp
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Timestamp < entries[j].Timestamp
	})

	useFSLog := fsLogPath != ""

	agents := extractAgents(entries)
	events := extractEventsOpt(entries, agents, useFSLog)
	timeRange := extractTimeRange(entries)

	if useFSLog {
		fsEvents, err := parseFSLog(fsLogPath)
		if err != nil {
			return nil, fmt.Errorf("parsing fs log: %w", err)
		}
		events = append(events, fsEvents...)
		// Extend time range to cover fs log events
		if len(fsEvents) > 0 {
			if fsEvents[0].Timestamp < timeRange.Start || timeRange.Start == "" {
				timeRange.Start = fsEvents[0].Timestamp
			}
			last := fsEvents[len(fsEvents)-1].Timestamp
			if last > timeRange.End || timeRange.End == "" {
				timeRange.End = last
			}
		}
		// Re-sort after merging
		sort.Slice(events, func(i, j int) bool {
			return events[i].Timestamp < events[j].Timestamp
		})
	}

	// Determine grove info
	groveID, groveName := extractGroveInfo(entries)

	manifest := PlaybackManifest{
		Type:      "manifest",
		TimeRange: timeRange,
		Agents:    agents,
		Files:     []FileNode{}, // Files are added dynamically via events
		GroveID:   groveID,
		GroveName: groveName,
		MaxDepth:  maxDepth,
	}

	return &ParseResult{
		Manifest: manifest,
		Events:   events,
	}, nil
}

// parseFSLog reads a fs-watcher NDJSON log and converts events to PlaybackEvents.
func parseFSLog(path string) ([]PlaybackEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening fs log: %w", err)
	}
	defer f.Close()

	var events []PlaybackEvent
	scanner := bufio.NewScanner(f)
	// Increase buffer for potentially long lines
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var fsEvt fsWatcherEvent
		if err := json.Unmarshal(line, &fsEvt); err != nil {
			continue // skip malformed lines
		}
		ts := fsEvt.Timestamp.UTC().Format(time.RFC3339Nano)
		action := mapFSAction(fsEvt.Action)
		eventType := "file_edit"
		if action == "delete" {
			eventType = "file_edit"
		}
		events = append(events, PlaybackEvent{
			Type:      eventType,
			Timestamp: ts,
			Data: FileEditEvent{
				AgentID:  fsEvt.AgentID,
				FilePath: fsEvt.Path,
				Action:   action,
			},
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading fs log: %w", err)
	}

	return events, nil
}

// mapFSAction maps fs-watcher action strings to agent-viz file event actions.
func mapFSAction(action string) string {
	switch action {
	case "create":
		return "create"
	case "modify":
		return "edit"
	case "delete":
		return "delete"
	case "rename_to":
		return "create"
	case "rename_from":
		return "delete"
	default:
		return "edit"
	}
}

func extractTimeRange(entries []GCPLogEntry) TimeRange {
	if len(entries) == 0 {
		return TimeRange{}
	}
	return TimeRange{
		Start: entries[0].Timestamp,
		End:   entries[len(entries)-1].Timestamp,
	}
}

func extractProjectInfo(entries []GCPLogEntry) (string, string) {
	for _, e := range entries {
		if gid, ok := e.Labels["grove_id"]; ok {
			// Try to find grove name from server logs
			name := gid
			if slug, ok := e.JSONPayload["slug"]; ok {
				if s, ok := slug.(string); ok && !strings.Contains(s, ":") {
					name = s
				}
			}
			return gid, name
		}
	}
	return "", ""
}

func extractAgents(entries []GCPLogEntry) []AgentInfo {
	agentMap := make(map[string]*AgentInfo)
	nameMap := make(map[string]string) // id -> name
	// Track which IDs had explicit lifecycle events (pre_start)
	hasLifecycle := make(map[string]bool)

	// First pass: find agent names and IDs from server "Agent created" logs and message events.
	for _, e := range entries {
		logName := logBaseName(e.LogName)
		jp := e.JSONPayload

		// scion-server "Agent created" logs carry name, slug, and agent_id
		if logName == "scion-server" && getStr(jp, "message") == "Agent created" {
			aid := getStr(jp, "agent_id")
			if aid == "" {
				aid = e.Labels["agent_id"]
			}
			name := getStr(jp, "slug")
			if name == "" {
				name = getStr(jp, "name")
			}
			if aid != "" && name != "" {
				nameMap[aid] = name
			}
		}

		if logName == "scion-messages" {
			for _, field := range []string{"sender", "recipient"} {
				val := getStr(jp, field)
				if val == "" {
					val = e.Labels[field]
				}
				idField := field + "_id"
				aid := getStr(jp, idField)
				if aid == "" {
					aid = e.Labels[idField]
				}
				if strings.HasPrefix(val, "agent:") {
					name := strings.TrimPrefix(val, "agent:")
					if aid != "" {
						nameMap[aid] = name
					}
					// When no UUID is available, skip creating a slug-based entry;
					// the agent should already be known by UUID from server or agent logs.
					// Adding slug-as-ID entries creates duplicates.
				}
			}
			// Also check agent_name / agent_id fields in message payloads
			if an := getStr(jp, "agent_name"); an != "" {
				if aid := getStr(jp, "agent_id"); aid != "" {
					nameMap[aid] = an
				}
			}
		}
	}

	// Second pass: collect agents from scion-agents logs (these have UUIDs and harness info)
	for _, e := range entries {
		logName := logBaseName(e.LogName)
		if logName != "scion-agents" {
			continue
		}
		aid := e.Labels["agent_id"]
		if aid == "" {
			continue
		}
		if _, exists := agentMap[aid]; !exists {
			harness := e.Labels["scion.harness"]
			name := nameMap[aid]
			if name == "" && len(aid) >= 8 {
				name = aid[:8]
			} else if name == "" {
				name = aid
			}
			agentMap[aid] = &AgentInfo{
				ID:      aid,
				Name:    name,
				Harness: harness,
			}
		}
		msg := getStr(e.JSONPayload, "message")
		if msg == "agent.lifecycle.pre_start" {
			hasLifecycle[aid] = true
		}
	}

	// Third pass: backfill agents discovered only from messages (no scion-agents entries).
	// These are agents that existed before the log window or whose agent logs weren't captured.
	// Build a set of known agent names to avoid creating duplicates with different IDs.
	knownNames := make(map[string]bool)
	for _, a := range agentMap {
		knownNames[a.Name] = true
	}
	for id, name := range nameMap {
		if _, exists := agentMap[id]; !exists {
			// Skip if an agent with this name already exists under a different (UUID) ID
			if knownNames[name] {
				continue
			}
			agentMap[id] = &AgentInfo{
				ID:      id,
				Name:    name,
				Harness: "unknown",
			}
			knownNames[name] = true
		}
	}

	// Assign colors after sorting for deterministic output
	agents := make([]AgentInfo, 0, len(agentMap))
	for _, a := range agentMap {
		agents = append(agents, *a)
	}

	sort.Slice(agents, func(i, j int) bool {
		return agents[i].Name < agents[j].Name
	})

	for i := range agents {
		agents[i].Color = agentColors[i%len(agentColors)]
	}

	return agents
}

// agentsWithLifecycle returns the set of agent IDs that had explicit lifecycle events.
func agentsWithLifecycle(entries []GCPLogEntry) map[string]bool {
	has := make(map[string]bool)
	for _, e := range entries {
		if logBaseName(e.LogName) != "scion-agents" {
			continue
		}
		msg := getStr(e.JSONPayload, "message")
		if msg == "agent.lifecycle.pre_start" {
			has[e.Labels["agent_id"]] = true
		}
	}
	return has
}

func extractFiles(entries []GCPLogEntry) []FileNode {
	filePaths := make(map[string]bool)

	for _, e := range entries {
		logName := logBaseName(e.LogName)
		if logName != "scion-agents" {
			continue
		}
		jp := e.JSONPayload
		// Any tool event with a file_path contributes to the file tree
		fp := extractFilePath(jp)
		if fp != "" {
			filePaths[fp] = true
		}
	}

	// Build file tree nodes from discovered paths
	nodes := make(map[string]*FileNode)

	// Always add workspace root as the tree anchor
	if len(filePaths) > 0 {
		nodes["."] = &FileNode{
			ID:    ".",
			Name:  "/workspace",
			IsDir: true,
		}
	}

	for fp := range filePaths {
		addFileToTree(nodes, fp)
	}

	result := make([]FileNode, 0, len(nodes))
	for _, n := range nodes {
		result = append(result, *n)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})

	return result
}

// extractFilePath tries to find a file path from a tool call's JSON payload.
func extractFilePath(jp map[string]any) string {
	for _, key := range []string{"file_path", "path", "filePath", "filename"} {
		fp := getStr(jp, key)
		if fp != "" {
			return normalizeFilePath(fp)
		}
	}
	return ""
}

// normalizeFilePath strips workspace prefixes and relative path markers.
func normalizeFilePath(fp string) string {
	fp = strings.TrimPrefix(fp, "/workspace/")
	fp = strings.TrimPrefix(fp, "./")
	return fp
}

func addFileToTree(nodes map[string]*FileNode, fp string) {
	// Add file node
	if _, exists := nodes[fp]; !exists {
		nodes[fp] = &FileNode{
			ID:     fp,
			Name:   filepath.Base(fp),
			Parent: filepath.Dir(fp),
			IsDir:  false,
		}
	}

	// Add parent directories
	dir := filepath.Dir(fp)
	for dir != "." && dir != "" {
		if _, exists := nodes[dir]; !exists {
			nodes[dir] = &FileNode{
				ID:     dir,
				Name:   filepath.Base(dir),
				Parent: filepath.Dir(dir),
				IsDir:  true,
			}
		}
		dir = filepath.Dir(dir)
	}
}

func extractEvents(entries []GCPLogEntry, agents []AgentInfo) []PlaybackEvent {
	return extractEventsOpt(entries, agents, false)
}

// extractEventsOpt extracts events from log entries. When skipFileEvents is true,
// file_edit and file_read events from tool calls are suppressed (used when an
// external fs-watcher log provides file events instead).
func extractEventsOpt(entries []GCPLogEntry, agents []AgentInfo, skipFileEvents bool) []PlaybackEvent {
	var events []PlaybackEvent

	agentNameByID := make(map[string]string)
	agentIDByName := make(map[string]string) // reverse lookup: name -> first known UUID
	for _, a := range agents {
		agentNameByID[a.ID] = a.Name
		if _, exists := agentIDByName[a.Name]; !exists {
			agentIDByName[a.Name] = a.ID
		}
	}

	// Track which agents had explicit lifecycle (pre_start) events
	hasLifecycle := agentsWithLifecycle(entries)
	// Track which agents we've already emitted a backfill create event for
	backfilled := make(map[string]bool)

	// Helper: ensure an agent has a create event. For agents without explicit lifecycle
	// events, we emit a synthetic agent_create at the timestamp of their first appearance.
	ensureAgent := func(agentID, ts string) {
		if hasLifecycle[agentID] || backfilled[agentID] {
			return
		}
		backfilled[agentID] = true
		events = append(events, PlaybackEvent{
			Type:      "agent_create",
			Timestamp: ts,
			Data: AgentLifecycleEvent{
				AgentID: agentID,
				Name:    agentNameByID[agentID],
				Action:  "create",
			},
		})
		events = append(events, PlaybackEvent{
			Type:      "agent_state",
			Timestamp: ts,
			Data: AgentStateEvent{
				AgentID:  agentID,
				Phase:    "running",
				Activity: "idle",
			},
		})
	}

	for _, e := range entries {
		logName := logBaseName(e.LogName)
		jp := e.JSONPayload
		ts := e.Timestamp

		switch logName {
		case "scion-agents":
			msg := getStr(jp, "message")
			aid := e.Labels["agent_id"]

			// Backfill agent if first appearance and no lifecycle event
			if aid != "" {
				ensureAgent(aid, ts)
			}

			switch msg {
			case "agent.session.start":
				events = append(events, PlaybackEvent{
					Type:      "agent_state",
					Timestamp: ts,
					Data: AgentStateEvent{
						AgentID:  aid,
						Phase:    "running",
						Activity: "idle",
					},
				})
			case "agent.session.end":
				events = append(events, PlaybackEvent{
					Type:      "agent_state",
					Timestamp: ts,
					Data: AgentStateEvent{
						AgentID:  aid,
						Phase:    "stopped",
						Activity: "completed",
					},
				})
			case "agent.turn.start":
				events = append(events, PlaybackEvent{
					Type:      "agent_state",
					Timestamp: ts,
					Data: AgentStateEvent{
						AgentID:  aid,
						Activity: "thinking",
					},
				})
			case "agent.turn.end":
				events = append(events, PlaybackEvent{
					Type:      "agent_state",
					Timestamp: ts,
					Data: AgentStateEvent{
						AgentID:  aid,
						Activity: "idle",
					},
				})
			case "agent.tool.call":
				toolName := getStr(jp, "tool_name")
				events = append(events, PlaybackEvent{
					Type:      "agent_state",
					Timestamp: ts,
					Data: AgentStateEvent{
						AgentID:  aid,
						Activity: "executing",
						ToolName: toolName,
					},
				})
				// Generate file events for tools that interact with files.
				// When fs-watcher log is active, write/edit events are suppressed
				// (the fs-watcher provides more accurate data for those).
				fp := extractFilePath(jp)
				if fp != "" {
					if !skipFileEvents && isFileEditTool(toolName) {
						action := "edit"
						if toolName == "write_file" || toolName == "create_file" || toolName == "Write" {
							action = "create"
						}
						events = append(events, PlaybackEvent{
							Type:      "file_edit",
							Timestamp: ts,
							Data: FileEditEvent{
								AgentID:  aid,
								FilePath: fp,
								Action:   action,
							},
						})
					} else if isFileReadTool(toolName) {
						// TODO: Remove this special case when fs-watcher supports read events.
						// Currently fs-watcher only captures writes, so file_read events
						// are always sourced from the primary log regardless of --fs-log.
						events = append(events, PlaybackEvent{
							Type:      "file_read",
							Timestamp: ts,
							Data: FileEditEvent{
								AgentID:  aid,
								FilePath: fp,
								Action:   "read",
							},
						})
					}
				}
			case "agent.tool.result":
				events = append(events, PlaybackEvent{
					Type:      "agent_state",
					Timestamp: ts,
					Data: AgentStateEvent{
						AgentID:  aid,
						Activity: "thinking",
					},
				})
			case "agent.lifecycle.pre_start":
				events = append(events, PlaybackEvent{
					Type:      "agent_create",
					Timestamp: ts,
					Data: AgentLifecycleEvent{
						AgentID: aid,
						Name:    agentNameByID[aid],
						Action:  "create",
					},
				})
				events = append(events, PlaybackEvent{
					Type:      "agent_state",
					Timestamp: ts,
					Data: AgentStateEvent{
						AgentID: aid,
						Phase:   "starting",
					},
				})
			case "agent.lifecycle.post_start":
				events = append(events, PlaybackEvent{
					Type:      "agent_state",
					Timestamp: ts,
					Data: AgentStateEvent{
						AgentID: aid,
						Phase:   "running",
					},
				})
			case "agent.lifecycle.pre_stop":
				events = append(events, PlaybackEvent{
					Type:      "agent_state",
					Timestamp: ts,
					Data: AgentStateEvent{
						AgentID: aid,
						Phase:   "stopping",
					},
				})
				events = append(events, PlaybackEvent{
					Type:      "agent_destroy",
					Timestamp: ts,
					Data: AgentLifecycleEvent{
						AgentID: aid,
						Name:    agentNameByID[aid],
						Action:  "destroy",
					},
				})
			case "Tool requires confirmation":
				events = append(events, PlaybackEvent{
					Type:      "agent_state",
					Timestamp: ts,
					Data: AgentStateEvent{
						AgentID:  aid,
						Activity: "waiting_for_input",
					},
				})
			}

		case "scion-messages":
			// Skip rejected messages (failed deliveries)
			msgAction := getStr(jp, "message")
			if strings.Contains(msgAction, "rejected") {
				continue
			}

			sender := getStr(jp, "sender")
			if sender == "" {
				sender = e.Labels["sender"]
			}
			recipient := getStr(jp, "recipient")
			if recipient == "" {
				recipient = e.Labels["recipient"]
			}
			msgType := getStr(jp, "msg_type")
			if msgType == "" {
				msgType = e.Labels["msg_type"]
			}
			content := getStr(jp, "message_content")
			broadcasted := getBool(jp, "broadcasted")

			if sender != "" && recipient != "" {
				senderName := strings.TrimPrefix(sender, "agent:")
				recipientName := strings.TrimPrefix(recipient, "agent:")

				// Backfill agents referenced in messages
				senderID := getStr(jp, "sender_id")
				if senderID == "" {
					senderID = e.Labels["sender_id"]
				}
				if senderID == "" && strings.HasPrefix(sender, "agent:") {
					// Resolve slug name to known UUID; fall back to slug only if unknown
					if uid, ok := agentIDByName[senderName]; ok {
						senderID = uid
					} else {
						senderID = senderName
					}
				}
				recipientID := getStr(jp, "recipient_id")
				if recipientID == "" {
					recipientID = e.Labels["recipient_id"]
				}
				if recipientID == "" && strings.HasPrefix(recipient, "agent:") {
					if uid, ok := agentIDByName[recipientName]; ok {
						recipientID = uid
					} else {
						recipientID = recipientName
					}
				}

				if senderID != "" && strings.HasPrefix(sender, "agent:") {
					ensureAgent(senderID, ts)
				}
				if recipientID != "" && strings.HasPrefix(recipient, "agent:") {
					ensureAgent(recipientID, ts)
				}

				// Resolve UUID-based names to friendly names
				// e.g., "agent:a35ea791-..." should become "orchestrator"
				if agentNameByID[senderName] != "" {
					senderName = agentNameByID[senderName]
				} else if senderID != "" && agentNameByID[senderID] != "" {
					senderName = agentNameByID[senderID]
				}
				if agentNameByID[recipientName] != "" {
					recipientName = agentNameByID[recipientName]
				} else if recipientID != "" && agentNameByID[recipientID] != "" {
					recipientName = agentNameByID[recipientID]
				}

				events = append(events, PlaybackEvent{
					Type:      "message",
					Timestamp: ts,
					Data: MessageEvent{
						Sender:      senderName,
						Recipient:   recipientName,
						MsgType:     msgType,
						Content:     content,
						Broadcasted: broadcasted,
					},
				})
			}
		}
	}

	// Collect DELETE agent requests from scion_request_log entries
	var deleteRequests []deleteAgentRequest
	for _, e := range entries {
		logName := logBaseName(e.LogName)
		if !strings.HasSuffix(logName, "scion_request_log") {
			continue
		}
		if e.HTTPRequest == nil || e.HTTPRequest.RequestMethod != "DELETE" {
			continue
		}
		matches := deleteAgentURLPattern.FindStringSubmatch(e.HTTPRequest.RequestURL)
		if len(matches) < 2 {
			continue
		}
		agentSlug := matches[1]
		deleteRequests = append(deleteRequests, deleteAgentRequest{
			agentName: agentSlug,
			timestamp: e.Timestamp,
		})
	}

	// Post-processing: enrich agent_create events with requestedBy
	// For each agent_create, look backward for the nearest Bash tool-start from another agent within 15s.
	for i, evt := range events {
		if evt.Type != "agent_create" {
			continue
		}
		lifecycle, ok := evt.Data.(AgentLifecycleEvent)
		if !ok || lifecycle.RequestedBy != "" {
			continue
		}
		createTime, err := TimestampToTime(evt.Timestamp)
		if err != nil {
			continue
		}

		for j := i - 1; j >= 0; j-- {
			prev := events[j]
			if prev.Type != "agent_state" {
				continue
			}
			stateEvt, ok := prev.Data.(AgentStateEvent)
			if !ok || stateEvt.Activity != "executing" || !isShellTool(stateEvt.ToolName) {
				continue
			}
			evtTime, err := TimestampToTime(prev.Timestamp)
			if err != nil {
				continue
			}
			delta := createTime.Sub(evtTime)
			if delta < 0 || delta > 15*time.Second {
				continue
			}
			// Must be a different agent
			if stateEvt.AgentID == lifecycle.AgentID {
				continue
			}
			if name, ok := agentNameByID[stateEvt.AgentID]; ok {
				lifecycle.RequestedBy = name
			} else {
				lifecycle.RequestedBy = stateEvt.AgentID
			}
			events[i].Data = lifecycle
			break
		}
	}

	// Post-processing: enrich agent_destroy events with requestedBy info
	for _, delReq := range deleteRequests {
		delTime, err := TimestampToTime(delReq.timestamp)
		if err != nil {
			continue
		}

		// Find the nearest agent_destroy event for this agent name (within 30 seconds)
		bestDestroyIdx := -1
		bestDestroyDelta := time.Duration(math.MaxInt64)
		for i, evt := range events {
			if evt.Type != "agent_destroy" {
				continue
			}
			lifecycle, ok := evt.Data.(AgentLifecycleEvent)
			if !ok {
				continue
			}
			if lifecycle.Name != delReq.agentName {
				continue
			}
			evtTime, err := TimestampToTime(evt.Timestamp)
			if err != nil {
				continue
			}
			delta := evtTime.Sub(delTime)
			if delta < 0 {
				delta = -delta
			}
			if delta <= 30*time.Second && delta < bestDestroyDelta {
				bestDestroyDelta = delta
				bestDestroyIdx = i
			}
		}
		if bestDestroyIdx < 0 {
			continue
		}

		// Find the nearest Bash tool-start from another agent within 15s before the DELETE request
		var requestingAgent string
		for i := len(events) - 1; i >= 0; i-- {
			evt := events[i]
			if evt.Type != "agent_state" {
				continue
			}
			stateEvt, ok := evt.Data.(AgentStateEvent)
			if !ok {
				continue
			}
			if stateEvt.Activity != "executing" || !isShellTool(stateEvt.ToolName) {
				continue
			}
			evtTime, err := TimestampToTime(evt.Timestamp)
			if err != nil {
				continue
			}
			delta := delTime.Sub(evtTime)
			if delta < 0 || delta > 15*time.Second {
				continue
			}
			// Must be a different agent than the one being destroyed
			destroyEvt := events[bestDestroyIdx].Data.(AgentLifecycleEvent)
			if stateEvt.AgentID == destroyEvt.AgentID {
				continue
			}
			if name, ok := agentNameByID[stateEvt.AgentID]; ok {
				requestingAgent = name
			} else {
				requestingAgent = stateEvt.AgentID
			}
			break
		}

		if requestingAgent != "" {
			lifecycle := events[bestDestroyIdx].Data.(AgentLifecycleEvent)
			lifecycle.RequestedBy = requestingAgent
			events[bestDestroyIdx].Data = lifecycle
		}
	}

	// Sort by timestamp
	sort.Slice(events, func(i, j int) bool {
		return events[i].Timestamp < events[j].Timestamp
	})

	return events
}

func isFileEditTool(name string) bool {
	switch name {
	case "write_file", "create_file", "Write", "edit_file", "Edit", "patch_file":
		return true
	}
	return false
}

func isShellTool(name string) bool {
	switch name {
	case "Bash", "run_shell_command":
		return true
	}
	return false
}

func isFileReadTool(name string) bool {
	switch name {
	case "read_file", "Read", "Grep", "Glob":
		return true
	}
	return false
}

func logBaseName(logName string) string {
	parts := strings.Split(logName, "/")
	return parts[len(parts)-1]
}

func getStr(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func getBool(m map[string]any, key string) bool {
	v, ok := m[key]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	if !ok {
		return false
	}
	return b
}

// TimestampToTime parses an ISO 8601 timestamp.
func TimestampToTime(ts string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, ts)
}
