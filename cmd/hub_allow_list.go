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
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var (
	allowListOutputJSON bool
	allowListAddNote    string
	allowListImportNote string
)

var hubAllowListCmd = &cobra.Command{
	Use:   "allow-list",
	Short: "Manage the user allow list",
	Long: `Manage the email allow list for invite-only access mode.

When user_access_mode is set to "invite_only", only emails on this
allow list (plus admin emails) are permitted to log in.

Examples:
  scion hub allow-list
  scion hub allow-list add alice@example.com --note "New hire"
  scion hub allow-list remove alice@example.com`,
	Args: cobra.NoArgs,
	RunE: runAllowListList,
}

var hubAllowListAddCmd = &cobra.Command{
	Use:   "add EMAIL",
	Short: "Add an email to the allow list",
	Long: `Add an email address to the allow list.

Examples:
  scion hub allow-list add alice@example.com
  scion hub allow-list add bob@example.com --note "Contractor, Q3"`,
	Args: cobra.ExactArgs(1),
	RunE: runAllowListAdd,
}

var hubAllowListRemoveCmd = &cobra.Command{
	Use:   "remove EMAIL",
	Short: "Remove an email from the allow list",
	Long: `Remove an email address from the allow list.

Examples:
  scion hub allow-list remove alice@example.com`,
	Args: cobra.ExactArgs(1),
	RunE: runAllowListRemove,
}

var hubAllowListListCmd = &cobra.Command{
	Use:   "list",
	Short: "List allow list entries",
	Long: `List all email addresses on the allow list.

Examples:
  scion hub allow-list list
  scion hub allow-list list --json`,
	Args: cobra.NoArgs,
	RunE: runAllowListList,
}

var hubAllowListImportCmd = &cobra.Command{
	Use:   "import FILE",
	Short: "Bulk import emails from a CSV file",
	Long: `Import email addresses from a CSV file into the allow list.

The CSV file should have one email per line, with an optional second column for notes.
A header row (starting with "email") is automatically skipped.

Examples:
  scion hub allow-list import emails.csv
  scion hub allow-list import emails.csv --note "Q3 batch import"`,
	Args: cobra.ExactArgs(1),
	RunE: runAllowListImport,
}

func init() {
	hubCmd.AddCommand(hubAllowListCmd)
	hubAllowListCmd.AddCommand(hubAllowListAddCmd)
	hubAllowListCmd.AddCommand(hubAllowListRemoveCmd)
	hubAllowListCmd.AddCommand(hubAllowListListCmd)
	hubAllowListCmd.AddCommand(hubAllowListImportCmd)

	hubAllowListAddCmd.Flags().StringVar(&allowListAddNote, "note", "", "Optional note for this entry")
	hubAllowListImportCmd.Flags().StringVar(&allowListImportNote, "note", "", "Note to apply to all imported entries")

	hubAllowListCmd.PersistentFlags().BoolVar(&allowListOutputJSON, "json", false, "Output in JSON format")
}

func runAllowListList(cmd *cobra.Command, args []string) error {
	resolvedPath, _, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		return fmt.Errorf("failed to resolve project path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	client, err := getHubClient(settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.AllowList().List(ctx, "")
	if err != nil {
		return fmt.Errorf("failed to list allow list: %w", err)
	}

	if allowListOutputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}

	if len(resp.Items) == 0 {
		fmt.Println("No entries in the allow list.")
		return nil
	}

	fmt.Printf("%-40s %-30s %s\n", "EMAIL", "ADDED", "NOTE")
	for _, entry := range resp.Items {
		fmt.Printf("%-40s %-30s %s\n",
			entry.Email,
			entry.Created.Format(time.RFC3339),
			entry.Note,
		)
	}
	fmt.Printf("\nTotal: %d entries\n", resp.TotalCount)

	return nil
}

func runAllowListAdd(cmd *cobra.Command, args []string) error {
	email := args[0]

	resolvedPath, _, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		return fmt.Errorf("failed to resolve project path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	client, err := getHubClient(settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	entry, err := client.AllowList().Add(ctx, email, allowListAddNote)
	if err != nil {
		return fmt.Errorf("failed to add to allow list: %w", err)
	}

	if allowListOutputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(entry)
	}

	fmt.Printf("Added %s to the allow list.\n", entry.Email)
	return nil
}

func runAllowListRemove(cmd *cobra.Command, args []string) error {
	email := args[0]

	resolvedPath, _, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		return fmt.Errorf("failed to resolve project path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	client, err := getHubClient(settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.AllowList().Remove(ctx, email); err != nil {
		return fmt.Errorf("failed to remove from allow list: %w", err)
	}

	fmt.Printf("Removed %s from the allow list.\n", email)
	return nil
}

func runAllowListImport(cmd *cobra.Command, args []string) error {
	filePath := args[0]

	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close()

	emails, err := parseImportCSV(f)
	if err != nil {
		return fmt.Errorf("failed to parse CSV: %w", err)
	}

	if len(emails) == 0 {
		fmt.Println("No valid emails found in the file.")
		return nil
	}

	// Apply the --note flag to entries that don't already have a note from the CSV
	if allowListImportNote != "" {
		for i := range emails {
			if emails[i].Note == "" {
				emails[i].Note = allowListImportNote
			}
		}
	}

	resolvedPath, _, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		return fmt.Errorf("failed to resolve project path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	client, err := getHubClient(settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := client.AllowList().BulkAdd(ctx, emails)
	if err != nil {
		return fmt.Errorf("failed to import: %w", err)
	}

	if allowListOutputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}

	fmt.Printf("Import complete: %d added, %d skipped (already on list), %d total processed.\n",
		resp.Added, resp.Skipped, resp.Total)
	return nil
}

func parseImportCSV(r io.Reader) ([]hubclient.AllowListAddRequest, error) {
	reader := csv.NewReader(bufio.NewReader(r))
	reader.TrimLeadingSpace = true
	reader.FieldsPerRecord = -1

	var emails []hubclient.AllowListAddRequest
	lineNum := 0
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNum+1, err)
		}
		lineNum++

		if len(record) == 0 {
			continue
		}

		email := strings.TrimSpace(record[0])

		if lineNum == 1 && (strings.EqualFold(email, "email") || strings.EqualFold(email, "e-mail")) {
			continue
		}

		if email == "" || !strings.Contains(email, "@") {
			continue
		}

		var note string
		if len(record) > 1 {
			note = strings.TrimSpace(record[1])
		}

		emails = append(emails, hubclient.AllowListAddRequest{Email: email, Note: note})
	}

	return emails, nil
}
