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

package util

import (
	"fmt"
	"os"
	"sync"
	"time"
)

var (
	debugEnabled     bool
	debugInitialized bool
	agentDebugPolicy bool
	debugMu          sync.RWMutex
)

// SetAgentDebugPolicy selects which environment variable turns on debug
// output when EnableDebug has not been called.
//
// Inside an agent container SCION_DEBUG is inherited from whoever started
// the agent (a broker or a CLI run with --debug) and is meant for the
// in-container tooling and agent logs, not for every CLI command the agent
// runs. With the agent policy on, SCION_DEBUG is ignored and only
// SCION_LOG_LEVEL=debug enables debug output. With it off (the default),
// SCION_DEBUG is honoured as before.
func SetAgentDebugPolicy(on bool) {
	debugMu.Lock()
	defer debugMu.Unlock()
	agentDebugPolicy = on
}

// EnableDebug explicitly enables debug mode (e.g., from --debug flag).
func EnableDebug() {
	SetExplicitDebug(true)
}

// SetExplicitDebug sets whether debug mode was requested explicitly (for
// example with --debug). Passing false clears an earlier explicit request,
// so DebugEnabled falls back to the environment.
func SetExplicitDebug(on bool) {
	debugMu.Lock()
	defer debugMu.Unlock()
	debugEnabled = on
	debugInitialized = on
}

// DebugEnabled returns true if debug mode is enabled.
// Debug mode is enabled if:
//   - EnableDebug() was called (e.g., --debug flag)
//   - the agent debug policy is on and SCION_LOG_LEVEL=debug is set
//   - the agent debug policy is off and SCION_DEBUG is set
func DebugEnabled() bool {
	debugMu.RLock()
	initialized, enabled, agentPolicy := debugInitialized, debugEnabled, agentDebugPolicy
	debugMu.RUnlock()
	if initialized {
		return enabled
	}

	// Not explicitly set, check environment
	if agentPolicy {
		return os.Getenv("SCION_LOG_LEVEL") == "debug"
	}
	return os.Getenv("SCION_DEBUG") != ""
}

// Debugf prints a debug message to stderr if debug mode is enabled.
// The message is prefixed with a timestamp and [DEBUG].
func Debugf(format string, args ...interface{}) {
	if DebugEnabled() {
		ts := time.Now().Format("15:04:05.000")
		fmt.Fprintf(os.Stderr, ts+" [DEBUG] "+format+"\n", args...)
	}
}

// DebugfTagged prints a debug message with a custom tag to stderr if debug mode is enabled.
// Example: DebugfTagged("hubsync", "syncing %d agents", count) -> [hubsync] syncing 5 agents
func DebugfTagged(tag, format string, args ...interface{}) {
	if DebugEnabled() {
		fmt.Fprintf(os.Stderr, "["+tag+"] "+format+"\n", args...)
	}
}
