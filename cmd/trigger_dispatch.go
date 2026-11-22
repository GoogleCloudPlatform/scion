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
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/triggertemplate"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const maxTriggerTaskFileBytes = 64 * 1024

// dispatchAgentFlagValues groups the CLI flags shared by `trigger create`,
// `trigger update`, and `trigger preset apply` when authoring a dispatch_agent
// trigger action (Stage 2 §11.3).
type dispatchAgentFlagValues struct {
	template       string
	task           string
	taskFile       string
	agentName      string
	onNameConflict string
	branch         string
	labels         []string
	ephemeral      bool
	reuseMessage   string
	githubToken    string
	agentRole      string
	allow          []string
}

func bindDispatchAgentFlags(fs *pflag.FlagSet, v *dispatchAgentFlagValues) {
	fs.StringVar(&v.template, "template", "", "Agent template name for dispatch-agent actions (literal only, no placeholders)")
	fs.StringVar(&v.task, "task", "", "Task template for dispatch-agent actions (mutually exclusive with --task-file)")
	fs.StringVar(&v.taskFile, "task-file", "", "Path to a file containing the task template for dispatch-agent actions (mutually exclusive with --task)")
	fs.StringVar(&v.agentName, "agent-name", "", "Agent name template for dispatch-agent actions (optional; default <trigger>-<6 base32>)")
	fs.StringVar(&v.onNameConflict, "on-name-conflict", "", "Name collision policy for dispatch-agent actions (suffix or skip; default suffix)")
	fs.StringVar(&v.branch, "branch", "", "Git branch template for dispatch-agent actions (optional)")
	fs.StringArrayVar(&v.labels, "label", nil, "Agent label in key=value template form for dispatch-agent actions (repeatable)")
	fs.BoolVar(&v.ephemeral, "ephemeral", false, "Request an ephemeral dispatched agent (currently refused by the Hub until ephemeral agents ship)")
	fs.StringVar(&v.reuseMessage, "reuse-message", "", "Message template delivered when reusing a live dispatched agent (Stage 3)")
	fs.StringVar(&v.githubToken, "github-token", "", "GitHub token precedence for dispatched agents (installation or user_preferred)")
	fs.StringVar(&v.agentRole, "agent-role", "", "Explicit role for dispatched agents (optional; defaults to project/hub ladder)")
	fs.StringArrayVar(&v.allow, "allow", nil, "Explicitly opt in an untrusted event variable path (or trailing .*) for template rendering (repeatable; derived automatically when omitted)")
}

func normalizeTriggerActionKind(raw string) (string, error) {
	switch strings.TrimSpace(raw) {
	case "", "message":
		return "message", nil
	case "dispatch-agent", "dispatch_agent":
		return "dispatch_agent", nil
	default:
		return "", newUsageError("unsupported --action %q (supported: message, dispatch-agent)", triggerTerminalText(raw, false))
	}
}

func readTriggerTaskFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", newUsageError("failed to open --task-file %q: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	raw, err := io.ReadAll(io.LimitReader(f, maxTriggerTaskFileBytes+1))
	if err != nil {
		return "", newUsageError("failed to read --task-file %q: %v", path, err)
	}
	if len(raw) > maxTriggerTaskFileBytes {
		return "", newUsageError("--task-file %q exceeds %d byte limit", path, maxTriggerTaskFileBytes)
	}
	return string(raw), nil
}

func parseTriggerLabels(rawArgs []string) (map[string]string, error) {
	if len(rawArgs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(rawArgs))
	for _, raw := range rawArgs {
		idx := strings.IndexByte(raw, '=')
		if idx <= 0 {
			return nil, newUsageError("invalid --label %q: expected key=value", triggerTerminalText(raw, false))
		}
		key := strings.TrimSpace(raw[:idx])
		if key == "" {
			return nil, newUsageError("invalid --label %q: label key is required", triggerTerminalText(raw, false))
		}
		out[key] = raw[idx+1:]
	}
	return out, nil
}

// deriveDispatchUntrustedVars parses every template slot on a dispatch_agent
// action using pkg/triggertemplate and returns the deduplicated untrusted
// placeholder paths in first-appearance order (Stage 2 D-S2-5).
func deriveDispatchUntrustedVars(action *hubclient.TriggerActionSpec) ([]string, error) {
	var (
		derived []string
		seen    = make(map[string]bool)
	)
	collect := func(raw, slot string) error {
		if raw == "" {
			return nil
		}
		tmpl, err := triggertemplate.Parse(raw, slot)
		if err != nil {
			var pe *triggertemplate.ParseError
			if errors.As(err, &pe) {
				return newUsageError("invalid template in slot %q at byte offset %d: %s",
					triggerTerminalText(pe.Slot, false), pe.Offset, triggerTerminalText(pe.Message, false))
			}
			return newUsageError("invalid template in slot %q: %v", triggerTerminalText(slot, false), err)
		}
		for _, p := range tmpl.Untrusted() {
			if !seen[p] {
				seen[p] = true
				derived = append(derived, p)
			}
		}
		return nil
	}

	if err := collect(action.Task, triggertemplate.SlotTask); err != nil {
		return nil, err
	}
	if err := collect(action.AgentName, triggertemplate.SlotName); err != nil {
		return nil, err
	}
	if err := collect(action.Branch, triggertemplate.SlotBranch); err != nil {
		return nil, err
	}
	if len(action.Labels) > 0 {
		keys := make([]string, 0, len(action.Labels))
		for k := range action.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := collect(action.Labels[k], triggertemplate.SlotLabel); err != nil {
				return nil, err
			}
		}
	}
	if err := collect(action.ReuseMessage, triggertemplate.SlotMessage); err != nil {
		return nil, err
	}
	return derived, nil
}

func buildDispatchAgentActionSpec(v *dispatchAgentFlagValues, allowExplicit bool) (hubclient.TriggerActionSpec, []string, error) {
	taskText := v.task
	taskFile := strings.TrimSpace(v.taskFile)
	if taskText != "" && taskFile != "" {
		return hubclient.TriggerActionSpec{}, nil, newUsageError("--task and --task-file cannot be used together")
	}
	if taskFile != "" {
		content, err := readTriggerTaskFile(taskFile)
		if err != nil {
			return hubclient.TriggerActionSpec{}, nil, err
		}
		taskText = content
	}

	tmplRef := strings.TrimSpace(v.template)
	if tmplRef == "" {
		return hubclient.TriggerActionSpec{}, nil, newUsageError("--template is required for --action dispatch-agent")
	}

	nameConflict := strings.TrimSpace(v.onNameConflict)
	if nameConflict != "" && nameConflict != "suffix" && nameConflict != "skip" {
		return hubclient.TriggerActionSpec{}, nil, newUsageError("invalid --on-name-conflict %q (supported: suffix, skip)", triggerTerminalText(nameConflict, false))
	}

	ghToken := strings.TrimSpace(v.githubToken)
	if ghToken != "" && ghToken != "installation" && ghToken != "user_preferred" {
		return hubclient.TriggerActionSpec{}, nil, newUsageError("invalid --github-token %q (supported: installation, user_preferred)", triggerTerminalText(ghToken, false))
	}

	labelsMap, err := parseTriggerLabels(v.labels)
	if err != nil {
		return hubclient.TriggerActionSpec{}, nil, err
	}

	action := hubclient.TriggerActionSpec{
		Type:         "dispatch_agent",
		Template:     tmplRef,
		Task:         taskText,
		AgentName:    strings.TrimSpace(v.agentName),
		NameConflict: nameConflict,
		Branch:       strings.TrimSpace(v.branch),
		Labels:       labelsMap,
		Ephemeral:    v.ephemeral,
		ReuseMessage: v.reuseMessage,
		GithubToken:  ghToken,
		AgentRole:    strings.TrimSpace(v.agentRole),
	}

	var derived []string
	if allowExplicit || len(v.allow) > 0 {
		action.UntrustedVars = append([]string(nil), v.allow...)
	} else {
		derived, err = deriveDispatchUntrustedVars(&action)
		if err != nil {
			return hubclient.TriggerActionSpec{}, nil, err
		}
		if len(derived) > 0 {
			action.UntrustedVars = derived
		}
	}

	return action, derived, nil
}

func printDerivedUntrustedVars(paths []string) {
	if len(paths) == 0 || isTriggerJSONOutput() {
		return
	}
	safe := make([]string, len(paths))
	for i, p := range paths {
		safe[i] = triggerTerminalText(p, false)
	}
	fmt.Printf("Event values that will be inserted as data: %s\n", strings.Join(safe, ", "))
}

// formatTriggerAPIError maps structured Hub trigger authoring errors (rules,
// templates, untrusted variables, labels, replay protection, ephemeral, and
// token modes) to readable CLI messages with slot and offset details where
// present (Stage 2 §11.3).
func formatTriggerAPIError(err error) error {
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	if apiErr.Code == "invalid_rule" {
		return formatTriggerRuleError(err)
	}

	cp := *apiErr
	switch apiErr.Code {
	case "invalid_template":
		slot := triggerTerminalText(triggerDetailString(apiErr.Details, "slot"), false)
		msg := triggerTerminalText(triggerDetailString(apiErr.Details, "message"), false)
		if msg == "" {
			msg = triggerTerminalText(apiErr.Message, false)
		}
		if offset, hasOffset := triggerDetailInt(apiErr.Details, "offset"); hasOffset {
			if slot != "" {
				cp.Message = fmt.Sprintf("invalid template in slot %q at byte offset %d: %s", slot, offset, msg)
			} else {
				cp.Message = fmt.Sprintf("invalid template at byte offset %d: %s", offset, msg)
			}
			return &cp
		}
		if slot != "" {
			cp.Message = fmt.Sprintf("invalid template in slot %q: %s", slot, msg)
			return &cp
		}

	case "untrusted_var_not_allowed":
		slot := triggerTerminalText(triggerDetailString(apiErr.Details, "slot"), false)
		path := triggerTerminalText(triggerDetailString(apiErr.Details, "path"), false)
		if path != "" && slot != "" {
			cp.Message = fmt.Sprintf("untrusted variable %q is not allowed in slot %q (pass --allow %s to opt in)", path, slot, path)
			return &cp
		}
		if path != "" {
			cp.Message = fmt.Sprintf("untrusted variable %q is not allowed (pass --allow %s to opt in)", path, path)
			return &cp
		}

	case "invalid_untrusted_var":
		if path := triggerTerminalText(triggerDetailString(apiErr.Details, "path"), false); path != "" {
			cp.Message = fmt.Sprintf("invalid --allow path %q: must be an untrusted event variable path or trailing \".*\" prefix", path)
			return &cp
		}

	case "reserved_label_key":
		if key := triggerTerminalText(triggerDetailString(apiErr.Details, "key"), false); key != "" {
			cp.Message = fmt.Sprintf("label key %q is reserved for hub trigger provenance (cannot be set via --label)", key)
			return &cp
		}

	case "source_not_replay_protected":
		cp.Message = "source does not support replay protection (required for dispatch-agent triggers; use a scion_v1, stripe_v1, or hub_token webhook endpoint)"
		return &cp

	case "ephemeral_unsupported":
		cp.Message = "ephemeral dispatched agents (--ephemeral) are not yet supported by the Hub"
		return &cp

	case "github_token_required":
		if apiErr.Message != "" {
			cp.Message = fmt.Sprintf("GitHub token configuration required: %s (pass --github-token installation|user_preferred)", triggerTerminalText(apiErr.Message, false))
		} else {
			cp.Message = "GitHub token configuration required (pass --github-token installation|user_preferred)"
		}
		return &cp

	case "github_token_unsupported", "dispatch_agent_github_unsupported":
		if apiErr.Message != "" {
			cp.Message = triggerTerminalText(apiErr.Message, false)
			return &cp
		}

	case "invalid_value":
		if field := triggerTerminalText(triggerDetailString(apiErr.Details, "field"), false); field != "" {
			cp.Message = fmt.Sprintf("invalid value for %s: %s", field, triggerTerminalText(apiErr.Message, false))
			return &cp
		}

	case "revision_conflict":
		cp.Message = "trigger was modified concurrently (revision conflict); refresh and retry"
		return &cp
	}

	return err
}

// --- scion trigger update ---

var (
	triggerUpdateName        string
	triggerUpdateDescription string
	triggerUpdateWebhookRef  string
	triggerUpdateGitHub      bool
	triggerUpdateMatchTypes  []string
	triggerUpdateWhereRules  []string
	triggerUpdateWhereFile   string
	triggerUpdateSubjectExpr string
	triggerUpdateOnSubject   string
	triggerUpdateRevision    int

	triggerUpdateAction    string
	triggerUpdateAgent     string
	triggerUpdateMessage   string
	triggerUpdateInterrupt bool
	triggerUpdateWake      bool
	triggerUpdatePlain     bool
	triggerUpdateDispatch  dispatchAgentFlagValues
)

var triggerUpdateCmd = &cobra.Command{
	Use:   "update <id|name>",
	Short: "Update an event trigger",
	Long: `Update an existing event trigger definition (PATCH).

Only flags explicitly provided are updated. When updating action fields without
passing --action, the trigger's existing action type and unchanged fields are
preserved; when passing --action to switch action kinds, provide the required
flags for the new action. When updating a dispatch-agent action without --allow,
the untrusted variable list is re-derived from the resulting templates
(narrowing any previously stored wildcard).

Note: agent callers cannot PATCH trigger definitions.`,
	Args: cobra.ExactArgs(1),
	RunE: runTriggerUpdate,
}

func runTriggerUpdate(cmd *cobra.Command, args []string) error {
	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	fullID, err := resolveTriggerIDOrName(ctx, hubCtx, projectID, args[0])
	if err != nil {
		return err
	}

	flags := cmd.Flags()
	req := &hubclient.UpdateTriggerRequest{}
	anyChange := false

	if flags.Changed("name") {
		name := strings.TrimSpace(triggerUpdateName)
		if name == "" {
			return newUsageError("--name cannot be empty")
		}
		req.Name = &name
		anyChange = true
	}
	if flags.Changed("description") {
		desc := triggerUpdateDescription
		req.Description = &desc
		anyChange = true
	}
	if flags.Changed("webhook") || flags.Changed("github") {
		webhookRef := strings.TrimSpace(triggerUpdateWebhookRef)
		if triggerUpdateGitHub && webhookRef != "" {
			return newUsageError("--github and --webhook are mutually exclusive")
		}
		if triggerUpdateGitHub {
			req.Source = &hubclient.TriggerSourceSpec{Kind: "github", ID: ""}
		} else if webhookRef != "" {
			req.Source = &hubclient.TriggerSourceSpec{Kind: "webhook", ID: webhookRef}
		} else {
			return newUsageError("either --webhook or --github must be non-empty when updating source")
		}
		anyChange = true
	}
	if flags.Changed("where") && flags.Changed("where-file") {
		return newUsageError("--where and --where-file cannot be used together")
	}
	if flags.Changed("type") || flags.Changed("where") || flags.Changed("where-file") {
		patch := &hubclient.TriggerMatchPatchSpec{}
		if flags.Changed("type") {
			types := append([]string(nil), triggerUpdateMatchTypes...)
			patch.Types = &types
		}
		if flags.Changed("where-file") {
			parsed, rErr := readTriggerWhereFile(strings.TrimSpace(triggerUpdateWhereFile))
			if rErr != nil {
				return rErr
			}
			patch.Rules = &parsed
		} else if flags.Changed("where") {
			rules := make([]hubclient.TriggerMatchRuleSpec, len(triggerUpdateWhereRules))
			for i, raw := range triggerUpdateWhereRules {
				rules[i] = parseTriggerWhereArg(raw)
			}
			patch.Rules = &rules
		}
		req.Match = patch
		anyChange = true
	}
	if flags.Changed("subject") || flags.Changed("on-subject") {
		subj := &hubclient.TriggerSubjectPatchSpec{}
		if flags.Changed("subject") {
			expr := triggerUpdateSubjectExpr
			subj.Expr = &expr
		}
		if flags.Changed("on-subject") {
			mode := strings.TrimSpace(triggerUpdateOnSubject)
			subj.Mode = &mode
		}
		req.Subject = subj
		anyChange = true
	}
	if flags.Changed("revision") {
		rev := triggerUpdateRevision
		req.ExpectedRevision = &rev
	}

	actionFlagsChanged := flags.Changed("action") ||
		flags.Changed("agent") || flags.Changed("message") || flags.Changed("interrupt") || flags.Changed("wake") || flags.Changed("plain") ||
		flags.Changed("template") || flags.Changed("task") || flags.Changed("task-file") || flags.Changed("agent-name") ||
		flags.Changed("on-name-conflict") || flags.Changed("branch") || flags.Changed("label") || flags.Changed("ephemeral") ||
		flags.Changed("reuse-message") || flags.Changed("github-token") || flags.Changed("agent-role") || flags.Changed("allow")

	var derivedUntrusted []string
	if actionFlagsChanged {
		existing, gErr := hubCtx.Client.Triggers(projectID).Get(ctx, fullID)
		if gErr != nil {
			return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to load trigger before update: %w", gErr))
		}
		if !flags.Changed("revision") && existing.AuthorizationRevision > 0 {
			rev := existing.AuthorizationRevision
			req.ExpectedRevision = &rev
		}
		targetType := existing.Action.Type
		if flags.Changed("action") {
			normalized, nErr := normalizeTriggerActionKind(triggerUpdateAction)
			if nErr != nil {
				return nErr
			}
			targetType = normalized
		}

		switch targetType {
		case "message":
			var spec hubclient.TriggerActionSpec
			if existing.Action.Type == "message" {
				spec = existing.Action
			} else {
				spec = hubclient.TriggerActionSpec{Type: "message"}
			}
			if flags.Changed("agent") {
				agentRef := strings.TrimSpace(triggerUpdateAgent)
				if agentRef == "" {
					return newUsageError("--agent cannot be empty")
				}
				if uuid.Validate(agentRef) == nil {
					spec.AgentID = agentRef
					spec.AgentName = ""
				} else {
					spec.AgentName = agentRef
					spec.AgentID = ""
				}
			}
			if flags.Changed("message") {
				spec.Message = triggerUpdateMessage
			}
			if flags.Changed("interrupt") {
				spec.Interrupt = triggerUpdateInterrupt
			}
			if flags.Changed("wake") {
				spec.Wake = triggerUpdateWake
			}
			if flags.Changed("plain") {
				spec.Plain = triggerUpdatePlain
			}
			req.Action = &spec

		case "dispatch_agent":
			if flags.Changed("task") && flags.Changed("task-file") {
				return newUsageError("--task and --task-file cannot be used together")
			}
			var spec hubclient.TriggerActionSpec
			if existing.Action.Type == "dispatch_agent" {
				spec = existing.Action
				if existing.Action.Labels != nil {
					spec.Labels = make(map[string]string, len(existing.Action.Labels))
					for k, v := range existing.Action.Labels {
						spec.Labels[k] = v
					}
				}
			} else {
				spec = hubclient.TriggerActionSpec{Type: "dispatch_agent"}
			}
			if flags.Changed("template") {
				spec.Template = strings.TrimSpace(triggerUpdateDispatch.template)
			}
			if flags.Changed("task") {
				spec.Task = triggerUpdateDispatch.task
			}
			if flags.Changed("task-file") {
				content, fErr := readTriggerTaskFile(strings.TrimSpace(triggerUpdateDispatch.taskFile))
				if fErr != nil {
					return fErr
				}
				spec.Task = content
			}
			if flags.Changed("agent-name") {
				spec.AgentName = strings.TrimSpace(triggerUpdateDispatch.agentName)
			}
			if flags.Changed("on-name-conflict") {
				nc := strings.TrimSpace(triggerUpdateDispatch.onNameConflict)
				if nc != "" && nc != "suffix" && nc != "skip" {
					return newUsageError("invalid --on-name-conflict %q (supported: suffix, skip)", triggerTerminalText(nc, false))
				}
				spec.NameConflict = nc
			}
			if flags.Changed("branch") {
				spec.Branch = strings.TrimSpace(triggerUpdateDispatch.branch)
			}
			if flags.Changed("label") {
				labelsMap, lErr := parseTriggerLabels(triggerUpdateDispatch.labels)
				if lErr != nil {
					return lErr
				}
				spec.Labels = labelsMap
			}
			if flags.Changed("ephemeral") {
				spec.Ephemeral = triggerUpdateDispatch.ephemeral
			}
			if flags.Changed("reuse-message") {
				spec.ReuseMessage = triggerUpdateDispatch.reuseMessage
			}
			if flags.Changed("github-token") {
				gt := strings.TrimSpace(triggerUpdateDispatch.githubToken)
				if gt != "" && gt != "installation" && gt != "user_preferred" {
					return newUsageError("invalid --github-token %q (supported: installation, user_preferred)", triggerTerminalText(gt, false))
				}
				spec.GithubToken = gt
			}
			if flags.Changed("agent-role") {
				spec.AgentRole = strings.TrimSpace(triggerUpdateDispatch.agentRole)
			}
			if flags.Changed("allow") {
				spec.UntrustedVars = append([]string(nil), triggerUpdateDispatch.allow...)
			} else {
				derived, dErr := deriveDispatchUntrustedVars(&spec)
				if dErr != nil {
					return dErr
				}
				spec.UntrustedVars = derived
				derivedUntrusted = derived
			}
			req.Action = &spec

		default:
			return newUsageError("unsupported action type %q for update", triggerTerminalText(targetType, false))
		}
		anyChange = true
	}

	if !anyChange {
		return newUsageError("at least one field flag is required to update a trigger")
	}

	printDerivedUntrustedVars(derivedUntrusted)

	updated, err := hubCtx.Client.Triggers(projectID).Update(ctx, fullID, req)
	if err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to update trigger: %w", formatTriggerAPIError(err)))
	}

	if isTriggerJSONOutput() {
		return outputTriggerJSON(updated)
	}

	fmt.Printf("Trigger updated: %s (revision %d)\n", triggerTerminalText(updated.ID, false), updated.AuthorizationRevision)
	printTriggerActionSummary(updated.Action)
	fmt.Printf("  Status:  %s\n", triggerTerminalText(triggerStatusLabel(updated.Status, updated.StatusReason), false))
	return nil
}

func printTriggerActionSummary(action hubclient.TriggerActionSpec) {
	if action.Type == "dispatch_agent" {
		fmt.Printf("  Action:  dispatch_agent (template: %s)\n", triggerTerminalText(action.Template, false))
		if action.AgentName != "" {
			fmt.Printf("  Agent:   %s\n", triggerTerminalText(action.AgentName, false))
		}
		if action.Branch != "" {
			fmt.Printf("  Branch:  %s\n", triggerTerminalText(action.Branch, false))
		}
		if action.GithubToken != "" {
			fmt.Printf("  GH Tok:  %s\n", triggerTerminalText(action.GithubToken, false))
		}
		if action.AgentRole != "" {
			fmt.Printf("  Role:    %s\n", triggerTerminalText(action.AgentRole, false))
		}
		return
	}
	agentLabel := action.AgentName
	if agentLabel == "" {
		agentLabel = action.AgentID
	}
	fmt.Printf("  Agent:   %s\n", triggerTerminalText(agentLabel, false))
}

func init() {
	triggerCmd.AddCommand(triggerUpdateCmd)

	f := triggerUpdateCmd.Flags()
	f.StringVar(&triggerUpdateName, "name", "", "Updated trigger name")
	f.StringVar(&triggerUpdateDescription, "description", "", "Updated trigger description")
	f.StringVar(&triggerUpdateWebhookRef, "webhook", "", "Updated webhook endpoint ID or name (mutually exclusive with --github)")
	f.BoolVar(&triggerUpdateGitHub, "github", false, "Switch trigger source to the project's GitHub App source (mutually exclusive with --webhook)")
	f.StringSliceVar(&triggerUpdateMatchTypes, "type", nil, "Updated event type patterns (repeatable)")
	f.StringArrayVar(&triggerUpdateWhereRules, "where", nil, "Updated ordered match rules [<dialect>:]<expr> (repeatable)")
	f.StringVar(&triggerUpdateWhereFile, "where-file", "", "Path to JSON file containing updated ordered {dialect, expr} match rules")
	f.StringVar(&triggerUpdateSubjectExpr, "subject", "", "Updated CEL subject expression")
	f.StringVar(&triggerUpdateOnSubject, "on-subject", "", "Updated subject concurrency policy (new or skip)")
	f.IntVar(&triggerUpdateRevision, "revision", 0, "Expected authorization revision for optimistic concurrency")

	f.StringVar(&triggerUpdateAction, "action", "", "Action kind (message or dispatch-agent)")
	f.StringVar(&triggerUpdateAgent, "agent", "", "Target agent name or ID (message action)")
	f.StringVar(&triggerUpdateMessage, "message", "", "Message text (message action)")
	f.BoolVar(&triggerUpdateInterrupt, "interrupt", false, "Interrupt target agent (message action)")
	f.BoolVar(&triggerUpdateWake, "wake", false, "Resume target agent if suspended (message action)")
	f.BoolVar(&triggerUpdatePlain, "plain", false, "Deliver message text without sender prefix (message action)")
	bindDispatchAgentFlags(f, &triggerUpdateDispatch)
}
