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

	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

func TestConfigureDebugOutput(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		explicit   bool
		scionDebug string
		logLevel   string
		want       bool
	}{
		{name: "agent mode ignores inherited SCION_DEBUG", mode: "agent", scionDebug: "1", want: false},
		{name: "agent mode with --debug", mode: "agent", explicit: true, scionDebug: "1", want: true},
		{name: "agent mode with SCION_LOG_LEVEL=debug", mode: "agent", scionDebug: "1", logLevel: "debug", want: true},
		{name: "human mode honours SCION_DEBUG", mode: "human", scionDebug: "1", want: true},
		{name: "assistant mode honours SCION_DEBUG", mode: "assistant", scionDebug: "1", want: true},
		{name: "human mode with nothing set", mode: "human", want: false},
		{name: "human mode with --debug", mode: "human", explicit: true, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Cleanup(func() { configureDebugOutput(false); util.SetAgentDebugPolicy(false) })
			t.Setenv("SCION_CLI_MODE", tt.mode)
			t.Setenv("SCION_DEBUG", tt.scionDebug)
			t.Setenv("SCION_LOG_LEVEL", tt.logLevel)

			configureDebugOutput(tt.explicit)

			if got := util.DebugEnabled(); got != tt.want {
				t.Errorf("util.DebugEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfigureDebugOutput_ClearsEarlierExplicitDebug(t *testing.T) {
	t.Cleanup(func() { configureDebugOutput(false); util.SetAgentDebugPolicy(false) })
	t.Setenv("SCION_CLI_MODE", "agent")
	t.Setenv("SCION_DEBUG", "1")
	t.Setenv("SCION_LOG_LEVEL", "")

	configureDebugOutput(true)
	configureDebugOutput(false)

	if util.DebugEnabled() {
		t.Error("debug output still enabled after an invocation without --debug")
	}
}
