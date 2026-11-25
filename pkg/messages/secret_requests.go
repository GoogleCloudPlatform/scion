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
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits of a secret request prompt.
const (
	// MaxSecretRequestsPerMessage is the maximum number of secret keys one
	// secret-request message may carry.
	MaxSecretRequestsPerMessage = 10
	// MaxSecretRequestKeyLen is the maximum length of a requested secret key.
	MaxSecretRequestKeyLen = 128
	// MaxSecretRequestDescriptionLen is the maximum number of runes in a
	// secret request description.
	MaxSecretRequestDescriptionLen = 1000
	// MaxSecretRequestTargetLen is the maximum number of runes in a secret
	// request target.
	MaxSecretRequestTargetLen = 256
	// MaxSecretRequestsPayloadBytes caps the JSON encoding of normalized
	// secret requests in one message.
	MaxSecretRequestsPayloadBytes = 32 * 1024
)

// Secret request secret_type values.
const (
	SecretRequestTypeEnvironment = "environment"
	SecretRequestTypeFile        = "file"
	SecretRequestTypeOnDemand    = "on_demand"
	// SecretRequestTypeVariable is the retired legacy type accepted by
	// NormalizeSecretRequests and mapped to SecretRequestTypeOnDemand.
	SecretRequestTypeVariable = "variable"
)

// secretRequestKeyRe matches valid secret keys (uppercase/lowercase letters,
// digits, underscores, starting with a letter or underscore).
var secretRequestKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// SecretRequestInput is one secret request entry as an agent supplies it in
// POST /api/v1/agents/{id}/outbound-message (or --secret-request-file).
type SecretRequestInput struct {
	Key         string `json:"key"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"`
	Target      string `json:"target,omitempty"`

	extraFields []string
}

// UnmarshalJSON decodes a secret request object, recording unknown fields so
// NormalizeSecretRequests reports them with their field path.
func (s *SecretRequestInput) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return fmt.Errorf("a secret request must be an object")
	}
	fields, order, err := decodeObject(data)
	if err != nil {
		return err
	}
	var out SecretRequestInput
	for _, name := range order {
		raw := fields[name]
		var dst *string
		switch name {
		case "key":
			dst = &out.Key
		case "description":
			dst = &out.Description
		case "type":
			dst = &out.Type
		case "target":
			dst = &out.Target
		default:
			out.extraFields = append(out.extraFields, name)
			continue
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			return fmt.Errorf("secret request %s must be a string", name)
		}
	}
	*s = out
	return nil
}

// MarshalJSON writes the canonical object form.
func (s SecretRequestInput) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Key         string `json:"key"`
		Description string `json:"description,omitempty"`
		Type        string `json:"type,omitempty"`
		Target      string `json:"target,omitempty"`
	}{s.Key, s.Description, s.Type, s.Target})
}

// SecretRequest is one secret request in canonical normalized form.
type SecretRequest struct {
	Key         string `json:"key"`
	KeyNorm     string `json:"key_norm"`
	Description string `json:"description,omitempty"`
	SecretType  string `json:"type"`
	Target      string `json:"target,omitempty"`
}

// SecretRequestKeyNorm returns the normalized form of a secret request key
// (upper-cased, trimmed). Uniqueness per agent is enforced on KeyNorm.
func SecretRequestKeyNorm(key string) string {
	return strings.ToUpper(strings.TrimSpace(key))
}

// ValidSecretRequestKey reports whether key matches the secret key syntax.
func ValidSecretRequestKey(key string) bool {
	return secretRequestKeyRe.MatchString(key)
}

// ValidateSecretRequestFileTarget validates the target path of a file-type
// secret request. Single source of truth shared by NormalizeSecretRequests and
// hub write validation: rejects ".." and requires a "~/" prefix so agent-chosen
// request targets stay inside the agent home rather than writing outside ~.
func ValidateSecretRequestFileTarget(target string) error {
	if strings.Contains(target, "..") {
		return fmt.Errorf("target path must not contain '..'")
	}
	if !strings.HasPrefix(target, "~/") {
		return fmt.Errorf("agent-written file secrets must target a path under the agent home (~/...)")
	}
	return nil
}
func isReservedSecretRequestEnvTarget(target string) bool {
	upper := strings.ToUpper(strings.TrimSpace(target))
	return strings.HasPrefix(upper, "SCION_") || strings.HasPrefix(upper, "GCE_METADATA_")
}

// NormalizeSecretRequests validates and normalizes secret request entries.
// Every failure is reported as a DecisionValidationErrors (field, code, message)
// under root (normally "secret_requests").
func NormalizeSecretRequests(root string, in []SecretRequestInput) ([]SecretRequest, error) {
	var errs DecisionValidationErrors
	add := func(field, code, format string, args ...any) {
		errs = append(errs, &DecisionValidationError{
			Field:   field,
			Code:    code,
			Message: fmt.Sprintf(format, args...),
		})
	}
	switch {
	case len(in) == 0:
		add(root, DecisionErrRequired, "at least one secret request is required")
		return nil, errs
	case len(in) > MaxSecretRequestsPerMessage:
		add(root, DecisionErrTooMany, "at most %d secret requests per message, got %d", MaxSecretRequestsPerMessage, len(in))
		return nil, errs
	}

	out := make([]SecretRequest, 0, len(in))
	seen := make(map[string]int, len(in))
	for i, item := range in {
		p := fmt.Sprintf("%s[%d]", root, i)
		for _, name := range item.extraFields {
			add(p+"."+name, DecisionErrUnknownField, "unknown field %q", name)
		}

		key := strings.TrimSpace(item.Key)
		norm := SecretRequestKeyNorm(key)
		switch {
		case key == "":
			add(p+".key", DecisionErrRequired, "key is required")
		case !ValidSecretRequestKey(key):
			add(p+".key", DecisionErrInvalid, "key must match [A-Za-z_][A-Za-z0-9_]{0,%d}", MaxSecretRequestKeyLen-1)
		default:
			if prev, dup := seen[norm]; dup {
				add(p+".key", DecisionErrDuplicate, "key %q is also used by %s[%d] (keys compare case-insensitively)", key, root, prev)
			} else {
				seen[norm] = i
			}
		}

		secretType := strings.TrimSpace(item.Type)
		if secretType == "" {
			secretType = SecretRequestTypeEnvironment
		}
		switch secretType {
		case SecretRequestTypeEnvironment, SecretRequestTypeFile, SecretRequestTypeOnDemand:
		case SecretRequestTypeVariable:
			secretType = SecretRequestTypeOnDemand
		default:
			add(p+".type", DecisionErrInvalid, "type must be one of: environment, file, on_demand")
		}

		target := strings.TrimSpace(item.Target)
		if target != "" {
			switch {
			case !validPlainText(target):
				add(p+".target", DecisionErrInvalid, "target must be one line of plain text without control characters")
			case utf8.RuneCountInString(target) > MaxSecretRequestTargetLen:
				add(p+".target", DecisionErrTooLong, "target is at most %d characters", MaxSecretRequestTargetLen)
			}
		}
		if secretType == SecretRequestTypeFile {
			if target == "" {
				add(p+".target", DecisionErrRequired, "target is required when type is \"file\"")
			} else if err := ValidateSecretRequestFileTarget(target); err != nil {
				add(p+".target", DecisionErrInvalid, "%s", err.Error())
			}
		}
		if secretType == SecretRequestTypeEnvironment {
			effectiveTarget := target
			if effectiveTarget == "" {
				effectiveTarget = key
			}
			if effectiveTarget != "" && isReservedSecretRequestEnvTarget(effectiveTarget) {
				field := p + ".key"
				if target != "" {
					field = p + ".target"
				}
				add(field, DecisionErrNotAllowed, "%q is reserved for scion's own control-plane environment variables", effectiveTarget)
			}
		}

		desc := strings.TrimSpace(item.Description)
		switch {
		case desc == "":
		case !validQuestionText(desc):
			add(p+".description", DecisionErrInvalid, "description must be valid UTF-8 text without control characters (line breaks and tabs are allowed)")
		case utf8.RuneCountInString(desc) > MaxSecretRequestDescriptionLen:
			add(p+".description", DecisionErrTooLong, "description is at most %d characters", MaxSecretRequestDescriptionLen)
		}

		out = append(out, SecretRequest{
			Key:         key,
			KeyNorm:     norm,
			Description: desc,
			SecretType:  secretType,
			Target:      target,
		})
	}
	if len(errs) > 0 {
		return nil, errs
	}
	enc, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if len(enc) > MaxSecretRequestsPayloadBytes {
		add(root, DecisionErrTooLarge, "secret requests are %d bytes after normalization; the limit is %d", len(enc), MaxSecretRequestsPayloadBytes)
		return nil, errs
	}
	return out, nil
}

// SecretRequestSendResult is one secret request entry in the outbound-message
// send response.
type SecretRequestSendResult struct {
	Key      string                 `json:"key"`
	Version  int                    `json:"version"`
	RowID    string                 `json:"row_id"`
	Replaced *SecretRequestReplaced `json:"replaced"`
}

// SecretRequestReplaced describes the prior version replaced by a send.
type SecretRequestReplaced struct {
	Version int    `json:"version"`
	Status  string `json:"status"`
}

// SecretRequestReplacedBy names the newer version that replaced a request row.
type SecretRequestReplacedBy struct {
	RowID     string    `json:"rowId"`
	MessageID string    `json:"messageId"`
	Version   int       `json:"version"`
	At        time.Time `json:"at"`
}

// SecretRequestFulfilled describes who fulfilled a request and when (metadata only).
type SecretRequestFulfilled struct {
	At                time.Time `json:"at"`
	ByUserID          string    `json:"byUserId,omitempty"`
	ByName            string    `json:"byName"`
	ByEmail           string    `json:"byEmail,omitempty"`
	Surface           string    `json:"surface"`
	ResponseMessageID string    `json:"responseMessageId,omitempty"`
	SecretVersion     int       `json:"secretVersion,omitempty"`
}

// SecretRequestWithdrawn describes when, why, and by whom ("agent" or "hub") a
// secret request was withdrawn.
type SecretRequestWithdrawn struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
	By     string    `json:"by"`
	ByName string    `json:"byName,omitempty"`
}

// SecretRequestDismissed describes when, why, and by whom a secret request was
// dismissed.
type SecretRequestDismissed struct {
	At                time.Time `json:"at"`
	ByUserID          string    `json:"byUserId,omitempty"`
	ByName            string    `json:"byName"`
	ByEmail           string    `json:"byEmail,omitempty"`
	Reason            string    `json:"reason,omitempty"`
	Surface           string    `json:"surface"`
	ResponseMessageID string    `json:"responseMessageId,omitempty"`
}

// SecretRequestExpired describes when a secret request expired.
type SecretRequestExpired struct {
	At time.Time `json:"at"`
}

// SecretRequestView is one version of a secret request as clients see it.
// It never carries a secret value. When the stored status is "fulfilling",
// Status is reported as "pending" with Submitting=true.
type SecretRequestView struct {
	RowID          string                   `json:"rowId"`
	MessageID      string                   `json:"messageId"`
	ProjectID      string                   `json:"projectId"`
	AgentID        string                   `json:"agentId"`
	AgentName      string                   `json:"agentName"`
	Key            string                   `json:"key"`
	Version        int                      `json:"version"`
	Description    string                   `json:"description,omitempty"`
	SecretType     string                   `json:"secretType"`
	Target         string                   `json:"target,omitempty"`
	Position       int                      `json:"position"`
	Status         string                   `json:"status"`
	Submitting     bool                     `json:"submitting,omitempty"`
	ConversationID string                   `json:"conversationId,omitempty"`
	CreatedAt      time.Time                `json:"createdAt"`
	ExpiresAt      time.Time                `json:"expiresAt"`
	ResolvedAt     *time.Time               `json:"resolvedAt,omitempty"`
	SecretVersion  int                      `json:"secretVersion,omitempty"`
	ReplacedBy     *SecretRequestReplacedBy `json:"replacedBy,omitempty"`
	Fulfilled      *SecretRequestFulfilled  `json:"fulfilled,omitempty"`
	Withdrawn      *SecretRequestWithdrawn  `json:"withdrawn,omitempty"`
	Dismissed      *SecretRequestDismissed  `json:"dismissed,omitempty"`
	Expired        *SecretRequestExpired    `json:"expired,omitempty"`
}

// SecretRequestCardResponse is the 200 response of GET /api/v1/secret-requests/{messageId}.
type SecretRequestCardResponse struct {
	MessageID      string              `json:"messageId"`
	ProjectID      string              `json:"projectId"`
	ProjectName    string              `json:"projectName,omitempty"`
	AgentID        string              `json:"agentId"`
	AgentName      string              `json:"agentName"`
	ConversationID string              `json:"conversationId,omitempty"`
	ThreadID       string              `json:"threadId,omitempty"`
	Intro          string              `json:"intro,omitempty"`
	Rows           []SecretRequestView `json:"rows"`
	CanRespond     bool                `json:"canRespond"`
	Reason         string              `json:"reason,omitempty"`
}

// FulfillSecretRequestBody is the request body of
// POST /api/v1/secret-requests/{messageId}/fulfill.
// It implements slog.LogValuer so secret values can never reach structured logs.
type FulfillSecretRequestBody struct {
	Values         map[string]string `json:"values"`
	Surface        string            `json:"surface,omitempty"`
	IdempotencyKey string            `json:"idempotencyKey,omitempty"`
}

// LogValue redacts Values so secret material never appears in slog output.
func (b FulfillSecretRequestBody) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("values", "[REDACTED]"),
		slog.Int("key_count", len(b.Values)),
		slog.String("surface", b.Surface),
		slog.String("idempotency_key", b.IdempotencyKey),
	)
}

// FulfillSecretRequestResponse is the 200 response of
// POST /api/v1/secret-requests/{messageId}/fulfill. It never echoes values.
type FulfillSecretRequestResponse struct {
	Rows              []SecretRequestView `json:"rows"`
	ResponseMessageID string              `json:"responseMessageId,omitempty"`
	Replayed          bool                `json:"replayed,omitempty"`
}

// DismissSecretRequestBody is the request body of
// POST /api/v1/secret-requests/{messageId}/dismiss.
type DismissSecretRequestBody struct {
	RowIDs         []string `json:"rowIds,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	Surface        string   `json:"surface,omitempty"`
	IdempotencyKey string   `json:"idempotencyKey,omitempty"`
}

// DismissSecretRequestResponse is the 200 response of
// POST /api/v1/secret-requests/{messageId}/dismiss.
type DismissSecretRequestResponse struct {
	Dismissed         []SecretRequestView `json:"dismissed"`
	AlreadyDismissed  []SecretRequestView `json:"alreadyDismissed"`
	ResponseMessageID string              `json:"responseMessageId,omitempty"`
	Replayed          bool                `json:"replayed,omitempty"`
}

// WithdrawSecretRequestsRequest is the request body of
// POST /api/v1/agent/secret-requests/withdraw.
type WithdrawSecretRequestsRequest struct {
	Keys     []string `json:"keys"`
	WholeSet bool     `json:"wholeSet,omitempty"`
	Reason   string   `json:"reason,omitempty"`
}

// WithdrawSecretRequestsResponse is the 200 response of
// POST /api/v1/agent/secret-requests/withdraw.
type WithdrawSecretRequestsResponse struct {
	Withdrawn        []SecretRequestView `json:"withdrawn"`
	AlreadyWithdrawn []SecretRequestView `json:"alreadyWithdrawn"`
}

// AgentSecretRequestListResponse is the 200 response of
// GET /api/v1/agent/secret-requests.
type AgentSecretRequestListResponse struct {
	Items      []SecretRequestView `json:"items"`
	NextCursor string              `json:"nextCursor,omitempty"`
}

// SecretRequestUpdatedEvent is the payload published on
// user.<id>.chat.secret_request.updated and project.<id>.chat.secret_request.updated.
type SecretRequestUpdatedEvent struct {
	SecretRequest SecretRequestView `json:"secret_request"`
	MessageID     string            `json:"messageId"`
	ThreadID      string            `json:"threadId"`
}

// SecretRequestFallbackItem is one requested key rendered into the Markdown
// fallback body of a secret-request message.
type SecretRequestFallbackItem struct {
	Key         string
	Version     int
	Description string
	SecretType  string
	Target      string
}

// RenderSecretRequestFallback renders the Markdown fallback body stored on a
// secret-request message so non-native surfaces display the request clearly
// and direct the user to the standalone Hub page (never to reply in chat).
func RenderSecretRequestFallback(intro, agentName, projectName string, items []SecretRequestFallbackItem, standaloneURL string) string {
	var b strings.Builder
	intro = strings.TrimSpace(intro)
	if intro != "" {
		b.WriteString(intro)
		b.WriteString("\n\n---\n\n")
	}
	header := "**Secret request**"
	switch {
	case agentName != "" && projectName != "":
		header = fmt.Sprintf("**Secret request** from **%s** in project **%s**", escapeMarkdownText(agentName), escapeMarkdownText(projectName))
	case agentName != "":
		header = fmt.Sprintf("**Secret request** from **%s**", escapeMarkdownText(agentName))
	}
	b.WriteString(header)
	b.WriteString("\n\n")
	for _, it := range items {
		b.WriteString("- `")
		b.WriteString(it.Key)
		b.WriteString("`")
		if it.Version > 0 {
			fmt.Fprintf(&b, " (v%d)", it.Version)
		}
		if it.SecretType != "" && it.SecretType != SecretRequestTypeEnvironment {
			b.WriteString(" [")
			b.WriteString(it.SecretType)
			if it.Target != "" {
				b.WriteString(": `")
				b.WriteString(escapeMarkdownText(it.Target))
				b.WriteString("`")
			}
			b.WriteString("]")
		}
		if desc := strings.TrimSpace(it.Description); desc != "" {
			b.WriteString(" — ")
			b.WriteString(escapeMarkdownText(desc))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	if standaloneURL != "" {
		fmt.Fprintf(&b, "Provide this secret in the Hub (sign-in required): %s\n\n", standaloneURL)
	}
	b.WriteString("Do not paste secret values into chat; values are stored directly in the Hub and readable only by the requesting agent.")
	return b.String()
}

// escapeMarkdownText neutralizes broadcast/bracket mentions and inline markdown
// specials in fallback text.
func escapeMarkdownText(s string) string {
	s = broadcastMentionRe.ReplaceAllString(s, "${1}@"+zeroWidthSpace+"${2}")
	s = bracketMentionRe.ReplaceAllString(s, "<"+zeroWidthSpace+"${1}")
	return s
}
