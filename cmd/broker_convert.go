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
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/GoogleCloudPlatform/scion/internal/writesonread"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerconvert"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var (
	brokerConvertPlan     bool
	brokerConvertApply    bool
	brokerConvertStatus   bool
	brokerConvertRollback bool
	brokerConvertProfiles []string
	brokerConvertChoices  string
	brokerConvertSkipHub  bool
	brokerConvertShowEnv  bool
)

// errConvertOneMode refuses convert without exactly one mode.
var errConvertOneMode = errors.New("give exactly one of --plan, --apply, --status or --rollback")

// errConvertPlanFlag refuses a --plan-only flag with another mode.
var errConvertPlanFlag = errors.New("--skip-hub and --show-env-values apply to --plan only")

// errConvertStatusFlag refuses plan inputs with --status or --rollback,
// which work from the recorded run.
var errConvertStatusFlag = errors.New("--profiles and --choices apply to --plan and --apply only; --status and --rollback work from the recorded run")

// errShowEnvValuesJSON refuses --show-env-values with JSON output: the JSON
// plan never carries env values.
var errShowEnvValuesJSON = errors.New("--show-env-values applies to the human output only; the JSON plan never contains env values")

// brokerConvertShort is the command's one-line description, also used in
// the runtime-broker command list.
const brokerConvertShort = "Plan, apply or roll back a move to flat Runtime Brokers (experimental)"

// brokerConvertCmd plans the move of this legacy Runtime Broker host to
// flat Runtime Broker instances. It is read-only: it writes no file (not
// even a settings migration), registers nothing and sends only GET
// requests to the Hub.
var brokerConvertCmd = &cobra.Command{
	Use:   "convert (--plan | --apply | --status | --rollback)",
	Short: brokerConvertShort,
	Long: `Plan a move from this host's legacy, profile-based Runtime Broker to flat
Runtime Broker instances (experimental, like flat Runtime Brokers themselves).
Nothing is written, registered or changed, locally or on the Hub.

The plan has five sections:
  1. the proposed server.broker.instances entries, as YAML to paste into the
     global settings file;
  2. the profile -> instance map;
  3. the conversion findings, blocking and notice;
  4. the proposed per-instance agent limits, or the decision you must make;
  5. the agents and projects on the Hub still bound to this Runtime Broker.

Sections 4 and 5 use your Hub login and this host's registered Runtime Broker
ID. With --skip-hub, or when no Hub is configured, only the local sections are
printed.

Harness env values are redacted: shown as <redacted> in the human output,
and omitted from the JSON plan, which lists their keys only. With
--show-env-values the human output prints them, and then contains secrets:
do not share it. --show-env-values is refused with --format json; the JSON
plan never contains env values, kubeconfig paths, contexts or secret
targets (it carries the instances in their publishable form). Findings are
printed from their code and settings key path with a fixed text, never with
a settings value.

Exactly one mode is required:
  --plan      print the plan (read-only; described above)
  --apply     convert this drained legacy host to the plan's flat instances:
              register them on the Hub, write server.broker.instances to
              settings.yaml, and link every mapped instance to every project
              this Runtime Broker provides, re-pointing defaults. The server
              must be stopped, and no non-terminal agent may be bound to this
              Runtime Broker. The switch takes effect at the next
              'scion server start'. Re-running resumes an interrupted apply
              of the same plan; a changed plan is refused.
              On a co-located host (the server process runs the Hub and
              embeds the Runtime Broker) --apply runs in two steps: with the
              server running it registers the instances and re-points
              projects; once you stop the server, a second --apply writes
              the settings. Stopping a co-located server takes the whole
              Hub down, so plan a maintenance window. After the next start,
              a third --apply removes the unused instance credentials files.
              With an external Hub database, the second step needs
              --single-hub-replica (exactly one Hub process serves it).
              The mode is read from the saved daemon launch: if neither the
              server nor the Runtime Broker was ever started through the
              daemon (foreground only), --apply and --rollback refuse
              (colocated_mode_unknown) until one of them is.
  --status    show the recorded run: step states and IDs only (read-only)
  --rollback  undo an apply: restore defaults, remove the instance links,
              deregister the instances, remove server.broker.instances and
              move the instance identities aside. The server must be stopped.
              On a co-located host it also runs in two steps: the Hub part
              with the server running, the local part after the stop. Its
              notes on a row named from an instance identity file appear
              in the first step's output only.

--apply and --rollback ask for confirmation unless --yes is given. They,
and --status, print only step states, IDs and fixed texts; no settings
value is printed. A copy of the settings file as it was before apply, and
of what apply wrote, is kept owner-only in
<global>/runtime-brokers/.convert/; both hold settings values: do not
share them.

Exit status: 0 when the plan has no blocking finding and needs no decision,
3 when it has a blocking finding or needs an operator decision, 1 on error.
A capacity plan the Hub could not supply (no Hub, or the Runtime Broker's
settings not visible to you) does not change the exit status; the summary
says so.

Examples:
  # Plan the move of every profile
  scion runtime-broker convert --plan

  # Plan two profiles only, with reviewed choices, as JSON
  scion runtime-broker convert --plan --profiles docker,k8s --choices choices.yaml --format json

  # The local conversion only, with no Hub requests
  scion runtime-broker convert --plan --skip-hub

  # Apply the reviewed plan, then check it
  scion runtime-broker convert --apply --choices choices.yaml
  scion runtime-broker convert --status

  # Undo the apply
  scion runtime-broker convert --rollback`,
	Args: cobra.NoArgs,
	RunE: runBrokerConvert,
}

func init() {
	brokerCmd.AddCommand(brokerConvertCmd)
	brokerConvertCmd.Flags().BoolVar(&brokerConvertPlan, "plan", false, "Print the plan without changing anything")
	brokerConvertCmd.Flags().BoolVar(&brokerConvertApply, "apply", false, "Convert this drained legacy host to the plan's flat instances (the server must be stopped)")
	brokerConvertCmd.Flags().BoolVar(&brokerConvertStatus, "status", false, "Show the recorded convert run (read-only)")
	brokerConvertCmd.Flags().BoolVar(&brokerConvertRollback, "rollback", false, "Undo the recorded apply (the server must be stopped)")
	brokerConvertCmd.Flags().StringSliceVar(&brokerConvertProfiles, "profiles", nil, "Limit the conversion to these profiles (default: all)")
	brokerConvertCmd.Flags().StringVar(&brokerConvertChoices, "choices", "", "YAML or JSON file of reviewed operator choices (split groups or field choices)")
	brokerConvertCmd.Flags().BoolVar(&brokerConvertSkipHub, "skip-hub", false, "Plan from the local settings only: no Hub requests, no Hub sections")
	brokerConvertCmd.Flags().BoolVar(&brokerConvertShowEnv, "show-env-values", false, "Print harness env values in the human output; the output then contains secrets and must not be shared (refused with --format json)")
}

// convertPlanExit carries the exit status of a produced plan (exitCoder).
// With JSON output the plan was already printed, so it unwraps to a
// jsonReportedError and nothing more is printed.
type convertPlanExit struct {
	code int
	msg  string
	json bool
}

func (e *convertPlanExit) Error() string { return e.msg }

// ExitCode implements exitCoder.
func (e *convertPlanExit) ExitCode() int { return e.code }

func (e *convertPlanExit) Unwrap() error {
	if e.json {
		return &jsonReportedError{msg: e.msg}
	}
	return nil
}

// convertHubClient opens the Hub client for the plan's Hub sections. A
// variable so tests can point it at a fake Hub.
var convertHubClient = func(settings *config.Settings) (hubclient.Client, error) {
	return getHubClient(settings)
}

func runBrokerConvert(cmd *cobra.Command, _ []string) error {
	modes := 0
	for _, m := range []bool{brokerConvertPlan, brokerConvertApply, brokerConvertStatus, brokerConvertRollback} {
		if m {
			modes++
		}
	}
	if modes != 1 {
		return errConvertOneMode
	}
	if brokerConvertSingleHubReplica && !brokerConvertApply {
		return errConvertSingleHubFlag
	}
	if brokerConvertHostMode != "" {
		if !brokerConvertApply && !brokerConvertRollback {
			return errConvertHostModeFlag
		}
		if brokerConvertHostMode != brokerconvert.HostModeColocated && brokerConvertHostMode != brokerconvert.HostModeRemote {
			return errConvertHostModeValue
		}
	}
	if !brokerConvertPlan {
		if brokerConvertSkipHub || brokerConvertShowEnv {
			return errConvertPlanFlag
		}
		if !brokerConvertApply && (len(brokerConvertProfiles) > 0 || brokerConvertChoices != "") {
			return errConvertStatusFlag
		}
		// The write modes and --status keep the no-write guard on for
		// read paths (isNoWriteCommand; convert-apply design section 3
		// and erratum E1): their explicit writers do not consult it.
		return runBrokerConvertWriteModes(cmd)
	}
	if brokerConvertShowEnv && isJSONOutput() {
		return errShowEnvValuesJSON
	}
	// Read-only: the settings loaders keep a legacy key's value in memory
	// instead of rewriting the file. The root hook already set this before
	// its own settings loads (isNoWriteCommand); setting it here covers a
	// direct call too.
	writesonread.Set()

	in, settings, err := loadConvertInput(cmd)
	if err != nil {
		return err
	}
	plan, err := brokerconvert.BuildLocal(in)
	if err != nil {
		return err
	}

	if !brokerConvertSkipHub && plan.Summary.ProfilesInScope > 0 {
		client, err := convertHubClient(settings)
		switch {
		case errors.Is(err, errHubNotConfigured):
			// No Hub: the local sections only, as with --skip-hub.
		case err != nil:
			return err
		default:
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			if err := brokerconvert.CollectHub(ctx, client, getCurrentHostBrokerID(settings), plan); err != nil {
				return err
			}
		}
	}

	if g, gerr := config.GetGlobalDir(); gerr == nil {
		plan.HostMode = planHostMode(g)
	}

	out := cmd.OutOrStdout()
	if isJSONOutput() {
		err = brokerconvert.WriteJSON(out, plan)
	} else {
		err = brokerconvert.WriteHuman(out, plan, brokerconvert.HumanOptions{ShowEnvValues: brokerConvertShowEnv})
		if err == nil && plan.HostMode != "" {
			if g, gerr := config.GetGlobalDir(); gerr == nil {
				_, err = fmt.Fprintln(out, planHostModeLine(g))
			}
		}
	}
	if err != nil {
		return err
	}
	return planExit(plan)
}

func planExit(plan *brokerconvert.Plan) error {
	switch plan.ExitCode() {
	case brokerconvert.ExitBlocked:
		return &convertPlanExit{code: brokerconvert.ExitBlocked, msg: "the plan has blocking findings or needs an operator decision", json: isJSONOutput()}
	case brokerconvert.ExitError:
		return &convertPlanExit{code: brokerconvert.ExitError, msg: "the plan is incomplete: a Hub request failed (see the notes above)", json: isJSONOutput()}
	}
	return nil
}

// loadConvertInput reads this host's effective legacy settings, the raw
// global settings file and the choices file. It only reads. Settings
// warnings go to cmd's error stream.
func loadConvertInput(cmd *cobra.Command) (brokerconvert.Input, *config.Settings, error) {
	var in brokerconvert.Input
	vs, warnings, err := config.LoadGlobalSettingsWithOverlay()
	if err != nil {
		return in, nil, fmt.Errorf("reading the global settings: %w", err)
	}
	for _, w := range warnings {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %s\n", w)
	}
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return in, nil, fmt.Errorf("resolving the global settings directory: %w", err)
	}
	if path := config.GetSettingsPath(globalDir); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return in, nil, fmt.Errorf("reading %s: %w", path, err)
		}
		in.RawSettings = raw
	}
	settings, err := config.LoadSettings(globalDir)
	if err != nil {
		return in, nil, fmt.Errorf("reading the global settings: %w", err)
	}
	if names, ok := config.ConfiguredRuntimeNames(); ok {
		in.ConfiguredRuntimes = func(name string) bool { return names[name] }
	}
	if brokerConvertChoices != "" {
		data, err := os.ReadFile(brokerConvertChoices)
		if err != nil {
			return in, nil, fmt.Errorf("reading the choices file: %w", err)
		}
		if in.Choices, err = brokerconvert.ParseChoices(data); err != nil {
			return in, nil, fmt.Errorf("%s: %w", brokerConvertChoices, err)
		}
	}
	in.Settings = vs
	in.Profiles = brokerConvertProfiles
	return in, settings, nil
}
