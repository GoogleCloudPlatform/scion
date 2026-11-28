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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
	ackComment     string
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
	fs.StringVar(&v.ackComment, "ack-comment", "", "GitHub acknowledgement comment template posted after dispatch-agent succeeds (requires --github)")
	fs.StringVar(&v.githubToken, "github-token", "", "GitHub token precedence for dispatched agents (installation or user_preferred)")
	fs.StringVar(&v.agentRole, "agent-role", "", "Explicit role for dispatched agents (optional; defaults to project/hub ladder)")
	fs.StringArrayVar(&v.allow, "allow", nil, "Optional narrowing list of allowed untrusted event variable paths (or trailing .*) for template rendering (repeatable; derived by the Hub when omitted)")
}

func normalizeTriggerActionKind(raw string) (string, error) {
	switch strings.TrimSpace(raw) {
	case "", "message":
		return "message", nil
	case "dispatch-agent", "dispatch_agent":
		return "dispatch_agent", nil
	case store.TriggerActionGitHubComment:
		return store.TriggerActionGitHubComment, nil
	default:
		return "", newUsageError("unsupported --action %q (supported: message, dispatch-agent, github.comment)", triggerTerminalText(raw, false))
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

// deriveGitHubCommentUntrustedVars parses a github.comment body template using
// the github.comment.body slot and returns the deduplicated untrusted
// placeholder paths in first-appearance order (Stage 3 §7.1).
func deriveGitHubCommentUntrustedVars(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	tmpl, err := triggertemplate.Parse(raw, hub.TriggerTemplateSlotGitHubCommentBody)
	if err != nil {
		var pe *triggertemplate.ParseError
		if errors.As(err, &pe) {
			return nil, newUsageError("invalid template in slot %q at byte offset %d: %s",
				triggerTerminalText(pe.Slot, false), pe.Offset, triggerTerminalText(pe.Message, false))
		}
		return nil, newUsageError("invalid template in slot %q: %v", hub.TriggerTemplateSlotGitHubCommentBody, err)
	}
	untrusted := tmpl.Untrusted()
	if len(untrusted) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(untrusted))
	derived := make([]string, 0, len(untrusted))
	for _, p := range untrusted {
		if !seen[p] {
			seen[p] = true
			derived = append(derived, p)
		}
	}
	return derived, nil
}

func buildGitHubCommentActionSpec(body string, replyInThread bool, allow []string, allowExplicit bool) (hubclient.TriggerActionSpec, []string, error) {
	if strings.TrimSpace(body) == "" {
		return hubclient.TriggerActionSpec{}, nil, newUsageError("--comment is required for --action github.comment")
	}
	action := hubclient.TriggerActionSpec{
		Type:          store.TriggerActionGitHubComment,
		Body:          body,
		Target:        "event",
		ReplyInThread: replyInThread,
	}
	var (
		derived []string
		err     error
	)
	if allowExplicit || len(allow) > 0 {
		action.UntrustedVars = append([]string(nil), allow...)
	} else {
		derived, err = deriveGitHubCommentUntrustedVars(body)
		if err != nil {
			return hubclient.TriggerActionSpec{}, nil, err
		}
		if len(derived) > 0 {
			action.UntrustedVars = derived
		}
	}
	return action, derived, nil
}

func hasDispatchAgentFlagValues(v *dispatchAgentFlagValues) (string, bool) {
	if v == nil {
		return "", false
	}
	switch {
	case strings.TrimSpace(v.template) != "":
		return "--template", true
	case v.task != "":
		return "--task", true
	case strings.TrimSpace(v.taskFile) != "":
		return "--task-file", true
	case strings.TrimSpace(v.agentName) != "":
		return "--agent-name", true
	case strings.TrimSpace(v.onNameConflict) != "":
		return "--on-name-conflict", true
	case strings.TrimSpace(v.branch) != "":
		return "--branch", true
	case len(v.labels) > 0:
		return "--label", true
	case v.ephemeral:
		return "--ephemeral", true
	case v.reuseMessage != "":
		return "--reuse-message", true
	case v.ackComment != "":
		return "--ack-comment", true
	case strings.TrimSpace(v.githubToken) != "":
		return "--github-token", true
	case strings.TrimSpace(v.agentRole) != "":
		return "--agent-role", true
	default:
		return "", false
	}
}

func hasMessageFlagValues(agent, message string, interrupt, wake, plain bool) (string, bool) {
	switch {
	case strings.TrimSpace(agent) != "":
		return "--agent", true
	case message != "":
		return "--message", true
	case interrupt:
		return "--interrupt", true
	case wake:
		return "--wake", true
	case plain:
		return "--plain", true
	default:
		return "", false
	}
}

func validateUpdateCrossActionFlags(flags *pflag.FlagSet, targetType string) error {
	if flags == nil {
		return nil
	}
	var disallowed []string
	switch targetType {
	case "message":
		disallowed = []string{
			"template", "task", "task-file", "agent-name", "on-name-conflict",
			"branch", "label", "ephemeral", "reuse-message", "ack-comment",
			"github-token", "agent-role",
		}
	case "dispatch_agent":
		disallowed = []string{"agent", "message", "interrupt", "wake", "plain", "offline", "queue-ttl"}
	default:
		return nil
	}
	for _, name := range disallowed {
		if flags.Changed(name) {
			return newUsageError("--%s cannot be used with action type %q", name, targetType)
		}
	}
	return nil
}

func parseTriggerQueueTTL(raw string) (int, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, newUsageError("invalid --queue-ttl %q: %v", triggerTerminalText(raw, false), err)
	}
	if d%time.Second != 0 {
		return 0, newUsageError("invalid --queue-ttl %q: must be a whole number of seconds", triggerTerminalText(raw, false))
	}
	secs := int(d / time.Second)
	if secs < store.MinMessageQueueTTLSeconds || secs > store.MaxMessageQueueTTLSeconds {
		return 0, newUsageError("--queue-ttl must be between 60s and 48h (got %s)", d)
	}
	return secs, nil
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

	var ackComment *hubclient.DispatchAgentAckComment
	if v.ackComment != "" {
		if strings.TrimSpace(v.ackComment) == "" {
			return hubclient.TriggerActionSpec{}, nil, newUsageError("--ack-comment cannot be empty")
		}
		ackComment = &hubclient.DispatchAgentAckComment{Body: v.ackComment}
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
		AckComment:   ackComment,
		GithubToken:  ghToken,
		AgentRole:    strings.TrimSpace(v.agentRole),
	}

	if allowExplicit || len(v.allow) > 0 {
		action.UntrustedVars = append([]string(nil), v.allow...)
	}

	return action, nil, nil
}

func collectTriggerUntrustedVars(actions []hubclient.TriggerActionSpec) []string {
	var (
		out  []string
		seen map[string]bool
	)
	for _, act := range actions {
		for _, p := range act.UntrustedVars {
			if seen == nil {
				seen = make(map[string]bool)
			}
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
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

	case "plain_with_untrusted":
		cp.Message = "cannot combine --plain with untrusted event placeholders (remove --plain or avoid untrusted placeholders)"
		return &cp

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

	case "github_token_unsupported", "dispatch_agent_github_unsupported", "ack_comment_requires_github_source":
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
	triggerUpdateName              string
	triggerUpdateDescription       string
	triggerUpdateWebhookRef        string
	triggerUpdateGitHub            bool
	triggerUpdateMatchTypes        []string
	triggerUpdateWhereRules        []string
	triggerUpdateWhereFile         string
	triggerUpdateSubjectExpr       string
	triggerUpdateOnSubject         string
	triggerUpdateCoalesceWindow    string
	triggerUpdateMaxFiresPerMinute int
	triggerUpdateMaxConcurrent     int
	triggerUpdateRevision          int

	triggerUpdateAction      string
	triggerUpdateAgent       string
	triggerUpdateMessage     string
	triggerUpdateInterrupt   bool
	triggerUpdateWake        bool
	triggerUpdatePlain       bool
	triggerUpdateOffline     string
	triggerUpdateQueueTTL    string
	triggerUpdateDispatch    dispatchAgentFlagValues
	updateActionCollector    triggerActionGroupCollector
	triggerUpdateNotifyUser  bool
	triggerUpdateNotifyAgent string
	triggerUpdateNotifyOn    []string
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
	defer updateActionCollector.reset()
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
	if flags.Changed("max-per-minute") {
		if triggerUpdateMaxFiresPerMinute < 0 {
			return newUsageError("--max-per-minute must be non-negative")
		}
		v := triggerUpdateMaxFiresPerMinute
		req.MaxFiresPerMinute = &v
		anyChange = true
	}
	if flags.Changed("max-concurrent") {
		if triggerUpdateMaxConcurrent < 0 {
			return newUsageError("--max-concurrent must be non-negative")
		}
		v := triggerUpdateMaxConcurrent
		req.MaxConcurrentDispatches = &v
		anyChange = true
	}
	if flags.Changed("notify-user") || flags.Changed("notify-agent") || flags.Changed("notify-on") {
		patch := &hubclient.TriggerNotifyPatchSpec{}
		if flags.Changed("notify-user") {
			v := triggerUpdateNotifyUser
			patch.UserNotice = &v
		}
		if flags.Changed("notify-agent") {
			ref := strings.TrimSpace(triggerUpdateNotifyAgent)
			empty := ""
			if ref == "" {
				patch.AgentID = &empty
				patch.AgentName = &empty
			} else if uuid.Validate(ref) == nil {
				patch.AgentID = &ref
			} else {
				patch.AgentName = &ref
			}
		}
		if flags.Changed("notify-on") {
			outcomes, nErr := parseTriggerNotifyOutcomes(triggerUpdateNotifyOn)
			if nErr != nil {
				return nErr
			}
			patch.OnOutcomes = &outcomes
		}
		req.Notify = patch
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
	if flags.Changed("subject") || flags.Changed("on-subject") || flags.Changed("coalesce") {
		subj := &hubclient.TriggerSubjectPatchSpec{}
		if flags.Changed("subject") {
			expr := triggerUpdateSubjectExpr
			subj.Expr = &expr
		}
		if flags.Changed("on-subject") {
			mode := strings.TrimSpace(triggerUpdateOnSubject)
			subj.Mode = &mode
		}
		if flags.Changed("coalesce") {
			coalesce := strings.TrimSpace(triggerUpdateCoalesceWindow)
			subj.CoalesceWindow = &coalesce
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
		flags.Changed("offline") || flags.Changed("queue-ttl") ||
		flags.Changed("template") || flags.Changed("task") || flags.Changed("task-file") || flags.Changed("agent-name") ||
		flags.Changed("on-name-conflict") || flags.Changed("branch") || flags.Changed("label") || flags.Changed("ephemeral") ||
		flags.Changed("reuse-message") || flags.Changed("ack-comment") || flags.Changed("github-token") || flags.Changed("agent-role") || flags.Changed("allow")

	if actionFlagsChanged {
		existing, gErr := hubCtx.Client.Triggers(projectID).Get(ctx, fullID)
		if gErr != nil {
			return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to load trigger before update: %w", gErr))
		}
		if !flags.Changed("revision") && existing.AuthorizationRevision > 0 {
			rev := existing.AuthorizationRevision
			req.ExpectedRevision = &rev
		}
		if updateActionCollector.isMulti() {
			isGitHub := existing.Source.Kind == "github"
			if flags.Changed("github") && triggerUpdateGitHub {
				isGitHub = true
			} else if flags.Changed("webhook") {
				isGitHub = false
			}
			specs, _, gErr := buildActionSpecsFromGroups(updateActionCollector.groups, isGitHub)
			if gErr != nil {
				return gErr
			}
			req.Actions = &specs
			anyChange = true
		} else {
			existingActions := effectiveTriggerActions(existing)
			var existingFirst hubclient.TriggerActionSpec
			if len(existingActions) > 0 {
				existingFirst = existingActions[0]
			}
			if !flags.Changed("action") && len(existingActions) > 1 {
				return newUsageError("updating actions on a multi-action trigger requires passing --action for each action in the replacement list")
			}
			targetType := existingFirst.Type
			if flags.Changed("action") {
				normalized, nErr := normalizeTriggerActionKind(triggerUpdateAction)
				if nErr != nil {
					return nErr
				}
				targetType = normalized
			}

			if err := validateUpdateCrossActionFlags(flags, targetType); err != nil {
				return err
			}

			switch targetType {
			case "message":
				spec := hubclient.TriggerActionSpec{Type: "message"}
				if existingFirst.Type == "message" && len(existingActions) <= 1 {
					spec.AgentID = existingFirst.AgentID
					spec.AgentName = existingFirst.AgentName
					spec.Message = existingFirst.Message
					spec.Interrupt = existingFirst.Interrupt
					spec.Plain = existingFirst.Plain
					spec.Wake = existingFirst.Wake
					spec.Offline = existingFirst.Offline
					spec.QueueTTLSeconds = existingFirst.QueueTTLSeconds
					if existingFirst.UntrustedVars != nil {
						spec.UntrustedVars = append([]string(nil), existingFirst.UntrustedVars...)
					}
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
				if flags.Changed("offline") {
					offline := strings.TrimSpace(triggerUpdateOffline)
					if offline != "" && offline != "skip" && offline != "queue" {
						return newUsageError("invalid --offline %q (supported: skip, queue)", triggerTerminalText(offline, false))
					}
					spec.Offline = offline
					if offline != "queue" {
						spec.QueueTTLSeconds = 0
					}
				}
				if flags.Changed("queue-ttl") {
					rawTTL := strings.TrimSpace(triggerUpdateQueueTTL)
					if rawTTL == "" {
						spec.QueueTTLSeconds = 0
					} else {
						if spec.Offline != "queue" {
							return newUsageError("--queue-ttl requires --offline queue")
						}
						secs, tErr := parseTriggerQueueTTL(rawTTL)
						if tErr != nil {
							return tErr
						}
						spec.QueueTTLSeconds = secs
					}
				}
				if flags.Changed("allow") {
					spec.UntrustedVars = append([]string(nil), triggerUpdateDispatch.allow...)
				} else if flags.Changed("message") || flags.Changed("action") {
					// Omit untrustedVars when templates change without --allow so the Hub re-derives.
					spec.UntrustedVars = nil
				}
				req.Action = &spec

			case "dispatch_agent":
				if flags.Changed("task") && flags.Changed("task-file") {
					return newUsageError("--task and --task-file cannot be used together")
				}
				spec := hubclient.TriggerActionSpec{Type: "dispatch_agent"}
				if existingFirst.Type == "dispatch_agent" && len(existingActions) <= 1 {
					spec.AgentName = existingFirst.AgentName
					spec.NameConflict = existingFirst.NameConflict
					spec.Template = existingFirst.Template
					spec.Task = existingFirst.Task
					spec.Branch = existingFirst.Branch
					spec.Ephemeral = existingFirst.Ephemeral
					spec.GithubToken = existingFirst.GithubToken
					spec.AgentRole = existingFirst.AgentRole
					spec.ReuseMessage = existingFirst.ReuseMessage
					if existingFirst.AckComment != nil {
						cp := *existingFirst.AckComment
						spec.AckComment = &cp
					}
					spec.NoAuth = existingFirst.NoAuth
					if existingFirst.Labels != nil {
						spec.Labels = make(map[string]string, len(existingFirst.Labels))
						for k, v := range existingFirst.Labels {
							spec.Labels[k] = v
						}
					}
					if existingFirst.UntrustedVars != nil {
						spec.UntrustedVars = append([]string(nil), existingFirst.UntrustedVars...)
					}
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
				if flags.Changed("ack-comment") {
					if strings.TrimSpace(triggerUpdateDispatch.ackComment) == "" {
						spec.AckComment = nil
					} else {
						spec.AckComment = &hubclient.DispatchAgentAckComment{Body: triggerUpdateDispatch.ackComment}
					}
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
				} else if flags.Changed("action") || flags.Changed("task") || flags.Changed("task-file") ||
					flags.Changed("agent-name") || flags.Changed("branch") || flags.Changed("label") ||
					flags.Changed("reuse-message") || flags.Changed("ack-comment") {
					// Omit untrustedVars when template slots change without --allow so the Hub re-derives.
					spec.UntrustedVars = nil
				}
				req.Action = &spec

			default:
				return newUsageError("unsupported action type %q for update", triggerTerminalText(targetType, false))
			}
			anyChange = true
		}
	}

	if !anyChange {
		return newUsageError("at least one field flag is required to update a trigger")
	}

	updated, err := hubCtx.Client.Triggers(projectID).Update(ctx, fullID, req)
	if err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to update trigger: %w", formatTriggerAPIError(err)))
	}

	if isTriggerJSONOutput() {
		return outputTriggerJSON(updated)
	}

	fmt.Printf("Trigger updated: %s (revision %d)\n", triggerTerminalText(updated.ID, false), updated.AuthorizationRevision)
	updatedActions := effectiveTriggerActions(updated)
	printTriggerActionsSummary(updatedActions)
	printDerivedUntrustedVars(collectTriggerUntrustedVars(updatedActions))
	fmt.Printf("  Status:  %s\n", triggerTerminalText(triggerStatusLabel(updated.Status, updated.StatusReason), false))
	return nil
}

func printTriggerActionsSummary(actions []hubclient.TriggerActionSpec) {
	if len(actions) == 1 {
		printTriggerActionSummary(actions[0])
		return
	}
	if len(actions) == 0 {
		return
	}
	fmt.Printf("  Actions: %d\n", len(actions))
	for i, act := range actions {
		switch act.Type {
		case "dispatch_agent":
			fmt.Printf("    %d/%d [dispatch_agent] template=%s\n", i+1, len(actions), triggerTerminalText(act.Template, false))
		case "message":
			agentLabel := act.AgentName
			if agentLabel == "" {
				agentLabel = act.AgentID
			}
			fmt.Printf("    %d/%d [message] agent=%s\n", i+1, len(actions), triggerTerminalText(agentLabel, false))
		default:
			fmt.Printf("    %d/%d [%s]\n", i+1, len(actions), triggerTerminalText(act.Type, false))
		}
	}
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
		if action.AckComment != nil && action.AckComment.Body != "" {
			fmt.Printf("  Ack:     %s\n", triggerTerminalText(action.AckComment.Body, true))
		}
		return
	}
	if action.Type == store.TriggerActionGitHubComment {
		if action.ReplyInThread {
			fmt.Printf("  Action:  github.comment (reply-in-thread)\n")
		} else {
			fmt.Printf("  Action:  github.comment\n")
		}
		if action.Body != "" {
			fmt.Printf("  Comment: %s\n", triggerTerminalText(action.Body, true))
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
	f.StringVar(&triggerUpdateOnSubject, "on-subject", "", "Updated subject concurrency policy (new, skip, or reuse)")
	f.StringVar(&triggerUpdateCoalesceWindow, "coalesce", "", "Updated coalescing window duration (0 or 0s clears, max 10m, e.g. 30s)")
	f.IntVar(&triggerUpdateMaxFiresPerMinute, "max-per-minute", 0, "Updated maximum fires per minute (0 = unlimited; max 600)")
	f.IntVar(&triggerUpdateMaxConcurrent, "max-concurrent", 0, "Updated maximum concurrent live agents for dispatch-agent triggers (0 = default 5; max 50)")
	f.BoolVar(&triggerUpdateNotifyUser, "notify-user", true, "Notify the trigger creator on failure (use --notify-user=false to disable)")
	f.StringVar(&triggerUpdateNotifyAgent, "notify-agent", "", "Updated agent ID or name to message on failure (empty clears)")
	f.StringSliceVar(&triggerUpdateNotifyOn, "notify-on", nil, "Updated comma-separated outcomes to notify on (failed,refused,expired[,skipped])")
	f.IntVar(&triggerUpdateRevision, "revision", 0, "Expected authorization revision for optimistic concurrency")

	bindGroupedTriggerActionFlags(
		f,
		&updateActionCollector,
		&triggerUpdateAction, "",
		&triggerUpdateAgent, &triggerUpdateMessage,
		&triggerUpdateInterrupt, &triggerUpdateWake, &triggerUpdatePlain,
		&triggerUpdateOffline, &triggerUpdateQueueTTL,
		&triggerUpdateDispatch,
		nil, nil,
	)
}
