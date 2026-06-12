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
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var registriesCmd = &cobra.Command{
	Use:   "registries",
	Short: "Manage external skill registries",
	Long:  `List, add, show, update, remove, and pin hashes for external skill registries.`,
}

var registriesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured skill registries",
	RunE:  runRegistriesList,
}

func runRegistriesList(cmd *cobra.Command, args []string) error {
	hubCtx, err := CheckHubAvailability(projectPath)
	if err != nil {
		return fmt.Errorf("Hub connection required: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := hubCtx.Client.SkillRegistries().List(ctx)
	if err != nil {
		return fmt.Errorf("failed to list registries: %w", err)
	}

	if isJSONOutput() {
		return json.NewEncoder(os.Stdout).Encode(resp.Items)
	}

	if len(resp.Items) == 0 {
		fmt.Println("No skill registries configured.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTYPE\tTRUST\tSTATUS\tENDPOINT")
	for _, r := range resp.Items {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			r.Name, r.Type, r.TrustLevel, r.Status, r.Endpoint)
	}
	return w.Flush()
}

var registriesAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Add an external skill registry",
	Args:  cobra.ExactArgs(1),
	RunE:  runRegistriesAdd,
}

func runRegistriesAdd(cmd *cobra.Command, args []string) error {
	hubCtx, err := CheckHubAvailability(projectPath)
	if err != nil {
		return fmt.Errorf("Hub connection required: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	endpoint, _ := cmd.Flags().GetString("endpoint")
	if endpoint == "" {
		return fmt.Errorf("--endpoint is required")
	}

	trust, _ := cmd.Flags().GetString("trust")
	regType, _ := cmd.Flags().GetString("type")
	description, _ := cmd.Flags().GetString("description")
	authToken, _ := cmd.Flags().GetString("auth-token")
	resolvePath, _ := cmd.Flags().GetString("resolve-path")

	req := &hubclient.CreateSkillRegistryRequest{
		Name:        args[0],
		Endpoint:    endpoint,
		Description: description,
		Type:        regType,
		TrustLevel:  trust,
		AuthToken:   authToken,
		ResolvePath: resolvePath,
	}

	registry, err := hubCtx.Client.SkillRegistries().Create(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to create registry: %w", err)
	}

	if isJSONOutput() {
		return json.NewEncoder(os.Stdout).Encode(registry)
	}

	fmt.Printf("Registry %q created (id: %s)\n", registry.Name, registry.ID)
	return nil
}

var registriesShowCmd = &cobra.Command{
	Use:   "show <name-or-id>",
	Short: "Show details of a skill registry",
	Args:  cobra.ExactArgs(1),
	RunE:  runRegistriesShow,
}

func runRegistriesShow(cmd *cobra.Command, args []string) error {
	hubCtx, err := CheckHubAvailability(projectPath)
	if err != nil {
		return fmt.Errorf("Hub connection required: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	registry, err := hubCtx.Client.SkillRegistries().Get(ctx, args[0])
	if err != nil {
		return fmt.Errorf("failed to get registry: %w", err)
	}

	if isJSONOutput() {
		return json.NewEncoder(os.Stdout).Encode(registry)
	}

	fmt.Printf("Name:        %s\n", registry.Name)
	fmt.Printf("ID:          %s\n", registry.ID)
	fmt.Printf("Endpoint:    %s\n", registry.Endpoint)
	fmt.Printf("Type:        %s\n", registry.Type)
	fmt.Printf("Trust Level: %s\n", registry.TrustLevel)
	fmt.Printf("Status:      %s\n", registry.Status)
	if registry.ResolvePath != "" {
		fmt.Printf("Resolve Path: %s\n", registry.ResolvePath)
	}
	if registry.Description != "" {
		fmt.Printf("Description: %s\n", registry.Description)
	}
	fmt.Printf("Created:     %s\n", registry.Created.Format(time.RFC3339))
	fmt.Printf("Updated:     %s\n", registry.Updated.Format(time.RFC3339))
	return nil
}

var registriesUpdateCmd = &cobra.Command{
	Use:   "update <name-or-id>",
	Short: "Update a skill registry",
	Args:  cobra.ExactArgs(1),
	RunE:  runRegistriesUpdate,
}

func runRegistriesUpdate(cmd *cobra.Command, args []string) error {
	hubCtx, err := CheckHubAvailability(projectPath)
	if err != nil {
		return fmt.Errorf("Hub connection required: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	endpoint, _ := cmd.Flags().GetString("endpoint")
	trust, _ := cmd.Flags().GetString("trust")
	status, _ := cmd.Flags().GetString("status")
	description, _ := cmd.Flags().GetString("description")
	authToken, _ := cmd.Flags().GetString("auth-token")
	resolvePath, _ := cmd.Flags().GetString("resolve-path")

	req := &hubclient.UpdateSkillRegistryRequest{
		Endpoint:    endpoint,
		TrustLevel:  trust,
		Status:      status,
		Description: description,
		AuthToken:   authToken,
		ResolvePath: resolvePath,
	}

	registry, err := hubCtx.Client.SkillRegistries().Update(ctx, args[0], req)
	if err != nil {
		return fmt.Errorf("failed to update registry: %w", err)
	}

	if isJSONOutput() {
		return json.NewEncoder(os.Stdout).Encode(registry)
	}

	fmt.Printf("Registry %q updated\n", registry.Name)
	return nil
}

var registriesRemoveCmd = &cobra.Command{
	Use:   "remove <name-or-id>",
	Short: "Remove a skill registry",
	Args:  cobra.ExactArgs(1),
	RunE:  runRegistriesRemove,
}

func runRegistriesRemove(cmd *cobra.Command, args []string) error {
	hubCtx, err := CheckHubAvailability(projectPath)
	if err != nil {
		return fmt.Errorf("Hub connection required: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := hubCtx.Client.SkillRegistries().Delete(ctx, args[0]); err != nil {
		return fmt.Errorf("failed to remove registry: %w", err)
	}

	if isJSONOutput() {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "deleted"})
	}

	fmt.Printf("Registry %q removed\n", args[0])
	return nil
}

var registriesPinCmd = &cobra.Command{
	Use:   "pin <name-or-id> <skill-uri>",
	Short: "Pin a skill hash for a registry",
	Args:  cobra.ExactArgs(2),
	RunE:  runRegistriesPin,
}

func runRegistriesPin(cmd *cobra.Command, args []string) error {
	hubCtx, err := CheckHubAvailability(projectPath)
	if err != nil {
		return fmt.Errorf("Hub connection required: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	hash, _ := cmd.Flags().GetString("hash")
	if hash == "" {
		return fmt.Errorf("--hash is required")
	}

	req := &hubclient.PinSkillHashRequest{
		URI:  args[1],
		Hash: hash,
	}

	if err := hubCtx.Client.SkillRegistries().Pin(ctx, args[0], req); err != nil {
		return fmt.Errorf("failed to pin hash: %w", err)
	}

	if isJSONOutput() {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{
			"status": "pinned",
			"uri":    args[1],
			"hash":   hash,
		})
	}

	fmt.Printf("Pinned %s → %s\n", args[1], hash)
	return nil
}

func init() {
	skillsCmd.AddCommand(registriesCmd)
	registriesCmd.AddCommand(registriesListCmd)
	registriesCmd.AddCommand(registriesAddCmd)
	registriesCmd.AddCommand(registriesShowCmd)
	registriesCmd.AddCommand(registriesUpdateCmd)
	registriesCmd.AddCommand(registriesRemoveCmd)
	registriesCmd.AddCommand(registriesPinCmd)

	// Flags for add command
	registriesAddCmd.Flags().String("endpoint", "", "Registry endpoint URL (required, HTTPS)")
	registriesAddCmd.Flags().String("trust", "pinned", "Trust level: trusted or pinned")
	registriesAddCmd.Flags().String("type", "hub", "Registry type: hub or gcp")
	registriesAddCmd.Flags().String("description", "", "Description of the registry")
	registriesAddCmd.Flags().String("auth-token", "", "Authentication token for the registry")
	registriesAddCmd.Flags().String("resolve-path", "", "Custom resolve endpoint path")

	// Flags for update command
	registriesUpdateCmd.Flags().String("endpoint", "", "Registry endpoint URL (HTTPS)")
	registriesUpdateCmd.Flags().String("trust", "", "Trust level: trusted or pinned")
	registriesUpdateCmd.Flags().String("status", "", "Status: active or disabled")
	registriesUpdateCmd.Flags().String("description", "", "Description of the registry")
	registriesUpdateCmd.Flags().String("auth-token", "", "Authentication token for the registry")
	registriesUpdateCmd.Flags().String("resolve-path", "", "Custom resolve endpoint path")

	// Flags for pin command
	registriesPinCmd.Flags().String("hash", "", "Content hash to pin (required, e.g., sha256:...)")
}
