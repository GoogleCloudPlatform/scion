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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var discordCmd = &cobra.Command{
	Use:   "discord",
	Short: "Interact with Discord channels and users",
	Long:  "Commands for querying Discord state linked to the current project.",
}

var discordChannelsCmd = &cobra.Command{
	Use:   "channels",
	Short: "List linked Discord channels",
	RunE:  runDiscordChannels,
}

var discordThreadsChannelID string

var discordThreadsCmd = &cobra.Command{
	Use:   "threads",
	Short: "List thread-agent mappings",
	RunE:  runDiscordThreads,
}

func runDiscordChannels(cmd *cobra.Command, args []string) error {
	hubCtx, err := CheckHubAvailability(projectPath)
	if err != nil {
		return err
	}
	PrintUsingHub(hubCtx.Endpoint)

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := hubCtx.Client.Discord(projectID).ListChannels(ctx)
	if err != nil {
		return wrapHubError(err)
	}

	fmt.Fprintln(os.Stdout, string(result))
	return nil
}

func runDiscordThreads(cmd *cobra.Command, args []string) error {
	hubCtx, err := CheckHubAvailability(projectPath)
	if err != nil {
		return err
	}
	PrintUsingHub(hubCtx.Endpoint)

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := hubCtx.Client.Discord(projectID).ListThreads(ctx, discordThreadsChannelID)
	if err != nil {
		return wrapHubError(err)
	}

	fmt.Fprintln(os.Stdout, string(result))
	return nil
}

var discordSetDefaultThreadID string

var discordSetDefaultCmd = &cobra.Command{
	Use:   "set-default <channel-id> <agent-slug>",
	Short: "Set default agent for a channel or thread",
	Args:  cobra.ExactArgs(2),
	RunE:  runDiscordSetDefault,
}

func runDiscordSetDefault(cmd *cobra.Command, args []string) error {
	channelID, agentSlug := args[0], args[1]

	hubCtx, err := CheckHubAvailability(projectPath)
	if err != nil {
		return err
	}
	PrintUsingHub(hubCtx.Endpoint)

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req := hubclient.SetDefaultRequest{
		ChannelID: channelID,
		ThreadID:  discordSetDefaultThreadID,
		AgentSlug: agentSlug,
	}
	result, err := hubCtx.Client.Discord(projectID).SetDefault(ctx, req)
	if err != nil {
		return wrapHubError(err)
	}

	fmt.Fprintln(os.Stdout, string(result))
	return nil
}

var (
	historyLimit      int
	historyBefore     string
	historyAfter      string
	historyHumansOnly bool
)

var discordHistoryCmd = &cobra.Command{
	Use:   "history <channel-id>",
	Short: "Fetch recent messages from a channel",
	Args:  cobra.ExactArgs(1),
	RunE:  runDiscordHistory,
}

func runDiscordHistory(cmd *cobra.Command, args []string) error {
	channelID := args[0]

	if historyLimit > 100 {
		historyLimit = 100
	}
	if historyLimit < 0 {
		historyLimit = 25
	}

	hubCtx, err := CheckHubAvailability(projectPath)
	if err != nil {
		return err
	}
	PrintUsingHub(hubCtx.Endpoint)

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	opts := hubclient.HistoryOptions{
		Limit:      historyLimit,
		Before:     historyBefore,
		After:      historyAfter,
		HumansOnly: historyHumansOnly,
	}
	result, err := hubCtx.Client.Discord(projectID).ChannelHistory(ctx, channelID, opts)
	if err != nil {
		return wrapHubError(err)
	}

	fmt.Fprintln(os.Stdout, string(result))
	return nil
}

var discordDmCmd = &cobra.Command{
	Use:   "dm <user-email> <message>",
	Short: "Send a DM to a registered user",
	Args:  cobra.ExactArgs(2),
	RunE:  runDiscordDM,
}

func runDiscordDM(cmd *cobra.Command, args []string) error {
	email, message := args[0], args[1]

	hubCtx, err := CheckHubAvailability(projectPath)
	if err != nil {
		return err
	}
	PrintUsingHub(hubCtx.Endpoint)

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req := hubclient.SendDMRequest{
		RecipientEmail: email,
		Message:        message,
	}
	result, err := hubCtx.Client.Discord(projectID).SendDM(ctx, req)
	if err != nil {
		return wrapHubError(err)
	}

	fmt.Fprintln(os.Stdout, string(result))
	return nil
}

func init() {
	discordThreadsCmd.Flags().StringVar(&discordThreadsChannelID, "channel", "", "Filter to threads in a specific channel")

	discordSetDefaultCmd.Flags().StringVar(&discordSetDefaultThreadID, "thread", "", "Thread ID for thread-level default")
	discordHistoryCmd.Flags().IntVar(&historyLimit, "limit", 25, "Number of messages to return (max 100)")
	discordHistoryCmd.Flags().StringVar(&historyBefore, "before", "", "Return messages before this message ID")
	discordHistoryCmd.Flags().StringVar(&historyAfter, "after", "", "Return messages after this message ID")
	discordHistoryCmd.Flags().BoolVar(&historyHumansOnly, "humans-only", false, "Exclude bot messages")

	discordCmd.AddCommand(discordChannelsCmd)
	discordCmd.AddCommand(discordThreadsCmd)
	discordCmd.AddCommand(discordSetDefaultCmd)
	discordCmd.AddCommand(discordHistoryCmd)
	discordCmd.AddCommand(discordDmCmd)
	rootCmd.AddCommand(discordCmd)
}
