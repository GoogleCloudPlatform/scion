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
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	triggerWebhookName        string
	triggerWebhookDescription string
	triggerWebhookListStatus  string
)

var triggerWebhookCmd = &cobra.Command{
	Use:   "webhook",
	Short: "Manage project webhook endpoints",
	Long:  `Create, list, and delete signed webhook ingress endpoints for the current project.`,
}

var triggerWebhookCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a webhook endpoint and display its one-time signing secret",
	Long: `Create a signed webhook endpoint for the current project.

The Hub generates a 32-byte base64url HMAC signing secret and returns it ONCE in
this command's output. Save it immediately — subsequent list and read operations
never return the secret.

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
  SIGNED="$(printf 'v1\n%s\n%s\n%s\n%s\n%s' "$TS" "$DELIVERY" "$EVENT" "$SUBJECT" "$BODY")"
  SIG="$(printf '%s' "$SIGNED" | openssl dgst -sha256 -hmac "$SECRET" -r | awk '{print $1}')"
  curl -fsS -X POST "$HOOK_URL" \
    -H "Content-Type: application/json" \
    -H "X-Scion-Signature: t=${TS},v1=${SIG}" \
    -H "X-Scion-Delivery: ${DELIVERY}" \
    -H "X-Scion-Event: ${EVENT}" \
    -H "X-Scion-Subject: ${SUBJECT}" \
    --data-raw "$BODY"`,
	RunE: runTriggerWebhookCreate,
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

// resolveWebhookEndpointIDOrName resolves an endpoint UUID, exact name, or unique ID prefix to a full UUID.
func resolveWebhookEndpointIDOrName(ctx context.Context, hubCtx *HubContext, projectID, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", newUsageError("webhook endpoint ID or name is required")
	}
	if uuid.Validate(ref) == nil {
		return ref, nil
	}

	resp, err := hubCtx.Client.WebhookEndpoints(projectID).List(ctx, nil)
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

func runTriggerWebhookCreate(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(triggerWebhookName)
	if name == "" {
		return newUsageError("--name is required")
	}

	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	created, err := hubCtx.Client.WebhookEndpoints(projectID).Create(ctx, &hubclient.CreateWebhookEndpointRequest{
		Name:        name,
		Description: triggerWebhookDescription,
	})
	if err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to create webhook endpoint: %w", err))
	}

	if isJSONOutput() {
		return outputJSON(created)
	}

	fmt.Printf("Webhook endpoint created: %s\n", created.ID)
	fmt.Printf("  Name:           %s\n", created.Name)
	fmt.Printf("  URL:            %s\n", created.URL)
	fmt.Printf("  Signature Mode: %s\n", created.SignatureMode)
	fmt.Printf("  Secret:         %s\n", created.Secret)
	fmt.Println()
	fmt.Println("Warning: save this signing secret now; it is shown only once and cannot be retrieved later.")
	fmt.Println("Note: anyone with read access to this project's triggers can read bodies sent here.")
	return nil
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
	resp, err := hubCtx.Client.WebhookEndpoints(projectID).List(ctx, opts)
	if err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to list webhook endpoints: %w", err))
	}

	if isJSONOutput() {
		return outputJSON(resp)
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
			shortID,
			ep.Name,
			ep.Status,
			ep.SignatureMode,
			ep.URL,
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

	return outputActionResult(ActionResult{
		Status:  "ok",
		Command: "trigger webhook delete",
		Message: fmt.Sprintf("Webhook endpoint %s deleted.", fullID),
	})
}

func init() {
	triggerWebhookCmd.AddCommand(triggerWebhookCreateCmd)
	triggerWebhookCmd.AddCommand(triggerWebhookListCmd)
	triggerWebhookCmd.AddCommand(triggerWebhookDeleteCmd)

	triggerWebhookCreateCmd.Flags().StringVar(&triggerWebhookName, "name", "", "Webhook endpoint name (required)")
	triggerWebhookCreateCmd.Flags().StringVar(&triggerWebhookDescription, "description", "", "Optional endpoint description")

	triggerWebhookListCmd.Flags().StringVar(&triggerWebhookListStatus, "status", "", "Filter by status (active or disabled)")
}
