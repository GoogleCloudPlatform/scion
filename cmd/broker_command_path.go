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
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Command path words: whether the Hub can send a broker commands (over its
// control channel). Only these fixed words are printed or put in JSON,
// never a value read from a broker or the Hub.
const (
	commandPathYes         = "yes"
	commandPathNo          = "no"
	commandPathPending     = "pending"
	commandPathNotReported = "not_reported"
)

// brokerReportedCommandPath maps the broker's stored self-report (its
// control_channel check) to a command path word: healthy is yes, unhealthy
// no. Anything else is not_reported: no report (an older broker or Hub), no
// check (the broker is starting or reconnecting within its grace, or runs
// no control channel), or unknown, which the Hub stores for a value outside
// the fixed set and so says nothing about the channel.
func brokerReportedCommandPath(h *api.BrokerHealthReport) string {
	if h == nil {
		return commandPathNotReported
	}
	switch h.Checks["control_channel"] {
	case api.BrokerHealthHealthy:
		return commandPathYes
	case api.BrokerHealthUnhealthy:
		return commandPathNo
	default:
		return commandPathNotReported
	}
}

// brokerStatusCommandPath is the command path word runtime-broker status
// shows for a broker with liveness status and stored report h: "" while the
// broker is not online (its last report is stale; the health summary and
// doctor ignore it too), else brokerReportedCommandPath.
func brokerStatusCommandPath(status string, h *api.BrokerHealthReport) string {
	if status != "online" {
		return ""
	}
	return brokerReportedCommandPath(h)
}

// brokerCommandsLine is the "Commands:" line runtime-broker status prints
// for command path word, or "" (no line) for an empty word.
func brokerCommandsLine(word string) string {
	if word == "" {
		return ""
	}
	return "  Commands:    " + brokerCommandPathText(word) + "\n"
}

// printBrokerCommandsLine prints brokerCommandsLine(word).
func printBrokerCommandsLine(word string) {
	fmt.Print(brokerCommandsLine(word))
}

// brokerCommandPathText is the "Commands:" line for a command path word.
func brokerCommandPathText(word string) string {
	switch word {
	case commandPathYes:
		return "yes (control channel connected)"
	case commandPathNo:
		return "no (the broker reports its control channel to the Hub is not connected; it is online but cannot receive commands)"
	case commandPathPending:
		return "pending (control channel connecting)"
	default:
		return "not reported"
	}
}

// brokerConnectionCommandsText is the "Commands:" line of one hub
// connection, from the running broker's live answer, or "" when the
// connection runs no control channel. A transport refusal names the hub
// setting to check.
func brokerConnectionCommandsText(info BrokerHubConnectionInfo) string {
	if !info.HasControlChannel {
		return ""
	}
	var word string
	switch info.CommandPath {
	case api.BrokerHealthHealthy:
		word = commandPathYes
	case api.BrokerHealthUnhealthy:
		word = commandPathNo
	case api.BrokerHealthUnknown:
		word = commandPathPending
	default:
		return brokerCommandPathText(commandPathNotReported)
	}
	if word == commandPathNo && info.TransportRefused {
		return "no (the Hub refused the control channel under its transport policy; check server.trusted_proxies on the Hub)"
	}
	return brokerCommandPathText(word)
}
