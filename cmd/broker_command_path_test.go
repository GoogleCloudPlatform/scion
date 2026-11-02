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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
)

// The command path is read only from the control_channel check, and only
// fixed words come out: a raw or unexpected value is not reported.
func TestBrokerReportedCommandPath(t *testing.T) {
	assert.Equal(t, commandPathNotReported, brokerReportedCommandPath(nil), "no report")
	for value, want := range map[string]string{
		"healthy":   commandPathYes,
		"unhealthy": commandPathNo,
		"unknown":   commandPathNotReported, // a foreign value the Hub normalised
		"":          commandPathNotReported,
		"degraded":  commandPathNotReported,
		"unhealthy: dial tcp 10.0.0.1:443: refused": commandPathNotReported,
	} {
		got := brokerReportedCommandPath(&api.BrokerHealthReport{Checks: map[string]string{"control_channel": value}})
		assert.Equal(t, want, got, "control_channel %q", value)
	}
	assert.Equal(t, commandPathNotReported, brokerReportedCommandPath(&api.BrokerHealthReport{Checks: map[string]string{"docker": "available"}}),
		"an older broker that does not report the check")

	assert.Equal(t, "yes (control channel connected)", brokerCommandPathText(commandPathYes))
	assert.Contains(t, brokerCommandPathText(commandPathNo), "cannot receive commands")
	assert.Equal(t, "pending (control channel connecting)", brokerCommandPathText(commandPathPending))
	assert.Equal(t, "not reported", brokerCommandPathText(commandPathNotReported))
	assert.Equal(t, "not reported", brokerCommandPathText("anything else"))
}

// A hub connection's Commands line comes from the running broker's live
// fixed words; a transport refusal names the hub setting, and a connection
// without a control channel prints no line.
func TestBrokerConnectionCommandsText(t *testing.T) {
	assert.Equal(t, "", brokerConnectionCommandsText(BrokerHubConnectionInfo{}), "no control channel")
	assert.Equal(t, "yes (control channel connected)",
		brokerConnectionCommandsText(BrokerHubConnectionInfo{HasControlChannel: true, CommandPath: "healthy"}))
	assert.Equal(t, "pending (control channel connecting)",
		brokerConnectionCommandsText(BrokerHubConnectionInfo{HasControlChannel: true, CommandPath: "unknown"}))
	assert.Contains(t, brokerConnectionCommandsText(BrokerHubConnectionInfo{HasControlChannel: true, CommandPath: "unhealthy"}),
		"cannot receive commands")
	refused := brokerConnectionCommandsText(BrokerHubConnectionInfo{HasControlChannel: true, CommandPath: "unhealthy", TransportRefused: true})
	assert.Contains(t, refused, "transport policy")
	assert.Contains(t, refused, "server.trusted_proxies")
	assert.Equal(t, "not reported",
		brokerConnectionCommandsText(BrokerHubConnectionInfo{HasControlChannel: true}), "an older broker without the field")
	assert.Equal(t, "not reported",
		brokerConnectionCommandsText(BrokerHubConnectionInfo{HasControlChannel: true, CommandPath: "<raw text>"}), "a value outside the fixed set")
}

// runtime-broker status shows the stored command path only for an online
// broker: an offline broker's last report is stale, so no Commands line is
// printed and the JSON field is empty, whatever the report said.
func TestBrokerStatusCommandPath_OnlyWhenOnline(t *testing.T) {
	healthy := &api.BrokerHealthReport{Status: "healthy", Checks: map[string]string{"control_channel": "healthy"}}
	unhealthy := &api.BrokerHealthReport{Status: "degraded", Checks: map[string]string{"control_channel": "unhealthy"}}

	for _, status := range []string{"offline", "degraded", ""} {
		assert.Equal(t, "", brokerStatusCommandPath(status, healthy), "status %q", status)
		assert.Equal(t, "", brokerStatusCommandPath(status, unhealthy), "status %q", status)
	}
	assert.Equal(t, "", brokerCommandsLine(brokerStatusCommandPath("offline", healthy)), "no line for an offline broker")

	assert.Equal(t, commandPathYes, brokerStatusCommandPath("online", healthy))
	assert.Equal(t, commandPathNo, brokerStatusCommandPath("online", unhealthy))
	assert.Equal(t, commandPathNotReported, brokerStatusCommandPath("online", nil))
	assert.Equal(t, "  Commands:    yes (control channel connected)\n", brokerCommandsLine(brokerStatusCommandPath("online", healthy)))
	assert.Equal(t, "  Commands:    not reported\n", brokerCommandsLine(brokerStatusCommandPath("online", nil)))
}
