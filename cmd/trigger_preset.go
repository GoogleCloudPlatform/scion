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
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	triggerPresetListSource string

	triggerPresetApplyName        string
	triggerPresetApplyDescription string
	triggerPresetApplyFields      []string
	triggerPresetApplyAction      string
	triggerPresetApplyAgent       string
	triggerPresetApplyMessage     string
	triggerPresetApplyInterrupt   bool
	triggerPresetApplyWake        bool
	triggerPresetApplyPlain       bool
	triggerPresetApplyDispatch    bool
	triggerPresetApplyComment     string
	triggerPresetApplyDryRun      bool
)

var triggerPresetCmd = &cobra.Command{
	Use:   "preset",
	Short: "List, inspect, and apply trigger rule presets",
	Long:  `List server-defined trigger rule presets, inspect their field schemas, or compile and apply a preset to create a trigger in the current project.`,
}

var triggerPresetListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available trigger rule presets",
	Long:  `List trigger rule presets registered on the Hub, optionally filtered by event source (for example --source github).`,
	RunE:  runTriggerPresetList,
}

var triggerPresetShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show a trigger preset schema and fields",
	Long:  `Display the description, supported action kinds, and configurable field schema for a trigger rule preset.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runTriggerPresetShow,
}

var triggerPresetApplyCmd = &cobra.Command{
	Use:   "apply <id>",
	Short: "Compile a trigger preset and create a trigger",
	Long: `Compile a trigger rule preset on the Hub and create a trigger in the current project with the compiled event types, match rules, subject policy, and form state.

Use --dry-run to compile and preview the resulting rule without creating a trigger.

Examples:
  # Preview the compiled rule for a GitHub slash-command preset
  scion trigger preset apply github.command --field command=review --dry-run

  # Create a message trigger from a preset
  scion trigger preset apply github.command --name pr-review-cmd \
    --field command=review --field on=pull_request \
    --agent lead --message "Handle /review command" --wake`,
	Args: cobra.ExactArgs(1),
	RunE: runTriggerPresetApply,
}

var triggerPresetHubContextFn = triggerPresetHubContext

func triggerPresetHubContext() (*HubContext, context.Context, context.CancelFunc, error) {
	hubCtx, err := requireHub(projectPath)
	if err != nil {
		return nil, nil, nil, err
	}
	if !isTriggerJSONOutput() {
		PrintUsingHub(hubCtx.Endpoint)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	return hubCtx, ctx, cancel, nil
}

func runTriggerPresetList(cmd *cobra.Command, args []string) error {
	hubCtx, ctx, cancel, err := triggerPresetHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	presets, err := hubCtx.Client.TriggerPresets().List(ctx, strings.TrimSpace(triggerPresetListSource))
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to list trigger presets: %w", err))
	}

	if isTriggerJSONOutput() {
		return outputTriggerJSON(presets)
	}

	if len(presets) == 0 {
		fmt.Println("No trigger presets found.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tSOURCE\tTITLE\tFIELDS")
	for _, p := range presets {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\n",
			triggerTerminalText(p.ID, false),
			triggerTerminalText(p.Source, false),
			triggerTerminalText(p.Title, false),
			len(p.Fields),
		)
	}
	_ = w.Flush()
	return nil
}

func runTriggerPresetShow(cmd *cobra.Command, args []string) error {
	presetID := strings.TrimSpace(args[0])
	if presetID == "" {
		return newUsageError("preset ID is required")
	}

	hubCtx, ctx, cancel, err := triggerPresetHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	presets, err := hubCtx.Client.TriggerPresets().List(ctx, "")
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to list trigger presets: %w", err))
	}

	var found *hubclient.TriggerPreset
	for i := range presets {
		if presets[i].ID == presetID {
			found = &presets[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("trigger preset %q not found", triggerTerminalText(presetID, false))
	}

	if isTriggerJSONOutput() {
		return outputTriggerJSON(found)
	}

	printTriggerPresetDetail(found)
	return nil
}

func printTriggerPresetDetail(p *hubclient.TriggerPreset) {
	fmt.Printf("Preset: %s\n", triggerTerminalText(p.ID, false))
	fmt.Printf("  Title:       %s\n", triggerTerminalText(p.Title, false))
	fmt.Printf("  Source:      %s\n", triggerTerminalText(p.Source, false))
	fmt.Printf("  Version:     %d\n", p.Version)
	if p.Description != "" {
		fmt.Printf("  Description: %s\n", triggerTerminalText(p.Description, true))
	}
	if len(p.ActionKinds) > 0 {
		kinds := make([]string, len(p.ActionKinds))
		for i, k := range p.ActionKinds {
			kinds[i] = triggerTerminalText(k, false)
		}
		fmt.Printf("  Actions:     %s\n", strings.Join(kinds, ", "))
	}
	if len(p.Fields) == 0 {
		fmt.Println("  Fields:      none")
		return
	}
	fmt.Println("  Fields:")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "    NAME\tTYPE\tREQUIRED\tDEFAULT\tOPTIONS\tHELP")
	for _, f := range p.Fields {
		reqLabel := "no"
		if f.Required {
			reqLabel = "yes"
		}
		defLabel := formatPresetFieldValue(f.Default)
		if defLabel == "" {
			defLabel = "-"
		}
		optLabel := "-"
		if len(f.Enum) > 0 {
			opts := make([]string, len(f.Enum))
			for i, o := range f.Enum {
				opts[i] = triggerTerminalText(o, false)
			}
			optLabel = strings.Join(opts, "|")
		}
		_, _ = fmt.Fprintf(w, "    %s\t%s\t%s\t%s\t%s\t%s\n",
			triggerTerminalText(f.Name, false),
			triggerTerminalText(f.Type, false),
			reqLabel,
			triggerTerminalText(defLabel, false),
			optLabel,
			triggerTerminalText(f.Help, false),
		)
	}
	_ = w.Flush()
}

func formatPresetFieldValue(v any) string {
	if v == nil {
		return ""
	}
	switch typed := v.(type) {
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case []string:
		return formatTriggerQuotedList(typed)
	case []any:
		strs := make([]string, 0, len(typed))
		allStrings := true
		for _, elem := range typed {
			s, ok := elem.(string)
			if !ok {
				allStrings = false
				break
			}
			strs = append(strs, s)
		}
		if allStrings {
			return formatTriggerQuotedList(strs)
		}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(raw)
}

func parseTriggerPresetFields(rawArgs []string, schema *hubclient.TriggerPreset) (map[string]any, error) {
	if len(rawArgs) == 0 {
		return nil, nil
	}
	fieldTypes := map[string]string{}
	if schema != nil {
		for _, f := range schema.Fields {
			fieldTypes[f.Name] = f.Type
		}
	}

	fields := make(map[string]any, len(rawArgs))
	for _, raw := range rawArgs {
		idx := strings.IndexByte(raw, '=')
		if idx <= 0 {
			return nil, newUsageError("invalid --field %q: expected key=value", triggerTerminalText(raw, false))
		}
		key := strings.TrimSpace(raw[:idx])
		val := raw[idx+1:]
		if key == "" {
			return nil, newUsageError("invalid --field %q: field name is required", triggerTerminalText(raw, false))
		}

		switch fieldTypes[key] {
		case "bool":
			parsed, err := strconv.ParseBool(strings.TrimSpace(val))
			if err != nil {
				return nil, newUsageError("invalid boolean value %q for field %q (expected true or false)",
					triggerTerminalText(val, false), triggerTerminalText(key, false))
			}
			fields[key] = parsed
		case "list":
			parsed, err := parsePresetListValue(key, val)
			if err != nil {
				return nil, err
			}
			if existing, ok := fields[key].([]string); ok {
				fields[key] = append(existing, parsed...)
			} else {
				fields[key] = parsed
			}
		default:
			fields[key] = val
		}
	}
	return fields, nil
}

func parsePresetListValue(key, raw string) ([]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return []string{}, nil
	}
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		var items []string
		if err := json.Unmarshal([]byte(trimmed), &items); err != nil {
			return nil, newUsageError("invalid JSON list for field %q: %v", triggerTerminalText(key, false), err)
		}
		if items == nil {
			items = []string{}
		}
		return items, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		out = append(out, strings.TrimSpace(part))
	}
	return out, nil
}

func validateTriggerPresetApplyAction(cmd *cobra.Command, dryRun bool) (hubclient.TriggerActionSpec, error) {
	if triggerPresetApplyDispatch {
		return hubclient.TriggerActionSpec{}, newUsageError(`action "dispatch_agent" is not yet available; only "message" (--agent and --message) is supported`)
	}
	if (cmd != nil && cmd.Flags().Changed("comment")) || strings.TrimSpace(triggerPresetApplyComment) != "" {
		return hubclient.TriggerActionSpec{}, newUsageError(`action "github.comment" is not yet available; only "message" (--agent and --message) is supported`)
	}
	actionKind := strings.TrimSpace(triggerPresetApplyAction)
	if actionKind != "" && actionKind != "message" {
		if actionKind == "dispatch_agent" || actionKind == "github.comment" {
			return hubclient.TriggerActionSpec{}, newUsageError(`action %q is not yet available; only "message" (--agent and --message) is supported`, actionKind)
		}
		return hubclient.TriggerActionSpec{}, newUsageError(`unsupported action %q; only "message" (--agent and --message) is supported`, actionKind)
	}
	if dryRun {
		return hubclient.TriggerActionSpec{}, nil
	}

	if strings.TrimSpace(triggerPresetApplyName) == "" {
		return hubclient.TriggerActionSpec{}, newUsageError("--name is required")
	}
	agentRef := strings.TrimSpace(triggerPresetApplyAgent)
	if agentRef == "" {
		return hubclient.TriggerActionSpec{}, newUsageError("--agent is required")
	}
	if strings.TrimSpace(triggerPresetApplyMessage) == "" {
		return hubclient.TriggerActionSpec{}, newUsageError("--message is required")
	}

	action := hubclient.TriggerActionSpec{
		Type:      "message",
		Message:   triggerPresetApplyMessage,
		Interrupt: triggerPresetApplyInterrupt,
		Wake:      triggerPresetApplyWake,
		Plain:     triggerPresetApplyPlain,
	}
	if uuid.Validate(agentRef) == nil {
		action.AgentID = agentRef
	} else {
		action.AgentName = agentRef
	}
	return action, nil
}

func formatTriggerPresetCompileError(err error) error {
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) || len(apiErr.Details) == 0 {
		return err
	}
	rawFields, ok := apiErr.Details["fields"].([]any)
	if !ok || len(rawFields) == 0 {
		return err
	}
	parts := make([]string, 0, len(rawFields))
	for _, raw := range rawFields {
		m, isMap := raw.(map[string]any)
		if !isMap {
			continue
		}
		field := triggerTerminalText(triggerDetailString(m, "field"), false)
		code := triggerTerminalText(triggerDetailString(m, "code"), false)
		msg := triggerTerminalText(triggerDetailString(m, "message"), false)
		switch {
		case field != "" && code != "" && msg != "":
			parts = append(parts, fmt.Sprintf("%s (%s): %s", field, code, msg))
		case field != "" && msg != "":
			parts = append(parts, fmt.Sprintf("%s: %s", field, msg))
		case msg != "":
			parts = append(parts, msg)
		}
	}
	if len(parts) == 0 {
		return err
	}
	cp := *apiErr
	cp.Message = apiErr.Message + ": " + strings.Join(parts, "; ")
	return &cp
}

func buildTriggerCreateRequestFromPreset(
	name, description, sourceKind string,
	compiled *hubclient.CompiledTriggerPreset,
	action hubclient.TriggerActionSpec,
) (*hubclient.CreateTriggerRequest, error) {
	formRaw, err := json.Marshal(compiled.Form)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal compiled preset form: %w", err)
	}
	rules := make([]hubclient.TriggerMatchRuleSpec, len(compiled.Match.Rules))
	for i, r := range compiled.Match.Rules {
		rules[i] = hubclient.TriggerMatchRuleSpec(r)
	}
	types := append([]string(nil), compiled.Match.Types...)
	req := &hubclient.CreateTriggerRequest{
		Name:        strings.TrimSpace(name),
		Description: description,
		Source: hubclient.TriggerSourceSpec{
			Kind: sourceKind,
			ID:   "",
		},
		Match: &hubclient.TriggerMatchSpec{
			Types: types,
			Rules: rules,
			Form:  json.RawMessage(formRaw),
		},
		Action: action,
	}
	if compiled.Subject.Mode != "" || compiled.Subject.Expr != "" {
		req.Subject = &hubclient.TriggerSubjectSpec{
			Expr: compiled.Subject.Expr,
			Mode: compiled.Subject.Mode,
		}
	}
	return req, nil
}

func runTriggerPresetApply(cmd *cobra.Command, args []string) error {
	presetID := strings.TrimSpace(args[0])
	if presetID == "" {
		return newUsageError("preset ID is required")
	}

	action, err := validateTriggerPresetApplyAction(cmd, triggerPresetApplyDryRun)
	if err != nil {
		return err
	}

	var (
		hubCtx    *HubContext
		projectID string
		ctx       context.Context
		cancel    context.CancelFunc
	)
	if triggerPresetApplyDryRun {
		hubCtx, ctx, cancel, err = triggerPresetHubContextFn()
		if err != nil {
			return err
		}
	} else {
		hubCtx, projectID, ctx, cancel, err = triggerHubContextFn()
		if err != nil {
			return err
		}
	}
	defer cancel()

	// Fetch preset schemas from the server to coerce typed --field values and
	// determine the source kind without hard-coding preset metadata client-side.
	presets, err := hubCtx.Client.TriggerPresets().List(ctx, "")
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to fetch trigger preset schema: %w", err))
	}
	var schema *hubclient.TriggerPreset
	for i := range presets {
		if presets[i].ID == presetID {
			schema = &presets[i]
			break
		}
	}

	fields, err := parseTriggerPresetFields(triggerPresetApplyFields, schema)
	if err != nil {
		return err
	}

	compiled, err := hubCtx.Client.TriggerPresets().Compile(ctx, &hubclient.CompileTriggerPresetRequest{
		Preset: presetID,
		Fields: fields,
	})
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to compile trigger preset: %w", formatTriggerPresetCompileError(err)))
	}

	if triggerPresetApplyDryRun {
		if isTriggerJSONOutput() {
			return outputTriggerJSON(compiled)
		}
		printCompiledTriggerPreset(compiled)
		return nil
	}

	sourceKind := ""
	if schema != nil {
		sourceKind = strings.TrimSpace(schema.Source)
	}
	if sourceKind == "" {
		if dot := strings.IndexByte(presetID, '.'); dot > 0 {
			sourceKind = presetID[:dot]
		}
	}

	createReq, err := buildTriggerCreateRequestFromPreset(
		triggerPresetApplyName,
		triggerPresetApplyDescription,
		sourceKind,
		compiled,
		action,
	)
	if err != nil {
		return err
	}

	created, err := hubCtx.Client.Triggers(projectID).Create(ctx, createReq)
	if err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to create trigger from preset: %w", formatTriggerRuleError(err)))
	}

	if isTriggerJSONOutput() {
		return outputTriggerJSON(created)
	}

	printCreatedPresetTrigger(created)
	return nil
}

func printCompiledTriggerPreset(c *hubclient.CompiledTriggerPreset) {
	fmt.Printf("Compiled preset: %s (v%d)\n", triggerTerminalText(c.Form.Preset, false), c.Form.Version)
	types := make([]string, len(c.Match.Types))
	for i, t := range c.Match.Types {
		types[i] = triggerTerminalText(t, false)
	}
	fmt.Printf("  Types:   %s\n", strings.Join(types, ", "))
	if len(c.Match.Rules) == 0 {
		fmt.Println("  Rules:   none (types only)")
	} else {
		fmt.Printf("  Rules:   %d\n", len(c.Match.Rules))
		for i, r := range c.Match.Rules {
			fmt.Printf("    %d/%d [%s] %q\n", i+1, len(c.Match.Rules), triggerTerminalText(r.Dialect, false), r.Expr)
		}
	}
	if c.Subject.Mode != "" || c.Subject.Expr != "" || c.Subject.CoalesceWindow != "" {
		subj := fmt.Sprintf("mode=%s", triggerTerminalText(c.Subject.Mode, false))
		if c.Subject.CoalesceWindow != "" {
			subj += fmt.Sprintf(", coalesce_window=%s", triggerTerminalText(c.Subject.CoalesceWindow, false))
		}
		if c.Subject.Expr != "" {
			subj += fmt.Sprintf(", expr=%q", c.Subject.Expr)
		}
		fmt.Printf("  Subject: %s\n", subj)
	}
	if len(c.Form.Fields) > 0 {
		keys := make([]string, 0, len(c.Form.Fields))
		for k := range c.Form.Fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Println("  Form:")
		for _, k := range keys {
			fmt.Printf("    %s = %s\n", triggerTerminalText(k, false), triggerTerminalText(formatPresetFieldValue(c.Form.Fields[k]), false))
		}
	}
}

func printCreatedPresetTrigger(created *hubclient.Trigger) {
	agentLabel := created.Action.AgentName
	if agentLabel == "" {
		agentLabel = created.Action.AgentID
	}
	fmt.Printf("Trigger created: %s\n", triggerTerminalText(created.ID, false))
	fmt.Printf("  Name:    %s\n", triggerTerminalText(created.Name, false))
	if presetLabel := formatTriggerMatchFormLabel(created.Match.Form); presetLabel != "" {
		fmt.Printf("  Preset:  %s\n", triggerTerminalText(presetLabel, false))
	}
	fmt.Printf("  Source:  %s\n", triggerTerminalText(formatTriggerSource(created.Source, 0), false))
	fmt.Printf("  Types:   %s\n", triggerTerminalText(strings.Join(created.Match.Types, ", "), false))
	rules := effectiveTriggerMatchRules(created.Match)
	if len(rules) == 1 {
		fmt.Printf("  Where:   1/1 [%s] %q\n", triggerTerminalText(rules[0].Dialect, false), rules[0].Expr)
	} else if len(rules) > 1 {
		fmt.Println("  Where:")
		for i, r := range rules {
			fmt.Printf("    %d/%d [%s] %q\n", i+1, len(rules), triggerTerminalText(r.Dialect, false), r.Expr)
		}
	}
	if created.Subject.Expr != "" {
		fmt.Printf("  Subject: %q\n", created.Subject.Expr)
	}
	fmt.Printf("  Agent:   %s\n", triggerTerminalText(agentLabel, false))
	fmt.Printf("  Status:  %s\n", triggerTerminalText(triggerStatusLabel(created.Status, created.StatusReason), false))
}

func formatTriggerMatchFormLabel(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var form struct {
		Preset  string `json:"preset"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(raw, &form); err != nil || form.Preset == "" {
		return ""
	}
	if form.Version > 0 {
		return fmt.Sprintf("%s (v%d)", form.Preset, form.Version)
	}
	return form.Preset
}

func init() {
	triggerPresetCmd.AddCommand(triggerPresetListCmd)
	triggerPresetCmd.AddCommand(triggerPresetShowCmd)
	triggerPresetCmd.AddCommand(triggerPresetApplyCmd)

	triggerPresetListCmd.Flags().StringVar(&triggerPresetListSource, "source", "", "Filter presets by source kind (for example github)")

	triggerPresetApplyCmd.Flags().StringVar(&triggerPresetApplyName, "name", "", "Trigger name (required unless --dry-run)")
	triggerPresetApplyCmd.Flags().StringVar(&triggerPresetApplyDescription, "description", "", "Optional trigger description")
	triggerPresetApplyCmd.Flags().StringArrayVar(&triggerPresetApplyFields, "field", nil, "Preset field in key=value form (repeatable)")
	triggerPresetApplyCmd.Flags().StringVar(&triggerPresetApplyAction, "action", "message", `Action kind (currently only "message" is supported)`)
	triggerPresetApplyCmd.Flags().StringVar(&triggerPresetApplyAgent, "agent", "", "Target agent name or ID (required unless --dry-run)")
	triggerPresetApplyCmd.Flags().StringVar(&triggerPresetApplyMessage, "message", "", "Message delivered to the target agent (required unless --dry-run)")
	triggerPresetApplyCmd.Flags().BoolVar(&triggerPresetApplyInterrupt, "interrupt", false, "Interrupt the target agent")
	triggerPresetApplyCmd.Flags().BoolVar(&triggerPresetApplyWake, "wake", false, "Resume the target agent if suspended when the trigger fires")
	triggerPresetApplyCmd.Flags().BoolVar(&triggerPresetApplyPlain, "plain", false, "Deliver message text without sender prefix")
	triggerPresetApplyCmd.Flags().BoolVar(&triggerPresetApplyDispatch, "dispatch", false, "Dispatch an agent (not yet available)")
	triggerPresetApplyCmd.Flags().StringVar(&triggerPresetApplyComment, "comment", "", "Post a GitHub comment (not yet available)")
	triggerPresetApplyCmd.Flags().BoolVar(&triggerPresetApplyDryRun, "dry-run", false, "Compile and print the rule without creating a trigger")
}
