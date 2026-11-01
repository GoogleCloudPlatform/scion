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
	"context"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Auxiliary runtime discovery after the hub connections start
// (ptone/scion#7010).
//
// Resolving a profile's runtime can be slow: for Kubernetes it builds a
// client and calls the API server (Client.Verify), and an unreachable
// cluster costs a connection timeout of about 30 s per attempt. Discovery
// used to run before the hub connections started, so one stale profile
// delayed the control channel on every broker restart. It now runs in the
// background after the connections start:
//
//   - each profile's runtime check is bounded by auxProfileCheckTimeout and
//     made once per pass (no retry), auxDiscoveryConcurrency at a time;
//   - a profile whose runtime is not available is reported in the broker's
//     self-health as profile_runtime.<name> = unavailable (see
//     profileRuntimeHealthChecks), naming the profile and nothing else;
//   - launch recovery (recoverLaunches), which needs the discovered
//     managers, runs when the pass ends.
//
// Commands do not depend on this pass: a start, create or restart resolves
// its profile's runtime itself (resolveManagerForOptsStrict, unbounded as
// before), and an operation on an existing agent resolves the agent's
// saved profile on demand (ensureAgentOwnRuntime). The heartbeat simply
// omits a runtime target that is not registered yet, and the hub draws no
// conclusion about the agents of a target absent from a heartbeat.

// auxProfileCheckTimeout bounds one profile's runtime check in the
// discovery pass. When it expires, the profile is reported unavailable and
// its worker slot is released at once; the abandoned check keeps running to
// its own end and, if it then succeeds, registers its runtime late
// (applyLateAuxResult). A variable so tests can shorten it.
var auxProfileCheckTimeout = 5 * time.Second

// auxDiscoveryConcurrency is the number of profile runtime checks the
// discovery pass runs at once.
const auxDiscoveryConcurrency = 4

// auxProfileHealthMax caps the profile_runtime.<name> self-health checks,
// so that they cannot crowd the broker's other checks out of the bounded
// report (api.BrokerHealthMaxChecks).
const auxProfileHealthMax = 4

// profileRuntimeCheckPrefix starts the name of a profile runtime
// self-health check.
const profileRuntimeCheckPrefix = "profile_runtime."

// startAuxiliaryRuntimeDiscovery runs auxiliary runtime discovery in the
// background and then launch recovery. It is called after the hub
// connections start. recoverLaunches runs even if discovery returns early.
// The pass's worst case is about ceil(checks / auxDiscoveryConcurrency) x
// auxProfileCheckTimeout, plus settings reads.
func (s *Server) startAuxiliaryRuntimeDiscovery(_ context.Context) {
	done := make(chan struct{})
	s.auxDiscoveryMu.Lock()
	s.auxDiscoveryDone = done
	s.auxDiscoveryMu.Unlock()
	go func() {
		defer close(done)
		defer afterAuxiliaryRuntimeDiscovery(s)
		s.discoverAuxiliaryRuntimes()
	}()
}

// afterAuxiliaryRuntimeDiscovery runs when a background discovery pass
// ends: launch recovery. A variable so tests can observe the order.
var afterAuxiliaryRuntimeDiscovery = func(s *Server) { s.recoverLaunches() }

// waitAuxiliaryRuntimeDiscovery waits up to timeout for a pass started by
// startAuxiliaryRuntimeDiscovery, and reports whether it finished (true
// when none was started).
func (s *Server) waitAuxiliaryRuntimeDiscovery(timeout time.Duration) bool {
	s.auxDiscoveryMu.Lock()
	done := s.auxDiscoveryDone
	s.auxDiscoveryMu.Unlock()
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// setProfileRuntimeUnavailable records (unavailable true) or clears the
// profile runtime self-health check of profileName.
func (s *Server) setProfileRuntimeUnavailable(profileName string, unavailable bool) {
	if profileName == "" {
		return
	}
	s.profileRuntimeHealthMu.Lock()
	defer s.profileRuntimeHealthMu.Unlock()
	if !unavailable {
		delete(s.profileRuntimeUnavailable, profileName)
		return
	}
	if s.profileRuntimeUnavailable == nil {
		s.profileRuntimeUnavailable = make(map[string]bool)
	}
	s.profileRuntimeUnavailable[profileName] = true
}

// profileRuntimeHealthChecks returns the self-health checks for profiles
// whose runtime is not available: profile_runtime.<name> = unavailable,
// with the name reduced to the characters a check name allows, at most
// auxProfileHealthMax of them in sorted order. They name the profile only:
// never the resolution error or anything from the runtime configuration.
func (s *Server) profileRuntimeHealthChecks() map[string]string {
	s.profileRuntimeHealthMu.Lock()
	names := make([]string, 0, len(s.profileRuntimeUnavailable))
	for name := range s.profileRuntimeUnavailable {
		names = append(names, name)
	}
	s.profileRuntimeHealthMu.Unlock()
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	checks := make(map[string]string)
	for _, name := range names {
		if len(checks) == auxProfileHealthMax {
			break
		}
		checks[profileRuntimeCheckName(name)] = api.BrokerHealthUnavailable
	}
	return checks
}

// profileRuntimeCheckName is the self-health check name for profileName:
// profileRuntimeCheckPrefix plus the name with every character outside
// [A-Za-z0-9_.-] replaced by '_', cut to api.BrokerHealthMaxNameChars.
func profileRuntimeCheckName(profileName string) string {
	var b strings.Builder
	b.WriteString(profileRuntimeCheckPrefix)
	for _, r := range profileName {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := b.String()
	if len(name) > api.BrokerHealthMaxNameChars {
		name = name[:api.BrokerHealthMaxNameChars]
	}
	return name
}
