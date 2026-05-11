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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
)

var (
	notificationsShowAll bool
	notificationsJSON    bool
	notificationsAckAll  bool

	subscribeAgent    string
	subscribeProject  string
	subscribeTriggers string

	unsubscribeAll     bool
	unsubscribeProject string

	updateTriggers string

	subscriptionsProject string
	subscriptionsJSON    bool
)

// Default trigger activities for subscriptions.
var defaultTriggers = []string{"COMPLETED", "WAITING_FOR_INPUT", "LIMITS_EXCEEDED"}

// notificationsCmd is the top-level command group for notification management.
var notificationsCmd = &cobra.Command{
	Use:     "notifications",
	Aliases: []string{"notification", "notif"},
	Short:   "Manage notifications and subscriptions",
	Long: `Manage notifications and notification subscriptions.

Notifications require Hub mode. Enable with 'scion hub enable <endpoint>'.

Commands:
  scion notifications                         List your notifications
  scion notifications ack [id]                Acknowledge notification(s)
  scion notifications subscribe               Create a subscription
  scion notifications unsubscribe [id]        Remove a subscription
  scion notifications update [id]             Update a subscription's triggers
  scion notifications subscriptions           List your subscriptions`,
	RunE: runNotificationsList,
}

// notificationsAckCmd acknowledges notifications.
var notificationsAckCmd = &cobra.Command{
	Use:   "ack [notification-id]",
	Short: "Acknowledge notification(s)",
	Long: `Acknowledge one or all notifications.

With an ID argument, acknowledges that specific notification.
With --all flag, acknowledges all unacknowledged notifications.

Examples:
  scion notifications ack a1b2c3d4
  scion notifications ack --all`,
	RunE: runNotificationsAck,
}

// notificationsSubscribeCmd creates a subscription.
var notificationsSubscribeCmd = &cobra.Command{
	Use:   "subscribe",
	Short: "Create a notification subscription",
	Long: `Subscribe to notifications for a specific agent or all agents in a project.

If --agent is provided, creates an agent-scoped subscription.
If only --project is provided, creates a project-scoped subscription.

The --project flag can be omitted if the current directory is within a project
that is linked to the Hub.

Examples:
  # Subscribe to a specific agent
  scion notifications subscribe --agent my-agent

  # Subscribe to all agents in a project
  scion notifications subscribe --project my-project

  # Subscribe with specific triggers
  scion notifications subscribe --agent my-agent --triggers COMPLETED,WAITING_FOR_INPUT`,
	RunE: runNotificationsSubscribe,
}

// notificationsUnsubscribeCmd removes a subscription.
var notificationsUnsubscribeCmd = &cobra.Command{
	Use:   "unsubscribe [subscription-id]",
	Short: "Remove a notification subscription",
	Long: `Remove a notification subscription by ID, or remove all subscriptions in a project.

Examples:
  scion notifications unsubscribe a1b2c3d4
  scion notifications unsubscribe --project my-project --all`,
	RunE: runNotificationsUnsubscribe,
}

// notificationsUpdateCmd updates a subscription's trigger activities.
var notificationsUpdateCmd = &cobra.Command{
	Use:   "update [subscription-id]",
	Short: "Update a subscription's trigger activities",
	Long: `Update the trigger activities for an existing subscription.

Examples:
  scion notifications update a1b2c3d4 --triggers COMPLETED,WAITING_FOR_INPUT,DELETED`,
	Args: cobra.ExactArgs(1),
	RunE: runNotificationsUpdate,
}

// notificationsSubscriptionsCmd lists subscriptions.
var notificationsSubscriptionsCmd = &cobra.Command{
	Use:     "subscriptions",
	Aliases: []string{"subs"},
	Short:   "List your notification subscriptions",
	Long: `List notification subscriptions owned by you.

Examples:
  scion notifications subscriptions
  scion notifications subscriptions --project my-project
  scion notifications subscriptions --json`,
	RunE: runNotificationsSubscriptions,
}

func init() {
	rootCmd.AddCommand(notificationsCmd)
	notificationsCmd.AddCommand(notificationsAckCmd)
	notificationsCmd.AddCommand(notificationsSubscribeCmd)
	notificationsCmd.AddCommand(notificationsUnsubscribeCmd)
	notificationsCmd.AddCommand(notificationsUpdateCmd)
	notificationsCmd.AddCommand(notificationsSubscriptionsCmd)

	// List notifications flags (on the parent command)
	notificationsCmd.Flags().BoolVar(&notificationsShowAll, "all", false, "Include acknowledged notifications")
	notificationsCmd.Flags().BoolVar(&notificationsJSON, "json", false, "Output in JSON format")

	// Ack flags
	notificationsAckCmd.Flags().BoolVar(&notificationsAckAll, "all", false, "Acknowledge all notifications")

	// Subscribe flags
	notificationsSubscribeCmd.Flags().StringVar(&subscribeAgent, "agent", "", "Agent name or ID to subscribe to")
	notificationsSubscribeCmd.Flags().StringVar(&subscribeProject, "project", "", "Project to subscribe in (inferred from context if omitted)")
	notificationsSubscribeCmd.Flags().StringVar(&subscribeProject, "grove", "", "Deprecated alias for --project")
	_ = notificationsSubscribeCmd.Flags().MarkDeprecated("grove", "use --project instead")
	_ = notificationsSubscribeCmd.Flags().MarkHidden("grove")
	notificationsSubscribeCmd.Flags().StringVar(&subscribeTriggers, "triggers", "", "Comma-separated trigger activities (default: COMPLETED,WAITING_FOR_INPUT,LIMITS_EXCEEDED)")

	// Unsubscribe flags
	notificationsUnsubscribeCmd.Flags().BoolVar(&unsubscribeAll, "all", false, "Remove all subscriptions in the project")
	notificationsUnsubscribeCmd.Flags().StringVar(&unsubscribeProject, "project", "", "Project to unsubscribe from (used with --all)")
	notificationsUnsubscribeCmd.Flags().StringVar(&unsubscribeProject, "grove", "", "Deprecated alias for --project")
	_ = notificationsUnsubscribeCmd.Flags().MarkDeprecated("grove", "use --project instead")
	_ = notificationsUnsubscribeCmd.Flags().MarkHidden("grove")

	// Update flags
	notificationsUpdateCmd.Flags().StringVar(&updateTriggers, "triggers", "", "Comma-separated trigger activities (required)")
	notificationsUpdateCmd.MarkFlagRequired("triggers")

	// Subscriptions list flags
	notificationsSubscriptionsCmd.Flags().StringVar(&subscriptionsProject, "project", "", "Filter by project")
	notificationsSubscriptionsCmd.Flags().StringVar(&subscriptionsProject, "grove", "", "Deprecated alias for --project")
	_ = notificationsSubscriptionsCmd.Flags().MarkDeprecated("grove", "use --project instead")
	_ = notificationsSubscriptionsCmd.Flags().MarkHidden("grove")
	notificationsSubscriptionsCmd.Flags().BoolVar(&subscriptionsJSON, "json", false, "Output in JSON format")
}

// requireHubClient resolves settings and returns a hub client, or errors if hub is not enabled.
func requireHubClient() (*config.Settings, hubclient.Client, error) {
	resolvedPath, _, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to resolve project path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load settings: %w", err)
	}

	if !settings.IsHubEnabled() {
		return nil, nil, fmt.Errorf("notifications require Hub mode. Enable with 'scion hub enable <endpoint>'")
	}

	client, err := getHubClient(settings)
	if err != nil {
		return nil, nil, err
	}

	return settings, client, nil
}

// resolveProjectID resolves the project ID from flag, settings, or current context.
func resolveProjectID(settings *config.Settings, projectFlag string) (string, error) {
	if projectFlag != "" {
		return projectFlag, nil
	}

	// Try hub project ID first, then local project ID
	projectID := settings.GetHubProjectID()
	if projectID == "" {
		projectID = settings.ProjectID
	}
	if projectID == "" {
		return "", fmt.Errorf("cannot determine project ID. Use --project flag or link this project with 'scion hub link'")
	}
	return projectID, nil
}

// resolveAgentIDForSubscription looks up an agent by name/slug in the project and returns its ID.
func resolveAgentIDForSubscription(ctx context.Context, client hubclient.Client, projectID, agentRef string) (string, error) {
	slug := api.Slugify(agentRef)
	resp, err := client.ProjectAgents(projectID).List(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("failed to look up agents: %w", err)
	}

	for _, agent := range resp.Agents {
		if agent.Slug == slug || agent.Name == agentRef || agent.ID == agentRef {
			return agent.ID, nil
		}
	}
	return "", fmt.Errorf("agent '%s' not found in project", agentRef)
}

func runNotificationsList(cmd *cobra.Command, args []string) error {
	if notificationsJSON {
		outputFormat = "json"
	}

	_, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	opts := &hubclient.ListNotificationsOptions{
		OnlyUnacknowledged: !notificationsShowAll,
	}

	notifs, err := client.Notifications().List(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to list notifications: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(notifs)
	}

	if len(notifs) == 0 {
		fmt.Println("No notifications")
		return nil
	}

	fmt.Printf("%-12s  %-14s  %-20s  %-20s  %s\n", "ID", "AGENT", "STATUS", "TIME", "MESSAGE")
	fmt.Printf("%-12s  %-14s  %-20s  %-20s  %s\n", "------------", "--------------", "--------------------", "--------------------", "-------")
	for _, n := range notifs {
		shortID := n.ID
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		agentDisplay := n.AgentID
		if len(agentDisplay) > 14 {
			agentDisplay = agentDisplay[:11] + "..."
		}
		timeStr := n.CreatedAt.Format("2006-01-02 15:04")
		msg := n.Message
		if len(msg) > 60 {
			msg = msg[:57] + "..."
		}
		fmt.Printf("%-12s  %-14s  %-20s  %-20s  %s\n", shortID, agentDisplay, truncate(n.Status, 20), timeStr, msg)
	}

	return nil
}

func runNotificationsAck(cmd *cobra.Command, args []string) error {
	hasID := len(args) > 0

	if !hasID && !notificationsAckAll {
		return fmt.Errorf("provide a notification ID or use --all to acknowledge all notifications")
	}
	if hasID && notificationsAckAll {
		return fmt.Errorf("provide either a notification ID or --all, not both")
	}

	_, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if notificationsAckAll {
		if err := client.Notifications().AcknowledgeAll(ctx); err != nil {
			return fmt.Errorf("failed to acknowledge notifications: %w", err)
		}
		fmt.Println("All notifications acknowledged.")
		return nil
	}

	notifID := args[0]
	if err := client.Notifications().Acknowledge(ctx, notifID); err != nil {
		return fmt.Errorf("failed to acknowledge notification: %w", err)
	}
	fmt.Printf("Notification %s acknowledged.\n", notifID)
	return nil
}

func runNotificationsSubscribe(cmd *cobra.Command, args []string) error {
	settings, client, err := requireHubClient()
	if err != nil {
		return err
	}

	projectID, err := resolveProjectID(settings, subscribeProject)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Determine scope
	scope := store.SubscriptionScopeProject
	var agentID string
	if subscribeAgent != "" {
		scope = store.SubscriptionScopeAgent
		agentID, err = resolveAgentIDForSubscription(ctx, client, projectID, subscribeAgent)
		if err != nil {
			return err
		}
	}

	// Parse triggers
	triggers := defaultTriggers
	if subscribeTriggers != "" {
		triggers = strings.Split(subscribeTriggers, ",")
		for i := range triggers {
			triggers[i] = strings.TrimSpace(triggers[i])
		}
	}

	req := &hubclient.CreateSubscriptionRequest{
		Scope:             scope,
		AgentID:           agentID,
		ProjectID:         projectID,
		TriggerActivities: triggers,
	}

	sub, err := client.Subscriptions().Create(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to create subscription: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(sub)
	}

	target := "(all agents)"
	if sub.Scope == store.SubscriptionScopeAgent {
		target = subscribeAgent
	}
	fmt.Printf("Subscribed to %s in project %s\n", target, projectID)
	fmt.Printf("  ID:       %s\n", sub.ID)
	fmt.Printf("  Scope:    %s\n", sub.Scope)
	fmt.Printf("  Triggers: %s\n", strings.Join(sub.TriggerActivities, ", "))
	return nil
}

func runNotificationsUpdate(cmd *cobra.Command, args []string) error {
	_, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	subID := args[0]

	triggers := strings.Split(updateTriggers, ",")
	for i := range triggers {
		triggers[i] = strings.TrimSpace(triggers[i])
	}

	if len(triggers) == 0 {
		return fmt.Errorf("--triggers must specify at least one trigger activity")
	}

	req := &hubclient.UpdateSubscriptionRequest{
		TriggerActivities: triggers,
	}

	sub, err := client.Subscriptions().Update(ctx, subID, req)
	if err != nil {
		return fmt.Errorf("failed to update subscription: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(sub)
	}

	fmt.Printf("Subscription %s updated.\n", sub.ID)
	fmt.Printf("  Triggers: %s\n", strings.Join(sub.TriggerActivities, ", "))
	return nil
}

func runNotificationsUnsubscribe(cmd *cobra.Command, args []string) error {
	hasID := len(args) > 0

	if !hasID && !unsubscribeAll {
		return fmt.Errorf("provide a subscription ID or use --all with --project to remove all subscriptions")
	}
	if hasID && unsubscribeAll {
		return fmt.Errorf("provide either a subscription ID or --all, not both")
	}

	settings, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if unsubscribeAll {
		projectID, err := resolveProjectID(settings, unsubscribeProject)
		if err != nil {
			return fmt.Errorf("--project is required with --all: %w", err)
		}

		// List all subscriptions in the project, then delete each
		subs, err := client.Subscriptions().List(ctx, &hubclient.ListSubscriptionsOptions{
			ProjectID: projectID,
		})
		if err != nil {
			return fmt.Errorf("failed to list subscriptions: %w", err)
		}

		if len(subs) == 0 {
			fmt.Println("No subscriptions found in project.")
			return nil
		}

		for _, sub := range subs {
			if err := client.Subscriptions().Delete(ctx, sub.ID); err != nil {
				return fmt.Errorf("failed to delete subscription %s: %w", sub.ID, err)
			}
		}
		fmt.Printf("Removed %d subscription(s) from project.\n", len(subs))
		return nil
	}

	subID := args[0]
	if err := client.Subscriptions().Delete(ctx, subID); err != nil {
		return fmt.Errorf("failed to delete subscription: %w", err)
	}
	fmt.Printf("Subscription %s removed.\n", subID)
	return nil
}

func runNotificationsSubscriptions(cmd *cobra.Command, args []string) error {
	if subscriptionsJSON {
		outputFormat = "json"
	}

	settings, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	opts := &hubclient.ListSubscriptionsOptions{}
	if subscriptionsProject != "" {
		opts.ProjectID = subscriptionsProject
	} else {
		// Default to current project if available
		projectID := settings.GetHubProjectID()
		if projectID == "" {
			projectID = settings.ProjectID
		}
		if projectID != "" {
			opts.ProjectID = projectID
		}
	}

	subs, err := client.Subscriptions().List(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to list subscriptions: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(subs)
	}

	if len(subs) == 0 {
		fmt.Println("No subscriptions")
		return nil
	}

	fmt.Printf("%-12s  %-6s  %-16s  %-16s  %-40s  %s\n", "ID", "SCOPE", "TARGET", "PROJECT", "TRIGGERS", "CREATED")
	fmt.Printf("%-12s  %-6s  %-16s  %-16s  %-40s  %s\n", "------------", "------", "----------------", "----------------", "----------------------------------------", "----------")
	for _, s := range subs {
		shortID := s.ID
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		target := "(all agents)"
		if s.Scope == store.SubscriptionScopeAgent {
			target = s.AgentID
			if len(target) > 16 {
				target = target[:13] + "..."
			}
		}
		projectDisplay := s.ProjectID
		if len(projectDisplay) > 16 {
			projectDisplay = projectDisplay[:13] + "..."
		}
		triggersStr := strings.Join(s.TriggerActivities, ",")
		if len(triggersStr) > 40 {
			triggersStr = triggersStr[:37] + "..."
		}
		dateStr := s.CreatedAt.Format("2006-01-02")
		fmt.Printf("%-12s  %-6s  %-16s  %-16s  %-40s  %s\n", shortID, s.Scope, target, projectDisplay, triggersStr, dateStr)
	}

	return nil
}
