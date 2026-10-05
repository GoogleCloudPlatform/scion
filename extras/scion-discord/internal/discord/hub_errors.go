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

package discord

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// staleLinkText is shown when the hub no longer accepts the Scion account a
// Discord user is linked to, or the link has no email.
const staleLinkText = "Your linked Scion account is no longer active. Run `/scion unregister`, then `/scion register`."

// agentListUnavailableText is shown when the agent list could not be
// fetched and there is nothing more specific to say.
const agentListUnavailableText = "Couldn't fetch the agent list for this project. Please try again later."

// HubError is a non-OK response from a hub read or write made for a linked
// user.
type HubError struct {
	// Op names the client operation, e.g. "list agents".
	Op         string
	StatusCode int
	// Code, Message and Details come from the hub's JSON error envelope
	// when present.
	Code    string
	Message string
	Details map[string]interface{}
}

func (e *HubError) Error() string {
	msg := fmt.Sprintf("%s returned status %d", e.Op, e.StatusCode)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// detail returns a string detail value, or "".
func (e *HubError) detail(key string) string {
	if v, ok := e.Details[key].(string); ok {
		return v
	}
	return ""
}

// isStaleLink reports whether the hub rejected the linked user itself
// (unknown or not active), as opposed to denying a specific action.
// It matches on the message text because the hub sends no distinct error
// code for this case: both use code "forbidden".
func (e *HubError) isStaleLink() bool {
	return e.StatusCode == http.StatusForbidden && strings.HasPrefix(e.Message, "on-behalf-of principal")
}

// isForbidden reports whether the hub denied the request.
func (e *HubError) isForbidden() bool {
	return e.StatusCode == http.StatusForbidden
}

// newHubError builds a HubError from a non-OK response, decoding the hub's
// JSON error envelope when present.
func newHubError(op string, resp *http.Response) *HubError {
	he := &HubError{Op: op, StatusCode: resp.StatusCode}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil || len(body) == 0 {
		return he
	}
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

// isForbiddenHubError reports whether err is a hub 403 of any kind.
func isForbiddenHubError(err error) bool {
	var he *HubError
	return errors.As(err, &he) && he.isForbidden()
}

// isStaleLinkError reports whether err means the linked account is no
// longer accepted.
func isStaleLinkError(err error) bool {
	var he *HubError
	return errors.As(err, &he) && he.isStaleLink()
}

// hubErrorText returns actionable text for a failed hub request made as the
// linked user email about project (a slug, or "" when unknown), or fallback
// when there is nothing more specific to say.
func hubErrorText(err error, email, project, fallback string) string {
	var he *HubError
	if !errors.As(err, &he) {
		return fallback
	}
	if he.isStaleLink() {
		return staleLinkText
	}
	if he.isForbidden() && he.Code == "forbidden" {
		if action := actionPhrase(he.detail("denied_action"), he.detail("resource_type")); action != "" {
			return permissionDeniedText(email, action, project)
		}
	}
	return fallback
}

// deniedText returns actionable text for a failed hub request that names
// action (e.g. "create agents") when the hub denies it without saying
// which action was denied. Other errors get fallback.
func deniedText(err error, email, project, action, fallback string) string {
	text := hubErrorText(err, email, project, "")
	switch {
	case text != "":
		return text
	case isForbiddenHubError(err):
		return permissionDeniedText(email, action, project)
	default:
		return fallback
	}
}

// permissionDeniedText tells the user their account may not perform action
// in project.
func permissionDeniedText(email, action, project string) string {
	account := "Your Scion account"
	if email != "" {
		account += " (" + email + ")"
	}
	return fmt.Sprintf("%s doesn't have permission to %s in %s. Ask a project owner.", account, action, projectLabel(project))
}

// projectLabel formats a project slug for a reply, or "this hub" when empty.
func projectLabel(project string) string {
	if project == "" {
		return "this hub"
	}
	return "**" + project + "**"
}

// emailFromPrincipal returns the email of a "user:<email>" principal, or "".
func emailFromPrincipal(principal string) string {
	return strings.TrimPrefix(principal, "user:")
}

// actionPhrase turns a denied action and resource type into readable text,
// e.g. ("list", "agent") -> "list agents". It returns "" when action is
// empty.
func actionPhrase(action, resourceType string) string {
	action = strings.TrimSpace(action)
	if action == "" {
		return ""
	}
	verb := action
	switch action {
	case "read":
		verb = "view"
	case "port_access":
		verb = "access ports of"
	case "stop_all":
		verb = "stop all"
	}
	resourceType = strings.ReplaceAll(strings.TrimSpace(resourceType), "_", " ")
	if resourceType == "" {
		return verb
	}
	return verb + " " + pluralize(resourceType)
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
