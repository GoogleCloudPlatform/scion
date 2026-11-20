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

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	triggerWebhookName        string
	triggerWebhookDescription string
	triggerWebhookAgent       string
	triggerWebhookToSelf      bool
	triggerWebhookMessage     string
	triggerWebhookInterrupt   bool
	triggerWebhookWake        bool
	triggerWebhookListStatus  string
)

var triggerWebhookCmd = &cobra.Command{
	Use:   "webhook",
	Short: "Manage project webhook endpoints",
	Long:  `Create, inspect, list, and delete signed webhook ingress endpoints for the current project.`,
}

var triggerWebhookCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a webhook endpoint and display its one-time signing secret",
	Long: `Create a signed webhook endpoint for the current project, optionally pairing it
with a default message trigger in the same transaction.

The Hub generates a 32-byte base64url HMAC signing secret and returns it ONCE in
this command's output. Save it immediately — subsequent list and read operations
never return the secret.

One-call endpoint + default trigger:
  Pass --agent <name|id> --message <text> (or --to-self --message <text> inside
  an agent container, resolved strictly from the caller's authenticated Hub
  identity) with optional --interrupt and --wake to create the endpoint and a
  default webhook.received message trigger named after the endpoint in a single
  transaction under the same authority checks as two separate calls.

Data visibility & payload limits:
  - Note: anyone with read access to this project's triggers can read bodies sent here.
  - Requests must be application/json with a body of at most 64 KiB (HTTP 413 above
    64 KiB; ensure any reverse proxy in front of the Hub also permits 64 KiB bodies).
  - Running this command in agent mode prints the one-time secret into the agent's
    transcript; agent-owned endpoints are isolated so the secret only reaches
    triggers on that agent's own endpoint.

Sender signing contract (scion_v1):
  Senders sign the newline-delimited canonical string:
    signed = "v1\n" + t + "\n" + X-Scion-Delivery + "\n" + X-Scion-Event + "\n" + X-Scion-Subject + "\n" + body
  (use an empty string for any optional X-Scion-* header not sent; t must be
  within 300 seconds of Hub time) and send:
    X-Scion-Signature: t=<unix_seconds>,v1=<hex_hmac_sha256>

Example (curl + openssl):
  HOOK_URL="https://hub.example.com/api/v1/hooks/<endpoint-id>"
  SECRET="<one-time-secret>"
  TS="$(date +%s)"
  DELIVERY="$(uuidgen)"
  EVENT="deploy.finished"
  SUBJECT="service/api"
  BODY='{"status":"ok","commit":"abc1234"}'
  SIG="$(printf 'v1\n%s\n%s\n%s\n%s\n%s' "$TS" "$DELIVERY" "$EVENT" "$SUBJECT" "$BODY" | openssl dgst -sha256 -hmac "$SECRET" -r | awk '{print $1}')"
  curl -fsS -X POST "$HOOK_URL" \
    -H "Content-Type: application/json" \
    -H "X-Scion-Signature: t=${TS},v1=${SIG}" \
    -H "X-Scion-Delivery: ${DELIVERY}" \
    -H "X-Scion-Event: ${EVENT}" \
    -H "X-Scion-Subject: ${SUBJECT}" \
    --data-raw "$BODY"`,
	RunE: runTriggerWebhookCreate,
}

var triggerWebhookGetCmd = &cobra.Command{
	Use:   "get <id|name>",
	Short: "Get a webhook endpoint",
	Long:  `Display detailed configuration and ingress counters for a webhook endpoint (never its signing secret).`,
	Args:  cobra.ExactArgs(1),
	RunE:  runTriggerWebhookGet,
}

var triggerWebhookListCmd = &cobra.Command{
	Use:   "list",
	Short: "List webhook endpoints",
	Long:  `List webhook endpoints for the current project. When called in agent mode, only endpoints owned by the calling agent are shown.`,
	RunE:  runTriggerWebhookList,
}

var triggerWebhookDeleteCmd = &cobra.Command{
	Use:   "delete <id|name>",
	Short: "Delete a webhook endpoint",
	Long:  `Delete a webhook endpoint and pause any active triggers attached to it (status_reason=source_deleted).`,
	Args:  cobra.ExactArgs(1),
	RunE:  runTriggerWebhookDelete,
}

// listAllWebhookEndpoints drains every cursor page matching baseOpts and
// returns a single aggregated response with NextCursor cleared.
func listAllWebhookEndpoints(ctx context.Context, svc hubclient.WebhookEndpointService, baseOpts *hubclient.ListWebhookEndpointsOptions) (*hubclient.ListWebhookEndpointsResponse, error) {
	var opts hubclient.ListWebhookEndpointsOptions
	if baseOpts != nil {
		opts = *baseOpts
	}
	if opts.Page.Limit <= 0 {
		opts.Page.Limit = triggerCLIMaxPageLimit
	}
	var agg *hubclient.ListWebhookEndpointsResponse
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
			cp.Endpoints = append([]hubclient.WebhookEndpoint(nil), page.Endpoints...)
			agg = &cp
		} else {
			agg.Endpoints = append(agg.Endpoints, page.Endpoints...)
		}
		if page.NextCursor == "" || seenCursors[page.NextCursor] {
			agg.NextCursor = ""
			return agg, nil
		}
		seenCursors[page.NextCursor] = true
		opts.Page.Cursor = page.NextCursor
	}
}

// resolveWebhookEndpointIDOrName resolves an endpoint UUID, exact name, or unique ID prefix to a full UUID.
func resolveWebhookEndpointIDOrName(ctx context.Context, hubCtx *HubContext, projectID, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", newUsageError("webhook endpoint ID or name is required")
	}
	if uuid.Validate(ref) == nil {
		return ref, nil
	}

	svc := hubCtx.Client.WebhookEndpoints(projectID)
	nameResp, err := svc.List(ctx, &hubclient.ListWebhookEndpointsOptions{
		Name: ref,
		Page: apiclient.PageOptions{Limit: 1},
	})
	if err != nil {
		return "", wrapTriggerHubError(hubCtx, fmt.Errorf("failed to list webhook endpoints for resolution: %w", err))
	}
	if nameResp == nil {
		return "", wrapTriggerHubError(hubCtx, fmt.Errorf("failed to list webhook endpoints for resolution: %w", errors.New("empty response from hub")))
	}
	for _, ep := range nameResp.Endpoints {
		if ep.Name == ref {
			return ep.ID, nil
		}
	}

	resp, err := listAllWebhookEndpoints(ctx, svc, nil)
	if err != nil {
		return "", wrapTriggerHubError(hubCtx, fmt.Errorf("failed to list webhook endpoints for resolution: %w", err))
	}

	for _, ep := range resp.Endpoints {
		if ep.Name == ref {
			return ep.ID, nil
		}
	}

	var prefixMatches []string
	for _, ep := range resp.Endpoints {
		if strings.HasPrefix(ep.ID, ref) {
			prefixMatches = append(prefixMatches, ep.ID)
		}
	}

	switch len(prefixMatches) {
	case 0:
		if isCallerScopedTriggerView(hubCtx, resp.Scope) {
			return "", fmt.Errorf("no webhook endpoint owned by this agent found matching %q (endpoints owned by another principal are hidden from agent callers)", ref)
		}
		return "", fmt.Errorf("no webhook endpoint found matching %q", ref)
	case 1:
		return prefixMatches[0], nil
	default:
		return "", fmt.Errorf("ambiguous webhook endpoint ID prefix %q matches %d endpoints: %s", ref, len(prefixMatches), strings.Join(prefixMatches, ", "))
	}
}

func buildCreateWebhookEndpointRequest() (*hubclient.CreateWebhookEndpointRequest, error) {
	name := strings.TrimSpace(triggerWebhookName)
	if name == "" {
		return nil, newUsageError("--name is required")
	}

	agentRef := strings.TrimSpace(triggerWebhookAgent)
	if agentRef != "" && triggerWebhookToSelf {
		return nil, newUsageError("--agent and --to-self cannot be used together")
	}

	hasTarget := agentRef != "" || triggerWebhookToSelf
	hasTriggerOpts := strings.TrimSpace(triggerWebhookMessage) != "" || triggerWebhookInterrupt || triggerWebhookWake
	if hasTarget && strings.TrimSpace(triggerWebhookMessage) == "" {
		return nil, newUsageError("--message is required when --agent or --to-self is set")
	}
	if !hasTarget && hasTriggerOpts {
		return nil, newUsageError("--agent or --to-self is required when --message, --interrupt, or --wake is set")
	}

	req := &hubclient.CreateWebhookEndpointRequest{
		Name:        name,
		Description: triggerWebhookDescription,
	}
	if hasTarget {
		dt := &hubclient.WebhookEndpointDefaultTriggerSpec{
			Message:   triggerWebhookMessage,
			Interrupt: triggerWebhookInterrupt,
			Wake:      triggerWebhookWake,
		}
		if triggerWebhookToSelf {
			dt.ToSelf = true
		} else if uuid.Validate(agentRef) == nil {
			dt.AgentID = agentRef
		} else {
			dt.AgentName = agentRef
		}
		req.DefaultTrigger = dt
	}
	return req, nil
}

func runTriggerWebhookCreate(cmd *cobra.Command, args []string) error {
	req, err := buildCreateWebhookEndpointRequest()
	if err != nil {
		return err
	}

	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	created, err := hubCtx.Client.WebhookEndpoints(projectID).Create(ctx, req)
	if err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to create webhook endpoint: %w", err))
	}

	if isTriggerJSONOutput() {
		return outputTriggerJSON(created)
	}

	fmt.Printf("Webhook endpoint created: %s\n", triggerTerminalText(created.ID, false))
	fmt.Printf("  Name:           %s\n", triggerTerminalText(created.Name, false))
	fmt.Printf("  URL:            %s\n", triggerTerminalText(created.URL, false))
	fmt.Printf("  Signature Mode: %s\n", triggerTerminalText(created.SignatureMode, false))
	fmt.Printf("  Secret:         %s\n", created.Secret)
	if created.TriggerID != "" {
		fmt.Printf("  Trigger:        %s\n", triggerTerminalText(created.TriggerID, false))
		if created.Trigger != nil {
			agentLabel := created.Trigger.Action.AgentName
			if agentLabel == "" {
				agentLabel = created.Trigger.Action.AgentID
			}
			if agentLabel != "" {
				fmt.Printf("  Target Agent:   %s\n", triggerTerminalText(agentLabel, false))
			}
		}
	}
	fmt.Println()
	fmt.Println("Warning: save this signing secret now; it is shown only once and cannot be retrieved later.")
	fmt.Println("Note: anyone with read access to this project's triggers can read bodies sent here.")
	return nil
}

func runTriggerWebhookGet(cmd *cobra.Command, args []string) error {
	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	fullID, err := resolveWebhookEndpointIDOrName(ctx, hubCtx, projectID, args[0])
	if err != nil {
		return err
	}

	ep, err := hubCtx.Client.WebhookEndpoints(projectID).Get(ctx, fullID)
	if err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to get webhook endpoint: %w", err))
	}

	if isTriggerJSONOutput() {
		return outputTriggerJSON(ep)
	}

	printWebhookEndpointDetail(ep)
	return nil
}

func printWebhookEndpointDetail(ep *hubclient.WebhookEndpoint) {
	fmt.Printf("Webhook Endpoint: %s\n", triggerTerminalText(ep.ID, false))
	fmt.Printf("  Name:           %s\n", triggerTerminalText(ep.Name, false))
	if ep.Description != "" {
		fmt.Printf("  Description:    %s\n", triggerTerminalText(ep.Description, false))
	}
	fmt.Printf("  Project:        %s\n", triggerTerminalText(ep.ProjectID, false))
	fmt.Printf("  Status:         %s\n", triggerTerminalText(ep.Status, false))
	fmt.Printf("  URL:            %s\n", triggerTerminalText(ep.URL, false))
	fmt.Printf("  Signature Mode: %s\n", triggerTerminalText(ep.SignatureMode, false))
	fmt.Printf("  Secret Version: %d\n", ep.SecretVersion)
	fmt.Printf("  Received:       %d (rejected: %d)\n", ep.ReceiveCount, ep.RejectCount)
	lastRecv := "-"
	if ep.LastReceivedAt != nil {
		lastRecv = clitime.Format(*ep.LastReceivedAt, clitime.Full)
	}
	fmt.Printf("  Last Received:  %s\n", lastRecv)
	fmt.Printf("  Created:        %s\n", clitime.Format(ep.CreatedAt, clitime.Full))
}

func runTriggerWebhookList(cmd *cobra.Command, args []string) error {
	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	opts := &hubclient.ListWebhookEndpointsOptions{
		Status: strings.TrimSpace(triggerWebhookListStatus),
	}
	resp, err := listAllWebhookEndpoints(ctx, hubCtx.Client.WebhookEndpoints(projectID), opts)
	if err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to list webhook endpoints: %w", err))
	}

	if isTriggerJSONOutput() {
		return outputTriggerJSON(resp)
	}

	callerScoped := isCallerScopedTriggerView(hubCtx, resp.Scope)
	if len(resp.Endpoints) == 0 {
		if callerScoped {
			fmt.Println("No webhook endpoints owned by this agent (other project endpoints are hidden).")
		} else {
			fmt.Println("No webhook endpoints found.")
		}
		return nil
	}

	if callerScoped {
		fmt.Println("Note: showing only webhook endpoints owned by this agent (other project endpoints are hidden).")
		fmt.Println()
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tNAME\tSTATUS\tMODE\tURL\tRECEIVED\tLAST RECEIVED")
	for _, ep := range resp.Endpoints {
		shortID := ep.ID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		lastRecv := "-"
		if ep.LastReceivedAt != nil {
			lastRecv = clitime.Ago(*ep.LastReceivedAt)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			triggerTerminalText(shortID, false),
			triggerTerminalText(ep.Name, false),
			triggerTerminalText(ep.Status, false),
			triggerTerminalText(ep.SignatureMode, false),
			triggerTerminalText(ep.URL, false),
			ep.ReceiveCount,
			lastRecv,
		)
	}
	_ = w.Flush()
	return nil
}

func runTriggerWebhookDelete(cmd *cobra.Command, args []string) error {
	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	fullID, err := resolveWebhookEndpointIDOrName(ctx, hubCtx, projectID, args[0])
	if err != nil {
		return err
	}

	if err := hubCtx.Client.WebhookEndpoints(projectID).Delete(ctx, fullID); err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to delete webhook endpoint: %w", err))
	}

	return outputTriggerActionResult(ActionResult{
		Status:  "ok",
		Command: "trigger webhook delete",
		Message: fmt.Sprintf("Webhook endpoint %s deleted.", fullID),
	})
}

func init() {
	triggerWebhookCmd.AddCommand(triggerWebhookCreateCmd)
	triggerWebhookCmd.AddCommand(triggerWebhookGetCmd)
	triggerWebhookCmd.AddCommand(triggerWebhookListCmd)
	triggerWebhookCmd.AddCommand(triggerWebhookDeleteCmd)

	triggerWebhookCreateCmd.Flags().StringVar(&triggerWebhookName, "name", "", "Webhook endpoint name (required)")
	triggerWebhookCreateCmd.Flags().StringVar(&triggerWebhookDescription, "description", "", "Optional endpoint description")
	triggerWebhookCreateCmd.Flags().StringVar(&triggerWebhookAgent, "agent", "", "Create a default message trigger targeting this agent name or ID (requires --message)")
	triggerWebhookCreateCmd.Flags().BoolVar(&triggerWebhookToSelf, "to-self", false, "Create a default message trigger targeting the calling agent via its Hub identity (requires --message)")
	triggerWebhookCreateCmd.Flags().StringVar(&triggerWebhookMessage, "message", "", "Message delivered by the default trigger (requires --agent or --to-self)")
	triggerWebhookCreateCmd.Flags().BoolVar(&triggerWebhookInterrupt, "interrupt", false, "Interrupt the target agent when the default trigger fires")
	triggerWebhookCreateCmd.Flags().BoolVar(&triggerWebhookWake, "wake", false, "Resume the target agent if suspended when the default trigger fires")

	triggerWebhookListCmd.Flags().StringVar(&triggerWebhookListStatus, "status", "", "Filter by status (active or disabled)")
}
