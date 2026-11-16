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

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

const defaultExecTimeoutSeconds = 60

// Schedule command flags
var (
	scheduleType      string
	scheduleStatus    string
	scheduleIn        string
	scheduleAt        string
	scheduleAgent     string
	scheduleMessage   string
	scheduleInterrupt bool
	scheduleWake      bool
	scheduleTimeout   string
	scheduleName      string
	scheduleCron      string
	scheduleTimeZone  string
	scheduleListType  string // "events", "recurring", "all"
)

// scheduleCmd is the top-level command group for schedule management.
var scheduleCmd = &cobra.Command{
	Use:   "schedule",
	Short: "Manage scheduled events",
	Long:  `List, inspect, create, and cancel scheduled events for the current project.`,
}

// scheduleListCmd lists scheduled events for the current project.
var scheduleListCmd = &cobra.Command{
	Use:   "list",
	Short: "List scheduled events",
	Long:  `List scheduled events and recurring schedules for the current project. When called as an agent, only schedules and events held by the calling agent are shown.`,
	RunE:  runScheduleList,
}

// scheduleGetCmd shows details of a specific scheduled event.
var scheduleGetCmd = &cobra.Command{
	Use:   "get <id>",
	Short: "Get details of a scheduled event",
	Args:  cobra.ExactArgs(1),
	RunE:  runScheduleGet,
}

// scheduleCancelCmd cancels a pending scheduled event.
var scheduleCancelCmd = &cobra.Command{
	Use:   "cancel <id>",
	Short: "Cancel a pending scheduled event",
	Args:  cobra.ExactArgs(1),
	RunE:  runScheduleCancel,
}

// scheduleCreateCmd creates a new one-shot scheduled event.
var scheduleCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a one-shot scheduled event",
	Long: `Create a one-shot scheduled event. Requires timing (--in or --at) and --agent.

For message events (default --type message), pass --message (and optionally --interrupt or --wake).
For exec events (--type exec), pass the command argv after '--' (and optionally --timeout, default 60s, max 60m).

Examples:
  # Send a message in 15 minutes
  scion schedule create --agent lead --in 15m --message "Recheck CI status"

  # Run a command directly in an agent container in 10 minutes
  scion schedule create --type exec --agent worker --in 10m --timeout 60s -- /usr/local/bin/check-queue`,
	Args: scheduleCreateArgs,
	RunE: runScheduleCreate,
}

// scheduleCreateRecurringCmd creates a new recurring schedule.
var scheduleCreateRecurringCmd = &cobra.Command{
	Use:   "create-recurring",
	Short: "Create a recurring schedule",
	Long: `Create a recurring schedule with a cron expression. Requires --name, --cron, and --agent.

For message schedules (default --type message), pass --message (and optionally --interrupt or --wake).
For exec schedules (--type exec), pass the command argv after '--' (and optionally --timeout, default 60s, max 60m).

The cron expression is evaluated in the zone given by --time-zone, an IANA
name such as America/Los_Angeles; without it the expression is evaluated in
UTC. A zoned schedule keeps its local time across daylight saving changes: a
local time skipped when the clocks go forward runs once at the change, and a
local time repeated when the clocks go back runs once, at its first
occurrence. Next run times are reported in the display zone (see --tz).
Zone prefixes in the expression (CRON_TZ=, TZ=) are not supported.

Examples:
  # 14:00 UTC on weekdays
  scion schedule create-recurring --name standup --cron "0 14 * * 1-5" --agent lead --message "standup"

  # 07:00 Pacific time every day, across daylight saving changes
  scion schedule create-recurring --name digest --cron "0 7 * * *" --time-zone America/Los_Angeles --agent lead --message "digest"

  # Run a command directly in an agent container every 15 minutes
  scion schedule create-recurring --name check-queue --cron "*/15 * * * *" --type exec --agent worker --timeout 60s -- /usr/local/bin/check-queue`,
	RunE: runScheduleCreateRecurring,
}

// schedulePauseCmd pauses an active recurring schedule.
var schedulePauseCmd = &cobra.Command{
	Use:   "pause <id>",
	Short: "Pause a recurring schedule",
	Args:  cobra.ExactArgs(1),
	RunE:  runSchedulePause,
}

// scheduleResumeCmd resumes a paused recurring schedule.
var scheduleResumeCmd = &cobra.Command{
	Use:   "resume <id>",
	Short: "Resume a paused recurring schedule",
	Args:  cobra.ExactArgs(1),
	RunE:  runScheduleResume,
}

// scheduleDeleteCmd deletes a recurring schedule.
var scheduleDeleteCmd = &cobra.Command{
	Use:   "delete <id>",
	Short: "Delete a recurring schedule",
	Args:  cobra.ExactArgs(1),
	RunE:  runScheduleDelete,
}

// scheduleHistoryCmd shows execution history for a schedule.
var scheduleHistoryCmd = &cobra.Command{
	Use:   "history [id]",
	Short: "View execution history for a schedule",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runScheduleHistory,
}

// isCallerScopedScheduleView reports whether the schedule view is filtered to
// rows held by the calling agent. It prefers the Hub's authoritative Scope
// marker on list responses and falls back to CredentialKindAgentToken when
// talking to an older Hub or when no list call ran.
func isCallerScopedScheduleView(hubCtx *HubContext, events *hubclient.ListScheduledEventsResponse, schedules *hubclient.ListSchedulesResponse) bool {
	if events != nil && events.Scope == hubclient.ScheduleListScopeCaller {
		return true
	}
	if schedules != nil && schedules.Scope == hubclient.ScheduleListScopeCaller {
		return true
	}
	return hubCtx != nil && hubCtx.CredentialKind == hubsync.CredentialKindAgentToken
}

// wrapScheduleHubError wraps a Hub client error and, when the caller uses an
// agent token against an older Hub that returned a generic 404/403/409 body,
// appends a content-free caller-scope hint without duplicating hints already
// supplied by newer Hubs.
func wrapScheduleHubError(hubCtx *HubContext, err error) error {
	if isCallerScopedScheduleView(hubCtx, nil, nil) {
		switch {
		case apiclient.IsConflictError(err) && !strings.Contains(err.Error(), "scion schedule list"):
			return wrapHubError(fmt.Errorf("%w (if this schedule name does not appear in 'scion schedule list', it is held by another principal and not visible to this agent)", err))
		case apiclient.IsNotFoundError(err) && !strings.Contains(err.Error(), "hidden from agent lists"):
			return wrapHubError(fmt.Errorf("%w (schedules and events held by another principal are hidden from agent lists and return 403 by ID)", err))
		case apiclient.IsForbiddenError(err) && strings.Contains(err.Error(), "Insufficient permissions"):
			return wrapHubError(fmt.Errorf("%w (agent callers can only access schedules and events they hold)", err))
		}
	}
	return wrapHubError(err)
}

// resolveScheduleID resolves a potentially truncated schedule/event ID to its full UUID.
// If the input is already a valid full UUID, it is returned as-is with an empty resourceType.
// Otherwise, it lists schedules and/or events and finds a unique prefix match.
// The targetType parameter controls which resources are searched:
//   - "" (empty) — search both events and schedules
//   - "event" — search only scheduled events
//   - "schedule" — search only recurring schedules
//
// Returns (fullID, resourceType, error) where resourceType is "schedule" or "event".
func resolveScheduleID(ctx context.Context, hubCtx *HubContext, projectID, prefix, targetType string) (string, string, error) {
	if prefix == "" {
		return "", "", fmt.Errorf("schedule/event ID is required")
	}

	if uuid.Validate(prefix) == nil {
		return prefix, "", nil
	}

	type match struct {
		id           string
		resourceType string
	}
	var (
		matches   []match
		evtResp   *hubclient.ListScheduledEventsResponse
		schedResp *hubclient.ListSchedulesResponse
	)

	// List one-shot events when targeting events or both
	if targetType == "" || targetType == "event" {
		var err error
		evtResp, err = hubCtx.Client.ScheduledEvents(projectID).List(ctx, nil)
		if err != nil {
			return "", "", fmt.Errorf("failed to list scheduled events for prefix resolution: %w", err)
		}
		for _, evt := range evtResp.Events {
			if strings.HasPrefix(evt.ID, prefix) {
				matches = append(matches, match{id: evt.ID, resourceType: "event"})
			}
		}
	}

	// List recurring schedules when targeting schedules or both
	if targetType == "" || targetType == "schedule" {
		var err error
		schedResp, err = hubCtx.Client.Schedules(projectID).List(ctx, nil)
		if err != nil {
			return "", "", fmt.Errorf("failed to list schedules for prefix resolution: %w", err)
		}
		for _, sched := range schedResp.Schedules {
			if strings.HasPrefix(sched.ID, prefix) {
				matches = append(matches, match{id: sched.ID, resourceType: "schedule"})
			}
		}
	}

	switch len(matches) {
	case 0:
		if isCallerScopedScheduleView(hubCtx, evtResp, schedResp) {
			resourceLabel := "schedule or event"
			switch targetType {
			case "schedule":
				resourceLabel = "recurring schedule"
			case "event":
				resourceLabel = "scheduled event"
			}
			return "", "", fmt.Errorf(
				"no %s held by this agent found with ID prefix %q (agent callers only see schedules and events they hold; schedules held by another principal are not visible, and commands take an ID or ID prefix rather than a schedule name)",
				resourceLabel, prefix)
		}
		return "", "", fmt.Errorf("no schedule or event found with ID prefix %q", prefix)
	case 1:
		return matches[0].id, matches[0].resourceType, nil
	default:
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.id
		}
		return "", "", fmt.Errorf("ambiguous ID prefix %q matches %d resources: %s", prefix, len(matches), strings.Join(ids, ", "))
	}
}

func scheduleHubContext() (*HubContext, string, context.Context, context.CancelFunc, error) {
	hubCtx, err := requireHub(projectPath)
	if err != nil {
		return nil, "", nil, nil, err
	}
	if !isJSONOutput() {
		PrintUsingHub(hubCtx.Endpoint)
	}

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return nil, "", nil, nil, wrapHubError(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	return hubCtx, projectID, ctx, cancel, nil
}

func runScheduleList(cmd *cobra.Command, args []string) error {
	hubCtx, projectID, ctx, cancel, err := scheduleHubContext()
	if err != nil {
		return err
	}
	defer cancel()

	return scheduleListViaHub(ctx, hubCtx, projectID)
}

func scheduleListViaHub(ctx context.Context, hubCtx *HubContext, projectID string) error {
	showEvents := scheduleListType == "" || scheduleListType == "all" || scheduleListType == "events"
	showRecurring := scheduleListType == "" || scheduleListType == "all" || scheduleListType == "recurring"

	type listOutput struct {
		Scope     string                                 `json:"scope,omitempty"`
		Events    *hubclient.ListScheduledEventsResponse `json:"events,omitempty"`
		Schedules *hubclient.ListSchedulesResponse       `json:"schedules,omitempty"`
	}
	output := listOutput{}

	if showEvents {
		opts := &hubclient.ListScheduledEventsOptions{}
		if scheduleStatus != "" {
			opts.Status = scheduleStatus
		}
		if scheduleType != "" {
			opts.EventType = scheduleType
		}

		resp, err := hubCtx.Client.ScheduledEvents(projectID).List(ctx, opts)
		if err != nil {
			return wrapHubError(fmt.Errorf("failed to list scheduled events: %w", err))
		}
		output.Events = resp
	}

	if showRecurring {
		opts := &hubclient.ListSchedulesOptions{}
		if scheduleStatus != "" {
			opts.Status = scheduleStatus
		}

		resp, err := hubCtx.Client.Schedules(projectID).List(ctx, opts)
		if err != nil {
			return wrapHubError(fmt.Errorf("failed to list recurring schedules: %w", err))
		}
		output.Schedules = resp
	}

	callerScoped := isCallerScopedScheduleView(hubCtx, output.Events, output.Schedules)
	if callerScoped {
		output.Scope = hubclient.ScheduleListScopeCaller
	}

	if isJSONOutput() {
		return outputJSON(output)
	}

	hasEvents := showEvents && output.Events != nil && len(output.Events.Events) > 0
	hasSchedules := showRecurring && output.Schedules != nil && len(output.Schedules.Schedules) > 0
	if !hasEvents && !hasSchedules {
		if callerScoped {
			fmt.Println("No scheduled events or recurring schedules held by this agent (other project schedules are hidden).")
		} else {
			fmt.Println("No scheduled events or recurring schedules found.")
		}
		return nil
	}

	if callerScoped {
		fmt.Println("Note: showing only schedules and events held by this agent (other project schedules are hidden).")
		fmt.Println()
	}

	printed := false

	// Print one-shot events
	if hasEvents {
		events := output.Events.Events
		fmt.Printf("SCHEDULED EVENTS (one-shot)\n")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "ID\tTYPE\tSTATUS\tFIRE AT\tCREATED")
		for _, evt := range events {
			id := evt.ID
			if len(id) > 8 {
				id = id[:8]
			}
			fireAt := scheduleWhen(evt.FireAt, evt.Status)
			created := clitime.Ago(evt.CreatedAt)
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", id, evt.EventType, evt.Status, fireAt, created)
		}
		_ = w.Flush()
		printed = true
	}

	// Print recurring schedules
	if hasSchedules {
		schedules := output.Schedules.Schedules
		if printed {
			fmt.Println()
		}
		fmt.Printf("RECURRING SCHEDULES\n")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "ID\tNAME\tCRON\tNEXT RUN\tSTATUS")
		for _, sched := range schedules {
			id := sched.ID
			if len(id) > 8 {
				id = id[:8]
			}
			nextRun := "-"
			if sched.NextRunAt != nil {
				nextRun = scheduleWhen(*sched.NextRunAt, sched.Status)
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", id, sched.Name, scheduleCronLabel(sched.CronExpr, sched.TimeZone), nextRun, scheduleStatusLabel(sched.Status, sched.StatusReason))
		}
		_ = w.Flush()
	}

	return nil
}

func runScheduleGet(cmd *cobra.Command, args []string) error {
	resourceID := args[0]

	hubCtx, projectID, ctx, cancel, err := scheduleHubContext()
	if err != nil {
		return err
	}
	defer cancel()

	return scheduleGetViaHub(ctx, hubCtx, projectID, resourceID)
}

func scheduleGetViaHub(ctx context.Context, hubCtx *HubContext, projectID, resourceID string) error {

	// Resolve potentially truncated ID to full UUID (get handles both types)
	fullID, resourceType, err := resolveScheduleID(ctx, hubCtx, projectID, resourceID, "")
	if err != nil {
		return err
	}

	// If we know the resource type from prefix resolution, try that first
	if resourceType == "event" {
		evt, err := hubCtx.Client.ScheduledEvents(projectID).Get(ctx, fullID)
		if err != nil {
			return wrapScheduleHubError(hubCtx, fmt.Errorf("failed to get scheduled event: %w", err))
		}
		if isJSONOutput() {
			return outputJSON(evt)
		}
		printEventDetail(evt)
		return nil
	}
	if resourceType == "schedule" {
		sched, err := hubCtx.Client.Schedules(projectID).Get(ctx, fullID)
		if err != nil {
			return wrapScheduleHubError(hubCtx, fmt.Errorf("failed to get recurring schedule: %w", err))
		}
		if isJSONOutput() {
			return outputJSON(sched)
		}
		printScheduleDetail(sched, fetchLatestScheduleExecSummary(ctx, hubCtx, projectID, sched))
		return nil
	}

	// Full UUID provided (resourceType is empty) — try as one-shot event first
	evt, evtErr := hubCtx.Client.ScheduledEvents(projectID).Get(ctx, fullID)
	if evtErr == nil {
		if isJSONOutput() {
			return outputJSON(evt)
		}
		printEventDetail(evt)
		return nil
	}

	// Try as recurring schedule
	sched, schedErr := hubCtx.Client.Schedules(projectID).Get(ctx, fullID)
	if schedErr == nil {
		if isJSONOutput() {
			return outputJSON(sched)
		}
		printScheduleDetail(sched, fetchLatestScheduleExecSummary(ctx, hubCtx, projectID, sched))
		return nil
	}

	// If one resource returned a specific error (such as 403 Forbidden on an
	// existing schedule held by another principal) while the other returned
	// 404 Not Found, surface the non-404 error rather than masking it as 404.
	if apiclient.IsNotFoundError(evtErr) && !apiclient.IsNotFoundError(schedErr) {
		return wrapScheduleHubError(hubCtx, fmt.Errorf("failed to get recurring schedule: %w", schedErr))
	}
	return wrapScheduleHubError(hubCtx, fmt.Errorf("failed to get scheduled event or recurring schedule: %w", evtErr))
}

func fetchLatestScheduleExecSummary(ctx context.Context, hubCtx *HubContext, projectID string, sched *hubclient.Schedule) *hubclient.ExecSummary {
	if sched == nil || sched.EventType != "exec" || sched.LastRunAt == nil || hubCtx == nil || hubCtx.Client == nil {
		return nil
	}
	hist, err := hubCtx.Client.Schedules(projectID).History(ctx, sched.ID, &hubclient.ListScheduledEventsOptions{
		Page: apiclient.PageOptions{Limit: 1},
	})
	if err != nil || hist == nil || len(hist.Events) == 0 {
		return nil
	}
	return hist.Events[0].ExecSummary
}

func printEventDetail(evt *hubclient.ScheduledEvent) {
	fmt.Printf("Scheduled Event: %s\n", evt.ID)
	fmt.Printf("  Type:       %s\n", evt.EventType)
	fmt.Printf("  Status:     %s\n", evt.Status)
	fmt.Printf("  Fire At:    %s (%s)\n", clitime.Format(evt.FireAt, clitime.Full), scheduleWhen(evt.FireAt, evt.Status))
	fmt.Printf("  Project:      %s\n", evt.ProjectID)
	fmt.Printf("  Created:    %s\n", clitime.Format(evt.CreatedAt, clitime.Full))
	if evt.CreatedBy != "" {
		fmt.Printf("  Created By: %s\n", evt.CreatedBy)
	}
	if evt.FiredAt != nil {
		fmt.Printf("  Fired At:   %s\n", clitime.Format(*evt.FiredAt, clitime.Full))
	}
	if evt.Error != "" {
		fmt.Printf("  Error:      %s\n", evt.Error)
	}
	if evt.ScheduleID != "" {
		fmt.Printf("  Schedule:   %s\n", evt.ScheduleID)
	}

	printSchedulePayloadDetail(evt.EventType, evt.Payload)
	if evt.ExecSummary != nil {
		printEventExecSummary(evt.ExecSummary)
	}
}

func printEventExecSummary(s *hubclient.ExecSummary) {
	fmt.Printf("  Outcome:    %s\n", s.Outcome)
	if s.ExitCode != nil {
		fmt.Printf("  Exit Code:  %d\n", *s.ExitCode)
	}
	if s.DurationMS != nil {
		fmt.Printf("  Duration:   %s\n", formatExecDurationMS(*s.DurationMS))
	}
	if s.OutputBytes != nil {
		fmt.Printf("  Output:     %d B\n", *s.OutputBytes)
	}
}

func printScheduleDetail(sched *hubclient.Schedule, lastExec *hubclient.ExecSummary) {
	fmt.Printf("Recurring Schedule: %s\n", sched.ID)
	fmt.Printf("  Name:       %s\n", sched.Name)
	fmt.Printf("  Status:     %s\n", sched.Status)
	if sched.StatusReason != "" {
		fmt.Printf("  Reason:     %s\n", sched.StatusReason)
	}
	fmt.Printf("  Cron:       %s\n", sched.CronExpr)
	fmt.Printf("  Zone:       %s\n", scheduleZoneLabel(sched.TimeZone))
	if sched.NextRunAt != nil {
		fmt.Printf("  Next Run:   %s (%s)\n", clitime.Format(*sched.NextRunAt, clitime.Full), scheduleWhen(*sched.NextRunAt, sched.Status))
	}
	if sched.LastRunAt != nil {
		lastRunInfo := clitime.Format(*sched.LastRunAt, clitime.Full)
		if sched.LastRunStatus != "" {
			lastRunInfo += " (" + sched.LastRunStatus + ")"
		}
		fmt.Printf("  Last Run:   %s\n", lastRunInfo)
	}
	fmt.Printf("  Event Type: %s\n", sched.EventType)
	fmt.Printf("  Project:      %s\n", sched.ProjectID)
	fmt.Printf("  Created:    %s\n", clitime.Format(sched.CreatedAt, clitime.Full))
	if sched.CreatedBy != "" {
		fmt.Printf("  Created By: %s\n", sched.CreatedBy)
	}
	fmt.Printf("  Run Count:  %d total", sched.RunCount)
	if sched.ErrorCount > 0 {
		fmt.Printf(", %d errors", sched.ErrorCount)
	}
	fmt.Println()
	if sched.LastRunError != "" {
		fmt.Printf("  Last Error: %s\n", sched.LastRunError)
	}
	if lastExec != nil {
		fmt.Printf("  Last Outcome: %s\n", lastExec.Outcome)
		if lastExec.ExitCode != nil {
			fmt.Printf("  Last Exit Code: %d\n", *lastExec.ExitCode)
		}
	}

	printSchedulePayloadDetail(sched.EventType, sched.Payload)
}

type execPayloadView struct {
	AgentID         string   `json:"agentId"`
	AgentName       string   `json:"agentName"`
	Command         []string `json:"command"`
	TimeoutSeconds  int      `json:"timeoutSeconds"`
	CommandRedacted bool     `json:"commandRedacted"`
	Argc            int      `json:"argc"`
	CommandSHA256   string   `json:"commandSha256"`
}

func formatExecArgv(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = strconv.Quote(a)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func formatExecDurationMS(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return (time.Duration(ms) * time.Millisecond).String()
}

func printSchedulePayloadDetail(eventType, rawPayload string) {
	if eventType == "exec" {
		var p execPayloadView
		if rawPayload != "" {
			_ = json.Unmarshal([]byte(rawPayload), &p)
		}
		if p.AgentName != "" {
			fmt.Printf("  Agent:      %s\n", p.AgentName)
		} else if p.AgentID != "" {
			fmt.Printf("  Agent:      %s\n", p.AgentID)
		}
		if p.CommandRedacted {
			if p.CommandSHA256 != "" {
				fmt.Printf("  Command:    <redacted, %d args, sha256:%s>\n", p.Argc, p.CommandSHA256)
			} else {
				fmt.Printf("  Command:    <redacted>\n")
			}
		} else if len(p.Command) > 0 {
			fmt.Printf("  Command:    %s\n", formatExecArgv(p.Command))
		}
		isCorruptRedacted := p.CommandRedacted && p.CommandSHA256 == "" && p.AgentID == "" && p.AgentName == "" && p.TimeoutSeconds == 0
		if !isCorruptRedacted && rawPayload != "" {
			timeoutSec := p.TimeoutSeconds
			if timeoutSec <= 0 {
				timeoutSec = defaultExecTimeoutSeconds
			}
			fmt.Printf("  Timeout:    %ds\n", timeoutSec)
		}
		return
	}

	var payload map[string]interface{}
	if rawPayload != "" {
		_ = json.Unmarshal([]byte(rawPayload), &payload)
	}
	if agentName, ok := payload["agentName"].(string); ok && agentName != "" {
		fmt.Printf("  Agent:      %s\n", agentName)
	}
	if message, ok := payload["message"].(string); ok && message != "" {
		fmt.Printf("  Message:    %q\n", message)
	}
	if eventType == "message" {
		wake, _ := payload["wake"].(bool)
		interrupt, _ := payload["interrupt"].(bool)
		fmt.Printf("  Wake:       %t\n", wake)
		fmt.Printf("  Interrupt:  %t\n", interrupt)
	}
	if template, ok := payload["template"].(string); ok && template != "" {
		fmt.Printf("  Template:   %s\n", template)
	}
	if task, ok := payload["task"].(string); ok && task != "" {
		fmt.Printf("  Task:       %q\n", task)
	}
	if branch, ok := payload["branch"].(string); ok && branch != "" {
		fmt.Printf("  Branch:     %s\n", branch)
	}
}

func runScheduleCancel(cmd *cobra.Command, args []string) error {
	eventID := args[0]

	hubCtx, projectID, ctx, cancel, err := scheduleHubContext()
	if err != nil {
		return err
	}
	defer cancel()

	// Resolve potentially truncated ID to full UUID (cancel only targets events)
	fullID, _, err := resolveScheduleID(ctx, hubCtx, projectID, eventID, "event")
	if err != nil {
		return err
	}

	if err := hubCtx.Client.ScheduledEvents(projectID).Cancel(ctx, fullID); err != nil {
		return wrapScheduleHubError(hubCtx, fmt.Errorf("failed to cancel scheduled event: %w", err))
	}

	return outputActionResult(ActionResult{
		Status:  "ok",
		Command: "schedule cancel",
		Message: fmt.Sprintf("Scheduled event %s cancelled.", fullID),
	})
}

func parseExecTimeoutSeconds(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid --timeout %q: must be a duration such as 60s or 5m", raw)
	}
	if d <= 0 {
		return 0, fmt.Errorf("--timeout must be positive (between 1s and %ds)", agentcontrol.MaxExecTimeoutSec)
	}
	if d%time.Second != 0 {
		return 0, fmt.Errorf("--timeout must be whole seconds (got %s)", raw)
	}
	secs := int(d / time.Second)
	if secs < 1 || secs > agentcontrol.MaxExecTimeoutSec {
		return 0, fmt.Errorf("--timeout must be between 1s and %ds (60m)", agentcontrol.MaxExecTimeoutSec)
	}
	return secs, nil
}

func extractExecCommandArgs(cmd *cobra.Command, args []string) ([]string, error) {
	if cmd != nil {
		dash := cmd.ArgsLenAtDash()
		if dash < 0 {
			if len(args) > 0 {
				return nil, fmt.Errorf("exec command must follow '--' (e.g. -- %s)", args[0])
			}
			return nil, fmt.Errorf("command is required after '--' for exec")
		}
		if dash > 0 {
			return nil, fmt.Errorf("unexpected arguments before '--': %v", args[:dash])
		}
		args = args[dash:]
	}
	if err := agentcontrol.ValidateExecArgv(args); err != nil {
		return nil, describeExecArgvError(args, err)
	}
	return args, nil
}

func describeExecArgvError(args []string, fallback error) error {
	if len(args) == 0 {
		return fmt.Errorf("command is required after '--' for exec")
	}
	if len(args) > agentcontrol.MaxExecArgc {
		return fmt.Errorf("exec command exceeds maximum of %d arguments", agentcontrol.MaxExecArgc)
	}
	if args[0] == "" {
		return fmt.Errorf("exec command argv[0] must not be empty")
	}
	total := 0
	for i, arg := range args {
		if strings.IndexByte(arg, 0) >= 0 {
			return fmt.Errorf("exec command argument %d contains a NUL byte", i)
		}
		if len(arg) > agentcontrol.MaxExecArgBytes {
			return fmt.Errorf("exec command argument %d exceeds %d bytes", i, agentcontrol.MaxExecArgBytes)
		}
		total += len(arg)
	}
	if total > agentcontrol.MaxExecArgvBytes {
		return fmt.Errorf("exec command exceeds total limit of %d bytes", agentcontrol.MaxExecArgvBytes)
	}
	return fmt.Errorf("invalid exec command: %w", fallback)
}

func validateScheduleTypeFlags(cmd *cobra.Command, args []string, resourcePlural string) error {
	switch scheduleType {
	case "message":
		if scheduleAgent == "" {
			return fmt.Errorf("--agent is required for message %s", resourcePlural)
		}
		if scheduleMessage == "" {
			return fmt.Errorf("--message is required for message %s", resourcePlural)
		}
		if scheduleTimeout != "" {
			return fmt.Errorf("--timeout is only valid with --type exec")
		}
		if cmd != nil && cmd.ArgsLenAtDash() >= 0 {
			return fmt.Errorf("command arguments after '--' are only valid with --type exec")
		}
	case "exec":
		if scheduleAgent == "" {
			return fmt.Errorf("--agent is required for exec %s", resourcePlural)
		}
		if scheduleMessage != "" {
			return fmt.Errorf("--message cannot be used with --type exec (pass command after '--')")
		}
		if scheduleInterrupt {
			return fmt.Errorf("--interrupt cannot be used with --type exec")
		}
		if scheduleWake {
			return fmt.Errorf("--wake is not supported with --type exec")
		}
		if _, err := parseExecTimeoutSeconds(scheduleTimeout); err != nil {
			return err
		}
		if _, err := extractExecCommandArgs(cmd, args); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported event type: %q (supported: message, exec)", scheduleType)
	}
	return nil
}

// scheduleCreateArgs validates schedule create's flags (timing and the
// type-specific flags). It is the command's Args validator, so it runs
// before root's PersistentPreRunE and its errors keep the usage block
// (ptone/scion#2859).
func scheduleCreateArgs(cmd *cobra.Command, args []string) error {
	if scheduleIn == "" && scheduleAt == "" {
		return fmt.Errorf("either --in or --at is required")
	}
	if scheduleIn != "" && scheduleAt != "" {
		return fmt.Errorf("--in and --at are mutually exclusive")
	}

	return validateScheduleTypeFlags(cmd, args, "events")
}

func runScheduleCreate(cmd *cobra.Command, args []string) error {
	hubCtx, projectID, ctx, cancel, err := scheduleHubContext()
	if err != nil {
		return err
	}
	defer cancel()

	return scheduleCreateViaHub(ctx, hubCtx, projectID, cmd, args)
}

func scheduleCreateViaHub(ctx context.Context, hubCtx *HubContext, projectID string, cmd *cobra.Command, args []string) error {
	req, err := buildCreateEventRequestForCmd(cmd, args)
	if err != nil {
		return err
	}
	evt, err := hubCtx.Client.ScheduledEvents(projectID).Create(ctx, req)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to create scheduled event: %w", err))
	}

	if isJSONOutput() {
		return outputJSON(evt)
	}

	fmt.Printf("Scheduled event created: %s\n", evt.ID)
	fmt.Printf("  Type:    %s\n", evt.EventType)
	fmt.Printf("  Fire At: %s\n", clitime.Format(evt.FireAt, clitime.Full))

	return nil
}

func validateScheduleCreateRecurring(cmd *cobra.Command, args []string) error {
	if scheduleName == "" {
		return newUsageError("--name is required")
	}
	if scheduleCron == "" {
		return newUsageError("--cron is required")
	}
	if err := validateScheduleTypeFlags(cmd, args, "schedules"); err != nil {
		return newUsageError("%s", err.Error())
	}
	return nil
}

func runScheduleCreateRecurring(cmd *cobra.Command, args []string) error {
	if err := validateScheduleCreateRecurring(cmd, args); err != nil {
		return err
	}

	hubCtx, projectID, ctx, cancel, err := scheduleHubContext()
	if err != nil {
		return err
	}
	defer cancel()

	return scheduleCreateRecurringViaHub(ctx, hubCtx, projectID, cmd, args)
}

func scheduleCreateRecurringViaHub(ctx context.Context, hubCtx *HubContext, projectID string, cmd *cobra.Command, args []string) error {
	req, err := buildCreateRecurringRequestForCmd(cmd, args)
	if err != nil {
		return err
	}
	sched, err := hubCtx.Client.Schedules(projectID).Create(ctx, req)
	if err != nil {
		return wrapScheduleHubError(hubCtx, fmt.Errorf("failed to create recurring schedule: %w", err))
	}

	if isJSONOutput() {
		return outputJSON(sched)
	}

	fmt.Printf("Recurring schedule created: %s\n", sched.ID)
	fmt.Printf("  Name:     %s\n", sched.Name)
	fmt.Printf("  Cron:     %s\n", sched.CronExpr)
	fmt.Printf("  Zone:     %s\n", scheduleZoneLabel(sched.TimeZone))
	fmt.Printf("  Status:   %s\n", sched.Status)
	if sched.NextRunAt != nil {
		fmt.Printf("  Next Run: %s\n", clitime.Format(*sched.NextRunAt, clitime.Full))
	}

	return nil
}

// buildCreateEventRequest builds the create request from the create flags.
func buildCreateEventRequest() *hubclient.CreateScheduledEventRequest {
	req := &hubclient.CreateScheduledEventRequest{
		EventType: scheduleType,
		AgentName: scheduleAgent,
		Message:   scheduleMessage,
		Interrupt: scheduleInterrupt,
		Wake:      scheduleWake,
	}
	if scheduleIn != "" {
		req.FireIn = scheduleIn
	} else {
		req.FireAt = scheduleAt
	}
	return req
}

func buildCreateEventRequestForCmd(cmd *cobra.Command, args []string) (*hubclient.CreateScheduledEventRequest, error) {
	if scheduleType != "exec" {
		return buildCreateEventRequest(), nil
	}
	argv, err := extractExecCommandArgs(cmd, args)
	if err != nil {
		return nil, err
	}
	timeoutSecs, err := parseExecTimeoutSeconds(scheduleTimeout)
	if err != nil {
		return nil, err
	}
	req := &hubclient.CreateScheduledEventRequest{
		EventType:      scheduleType,
		AgentName:      scheduleAgent,
		Command:        argv,
		TimeoutSeconds: timeoutSecs,
	}
	if scheduleIn != "" {
		req.FireIn = scheduleIn
	} else {
		req.FireAt = scheduleAt
	}
	return req, nil
}

// buildCreateRecurringRequest builds the create request from the
// create-recurring flags.
func buildCreateRecurringRequest() *hubclient.CreateScheduleRequest {
	return &hubclient.CreateScheduleRequest{
		Name:      scheduleName,
		CronExpr:  scheduleCron,
		TimeZone:  scheduleTimeZone,
		EventType: scheduleType,
		AgentName: scheduleAgent,
		Message:   scheduleMessage,
		Interrupt: scheduleInterrupt,
		Wake:      scheduleWake,
	}
}

func buildCreateRecurringRequestForCmd(cmd *cobra.Command, args []string) (*hubclient.CreateScheduleRequest, error) {
	if scheduleType != "exec" {
		return buildCreateRecurringRequest(), nil
	}
	argv, err := extractExecCommandArgs(cmd, args)
	if err != nil {
		return nil, err
	}
	timeoutSecs, err := parseExecTimeoutSeconds(scheduleTimeout)
	if err != nil {
		return nil, err
	}
	return &hubclient.CreateScheduleRequest{
		Name:           scheduleName,
		CronExpr:       scheduleCron,
		TimeZone:       scheduleTimeZone,
		EventType:      scheduleType,
		AgentName:      scheduleAgent,
		Command:        argv,
		TimeoutSeconds: timeoutSecs,
	}, nil
}

// scheduleZoneLabel returns the zone a schedule's cron expression is
// evaluated in, for display: the stored IANA name, or UTC when empty.
func scheduleZoneLabel(zone string) string {
	if zone == "" {
		return "UTC"
	}
	return zone
}

// scheduleCronLabel returns the cron column of the schedule list: the
// expression, followed by its zone in parentheses when it is not UTC.
func scheduleCronLabel(expr, zone string) string {
	if zone == "" {
		return expr
	}
	return expr + " (" + zone + ")"
}

func runSchedulePause(cmd *cobra.Command, args []string) error {
	scheduleID := args[0]

	hubCtx, projectID, ctx, cancel, err := scheduleHubContext()
	if err != nil {
		return err
	}
	defer cancel()

	// Resolve potentially truncated ID to full UUID (pause only targets schedules)
	fullID, _, err := resolveScheduleID(ctx, hubCtx, projectID, scheduleID, "schedule")
	if err != nil {
		return err
	}

	if _, err := hubCtx.Client.Schedules(projectID).Pause(ctx, fullID); err != nil {
		return wrapScheduleHubError(hubCtx, fmt.Errorf("failed to pause schedule: %w", err))
	}

	return outputActionResult(ActionResult{
		Status:  "ok",
		Command: "schedule pause",
		Message: fmt.Sprintf("Schedule %s paused.", fullID),
	})
}

func runScheduleResume(cmd *cobra.Command, args []string) error {
	scheduleID := args[0]

	hubCtx, projectID, ctx, cancel, err := scheduleHubContext()
	if err != nil {
		return err
	}
	defer cancel()

	// Resolve potentially truncated ID to full UUID (resume only targets schedules)
	fullID, _, err := resolveScheduleID(ctx, hubCtx, projectID, scheduleID, "schedule")
	if err != nil {
		return err
	}

	sched, err := hubCtx.Client.Schedules(projectID).Resume(ctx, fullID)
	if err != nil {
		return wrapScheduleHubError(hubCtx, fmt.Errorf("failed to resume schedule: %w", err))
	}

	msg := fmt.Sprintf("Schedule %s resumed.", fullID)
	if sched.NextRunAt != nil {
		msg += fmt.Sprintf(" Next run: %s", clitime.Format(*sched.NextRunAt, clitime.Full))
	}

	return outputActionResult(ActionResult{
		Status:  "ok",
		Command: "schedule resume",
		Message: msg,
	})
}

func runScheduleDelete(cmd *cobra.Command, args []string) error {
	scheduleID := args[0]

	hubCtx, projectID, ctx, cancel, err := scheduleHubContext()
	if err != nil {
		return err
	}
	defer cancel()

	// Resolve potentially truncated ID to full UUID (delete only targets schedules)
	fullID, _, err := resolveScheduleID(ctx, hubCtx, projectID, scheduleID, "schedule")
	if err != nil {
		return err
	}

	if err := hubCtx.Client.Schedules(projectID).Delete(ctx, fullID); err != nil {
		return wrapScheduleHubError(hubCtx, fmt.Errorf("failed to delete schedule: %w", err))
	}

	return outputActionResult(ActionResult{
		Status:  "ok",
		Command: "schedule delete",
		Message: fmt.Sprintf("Schedule %s deleted.", fullID),
	})
}

func runScheduleHistory(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		// No schedule ID - list all events (already done via 'schedule list --type events')
		return newUsageError("schedule ID is required for history (usage: scion schedule history <id>)")
	}

	hubCtx, projectID, ctx, cancel, err := scheduleHubContext()
	if err != nil {
		return err
	}
	defer cancel()

	return scheduleHistoryViaHub(ctx, hubCtx, projectID, args[0])
}

func scheduleHistoryViaHub(ctx context.Context, hubCtx *HubContext, projectID, scheduleID string) error {
	// Resolve potentially truncated ID to full UUID (history only targets schedules)
	fullID, _, resolveErr := resolveScheduleID(ctx, hubCtx, projectID, scheduleID, "schedule")
	if resolveErr != nil {
		return resolveErr
	}

	resp, err := hubCtx.Client.Schedules(projectID).History(ctx, fullID, nil)
	if err != nil {
		return wrapScheduleHubError(hubCtx, fmt.Errorf("failed to get schedule history: %w", err))
	}

	if isJSONOutput() {
		return outputJSON(resp)
	}

	printScheduleHistory(resp.Events)
	return nil
}

func printScheduleHistory(events []hubclient.ScheduledEvent) {
	if len(events) == 0 {
		fmt.Println("No execution history found.")
		return
	}

	hasExec := false
	for _, evt := range events {
		if evt.EventType == "exec" || evt.ExecSummary != nil {
			hasExec = true
			break
		}
	}

	fmt.Printf("EXECUTION HISTORY (%d events)\n", len(events))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if hasExec {
		_, _ = fmt.Fprintln(w, "ID\tSTATUS\tOUTCOME\tEXIT\tDURATION\tOUTPUT\tFIRED AT\tERROR")
	} else {
		_, _ = fmt.Fprintln(w, "ID\tSTATUS\tFIRED AT\tERROR")
	}
	for _, evt := range events {
		id := evt.ID
		if len(id) > 8 {
			id = id[:8]
		}
		firedAt := "-"
		if evt.FiredAt != nil {
			firedAt = clitime.Ago(*evt.FiredAt)
		}
		errStr := "-"
		if evt.Error != "" {
			errStr = evt.Error
			if len(errStr) > 60 {
				errStr = errStr[:60] + "..."
			}
		}
		if hasExec {
			outcome, exitStr, durStr, outStr := "-", "-", "-", "-"
			if evt.ExecSummary != nil {
				if evt.ExecSummary.Outcome != "" {
					outcome = evt.ExecSummary.Outcome
				}
				if evt.ExecSummary.ExitCode != nil {
					exitStr = strconv.Itoa(*evt.ExecSummary.ExitCode)
				}
				if evt.ExecSummary.DurationMS != nil {
					durStr = formatExecDurationMS(*evt.ExecSummary.DurationMS)
				}
				if evt.ExecSummary.OutputBytes != nil {
					outStr = fmt.Sprintf("%d B", *evt.ExecSummary.OutputBytes)
				}
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", id, evt.Status, outcome, exitStr, durStr, outStr, firedAt, errStr)
		} else {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", id, evt.Status, firedAt, errStr)
		}
	}
	_ = w.Flush()
}

func init() {
	rootCmd.AddCommand(scheduleCmd)

	scheduleCmd.AddCommand(scheduleListCmd)
	scheduleCmd.AddCommand(scheduleGetCmd)
	scheduleCmd.AddCommand(scheduleCancelCmd)
	scheduleCmd.AddCommand(scheduleCreateCmd)
	scheduleCmd.AddCommand(scheduleCreateRecurringCmd)
	scheduleCmd.AddCommand(schedulePauseCmd)
	scheduleCmd.AddCommand(scheduleResumeCmd)
	scheduleCmd.AddCommand(scheduleDeleteCmd)
	scheduleCmd.AddCommand(scheduleHistoryCmd)

	// List flags
	scheduleListCmd.Flags().StringVar(&scheduleStatus, "status", "", "Filter by status (pending, firing, fired, skipped, cancelled, expired, failed, active, paused)")
	scheduleListCmd.Flags().StringVar(&scheduleType, "type", "", "Filter by event type (message, dispatch_agent, exec)")
	scheduleListCmd.Flags().StringVar(&scheduleListType, "show", "", "Filter by resource type: events, recurring, or all (default: all)")

	// Create one-shot flags
	registerScheduleCreateFlags(scheduleCreateCmd)

	// Create recurring flags
	registerScheduleCreateRecurringFlags(scheduleCreateRecurringCmd)
}

func registerScheduleCreateFlags(c *cobra.Command) {
	c.Flags().StringVar(&scheduleType, "type", "message", "Event type (message or exec)")
	c.Flags().StringVar(&scheduleIn, "in", "", "Schedule after a duration (e.g. 30m, 1h)")
	c.Flags().StringVar(&scheduleAt, "at", "", "Schedule at an absolute time (ISO 8601)")
	c.Flags().StringVar(&scheduleAgent, "agent", "", "Target agent name")
	c.Flags().StringVar(&scheduleMessage, "message", "", "Message body (message events)")
	c.Flags().BoolVar(&scheduleInterrupt, "interrupt", false, "Message events only: interrupt the agent")
	c.Flags().BoolVar(&scheduleWake, "wake", false, "Message events only: resume the target agent if it is suspended when the message fires (requires permission to start it)")
	c.Flags().StringVar(&scheduleTimeout, "timeout", "", "Exec events only: command timeout as a whole-second duration from 1s to 60m (default 60s)")
}

func registerScheduleCreateRecurringFlags(c *cobra.Command) {
	c.Flags().StringVar(&scheduleName, "name", "", "Schedule name (required)")
	c.Flags().StringVar(&scheduleCron, "cron", "", "Cron expression, evaluated in --time-zone (required, 5-field: minute hour day month weekday)")
	c.Flags().StringVar(&scheduleTimeZone, "time-zone", "", "IANA time zone the cron expression is evaluated in, e.g. America/Los_Angeles (default UTC)")
	c.Flags().StringVar(&scheduleType, "type", "message", "Event type (message or exec)")
	c.Flags().StringVar(&scheduleAgent, "agent", "", "Target agent name")
	c.Flags().StringVar(&scheduleMessage, "message", "", "Message body (message schedules)")
	c.Flags().BoolVar(&scheduleInterrupt, "interrupt", false, "Message schedules only: interrupt the agent")
	c.Flags().BoolVar(&scheduleWake, "wake", false, "Message schedules only: resume the target agent if it is suspended when the message fires (requires permission to start it)")
	c.Flags().StringVar(&scheduleTimeout, "timeout", "", "Exec schedules only: command timeout as a whole-second duration from 1s to 60m (default 60s)")
}

// scheduleWhen renders a scheduled time relative to now. A pending time that
// is already due reads "now", as in the web scheduler views, rather than
// "Xm ago"; every other time uses clitime.Relative.
func scheduleWhen(t time.Time, status string) string {
	if status == "pending" && !t.IsZero() && !t.After(clitime.Now()) {
		return "now"
	}
	return clitime.Relative(t)
}

// scheduleStatusLabel renders a recurring schedule's status for the list
// table, with the system reason it was paused, if any, in parentheses (for
// example "paused (creator_deleted)").
func scheduleStatusLabel(status, reason string) string {
	if reason == "" {
		return status
	}
	return status + " (" + reason + ")"
}
