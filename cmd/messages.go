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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var (
	messagesShowAll bool
	messagesJSON    bool
	messagesReadAll bool
	messagesAgent   string
)

// messagesCmd is the top-level command group for message inbox management.
var messagesCmd = &cobra.Command{
	Use:     "messages",
	Aliases: []string{"msgs", "inbox"},
	Short:   "View messages from agents",
	Long: `View and manage messages sent to you by agents.

Messages require Hub mode. Enable with 'scion hub enable <endpoint>'.

Commands:
  scion messages                            List your unread messages
  scion messages --all                      List all messages (including read)
  scion messages --agent <name>             Filter by agent
  scion messages read [id]                  Mark message(s) as read
  scion messages read --all                 Mark all messages as read`,
	RunE: runMessagesList,
}

// messagesReadCmd marks messages as read.
var messagesReadCmd = &cobra.Command{
	Use:   "read [message-id]",
	Short: "Mark message(s) as read",
	Long: `Mark one or all messages as read.

With an ID argument, marks that specific message as read.
With --all flag, marks all unread messages as read.

Examples:
  scion messages read a1b2c3d4
  scion messages read --all`,
	RunE: runMessagesRead,
}

func init() {
	rootCmd.AddCommand(messagesCmd)
	messagesCmd.AddCommand(messagesReadCmd)

	// List flags (on the parent command)
	messagesCmd.Flags().BoolVar(&messagesShowAll, "all", false, "Include read messages")
	messagesCmd.Flags().BoolVar(&messagesJSON, "json", false, "Output in JSON format")
	messagesCmd.Flags().StringVar(&messagesAgent, "agent", "", "Filter by agent name or ID")

	// Read flags
	messagesReadCmd.Flags().BoolVar(&messagesReadAll, "all", false, "Mark all messages as read")
}

func runMessagesList(cmd *cobra.Command, args []string) error {
	if messagesJSON {
		outputFormat = "json"
	}

	_, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	opts := &hubclient.ListMessagesOptions{
		OnlyUnread: !messagesShowAll,
		AgentID:    messagesAgent,
	}

	result, err := client.Messages().List(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to list messages: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(result)
	}

	msgs := result.Items
	if len(msgs) == 0 {
		fmt.Println("No messages")
		return nil
	}

	fmt.Printf("%-12s  %-14s  %-14s  %-20s  %s\n", "ID", "AGENT", "TYPE", "TIME", "MESSAGE")
	fmt.Printf("%-12s  %-14s  %-14s  %-20s  %s\n",
		"------------", "--------------", "--------------", "--------------------", "-------")
	for _, m := range msgs {
		shortID := m.ID
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		agentDisplay := m.AgentID
		if len(agentDisplay) > 14 {
			agentDisplay = agentDisplay[:11] + "..."
		}
		timeStr := m.CreatedAt.Format("2006-01-02 15:04")
		msgText := m.Msg
		if len(msgText) > 60 {
			msgText = msgText[:57] + "..."
		}
		dispatchInfo := ""
		if m.DispatchState == "failed" {
			dispatchInfo = " [DELIVERY FAILED"
			if m.DispatchFailureReason != nil && *m.DispatchFailureReason != "" {
				dispatchInfo += ": " + *m.DispatchFailureReason
			}
			dispatchInfo += "]"
		}
		fmt.Printf("%-12s  %-14s  %-14s  %-20s  %s%s\n",
			shortID, agentDisplay, truncate(m.Type, 14), timeStr, msgText, dispatchInfo)
	}

	return nil
}

func runMessagesRead(cmd *cobra.Command, args []string) error {
	hasID := len(args) > 0

	if !hasID && !messagesReadAll {
		return fmt.Errorf("provide a message ID or use --all to mark all messages as read")
	}
	if hasID && messagesReadAll {
		return fmt.Errorf("provide either a message ID or --all, not both")
	}

	_, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if messagesReadAll {
		if err := client.Messages().MarkAllRead(ctx); err != nil {
			return fmt.Errorf("failed to mark messages as read: %w", err)
		}
		fmt.Println("All messages marked as read.")
		return nil
	}

	msgID := args[0]
	if err := client.Messages().MarkRead(ctx, msgID); err != nil {
		return fmt.Errorf("failed to mark message as read: %w", err)
	}
	fmt.Printf("Message %s marked as read.\n", msgID)
	return nil
}
