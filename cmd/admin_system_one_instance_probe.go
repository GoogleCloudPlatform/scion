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
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	systemoneruntime "github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
	"github.com/spf13/cobra"
)

type adminSystemOneProbeFlags struct {
	models   bool
	project  string
	specFile string
	text     string
	textFile string
	kind     string
	ruleID   string
	params   []string
}

var adminSystemOneProbeOpts adminSystemOneProbeFlags

var adminSystemOneInstanceProbeCmd = &cobra.Command{
	Use:   "probe <name>",
	Short: "Probe a Hub System One provider instance",
	Long: `Run an administrative diagnostic probe against a Hub System One provider
instance, either listing available provider models (--models) or evaluating an
inline System One spec against synthetic probe text (--spec-file with --text or
--text-file).

Model listing (--models) requires an adapter that implements model discovery
(such as typesafe); adapters without model listing (such as gemini_decisions)
return an "adapter does not support model listing" validation error.

When probing a project- or project_or_hub-scoped named-secret instance with
--project, the caller must hold both project.read and project.secret_read on
that project (even when project_or_hub would fall back to the Hub secret).

Exits 0 when the probe completes with class "success", and non-zero when the
evaluated outcome class is not "success" (for example "failed" or "uncertain")
or the request fails. The probe result is still written to stdout before exiting.

Examples:
  # List models reported by a TypeSafe provider instance
  scion admin system-one instance probe typesafe-main --models

  # Evaluate an inline spec (JSON or YAML) against synthetic text on TypeSafe
  scion admin system-one instance probe typesafe-main \
    --spec-file ./probe-spec.json \
    --text "Synthetic test sample."

  # Evaluate an inline spec against synthetic text on Gemini Decision 1
  scion admin system-one instance probe gemini-main \
    --spec-file ./probe-spec.json \
    --text "Synthetic test sample."`,
	Args: cobra.ExactArgs(1),
	RunE: runAdminSystemOneInstanceProbe,
}

func init() {
	f := adminSystemOneInstanceProbeCmd.Flags()
	f.BoolVar(&adminSystemOneProbeOpts.models, "models", false, "Query models supported by the provider instance (requires adapter model-listing support, e.g. typesafe)")
	f.StringVar(&adminSystemOneProbeOpts.project, "project", "", "Optional project ID or slug providing evaluation project context for project- or project_or_hub-scoped named secrets (requires project.read and project.secret_read on that project)")
	f.StringVar(&adminSystemOneProbeOpts.specFile, "spec-file", "", "Path to inline System One spec or rule envelope (JSON or YAML, or '-' for stdin)")
	f.StringVar(&adminSystemOneProbeOpts.text, "text", "", "Synthetic text bound to probe.text")
	f.StringVar(&adminSystemOneProbeOpts.textFile, "text-file", "", "Path to file containing synthetic text bound to probe.text")
	f.StringVar(&adminSystemOneProbeOpts.kind, "kind", "", "Optional consumer projection kind (filter, route, judgment_bool, judgment_choice, judgments)")
	f.StringVar(&adminSystemOneProbeOpts.ruleID, "rule-id", "", "Optional rule identifier recorded in evaluation provenance")
	f.StringArrayVar(&adminSystemOneProbeOpts.params, "param", nil, "Optional rule parameter binding as KEY=VALUE (repeatable)")

	adminSystemOneInstanceCmd.AddCommand(adminSystemOneInstanceProbeCmd)
}

func runAdminSystemOneInstanceProbe(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	if name == "" {
		return errors.New("instance name is required")
	}

	req, err := buildAdminSystemOneProbeRequest(cmd, adminSystemOneProbeOpts)
	if err != nil {
		return err
	}

	svc, ctx, cancel, err := systemOneAdminService(cmd, adminSystemOneDefaultTimeout)
	if err != nil {
		return err
	}
	defer cancel()

	resp, err := svc.ProbeInstance(ctx, name, req)
	if err != nil {
		return err
	}

	failed := resp == nil || resp.Class != systemoneruntime.ClassSuccess
	if isJSONOutput() {
		if err := writeJSONTo(cmd.OutOrStdout(), resp); err != nil {
			return err
		}
		if failed {
			return &jsonReportedError{msg: formatAdminSystemOneProbeFailure(resp)}
		}
		return nil
	}

	renderAdminSystemOneProbeHuman(cmd.OutOrStdout(), resp)
	if failed {
		return errors.New(formatAdminSystemOneProbeFailure(resp))
	}
	return nil
}

func formatAdminSystemOneProbeFailure(resp *hubclient.ProbeSystemOneInstanceResponse) string {
	if resp == nil {
		return "probe returned an empty response"
	}
	if resp.Reason != "" {
		return fmt.Sprintf("probe returned class %q (%s)", resp.Class, resp.Reason)
	}
	return fmt.Sprintf("probe returned class %q", resp.Class)
}

func buildAdminSystemOneProbeRequest(cmd *cobra.Command, opts adminSystemOneProbeFlags) (*hubclient.ProbeSystemOneInstanceRequest, error) {
	specFile := strings.TrimSpace(opts.specFile)
	textFile := strings.TrimSpace(opts.textFile)
	textChanged := cmd.Flags().Changed("text")
	kind := strings.TrimSpace(opts.kind)
	ruleID := strings.TrimSpace(opts.ruleID)

	projectRef := strings.TrimSpace(opts.project)

	if opts.models {
		if specFile != "" || textChanged || textFile != "" || kind != "" || ruleID != "" || len(opts.params) > 0 {
			return nil, errors.New("--models cannot be combined with --spec-file, --text, --text-file, --kind, --rule-id, or --param")
		}
		return &hubclient.ProbeSystemOneInstanceRequest{
			Models:  true,
			Project: projectRef,
		}, nil
	}

	if specFile == "" {
		return nil, errors.New("either --models or --spec-file is required")
	}
	if textChanged && textFile != "" {
		return nil, errors.New("use either --text or --text-file, not both")
	}
	if !textChanged && textFile == "" {
		return nil, errors.New("either --text or --text-file is required when probing with --spec-file")
	}
	if specFile == "-" && textFile == "-" {
		return nil, errors.New("--spec-file and --text-file cannot both read from stdin ('-')")
	}

	var specBytes []byte
	var err error
	if specFile == "-" {
		specBytes, err = readBoundedReader(cmd.InOrStdin(), maxAdminSystemOneProbeReadBytes)
	} else {
		specBytes, err = readBoundedFile(specFile, maxAdminSystemOneProbeReadBytes)
	}
	if err != nil {
		return nil, fmt.Errorf("read --spec-file: %w", err)
	}
	specRaw, err := decodeYAMLOrJSONToRawMessage(specBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid --spec-file: %w", err)
	}

	probeText := opts.text
	if textFile != "" {
		var textBytes []byte
		if textFile == "-" {
			textBytes, err = readBoundedReader(cmd.InOrStdin(), maxAdminSystemOneProbeReadBytes)
		} else {
			textBytes, err = readBoundedFile(textFile, maxAdminSystemOneProbeReadBytes)
		}
		if err != nil {
			return nil, fmt.Errorf("read --text-file: %w", err)
		}
		probeText = string(textBytes)
	}

	params, err := parseAdminSystemOneKeyValues(opts.params, "--param")
	if err != nil {
		return nil, err
	}

	return &hubclient.ProbeSystemOneInstanceRequest{
		Project: projectRef,
		Kind:    kind,
		RuleID:  ruleID,
		Params:  params,
		Spec:    specRaw,
		Probe:   &hubclient.ProbeSystemOneInput{Text: probeText},
	}, nil
}

func renderAdminSystemOneProbeHuman(out io.Writer, resp *hubclient.ProbeSystemOneInstanceResponse) {
	if resp == nil {
		return
	}
	_, _ = fmt.Fprintf(out, "Class: %s\n", resp.Class)
	if resp.Reason != "" {
		_, _ = fmt.Fprintf(out, "Reason: %s\n", resp.Reason)
	}
	if len(resp.Models) > 0 {
		_, _ = fmt.Fprintf(out, "Models (%d):\n", len(resp.Models))
		for _, m := range resp.Models {
			_, _ = fmt.Fprintf(out, "  - %s\n", m)
		}
	}
	if resp.Decision != nil {
		switch {
		case resp.Decision.Bool != nil:
			_, _ = fmt.Fprintf(out, "Decision (%s): %t\n", resp.Decision.Kind, *resp.Decision.Bool)
		case resp.Decision.String != nil:
			_, _ = fmt.Fprintf(out, "Decision (%s): %s\n", resp.Decision.Kind, *resp.Decision.String)
		default:
			_, _ = fmt.Fprintf(out, "Decision (%s)\n", resp.Decision.Kind)
		}
	}
	if resp.Evaluation != nil {
		renderAdminSystemOneEvaluationHuman(out, resp.Evaluation)
	}
}

func renderAdminSystemOneEvaluationHuman(out io.Writer, eval *systemoneruntime.Evaluation) {
	_, _ = fmt.Fprintln(out, "Evaluation:")
	if eval.EvalID != "" {
		_, _ = fmt.Fprintf(out, "  Eval ID:              %s\n", eval.EvalID)
	}
	if eval.ConfiguredModel != "" {
		_, _ = fmt.Fprintf(out, "  Configured Model:     %s\n", eval.ConfiguredModel)
	}
	if eval.ResolvedModel != "" {
		_, _ = fmt.Fprintf(out, "  Resolved Model:       %s\n", eval.ResolvedModel)
	} else if eval.HasModelUnreported() {
		_, _ = fmt.Fprintf(out, "  Resolved Model:       unreported (%s)\n", provider.NoteModelUnreported)
	}
	_, _ = fmt.Fprintf(out, "  Model Alias Reported: %t\n", eval.HasModelAliasReported())
	if eval.Auth != nil {
		if eval.Auth.AuthKind != "" {
			_, _ = fmt.Fprintf(out, "  Auth Kind:            %s\n", eval.Auth.AuthKind)
		}
		if eval.Auth.SecretRef != nil && eval.Auth.SecretRef.Name != "" {
			switch {
			case eval.Auth.SecretRef.Scope != "" && eval.Auth.SecretRef.ScopeUsed != "":
				_, _ = fmt.Fprintf(out, "  Secret Ref:           %s (scope: %s, scope_used: %s)\n",
					eval.Auth.SecretRef.Name, eval.Auth.SecretRef.Scope, eval.Auth.SecretRef.ScopeUsed)
			case eval.Auth.SecretRef.ScopeUsed != "":
				_, _ = fmt.Fprintf(out, "  Secret Ref:           %s (scope_used: %s)\n",
					eval.Auth.SecretRef.Name, eval.Auth.SecretRef.ScopeUsed)
			case eval.Auth.SecretRef.Scope != "":
				_, _ = fmt.Fprintf(out, "  Secret Ref:           %s (scope: %s)\n",
					eval.Auth.SecretRef.Name, eval.Auth.SecretRef.Scope)
			default:
				_, _ = fmt.Fprintf(out, "  Secret Ref:           %s\n", eval.Auth.SecretRef.Name)
			}
		}
		if eval.Auth.SecretVersion > 0 {
			_, _ = fmt.Fprintf(out, "  Secret Version:       %d\n", eval.Auth.SecretVersion)
		}
		if len(eval.Auth.Scopes) > 0 {
			_, _ = fmt.Fprintf(out, "  Auth Scopes:          %s\n", strings.Join(eval.Auth.Scopes, ", "))
		}
		if eval.Auth.QuotaProject != "" {
			_, _ = fmt.Fprintf(out, "  Quota Project:        %s\n", eval.Auth.QuotaProject)
		} else if eval.Auth.AuthKind == string(provider.AuthKindGoogleAccessToken) {
			_, _ = fmt.Fprintf(out, "  Quota Project:        %s\n", provider.FormatQuotaProjectProvenance(""))
		}
		if eval.Auth.CredentialType != "" {
			_, _ = fmt.Fprintf(out, "  Credential Type:      %s\n", eval.Auth.CredentialType)
		}
		if eval.Auth.ServiceAccountEmail != "" {
			_, _ = fmt.Fprintf(out, "  Service Account:      %s\n", eval.Auth.ServiceAccountEmail)
		}
		if eval.Auth.ReasonHint != "" {
			_, _ = fmt.Fprintf(out, "  Auth Hint:            %s\n", eval.Auth.ReasonHint)
		}
	}
	if eval.Usage != nil && !eval.HasUsageUnreported() {
		if eval.Usage.Truncated != nil {
			_, _ = fmt.Fprintf(out, "  Usage:                input_tokens=%d, output_tokens=%d, truncated=%t\n",
				eval.Usage.InputTokens, eval.Usage.OutputTokens, *eval.Usage.Truncated)
		} else {
			_, _ = fmt.Fprintf(out, "  Usage:                input_tokens=%d, output_tokens=%d\n",
				eval.Usage.InputTokens, eval.Usage.OutputTokens)
		}
	} else if eval.HasUsageUnreported() {
		_, _ = fmt.Fprintf(out, "  Usage:                usage unavailable (%s)\n", provider.NoteUsageUnreported)
	} else {
		_, _ = fmt.Fprintf(out, "  Usage:                usage unavailable\n")
	}
	if len(eval.Notes) > 0 {
		_, _ = fmt.Fprintf(out, "  Notes:                %s\n", strings.Join(eval.Notes, ", "))
	}
	if eval.Elapsed > 0 {
		_, _ = fmt.Fprintf(out, "  Elapsed:              %s\n", eval.Elapsed)
	}

	if len(eval.Answers) > 0 {
		_, _ = fmt.Fprintln(out, "  Answers:")
		qids := make([]string, 0, len(eval.Answers))
		for qid := range eval.Answers {
			qids = append(qids, qid)
		}
		sort.Strings(qids)
		for _, qid := range qids {
			ans := eval.Answers[qid]
			val := ans.Status
			switch {
			case ans.Bool != nil:
				val = fmt.Sprintf("%t", *ans.Bool)
			case ans.Choice != nil:
				val = *ans.Choice
			case ans.Level != nil:
				val = *ans.Level
			}
			_, _ = fmt.Fprintf(out, "    %s (%s): %s\n", qid, ans.Primitive, val)
			if len(ans.Distribution) > 0 {
				opts := make([]string, 0, len(ans.Distribution))
				for opt := range ans.Distribution {
					opts = append(opts, opt)
				}
				sort.Strings(opts)
				parts := make([]string, 0, len(opts))
				for _, opt := range opts {
					parts = append(parts, fmt.Sprintf("%s=%.4f", opt, ans.Distribution[opt]))
				}
				_, _ = fmt.Fprintf(out, "      distribution: %s\n", strings.Join(parts, ", "))
			}
			if len(ans.Metrics) > 0 {
				names := make([]string, 0, len(ans.Metrics))
				for name := range ans.Metrics {
					names = append(names, name)
				}
				sort.Strings(names)
				parts := make([]string, 0, len(names))
				for _, name := range names {
					parts = append(parts, fmt.Sprintf("%s=%.4f", name, ans.Metrics[name].Value))
				}
				_, _ = fmt.Fprintf(out, "      metrics: %s\n", strings.Join(parts, ", "))
			}
			if len(ans.NativeMetrics) > 0 {
				names := make([]string, 0, len(ans.NativeMetrics))
				for name := range ans.NativeMetrics {
					names = append(names, name)
				}
				sort.Strings(names)
				parts := make([]string, 0, len(names))
				for _, name := range names {
					parts = append(parts, fmt.Sprintf("%s=%.4f", name, ans.NativeMetrics[name].Value))
				}
				_, _ = fmt.Fprintf(out, "      native_metrics: %s\n", strings.Join(parts, ", "))
			}
			if len(ans.Notes) > 0 {
				_, _ = fmt.Fprintf(out, "      notes: %s\n", strings.Join(ans.Notes, ", "))
			}
		}
	}

	if len(eval.Attempts) > 0 {
		_, _ = fmt.Fprintln(out, "  Attempts:")
		for i, att := range eval.Attempts {
			_, _ = fmt.Fprintf(out, "    [%d] instance=%s adapter=%s outcome=%s status=%s duration=%s requests=%d retries=%d\n",
				i+1,
				att.Instance,
				att.Adapter,
				att.Outcome,
				att.HTTPStatusClass,
				att.Duration,
				att.Requests,
				att.Retries,
			)
		}
	}
}
