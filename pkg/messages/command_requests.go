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

package messages

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits and defaults for command requests (ptone/scion#9278 §4.3-§4.5).
const (
	MaxCommandRequestKeyLen           = 128
	MaxCommandRequestDescriptionRunes = 500
	MaxCommandRequestScriptBytes      = 4096 // agentcontrol.MaxExecArgBytes
	MaxCommandRequestWorkdirLen       = 512
	MinCommandRequestTimeoutS         = 1
	DefaultCommandRequestTimeoutS     = 600
	MaxCommandRequestTimeoutS         = 3600

	CommandRequestInterpreterBashLogin = "bash-login"
	CommandRequestHashPrefix           = "scion.command_request.v1\n"
)

var commandRequestKeyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// CommandRequestKeyNorm returns the normalized form of a command request key
// (lowercased, trimmed). Uniqueness per agent is enforced on KeyNorm.
func CommandRequestKeyNorm(key string) string {
	return strings.ToLower(strings.TrimSpace(key))
}

// ValidCommandRequestKey reports whether key matches the command request key syntax.
func ValidCommandRequestKey(key string) bool {
	return commandRequestKeyRe.MatchString(key)
}

// containsBidiControl reports whether s contains a Unicode bidirectional
// override/isolate/mark control character (U+202A–U+202E, U+2066–U+2069,
// U+200E, U+200F, U+061C).
func containsBidiControl(s string) bool {
	for _, r := range s {
		if isBidiControl(r) {
			return true
		}
	}
	return false
}

func isBidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) ||
		(r >= 0x2066 && r <= 0x2069) ||
		r == 0x200E || r == 0x200F || r == 0x061C
}

// ValidateCommandRequestScript validates a command script per §4.5:
// valid UTF-8, 1..4096 bytes, no NUL, no C0 controls other than \n and \t,
// no DEL (0x7f), and no bidi controls.
func ValidateCommandRequestScript(script string) error {
	if len(script) == 0 {
		return fmt.Errorf("script is required")
	}
	if len(script) > MaxCommandRequestScriptBytes {
		return fmt.Errorf("script is %d bytes; the limit is %d bytes", len(script), MaxCommandRequestScriptBytes)
	}
	if !utf8.ValidString(script) {
		return fmt.Errorf("script must be valid UTF-8")
	}
	for _, r := range script {
		if r == 0 {
			return fmt.Errorf("script must not contain NUL bytes")
		}
		if r < 0x20 && r != '\n' && r != '\t' {
			return fmt.Errorf("script must not contain control character U+%04X (only newline and tab are allowed)", r)
		}
		if r == 0x7f {
			return fmt.Errorf("script must not contain DEL (U+007F)")
		}
		if isBidiControl(r) {
			return fmt.Errorf("script must not contain bidirectional control character U+%04X", r)
		}
	}
	return nil
}

// ValidateCommandRequestWorkdir validates an optional command working directory
// per §4.5: "" means $HOME; otherwise an absolute, clean POSIX path <= 512 bytes
// with no control or bidi characters.
func ValidateCommandRequestWorkdir(workdir string) error {
	if workdir == "" {
		return nil
	}
	if len(workdir) > MaxCommandRequestWorkdirLen {
		return fmt.Errorf("workdir is %d bytes; the limit is %d bytes", len(workdir), MaxCommandRequestWorkdirLen)
	}
	if !utf8.ValidString(workdir) {
		return fmt.Errorf("workdir must be valid UTF-8")
	}
	for _, r := range workdir {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("workdir must not contain control characters")
		}
		if isBidiControl(r) {
			return fmt.Errorf("workdir must not contain bidirectional control characters")
		}
	}
	if !strings.HasPrefix(workdir, "/") {
		return fmt.Errorf("workdir must be an absolute path starting with '/'")
	}
	if path.Clean(workdir) != workdir {
		return fmt.Errorf("workdir must be a clean path (no '..', '.', or redundant slashes)")
	}
	return nil
}

type canonicalCommandPayload struct {
	Interpreter string `json:"interpreter"`
	Script      string `json:"script"`
	TimeoutS    int    `json:"timeout_s"`
	Workdir     string `json:"workdir"`
}

// CommandRequestCanonicalHash computes the canonical SHA-256 hex digest of a
// command request per §4.4:
//
//	hex(sha256("scion.command_request.v1\n" + json_canonical({"interpreter":"bash-login","script":...,"timeout_s":...,"workdir":...})))
func CommandRequestCanonicalHash(script, workdir string, timeoutS int) string {
	var buf bytes.Buffer
	buf.WriteString(CommandRequestHashPrefix)
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(canonicalCommandPayload{
		Interpreter: CommandRequestInterpreterBashLogin,
		Script:      script,
		TimeoutS:    timeoutS,
		Workdir:     workdir,
	})
	raw := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// CommandRequestInput is the structured command request payload supplied by an
// agent in POST /api/v1/agents/{id}/outbound-message.
type CommandRequestInput struct {
	Key                  string `json:"key"`
	Description          string `json:"description,omitempty"`
	Script               string `json:"script"`
	Workdir              string `json:"workdir,omitempty"`
	TimeoutS             int    `json:"timeout_s,omitempty"`
	ShareOutputRequested bool   `json:"share_output_requested,omitempty"`
}

// CommandRequest is a validated, normalized command request ready for persistence.
type CommandRequest struct {
	Key                  string `json:"key"`
	KeyNorm              string `json:"key_norm"`
	Description          string `json:"description,omitempty"`
	Script               string `json:"script"`
	Workdir              string `json:"workdir,omitempty"`
	TimeoutS             int    `json:"timeout_s"`
	ShareOutputRequested bool   `json:"share_output_requested,omitempty"`
	CommandSHA256        string `json:"command_sha256"`
}

// NormalizeCommandRequest validates and normalizes a single CommandRequestInput.
// Failures are returned as DecisionValidationErrors under root (normally "command_request").
func NormalizeCommandRequest(root string, in *CommandRequestInput) (*CommandRequest, error) {
	var errs DecisionValidationErrors
	add := func(field, code, format string, args ...any) {
		errs = append(errs, &DecisionValidationError{
			Field:   field,
			Code:    code,
			Message: fmt.Sprintf(format, args...),
		})
	}
	if in == nil {
		add(root, DecisionErrRequired, "command_request is required")
		return nil, errs
	}

	key := strings.TrimSpace(in.Key)
	norm := CommandRequestKeyNorm(key)
	switch {
	case key == "":
		add(root+".key", DecisionErrRequired, "key is required")
	case !ValidCommandRequestKey(key):
		add(root+".key", DecisionErrInvalid, "key must match [A-Za-z0-9][A-Za-z0-9._-]{0,%d}", MaxCommandRequestKeyLen-1)
	}

	desc := strings.TrimSpace(in.Description)
	switch {
	case desc == "":
	case !validQuestionText(desc) || containsBidiControl(desc):
		add(root+".description", DecisionErrInvalid, "description must be valid UTF-8 text without control or bidi characters")
	case utf8.RuneCountInString(desc) > MaxCommandRequestDescriptionRunes:
		add(root+".description", DecisionErrTooLong, "description is at most %d characters", MaxCommandRequestDescriptionRunes)
	}

	if err := ValidateCommandRequestScript(in.Script); err != nil {
		code := DecisionErrInvalid
		if len(in.Script) == 0 {
			code = DecisionErrRequired
		} else if len(in.Script) > MaxCommandRequestScriptBytes {
			code = DecisionErrTooLong
		}
		add(root+".script", code, "%s", err.Error())
	}

	workdir := in.Workdir
	if err := ValidateCommandRequestWorkdir(workdir); err != nil {
		code := DecisionErrInvalid
		if len(workdir) > MaxCommandRequestWorkdirLen {
			code = DecisionErrTooLong
		}
		add(root+".workdir", code, "%s", err.Error())
	}

	timeoutS := in.TimeoutS
	if timeoutS == 0 {
		timeoutS = DefaultCommandRequestTimeoutS
	}
	if timeoutS < MinCommandRequestTimeoutS || timeoutS > MaxCommandRequestTimeoutS {
		add(root+".timeout_s", DecisionErrInvalid, "timeout_s must be between %d and %d seconds", MinCommandRequestTimeoutS, MaxCommandRequestTimeoutS)
	}

	if len(errs) > 0 {
		return nil, errs
	}

	return &CommandRequest{
		Key:                  key,
		KeyNorm:              norm,
		Description:          desc,
		Script:               in.Script,
		Workdir:              workdir,
		TimeoutS:             timeoutS,
		ShareOutputRequested: in.ShareOutputRequested,
		CommandSHA256:        CommandRequestCanonicalHash(in.Script, workdir, timeoutS),
	}, nil
}

// CommandRequestSendResult is returned in the outbound-message send response.
type CommandRequestSendResult struct {
	Key           string                  `json:"key"`
	Version       int                     `json:"version"`
	RowID         string                  `json:"row_id"`
	CommandSHA256 string                  `json:"command_sha256"`
	Replaced      *CommandRequestReplaced `json:"replaced,omitempty"`
}

// CommandRequestReplaced describes the prior version replaced by a send.
type CommandRequestReplaced struct {
	Version int    `json:"version"`
	Status  string `json:"status"`
}

// CommandRequestReplacedBy names the newer version that replaced a request row.
type CommandRequestReplacedBy struct {
	RowID     string    `json:"rowId"`
	MessageID string    `json:"messageId,omitempty"`
	Version   int       `json:"version"`
	At        time.Time `json:"at"`
}

// CommandRequestApproved describes who approved a request and when.
type CommandRequestApproved struct {
	At       time.Time `json:"at"`
	ByUserID string    `json:"byUserId,omitempty"`
	ByName   string    `json:"byName"`
	ByEmail  string    `json:"byEmail,omitempty"`
	Surface  string    `json:"surface"`
}

// CommandRequestRunView is the client-visible view of a command run (no output).
type CommandRequestRunView struct {
	RunID         string     `json:"runId"`
	RequestID     string     `json:"requestId"`
	Status        string     `json:"status"`
	Reason        string     `json:"reason,omitempty"`
	Wake          string     `json:"wake,omitempty"`
	ExecAgentID   string     `json:"execAgentId"`
	ExecProjectID string     `json:"execProjectId"`
	ShareOutput   bool       `json:"shareOutput"`
	TimeoutS      int        `json:"timeoutS"`
	Workdir       string     `json:"workdir,omitempty"`
	ExitCode      *int       `json:"exitCode,omitempty"`
	DurationMS    *int64     `json:"durationMs,omitempty"`
	OutputBytes   *int64     `json:"outputBytes,omitempty"`
	OutputStatus  string     `json:"outputStatus,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	DispatchedAt  *time.Time `json:"dispatchedAt,omitempty"`
	FinishedAt    *time.Time `json:"finishedAt,omitempty"`
}

// CommandRequestExecContainerView describes the approver's execution container
// state returned on GET /api/v1/command-requests/{id} for the approver only.
type CommandRequestExecContainerView struct {
	State          string `json:"state"`
	AgentID        string `json:"agentId,omitempty"`
	AgentName      string `json:"agentName,omitempty"`
	ProjectID      string `json:"projectId,omitempty"`
	ProjectName    string `json:"projectName,omitempty"`
	HomePersistent *bool  `json:"homePersistent,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

// CommandRequestView is one version of a command request as clients see it.
type CommandRequestView struct {
	RowID                string                    `json:"rowId"`
	MessageID            string                    `json:"messageId,omitempty"`
	ProjectID            string                    `json:"projectId"`
	AgentID              string                    `json:"agentId"`
	AgentName            string                    `json:"agentName"`
	Key                  string                    `json:"key"`
	Version              int                       `json:"version"`
	Description          string                    `json:"description,omitempty"`
	Script               string                    `json:"script"`
	Workdir              string                    `json:"workdir,omitempty"`
	TimeoutS             int                       `json:"timeoutS"`
	ShareOutputRequested bool                      `json:"shareOutputRequested"`
	CommandSHA256        string                    `json:"commandSha256"`
	ApproverUserID       string                    `json:"approverUserId"`
	ConversationID       string                    `json:"conversationId,omitempty"`
	Status               string                    `json:"status"`
	CreatedAt            time.Time                 `json:"createdAt"`
	ExpiresAt            time.Time                 `json:"expiresAt"`
	ResolvedAt           *time.Time                `json:"resolvedAt,omitempty"`
	ReplacedBy           *CommandRequestReplacedBy `json:"replacedBy,omitempty"`
	Approved             *CommandRequestApproved   `json:"approved,omitempty"`
	Run                  *CommandRequestRunView    `json:"run,omitempty"`
}

// CommandRequestDetailResponse is the 200 response of GET /api/v1/command-requests/{id}.
type CommandRequestDetailResponse struct {
	Request       CommandRequestView               `json:"request"`
	Run           *CommandRequestRunView           `json:"run,omitempty"`
	ExecContainer *CommandRequestExecContainerView `json:"exec_container,omitempty"`
	CanApprove    bool                             `json:"canApprove"`
	Reason        string                           `json:"reason,omitempty"`
}

// ApproveCommandRequestBody is the request body of POST /api/v1/command-requests/{id}/approve.
type ApproveCommandRequestBody struct {
	RowID          string `json:"row_id,omitempty"`
	CommandSHA256  string `json:"command_sha256"`
	ShareOutput    bool   `json:"share_output,omitempty"`
	Surface        string `json:"surface,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// ApproveCommandRequestResponse is the 202 (or 200 on idempotent replay) response
// of POST /api/v1/command-requests/{id}/approve.
type ApproveCommandRequestResponse struct {
	Request  CommandRequestView    `json:"request"`
	Run      CommandRequestRunView `json:"run"`
	Replayed bool                  `json:"replayed,omitempty"`
}

// AgentCommandRequestListResponse is the 200 response of GET /api/v1/agent/command-requests.
type AgentCommandRequestListResponse struct {
	Items      []CommandRequestView `json:"items"`
	NextCursor string               `json:"nextCursor,omitempty"`
}

// CommandRequestFallbackInput holds the fields rendered into the Markdown
// fallback body of a command-request message (§4.5).
type CommandRequestFallbackInput struct {
	Key          string
	Version      int
	Description  string
	Script       string
	Workdir      string
	TimeoutS     int
	ApproverHint string
	AgentName    string
	ProjectName  string
}

// RenderCommandRequestFallback renders the Markdown fallback body stored on a
// command-request message for non-web clients (§4.5): description, then the
// script in a fenced sh block, then workdir, timeout, and approver.
func RenderCommandRequestFallback(intro string, in CommandRequestFallbackInput) string {
	var b strings.Builder
	intro = strings.TrimSpace(intro)
	if intro != "" {
		b.WriteString(intro)
		b.WriteString("\n\n---\n\n")
	}
	header := fmt.Sprintf("**Command request** `%s`", in.Key)
	if in.Version > 0 {
		header += fmt.Sprintf(" (v%d)", in.Version)
	}
	switch {
	case in.AgentName != "" && in.ProjectName != "":
		header += fmt.Sprintf(" from **%s** in project **%s**", escapeMarkdownText(in.AgentName), escapeMarkdownText(in.ProjectName))
	case in.AgentName != "":
		header += fmt.Sprintf(" from **%s**", escapeMarkdownText(in.AgentName))
	}
	b.WriteString(header)
	b.WriteString("\n\n")
	if desc := strings.TrimSpace(in.Description); desc != "" {
		b.WriteString(escapeMarkdownText(desc))
		b.WriteString("\n\n")
	}

	// Pick a fence longer than any backtick run in the script so the script
	// cannot break out of its fenced block.
	fence := "```"
	for strings.Contains(in.Script, fence) {
		fence += "`"
	}
	b.WriteString(fence)
	b.WriteString("sh\n")
	b.WriteString(in.Script)
	if !strings.HasSuffix(in.Script, "\n") {
		b.WriteString("\n")
	}
	b.WriteString(fence)
	b.WriteString("\n\n")

	wd := in.Workdir
	if wd == "" {
		wd = "$HOME"
	}
	fmt.Fprintf(&b, "- **Workdir:** `%s`\n", wd)
	fmt.Fprintf(&b, "- **Timeout:** %ds\n", in.TimeoutS)
	if in.ApproverHint != "" {
		fmt.Fprintf(&b, "- **Approver:** %s\n", escapeMarkdownText(in.ApproverHint))
	}
	return b.String()
}
