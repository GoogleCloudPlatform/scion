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

func init() {
	discordThreadsCmd.Flags().StringVar(&discordThreadsChannelID, "channel", "", "Filter to threads in a specific channel")

	discordCmd.AddCommand(discordChannelsCmd)
	discordCmd.AddCommand(discordThreadsCmd)
	rootCmd.AddCommand(discordCmd)
}
