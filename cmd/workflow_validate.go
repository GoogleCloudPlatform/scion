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

	"github.com/GoogleCloudPlatform/scion/pkg/workflow"
	"github.com/spf13/cobra"
)

// workflowValidate flags
var (
	workflowValidateInputs    []string
	workflowValidateInputFile string
)

// workflowValidateCmd validates a duckflux workflow file via quack.
var workflowValidateCmd = &cobra.Command{
	Use:   "validate <file.duck.yaml>",
	Short: "Validate a duckflux workflow file via quack",
	Long: `Validate a duckflux workflow file by delegating to 'quack validate'.

Validation checks schema correctness, semantic rules, and optionally
validates declared inputs against provided values.

quack must be available on PATH. Exit codes:
  0  workflow is valid
  1  validation failed (schema, semantic, or input error)

Examples:
  scion workflow validate flow.duck.yaml
  scion workflow validate flow.duck.yaml --input name=world
  scion workflow validate flow.duck.yaml --input-file inputs.json`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkflowValidate,
}

func runWorkflowValidate(cmd *cobra.Command, args []string) error {
	file := args[0]

	if err := workflow.ValidateInputFlags(workflowValidateInputs); err != nil {
		return fmt.Errorf("invalid --input flag: %w", err)
	}

	req := workflow.RunLocalRequest{
		File:      file,
		Inputs:    workflowValidateInputs,
		InputFile: workflowValidateInputFile,
		// Stdio left nil: ValidateLocal substitutes os.Stdin/Stdout/Stderr.
	}

	result, err := workflow.ValidateLocal(context.Background(), req)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		os.Exit(result.ExitCode)
	}
	return nil
}

func init() {
	workflowCmd.AddCommand(workflowValidateCmd)

	workflowValidateCmd.Flags().StringArrayVar(&workflowValidateInputs, "input", nil, "Input key=value pair (repeatable)")
	workflowValidateCmd.Flags().StringVar(&workflowValidateInputFile, "input-file", "", "Path to a JSON input envelope file")
}
