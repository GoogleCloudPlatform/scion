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

package agent

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// detectCloudHost is the cloud-host detector used for block-mode starts
// under host networking. Tests replace it.
var detectCloudHost = func() runtime.CloudHost { return runtime.DetectCloudHost("/") }

// hostNetworkBlockPolicy decides a metadata block-mode start under host
// networking, where the block cannot be enforced. It applies the broker's
// SCION_HOST_NETWORK_BLOCK_POLICY setting and local cloud-host detection.
// When the start may proceed it returns the user-visible warning (warning
// unchanged, or extended with the operator acknowledgement); when the start
// is refused it returns an error naming the fix.
func hostNetworkBlockPolicy(agentName, warning string) (string, error) {
	policy, known := runtime.ParseHostNetworkBlockPolicy(os.Getenv(runtime.HostNetworkBlockPolicyEnvVar))
	if !known {
		slog.Error("unrecognized host network block policy; treating it as strict",
			"setting", runtime.HostNetworkBlockPolicyEnvVar, "agent", agentName)
	}

	var cloud runtime.CloudHost
	if policy != runtime.HostNetworkBlockPolicyStrict {
		cloud = detectCloudHost()
	}

	switch runtime.DecideHostNetworkBlock(policy, cloud) {
	case runtime.HostNetworkBlockWarn:
		return warning, nil
	case runtime.HostNetworkBlockAllowAcknowledged:
		slog.Warn("Start: GCP metadata block mode under host networking on a detected cloud host is allowed by broker policy; the block is not enforced",
			"agent", agentName, "setting", runtime.HostNetworkBlockPolicyEnvVar, "detection", cloud.Source)
		return warning + "; this broker runs on a detected cloud host and the start was allowed by the broker setting " +
			runtime.HostNetworkBlockPolicyEnvVar, nil
	}

	if policy == runtime.HostNetworkBlockPolicyStrict {
		return "", &hostNetworkRefusal{reason: hostNetworkRefusalStrict,
			msg: fmt.Sprintf("cannot start agent %q: GCP metadata mode \"block\" is not enforced under host networking, and the broker setting %s refuses such starts; %s",
				agentName, runtime.HostNetworkBlockPolicyEnvVar, hostNetworkMetadataFix)}
	}
	return "", &hostNetworkRefusal{reason: hostNetworkRefusalCloudHost,
		msg: fmt.Sprintf("cannot start agent %q: GCP metadata mode \"block\" is not enforced under host networking, and this broker runs on a detected cloud host; %s, or set %s=allow on the broker to start anyway",
			agentName, hostNetworkMetadataFix, runtime.HostNetworkBlockPolicyEnvVar)}
}

// Refusal reasons for a start refused by checkHostNetworkMetadata. They are
// fixed log keys and name no setting values.
const (
	hostNetworkRefusalAssignUnsupported = "assign_unsupported"
	hostNetworkRefusalCloudHost         = "cloud_host"
	hostNetworkRefusalStrict            = "strict"
)

// hostNetworkRefusal is the error returned when checkHostNetworkMetadata
// refuses a start; reason is one of the hostNetworkRefusal* keys.
type hostNetworkRefusal struct {
	reason string
	msg    string
}

func (e *hostNetworkRefusal) Error() string { return e.msg }

// hostNetworkRefusalReason returns the refusal reason key of err, or
// "unknown" when err is not a host network refusal.
func hostNetworkRefusalReason(err error) string {
	var r *hostNetworkRefusal
	if errors.As(err, &r) {
		return r.reason
	}
	return "unknown"
}
