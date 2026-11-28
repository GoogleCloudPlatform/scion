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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
)

const errMsgCommandRequestsExperimentRequired = "command requests need the hub.command_requests experiment"

// Command request flags of scion message (ptone/scion#9278 §4.5).
var (
	msgCommandRequestKey  string
	msgCommandInline      string
	msgCommandFile        string
	msgCommandDescription string
	msgCommandWorkdir     string
	msgCommandTimeout     string
	msgCommandShareOutput bool
	msgCommandApprover    string
	msgCommandReqPayload  json.RawMessage
)

// commandRequestStdin is where --command-file - reads; tests replace it.
var commandRequestStdin io.Reader = os.Stdin

func commandRequestRequested() bool {
	return strings.TrimSpace(msgCommandRequestKey) != ""
}

func resetCommandRequestFlags() {
	msgCommandRequestKey = ""
	msgCommandInline = ""
	msgCommandFile = ""
	msgCommandDescription = ""
	msgCommandWorkdir = ""
	msgCommandTimeout = ""
	msgCommandShareOutput = false
	msgCommandApprover = ""
	msgCommandReqPayload = nil
}

// validateCommandRequestFlags checks the command-request flags against each
// other and the recipient before reading files or sending (§4.5, D6).
// Recipients may be a user (DM) or a conversation/thread (requiring --approver).
func validateCommandRequestFlags(agentTarget, group, convTarget bool) error {
	if !commandRequestRequested() {
		if msgCommandInline != "" || msgCommandFile != "" || msgCommandDescription != "" || msgCommandWorkdir != "" || msgCommandTimeout != "" || msgCommandShareOutput || msgCommandApprover != "" {
			return newUsageError("--command, --command-file, --description, --workdir, --timeout, --share-output, and --approver are valid only with --command-request")
		}
		return nil
	}
	if msgCommandInline != "" && msgCommandFile != "" {
		return newUsageError("--command and --command-file are mutually exclusive")
	}
	if msgCommandInline == "" && msgCommandFile == "" {
		return newUsageError("--command-request requires either --command or --command-file")
	}
	if decisionsRequested() {
		return newUsageError("--command-request cannot be combined with --decisions/--decisions-file")
	}
	if secretRequestsRequested() {
		return newUsageError("--command-request cannot be combined with --secret-request/--secret-request-file")
	}
	if msgCommandFile == "-" && msgBodyFile == "-" {
		return newUsageError("--command-file - and --body-file - both read stdin; use a file for one of them")
	}
	if agentTarget || group {
		return errors.New("command requests can be sent only to a user or a conversation, not to an agent or a message group")
	}
	if convTarget && strings.TrimSpace(msgCommandApprover) == "" {
		return newUsageError("command requests sent to a conversation or thread require --approver naming one human participant")
	}
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"--attach", len(msgAttach) > 0},
		{"--artifact", len(msgArtifacts) > 0},
		{"--wake", msgWake},
	} {
		if f.set {
			return newUsageError("--command-request cannot be combined with %s", f.name)
		}
	}
	if !decisionsSenderIsAgent() {
		return errors.New("command requests can be sent only by an agent; it works inside an agent container (agent mode, with SCION_AGENT_NAME set)")
	}
	return nil
}

func loadCommandRequestPayload() (json.RawMessage, error) {
	var script string
	if msgCommandFile != "" {
		var raw []byte
		var err error
		if msgCommandFile == "-" {
			raw, err = io.ReadAll(commandRequestStdin)
			if err != nil {
				return nil, fmt.Errorf("reading --command-file from stdin: %w", err)
			}
		} else {
			raw, err = os.ReadFile(msgCommandFile)
			if err != nil {
				return nil, fmt.Errorf("reading --command-file: %w", err)
			}
		}
		script = string(raw)
	} else {
		script = msgCommandInline
	}

	timeoutS := 0
	if raw := strings.TrimSpace(msgCommandTimeout); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, newUsageError("invalid --timeout %q: %v (use a Go duration like 10m or 90s)", raw, err)
		}
		if d <= 0 || d%time.Second != 0 {
			return nil, newUsageError("--timeout must be a positive whole number of seconds (1s..60m)")
		}
		timeoutS = int(d / time.Second)
	}

	input := &messages.CommandRequestInput{
		Key:                  strings.TrimSpace(msgCommandRequestKey),
		Description:          msgCommandDescription,
		Script:               script,
		Workdir:              msgCommandWorkdir,
		TimeoutS:             timeoutS,
		ShareOutputRequested: msgCommandShareOutput,
		Approver:             strings.TrimSpace(msgCommandApprover),
	}
	if _, err := messages.NormalizeCommandRequest("command_request", input); err != nil {
		return nil, err
	}
	enc, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encoding --command-request: %w", err)
	}
	return json.RawMessage(enc), nil
}

func applyCommandRequestToOutbound(out *hubclient.OutboundMessageRequest) {
	if msgCommandReqPayload == nil {
		return
	}
	out.CommandRequest = msgCommandReqPayload
	out.CommandRequestUpdate = msgDecisionsUpdate
	out.Type = ""
}

func commandRequestSendLine(r *messages.CommandRequestSendResult) string {
	if r == nil {
		return ""
	}
	if r.Replaced == nil {
		return fmt.Sprintf("%s v%d (id %s)", safeText(r.Key), r.Version, safeText(r.RowID))
	}
	return fmt.Sprintf("%s v%d (id %s, replaced v%d)", safeText(r.Key), r.Version, safeText(r.RowID), r.Replaced.Version)
}

func printSentCommandRequest(result *hubclient.OutboundMessageResult) {
	if result == nil || result.CommandRequest == nil {
		return
	}
	fmt.Println(commandRequestSendLine(result.CommandRequest))
}

func formatCommandRequestSendError(recipient string, err error) error {
	if msgCommandReqPayload == nil {
		return formatSecretRequestSendError(recipient, err)
	}
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) {
		return outboundMessageSendError(recipient, err)
	}
	switch {
	case apiErr.Code == "experiment_disabled":
		if msg := strings.TrimSpace(apiErr.Message); msg != "" {
			return errors.New(safeText(msg))
		}
		return errors.New(errMsgCommandRequestsExperimentRequired)
	case apiErr.StatusCode == http.StatusConflict && apiErr.Code == msgaction.ConflictCmdKeyPending:
		if msg := strings.TrimSpace(apiErr.Message); msg != "" {
			return errors.New(safeText(msg))
		}
	case apiErr.StatusCode == http.StatusTooManyRequests && apiErr.Code == msgaction.ConflictCmdRequestLimit:
		if msg := strings.TrimSpace(apiErr.Message); msg != "" {
			return errors.New(safeText(msg))
		}
	}
	return outboundMessageSendError(recipient, err)
}
