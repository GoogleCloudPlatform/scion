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

package workflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// ErrQuackNotFound is returned when the quack binary is not found in PATH.
var ErrQuackNotFound = errors.New("quack (duckflux runner) not found in PATH")

// RunLocalRequest describes a local quack invocation.
type RunLocalRequest struct {
	// File is the path to the workflow YAML file (required).
	File string

	// Inputs is a slice of raw --input k=v strings passed through to quack unparsed.
	Inputs []string

	// InputFile is the path to a JSON input envelope (--input-file).
	InputFile string

	// Cwd overrides the working directory for exec participants (--cwd).
	Cwd string

	// TraceDir is the directory for structured trace output (--trace-dir).
	TraceDir string

	// EventBackend selects the event hub backend: memory, nats, or redis (--event-backend).
	EventBackend string

	// Verbose enables extra diagnostic output (--verbose).
	Verbose bool

	// Quiet suppresses info logs on stderr (--quiet).
	Quiet bool

	// Stdin is the reader to attach to the subprocess stdin.
	// If nil, os.Stdin is used.
	Stdin io.Reader

	// Stdout is the writer to attach to the subprocess stdout.
	// If nil, os.Stdout is used.
	Stdout io.Writer

	// Stderr is the writer to attach to the subprocess stderr.
	// If nil, os.Stderr is used.
	Stderr io.Writer
}

// RunLocalResult holds the outcome of a local quack invocation.
type RunLocalResult struct {
	// ExitCode is the exit code returned by quack:
	//   0 = workflow succeeded
	//   1 = CLI/usage error
	//   2 = workflow executed but ended with success=false
	ExitCode int
}

// RunLocal invokes `quack run <file> [flags...]` as a subprocess.
// Stdio is piped through from req; nil values fall back to os.Stdin/Stdout/Stderr.
func RunLocal(ctx context.Context, req RunLocalRequest) (*RunLocalResult, error) {
	return runQuack(ctx, "run", req)
}

// ValidateLocal invokes `quack validate <file> [flags...]` as a subprocess.
// It shares the same request type as RunLocal.
func ValidateLocal(ctx context.Context, req RunLocalRequest) (*RunLocalResult, error) {
	return runQuack(ctx, "validate", req)
}

// runQuack is the shared implementation for RunLocal and ValidateLocal.
func runQuack(ctx context.Context, subcmd string, req RunLocalRequest) (*RunLocalResult, error) {
	quackPath, err := exec.LookPath("quack")
	if err != nil {
		return nil, fmt.Errorf("%w: install with 'npm install -g @duckflux/runner' or use a Scion agent image with quack baked in (available from Phase 2)", ErrQuackNotFound)
	}

	args := []string{subcmd, req.File}

	for _, kv := range req.Inputs {
		args = append(args, "--input", kv)
	}
	if req.InputFile != "" {
		args = append(args, "--input-file", req.InputFile)
	}
	if req.Cwd != "" {
		args = append(args, "--cwd", req.Cwd)
	}
	if req.TraceDir != "" {
		args = append(args, "--trace-dir", req.TraceDir)
	}
	if req.EventBackend != "" {
		args = append(args, "--event-backend", req.EventBackend)
	}
	if req.Verbose {
		args = append(args, "--verbose")
	}
	if req.Quiet {
		args = append(args, "--quiet")
	}

	cmd := exec.CommandContext(ctx, quackPath, args...)

	if req.Stdin != nil {
		cmd.Stdin = req.Stdin
	} else {
		cmd.Stdin = os.Stdin
	}
	if req.Stdout != nil {
		cmd.Stdout = req.Stdout
	} else {
		cmd.Stdout = os.Stdout
	}
	if req.Stderr != nil {
		cmd.Stderr = req.Stderr
	} else {
		cmd.Stderr = os.Stderr
	}

	runErr := cmd.Run()
	if runErr == nil {
		return &RunLocalResult{ExitCode: 0}, nil
	}

	// Context cancellation takes priority over the process exit code.
	if ctx.Err() != nil {
		return nil, fmt.Errorf("workflow run cancelled: %w", ctx.Err())
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return &RunLocalResult{ExitCode: exitErr.ExitCode()}, nil
	}

	// Unexpected error (e.g., could not fork, binary not executable).
	return nil, fmt.Errorf("quack subprocess error: %w", runErr)
}
