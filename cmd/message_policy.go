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
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

const (
	messagePolicyDefaultTimeout = 30 * time.Second

	// maxMessagePolicySpecFileBytes bounds policy spec/rule files read by CLI commands.
	maxMessagePolicySpecFileBytes = 64 * 1024

	// maxMessagePolicyTestInputBytes bounds policy test sample/request files so
	// 16 KiB+ bodies (for testing UTF-8 truncation behavior) fit comfortably.
	maxMessagePolicyTestInputBytes = 256 * 1024

	errMsgMessagePoliciesExperimentRequired = "message policies require the hub.message_policies experiment to be enabled on the Hub"
)

// messagePolicyClientLoader resolves the project-scoped MessagePolicyService.
// Tests replace this seam to run CLI commands against an httptest Hub without
// touching filesystem project discovery.
var messagePolicyClientLoader = func(cmd *cobra.Command) (hubclient.MessagePolicyService, error) {
	hubCtx, err := requireHub(projectPath)
	if err != nil {
		return nil, err
	}
	if !messagePolicyWantJSON(cmd) {
		PrintUsingHub(hubCtx.Endpoint)
	}
	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return nil, wrapHubError(err)
	}
	return hubCtx.Client.ProjectMessagePolicies(projectID), nil
}

// messagePolicyCmd is the top-level command group for project message policies.
var messagePolicyCmd = newMessagePolicyCmd()

func newMessagePolicyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "message-policy",
		Short: "Manage project message policies",
		Long: `Author, inspect, test, and audit project message policies.

Each Hub reports its active enforcement coverage (coverage.enforced_surfaces and
coverage.uncovered_agent_paths) in list and get responses. Currently enforced
outbound agent surfaces are agent_to_user and agent_to_conversation; uncovered
agent paths (agent_dm, group_post, group_set, mention_fanout, mention_delivery,
broadcast), scheduled deliveries, user-originated sends, and inbound external
bridges are NOT evaluated.

In agent mode, only read-only inspection ('list' and 'get') is available;
policy authoring, updates, deletion, sample testing, and decision log queries are
human-mode operations requiring project owner/admin or Hub admin permissions.`,
		// Args/Run make this command Runnable so cobra validates subcommand
		// names, as for "scion artifact" and "scion hub": an unrecognized
		// subcommand, including one removed in agent mode (create, delete, test,
		// decisions), returns an error with a non-zero exit instead of falling
		// through to this group's help with exit status 0.
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 || args[0] == "help" {
				return pflag.ErrHelp
			}
			return cobra.NoArgs(cmd, args)
		},
		Run: func(cmd *cobra.Command, args []string) {},
	}
	cmd.AddCommand(
		newMessagePolicyCreateCmd(),
		newMessagePolicyListCmd(),
		newMessagePolicyGetCmd(),
		newMessagePolicyUpdateCmd(),
		newMessagePolicyDeleteCmd(),
		newMessagePolicyTestCmd(),
		newMessagePolicyDecisionsCmd(),
	)
	return cmd
}

func init() {
	rootCmd.AddCommand(messagePolicyCmd)
}

func messagePolicyServiceForCmd(cmd *cobra.Command) (hubclient.MessagePolicyService, context.Context, context.CancelFunc, error) {
	svc, err := messagePolicyClientLoader(cmd)
	if err != nil {
		return nil, nil, nil, err
	}
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, messagePolicyDefaultTimeout)
	return svc, ctx, cancel, nil
}

func messagePolicyWantJSON(cmd *cobra.Command) bool {
	if isJSONOutput() {
		return true
	}
	if cmd == nil {
		return false
	}
	v, err := cmd.Flags().GetBool("json")
	return err == nil && v
}

type formattedMessagePolicyError struct {
	msg   string
	cause error
}

func (e *formattedMessagePolicyError) Error() string { return e.msg }
func (e *formattedMessagePolicyError) Unwrap() error { return e.cause }

func formatMessagePolicyAPIError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) {
		return wrapHubError(err)
	}

	msg := strings.TrimSpace(apiErr.Message)
	if apiErr.Code == "experiment_disabled" ||
		(apiErr.StatusCode == http.StatusNotFound && strings.EqualFold(msg, "route not found")) {
		return &formattedMessagePolicyError{
			msg:   errMsgMessagePoliciesExperimentRequired,
			cause: err,
		}
	}

	switch apiErr.Code {
	case messagepolicy.ErrCodeInvalidRule, messagepolicy.ErrCodeNotYetSupported:
		var qualifiers []string
		if cat := detailString(apiErr.Details, "category"); cat != "" {
			qualifiers = append(qualifiers, "category="+safeText(cat))
		}
		if path := detailString(apiErr.Details, "path"); path != "" {
			qualifiers = append(qualifiers, "path="+safeText(path))
		}
		if line, ok := detailInt(apiErr.Details, "line"); ok && line > 0 {
			qualifiers = append(qualifiers, fmt.Sprintf("line=%d", line))
		}
		if col, ok := detailInt(apiErr.Details, "column"); ok && col > 0 {
			qualifiers = append(qualifiers, fmt.Sprintf("column=%d", col))
		}
		if field := detailString(apiErr.Details, "field"); field != "" && len(qualifiers) == 0 {
			qualifiers = append(qualifiers, "field="+safeText(field))
		}
		prefix := safeText(apiErr.Code)
		if len(qualifiers) > 0 {
			prefix += " [" + strings.Join(qualifiers, ", ") + "]"
		}
		return &formattedMessagePolicyError{
			msg:   fmt.Sprintf("%s: %s", prefix, safeText(msg)),
			cause: err,
		}

	case messagepolicy.ErrCodeSurfaceNotCovered:
		field := detailString(apiErr.Details, "field")
		suffix := ""
		if enforced := detailEnforcedSurfaces(apiErr.Details); len(enforced) > 0 {
			suffix = fmt.Sprintf(" (Coverage (this Hub): %s)", safeListOrNone(enforced))
		}
		if field != "" {
			return &formattedMessagePolicyError{
				msg:   fmt.Sprintf("surface_not_covered (%s): %s%s", safeText(field), safeText(msg), suffix),
				cause: err,
			}
		}
		return &formattedMessagePolicyError{
			msg:   fmt.Sprintf("surface_not_covered: %s%s", safeText(msg), suffix),
			cause: err,
		}
	}

	if apiErr.IsUnauthorized() || apiErr.IsServerError() {
		return wrapHubError(err)
	}
	if msg != "" {
		if apiErr.Code != "" {
			return &formattedMessagePolicyError{
				msg:   fmt.Sprintf("%s: %s", safeText(apiErr.Code), safeText(msg)),
				cause: err,
			}
		}
		return &formattedMessagePolicyError{
			msg:   safeText(msg),
			cause: err,
		}
	}
	return wrapHubError(err)
}

func detailString(details map[string]interface{}, key string) string {
	if len(details) == 0 {
		return ""
	}
	v, ok := details[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

func detailInt(details map[string]interface{}, key string) (int, bool) {
	if len(details) == 0 {
		return 0, false
	}
	switch v := details[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	case string:
		n, err := strconv.Atoi(v)
		return n, err == nil
	default:
		return 0, false
	}
}

func detailStringSlice(details map[string]interface{}, key string) []string {
	if len(details) == 0 {
		return nil
	}
	switch raw := details[key].(type) {
	case []string:
		out := make([]string, 0, len(raw))
		for _, item := range raw {
			if trimmed := strings.TrimSpace(item); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out
	case []interface{}:
		out := make([]string, 0, len(raw))
		for _, elem := range raw {
			if s, ok := elem.(string); ok {
				if trimmed := strings.TrimSpace(s); trimmed != "" {
					out = append(out, trimmed)
				}
			}
		}
		return out
	default:
		return nil
	}
}

func detailEnforcedSurfaces(details map[string]interface{}) []string {
	if s := detailStringSlice(details, "enforced_surfaces"); len(s) > 0 {
		return s
	}
	if covMap, ok := details["coverage"].(map[string]interface{}); ok {
		if s := detailStringSlice(covMap, "enforced_surfaces"); len(s) > 0 {
			return s
		}
	}
	return nil
}

func writeMessagePolicyCoverage(out io.Writer, cov messagepolicy.CoverageInfo) {
	_, _ = fmt.Fprintln(out, "Coverage (this Hub):")
	_, _ = fmt.Fprintf(out, "  enforced_surfaces:     %s\n", safeListOrNone(cov.EnforcedSurfaces))
	_, _ = fmt.Fprintf(out, "  uncovered_agent_paths: %s\n", safeListOrNone(cov.UncoveredAgentPaths))
}

func safeListOrNone(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}
	cleaned := make([]string, 0, len(items))
	for _, it := range items {
		cleaned = append(cleaned, safeText(it))
	}
	return strings.Join(cleaned, ", ")
}

func writeMessagePolicyDetail(out io.Writer, pol *hubclient.MessagePolicyResponse) {
	if pol == nil {
		return
	}
	_, _ = fmt.Fprintf(out, "Name:               %s\n", safeText(pol.Name))
	_, _ = fmt.Fprintf(out, "ID:                 %s\n", safeText(pol.ID))
	_, _ = fmt.Fprintf(out, "Project ID:         %s\n", safeText(pol.ProjectID))
	_, _ = fmt.Fprintf(out, "Mode:               %s\n", safeText(pol.Mode))
	_, _ = fmt.Fprintf(out, "Enabled:            %t\n", pol.Enabled)
	_, _ = fmt.Fprintf(out, "Revision:           %d\n", pol.Revision)
	if pol.Description != "" {
		_, _ = fmt.Fprintf(out, "Description:        %s\n", safeText(pol.Description))
	}
	_, _ = fmt.Fprintf(out, "Surfaces:           %s\n", safeListOrNone(pol.Scope.Surfaces))
	if len(pol.Scope.Sender.AgentIDs) > 0 {
		_, _ = fmt.Fprintf(out, "Sender Agents:      %s\n", safeListOrNone(pol.Scope.Sender.AgentIDs))
	} else {
		_, _ = fmt.Fprintln(out, "Sender Agents:      all project agents")
	}
	if len(pol.Scope.Recipient.Kinds) > 0 {
		_, _ = fmt.Fprintf(out, "Recipient Kinds:    %s\n", safeListOrNone(pol.Scope.Recipient.Kinds))
	}
	_, _ = fmt.Fprintf(out, "On Failed:          %s\n", safeText(pol.OnFailed))
	if pol.OnUncertain != "" {
		_, _ = fmt.Fprintf(out, "On Uncertain:       %s\n", safeText(pol.OnUncertain))
	}
	if len(pol.OnFailedReasons) > 0 {
		keys := make([]string, 0, len(pol.OnFailedReasons))
		for k := range pol.OnFailedReasons {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		pairs := make([]string, 0, len(keys))
		for _, k := range keys {
			pairs = append(pairs, fmt.Sprintf("%s=%s", safeText(k), safeText(pol.OnFailedReasons[k])))
		}
		_, _ = fmt.Fprintf(out, "On Failed Reasons:  %s\n", strings.Join(pairs, ", "))
	}
	if pol.RejectReason != "" {
		_, _ = fmt.Fprintf(out, "Reject Reason:      %s\n", safeText(pol.RejectReason))
	}
	if pol.RejectRemediation != "" {
		_, _ = fmt.Fprintf(out, "Reject Remediation: %s\n", safeText(pol.RejectRemediation))
	}
	_, _ = fmt.Fprintf(out, "Violation Rule:     %s\n", collapseSingleLine(pol.ViolationRule))
	if strings.TrimSpace(pol.SystemOneRule) != "" {
		_, _ = fmt.Fprintf(out, "System One Rule:    %s\n", collapseSingleLine(pol.SystemOneRule))
	}
	if len(pol.SystemOneRefs) > 0 {
		_, _ = fmt.Fprintf(out, "System One Refs:    %s\n", safeListOrNone(pol.SystemOneRefs))
	}
	if !pol.CreatedAt.IsZero() {
		_, _ = fmt.Fprintf(out, "Created:            %s\n", clitime.Format(pol.CreatedAt, clitime.Full))
	}
	if !pol.UpdatedAt.IsZero() {
		_, _ = fmt.Fprintf(out, "Updated:            %s\n", clitime.Format(pol.UpdatedAt, clitime.Full))
	}
	_, _ = fmt.Fprintln(out)
	writeMessagePolicyCoverage(out, pol.Coverage)
}

// readBoundedInput reads at most maxBytes from path (or cmd.InOrStdin() when
// path is "-"). It performs read-only access and never writes local files.
func readBoundedInput(cmd *cobra.Command, path string, maxBytes int64, label string) ([]byte, error) {
	var r io.Reader
	if path == "-" {
		if cmd != nil {
			r = cmd.InOrStdin()
		} else {
			r = os.Stdin
		}
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("failed to open %s %q: %w", label, path, err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}

	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", label, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%s exceeds maximum size of %d bytes", label, maxBytes)
	}
	return data, nil
}

// decodeYAMLOrJSONStrict decodes YAML or JSON into out, converting via JSON so
// struct `json` tags remain the single wire schema while rejecting unknown keys.
func decodeYAMLOrJSONStrict(data []byte, out any, label string) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return fmt.Errorf("%s is empty", label)
	}
	var raw any
	dec := yaml.NewDecoder(bytes.NewReader(trimmed))
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("failed to parse %s: %w", label, err)
	}
	jsonBytes, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("failed to normalize %s: %w", label, err)
	}
	jdec := json.NewDecoder(bytes.NewReader(jsonBytes))
	jdec.DisallowUnknownFields()
	if err := jdec.Decode(out); err != nil {
		return fmt.Errorf("invalid %s: %w", label, err)
	}
	return nil
}

func parseKeyValuePairs(entries []string, flagName string) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		k, v, ok := strings.Cut(entry, "=")
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			return nil, newUsageError("invalid %s %q: expected REASON=OUTCOME (e.g. budget_exhausted=admit)", flagName, entry)
		}
		out[k] = v
	}
	return out, nil
}
