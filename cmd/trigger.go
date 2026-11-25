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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

const maxTriggerWhereFileBytes = 64 * 1024

var (
	triggerName              string
	triggerDescription       string
	triggerWebhookRef        string
	triggerGitHubSource      bool
	triggerMatchTypes        []string
	triggerWhereRules        []string
	triggerWhereFile         string
	triggerAction            string
	triggerAgent             string
	triggerMessage           string
	triggerInterrupt         bool
	triggerWake              bool
	triggerPlain             bool
	triggerOffline           string
	triggerQueueTTL          string
	triggerDispatch          dispatchAgentFlagValues
	triggerComment           string
	triggerReplyInThread     bool
	createActionCollector    triggerActionGroupCollector
	triggerSubjectExpr       string
	triggerOnSubject         string
	triggerCoalesceWindow    string
	triggerMaxFiresPerMinute int
	triggerMaxConcurrent     int

	triggerListSource  string
	triggerListWebhook string
	triggerListStatus  string

	triggerHistoryLimit   int
	triggerHistoryCursor  string
	triggerHistoryVerbose bool

	triggerOutputJSON bool
)

func isTriggerJSONOutput() bool {
	return isJSONOutput() || triggerOutputJSON
}

func outputTriggerJSON(v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, escapeTriggerJSONBytes(raw))
	return err
}

func outputTriggerActionResult(r ActionResult) error {
	if isTriggerJSONOutput() {
		return outputTriggerJSON(r)
	}
	return outputActionResult(r)
}

// triggerCmd is the top-level command group for trigger and webhook endpoint management.
var triggerCmd = &cobra.Command{
	Use:   "trigger",
	Short: "Manage event triggers and webhook endpoints",
	Long:  `Create, inspect, list, pause, resume, and delete event triggers, received trigger events, and webhook endpoints for the current project.`,
}

var triggerCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create an event trigger",
	Long: `Create an event trigger attached to a webhook endpoint (--webhook) or the project's GitHub App source (--github). Requires --name, either --webhook or --github, --agent, and --message (--github also requires at least one --type).

Ordered Match Rules & CEL Guidance:
  --where [<dialect>:]<expr> is repeatable (1-8 elements, <= 4096 bytes each;
  total encoded rule list <= 16 KiB). Rules are logically AND'd and evaluated
  strictly in order of appearance, short-circuiting on the first false result.
  Put cheap CEL guards first so inexpensive deterministic checks filter events
  before heavier predicates run. Alternatively, pass --where-file <path> with a
  JSON array of {"dialect": "...", "expr": "..."} objects.

  A [<dialect>:] prefix is recognized only for registered dialect names (cel,
  system_one); any other prefix is treated as part of the CEL expression text.
  --subject (string, <= 1024 bytes) is a scalar CEL expression deriving the
  concurrency subject.

  CEL expressions are compiled at authoring time against event.{id,type,
  source.kind,source.id,subject,occurred_at,received_at,attributes,payload} and
  trigger.{id,name,project_id}. Guard optional JSON payload fields or optional
  attributes with has(...) (for example has(event.payload.env) or
  has(event.attributes.subject)) so absent keys evaluate cleanly instead of
  skipping the rule as missing_input.

  matches() accepts literal RE2 patterns only (dynamic patterns fail compilation
  at authoring time). Run matches() on event.attributes.<k> (webhook keys:
  endpoint_name, event, delivery, subject, signature_mode, content_type,
  payload_bytes, remote_ip) or one event.payload.<field>, not on the whole
  payload: against a full 64 KiB string the regex work bound passes up to ~14
  case-insensitive keywords (16+ refuses) and an anchored ^[a-z0-9-]{1,63}$
  refuses on 64 KiB input (passes on 16 KiB). Runtime evaluation refusals record
  skipped ("rule evaluation failed") in trigger history.

Examples:
  # Deliver a message to agent "lead" whenever webhook endpoint "deploys" receives an event
  scion trigger create --name deploy-finished --webhook deploys --agent lead --message "Check incoming deploy event" --wake

  # Match specific webhook event types
  scion trigger create --name prod-deploy --webhook deploys --type webhook.deploy.finished --agent lead --message "Production deploy finished"

  # Filter with ordered CEL predicates (put cheap CEL guards first; short-circuits on false)
  # and derive a concurrency subject from a guarded field
  scion trigger create --name prod-service-deploy --webhook deploys \
    --where 'has(event.payload) && has(event.payload.env) && event.payload.env == "prod"' \
    --where 'has(event.attributes.subject) && event.attributes.subject.matches("^release/")' \
    --subject 'has(event.payload) && has(event.payload.service) ? string(event.payload.service) : string(event.subject)' \
    --agent lead --message "Inspect production release event"

  # Deliver a message to agent "lead" on GitHub issue/PR comments
  scion trigger create --name gh-ping --github --type github.issue_comment.created --agent lead --message "Check incoming GitHub comment" --wake`,
	RunE: runTriggerCreate,
}

var triggerListCmd = &cobra.Command{
	Use:   "list",
	Short: "List event triggers",
	Long:  `List event triggers for the current project. When called in agent mode, only triggers held by the calling agent are shown.`,
	RunE:  runTriggerList,
}

var triggerDeleteCmd = &cobra.Command{
	Use:   "delete <id|name>",
	Short: "Delete an event trigger",
	Args:  cobra.ExactArgs(1),
	RunE:  runTriggerDelete,
}

var triggerPauseCmd = &cobra.Command{
	Use:   "pause <id|name>",
	Short: "Pause an active event trigger",
	Args:  cobra.ExactArgs(1),
	RunE:  runTriggerPause,
}

var triggerResumeCmd = &cobra.Command{
	Use:   "resume <id|name>",
	Short: "Resume a paused event trigger",
	Args:  cobra.ExactArgs(1),
	RunE:  runTriggerResume,
}

var triggerHistoryCmd = &cobra.Command{
	Use:   "history <id|name>",
	Short: "Show fire history for an event trigger",
	Long:  `List fire history records for an event trigger, ordered newest first.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runTriggerHistory,
}

var triggerHubContextFn = triggerHubContext

func triggerHubContext() (*HubContext, string, context.Context, context.CancelFunc, error) {
	hubCtx, err := requireHub(projectPath)
	if err != nil {
		return nil, "", nil, nil, err
	}
	if !isTriggerJSONOutput() {
		PrintUsingHub(hubCtx.Endpoint)
	}

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return nil, "", nil, nil, wrapHubError(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	return hubCtx, projectID, ctx, cancel, nil
}

func isCallerScopedTriggerView(hubCtx *HubContext, scope string) bool {
	if scope == hubclient.TriggerListScopeCaller {
		return true
	}
	return hubCtx != nil && hubCtx.CredentialKind == hubsync.CredentialKindAgentToken
}

func triggerDetailInt(details map[string]any, key string) (int, bool) {
	v, ok := details[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}

func triggerDetailString(details map[string]any, key string) string {
	v, _ := details[key].(string)
	return v
}

func formatTriggerRuleError(err error) error {
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "invalid_rule" || len(apiErr.Details) == 0 {
		return err
	}
	slot := triggerDetailString(apiErr.Details, "field")
	if slot == "" {
		slot = "trigger"
	}
	if rawElems, ok := apiErr.Details["elements"].([]any); ok && len(rawElems) > 0 {
		var parts []string
		for _, raw := range rawElems {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			// details.elements[].element is 1-based (1..N), matching `trigger get`
			// numbering (1/N..N/N) and triggerrule.MatchResult.Through.
			elemNum, _ := triggerDetailInt(m, "element")
			if elemNum <= 0 {
				elemNum = 1
			}
			if line, hasLine := triggerDetailInt(m, "line"); hasLine {
				col, _ := triggerDetailInt(m, "column")
				cat := triggerDetailString(m, "category")
				msg := triggerDetailString(m, "message")
				if cat != "" {
					parts = append(parts, fmt.Sprintf("invalid %s rule [element %d] at line %d, column %d (%s): %s", slot, elemNum, line, col, cat, msg))
				} else {
					parts = append(parts, fmt.Sprintf("invalid %s rule [element %d] at line %d, column %d: %s", slot, elemNum, line, col, msg))
				}
				continue
			}
			if dialect := triggerDetailString(m, "dialect"); dialect != "" {
				reason := triggerDetailString(m, "reason")
				if reason != "" {
					parts = append(parts, fmt.Sprintf("unsupported %s dialect %q [element %d]: %s", slot, dialect, elemNum, reason))
				} else {
					parts = append(parts, fmt.Sprintf("unsupported %s dialect %q [element %d]", slot, dialect, elemNum))
				}
			}
		}
		if len(parts) > 0 {
			joined := strings.Join(parts, "; ")
			if apiErr.Message != joined {
				cp := *apiErr
				cp.Message = joined
				return &cp
			}
		}
		return err
	}
	if line, hasLine := triggerDetailInt(apiErr.Details, "line"); hasLine {
		col, _ := triggerDetailInt(apiErr.Details, "column")
		posNeedle := fmt.Sprintf("line %d, column %d", line, col)
		shortNeedle := fmt.Sprintf("%d:%d", line, col)
		if !strings.Contains(apiErr.Message, posNeedle) && !strings.Contains(apiErr.Message, shortNeedle) {
			cat := triggerDetailString(apiErr.Details, "category")
			detailMsg := triggerDetailString(apiErr.Details, "message")
			if detailMsg == "" {
				detailMsg = apiErr.Message
			}
			cp := *apiErr
			if cat != "" {
				cp.Message = fmt.Sprintf("invalid %s rule at line %d, column %d (%s): %s", slot, line, col, cat, detailMsg)
			} else {
				cp.Message = fmt.Sprintf("invalid %s rule at line %d, column %d: %s", slot, line, col, detailMsg)
			}
			return &cp
		}
		return err
	}
	if reason := triggerDetailString(apiErr.Details, "reason"); reason != "" && !strings.Contains(apiErr.Message, reason) {
		cp := *apiErr
		cp.Message = apiErr.Message + " (reason: " + reason + ")"
		return &cp
	}
	return err
}

const triggerDialectSystemOne = "system_one"

func isRegisteredTriggerDialect(name string) bool {
	return name == ruleengine.DialectCEL || name == triggerDialectSystemOne
}

// parseTriggerWhereArg parses a --where value in [<dialect>:]<expr> form.
// The prefix is recognized only when <dialect> is a registered dialect name
// ("cel" or "system_one") followed by ':'; otherwise the whole value is treated
// as CEL expression text (mixed-rule-arrays §2.3).
func parseTriggerWhereArg(raw string) hubclient.TriggerMatchRuleSpec {
	if idx := strings.IndexByte(raw, ':'); idx > 0 {
		prefix := raw[:idx]
		if isRegisteredTriggerDialect(prefix) {
			return hubclient.TriggerMatchRuleSpec{
				Dialect: prefix,
				Expr:    raw[idx+1:],
			}
		}
	}
	return hubclient.TriggerMatchRuleSpec{
		Dialect: ruleengine.DialectCEL,
		Expr:    raw,
	}
}

func readTriggerWhereFile(path string) ([]hubclient.TriggerMatchRuleSpec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, newUsageError("failed to open --where-file %q: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	raw, err := io.ReadAll(io.LimitReader(f, maxTriggerWhereFileBytes+1))
	if err != nil {
		return nil, newUsageError("failed to read --where-file %q: %v", path, err)
	}
	if len(raw) > maxTriggerWhereFileBytes {
		return nil, newUsageError("--where-file %q exceeds %d byte limit", path, maxTriggerWhereFileBytes)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, newUsageError("invalid --where-file %q: expected JSON array of {dialect, expr} rule objects", path)
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var rules []hubclient.TriggerMatchRuleSpec
	if err := dec.Decode(&rules); err != nil {
		return nil, newUsageError("invalid --where-file %q: %v", path, err)
	}
	if dec.More() {
		return nil, newUsageError("invalid --where-file %q: unexpected trailing JSON data", path)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, newUsageError("invalid --where-file %q: unexpected trailing JSON data", path)
	}
	for i := range rules {
		if strings.TrimSpace(rules[i].Dialect) == "" {
			rules[i].Dialect = ruleengine.DialectCEL
		}
	}
	if rules == nil {
		rules = []hubclient.TriggerMatchRuleSpec{}
	}
	return rules, nil
}

func wrapTriggerHubError(hubCtx *HubContext, err error) error {
	if isCallerScopedTriggerView(hubCtx, "") {
		switch {
		case apiclient.IsConflictError(err) && !strings.Contains(err.Error(), "scion trigger"):
			return wrapHubError(fmt.Errorf("%w (if this name does not appear in 'scion trigger list' or 'scion trigger webhook list', it may be held by another principal and hidden from this agent)", err))
		case apiclient.IsForbiddenError(err) && strings.Contains(err.Error(), "Insufficient permissions"):
			return wrapHubError(fmt.Errorf("%w (agent callers can only access triggers and webhook endpoints they own)", err))
		}
	}
	return wrapHubError(err)
}

const triggerCLIMaxPageLimit = 200

// listAllTriggers drains every cursor page matching baseOpts and returns a
// single aggregated response with NextCursor cleared.
func listAllTriggers(ctx context.Context, svc hubclient.TriggerService, baseOpts *hubclient.ListTriggersOptions) (*hubclient.ListTriggersResponse, error) {
	var opts hubclient.ListTriggersOptions
	if baseOpts != nil {
		opts = *baseOpts
	}
	if opts.Page.Limit <= 0 {
		opts.Page.Limit = triggerCLIMaxPageLimit
	}
	var agg *hubclient.ListTriggersResponse
	seenCursors := map[string]bool{}
	for {
		page, err := svc.List(ctx, &opts)
		if err != nil {
			return nil, err
		}
		if page == nil {
			return nil, errors.New("empty response from hub")
		}
		if agg == nil {
			cp := *page
			cp.Triggers = append([]hubclient.Trigger(nil), page.Triggers...)
			agg = &cp
		} else {
			agg.Triggers = append(agg.Triggers, page.Triggers...)
		}
		if page.NextCursor == "" || seenCursors[page.NextCursor] {
			agg.NextCursor = ""
			return agg, nil
		}
		seenCursors[page.NextCursor] = true
		opts.Page.Cursor = page.NextCursor
	}
}

// resolveTriggerIDOrName resolves a trigger UUID, exact name, or unique ID prefix to a full UUID.
func resolveTriggerIDOrName(ctx context.Context, hubCtx *HubContext, projectID, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", newUsageError("trigger ID or name is required")
	}
	if uuid.Validate(ref) == nil {
		return ref, nil
	}

	svc := hubCtx.Client.Triggers(projectID)
	nameResp, err := svc.List(ctx, &hubclient.ListTriggersOptions{
		Name: ref,
		Page: apiclient.PageOptions{Limit: 1},
	})
	if err != nil {
		return "", wrapTriggerHubError(hubCtx, fmt.Errorf("failed to list triggers for resolution: %w", err))
	}
	if nameResp == nil {
		return "", wrapTriggerHubError(hubCtx, fmt.Errorf("failed to list triggers for resolution: %w", errors.New("empty response from hub")))
	}
	for _, tr := range nameResp.Triggers {
		if tr.Name == ref {
			return tr.ID, nil
		}
	}

	resp, err := listAllTriggers(ctx, svc, nil)
	if err != nil {
		return "", wrapTriggerHubError(hubCtx, fmt.Errorf("failed to list triggers for resolution: %w", err))
	}

	for _, tr := range resp.Triggers {
		if tr.Name == ref {
			return tr.ID, nil
		}
	}

	var prefixMatches []string
	for _, tr := range resp.Triggers {
		if strings.HasPrefix(tr.ID, ref) {
			prefixMatches = append(prefixMatches, tr.ID)
		}
	}

	switch len(prefixMatches) {
	case 0:
		if isCallerScopedTriggerView(hubCtx, resp.Scope) {
			return "", fmt.Errorf("no trigger held by this agent found matching %q (triggers held by another principal are hidden from agent callers)", ref)
		}
		return "", fmt.Errorf("no trigger found matching %q", ref)
	case 1:
		return prefixMatches[0], nil
	default:
		return "", fmt.Errorf("ambiguous trigger ID prefix %q matches %d triggers: %s", ref, len(prefixMatches), strings.Join(prefixMatches, ", "))
	}
}

func buildCreateTriggerRequest() (*hubclient.CreateTriggerRequest, error) {
	req, _, err := buildCreateTriggerRequestWithFlags(false, false, false, false)
	return req, err
}

func buildCreateTriggerRequestWithAllow(allowExplicit bool) (*hubclient.CreateTriggerRequest, []string, error) {
	return buildCreateTriggerRequestWithFlags(allowExplicit, false, false, false)
}

func buildCreateTriggerRequestWithFlags(allowExplicit, actionExplicit, commentExplicit, ackCommentExplicit bool) (*hubclient.CreateTriggerRequest, []string, error) {
	name := strings.TrimSpace(triggerName)
	if name == "" {
		return nil, nil, newUsageError("--name is required")
	}
	webhookRef := strings.TrimSpace(triggerWebhookRef)
	if triggerGitHubSource && webhookRef != "" {
		return nil, nil, newUsageError("--github and --webhook are mutually exclusive")
	}
	if !triggerGitHubSource && webhookRef == "" {
		return nil, nil, newUsageError("either --webhook or --github is required")
	}
	if triggerGitHubSource && len(triggerMatchTypes) == 0 {
		return nil, nil, newUsageError("--type is required when --github is set")
	}

	var (
		action           hubclient.TriggerActionSpec
		actions          []hubclient.TriggerActionSpec
		derivedUntrusted []string
	)
	if createActionCollector.isMulti() {
		var gErr error
		actions, derivedUntrusted, gErr = buildActionSpecsFromGroups(createActionCollector.groups, triggerGitHubSource)
		if gErr != nil {
			return nil, nil, gErr
		}
	} else {
		commentSet := commentExplicit || triggerComment != ""
		ackCommentSet := ackCommentExplicit || triggerDispatch.ackComment != ""
		if triggerReplyInThread && !commentSet {
			return nil, nil, newUsageError("--reply-in-thread requires --comment")
		}

		rawAction := strings.TrimSpace(triggerAction)
		if commentSet {
			if actionExplicit && rawAction != "github.comment" {
				return nil, nil, newUsageError("--comment cannot be used with --action %s", triggerTerminalText(rawAction, false))
			}
			rawAction = "github.comment"
		}

		actionKind, err := normalizeTriggerActionKind(rawAction)
		if err != nil {
			return nil, nil, err
		}

		if actionKind == "github.comment" && !triggerGitHubSource {
			return nil, nil, newUsageError("--comment requires --github")
		}
		if ackCommentSet && !triggerGitHubSource {
			return nil, nil, newUsageError("--ack-comment requires --github")
		}

		switch actionKind {
		case "message":
			if badFlag, ok := hasDispatchAgentFlagValues(&triggerDispatch); ok {
				return nil, nil, newUsageError("%s cannot be used with --action message", badFlag)
			}
			if ackCommentExplicit {
				return nil, nil, newUsageError("--ack-comment cannot be used with --action message")
			}
			agentRef := strings.TrimSpace(triggerAgent)
			if agentRef == "" {
				return nil, nil, newUsageError("--agent is required")
			}
			if strings.TrimSpace(triggerMessage) == "" {
				return nil, nil, newUsageError("--message is required")
			}
			offline := strings.TrimSpace(triggerOffline)
			if offline != "" && offline != "skip" && offline != "queue" {
				return nil, nil, newUsageError("invalid --offline %q (supported: skip, queue)", triggerTerminalText(offline, false))
			}
			var queueTTLSeconds int
			if rawTTL := strings.TrimSpace(triggerQueueTTL); rawTTL != "" {
				if offline != "queue" {
					return nil, nil, newUsageError("--queue-ttl requires --offline queue")
				}
				secs, tErr := parseTriggerQueueTTL(rawTTL)
				if tErr != nil {
					return nil, nil, tErr
				}
				queueTTLSeconds = secs
			}
			action = hubclient.TriggerActionSpec{
				Type:            "message",
				Message:         triggerMessage,
				Interrupt:       triggerInterrupt,
				Wake:            triggerWake,
				Plain:           triggerPlain,
				Offline:         offline,
				QueueTTLSeconds: queueTTLSeconds,
			}
			if allowExplicit || len(triggerDispatch.allow) > 0 {
				action.UntrustedVars = append([]string(nil), triggerDispatch.allow...)
			} else {
				derived, dErr := deriveMessageUntrustedVars(action.Message)
				if dErr != nil {
					return nil, nil, dErr
				}
				if len(derived) > 0 {
					action.UntrustedVars = derived
					derivedUntrusted = derived
				}
			}
			if uuid.Validate(agentRef) == nil {
				action.AgentID = agentRef
			} else {
				action.AgentName = agentRef
			}
		case "dispatch_agent":
			if badFlag, ok := hasMessageFlagValues(triggerAgent, triggerMessage, triggerInterrupt, triggerWake, triggerPlain); ok {
				return nil, nil, newUsageError("%s cannot be used with --action dispatch-agent", badFlag)
			}
			if strings.TrimSpace(triggerOffline) != "" {
				return nil, nil, newUsageError("--offline cannot be used with --action dispatch-agent")
			}
			if strings.TrimSpace(triggerQueueTTL) != "" {
				return nil, nil, newUsageError("--queue-ttl cannot be used with --action dispatch-agent")
			}
			if ackCommentExplicit && strings.TrimSpace(triggerDispatch.ackComment) == "" {
				return nil, nil, newUsageError("--ack-comment cannot be empty")
			}
			action, derivedUntrusted, err = buildDispatchAgentActionSpec(&triggerDispatch, allowExplicit)
			if err != nil {
				return nil, nil, err
			}
		case "github.comment":
			if badFlag, ok := hasMessageFlagValues(triggerAgent, triggerMessage, triggerInterrupt, triggerWake, triggerPlain); ok {
				return nil, nil, newUsageError("%s cannot be used with --action github.comment", badFlag)
			}
			if strings.TrimSpace(triggerOffline) != "" {
				return nil, nil, newUsageError("--offline cannot be used with --action github.comment")
			}
			if strings.TrimSpace(triggerQueueTTL) != "" {
				return nil, nil, newUsageError("--queue-ttl cannot be used with --action github.comment")
			}
			if badFlag, ok := hasDispatchAgentFlagValues(&triggerDispatch); ok {
				return nil, nil, newUsageError("%s cannot be used with --action github.comment", badFlag)
			}
			if ackCommentExplicit {
				return nil, nil, newUsageError("--ack-comment cannot be used with --action github.comment")
			}
			action, derivedUntrusted, err = buildGitHubCommentActionSpec(triggerComment, triggerReplyInThread, triggerDispatch.allow, allowExplicit)
			if err != nil {
				return nil, nil, err
			}
		}
	}

	source := hubclient.TriggerSourceSpec{
		Kind: "webhook",
		ID:   webhookRef,
	}
	if triggerGitHubSource {
		source = hubclient.TriggerSourceSpec{
			Kind: "github",
			ID:   "",
		}
	}

	req := &hubclient.CreateTriggerRequest{
		Name:        name,
		Description: triggerDescription,
		Source:      source,
		Action:      action,
		Actions:     actions,
	}

	whereFile := strings.TrimSpace(triggerWhereFile)
	if len(triggerWhereRules) > 0 && whereFile != "" {
		return nil, nil, newUsageError("--where and --where-file cannot be used together")
	}
	var matchRules []hubclient.TriggerMatchRuleSpec
	hasMatchRules := false
	if whereFile != "" {
		parsed, err := readTriggerWhereFile(whereFile)
		if err != nil {
			return nil, nil, err
		}
		matchRules = parsed
		hasMatchRules = true
	} else if len(triggerWhereRules) > 0 {
		matchRules = make([]hubclient.TriggerMatchRuleSpec, len(triggerWhereRules))
		for i, raw := range triggerWhereRules {
			matchRules[i] = parseTriggerWhereArg(raw)
		}
		hasMatchRules = true
	}

	if len(triggerMatchTypes) > 0 || hasMatchRules {
		req.Match = &hubclient.TriggerMatchSpec{
			Types: append([]string(nil), triggerMatchTypes...),
			Rules: matchRules,
		}
	}
	if triggerSubjectExpr != "" || triggerOnSubject != "" || triggerCoalesceWindow != "" {
		req.Subject = &hubclient.TriggerSubjectSpec{
			Expr:           triggerSubjectExpr,
			Mode:           triggerOnSubject,
			CoalesceWindow: triggerCoalesceWindow,
		}
	}
	if triggerMaxFiresPerMinute < 0 {
		return nil, nil, newUsageError("--max-per-minute must be non-negative")
	}
	if triggerMaxConcurrent < 0 {
		return nil, nil, newUsageError("--max-concurrent must be non-negative")
	}
	req.MaxFiresPerMinute = triggerMaxFiresPerMinute
	req.MaxConcurrentDispatches = triggerMaxConcurrent

	return req, derivedUntrusted, nil
}

func runTriggerCreate(cmd *cobra.Command, args []string) error {
	defer createActionCollector.reset()
	allowExplicit := cmd != nil && cmd.Flags().Changed("allow")
	actionExplicit := cmd != nil && cmd.Flags().Changed("action")
	commentExplicit := cmd != nil && cmd.Flags().Changed("comment")
	ackCommentExplicit := cmd != nil && cmd.Flags().Changed("ack-comment")
	req, derivedUntrusted, err := buildCreateTriggerRequestWithFlags(allowExplicit, actionExplicit, commentExplicit, ackCommentExplicit)
	if err != nil {
		return err
	}

	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	printDerivedUntrustedVars(derivedUntrusted)

	created, err := hubCtx.Client.Triggers(projectID).Create(ctx, req)
	if err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to create trigger: %w", formatTriggerAPIError(err)))
	}

	if isTriggerJSONOutput() {
		return outputTriggerJSON(created)
	}

	fmt.Printf("Trigger created: %s\n", created.ID)
	fmt.Printf("  Name:    %s\n", created.Name)
	fmt.Printf("  Source:  %s\n", formatTriggerSource(created.Source, 0))
	fmt.Printf("  Types:   %s\n", strings.Join(created.Match.Types, ", "))
	rules := effectiveTriggerMatchRules(created.Match)
	if len(rules) == 1 {
		fmt.Printf("  Where:   1/1 [%s] %q\n", rules[0].Dialect, rules[0].Expr)
	} else if len(rules) > 1 {
		fmt.Printf("  Where:\n")
		for i, r := range rules {
			fmt.Printf("    %d/%d [%s] %q\n", i+1, len(rules), r.Dialect, r.Expr)
		}
	}
	if created.Subject.Expr != "" {
		fmt.Printf("  Subject: %q\n", created.Subject.Expr)
	}
	if created.Subject.CoalesceWindow != "" {
		fmt.Printf("  Coalesce: %s\n", triggerTerminalText(created.Subject.CoalesceWindow, false))
	}
	printTriggerActionsSummary(effectiveTriggerActions(created))
	fmt.Printf("  Status:  %s\n", triggerStatusLabel(created.Status, created.StatusReason))
	return nil
}

func runTriggerList(cmd *cobra.Command, args []string) error {
	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	opts := &hubclient.ListTriggersOptions{
		Source: strings.TrimSpace(triggerListSource),
		Status: strings.TrimSpace(triggerListStatus),
	}
	if webhookRef := strings.TrimSpace(triggerListWebhook); webhookRef != "" {
		resolvedID, rErr := resolveWebhookEndpointIDOrName(ctx, hubCtx, projectID, webhookRef)
		if rErr != nil {
			return rErr
		}
		opts.SourceID = resolvedID
		if opts.Source == "" {
			opts.Source = "webhook"
		}
	}

	resp, err := listAllTriggers(ctx, hubCtx.Client.Triggers(projectID), opts)
	if err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to list triggers: %w", err))
	}

	if isTriggerJSONOutput() {
		return outputTriggerJSON(resp)
	}

	callerScoped := isCallerScopedTriggerView(hubCtx, resp.Scope)
	if len(resp.Triggers) == 0 {
		if callerScoped {
			fmt.Println("No triggers held by this agent (other project triggers are hidden).")
		} else {
			fmt.Println("No triggers found.")
		}
		return nil
	}

	if callerScoped {
		fmt.Println("Note: showing only triggers held by this agent (other project triggers are hidden).")
		fmt.Println()
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tNAME\tSOURCE\tTYPES\tAGENT\tSTATUS\tFIRES\tLAST FIRE")
	for _, tr := range resp.Triggers {
		shortID := tr.ID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
<<<<<<< HEAD
		agentLabel := tr.Action.AgentName
		if tr.Action.Type == "dispatch_agent" {
			agentLabel = "dispatch:" + tr.Action.Template
		} else if tr.Action.Type == "github.comment" {
			agentLabel = "github.comment"
		} else if agentLabel == "" {
			agentLabel = tr.Action.AgentID
			if len(agentLabel) > 8 {
				agentLabel = agentLabel[:8]
=======
		actions := effectiveTriggerActions(&tr)
		agentLabel := "-"
		if len(actions) > 0 {
			first := actions[0]
			switch first.Type {
			case "dispatch_agent":
				agentLabel = "dispatch:" + first.Template
			case "github.comment":
				agentLabel = "github.comment"
			default:
				agentLabel = first.AgentName
				if agentLabel == "" {
					agentLabel = first.AgentID
					if len(agentLabel) > 8 {
						agentLabel = agentLabel[:8]
					}
				}
			}
			if len(actions) > 1 {
				agentLabel = fmt.Sprintf("%s (+%d)", agentLabel, len(actions)-1)
>>>>>>> c8085a518 (feat(triggers): support ordered actions[] per trigger (P9))
			}
		}
		lastFire := "-"
		if tr.LastFireAt != nil {
			lastFire = clitime.Ago(*tr.LastFireAt)
			if tr.LastFireStatus != "" {
				lastFire += " (" + tr.LastFireStatus + ")"
			}
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			shortID,
			tr.Name,
			formatTriggerSource(tr.Source, 8),
			strings.Join(tr.Match.Types, ","),
			agentLabel,
			triggerStatusLabel(tr.Status, tr.StatusReason),
			tr.FireCount,
			lastFire,
		)
	}
	_ = w.Flush()
	return nil
}

func runTriggerDelete(cmd *cobra.Command, args []string) error {
	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	fullID, err := resolveTriggerIDOrName(ctx, hubCtx, projectID, args[0])
	if err != nil {
		return err
	}

	if err := hubCtx.Client.Triggers(projectID).Delete(ctx, fullID); err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to delete trigger: %w", err))
	}

	return outputTriggerActionResult(ActionResult{
		Status:  "ok",
		Command: "trigger delete",
		Message: fmt.Sprintf("Trigger %s deleted.", fullID),
	})
}

func runTriggerPause(cmd *cobra.Command, args []string) error {
	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	fullID, err := resolveTriggerIDOrName(ctx, hubCtx, projectID, args[0])
	if err != nil {
		return err
	}

	if _, err := hubCtx.Client.Triggers(projectID).Pause(ctx, fullID); err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to pause trigger: %w", err))
	}

	return outputTriggerActionResult(ActionResult{
		Status:  "ok",
		Command: "trigger pause",
		Message: fmt.Sprintf("Trigger %s paused.", fullID),
	})
}

func runTriggerResume(cmd *cobra.Command, args []string) error {
	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	fullID, err := resolveTriggerIDOrName(ctx, hubCtx, projectID, args[0])
	if err != nil {
		return err
	}

	if _, err := hubCtx.Client.Triggers(projectID).Resume(ctx, fullID); err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to resume trigger: %w", formatTriggerAPIError(err)))
	}

	return outputTriggerActionResult(ActionResult{
		Status:  "ok",
		Command: "trigger resume",
		Message: fmt.Sprintf("Trigger %s resumed.", fullID),
	})
}

func runTriggerHistory(cmd *cobra.Command, args []string) error {
	if triggerHistoryLimit < 0 {
		return newUsageError("--limit must be non-negative")
	}

	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	fullID, err := resolveTriggerIDOrName(ctx, hubCtx, projectID, args[0])
	if err != nil {
		return err
	}

	var opts *hubclient.ListTriggerFiresOptions
	if triggerHistoryLimit > 0 || strings.TrimSpace(triggerHistoryCursor) != "" {
		opts = &hubclient.ListTriggerFiresOptions{
			Page: apiclient.PageOptions{
				Limit:  triggerHistoryLimit,
				Cursor: strings.TrimSpace(triggerHistoryCursor),
			},
		}
	}

	resp, err := hubCtx.Client.Triggers(projectID).History(ctx, fullID, opts)
	if err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to get trigger history: %w", err))
	}

	if isTriggerJSONOutput() {
		return outputTriggerJSON(resp)
	}

	if len(resp.Fires) == 0 {
		fmt.Println("No fire history found.")
		return nil
	}

	fmt.Printf("FIRE HISTORY (%d of %d fires)\n", len(resp.Fires), resp.TotalCount)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tEVENT\tACTION\tSTATUS\tMATCHED\tDIALECT\tSUBJECT\tSTARTED\tFINISHED\tWAIT UNTIL\tERROR")
	for _, fire := range resp.Fires {
		shortID := fire.ID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		shortEvent := fire.EventID
		if len(shortEvent) > 8 {
			shortEvent = shortEvent[:8]
		}
		actionCol := fmt.Sprintf("#%d", fire.ActionIndex)
		if fire.ActionType != "" {
			actionCol = fmt.Sprintf("#%d:%s", fire.ActionIndex, fire.ActionType)
		}
		matchedThrough := fire.MatchedThrough
		if matchedThrough == "" {
			matchedThrough = "-"
		}
		decidedDialect := fire.DecidedDialect
		if decidedDialect == "" {
			decidedDialect = "-"
		}
		subj := fire.Subject
		if subj == "" {
			subj = "-"
		}
		started := clitime.Ago(fire.StartedAt)
		finished := "-"
		if fire.FinishedAt != nil {
			finished = clitime.Ago(*fire.FinishedAt)
		}
		waitUntil := "-"
		if fire.WaitUntil != nil {
			waitUntil = fire.WaitUntil.UTC().Format(time.RFC3339)
		}
		errMsg := fire.Error
		if errMsg == "" {
			errMsg = "-"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			triggerTerminalText(shortID, false),
			triggerTerminalText(shortEvent, false),
			triggerTerminalText(actionCol, false),
			triggerTerminalText(fire.Status, false),
			triggerTerminalText(matchedThrough, false),
			triggerTerminalText(decidedDialect, false),
			triggerTerminalText(subj, false),
			started,
			finished,
			waitUntil,
			triggerTerminalText(errMsg, false),
		)
	}
	_ = w.Flush()

	if triggerHistoryVerbose {
		for _, fire := range resp.Fires {
			if fire.RuleTrace == nil {
				continue
			}
			shortID := fire.ID
			if len(shortID) > 8 {
				shortID = shortID[:8]
			}
			fmt.Printf("\nRule trace (%s):\n", triggerTerminalText(shortID, false))
			for _, el := range fire.RuleTrace.Elements {
				line := fmt.Sprintf("  %d [%s] %s (%dms)",
					el.Index,
					triggerTerminalText(el.Dialect, false),
					triggerTerminalText(el.Class, false),
					el.DurationMs,
				)
				if el.Reason != "" {
					line += " reason=" + triggerTerminalText(el.Reason, false)
				}
				if el.Provenance != "" {
					line += fmt.Sprintf(" provenance=%q", triggerTerminalText(el.Provenance, false))
				}
				fmt.Println(line)
			}
			if len(fire.RuleTrace.Unexecuted) > 0 {
				fmt.Printf("  unexecuted: %v\n", fire.RuleTrace.Unexecuted)
			}
			if fire.RuleTrace.BudgetRemainingMs != nil {
				fmt.Printf("  budget_remaining_ms: %d\n", *fire.RuleTrace.BudgetRemainingMs)
			}
		}
	}

	if resp.NextCursor != "" {
		fmt.Printf("\nNext cursor: %s\n", triggerTerminalText(resp.NextCursor, false))
	}
	return nil
}

// triggerTerminalText unconditionally escapes C0/DEL/C1 control characters,
// Unicode Cf format characters (including bidirectional overrides), invalid
// UTF-8 bytes, and Unicode line/paragraph separators for terminal display,
// regardless of whether stdout is a TTY. Newlines and tabs are preserved only
// when keepLines is true (multi-line blocks).
func triggerTerminalText(s string, keepLines bool) string {
	escaped := escapeForTerminal(s, keepLines)
	if !strings.ContainsAny(escaped, "\u2028\u2029") {
		return escaped
	}
	var b strings.Builder
	b.Grow(len(escaped) + 8)
	for _, r := range escaped {
		switch r {
		case '\u2028', '\u2029':
			fmt.Fprintf(&b, `\u{%04x}`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// formatTriggerQuotedList formats a string slice as [%q,%q,...] using Go's %q
// quoting (matching 3b-1 rule quoting) so C0/C1 controls, DEL, newlines, and
// Unicode Cf characters are escaped without breaking clean ["a","b"] output.
func formatTriggerQuotedList(vals []string) string {
	if len(vals) == 0 {
		return "[]"
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range vals {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%q", v)
	}
	b.WriteByte(']')
	return b.String()
}

// escapeTriggerJSONBytes rewrites DEL (0x7f), C1 controls (0x80..0x9f),
// Unicode Cf format characters (including bidirectional overrides), U+2028 and
// U+2029 line/paragraph separators, and non-newline C0 controls inside indented
// JSON output to standard RFC 8259 \uXXXX escapes (or UTF-16 surrogate pairs
// above U+FFFF), and normalises invalid UTF-8 bytes to \ufffd. This keeps JSON
// output valid while preventing raw terminal control or bidi injection.
func escapeTriggerJSONBytes(encoded []byte) string {
	needsEscape := false
	for i := 0; i < len(encoded); {
		r, size := utf8.DecodeRune(encoded[i:])
		if (r == utf8.RuneError && size <= 1) ||
			(r < 0x20 && r != '\n') ||
			r == 0x7f ||
			(r >= 0x80 && r <= 0x9f) ||
			r == '\u2028' ||
			r == '\u2029' ||
			unicode.Is(unicode.Cf, r) {
			needsEscape = true
			break
		}
		i += size
	}
	if !needsEscape {
		return string(encoded)
	}
	var b strings.Builder
	b.Grow(len(encoded) + 16)
	for i := 0; i < len(encoded); {
		r, size := utf8.DecodeRune(encoded[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			b.WriteString(`\ufffd`)
		case (r < 0x20 && r != '\n') || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == '\u2028' || r == '\u2029' || unicode.Is(unicode.Cf, r):
			if r <= 0xffff {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				r1, r2 := utf16.EncodeRune(r)
				fmt.Fprintf(&b, `\u%04x\u%04x`, r1, r2)
			}
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

func formatTriggerSource(src hubclient.TriggerSourceSpec, maxIDLen int) string {
	if src.ID == "" {
		return src.Kind
	}
	id := src.ID
	if maxIDLen > 0 && len(id) > maxIDLen {
		id = id[:maxIDLen]
	}
	return src.Kind + ":" + id
}

func triggerStatusLabel(status, reason string) string {
	if reason == "" {
		return status
	}
	return status + " (" + reason + ")"
}

func init() {
	rootCmd.AddCommand(triggerCmd)
	triggerCmd.PersistentFlags().BoolVar(&triggerOutputJSON, "json", false, "Output in JSON format")

	triggerCmd.AddCommand(triggerCreateCmd)
	triggerCmd.AddCommand(triggerGetCmd)
	triggerCmd.AddCommand(triggerListCmd)
	triggerCmd.AddCommand(triggerDeleteCmd)
	triggerCmd.AddCommand(triggerPauseCmd)
	triggerCmd.AddCommand(triggerResumeCmd)
	triggerCmd.AddCommand(triggerHistoryCmd)
	triggerCmd.AddCommand(triggerEventCmd)
	triggerCmd.AddCommand(triggerWebhookCmd)
	triggerCmd.AddCommand(triggerPresetCmd)
	triggerCmd.AddCommand(triggerGitHubCmd)

	triggerCreateCmd.Flags().StringVar(&triggerName, "name", "", "Trigger name (required)")
	triggerCreateCmd.Flags().StringVar(&triggerDescription, "description", "", "Optional trigger description")
	triggerCreateCmd.Flags().StringVar(&triggerWebhookRef, "webhook", "", "Webhook endpoint ID or name (mutually exclusive with --github)")
	triggerCreateCmd.Flags().BoolVar(&triggerGitHubSource, "github", false, "Attach to the project's GitHub App source (mutually exclusive with --webhook)")
	triggerCreateCmd.Flags().StringSliceVar(&triggerMatchTypes, "type", nil, "Event type pattern to match (repeatable; default webhook.received for --webhook; required for --github)")
	triggerCreateCmd.Flags().StringArrayVar(&triggerWhereRules, "where", nil, "Ordered match rule [<dialect>:]<expr> (repeatable, 1-8 rules, <= 4096B each; put cheap CEL guards first; run matches() on event.attributes.<k> or one event.payload.<field>, not whole payload)")
	triggerCreateCmd.Flags().StringVar(&triggerWhereFile, "where-file", "", "Path to JSON file containing an ordered array of {dialect, expr} match rules")
	bindGroupedTriggerActionFlags(
		triggerCreateCmd.Flags(),
		&createActionCollector,
		&triggerAction, "message",
		&triggerAgent, &triggerMessage,
		&triggerInterrupt, &triggerWake, &triggerPlain,
		&triggerOffline, &triggerQueueTTL,
		&triggerDispatch,
		&triggerComment, &triggerReplyInThread,
	)
	triggerCreateCmd.Flags().StringVar(&triggerSubjectExpr, "subject", "", "Optional CEL expression deriving the concurrency subject (string, <= 1024B)")
	triggerCreateCmd.Flags().StringVar(&triggerOnSubject, "on-subject", "", "Subject concurrency policy (new, skip, or reuse; default new)")
	triggerCreateCmd.Flags().StringVar(&triggerCoalesceWindow, "coalesce", "", "Coalescing window duration (0s disables, max 10m, e.g. 30s)")
	triggerCreateCmd.Flags().IntVar(&triggerMaxFiresPerMinute, "max-per-minute", 0, "Maximum fires per minute for this trigger (0 = unlimited; max 600)")
	triggerCreateCmd.Flags().IntVar(&triggerMaxConcurrent, "max-concurrent", 0, "Maximum concurrent live agents for dispatch-agent triggers (0 = default 5; max 50)")

	triggerListCmd.Flags().StringVar(&triggerListSource, "source", "", "Filter by source kind (webhook or github)")
	triggerListCmd.Flags().StringVar(&triggerListWebhook, "webhook", "", "Filter by webhook endpoint ID or name")
	triggerListCmd.Flags().StringVar(&triggerListStatus, "status", "", "Filter by status (active or paused)")

	triggerHistoryCmd.Flags().IntVar(&triggerHistoryLimit, "limit", 0, "Maximum number of fire records to return")
	triggerHistoryCmd.Flags().StringVar(&triggerHistoryCursor, "cursor", "", "Pagination cursor from a previous history response")
	triggerHistoryCmd.Flags().BoolVar(&triggerHistoryVerbose, "verbose", false, "Show content-free per-element rule evaluation trace")
}
