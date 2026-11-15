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
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type adminSystemOneCreateFlags struct {
	adapter        string
	model          string
	endpoint       string
	location       string
	keyFile        string
	keyStdin       bool
	options        []string
	maxInFlight    int
	ratePerSec     float64
	maxInputTokens int
	callTimeout    string
	enabled        bool
}

var adminSystemOneCreateOpts adminSystemOneCreateFlags

var adminSystemOneInstanceCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Register a Hub System One provider instance",
	Long: `Register a System One provider instance on the Hub.

For security, the provider API key is read ONLY from standard input (piped or
with --key-stdin) or from --key-file <path>. Passing the key via positional
arguments, command-line flags, or environment variables is not supported.

Examples:
  # Read API key from a file
  scion admin system-one instance create typesafe-main \
    --adapter typesafe \
    --model jev-1.14.0 \
    --key-file /path/to/key.txt

  # Read API key from standard input
  scion admin system-one instance create typesafe-main \
    --adapter typesafe \
    --model jev-1.14.0 \
    --key-stdin < /path/to/key.txt`,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return errors.New("instance name is required")
		}
		if len(args) > 1 {
			return fmt.Errorf("accepts 1 argument (<name>), received %d; pass the API key via --key-file or stdin, never on the command line", len(args))
		}
		return nil
	},
	RunE: runAdminSystemOneInstanceCreate,
}

func init() {
	f := adminSystemOneInstanceCreateCmd.Flags()
	f.StringVar(&adminSystemOneCreateOpts.adapter, "adapter", "typesafe", "Provider adapter name (for example: typesafe)")
	f.StringVar(&adminSystemOneCreateOpts.model, "model", "", "Provider model identifier (required for typesafe, e.g. jev-1.14.0)")
	f.StringVar(&adminSystemOneCreateOpts.endpoint, "endpoint", "", "Provider base HTTPS URL (defaults to adapter default when omitted)")
	f.StringVar(&adminSystemOneCreateOpts.location, "location", "", "Optional provider location or region")
	f.StringVar(&adminSystemOneCreateOpts.keyFile, "key-file", "", "Path to file containing the provider API key (or '-' for stdin)")
	f.BoolVar(&adminSystemOneCreateOpts.keyStdin, "key-stdin", false, "Read the provider API key from standard input")
	f.StringArrayVar(&adminSystemOneCreateOpts.options, "option", nil, "Non-secret adapter option as KEY=VALUE (repeatable)")
	f.IntVar(&adminSystemOneCreateOpts.maxInFlight, "max-in-flight", 0, "Maximum concurrent evaluations on this instance")
	f.Float64Var(&adminSystemOneCreateOpts.ratePerSec, "rate-per-sec", 0, "Sustained evaluation rate limit per second")
	f.IntVar(&adminSystemOneCreateOpts.maxInputTokens, "max-input-tokens", 0, "Maximum input token ceiling")
	f.StringVar(&adminSystemOneCreateOpts.callTimeout, "call-timeout", "", "Per-call timeout ceiling as a Go duration (for example: 5s)")
	f.BoolVar(&adminSystemOneCreateOpts.enabled, "enabled", true, "Enable the instance immediately upon creation")

	adminSystemOneInstanceCmd.AddCommand(adminSystemOneInstanceCreateCmd)
}

func runAdminSystemOneInstanceCreate(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	if name == "" {
		return errors.New("instance name is required")
	}

	optsMap, err := parseAdminSystemOneKeyValues(adminSystemOneCreateOpts.options, "--option")
	if err != nil {
		return err
	}

	apiKey, err := readAdminSystemOneAPIKey(cmd, adminSystemOneCreateOpts.keyFile, adminSystemOneCreateOpts.keyStdin)
	if err != nil {
		return err
	}

	var limits *hubclient.SystemOneInstanceLimitsDTO
	if cmd.Flags().Changed("max-in-flight") ||
		cmd.Flags().Changed("rate-per-sec") ||
		cmd.Flags().Changed("max-input-tokens") ||
		cmd.Flags().Changed("call-timeout") {
		limits = &hubclient.SystemOneInstanceLimitsDTO{
			MaxInFlight:    adminSystemOneCreateOpts.maxInFlight,
			RatePerSec:     adminSystemOneCreateOpts.ratePerSec,
			MaxInputTokens: adminSystemOneCreateOpts.maxInputTokens,
			CallTimeout:    strings.TrimSpace(adminSystemOneCreateOpts.callTimeout),
		}
	}

	enabled := adminSystemOneCreateOpts.enabled
	req := &hubclient.CreateSystemOneInstanceRequest{
		Name:     name,
		Adapter:  strings.TrimSpace(adminSystemOneCreateOpts.adapter),
		Model:    strings.TrimSpace(adminSystemOneCreateOpts.model),
		Endpoint: strings.TrimSpace(adminSystemOneCreateOpts.endpoint),
		Location: strings.TrimSpace(adminSystemOneCreateOpts.location),
		Auth: &hubclient.CreateSystemOneInstanceAuthRequest{
			Kind:   "api_key",
			APIKey: apiKey,
		},
		Options: optsMap,
		Limits:  limits,
		Enabled: &enabled,
	}

	svc, ctx, cancel, err := systemOneAdminService(cmd, adminSystemOneDefaultTimeout)
	if err != nil {
		return err
	}
	defer cancel()

	inst, err := svc.CreateInstance(ctx, req)
	if err != nil {
		return redactSecretFromError(err, apiKey)
	}

	if isJSONOutput() {
		return writeJSONTo(cmd.OutOrStdout(), inst)
	}

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Created System One instance %s\n", inst.Name)
	_, _ = fmt.Fprintf(out, "  ID:       %s\n", inst.ID)
	_, _ = fmt.Fprintf(out, "  Adapter:  %s\n", inst.Adapter)
	if inst.Model != "" {
		_, _ = fmt.Fprintf(out, "  Model:    %s\n", inst.Model)
	}
	_, _ = fmt.Fprintf(out, "  Endpoint: %s\n", inst.Endpoint)
	_, _ = fmt.Fprintf(out, "  Enabled:  %t\n", inst.Enabled)
	if inst.Auth.SecretName != "" {
		_, _ = fmt.Fprintf(out, "  Secret:   %s\n", inst.Auth.SecretName)
	}
	_, _ = fmt.Fprintf(out, "  Created:  %s\n", clitime.Format(inst.CreatedAt, clitime.Full))
	return nil
}

// readAdminSystemOneAPIKey reads the credential exclusively from --key-file or
// stdin. It never reads environment variables or flag values, and never echoes
// credential bytes in any error message.
func readAdminSystemOneAPIKey(cmd *cobra.Command, keyFile string, keyStdin bool) (string, error) {
	keyFile = strings.TrimSpace(keyFile)
	if keyFile != "" && keyStdin {
		return "", errors.New("use either --key-file or --key-stdin, not both")
	}

	var raw []byte
	var err error
	switch {
	case keyFile != "" && keyFile != "-":
		raw, err = readBoundedFile(keyFile, maxAdminSystemOneAPIKeyReadBytes)
		if err != nil {
			return "", fmt.Errorf("read --key-file: %w", err)
		}
	case keyStdin || keyFile == "-":
		raw, err = readBoundedReader(cmd.InOrStdin(), maxAdminSystemOneAPIKeyReadBytes)
		if err != nil {
			return "", fmt.Errorf("read API key from stdin: %w", err)
		}
	default:
		in := cmd.InOrStdin()
		if isInteractiveTerminalReader(in) {
			return "", errors.New("API key is required: pipe it on stdin (or pass --key-stdin) or provide --key-file <path>")
		}
		raw, err = readBoundedReader(in, maxAdminSystemOneAPIKeyReadBytes)
		if err != nil {
			return "", fmt.Errorf("read API key from stdin: %w", err)
		}
	}

	key := strings.TrimSpace(string(raw))
	if key == "" {
		return "", errors.New("API key is empty; provide a non-empty credential via --key-file or stdin")
	}
	if !utf8.ValidString(key) {
		return "", errors.New("API key must be valid UTF-8")
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("API key must not contain control characters or multiple lines")
		}
	}
	return key, nil
}

func isInteractiveTerminalReader(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok || f == nil {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

func readBoundedFile(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readBoundedReader(f, maxBytes)
}

func readBoundedReader(r io.Reader, maxBytes int64) ([]byte, error) {
	if r == nil {
		return nil, errors.New("input stream is nil")
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("input exceeds maximum of %d bytes", maxBytes)
	}
	return data, nil
}

// redactSecretFromError ensures that even if an upstream proxy or transport
// unexpectedly echoes request payload bytes in an error string, the API key is
// scrubbed before returning to Cobra.
func redactSecretFromError(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, secret) {
		return err
	}
	return errors.New(strings.ReplaceAll(msg, secret, "[REDACTED]"))
}
