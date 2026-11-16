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
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	triggerName        string
	triggerDescription string
	triggerWebhookRef  string
	triggerMatchTypes  []string
	triggerWhereExpr   string
	triggerAgent       string
	triggerMessage     string
	triggerInterrupt   bool
	triggerWake        bool
	triggerPlain       bool
	triggerSubjectExpr string
	triggerOnSubject   string

	triggerListSource  string
	triggerListWebhook string
	triggerListStatus  string

	triggerHistoryLimit  int
	triggerHistoryCursor string
)

// triggerCmd is the top-level command group for trigger and webhook endpoint management.
var triggerCmd = &cobra.Command{
	Use:   "trigger",
	Short: "Manage event triggers and webhook endpoints",
	Long:  `Create, list, pause, resume, and delete event triggers and webhook endpoints for the current project.`,
}

var triggerCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create an event trigger",
	Long: `Create an event trigger attached to a webhook endpoint. Requires --name, --webhook, --agent, and --message.

Examples:
  # Deliver a message to agent "lead" whenever webhook endpoint "deploys" receives an event
  scion trigger create --name deploy-finished --webhook deploys --agent lead --message "Check incoming deploy event" --wake

  # Match specific event types
  scion trigger create --name prod-deploy --webhook deploys --type webhook.deploy.finished --agent lead --message "Production deploy finished"`,
	RunE: runTriggerCreate,
}

var triggerListCmd = &cobra.Command{
	Use:   "list",
	Short: "List event triggers",
	Long:  `List event triggers for the current project. When called in agent mode, only triggers held by the calling agent are shown.`,
	RunE:  runTriggerList,
}

var triggerDeleteCmd = &cobra.Command{
	Use:   "delete <id|name>",
	Short: "Delete an event trigger",
	Args:  cobra.ExactArgs(1),
	RunE:  runTriggerDelete,
}

var triggerPauseCmd = &cobra.Command{
	Use:   "pause <id|name>",
	Short: "Pause an active event trigger",
	Args:  cobra.ExactArgs(1),
	RunE:  runTriggerPause,
}

var triggerResumeCmd = &cobra.Command{
	Use:   "resume <id|name>",
	Short: "Resume a paused event trigger",
	Args:  cobra.ExactArgs(1),
	RunE:  runTriggerResume,
}

var triggerHistoryCmd = &cobra.Command{
	Use:   "history <id|name>",
	Short: "Show fire history for an event trigger",
	Long:  `List fire history records for an event trigger, ordered newest first.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runTriggerHistory,
}

var triggerHubContextFn = triggerHubContext

func triggerHubContext() (*HubContext, string, context.Context, context.CancelFunc, error) {
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

func isCallerScopedTriggerView(hubCtx *HubContext, scope string) bool {
	if scope == hubclient.TriggerListScopeCaller {
		return true
	}
	return hubCtx != nil && hubCtx.CredentialKind == hubsync.CredentialKindAgentToken
}

func wrapTriggerHubError(hubCtx *HubContext, err error) error {
	if isCallerScopedTriggerView(hubCtx, "") {
		switch {
		case apiclient.IsConflictError(err) && !strings.Contains(err.Error(), "scion trigger"):
			return wrapHubError(fmt.Errorf("%w (if this name does not appear in 'scion trigger list' or 'scion trigger webhook list', it may be held by another principal and hidden from this agent)", err))
		case apiclient.IsForbiddenError(err) && strings.Contains(err.Error(), "Insufficient permissions"):
			return wrapHubError(fmt.Errorf("%w (agent callers can only access triggers and webhook endpoints they own)", err))
		}
	}
	return wrapHubError(err)
}

const triggerCLIMaxPageLimit = 200

// listAllTriggers drains every cursor page matching baseOpts and returns a
// single aggregated response with NextCursor cleared.
func listAllTriggers(ctx context.Context, svc hubclient.TriggerService, baseOpts *hubclient.ListTriggersOptions) (*hubclient.ListTriggersResponse, error) {
	var opts hubclient.ListTriggersOptions
	if baseOpts != nil {
		opts = *baseOpts
	}
	if opts.Page.Limit <= 0 {
		opts.Page.Limit = triggerCLIMaxPageLimit
	}
	var agg *hubclient.ListTriggersResponse
	seenCursors := map[string]bool{}
	for {
		page, err := svc.List(ctx, &opts)
		if err != nil {
			return nil, err
		}
		if page == nil {
			return nil, errors.New("empty response from hub")
		}
		if agg == nil {
			cp := *page
			cp.Triggers = append([]hubclient.Trigger(nil), page.Triggers...)
			agg = &cp
		} else {
			agg.Triggers = append(agg.Triggers, page.Triggers...)
		}
		if page.NextCursor == "" || seenCursors[page.NextCursor] {
			agg.NextCursor = ""
			return agg, nil
		}
		seenCursors[page.NextCursor] = true
		opts.Page.Cursor = page.NextCursor
	}
}

// resolveTriggerIDOrName resolves a trigger UUID, exact name, or unique ID prefix to a full UUID.
func resolveTriggerIDOrName(ctx context.Context, hubCtx *HubContext, projectID, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", newUsageError("trigger ID or name is required")
	}
	if uuid.Validate(ref) == nil {
		return ref, nil
	}

	svc := hubCtx.Client.Triggers(projectID)
	nameResp, err := svc.List(ctx, &hubclient.ListTriggersOptions{
		Name: ref,
		Page: apiclient.PageOptions{Limit: 1},
	})
	if err != nil {
		return "", wrapTriggerHubError(hubCtx, fmt.Errorf("failed to list triggers for resolution: %w", err))
	}
	if nameResp == nil {
		return "", wrapTriggerHubError(hubCtx, fmt.Errorf("failed to list triggers for resolution: %w", errors.New("empty response from hub")))
	}
	for _, tr := range nameResp.Triggers {
		if tr.Name == ref {
			return tr.ID, nil
		}
	}

	resp, err := listAllTriggers(ctx, svc, nil)
	if err != nil {
		return "", wrapTriggerHubError(hubCtx, fmt.Errorf("failed to list triggers for resolution: %w", err))
	}

	for _, tr := range resp.Triggers {
		if tr.Name == ref {
			return tr.ID, nil
		}
	}

	var prefixMatches []string
	for _, tr := range resp.Triggers {
		if strings.HasPrefix(tr.ID, ref) {
			prefixMatches = append(prefixMatches, tr.ID)
		}
	}

	switch len(prefixMatches) {
	case 0:
		if isCallerScopedTriggerView(hubCtx, resp.Scope) {
			return "", fmt.Errorf("no trigger held by this agent found matching %q (triggers held by another principal are hidden from agent callers)", ref)
		}
		return "", fmt.Errorf("no trigger found matching %q", ref)
	case 1:
		return prefixMatches[0], nil
	default:
		return "", fmt.Errorf("ambiguous trigger ID prefix %q matches %d triggers: %s", ref, len(prefixMatches), strings.Join(prefixMatches, ", "))
	}
}

func buildCreateTriggerRequest() (*hubclient.CreateTriggerRequest, error) {
	name := strings.TrimSpace(triggerName)
	if name == "" {
		return nil, newUsageError("--name is required")
	}
	webhookRef := strings.TrimSpace(triggerWebhookRef)
	if webhookRef == "" {
		return nil, newUsageError("--webhook is required")
	}
	agentRef := strings.TrimSpace(triggerAgent)
	if agentRef == "" {
		return nil, newUsageError("--agent is required")
	}
	if strings.TrimSpace(triggerMessage) == "" {
		return nil, newUsageError("--message is required")
	}

	action := hubclient.TriggerActionSpec{
		Type:      "message",
		Message:   triggerMessage,
		Interrupt: triggerInterrupt,
		Wake:      triggerWake,
		Plain:     triggerPlain,
	}
	if uuid.Validate(agentRef) == nil {
		action.AgentID = agentRef
	} else {
		action.AgentName = agentRef
	}

	req := &hubclient.CreateTriggerRequest{
		Name:        name,
		Description: triggerDescription,
		Source: hubclient.TriggerSourceSpec{
			Kind: "webhook",
			ID:   webhookRef,
		},
		Action: action,
	}

	if len(triggerMatchTypes) > 0 || triggerWhereExpr != "" {
		req.Match = &hubclient.TriggerMatchSpec{
			Types: append([]string(nil), triggerMatchTypes...),
			Expr:  triggerWhereExpr,
		}
	}
	if triggerSubjectExpr != "" || triggerOnSubject != "" {
		req.Subject = &hubclient.TriggerSubjectSpec{
			Expr: triggerSubjectExpr,
			Mode: triggerOnSubject,
		}
	}

	return req, nil
}

func runTriggerCreate(cmd *cobra.Command, args []string) error {
	req, err := buildCreateTriggerRequest()
	if err != nil {
		return err
	}

	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	created, err := hubCtx.Client.Triggers(projectID).Create(ctx, req)
	if err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to create trigger: %w", err))
	}

	if isJSONOutput() {
		return outputJSON(created)
	}

	agentLabel := created.Action.AgentName
	if agentLabel == "" {
		agentLabel = created.Action.AgentID
	}
	fmt.Printf("Trigger created: %s\n", created.ID)
	fmt.Printf("  Name:    %s\n", created.Name)
	fmt.Printf("  Source:  %s:%s\n", created.Source.Kind, created.Source.ID)
	fmt.Printf("  Types:   %s\n", strings.Join(created.Match.Types, ", "))
	fmt.Printf("  Agent:   %s\n", agentLabel)
	fmt.Printf("  Status:  %s\n", triggerStatusLabel(created.Status, created.StatusReason))
	return nil
}

func runTriggerList(cmd *cobra.Command, args []string) error {
	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	opts := &hubclient.ListTriggersOptions{
		Source: strings.TrimSpace(triggerListSource),
		Status: strings.TrimSpace(triggerListStatus),
	}
	if webhookRef := strings.TrimSpace(triggerListWebhook); webhookRef != "" {
		resolvedID, rErr := resolveWebhookEndpointIDOrName(ctx, hubCtx, projectID, webhookRef)
		if rErr != nil {
			return rErr
		}
		opts.SourceID = resolvedID
		if opts.Source == "" {
			opts.Source = "webhook"
		}
	}

	resp, err := listAllTriggers(ctx, hubCtx.Client.Triggers(projectID), opts)
	if err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to list triggers: %w", err))
	}

	if isJSONOutput() {
		return outputJSON(resp)
	}

	callerScoped := isCallerScopedTriggerView(hubCtx, resp.Scope)
	if len(resp.Triggers) == 0 {
		if callerScoped {
			fmt.Println("No triggers held by this agent (other project triggers are hidden).")
		} else {
			fmt.Println("No triggers found.")
		}
		return nil
	}

	if callerScoped {
		fmt.Println("Note: showing only triggers held by this agent (other project triggers are hidden).")
		fmt.Println()
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tNAME\tSOURCE\tTYPES\tAGENT\tSTATUS\tFIRES\tLAST FIRE")
	for _, tr := range resp.Triggers {
		shortID := tr.ID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		sourceShort := tr.Source.ID
		if len(sourceShort) > 8 {
			sourceShort = sourceShort[:8]
		}
		agentLabel := tr.Action.AgentName
		if agentLabel == "" {
			agentLabel = tr.Action.AgentID
			if len(agentLabel) > 8 {
				agentLabel = agentLabel[:8]
			}
		}
		lastFire := "-"
		if tr.LastFireAt != nil {
			lastFire = clitime.Ago(*tr.LastFireAt)
			if tr.LastFireStatus != "" {
				lastFire += " (" + tr.LastFireStatus + ")"
			}
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s:%s\t%s\t%s\t%s\t%d\t%s\n",
			shortID,
			tr.Name,
			tr.Source.Kind,
			sourceShort,
			strings.Join(tr.Match.Types, ","),
			agentLabel,
			triggerStatusLabel(tr.Status, tr.StatusReason),
			tr.FireCount,
			lastFire,
		)
	}
	_ = w.Flush()
	return nil
}

func runTriggerDelete(cmd *cobra.Command, args []string) error {
	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	fullID, err := resolveTriggerIDOrName(ctx, hubCtx, projectID, args[0])
	if err != nil {
		return err
	}

	if err := hubCtx.Client.Triggers(projectID).Delete(ctx, fullID); err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to delete trigger: %w", err))
	}

	return outputActionResult(ActionResult{
		Status:  "ok",
		Command: "trigger delete",
		Message: fmt.Sprintf("Trigger %s deleted.", fullID),
	})
}

func runTriggerPause(cmd *cobra.Command, args []string) error {
	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	fullID, err := resolveTriggerIDOrName(ctx, hubCtx, projectID, args[0])
	if err != nil {
		return err
	}

	if _, err := hubCtx.Client.Triggers(projectID).Pause(ctx, fullID); err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to pause trigger: %w", err))
	}

	return outputActionResult(ActionResult{
		Status:  "ok",
		Command: "trigger pause",
		Message: fmt.Sprintf("Trigger %s paused.", fullID),
	})
}

func runTriggerResume(cmd *cobra.Command, args []string) error {
	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	fullID, err := resolveTriggerIDOrName(ctx, hubCtx, projectID, args[0])
	if err != nil {
		return err
	}

	if _, err := hubCtx.Client.Triggers(projectID).Resume(ctx, fullID); err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to resume trigger: %w", err))
	}

	return outputActionResult(ActionResult{
		Status:  "ok",
		Command: "trigger resume",
		Message: fmt.Sprintf("Trigger %s resumed.", fullID),
	})
}

func runTriggerHistory(cmd *cobra.Command, args []string) error {
	if triggerHistoryLimit < 0 {
		return newUsageError("--limit must be non-negative")
	}

	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	fullID, err := resolveTriggerIDOrName(ctx, hubCtx, projectID, args[0])
	if err != nil {
		return err
	}

	var opts *hubclient.ListTriggerFiresOptions
	if triggerHistoryLimit > 0 || strings.TrimSpace(triggerHistoryCursor) != "" {
		opts = &hubclient.ListTriggerFiresOptions{
			Page: apiclient.PageOptions{
				Limit:  triggerHistoryLimit,
				Cursor: strings.TrimSpace(triggerHistoryCursor),
			},
		}
	}

	resp, err := hubCtx.Client.Triggers(projectID).History(ctx, fullID, opts)
	if err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to get trigger history: %w", err))
	}

	if isJSONOutput() {
		return outputJSON(resp)
	}

	if len(resp.Fires) == 0 {
		fmt.Println("No fire history found.")
		return nil
	}

	fmt.Printf("FIRE HISTORY (%d of %d fires)\n", len(resp.Fires), resp.TotalCount)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tEVENT\tSTATUS\tSUBJECT\tSTARTED\tFINISHED\tERROR")
	for _, fire := range resp.Fires {
		shortID := fire.ID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		shortEvent := fire.EventID
		if len(shortEvent) > 8 {
			shortEvent = shortEvent[:8]
		}
		subj := fire.Subject
		if subj == "" {
			subj = "-"
		}
		started := clitime.Ago(fire.StartedAt)
		finished := "-"
		if fire.FinishedAt != nil {
			finished = clitime.Ago(*fire.FinishedAt)
		}
		errMsg := fire.Error
		if errMsg == "" {
			errMsg = "-"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			shortID,
			shortEvent,
			fire.Status,
			subj,
			started,
			finished,
			errMsg,
		)
	}
	_ = w.Flush()
	if resp.NextCursor != "" {
		fmt.Printf("\nNext cursor: %s\n", resp.NextCursor)
	}
	return nil
}

func triggerStatusLabel(status, reason string) string {
	if reason == "" {
		return status
	}
	return status + " (" + reason + ")"
}

func init() {
	rootCmd.AddCommand(triggerCmd)

	triggerCmd.AddCommand(triggerCreateCmd)
	triggerCmd.AddCommand(triggerListCmd)
	triggerCmd.AddCommand(triggerDeleteCmd)
	triggerCmd.AddCommand(triggerPauseCmd)
	triggerCmd.AddCommand(triggerResumeCmd)
	triggerCmd.AddCommand(triggerHistoryCmd)
	triggerCmd.AddCommand(triggerWebhookCmd)

	triggerCreateCmd.Flags().StringVar(&triggerName, "name", "", "Trigger name (required)")
	triggerCreateCmd.Flags().StringVar(&triggerDescription, "description", "", "Optional trigger description")
	triggerCreateCmd.Flags().StringVar(&triggerWebhookRef, "webhook", "", "Webhook endpoint ID or name (required)")
	triggerCreateCmd.Flags().StringSliceVar(&triggerMatchTypes, "type", nil, "Event type pattern to match (repeatable; default webhook.received)")
	triggerCreateCmd.Flags().StringVar(&triggerWhereExpr, "where", "", "Optional CEL filter predicate")
	triggerCreateCmd.Flags().StringVar(&triggerAgent, "agent", "", "Target agent name or ID (required)")
	triggerCreateCmd.Flags().StringVar(&triggerMessage, "message", "", "Message delivered to the target agent (required)")
	triggerCreateCmd.Flags().BoolVar(&triggerInterrupt, "interrupt", false, "Interrupt the target agent")
	triggerCreateCmd.Flags().BoolVar(&triggerWake, "wake", false, "Resume the target agent if suspended when the trigger fires")
	triggerCreateCmd.Flags().BoolVar(&triggerPlain, "plain", false, "Deliver message text without sender prefix")
	triggerCreateCmd.Flags().StringVar(&triggerSubjectExpr, "subject", "", "Optional CEL expression deriving the concurrency subject")
	triggerCreateCmd.Flags().StringVar(&triggerOnSubject, "on-subject", "", "Subject concurrency policy (new or skip; default new)")

	triggerListCmd.Flags().StringVar(&triggerListSource, "source", "", "Filter by source kind (e.g. webhook)")
	triggerListCmd.Flags().StringVar(&triggerListWebhook, "webhook", "", "Filter by webhook endpoint ID or name")
	triggerListCmd.Flags().StringVar(&triggerListStatus, "status", "", "Filter by status (active or paused)")

	triggerHistoryCmd.Flags().IntVar(&triggerHistoryLimit, "limit", 0, "Maximum number of fire records to return")
	triggerHistoryCmd.Flags().StringVar(&triggerHistoryCursor, "cursor", "", "Pagination cursor from a previous history response")
}
