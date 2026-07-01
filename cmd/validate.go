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

var templatesValidateCmd = &cobra.Command{
	Use:   "validate [name]",
	Short: "Validate storage consistency for a template",
	Long: `Check that all files listed in a template's database record exist in storage.

Reports PASS or FAIL with a list of issues found. Use --all to validate
all templates visible to the current user.

Examples:
  scion template validate default
  scion template validate --all`,
	Args: cobra.MaximumNArgs(1),
	RunE: runTemplateValidate,
}

func runTemplateValidate(cmd *cobra.Command, args []string) error {
	validateAll, _ := cmd.Flags().GetBool("all")

	if !validateAll && len(args) == 0 {
		return fmt.Errorf("requires a template name argument or --all flag")
	}
	if validateAll && len(args) > 0 {
		return fmt.Errorf("cannot specify both a template name and --all")
	}

	hubCtx, err := CheckHubAvailability(projectPath)
	if err != nil {
		return err
	}
	if hubCtx == nil {
		return fmt.Errorf("Hub integration is not enabled. Use 'scion hub enable' first")
	}

	PrintUsingHub(hubCtx.Endpoint)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if validateAll {
		return validateAllTemplates(ctx, hubCtx)
	}

	return validateTemplate(ctx, hubCtx, args[0])
}

func validateTemplate(ctx context.Context, hubCtx *HubContext, name string) error {
	resp, err := hubCtx.Client.Templates().List(ctx, &hubclient.ListTemplatesOptions{
		Name:   name,
		Status: "active",
	})
	if err != nil {
		return fmt.Errorf("failed to list templates: %w", err)
	}

	var match *hubclient.Template
	for i := range resp.Templates {
		if resp.Templates[i].Name == name || resp.Templates[i].Slug == name {
			match = &resp.Templates[i]
			break
		}
	}
	if match == nil {
		return fmt.Errorf("template %q not found on Hub", name)
	}

	report, err := hubCtx.Client.Templates().Validate(ctx, match.ID)
	if err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	return printValidationReport(name, report)
}

func validateAllTemplates(ctx context.Context, hubCtx *HubContext) error {
	resp, err := hubCtx.Client.Templates().List(ctx, &hubclient.ListTemplatesOptions{
		Status: "active",
	})
	if err != nil {
		return fmt.Errorf("failed to list templates: %w", err)
	}

	if len(resp.Templates) == 0 {
		fmt.Println("No templates found.")
		return nil
	}

	var passed, failed int
	for _, t := range resp.Templates {
		report, err := hubCtx.Client.Templates().Validate(ctx, t.ID)
		if err != nil {
			fmt.Printf("FAIL  %s  (error: %v)\n", t.Name, err)
			failed++
			continue
		}
		if len(report.Issues) == 0 {
			fmt.Printf("PASS  %s\n", t.Name)
			passed++
		} else {
			fmt.Printf("FAIL  %s\n", t.Name)
			for _, issue := range report.Issues {
				fmt.Printf("  - [%s] %s\n", issue.Kind, issue.Message)
			}
			failed++
		}
	}

	fmt.Printf("\n%d passed, %d failed\n", passed, failed)
	if failed > 0 {
		return fmt.Errorf("%d template(s) failed validation", failed)
	}
	return nil
}

var harnessConfigValidateCmd = &cobra.Command{
	Use:   "validate [name]",
	Short: "Validate storage consistency for a harness-config",
	Long: `Check that all files listed in a harness-config's database record exist in storage.

Reports PASS or FAIL with a list of issues found. Use --all to validate
all harness-configs visible to the current user.

Examples:
  scion harness-config validate claude
  scion harness-config validate --all`,
	Args: cobra.MaximumNArgs(1),
	RunE: runHarnessConfigValidate,
}

func runHarnessConfigValidate(cmd *cobra.Command, args []string) error {
	validateAll, _ := cmd.Flags().GetBool("all")

	if !validateAll && len(args) == 0 {
		return fmt.Errorf("requires a harness-config name argument or --all flag")
	}
	if validateAll && len(args) > 0 {
		return fmt.Errorf("cannot specify both a harness-config name and --all")
	}

	hubCtx, err := CheckHubAvailability(projectPath)
	if err != nil {
		return err
	}
	if hubCtx == nil {
		return fmt.Errorf("Hub integration is not enabled. Use 'scion hub enable' first")
	}

	PrintUsingHub(hubCtx.Endpoint)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if validateAll {
		return validateAllHarnessConfigs(ctx, hubCtx)
	}

	return validateHarnessConfig(ctx, hubCtx, args[0])
}

func validateHarnessConfig(ctx context.Context, hubCtx *HubContext, name string) error {
	resp, err := hubCtx.Client.HarnessConfigs().List(ctx, &hubclient.ListHarnessConfigsOptions{
		Name:   name,
		Status: "active",
	})
	if err != nil {
		return fmt.Errorf("failed to list harness-configs: %w", err)
	}

	var match *hubclient.HarnessConfig
	for i := range resp.HarnessConfigs {
		if resp.HarnessConfigs[i].Name == name || resp.HarnessConfigs[i].Slug == name {
			match = &resp.HarnessConfigs[i]
			break
		}
	}
	if match == nil {
		return fmt.Errorf("harness-config %q not found on Hub", name)
	}

	report, err := hubCtx.Client.HarnessConfigs().Validate(ctx, match.ID)
	if err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	return printValidationReport(name, report)
}

func validateAllHarnessConfigs(ctx context.Context, hubCtx *HubContext) error {
	resp, err := hubCtx.Client.HarnessConfigs().List(ctx, &hubclient.ListHarnessConfigsOptions{
		Status: "active",
	})
	if err != nil {
		return fmt.Errorf("failed to list harness-configs: %w", err)
	}

	if len(resp.HarnessConfigs) == 0 {
		fmt.Println("No harness-configs found.")
		return nil
	}

	var passed, failed int
	for _, hc := range resp.HarnessConfigs {
		report, err := hubCtx.Client.HarnessConfigs().Validate(ctx, hc.ID)
		if err != nil {
			fmt.Printf("FAIL  %s  (error: %v)\n", hc.Name, err)
			failed++
			continue
		}
		if len(report.Issues) == 0 {
			fmt.Printf("PASS  %s\n", hc.Name)
			passed++
		} else {
			fmt.Printf("FAIL  %s\n", hc.Name)
			for _, issue := range report.Issues {
				fmt.Printf("  - [%s] %s\n", issue.Kind, issue.Message)
			}
			failed++
		}
	}

	fmt.Printf("\n%d passed, %d failed\n", passed, failed)
	if failed > 0 {
		return fmt.Errorf("%d harness-config(s) failed validation", failed)
	}
	return nil
}

func printValidationReport(name string, report *hubclient.ValidationReport) error {
	if isJSONOutput() {
		return outputJSON(report)
	}

	if len(report.Issues) == 0 {
		fmt.Printf("PASS  %s\n", name)
		return nil
	}

	fmt.Printf("FAIL  %s\n", name)
	for _, issue := range report.Issues {
		if issue.File != "" {
			fmt.Printf("  - [%s] %s: %s\n", issue.Kind, issue.File, issue.Message)
		} else {
			fmt.Printf("  - [%s] %s\n", issue.Kind, issue.Message)
		}
	}

	if report.ResourceKind == "template" {
		fmt.Printf("\nRun 'scion template sync %s' to repair.\n", name)
	} else if report.ResourceKind == "harness-config" {
		fmt.Printf("\nRun 'scion harness-config sync %s' to repair.\n", name)
	}

	return fmt.Errorf("%s failed validation with %d issue(s)", name, len(report.Issues))
}

func init() {
	templatesCmd.AddCommand(templatesValidateCmd)
	templatesValidateCmd.Flags().Bool("all", false, "Validate all templates")

	harnessConfigCmd.AddCommand(harnessConfigValidateCmd)
	harnessConfigValidateCmd.Flags().Bool("all", false, "Validate all harness-configs")

	// Add validate to singular 'template' alias
	templateValidateAlias := &cobra.Command{
		Use:   "validate [name]",
		Short: "Validate storage consistency for a template",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runTemplateValidate,
	}
	templateValidateAlias.Flags().Bool("all", false, "Validate all templates")

	// Find the singular 'template' command and add to it
	for _, cmd := range rootCmd.Commands() {
		if cmd.Use == "template" {
			cmd.AddCommand(templateValidateAlias)
			break
		}
	}
}
