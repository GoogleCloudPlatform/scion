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

package conduit

// Broker RPC capability names (contracts §2): the transient operations a
// broker session serves over conduit, listed in Hello.capabilities.rpc.
// They are a separate namespace from the stream kinds ("logs" is a stream
// kind, RPCAgentLogs the RPC). The hub sends an operation over a broker
// session only when that session lists its name.
const (
	RPCAgentLogs      = "agent_logs"
	RPCAgentExec      = "agent_exec"
	RPCImageStatus    = "image_status"
	RPCImagePull      = "image_pull"
	RPCImageDelete    = "image_delete"
	RPCProjectCleanup = "project_cleanup"
)

// BrokerRPCSessionHeader names the conduit session a hub-signed broker
// RPC is issued for. The hub sets it to the receiving session's id and
// covers it with its signature; the broker serves the request only on that
// session.
const BrokerRPCSessionHeader = "X-Scion-Conduit-Session"

// BrokerRPCs returns every broker RPC capability name, in contract order.
func BrokerRPCs() []string {
	return []string{RPCAgentLogs, RPCAgentExec, RPCImageStatus, RPCImagePull, RPCImageDelete, RPCProjectCleanup}
}
