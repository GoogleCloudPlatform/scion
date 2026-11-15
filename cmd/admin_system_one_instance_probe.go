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
	systemoneruntime "github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
	"github.com/spf13/cobra"
)

type adminSystemOneProbeFlags struct {
	models   bool
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

Examples:
  # List models reported by the provider instance
  scion admin system-one instance probe typesafe-main --models

  # Evaluate an inline spec (JSON or YAML) against synthetic text
  scion admin system-one instance probe typesafe-main \
    --spec-file ./probe-spec.json \
    --text "Synthetic test sample."`,
	Args: cobra.ExactArgs(1),
	RunE: runAdminSystemOneInstanceProbe,
}

func init() {
	f := adminSystemOneInstanceProbeCmd.Flags()
	f.BoolVar(&adminSystemOneProbeOpts.models, "models", false, "Query models supported by the provider instance")
	f.StringVar(&adminSystemOneProbeOpts.specFile, "spec-file", "", "Path to inline System One spec or rule envelope (JSON or YAML, or '-' for stdin)")
	f.StringVar(&adminSystemOneProbeOpts.text, "text", "", "Synthetic text bound to probe.text")
	f.StringVar(&adminSystemOneProbeOpts.textFile, "text-file", "", "Path to file containing synthetic text bound to probe.text")
	f.StringVar(&adminSystemOneProbeOpts.kind, "kind", "", "Optional consumer projection kind (filter, route, judgment_bool, judgment_choice, judgment_score, judgments)")
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

	if isJSONOutput() {
		return writeJSONTo(cmd.OutOrStdout(), resp)
	}

	renderAdminSystemOneProbeHuman(cmd.OutOrStdout(), resp)
	return nil
}

func buildAdminSystemOneProbeRequest(cmd *cobra.Command, opts adminSystemOneProbeFlags) (*hubclient.ProbeSystemOneInstanceRequest, error) {
	specFile := strings.TrimSpace(opts.specFile)
	textFile := strings.TrimSpace(opts.textFile)
	textChanged := cmd.Flags().Changed("text")
	kind := strings.TrimSpace(opts.kind)
	ruleID := strings.TrimSpace(opts.ruleID)

	if opts.models {
		if specFile != "" || textChanged || textFile != "" || kind != "" || ruleID != "" || len(opts.params) > 0 {
			return nil, errors.New("--models cannot be combined with --spec-file, --text, --text-file, --kind, --rule-id, or --param")
		}
		return &hubclient.ProbeSystemOneInstanceRequest{Models: true}, nil
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
		Kind:   kind,
		RuleID: ruleID,
		Params: params,
		Spec:   specRaw,
		Probe:  &hubclient.ProbeSystemOneInput{Text: probeText},
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
	}
	_, _ = fmt.Fprintf(out, "  Model Alias Reported: %t\n", eval.HasModelAliasReported())
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
