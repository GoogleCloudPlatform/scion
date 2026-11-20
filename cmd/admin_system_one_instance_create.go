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
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type adminSystemOneCreateFlags struct {
	adapter        string
	model          string
	endpoint       string
	location       string
	authKind       string
	authScopes     []string
	quotaProject   string
	secretName     string
	secretScope    string
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

When --auth-kind is api_key (the default), two credential modes are supported
(mutually exclusive):
  1. Instance-internal secret: pass the API key ONLY via standard input (piped
     or with --key-stdin) or --key-file <path>. Passing secret values via
     positional arguments, flags, or process environment variables is refused.
  2. Named Hub/project secret reference (--secret-name / --secret-scope):
     - On gemini_decisions, omitting --key-stdin and --key-file defaults to
       --secret-name GEMINI_API_KEY --secret-scope project_or_hub.
     - --secret-scope selects hub, project, or project_or_hub (default:
       project_or_hub, checking the evaluation project's secret first and
       falling back to the Hub secret only when the project secret is not found).
     - Allowed secret types are environment and variable (internal and file
       secrets are refused). Binding a named secret to an instance never changes
       that secret's type, injection mode, or delivery. Note that an environment
       secret with injection mode "always" is also delivered to agent containers
       in that scope; prefer a non-injected secret (for example --type variable
       or --injection-mode as_needed) when agents should not receive the policy
       evaluation key.
     - Process environment variables (such as GEMINI_API_KEY), user secrets, and
       runtime-broker secrets are never consulted.

When --auth-kind is google_access_token (supported on gemini_decisions), the Hub
acquires OAuth2 access tokens in memory via Google Application Default
Credentials (ADC) and stores no secret or token:
  - Pass --auth-scope explicitly (repeatable; must start with
    https://www.googleapis.com/auth/). The required Decisions scope is
    unverified (U-G9); cloud-platform alone was observed insufficient; pass
    --auth-scope explicitly.
  - ADC prerequisites: enable the Generative Language API on a paid-tier GCP
    project, and ensure GCE/GKE VM or node access scopes permit the requested
    OAuth scope.
  - --quota-project is optional and unset by default for service-account and
    metadata/Workload Identity ADC (omitting x-goog-user-project so the service
    account's associated project bills). Set --quota-project only to bill a
    deliberate, different project where the principal has
    serviceusage.services.use, or when using authorized_user ADC (where
    --quota-project is required).
  - Do not combine --auth-kind google_access_token with --key-stdin or
    --key-file.
  - ADC credentials and token sources are cached in memory per scope set; a
    changed ADC environment requires a Hub restart. Instance create, get, and
    list perform no ADC discovery or metadata I/O and report Credential Type as
    "unresolved (run probe)" until resolved in-process by a probe or evaluation.

Examples:
  # Register a TypeSafe instance reading the API key from a file
  scion admin system-one instance create typesafe-main \
    --adapter typesafe \
    --model jev-1.14.0 \
    --key-file /path/to/key.txt

  # Register a TypeSafe instance reading the API key from standard input
  scion admin system-one instance create typesafe-main \
    --adapter typesafe \
    --model jev-1.14.0 \
    --key-stdin < /path/to/key.txt

  # Register a Gemini Decision 1 instance referencing a non-injected named secret
  # (recommended setup: store GEMINI_API_KEY as --type variable at project or hub scope)
  scion secret set GEMINI_API_KEY --type variable < /path/to/gemini-key.txt
  scion admin system-one instance create gemini-main \
    --adapter gemini_decisions \
    --model gemini-decision-1 \
    --secret-name GEMINI_API_KEY \
    --secret-scope project_or_hub

  # Register a Gemini Decision 1 instance with an inline instance-internal API key
  scion admin system-one instance create gemini-inline \
    --adapter gemini_decisions \
    --model gemini-decision-1 \
    --key-stdin < /path/to/gemini-key.txt

  # Register a keyless ADC Gemini Decision 1 instance with an explicit scope
  scion admin system-one instance create gemini-adc \
    --adapter gemini_decisions \
    --model gemini-decision-1 \
    --auth-kind google_access_token \
    --auth-scope https://www.googleapis.com/auth/<scope>`,
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
	f.StringVar(&adminSystemOneCreateOpts.adapter, "adapter", "typesafe", "Provider adapter name (for example: typesafe, gemini_decisions)")
	f.StringVar(&adminSystemOneCreateOpts.model, "model", "", "Provider model identifier (required for typesafe and gemini_decisions, e.g. jev-1.14.0 or gemini-decision-1)")
	f.StringVar(&adminSystemOneCreateOpts.endpoint, "endpoint", "", "Provider base HTTPS URL (defaults to adapter default when supported; required otherwise)")
	f.StringVar(&adminSystemOneCreateOpts.location, "location", "", "Optional provider location or region")
	f.StringVar(&adminSystemOneCreateOpts.authKind, "auth-kind", "api_key", "Authentication kind (api_key or google_access_token)")
	f.StringArrayVar(&adminSystemOneCreateOpts.authScopes, "auth-scope", nil, "OAuth2 scope URL for --auth-kind google_access_token (repeatable; required for google_access_token; must start with https://www.googleapis.com/auth/)")
	f.StringVar(&adminSystemOneCreateOpts.quotaProject, "quota-project", "", "Optional GCP billing/quota project ID for --auth-kind google_access_token (sent as x-goog-user-project; required only for authorized_user ADC)")
	f.StringVar(&adminSystemOneCreateOpts.secretName, "secret-name", "", "Named Hub/project secret key for --auth-kind api_key (matches ^[A-Za-z_][A-Za-z0-9_]{0,127}$; defaults to GEMINI_API_KEY on gemini_decisions when no inline key is given)")
	f.StringVar(&adminSystemOneCreateOpts.secretScope, "secret-scope", "", "Named-secret lookup scope for --auth-kind api_key: hub, project, or project_or_hub (default: project_or_hub)")
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

	authKind := strings.TrimSpace(adminSystemOneCreateOpts.authKind)
	if authKind == "" {
		authKind = "api_key"
	}

	adapterName := strings.TrimSpace(adminSystemOneCreateOpts.adapter)
	secretNameChanged := cmd.Flags().Changed("secret-name")
	secretScopeChanged := cmd.Flags().Changed("secret-scope")
	hasSecretRefFlags := secretNameChanged || secretScopeChanged ||
		strings.TrimSpace(adminSystemOneCreateOpts.secretName) != "" ||
		strings.TrimSpace(adminSystemOneCreateOpts.secretScope) != ""
	hasInlineKeyFlags := adminSystemOneCreateOpts.keyStdin || strings.TrimSpace(adminSystemOneCreateOpts.keyFile) != ""

	var (
		apiKey  string
		authReq *hubclient.CreateSystemOneInstanceAuthRequest
	)
	switch authKind {
	case "api_key":
		if len(adminSystemOneCreateOpts.authScopes) > 0 {
			return errors.New("--auth-scope is only supported with --auth-kind google_access_token")
		}
		if strings.TrimSpace(adminSystemOneCreateOpts.quotaProject) != "" {
			return errors.New("--quota-project is only supported with --auth-kind google_access_token")
		}
		if _, hasOptQP := optsMap[provider.OptionQuotaProject]; hasOptQP {
			return errors.New("option quota_project is only supported with --auth-kind google_access_token")
		}
		if hasSecretRefFlags && hasInlineKeyFlags {
			return errors.New("--secret-name and --secret-scope cannot be combined with --key-stdin or --key-file")
		}

		useSecretRef := hasSecretRefFlags
		var pipedKey string
		if !hasSecretRefFlags && !hasInlineKeyFlags && adapterName == "gemini_decisions" {
			in := cmd.InOrStdin()
			if isInteractiveTerminalReader(in) {
				useSecretRef = true
			} else {
				raw, readErr := readBoundedReader(in, maxAdminSystemOneAPIKeyReadBytes)
				if readErr != nil {
					return fmt.Errorf("read API key from stdin: %w", readErr)
				}
				if strings.TrimSpace(string(raw)) == "" {
					useSecretRef = true
				} else {
					valKey, valErr := validateAdminSystemOneAPIKeyBytes(raw)
					if valErr != nil {
						return valErr
					}
					pipedKey = valKey
				}
			}
		}

		if useSecretRef {
			refName := strings.TrimSpace(adminSystemOneCreateOpts.secretName)
			if refName == "" && !secretNameChanged && adapterName == "gemini_decisions" {
				refName = provider.DefaultGeminiAPIKeySecretName
			}
			if !provider.IsValidSecretRefName(refName) || isReservedCLISystemOneSecretName(refName) {
				return errors.New("invalid --secret-name: must match ^[A-Za-z_][A-Za-z0-9_]{0,127}$ and must not use the reserved System One secret prefix")
			}
			refScope := strings.TrimSpace(adminSystemOneCreateOpts.secretScope)
			if refScope == "" && !secretScopeChanged {
				refScope = provider.DefaultSecretRefScope
			}
			if !provider.IsValidSecretRefScope(refScope) {
				return errors.New("invalid --secret-scope: must be \"hub\", \"project\", or \"project_or_hub\"")
			}
			authReq = &hubclient.CreateSystemOneInstanceAuthRequest{
				Kind: "api_key",
				SecretRef: &hubclient.SystemOneSecretRefDTO{
					Name:  refName,
					Scope: refScope,
				},
			}
		} else {
			if pipedKey != "" {
				apiKey = pipedKey
			} else {
				apiKey, err = readAdminSystemOneAPIKey(cmd, adminSystemOneCreateOpts.keyFile, adminSystemOneCreateOpts.keyStdin)
				if err != nil {
					return err
				}
			}
			authReq = &hubclient.CreateSystemOneInstanceAuthRequest{
				Kind:   "api_key",
				APIKey: apiKey,
			}
		}

	case string(provider.AuthKindGoogleAccessToken):
		if hasInlineKeyFlags {
			return errors.New("--key-stdin and --key-file cannot be combined with --auth-kind google_access_token")
		}
		if hasSecretRefFlags {
			return errors.New("--secret-name and --secret-scope cannot be combined with --auth-kind google_access_token")
		}
		cleanScopes, scopeErr := validateAdminSystemOneCLIScopes(adminSystemOneCreateOpts.authScopes)
		if scopeErr != nil {
			return scopeErr
		}
		qp := strings.TrimSpace(adminSystemOneCreateOpts.quotaProject)
		if optQP, hasOptQP := optsMap[provider.OptionQuotaProject]; hasOptQP {
			if qp != "" && qp != optQP {
				return errors.New("conflicting --quota-project and --option quota_project values")
			}
			qp = optQP
		}
		if qp != "" || cmd.Flags().Changed("quota-project") {
			if err := egress.ValidateQuotaProject(qp); err != nil {
				return errors.New("invalid --quota-project: must be a valid GCP project ID (6..30 lowercase letters, digits, or hyphens)")
			}
			if optsMap == nil {
				optsMap = make(map[string]string, 1)
			}
			optsMap[provider.OptionQuotaProject] = qp
		}
		authReq = &hubclient.CreateSystemOneInstanceAuthRequest{
			Kind:   string(provider.AuthKindGoogleAccessToken),
			Scopes: cleanScopes,
		}

	default:
		return fmt.Errorf("unsupported --auth-kind %q (supported: api_key, google_access_token)", authKind)
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
		Auth:     authReq,
		Options:  optsMap,
		Limits:   limits,
		Enabled:  &enabled,
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
	if inst.Auth.Kind != "" {
		_, _ = fmt.Fprintf(out, "  Auth Kind:       %s\n", inst.Auth.Kind)
	}
	if inst.Auth.SecretName != "" {
		_, _ = fmt.Fprintf(out, "  Secret:   %s\n", inst.Auth.SecretName)
	}
	if inst.Auth.SecretRef != nil {
		_, _ = fmt.Fprintf(out, "  Secret Ref:      %s (scope: %s)\n", inst.Auth.SecretRef.Name, inst.Auth.SecretRef.Scope)
	}
	if len(inst.Auth.Scopes) > 0 {
		_, _ = fmt.Fprintf(out, "  Scopes:          %s\n", strings.Join(inst.Auth.Scopes, ", "))
	}
	if inst.Auth.QuotaProject != "" {
		_, _ = fmt.Fprintf(out, "  Quota Project:   %s\n", inst.Auth.QuotaProject)
	} else if inst.Auth.Kind == string(provider.AuthKindGoogleAccessToken) {
		_, _ = fmt.Fprintf(out, "  Quota Project:   %s\n", provider.FormatQuotaProjectProvenance(inst.Options[provider.OptionQuotaProject]))
	}
	if inst.Auth.CredentialType != "" {
		_, _ = fmt.Fprintf(out, "  Credential Type: %s\n", inst.Auth.CredentialType)
	}
	if inst.Auth.ServiceAccountEmail != "" {
		_, _ = fmt.Fprintf(out, "  Service Account: %s\n", inst.Auth.ServiceAccountEmail)
	}
	_, _ = fmt.Fprintf(out, "  Created:  %s\n", clitime.Format(inst.CreatedAt, clitime.Full))
	return nil
}

func validateAdminSystemOneCLIScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return nil, errors.New("--auth-scope is required with --auth-kind google_access_token (the required Decisions scope is unverified; pass --auth-scope explicitly)")
	}
	seen := make(map[string]struct{}, len(scopes))
	clean := make([]string, 0, len(scopes))
	for _, raw := range scopes {
		s := strings.TrimSpace(raw)
		if s == "" || s != raw {
			return nil, errors.New("invalid --auth-scope: must be a non-empty trimmed https://www.googleapis.com/auth/ URL")
		}
		if !strings.HasPrefix(s, provider.GoogleScopePrefix) || len(s) <= len(provider.GoogleScopePrefix) {
			return nil, fmt.Errorf("invalid --auth-scope %q: must start with %s", s, provider.GoogleScopePrefix)
		}
		for i := 0; i < len(s); i++ {
			b := s[i]
			if b < 0x21 || b > 0x7e || b == '?' || b == '#' {
				return nil, errors.New("invalid --auth-scope: contains disallowed URL characters")
			}
		}
		if _, dup := seen[s]; !dup {
			seen[s] = struct{}{}
			clean = append(clean, s)
		}
	}
	return clean, nil
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

	return validateAdminSystemOneAPIKeyBytes(raw)
}

func validateAdminSystemOneAPIKeyBytes(raw []byte) (string, error) {
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

func isReservedCLISystemOneSecretName(name string) bool {
	folded := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "_", "-")
	folded = strings.ReplaceAll(folded, ".", "-")
	return strings.HasPrefix(folded, "systemone-instance-")
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
	defer func() { _ = f.Close() }()
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
