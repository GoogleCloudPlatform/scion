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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
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
	workflowRunViaHub       bool
	workflowRunWait         *bool // nil means auto-detect via TTY
	workflowRunGroveID      string
)

// workflowRunCmd runs a duckflux workflow file locally via quack or via the Hub.
var workflowRunCmd = &cobra.Command{
	Use:   "run <file.duck.yaml>",
	Short: "Run a duckflux workflow locally or via the Hub",
	Long: `Execute a duckflux workflow file.

Without --via-hub, delegates directly to the local quack CLI (Phase 1 behavior):
  quack must be available on PATH. Exit codes are propagated directly:
    0  workflow completed successfully
    1  CLI/usage error (e.g. missing file, bad flags)
    2  workflow executed but ended with success=false

With --via-hub, the workflow is dispatched to the configured Hub instead of
running locally. The source file is read and sent to the Hub API.

  --wait (default: true when stdout is a TTY, false in scripts/pipes):
    Stream log events to stdout and exit with the run's exit code.
    --wait=false prints only the run ID and exits 0.

Exit codes in Hub mode:
  0  run succeeded (or --wait=false)
  1  run failed, timed out, canceled, or API error

Examples:
  scion workflow run flow.duck.yaml
  scion workflow run flow.duck.yaml --input name=world --input count=3
  scion workflow run flow.duck.yaml --input-file inputs.json --trace-dir ./trace
  scion workflow run flow.duck.yaml --via-hub
  scion workflow run flow.duck.yaml --via-hub --wait=false
  scion workflow run flow.duck.yaml --via-hub --grove my-grove-id`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkflowRun,
}

func runWorkflowRun(cmd *cobra.Command, args []string) error {
	file := args[0]

	if err := workflow.ValidateInputFlags(workflowRunInputs); err != nil {
		return fmt.Errorf("invalid --input flag: %w", err)
	}

	// Hub mode: dispatch to Hub API.
	if workflowRunViaHub {
		return runWorkflowRunViaHub(cmd, file)
	}

	// Local mode (Phase 1): delegate to quack subprocess.
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

// runWorkflowRunViaHub dispatches a workflow run to the Hub.
func runWorkflowRunViaHub(cmd *cobra.Command, file string) error {
	// Read the source file.
	sourceBytes, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("reading workflow file: %w", err)
	}
	sourceYAML := string(sourceBytes)

	// Resolve grove ID: prefer explicit --grove flag, then settings/hub context.
	groveID := workflowRunGroveID

	// Build hub client from settings.
	settings, err := loadSettingsForWorkflow()
	if err != nil {
		return err
	}

	client, err := getHubClient(settings)
	if err != nil {
		return fmt.Errorf("connecting to Hub: %w", err)
	}

	// If no grove ID was specified, look it up through the hub context path.
	if groveID == "" {
		hubCtx, hubErr := CheckHubAvailabilityWithOptions(grovePath, true)
		if hubErr != nil {
			return fmt.Errorf("resolving grove: %w\n\nUse --grove <id> to specify a grove ID explicitly", hubErr)
		}
		if hubCtx != nil {
			var lookupErr error
			groveID, lookupErr = GetGroveID(hubCtx)
			if lookupErr != nil {
				return fmt.Errorf("resolving grove ID: %w", lookupErr)
			}
			// Use the client from the hub context (it carries auth).
			client = hubCtx.Client
		}
	}

	if groveID == "" {
		return fmt.Errorf("no grove ID resolved; use --grove <id> or configure a Hub grove in settings.yaml")
	}

	// Build inputs JSON from --input flags and/or --input-file.
	inputsJSON, err := buildInputsJSON(workflowRunInputs, workflowRunInputFile)
	if err != nil {
		return fmt.Errorf("building inputs: %w", err)
	}

	req := &hubclient.CreateWorkflowRunRequest{
		GroveID:    groveID,
		SourceYAML: sourceYAML,
		Inputs:     inputsJSON,
	}

	ctx := cmd.Context()

	PrintUsingHub(GetHubEndpoint(settings))
	statusf("Dispatching workflow run to Hub...\n")

	run, err := client.CreateWorkflowRun(ctx, req)
	if err != nil {
		return fmt.Errorf("creating workflow run: %w", err)
	}

	// Determine --wait default: true when stdout is a TTY (user is not piping output).
	waitForRun := util.IsStdoutTerminal()
	if workflowRunWait != nil {
		waitForRun = *workflowRunWait
	}

	if !waitForRun {
		fmt.Println(run.ID)
		return nil
	}

	// Stream logs and wait for terminal event.
	statusf("Run ID: %s\n", run.ID)
	statusf("Streaming logs (Ctrl+C to detach)...\n")

	ch, err := client.StreamWorkflowRunLogs(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("opening log stream: %w", err)
	}

	jsonOut := isJSONOutput()
	terminalStatus, err := streamWorkflowLogs(ctx, ch, run.ID, false, jsonOut)
	if err != nil {
		return fmt.Errorf("streaming logs: %w", err)
	}
	finalStatus := terminalStatus
	if finalStatus == "" {
		finalStatus = run.Status
	}

	// Map final status to exit code.
	switch finalStatus {
	case "succeeded":
		return nil
	case "failed", "timed_out", "canceled":
		fmt.Fprintf(os.Stderr, "Workflow run %s: %s\n", run.ID, finalStatus)
		os.Exit(1)
	default:
		// Unknown or still in non-terminal state when stream closed.
		fmt.Fprintf(os.Stderr, "Workflow run %s ended with status: %s\n", run.ID, finalStatus)
		os.Exit(1)
	}
	return nil
}

// loadSettingsForWorkflow loads settings. On load error it returns an empty
// (but non-nil) *config.Settings so callers can still proceed when the Hub
// endpoint is supplied via --hub or SCION_HUB_ENDPOINT. Returning a nil
// *config.Settings here would panic in GetHubEndpoint (the nil-receiver's
// method dereferences s.Hub).
func loadSettingsForWorkflow() (*config.Settings, error) {
	settings, err := config.LoadSettings(grovePath)
	if err != nil {
		// Not fatal: Hub endpoint may be provided via --via-hub flag or env vars.
		// Return an empty Settings so getHubClient/GetHubEndpoint can still
		// consult env/flags without nil-deref.
		return &config.Settings{}, nil //nolint:nilerr
	}
	return settings, nil
}

// buildInputsJSON converts --input flags and an optional --input-file to a JSON string.
// Returns "" if no inputs are provided.
func buildInputsJSON(inputs []string, inputFile string) (string, error) {
	merged := map[string]interface{}{}

	// Load from file first (lower precedence).
	if inputFile != "" {
		data, err := os.ReadFile(inputFile)
		if err != nil {
			return "", fmt.Errorf("reading input file %q: %w", inputFile, err)
		}
		if err := json.Unmarshal(data, &merged); err != nil {
			return "", fmt.Errorf("parsing input file %q: %w", inputFile, err)
		}
	}

	// Apply --input flags (higher precedence).
	for _, kv := range inputs {
		idx := 0
		for idx < len(kv) && kv[idx] != '=' {
			idx++
		}
		if idx >= len(kv) {
			continue // already validated by ValidateInputFlags
		}
		key := kv[:idx]
		val := kv[idx+1:]
		merged[key] = val
	}

	if len(merged) == 0 {
		return "", nil
	}

	b, err := json.Marshal(merged)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func init() {
	workflowCmd.AddCommand(workflowRunCmd)

	workflowRunCmd.Flags().StringArrayVar(&workflowRunInputs, "input", nil, "Input key=value pair (repeatable); highest precedence over --input-file")
	workflowRunCmd.Flags().StringVar(&workflowRunInputFile, "input-file", "", "Path to a JSON input envelope file")
	workflowRunCmd.Flags().StringVar(&workflowRunCwd, "cwd", "", "Working directory for exec participants (local mode only)")
	workflowRunCmd.Flags().StringVar(&workflowRunTraceDir, "trace-dir", "", "Directory for structured trace output (local mode only)")
	workflowRunCmd.Flags().StringVar(&workflowRunEventBackend, "event-backend", "memory", "Event hub backend: memory, nats, or redis (local mode only)")
	workflowRunCmd.Flags().BoolVarP(&workflowRunVerbose, "verbose", "v", false, "Enable extra diagnostic output (local mode only)")
	workflowRunCmd.Flags().BoolVarP(&workflowRunQuiet, "quiet", "q", false, "Suppress info logs on stderr (local mode only)")
	workflowRunCmd.Flags().BoolVar(&workflowRunLocal, "local", false, "Force local subprocess dispatch (Phase 1 behavior)")

	// Hub dispatch flags.
	workflowRunCmd.Flags().BoolVar(&workflowRunViaHub, "via-hub", false, "Dispatch workflow run via the Hub instead of running locally")
	workflowRunCmd.Flags().StringVar(&workflowRunGroveID, "grove-id", "", "Grove ID for Hub dispatch (overrides grove resolution)")

	// --wait flag: tri-state (unset = auto, true, false).
	workflowRunCmd.Flags().Func("wait", "Wait for run to complete and stream logs (default: auto; true when stdout is a TTY)", func(s string) error {
		var b bool
		switch s {
		case "true", "1", "yes":
			b = true
		case "false", "0", "no":
			b = false
		default:
			return fmt.Errorf("invalid --wait value %q: use true or false", s)
		}
		workflowRunWait = &b
		return nil
	})
}
