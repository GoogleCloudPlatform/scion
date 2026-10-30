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

package commands

import (
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/delegation"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// delegationSocketPath is where init serves the delegation socket: next to
// the agent token file, in the agent user's ~/.scion.
func delegationSocketPath() string {
	return filepath.Join(filepath.Dir(hub.TokenFilePath()), delegation.SocketName)
}

// startDelegationHelper serves the delegation socket for processes in the
// agent container (.design/agent-delegation.md §18.7), owned by uid:gid
// (uid <= 0 leaves it to init's own user). It returns nil, after logging,
// when the socket cannot be served; the agent runs on without it.
func startDelegationHelper(client *hub.Client, uid, gid int) *delegation.Helper {
	if !hub.TokenHomeIsolated() {
		// Under go test without SetTokenHome the socket path would be the
		// real agent home's; never create or replace a socket there.
		log.Debug("Delegation helper not started: token home not isolated in this test")
		return nil
	}
	h := delegation.New(delegation.Config{
		Client:     client,
		SocketPath: delegationSocketPath(),
		UID:        uid,
		GID:        gid,
	})
	if err := h.Start(); err != nil {
		log.Error("Delegation helper not started: %v", err)
		return nil
	}
	return h
}
