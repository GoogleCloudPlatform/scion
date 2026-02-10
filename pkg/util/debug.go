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
)

var (
	debugEnabled     bool
	debugInitialized bool
	debugMu          sync.RWMutex
)

// EnableDebug explicitly enables debug mode (e.g., from --debug flag).
func EnableDebug() {
	debugMu.Lock()
	defer debugMu.Unlock()
	debugEnabled = true
	debugInitialized = true
}

// DebugEnabled returns true if debug mode is enabled.
// Debug mode is enabled if:
// - EnableDebug() was called (e.g., --debug flag)
// - SCION_DEBUG environment variable is set
func DebugEnabled() bool {
	debugMu.RLock()
	if debugInitialized {
		result := debugEnabled
		debugMu.RUnlock()
		return result
	}
	debugMu.RUnlock()

	// Not explicitly set, check environment
	return os.Getenv("SCION_DEBUG") != ""
}

// Debugf prints a debug message to stderr if debug mode is enabled.
// The message is prefixed with [DEBUG].
func Debugf(format string, args ...interface{}) {
	if DebugEnabled() {
		fmt.Fprintf(os.Stderr, "[DEBUG] "+format+"\n", args...)
	}
}

// DebugfTagged prints a debug message with a custom tag to stderr if debug mode is enabled.
// Example: DebugfTagged("hubsync", "syncing %d agents", count) -> [hubsync] syncing 5 agents
func DebugfTagged(tag, format string, args ...interface{}) {
	if DebugEnabled() {
		fmt.Fprintf(os.Stderr, "["+tag+"] "+format+"\n", args...)
	}
}
