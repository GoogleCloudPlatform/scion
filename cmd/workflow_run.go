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

// workflowRun flags
var (
	workflowRunInputs       []string
	workflowRunInputFile    string
	workflowRunCwd          string
	workflowRunTraceDir     string
	workflowRunEventBackend string
	workflowRunVerbose      bool
	workflowRunQuiet        bool
	workflowRunLocal        bool
)

// workflowRunCmd runs a duckflux workflow file locally via quack.
var workflowRunCmd = &cobra.Command{
	Use:   "run <file.duck.yaml>",
	Short: "Run a duckflux workflow locally via quack",
	Long: `Execute a duckflux workflow file by delegating to the quack CLI.

quack must be available on PATH. Exit codes are propagated directly:
  0  workflow completed successfully
  1  CLI/usage error (e.g. missing file, bad flags)
  2  workflow executed but ended with success=false

Examples:
  scion workflow run flow.duck.yaml
  scion workflow run flow.duck.yaml --input name=world --input count=3
  scion workflow run flow.duck.yaml --input-file inputs.json --trace-dir ./trace`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkflowRun,
}

func runWorkflowRun(cmd *cobra.Command, args []string) error {
	file := args[0]

	if err := workflow.ValidateInputFlags(workflowRunInputs); err != nil {
		return fmt.Errorf("invalid --input flag: %w", err)
	}

	req := workflow.RunLocalRequest{
		File:         file,
		Inputs:       workflowRunInputs,
		InputFile:    workflowRunInputFile,
		Cwd:          workflowRunCwd,
		TraceDir:     workflowRunTraceDir,
		EventBackend: workflowRunEventBackend,
		Verbose:      workflowRunVerbose,
		Quiet:        workflowRunQuiet,
		// Stdio left nil: RunLocal substitutes os.Stdin/Stdout/Stderr.
	}

	result, err := workflow.RunLocal(context.Background(), req)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		os.Exit(result.ExitCode)
	}
	return nil
}

func init() {
	workflowCmd.AddCommand(workflowRunCmd)

	workflowRunCmd.Flags().StringArrayVar(&workflowRunInputs, "input", nil, "Input key=value pair (repeatable); highest precedence over --input-file")
	workflowRunCmd.Flags().StringVar(&workflowRunInputFile, "input-file", "", "Path to a JSON input envelope file")
	workflowRunCmd.Flags().StringVar(&workflowRunCwd, "cwd", "", "Working directory for exec participants")
	workflowRunCmd.Flags().StringVar(&workflowRunTraceDir, "trace-dir", "", "Directory for structured trace output")
	workflowRunCmd.Flags().StringVar(&workflowRunEventBackend, "event-backend", "memory", "Event hub backend: memory, nats, or redis")
	workflowRunCmd.Flags().BoolVarP(&workflowRunVerbose, "verbose", "v", false, "Enable extra diagnostic output")
	workflowRunCmd.Flags().BoolVarP(&workflowRunQuiet, "quiet", "q", false, "Suppress info logs on stderr")
	workflowRunCmd.Flags().BoolVar(&workflowRunLocal, "local", true, "Force local subprocess dispatch (always on in Phase 1; reserved for future --hub default)")
}
