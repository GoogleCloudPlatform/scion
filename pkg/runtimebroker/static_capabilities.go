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
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// StaticCapabilities are the capabilities this broker binary reports for a
// default runtime rt. They are fixed by the binary and the runtime, so a
// broker sends them at registration (StaticCapabilityNames, on the
// registration and join) as well as on every heartbeat: the Hub then knows
// them before the first heartbeat and does not refuse a create with a
// spurious 412 in between (ptone/scion#2918).
//
// StartsInFlight is set only when the heartbeat actually carries the
// starts-in-flight list, so it is not part of this static set.
//
// nodeImageRegistry is the broker's SCION_IMAGE_REGISTRY
// (config.ImageRegistryFromEnv); it is a node configuration value carried
// with the capabilities, not a feature flag (ptone/scion#3916).
func StaticCapabilities(rt scionrt.Runtime, nodeImageRegistry string) *hubclient.BrokerCapabilities {
	return &hubclient.BrokerCapabilities{
		NodeImageRegistry:      nodeImageRegistry,
		WebPTY:                 webPTYSupported(rt),
		Sync:                   true,
		Attach:                 scionrt.HasAttachSupport(rt),
		Reprovision:            true,
		AsyncLaunch:            true,
		AsyncStart:             true, // async start of existing agents (ptone/scion#4669)
		EmptyPerAgentWorkspace: scionrt.HasEmptyPerAgentSupport(rt),
		// This broker honours localOnly deletes and confirms a moved
		// agent's NFS workspace before provisioning it (agent move).
		AgentMove: true,
		// This broker can be the target of a forced move: it confirms it
		// has no files for the agent before provisioning it with a fresh
		// workspace (ptone/scion#5441).
		AgentMoveRecreate: true,
		// This broker's reprovision reuses an empty-per-agent workspace in
		// place (miller79/scion#167).
		ReprovisionEmptyPerAgent: true,
	}
}

// webPTYSupported reports whether this broker's web terminal (the WebSocket
// PTY attach in pty_handlers.go, reached directly or over the control
// channel) can open a session on agents of runtime rt. It is the single
// source for the WebPTY capability, used by both StaticCapabilities and the
// info handler.
//
// It is an allow-list of the runtime implementations the PTY path is known
// to serve, so a runtime added later reports false until it is added here:
//   - docker, podman and the Apple container runtime: "<name> exec -it ...
//     tmux attach-session" under a local PTY (startDockerExec), the same
//     command each runtime's own Attach runs;
//   - kubernetes: the client-go SPDY exec bridged to the WebSocket
//     (runK8sExec);
//   - cloudrun-sandbox: the sandbox binary's exec under a local PTY
//     (startCloudRunSandboxExec).
//
// Everything else reports false, including a nil rt, a degraded
// *ErrorRuntime placeholder, cloudrun (its instances are reached through
// the Cloud Run exec connector, which the PTY path does not use) and
// substrate (no attach primitive: the direct-connect
// handler refuses it with a 501 before the WebSocket upgrade, and the
// control-channel handler closes the stream before starting an exec
// session). The type check is on the runtime implementation itself, so
// profile aliases (k8s, remote, local) resolve to the same answer as the
// runtime they construct.
func webPTYSupported(rt scionrt.Runtime) bool {
	switch rt.(type) {
	case *scionrt.DockerRuntime, *scionrt.PodmanRuntime, *scionrt.AppleContainerRuntime,
		*scionrt.KubernetesRuntime, *scionrt.CloudRunSandboxRuntime:
		// None of these opts out of attach today; the check keeps the
		// answer right in case a listed runtime later does.
		return scionrt.HasAttachSupport(rt)
	default:
		return false
	}
}

// StaticCapabilityNames is StaticCapabilities as the capability names the
// registration and join requests carry (the names the Hub's
// capabilitiesFromStrings accepts).
func StaticCapabilityNames(rt scionrt.Runtime) []string {
	c := StaticCapabilities(rt, "")
	var names []string
	add := func(on bool, name string) {
		if on {
			names = append(names, name)
		}
	}
	add(c.WebPTY, "webPty")
	add(c.Sync, "sync")
	add(c.Attach, "attach")
	add(c.Reprovision, "reprovision")
	add(c.AsyncLaunch, "asyncLaunch")
	add(c.AsyncStart, "asyncStart")
	add(c.EmptyPerAgentWorkspace, "emptyPerAgentWorkspace")
	add(c.AgentMove, "agentMove")
	add(c.AgentMoveRecreate, "agentMoveRecreate")
	add(c.ReprovisionEmptyPerAgent, "reprovisionEmptyPerAgent")
	return names
}
