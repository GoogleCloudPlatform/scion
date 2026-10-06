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
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var (
	reincarnateHandoffFile     string
	reincarnateDryRun          bool
	reincarnateHandoffTemplate bool
	reincarnateBroker          string
	reincarnateSharedDirs      []string
	reincarnateAllowEmptySD    bool
)

// reincarnateFiveLineContract is the design §3.9 self-migration contract,
// verbatim, stated both in `--help` (embedded in Long below) and usable
// wherever the exact wording needs to be pinned (golden tests). It is a
// plain (not raw) string literal because it contains backtick-quoted
// commands.
const reincarnateFiveLineContract = "1. Commit and push your branch.\n" +
	"2. Write a handoff file.\n" +
	"3. Run `scion reincarnate --dry-run` to see what changes.\n" +
	"4. Run `scion reincarnate --handoff-file <f>`.\n" +
	"5. Do nothing after that call; your container will be stopped.\n"

// reincarnateHandoffTemplateText is the embedded handoff template printed by
// `scion reincarnate --handoff-template` (design §3.9). Its section order
// and names follow §3.9's "Handoff template sections" list; the prose under
// each heading is derived from the prior-art handoff at
// /scion-volumes/scratchpad/integration-design-handoff.md, generalized away
// from that document's specific project and role.
const reincarnateHandoffTemplateText = `# Reincarnation handoff

Prepared by the outgoing generation, for the generation that replaces it.

## Role charter

What this role is for and what it owns. State the charter in your own words
so the next generation does not have to re-derive it from scattered
messages.

## Immediate active work (status, next action)

The current status of the work in progress, and the concrete next action to
take first.

## Canonical files and artifacts

Absolute paths to the design docs, reports, and scratchpad files the next
generation needs to read before acting. Note which one is authoritative if
they disagree.

## Authority and ownership (who to ask, who can approve)

Who owns this work, who can approve changes or exceptions, and who to
escalate to if blocked.

## Live conversations (conv ids) and counterparties

Conversation IDs you are actively part of, and who is on the other end of
each, so the next generation can pick the threads back up.

## Children agents and their state

Any agents you created or supervise: their names, roles, and current
status.

## Pending waits and scheduled events

Anything you are waiting on (a reply, another agent, a scheduled event),
and when to expect it.

## Open questions to humans (already asked and not yet asked)

Questions you have already asked and are waiting on an answer for, and
questions you have not yet asked but the next generation should raise.

## Operating constraints and lessons learned

Rules, gotchas, and mistakes to avoid that are not obvious from the code or
the design docs.

## Do not redo

Work that is already done. Redoing it would waste time or cause harm — for
example, re-sending a message, re-running a destructive operation, or
re-opening a decision that is already settled.
`

// reincarnateCmd represents the `scion reincarnate` command (design
// /scion-volumes/scratchpad/projects/agent-migrate/design.md §3.2): stop an
// agent, re-resolve its configuration against the current template/harness
// catalog, and start a fresh generation with the same identity, handing it
// an agent-authored handoff as its first task. Phase 1 supports only the
// handoff and dry-run; every override flag (--image, --model, --harness,
// --rollback, etc.) is design-scoped for later phases.
var reincarnateCmd = &cobra.Command{
	Use:   "reincarnate [agent]",
	Short: "Migrate an agent to a fresh generation (new template/config, same identity)",
	Long: `Reincarnate stops an agent, re-resolves its configuration against the
current template and harness-config catalog, and starts a new generation
with the same agent ID and slug. The new generation's first task is a
hub-built preamble plus the handoff you provide with --handoff-file.

Run with no argument inside an agent container to migrate yourself
(self-migration); a handoff file is required in that case, since there is no
one else to describe the work in progress. When migrating another agent, the
handoff is optional.

Self-migration follows this contract:

` + reincarnateFiveLineContract + `
Run ` + "`scion reincarnate --handoff-template`" + ` to print the handoff's expected
sections.

Use --dry-run to see the planned changes (template, image, harness config,
model, env keys, branch) without migrating anything.

Use --broker <name|id> to move the agent to another runtime broker. Both
brokers must mount the same NFS export, so the workspace moves without being
copied. Only --dry-run is supported with --broker for now: it reports each
eligibility check and changes nothing.

Use --shared-dir-backend NAME=nfs to move a shared dir's recorded storage
backend from local to nfs. Only this agent's record changes, while the
directory belongs to the project: stop every agent that uses it, copy the
local directory's contents into the nfs directory keeping ownership, modes,
the setgid bit and ACLs (for example rsync -aAX LOCAL/ NFS/, then check the
nfs directory with getfacl), then reincarnate each of those agents with the
flag. The local directory is never moved or deleted. The start
refuses an empty nfs directory while the previous local directory is not
empty (on Kubernetes, whenever the nfs directory is empty); add
--allow-empty-shared-dir to start anyway.

The broker checks the change (the dir is one of the agent's shared dirs,
the nfs settings are complete, the broker supports it) after the hub has
stopped the agent. If the broker refuses, the reincarnation fails and the
agent stays stopped with its record unchanged. --dry-run does not run these
broker checks.`,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) > 1 {
			return fmt.Errorf("accepts at most 1 argument (agent name)")
		}
		return nil
	},
	ValidArgsFunction: getAgentNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		// --handoff-template is a pure, local, offline operation (design
		// §3.9: "The template is embedded in the CLI binary, not stored as a
		// skill file"): it never needs a hub connection or a resolved
		// target, so it is handled before any of that and short-circuits
		// the rest of RunE regardless of other flags or arguments.
		if reincarnateHandoffTemplate {
			_, err := fmt.Fprint(cmd.OutOrStdout(), reincarnateHandoffTemplateText)
			return err
		}

		if err := validateReincarnateBrokerFlags(reincarnateBroker, reincarnateDryRun); err != nil {
			return err
		}
		agentName, isSelf, err := resolveReincarnateTarget(args, os.Getenv("SCION_AGENT_NAME"), reincarnateHandoffFile != "", reincarnateDryRun)
		if err != nil {
			return err
		}

		var handoff string
		if reincarnateHandoffFile != "" {
			data, err := os.ReadFile(reincarnateHandoffFile)
			if err != nil {
				return fmt.Errorf("failed to read --handoff-file: %w", err)
			}
			handoff = string(data)
		}

		hubCtx, err := CheckHubAvailabilityForAgent(projectPath, agentName, true)
		if err != nil {
			return err
		}
		if hubCtx == nil {
			return fmt.Errorf("agent migration requires hub mode")
		}

		return reincarnateAgentViaHub(hubCtx, agentName, handoff, isSelf)
	},
}

// resolveReincarnateTarget implements the self/handoff-required rule
// (design §3.4 Amendment A3.11): resolve the target agent name
// from the positional arg or $SCION_AGENT_NAME, decide whether this is a
// self-migration, and require a handoff for one — unless this is a dry run,
// which migrates nothing and so has nothing to hand off. Split out from RunE
// so the decision itself (no cobra, no file I/O, no hub client) is directly
// testable.
func resolveReincarnateTarget(args []string, selfName string, hasHandoffFile, dryRun bool) (agentName string, isSelf bool, err error) {
	if len(args) == 1 {
		agentName = api.Slugify(args[0])
	} else {
		if selfName == "" {
			return "", false, newUsageError("specify an agent name, or run this inside an agent container to migrate yourself")
		}
		agentName = api.Slugify(selfName)
	}

	isSelf = selfName != "" && api.Slugify(selfName) == agentName

	if isSelf && !hasHandoffFile && !dryRun {
		return "", false, fmt.Errorf("self-migration requires --handoff-file: run `scion reincarnate --handoff-template` to see the expected sections, write your handoff, then pass it with --handoff-file")
	}

	return agentName, isSelf, nil
}

func reincarnateAgentViaHub(hubCtx *HubContext, agentName, handoff string, isSelf bool) error {
	if err := validateReincarnateBrokerFlags(reincarnateBroker, reincarnateDryRun); err != nil {
		return err
	}
	// The shared dir flags are checked before any agent request.
	sharedDirBackends, err := parseSharedDirBackendFlags(reincarnateSharedDirs, reincarnateAllowEmptySD)
	if err != nil {
		return err
	}
	if isSelf && len(sharedDirBackends) > 0 {
		return fmt.Errorf("an agent cannot change its own shared dir backend; ask a user or another agent to run scion reincarnate --shared-dir-backend")
	}
	PrintUsingHub(hubCtx.Endpoint)

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	agentSvc := hubCtx.Client.ProjectAgents(projectID)

	req := &hubclient.ReincarnateAgentRequest{
		Handoff:             handoff,
		DryRun:              reincarnateDryRun,
		TargetBroker:        reincarnateBroker,
		SharedDirBackends:   sharedDirBackends,
		AllowEmptySharedDir: reincarnateAllowEmptySD,
	}

	if reincarnateDryRun {
		statusf("Resolving reincarnation plan for '%s'...\n", agentName)
	} else if isSelf {
		statusf("Reincarnating '%s' (self-migration)...\n", agentName)
	} else {
		statusf("Reincarnating '%s'...\n", agentName)
	}

	resp, err := agentSvc.Reincarnate(ctx, agentName, req)
	if err != nil {
		if v := moveVerdictFromError(err); v != nil && !isJSONOutput() {
			printMoveVerdict(os.Stderr, v)
		}
		if apiclient.IsConflictError(err) && moveVerdictFromError(err) == nil {
			return wrapHubError(fmt.Errorf("a reincarnation is already pending for '%s': %w", agentName, err))
		}
		return wrapHubError(fmt.Errorf("failed to reincarnate agent via Hub: %w", err))
	}

	if err := checkHubSupportsMove(reincarnateBroker, resp); err != nil {
		return err
	}

	if isJSONOutput() {
		return outputJSON(resp)
	}

	if resp.MoveVerdict != nil {
		printMoveVerdict(os.Stdout, resp.MoveVerdict)
	}
	printReincarnationPlan(resp.Plan)

	if reincarnateDryRun {
		fmt.Println("\nDry run only; nothing was changed.")
		return nil
	}

	fmt.Printf("\nAgent '%s' is reincarnating: generation %d, state=%s.\n", agentName, resp.Generation, resp.State)
	if isSelf {
		fmt.Println("This container will be stopped shortly as part of the migration.")
	}
	return nil
}

// parseSharedDirBackendFlags parses repeated --shared-dir-backend NAME=nfs
// values into a map. nfs is the only supported backend, a name may appear
// once, and --allow-empty-shared-dir needs at least one value.
func parseSharedDirBackendFlags(values []string, allowEmpty bool) (map[string]string, error) {
	if len(values) == 0 {
		if allowEmpty {
			return nil, fmt.Errorf("--allow-empty-shared-dir needs --shared-dir-backend NAME=nfs")
		}
		return nil, nil
	}
	out := make(map[string]string, len(values))
	for _, v := range values {
		name, backend, ok := strings.Cut(v, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("--shared-dir-backend %q: want NAME=nfs", v)
		}
		if err := api.ValidateSharedDirs([]api.SharedDir{{Name: name}}); err != nil {
			return nil, fmt.Errorf("--shared-dir-backend %q: invalid shared dir name %q", v, name)
		}
		if backend != "nfs" {
			return nil, fmt.Errorf("--shared-dir-backend %q: only nfs is supported", v)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("--shared-dir-backend: shared dir %q given more than once", name)
		}
		out[name] = backend
	}
	return out, nil
}

// validateReincarnateBrokerFlags refuses --broker without --dry-run before
// any hub call: a hub that does not know --broker would ignore it and run a
// real in-place reincarnation.
func validateReincarnateBrokerFlags(broker string, dryRun bool) error {
	if broker != "" && !dryRun {
		return newUsageError("--broker requires --dry-run: moving an agent between brokers is not supported yet")
	}
	return nil
}

// checkHubSupportsMove fails a --broker request whose response has no
// target broker: the hub ignored --broker, so its plan is for the current
// broker, not the requested one.
func checkHubSupportsMove(broker string, resp *hubclient.ReincarnateAgentResponse) error {
	if broker != "" && (resp == nil || resp.TargetBrokerID == "") {
		return fmt.Errorf("this hub does not support --broker; upgrade the hub")
	}
	return nil
}

// moveVerdictFromError returns the move eligibility verdict carried in a
// refused reincarnate request's error details, or nil.
func moveVerdictFromError(err error) *hubclient.MoveVerdict {
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) || apiErr.Details == nil {
		return nil
	}
	raw, ok := apiErr.Details["verdict"]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var v hubclient.MoveVerdict
	if err := json.Unmarshal(b, &v); err != nil || len(v.Checks) == 0 {
		return nil
	}
	return &v
}

// printMoveVerdict renders a move eligibility verdict: the brokers, the
// resolved target profile, and each check's result.
func printMoveVerdict(w io.Writer, v *hubclient.MoveVerdict) {
	ref := func(b hubclient.MoveBrokerRef) string {
		if b.Name != "" {
			return fmt.Sprintf("%s (%s)", b.Name, b.ID)
		}
		return b.ID
	}
	_, _ = fmt.Fprintf(w, "Move eligibility: %s -> %s\n", ref(v.SourceBroker), ref(v.TargetBroker))
	if v.Profile != "" {
		_, _ = fmt.Fprintf(w, "  %-28s %s (%s)\n", "Target profile:", v.Profile, valueOrNone(v.RuntimeType))
	}
	for _, c := range v.Checks {
		line := fmt.Sprintf("  %-28s %s", c.Name+":", c.Result)
		if c.Message != "" {
			line += " - " + c.Message
		}
		_, _ = fmt.Fprintln(w, line)
	}
}

// printReincarnationPlan renders a ReincarnationPlan as plain text: old vs
// new for each scalar field, added/removed/changed env key names (never
// values, per design §3.2), the resolved branch, and any warnings from a
// legacy-agent CreateInputs reconstruction.
func printReincarnationPlan(plan hubclient.ReincarnationPlan) {
	printChange := func(label string, c hubclient.FieldChange) {
		if c.Old == c.New {
			fmt.Printf("  %-14s %s (unchanged)\n", label+":", valueOrNone(c.New))
			return
		}
		fmt.Printf("  %-14s %s -> %s\n", label+":", valueOrNone(c.Old), valueOrNone(c.New))
	}

	fmt.Println("Reincarnation plan:")
	printChange("Template", plan.Template)
	printChange("Image", plan.Image)
	printChange("Harness cfg", plan.HarnessCfg)
	printChange("Model", plan.Model)
	fmt.Printf("  %-14s %s\n", "Branch:", valueOrNone(plan.Branch))
	if len(plan.SharedDirBackends) > 0 {
		names := make([]string, 0, len(plan.SharedDirBackends))
		for name := range plan.SharedDirBackends {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			line := fmt.Sprintf("  Shared dir:    %s -> %s (record only; copy the data yourself)", name, plan.SharedDirBackends[name])
			if plan.AllowEmptySharedDir {
				line += ", empty nfs directory allowed"
			}
			fmt.Println(line)
		}
	}

	if len(plan.EnvKeys.Added) > 0 {
		fmt.Printf("  Env added:     %s\n", strings.Join(plan.EnvKeys.Added, ", "))
	}
	if len(plan.EnvKeys.Removed) > 0 {
		fmt.Printf("  Env removed:   %s\n", strings.Join(plan.EnvKeys.Removed, ", "))
	}
	if len(plan.EnvKeys.Changed) > 0 {
		fmt.Printf("  Env changed:   %s\n", strings.Join(plan.EnvKeys.Changed, ", "))
	}

	for _, w := range plan.Warnings {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", w)
	}
}

// isReincarnateHandoffTemplateInvocation reports whether cmd is the
// `reincarnate` command with `--handoff-template` set (design Amendment
// A26.2 O1). root.go's PersistentPreRunE calls this to exempt the flag from
// the agent-container-context gate and the requires-project check: the flag
// is a pure local print (see RunE above) and must work anywhere. Compares by
// identity (cmd == reincarnateCmd), not by name, so no other command named
// "reincarnate" in some other subtree could ever match (Amendment A26.3 N3).
func isReincarnateHandoffTemplateInvocation(cmd *cobra.Command) bool {
	return cmd == reincarnateCmd && reincarnateHandoffTemplate
}

func init() {
	reincarnateCmd.Flags().StringVar(&reincarnateHandoffFile, "handoff-file", "", "File whose content becomes the new generation's first task (required for self-migration)")
	reincarnateCmd.Flags().BoolVar(&reincarnateDryRun, "dry-run", false, "Print the resolved reincarnation plan without migrating anything")
	reincarnateCmd.Flags().StringVar(&reincarnateBroker, "broker", "", "Move the agent to this runtime broker (name or ID); both brokers must mount the same NFS export. Requires --dry-run for now")
	reincarnateCmd.Flags().BoolVar(&reincarnateHandoffTemplate, "handoff-template", false, "Print the handoff template and exit")
	reincarnateCmd.Flags().StringArrayVar(&reincarnateSharedDirs, "shared-dir-backend", nil, "Change a shared dir's recorded storage backend, as NAME=nfs (repeatable). Only the record changes; copy the data to the nfs directory first")
	reincarnateCmd.Flags().BoolVar(&reincarnateAllowEmptySD, "allow-empty-shared-dir", false, "With --shared-dir-backend, start even if the nfs directory is empty while the previous local directory is not")
	rootCmd.AddCommand(reincarnateCmd)
}
