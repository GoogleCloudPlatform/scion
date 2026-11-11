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

package teams

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// HubError is a non-2xx response from the hub API.
type HubError struct {
	// Op names the request, e.g. "list agents".
	Op         string
	StatusCode int
	// Code, Message and Details come from the hub's JSON error envelope
	// when present.
	Code    string
	Message string
	Details map[string]interface{}
	// Body is the raw (size-limited) response body.
	Body string
}

func (e *HubError) Error() string {
	return fmt.Sprintf("%s returned status %d: %s", e.Op, e.StatusCode, e.Body)
}

// readHubError builds a HubError from a non-2xx response.
func readHubError(op string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	he := &HubError{Op: op, StatusCode: resp.StatusCode, Body: string(body)}
	var envelope struct {
		Error struct {
			Code    string                 `json:"code"`
			Message string                 `json:"message"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		he.Code = envelope.Error.Code
		he.Message = envelope.Error.Message
		he.Details = envelope.Error.Details
	}
	return he
}

// staleLinkText is shown when the linked Scion account can no longer be used.
const staleLinkText = "Your linked Scion account is no longer active. Run `unregister`, then `register`."

// staleLinkMessages are hub error messages meaning the linked account is
// unknown or inactive.
var staleLinkMessages = []string{
	"on-behalf-of principal not found",
	"on-behalf-of principal is not active",
	"sender identity could not be resolved",
	"sender identity is not active",
}

// userFacingHubError returns actionable text for a hub permission or
// stale-link error. email is the linked Scion account and project the
// project slug the request was about. It returns false for other errors.
func userFacingHubError(err error, email, project string) (string, bool) {
	var he *HubError
	if !errors.As(err, &he) || he.StatusCode != http.StatusForbidden {
		return "", false
	}
	for _, m := range staleLinkMessages {
		if strings.Contains(he.Message, m) {
			return staleLinkText, true
		}
	}
	project = projectLabel(project)
	switch he.Code {
	case "forbidden":
		action, _ := he.Details["denied_action"].(string)
		if action == "" {
			return "", false
		}
		resourceType, _ := he.Details["resource_type"].(string)
		return permissionDeniedText(email, actionPhrase(action, resourceType), project), true
	case "message_denied":
		return permissionDeniedText(email, "message agents", project), true
	}
	return "", false
}

func permissionDeniedText(email, action, project string) string {
	account := "Your Scion account"
	if email != "" {
		account += " (" + email + ")"
	}
	return fmt.Sprintf("%s doesn't have permission to %s in %s. Ask a project owner.", account, action, project)
}

// projectLabel formats a project slug for a reply, or "this hub" when empty.
func projectLabel(project string) string {
	if project == "" {
		return "this hub"
	}
	return "**" + project + "**"
}

// inboundFailureText returns the reply for a failed inbound delivery to
// agentSlug in project.
func inboundFailureText(err error, mapping *TeamsUserMapping, project, agentSlug string) string {
	var he *HubError
	if errors.As(err, &he) && he.StatusCode == http.StatusNotFound && he.Code == "agent_not_found" {
		return fmt.Sprintf("Agent **%s** was not found in %s. Use `agents` to see available agents.", agentSlug, projectLabel(project))
	}
	return hubErrorText(err, mapping, project, inboundDeliveryFailureText(agentSlug))
}

// actionPhrase turns a denied action and resource type into readable text,
// e.g. ("list", "agent") -> "list agents".
func actionPhrase(action, resourceType string) string {
	verb := action
	switch action {
	case "read":
		verb = "view"
	case "port_access":
		verb = "access ports of"
	case "stop_all":
		verb = "stop all"
	}
	if resourceType == "" {
		return verb
	}
	return verb + " " + pluralize(strings.ReplaceAll(resourceType, "_", " "))
}

// pluralize returns the plural of a resource noun, e.g. "agent" -> "agents",
// "policy" -> "policies".
func pluralize(noun string) string {
	switch {
	case strings.HasSuffix(noun, "s"):
		return noun
	case len(noun) > 1 && strings.HasSuffix(noun, "y") && !strings.ContainsAny(noun[len(noun)-2:len(noun)-1], "aeiou"):
		return noun[:len(noun)-1] + "ies"
	}
	return noun + "s"
}

// hubErrorText returns actionable text for err when it is a permission or
// stale-link error, and fallback otherwise.
func hubErrorText(err error, mapping *TeamsUserMapping, project, fallback string) string {
	email := ""
	if mapping != nil {
		email = mapping.ScionEmail
	}
	if text, ok := userFacingHubError(err, email, project); ok {
		return text
	}
	return fallback
}
