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
	"encoding/json"
	"fmt"
	"log/slog"
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
//   - each profile's runtime check is bounded by the check timeout
//     (auxProfileTimeout) and made once per pass (no retry),
//     auxDiscoveryConcurrency at a time;
//   - a check that hit the timeout, or a failure of a project's active
//     profile, is reported in the broker's self-health as
//     profile_runtime.<name> = unavailable (profileRuntimeHealthChecks),
//     naming the profile and nothing else. A fast failure of a non-active
//     profile (for example the built-in profiles on a broker without
//     cluster access) is not reported there and keeps its Info log line;
//   - launch recovery (recoverLaunches), which needs the discovered
//     managers, runs when the pass ends, unless the server is stopping.
//
// The pass stops with the server: Shutdown cancels its context and waits
// for it, bounded. Commands do not depend on the pass: a start, create or
// restart resolves its profile's runtime itself (resolveManagerForOptsStrict,
// unbounded as before), and an operation on an existing agent resolves the
// agent's saved profile on demand (ensureAgentOwnRuntime). The heartbeat
// simply omits a runtime target that is not registered yet, and the hub
// draws no conclusion about the agents of a target absent from a heartbeat.

// auxProfileCheckTimeout is the default bound of one profile's runtime
// check in the discovery pass. When it expires, the profile is reported
// unavailable and its worker slot is released at once; the abandoned check
// keeps running to its own end and, if it then succeeds, registers its
// runtime late (applyLateAuxResult). Server.auxCheckTimeout overrides it.
const auxProfileCheckTimeout = 5 * time.Second

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

// auxDiscoveryStopWait bounds how long Shutdown waits for a running pass.
const auxDiscoveryStopWait = 3 * time.Second

// auxProfileTimeout is the profile runtime check bound of this server.
func (s *Server) auxProfileTimeout() time.Duration {
	if s.auxCheckTimeout > 0 {
		return s.auxCheckTimeout
	}
	return auxProfileCheckTimeout
}

// startAuxiliaryRuntimeDiscovery runs auxiliary runtime discovery in the
// background and then launch recovery. It is called after the hub
// connections start. The pass runs on a context derived from ctx that
// Shutdown cancels (stopAuxiliaryRuntimeDiscovery). recoverLaunches runs
// when the pass ends, also after an early return, unless that context is
// done: a stopping server leaves its launch journal to the next start. The
// pass's worst case is about ceil(checks / auxDiscoveryConcurrency) x the
// check timeout, plus settings reads.
func (s *Server) startAuxiliaryRuntimeDiscovery(ctx context.Context) {
	pctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.auxDiscoveryMu.Lock()
	if s.auxDiscoveryStopped {
		// Shutdown already ran: start no pass that it could not stop.
		s.auxDiscoveryMu.Unlock()
		cancel()
		slog.Info("Server stopping: auxiliary runtime discovery and launch recovery not started")
		return
	}
	s.auxDiscoveryDone, s.auxDiscoveryCancel = done, cancel
	s.auxDiscoveryMu.Unlock()
	go func() {
		defer close(done)
		defer func() {
			if pctx.Err() != nil {
				slog.Info("Server stopping: launch recovery after auxiliary runtime discovery skipped; the journal is kept for the next start")
				return
			}
			if s.afterAuxDiscovery != nil {
				s.afterAuxDiscovery()
				return
			}
			s.recoverLaunches()
		}()
		s.discoverAuxiliaryRuntimes(pctx)
	}()
}

// stopAuxiliaryRuntimeDiscovery cancels a running pass and waits for it,
// up to auxDiscoveryStopWait, and keeps a later start from starting one.
// Called by Shutdown.
func (s *Server) stopAuxiliaryRuntimeDiscovery() {
	s.auxDiscoveryMu.Lock()
	s.auxDiscoveryStopped = true
	cancel := s.auxDiscoveryCancel
	s.auxDiscoveryMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	if !s.waitAuxiliaryRuntimeDiscovery(auxDiscoveryStopWait) {
		slog.Warn("Auxiliary runtime discovery did not stop in time", "wait", auxDiscoveryStopWait)
	}
}

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

// auxProfileKey identifies one profile's runtime settings: the profile
// name, the resolved runtime type and runtime config. It is the key the
// discovery pass de-duplicates candidates by, and the key the unavailable
// state is kept under, so that a same-named profile with other settings
// (in another project) neither hides nor clears the report.
func auxProfileKey(profileName, runtimeType string, rtConfig any, projectPath string) string {
	key := profileName + "\x00" + runtimeType
	if b, err := json.Marshal(rtConfig); err == nil {
		return key + "\x00" + string(b)
	}
	return key + "\x00" + projectPath
}

// setProfileRuntimeUnavailable records (unavailable true) or clears the
// unavailable state of the profile settings identified by key (see
// auxProfileKey); profileName is the name the report shows.
func (s *Server) setProfileRuntimeUnavailable(key, profileName string, unavailable bool) {
	if key == "" {
		return
	}
	s.profileRuntimeHealthMu.Lock()
	defer s.profileRuntimeHealthMu.Unlock()
	if !unavailable {
		delete(s.profileRuntimeUnavailable, key)
		return
	}
	if s.profileRuntimeUnavailable == nil {
		s.profileRuntimeUnavailable = make(map[string]string)
	}
	s.profileRuntimeUnavailable[key] = profileName
}

// profileRuntimeHealthChecks returns the self-health checks for profile
// settings whose runtime is not available: profile_runtime.<name> =
// unavailable, with the name reduced to the characters a check name allows.
// Entries are taken in sorted key order; when two entries map to the same
// check name (the same profile name with other settings), the later ones get
// a "-2", "-3", ... suffix. At most auxProfileHealthMax checks are returned.
// They name the profile only: never the resolution error or anything from
// the runtime configuration.
func (s *Server) profileRuntimeHealthChecks() map[string]string {
	s.profileRuntimeHealthMu.Lock()
	keys := make([]string, 0, len(s.profileRuntimeUnavailable))
	for k := range s.profileRuntimeUnavailable {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = s.profileRuntimeUnavailable[k]
	}
	s.profileRuntimeHealthMu.Unlock()
	if len(keys) == 0 {
		return nil
	}
	checks := make(map[string]string)
	for _, name := range names {
		if len(checks) == auxProfileHealthMax {
			break
		}
		base := profileRuntimeCheckName(name)
		check := base
		for n := 2; checks[check] != ""; n++ {
			suffix := fmt.Sprintf("-%d", n)
			if len(base)+len(suffix) > api.BrokerHealthMaxNameChars {
				check = base[:api.BrokerHealthMaxNameChars-len(suffix)] + suffix
			} else {
				check = base + suffix
			}
		}
		checks[check] = api.BrokerHealthUnavailable
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

// logAuxProfileCheckTimedOut reports, at Warn, a profile runtime check the
// pass stopped waiting for: the unreachable-runtime case #7010 is about.
func logAuxProfileCheckTimedOut(profileName string, timeout time.Duration) {
	slog.Warn("Runtime of a profile is not available on this broker: its runtime check did not finish in time; agents that use it are not found until it resolves",
		"profile", profileName, "timeout", timeout.String(),
		"hint", "fix the profile's runtime configuration if agents use it")
}
