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
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// brokerReturnedErrorMarker is the text the Hub puts in front of a runtime
// broker's raw error body when it relays a failed create or start as a 502
// runtime_error ("Failed to dispatch to runtime broker: runtime broker
// returned error <status>: <body>"). The Hub does not copy the broker's
// error code or details into structured fields, so the CLI reads them back
// out of the body.
const brokerReturnedErrorMarker = "runtime broker returned error "

// brokerFailure is a runtime broker error recovered from a Hub response.
type brokerFailure struct {
	Status  int
	Code    string
	Details map[string]interface{}
}

// parseBrokerFailure extracts the broker's status, error code and details
// from a Hub API error that relays a broker failure. ok is false when err
// does not carry one.
func parseBrokerFailure(err error) (apiErr *apiclient.APIError, bf brokerFailure, ok bool) {
	if !errors.As(err, &apiErr) {
		return nil, bf, false
	}
	i := strings.Index(apiErr.Message, brokerReturnedErrorMarker)
	if i < 0 {
		return nil, bf, false
	}
	rest := apiErr.Message[i+len(brokerReturnedErrorMarker):]
	statusText, body, found := strings.Cut(rest, ":")
	if !found {
		return nil, bf, false
	}
	status, convErr := strconv.Atoi(strings.TrimSpace(statusText))
	if convErr != nil {
		return nil, bf, false
	}
	bf.Status = status
	var parsed struct {
		Error struct {
			Code    string                 `json:"code"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(body)), &parsed) == nil {
		bf.Code = parsed.Error.Code
		bf.Details = parsed.Error.Details
	}
	return apiErr, bf, true
}

// brokerFailureCategory is the short, user-facing description of a broker
// failure. It is built from the broker's error code and structured details
// only, never from the broker's free-text message, which can carry internal
// errors.
func brokerFailureCategory(bf brokerFailure) string {
	detail := func(key string) string {
		if v, ok := bf.Details[key].(string); ok {
			return v
		}
		return ""
	}
	switch bf.Code {
	case "template_error":
		if detail("cause") == "file_not_found" {
			msg := "a template or harness-config file is missing on the runtime broker"
			if f := detail("file"); f != "" {
				msg += fmt.Sprintf(" (file %q)", f)
			}
			return msg + "; re-sync the template or harness-config to the Hub"
		}
		return "the runtime broker could not prepare the agent's template"
	case "runtime_error":
		return "the runtime broker could not create or start the agent container (for example, an image pull or container runtime failure)"
	case "runtime_unavailable":
		return "the container runtime is not available on the runtime broker; retry later"
	case "hub_unreachable":
		return "the runtime broker could not reach the Hub"
	case "nfs_unavailable":
		return "shared storage is not available on the runtime broker"
	case "workspace_storage_unconfigured":
		return "workspace storage is not configured for the runtime broker"
	case "conflict":
		return "the agent is in a conflicting state on the runtime broker"
	case "":
		return fmt.Sprintf("the runtime broker returned an unrecognized error (status %d)", bf.Status)
	default:
		return fmt.Sprintf("the runtime broker reported %s", bf.Code)
	}
}

// renderBrokerFailure returns err's message with the relayed broker error
// replaced by a short failure category, plus the codes and request ID
// needed to find the full cause in the broker and Hub logs. ok is false
// when err does not relay a broker failure.
func renderBrokerFailure(err error) (string, bool) {
	apiErr, bf, ok := parseBrokerFailure(err)
	if !ok {
		return "", false
	}
	ref := []string{}
	if bf.Code != "" {
		ref = append(ref, "broker code: "+bf.Code)
	}
	ref = append(ref, fmt.Sprintf("broker status: %d", bf.Status))
	if apiErr.RequestID != "" {
		ref = append(ref, "request: "+apiErr.RequestID)
	}
	summary := fmt.Sprintf("runtime broker failure: %s (%s)", brokerFailureCategory(bf), strings.Join(ref, ", "))
	full := err.Error()
	if raw := apiErr.Error(); strings.Contains(full, raw) {
		return strings.Replace(full, raw, summary, 1), true
	}
	return summary, true
}
