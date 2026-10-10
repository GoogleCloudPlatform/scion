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

package telegram

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// staleLinkText is shown when the hub no longer accepts the Scion account a
// Telegram user is linked to.
const staleLinkText = "Your linked Scion account is no longer active. Run /unregister, then /register."

// HubError is a non-OK response from the hub API.
type HubError struct {
	// Op names the client operation, e.g. "list agents".
	Op         string
	StatusCode int
	Code       string
	Message    string
	Details    map[string]interface{}
}

func (e *HubError) Error() string {
	msg := fmt.Sprintf("%s returned status %d", e.Op, e.StatusCode)
	if e.Code != "" {
		msg += ": " + e.Code
	}
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

// hubErrorText returns actionable text for a hub error on behalf of the
// linked user email in project, or fallback when there is nothing more
// specific to say.
func hubErrorText(err error, email, project, fallback string) string {
	var he *HubError
	if !errors.As(err, &he) {
		return fallback
	}
	if he.isStaleLink() {
		return staleLinkText
	}
	if he.isForbidden() && he.Code == "forbidden" {
		if action := deniedActionPhrase(he.detail("denied_action"), he.detail("resource_type")); action != "" {
			if project == "" {
				project = "this project"
			}
			account := "Your Scion account"
			if email != "" {
				account = fmt.Sprintf("Your Scion account (%s)", email)
			}
			return fmt.Sprintf("%s doesn't have permission to %s in %s. Ask a project owner.", account, action, project)
		}
	}
	return fallback
}

// deniedActionPhrase turns a denied action and resource type into a short
// phrase such as "list agents". It returns "" when action is empty.
func deniedActionPhrase(action, resourceType string) string {
	action = strings.TrimSpace(action)
	if action == "" {
		return ""
	}
	resourceType = strings.ReplaceAll(strings.TrimSpace(resourceType), "_", " ")
	if resourceType == "" {
		return action
	}
	return action + " " + pluralize(resourceType)
}

func pluralize(noun string) string {
	switch {
	case strings.HasSuffix(noun, "s"):
		return noun
	case strings.HasSuffix(noun, "y") && !strings.HasSuffix(noun, "ey"):
		return strings.TrimSuffix(noun, "y") + "ies"
	default:
		return noun + "s"
	}
}
