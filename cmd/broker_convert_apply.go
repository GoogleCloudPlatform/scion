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
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/brokerconvert"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
)

// convertApplyTimeout bounds one --apply, --status or --rollback run.
const convertApplyTimeout = 10 * time.Minute

// convertEnvHook lets tests replace parts of the environment the write
// modes use (a fake Hub, a fake registration, a fake server probe).
var convertEnvHook func(env *brokerconvert.Env)

// convertServerRunning is P-4's local probe: whether anything listens on
// this host's Runtime Broker port. Any listener counts, so a foreign
// process on the port also refuses (fail closed). A variable for tests.
var convertServerRunning = func(ctx context.Context) bool {
	cfg, err := config.LoadGlobalConfig("")
	if err != nil || cfg == nil {
		return true
	}
	return checkPort(cfg.RuntimeBroker.Host, cfg.RuntimeBroker.Port).inUse
}

// convertConfirm asks the operator to confirm an apply or a rollback.
var convertConfirm = func(prompt string) bool {
	return hubsync.ConfirmAction(prompt, false, autoConfirm)
}

// runBrokerConvertWriteModes runs --apply, --status or --rollback. Every
// output goes through the convert report, which carries step states, IDs
// and fixed texts only; no settings value reaches it (sec R2).
func runBrokerConvertWriteModes(cmd *cobra.Command) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), convertApplyTimeout)
	defer cancel()
	out := cmd.OutOrStdout()

	env, settings, err := newConvertEnv()
	if err != nil {
		return err
	}

	var rep *brokerconvert.Report
	switch {
	case brokerConvertStatus:
		rep, err = brokerconvert.Status(ctx, env)
	case brokerConvertRollback:
		if !convertConfirmTo(cmd.ErrOrStderr(), "Roll back the recorded convert run on this host? The flat instance is deregistered and its identity moved aside.") {
			return errors.New("rollback not confirmed")
		}
		rep, err = brokerconvert.Rollback(ctx, env)
	default:
		rep, err = runConvertApply(ctx, cmd, env, settings)
	}
	if rep != nil {
		if werr := writeConvertReport(out, rep); werr != nil {
			return werr
		}
	}
	if err != nil {
		var ref *brokerconvert.Refusal
		if errors.As(err, &ref) {
			return &convertPlanExit{code: 1, msg: "convert --" + rep.Mode + " refused: " + ref.Text, json: isJSONOutput()}
		}
		return err
	}
	return nil
}

// convertConfirmTo prints prompt to w for the operator and asks.
func convertConfirmTo(w io.Writer, prompt string) bool {
	if autoConfirm {
		return true
	}
	_, _ = fmt.Fprintln(w, prompt)
	return convertConfirm("Continue?")
}

func writeConvertReport(w io.Writer, rep *brokerconvert.Report) error {
	if isJSONOutput() {
		return brokerconvert.WriteReportJSON(w, rep)
	}
	return brokerconvert.WriteReportHuman(w, rep)
}

// runConvertApply builds the plan with the same builder and inputs as
// --plan, shows it (redacted, as --plan does), asks for confirmation and
// applies it.
func runConvertApply(ctx context.Context, cmd *cobra.Command, env *brokerconvert.Env, settings *config.Settings) (*brokerconvert.Report, error) {
	in, _, err := loadConvertInput(cmd)
	if err != nil {
		return nil, err
	}
	plan, err := brokerconvert.BuildLocal(in)
	if errors.Is(err, brokerconvert.ErrInstancesConfigured) {
		// An interrupted apply already wrote server.broker.instances (its
		// only settings change): rebuild the plan from the settings without
		// that key, and the evidence copy's raw bytes. The digest check in
		// Apply refuses if anything else changed.
		in, err = resumeConvertInput(env.GlobalDir, in)
		if err != nil {
			return nil, err
		}
		plan, err = brokerconvert.BuildLocal(in)
	}
	if err != nil {
		return nil, err
	}
	if err := brokerconvert.CollectHub(ctx, env.Client, getCurrentHostBrokerID(settings), plan); err != nil {
		return nil, err
	}
	if !isJSONOutput() {
		if err := brokerconvert.WriteHuman(cmd.ErrOrStderr(), plan, brokerconvert.HumanOptions{}); err != nil {
			return nil, err
		}
	}
	digest, err := brokerconvert.PlanDigest(plan, in.Choices)
	if err != nil {
		return nil, err
	}
	if !convertConfirmTo(cmd.ErrOrStderr(), "Apply this plan (digest "+digest+") to this host? The server must be stopped; the switch takes effect at the next 'scion server start'.") {
		return nil, errors.New("apply not confirmed")
	}
	defaultProfile := ""
	if in.Settings != nil {
		defaultProfile = in.Settings.ActiveProfile
	}
	return brokerconvert.Apply(ctx, env, brokerconvert.ApplyInput{Plan: plan, Choices: in.Choices, LegacyDefaultProfile: defaultProfile})
}

// newConvertEnv wires the convert engine to this host and the operator's
// Hub login.
func newConvertEnv() (*brokerconvert.Env, *config.Settings, error) {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return nil, nil, fmt.Errorf("resolving the global settings directory: %w", err)
	}
	settings, err := config.LoadSettings(globalDir)
	if err != nil {
		return nil, nil, fmt.Errorf("reading the global settings: %w", err)
	}
	env := &brokerconvert.Env{
		GlobalDir:     globalDir,
		ServerRunning: convertServerRunning,
		LoadInstances: func() ([]config.V1RuntimeBrokerInstanceConfig, error) {
			return config.LoadRuntimeBrokerInstances("")
		},
		ReadSettings: func() ([]byte, error) {
			path := config.GetSettingsPath(globalDir)
			if path == "" {
				return nil, config.ErrInstancesSettingsMissing
			}
			return os.ReadFile(path)
		},
		WriteInstances: func(instances []config.V1RuntimeBrokerInstanceConfig) error {
			return config.WriteRuntimeBrokerInstances(globalDir, instances)
		},
		RemoveInstances: func(expected []config.V1RuntimeBrokerInstanceConfig) error {
			return config.RemoveRuntimeBrokerInstances(globalDir, expected)
		},
		Now: time.Now,
	}
	client, err := convertHubClient(settings)
	switch {
	case errors.Is(err, errHubNotConfigured) && brokerConvertStatus:
		// --status without a Hub: the journal only.
	case err != nil:
		return nil, nil, err
	default:
		env.Client = client
	}
	env.Register = func(ctx context.Context, inst config.V1RuntimeBrokerInstanceConfig) (string, string, error) {
		endpoint := invocationHubEndpoint()
		if endpoint == "" {
			return "", "", errHubNotConfigured
		}
		if err := checkBrokerHubURL(endpoint); err != nil {
			return "", "", err
		}
		// The Hub's kept-settings notes are not printed: the report is the
		// only output.
		creds, id, err := registerConfiguredFlatInstance(ctx, settings, globalDir, endpoint, inst, io.Discard)
		if err != nil {
			return "", "", err
		}
		return creds.BrokerID, id.RuntimeTarget.ID, nil
	}
	if convertEnvHook != nil {
		convertEnvHook(env)
	}
	return env, settings, nil
}

// errNoApplyToResume refuses --apply on flat settings with no interrupted
// apply recorded.
var errNoApplyToResume = errors.New("server.broker.instances is already set and no interrupted apply is recorded on this host; there is nothing to convert (see 'scion runtime-broker convert --status')")

// resumeConvertInput is the plan input of an apply whose step 2 wrote
// server.broker.instances (interrupted later, or complete): the effective
// settings without that key and the evidence copy of the pre-apply
// settings file.
func resumeConvertInput(globalDir string, in brokerconvert.Input) (brokerconvert.Input, error) {
	j, err := brokerconvert.LoadJournal(globalDir)
	if err != nil {
		return in, err
	}
	// An applied run is rebuilt too, so a second --apply reports the
	// no-op (after its digest check) instead of an error.
	if j == nil || (j.State != brokerconvert.JournalApplying && j.State != brokerconvert.JournalApplied) || j.EvidenceFile == "" {
		return in, errNoApplyToResume
	}
	raw, err := brokerconvert.ReadRunCopy(globalDir, j.EvidenceFile)
	if err != nil {
		return in, err
	}
	if in.Settings != nil && in.Settings.Server != nil && in.Settings.Server.Broker != nil {
		vs := *in.Settings
		server := *vs.Server
		broker := *server.Broker
		broker.Instances = nil
		server.Broker = &broker
		vs.Server = &server
		in.Settings = &vs
	}
	in.RawSettings = raw
	return in, nil
}
