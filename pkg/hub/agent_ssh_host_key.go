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

package hub

import (
	"net/http"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// maxSSHHostKeyLen bounds a reported SSH host key (an authorized_keys
// line).
const maxSSHHostKeyLen = 4096

// recordAgentSSHHostKey records the SSH host key an agent reports for
// itself in its status POST (field sshHostKey), with the conduit
// experiment on. The reported host key is bound to the reporting agent's
// current run. Anything it does not record is ignored: the status POST
// itself is unaffected, and the key is never logged.
//
// The run comes from the verified agent token's run claim. A token
// without a run claim uses the X-Scion-Run-Id header instead, and the
// store write applies only while that run is the agent's current run.
// The first host key reported for a run is kept; a repeat of the same key
// succeeds.
func (s *Server) recordAgentSSHHostKey(r *http.Request, agentID, reported string) {
	ctx := r.Context()
	if !s.experimentEnabled(conduitExperiment) {
		return
	}
	ident, ok := GetIdentityFromContext(ctx).(AgentIdentity)
	claims := GetAgentFromContext(ctx)
	if !ok || claims == nil || ident.ID() != agentID {
		return
	}
	runID, ok := sshHostKeyRunID(claims.RunID, r.Header.Get(AgentRunIDHeader))
	if !ok {
		s.agentLifecycleLog.Debug("SSH host key not recorded: run not established", "agent_id", agentID)
		return
	}
	key, ok := normalizeSSHHostKey(reported)
	if !ok {
		s.agentLifecycleLog.Debug("SSH host key not recorded: not a public key line", "agent_id", agentID)
		return
	}
	res, err := s.store.SetAgentSSHHostKey(ctx, agentID, runID, key)
	switch {
	case err != nil:
		s.agentLifecycleLog.Warn("SSH host key not recorded", "agent_id", agentID, "error", err)
	case res == store.SSHHostKeyNotCurrentRun:
		s.agentLifecycleLog.Debug("SSH host key not recorded: not the agent's current run", "agent_id", agentID)
	case res == store.SSHHostKeyAlreadySet:
		s.agentLifecycleLog.Debug("SSH host key not recorded: a host key is already recorded for this run", "agent_id", agentID)
	}
}

// sshHostKeyRunID picks the run a reported host key is recorded for: the
// token's run claim when it has one (a differing X-Scion-Run-Id header
// means nothing is recorded), else the header (which the store write
// checks against the agent's current run). ok is false when no run is
// established.
func sshHostKeyRunID(claimRunID, header string) (string, bool) {
	if claimRunID != "" {
		if header != "" && header != claimRunID {
			return "", false
		}
		return claimRunID, true
	}
	if header == "" {
		return "", false
	}
	return header, true
}

// normalizeSSHHostKey accepts exactly one ed25519 public key in
// authorized_keys format (the type sciontool generates), without options,
// and returns it re-encoded without a comment or trailing newline. Other
// key types and certificates are refused.
func normalizeSSHHostKey(line string) (string, bool) {
	if len(line) > maxSSHHostKeyLen {
		return "", false
	}
	pub, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || len(options) != 0 || strings.TrimSpace(string(rest)) != "" || pub.Type() != ssh.KeyAlgoED25519 {
		return "", false
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), true
}

// agentSSHHostKey is the sshHostKey served on the single-agent GET: the
// recorded host key while it belongs to the agent's current run, with the
// conduit experiment on; otherwise "".
func (s *Server) agentSSHHostKey(agent *store.Agent) string {
	if !s.experimentEnabled(conduitExperiment) || agent.SSHHostKey == "" ||
		agent.SSHHostKeyRunID == "" || agent.SSHHostKeyRunID != agent.RunID {
		return ""
	}
	return agent.SSHHostKey
}
