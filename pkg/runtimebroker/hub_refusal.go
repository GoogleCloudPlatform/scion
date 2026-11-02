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

package runtimebroker

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// hubTransportRefusalCode is the error code of a hub (or conduit relay)
// that refused a broker request because it did not arrive over TLS, from
// the hub's host, or through a proxy the hub trusts.
const hubTransportRefusalCode = "transport_not_confidential"

// hubRefusalReasonUnspecified is the reason of a transport refusal whose
// body names none (a hub from before refusal details).
const hubRefusalReasonUnspecified = "unspecified"

// hubRefusalBodyLimit caps how much of a refusal body is read.
const hubRefusalBodyLimit = 4096

// hubRefusalError is a hub's JSON error answer to a broker's upgrade
// request: the HTTP status, the error code and message, and for a
// transport refusal the reason and the hub setting that admits a proxy.
type hubRefusalError struct {
	Status  int
	Code    string
	Message string
	Reason  string
	Setting string
}

func (e *hubRefusalError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("hub refused the request (status %d, %s, reason %s): %s", e.Status, e.Code, e.Reason, e.Message)
	}
	return fmt.Sprintf("hub refused the request (status %d, %s): %s", e.Status, e.Code, e.Message)
}

// transportPolicy reports whether e is the hub's transport refusal.
func (e *hubRefusalError) transportPolicy() bool {
	return e != nil && e.Code == hubTransportRefusalCode
}

// logMessage is the broker's line for a transport refusal of channel
// ("control channel", "conduit"): it names the reason and the hub setting.
// The message text holds only fixed strings: the reason when it is one the
// broker knows (knownRefusalReason), else hubRefusalReasonUnspecified, and
// config.TrustedProxiesKey. The hub's raw reason and setting go in the log
// attributes only.
func (e *hubRefusalError) logMessage(channel string) string {
	return "Hub refused the " + channel + ": the hub requires TLS or a trusted proxy (reason " + knownRefusalReason(e.Reason) +
		"). If the hub is behind a TLS-terminating proxy, set " + config.TrustedProxiesKey + " on the hub and restart it."
}

// knownRefusalReason returns reason when it is one of the transport
// refusal reasons (config.ArrivalRefusal), else hubRefusalReasonUnspecified.
func knownRefusalReason(reason string) string {
	switch reason {
	case config.ArrivalRefusalUntrustedLocalProxy, config.ArrivalRefusalLocalProxyNotTrusted,
		config.ArrivalRefusalProxyProtoNotHTTPS, config.ArrivalRefusalNotTLS:
		return reason
	}
	return hubRefusalReasonUnspecified
}

// parseHubRefusal parses a hub's JSON error body ({"error": {"code",
// "message", "details"}}) for status. It returns nil when body is not such
// an error. For a transport refusal without details (an older hub), the
// reason is hubRefusalReasonUnspecified and the setting is
// config.TrustedProxiesKey, which the older hub's message names.
func parseHubRefusal(status int, body []byte) *hubRefusalError {
	if len(body) > hubRefusalBodyLimit {
		body = body[:hubRefusalBodyLimit]
	}
	var resp struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Error.Code == "" {
		return nil
	}
	e := &hubRefusalError{
		Status:  status,
		Code:    resp.Error.Code,
		Message: strings.TrimSpace(resp.Error.Message),
	}
	if s, ok := resp.Error.Details["reason"].(string); ok {
		e.Reason = s
	}
	if s, ok := resp.Error.Details["setting"].(string); ok {
		e.Setting = s
	}
	if e.transportPolicy() {
		if e.Reason == "" {
			e.Reason = hubRefusalReasonUnspecified
		}
		if e.Setting == "" {
			e.Setting = config.TrustedProxiesKey
		}
	}
	return e
}
