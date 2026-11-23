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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
)

const errMsgSecretRequestsExperimentRequired = "secret requests need the hub.agent_secrets experiment"

// Secret request flags of scion message (design §4.6).
var (
	msgSecretRequests    []string
	msgSecretDescription string
	msgSecretRequestFile string
	// msgSecretRequestsPayload is the secret requests JSON loaded from
	// --secret-request or --secret-request-file for this invocation; nil for
	// an ordinary message.
	msgSecretRequestsPayload json.RawMessage
)

// secretRequestsStdin is where --secret-request-file - reads; tests replace it.
var secretRequestsStdin io.Reader = os.Stdin

// secretRequestsRequested reports whether this invocation sends secret requests.
func secretRequestsRequested() bool {
	return len(msgSecretRequests) > 0 || msgSecretRequestFile != ""
}

// resetSecretRequestFlags clears the secret request flag state (tests reuse the command).
func resetSecretRequestFlags() {
	msgSecretRequests = nil
	msgSecretDescription = ""
	msgSecretRequestFile = ""
	msgSecretRequestsPayload = nil
}

// validateSecretRequestFlags checks the secret-request flags against each other
// and the recipient, before anything is read or sent.
func validateSecretRequestFlags(agentTarget, group bool) error {
	if !secretRequestsRequested() {
		if msgSecretDescription != "" {
			return newUsageError("--secret-description is valid only with --secret-request")
		}
		return nil
	}
	if len(msgSecretRequests) > 0 && msgSecretRequestFile != "" {
		return newUsageError("--secret-request and --secret-request-file are mutually exclusive")
	}
	if msgSecretDescription != "" && msgSecretRequestFile != "" {
		return newUsageError("--secret-description is valid only with --secret-request, not --secret-request-file")
	}
	if msgSecretDescription != "" && len(msgSecretRequests) > 1 {
		return newUsageError("--secret-description can be used with only one --secret-request; use --secret-request-file for multiple keys with descriptions")
	}
	if decisionsRequested() {
		return newUsageError("--secret-request/--secret-request-file cannot be combined with --decisions/--decisions-file")
	}
	if msgSecretRequestFile == "-" && msgBodyFile == "-" {
		return newUsageError("--secret-request-file - and --body-file - both read stdin; use a file for one of them")
	}
	if agentTarget || group {
		return errors.New("secret requests can be sent only to a user or a conversation (user:<email>, @<email>, conv:<id>, #<thread>), not to an agent or a group")
	}
	if msgIn != "" || msgAt != "" {
		return newUsageError("--secret-request cannot be combined with --in or --at")
	}
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"--attach", len(msgAttach) > 0},
		{"--artifact", len(msgArtifacts) > 0},
		{"--wake", msgWake},
		{"--cc", len(msgCC) > 0},
		{"--channel", msgChannel != ""},
		{"--thread-id", msgThreadID != ""},
	} {
		if f.set {
			return newUsageError("--secret-request cannot be combined with %s", f.name)
		}
	}
	if !decisionsSenderIsAgent() {
		return errors.New("secret requests can be sent only by an agent; it works inside an agent container (agent mode, with SCION_AGENT_NAME set)")
	}
	return nil
}

// loadSecretRequestsPayload builds or reads the secret_requests JSON array.
// When reading --secret-request-file, only the JSON array shape is checked;
// full schema validation is the hub's to enforce.
func loadSecretRequestsPayload() (json.RawMessage, error) {
	if len(msgSecretRequests) > 0 {
		entries := make([]messages.SecretRequestInput, 0, len(msgSecretRequests))
		for _, rawKey := range msgSecretRequests {
			key := strings.TrimSpace(rawKey)
			if key == "" {
				return nil, newUsageError("--secret-request key must be non-empty")
			}
			entries = append(entries, messages.SecretRequestInput{
				Key:         key,
				Description: msgSecretDescription,
			})
		}
		enc, err := json.Marshal(entries)
		if err != nil {
			return nil, fmt.Errorf("encoding --secret-request entries: %w", err)
		}
		return json.RawMessage(enc), nil
	}

	var raw []byte
	if msgSecretRequestFile == "-" {
		b, err := io.ReadAll(secretRequestsStdin)
		if err != nil {
			return nil, fmt.Errorf("reading --secret-request-file from stdin: %w", err)
		}
		raw = b
	} else {
		b, err := os.ReadFile(msgSecretRequestFile)
		if err != nil {
			return nil, fmt.Errorf("reading --secret-request-file: %w", err)
		}
		raw = b
	}
	raw = bytes.TrimSpace(raw)
	var probe []json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("--secret-request-file must be a JSON array of secret requests: %v", err)
	}
	if len(probe) == 0 {
		return nil, errors.New("--secret-request-file must hold at least one secret request")
	}
	return json.RawMessage(raw), nil
}

// secretRequestSendLine formats one secret request in the text send output
// (design §4.6): "GITHUB_TOKEN v1 (new)" or "GITHUB_TOKEN v2 (replaced v1)".
func secretRequestSendLine(r messages.SecretRequestSendResult) string {
	if r.Replaced == nil {
		return fmt.Sprintf("%s v%d (new)", safeText(r.Key), r.Version)
	}
	return fmt.Sprintf("%s v%d (replaced v%d)", safeText(r.Key), r.Version, r.Replaced.Version)
}

// printSentSecretRequests prints one line per key of a sent secret request prompt.
func printSentSecretRequests(result *hubclient.OutboundMessageResult) {
	if result == nil {
		return
	}
	for _, r := range result.SecretRequests {
		fmt.Println(secretRequestSendLine(r))
	}
}

// applySecretRequestsToOutbound puts the loaded secret requests on an outbound
// request. The hub sets the message type of a secret-request prompt.
func applySecretRequestsToOutbound(out *hubclient.OutboundMessageRequest) {
	if msgSecretRequestsPayload == nil {
		return
	}
	out.SecretRequests = msgSecretRequestsPayload
	out.SecretRequestsUpdate = msgDecisionsUpdate
	out.Type = ""
}

// formatSecretRequestSendError surfaces clear, actionable hub errors when
// sending a secret request (409 secret_request_key_pending per-key hints,
// 409 secret_request_submitting, and 400 experiment_disabled). For non-secret
// sends it preserves the existing wrapHubError formatting.
func formatSecretRequestSendError(recipient string, err error) error {
	if msgSecretRequestsPayload == nil {
		return wrapHubError(fmt.Errorf("failed to send message to %s: %w", recipient, err))
	}
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) {
		return wrapHubError(fmt.Errorf("failed to send message to %s: %w", recipient, err))
	}
	switch {
	case apiErr.Code == "experiment_disabled":
		if msg := strings.TrimSpace(apiErr.Message); msg != "" {
			return errors.New(safeText(msg))
		}
		return errors.New(errMsgSecretRequestsExperimentRequired)
	case apiErr.StatusCode == http.StatusConflict && apiErr.Code == msgaction.ConflictSecretRequestKeyPending:
		if formatted := formatSecretRequestKeyPendingError(apiErr); formatted != "" {
			return errors.New(formatted)
		}
	case apiErr.StatusCode == http.StatusConflict && apiErr.Code == msgaction.ConflictSecretRequestSubmitting:
		if msg := strings.TrimSpace(apiErr.Message); msg != "" {
			return errors.New(safeText(msg))
		}
	}
	return wrapHubError(fmt.Errorf("failed to send message to %s: %w", recipient, err))
}

func formatSecretRequestKeyPendingError(apiErr *apiclient.APIError) string {
	msg := strings.TrimSpace(apiErr.Message)
	var lines []string
	if rawList, ok := apiErr.Details["existing"].([]interface{}); ok && len(rawList) > 0 {
		for _, raw := range rawList {
			entry, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			key, _ := entry["key"].(string)
			if key == "" {
				continue
			}
			version := 0
			if v, ok := entry["version"].(float64); ok {
				version = int(v)
			}
			messageID, _ := entry["messageId"].(string)
			var b strings.Builder
			b.WriteString(safeText(key))
			if version > 0 {
				_, _ = fmt.Fprintf(&b, " v%d", version)
			}
			b.WriteString(" is already pending")
			if messageID != "" {
				_, _ = fmt.Fprintf(&b, " (message %s)", safeText(messageID))
			}
			_, _ = fmt.Fprintf(&b, "; pass --update to replace it, or scion secret request withdraw %s", safeText(key))
			lines = append(lines, b.String())
		}
	}
	if msg != "" {
		// If the hub already formatted the per-key hint in its message, print it
		// directly; otherwise append the structured per-key hints.
		if len(lines) == 0 || (strings.Contains(msg, "--update") && strings.Contains(msg, "withdraw")) {
			return safeText(msg)
		}
		return safeText(msg) + "\n" + strings.Join(lines, "\n")
	}
	return strings.Join(lines, "\n")
}
