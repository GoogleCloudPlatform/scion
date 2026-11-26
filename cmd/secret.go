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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	sciontoolhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/spf13/cobra"
)

// maxAgentSecretValueBytes caps secret values read from stdin, @file, or argv.
const maxAgentSecretValueBytes = 64 * 1024

// Flag variables for agent secret commands. Named with "agentSecret" prefix
// to avoid collisions with the hub secret command flags in hub_secret.go.
var (
	agentSecretType         string
	agentSecretTarget       string
	agentSecretSetScope     string
	agentSecretForce        bool
	agentSecretAllowProgeny bool
	agentSecretGetMetadata  bool
	agentSecretGetValue     bool
	agentSecretGetScope     string
	agentSecretListScope    string
)

// agentSecretCmd is the top-level `scion secret` command group.
var agentSecretCmd = &cobra.Command{
	Use:   "secret",
	Short: "Manage agent secrets",
	Long: `Commands for managing secrets from the host or within an agent container.

On the host, secrets default to the current project's scope. Inside a running
agent container, 'scion secret list' and 'scion secret get' support project
scope and (when hub.agent_secrets is enabled) agent scope via '--scope agent'
(default: agent then project for 'get', all for 'list'); 'scion secret set'
stores secrets at project, user (pending approval), or agent scope; and
'scion secret request' lists or withdraws the agent's secret requests. To
delete an agent-scoped secret from inside an agent container, use
'sciontool secret delete --scope agent'.

For full scope management (--scope hub, --project, --broker, --agent), use 'scion hub secret'.`,
}

// secretGetPrintsValueByDefault selects what plain `scion secret get KEY`
// prints: the secret value (true) or its metadata (false). --value and
// --metadata override it either way, so flipping the default is a change
// to this constant alone.
const secretGetPrintsValueByDefault = false

// errSecretValueNeedsAgent is returned when `scion secret get` is asked for
// a value without an agent's hub identity. The hub returns secret values
// only to agents, through the agent-scoped endpoint.
var errSecretValueNeedsAgent = errors.New("printing a secret value (--value) requires an agent's hub identity " +
	"(run inside an agent container); without --value, 'scion secret get KEY' prints the stored metadata")

// newSecretValueHubClient builds the Hub client for reading a secret value.
// It is the agent's own client (the same one sciontool uses), so the read
// is made as the calling agent. Overridden in tests.
var newSecretValueHubClient = func() *sciontoolhub.Client {
	return sciontoolhub.NewClient()
}

// agentSecretGetCmd retrieves a secret's metadata or (inside an agent container) its value.
var agentSecretGetCmd = &cobra.Command{
	Use:   "get KEY",
	Short: "Get a secret's metadata, or its value with --value",
	Long: `Get a secret from the Hub.

On the host, looks up the secret in the current project's scope. Inside an agent
container (when hub.agent_secrets is enabled), checks agent scope first and then
project scope; pass --scope agent or --scope project to pin the lookup.

By default this prints the secret's metadata (key, scope, type, target,
version and timestamps). --format json prints the metadata object and
never the value.

With --value, the decoded value is written to stdout as-is, with no added
newline or labels, so it is safe to capture with $(...). Reading a value
requires an agent's hub identity: run it inside an agent container.

Examples:
  # Show secret metadata
  scion secret get MY_API_KEY

  # Print a secret value (inside an agent container)
  scion secret get --value MY_API_KEY

  # Use in a script
  export API_KEY=$(scion secret get --value MY_API_KEY)`,
	Args: cobra.ExactArgs(1),
	RunE: runAgentSecretGet,
}

// agentSecretSetCmd stores a secret in the Hub.
var agentSecretSetCmd = &cobra.Command{
	Use:   "set KEY VALUE",
	Short: "Store a secret in the Hub",
	Long: `Store a secret in the Hub from the host or within an agent container.

By default, the secret is scoped to the current project. Inside an agent
container, use --scope=agent to store a secret private to the current agent
(requires the hub.agent_secrets experiment) or --scope=user to save a personal
credential for your originating user: a user-scope secret waits for user
approval in Scion before any agent receives it, and --force has no effect on it.

If VALUE is "-", the secret value is read from stdin (up to 64 KiB) so it
stays out of process listings. For non-file types, a single trailing newline
is trimmed from stdin. Prefer "-" (stdin): values given as arguments are
visible in process listings and shell history.

If VALUE starts with @, the remainder is treated as a file path. The file
contents are read and base64-encoded, and --type defaults to "file".

Examples:
  # Store an agent-scoped secret from stdin
  printf '%s' "$TOKEN" | scion secret set MY_AGENT_TOKEN - --scope agent

  # Store a user-scoped (personal) secret pending user approval
  printf '%s' "$TOKEN" | scion secret set MY_TOKEN - --scope user

  # Store a project-scoped secret from stdin
  printf '%s' "$API_KEY" | scion secret set MY_API_KEY -

  # Store a credential file
  scion secret set CLAUDE_AUTH @~/.claude/.credentials.json

  # Store a file secret with explicit target path
  scion secret set MY_CERT @/tmp/cert.pem --type file --target ~/certs/cert.pem

  # Overwrite an existing project-scoped secret
  printf '%s' "$NEW_VAL" | scion secret set MY_KEY - --force`,
	Args: cobra.ExactArgs(2),
	RunE: runAgentSecretSet,
}

// agentSecretListCmd lists metadata for available secrets.
var agentSecretListCmd = &cobra.Command{
	Use:   "list",
	Short: "List secret metadata",
	Long: `List secret metadata from the Hub.

Inside an agent container (when hub.agent_secrets is enabled), defaults to
--scope all and lists both agent- and project-scoped secrets with a SCOPE
column (filter with --scope agent, --scope project, or --scope all). On the host
(or when hub.agent_secrets is off), lists project-scoped secrets.

Only metadata is returned — secret values are not included.

Examples:
  scion secret list`,
	Args: cobra.NoArgs,
	RunE: runAgentSecretList,
}

func init() {
	rootCmd.AddCommand(agentSecretCmd)
	agentSecretCmd.AddCommand(agentSecretGetCmd)
	agentSecretCmd.AddCommand(agentSecretSetCmd)
	agentSecretCmd.AddCommand(agentSecretListCmd)
	agentSecretCmd.AddCommand(newSecretRequestCmd())

	agentSecretGetCmd.Flags().BoolVar(&agentSecretGetMetadata, "metadata", false, "Print the secret's metadata (the default)")
	agentSecretGetCmd.Flags().BoolVar(&agentSecretGetValue, "value", false, "Print the secret's decoded value (requires an agent's hub identity)")
	agentSecretGetCmd.Flags().StringVar(&agentSecretGetScope, "scope", "", "Pin lookup to a scope: agent or project (default: agent then project)")
	agentSecretGetCmd.MarkFlagsMutuallyExclusive("metadata", "value")

	agentSecretListCmd.Flags().StringVar(&agentSecretListScope, "scope", "", "Filter by scope: agent, project, or all (default: all)")

	agentSecretSetCmd.Flags().StringVar(&agentSecretType, "type", "", "Secret type: environment (default), file, on_demand")
	agentSecretSetCmd.Flags().StringVar(&agentSecretTarget, "target", "", "Injection target path (defaults to key for env, required for file; not supported for on_demand)")
	agentSecretSetCmd.Flags().StringVar(&agentSecretSetScope, "scope", "", "Secret scope: project (default), user, or agent")
	agentSecretSetCmd.Flags().BoolVar(&agentSecretForce, "force", false, "Overwrite existing secret")
	agentSecretSetCmd.Flags().BoolVar(&agentSecretAllowProgeny, "allow-progeny", false, "Allow progeny agents to inherit this secret (user scope only)")
}

// secretGetWantsValue reports whether `scion secret get` should print the
// value rather than the metadata, from the flags and the default.
func secretGetWantsValue(metadata, value, jsonOutput bool) (bool, error) {
	if jsonOutput {
		if value {
			return false, newUsageError("--value cannot be combined with --format json: JSON output is metadata only")
		}
		return false, nil
	}
	switch {
	case metadata:
		return false, nil
	case value:
		return true, nil
	}
	return secretGetPrintsValueByDefault, nil
}

func runAgentSecretGet(cmd *cobra.Command, args []string) error {
	switch agentSecretGetScope {
	case "", "agent", "project":
	default:
		return newUsageError("invalid --scope %q: must be 'agent' or 'project'", agentSecretGetScope)
	}
	wantValue, err := secretGetWantsValue(agentSecretGetMetadata, agentSecretGetValue, isJSONOutput())
	if err != nil {
		return err
	}
	if wantValue {
		return runAgentSecretGetValue(cmd, args[0])
	}
	return runAgentSecretGetMetadata(cmd, args[0])
}

// runAgentSecretGetValue writes the decoded secret value to stdout, using
// the agent-scoped endpoint as the calling agent (as sciontool did).
func runAgentSecretGetValue(cmd *cobra.Command, key string) error {
	client := newSecretValueHubClient()
	if client == nil || !client.IsConfigured() {
		return errSecretValueNeedsAgent
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	resp, err := client.GetSecretWithScope(ctx, key, agentSecretGetScope)
	if err != nil {
		return fmt.Errorf("failed to get secret: %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(resp.Value)
	if err != nil {
		return fmt.Errorf("failed to decode secret value: %w", err)
	}
	if _, err := cmd.OutOrStdout().Write(decoded); err != nil {
		return fmt.Errorf("failed to write secret value: %w", err)
	}
	return nil
}

// fetchProjectSecretMeta reads the metadata of a project-scoped secret from
// the Hub for the `scion secret get` metadata view (the default, or
// --metadata). Overridden in tests.
var fetchProjectSecretMeta = func(cmd *cobra.Command, key string) (*hubclient.Secret, error) {
	hubCtx, err := requireHub(projectPath)
	if err != nil {
		return nil, err
	}
	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return nil, wrapHubError(err)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	opts := &hubclient.SecretScopeOptions{
		Scope:   "project",
		ScopeID: projectID,
	}

	secret, err := hubCtx.Client.Secrets().Get(ctx, key, opts)
	if err != nil {
		return nil, wrapHubError(fmt.Errorf("failed to get secret: %w", err))
	}
	return secret, nil
}

func runAgentSecretGetMetadata(cmd *cobra.Command, key string) error {
	var sec *hubclient.Secret
	var err error

	// Inside an agent container (or when --scope agent is explicitly passed),
	// resolve metadata through the agent secrets route so --scope is honored
	// and default lookups check agent scope before project scope (matching
	// --value). Outside an agent container (no SCION_AGENT_ID and no --scope
	// agent), use the project-scoped route directly without constructing an
	// agent value client.
	if os.Getenv("SCION_AGENT_ID") != "" || agentSecretGetScope == "agent" {
		agentClient := newSecretValueHubClient()
		if agentClient == nil || !agentClient.IsConfigured() {
			if agentSecretGetScope == "agent" {
				return errSecretValueNeedsAgent
			}
			sec, err = fetchProjectSecretMeta(cmd, key)
		} else {
			sec, err = fetchAgentSecretMeta(cmd, agentClient, key, agentSecretGetScope)
		}
	} else {
		sec, err = fetchProjectSecretMeta(cmd, key)
	}
	if err != nil {
		return err
	}

	if isJSONOutput() {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(sec)
	}

	printSecretMetadata(cmd.OutOrStdout(), sec)
	return nil
}

// fetchAgentSecretMeta resolves secret metadata through the agent-scoped list
// route (/api/v1/agents/{id}/secrets). When scope is empty, it probes with
// scope=all (matching `scion secret list`) and picks agent scope before
// project scope on the client so metadata lookups match --value. Whenever the
// resolved scope is project (explicit --scope project, default lookup matching
// a project secret, or experiment-off fallback), metadata is fetched via
// fetchProjectSecretMeta so Version/Created/Updated/Description are preserved.
func fetchAgentSecretMeta(cmd *cobra.Command, agentClient *sciontoolhub.Client, key, scope string) (*hubclient.Secret, error) {
	if scope == "project" {
		return fetchProjectSecretMeta(cmd, key)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	if scope == "agent" {
		resp, err := agentClient.ListSecretsWithScope(ctx, "agent")
		if err != nil {
			return nil, fmt.Errorf("failed to get secret: %w", err)
		}
		for _, item := range resp.Secrets {
			if item.Key == key {
				return &hubclient.Secret{
					Key:        item.Key,
					Scope:      "agent",
					SecretType: item.Type,
					Target:     item.Target,
				}, nil
			}
		}
		return nil, fmt.Errorf("secret %q not found in scope agent", key)
	}

	// Default (--scope unset): probe with scope=all (same as `scion secret list`),
	// then pick agent-then-project on the client.
	resp, err := agentClient.ListSecretsWithScope(ctx, "all")
	if err != nil {
		if !errors.Is(err, sciontoolhub.ErrAgentScopeUnavailable) {
			return nil, fmt.Errorf("failed to get secret: %w", err)
		}
		// Experiment off: preserve pre-existing project-scoped metadata view.
		return fetchProjectSecretMeta(cmd, key)
	}

	var foundProject bool
	for _, item := range resp.Secrets {
		if item.Key != key {
			continue
		}
		if item.Scope == "agent" {
			return &hubclient.Secret{
				Key:        item.Key,
				Scope:      "agent",
				SecretType: item.Type,
				Target:     item.Target,
			}, nil
		}
		if item.Scope == "project" || item.Scope == "" {
			foundProject = true
		}
	}
	if foundProject {
		return fetchProjectSecretMeta(cmd, key)
	}
	return nil, fmt.Errorf("secret %q not found", key)
}

// printSecretMetadata writes the text metadata view of a secret, the
// default output of `scion secret get KEY`.
func printSecretMetadata(w io.Writer, secret *hubclient.Secret) {
	_, _ = fmt.Fprintf(w, "Secret: %s\n", secret.Key)
	_, _ = fmt.Fprintf(w, "  Scope:   %s\n", secret.Scope)
	typeLabel := secret.SecretType
	if typeLabel == "" {
		typeLabel = "environment"
	}
	_, _ = fmt.Fprintf(w, "  Type:    %s\n", typeLabel)
	if secret.Target != "" && secret.Target != secret.Key {
		_, _ = fmt.Fprintf(w, "  Target:  %s\n", secret.Target)
	}
	if secret.SecretRef != "" {
		_, _ = fmt.Fprintf(w, "  Ref:     %s\n", secret.SecretRef)
	}
	if secret.Version > 0 {
		_, _ = fmt.Fprintf(w, "  Version: %d\n", secret.Version)
	}
	if !secret.Created.IsZero() {
		_, _ = fmt.Fprintf(w, "  Created: %s\n", clitime.Format(secret.Created, clitime.Full))
	}
	if !secret.Updated.IsZero() {
		_, _ = fmt.Fprintf(w, "  Updated: %s\n", clitime.Format(secret.Updated, clitime.Full))
	}
	if secret.Description != "" {
		_, _ = fmt.Fprintf(w, "  Description: %s\n", secret.Description)
	}
}

func runAgentSecretSet(cmd *cobra.Command, args []string) error {
	key := args[0]
	value := args[1]

	// Validate key.
	if key == "" {
		return newUsageError("key cannot be empty")
	}
	if strings.ContainsAny(key, "= \t\n") {
		return newUsageError("key cannot contain spaces, tabs, newlines, or '='")
	}

	// Validate scope flag.
	if agentSecretSetScope != "" && agentSecretSetScope != "project" && agentSecretSetScope != "user" && agentSecretSetScope != "agent" {
		return newUsageError("--scope must be \"project\", \"user\", or \"agent\"")
	}

	// Validate type flag and on_demand constraints client-side.
	if err := validateSecretTypeFlag(agentSecretType); err != nil {
		return err
	}
	if agentSecretType == "on_demand" {
		if cmd.Flags().Changed("target") || agentSecretTarget != "" {
			return newUsageError("--target cannot be used with --type on_demand")
		}
		if agentSecretSetScope == "user" {
			return newUsageError("on_demand secrets are only supported at project and agent scope")
		}
	}

	localType := agentSecretType
	localTarget := agentSecretTarget

	switch {
	case value == "-":
		data, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), maxAgentSecretValueBytes+1))
		if err != nil {
			return fmt.Errorf("failed to read secret value from stdin: %w", err)
		}
		if len(data) > maxAgentSecretValueBytes {
			return fmt.Errorf("secret value from stdin exceeds 64KB limit")
		}
		if localType != "file" {
			s := string(data)
			if strings.HasSuffix(s, "\r\n") {
				s = strings.TrimSuffix(s, "\r\n")
			} else {
				s = strings.TrimSuffix(s, "\n")
			}
			data = []byte(s)
		}
		if len(data) == 0 {
			return fmt.Errorf("secret value from stdin is empty")
		}
		value = base64.StdEncoding.EncodeToString(data)

	case strings.HasPrefix(value, "@"):
		// Handle @file syntax: read file and base64-encode contents.
		filePath := value[1:]
		if filePath == "" {
			return newUsageError("empty file path: VALUE starting with @ must be followed by a file path (e.g., @/path/to/file)")
		}
		// Expand ~ in source file path for reading.
		if filePath == "~" || strings.HasPrefix(filePath, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("failed to expand home directory: %w", err)
			}
			filePath = filepath.Join(home, strings.TrimPrefix(filePath[1:], "/"))
		}
		info, err := os.Stat(filePath)
		if err != nil {
			return fmt.Errorf("failed to stat file %s: %w", filePath, err)
		}
		if info.IsDir() {
			return fmt.Errorf("cannot read secret from directory: %s (expected a file)", filePath)
		}
		if info.Size() > maxAgentSecretValueBytes {
			return fmt.Errorf("file exceeds 64KB limit (%d bytes)", info.Size())
		}
		data, err := os.ReadFile(filePath)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %w", filePath, err)
		}
		value = base64.StdEncoding.EncodeToString(data)
		if localType == "" {
			localType = "file"
		}
		// Auto-set target from source file path for file secrets if not explicitly provided.
		if localType == "file" && localTarget == "" {
			absPath, err := filepath.Abs(filePath)
			if err != nil {
				return fmt.Errorf("failed to resolve absolute path for %s: %w", filePath, err)
			}
			// Convert paths under the user's home directory to ~/ so they
			// map to the container user's home directory at projection time.
			home, err := os.UserHomeDir()
			if err == nil && strings.HasPrefix(absPath, home+"/") {
				localTarget = "~/" + absPath[len(home)+1:]
			} else {
				localTarget = absPath
			}
		}

	default:
		if len(value) > maxAgentSecretValueBytes {
			return fmt.Errorf("secret value exceeds 64KB limit")
		}
		value = base64.StdEncoding.EncodeToString([]byte(value))
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	// Inside an agent container, write through the agent-scoped Hub endpoint
	// (PUT /api/v1/agents/{agentID}/secrets/{key}) using the agent token.
	if agentClient := newSecretValueHubClient(); agentClient != nil && agentClient.IsConfigured() {
		// Resolve allowProgeny. User-scoped captured secrets default to
		// enabling progeny so spawned children inherit the credential,
		// unless the caller explicitly passed --allow-progeny.
		var allowProgeny *bool
		if cmd.Flags().Changed("allow-progeny") {
			allowProgeny = &agentSecretAllowProgeny
		} else if agentSecretSetScope == "user" {
			t := true
			allowProgeny = &t
		}

		resp, err := agentClient.SetSecret(ctx, key, value, localType, localTarget, agentSecretSetScope, agentSecretForce, allowProgeny)
		if err != nil {
			return err
		}
		if isJSONOutput() {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(resp)
		}
		reportAgentSetSecretResult(cmd.OutOrStdout(), key, agentSecretSetScope, resp)
		return nil
	}

	if agentSecretSetScope == "agent" || agentSecretSetScope == "user" {
		return fmt.Errorf("--scope %s on 'scion secret set' requires an agent's hub identity (run inside an agent container); on the host, use 'scion hub secret set'", agentSecretSetScope)
	}

	// Host fallback for project-scoped secrets.
	hubCtx, err := requireHub(projectPath)
	if err != nil {
		return err
	}
	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	typeLabel := localType
	if typeLabel == "" {
		typeLabel = "environment"
	}

	req := &hubclient.SetSecretRequest{
		Value:   value,
		Scope:   "project",
		ScopeID: projectID,
		Type:    localType,
		Target:  localTarget,
	}

	resp, err := hubCtx.Client.Secrets().Set(ctx, key, req)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to set secret: %w", err))
	}

	if resp.Created {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Created secret %s (scope: project, type: %s)\n", key, typeLabel)
	} else {
		version := 0
		if resp.Secret != nil {
			version = resp.Secret.Version
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Updated secret %s (scope: project, type: %s, version: %d)\n", key, typeLabel, version)
	}

	return nil
}

// reportAgentSetSecretResult reports a successful agent-originated secret save.
// Pending user-scope results include "pending approval in Scion" on stdout so
// callers such as capture_auth.py can detect them reliably.
func reportAgentSetSecretResult(w io.Writer, key, requestedScope string, resp *sciontoolhub.SetSecretResponse) {
	if resp != nil && resp.Pending() {
		msg := fmt.Sprintf("Secret %q saved, pending approval in Scion (scope: %s)", resp.Key, resp.Scope)
		if resp.ReplacesExisting {
			msg += "; it replaces your existing secret once approved"
		}
		_, _ = fmt.Fprintln(w, msg)
		return
	}
	scopeLabel := ""
	if resp != nil && resp.Scope != "" {
		scopeLabel = resp.Scope
	} else if requestedScope != "" {
		scopeLabel = requestedScope
	} else {
		scopeLabel = "project"
	}
	_, _ = fmt.Fprintf(w, "Secret %q stored (scope: %s)\n", key, scopeLabel)
}

func runAgentSecretList(cmd *cobra.Command, _ []string) error {
	switch agentSecretListScope {
	case "", "agent", "project", "all":
	default:
		return newUsageError("invalid --scope %q: must be 'agent', 'project', or 'all'", agentSecretListScope)
	}

	// Inside an agent container, query the agent-scoped endpoint with scope=all
	// (or the explicit --scope) so agent-scope secrets are shown alongside
	// project-scope secrets with a SCOPE column. When hub.agent_secrets is off
	// and no explicit scope was requested, the Hub answers 400 (ErrAgentScopeUnavailable)
	// and we fall back to the pre-existing project-scoped list unchanged.
	if agentClient := newSecretValueHubClient(); agentClient != nil && agentClient.IsConfigured() && agentSecretListScope != "project" {
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()

		queryScope := agentSecretListScope
		if queryScope == "" {
			queryScope = "all"
		}
		resp, err := agentClient.ListSecretsWithScope(ctx, queryScope)
		if err == nil {
			if isJSONOutput() {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(resp)
			}
			if len(resp.Secrets) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No secrets found")
				return nil
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-30s  %-10s  %-12s  %s\n", "KEY", "SCOPE", "TYPE", "TARGET")
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-30s  %-10s  %-12s  %s\n", "------------------------------", "----------", "------------", "------------------------------")
			for _, s := range resp.Secrets {
				scopeLabel := s.Scope
				if scopeLabel == "" {
					scopeLabel = "project"
				}
				typeLabel := s.Type
				if typeLabel == "" {
					typeLabel = "environment"
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-30s  %-10s  %-12s  %s\n", truncate(s.Key, 30), scopeLabel, typeLabel, s.Target)
			}
			return nil
		}
		if agentSecretListScope != "" || !errors.Is(err, sciontoolhub.ErrAgentScopeUnavailable) {
			return fmt.Errorf("failed to list secrets: %w", err)
		}
		// Experiment off (default invocation): fall through to unchanged project-scoped list.
	}

	hubCtx, err := requireHub(projectPath)
	if err != nil {
		return err
	}
	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	opts := &hubclient.ListSecretOptions{
		Scope:   "project",
		ScopeID: projectID,
	}

	resp, err := hubCtx.Client.Secrets().List(ctx, opts)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to list secrets: %w", err))
	}

	if isJSONOutput() {
		return outputJSON(newSecretListOutput("project", resp.Secrets))
	}

	if len(resp.Secrets) == 0 {
		fmt.Println("No secrets found (scope: project)")
		return nil
	}

	fmt.Println("Secrets (scope: project):")
	fmt.Printf("%-30s  %-12s  %-8s  %s\n", "KEY", "TYPE", "VERSION", "UPDATED")
	fmt.Printf("%-30s  %-12s  %-8s  %s\n", "------------------------------", "------------", "--------", "-----------------------")
	for _, s := range resp.Secrets {
		typeLabel := s.SecretType
		if typeLabel == "" {
			typeLabel = "environment"
		}
		fmt.Printf("%-30s  %-12s  v%-7d  %s\n", truncate(s.Key, 30), typeLabel, s.Version, clitime.Format(s.Updated, clitime.Full))
	}

	return nil
}
